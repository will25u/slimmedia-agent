package ffmpeg

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/will25u/slimmedia-agent/internal/jobpaths"
	"github.com/will25u/slimmedia-agent/internal/jobspec"
	"github.com/will25u/slimmedia-agent/internal/safety"
	"github.com/will25u/slimmedia-agent/internal/protocol"
)

// segmentSecs is the fixed chunk duration for the split-first pipeline. It is
// a package variable (not a constant) so integration tests can shrink it and
// exercise real multi-chunk seams without encoding minutes of media.
var segmentSecs = 300.0

type Progress struct {
	Phase        string
	CurrentIndex int
	TotalCount   int
	ElapsedSecs  float64
	Frame        int
	TotalFrames  int
	FPS          float64
	TimeSeconds  float64
	Speed        float64
	ProgressPct  float64
	ETASeconds   int
	BitrateKbps  int
	SourceCodec  string
	TargetCodec  string

	SubPhase         string
	Activity         string
	TransferredBytes int64
	TotalBytes       int64
}

func sendJobProgress(job TranscodeJob, p Progress) {
	if job.ProgressChan != nil {
		select {
		case job.ProgressChan <- p:
		default:
		}
	}
}

func parseFFmpegTimeString(val string) float64 {
	pParts := strings.Split(val, ":")
	if len(pParts) == 3 {
		h, _ := strconv.ParseFloat(pParts[0], 64)
		m, _ := strconv.ParseFloat(pParts[1], 64)
		s, _ := strconv.ParseFloat(pParts[2], 64)
		return h*3600 + m*60 + s
	}
	return 0
}

const maxProgressWriterBytes = 64 * 1024

type progressWriter struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	lastTime float64
}

func (w *progressWriter) Write(p []byte) (n int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	if w.buf.Len() > maxProgressWriterBytes*2 {
		b := w.buf.Bytes()
		trimmed := b[len(b)-maxProgressWriterBytes:]
		w.buf.Reset()
		w.buf.Write(trimmed)
	}
	s := string(p)
	if idx := strings.Index(s, "time="); idx != -1 {
		rest := s[idx+5:]
		end := strings.IndexAny(rest, " \r\n\t")
		var timeStr string
		if end != -1 {
			timeStr = rest[:end]
		} else {
			timeStr = rest
		}
		if t := parseFFmpegTimeString(timeStr); t > 0 {
			w.lastTime = t
		}
	}
	return len(p), nil
}

func (w *progressWriter) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Bytes()
}

func (w *progressWriter) LastTime() float64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastTime
}

type TranscodeJob struct {
	// JobID attributes phase logs to the owning job; empty in direct
	// runner tests, where the input file name identifies the run.
	JobID string

	// Spec is the job's preset-tuning options, owned by the jobspec package
	// and passed down from the dispatch boundary unchanged.
	Spec jobspec.JobSpec

	FFmpegPath   string
	InputFile    string
	OutputFile   string
	Codec        string
	SourceCodec  string
	DurationSec      float64
	TotalFrames      int
	ExactFramesKnown bool
	ProgressChan     chan<- Progress

	// Execution-only fields derived from the environment, not the wire
	// payload: probed encoder capabilities, stream/color probes, and the
	// daemon's pause/schedule hooks.
	Caps *EncoderCaps
	// HDR color metadata probed from the input; nil for SDR or probe
	// failure. Not a wire field: populated by RunTranscode itself.
	ColorInfo *ColorInfo
	// Steering is the topology-driven thread steering for software encodes
	// (issue #201): encoder pool/thread caps in the argv plus an optional
	// process CPU affinity. Nil or disabled leaves the spawn untouched.
	// Not a wire field: derived locally from the startup hardware probe.
	Steering *ThreadSteering

	Schedule     *protocol.ScheduleConfig
	GetSchedule  func() *protocol.ScheduleConfig
	OnPause      func(paused bool)
	IsPaused     func() bool
	PollInterval time.Duration
	NowFunc      func() time.Time

	// Paths carries the job's segment directory and derived file names,
	// owned by the jobpaths package. When nil, the layout is derived from
	// OutputFile via jobpaths.FromOutput.
	Paths *jobpaths.JobPaths

	// TracePhase labels this job's ffmpeg invocations in the command trace
	// ("encode" for full-stream passes, "encode:chunk" for chunk encodes,
	// "concat" for assembly). Empty defaults to "encode".
	TracePhase string
	// Trace, when non-nil, collects one CommandRecord per ffmpeg phase so
	// the exact argv is reportable at completion. Written once per phase,
	// never on the progress hot path.
	Trace *[]protocol.FfmpegCommandRecord
	// OnTrace, when non-nil, is invoked each time a NEW phase record is
	// appended to Trace (not on Count increments). The daemon uses it to
	// persist the command trace at phase-start time, before the encode
	// finishes — so crashes and cancellations keep the argv too.
	OnTrace func(trace []protocol.FfmpegCommandRecord)
}

// CommandRecord is the wire shape of one recorded ffmpeg invocation.
type CommandRecord = protocol.FfmpegCommandRecord

