package daemon

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Version is the agent version reported in heartbeats, capabilities reports,
// and update checks. The release version is not hardcoded here: release
// builds (Dockerfiles, CI) inject the repo-root VERSION file via
// -ldflags -X, and dev builds keep the "0.0.0-dev" default.
//
// Every bump must follow the version-bump checklist in ADR-008 §4: the
// VERSION file, a new agent_releases record (see api/internal/db/seed.sql,
// which the API applies automatically at startup), and a CHANGELOG entry.
// TestVersionFileIsSingleSourceOfTruth fails when VERSION and the newest
// seed row disagree.
var Version = "0.0.0-dev"

// isNewerVersion reports whether remote is a strictly newer dotted version
// than local. A leading "v" on either side is ignored; malformed or equal
// versions are not newer.
func isNewerVersion(remote, local string) bool {
	remote = strings.TrimPrefix(strings.TrimSpace(remote), "v")
	local = strings.TrimPrefix(strings.TrimSpace(local), "v")
	if remote == "" || local == "" || remote == local {
		return false
	}
	rParts := strings.Split(remote, ".")
	lParts := strings.Split(local, ".")
	maxLen := len(rParts)
	if len(lParts) > maxLen {
		maxLen = len(lParts)
	}
	for i := 0; i < maxLen; i++ {
		var rVal, lVal int
		if i < len(rParts) {
			rVal, _ = strconv.Atoi(rParts[i])
		}
		if i < len(lParts) {
			lVal, _ = strconv.Atoi(lParts[i])
		}
		if rVal > lVal {
			return true
		}
		if rVal < lVal {
			return false
		}
	}
	return false
}

// fetchServerLatestVersion queries the server's /api/v1/version endpoint and
// returns the advertised latest agent version (falling back to the server
// version itself when no agent-specific field is set).
func fetchServerLatestVersion(apiURL string, client *http.Client) (string, error) {
	resp, err := client.Get(strings.TrimRight(apiURL, "/") + "/api/v1/version")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("version check returned %s", resp.Status)
	}

	var data struct {
		LatestAgentVersion string `json:"latest_agent_version"`
		Version            string `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBodyBytes)).Decode(&data); err != nil {
		return "", err
	}

	latest := data.LatestAgentVersion
	if latest == "" {
		latest = data.Version
	}
	return SanitizeTerminalString(latest), nil
}

// SanitizeTerminalString strips ASCII control characters (including ESC)
// and DEL from untrusted strings to prevent ANSI escape sequence injection
// into terminal displays.
func SanitizeTerminalString(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// checkVersion queries the server for the latest agent version on startup
// and warns (log + stdout) when a newer one is available. Best-effort: any
// failure is logged and ignored.
func checkVersion(apiURL string) {
	client := &http.Client{Timeout: 5 * time.Second}
	latest, err := fetchServerLatestVersion(apiURL, client)
	if err != nil {
		log.Printf("Could not check agent version: %v", err)
		return
	}

	cleanLatest := SanitizeTerminalString(latest)
	cleanVersion := SanitizeTerminalString(Version)
	if isNewerVersion(cleanLatest, cleanVersion) {
		fmt.Printf("\n⚠️  WARNING: A newer agent version (%s) is available. You are running version %s. Please update your agent!\n\n", cleanLatest, cleanVersion)
		log.Printf("WARNING: A newer agent version (%s) is available (current: %s)", cleanLatest, cleanVersion)
	} else {
		log.Printf("Agent version %s is up to date", cleanVersion)
	}
}
