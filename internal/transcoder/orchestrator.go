package transcoder

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/will25u/slimmedia-agent/internal/db"
	"github.com/will25u/slimmedia-agent/internal/ffmpeg"
	"github.com/will25u/slimmedia-agent/internal/hardware"
	"github.com/will25u/slimmedia-agent/internal/jobpaths"
	"github.com/will25u/slimmedia-agent/internal/jobspec"
	"github.com/will25u/slimmedia-agent/internal/scanner"
	"github.com/will25u/slimmedia-agent/internal/models"
	"github.com/will25u/slimmedia-agent/internal/protocol"
)

type Orchestrator struct {
	SM       *StateMachine
	Pipeline *Pipeline
	// FFmpegPath is the path of the ffmpeg binary; ffprobe is expected next
	// to it.
	FFmpegPath string
	// Library, when non-nil, confines jobs to canonical paths the local
	// scanner discovered. Jobs for any other path are rejected before any
	// filesystem mutation.
	Library ScannedLibrary
	// EncoderCaps are the probed encoder capabilities used to honor preset
	// encoder-engine requests with hardware fallback.
	Caps ffmpeg.EncoderCaps
	// CPUTopo is the machine's detected core/die layout (issue #201); the
	// zero value means "no steering". Software encode jobs derive their
	// ThreadSteering from it per resolved encoder.
	CPUTopo hardware.CPUTopology
	// Queries provides access to the agent's SQLite database.
	Queries *db.Queries
	// Runner executes the transcode; defaults to ffmpeg.RunTranscode.
	// Replaceable in tests.
	Runner func(ctx context.Context, job ffmpeg.TranscodeJob) error
	// OnTrace, when non-nil, receives the ffmpeg command trace each time a
	// new phase record appears (phase-start time), before the encode
	// finishes. The daemon bridges it to a transcode.trace envelope so the
	// exact argv is persisted even for jobs that never complete.
	OnTrace func(jobID string, trace []protocol.FfmpegCommandRecord)
}

func NewOrchestrator(sm *StateMachine, pipeline *Pipeline, ffmpegPath string) *Orchestrator {
	return &Orchestrator{
		SM:         sm,
		Pipeline:   pipeline,
		FFmpegPath: ffmpegPath,
		Runner:     ffmpeg.RunTranscode,
	}
}

func (o *Orchestrator) runner() func(ctx context.Context, job ffmpeg.TranscodeJob) error {
	if o.Runner != nil {
		return o.Runner
	}
	return ffmpeg.RunTranscode
}

// steeringFor resolves the topology-driven thread steering for one resolved
// encoder; nil means "leave the encode untouched" (symmetric CPUs, hardware
// encoders, undetected topology all land here).
func steeringFor(topo hardware.CPUTopology, encoder string) *ffmpeg.ThreadSteering {
	s := ffmpeg.SteeringFor(topo, encoder)
	if !s.Enabled {
		return nil
	}
	return &s
}

// resolveEncoder mirrors NewCommandBuilderFromJob's encoder resolution so
// steering keys off the same encoder the builder will emit (e.g. "libx265",
// not the "hevc" family string) — without this, steering would silently
// never engage on daemon-dispatched jobs.
func (o *Orchestrator) resolveEncoder(spec jobspec.JobSpec, targetCodec string) string {
	if spec.EncoderEngine != "" {
		return ffmpeg.ResolveEncoder(spec.EncoderEngine, targetCodec, o.Caps)
	}
	return targetCodec
}

type JobResult struct {
	OriginalSize int64
	NewSize      int64
	SavingsBytes int64
	DurationSecs float64
	OriginalHash string
	NewHash      string
	// Commands is the exact ffmpeg argv trace of the job's encode phases,
	// collected by the runner and reported back for post-hoc inspection.
	Commands []protocol.FfmpegCommandRecord
	// VMAFScore is the opt-in spot-check score (nil when not computed).
	VMAFScore *float64
}

type ProgressCallback func(p ffmpeg.Progress)

