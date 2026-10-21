package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/will25u/slimmedia-agent/internal/db"
	"github.com/will25u/slimmedia-agent/internal/ffmpeg"
	"github.com/will25u/slimmedia-agent/internal/hardware"
	"github.com/will25u/slimmedia-agent/internal/jobpaths"
	"github.com/will25u/slimmedia-agent/internal/jobspec"
	"github.com/will25u/slimmedia-agent/internal/safety"
	"github.com/will25u/slimmedia-agent/internal/transcoder"
	"github.com/will25u/slimmedia-agent/internal/models"
	"github.com/will25u/slimmedia-agent/internal/protocol"
)

// Runner is the orchestrator seam the JobSupervisor drives.
// *transcoder.Orchestrator satisfies it; unit tests substitute a fake.
type Runner interface {
	ProcessJobWithSpec(ctx context.Context, job *transcoder.Job, targetCodec string, spec jobspec.JobSpec, hooks transcoder.JobHooks, onProgress transcoder.ProgressCallback) (*transcoder.JobResult, error)
	DiscardJobFiles(ctx context.Context, filePath, jobID string)
}

// JobJournal is the cancel-time lookup the supervisor needs to clean up
// scratch files of jobs that already terminated locally. *db.Queries
// satisfies it; nil disables the cleanup.
type JobJournal interface {
	GetJobJournal(ctx context.Context, jobID string) (db.JobJournal, error)
}

// StreamSafetyChecker gates job starts on active media-server streams.
// *safety.MediaServerChecker satisfies it; nil skips the check.
type StreamSafetyChecker interface {
	CheckConflict(ctx context.Context, inputPath string) (conflict bool, serverName string, err error)
}

// InputValidator gates job starts on the path-confinement rule: the
// server-supplied input path must resolve to a file the local scanner
// discovered. *scannedLibraryConfinement satisfies it.
type InputValidator interface {
	ValidateJobInputPath(inputPath string) (string, error)
}

// SupervisorDeps are the JobSupervisor's injected dependencies. Every field
// except Runner and Confinement may be nil, which disables the corresponding
// behavior; the telemetry callbacks deliver progress and outcomes to whoever
// owns the outbound channels (the daemon's event stream and websocket client).
type SupervisorDeps struct {
	Runner Runner

	Journal JobJournal
	Safety  StreamSafetyChecker
	// Confinement resolves and validates the server-supplied input path.
	Confinement InputValidator
	// Updating reports whether an agent update is scheduled or in
	// progress; job starts defer while it returns true. nil means never.
	Updating func() bool

	// PathLocker manages mutual exclusion over working paths across jobs;
	// nil defaults to an in-memory locker.
	PathLocker PathLocker

	ScratchDir    string
	QuarantineDir string
	MediaDir      string

	Emit      func(Event)
	Send      func(protocol.Envelope)
	SetStatus func(status, activity string)
	// OnJobComplete, when set, is called on any terminal job outcome (success, failure, cancellation, or rejection).
	OnJobComplete func(jobID string, success bool)

	// Concurrency is the maximum number of concurrent transcode jobs the
	// supervisor will admit. If <= 0 or > maxConcurrentTranscodes, it
	// defaults to maxConcurrentTranscodes.
	Concurrency int
}

// JobSupervisor owns the lifecycle of every in-flight transcode job: the
// per-job cancel funcs and pause flags, the run goroutine, and the bridge
// from transcoder progress to outbound telemetry envelopes. It contains no
// job state beyond what is needed to run and stop jobs — preset tuning lives
// in the jobspec, path layout in the jobpaths, and daemon lifecycle/wiring
// stays on the Daemon.
type JobSupervisor struct {
	runner        Runner
	journal       JobJournal
	safety        StreamSafetyChecker
	confinement   InputValidator
	updating      func() bool
	pathLocker    PathLocker
	scratchDir    string
	quarantineDir string
	mediaDirMu    sync.RWMutex
	mediaDir      string
	mountErrMu    sync.RWMutex
	mountErr      string
	emit          func(Event)
	send          func(protocol.Envelope)
	setStatus     func(status, activity string)
	onJobComplete func(jobID string, success bool)
	concurrency   atomic.Int32

	// mu guards cancels and pauses, the per-job registries consulted by
	// CancelJob/PauseJob/ResumeJob and by the drain-time CancelAll.
	mu      sync.Mutex
	cancels map[string]context.CancelFunc
	pauses  map[string]*atomic.Bool

	// wg and count track in-flight jobs for daemon shutdown: Wait blocks
	// until every run goroutine finished, CancelAll force-cancels them.
	wg    sync.WaitGroup
	count atomic.Int64

	scheduleMu      sync.RWMutex
	currentSchedule *protocol.ScheduleConfig

	rateMu            sync.RWMutex
	telemetryInterval time.Duration
}

