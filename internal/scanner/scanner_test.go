package scanner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/will25u/slimmedia-agent/internal/ffmpeg"
)

func TestScanMedia(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "scannertest_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	subDir := filepath.Join(tempDir, "movies")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("Failed to create sub dir: %v", err)
	}

	// Create sample dummy media files
	file1 := filepath.Join(tempDir, "test1.mp4")
	if err := os.WriteFile(file1, []byte("fake mp4 data content"), 0644); err != nil {
		t.Fatalf("Failed to write file1: %v", err)
	}

	file2 := filepath.Join(subDir, "Inception.2010.mkv")
	if err := os.WriteFile(file2, []byte("fake mkv data content"), 0644); err != nil {
		t.Fatalf("Failed to write file2: %v", err)
	}

	// Create non-media file
	file3 := filepath.Join(tempDir, "notes.txt")
	if err := os.WriteFile(file3, []byte("text data"), 0644); err != nil {
		t.Fatalf("Failed to write file3: %v", err)
	}

	// Test Walk
	files, err := Walk(tempDir)
	if err != nil {
		t.Fatalf("Walk failed: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("Expected 2 media files, got %d", len(files))
	}

	// Test ScanMedia (with empty ffprobePath, falls back to file stat)
	scanned, err := ScanMedia(tempDir, "")
	if err != nil {
		t.Fatalf("ScanMedia failed: %v", err)
	}
	if len(scanned) != 2 {
		t.Fatalf("Expected 2 scanned files, got %d", len(scanned))
	}

	for _, item := range scanned {
		if item.Filename != "test1.mp4" && item.Filename != "Inception.2010.mkv" {
			t.Errorf("Unexpected filename: %s", item.Filename)
		}
		if item.SizeBytes <= 0 {
			t.Errorf("Expected positive file size, got %d", item.SizeBytes)
		}
		if item.Hash == "" {
			t.Errorf("Expected non-empty hash for %s", item.Filename)
		}
	}
}

func TestDirectoryTraversal(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "traversal_test_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	deepDir := filepath.Join(tempDir, "shows", "season1", "extras")
	if err := os.MkdirAll(deepDir, 0755); err != nil {
		t.Fatalf("Failed to create deep dir: %v", err)
	}

	f1 := filepath.Join(tempDir, "root.mp4")
	f2 := filepath.Join(tempDir, "shows", "s01e01.mkv")
	f3 := filepath.Join(deepDir, "featurette.avi")

	for _, f := range []string{f1, f2, f3} {
		if err := os.WriteFile(f, []byte("media_content"), 0644); err != nil {
			t.Fatalf("Failed to write file %s: %v", f, err)
		}
	}

	found, err := Walk(tempDir)
	if err != nil {
		t.Fatalf("Walk failed: %v", err)
	}
	if len(found) != 3 {
		t.Fatalf("Expected 3 files across nested hierarchy, got %d", len(found))
	}
}

func TestExtensionFiltering(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "filter_test_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Valid extensions
	valid := []string{"video.mkv", "movie.mp4", "clip.avi", "show.mov", "rec.m4v", "stream.webm", "UPPER.MKV", "UPPER.MP4"}
	for _, f := range valid {
		if err := os.WriteFile(filepath.Join(tempDir, f), []byte("media"), 0644); err != nil {
			t.Fatalf("failed to write %s: %v", f, err)
		}
	}

	// Ignored extensions
	ignored := []string{"readme.txt", "movie.nfo", "poster.jpg", "sub.srt", "app.exe", ".DS_Store", "data.json"}
	for _, f := range ignored {
		if err := os.WriteFile(filepath.Join(tempDir, f), []byte("ignore"), 0644); err != nil {
			t.Fatalf("failed to write %s: %v", f, err)
		}
	}

	found, err := Walk(tempDir)
	if err != nil {
		t.Fatalf("Walk failed: %v", err)
	}

	if len(found) != len(valid) {
		t.Fatalf("Expected exactly %d valid media files, got %d", len(valid), len(found))
	}

	for _, f := range found {
		ext := filepath.Ext(f)
		if ext == ".txt" || ext == ".nfo" || ext == ".jpg" || ext == ".srt" || ext == ".exe" || ext == ".json" {
			t.Errorf("Unexpected non-media file included: %s", f)
		}
	}
}

func TestFingerprintRejectsSymlink(t *testing.T) {
	tempDir := t.TempDir()
	realFile := filepath.Join(tempDir, "real.mp4")
	if err := os.WriteFile(realFile, []byte("media payload"), 0644); err != nil {
		t.Fatalf("failed to write real file: %v", err)
	}
	symlink := filepath.Join(tempDir, "link.mp4")
	if err := os.Symlink(realFile, symlink); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	if _, err := Fingerprint(symlink); err == nil {
		t.Fatalf("Fingerprint on symlink should fail with safe path pinning error, got nil")
	}
}

