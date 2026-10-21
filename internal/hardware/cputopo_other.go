//go:build !linux && !windows

package hardware

// DetectCPUTopology has no producer on this platform; the zero topology
// leaves every encode untouched.
func DetectCPUTopology() CPUTopology {
	return CPUTopology{Symmetric: true}
}
