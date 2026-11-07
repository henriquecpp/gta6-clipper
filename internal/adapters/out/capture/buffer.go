package capture

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// segment is one file of the circular buffer.
type segment struct {
	path  string
	start domain.StreamPos
	end   domain.StreamPos
	size  int64
	seq   uint64 // global insertion order, for the byte limit
}

type streamBuffer struct {
	dir  string
	segs []segment // ordered by start
	runs int
}

// buffer indexes the segments of every live and enforces the time and byte
// limits. Segments in use by a retention are pinned: they leave the index but
// their files are deleted only when unpinned.
type buffer struct {
	mu       sync.Mutex
	root     string
	duration time.Duration
	maxBytes int64
	streams  map[domain.StreamID]*streamBuffer
	total    int64
	seq      uint64
	pins     map[string]int
	deferred map[string]bool
}

func newBuffer(root string, duration time.Duration, maxBytes int64) *buffer {
	return &buffer{
		root: root, duration: duration, maxBytes: maxBytes,
		streams: make(map[domain.StreamID]*streamBuffer), pins: make(map[string]int), deferred: make(map[string]bool),
	}
}

// startRun prepares the directory of a live and returns a new run number.
func (b *buffer) startRun(id domain.StreamID) (string, int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sb, ok := b.streams[id]
	if !ok {
		sb = &streamBuffer{dir: filepath.Join(b.root, safeName(string(id)))}
		b.streams[id] = sb
	}
	if err := os.MkdirAll(sb.dir, 0o700); err != nil {
		return "", 0, fmt.Errorf("create buffer dir: %w", err)
	}
	sb.runs++
	return sb.dir, sb.runs, nil
}

// add indexes a finished segment and applies the limits. It returns the
// files to delete (outside the lock).
func (b *buffer) add(id domain.StreamID, s segment) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	sb, ok := b.streams[id]
	if !ok {
		return []string{s.path} // live already dropped
	}
	b.seq++
	s.seq = b.seq
	i := len(sb.segs)
	for i > 0 && sb.segs[i-1].start > s.start {
		i--
	}
	sb.segs = append(sb.segs, segment{})
	copy(sb.segs[i+1:], sb.segs[i:])
	sb.segs[i] = s
	b.total += s.size
	return b.enforceLocked(id)
}

func (b *buffer) enforceLocked(id domain.StreamID) []string {
	var del []string
	if sb, ok := b.streams[id]; ok && len(sb.segs) > 0 {
		latest := sb.segs[len(sb.segs)-1].end
		cut := latest - domain.StreamPos(b.duration)
		n := 0
		for n < len(sb.segs) && sb.segs[n].end < cut {
			del = append(del, b.dropLocked(sb.segs[n])...)
			n++
		}
		sb.segs = sb.segs[n:]
	}
	for b.total > b.maxBytes {
		var oldestID domain.StreamID
		var oldest *segment
		for sid, sb := range b.streams {
			if len(sb.segs) > 0 && (oldest == nil || sb.segs[0].seq < oldest.seq) {
				oldestID, oldest = sid, &sb.segs[0]
			}
		}
		if oldest == nil {
			break
		}
		del = append(del, b.dropLocked(*oldest)...)
		sb := b.streams[oldestID]
		sb.segs = sb.segs[1:]
	}
	return del
}

// dropLocked removes a segment from the accounting; its file is returned for
// deletion unless pinned.
func (b *buffer) dropLocked(s segment) []string {
	b.total -= s.size
	if b.pins[s.path] > 0 {
		b.deferred[s.path] = true
		return nil
	}
	return []string{s.path}
}

// window returns the segments overlapping [from, to] and pins them until
// unpin is called. The window is unavailable if nothing overlaps or the end
// has not been captured yet (within one segment of tolerance).
func (b *buffer) window(id domain.StreamID, from, to domain.StreamPos, tolerance time.Duration) ([]segment, func() []string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sb, ok := b.streams[id]
	if !ok || len(sb.segs) == 0 {
		return nil, nil, fmt.Errorf("stream %s has no buffered media: %w", id, domain.ErrWindowUnavailable)
	}
	var out []segment
	for _, s := range sb.segs {
		if s.end > from && s.start < to {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil, nil, fmt.Errorf("window %v–%v not in buffer of %s: %w", from, to, id, domain.ErrWindowUnavailable)
	}
	if out[len(out)-1].end+domain.StreamPos(tolerance) < to {
		return nil, nil, fmt.Errorf("window end %v not captured yet (buffer ends at %v): %w", to, out[len(out)-1].end, domain.ErrWindowUnavailable)
	}
	for _, s := range out {
		b.pins[s.path]++
	}
	unpin := func() []string {
		b.mu.Lock()
		defer b.mu.Unlock()
		var del []string
		for _, s := range out {
			b.pins[s.path]--
			if b.pins[s.path] <= 0 {
				delete(b.pins, s.path)
				if b.deferred[s.path] {
					delete(b.deferred, s.path)
					del = append(del, s.path)
				}
			}
		}
		return del
	}
	return out, unpin, nil
}

// dropStream forgets a live and returns its directory for removal.
func (b *buffer) dropStream(id domain.StreamID) (dir string, files []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sb, ok := b.streams[id]
	if !ok {
		return "", nil
	}
	for _, s := range sb.segs {
		files = append(files, b.dropLocked(s)...)
	}
	delete(b.streams, id)
	return sb.dir, files
}

// stats returns the number of segments and bytes buffered.
func (b *buffer) stats() (segments int, bytes int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, sb := range b.streams {
		segments += len(sb.segs)
	}
	return segments, b.total
}

func fileSafe(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_'
}

// safeName keeps ids usable as file names.
func safeName(s string) string {
	out := []rune(s)
	for i, r := range out {
		if !fileSafe(r) {
			out[i] = '_'
		}
	}
	if len(out) == 0 {
		return "_"
	}
	return string(out)
}
