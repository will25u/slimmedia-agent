// Package daemon exposes the SlimMedia agent engine as an importable
// library with a lifecycle API: resolve a configuration, pair, start,
// observe events, query status, and shut down gracefully. The Linux CLI
// `run` command is a thin wrapper over this package; the Windows tray app
// (ADR-009) embeds the same engine.
package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/will25u/slimmedia-agent/internal/db"
	"github.com/will25u/slimmedia-agent/internal/ffmpeg"
	"github.com/will25u/slimmedia-agent/internal/hardware"
	"github.com/will25u/slimmedia-agent/internal/safety"
	"github.com/will25u/slimmedia-agent/internal/transcoder"
	"github.com/will25u/slimmedia-agent/internal/updater"
	"github.com/will25u/slimmedia-agent/internal/ws"
	"github.com/will25u/slimmedia-agent/internal/models"
	"github.com/will25u/slimmedia-agent/internal/protocol"
)

// maxResponseBodyBytes caps how much of an HTTP error response body the agent
// will buffer into memory.
const maxResponseBodyBytes = 1 << 20 // 1 MiB

// defaultDrainTimeout bounds how long Shutdown waits for in-flight transcode
// jobs to finish before giving up (matching the historic CLI behavior).
const defaultDrainTimeout = 10 * time.Second

// eventBuffer bounds the events channel; a consumer that falls behind drops
// events rather than stalling the daemon.
const eventBuffer = 256

// EventType identifies what a daemon Event reports.
type EventType string

const (
	// EventConnection reports WebSocket connection transitions.
	EventConnection EventType = "connection"
	// EventStatus reports agent status changes (IDLE/WORKING/PAUSED/SCANNING).
	EventStatus EventType = "status"
	// EventJobProgress reports transcode progress for an active job.
	EventJobProgress EventType = "job_progress"
	// EventJobComplete reports a terminal job outcome (success or failure),
	// including early rejections such as confinement violations.
	EventJobComplete EventType = "job_complete"
	// EventError reports operational errors (rejected jobs, scan failures).
	EventError EventType = "error"
	// EventUpdateAvailable reports that the server offered a newer agent
	// version via AGENT_UPDATE.
	EventUpdateAvailable EventType = "update_available"
	// EventUpdateConsentRequested reports that user consent is being sought
	// before applying an update. When a consent handler is registered (the
	// Windows tray app, #79) the update blocks on the user's decision; with
	// no handler (CLI), the dashboard's update push is both the request and
	// the grant.
	EventUpdateConsentRequested EventType = "update_consent_requested"
	// EventUpdateConsentGranted reports that consent was given and the update
	// is proceeding.
	EventUpdateConsentGranted EventType = "update_consent_granted"
	// EventUpdateConsentDenied reports that the user declined the update; the
	// flow aborts before anything is downloaded.
	EventUpdateConsentDenied EventType = "update_consent_denied"
	// EventUpdateApplied reports that a verified update replaced the running
	// binary and the agent restarted into it.
	EventUpdateApplied EventType = "update_applied"
	// EventUpdateRolledBack reports that an update failed after swapping and
	// the previous binary was restored.
	EventUpdateRolledBack EventType = "update_rolled_back"
	// EventUnpaired reports that the agent was deleted by the server and unpaired.
	EventUnpaired EventType = "unpaired"
)

// Event is a single telemetry item on the daemon's event stream. The fields
// that are populated depend on Type:
//
//	EventConnection:  Connected, Detail
//	EventStatus:      Status, Activity
//	EventJobProgress: JobID, InputPath, ProgressPct, FPS, Speed, Phase
//	EventJobComplete: JobID, InputPath, Success, Cancelled, Message, OriginalSize, NewSize, SavingsBytes
//	EventError:       JobID (optional), Message, Detail
//	Update lifecycle: Message carries the version, Detail the outcome.
type Event struct {
	Type      EventType
	Time      time.Time
	Connected bool
	Status    string
	Activity  string
	JobID     string
	InputPath string
	Phase     string

	ProgressPct float64
	FPS         float64
	Speed       float64

	SubPhase         string
	TransferredBytes int64
	TotalBytes       int64

	Success bool
	// Cancelled marks a terminal job outcome caused by CANCEL_JOB, lease
	// revocation, or shutdown (Success is false). Cancellation is a normal,
	// user-visible outcome rather than an error: consumers must not surface
	// it as a failure (e.g. the tray's red error line).
	Cancelled    bool
	Message      string
	Detail       string
	OriginalSize int64
	NewSize      int64
	SavingsBytes int64
}

