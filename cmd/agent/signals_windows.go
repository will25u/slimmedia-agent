//go:build windows

package main

import "os"

// shutdownSignals returns the signals that trigger a graceful daemon
// shutdown on Windows. Windows has no POSIX signal delivery to services;
// Ctrl+C/Ctrl+Break arrive as os.Interrupt.
func shutdownSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}
