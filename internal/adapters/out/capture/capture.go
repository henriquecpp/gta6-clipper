package capture

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/app"
	"github.com/henriquecpp/gta6-clipper/internal/domain"
	"github.com/henriquecpp/gta6-clipper/internal/platform/proc"
)

// sampleRate of the PCM used for the voice signal.
const sampleRate = 16000

// Config configures the capture.
type Config struct {
	BufferDir       string
	PendingDir      string
	BufferDuration  time.Duration
	MaxBytes        int64
	SegmentDuration time.Duration
	Quality         string
	// Frame is the VAD frame (e.g. 30 ms).
	Frame time.Duration
	// Threads per short ffmpeg job; Workers bounds concurrent short jobs.
	Threads int
	Workers int
	// FFmpeg and Streamlink binaries (looked up on PATH).
	FFmpeg     string
	Streamlink string
}

// SourceFunc builds the command that writes the live MPEG-TS to stdout.
type SourceFunc func(ctx context.Context, live domain.LiveStream) *exec.Cmd

// Capture is the capture adapter.
type Capture struct {
	cfg    Config
	buf    *buffer
	log    *slog.Logger
	now    func() time.Time
	source SourceFunc
	jobs   chan struct{}
}

var _ app.MediaCapture = (*Capture)(nil)

// Option configures a Capture.
type Option func(*Capture)

// WithSource replaces Streamlink as the stream source (integration tests).
func WithSource(f SourceFunc) Option { return func(c *Capture) { c.source = f } }

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option { return func(c *Capture) { c.log = l } }

// New creates the capture, emptying the buffer directory (its content never
// survives a restart) and creating the pending directory.
func New(cfg Config, opts ...Option) (*Capture, error) {
	if cfg.FFmpeg == "" {
		cfg.FFmpeg = "ffmpeg"
	}
	if cfg.Streamlink == "" {
		cfg.Streamlink = "streamlink"
	}
	if cfg.Frame <= 0 {
		cfg.Frame = 30 * time.Millisecond
	}
	if cfg.SegmentDuration <= 0 {
		cfg.SegmentDuration = 2 * time.Second
	}
	cfg.Workers = max(cfg.Workers, 1)
	if err := os.MkdirAll(cfg.BufferDir, 0o700); err != nil {
		return nil, fmt.Errorf("create buffer dir: %w", err)
	}
	if err := emptyDir(cfg.BufferDir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.PendingDir, 0o700); err != nil {
		return nil, fmt.Errorf("create pending dir: %w", err)
	}
	c := &Capture{
		cfg:  cfg,
		buf:  newBuffer(cfg.BufferDir, cfg.BufferDuration, cfg.MaxBytes),
		log:  slog.Default(),
		now:  func() time.Time { return time.Now().UTC() },
		jobs: make(chan struct{}, cfg.Workers),
	}
	c.source = func(ctx context.Context, live domain.LiveStream) *exec.Cmd {
		return proc.Command(ctx, cfg.Streamlink, streamlinkArgs(live.Login, cfg.Quality)...)
	}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

func emptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read %s: %w", dir, err)
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return fmt.Errorf("clean %s: %w", dir, err)
		}
	}
	return nil
}

