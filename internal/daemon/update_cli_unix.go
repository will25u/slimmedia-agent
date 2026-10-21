//go:build unix

package daemon

import "github.com/will25u/slimmedia-agent/internal/updater"

// cliUpdateNote follows the CLI `update` success line and explains the
// platform-specific restart semantics.
const cliUpdateNote = "Note: a running daemon keeps its old binary until restarted — restart it to pick up the new version."

// installCLIUpdate downloads, verifies, and installs the update for the
// on-demand `slimmedia-agent update` command. On Unix the verified binary is
// renamed over the running executable immediately; the note tells the
// operator a running daemon still needs a restart.
func installCLIUpdate(apiURL, downloadPath, checksum, apiKey, signature string, version ...string) (string, error) {
	return updater.FetchAndInstallAuth(apiURL, downloadPath, checksum, apiKey, signature, version...)
}
