package main

import (
	"context"
	"testing"

	"github.com/henriquecpp/gta6-clipper/internal/adapters/out/resilience"
	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// Disabled ports must fail at once: retrying a missing credential only delays
// the failure and pollutes the logs.
func TestDisabledPortsAreNotRetried(t *testing.T) {
	if resilience.Retryable(errTelegramDisabled) {
		t.Error("errTelegramDisabled is retryable")
	}
	_, _, err := disabledEditor{}.Plan(context.Background(), domain.EditRequest{})
	if err == nil || resilience.Retryable(err) {
		t.Errorf("disabled editor error = %v, want a non-retryable error", err)
	}
}
