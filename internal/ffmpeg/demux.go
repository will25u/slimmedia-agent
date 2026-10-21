package ffmpeg

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/will25u/slimmedia-agent/internal/jobpaths"
	"github.com/will25u/slimmedia-agent/internal/protocol"
)

// demuxDurationTolerance is the allowed per-track duration drift between the
// source streams and the demuxed scratch container. Stream copy is lossless;
// this only absorbs muxer-level rounding (e.g. MKV timecode granularity).
const demuxDurationTolerance = 1.0

// demuxErrorTailBytes bounds the process output embedded in a demux failure
// error. The interesting signal is at the end of the stderr (muxer errors,
// fatal lines); a demux pass also logs one probe warning per stream, so an
// unbounded copy turns the job error into kilobytes of repetition.
const demuxErrorTailBytes = 512

// tailForError keeps the trailing bytes of process output for an error
// message, prefixed with an ellipsis marker when truncated.
func tailForError(output string, maxBytes int) string {
	output = strings.TrimSpace(output)
	if len(output) <= maxBytes {
		return output
	}
	return "…" + output[len(output)-maxBytes:]
}

// buildDemuxArgs composes the upfront stream-copy extraction pass: one
// unbroken read of the source that copies every audio stream and every
// Matroska-muxable subtitle stream plus chapter markers into a local scratch
// container. subtitleIndexes are source subtitle indexes (0:s:N) selected by
// the caller; subtitle codecs the Matroska muxer cannot hold (mov_text) are
// skipped by the caller and logged, mirroring the pipeline's existing MP4
// bitmap-subtitle handling. Optional audio maps (0:a?) let the command
// succeed on sources with no audio, but callers skip the pass entirely when
// the source probe shows no audio or muxable subtitle streams at all — a
// container with zero streams is not muxable.
func buildDemuxArgs(sourcePath, demuxPath string, subtitleIndexes []int) []string {
	args := []string{
		"-hide_banner",
		"-nostdin",
		"-nostats",
		"-y",
		"-probesize", "10000000",
		"-analyzeduration", "10000000",
		"-i", sourcePath,
		"-map", "0:a?",
	}
	for _, i := range subtitleIndexes {
		args = append(args, "-map", streamRef("0:s", i))
	}
	args = append(args,
		"-map_metadata", "0", "-map_chapters", "0",
		"-c", "copy",
		demuxPath,
	)
	return args
}

// matroskaUnsupportedSubtitleCodecs lists subtitle codecs the Matroska
// muxer refuses (stream copy fails at header write). Tracks using them are
// excluded from the demux container so the rest of the extraction still
// succeeds; like the pipeline's MP4 bitmap-subtitle handling, such tracks
// are dropped from the output rather than read from the source a second
// time.
var matroskaUnsupportedSubtitleCodecs = map[string]bool{
	"mov_text": true,
}

// demuxableSubtitleIndexes partitions the probed subtitle streams into the
// source indexes (0:s:N) the scratch container can hold, logging each skip.
func demuxableSubtitleIndexes(job TranscodeJob, streams []StreamInfo) []int {
	subs := streamsOfType(streams, "subtitle")
	var muxable []int
	for i, s := range subs {
		if matroskaUnsupportedSubtitleCodecs[strings.ToLower(s.Codec)] {
			job.logf("dropping subtitle stream 0:s:%d (%s): scratch container (matroska) cannot mux it", i, s.Codec)
			continue
		}
		muxable = append(muxable, i)
	}
	return muxable
}

