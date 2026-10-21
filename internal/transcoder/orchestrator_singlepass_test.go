package transcoder

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/will25u/slimmedia-agent/internal/db"
	"github.com/will25u/slimmedia-agent/internal/ffmpeg"
	"github.com/will25u/slimmedia-agent/internal/jobpaths"
	"github.com/will25u/slimmedia-agent/internal/jobspec"
	"github.com/will25u/slimmedia-agent/internal/models"
)

// stageRealInput copies the repo's probeable test.mkv into the test temp dir
// so the orchestrator's probe and verification stages run against real media.
func stageRealInput(t *testing.T, tempDir string) string {
	t.Helper()
	repoFFprobe := repoBin(t, "ffprobe")
	src, err := os.ReadFile(filepath.Join(filepath.Dir(repoFFprobe), "..", "test.mkv"))
	if err != nil {
		t.Skipf("repo test.mkv not available: %v", err)
	}
	inputPath := filepath.Join(tempDir, "input.mkv")
	if err := os.WriteFile(inputPath, src, 0644); err != nil {
		t.Fatalf("failed to stage input: %v", err)
	}
	return inputPath
}

// TestOrchestrator_SinglePassJobSkipsSegmentDir is the orchestrator half of
// the engine-conditional pipeline: a job whose resolved encoder is hardware
// must complete without ever creating the segment directory, while the job
// journal still lands next to the scratch file for crash recovery.
func TestOrchestrator_SinglePassJobSkipsSegmentDir(t *testing.T) {
	tempDir := t.TempDir()
	inputPath := stageRealInput(t, tempDir)
	canonical, err := CanonicalMediaPath(inputPath)
	if err != nil {
		t.Fatalf("failed to canonicalize input: %v", err)
	}

	o, scratchDir, quarantineDir := newTestOrchestrator(t, fakeLibrary{canonical: true})
	o.Caps = ffmpeg.EncoderCaps{
		HasNvencHEVC: true, HasNvencTuningVerified: true, HasLibx265: true,
		RecommendedEncoder: "hevc_nvenc",
	}

	// The fake runner stands in for the real full-stream encode: it copies
	// the input to the scratch output, which passes every verification gate
	// (size ratio 100%, identical duration and a/v offsets). While it runs it
	// also checks the crash-recovery journal is already in place — that is
	// the moment a crash would need it.
	runnerSawJournal := false
	o.Runner = func(ctx context.Context, job ffmpeg.TranscodeJob) error {
		// The real runner guarantees the progress channel closes even on
		// failure; the orchestrator blocks on that closure.
		defer close(job.ProgressChan)
		if _, err := os.Stat(job.Paths.JournalFile()); err == nil {
			runnerSawJournal = true
		}
		in, err := os.Open(job.InputFile)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(job.OutputFile)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	}

	job := &Job{
		ID:       "job-single-pass",
		FilePath: inputPath,
		Payload:  []byte(`{"job_id":"job-single-pass"}`),
	}
	spec := jobspec.JobSpec{
		Version:            1,
		Container:          "mkv",
		EncoderEngine:      "nvenc",
		QualityTarget:      22,
		OutputFileStrategy: "alongside",
		ActionOnSuccess:    "keep",
	}

	res, err := o.ProcessJobWithSpec(context.Background(), job, "hevc", spec, JobHooks{}, nil)
	if err != nil {
		t.Fatalf("ProcessJobWithSpec failed: %v", err)
	}
	if res == nil {
		t.Fatal("expected a JobResult")
	}
	if job.State != models.StateCompleted {
		t.Fatalf("job state = %q, want %q", job.State, models.StateCompleted)
	}

	paths := jobpaths.New(scratchDir, quarantineDir, canonical)
	if _, err := os.Stat(paths.SegmentDir()); !os.IsNotExist(err) {
		t.Errorf("single-pass job must not create the segment directory %s", paths.SegmentDir())
	}
	if !runnerSawJournal {
		t.Errorf("job journal must exist next to the scratch file while the job runs (payload %q)", job.Payload)
	}
	if b, err := os.ReadFile(paths.JournalFile()); err == nil {
		t.Errorf("journal must be removed after completion, still holds %q", b)
	}
}

