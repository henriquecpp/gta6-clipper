package logging

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
)

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{"": slog.LevelInfo, "info": slog.LevelInfo, "DEBUG": slog.LevelDebug, "warn": slog.LevelWarn, "warning": slog.LevelWarn, "error": slog.LevelError}
	for in, want := range cases {
		got, err := ParseLevel(in)
		if err != nil || got != want {
			t.Errorf("ParseLevel(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseLevel("verbose"); err == nil {
		t.Error("expected error for invalid level")
	}
}

func TestNewJSON(t *testing.T) {
	var buf bytes.Buffer
	log, err := New(&buf, "info", FormatJSON)
	if err != nil {
		t.Fatal(err)
	}
	log.Debug("hidden")
	log.Info("hello", slog.String(KeyClipID, "c1"), Err(errors.New("boom")))
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("not JSON: %v: %s", err, buf.String())
	}
	if m["msg"] != "hello" || m[KeyClipID] != "c1" || m[KeyErr] != "boom" {
		t.Fatalf("unexpected record: %v", m)
	}
	if _, err := New(&buf, "info", "xml"); err == nil {
		t.Error("expected error for invalid format")
	}
	if Err(nil).Value.String() != "" {
		t.Error("Err(nil) should be empty")
	}
}
