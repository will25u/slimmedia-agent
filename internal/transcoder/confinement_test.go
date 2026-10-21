package transcoder

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCanonicalMediaPath(t *testing.T) {
	dir := t.TempDir()
	realFile := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(realFile, []byte("video"), 0644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	t.Run("resolves regular file to absolute path", func(t *testing.T) {
		got, err := CanonicalMediaPath(realFile)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !filepath.IsAbs(got) || got != filepath.Clean(realFile) {
			t.Fatalf("expected absolute cleaned path, got %q", got)
		}
	})

	t.Run("resolves symlink to canonical target", func(t *testing.T) {
		link := filepath.Join(dir, "link.mkv")
		if err := os.Symlink(realFile, link); err != nil {
			t.Fatalf("setup failed: %v", err)
		}
		got, err := CanonicalMediaPath(link)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != filepath.Clean(realFile) {
			t.Fatalf("expected symlink to resolve to %q, got %q", realFile, got)
		}
	})

	t.Run("rejects nonexistent file", func(t *testing.T) {
		if _, err := CanonicalMediaPath(filepath.Join(dir, "missing.mkv")); err == nil {
			t.Fatalf("expected error for missing file")
		}
	})

	t.Run("rejects directory", func(t *testing.T) {
		if _, err := CanonicalMediaPath(dir); err == nil {
			t.Fatalf("expected error for directory")
		}
	})

	t.Run("rejects remote and protocol paths", func(t *testing.T) {
		for _, p := range []string{
			"http://169.254.169.254/latest/meta-data",
			"https://example.com/movie.mkv",
			"concat:a|b",
			"-show_frames",
			"file:/etc/passwd",
		} {
			if _, err := CanonicalMediaPath(p); err == nil {
				t.Fatalf("expected rejection for %q", p)
			}
		}
	})
}