// JobHooks carries the daemon-attached execution controls for one job:
// the quiet-hours schedule and the pause/resume callbacks. These are
// environment plumbing, not preset tuning, so they travel separately from
// the jobspec.JobSpec.
type JobHooks struct {
	Schedule    *protocol.ScheduleConfig
	GetSchedule func() *protocol.ScheduleConfig
	OnPause     func(paused bool)
	IsPaused    func() bool
}

func (o *Orchestrator) ProcessJob(ctx context.Context, job *Job, targetCodec string) error {
	_, err := o.ProcessJobWithProgress(ctx, job, targetCodec, nil)
	return err
}

func (o *Orchestrator) ProcessJobWithProgress(ctx context.Context, job *Job, targetCodec string, onProgress ProgressCallback) (*JobResult, error) {
	return o.ProcessJobWithSpec(ctx, job, targetCodec, jobspec.JobSpec{}, JobHooks{}, onProgress)
}

func (o *Orchestrator) ProcessJobWithSpec(ctx context.Context, job *Job, targetCodec string, spec jobspec.JobSpec, hooks JobHooks, onProgress ProgressCallback) (*JobResult, error) {
	if job.State == "" {
		job.State = models.StateQueued
	}

	// 0. Path confinement: resolve the server-supplied path and verify the
	// local scanner discovered it. This runs before any filesystem mutation.
	canonicalPath, err := CanonicalMediaPath(job.FilePath)
	if err != nil {
		o.SM.Transition(job, models.StateSkipped)
		return nil, fmt.Errorf("job input rejected: %w", err)
	}
	if o.Library != nil {
		allowed, err := o.Library.Contains(canonicalPath)
		if err != nil {
			o.SM.Transition(job, models.StateSkipped)
			return nil, fmt.Errorf("cannot verify job input against scanned library: %w", err)
		}
		if !allowed {
			o.SM.Transition(job, models.StateSkipped)
			return nil, fmt.Errorf("job input rejected: %q is outside the scanned media library", canonicalPath)
		}
	}
	job.FilePath = canonicalPath

	// 0.5. Validate custom flags: reject bare tokens and forbidden options before process launch or preparing state.
	if spec.CustomFlags != "" {
		if _, err := ffmpeg.SanitizeCustomFlags(spec.CustomFlags); err != nil {
			o.SM.Transition(job, models.StateSkipped)
			return nil, fmt.Errorf("job input rejected: invalid custom flags: %w", err)
		}
	}

	// Prepare paths. JobPaths owns the entire per-job layout (scratch file,
	// segment directory, quarantine destination); every stage below consumes
	// it instead of re-deriving names.
	paths := jobpaths.New(o.Pipeline.ScratchDir, o.Pipeline.QuarantineDir, canonicalPath)
	destPath, err := paths.DestinationPath(spec.OutputFileStrategy, spec.Container)
	if err != nil {
		o.SM.Transition(job, models.StateSkipped)
		return nil, fmt.Errorf("job input rejected: invalid destination path: %w", err)
	}

	// The pipeline shape is decided once, up front, from the resolved encoder
	// (or a spec override): software encoders run the chunked path; hardware
	// encoders run one full-stream pass and never touch the segment
	// directory.
	chunked := ffmpeg.UseChunkedPipeline(spec, targetCodec, &o.Caps)

	// 1. Check budget before starting across both scratch volume and destination volume
	if err := o.Pipeline.CheckBudget(filepath.Dir(destPath)); err != nil {
		o.SM.Transition(job, models.StateSkipped)
		return nil, fmt.Errorf("skipping job, budget or disk space critical: %w", err)
	}

	// 1.5. Loop prevention: check if already completed
	if o.Queries != nil {
		rec, err := o.Queries.GetCompletedTranscode(ctx, job.FilePath)
		if err == nil {
			origHash, err := scanner.Fingerprint(job.FilePath)
			if err == nil && rec.FileHash == origHash {
				o.SM.Transition(job, models.StateSkipped)
				return nil, fmt.Errorf("skipping job: file hash matches previously completed transcode")
			}
		} else if err != sql.ErrNoRows {
			// Log but don't fail the job if DB check fails?
			// The safest is to proceed.
		}
	}

	// Move to PREPARING
	if err := o.SM.Transition(job, models.StatePreparing); err != nil {
		return nil, err
	}

	scratchPath := paths.Scratch()
	segDir := paths.SegmentDir()
	if chunked {
		_ = os.MkdirAll(segDir, 0755)
	}
	if len(job.Payload) > 0 {
		_ = writeJournalAtomic(paths.JournalFile(), job.Payload)
	}

	// Probe original (we'd pass the actual duration)
	origInfo, err := ffmpeg.ProbeMedia(filepath.Join(filepath.Dir(o.FFmpegPath), "ffprobe"), job.FilePath)
	if err != nil {
		_ = os.RemoveAll(segDir)
		o.failJob(job, fmt.Sprintf("ffprobe failed: %v", err), paths)
		return nil, err
	}
	origHash, err := scanner.Fingerprint(job.FilePath)
	if err != nil {
		_ = os.RemoveAll(segDir)
		o.failJob(job, fmt.Sprintf("fingerprint failed: %v", err), paths)
		return nil, err
	}

	// Move to TRANSCODING
	if err := o.SM.Transition(job, models.StateTranscoding); err != nil {
		return nil, err
	}

	progressChan := make(chan ffmpeg.Progress, 100)

	// Command trace: the runner records the exact ffmpeg argv once per
	// encode phase; reported on the completion envelope after the job
	// finishes. In-memory only, so the resume engine (ADR-010) is
	// unaffected — a recovered job re-runs its phases and rebuilds it.
	var cmdTrace []protocol.FfmpegCommandRecord

	// Launch FFmpeg with transcode context so budget exhaustion can cleanly abort
	transcodeCtx, cancelTranscode := context.WithCancel(ctx)
	defer cancelTranscode()

	var (
		budgetMu  sync.Mutex
		budgetErr error
	)
	checkBudgetPeriodic := func() {
		if bErr := o.Pipeline.CheckBudget(filepath.Dir(destPath)); bErr != nil {
			budgetMu.Lock()
			if budgetErr == nil {
				budgetErr = bErr
			}
			budgetMu.Unlock()
			cancelTranscode()
		}
	}

	budgetTicker := time.NewTicker(3 * time.Second)
	defer budgetTicker.Stop()

	errChan := make(chan error, 1)
	go func() {
		errChan <- o.runner()(transcodeCtx, ffmpeg.TranscodeJob{
			JobID:        job.ID,
			Spec:         spec,
			FFmpegPath:   o.FFmpegPath,
			InputFile:    job.FilePath,
			OutputFile:   scratchPath,
			Codec:        targetCodec,
			SourceCodec:  origInfo.Codec,
			DurationSec:      origInfo.Duration,
			TotalFrames:      origInfo.TotalFrames,
			ExactFramesKnown: origInfo.ExactFramesKnown,
			ProgressChan:     progressChan,
			Caps:         &o.Caps,
			Steering:     steeringFor(o.CPUTopo, o.resolveEncoder(spec, targetCodec)),
			Schedule:     hooks.Schedule,
			GetSchedule:  hooks.GetSchedule,
			OnPause:      hooks.OnPause,
			IsPaused:     hooks.IsPaused,
			Paths:        &paths,
			Trace:        &cmdTrace,
			OnTrace: func(trace []protocol.FfmpegCommandRecord) {
				if o.OnTrace != nil && job.ID != "" {
					o.OnTrace(job.ID, append([]protocol.FfmpegCommandRecord{}, trace...))
				}
			},
		})
	}()

	// Consume progress until the runner closes the channel. RunTranscode
	// guarantees closure even when the binary fails to start; the ctx case
	// additionally ensures a wedged runner cannot block daemon shutdown.
wait:
	for {
		select {
		case prog, ok := <-progressChan:
			if !ok {
				break wait
			}
			if onProgress != nil {
				onProgress(prog)
			}
			checkBudgetPeriodic()
		case <-budgetTicker.C:
			checkBudgetPeriodic()
		case <-ctx.Done():
			break wait
		case <-transcodeCtx.Done():
			break wait
		}
	}

	// Await the runner process termination so active FFmpeg processes exit
	// before any temporary scratch or segment directories are removed.
	runErr := <-errChan

	budgetMu.Lock()
	bErr := budgetErr
	budgetMu.Unlock()

	if bErr != nil {
		o.failJob(job, fmt.Sprintf("budget or disk space critical: %v", bErr), paths)
		return nil, bErr
	}

	if runErr != nil || ctx.Err() != nil {
		if ctx.Err() != nil {
			// A user/server cancellation is a normal outcome, not a failure:
			// keep the message short and free of process-output noise (runner
			// errors can embed ffmpeg stderr), both for the job record and
			// for the surfaces (tray UI) that display it.
			o.failJobStopped(job, "job cancelled", paths)
			if runErr == nil {
				runErr = ctx.Err()
			}
		} else {
			o.failJob(job, fmt.Sprintf("ffmpeg failed: %v", runErr), paths)
		}
		return nil, runErr
	}

	// Move to VERIFYING
	if err := o.SM.Transition(job, models.StateVerifying); err != nil {
		return nil, err
	}
	log.Printf("Job %s: phase verify: verifying output size, decodability, duration, and a/v alignment", job.ID)
	if onProgress != nil {
		onProgress(ffmpeg.Progress{
			Phase:       "verifying",
			ProgressPct: 100.0,
		})
	}

	if err := o.Pipeline.VerifySize(job.FilePath, scratchPath); err != nil {
		o.failVerification(ctx, job, fmt.Sprintf("verification failed: %v", err), paths)
		return nil, err
	}

	// A same-size but corrupted transcode output must never replace the
	// original: the size gate above cannot catch it, and the replace
	// sequence only verifies copy fidelity. Probe the output as decodable
	// media before anything is deleted, and compare durations like-for-like
	// (video-stream durations when both probes expose them — see
	// verifyDurationMatch).
	newInfo, err := ffmpeg.ProbeMedia(filepath.Join(filepath.Dir(o.FFmpegPath), "ffprobe"), scratchPath)
	if err != nil {
		o.failVerification(ctx, job, fmt.Sprintf("transcoded output failed validation: %v", err), paths)
		return nil, err
	}
	if err := verifyDurationMatch(origInfo, newInfo); err != nil {
		o.failVerification(ctx, job, err.Error(), paths)
		return nil, err
	}
	if err := verifyAVAlignment(origInfo, newInfo); err != nil {
		o.failVerification(ctx, job, err.Error(), paths)
		return nil, err
	}

	// Opt-in quality spot check: compare a bounded sample of the original
	// against the assembled output. Runs on the scratch file for both
	// pipeline shapes (chunked output is fully assembled by the time the
	// runner returns) and before any disposition touches the source. A low
	// score or an unmeasurable one is a warning, never a failure.
	var vmafScore *float64
	if spec.PostTranscodeVerification {
		vmafScore = o.spotCheckVMAF(ctx, job, scratchPath, origInfo.Duration, spec.QualityTier)
	}
	log.Printf("Job %s: phase verify: passed", job.ID)

	// Verification succeeded; the segment directory and the crash-recovery
	// journal are no longer needed.
	_ = os.RemoveAll(segDir)
	_ = os.Remove(paths.JournalFile())

	scratchStat, err := os.Stat(scratchPath)
	if err != nil {
		err = fmt.Errorf("failed to stat scratch file: %w", err)
		o.failJob(job, err.Error(), paths)
		return nil, err
	}
	newSize := scratchStat.Size()
	origSize := origInfo.Size
	savingsBytes := origSize - newSize
	if savingsBytes < 0 {
		savingsBytes = 0
	}

	// Move to REPLACING
	if err := o.SM.Transition(job, models.StateReplacing); err != nil {
		return nil, err
	}
	log.Printf("Job %s: phase replace: placing transcoded output (%s) and applying source action (%s)",
		job.ID, spec.OutputFileStrategy, spec.ActionOnSuccess)

	var placementProg PlacementProgressCallback
	if onProgress != nil {
		placementProg = func(subPhase, activity string, transferred, total int64, pct float64) {
			onProgress(ffmpeg.Progress{
				Phase:            "replacing",
				SubPhase:         subPhase,
				Activity:         activity,
				TransferredBytes: transferred,
				TotalBytes:       total,
				ProgressPct:      pct,
			})
		}
	}

	placedPath, err := o.Pipeline.ApplyPlacementAndDisposition(paths, spec.OutputFileStrategy, spec.ActionOnSuccess, spec.Container, placementProg)
	if err != nil {
		if destPath != "" && destPath != paths.Source() {
			_ = os.Remove(destPath)
		}
		o.failJob(job, fmt.Sprintf("placement and disposition failed: %v", err), paths)
		return nil, err
	}
	log.Printf("Job %s: phase replace: done, placed at %s", job.ID, placedPath)

	// Move to COMPLETED
	if err := o.SM.Transition(job, models.StateCompleted); err != nil {
		return nil, err
	}

	newHash, err := scanner.Fingerprint(placedPath)
	if err != nil {
		log.Printf("Fingerprint failed for new file %s: %v", placedPath, err)
	}

	if o.Queries != nil && newHash != "" {
		if err := o.Queries.UpsertCompletedTranscode(ctx, db.UpsertCompletedTranscodeParams{
			FilePath:    placedPath,
			FileHash:    newHash,
			SourceCodec: origInfo.Codec,
			TargetCodec: targetCodec,
			OutputSize:  newSize,
		}); err != nil {
			log.Printf("Failed to upsert completed transcode for %s: %v", placedPath, err)
		}
	}

	return &JobResult{
		OriginalSize: origSize,
		NewSize:      newSize,
		SavingsBytes: savingsBytes,
		DurationSecs: origInfo.Duration,
		OriginalHash: origHash,
		NewHash:      newHash,
		Commands:     cmdTrace,
		VMAFScore:    vmafScore,
	}, nil
}

