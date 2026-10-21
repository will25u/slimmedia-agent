package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/will25u/slimmedia-agent/internal/db"
	"github.com/will25u/slimmedia-agent/internal/transcoder"
	"github.com/will25u/slimmedia-agent/internal/protocol"
)

var (
	// ErrNoWork reports that the queue currently has no unassigned or eligible jobs.
	ErrNoWork = errors.New("no work available in queue")
	// ErrLeaseRevoked reports that the server rejected the heartbeat because the lease expired or was revoked.
	ErrLeaseRevoked = errors.New("lease revoked or expired")
	// ErrFileNotFound reports that the target media file was not found on disk.
	ErrFileNotFound = errors.New("ErrFileNotFound")
	// ErrLibraryNotMounted reports that the library local path mount does not exist or is not a directory.
	ErrLibraryNotMounted = errors.New("ErrLibraryNotMounted")
)

// LeaseStore defines the SQLite persistence methods for active job leases.
// *db.Queries satisfies this interface.
type LeaseStore interface {
	UpsertJobLease(ctx context.Context, arg db.UpsertJobLeaseParams) error
	DeleteJobLease(ctx context.Context, jobID string) error
	GetJobLease(ctx context.Context, jobID string) (db.JobLease, error)
	ListActiveJobLeases(ctx context.Context) ([]db.JobLease, error)
}

// ClaimedJob models the job payload returned by POST /api/v1/jobs/claim.
type ClaimedJob struct {
	ID             string     `json:"id"`
	UserID         string     `json:"user_id"`
	AgentID        string     `json:"agent_id"`
	MediaItemID    string     `json:"media_item_id"`
	Status         string     `json:"status"`
	InputPath      string     `json:"input_path"`
	RelativePath   string     `json:"relative_path,omitempty"`
	LocalPath      string     `json:"local_path,omitempty"`
	TargetCodec    string     `json:"target_codec"`
	OriginalSize   int64      `json:"original_size"`
	LeaseStatus    string     `json:"lease_status"`
	AssignedWorker string          `json:"assigned_worker"`
	LeaseExpiresAt *time.Time      `json:"lease_expires_at"`
	ResolvedPayload json.RawMessage `json:"resolved_payload,omitempty"`
	// ConcurrentTranscodeLimit is the user's global setting, advertised
	// with every claim so the worker can size its job concurrency.
	ConcurrentTranscodeLimit int32 `json:"concurrent_transcode_limit,omitempty"`
}

type claimedJobAlias ClaimedJob

