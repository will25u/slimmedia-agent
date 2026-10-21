package ffmpeg

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/will25u/slimmedia-agent/internal/jobpaths"
	"github.com/will25u/slimmedia-agent/internal/jobspec"
	"github.com/will25u/slimmedia-agent/internal/protocol"
)

// TestBuildFfmpegArgsLegacy verifies a pre-tuning payload (none of the new
// fields set) produces exactly the argv the agent always built.
func TestBuildFfmpegArgsLegacy(t *testing.T) {
	job := TranscodeJob{
		InputFile:  "/media/in.mkv",
		OutputFile: "/media/out.mkv",
		Codec:      "libx264",
		Spec: jobspec.JobSpec{
			CRF:         30,
			PresetSpeed: "ultrafast",
		},
	}
	want := []string{
		"-hide_banner", "-nostdin", "-y", "-fflags", "+genpts", "-probesize", "10000000", "-analyzeduration", "10000000",
		"-protocol_whitelist", "file,pipe", "-i", "/media/in.mkv",
		"-map", "0:v?", "-map", "0:a?", "-map", "0:s?",
		"-ignore_unknown", "-map_metadata", "0", "-map_chapters", "0",
		"-max_muxing_queue_size", "4096", "-copyts", "-c:a", "copy",
		"-c:s", "copy",
		"-c:v", "libx264", "-crf", "30", "-preset", "ultrafast",
		"-progress", "pipe:1", "-nostats", "/media/out.mkv",
	}
	got, err := buildFfmpegArgs(job, nil)
	if err != nil {
		t.Fatalf("buildFfmpegArgs: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("legacy argv mismatch:\n got: %q\nwant: %q", got, want)
	}
}

// TestBuildFfmpegArgsFullPreset is the definition-of-done check: a preset
// with every 000012 tuning field set produces the expected exact argv.
func TestBuildFfmpegArgsFullPreset(t *testing.T) {

	caps := EncoderCaps{HasLibx265: true, RecommendedEncoder: "libx265"}
	job := TranscodeJob{
		InputFile:  "/media/in.mkv",
		OutputFile: "/media/out.mkv",
		Codec:      "hevc",
		Spec: jobspec.JobSpec{
			Container:        "mkv",
			EncoderEngine:    "cpu",
			QualityTarget:    22,
			AudioHandling:    "copy_all",
			SubtitleHandling: "copy_all",
			ColorDepth:       "10bit",
			FilmGrainSao:     false,
			AqMode:           2,
			ContentTuning:    "film",
			CustomFlags:      "keyint=60",
		},
		Caps: &caps,
	}
	want := []string{
		"-hide_banner", "-nostdin", "-y", "-fflags", "+genpts", "-probesize", "10000000", "-analyzeduration", "10000000",
		"-protocol_whitelist", "file,pipe", "-i", "/media/in.mkv",
		"-map", "0:v?", "-map", "0:a?", "-map", "0:s?",
		"-ignore_unknown", "-map_metadata", "0", "-map_chapters", "0",
		"-max_muxing_queue_size", "4096", "-copyts", "-c:a", "copy",
		"-c:s", "copy",
		"-c:v", "libx265", "-crf", "22",
		"-pix_fmt", "yuv420p10le",
		"-preset", "medium",
		"-x265-params", "aq-mode=2:sao=0:tune=film:keyint=60",
		"-progress", "pipe:1", "-nostats", "/media/out.mkv",
	}
	got, err := buildFfmpegArgs(job, tuningTestStreams)
	if err != nil {
		t.Fatalf("buildFfmpegArgs: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("full-preset argv mismatch:\n got: %q\nwant: %q", got, want)
	}
}

// TestBuildFfmpegArgsHardwarePreset covers the NVENC path: p010le for 10-bit
// hardware HEVC, vbr/cq quality args, the verified tuning block, the p6
// default preset, explicit text-subtitle mapping for MP4, and no
// -x265-params.
func TestBuildFfmpegArgsHardwarePreset(t *testing.T) {

	caps := EncoderCaps{HasNvencHEVC: true, HasNvencTuningVerified: true, RecommendedEncoder: "hevc_nvenc"}
	job := TranscodeJob{
		InputFile:  "/media/in.mkv",
		OutputFile: "/media/out.mp4",
		Codec:      "hevc",
		Spec: jobspec.JobSpec{
			Container:        "mp4",
			EncoderEngine:    "nvenc",
			QualityTarget:    22,
			AudioHandling:    "copy_all",
			SubtitleHandling: "copy_all",
			ColorDepth:       "10bit",
			FilmGrainSao:     true,
			AqMode:           3,
			ContentTuning:    "grain",
			CustomFlags:      "keyint=120",
		},
		Caps: &caps,
	}
	want := []string{
		"-hide_banner", "-nostdin", "-y", "-fflags", "+genpts", "-probesize", "10000000", "-analyzeduration", "10000000",
		"-protocol_whitelist", "file,pipe", "-i", "/media/in.mkv",
		"-map", "0:v?", "-map", "0:a?", "-map", "0:s:1",
		"-ignore_unknown", "-map_metadata", "0", "-map_chapters", "0",
		"-max_muxing_queue_size", "4096", "-copyts", "-c:a", "copy",
		"-movflags", "+faststart",
		"-c:v", "hevc_nvenc", "-rc:v", "vbr", "-cq:v", "22", "-b:v", "0",
		"-profile:v", "main10", "-pix_fmt", "p010le",
		"-g", "240",
		"-tune", "hq", "-multipass", "fullres", "-spatial-aq", "1", "-temporal-aq", "1", "-b_ref_mode", "middle",
		"-color_primaries", "bt709", "-color_trc", "bt709", "-colorspace", "bt709",
		"-preset", "p6",
		"-progress", "pipe:1", "-nostats", "/media/out.mp4",
	}
	got, err := buildFfmpegArgs(job, tuningTestStreams)
	if err != nil {
		t.Fatalf("buildFfmpegArgs: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("hardware-preset argv mismatch:\n got: %q\nwant: %q", got, want)
	}
}

// --- Runner-level tests with fake ffmpeg/ffprobe binaries ---

const fakeFFprobeScript = `#!/bin/sh
cat <<'EOF'
{"streams":[
 {"codec_type":"video","codec_name":"hevc"},
 {"codec_type":"audio","codec_name":"truehd","disposition":{"default":1},"tags":{"language":"eng"}},
 {"codec_type":"audio","codec_name":"ac3","disposition":{"default":0},"tags":{"language":"jpn"}},
 {"codec_type":"subtitle","codec_name":"hdmv_pgs_subtitle","disposition":{"default":1},"tags":{"language":"eng"}},
 {"codec_type":"subtitle","codec_name":"subrip","disposition":{"default":0},"tags":{"language":"eng"}}
]}
EOF
`

// fakeChunkOnlyStreamJSON is the stream-layout document every intermediate
// chunk (split or encoded) must report under video-only chunk handling
// (-map 0:v:0 -an -sn -dn): exactly one video stream, nothing else.
// verifyChunkVideoOnly asserts this invariant for split output and after
// every chunk encode.
const fakeChunkOnlyStreamJSON = `{"streams":[{"codec_type":"video","codec_name":"hevc"}]}`

// chunkAwareProbeScript wraps an existing ffprobe payload so probed paths
// ending in chunk-*.mkv (encoded chunks) or split-*.mkv (segment-muxer split
// chunks) answer with the pure-video stream layout while every other path
// (source probes, demux verification, chapter probes) gets the original
// payload.
func chunkAwareProbeScript(otherPayload string) string {
	return `#!/bin/sh
last=""
for a in "$@"; do last="$a"; done
case "$last" in
*/chunk-*.mkv|*/split-*.mkv)
printf '%s\n' '` + fakeChunkOnlyStreamJSON + `'
;;
*)
` + otherPayload + `
;;
esac
`
}

// fakeToolChunkAwareFFprobe installs an ffprobe fake with the standard
// multi-stream source layout for ordinary paths and the pure-video layout
// for encoded chunks (see chunkAwareProbeScript).
func fakeToolChunkAwareFFprobe(t *testing.T, dir string) string {
	t.Helper()
	other := `cat <<'EOF'
{"streams":[
 {"codec_type":"video","codec_name":"hevc"},
 {"codec_type":"audio","codec_name":"truehd","disposition":{"default":1},"tags":{"language":"eng"}},
 {"codec_type":"audio","codec_name":"ac3","disposition":{"default":0},"tags":{"language":"jpn"}},
 {"codec_type":"subtitle","codec_name":"hdmv_pgs_subtitle","disposition":{"default":1},"tags":{"language":"eng"}},
 {"codec_type":"subtitle","codec_name":"subrip","disposition":{"default":0},"tags":{"language":"eng"}}
]}
EOF`
	return fakeTool(t, dir, "ffprobe", chunkAwareProbeScript(other))
}

// fakeTool writes an executable shell script named name into dir.
func fakeTool(t *testing.T, dir, name, script string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write fake %s: %v", name, err)
	}
	return p
}

// readFakeArgs reads NUL-separated argv recorded by the fake ffmpeg.
func readFakeArgs(t *testing.T, path string) [][]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("fake ffmpeg recorded no invocations: %v", err)
	}
	var invocations [][]string
	for _, rec := range strings.Split(strings.TrimRight(string(data), "\x00"), "\x00\x00") {
		if rec == "" {
			continue
		}
		invocations = append(invocations, strings.Split(rec, "\x00"))
	}
	return invocations
}

