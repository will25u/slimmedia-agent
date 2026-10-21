//go:build unix

package main

import (
	"os"
	"syscall"
)

// shutdownSignals returns the signals that trigger a graceful daemon
// shutdown on Unix.
func shutdownSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM}
}
