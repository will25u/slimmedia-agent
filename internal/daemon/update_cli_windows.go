//go:build windows

package daemon

import (
	"fmt"
	"os"

	"github.com/will25u/slimmedia-agent/internal/updater"
)

// cliUpdateNote follows the CLI `update` success line and explains the
// platform-specific restart semantics.
const cliUpdateNote = "A background helper finishes the installation once this process exits, then relaunches the agent with its saved configuration."

// installCLIUpdate downloads, verifies, and stages the update for the
// on-demand `slimmedia-agent update` command. A running .exe is locked on
// Windows, so the swap cannot happen from this process: the verified binary
// is staged under the per-user state dir and the detached swap helper
// replaces the executable after this process exits (this process itself
// holds the executable lock, so the helper waits on our PID), then
// relaunches the agent. If the agent daemon is also currently running it
// keeps holding the lock; the helper detects that, leaves the previous
// binary in place, and logs the outcome under the state dir.
func installCLIUpdate(apiURL, downloadPath, checksum, apiKey, signature string, version ...string) (string, error) {
	exePath, err := updater.ExecutablePath()
	if err != nil {
		return "", err
	}

	staged, err := updater.DownloadVerified(apiURL, downloadPath, checksum, apiKey, updater.StageDir(StateDir()), signature, version...)
	if err != nil {
		return "", err
	}

	if err := updater.SpawnSwapHelper(updater.SwapParams{
		Target:   exePath,
		Staged:   staged,
		Backup:   exePath + ".old",
		StateDir: StateDir(),
		PID:      os.Getpid(), // the helper swaps once this CLI process exits
		Args:     []string{"run"},
	}); err != nil {
		return "", fmt.Errorf("could not spawn update helper: %w", err)
	}
	return exePath, nil
}
