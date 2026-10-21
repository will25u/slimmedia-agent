package transcoder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/will25u/slimmedia-agent/internal/jobpaths"
)

func TestReplaceSequence_Success(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "pipeline_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	scratchDir := filepath.Join(tempDir, "scratch")
	quarantineDir := filepath.Join(tempDir, "quarantine")
	mediaDir := filepath.Join(tempDir, "media")
	os.MkdirAll(mediaDir, 0755)

	p := NewPipeline(scratchDir, quarantineDir, 1024*1024*100)

	origFile := filepath.Join(mediaDir, "movie.mkv")
	origContent := []byte("ORIGINAL_FILE_CONTENT_VIDEO_STREAM")
	if err := os.WriteFile(origFile, origContent, 0644); err != nil {
		t.Fatalf("failed to create orig file: %v", err)
	}

	paths := jobpaths.New(scratchDir, quarantineDir, origFile)
	scratchFile := paths.Scratch()
	scratchContent := []byte("NEW_TRANSCODED_FILE_CONTENT_STREAM")
	if err := os.WriteFile(scratchFile, scratchContent, 0644); err != nil {
		t.Fatalf("failed to create scratch file: %v", err)
	}

	// Execute ReplaceSequence
	if err := p.ReplaceSequence(paths); err != nil {
		t.Fatalf("ReplaceSequence failed: %v", err)
	}

	// 1. Original file path should now contain the new transcoded content
	updatedContent, err := os.ReadFile(origFile)
	if err != nil {
		t.Fatalf("failed to read updated orig file: %v", err)
	}
	if string(updatedContent) != string(scratchContent) {
		t.Errorf("expected orig file to have new content %q, got %q", string(scratchContent), string(updatedContent))
	}

	// 2. Scratch file should be cleaned up
	if _, err := os.Stat(scratchFile); !os.IsNotExist(err) {
		t.Errorf("expected scratch file to be deleted, but it exists")
	}

	// 3. Quarantine copy should exist (namespaced by canonical path) and have original content
	quarantineCopy := paths.Quarantine()
	qContent, err := os.ReadFile(quarantineCopy)
	if err != nil {
		t.Fatalf("failed to read quarantine file: %v", err)
	}
	if string(qContent) != string(origContent) {
		t.Errorf("expected quarantine file to match original content")
	}
}

func TestReplaceSequence_PreservationOfOriginalOnFailure(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "pipeline_fail_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	scratchDir := filepath.Join(tempDir, "scratch")
	quarantineDir := filepath.Join(tempDir, "quarantine")
	mediaDir := filepath.Join(tempDir, "media")
	os.MkdirAll(mediaDir, 0755)

	p := NewPipeline(scratchDir, quarantineDir, 1024*1024*100)

	origFile := filepath.Join(mediaDir, "important_movie.mkv")
	origContent := []byte("PRESERVE_ME_CRITICAL_ORIGINAL_CONTENT")
	if err := os.WriteFile(origFile, origContent, 0644); err != nil {
		t.Fatalf("failed to create orig file: %v", err)
	}

	// Specify a scratch path whose file does not exist to simulate failure during Step 6 (PLACE NEW)
	paths := jobpaths.New(scratchDir, quarantineDir, origFile)

	err = p.ReplaceSequence(paths)
	if err == nil {
		t.Fatalf("expected ReplaceSequence to fail when scratch does not exist, got nil")
	}

	// Verification: Original file MUST still exist with original content preserved via recovery
	restoredContent, err := os.ReadFile(origFile)
	if err != nil {
		t.Fatalf("original file was lost or unreadable after failure: %v", err)
	}
	if string(restoredContent) != string(origContent) {
		t.Fatalf("original content was corrupted: expected %q, got %q", string(origContent), string(restoredContent))
	}
}

