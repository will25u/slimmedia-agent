//go:build unix

package daemon

import (
	"os"
	"syscall"
)

// execSelf replaces the current process with the binary at newPath,
// preserving argv (minus one-time setup flags) and the environment.
func execSelf(newPath string) error {
	argv0 := newPath
	if len(os.Args) > 0 && os.Args[0] != "" {
		argv0 = os.Args[0]
	}
	args := append([]string{argv0}, relaunchArgs()...)
	return syscall.Exec(newPath, args, os.Environ())
}
