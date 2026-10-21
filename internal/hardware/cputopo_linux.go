//go:build linux

package hardware

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Path overrides for tests; the producers read only these roots.
var (
	cpuInfoPath  = "/proc/cpuinfo"
	sysfsCPUPath = "/sys/devices/system/cpu"
	sysfsNodeDir = "/sys/devices/system/node"
)

// DetectCPUTopology probes sysfs and /proc/cpuinfo for the machine's core
// class (Intel P/E) and CCD/NUMA layout. Best-effort: any unreadable fact
// degrades toward "symmetric", never toward steering.
func DetectCPUTopology() CPUTopology {
	return detectTopology(sysfsCPUPath, sysfsNodeDir)
}

func detectTopology(sysCPU, sysNode string) CPUTopology {
	vendor, models := probeCPUInfoModels()
	topo := CPUTopology{Vendor: vendor}

	if hybrid, classes := classifyIntelModels(vendor, models); hybrid {
		groups := probeCoreGroups(sysCPU, len(models))
		topo = topologyFromClasses(classes, groups)
		topo.Vendor = vendor
	}

	if domains := probeL3Domains(sysCPU); len(domains) > 0 {
		topo.CCDs = len(domains)
		topo.CCDCPUCounts = domains
	}
	if n := probeNUMANodes(sysNode); n > 0 {
		topo.NUMANodes = n
	}
	return finalizeTopology(topo)
}

// probeCPUInfoModels reads vendor and per-logical-CPU model numbers from
// /proc/cpuinfo. On Intel hybrid parts each core type reports its own CPUID
// model, which is what classification keys on.
func probeCPUInfoModels() (string, []int) {
	f, err := os.Open(cpuInfoPath)
	if err != nil {
		return "", nil
	}
	defer f.Close()
	return parseCPUInfoModels(f)
}

// parseCPUInfoModels is the testable half of probeCPUInfoModels: it extracts
// the first "vendor_id" and the "model" value of every "processor" block, in
// processor order.
func parseCPUInfoModels(r io.Reader) (string, []int) {
	var vendor string
	var models []int
	current := -1 // index into models for the open processor block

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		switch key {
		case "vendor_id":
			if vendor == "" {
				vendor = val
			}
		case "processor":
			if n, err := strconv.Atoi(val); err == nil && n >= 0 && n <= maxCPUIndex {
				for len(models) <= n {
					models = append(models, -1)
				}
				current = n
			} else {
				// Absurd index (corrupted /proc): fail the parse rather
				// than preallocate gigabytes.
				return vendor, nil
			}
		case "model":
			if current >= 0 && current < len(models) {
				if n, err := strconv.Atoi(val); err == nil {
					models[current] = n
				}
			}
		}
	}
	return vendor, models
}

var cpuDirName = regexp.MustCompile(`^cpu(\d+)$`)

// cpuDirs returns the numeric ids of cpuN directories under the sysfs CPU
// root, ascending.
func cpuDirs(sysCPU string) []int {
	entries, err := os.ReadDir(sysCPU)
	if err != nil {
		return nil
	}
	var ids []int
	for _, e := range entries {
		m := cpuDirName.FindStringSubmatch(e.Name())
		if m == nil || !e.IsDir() {
			continue
		}
		if n, err := strconv.Atoi(m[1]); err == nil {
			ids = append(ids, n)
		}
	}
	sort.Ints(ids)
	return ids
}

// probeCoreGroups reads topology/core_cpus_list for every CPU, returned
// indexed by logical CPU id ("" where unreadable).
func probeCoreGroups(sysCPU string, ncpu int) []string {
	groups := make([]string, ncpu)
	for _, id := range cpuDirs(sysCPU) {
		if id >= ncpu {
			continue
		}
		b, err := os.ReadFile(filepath.Join(sysCPU, "cpu"+strconv.Itoa(id), "topology", "core_cpus_list"))
		if err != nil {
			continue
		}
		groups[id] = strings.TrimSpace(string(b))
	}
	return groups
}

// probeL3Domains groups CPUs by their L3 cache shared_cpu_list. Each distinct
// shared L3 is one die/CCD domain; the return is per-domain logical-CPU
// counts in ascending first-CPU order. Returns nil when no L3 topology is
// exposed (then the caller leaves CCDs at 0).
func probeL3Domains(sysCPU string) []int {
	domains := map[string][]int{}
	var order []string
	for _, id := range cpuDirs(sysCPU) {
		cacheDir := filepath.Join(sysCPU, "cpu"+strconv.Itoa(id), "cache")
		entries, err := os.ReadDir(cacheDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			b, err := os.ReadFile(filepath.Join(cacheDir, e.Name(), "level"))
			if err != nil || strings.TrimSpace(string(b)) != "3" {
				continue
			}
			shared, err := os.ReadFile(filepath.Join(cacheDir, e.Name(), "shared_cpu_list"))
			if err != nil {
				continue
			}
			key := strings.TrimSpace(string(shared))
			if _, ok := domains[key]; !ok {
				order = append(order, key)
			}
			domains[key] = append(domains[key], id)
			break // one L3 per CPU is enough
		}
	}
	if len(domains) == 0 {
		return nil
	}
	sort.Slice(order, func(i, j int) bool {
		a, b := parseCPUList(order[i]), parseCPUList(order[j])
		if len(a) == 0 || len(b) == 0 {
			return order[i] < order[j]
		}
		return a[0] < b[0]
	})
	counts := make([]int, 0, len(order))
	for _, key := range order {
		counts = append(counts, len(domains[key]))
	}
	return counts
}

// probeNUMANodes counts nodeN directories under the sysfs node root.
func probeNUMANodes(sysNode string) int {
	entries, err := os.ReadDir(sysNode)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "node") && e.IsDir() {
			n++
		}
	}
	return n
}
