// Thread steering for software encodes (issue #201). The agent detects the
// machine's CPU topology at startup; hybrid Intel agents pin software
// encodes to P-cores, multi-CCD agents partition x265 pools per die, and
// symmetric machines steer nothing at all.
package ffmpeg

import (
	"strconv"
	"strings"

	"github.com/will25u/slimmedia-agent/internal/hardware"
)

// ThreadSteering is the resolved per-encode steering decision. The zero
// value means "leave the encode untouched" and must leave the argv
// byte-for-byte identical to a no-topology build.
type ThreadSteering struct {
	// Enabled gates every steering effect; when false the topology had
	// nothing to say for this encoder.
	Enabled bool
	// Pools is the x265 pools= value ("16" for a P-core restriction,
	// "8,8" for two CCDs); empty when no pool restriction applies.
	Pools string
	// Threads caps the encoder's thread count for encoders without a pool
	// concept (libx264 -threads, libsvtav1 lp=); 0 leaves the default.
	Threads int
	// CPUs is the logical-CPU set the encode process is pinned to; empty
	// means "inherit the parent's affinity".
	CPUs []int
}

// SteeringFor resolves the steering decision for one encoder on one
// topology. It is pure: the same inputs always produce the same argv, which
// keeps command traces and resume behavior deterministic.
//
// Rules (issue #201):
//   - Only the software encoder families steer; hardware encoders and any
//     unknown encoder are returned untouched.
//   - Hybrid Intel: the encode is pinned to the P-core CPU set with a
//     matching thread count for every software family.
//   - Multi-CCD/NUMA: libx265 gets a per-die pools= partition; encoders
//     without a pool concept are left alone (no affinity games).
//   - Anything symmetric: the zero value — no argv change anywhere.
func SteeringFor(topo hardware.CPUTopology, encoder string) ThreadSteering {
	switch encoder {
	case "libx265", "libx264", "libsvtav1":
	default:
		return ThreadSteering{}
	}
	if topo.Hybrid && len(topo.PerformanceCPUs) > 0 {
		n := len(topo.PerformanceCPUs)
		return ThreadSteering{
			Enabled: true,
			Pools:   strconv.Itoa(n),
			Threads: n,
			CPUs:    append([]int(nil), topo.PerformanceCPUs...),
		}
	}
	if encoder == "libx265" && topo.CCDs > 1 && len(topo.CCDCPUCounts) == topo.CCDs {
		parts := make([]string, len(topo.CCDCPUCounts))
		total := 0
		for i, n := range topo.CCDCPUCounts {
			parts[i] = strconv.Itoa(n)
			total += n
		}
		return ThreadSteering{
			Enabled: true,
			Pools:   strings.Join(parts, ","),
			Threads: total,
		}
	}
	return ThreadSteering{}
}
