package hardware

// relationProcessorCore is LOGICAL_PROCESSOR_RELATIONSHIP RelationProcessorCore.
const relationProcessorCore = 0

// logicalProcessorInfo mirrors SYSTEM_LOGICAL_PROCESSOR_INFORMATION; only the
// relationship field is interpreted, so the union is left opaque. Go's own
// alignment pads the record to the native 32 bytes on amd64 and 24 on 386.
// It lives untagged so the counting logic is unit testable on any GOOS; only
// the producer (GetLogicalProcessorInformation) is in probe_windows.go.
type logicalProcessorInfo struct {
	processorMask uintptr
	relationship  uint32
	reserved      [2]uint64
}

// countRelationship returns how many records carry the given
// LOGICAL_PROCESSOR_RELATIONSHIP value.
func countRelationship(records []logicalProcessorInfo, relationship uint32) int {
	count := 0
	for _, rec := range records {
		if rec.relationship == relationship {
			count++
		}
	}
	return count
}