// State is the daemon lifecycle state.
type State string

const (
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateStopped  State = "stopped"
	StateUnpaired State = "UNPAIRED"
)

// Status is a point-in-time snapshot of the daemon.
type Status struct {
	State           State
	Connected       bool
	AgentStatus     string
	Activity        string
	ActiveJobs      int
	UpdateScheduled bool
	Version         string
	Server          string
	Name            string
}

// Daemon is the running agent engine. Create it with Start; observe it via
// Status and Events; stop it with Shutdown.
type Daemon struct {
	cfg *ResolvedConfig

	db       *sql.DB
	wsClient *ws.Client

	daemonCtx    context.Context
	daemonCancel context.CancelFunc

	events chan Event
	// eventsMu serializes emission against the channel close in Shutdown:
	// emit holds the read lock while sending, Shutdown takes the write lock
	// around close(d.events), so no send can race the close.
	eventsMu sync.RWMutex
	// done is closed at the beginning of Shutdown so producers stop
	// emitting before the events channel itself is closed.
	done chan struct{}

	mu       sync.Mutex
	scanMu   sync.Mutex
	state    State
	mountErr string

	jobsMu sync.RWMutex
	// jobs owns the lifecycle of every in-flight transcode job (cancel
	// funcs, pause flags, run goroutines, progress telemetry); the daemon
	// delegates START_JOB/PAUSE_JOB/RESUME_JOB/CANCEL_JOB to it.
	jobs *JobSupervisor

	// updater owns the server-dispatched self-update flow (AGENT_UPDATE /
	// CANCEL_AGENT_UPDATE), including consent and cancellation state; the
	// daemon delegates update commands to it.
	updater *Updater

	// worker executes the queue lease polling, local persistence, and heartbeat loop.
	worker *Worker

	shutdownOnce sync.Once
	shutdownErr  error
}

func (d *Daemon) getJobs() *JobSupervisor {
	d.jobsMu.RLock()
	defer d.jobsMu.RUnlock()
	return d.jobs
}

func (d *Daemon) setJobs(j *JobSupervisor) {
	d.jobsMu.Lock()
	defer d.jobsMu.Unlock()
	d.jobs = j
}