// spotCheckVMAF runs the opt-in VMAF comparison between the original and
// the assembled scratch output over a bounded sample. An unmeasurable score
// (ffmpeg build without libvmaf filter, unreadable input) skips the check;
// a score below the quality floor logs a warning; neither ever fails the job.
func (o *Orchestrator) spotCheckVMAF(ctx context.Context, job *Job, scratchPath string, duration float64, tierOrPreset string) *float64 {
	if duration <= 0 {
		log.Printf("Job %s: phase verify: vmaf skipped: unknown source duration", job.ID)
		return nil
	}
	sample := ffmpeg.VMAFSampleDurationSecs
	if duration < sample {
		sample = duration
	}
	offset := ffmpeg.VMAFSampleOffsetSecs
	if maxOff := duration - sample; offset > maxOff {
		// Short sources: pull the sample window back so it still fits.
		offset = maxOff
		if offset < 0 {
			offset = 0
		}
	}
	// Bounded so a wedged comparison cannot hold up job teardown; the
	// sample is a 60s decode and finishes in seconds on healthy files.
	vmafCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	score, err := ffmpeg.MeasureVMAF(vmafCtx, o.FFmpegPath, job.FilePath, scratchPath, offset, sample)
	if err != nil {
		log.Printf("Job %s: phase verify: vmaf skipped: %v", job.ID, err)
		return nil
	}
	floor := ffmpeg.QualityFloor(tierOrPreset)
	if score < floor {
		log.Printf("Job %s: phase verify: WARNING vmaf %.2f below quality floor %.2f (non-fatal)", job.ID, score, floor)
	} else {
		log.Printf("Job %s: phase verify: vmaf %.2f (sample %.0fs at +%.0fs)", job.ID, score, sample, offset)
	}
	return &score
}

