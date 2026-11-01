package detect

import (
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// Detector is one signal in the fusion (Strategy pattern): the scorer sums
// weighted values of every detector through this interface, without knowing
// how each signal is computed.
type Detector interface {
	Kind() domain.SignalKind
	// Advance lets time-driven detectors process everything up to pos.
	Advance(pos domain.StreamPos)
	// Value is the normalized intensity at pos (z-score for voice and chat,
	// clip count for clips). pos should not be after the last Advance.
	Value(pos domain.StreamPos) float64
}

// second returns the index of the whole second containing pos.
func second(pos domain.StreamPos) int64 {
	d := time.Duration(pos)
	s := int64(d / time.Second)
	if d < 0 && d%time.Second != 0 {
		s--
	}
	return s
}
