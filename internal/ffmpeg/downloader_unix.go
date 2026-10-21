//go:build unix

package ffmpeg

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// pinnedFFmpegURL is a versioned, immutable release artifact (no rolling
// "latest" URL) so the embedded checksum stays meaningful. The BtbN GPL
// build is used because it ships hardware encoders (NVENC, QSV, AMF) —
// the johnvansickle static builds are CPU-only, which would leave hardware
// transcode agents permanently undetected.
const pinnedFFmpegURL = "https://github.com/BtbN/FFmpeg-Builds/releases/download/autobuild-2026-09-14-13-17/ffmpeg-n8.1.2-52-g5a03dfa0f6-linux64-gpl-8.1.tar.xz"

// pinnedFFmpegChecksum is the SHA-256 of the pinned tarball above.
const pinnedFFmpegChecksum = "0a0c3002405807439bf558fab62f70ba4c958ff1fef8cb2676be75a9aab2e0b4"

const downloaderArchiveExt = ".tar.xz"

const binarySuffix = ""

// tarExtractTimeout bounds the maximum duration permitted for archive unpacking.
var tarExtractTimeout = 2 * time.Minute

// extractAndInstall extracts the pinned tarball into a private temp dir with
// ownership and permission bits from the archive neutralized, then installs
// only the binaries we use with explicit permissions.
func extractAndInstall(archivePath, destDir string) error {
	tmpDir, err := os.MkdirTemp(destDir, ".ffmpeg-extract-*")
	if err != nil {
		return fmt.Errorf("failed to create extract dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	ctx, cancel := context.WithTimeout(context.Background(), tarExtractTimeout)
	defer cancel()

	cmd := execCommandContext(ctx, "tar", "-xf", archivePath, "-C", tmpDir,
		"--strip-components=1", "--no-same-owner", "--no-same-permissions")
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("tar extraction timed out: %w", ctx.Err())
		}
		return fmt.Errorf("failed to extract: %w", err)
	}

	for _, name := range binaryNames {
		// Layout differs between upstream builds: some place binaries at the
		// archive root, others (e.g. BtbN) under a bin/ directory.
		src := filepath.Join(tmpDir, name)
		if _, err := os.Stat(src); err != nil {
			src = filepath.Join(tmpDir, "bin", name)
			if _, err := os.Stat(src); err != nil {
				continue
			}
		}
		if err := installBinary(src, destDir, name); err != nil {
			return err
		}
	}

	return nil
}
