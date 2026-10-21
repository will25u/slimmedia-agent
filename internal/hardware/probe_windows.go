//go:build windows

package hardware

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/cpu"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

func probePlatform(caps *Capabilities) {
	probeWindowsCPU(caps)
	probeWindowsPhysicalCores(caps)
	probeWindowsGPU(caps)
	probeWindowsMemory(caps)
	probeWindowsOS(caps)
}

var procGetLogicalProcessorInformation = kernel32.NewProc("GetLogicalProcessorInformation")

// probeWindowsPhysicalCores counts physical cores via
// GetLogicalProcessorInformation, falling back to the NumberOfCores value in
// the first CentralProcessor registry key.
func probeWindowsPhysicalCores(caps *Capabilities) {
	if n := countProcessorCores(); n > 0 {
		caps.CPU.PhysicalCores = n
		return
	}
	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`HARDWARE\DESCRIPTION\System\CentralProcessor\0`, registry.QUERY_VALUE)
	if err != nil {
		return
	}
	defer k.Close()
	if n, _, err := k.GetIntegerValue("NumberOfCores"); err == nil && n > 0 {
		caps.CPU.PhysicalCores = int(n)
	}
}

// countProcessorCores returns the number of RelationProcessorCore records
// reported by GetLogicalProcessorInformation, or 0 if the call fails.
func countProcessorCores() int {
	var needed uintptr
	r, _, callErr := procGetLogicalProcessorInformation.Call(0, uintptr(unsafe.Pointer(&needed)))
	if r != 0 || callErr != syscall.ERROR_INSUFFICIENT_BUFFER || needed == 0 {
		return 0
	}
	recordSize := unsafe.Sizeof(logicalProcessorInfo{})
	buf := make([]logicalProcessorInfo, int(needed)/int(recordSize))
	if len(buf) == 0 {
		return 0
	}
	needed = uintptr(len(buf)) * recordSize
	r, _, _ = procGetLogicalProcessorInformation.Call(
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&needed)))
	if r == 0 {
		return 0
	}
	return countRelationship(buf, relationProcessorCore)
}

func probeWindowsCPU(caps *Capabilities) {
	caps.CPU.HasAVX = cpu.X86.HasAVX
	caps.CPU.HasAVX2 = cpu.X86.HasAVX2
	if caps.CPU.HasAVX2 {
		caps.CPU.HasAVX = true
	}
	// ProcessorNameString from the first processor key is the human-readable model.
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`HARDWARE\DESCRIPTION\System\CentralProcessor\0`, registry.QUERY_VALUE); err == nil {
		if name, _, err := k.GetStringValue("ProcessorNameString"); err == nil {
			caps.CPU.Model = name
		}
		k.Close()
	}
}

var kernel32 = windows.NewLazySystemDLL("kernel32.dll")
var procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")

// memoryStatusEx mirrors MEMORYSTATUSEX; x/sys/windows does not expose it, so
// declare the layout and call the proc directly.
type memoryStatusEx struct {
	length               uint32
	memoryLoad           uint32
	totalPhys            uint64
	availPhys            uint64
	totalPageFile        uint64
	availPageFile        uint64
	totalVirtual         uint64
	availVirtual         uint64
	availExtendedVirtual uint64
}

func probeWindowsMemory(caps *Capabilities) {
	var mem memoryStatusEx
	mem.length = uint32(unsafe.Sizeof(mem))
	r, _, err := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&mem)))
	if r == 0 {
		_ = err // best-effort probe; leave fields at zero
		return
	}
	caps.Memory.TotalGB = float64(mem.totalPhys) / (1024 * 1024 * 1024)
}

func checkAvailableMemory() error {
	if os.Getenv("SLIMMEDIA_SKIP_MEMORY_CHECK") == "1" {
		return nil
	}
	var mem memoryStatusEx
	mem.length = uint32(unsafe.Sizeof(mem))
	r, _, err := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&mem)))
	if r == 0 {
		return fmt.Errorf("pre-flight memory check failed: %w", err)
	}
	availGB := float64(mem.availPhys) / (1024 * 1024 * 1024)
	if availGB < 2.0 {
		return fmt.Errorf("insufficient host memory: %.2f GB available, minimum 2.0 GB required", availGB)
	}
	return nil
}

func probeWindowsOS(caps *Capabilities) {
	caps.OS.Name = "Windows"
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE); err == nil {
		if name, _, err := k.GetStringValue("ProductName"); err == nil && name != "" {
			caps.OS.Name = name
			if display, _, err := k.GetStringValue("DisplayVersion"); err == nil && display != "" {
				caps.OS.Name += " " + display
			}
		}
		k.Close()
	}
}
