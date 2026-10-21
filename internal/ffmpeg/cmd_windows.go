//go:build windows

package ffmpeg

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"

	"golang.org/x/sys/windows"
)

// createNoWindow prevents Windows from allocating a console window for child processes.
const createNoWindow = 0x08000000

// prepareCmd sets Windows process attributes so that console applications (ffmpeg, ffprobe)
// run silently in their own process group without interactive stdin.
func prepareCmd(cmd *exec.Cmd) *exec.Cmd {
	if cmd == nil {
		return nil
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow | windows.CREATE_NEW_PROCESS_GROUP
	cmd.Stdin = nil
	return cmd
}

func hideCmd(cmd *exec.Cmd) *exec.Cmd {
	return prepareCmd(cmd)
}

func interruptProcessGroup(pid int) error {
	// GenerateConsoleCtrlEvent sends CTRL_BREAK_EVENT to the process group.
	// When CREATE_NEW_PROCESS_GROUP is used on Windows, the root PID is also the process group ID.
	err := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(pid))
	if err != nil {
		return killProcessGroup(pid)
	}
	return nil
}

func killProcessGroup(pid int) error {
	cmd := exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid))
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: createNoWindow,
		HideWindow:    true,
	}
	if err := cmd.Run(); err == nil {
		return nil
	}
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(h)
	return windows.TerminateProcess(h, 1)
}

func isProcessGroupAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var exitCode uint32
	if err := windows.GetExitCodeProcess(h, &exitCode); err != nil {
		return false
	}
	const stillActive = 259
	return exitCode == stillActive
}

func isProcessDone(err error) bool {
	return errors.Is(err, os.ErrProcessDone)
}

