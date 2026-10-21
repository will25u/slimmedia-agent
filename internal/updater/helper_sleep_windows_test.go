//go:build windows

package updater

import "os/exec"

func helperSleepCommand() *exec.Cmd {
	return exec.Command("powershell", "-Command", "Start-Sleep -Seconds 30")
}
