// Package tray folds the daemon's event stream and status snapshots into a
// render-ready view state for the Windows tray GUI, and generates the tray
// icons at runtime. It is pure Go so the event-folding logic is unit-testable
// off-Windows.
package tray

import (
	"fmt"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/dustin/go-humanize"

	"github.com/will25u/slimmedia-agent/internal/daemon"
)

// maxLogLines caps how many log lines the model retains; older lines are
// dropped once the cap is exceeded.
const maxLogLines = 200

// ConnState is the tray-facing connection state, derived from the daemon's
// connection events and status snapshots.
type ConnState string

const (
	// ConnStarting is the initial state before the first connection outcome.
	ConnStarting ConnState = "starting"
	// ConnConnected means the agent is connected to the server.
	ConnConnected ConnState = "connected"
	// ConnReconnecting means the connection was lost and the agent retries.
	ConnReconnecting ConnState = "reconnecting"
	// ConnOffline means the daemon has stopped.
	ConnOffline ConnState = "offline"
)

// UpdateState is where a server-pushed agent update currently stands.
type UpdateState string

const (
	// UpdateNone means no update activity has been seen.
	UpdateNone UpdateState = ""
	// UpdateAvailable means the server offered a newer agent version.
	UpdateAvailable UpdateState = "available"
	// UpdateAwaitingConsent means the update is blocked on the user's decision.
	UpdateAwaitingConsent UpdateState = "awaiting_consent"
	// UpdateGranted means the user consented and the update is proceeding.
	UpdateGranted UpdateState = "granted"
	// UpdateDenied means the user declined the update.
	UpdateDenied UpdateState = "denied"
	// UpdateApplied means a verified update replaced the running binary.
	UpdateApplied UpdateState = "applied"
	// UpdateRolledBack means the update failed after swapping and the
	// previous binary was restored.
	UpdateRolledBack UpdateState = "rolled_back"
)

// UpdateInfo describes the latest agent-update activity, if any.
type UpdateInfo struct {
	Version string
	State   UpdateState
	Detail  string
}

// JobInfo is the tray-facing view of the most recently observed transcode
// job.
type JobInfo struct {
	ID          string
	InputPath   string
	ProgressPct float64 // clamped to [0,100]
	FPS         float64
	Speed       float64
	Phase            string
	SubPhase         string
	Activity         string
	TransferredBytes int64
	TotalBytes       int64
	Done             bool // terminal: complete (Success) or failed (!Success)
	Success     bool
	// Cancelled marks a terminal outcome caused by CANCEL_JOB, lease
	// revocation, or shutdown. Displayed as "cancelled", not "failed", and
	// never promoted to LastError.
	Cancelled bool
	Message   string // terminal message, e.g. failure reason
}

// LogLine is one entry in the model's rolling log tail. Text carries no
// timestamp; the GUI renders Time and Text together.
type LogLine struct {
	Time time.Time
	Text string
}

// ViewState is a render-ready snapshot of everything the tray GUI displays.
type ViewState struct {
	Version     string
	Name        string
	Server      string
	Conn        ConnState
	ConnDetail  string
	AgentStatus string
	Activity    string
	ActiveJobs  int
	Job         *JobInfo    // nil when no job has been seen since model start
	LastError   string      // most recent error; "" when none
	Update      *UpdateInfo // nil when no update activity
	LogTail     []LogLine   // oldest first, capped at maxLogLines
}

// Model folds daemon events and status snapshots into a ViewState. All
// methods are safe for concurrent use.
type Model struct {
	mu   sync.Mutex
	view ViewState
}

// NewModel returns a Model in the ConnStarting state with the given agent
// version.
func NewModel(version string) *Model {
	return &Model{view: ViewState{Conn: ConnStarting, Version: version}}
}

