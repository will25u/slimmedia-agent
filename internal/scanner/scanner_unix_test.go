//go:build !windows

package scanner

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func TestWalk_InaccessibleSubfolderResilience(t *testing.T) {
	if os.Getenv("TEST_UNPRIVILEGED_WALK") == "1" {
		targetDir := os.Getenv("TEST_WALK_DIR")
		files, err := Walk(targetDir)
		if err != nil {
			t.Fatalf("Walk failed on inaccessible subfolder: %v", err)
		}
		if len(files) != 1 {
			t.Fatalf("expected 1 file, got %d: %v", len(files), files)
		}
		return
	}

	tempDir := t.TempDir()
	_ = os.Chmod(filepath.Dir(tempDir), 0755)
	_ = os.Chmod(tempDir, 0755)

	validFile := filepath.Join(tempDir, "valid.mp4")
	if err := os.WriteFile(validFile, []byte("valid content"), 0644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	restrictedDir := filepath.Join(tempDir, "restricted")
	if err := os.Mkdir(restrictedDir, 0755); err != nil {
		t.Fatalf("failed to mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(restrictedDir, "inaccessible.mkv"), []byte("data"), 0644); err != nil {
		t.Fatalf("failed to write restricted file: %v", err)
	}
	if err := os.Chmod(restrictedDir, 0000); err != nil {
		t.Fatalf("failed to chmod: %v", err)
	}
	defer os.Chmod(restrictedDir, 0755)

	if os.Geteuid() == 0 {
		// When running as root, copy test binary so unprivileged user can execute it
		testBin := filepath.Join(tempDir, "scanner.test")
		input, err := os.ReadFile(os.Args[0])
		if err != nil {
			t.Fatalf("failed to read test binary: %v", err)
		}
		if err := os.WriteFile(testBin, input, 0755); err != nil {
			t.Fatalf("failed to write test binary: %v", err)
		}
		defer os.Remove(testBin)

		cmd := exec.Command(testBin, "-test.run=^TestWalk_InaccessibleSubfolderResilience$")
		cmd.Env = append(os.Environ(), "TEST_UNPRIVILEGED_WALK=1", "TEST_WALK_DIR="+tempDir)
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Credential: &syscall.Credential{Uid: 65534, Gid: 65534},
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("subprocess failed: %v, output: %s", err, string(out))
		}
	} else {
		files, err := Walk(tempDir)
		if err != nil {
			t.Fatalf("Walk failed on inaccessible subfolder: %v", err)
		}
		if len(files) != 1 {
			t.Fatalf("expected 1 file, got %d: %v", len(files), files)
		}
	}
}