func TestVerifySize(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "size_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	p := NewPipeline(tempDir, tempDir, 0)

	origPath := filepath.Join(tempDir, "orig.bin")
	origSize := 1000
	if err := os.WriteFile(origPath, make([]byte, origSize), 0644); err != nil {
		t.Fatalf("failed to create orig file: %v", err)
	}

	cases := []struct {
		name      string
		size      int
		shouldErr bool
	}{
		{"Valid 50%", 500, false},
		{"Valid Min Boundary 5%", 50, false},
		{"Valid Max Boundary 110%", 1100, false},
		{"Invalid Below Min 4%", 40, true},
		{"Invalid Above Max 115%", 1150, true},
		{"Invalid Zero Bytes", 0, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			newPath := filepath.Join(tempDir, "new_"+tc.name+".bin")
			if err := os.WriteFile(newPath, make([]byte, tc.size), 0644); err != nil {
				t.Fatalf("failed to create new file: %v", err)
			}

			err := p.VerifySize(origPath, newPath)
			if tc.shouldErr && err == nil {
				t.Errorf("expected error for size %d, got nil", tc.size)
			}
			if !tc.shouldErr && err != nil {
				t.Errorf("expected success for size %d, got error: %v", tc.size, err)
			}
			if tc.name == "Invalid Above Max 115%" && err != nil {
				expectedSub := "larger than original"
				if !strings.Contains(err.Error(), expectedSub) {
					t.Errorf("expected error to contain %q, got: %v", expectedSub, err)
				}
			}
			if tc.name == "Invalid Below Min 4%" && err != nil {
				expectedSub := "suspiciously small"
				if !strings.Contains(err.Error(), expectedSub) {
					t.Errorf("expected error to contain %q, got: %v", expectedSub, err)
				}
			}
		})
	}
}

func TestFormatBytes(t *testing.T) {
	cases := []struct {
		bytes    int64
		expected string
	}{
		{500, "500 B"},
		{1024, "1.00 KB"},
		{1024 * 1024, "1.00 MB"},
		{1024 * 1024 * 1024, "1.00 GB"},
		{1024 * 1024 * 1024 * 1024, "1.00 TB"},
		{1024 * 1024 * 1024 * 1024 * 1024, "1.00 PB"},
	}

	for _, tc := range cases {
		actual := formatBytes(tc.bytes)
		if actual != tc.expected {
			t.Errorf("formatBytes(%d) = %q; want %q", tc.bytes, actual, tc.expected)
		}
	}
}

func TestCopyFile(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "copy_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	p := NewPipeline(tempDir, tempDir, 0)

	src := filepath.Join(tempDir, "src.bin")
	dst := filepath.Join(tempDir, "dst.bin")
	data := []byte("test_copy_payload_123")

	if err := os.WriteFile(src, data, 0644); err != nil {
		t.Fatalf("failed to write src: %v", err)
	}

	if err := p.CopyFile(src, dst); err != nil {
		t.Fatalf("CopyFile failed: %v", err)
	}

	read, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("failed to read dst: %v", err)
	}
	if string(read) != string(data) {
		t.Errorf("copied content mismatch: got %q, want %q", string(read), string(data))
	}

	// Non-existent source
	if err := p.CopyFile(filepath.Join(tempDir, "missing.bin"), dst); err == nil {
		t.Errorf("expected error copying non-existent source, got nil")
	}
}