// recordCommand logs the full ffmpeg argv at info level and appends it to
// the job's command trace. Callers invoke it once per phase. Consecutive
// records with the same phase label collapse into one record with an
// incremented Count — per-chunk encodes share one argv shape modulo their
// input/output paths, so the trace stays bounded on long sources.
func (j *TranscodeJob) recordCommand(argv []string) {
	phase := j.TracePhase
	if phase == "" {
		phase = "encode"
	}
	j.logf("phase %s: ffmpeg %s", phase, strings.Join(argv, " "))
	if j.Trace == nil {
		return
	}
	trace := *j.Trace
	if n := len(trace); n > 0 && trace[n-1].Phase == phase {
		trace[n-1].Count++
		*j.Trace = trace
		return
	}
	*j.Trace = append(trace, protocol.FfmpegCommandRecord{
		Phase: phase,
		Argv:  append([]string{}, argv...),
		Count: 1,
	})
	if j.OnTrace != nil {
		j.OnTrace(append([]protocol.FfmpegCommandRecord{}, *j.Trace...))
	}
}

// logf prefixes a phase telemetry line with the owning job's identity.
func (j *TranscodeJob) logf(format string, args ...any) {
	id := j.JobID
	if id == "" {
		id = filepath.Base(j.InputFile)
	}
	log.Printf("Job %s: "+format, append([]any{id}, args...)...)
}

