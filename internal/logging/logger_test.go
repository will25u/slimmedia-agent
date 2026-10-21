package logging

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRotatingFile_WriteAndRotate(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "log_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	logPath := filepath.Join(tempDir, "test.log")
	// Set small max size (50 bytes) to force rotation
	rf, err := NewRotatingFile(logPath, 50, 24*time.Hour)
	if err != nil {
		t.Fatalf("failed to create rotating file: %v", err)
	}
	defer rf.Close()

	// Write 40 bytes
	data1 := []byte("1234567890123456789012345678901234567890")
	if _, err := rf.Write(data1); err != nil {
		t.Fatalf("first write failed: %v", err)
	}

	// Write 40 more bytes - should trigger rotate
	data2 := []byte("abcdefghijklmnopqrstuvwxyz0123456789abcd")
	if _, err := rf.Write(data2); err != nil {
		t.Fatalf("second write failed: %v", err)
	}

	files, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatalf("failed to read dir: %v", err)
	}

	// Should have original test.log plus at least one rotated file
	if len(files) < 2 {
		t.Errorf("expected at least 2 log files after rotation, found %d", len(files))
	}
}

func TestRotatingFile_Permissions(t *testing.T) {
	t.Run("new file is 0640 and dir is 0750", func(t *testing.T) {
		logDir := filepath.Join(t.TempDir(), "logs")
		rf, err := NewRotatingFile(filepath.Join(logDir, "test.log"), 0, 0)
		if err != nil {
			t.Fatalf("failed to create rotating file: %v", err)
		}
		defer rf.Close()

		info, err := os.Stat(filepath.Join(logDir, "test.log"))
		if err != nil {
			t.Fatalf("stat log file: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0640 {
			t.Fatalf("log file mode = %o, want 0640", perm)
		}

		dirInfo, err := os.Stat(logDir)
		if err != nil {
			t.Fatalf("stat log dir: %v", err)
		}
		if perm := dirInfo.Mode().Perm(); perm != 0750 {
			t.Fatalf("log dir mode = %o, want 0750", perm)
		}
	})

	t.Run("pre-existing world-readable file tightened", func(t *testing.T) {
		logDir := t.TempDir()
		logPath := filepath.Join(logDir, "legacy.log")
		// Simulate a log created by an older build (0644).
		if err := os.WriteFile(logPath, []byte("old"), 0644); err != nil {
			t.Fatalf("write legacy log: %v", err)
		}

		rf, err := NewRotatingFile(logPath, 0, 0)
		if err != nil {
			t.Fatalf("failed to open rotating file: %v", err)
		}
		defer rf.Close()

		info, err := os.Stat(logPath)
		if err != nil {
			t.Fatalf("stat log file: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0640 {
			t.Fatalf("log file mode = %o after open, want 0640", perm)
		}
	})
}

func TestRotatingFile_RapidSameSecondRotations(t *testing.T) {
	tempDir := t.TempDir()
	logPath := filepath.Join(tempDir, "rapid.log")

	// Max size 10 bytes so each write forces a rotation
	rf, err := NewRotatingFile(logPath, 10, 24*time.Hour)
	if err != nil {
		t.Fatalf("failed to create rotating file: %v", err)
	}
	defer rf.Close()

	// Perform 5 rapid writes in immediate succession
	for i := 0; i < 5; i++ {
		if _, err := rf.Write([]byte("0123456789ABCDEF")); err != nil {
			t.Fatalf("write %d failed: %v", i, err)
		}
	}

	entries, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}

	// Should have the active log file plus 4 or 5 rotated archive files
	if len(entries) < 5 {
		t.Fatalf("expected at least 5 log files from rapid rotations, got %d", len(entries))
	}
}