func TestXXHashConsistency(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "xxhash_test_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Case 1: Small file (<1MB)
	smallFile := filepath.Join(tempDir, "small.mp4")
	contentSmall := []byte("identical small content payload 123456789")
	if err := os.WriteFile(smallFile, contentSmall, 0644); err != nil {
		t.Fatalf("failed to write small file: %v", err)
	}

	hash1, err := Fingerprint(smallFile)
	if err != nil {
		t.Fatalf("Fingerprint small file failed: %v", err)
	}
	hash2, err := Fingerprint(smallFile)
	if err != nil {
		t.Fatalf("Fingerprint small file second run failed: %v", err)
	}
	if hash1 != hash2 {
		t.Fatalf("Hash mismatch on identical small file: %s vs %s", hash1, hash2)
	}

	// Case 2: Larger file (>1MB, between 1MB and 2MB)
	mediumFile := filepath.Join(tempDir, "medium.mkv")
	contentMedium := make([]byte, 1500*1024) // 1.5MB
	for i := range contentMedium {
		contentMedium[i] = byte(i % 256)
	}
	if err := os.WriteFile(mediumFile, contentMedium, 0644); err != nil {
		t.Fatalf("failed to write medium file: %v", err)
	}

	mHash1, err := Fingerprint(mediumFile)
	if err != nil {
		t.Fatalf("Fingerprint medium file failed: %v", err)
	}
	mHash2, err := Fingerprint(mediumFile)
	if err != nil {
		t.Fatalf("Fingerprint medium file repeat failed: %v", err)
	}
	if mHash1 != mHash2 {
		t.Fatalf("Hash mismatch on identical medium file: %s vs %s", mHash1, mHash2)
	}

	// Case 3: Large file (>2MB)
	largeFile := filepath.Join(tempDir, "large.mkv")
	contentLarge := make([]byte, 2500*1024) // 2.5MB
	for i := range contentLarge {
		contentLarge[i] = byte((i * 7) % 256)
	}
	if err := os.WriteFile(largeFile, contentLarge, 0644); err != nil {
		t.Fatalf("failed to write large file: %v", err)
	}

	lHash1, err := Fingerprint(largeFile)
	if err != nil {
		t.Fatalf("Fingerprint large file failed: %v", err)
	}
	lHash2, err := Fingerprint(largeFile)
	if err != nil {
		t.Fatalf("Fingerprint large file repeat failed: %v", err)
	}
	if lHash1 != lHash2 {
		t.Fatalf("Hash mismatch on identical large file: %s vs %s", lHash1, lHash2)
	}

	// Ensure distinct files have distinct fingerprints
	if hash1 == mHash1 || hash1 == lHash1 || mHash1 == lHash1 {
		t.Fatalf("Expected different files to have different fingerprints: small=%s, med=%s, large=%s", hash1, mHash1, lHash1)
	}

	// Error on non-existent file
	_, err = Fingerprint(filepath.Join(tempDir, "does_not_exist.mkv"))
	if err == nil {
		t.Fatalf("expected error on non-existent file, got nil")
	}
}

func TestWalk_RootNotFound(t *testing.T) {
	_, err := Walk(filepath.Join(t.TempDir(), "nonexistent"))
	if err == nil {
		t.Fatal("expected error when walking non-existent root, got nil")
	}
}

