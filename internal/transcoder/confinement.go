package transcoder

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/will25u/slimmedia-agent/internal/ffmpeg"
)

// ScannedLibrary is the set of canonical absolute paths discovered by the
// agent's local media scanner. The orchestrator refuses to process a job
// whose input path is not a member, so a compromised server (or a leaked
// agent key) cannot point the agent at arbitrary host files.
type ScannedLibrary interface {
	Contains(canonicalPath string) (bool, error)
}

// CanonicalMediaPath resolves a server-supplied path to the canonical
// absolute path of the local file it refers to. It rejects anything that is
// not an existing regular local file: remote URLs, ffmpeg protocol
// references, flag-like strings, and dangling or non-regular paths.
func CanonicalMediaPath(p string) (string, error) {
	if err := ffmpeg.CheckLocalMediaPath(p); err != nil {
		return "", err
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("cannot make path absolute: %w", err)
	}
	cleaned := filepath.Clean(abs)
	resolved, err := filepath.EvalSymlinks(cleaned)
	if err != nil {
		return "", fmt.Errorf("cannot resolve path %q: %w", cleaned, err)
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		return "", fmt.Errorf("cannot stat resolved path %q: %w", resolved, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("resolved path %q is not a regular file", resolved)
	}
	f, err := ffmpeg.OpenNoFollow(resolved)
	if err != nil {
		return "", fmt.Errorf("resolved path %q cannot be opened with safe path pinning: %w", resolved, err)
	}
	f.Close()
	return resolved, nil
}
