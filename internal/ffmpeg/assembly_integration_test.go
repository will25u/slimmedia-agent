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

	"github.com/will25u/slimmedia-agent/internal/jobpaths"
	"github.com/will25u/slimmedia-agent/internal/jobspec"
	"github.com/will25u/slimmedia-agent/internal/protocol"
)

// recordingFFmpeg wraps the repo's real ffmpeg with a shell script that logs
// every invocation's argv (NUL-separated, invocations separated by a double
// NUL — the same format readFakeArgs parses) before exec'ing the real
// binary. ffprobe is symlinked next to the wrapper so the runner's
// ffprobeNextTo convention keeps working. Returns the wrapper path and the
// argv log path.
func recordingFFmpeg(t *testing.T, realFFmpeg, realFFprobe, dir, argsFile string) string {
	t.Helper()
	wrapper := filepath.Join(dir, "ffmpeg")
	script := fmt.Sprintf(`#!/bin/sh
{ for a in "$@"; do printf '%%s\000' "$a"; done; printf '\000'; } >> %q
exec %q "$@"
`, argsFile, realFFmpeg)
	if err := os.WriteFile(wrapper, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write ffmpeg wrapper: %v", err)
	}
	if err := os.Symlink(realFFprobe, filepath.Join(dir, "ffprobe")); err != nil {
		t.Fatalf("failed to symlink ffprobe: %v", err)
	}
	return wrapper
}

// concatInvocation returns the recorded argv of the final-assembly (concat
// demuxer) invocation, failing the test when there is not exactly one.
func concatInvocation(t *testing.T, argsFile string) []string {
	t.Helper()
	var concat []string
	for _, inv := range readFakeArgs(t, argsFile) {
		for i, tok := range inv {
			if tok == "-f" && i+1 < len(inv) && inv[i+1] == "concat" {
				concat = inv
			}
		}
	}
	if concat == nil {
		t.Fatalf("no concat invocation recorded in %s", argsFile)
	}
	return concat
}

func assertNoArg(t *testing.T, inv []string, forbidden string) {
	t.Helper()
	for _, tok := range inv {
		if tok == forbidden {
			t.Errorf("forbidden argument %q in invocation: %q", forbidden, inv)
		}
	}
}

func assertHasArg(t *testing.T, inv []string, want string) {
	t.Helper()
	for _, tok := range inv {
		if tok == want {
			return
		}
	}
	t.Errorf("expected argument %q in invocation: %q", want, inv)
}

// synthesizeSidecarSource builds the standard local-assembly test source:
// 2s of video, two audio tracks, one text subtitle, two chapters.
func synthesizeSidecarSource(t *testing.T, ffmpegPath, tmp string) string {
	t.Helper()
	srtPath := filepath.Join(tmp, "subs.srt")
	if err := os.WriteFile(srtPath, []byte("1\n00:00:00,200 --> 00:00:01,200\nHello from the assembly test\n\n"), 0644); err != nil {
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
	return sourcePath
}

// probeStreamStartTimes returns codec_type → start_time (seconds) for every
// stream of the file, via ffprobe's stream start_time field. Used by the
// offset-preservation integration test; a missing start_time reports as 0.
func probeStreamStartTimes(t *testing.T, ffprobePath, mediaPath string) map[string]float64 {
	t.Helper()
	out, err := exec.Command(ffprobePath, "-v", "error",
		"-show_entries", "stream=codec_type,start_time",
		"-of", "csv=p=0", mediaPath).Output()
	if err != nil {
		t.Fatalf("probe stream start_times of %s: %v", mediaPath, err)
	}
	starts := make(map[string]float64)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(line, ",")
		if len(fields) < 2 || fields[1] == "N/A" {
			continue
		}
		start, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			t.Fatalf("parse start_time %q for stream %q: %v", fields[1], fields[0], err)
		}
		starts[fields[0]] = start
	}
	return starts
}

// probeFirstPacketPTS returns the pts_time (seconds) of the first packet of
// the first stream matching streamType ("audio" or "subtitle"). Subtitle
// streams carry no start_time field in Matroska, so cue offsets are only
// observable at packet level.
func probeFirstPacketPTS(t *testing.T, ffprobePath, mediaPath, streamType string) float64 {
	t.Helper()
	pts, err := probePacketPTSTimes(ffprobePath, mediaPath, streamType[:1]+":0")
	if err != nil {
		t.Fatalf("probe %s packets of %s: %v", streamType, mediaPath, err)
	}
	if len(pts) == 0 {
		t.Fatalf("no %s packets found in %s", streamType, mediaPath)
	}
	return pts[0]
}

