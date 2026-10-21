package ffmpeg

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// extractZipBinaries extracts the named binaries (matched by archive entry
// base name, e.g. "ffmpeg.exe") from the zip at zipPath into destDir with
// explicit executable permissions, and returns an error naming any of them
// that the archive did not contain — a pinned artifact that fails to yield
// ffmpeg.exe must not look like a successful install. Entries are matched by
// base name only and written to destDir/<name>, so a crafted archive path
// cannot escape destDir (zip-slip neutralized).
//
// This file is platform-independent so the Windows extraction path is
// unit-testable on the Linux CI runner.
func extractZipBinaries(zipPath, destDir string, names []string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("failed to open zip: %w", err)
	}
	defer r.Close()

	remaining := make(map[string]bool, len(names))
	for _, n := range names {
		remaining[n] = true
	}

	for _, f := range r.File {
		base := filepath.Base(f.Name)
		if !remaining[base] {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("failed to read %s from zip: %w", f.Name, err)
		}
		dst := filepath.Join(destDir, base)
		os.Remove(dst)
		out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0755)
		if err != nil {
			rc.Close()
			return fmt.Errorf("failed to create %s: %w", dst, err)
		}
		_, copyErr := io.Copy(out, rc)
		closeErr := out.Close()
		rc.Close()
		if copyErr != nil {
			return fmt.Errorf("failed to extract %s: %w", base, copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("failed to finalize %s: %w", dst, closeErr)
		}
		delete(remaining, base)
	}
	if len(remaining) > 0 {
		missing := make([]string, 0, len(remaining))
		for name := range remaining {
			missing = append(missing, name)
		}
		sort.Strings(missing)
		return fmt.Errorf("archive is missing required binaries: %s", strings.Join(missing, ", "))
	}
	return nil
}
