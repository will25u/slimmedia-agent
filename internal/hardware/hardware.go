// Package hardware probes the local machine for CPU, GPU, memory, OS, and
// disk facts used by the audit report and tier assessment. Platform-specific
// probers live in the per-GOOS files (probe_linux.go, probe_windows.go);
// everything here is portable.
package hardware

import (
	"runtime"
)

type DiskInfo struct {
	Mount   string
	TotalGB float64
	FreeGB  float64
}

type Capabilities struct {
	CPU struct {
		Arch          string
		Model         string
		PhysicalCores int
		LogicalCores  int
		HasAVX        bool
		HasAVX2       bool
		// Topology is the detected core/die layout (issue #201): Intel P/E
		// classes, CCD/NUMA domains, or definitively symmetric.
		Topology CPUTopology
	}
	GPU struct {
		HasNvidia         bool
		HasIntel          bool
		HasAMD            bool
		Model             string
		Architecture      string
		DedicatedVRAMMB   uint64
		SharedSystemMemMB uint64
		TotalMemoryMB     uint64
	}
	Memory struct {
		TotalGB float64
	}
	OS struct {
		Name string
	}
	Disks []DiskInfo
}

func Probe() Capabilities {
	var caps Capabilities
	caps.CPU.Arch = runtime.GOARCH
	caps.CPU.LogicalCores = runtime.NumCPU()
	caps.CPU.Topology = DetectCPUTopology()

	probePlatform(&caps)
	return caps
}

// CheckAvailableMemory performs a lightweight pre-flight host safety check
// verifying that at least 2GB of available memory exists.
func CheckAvailableMemory() error {
	return checkAvailableMemory()
}
