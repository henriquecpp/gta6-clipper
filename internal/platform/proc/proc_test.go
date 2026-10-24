package proc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTailBufferKeepsLastBytes(t *testing.T) {
	tb := &TailBuffer{Max: 10}
	_, _ = tb.Write([]byte("0123456789"))
	_, _ = tb.Write([]byte("abc"))
	if got := string(tb.buf); got != "3456789abc" {
		t.Fatalf("buf = %q", got)
	}
}

func TestTailBufferLastLines(t *testing.T) {
	tb := &TailBuffer{}
	_, _ = tb.Write([]byte("one\n\ntwo\nthree\nfour\n"))
	if got := tb.LastLines(2); got != "three | four" {
		t.Fatalf("LastLines = %q", got)
	}
	if got := tb.LastLines(10); got != "one | two | three | four" {
		t.Fatalf("LastLines = %q", got)
	}
}

// TestRunUsesGoAsSubprocess runs the Go toolchain itself, which is always
// available where tests run, to exercise success and failure paths.
func TestRunSuccessAndFailure(t *testing.T) {
	out, err := Run(context.Background(), nil, "go", "env", "GOOS")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) == "" {
		t.Fatal("empty output")
	}
	_, err = Run(context.Background(), nil, "go", "definitely-not-a-command")
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("want *Error, got %v", err)
	}
	if pe.Stderr == "" || !strings.Contains(pe.Error(), "go:") {
		t.Fatalf("stderr tail missing: %v", pe)
	}
}

func TestRunCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Run(ctx, nil, "go", "version")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestAvailableAndExists(t *testing.T) {
	if !Available("go") {
		t.Error("go should be on PATH")
	}
	if Available("definitely-not-a-binary-xyz") {
		t.Error("unexpected binary")
	}
	f := filepath.Join(t.TempDir(), "x")
	if Exists(f) {
		t.Error("file should not exist yet")
	}
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !Exists(f) || Exists(filepath.Dir(f)) {
		t.Error("Exists wrong")
	}
	e := &Error{Name: "x", Err: errors.New("boom")}
	if e.Error() != "x: boom" {
		t.Errorf("Error() = %q", e.Error())
	}
}