// verifyDurationMatch enforces ADR-004's mandatory duration gate. Chunked
// output is compared like-for-like on video-stream durations when both probes
// expose them: concatenated Matroska accumulates muxer/DTS padding at chunk
// junctions (~4 frames per seam) that inflates the container format duration
// without representing lost or added content. When a stream-level duration is
// unavailable for either side, the container durations are compared instead.
// The tolerance is a flat ±2s regardless of chunk count: seam drift is
// eliminated by the segment timing manifest at concat time (#190), so a
// per-seam inflation would only let multi-second content drift — including
// A/V desync expressed as duration drift — pass silently.
func verifyDurationMatch(origInfo, newInfo ffmpeg.MediaInfo) error {
	if origInfo.VideoStreamDuration > 0 && newInfo.VideoStreamDuration > 0 {
		diff := newInfo.VideoStreamDuration - origInfo.VideoStreamDuration
		if diff < -2.0 || diff > 2.0 {
			return fmt.Errorf("duration verification failed: output video stream duration %.2f differs from input %.2f by >2s", newInfo.VideoStreamDuration, origInfo.VideoStreamDuration)
		}
		return nil
	}

	diff := newInfo.Duration - origInfo.Duration
	if diff < -2.0 || diff > 2.0 {
		return fmt.Errorf("duration verification failed: output duration %.2f differs from input %.2f by >2s", newInfo.Duration, origInfo.Duration)
	}
	return nil
}

