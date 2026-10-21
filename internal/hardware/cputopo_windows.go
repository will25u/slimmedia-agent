//go:build windows

package hardware

import (
	"syscall"
	"unsafe"
)

// DetectCPUTopology probes GetLogicalProcessorInformationEx for core
// efficiency classes (Intel hybrid) and L3/die/NUMA layout (AMD multi-CCD).
// Best-effort: any failure degrades toward "symmetric", never toward
// steering.
func DetectCPUTopology() CPUTopology {
	buf, err := processorInfoExBuffer()
	if err != nil || len(buf) == 0 {
		return CPUTopology{Symmetric: true}
	}
	return topologyFromProcessorInfoEx(buf)
}

var procGetLogicalProcessorInformationEx = kernel32.NewProc("GetLogicalProcessorInformationEx")

// processorInfoExBuffer returns the raw RelationAll record buffer. The
// size-query call passes Buffer=NULL with ReturnLength=&needed, per the
// GetLogicalProcessorInformationEx contract (same idiom as
// countProcessorCores in probe_windows.go).
func processorInfoExBuffer() ([]byte, error) {
	var needed uintptr
	r, _, callErr := procGetLogicalProcessorInformationEx.Call(
		0xffff, 0, uintptr(unsafe.Pointer(&needed)))
	if r != 0 || callErr != syscall.ERROR_INSUFFICIENT_BUFFER || needed == 0 {
		return nil, callErr
	}
	buf := make([]byte, needed)
	r, _, err := procGetLogicalProcessorInformationEx.Call(
		0xffff, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&needed)))
	if r == 0 {
		return nil, err
	}
	return buf, nil
}
