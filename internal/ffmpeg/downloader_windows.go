//go:build windows

package ffmpeg

// pinnedFFmpegURL is a versioned, immutable gyan.dev release package (the
// rolling ffmpeg-release-essentials.zip redirects here), so the embedded
// checksum stays meaningful. The essentials build ships the hardware
// encoder support (NVENC, QSV, AMF) the tier probe relies on.
const pinnedFFmpegURL = "https://www.gyan.dev/ffmpeg/builds/packages/ffmpeg-9.0.2-essentials_build.zip"

// pinnedFFmpegChecksum is the SHA-256 of the pinned zip above.
const pinnedFFmpegChecksum = "60f467265b1e312373dbcd92200c2618a74850f98d3d078e94296bb3fa2047ba"

const downloaderArchiveExt = ".zip"

const binarySuffix = ".exe"

// extractAndInstall extracts ffmpeg.exe and ffprobe.exe from the pinned
// gyan.dev zip using Go's archive/zip — no external tar/7z binary is
// required on Windows. The gyan.dev zip does not ship qt-faststart, so only
// the two binaries the engine actually invokes are required.
func extractAndInstall(archivePath, destDir string) error {
	return extractZipBinaries(archivePath, destDir, []string{
		"ffmpeg" + binarySuffix,
		"ffprobe" + binarySuffix,
	})
}
