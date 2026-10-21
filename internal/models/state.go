package models

// JobState represents the state of a transcoding job.
type JobState string

const (
	StateDiscovered  JobState = "discovered"
	StateQueued      JobState = "queued"
	StatePending     JobState = "pending"
	StateLeased      JobState = "leased"
	StateResuming    JobState = "resuming"
	StatePreparing   JobState = "preparing"
	StateTranscoding JobState = "transcoding"
	StateVerifying   JobState = "verifying"
	StateReplacing   JobState = "replacing"
	StateCompleted   JobState = "completed"
	StateQuarantined JobState = "quarantined"
	StateExpired     JobState = "expired"
	StateSkipped     JobState = "skipped"
	StateFailed      JobState = "failed"
	StateRetry       JobState = "retry"
	StatePermFailed  JobState = "perm_failed"
	StateStopped     JobState = "stopped"
	StatePaused      JobState = "paused"
	StateDeferred    JobState = "deferred"
	StateCancelled   JobState = "cancelled"
)

// IsValid reports whether the JobState is a known valid state.
func (s JobState) IsValid() bool {
	switch s {
	case StateDiscovered, StateQueued, StatePending, StateLeased, StateResuming, StatePreparing, StateTranscoding,
		StateVerifying, StateReplacing, StateCompleted, StateQuarantined,
		StateExpired, StateSkipped, StateFailed, StateRetry,
		StatePermFailed, StateStopped, StatePaused, StateDeferred, StateCancelled:
		return true
	default:
		return false
	}
}
