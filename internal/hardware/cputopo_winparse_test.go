package hardware

import (
	"encoding/binary"
	"reflect"
	"testing"
)

// The record builders below assemble raw GetLogicalProcessorInformationEx
// buffers byte-by-byte per the documented layouts, so a layout regression
// (offset or constant drift) fails here on any GOOS, not just on Windows.

func putU32(buf []byte, off int, v uint32) { binary.LittleEndian.PutUint32(buf[off:], v) }
func putU16(buf []byte, off int, v uint16) { binary.LittleEndian.PutUint16(buf[off:], v) }

// coreRecord appends a RelationProcessorCore record (48 bytes): header(8),
// then PROCESSOR_RELATIONSHIP with EfficiencyClass at union offset 1,
// GroupCount at union offset 22, and GroupMask.mask at union offset 24.
func coreRecord(buf []byte, efficiencyClass byte, mask uintptr) []byte {
	rec := make([]byte, 48)
	putU32(rec, 0, relationProcessorCore)
	putU32(rec, 4, 48)
	rec[8] = 0 // Flags
	rec[9] = efficiencyClass
	putU16(rec, 8+22, 1) // GroupCount
	for i := 0; i < 64; i++ {
		if mask&(uintptr(1)<<uint(i)) != 0 {
			rec[8+24+i/8] |= byte(1) << uint(i%8)
		}
	}
	return append(buf, rec...)
}

// l3Record appends a RelationCache record for a unified L3 (56 bytes):
// header(8), then CACHE_RELATIONSHIP with Level at union offset 0, Type at
// offset 8, and GroupMask.mask at union offset 32.
func l3Record(buf []byte, mask uintptr) []byte {
	rec := make([]byte, 56)
	putU32(rec, 0, relationCache)
	putU32(rec, 4, 56)
	rec[8] = cacheLevelL3
	putU32(rec, 8+8, cacheTypeUnified)
	putU16(rec, 8+30, 1) // GroupCount
	for i := 0; i < 64; i++ {
		if mask&(uintptr(1)<<uint(i)) != 0 {
			rec[8+32+i/8] |= byte(1) << uint(i%8)
		}
	}
	return append(buf, rec...)
}

func countRecord(buf []byte, relationship uint32) []byte {
	rec := make([]byte, 56)
	putU32(rec, 0, relationship)
	putU32(rec, 4, 56)
	return append(buf, rec...)
}

func TestWindowsTopologyHybrid(t *testing.T) {
	// P-cores: class 2, SMT pairs (cpus 0+8, 1+9, 2+10, 3+11).
	// E-cores: class 1, single-thread (cpus 4-7).
	cores := []processorCoreInfo{
		{efficiencyClass: 2, cpus: []int{0, 8}},
		{efficiencyClass: 2, cpus: []int{1, 9}},
		{efficiencyClass: 2, cpus: []int{2, 10}},
		{efficiencyClass: 2, cpus: []int{3, 11}},
		{efficiencyClass: 1, cpus: []int{4}},
		{efficiencyClass: 1, cpus: []int{5}},
		{efficiencyClass: 1, cpus: []int{6}},
		{efficiencyClass: 1, cpus: []int{7}},
	}
	topo := finalizeTopology(windowsTopology(cores, nil, 0, 1))
	if !topo.Hybrid || topo.Symmetric {
		t.Fatalf("hybrid/symmetric wrong: %+v", topo)
	}
	if topo.Vendor != "GenuineIntel" {
		t.Fatalf("vendor = %q", topo.Vendor)
	}
	if topo.PerformanceCores != 4 || topo.EfficiencyCores != 4 {
		t.Fatalf("cores = %dP/%dE, want 4P/4E", topo.PerformanceCores, topo.EfficiencyCores)
	}
	wantCPUs := []int{0, 1, 2, 3, 8, 9, 10, 11}
	if !reflect.DeepEqual(topo.PerformanceCPUs, wantCPUs) {
		t.Fatalf("performance CPUs = %v, want %v", topo.PerformanceCPUs, wantCPUs)
	}
}

