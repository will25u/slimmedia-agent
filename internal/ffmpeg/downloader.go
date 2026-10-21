package ffmpeg

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The pinned download URL and SHA-256 live in the per-GOOS files
// (downloader_unix.go, downloader_windows.go): Linux fetches a versioned
// BtbN tarball, Windows a versioned gyan.dev zip.

// downloaderArchiveExt is the archive file extension used for the downloaded
// artifact on this platform (with the leading dot); defined per-GOOS in
// downloader_unix.go / downloader_windows.go.

// binaryNames lists the archive member (base) names installed into DestDir,
// in priority order. Windows skips members absent from the gyan.dev zip.
var binaryNames = []string{"ffmpeg", "ffprobe", "qt-faststart"}

// maxArchiveBytes caps the size of downloaded ffmpeg archives (512 MiB) to prevent
// disk-exhaustion attacks from an oversized response.
var maxArchiveBytes int64 = 512 * 1024 * 1024 // 512 MiB

type Downloader struct {
	URL      string
	Checksum string
	DestDir  string
}

func (d *Downloader) Download() error {
	if d.Checksum == "" {
		return fmt.Errorf("refusing to download without a checksum")
	}

	archivePath := filepath.Join(d.DestDir, "ffmpeg-static"+downloaderArchiveExt)
	// Never leave the archive behind, whatever happens.
	defer os.Remove(archivePath)

	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Get(d.URL)
	if err != nil {
		return fmt.Errorf("failed to download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bad status: %s", resp.Status)
	}

	if resp.ContentLength > maxArchiveBytes {
		return fmt.Errorf("archive payload exceeds maximum allowed size (%d bytes)", maxArchiveBytes)
	}

	out, err := os.Create(archivePath)
	if err != nil {
		return fmt.Errorf("failed to create file: %w", err)
	}

	hasher := sha256.New()
	writer := io.MultiWriter(out, hasher)
	limited := io.LimitReader(resp.Body, maxArchiveBytes+1)

	n, err := io.Copy(writer, limited)
	if err != nil {
		out.Close()
		return fmt.Errorf("failed to write file: %w", err)
	}
	if n > maxArchiveBytes {
		out.Close()
		return fmt.Errorf("archive payload exceeds maximum allowed size (%d bytes)", maxArchiveBytes)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("failed to close file: %w", err)
	}

	hash := hex.EncodeToString(hasher.Sum(nil))
	if hash != d.Checksum {
		return fmt.Errorf("checksum mismatch: expected %s, got %s", d.Checksum, hash)
	}

	return extractAndInstall(archivePath, d.DestDir)
}

var ensureFFmpegMu sync.Mutex

// BinaryPath returns the platform-specific path to a binary in destDir.
func BinaryPath(destDir, name string) string {
	return filepath.Join(destDir, name+binarySuffix)
}

// ResolveBinaryPath locates an executable in destDir (with OS extension), then
// PATH, and finally falls back to downloading via EnsureFFmpeg.
func ResolveBinaryPath(destDir, name string) string {
	target := filepath.Join(destDir, name+binarySuffix)
	if _, err := os.Stat(target); err == nil {
		return target
	}
	if binarySuffix != "" {
		if _, err := os.Stat(filepath.Join(destDir, name)); err == nil {
			return filepath.Join(destDir, name)
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	_ = EnsureFFmpeg(destDir)
	if _, err := os.Stat(target); err == nil {
		return target
	}
	return target
}

func EnsureFFmpeg(destDir string) error {
	ensureFFmpegMu.Lock()
	defer ensureFFmpegMu.Unlock()

	ensureVMAFModel(destDir)

	// check if ffprobe exists
	ffprobePath := filepath.Join(destDir, "ffprobe"+binarySuffix)
	if _, err := os.Stat(ffprobePath); err == nil {
		// An ffmpeg binary from an older pin (johnvansickle static) is
		// CPU-only and would permanently hide hardware transcoding. Encoder
		// availability is compile-time, so a binary that lists no hardware
		// encoders is stale regardless of GPU presence — re-download.
		ffmpegPath := filepath.Join(destDir, "ffmpeg"+binarySuffix)
		if _, err := os.Stat(ffmpegPath); err == nil && !hwEncodersPresent(ffmpegPath) {
			// stale build; fall through to re-download
		} else {
			return nil
		}
	}

	dl := Downloader{
		URL:      pinnedFFmpegURL,
		Checksum: pinnedFFmpegChecksum,
		DestDir:  destDir,
	}

	return dl.Download()
}

func ensureVMAFModel(destDir string) {
	if len(defaultVMAFModelJSON) == 0 {
		return
	}
	modelPath := filepath.Join(destDir, "vmaf_v0.6.1.json")
	if _, err := os.Stat(modelPath); os.IsNotExist(err) {
		_ = os.WriteFile(modelPath, defaultVMAFModelJSON, 0644)
	}
}

// hwEncodersPresent reports whether the ffmpeg binary lists hardware
// encoders (compile-time property, independent of GPU presence).
func hwEncodersPresent(ffmpegPath string) bool {
	out, err := execCommand(ffmpegPath, "-hide_banner", "-encoders", "-v", "quiet").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), " h264_nvenc ")
}

// installBinary moves src to destDir/name with explicit executable
// permissions, replacing any previous copy.
func installBinary(src, destDir, name string) error {
	dst := filepath.Join(destDir, name)
	os.Remove(dst)
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("failed to install %s: %w", name, err)
	}
	if err := os.Chmod(dst, 0755); err != nil {
		return fmt.Errorf("failed to set permissions on %s: %w", name, err)
	}
	return nil
}
