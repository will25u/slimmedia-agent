package ffmpeg

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/will25u/slimmedia-agent/internal/jobpaths"
)

// repoBinFFmpeg resolves the repo-root ffmpeg/ffprobe pair for integration
// tests, skipping when the binaries are unavailable.
func repoBinFFmpeg(t *testing.T) (string, string) {
	t.Helper()
	ffmpegPath, err := filepath.Abs(filepath.Join("..", "..", "..", "bin", "ffmpeg"))
	if err != nil {
		t.Fatalf("failed to resolve bin path: %v", err)
	}
	ffprobePath := filepath.Join(filepath.Dir(ffmpegPath), "ffprobe")
	if _, err := os.Stat(ffmpegPath); err != nil {
		t.Skipf("repo ffmpeg not available: %v", err)
	}
	if _, err := os.Stat(ffprobePath); err != nil {
		t.Skipf("repo ffprobe not available: %v", err)
	}
	return ffmpegPath, ffprobePath
}

// TestExtractSidecarStreamsAgainstRealFFmpeg synthesizes a source with two
// audio tracks, a text subtitle track, and two chapters using the repo's
// real ffmpeg, then runs the upfront demux pass and verifies the scratch
// container with ffprobe: track counts, codecs, and chapter markers must all
// match the source. This is the acceptance-criteria check for the demux pass
// end to end; the unit tests above cover failure and resume semantics with
// fakes.
func TestExtractSidecarStreamsAgainstRealFFmpeg(t *testing.T) {
	ffmpegPath, ffprobePath := repoBinFFmpeg(t)
	tmp := t.TempDir()

	srtPath := filepath.Join(tmp, "subs.srt")
	srt := "1\n00:00:00,200 --> 00:00:01,200\nHello from the demux test\n\n"
	if err := os.WriteFile(srtPath, []byte(srt), 0644); err != nil {
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

	sourceStreams := ProbeStreams(ffprobePath, sourcePath)
	if len(sourceStreams) != 3 {
		t.Fatalf("source probe: got %d audio/subtitle streams, want 3", len(sourceStreams))
	}
	sourceChapters := ProbeChapterCount(ffprobePath, sourcePath)
	if sourceChapters != 2 {
		t.Fatalf("source chapter count = %d, want 2", sourceChapters)
	}

	job := TranscodeJob{
		FFmpegPath: ffmpegPath,
		InputFile:  sourcePath,
		OutputFile: filepath.Join(tmp, "out.mkv"),
	}
	paths := jobpaths.FromOutput(job.OutputFile)
	if err := os.MkdirAll(paths.SegmentDir(), 0755); err != nil {
		t.Fatal(err)
	}

	if err := extractSidecarStreams(t.Context(), job, &paths, sourceStreams); err != nil {
		t.Fatalf("extractSidecarStreams: %v", err)
	}

	if err := verifyDemuxContainer(ffprobePath, sourceStreams, sourceChapters, paths.DemuxContainer()); err != nil {
		t.Fatalf("verifyDemuxContainer: %v", err)
	}
	if got := ProbeChapterCount(ffprobePath, paths.DemuxContainer()); got != sourceChapters {
		t.Errorf("demux chapter count = %d, want %d", got, sourceChapters)
	}
	demuxAudio := 0
	for _, s := range ProbeStreams(ffprobePath, paths.DemuxContainer()) {
		if s.Type == "audio" {
			demuxAudio++
		}
	}
	if demuxAudio != 2 {
		t.Errorf("demux audio track count = %d, want 2", demuxAudio)
	}
}

// TestExtractSidecarStreamsSkipsMovTextAgainstRealFFmpeg is the regression
// check for MP4-sourced mov_text subtitles: the Matroska scratch container
// cannot hold them, so extraction must still succeed with the audio track
// while the subtitle is skipped (and logged).
func TestExtractSidecarStreamsSkipsMovTextAgainstRealFFmpeg(t *testing.T) {
	ffmpegPath, ffprobePath := repoBinFFmpeg(t)
	tmp := t.TempDir()

	srtPath := filepath.Join(tmp, "subs.srt")
	if err := os.WriteFile(srtPath, []byte("1\n00:00:00,200 --> 00:00:01,200\nHello\n\n"), 0644); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(tmp, "source.mp4")
	cmd := exec.Command(ffmpegPath,
		"-hide_banner", "-nostdin", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=2",
		"-i", srtPath,
		"-map", "0:v", "-map", "1:a", "-map", "2:s",
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "40",
		"-c:a", "aac",
		"-c:s", "mov_text",
		sourcePath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to synthesize source: %v: %s", err, out)
	}

	sourceStreams := ProbeStreams(ffprobePath, sourcePath)
	if len(sourceStreams) != 2 {
		t.Fatalf("source probe: got %d audio/subtitle streams, want 2", len(sourceStreams))
	}

	job := TranscodeJob{
		FFmpegPath: ffmpegPath,
		InputFile:  sourcePath,
		OutputFile: filepath.Join(tmp, "out.mkv"),
	}
	paths := jobpaths.FromOutput(job.OutputFile)
	if err := os.MkdirAll(paths.SegmentDir(), 0755); err != nil {
		t.Fatal(err)
	}

	if err := extractSidecarStreams(t.Context(), job, &paths, sourceStreams); err != nil {
		t.Fatalf("extractSidecarStreams with mov_text source: %v", err)
	}
	for _, s := range ProbeStreams(ffprobePath, paths.DemuxContainer()) {
		if s.Codec == "mov_text" {
			t.Errorf("mov_text track must not reach the matroska container")
		}
	}
}
