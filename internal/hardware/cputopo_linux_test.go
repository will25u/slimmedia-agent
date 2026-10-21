//go:build linux

package hardware

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// writeFile creates parent dirs and writes a fixture file.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// cpuInfoFixture renders a /proc/cpuinfo with the given vendor and one block
// per logical CPU (in order) carrying the given model number.
func cpuInfoFixture(vendor string, models []int) string {
	var b strings.Builder
	for cpu, model := range models {
		b.WriteString("processor\t: " + strconv.Itoa(cpu) + "\n")
		if vendor != "" {
			b.WriteString("vendor_id\t: " + vendor + "\n")
		}
		b.WriteString("model\t\t: " + strconv.Itoa(model) + "\n\n")
	}
	return b.String()
}

// fixtureCPU writes the sysfs topology + cache files for one logical CPU.
// core is the core_cpus_list, l3 the shared L3 cpu list.
func fixtureCPU(t *testing.T, sysCPU string, id int, core, l3 string) {
	base := filepath.Join(sysCPU, "cpu"+strconv.Itoa(id))
	writeFile(t, filepath.Join(base, "topology", "core_cpus_list"), core+"\n")
	writeFile(t, filepath.Join(base, "cache", "index0", "level"), "1\n")
	writeFile(t, filepath.Join(base, "cache", "index0", "type"), "Data\n")
	writeFile(t, filepath.Join(base, "cache", "index0", "shared_cpu_list"), strconv.Itoa(id)+"\n")
	writeFile(t, filepath.Join(base, "cache", "index3", "level"), "3\n")
	writeFile(t, filepath.Join(base, "cache", "index3", "type"), "Unified\n")
	writeFile(t, filepath.Join(base, "cache", "index3", "shared_cpu_list"), l3+"\n")
}

// withCPUInfo points cpuInfoPath at a temp file for the duration of the test.
func withCPUInfo(t *testing.T, content string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cpuinfo")
	writeFile(t, path, content)
	prev := cpuInfoPath
	cpuInfoPath = path
	t.Cleanup(func() { cpuInfoPath = prev })
}

func TestDetectTopologyIntelHybrid(t *testing.T) {
	// Alder Lake-like: 8 E-cores (cpus 0-7, model 154, one thread per
	// core), 4 P-cores with HT (cpus 8-11 + 12-15, model 151), one shared
	// L3, one NUMA node.
	models := make([]int, 16)
	for i := 0; i < 8; i++ {
		models[i] = 154
	}
	for i := 8; i < 16; i++ {
		models[i] = 151
	}
	withCPUInfo(t, cpuInfoFixture("GenuineIntel", models))

	sysRoot := t.TempDir()
	sysCPU := filepath.Join(sysRoot, "cpu")
	sysNode := filepath.Join(sysRoot, "node")
	for i := 0; i < 8; i++ {
		fixtureCPU(t, sysCPU, i, strconv.Itoa(i), "0-15")
	}
	for i := 8; i < 12; i++ {
		fixtureCPU(t, sysCPU, i, strconv.Itoa(i)+","+strconv.Itoa(i+4), "0-15")
	}
	for i := 12; i < 16; i++ {
		fixtureCPU(t, sysCPU, i, strconv.Itoa(i-4)+","+strconv.Itoa(i), "0-15")
	}
	writeFile(t, filepath.Join(sysNode, "node0", "cpulist"), "0-15\n")

	topo := detectTopology(sysCPU, sysNode)
	if !topo.Hybrid {
		t.Fatal("hybrid = false, want true")
	}
	if topo.Symmetric {
		t.Fatal("symmetric = true, want false")
	}
	if topo.PerformanceCores != 4 || topo.EfficiencyCores != 8 {
		t.Fatalf("cores = %dP/%dE, want 4P/8E", topo.PerformanceCores, topo.EfficiencyCores)
	}
	wantCPUs := []int{8, 9, 10, 11, 12, 13, 14, 15}
	if !reflect.DeepEqual(topo.PerformanceCPUs, wantCPUs) {
		t.Fatalf("performance CPUs = %v, want %v", topo.PerformanceCPUs, wantCPUs)
	}
	if topo.CCDs != 1 || topo.NUMANodes != 1 {
		t.Fatalf("CCDs = %d, NUMA = %d; want 1/1", topo.CCDs, topo.NUMANodes)
	}
}

