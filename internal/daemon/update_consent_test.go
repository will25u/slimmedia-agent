package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/will25u/slimmedia-agent/internal/protocol"
)

// newTestUpdater builds an updater with a nil transport (status envelopes
// dropped) and no consent handler, so the update flow is exercised through
// its injected seams without a websocket client or server.
func newTestUpdater(apiURL string, activeJobs func() int64, emit func(Event)) *Updater {
	return NewUpdater(UpdaterDeps{
		APIURL:     apiURL,
		ActiveJobs: activeJobs,
		Emit:       emit,
	})
}

// TestUpdaterConsentGrantedProceeds: when a consent handler is registered
// (the tray app), a granting decision lets the flow continue; the grant
// event records the tray as the consent surface.
func TestUpdaterConsentGrantedProceeds(t *testing.T) {
	payload := []byte("fake new agent binary")
	claimed := sha256.Sum256([]byte("something else"))
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer ts.Close()

	var events []Event
	var activeJobs atomic.Int64
	var asked []string
	consent := func(version string) bool {
		asked = append(asked, version)
		return true
	}

	u := newTestUpdater(ts.URL, activeJobs.Load, func(ev Event) { events = append(events, ev) })
	u.SetConsentHandler(consent)
	u.run(context.Background(), protocol.AgentUpdatePayload{
		Version:      "999.0.0",
		DownloadPath: "/api/v1/agents/download",
		Checksum:     hex.EncodeToString(claimed[:]),
	})

	if len(asked) != 1 || asked[0] != "999.0.0" {
		t.Fatalf("consent asked %v, want once for 999.0.0", asked)
	}
	want := []EventType{EventUpdateAvailable, EventUpdateConsentRequested, EventUpdateConsentGranted}
	if len(events) != len(want) {
		t.Fatalf("events = %v, want %v", eventTypes(events), want)
	}
	for i, typ := range want {
		if events[i].Type != typ {
			t.Fatalf("events = %v, want %v", eventTypes(events), want)
		}
	}
	if events[2].Detail != "consent granted via tray prompt" {
		t.Fatalf("grant detail = %q, want tray consent surface", events[2].Detail)
	}
}

// TestUpdaterAutoSkipsTrayConsent: a payload with Auto set (the operator
// clicked Update on the dashboard) installs without consulting a registered
// consent handler — the dashboard click is the consenting action, and the
// Windows tray must not prompt.
func TestUpdaterAutoSkipsTrayConsent(t *testing.T) {
	payload := []byte("fake new agent binary")
	claimed := sha256.Sum256([]byte("something else"))
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer ts.Close()

	var events []Event
	var activeJobs atomic.Int64
	var asked []string
	u := newTestUpdater(ts.URL, activeJobs.Load, func(ev Event) { events = append(events, ev) })
	u.SetConsentHandler(func(version string) bool {
		asked = append(asked, version)
		return false // even a denying prompt must never run
	})
	u.run(context.Background(), protocol.AgentUpdatePayload{
		Version:      "999.0.0",
		DownloadPath: "/api/v1/agents/download",
		Checksum:     hex.EncodeToString(claimed[:]),
		Auto:         true,
	})

	if len(asked) != 0 {
		t.Fatalf("consent handler consulted %v despite auto payload", asked)
	}
	want := []EventType{EventUpdateAvailable, EventUpdateConsentGranted}
	if len(events) != len(want) {
		t.Fatalf("events = %v, want %v", eventTypes(events), want)
	}
	for i, typ := range want {
		if events[i].Type != typ {
			t.Fatalf("events = %v, want %v", eventTypes(events), want)
		}
	}
	if events[1].Detail != "consent granted via dashboard update push" {
		t.Fatalf("grant detail = %q, want dashboard consent surface", events[1].Detail)
	}
}

// TestUpdaterConsentDeniedAbortsBeforeDownload: a denying decision emits the
// denied event and stops the flow before any download is attempted.
func TestUpdaterConsentDeniedAbortsBeforeDownload(t *testing.T) {
	var downloads atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads.Add(1)
		w.Write([]byte("fake"))
	}))
	defer ts.Close()

	var events []Event
	var activeJobs atomic.Int64
	u := newTestUpdater(ts.URL, activeJobs.Load, func(ev Event) { events = append(events, ev) })
	u.SetConsentHandler(func(string) bool { return false })
	u.run(context.Background(), protocol.AgentUpdatePayload{
		Version:      "999.0.0",
		DownloadPath: "/api/v1/agents/download",
		Checksum:     "deadbeef",
	})

	want := []EventType{EventUpdateAvailable, EventUpdateConsentRequested, EventUpdateConsentDenied}
	if len(events) != len(want) {
		t.Fatalf("events = %v, want %v", eventTypes(events), want)
	}
	for i, typ := range want {
		if events[i].Type != typ {
			t.Fatalf("events = %v, want %v", eventTypes(events), want)
		}
	}
	if downloads.Load() != 0 {
		t.Fatalf("denied update hit the download endpoint %d times", downloads.Load())
	}
}

// TestUpdaterConsentWaitCancelled: CANCEL_AGENT_UPDATE (or shutdown)
// cancelling the update context while the user is still deciding must abort
// the flow instead of parking the update half-pending: the Updating flag
// would otherwise stay set and defer every incoming job.
func TestUpdaterConsentWaitCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	consentStarted := make(chan struct{})
	var events []Event
	var activeJobs atomic.Int64
	u := newTestUpdater("http://localhost:8082", activeJobs.Load, func(ev Event) { events = append(events, ev) })
	u.SetConsentHandler(func(string) bool {
		close(consentStarted)
		select {} // user walks away; never answers
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		u.run(ctx, protocol.AgentUpdatePayload{Version: "999.0.0"})
	}()

	<-consentStarted
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("update flow did not return after cancel during consent wait")
	}

	want := []EventType{EventUpdateAvailable, EventUpdateConsentRequested}
	if len(events) != len(want) {
		t.Fatalf("events = %v, want %v", eventTypes(events), want)
	}
	for i, typ := range want {
		if events[i].Type != typ {
			t.Fatalf("events = %v, want %v", eventTypes(events), want)
		}
	}
}

// TestDaemonSetUpdateConsentHandler: registration and removal through the
// daemon's public delegate are visible to the updater that owns the gate.
func TestDaemonSetUpdateConsentHandler(t *testing.T) {
	d := &Daemon{updater: NewUpdater(UpdaterDeps{ActiveJobs: func() int64 { return 0 }})}
	d.SetUpdateConsentHandler(func(string) bool { return true })
	if h := d.updater.consentHandler(); h == nil {
		t.Fatal("consent handler not visible after registration")
	}
	d.SetUpdateConsentHandler(nil)
	if h := d.updater.consentHandler(); h != nil {
		t.Fatal("consent handler still set after removal")
	}
}