func RunTranscode(ctx context.Context, job TranscodeJob) error {
	if job.ProgressChan != nil {
		defer close(job.ProgressChan)
	}

	if err := CheckLocalMediaPath(job.InputFile); err != nil {
		return fmt.Errorf("invalid input path: %w", err)
	}
	if err := CheckLocalMediaPath(job.OutputFile); err != nil {
		return fmt.Errorf("invalid output path: %w", err)
	}

	if info, err := os.Lstat(job.InputFile); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("invalid input path: symlink traversal not allowed: %q", job.InputFile)
		}
		if f, err := OpenNoFollow(job.InputFile); err != nil {
			return fmt.Errorf("invalid input path: cannot open with safe path pinning: %w", err)
		} else {
			f.Close()
		}
	}

	// A legacy-shaped payload (pre-tuning, no spec_version) keeps the historic
	// single-pass argv only when no pause/schedule controls are attached.
	// The daemon always attaches IsPaused, and legacy jobs resolve to a
	// software encoder, so daemon-dispatched legacy jobs take the chunked
	// path — the chunk boundary is where pause and quiet hours take effect.
	legacySinglePass := job.Spec.Legacy() && job.Schedule == nil && job.GetSchedule == nil && job.IsPaused == nil

	// The pipeline shape follows the resolved encoder: software encoders run
	// the chunked path (pause/resume and crash recovery are chunk-granular
	// there); hardware encoders run one full-stream pass with no demux, split,
	// or concat phases. A spec-level override forces either mode.
	chunked := true
	if !legacySinglePass {
		chunked = UseChunkedPipeline(job.Spec, job.Codec, job.Caps)
	}

	if job.Spec.CustomFlags != "" {
		tokens, err := SanitizeCustomFlags(job.Spec.CustomFlags)
		if err != nil {
			return fmt.Errorf("invalid custom flags: %w", err)
		}
		// Jobs routed through the chunked pipeline lock their frame timing
		// per chunk: fps-affecting custom flags collide in chunk scope and
		// must fail here, before any phase work, instead of silently
		// overriding the passthrough at the first chunk boundary. Full-stream
		// single-pass encodes carry no such invariant, so a user rate option
		// is honored there.
		if !legacySinglePass {
			scope := protocol.ScopeEncodeAll
			if chunked {
				scope = protocol.ScopeEncodeChunk
			}
			for _, tok := range tokens {
				if col := protocol.CheckInvariantCollision(tok, scope); col != nil {
					return fmt.Errorf("invalid custom flags: %w", col)
				}
			}
		}
	}

	if legacySinglePass {
		job.logf("phase encode: starting single-pass transcode")
		_, _, err := runSinglePass(ctx, job, job.ProgressChan, nil, 0, 0, 0, progressLatch{})
		return err
	}

	if !chunked {
		return runFullStreamSinglePass(ctx, job)
	}

	streams := ProbeStreams(ffprobeNextTo(job.FFmpegPath), job.InputFile)
	job.ColorInfo = ProbeColorInfo(ffprobeNextTo(job.FFmpegPath), job.InputFile)

	duration := job.DurationSec
	if duration <= 0 {
		duration = 0
	}

	totalSegments := 1
	if duration > 0 {
		totalSegments = int(math.Ceil(duration / segmentSecs))
	}

	paths := job.Paths
	if paths == nil {
		derived := jobpaths.FromOutput(job.OutputFile)
		paths = &derived
	}
	segDir := paths.SegmentDir()
	if err := os.MkdirAll(segDir, 0755); err != nil {
		return fmt.Errorf("failed to create segment dir: %w", err)
	}

	pollInterval := job.PollInterval
	if pollInterval <= 0 {
		pollInterval = 5 * time.Second
	}
	nowFn := job.NowFunc
	if nowFn == nil {
		nowFn = time.Now
	}

	// Phase 0: Upfront demux — one unbroken stream-copy pass over the source
	// that extracts every audio, subtitle, and chapter stream into a local
	// scratch container, verified with ffprobe. Later phases consume the
	// container instead of re-reading the (possibly network-mounted) source.
	if err := extractSidecarStreams(ctx, job, paths, streams); err != nil {
		return err
	}

	// Phase 1: Split-First — stream-copy the source into fixed-duration chunks
	// using the segment muxer. Each chunk starts at a true keyframe boundary
	// with no timestamp drift, avoiding the -ss/-t seek approximation. The
	// muxer records a segment timing manifest (segments.csv) alongside the
	// chunks: exact source-timeline start/end per chunk, driving the concat
	// manifest's duration directives and per-chunk progress offsets.
	job.logf("phase split: splitting source into chunks of %.0fs", segmentSecs)
	splitFiles, timings, err := splitSourceIntoChunks(ctx, job, paths, segDir, segmentSecs, totalSegments)
	if err != nil {
		return fmt.Errorf("source split failed: %w", err)
	}
	job.logf("phase split: done (%d chunks)", len(splitFiles))

	var segmentFiles []string
	ffprobePath := ffprobeNextTo(job.FFmpegPath)

	var accumulatedFrames int
	var latch progressLatch

	// Phase 2: Transcode each pre-split chunk directly — no -ss or -t flags.
	for i, srcChunk := range splitFiles {
		encodedChunk := paths.EncodedChunk(i)

		if _, err := os.Stat(encodedChunk); err == nil {
			types, probeErr := probeStreamCodecTypes(ffprobePath, encodedChunk)
			if probeErr != nil {
				return fmt.Errorf("chunk %d stream probe failed: %w", i, probeErr)
			}
			if !isPureVideoStreamList(types) {
				job.logf("phase encode: chunk %d/%d exists but is not pure video (streams %v); re-encoding", i+1, len(splitFiles), types)
				_ = os.Remove(encodedChunk)
			} else {
				expectedDur := segmentSecs
				if i < len(timings) && timings[i].Duration() > 0 {
					expectedDur = timings[i].Duration()
				} else if duration > 0 && i == len(splitFiles)-1 {
					expectedDur = duration - float64(i)*segmentSecs
				}

				if err := verifyChunkIntegrity(ctx, job.FFmpegPath, ffprobePath, encodedChunk, expectedDur); err != nil {
					job.logf("phase encode: chunk %d/%d exists but failed integrity verification (%v); re-encoding", i+1, len(splitFiles), err)
					_ = os.Remove(encodedChunk)
				} else {
					job.logf("phase encode: chunk %d/%d already encoded and verified; skipping", i+1, len(splitFiles))
					segmentFiles = append(segmentFiles, encodedChunk)
					if cf := probeChunkFrames(ffprobePath, encodedChunk); cf > 0 {
						accumulatedFrames += cf
					} else if job.TotalFrames > 0 && duration > 0 {
						accumulatedFrames += int(math.Round(segmentSecs * (float64(job.TotalFrames) / duration)))
					}
					continue
				}
			}
		}

		if err := waitForUnpaused(ctx, job, pollInterval, nowFn); err != nil {
			return err
		}

		chunkJob := job
		chunkJob.InputFile = srcChunk
		chunkJob.OutputFile = encodedChunk
		chunkJob.TracePhase = "encode:chunk"
		// Chunks transcode strictly video: audio/subtitle/data streams live in
		// the demux scratch container and are muxed back at final assembly, so
		// carrying them through chunk encodes only adds sample overhang,
		// boundary priming delays, and timestamp bloat at every segment seam.
		chunkJob.Spec.SubtitleHandling = "strip_all"
		chunkJob.Spec.AudioHandling = "strip_all"

		job.logf("phase encode: starting chunk %d/%d", i+1, len(splitFiles))
		// Progress offsets reflect the chunk's real start in the source
		// timeline (from the timing manifest), not the nominal i*segmentSecs:
		// a source with no keyframe inside a segment window starts its next
		// chunk late, and nominal offsets would make progress jump backwards.
		baseTime := float64(i) * segmentSecs
		if i < len(timings) {
			baseTime = timings[i].Start
		}
		chunkFrames, newLatch, err := runSinglePass(ctx, chunkJob, job.ProgressChan, streams, baseTime, duration, accumulatedFrames, latch)
		if err != nil {
			return fmt.Errorf("chunk %d encode failed: %w", i, err)
		}
		latch = newLatch
		if chunkFrames > 0 {
			accumulatedFrames += chunkFrames
		} else if cf := probeChunkFrames(ffprobePath, encodedChunk); cf > 0 {
			accumulatedFrames += cf
		} else if job.TotalFrames > 0 && duration > 0 {
			accumulatedFrames += int(math.Round(segmentSecs * (float64(job.TotalFrames) / duration)))
		}
		if err := verifyChunkVideoOnly(ffprobePath, encodedChunk); err != nil {
			_ = os.Remove(encodedChunk)
			return fmt.Errorf("chunk %d failed video-only verification: %w", i, err)
		}
		job.logf("phase encode: chunk %d/%d done", i+1, len(splitFiles))
		segmentFiles = append(segmentFiles, encodedChunk)
	}

	// Phase 3: Local stitch — concatenate the encoded chunks (input 0) and
	// mux audio, subtitles, and chapters exclusively from the local scratch
	// artifacts of the upfront demux pass (input 1). The remote source is
	// never re-opened: total per-job source reads stay at the one upfront
	// pass, and the assembly itself finishes in seconds.
	// The manifest carries an explicit duration per chunk, derived from the
	// split phase's timing manifest (end - start in the source timeline).
	// Without it the concat demuxer infers each seam offset from the chunk
	// container duration, which includes B-frame flush padding and accumulates
	// +40–80ms of video drift per seam against the un-split audio.
	job.logf("phase concat: stitching %d encoded chunks", len(segmentFiles))
	concatFile := paths.ConcatFile()
	var manifest string
	if len(timings) > 0 {
		if len(timings) != len(segmentFiles) {
			return fmt.Errorf("segment timing manifest has %d entries for %d encoded chunks", len(timings), len(segmentFiles))
		}
		durations := make([]float64, len(segmentFiles))
		for i := range segmentFiles {
			if i+1 < len(timings) {
				// A non-last chunk's timeline advance is the next chunk's
				// start minus its own: the recorded end is the last packet's
				// end time, which on VFR sources can fall short of the true
				// boundary (a dropped-frame hole before the keyframe), and
				// placing the next chunk by it would pull every later seam
				// early. For CFR sources the two coincide.
				durations[i] = timings[i+1].Start - timings[i].Start
			} else {
				// The last chunk has no successor, so its own recorded
				// duration stands.
				durations[i] = timings[i].Duration()
			}
		}
		manifest, err = BuildConcatManifestWithDurations(segmentFiles, durations)
	} else {
		manifest, err = BuildConcatManifest(segmentFiles)
	}
	if err != nil {
		return fmt.Errorf("failed to build concat manifest: %w", err)
	}
	if err := os.WriteFile(concatFile, []byte(manifest), 0644); err != nil {
		return fmt.Errorf("failed to write concat file: %w", err)
	}

	sidecar := AssemblySidecar{}
	if _, err := os.Stat(paths.DemuxContainer()); err == nil {
		sidecar.DemuxPath = paths.DemuxContainer()
		// Map audio/subtitles against the container's own stream layout:
		// indexes there are authoritative (the demux pass may have dropped
		// Matroska-incompatible subtitle tracks the source carried).
		if demuxStreams := ProbeStreams(ffprobePath, sidecar.DemuxPath); demuxStreams != nil {
			streams = demuxStreams
		}
	} else if _, err := os.Stat(paths.ChapterFile()); err == nil {
		sidecar.ChapterPath = paths.ChapterFile()
	}

	concatArgs := BuildConcatArgs(job, concatFile, sidecar, streams)

	if err := protocol.VerifyDefenseInDepth(concatArgs, protocol.ScopeConcat); err != nil {
		return err
	}

	job.TracePhase = "concat"
	job.recordCommand(concatArgs)

	concatCmd := execCommandContext(ctx, job.FFmpegPath, concatArgs...)
	if out, err := concatCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("concat failed: %v: %s", err, string(out))
	}
	job.logf("phase concat: done")

	return nil
}