func TestWindowsTopologyHigherClassIsPerformance(t *testing.T) {
	// The documented semantics: HIGHER EfficiencyClass = more performant.
	// If a future "efficiency boost" class 3 appears on E-cores, the max
	// class must still track the P-cores — this test pins the max rule.
	cores := []processorCoreInfo{
		{efficiencyClass: 1, cpus: []int{0, 1}}, // P (this CPU's convention)
		{efficiencyClass: 0, cpus: []int{2}},    // E
	}
	topo := finalizeTopology(windowsTopology(cores, nil, 0, 0))
	if !topo.Hybrid || len(topo.PerformanceCPUs) != 2 || topo.PerformanceCPUs[0] != 0 {
		t.Fatalf("max class must be the performance class: %+v", topo)
	}
}

func TestWindowsTopologyAmdMultiCCD(t *testing.T) {
	// 16 homogeneous cores across two L3 domains; no efficiency classes.
	var cores []processorCoreInfo
	for i := 0; i < 16; i++ {
		cores = append(cores, processorCoreInfo{cpus: []int{i}})
	}
	l3 := []l3Domain{{mask: 0x00FF}, {mask: 0xFF00}}
	topo := finalizeTopology(windowsTopology(cores, l3, 0, 2))
	if topo.Hybrid {
		t.Fatal("hybrid = true for homogeneous cores")
	}
	if topo.Symmetric {
		t.Fatal("symmetric = true for multi-CCD part")
	}
	if topo.CCDs != 2 || !reflect.DeepEqual(topo.CCDCPUCounts, []int{8, 8}) {
		t.Fatalf("CCDs = %d counts %v, want 2/[8 8]", topo.CCDs, topo.CCDCPUCounts)
	}
	if topo.NUMANodes != 2 {
		t.Fatalf("NUMA nodes = %d, want 2", topo.NUMANodes)
	}
	if topo.Vendor != "" {
		t.Fatalf("vendor = %q, want empty (unknown, not Intel)", topo.Vendor)
	}
}

func TestWindowsTopologySymmetric(t *testing.T) {
	var cores []processorCoreInfo
	for i := 0; i < 8; i++ {
		cores = append(cores, processorCoreInfo{cpus: []int{i}})
	}
	topo := finalizeTopology(windowsTopology(cores, []l3Domain{{mask: 0xFF}}, 0, 1))
	if !topo.Symmetric || topo.Hybrid || topo.CCDs > 1 {
		t.Fatalf("symmetric verdict wrong: %+v", topo)
	}
}

// TestWindowsParseRawBuffer drives the byte-level parser end to end,
// including the relationship constants and header field order.
func TestWindowsParseRawBuffer(t *testing.T) {
	var buf []byte
	// Hybrid: one P-core record (class 1, cpu 0) + one E-core (class 0,
	// cpu 1).
	buf = coreRecord(buf, 1, 0x1)
	buf = coreRecord(buf, 0, 0x2)
	// Two L3 domains + 2 NUMA nodes.
	buf = l3Record(buf, 0x1)
	buf = l3Record(buf, 0x2)
	buf = countRecord(buf, relationNumaNode)
	buf = countRecord(buf, relationNumaNode)

	topo := topologyFromProcessorInfoEx(buf)
	if !topo.Hybrid {
		t.Fatalf("hybrid not detected from raw buffer: %+v", topo)
	}
	if !reflect.DeepEqual(topo.PerformanceCPUs, []int{0}) {
		t.Fatalf("performance CPUs = %v, want [0]", topo.PerformanceCPUs)
	}
	if topo.CCDs != 2 || topo.NUMANodes != 2 {
		t.Fatalf("CCDs = %d NUMA = %d, want 2/2 (relation constants swapped?)", topo.CCDs, topo.NUMANodes)
	}
}

func TestWindowsParseGarbage(t *testing.T) {
	// Truncated and zeroed buffers must degrade to symmetric, not panic.
	for _, buf := range [][]byte{nil, {}, {1, 2, 3}, make([]byte, 64)} {
		topo := topologyFromProcessorInfoEx(buf)
		if !topo.Symmetric {
			t.Fatalf("garbage buffer not symmetric: %v -> %+v", buf, topo)
		}
	}
}
