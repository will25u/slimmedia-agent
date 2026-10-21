//go:build windows

package daemon

import (
	"os"

	"github.com/will25u/slimmedia-agent/internal/updater"
)

// updateActivate downloads and verifies the new binary, stages it under the
// per-user state dir, and spawns the detached swap helper. A running .exe is
// locked on Windows, so the swap itself happens after this process exits;
// the helper then relaunches the agent. Returns the target executable path
// (the helper replaces its contents moments later).
func updateActivate(p updateActivateParams) (string, error) {
	exePath, err := updater.ExecutablePath()
	if err != nil {
		return "", err
	}

	staged, err := updater.DownloadVerified(p.apiURL, p.downloadPath, p.checksum, p.apiKey, updater.StageDir(p.stateDir), p.signature, p.version)
	if err != nil {
		return "", err
	}

	err = updater.SpawnSwapHelper(updater.SwapParams{
		Target:      exePath,
		Staged:      staged,
		Backup:      exePath + ".old",
		StateDir:    p.stateDir,
		PID:         os.Getpid(),
		Args:        relaunchArgs(),
		FromVersion: Version,
		ToVersion:   p.version,
	})
	if err != nil {
		return "", err
	}
	return exePath, nil
}

// updateFinish completes a Windows update: exit immediately so the swap
// helper can replace the now-unlocked executable and relaunch the agent. The
// applied (or rolled-back) event is emitted by the relaunched daemon from
// the helper's result marker, so onApplied is unused here. Never returns.
func updateFinish(newPath, version string, onApplied func()) error {
	os.Exit(0)
	return nil
}
