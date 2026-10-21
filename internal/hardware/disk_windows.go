//go:build windows

package hardware

import (
	"fmt"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// ProbeDisk returns disk usage for the given path (or every fixed volume if
// path is empty), via the Windows volume APIs.
func ProbeDisk(path string) []DiskInfo {
	if path != "" {
		return []DiskInfo{diskStatFor(path)}
	}

	drives, err := windows.GetLogicalDrives()
	if err != nil {
		return nil
	}
	var disks []DiskInfo
	for i := 0; i < 26; i++ {
		if drives&(1<<i) == 0 {
			continue
		}
		root := fmt.Sprintf("%c:\\", 'A'+i)
		d := diskStatFor(root)
		if d.TotalGB > 0 {
			disks = append(disks, d)
		}
	}
	return disks
}

func diskStatFor(path string) DiskInfo {
	free, total, err := diskSpace(path)
	if err != nil || total == 0 {
		return DiskInfo{Mount: path}
	}
	return DiskInfo{
		Mount:   path,
		TotalGB: float64(total) / (1024 * 1024 * 1024),
		FreeGB:  float64(free) / (1024 * 1024 * 1024),
	}
}

// DiskFreeBytes returns the number of bytes available to the current user on
// the volume containing path.
func DiskFreeBytes(path string) (uint64, error) {
	free, _, err := diskSpace(path)
	return free, err
}

// diskSpace wraps GetDiskFreeSpaceEx, returning (freeBytesAvailableToCaller,
// totalBytes). A relative path is anchored at its volume root, since
// GetDiskFreeSpaceEx operates on volumes, not files.
func diskSpace(path string) (free, total uint64, err error) {
	volume := path
	if len(volume) < 2 || volume[1] != ':' {
		// Not a drive-prefixed path; anchor it so GetDiskFreeSpaceEx gets a
		// path it can resolve to a volume.
		full, err := filepath.Abs(path)
		if err != nil {
			return 0, 0, err
		}
		volume = full
	}
	p, err := windows.UTF16PtrFromString(volume)
	if err != nil {
		return 0, 0, err
	}
	var freeBytes, totalBytes uint64
	if err := windows.GetDiskFreeSpaceEx(p, &freeBytes, &totalBytes, nil); err != nil {
		return 0, 0, err
	}
	return freeBytes, totalBytes, nil
}
