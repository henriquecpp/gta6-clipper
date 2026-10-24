// Package proc runs external programs (ffmpeg, streamlink, nvidia-smi) safely:
// always exec.CommandContext with an argument slice (never a shell), bounded
// stderr capture whose tail is included in errors, graceful termination on
// cancellation and a Wait so no zombie is left behind (CLAUDE.md §6).
package proc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

// DefaultStderrBytes is how much stderr is retained per process.
const DefaultStderrBytes = 16 << 10

// DefaultWaitDelay is how long a cancelled process gets to exit after SIGTERM
// before it is killed.
const DefaultWaitDelay = 5 * time.Second

// TailBuffer is an io.Writer that keeps only the last Max bytes written.
type TailBuffer struct {
	Max int
	mu  sync.Mutex
	buf []byte
}

// Write appends p, discarding the oldest bytes beyond Max.
func (t *TailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	max := t.Max
	if max <= 0 {
		max = DefaultStderrBytes
	}
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - max; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(p), nil
}

// LastLines returns up to n trailing non-empty lines.
func (t *TailBuffer) LastLines(n int) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	lines := strings.Split(strings.TrimSpace(string(t.buf)), "\n")
	var keep []string
	for i := len(lines) - 1; i >= 0 && len(keep) < n; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			keep = append(keep, l)
		}
	}
	for i, j := 0, len(keep)-1; i < j; i, j = i+1, j-1 {
		keep[i], keep[j] = keep[j], keep[i]
	}
	return strings.Join(keep, " | ")
}

// Error is returned when a process fails; it carries the stderr tail.
type Error struct {
	Name   string
	Err    error
	Stderr string
}

func (e *Error) Error() string {
	if e.Stderr == "" {
		return fmt.Sprintf("%s: %v", e.Name, e.Err)
	}
	return fmt.Sprintf("%s: %v: %s", e.Name, e.Err, e.Stderr)
}

func (e *Error) Unwrap() error { return e.Err }

// Command builds an exec.Cmd bound to ctx that is terminated gracefully
// (SIGTERM, then SIGKILL after DefaultWaitDelay) when ctx is cancelled.
func Command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Cancel = func() error {
		if runtime.GOOS == "windows" {
			return cmd.Process.Kill()
		}
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = DefaultWaitDelay
	return cmd
}

// Run executes the program to completion and returns its stdout. stdin may be nil.
func Run(ctx context.Context, stdin io.Reader, name string, args ...string) ([]byte, error) {
	cmd := Command(ctx, name, args...)
	var stdout bytes.Buffer
	stderr := &TailBuffer{Max: DefaultStderrBytes}
	cmd.Stdin = stdin
	cmd.Stdout = &stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = errors.Join(ctxErr, err)
		}
		return stdout.Bytes(), &Error{Name: name, Err: err, Stderr: stderr.LastLines(8)}
	}
	return stdout.Bytes(), nil
}

// Available reports whether name can be found on PATH.
func Available(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// Exists reports whether a regular file exists at path.
func Exists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}
