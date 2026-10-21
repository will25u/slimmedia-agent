package transcoder

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/will25u/slimmedia-agent/internal/db"
	"github.com/will25u/slimmedia-agent/internal/ffmpeg"
	"github.com/will25u/slimmedia-agent/internal/jobpaths"
	"github.com/will25u/slimmedia-agent/internal/jobspec"
	"github.com/will25u/slimmedia-agent/internal/models"
)

// TestOrchestrator_RecoverJobs_RestoreFailed_PreservesQuarantine verifies that when
// the copy from quarantine back to the original path fails, the quarantine file is NOT
// deleted and the journal is NOT marked queued (so the job won't be re-dispatched with
// data loss risk).
func TestOrchestrator_RecoverJobs_RestoreFailed_PreservesQuarantine(t *testing.T) {
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

	o := NewOrchestrator(NewStateMachine(nil), pipeline, "")
	o.Queries = queries

	// Original is missing; quarantine exists — this is a mid-replace crash scenario.
	inputPath := filepath.Join(tempDir, "does-not-exist-dir", "media.mkv")
	paths := jobpaths.New(scratchDir, quarantineDir, inputPath)
	quarPath := paths.Quarantine()
	_ = os.WriteFile(quarPath, []byte("quarantined data"), 0644)

	_ = queries.UpsertJobJournal(context.Background(), db.UpsertJobJournalParams{
		JobID:    "job-restore-fail",
		FilePath: inputPath,
		State:    "REPLACING",
	})

	o.RecoverJobs(context.Background())

	// Quarantine file must still exist — we cannot confirm the restore succeeded.
	if _, err := os.Stat(quarPath); os.IsNotExist(err) {
		t.Fatalf("quarantine file was deleted despite restore failure: data loss risk")
	}

	// The journal entry must be set to perm_failed to surface the unrestorable state.
	jobs, _ := queries.ListIncompleteJobs(context.Background())
	if len(jobs) != 1 {
		t.Fatalf("expected 1 journal entry, got %d", len(jobs))
	}
	if jobs[0].State != string(models.StatePermFailed) {
		t.Fatalf("expected journal state %s after restore failure, got %s", models.StatePermFailed, jobs[0].State)
	}
}

// TestOrchestrator_RecoverJobs_SuccessfulRestore_RemovesQuarantine verifies the happy-path:
// when the copy from quarantine succeeds AND sizes match, the quarantine is removed and
// the restored file contains the original bytes.
func TestOrchestrator_RecoverJobs_SuccessfulRestore_RemovesQuarantine(t *testing.T) {
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

	o := NewOrchestrator(NewStateMachine(nil), pipeline, "")
	o.Queries = queries

	// inputPath does NOT exist; quarantine exists — simulate a crash mid-replace.
	inputPath := filepath.Join(tempDir, "restore-ok.mkv")
	paths := jobpaths.New(scratchDir, quarantineDir, inputPath)
	quarPath := paths.Quarantine()

	quarContent := []byte("original media bytes")
	_ = os.WriteFile(quarPath, quarContent, 0644)

	_ = queries.UpsertJobJournal(context.Background(), db.UpsertJobJournalParams{
		JobID:    "job-restore-ok",
		FilePath: inputPath,
		State:    "REPLACING",
	})

	o.RecoverJobs(context.Background())

	// Restored file must exist with the correct content.
	got, err := os.ReadFile(inputPath)
	if err != nil {
		t.Fatalf("restored file not found: %v", err)
	}
	if string(got) != string(quarContent) {
		t.Fatalf("restored content %q != quarantine content %q", got, quarContent)
	}

	// Quarantine must be removed after a verified successful restore.
	if _, err := os.Stat(quarPath); !os.IsNotExist(err) {
		t.Fatalf("quarantine was not removed after successful restore")
	}
}

