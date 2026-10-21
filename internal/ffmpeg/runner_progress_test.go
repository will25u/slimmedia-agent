package ffmpeg

import (
	"path/filepath"
	"testing"

	"github.com/will25u/slimmedia-agent/internal/jobspec"
)

// fakeFFmpegWithProgress builds a fake ffmpeg that stream-copy-splits into 2
// chunks (segment muxer), then emits two -progress blocks per chunk encode
// (150s and 300s of chunk-relative output time) before exiting cleanly.
func fakeFFmpegWithProgress(t *testing.T, dir string) string {
	t.Helper()
	script := `#!/bin/sh
is_seg=0
is_demux=0
has_progress=0
out=""
for a in "$@"; do
  [ "$a" = "segment" ] && is_seg=1
  case "$a" in 0:s*) is_demux=1 ;; esac
  [ "$a" = "pipe:1" ] && has_progress=1
  out="$a"
done
if [ "$is_demux" = "1" ]; then
  printf 'x' > "$out"
  exit 0
fi
if [ "$is_seg" = "1" ]; then
  i=0
  while [ "$i" -lt 2 ]; do
    idx=$(printf '%03d' $i)
    f=$(echo "$out" | sed "s/%03d/$idx/")
    touch "$f"
    i=$((i+1))
  done
  exit 0
fi
if [ "$has_progress" = "1" ]; then
  cat <<'PROGRESS_EOF'
frame=3750
fps=62.5
out_time_us=150000000
speed=2.0
bitrate=2500kbits/s
progress=continue
frame=7500
fps=62.5
out_time_us=300000000
speed=2.0
bitrate=2500kbits/s
progress=end
PROGRESS_EOF
  exit 0
fi
exit 0
`
	return fakeTool(t, dir, "ffmpeg", script)
}

// TestRunTranscodeChunkProgressUsesRealChunkStarts verifies that per-chunk
// progress offsets come from the split phase's recorded timing manifest, not
// the nominal i*segmentSecs: a source whose second chunk starts late (no
// keyframe inside the first segment window) must report that chunk's progress
// against its real start time.
func TestRunTranscodeChunkProgressUsesRealChunkStarts(t *testing.T) {
	tmp := t.TempDir()

	oldSegmentSecs := segmentSecs
	segmentSecs = 2.0
	defer func() { segmentSecs = oldSegmentSecs }()

	// Split produces 2 chunks with real boundaries [0, 2.5] and [2.5, 5]
	// (nominal windows would be [0,2] and [2,4]); both chunk encodes report
	// 1s and 2s of chunk-relative output time.
	script := `#!/bin/sh
is_seg=0
is_demux=0
has_progress=0
seg_list=""
prev=""
out=""
for a in "$@"; do
  [ "$a" = "segment" ] && is_seg=1
  case "$a" in 0:s*) is_demux=1 ;; esac
  [ "$a" = "pipe:1" ] && has_progress=1
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
  printf '%s,0.000000,2.500000\n%s,2.500000,5.000000\n' "$dir/split-000.mkv" "$dir/split-001.mkv" > "$seg_list"
  exit 0
fi
if [ "$has_progress" = "1" ]; then
  cat <<'PROGRESS_EOF'
frame=25
fps=25.0
out_time_us=1000000
speed=2.0
bitrate=2500kbits/s
progress=continue
frame=50
fps=25.0
out_time_us=2000000
speed=2.0
bitrate=2500kbits/s
progress=end
PROGRESS_EOF
  exit 0
fi
exit 0
`
	fakeTool(t, tmp, "ffmpeg", script)
	fakeToolChunkAwareFFprobe(t, tmp)

	progress := make(chan Progress, 64)
	job := TranscodeJob{
		FFmpegPath:  filepath.Join(tmp, "ffmpeg"),
		InputFile:   filepath.Join(tmp, "in.mkv"),
		OutputFile:  filepath.Join(tmp, "out.mkv"),
		Codec:       "libx264",
		DurationSec: 5,
		TotalFrames: 125,
		Spec: jobspec.JobSpec{
			Version:       1,
			Container:     "mkv",
			EncoderEngine: "cpu",
			QualityTarget: 22,
		},
		ProgressChan: progress,
	}

	if err := runWithTimeout(t, job); err != nil {
		t.Fatalf("RunTranscode failed: %v", err)
	}

	var events []Progress
	for p := range progress {
		if p.Phase == "encode" {
			events = append(events, p)
		}
	}
	if len(events) == 0 {
		t.Fatal("expected encode progress events")
	}

	// Chunk 1 finished 2s into a chunk that really starts at 2.5s: the final
	// absolute position is 4.5s of 5s (90%), not the nominal 2s+2s=4s (80%).
	last := events[len(events)-1]
	if last.TimeSeconds != 4.5 {
		t.Fatalf("final TimeSeconds = %.2f, want 4.5 (real chunk start 2.5 + 2s chunk progress)", last.TimeSeconds)
	}
	if last.ProgressPct != 90 {
		t.Fatalf("final ProgressPct = %.2f, want 90", last.ProgressPct)
	}
	for _, p := range events {
		if p.TimeSeconds == 4.0 {
			t.Fatalf("event with TimeSeconds=4.0 found: nominal (not real) chunk start used: %+v", p)
		}
	}
}