func TestWalkStream(t *testing.T) {
	tempDir := t.TempDir()

	for i := 0; i < 5; i++ {
		p := filepath.Join(tempDir, "file"+string(rune('0'+i))+".mkv")
		if err := os.WriteFile(p, []byte("media"), 0644); err != nil {
			t.Fatalf("failed to write file: %v", err)
		}
	}
	// Non-media file
	if err := os.WriteFile(filepath.Join(tempDir, "ignore.txt"), []byte("text"), 0644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	ch := make(chan string, 2)
	go func() {
		defer close(ch)
		if err := WalkStream(context.Background(), tempDir, ch); err != nil {
			t.Errorf("WalkStream failed: %v", err)
		}
	}()

	var streamed []string
	for p := range ch {
		streamed = append(streamed, p)
	}
	if len(streamed) != 5 {
		t.Fatalf("expected 5 streamed media files, got %d", len(streamed))
	}
}

func TestWalkStream_ContextCancellation(t *testing.T) {
	tempDir := t.TempDir()

	for i := 0; i < 10; i++ {
		p := filepath.Join(tempDir, "file"+string(rune('0'+i))+".mkv")
		if err := os.WriteFile(p, []byte("media"), 0644); err != nil {
			t.Fatalf("failed to write file: %v", err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	// Unbuffered channel so walking blocks on sending second item
	ch := make(chan string)

	errChan := make(chan error, 1)
	go func() {
		errChan <- WalkStream(ctx, tempDir, ch)
	}()

	// Read one item, then cancel context
	<-ch
	cancel()

	err := <-errChan
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestScanMediaWithProgressBoundedChannel(t *testing.T) {
	tempDir := t.TempDir()

	for i := 0; i < 6; i++ {
		p := filepath.Join(tempDir, "stream"+string(rune('0'+i))+".mp4")
		if err := os.WriteFile(p, []byte("mp4 content"), 0644); err != nil {
			t.Fatalf("failed to write file: %v", err)
		}
	}

	var probedCount atomic.Int64
	var observedTotals atomic.Int64
	results, err := ScanMediaWithProgress(tempDir, "", func(file string, info ffmpeg.MediaInfo, current int, max int, completed int, total int) {
		probedCount.Add(1)
		if total == 6 {
			observedTotals.Add(1)
		}
	})
	if err != nil {
		t.Fatalf("ScanMediaWithProgress failed: %v", err)
	}
	if len(results) != 6 {
		t.Fatalf("expected 6 scanned files, got %d", len(results))
	}
	if probedCount.Load() != 6 {
		t.Fatalf("expected 6 callbacks, got %d", probedCount.Load())
	}
	if observedTotals.Load() != 6 {
		t.Fatalf("expected all 6 callbacks to observe total == 6, got %d", observedTotals.Load())
	}
	for _, res := range results {
		if res.RelativePath == "" {
			t.Fatalf("expected non-empty relative_path for file %s", res.FilePath)
		}
		if filepath.IsAbs(res.RelativePath) {
			t.Fatalf("expected relative_path to be relative, got %s", res.RelativePath)
		}
	}
}

func TestCountMediaFiles(t *testing.T) {
	tempDir := t.TempDir()

	for i := 0; i < 4; i++ {
		p := filepath.Join(tempDir, "video"+string(rune('0'+i))+".mkv")
		if err := os.WriteFile(p, []byte("mkv"), 0644); err != nil {
			t.Fatalf("failed to write mkv: %v", err)
		}
	}
	// Write a non-media file that should be ignored
	if err := os.WriteFile(filepath.Join(tempDir, "notes.txt"), []byte("text"), 0644); err != nil {
		t.Fatalf("failed to write txt: %v", err)
	}

	count, err := CountMediaFiles(context.Background(), tempDir)
	if err != nil {
		t.Fatalf("CountMediaFiles failed: %v", err)
	}
	if count != 4 {
		t.Fatalf("expected count 4, got %d", count)
	}
}

func TestNormalizeRelativePath(t *testing.T) {
	tests := []struct {
		name        string
		root        string
		file        string
		wantRel     string
		expectError bool
	}{
		{
			name:    "simple linux path",
			root:    "/mnt/media",
			file:    "/mnt/media/Movies/Elf.mkv",
			wantRel: "Movies/Elf.mkv",
		},
		{
			name:    "nested linux path with special characters",
			root:    "/mnt/media",
			file:    "/mnt/media/Movies/Christmas/Elf (2003)/Elf.mkv",
			wantRel: "Movies/Christmas/Elf (2003)/Elf.mkv",
		},
		{
			name:    "simulated windows path with backslashes",
			root:    `C:\Media`,
			file:    `C:\Media\Show\Episode.mkv`,
			wantRel: "Show/Episode.mkv",
		},
		{
			name:    "simulated windows path with lowercase drive and trailing slash",
			root:    `c:\media\`,
			file:    `C:\Media\Movies\Action\Die Hard (1988).mkv`,
			wantRel: "Movies/Action/Die Hard (1988).mkv",
		},
		{
			name:        "parent directory traversal rejected",
			root:        "/mnt/media",
			file:        "/mnt/media/../etc/passwd",
			expectError: true,
		},
		{
			name:        "windows traversal rejected",
			root:        `C:\Media`,
			file:        `C:\Media\..\Windows\System32\cmd.exe`,
			expectError: true,
		},
		{
			name:        "completely outside path rejected",
			root:        "/mnt/media",
			file:        "/var/log/syslog",
			expectError: true,
		},
		{
			name:        "root itself rejected",
			root:        "/mnt/media",
			file:        "/mnt/media",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rel, err := NormalizeRelativePath(tt.root, tt.file)
			if tt.expectError {
				if err == nil {
					t.Fatalf("expected error for root=%q, file=%q, got nil (rel=%q)", tt.root, tt.file, rel)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if rel != tt.wantRel {
				t.Fatalf("NormalizeRelativePath(%q, %q) = %q, want %q", tt.root, tt.file, rel, tt.wantRel)
			}
			// Verify forward slash only
			if strings.Contains(rel, "\\") {
				t.Fatalf("relative path %q contains backslashes", rel)
			}
		})
	}
}