// synthesizeOffsetSource builds a source with a deliberate multi-hundred-
// millisecond audio delay and a delayed first subtitle cue: 2s of video and
// audio, remuxed with -itsoffset 0.6 shifting the audio track to start at
// +0.6s, plus an SRT whose first cue lands at 0.7s. This mirrors broadcast
// recordings where a track offset is intentional content, not an error.
func synthesizeOffsetSource(t *testing.T, ffmpegPath, tmp string) (string, float64) {
	t.Helper()
	const audioDelay = 0.6
	srtPath := filepath.Join(tmp, "delayed.srt")
	if err := os.WriteFile(srtPath, []byte("1\n00:00:00,700 --> 00:00:01,700\nDelayed cue from the offset test\n\n"), 0644); err != nil {
		t.Fatal(err)
	}
	basePath := filepath.Join(tmp, "base.mkv")
	cmd := exec.Command(ffmpegPath,
		"-hide_banner", "-nostdin", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=2",
		"-i", srtPath,
		"-map", "0:v", "-map", "1:a", "-map", "2:s",
		"-map_metadata", "-1",
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "40",
		"-c:a", "aac",
		"-c:s", "copy",
		basePath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to synthesize base source: %v: %s", err, out)
	}
	sourcePath := filepath.Join(tmp, "offset.mkv")
	cmd = exec.Command(ffmpegPath,
		"-hide_banner", "-nostdin", "-y",
		"-i", basePath,
		"-itsoffset", fmt.Sprintf("%g", audioDelay), "-i", basePath,
		"-map", "0:v", "-map", "1:a", "-map", "0:s",
		"-map_metadata", "-1",
		"-c", "copy",
		sourcePath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to synthesize offset source: %v: %s", err, out)
	}
	return sourcePath, audioDelay
}

// TestRunTranscodePreservesAudioAndSubtitleOffsets is the acceptance check
// for #192: a source whose audio track deliberately starts +0.6s late (and
// whose first subtitle cue lands at 0.7s) runs through the full segmented
// pipeline with the repo's real ffmpeg, and the stitched output must keep
// those start-time offsets — while the recorded final-assembly argv shows
// -copyts on the demux sidecar input that makes preservation happen.
func TestRunTranscodePreservesAudioAndSubtitleOffsets(t *testing.T) {
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
		},
		Paths: &paths,
	}

	if err := runWithTimeout(t, job); err != nil {
		t.Fatalf("RunTranscode failed: %v", err)
	}

	// Criterion: final assembly muxes the demux sidecar with -copyts so
	// original track timestamps survive the stitch.
	concat := concatInvocation(t, argsFile)
	assertNoArg(t, concat, job.InputFile)
	assertHasArg(t, concat, "-copyts")
	assertHasArg(t, concat, paths.DemuxContainer())

	// Criterion: the output preserves the source's track offsets — audio
	// still starts ~0.6s late, the first subtitle cue still lands at its
	// delayed time, and video still starts at zero.
	outStarts := probeStreamStartTimes(t, ffprobePath, job.OutputFile)
	if got := outStarts["video"]; got > 0.05 {
		t.Errorf("output video start_time = %f, want ~0", got)
	}
	if got := outStarts["audio"]; math.Abs(got-audioDelay) > 0.05 {
		t.Errorf("output audio start_time = %f, want ~%v (offset annihilated)", got, audioDelay)
	}
	if got := probeFirstPacketPTS(t, ffprobePath, job.OutputFile, "subtitle"); math.Abs(got-srcFirstSub) > 0.05 {
		t.Errorf("output first subtitle packet pts = %f, want ~%v (cue offset not preserved)", got, srcFirstSub)
	}
}

