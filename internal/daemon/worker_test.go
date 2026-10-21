package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/will25u/slimmedia-agent/internal/db"
	"github.com/will25u/slimmedia-agent/internal/jobspec"
	"github.com/will25u/slimmedia-agent/internal/transcoder"
)

type mockLeaseStore struct {
	mu      sync.Mutex
	leases  map[string]db.JobLease
	upserts []db.UpsertJobLeaseParams
	deletes []string
}

func newMockLeaseStore() *mockLeaseStore {
	return &mockLeaseStore{
		leases: make(map[string]db.JobLease),
	}
}

func (m *mockLeaseStore) UpsertJobLease(ctx context.Context, arg db.UpsertJobLeaseParams) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.upserts = append(m.upserts, arg)
	m.leases[arg.JobID] = db.JobLease{
		JobID:          arg.JobID,
		LeaseStatus:    arg.LeaseStatus,
		LeaseExpiresAt: arg.LeaseExpiresAt,
		UpdatedAt:      time.Now(),
	}
	return nil
}

func (m *mockLeaseStore) DeleteJobLease(ctx context.Context, jobID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletes = append(m.deletes, jobID)
	delete(m.leases, jobID)
	return nil
}

func (m *mockLeaseStore) GetJobLease(ctx context.Context, jobID string) (db.JobLease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.leases[jobID]
	if !ok {
		return db.JobLease{}, sql.ErrNoRows
	}
	return l, nil
}

func (m *mockLeaseStore) ListActiveJobLeases(ctx context.Context) ([]db.JobLease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var res []db.JobLease
	for _, l := range m.leases {
		res = append(res, l)
	}
	return res, nil
}

type mockLeaseClient struct {
	mu           sync.Mutex
	claimFunc    func(ctx context.Context, token string) (*ClaimedJob, error)
	heartbeatFn  func(ctx context.Context, token string, jobID string) error
	claimCalls   int
	heartbeatCalls int
}

func (m *mockLeaseClient) ClaimJob(ctx context.Context, token string) (*ClaimedJob, error) {
	m.mu.Lock()
	m.claimCalls++
	fn := m.claimFunc
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, token)
	}
	return nil, ErrNoWork
}

func (m *mockLeaseClient) HeartbeatJob(ctx context.Context, token string, jobID string) error {
	m.mu.Lock()
	m.heartbeatCalls++
	fn := m.heartbeatFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, token, jobID)
	}
	return nil
}

type blockingRunner struct {
	mu      sync.Mutex
	started chan string
	release chan struct{}
}

func newBlockingRunner() *blockingRunner {
	return &blockingRunner{
		started: make(chan string, 16),
		release: make(chan struct{}),
	}
}

func (b *blockingRunner) ProcessJobWithSpec(ctx context.Context, job *transcoder.Job, targetCodec string, spec jobspec.JobSpec, hooks transcoder.JobHooks, onProgress transcoder.ProgressCallback) (*transcoder.JobResult, error) {
	b.started <- job.ID
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-b.release:
		return &transcoder.JobResult{
			OriginalSize: 1000,
			NewSize:      500,
			SavingsBytes: 500,
			DurationSecs: 10,
		}, nil
	}
}

func (b *blockingRunner) DiscardJobFiles(ctx context.Context, filePath, jobID string) {}

