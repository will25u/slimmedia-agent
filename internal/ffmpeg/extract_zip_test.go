package ffmpeg

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func writeTestZip(t *testing.T, entries map[string][]byte) string {
	t.Helper()
	buf := new(bytes.Buffer)
	zw := zip.NewWriter(buf)
	// Deterministic order for tests that overwrite by base name.
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create zip entry: %v", err)
		}
		if _, err := w.Write(entries[name]); err != nil {
			t.Fatalf("write zip entry: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	zipPath := filepath.Join(t.TempDir(), "test.zip")
	if err := os.WriteFile(zipPath, buf.Bytes(), 0644); err != nil {
		t.Fatalf("write zip file: %v", err)
	}
	return zipPath
}

func TestExtractZipBinariesInstallsMatchedEntries(t *testing.T) {
	zipPath := writeTestZip(t, map[string][]byte{
		"ffmpeg-9.0.2-essentials_build/bin/ffmpeg.exe":  []byte("ffmpeg-bytes"),
		"ffmpeg-9.0.2-essentials_build/bin/ffprobe.exe": []byte("ffprobe-bytes"),
		"ffmpeg-9.0.2-essentials_build/doc/index.html":  []byte("docs"),
	})
	destDir := t.TempDir()

	if err := extractZipBinaries(zipPath, destDir, []string{"ffmpeg.exe", "ffprobe.exe"}); err != nil {
		t.Fatalf("extractZipBinaries: %v", err)
	}

	for name, want := range map[string]string{
		"ffmpeg.exe":  "ffmpeg-bytes",
		"ffprobe.exe": "ffprobe-bytes",
	} {
		got, err := os.ReadFile(filepath.Join(destDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("%s content = %q, want %q", name, got, want)
		}
		info, err := os.Stat(filepath.Join(destDir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if info.IsDir() {
			t.Errorf("%s is a directory, want regular file", name)
		}
	}

	// Non-matching entries must not be extracted.
	if _, err := os.Stat(filepath.Join(destDir, "index.html")); !os.IsNotExist(err) {
		t.Errorf("index.html should not have been extracted")
	}
}

func TestExtractZipBinariesFailsWhenRequiredBinaryMissing(t *testing.T) {
	zipPath := writeTestZip(t, map[string][]byte{
		"pkg/bin/ffmpeg.exe": []byte("ffmpeg-bytes"),
	})
	destDir := t.TempDir()

	// A pinned artifact that does not yield ffprobe.exe must surface an
	// error, not a silently broken install.
	if err := extractZipBinaries(zipPath, destDir, []string{"ffmpeg.exe", "ffprobe.exe"}); err == nil {
		t.Fatalf("expected an error for the missing ffprobe.exe, got nil")
	}
}

func TestExtractZipBinariesCannotEscapeDestDir(t *testing.T) {
	zipPath := writeTestZip(t, map[string][]byte{
		"../evil.exe": []byte("evil"),
	})
	destDir := t.TempDir()
	outside := filepath.Join(destDir, "..", "evil.exe")

	if err := extractZipBinaries(zipPath, destDir, []string{"evil.exe"}); err != nil {
		t.Fatalf("extractZipBinaries: %v", err)
	}
	// The entry is installed under destDir by base name, not at its crafted path.
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Errorf("zip entry escaped destDir: %s exists", outside)
	}
	if got, err := os.ReadFile(filepath.Join(destDir, "evil.exe")); err != nil || string(got) != "evil" {
		t.Errorf("evil.exe not installed under destDir (content %q, err %v)", got, err)
	}
}
