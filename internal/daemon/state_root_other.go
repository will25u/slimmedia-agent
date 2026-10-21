//go:build !unix && !windows

package daemon

// stateRoot is unknown on this platform; StateDir falls back to <cwd>/bin.
func stateRoot() string { return "" }
