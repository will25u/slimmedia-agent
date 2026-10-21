package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIsNewerVersion(t *testing.T) {
	tests := []struct {
		remote   string
		local    string
		expected bool
	}{
		{"1.0.1", "1.0.0", true},
		{"1.1.0", "1.0.0", true},
		{"2.0.0", "1.9.9", true},
		{"v1.2.0", "1.1.0", true},
		{"1.2.0", "v1.1.0", true},
		{"1.0.0", "1.0.0", false},
		{"0.9.9", "1.0.0", false},
		{"1.0.0", "1.0.1", false},
		{"1.0.0", "1.1.0", false},
		{"", "1.0.0", false},
		{"1.0.0", "", false},
	}

	for _, tc := range tests {
		got := isNewerVersion(tc.remote, tc.local)
		if got != tc.expected {
			t.Errorf("isNewerVersion(%q, %q) = %v, want %v", tc.remote, tc.local, got, tc.expected)
		}
	}
}

func TestCheckVersion(t *testing.T) {
	// Test mock server
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/version" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"latest_agent_version": "2.0.0",
			"version":              "2.0.0",
		})
	}))
	defer ts.Close()

	// Should successfully query and print warning without panic
	checkVersion(ts.URL)
}

// TestFetchServerLatestVersionLegacyFallback pins that the version check
// falls back to the legacy "version" field when the server does not set
// "latest_agent_version" (older servers only report their own version).
func TestFetchServerLatestVersionLegacyFallback(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/version" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"version": "1.2.3"})
	}))
	defer ts.Close()

	latest, err := fetchServerLatestVersion(ts.URL, ts.Client())
	if err != nil {
		t.Fatalf("fetchServerLatestVersion returned error: %v", err)
	}
	if latest != "1.2.3" {
		t.Fatalf("latest = %q, want 1.2.3 from legacy version field", latest)
	}
}

// TestFetchServerLatestVersionEmpty pins that a server advertising neither
// version field yields ("", nil) from fetchServerLatestVersion, and that the
// CLI update flow's fetchLatestAgentVersion turns that into a clear error
// instead of silently "updating" to an empty version.
func TestFetchServerLatestVersionEmpty(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/version" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{})
	}))
	defer ts.Close()

	latest, err := fetchServerLatestVersion(ts.URL, ts.Client())
	if err != nil {
		t.Fatalf("fetchServerLatestVersion returned error: %v", err)
	}
	if latest != "" {
		t.Fatalf("latest = %q, want empty when neither version field is set", latest)
	}

	_, err = fetchLatestAgentVersion(ts.URL)
	if err == nil {
		t.Fatal("fetchLatestAgentVersion accepted a server advertising no agent version")
	}
	if !strings.Contains(err.Error(), "server returned no agent version") {
		t.Fatalf("fetchLatestAgentVersion error = %v, want it to contain 'server returned no agent version'", err)
	}
}

func TestFetchServerLatestVersionOversizedBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"` + strings.Repeat("x", maxResponseBodyBytes+1)))
	}))
	defer ts.Close()

	_, err := fetchServerLatestVersion(ts.URL, ts.Client())
	if err == nil {
		t.Fatal("expected decode failure on oversized truncated body")
	}
}

func TestSanitizeTerminalString(t *testing.T) {
	cases := []struct {
		input    string
		expected string
	}{
		{"2.0.0", "2.0.0"},
		{"\x1b[31m2.0.0\x1b[0m", "[31m2.0.0[0m"},
		{"\x1b]8;;http://evil.com\x07Click\x1b]8;;\x07", "]8;;http://evil.comClick]8;;"},
		{"v1.2.3\r\n\x00\x08", "v1.2.3"},
		{"\x7fdelete\x1b", "delete"},
	}

	for _, tc := range cases {
		got := SanitizeTerminalString(tc.input)
		if got != tc.expected {
			t.Errorf("SanitizeTerminalString(%q) = %q, want %q", tc.input, got, tc.expected)
		}
		for _, r := range got {
			if r < 0x20 || r == 0x7f {
				t.Errorf("SanitizeTerminalString output contains control char %d in %q", r, got)
			}
		}
	}
}
