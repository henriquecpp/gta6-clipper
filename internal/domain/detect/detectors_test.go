package detect

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

func posNS(ns int64) domain.StreamPos { return domain.StreamPos(time.Duration(ns)) }

func at(d time.Duration) domain.StreamPos { return domain.StreamPos(d) }

// feedVoice sends 30 ms frames from start to end at the given level.
func feedVoice(v *Voice, start, end time.Duration, dbfs float64, speech bool, jitter bool) {
	i := 0
	for p := start; p < end; p += 30 * time.Millisecond {
		level := dbfs
		if jitter {
			level += []float64{-1.5, 0.5, 1.5, -0.5}[i%4]
		}
		v.Observe(domain.AudioFrame{Pos: at(p), DBFS: level, Speech: speech})
		i++
	}
}

func TestVoiceDetectsLoudSpeech(t *testing.T) {
	v := NewVoice(VoiceConfig{Window: 400 * time.Millisecond, Alpha: 0.02, StaleAfter: 2 * time.Second})
	feedVoice(v, 0, 5*time.Minute, -30, true, true)
	if z := v.Value(at(5 * time.Minute)); math.Abs(z) > 1.5 {
		t.Fatalf("steady speech z = %v", z)
	}
	// Shouting for 2 s.
	feedVoice(v, 5*time.Minute, 5*time.Minute+2*time.Second, -12, true, false)
	feedVoice(v, 5*time.Minute+2*time.Second, 5*time.Minute+2500*time.Millisecond, -30, true, true)
	if z := v.Value(at(5*time.Minute + 2*time.Second)); z < 4 {
		t.Fatalf("shout z = %v, want > 4", z)
	}
	if v.Kind() != domain.SignalVoice {
		t.Fatal("kind")
	}
	v.Advance(0) // no-op
}

func TestVoiceIgnoresNonSpeechAndGoesStale(t *testing.T) {
	v := NewVoice(VoiceConfig{Window: 400 * time.Millisecond, Alpha: 0.02, StaleAfter: 2 * time.Second})
	feedVoice(v, 0, 2*time.Minute, -30, true, true)
	n := v.Baseline().Count()
	// An explosion (loud, not speech) must not move the baseline nor score.
	feedVoice(v, 2*time.Minute, 2*time.Minute+3*time.Second, -3, false, false)
	// Only the speech window that was open at 2:00 is closed (+1).
	if v.Baseline().Count() != n+1 {
		t.Fatalf("non-speech windows updated the baseline: %d → %d", n, v.Baseline().Count())
	}
	if z := v.Value(at(2*time.Minute + 3*time.Second)); z != 0 {
		t.Fatalf("value after 3 s without speech = %v, want 0 (stale)", z)
	}
	if v.Value(at(-time.Second)) != 0 {
		t.Fatal("value before any window must be 0")
	}
	// Out-of-order frames are dropped.
	v.Observe(domain.AudioFrame{Pos: at(time.Second), DBFS: 0, Speech: true})
}

func TestVoiceTrimsHistory(t *testing.T) {
	v := NewVoice(VoiceConfig{Window: 400 * time.Millisecond, Alpha: 0.02, Retention: time.Minute})
	feedVoice(v, 0, 3*time.Minute, -30, true, true)
	if len(v.history) > 160 {
		t.Fatalf("history not trimmed: %d windows", len(v.history))
	}
	if v.Value(at(10*time.Second)) != 0 {
		t.Fatal("trimmed window still visible")
	}
}

func TestChatWeightAndBurst(t *testing.T) {
	c := NewChat(ChatConfig{
		Window: 10 * time.Second, Sustain: 4 * time.Second, Alpha: 0.02,
		Terms: map[string]float64{"KEKW": 2, "OMEGALUL": 2.5, "LUL": 1.5, "kkkk": 2},
	})
	if c.Weight("hello") != 1 || c.Weight("KEKW KEKW KEKW") != 2 || c.Weight("OMEGALUL") != 2.5 || c.Weight("kkkkkkkk") != 2 || c.Weight("kekw") != 1 {
		t.Fatal("weights")
	}
	c.SetViewers(1000)
	if c.Value(0) != 0 {
		t.Fatal("value before advance must be 0")
	}
	// 5 minutes of 2 msg/s with mild variation.
	for s := range 300 {
		n := 2
		if s%3 == 0 {
			n = 3
		}
		for i := range n {
			c.Observe(at(time.Duration(s)*time.Second+time.Duration(i)*100*time.Millisecond), "hi")
		}
		c.Advance(at(time.Duration(s) * time.Second))
	}
	if z := c.Value(at(299 * time.Second)); math.Abs(z) > 1.5 {
		t.Fatalf("steady chat z = %v", z)
	}
	// Burst: 15 laughing messages per second for 8 s.
	for s := 300; s < 308; s++ {
		for i := range 15 {
			c.Observe(at(time.Duration(s)*time.Second+time.Duration(i)*50*time.Millisecond), "KEKW")
		}
		c.Advance(at(time.Duration(s) * time.Second))
	}
	if z := c.RawZ(at(301 * time.Second)); z < 3 {
		t.Fatalf("raw z after 2 s of burst = %v", z)
	}
	if z := c.Value(at(307 * time.Second)); z < 3 {
		t.Fatalf("sustained z = %v, want > 3", z)
	}
	if c.Kind() != domain.SignalChat {
		t.Fatal("kind")
	}
}

