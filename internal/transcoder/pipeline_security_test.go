package transcoder

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/will25u/slimmedia-agent/internal/jobpaths"
)

// If the original is swapped for a symlink before the replace sequence
// runs, the symlink must be refused — not followed into the quarantine
// (arbitrary read) or overwritten (arbitrary write).
func TestReplaceSequence_RejectsSymlinkedOriginal(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "pipeline_symlink_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	scratchDir := filepath.Join(tempDir, "scratch")
	quarantineDir := filepath.Join(tempDir, "quarantine")
	mediaDir := filepath.Join(tempDir, "media")
	os.MkdirAll(mediaDir, 0755)

	p := NewPipeline(scratchDir, quarantineDir, 0)

	origFile := filepath.Join(mediaDir, "movie.mkv")
	if err := os.WriteFile(origFile, []byte("ORIGINAL_CONTENT"), 0644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	sensitiveFile := filepath.Join(tempDir, "sensitive.txt")
	sensitiveContent := []byte("TOP_SECRET_DO_NOT_TOUCH")
	if err := os.WriteFile(sensitiveFile, sensitiveContent, 0644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	paths := jobpaths.New(scratchDir, quarantineDir, origFile)
	if err := os.WriteFile(paths.Scratch(), []byte("TRANSCODED_CONTENT"), 0644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	// Attacker swaps the job target for a symlink to a sensitive file
	// between scan and replace.
	if err := os.Remove(origFile); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	if err := os.Symlink(sensitiveFile, origFile); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	err = p.ReplaceSequence(paths)
	if err == nil {
		t.Fatalf("expected ReplaceSequence to refuse symlinked original")
	}

	// The sensitive file must be byte-identical.
	got, err := os.ReadFile(sensitiveFile)
	if err != nil {
		t.Fatalf("sensitive file unreadable: %v", err)
	}
	if string(got) != string(sensitiveContent) {
		t.Fatalf("sensitive file was modified: %q", got)
	}

	// The symlink itself must still be in place (os.Remove never follows
	// it, and nothing was placed over it).
	fi, err := os.Lstat(origFile)
	if err != nil {
		t.Fatalf("orig path missing: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected orig path to still be the symlink")
	}

	// Nothing may have landed in quarantine either.
	entries, _ := os.ReadDir(quarantineDir)
	if len(entries) != 0 {
		t.Fatalf("expected empty quarantine, found %d entries", len(entries))
	}
}

// A symlink swapped in as the quarantine destination must not be followed
// into an overwrite of its target.
func TestCopyFile_RejectsSymlinkEnds(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "pipeline_copy_symlink_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	p := NewPipeline(tempDir, tempDir, 0)

	src := filepath.Join(tempDir, "src.bin")
	if err := os.WriteFile(src, []byte("payload"), 0644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	sensitive := filepath.Join(tempDir, "sensitive.bin")
	if err := os.WriteFile(sensitive, []byte("SECRET"), 0644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	link := filepath.Join(tempDir, "dst.bin")
	if err := os.Symlink(sensitive, link); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	if err := p.CopyFile(src, link); err == nil {
		t.Fatalf("expected CopyFile to refuse symlink destination")
	}
	got, _ := os.ReadFile(sensitive)
	if string(got) != "SECRET" {
		t.Fatalf("symlink target was overwritten: %q", got)
	}

	if err := p.CopyFile(link, filepath.Join(tempDir, "out.bin")); err == nil {
		t.Fatalf("expected CopyFile to refuse symlink source")
	}
}

// SHA-256 verification must catch corruption that keeps the file size
// identical — the old size-only comparison let this pass and the original
// was deleted anyway.
func TestVerifyFileHash_SameSizeCorruption(t *testing.T) {
	tempDir := t.TempDir()
	a := filepath.Join(tempDir, "a.bin")
	b := filepath.Join(tempDir, "b.bin")
	content := []byte("IDENTICAL_SIZE_DIFFERENT_BYTES!")
	if err := os.WriteFile(a, content, 0644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	corrupted := make([]byte, len(content))
	for i := range corrupted {
		corrupted[i] = content[i] ^ 0xFF
	}
	if err := os.WriteFile(b, corrupted, 0644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	stA, _ := os.Stat(a)
	stB, _ := os.Stat(b)
	if stA.Size() != stB.Size() {
		t.Fatalf("test requires same-size files")
	}

	hashA, err := hashFileNoFollow(a)
	if err != nil {
		t.Fatalf("hash failed: %v", err)
	}
	if err := VerifyFileHash(b, hashA); err == nil {
		t.Fatalf("expected same-size corruption to be detected")
	}
	if err := VerifyFileHash(a, hashA); err != nil {
		t.Fatalf("expected identical file to verify, got: %v", err)
	}
}

func TestReplaceSequence_RejectsHardlinkSwap(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "pipeline_hardlink_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	scratchDir := filepath.Join(tempDir, "scratch")
	quarantineDir := filepath.Join(tempDir, "quarantine")
	mediaDir := filepath.Join(tempDir, "media")
	os.MkdirAll(mediaDir, 0755)

	p := NewPipeline(scratchDir, quarantineDir, 0)

	origFile := filepath.Join(mediaDir, "movie.mkv")
	sensitiveFile := filepath.Join(tempDir, "sensitive.txt")
	sensitiveContent := []byte("TOP_SECRET_DO_NOT_TOUCH")
	if err := os.WriteFile(sensitiveFile, sensitiveContent, 0644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	// Create a large file so copyAndHash takes some time
	largeContent := make([]byte, 1024*1024*50) // 50MB
	if err := os.WriteFile(origFile, largeContent, 0644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	paths := jobpaths.New(scratchDir, quarantineDir, origFile)
	if err := os.WriteFile(paths.Scratch(), []byte("TRANSCODED_CONTENT"), 0644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	errCh := make(chan error)
	go func() {
		errCh <- p.ReplaceSequence(paths)
	}()

	// Wait a tiny bit for copyAndHash to open the file and start copying
	time.Sleep(5 * time.Millisecond)

	// Swap the original file for a hardlink to sensitive file
	os.Remove(origFile)
	if err := os.Link(sensitiveFile, origFile); err != nil {
		t.Logf("failed to create hardlink, test might not race correctly: %v", err)
	}

	err = <-errCh
	if err == nil {
		t.Fatalf("expected ReplaceSequence to refuse swapped hardlinked original")
	}
}
