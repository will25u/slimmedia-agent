//go:build windows

package updater

import (
	"encoding/json"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// helperCommand builds the detached helper invocation. The helper gets its
// own process group and no console window, and inherits no handles or stdio:
// it is spawned while the agent is still running and must survive the
// agent's exit; its diagnostics go to the state-dir log.
func helperCommand(self string, p SwapParams) (*exec.Cmd, error) {
	argsJSON, err := json.Marshal(p.Args)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(self, helperArgs(p, string(argsJSON))...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW,
		HideWindow:    true,
	}
	return cmd, nil
}

// helperArgs renders the flag form parseHelperFlags reads back. Kept as a
// function so the round trip is unit-testable on every platform.
func helperArgs(p SwapParams, argsJSON string) []string {
	args := []string{
		"update-helper",
		"-target", p.Target,
		"-staged", p.Staged,
		"-backup", p.Backup,
		"-state-dir", p.StateDir,
		"-pid", strconv.Itoa(p.PID),
		"-args-json", argsJSON,
	}
	if p.FromVersion != "" {
		args = append(args, "-from-version", p.FromVersion)
	}
	if p.ToVersion != "" {
		args = append(args, "-to-version", p.ToVersion)
	}
	return args
}

// waitForProcessExit blocks until the agent's process object is signaled.
// Windows reaps the handle on exit, so this observes the real process rather
// than a recycled PID.
func waitForProcessExit(pid int, timeout time.Duration) error {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		// The agent is already gone (or we cannot open it): either way there
		// is nothing left holding the executable lock.
		return nil
	}
	defer windows.CloseHandle(h)

	millis := uint32(timeout / time.Millisecond)
	if timeout%time.Millisecond != 0 {
		millis++
	}
	event, _ := windows.WaitForSingleObject(h, millis)
	if event == windows.WAIT_OBJECT_0 {
		return nil
	}
	return errStillRunning
}

// watchProcess reports whether the relaunched agent stays alive for the
// whole grace period. The exited channel is closed (and the process handle
// reaped) the moment it terminates, so an immediate crash is observed even
// though the process is this helper's own child.
func watchProcess(exited <-chan struct{}, grace time.Duration) bool {
	select {
	case <-exited:
		return false
	case <-time.After(grace):
		return true
	}
}

// launchDetached starts the agent outside the helper's job/console so it
// keeps running after the helper exits. The returned channel is closed when
// the process exits.
func launchDetached(path string, args []string) (int, <-chan struct{}, error) {
	cmd := exec.Command(path, args...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW,
		HideWindow:    true,
	}
	if err := cmd.Start(); err != nil {
		return 0, nil, err
	}
	exited := make(chan struct{})
	go func() {
		cmd.Wait()
		close(exited)
	}()
	return cmd.Process.Pid, exited, nil
}
