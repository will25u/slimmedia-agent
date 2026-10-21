package ffmpeg

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/will25u/slimmedia-agent/internal/jobpaths"
	"github.com/will25u/slimmedia-agent/internal/jobspec"
	"github.com/will25u/slimmedia-agent/internal/protocol"
)

// TestRunTranscodeSinglePassAgainstRealFFmpeg is the end-to-end acceptance
// check for the engine-conditional single-pass path (#194): a real source
// carrying audio, subtitle, and chapter streams is encoded in exactly one
// ffmpeg invocation — no demux, no segment-muxer split, no concat — and the
// output retains every stream. The spec-level override pins the single-pass
// shape because this machine resolves software encoders (which stay chunked).
func TestRunTranscodeSinglePassAgainstRealFFmpeg(t *testing.T) {
	ffmpegPath, ffprobePath := repoBinFFmpeg(t)
	tmp := t.TempDir()
	sourcePath := synthesizeSidecarSource(t, ffmpegPath, tmp)

	argsFile := filepath.Join(tmp, "args.bin")
	wrappedFFmpeg := recordingFFmpeg(t, ffmpegPath, ffprobePath, tmp, argsFile)

	srcInfo, err := ProbeMedia(ffprobePath, sourcePath)
	if err != nil {
		t.Fatalf("probe source: %v", err)
	}

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

	// Criterion: exactly one ffmpeg invocation — the encode itself. No
	// demux, split, or concat preflight phases exist on this path.
	invocations := readFakeArgs(t, argsFile)
	if len(invocations) != 1 {
		t.Fatalf("expected 1 ffmpeg invocation (full-stream encode), got %d: %q", len(invocations), invocations)
	}
	encode := invocations[0]
	for _, tok := range encode {
		if tok == "segment" || tok == "concat" {
			t.Fatalf("single-pass encode must not use the %s muxer: %q", tok, encode)
		}
	}
	assertHasArg(t, encode, "-c:a")
	assertHasArg(t, encode, "copy")
	if _, err := os.Stat(paths.SegmentDir()); !os.IsNotExist(err) {
		t.Errorf("single-pass job must not create the segment directory, but %s exists", paths.SegmentDir())
	}

	// Criterion: the output retains all source streams — audio (2), text
	// subtitle (1), and chapters (2) — and matches the source duration.
	outStreams := ProbeStreams(ffprobePath, job.OutputFile)
	audio, subtitle := 0, 0
	for _, s := range outStreams {
		switch s.Type {
		case "audio":
			audio++
		case "subtitle":
			subtitle++
		}
	}
	if audio != 2 {
		t.Errorf("output audio track count = %d, want 2", audio)
	}
	if subtitle != 1 {
		t.Errorf("output subtitle track count = %d, want 1", subtitle)
	}
	if got := ProbeChapterCount(ffprobePath, job.OutputFile); got != 2 {
		t.Errorf("output chapter count = %d, want 2", got)
	}

	outInfo, err := ProbeMedia(ffprobePath, job.OutputFile)
	if err != nil {
		t.Fatalf("probe output: %v", err)
	}
	if diff := math.Abs(outInfo.VideoStreamDuration - srcInfo.VideoStreamDuration); diff > 0.5 {
		t.Errorf("output video stream duration %.2f differs from source %.2f by %.2fs (>0.5s)",
			outInfo.VideoStreamDuration, srcInfo.VideoStreamDuration, diff)
	}

	// Criterion: whole-job progress was reported as one encode phase.
	if len(progress) == 0 {
		t.Errorf("expected whole-job progress events on the progress channel")
	}
	for p := range progress {
		if p.Phase != "" && p.Phase != "encode" {
			t.Errorf("unexpected preflight phase %q on the single-pass path", p.Phase)
		}
	}
}
