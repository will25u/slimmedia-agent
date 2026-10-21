package ffmpeg

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/will25u/slimmedia-agent/internal/jobspec"
)

// TestRunTranscodeIsPausedHaltsBetweenChunks: when IsPaused flips true while
// a chunk is encoding, the runner must fire OnPause(true), must not start the
// next chunk until IsPaused returns false, and must fire OnPause(false) on
// resume. This is the wire behavior the daemon's PAUSE_JOB handling relies on.
func TestRunTranscodeIsPausedHaltsBetweenChunks(t *testing.T) {
	tmp := t.TempDir()
	argsFile := filepath.Join(tmp, "args.bin")
	// Each fake invocation records its argv plus a start timestamp, then sleeps
	// long enough that a pause request can land mid-chunk.
	// When called as a segment muxer (-f segment), it creates the split files.
	fakeScript := "#!/bin/sh\n" +
		"{ for a in \"$@\"; do printf '%s\\000' \"$a\"; done; printf '\\000'; } >> \"" + argsFile + "\"\n" +
		"date +%s%N >> \"" + filepath.Join(tmp, "starts.log") + "\"\n" +
		"is_seg=0\nis_demux=0\npat=\"\"\nfor a in \"$@\"; do\n  [ \"$a\" = \"segment\" ] && is_seg=1\n  case \"$a\" in 0:s*) is_demux=1 ;; esac\n  pat=\"$a\"\ndone\n" +
		"if [ \"$is_demux\" = \"1\" ]; then\n" +
		"  printf 'x' > \"$pat\"\n" +
		"  exit 0\n" +
		"fi\n" +
		"if [ \"$is_seg\" = \"1\" ]; then\n" +
		"  for i in 0 1; do\n" +
		"    idx=$(printf '%03d' $i)\n" +
		"    out=$(echo \"$pat\" | sed \"s/%03d/$idx/\")\n" +
		"    touch \"$out\"\n" +
		"  done\n" +
		"  exit 0\n" +
		"fi\n" +
		"sleep 0.5\nexit 0\n"
	fakeFFmpeg := fakeTool(t, tmp, "ffmpeg", fakeScript)
	fakeToolChunkAwareFFprobe(t, tmp)

	var paused atomic.Bool
	var mu sync.Mutex
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
		DurationSec:  600, // 2 chunks of 300s
		PollInterval: 10 * time.Millisecond,
		IsPaused:     func() bool { return paused.Load() },
		OnPause: func(p bool) {
			mu.Lock()
			pauseEvents = append(pauseEvents, p)
			mu.Unlock()
		},
	}

	// Request pause shortly after chunk 0 starts, hold it for ~1.2s, then resume.
	go func() {
		time.Sleep(200 * time.Millisecond)
		paused.Store(true)
	}()
	go func() {
		time.Sleep(1400 * time.Millisecond)
		paused.Store(false)
	}()

	if err := runWithTimeout(t, job); err != nil {
		t.Fatalf("RunTranscode failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(pauseEvents) != 2 || pauseEvents[0] != true || pauseEvents[1] != false {
		t.Fatalf("expected pause events [true false], got %v", pauseEvents)
	}

	// Measure the wall-clock gap between consecutive fake-ffmpeg invocations:
	// the gap spanning the paused window must be at least ~1s (0.5s chunk
	// work + 1.2s paused window); if pause were ignored the gap would be ~0.5s.
	data, err := os.ReadFile(filepath.Join(tmp, "starts.log"))
	if err != nil {
		t.Fatalf("fake ffmpeg recorded no invocations: %v", err)
	}
	var starts []int64
	for _, line := range splitLines(string(data)) {
		var ts int64
		if _, err := fmt.Sscanf(line, "%d", &ts); err == nil {
			starts = append(starts, ts)
		}
	}
	if len(starts) < 5 {
		t.Fatalf("expected at least 5 invocations (demux, split, chunk 0, chunk 1, concat), got %d", len(starts))
	}
	// starts[0]=demux pass (fast), starts[1]=split pass (fast), starts[2]=chunk 0,
	// starts[3]=chunk 1, starts[4]=concat.
	// The gap between chunk 0 start and chunk 1 start must be ≥ ~1.2s (pause window).
	gap := time.Duration(starts[3]-starts[2]) * time.Nanosecond
	if gap < 900*time.Millisecond {
		t.Fatalf("chunk 1 started only %v after chunk 0 — pause was not honored (expected >= ~1.2s gap)", gap)
	}
}

// TestRunTranscodeLegacyJobHonorsIsPaused: a job with none of the advanced
// tuning fields (a pre-tuning / no-preset payload) still carries IsPaused,
// and pause requests must not be silently ignored for it.
func TestRunTranscodeLegacyJobHonorsIsPaused(t *testing.T) {
	tmp := t.TempDir()
	argsFile := filepath.Join(tmp, "args.bin")
	fakeScript := "#!/bin/sh\n" +
		"{ for a in \"$@\"; do printf '%s\\000' \"$a\"; done; printf '\\000'; } >> \"" + argsFile + "\"\n" +
		"is_seg=0\nis_demux=0\npat=\"\"\nfor a in \"$@\"; do\n  [ \"$a\" = \"segment\" ] && is_seg=1\n  case \"$a\" in 0:s*) is_demux=1 ;; esac\n  pat=\"$a\"\ndone\n" +
		"if [ \"$is_demux\" = \"1\" ]; then\n" +
		"  printf 'x' > \"$pat\"\n" +
		"  exit 0\n" +
		"fi\n" +
		"if [ \"$is_seg\" = \"1\" ]; then\n" +
		"  for i in 0 1; do\n" +
		"    idx=$(printf '%03d' $i)\n" +
		"    out=$(echo \"$pat\" | sed \"s/%03d/$idx/\")\n" +
		"    touch \"$out\"\n" +
		"  done\n" +
		"  exit 0\n" +
		"fi\n" +
		"sleep 0.3\nexit 0\n"
	fakeFFmpeg := fakeTool(t, tmp, "ffmpeg", fakeScript)
	// Chunk encodes are ffprobe-verified as pure video even on this legacy-shaped job.
	fakeToolChunkAwareFFprobe(t, tmp)

	var paused atomic.Bool
	var mu sync.Mutex
	var pauseEvents []bool

	job := TranscodeJob{
		FFmpegPath:   fakeFFmpeg,
		InputFile:    filepath.Join(tmp, "in.mkv"),
		OutputFile:   filepath.Join(tmp, "out.mkv"),
		Codec:        "hevc",
		DurationSec:  600,
		PollInterval: 10 * time.Millisecond,
		IsPaused:     func() bool { return paused.Load() },
		OnPause: func(p bool) {
			mu.Lock()
			pauseEvents = append(pauseEvents, p)
			mu.Unlock()
		},
	}

	go func() {
		time.Sleep(100 * time.Millisecond)
		paused.Store(true)
	}()
	go func() {
		time.Sleep(900 * time.Millisecond)
		paused.Store(false)
	}()

	if err := runWithTimeout(t, job); err != nil {
		t.Fatalf("RunTranscode failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(pauseEvents) != 2 || pauseEvents[0] != true || pauseEvents[1] != false {
		t.Fatalf("legacy job: expected pause events [true false], got %v", pauseEvents)
	}
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}