// TestRunTranscodeChunkProgressAccumulatesBaseTime is the regression test for
// the Split-First progress sawtooth: chunk encodes run with a per-chunk
// baseTime (real chunk start from the timing manifest, or i*segmentSecs when
// the manifest is unavailable), so the -progress parser must add baseTime
// whenever it is nonzero or every chunk restarts progress near 0%. Progress
// must climb monotonically to 100% across chunks.
func TestRunTranscodeChunkProgressAccumulatesBaseTime(t *testing.T) {
	tmp := t.TempDir()
	fakeFFmpeg := fakeFFmpegWithProgress(t, tmp)
	fakeToolChunkAwareFFprobe(t, tmp)

	caps := EncoderCaps{HasLibx265: true, RecommendedEncoder: "libx265"}
	progress := make(chan Progress, 64)
	job := TranscodeJob{
		FFmpegPath:  fakeFFmpeg,
		InputFile:   filepath.Join(tmp, "in.mkv"),
		OutputFile:  filepath.Join(tmp, "out.mkv"),
		Codec:       "libx265",
		DurationSec: 600, // 2 chunks of 300s
		Spec: jobspec.JobSpec{
			Version:       1,
			Container:     "mkv",
			EncoderEngine: "cpu",
			QualityTarget: 22,
		},
		Caps:         &caps,
		ProgressChan: progress,
	}

	if err := runWithTimeout(t, job); err != nil {
		t.Fatalf("RunTranscode failed: %v", err)
	}

	var events []Progress
	for p := range progress {
		events = append(events, p)
	}
	if len(events) == 0 {
		t.Fatal("expected progress events from both chunk encodes")
	}

	// Monotonic non-decreasing progress: a chunk boundary must never reset
	// TimeSeconds, ProgressPct, or Frame (the reported sawtooth).
	for i := 1; i < len(events); i++ {
		if events[i].TimeSeconds < events[i-1].TimeSeconds {
			t.Fatalf("TimeSeconds reset at event %d: %.2f -> %.2f (progress sawtooth)", i, events[i-1].TimeSeconds, events[i].TimeSeconds)
		}
		if events[i].ProgressPct < events[i-1].ProgressPct {
			t.Fatalf("ProgressPct went backwards at event %d: %.2f -> %.2f (progress sawtooth)", i, events[i-1].ProgressPct, events[i].ProgressPct)
		}
		if events[i].Frame < events[i-1].Frame {
			t.Fatalf("Frame reset at event %d: %d -> %d (frame sawtooth)", i, events[i-1].Frame, events[i].Frame)
		}
	}

	// The final event must reflect the accumulated position: chunk 1 finished
	// its 300s chunk at baseTime 300 -> 600s of 600s total.
	last := events[len(events)-1]
	if last.TimeSeconds != 600 {
		t.Fatalf("final TimeSeconds = %.2f, want 600 (baseTime not accumulated across chunks)", last.TimeSeconds)
	}
	if last.ProgressPct != 100 {
		t.Fatalf("final ProgressPct = %.2f, want 100", last.ProgressPct)
	}
	if last.Frame != 15000 {
		t.Fatalf("final Frame = %d, want 15000 (frames not accumulated across chunks)", last.Frame)
	}

	// Mid-chunk-1 events must already carry baseTime and accumulated frames:
	// 150s into chunk 1 is 450s absolute (75%) and frame 11250.
	foundAccumulated := false
	for _, p := range events {
		if p.TimeSeconds == 450 {
			foundAccumulated = true
			if p.ProgressPct != 75 {
				t.Fatalf("accumulated event ProgressPct = %.2f, want 75", p.ProgressPct)
			}
			if p.Frame != 11250 {
				t.Fatalf("accumulated event Frame = %d, want 11250", p.Frame)
			}
		}
	}
	if !foundAccumulated {
		t.Fatalf("expected an event with accumulated TimeSeconds=450, got %d events with max TimeSeconds=%.2f", len(events), events[len(events)-1].TimeSeconds)
	}
}

