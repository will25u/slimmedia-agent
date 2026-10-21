package ffmpeg

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/will25u/slimmedia-agent/internal/jobpaths"
	"github.com/will25u/slimmedia-agent/internal/jobspec"
	"github.com/will25u/slimmedia-agent/internal/protocol"
)

// chunkModeJob returns a TranscodeJob shaped exactly like the runner's
// per-chunk encode job: audio and subtitle handling both strip_all. FFmpegPath
// is empty so custom-flag dry-run validation is skipped.
func chunkModeJob(customFlags string) TranscodeJob {
	return TranscodeJob{
		InputFile:  "/media/in.mkv",
		OutputFile: "/media/out.segments/chunk-000.mkv",
		Codec:      "libx264",
		Spec: jobspec.JobSpec{
			Version:          protocol.CurrentJobSpecVersion,
			Container:        "mkv",
			CRF:              22,
			PresetSpeed:      "medium",
			AudioHandling:    "strip_all",
			SubtitleHandling: "strip_all",
			CustomFlags:      customFlags,
		},
	}
}

// singlePassModeJob returns a TranscodeJob shaped like an ordinary single-pass
// encode (audio copied), which must not carry the chunk frame-timing flags.
func singlePassModeJob(customFlags string) TranscodeJob {
	return TranscodeJob{
		InputFile:  "/media/in.mkv",
		OutputFile: "/media/out.mkv",
		Codec:      "libx264",
		Spec: jobspec.JobSpec{
			Version:       protocol.CurrentJobSpecVersion,
			Container:     "mkv",
			CRF:           22,
			PresetSpeed:   "medium",
			AudioHandling: "copy_all",
			CustomFlags:   customFlags,
		},
	}
}

// TestChunkEncodeEmitsFPSModePassthrough: chunk encodes pass frames through at
// source timing by default; single-pass encodes are unaffected.
func TestChunkEncodeEmitsFPSModePassthrough(t *testing.T) {
	chunkArgs, err := buildFfmpegArgs(chunkModeJob(""), nil)
	if err != nil {
		t.Fatalf("chunk buildFfmpegArgs: %v", err)
	}
	if !containsSubsequence(chunkArgs, []string{"-fps_mode", "passthrough"}) {
		t.Errorf("chunk encode must emit -fps_mode passthrough: %q", chunkArgs)
	}

	singleArgs, err := buildFfmpegArgs(singlePassModeJob(""), nil)
	if err != nil {
		t.Fatalf("single-pass buildFfmpegArgs: %v", err)
	}
	if containsArg(singleArgs, "-fps_mode") {
		t.Errorf("single-pass encode must not emit -fps_mode: %q", singleArgs)
	}
}

// TestChunkEncodeUserRateFlagSkipsPassthrough: a user custom flag that sets an
// fps-affecting rate option wins deterministically — the phase's passthrough
// is skipped instead of silently nullifying the user's flag (or vice versa).
func TestChunkEncodeUserRateFlagSkipsPassthrough(t *testing.T) {
	cases := []struct {
		name string
		flag string
		want []string
	}{
		{"-r output rate", "-r 24", []string{"-r", "24"}},
		{"-r:v stream-specified rate", "-r:v 24", []string{"-r:v", "24"}},
		{"-vsync mode", "-vsync cfr", []string{"-vsync", "cfr"}},
		{"-vsync numeric mode", "-vsync 1", []string{"-vsync", "1"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args, err := buildFfmpegArgs(chunkModeJob(tc.flag), nil)
			if err != nil {
				t.Fatalf("buildFfmpegArgs: %v", err)
			}
			if containsArg(args, "-fps_mode") {
				t.Errorf("user rate flag %q must skip the passthrough, got -fps_mode in: %q", tc.flag, args)
			}
			if !containsSubsequence(args, tc.want) {
				t.Errorf("user flag %q must survive into argv: %q", tc.flag, args)
			}
		})
	}
}

// TestChunkEncodeRejectsFPSModeOverride: -fps_mode is a phase-locked chunk
// invariant; a user override collides in chunk scope with a clear error
// instead of silently overriding the passthrough.
func TestChunkEncodeRejectsFPSModeOverride(t *testing.T) {
	for _, flag := range []string{"-fps_mode cfr", "-fps_mode=vfr", "-fps_mode:v passthrough"} {
		t.Run(flag, func(t *testing.T) {
			_, err := buildFfmpegArgs(chunkModeJob(flag), nil)
			if err == nil {
				t.Fatalf("expected chunk-scope collision for %q, got nil", flag)
			}
			if !strings.Contains(err.Error(), "encode_chunk") || !strings.Contains(err.Error(), "immutable") {
				t.Errorf("expected clear chunk-scope immutable error for %q, got: %v", flag, err)
			}
		})
	}
}