// Start launches the agent engine against the resolved configuration and
// returns once the daemon is running (connected or reconnecting). It fails
// when no API key is present or local state (database, websocket client)
// cannot be initialized.
func Start(ctx context.Context, cfg *ResolvedConfig) (*Daemon, error) {
	if cfg == nil {
		return nil, fmt.Errorf("no configuration provided")
	}
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("no API key provided. Run with --pair <code> or --api-key <key>, or set SLIMMEDIA_API_KEY")
	}
	if cfg.MediaDir == "" {
		return nil, fmt.Errorf("media directory is required: specify via --media-dir, SLIMMEDIA_MEDIA_DIR, or saved config")
	}

	log.Println("Starting SlimMedia Agent Daemon...")
	agentDB, err := db.InitDB(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("Failed to initialize database: %w", err)
	}
	log.Println("Database initialized in WAL mode. Standing by.")

	daemonCtx, daemonCancel := context.WithCancel(ctx)

	d := &Daemon{
		cfg:          cfg,
		db:           agentDB,
		daemonCtx:    daemonCtx,
		daemonCancel: daemonCancel,
		events:       make(chan Event, eventBuffer),
		done:         make(chan struct{}),
		state:        StateStarting,
	}
	// From here on, failure paths must release the database.
	fail := func(err error) (*Daemon, error) {
		daemonCancel()
		agentDB.Close()
		return nil, err
	}

	// Server/WSURL arrive already normalized and policy-checked by
	// ResolveConfig (TLS by default; plaintext only to loopback or with
	// Insecure).

	// Check agent version against server on startup
	checkVersion(cfg.Server)

	// Report the update supply-chain posture once: signature verification is
	// always enforced, so a build still carrying the all-zero dev sentinel
	// refuses every update until a real release key is injected.
	if updater.EmbeddedKeyConfigured() {
		slog.Info("Agent update release key configured (Ed25519); signature verification is enforced")
	} else {
		slog.Warn("Agent update release key is the all-zero dev sentinel (or invalid): signature verification is enforced, so agent updates will be refused. Release builds inject the real key via -ldflags -X")
	}

	// Report a pending Windows self-update outcome (applied / rolled back),
	// if the swap helper left a marker for us.
	d.emitUpdateResult()

	wsClient, err := ws.NewClient(ws.Config{
		APIKey:    cfg.APIKey,
		APIURL:    cfg.Server,
		WSURL:     cfg.WSURL,
		AgentName: cfg.Name,
		Version:   Version,
		Insecure:  cfg.Insecure,
		DB:        agentDB,
		OnConnectionChange: func(connected bool, detail string) {
			d.emit(Event{Type: EventConnection, Connected: connected, Detail: detail})
		},
		OnAgentDeleted: func() {
			d.handleAgentDeleted()
		},
		OnSetLibraryPath: func(p protocol.SetLibraryPathPayload) {
			_ = d.SetLibraryPath(p.LibraryID, p.LocalPath)
		},
		OnCircuitBreakerTripped: func() {
			d.emit(Event{
				Type:    EventError,
				Message: "session usurped by another agent instance",
				Detail:  "another active agent instance is using this agent token/ID",
			})
			d.setStatus("PAUSED", "session usurped")
		},
	})
	if err != nil {
		return fail(fmt.Errorf("Failed to configure websocket client: %w", err))
	}
	d.wsClient = wsClient

	// The updater receives commands through the dispatcher installed below.
	// Construct it (and register the configured consent handler) before the
	// dispatcher is installed and the websocket client starts, so an update
	// can never race ahead of the consent gate.
	d.updater = NewUpdater(UpdaterDeps{
		Send:         wsClient.Send,
		SetScheduled: wsClient.SetUpdateScheduled,
		APIURL:       cfg.Server,
		ActiveJobs: func() int64 {
			if j := d.getJobs(); j != nil {
				return j.ActiveCount()
			}
			return 0
		},
		Emit: d.emit,
	})
	if cfg.UpdateConsentHandler != nil {
		d.updater.SetConsentHandler(cfg.UpdateConsentHandler)
	}

	dataDir := cfg.DataDir
	ffmpegPath := ffmpeg.ResolveBinaryPath(dataDir, "ffmpeg")

	// Detect media servers (Plex/Jellyfin/Emby) once at startup. The
	// stream-safety check only consults servers that are configured or
	// reachable, so a machine without Plex running doesn't fail every job.
	mediaPresence := safety.ProbeMediaServerPresence()
	log.Printf("Media server detection: Plex reachable=%v configured=%v, Jellyfin reachable=%v configured=%v, Emby reachable=%v configured=%v",
		mediaPresence.Plex.Reachable, mediaPresence.Plex.Configured,
		mediaPresence.Jellyfin.Reachable, mediaPresence.Jellyfin.Configured,
		mediaPresence.Emby.Reachable, mediaPresence.Emby.Configured)

	scratchDir := cfg.TempDir
	if scratchDir == "" {
		scratchDir = filepath.Join(dataDir, "scratch")
	}
	quarantineDir := filepath.Join(dataDir, "quarantine")

	// Media disk stats feed both the quarantine budget and the startup
	// capabilities report, so probe once and reuse.
	diskInfo := hardware.ProbeDisk(cfg.MediaDir)

	// Default quarantine budget: 10% of free space up to 200GB
	var maxQuarantine uint64 = 200 * 1024 * 1024 * 1024
	if len(diskInfo) > 0 {
		tenPercent := uint64(diskInfo[0].FreeGB * 1024 * 1024 * 1024 * 0.1)
		if tenPercent < maxQuarantine {
			maxQuarantine = tenPercent
		}
	}
	pipeline := transcoder.NewPipeline(scratchDir, quarantineDir, maxQuarantine)

	queries := db.New(agentDB)

	sm := transcoder.NewStateMachine(func(job *transcoder.Job, oldState, newState models.JobState) {
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
	orchestrator := transcoder.NewOrchestrator(sm, pipeline, ffmpegPath)
	orchestrator.Library = dbLibrarySet{db: agentDB, mediaDir: cfg.MediaDir}
	orchestrator.Queries = queries
	// Persist the ffmpeg command trace at phase-start time so the exact argv
	// survives jobs that never reach FILE_COMPLETE (crash, cancel, failed
	// encode). Fire-and-forget like progress telemetry: no ack, no offline
	// queue — a gap only loses the mid-flight optimization, since a
	// completing job reports the full trace with FILE_COMPLETE anyway.
	orchestrator.OnTrace = func(jobID string, trace []protocol.FfmpegCommandRecord) {
		scrubber := protocol.PathScrubber{
			MediaDir:      cfg.MediaDir,
			ScratchDir:    scratchDir,
			QuarantineDir: quarantineDir,
		}
		scrubbed := protocol.ScrubFfmpegCommands(trace, scrubber)
		b, err := json.Marshal(protocol.JobTracePayload{JobID: jobID, Commands: scrubbed})
		if err != nil {
			return
		}
		wsClient.Send(protocol.Envelope{
			Type:    protocol.TypeJobTrace,
			ID:      fmt.Sprintf("trace-%s-%d", jobID, time.Now().UnixNano()),
			TS:      time.Now(),
			Payload: b,
		})
	}
	hwCaps := hardware.Probe()
	orchestrator.Caps = ffmpeg.ProbeEncoders(ffmpegPath)
	orchestrator.Caps.StripUnverified(hwCaps.GPU.HasNvidia, hwCaps.GPU.HasIntel, hwCaps.GPU.HasAMD)
	orchestrator.CPUTopo = hwCaps.CPU.Topology
	if t := hwCaps.CPU.Topology; t.Hybrid {
		log.Printf("cpu topology: hybrid Intel (%d P-cores, %d E-cores); software encodes steer to P-cores",
			t.PerformanceCores, t.EfficiencyCores)
	} else if t.CCDs > 1 {
		log.Printf("cpu topology: %d CCDs; libx265 encodes partition pools per die", t.CCDs)
	} else {
		log.Printf("cpu topology: symmetric; no encode steering")
	}

	// Report hardware/encoder capabilities before connecting so the dashboard
	// knows what this agent can encode (e.g. AV1 hardware) from the first ping
	// (issue #196). The upload is best-effort and runs off the startup path.
	report := BuildCapabilitiesReport(hwCaps, orchestrator.Caps, mediaPresence)
	if len(diskInfo) > 0 {
		report.Hardware.MediaDisk.Mount = diskInfo[0].Mount
		report.Hardware.MediaDisk.TotalGB = diskInfo[0].TotalGB
		report.Hardware.MediaDisk.FreeGB = diskInfo[0].FreeGB
	}
	go d.postAuditReport(report)

	// The stream-safety check only consults servers that are configured or
	// reachable, so a machine without Plex running doesn't fail every job.
	mediaChecker := safety.NewMediaServerChecker()
	mediaChecker.Presence = &mediaPresence
	mediaChecker.Logf = log.Printf

	var worker *Worker
	jobsSup := NewJobSupervisor(SupervisorDeps{
		Runner:        orchestrator,
		Journal:       queries,
		Safety:        mediaChecker,
		Confinement:   scannedLibraryConfinement{db: agentDB, mediaDir: cfg.MediaDir},
		Updating:      d.updater.Updating,
		ScratchDir:    scratchDir,
		QuarantineDir: quarantineDir,
		MediaDir:      cfg.MediaDir,
		Emit:          d.emit,
		Send:          wsClient.Send,
		SetStatus:     d.setStatus,
		OnJobComplete: func(jobID string, success bool) {
			if worker != nil {
				worker.OnJobComplete(jobID, success)
			}
		},
	})
	d.setJobs(jobsSup)

	worker = NewWorker(WorkerDeps{
		ServerURL:   cfg.Server,
		GetToken:    wsClient.Token,
		Supervisor:  jobsSup,
		LeaseStore:  queries,
		Updating:    d.updater.Updating,
		Concurrency: 1,
		MediaDir:    cfg.MediaDir,
	})
	d.worker = worker
	worker.Start(d.daemonCtx)

	handler := d.commandHandler()
	d.wsClient.SetCommandHandler(handler)

	for _, p := range orchestrator.RecoverJobs(context.Background()) {
		d.wsClient.Send(protocol.Envelope{
			Type:    "JOB_RESUMING",
			Payload: p,
		})
		handler(protocol.Envelope{
			Type:    protocol.TypeStartJob,
			Payload: p,
		})
	}

	d.wsClient.Start()

	d.mu.Lock()
	d.state = StateRunning
	d.mu.Unlock()

	// On startup, scan all configured media directories
	dirsToScan := make([]LibraryMapping, 0)
	if len(cfg.Libraries) > 0 {
		dirsToScan = append(dirsToScan, cfg.Libraries...)
	} else if cfg.MediaDir != "" {
		dirsToScan = append(dirsToScan, LibraryMapping{
			LibraryID: cfg.LibraryID,
			LocalPath: cfg.MediaDir,
		})
	}
	for _, lib := range dirsToScan {
		targetPath := lib.LocalPath
		targetLibID := lib.LibraryID
		if targetPath != "" {
			if fi, err := os.Stat(targetPath); err == nil && fi.IsDir() {
				go d.syncMediaLibrary(targetPath, targetLibID)
			}
		}
	}

	log.Printf("Agent %q running. Connected/connecting to %s", cfg.Name, cfg.WSURL)
	return d, nil
}

func (d *Daemon) sendEnvelope(env protocol.Envelope) {
	if d.wsClient != nil {
		if env.ID == "" {
			env.ID = uuid.New().String()
		}
		if env.TS.IsZero() {
			env.TS = time.Now()
		}
		d.wsClient.Send(env)
	}
}

// SetLibraryPath sanitizes and validates the incoming local media mount path,
// reporting validation success or failure back to the SaaS control plane.
// On success, it persists configuration, propagates the path to worker and supervisor,
// and initiates a media sync. On failure, it leaves the library unmounted with a descriptive
// error so future jobs fail fast.
func (d *Daemon) SetLibraryPath(libraryID, localPath string) error {
	rawPath := localPath
	cleanPath := CleanLocalPath(localPath)

	ok, reason := ValidateLocalLibraryDirectory(cleanPath)
	if !ok {
		log.Printf("WARNING: SET_LIBRARY_PATH validation failed for library %s (%q): %s", libraryID, rawPath, reason)

		// Emit structured WebSocket error event to control plane
		statusPayload, _ := json.Marshal(protocol.LibraryPathStatusPayload{
			LibraryID: libraryID,
			Path:      rawPath,
			Status:    "error",
			Valid:     false,
			Reason:    reason,
			Error:     reason,
		})
		d.sendEnvelope(protocol.Envelope{
			Type:    protocol.TypeLibraryPathError,
			ID:      uuid.New().String(),
			TS:      time.Now(),
			Payload: statusPayload,
		})

		d.emit(Event{
			Type:    EventError,
			Message: fmt.Sprintf("Library path validation failed: %s", reason),
			Detail:  rawPath,
		})

		d.mu.Lock()
		d.mountErr = reason
		d.mu.Unlock()

		if d.worker != nil {
			d.worker.SetMediaDir("")
		}
		if jobs := d.getJobs(); jobs != nil {
			jobs.SetMediaDir("")
			jobs.SetMountError(reason)
		}

		return fmt.Errorf("library path validation failed: %s", reason)
	}

	d.mu.Lock()
	d.mountErr = ""
	if d.cfg != nil {
		d.cfg.LibraryID = libraryID
		d.cfg.MediaDir = cleanPath

		found := false
		for i, lm := range d.cfg.Libraries {
			if lm.LibraryID == libraryID {
				d.cfg.Libraries[i].LocalPath = cleanPath
				found = true
				break
			}
		}
		if !found {
			d.cfg.Libraries = append(d.cfg.Libraries, LibraryMapping{
				LibraryID: libraryID,
				LocalPath: cleanPath,
			})
		}

		configPath := d.cfg.ConfigPath
		apiKey := d.cfg.APIKey
		server := d.cfg.Server
		name := d.cfg.Name
		tempDir := d.cfg.TempDir
		libsCopy := append([]LibraryMapping(nil), d.cfg.Libraries...)
		d.mu.Unlock()

		if configPath != "" {
			localCfg, err := LoadConfig(configPath)
			if err != nil {
				localCfg = &LocalConfig{
					APIKey:  apiKey,
					Server:  server,
					Name:    name,
					TempDir: tempDir,
				}
			}
			localCfg.LibraryID = libraryID
			localCfg.MediaDir = cleanPath
			localCfg.Libraries = libsCopy
			if err := SaveConfig(configPath, localCfg); err != nil {
				log.Printf("Failed to persist updated library config: %v", err)
			}
		}
	} else {
		d.mu.Unlock()
	}

	if d.worker != nil {
		d.worker.SetMediaDir(cleanPath)
	}
	if jobs := d.getJobs(); jobs != nil {
		jobs.SetMediaDir(cleanPath)
		jobs.SetMountError("")
	}

	log.Printf("Applied SET_LIBRARY_PATH: library_id=%s local_path=%s", libraryID, cleanPath)

	// Emit structured WebSocket success event to control plane
	statusPayload, _ := json.Marshal(protocol.LibraryPathStatusPayload{
		LibraryID: libraryID,
		Path:      cleanPath,
		Status:    "mounted",
		Valid:     true,
	})
	d.sendEnvelope(protocol.Envelope{
		Type:    protocol.TypeLibraryPathValid,
		ID:      uuid.New().String(),
		TS:      time.Now(),
		Payload: statusPayload,
	})

	if cleanPath != "" {
		go d.syncMediaLibrary(cleanPath, libraryID)
	}
	return nil
}

// commandHandler builds the inbound command dispatcher: it decodes each
// envelope once and routes the typed command to the JobSupervisor or the
// updater. RecoverJobs replay and live commands share it.
func (d *Daemon) commandHandler() func(protocol.Envelope) {
	dsp := &commandDispatcher{
		ctx:     d.daemonCtx,
		jobs:    func() jobCommands { return d.getJobs() },
		updater: d.updater,
		scanner: func(path, libID string) {
			targetDir := path
			targetLib := libID
			if targetDir == "" && d.cfg != nil {
				d.mu.Lock()
				targetDir = d.cfg.MediaDir
				if targetLib == "" {
					targetLib = d.cfg.LibraryID
				}
				d.mu.Unlock()
			}
			d.syncMediaLibrary(targetDir, targetLib)
		},
		setLibraryPath: func(p protocol.SetLibraryPathPayload) {
			_ = d.SetLibraryPath(p.LibraryID, p.LocalPath)
		},
		emit: d.emit,
	}
	return dsp.handle
}

// emit delivers an event to the event stream. Events are dropped (with a
// log line) when the buffer is full or the daemon is shutting down, so
// producers never block and never race Shutdown's channel close.
func (d *Daemon) emit(ev Event) {
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	d.eventsMu.RLock()
	defer d.eventsMu.RUnlock()
	select {
	case <-d.done:
		return
	default:
	}
	select {
	case d.events <- ev:
	case <-d.done:
	default:
		log.Printf("dropping daemon event %s: event buffer full", ev.Type)
	}
}

// setStatus updates the agent status reported to the server and mirrors the
// change onto the event stream.
func (d *Daemon) setStatus(status, activity string) {
	d.wsClient.SetStatusWithActivity(status, activity)
	d.emit(Event{Type: EventStatus, Status: status, Activity: activity})
}

var exitFunc = os.Exit

func (d *Daemon) handleAgentDeleted() {
	if d.wsClient != nil {
		d.wsClient.Stop()
	}

	if d.cfg != nil && d.cfg.ConfigPath != "" {
		if err := os.Remove(d.cfg.ConfigPath); err != nil && !os.IsNotExist(err) {
			log.Printf("Failed to remove config file %s: %v", d.cfg.ConfigPath, err)
		}
	}

	d.mu.Lock()
	d.state = StateUnpaired
	d.mu.Unlock()

	d.emit(Event{
		Type:   EventUnpaired,
		Status: string(StateUnpaired),
		Detail: "Agent deleted by server",
	})
	d.emit(Event{
		Type:   EventStatus,
		Status: string(StateUnpaired),
		Detail: "Agent deleted by server",
	})

	if d.cfg == nil || !d.cfg.UIAttached {
		log.Println("FATAL: Agent deleted by server. Run with --pair to re-register.")
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = d.Shutdown(ctx)
			exitFunc(0)
		}()
	}
}

