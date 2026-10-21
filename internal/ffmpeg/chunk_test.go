package ffmpeg

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/will25u/slimmedia-agent/internal/jobpaths"
	"github.com/will25u/slimmedia-agent/internal/jobspec"
	"github.com/will25u/slimmedia-agent/internal/protocol"
)

// scriptedStreamTypesFFprobe writes an ffprobe fake that always emits the
// given stream codec_types as ffprobe's -show_entries stream=codec_type JSON.
func scriptedStreamTypesFFprobe(t *testing.T, dir string, codecTypes ...string) string {
	t.Helper()
	var streams strings.Builder
	for i, ct := range codecTypes {
		if i > 0 {
			streams.WriteString(",")
		}
		fmt.Fprintf(&streams, `{"codec_type":%q}`, ct)
	}
	script := "#!/bin/sh\ncat <<'EOF'\n" +
		`{"streams":[` + streams.String() + "]}\nEOF\n"
	return fakeTool(t, dir, "ffprobe", script)
}

func TestVerifyChunkVideoOnlyAcceptsPureVideo(t *testing.T) {
	tmp := t.TempDir()
	ffprobe := scriptedStreamTypesFFprobe(t, tmp, "video")

	if err := verifyChunkVideoOnly(ffprobe, tmp+"/chunk-000.mkv"); err != nil {
		t.Fatalf("verifyChunkVideoOnly: %v", err)
	}
}

func TestVerifyChunkVideoOnlyRejectsNonVideoStreams(t *testing.T) {
	cases := map[string][]string{
		"audio alongside video":      {"video", "audio"},
		"subtitle alongside video":   {"video", "subtitle"},
		"data alongside video":       {"video", "data"},
		"attachment alongside video": {"video", "attachment"},
		"audio only":                 {"audio"},
		"no streams":                 {},
	}
	for name, types := range cases {
		t.Run(name, func(t *testing.T) {
			tmp := t.TempDir()
			ffprobe := scriptedStreamTypesFFprobe(t, tmp, types...)

			err := verifyChunkVideoOnly(ffprobe, tmp+"/chunk-000.mkv")
			if err == nil || !strings.Contains(err.Error(), "want exactly one video stream") {
				t.Fatalf("expected video-only rejection, got: %v", err)
			}
		})
	}
}

func TestVerifyChunkVideoOnlySurfacesProbeFailure(t *testing.T) {
	tmp := t.TempDir()
	ffprobe := fakeTool(t, tmp, "ffprobe", "#!/bin/sh\nprintf 'probe exploded\n' >&2\nexit 1\n")

	err := verifyChunkVideoOnly(ffprobe, tmp+"/chunk-000.mkv")
	if err == nil || !strings.Contains(err.Error(), "stream probe failed") {
		t.Fatalf("expected probe failure, got: %v", err)
	}
}

