package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/will25u/slimmedia-agent/internal/protocol"
)

func TestSyncMediaLibrary_ChunkedPayloadsAndSessionID(t *testing.T) {
	type receivedPayload struct {
		SyncSessionID string                   `json:"sync_session_id"`
		IsFinalBatch  bool                     `json:"is_final_batch"`
		Items         []map[string]interface{} `json:"items"`
	}

	var mu sync.Mutex
	var received []receivedPayload

	srv := newFakeServer(t)
	srv.ts.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/library/sync" {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			var p receivedPayload
			if err := json.Unmarshal(body, &p); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			mu.Lock()
			received = append(received, p)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/api/v1/auth/exchange" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"seam-test-token"}`))
			return
		}
		if r.URL.Path == "/api/v1/version" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"version":"1.0.0","latest_agent_version":"1.0.0"}`))
			return
		}
		if r.URL.Path == "/api/v1/agents/audit-report" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/ws/agent" {
			srv.handleWS(w, r)
			return
		}
		http.NotFound(w, r)
	})

	cfg := testResolvedConfig(t, srv)
	mediaDir := t.TempDir()
	cfg.MediaDir = mediaDir

	// Create 502 test media files so totalChunks = 2 (chunk 1: 500, chunk 2: 2)
	subDir := filepath.Join(mediaDir, "movies")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}
	for i := 0; i < 502; i++ {
		p := filepath.Join(subDir, fmt.Sprintf("movie_%03d.mkv", i))
		if err := os.WriteFile(p, []byte("fake-video"), 0644); err != nil {
			t.Fatalf("write file failed: %v", err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err := Start(ctx, cfg)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Wait for the startup sync to complete (expecting 2 chunks)
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		count := len(received)
		mu.Unlock()
		if count >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(received) < 2 {
		t.Fatalf("expected 2 chunks, got %d", len(received))
	}

	chunk1 := received[0]
	chunk2 := received[1]

	if chunk1.SyncSessionID == "" {
		t.Errorf("chunk 1 session ID is empty")
	}
	if chunk1.SyncSessionID != chunk2.SyncSessionID {
		t.Errorf("expected session ID match between chunks: %q vs %q", chunk1.SyncSessionID, chunk2.SyncSessionID)
	}

	if len(chunk1.Items) != 500 {
		t.Errorf("expected chunk 1 to have 500 items, got %d", len(chunk1.Items))
	}
	if chunk1.IsFinalBatch {
		t.Errorf("expected chunk 1 to have is_final_batch: false")
	}

	if len(chunk2.Items) != 2 {
		t.Errorf("expected chunk 2 to have 2 items, got %d", len(chunk2.Items))
	}
	if !chunk2.IsFinalBatch {
		t.Errorf("expected chunk 2 to have is_final_batch: true")
	}

	// Verify relative_path
	firstItem := chunk1.Items[0]
	rel, _ := firstItem["relative_path"].(string)
	if !strings.HasPrefix(rel, "movies") {
		t.Errorf("expected relative_path starting with 'movies', got %q", rel)
	}
}

func TestSyncMediaLibrary_TransmitsRelativePathAndCreatesZeroMarkers(t *testing.T) {
	assignedLibID := "server-library-uuid-777"
	var receivedPayload struct {
		LibraryID string `json:"library_id"`
		LocalPath string `json:"local_path"`
		Items     []struct {
			FilePath     string `json:"file_path"`
			RelativePath string `json:"relative_path"`
			Filename     string `json:"filename"`
		} `json:"items"`
	}
	var syncCalled bool
	var mu sync.Mutex

	srv := newFakeServer(t)
	srv.ts.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/library/sync" {
			mu.Lock()
			syncCalled = true
			_ = json.NewDecoder(r.Body).Decode(&receivedPayload)
			mu.Unlock()

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"library_id": assignedLibID,
				"reconciled": true,
				"synced":     1,
			})
			return
		}
		if r.URL.Path == "/api/v1/auth/exchange" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"seam-test-token"}`))
			return
		}
		if r.URL.Path == "/api/v1/version" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"version":"1.0.0","latest_agent_version":"1.0.0"}`))
			return
		}
		if r.URL.Path == "/api/v1/agents/audit-report" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/ws/agent" {
			srv.handleWS(w, r)
			return
		}
		http.NotFound(w, r)
	})

	mediaDir := t.TempDir()
	subDir := filepath.Join(mediaDir, "Movies", "Action")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}
	testFile := filepath.Join(subDir, "test.mkv")
	if err := os.WriteFile(testFile, []byte("video-bytes"), 0644); err != nil {
		t.Fatalf("write file failed: %v", err)
	}

	cfg := testResolvedConfig(t, srv)
	cfg.MediaDir = mediaDir
	cfg.LibraryID = "initial-id"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Wait for sync to complete
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		done := syncCalled
		mu.Unlock()
		if done || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	_ = d.Shutdown(ctx)

	mu.Lock()
	defer mu.Unlock()

	if !syncCalled {
		t.Fatalf("expected library sync to be called")
	}

	if receivedPayload.LocalPath != mediaDir {
		t.Errorf("expected local_path %q, got %q", mediaDir, receivedPayload.LocalPath)
	}

	if len(receivedPayload.Items) == 0 {
		t.Fatalf("expected at least 1 item synced")
	}

	wantRel := "Movies/Action/test.mkv"
	gotRel := receivedPayload.Items[0].RelativePath
	if gotRel != wantRel {
		t.Errorf("expected relative_path %q, got %q", wantRel, gotRel)
	}

	// Verify ZERO marker files created anywhere on media shares
	marker1 := filepath.Join(mediaDir, ".slimmedia")
	marker2 := filepath.Join(subDir, ".slimmedia")
	marker3 := filepath.Join(mediaDir, "marker.json")
	for _, p := range []string{marker1, marker2, marker3} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("expected marker file %s not to exist", p)
		}
	}
}

func TestSyncMediaLibrary_BroadcastsFinalizingRegistryStatus(t *testing.T) {
	srv := newFakeServer(t)

	cfg := testResolvedConfig(t, srv)
	mediaDir := t.TempDir()
	cfg.MediaDir = mediaDir

	testFile := filepath.Join(mediaDir, "test.mkv")
	if err := os.WriteFile(testFile, []byte("fake-video"), 0644); err != nil {
		t.Fatalf("write file failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer func() { _ = d.Shutdown(ctx) }()

	foundFinalizing := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case env := <-srv.incoming:
			if env.Type == protocol.TypeScanProgress {
				var p protocol.ScanProgressPayload
				if err := json.Unmarshal(env.Payload, &p); err == nil {
					if p.Status == "Finalizing local registry..." {
						foundFinalizing = true
						break
					}
				}
			}
		case <-time.After(50 * time.Millisecond):
		}
		if foundFinalizing {
			break
		}
	}

	if !foundFinalizing {
		t.Fatalf("expected to receive scan progress with status 'Finalizing local registry...'")
	}
}