func TestRunTranscodeEmitsPreEncodeProgress(t *testing.T) {
	tmp := t.TempDir()
	fakeFFmpeg := fakeFFmpegWithProgress(t, tmp)
	fakeToolChunkAwareFFprobe(t, tmp)

	caps := EncoderCaps{HasLibx264: true, RecommendedEncoder: "libx264"}
	progress := make(chan Progress, 64)
	job := TranscodeJob{
		FFmpegPath:  fakeFFmpeg,
		InputFile:   filepath.Join(tmp, "in.mkv"),
		OutputFile:  filepath.Join(tmp, "out.mkv"),
		Codec:       "libx264",
		SourceCodec: "h264",
		DurationSec: 600,
		TotalFrames: 15000,
		Spec: jobspec.JobSpec{
			Version:       1,
			Container:     "mkv",
			EncoderEngine: "cpu",
			QualityTarget: 22,
		},
		Caps:         &caps,
		ProgressChan: progress,
	}

	if err := runWithTimeout(t, job); err != nil {
		t.Fatalf("RunTranscode failed: %v", err)
	}

	var events []Progress
	for p := range progress {
		events = append(events, p)
	}

	var hasDemux, hasSplit bool
	for _, p := range events {
		if p.Phase == "demux" {
			hasDemux = true
			if p.TotalCount <= 0 {
				t.Errorf("demux progress TotalCount = %d, want > 0", p.TotalCount)
			}
		}
		if p.Phase == "split" {
			hasSplit = true
			if p.TotalCount != 2 {
				t.Errorf("split progress TotalCount = %d, want 2", p.TotalCount)
			}
		}
	}
	if !hasDemux {
		t.Error("expected demux phase progress events")
	}
	if !hasSplit {
		t.Error("expected split phase progress events")
	}
}

