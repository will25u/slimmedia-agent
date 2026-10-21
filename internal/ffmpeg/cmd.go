package ffmpeg

import (
	"context"
	"os/exec"
	"time"
)

// gracefulShutdownTimeout is the duration to wait after sending an interrupt
// signal before escalating to SIGKILL. Package variable so tests can tune it.
var gracefulShutdownTimeout = 3 * time.Second

func execCommand(name string, arg ...string) *exec.Cmd {
	cmd := exec.Command(name, arg...)
	return prepareCmd(cmd)
}

func execCommandContext(ctx context.Context, name string, arg ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, arg...)
	prepareCmd(cmd)
	configureCancellation(cmd)
	return cmd
}

func configureCancellation(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	cmd.Cancel = func() error {
		if cmd.Process == nil || cmd.Process.Pid <= 0 {
			return nil
		}
		pid := cmd.Process.Pid
		if err := interruptProcessGroup(pid); err != nil {
			if isProcessDone(err) {
				return nil
			}
		}
		go escalateProcessGroup(pid, gracefulShutdownTimeout)
		return nil
	}
	cmd.WaitDelay = gracefulShutdownTimeout + 500*time.Millisecond
}

func escalateProcessGroup(pid int, timeout time.Duration) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timer.C:
			_ = killProcessGroup(pid)
			return
		case <-ticker.C:
			if !isProcessGroupAlive(pid) {
				return
			}
		}
	}
}