// Capture runs one capture of the live until the source ends or ctx is
// cancelled. When ctx is cancelled (live ended or shutdown) the buffer of the
// live is deleted; when the source fails, the buffer is kept for the restart.
func (c *Capture) Capture(ctx context.Context, live domain.LiveStream, captureStart time.Time, onFrame func(domain.AudioFrame)) (err error) {
	dir, run, err := c.buf.startRun(live.ID)
	if err != nil {
		return err
	}
	defer func() {
		if ctx.Err() != nil {
			c.DropStream(live.ID)
		}
	}()
	posBase := domain.StreamPos(c.now().Sub(captureStart))

	src := c.source(ctx, live)
	ff := proc.Command(ctx, c.cfg.FFmpeg, ffmpegCaptureArgs(dir, run, c.cfg.SegmentDuration, sampleRate)...)
	srcErr := &proc.TailBuffer{Max: proc.DefaultStderrBytes}
	ffErr := &proc.TailBuffer{Max: proc.DefaultStderrBytes}
	src.Stderr, ff.Stderr = srcErr, ffErr

	tsR, tsW, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("pipe: %w", err)
	}
	listR, listW, err := os.Pipe()
	if err != nil {
		closeAll(tsR, tsW)
		return fmt.Errorf("pipe: %w", err)
	}
	src.Stdout = tsW
	ff.Stdin = tsR
	ff.ExtraFiles = []*os.File{listW} // fd 3 = segment list (CSV)
	pcm, err := ff.StdoutPipe()
	if err != nil {
		closeAll(tsR, tsW, listR, listW)
		return fmt.Errorf("ffmpeg stdout: %w", err)
	}
	if err := ff.Start(); err != nil {
		closeAll(tsR, tsW, listR, listW)
		return fmt.Errorf("start ffmpeg: %w", err)
	}
	if err := src.Start(); err != nil {
		closeAll(tsR, tsW, listW)
		_ = ff.Process.Kill() // ffmpeg would wait forever for input
		_ = ff.Wait()         // reap it; the start error is the one to report
		_ = listR.Close()
		return fmt.Errorf("start source: %w", err)
	}
	// The children hold their own copies of the pipe ends.
	closeAll(tsR, tsW, listW)

	c.log.Info("capture started", slog.String("streamer", live.Login), slog.String("stream_id", string(live.ID)),
		slog.Int("run", run), slog.String("pos_base", posBase.String()))

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer func() { _ = listR.Close() }() // read side; EOF already reached or process gone
		c.readSegments(listR, live.ID, dir, posBase)
	}()
	frames := 0
	go func() {
		defer wg.Done()
		frames = c.readPCM(pcm, posBase, onFrame)
	}()

	srcWaitErr := src.Wait()
	wg.Wait() // readers finish at EOF, after ffmpeg flushed its outputs
	ffWaitErr := ff.Wait()
	c.log.Info("capture ended", slog.String("streamer", live.Login), slog.String("stream_id", string(live.ID)),
		slog.Int("run", run), slog.Int("audio_frames", frames))
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var errs []error
	if srcWaitErr != nil {
		errs = append(errs, &proc.Error{Name: "source", Err: srcWaitErr, Stderr: srcErr.LastLines(5)})
	}
	if ffWaitErr != nil {
		errs = append(errs, &proc.Error{Name: "ffmpeg", Err: ffWaitErr, Stderr: ffErr.LastLines(5)})
	}
	if len(errs) == 0 {
		return io.EOF // the stream ended normally (offline, raid...)
	}
	return errors.Join(errs...)
}

func closeAll(files ...*os.File) {
	for _, f := range files {
		_ = f.Close() // parent copies of pipe ends; nothing to report
	}
}

// readSegments indexes each finished segment announced by ffmpeg.
func (c *Capture) readSegments(r io.Reader, id domain.StreamID, dir string, posBase domain.StreamPos) {
	sc := bufio.NewScanner(r)
	first, hasFirst := 0.0, false
	for sc.Scan() {
		name, start, end, err := parseSegmentLine(sc.Text())
		if err != nil {
			c.log.Warn("capture: ignoring segment list line", slog.String("err", err.Error()))
			continue
		}
		if !hasFirst {
			first, hasFirst = start, true
		}
		path := filepath.Join(dir, filepath.Base(name))
		st, err := os.Stat(path)
		if err != nil {
			c.log.Warn("capture: segment missing", slog.String("path", path), slog.String("err", err.Error()))
			continue
		}
		s := segment{
			path:  path,
			start: posBase + domain.StreamPos(secondsToDuration(start-first)),
			end:   posBase + domain.StreamPos(secondsToDuration(end-first)),
			size:  st.Size(),
		}
		removeFiles(c.log, c.buf.add(id, s))
	}
}

func secondsToDuration(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// readPCM classifies every frame of PCM and reports it on the stream timeline.
func (c *Capture) readPCM(r io.Reader, posBase domain.StreamPos, onFrame func(domain.AudioFrame)) int {
	frameMS := int(c.cfg.Frame / time.Millisecond)
	n := sampleRate * frameMS / 1000
	raw := make([]byte, 2*n)
	samples := make([]int16, n)
	vad := NewVAD(sampleRate, frameMS)
	br := bufio.NewReaderSize(r, 64<<10)
	i := 0
	for ; ; i++ {
		if _, err := io.ReadFull(br, raw); err != nil {
			return i
		}
		for j := range samples {
			samples[j] = int16(binary.LittleEndian.Uint16(raw[2*j:]))
		}
		db, speech := vad.Frame(samples)
		onFrame(domain.AudioFrame{Pos: posBase + domain.StreamPos(time.Duration(i)*c.cfg.Frame), DBFS: db, Speech: speech})
	}
}

func removeFiles(log *slog.Logger, paths []string) {
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Warn("capture: remove segment", slog.String("path", p), slog.String("err", err.Error()))
		}
	}
}

