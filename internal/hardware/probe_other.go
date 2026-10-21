//go:build !linux && !windows

package hardware

import (
	"fmt"
	"runtime"

	"golang.org/x/sys/cpu"
)

// probePlatform is the fallback for platforms without dedicated probers
// (e.g. darwin). Note: Apple Silicon does not have AVX, it has NEON. If
// running on Intel Mac via Rosetta or natively, we can use cpu.X86.
func probePlatform(caps *Capabilities) {
	caps.CPU.HasAVX = cpu.X86.HasAVX
	caps.CPU.HasAVX2 = cpu.X86.HasAVX2
}

func checkAvailableMemory() error {
	return nil
}

// ProbeDisk returns disk usage for the given path. Mount enumeration is not
// implemented on this platform; only explicit paths are supported.
func ProbeDisk(path string) []DiskInfo {
	if path == "" {
		return nil
	}
	return []DiskInfo{{Mount: path}}
}

// DiskFreeBytes is not implemented on this platform.
func DiskFreeBytes(path string) (uint64, error) {
	return 0, fmt.Errorf("disk free space probe not supported on %s", runtime.GOOS)
}
