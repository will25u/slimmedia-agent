package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/will25u/slimmedia-agent/internal/db"
	"github.com/will25u/slimmedia-agent/internal/ffmpeg"
	"github.com/will25u/slimmedia-agent/internal/jobspec"
	"github.com/will25u/slimmedia-agent/internal/transcoder"
	"github.com/will25u/slimmedia-agent/internal/protocol"
)

// gateRunner is a fake Runner whose ProcessJobWithSpec reports any queued
// progress, then blocks until the test releases the job (or the job context
// is cancelled). It lets a test hold jobs in flight and drive
// pause/resume/cancel semantics through the supervisor's interface — no
// WebSocket server, no real SQLite, no ffmpeg stubs.
type gateRunner struct {
	mu        sync.Mutex
	hooks     map[string]transcoder.JobHooks
	gates     map[string]chan struct{}
	progress  map[string][]ffmpeg.Progress
	failErrs  map[string]error
	commands  map[string][]protocol.FfmpegCommandRecord
	discarded []string

	started chan string
}

func newGateRunner() *gateRunner {
	return &gateRunner{
		hooks:    map[string]transcoder.JobHooks{},
		gates:    map[string]chan struct{}{},
		progress: map[string][]ffmpeg.Progress{},
		failErrs: map[string]error{},
		commands: map[string][]protocol.FfmpegCommandRecord{},
		started:  make(chan string, 16),
	}
}

// queueFailure arms a terminal error the runner reports once the test
// releases the job, instead of a successful result.
func (r *gateRunner) queueFailure(jobID string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failErrs[jobID] = err
}

// queueCommands arms ffmpeg command traces returned in JobResult upon completion.
func (r *gateRunner) queueCommands(jobID string, cmds []protocol.FfmpegCommandRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commands[jobID] = cmds
}

// queueProgress arms progress reports the runner emits (via the supervisor's
// progress callback) before it blocks on the gate.
func (r *gateRunner) queueProgress(jobID string, p []ffmpeg.Progress) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.progress[jobID] = p
}

func (r *gateRunner) ProcessJobWithSpec(ctx context.Context, job *transcoder.Job, targetCodec string, spec jobspec.JobSpec, hooks transcoder.JobHooks, onProgress transcoder.ProgressCallback) (*transcoder.JobResult, error) {
	r.mu.Lock()
	r.hooks[job.ID] = hooks
	gate, ok := r.gates[job.ID]
	if !ok {
		gate = make(chan struct{})
		r.gates[job.ID] = gate
	}
	progress := r.progress[job.ID]
	r.mu.Unlock()

	r.started <- job.ID
	if onProgress != nil {
		for _, p := range progress {
			onProgress(p)
		}
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-gate:
		r.mu.Lock()
		failErr := r.failErrs[job.ID]
		cmds := r.commands[job.ID]
		r.mu.Unlock()
		if failErr != nil {
			return nil, failErr
		}
		return &transcoder.JobResult{
			OriginalSize: 1000,
			NewSize:      500,
			SavingsBytes: 500,
			DurationSecs: 42,
			Commands:     cmds,
		}, nil
	}
}

func (r *gateRunner) DiscardJobFiles(_ context.Context, filePath, jobID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.discarded = append(r.discarded, jobID+"="+filePath)
}

// hooksFor returns the hooks the runner saw for a job, once it started.
func (r *gateRunner) hooksFor(t *testing.T, jobID string) transcoder.JobHooks {
	t.Helper()
	waitFor(t, func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		_, ok := r.hooks[jobID]
		return ok
	})
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hooks[jobID]
}

// release lets one in-flight job finish successfully.
func (r *gateRunner) release(jobID string) {
	r.mu.Lock()
	gate := r.gates[jobID]
	r.mu.Unlock()
	close(gate)
}

// supervisorFixture wires a JobSupervisor to recording callbacks.
type supervisorFixture struct {
	supervisor *JobSupervisor
	runner     *gateRunner

	mu         sync.Mutex
	events     []Event
	envelopes  []protocol.Envelope
	statuses   []string
	activities []string
}

func newSupervisorFixture(t *testing.T) *supervisorFixture {
	t.Helper()
	f := &supervisorFixture{runner: newGateRunner()}
	f.supervisor = NewJobSupervisor(SupervisorDeps{
		Runner:      f.runner,
		Confinement: passThroughConfinement{},
		Emit:        f.recordEvent,
		Send:        f.recordEnvelope,
		SetStatus: func(status, activity string) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.statuses = append(f.statuses, status)
			f.activities = append(f.activities, activity)
			f.events = append(f.events, Event{Type: EventStatus, Status: status, Activity: activity})
		},
	})
	return f
}

func (f *supervisorFixture) recordEvent(ev Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
}

func (f *supervisorFixture) recordEnvelope(env protocol.Envelope) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.envelopes = append(f.envelopes, env)
}

func (f *supervisorFixture) sawEvent(pred func(Event) bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ev := range f.events {
		if pred(ev) {
			return true
		}
	}
	return false
}

func (f *supervisorFixture) envelopesOfType(envType string) []protocol.Envelope {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []protocol.Envelope
	for _, env := range f.envelopes {
		if env.Type == envType {
			out = append(out, env)
		}
	}
	return out
}

func (f *supervisorFixture) lastStatus() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.statuses) == 0 {
		return ""
	}
	return f.statuses[len(f.statuses)-1]
}

// waitFor polls cond until it holds or the test deadline passes.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func testStartPayload(jobID string) protocol.StartJobPayload {
	return protocol.StartJobPayload{
		JobID:       jobID,
		MediaItemID: "media-" + jobID,
		InputPath:   "/media/movie-" + jobID + ".mkv",
		TargetCodec: "hevc",
		Container:   "mkv",
		CRF:         22,
		SpecVersion: protocol.CurrentJobSpecVersion,
	}
}

// TestJobSupervisorPauseResume drives PAUSE/RESUME through the supervisor's
// interface with a fake runner acting as the segmented runner would at a
// chunk boundary: it observes IsPaused, reports PAUSED via OnPause, holds,
// then reports WORKING on resume.
func TestJobSupervisorPauseResume(t *testing.T) {
	f := newSupervisorFixture(t)
	payload := testStartPayload("pause-job")

	if err := f.supervisor.StartJob(context.Background(), payload, nil); err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	hooks := f.runner.hooksFor(t, payload.JobID)

	// At the chunk boundary the runner sees the pause flag flip and reports
	// PAUSED; the supervisor mirrors that onto the status stream.
	f.supervisor.PauseJob(payload.JobID)
	waitFor(t, hooks.IsPaused)
	hooks.OnPause(true) // the runner's report when it halts at the boundary
	waitFor(t, func() bool { return f.lastStatus() == "PAUSED" })

	// While paused the runner holds: releasing the gate is the only way out,
	// and the supervisor must not have reported completion.
	f.supervisor.ResumeJob(payload.JobID)
	waitFor(t, func() bool { return !hooks.IsPaused() })
	hooks.OnPause(false) // the runner's report when it proceeds again
	waitFor(t, func() bool { return f.lastStatus() == "WORKING" })

	f.runner.release(payload.JobID)
	waitFor(t, func() bool {
		return f.sawEvent(func(ev Event) bool {
			return ev.Type == EventJobComplete && ev.JobID == payload.JobID && ev.Success
		})
	})
	if n := f.supervisor.ActiveCount(); n != 0 {
		t.Fatalf("active count after completion = %d, want 0", n)
	}
}