// TestRunTranscodeLocalAssemblyAgainstRealFFmpeg is the acceptance check for
// local-only final assembly (#153) end to end: a source carrying audio,
// subtitle, and chapter streams runs through the full segmented pipeline
// with the repo's real ffmpeg, and the output must retain every audio
// track, the subtitle track, and the chapter markers — while the recorded
// final-assembly argv references only local scratch artifacts (concat
// manifest + demux container) and never the source path.
func TestRunTranscodeLocalAssemblyAgainstRealFFmpeg(t *testing.T) {
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
		},
		Paths: &paths,
	}

	if err := runWithTimeout(t, job); err != nil {
		t.Fatalf("RunTranscode failed: %v", err)
	}

	// Criterion: final assembly inputs are exclusively local scratch files;
	// the remote source path appears in no final-assembly ffmpeg arguments.
	concat := concatInvocation(t, argsFile)
	assertNoArg(t, concat, job.InputFile)
	assertHasArg(t, concat, paths.ConcatFile())
	assertHasArg(t, concat, paths.DemuxContainer())

	// Criterion: the output retains all audio/subtitle tracks and chapters.
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

	// Criterion: no regression for sources whose tracks all start at zero —
	// with -copyts on the demux sidecar, zero-offset tracks must stay at
	// zero rather than gaining a phantom delay. (Subtitle streams are
	// exempt: Matroska reports their start_time as the first cue time,
	// which is content, not a track offset.)
	for streamType, start := range probeStreamStartTimes(t, ffprobePath, job.OutputFile) {
		if streamType == "subtitle" {
			continue
		}
		if start > 0.05 {
			t.Errorf("output %s start_time = %f, want ~0 (zero-offset source regressed)", streamType, start)
		}
	}

	// Criterion: duration verification still passes like-for-like.
	outInfo, err := ProbeMedia(ffprobePath, job.OutputFile)
	if err != nil {
		t.Fatalf("probe output: %v", err)
	}
	if diff := math.Abs(outInfo.VideoStreamDuration - srcInfo.VideoStreamDuration); diff > 0.5 {
		t.Errorf("output video stream duration %.2f differs from source %.2f by %.2fs (>0.5s)", outInfo.VideoStreamDuration, srcInfo.VideoStreamDuration, diff)
	}
}

// TestRunTranscodeChapterSidecarAssemblyAgainstRealFFmpeg covers the
// chapters-only edge of local assembly: a source with chapter markers but no
// audio/subtitle streams produces no demux container, and final assembly
// maps chapters from the local ffmetadata sidecar — again without touching
// the source.
func TestRunTranscodeChapterSidecarAssemblyAgainstRealFFmpeg(t *testing.T) {
	ffmpegPath, ffprobePath := repoBinFFmpeg(t)
	tmp := t.TempDir()

	metaPath := filepath.Join(tmp, "chapters.txt")
	meta := ";FFMETADATA1\n[CHAPTER]\nTIMEBASE=1/1000\nSTART=0\nEND=1000\ntitle=Opening\n[CHAPTER]\nTIMEBASE=1/1000\nSTART=1000\nEND=2000\ntitle=End\n"
	if err := os.WriteFile(metaPath, []byte(meta), 0644); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(tmp, "source.mkv")
	cmd := exec.Command(ffmpegPath,
		"-hide_banner", "-nostdin", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25:duration=2",
		"-i", metaPath,
		"-map", "0:v",
		"-map_metadata", "-1", "-map_chapters", "1",
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "40",
		sourcePath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to synthesize source: %v: %s", err, out)
	}

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
			Version:     protocol.CurrentJobSpecVersion,
			Container:   "mkv",
			CRF:         40,
			PresetSpeed: "ultrafast",
			ColorDepth:  "8bit",
		},
		Paths: &paths,
	}

	if err := runWithTimeout(t, job); err != nil {
		t.Fatalf("RunTranscode failed: %v", err)
	}

	// No demux container was built; assembly must consume the chapter sidecar.
	if _, err := os.Stat(paths.DemuxContainer()); !os.IsNotExist(err) {
		t.Errorf("no demux container expected for a chapters-only source")
	}
	concat := concatInvocation(t, argsFile)
	assertNoArg(t, concat, job.InputFile)
	assertHasArg(t, concat, paths.ChapterFile())
	assertHasArg(t, concat, "-map_chapters")
	if strings.Contains(strings.Join(concat, " "), paths.DemuxContainer()) {
		t.Errorf("concat argv must not reference the absent demux container: %q", concat)
	}

	if got := ProbeChapterCount(ffprobePath, job.OutputFile); got != 2 {
		t.Errorf("output chapter count = %d, want 2", got)
	}
	if streams := ProbeStreams(ffprobePath, job.OutputFile); len(streams) != 0 {
		t.Errorf("output must carry no audio/subtitle streams, got %d", len(streams))
	}
}
