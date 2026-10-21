package daemon

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/will25u/slimmedia-agent/internal/transport"
	"github.com/will25u/slimmedia-agent/internal/updater"
)

// agentDownloadPath and agentChecksumPath are the server-hosted endpoints the
// update flow pulls the binary and its SHA-256 from. The server dispatches
// the same paths in its AGENT_UPDATE payloads, so the manual and dashboard-triggered
// flows always fetch an identical payload. Both are fetched with the agent's
// GOOS pinned on the query string so the server serves the platform-matching
// binary (see updater.WithGOOS).
const (
	agentDownloadPath = "/api/v1/agents/download"
	agentChecksumPath = "/api/v1/agents/download/sha256"
)

// setUpdateAuthHeaders attaches the agent's API key to update-related requests.
// Only needed against deployments that set AGENT_DOWNLOAD_AUTH_REQUIRED; the
// default download endpoints are open.
func setUpdateAuthHeaders(req *http.Request, apiKey string) {
	if apiKey == "" {
		return
	}
	req.Header.Set("X-API-Key", apiKey)
	req.Header.Set("Authorization", "Bearer "+apiKey)
}

// fetchLatestAgentVersion asks the server which agent version it currently
// ships. Mirrors the startup version check, but fatal-free so the CLI
// command can surface the error itself.
func fetchLatestAgentVersion(apiURL string) (string, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	latest, err := fetchServerLatestVersion(apiURL, client)
	if err != nil {
		return "", err
	}
	if latest == "" {
		return "", fmt.Errorf("server returned no agent version")
	}
	return latest, nil
}

// fetchAgentChecksum retrieves the SHA-256 and Ed25519 signature of the binary
// the server would serve this agent's platform, so the download can be verified
// before it replaces the running executable.
func fetchAgentChecksum(apiURL, apiKey string) (checksum, signature string, err error) {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(apiURL, "/")+updater.WithGOOS(agentChecksumPath), nil)
	if err != nil {
		return "", "", err
	}
	setUpdateAuthHeaders(req, apiKey)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("checksum endpoint returned %s", resp.Status)
	}
	var data struct {
		SHA256    string `json:"sha256"`
		Signature string `json:"signature"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBodyBytes)).Decode(&data); err != nil {
		return "", "", err
	}
	if data.SHA256 == "" {
		return "", "", fmt.Errorf("server returned an empty checksum")
	}
	return strings.ToLower(strings.TrimSpace(data.SHA256)), data.Signature, nil
}

// configuredAgentAPIKey resolves the agent's configured API key from the
// SLIMMEDIA_API_KEY environment variable or the saved config under the
// per-user state dir.
func configuredAgentAPIKey() string {
	if k := os.Getenv("SLIMMEDIA_API_KEY"); k != "" {
		return k
	}
	if savedCfg, err := LoadSavedConfig(); err == nil && savedCfg.APIKey != "" {
		return savedCfg.APIKey
	}
	return ""
}

// RunUpdate implements the `slimmedia-agent update` command: query the
// server for the latest agent version and, when it is newer than the running
// binary (or force is given), download the server-hosted binary, verify
// its SHA-256, and atomically replace the running executable. Unlike the
// dashboard-triggered AGENT_UPDATE path, this runs on demand from the shell
// and never restarts a running daemon — the operator restarts it to pick up
// the new binary.
func RunUpdate(server, apiKey string, insecure, force bool) error {
	if apiKey == "" {
		apiKey = configuredAgentAPIKey()
	}
	if server == "" {
		if savedCfg, err := LoadSavedConfig(); err == nil && savedCfg.Server != "" {
			server = savedCfg.Server
		}
	}
	if server == "" {
		server = "http://localhost:8082"
	}

	apiURL, _, err := transport.ResolveServerURLs(server, insecure)
	if err != nil {
		return fmt.Errorf("invalid --server URL: %w", err)
	}

	latest, err := fetchLatestAgentVersion(apiURL)
	if err != nil {
		return fmt.Errorf("could not check for updates: %w", err)
	}

	cleanLatest := SanitizeTerminalString(latest)
	cleanVersion := SanitizeTerminalString(Version)
	if !force && !isNewerVersion(cleanLatest, cleanVersion) {
		fmt.Printf("slimmedia-agent is up to date (running v%s, server offers v%s)\n", cleanVersion, cleanLatest)
		return nil
	}

	fmt.Printf("Updating slimmedia-agent from v%s to v%s...\n", cleanVersion, cleanLatest)

	checksum, sig, err := fetchAgentChecksum(apiURL, apiKey)
	if err != nil {
		return fmt.Errorf("could not fetch update checksum: %w", err)
	}

	newPath, err := installCLIUpdate(apiURL, agentDownloadPath, checksum, apiKey, sig, cleanLatest)
	if err != nil {
		return fmt.Errorf("update failed: %w", err)
	}

	fmt.Printf("Updated %s to v%s.\n", newPath, cleanLatest)
	fmt.Println(cliUpdateNote)
	return nil
}

// emitUpdateResult reports the outcome of a helper-completed Windows
// self-update on daemon startup. The swap helper writes an update_result.json
// marker in the state dir before relaunching the agent; consuming it here
// turns it into update_applied / update_rolled_back events (and a log line)
// exactly once. Unix never writes the marker — there the applied event is
// emitted by the updating process itself before its re-exec.
func (d *Daemon) emitUpdateResult() {
	res, err := updater.TakeUpdateResult(StateDir())
	if err != nil {
		log.Printf("Could not read update result marker: %v", err)
		return
	}
	if res == nil {
		return
	}
	switch res.Status {
	case updater.UpdateResultApplied:
		log.Printf("Self-update applied: agent v%s is now running v%s", res.FromVersion, res.ToVersion)
		d.emit(Event{Type: EventUpdateApplied, Message: res.ToVersion, Detail: "agent updated from v" + res.FromVersion + " to v" + res.ToVersion})
	case updater.UpdateResultRolledBack:
		log.Printf("Self-update to v%s failed and was rolled back to v%s", res.ToVersion, res.FromVersion)
		d.emit(Event{Type: EventUpdateRolledBack, Message: res.FromVersion, Detail: "update to v" + res.ToVersion + " rolled back; agent v" + res.FromVersion + " restored"})
	default:
		log.Printf("Ignoring update result marker with unknown status %q", res.Status)
	}
}
