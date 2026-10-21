//go:build unix

package daemon

import (
	"os"
	"path/filepath"
)

// stateRoot returns the per-user state home on Unix (XDG_STATE_HOME or
// ~/.local/state), or "" when no home directory can be determined.
func stateRoot() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return d
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".local", "state")
	}
	return ""
}
