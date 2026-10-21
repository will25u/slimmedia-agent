//go:build unix

package ffmpeg

import (
	"os"
	"syscall"
)

// OpenNoFollow opens a file read-only with safe path pinning, refusing to
// follow a symlink as the final path component (returning ELOOP instead).
func OpenNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}