// NewJobSupervisor constructs a supervisor with its dependencies injected.
func NewJobSupervisor(deps SupervisorDeps) *JobSupervisor {
	if deps.Runner == nil {
		panic("NewJobSupervisor: Runner is nil")
	}
	if deps.Confinement == nil {
		panic("NewJobSupervisor: Confinement is nil")
	}
	locker := deps.PathLocker
	if locker == nil {
		locker = NewPathLocker()
	}
	concurrency := deps.Concurrency
	if concurrency <= 0 || concurrency > maxConcurrentTranscodes {
		concurrency = maxConcurrentTranscodes
	}
	s := &JobSupervisor{
		runner:            deps.Runner,
		journal:           deps.Journal,
		safety:            deps.Safety,
		confinement:       deps.Confinement,
		updating:          deps.Updating,
		pathLocker:        locker,
		scratchDir:        deps.ScratchDir,
		quarantineDir:     deps.QuarantineDir,
		mediaDir:          deps.MediaDir,
		emit:              deps.Emit,
		send:              deps.Send,
		setStatus:         deps.SetStatus,
		onJobComplete:     deps.OnJobComplete,
		cancels:           make(map[string]context.CancelFunc),
		pauses:            make(map[string]*atomic.Bool),
		telemetryInterval: time.Second,
	}
	s.concurrency.Store(int32(concurrency))
	return s
}

// GetMediaDir returns the supervisor's current media directory root.
func (s *JobSupervisor) GetMediaDir() string {
	s.mediaDirMu.RLock()
	defer s.mediaDirMu.RUnlock()
	return s.mediaDir
}

// SetMediaDir updates the supervisor's current media directory root.
func (s *JobSupervisor) SetMediaDir(dir string) {
	s.mediaDirMu.Lock()
	defer s.mediaDirMu.Unlock()
	s.mediaDir = CleanLocalPath(dir)
}

// SetMountError updates the supervisor's recorded library mount error state.
func (s *JobSupervisor) SetMountError(errStr string) {
	s.mountErrMu.Lock()
	defer s.mountErrMu.Unlock()
	s.mountErr = errStr
}

// GetMountError returns any recorded library mount error.
func (s *JobSupervisor) GetMountError() string {
	s.mountErrMu.RLock()
	defer s.mountErrMu.RUnlock()
	return s.mountErr
}