// Status returns a point-in-time snapshot of the daemon.
func (d *Daemon) Status() Status {
	activeJobs := 0
	if j := d.getJobs(); j != nil {
		activeJobs = int(j.ActiveCount())
	}
	d.mu.Lock()
	state := d.state
	server := ""
	name := ""
	if d.cfg != nil {
		server = d.cfg.Server
		name = d.cfg.Name
	}
	d.mu.Unlock()
	status, activity := d.wsClient.GetStatusWithActivity()
	if state == StateUnpaired {
		status = string(StateUnpaired)
	}
	return Status{
		State:           state,
		Connected:       d.wsClient.IsConnected(),
		AgentStatus:     status,
		Activity:        activity,
		ActiveJobs:      activeJobs,
		UpdateScheduled: d.wsClient.GetUpdateScheduled(),
		Version:         Version,
		Server:          server,
		Name:            name,
	}
}

// SetUpdateConsentHandler registers the callback that decides whether a
// server-pushed AGENT_UPDATE may proceed; it delegates to the updater, which
// owns the consent gate. See Updater.SetConsentHandler for the contract.
//
// It must be called after Start (the updater is constructed during Start);
// to register consent for a daemon that has not started yet, pass
// ResolvedConfig.UpdateConsentHandler instead.
func (d *Daemon) SetUpdateConsentHandler(h func(version string) bool) {
	if d.updater == nil {
		panic("SetUpdateConsentHandler: daemon not started; pass UpdateConsentHandler in the config instead")
	}
	d.updater.SetConsentHandler(h)
}