func TestWorker_PollAndClaimSuccess(t *testing.T) {
	store := newMockLeaseStore()
	runner := newBlockingRunner()

	var workerRef *Worker
	sup := NewJobSupervisor(SupervisorDeps{
		Runner:      runner,
		Confinement: passThroughConfinement{},
		OnJobComplete: func(jobID string, success bool) {
			if workerRef != nil {
				workerRef.OnJobComplete(jobID, success)
			}
		},
	})

	jobClaimed := false
	expires := time.Now().Add(1 * time.Hour)
	client := &mockLeaseClient{
		claimFunc: func(ctx context.Context, token string) (*ClaimedJob, error) {
			if !jobClaimed {
				jobClaimed = true
				return &ClaimedJob{
					ID:             "job-123",
					InputPath:      "/media/video.mkv",
					TargetCodec:    "hevc",
					LeaseStatus:    "leased",
					LeaseExpiresAt: &expires,
				}, nil
			}
			return nil, ErrNoWork
		},
	}

	worker := NewWorker(WorkerDeps{
		ServerURL:         "http://localhost:8082",
		GetToken:          func() string { return "test-token" },
		Supervisor:        sup,
		LeaseStore:        store,
		LeaseClient:       client,
		Concurrency:       1,
		MinBackoff:        10 * time.Millisecond,
		MaxBackoff:        50 * time.Millisecond,
		HeartbeatInterval: 20 * time.Millisecond,
		EnablePolling:     true,
	})
	workerRef = worker

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	worker.Start(ctx)
	defer worker.Stop()

	// Wait for job to start
	select {
	case jobID := <-runner.started:
		if jobID != "job-123" {
			t.Fatalf("expected job-123, got %s", jobID)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for job to start")
	}

	// Verify lease record was written to store before completion
	store.mu.Lock()
	if len(store.upserts) != 1 || store.upserts[0].JobID != "job-123" {
		t.Fatalf("expected 1 upsert for job-123, got %v", store.upserts)
	}
	if len(store.leases) != 1 {
		t.Fatalf("expected 1 active lease, got %d", len(store.leases))
	}
	store.mu.Unlock()

	// Wait for at least one heartbeat
	waitFor(t, func() bool {
		client.mu.Lock()
		defer client.mu.Unlock()
		return client.heartbeatCalls >= 1
	})

	// Release job and let it complete
	close(runner.release)

	// Verify lease was deleted upon completion
	waitFor(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return len(store.deletes) == 1 && store.deletes[0] == "job-123"
	})
}

func TestWorker_ConcurrencyPausesPolling(t *testing.T) {
	store := newMockLeaseStore()
	runner := newBlockingRunner()

	var workerRef *Worker
	sup := NewJobSupervisor(SupervisorDeps{
		Runner:      runner,
		Confinement: passThroughConfinement{},
		OnJobComplete: func(jobID string, success bool) {
			if workerRef != nil {
				workerRef.OnJobComplete(jobID, success)
			}
		},
	})

	var claimCount atomic.Int32
	client := &mockLeaseClient{
		claimFunc: func(ctx context.Context, token string) (*ClaimedJob, error) {
			count := claimCount.Add(1)
			if count == 1 {
				return &ClaimedJob{
					ID:          "job-concurrency-1",
					InputPath:   "/media/test.mkv",
					TargetCodec: "hevc",
					LeaseStatus: "leased",
				}, nil
			}
			return nil, ErrNoWork
		},
	}

	worker := NewWorker(WorkerDeps{
		ServerURL:         "http://localhost:8082",
		GetToken:          func() string { return "test-token" },
		Supervisor:        sup,
		LeaseStore:        store,
		LeaseClient:       client,
		Concurrency:       1,
		MinBackoff:        10 * time.Millisecond,
		MaxBackoff:        20 * time.Millisecond,
		HeartbeatInterval: 100 * time.Millisecond,
		EnablePolling:     true,
	})
	workerRef = worker

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	worker.Start(ctx)
	defer worker.Stop()

	// Wait for job to start
	select {
	case <-runner.started:
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for job to start")
	}

	// While job is in flight, claimCount must remain 1 because concurrency=1 is saturated
	time.Sleep(100 * time.Millisecond)
	if claimCount.Load() != 1 {
		t.Fatalf("expected claimCount to stay 1 while running, got %d", claimCount.Load())
	}

	// Release job to finish
	close(runner.release)

	// Once job completes, capacity is restored, so claim count increases
	waitFor(t, func() bool {
		return claimCount.Load() > 1
	})
}