// fakeFFmpegWithSplit creates a fake ffmpeg that records argv (like fakeTool)
// AND materializes outputs for the two stream-copy passes that write into the
// segment dir: when invoked as a segment muxer (-f segment) it creates
// numSplitFiles split-NNN.mkv files from the output pattern arg plus a
// segment timing CSV at the -segment_list path (nominal 300s boundaries,
// matching the runner's default segmentSecs), and when invoked as the
// upfront demux pass (maps 0:s?) it writes the demux container (the last
// arg). This satisfies splitSourceIntoChunks and extractSidecarStreams
// without real FFmpeg.
func fakeFFmpegWithSplit(t *testing.T, dir, argsFile string, numSplitFiles int) string {
	t.Helper()
	// The last argument to the segment muxer call is the output pattern,
	// e.g. /path/to/segments/split-%03d.mkv. We use sed to substitute the index.
	script := fmt.Sprintf(`#!/bin/sh
{ for a in "$@"; do printf '%%s\000' "$a"; done; printf '\000'; } >> %q
is_seg=0
is_demux=0
seg_list=""
prev=""
pat=""
for a in "$@"; do
  [ "$a" = "segment" ] && is_seg=1
  case "$a" in 0:s*) is_demux=1 ;; esac
  if [ "$prev" = "-segment_list" ]; then seg_list="$a"; fi
  prev="$a"
  pat="$a"
done
if [ "$is_seg" = "1" ]; then
  if [ -n "$seg_list" ]; then
    : > "$seg_list"
  fi
  i=0
  while [ "$i" -lt %d ]; do
    idx=$(printf '%%03d' "$i")
    out=$(echo "$pat" | sed "s/%%03d/$idx/")
    touch "$out"
    if [ -n "$seg_list" ]; then
      start=$(awk -v i="$i" 'BEGIN{printf "%%.6f", i*300}')
      end=$(awk -v i="$i" 'BEGIN{printf "%%.6f", (i+1)*300}')
      printf '%%s,%%s,%%s\n' "$out" "$start" "$end" >> "$seg_list"
    fi
    i=$((i+1))
  done
fi
if [ "$is_demux" = "1" ]; then
  printf 'x' > "$pat"
fi
exit 0
`, argsFile, numSplitFiles)
	return fakeTool(t, dir, "ffmpeg", script)
}



