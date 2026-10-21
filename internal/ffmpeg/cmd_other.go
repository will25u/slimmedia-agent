//go:build !windows

package ffmpeg

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func prepareCmd(cmd *exec.Cmd) *exec.Cmd {
	if cmd == nil {
		return nil
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Stdin = nil
	return cmd
}

func hideCmd(cmd *exec.Cmd) *exec.Cmd {
	return prepareCmd(cmd)
}

func interruptProcessGroup(pid int) error {
	return syscall.Kill(-pid, syscall.SIGINT)
}

func killProcessGroup(pid int) error {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = syscall.Kill(pid, syscall.SIGKILL)
	return nil
}

func isProcessGroupAlive(pid int) bool {
	err := syscall.Kill(-pid, 0)
	return err == nil
}

func isProcessDone(err error) bool {
	return errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone)
}

