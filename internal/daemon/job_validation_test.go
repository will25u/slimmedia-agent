package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/will25u/slimmedia-agent/internal/db"
	"github.com/will25u/slimmedia-agent/internal/scanner"
	"github.com/will25u/slimmedia-agent/internal/transcoder"
	"github.com/will25u/slimmedia-agent/internal/protocol"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dataDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	sqlDB, err := db.InitDB(dataDir)
	if err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	return sqlDB
}

func TestValidateJobInputPath(t *testing.T) {
	sqlDB := setupTestDB(t)

	// Simulate a scanned library: two files under a media dir.
	mediaDir := t.TempDir()
	libraryFile := filepath.Join(mediaDir, "movie.mkv")
	if err := os.WriteFile(libraryFile, []byte("video"), 0644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	outsideFile := filepath.Join(t.TempDir(), "outside.mkv")
	if err := os.WriteFile(outsideFile, []byte("video"), 0644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	recordScannedFiles(sqlDB, []scanner.ScannedFile{{FilePath: libraryFile}}, mediaDir)

	t.Run("accepts scanned path", func(t *testing.T) {
		got, err := validateJobInputPath(sqlDB, libraryFile)
		if err != nil {
			t.Fatalf("expected acceptance, got: %v", err)
		}
		if got != filepath.Clean(libraryFile) {
			t.Fatalf("expected canonical path %q, got %q", libraryFile, got)
		}
	})

	t.Run("accepts scanned path via symlink alias", func(t *testing.T) {
		link := filepath.Join(mediaDir, "alias.mkv")
		if err := os.Symlink(libraryFile, link); err != nil {
			t.Fatalf("setup failed: %v", err)
		}
		got, err := validateJobInputPath(sqlDB, link)
		if err != nil {
			t.Fatalf("expected acceptance via canonical alias, got: %v", err)
		}
		if got != filepath.Clean(libraryFile) {
			t.Fatalf("expected canonical path %q, got %q", libraryFile, got)
		}
	})

	t.Run("rejects path outside scanned library", func(t *testing.T) {
		if _, err := validateJobInputPath(sqlDB, outsideFile); err == nil {
			t.Fatalf("expected rejection for unscanned path")
		}
	})

	t.Run("rejects protocol and flag paths", func(t *testing.T) {
		for _, p := range []string{
			"http://169.254.169.254/x",
			"concat:a|b",
			"-something",
		} {
			if _, err := validateJobInputPath(sqlDB, p); err == nil {
				t.Fatalf("expected rejection for %q", p)
			}
		}
	})

	t.Run("rejects nonexistent path", func(t *testing.T) {
		if _, err := validateJobInputPath(sqlDB, filepath.Join(mediaDir, "missing.mkv")); err == nil {
			t.Fatalf("expected rejection for nonexistent path")
		}
	})
}

func TestRecordScannedFilesCanonicalizes(t *testing.T) {
	sqlDB := setupTestDB(t)

	mediaDir := t.TempDir()
	realFile := filepath.Join(mediaDir, "movie.mkv")
	if err := os.WriteFile(realFile, []byte("video"), 0644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	link := filepath.Join(mediaDir, "alias.mkv")
	if err := os.Symlink(realFile, link); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	// The symlink alias records the canonical target, so a later job pushed
	// with either spelling is accepted.
	recordScannedFiles(sqlDB, []scanner.ScannedFile{{FilePath: link}}, mediaDir)

	lib := dbLibrarySet{db: sqlDB}
	ok, err := lib.Contains(filepath.Clean(realFile))
	if err != nil {
		t.Fatalf("Contains failed: %v", err)
	}
	if !ok {
		t.Fatalf("expected canonical target to be recorded")
	}
}

func TestRecordScannedFilesRejectsExternalSymlinks(t *testing.T) {
	sqlDB := setupTestDB(t)

	mediaDir := t.TempDir()
	outsideDir := t.TempDir()

	realFile := filepath.Join(outsideDir, "evil.mkv")
	if err := os.WriteFile(realFile, []byte("video"), 0644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	link := filepath.Join(mediaDir, "alias.mkv")
	if err := os.Symlink(realFile, link); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	// Try recording the in-tree link pointing outside
	recordScannedFiles(sqlDB, []scanner.ScannedFile{{FilePath: link}}, mediaDir)

	lib := dbLibrarySet{db: sqlDB}
	ok, err := lib.Contains(filepath.Clean(realFile))
	if err != nil {
		t.Fatalf("Contains failed: %v", err)
	}
	if ok {
		t.Fatalf("expected canonical target pointing outside media directory to be rejected")
	}
}

func TestRecordScannedFilesAcceptsDotDotPrefixedNames(t *testing.T) {
	sqlDB := setupTestDB(t)

	mediaDir := t.TempDir()
	// A file whose name merely starts with ".." is not parent traversal.
	dotFile := filepath.Join(mediaDir, "..foo.mkv")
	if err := os.WriteFile(dotFile, []byte("video"), 0644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	parentFile := filepath.Join(filepath.Dir(mediaDir), "parent-escape.mkv")
	if err := os.WriteFile(parentFile, []byte("video"), 0644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	t.Cleanup(func() { os.Remove(parentFile) })

	recordScannedFiles(sqlDB, []scanner.ScannedFile{
		{FilePath: dotFile},
		{FilePath: parentFile},
	}, mediaDir)

	lib := dbLibrarySet{db: sqlDB}
	ok, err := lib.Contains(filepath.Clean(dotFile))
	if err != nil {
		t.Fatalf("Contains failed: %v", err)
	}
	if !ok {
		t.Fatalf("expected '..foo.mkv' to be recorded")
	}
	ok, err = lib.Contains(filepath.Clean(parentFile))
	if err != nil {
		t.Fatalf("Contains failed: %v", err)
	}
	if ok {
		t.Fatalf("expected real '..' traversal to be rejected")
	}
}

func TestDbLibrarySetMatchesOrchestratorInterface(t *testing.T) {
	// Compile-time guard that the adapter satisfies the orchestrator's
	// confinement interface.
	var _ transcoder.ScannedLibrary = dbLibrarySet{}
}

func TestValidateJobFile_StatelessValidation(t *testing.T) {
	tempDir := t.TempDir()
	libraryDir := filepath.Join(tempDir, "library")
	if err := os.MkdirAll(libraryDir, 0755); err != nil {
		t.Fatalf("mkdir library failed: %v", err)
	}

	validFile := filepath.Join(libraryDir, "movie.mkv")
	if err := os.WriteFile(validFile, []byte("fake-video"), 0644); err != nil {
		t.Fatalf("write file failed: %v", err)
	}

	t.Run("valid existing file via local_path and relative_path", func(t *testing.T) {
		got, err := ValidateJobFile(libraryDir, "movie.mkv", "")
		if err != nil {
			t.Fatalf("expected success, got error: %v", err)
		}
		if got != filepath.Clean(validFile) {
			t.Errorf("got %q, want %q", got, validFile)
		}
	})

	t.Run("missing file returns ErrFileNotFound", func(t *testing.T) {
		_, err := ValidateJobFile(libraryDir, "missing.mkv", "")
		if err == nil {
			t.Fatalf("expected error, got nil")
		}
		if err != ErrFileNotFound {
			t.Errorf("expected ErrFileNotFound, got %v", err)
		}
	})

	t.Run("unmounted library returns ErrLibraryNotMounted", func(t *testing.T) {
		unmountedDir := filepath.Join(tempDir, "unmounted_share")
		_, err := ValidateJobFile(unmountedDir, "movie.mkv", "")
		if err == nil {
			t.Fatalf("expected error, got nil")
		}
		if err != ErrLibraryNotMounted {
			t.Errorf("expected ErrLibraryNotMounted, got %v", err)
		}
	})

	t.Run("directory path returns ErrFileNotFound", func(t *testing.T) {
		subDir := filepath.Join(libraryDir, "subdir")
		if err := os.MkdirAll(subDir, 0755); err != nil {
			t.Fatalf("mkdir failed: %v", err)
		}
		_, err := ValidateJobFile(libraryDir, "subdir", "")
		if err != ErrFileNotFound {
			t.Errorf("expected ErrFileNotFound for directory, got %v", err)
		}
	})

	t.Run("rejects path traversal in relative_path", func(t *testing.T) {
		for _, traversal := range []string{"../secret.mkv", "../../etc/passwd", "/abs/path.mkv", "subdir/../../escape.mkv"} {
			_, err := ValidateJobFile(libraryDir, traversal, "")
			if err != ErrFileNotFound {
				t.Errorf("expected ErrFileNotFound for traversal %q, got %v", traversal, err)
			}
		}
	})

	t.Run("accepts legal relative path starting with ..", func(t *testing.T) {
		inboxDir := filepath.Join(libraryDir, "..inbox")
		if err := os.MkdirAll(inboxDir, 0755); err != nil {
			t.Fatalf("mkdir failed: %v", err)
		}
		inboxFile := filepath.Join(inboxDir, "movie.mkv")
		if err := os.WriteFile(inboxFile, []byte("content"), 0644); err != nil {
			t.Fatalf("write file failed: %v", err)
		}
		got, err := ValidateJobFile(libraryDir, "..inbox/movie.mkv", "")
		if err != nil {
			t.Fatalf("ValidateJobFile failed for legal .. prefix: %v", err)
		}
		if got != inboxFile {
			t.Fatalf("ValidateJobFile = %q, want %q", got, inboxFile)
		}
	})

	t.Run("rejects symlink according to path safety standards", func(t *testing.T) {
		link := filepath.Join(libraryDir, "symlink_movie.mkv")
		if err := os.Symlink(validFile, link); err != nil {
			t.Fatalf("create symlink failed: %v", err)
		}
		_, err := ValidateJobFile(libraryDir, "symlink_movie.mkv", "")
		if err != ErrFileNotFound {
			t.Errorf("expected ErrFileNotFound for symlink, got %v", err)
		}
	})

	t.Run("empty input returns ErrFileNotFound without fallthrough bug", func(t *testing.T) {
		_, err := ValidateJobFile("", "", "")
		if err != ErrFileNotFound {
			t.Errorf("expected ErrFileNotFound, got %v", err)
		}
		_, err = ValidateJobFile("", "", "/nonexistent/path/file.mkv")
		if err != ErrFileNotFound {
			t.Errorf("expected ErrFileNotFound for missing inputPath, got %v", err)
		}
	})

	t.Run("rejects relative_path without local_path anchor", func(t *testing.T) {
		// Without a library root the join would fall back to the process
		// working directory; an anchorless relative_path is refused outright.
		_, err := ValidateJobFile("", "movie.mkv", "")
		if err != ErrLibraryNotMounted {
			t.Errorf("expected ErrLibraryNotMounted for anchorless relative_path, got %v", err)
		}
	})

	t.Run("rejects bare relative inputPath", func(t *testing.T) {
		_, err := ValidateJobFile("", "", "movie.mkv")
		if err != ErrFileNotFound {
			t.Errorf("expected ErrFileNotFound for relative inputPath, got %v", err)
		}
	})

	t.Run("accepts bare absolute inputPath", func(t *testing.T) {
		got, err := ValidateJobFile("", "", validFile)
		if err != nil {
			t.Fatalf("expected success, got error: %v", err)
		}
		if got != filepath.Clean(validFile) {
			t.Errorf("got %q, want %q", got, validFile)
		}
	})
}

func TestWorker_StatelessJobRejection(t *testing.T) {
	store := newMockLeaseStore()
	runner := newBlockingRunner()

	var rejectedReason string
	var mu sync.Mutex

	var workerRef *Worker
	sup := NewJobSupervisor(SupervisorDeps{
		Runner:      runner,
		Confinement: passThroughConfinement{},
		OnJobComplete: func(jobID string, success bool) {
			if workerRef != nil {
				workerRef.OnJobComplete(jobID, success)
			}
		},
		Send: func(env protocol.Envelope) {
			if env.Type == protocol.TypeFileComplete {
				var p protocol.FileCompletePayload
				if json.Unmarshal(env.Payload, &p) == nil {
					mu.Lock()
					rejectedReason = p.ErrorMessage
					mu.Unlock()
				}
			}
		},
	})

	claimedJobs := []*ClaimedJob{
		{
			ID:           "job-missing-file",
			LocalPath:    t.TempDir(),
			RelativePath: "nonexistent.mkv",
			TargetCodec:  "hevc",
			LeaseStatus:  "leased",
		},
		{
			ID:           "job-unmounted-lib",
			LocalPath:    filepath.Join(t.TempDir(), "not_mounted"),
			RelativePath: "any.mkv",
			TargetCodec:  "hevc",
			LeaseStatus:  "leased",
		},
	}
	jobIdx := 0

	client := &mockLeaseClient{
		claimFunc: func(ctx context.Context, token string) (*ClaimedJob, error) {
			if jobIdx < len(claimedJobs) {
				j := claimedJobs[jobIdx]
				jobIdx++
				return j, nil
			}
			return nil, ErrNoWork
		},
	}

	worker := NewWorker(WorkerDeps{
		ServerURL:     "http://localhost:8082",
		GetToken:      func() string { return "test-token" },
		Supervisor:    sup,
		LeaseStore:    store,
		LeaseClient:   client,
		MinBackoff:    10 * time.Millisecond,
		EnablePolling: true,
	})
	workerRef = worker

	// 1. Validate claimed job 1 directly
	_, err := worker.ValidateJob(claimedJobs[0])
	if err != ErrFileNotFound {
		t.Errorf("expected ErrFileNotFound, got %v", err)
	}

	// 2. Validate claimed job 2 directly
	_, err = worker.ValidateJob(claimedJobs[1])
	if err != ErrLibraryNotMounted {
		t.Errorf("expected ErrLibraryNotMounted, got %v", err)
	}

	// 3. Start worker to process claimed jobs and verify structured error rejection
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker.Start(ctx)
	defer worker.Stop()

	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return rejectedReason == ErrFileNotFound.Error() || rejectedReason == ErrLibraryNotMounted.Error()
	})

	// Ensure lease is cleared upon rejection
	waitFor(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return len(store.deletes) >= 1
	})
}

// TestJobSupervisorLocalRelativePathConfinedToScannedLibrary pins the
// local_path/relative_path branch to the same scanned-library membership rule
// as the plain input_path branch: a job resolving to a real file the scanner
// never recorded is rejected, and one resolving to a scanned file is
// accepted.
func TestJobSupervisorLocalRelativePathConfinedToScannedLibrary(t *testing.T) {
	sqlDB := setupTestDB(t)

	mediaDir := t.TempDir()
	libraryFile := filepath.Join(mediaDir, "movie.mkv")
	if err := os.WriteFile(libraryFile, []byte("video"), 0644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	recordScannedFiles(sqlDB, []scanner.ScannedFile{{FilePath: libraryFile}}, mediaDir)

	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "outside.mkv")
	if err := os.WriteFile(outsideFile, []byte("video"), 0644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	f := newSupervisorFixture(t)
	f.supervisor.confinement = scannedLibraryConfinement{db: sqlDB}

	t.Run("rejects file outside scanned library", func(t *testing.T) {
		payload := testStartPayload("job-outside-lib")
		payload.LocalPath = outsideDir
		payload.RelativePath = "outside.mkv"

		err := f.supervisor.StartJob(context.Background(), payload, nil)
		if err == nil {
			t.Fatal("StartJob accepted a local_path/relative_path job outside the scanned library")
		}
		if !strings.Contains(err.Error(), "outside the scanned media library") {
			t.Fatalf("rejection reason = %v, want outside the scanned media library", err)
		}

		completes := f.envelopesOfType(protocol.TypeFileComplete)
		if len(completes) != 1 {
			t.Fatalf("FILE_COMPLETE envelopes = %d, want 1", len(completes))
		}
		var complete protocol.FileCompletePayload
		if err := json.Unmarshal(completes[0].Payload, &complete); err != nil {
			t.Fatalf("unmarshal FILE_COMPLETE: %v", err)
		}
		if complete.Success || !strings.Contains(complete.ErrorMessage, "outside the scanned media library") {
			t.Fatalf("complete payload = %+v, want failed with confinement reason", complete)
		}
		select {
		case id := <-f.runner.started:
			t.Fatalf("runner was invoked for rejected job %s", id)
		default:
		}
	})

	t.Run("accepts scanned file", func(t *testing.T) {
		payload := testStartPayload("job-inside-lib")
		payload.LocalPath = mediaDir
		payload.RelativePath = "movie.mkv"

		if err := f.supervisor.StartJob(context.Background(), payload, nil); err != nil {
			t.Fatalf("StartJob rejected a scanned local_path/relative_path job: %v", err)
		}
		select {
		case id := <-f.runner.started:
			if id != "job-inside-lib" {
				t.Fatalf("runner started %s, want job-inside-lib", id)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("runner did not start for scanned local_path/relative_path job")
		}
		f.runner.release(payload.JobID)
		waitFor(t, func() bool {
			return f.sawEvent(func(ev Event) bool {
				return ev.Type == EventJobComplete && ev.JobID == payload.JobID && ev.Success
			})
		})
	})
}

// TestWorker_LocalRelativePathConfinedToScannedLibrary proves an
// outside-library local_path/relative_path job is rejected end-to-end through
// the worker: the worker's stateless validation passes (the file exists) and
// JobSupervisor.StartJob — the single membership decision point — refuses the
// job, releasing the lease without ever reaching the runner. A scanned job
// launches.
func TestWorker_LocalRelativePathConfinedToScannedLibrary(t *testing.T) {
	sqlDB := setupTestDB(t)

	mediaDir := t.TempDir()
	libraryFile := filepath.Join(mediaDir, "movie.mkv")
	if err := os.WriteFile(libraryFile, []byte("video"), 0644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	recordScannedFiles(sqlDB, []scanner.ScannedFile{{FilePath: libraryFile}}, mediaDir)

	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "outside.mkv")
	if err := os.WriteFile(outsideFile, []byte("video"), 0644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	store := newMockLeaseStore()
	runner := newBlockingRunner()

	var rejectMu sync.Mutex
	var rejected []string

	var workerRef *Worker
	sup := NewJobSupervisor(SupervisorDeps{
		Runner:      runner,
		Confinement: scannedLibraryConfinement{db: sqlDB},
		OnJobComplete: func(jobID string, success bool) {
			if workerRef != nil {
				workerRef.OnJobComplete(jobID, success)
			}
		},
		Send: func(env protocol.Envelope) {
			if env.Type != protocol.TypeFileComplete {
				return
			}
			var p protocol.FileCompletePayload
			if json.Unmarshal(env.Payload, &p) == nil && !p.Success {
				rejectMu.Lock()
				rejected = append(rejected, p.JobID)
				rejectMu.Unlock()
			}
		},
	})

	claimedJobs := []*ClaimedJob{
		{
			ID:           "job-outside-lib",
			LocalPath:    outsideDir,
			RelativePath: "outside.mkv",
			TargetCodec:  "hevc",
			LeaseStatus:  "leased",
		},
		{
			ID:           "job-inside-lib",
			LocalPath:    mediaDir,
			RelativePath: "movie.mkv",
			TargetCodec:  "hevc",
			LeaseStatus:  "leased",
		},
	}
	jobIdx := 0
	client := &mockLeaseClient{
		claimFunc: func(ctx context.Context, token string) (*ClaimedJob, error) {
			if jobIdx < len(claimedJobs) {
				j := claimedJobs[jobIdx]
				jobIdx++
				return j, nil
			}
			return nil, ErrNoWork
		},
	}

	worker := NewWorker(WorkerDeps{
		ServerURL:     "http://localhost:8082",
		GetToken:      func() string { return "test-token" },
		Supervisor:    sup,
		LeaseStore:    store,
		LeaseClient:   client,
		MinBackoff:    10 * time.Millisecond,
		EnablePolling: true,
	})
	workerRef = worker

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker.Start(ctx)
	defer worker.Stop()

	// The outside-library job survives the worker's stateless validation (the
	// file exists), then StartJob's confinement gate refuses it; the
	// supervisor's rejection triggers OnJobComplete, clearing the lease
	// without the runner ever seeing the job.
	waitFor(t, func() bool {
		rejectMu.Lock()
		defer rejectMu.Unlock()
		return len(rejected) >= 1
	})
	waitFor(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return len(store.deletes) >= 1
	})

	// The scanned job passes both gates and starts.
	select {
	case id := <-runner.started:
		if id != "job-inside-lib" {
			t.Fatalf("runner started %s, want job-inside-lib", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not start for scanned local_path/relative_path job")
	}
	close(runner.release)

	waitFor(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return len(store.deletes) >= 2
	})
}

// TestStatelessFolderConfinement_UnscannedFilesSucceed verifies AC 4c:
// jobs for unscanned files confined within the configured library directory
// succeed statelessly rather than being rejected for absence from SQLite history.
func TestStatelessFolderConfinement_UnscannedFilesSucceed(t *testing.T) {
	sqlDB := setupTestDB(t)

	mediaDir := t.TempDir()
	unscannedFile := filepath.Join(mediaDir, "unscanned_movie.mkv")
	if err := os.WriteFile(unscannedFile, []byte("video data"), 0644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	// Note: We deliberately DO NOT call recordScannedFiles; sqlDB has zero records.

	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "outside_movie.mkv")
	if err := os.WriteFile(outsideFile, []byte("video data"), 0644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	f := newSupervisorFixture(t)
	f.supervisor.mediaDir = mediaDir
	f.supervisor.confinement = scannedLibraryConfinement{db: sqlDB, mediaDir: mediaDir}

	t.Run("unscanned file confined within mediaDir succeeds", func(t *testing.T) {
		payload := testStartPayload("job-unscanned-inside")
		payload.LocalPath = mediaDir
		payload.RelativePath = "unscanned_movie.mkv"

		if err := f.supervisor.StartJob(context.Background(), payload, nil); err != nil {
			t.Fatalf("StartJob rejected unscanned file within mediaDir: %v", err)
		}
		select {
		case id := <-f.runner.started:
			if id != "job-unscanned-inside" {
				t.Fatalf("runner started %s, want job-unscanned-inside", id)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("runner did not start for unscanned file inside mediaDir")
		}
		f.runner.release(payload.JobID)
	})

	t.Run("file outside mediaDir is rejected", func(t *testing.T) {
		payload := testStartPayload("job-unscanned-outside")
		payload.LocalPath = outsideDir
		payload.RelativePath = "outside_movie.mkv"

		err := f.supervisor.StartJob(context.Background(), payload, nil)
		if err == nil {
			t.Fatal("StartJob accepted file outside mediaDir")
		}
		if !strings.Contains(err.Error(), "outside the scanned media library") {
			t.Fatalf("unexpected error message: %v", err)
		}
	})
}

// TestCrossPlatformPathNormalization_Confinement verifies AC 4b:
// Windows-style paths dispatched to Linux agents (and vice-versa) normalize
// and validate confinement statelessly.
func TestCrossPlatformPathNormalization_Confinement(t *testing.T) {
	mediaDir := t.TempDir()
	subDir := filepath.Join(mediaDir, "Movies", "Sci-Fi")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}
	targetFile := filepath.Join(subDir, "The Matrix.mkv")
	if err := os.WriteFile(targetFile, []byte("matrix video"), 0644); err != nil {
		t.Fatalf("write file failed: %v", err)
	}

	t.Run("Windows backslash path normalizes and validates", func(t *testing.T) {
		got, err := ValidateJobFile(mediaDir, `Movies\Sci-Fi\The Matrix.mkv`, "")
		if err != nil {
			t.Fatalf("ValidateJobFile failed for Windows-style path: %v", err)
		}
		if got != targetFile {
			t.Fatalf("ValidateJobFile = %q, want %q", got, targetFile)
		}
	})

	t.Run("Windows redundant backslashes normalize and validate", func(t *testing.T) {
		got, err := ValidateJobFile(mediaDir, `Movies\\Sci-Fi\\The Matrix.mkv`, "")
		if err != nil {
			t.Fatalf("ValidateJobFile failed for redundant backslashes: %v", err)
		}
		if got != targetFile {
			t.Fatalf("ValidateJobFile = %q, want %q", got, targetFile)
		}
	})

	t.Run("POSIX forward slash path validates", func(t *testing.T) {
		got, err := ValidateJobFile(mediaDir, "Movies/Sci-Fi/The Matrix.mkv", "")
		if err != nil {
			t.Fatalf("ValidateJobFile failed for POSIX path: %v", err)
		}
		if got != targetFile {
			t.Fatalf("ValidateJobFile = %q, want %q", got, targetFile)
		}
	})

	t.Run("Windows backslash directory traversal is rejected", func(t *testing.T) {
		_, err := ValidateJobFile(mediaDir, `Movies\..\..\outside.mkv`, "")
		if err != ErrFileNotFound {
			t.Fatalf("expected ErrFileNotFound for escaping backslash path, got: %v", err)
		}
	})

	t.Run("Bare Windows drive letter or absolute path is rejected", func(t *testing.T) {
		_, err := ValidateJobFile(mediaDir, `C:\Windows\System32\calc.exe`, "")
		if err != ErrFileNotFound {
			t.Fatalf("expected ErrFileNotFound for absolute Windows path, got: %v", err)
		}
	})
}

func TestIsPathConfined(t *testing.T) {
	rootDir := t.TempDir()
	outsideDir := t.TempDir()

	subDir := filepath.Join(rootDir, "valid_sub")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("failed to create subDir: %v", err)
	}

	outsideSymlink := filepath.Join(rootDir, "symlink_outside")
	if err := os.Symlink(outsideDir, outsideSymlink); err != nil {
		t.Fatalf("failed to create outside symlink: %v", err)
	}

	insideSymlink := filepath.Join(rootDir, "symlink_inside")
	if err := os.Symlink(subDir, insideSymlink); err != nil {
		t.Fatalf("failed to create inside symlink: %v", err)
	}

	brokenSymlink := filepath.Join(rootDir, "broken_symlink")
	if err := os.Symlink(filepath.Join(outsideDir, "nonexistent_target"), brokenSymlink); err != nil {
		t.Fatalf("failed to create broken symlink: %v", err)
	}

	existingFile := filepath.Join(rootDir, "existing.mkv")
	if err := os.WriteFile(existingFile, []byte("data"), 0644); err != nil {
		t.Fatalf("failed to create existing file: %v", err)
	}

	existingOutsideFile := filepath.Join(outsideDir, "outside.mkv")
	if err := os.WriteFile(existingOutsideFile, []byte("data"), 0644); err != nil {
		t.Fatalf("failed to create existing outside file: %v", err)
	}
	symlinkToFileOutside := filepath.Join(rootDir, "link_to_outside.mkv")
	if err := os.Symlink(existingOutsideFile, symlinkToFileOutside); err != nil {
		t.Fatalf("failed to create symlink to outside file: %v", err)
	}

	t.Run("non-existent file directly in root is confined", func(t *testing.T) {
		confined, err := isPathConfined(rootDir, filepath.Join(rootDir, "new_file.mkv"))
		if err != nil || !confined {
			t.Fatalf("expected confined true, got confined=%v err=%v", confined, err)
		}
	})

	t.Run("non-existent file in valid subdirectory is confined", func(t *testing.T) {
		confined, err := isPathConfined(rootDir, filepath.Join(subDir, "new_file.mkv"))
		if err != nil || !confined {
			t.Fatalf("expected confined true, got confined=%v err=%v", confined, err)
		}
	})

	t.Run("non-existent file in symlink pointing inside root is confined", func(t *testing.T) {
		confined, err := isPathConfined(rootDir, filepath.Join(insideSymlink, "new_file.mkv"))
		if err != nil || !confined {
			t.Fatalf("expected confined true, got confined=%v err=%v", confined, err)
		}
	})

	t.Run("non-existent file via intermediate symlink pointing outside root is rejected", func(t *testing.T) {
		confined, err := isPathConfined(rootDir, filepath.Join(outsideSymlink, "non_existent_dest.mkv"))
		if err != nil {
			t.Fatalf("expected err == nil for escaped confinement, got %v", err)
		}
		if confined {
			t.Fatalf("expected escaped path via outside symlink to be rejected, got confined=true")
		}
	})

	t.Run("non-existent file in broken symlink parent fails closed", func(t *testing.T) {
		confined, err := isPathConfined(rootDir, filepath.Join(brokenSymlink, "new_file.mkv"))
		if err == nil {
			t.Fatalf("expected error for broken parent symlink, got nil (confined=%v)", confined)
		}
		if confined {
			t.Fatalf("expected confined=false on error")
		}
	})

	t.Run("non-existent file in non-existent parent directory fails closed", func(t *testing.T) {
		confined, err := isPathConfined(rootDir, filepath.Join(rootDir, "no_such_dir", "new_file.mkv"))
		if err == nil {
			t.Fatalf("expected error for non-existent parent directory, got nil (confined=%v)", confined)
		}
		if confined {
			t.Fatalf("expected confined=false on error")
		}
	})

	t.Run("non-existent root directory fails closed", func(t *testing.T) {
		confined, err := isPathConfined(filepath.Join(rootDir, "missing_root"), filepath.Join(rootDir, "new_file.mkv"))
		if err == nil {
			t.Fatalf("expected error for non-existent root directory, got nil (confined=%v)", confined)
		}
		if confined {
			t.Fatalf("expected confined=false on error")
		}
	})

	t.Run("existing file in root is confined", func(t *testing.T) {
		confined, err := isPathConfined(rootDir, existingFile)
		if err != nil || !confined {
			t.Fatalf("expected confined true, got confined=%v err=%v", confined, err)
		}
	})

	t.Run("existing file symlinked to outside is rejected", func(t *testing.T) {
		confined, err := isPathConfined(rootDir, symlinkToFileOutside)
		if err != nil {
			t.Fatalf("expected err == nil for escaped target, got %v", err)
		}
		if confined {
			t.Fatalf("expected symlink to outside file to be rejected, got confined=true")
		}
	})

	t.Run("root directory itself is not confined as descendant file", func(t *testing.T) {
		confined, err := isPathConfined(rootDir, rootDir)
		if err != nil {
			t.Fatalf("expected err == nil, got %v", err)
		}
		if confined {
			t.Fatalf("expected root path itself to not be confined descendant")
		}
	})

	t.Run("empty root or target returns false without error", func(t *testing.T) {
		if c, err := isPathConfined("", existingFile); err != nil || c {
			t.Fatalf("expected false, nil for empty root, got %v, %v", c, err)
		}
		if c, err := isPathConfined(rootDir, ""); err != nil || c {
			t.Fatalf("expected false, nil for empty target, got %v, %v", c, err)
		}
	})
}


