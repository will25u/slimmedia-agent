package tray

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dustin/go-humanize"

	"github.com/will25u/slimmedia-agent/internal/daemon"
)

func TestNewModel(t *testing.T) {
	m := NewModel("1.2.3")
	v := m.View()
	if v.Conn != ConnStarting {
		t.Fatalf("Conn = %q, want %q", v.Conn, ConnStarting)
	}
	if v.Version != "1.2.3" {
		t.Fatalf("Version = %q, want 1.2.3", v.Version)
	}
	if v.Job != nil {
		t.Fatalf("Job = %+v, want nil", v.Job)
	}
	if v.Update != nil {
		t.Fatalf("Update = %+v, want nil", v.Update)
	}
	if len(v.LogTail) != 0 {
		t.Fatalf("LogTail = %d lines, want 0", len(v.LogTail))
	}
}

func TestApplyStatus(t *testing.T) {
	tests := []struct {
		name  string
		setup func(m *Model)
		want  func(t *testing.T, v ViewState)
	}{
		{
			name: "stopped forces offline",
			want: func(t *testing.T, v ViewState) {
				if v.Conn != ConnOffline {
					t.Errorf("Conn = %q, want %q", v.Conn, ConnOffline)
				}
			},
		},
		{
			name: "connected",
			want: func(t *testing.T, v ViewState) {
				if v.Conn != ConnConnected {
					t.Errorf("Conn = %q, want %q", v.Conn, ConnConnected)
				}
			},
		},
		{
			name: "disconnected while running means reconnecting",
			want: func(t *testing.T, v ViewState) {
				if v.Conn != ConnReconnecting {
					t.Errorf("Conn = %q, want %q", v.Conn, ConnReconnecting)
				}
			},
		},
		{
			name: "stopped wins over connected flag",
			want: func(t *testing.T, v ViewState) {
				if v.Conn != ConnOffline {
					t.Errorf("Conn = %q, want %q", v.Conn, ConnOffline)
				}
			},
		},
		{
			name: "name and server copied",
			want: func(t *testing.T, v ViewState) {
				if v.Name != "agent-a" || v.Server != "https://server" {
					t.Errorf("Name/Server = %q/%q, want agent-a/https://server", v.Name, v.Server)
				}
			},
		},
		{
			name: "empty name and server do not overwrite",
			setup: func(m *Model) {
				m.ApplyStatus(daemon.Status{State: daemon.StateRunning, Connected: true, Name: "agent-a", Server: "https://server"})
			},
			want: func(t *testing.T, v ViewState) {
				if v.Name != "agent-a" || v.Server != "https://server" {
					t.Errorf("Name/Server = %q/%q, want agent-a/https://server", v.Name, v.Server)
				}
			},
		},
		{
			name: "scheduled flag leaves awaiting consent untouched",
			setup: func(m *Model) {
				m.ApplyEvent(daemon.Event{Type: daemon.EventUpdateAvailable, Message: "2.0.0"})
				m.ApplyEvent(daemon.Event{Type: daemon.EventUpdateConsentRequested, Message: "2.0.0"})
			},
			want: func(t *testing.T, v ViewState) {
				if v.Update == nil || v.Update.State != UpdateAwaitingConsent {
					t.Fatalf("Update = %+v, want awaiting_consent", v.Update)
				}
			},
		},
		{
			name: "scheduled flag does not create update info",
			want: func(t *testing.T, v ViewState) {
				if v.Update != nil {
					t.Errorf("Update = %+v, want nil", v.Update)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := NewModel("1.0.0")
			status := daemon.Status{
				State:           daemon.StateRunning,
				Connected:       true,
				AgentStatus:     "WORKING",
				Activity:        "transcoding",
				ActiveJobs:      2,
				UpdateScheduled: true,
			}
			switch tc.name {
			case "stopped forces offline":
				status.State = daemon.StateStopped
				status.Connected = false
			case "disconnected while running means reconnecting":
				status.Connected = false
			case "stopped wins over connected flag":
				status.State = daemon.StateStopped
			case "name and server copied", "empty name and server do not overwrite":
				// second apply in the empty-overwrite case leaves these unset
			default:
				status.Name = ""
				status.Server = ""
			}
			if tc.name == "name and server copied" {
				status.Name = "agent-a"
				status.Server = "https://server"
			}
			if tc.setup != nil {
				tc.setup(m)
			}
			m.ApplyStatus(status)
			v := m.View()
			if v.AgentStatus != "WORKING" || v.Activity != "transcoding" || v.ActiveJobs != 2 {
				t.Errorf("AgentStatus/Activity/ActiveJobs = %q/%q/%d, want WORKING/transcoding/2",
					v.AgentStatus, v.Activity, v.ActiveJobs)
			}
			tc.want(t, v)
		})
	}
}

func lastLog(v ViewState) string {
	if len(v.LogTail) == 0 {
		return ""
	}
	return v.LogTail[len(v.LogTail)-1].Text
}

func TestApplyEvent(t *testing.T) {
	tests := []struct {
		name  string
		setup func(m *Model)
		event daemon.Event
		check func(t *testing.T, v ViewState)
	}{
		{
			name:  "connection up with detail",
			event: daemon.Event{Type: daemon.EventConnection, Connected: true, Detail: "TLS established"},
			check: func(t *testing.T, v ViewState) {
				if v.Conn != ConnConnected || v.ConnDetail != "TLS established" {
					t.Errorf("Conn/ConnDetail = %q/%q", v.Conn, v.ConnDetail)
				}
				if got := lastLog(v); got != "connection: TLS established" {
					t.Errorf("log = %q", got)
				}
			},
		},
		{
			name:  "connection up without detail",
			event: daemon.Event{Type: daemon.EventConnection, Connected: true},
			check: func(t *testing.T, v ViewState) {
				if got := lastLog(v); got != "connection: connected" {
					t.Errorf("log = %q", got)
				}
			},
		},
		{
			name:  "connection down without detail",
			event: daemon.Event{Type: daemon.EventConnection, Connected: false},
			check: func(t *testing.T, v ViewState) {
				if v.Conn != ConnReconnecting {
					t.Errorf("Conn = %q, want %q", v.Conn, ConnReconnecting)
				}
				if got := lastLog(v); got != "connection: disconnected" {
					t.Errorf("log = %q", got)
				}
			},
		},
		{
			name:  "status with activity",
			event: daemon.Event{Type: daemon.EventStatus, Status: "WORKING", Activity: "transcoding"},
			check: func(t *testing.T, v ViewState) {
				if v.AgentStatus != "WORKING" || v.Activity != "transcoding" {
					t.Errorf("AgentStatus/Activity = %q/%q", v.AgentStatus, v.Activity)
				}
				if got := lastLog(v); got != "status: WORKING transcoding" {
					t.Errorf("log = %q", got)
				}
			},
		},
		{
			name:  "status without activity",
			event: daemon.Event{Type: daemon.EventStatus, Status: "IDLE"},
			check: func(t *testing.T, v ViewState) {
				if got := lastLog(v); got != "status: IDLE" {
					t.Errorf("log = %q", got)
				}
			},
		},
		{
			name:  "job progress creates current job",
			event: daemon.Event{Type: daemon.EventJobProgress, JobID: "abc", InputPath: "/media/movies/foo.mkv", ProgressPct: 42.5, FPS: 61, Speed: 2.5},
			check: func(t *testing.T, v ViewState) {
				if v.Job == nil {
					t.Fatal("Job is nil")
				}
				if v.Job.ID != "abc" || v.Job.InputPath != "/media/movies/foo.mkv" ||
					v.Job.ProgressPct != 42.5 || v.Job.FPS != 61 || v.Job.Speed != 2.5 {
					t.Errorf("Job = %+v", v.Job)
				}
				if v.Job.Done {
					t.Error("Job.Done = true, want false")
				}
				if got := lastLog(v); got != "job foo.mkv: 42.5%" {
					t.Errorf("log = %q", got)
				}
			},
		},
		{
			name:  "job progress with verifying phase",
			event: daemon.Event{Type: daemon.EventJobProgress, JobID: "abc", InputPath: "/media/movies/foo.mkv", Phase: "verifying", ProgressPct: 100},
			check: func(t *testing.T, v ViewState) {
				if v.Job == nil {
					t.Fatal("Job is nil")
				}
				if v.Job.ID != "abc" || v.Job.Phase != "verifying" || v.Job.ProgressPct != 100 {
					t.Errorf("Job = %+v", v.Job)
				}
				if v.Job.Done {
					t.Error("Job.Done = true, want false")
				}
				if got := lastLog(v); got != "job foo.mkv: verifying" {
					t.Errorf("log = %q, want 'job foo.mkv: verifying'", got)
				}
			},
		},
		{
			name: "job progress with placement phase and byte counters",
			event: daemon.Event{
				Type:             daemon.EventJobProgress,
				JobID:            "abc",
				InputPath:        "/media/movies/foo.mkv",
				Phase:            "replacing",
				SubPhase:         "quarantine_backup",
				Activity:         "Backing up original",
				ProgressPct:      42.5,
				TransferredBytes: 4200000000,
				TotalBytes:       9800000000,
			},
			check: func(t *testing.T, v ViewState) {
				if v.Job == nil {
					t.Fatal("Job is nil")
				}
				if v.Job.ID != "abc" || v.Job.Phase != "replacing" || v.Job.SubPhase != "quarantine_backup" ||
					v.Job.Activity != "Backing up original" || v.Job.ProgressPct != 42.5 ||
					v.Job.TransferredBytes != 4200000000 || v.Job.TotalBytes != 9800000000 {
					t.Errorf("Job = %+v", v.Job)
				}
				if got := lastLog(v); got != "job foo.mkv: Backing up original (4.2 GB / 9.8 GB, 42.5%)" {
					t.Errorf("log = %q, want 'job foo.mkv: Backing up original (4.2 GB / 9.8 GB, 42.5%%)'", got)
				}
			},
		},
		{
			name:  "job progress clamps below zero",
			event: daemon.Event{Type: daemon.EventJobProgress, JobID: "abc", InputPath: "/a.mkv", ProgressPct: -3},
			check: func(t *testing.T, v ViewState) {
				if v.Job.ProgressPct != 0 {
					t.Errorf("ProgressPct = %v, want 0", v.Job.ProgressPct)
				}
				if got := lastLog(v); got != "job a.mkv: 0%" {
					t.Errorf("log = %q", got)
				}
			},
		},
		{
			name:  "job progress clamps above 100",
			event: daemon.Event{Type: daemon.EventJobProgress, JobID: "abc", InputPath: "/a.mkv", ProgressPct: 120},
			check: func(t *testing.T, v ViewState) {
				if v.Job.ProgressPct != 100 {
					t.Errorf("ProgressPct = %v, want 100", v.Job.ProgressPct)
				}
				if got := lastLog(v); got != "job a.mkv: 100%" {
					t.Errorf("log = %q", got)
				}
			},
		},
		{
			name: "job progress for a different id replaces",
			setup: func(m *Model) {
				m.ApplyEvent(daemon.Event{Type: daemon.EventJobProgress, JobID: "abc", InputPath: "/a.mkv", ProgressPct: 10})
			},
			event: daemon.Event{Type: daemon.EventJobProgress, JobID: "def", InputPath: "/b.mkv", ProgressPct: 5},
			check: func(t *testing.T, v ViewState) {
				if v.Job.ID != "def" || v.Job.ProgressPct != 5 {
					t.Errorf("Job = %+v, want def at 5%%", v.Job)
				}
			},
		},
		{
			name: "job complete success with savings",
			setup: func(m *Model) {
				m.ApplyEvent(daemon.Event{Type: daemon.EventJobProgress, JobID: "abc", InputPath: "/a.mkv", ProgressPct: 99})
			},
			event: daemon.Event{Type: daemon.EventJobComplete, JobID: "abc", Success: true, SavingsBytes: 1234567890},
			check: func(t *testing.T, v ViewState) {
				if !v.Job.Done || !v.Job.Success {
					t.Errorf("Job = %+v, want done+success", v.Job)
				}
				want := "job abc complete: success (saved " + humanize.Bytes(1234567890) + ")"
				if got := lastLog(v); got != want {
					t.Errorf("log = %q, want %q", got, want)
				}
				if v.LastError != "" {
					t.Errorf("LastError = %q, want empty", v.LastError)
				}
			},
		},
		{
			name: "job complete success without savings",
			setup: func(m *Model) {
				m.ApplyEvent(daemon.Event{Type: daemon.EventJobProgress, JobID: "abc", InputPath: "/a.mkv"})
			},
			event: daemon.Event{Type: daemon.EventJobComplete, JobID: "abc", Success: true},
			check: func(t *testing.T, v ViewState) {
				if got := lastLog(v); got != "job abc complete: success" {
					t.Errorf("log = %q", got)
				}
			},
		},
		{
			name:  "job complete failure without prior progress",
			event: daemon.Event{Type: daemon.EventJobComplete, JobID: "abc", Success: false, Message: "confinement violation"},
			check: func(t *testing.T, v ViewState) {
				if v.Job == nil || !v.Job.Done || v.Job.Success || v.Job.Message != "confinement violation" {
					t.Errorf("Job = %+v", v.Job)
				}
				if v.LastError != "confinement violation" {
					t.Errorf("LastError = %q", v.LastError)
				}
				if got := lastLog(v); got != "job abc failed: confinement violation" {
					t.Errorf("log = %q", got)
				}
			},
		},
		{
			name:  "job complete failure with empty message falls back",
			event: daemon.Event{Type: daemon.EventJobComplete, JobID: "abc", Success: false},
			check: func(t *testing.T, v ViewState) {
				if v.LastError != "job abc failed" {
					t.Errorf("LastError = %q", v.LastError)
				}
				if got := lastLog(v); got != "job abc failed: job abc failed" {
					t.Errorf("log = %q", got)
				}
			},
		},
		{
			name: "job complete cancelled is not an error",
			setup: func(m *Model) {
				m.ApplyEvent(daemon.Event{Type: daemon.EventJobProgress, JobID: "abc", InputPath: "/a.mkv", ProgressPct: 40})
			},
			event: daemon.Event{Type: daemon.EventJobComplete, JobID: "abc", Success: false, Cancelled: true, Message: "job cancelled"},
			check: func(t *testing.T, v ViewState) {
				if v.Job == nil || !v.Job.Done || v.Job.Success || !v.Job.Cancelled {
					t.Errorf("Job = %+v, want done+cancelled", v.Job)
				}
				if v.LastError != "" {
					t.Errorf("LastError = %q, want empty for a cancellation", v.LastError)
				}
				if got := lastLog(v); got != "job abc cancelled" {
					t.Errorf("log = %q", got)
				}
			},
		},
		{
			name: "job complete for other id leaves current job alone",
			setup: func(m *Model) {
				m.ApplyEvent(daemon.Event{Type: daemon.EventJobProgress, JobID: "abc", InputPath: "/a.mkv", ProgressPct: 50})
			},
			event: daemon.Event{Type: daemon.EventJobComplete, JobID: "def", Success: false, Message: "nope"},
			check: func(t *testing.T, v ViewState) {
				if v.Job.ID != "abc" || v.Job.Done {
					t.Errorf("Job = %+v, want untouched abc", v.Job)
				}
				if v.LastError != "nope" {
					t.Errorf("LastError = %q, want nope", v.LastError)
				}
			},
		},
		{
			name:  "error with detail",
			event: daemon.Event{Type: daemon.EventError, Message: "scan failed", Detail: "permission denied"},
			check: func(t *testing.T, v ViewState) {
				if v.LastError != "scan failed: permission denied" {
					t.Errorf("LastError = %q", v.LastError)
				}
				if got := lastLog(v); got != "error: scan failed" {
					t.Errorf("log = %q", got)
				}
			},
		},
		{
			name:  "error with duplicate detail",
			event: daemon.Event{Type: daemon.EventError, Message: "boom", Detail: "boom"},
			check: func(t *testing.T, v ViewState) {
				if v.LastError != "boom" {
					t.Errorf("LastError = %q", v.LastError)
				}
			},
		},
		{
			name:  "unknown event type",
			event: daemon.Event{Type: "mystery"},
			check: func(t *testing.T, v ViewState) {
				if got := lastLog(v); got != "event: mystery" {
					t.Errorf("log = %q", got)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := NewModel("1.0.0")
			if tc.setup != nil {
				tc.setup(m)
			}
			m.ApplyEvent(tc.event)
			tc.check(t, m.View())
		})
	}
}

func TestUpdateLifecycleGranted(t *testing.T) {
	m := NewModel("1.0.0")
	m.ApplyEvent(daemon.Event{Type: daemon.EventUpdateAvailable, Message: "2.0.0", Detail: "security fixes"})
	v := m.View()
	if v.Update == nil || v.Update.Version != "2.0.0" || v.Update.State != UpdateAvailable || v.Update.Detail != "security fixes" {
		t.Fatalf("after available: %+v", v.Update)
	}

	m.ApplyEvent(daemon.Event{Type: daemon.EventUpdateConsentRequested, Message: "2.0.0"})
	v = m.View()
	if v.Update.State != UpdateAwaitingConsent {
		t.Fatalf("after requested: %+v", v.Update)
	}
	if got := lastLog(v); got != "update: consent requested for v2.0.0" {
		t.Errorf("log = %q", got)
	}

	m.ApplyEvent(daemon.Event{Type: daemon.EventUpdateConsentGranted, Detail: "downloading"})
	v = m.View()
	if v.Update.State != UpdateGranted || v.Update.Detail != "downloading" {
		t.Fatalf("after granted: %+v", v.Update)
	}
	if got := lastLog(v); got != "update: consent granted" {
		t.Errorf("log = %q", got)
	}

	m.ApplyEvent(daemon.Event{Type: daemon.EventUpdateApplied, Message: "2.0.1"})
	v = m.View()
	if v.Update.State != UpdateApplied || v.Update.Version != "2.0.1" {
		t.Fatalf("after applied: %+v", v.Update)
	}
	if got := lastLog(v); got != "update: applied v2.0.1" {
		t.Errorf("log = %q", got)
	}
}

func TestUpdateLifecycleDenied(t *testing.T) {
	m := NewModel("1.0.0")
	m.ApplyEvent(daemon.Event{Type: daemon.EventUpdateAvailable, Message: "2.0.0"})
	m.ApplyEvent(daemon.Event{Type: daemon.EventUpdateConsentRequested, Message: "2.0.0"})
	m.ApplyEvent(daemon.Event{Type: daemon.EventUpdateConsentDenied, Detail: "user said no"})

	v := m.View()
	if v.Update.State != UpdateDenied || v.Update.Detail != "user said no" || v.Update.Version != "2.0.0" {
		t.Fatalf("after denied: %+v", v.Update)
	}
	if got := lastLog(v); got != "update: declined" {
		t.Errorf("log = %q", got)
	}
}

func TestUpdateRolledBack(t *testing.T) {
	m := NewModel("1.0.0")
	m.ApplyEvent(daemon.Event{Type: daemon.EventUpdateApplied, Message: "2.0.0"})
	m.ApplyEvent(daemon.Event{Type: daemon.EventUpdateRolledBack, Message: "2.0.1", Detail: "signature check failed"})

	v := m.View()
	if v.Update.State != UpdateRolledBack || v.Update.Detail != "signature check failed" {
		t.Fatalf("after rolled back: %+v", v.Update)
	}
	if got := lastLog(v); got != "update: rolled back (signature check failed)" {
		t.Errorf("log = %q", got)
	}
}

func TestConsentRequestedWithoutPriorAvailable(t *testing.T) {
	m := NewModel("1.0.0")
	m.ApplyEvent(daemon.Event{Type: daemon.EventUpdateConsentRequested, Message: "3.0.0"})
	v := m.View()
	if v.Update == nil || v.Update.Version != "3.0.0" || v.Update.State != UpdateAwaitingConsent {
		t.Fatalf("Update = %+v", v.Update)
	}
}

func TestUpdateAppliedKeepsVersionWhenMessageEmpty(t *testing.T) {
	m := NewModel("1.0.0")
	m.ApplyEvent(daemon.Event{Type: daemon.EventUpdateAvailable, Message: "2.0.0"})
	m.ApplyEvent(daemon.Event{Type: daemon.EventUpdateApplied})
	v := m.View()
	if v.Update.State != UpdateApplied || v.Update.Version != "2.0.0" {
		t.Fatalf("Update = %+v", v.Update)
	}
	if got := lastLog(v); got != "update: applied v2.0.0" {
		t.Errorf("log = %q", got)
	}
}

func TestLogCapEvictsOldest(t *testing.T) {
	m := NewModel("1.0.0")
	for i := 0; i < 250; i++ {
		m.ApplyEvent(daemon.Event{Type: daemon.EventError, Message: fmt.Sprintf("err %d", i)})
	}
	v := m.View()
	if len(v.LogTail) != maxLogLines {
		t.Fatalf("LogTail = %d lines, want %d", len(v.LogTail), maxLogLines)
	}
	if got := v.LogTail[0].Text; got != "error: err 50" {
		t.Errorf("oldest retained = %q, want %q", got, "error: err 50")
	}
	if got := v.LogTail[maxLogLines-1].Text; got != "error: err 249" {
		t.Errorf("newest = %q, want %q", got, "error: err 249")
	}
}

func TestLogLineTimeDefaultsToNow(t *testing.T) {
	m := NewModel("1.0.0")
	before := time.Now()
	m.ApplyEvent(daemon.Event{Type: daemon.EventStatus, Status: "IDLE"})
	v := m.View()
	if len(v.LogTail) != 1 {
		t.Fatalf("LogTail = %d lines, want 1", len(v.LogTail))
	}
	if ts := v.LogTail[0].Time; ts.Before(before) || ts.IsZero() {
		t.Errorf("log Time = %v, want >= %v", ts, before)
	}
	if strings.Contains(v.LogTail[0].Text, "status") && strings.HasPrefix(v.LogTail[0].Text, "20") {
		t.Errorf("log Text should not embed a timestamp: %q", v.LogTail[0].Text)
	}
}

func TestViewDeepCopy(t *testing.T) {
	m := NewModel("1.0.0")
	m.ApplyStatus(daemon.Status{State: daemon.StateRunning, Connected: true, Name: "agent-a"})
	m.ApplyEvent(daemon.Event{Type: daemon.EventJobProgress, JobID: "abc", InputPath: "/a.mkv", ProgressPct: 10})
	m.ApplyEvent(daemon.Event{Type: daemon.EventUpdateAvailable, Message: "2.0.0"})
	for i := 0; i < 5; i++ {
		m.ApplyEvent(daemon.Event{Type: daemon.EventStatus, Status: "IDLE"})
	}

	v := m.View()
	v.Job.ID = "mutated"
	v.Job.ProgressPct = 99
	v.Update.Version = "9.9.9"
	v.Update.State = UpdateApplied
	v.LogTail[0].Text = "mutated"
	v.LogTail = append(v.LogTail, LogLine{Text: "extra"})
	v.AgentStatus = "MUTATED"

	v2 := m.View()
	if v2.Job.ID != "abc" || v2.Job.ProgressPct != 10 {
		t.Errorf("Job mutated through View: %+v", v2.Job)
	}
	if v2.Update.Version != "2.0.0" || v2.Update.State != UpdateAvailable {
		t.Errorf("Update mutated through View: %+v", v2.Update)
	}
	if len(v2.LogTail) != 7 {
		t.Fatalf("LogTail len = %d, want 7", len(v2.LogTail))
	}
	if v2.LogTail[0].Text == "mutated" || v2.LogTail[0].Text == "" {
		t.Errorf("LogTail mutated through View: %q", v2.LogTail[0].Text)
	}
	if v2.AgentStatus != "IDLE" {
		t.Errorf("AgentStatus = %q, want IDLE", v2.AgentStatus)
	}
}

func TestConcurrentUse(t *testing.T) {
	m := NewModel("1.0.0")
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				m.ApplyEvent(daemon.Event{
					Type:        daemon.EventJobProgress,
					JobID:       fmt.Sprintf("job-%d", g),
					InputPath:   fmt.Sprintf("/media/%d.mkv", g),
					ProgressPct: float64(i % 120),
				})
				m.ApplyStatus(daemon.Status{
					State:       daemon.StateRunning,
					Connected:   i%2 == 0,
					AgentStatus: "WORKING",
					ActiveJobs:  1,
				})
				m.ApplyEvent(daemon.Event{Type: daemon.EventStatus, Status: "WORKING"})
				v := m.View()
				if len(v.LogTail) > maxLogLines {
					t.Errorf("LogTail grew past cap: %d", len(v.LogTail))
				}
			}
		}(g)
	}
	wg.Wait()
	v := m.View()
	if len(v.LogTail) > maxLogLines {
		t.Fatalf("LogTail = %d lines, want <= %d", len(v.LogTail), maxLogLines)
	}
	if v.Conn != ConnConnected && v.Conn != ConnReconnecting {
		t.Errorf("Conn = %q", v.Conn)
	}
}

// TestApplyStatus_ClearsStaleConnDetail: a reconnect reason set by an event
// must not survive the reconnect itself — the status snapshot that reports
// Connected carries no detail and clears it.
func TestApplyStatus_ClearsStaleConnDetail(t *testing.T) {
	m := NewModel("1.6.0")
	m.ApplyEvent(daemon.Event{Type: daemon.EventConnection, Connected: false, Detail: "connection reset"})
	if v := m.View(); v.ConnDetail != "connection reset" {
		t.Fatalf("ConnDetail = %q, want reconnect reason", v.ConnDetail)
	}
	m.ApplyStatus(daemon.Status{State: daemon.StateRunning, Connected: true, AgentStatus: "IDLE"})
	if v := m.View(); v.Conn != ConnConnected || v.ConnDetail != "" {
		t.Fatalf("Conn/ConnDetail = %q/%q, want connected with cleared detail", v.Conn, v.ConnDetail)
	}
}