// avAlignToleranceSecs is the alignment gate: input and output audio/video
// start-time offsets must agree within ~60ms (preventing false-positive
// failures from 1-frame container rounding at ~24fps) or the job fails
// instead of shipping visible desync.
const avAlignToleranceSecs = 0.060

// verifyAVAlignment compares the output's audio-minus-video start-time offset
// against the input's. Seam-exact stitching (#190) and preserved track
// offsets (#192) make the offset invariant across a healthy transcode, so any
// shift beyond avAlignToleranceSecs means desync. When either probe lacks
// start-time metadata for both streams the check is skipped rather than
// failing the job on a metadata hiccup.
func verifyAVAlignment(origInfo, newInfo ffmpeg.MediaInfo) error {
	origOffset, origOK := origInfo.AVOffset()
	newOffset, newOK := newInfo.AVOffset()
	if !origOK || !newOK {
		return nil
	}
	if delta := newOffset - origOffset; delta < -avAlignToleranceSecs || delta > avAlignToleranceSecs {
		return fmt.Errorf("a/v alignment verification failed: output audio/video offset %.3fs differs from input %.3fs by %.0fms (>±60ms)",
			newOffset, origOffset, delta*1000)
	}
	return nil
}

func (o *Orchestrator) failJob(job *Job, reason string, paths jobpaths.JobPaths) {
	job.ErrorMessage = reason
	o.SM.Transition(job, models.StateFailed)
	if scratchPath := paths.Scratch(); scratchPath != "" {
		os.Remove(scratchPath)
	}
	for _, strat := range []string{"alongside", "alternate"} {
		if dPath, err := paths.DestinationPath(strat, ""); err == nil && dPath != "" && dPath != paths.Source() {
			_ = os.Remove(dPath)
		}
	}
	// The demux scratch container lives in the segment dir, which is kept
	// after a plain failure so the journal can resume the job — remove the
	// container explicitly (the resume re-extracts it if it is gone). The
	// chapter sidecar follows the same contract.
	if demuxPath := paths.DemuxContainer(); demuxPath != "" {
		os.Remove(demuxPath)
	}
	if chapterPath := paths.ChapterFile(); chapterPath != "" {
		os.Remove(chapterPath)
	}
}