// pauseRequested reports whether the job's pause flag is set or its
// quiet-hours schedule is currently active. It is the single predicate both
// pipeline shapes consult before starting (or, for single-pass, while
// running) an encode.
func pauseRequested(job TranscodeJob, nowFn func() time.Time) bool {
	sched := job.Schedule
	if job.GetSchedule != nil {
		sched = job.GetSchedule()
	}
	return (sched != nil && safety.IsQuietHoursActive(nowFn(), sched)) ||
		(job.IsPaused != nil && job.IsPaused())
}

// waitForUnpaused blocks while a pause flag or quiet-hours schedule is
// active, reporting OnPause transitions: OnPause(true) when the hold begins,
// OnPause(false) when it ends. It returns nil once neither gate is active
// (or immediately when the job carries no pause/schedule controls) and
// ctx.Err() when the job is cancelled while held.
func waitForUnpaused(ctx context.Context, job TranscodeJob, pollInterval time.Duration, nowFn func() time.Time) error {
	if job.Schedule == nil && job.GetSchedule == nil && job.IsPaused == nil {
		return nil
	}
	isPaused := false
	for {
		if !pauseRequested(job, nowFn) {
			break
		}
		if !isPaused {
			isPaused = true
			if job.OnPause != nil {
				job.OnPause(true)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
	if isPaused && job.OnPause != nil {
		job.OnPause(false)
	}
	return nil
}

// runFullStreamSinglePass transcodes the entire source in one ffmpeg pass,
// mapping audio and subtitle streams per the spec. Hardware encoders take
// this path: the chunked pipeline's split phase would re-read the source for
// nothing, and a single pass has no chunk boundary at which pause could take
// effect. Pause and quiet hours therefore cancel the running pass and
// restart it from the beginning once the hold lifts — cancel-and-requeue at
// encode granularity, acceptable because hardware transcodes are fast.
func runFullStreamSinglePass(ctx context.Context, job TranscodeJob) error {
	streams := ProbeStreams(ffprobeNextTo(job.FFmpegPath), job.InputFile)
	job.ColorInfo = ProbeColorInfo(ffprobeNextTo(job.FFmpegPath), job.InputFile)

	pollInterval := job.PollInterval
	if pollInterval <= 0 {
		pollInterval = 5 * time.Second
	}
	nowFn := job.NowFunc
	if nowFn == nil {
		nowFn = time.Now
	}

	var latch progressLatch
	for {
		if err := waitForUnpaused(ctx, job, pollInterval, nowFn); err != nil {
			return err
		}

		// A per-attempt context lets the watcher kill the encode the moment a
		// pause request or quiet-hours window lands, without cancelling the
		// job itself. The parent context distinguishes a real cancel (job
		// stopped) from a pause interrupt (encode restarts).
		attemptCtx, cancelAttempt := context.WithCancel(ctx)
		var interrupted atomic.Bool
		stopWatch := make(chan struct{})
		watchDone := make(chan struct{})
		go func() {
			defer close(watchDone)
			ticker := time.NewTicker(pollInterval)
			defer ticker.Stop()
			for {
				select {
				case <-stopWatch:
					return
				case <-attemptCtx.Done():
					return
				case <-ticker.C:
					if pauseRequested(job, nowFn) {
						interrupted.Store(true)
						cancelAttempt()
						return
					}
				}
			}
		}()

		job.logf("phase encode: starting full-stream single-pass transcode")
		_, _, err := runSinglePass(attemptCtx, job, job.ProgressChan, streams, 0, job.DurationSec, 0, latch)
		close(stopWatch)
		cancelAttempt()
		<-watchDone

		if err == nil {
			job.logf("phase encode: done")
			return nil
		}
		if ctx.Err() != nil {
			return err
		}
		if interrupted.Load() {
			// Cancel-and-requeue: the partial output is discarded and the
			// pass restarts from the beginning after the hold lifts.
			job.logf("phase encode: pass interrupted by pause request; discarding partial output and restarting")
			_ = os.Remove(job.OutputFile)
			latch = progressLatch{}
			continue
		}
		return fmt.Errorf("encode failed: %w", err)
	}
}

// splitSourceIntoChunks runs a single FFmpeg stream-copy pass using the
// segment muxer to split the source into fixed-duration MKV chunks that each
// start at a true keyframe boundary. Returns the ordered list of split paths
// plus the segment timing manifest the muxer records (one entry per chunk
// with its exact source-timeline start and end); the timings are nil when
// the manifest is unavailable, in which case callers fall back to nominal
// chunk timing (the historical behavior).
func splitSourceIntoChunks(ctx context.Context, job TranscodeJob, paths *jobpaths.JobPaths, segDir string, segmentSecs float64, totalSegments int) ([]string, []SegmentTiming, error) {
	ffprobePath := ffprobeNextTo(job.FFmpegPath)

	// Resume: reuse split chunks only when every recorded chunk exists and
	// verifies as pure video. A chunk that smuggles non-video streams (e.g.
	// left behind by an older agent whose split used default stream selection)
	// is deleted so the split re-runs; a probe error is inconclusive and fails
	// the job so a later attempt can re-verify instead of discarding healthy
	// work.
	//
	// The recorded timing manifest is the primary resume key: it lists the
	// chunks the split actually produced, so a source with no keyframe inside
	// a segment window (fewer chunks than ceil(duration/segmentSecs)) resumes
	// without re-splitting. Jobs split before the manifest existed fall back
	// to probing the nominal chunk count; recorded stays nil and callers use
	// nominal chunk timing.
	var recorded []SegmentTiming
	var chunkPaths []string
	if rec, err := ParseSegmentTimingManifest(paths.SegmentTimingManifest()); err == nil {
		recorded = rec
		for _, t := range recorded {
			chunkPaths = append(chunkPaths, filepath.Join(segDir, filepath.Base(t.File)))
		}
	} else {
		for i := 0; i < totalSegments; i++ {
			chunkPaths = append(chunkPaths, paths.SplitChunk(i))
		}
	}
	existing, stale, err := verifySplitChunks(job, ffprobePath, chunkPaths)
	if err != nil {
		return nil, nil, err
	}
	if stale {
		// Remove every split chunk on disk — including strays the manifest
		// does not record — so the post-split manifest/count validation cannot
		// trip over leftovers from an older, longer split.
		removeSplitChunks(paths)
	} else if len(existing) > 0 {
		job.logf("phase split: resuming with %d existing chunks", len(existing))
		return existing, recorded, nil
	}
	// Fall through to re-split; ffmpeg -y overwrites the manifest and any
	// remaining chunks.

	segPattern := paths.SplitChunkPattern()

	args := []string{
		"-hide_banner",
		"-nostdin",
		"-nostats",
		"-y",
		"-probesize", "10000000",
		"-analyzeduration", "10000000",
		"-i", job.InputFile,
		// Map only the video stream: split chunks must be pure video by
		// construction. Default stream selection would silently copy one audio
		// track (and subtitles/data where present) into every chunk, and the
		// audio packet overhang distorts chunk container durations, undermining
		// the seam math the video-only encode exists to protect.
		"-map", "0:v:0",
		"-an", "-sn", "-dn",
		"-c", "copy",
		"-f", "segment",
		"-segment_time", fmt.Sprintf("%.3f", segmentSecs),
		"-reset_timestamps", "1",
		"-segment_format", "matroska",
		// Record the exact per-chunk source-timeline boundaries; the concat
		// manifest derives duration directives from it and resume validates
		// against it.
		"-segment_list", paths.SegmentTimingManifest(),
		"-segment_list_type", "csv",
		segPattern,
	}

	if err := protocol.VerifyDefenseInDepth(args, protocol.ScopeSplit); err != nil {
		return nil, nil, err
	}

	startTime := time.Now()
	sendJobProgress(job, Progress{
		Phase:        "split",
		CurrentIndex: 1,
		TotalCount:   totalSegments,
		ElapsedSecs:  0,
		ProgressPct:  0,
	})

	pw := &progressWriter{}
	cmd := execCommandContext(ctx, job.FFmpegPath, args...)
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("segment muxer failed: %w", err)
	}

	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				elapsed := time.Since(startTime).Seconds()
				created := 0
				for i := 0; i < totalSegments; i++ {
					if _, err := os.Stat(paths.SplitChunk(i)); err == nil {
						created++
					} else {
						break
					}
				}
				curChunk := created + 1
				if curChunk > totalSegments {
					curChunk = totalSegments
				}
				sendJobProgress(job, Progress{
					Phase:        "split",
					CurrentIndex: curChunk,
					TotalCount:   totalSegments,
					ElapsedSecs:  elapsed,
				})
			}
		}
	}()

	err = cmd.Wait()
	close(done)

	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, nil, fmt.Errorf("segment muxer stopped: %w", ctxErr)
		}
		return nil, nil, fmt.Errorf("segment muxer failed: %v: %s", err, tailForError(string(pw.Bytes()), demuxErrorTailBytes))
	}

	sendJobProgress(job, Progress{
		Phase:        "split",
		CurrentIndex: totalSegments,
		TotalCount:   totalSegments,
		ElapsedSecs:  time.Since(startTime).Seconds(),
	})

	var splitFiles []string
	for i := 0; i < 9999; i++ {
		p := paths.SplitChunk(i)
		if _, err := os.Stat(p); err != nil {
			break
		}
		splitFiles = append(splitFiles, p)
	}
	if len(splitFiles) == 0 {
		return nil, nil, fmt.Errorf("segment muxer produced no output files")
	}

	// The timing manifest is the seam-exactness contract for the whole job:
	// it must exist after a fresh split and agree with the chunks on disk.
	// Its absence only occurs with a non-conforming ffmpeg; degrade to
	// nominal timing (bare concat manifest, nominal progress offsets) so the
	// job still completes the way it did before the manifest existed.
	timings, err := ParseSegmentTimingManifest(paths.SegmentTimingManifest())
	if err != nil {
		job.logf("phase split: segment timing manifest unavailable (%v); falling back to nominal seam timing", err)
		timings = nil
	} else {
		if len(timings) != len(splitFiles) {
			return nil, nil, fmt.Errorf("segment timing manifest records %d chunks but split produced %d", len(timings), len(splitFiles))
		}
		for i, t := range timings {
			if got, want := filepath.Base(t.File), filepath.Base(splitFiles[i]); got != want {
				return nil, nil, fmt.Errorf("segment timing manifest chunk %d is %q but split produced %q", i, got, want)
			}
		}
	}

	// Pure video is an invariant of the split output, not a cleanup check for
	// a downstream defect: every split chunk must carry exactly one video
	// stream before the encode phase begins. A violation means the segment
	// muxer ignored the video-only mapping; fail the job rather than let seam
	// overhang reach concatenation. (Non-compliant pre-existing chunks were
	// already discarded above, so a re-split lands here only on a fresh
	// violation.)
	for i, p := range splitFiles {
		if err := verifyChunkVideoOnly(ffprobePath, p); err != nil {
			return nil, nil, fmt.Errorf("split chunk %d failed video-only verification: %w", i, err)
		}
	}
	return splitFiles, timings, nil
}

