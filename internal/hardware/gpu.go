package hardware

import (
	"strconv"
	"strings"
)

// PCI vendor IDs for GPU classification, shared by the platform probers.
const (
	vendorNVIDIA    uint32 = 0x10DE
	vendorIntel     uint32 = 0x8086
	vendorAMD       uint32 = 0x1002
	vendorMicrosoft uint32 = 0x1414
)

// adapterInfo is a platform-neutral description of one display adapter, as
// enumerated by DXGI on Windows (ADR-006: D3D11 backend) or lspci/nvidia-smi
// on Linux. It lives untagged so the classification logic below is unit
// testable on any GOOS; only the producers live in build-tagged files.
type adapterInfo struct {
	description           string
	vendorID              uint32
	deviceID              uint32
	dedicatedVideoMemory  uint64
	dedicatedSystemMemory uint64
	sharedSystemMemory    uint64
	// software marks emulated adapters (WARP, Microsoft Basic Render
	// Driver) that must not count as hardware GPU presence.
	software bool
}

// isSoftwareAdapter reports whether an adapter description refers to an
// emulated render device rather than physical GPU hardware.
func isSoftwareAdapter(vendorID uint32, description string) bool {
	if vendorID == vendorMicrosoft {
		return true
	}
	return strings.Contains(strings.ToLower(description), "basic render driver")
}

// isIntegratedGPU reports whether an adapter is an integrated graphics chip
// (Intel UHD/Iris, AMD APU) that primarily relies on dynamically shared system RAM.
func isIntegratedGPU(vendorID uint32, description string) bool {
	lower := strings.ToLower(description)
	if vendorID == vendorIntel {
		if strings.Contains(lower, "arc ") || strings.Contains(lower, "arc(tm)") || strings.Contains(lower, "arc a") || strings.Contains(lower, "arc b") {
			return false
		}
		return true
	}
	if vendorID == vendorAMD {
		if strings.Contains(lower, "apu") || strings.Contains(lower, "vega 3") || strings.Contains(lower, "vega 6") ||
			strings.Contains(lower, "vega 7") || strings.Contains(lower, "vega 8") || strings.Contains(lower, "vega 10") ||
			strings.Contains(lower, "vega 11") || strings.Contains(lower, "radeon graphics") ||
			strings.Contains(lower, "680m") || strings.Contains(lower, "780m") || strings.Contains(lower, "890m") {
			return true
		}
	}
	return false
}

// ClassifyArchitecture identifies the silicon generation / architecture from
// PCI vendor ID, device ID, and model description string.
func ClassifyArchitecture(vendorID uint32, deviceID uint32, description string) string {
	lower := strings.ToLower(description)
	switch vendorID {
	case vendorNVIDIA:
		return classifyNvidiaArch(deviceID, lower)
	case vendorIntel:
		return classifyIntelArch(deviceID, lower)
	case vendorAMD:
		return classifyAMDArch(deviceID, lower)
	default:
		if strings.Contains(lower, "nvidia") || strings.Contains(lower, "geforce") || strings.Contains(lower, "rtx") || strings.Contains(lower, "gtx") {
			return classifyNvidiaArch(deviceID, lower)
		} else if strings.Contains(lower, "intel") {
			return classifyIntelArch(deviceID, lower)
		} else if strings.Contains(lower, "amd") || strings.Contains(lower, "radeon") {
			return classifyAMDArch(deviceID, lower)
		}
		return ""
	}
}