// Events returns the daemon's event stream. The channel is closed when
// Shutdown completes.
func (d *Daemon) Events() <-chan Event {
	return d.events
}

// Shutdown drains active transcode jobs (bounded by the drain timeout),
// stops the websocket client, and closes the event stream. When the drain
// deadline expires or the caller's ctx is cancelled first, the remaining
// in-flight job contexts are cancelled so jobs wind down through their
// normal failure path, and a non-nil error describing the incomplete drain
// is returned (nil on a clean drain). It is safe to call multiple times;
// every call returns the outcome of the first.
func (d *Daemon) Shutdown(ctx context.Context) error {
	d.shutdownOnce.Do(func() {
		slog.Info("Shutdown requested, draining active transcode pipeline...")
		d.daemonCancel()

		drainDone := make(chan struct{})
		go func() {
			if j := d.getJobs(); j != nil {
				j.Wait()
			}
			close(drainDone)
		}()

		select {
		case <-drainDone:
			slog.Info("All active transcode jobs drained cleanly")
		case <-time.After(defaultDrainTimeout):
			d.shutdownErr = fmt.Errorf("timed out after %s waiting for transcode pipeline to drain", defaultDrainTimeout)
		case <-ctx.Done():
			d.shutdownErr = fmt.Errorf("shutdown context cancelled while draining transcode pipeline: %w", ctx.Err())
		}
		if d.shutdownErr != nil {
			slog.Warn("Drain incomplete; cancelling in-flight job contexts", "error", d.shutdownErr)
			if j := d.getJobs(); j != nil {
				j.CancelAll()
			}
		}

		d.wsClient.Stop()
		if d.worker != nil {
			d.worker.Stop()
		}

		// Producers must stop before the events channel closes; events
		// emitted during the drain above remain readable in the buffer.
		close(d.done)

		d.mu.Lock()
		d.state = StateStopped
		d.mu.Unlock()

		d.db.Close()

		d.eventsMu.Lock()
		close(d.events)
		d.eventsMu.Unlock()
		log.Println("Shutting down SlimMedia Agent Daemon complete.")
	})
	return d.shutdownErr
}