func TestOrchestrator_RecoverJobs(t *testing.T) {
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

	o := NewOrchestrator(NewStateMachine(nil), pipeline, "")
	o.Queries = queries

	// Setup a job in replacing state with missing original but existing quarantine
	inputPath := filepath.Join(tempDir, "media.mkv")
	quarPath := jobpaths.New(scratchDir, quarantineDir, inputPath).Quarantine()
	_ = os.WriteFile(quarPath, []byte("quarantined data"), 0644)
	_ = queries.UpsertJobJournal(context.Background(), db.UpsertJobJournalParams{
		JobID:    "job-1",
		FilePath: inputPath,
		State:    "REPLACING",
	})

	o.RecoverJobs(context.Background())

	// original file should be restored
	content, err := os.ReadFile(inputPath)
	if err != nil {
		t.Fatalf("expected original to be restored, got error: %v", err)
	}
	if string(content) != "quarantined data" {
		t.Fatalf("expected 'quarantined data', got %s", string(content))
	}

	// journal should be updated to queued state for resumption
	jobs, _ := queries.ListIncompleteJobs(context.Background())
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job in journal, got %d", len(jobs))
	}
	if jobs[0].State != "queued" {
		t.Fatalf("expected job state to be 'queued', got %s", jobs[0].State)
	}
}

func TestOrchestrator_RecoverJobs_CorruptedSegment(t *testing.T) {
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

	// Create dummy ffprobe/ffmpeg that fail
	dummyBin := filepath.Join(tempDir, "ffprobe")
	os.WriteFile(dummyBin, []byte("#!/bin/sh\nexit 1\n"), 0755)

	o := NewOrchestrator(NewStateMachine(nil), pipeline, dummyBin)
	o.Queries = queries

	inputPath := filepath.Join(tempDir, "media2.mkv")

	paths := jobpaths.New(scratchDir, quarantineDir, inputPath)
	segDir := paths.SegmentDir()
	os.MkdirAll(segDir, 0755)
	os.WriteFile(paths.JournalFile(), []byte(`{"JobID":"job-bad"}`), 0644)
	os.WriteFile(paths.Chunk(0), []byte("corrupt data"), 0644)

	_ = queries.UpsertJobJournal(context.Background(), db.UpsertJobJournalParams{
		JobID:    "job-bad",
		FilePath: inputPath,
		State:    "TRANSCODING",
	})

	o.RecoverJobs(context.Background())

	jobs, _ := queries.ListIncompleteJobs(context.Background())
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job in journal, got %d", len(jobs))
	}
	if jobs[0].State != "queued" {
		t.Fatalf("expected job state to be 'queued', got %s", jobs[0].State)
	}

	if _, err := os.Stat(segDir); !os.IsNotExist(err) {
		t.Fatalf("expected segDir to be deleted, but it exists")
	}
}