func classifyNvidiaArch(deviceID uint32, lower string) string {
	if strings.Contains(lower, "rtx 50") || strings.Contains(lower, "blackwell") || (deviceID >= 0x2900 && deviceID <= 0x2fff) {
		return "Blackwell"
	}
	if strings.Contains(lower, "rtx 40") || strings.Contains(lower, "ada") || strings.Contains(lower, "l40") || strings.Contains(lower, "l4") || (deviceID >= 0x2680 && deviceID <= 0x28ff) {
		return "Ada Lovelace"
	}
	if strings.Contains(lower, "h100") || strings.Contains(lower, "h200") || strings.Contains(lower, "hopper") || (deviceID >= 0x2300 && deviceID <= 0x23ff) {
		return "Hopper"
	}
	if strings.Contains(lower, "rtx 30") || strings.Contains(lower, "a100") || strings.Contains(lower, "a40") || strings.Contains(lower, "a30") ||
		strings.Contains(lower, "a16") || strings.Contains(lower, "a10") || strings.Contains(lower, "a4000") || strings.Contains(lower, "a5000") ||
		strings.Contains(lower, "a6000") || strings.Contains(lower, "a2000") || strings.Contains(lower, "rtx a") || strings.Contains(lower, "ampere") ||
		(deviceID >= 0x2200 && deviceID <= 0x25ff) {
		return "Ampere"
	}
	if strings.Contains(lower, "rtx 20") || strings.Contains(lower, "gtx 16") || strings.Contains(lower, "t4") || strings.Contains(lower, "titan rtx") ||
		strings.Contains(lower, "quadro rtx") || strings.Contains(lower, "turing") || (deviceID >= 0x1e00 && deviceID <= 0x21ff) {
		return "Turing"
	}
	if strings.Contains(lower, "titan v") || strings.Contains(lower, "v100") || strings.Contains(lower, "volta") || (deviceID >= 0x1d00 && deviceID <= 0x1dff) {
		return "Volta"
	}
	if strings.Contains(lower, "gtx 10") || strings.Contains(lower, "p100") || strings.Contains(lower, "p40") || strings.Contains(lower, "p4") ||
		strings.Contains(lower, "p1000") || strings.Contains(lower, "p2000") || strings.Contains(lower, "p4000") || strings.Contains(lower, "p5000") ||
		strings.Contains(lower, "p6000") || strings.Contains(lower, "pascal") || (deviceID >= 0x1b00 && deviceID <= 0x1cff) {
		return "Pascal"
	}
	if strings.Contains(lower, "gtx 9") || strings.Contains(lower, "gtx 750") || strings.Contains(lower, "m2000") || strings.Contains(lower, "m4000") ||
		strings.Contains(lower, "maxwell") || (deviceID >= 0x1340 && deviceID <= 0x17ff) {
		return "Maxwell"
	}
	if strings.Contains(lower, "gtx 6") || strings.Contains(lower, "gtx 7") || strings.Contains(lower, "k2000") || strings.Contains(lower, "k4000") ||
		strings.Contains(lower, "k5000") || strings.Contains(lower, "kepler") || (deviceID >= 0x0fc0 && deviceID <= 0x11ff) {
		return "Kepler"
	}
	return ""
}

func classifyIntelArch(deviceID uint32, lower string) string {
	clean := strings.ReplaceAll(strings.ReplaceAll(lower, "(tm)", ""), "(r)", "")
	clean = strings.Join(strings.Fields(clean), " ")
	if strings.Contains(clean, "arc b") || strings.Contains(clean, "battlemage") || strings.Contains(clean, "lunar lake") {
		return "QuickSync Gen 12.7"
	}
	if strings.Contains(clean, "arc a") || strings.Contains(clean, "arc ") || strings.Contains(clean, "meteor lake") {
		return "QuickSync Gen 12.5"
	}
	if strings.Contains(clean, "uhd 7") || strings.Contains(clean, "iris xe") || strings.Contains(clean, "tiger lake") ||
		strings.Contains(clean, "alder lake") || strings.Contains(clean, "raptor lake") || strings.Contains(clean, "arrow lake") ||
		(deviceID >= 0x4600 && deviceID <= 0x46ff) || (deviceID >= 0x4c00 && deviceID <= 0x4cff) || (deviceID >= 0x9a00 && deviceID <= 0x9aff) {
		return "QuickSync Gen 12"
	}
	if strings.Contains(lower, "iris plus") || strings.Contains(lower, "ice lake") || strings.Contains(lower, "uhd 9") || (deviceID >= 0x8a00 && deviceID <= 0x8aff) {
		return "QuickSync Gen 11"
	}
	if strings.Contains(lower, "uhd 6") || strings.Contains(lower, "kaby lake") || strings.Contains(lower, "coffee lake") ||
		strings.Contains(lower, "comet lake") || (deviceID >= 0x3e00 && deviceID <= 0x3eff) || (deviceID >= 0x5900 && deviceID <= 0x59ff) || (deviceID >= 0x9b00 && deviceID <= 0x9bff) {
		return "QuickSync Gen 9.5"
	}
	if strings.Contains(lower, "hd 5") || strings.Contains(lower, "skylake") || (deviceID >= 0x1900 && deviceID <= 0x19ff) {
		return "QuickSync Gen 9"
	}
	if strings.Contains(lower, "hd 6000") || strings.Contains(lower, "iris 6100") || strings.Contains(lower, "broadwell") {
		return "QuickSync Gen 8"
	}
	if strings.Contains(lower, "hd 4600") || strings.Contains(lower, "hd 4400") || strings.Contains(lower, "haswell") {
		return "QuickSync Gen 7.5"
	}
	if strings.Contains(lower, "hd 4000") || strings.Contains(lower, "hd 2500") || strings.Contains(lower, "ivy bridge") {
		return "QuickSync Gen 7"
	}
	return ""
}