// TestSinglePassAllowsFPSModeOverride: outside chunk scope the user keeps full
// control of frame timing — single-pass encodes accept -fps_mode untouched.
func TestSinglePassAllowsFPSModeOverride(t *testing.T) {
	args, err := buildFfmpegArgs(singlePassModeJob("-fps_mode cfr"), nil)
	if err != nil {
		t.Fatalf("single-pass buildFfmpegArgs with -fps_mode cfr: %v", err)
	}
	if !containsSubsequence(args, []string{"-fps_mode", "cfr"}) {
		t.Errorf("single-pass encode must honor user -fps_mode cfr: %q", args)
	}
}

// TestRunTranscodeRejectsFPSModeCustomFlagBeforeAnyPhase: a chunked job whose
// custom flags override the chunk frame-timing invariant must fail at the
// validation gate before any phase work — no demux, split, encode, or concat
// invocation may run.
func TestRunTranscodeRejectsFPSModeCustomFlagBeforeAnyPhase(t *testing.T) {
	tmp := t.TempDir()
	argsFile := filepath.Join(tmp, "args.bin")
	fakeFFmpeg := fakeFFmpegWithSplit(t, tmp, argsFile, 2)
	fakeToolChunkAwareFFprobe(t, tmp)

	progress := make(chan Progress, 16)
	job := TranscodeJob{
		FFmpegPath:   fakeFFmpeg,
		InputFile:    filepath.Join(tmp, "in.mkv"),
		OutputFile:   filepath.Join(tmp, "out.mkv"),
		Codec:        "libx264",
		DurationSec:  600,
		ProgressChan: progress,
		Spec: jobspec.JobSpec{
			Version:     protocol.CurrentJobSpecVersion,
			CustomFlags: "-fps_mode cfr",
		},
	}

	err := runWithTimeout(t, job)
	if err == nil || !strings.Contains(err.Error(), "encode_chunk") {
		t.Fatalf("expected chunk-scope collision failure, got: %v", err)
	}
	// Zero ffmpeg invocations means the collision failed the job before any
	// phase work; the args file only exists once the fake records one.
	if _, statErr := os.Stat(argsFile); !os.IsNotExist(statErr) {
		if invocations := readFakeArgs(t, argsFile); len(invocations) != 0 {
			t.Errorf("collision must fail before any phase work, got %d ffmpeg invocations", len(invocations))
		}
	}
}

// TestRunTranscodeChunkEncodesCarryFPSPassthrough: every chunk encode
// invocation in the segmented pipeline must carry -fps_mode passthrough.
func TestRunTranscodeChunkEncodesCarryFPSPassthrough(t *testing.T) {
	tmp := t.TempDir()
	argsFile := filepath.Join(tmp, "args.bin")
	fakeFFmpeg := fakeFFmpegWithSplit(t, tmp, argsFile, 2)
	fakeToolChunkAwareFFprobe(t, tmp)

	progress := make(chan Progress, 16)
	job := TranscodeJob{
		FFmpegPath:   fakeFFmpeg,
		InputFile:    filepath.Join(tmp, "in.mkv"),
		OutputFile:   filepath.Join(tmp, "out.mkv"),
		Codec:        "libx264",
		DurationSec:  600,
		ProgressChan: progress,
		Spec:         jobspec.JobSpec{Version: protocol.CurrentJobSpecVersion},
	}

	if err := runWithTimeout(t, job); err != nil {
		t.Fatalf("RunTranscode failed: %v", err)
	}

	paths := jobpaths.FromOutput(job.OutputFile)
	chunkInputs := map[string]bool{
		paths.SplitChunk(0): true,
		paths.SplitChunk(1): true,
	}
	chunkEncodes := 0
	for _, inv := range readFakeArgs(t, argsFile) {
		for i, tok := range inv {
			if tok == "-i" && i+1 < len(inv) && chunkInputs[inv[i+1]] {
				chunkEncodes++
				if !containsSubsequence(inv, []string{"-fps_mode", "passthrough"}) {
					t.Errorf("chunk encode invocation missing -fps_mode passthrough: %q", inv)
				}
			}
		}
	}
	if chunkEncodes != 2 {
		t.Errorf("expected 2 chunk encode invocations, got %d", chunkEncodes)
	}
}

