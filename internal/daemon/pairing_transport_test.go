package daemon

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/will25u/slimmedia-agent/internal/transport"
	"github.com/will25u/slimmedia-agent/internal/logging"
)

// setupPairingTestServer starts a TLS test server for handler and points the
// pairing HTTP client at it for the duration of the test, so each pairing
// test doesn't repeat the client-swap boilerplate.
func setupPairingTestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	ts := httptest.NewTLSServer(handler)
	originalClient := registerHTTPClient
	registerHTTPClient = ts.Client()
	t.Cleanup(func() {
		ts.Close()
		registerHTTPClient = originalClient
	})
	return ts
}

// TestPairAgent_OverTLS exercises the pairing code path against a real TLS
// server and then greps the produced log file: neither the pairing code nor
// the API key the server returned may appear in it, and the log file must be
// group-readable-only (0640).
func TestPairAgent_OverTLS(t *testing.T) {
	const (
		pairingCode = "PAIR-CODE-7f3a-SECRET-9d21"
		returnedKey = "slm_returned_api_key_Zz9x"
	)

	ts := setupPairingTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/agents/register" {
			http.NotFound(w, r)
			return
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if body["code"] != pairingCode {
			http.Error(w, "bad code", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"api_key":"` + returnedKey + `","agent_id":"agent-1","name":"test"}`))
	}))

	logDir := filepath.Join(t.TempDir(), "logs")
	_, closer, err := logging.SetupLogger("slimmedia-agent", logDir)
	if err != nil {
		t.Fatalf("SetupLogger failed: %v", err)
	}
	defer closer.Close()

	stateDir := t.TempDir()
	configPath := filepath.Join(stateDir, "config.json")
	customTempDir := filepath.Join(stateDir, "transcode-temp")

	key, err := Pair(ts.URL, pairingCode, "test-agent", configPath, stateDir, customTempDir)
	if err != nil {
		t.Fatalf("pairAgent failed: %v", err)
	}
	if key != returnedKey {
		t.Fatalf("pairAgent key = %q, want %q", key, returnedKey)
	}

	// The saved config legitimately contains the key and temp dir; the log must not.
	savedCfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if savedCfg.APIKey != returnedKey {
		t.Fatalf("expected config.json to persist the returned API key")
	}
	if savedCfg.TempDir != customTempDir {
		t.Fatalf("expected config.json to persist temp_dir %q, got %q", customTempDir, savedCfg.TempDir)
	}

	// --- grep the produced log file ---
	logPath := filepath.Join(logDir, "slimmedia-agent.log")
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if strings.Contains(string(logData), pairingCode) {
		t.Fatal("pairing code leaked into the agent log")
	}
	if strings.Contains(string(logData), returnedKey) {
		t.Fatal("API key leaked into the agent log")
	}

	// --- log file mode ---
	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat log file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0640 {
		t.Fatalf("log file mode = %o, want 0640", perm)
	}
}

// TestPairConfigSaveFailureObservable pins that a pairing the server accepted
// but that cannot be persisted surfaces an error wrapping
// ErrConfigNotPersisted (with the key still returned) instead of a silent
// "success" that loses the pairing on restart.
func TestPairConfigSaveFailureObservable(t *testing.T) {
	const returnedKey = "slm_returned_api_key_savefail"

	ts := setupPairingTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"api_key":"` + returnedKey + `","agent_id":"agent-1","name":"test"}`))
	}))

	// A directory at the config path makes WriteFile fail even for root.
	configPath := filepath.Join(t.TempDir(), "config-dir")
	if err := os.Mkdir(configPath, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	key, err := Pair(ts.URL, "PAIR-CODE", "test-agent", configPath, t.TempDir(), "")
	if err == nil {
		t.Fatal("Pair succeeded despite config save failure")
	}
	if !errors.Is(err, ErrConfigNotPersisted) {
		t.Fatalf("error = %v, want wrap of ErrConfigNotPersisted", err)
	}
	if key != returnedKey {
		t.Fatalf("key = %q, want %q returned alongside the error", key, returnedKey)
	}
}

// TestPairSuccessPathBodyCapped pins that the pairing success decode is
// bounded like the error path: an oversized body fails cleanly instead of
// being buffered without limit.
func TestPairSuccessPathBodyCapped(t *testing.T) {
	ts := setupPairingTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"api_key":"` + strings.Repeat("x", maxResponseBodyBytes+1) + `"}`))
	}))

	configPath := filepath.Join(t.TempDir(), "config.json")
	if _, err := Pair(ts.URL, "PAIR-CODE", "test-agent", configPath, t.TempDir(), ""); err == nil {
		t.Fatal("Pair accepted an oversized success body")
	}
	if _, statErr := os.Stat(configPath); !os.IsNotExist(statErr) {
		t.Fatalf("config written despite oversized response (stat err = %v)", statErr)
	}
}

// TestPlaintextNonLoopbackRefusedBeforeTraffic documents the ordering
// guarantee for the run flow: the transport policy refusal happens in
// ResolveServerURLs — before pairAgent/registerAgent/fetchToken ever run —
// so a pairing code or API key never leaves the machine for a plaintext
// non-loopback endpoint without --insecure.
func TestPlaintextNonLoopbackRefusedBeforeTraffic(t *testing.T) {
	for _, raw := range []string{"http://media.example.com", "ws://192.168.1.10:8082"} {
		apiURL, wsURL, err := transport.ResolveServerURLs(raw, false)
		if err == nil {
			t.Fatalf("plaintext non-loopback URL %s accepted without --insecure (resolved to %q / %q)", raw, apiURL, wsURL)
		}
		if !errors.Is(err, transport.ErrPlaintextTransport) {
			t.Fatalf("error for %s = %v, want ErrPlaintextTransport", raw, err)
		}
	}
	// With the explicit opt-in the same endpoints resolve.
	apiURL, wsURL, err := transport.ResolveServerURLs("http://media.example.com", true)
	if err != nil || apiURL != "http://media.example.com" || wsURL != "ws://media.example.com" {
		t.Fatalf("insecure opt-in resolution = (%q, %q, %v)", apiURL, wsURL, err)
	}
}