// StartJob evaluates the job-start gates (agent-update deferral, path
// confinement, quiet hours, stream safety) and, when they pass, launches the
// run goroutine. startPayload.InputPath is the server-supplied path used for
// telemetry; the confinement check resolves it to the canonical local input
// handed to the runner. Individual gates emit DEBUG logs on entry and exit,
// with summary INFO logs marking preflight start and completion. The job is
// registered as cancellable before the gates run: a CANCEL_JOB arriving
// while a gate executes aborts the start instead of being dropped as an
// unknown job ID. Gate rejections report the terminal outcome through the
// telemetry callbacks and return a descriptive error; a launched job returns
// nil and reports its outcome asynchronously on the event stream and through
// Send.
func (s *JobSupervisor) StartJob(parentCtx context.Context, startPayload protocol.StartJobPayload, rawPayload json.RawMessage) error {
	s.mu.Lock()
	if _, active := s.cancels[startPayload.JobID]; active {
		s.mu.Unlock()
		log.Printf("Job %s is already active; ignoring duplicate start as no-op", startPayload.JobID)
		return nil
	}

	limit := s.Concurrency()
	activeJobs := len(s.cancels)
	if c := int(s.count.Load()); c > activeJobs {
		activeJobs = c
	}
	if activeJobs >= limit {
		s.mu.Unlock()
		log.Printf("Job %s deferred: agent concurrency limit reached (%d/%d)", startPayload.JobID, activeJobs, limit)
		reason := "deferred: agent concurrency limit reached"
		s.rejectJob(startPayload, reason)
		return fmt.Errorf("job %s: %s", startPayload.JobID, reason)
	}

	log.Printf("Received job %s: input %s (target codec: %s)", startPayload.JobID, startPayload.InputPath, startPayload.TargetCodec)

	// Register the job as cancellable before any gate runs so a cancel
	// arriving mid-gate aborts the start (issue #150).
	jobCtx, jobCancel := context.WithCancel(parentCtx)
	pauseFlag := &atomic.Bool{}
	s.cancels[startPayload.JobID] = jobCancel
	s.pauses[startPayload.JobID] = pauseFlag
	s.mu.Unlock()
	unregister := func() {
		jobCancel()
		s.unregisterJob(startPayload.JobID)
	}
	pathsLocked := false
	// abortIfCancelled stops the start when a cancel landed while the gates
	// ran: it releases the working-path lock when held, unregisters the
	// job, and reports the terminal outcome synchronously.
	abortIfCancelled := func() error {
		if jobCtx.Err() == nil {
			return nil
		}
		if pathsLocked {
			s.pathLocker.Unlock(startPayload.JobID)
		}
		log.Printf("Job %s cancelled during pre-spawn validation gates", startPayload.JobID)
		unregister()
		s.rejectJob(startPayload, "cancelled before transcode started")
		return fmt.Errorf("job %s cancelled before transcode started", startPayload.JobID)
	}

	log.Printf("Job %s: preflight gates starting", startPayload.JobID)

	// 0. Defer to an in-flight agent update: the updater swaps the running
	// binary, so a fresh job would not survive the restart.
	slog.Debug(fmt.Sprintf("Job %s: gate agent-update: entering", startPayload.JobID))
	if s.updating != nil && s.updating() {
		unregister()
		log.Printf("Job %s deferred: agent update is scheduled or in progress", startPayload.JobID)
		s.rejectJob(startPayload, "deferred: agent update is pending")
		return fmt.Errorf("job %s deferred: agent update is pending", startPayload.JobID)
	}
	slog.Debug(fmt.Sprintf("Job %s: gate agent-update: passed", startPayload.JobID))
	if err := abortIfCancelled(); err != nil {
		return err
	}

	// 0.5. Evaluate quiet-hour schedule window before filesystem, locking, or hardware preflight gates
	sched := startPayload.Schedule
	if sched == nil {
		sched = s.GetSchedule()
	}
	slog.Debug(fmt.Sprintf("Job %s: gate quiet-hours: evaluating schedule window", startPayload.JobID))
	if safety.IsQuietHoursActive(time.Now(), sched) {
		unregister()
		log.Printf("Job %s skipped: quiet hours currently active", startPayload.JobID)
		s.rejectJob(startPayload, "skipped: quiet hours active")
		return fmt.Errorf("job %s skipped: quiet hours active", startPayload.JobID)
	}
	slog.Debug(fmt.Sprintf("Job %s: gate quiet-hours: passed", startPayload.JobID))
	if err := abortIfCancelled(); err != nil {
		return err
	}

	// 1. Path confinement: every server-supplied path — whether carried in
	// InputPath or resolved from LocalPath/RelativePath — must land inside
	// the scanned library before the job is accepted. LocalPath/RelativePath
	// are resolved and stat'd statelessly first; the confinement check then
	// verifies scanned-library membership of the resolved path. This is the
	// single membership decision point: no branch may skip it.
	canonicalInput := startPayload.InputPath
	currentMediaDir := CleanLocalPath(s.GetMediaDir())
	rootPath := CleanLocalPath(startPayload.LocalPath)
	if rootPath == "" {
		rootPath = currentMediaDir
	}

	// Fast-fail if the library mount state is in an error state
	if mountErr := s.GetMountError(); mountErr != "" {
		unregister()
		reason := fmt.Sprintf("%s: %s", ErrLibraryNotMounted.Error(), mountErr)
		log.Printf("Job %s rejected fast: %s", startPayload.JobID, reason)
		s.rejectJob(startPayload, reason)
		return fmt.Errorf("job %s rejected: %s", startPayload.JobID, reason)
	}

	if startPayload.LocalPath != "" || startPayload.RelativePath != "" {
		slog.Debug(fmt.Sprintf("Job %s: gate input-validation: resolving and statting local input paths", startPayload.JobID))
		canonical, err := ValidateJobFile(rootPath, startPayload.RelativePath, startPayload.InputPath)
		if err != nil {
			unregister()
			log.Printf("Job %s rejected: %v", startPayload.JobID, err)
			reason := err.Error()
			if errors.Is(err, ErrLibraryNotMounted) {
				if rootPath != "" {
					if ok, dErr := ValidateLocalLibraryDirectory(rootPath); !ok && dErr != "" {
						reason = fmt.Sprintf("%s: %s", ErrLibraryNotMounted.Error(), dErr)
					} else {
						reason = ErrLibraryNotMounted.Error()
					}
				} else {
					reason = ErrLibraryNotMounted.Error()
				}
			} else if errors.Is(err, ErrFileNotFound) {
				reason = ErrFileNotFound.Error()
			}
			s.rejectJob(startPayload, reason)
			return fmt.Errorf("job %s rejected: %w", startPayload.JobID, err)
		}
		slog.Debug(fmt.Sprintf("Job %s: gate input-validation: passed", startPayload.JobID))
		slog.Debug(fmt.Sprintf("Job %s: gate confinement: verifying scanned-library membership", startPayload.JobID))
		canonical, err = s.confinement.ValidateJobInputPath(canonical)
		if err != nil && currentMediaDir != "" {
			if confined, cErr := isPathConfined(currentMediaDir, canonical); cErr == nil && confined {
				err = nil
			}
		}
		if err != nil {
			unregister()
			log.Printf("Job %s rejected: %v", startPayload.JobID, err)
			s.rejectJob(startPayload, err.Error())
			return fmt.Errorf("job %s rejected: %w", startPayload.JobID, err)
		}
		slog.Debug(fmt.Sprintf("Job %s: gate confinement: passed", startPayload.JobID))
		canonicalInput = canonical
	} else {
		slog.Debug(fmt.Sprintf("Job %s: gate input-validation: using server-supplied input path", startPayload.JobID))
		slog.Debug(fmt.Sprintf("Job %s: gate confinement: verifying scanned-library membership", startPayload.JobID))
		canonical, err := s.confinement.ValidateJobInputPath(startPayload.InputPath)
		if err != nil && currentMediaDir != "" {
			if c, cErr := transcoder.CanonicalMediaPath(startPayload.InputPath); cErr == nil {
				if confined, cErr := isPathConfined(currentMediaDir, c); cErr == nil && confined {
					err = nil
					canonical = c
				}
			}
		}
		if err != nil {
			unregister()
			log.Printf("Job %s rejected: %v", startPayload.JobID, err)
			s.rejectJob(startPayload, err.Error())
			return fmt.Errorf("job %s rejected: %w", startPayload.JobID, err)
		}
		slog.Debug(fmt.Sprintf("Job %s: gate confinement: passed", startPayload.JobID))
		canonicalInput = canonical
	}
	if err := abortIfCancelled(); err != nil {
		return err
	}

	// 1.5. Custom flags validation: reject bare non-flag tokens, forbidden
	// parameters, and multiple output targets upfront before process launch.
	slog.Debug(fmt.Sprintf("Job %s: gate custom-flags: validating custom flags", startPayload.JobID))
	if startPayload.CustomFlags != "" {
		if _, err := protocol.SanitizeCustomFlags(startPayload.CustomFlags); err != nil {
			unregister()
			log.Printf("Job %s rejected: %v", startPayload.JobID, err)
			s.rejectJob(startPayload, fmt.Sprintf("invalid custom flags: %v", err))
			return fmt.Errorf("job %s rejected: invalid custom flags: %w", startPayload.JobID, err)
		}
	}
	slog.Debug(fmt.Sprintf("Job %s: gate custom-flags: passed", startPayload.JobID))
	if err := abortIfCancelled(); err != nil {
		return err
	}

	// 1.75. Active working path locks: guard against concurrent transcode supervisors
	// or concurrent jobs running over the same source file, scratch folder, or quarantine destination.
	slog.Debug(fmt.Sprintf("Job %s: gate working-paths: deriving and locking working paths", startPayload.JobID))
	workingPaths := s.deriveWorkingPaths(canonicalInput)
	if err := s.pathLocker.TryLock(startPayload.JobID, workingPaths); err != nil {
		unregister()
		log.Printf("Job %s rejected: %v", startPayload.JobID, err)
		s.rejectJob(startPayload, fmt.Sprintf("working path locked: %v", err))
		return fmt.Errorf("job %s rejected: working path locked: %w", startPayload.JobID, err)
	}
	pathsLocked = true
	slog.Debug(fmt.Sprintf("Job %s: gate working-paths: passed", startPayload.JobID))
	if err := abortIfCancelled(); err != nil {
		return err
	}

	// 1.8. Host safety check: ensure sufficient available memory (>2GB)
	slog.Debug(fmt.Sprintf("Job %s: gate memory-preflight: checking available memory", startPayload.JobID))
	if err := hardware.CheckAvailableMemory(); err != nil {
		s.pathLocker.Unlock(startPayload.JobID)
		unregister()
		log.Printf("Job %s rejected: %v", startPayload.JobID, err)
		s.rejectJob(startPayload, err.Error())
		return fmt.Errorf("job %s rejected: %w", startPayload.JobID, err)
	}
	slog.Debug(fmt.Sprintf("Job %s: gate memory-preflight: passed", startPayload.JobID))
	if err := abortIfCancelled(); err != nil {
		return err
	}

	// 3. Check local media server streaming conflicts (Plex, Jellyfin, Emby)
	if s.safety != nil {
		slog.Debug(fmt.Sprintf("Job %s: gate stream-safety: checking media-server streaming conflicts", startPayload.JobID))
		conflict, serverName, err := s.safety.CheckConflict(jobCtx, canonicalInput)
		if err != nil {
			if jobCtx.Err() != nil {
				// The check aborted because the job was cancelled
				// mid-gate; abortIfCancelled performs the unlock,
				// unregistration, and outcome reporting.
				return abortIfCancelled()
			}
			s.pathLocker.Unlock(startPayload.JobID)
			unregister()
			log.Printf("Job %s failed: stream-safety check failed on %s: %v", startPayload.JobID, serverName, err)
			s.rejectJob(startPayload, fmt.Sprintf("stream-safety check failed on %s: %v", serverName, err))
			return fmt.Errorf("job %s: stream-safety check failed on %s: %v", startPayload.JobID, serverName, err)
		}
		if conflict {
			s.pathLocker.Unlock(startPayload.JobID)
			unregister()
			log.Printf("Job %s deferred: active streaming conflict on %s", startPayload.JobID, serverName)
			s.rejectJob(startPayload, fmt.Sprintf("deferred: active streaming conflict on %s", serverName))
			return fmt.Errorf("job %s deferred: active streaming conflict on %s", startPayload.JobID, serverName)
		}
		slog.Debug(fmt.Sprintf("Job %s: gate stream-safety: passed", startPayload.JobID))
		if err := abortIfCancelled(); err != nil {
			return err
		}
	}

	log.Printf("Job %s: all preflight gates passed", startPayload.JobID)

	if startPayload.Schedule != nil {
		s.scheduleMu.Lock()
		s.currentSchedule = startPayload.Schedule
		s.scheduleMu.Unlock()
	}

	s.reportStatus("WORKING", "")
	s.wg.Add(1)
	s.count.Add(1)

	go s.runJob(jobCtx, startPayload, canonicalInput, rawPayload, pauseFlag)
	return nil
}