// TestRunTranscodeChunkProgressLatchesStatsAcrossHandoff verifies that between
// chunk completion and the subsequent chunk's first live progress packet,
// last-known aggregate stats (FPS, Speed, BitrateKbps) are held by the state latch
// and not cleared to zero.
func TestRunTranscodeChunkProgressLatchesStatsAcrossHandoff(t *testing.T) {
	tmp := t.TempDir()
	script := `#!/bin/sh
is_seg=0
is_demux=0
has_progress=0
is_chunk_1=0
out=""
for a in "$@"; do
  [ "$a" = "segment" ] && is_seg=1
  case "$a" in 0:s*) is_demux=1 ;; esac
  [ "$a" = "pipe:1" ] && has_progress=1
  case "$a" in *split-001*) is_chunk_1=1 ;; esac
  out="$a"
done
if [ "$is_demux" = "1" ]; then
  printf 'x' > "$out"
  exit 0
fi
if [ "$is_seg" = "1" ]; then
  i=0
  while [ "$i" -lt 2 ]; do
    idx=$(printf '%03d' $i)
    f=$(echo "$out" | sed "s/%03d/$idx/")
    touch "$f"
    i=$((i+1))
  done
  exit 0
fi
if [ "$has_progress" = "1" ]; then
  if [ "$is_chunk_1" = "1" ]; then
    # Chunk 1 starts with a typical ffmpeg 0-stat/N/A startup blip
    cat <<'PROGRESS_EOF'
frame=1
fps=0.0
out_time_us=0
speed=N/A
bitrate=N/A
progress=continue
frame=3750
fps=60.0
out_time_us=150000000
speed=2.0
bitrate=2400kbits/s
progress=continue
frame=7500
fps=60.0
out_time_us=300000000
speed=2.0
bitrate=2400kbits/s
progress=end
PROGRESS_EOF
  else
    # Chunk 0 normal progress
    cat <<'PROGRESS_EOF'
frame=3750
fps=62.5
out_time_us=150000000
speed=2.5
bitrate=2500kbits/s
progress=continue
frame=7500
fps=62.5
out_time_us=300000000
speed=2.5
bitrate=2500kbits/s
progress=end
PROGRESS_EOF
  fi
  exit 0
fi
exit 0
`
	fakeFFmpeg := fakeTool(t, tmp, "ffmpeg", script)
	fakeToolChunkAwareFFprobe(t, tmp)

	caps := EncoderCaps{HasLibx264: true, RecommendedEncoder: "libx264"}
	progress := make(chan Progress, 64)
	job := TranscodeJob{
		FFmpegPath:  fakeFFmpeg,
		InputFile:   filepath.Join(tmp, "in.mkv"),
		OutputFile:  filepath.Join(tmp, "out.mkv"),
		Codec:       "libx264",
		DurationSec: 600,
		TotalFrames: 15000,
		Spec: jobspec.JobSpec{
			Version:       1,
			Container:     "mkv",
			EncoderEngine: "cpu",
			QualityTarget: 22,
		},
		Caps:         &caps,
		ProgressChan: progress,
	}

	if err := runWithTimeout(t, job); err != nil {
		t.Fatalf("RunTranscode failed: %v", err)
	}

	var events []Progress
	for p := range progress {
		if p.Phase == "encode" {
			events = append(events, p)
		}
	}
	if len(events) < 3 {
		t.Fatalf("expected at least 3 encode progress events, got %d", len(events))
	}

	// Verify that none of the encode events dropped FPS, Speed, or Bitrate to 0
	for i, ev := range events {
		if ev.FPS <= 0 {
			t.Errorf("event %d (frame %d) had FPS <= 0 (%.2f), stats blipped", i, ev.Frame, ev.FPS)
		}
		if ev.Speed <= 0 {
			t.Errorf("event %d (frame %d) had Speed <= 0 (%.2f), stats blipped", i, ev.Frame, ev.Speed)
		}
		if ev.BitrateKbps <= 0 {
			t.Errorf("event %d (frame %d) had Bitrate <= 0 (%d), stats blipped", i, ev.Frame, ev.BitrateKbps)
		}
	}
}

// fakeFFmpegStartupGap simulates a transcode where out_time_us is stalled at 0
// during opening frames (e.g. 60s+ audio/subtitle lead gap or initial muxer buffering),
// then out_time_us resumes and advances to completion.
func fakeFFmpegStartupGap(t *testing.T, dir string) string {
	t.Helper()
	script := `#!/bin/sh
out=""
for a in "$@"; do
  out="$a"
done
case "$out" in
  *.mkv|*.mp4)
    touch "$out"
    ;;
esac
cat <<'PROGRESS_EOF'
frame=250
fps=50.0
out_time_us=0
speed=2.0
progress=continue
frame=500
fps=50.0
out_time_us=0
speed=2.0
progress=continue
frame=750
fps=50.0
out_time_us=0
speed=2.0
progress=continue
frame=1000
fps=50.0
out_time_us=0
speed=2.0
progress=continue
frame=1200
fps=50.0
out_time_us=48000000
speed=2.0
progress=continue
frame=2000
fps=50.0
out_time_us=80000000
speed=2.0
progress=continue
frame=10000
fps=50.0
out_time_us=400000000
speed=2.0
progress=end
PROGRESS_EOF
exit 0
`
	return fakeTool(t, dir, "ffmpeg-gap", script)
}