// TestRunTranscodeRejectsInvalidCustomFlags: a custom_flags dry-run failure
// must reject the job with the ffmpeg stderr, before any job encode runs.
func TestRunTranscodeRejectsInvalidCustomFlags(t *testing.T) {
	tmp := t.TempDir()
	// Stream-copy passes (demux + segment split) succeed; the lavfi custom
	// flags dry-run fails with ffmpeg stderr.
	fakeFFmpeg := fakeTool(t, tmp, "ffmpeg", `#!/bin/sh
is_seg=0
is_demux=0
pat=""
for a in "$@"; do
  [ "$a" = "segment" ] && is_seg=1
  case "$a" in 0:s*) is_demux=1 ;; esac
  pat="$a"
done
if [ "$is_demux" = "1" ]; then
  printf 'x' > "$pat"
  exit 0
fi
if [ "$is_seg" = "1" ]; then
  out=$(echo "$pat" | sed "s/%03d/000/")
  touch "$out"
  exit 0
fi
printf 'x265 [error]: bad thing happened\n' >&2
exit 1
`)
	fakeTool(t, tmp, "ffprobe", chunkAwareProbeScript(strings.TrimPrefix(fakeFFprobeScript, "#!/bin/sh\n")))

	progress := make(chan Progress, 16)
	job := TranscodeJob{
		FFmpegPath: fakeFFmpeg,
		InputFile:  filepath.Join(tmp, "in.mkv"),
		OutputFile: filepath.Join(tmp, "out.mkv"),
		Codec:      "libx265",
		Spec: jobspec.JobSpec{
			Version:       protocol.CurrentJobSpecVersion,
			EncoderEngine: "cpu",
			AudioHandling: "copy_all",
			CustomFlags:   "definitely-not-a-real-param=1",
		},
		ProgressChan: progress,
	}

	err := runWithTimeout(t, job)
	if err == nil {
		t.Fatal("expected custom-flags rejection, got nil error")
	}
	if !strings.Contains(err.Error(), "bad thing happened") {
		t.Errorf("rejection must carry ffmpeg stderr, got: %v", err)
	}
	assertChannelClosed(t, progress)
}

