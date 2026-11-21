package detect

import "github.com/henriquecpp/gta6-clipper/internal/domain"

// CalibrationConfig turns the approver's decisions into a per-streamer
// strictness offset, added to threshold_on, min_signal_z and the clip rain z:
// every discard raises that streamer's bar, every approval lowers it.
type CalibrationConfig struct {
	// Window is how many recent decisions of a streamer count.
	Window int
	// StepUp is added for each discard and StepDown removed for each
	// approval. The offset settles where approvals/discards = StepUp/StepDown
	// (equal steps: about half of the previews approved).
	StepUp   float64
	StepDown float64
	// Max caps the offset; 0 disables the calibration.
	Max float64
}

// Offset replays the decisions (oldest first; only the last Window count)
// and returns the strictness offset in [0, Max]. The clamp is applied at each
// step, so old approvals do not build up a credit that hides new discards.
// Expired previews are neutral: not looking at a preview is not a verdict.
func (c CalibrationConfig) Offset(decisions []domain.Decision) float64 {
	if c.Max <= 0 {
		return 0
	}
	if c.Window > 0 && len(decisions) > c.Window {
		decisions = decisions[len(decisions)-c.Window:]
	}
	off := 0.0
	for _, d := range decisions {
		switch d {
		case domain.DecisionDiscarded:
			off += c.StepUp
		case domain.DecisionApproved:
			off -= c.StepDown
		}
		off = clamp(off, 0, c.Max)
	}
	return off
}
