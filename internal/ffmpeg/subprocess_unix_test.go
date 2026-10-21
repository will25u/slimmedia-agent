//go:build !windows

package ffmpeg

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func checkSysProcAttr(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr is nil")
	}
	if !cmd.SysProcAttr.Setpgid {
		t.Errorf("expected Setpgid: true on Unix")
	}
}

func TestContextCancellation_GracefulShutdownSIGINT(t *testing.T) {
	tmp := t.TempDir()
	script := `#!/bin/sh
trap 'exit 0' INT
while true; do
    sleep 0.05
done
`
	scriptPath := filepath.Join(tmp, "graceful.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write script: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := execCommandContext(ctx, scriptPath)

	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start failed: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	cancel()

	err := cmd.Wait()
	elapsed := time.Since(start)

	if err != nil {
		t.Logf("cmd.Wait returned error: %v", err)
	}

	// Should have exited promptly on SIGINT without waiting for the 3s fallback
	if elapsed >= 2*time.Second {
		t.Errorf("graceful cancellation took %v, expected < 2s", elapsed)
	}
}

func TestContextCancellation_EscalationToSIGKILL(t *testing.T) {
	origTimeout := gracefulShutdownTimeout
	gracefulShutdownTimeout = 150 * time.Millisecond
	defer func() { gracefulShutdownTimeout = origTimeout }()

	tmp := t.TempDir()
	// Script that deliberately ignores SIGINT
	script := `#!/bin/sh
trap '' INT
while true; do
    sleep 0.05
done
`
	scriptPath := filepath.Join(tmp, "stubborn.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write script: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := execCommandContext(ctx, scriptPath)

	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start failed: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	start := time.Now()
	cancel()

	err := cmd.Wait()
	elapsed := time.Since(start)

	if err == nil {
		t.Errorf("expected error from killed process, got nil")
	}

	// Must have waited at least the bounded grace period before SIGKILL
	if elapsed < 100*time.Millisecond {
		t.Errorf("process terminated too quickly (%v), expected escalation after %v", elapsed, gracefulShutdownTimeout)
	}
}

func TestContextCancellation_TerminatesEntireProcessGroup(t *testing.T) {
	origTimeout := gracefulShutdownTimeout
	gracefulShutdownTimeout = 100 * time.Millisecond
	defer func() { gracefulShutdownTimeout = origTimeout }()

	tmp := t.TempDir()
	pidFile := filepath.Join(tmp, "child.pid")

	// Script spawns a background child process in the same process group,
	// writes child's PID to a file, and waits.
	script := fmt.Sprintf(`#!/bin/sh
sleep 60 &
echo $! > %s
wait
`, pidFile)

	scriptPath := filepath.Join(tmp, "spawn_group.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write script: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := execCommandContext(ctx, scriptPath)

	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start failed: %v", err)
	}

	// Wait for child PID file to be written
	var childPID int
	for i := 0; i < 50; i++ {
		time.Sleep(50 * time.Millisecond)
		data, err := os.ReadFile(pidFile)
		if err == nil && len(strings.TrimSpace(string(data))) > 0 {
			childPID, _ = strconv.Atoi(strings.TrimSpace(string(data)))
			if childPID > 0 {
				break
			}
		}
	}

	if childPID <= 0 {
		cancel()
		_ = cmd.Wait()
		t.Fatalf("failed to obtain child PID from script")
	}

	// Verify child process is running
	if err := syscall.Kill(childPID, 0); err != nil {
		cancel()
		_ = cmd.Wait()
		t.Fatalf("child process %d is not alive before cancellation: %v", childPID, err)
	}

	// Cancel context to trigger group termination
	cancel()
	_ = cmd.Wait()

	// Give a moment for signal propagation and process reaping
	deadline := time.Now().Add(1 * time.Second)
	childStillAlive := true
	for time.Now().Before(deadline) {
		if err := syscall.Kill(childPID, 0); err != nil {
			childStillAlive = false
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	if childStillAlive {
		t.Errorf("child process %d in process group was not terminated upon context cancellation (orphaned zombie)", childPID)
		_ = syscall.Kill(childPID, syscall.SIGKILL)
	}
}
