//go:build windows

package transcoder

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"

	"github.com/will25u/slimmedia-agent/internal/ffmpeg"
)

// openNoFollow opens a file read-only, refusing to follow a symlink or other
// reparse point as the final path component. Windows has no O_NOFOLLOW flag;
// instead the handle is opened with FILE_FLAG_OPEN_REPARSE_POINT so the open
// binds to the reparse point itself rather than its target. Reading through
// that handle fails (a symlink has no file payload), so a swapped-in link is
// refused exactly as on Unix.
func openNoFollow(path string) (*os.File, error) {
	return ffmpeg.OpenNoFollow(path)
}

// createNoFollow creates or truncates a file, refusing to follow a symlink
// as the final path component. CREATE_ALWAYS with OPEN_REPARSE_POINT would
// open the link itself for writing, so a pre-existing reparse point is
// refused up front instead.
func createNoFollow(path string) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("refusing to open symlink %s", path)
	}
	return createFileReparsePoint(path, windows.GENERIC_WRITE, windows.CREATE_ALWAYS)
}

func createFileReparsePoint(path string, access, creation uint32) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p,
		access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		creation,
		windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}