func TestRunTranscodeStartupGapFrameKickstartAndHandoff(t *testing.T) {
	tmp := t.TempDir()
	fakeFFmpeg := fakeFFmpegStartupGap(t, tmp)
	caps := EncoderCaps{HasLibx264: true, RecommendedEncoder: "libx264"}
	progress := make(chan Progress, 32)

	job := TranscodeJob{
		FFmpegPath:       fakeFFmpeg,
		InputFile:        filepath.Join(tmp, "in.mkv"),
		OutputFile:       filepath.Join(tmp, "out.mkv"),
		Codec:            "libx264",
		DurationSec:      400,   // 400 seconds total
		TotalFrames:      10000, // 10,000 frames total (25 fps)
		ExactFramesKnown: true,
		Spec: jobspec.JobSpec{
			Version:       1,
			Container:     "mkv",
			EncoderEngine: "cpu",
			QualityTarget: 22,
		},
		Caps:         &caps,
		ProgressChan: progress,
	}

	_, _, err := runSinglePass(t.Context(), job, progress, nil, 0, 400, 0, progressLatch{})
	if err != nil {
		t.Fatalf("runSinglePass failed: %v", err)
	}
	close(progress)

	var events []Progress
	for p := range progress {
		events = append(events, p)
	}
	if len(events) != 7 {
		t.Fatalf("expected 7 progress events, got %d", len(events))
	}

	// Block 1: frame=250, out_time=0 -> 250/10000 = 2.5%
	if events[0].ProgressPct != 2.5 {
		t.Errorf("event 0 ProgressPct = %.2f, want 2.5 (kickstart from frame count)", events[0].ProgressPct)
	}

	// Block 2: frame=500, out_time=0 -> 500/10000 = 5.0%
	if events[1].ProgressPct != 5.0 {
		t.Errorf("event 1 ProgressPct = %.2f, want 5.0", events[1].ProgressPct)
	}

	// Blocks 3 & 4: frame=750 & 1000, out_time=0 -> video frame progress continues steadily
	if events[2].ProgressPct != 7.5 {
		t.Errorf("event 2 ProgressPct = %.2f, want 7.5", events[2].ProgressPct)
	}
	if events[3].ProgressPct != 10.0 {
		t.Errorf("event 3 ProgressPct = %.2f, want 10.0", events[3].ProgressPct)
	}

	// Block 5: out_time_us=48s, frame=1200 (12.0%)
	if events[4].ProgressPct != 12.0 {
		t.Errorf("event 4 ProgressPct = %.2f, want 12.0", events[4].ProgressPct)
	}

	// Block 6: out_time_us=80s, frame=2000 (20.0%)
	if events[5].ProgressPct != 20.0 {
		t.Errorf("event 5 ProgressPct = %.2f, want 20.0", events[5].ProgressPct)
	}

	// Block 7: out_time_us=400s (400/400 = 100.0%)
	if events[6].ProgressPct != 100.0 {
		t.Errorf("event 6 ProgressPct = %.2f, want 100.0", events[6].ProgressPct)
	}

	// Monotonic check: progress must never decrease
	for i := 1; i < len(events); i++ {
		if events[i].ProgressPct < events[i-1].ProgressPct {
			t.Errorf("progress decreased at event %d: %.2f -> %.2f", i, events[i-1].ProgressPct, events[i].ProgressPct)
		}
	}
}

