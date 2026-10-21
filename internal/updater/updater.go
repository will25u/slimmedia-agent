// Package updater implements the agent's self-update path: download the new
// binary from the SlimMedia server, verify its SHA-256 against the checksum
// delivered in the signed WebSocket command, replace the running executable,
// and hand off to the new binary.
//
// The checksum check is the trust anchor: the AGENT_UPDATE envelope arrives
// over the agent's authenticated WebSocket (server-authenticated via JWT),
// and the binary bytes arrive over TLS from the same server. Refusing to swap
// on mismatch prevents a truncated or substituted download from bricking the
// installation.
//
// Activation is platform-split (install_unix.go / install_windows.go): on
// Unix the verified binary is renamed over the running executable and the
// process re-execs; on Windows a running .exe is locked, so the verified
// binary is staged under the per-user state dir and a small helper process
// (the `update-helper` subcommand, see helper.go) swaps it in after the
// agent exits, then relaunches the agent.
package updater

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// maxUpdateBytes caps the size of downloaded agent updates (512 MiB) to prevent
// disk-exhaustion attacks from an oversized response.
var maxUpdateBytes int64 = 512 * 1024 * 1024 // 512 MiB

// WithGOOS pins a download or checksum path to the agent's own operating
// system so the server serves the matching binary. Paths that already carry
// the parameter (server-built payloads) are returned unchanged.
func WithGOOS(downloadPath string) string {
	if strings.Contains(downloadPath, "goos=") {
		return downloadPath
	}
	sep := "?"
	if strings.Contains(downloadPath, "?") {
		sep = "&"
	}
	return downloadPath + sep + "goos=" + runtime.GOOS
}

// ExecutablePath resolves the real on-disk path of the running executable,
// following symlinks (e.g. /proc/self/exe or a bin/ symlink) so the file
// actually replaced is the one a service manager or shortcut points at.
func ExecutablePath() (string, error) {
	exePath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("could not locate running executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = resolved
	}
	return exePath, nil
}

// StageDir returns where verified update binaries are staged on platforms
// that cannot rename over the running executable. The directory lives under
// the per-user state root so a staged update survives the agent's exit (and
// a reboot between staging and the helper's swap).
func StageDir(stateDir string) string {
	return filepath.Join(stateDir, "updates")
}