// DropStream deletes the buffer of a live.
func (c *Capture) DropStream(id domain.StreamID) {
	dir, files := c.buf.dropStream(id)
	removeFiles(c.log, files)
	if dir != "" {
		if err := os.RemoveAll(dir); err != nil {
			c.log.Warn("capture: remove buffer dir", slog.String("dir", dir), slog.String("err", err.Error()))
		}
	}
}

// RetainWindow copies the whole segments covering [from, to] to the pending
// storage without re-encoding and returns the window actually retained.
func (c *Capture) RetainWindow(ctx context.Context, id domain.StreamID, from, to domain.StreamPos) (domain.MediaRef, domain.Window, error) {
	segs, unpin, err := c.buf.window(id, from, to, c.cfg.SegmentDuration)
	if err != nil {
		return "", domain.Window{}, err
	}
	defer func() { removeFiles(c.log, unpin()) }()

	select {
	case c.jobs <- struct{}{}:
	case <-ctx.Done():
		return "", domain.Window{}, ctx.Err()
	}
	defer func() { <-c.jobs }()

	suffix, err := randomSuffix()
	if err != nil {
		return "", domain.Window{}, err
	}
	paths := make([]string, len(segs))
	for i, s := range segs {
		paths[i] = s.path
	}
	list := filepath.Join(c.cfg.PendingDir, ".concat-"+suffix+".txt")
	if err := os.WriteFile(list, []byte(concatList(paths)), 0o600); err != nil {
		return "", domain.Window{}, fmt.Errorf("write concat list: %w", err)
	}
	defer func() { _ = os.Remove(list) }() // temporary file
	w := domain.Window{From: segs[0].start, To: segs[len(segs)-1].end}
	out := filepath.Join(c.cfg.PendingDir, fmt.Sprintf("%s_%d_%d_%s.ts", safeName(string(id)),
		time.Duration(w.From).Milliseconds(), time.Duration(w.To).Milliseconds(), suffix))
	if _, err := proc.Run(ctx, nil, c.cfg.FFmpeg, concatArgs(list, out, c.cfg.Threads)...); err != nil {
		_ = os.Remove(out) // partial output
		return "", domain.Window{}, fmt.Errorf("retain window %v–%v of %s: %w", from, to, id, err)
	}
	return domain.MediaRef(out), w, nil
}

// Delete removes a file of the pending storage.
func (c *Capture) Delete(_ context.Context, ref domain.MediaRef) error {
	path, err := c.pendingPath(ref)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete %s: %w", path, err)
	}
	return nil
}

// pendingPath validates that ref points inside the pending directory.
func (c *Capture) pendingPath(ref domain.MediaRef) (string, error) {
	abs, err := filepath.Abs(string(ref))
	if err != nil {
		return "", err
	}
	root, err := filepath.Abs(c.cfg.PendingDir)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return "", fmt.Errorf("media ref %q is outside the pending directory", ref)
	}
	return abs, nil
}

// Prune deletes every file of the pending storage not referenced by keep
// (orphans left by crashes). Hidden temporary files are always removed.
func (c *Capture) Prune(keep map[domain.MediaRef]bool) (int, error) {
	entries, err := os.ReadDir(c.cfg.PendingDir)
	if err != nil {
		return 0, fmt.Errorf("read pending dir: %w", err)
	}
	keepAbs := make(map[string]bool, len(keep))
	for ref := range keep {
		if p, err := filepath.Abs(string(ref)); err == nil {
			keepAbs[p] = true
		}
	}
	removed := 0
	var errs []error
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		p, err := filepath.Abs(filepath.Join(c.cfg.PendingDir, e.Name()))
		if err != nil || keepAbs[p] {
			continue
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

// Stats reports the buffered segments and bytes (metrics, tests).
func (c *Capture) Stats() (segments int, bytes int64) { return c.buf.stats() }

func randomSuffix() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("random suffix: %w", err)
	}
	return hex.EncodeToString(b), nil
}
