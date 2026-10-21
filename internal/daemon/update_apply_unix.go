//go:build unix

package daemon

import (
	"github.com/will25u/slimmedia-agent/internal/updater"
)

// updateActivate downloads and verifies the new binary and renames it over
// the running executable. On Unix the old inode stays valid for the running
// process, so the replacement is atomic and safe mid-run. Returns the
// installed path for updateFinish to re-exec into.
func updateActivate(p updateActivateParams) (string, error) {
	return updater.FetchAndInstallAuth(p.apiURL, p.downloadPath, p.checksum, p.apiKey, p.signature, p.version)
}

// updateFinish completes a Unix update: report the applied event, then
// replace this process with the new binary. If execSelf fails, it automatically
// restores the previous binary from backup before returning the error (AC#2).
func updateFinish(newPath, version string, onApplied func()) error {
	if onApplied != nil {
		onApplied()
	}
	err := execSelf(newPath)
	if err != nil {
		// Automated rollback on failed launch
		_ = updater.RestoreBackup()
	}
	return err
}