func TestDetectTopologyAMDMultiCCD(t *testing.T) {
	// Ryzen-like: 16 homogeneous cores (model 97), two L3 domains of 8
	// CPUs (one per CCD), two NUMA nodes.
	models := make([]int, 16)
	for i := range models {
		models[i] = 97
	}
	withCPUInfo(t, cpuInfoFixture("AuthenticAMD", models))

	sysRoot := t.TempDir()
	sysCPU := filepath.Join(sysRoot, "cpu")
	sysNode := filepath.Join(sysRoot, "node")
	for i := 0; i < 8; i++ {
		fixtureCPU(t, sysCPU, i, strconv.Itoa(i)+","+strconv.Itoa(i+8), "0-7")
	}
	for i := 8; i < 16; i++ {
		fixtureCPU(t, sysCPU, i, strconv.Itoa(i-8)+","+strconv.Itoa(i), "8-15")
	}
	writeFile(t, filepath.Join(sysNode, "node0", "cpulist"), "0-7\n")
	writeFile(t, filepath.Join(sysNode, "node1", "cpulist"), "8-15\n")

	topo := detectTopology(sysCPU, sysNode)
	if topo.Hybrid {
		t.Fatal("hybrid = true for homogeneous AMD, want false")
	}
	if topo.Symmetric {
		t.Fatal("symmetric = true for multi-CCD part, want false")
	}
	if topo.CCDs != 2 || !reflect.DeepEqual(topo.CCDCPUCounts, []int{8, 8}) {
		t.Fatalf("CCDs = %d counts %v, want 2/[8 8]", topo.CCDs, topo.CCDCPUCounts)
	}
	if topo.NUMANodes != 2 {
		t.Fatalf("NUMA nodes = %d, want 2", topo.NUMANodes)
	}
	if len(topo.PerformanceCPUs) != 0 {
		t.Fatalf("performance CPUs = %v, want none", topo.PerformanceCPUs)
	}
}

func TestDetectTopologySymmetric(t *testing.T) {
	// Homogeneous 8-core Intel, single L3, single NUMA node: detection
	// must definitively report "symmetric — no steering needed".
	models := make([]int, 8)
	for i := range models {
		models[i] = 151
	}
	withCPUInfo(t, cpuInfoFixture("GenuineIntel", models))

	sysRoot := t.TempDir()
	sysCPU := filepath.Join(sysRoot, "cpu")
	sysNode := filepath.Join(sysRoot, "node")
	for i := 0; i < 8; i++ {
		fixtureCPU(t, sysCPU, i, strconv.Itoa(i), "0-7")
	}
	writeFile(t, filepath.Join(sysNode, "node0", "cpulist"), "0-7\n")

	topo := detectTopology(sysCPU, sysNode)
	if !topo.Symmetric {
		t.Fatalf("symmetric = false, want true: %+v", topo)
	}
	if topo.Hybrid || topo.CCDs > 1 || len(topo.PerformanceCPUs) > 0 {
		t.Fatalf("steering fields set on symmetric topology: %+v", topo)
	}
}

func TestParseCPUInfoModels(t *testing.T) {
	// Two processor blocks out of numeric order; the second vendor_id must
	// be ignored and the "model name" line must not be mistaken for "model".
	info := strings.Join([]string{
		"processor\t: 1",
		"vendor_id\t: GenuineIntel",
		"model name\t: Intel(R) Core(TM) i9-12900K",
		"model\t\t: 154",
		"",
		"processor\t: 0",
		"vendor_id\t: GenuineIntel",
		"model\t\t: 151",
		"",
	}, "\n")

	vendor, models := parseCPUInfoModels(strings.NewReader(info))
	if vendor != "GenuineIntel" {
		t.Fatalf("vendor = %q", vendor)
	}
	want := []int{151, 154}
	if !reflect.DeepEqual(models, want) {
		t.Fatalf("models = %v, want %v", models, want)
	}
}

func TestDetectCPUTopologyLive(t *testing.T) {
	// Smoke test on the actual machine: detection must not panic and the
	// symmetric verdict must agree with the steering fields.
	topo := DetectCPUTopology()
	if topo.Symmetric == (topo.Hybrid || topo.CCDs > 1) {
		t.Fatalf("symmetric verdict disagrees with steering fields: %+v", topo)
	}
	t.Logf("live topology: %+v", topo)
}
