package updater

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// These mirror the FetchAndInstall safety tests in updater_test.go for the
// shared download/verify/stage step both platform installers build on. All
// cases run on every OS: failure cases abort before any staged file is
// handed to a caller, and the success case only stages into a temp dir.

func TestDownloadVerifiedRefusesMissingChecksum(t *testing.T) {
	if _, err := DownloadVerified("http://localhost", "/dl", "", "", t.TempDir(), ""); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("expected checksum refusal, got %v", err)
	}
}

func TestDownloadVerifiedBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer srv.Close()

	if _, err := DownloadVerified(srv.URL, "/dl", "deadbeef", "", t.TempDir(), ""); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("expected 404 error, got %v", err)
	}
}

func TestDownloadVerifiedChecksumMismatchLeavesNothingStaged(t *testing.T) {
	stageDir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "not the real binary")
	}))
	defer srv.Close()

	_, err := DownloadVerified(srv.URL, "/dl", "0000000000000000000000000000000000000000000000000000000000000000", "", stageDir, "")
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected checksum mismatch, got %v", err)
	}
	entries, _ := os.ReadDir(stageDir)
	if len(entries) != 0 {
		t.Fatalf("staged files left behind after failed verification: %v", entries)
	}
}

func TestDownloadVerifiedSendsAPIKey(t *testing.T) {
	var gotKey, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-API-Key")
		gotAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, "payload")
	}))
	defer srv.Close()

	// The download itself fails checksum verification, but only after the
	// server has seen the request headers.
	DownloadVerified(srv.URL, "/dl", "0000000000000000000000000000000000000000000000000000000000000000", "sm_testkey", t.TempDir(), "")

	if gotKey != "sm_testkey" {
		t.Fatalf("X-API-Key = %q, want sm_testkey", gotKey)
	}
	if gotAuth != "Bearer sm_testkey" {
		t.Fatalf("Authorization = %q, want Bearer sm_testkey", gotAuth)
	}
}

func TestDownloadVerifiedSuccessStagesExecutable(t *testing.T) {
	priv := withReleaseKey(t)

	stageDir := t.TempDir()
	payload := []byte("the real binary")
	sum := sha256.Sum256(payload)
	sig := ed25519.Sign(priv, payload)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer srv.Close()

	staged, err := DownloadVerified(srv.URL, "/dl", hex.EncodeToString(sum[:]), "", stageDir, hex.EncodeToString(sig))
	if err != nil {
		t.Fatalf("DownloadVerified returned error: %v", err)
	}
	defer os.Remove(staged)

	got, err := os.ReadFile(staged)
	if err != nil {
		t.Fatalf("staged file missing: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("staged content = %q, want %q", got, payload)
	}
	info, err := os.Stat(staged)
	if err != nil {
		t.Fatalf("stat staged file: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0755 {
		t.Fatalf("staged mode = %v, want 0755", info.Mode().Perm())
	}
}

// TestDownloadVerifiedDevSentinelRefusesSignedUpdate: with the default
// all-zero dev sentinel key embedded, even a checksum-correct, correctly
// signed payload is refused and nothing is staged.
func TestDownloadVerifiedDevSentinelRefusesSignedUpdate(t *testing.T) {
	stageDir := t.TempDir()
	payload := []byte("the real binary")
	sum := sha256.Sum256(payload)

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	sig := ed25519.Sign(priv, payload)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer srv.Close()

	_, err = DownloadVerified(srv.URL, "/dl", hex.EncodeToString(sum[:]), "", stageDir, hex.EncodeToString(sig))
	if err == nil || !strings.Contains(err.Error(), "dev sentinel") {
		t.Fatalf("expected dev sentinel refusal, got %v", err)
	}
	entries, _ := os.ReadDir(stageDir)
	if len(entries) != 0 {
		t.Fatalf("staged files left behind after refused update: %v", entries)
	}
}

// TestDownloadVerifiedMissingSignatureRefused: a real embedded key with a
// valid checksum but no signature is refused — a checksum present means a
// signature is required.
func TestDownloadVerifiedMissingSignatureRefused(t *testing.T) {
	withReleaseKey(t)

	stageDir := t.TempDir()
	payload := []byte("the real binary")
	sum := sha256.Sum256(payload)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer srv.Close()

	_, err := DownloadVerified(srv.URL, "/dl", hex.EncodeToString(sum[:]), "", stageDir, "")
	if err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("expected missing-signature refusal, got %v", err)
	}
	entries, _ := os.ReadDir(stageDir)
	if len(entries) != 0 {
		t.Fatalf("staged files left behind after refused update: %v", entries)
	}
}

func TestWithGOOS(t *testing.T) {
	want := "goos=" + runtime.GOOS
	if got := WithGOOS("/api/v1/agents/download"); !strings.HasSuffix(got, "?"+want) {
		t.Fatalf("WithGOOS plain path = %q, want suffix ?%s", got, want)
	}
	if got := WithGOOS("/api/v1/agents/download?auth=1"); !strings.HasSuffix(got, "&"+want) {
		t.Fatalf("WithGOOS queried path = %q, want suffix &%s", got, want)
	}
	pinned := "/api/v1/agents/download?goos=windows"
	if got := WithGOOS(pinned); got != pinned {
		t.Fatalf("WithGOOS re-wrote a pinned path: %q -> %q", pinned, got)
	}
}

func TestStageDir(t *testing.T) {
	if got, want := StageDir("/state"), filepath.Join("/state", "updates"); got != want {
		t.Fatalf("StageDir = %q, want %q", got, want)
	}
}

func TestDownloadVerifiedExceedsSizeLimit(t *testing.T) {
	origLimit := maxUpdateBytes
	maxUpdateBytes = 32
	t.Cleanup(func() { maxUpdateBytes = origLimit })

	stageDir := t.TempDir()

	t.Run("content length header exceeds limit", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		_, err := DownloadVerified(srv.URL, "/dl", "0000000000000000000000000000000000000000000000000000000000000000", "", stageDir, "")
		if err == nil || !strings.Contains(err.Error(), "exceeds maximum allowed size") {
			t.Fatalf("expected size limit error, got %v", err)
		}
		entries, _ := os.ReadDir(stageDir)
		if len(entries) != 0 {
			t.Fatalf("staged files left behind after size limit failure: %v", entries)
		}
	})

	t.Run("stream body exceeds limit", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Write 100 bytes without Content-Length
			w.Write([]byte(strings.Repeat("a", 100)))
		}))
		defer srv.Close()

		_, err := DownloadVerified(srv.URL, "/dl", "0000000000000000000000000000000000000000000000000000000000000000", "", stageDir, "")
		if err == nil || !strings.Contains(err.Error(), "exceeds maximum allowed size") {
			t.Fatalf("expected size limit error, got %v", err)
		}
		entries, _ := os.ReadDir(stageDir)
		if len(entries) != 0 {
			t.Fatalf("staged files left behind after size limit failure: %v", entries)
		}
	})
}