// fakeFFmpegStreamCopiedAudioLead simulates the Grinch Remux case where copied
// audio packets flood the muxer queue immediately (out_time_us jumps to 25% in
// the first 3 seconds, then stays stalled for 30s while video frames catch up).
func fakeFFmpegStreamCopiedAudioLead(t *testing.T, dir string) string {
	t.Helper()
	script := `#!/bin/sh
out=""
for a in "$@"; do
  out="$a"
done
case "$out" in
  *.mkv|*.mp4)
    touch "$out"
    ;;
esac
cat <<'PROGRESS_EOF'
frame=25
fps=50.0
out_time_us=100000000
speed=2.0
progress=continue
frame=50
fps=50.0
out_time_us=100000000
speed=2.0
progress=continue
frame=100
fps=50.0
out_time_us=100000000
speed=2.0
progress=continue
frame=500
fps=50.0
out_time_us=100000000
speed=2.0
progress=continue
frame=2500
fps=50.0
out_time_us=100000000
speed=2.0
progress=continue
frame=5000
fps=50.0
out_time_us=200000000
speed=2.0
progress=continue
frame=10000
fps=50.0
out_time_us=400000000
speed=2.0
progress=end
PROGRESS_EOF
exit 0
`
	return fakeTool(t, dir, "ffmpeg-audiolead", script)
}

func TestRunTranscodeStreamCopiedAudioDoesNotStallProgress(t *testing.T) {
	tmp := t.TempDir()
	fakeFFmpeg := fakeFFmpegStreamCopiedAudioLead(t, tmp)
	caps := EncoderCaps{HasLibx264: true, RecommendedEncoder: "libx264"}
	progress := make(chan Progress, 32)

	job := TranscodeJob{
		FFmpegPath:       fakeFFmpeg,
		InputFile:        filepath.Join(tmp, "in.mkv"),
		OutputFile:       filepath.Join(tmp, "out.mkv"),
		Codec:            "libx264",
		DurationSec:      400,   // 400 seconds total
		TotalFrames:      10000, // 10,000 frames total (25 fps)
		ExactFramesKnown: true,
		Spec: jobspec.JobSpec{
			Version:       1,
			Container:     "mkv",
			EncoderEngine: "cpu",
			QualityTarget: 22,
		},
		Caps:         &caps,
		ProgressChan: progress,
	}

	_, _, err := runSinglePass(t.Context(), job, progress, nil, 0, 400, 0, progressLatch{})
	if err != nil {
		t.Fatalf("runSinglePass failed: %v", err)
	}
	close(progress)

	var events []Progress
	for p := range progress {
		events = append(events, p)
	}
	if len(events) != 7 {
		t.Fatalf("expected 7 progress events, got %d", len(events))
	}

	// Although out_time_us jumped to 100s (25%) immediately, video frame progress
	// must pace the progress bar:
	// Event 0: frame=25 / 10000 = 0.25% (NOT 25%!)
	if events[0].ProgressPct != 0.25 {
		t.Errorf("event 0 ProgressPct = %.2f, want 0.25 (video paced)", events[0].ProgressPct)
	}
	// Event 1: frame=50 / 10000 = 0.50% (NOT stalled at 25%!)
	if events[1].ProgressPct != 0.50 {
		t.Errorf("event 1 ProgressPct = %.2f, want 0.50", events[1].ProgressPct)
	}
	// Event 2: frame=100 / 10000 = 1.00%
	if events[2].ProgressPct != 1.00 {
		t.Errorf("event 2 ProgressPct = %.2f, want 1.00", events[2].ProgressPct)
	}
	// Event 3: frame=500 / 10000 = 5.00%
	if events[3].ProgressPct != 5.00 {
		t.Errorf("event 3 ProgressPct = %.2f, want 5.00", events[3].ProgressPct)
	}
	// Event 4: frame=2500 / 10000 = 25.00% (video catches up to audio)
	if events[4].ProgressPct != 25.00 {
		t.Errorf("event 4 ProgressPct = %.2f, want 25.00", events[4].ProgressPct)
	}
	// Event 5: frame=5000 / 10000 = 50.00%
	if events[5].ProgressPct != 50.00 {
		t.Errorf("event 5 ProgressPct = %.2f, want 50.00", events[5].ProgressPct)
	}
	// Event 6: frame=10000 / 10000 = 100.00%
	if events[6].ProgressPct != 100.00 {
		t.Errorf("event 6 ProgressPct = %.2f, want 100.00", events[6].ProgressPct)
	}
}