func TestWorker_ConcurrencyFromClaimRunsJobsInParallel(t *testing.T) {
	store := newMockLeaseStore()
	runner := newBlockingRunner()

	var workerRef *Worker
	sup := NewJobSupervisor(SupervisorDeps{
		Runner:      runner,
		Confinement: passThroughConfinement{},
		OnJobComplete: func(jobID string, success bool) {
			if workerRef != nil {
				workerRef.OnJobComplete(jobID, success)
			}
		},
	})

	var claimCount atomic.Int32
	client := &mockLeaseClient{
		claimFunc: func(ctx context.Context, token string) (*ClaimedJob, error) {
			count := claimCount.Add(1)
			if count > 2 {
				return nil, ErrNoWork
			}
			return &ClaimedJob{
				ID:          fmt.Sprintf("job-parallel-%d", count),
				InputPath:   fmt.Sprintf("/media/test-%d.mkv", count),
				TargetCodec: "hevc",
				LeaseStatus: "leased",
				// The server advertises the user's global limit per claim.
				ConcurrentTranscodeLimit: 2,
			}, nil
		},
	}

	worker := NewWorker(WorkerDeps{
		ServerURL:         "http://localhost:8082",
		GetToken:          func() string { return "test-token" },
		Supervisor:        sup,
		LeaseStore:        store,
		LeaseClient:       client,
		Concurrency:       1,
		MinBackoff:        10 * time.Millisecond,
		MaxBackoff:        20 * time.Millisecond,
		HeartbeatInterval: 100 * time.Millisecond,
		EnablePolling:     true,
	})
	workerRef = worker

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker.Start(ctx)
	defer worker.Stop()

	// Both jobs must be running concurrently once the advertised limit
	// raises the worker's capacity from its initial 1.
	for i := 0; i < 2; i++ {
		select {
		case <-runner.started:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for job %d to start", i+1)
		}
	}

	// Releasing one job must not wait on the other.
	close(runner.release)
	waitFor(t, func() bool {
		return claimCount.Load() > 2
	})
}

func TestWorker_SetConcurrencyClamps(t *testing.T) {
	w := &Worker{}
	w.setConcurrency(0)
	if w.concurrency != 1 {
		t.Errorf("concurrency = %d, want 1 for zero input", w.concurrency)
	}
	w.setConcurrency(1000)
	if w.concurrency != maxConcurrentTranscodes {
		t.Errorf("concurrency = %d, want cap %d", w.concurrency, maxConcurrentTranscodes)
	}
	w.setConcurrency(2)
	if w.concurrency != 2 {
		t.Errorf("concurrency = %d, want 2", w.concurrency)
	}
}

func TestWorker_HeartbeatRevokedCancelsJob(t *testing.T) {
	store := newMockLeaseStore()
	runner := newBlockingRunner()

	var workerRef *Worker
	sup := NewJobSupervisor(SupervisorDeps{
		Runner:      runner,
		Confinement: passThroughConfinement{},
		OnJobComplete: func(jobID string, success bool) {
			if workerRef != nil {
				workerRef.OnJobComplete(jobID, success)
			}
		},
	})

	client := &mockLeaseClient{
		claimFunc: func(ctx context.Context, token string) (*ClaimedJob, error) {
			return &ClaimedJob{
				ID:          "job-revoked",
				InputPath:   "/media/test.mkv",
				TargetCodec: "hevc",
				LeaseStatus: "leased",
			}, nil
		},
		heartbeatFn: func(ctx context.Context, token string, jobID string) error {
			return ErrLeaseRevoked
		},
	}

	worker := NewWorker(WorkerDeps{
		ServerURL:            "http://localhost:8082",
		GetToken:             func() string { return "test-token" },
		Supervisor:           sup,
		LeaseStore:           store,
		LeaseClient:          client,
		Concurrency:          1,
		MinBackoff:           10 * time.Millisecond,
		HeartbeatInterval:    10 * time.Millisecond,
		MaxHeartbeatFailures: 2,
		EnablePolling:        true,
	})
	workerRef = worker

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	worker.Start(ctx)
	defer worker.Stop()

	// Wait for job to start
	select {
	case <-runner.started:
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for job to start")
	}

	// Heartbeat should fail repeatedly with ErrLeaseRevoked, causing supervisor.CancelJob
	// which cancels runner context and triggers OnJobComplete -> DeleteJobLease
	waitFor(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return len(store.deletes) >= 1 && store.deletes[0] == "job-revoked"
	})
}

