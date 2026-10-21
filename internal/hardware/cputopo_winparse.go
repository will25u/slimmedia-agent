// Windows topology parsing for GetLogicalProcessorInformationEx output.
//
// This file is deliberately untagged: the byte-level record parsing is pure
// (only unsafe.Pointer layout casts) and unit-tests on any GOOS — the same
// pattern as topology.go's logicalProcessorInfo. Only the buffer producer
// (the syscall) lives in the tagged cputopo_windows.go.
package hardware

import (
	"sort"
	"unsafe"
)

// LOGICAL_PROCESSOR_RELATIONSHIP values used here. N.B. the enum order:
// NumaNode=1, Cache=2 — they are NOT adjacent to their use sites' intuition.
const (
	relationNumaNode = 1
	relationCache    = 2
	relationProcDie  = 5
)

// groupAffinityEx mirrors GROUP_AFFINITY (KAFFINITY + group + reserved).
type groupAffinityEx struct {
	mask     uintptr
	group    uint16
	reserved [3]uint16
}

// processorRelationshipEx mirrors PROCESSOR_RELATIONSHIP, the union arm for
// RelationProcessorCore (and Die/Package). Per MSDN, EfficiencyClass is a
// BYTE in which a HIGHER value means intrinsically GREATER performance
// (Intel P-cores outrank E-cores); it is nonzero only on hybrid systems.
type processorRelationshipEx struct {
	flags               byte
	efficiencyClass     byte
	relativePerformance uint16
	relativeEfficiency  uint16
	reserved            [16]byte
	groupCount          uint16
	groupMask           groupAffinityEx
}

// cacheRelationshipEx mirrors CACHE_RELATIONSHIP: the L3 (Level 3, Unified)
// record's GroupMask lists the CPUs sharing that L3 — one CCD domain per
// distinct mask.
type cacheRelationshipEx struct {
	level         byte
	associativity byte
	lineSize      uint16
	cacheSize     uint32
	cacheType     uint32
	reserved      [18]byte
	groupCount    uint16
	groupMask     groupAffinityEx
}

const (
	cacheTypeUnified = 0
	cacheLevelL3     = 3
)

type processorCoreInfo struct {
	efficiencyClass byte
	cpus            []int
}

// l3Domain identifies one shared-L3 domain; the group disambiguates masks
// on multi-group machines, and a zero mask is never a valid domain.
type l3Domain struct {
	group uint16
	mask  uintptr
}

func (d l3Domain) groupMask() groupAffinityEx {
	return groupAffinityEx{group: d.group, mask: d.mask}
}

// topologyFromProcessorInfoEx is the testable half of the Windows producer:
// it walks the raw RelationAll record buffer and builds the topology verdict.
// Best-effort: malformed or short records end parsing; missing facts degrade
// toward "symmetric", never toward steering.
func topologyFromProcessorInfoEx(buf []byte) CPUTopology {
	var cores []processorCoreInfo
	l3Shared := map[l3Domain]bool{}
	var l3Order []l3Domain
	numaNodes, dies := 0, 0

	for off := 0; off+8 <= len(buf); {
		// SYSTEM_LOGICAL_PROCESSOR_INFORMATION_EX header: Relationship
		// first, then Size.
		relationship := *(*uint32)(unsafe.Pointer(&buf[off]))
		size := *(*uint32)(unsafe.Pointer(&buf[off+4]))
		if size < 8 || off+int(size) > len(buf) {
			break
		}
		rec := buf[off : off+int(size)]
		u := rec[8:]
		switch relationship {
		case relationProcessorCore:
			if len(u) < int(unsafe.Sizeof(processorRelationshipEx{})) {
				break
			}
			core := (*processorRelationshipEx)(unsafe.Pointer(&u[0]))
			cores = append(cores, processorCoreInfo{
				efficiencyClass: core.efficiencyClass,
				cpus:            maskCPUs(core.groupMask),
			})
		case relationCache:
			if len(u) < int(unsafe.Sizeof(cacheRelationshipEx{})) {
				break
			}
			cache := (*cacheRelationshipEx)(unsafe.Pointer(&u[0]))
			if cache.level == cacheLevelL3 && cache.cacheType == cacheTypeUnified && cache.groupMask.mask != 0 {
				key := l3Domain{group: cache.groupMask.group, mask: cache.groupMask.mask}
				if !l3Shared[key] {
					l3Shared[key] = true
					l3Order = append(l3Order, key)
				}
			}
		case relationNumaNode:
			numaNodes++
		case relationProcDie:
			dies++
		}
		off += int(size)
	}

	return finalizeTopology(windowsTopology(cores, l3Order, dies, numaNodes))
}

// maskCPUs expands a group mask into logical CPU ids. Groups beyond 0 are
// offset by 64 ids per group, matching Windows processor-group numbering.
func maskCPUs(ga groupAffinityEx) []int {
	var cpus []int
	base := int(ga.group) * 64
	for bit := 0; bit < 64; bit++ {
		if ga.mask&(uintptr(1)<<uint(bit)) != 0 {
			cpus = append(cpus, base+bit)
		}
	}
	return cpus
}

// windowsTopology assembles a CPUTopology from parsed Ex records. Core
// efficiency classes follow the documented ordering: a HIGHER class is more
// performant, so the maximum class is the performance class. A single class
// everywhere (or all zero, e.g. pre-Windows-10) means a symmetric part.
// Vendor is reported only for the hybrid case: per-core efficiency classes
// are an Intel P/E signature today, and claiming GenuineIntel for an AMD
// multi-CCD part would be a lie.
func windowsTopology(cores []processorCoreInfo, l3Domains []l3Domain, dies, numaNodes int) CPUTopology {
	var topo CPUTopology

	if len(cores) > 0 {
		classes := map[byte]bool{}
		maxClass := cores[0].efficiencyClass
		for _, c := range cores {
			classes[c.efficiencyClass] = true
			if c.efficiencyClass > maxClass {
				maxClass = c.efficiencyClass
			}
		}
		if len(classes) > 1 {
			topo.Hybrid = true
			topo.Vendor = "GenuineIntel"
			for _, c := range cores {
				if c.efficiencyClass != maxClass {
					topo.EfficiencyCores++
					continue
				}
				topo.PerformanceCores++
				topo.PerformanceCPUs = append(topo.PerformanceCPUs, c.cpus...)
			}
			if len(topo.PerformanceCPUs) == 0 {
				topo = CPUTopology{}
			}
		}
	}

	if len(l3Domains) > 1 {
		counts := make([]int, 0, len(l3Domains))
		for _, d := range l3Domains {
			counts = append(counts, len(maskCPUs(d.groupMask())))
		}
		topo.CCDs = len(counts)
		topo.CCDCPUCounts = counts
	} else if dies > 1 {
		topo.CCDs = dies
	} else if numaNodes > 1 {
		topo.CCDs = numaNodes
	}
	if numaNodes > 0 {
		topo.NUMANodes = numaNodes
	}
	sort.Ints(topo.PerformanceCPUs)
	return topo
}
