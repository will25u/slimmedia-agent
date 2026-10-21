//go:build windows

package ffmpeg

import (
	"os/exec"
	"testing"

	"golang.org/x/sys/windows"
)

func checkSysProcAttr(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr is nil")
	}
	if cmd.SysProcAttr.CreationFlags&windows.CREATE_NEW_PROCESS_GROUP == 0 {
		t.Errorf("expected CREATE_NEW_PROCESS_GROUP on Windows")
	}
}