func TestChatSustainRequiresDuration(t *testing.T) {
	c := NewChat(ChatConfig{Window: 2 * time.Second, Sustain: 4 * time.Second, Alpha: 0.05, MinViewers: 1})
	for s := range 200 {
		c.Observe(at(time.Duration(s)*time.Second), "a")
		c.Advance(at(time.Duration(s) * time.Second))
	}
	// One-second spike only.
	for range 40 {
		c.Observe(at(200*time.Second), "a")
	}
	c.Advance(at(200 * time.Second))
	if c.RawZ(at(200*time.Second)) < 3 {
		t.Fatalf("raw z = %v", c.RawZ(at(200*time.Second)))
	}
	if z := c.Value(at(200 * time.Second)); z > 1 {
		t.Fatalf("a 1 s spike must not be sustained: %v", z)
	}
}

func TestChatViewerNormalizationAndGaps(t *testing.T) {
	small := NewChat(ChatConfig{Window: time.Second, Alpha: 0.1, Retention: time.Minute})
	small.SetViewers(10) // floored at 50
	small.Observe(0, "x")
	small.Advance(0)
	// A long gap skips ahead instead of replaying hours of seconds.
	small.Advance(at(10 * time.Hour))
	if len(small.zs) > 62 {
		t.Fatalf("zs not trimmed after gap: %d", len(small.zs))
	}
	// Messages far in the past are ignored.
	small.Observe(at(time.Hour), "old")
	if _, ok := small.buckets[3600]; ok {
		t.Fatal("old message recorded")
	}
}

func TestClipsCountAndClusters(t *testing.T) {
	c := NewClips(ClipsConfig{GroupWindow: 15 * time.Second, MinClips: 3, Retention: 10 * time.Minute})
	for i, p := range []time.Duration{100, 104, 110, 112, 300, 305, 600} {
		if !c.Observe(string(rune('a'+i)), at(p*time.Second)) {
			t.Fatal("new clip reported as duplicate")
		}
	}
	if c.Observe("a", at(100*time.Second)) {
		t.Fatal("duplicate not detected")
	}
	if got := c.Value(at(105 * time.Second)); got != 4 {
		t.Fatalf("count near 105 = %v, want 4", got)
	}
	if got := c.Value(at(300 * time.Second)); got != 2 {
		t.Fatalf("count near 300 = %v, want 2", got)
	}
	cl := c.Clusters()
	if len(cl) != 1 || cl[0].Count != 4 || cl[0].Center != at(110*time.Second) {
		t.Fatalf("clusters = %+v", cl)
	}
	c.Advance(at(700 * time.Second)) // retention 10 min drops clips before 100 s
	c.Advance(at(711 * time.Second))
	if got := c.Value(at(105 * time.Second)); got != 1 {
		t.Fatalf("after trim count = %v, want 1 (only 112 s)", got)
	}
	if c.Kind() != domain.SignalClips {
		t.Fatal("kind")
	}
	// Boundary: a clip exactly at pos+window counts.
	b := NewClips(ClipsConfig{GroupWindow: 10 * time.Second})
	b.Observe("x", at(20*time.Second))
	b.Observe("y", at(20*time.Second))
	if b.Value(at(10*time.Second)) != 2 {
		t.Fatal("boundary clips not counted")
	}
}

func TestClipsRequiredGrowsWithTheChannelClipRate(t *testing.T) {
	cfg := ClipsConfig{GroupWindow: 15 * time.Second, MinClips: 3, MinZ: 3.5, Retention: 15 * time.Minute}
	// rate builds a detector that saw perMin clips per minute, evenly, over the 10 minutes before now.
	rate := func(perMin int) *Clips {
		c := NewClips(cfg)
		n := 10 * perMin
		for i := range n {
			c.Observe(fmt.Sprintf("c%d", i), at(time.Duration(i)*10*time.Minute/time.Duration(n)))
		}
		return c
	}
	now := at(10 * time.Minute)
	tests := []struct {
		name   string
		perMin int
		want   int
	}{
		{"no clips yet: the floor", 0, 3},
		{"1 clip/min: still the floor", 1, 3},    // λ = 0.5 → ⌈0.5 + 3.5·0.71⌉ = 3
		{"caseoh-like (2/min)", 2, 5},            // λ = 1 → ⌈1 + 3.5⌉ = 5
		{"kaicenat-like (4/min)", 4, 7},          // λ = 2 → ⌈2 + 3.5·1.41⌉ = 7
		{"a very busy channel (10/min)", 10, 13}, // λ = 5 → ⌈5 + 3.5·2.24⌉ = 13
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rate(tt.perMin).Required(now); got != tt.want {
				t.Fatalf("Required = %d, want %d (rate %.3f/s)", got, tt.want, rate(tt.perMin).Rate(now))
			}
		})
	}
	// Right after the capture starts, a handful of clips is averaged over at
	// least 5 minutes instead of looking like a huge rate.
	early := NewClips(cfg)
	for i := range 4 {
		early.Observe(fmt.Sprintf("e%d", i), at(time.Duration(i)*10*time.Second))
	}
	if got := early.Required(at(40 * time.Second)); got != 3 {
		t.Fatalf("early Required = %d, want the floor 3", got)
	}
	// MinZ 0 keeps the fixed MinClips behaviour.
	plain := NewClips(ClipsConfig{GroupWindow: 15 * time.Second, MinClips: 3})
	for i := range 40 {
		plain.Observe(fmt.Sprintf("p%d", i), at(time.Duration(i)*15*time.Second))
	}
	if got := plain.Required(at(10 * time.Minute)); got != 3 {
		t.Fatalf("Required without MinZ = %d, want 3", got)
	}
}
