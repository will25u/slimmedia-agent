package transcoder

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/will25u/slimmedia-agent/internal/jobpaths"
	"github.com/will25u/slimmedia-agent/internal/models"
)

// TestFailJobRemovesDemuxContainer is the cleanup half of the upfront demux
// pass: when a job fails, the demux scratch container must be removed along
// with the scratch output. The segment dir itself stays (the journal it
// holds is what crash recovery resumes from).
func TestFailJobRemovesDemuxContainer(t *testing.T) {
	o, scratchDir, quarantineDir := newTestOrchestrator(t, fakeLibrary{})

	paths := jobpaths.New(scratchDir, quarantineDir, filepath.Join(t.TempDir(), "movie.mkv"))
	if err := os.MkdirAll(paths.SegmentDir(), 0755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := os.WriteFile(paths.DemuxContainer(), []byte("x"), 0644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := os.WriteFile(paths.Scratch(), []byte("x"), 0644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	job := &Job{ID: "job-cleanup", State: models.StateTranscoding}
	o.failJob(job, "boom", paths)

	if job.State != models.StateFailed {
		t.Errorf("job state = %q, want %q", job.State, models.StateFailed)
	}
	if _, err := os.Stat(paths.DemuxContainer()); !os.IsNotExist(err) {
		t.Errorf("demux container must be removed on job failure")
	}
	if _, err := os.Stat(paths.Scratch()); !os.IsNotExist(err) {
		t.Errorf("scratch output must be removed on job failure")
	}
	if _, err := os.Stat(paths.SegmentDir()); err != nil {
		t.Errorf("segment dir (journal home) must survive a plain failure: %v", err)
	}
}
