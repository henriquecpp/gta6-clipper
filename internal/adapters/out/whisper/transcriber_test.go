package whisper

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func fakeWhisper(t *testing.T, status int, concurrent *atomic.Int32, maxSeen *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(status)
			return
		case "/inference":
		default:
			http.NotFound(w, r)
			return
		}
		if concurrent != nil {
			n := concurrent.Add(1)
			defer concurrent.Add(-1)
			for {
				m := maxSeen.Load()
				if n <= m || maxSeen.CompareAndSwap(m, n) {
					break
				}
			}
			time.Sleep(30 * time.Millisecond)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.FormValue("response_format") != "verbose_json" || r.FormValue("language") != "auto" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = f.Close()
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = w.Write([]byte(sampleVerbose))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func fakeExtract(context.Context, string) ([]byte, error) { return []byte("RIFF....WAVE"), nil }

func TestTranscribe(t *testing.T) {
	srv := fakeWhisper(t, http.StatusOK, nil, nil)
	tr := NewTranscriber(NewClient(srv.URL, srv.Client()), withExtractor(fakeExtract), WithTimeout(time.Minute), WithTimestampSource(TimestampsToken))
	got, err := tr.Transcribe(context.Background(), "/pending/x.ts")
	if err != nil {
		t.Fatal(err)
	}
	if got.Language != "en" || len(got.Words) != 5 || got.Words[0].Start != 100*time.Millisecond {
		t.Fatalf("transcript = %+v", got)
	}
	if err := NewClient(srv.URL, srv.Client()).Health(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestTranscribeErrors(t *testing.T) {
	loading := fakeWhisper(t, http.StatusServiceUnavailable, nil, nil)
	tr := NewTranscriber(NewClient(loading.URL, loading.Client()), withExtractor(fakeExtract))
	if _, err := tr.Transcribe(context.Background(), "x"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("503 err = %v", err)
	}
	if err := NewClient(loading.URL, nil).Health(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("health err = %v", err)
	}
	broken := fakeWhisper(t, http.StatusInternalServerError, nil, nil)
	tr = NewTranscriber(NewClient(broken.URL, broken.Client()), withExtractor(fakeExtract))
	if _, err := tr.Transcribe(context.Background(), "x"); err == nil || errors.Is(err, ErrUnavailable) {
		t.Fatalf("500 err = %v", err)
	}
	down := NewTranscriber(NewClient("http://127.0.0.1:1", nil), withExtractor(fakeExtract))
	if _, err := down.Transcribe(context.Background(), "x"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("connection refused err = %v", err)
	}
	noAudio := NewTranscriber(NewClient(broken.URL, nil), withExtractor(func(context.Context, string) ([]byte, error) {
		return nil, errors.New("no audio stream")
	}))
	if _, err := noAudio.Transcribe(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "extract audio") {
		t.Fatalf("extract err = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	busy := NewTranscriber(NewClient(broken.URL, nil), withExtractor(fakeExtract))
	busy.sem <- struct{}{}
	if _, err := busy.Transcribe(ctx, "x"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled err = %v", err)
	}
}

func TestTranscriberLimitsConcurrency(t *testing.T) {
	var concurrent, maxSeen atomic.Int32
	srv := fakeWhisper(t, http.StatusOK, &concurrent, &maxSeen)
	tr := NewTranscriber(NewClient(srv.URL, srv.Client()), withExtractor(fakeExtract), WithWorkers(1))
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := tr.Transcribe(context.Background(), "x"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if maxSeen.Load() != 1 {
		t.Fatalf("max concurrent requests = %d, want 1", maxSeen.Load())
	}
	if got := strings.Join(extractArgs("/p/a.ts"), " "); !strings.Contains(got, "-ar 16000") || !strings.Contains(got, "-ac 1") {
		t.Fatalf("extract args = %s", got)
	}
}
