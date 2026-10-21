package ffmpeg

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/will25u/slimmedia-agent/internal/jobspec"
)

// repoRoot resolves the repository root from this package's test working dir.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("failed to resolve repo root: %v", err)
	}
	return root
}

// runWithTimeout fails the test if RunTranscode does not return in time.
func runWithTimeout(t *testing.T, job TranscodeJob) error {
	t.Helper()
	type result struct {
		err error
	}
	done := make(chan result, 1)
	go func() {
		done <- result{err: RunTranscode(context.Background(), job)}
	}()
	select {
	case r := <-done:
		return r.err
	case <-time.After(15 * time.Second):
		t.Fatal("RunTranscode did not return within 15s — progress channel deadlock")
		return nil
	}
}

// assertChannelClosed fails unless ProgressChan is closed (ok=false) — the
// guarantee the orchestrator relies on to avoid blocking forever.
func assertChannelClosed(t *testing.T, ch <-chan Progress) {
	t.Helper()
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-time.After(5 * time.Second):
			t.Fatal("ProgressChan was not closed after RunTranscode returned — orchestrator would deadlock")
		}
	}
}

// Negative: a binary that cannot start (non-executable file) must not wedge
// the orchestrator — RunTranscode must return an error AND close the
// progress channel.
func TestRunTranscode_StartFailureClosesProgressChan(t *testing.T) {
	tmp := t.TempDir()

	// Non-executable decoy binary: exec fails at Start with permission denied.
	badBinary := filepath.Join(tmp, "ffmpeg")
	if err := os.WriteFile(badBinary, []byte("#!/bin/sh\necho nope\n"), 0644); err != nil {
		t.Fatalf("failed to write decoy binary: %v", err)
	}

	progress := make(chan Progress, 16)
	job := TranscodeJob{
		FFmpegPath:   badBinary,
		InputFile:    filepath.Join(tmp, "in.mkv"),
		OutputFile:   filepath.Join(tmp, "out.mkv"),
		Codec:        "libx264",
		DurationSec:  1,
		ProgressChan: progress,
	}

	err := runWithTimeout(t, job)
	if err == nil {
		t.Fatal("RunTranscode with a non-executable binary returned nil error")
	}
	assertChannelClosed(t, progress)
}

// Negative: a binary that starts but exits immediately (exit 1, no progress
// output) must still close the channel.
func TestRunTranscode_ImmediateExitClosesProgressChan(t *testing.T) {
	tmp := t.TempDir()
	progress := make(chan Progress, 16)
	job := TranscodeJob{
		FFmpegPath:   "/bin/false",
		InputFile:    filepath.Join(tmp, "in.mkv"),
		OutputFile:   filepath.Join(tmp, "out.mkv"),
		Codec:        "libx264",
		DurationSec:  1,
		ProgressChan: progress,
	}

	err := runWithTimeout(t, job)
	if err == nil {
		t.Fatal("RunTranscode with /bin/false returned nil error")
	}
	assertChannelClosed(t, progress)
}

// Positive: a real transcode streams progress and closes the channel cleanly,
// exactly as before the hardening.
func TestRunTranscode_StreamsProgressAndCloses(t *testing.T) {
	root := repoRoot(t)
	ffmpegPath := filepath.Join(root, "bin", "ffmpeg")
	if _, err := os.Stat(ffmpegPath); err != nil {
		t.Skipf("repo ffmpeg binary not available: %v", err)
	}
	src := filepath.Join(root, "test.mkv")
	if _, err := os.Stat(src); err != nil {
		t.Skipf("repo test.mkv not available: %v", err)
	}

	tmp := t.TempDir()
	progress := make(chan Progress, 64)
	job := TranscodeJob{
		FFmpegPath: ffmpegPath,
		InputFile:  src,
		OutputFile: filepath.Join(tmp, "out.mkv"),
		Codec:      "libx264",
		Spec: jobspec.JobSpec{
			CRF:         30,
			PresetSpeed: "ultrafast",
		},
		DurationSec:  1,
		ProgressChan: progress,
	}

	err := runWithTimeout(t, job)
	if err != nil {
		t.Fatalf("RunTranscode failed: %v", err)
	}
	if _, err := os.Stat(job.OutputFile); err != nil {
		t.Fatalf("expected output file %s: %v", job.OutputFile, err)
	}

	// Drain: at least one progress event, then the channel must be closed.
	events := 0
	for p := range progress {
		if p.Frame >= 0 {
			events++
		}
	}
	if events == 0 {
		t.Fatal("expected at least one progress event during a real transcode")
	}
}

func TestRunTranscode_CapturesStderrDiagnosticsOnFailure(t *testing.T) {
	tmp := t.TempDir()
	fakeFFmpeg := fakeTool(t, tmp, "ffmpeg", `#!/bin/sh
>&2 echo "Error initializing output stream 0:0: Unknown encoder 'bad_codec'"
exit 1
`)
	progress := make(chan Progress, 16)
	job := TranscodeJob{
		FFmpegPath:   fakeFFmpeg,
		InputFile:    filepath.Join(tmp, "in.mkv"),
		OutputFile:   filepath.Join(tmp, "out.mkv"),
		Codec:        "libx264",
		DurationSec:  1,
		ProgressChan: progress,
	}

	err := runWithTimeout(t, job)
	if err == nil {
		t.Fatal("expected transcode failure, got nil")
	}
	if !strings.Contains(err.Error(), "Unknown encoder 'bad_codec'") {
		t.Fatalf("expected error to contain stderr diagnostics, got: %v", err)
	}
	assertChannelClosed(t, progress)
}

