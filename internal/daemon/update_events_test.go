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

	"github.com/will25u/slimmedia-agent/internal/updater"
	"github.com/will25u/slimmedia-agent/internal/protocol"
)

// TestUpdaterEmitsLifecycleEvents proves the dashboard-pushed flow surfaces
// update lifecycle events on the daemon events channel: the offer and the
// consent grant (the dashboard push is the consenting action, so no local
// consent request fires), and — when installation succeeds — the applied
// event. This test aborts at the checksum comparison, before any binary
// replacement.
func TestUpdaterEmitsLifecycleEvents(t *testing.T) {
	payload := []byte("fake new agent binary")
	claimed := sha256.Sum256([]byte("something else"))
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer ts.Close()

	var events []Event
	emit := func(ev Event) { events = append(events, ev) }

	var activeJobs atomic.Int64
	u := newTestUpdater(ts.URL, activeJobs.Load, emit)
	u.run(context.Background(), protocol.AgentUpdatePayload{
		Version:      "999.0.0",
		DownloadPath: "/api/v1/agents/download",
		Checksum:     hex.EncodeToString(claimed[:]),
		Auto:         true,
	})

	want := []EventType{EventUpdateAvailable, EventUpdateConsentGranted}
	if len(events) != len(want) {
		t.Fatalf("events = %v, want %v", eventTypes(events), want)
	}
	for i, typ := range want {
		if events[i].Type != typ {
			t.Fatalf("events = %v, want %v", eventTypes(events), want)
		}
		if events[i].Message != "999.0.0" {
			t.Fatalf("event %s Message = %q, want version", typ, events[i].Message)
		}
	}
	for _, ev := range events {
		if ev.Type == EventUpdateApplied || ev.Type == EventUpdateRolledBack {
			t.Fatalf("terminal event %s emitted despite failed install", ev.Type)
		}
	}
}

// TestUpdaterIgnoredVersionEmitsNothing: an AGENT_UPDATE that is not newer
// than the running version is a no-op and must not raise consent or
// lifecycle noise.
func TestUpdaterIgnoredVersionEmitsNothing(t *testing.T) {
	var events []Event
	var activeJobs atomic.Int64
	u := newTestUpdater("http://localhost:8082", activeJobs.Load, func(ev Event) { events = append(events, ev) })
	u.run(context.Background(), protocol.AgentUpdatePayload{
		Version: Version,
	})

	if len(events) != 0 {
		t.Fatalf("events = %v, want none", eventTypes(events))
	}
}

func eventTypes(events []Event) []EventType {
	types := make([]EventType, len(events))
	for i, ev := range events {
		types[i] = ev.Type
	}
	return types
}

// TestEmitUpdateResult_* cover the startup side of the Windows flow: the
// swap helper's marker becomes exactly one applied/rolled-back event.
func TestEmitUpdateResult_Applied(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("SLIMMEDIA_STATE_DIR", stateDir)

	if err := updater.WriteUpdateResult(stateDir, updater.UpdateResult{
		Status:      updater.UpdateResultApplied,
		FromVersion: "1.4.0",
		ToVersion:   "1.5.0",
		Time:        time.Now(),
	}); err != nil {
		t.Fatalf("WriteUpdateResult: %v", err)
	}

	d := &Daemon{events: make(chan Event, eventBuffer)}
	d.emitUpdateResult()

	select {
	case ev := <-d.events:
		if ev.Type != EventUpdateApplied || ev.Message != "1.5.0" {
			t.Fatalf("event = %+v, want update_applied for 1.5.0", ev)
		}
	default:
		t.Fatal("no event emitted")
	}

	// The marker is consumed; a second startup reports nothing.
	d.emitUpdateResult()
	select {
	case ev := <-d.events:
		t.Fatalf("unexpected second event: %+v", ev)
	default:
	}
}

func TestEmitUpdateResult_RolledBack(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("SLIMMEDIA_STATE_DIR", stateDir)

	if err := updater.WriteUpdateResult(stateDir, updater.UpdateResult{
		Status:      updater.UpdateResultRolledBack,
		FromVersion: "1.4.0",
		ToVersion:   "1.5.0",
		Time:        time.Now(),
	}); err != nil {
		t.Fatalf("WriteUpdateResult: %v", err)
	}

	d := &Daemon{events: make(chan Event, eventBuffer)}
	d.emitUpdateResult()

	select {
	case ev := <-d.events:
		if ev.Type != EventUpdateRolledBack || ev.Message != "1.4.0" {
			t.Fatalf("event = %+v, want update_rolled_back to 1.4.0", ev)
		}
	default:
		t.Fatal("no event emitted")
	}
}

func TestEmitUpdateResult_NoMarker(t *testing.T) {
	t.Setenv("SLIMMEDIA_STATE_DIR", t.TempDir())

	d := &Daemon{events: make(chan Event, eventBuffer)}
	d.emitUpdateResult()

	select {
	case ev := <-d.events:
		t.Fatalf("unexpected event with no marker: %+v", ev)
	default:
	}
}