// verifySplitChunks checks a recorded chunk list for existence and the
// pure-video invariant. It returns the ordered chunk paths when every chunk
// verifies, or stale=true when any chunk is missing or impure (the caller
// deletes the leftovers and re-splits). A probe error is inconclusive — not a
// compliance verdict — and is returned as an error so the job fails while
// healthy work is preserved for a later attempt.
//
// Deliberately probeStreamCodecTypes rather than verifyChunkVideoOnly: the
// resume path must distinguish a probe error (inconclusive — fail and
// preserve) from an impure stream list (delete and re-split), which the
// wrapper collapses into one error.
func verifySplitChunks(job TranscodeJob, ffprobePath string, chunkPaths []string) ([]string, bool, error) {
	var existing []string
	for i, p := range chunkPaths {
		if _, err := os.Stat(p); err != nil {
			job.logf("phase split: split chunk %d missing; re-splitting", i)
			return nil, true, nil
		}
		types, err := probeStreamCodecTypes(ffprobePath, p)
		switch {
		case err != nil:
			return nil, false, fmt.Errorf("split chunk %d stream probe failed: %w", i, err)
		case !isPureVideoStreamList(types):
			job.logf("phase split: existing chunk %d is not pure video (streams %v); re-splitting", i, types)
			return nil, true, nil
		default:
			existing = append(existing, p)
		}
	}
	return existing, false, nil
}

