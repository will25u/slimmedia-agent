package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/will25u/slimmedia-agent/internal/transport"
	"github.com/will25u/slimmedia-agent/internal/protocol"
)

func TestRunUpdateRefusesPlaintextNonLoopback(t *testing.T) {
	// RunUpdate falls back to configuredAgentAPIKey() (env, then cwd config);
	// clear the env so the refusal below is exercised against a bare server
	// flag regardless of the developer's shell.
	t.Setenv("SLIMMEDIA_API_KEY", "")

	// The refusal happens in transport.ResolveServerURLs before any request
	// is built, so no test server is needed — and insecure must stay false,
	// or the call would proceed to a real network dial.
	err := RunUpdate("http://media.example.com", "", false, false)
	if err == nil {
		t.Fatal("RunUpdate accepted a plaintext non-loopback server without --insecure")
	}
	if !errors.Is(err, transport.ErrPlaintextTransport) {
		t.Fatalf("error = %v, want ErrPlaintextTransport", err)
	}
	if !strings.HasPrefix(err.Error(), "invalid --server URL:") {
		t.Fatalf("error = %q, want prefix 'invalid --server URL:'", err.Error())
	}
}

func TestRunUpdateAlreadyCurrent(t *testing.T) {
	var downloadHit atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/version":
			json.NewEncoder(w).Encode(map[string]string{
				"latest_agent_version": Version,
				"version":              Version,
			})
		case agentDownloadPath, agentChecksumPath:
			downloadHit.Store(true)
			http.Error(w, "should not be requested", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	if err := RunUpdate(ts.URL, "", true, false); err != nil {
		t.Fatalf("runUpdate returned error: %v", err)
	}
	if downloadHit.Load() {
		t.Fatal("download endpoints were hit even though the agent is already current")
	}
}

func TestRunUpdateFlowsToVerifiedDownload(t *testing.T) {
	// Serve a binary whose bytes do not match the advertised checksum; the
	// update must abort at the checksum comparison, before the staged file is
	// ever renamed over the running executable.
	payload := []byte("fake new agent binary")
	claimed := sha256.Sum256([]byte("something else"))
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/version":
			json.NewEncoder(w).Encode(map[string]string{
				"latest_agent_version": "999.0.0",
				"version":              "999.0.0",
			})
		case agentChecksumPath:
			json.NewEncoder(w).Encode(map[string]string{"sha256": hex.EncodeToString(claimed[:])})
		case agentDownloadPath:
			w.Write(payload)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	err := RunUpdate(ts.URL, "", true, false)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected checksum mismatch abort, got %v", err)
	}
}

func TestRunUpdateForceReachesDownload(t *testing.T) {
	// With --force the version comparison is skipped: an equal server version
	// must still proceed to the checksum/download step.
	payload := []byte("fake new agent binary")
	claimed := sha256.Sum256([]byte("something else"))
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/version":
			json.NewEncoder(w).Encode(map[string]string{
				"latest_agent_version": Version,
				"version":              Version,
			})
		case agentChecksumPath:
			json.NewEncoder(w).Encode(map[string]string{"sha256": hex.EncodeToString(claimed[:])})
		case agentDownloadPath:
			w.Write(payload)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	err := RunUpdate(ts.URL, "", true, true)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected forced update to reach verified download, got %v", err)
	}
}

func TestFetchAgentChecksum(t *testing.T) {
	sum := sha256.Sum256([]byte("binary bytes"))
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != agentChecksumPath {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"sha256":"`+strings.ToUpper(hex.EncodeToString(sum[:]))+`"}`)
	}))
	defer ts.Close()

	got, _, err := fetchAgentChecksum(ts.URL, "")
	if err != nil {
		t.Fatalf("fetchAgentChecksum returned error: %v", err)
	}
	if want := hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("fetchAgentChecksum = %q, want %q", got, want)
	}
}

func TestFetchAgentChecksumOversizedBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sha256":"` + strings.Repeat("a", maxResponseBodyBytes+1)))
	}))
	defer ts.Close()

	_, _, err := fetchAgentChecksum(ts.URL, "")
	if err == nil {
		t.Fatal("expected decode failure on oversized truncated body")
	}
}

func TestUpdaterCancellationWhileWaiting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var activeJobs atomic.Int64
	activeJobs.Store(1)

	u := newTestUpdater("http://localhost:8082", activeJobs.Load, func(Event) {})

	done := make(chan struct{})
	go func() {
		u.run(ctx, protocol.AgentUpdatePayload{
			Version: "999.0.0",
		})
		close(done)
	}()

	// Verify it's blocked while active jobs > 0
	time.Sleep(50 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("updater run exited before cancel while active jobs > 0")
	default:
	}

	// Trigger cancellation
	cancel()

	select {
	case <-done:
		// Successfully unblocked on cancellation
	case <-time.After(2 * time.Second):
		t.Fatal("updater run did not unblock on context cancellation")
	}
}

func TestHandleAgentUpdate_PassesConfiguredAPIKey(t *testing.T) {
	t.Setenv("SLIMMEDIA_API_KEY", "sm_daemon_test_key_123")

	var gotKey, gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-API-Key")
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("fake binary"))
	}))
	defer ts.Close()

	var activeJobs atomic.Int64
	payload := protocol.AgentUpdatePayload{
		Version:      "999.0.0",
		DownloadPath: "/api/v1/agents/download",
		Checksum:     "0000000000000000000000000000000000000000000000000000000000000000",
	}

	// Mismatched checksum stops execution before renaming executable or re-execing.
	u := newTestUpdater(ts.URL, activeJobs.Load, func(Event) {})
	u.run(context.Background(), payload)

	if gotKey != "sm_daemon_test_key_123" {
		t.Fatalf("expected X-API-Key 'sm_daemon_test_key_123', got %q", gotKey)
	}
	if gotAuth != "Bearer sm_daemon_test_key_123" {
		t.Fatalf("expected Authorization 'Bearer sm_daemon_test_key_123', got %q", gotAuth)
	}
}

func TestHandleAgentUpdate_PassesConfiguredAPIKeyFromConfig(t *testing.T) {
	t.Setenv("SLIMMEDIA_API_KEY", "")
	tmpDir := t.TempDir()
	// The saved config resolves under the per-user state dir (never
	// cwd-relative); point the state override at tmpDir and run from an
	// unrelated working directory to prove cwd-independence.
	t.Setenv("SLIMMEDIA_STATE_DIR", tmpDir)
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}
	cfg := &LocalConfig{APIKey: "sm_config_file_key_456"}
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(tmpDir, "config.json"), data, 0600); err != nil {
		t.Fatalf("write config failed: %v", err)
	}

	origWd, _ := os.Getwd()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("chdir failed: %v", err)
	}
	defer os.Chdir(origWd)

	var gotKey, gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-API-Key")
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("fake binary"))
	}))
	defer ts.Close()

	var activeJobs atomic.Int64
	payload := protocol.AgentUpdatePayload{
		Version:      "999.0.0",
		DownloadPath: "/api/v1/agents/download",
		Checksum:     "0000000000000000000000000000000000000000000000000000000000000000",
	}

	u := newTestUpdater(ts.URL, activeJobs.Load, func(Event) {})
	u.run(context.Background(), payload)

	if gotKey != "sm_config_file_key_456" {
		t.Fatalf("expected X-API-Key 'sm_config_file_key_456', got %q", gotKey)
	}
	if gotAuth != "Bearer sm_config_file_key_456" {
		t.Fatalf("expected Authorization 'Bearer sm_config_file_key_456', got %q", gotAuth)
	}
}