func (o *Orchestrator) failVerification(ctx context.Context, job *Job, reason string, paths jobpaths.JobPaths) {
	job.ErrorMessage = reason
	_ = o.SM.Transition(job, models.StateFailed)
	_ = o.SM.Transition(job, models.StatePermFailed)
	if scratchPath := paths.Scratch(); scratchPath != "" {
		_ = os.Remove(scratchPath)
	}
	for _, strat := range []string{"alongside", "alternate"} {
		if dPath, err := paths.DestinationPath(strat, ""); err == nil && dPath != "" && dPath != paths.Source() {
			_ = os.Remove(dPath)
		}
	}
	if segDir := paths.SegmentDir(); segDir != "" {
		_ = os.RemoveAll(segDir)
	}
	_ = os.Remove(paths.JournalFile())
	if o.Queries != nil && job.ID != "" {
		_ = o.Queries.DeleteJobJournal(context.Background(), job.ID)
	}
}

func (o *Orchestrator) failJobStopped(job *Job, reason string, paths jobpaths.JobPaths) {
	job.ErrorMessage = reason
	_ = o.SM.Transition(job, models.StateStopped)
	if scratchPath := paths.Scratch(); scratchPath != "" {
		_ = os.Remove(scratchPath)
	}
	for _, strat := range []string{"alongside", "alternate"} {
		if dPath, err := paths.DestinationPath(strat, ""); err == nil && dPath != "" && dPath != paths.Source() {
			_ = os.Remove(dPath)
		}
	}
	if segDir := paths.SegmentDir(); segDir != "" {
		_ = os.RemoveAll(segDir)
	}
	_ = os.Remove(paths.JournalFile())
	if o.Queries != nil && job.ID != "" {
		_ = o.Queries.DeleteJobJournal(context.Background(), job.ID)
	}
}

