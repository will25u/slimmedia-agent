package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// registerHTTPClient bounds pairing/registration calls. Tests replace it to
// trust a local test-CA certificate.
var registerHTTPClient = &http.Client{Timeout: 15 * time.Second}

// registerAgent exchanges a pairing code for an API key. apiURL must already
// be canonicalized and policy-checked by the caller — this function is only
// ever reached after the transport policy accepted the endpoint, so the
// pairing code never rides a plaintext non-loopback connection.
func registerAgent(apiURL, pairingCode, agentName, mediaDir string) (string, string, error) {
	endpoint := strings.TrimRight(apiURL, "/") + "/api/v1/agents/register"

	bodyMap := map[string]string{
		"code": pairingCode,
		"name": agentName,
	}
	if mediaDir != "" {
		bodyMap["local_path"] = mediaDir
	}

	reqBody, err := json.Marshal(bodyMap)
	if err != nil {
		return "", "", err
	}

	resp, err := registerHTTPClient.Post(endpoint, "application/json", bytes.NewBuffer(reqBody))
	if err != nil {
		return "", "", fmt.Errorf("failed to reach server: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyBytes))
		return "", "", fmt.Errorf("registration failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}

	var regResp struct {
		APIKey    string `json:"api_key"`
		AgentID   string `json:"agent_id"`
		Name      string `json:"name"`
		LibraryID string `json:"library_id"`
	}
	// Bound the success path too: a slow-drip server must not be able to
	// inflate memory for the duration of the client timeout.
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBodyBytes)).Decode(&regResp); err != nil {
		return "", "", fmt.Errorf("failed to parse response: %w", err)
	}
	return regResp.APIKey, regResp.LibraryID, nil
}

// ErrConfigNotPersisted marks a pairing that succeeded at the server but
// whose config could not be saved: the caller gets the API key back but must
// treat the pairing as failed, otherwise the agent silently needs re-pairing
// after a restart.
var ErrConfigNotPersisted = errors.New("paired but config not persisted")

// Pair exchanges a pairing code for an API key and persists the resulting
// configuration. The pairing code is deliberately never logged: log files
// are only group-readable at best and the code is a credential.
//
// If the server accepted the pairing but the config cannot be saved, Pair
// returns the API key together with an error wrapping ErrConfigNotPersisted,
// so callers can surface the failure instead of reporting a clean success.
func Pair(apiURL, pairingCode, agentName, configPath, mediaDir, tempDir string) (string, error) {
	log.Printf("Pairing with server %s...", apiURL)
	registeredKey, libraryID, err := registerAgent(apiURL, pairingCode, agentName, mediaDir)
	if err != nil {
		return "", err
	}
	cfg := &LocalConfig{
		APIKey:    registeredKey,
		Server:    apiURL,
		Name:      agentName,
		MediaDir:  mediaDir,
		TempDir:   tempDir,
		LibraryID: libraryID,
	}
	if libraryID != "" && mediaDir != "" {
		cfg.Libraries = []LibraryMapping{
			{
				LibraryID: libraryID,
				LocalPath: mediaDir,
			},
		}
	}
	if err := SaveConfig(configPath, cfg); err != nil {
		return registeredKey, fmt.Errorf("%w to %s: %v", ErrConfigNotPersisted, MaskUserPath(configPath), err)
	}
	log.Printf("Successfully paired! Saved API key to %s", MaskUserPath(configPath))
	return registeredKey, nil
}
