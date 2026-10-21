//go:build windows

package daemon

import "os"

// stateRoot returns the per-user roaming application data directory on
// Windows (%AppData%), so agent state never lands next to whatever working
// directory the binary was launched from.
func stateRoot() string {
	return os.Getenv("APPDATA")
}