func TestRunTranscodeSafetyFallbackDisabled(t *testing.T) {
	tmp := t.TempDir()
	fakeFFmpeg := fakeFFmpegStartupGap(t, tmp)
	caps := EncoderCaps{HasLibx264: true, RecommendedEncoder: "libx264"}

	tests := []struct {
		name        string
		totalFrames int
		customFlags string
	}{
		{"ZeroTotalFrames", 0, ""},
		{"FrameRateFlag_r", 10000, "-r 24"},
		{"FrameRateFlag_fps", 10000, "-fps_mode cfr"},
		{"FrameRateFlag_vsync", 10000, "-vsync 0"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			progress := make(chan Progress, 32)
			job := TranscodeJob{
				FFmpegPath:       fakeFFmpeg,
				InputFile:        filepath.Join(tmp, "in.mkv"),
				OutputFile:       filepath.Join(tmp, "out.mkv"),
				Codec:            "libx264",
				DurationSec:      400,
				TotalFrames:      tc.totalFrames,
				ExactFramesKnown: false,
				Spec: jobspec.JobSpec{
					Version:       1,
					Container:     "mkv",
					EncoderEngine: "cpu",
					QualityTarget: 22,
					CustomFlags:   tc.customFlags,
				},
				Caps:         &caps,
				ProgressChan: progress,
			}

			_, _, err := runSinglePass(t.Context(), job, progress, nil, 0, 400, 0, progressLatch{})
			if err != nil {
				t.Fatalf("runSinglePass failed: %v", err)
			}
			close(progress)

			var events []Progress
			for p := range progress {
				events = append(events, p)
			}
			if len(events) != 7 {
				t.Fatalf("expected 7 progress events, got %d", len(events))
			}

			// For the first 4 events where out_time_us is 0, frame kickstart must be disabled
			// and ProgressPct must remain 0.0
			for i := 0; i < 4; i++ {
				if events[i].ProgressPct != 0.0 {
					t.Errorf("event %d ProgressPct = %.2f, want 0.0 (kickstart should be disabled)", i, events[i].ProgressPct)
				}
			}

			// When out_time_us arrives at event 4 (48s / 400s = 12.0%), time progress activates
			if events[4].ProgressPct != 12.0 {
				t.Errorf("event 4 ProgressPct = %.2f, want 12.0", events[4].ProgressPct)
			}
		})
	}
}

func TestRunTranscodeDerivedFramesPacesProgressWithoutStreamHeaderNbFrames(t *testing.T) {
	tmp := t.TempDir()
	fakeFFmpeg := fakeFFmpegStreamCopiedAudioLead(t, tmp)
	caps := EncoderCaps{HasLibx264: true, RecommendedEncoder: "libx264"}
	progress := make(chan Progress, 32)

	// Even when ExactFramesKnown is false (e.g. Matroska Remux where total frames was
	// derived from duration * fps), frame completion must pace progress to prevent
	// stream-copied audio buffering from jumping ahead and stalling.
	job := TranscodeJob{
		FFmpegPath:       fakeFFmpeg,
		InputFile:        filepath.Join(tmp, "in.mkv"),
		OutputFile:       filepath.Join(tmp, "out.mkv"),
		Codec:            "libx264",
		DurationSec:      400,
		TotalFrames:      10000,
		ExactFramesKnown: false, // NOT known in stream headers!
		Spec: jobspec.JobSpec{
			Version:       1,
			Container:     "mkv",
			EncoderEngine: "cpu",
			QualityTarget: 22,
		},
		Caps:         &caps,
		ProgressChan: progress,
	}

	_, _, err := runSinglePass(t.Context(), job, progress, nil, 0, 400, 0, progressLatch{})
	if err != nil {
		t.Fatalf("runSinglePass failed: %v", err)
	}
	close(progress)

	var events []Progress
	for p := range progress {
		events = append(events, p)
	}
	if len(events) != 7 {
		t.Fatalf("expected 7 progress events, got %d", len(events))
	}

	// Must advance smoothly with video frame progress, not jump to 25% or freeze
	if events[0].ProgressPct != 0.25 {
		t.Errorf("event 0 ProgressPct = %.2f, want 0.25 (video frame paced)", events[0].ProgressPct)
	}
	if events[1].ProgressPct != 0.50 {
		t.Errorf("event 1 ProgressPct = %.2f, want 0.50", events[1].ProgressPct)
	}
	if events[2].ProgressPct != 1.00 {
		t.Errorf("event 2 ProgressPct = %.2f, want 1.00", events[2].ProgressPct)
	}
	if events[3].ProgressPct != 5.00 {
		t.Errorf("event 3 ProgressPct = %.2f, want 5.00", events[3].ProgressPct)
	}
	if events[4].ProgressPct != 25.00 {
		t.Errorf("event 4 ProgressPct = %.2f, want 25.00", events[4].ProgressPct)
	}
	if events[5].ProgressPct != 50.00 {
		t.Errorf("event 5 ProgressPct = %.2f, want 50.00", events[5].ProgressPct)
	}
	if events[6].ProgressPct != 100.00 {
		t.Errorf("event 6 ProgressPct = %.2f, want 100.00", events[6].ProgressPct)
	}
}

