package transcoder

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/will25u/slimmedia-agent/internal/db"
	"github.com/will25u/slimmedia-agent/internal/ffmpeg"
	"github.com/will25u/slimmedia-agent/internal/jobspec"
	"github.com/will25u/slimmedia-agent/internal/models"
	_ "modernc.org/sqlite"
)

func TestOrchestrator_LoopPrevention(t *testing.T) {
	repoFFprobe := repoBin(t, "ffprobe")
	tempDir := t.TempDir()

	binDir := filepath.Join(tempDir, "bin")
	_ = os.MkdirAll(binDir, 0755)
	_ = os.WriteFile(filepath.Join(binDir, "ffmpeg"), []byte("echo fake ffmpeg"), 0755)
	_ = os.Symlink(repoFFprobe, filepath.Join(binDir, "ffprobe"))

	src, err := os.ReadFile(filepath.Join(filepath.Dir(repoFFprobe), "..", "test.mkv"))
	if err != nil {
		t.Skipf("repo test.mkv not available: %v", err)
	}
	inputPath := filepath.Join(tempDir, "input.mkv")
	_ = os.WriteFile(inputPath, src, 0644)
	canonical, _ := CanonicalMediaPath(inputPath)

	dbConn, err := db.InitDB(tempDir)
	if err != nil {
		t.Fatal(err)
	}
	defer dbConn.Close()
	queries := db.New(dbConn)

	o := NewOrchestrator(
		NewStateMachine(nil),
		NewPipeline(filepath.Join(tempDir, "scratch"), filepath.Join(tempDir, "quarantine"), 0),
		filepath.Join(binDir, "ffmpeg"),
	)
	o.Library = fakeLibrary{canonical: true}
	o.Queries = queries
	o.Runner = func(ctx context.Context, job ffmpeg.TranscodeJob) error {
		defer close(job.ProgressChan)
		return os.WriteFile(job.OutputFile, src, 0644)
	}

	job1 := &Job{ID: "job-1", FilePath: canonical}
	res, err := o.ProcessJobWithSpec(context.Background(), job1, "hevc", jobspec.JobSpec{}, JobHooks{}, nil)
	if err != nil {
		t.Fatalf("first run failed: %v", err)
	}
	if res.NewHash == "" {
		t.Fatalf("expected NewHash to be set")
	}

	job2 := &Job{ID: "job-2", FilePath: canonical}
	_, err = o.ProcessJobWithSpec(context.Background(), job2, "hevc", jobspec.JobSpec{}, JobHooks{}, nil)
	if err == nil {
		t.Fatalf("second run should have been skipped")
	}
	if job2.State != models.StateSkipped {
		t.Fatalf("expected job state to be skipped, got %s", job2.State)
	}
}