// TestJobSupervisorCancelActiveJob cancels an in-flight job: the runner sees
// its context cancelled, the outcome is reported as a cancellation (not a
// failure with the runner's error text), and the drain WaitGroup releases.
func TestJobSupervisorCancelActiveJob(t *testing.T) {
	f := newSupervisorFixture(t)
	payload := testStartPayload("cancel-job")

	if err := f.supervisor.StartJob(context.Background(), payload, nil); err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	<-f.runner.started

	f.supervisor.CancelJob(context.Background(), payload.JobID)

	waitFor(t, func() bool {
		return f.sawEvent(func(ev Event) bool {
			return ev.Type == EventJobComplete && ev.JobID == payload.JobID && !ev.Success && ev.Cancelled
		})
	})
	f.supervisor.Wait()
	if n := f.supervisor.ActiveCount(); n != 0 {
		t.Fatalf("active count after cancel = %d, want 0", n)
	}

	// The cancellation must surface as a short, UI-safe message — never the
	// runner's raw error, which can embed process stderr (a cancelled demux
	// used to reach the tray as a multi-KB "source demux failed" dump).
	var complete Event
	found := f.sawEvent(func(ev Event) bool {
		if ev.Type == EventJobComplete && ev.JobID == payload.JobID {
			complete = ev
			return true
		}
		return false
	})
	if !found {
		t.Fatal("EventJobComplete not found")
	}
	if complete.Message != "job cancelled" {
		t.Errorf("Message = %q, want %q", complete.Message, "job cancelled")
	}

	// A cancel for an unknown job ID must not panic or fabricate outcomes.
	f.supervisor.CancelJob(context.Background(), "never-started")
}

// fakeJournal stubs the JobJournal lookup for cancel-time cleanup tests.
type fakeJournal struct {
	rows map[string]db.JobJournal
}

func (j fakeJournal) GetJobJournal(_ context.Context, jobID string) (db.JobJournal, error) {
	if row, ok := j.rows[jobID]; ok {
		return row, nil
	}
	return db.JobJournal{}, sql.ErrNoRows
}

// TestJobSupervisorCancelLateJobDiscardsFiles covers the CANCEL_JOB that
// arrives after the job already terminated locally: the supervisor consults
// the journal and asks the runner to discard the scratch files.
func TestJobSupervisorCancelLateJobDiscardsFiles(t *testing.T) {
	f := newSupervisorFixture(t)
	f.supervisor.journal = fakeJournal{rows: map[string]db.JobJournal{
		"late-job": {JobID: "late-job", FilePath: "/media/late.mkv"},
	}}

	f.supervisor.CancelJob(context.Background(), "late-job")

	waitFor(t, func() bool {
		f.runner.mu.Lock()
		defer f.runner.mu.Unlock()
		return len(f.runner.discarded) == 1
	})
	f.runner.mu.Lock()
	defer f.runner.mu.Unlock()
	if got := f.runner.discarded[0]; got != "late-job=/media/late.mkv" {
		t.Fatalf("discarded = %q, want late-job=/media/late.mkv", got)
	}
}

// TestJobSupervisorQuietHoursRejectJob verifies the quiet-hour gate rejects
// before any runner involvement and reports the terminal outcome.
func TestJobSupervisorQuietHoursRejectJob(t *testing.T) {
	f := newSupervisorFixture(t)
	payload := testStartPayload("quiet-job")
	// All-ones grid: every hour of the week is a quiet hour.
	payload.Schedule = &protocol.ScheduleConfig{
		QuietHoursEnabled: true,
		ScheduleGrid:      protocol.WeeklySchedule(strings.Repeat("1", 168)),
	}

	if err := f.supervisor.StartJob(context.Background(), payload, nil); err == nil {
		t.Fatal("StartJob succeeded during quiet hours; want rejection")
	}

	completes := f.envelopesOfType(protocol.TypeFileComplete)
	if len(completes) != 1 {
		t.Fatalf("FILE_COMPLETE envelopes = %d, want 1", len(completes))
	}
	var complete protocol.FileCompletePayload
	if err := json.Unmarshal(completes[0].Payload, &complete); err != nil {
		t.Fatalf("unmarshal FILE_COMPLETE: %v", err)
	}
	if complete.Success || !strings.Contains(complete.ErrorMessage, "quiet hours") {
		t.Fatalf("complete payload = %+v, want failed with quiet-hours reason", complete)
	}
	select {
	case id := <-f.runner.started:
		t.Fatalf("runner was invoked for rejected job %s", id)
	default:
	}
}

// fakeSafety stubs the stream-safety gate.
type fakeSafety struct {
	conflict   bool
	serverName string
	err        error
	checked    chan string
}

func (s *fakeSafety) CheckConflict(_ context.Context, inputPath string) (bool, string, error) {
	if s.checked != nil {
		s.checked <- inputPath
	}
	return s.conflict, s.serverName, s.err
}

// blockingSafety stubs the stream-safety gate as a check that blocks until
// its context is cancelled, parking StartJob inside the gate so a test can
// deliver a CANCEL_JOB mid-gate.
type blockingSafety struct {
	entered chan struct{}
	once    sync.Once
}

func (b *blockingSafety) CheckConflict(ctx context.Context, _ string) (bool, string, error) {
	b.once.Do(func() { close(b.entered) })
	<-ctx.Done()
	return false, "", ctx.Err()
}

// TestJobSupervisorCancelDuringGatesCancelsJob verifies that a CANCEL_JOB
// arriving while the pre-spawn gates still run actually cancels the start:
// the job is registered before the gates, the blocked gate unblocks via the
// cancelled context, the runner is never launched, the terminal outcome is
// reported, and the cancellability registration is cleaned up (issue #150).
func TestJobSupervisorCancelDuringGatesCancelsJob(t *testing.T) {
	f := newSupervisorFixture(t)
	safety := &blockingSafety{entered: make(chan struct{})}
	f.supervisor.safety = safety
	payload := testStartPayload("gate-cancel")

	startErr := make(chan error, 1)
	go func() {
		startErr <- f.supervisor.StartJob(context.Background(), payload, nil)
	}()
	<-safety.entered

	f.supervisor.CancelJob(context.Background(), payload.JobID)

	if err := <-startErr; err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("StartJob err = %v, want cancelled-before-start error", err)
	}

	select {
	case id := <-f.runner.started:
		t.Fatalf("runner was launched for gate-cancelled job %s", id)
	default:
	}

	completes := f.envelopesOfType(protocol.TypeFileComplete)
	if len(completes) != 1 {
		t.Fatalf("FILE_COMPLETE envelopes = %d, want 1", len(completes))
	}
	var complete protocol.FileCompletePayload
	if err := json.Unmarshal(completes[0].Payload, &complete); err != nil {
		t.Fatalf("unmarshal FILE_COMPLETE: %v", err)
	}
	if complete.Success || !strings.Contains(complete.ErrorMessage, "cancelled") {
		t.Fatalf("complete payload = %+v, want failed with cancelled reason", complete)
	}
	if n := f.supervisor.ActiveCount(); n != 0 {
		t.Fatalf("active count after gate cancel = %d, want 0", n)
	}

	f.supervisor.mu.Lock()
	_, stillRegistered := f.supervisor.cancels[payload.JobID]
	f.supervisor.mu.Unlock()
	if stillRegistered {
		t.Fatal("job still registered as cancellable after gate-time cancel")
	}
}

// TestJobSupervisorStreamSafetyConflictRejects verifies an active streaming
// conflict defers the job before the runner is involved.
func TestJobSupervisorStreamSafetyConflictRejects(t *testing.T) {
	checker := &fakeSafety{conflict: true, serverName: "Plex Media Server", checked: make(chan string, 1)}
	f := newSupervisorFixture(t)
	f.supervisor.safety = checker
	payload := testStartPayload("conflict-job")

	if err := f.supervisor.StartJob(context.Background(), payload, nil); err == nil {
		t.Fatal("StartJob succeeded during a streaming conflict; want rejection")
	}
	if path := <-checker.checked; path != payload.InputPath {
		t.Fatalf("safety checked %q, want %q", path, payload.InputPath)
	}

	completes := f.envelopesOfType(protocol.TypeFileComplete)
	if len(completes) != 1 {
		t.Fatalf("FILE_COMPLETE envelopes = %d, want 1", len(completes))
	}
	var complete protocol.FileCompletePayload
	if err := json.Unmarshal(completes[0].Payload, &complete); err != nil {
		t.Fatalf("unmarshal FILE_COMPLETE: %v", err)
	}
	if complete.Success || !strings.Contains(complete.ErrorMessage, "Plex Media Server") {
		t.Fatalf("complete payload = %+v, want failed with conflict reason", complete)
	}
	select {
	case id := <-f.runner.started:
		t.Fatalf("runner was invoked for rejected job %s", id)
	default:
	}
}

