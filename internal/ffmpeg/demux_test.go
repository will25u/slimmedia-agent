package ffmpeg

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/will25u/slimmedia-agent/internal/jobpaths"
)

func TestBuildDemuxArgs(t *testing.T) {
	want := []string{
		"-hide_banner", "-nostdin", "-nostats", "-y", "-probesize", "10000000", "-analyzeduration", "10000000",
		"-i", "/media/in.mkv",
		"-map", "0:a?",
		"-map", "0:s:0", "-map", "0:s:1",
		"-map_metadata", "0", "-map_chapters", "0",
		"-c", "copy",
		"/scratch/seg/demux.mkv",
	}
	if got := buildDemuxArgs("/media/in.mkv", "/scratch/seg/demux.mkv", []int{0, 1}); !reflect.DeepEqual(got, want) {
		t.Errorf("buildDemuxArgs mismatch:\n got: %q\nwant: %q", got, want)
	}
}

// TestDemuxableSubtitleIndexesSkipsMovText locks the regression guard for
// MP4-sourced mov_text subtitles: the Matroska demux container cannot hold
// them, so they are skipped (and logged) while other tracks still extract.
// Crucially the surviving indexes stay anchored to the SOURCE subtitle
// positions (0:s:N) — a muxable track after a skipped one keeps its original
// index.
func TestDemuxableSubtitleIndexesSkipsMovText(t *testing.T) {
	job := TranscodeJob{InputFile: "/media/in.mp4"}
	streams := []StreamInfo{
		{Type: "audio", Codec: "aac"},
		{Type: "subtitle", Codec: "mov_text"},
		{Type: "subtitle", Codec: "subrip"},
		{Type: "subtitle", Codec: "mov_text"},
		{Type: "subtitle", Codec: "hdmv_pgs_subtitle"},
	}
	idx := demuxableSubtitleIndexes(job, streams)
	if !reflect.DeepEqual(idx, []int{1, 3}) {
		t.Fatalf("demuxable subtitle indexes = %v, want [1 3]", idx)
	}
}

// fakeDemuxFFmpeg records argv and materializes the demux container (last
// argument) whenever the invocation maps source subtitle streams — the
// signature of the upfront extraction pass. Other invocations just record.
const fakeDemuxFFmpegScript = `#!/bin/sh
{ for a in "$@"; do printf '%%s\000' "$a"; done; printf '\000'; } >> "%s"
is_demux=0
pat=""
for a in "$@"; do
  case "$a" in 0:s*) is_demux=1 ;; esac
  pat="$a"
done
if [ "$is_demux" = "1" ]; then
  printf 'x' > "$pat"
fi
exit 0
`

func TestExtractSidecarStreamsRunsExtractionAndVerification(t *testing.T) {
	tmp := t.TempDir()
	argsFile := filepath.Join(tmp, "args.bin")
	fakeFFmpeg := fakeTool(t, tmp, "ffmpeg", fmt.Sprintf(fakeDemuxFFmpegScript, argsFile))
	fakeTool(t, tmp, "ffprobe", fakeFFprobeScript)

	job := TranscodeJob{
		FFmpegPath: fakeFFmpeg,
		InputFile:  filepath.Join(tmp, "in.mkv"),
		OutputFile: filepath.Join(tmp, "out.mkv"),
	}
	paths := jobpaths.FromOutput(job.OutputFile)
	streams := ProbeStreams(filepath.Join(tmp, "ffprobe"), job.InputFile)
	if err := os.MkdirAll(paths.SegmentDir(), 0755); err != nil {
		t.Fatal(err)
	}

	if err := extractSidecarStreams(t.Context(), job, &paths, streams); err != nil {
		t.Fatalf("extractSidecarStreams: %v", err)
	}

	invocations := readFakeArgs(t, argsFile)
	if len(invocations) != 1 {
		t.Fatalf("expected exactly 1 ffmpeg invocation (demux), got %d", len(invocations))
	}
	want := buildDemuxArgs(job.InputFile, paths.DemuxContainer(), demuxableSubtitleIndexes(job, streams))
	if !reflect.DeepEqual(invocations[0], want) {
		t.Errorf("demux argv mismatch:\n got: %q\nwant: %q", invocations[0], want)
	}
	if fi, err := os.Stat(paths.DemuxContainer()); err != nil || fi.Size() == 0 {
		t.Errorf("demux container missing or empty after extraction: %v", err)
	}
}

