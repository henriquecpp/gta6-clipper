//go:build gpu

package whisper

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestGPUTranscribeRealWhisper transcribes the TTS sample with the real
// whisper-server (CUDA) of the compose stack (WHISPER_URL).
func TestGPUTranscribeRealWhisper(t *testing.T) {
	url := os.Getenv("WHISPER_URL")
	if url == "" {
		t.Skip("WHISPER_URL not set")
	}
	c := NewClient(url, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for c.Health(ctx) != nil {
		select {
		case <-ctx.Done():
			t.Fatal("whisper not healthy")
		case <-time.After(2 * time.Second):
		}
	}
	tr := NewTranscriber(c, WithTimeout(2*time.Minute), WithBeamSize(5))
	got, err := tr.Transcribe(ctx, "../../../../testdata/speech/pt_sample.wav", "pt")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ToLower(got.Text())
	if got.Language != "pt" || !strings.Contains(text, "ponte") || !strings.Contains(text, "viatura") {
		t.Fatalf("transcript (%s): %s", got.Language, got.Text())
	}
	for i := 1; i < len(got.Words); i++ {
		if got.Words[i].Start < got.Words[i-1].Start-500*time.Millisecond {
			t.Fatalf("word times not monotonic at %d: %+v", i, got.Words[i-1:i+1])
		}
	}
	t.Logf("%d words: %s", len(got.Words), got.Text())
}
