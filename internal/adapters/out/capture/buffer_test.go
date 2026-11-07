package capture

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

func sec(s float64) domain.StreamPos {
	return domain.StreamPos(time.Duration(s * float64(time.Second)))
}

func seg(name string, start, end float64, size int64) segment {
	return segment{path: name, start: sec(start), end: sec(end), size: size}
}

func TestBufferTimeLimit(t *testing.T) {
	b := newBuffer(t.TempDir(), 10*time.Second, 1<<30)
	if _, run, err := b.startRun("s1"); err != nil || run != 1 {
		t.Fatalf("run = %d %v", run, err)
	}
	var deleted []string
	for i := range 10 {
		deleted = append(deleted, b.add("s1", seg(string(rune('a'+i)), float64(2*i), float64(2*i+2), 100))...)
	}
	// Latest end 20 s, limit 10 s: segments ending before 10 s go (a..d).
	if !slices.Equal(deleted, []string{"a", "b", "c", "d"}) {
		t.Fatalf("deleted = %v", deleted)
	}
	n, bytes := b.stats()
	if n != 6 || bytes != 600 {
		t.Fatalf("stats = %d segments %d bytes", n, bytes)
	}
}

func TestBufferByteLimitDropsGloballyOldest(t *testing.T) {
	b := newBuffer(t.TempDir(), time.Hour, 300)
	_, _, _ = b.startRun("s1")
	_, _, _ = b.startRun("s2")
	var deleted []string
	deleted = append(deleted, b.add("s1", seg("s1a", 0, 2, 100))...)
	deleted = append(deleted, b.add("s2", seg("s2a", 0, 2, 100))...)
	deleted = append(deleted, b.add("s1", seg("s1b", 2, 4, 100))...)
	deleted = append(deleted, b.add("s2", seg("s2b", 2, 4, 100))...) // 400 > 300 → drop s1a
	deleted = append(deleted, b.add("s2", seg("s2c", 4, 6, 100))...) // drop s2a
	if !slices.Equal(deleted, []string{"s1a", "s2a"}) {
		t.Fatalf("deleted = %v", deleted)
	}
	if _, bytes := b.stats(); bytes != 300 {
		t.Fatalf("bytes = %d", bytes)
	}
}

func TestBufferWindowAndPins(t *testing.T) {
	b := newBuffer(t.TempDir(), 10*time.Second, 1<<30)
	_, _, _ = b.startRun("s1")
	for i := range 5 {
		b.add("s1", seg(string(rune('a'+i)), float64(2*i), float64(2*i+2), 1))
	}
	// Out-of-order insert keeps the order.
	b.add("s1", seg("z", 10, 12, 1))

	segs, unpin, err := b.window("s1", sec(3), sec(7), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range segs {
		names = append(names, s.path)
	}
	if !slices.Equal(names, []string{"b", "c", "d"}) {
		t.Fatalf("window = %v", names)
	}
	// While pinned, aging out does not delete the files.
	var deleted []string
	for i := 6; i < 12; i++ {
		deleted = append(deleted, b.add("s1", seg(string(rune('A'+i)), float64(2*i), float64(2*i+2), 1))...)
	}
	if slices.Contains(deleted, "b") || slices.Contains(deleted, "c") || !slices.Contains(deleted, "a") {
		t.Fatalf("deleted while pinned = %v", deleted)
	}
	later := unpin()
	slices.Sort(later)
	if !slices.Equal(later, []string{"b", "c", "d"}) {
		t.Fatalf("deferred deletions = %v", later)
	}

	for _, tc := range []struct {
		id       domain.StreamID
		from, to float64
	}{
		{"nope", 0, 1}, // unknown stream
		{"s1", 0, 1},   // aged out
		{"s1", 20, 40}, // not captured yet
	} {
		if _, _, err := b.window(tc.id, sec(tc.from), sec(tc.to), time.Second); !errors.Is(err, domain.ErrWindowUnavailable) {
			t.Errorf("window(%s, %v, %v) err = %v", tc.id, tc.from, tc.to, err)
		}
	}
}

func TestBufferDropStream(t *testing.T) {
	b := newBuffer(t.TempDir(), time.Minute, 1<<30)
	dir, _, _ := b.startRun("s1")
	b.add("s1", seg("a", 0, 2, 10))
	gotDir, files := b.dropStream("s1")
	if gotDir != dir || !slices.Equal(files, []string{"a"}) {
		t.Fatalf("drop = %q %v", gotDir, files)
	}
	if d, f := b.dropStream("s1"); d != "" || f != nil {
		t.Fatal("second drop should be empty")
	}
	if del := b.add("s1", seg("late", 2, 4, 10)); !slices.Equal(del, []string{"late"}) {
		t.Fatalf("segment of dropped stream should be deleted: %v", del)
	}
	if safeName("a/b:c") != "a_b_c" || safeName("") != "_" {
		t.Fatal("safeName")
	}
}