// PauseJob requests that the active job halt. On the chunked pipeline
// (software encoders) the pause takes effect at the next chunk boundary,
// reported via OnPause. On a full-stream single-pass encode (hardware
// encoders) there is no chunk boundary: the running pass is cancelled and
// requeued — restarted from the beginning once the pause lifts. The pause
// takes effect when the runner consults IsPaused and reports PAUSED via
// OnPause.
func (s *JobSupervisor) PauseJob(jobID string) {
	s.mu.Lock()
	p, ok := s.pauses[jobID]
	s.mu.Unlock()
	if !ok {
		return
	}
	p.Store(true)
	s.reportStatus("WORKING", "pausing")
	log.Printf("Pause requested for active job %s", jobID)
}

// ResumeJob clears the pause flag so the runner proceeds — at the next chunk
// boundary on the chunked pipeline, or by restarting the cancelled pass on a
// single-stream encode — and reports WORKING again via OnPause.
func (s *JobSupervisor) ResumeJob(jobID string) {
	s.mu.Lock()
	p, ok := s.pauses[jobID]
	s.mu.Unlock()
	if !ok {
		return
	}
	p.Store(false)
	s.reportStatus("WORKING", "resuming")
	log.Printf("Resumed active job %s", jobID)
}

// CancelJob cancels the in-flight job's context, if the job is still
// running, and cleans up scratch files of a job that already terminated
// locally (a late or offline CANCEL_JOB) via the journal lookup.
func (s *JobSupervisor) CancelJob(ctx context.Context, jobID string) {
	s.mu.Lock()
	cancel, ok := s.cancels[jobID]
	s.mu.Unlock()
	if ok {
		cancel()
		log.Printf("Cancelled active job %s", jobID)
	}

	if s.journal != nil {
		if journal, err := s.journal.GetJobJournal(ctx, jobID); err == nil {
			s.runner.DiscardJobFiles(ctx, journal.FilePath, jobID)
			log.Printf("Cleaned up offline/late job files for job %s", jobID)
		}
	}
}