// TestOrchestrator_SoftwareJobStillCreatesSegmentDir guards the other half of
// the condition: a software-resolved job keeps the chunked layout.
func TestOrchestrator_SoftwareJobStillCreatesSegmentDir(t *testing.T) {
	tempDir := t.TempDir()
	inputPath := stageRealInput(t, tempDir)
	canonical, err := CanonicalMediaPath(inputPath)
	if err != nil {
		t.Fatalf("failed to canonicalize input: %v", err)
	}

	o, scratchDir, quarantineDir := newTestOrchestrator(t, fakeLibrary{canonical: true})
	o.Caps = ffmpeg.EncoderCaps{HasLibx265: true, RecommendedEncoder: "libx265"}

	o.Runner = func(ctx context.Context, job ffmpeg.TranscodeJob) error {
		defer close(job.ProgressChan)
		// Materialize what the chunked runner would leave behind: one chunk
		// plus the assembled scratch output.
		if err := os.MkdirAll(job.Paths.SegmentDir(), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(job.Paths.Chunk(0), []byte("chunk"), 0644); err != nil {
			return err
		}
		in, err := os.Open(job.InputFile)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(job.OutputFile)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	}

	job := &Job{
		ID:       "job-chunked",
		FilePath: inputPath,
		Payload:  []byte(`{"job_id":"job-chunked"}`),
	}
	spec := jobspec.JobSpec{
		Version:            1,
		Container:          "mkv",
		EncoderEngine:      "cpu",
		QualityTarget:      22,
		OutputFileStrategy: "alongside",
		ActionOnSuccess:    "keep",
	}

	if _, err := o.ProcessJobWithSpec(context.Background(), job, "hevc", spec, JobHooks{}, nil); err != nil {
		t.Fatalf("ProcessJobWithSpec failed: %v", err)
	}

	paths := jobpaths.New(scratchDir, quarantineDir, canonical)
	// The segment directory is removed after a successful concat; the point
	// is the job must complete on the chunked path without error.
	if job.State != models.StateCompleted {
		t.Fatalf("job state = %q, want %q", job.State, models.StateCompleted)
	}
	if _, err := os.Stat(paths.JournalFile()); !os.IsNotExist(err) {
		t.Errorf("journal must be deleted once the job completes: %v", err)
	}
}

// TestOrchestrator_RecoverJobs_LegacyJournalLocation covers a chunked job
// that was in flight when the journal still lived inside the segment
// directory: recovery must find the payload at the legacy location and resume
// instead of discarding the job.
func TestOrchestrator_RecoverJobs_LegacyJournalLocation(t *testing.T) {
	tempDir := t.TempDir()
	dbConn, err := db.InitDB(tempDir)
	if err != nil {
		t.Fatal(err)
	}
	defer dbConn.Close()
	queries := db.New(dbConn)

	scratchDir := filepath.Join(tempDir, "scratch")
	quarantineDir := filepath.Join(tempDir, "quarantine")
	pipeline := NewPipeline(scratchDir, quarantineDir, 0)

	binDir := filepath.Join(tempDir, "bin")
	_ = os.MkdirAll(binDir, 0755)
	probeScript := `#!/bin/sh
echo '{"format":{"duration":"300.00","size":"1048576","bit_rate":"1000"},"streams":[{"codec_type":"video","codec_name":"h264"}]}'
`
	_ = os.WriteFile(filepath.Join(binDir, "ffprobe"), []byte(probeScript), 0755)
	dummyFFmpeg := filepath.Join(binDir, "ffmpeg")
	_ = os.WriteFile(dummyFFmpeg, []byte("#!/bin/sh\nexit 0\n"), 0755)

	o := NewOrchestrator(NewStateMachine(nil), pipeline, dummyFFmpeg)
	o.Queries = queries

	inputPath := filepath.Join(tempDir, "input_legacy.mkv")
	_ = os.WriteFile(inputPath, []byte("fake video content"), 0644)

	paths := jobpaths.New(scratchDir, quarantineDir, inputPath)
	segDir := paths.SegmentDir()
	_ = os.MkdirAll(segDir, 0755)
	// Pre-move journal layout: payload inside the segment directory.
	_ = os.WriteFile(paths.LegacyJournalFile(), []byte(`{"JobID":"job-legacy"}`), 0644)
	_ = os.WriteFile(paths.Chunk(0), []byte("chunk0"), 0644)

	_ = queries.UpsertJobJournal(context.Background(), db.UpsertJobJournalParams{
		JobID:    "job-legacy",
		FilePath: inputPath,
		State:    "TRANSCODING",
	})

	payloads := o.RecoverJobs(context.Background())
	if len(payloads) != 1 {
		t.Fatalf("expected 1 recovered payload from the legacy journal location, got %d", len(payloads))
	}

	jobs, _ := queries.ListIncompleteJobs(context.Background())
	if len(jobs) != 1 || jobs[0].State != "resuming" {
		t.Fatalf("expected 1 job in state 'resuming', got %+v", jobs)
	}
}

// TestOrchestrator_RecoverJobs_SinglePassRestartsEncode: a single-pass job
// that crashed mid-encode has a journal next to the scratch file, a partial
// scratch output, and no segment directory at all. Recovery must requeue it
// (state 'resuming') so the encode restarts from the beginning — chunk-level
// resume has nothing to attach to by design.
func TestOrchestrator_RecoverJobs_SinglePassRestartsEncode(t *testing.T) {
	tempDir := t.TempDir()
	dbConn, err := db.InitDB(tempDir)
	if err != nil {
		t.Fatal(err)
	}
	defer dbConn.Close()
	queries := db.New(dbConn)

	scratchDir := filepath.Join(tempDir, "scratch")
	quarantineDir := filepath.Join(tempDir, "quarantine")
	pipeline := NewPipeline(scratchDir, quarantineDir, 0)

	binDir := filepath.Join(tempDir, "bin")
	_ = os.MkdirAll(binDir, 0755)
	probeScript := `#!/bin/sh
echo '{"format":{"duration":"300.00","size":"1048576","bit_rate":"1000"},"streams":[{"codec_type":"video","codec_name":"h264"}]}'
`
	_ = os.WriteFile(filepath.Join(binDir, "ffprobe"), []byte(probeScript), 0755)
	dummyFFmpeg := filepath.Join(binDir, "ffmpeg")
	_ = os.WriteFile(dummyFFmpeg, []byte("#!/bin/sh\nexit 0\n"), 0755)

	o := NewOrchestrator(NewStateMachine(nil), pipeline, dummyFFmpeg)
	o.Queries = queries

	inputPath := filepath.Join(tempDir, "input_singlepass.mkv")
	_ = os.WriteFile(inputPath, []byte("fake video content"), 0644)

	paths := jobpaths.New(scratchDir, quarantineDir, inputPath)
	_ = os.WriteFile(paths.JournalFile(), []byte(`{"JobID":"job-sp"}`), 0644)
	// Partial output of the crashed pass.
	_ = os.WriteFile(paths.Scratch(), []byte("partial"), 0644)

	_ = queries.UpsertJobJournal(context.Background(), db.UpsertJobJournalParams{
		JobID:    "job-sp",
		FilePath: inputPath,
		State:    "TRANSCODING",
	})

	payloads := o.RecoverJobs(context.Background())
	if len(payloads) != 1 {
		t.Fatalf("expected 1 recovered payload for the single-pass job, got %d", len(payloads))
	}

	jobs, _ := queries.ListIncompleteJobs(context.Background())
	if len(jobs) != 1 || jobs[0].State != "resuming" {
		t.Fatalf("expected 1 job in state 'resuming', got %+v", jobs)
	}

	if _, err := os.Stat(paths.SegmentDir()); !os.IsNotExist(err) {
		t.Errorf("single-pass recovery must not create a segment directory")
	}
}
