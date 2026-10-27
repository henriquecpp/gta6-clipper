package domain

import (
	"errors"
	"testing"
	"time"
)

func TestStreamerValidate(t *testing.T) {
	ok := Streamer{Login: "a", Layout: LayoutBlur}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	stack := Streamer{Login: "b", Layout: LayoutStack, Stack: StackRegions{
		Facecam: Rect{X: 0.7, Y: 0, W: 0.3, H: 0.3}, Gameplay: Rect{X: 0, Y: 0, W: 1, H: 1},
	}}
	if err := stack.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := []Streamer{
		{Layout: LayoutBlur},
		{Login: "a", Layout: "weird"},
		{Login: "a", Layout: LayoutStack},
		{Login: "a", Layout: LayoutStack, Stack: StackRegions{Facecam: Rect{X: 0.8, W: 0.3, H: 0.1}, Gameplay: Rect{W: 1, H: 1}}},
		{Login: "a", Layout: LayoutCrop, Offsets: Offsets{Chat: -time.Second}},
	}
	for i, s := range bad {
		if err := s.Validate(); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("case %d: err = %v", i, err)
		}
	}
}

func TestStreamPosAndWindow(t *testing.T) {
	if s := StreamPos(time.Hour + 23*time.Minute + 45*time.Second + 600*time.Millisecond).String(); s != "01:23:45" {
		t.Errorf("String = %q", s)
	}
	if s := StreamPos(-5 * time.Second).String(); s != "-00:00:05" {
		t.Errorf("negative String = %q", s)
	}
	w := Window{From: StreamPos(10 * time.Second), To: StreamPos(40 * time.Second)}
	if w.Duration() != 30*time.Second || !w.Valid() || !w.Contains(StreamPos(10*time.Second)) || w.Contains(StreamPos(41*time.Second)) {
		t.Errorf("window helpers wrong: %+v", w)
	}
	if (Window{From: -1, To: 5}).Valid() {
		t.Error("negative window valid")
	}
	if StreamPos(time.Second).Duration() != time.Second {
		t.Error("Duration")
	}
}

func TestTranscriptHelpers(t *testing.T) {
	tr := Transcript{Language: "en", Words: []Word{
		{Text: "hello", Start: 0, End: time.Second},
		{Text: " ", Start: time.Second, End: time.Second},
		{Text: "world", Start: 2 * time.Second, End: 3 * time.Second},
	}}
	if tr.Text() != "hello world" {
		t.Errorf("Text = %q", tr.Text())
	}
	if got := tr.Between(1500*time.Millisecond, 5*time.Second); len(got) != 1 || got[0].Text != "world" {
		t.Errorf("Between = %+v", got)
	}
}

func TestLayoutValid(t *testing.T) {
	for _, l := range []Layout{LayoutBlur, LayoutStack, LayoutCrop} {
		if !l.Valid() {
			t.Errorf("%s invalid", l)
		}
	}
	if Layout("x").Valid() {
		t.Error("x valid")
	}
}