// GetSchedule returns the supervisor's active quiet-hours schedule.
func (s *JobSupervisor) GetSchedule() *protocol.ScheduleConfig {
	s.scheduleMu.RLock()
	defer s.scheduleMu.RUnlock()
	return s.currentSchedule
}

// UpdateSchedule updates the supervisor's quiet-hours schedule and signals
// the active transcode to pause after the current chunk if quiet hours are active.
func (s *JobSupervisor) UpdateSchedule(sched *protocol.ScheduleConfig) {
	s.scheduleMu.Lock()
	s.currentSchedule = sched
	s.scheduleMu.Unlock()

	if sched != nil && sched.IsQuietHoursActive(time.Now()) && s.ActiveCount() > 0 {
		s.reportStatus("WORKING", "pausing")
		log.Printf("Quiet hours enabled during active transcode (pausing)")
	} else if (sched == nil || !sched.IsQuietHoursActive(time.Now())) && s.ActiveCount() > 0 {
		s.mu.Lock()
		anyPaused := false
		for _, p := range s.pauses {
			if p.Load() {
				anyPaused = true
				break
			}
		}
		s.mu.Unlock()
		if !anyPaused {
			s.reportStatus("WORKING", "")
		}
	}
}

// SetTelemetryRate updates the progress emission cadence based on the
// streaming mode and optional rate override.
func (s *JobSupervisor) SetTelemetryRate(p protocol.SetTelemetryRatePayload) {
	interval := time.Second
	if p.RateMS > 0 {
		interval = time.Duration(p.RateMS) * time.Millisecond
	} else {
		switch p.Mode {
		case protocol.TelemetryModeLive:
			interval = 1 * time.Second
		case protocol.TelemetryModeBackground:
			interval = 15 * time.Second
		case protocol.TelemetryModeIdle:
			interval = 30 * time.Second
		}
	}
	s.rateMu.Lock()
	s.telemetryInterval = interval
	s.rateMu.Unlock()
	log.Printf("Dynamic telemetry throttler: updated mode %s (interval: %v)", p.Mode, interval)
}

// TelemetryInterval returns the current progress telemetry interval.
func (s *JobSupervisor) TelemetryInterval() time.Duration {
	s.rateMu.RLock()
	defer s.rateMu.RUnlock()
	return s.telemetryInterval
}

// ActiveCount reports how many jobs are currently in flight.
func (s *JobSupervisor) ActiveCount() int64 {
	return s.count.Load()
}