// removeSplitChunks deletes every split-phase chunk (split-*.mkv) in the
// segment directory so a re-split starts from a clean slate: strays left by
// an older, longer split would otherwise survive and trip the post-split
// manifest/count validation.
func removeSplitChunks(paths *jobpaths.JobPaths) {
	glob := strings.Replace(paths.SplitChunkPattern(), "%03d", "*", 1)
	matches, err := filepath.Glob(glob)
	if err != nil {
		return
	}
	for _, m := range matches {
		_ = os.Remove(m)
	}
}

// probeChunkFrames probes an encoded chunk's video stream via ffprobe to return its total frame count.
func probeChunkFrames(ffprobePath, filePath string) int {
	if ffprobePath == "" || filePath == "" {
		return 0
	}
	info, err := ProbeMedia(ffprobePath, filePath)
	if err == nil && info.TotalFrames > 0 {
		return info.TotalFrames
	}
	return 0
}

type progressLatch struct {
	FPS         float64
	Speed       float64
	BitrateKbps int
	ETASeconds  int
	ProgressPct float64
}

// hasFrameRateAlteringFilters reports whether custom flags or video filters
// modify output frame timing or drop/duplicate frames (e.g. framerate changes,
// cadence decimation, or telecine), which would desynchronize frame count from
// source stream presentation duration.
func hasFrameRateAlteringFilters(customFlags string) bool {
	if customFlags == "" {
		return false
	}
	low := strings.ToLower(customFlags)
	ratePatterns := []string{
		"-r ", "-r\t", "-r=", "-r:",
		"-framerate",
		"-fps_mode",
		"-vsync",
		"fps=", "fps:",
		"decimate",
		"mpdecimate",
		"pullup",
		"telecine",
		"fieldmatch",
		"framestep",
		"minterpolate",
		"tblend",
	}
	for _, p := range ratePatterns {
		if strings.Contains(low, p) {
			return true
		}
	}
	for _, tok := range strings.Fields(low) {
		if tok == "-r" || isFPSAffectingRateFlag(tok) {
			return true
		}
	}
	return false
}