func TestExtractSidecarStreamsSkipsSourcesWithoutSideStreams(t *testing.T) {
	tmp := t.TempDir()
	argsFile := filepath.Join(tmp, "args.bin")
	fakeFFmpeg := fakeTool(t, tmp, "ffmpeg", fmt.Sprintf(fakeDemuxFFmpegScript, argsFile))

	job := TranscodeJob{
		FFmpegPath: fakeFFmpeg,
		InputFile:  filepath.Join(tmp, "in.mkv"),
		OutputFile: filepath.Join(tmp, "out.mkv"),
	}
	paths := jobpaths.FromOutput(job.OutputFile)

	if err := extractSidecarStreams(t.Context(), job, &paths, nil); err != nil {
		t.Fatalf("extractSidecarStreams with nil streams: %v", err)
	}
	if err := extractSidecarStreams(t.Context(), job, &paths, []StreamInfo{{Type: "video", Codec: "hevc"}}); err != nil {
		t.Fatalf("extractSidecarStreams with video-only streams: %v", err)
	}

	if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
		t.Errorf("no ffmpeg invocation expected for sources without audio/subtitle streams")
	}
	if _, err := os.Stat(paths.DemuxContainer()); !os.IsNotExist(err) {
		t.Errorf("no demux container expected when extraction is skipped")
	}
}