func classifyAMDArch(deviceID uint32, lower string) string {
	if strings.Contains(lower, "rx 7") || strings.Contains(lower, "780m") || strings.Contains(lower, "890m") || strings.Contains(lower, "rdna 3") || strings.Contains(lower, "rdna3") {
		return "RDNA 3"
	}
	if strings.Contains(lower, "rx 6") || strings.Contains(lower, "680m") || strings.Contains(lower, "660m") || strings.Contains(lower, "rdna 2") || strings.Contains(lower, "rdna2") {
		return "RDNA 2"
	}
	if strings.Contains(lower, "rx 5700") || strings.Contains(lower, "rx 5600") || strings.Contains(lower, "rx 5500") || strings.Contains(lower, "rdna 1") || strings.Contains(lower, "rdna") {
		return "RDNA 1"
	}
	if strings.Contains(lower, "vega") || strings.Contains(lower, "radeon vii") {
		return "GCN 5 (Vega)"
	}
	if strings.Contains(lower, "rx 580") || strings.Contains(lower, "rx 570") || strings.Contains(lower, "rx 480") ||
		strings.Contains(lower, "rx 470") || strings.Contains(lower, "rx 560") || strings.Contains(lower, "rx 550") || strings.Contains(lower, "polaris") {
		return "GCN 4 (Polaris)"
	}
	if strings.Contains(lower, "r9") || strings.Contains(lower, "r7") || strings.Contains(lower, "hd 7") || strings.Contains(lower, "hd 8") || strings.Contains(lower, "gcn") {
		return "GCN"
	}
	return ""
}

// applyAdapters folds enumerated adapters into the GPU capability flags.
// Software adapters are excluded so a GPU-less machine never claims hardware
// encode eligibility; the first hardware adapter's description becomes the
// reported model.
func applyAdapters(caps *Capabilities, adapters []adapterInfo) {
	for _, a := range adapters {
		if a.software {
			continue
		}
		switch a.vendorID {
		case vendorNVIDIA:
			caps.GPU.HasNvidia = true
		case vendorIntel:
			caps.GPU.HasIntel = true
		case vendorAMD:
			caps.GPU.HasAMD = true
		default:
			lower := strings.ToLower(a.description)
			if strings.Contains(lower, "nvidia") || strings.Contains(lower, "geforce") {
				caps.GPU.HasNvidia = true
			}
			if strings.Contains(lower, "intel") || strings.Contains(lower, "uhd graphics") || strings.Contains(lower, "iris") {
				caps.GPU.HasIntel = true
			}
			if strings.Contains(lower, "amd") || strings.Contains(lower, "radeon") {
				caps.GPU.HasAMD = true
			}
		}
		if caps.GPU.Model == "" {
			caps.GPU.Model = a.description
			caps.GPU.Architecture = ClassifyArchitecture(a.vendorID, a.deviceID, a.description)
			caps.GPU.DedicatedVRAMMB = a.dedicatedVideoMemory / (1024 * 1024)
			caps.GPU.SharedSystemMemMB = a.sharedSystemMemory / (1024 * 1024)
			if isIntegratedGPU(a.vendorID, a.description) && caps.GPU.SharedSystemMemMB == 0 && caps.Memory.TotalGB > 0 {
				caps.GPU.SharedSystemMemMB = uint64(caps.Memory.TotalGB * 0.5 * 1024)
			}
			caps.GPU.TotalMemoryMB = caps.GPU.DedicatedVRAMMB + caps.GPU.SharedSystemMemMB
		}
	}
}

// parseNvidiaSMINames parses the line-oriented output of
// `nvidia-smi --query-gpu=name --format=csv,noheader` into trimmed GPU names.
func parseNvidiaSMINames(out []byte) []string {
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		if name := strings.TrimSpace(line); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// parseNvidiaSMIAdapters parses the output of
// `nvidia-smi --query-gpu=name,memory.total --format=csv,noheader,nounits`
func parseNvidiaSMIAdapters(out []byte) []adapterInfo {
	var adapters []adapterInfo
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, ",")
		name := strings.TrimSpace(parts[0])
		var vramBytes uint64
		if len(parts) >= 2 {
			if mb, err := strconv.ParseUint(strings.TrimSpace(parts[1]), 10, 64); err == nil {
				vramBytes = mb * 1024 * 1024
			}
		}
		adapters = append(adapters, adapterInfo{
			description:          name,
			vendorID:             vendorNVIDIA,
			dedicatedVideoMemory: vramBytes,
		})
	}
	return adapters
}

