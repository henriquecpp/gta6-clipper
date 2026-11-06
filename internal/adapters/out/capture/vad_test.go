package capture

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// readWAV reads a 16-bit PCM mono WAV (as written by scripts/make-testdata.ps1).
func readWAV(t *testing.T, path string) ([]int16, int) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		t.Fatal("not a WAV file")
	}
	var rate int
	for off := 12; off+8 <= len(b); {
		id := string(b[off : off+4])
		size := int(binary.LittleEndian.Uint32(b[off+4 : off+8]))
		body := b[off+8 : min(off+8+size, len(b))]
		switch id {
		case "fmt ":
			if binary.LittleEndian.Uint16(body[0:2]) != 1 || binary.LittleEndian.Uint16(body[2:4]) != 1 || binary.LittleEndian.Uint16(body[14:16]) != 16 {
				t.Fatal("want 16-bit PCM mono")
			}
			rate = int(binary.LittleEndian.Uint32(body[4:8]))
		case "data":
			s := make([]int16, len(body)/2)
			for i := range s {
				s[i] = int16(binary.LittleEndian.Uint16(body[2*i:]))
			}
			return s, rate
		}
		off += 8 + size + size%2
	}
	t.Fatal("no data chunk")
	return nil, 0
}

func frames(samples []int16, n int) [][]int16 {
	var out [][]int16
	for i := 0; i+n <= len(samples); i += n {
		out = append(out, samples[i:i+n])
	}
	return out
}

func TestVADOnSpeechSample(t *testing.T) {
	samples, rate := readWAV(t, filepath.Join("..", "..", "..", "..", "testdata", "speech", "en_sample.wav"))
	if rate != 16000 {
		t.Fatalf("rate = %d", rate)
	}
	var words []struct {
		StartMS int64 `json:"start_ms"`
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "testdata", "speech", "en_sample.words.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &words); err != nil {
		t.Fatal(err)
	}
	last := time.Duration(words[len(words)-1].StartMS) * time.Millisecond

	// Mix a quiet pink-ish noise bed under the speech, like game audio.
	rng := rand.New(rand.NewPCG(1, 2))
	prev := 0.0
	mixed := make([]int16, len(samples)+3*rate) // plus 3 s of noise only
	for i := range mixed {
		prev = 0.95*prev + 0.05*(rng.Float64()*2-1)
		v := prev * 300
		if i < len(samples) {
			v += float64(samples[i])
		}
		mixed[i] = int16(max(min(v, 32767), -32768))
	}

	v := NewVAD(rate, 30)
	speechIn, totalIn, speechAfter, totalAfter := 0, 0, 0, 0
	for i, f := range frames(mixed, rate*30/1000) {
		_, speech := v.Frame(f)
		at := time.Duration(i) * 30 * time.Millisecond
		switch {
		case at >= time.Duration(words[0].StartMS)*time.Millisecond && at < last:
			totalIn++
			if speech {
				speechIn++
			}
		case at > time.Duration(len(samples))*time.Second/time.Duration(rate)+300*time.Millisecond:
			totalAfter++
			if speech {
				speechAfter++
			}
		}
	}
	if r := float64(speechIn) / float64(totalIn); r < 0.6 {
		t.Errorf("speech recall = %.2f (%d/%d), want >= 0.6", r, speechIn, totalIn)
	}
	if r := float64(speechAfter) / float64(totalAfter); r > 0.05 {
		t.Errorf("false positives on noise = %.2f (%d/%d), want <= 0.05", r, speechAfter, totalAfter)
	}
}

func TestVADRejectsSilenceNoiseAndRumble(t *testing.T) {
	const rate = 16000
	n := rate * 30 / 1000
	v := NewVAD(rate, 30)
	if db, s := v.Frame(make([]int16, n)); s || db != silenceDBFS {
		t.Fatalf("silence: db=%v speech=%v", db, s)
	}
	if db, s := v.Frame(nil); s || db != silenceDBFS {
		t.Fatal("empty frame")
	}
	rng := rand.New(rand.NewPCG(3, 4))
	white := func() []int16 {
		f := make([]int16, n)
		for i := range f {
			f[i] = int16((rng.Float64()*2 - 1) * 8000)
		}
		return f
	}
	rumble := func(i int) []int16 { // 60 Hz explosion-like rumble
		f := make([]int16, n)
		for j := range f {
			f[j] = int16(20000 * math.Sin(2*math.Pi*60*float64(i*n+j)/rate))
		}
		return f
	}
	speechFrames := 0
	for i := range 300 {
		var f []int16
		if i%2 == 0 {
			f = white()
		} else {
			f = rumble(i)
		}
		if db, s := v.Frame(f); s {
			speechFrames++
		} else if db > 0 || db < silenceDBFS {
			t.Fatalf("dbfs out of range: %v", db)
		}
	}
	if speechFrames > 15 {
		t.Errorf("white noise / rumble classified as speech in %d of 300 frames", speechFrames)
	}
}

func TestDBFSOfFullScaleSine(t *testing.T) {
	const rate = 16000
	f := make([]int16, 480)
	for i := range f {
		f[i] = int16(32767 * math.Sin(2*math.Pi*1000*float64(i)/rate))
	}
	db, _ := NewVAD(rate, 30).Frame(f)
	if math.Abs(db-(-3.01)) > 0.1 {
		t.Fatalf("full-scale sine = %.2f dBFS, want ≈ -3.01", db)
	}
}