func TestOrchestrator_RecoverJobs_TruncatedSegment(t *testing.T) {
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

	// Create dummy ffprobe that reports 750s for original, 300s for chunk-000, 45s for chunk-001 (truncated)
	binDir := filepath.Join(tempDir, "bin")
	_ = os.MkdirAll(binDir, 0755)
	dummyProbe := filepath.Join(binDir, "ffprobe")
	probeScript := `#!/bin/sh
if echo "$@" | grep -q "chunk-001"; then
	echo '{"format":{"duration":"45.00","size":"1048576","bit_rate":"1000"},"streams":[{"codec_type":"video","codec_name":"h264"}]}'
elif echo "$@" | grep -q "chunk-000"; then
	echo '{"format":{"duration":"300.00","size":"1048576","bit_rate":"1000"},"streams":[{"codec_type":"video","codec_name":"h264"}]}'
else
	echo '{"format":{"duration":"750.00","size":"1048576","bit_rate":"1000"},"streams":[{"codec_type":"video","codec_name":"h264"}]}'
fi
`
	_ = os.WriteFile(dummyProbe, []byte(probeScript), 0755)

	dummyFFmpeg := filepath.Join(binDir, "ffmpeg")
	_ = os.WriteFile(dummyFFmpeg, []byte("#!/bin/sh\nexit 0\n"), 0755)

	o := NewOrchestrator(NewStateMachine(nil), pipeline, dummyFFmpeg)
	o.Queries = queries

	inputPath := filepath.Join(tempDir, "input_trunc.mkv")
	_ = os.WriteFile(inputPath, []byte("fake video content"), 0644)

	paths := jobpaths.New(scratchDir, quarantineDir, inputPath)
	segDir := paths.SegmentDir()
	_ = os.MkdirAll(segDir, 0755)
	_ = os.WriteFile(paths.JournalFile(), []byte(`{"JobID":"job-trunc"}`), 0644)
	_ = os.WriteFile(paths.Chunk(0), []byte("chunk0"), 0644)
	_ = os.WriteFile(paths.Chunk(1), []byte("chunk1-truncated"), 0644)

	_ = queries.UpsertJobJournal(context.Background(), db.UpsertJobJournalParams{
		JobID:    "job-trunc",
		FilePath: inputPath,
		State:    "TRANSCODING",
	})

	o.RecoverJobs(context.Background())

	jobs, _ := queries.ListIncompleteJobs(context.Background())
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job in journal, got %d", len(jobs))
	}
	if jobs[0].State != "queued" {
		t.Fatalf("expected job state to be 'queued' due to truncated chunk, got %s", jobs[0].State)
	}

	if _, err := os.Stat(segDir); !os.IsNotExist(err) {
		t.Fatalf("expected segDir to be deleted due to truncated chunk, but it exists")
	}
}

func TestOrchestrator_RecoverJobs_ValidFinalChunk(t *testing.T) {
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

	// Original is 345s total: chunk-000 is 300s, chunk-001 (final) is 45s
	binDir := filepath.Join(tempDir, "bin")
	_ = os.MkdirAll(binDir, 0755)
	dummyProbe := filepath.Join(binDir, "ffprobe")
	probeScript := `#!/bin/sh
if echo "$@" | grep -q "chunk-001"; then
	echo '{"format":{"duration":"45.00","size":"1048576","bit_rate":"1000"},"streams":[{"codec_type":"video","codec_name":"h264"}]}'
elif echo "$@" | grep -q "chunk-000"; then
	echo '{"format":{"duration":"300.00","size":"1048576","bit_rate":"1000"},"streams":[{"codec_type":"video","codec_name":"h264"}]}'
else
	echo '{"format":{"duration":"345.00","size":"1048576","bit_rate":"1000"},"streams":[{"codec_type":"video","codec_name":"h264"}]}'
fi
`
	_ = os.WriteFile(dummyProbe, []byte(probeScript), 0755)

	dummyFFmpeg := filepath.Join(binDir, "ffmpeg")
	_ = os.WriteFile(dummyFFmpeg, []byte("#!/bin/sh\nexit 0\n"), 0755)

	o := NewOrchestrator(NewStateMachine(nil), pipeline, dummyFFmpeg)
	o.Queries = queries

	inputPath := filepath.Join(tempDir, "input_valid.mkv")
	_ = os.WriteFile(inputPath, []byte("fake video content"), 0644)

	paths := jobpaths.New(scratchDir, quarantineDir, inputPath)
	segDir := paths.SegmentDir()
	_ = os.MkdirAll(segDir, 0755)
	_ = os.WriteFile(paths.JournalFile(), []byte(`{"JobID":"job-valid"}`), 0644)
	_ = os.WriteFile(paths.Chunk(0), []byte("chunk0"), 0644)
	_ = os.WriteFile(paths.Chunk(1), []byte("chunk1-final"), 0644)

	_ = queries.UpsertJobJournal(context.Background(), db.UpsertJobJournalParams{
		JobID:    "job-valid",
		FilePath: inputPath,
		State:    "TRANSCODING",
	})

	payloads := o.RecoverJobs(context.Background())

	if len(payloads) != 1 {
		t.Fatalf("expected 1 recovered payload, got %d", len(payloads))
	}

	jobs, _ := queries.ListIncompleteJobs(context.Background())
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job in journal, got %d", len(jobs))
	}
	if jobs[0].State != "resuming" {
		t.Fatalf("expected job state to be 'resuming', got %s", jobs[0].State)
	}

	if _, err := os.Stat(segDir); os.IsNotExist(err) {
		t.Fatalf("expected segDir to be preserved, but it was deleted")
	}
}