func TestWorker_SupervisorRejectionClearsLease(t *testing.T) {
	store := newMockLeaseStore()
	runner := newBlockingRunner()

	// Reject all inputs by failing confinement check
	confinement := &failingConfinement{err: errors.New("file not in library")}

	var workerRef *Worker
	sup := NewJobSupervisor(SupervisorDeps{
		Runner:      runner,
		Confinement: confinement,
		OnJobComplete: func(jobID string, success bool) {
			if workerRef != nil {
				workerRef.OnJobComplete(jobID, success)
			}
		},
	})

	claimedOnce := false
	client := &mockLeaseClient{
		claimFunc: func(ctx context.Context, token string) (*ClaimedJob, error) {
			if !claimedOnce {
				claimedOnce = true
				return &ClaimedJob{
					ID:          "job-rejected",
					InputPath:   "/media/unscanned.mkv",
					TargetCodec: "hevc",
					LeaseStatus: "leased",
				}, nil
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

	// Job should be rejected by supervisor and lease immediately cleared
	waitFor(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return len(store.upserts) >= 1 && len(store.deletes) >= 1
	})
}

func TestWorker_ExponentialBackoff(t *testing.T) {
	store := newMockLeaseStore()
	runner := newBlockingRunner()

	sup := NewJobSupervisor(SupervisorDeps{
		Runner:      runner,
		Confinement: passThroughConfinement{},
	})

	var claimTimes []time.Time
	var claimMu sync.Mutex

	client := &mockLeaseClient{
		claimFunc: func(ctx context.Context, token string) (*ClaimedJob, error) {
			claimMu.Lock()
			claimTimes = append(claimTimes, time.Now())
			claimMu.Unlock()
			return nil, ErrNoWork
		},
	}

	worker := NewWorker(WorkerDeps{
		ServerURL:     "http://localhost:8082",
		GetToken:      func() string { return "test-token" },
		Supervisor:    sup,
		LeaseStore:    store,
		LeaseClient:   client,
		MinBackoff:    20 * time.Millisecond,
		MaxBackoff:    80 * time.Millisecond,
		EnablePolling: true,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	worker.Start(ctx)
	defer worker.Stop()

	// Wait for at least 4 claims to record backoffs
	waitFor(t, func() bool {
		claimMu.Lock()
		defer claimMu.Unlock()
		return len(claimTimes) >= 4
	})

	claimMu.Lock()
	defer claimMu.Unlock()
	d1 := claimTimes[1].Sub(claimTimes[0])
	d2 := claimTimes[2].Sub(claimTimes[1])
	d3 := claimTimes[3].Sub(claimTimes[2])

	// Each subsequent backoff interval should be larger than or equal to the previous (within scheduling tolerance)
	if d2 < d1*9/10 {
		t.Fatalf("expected d2 (%v) >= d1 (%v)", d2, d1)
	}
	if d3 < d2*9/10 {
		t.Fatalf("expected d3 (%v) >= d2 (%v)", d3, d2)
	}
}

type failingConfinement struct {
	err error
}

func (f *failingConfinement) ValidateJobInputPath(inputPath string) (string, error) {
	return "", f.err
}

// TestClaimJobRejectsOversizedSuccessBody pins that the job-claim success
// decode is bounded like the error path: a server dripping a 200 body over
// the response cap fails the claim instead of being buffered without limit.
func TestClaimJobRejectsOversizedSuccessBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Unterminated JSON string that exceeds the response cap.
		_, _ = w.Write([]byte(`{"id":"` + strings.Repeat("x", maxResponseBodyBytes+1)))
	}))
	defer ts.Close()

	client := newHTTPLeaseClient(ts.URL)
	if _, err := client.ClaimJob(context.Background(), "test-token"); err == nil {
		t.Fatal("ClaimJob accepted an oversized success body")
	}
}

