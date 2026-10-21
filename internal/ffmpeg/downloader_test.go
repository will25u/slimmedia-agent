package ffmpeg

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// buildTestTar returns a gzipped tar (auto-detected by tar -xf) holding the
// given path/content pairs.
func buildTestTar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name,
			Mode: 0644,
			Size: int64(len(content)),
		}); err != nil {
			t.Fatalf("tar write failed: %v", err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("tar write failed: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close failed: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close failed: %v", err)
	}
	return buf.Bytes()
}

func TestDownloaderRequiresChecksum(t *testing.T) {
	d := Downloader{
		URL:     "https://example.invalid/ffmpeg.tar.xz",
		DestDir: t.TempDir(),
	}
	if err := d.Download(); err == nil {
		t.Fatalf("expected refusal to download without a checksum")
	}
}

func TestDownloaderChecksumMismatch(t *testing.T) {
	payload := buildTestTar(t, map[string]string{"ffmpeg-1/ffmpeg": "binary"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer srv.Close()

	d := Downloader{
		URL:      srv.URL,
		Checksum: hex.EncodeToString(sha256.New().Sum(nil)), // wrong on purpose
		DestDir:  t.TempDir(),
	}
	if err := d.Download(); err == nil {
		t.Fatalf("expected checksum mismatch error")
	}
}

func TestDownloaderInstallsWithNeutralizedTar(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tar-based downloader not supported on windows")
	}
	payload := buildTestTar(t, map[string]string{
		"ffmpeg-6.0.1-amd64-static/ffmpeg":     "#!/bin/sh\necho ffmpeg",
		"ffmpeg-6.0.1-amd64-static/ffprobe":    "#!/bin/sh\necho ffprobe",
		"ffmpeg-6.0.1-amd64-static/README.txt": "not installed",
	})
	sum := sha256.Sum256(payload)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer srv.Close()

	destDir := t.TempDir()
	d := Downloader{
		URL:      srv.URL,
		Checksum: hex.EncodeToString(sum[:]),
		DestDir:  destDir,
	}
	if err := d.Download(); err != nil {
		t.Fatalf("Download failed: %v", err)
	}

	// Binaries installed with explicit 0755.
	for _, name := range []string{"ffmpeg", "ffprobe"} {
		info, err := os.Stat(filepath.Join(destDir, name))
		if err != nil {
			t.Fatalf("%s was not installed: %v", name, err)
		}
		if info.Mode().Perm() != 0755 {
			t.Fatalf("%s has permissions %o, want 0755", name, info.Mode().Perm())
		}
	}

	// Archive litter (README) and the tarball are not left behind.
	if _, err := os.Stat(filepath.Join(destDir, "README.txt")); !os.IsNotExist(err) {
		t.Fatalf("README.txt should not be installed")
	}
	if _, err := os.Stat(filepath.Join(destDir, "ffmpeg-static.tar.xz")); !os.IsNotExist(err) {
		t.Fatalf("tarball should be deleted after install")
	}
	entries, err := os.ReadDir(destDir)
	if err != nil {
		t.Fatalf("failed to read dest dir: %v", err)
	}
	if len(entries) != 2 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected exactly ffmpeg+ffprobe in dest dir, found: %v", names)
	}
}

func TestDownloaderInstallsBinSubdirLayout(t *testing.T) {
	// BtbN-style archives place binaries under a bin/ directory after
	// --strip-components=1, rather than at the archive root.
	if runtime.GOOS == "windows" {
		t.Skip("tar-based downloader not supported on windows")
	}
	payload := buildTestTar(t, map[string]string{
		"pkg/bin/ffmpeg":  "#!/bin/sh\necho ffmpeg",
		"pkg/bin/ffprobe": "#!/bin/sh\necho ffprobe",
	})
	sum := sha256.Sum256(payload)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer srv.Close()

	destDir := t.TempDir()
	d := Downloader{
		URL:      srv.URL,
		Checksum: hex.EncodeToString(sum[:]),
		DestDir:  destDir,
	}
	if err := d.Download(); err != nil {
		t.Fatalf("Download failed: %v", err)
	}

	for _, name := range []string{"ffmpeg", "ffprobe"} {
		if _, err := os.Stat(filepath.Join(destDir, name)); err != nil {
			t.Fatalf("%s was not installed from bin/ layout: %v", name, err)
		}
	}
}

func TestEnsureFFmpegSkipsWhenPresent(t *testing.T) {
	destDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(destDir, "ffprobe"+binarySuffix), []byte("x"), 0755); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	// URL points nowhere; success proves no download was attempted.
	if err := EnsureFFmpeg(destDir); err != nil {
		t.Fatalf("EnsureFFmpeg should be a no-op when ffprobe exists: %v", err)
	}
}

func TestResolveBinaryPath(t *testing.T) {
	destDir := t.TempDir()
	dummy := filepath.Join(destDir, "ffmpeg"+binarySuffix)
	if err := os.WriteFile(dummy, []byte("x"), 0755); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	got := ResolveBinaryPath(destDir, "ffmpeg")
	if got != dummy {
		t.Fatalf("ResolveBinaryPath = %q, want %q", got, dummy)
	}
}