// Concurrency returns the configured maximum concurrent transcode jobs.
func (s *JobSupervisor) Concurrency() int {
	c := int(s.concurrency.Load())
	if c <= 0 || c > maxConcurrentTranscodes {
		return maxConcurrentTranscodes
	}
	return c
}

// SetConcurrency updates the supervisor's active concurrency limit.
func (s *JobSupervisor) SetConcurrency(n int) {
	if n < 1 {
		n = 1
	}
	if n > maxConcurrentTranscodes {
		n = maxConcurrentTranscodes
	}
	s.concurrency.Store(int32(n))
}

// Wait blocks until every in-flight job has finished. Daemon shutdown runs
// it under a deadline; when the deadline expires, CancelAll unwinds the
// remainder through each job's normal cancellation path.
func (s *JobSupervisor) Wait() {
	s.wg.Wait()
}

// CancelAll cancels every in-flight job context. Used by daemon shutdown
// when the drain deadline expires.
func (s *JobSupervisor) CancelAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for jobID, cancel := range s.cancels {
		cancel()
		slog.Info("Cancelled in-flight job due to incomplete drain", "job_id", jobID)
	}
}

// runJob is the per-job goroutine: it builds the transcoder job and hooks,
// bridges progress to telemetry envelopes, and reports the terminal outcome.
func (s *JobSupervisor) runJob(jobCtx context.Context, startPayload protocol.StartJobPayload, canonicalInput string, rawPayload json.RawMessage, pauseFlag *atomic.Bool) {
	defer s.jobFinished()

	job := &transcoder.Job{
		ID:        startPayload.JobID,
		FilePath:  canonicalInput,
		State:     models.StateQueued,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Payload:   rawPayload,
	}

	targetCodec := startPayload.TargetCodec
	if targetCodec == "" {
		targetCodec = "hevc"
	}

	// The spec is built once from the wire payload and passed down to the
	// runner unchanged; the orchestrator and runner derive their
	// execution-only fields from it instead of re-copying tuning fields.
	spec, err := jobspec.FromWire(startPayload)
	if err != nil {
		log.Printf("Job %s rejected: invalid job spec: %v", startPayload.JobID, err)
		s.reportStatus("IDLE", "")
		s.pathLocker.Unlock(startPayload.JobID)
		s.unregisterJob(startPayload.JobID)
		return
	}
	hooks := transcoder.JobHooks{
		Schedule:    startPayload.Schedule,
		GetSchedule: s.GetSchedule,
	}

	var lastProg protocol.LiveTelemetry
	var lastProgMu sync.Mutex

	hooks.OnPause = func(paused bool) {
		if paused {
			reason := "User"
			sched := s.GetSchedule()
			if sched == nil {
				sched = startPayload.Schedule
			}
			if sched != nil && safety.IsQuietHoursActive(time.Now(), sched) {
				reason = "Quiet Hours"
			}
			s.reportStatus("PAUSED", reason)
			log.Printf("Job %s paused (%s)", startPayload.JobID, reason)
			lastProgMu.Lock()
			if lastProg.JobID != "" {
				lastProg.Activity = reason
				b, _ := json.Marshal(lastProg)
				s.sendEnv(protocol.Envelope{
					Type:    protocol.TypeJobProgress,
					ID:      fmt.Sprintf("%s-paused-%d", startPayload.JobID, time.Now().UnixNano()),
					TS:      time.Now(),
					Payload: b,
				})
			}
			lastProgMu.Unlock()
		} else {
			s.reportStatus("WORKING", "")
			log.Printf("Job %s resumed", startPayload.JobID)
		}
	}

	hooks.IsPaused = func() bool {
		return pauseFlag.Load()
	}

	defer func() {
		s.unregisterJob(startPayload.JobID)
		s.pathLocker.Unlock(startPayload.JobID)
	}()

	var lastProgressSent time.Time
	var lastPhase string
	var throttleMu sync.Mutex

	result, err := s.runner.ProcessJobWithSpec(jobCtx, job, targetCodec, spec, hooks, func(p ffmpeg.Progress) {
		progPayload := protocol.LiveTelemetry{
			JobID:            startPayload.JobID,
			Phase:            p.Phase,
			CurrentIndex:     p.CurrentIndex,
			TotalCount:       p.TotalCount,
			ElapsedSeconds:   p.ElapsedSecs,
			FPS:              p.FPS,
			ETASeconds:       p.ETASeconds,
			Bitrate:          p.BitrateKbps,
			ProgressPct:      p.ProgressPct,
			CurrentFrame:     p.Frame,
			TotalFrames:      p.TotalFrames,
			SourceCodec:      p.SourceCodec,
			TargetCodec:      p.TargetCodec,
			Speed:            p.Speed,
			InputPath:        protocol.SanitizeTelemetryPath(startPayload.InputPath, startPayload.RelativePath),
			SubPhase:         p.SubPhase,
			Activity:         p.Activity,
			TransferredBytes: p.TransferredBytes,
			TotalBytes:       p.TotalBytes,
		}
		if p.Phase == "verifying" {
			progPayload.Activity = "verifying"
		}
		lastProgMu.Lock()
		lastProg = progPayload
		lastProgMu.Unlock()

		throttleMu.Lock()
		now := time.Now()
		interval := s.TelemetryInterval()
		phaseChanged := p.Phase != lastPhase
		shouldSend := lastProgressSent.IsZero() || phaseChanged || now.Sub(lastProgressSent) >= interval
		if shouldSend {
			lastProgressSent = now
			lastPhase = p.Phase
		}
		throttleMu.Unlock()

		if shouldSend {
			b, _ := json.Marshal(progPayload)
			s.sendEnv(protocol.Envelope{
				Type:    protocol.TypeJobProgress,
				ID:      fmt.Sprintf("%s-%d", startPayload.JobID, time.Now().UnixNano()),
				TS:      time.Now(),
				Payload: b,
			})
		}
		if p.Phase == "encode" || p.Phase == "" {
			s.emitEvent(Event{
				Type:        EventJobProgress,
				JobID:       startPayload.JobID,
				InputPath:   startPayload.InputPath,
				Phase:       p.Phase,
				ProgressPct: p.ProgressPct,
				FPS:         p.FPS,
				Speed:       p.Speed,
			})
		} else if p.Phase == "verifying" {
			s.reportStatus("WORKING", "verifying")
			s.emitEvent(Event{
				Type:        EventJobProgress,
				JobID:       startPayload.JobID,
				InputPath:   startPayload.InputPath,
				Phase:       "verifying",
				ProgressPct: p.ProgressPct,
			})
		} else if p.Phase == "replacing" || p.Phase == "placement" {
			activity := p.Activity
			if activity == "" {
				activity = "placing output"
			}
			s.reportStatus("WORKING", activity)
			s.emitEvent(Event{
				Type:             EventJobProgress,
				JobID:            startPayload.JobID,
				InputPath:        startPayload.InputPath,
				Phase:            p.Phase,
				SubPhase:         p.SubPhase,
				Activity:         activity,
				ProgressPct:      p.ProgressPct,
				TransferredBytes: p.TransferredBytes,
				TotalBytes:       p.TotalBytes,
			})
		}
	})

	sanitizedInput := protocol.SanitizeTelemetryPath(startPayload.InputPath, startPayload.RelativePath)
	scrubber := protocol.PathScrubber{
		MediaDir:      s.GetMediaDir(),
		ScratchDir:    s.scratchDir,
		QuarantineDir: s.quarantineDir,
		InputPath:     startPayload.InputPath,
		RelativePath:  startPayload.RelativePath,
	}

	completePayload := protocol.FileCompletePayload{
		JobID:       startPayload.JobID,
		MediaItemID: startPayload.MediaItemID,
		InputPath:   sanitizedInput,
		Success:     err == nil,
	}
	if result != nil {
		completePayload.OriginalSize = result.OriginalSize
		completePayload.NewSize = result.NewSize
		completePayload.SavingsBytes = result.SavingsBytes
		completePayload.DurationSecs = result.DurationSecs
		completePayload.OriginalHash = result.OriginalHash
		completePayload.NewHash = result.NewHash
		completePayload.FfmpegCommands = protocol.ScrubFfmpegCommands(result.Commands, scrubber)
		completePayload.VMAFScore = result.VMAFScore
	}
	// A job whose context was cancelled (CANCEL_JOB, lease revocation,
	// shutdown) ends as a normal cancellation, not a failure: report a short
	// message instead of the runner's error text, which may embed process
	// stderr. The completion envelope still reports Success=false so the
	// server's cancel drain (issue #118) is unchanged.
	cancelled := err != nil && jobCtx.Err() != nil
	if err != nil {
		if cancelled {
			completePayload.ErrorMessage = "job cancelled"
			log.Printf("Job %s cancelled", startPayload.JobID)
		} else {
			completePayload.ErrorMessage = protocol.SanitizeFileCompleteError(err.Error(), scrubber)
			log.Printf("Job %s failed: %v", startPayload.JobID, err)
		}
	} else {
		log.Printf("Job %s completed successfully! Original: %d, New: %d, Savings: %d",
			startPayload.JobID, completePayload.OriginalSize, completePayload.NewSize, completePayload.SavingsBytes)
	}

	b, _ := json.Marshal(completePayload)
	s.sendEnv(protocol.Envelope{
		Type:    protocol.TypeFileComplete,
		ID:      fmt.Sprintf("complete-%s", startPayload.JobID),
		TS:      time.Now(),
		Payload: b,
	})
	s.emitEvent(Event{
		Type:         EventJobComplete,
		JobID:        startPayload.JobID,
		InputPath:    startPayload.InputPath,
		Success:      err == nil,
		Cancelled:    cancelled,
		Message:      completePayload.ErrorMessage,
		OriginalSize: completePayload.OriginalSize,
		NewSize:      completePayload.NewSize,
		SavingsBytes: completePayload.SavingsBytes,
	})
	if s.onJobComplete != nil {
		s.onJobComplete(startPayload.JobID, err == nil)
	}
}

