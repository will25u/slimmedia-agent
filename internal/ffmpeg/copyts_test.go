package ffmpeg

import (
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/will25u/slimmedia-agent/internal/jobpaths"
	"github.com/will25u/slimmedia-agent/internal/jobspec"
	"github.com/will25u/slimmedia-agent/internal/protocol"
)

// TestFullStreamEncodeEmitsCopyTS is the argv-shape half of the #200 fix: the
// full-stream (single-pass) encode shape carries -copyts so the muxer cannot
// rebase per-stream start times relative to the first packet — the rebase
// that surfaced as a one-frame A/V offset shift on hevc_qsv jobs. The chunked
// shape must NOT carry it: segment timing is owned by the split/concat
// phases, and the concat assembly applies -copyts to the demux sidecar
// itself (#192).
func TestFullStreamEncodeEmitsCopyTS(t *testing.T) {
	singleArgs, err := buildFfmpegArgs(singlePassModeJob(""), nil)
	if err != nil {
		t.Fatalf("single-pass buildFfmpegArgs: %v", err)
	}
	if !containsArg(singleArgs, "-copyts") {
		t.Errorf("full-stream encode must emit -copyts: %q", singleArgs)
	}

	chunkArgs, err := buildFfmpegArgs(chunkModeJob(""), nil)
	if err != nil {
		t.Fatalf("chunk buildFfmpegArgs: %v", err)
	}
	if containsArg(chunkArgs, "-copyts") {
		t.Errorf("chunk encode must not emit -copyts: %q", chunkArgs)
	}
}

// TestEncodeRejectsCopyTSCustomFlag: -copyts is platform-owned timestamp
// plumbing (full-stream encode argv and concat sidecar input), so a user
// custom flag carrying it must collide with a clear error instead of
// duplicating the flag.
func TestEncodeRejectsCopyTSCustomFlag(t *testing.T) {
	for _, job := range []TranscodeJob{singlePassModeJob("-copyts"), chunkModeJob("-copyts")} {
		_, err := buildFfmpegArgs(job, nil)
		if err == nil {
			t.Fatalf("expected -copyts custom flag to be rejected, got nil")
		}
		if !strings.Contains(err.Error(), "forbidden token") {
			t.Errorf("expected forbidden-token error for -copyts, got: %v", err)
		}
	}
}

// TestRunTranscodeSinglePassPreservesAVOffset is the behavioral half of the
// #200 fix: a source whose audio track deliberately starts +0.6s late runs
// through the real-ffmpeg full-stream single-pass pipeline, and the output
// must keep that audio/video start-time offset. The recorded encode argv must
// show the -copyts that makes preservation happen.
func TestRunTranscodeSinglePassPreservesAVOffset(t *testing.T) {
	ffmpegPath, ffprobePath := repoBinFFmpeg(t)
	tmp := t.TempDir()
	sourcePath, audioDelay := synthesizeOffsetSource(t, ffmpegPath, tmp)

	argsFile := filepath.Join(tmp, "args.bin")
	wrappedFFmpeg := recordingFFmpeg(t, ffmpegPath, ffprobePath, tmp, argsFile)

	srcInfo, err := ProbeMedia(ffprobePath, sourcePath)
	if err != nil {
		t.Fatalf("probe source: %v", err)
	}
	srcStarts := probeStreamStartTimes(t, ffprobePath, sourcePath)
	if got := srcStarts["audio"]; math.Abs(got-audioDelay) > 0.05 {
		t.Fatalf("source audio start_time = %f, want ~%v (itsoffset application failed)", got, audioDelay)
	}
	srcFirstSub := probeFirstPacketPTS(t, ffprobePath, sourcePath, "subtitle")

	paths := jobpaths.FromOutput(filepath.Join(tmp, "out.mkv"))
	progress := make(chan Progress, 16)
	job := TranscodeJob{
		FFmpegPath:   wrappedFFmpeg,
		InputFile:    sourcePath,
		OutputFile:   filepath.Join(tmp, "out.mkv"),
		Codec:        "libx264",
		DurationSec:  srcInfo.Duration,
		TotalFrames:  srcInfo.TotalFrames,
		ProgressChan: progress,
		Spec: jobspec.JobSpec{
			Version:       protocol.CurrentJobSpecVersion,
			Container:     "mkv",
			CRF:           40,
			PresetSpeed:   "ultrafast",
			ColorDepth:    "8bit",
			AudioHandling: "copy_all",
			Chunking:      jobspec.ChunkingSinglePass,
		},
		Paths: &paths,
	}

	if err := runWithTimeout(t, job); err != nil {
		t.Fatalf("RunTranscode failed: %v", err)
	}

	// Criterion: the full-stream encode carries -copyts so per-stream
	// timestamps pass through unmodified.
	invocations := readFakeArgs(t, argsFile)
	if len(invocations) != 1 {
		t.Fatalf("expected 1 ffmpeg invocation (full-stream encode), got %d: %q", len(invocations), invocations)
	}
	assertHasArg(t, invocations[0], "-copyts")

	// Criterion: the output preserves the source's audio/video offset —
	// audio still starts ~0.6s late relative to video, and the first
	// subtitle cue still lands at its delayed time. This is the invariant
	// the ±40ms alignment gate (#193) verifies on every completed job.
	outStarts := probeStreamStartTimes(t, ffprobePath, job.OutputFile)
	if got := outStarts["video"]; got > 0.05 {
		t.Errorf("output video start_time = %f, want ~0", got)
	}
	if got := outStarts["audio"]; math.Abs(got-audioDelay) > 0.05 {
		t.Errorf("output audio start_time = %f, want ~%v (A/V offset annihilated)", got, audioDelay)
	}
	if got := probeFirstPacketPTS(t, ffprobePath, job.OutputFile, "subtitle"); math.Abs(got-srcFirstSub) > 0.05 {
		t.Errorf("output first subtitle packet pts = %f, want ~%v (cue offset not preserved)", got, srcFirstSub)
	}

	outInfo, err := ProbeMedia(ffprobePath, job.OutputFile)
	if err != nil {
		t.Fatalf("probe output: %v", err)
	}
	srcOffset, srcOK := srcInfo.AVOffset()
	outOffset, outOK := outInfo.AVOffset()
	if !srcOK || !outOK {
		t.Fatalf("AVOffset metadata missing: srcOK=%v outOK=%v", srcOK, outOK)
	}
	if delta := math.Abs(outOffset - srcOffset); delta > 0.060 {
		t.Errorf("output A/V offset %.3fs differs from input %.3fs by %.0fms (>±60ms)",
			outOffset, srcOffset, delta*1000)
	}
}
