package transcoder

import (
	"fmt"
	"sync"
	"time"

	"github.com/will25u/slimmedia-agent/internal/models"
)

type Job struct {
	ID           string
	FilePath     string
	State        models.JobState
	RetryCount   int
	CreatedAt    time.Time
	UpdatedAt    time.Time
	ErrorMessage string
	Payload      []byte
}

type StateMachine struct {
	mu               sync.Mutex
	validTransitions map[models.JobState][]models.JobState
	onTransition     func(job *Job, oldState, newState models.JobState)
}

func NewStateMachine(onTransition func(job *Job, oldState, newState models.JobState)) *StateMachine {
	sm := &StateMachine{
		validTransitions: make(map[models.JobState][]models.JobState),
		onTransition:     onTransition,
	}

	// Define explicit transition table as per ADR-004
	sm.validTransitions[models.StateDiscovered] = []models.JobState{models.StateQueued, models.StateSkipped}
	sm.validTransitions[models.StateQueued] = []models.JobState{models.StatePreparing, models.StateSkipped, models.StateStopped, models.StatePaused, models.StateResuming}
	sm.validTransitions[models.StateResuming] = []models.JobState{models.StatePreparing, models.StateFailed, models.StateStopped}
	sm.validTransitions[models.StatePreparing] = []models.JobState{models.StateTranscoding, models.StateFailed, models.StateStopped, models.StatePaused}
	sm.validTransitions[models.StateTranscoding] = []models.JobState{models.StateVerifying, models.StateFailed, models.StateStopped, models.StatePaused}
	sm.validTransitions[models.StateVerifying] = []models.JobState{models.StateReplacing, models.StateFailed, models.StateStopped}
	sm.validTransitions[models.StateReplacing] = []models.JobState{models.StateCompleted, models.StateQuarantined, models.StateFailed, models.StateStopped}
	sm.validTransitions[models.StateCompleted] = []models.JobState{}                                             // Terminal or goes to quarantined depending on policy, but REPLACING handles that
	sm.validTransitions[models.StateQuarantined] = []models.JobState{models.StateExpired, models.StateCompleted} // Can be restored or expired
	sm.validTransitions[models.StateFailed] = []models.JobState{models.StateRetry, models.StatePermFailed}
	sm.validTransitions[models.StateRetry] = []models.JobState{models.StateQueued, models.StatePermFailed}
	sm.validTransitions[models.StatePaused] = []models.JobState{models.StateQueued, models.StatePreparing, models.StateTranscoding, models.StateStopped} // Resume goes back

	// Terminal states have no outward transitions naturally, but users could force actions
	sm.validTransitions[models.StateSkipped] = []models.JobState{models.StateQueued}    // Force re-queue
	sm.validTransitions[models.StateStopped] = []models.JobState{models.StateQueued}    // Restart
	sm.validTransitions[models.StatePermFailed] = []models.JobState{models.StateQueued} // Restart

	return sm
}

func (sm *StateMachine) Transition(job *Job, newState models.JobState) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	validTargets, exists := sm.validTransitions[job.State]
	if !exists {
		return fmt.Errorf("invalid state transition: no outbound transitions from %s", job.State)
	}

	isValid := false
	for _, target := range validTargets {
		if target == newState {
			isValid = true
			break
		}
	}

	if !isValid {
		return fmt.Errorf("invalid state transition: cannot move from %s to %s", job.State, newState)
	}

	oldState := job.State
	job.State = newState
	job.UpdatedAt = time.Now()

	if sm.onTransition != nil {
		sm.onTransition(job, oldState, newState)
	}

	return nil
}