// parsePNPDeviceID extracts PCI vendor and device IDs from a PNPDeviceID string
// (e.g. `PCI\VEN_8086&DEV_4690&SUBSYS_...`).
func parsePNPDeviceID(pnpID string) (vendorID, deviceID uint32) {
	upper := strings.ToUpper(pnpID)
	if idx := strings.Index(upper, "VEN_"); idx != -1 && len(upper) >= idx+8 {
		venStr := upper[idx+4 : idx+8]
		if val, err := strconv.ParseUint(venStr, 16, 32); err == nil {
			vendorID = uint32(val)
		}
	}
	if idx := strings.Index(upper, "DEV_"); idx != -1 && len(upper) >= idx+8 {
		devStr := upper[idx+4 : idx+8]
		if val, err := strconv.ParseUint(devStr, 16, 32); err == nil {
			deviceID = uint32(val)
		}
	}
	return
}

// parseWMIAdapterLines parses delimited lines (e.g. from PowerShell Get-CimInstance)
// formatted as Name|PNPDeviceID|AdapterRAM into adapterInfo records.
func parseWMIAdapterLines(output string) []adapterInfo {
	var adapters []adapterInfo
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		name := strings.TrimSpace(parts[0])
		if name == "" {
			continue
		}
		var pnpID string
		if len(parts) > 1 {
			pnpID = strings.TrimSpace(parts[1])
		}
		var ramBytes uint64
		if len(parts) > 2 {
			if b, err := strconv.ParseUint(strings.TrimSpace(parts[2]), 10, 64); err == nil {
				ramBytes = b
			}
		}
		vendorID, deviceID := parsePNPDeviceID(pnpID)
		if vendorID == 0 {
			lower := strings.ToLower(name)
			if strings.Contains(lower, "intel") || strings.Contains(lower, "uhd") || strings.Contains(lower, "iris") {
				vendorID = vendorIntel
			} else if strings.Contains(lower, "nvidia") || strings.Contains(lower, "geforce") {
				vendorID = vendorNVIDIA
			} else if strings.Contains(lower, "amd") || strings.Contains(lower, "radeon") {
				vendorID = vendorAMD
			}
		}
		adapters = append(adapters, adapterInfo{
			description:          name,
			vendorID:             vendorID,
			deviceID:             deviceID,
			dedicatedVideoMemory: ramBytes,
			software:             isSoftwareAdapter(vendorID, name),
		})
	}
	return adapters
}

// parseWMIAdapterCSV parses CSV output from `wmic path win32_videocontroller`
// into adapterInfo records.
func parseWMIAdapterCSV(output string) []adapterInfo {
	var adapters []adapterInfo
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(strings.ToLower(line), "node,") {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) < 2 {
			continue
		}
		var name, pnpID string
		var ramBytes uint64
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if strings.HasPrefix(strings.ToUpper(p), "PCI\\") {
				pnpID = p
			} else if b, err := strconv.ParseUint(p, 10, 64); err == nil && b > 0 {
				ramBytes = b
			} else if len(p) > 0 && !strings.EqualFold(p, "node") {
				name = p
			}
		}
		if name == "" {
			continue
		}
		vendorID, deviceID := parsePNPDeviceID(pnpID)
		if vendorID == 0 {
			lower := strings.ToLower(name)
			if strings.Contains(lower, "intel") || strings.Contains(lower, "uhd") || strings.Contains(lower, "iris") {
				vendorID = vendorIntel
			} else if strings.Contains(lower, "nvidia") || strings.Contains(lower, "geforce") {
				vendorID = vendorNVIDIA
			} else if strings.Contains(lower, "amd") || strings.Contains(lower, "radeon") {
				vendorID = vendorAMD
			}
		}
		adapters = append(adapters, adapterInfo{
			description:          name,
			vendorID:             vendorID,
			deviceID:             deviceID,
			dedicatedVideoMemory: ramBytes,
			software:             isSoftwareAdapter(vendorID, name),
		})
	}
	return adapters
}

// isIntelCPUWithIntegratedGraphics reports whether an Intel CPU model is known
// to include integrated graphics (e.g. Intel Core i5-14500 with Intel UHD 770).
func isIntelCPUWithIntegratedGraphics(cpuModel string) bool {
	lower := strings.ToLower(cpuModel)
	if !strings.Contains(lower, "intel") {
		return false
	}
	// Specifically Intel Core i5-14500 (UHD 770)
	if strings.Contains(lower, "14500") && !strings.Contains(lower, "14500f") {
		return true
	}
	// General Intel Core processors (non-F/KF variants)
	if strings.Contains(lower, "core") {
		fields := strings.Fields(lower)
		for _, f := range fields {
			if strings.HasPrefix(f, "i3-") || strings.HasPrefix(f, "i5-") || strings.HasPrefix(f, "i7-") || strings.HasPrefix(f, "i9-") {
				return !strings.HasSuffix(f, "f") && !strings.HasSuffix(f, "kf")
			}
		}
	}
	return false
}
