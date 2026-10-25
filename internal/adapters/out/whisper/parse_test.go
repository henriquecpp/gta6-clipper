package whisper

import (
	"testing"
	"time"
)

const sampleVerbose = `{
 "task":"transcribe","language":"english","duration":3.2,"text":" No way, did you",
 "detected_language":"english","detected_language_probability":0.98,
 "language_probabilities":{"en":0.98,"pt":0.01},
 "segments":[
  {"id":0,"text":" No way, did you","start":0.0,"end":1.5,"words":[
    {"word":"[_BEG_]","start":0.0,"end":0.0,"t_dtw":-1,"probability":0.9},
    {"word":" No","start":0.10,"end":0.30,"t_dtw":12,"probability":0.9},
    {"word":" way","start":0.32,"end":0.60,"t_dtw":34,"probability":0.8},
    {"word":",","start":0.60,"end":0.61,"t_dtw":60,"probability":0.7},
    {"word":" did","start":1.00,"end":1.15,"t_dtw":101,"probability":0.9},
    {"word":" y","start":1.20,"end":1.25,"t_dtw":121,"probability":0.5},
    {"word":"ou","start":1.25,"end":1.32,"t_dtw":125,"probability":0.7}
  ]},
  {"id":1,"text":" ok","start":2.0,"end":2.5,"words":[
    {"word":"ok","start":2.0,"end":2.4,"t_dtw":-1,"probability":1.0},
    {"word":"[_TT_250]","start":2.5,"end":2.5,"probability":1.0}
  ]}
 ]}`

func TestParseVerboseJSONTokens(t *testing.T) {
	tr, err := ParseVerboseJSON([]byte(sampleVerbose), TimestampsToken)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Language != "en" {
		t.Errorf("language = %q", tr.Language)
	}
	want := []struct {
		text       string
		start, end time.Duration
	}{
		{"No", 100 * time.Millisecond, 300 * time.Millisecond},
		{"way,", 320 * time.Millisecond, 610 * time.Millisecond},
		{"did", 1000 * time.Millisecond, 1150 * time.Millisecond},
		{"you", 1200 * time.Millisecond, 1320 * time.Millisecond},
		{"ok", 2000 * time.Millisecond, 2400 * time.Millisecond},
	}
	if len(tr.Words) != len(want) {
		t.Fatalf("got %d words: %+v", len(tr.Words), tr.Words)
	}
	for i, w := range want {
		g := tr.Words[i]
		if g.Text != w.text || g.Start != w.start || g.End != w.end {
			t.Errorf("word %d = %+v, want %+v", i, g, w)
		}
	}
	if p := tr.Words[3].Prob; p < 0.59 || p > 0.61 {
		t.Errorf("merged prob = %v, want 0.6", p)
	}
	if tr.Text() != "No way, did you ok" {
		t.Errorf("text = %q", tr.Text())
	}
}

func TestParseVerboseJSONDTW(t *testing.T) {
	tr, err := ParseVerboseJSON([]byte(sampleVerbose), TimestampsDTW)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Words[0].Start != 120*time.Millisecond {
		t.Errorf("dtw start = %v", tr.Words[0].Start)
	}
	// t_dtw = -1 falls back to the token timestamp.
	if tr.Words[4].Start != 2*time.Second {
		t.Errorf("fallback start = %v", tr.Words[4].Start)
	}
}

func TestParseVerboseJSONErrors(t *testing.T) {
	if _, err := ParseVerboseJSON([]byte("{"), TimestampsToken); err == nil {
		t.Error("expected decode error")
	}
	noTS := `{"language":"portuguese","segments":[{"words":[{"word":" oi","probability":1}]}]}`
	if _, err := ParseVerboseJSON([]byte(noTS), TimestampsToken); err == nil {
		t.Error("expected error when timestamps are missing")
	}
}

func TestDetectLanguageFallback(t *testing.T) {
	tr, err := ParseVerboseJSON([]byte(`{"language":"portuguese","segments":[]}`), TimestampsToken)
	if err != nil || tr.Language != "pt" {
		t.Fatalf("got %q, %v", tr.Language, err)
	}
	tr, _ = ParseVerboseJSON([]byte(`{"language":"klingon","segments":[]}`), TimestampsToken)
	if tr.Language != "klingon" {
		t.Fatalf("got %q", tr.Language)
	}
}
