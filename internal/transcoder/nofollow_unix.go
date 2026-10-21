//go:build unix

package transcoder

import (
	"os"
	"syscall"

	"github.com/will25u/slimmedia-agent/internal/ffmpeg"
)

// openNoFollow opens a file read-only, refusing to follow a symlink as the
// final path component (ELOOP instead).
func openNoFollow(path string) (*os.File, error) {
	return ffmpeg.OpenNoFollow(path)
}

// createNoFollow creates or truncates a file, refusing to follow a symlink
// as the final path component.
func createNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0644)
}
