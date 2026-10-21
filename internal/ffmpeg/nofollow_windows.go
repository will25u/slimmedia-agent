//go:build windows

package ffmpeg

import (
	"os"

	"golang.org/x/sys/windows"
)

// OpenNoFollow opens a file read-only with safe path pinning, refusing to
// follow a symlink or other reparse point as the final path component.
func OpenNoFollow(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}
