//go:build windows

package main

import "fmt"

// The interactive audit TUI depends on Unix terminal control (raw-mode
// termios ioctl, ANSI cursor movement) and is excluded from the Windows
// build; the daemon's automatic pre-connect capabilities report covers the
// headless path.
func runAudit(targetDir, saasServer, saasAPIKey string, insecure bool) {
	fmt.Println("The interactive audit command is not supported on Windows; capabilities are reported automatically when the agent connects.")
}

// auditConfigFallback is a no-op on Windows (audit is unsupported); it exists
// so cmd/main.go compiles on every platform.
func auditConfigFallback(saasServer, saasAPIKey string) (string, string) {
	return saasServer, saasAPIKey
}