// TestWorker_StatelessCrossPlatformValidation pins that the worker statelessly validates
// cross-platform paths (Windows \ or POSIX /) and falls back to MediaDir when local_path is omitted.
func TestWorker_StatelessCrossPlatformValidation(t *testing.T) {
	mediaDir := t.TempDir()
	subDir := filepath.Join(mediaDir, "tv", "show1")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}
	targetFile := filepath.Join(subDir, "s01e01.mkv")
	if err := os.WriteFile(targetFile, []byte("episode"), 0644); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	worker := &Worker{
		mediaDir: mediaDir,
	}

	// 1. ClaimedJob with Windows-style backslashes and explicit LocalPath
	jobWin := &ClaimedJob{
		ID:           "job-win",
		LocalPath:    mediaDir,
		RelativePath: `tv\show1\s01e01.mkv`,
	}
	path, err := worker.ValidateJob(jobWin)
	if err != nil {
		t.Fatalf("ValidateJob failed for Windows relative path: %v", err)
	}
	if path != targetFile {
		t.Errorf("ValidateJob = %q, want %q", path, targetFile)
	}

	// 2. ClaimedJob with omitted LocalPath falling back to worker.mediaDir
	jobFallback := &ClaimedJob{
		ID:           "job-fallback",
		RelativePath: `tv/show1/s01e01.mkv`,
	}
	path, err = worker.ValidateJob(jobFallback)
	if err != nil {
		t.Fatalf("ValidateJob failed for fallback to mediaDir: %v", err)
	}
	if path != targetFile {
		t.Errorf("ValidateJob = %q, want %q", path, targetFile)
	}

	// 3. Traversal outside mediaDir is refused
	jobEscape := &ClaimedJob{
		ID:           "job-escape",
		LocalPath:    mediaDir,
		RelativePath: `..\outside.mkv`,
	}
	_, err = worker.ValidateJob(jobEscape)
	if err != ErrFileNotFound {
		t.Errorf("expected ErrFileNotFound for escaping path, got: %v", err)
	}
}

type specRecordingRunner struct {
	specChan chan jobspec.JobSpec
}

func (s *specRecordingRunner) ProcessJobWithSpec(_ context.Context, job *transcoder.Job, _ string, spec jobspec.JobSpec, _ transcoder.JobHooks, _ transcoder.ProgressCallback) (*transcoder.JobResult, error) {
	s.specChan <- spec
	return &transcoder.JobResult{OriginalSize: 100, NewSize: 50, SavingsBytes: 50, DurationSecs: 1}, nil
}

func (s *specRecordingRunner) DiscardJobFiles(_ context.Context, _, _ string) {}

func TestWorker_ClaimPreservesResolvedPayload(t *testing.T) {
	runner := &specRecordingRunner{specChan: make(chan jobspec.JobSpec, 1)}
	sup := NewJobSupervisor(SupervisorDeps{
		Runner:      runner,
		Confinement: passThroughConfinement{},
	})

	resolvedJSON := []byte(`{
		"spec_version": 2,
		"job_id": "job-preset-test",
		"video": {
			"target_codec": "hevc",
			"encoder_engine": "cpu",
			"crf": 22,
			"preset_speed": "medium",
			"quality_target": 22,
			"color_depth": "10bit"
		}
	}`)

	claimed := false
	expires := time.Now().Add(1 * time.Hour)
	client := &mockLeaseClient{
		claimFunc: func(ctx context.Context, token string) (*ClaimedJob, error) {
			if !claimed {
				claimed = true
				return &ClaimedJob{
					ID:              "job-preset-test",
					InputPath:       "/media/video.mkv",
					TargetCodec:     "hevc",
					LeaseStatus:     "leased",
					LeaseExpiresAt:  &expires,
					ResolvedPayload: resolvedJSON,
				}, nil
			}
			return nil, ErrNoWork
		},
	}

	worker := NewWorker(WorkerDeps{
		ServerURL:         "http://localhost:8082",
		GetToken:          func() string { return "test-token" },
		Supervisor:        sup,
		LeaseStore:        newMockLeaseStore(),
		LeaseClient:       client,
		Concurrency:       1,
		MinBackoff:        10 * time.Millisecond,
		MaxBackoff:        50 * time.Millisecond,
		HeartbeatInterval: 20 * time.Millisecond,
		EnablePolling:     true,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	worker.Start(ctx)
	defer worker.Stop()

	select {
	case spec := <-runner.specChan:
		if spec.CRF != 22 {
			t.Errorf("expected CRF 22 from resolved payload, got %d", spec.CRF)
		}
		if spec.PresetSpeed != "medium" {
			t.Errorf("expected preset speed 'medium', got %q", spec.PresetSpeed)
		}
		if spec.EncoderEngine != "cpu" {
			t.Errorf("expected encoder engine 'cpu', got %q", spec.EncoderEngine)
		}
		if spec.ColorDepth != "10bit" {
			t.Errorf("expected color depth '10bit', got %q", spec.ColorDepth)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for job spec")
	}
}
