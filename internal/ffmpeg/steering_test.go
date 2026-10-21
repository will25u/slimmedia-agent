package ffmpeg

import (
	"reflect"
	"strings"
	"testing"

	"github.com/will25u/slimmedia-agent/internal/hardware"
	"github.com/will25u/slimmedia-agent/internal/jobspec"
	"github.com/will25u/slimmedia-agent/internal/protocol"
)

func hybridTopo() hardware.CPUTopology {
	return hardware.CPUTopology{
		Vendor:           "GenuineIntel",
		Hybrid:           true,
		PerformanceCores: 4,
		EfficiencyCores:  8,
		PerformanceCPUs:  []int{8, 9, 10, 11, 12, 13, 14, 15},
	}
}

func multiCCDTopo() hardware.CPUTopology {
	return hardware.CPUTopology{
		Vendor:       "AuthenticAMD",
		CCDs:         2,
		CCDCPUCounts: []int{8, 8},
		NUMANodes:    2,
	}
}

func symmetricTopo() hardware.CPUTopology {
	return hardware.CPUTopology{Vendor: "AuthenticAMD", Symmetric: true}
}

// steeringJob builds a minimal encode job for the given software codec;
// chunk selects the video-only chunk shape.
func steeringJob(codec string, chunk bool) TranscodeJob {
	spec := jobspec.JobSpec{
		Version:       protocol.CurrentJobSpecVersion,
		Container:     "mkv",
		CRF:           22,
		PresetSpeed:   "medium",
		AqMode:        3,
		AudioHandling: "copy_all",
	}
	if chunk {
		spec.AudioHandling = "strip_all"
		spec.SubtitleHandling = "strip_all"
	}
	return TranscodeJob{
		InputFile:  "/media/in.mkv",
		OutputFile: "/media/out.mkv",
		Codec:      codec,
		Spec:       spec,
	}
}

func TestSteeringFor(t *testing.T) {
	hybrid := ThreadSteering{
		Enabled: true,
		Pools:   "8",
		Threads: 8,
		CPUs:    []int{8, 9, 10, 11, 12, 13, 14, 15},
	}
	tests := []struct {
		name    string
		topo    hardware.CPUTopology
		encoder string
		want    ThreadSteering
	}{
		{"hybrid intel libx265", hybridTopo(), "libx265", hybrid},
		{"hybrid intel libx264", hybridTopo(), "libx264", hybrid},
		{"hybrid intel libsvtav1", hybridTopo(), "libsvtav1", hybrid},
		{"hybrid intel hardware encoder untouched", hybridTopo(), "hevc_nvenc", ThreadSteering{}},
		{"codec family string untouched", hybridTopo(), "hevc", ThreadSteering{}},
		{"amd multi-ccd libx265 pools per die", multiCCDTopo(), "libx265", ThreadSteering{
			Enabled: true, Pools: "8,8", Threads: 16,
		}},
		{"amd multi-ccd libx264 untouched (no pool concept)", multiCCDTopo(), "libx264", ThreadSteering{}},
		{"symmetric libx265 untouched", symmetricTopo(), "libx265", ThreadSteering{}},
		{"zero topology untouched", hardware.CPUTopology{}, "libx265", ThreadSteering{}},
		{"ccd counts mismatch defensive", hardware.CPUTopology{
			Vendor: "AuthenticAMD", CCDs: 2, CCDCPUCounts: []int{8},
		}, "libx265", ThreadSteering{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SteeringFor(tt.topo, tt.encoder)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("SteeringFor(%+v, %q) = %+v, want %+v", tt.topo, tt.encoder, got, tt.want)
			}
		})
	}
}

func TestCompoundParamHas(t *testing.T) {
	if !compoundParamHas("aq-mode=3:pools=16", "pools") {
		t.Error("pools not found")
	}
	if compoundParamHas("aq-mode=3", "pools") {
		t.Error("pools found where absent")
	}
	if !compoundParamHas("pools=none", "POOLS") {
		t.Error("case-insensitive key lookup failed")
	}
	if compoundParamHas("", "pools") {
		t.Error("found in empty params")
	}
}

func TestAppendCompoundParam(t *testing.T) {
	if got := appendCompoundParam("", "pools=8"); got != "pools=8" {
		t.Fatalf("empty base: %q", got)
	}
	if got := appendCompoundParam("aq-mode=3", "pools=8"); got != "aq-mode=3:pools=8" {
		t.Fatalf("append: %q", got)
	}
}