// TestRunTranscodeTuningEndToEnd runs the full arg-building pipeline against
// fake ffmpeg/ffprobe binaries and asserts the exact argv of the encode
// invocation, plus that custom flags were validated via a lavfi dry-run
// first and passed as a single -x265-params argument.
func TestRunTranscodeTuningEndToEnd(t *testing.T) {
	tmp := t.TempDir()
	argsFile := filepath.Join(tmp, "args.bin")
	// Each invocation records its argv NUL-separated, invocations separated
	// by a double NUL.
	fakeFFmpeg := fakeFFmpegWithSplit(t, tmp, argsFile, 1)
	fakeToolChunkAwareFFprobe(t, tmp)

	caps := EncoderCaps{HasLibx265: true, RecommendedEncoder: "libx265"}
	progress := make(chan Progress, 16)
	job := TranscodeJob{
		FFmpegPath: fakeFFmpeg,
		InputFile:  filepath.Join(tmp, "in.mkv"),
		OutputFile: filepath.Join(tmp, "out.mkv"),
		Codec:      "hevc",
		Spec: jobspec.JobSpec{
			Version:          1,
			Container:        "mkv",
			EncoderEngine:    "cpu",
			QualityTarget:    22,
			AudioHandling:    "copy_all",
			SubtitleHandling: "copy_all",
			ColorDepth:       "10bit",
			AqMode:           2,
			ContentTuning:    "film",
			CustomFlags:      "keyint=60:scenecut=40",
		},
		Caps:         &caps,
		ProgressChan: progress,
	}

	if err := runWithTimeout(t, job); err != nil {
		t.Fatalf("RunTranscode failed: %v", err)
	}
	assertChannelClosed(t, progress)

	invocations := readFakeArgs(t, argsFile)
	if len(invocations) != 5 {
		t.Fatalf("expected 5 ffmpeg invocations (demux + split + dry-run + encode + concat), got %d", len(invocations))
	}

	// First invocation is the upfront demux stream-copy pass.
	demuxInv := invocations[0]
	paths := jobpaths.FromOutput(job.OutputFile)
	wantDemux := buildDemuxArgs(job.InputFile, paths.DemuxContainer(), demuxableSubtitleIndexes(job, ProbeStreams(filepath.Join(tmp, "ffprobe"), job.InputFile)))
	if !reflect.DeepEqual(demuxInv, wantDemux) {
		t.Errorf("demux argv mismatch:\n got: %q\nwant: %q", demuxInv, wantDemux)
	}

	// Second invocation is the segment-muxer stream-copy split pass.
	splitInv := invocations[1]
	splitStr := strings.Join(splitInv, " ")
	for _, piece := range []string{"-f segment", "-c copy", "-segment_time", "-reset_timestamps"} {
		if !strings.Contains(splitStr, piece) {
			t.Errorf("split invocation missing %q: %q", piece, splitStr)
		}
	}

	// Third invocation is the custom-flags lavfi dry-run validation.
	dryRun := invocations[2]
	dryRunStr := strings.Join(dryRun, " ")
	for _, piece := range []string{"-f lavfi", "color=black:s=1920x1080:d=1", "-c:v libx265", "-x265-params", "keyint=60:scenecut=40", "-f null -"} {
		if !strings.Contains(dryRunStr, piece) {
			t.Errorf("dry-run invocation missing %q: %q", piece, dryRunStr)
		}
	}

	// Fourth invocation is the chunk encode — input is the split chunk, no -ss/-t flags.
	chunkInput := paths.SplitChunk(0)
	chunkOutput := paths.EncodedChunk(0)
	wantChunk := []string{
		"-hide_banner", "-nostdin", "-y", "-fflags", "+genpts", "-probesize", "10000000", "-analyzeduration", "10000000",
		"-protocol_whitelist", "file,pipe",
		"-i", chunkInput,
		"-map", "0:v:0", "-ignore_unknown", "-map_metadata", "0", "-map_chapters", "0",
		"-max_muxing_queue_size", "4096", "-an", "-sn", "-dn",
		"-c:v", "libx265", "-fps_mode", "passthrough", "-crf", "22",
		"-pix_fmt", "yuv420p10le",
		"-preset", "medium",
		"-x265-params", "aq-mode=2:sao=0:tune=film:keyint=60:scenecut=40",
		"-progress", "pipe:1", "-nostats", chunkOutput,
	}
	if !reflect.DeepEqual(invocations[3], wantChunk) {
		t.Errorf("chunk encode argv mismatch:\n got: %q\nwant: %q", invocations[3], wantChunk)
	}

	// Fifth invocation is the concat command — local stitch from the concat
	// manifest and the demux scratch container; the remote source is absent.
	concatFile := paths.ConcatFile()
	wantConcat := []string{
		"-hide_banner", "-y", "-f", "concat", "-safe", "0",
		"-protocol_whitelist", "file,pipe",
		"-i", concatFile, "-copyts", "-i", paths.DemuxContainer(),
		"-map", "0", "-map", "1:a?", "-map", "1:s?", "-c", "copy", "-map_chapters", "1", job.OutputFile,
	}
	if !reflect.DeepEqual(invocations[4], wantConcat) {
		t.Errorf("concat argv mismatch:\n got: %q\nwant: %q", invocations[4], wantConcat)
	}
	for _, arg := range invocations[4] {
		if arg == job.InputFile {
			t.Errorf("remote source path must not appear in the concat argv: %q", invocations[4])
		}
	}
}

