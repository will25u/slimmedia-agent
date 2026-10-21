// Package main's startup helpers decide where the Windows "Start on login"
// shortcut lives and whether it exists. This file is intentionally free of
// build tags and OS imports beyond os/filepath so the path logic is unit-
// testable on Linux; the COM-based enable/disable live in
// startup_windows.go.
package main

import (
	"os"
	"path/filepath"
)

// startupShortcutPath returns the path of the per-user Startup folder
// shortcut that makes the tray app launch on sign-in. appData is the
// %AppData% root (roaming profile); on non-Windows hosts it is empty, which
// yields a harmless relative path that os.Stat simply reports missing.
func startupShortcutPath(appData string) string {
	return filepath.Join(appData, "Microsoft", "Windows", "Start Menu", "Programs", "Startup", "SlimMedia Agent.lnk")
}

// autostartEnabled reports whether the Startup folder shortcut exists.
func autostartEnabled() bool {
	_, err := os.Stat(startupShortcutPath(os.Getenv("APPDATA")))
	return err == nil
}
