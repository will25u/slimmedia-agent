package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"
)

// TestDaemonPostsAuditReportAtStartup locks the #196 contract: the daemon
// must build its capabilities report and POST it to the server at startup,
// so the dashboard has hardware/FFmpeg data without a manual audit run.
func TestDaemonPostsAuditReportAtStartup(t *testing.T) {
	var mu sync.Mutex
	var postCount int
	var gotBody []byte
	var gotAPIKey, gotAuth, gotContentType string

	srv := newFakeServer(t)
	fallback := srv.ts.Config.Handler
	srv.ts.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/agents/audit-report" {
			fallback.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		postCount++
		gotBody = body
		gotAPIKey = r.Header.Get("X-API-Key")
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})

	d, err := Start(context.Background(), testResolvedConfig(t, srv))
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer d.Shutdown(context.Background())

	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return postCount > 0
	})

	mu.Lock()
	defer mu.Unlock()

	if gotAPIKey != "seam-test-api-key" {
		t.Errorf("X-API-Key = %q, want the configured API key", gotAPIKey)
	}
	if gotAuth != "Bearer seam-test-api-key" {
		t.Errorf("Authorization = %q, want Bearer <api key>", gotAuth)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}

	var report AuditReport
	if err := json.Unmarshal(gotBody, &report); err != nil {
		t.Fatalf("report body does not round-trip: %v\nbody: %s", err, gotBody)
	}
	if report.AgentVersion != Version {
		t.Errorf("AgentVersion = %q, want %q", report.AgentVersion, Version)
	}
	if report.ScannedAt == "" {
		t.Error("ScannedAt is empty")
	}
	if report.Hardware.CPU.Arch == "" {
		t.Error("Hardware.CPU.Arch is empty: hardware section not populated")
	}
}

// TestDaemonAuditReportUploadFailureIsNonFatal: a server that rejects the
// report must not fail or block daemon startup (best-effort upload).
func TestDaemonAuditReportUploadFailureIsNonFatal(t *testing.T) {
	srv := newFakeServer(t)
	fallback := srv.ts.Config.Handler
	srv.ts.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agents/audit-report" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fallback.ServeHTTP(w, r)
	})

	d, err := Start(context.Background(), testResolvedConfig(t, srv))
	if err != nil {
		t.Fatalf("Start failed despite rejected audit report: %v", err)
	}
	defer d.Shutdown(context.Background())
}