// TestRunTranscodeLegacyArgvUnchanged proves old-payload jobs (no tuning
// fields) never probe and build the historical argv.
func TestRunTranscodeLegacyArgvUnchanged(t *testing.T) {
	tmp := t.TempDir()
	argsFile := filepath.Join(tmp, "args.bin")
	fakeFFmpeg := fakeTool(t, tmp, "ffmpeg", "#!/bin/sh\n{ for a in \"$@\"; do printf '%s\\000' \"$a\"; done; printf '\\000'; } >> \""+argsFile+"\"\nexit 0\n")
	// A fake ffprobe that would fail the test if probed: legacy jobs must not
	// probe at all.
	fakeTool(t, tmp, "ffprobe", "#!/bin/sh\nexit 99\n")

	progress := make(chan Progress, 16)
	job := TranscodeJob{
		FFmpegPath: fakeFFmpeg,
		InputFile:  filepath.Join(tmp, "in.mkv"),
		OutputFile: filepath.Join(tmp, "out.mkv"),
		Codec:      "libx264",
		Spec: jobspec.JobSpec{
			CRF:         30,
			PresetSpeed: "ultrafast",
		},
		ProgressChan: progress,
	}

	if err := runWithTimeout(t, job); err != nil {
		t.Fatalf("RunTranscode failed: %v", err)
	}
	assertChannelClosed(t, progress)

	invocations := readFakeArgs(t, argsFile)
	if len(invocations) != 1 {
		t.Fatalf("legacy job must invoke ffmpeg exactly once (no dry-run, no probe), got %d", len(invocations))
	}
	want := []string{
		"-hide_banner", "-nostdin", "-y", "-fflags", "+genpts", "-probesize", "10000000", "-analyzeduration", "10000000",
		"-protocol_whitelist", "file,pipe", "-i", job.InputFile,
		"-map", "0:v?", "-map", "0:a?", "-map", "0:s?",
		"-ignore_unknown", "-map_metadata", "0", "-map_chapters", "0",
		"-max_muxing_queue_size", "4096", "-copyts", "-c:a", "copy",
		"-c:s", "copy",
		"-c:v", "libx264", "-crf", "30", "-preset", "ultrafast",
		"-progress", "pipe:1", "-nostats", job.OutputFile,
	}
	if !reflect.DeepEqual(invocations[0], want) {
		t.Errorf("legacy argv mismatch:\n got: %q\nwant: %q", invocations[0], want)
	}
}

func TestRunTranscodeQuietHoursPauseAndResume(t *testing.T) {
	tmp := t.TempDir()
	argsFile := filepath.Join(tmp, "args.bin")
	fakeFFmpeg := fakeFFmpegWithSplit(t, tmp, argsFile, 2)
	fakeTool(t, tmp, "ffprobe", chunkAwareProbeScript("printf 'index=0,codec_type=video,codec_name=h264\\n'"))

	// Schedule where hour 2 is quiet hours (active) and all other hours are inactive
	gridBytes := make([]byte, 168)
	for i := range gridBytes {
		gridBytes[i] = '0'
	}
	gridBytes[2] = '1' // Sunday 02:00 is quiet hours

	sched := &protocol.ScheduleConfig{
		QuietHoursEnabled: true,
		ScheduleGrid:      protocol.WeeklySchedule(gridBytes),
	}

	callCount := 0
	// Base Sunday date
	t0 := time.Date(2026, 9, 20, 1, 0, 0, 0, time.UTC)       // 01:00 (not quiet)
	tQuiet := time.Date(2026, 9, 20, 2, 0, 0, 0, time.UTC)   // 02:00 (quiet hours active)
	tResumed := time.Date(2026, 9, 20, 3, 0, 0, 0, time.UTC) // 03:00 (quiet hours ended)

	nowFunc := func() time.Time {
		callCount++
		if callCount == 1 {
			return t0 // before chunk 0: not quiet
		}
		if callCount == 2 {
			return tQuiet // before chunk 1: quiet hours active! triggers pause
		}
		return tResumed // subsequent calls: quiet hours ended! resumes
	}

	var pauseEvents []bool
	job := TranscodeJob{
		FFmpegPath: fakeFFmpeg,
		InputFile:  filepath.Join(tmp, "in.mkv"),
		OutputFile: filepath.Join(tmp, "out.mkv"),
		Codec:      "hevc",
		Spec: jobspec.JobSpec{
			Container:     "mkv",
			EncoderEngine: "cpu",
			QualityTarget: 22,
		},
		DurationSec:  600.0, // 2 segments (300s each)
		Schedule:     sched,
		PollInterval: 10 * time.Millisecond,
		NowFunc:      nowFunc,
		OnPause: func(paused bool) {
			pauseEvents = append(pauseEvents, paused)
		},
	}

	if err := runWithTimeout(t, job); err != nil {
		t.Fatalf("RunTranscode failed: %v", err)
	}

	if len(pauseEvents) != 2 || !pauseEvents[0] || pauseEvents[1] {
		t.Errorf("expected pause events [true, false], got %v", pauseEvents)
	}

	invocations := readFakeArgs(t, argsFile)
	// split + chunk 0 + chunk 1 + concat = 4 invocations minimum
	if len(invocations) < 4 {
		t.Fatalf("expected at least 4 ffmpeg invocations (split, chunk 0, chunk 1, concat), got %d", len(invocations))
	}
}