// ApplyStatus overlays a daemon status snapshot onto the model. A stopped
// daemon forces ConnOffline; otherwise the connection flag maps to
// ConnConnected or ConnReconnecting. The update-scheduled flag is a
// dashboard-side concern and never mutates the update state here.
func (m *Model) ApplyStatus(s daemon.Status) {
	m.mu.Lock()
	defer m.mu.Unlock()

	switch {
	case s.State == daemon.StateStopped || s.State == daemon.StateUnpaired:
		m.view.Conn = ConnOffline
	case s.Connected:
		m.view.Conn = ConnConnected
		// The status snapshot carries no reconnect reason; a stale detail
		// from a previous drop must not outlive the reconnect.
		m.view.ConnDetail = ""
	default:
		m.view.Conn = ConnReconnecting
	}
	m.view.AgentStatus = s.AgentStatus
	m.view.Activity = s.Activity
	m.view.ActiveJobs = s.ActiveJobs
	if s.Name != "" {
		m.view.Name = s.Name
	}
	if s.Server != "" {
		m.view.Server = s.Server
	}
}

// ApplyEvent folds one daemon event into the model and appends the derived
// log line.
func (m *Model) ApplyEvent(ev daemon.Event) {
	m.mu.Lock()
	defer m.mu.Unlock()

	ts := ev.Time
	if ts.IsZero() {
		ts = time.Now()
	}

	var line string
	switch ev.Type {
	case daemon.EventConnection:
		if ev.Connected {
			m.view.Conn = ConnConnected
		} else {
			m.view.Conn = ConnReconnecting
		}
		m.view.ConnDetail = ev.Detail
		detail := ev.Detail
		if detail == "" {
			if ev.Connected {
				detail = "connected"
			} else {
				detail = "disconnected"
			}
		}
		line = "connection: " + detail

	case daemon.EventStatus:
		m.view.AgentStatus = ev.Status
		m.view.Activity = ev.Activity
		line = "status: " + ev.Status
		if ev.Activity != "" {
			line += " " + ev.Activity
		}

	case daemon.EventJobProgress:
		m.view.Job = &JobInfo{
			ID:               ev.JobID,
			InputPath:        ev.InputPath,
			ProgressPct:      clampPct(ev.ProgressPct),
			FPS:              ev.FPS,
			Speed:            ev.Speed,
			Phase:            ev.Phase,
			SubPhase:         ev.SubPhase,
			Activity:         ev.Activity,
			TransferredBytes: ev.TransferredBytes,
			TotalBytes:       ev.TotalBytes,
		}
		if ev.Phase == "verifying" {
			line = "job " + filepath.Base(ev.InputPath) + ": verifying"
		} else if ev.Phase == "replacing" || ev.Phase == "placement" {
			if ev.Activity != "" && ev.TotalBytes > 0 {
				line = fmt.Sprintf("job %s: %s (%s / %s, %.1f%%)",
					filepath.Base(ev.InputPath),
					ev.Activity,
					humanize.Bytes(uint64(ev.TransferredBytes)),
					humanize.Bytes(uint64(ev.TotalBytes)),
					clampPct(ev.ProgressPct),
				)
			} else if ev.Activity != "" {
				line = "job " + filepath.Base(ev.InputPath) + ": " + ev.Activity
			} else {
				line = "job " + filepath.Base(ev.InputPath) + ": " + formatPct(ev.ProgressPct) + "%"
			}
		} else {
			line = "job " + filepath.Base(ev.InputPath) + ": " + formatPct(ev.ProgressPct) + "%"
		}

	case daemon.EventJobComplete:
		if m.view.Job == nil {
			m.view.Job = &JobInfo{ID: ev.JobID, InputPath: ev.InputPath}
		}
		if m.view.Job.ID == ev.JobID {
			m.view.Job.Done = true
			m.view.Job.Success = ev.Success
			m.view.Job.Cancelled = ev.Cancelled
			m.view.Job.Message = ev.Message
		}
		if ev.Success {
			line = "job " + ev.JobID + " complete: success"
			if ev.SavingsBytes > 0 {
				line += " (saved " + humanize.Bytes(uint64(ev.SavingsBytes)) + ")"
			}
		} else if ev.Cancelled {
			// User/server-initiated stop: a normal outcome, not an error —
			// it must not light up the red error line.
			line = "job " + ev.JobID + " cancelled"
		} else {
			msg := ev.Message
			if msg == "" {
				msg = "job " + ev.JobID + " failed"
			}
			m.view.LastError = msg
			line = "job " + ev.JobID + " failed: " + msg
		}

	case daemon.EventError:
		msg := ev.Message
		if ev.Detail != "" && ev.Detail != msg {
			msg += ": " + ev.Detail
		}
		m.view.LastError = msg
		line = "error: " + ev.Message

	case daemon.EventUpdateAvailable:
		m.view.Update = &UpdateInfo{Version: ev.Message, State: UpdateAvailable, Detail: ev.Detail}
		line = "update: v" + ev.Message + " available"

	case daemon.EventUpdateConsentRequested:
		m.ensureUpdate(ev.Message)
		m.view.Update.State = UpdateAwaitingConsent
		line = "update: consent requested for v" + m.view.Update.Version

	case daemon.EventUpdateConsentGranted:
		m.ensureUpdate(ev.Message)
		m.view.Update.State = UpdateGranted
		m.view.Update.Detail = ev.Detail
		line = "update: consent granted"

	case daemon.EventUpdateConsentDenied:
		m.ensureUpdate(ev.Message)
		m.view.Update.State = UpdateDenied
		m.view.Update.Detail = ev.Detail
		line = "update: declined"

	case daemon.EventUpdateApplied:
		m.ensureUpdate("")
		m.view.Update.State = UpdateApplied
		if ev.Message != "" {
			m.view.Update.Version = ev.Message
		}
		m.view.Update.Detail = ev.Detail
		line = "update: applied v" + m.view.Update.Version

	case daemon.EventUpdateRolledBack:
		m.ensureUpdate("")
		m.view.Update.State = UpdateRolledBack
		m.view.Update.Detail = ev.Detail
		if ev.Message != "" {
			m.view.Update.Version = ev.Message
		}
		line = "update: rolled back (" + ev.Detail + ")"

	case daemon.EventUnpaired:
		m.view.Conn = ConnOffline
		m.view.AgentStatus = "UNPAIRED"
		m.view.LastError = "Agent deleted by server"
		line = "agent deleted by server — unpaired"

	default:
		line = "event: " + string(ev.Type)
	}

	m.appendLog(LogLine{Time: ts, Text: line})
}