// unregisterJob removes a job's cancel/pause registrations after a terminal
// outcome: a gate rejection, a cancel that landed during the gates, or the
// run goroutine's cleanup.
func (s *JobSupervisor) unregisterJob(jobID string) {
	s.mu.Lock()
	delete(s.cancels, jobID)
	delete(s.pauses, jobID)
	s.mu.Unlock()
}

// jobFinished marks one transcode job as complete: it releases the drain
// WaitGroup, decrements the active-job count, and reports IDLE only when
// the last concurrent job finishes — an early-finishing sibling must not
// flip the agent to IDLE while others are still running.
func (s *JobSupervisor) jobFinished() {
	if s.count.Add(-1) == 0 {
		s.reportStatus("IDLE", "")
	} else {
		s.reportStatus("WORKING", "")
	}
	s.wg.Done()
}

// rejectJob refuses a job before it starts: it reports the failure to the
// server and mirrors the terminal outcome onto the event stream.
func (s *JobSupervisor) rejectJob(startPayload protocol.StartJobPayload, reason string) {
	var rejectionReason protocol.RejectionReason
	if strings.Contains(reason, "ErrFileNotFound") {
		rejectionReason = protocol.RejectionFileNotFound
	} else if strings.Contains(reason, "ErrLibraryNotMounted") {
		rejectionReason = protocol.RejectionLibraryNotMounted
	}
	sanitizedInput := protocol.SanitizeTelemetryPath(startPayload.InputPath, startPayload.RelativePath)
	scrubber := protocol.PathScrubber{
		MediaDir:      s.GetMediaDir(),
		ScratchDir:    s.scratchDir,
		QuarantineDir: s.quarantineDir,
		InputPath:     startPayload.InputPath,
		RelativePath:  startPayload.RelativePath,
	}
	sanitizedReason := protocol.SanitizeFileCompleteError(reason, scrubber)
	completePayload := protocol.FileCompletePayload{
		JobID:           startPayload.JobID,
		MediaItemID:     startPayload.MediaItemID,
		InputPath:       sanitizedInput,
		Success:         false,
		ErrorMessage:    sanitizedReason,
		RejectionReason: rejectionReason,
	}
	b, _ := json.Marshal(completePayload)
	s.sendEnv(protocol.Envelope{
		Type:    protocol.TypeFileComplete,
		ID:      fmt.Sprintf("complete-%s", startPayload.JobID),
		TS:      time.Now(),
		Payload: b,
	})
	s.emitEvent(Event{Type: EventError, JobID: startPayload.JobID, InputPath: startPayload.InputPath, Message: sanitizedReason})
	s.emitEvent(Event{Type: EventJobComplete, JobID: startPayload.JobID, InputPath: startPayload.InputPath, Success: false, Message: sanitizedReason})
	if s.onJobComplete != nil {
		s.onJobComplete(startPayload.JobID, false)
	}
}

