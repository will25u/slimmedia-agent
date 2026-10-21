//go:build unix

package updater

import (
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// helperCommand builds the detached helper invocation. The helper inherits
// no stdio: it is spawned while the agent is still running and must not die
// with the agent's process group; its diagnostics go to the state-dir log.
func helperCommand(self string, p SwapParams) (*exec.Cmd, error) {
	argsJSON, err := json.Marshal(p.Args)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(self, helperArgs(p, string(argsJSON))...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
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
		"-pid", itoa(p.PID),
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

// waitForProcessExit polls until the PID is gone or the budget expires.
func waitForProcessExit(pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if !processAlive(pid) {
			return nil
		}
		if time.Now().After(deadline) {
			return errStillRunning
		}
		time.Sleep(pollInterval)
	}
}

// processAlive reports whether a signal-0 probe succeeds. A PID that has
// been recycled by an unrelated process still passes the probe; the helper
// mitigates this by only ever waiting on the agent that spawned it moments
// ago, long before the PID can plausibly be recycled.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// watchProcess reports whether the relaunched agent stays alive for the
// whole grace period. The exited channel is closed (and the process reaped)
// the moment it terminates, so an immediate crash is observed even though
// the process is this helper's own child.
func watchProcess(exited <-chan struct{}, grace time.Duration) bool {
	select {
	case <-exited:
		return false
	case <-time.After(grace):
		return true
	}
}

// launchDetached starts the agent in its own session, detached from the
// helper's lifetime. Stdio stays nil (the daemon logs to its own log dir).
// The returned channel is closed when the process exits.
func launchDetached(path string, args []string) (int, <-chan struct{}, error) {
	cmd := exec.Command(path, args...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
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

func itoa(n int) string {
	return strconv.Itoa(n)
}
