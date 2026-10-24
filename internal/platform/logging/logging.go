// Package logging configures the process-wide slog logger.
//
// Standard field names (CLAUDE.md §6): streamer, stream_id, clip_id, stage,
// duration_ms, err. Tokens, keys and .env values must never be logged.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// Standard attribute keys shared by every package that logs.
const (
	KeyStreamer   = "streamer"
	KeyStreamID   = "stream_id"
	KeyClipID     = "clip_id"
	KeyStage      = "stage"
	KeyDurationMS = "duration_ms"
	KeyErr        = "err"
)

// Format selects the log encoding.
type Format string

// Supported formats. JSON is the production default.
const (
	FormatJSON Format = "json"
	FormatText Format = "text"
)

// ParseLevel converts "debug", "info", "warn" or "error" into a slog.Level.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("invalid log level %q (want debug, info, warn or error)", s)
	}
}

// New builds a logger writing to w with the given level and format.
func New(w io.Writer, level string, format Format) (*slog.Logger, error) {
	lvl, err := ParseLevel(level)
	if err != nil {
		return nil, err
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler
	switch format {
	case FormatText:
		h = slog.NewTextHandler(w, opts)
	case FormatJSON, "":
		h = slog.NewJSONHandler(w, opts)
	default:
		return nil, fmt.Errorf("invalid log format %q", format)
	}
	return slog.New(h), nil
}

// Err returns the standard attribute for an error.
func Err(err error) slog.Attr {
	if err == nil {
		return slog.String(KeyErr, "")
	}
	return slog.String(KeyErr, err.Error())
}