func TestRunTranscodeQuietHoursDynamicEnable(t *testing.T) {
	tmp := t.TempDir()
	argsFile := filepath.Join(tmp, "args.bin")
	fakeFFmpeg := fakeFFmpegWithSplit(t, tmp, argsFile, 2)
	fakeTool(t, tmp, "ffprobe", chunkAwareProbeScript("printf 'index=0,codec_type=video,codec_name=h264\\n'"))

	// Initially disabled quiet hours
	activeGrid := strings.Repeat("1", 168)
	schedCall := 0
	getSchedule := func() *protocol.ScheduleConfig {
		schedCall++
		if schedCall == 1 {
			return &protocol.ScheduleConfig{QuietHoursEnabled: false}
		}
		if schedCall < 4 {
			return &protocol.ScheduleConfig{
				QuietHoursEnabled: true,
				ScheduleGrid:      protocol.WeeklySchedule(activeGrid),
			}
		}
		return &protocol.ScheduleConfig{QuietHoursEnabled: false}
	}

	var pauseEvents []bool
	job := TranscodeJob{
		FFmpegPath: fakeFFmpeg,
		InputFile:  filepath.Join(tmp, "in.mkv"),
		OutputFile: filepath.Join(tmp, "out.mkv"),
		Codec:      "hevc",
		Spec: jobspec.JobSpec{
			Container:     "mkv",
			EncoderEngine: "cpu",
			QualityTarget: 22,
		},
		DurationSec:  600.0, // 2 segments (300s each)
		GetSchedule:  getSchedule,
		PollInterval: 10 * time.Millisecond,
		OnPause: func(paused bool) {
			pauseEvents = append(pauseEvents, paused)
		},
	}

	if err := runWithTimeout(t, job); err != nil {
		t.Fatalf("RunTranscode failed: %v", err)
	}

	if len(pauseEvents) != 2 || !pauseEvents[0] || pauseEvents[1] {
		t.Errorf("expected dynamic pause events [true, false], got %v", pauseEvents)
	}
}

func TestRunTranscodeMP4ConcatOmitsBitmapSubtitles(t *testing.T) {
	tmp := t.TempDir()
	argsFile := filepath.Join(tmp, "args.bin")
	fakeFFmpeg := fakeFFmpegWithSplit(t, tmp, argsFile, 1)
	fakeToolChunkAwareFFprobe(t, tmp)

	caps := EncoderCaps{HasLibx265: true, RecommendedEncoder: "libx265"}
	progress := make(chan Progress, 16)
	job := TranscodeJob{
		FFmpegPath: fakeFFmpeg,
		InputFile:  filepath.Join(tmp, "in.mkv"),
		OutputFile: filepath.Join(tmp, "out.mp4"),
		Codec:      "hevc",
		Spec: jobspec.JobSpec{
			Version:          1,
			Container:        "mp4",
			EncoderEngine:    "cpu",
			QualityTarget:    22,
			AudioHandling:    "copy_all",
			SubtitleHandling: "copy_all",
			ColorDepth:       "10bit",
		},
		Caps:         &caps,
		ProgressChan: progress,
	}

	if err := runWithTimeout(t, job); err != nil {
		t.Fatalf("RunTranscode failed: %v", err)
	}
	assertChannelClosed(t, progress)

	invocations := readFakeArgs(t, argsFile)
	if len(invocations) < 2 {
		t.Fatalf("expected at least 2 ffmpeg invocations (chunk + concat), got %d", len(invocations))
	}

	concatInv := invocations[len(invocations)-1]
	concatFile := jobpaths.FromOutput(job.OutputFile).ConcatFile()
	wantConcat := []string{
		"-hide_banner", "-y", "-f", "concat", "-safe", "0",
		"-protocol_whitelist", "file,pipe",
		"-i", concatFile, "-copyts", "-i", jobpaths.FromOutput(job.OutputFile).DemuxContainer(),
		"-map", "0", "-map", "1:a?", "-map", "1:s:1", "-c", "copy", "-map_chapters", "1", "-movflags", "+faststart", job.OutputFile,
	}
	if !reflect.DeepEqual(concatInv, wantConcat) {
		t.Errorf("concat argv mismatch for MP4 output:\n got: %q\nwant: %q", concatInv, wantConcat)
	}
}

