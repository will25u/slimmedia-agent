//go:build linux

package hardware

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func probePlatform(caps *Capabilities) {
	probeLinuxCPU(caps)
	probeLinuxGPU(caps)
	probeLinuxMemory(caps)
	probeLinuxOS(caps)
}

func probeLinuxCPU(caps *Capabilities) {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		lower := strings.ToLower(line)

		if strings.HasPrefix(lower, "model name") {
			if caps.CPU.Model == "" {
				parts := strings.SplitN(line, ":", 2)
				if len(parts) == 2 {
					caps.CPU.Model = strings.TrimSpace(parts[1])
				}
			}
		}
		if strings.HasPrefix(lower, "cpu cores") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				n, _ := strconv.Atoi(strings.TrimSpace(parts[1]))
				if n > caps.CPU.PhysicalCores {
					caps.CPU.PhysicalCores = n
				}
			}
		}
		if strings.Contains(lower, " avx2") || strings.Contains(lower, "\tavx2") ||
			strings.HasSuffix(lower, " avx2") {
			caps.CPU.HasAVX2 = true
		}
		if strings.Contains(lower, " avx ") || strings.Contains(lower, "\tavx ") ||
			strings.HasSuffix(lower, " avx") {
			caps.CPU.HasAVX = true
		}
	}

	// AVX2 implies AVX
	if caps.CPU.HasAVX2 {
		caps.CPU.HasAVX = true
	}
	if caps.CPU.PhysicalCores == 0 {
		caps.CPU.PhysicalCores = caps.CPU.LogicalCores
	}
}

func probeLinuxMemory(caps *Capabilities) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		kb, _ := strconv.ParseUint(fields[1], 10, 64)
		if fields[0] == "MemTotal:" {
			caps.Memory.TotalGB = float64(kb) / (1024 * 1024)
		}
	}
}

func checkAvailableMemory() error {
	if os.Getenv("SLIMMEDIA_SKIP_MEMORY_CHECK") == "1" {
		return nil
	}
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return fmt.Errorf("pre-flight memory check failed: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "MemAvailable:" {
			kb, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return fmt.Errorf("pre-flight memory check failed: parse error: %w", err)
			}
			availGB := float64(kb) / (1024 * 1024)
			if availGB < 2.0 {
				return fmt.Errorf("insufficient host memory: %.2f GB available, minimum 2.0 GB required", availGB)
			}
			return nil
		}
	}
	return nil
}

func probeLinuxOS(caps *Capabilities) {
	// Read /etc/os-release for distro name
	f, err := os.Open("/etc/os-release")
	if err != nil {
		caps.OS.Name = "Linux"
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "PRETTY_NAME=") {
			val := strings.TrimPrefix(line, "PRETTY_NAME=")
			val = strings.Trim(val, `"`)
			caps.OS.Name = val
			return
		}
	}
	caps.OS.Name = "Linux"
}

// verifyDeviceNodeAccess checks read/write permissions on hardware acceleration nodes
// (/dev/dri/* and /dev/nvidia*) and logs clear actionable diagnostics if permissions are missing.
func verifyDeviceNodeAccess() {
	var nodes []string

	if entries, err := os.ReadDir("/dev/dri"); err == nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "card") || strings.HasPrefix(e.Name(), "renderD") {
				nodes = append(nodes, filepath.Join("/dev/dri", e.Name()))
			}
		}
	}

	if matches, err := filepath.Glob("/dev/nvidia*"); err == nil {
		for _, m := range matches {
			nodes = append(nodes, m)
		}
	}

	for _, node := range nodes {
		// Check read/write access (R_OK=4, W_OK=2 -> 6)
		if err := syscall.Access(node, 6); err != nil {
			log.Printf("WARNING: Acceleration device node %s exists but lacks read/write permissions (%v). Hardware transcoding may fail. Check user group membership (e.g. 'video' or 'render') or container device passthrough permissions.", node, err)
		}
	}
}

func probeLinuxDRMAdapters() []adapterInfo {
	entries, err := os.ReadDir("/sys/class/drm")
	if err != nil {
		return nil
	}

	var adapters []adapterInfo
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "card") || strings.Contains(name, "-") {
			continue
		}

		devPath := filepath.Join("/sys/class/drm", name, "device")
		vendorBytes, err := os.ReadFile(filepath.Join(devPath, "vendor"))
		if err != nil {
			continue
		}
		deviceBytes, err := os.ReadFile(filepath.Join(devPath, "device"))
		if err != nil {
			continue
		}

		vendorStr := strings.TrimSpace(string(vendorBytes))
		deviceStr := strings.TrimSpace(string(deviceBytes))

		vID, _ := strconv.ParseUint(strings.TrimPrefix(vendorStr, "0x"), 16, 32)
		dID, _ := strconv.ParseUint(strings.TrimPrefix(deviceStr, "0x"), 16, 32)

		var vramBytes uint64
		if vramData, err := os.ReadFile(filepath.Join(devPath, "mem_info_vram_total")); err == nil {
			vramBytes, _ = strconv.ParseUint(strings.TrimSpace(string(vramData)), 10, 64)
		}
		var gttBytes uint64
		if gttData, err := os.ReadFile(filepath.Join(devPath, "mem_info_gtt_total")); err == nil {
			gttBytes, _ = strconv.ParseUint(strings.TrimSpace(string(gttData)), 10, 64)
		}

		adapters = append(adapters, adapterInfo{
			vendorID:             uint32(vID),
			deviceID:             uint32(dID),
			dedicatedVideoMemory: vramBytes,
			sharedSystemMemory:   gttBytes,
		})
	}
	return adapters
}