func TestOrchestrator_VerificationFailure_PurgesSegmentsAndClearsJournal(t *testing.T) {
	tempDir := t.TempDir()
	dbConn, err := db.InitDB(tempDir)
	if err != nil {
		t.Fatal(err)
	}
	defer dbConn.Close()
	queries := db.New(dbConn)

	binDir := filepath.Join(tempDir, "bin")
	_ = os.MkdirAll(binDir, 0755)

	// Fake ffprobe where scratch duration is 20s while input is 10s (violates 2s delta)
	fakeProbe := filepath.Join(binDir, "ffprobe")
	probeScript := `#!/bin/sh
if echo "$@" | grep -q "scratch"; then
	echo '{"format":{"duration":"20.00","size":"1048576","bit_rate":"1000"},"streams":[{"codec_type":"video","codec_name":"h264"}]}'
else
	echo '{"format":{"duration":"10.00","size":"1048576","bit_rate":"1000"},"streams":[{"codec_type":"video","codec_name":"h264"}]}'
fi
`
	_ = os.WriteFile(fakeProbe, []byte(probeScript), 0755)
	fakeFFmpeg := filepath.Join(binDir, "ffmpeg")
	_ = os.WriteFile(fakeFFmpeg, []byte("#!/bin/sh\nexit 0\n"), 0755)

	scratchDir := filepath.Join(tempDir, "scratch")
	quarantineDir := filepath.Join(tempDir, "quarantine")
	pipeline := NewPipeline(scratchDir, quarantineDir, 0)

	sm := NewStateMachine(func(job *Job, oldState, newState models.JobState) {
		if newState == models.StateCompleted || newState == models.StatePermFailed || newState == models.StateSkipped || newState == models.StateStopped {
			_ = queries.DeleteJobJournal(context.Background(), job.ID)
		} else {
			_ = queries.UpsertJobJournal(context.Background(), db.UpsertJobJournalParams{
				JobID:    job.ID,
				FilePath: job.FilePath,
				State:    string(newState),
			})
		}
	})

	o := NewOrchestrator(sm, pipeline, fakeFFmpeg)
	o.Queries = queries

	inputPath := filepath.Join(tempDir, "input_fail.mkv")
	_ = os.WriteFile(inputPath, []byte("fake video content 1234567890"), 0644)
	canonical, _ := CanonicalMediaPath(inputPath)

	paths := jobpaths.New(scratchDir, quarantineDir, canonical)
	scratchPath := paths.Scratch()
	segDir := paths.SegmentDir()

	o.Runner = func(ctx context.Context, job ffmpeg.TranscodeJob) error {
		_ = os.WriteFile(job.Paths.Chunk(0), []byte("chunkdata"), 0644)
		_ = os.WriteFile(job.OutputFile, []byte("transcoded-scratch-content"), 0644)
		close(job.ProgressChan)
		return nil
	}

	job := &Job{ID: "job-verify-fail", FilePath: canonical, Payload: []byte(`{"id":"job-verify-fail"}`)}
	_ = queries.UpsertJobJournal(context.Background(), db.UpsertJobJournalParams{
		JobID:    job.ID,
		FilePath: canonical,
		State:    "queued",
	})

	_, err = o.ProcessJobWithSpec(context.Background(), job, "hevc", jobspec.JobSpec{}, JobHooks{}, nil)
	if err == nil {
		t.Fatalf("expected verification failure")
	}

	if job.State != models.StatePermFailed {
		t.Fatalf("expected job state to be %s, got %s", models.StatePermFailed, job.State)
	}

	// Verify scratch output file and entire temporary .segments directory are purged
	if _, err := os.Stat(scratchPath); !os.IsNotExist(err) {
		t.Fatalf("expected scratch output file to be deleted, but it exists")
	}
	if _, err := os.Stat(segDir); !os.IsNotExist(err) {
		t.Fatalf("expected .segments directory to be deleted on verification failure, but it exists")
	}

	// Verify job is removed from job_journal to prevent infinite restart loop
	if _, err := queries.GetJobJournal(context.Background(), job.ID); err != sql.ErrNoRows {
		t.Fatalf("expected job to be deleted from job_journal, got err: %v", err)
	}
}