// TestRunTranscodeHardwareEndToEnd verifies the complete transcode pipeline
// lifecycle for hardware encoders (NVENC): the resolved hardware engine runs
// one full-stream encode — custom flags dry-run validation first, then a
// single pass with hardware tuning flags and MP4 web-streaming flags. The
// demux, split, and concat phases of the chunked pipeline do not run at all.
func TestRunTranscodeHardwareEndToEnd(t *testing.T) {
	tmp := t.TempDir()
	argsFile := filepath.Join(tmp, "args.bin")
	fakeFFmpeg := fakeFFmpegWithSplit(t, tmp, argsFile, 1)
	fakeToolChunkAwareFFprobe(t, tmp)

	caps := EncoderCaps{HasNvencHEVC: true, HasNvencTuningVerified: true, RecommendedEncoder: "hevc_nvenc"}
	progress := make(chan Progress, 16)
	job := TranscodeJob{
		FFmpegPath: fakeFFmpeg,
		InputFile:  filepath.Join(tmp, "in.mkv"),
		OutputFile: filepath.Join(tmp, "out.mp4"),
		Codec:      "hevc",
		Spec: jobspec.JobSpec{
			Version:          1,
			Container:        "mp4",
			EncoderEngine:    "nvenc",
			QualityTarget:    22,
			AudioHandling:    "copy_all",
			SubtitleHandling: "copy_all",
			ColorDepth:       "10bit",
			CustomFlags:      "-spatial_aq 1",
			StripMetadata:    true,
		},
		Caps:         &caps,
		ProgressChan: progress,
	}

	if err := runWithTimeout(t, job); err != nil {
		t.Fatalf("RunTranscode failed: %v", err)
	}
	assertChannelClosed(t, progress)

	invocations := readFakeArgs(t, argsFile)
	if len(invocations) != 2 {
		t.Fatalf("expected 2 ffmpeg invocations (custom-flags dry-run + full-stream encode), got %d", len(invocations))
	}

	// 1. First invocation: raw flags dry-run validation with hevc_nvenc.
	dryRun := invocations[0]
	dryRunStr := strings.Join(dryRun, " ")
	for _, piece := range []string{"-f lavfi", "color=black:s=1920x1080:d=1", "-c:v hevc_nvenc", "-spatial_aq 1"} {
		if !strings.Contains(dryRunStr, piece) {
			t.Errorf("hardware dry-run invocation missing %q: %q", piece, dryRunStr)
		}
	}

	// 2. Second invocation: the full-stream encode — the source is read once,
	// audio and subtitles are mapped per the preset, and no chunk-phase
	// artifacts (segment dir, split chunks, concat) exist.
	wantEncode := []string{
		"-hide_banner", "-nostdin", "-y", "-fflags", "+genpts", "-probesize", "10000000", "-analyzeduration", "10000000",
		"-protocol_whitelist", "file,pipe",
		"-i", job.InputFile,
		"-map", "0:v?", "-map", "0:a?", "-map", "0:s:1",
		"-ignore_unknown", "-map_metadata", "-1", "-map_chapters", "0",
		"-max_muxing_queue_size", "4096", "-copyts", "-c:a", "copy",
		"-movflags", "+faststart",
		"-c:v", "hevc_nvenc", "-rc:v", "vbr", "-cq:v", "22", "-b:v", "0",
		"-profile:v", "main10",
		"-pix_fmt", "p010le",
		"-g", "240",
		"-tune", "hq", "-multipass", "fullres", "-spatial-aq", "1", "-temporal-aq", "1", "-b_ref_mode", "middle",
		"-color_primaries", "bt709", "-color_trc", "bt709", "-colorspace", "bt709",
		"-preset", "p6",
		"-progress", "pipe:1", "-nostats",
		"-spatial_aq", "1",
		job.OutputFile,
	}
	if !reflect.DeepEqual(invocations[1], wantEncode) {
		t.Errorf("hardware full-stream encode argv mismatch:\n got: %q\nwant: %q", invocations[1], wantEncode)
	}

	hwPaths := jobpaths.FromOutput(job.OutputFile)
	if _, err := os.Stat(hwPaths.SegmentDir()); !os.IsNotExist(err) {
		t.Errorf("hardware encode must not create the segment directory, but %s exists", hwPaths.SegmentDir())
	}
}