// extractSidecarStreams runs Phase 0 of the segmented pipeline: a single
// stream-copy pass over the source that extracts all audio, subtitle, and
// chapter streams into the job's local scratch container, so no later phase
// needs to re-read the (possibly network-mounted) source for them. The
// container is verified with ffprobe before the phase is considered done;
// a mismatch fails the job rather than silently dropping tracks. An existing
// container that verifies is reused (crash-resume) instead of re-reading
// the source.
func extractSidecarStreams(ctx context.Context, job TranscodeJob, paths *jobpaths.JobPaths, streams []StreamInfo) error {
	subs := streamsOfType(streams, "subtitle")
	muxableSubIdx := demuxableSubtitleIndexes(job, streams)
	expected := streamsOfType(streams, "audio")
	for _, i := range muxableSubIdx {
		expected = append(expected, subs[i])
	}
	if len(expected) == 0 {
		// No audio/subtitle streams: the demux container cannot be built
		// from zero streams, but chapter markers (if any) still need a local
		// home so final assembly never re-opens the source for them.
		return extractChapterSidecar(ctx, job, paths)
	}

	ffprobePath := ffprobeNextTo(job.FFmpegPath)
	demuxPath := paths.DemuxContainer()
	sourceChapters := ProbeChapterCount(ffprobePath, job.InputFile)

	if _, err := os.Stat(demuxPath); err == nil {
		if err := verifyDemuxContainer(ffprobePath, expected, sourceChapters, demuxPath); err == nil {
			job.logf("phase demux: reusing existing verified scratch container")
			return nil
		} else {
			job.logf("phase demux: existing scratch container failed verification (%v); re-extracting", err)
			_ = os.Remove(demuxPath)
		}
	}

	job.logf("phase demux: extracting audio/subtitle/chapter streams to local scratch container")
	args := buildDemuxArgs(job.InputFile, demuxPath, muxableSubIdx)
	if err := protocol.VerifyDefenseInDepth(args, protocol.ScopeDemux); err != nil {
		return err
	}
	totalStreams := len(expected)
	startTime := time.Now()
	sendJobProgress(job, Progress{
		Phase:        "demux",
		CurrentIndex: 1,
		TotalCount:   totalStreams,
		ElapsedSecs:  0,
		ProgressPct:  0,
	})

	pw := &progressWriter{}
	cmd := execCommandContext(ctx, job.FFmpegPath, args...)
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("source demux failed to start: %w", err)
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
				lastT := pw.LastTime()
				curStream := 1
				if job.DurationSec > 0 && lastT > 0 {
					curStream = int((lastT/job.DurationSec)*float64(totalStreams)) + 1
					if curStream > totalStreams {
						curStream = totalStreams
					}
				}
				sendJobProgress(job, Progress{
					Phase:        "demux",
					CurrentIndex: curStream,
					TotalCount:   totalStreams,
					ElapsedSecs:  elapsed,
				})
			}
		}
	}()

	err := cmd.Wait()
	close(done)

	if err != nil {
		// A kill from CommandContext (CANCEL_JOB, daemon shutdown) surfaces
		// as a generic process error — on Windows the terminated process
		// reports "exit status 1", which is indistinguishable from a real
		// demux failure. Attribute the outcome to the cancellation instead,
		// without the process's stderr: the caller (orchestrator) keys its
		// cancelled-vs-failed handling off ctx.Err(), and a cancelled job's
		// error must not carry kilobytes of probe noise into the UI.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("source demux stopped: %w", ctxErr)
		}
		return fmt.Errorf("source demux failed: %w: %s", err, tailForError(string(pw.Bytes()), demuxErrorTailBytes))
	}

	sendJobProgress(job, Progress{
		Phase:        "demux",
		CurrentIndex: totalStreams,
		TotalCount:   totalStreams,
		ElapsedSecs:  time.Since(startTime).Seconds(),
	})
	if err := verifyDemuxContainer(ffprobePath, expected, sourceChapters, demuxPath); err != nil {
		_ = os.Remove(demuxPath)
		return err
	}
	job.logf("phase demux: scratch container verified (%d streams probed)", len(expected))
	return nil
}

// verifyDemuxContainer checks the local scratch container against the
// probed source: track counts, codecs, and (when both probes expose them)
// per-track durations must match, and the chapter count must be preserved.
// A nil sourceStreams (source probe failed) degrades to an existence probe,
// matching ProbeStreams' degrade-don't-fail contract.
func verifyDemuxContainer(ffprobePath string, sourceStreams []StreamInfo, sourceChapters int, demuxPath string) error {
	fi, err := os.Stat(demuxPath)
	if err != nil || fi.Size() == 0 {
		return fmt.Errorf("demux scratch container %s missing or empty", demuxPath)
	}

	demuxStreams := ProbeStreams(ffprobePath, demuxPath)
	if demuxStreams == nil {
		return fmt.Errorf("demux scratch container %s failed ffprobe verification", demuxPath)
	}
	if sourceStreams == nil {
		return nil
	}

	for _, streamType := range []string{"audio", "subtitle"} {
		if err := compareDemuxStreamGroup(streamType, streamsOfType(sourceStreams, streamType), streamsOfType(demuxStreams, streamType)); err != nil {
			return err
		}
	}

	if sourceChapters >= 0 {
		if demuxChapters := ProbeChapterCount(ffprobePath, demuxPath); demuxChapters >= 0 && demuxChapters != sourceChapters {
			return fmt.Errorf("demux scratch container chapter count %d does not match source %d", demuxChapters, sourceChapters)
		}
	}
	return nil
}

