package domain

import (
	"fmt"
	"strings"
	"time"
)

// Layout is how a 16:9 source is arranged in the 9:16 final video.
type Layout string

// Supported layouts (CLAUDE.md §10).
const (
	// LayoutBlur centres the 16:9 video over a zoomed, blurred copy of itself.
	LayoutBlur Layout = "blur"
	// LayoutStack puts the facecam region on top and the gameplay region below.
	LayoutStack Layout = "stack"
	// LayoutCrop is a simple centre crop.
	LayoutCrop Layout = "crop"
)

// Valid reports whether l is a known layout.
func (l Layout) Valid() bool {
	switch l {
	case LayoutBlur, LayoutStack, LayoutCrop:
		return true
	}
	return false
}

// Rect is a region in normalized coordinates (0–1) of the source frame.
type Rect struct {
	X, Y, W, H float64
}

// Valid reports whether the rectangle is non-empty and inside the frame.
func (r Rect) Valid() bool {
	return r.X >= 0 && r.Y >= 0 && r.W > 0 && r.H > 0 && r.X+r.W <= 1.000001 && r.Y+r.H <= 1.000001
}

// StackRegions are the facecam and gameplay regions for LayoutStack.
type StackRegions struct {
	Facecam  Rect
	Gameplay Rect
}

// Offsets align each signal source with the captured stream timeline (§9).
type Offsets struct {
	// CaptureLatency is how far the captured HLS lags behind real time.
	CaptureLatency time.Duration
	// Chat is the viewers' delay plus reaction time: chat reacts this much
	// after the event happened.
	Chat time.Duration
	// Clip is the delay applied to viewer clips without vod_offset.
	Clip time.Duration
}

// Streamer is a channel from the allowlist.
type Streamer struct {
	Login       string
	Language    string
	AllowsClips bool
	Layout      Layout
	Stack       StackRegions
	Offsets     Offsets
}

// Validate checks the streamer configuration.
func (s Streamer) Validate() error {
	if s.Login == "" {
		return fmt.Errorf("%w: streamer login is empty", ErrInvalidArgument)
	}
	if !s.Layout.Valid() {
		return fmt.Errorf("%w: streamer %s: unknown layout %q", ErrInvalidArgument, s.Login, s.Layout)
	}
	if s.Layout == LayoutStack && (!s.Stack.Facecam.Valid() || !s.Stack.Gameplay.Valid()) {
		return fmt.Errorf("%w: streamer %s: stack layout needs valid facecam and gameplay regions", ErrInvalidArgument, s.Login)
	}
	if s.Offsets.CaptureLatency < 0 || s.Offsets.Chat < 0 || s.Offsets.Clip < 0 {
		return fmt.Errorf("%w: streamer %s: offsets must be >= 0", ErrInvalidArgument, s.Login)
	}
	return nil
}

// StreamID identifies one live broadcast (Twitch stream id).
type StreamID string

// LiveStream is a broadcast currently on air.
type LiveStream struct {
	ID            StreamID
	BroadcasterID string
	Login         string
	DisplayName   string
	Title         string
	GameID        string // Twitch category id
	Game          string // Twitch category name
	Language      string
	Viewers       int
	StartedAt     time.Time
}

// GameFilter lists the categories whose lives are monitored. An entry matches
// the category id exactly or its name ignoring case, so both "1867279146" and
// "Grand Theft Auto VI" select GTA VI. An empty filter allows every category.
type GameFilter []string

// Allows reports whether the live is streaming one of the listed categories.
// A live without a category only passes an empty filter.
func (f GameFilter) Allows(l LiveStream) bool {
	if len(f) == 0 {
		return true
	}
	for _, g := range f {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		if g == l.GameID || strings.EqualFold(g, strings.TrimSpace(l.Game)) {
			return true
		}
	}
	return false
}

// ViewerClip is a clip created by a viewer during a broadcast.
type ViewerClip struct {
	ID            string
	BroadcasterID string
	Title         string
	ViewCount     int
	CreatedAt     time.Time
	Duration      time.Duration
	// VODOffset is where the clip starts in the VOD; only meaningful when
	// HasVODOffset is true (Twitch fills it minutes after creation, if ever).
	VODOffset    time.Duration
	HasVODOffset bool
}