// TestRunTranscodeSegmentedMultiChunkSplitFirst verifies that the Split-First
// architecture correctly: runs one split pass, then encodes each chunk from its
// pre-split file (no -ss/-t) as pure video (-map 0:v:0 -an -sn -dn), and
// finally concat-stitches cleanly.
func TestRunTranscodeSegmentedMultiChunkTimingAndSurvivingStreams(t *testing.T) {
	tmp := t.TempDir()
	argsFile := filepath.Join(tmp, "args.bin")
	fakeFFmpeg := fakeFFmpegWithSplit(t, tmp, argsFile, 2)

	probeScript := chunkAwareProbeScript(`cat <<'EOF'
{"streams":[
 {"codec_type":"video","codec_name":"hevc"},
 {"codec_type":"audio","codec_name":"truehd","disposition":{"default":1},"tags":{"language":"eng"}},
 {"codec_type":"audio","codec_name":"ac3","disposition":{"default":0},"tags":{"language":"jpn"}},
 {"codec_type":"audio","codec_name":"ac3","disposition":{"commentary":1},"tags":{"language":"eng","title":"Director's Commentary"}},
 {"codec_type":"subtitle","codec_name":"hdmv_pgs_subtitle","disposition":{"default":1},"tags":{"language":"eng"}},
 {"codec_type":"subtitle","codec_name":"subrip","disposition":{"default":0},"tags":{"language":"eng"}}
]}
EOF`)
	fakeTool(t, tmp, "ffprobe", probeScript)

	caps := EncoderCaps{HasLibx265: true, RecommendedEncoder: "libx265"}
	progress := make(chan Progress, 16)
	job := TranscodeJob{
		FFmpegPath: fakeFFmpeg,
		InputFile:  filepath.Join(tmp, "in.mkv"),
		OutputFile: filepath.Join(tmp, "out.mp4"),
		Codec:      "libx265",
		DurationSec: 600, // 2 chunks of 300s
		Spec: jobspec.JobSpec{
			Version:                 1,
			Container:               "mp4",
			EncoderEngine:           "cpu",
			QualityTarget:           22,
			AudioHandling:           "copy_all",
			SubtitleHandling:        "copy_all",
			PreferredAudioLanguages: []string{"eng"},
			AudioLanguageMode:       "whitelist",
			StripMetadata:           true,
		},
		Caps:         &caps,
		ProgressChan: progress,
	}

	if err := runWithTimeout(t, job); err != nil {
		t.Fatalf("RunTranscode failed: %v", err)
	}
	assertChannelClosed(t, progress)

	invocations := readFakeArgs(t, argsFile)
	if len(invocations) != 5 {
		t.Fatalf("expected 5 ffmpeg invocations (demux + split + chunk 0 + chunk 1 + concat), got %d", len(invocations))
	}

	paths := jobpaths.FromOutput(job.OutputFile)

	// Invocation 0: upfront demux stream-copy pass into the local scratch container.
	demuxWant := buildDemuxArgs(job.InputFile, paths.DemuxContainer(), demuxableSubtitleIndexes(job, ProbeStreams(filepath.Join(tmp, "ffprobe"), job.InputFile)))
	if !reflect.DeepEqual(invocations[0], demuxWant) {
		t.Errorf("demux argv mismatch:\n got: %q\nwant: %q", invocations[0], demuxWant)
	}

	// Invocation 1: segment-muxer stream-copy split pass.
	splitStr := strings.Join(invocations[1], " ")
	for _, piece := range []string{"-f segment", "-c copy", "-segment_time", "-reset_timestamps"} {
		if !strings.Contains(splitStr, piece) {
			t.Errorf("split invocation missing %q: %q", piece, splitStr)
		}
	}

	// Invocation 2: Chunk 0 encode — input is split-000.mkv, no -ss/-t flags.
	// Chunks are pure video: single 0:v:0 map with -an -sn -dn; audio hygiene
	// (commentary/jpn stripping) no longer applies to chunk encodes because
	// side streams live in the demux scratch container for final assembly.
	chunk0Want := []string{
		"-hide_banner", "-nostdin", "-y", "-fflags", "+genpts", "-probesize", "10000000", "-analyzeduration", "10000000",
		"-protocol_whitelist", "file,pipe",
		"-i", paths.SplitChunk(0),
		"-map", "0:v:0",
		"-ignore_unknown", "-map_metadata", "-1", "-map_chapters", "0",
		"-max_muxing_queue_size", "4096", "-an", "-sn", "-dn",
		"-c:v", "libx265", "-fps_mode", "passthrough", "-crf", "22",
		"-pix_fmt", "yuv420p10le",
		"-preset", "medium",
		"-x265-params", "sao=0",
		"-progress", "pipe:1", "-nostats",
		paths.EncodedChunk(0),
	}
	if !reflect.DeepEqual(invocations[2], chunk0Want) {
		t.Errorf("chunk 0 argv mismatch:\n got: %q\nwant: %q", invocations[2], chunk0Want)
	}

	// Invocation 3: Chunk 1 encode — input is split-001.mkv, no -ss/-t flags.
	chunk1Want := []string{
		"-hide_banner", "-nostdin", "-y", "-fflags", "+genpts", "-probesize", "10000000", "-analyzeduration", "10000000",
		"-protocol_whitelist", "file,pipe",
		"-i", paths.SplitChunk(1),
		"-map", "0:v:0",
		"-ignore_unknown", "-map_metadata", "-1", "-map_chapters", "0",
		"-max_muxing_queue_size", "4096", "-an", "-sn", "-dn",
		"-c:v", "libx265", "-fps_mode", "passthrough", "-crf", "22",
		"-pix_fmt", "yuv420p10le",
		"-preset", "medium",
		"-x265-params", "sao=0",
		"-progress", "pipe:1", "-nostats",
		paths.EncodedChunk(1),
	}
	if !reflect.DeepEqual(invocations[3], chunk1Want) {
		t.Errorf("chunk 1 argv mismatch:\n got: %q\nwant: %q", invocations[3], chunk1Want)
	}

	// Invocation 4: Concat — local stitch from the concat manifest and the
	// demux scratch container: audio copied from 1:a:0 after the preset's
	// hygiene (jpn dub stripped, commentary pruned — now applied at assembly
	// since chunks are video-only), text subtitle 1:s:1 kept, bitmap PGS
	// 1:s:0 dropped for MP4, chapters from the container.
	concatWant := []string{
		"-hide_banner", "-y", "-f", "concat", "-safe", "0",
		"-protocol_whitelist", "file,pipe",
		"-i", paths.ConcatFile(), "-copyts", "-i", paths.DemuxContainer(),
		"-map", "0", "-map", "1:a:0", "-map", "1:s:1", "-c", "copy",
		"-map_metadata", "-1", "-map_chapters", "1",
		"-movflags", "+faststart",
		job.OutputFile,
	}
	if !reflect.DeepEqual(invocations[4], concatWant) {
		t.Errorf("concat argv mismatch:\n got: %q\nwant: %q", invocations[4], concatWant)
	}
}


