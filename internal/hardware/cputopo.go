// CPU topology detection for thread steering (issue #201). The producers in
// the per-GOOS files (cputopo_linux.go, cputopo_windows.go) gather raw OS
// facts; everything here — parsing, classification, and the CPUTopology
// verdict — is pure and unit-testable on any platform.
package hardware

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// CPUTopology describes the detected core/die layout of the machine. It
// drives software-encoder thread steering: hybrid Intel agents steer encodes
// onto P-cores, multi-CCD/NUMA agents partition encoder pools per die, and a
// symmetric machine means "leave every encode untouched".
type CPUTopology struct {
	// Vendor is the CPUID vendor string ("GenuineIntel", "AuthenticAMD");
	// empty when unknown.
	Vendor string `json:"vendor,omitempty"`
	// Hybrid reports an asymmetric Intel P/E core arrangement (12th gen and
	// later). Only set when the core classes were positively classified.
	Hybrid bool `json:"hybrid"`
	// PerformanceCores / EfficiencyCores are physical core counts per class.
	PerformanceCores int `json:"performance_cores,omitempty"`
	EfficiencyCores  int `json:"efficiency_cores,omitempty"`
	// PerformanceCPUs lists the logical CPU IDs of the performant cores; the
	// encode process is pinned to exactly this set on hybrid machines.
	PerformanceCPUs []int `json:"performance_cpus,omitempty"`
	// CCDs is the number of distinct L3/die domains (one per CCD on AMD
	// multi-CCD parts, one per socket/die generally). 0 or 1 means a single
	// domain — no pool partitioning.
	CCDs int `json:"ccds,omitempty"`
	// CCDCPUCounts is the logical-CPU count of each L3 domain, in ascending
	// domain order; it backs the x265 per-pool thread counts.
	CCDCPUCounts []int `json:"ccd_cpu_counts,omitempty"`
	// NUMANodes is the number of NUMA nodes the OS exposes.
	NUMANodes int `json:"numa_nodes,omitempty"`
	// Symmetric is true when detection ran and found no asymmetric topology:
	// no P/E split and a single CCD/NUMA domain. Encodes stay untouched.
	Symmetric bool `json:"symmetric"`
}

// maxCPUExpand bounds a single "a-b" element of a CPU list; a corrupted
// sysfs value must not make the agent allocate gigabytes.
const maxCPUExpand = 1 << 16

// maxCPUIndex bounds a logical CPU index from /proc/cpuinfo; beyond this the
// parse is treated as failed (symmetric, no steering) rather than trusted.
const maxCPUIndex = 1 << 16

type coreClass int

const (
	classUnknown coreClass = iota
	classPerformance
	classEfficiency
)

// intelHybridCoreClass maps Intel CPUID model numbers (as reported per core
// in /proc/cpuinfo on hybrid parts, where P-cores and E-cores expose distinct
// signatures) to their core class. Values per Intel SDM / Linux cpuinfo
// observations: Alder Lake P=0x97/E=0x9A, Raptor Lake P=0xB7/E=0xBA,
// Meteor Lake P=0xAA/E=0xAC, Arrow Lake P=0xC5/E=0xC6, Lunar Lake
// P=0xBD/E=0xBE.
var intelHybridCoreClass = map[int]coreClass{
	0x97: classPerformance, // Alder Lake P (Golden Cove)
	0x9A: classEfficiency,  // Alder Lake E (Gracemont)
	0xB7: classPerformance, // Raptor Lake P (Raptor Cove)
	0xBA: classEfficiency,  // Raptor Lake E (Gracemont)
	0xAA: classPerformance, // Meteor Lake P (Redwood Cove)
	0xAC: classEfficiency,  // Meteor Lake E (Crestmont)
	0xC5: classPerformance, // Arrow Lake P (Lion Cove)
	0xC6: classEfficiency,  // Arrow Lake E (Skymont)
	0xBD: classPerformance, // Lunar Lake P (Lion Cove)
	0xBE: classEfficiency,  // Lunar Lake E (Skymont)
}

