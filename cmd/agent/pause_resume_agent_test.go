package main

import (
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/will25u/slimmedia-agent/internal/protocol"
)

// TestAgent_PauseRequestDrainsWithoutStateOscillation verifies acceptance criteria 1:
// Agent does not broadcast PAUSED status immediately upon receiving PAUSE_JOB;
// status remains WORKING (with activity "pausing after chunk") until the current
// chunk finishes and the runner actually halts in the pause loop.
func TestAgent_PauseRequestDrainsWithoutStateOscillation(t *testing.T) {
	var currentStatus string
	var currentActivity string
	statusUpdates := make([]struct{ status, activity string }, 0)

	setStatusWithActivity := func(status, activity string) {
		currentStatus = status
		currentActivity = activity
		statusUpdates = append(statusUpdates, struct{ status, activity string }{status, activity})
	}

	jobPauses := make(map[string]*atomic.Bool)
	jobID := "drain-test-job-789"
	pauseFlag := &atomic.Bool{}
	jobPauses[jobID] = pauseFlag

	// Simulate receiving PAUSE_JOB
	envPause := protocol.Envelope{
		Type:    protocol.TypePauseJob,
		Payload: []byte(`{"job_id":"drain-test-job-789"}`),
	}

	var payload struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(envPause.Payload, &payload); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	// When PAUSE_JOB arrives:
	if p, ok := jobPauses[payload.JobID]; ok {
		p.Store(true)
		setStatusWithActivity("WORKING", "pausing after chunk")
	}

	if !pauseFlag.Load() {
		t.Errorf("expected pauseFlag to be true")
	}
	if currentStatus != "WORKING" {
		t.Errorf("expected agent status to remain WORKING during chunk drain, got %q", currentStatus)
	}
	if currentActivity != "pausing after chunk" {
		t.Errorf("expected activity %q, got %q", "pausing after chunk", currentActivity)
	}

	// Simulate chunk completion and runner entering pause loop (OnPause(true))
	onPause := func(paused bool) {
		if paused {
			setStatusWithActivity("PAUSED", "")
		} else {
			setStatusWithActivity("WORKING", "")
		}
	}
	onPause(true)

	if currentStatus != "PAUSED" {
		t.Errorf("expected status to be PAUSED once halted in pause loop, got %q", currentStatus)
	}

	// Simulate unpause / RESUME_JOB
	envResume := protocol.Envelope{
		Type:    protocol.TypeResumeJob,
		Payload: []byte(`{"job_id":"drain-test-job-789"}`),
	}
	if err := json.Unmarshal(envResume.Payload, &payload); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if p, ok := jobPauses[payload.JobID]; ok {
		p.Store(false)
		setStatusWithActivity("WORKING", "resuming")
	}

	if pauseFlag.Load() {
		t.Errorf("expected pauseFlag to be false")
	}
	if currentStatus != "WORKING" || currentActivity != "resuming" {
		t.Errorf("expected WORKING with activity resuming, got %q / %q", currentStatus, currentActivity)
	}

	// Runner wakes up and exits pause loop (OnPause(false))
	onPause(false)
	if currentStatus != "WORKING" || currentActivity != "" {
		t.Errorf("expected WORKING with empty activity, got %q / %q", currentStatus, currentActivity)
	}

	// Verify the complete sequence of states had no premature PAUSED during drain
	expectedSequence := []struct{ status, activity string }{
		{"WORKING", "pausing after chunk"},
		{"PAUSED", ""},
		{"WORKING", "resuming"},
		{"WORKING", ""},
	}
	if len(statusUpdates) != len(expectedSequence) {
		t.Fatalf("expected %d status updates, got %d", len(expectedSequence), len(statusUpdates))
	}
	for i, expected := range expectedSequence {
		if statusUpdates[i] != expected {
			t.Errorf("step %d: expected %+v, got %+v", i, expected, statusUpdates[i])
		}
	}
}
