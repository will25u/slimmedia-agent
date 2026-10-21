//go:build linux

package hardware

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// ProbeDisk returns disk usage for the given path (or all mount points if path is empty).
func ProbeDisk(path string) []DiskInfo {
	if path != "" {
		return []DiskInfo{diskStatFor(path)}
	}

	// Read /proc/mounts and stat each physical mount point
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return nil
	}
	defer f.Close()

	seen := make(map[string]bool)
	var disks []DiskInfo
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		dev := fields[0]
		mount := fields[1]
		// Only real block devices
		if !strings.HasPrefix(dev, "/dev/") {
			continue
		}
		// Skip bind-mounts and snap/loop
		if strings.Contains(dev, "loop") {
			continue
		}
		cleanMount := filepath.Clean(mount)
		if seen[cleanMount] {
			continue
		}
		seen[cleanMount] = true
		d := diskStatFor(cleanMount)
		if d.TotalGB > 0 {
			disks = append(disks, d)
		}
	}
	return disks
}

func diskStatFor(path string) DiskInfo {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return DiskInfo{Mount: path}
	}
	total := float64(stat.Blocks) * float64(stat.Bsize)
	free := float64(stat.Bavail) * float64(stat.Bsize)
	return DiskInfo{
		Mount:   path,
		TotalGB: total / (1024 * 1024 * 1024),
		FreeGB:  free / (1024 * 1024 * 1024),
	}
}

// DiskFreeBytes returns the number of bytes available to the current user on
// the filesystem containing path.
func DiskFreeBytes(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return stat.Bavail * uint64(stat.Bsize), nil
}
