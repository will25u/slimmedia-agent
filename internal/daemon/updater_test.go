package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/will25u/slimmedia-agent/internal/protocol"
)

// fakeUpdateTransport stands in for the websocket client: it records the
// status envelopes the updater sends and the scheduled-flag transitions it
// mirrors, so the update flow is testable without a websocket server.
type fakeUpdateTransport struct {
	mu         sync.Mutex
	envelopes  []protocol.Envelope
	lastSched  bool
	sawSchedOn bool
}

func (f *fakeUpdateTransport) send(env protocol.Envelope) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.envelopes = append(f.envelopes, env)
}

func (f *fakeUpdateTransport) setScheduled(scheduled bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastSched = scheduled
	if scheduled {
		f.sawSchedOn = true
	}
}

func (f *fakeUpdateTransport) statuses() []protocol.AgentUpdateStatusPayload {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []protocol.AgentUpdateStatusPayload
	for _, env := range f.envelopes {
		if env.Type != protocol.TypeAgentUpdateStatus {
			continue
		}
		var p protocol.AgentUpdateStatusPayload
		if err := json.Unmarshal(env.Payload, &p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

func waitForUpdating(t *testing.T, u *Updater, want bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if u.Updating() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("Updating() did not become %v within 5s", want)
}

// TestUpdaterStartUpdateReportsThroughFakeTransport: the full dashboard-push
// flow runs through the updater's interface against a fake transport — no
// websocket server — and reports downloading/installing/failed statuses
// while mirroring the scheduled flag on and back off.
func TestUpdaterStartUpdateReportsThroughFakeTransport(t *testing.T) {
	// Serve a binary whose bytes do not match the advertised checksum; the
	// flow aborts at the checksum comparison, before any binary swap.
	sum := sha256.Sum256([]byte("something else"))
	claimed := hex.EncodeToString(sum[:])
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("fake new agent binary"))
	}))
	defer ts.Close()

	ft := &fakeUpdateTransport{}
	u := NewUpdater(UpdaterDeps{
		Send:         ft.send,
		SetScheduled: ft.setScheduled,
		APIURL:       ts.URL,
		ActiveJobs:   func() int64 { return 0 },
	})

	waitForUpdating(t, u, false)
	u.StartUpdate(protocol.AgentUpdatePayload{
		Version:      "999.0.0",
		DownloadPath: "/api/v1/agents/download",
		Checksum:     claimed,
	})
	waitForUpdating(t, u, false)

	statuses := ft.statuses()
	want := []string{protocol.UpdateStatusDownloading, protocol.UpdateStatusInstalling, protocol.UpdateStatusFailed}
	if len(statuses) != len(want) {
		t.Fatalf("statuses = %v, want %v", statusPayloads(statuses), want)
	}
	for i, status := range want {
		if statuses[i].Status != status {
			t.Fatalf("statuses = %v, want %v", statusPayloads(statuses), want)
		}
		if statuses[i].Version != "999.0.0" {
			t.Fatalf("status %d version = %q, want payload version", i, statuses[i].Version)
		}
	}

	if !ft.sawSchedOn {
		t.Fatal("scheduled flag never set while update was in flight")
	}
	if ft.lastSched {
		t.Fatal("scheduled flag still set after update flow finished")
	}
}

// TestUpdaterDuplicateStartRefused: a second AGENT_UPDATE while one is
// blocked in the consent gate is refused — consent is asked exactly once —
// and CANCEL_AGENT_UPDATE unwinds the in-flight flow.
func TestUpdaterDuplicateStartRefused(t *testing.T) {
	ft := &fakeUpdateTransport{}
	consentStarted := make(chan struct{})
	var asked int
	var askedMu sync.Mutex

	u := NewUpdater(UpdaterDeps{
		Send:         ft.send,
		SetScheduled: ft.setScheduled,
		APIURL:       "http://localhost:8082",
		ActiveJobs:   func() int64 { return 0 },
	})
	u.SetConsentHandler(func(string) bool {
		askedMu.Lock()
		asked++
		askedMu.Unlock()
		close(consentStarted)
		select {} // user walks away; never answers
	})

	u.StartUpdate(protocol.AgentUpdatePayload{Version: "999.0.0"})
	<-consentStarted
	waitForUpdating(t, u, true)

	// Duplicate while the first is still awaiting consent: refused by the
	// isUpdating gate without entering the flow again.
	u.StartUpdate(protocol.AgentUpdatePayload{Version: "999.0.0"})

	askedMu.Lock()
	if asked != 1 {
		askedMu.Unlock()
		t.Fatalf("consent asked %d times, want 1 (duplicate start entered the flow)", asked)
	}
	askedMu.Unlock()

	u.CancelUpdate()
	waitForUpdating(t, u, false)

	askedMu.Lock()
	defer askedMu.Unlock()
	if asked != 1 {
		t.Fatalf("consent asked %d times after cancel, want still 1", asked)
	}
}

func statusPayloads(statuses []protocol.AgentUpdateStatusPayload) []string {
	out := make([]string, len(statuses))
	for i, s := range statuses {
		out[i] = s.Status
	}
	return out
}
