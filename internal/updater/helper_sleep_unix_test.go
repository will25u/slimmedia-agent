//go:build unix

package updater

import "os/exec"

func helperSleepCommand() *exec.Cmd {
	return exec.Command("/bin/sh", "-c", "sleep 30")
}