// DiscardJobFiles removes the scratch file and segment directory of a job
// that is no longer active locally (e.g. a CANCEL_JOB arrived after the job
// already terminated) and deletes its journal payload. The daemon's cancel
// path delegates here so the path layout stays owned by JobPaths.
func (o *Orchestrator) DiscardJobFiles(ctx context.Context, filePath, jobID string) {
	paths := jobpaths.New(o.Pipeline.ScratchDir, o.Pipeline.QuarantineDir, filePath)
	_ = os.Remove(paths.Scratch())
	_ = os.RemoveAll(paths.SegmentDir())
	_ = os.Remove(paths.JournalFile())
	if o.Queries != nil && jobID != "" {
		_ = o.Queries.DeleteJobJournal(ctx, jobID)
	}
}

func (o *Orchestrator) RecoverJobs(ctx context.Context) [][]byte {
	var payloads [][]byte
	if o.Queries == nil {
		return nil
	}
	jobs, err := o.Queries.ListIncompleteJobs(ctx)
	if err != nil {
		return nil
	}
	for _, j := range jobs {
		paths := jobpaths.New(o.Pipeline.ScratchDir, o.Pipeline.QuarantineDir, j.FilePath)
		scratchPath := paths.Scratch()
		quarantineDest := paths.Quarantine()

		_, origErr := os.Stat(j.FilePath)
		_, quarErr := os.Stat(quarantineDest)

		restoredQuarantine := false
		if origErr != nil && quarErr == nil {
			quarHash, _, copyErr := copyAndHash(quarantineDest, j.FilePath)
			if copyErr != nil {
				log.Printf("recovery: failed to restore quarantine %s to %s: %v; quarantine preserved", quarantineDest, j.FilePath, copyErr)
				_ = o.Queries.UpsertJobJournal(ctx, db.UpsertJobJournalParams{
					JobID:    j.JobID,
					FilePath: j.FilePath,
					State:    string(models.StatePermFailed),
				})
				continue
			} else if hashErr := VerifyFileHash(j.FilePath, quarHash); hashErr != nil {
				// SHA-256 of the destination doesn't match the source copy — unsafe to proceed.
				log.Printf("recovery: hash verification failed for restored file %s: %v; removing corrupt destination, quarantine preserved", j.FilePath, hashErr)
				_ = os.Remove(j.FilePath)
				_ = o.Queries.UpsertJobJournal(ctx, db.UpsertJobJournalParams{
					JobID:    j.JobID,
					FilePath: j.FilePath,
					State:    string(models.StatePermFailed),
				})
				continue
			} else {
				_ = os.Remove(quarantineDest)
				restoredQuarantine = true
			}
		}

		segDir := paths.SegmentDir()

		if j.State == string(models.StatePermFailed) || j.State == string(models.StateStopped) {
			_ = os.Remove(scratchPath)
			_ = os.RemoveAll(segDir)
			_ = os.Remove(paths.JournalFile())
			_ = o.Queries.DeleteJobJournal(ctx, j.JobID)
			continue
		}

		// Jobs in flight before the journal moved next to the scratch file
		// still carry it inside the segment directory; consult that location
		// as a fallback so they resume instead of being discarded.
		b, err := os.ReadFile(paths.JournalFile())
		if err != nil {
			b, err = os.ReadFile(paths.LegacyJournalFile())
		}

		if err == nil {
			// Apply disk space gate before attempting job recovery to avoid infinite retry loops on full volumes.
			if bErr := o.Pipeline.CheckBudget(filepath.Dir(j.FilePath)); bErr != nil {
				log.Printf("recovery: disk space or quarantine budget critical for job %s: %v; leaving in QUEUED", j.JobID, bErr)
				_ = o.Queries.UpsertJobJournal(ctx, db.UpsertJobJournalParams{
					JobID:    j.JobID,
					FilePath: j.FilePath,
					State:    string(models.StateQueued),
				})
				continue
			}

			probeBin := filepath.Join(filepath.Dir(o.FFmpegPath), "ffprobe")
			origDuration := 0.0
			if origInfo, err := ffmpeg.ProbeMedia(probeBin, j.FilePath); err == nil {
				origDuration = origInfo.Duration
			}
			segmentSecs := 300.0
			totalSegments := 1
			if origDuration > 0 {
				totalSegments = int(math.Ceil(origDuration / segmentSecs))
			}

			// Validate segments
			files, _ := os.ReadDir(segDir)
			// The split phase's recorded timing manifest knows each chunk's
			// real source-timeline duration and count; a source with no
			// keyframe inside a segment window produces chunks longer (or
			// shorter) than the nominal segmentSecs expectation, which would
			// wrongly reject healthy work and force a full re-split on resume.
			segTimings, segTimingErr := ffmpeg.ParseSegmentTimingManifest(paths.SegmentTimingManifest())
			if segTimingErr != nil && !errors.Is(segTimingErr, os.ErrNotExist) {
				log.Printf("recovery: segment timing manifest %s unreadable, falling back to nominal chunk expectations: %v", paths.SegmentTimingManifest(), segTimingErr)
			}
			expectedChunks := totalSegments
			if len(segTimings) > 0 {
				expectedChunks = len(segTimings)
			}
			validationFailed := false
			for i := 0; i < len(files); i++ {
				chunkFile := paths.Chunk(i)
				if _, err := os.Stat(chunkFile); err != nil {
					break // Missing chunk, we will resume from here
				}
				expectedDur := segmentSecs
				if origDuration > 0 && i == totalSegments-1 {
					expectedDur = origDuration - float64(i)*segmentSecs
				}
				if i < len(segTimings) {
					if d := segTimings[i].Duration(); d > 0 {
						expectedDur = d
					}
				}
				if origDuration > 0 && i >= expectedChunks {
					validationFailed = true
					break
				}
				if err := ffmpeg.VerifyChunkCandidate(ctx, o.FFmpegPath, probeBin, chunkFile, expectedDur); err != nil {
					validationFailed = true
					break
				}
			}

			if validationFailed {
				os.RemoveAll(segDir)
				payloads = append(payloads, b)
				_ = o.Queries.UpsertJobJournal(ctx, db.UpsertJobJournalParams{
					JobID:    j.JobID,
					FilePath: j.FilePath,
					State:    string(models.StateQueued),
				})
				continue
			}

			// Remove any chunks after the first missing/invalid one to ensure contiguous offset
			for i := 0; i < 9999; i++ {
				chunkFile := paths.Chunk(i)
				if _, err := os.Stat(chunkFile); err != nil {
					// from here on, remove anything that exists
					for k := i + 1; k < 9999; k++ {
						nextChunk := paths.Chunk(k)
						if _, err2 := os.Stat(nextChunk); err2 == nil {
							os.Remove(nextChunk)
						} else {
							break
						}
					}
					break
				}
			}

			payloads = append(payloads, b)
			_ = o.Queries.UpsertJobJournal(ctx, db.UpsertJobJournalParams{
				JobID:    j.JobID,
				FilePath: j.FilePath,
				State:    string(models.StateResuming),
			})
			continue
		}

		_, scratchErr := os.Stat(scratchPath)
		if scratchErr == nil {
			_ = os.Remove(scratchPath)
		}
		_ = os.RemoveAll(segDir)

		if restoredQuarantine {
			_ = o.Queries.UpsertJobJournal(ctx, db.UpsertJobJournalParams{
				JobID:    j.JobID,
				FilePath: j.FilePath,
				State:    string(models.StateQueued),
			})
		} else {
			_ = o.Queries.DeleteJobJournal(ctx, j.JobID)
		}
	}
	return payloads
}

// writeJournalAtomic writes data to a temporary file, flushes it to disk,
// and renames it over target so crashes mid-write cannot leave corrupt payloads.
func writeJournalAtomic(target string, data []byte) error {
	tmp := target + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if dir, err := os.Open(filepath.Dir(target)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}