// TestRunTranscodeReEncodesStaleNonVideoOnlyChunk: a chunk left behind by an
// older agent (audio-bearing segments) must not be reused on resume. The
// runner must fail its video-only verification, delete it, and re-encode.
func TestRunTranscodeReEncodesStaleNonVideoOnlyChunk(t *testing.T) {
	tmp := t.TempDir()
	argsFile := filepath.Join(tmp, "args.bin")
	fakeFFmpeg := fakeFFmpegWithSplit(t, tmp, argsFile, 2)

	// The first probe of each chunk path reports the old audio-bearing layout;
	// every later probe (after re-encode) reports the pure-video layout.
	probeScript := `#!/bin/sh
last=""
for a in "$@"; do last="$a"; done
case "$last" in
*/chunk-000.mkv)
marker="` + tmp + `/probed-stale-chunk"
if [ -f "$marker" ]; then
printf '%s\n' '` + fakeChunkOnlyStreamJSON + `'
else
touch "$marker"
cat <<'EOF'
{"streams":[
 {"codec_type":"video","codec_name":"hevc"},
 {"codec_type":"audio","codec_name":"truehd","disposition":{"default":1},"tags":{"language":"eng"}}
]}
EOF
fi
;;
*/chunk-*.mkv|*/split-*.mkv)
printf '%s\n' '` + fakeChunkOnlyStreamJSON + `'
;;
*)
cat <<'EOF'
{"streams":[
 {"codec_type":"video","codec_name":"hevc"},
 {"codec_type":"audio","codec_name":"truehd","disposition":{"default":1},"tags":{"language":"eng"}},
 {"codec_type":"subtitle","codec_name":"subrip","disposition":{"default":0},"tags":{"language":"eng"}}
]}
EOF
;;
esac
`
	fakeTool(t, tmp, "ffprobe", probeScript)

	paths := jobpaths.FromOutput(filepath.Join(tmp, "out.mkv"))
	if err := os.MkdirAll(paths.SegmentDir(), 0755); err != nil {
		t.Fatal(err)
	}
	// Pre-existing split chunks make Phase 1 resume silently; the stale
	// encoded chunk 0 simulates an interrupted pre-#152 job.
	for i := 0; i < 2; i++ {
		if err := os.WriteFile(paths.SplitChunk(i), []byte("split"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(paths.EncodedChunk(0), []byte("stale audio-bearing chunk"), 0644); err != nil {
		t.Fatal(err)
	}

	caps := EncoderCaps{HasLibx265: true, RecommendedEncoder: "libx265"}
	progress := make(chan Progress, 16)
	job := TranscodeJob{
		FFmpegPath:  fakeFFmpeg,
		InputFile:   filepath.Join(tmp, "in.mkv"),
		OutputFile:  filepath.Join(tmp, "out.mkv"),
		Codec:       "libx264",
		DurationSec: 600, // 2 chunks of 300s
		Spec: jobspec.JobSpec{
			Version: protocol.CurrentJobSpecVersion,
		},
		Caps:         &caps,
		ProgressChan: progress,
	}

	if err := runWithTimeout(t, job); err != nil {
		t.Fatalf("RunTranscode failed: %v", err)
	}
	assertChannelClosed(t, progress)

	// The stale chunk must have been re-encoded: a chunk-0 encode invocation
	// (input = split-000.mkv) exists alongside the demux, chunk-1, and concat
	// invocations. Split resumed from disk, so it contributes no invocation.
	invocations := readFakeArgs(t, argsFile)
	chunk0Encodes := 0
	for _, inv := range invocations {
		for i, tok := range inv {
			if tok == "-i" && i+1 < len(inv) && inv[i+1] == paths.SplitChunk(0) {
				chunk0Encodes++
			}
		}
	}
	if chunk0Encodes != 1 {
		t.Fatalf("stale chunk 0 must be re-encoded exactly once, got %d chunk-0 encode invocations across %d invocations", chunk0Encodes, len(invocations))
	}
}

// TestRunTranscodeChunksAreVideoOnlyAgainstRealFFmpeg is the acceptance check
// for video-only chunk encoding end to end: a source carrying audio,
// subtitle, and chapter streams runs through the full segmented pipeline with
// the repo's real ffmpeg, every encoded chunk must probe as exactly one video
// stream, and the concatenated output's video duration must match the source.
func TestRunTranscodeChunksAreVideoOnlyAgainstRealFFmpeg(t *testing.T) {
	ffmpegPath, ffprobePath := repoBinFFmpeg(t)
	tmp := t.TempDir()

	srtPath := filepath.Join(tmp, "subs.srt")
	if err := os.WriteFile(srtPath, []byte("1\n00:00:00,200 --> 00:00:01,200\nHello from the chunk test\n\n"), 0644); err != nil {
		t.Fatal(err)
	}
	metaPath := filepath.Join(tmp, "chapters.txt")
	meta := ";FFMETADATA1\n[CHAPTER]\nTIMEBASE=1/1000\nSTART=0\nEND=1000\ntitle=Opening\n[CHAPTER]\nTIMEBASE=1/1000\nSTART=1000\nEND=2000\ntitle=End\n"
	if err := os.WriteFile(metaPath, []byte(meta), 0644); err != nil {
		t.Fatal(err)
	}

	sourcePath := filepath.Join(tmp, "source.mkv")
	cmd := exec.Command(ffmpegPath,
		"-hide_banner", "-nostdin", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=554:sample_rate=44100:duration=2",
		"-i", srtPath,
		"-i", metaPath,
		"-map", "0:v", "-map", "1:a", "-map", "2:a", "-map", "3:s",
		"-map_metadata", "-1", "-map_chapters", "4",
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "40",
		"-c:a", "aac",
		"-c:s", "copy",
		sourcePath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to synthesize source: %v: %s", err, out)
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
			CRF:           40,
			PresetSpeed:   "ultrafast",
			ColorDepth:    "8bit",
			AudioHandling: "copy_all",
		},
		Paths: &paths,
	}

	if err := runWithTimeout(t, job); err != nil {
		t.Fatalf("RunTranscode failed: %v", err)
	}

	// Criterion: the split output itself is pure video — the segment muxer
	// maps only the video stream, so no audio/subtitle/data streams reach the
	// chunk encodes at all.
	splitPath := paths.SplitChunk(0)
	if _, err := os.Stat(splitPath); err != nil {
		t.Fatalf("split chunk missing: %v", err)
	}
	if err := verifyChunkVideoOnly(ffprobePath, splitPath); err != nil {
		t.Fatalf("split chunk video-only verification: %v", err)
	}

	// Criterion: every chunk output contains exactly one video stream and
	// nothing else. (The runner already verified this per chunk; re-assert
	// here as the acceptance probe.)
	chunkPath := paths.EncodedChunk(0)
	if _, err := os.Stat(chunkPath); err != nil {
		t.Fatalf("encoded chunk missing: %v", err)
	}
	if err := verifyChunkVideoOnly(ffprobePath, chunkPath); err != nil {
		t.Fatalf("chunk video-only verification: %v", err)
	}

	// Criterion: the concatenated output's video duration still matches the
	// source (single-chunk job, so no seam tolerance is needed).
	outInfo, err := ProbeMedia(ffprobePath, job.OutputFile)
	if err != nil {
		t.Fatalf("probe output: %v", err)
	}
	if diff := math.Abs(outInfo.VideoStreamDuration - srcInfo.VideoStreamDuration); diff > 0.5 {
		t.Errorf("output video stream duration %.2f differs from source %.2f by %.2fs (>0.5s)", outInfo.VideoStreamDuration, srcInfo.VideoStreamDuration, diff)
	}

	// Criterion: a multi-audio source still delivers every audio track in the
	// final output. Audio must reach the output exclusively through the
	// whole-file demux pass — never through the video-only split chunks.
	countType := func(streams []StreamInfo, typ string) int {
		n := 0
		for _, s := range streams {
			if s.Type == typ {
				n++
			}
		}
		return n
	}
	srcStreams := ProbeStreams(ffprobePath, sourcePath)
	outStreams := ProbeStreams(ffprobePath, job.OutputFile)
	if got, want := countType(outStreams, "audio"), countType(srcStreams, "audio"); got != want {
		t.Errorf("output has %d audio streams, want %d (every source track must survive via the demux pass)", got, want)
	}
}

// TestRunTranscodeMultiChunkVideoOnlyConcatDurationAgainstRealFFmpeg covers
// the seam-tolerance acceptance criterion with real media: a source longer
// than two chunk durations is split into three chunks (two seams), every
// chunk must verify as pure video, and the concatenated output's video
// duration must stay within the job-level duration gate (2s).
func TestRunTranscodeMultiChunkVideoOnlyConcatDurationAgainstRealFFmpeg(t *testing.T) {
	ffmpegPath, ffprobePath := repoBinFFmpeg(t)
	tmp := t.TempDir()

	oldSegmentSecs := segmentSecs
	segmentSecs = 2.0
	defer func() { segmentSecs = oldSegmentSecs }()

	const srcDur = 5 // 3 chunks of 2s/2s/1s — two real seams
	sourcePath := filepath.Join(tmp, "source.mkv")
	cmd := exec.Command(ffmpegPath,
		"-hide_banner", "-nostdin", "-y",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=320x240:rate=25:duration=%d", srcDur),
		"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:sample_rate=48000:duration=%d", srcDur),
		"-map", "0:v", "-map", "1:a",
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "45", "-g", "25",
		"-c:a", "aac",
		sourcePath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to synthesize source: %v: %s", err, out)
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

	done := make(chan error, 1)
	go func() { done <- RunTranscode(t.Context(), job) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunTranscode failed: %v", err)
		}
	case <-time.After(120 * time.Second):
		t.Fatal("RunTranscode did not finish within 120s")
	}

	// Three chunks must exist and each must be pure video.
	for i := 0; i < 3; i++ {
		chunkPath := paths.EncodedChunk(i)
		if _, err := os.Stat(chunkPath); err != nil {
			t.Fatalf("encoded chunk %d missing: %v", i, err)
		}
		if err := verifyChunkVideoOnly(ffprobePath, chunkPath); err != nil {
			t.Errorf("chunk %d video-only verification: %v", i, err)
		}
	}
	if _, err := os.Stat(paths.EncodedChunk(3)); !os.IsNotExist(err) {
		t.Errorf("unexpected fourth chunk exists: %v", err)
	}

	// Seam tolerance: concatenated video duration vs source, same 2s gate the
	// orchestrator's duration verification applies.
	outInfo, err := ProbeMedia(ffprobePath, job.OutputFile)
	if err != nil {
		t.Fatalf("probe output: %v", err)
	}
	if outInfo.VideoStreamDuration <= 0 || srcInfo.VideoStreamDuration <= 0 {
		t.Fatalf("video stream durations must be reported by both probes: out=%.2f src=%.2f", outInfo.VideoStreamDuration, srcInfo.VideoStreamDuration)
	}
	if diff := outInfo.VideoStreamDuration - srcInfo.VideoStreamDuration; diff < -2.0 || diff > 2.0 {
		t.Errorf("concatenated video stream duration %.2f differs from source %.2f by >2s (seam tolerance)", outInfo.VideoStreamDuration, srcInfo.VideoStreamDuration)
	}
}

// TestRunTranscodePreservesChunkOnProbeError: when ffprobe cannot verify an
// existing chunk (probe error, not a compliance verdict), the runner must not
// delete the chunk — the job fails and the chunk survives for a later
// attempt.
func TestRunTranscodePreservesChunkOnProbeError(t *testing.T) {
	tmp := t.TempDir()
	argsFile := filepath.Join(tmp, "args.bin")
	fakeFFmpeg := fakeFFmpegWithSplit(t, tmp, argsFile, 2)

	// Standard layout for ordinary paths, hard failure for encoded chunks.
	probeScript := `#!/bin/sh
last=""
for a in "$@"; do last="$a"; done
case "$last" in
*/chunk-*.mkv)
exit 99
;;
*/split-*.mkv)
printf '%s\n' '` + fakeChunkOnlyStreamJSON + `'
;;
*)
cat <<'EOF'
{"streams":[
 {"codec_type":"video","codec_name":"hevc"},
 {"codec_type":"audio","codec_name":"truehd","disposition":{"default":1},"tags":{"language":"eng"}},
 {"codec_type":"subtitle","codec_name":"subrip","disposition":{"default":0},"tags":{"language":"eng"}}
]}
EOF
;;
esac
`
	fakeTool(t, tmp, "ffprobe", probeScript)

	paths := jobpaths.FromOutput(filepath.Join(tmp, "out.mkv"))
	if err := os.MkdirAll(paths.SegmentDir(), 0755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := os.WriteFile(paths.SplitChunk(i), []byte("split"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	staleChunk := paths.EncodedChunk(0)
	if err := os.WriteFile(staleChunk, []byte("unverifiable chunk"), 0644); err != nil {
		t.Fatal(err)
	}

	// A verified demux container so Phase 0 resumes without a demux invocation.
	if err := os.WriteFile(paths.DemuxContainer(), []byte("demux"), 0644); err != nil {
		t.Fatal(err)
	}

	progress := make(chan Progress, 16)
	job := TranscodeJob{
		FFmpegPath:   fakeFFmpeg,
		InputFile:    filepath.Join(tmp, "in.mkv"),
		OutputFile:   filepath.Join(tmp, "out.mkv"),
		Codec:        "libx264",
		DurationSec:  600,
		Spec:         jobspec.JobSpec{Version: protocol.CurrentJobSpecVersion},
		ProgressChan: progress,
	}

	err := runWithTimeout(t, job)
	if err == nil || !strings.Contains(err.Error(), "stream probe failed") {
		t.Fatalf("expected chunk probe failure, got: %v", err)
	}
	assertChannelClosed(t, progress)

	content, readErr := os.ReadFile(staleChunk)
	if readErr != nil || string(content) != "unverifiable chunk" {
		t.Errorf("inconclusive probe must preserve the existing chunk (content=%q, err=%v)", content, readErr)
	}
}

// splitAwareProbeScript builds an ffprobe fake whose split-*.mkv probes
// answer with splitPayload while every other path reports the pure-video
// layout (the standard answer for source/demux/chunk probes in these tests).
func splitAwareProbeScript(splitPayload string) string {
	return `#!/bin/sh
last=""
for a in "$@"; do last="$a"; done
case "$last" in
*/split-*.mkv)
` + splitPayload + `
;;
*)
printf '%s\n' '` + fakeChunkOnlyStreamJSON + `'
;;
esac
`
}

// TestSplitMapsVideoOnly: the segment-muxer split must map only the video
// stream (-map 0:v:0 -an -sn -dn) so split chunks are pure video by
// construction — default stream selection would silently copy an audio track
// into every chunk and distort the seam math.
func TestSplitMapsVideoOnly(t *testing.T) {
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
		DurationSec:  600, // 2 chunks of 300s
		Spec:         jobspec.JobSpec{Version: protocol.CurrentJobSpecVersion},
		ProgressChan: progress,
	}

	if err := runWithTimeout(t, job); err != nil {
		t.Fatalf("RunTranscode failed: %v", err)
	}
	assertChannelClosed(t, progress)

	var splitArgs []string
	for _, inv := range readFakeArgs(t, argsFile) {
		for _, tok := range inv {
			if tok == "-segment_time" {
				splitArgs = inv
				break
			}
		}
		if splitArgs != nil {
			break
		}
	}
	if splitArgs == nil {
		t.Fatal("no segment-muxer split invocation recorded")
	}

	found := false
	for j, tok := range splitArgs {
		if tok == "-map" && j+1 < len(splitArgs) && splitArgs[j+1] == "0:v:0" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("split invocation missing -map 0:v:0: %v", splitArgs)
	}
	for _, flag := range []string{"-an", "-sn", "-dn"} {
		found := false
		for _, tok := range splitArgs {
			if tok == flag {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("split invocation missing %s: %v", flag, splitArgs)
		}
	}
}

// TestRunTranscodeReSplitsStaleNonVideoOnlySplitChunks: split chunks left
// behind by an older agent (audio-bearing segments from default stream
// selection) must fail the pure-video invariant, be deleted, and re-split —
// not be reused on resume.
func TestRunTranscodeReSplitsStaleNonVideoOnlySplitChunks(t *testing.T) {
	tmp := t.TempDir()
	argsFile := filepath.Join(tmp, "args.bin")
	fakeFFmpeg := fakeFFmpegWithSplit(t, tmp, argsFile, 2)

	// The first probe round of split chunks reports the old audio-bearing
	// layout; every later probe (after re-split) reports pure video.
	probeScript := splitAwareProbeScript(`marker="` + tmp + `/probed-stale-split"
if [ -f "$marker" ]; then
printf '%s\n' '` + fakeChunkOnlyStreamJSON + `'
else
touch "$marker"
cat <<'EOF'
{"streams":[
 {"codec_type":"video","codec_name":"hevc"},
 {"codec_type":"audio","codec_name":"truehd","disposition":{"default":1},"tags":{"language":"eng"}}
]}
EOF
fi`)
	fakeTool(t, tmp, "ffprobe", probeScript)

	paths := jobpaths.FromOutput(filepath.Join(tmp, "out.mkv"))
	if err := os.MkdirAll(paths.SegmentDir(), 0755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := os.WriteFile(paths.SplitChunk(i), []byte("stale audio-bearing split"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	progress := make(chan Progress, 16)
	job := TranscodeJob{
		FFmpegPath:   fakeFFmpeg,
		InputFile:    filepath.Join(tmp, "in.mkv"),
		OutputFile:   filepath.Join(tmp, "out.mkv"),
		Codec:        "libx264",
		DurationSec:  600, // 2 chunks of 300s
		Spec:         jobspec.JobSpec{Version: protocol.CurrentJobSpecVersion},
		ProgressChan: progress,
	}

	if err := runWithTimeout(t, job); err != nil {
		t.Fatalf("RunTranscode failed: %v", err)
	}
	assertChannelClosed(t, progress)

	// The stale split chunks must have been re-split: exactly one
	// segment-muxer invocation despite the pre-existing chunks.
	splitRuns := 0
	for _, inv := range readFakeArgs(t, argsFile) {
		for _, tok := range inv {
			if tok == "-segment_time" {
				splitRuns++
				break
			}
		}
	}
	if splitRuns != 1 {
		t.Fatalf("stale split chunks must be re-split exactly once, got %d split invocations", splitRuns)
	}
}

// TestRunTranscodeFailsWhenSplitChunksAreNotVideoOnly: pure video is an
// invariant of the split output. If the segment muxer ever emits chunks with
// non-video streams, the job must fail at the split gate rather than let
// seam overhang reach chunk encoding and concatenation.
func TestRunTranscodeFailsWhenSplitChunksAreNotVideoOnly(t *testing.T) {
	tmp := t.TempDir()
	argsFile := filepath.Join(tmp, "args.bin")
	fakeFFmpeg := fakeFFmpegWithSplit(t, tmp, argsFile, 2)

	probeScript := splitAwareProbeScript(`cat <<'EOF'
{"streams":[
 {"codec_type":"video","codec_name":"hevc"},
 {"codec_type":"audio","codec_name":"truehd","disposition":{"default":1},"tags":{"language":"eng"}}
]}
EOF`)
	fakeTool(t, tmp, "ffprobe", probeScript)

	progress := make(chan Progress, 16)
	job := TranscodeJob{
		FFmpegPath:   fakeFFmpeg,
		InputFile:    filepath.Join(tmp, "in.mkv"),
		OutputFile:   filepath.Join(tmp, "out.mkv"),
		Codec:        "libx264",
		DurationSec:  600, // 2 chunks of 300s
		Spec:         jobspec.JobSpec{Version: protocol.CurrentJobSpecVersion},
		ProgressChan: progress,
	}

	err := runWithTimeout(t, job)
	if err == nil || !strings.Contains(err.Error(), "split chunk 0 failed video-only verification") {
		t.Fatalf("expected split video-only verification failure, got: %v", err)
	}
	assertChannelClosed(t, progress)
}

// countSegmentInvocations returns how many recorded invocations were
// segment-muxer split passes.
func countSegmentInvocations(t *testing.T, argsFile string) int {
	t.Helper()
	n := 0
	for _, inv := range readFakeArgs(t, argsFile) {
		for _, tok := range inv {
			if tok == "-segment_time" {
				n++
				break
			}
		}
	}
	return n
}

// TestSplitEmitsSegmentTimingManifest: the split phase must record the
// segment-muxer timing manifest as a first-class job-path artifact — the
// segment_list CSV argument points at it, and it parses into one entry per
// chunk with the nominal chunk durations.
func TestSplitEmitsSegmentTimingManifest(t *testing.T) {
	tmp := t.TempDir()
	argsFile := filepath.Join(tmp, "args.bin")
	fakeFFmpeg := fakeFFmpegWithSplit(t, tmp, argsFile, 2)
	fakeToolChunkAwareFFprobe(t, tmp)

	paths := jobpaths.FromOutput(filepath.Join(tmp, "out.mkv"))
	progress := make(chan Progress, 16)
	job := TranscodeJob{
		FFmpegPath:   fakeFFmpeg,
		InputFile:    filepath.Join(tmp, "in.mkv"),
		OutputFile:   filepath.Join(tmp, "out.mkv"),
		Codec:        "libx264",
		DurationSec:  600, // 2 chunks of 300s
		Spec:         jobspec.JobSpec{Version: protocol.CurrentJobSpecVersion},
		ProgressChan: progress,
	}

	if err := runWithTimeout(t, job); err != nil {
		t.Fatalf("RunTranscode failed: %v", err)
	}
	assertChannelClosed(t, progress)

	var splitArgs []string
	for _, inv := range readFakeArgs(t, argsFile) {
		for _, tok := range inv {
			if tok == "-segment_time" {
				splitArgs = inv
				break
			}
		}
		if splitArgs != nil {
			break
		}
	}
	if splitArgs == nil {
		t.Fatal("no segment-muxer split invocation recorded")
	}
	foundList := false
	for i, tok := range splitArgs {
		if tok == "-segment_list" && i+1 < len(splitArgs) && splitArgs[i+1] == paths.SegmentTimingManifest() {
			foundList = true
		}
	}
	if !foundList {
		t.Errorf("split invocation missing -segment_list %q: %v", paths.SegmentTimingManifest(), splitArgs)
	}
	assertHasArg(t, splitArgs, "csv")

	timings, err := ParseSegmentTimingManifest(paths.SegmentTimingManifest())
	if err != nil {
		t.Fatalf("ParseSegmentTimingManifest: %v", err)
	}
	if len(timings) != 2 {
		t.Fatalf("timing manifest has %d entries, want 2", len(timings))
	}
	for i, timing := range timings {
		if d := timing.Duration(); d != 300.0 {
			t.Errorf("chunk %d duration = %f, want 300.0", i, d)
		}
		if got := filepath.Base(timing.File); got != filepath.Base(paths.SplitChunk(i)) {
			t.Errorf("chunk %d file = %q, want %q", i, got, filepath.Base(paths.SplitChunk(i)))
		}
	}
}

// TestRunTranscodeResumesSparseKeyframeSplitFromManifest: a source with no
// keyframe inside a segment window produces fewer chunks than
// ceil(duration/segmentSecs). The recorded timing manifest is the resume key:
// the job must reuse the existing chunks without re-splitting, and the concat
// manifest must carry the real (not nominal) per-chunk durations.
func TestRunTranscodeResumesSparseKeyframeSplitFromManifest(t *testing.T) {
	tmp := t.TempDir()
	argsFile := filepath.Join(tmp, "args.bin")
	fakeFFmpeg := fakeFFmpegWithSplit(t, tmp, argsFile, 2)
	fakeToolChunkAwareFFprobe(t, tmp)

	paths := jobpaths.FromOutput(filepath.Join(tmp, "out.mkv"))
	if err := os.MkdirAll(paths.SegmentDir(), 0755); err != nil {
		t.Fatal(err)
	}
	// 900s source, 300s windows -> 3 nominal chunks, but the source had
	// keyframes only at 0s and 500s: the split produced 2 chunks.
	for i := 0; i < 2; i++ {
		if err := os.WriteFile(paths.SplitChunk(i), []byte("split"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	csv := paths.SegmentTimingManifest()
	content := paths.SplitChunk(0) + ",0.000000,500.000000\n" +
		paths.SplitChunk(1) + ",500.000000,900.000000\n"
	if err := os.WriteFile(csv, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	progress := make(chan Progress, 16)
	job := TranscodeJob{
		FFmpegPath:   fakeFFmpeg,
		InputFile:    filepath.Join(tmp, "in.mkv"),
		OutputFile:   filepath.Join(tmp, "out.mkv"),
		Codec:        "libx264",
		DurationSec:  900, // 3 nominal chunks of 300s
		Spec:         jobspec.JobSpec{Version: protocol.CurrentJobSpecVersion},
		ProgressChan: progress,
	}

	if err := runWithTimeout(t, job); err != nil {
		t.Fatalf("RunTranscode failed: %v", err)
	}
	assertChannelClosed(t, progress)

	if n := countSegmentInvocations(t, argsFile); n != 0 {
		t.Fatalf("sparse-keyframe resume must not re-split, got %d segment-muxer invocations", n)
	}

	// The concat manifest must carry the recorded durations (500s and 400s),
	// not the nominal 300s window.
	manifest, err := os.ReadFile(paths.ConcatFile())
	if err != nil {
		t.Fatalf("read concat manifest: %v", err)
	}
	if !strings.Contains(string(manifest), "duration 500.000000") {
		t.Errorf("concat manifest missing real chunk duration 500s:\n%s", manifest)
	}
	if !strings.Contains(string(manifest), "duration 400.000000") {
		t.Errorf("concat manifest missing real chunk duration 400s:\n%s", manifest)
	}
}

// TestRunTranscodeReSplitsWhenManifestChunkMissing: a recorded manifest whose
// chunks are not all on disk means the split was interrupted; the job must
// re-split rather than resume from a partial chunk set.
func TestRunTranscodeReSplitsWhenManifestChunkMissing(t *testing.T) {
	tmp := t.TempDir()
	argsFile := filepath.Join(tmp, "args.bin")
	fakeFFmpeg := fakeFFmpegWithSplit(t, tmp, argsFile, 2)
	fakeToolChunkAwareFFprobe(t, tmp)

	paths := jobpaths.FromOutput(filepath.Join(tmp, "out.mkv"))
	if err := os.MkdirAll(paths.SegmentDir(), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.SplitChunk(0), []byte("split"), 0644); err != nil {
		t.Fatal(err)
	}
	// Manifest records two chunks; only chunk 0 exists.
	content := paths.SplitChunk(0) + ",0.000000,300.000000\n" +
		paths.SplitChunk(1) + ",300.000000,600.000000\n"
	if err := os.WriteFile(paths.SegmentTimingManifest(), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	progress := make(chan Progress, 16)
	job := TranscodeJob{
		FFmpegPath:   fakeFFmpeg,
		InputFile:    filepath.Join(tmp, "in.mkv"),
		OutputFile:   filepath.Join(tmp, "out.mkv"),
		Codec:        "libx264",
		DurationSec:  600,
		Spec:         jobspec.JobSpec{Version: protocol.CurrentJobSpecVersion},
		ProgressChan: progress,
	}

	if err := runWithTimeout(t, job); err != nil {
		t.Fatalf("RunTranscode failed: %v", err)
	}
	assertChannelClosed(t, progress)

	if n := countSegmentInvocations(t, argsFile); n != 1 {
		t.Fatalf("incomplete manifest must re-split exactly once, got %d segment-muxer invocations", n)
	}
}

// TestRunTranscodeFailsWhenManifestMismatchesSplitOutput: a timing manifest
// that disagrees with the chunks the split produced is a corrupted contract —
// the job must fail rather than stitch with wrong seam durations.
func TestRunTranscodeFailsWhenManifestMismatchesSplitOutput(t *testing.T) {
	tmp := t.TempDir()
	argsFile := filepath.Join(tmp, "args.bin")

	// Split fake: creates two chunks but records only one CSV row.
	script := fmt.Sprintf(`#!/bin/sh
{ for a in "$@"; do printf '%%s\000' "$a"; done; printf '\000'; } >> %q
is_seg=0
is_demux=0
seg_list=""
prev=""
out=""
for a in "$@"; do
  [ "$a" = "segment" ] && is_seg=1
  case "$a" in 0:s*) is_demux=1 ;; esac
  if [ "$prev" = "-segment_list" ]; then seg_list="$a"; fi
  prev="$a"
  out="$a"
done
if [ "$is_demux" = "1" ]; then
  printf 'x' > "$out"
  exit 0
fi
if [ "$is_seg" = "1" ]; then
  dir=$(dirname "$out")
  printf 'x' > "$dir/split-000.mkv"
  printf 'x' > "$dir/split-001.mkv"
  printf '%%s,0.000000,600.000000\n' "$dir/split-000.mkv" > "$seg_list"
  exit 0
fi
exit 0
`, argsFile)
	fakeTool(t, tmp, "ffmpeg", script)
	fakeToolChunkAwareFFprobe(t, tmp)

	progress := make(chan Progress, 16)
	job := TranscodeJob{
		FFmpegPath:   filepath.Join(tmp, "ffmpeg"),
		InputFile:    filepath.Join(tmp, "in.mkv"),
		OutputFile:   filepath.Join(tmp, "out.mkv"),
		Codec:        "libx264",
		DurationSec:  600,
		Spec:         jobspec.JobSpec{Version: protocol.CurrentJobSpecVersion},
		ProgressChan: progress,
	}

	err := runWithTimeout(t, job)
	if err == nil || !strings.Contains(err.Error(), "segment timing manifest records 1 chunks but split produced 2") {
		t.Fatalf("expected manifest mismatch failure, got: %v", err)
	}
	assertChannelClosed(t, progress)
}

// TestRunTranscodeMultiChunkSeamExactConcatAgainstRealFFmpeg is the acceptance
// check for the segment timing manifest (#190): a multi-chunk job must record
// the split timing CSV, derive explicit per-chunk duration directives in the
// concat manifest from it, and stitch to a video duration that is exact
// against the split boundaries — no cumulative B-frame-flush seam padding.
func TestRunTranscodeMultiChunkSeamExactConcatAgainstRealFFmpeg(t *testing.T) {
	ffmpegPath, ffprobePath := repoBinFFmpeg(t)
	tmp := t.TempDir()

	oldSegmentSecs := segmentSecs
	segmentSecs = 2.0
	defer func() { segmentSecs = oldSegmentSecs }()

	const srcDur = 5 // 3 chunks of ~2s — two real seams
	sourcePath := filepath.Join(tmp, "source.mkv")
	cmd := exec.Command(ffmpegPath,
		"-hide_banner", "-nostdin", "-y",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=320x240:rate=25:duration=%d", srcDur),
		"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:sample_rate=48000:duration=%d", srcDur),
		"-map", "0:v", "-map", "1:a",
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "45", "-g", "25",
		"-c:a", "aac",
		sourcePath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to synthesize source: %v: %s", err, out)
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

	done := make(chan error, 1)
	go func() { done <- RunTranscode(t.Context(), job) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunTranscode failed: %v", err)
		}
	case <-time.After(120 * time.Second):
		t.Fatal("RunTranscode did not finish within 120s")
	}

	// Criterion: the split phase recorded the timing manifest as a
	// first-class artifact, one entry per chunk with positive durations.
	timings, err := ParseSegmentTimingManifest(paths.SegmentTimingManifest())
	if err != nil {
		t.Fatalf("segment timing manifest missing or invalid: %v", err)
	}
	if len(timings) != 3 {
		t.Fatalf("timing manifest has %d entries, want 3", len(timings))
	}
	var sumDurations float64
	for i, timing := range timings {
		if d := timing.Duration(); d <= 0 {
			t.Errorf("chunk %d has non-positive duration %f", i, d)
		} else {
			sumDurations += d
		}
	}

	// Criterion: the concat manifest carries explicit duration directives
	// derived from the split manifest.
	manifest, err := os.ReadFile(paths.ConcatFile())
	if err != nil {
		t.Fatalf("read concat manifest: %v", err)
	}
	if strings.Count(string(manifest), "duration ") != 3 {
		t.Errorf("concat manifest must carry one duration directive per chunk:\n%s", manifest)
	}
	for _, timing := range timings {
		directive := fmt.Sprintf("duration %.6f", timing.Duration())
		if !strings.Contains(string(manifest), directive) {
			t.Errorf("concat manifest missing %q:\n%s", directive, manifest)
		}
	}

	// Criterion: seam-exact duration. Without explicit durations the concat
	// demuxer accumulates +40–80ms of B-frame flush padding per seam (≥0.08s
	// over these two seams); the stitched video stream must match the source
	// within a single frame (40ms at 25fps) and match the recorded split
	// boundaries just as tightly.
	outInfo, err := ProbeMedia(ffprobePath, job.OutputFile)
	if err != nil {
		t.Fatalf("probe output: %v", err)
	}
	const frameTolerance = 0.05
	if diff := math.Abs(outInfo.VideoStreamDuration - srcInfo.VideoStreamDuration); diff > frameTolerance {
		t.Errorf("stitched video duration %.3f differs from source %.3f by %.3fs (>%.3fs: cumulative seam padding)",
			outInfo.VideoStreamDuration, srcInfo.VideoStreamDuration, diff, frameTolerance)
	}
	if diff := math.Abs(outInfo.VideoStreamDuration - sumDurations); diff > frameTolerance {
		t.Errorf("stitched video duration %.3f differs from split-boundary sum %.3f by %.3fs (>%.3fs: seam offsets not sample-exact)",
			outInfo.VideoStreamDuration, sumDurations, diff, frameTolerance)
	}

	// Criterion: seam-exact frame placement. The concat demuxer places chunk i
	// at the cumulative recorded duration of the preceding chunks, so the
	// first frame after each seam must land exactly on the recorded split
	// boundary. A bare manifest instead derives the offset from the chunk
	// container duration, which drifts +40–80ms per seam (cumulative).
	ptsTimes, err := probeVideoPacketPTSTimes(ffprobePath, job.OutputFile)
	if err != nil {
		t.Fatalf("probe output packet timestamps: %v", err)
	}
	if len(ptsTimes) == 0 {
		t.Fatal("no video packets probed in stitched output")
	}
	srcPTS, err := probeVideoPacketPTSTimes(ffprobePath, sourcePath)
	if err != nil {
		t.Fatalf("probe source packet timestamps: %v", err)
	}
	if len(ptsTimes) != len(srcPTS) {
		t.Errorf("stitched frame count = %d, want %d (seams must not drop or duplicate frames)", len(ptsTimes), len(srcPTS))
	}
	var offset float64
	for i := 1; i < len(timings); i++ {
		offset += timings[i-1].Duration()
		closest := -1.0
		for _, p := range ptsTimes {
			if d := math.Abs(p - offset); closest < 0 || d < math.Abs(closest-offset) {
				closest = p
			}
		}
		if math.Abs(closest-offset) > 0.02 {
			t.Errorf("no stitched frame within 20ms of split boundary %.3f (closest %.3f): seam drifted from the recorded manifest", offset, closest)
		}
	}
}

// TestRunTranscodeMultiChunkVFRDelayedAudioSparseKeyframesAgainstRealFFmpeg is
// the #193 acceptance check on the tightest real-world-like input: a VFR
// source (every 5th frame dropped, so timestamps carry holes), a deliberate
// +0.6s audio delay, and sparse keyframes (three forced IDRs across the whole
// file). The stitched output must be frame-exact in duration and frame count
// against the source, and its audio/video start-time offset must match the
// source's within ±40ms — the flat duration gate and the a/v alignment gate
// must both pass on exactly this shape of media.
func TestRunTranscodeMultiChunkVFRDelayedAudioSparseKeyframesAgainstRealFFmpeg(t *testing.T) {
	ffmpegPath, ffprobePath := repoBinFFmpeg(t)
	tmp := t.TempDir()

	oldSegmentSecs := segmentSecs
	segmentSecs = 2.0
	defer func() { segmentSecs = oldSegmentSecs }()

	// VFR base: 5s at 25fps with every 5th frame dropped (100 frames on the
	// 25fps timestamp grid with holes), keyframes only at 0/2/4s via
	// force_key_frames with -g 250 suppressing natural IDRs. The drop pattern
	// is phase-shifted (drops n=4,9,14,...) so the frames *starting* at the
	// 2s/4s split boundaries survive; the frame *ending* at 2.0 is dropped,
	// which is exactly the VFR boundary-hole shape the seam math must handle
	// (recorded chunk end 1.96, next chunk start 2.0).
	const srcDur = 5 // forced IDRs at 0/2/4 -> 3 chunks of 2s/2s/1s
	const audioDelay = 0.6
	basePath := filepath.Join(tmp, "base.mkv")
	cmd := exec.Command(ffmpegPath,
		"-hide_banner", "-nostdin", "-y",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=320x240:rate=25:duration=%d", srcDur),
		"-vf", "select='if(mod(n+1\\,5),1,0)'",
		"-fps_mode", "passthrough",
		"-map", "0:v",
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "45",
		"-g", "250", "-force_key_frames", "0,2,4",
		basePath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to synthesize VFR base: %v: %s", err, out)
	}

	// Deliberate audio delay: remux with -itsoffset 0.6 shifting the audio
	// track, mirroring a broadcast recording whose offset is content.
	sourcePath := filepath.Join(tmp, "source.mkv")
	cmd = exec.Command(ffmpegPath,
		"-hide_banner", "-nostdin", "-y",
		"-i", basePath,
		"-itsoffset", fmt.Sprintf("%g", audioDelay), "-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:sample_rate=48000:duration=%d", srcDur),
		"-map", "0:v", "-map", "1:a",
		"-map_metadata", "-1",
		"-c", "copy",
		sourcePath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to synthesize delayed-audio source: %v: %s", err, out)
	}

	// Confirm the source really is VFR: every 5th frame dropped leaves holes
	// in the packet timestamp grid (frame deltas other than the 40ms nominal).
	out, err := exec.Command(ffprobePath, "-v", "error",
		"-select_streams", "v:0",
		"-read_intervals", "%+#12",
		"-show_entries", "packet=pts_time",
		"-of", "csv=p=0", sourcePath).Output()
	if err != nil {
		t.Fatalf("probe source packet timestamps: %v", err)
	}
	var prevPts float64
	vfr := false
	for i, line := range strings.Fields(strings.TrimSpace(string(out))) {
		pts, err := strconv.ParseFloat(line, 64)
		if err != nil {
			t.Fatalf("parse packet pts %q: %v", line, err)
		}
		if i > 0 && math.Abs(pts-prevPts-0.04) > 0.001 {
			vfr = true
			break
		}
		prevPts = pts
	}
	if !vfr {
		t.Fatal("source must be VFR (packet timestamps carry holes from dropped frames)")
	}

	srcInfo, err := ProbeMedia(ffprobePath, sourcePath)
	if err != nil {
		t.Fatalf("probe source: %v", err)
	}
	srcOffset, ok := srcInfo.AVOffset()
	if !ok {
		t.Fatalf("source a/v offsets must be probed: video set=%v audio set=%v", srcInfo.VideoStartTimeSet, srcInfo.AudioStartTimeSet)
	}
	if math.Abs(srcOffset-audioDelay) > 0.05 {
		t.Fatalf("source a/v offset = %.3f, want ~%.2f (itsoffset application failed)", srcOffset, audioDelay)
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

	done := make(chan error, 1)
	go func() { done <- RunTranscode(t.Context(), job) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunTranscode failed: %v", err)
		}
	case <-time.After(120 * time.Second):
		t.Fatal("RunTranscode did not finish within 120s")
	}

	// Criterion: frame-exact duration. The flat ±2s gate is trivially met;
	// the real bar is a single frame (40ms at 25fps) — seam drift, junction
	// padding, or VFR timestamp loss all blow past it.
	outInfo, err := ProbeMedia(ffprobePath, job.OutputFile)
	if err != nil {
		t.Fatalf("probe output: %v", err)
	}
	const frameTolerance = 0.05
	if diff := math.Abs(outInfo.VideoStreamDuration - srcInfo.VideoStreamDuration); diff > frameTolerance {
		t.Errorf("stitched video duration %.3f differs from source %.3f by %.3fs (>%.3fs: seam or VFR drift)",
			outInfo.VideoStreamDuration, srcInfo.VideoStreamDuration, diff, frameTolerance)
	}
	if outInfo.TotalFrames != srcInfo.TotalFrames {
		t.Errorf("stitched frame count = %d, want %d (seams must not drop or duplicate VFR frames)", outInfo.TotalFrames, srcInfo.TotalFrames)
	}

	// Criterion: a/v alignment. The output's audio-minus-video offset must
	// match the source's +0.6s within ±40ms — this is exactly what the
	// orchestrator's verifyAVAlignment gate enforces.
	newOffset, ok := outInfo.AVOffset()
	if !ok {
		t.Fatalf("output a/v offsets must be probed: video set=%v audio set=%v", outInfo.VideoStartTimeSet, outInfo.AudioStartTimeSet)
	}
	if delta := newOffset - srcOffset; math.Abs(delta) > 0.040 {
		t.Errorf("output a/v offset %.3f differs from source %.3f by %.0fms (>±40ms: desync)", newOffset, srcOffset, delta*1000)
	}
}
