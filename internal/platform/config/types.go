package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Duration is a time.Duration that unmarshals from Go duration strings ("30s").
type Duration time.Duration

// D converts to time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// UnmarshalYAML parses "1h30m", "500ms", ...
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return fmt.Errorf("line %d: duration must be a string like 30s: %w", n.Line, err)
	}
	v, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("line %d: invalid duration %q: %w", n.Line, s, err)
	}
	*d = Duration(v)
	return nil
}

// ByteSize is a size in bytes that unmarshals from "4GiB", "45MiB", "512KiB" or a plain number.
type ByteSize int64

var byteUnits = []struct {
	suffix string
	mult   int64
}{
	{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10},
	{"TB", 1e12}, {"GB", 1e9}, {"MB", 1e6}, {"KB", 1e3}, {"B", 1},
}

// ParseByteSize parses a human-readable size.
func ParseByteSize(s string) (ByteSize, error) {
	s = strings.TrimSpace(s)
	for _, u := range byteUnits {
		if num, ok := strings.CutSuffix(s, u.suffix); ok {
			f, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
			if err != nil || f < 0 || math.IsInf(f, 0) || math.IsNaN(f) {
				return 0, fmt.Errorf("invalid size %q", s)
			}
			return ByteSize(f * float64(u.mult)), nil
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return ByteSize(n), nil
}

// UnmarshalYAML parses sizes such as "4GiB".
func (b *ByteSize) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return fmt.Errorf("line %d: size must be a string like 4GiB: %w", n.Line, err)
	}
	v, err := ParseByteSize(s)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*b = v
	return nil
}