func runSinglePass(ctx context.Context, job TranscodeJob, origProgress chan<- Progress, streams []StreamInfo, baseTime, totalDuration float64, accumulatedFrames int, latch progressLatch) (int, progressLatch, error) {
	finalArgs, err := buildFfmpegArgs(job, streams)
	if err != nil {
		return 0, latch, err
	}

	verifyScope := protocol.ScopeEncodeAll
	if specIsChunkEncode(job.Spec) {
		verifyScope = protocol.ScopeEncodeChunk
	}
	if err := protocol.VerifyDefenseInDepth(finalArgs, verifyScope); err != nil {
		return 0, latch, err
	}

	job.recordCommand(finalArgs)

	pin := job.Steering != nil && job.Steering.Enabled && len(job.Steering.CPUs) > 0
	cmd := execCommandContext(ctx, job.FFmpegPath, finalArgs...)

	stderrWriter := &progressWriter{}
	cmd.Stderr = stderrWriter

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, latch, err
	}

	if err := cmd.Start(); err != nil {
		return 0, latch, err
	}
	if pin {
		if err := pinRunningProcess(cmd.Process, job.Steering.CPUs); err != nil {
			// The argv-side steering (pools=/threads) already applies; a
			// failed pin must not kill the encode.
			job.logf("phase encode: CPU affinity pin failed (continuing unpinned): %v", err)
		}
	}

	scanDone := make(chan struct{})
	var lastChunkFrame int

	go func() {
		defer close(scanDone)
		scanner := bufio.NewScanner(stdout)
		var p Progress
		p.Phase = "encode"
		p.TotalFrames = job.TotalFrames
		p.SourceCodec = job.SourceCodec
		p.TargetCodec = job.Codec
		p.Frame = accumulatedFrames
		p.FPS = latch.FPS
		p.Speed = latch.Speed
		p.BitrateKbps = latch.BitrateKbps
		p.ETASeconds = latch.ETASeconds
		p.ProgressPct = latch.ProgressPct

		latchedPct := latch.ProgressPct
		isSinglePass := baseTime == 0 && accumulatedFrames == 0
		canUseFrames := !hasFrameRateAlteringFilters(job.Spec.CustomFlags) && job.TotalFrames > 0 && (isSinglePass || job.ExactFramesKnown)

		updateProgress := func() {
			dur := job.DurationSec
			if totalDuration > 0 {
				dur = totalDuration
			}
			var timePct float64
			if dur > 0 && p.TimeSeconds > 0 {
				timePct = (p.TimeSeconds / dur) * 100.0
				if timePct > 100 {
					timePct = 100
				}
			}

			effectivePct := timePct
			if canUseFrames {
				framePct := (float64(p.Frame) / float64(job.TotalFrames)) * 100.0
				if framePct > 100 {
					framePct = 100
				}
				if framePct < 0 {
					framePct = 0
				}

				if timePct == 0 || timePct > framePct {
					// Copied/muxed audio has buffered ahead of the video encoder,
					// or muxer hasn't emitted initial timestamp yet.
					// Video encoding is the true bottleneck workload; follow the video frame pace
					// so progress doesn't artificially jump ahead and stall.
					effectivePct = framePct
				} else {
					// Normal synchronized encoding or end of file
					effectivePct = timePct
				}
			}

			if effectivePct > latchedPct {
				latchedPct = effectivePct
			}
			p.ProgressPct = latchedPct
			latch.ProgressPct = latchedPct
		}

		for scanner.Scan() {
			line := scanner.Text()
			parts := strings.SplitN(line, "=", 2)
			if len(parts) != 2 {
				continue
			}

			key := parts[0]
			val := parts[1]

			switch key {
			case "frame":
				if i, err := strconv.Atoi(val); err == nil {
					lastChunkFrame = i
					p.Frame = accumulatedFrames + i
					updateProgress()
				}
			case "fps":
				if f, err := strconv.ParseFloat(val, 64); err == nil && f > 0 {
					p.FPS = f
					latch.FPS = f
				}
			case "out_time_us":
				if us, err := strconv.ParseInt(val, 10, 64); err == nil && us >= 0 {
					p.TimeSeconds = float64(us) / 1000000.0
					if baseTime > 0 {
						p.TimeSeconds += baseTime
					}
					updateProgress()
				}
			case "out_time_ms":
				if ms, err := strconv.ParseInt(val, 10, 64); err == nil && ms >= 0 {
					p.TimeSeconds = float64(ms) / 1000.0
					if baseTime > 0 {
						p.TimeSeconds += baseTime
					}
					updateProgress()
				}
			case "out_time":
				pParts := strings.Split(val, ":")
				if len(pParts) == 3 {
					h, _ := strconv.ParseFloat(pParts[0], 64)
					m, _ := strconv.ParseFloat(pParts[1], 64)
					s, _ := strconv.ParseFloat(pParts[2], 64)
					sec := h*3600 + m*60 + s
					if sec >= 0 {
						p.TimeSeconds = sec
						if baseTime > 0 {
							p.TimeSeconds += baseTime
						}
						updateProgress()
					}
				}
			case "speed":
				val = strings.TrimSuffix(val, "x")
				if f, err := strconv.ParseFloat(val, 64); err == nil && f > 0 {
					p.Speed = f
					latch.Speed = f
				}
			case "bitrate":
				if strings.HasSuffix(val, "kbits/s") {
					val = strings.TrimSuffix(val, "kbits/s")
					if f, err := strconv.ParseFloat(val, 64); err == nil && f > 0 {
						p.BitrateKbps = int(f)
						latch.BitrateKbps = int(f)
					}
				}
			case "progress":
				updateProgress()
				dur := job.DurationSec
				if totalDuration > 0 {
					dur = totalDuration
				}
				if canUseFrames && p.FPS > 0 && job.TotalFrames > 0 && job.TotalFrames > p.Frame {
					remainingSec := float64(job.TotalFrames-p.Frame) / p.FPS
					if remainingSec < 0 {
						remainingSec = 0
					}
					p.ETASeconds = int(remainingSec)
					latch.ETASeconds = p.ETASeconds
				} else if p.Speed > 0 && dur > 0 && p.TimeSeconds > 0 {
					remainingSec := (dur - p.TimeSeconds) / p.Speed
					if remainingSec < 0 {
						remainingSec = 0
					}
					p.ETASeconds = int(remainingSec)
					latch.ETASeconds = p.ETASeconds
				}
				if origProgress != nil {
					select {
					case origProgress <- p:
					case <-ctx.Done():
						return
					default:
					}
				}

			}
		}
	}()

	err = cmd.Wait()
	<-scanDone
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return lastChunkFrame, latch, ctxErr
		}
		if stderrTail := tailForError(string(stderrWriter.Bytes()), 2048); stderrTail != "" {
			job.logf("phase encode: ffmpeg stderr: %s", stderrTail)
			return lastChunkFrame, latch, fmt.Errorf("ffmpeg failed (%w): %s", err, stderrTail)
		}
	}
	return lastChunkFrame, latch, err
}

func isValidCodec(c string) bool {
	return protocol.IsValidCodec(c)
}

func isValidPreset(p string) bool {
	return protocol.IsValidPreset(p)
}

func isValidAudioCodec(a string) bool {
	return protocol.IsValidAudioCodec(a)
}