func probeLinuxGPU(caps *Capabilities) {
	verifyDeviceNodeAccess()

	// 1. Try nvidia-smi with memory query
	ctxNvidia, cancelNvidia := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelNvidia()
	if out, err := exec.CommandContext(ctxNvidia, "nvidia-smi", "--query-gpu=name,memory.total", "--format=csv,noheader,nounits").Output(); err == nil {
		if adapters := parseNvidiaSMIAdapters(out); len(adapters) > 0 {
			applyAdapters(caps, adapters)
			return
		}
	} else if ctxNvidia.Err() == context.DeadlineExceeded {
		log.Printf("nvidia-smi probe timed out after 3 seconds: %v", err)
	}

	if ctxNvidia.Err() == nil {
		ctxNvidia2, cancelNvidia2 := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancelNvidia2()
		if out, err := exec.CommandContext(ctxNvidia2, "nvidia-smi", "--query-gpu=name", "--format=csv,noheader").Output(); err == nil {
			if names := parseNvidiaSMINames(out); len(names) > 0 {
				caps.GPU.HasNvidia = true
				if caps.GPU.Model == "" {
					caps.GPU.Model = names[0]
					caps.GPU.Architecture = ClassifyArchitecture(vendorNVIDIA, 0, names[0])
				}
				return
			}
		} else if ctxNvidia2.Err() == context.DeadlineExceeded {
			log.Printf("nvidia-smi probe timed out after 3 seconds: %v", err)
		}
	}

	// 2. Query DRM nodes for sysfs VRAM and device IDs
	drmAdapters := probeLinuxDRMAdapters()

	// 3. Fallback / supplement with lspci for model strings
	ctxLspci, cancelLspci := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelLspci()
	out, err := exec.CommandContext(ctxLspci, "lspci").Output()
	if err != nil {
		if ctxLspci.Err() == context.DeadlineExceeded {
			log.Printf("lspci probe timed out after 3 seconds: %v", err)
		}
	} else {
		s := strings.ToLower(string(out))
		var lspciAdapters []adapterInfo
		for _, line := range strings.Split(s, "\n") {
			if strings.Contains(line, "vga compatible controller") || strings.Contains(line, "display controller") {
				if strings.Contains(line, "intel") {
					model := extractLSPCIModel(line, "Intel")
					var vram, gtt uint64
					var devID uint32
					for _, da := range drmAdapters {
						if da.vendorID == vendorIntel {
							vram = da.dedicatedVideoMemory
							gtt = da.sharedSystemMemory
							devID = da.deviceID
							break
						}
					}
					lspciAdapters = append(lspciAdapters, adapterInfo{
						description:          model,
						vendorID:             vendorIntel,
						deviceID:             devID,
						dedicatedVideoMemory: vram,
						sharedSystemMemory:   gtt,
					})
				} else if strings.Contains(line, "amd") || strings.Contains(line, "radeon") || strings.Contains(line, "advanced micro") {
					model := extractLSPCIModel(line, "AMD")
					var vram, gtt uint64
					var devID uint32
					for _, da := range drmAdapters {
						if da.vendorID == vendorAMD {
							vram = da.dedicatedVideoMemory
							gtt = da.sharedSystemMemory
							devID = da.deviceID
							break
						}
					}
					lspciAdapters = append(lspciAdapters, adapterInfo{
						description:          model,
						vendorID:             vendorAMD,
						deviceID:             devID,
						dedicatedVideoMemory: vram,
						sharedSystemMemory:   gtt,
					})
				}
			}
		}
		if len(lspciAdapters) > 0 {
			applyAdapters(caps, lspciAdapters)
			return
		}
	}

	// If lspci failed but DRM adapters were found
	if len(drmAdapters) > 0 {
		applyAdapters(caps, drmAdapters)
	}
}

func extractLSPCIModel(line, vendor string) string {
	// lspci line format: "00:02.0 VGA compatible controller: Intel Corporation UHD Graphics 630"
	if idx := strings.Index(line, ":"); idx >= 0 {
		rest := strings.TrimSpace(line[idx+1:])
		if idx2 := strings.Index(rest, ":"); idx2 >= 0 {
			return strings.TrimSpace(rest[idx2+1:])
		}
	}
	return vendor + " GPU"
}