func TestOrchestrator_RecoverJobs_ClearsBrokenJournalWithoutPayload(t *testing.T) {
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

	o := NewOrchestrator(NewStateMachine(nil), pipeline, "")
	o.Queries = queries

	// Setup a job with no segments/job.json and no quarantine restore (e.g. stranded in journal)
	brokenPath := filepath.Join(tempDir, "broken.mkv")
	_ = os.WriteFile(brokenPath, []byte("existing original file"), 0644)
	_ = queries.UpsertJobJournal(context.Background(), db.UpsertJobJournalParams{
		JobID:    "job-broken-loop",
		FilePath: brokenPath,
		State:    "verifying",
	})

	payloads := o.RecoverJobs(context.Background())
	if len(payloads) != 0 {
		t.Fatalf("expected 0 payloads for unrecoverable broken job, got %d", len(payloads))
	}

	jobs, err := queries.ListIncompleteJobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("expected broken job to be purged from journal, but found: %v", jobs)
	}
}

func TestOrchestrator_Cancellation_RaceConditionNoReinsertion(t *testing.T) {
	tempDir := t.TempDir()
	dbConn, err := db.InitDB(tempDir)
	if err != nil {
		t.Fatal(err)
	}
	defer dbConn.Close()
	queries := db.New(dbConn)

	binDir := filepath.Join(tempDir, "bin")
	_ = os.MkdirAll(binDir, 0755)
	fakeProbe := filepath.Join(binDir, "ffprobe")
	_ = os.WriteFile(fakeProbe, []byte("#!/bin/sh\necho '{\"format\":{\"duration\":\"10.00\",\"size\":\"1048576\",\"bit_rate\":\"1000\"},\"streams\":[{\"codec_type\":\"video\",\"codec_name\":\"h264\"}]}'\n"), 0755)
	fakeFFmpeg := filepath.Join(binDir, "ffmpeg")
	_ = os.WriteFile(fakeFFmpeg, []byte("#!/bin/sh\nexit 0\n"), 0755)

	scratchDir := filepath.Join(tempDir, "scratch")
	quarantineDir := filepath.Join(tempDir, "quarantine")
	pipeline := NewPipeline(scratchDir, quarantineDir, 0)

	sm := NewStateMachine(func(job *Job, oldState, newState models.JobState) {
		if newState == models.StateCompleted || newState == models.StatePermFailed || newState == models.StateSkipped || newState == models.StateStopped {
			_ = queries.DeleteJobJournal(context.Background(), job.ID)
		} else {
			if _, err := queries.GetJobJournal(context.Background(), job.ID); err == sql.ErrNoRows && oldState != models.StateQueued {
				return
			}
			_ = queries.UpsertJobJournal(context.Background(), db.UpsertJobJournalParams{
				JobID:    job.ID,
				FilePath: job.FilePath,
				State:    string(newState),
			})
		}
	})

	o := NewOrchestrator(sm, pipeline, fakeFFmpeg)
	o.Queries = queries

	inputPath := filepath.Join(tempDir, "input_cancel.mkv")
	_ = os.WriteFile(inputPath, []byte("fake video content"), 0644)
	canonical, _ := CanonicalMediaPath(inputPath)

	ctx, cancel := context.WithCancel(context.Background())

	started := make(chan struct{})
	o.Runner = func(runCtx context.Context, job ffmpeg.TranscodeJob) error {
		close(started)
		<-runCtx.Done()
		return runCtx.Err()
	}

	job := &Job{ID: "job-race-cancel", FilePath: canonical, Payload: []byte(`{"id":"job-race-cancel"}`)}
	_ = queries.UpsertJobJournal(context.Background(), db.UpsertJobJournalParams{
		JobID:    job.ID,
		FilePath: canonical,
		State:    "queued",
	})

	go func() {
		<-started
		// Simulate CANCEL_JOB event handler:
		// 1. Cancel the context
		cancel()
		// 2. Delete from journal
		_ = queries.DeleteJobJournal(context.Background(), job.ID)
	}()

	_, err = o.ProcessJobWithSpec(ctx, job, "hevc", jobspec.JobSpec{}, JobHooks{}, nil)
	if err == nil {
		t.Fatalf("expected context cancellation error")
	}

	if job.State != models.StateStopped {
		t.Fatalf("expected job state to be %s, got %s", models.StateStopped, job.State)
	}

	// Verify the journal entry was NOT re-inserted by runner termination
	if _, err := queries.GetJobJournal(context.Background(), job.ID); err != sql.ErrNoRows {
		t.Fatalf("expected job to remain deleted from job_journal, but found journal entry")
	}
}