func TestExtractSidecarStreamsResumeReusesVerifiedContainer(t *testing.T) {
	tmp := t.TempDir()
	argsFile := filepath.Join(tmp, "args.bin")
	fakeFFmpeg := fakeTool(t, tmp, "ffmpeg", fmt.Sprintf(fakeDemuxFFmpegScript, argsFile))
	fakeTool(t, tmp, "ffprobe", fakeFFprobeScript)

	job := TranscodeJob{
		FFmpegPath: fakeFFmpeg,
		InputFile:  filepath.Join(tmp, "in.mkv"),
		OutputFile: filepath.Join(tmp, "out.mkv"),
	}
	paths := jobpaths.FromOutput(job.OutputFile)
	streams := ProbeStreams(filepath.Join(tmp, "ffprobe"), job.InputFile)

	if err := os.MkdirAll(paths.SegmentDir(), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.DemuxContainer(), []byte("already here"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := extractSidecarStreams(t.Context(), job, &paths, streams); err != nil {
		t.Fatalf("extractSidecarStreams resume: %v", err)
	}
	if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
		t.Errorf("verified existing container must not trigger a re-extraction ffmpeg call")
	}
}

func TestExtractSidecarStreamsReExtractsUnverifiableContainer(t *testing.T) {
	tmp := t.TempDir()
	argsFile := filepath.Join(tmp, "args.bin")
	fakeFFmpeg := fakeTool(t, tmp, "ffmpeg", fmt.Sprintf(fakeDemuxFFmpegScript, argsFile))
	fakeTool(t, tmp, "ffprobe", fakeFFprobeScript)

	job := TranscodeJob{
		FFmpegPath: fakeFFmpeg,
		InputFile:  filepath.Join(tmp, "in.mkv"),
		OutputFile: filepath.Join(tmp, "out.mkv"),
	}
	paths := jobpaths.FromOutput(job.OutputFile)
	streams := ProbeStreams(filepath.Join(tmp, "ffprobe"), job.InputFile)

	if err := os.MkdirAll(paths.SegmentDir(), 0755); err != nil {
		t.Fatal(err)
	}
	// An empty container fails verification and must be replaced.
	if err := os.WriteFile(paths.DemuxContainer(), nil, 0644); err != nil {
		t.Fatal(err)
	}

	if err := extractSidecarStreams(t.Context(), job, &paths, streams); err != nil {
		t.Fatalf("extractSidecarStreams re-extraction: %v", err)
	}
	invocations := readFakeArgs(t, argsFile)
	if len(invocations) != 1 {
		t.Fatalf("expected 1 re-extraction ffmpeg invocation, got %d", len(invocations))
	}
}

func TestExtractSidecarStreamsFailsJobOnDemuxError(t *testing.T) {
	tmp := t.TempDir()
	fakeFFmpeg := fakeTool(t, tmp, "ffmpeg", "#!/bin/sh\nprintf 'muxer exploded\n' >&2\nexit 1\n")
	fakeTool(t, tmp, "ffprobe", fakeFFprobeScript)

	job := TranscodeJob{
		FFmpegPath: fakeFFmpeg,
		InputFile:  filepath.Join(tmp, "in.mkv"),
		OutputFile: filepath.Join(tmp, "out.mkv"),
	}
	paths := jobpaths.FromOutput(job.OutputFile)
	streams := ProbeStreams(filepath.Join(tmp, "ffprobe"), job.InputFile)

	err := extractSidecarStreams(t.Context(), job, &paths, streams)
	if err == nil || !strings.Contains(err.Error(), "source demux failed") {
		t.Fatalf("expected source demux failure, got: %v", err)
	}
	if _, statErr := os.Stat(paths.DemuxContainer()); !os.IsNotExist(statErr) {
		t.Errorf("failed demux must not leave a container behind")
	}
}

// TestExtractSidecarStreamsCancelDuringDemux locks the cancellation contract:
// killing the demux process (CANCEL_JOB) must surface as a cancellation, not
// as a "source demux failed" hard failure carrying the process's full stderr
// (on Windows the killed process reports "exit status 1", which previously
// masqueraded as a real demux failure and flooded the tray UI with stderr).
func TestExtractSidecarStreamsCancelDuringDemux(t *testing.T) {
	tmp := t.TempDir()
	fakeFFmpeg := fakeTool(t, tmp, "ffmpeg", "#!/bin/sh\nprintf 'pgssub: unspecified size probe noise\n' >&2\nexec sleep 30\n")
	fakeTool(t, tmp, "ffprobe", fakeFFprobeScript)

	job := TranscodeJob{
		FFmpegPath: fakeFFmpeg,
		InputFile:  filepath.Join(tmp, "in.mkv"),
		OutputFile: filepath.Join(tmp, "out.mkv"),
	}
	paths := jobpaths.FromOutput(job.OutputFile)
	streams := ProbeStreams(filepath.Join(tmp, "ffprobe"), job.InputFile)
	if err := os.MkdirAll(paths.SegmentDir(), 0755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() {
		errCh <- extractSidecarStreams(ctx, job, &paths, streams)
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("cancelled demux must return an error")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("cancelled demux must wrap context.Canceled, got: %v", err)
		}
		if strings.Contains(err.Error(), "source demux failed") {
			t.Errorf("cancelled demux must not report a demux failure: %v", err)
		}
		if strings.Contains(err.Error(), "unspecified size") {
			t.Errorf("cancelled demux must not embed process stderr: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("extractSidecarStreams did not return promptly after cancel")
	}
}

// scriptedProbeFFprobe writes an ffprobe fake that emits sourceStreamsJSON
// when the probed path is the source file and demuxStreamsJSON otherwise.
func scriptedProbeFFprobe(t *testing.T, dir, sourcePath, sourceJSON, demuxJSON string) {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
last=""
for a in "$@"; do last="$a"; done
if [ "$last" = "%s" ]; then
cat <<'EOF'
%s
EOF
else
cat <<'EOF'
%s
EOF
fi
`, sourcePath, sourceJSON, demuxJSON)
	fakeTool(t, dir, "ffprobe", script)
}

func TestVerifyDemuxContainerMatches(t *testing.T) {
	tmp := t.TempDir()
	sourcePath := filepath.Join(tmp, "in.mkv")
	demuxPath := filepath.Join(tmp, "demux.mkv")
	if err := os.WriteFile(demuxPath, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	sourceJSON := `{"streams":[
	 {"codec_type":"audio","codec_name":"ac3","duration":"3600.000000"},
	 {"codec_type":"subtitle","codec_name":"subrip","tags":{"DURATION":"01:00:00.000000"}}
	]}`
	// Same tracks: ac3 with a slightly rounded duration (within tolerance),
	// subrip resolved from the DURATION tag.
	demuxJSON := `{"streams":[
	 {"codec_type":"audio","codec_name":"ac3","duration":"3600.040000"},
	 {"codec_type":"subtitle","codec_name":"subrip","tags":{"DURATION":"01:00:00.040000"}}
	]}`
	scriptedProbeFFprobe(t, tmp, sourcePath, sourceJSON, demuxJSON)
	ffprobePath := filepath.Join(tmp, "ffprobe")

	sourceStreams := ProbeStreams(ffprobePath, sourcePath)
	if len(sourceStreams) != 2 {
		t.Fatalf("source probe returned %d streams, want 2", len(sourceStreams))
	}
	if sourceStreams[1].Duration != 3600.0 {
		t.Errorf("DURATION tag parse: got %v, want 3600", sourceStreams[1].Duration)
	}

	if err := verifyDemuxContainer(ffprobePath, sourceStreams, 0, demuxPath); err != nil {
		t.Errorf("verifyDemuxContainer: %v", err)
	}
}

func TestVerifyDemuxContainerDetectsMismatch(t *testing.T) {
	tmp := t.TempDir()
	sourcePath := filepath.Join(tmp, "in.mkv")
	demuxPath := filepath.Join(tmp, "demux.mkv")
	if err := os.WriteFile(demuxPath, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	sourceJSON := `{"streams":[
	 {"codec_type":"audio","codec_name":"ac3","duration":"3600.000000"},
	 {"codec_type":"audio","codec_name":"truehd","duration":"3600.000000"},
	 {"codec_type":"subtitle","codec_name":"subrip","duration":"3600.000000"}
	]}`
	ffprobePath := filepath.Join(tmp, "ffprobe")

	cases := []struct {
		name     string
		demuxJSON string
		wantErr  string
	}{
		{
			name: "track count drop",
			demuxJSON: `{"streams":[
			 {"codec_type":"audio","codec_name":"ac3","duration":"3600.000000"}
			]}`,
			wantErr: "track count",
		},
		{
			name: "codec change",
			demuxJSON: `{"streams":[
			 {"codec_type":"audio","codec_name":"ac3","duration":"3600.000000"},
			 {"codec_type":"audio","codec_name":"aac","duration":"3600.000000"},
			 {"codec_type":"subtitle","codec_name":"subrip","duration":"3600.000000"}
			]}`,
			wantErr: "codec",
		},
		{
			name: "duration drift",
			demuxJSON: `{"streams":[
			 {"codec_type":"audio","codec_name":"ac3","duration":"3600.000000"},
			 {"codec_type":"audio","codec_name":"truehd","duration":"3595.000000"},
			 {"codec_type":"subtitle","codec_name":"subrip","duration":"3600.000000"}
			]}`,
			wantErr: "duration",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scriptedProbeFFprobe(t, tmp, sourcePath, sourceJSON, tc.demuxJSON)
			sourceStreams := ProbeStreams(ffprobePath, sourcePath)
			if len(sourceStreams) != 3 {
				t.Fatalf("source probe returned %d streams, want 3", len(sourceStreams))
			}
			err := verifyDemuxContainer(ffprobePath, sourceStreams, 0, demuxPath)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("expected error containing %q, got: %v", tc.wantErr, err)
			}
		})
	}
}

func TestVerifyDemuxContainerMissingOrEmpty(t *testing.T) {
	tmp := t.TempDir()
	demuxPath := filepath.Join(tmp, "demux.mkv")
	ffprobePath := filepath.Join(tmp, "ffprobe")

	if err := verifyDemuxContainer(ffprobePath, nil, -1, demuxPath); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("missing container: expected missing error, got: %v", err)
	}
	if err := os.WriteFile(demuxPath, nil, 0644); err != nil {
		t.Fatal(err)
	}
	if err := verifyDemuxContainer(ffprobePath, nil, -1, demuxPath); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("empty container: expected empty error, got: %v", err)
	}
}

// chapterProbeFFprobeScript is an ffprobe fake that answers every invocation
// with a fixed two-chapter document, mimicking a chapters-only source probe.
const chapterProbeFFprobeScript = `#!/bin/sh
cat <<'PROBE_EOF'
{"chapters":[
 {"start_time":"0.000000","end_time":"5.500000","tags":{"title":"Opening"}},
 {"start_time":"5.500000","end_time":"12.000000","tags":{"title":"End"}}
]}
PROBE_EOF
`

func TestExtractSidecarStreamsChapterOnlySourceWritesChapterFile(t *testing.T) {
	tmp := t.TempDir()
	argsFile := filepath.Join(tmp, "args.bin")
	fakeFFmpeg := fakeTool(t, tmp, "ffmpeg", fmt.Sprintf(fakeDemuxFFmpegScript, argsFile))
	fakeTool(t, tmp, "ffprobe", chapterProbeFFprobeScript)

	job := TranscodeJob{
		FFmpegPath: fakeFFmpeg,
		InputFile:  filepath.Join(tmp, "in.mkv"),
		OutputFile: filepath.Join(tmp, "out.mkv"),
	}
	paths := jobpaths.FromOutput(job.OutputFile)
	if err := os.MkdirAll(paths.SegmentDir(), 0755); err != nil {
		t.Fatal(err)
	}

	// Video-only source: no demux container possible, chapters go to the
	// ffmetadata sidecar instead.
	err := extractSidecarStreams(t.Context(), job, &paths, []StreamInfo{{Type: "video", Codec: "hevc"}})
	if err != nil {
		t.Fatalf("extractSidecarStreams chapters-only: %v", err)
	}
	if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
		t.Errorf("no ffmpeg invocation expected for a chapters-only source")
	}
	if _, err := os.Stat(paths.DemuxContainer()); !os.IsNotExist(err) {
		t.Errorf("no demux container expected for a chapters-only source")
	}
	data, err := os.ReadFile(paths.ChapterFile())
	if err != nil {
		t.Fatalf("chapter sidecar must be written: %v", err)
	}
	content := string(data)
	for _, piece := range []string{";FFMETADATA1", "[CHAPTER]", "START=0", "END=5500", "title=Opening", "END=12000", "title=End"} {
		if !strings.Contains(content, piece) {
			t.Errorf("chapter sidecar missing %q:\n%s", piece, content)
		}
	}
}

func TestExtractSidecarStreamsChapterFileReusedOnResume(t *testing.T) {
	tmp := t.TempDir()
	fakeFFmpeg := fakeTool(t, tmp, "ffmpeg", "#!/bin/sh\nexit 99\n")
	fakeTool(t, tmp, "ffprobe", chapterProbeFFprobeScript)

	job := TranscodeJob{
		FFmpegPath: fakeFFmpeg,
		InputFile:  filepath.Join(tmp, "in.mkv"),
		OutputFile: filepath.Join(tmp, "out.mkv"),
	}
	paths := jobpaths.FromOutput(job.OutputFile)
	if err := os.MkdirAll(paths.SegmentDir(), 0755); err != nil {
		t.Fatal(err)
	}
	existing := ";FFMETADATA1\n[CHAPTER]\nTIMEBASE=1/1000\nSTART=0\nEND=1000\ntitle=Kept\n"
	if err := os.WriteFile(paths.ChapterFile(), []byte(existing), 0644); err != nil {
		t.Fatal(err)
	}

	if err := extractSidecarStreams(t.Context(), job, &paths, nil); err != nil {
		t.Fatalf("extractSidecarStreams resume: %v", err)
	}
	data, err := os.ReadFile(paths.ChapterFile())
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != existing {
		t.Errorf("existing chapter sidecar must be reused, got:\n%s", data)
	}
}

func TestExtractSidecarStreamsNoChaptersLeavesNoSidecar(t *testing.T) {
	tmp := t.TempDir()
	fakeFFmpeg := fakeTool(t, tmp, "ffmpeg", "#!/bin/sh\nexit 99\n")
	fakeTool(t, tmp, "ffprobe", "#!/bin/sh\nprintf '{\"chapters\":[]}\\n'\n")

	job := TranscodeJob{
		FFmpegPath: fakeFFmpeg,
		InputFile:  filepath.Join(tmp, "in.mkv"),
		OutputFile: filepath.Join(tmp, "out.mkv"),
	}
	paths := jobpaths.FromOutput(job.OutputFile)

	if err := extractSidecarStreams(t.Context(), job, &paths, nil); err != nil {
		t.Fatalf("extractSidecarStreams no side streams: %v", err)
	}
	if _, err := os.Stat(paths.ChapterFile()); !os.IsNotExist(err) {
		t.Errorf("no chapter sidecar expected when the source has none")
	}
	if _, err := os.Stat(paths.DemuxContainer()); !os.IsNotExist(err) {
		t.Errorf("no demux container expected when the source has none")
	}
}

func TestWriteChapterMetadataSanitizesTitles(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "chapters.ffmeta")
	chapters := []Chapter{
		{Start: 0, End: 1500, Title: "Intro = prologue; v1 #1\\\nsplit"},
		{Start: 1500, End: 3000, Title: ""},
	}
	if err := writeChapterMetadata(t.Context(), path, chapters); err != nil {
		t.Fatalf("writeChapterMetadata: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	for _, piece := range []string{
		";FFMETADATA1",
		"START=0", "END=1500",
		`title=Intro \= prologue\; v1 \#1\\ split`,
		"START=1500", "END=3000",
	} {
		if !strings.Contains(content, piece) {
			t.Errorf("chapter metadata missing %q:\n%s", piece, content)
		}
	}
}