// TestJobSupervisorStreamSafetyUsesCanonicalPath verifies that media server
// stream conflict checks use the resolved canonical path rather than the wire path.
func TestJobSupervisorStreamSafetyUsesCanonicalPath(t *testing.T) {
	checker := &fakeSafety{conflict: false, checked: make(chan string, 1)}
	confinement := &fakeConfinement{canonical: "/media/real/canonical-movie.mkv"}
	f := newSupervisorFixture(t)
	f.supervisor.safety = checker
	f.supervisor.confinement = confinement

	payload := testStartPayload("canonical-stream-job")
	payload.InputPath = "/media/symlinks/symlink-movie.mkv"

	if err := f.supervisor.StartJob(context.Background(), payload, nil); err != nil {
		t.Fatalf("StartJob failed: %v", err)
	}

	select {
	case checkedPath := <-checker.checked:
		if checkedPath != "/media/real/canonical-movie.mkv" {
			t.Fatalf("stream safety checked %q, want canonical path %q", checkedPath, "/media/real/canonical-movie.mkv")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("stream safety check was not invoked")
	}

	<-f.runner.started
	f.runner.release(payload.JobID)
}

// fakeConfinement stubs the path-confinement gate.
type fakeConfinement struct {
	canonical string
	err       error
	checked   chan string
}

func (c *fakeConfinement) ValidateJobInputPath(inputPath string) (string, error) {
	if c.checked != nil {
		c.checked <- inputPath
	}
	return c.canonical, c.err
}

// passThroughConfinement echoes the supplied path, standing in for the
// scanned-library confinement when a test does not care about membership.
type passThroughConfinement struct{}

func (passThroughConfinement) ValidateJobInputPath(inputPath string) (string, error) {
	return inputPath, nil
}

// TestJobSupervisorConfinementRejectJob verifies a path-confinement failure
// rejects the job before the runner is involved and reports the terminal
// outcome.
func TestJobSupervisorConfinementRejectJob(t *testing.T) {
	checker := &fakeConfinement{err: errors.New("path outside the scanned media library"), checked: make(chan string, 1)}
	f := newSupervisorFixture(t)
	f.supervisor.confinement = checker
	payload := testStartPayload("confined-job")

	if err := f.supervisor.StartJob(context.Background(), payload, nil); err == nil {
		t.Fatal("StartJob succeeded for an unconfined path; want rejection")
	}
	if path := <-checker.checked; path != payload.InputPath {
		t.Fatalf("confinement checked %q, want %q", path, payload.InputPath)
	}

	completes := f.envelopesOfType(protocol.TypeFileComplete)
	if len(completes) != 1 {
		t.Fatalf("FILE_COMPLETE envelopes = %d, want 1", len(completes))
	}
	var complete protocol.FileCompletePayload
	if err := json.Unmarshal(completes[0].Payload, &complete); err != nil {
		t.Fatalf("unmarshal FILE_COMPLETE: %v", err)
	}
	if complete.Success || !strings.Contains(complete.ErrorMessage, "outside the scanned media library") {
		t.Fatalf("complete payload = %+v, want failed with confinement reason", complete)
	}
	select {
	case id := <-f.runner.started:
		t.Fatalf("runner was invoked for rejected job %s", id)
	default:
	}
}

func TestJobSupervisorRejectsInvalidCustomFlags(t *testing.T) {
	invalidFlags := []string{
		"-preset ultrafast /tmp/extra.mkv",
		"/tmp/extra.mkv -preset ultrafast",
		"-f null /dev/null",
		"-dump_attachment:t:0 /etc/shadow",
		"-bf 4 -",
		"keyint=60 /tmp/evil.mkv",
		"-vf subtitles=/etc/shadow",
		"-af loudnorm",
		"-filter:v scale=1280:720",
		"-filter:a loudnorm",
		"-filter scale=1280:720",
		"-filter_script /tmp/graph",
		"-filter_script:v /tmp/graph",
		"-filter_script:a /tmp/graph",
		"-filter_complex_script /tmp/graph",
		"-report",
	}

	for _, flags := range invalidFlags {
		t.Run(flags, func(t *testing.T) {
			f := newSupervisorFixture(t)
			f.supervisor.confinement = &fakeConfinement{checked: make(chan string, 1)}
			payload := testStartPayload("flags-job")
			payload.CustomFlags = flags

			if err := f.supervisor.StartJob(context.Background(), payload, nil); err == nil {
				t.Fatalf("StartJob succeeded for invalid custom flags %q; want rejection", flags)
			}

			completes := f.envelopesOfType(protocol.TypeFileComplete)
			if len(completes) != 1 {
				t.Fatalf("FILE_COMPLETE envelopes = %d, want 1", len(completes))
			}
			var complete protocol.FileCompletePayload
			if err := json.Unmarshal(completes[0].Payload, &complete); err != nil {
				t.Fatalf("unmarshal FILE_COMPLETE: %v", err)
			}
			if complete.Success || !strings.Contains(complete.ErrorMessage, "invalid custom flags") {
				t.Fatalf("complete payload = %+v, want failed with invalid custom flags", complete)
			}
			select {
			case id := <-f.runner.started:
				t.Fatalf("runner was invoked for rejected job %s", id)
			default:
			}
		})
	}
}

// TestJobSupervisorCapsFileCompleteError verifies a runaway runner error
// (e.g. ffmpeg stderr quoting file content) is truncated before it is sent
// in the FILE_COMPLETE envelope, closing the error-channel exfiltration path.
func TestJobSupervisorCapsFileCompleteError(t *testing.T) {
	f := newSupervisorFixture(t)
	payload := testStartPayload("big-error-job")
	huge := "ffmpeg failed: " + strings.Repeat("x", protocol.MaxFileCompleteErrorLen+5000)
	f.runner.queueFailure(payload.JobID, errors.New(huge))

	if err := f.supervisor.StartJob(context.Background(), payload, nil); err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	<-f.runner.started
	f.runner.release(payload.JobID)

	waitFor(t, func() bool {
		return f.sawEvent(func(ev Event) bool {
			return ev.Type == EventJobComplete && ev.JobID == payload.JobID && !ev.Success
		})
	})

	completes := f.envelopesOfType(protocol.TypeFileComplete)
	if len(completes) != 1 {
		t.Fatalf("FILE_COMPLETE envelopes = %d, want 1", len(completes))
	}
	var complete protocol.FileCompletePayload
	if err := json.Unmarshal(completes[0].Payload, &complete); err != nil {
		t.Fatalf("unmarshal FILE_COMPLETE: %v", err)
	}
	if complete.Success {
		t.Fatalf("complete payload = %+v, want failure", complete)
	}
	if len(complete.ErrorMessage) > protocol.MaxFileCompleteErrorLen+len("… (truncated)") {
		t.Fatalf("error message not capped: len=%d", len(complete.ErrorMessage))
	}
	if !strings.Contains(complete.ErrorMessage, "(truncated)") {
		t.Fatalf("capped message missing truncation marker: %q", complete.ErrorMessage[len(complete.ErrorMessage)-40:])
	}
	if !strings.HasPrefix(complete.ErrorMessage, "ffmpeg failed: ") {
		t.Fatalf("capped message lost its head: %q...", complete.ErrorMessage[:50])
	}
}

// TestJobSupervisorConfinementCanonicalizes verifies a passing confinement
// check hands its canonical path to the runner.
func TestJobSupervisorConfinementCanonicalizes(t *testing.T) {
	f := newSupervisorFixture(t)
	f.supervisor.confinement = &fakeConfinement{canonical: "/library/movie.mkv"}
	payload := testStartPayload("canonical-job")

	if err := f.supervisor.StartJob(context.Background(), payload, nil); err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	<-f.runner.started
	f.runner.release(payload.JobID)
	waitFor(t, func() bool {
		return f.sawEvent(func(ev Event) bool {
			return ev.Type == EventJobComplete && ev.JobID == payload.JobID && ev.Success
		})
	})
}

// TestJobSupervisorDefersWhileUpdating verifies job starts defer to an
// in-flight agent update, reporting the terminal outcome without involving
// the runner.
func TestJobSupervisorDefersWhileUpdating(t *testing.T) {
	f := newSupervisorFixture(t)
	f.supervisor.updating = func() bool { return true }
	payload := testStartPayload("updating-job")

	if err := f.supervisor.StartJob(context.Background(), payload, nil); err == nil {
		t.Fatal("StartJob succeeded during an agent update; want deferral")
	}

	completes := f.envelopesOfType(protocol.TypeFileComplete)
	if len(completes) != 1 {
		t.Fatalf("FILE_COMPLETE envelopes = %d, want 1", len(completes))
	}
	var complete protocol.FileCompletePayload
	if err := json.Unmarshal(completes[0].Payload, &complete); err != nil {
		t.Fatalf("unmarshal FILE_COMPLETE: %v", err)
	}
	if complete.Success || !strings.Contains(complete.ErrorMessage, "agent update is pending") {
		t.Fatalf("complete payload = %+v, want failed with update-deferral reason", complete)
	}
	select {
	case id := <-f.runner.started:
		t.Fatalf("runner was invoked for deferred job %s", id)
	default:
	}
}

// TestJobSupervisorProgressBridge verifies transcoder progress reaches both
// the outbound JOB_PROGRESS envelope and the daemon event stream, and that a
// pause afterwards flushes the last known progress to the server.
func TestJobSupervisorProgressBridge(t *testing.T) {
	f := newSupervisorFixture(t)
	payload := testStartPayload("progress-job")
	f.runner.queueProgress(payload.JobID, []ffmpeg.Progress{
		{ProgressPct: 42.5, FPS: 25, Speed: 1.5, Frame: 100, TotalFrames: 200},
	})

	if err := f.supervisor.StartJob(context.Background(), payload, nil); err != nil {
		t.Fatalf("StartJob: %v", err)
	}

	// Progress must arrive as a JOB_PROGRESS envelope and an event.
	waitFor(t, func() bool { return len(f.envelopesOfType(protocol.TypeJobProgress)) == 1 })
	if !f.sawEvent(func(ev Event) bool {
		return ev.Type == EventJobProgress && ev.JobID == payload.JobID && ev.ProgressPct == 42.5
	}) {
		t.Fatal("no EventJobProgress with ProgressPct 42.5 on the event stream")
	}
	progressEnvelopes := f.envelopesOfType(protocol.TypeJobProgress)
	var live protocol.LiveTelemetry
	if err := json.Unmarshal(progressEnvelopes[0].Payload, &live); err != nil {
		t.Fatalf("unmarshal JOB_PROGRESS: %v", err)
	}
	expectedPath := protocol.SanitizeTelemetryPath(payload.InputPath, payload.RelativePath)
	if live.JobID != payload.JobID || live.ProgressPct != 42.5 || live.InputPath != expectedPath {
		t.Fatalf("live telemetry = %+v, want job/pct/input %q", live, expectedPath)
	}

	// When the runner pauses at the chunk boundary, the supervisor flushes
	// the last known progress again so the dashboard keeps a stable reading.
	hooks := f.runner.hooksFor(t, payload.JobID)
	hooks.OnPause(true)
	waitFor(t, func() bool { return len(f.envelopesOfType(protocol.TypeJobProgress)) == 2 })
	waitFor(t, func() bool { return f.lastStatus() == "PAUSED" })

	pausedEnvelopes := f.envelopesOfType(protocol.TypeJobProgress)
	if !strings.HasPrefix(pausedEnvelopes[1].ID, payload.JobID+"-paused-") {
		t.Fatalf("paused progress envelope ID = %q, want %q-paused-*", pausedEnvelopes[1].ID, payload.JobID)
	}

	hooks.OnPause(false)
	f.runner.release(payload.JobID)
	waitFor(t, func() bool {
		return f.sawEvent(func(ev Event) bool {
			return ev.Type == EventJobComplete && ev.JobID == payload.JobID && ev.Success
		})
	})
}

func TestJobSupervisorPreEncodeProgressBridge(t *testing.T) {
	f := newSupervisorFixture(t)
	payload := testStartPayload("preflight-job")
	f.runner.queueProgress(payload.JobID, []ffmpeg.Progress{
		{Phase: "demux", CurrentIndex: 3, TotalCount: 20, ElapsedSecs: 15.5},
		{Phase: "split", CurrentIndex: 4, TotalCount: 20, ElapsedSecs: 45.0},
	})

	if err := f.supervisor.StartJob(context.Background(), payload, nil); err != nil {
		t.Fatalf("StartJob: %v", err)
	}

	waitFor(t, func() bool { return len(f.envelopesOfType(protocol.TypeJobProgress)) == 2 })
	envelopes := f.envelopesOfType(protocol.TypeJobProgress)

	var d1, d2 protocol.LiveTelemetry
	if err := json.Unmarshal(envelopes[0].Payload, &d1); err != nil {
		t.Fatalf("unmarshal demux envelope: %v", err)
	}
	if d1.Phase != "demux" || d1.CurrentIndex != 3 || d1.TotalCount != 20 || d1.ElapsedSeconds != 15.5 {
		t.Fatalf("unexpected demux telemetry: %+v", d1)
	}

	if err := json.Unmarshal(envelopes[1].Payload, &d2); err != nil {
		t.Fatalf("unmarshal split envelope: %v", err)
	}
	if d2.Phase != "split" || d2.CurrentIndex != 4 || d2.TotalCount != 20 || d2.ElapsedSeconds != 45.0 {
		t.Fatalf("unexpected split telemetry: %+v", d2)
	}

	f.runner.release(payload.JobID)
	f.supervisor.Wait()
}

func TestJobSupervisorVerifyingProgressBridge(t *testing.T) {
	f := newSupervisorFixture(t)
	payload := testStartPayload("verifying-job")
	f.runner.queueProgress(payload.JobID, []ffmpeg.Progress{
		{Phase: "verifying", ProgressPct: 100.0},
	})

	if err := f.supervisor.StartJob(context.Background(), payload, nil); err != nil {
		t.Fatalf("StartJob: %v", err)
	}

	waitFor(t, func() bool { return len(f.envelopesOfType(protocol.TypeJobProgress)) == 1 })
	envelopes := f.envelopesOfType(protocol.TypeJobProgress)

	var live protocol.LiveTelemetry
	if err := json.Unmarshal(envelopes[0].Payload, &live); err != nil {
		t.Fatalf("unmarshal verifying envelope: %v", err)
	}
	if live.Phase != "verifying" || live.Activity != "verifying" || live.ProgressPct != 100.0 {
		t.Fatalf("unexpected verifying telemetry: %+v", live)
	}

	// Verify EventJobProgress with Phase: "verifying" was emitted on the event stream
	if !f.sawEvent(func(ev Event) bool {
		return ev.Type == EventJobProgress && ev.JobID == payload.JobID && ev.Phase == "verifying" && ev.ProgressPct == 100.0
	}) {
		t.Fatal("no EventJobProgress with Phase 'verifying' and ProgressPct 100 on the event stream")
	}

	// Verify EventStatus with Activity: "verifying" was emitted
	if !f.sawEvent(func(ev Event) bool {
		return ev.Type == EventStatus && ev.Status == "WORKING" && ev.Activity == "verifying"
	}) {
		t.Fatal("no EventStatus with Activity 'verifying' on the event stream")
	}

	f.runner.release(payload.JobID)
	f.supervisor.Wait()

	// Verify completion transitions cleanly and resets status
	waitFor(t, func() bool {
		return f.sawEvent(func(ev Event) bool {
			return ev.Type == EventJobComplete && ev.JobID == payload.JobID && ev.Success
		})
	})
	if status := f.lastStatus(); status != "IDLE" {
		t.Fatalf("status after job completion = %q, want IDLE", status)
	}
}

func TestJobSupervisorPlacementProgressBridge(t *testing.T) {
	f := newSupervisorFixture(t)
	payload := testStartPayload("placement-job")
	f.runner.queueProgress(payload.JobID, []ffmpeg.Progress{
		{
			Phase:            "replacing",
			SubPhase:         "quarantine_backup",
			Activity:         "Backing up original",
			TransferredBytes: 4200000000,
			TotalBytes:       9800000000,
			ProgressPct:      42.85,
		},
	})

	if err := f.supervisor.StartJob(context.Background(), payload, nil); err != nil {
		t.Fatalf("StartJob: %v", err)
	}

	waitFor(t, func() bool { return len(f.envelopesOfType(protocol.TypeJobProgress)) == 1 })
	envelopes := f.envelopesOfType(protocol.TypeJobProgress)

	var live protocol.LiveTelemetry
	if err := json.Unmarshal(envelopes[0].Payload, &live); err != nil {
		t.Fatalf("unmarshal placement envelope: %v", err)
	}
	if live.Phase != "replacing" || live.SubPhase != "quarantine_backup" ||
		live.Activity != "Backing up original" || live.TransferredBytes != 4200000000 ||
		live.TotalBytes != 9800000000 || live.ProgressPct != 42.85 {
		t.Fatalf("unexpected placement telemetry: %+v", live)
	}

	// Verify EventJobProgress with placement fields was emitted on the event stream
	if !f.sawEvent(func(ev Event) bool {
		return ev.Type == EventJobProgress && ev.JobID == payload.JobID &&
			ev.Phase == "replacing" && ev.SubPhase == "quarantine_backup" &&
			ev.Activity == "Backing up original" && ev.TransferredBytes == 4200000000 &&
			ev.TotalBytes == 9800000000 && ev.ProgressPct == 42.85
	}) {
		t.Fatal("no EventJobProgress with placement fields on the event stream")
	}

	// Verify EventStatus with Activity: "Backing up original" was emitted
	if !f.sawEvent(func(ev Event) bool {
		return ev.Type == EventStatus && ev.Status == "WORKING" && ev.Activity == "Backing up original"
	}) {
		t.Fatal("no EventStatus with Activity 'Backing up original' on the event stream")
	}

	f.runner.release(payload.JobID)
	f.supervisor.Wait()

	// Verify completion transitions cleanly and resets status
	waitFor(t, func() bool {
		return f.sawEvent(func(ev Event) bool {
			return ev.Type == EventJobComplete && ev.JobID == payload.JobID && ev.Success
		})
	})
	if status := f.lastStatus(); status != "IDLE" {
		t.Fatalf("status after job completion = %q, want IDLE", status)
	}
}



// TestJobSupervisorCompletionEnvelope verifies a successful run reports
// sizes on both the FILE_COMPLETE envelope and the event stream.
func TestJobSupervisorCompletionEnvelope(t *testing.T) {
	f := newSupervisorFixture(t)
	payload := testStartPayload("done-job")

	if err := f.supervisor.StartJob(context.Background(), payload, nil); err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	<-f.runner.started
	f.runner.release(payload.JobID)

	waitFor(t, func() bool {
		return f.sawEvent(func(ev Event) bool {
			return ev.Type == EventJobComplete && ev.Success && ev.SavingsBytes == 500
		})
	})

	completes := f.envelopesOfType(protocol.TypeFileComplete)
	if len(completes) != 1 {
		t.Fatalf("FILE_COMPLETE envelopes = %d, want 1", len(completes))
	}
	var complete protocol.FileCompletePayload
	if err := json.Unmarshal(completes[0].Payload, &complete); err != nil {
		t.Fatalf("unmarshal FILE_COMPLETE: %v", err)
	}
	if !complete.Success || complete.OriginalSize != 1000 || complete.NewSize != 500 || complete.SavingsBytes != 500 {
		t.Fatalf("complete payload = %+v, want success with 1000/500/500", complete)
	}
}

// TestJobSupervisorStaysWorkingUntilLastJobCompletes is the supervisor-side
// regression for the IDLE race: finishing one of two concurrent jobs must
// not flip the agent to IDLE while the sibling is still running.
func TestJobSupervisorStaysWorkingUntilLastJobCompletes(t *testing.T) {
	f := newSupervisorFixture(t)

	if err := f.supervisor.StartJob(context.Background(), testStartPayload("job-a"), nil); err != nil {
		t.Fatalf("StartJob a: %v", err)
	}
	if err := f.supervisor.StartJob(context.Background(), testStartPayload("job-b"), nil); err != nil {
		t.Fatalf("StartJob b: %v", err)
	}
	<-f.runner.started
	<-f.runner.started
	waitFor(t, func() bool { return f.supervisor.ActiveCount() == 2 })

	f.runner.release("job-a")
	waitFor(t, func() bool { return f.supervisor.ActiveCount() == 1 })
	if status := f.lastStatus(); status != "WORKING" {
		t.Fatalf("status after first job finished = %q, want WORKING", status)
	}

	f.runner.release("job-b")
	waitFor(t, func() bool { return f.supervisor.ActiveCount() == 0 })
	if status := f.lastStatus(); status != "IDLE" {
		t.Fatalf("status after last job finished = %q, want IDLE", status)
	}

	waitDone := make(chan struct{})
	go func() { f.supervisor.Wait(); close(waitDone) }()
	select {
	case <-waitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor Wait did not return after both jobs finished")
	}
}

// TestJobSupervisorRejectsDuplicateActiveWorkingPaths verifies that attempting
// to start a transcode on an active path or file returns an error, and releasing
// the job permits subsequent jobs on that path.
func TestJobSupervisorRejectsDuplicateActiveWorkingPaths(t *testing.T) {
	f := newSupervisorFixture(t)
	payload1 := testStartPayload("job-1")
	payload1.InputPath = "/media/movies/movie-duplicate.mkv"

	if err := f.supervisor.StartJob(context.Background(), payload1, nil); err != nil {
		t.Fatalf("StartJob 1: %v", err)
	}
	<-f.runner.started

	// Second job on the exact same input path must be rejected with error
	payload2 := testStartPayload("job-2")
	payload2.InputPath = "/media/movies/movie-duplicate.mkv"
	err := f.supervisor.StartJob(context.Background(), payload2, nil)
	if err == nil {
		t.Fatal("expected StartJob 2 to fail due to active working path lock, got nil")
	}
	if !strings.Contains(err.Error(), "locked") {
		t.Fatalf("expected error message to mention locked path, got: %v", err)
	}

	// Runner must not have been invoked for job-2
	select {
	case id := <-f.runner.started:
		t.Fatalf("runner was invoked for rejected duplicate job %s", id)
	default:
	}

	// Completing job-1 releases the lock
	f.runner.release("job-1")
	waitFor(t, func() bool { return f.supervisor.ActiveCount() == 0 })

	// Job-2 should now succeed
	if err := f.supervisor.StartJob(context.Background(), payload2, nil); err != nil {
		t.Fatalf("StartJob 2 after job-1 completed failed: %v", err)
	}
	<-f.runner.started
	f.runner.release("job-2")
	waitFor(t, func() bool { return f.supervisor.ActiveCount() == 0 })
}

// TestJobSupervisorDuplicateStartJobIsIdempotentNoOp verifies that receiving
// a duplicate START_JOB for an already in-flight job returns nil without re-running gates,
// does not launch duplicate runner workers, and never sends a failure FILE_COMPLETE envelope.
func TestJobSupervisorDuplicateStartJobIsIdempotentNoOp(t *testing.T) {
	f := newSupervisorFixture(t)
	payload := testStartPayload("job-dup")

	if err := f.supervisor.StartJob(context.Background(), payload, nil); err != nil {
		t.Fatalf("initial StartJob failed: %v", err)
	}
	select {
	case id := <-f.runner.started:
		if id != "job-dup" {
			t.Fatalf("runner started job %s, want job-dup", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not start")
	}

	if n := f.supervisor.ActiveCount(); n != 1 {
		t.Fatalf("active count before duplicate = %d, want 1", n)
	}

	// Send duplicate start command for the actively running job
	if err := f.supervisor.StartJob(context.Background(), payload, nil); err != nil {
		t.Fatalf("expected duplicate StartJob to return nil as an idempotent no-op, got error: %v", err)
	}

	// Verify no duplicate runner worker was spawned
	select {
	case id := <-f.runner.started:
		t.Fatalf("unexpected runner invocation for duplicate job %s", id)
	case <-time.After(100 * time.Millisecond):
	}

	// Verify no failure FILE_COMPLETE envelope was emitted
	completes := f.envelopesOfType(protocol.TypeFileComplete)
	if len(completes) != 0 {
		t.Fatalf("expected 0 FILE_COMPLETE envelopes during duplicate start, got %d", len(completes))
	}

	if n := f.supervisor.ActiveCount(); n != 1 {
		t.Fatalf("active count after duplicate = %d, want 1", n)
	}

	// Let the original job finish successfully
	f.runner.release("job-dup")
	waitFor(t, func() bool { return f.supervisor.ActiveCount() == 0 })

	// Verify only 1 FILE_COMPLETE envelope was sent and it was successful
	completes = f.envelopesOfType(protocol.TypeFileComplete)
	if len(completes) != 1 {
		t.Fatalf("expected 1 FILE_COMPLETE envelope after completion, got %d", len(completes))
	}
	var complete protocol.FileCompletePayload
	if err := json.Unmarshal(completes[0].Payload, &complete); err != nil {
		t.Fatalf("unmarshal FILE_COMPLETE: %v", err)
	}
	if !complete.Success {
		t.Fatalf("complete payload success = false (%s), want true", complete.ErrorMessage)
	}
}

// TestJobSupervisorReleasesWorkingPathsOnCancellation verifies path locks are
// freed when an active job is cancelled.
func TestJobSupervisorReleasesWorkingPathsOnCancellation(t *testing.T) {
	f := newSupervisorFixture(t)
	payload1 := testStartPayload("job-cancel-1")
	payload1.InputPath = "/media/movies/movie-cancel.mkv"

	if err := f.supervisor.StartJob(context.Background(), payload1, nil); err != nil {
		t.Fatalf("StartJob 1: %v", err)
	}
	<-f.runner.started

	// Cancel job 1
	f.supervisor.CancelJob(context.Background(), "job-cancel-1")
	waitFor(t, func() bool { return f.supervisor.ActiveCount() == 0 })

	// Second job on the same path must now succeed
	payload2 := testStartPayload("job-cancel-2")
	payload2.InputPath = "/media/movies/movie-cancel.mkv"
	if err := f.supervisor.StartJob(context.Background(), payload2, nil); err != nil {
		t.Fatalf("StartJob 2 after cancellation failed: %v", err)
	}
	<-f.runner.started
	f.runner.release("job-cancel-2")
	waitFor(t, func() bool { return f.supervisor.ActiveCount() == 0 })
}

// TestJobSupervisorConcurrentSupervisorsSharedLocker verifies that two
// supervisor instances sharing a PathLocker prevent concurrent transcodes on the same path.
func TestJobSupervisorConcurrentSupervisorsSharedLocker(t *testing.T) {
	sharedLocker := NewPathLocker()

	runner1 := newGateRunner()
	s1 := NewJobSupervisor(SupervisorDeps{
		Runner:      runner1,
		Confinement: passThroughConfinement{},
		PathLocker:  sharedLocker,
	})

	runner2 := newGateRunner()
	s2 := NewJobSupervisor(SupervisorDeps{
		Runner:      runner2,
		Confinement: passThroughConfinement{},
		PathLocker:  sharedLocker,
	})

	p1 := testStartPayload("s1-job")
	p1.InputPath = "/media/movies/shared-movie.mkv"

	p2 := testStartPayload("s2-job")
	p2.InputPath = "/media/movies/shared-movie.mkv"

	if err := s1.StartJob(context.Background(), p1, nil); err != nil {
		t.Fatalf("s1 StartJob failed: %v", err)
	}
	<-runner1.started

	// s2 attempting to start on the same working path must fail
	if err := s2.StartJob(context.Background(), p2, nil); err == nil {
		t.Fatal("expected s2 StartJob to fail with path collision, got nil")
	}

	runner1.release("s1-job")
	waitFor(t, func() bool { return s1.ActiveCount() == 0 })

	// s2 can now acquire the path
	if err := s2.StartJob(context.Background(), p2, nil); err != nil {
		t.Fatalf("s2 StartJob failed after s1 released: %v", err)
	}
	<-runner2.started
	runner2.release("s2-job")
	waitFor(t, func() bool { return s2.ActiveCount() == 0 })
}

func TestJobSupervisorUpdateSchedule_PausesRunningJob(t *testing.T) {
	f := newSupervisorFixture(t)
	var reportedStatus, reportedActivity string
	var statusMu sync.Mutex
	f.supervisor.setStatus = func(status, activity string) {
		statusMu.Lock()
		defer statusMu.Unlock()
		reportedStatus = status
		reportedActivity = activity
	}

	payload := testStartPayload("job-active-transcode")
	if err := f.supervisor.StartJob(context.Background(), payload, nil); err != nil {
		t.Fatalf("StartJob failed: %v", err)
	}
	<-f.runner.started

	statusMu.Lock()
	if reportedStatus != "WORKING" || reportedActivity != "" {
		t.Fatalf("initial status = %q/%q, want WORKING/empty", reportedStatus, reportedActivity)
	}
	statusMu.Unlock()

	quietSched := &protocol.ScheduleConfig{
		QuietHoursEnabled: true,
		ScheduleGrid:      protocol.WeeklySchedule(strings.Repeat("1", 168)),
	}
	f.supervisor.UpdateSchedule(quietSched)

	statusMu.Lock()
	if reportedStatus != "WORKING" || reportedActivity != "pausing" {
		t.Fatalf("after quiet hours enabled, status = %q/%q, want WORKING/'pausing'", reportedStatus, reportedActivity)
	}
	statusMu.Unlock()

	f.runner.release(payload.JobID)
	waitFor(t, func() bool { return f.supervisor.ActiveCount() == 0 })
}

func TestJobSupervisorDynamicTelemetryThrottler(t *testing.T) {
	f := newSupervisorFixture(t)
	payload := testStartPayload("throttle-job")

	// Set dynamic telemetry rate to 200ms
	f.supervisor.SetTelemetryRate(protocol.SetTelemetryRatePayload{
		RateMS: 200,
	})
	if f.supervisor.TelemetryInterval() != 200*time.Millisecond {
		t.Fatalf("expected telemetry interval 200ms, got %v", f.supervisor.TelemetryInterval())
	}

	// Queue progress reports:
	// First event: encode at 10% (should send immediately)
	// Second event: encode at 20% (immediate burst, should be throttled/dropped)
	f.runner.queueProgress(payload.JobID, []ffmpeg.Progress{
		{Phase: "encode", ProgressPct: 10.0, FPS: 30},
		{Phase: "encode", ProgressPct: 20.0, FPS: 30},
	})

	if err := f.supervisor.StartJob(context.Background(), payload, nil); err != nil {
		t.Fatalf("StartJob: %v", err)
	}

	// Should emit exactly 1 JOB_PROGRESS envelope because the 2nd one was within 200ms in the same phase
	waitFor(t, func() bool { return len(f.envelopesOfType(protocol.TypeJobProgress)) == 1 })

	// Wait 250ms exceeding the 200ms throttle interval, then trigger via runner progress callback
	time.Sleep(250 * time.Millisecond)

	// Since runner is blocked on gate, verify via the hooks progress callback directly if possible, or release
	// Let's release the job and verify FILE_COMPLETE is sent immediately bypassing throttling
	f.runner.release(payload.JobID)

	waitFor(t, func() bool { return len(f.envelopesOfType(protocol.TypeFileComplete)) == 1 })
	completes := f.envelopesOfType(protocol.TypeFileComplete)
	var complete protocol.FileCompletePayload
	if err := json.Unmarshal(completes[0].Payload, &complete); err != nil {
		t.Fatalf("unmarshal FILE_COMPLETE: %v", err)
	}
	if !complete.Success {
		t.Fatalf("FILE_COMPLETE failed: %s", complete.ErrorMessage)
	}

	// Test mode switching defaults
	f.supervisor.SetTelemetryRate(protocol.SetTelemetryRatePayload{
		Mode: protocol.TelemetryModeBackground,
	})
	if f.supervisor.TelemetryInterval() != 15*time.Second {
		t.Fatalf("expected telemetry interval 15s for BACKGROUND mode, got %v", f.supervisor.TelemetryInterval())
	}

	f.supervisor.SetTelemetryRate(protocol.SetTelemetryRatePayload{
		Mode: protocol.TelemetryModeIdle,
	})
	if f.supervisor.TelemetryInterval() != 30*time.Second {
		t.Fatalf("expected telemetry interval 30s for IDLE mode, got %v", f.supervisor.TelemetryInterval())
	}

	f.supervisor.SetTelemetryRate(protocol.SetTelemetryRatePayload{
		Mode: protocol.TelemetryModeLive,
	})
	if f.supervisor.TelemetryInterval() != 1*time.Second {
		t.Fatalf("expected telemetry interval 1s for LIVE mode, got %v", f.supervisor.TelemetryInterval())
	}
}

func TestJobSupervisor_ConcurrencyBoundsClampingAndDefaults(t *testing.T) {
	runner := newGateRunner()
	// Default when unset or <= 0
	supDefault := NewJobSupervisor(SupervisorDeps{
		Runner:      runner,
		Confinement: passThroughConfinement{},
	})
	if supDefault.Concurrency() != MaxConcurrentTranscodes {
		t.Fatalf("default concurrency = %d, want %d", supDefault.Concurrency(), MaxConcurrentTranscodes)
	}

	// Explicit valid concurrency in deps
	supExplicit := NewJobSupervisor(SupervisorDeps{
		Runner:      runner,
		Confinement: passThroughConfinement{},
		Concurrency: 3,
	})
	if supExplicit.Concurrency() != 3 {
		t.Fatalf("explicit concurrency = %d, want 3", supExplicit.Concurrency())
	}

	// Clamping on SetConcurrency
	supExplicit.SetConcurrency(0)
	if supExplicit.Concurrency() != 1 {
		t.Fatalf("SetConcurrency(0) = %d, want 1", supExplicit.Concurrency())
	}
	supExplicit.SetConcurrency(-10)
	if supExplicit.Concurrency() != 1 {
		t.Fatalf("SetConcurrency(-10) = %d, want 1", supExplicit.Concurrency())
	}
	supExplicit.SetConcurrency(MaxConcurrentTranscodes + 5)
	if supExplicit.Concurrency() != MaxConcurrentTranscodes {
		t.Fatalf("SetConcurrency(over max) = %d, want %d", supExplicit.Concurrency(), MaxConcurrentTranscodes)
	}
	supExplicit.SetConcurrency(4)
	if supExplicit.Concurrency() != 4 {
		t.Fatalf("SetConcurrency(4) = %d, want 4", supExplicit.Concurrency())
	}
}

func TestJobSupervisor_ConcurrencyBounds_BurstOverCapacity(t *testing.T) {
	f := newSupervisorFixture(t)
	f.supervisor.SetConcurrency(2)

	// Start two jobs to saturate the agent's concurrency capacity
	if err := f.supervisor.StartJob(context.Background(), testStartPayload("active-1"), nil); err != nil {
		t.Fatalf("StartJob active-1: %v", err)
	}
	if err := f.supervisor.StartJob(context.Background(), testStartPayload("active-2"), nil); err != nil {
		t.Fatalf("StartJob active-2: %v", err)
	}

	<-f.runner.started
	<-f.runner.started
	waitFor(t, func() bool { return f.supervisor.ActiveCount() == 2 })

	// Under saturated conditions, burst 10 concurrent START_JOB requests
	burstCount := 10
	errChan := make(chan error, burstCount)
	var wg sync.WaitGroup

	for i := 0; i < burstCount; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			jobID := fmt.Sprintf("burst-job-%d", idx)
			errChan <- f.supervisor.StartJob(context.Background(), testStartPayload(jobID), nil)
		}(i)
	}

	wg.Wait()
	close(errChan)

	// All 10 burst jobs must be rejected/deferred
	rejectedCount := 0
	for err := range errChan {
		if err == nil {
			t.Fatal("expected burst job to be rejected when saturated, got nil error")
		}
		if !strings.Contains(err.Error(), "deferred: agent concurrency limit reached") {
			t.Fatalf("expected error to contain 'deferred: agent concurrency limit reached', got: %v", err)
		}
		rejectedCount++
	}
	if rejectedCount != burstCount {
		t.Fatalf("rejectedCount = %d, want %d", rejectedCount, burstCount)
	}

	// Verify no additional runner instances were spawned
	select {
	case unexpectedID := <-f.runner.started:
		t.Fatalf("unexpected runner execution for over-capacity job %s", unexpectedID)
	case <-time.After(100 * time.Millisecond):
	}

	// Verify FILE_COMPLETE rejection envelopes emitted for all rejected burst jobs
	waitFor(t, func() bool {
		completes := f.envelopesOfType(protocol.TypeFileComplete)
		return len(completes) >= burstCount
	})

	completes := f.envelopesOfType(protocol.TypeFileComplete)
	if len(completes) != burstCount {
		t.Fatalf("expected %d FILE_COMPLETE rejection envelopes, got %d", burstCount, len(completes))
	}
	for _, env := range completes {
		var payload protocol.FileCompletePayload
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			t.Fatalf("unmarshal FILE_COMPLETE: %v", err)
		}
		if payload.Success {
			t.Fatalf("expected failed/deferred FILE_COMPLETE, got success for job %s", payload.JobID)
		}
		if !strings.HasPrefix(payload.ErrorMessage, "deferred: agent concurrency limit reached") {
			t.Fatalf("unexpected error message in FILE_COMPLETE: %s", payload.ErrorMessage)
		}
	}

	// Active count must still be exactly 2
	if n := f.supervisor.ActiveCount(); n != 2 {
		t.Fatalf("active count during saturation = %d, want 2", n)
	}

	// Release the active jobs and ensure capacity is restored
	f.runner.release("active-1")
	f.runner.release("active-2")
	waitFor(t, func() bool { return f.supervisor.ActiveCount() == 0 })

	// Start a fresh job now that capacity is restored
	if err := f.supervisor.StartJob(context.Background(), testStartPayload("job-fresh"), nil); err != nil {
		t.Fatalf("StartJob job-fresh after drain: %v", err)
	}
	select {
	case id := <-f.runner.started:
		if id != "job-fresh" {
			t.Fatalf("runner started %s, want job-fresh", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not start job-fresh")
	}
	f.runner.release("job-fresh")
	waitFor(t, func() bool { return f.supervisor.ActiveCount() == 0 })
}

func TestJobSupervisor_DuplicateStartJob_Burst(t *testing.T) {
	f := newSupervisorFixture(t)
	jobID := "burst-dup-job"
	payload := testStartPayload(jobID)

	burstCount := 10
	var wg sync.WaitGroup
	errChan := make(chan error, burstCount)

	// Launch burst of duplicate starts simultaneously
	startGate := make(chan struct{})
	for i := 0; i < burstCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-startGate
			errChan <- f.supervisor.StartJob(context.Background(), payload, nil)
		}()
	}

	close(startGate)
	wg.Wait()
	close(errChan)

	// All starts should succeed (the first admits the job, siblings are idempotent no-ops)
	for err := range errChan {
		if err != nil {
			t.Fatalf("expected duplicate StartJob to return nil as no-op, got error: %v", err)
		}
	}

	// Runner must be invoked exactly once
	select {
	case id := <-f.runner.started:
		if id != jobID {
			t.Fatalf("runner started %s, want %s", id, jobID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runner was not started")
	}

	select {
	case unexpectedID := <-f.runner.started:
		t.Fatalf("unexpected duplicate runner invocation for %s", unexpectedID)
	case <-time.After(100 * time.Millisecond):
	}

	// Active count must be 1
	if n := f.supervisor.ActiveCount(); n != 1 {
		t.Fatalf("active count = %d, want 1", n)
	}

	// No failure FILE_COMPLETE envelopes
	completes := f.envelopesOfType(protocol.TypeFileComplete)
	if len(completes) != 0 {
		t.Fatalf("expected 0 FILE_COMPLETE envelopes during duplicate burst, got %d", len(completes))
	}

	// Release and finish cleanly
	f.runner.release(jobID)
	waitFor(t, func() bool { return f.supervisor.ActiveCount() == 0 })

	completes = f.envelopesOfType(protocol.TypeFileComplete)
	if len(completes) != 1 {
		t.Fatalf("expected exactly 1 FILE_COMPLETE envelope upon completion, got %d", len(completes))
	}
	var complete protocol.FileCompletePayload
	if err := json.Unmarshal(completes[0].Payload, &complete); err != nil {
		t.Fatalf("unmarshal FILE_COMPLETE: %v", err)
	}
	if !complete.Success {
		t.Fatalf("expected successful FILE_COMPLETE, got error: %s", complete.ErrorMessage)
	}
}

func TestSupervisor_ScrubsPathsInTelemetryAndTraces(t *testing.T) {
	mediaDir := t.TempDir()
	scratchDir := filepath.Join(t.TempDir(), "scratch")
	quarantineDir := filepath.Join(t.TempDir(), "quarantine")
	_ = os.MkdirAll(scratchDir, 0755)
	_ = os.MkdirAll(quarantineDir, 0755)

	relPath := "movies/action/film.mkv"
	fullInput := filepath.Join(mediaDir, relPath)
	_ = os.MkdirAll(filepath.Dir(fullInput), 0755)
	_ = os.WriteFile(fullInput, []byte("fake-video"), 0644)

	runner := newGateRunner()
	var mu sync.Mutex
	var envelopes []protocol.Envelope
	recordEnvelope := func(env protocol.Envelope) {
		mu.Lock()
		defer mu.Unlock()
		envelopes = append(envelopes, env)
	}

	sup := NewJobSupervisor(SupervisorDeps{
		Runner:        runner,
		Confinement:   passThroughConfinement{},
		Emit:          func(Event) {},
		Send:          recordEnvelope,
		SetStatus:     func(status, activity string) {},
		ScratchDir:    scratchDir,
		QuarantineDir: quarantineDir,
		MediaDir:      mediaDir,
	})

	jobID := "job-scrub-success"
	runner.queueProgress(jobID, []ffmpeg.Progress{
		{Phase: "encode", ProgressPct: 50.0},
	})
	runner.queueCommands(jobID, []protocol.FfmpegCommandRecord{
		{
			Phase: "encode",
			Argv: []string{
				"-i", fullInput,
				"-vf", "subtitles=" + filepath.Join(scratchDir, "job-scrub-success", "subs.srt"),
				filepath.Join(scratchDir, "job-scrub-success", "chunk.mkv"),
			},
			Count: 1,
		},
	})

	startPayload := protocol.StartJobPayload{
		JobID:        jobID,
		MediaItemID:  "media-item-1",
		InputPath:    fullInput,
		RelativePath: relPath,
		SpecVersion:  1,
	}
	raw, _ := json.Marshal(startPayload)
	if err := sup.StartJob(context.Background(), startPayload, raw); err != nil {
		t.Fatalf("StartJob failed: %v", err)
	}

	<-runner.started
	runner.release(jobID)
	waitFor(t, func() bool { return sup.ActiveCount() == 0 })

	mu.Lock()
	var completeEnv *protocol.Envelope
	var progressEnvs []protocol.Envelope
	for i := range envelopes {
		if envelopes[i].Type == protocol.TypeFileComplete {
			completeEnv = &envelopes[i]
		} else if envelopes[i].Type == protocol.TypeJobProgress {
			progressEnvs = append(progressEnvs, envelopes[i])
		}
	}
	mu.Unlock()

	// Verify progress envelopes scrub input path
	for _, env := range progressEnvs {
		var prog protocol.LiveTelemetry
		if err := json.Unmarshal(env.Payload, &prog); err != nil {
			t.Fatalf("unmarshal progress envelope: %v", err)
		}
		if strings.Contains(prog.InputPath, mediaDir) {
			t.Errorf("progress envelope leaked host media path: %s", prog.InputPath)
		}
		if prog.InputPath != relPath {
			t.Errorf("progress envelope input path = %q, want %s", prog.InputPath, relPath)
		}
	}

	if completeEnv == nil {
		t.Fatal("expected FILE_COMPLETE envelope")
	}
	var complete protocol.FileCompletePayload
	if err := json.Unmarshal(completeEnv.Payload, &complete); err != nil {
		t.Fatalf("unmarshal FILE_COMPLETE: %v", err)
	}
	if !complete.Success {
		t.Fatalf("expected success, got err: %s", complete.ErrorMessage)
	}
	if strings.Contains(complete.InputPath, mediaDir) {
		t.Errorf("complete envelope leaked host media path: %s", complete.InputPath)
	}
	if complete.InputPath != relPath {
		t.Errorf("complete envelope input path = %q, want %s", complete.InputPath, relPath)
	}
	if len(complete.FfmpegCommands) == 0 {
		t.Fatal("expected FfmpegCommands in complete payload")
	}
	for _, cmd := range complete.FfmpegCommands {
		for _, arg := range cmd.Argv {
			if strings.Contains(arg, scratchDir) {
				t.Errorf("ffmpeg command leaked scratchDir: %s", arg)
			}
			if strings.Contains(arg, mediaDir) {
				t.Errorf("ffmpeg command leaked mediaDir: %s", arg)
			}
		}
	}

	// Verify failure error message scrubbing
	failJobID := "job-scrub-failure"
	failRelPath := "tv/show.mkv"
	fullFailInput := filepath.Join(mediaDir, failRelPath)
	_ = os.MkdirAll(filepath.Dir(fullFailInput), 0755)
	_ = os.WriteFile(fullFailInput, []byte("fake-video"), 0644)

	runner.queueFailure(failJobID, errors.New("ffmpeg exited 1: "+filepath.Join(scratchDir, "job-scrub-failure", "out.mkv")+": invalid data"))
	failStartPayload := protocol.StartJobPayload{
		JobID:        failJobID,
		MediaItemID:  "media-item-2",
		InputPath:    fullFailInput,
		RelativePath: failRelPath,
		SpecVersion:  1,
	}
	failRaw, _ := json.Marshal(failStartPayload)
	if err := sup.StartJob(context.Background(), failStartPayload, failRaw); err != nil {
		t.Fatalf("StartJob for failure test failed: %v", err)
	}
	<-runner.started
	runner.release(failJobID)
	waitFor(t, func() bool { return sup.ActiveCount() == 0 })

	mu.Lock()
	var failCompleteEnv *protocol.Envelope
	for i := range envelopes {
		if envelopes[i].Type == protocol.TypeFileComplete && strings.Contains(envelopes[i].ID, failJobID) {
			failCompleteEnv = &envelopes[i]
		}
	}
	mu.Unlock()

	if failCompleteEnv == nil {
		t.Fatal("expected FILE_COMPLETE envelope for failure")
	}
	var failComplete protocol.FileCompletePayload
	if err := json.Unmarshal(failCompleteEnv.Payload, &failComplete); err != nil {
		t.Fatalf("unmarshal failure FILE_COMPLETE: %v", err)
	}
	if failComplete.Success {
		t.Fatal("expected failure outcome")
	}
	if strings.Contains(failComplete.ErrorMessage, scratchDir) {
		t.Errorf("failure error message leaked scratchDir: %s", failComplete.ErrorMessage)
	}
	if !strings.Contains(failComplete.ErrorMessage, "<SCRATCH_DIR>/job-scrub-failure/out.mkv") {
		t.Errorf("failure error message = %q, want containing <SCRATCH_DIR>/job-scrub-failure/out.mkv", failComplete.ErrorMessage)
	}
}