func TestWriteJournalAtomic(t *testing.T) {
	tempDir := t.TempDir()
	target := filepath.Join(tempDir, "job.json")
	payload := []byte(`{"JobID":"atomic-job","Payload":"test"}`)

	if err := writeJournalAtomic(target, payload); err != nil {
		t.Fatalf("writeJournalAtomic failed: %v", err)
	}

	// Verify temp file does not linger
	if _, err := os.Stat(target + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("expected temporary file %s to be removed", target+".tmp")
	}

	// Verify target content
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("failed to read target file: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("got content %q, want %q", string(got), string(payload))
	}
}

func TestRecoverJobs_DiskSpaceGate(t *testing.T) {
	tempDir := t.TempDir()
	dbConn, err := db.InitDB(tempDir)
	if err != nil {
		t.Fatal(err)
	}
	defer dbConn.Close()
	queries := db.New(dbConn)

	scratchDir := filepath.Join(tempDir, "scratch")
	quarantineDir := filepath.Join(tempDir, "quarantine")
	_ = os.MkdirAll(quarantineDir, 0755)
	// Write a file to quarantine and set budget to 1 byte to trigger budget exhaustion
	_ = os.WriteFile(filepath.Join(quarantineDir, "overflow.bin"), []byte("overflow-data"), 0644)
	pipeline := NewPipeline(scratchDir, quarantineDir, 1)

	binDir := filepath.Join(tempDir, "bin")
	_ = os.MkdirAll(binDir, 0755)
	dummyProbe := filepath.Join(binDir, "ffprobe")
	_ = os.WriteFile(dummyProbe, []byte("#!/bin/sh\nexit 0\n"), 0755)

	o := NewOrchestrator(NewStateMachine(nil), pipeline, dummyProbe)
	o.Queries = queries

	inputPath := filepath.Join(tempDir, "media.mkv")
	_ = os.WriteFile(inputPath, []byte("fake video content"), 0644)

	paths := jobpaths.New(scratchDir, quarantineDir, inputPath)
	_ = os.WriteFile(paths.JournalFile(), []byte(`{"JobID":"job-full-disk"}`), 0644)

	_ = queries.UpsertJobJournal(context.Background(), db.UpsertJobJournalParams{
		JobID:    "job-full-disk",
		FilePath: inputPath,
		State:    "TRANSCODING",
	})

	payloads := o.RecoverJobs(context.Background())
	if len(payloads) != 0 {
		t.Fatalf("expected 0 payloads recovered due to disk/budget gate, got %d", len(payloads))
	}

	jobs, err := queries.ListIncompleteJobs(context.Background())
	if err != nil || len(jobs) != 1 {
		t.Fatalf("expected 1 incomplete job, got %d (err: %v)", len(jobs), err)
	}
	if jobs[0].State != "queued" {
		t.Fatalf("expected job state to remain 'queued', got %q", jobs[0].State)
	}
}

