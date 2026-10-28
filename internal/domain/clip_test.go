package domain

import (
	"errors"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

func newTestClip(t *testing.T) *Clip {
	t.Helper()
	c, err := NewClip(NewClipParams{
		ID:       "c1",
		Streamer: "streamer",
		Live:     LiveStream{ID: "s1", Game: "GTA VI", Title: "title"},
		Moment:   Moment{Peak: StreamPos(90 * time.Second), Score: 4.2, Values: map[SignalKind]float64{SignalVoice: 3.8}},
		Window:   Window{From: StreamPos(30 * time.Second), To: StreamPos(120 * time.Second)},
		Timeline: []SignalPoint{{Pos: StreamPos(30 * time.Second), Voice: 1}},
		Source:   "/pending/c1.ts",
	}, t0)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNewClipValidation(t *testing.T) {
	base := NewClipParams{
		ID: "c1", Streamer: "s", Source: "/x",
		Moment: Moment{Peak: StreamPos(10 * time.Second)},
		Window: Window{From: 0, To: StreamPos(20 * time.Second)},
	}
	cases := map[string]func(p *NewClipParams){
		"empty id":       func(p *NewClipParams) { p.ID = "" },
		"empty streamer": func(p *NewClipParams) { p.Streamer = "" },
		"empty source":   func(p *NewClipParams) { p.Source = "" },
		"bad window":     func(p *NewClipParams) { p.Window = Window{From: 5, To: 5} },
		"peak outside":   func(p *NewClipParams) { p.Moment.Peak = StreamPos(time.Minute) },
	}
	for name, mutate := range cases {
		p := base
		mutate(&p)
		if _, err := NewClip(p, t0); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: err = %v, want ErrInvalidArgument", name, err)
		}
	}
	c, err := NewClip(base, t0)
	if err != nil {
		t.Fatal(err)
	}
	if c.State != StateDetected || !c.CreatedAt.Equal(t0) {
		t.Fatalf("unexpected clip %+v", c)
	}
	if at, ok := c.EnteredAt(StateDetected); !ok || !at.Equal(t0) {
		t.Fatal("detected time not recorded")
	}
}

func TestTransitionTable(t *testing.T) {
	valid := map[[2]ClipState]bool{
		{StateDetected, StatePreviewSent}:  true,
		{StateDetected, StateFailed}:       true,
		{StatePreviewSent, StateApproved}:  true,
		{StatePreviewSent, StateDiscarded}: true,
		{StatePreviewSent, StateExpired}:   true,
		{StateApproved, StateEditing}:      true,
		{StateEditing, StateRendered}:      true,
		{StateRendered, StateDelivered}:    true,
		{StateEditing, StateFailed}:        true,
		{StateRendered, StateFailed}:       true,
		{StateFailed, StateApproved}:       true,
	}
	for _, from := range ClipStates {
		for _, to := range ClipStates {
			want := valid[[2]ClipState{from, to}]
			if got := CanTransition(from, to); got != want {
				t.Errorf("CanTransition(%s, %s) = %v, want %v", from, to, got, want)
			}
			c := &Clip{ID: "x", State: from}
			var err error
			if to == StateFailed {
				err = c.Fail("boom", t0)
			} else {
				err = c.Transition(to, t0)
			}
			if want && err != nil {
				t.Errorf("%s → %s: unexpected error %v", from, to, err)
			}
			if !want && !errors.Is(err, ErrInvalidTransition) {
				t.Errorf("%s → %s: err = %v, want ErrInvalidTransition", from, to, err)
			}
			if !want && c.State != from {
				t.Errorf("%s → %s: state changed on invalid transition", from, to)
			}
		}
	}
}

func TestFinalStates(t *testing.T) {
	for _, s := range ClipStates {
		final := s == StateDiscarded || s == StateExpired || s == StateDelivered
		if s.Final() != final {
			t.Errorf("%s.Final() = %v", s, s.Final())
		}
		if final && len(transitions[s]) != 0 {
			t.Errorf("final state %s has outgoing transitions", s)
		}
		if !s.Valid() {
			t.Errorf("%s not valid", s)
		}
	}
	if ClipState("bogus").Valid() {
		t.Error("bogus state valid")
	}
}

func TestHappyPathRecordsDecisionAndTimes(t *testing.T) {
	c := newTestClip(t)
	steps := []ClipState{StatePreviewSent, StateApproved, StateEditing, StateRendered, StateDelivered}
	for i, s := range steps {
		if err := c.Transition(s, t0.Add(time.Duration(i+1)*time.Minute)); err != nil {
			t.Fatalf("→ %s: %v", s, err)
		}
	}
	if c.Decision != DecisionApproved || !c.DecidedAt.Equal(t0.Add(2*time.Minute)) {
		t.Errorf("decision = %s at %v", c.Decision, c.DecidedAt)
	}
	if !c.UpdatedAt.Equal(t0.Add(5 * time.Minute)) {
		t.Errorf("updated at %v", c.UpdatedAt)
	}
	if !c.State.Final() {
		t.Error("delivered should be final")
	}
	// Duplicate approval is rejected (idempotency is enforced by callers via InTx).
	if err := c.Transition(StateApproved, t0); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("duplicate approval err = %v", err)
	}
}

func TestDecisionsForDiscardAndExpire(t *testing.T) {
	for to, want := range map[ClipState]Decision{StateDiscarded: DecisionDiscarded, StateExpired: DecisionExpired} {
		c := newTestClip(t)
		_ = c.Transition(StatePreviewSent, t0)
		if err := c.Transition(to, t0.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if c.Decision != want {
			t.Errorf("%s: decision %s", to, c.Decision)
		}
	}
}

func TestFailAndRetry(t *testing.T) {
	c := newTestClip(t)
	_ = c.Transition(StatePreviewSent, t0)
	_ = c.Transition(StateApproved, t0)
	_ = c.Transition(StateEditing, t0)
	if err := c.Fail("  ", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if c.FailedFrom != StateEditing || c.FailureReason != "unknown error" {
		t.Fatalf("failed from %s reason %q", c.FailedFrom, c.FailureReason)
	}
	if err := c.Transition(StateApproved, t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if c.FailureReason != "" || c.FailedFrom != "" {
		t.Error("retry should clear failure")
	}
	// Retry is not a human decision on the preview.
	if !c.DecidedAt.Equal(t0) {
		t.Errorf("decided at changed: %v", c.DecidedAt)
	}
}

func TestIsExpired(t *testing.T) {
	c := newTestClip(t)
	ttl := 6 * time.Hour
	if c.IsExpired(t0.Add(7*time.Hour), ttl) {
		t.Error("detected clip cannot expire")
	}
	if _, ok := c.ApprovalDeadline(ttl); ok {
		t.Error("no deadline before preview")
	}
	_ = c.Transition(StatePreviewSent, t0)
	if c.IsExpired(t0.Add(ttl-time.Second), ttl) {
		t.Error("expired too early")
	}
	if !c.IsExpired(t0.Add(ttl), ttl) {
		t.Error("should be expired at deadline")
	}
}

func TestCloneIsDeep(t *testing.T) {
	c := newTestClip(t)
	cp := c.Clone()
	cp.Moment.Values[SignalVoice] = 99
	cp.Timeline[0].Voice = 99
	cp.StateTimes[StateDetected] = time.Time{}
	if c.Moment.Values[SignalVoice] == 99 || c.Timeline[0].Voice == 99 || c.StateTimes[StateDetected].IsZero() {
		t.Fatal("clone shares memory")
	}
	var nilClip *Clip
	if nilClip.Clone() != nil {
		t.Fatal("nil clone")
	}
}