func (s *JobSupervisor) emitEvent(ev Event) {
	if s.emit != nil {
		s.emit(ev)
	}
}

func (s *JobSupervisor) sendEnv(env protocol.Envelope) {
	if s.send != nil {
		s.send(env)
	}
}

func (s *JobSupervisor) reportStatus(status, activity string) {
	if s.setStatus != nil {
		s.setStatus(status, activity)
	}
}

// deriveWorkingPaths computes the canonical paths that must be locked for a job:
// the canonical media source, the scratch output file, the segment directory,
// and the quarantine destination (if configured).
func (s *JobSupervisor) deriveWorkingPaths(canonicalInput string) []string {
	resolvedInput := canonicalWorkingPath(canonicalInput)
	paths := jobpaths.New(s.scratchDir, s.quarantineDir, resolvedInput)
	working := []string{
		resolvedInput,
		canonicalWorkingPath(paths.Scratch()),
		canonicalWorkingPath(paths.SegmentDir()),
	}
	if s.quarantineDir != "" || paths.Quarantine() != "" {
		if q := paths.Quarantine(); q != "" {
			working = append(working, canonicalWorkingPath(q))
		}
	}
	return working
}

// canonicalWorkingPath resolves a filesystem path to its canonical absolute representation.
// For existing files/directories, symlinks are evaluated. For non-existent target paths
// (e.g. pending scratch files or quarantine locations), the deepest existing ancestor directory
// is resolved to preserve symlink canonicalization across the directory tree.
func canonicalWorkingPath(p string) string {
	if p == "" {
		return ""
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = filepath.Clean(p)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(resolved)
	}
	cur := abs
	var trailing []string
	for {
		parent := filepath.Dir(cur)
		base := filepath.Base(cur)
		if parent == cur {
			trailing = append([]string{cur}, trailing...)
			return filepath.Clean(filepath.Join(trailing...))
		}
		trailing = append([]string{base}, trailing...)
		if resolvedParent, err := filepath.EvalSymlinks(parent); err == nil {
			parts := append([]string{resolvedParent}, trailing...)
			return filepath.Clean(filepath.Join(parts...))
		}
		cur = parent
	}
}