// View returns a deep copy of the current view state; mutating the result
// does not affect the model.
func (m *Model) View() ViewState {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := m.view
	if m.view.Job != nil {
		job := *m.view.Job
		out.Job = &job
	}
	if m.view.Update != nil {
		update := *m.view.Update
		out.Update = &update
	}
	out.LogTail = make([]LogLine, len(m.view.LogTail))
	copy(out.LogTail, m.view.LogTail)
	return out
}

// ensureUpdate makes sure Update is non-nil, recording version when the
// update info is created. Callers must hold m.mu.
func (m *Model) ensureUpdate(version string) {
	if m.view.Update == nil {
		m.view.Update = &UpdateInfo{Version: version}
	}
}

// appendLog appends a line to the log tail, dropping the oldest lines past
// the cap. Callers must hold m.mu.
func (m *Model) appendLog(l LogLine) {
	m.view.LogTail = append(m.view.LogTail, l)
	if len(m.view.LogTail) > maxLogLines {
		m.view.LogTail = m.view.LogTail[len(m.view.LogTail)-maxLogLines:]
	}
}

// clampPct clamps a progress percentage into [0,100] without rounding.
func clampPct(p float64) float64 {
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}

// formatPct renders a percentage for log lines without rounding artifacts.
func formatPct(p float64) string {
	return strconv.FormatFloat(clampPct(p), 'f', -1, 64)
}