// TestRunTranscodeVFRPreservesSourceFrameCountAgainstRealFFmpeg is the
// acceptance check for frame-timing passthrough end to end: a true VFR source
// (2.5s at 24fps + 2.5s at 60fps, nominal average ~42fps) runs through the
// full segmented pipeline with the repo's real ffmpeg, and the stitched
// output must contain exactly the source's frame count — no frames dropped to
// the nominal rate, no frames duplicated at the seams.
func TestRunTranscodeVFRPreservesSourceFrameCountAgainstRealFFmpeg(t *testing.T) {
	ffmpegPath, ffprobePath := repoBinFFmpeg(t)
	tmp := t.TempDir()

	oldSegmentSecs := segmentSecs
	segmentSecs = 2.0
	defer func() { segmentSecs = oldSegmentSecs }()

	// Build the VFR source from two CFR clips concatenated on the demuxer and
	// re-encoded with -fps_mode passthrough, so the container records the
	// real per-frame timing instead of one nominal rate.
	mkClip := func(name string, rate int) string {
		p := filepath.Join(tmp, name)
		cmd := exec.Command(ffmpegPath,
			"-hide_banner", "-nostdin", "-y",
			"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=320x240:rate=%d:duration=2.5", rate),
			"-c:v", "libx264", "-preset", "ultrafast", "-crf", "45", "-g", "25",
			"-an", p,
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("failed to synthesize %s: %v: %s", name, err, out)
		}
		return p
	}
	clipA := mkClip("a.mkv", 24) // 60 frames
	clipB := mkClip("b.mkv", 60) // 150 frames

	listPath := filepath.Join(tmp, "list.txt")
	list := fmt.Sprintf("file '%s'\nfile '%s'\n", clipA, clipB)
	if err := os.WriteFile(listPath, []byte(list), 0644); err != nil {
		t.Fatal(err)
	}

	sourcePath := filepath.Join(tmp, "source.mkv")
	cmd := exec.Command(ffmpegPath,
		"-hide_banner", "-nostdin", "-y",
		"-f", "concat", "-safe", "0", "-i", listPath,
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "45", "-g", "25",
		"-fps_mode", "passthrough",
		"-an", sourcePath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to synthesize VFR source: %v: %s", err, out)
	}

	srcPTS, err := probeVideoPacketPTSTimes(ffprobePath, sourcePath)
	if err != nil {
		t.Fatalf("probe source packet timestamps: %v", err)
	}
	const wantFrames = 210 // 60 @ 24fps + 150 @ 60fps
	if len(srcPTS) != wantFrames {
		t.Fatalf("synthesized source has %d frames, want %d (test asset shape changed)", len(srcPTS), wantFrames)
	}
	// Sanity: the source must really be VFR — a single-rate source would not
	// exercise decimation. The 24fps half spaces frames at ~41.7ms, the 60fps
	// half at ~16.7ms.
	var short, long int
	for i := 1; i < len(srcPTS); i++ {
		if d := srcPTS[i] - srcPTS[i-1]; d < 0.03 {
			short++
		} else if d > 0.035 {
			long++
		}
	}
	if short == 0 || long == 0 {
		t.Fatalf("source is not variable-frame-rate (short gaps=%d, long gaps=%d)", short, long)
	}

	srcInfo, err := ProbeMedia(ffprobePath, sourcePath)
	if err != nil {
		t.Fatalf("probe source: %v", err)
	}

	paths := jobpaths.FromOutput(filepath.Join(tmp, "out.mkv"))
	progress := make(chan Progress, 16)
	job := TranscodeJob{
		FFmpegPath:   ffmpegPath,
		InputFile:    sourcePath,
		OutputFile:   filepath.Join(tmp, "out.mkv"),
		Codec:        "libx264",
		DurationSec:  srcInfo.Duration,
		TotalFrames:  srcInfo.TotalFrames,
		ProgressChan: progress,
		Spec: jobspec.JobSpec{
			Version:       protocol.CurrentJobSpecVersion,
			Container:     "mkv",
			CRF:           45,
			PresetSpeed:   "ultrafast",
			ColorDepth:    "8bit",
			AudioHandling: "copy_all",
		},
		Paths: &paths,
	}

	if err := runWithTimeout(t, job); err != nil {
		t.Fatalf("RunTranscode failed: %v", err)
	}

	outPTS, err := probeVideoPacketPTSTimes(ffprobePath, job.OutputFile)
	if err != nil {
		t.Fatalf("probe output packet timestamps: %v", err)
	}
	if len(outPTS) != len(srcPTS) {
		t.Errorf("stitched VFR frame count = %d, want %d (chunk passthrough must not drop or duplicate frames)",
			len(outPTS), len(srcPTS))
	}
}