// UnmarshalJSON flexibly unmarshals ClaimedJob supporting RFC3339 string or sql.NullTime for lease_expires_at.
func (c *ClaimedJob) UnmarshalJSON(data []byte) error {
	var raw struct {
		claimedJobAlias
		LeaseExpiresAt json.RawMessage `json:"lease_expires_at"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*c = ClaimedJob(raw.claimedJobAlias)

	if len(raw.LeaseExpiresAt) > 0 && string(raw.LeaseExpiresAt) != "null" {
		var s string
		if err := json.Unmarshal(raw.LeaseExpiresAt, &s); err == nil && s != "" {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				c.LeaseExpiresAt = &t
				return nil
			}
			if t, err := time.Parse("2006-01-02T15:04:05Z07:00", s); err == nil {
				c.LeaseExpiresAt = &t
				return nil
			}
		}

		var nt struct {
			Time  time.Time `json:"Time"`
			Valid bool      `json:"Valid"`
		}
		if err := json.Unmarshal(raw.LeaseExpiresAt, &nt); err == nil && nt.Valid {
			c.LeaseExpiresAt = &nt.Time
			return nil
		}
	}
	c.LeaseExpiresAt = nil
	return nil
}


// LeaseClient models the HTTP interactions for leasing and heartbeating jobs.
type LeaseClient interface {
	ClaimJob(ctx context.Context, token string) (*ClaimedJob, error)
	HeartbeatJob(ctx context.Context, token string, jobID string) error
}

type httpLeaseClient struct {
	serverURL  string
	httpClient *http.Client
}

func newHTTPLeaseClient(serverURL string) *httpLeaseClient {
	return &httpLeaseClient{
		serverURL:  strings.TrimRight(serverURL, "/"),
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
}

func (c *httpLeaseClient) ClaimJob(ctx context.Context, token string) (*ClaimedJob, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.serverURL+"/api/v1/jobs/claim", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		return nil, ErrNoWork
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyBytes))
		return nil, fmt.Errorf("claim job failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var job ClaimedJob
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBodyBytes)).Decode(&job); err != nil {
		return nil, fmt.Errorf("failed to decode claimed job: %w", err)
	}
	return &job, nil
}

func (c *httpLeaseClient) HeartbeatJob(ctx context.Context, token string, jobID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.serverURL+"/api/v1/jobs/"+jobID+"/heartbeat", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return nil
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
		return ErrLeaseRevoked
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyBytes))
	return fmt.Errorf("heartbeat failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
}

// WorkerDeps provides dependencies for the queue lease worker.
type WorkerDeps struct {
	ServerURL            string
	GetToken             func() string
	Supervisor           *JobSupervisor
	LeaseStore           LeaseStore
	LeaseClient          LeaseClient // nil uses default HTTP client
	Updating             func() bool
	Concurrency          int
	MediaDir             string
	MinBackoff           time.Duration
	MaxBackoff           time.Duration
	HeartbeatInterval    time.Duration
	MaxHeartbeatFailures int
	// EnablePolling, when true, runs the background HTTP job polling loop (POST /api/v1/jobs/claim).
	// By default this is false, as job dispatch is 100% push-driven over WebSocket START_JOB.
	EnablePolling bool
}

// Worker executes the lease polling worker loop, tracking active leases locally
// and maintaining server heartbeats.
type Worker struct {
	serverURL            string
	getToken             func() string
	supervisor           *JobSupervisor
	leaseStore           LeaseStore
	client               LeaseClient
	updating             func() bool
	concurrency          int
	mediaDir             string
	minBackoff           time.Duration
	maxBackoff           time.Duration
	heartbeatInterval    time.Duration
	maxHeartbeatFailures int
	enablePolling        bool

	capacityChan chan struct{}

	mu         sync.Mutex
	activeJobs map[string]chan struct{} // jobID -> done channel for heartbeat ticker

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewWorker initializes a worker with the given dependencies.
func NewWorker(deps WorkerDeps) *Worker {
	if deps.Supervisor == nil {
		panic("NewWorker: Supervisor is nil")
	}
	concurrency := deps.Concurrency
	if concurrency <= 0 {
		concurrency = 1
	}
	minBackoff := deps.MinBackoff
	if minBackoff <= 0 {
		minBackoff = 2 * time.Second
	}
	maxBackoff := deps.MaxBackoff
	if maxBackoff <= 0 {
		maxBackoff = 30 * time.Second
	}
	hbInterval := deps.HeartbeatInterval
	if hbInterval <= 0 {
		hbInterval = 30 * time.Second
	}
	maxHbFailures := deps.MaxHeartbeatFailures
	if maxHbFailures <= 0 {
		maxHbFailures = 3
	}

	client := deps.LeaseClient
	if client == nil {
		client = newHTTPLeaseClient(deps.ServerURL)
	}

	return &Worker{
		serverURL:            deps.ServerURL,
		getToken:             deps.GetToken,
		supervisor:           deps.Supervisor,
		leaseStore:           deps.LeaseStore,
		client:               client,
		updating:             deps.Updating,
		concurrency:          concurrency,
		mediaDir:             deps.MediaDir,
		minBackoff:           minBackoff,
		maxBackoff:           maxBackoff,
		heartbeatInterval:    hbInterval,
		maxHeartbeatFailures: maxHbFailures,
		enablePolling:        deps.EnablePolling,
		capacityChan:         make(chan struct{}, 1),
		activeJobs:           make(map[string]chan struct{}),
	}
}

// Start initiates the worker lifecycle (cleaning stale leases).
// If EnablePolling is true, it launches the background claim poll loop.
func (w *Worker) Start(parentCtx context.Context) {
	w.ctx, w.cancel = context.WithCancel(parentCtx)

	// Clean up stale or expired local leases from previous crashed runs
	if w.leaseStore != nil {
		if leases, err := w.leaseStore.ListActiveJobLeases(w.ctx); err == nil {
			for _, l := range leases {
				if l.LeaseExpiresAt.Valid && time.Now().After(l.LeaseExpiresAt.Time) {
					_ = w.leaseStore.DeleteJobLease(w.ctx, l.JobID)
				}
			}
		}
	}

	if w.enablePolling {
		w.wg.Add(1)
		go w.pollLoop()
	}
}

// Stop cleanly cancels the worker loop and waits for all routines to unwind.
func (w *Worker) Stop() {
	if w.cancel != nil {
		w.cancel()
	}
	w.wg.Wait()
}

func (w *Worker) pollLoop() {
	defer w.wg.Done()

	backoff := w.minBackoff
	for {
		select {
		case <-w.ctx.Done():
			return
		default:
		}

		// 1. If an update is in progress, sleep and retry
		if w.updating != nil && w.updating() {
			select {
			case <-w.ctx.Done():
				return
			case <-time.After(backoff):
				continue
			}
		}

		// 2. Concurrency check: pause while capacity is exhausted
		if w.supervisor.ActiveCount() >= int64(w.concurrency) {
			select {
			case <-w.ctx.Done():
				return
			case <-w.capacityChan:
				backoff = w.minBackoff
			case <-time.After(5 * time.Second):
			}
			continue
		}

		// 3. Ensure auth token is available
		var token string
		if w.getToken != nil {
			token = w.getToken()
		}
		if token == "" {
			select {
			case <-w.ctx.Done():
				return
			case <-time.After(backoff):
				continue
			}
		}

		// 3.5. Quiet hours check: do not claim work if quiet hours are currently active
		if sched := w.supervisor.GetSchedule(); sched != nil && sched.IsQuietHoursActive(time.Now()) {
			select {
			case <-w.ctx.Done():
				return
			case <-time.After(backoff):
				continue
			}
		}

		// 4. Query central lease endpoint
		claimed, err := w.client.ClaimJob(w.ctx, token)
		if err != nil {
			if errors.Is(err, ErrNoWork) {
				// Exponential backoff when no work is available
				select {
				case <-w.ctx.Done():
					return
				case <-time.After(backoff):
					backoff *= 2
					if backoff > w.maxBackoff {
						backoff = w.maxBackoff
					}
					continue
				}
			}

			log.Printf("worker: claim job failed: %v", err)
			// Transient error
			select {
			case <-w.ctx.Done():
				return
			case <-time.After(backoff):
				backoff *= 2
				if backoff > w.maxBackoff {
					backoff = w.maxBackoff
				}
				continue
			}
		}

		// Job claimed successfully; reset backoff
		backoff = w.minBackoff
		if claimed.ConcurrentTranscodeLimit > 0 {
			w.setConcurrency(int(claimed.ConcurrentTranscodeLimit))
		}
		w.launchJob(claimed)
	}
}

// MaxConcurrentTranscodes caps the advertised concurrency: an accidental
// huge value in settings must not spawn an unbounded number of encoders.
const MaxConcurrentTranscodes = 8
const maxConcurrentTranscodes = MaxConcurrentTranscodes

// setConcurrency updates the worker's job concurrency. Called from the
// poll loop only, so the unsynchronized read at the capacity gate is safe.
func (w *Worker) setConcurrency(n int) {
	if n < 1 {
		n = 1
	}
	if n > maxConcurrentTranscodes {
		n = maxConcurrentTranscodes
	}
	if n != w.concurrency {
		log.Printf("worker: concurrency set to %d (from server)", n)
		w.concurrency = n
		if w.supervisor != nil {
			w.supervisor.SetConcurrency(n)
		}
	}
}

// ValidateJobFile statelessly validates that the target file is accessible on disk
// using os.Stat(filepath.Join(local_path, relative_path)) without consulting local SQLite.
// If localPath is specified and does not exist or is not a directory, ErrLibraryNotMounted is returned.
// If the target file does not exist, is a symlink, or is unreadable, ErrFileNotFound is returned.
// relative_path without a local_path anchor, and a bare relative input_path,
// are refused outright: either would resolve against the process working
// directory, which no job path may do.
func ValidateJobFile(localPath, relativePath, inputPath string) (string, error) {
	localPath = CleanLocalPath(localPath)
	if relativePath != "" {
		rel := strings.ReplaceAll(relativePath, "\\", "/")
		cleanRel := path.Clean(rel)
		if cleanRel == "." || cleanRel == ".." || strings.HasPrefix(cleanRel, "../") || strings.HasPrefix(cleanRel, "/") || cleanRel == "" {
			return "", ErrFileNotFound
		}
		nativeRel := filepath.FromSlash(cleanRel)
		cleanNative := filepath.Clean(nativeRel)
		if !filepath.IsLocal(cleanNative) || filepath.IsAbs(cleanNative) || cleanNative == "." || cleanNative == ".." || strings.HasPrefix(cleanNative, ".."+string(filepath.Separator)) {
			return "", ErrFileNotFound
		}
		if localPath == "" {
			return "", ErrLibraryNotMounted
		}
	}

	if localPath != "" {
		if ok, _ := ValidateLocalLibraryDirectory(localPath); !ok {
			return "", ErrLibraryNotMounted
		}
	}

	targetPath := ""
	if localPath != "" && relativePath != "" {
		rel := strings.ReplaceAll(relativePath, "\\", "/")
		cleanRel := path.Clean(rel)
		targetPath = filepath.Join(localPath, filepath.FromSlash(cleanRel))
	} else if localPath != "" && inputPath != "" {
		cleanInput := filepath.Clean(filepath.FromSlash(strings.ReplaceAll(inputPath, "\\", "/")))
		if filepath.IsLocal(cleanInput) && !filepath.IsAbs(cleanInput) {
			targetPath = filepath.Join(localPath, cleanInput)
		} else {
			targetPath = inputPath
		}
	} else if inputPath != "" {
		// Bare inputPath only reaches here with no local/relative anchor; a
		// relative one would resolve against the process working directory.
		if !filepath.IsAbs(inputPath) {
			return "", ErrFileNotFound
		}
		targetPath = inputPath
	}

	if targetPath == "" {
		return "", ErrFileNotFound
	}

	lst, err := os.Lstat(targetPath)
	if err != nil {
		return "", ErrFileNotFound
	}
	if lst.Mode()&os.ModeSymlink != 0 || lst.IsDir() {
		return "", ErrFileNotFound
	}

	canonical, err := transcoder.CanonicalMediaPath(targetPath)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrFileNotFound, err)
	}

	if localPath != "" {
		confined, err := isPathConfined(localPath, canonical)
		if err != nil || !confined {
			return "", ErrFileNotFound
		}
	}

	return canonical, nil
}

// GetMediaDir returns the worker's current media directory root.
func (w *Worker) GetMediaDir() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.mediaDir
}

// SetMediaDir updates the worker's current media directory root.
func (w *Worker) SetMediaDir(dir string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.mediaDir = CleanLocalPath(dir)
}

// ValidateJob validates the target file for a claimed job statelessly via os.Stat.
func (w *Worker) ValidateJob(job *ClaimedJob) (string, error) {
	if job == nil {
		return "", ErrFileNotFound
	}
	localPath := CleanLocalPath(job.LocalPath)
	if localPath == "" {
		localPath = CleanLocalPath(w.GetMediaDir())
	}
	return ValidateJobFile(localPath, job.RelativePath, job.InputPath)
}

func (w *Worker) rejectJob(startPayload protocol.StartJobPayload, reason string) {
	if w.supervisor != nil {
		w.supervisor.rejectJob(startPayload, reason)
	} else {
		w.OnJobComplete(startPayload.JobID, false)
	}
}

func (w *Worker) launchJob(job *ClaimedJob) {
	if job == nil {
		return
	}

	// 1. Record active lease state in SQLite before launching
	if w.leaseStore != nil {
		var expiresAt sql.NullTime
		if job.LeaseExpiresAt != nil {
			expiresAt = sql.NullTime{Time: *job.LeaseExpiresAt, Valid: true}
		}
		if err := w.leaseStore.UpsertJobLease(w.ctx, db.UpsertJobLeaseParams{
			JobID:          job.ID,
			LeaseStatus:    job.LeaseStatus,
			LeaseExpiresAt: expiresAt,
		}); err != nil {
			log.Printf("worker: failed to record active lease for %s: %v", job.ID, err)
		}
	}

	if job.LocalPath == "" && w.mediaDir != "" {
		job.LocalPath = w.mediaDir
	}
	if job.RelativePath != "" {
		job.RelativePath = strings.ReplaceAll(job.RelativePath, "\\", "/")
	}

	targetCodec := job.TargetCodec
	if targetCodec == "" {
		targetCodec = "hevc"
	}
	startPayload := protocol.StartJobPayload{
		JobID:        job.ID,
		MediaItemID:  job.MediaItemID,
		InputPath:    job.InputPath,
		RelativePath: job.RelativePath,
		LocalPath:    job.LocalPath,
		TargetCodec:  targetCodec,
	}
	if len(job.ResolvedPayload) > 0 && string(job.ResolvedPayload) != "{}" && string(job.ResolvedPayload) != "null" {
		var resolved protocol.StartJobPayload
		if err := json.Unmarshal(job.ResolvedPayload, &resolved); err == nil {
			startPayload = resolved
			if startPayload.JobID == "" {
				startPayload.JobID = job.ID
			}
			if startPayload.MediaItemID == "" {
				startPayload.MediaItemID = job.MediaItemID
			}
			if startPayload.InputPath == "" {
				startPayload.InputPath = job.InputPath
			}
			if startPayload.RelativePath == "" {
				startPayload.RelativePath = job.RelativePath
			}
			if startPayload.LocalPath == "" {
				startPayload.LocalPath = job.LocalPath
			}
			if startPayload.TargetCodec == "" {
				startPayload.TargetCodec = targetCodec
			}
		} else {
			log.Printf("worker: failed to unmarshal resolved payload for job %s: %v", job.ID, err)
		}
	}

	// 2. Statelessly validate the resolved job input path via
	// os.Stat(filepath.Join(local_path, relative_path)). Scanned-library
	// membership is enforced once, by JobSupervisor.StartJob, on every path
	// branch.
	if job.LocalPath != "" || job.RelativePath != "" {
		validPath, err := w.ValidateJob(job)
		if err != nil {
			log.Printf("worker: job %s rejected: %v", job.ID, err)
			if errors.Is(err, ErrLibraryNotMounted) {
				w.rejectJob(startPayload, ErrLibraryNotMounted.Error())
			} else {
				w.rejectJob(startPayload, ErrFileNotFound.Error())
			}
			return
		}
		startPayload.InputPath = validPath
	}
	rawPayload, _ := json.Marshal(startPayload)

	doneChan := make(chan struct{})
	w.mu.Lock()
	w.activeJobs[job.ID] = doneChan
	w.mu.Unlock()

	// Forward leased job parameters into JobSupervisor
	if err := w.supervisor.StartJob(w.ctx, startPayload, rawPayload); err != nil {
		log.Printf("worker: job %s rejected by supervisor: %v", job.ID, err)
		// OnJobComplete will be called by supervisor.rejectJob, clearing the lease
		return
	}

	// Start background heartbeat ticker
	w.wg.Add(1)
	go w.heartbeatLoop(job.ID, doneChan)
}

func (w *Worker) heartbeatLoop(jobID string, doneChan <-chan struct{}) {
	defer w.wg.Done()
	ticker := time.NewTicker(w.heartbeatInterval)
	defer ticker.Stop()

	revokedFailures := 0
	for {
		select {
		case <-w.ctx.Done():
			return
		case <-doneChan:
			return
		case <-ticker.C:
			var token string
			if w.getToken != nil {
				token = w.getToken()
			}
			if token == "" {
				continue
			}

			err := w.client.HeartbeatJob(w.ctx, token, jobID)
			if err != nil {
				if errors.Is(err, ErrLeaseRevoked) {
					revokedFailures++
					log.Printf("worker: heartbeat rejected for job %s (lease revoked/expired) [%d/%d]", jobID, revokedFailures, w.maxHeartbeatFailures)
					if revokedFailures >= w.maxHeartbeatFailures {
						log.Printf("worker: lease for job %s revoked repeatedly, terminating local job cleanly", jobID)
						w.supervisor.CancelJob(context.Background(), jobID)
						return
					}
				} else {
					log.Printf("worker: heartbeat error for job %s: %v", jobID, err)
				}
			} else {
				revokedFailures = 0
			}
		}
	}
}

// OnJobComplete handles cleanup upon terminal job outcome.
func (w *Worker) OnJobComplete(jobID string, success bool) {
	w.mu.Lock()
	if doneChan, ok := w.activeJobs[jobID]; ok {
		close(doneChan)
		delete(w.activeJobs, jobID)
	}
	w.mu.Unlock()

	// Clear local lease record in SQLite
	if w.leaseStore != nil {
		if err := w.leaseStore.DeleteJobLease(context.Background(), jobID); err != nil {
			log.Printf("worker: failed to clear local lease record for %s: %v", jobID, err)
		}
	}

	// Wake up polling loop with new capacity
	select {
	case w.capacityChan <- struct{}{}:
	default:
	}
}
