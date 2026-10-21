package transcoder

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/will25u/slimmedia-agent/internal/ffmpeg"
	"github.com/will25u/slimmedia-agent/internal/jobpaths"
	"github.com/will25u/slimmedia-agent/internal/jobspec"
	"github.com/will25u/slimmedia-agent/internal/models"
)

// TestOrchestrator_FailingFFmpegStartReturns is the agent-visible regression
// for the runner deadlock: when the ffmpeg binary cannot start, the
// orchestrator must return (job FAILED) instead of blocking forever on the
// progress channel — the agent must be able to go back to IDLE and shut down.
func TestOrchestrator_FailingFFmpegStartReturns(t *testing.T) {
	repoFFprobe := repoBin(t, "ffprobe")

	tempDir := t.TempDir()

	// A fake ffmpeg "binary" that is not executable, so cmd.Start fails.
	// The sibling ffprobe is the real one so the probe stage succeeds and
	// the failure happens exactly at ffmpeg start.
	badBinDir := filepath.Join(tempDir, "badbin")
	if err := os.MkdirAll(badBinDir, 0755); err != nil {
		t.Fatalf("failed to create bad bin dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(badBinDir, "ffmpeg"), []byte("not an executable"), 0644); err != nil {
		t.Fatalf("failed to write decoy ffmpeg: %v", err)
	}
	ffprobeLink := filepath.Join(badBinDir, "ffprobe")
	if err := os.Symlink(repoFFprobe, ffprobeLink); err != nil {
		t.Fatalf("failed to link ffprobe: %v", err)
	}

	// A real, probeable media file the library allowlist accepts.
	src, err := os.ReadFile(filepath.Join(filepath.Dir(repoFFprobe), "..", "test.mkv"))
	if err != nil {
		t.Skipf("repo test.mkv not available: %v", err)
	}
	inputPath := filepath.Join(tempDir, "input.mkv")
	if err := os.WriteFile(inputPath, src, 0644); err != nil {
		t.Fatalf("failed to stage input: %v", err)
	}
	canonical, err := CanonicalMediaPath(inputPath)
	if err != nil {
		t.Fatalf("failed to canonicalize input: %v", err)
	}

	o := NewOrchestrator(
		NewStateMachine(nil),
		NewPipeline(filepath.Join(tempDir, "scratch"), filepath.Join(tempDir, "quarantine"), 0),
		filepath.Join(badBinDir, "ffmpeg"),
	)
	o.Library = fakeLibrary{canonical: true}

	job := &Job{ID: "job-deadlock", FilePath: inputPath}

	type result struct {
		res *JobResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := o.ProcessJobWithSpec(context.Background(), job, "libx264", jobspec.JobSpec{CRF: 30, PresetSpeed: "ultrafast"}, JobHooks{}, nil)
		done <- result{res: res, err: err}
	}()

	select {
	case r := <-done:
		if r.err == nil {
			t.Fatal("ProcessJobWithSpec succeeded with a non-executable ffmpeg; want failure")
		}
		if job.State != models.StateFailed {
			t.Fatalf("job state = %q, want %q (agent must return to a settled state)", job.State, models.StateFailed)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("ProcessJobWithSpec blocked for 15s on a failed ffmpeg start — orchestrator deadlock")
	}

	// The runner goroutine must be gone: a second ProcessJob on a fresh
	// orchestrator with the same setup must complete just as fast (no leak
	// accumulation from the first attempt).
	o2 := NewOrchestrator(
		NewStateMachine(nil),
		NewPipeline(filepath.Join(tempDir, "scratch2"), filepath.Join(tempDir, "quarantine2"), 0),
		filepath.Join(badBinDir, "ffmpeg"),
	)
	o2.Library = fakeLibrary{canonical: true}
	job2 := &Job{ID: "job-deadlock-2", FilePath: inputPath}
	done2 := make(chan error, 1)
	go func() {
		_, err := o2.ProcessJobWithSpec(context.Background(), job2, "libx264", jobspec.JobSpec{}, JobHooks{}, nil)
		done2 <- err
	}()
	select {
	case err := <-done2:
		if err == nil {
			t.Fatal("second run unexpectedly succeeded")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("second ProcessJobWithSpec blocked — goroutine leak from first failure")
	}
}

func TestOrchestrator_CancellationAwaitsRunnerTerminationBeforeCleanup(t *testing.T) {
	repoFFprobe := repoBin(t, "ffprobe")
	tempDir := t.TempDir()

	src, err := os.ReadFile(filepath.Join(filepath.Dir(repoFFprobe), "..", "test.mkv"))
	if err != nil {
		t.Skipf("repo test.mkv not available: %v", err)
	}
	inputPath := filepath.Join(tempDir, "input.mkv")
	if err := os.WriteFile(inputPath, src, 0644); err != nil {
		t.Fatalf("failed to stage input: %v", err)
	}

	scratchDir := filepath.Join(tempDir, "scratch")
	quarantineDir := filepath.Join(tempDir, "quarantine")
	_ = os.MkdirAll(scratchDir, 0755)
	_ = os.MkdirAll(quarantineDir, 0755)

	o := NewOrchestrator(
		NewStateMachine(nil),
		NewPipeline(scratchDir, quarantineDir, 0),
		filepath.Join(filepath.Dir(repoFFprobe), "ffmpeg"),
	)
	canonical, err := CanonicalMediaPath(inputPath)
	if err != nil {
		t.Fatalf("failed to canonicalize input: %v", err)
	}
	o.Library = fakeLibrary{canonical: true}

	started := make(chan struct{})
	runnerDone := make(chan struct{})
	var cleanupPremature atomic.Bool

	o.Runner = func(ctx context.Context, job ffmpeg.TranscodeJob) error {
		defer close(runnerDone)
		chunkFile := job.Paths.Chunk(0)
		_ = os.WriteFile(chunkFile, []byte("chunk-data"), 0644)
		_ = os.WriteFile(job.OutputFile, []byte("scratch-data"), 0644)
		close(started)

		<-ctx.Done()
		// Simulate child process termination time
		time.Sleep(50 * time.Millisecond)

		// Before the runner returns, the temporary files MUST still exist;
		// they must not be deleted while the runner process is active.
		if _, err := os.Stat(chunkFile); os.IsNotExist(err) {
			cleanupPremature.Store(true)
		}
		return ctx.Err()
	}

	job := &Job{ID: "job-cancel-test", FilePath: inputPath}
	ctx, cancel := context.WithCancel(context.Background())

	errChan := make(chan error, 1)
	go func() {
		_, err := o.ProcessJobWithSpec(ctx, job, "libx264", jobspec.JobSpec{}, JobHooks{}, nil)
		errChan <- err
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("runner failed to start")
	}

	cancel()

	select {
	case err := <-errChan:
		if err == nil {
			t.Fatal("expected cancellation error, got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ProcessJobWithSpec did not return after cancellation")
	}

	if cleanupPremature.Load() {
		t.Fatal("temporary chunk files were deleted before runner process terminated")
	}

	if job.State != models.StateStopped {
		t.Fatalf("job state = %q, want %q", job.State, models.StateStopped)
	}

	// Verify temporary files are now cleaned up
	paths := jobpaths.New(scratchDir, quarantineDir, inputPath)
	if _, err := os.Stat(paths.Chunk(0)); !os.IsNotExist(err) {
		t.Errorf("chunk file was not cleaned up after cancellation")
	}
	if _, err := os.Stat(paths.Scratch()); !os.IsNotExist(err) {
		t.Errorf("scratch file was not cleaned up after cancellation")
	}
}