func TestHWEncodersPresent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake ffmpeg not supported on windows")
	}
	dir := t.TempDir()

	// Fake ffmpeg listing a hardware encoder (compile-time property).
	hw := filepath.Join(dir, "hw-ffmpeg")
	if err := os.WriteFile(hw, []byte("#!/bin/sh\necho ' V..... h264_nvenc             NVIDIA NVENC H.264 encoder'\n"), 0755); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	if !hwEncodersPresent(hw) {
		t.Error("hwEncodersPresent = false for binary listing h264_nvenc, want true")
	}

	// Fake CPU-only ffmpeg (johnvansickle-style output).
	cpu := filepath.Join(dir, "cpu-ffmpeg")
	if err := os.WriteFile(cpu, []byte("#!/bin/sh\necho ' V..... libx264             libx264 H.264 encoder'\n"), 0755); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	if hwEncodersPresent(cpu) {
		t.Error("hwEncodersPresent = true for CPU-only binary, want false")
	}

	// Missing/unrunnable binary.
	if hwEncodersPresent(filepath.Join(dir, "no-such-ffmpeg")) {
		t.Error("hwEncodersPresent = true for missing binary, want false")
	}
}

func TestDownloaderPinnedConstants(t *testing.T) {
	// Guard against accidentally reintroducing a rolling release URL or an
	// empty checksum pin. The pin must target an immutable release artifact
	// (BtbN date-stamped autobuild tag), never the rolling "latest" tag.
	if pinnedFFmpegURL == "" || pinnedFFmpegChecksum == "" {
		t.Fatalf("pinned URL and checksum must both be set")
	}
	if !bytes.Contains([]byte(pinnedFFmpegURL), []byte("/releases/download/")) {
		t.Fatalf("pinned URL must be an immutable release artifact, got: %s", pinnedFFmpegURL)
	}
	if bytes.Contains([]byte(pinnedFFmpegURL), []byte("/latest/")) {
		t.Fatalf("pinned URL must not use the rolling \"latest\" tag, got: %s", pinnedFFmpegURL)
	}
	if _, err := hex.DecodeString(pinnedFFmpegChecksum); err != nil || len(pinnedFFmpegChecksum) != 64 {
		t.Fatalf("pinned checksum is not a hex SHA-256: %q", pinnedFFmpegChecksum)
	}
}

func TestDownloaderExceedsSizeLimit(t *testing.T) {
	origLimit := maxArchiveBytes
	maxArchiveBytes = 32
	t.Cleanup(func() { maxArchiveBytes = origLimit })

	destDir := t.TempDir()

	t.Run("content length header exceeds limit", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		d := Downloader{
			URL:      srv.URL,
			Checksum: hex.EncodeToString(sha256.New().Sum(nil)),
			DestDir:  destDir,
		}
		err := d.Download()
		if err == nil || !strings.Contains(err.Error(), "exceeds maximum allowed size") {
			t.Fatalf("expected size limit error, got %v", err)
		}
		entries, _ := os.ReadDir(destDir)
		if len(entries) != 0 {
			t.Fatalf("archive file left behind after size limit failure: %v", entries)
		}
	})

	t.Run("stream body exceeds limit", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Write 100 bytes without Content-Length
			w.Write([]byte(strings.Repeat("a", 100)))
		}))
		defer srv.Close()

		d := Downloader{
			URL:      srv.URL,
			Checksum: hex.EncodeToString(sha256.New().Sum(nil)),
			DestDir:  destDir,
		}
		err := d.Download()
		if err == nil || !strings.Contains(err.Error(), "exceeds maximum allowed size") {
			t.Fatalf("expected size limit error, got %v", err)
		}
		entries, _ := os.ReadDir(destDir)
		if len(entries) != 0 {
			t.Fatalf("archive file left behind after size limit failure: %v", entries)
		}
	})
}

func TestDownloaderStalledExtractionAbortsCleanly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tar-based stalled extraction test applies to unix")
	}

	origTimeout := tarExtractTimeout
	tarExtractTimeout = 100 * time.Millisecond
	t.Cleanup(func() { tarExtractTimeout = origTimeout })

	origGraceful := gracefulShutdownTimeout
	gracefulShutdownTimeout = 50 * time.Millisecond
	t.Cleanup(func() { gracefulShutdownTimeout = origGraceful })

	fakeBinDir := t.TempDir()
	// Fake tar that stalls and ignores SIGINT so escalation is triggered
	fakeTar := filepath.Join(fakeBinDir, "tar")
	script := `#!/bin/sh
trap '' INT
while true; do
	sleep 0.05
done
`
	if err := os.WriteFile(fakeTar, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write fake tar: %v", err)
	}

	t.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	payload := buildTestTar(t, map[string]string{
		"ffmpeg-1/ffmpeg": "binary",
	})
	sum := sha256.Sum256(payload)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer srv.Close()

	destDir := t.TempDir()
	d := Downloader{
		URL:      srv.URL,
		Checksum: hex.EncodeToString(sum[:]),
		DestDir:  destDir,
	}

	start := time.Now()
	err := d.Download()
	elapsed := time.Since(start)

	if err == nil || (!strings.Contains(err.Error(), "timed out") && !strings.Contains(err.Error(), "deadline exceeded")) {
		t.Fatalf("expected timeout error, got %v", err)
	}

	if elapsed > 3*time.Second {
		t.Fatalf("stalled extraction took too long to abort: %v", elapsed)
	}

	// Verify no temporary extraction dirs or archives leaked
	entries, err := os.ReadDir(destDir)
	if err != nil {
		t.Fatalf("failed to read destDir: %v", err)
	}
	if len(entries) != 0 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected empty destDir, found leftover files/dirs: %v", names)
	}
}