// classifyIntelModels classifies a per-logical-CPU list of CPUID model
// numbers. It reports a hybrid topology only when the CPU is a known hybrid
// part and every distinct model maps to a known class; anything else returns
// a zero topology so steering stays off (conservative by construction).
func classifyIntelModels(vendor string, models []int) (hybrid bool, classes []coreClass) {
	if vendor != "GenuineIntel" || len(models) == 0 {
		return false, nil
	}
	seen := map[int]bool{}
	known := true
	for _, m := range models {
		if seen[m] {
			continue
		}
		seen[m] = true
		if _, ok := intelHybridCoreClass[m]; !ok {
			known = false
		}
	}
	if len(seen) < 2 || !known {
		return false, nil
	}
	classes = make([]coreClass, len(models))
	for i, m := range models {
		classes[i] = intelHybridCoreClass[m]
	}
	return true, classes
}

// topologyFromClasses builds the hybrid half of a CPUTopology from per-CPU
// core classes. coreGroups, when non-nil, maps each logical CPU to its core's
// sibling list (used to count physical cores); nil counts each logical CPU as
// its own core.
func topologyFromClasses(classes []coreClass, coreGroups []string) CPUTopology {
	var topo CPUTopology
	topo.Vendor = "GenuineIntel"
	topo.Hybrid = true
	seenCore := map[string]bool{}
	for cpu, class := range classes {
		switch class {
		case classPerformance:
			topo.PerformanceCPUs = append(topo.PerformanceCPUs, cpu)
			if !seenCore[coreKey(coreGroups, cpu)] {
				seenCore[coreKey(coreGroups, cpu)] = true
				topo.PerformanceCores++
			}
		case classEfficiency:
			if !seenCore[coreKey(coreGroups, cpu)] {
				seenCore[coreKey(coreGroups, cpu)] = true
				topo.EfficiencyCores++
			}
		}
	}
	if len(topo.PerformanceCPUs) == 0 {
		return CPUTopology{Vendor: "GenuineIntel"}
	}
	return topo
}

// coreKey returns the sibling-list identity of the core owning logical CPU i,
// or the CPU id itself when no grouping is available.
func coreKey(coreGroups []string, i int) string {
	if i < len(coreGroups) && coreGroups[i] != "" {
		return coreGroups[i]
	}
	return strconv.Itoa(i)
}

// finalizeTopology applies the symmetric verdict and orders the performance
// CPU list; every producer ends with this.
func finalizeTopology(t CPUTopology) CPUTopology {
	sort.Ints(t.PerformanceCPUs)
	t.Symmetric = !t.Hybrid && t.CCDs <= 1
	return t
}

// parseCPUList parses a sysfs-style CPU list ("0-3,8,12-15") into ascending
// CPU ids. Invalid elements and absurd ranges (a corrupted list must not
// make the daemon allocate gigabytes) are skipped.
func parseCPUList(s string) []int {
	var cpus []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if lo, hi, ok := strings.Cut(part, "-"); ok {
			loN, err1 := strconv.Atoi(lo)
			hiN, err2 := strconv.Atoi(hi)
			if err1 != nil || err2 != nil || hiN < loN || hiN-loN > maxCPUExpand {
				continue
			}
			for c := loN; c <= hiN; c++ {
				cpus = append(cpus, c)
			}
			continue
		}
		if n, err := strconv.Atoi(part); err == nil {
			cpus = append(cpus, n)
		}
	}
	return cpus
}

// formatCPUList renders CPU ids in the compact sysfs list form ("0-3,8").
func formatCPUList(cpus []int) string {
	if len(cpus) == 0 {
		return ""
	}
	sorted := append([]int(nil), cpus...)
	sort.Ints(sorted)
	var parts []string
	start, prev := sorted[0], sorted[0]
	flush := func() {
		if start == prev {
			parts = append(parts, strconv.Itoa(start))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", start, prev))
		}
	}
	for _, c := range sorted[1:] {
		if c == prev+1 {
			prev = c
			continue
		}
		flush()
		start, prev = c, c
	}
	flush()
	return strings.Join(parts, ",")
}