func TestDownloadVerifiedRefusesForeignOrigin(t *testing.T) {
	stageDir := t.TempDir()
	// apiURL is localhost, downloadPath points to external/attacker domain
	_, err := DownloadVerified("http://localhost:8082", "http://attacker.example.com/malicious-agent", "0000000000000000000000000000000000000000000000000000000000000000", "", stageDir, "")
	if err == nil || !strings.Contains(err.Error(), "untrusted origin") {
		t.Fatalf("expected origin mismatch error, got %v", err)
	}
	entries, _ := os.ReadDir(stageDir)
	if len(entries) != 0 {
		t.Fatalf("staged files left behind after origin refusal: %v", entries)
	}
}

func TestDownloadVerifiedRefusesPathTraversal(t *testing.T) {
	stageDir := t.TempDir()
	_, err := DownloadVerified("http://localhost:8082", "/../../etc/shadow", "0000000000000000000000000000000000000000000000000000000000000000", "", stageDir, "")
	if err == nil || !strings.Contains(err.Error(), "untrusted origin") {
		t.Fatalf("expected path traversal refusal error, got %v", err)
	}
}

func TestValidateDownloadOriginQueryPaths(t *testing.T) {
	cases := []struct {
		name    string
		apiURL  string
		path    string
		wantURI string
	}{
		{
			name:    "windows agent download query path",
			apiURL:  "http://localhost:8082",
			path:    "/api/v1/agents/download?goos=windows",
			wantURI: "/api/v1/agents/download?goos=windows",
		},
		{
			name:    "plain download path",
			apiURL:  "http://localhost:8082",
			path:    "/api/v1/agents/download",
			wantURI: "/api/v1/agents/download",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateDownloadOrigin(tc.apiURL, tc.path)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantURI {
				t.Fatalf("got %q, want %q", got, tc.wantURI)
			}
		})
	}
}

func TestDownloadVerifiedVersionBoundSignature(t *testing.T) {
	priv := withReleaseKey(t)
	stageDir := t.TempDir()
	payload := []byte("the real agent binary")
	sum := sha256.Sum256(payload)
	sumHex := hex.EncodeToString(sum[:])
	version := "2.6.4"

	signedPayload := ReleaseSignaturePayload(version, sumHex)
	sig := ed25519.Sign(priv, signedPayload)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer srv.Close()

	// 1. Success when target version matches
	staged, err := DownloadVerified(srv.URL, "/dl", sumHex, "", stageDir, hex.EncodeToString(sig), version)
	if err != nil {
		t.Fatalf("DownloadVerified failed with matching version: %v", err)
	}
	os.Remove(staged)

	// 2. Failure when server offers an older or replayed version
	_, err = DownloadVerified(srv.URL, "/dl", sumHex, "", stageDir, hex.EncodeToString(sig), "2.6.3")
	if err == nil || !strings.Contains(err.Error(), "update binary rejected") {
		t.Fatalf("DownloadVerified accepted replayed signature from different version, got: %v", err)
	}
}