func TestCheckBudget(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "budget_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	quarantineDir := filepath.Join(tempDir, "quarantine")
	os.MkdirAll(quarantineDir, 0755)

	// Create a file of 200 bytes
	filePath := filepath.Join(quarantineDir, "sample.bin")
	if err := os.WriteFile(filePath, make([]byte, 200), 0644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	// Pipeline with budget of 100 bytes (should exceed budget if disk space check passes)
	p := NewPipeline(tempDir, quarantineDir, 100)
	err = p.CheckBudget()
	if err == nil {
		t.Fatalf("expected budget error, got nil")
	}
	// Error could be budget exceeded or critical disk space (<50GB on test machine), both are valid safety checks
	if !strings.Contains(err.Error(), "quarantine budget exceeded") && !strings.Contains(err.Error(), "critical disk space") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestCheckBudgetDualVolume(t *testing.T) {
	tempDir := t.TempDir()
	scratchDir := filepath.Join(tempDir, "scratch")
	quarantineDir := filepath.Join(tempDir, "quarantine")
	destDir := filepath.Join(tempDir, "dest")
	_ = os.MkdirAll(scratchDir, 0755)
	_ = os.MkdirAll(quarantineDir, 0755)
	_ = os.MkdirAll(destDir, 0755)

	p := NewPipeline(scratchDir, quarantineDir, 1024*1024*1024)

	// CheckBudget with explicit destination directory checks both scratch and destination
	err := p.CheckBudget(destDir)
	// On machines with < 50GB free space, it should report critical disk space on either volume
	if err != nil && !strings.Contains(err.Error(), "critical disk space") {
		t.Fatalf("unexpected error from CheckBudget: %v", err)
	}

	// Non-existent destination path inside existing directory resolves to parent directory
	err = p.CheckBudget(filepath.Join(destDir, "nested", "media.mkv"))
	if err != nil && !strings.Contains(err.Error(), "critical disk space") {
		t.Fatalf("unexpected error from CheckBudget with nested non-existent path: %v", err)
	}
}


func TestApplyPlacementAndDisposition_Alongside_Keep(t *testing.T) {
	tempDir := t.TempDir()
	scratchDir := filepath.Join(tempDir, "scratch")
	quarantineDir := filepath.Join(tempDir, "quarantine")
	mediaDir := filepath.Join(tempDir, "media")
	_ = os.MkdirAll(mediaDir, 0755)

	p := NewPipeline(scratchDir, quarantineDir, 100*1024*1024)

	origFile := filepath.Join(mediaDir, "show.mkv")
	origBytes := []byte("SOURCE_FILE_DATA")
	if err := os.WriteFile(origFile, origBytes, 0644); err != nil {
		t.Fatal(err)
	}

	paths := jobpaths.New(scratchDir, quarantineDir, origFile)
	scratchBytes := []byte("TRANSCODED_NEW_DATA")
	if err := os.WriteFile(paths.Scratch(), scratchBytes, 0644); err != nil {
		t.Fatal(err)
	}

	placedPath, err := p.ApplyPlacementAndDisposition(paths, "alongside", "keep", "")
	if err != nil {
		t.Fatalf("ApplyPlacementAndDisposition failed: %v", err)
	}

	wantPlaced := filepath.Join(mediaDir, "show.transcoded.mkv")
	if placedPath != wantPlaced {
		t.Errorf("placedPath = %q, want %q", placedPath, wantPlaced)
	}

	// 1. Placed file has new content
	placedContent, err := os.ReadFile(placedPath)
	if err != nil || string(placedContent) != string(scratchBytes) {
		t.Errorf("placed file content mismatch: %v", err)
	}

	// 2. Original file is kept untouched
	origContent, err := os.ReadFile(origFile)
	if err != nil || string(origContent) != string(origBytes) {
		t.Errorf("original file was unexpectedly modified or deleted: %v", err)
	}

	// 3. Scratch file is cleaned up
	if _, err := os.Stat(paths.Scratch()); !os.IsNotExist(err) {
		t.Errorf("expected scratch to be deleted")
	}
}

func TestApplyPlacementAndDisposition_Alongside_Delete(t *testing.T) {
	tempDir := t.TempDir()
	scratchDir := filepath.Join(tempDir, "scratch")
	quarantineDir := filepath.Join(tempDir, "quarantine")
	mediaDir := filepath.Join(tempDir, "media")
	_ = os.MkdirAll(mediaDir, 0755)

	p := NewPipeline(scratchDir, quarantineDir, 100*1024*1024)

	origFile := filepath.Join(mediaDir, "movie.mkv")
	if err := os.WriteFile(origFile, []byte("ORIGINAL_DATA"), 0644); err != nil {
		t.Fatal(err)
	}

	paths := jobpaths.New(scratchDir, quarantineDir, origFile)
	if err := os.WriteFile(paths.Scratch(), []byte("TRANSCODED_DATA"), 0644); err != nil {
		t.Fatal(err)
	}

	placedPath, err := p.ApplyPlacementAndDisposition(paths, "alongside", "delete", "")
	if err != nil {
		t.Fatalf("failed: %v", err)
	}

	// 1. Placed file exists
	if _, err := os.Stat(placedPath); err != nil {
		t.Errorf("placed file missing: %v", err)
	}

	// 2. Original file deleted
	if _, err := os.Stat(origFile); !os.IsNotExist(err) {
		t.Errorf("expected original file to be deleted")
	}
}

func TestApplyPlacementAndDisposition_Alongside_Trash(t *testing.T) {
	tempDir := t.TempDir()
	scratchDir := filepath.Join(tempDir, "scratch")
	quarantineDir := filepath.Join(tempDir, "quarantine")
	mediaDir := filepath.Join(tempDir, "media")
	_ = os.MkdirAll(mediaDir, 0755)

	p := NewPipeline(scratchDir, quarantineDir, 100*1024*1024)

	origFile := filepath.Join(mediaDir, "movie.mkv")
	origBytes := []byte("SOURCE_FILE_ORIGINAL")
	if err := os.WriteFile(origFile, origBytes, 0644); err != nil {
		t.Fatal(err)
	}

	paths := jobpaths.New(scratchDir, quarantineDir, origFile)
	if err := os.WriteFile(paths.Scratch(), []byte("NEW_DATA"), 0644); err != nil {
		t.Fatal(err)
	}

	placedPath, err := p.ApplyPlacementAndDisposition(paths, "alongside", "trash", "")
	if err != nil {
		t.Fatalf("failed: %v", err)
	}

	// 1. Placed file exists
	if _, err := os.Stat(placedPath); err != nil {
		t.Errorf("placed file missing: %v", err)
	}

	// 2. Original file moved from source
	if _, err := os.Stat(origFile); !os.IsNotExist(err) {
		t.Errorf("expected original file removed from source")
	}

	// 3. Quarantined file exists with original bytes
	qBytes, err := os.ReadFile(paths.Quarantine())
	if err != nil || string(qBytes) != string(origBytes) {
		t.Errorf("expected quarantine file to exist with original bytes: %v", err)
	}
}

func TestApplyPlacementAndDisposition_Replace_Delete(t *testing.T) {
	tempDir := t.TempDir()
	scratchDir := filepath.Join(tempDir, "scratch")
	quarantineDir := filepath.Join(tempDir, "quarantine")
	mediaDir := filepath.Join(tempDir, "media")
	_ = os.MkdirAll(mediaDir, 0755)

	p := NewPipeline(scratchDir, quarantineDir, 100*1024*1024)

	origFile := filepath.Join(mediaDir, "movie.mkv")
	if err := os.WriteFile(origFile, []byte("ORIGINAL_CONTENT"), 0644); err != nil {
		t.Fatal(err)
	}

	paths := jobpaths.New(scratchDir, quarantineDir, origFile)
	newBytes := []byte("REPLACED_CONTENT")
	if err := os.WriteFile(paths.Scratch(), newBytes, 0644); err != nil {
		t.Fatal(err)
	}

	placedPath, err := p.ApplyPlacementAndDisposition(paths, "replace", "delete", "")
	if err != nil {
		t.Fatalf("failed: %v", err)
	}
	if placedPath != origFile {
		t.Errorf("placedPath = %q, want %q", placedPath, origFile)
	}

	// 1. Original file path replaced with new content
	read, err := os.ReadFile(origFile)
	if err != nil || string(read) != string(newBytes) {
		t.Errorf("replaced content mismatch: %v", err)
	}

	// 2. Quarantine backup was purged because action is delete
	if _, err := os.Stat(paths.Quarantine()); !os.IsNotExist(err) {
		t.Errorf("expected quarantine backup to be purged when action=delete")
	}
}

func TestApplyPlacementAndDisposition_Alternate_Keep(t *testing.T) {
	tempDir := t.TempDir()
	scratchDir := filepath.Join(tempDir, "scratch")
	quarantineDir := filepath.Join(tempDir, "quarantine")
	mediaDir := filepath.Join(tempDir, "media")
	_ = os.MkdirAll(mediaDir, 0755)

	p := NewPipeline(scratchDir, quarantineDir, 100*1024*1024)

	origFile := filepath.Join(mediaDir, "clip.mkv")
	origBytes := []byte("SOURCE_CLIP")
	if err := os.WriteFile(origFile, origBytes, 0644); err != nil {
		t.Fatal(err)
	}

	paths := jobpaths.New(scratchDir, quarantineDir, origFile)
	newBytes := []byte("ENCODED_CLIP")
	if err := os.WriteFile(paths.Scratch(), newBytes, 0644); err != nil {
		t.Fatal(err)
	}

	placedPath, err := p.ApplyPlacementAndDisposition(paths, "alternate", "keep", "mp4")
	if err != nil {
		t.Fatalf("failed: %v", err)
	}

	wantPlaced := filepath.Join(tempDir, "quarantine", "transcoded", "clip.mp4")
	if filepath.Clean(placedPath) != filepath.Clean(wantPlaced) {
		t.Errorf("placedPath = %q, want %q", placedPath, wantPlaced)
	}

	// 1. Placed file in alternate directory has new content
	read, err := os.ReadFile(placedPath)
	if err != nil || string(read) != string(newBytes) {
		t.Errorf("alternate file content mismatch: %v", err)
	}

	// 2. Source file kept
	sRead, err := os.ReadFile(origFile)
	if err != nil || string(sRead) != string(origBytes) {
		t.Errorf("source file altered: %v", err)
	}
}

func TestApplyPlacementAndDisposition_EmitsProgress(t *testing.T) {
	tempDir := t.TempDir()
	scratchDir := filepath.Join(tempDir, "scratch")
	quarantineDir := filepath.Join(tempDir, "quarantine")
	mediaDir := filepath.Join(tempDir, "media")
	_ = os.MkdirAll(mediaDir, 0755)

	p := NewPipeline(scratchDir, quarantineDir, 100*1024*1024)

	origFile := filepath.Join(mediaDir, "clip.mkv")
	origBytes := []byte("ORIGINAL_CLIP_DATA_1234567890")
	if err := os.WriteFile(origFile, origBytes, 0644); err != nil {
		t.Fatal(err)
	}

	paths := jobpaths.New(scratchDir, quarantineDir, origFile)
	newBytes := []byte("TRANSCODED_CLIP_DATA_0987654321")
	if err := os.WriteFile(paths.Scratch(), newBytes, 0644); err != nil {
		t.Fatal(err)
	}

	type progEvent struct {
		subPhase    string
		activity    string
		transferred int64
		total       int64
		pct         float64
	}
	var events []progEvent
	cb := func(subPhase, activity string, transferred, total int64, pct float64) {
		events = append(events, progEvent{
			subPhase:    subPhase,
			activity:    activity,
			transferred: transferred,
			total:       total,
			pct:         pct,
		})
	}

	placedPath, err := p.ApplyPlacementAndDisposition(paths, "replace", "archive", "", cb)
	if err != nil {
		t.Fatalf("ApplyPlacementAndDisposition failed: %v", err)
	}
	if placedPath != origFile {
		t.Errorf("placedPath = %q, want %q", placedPath, origFile)
	}

	// Verify events were emitted for quarantine backup, scratch verify, and destination verify
	if len(events) == 0 {
		t.Fatal("expected placement progress events, got none")
	}

	subPhasesSeen := make(map[string]bool)
	for _, ev := range events {
		subPhasesSeen[ev.subPhase] = true
		if ev.total <= 0 {
			t.Errorf("expected positive total bytes in event %+v", ev)
		}
		if ev.pct < 0 || ev.pct > 100 {
			t.Errorf("percentage out of range in event %+v", ev)
		}
	}

	if !subPhasesSeen["quarantine_backup"] {
		t.Error("expected quarantine_backup events")
	}
	if !subPhasesSeen["quarantine_verify"] {
		t.Error("expected quarantine_verify events")
	}
	if !subPhasesSeen["scratch_verify"] {
		t.Error("expected scratch_verify events")
	}
	if !subPhasesSeen["destination_verify"] {
		t.Error("expected destination_verify events")
	}
}

