package capture

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

func TestStallWatchStopsACaptureWithoutAudio(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newStallWatch(time.Minute, time.Now)
		stopped := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			w.run(t.Context(), func() { close(stopped) })
		}()

		time.Sleep(59 * time.Second)
		synctest.Wait()
		select {
		case <-stopped:
			t.Fatal("stopped before the timeout")
		default:
		}
		time.Sleep(time.Minute)
		<-done
		select {
		case <-stopped:
		default:
			t.Fatal("not stopped after a minute without audio")
		}
		if !w.stalled.Load() {
			t.Fatal("capture not marked as stalled")
		}
	})
}

func TestStallWatchKeepsACaptureWithAudio(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newStallWatch(time.Minute, time.Now)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			w.run(ctx, func() { t.Error("a capture with audio was stopped") })
		}()
		// Audio every 30 s for 10 min (gaps shorter than the timeout, like ads).
		for range 20 {
			time.Sleep(30 * time.Second)
			w.progress()
		}
		cancel()
		<-done
		if w.stalled.Load() {
			t.Fatal("marked as stalled")
		}
	})
}