// TestBuildSteeringInjected verifies topology steering lands in the encode
// argv for every software family, on both the chunked and full-stream shapes.
func TestBuildSteeringInjected(t *testing.T) {
	steer := SteeringFor(hybridTopo(), "libx265")

	tests := []struct {
		name   string
		job    TranscodeJob
		needle string // substring that must appear in the joined argv
	}{
		{"libx265 full-stream pools", steeringJob("libx265", false), "-x265-params aq-mode=3:sao=0:pools=8"},
		{"libx265 chunk pools", steeringJob("libx265", true), "-x265-params aq-mode=3:sao=0:pools=8"},
		{"libx264 threads", steeringJob("libx264", false), "-threads 8"},
		{"libsvtav1 lp", steeringJob("libsvtav1", false), "-svtav1-params lp=8"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.job.Steering = &steer
			args, err := buildFfmpegArgs(tt.job, nil)
			if err != nil {
				t.Fatalf("buildFfmpegArgs: %v", err)
			}
			if !strings.Contains(strings.Join(args, " "), tt.needle) {
				t.Fatalf("argv %v missing %q", args, tt.needle)
			}
		})
	}
}

// TestBuildSteeringMultiCCDPartitionsPools: a two-CCD agent's libx265 encode
// carries one pool per die; a single-CCD agent's argv is untouched.
func TestBuildSteeringMultiCCDPartitionsPools(t *testing.T) {
	steer := SteeringFor(multiCCDTopo(), "libx265")
	job := steeringJob("libx265", false)
	job.Steering = &steer
	args, err := buildFfmpegArgs(job, nil)
	if err != nil {
		t.Fatalf("buildFfmpegArgs: %v", err)
	}
	if !strings.Contains(strings.Join(args, " "), "pools=8,8") {
		t.Fatalf("multi-CCD argv missing pools=8,8: %v", args)
	}

	singleCCD := multiCCDTopo()
	singleCCD.CCDs = 1
	singleCCD.CCDCPUCounts = nil
	if got := SteeringFor(singleCCD, "libx265"); got.Enabled {
		t.Fatalf("single-CCD steering enabled: %+v", got)
	}
}

// TestBuildSteeringDisabledIsByteIdentical: symmetric-CPU agents (and any
// agent with no steering configured) must see byte-for-byte identical argv.
func TestBuildSteeringDisabledIsByteIdentical(t *testing.T) {
	baseline, err := buildFfmpegArgs(steeringJob("libx265", false), nil)
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	for _, steering := range []*ThreadSteering{nil, {}, {Enabled: false}} {
		job := steeringJob("libx265", false)
		job.Steering = steering
		args, err := buildFfmpegArgs(job, nil)
		if err != nil {
			t.Fatalf("buildFfmpegArgs: %v", err)
		}
		if !reflect.DeepEqual(args, baseline) {
			t.Fatalf("steering %+v changed argv:\n got %v\nwant %v", steering, args, baseline)
		}
	}
}

// TestBuildSteeringUserPoolsWin: a user-supplied pools= in custom flags must
// not be overridden (or duplicated) by topology steering.
func TestBuildSteeringUserPoolsWin(t *testing.T) {
	steer := SteeringFor(hybridTopo(), "libx265")

	job := steeringJob("libx265", false)
	job.Spec.CustomFlags = "pools=2:pmode=1"
	job.Steering = &steer
	args, err := buildFfmpegArgs(job, nil)
	if err != nil {
		t.Fatalf("buildFfmpegArgs: %v", err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "pools=2") {
		t.Fatalf("user pools= overridden by steering: %v", args)
	}
	if strings.Contains(joined, "pools=8") {
		t.Fatalf("steering pools duplicated alongside user pools: %v", args)
	}
}

// TestBuildSteeringHardwareEncoderUnchanged: even a malformed steering value
// on a hardware encoder must not change the argv.
func TestBuildSteeringHardwareEncoderUnchanged(t *testing.T) {
	job := steeringJob("hevc_nvenc", false)
	baseline, err := buildFfmpegArgs(job, nil)
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	job.Steering = &ThreadSteering{Enabled: true, Pools: "8", Threads: 8, CPUs: []int{0}}
	args, err := buildFfmpegArgs(job, nil)
	if err != nil {
		t.Fatalf("buildFfmpegArgs: %v", err)
	}
	if !reflect.DeepEqual(args, baseline) {
		t.Fatalf("hardware encoder argv changed:\n got %v\nwant %v", args, baseline)
	}
}