// compareDemuxStreamGroup verifies that the tracks of one type (audio or
// subtitle) survived extraction intact: same count, same codec per
// position, and matching durations when both sides report them.
func compareDemuxStreamGroup(streamType string, source, demuxed []StreamInfo) error {
	if len(source) != len(demuxed) {
		return fmt.Errorf("demux scratch container %s track count %d does not match source %d", streamType, len(demuxed), len(source))
	}
	for i, s := range source {
		d := demuxed[i]
		if d.Codec != s.Codec {
			return fmt.Errorf("demux scratch container %s track %d codec %s does not match source %s", streamType, i, d.Codec, s.Codec)
		}
		if s.Duration > 0 && d.Duration > 0 {
			if diff := d.Duration - s.Duration; diff < -demuxDurationTolerance || diff > demuxDurationTolerance {
				return fmt.Errorf("demux scratch container %s track %d duration %.2f differs from source %.2f by >%.2fs", streamType, i, d.Duration, s.Duration, demuxDurationTolerance)
			}
		}
	}
	return nil
}

func streamsOfType(streams []StreamInfo, streamType string) []StreamInfo {
	var out []StreamInfo
	for _, s := range streams {
		if s.Type == streamType {
			out = append(out, s)
		}
	}
	return out
}

// extractChapterSidecar handles the chapters-only variant of Phase 0: the
// source has no audio or muxable subtitle streams, so there is no demux
// container, but chapter markers still need a local home for final assembly
// (the demux command cannot mux a zero-stream container). The chapters are
// probed from the source and written to a local ffmetadata file; an existing
// file is reused (crash-resume). Probe failures degrade to skipping the
// sidecar, matching the metadata probes' degrade-don't-fail contract — a
// missing chapter sidecar loses markers but never the job.
func extractChapterSidecar(ctx context.Context, job TranscodeJob, paths *jobpaths.JobPaths) error {
	chapterPath := paths.ChapterFile()
	if _, err := os.Stat(chapterPath); err == nil {
		job.logf("phase demux: reusing existing chapter metadata file")
		return nil
	}

	chapters := ProbeChapters(ffprobeNextTo(job.FFmpegPath), job.InputFile)
	if len(chapters) == 0 {
		job.logf("phase demux: no audio/subtitle streams and no chapters in source; skipping extraction")
		return nil
	}

	if err := writeChapterMetadata(ctx, chapterPath, chapters); err != nil {
		return fmt.Errorf("chapter sidecar extraction failed: %w", err)
	}
	job.logf("phase demux: extracted %d chapter markers to local metadata file", len(chapters))
	return nil
}

// writeChapterMetadata renders chapters into ffmpeg's ffmetadata format so
// final assembly can map them with -map_chapters without re-reading the
// source. Titles are sanitized for the format's special characters
// (= ; # \ and newline).
func writeChapterMetadata(_ context.Context, path string, chapters []Chapter) error {
	var b strings.Builder
	b.WriteString(";FFMETADATA1\n")
	for _, c := range chapters {
		b.WriteString("[CHAPTER]\nTIMEBASE=1/1000\n")
		fmt.Fprintf(&b, "START=%d\nEND=%d\n", c.Start, c.End)
		if title := sanitizeChapterTitle(c.Title); title != "" {
			b.WriteString("title=" + title + "\n")
		}
	}
	if err := os.WriteFile(path, []byte(b.String()), 0644); err != nil {
		return err
	}
	return nil
}

func sanitizeChapterTitle(title string) string {
	replacer := strings.NewReplacer(
		"\n", " ",
		"\r", " ",
		"\\", "\\\\",
		"=", "\\=",
		";", "\\;",
		"#", "\\#",
	)
	return replacer.Replace(title)
}
