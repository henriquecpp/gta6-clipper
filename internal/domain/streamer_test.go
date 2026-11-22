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

func TestGameFilterAllows(t *testing.T) {
	gta := LiveStream{GameID: "1867279146", Game: "Grand Theft Auto VI"}
	chatting := LiveStream{GameID: "509658", Game: "Just Chatting"}
	none := LiveStream{}
	cases := []struct {
		name   string
		filter GameFilter
		live   LiveStream
		want   bool
	}{
		{"empty filter allows any category", nil, chatting, true},
		{"empty filter allows a live without category", GameFilter{}, none, true},
		{"by name", GameFilter{"Grand Theft Auto VI"}, gta, true},
		{"by name ignoring case and spaces", GameFilter{"  grand theft auto vi "}, gta, true},
		{"by id", GameFilter{"1867279146"}, gta, true},
		{"other category", GameFilter{"Grand Theft Auto VI"}, chatting, false},
		{"any of several", GameFilter{"Just Chatting", "1867279146"}, chatting, true},
		{"no category never matches a filter", GameFilter{"Grand Theft Auto VI"}, none, false},
		{"blank entries match nothing", GameFilter{"", "  "}, none, false},
		{"id is not matched by prefix", GameFilter{"186727914"}, gta, false},
	}
	for _, c := range cases {
		if got := c.filter.Allows(c.live); got != c.want {
			t.Errorf("%s: Allows = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAutoLayout(t *testing.T) {
	if !LayoutAuto.Valid() {
		t.Fatal("auto must be a valid layout")
	}
	if err := (Streamer{Login: "a", Layout: LayoutAuto}).Validate(); err != nil {
		t.Fatalf("auto without fallback regions: %v", err)
	}
	ok := Streamer{Login: "a", Layout: LayoutAuto, Stack: StackRegions{Facecam: Rect{X: 0.7, W: 0.3, H: 0.3}, Gameplay: FullFrame}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("auto with fallback regions: %v", err)
	}
	bad := Streamer{Login: "a", Layout: LayoutAuto, Stack: StackRegions{Facecam: Rect{X: 0.9, W: 0.3, H: 0.3}}}
	if err := bad.Validate(); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("auto with invalid fallback regions: %v", err)
	}
}
