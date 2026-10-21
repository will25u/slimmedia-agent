package transcoder

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/will25u/slimmedia-agent/internal/jobpaths"
)

// Two files that share a basename in different directories must get
// distinct quarantine (and scratch) slots — otherwise the second
// transcode overwrites the first's only backup.
func TestReplaceSequence_SameBasenameDistinctSlots(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "pipeline_collision_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	scratchDir := filepath.Join(tempDir, "scratch")
	quarantineDir := filepath.Join(tempDir, "quarantine")
	dirA := filepath.Join(tempDir, "media", "a")
	dirB := filepath.Join(tempDir, "media", "b")
	os.MkdirAll(dirA, 0755)
	os.MkdirAll(dirB, 0755)

	p := NewPipeline(scratchDir, quarantineDir, 0)

	mkFile := func(dir, name string, content []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, content, 0644); err != nil {
			t.Fatalf("setup failed: %v", err)
		}
		return path
	}
	mkPaths := func(origPath string, content []byte) jobpaths.JobPaths {
		paths := jobpaths.New(scratchDir, quarantineDir, origPath)
		if err := os.WriteFile(paths.Scratch(), content, 0644); err != nil {
			t.Fatalf("setup failed: %v", err)
		}
		return paths
	}

	// Slot names must differ for the same basename in different dirs.
	pathsA := jobpaths.New(scratchDir, quarantineDir, mkFile(dirA, "movie.mkv", []byte("CONTENT_A")))
	pathsB := jobpaths.New(scratchDir, quarantineDir, mkFile(dirB, "movie.mkv", []byte("CONTENT_B")))
	if pathsA.Scratch() == pathsB.Scratch() || pathsA.Quarantine() == pathsB.Quarantine() {
		t.Fatalf("expected distinct slots for same-basename files, got %q", pathsA.Scratch())
	}

	// Run the full sequence for A, then B; both originals must land in
	// quarantine intact.
	origA := filepath.Join(dirA, "movie.mkv")
	origB := filepath.Join(dirB, "movie.mkv")
	if err := p.ReplaceSequence(mkPaths(origA, []byte("TRANSCODED_A"))); err != nil {
		t.Fatalf("ReplaceSequence A failed: %v", err)
	}
	if err := p.ReplaceSequence(mkPaths(origB, []byte("TRANSCODED_B"))); err != nil {
		t.Fatalf("ReplaceSequence B failed: %v", err)
	}

	entries, err := os.ReadDir(quarantineDir)
	if err != nil {
		t.Fatalf("failed to read quarantine dir: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 quarantine entries, got %d", len(entries))
	}

	quarantineA, err := os.ReadFile(pathsA.Quarantine())
	if err != nil {
		t.Fatalf("quarantine A missing: %v", err)
	}
	quarantineB, err := os.ReadFile(pathsB.Quarantine())
	if err != nil {
		t.Fatalf("quarantine B missing: %v", err)
	}
	if string(quarantineA) != "CONTENT_A" || string(quarantineB) != "CONTENT_B" {
		t.Fatalf("quarantine contents corrupted: A=%q B=%q", quarantineA, quarantineB)
	}

	// Replacements in place.
	replacedA, _ := os.ReadFile(origA)
	replacedB, _ := os.ReadFile(origB)
	if string(replacedA) != "TRANSCODED_A" || string(replacedB) != "TRANSCODED_B" {
		t.Fatalf("replacement contents wrong: A=%q B=%q", replacedA, replacedB)
	}
}
