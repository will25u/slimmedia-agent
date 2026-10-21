package models_test

import (
	"testing"

	"github.com/will25u/slimmedia-agent/internal/models"
)

func TestJobState_Positive(t *testing.T) {
	expectedStates := map[models.JobState]string{
		models.StateDiscovered:  "discovered",
		models.StateQueued:      "queued",
		models.StatePreparing:   "preparing",
		models.StateTranscoding: "transcoding",
		models.StateVerifying:   "verifying",
		models.StateReplacing:   "replacing",
		models.StateCompleted:   "completed",
		models.StateQuarantined: "quarantined",
		models.StateExpired:     "expired",
		models.StateSkipped:     "skipped",
		models.StateFailed:      "failed",
		models.StateRetry:       "retry",
		models.StatePermFailed:  "perm_failed",
		models.StateStopped:     "stopped",
		models.StatePaused:      "paused",
		models.StateDeferred:    "deferred",
		models.StateCancelled:   "cancelled",
	}

	for state, expectedWire := range expectedStates {
		if string(state) != expectedWire {
			t.Errorf("expected state %s to have wire value %q, got %q", state, expectedWire, string(state))
		}
		if !state.IsValid() {
			t.Errorf("expected state %s to be valid", state)
		}
	}
}

func TestJobState_Negative(t *testing.T) {
	invalidStates := []models.JobState{
		"",
		"QUEUED",
		"COMPLETED",
		"TRANSCODING",
		"invalid",
		"in_progress",
		"finished",
		"DEFERRED",
		"unknown_state",
	}

	for _, state := range invalidStates {
		if state.IsValid() {
			t.Errorf("expected invalid state %q to return IsValid() = false", state)
		}
	}
}