// DownloadVerified downloads the agent binary from apiURL+downloadPath,
// verifies it against checksum (lowercase hex SHA-256) and signature
// (hex-encoded Ed25519 signature from the release private key), and stages
// the verified bytes in stageDir. On success the staged path is returned and
// ownership passes to the caller (Unix renames it over the running
// executable; Windows hands it to the swap helper). On any failure the
// staged file is removed, so a failed download never looks like an installed
// binary.
//
// Signature verification is enforced on every download: a missing signature,
// an invalid signature, or an embedded dev-sentinel release key each refuse
// the update (see VerifySignature).
// validateDownloadOrigin validates that downloadPath does not attempt to redirect
// or target an origin different from apiURL. If downloadPath is an absolute URL,
// its scheme and host must match apiURL exactly. Relative paths must not attempt
// directory traversal.
func validateDownloadOrigin(apiURL, downloadPath string) (string, error) {
	apiU, err := url.Parse(apiURL)
	if err != nil || apiU.Host == "" {
		return "", fmt.Errorf("invalid server apiURL %q: %w", apiURL, err)
	}

	trimmedPath := strings.TrimSpace(downloadPath)
	if trimmedPath == "" {
		return "", fmt.Errorf("empty download path")
	}

	dlU, err := url.Parse(trimmedPath)
	if err != nil {
		return "", fmt.Errorf("invalid download path %q: %w", downloadPath, err)
	}

	// If downloadPath provides an absolute URL, enforce identical origin
	if dlU.IsAbs() || dlU.Host != "" {
		if !strings.EqualFold(dlU.Scheme, apiU.Scheme) || !strings.EqualFold(dlU.Host, apiU.Host) {
			return "", fmt.Errorf("download origin mismatch: %s does not match server %s", dlU.Host, apiU.Host)
		}
		return dlU.RequestURI(), nil
	}

	// Relative path check: reject directory traversal attempts
	rawPath := dlU.Path
	if strings.Contains(rawPath, "/../") || strings.HasSuffix(rawPath, "/..") || strings.HasPrefix(rawPath, "../") || rawPath == ".." || strings.Contains(rawPath, `\..\`) {
		return "", fmt.Errorf("invalid download path traversal: %s", downloadPath)
	}

	cleanPath := path.Clean("/" + strings.TrimPrefix(dlU.Path, "/"))
	if !strings.HasPrefix(cleanPath, "/") || strings.Contains(cleanPath, "..") {
		return "", fmt.Errorf("invalid download path traversal: %s", downloadPath)
	}

	reqURI := cleanPath
	if dlU.RawQuery != "" {
		reqURI += "?" + dlU.RawQuery
	}
	return reqURI, nil
}

// DownloadVerified downloads the agent binary from apiURL+downloadPath,
// verifies it against checksum (lowercase hex SHA-256) and signature
// (hex-encoded Ed25519 signature from the release private key), and stages
// the verified bytes in stageDir. On success the staged path is returned and
// ownership passes to the caller (Unix renames it over the running
// executable; Windows hands it to the swap helper). On any failure the
// staged file is removed, so a failed download never looks like an installed
// binary.
//
// Signature verification is enforced on every download: a missing signature,
// an invalid signature, or an embedded dev-sentinel release key each refuse
// the update (see VerifySignature). When targetVersion is provided, signature
// verification verifies version||sha256 to prevent downgrade/replay attacks.
func DownloadVerified(apiURL, downloadPath, checksum, apiKey, stageDir string, signature string, targetVersion ...string) (string, error) {
	if checksum == "" {
		return "", fmt.Errorf("refusing to self-update without a checksum")
	}
	checksum = strings.ToLower(strings.TrimSpace(checksum))

	pub, err := releasePublicKey()
	if err != nil {
		return "", fmt.Errorf("embedded release public key is invalid: %w", err)
	}

	validPath, err := validateDownloadOrigin(apiURL, downloadPath)
	if err != nil {
		return "", fmt.Errorf("refusing download from untrusted origin: %w", err)
	}

	downloadURL := strings.TrimRight(apiURL, "/") + WithGOOS(validPath)
	client := &http.Client{Timeout: 10 * time.Minute}
	req, err := http.NewRequest(http.MethodGet, downloadURL, nil)
	if err != nil {
		return "", fmt.Errorf("could not build download request: %w", err)
	}
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download returned %s", resp.Status)
	}

	if err := os.MkdirAll(stageDir, 0700); err != nil {
		return "", fmt.Errorf("could not create update staging dir: %w", err)
	}
	// Hidden temp name keeps a failed update from looking like an installed
	// binary; staging on one filesystem keeps the final rename atomic.
	// Created with restrictive 0600 permissions by os.CreateTemp.
	tmp, err := os.CreateTemp(stageDir, ".slimmedia-agent-update-*")
	if err != nil {
		return "", fmt.Errorf("could not stage update: %w", err)
	}
	tmpPath := tmp.Name()
	keep := false
	defer func() {
		if !keep {
			os.Remove(tmpPath)
		}
	}()

	// Ensure restrictive permissions on staged file prior to verification and swap (AC#6)
	_ = os.Chmod(tmpPath, 0600)

	if resp.ContentLength > maxUpdateBytes {
		return "", fmt.Errorf("update payload exceeds maximum allowed size (%d bytes)", maxUpdateBytes)
	}

	hasher := sha256.New()
	limited := io.LimitReader(resp.Body, maxUpdateBytes+1)
	n, err := io.Copy(io.MultiWriter(tmp, hasher), limited)
	if err != nil {
		tmp.Close()
		return "", fmt.Errorf("download interrupted: %w", err)
	}
	if n > maxUpdateBytes {
		tmp.Close()
		return "", fmt.Errorf("update payload exceeds maximum allowed size (%d bytes)", maxUpdateBytes)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("could not finalize staged update: %w", err)
	}

	got := hex.EncodeToString(hasher.Sum(nil))
	if got != checksum {
		return "", fmt.Errorf("checksum mismatch: expected %s, got %s", checksum, got)
	}

	// Ed25519 signature verification. Read the staged file back — SHA-256
	// confirmed integrity, so the bytes on disk are exactly what the server
	// sent. A checksum present means a signature is required: VerifySignature
	// refuses empty signatures and an embedded dev-sentinel key.
	binaryBytes, err := os.ReadFile(tmpPath)
	if err != nil {
		return "", fmt.Errorf("could not read staged binary for sig verification: %w", err)
	}
	var ver string
	if len(targetVersion) > 0 {
		ver = targetVersion[0]
	}
	if err := VerifySignature(pub, binaryBytes, signature, ver); err != nil {
		return "", fmt.Errorf("update binary rejected: %w", err)
	}

	if err := os.Chmod(tmpPath, 0755); err != nil {
		return "", fmt.Errorf("could not mark update executable: %w", err)
	}

	keep = true
	return tmpPath, nil
}

