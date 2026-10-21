//go:build unix

package updater

import (
	"fmt"
	"os"
	"path/filepath"
)

// FetchAndInstall downloads the agent binary from apiURL+downloadPath,
// verifies it against checksum (lowercase hex SHA-256) and the Ed25519
// signature (signature), and atomically replaces the running executable.
// On success it returns the installed path; the caller is expected to
// re-exec into it immediately.
func FetchAndInstall(apiURL, downloadPath, checksum, signature string) (string, error) {
	return FetchAndInstallAuth(apiURL, downloadPath, checksum, "", signature)
}

// BackupSuffix is the suffix used to preserve the previous binary on Unix.
const BackupSuffix = ".old"

// FetchAndInstallAuth is FetchAndInstall for servers that require download
// auth (AGENT_DOWNLOAD_AUTH_REQUIRED): apiKey is sent as both X-API-Key and
// Authorization: Bearer on the download request. The previous binary is
// preserved at exePath + ".old" to support automated rollback.
func FetchAndInstallAuth(apiURL, downloadPath, checksum, apiKey, signature string, targetVersion ...string) (string, error) {
	exePath, err := ExecutablePath()
	if err != nil {
		return "", err
	}

	staged, err := DownloadVerified(apiURL, downloadPath, checksum, apiKey, filepath.Dir(exePath), signature, targetVersion...)
	if err != nil {
		return "", err
	}

	backupPath := exePath + BackupSuffix
	_ = os.Remove(backupPath)
	// Preserve the previous binary for rollback (AC#2)
	if err := os.Rename(exePath, backupPath); err != nil {
		// If rename fails (e.g. cross-device or permission), try best-effort copy or proceed with direct replace
		_ = os.Remove(staged)
		return "", fmt.Errorf("could not back up existing binary %s: %w", exePath, err)
	}

	// Rename staged binary over exePath
	if err := os.Rename(staged, exePath); err != nil {
		// Roll back immediately if replacing fails
		_ = os.Rename(backupPath, exePath)
		return "", fmt.Errorf("could not replace %s: %w", exePath, err)
	}

	return exePath, nil
}

// RestoreBackup restores exePath from exePath + BackupSuffix if present.
func RestoreBackup() error {
	exePath, err := ExecutablePath()
	if err != nil {
		return err
	}
	backupPath := exePath + BackupSuffix
	if _, err := os.Stat(backupPath); err != nil {
		return err
	}
	_ = os.Remove(exePath)
	return os.Rename(backupPath, exePath)
}
