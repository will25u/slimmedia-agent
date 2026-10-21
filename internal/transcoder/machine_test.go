package transcoder

import (
	"testing"
	"time"

	"github.com/will25u/slimmedia-agent/internal/models"
)

func TestStateMachine_AllValidTransitions(t *testing.T) {
	expectedTransitions := map[models.JobState][]models.JobState{
		models.StateDiscovered:  {models.StateQueued, models.StateSkipped},
		models.StateQueued:      {models.StatePreparing, models.StateSkipped, models.StateStopped, models.StatePaused, models.StateResuming},
		models.StateResuming:    {models.StatePreparing, models.StateFailed, models.StateStopped},
		models.StatePreparing:   {models.StateTranscoding, models.StateFailed, models.StateStopped, models.StatePaused},
		models.StateTranscoding: {models.StateVerifying, models.StateFailed, models.StateStopped, models.StatePaused},
		models.StateVerifying:   {models.StateReplacing, models.StateFailed, models.StateStopped},
		models.StateReplacing:   {models.StateCompleted, models.StateQuarantined, models.StateFailed, models.StateStopped},
		models.StateQuarantined: {models.StateExpired, models.StateCompleted},
		models.StateFailed:      {models.StateRetry, models.StatePermFailed},
		models.StateRetry:       {models.StateQueued, models.StatePermFailed},
		models.StatePaused:      {models.StateQueued, models.StatePreparing, models.StateTranscoding, models.StateStopped},
		models.StateSkipped:     {models.StateQueued},
		models.StateStopped:     {models.StateQueued},
		models.StatePermFailed:  {models.StateQueued},
	}

	for startState, targets := range expectedTransitions {
		for _, target := range targets {
			t.Run(string(startState)+"->"+string(target), func(t *testing.T) {
				var callbackCalled bool
				var recordedOld, recordedNew models.JobState

				sm := NewStateMachine(func(job *Job, oldState, newState models.JobState) {
					callbackCalled = true
					recordedOld = oldState
					recordedNew = newState
				})

				job := &Job{
					ID:        "test-job-1",
					FilePath:  "/media/test.mkv",
					State:     startState,
					CreatedAt: time.Now(),
					UpdatedAt: time.Now(),
				}

				err := sm.Transition(job, target)
				if err != nil {
					t.Fatalf("expected valid transition from %s to %s, got error: %v", startState, target, err)
				}

				if job.State != target {
					t.Fatalf("expected job state to be %s, got %s", target, job.State)
				}

				if !callbackCalled {
					t.Fatalf("expected onTransition callback to be invoked")
				}
				if recordedOld != startState || recordedNew != target {
					t.Fatalf("callback got (%s, %s), expected (%s, %s)", recordedOld, recordedNew, startState, target)
				}
			})
		}
	}
}

func TestStateMachine_InvalidTransitions(t *testing.T) {
	allStates := []models.JobState{
		models.StateDiscovered,
		models.StateQueued,
		models.StatePreparing,
		models.StateTranscoding,
		models.StateVerifying,
		models.StateReplacing,
		models.StateCompleted,
		models.StateQuarantined,
		models.StateExpired,
		models.StateSkipped,
		models.StateFailed,
		models.StateRetry,
		models.StatePermFailed,
		models.StateStopped,
		models.StatePaused,
	}

	validMap := map[models.JobState]map[models.JobState]bool{
		models.StateDiscovered:  {models.StateQueued: true, models.StateSkipped: true},
		models.StateQueued:      {models.StatePreparing: true, models.StateSkipped: true, models.StateStopped: true, models.StatePaused: true, models.StateResuming: true},
		models.StateResuming:    {models.StatePreparing: true, models.StateFailed: true, models.StateStopped: true},
		models.StatePreparing:   {models.StateTranscoding: true, models.StateFailed: true, models.StateStopped: true, models.StatePaused: true},
		models.StateTranscoding: {models.StateVerifying: true, models.StateFailed: true, models.StateStopped: true, models.StatePaused: true},
		models.StateVerifying:   {models.StateReplacing: true, models.StateFailed: true, models.StateStopped: true},
		models.StateReplacing:   {models.StateCompleted: true, models.StateQuarantined: true, models.StateFailed: true, models.StateStopped: true},
		models.StateCompleted:   {},
		models.StateQuarantined: {models.StateExpired: true, models.StateCompleted: true},
		models.StateFailed:      {models.StateRetry: true, models.StatePermFailed: true},
		models.StateRetry:       {models.StateQueued: true, models.StatePermFailed: true},
		models.StatePaused:      {models.StateQueued: true, models.StatePreparing: true, models.StateTranscoding: true, models.StateStopped: true},
		models.StateSkipped:     {models.StateQueued: true},
		models.StateStopped:     {models.StateQueued: true},
		models.StatePermFailed:  {models.StateQueued: true},
		models.StateExpired:     {},
	}

	sm := NewStateMachine(nil)

	for _, startState := range allStates {
		validTargets := validMap[startState]
		for _, targetState := range allStates {
			if validTargets != nil && validTargets[targetState] {
				continue // Skip valid transitions
			}

			t.Run("Invalid_"+string(startState)+"->"+string(targetState), func(t *testing.T) {
				job := &Job{
					ID:        "invalid-test-job",
					FilePath:  "/media/test.mkv",
					State:     startState,
					CreatedAt: time.Now(),
					UpdatedAt: time.Now(),
				}

				err := sm.Transition(job, targetState)
				if err == nil {
					t.Fatalf("expected error for invalid transition from %s to %s, but got nil", startState, targetState)
				}

				if job.State != startState {
					t.Fatalf("job state mutated during invalid transition from %s to %s; got %s", startState, targetState, job.State)
				}
			})
		}
	}
}

func TestStateMachine_UnknownOrUndefinedState(t *testing.T) {
	sm := NewStateMachine(nil)
	job := &Job{
		ID:       "unknown-job",
		FilePath: "/media/test.mkv",
		State:    models.JobState("NON_EXISTENT_STATE"),
	}

	err := sm.Transition(job, models.StateQueued)
	if err == nil {
		t.Fatalf("expected error transitioning from unknown state, got nil")
	}
}