func TestContinuousBudgetCheckAbortsTranscode(t *testing.T) {
	tempDir := t.TempDir()
	scratchDir := filepath.Join(tempDir, "scratch")
	quarantineDir := filepath.Join(tempDir, "quarantine")
	_ = os.MkdirAll(scratchDir, 0755)
	_ = os.MkdirAll(quarantineDir, 0755)

	pipeline := NewPipeline(scratchDir, quarantineDir, 1000)

	binDir := filepath.Join(tempDir, "bin")
	_ = os.MkdirAll(binDir, 0755)
	dummyProbe := filepath.Join(binDir, "ffprobe")
	probeScript := `#!/bin/sh
echo '{"format":{"duration":"10.00","size":"1048576","bit_rate":"1000"},"streams":[{"codec_type":"video","codec_name":"h264"}]}'
`
	_ = os.WriteFile(dummyProbe, []byte(probeScript), 0755)

	inputPath := filepath.Join(tempDir, "input.mkv")
	_ = os.WriteFile(inputPath, []byte("fake video content"), 0644)
	canonical, _ := CanonicalMediaPath(inputPath)

	o := NewOrchestrator(NewStateMachine(nil), pipeline, dummyProbe)
	o.Library = fakeLibrary{canonical: true}

	job := &Job{ID: "job-budget-abort", FilePath: canonical}

	o.Runner = func(ctx context.Context, transJob ffmpeg.TranscodeJob) error {
		defer close(transJob.ProgressChan)
		// Send initial progress
		transJob.ProgressChan <- ffmpeg.Progress{ProgressPct: 10.0}

		// Exceed quarantine budget while transcode is in flight
		_ = os.WriteFile(filepath.Join(quarantineDir, "huge.bin"), make([]byte, 2000), 0644)

		// Send next progress to trigger budget check
		transJob.ProgressChan <- ffmpeg.Progress{ProgressPct: 20.0}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
			return nil
		}
	}

	_, err := o.ProcessJobWithSpec(context.Background(), job, "hevc", jobspec.JobSpec{}, JobHooks{}, nil)
	if err == nil {
		t.Fatalf("expected error from budget exhaustion")
	}
	if job.State != models.StateFailed {
		t.Fatalf("expected job state to be %s, got %s", models.StateFailed, job.State)
	}
}

func TestFailJob_CleansUpDestination(t *testing.T) {
	tempDir := t.TempDir()
	scratchDir := filepath.Join(tempDir, "scratch")
	quarantineDir := filepath.Join(tempDir, "quarantine")
	_ = os.MkdirAll(scratchDir, 0755)
	_ = os.MkdirAll(quarantineDir, 0755)

	pipeline := NewPipeline(scratchDir, quarantineDir, 0)
	o := NewOrchestrator(NewStateMachine(nil), pipeline, "")

	inputPath := filepath.Join(tempDir, "action.mkv")
	_ = os.WriteFile(inputPath, []byte("original media"), 0644)

	paths := jobpaths.New(scratchDir, quarantineDir, inputPath)
	destPath, err := paths.DestinationPath("alongside", "")
	if err != nil {
		t.Fatalf("unexpected error getting destination path: %v", err)
	}

	// Simulate partial destination left by failed copy
	_ = os.WriteFile(destPath, []byte("partial output"), 0644)

	job := &Job{ID: "job-cleanup-dest", FilePath: inputPath}
	o.failJob(job, "simulated copy failure", paths)

	if _, err := os.Stat(destPath); !os.IsNotExist(err) {
		t.Fatalf("expected alongside destination %s to be cleaned up on failJob", destPath)
	}

	// Verify original media was NOT removed
	if _, err := os.Stat(inputPath); err != nil {
		t.Fatalf("original media file should be preserved: %v", err)
	}
}