func TestHasFrameRateAlteringFilters(t *testing.T) {
	cases := []struct {
		flags string
		want  bool
	}{
		{"", false},
		{"-bf 4 -rc-lookahead 30", false},
		{"keyint=60:bframes=4", false},
		{"-r 24", true},
		{"-fps_mode cfr", true},
		{"-vsync 0", true},
		{"-framerate 30", true},
		{"fps=30", true},
		{"mpdecimate", true},
		{"decimate", true},
		{"telecine", true},
		{"fieldmatch", true},
		{"minterpolate", true},
	}
	for _, tc := range cases {
		if got := hasFrameRateAlteringFilters(tc.flags); got != tc.want {
			t.Errorf("hasFrameRateAlteringFilters(%q) = %v, want %v", tc.flags, got, tc.want)
		}
	}
}

func TestProgressOutTimeMsParsing(t *testing.T) {
	tmp := t.TempDir()
	script := `#!/bin/sh
out=""
for a in "$@"; do
  out="$a"
done
case "$out" in
  *.mkv|*.mp4)
    touch "$out"
    ;;
esac
cat <<'PROGRESS_EOF'
frame=100
fps=25.0
out_time_ms=5000000
speed=1.5
progress=continue
frame=200
fps=25.0
out_time_ms=10000000
speed=1.5
progress=end
PROGRESS_EOF
exit 0
`
	fakeFFmpeg := fakeTool(t, tmp, "ffmpeg", script)
	caps := EncoderCaps{HasLibx264: true, RecommendedEncoder: "libx264"}
	progressChan := make(chan Progress, 16)

	job := TranscodeJob{
		FFmpegPath:   fakeFFmpeg,
		InputFile:    filepath.Join(tmp, "in.mkv"),
		OutputFile:   filepath.Join(tmp, "out.mkv"),
		DurationSec:  10000.0,
		Codec:        "libx264",
		Caps:         &caps,
		ProgressChan: progressChan,
		Spec: jobspec.JobSpec{
			Version:       1,
			Container:     "mkv",
			EncoderEngine: "cpu",
			QualityTarget: 22,
		},
	}

	_, latch, err := runSinglePass(t.Context(), job, progressChan, nil, 0, 10000.0, 0, progressLatch{})
	if err != nil {
		t.Fatalf("runSinglePass: %v", err)
	}

	close(progressChan)
	var events []Progress
	for p := range progressChan {
		events = append(events, p)
	}

	if len(events) == 0 {
		t.Fatalf("expected progress events, got none")
	}

	// 5000000 ms should be 5000 seconds
	firstTime := events[0].TimeSeconds
	if firstTime != 5000.0 {
		t.Errorf("expected first TimeSeconds=5000.0, got %f", firstTime)
	}
	if latch.ProgressPct != 100.0 {
		t.Errorf("expected final progress pct == 100.0, got %f", latch.ProgressPct)
	}
}



