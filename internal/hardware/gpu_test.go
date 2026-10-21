package hardware

import (
	"reflect"
	"testing"
)

func TestApplyAdapters(t *testing.T) {
	tests := []struct {
		name     string
		adapters []adapterInfo
		wantN    bool
		wantI    bool
		wantA    bool
		wantM    string
	}{
		{
			name:     "no adapters leaves flags unset",
			adapters: nil,
		},
		{
			name:     "nvidia discrete gpu",
			adapters: []adapterInfo{{description: "NVIDIA GeForce RTX 4070", vendorID: vendorNVIDIA}},
			wantN:    true,
			wantM:    "NVIDIA GeForce RTX 4070",
		},
		{
			name:     "intel integrated gpu",
			adapters: []adapterInfo{{description: "Intel(R) UHD Graphics 770", vendorID: vendorIntel}},
			wantI:    true,
			wantM:    "Intel(R) UHD Graphics 770",
		},
		{
			name:     "amd gpu",
			adapters: []adapterInfo{{description: "AMD Radeon RX 7800 XT", vendorID: vendorAMD}},
			wantA:    true,
			wantM:    "AMD Radeon RX 7800 XT",
		},
		{
			name: "microsoft basic render driver is not hardware",
			adapters: []adapterInfo{
				{description: "Microsoft Basic Render Driver", vendorID: vendorMicrosoft, software: true},
			},
		},
		{
			name: "software adapter mixed with real gpu is skipped",
			adapters: []adapterInfo{
				{description: "Microsoft Basic Render Driver", vendorID: vendorMicrosoft, software: true},
				{description: "NVIDIA GeForce RTX 4070", vendorID: vendorNVIDIA},
			},
			wantN: true,
			wantM: "NVIDIA GeForce RTX 4070",
		},
		{
			name: "first hardware adapter wins the model slot",
			adapters: []adapterInfo{
				{description: "Intel(R) UHD Graphics 770", vendorID: vendorIntel},
				{description: "NVIDIA GeForce RTX 4070", vendorID: vendorNVIDIA},
			},
			wantN: true,
			wantI: true,
			wantM: "Intel(R) UHD Graphics 770",
		},
		{
			name:     "unknown vendor records model without vendor flag",
			adapters: []adapterInfo{{description: "VMware SVGA II Adapter", vendorID: 0x15AD}},
			wantM:    "VMware SVGA II Adapter",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var caps Capabilities
			applyAdapters(&caps, tt.adapters)
			if caps.GPU.HasNvidia != tt.wantN || caps.GPU.HasIntel != tt.wantI || caps.GPU.HasAMD != tt.wantA {
				t.Errorf("vendor flags = nvidia:%v intel:%v amd:%v, want nvidia:%v intel:%v amd:%v",
					caps.GPU.HasNvidia, caps.GPU.HasIntel, caps.GPU.HasAMD, tt.wantN, tt.wantI, tt.wantA)
			}
			if caps.GPU.Model != tt.wantM {
				t.Errorf("model = %q, want %q", caps.GPU.Model, tt.wantM)
			}
		})
	}
}

func TestClassifyArchitecture(t *testing.T) {
	tests := []struct {
		name        string
		vendorID    uint32
		deviceID    uint32
		description string
		wantArch    string
	}{
		// NVIDIA
		{"nvidia ada 4090", vendorNVIDIA, 0x2684, "NVIDIA GeForce RTX 4090", "Ada Lovelace"},
		{"nvidia ampere 3080", vendorNVIDIA, 0x2206, "NVIDIA GeForce RTX 3080", "Ampere"},
		{"nvidia turing 2080", vendorNVIDIA, 0x1e82, "NVIDIA GeForce RTX 2080", "Turing"},
		{"nvidia turing gtx 1660", vendorNVIDIA, 0x2184, "NVIDIA GeForce GTX 1660 Ti", "Turing"},
		{"nvidia pascal 1080", vendorNVIDIA, 0x1b80, "NVIDIA GeForce GTX 1080", "Pascal"},
		{"nvidia maxwell 980", vendorNVIDIA, 0x13c0, "NVIDIA GeForce GTX 980", "Maxwell"},
		// Intel
		{"intel uhd 770 gen12", vendorIntel, 0x4690, "Intel(R) UHD Graphics 770", "QuickSync Gen 12"},
		{"intel iris xe gen12", vendorIntel, 0x9a49, "Intel(R) Iris(R) Xe Graphics", "QuickSync Gen 12"},
		{"intel arc a770 gen12.5", vendorIntel, 0x56a0, "Intel(R) Arc(TM) A770 Graphics", "QuickSync Gen 12.5"},
		{"intel arc b580 gen12.7", vendorIntel, 0x0000, "Intel(R) Arc(TM) B580 Graphics", "QuickSync Gen 12.7"},
		{"intel uhd 630 gen9.5", vendorIntel, 0x3e92, "Intel(R) UHD Graphics 630", "QuickSync Gen 9.5"},
		{"intel hd 530 gen9", vendorIntel, 0x1912, "Intel(R) HD Graphics 530", "QuickSync Gen 9"},
		// AMD
		{"amd rx 7900 rdna3", vendorAMD, 0x744c, "AMD Radeon RX 7900 XTX", "RDNA 3"},
		{"amd rx 6800 rdna2", vendorAMD, 0x73bf, "AMD Radeon RX 6800 XT", "RDNA 2"},
		{"amd rx 5700 rdna1", vendorAMD, 0x731f, "AMD Radeon RX 5700 XT", "RDNA 1"},
		{"amd vega 64 gcn5", vendorAMD, 0x687f, "Radeon RX Vega 64", "GCN 5 (Vega)"},
		{"amd rx 580 gcn4", vendorAMD, 0x67df, "Radeon RX 580 Series", "GCN 4 (Polaris)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyArchitecture(tt.vendorID, tt.deviceID, tt.description)
			if got != tt.wantArch {
				t.Errorf("ClassifyArchitecture(%#x, %#x, %q) = %q, want %q", tt.vendorID, tt.deviceID, tt.description, got, tt.wantArch)
			}
		})
	}
}

func TestApplyAdapters_VRAMAndSharedMemory(t *testing.T) {
	t.Run("discrete gpu preserves dedicated vram", func(t *testing.T) {
		var caps Capabilities
		caps.Memory.TotalGB = 32.0
		adapters := []adapterInfo{
			{
				description:          "NVIDIA GeForce RTX 3080",
				vendorID:             vendorNVIDIA,
				deviceID:             0x2206,
				dedicatedVideoMemory: 10 * 1024 * 1024 * 1024, // 10 GB
			},
		}
		applyAdapters(&caps, adapters)
		if caps.GPU.DedicatedVRAMMB != 10240 {
			t.Errorf("DedicatedVRAMMB = %d, want 10240", caps.GPU.DedicatedVRAMMB)
		}
		if caps.GPU.SharedSystemMemMB != 0 {
			t.Errorf("SharedSystemMemMB = %d, want 0", caps.GPU.SharedSystemMemMB)
		}
		if caps.GPU.TotalMemoryMB != 10240 {
			t.Errorf("TotalMemoryMB = %d, want 10240", caps.GPU.TotalMemoryMB)
		}
		if caps.GPU.Architecture != "Ampere" {
			t.Errorf("Architecture = %q, want Ampere", caps.GPU.Architecture)
		}
	})

	t.Run("integrated intel gpu calculates shared system memory", func(t *testing.T) {
		var caps Capabilities
		caps.Memory.TotalGB = 16.0
		adapters := []adapterInfo{
			{
				description:          "Intel(R) UHD Graphics 770",
				vendorID:             vendorIntel,
				deviceID:             0x4690,
				dedicatedVideoMemory: 128 * 1024 * 1024, // 128 MB dedicated
			},
		}
		applyAdapters(&caps, adapters)
		if caps.GPU.DedicatedVRAMMB != 128 {
			t.Errorf("DedicatedVRAMMB = %d, want 128", caps.GPU.DedicatedVRAMMB)
		}
		// 50% of 16 GB = 8 GB = 8192 MB
		if caps.GPU.SharedSystemMemMB != 8192 {
			t.Errorf("SharedSystemMemMB = %d, want 8192", caps.GPU.SharedSystemMemMB)
		}
		if caps.GPU.TotalMemoryMB != 128+8192 {
			t.Errorf("TotalMemoryMB = %d, want %d", caps.GPU.TotalMemoryMB, 128+8192)
		}
		if caps.GPU.Architecture != "QuickSync Gen 12" {
			t.Errorf("Architecture = %q, want QuickSync Gen 12", caps.GPU.Architecture)
		}
	})

	t.Run("integrated amd apu calculates shared system memory", func(t *testing.T) {
		var caps Capabilities
		caps.Memory.TotalGB = 32.0
		adapters := []adapterInfo{
			{
				description:          "AMD Radeon(TM) 780M Graphics",
				vendorID:             vendorAMD,
				dedicatedVideoMemory: 512 * 1024 * 1024, // 512 MB dedicated
			},
		}
		applyAdapters(&caps, adapters)
		if caps.GPU.DedicatedVRAMMB != 512 {
			t.Errorf("DedicatedVRAMMB = %d, want 512", caps.GPU.DedicatedVRAMMB)
		}
		// 50% of 32 GB = 16 GB = 16384 MB
		if caps.GPU.SharedSystemMemMB != 16384 {
			t.Errorf("SharedSystemMemMB = %d, want 16384", caps.GPU.SharedSystemMemMB)
		}
		if caps.GPU.TotalMemoryMB != 512+16384 {
			t.Errorf("TotalMemoryMB = %d, want %d", caps.GPU.TotalMemoryMB, 512+16384)
		}
		if caps.GPU.Architecture != "RDNA 3" {
			t.Errorf("Architecture = %q, want RDNA 3", caps.GPU.Architecture)
		}
	})
}

func TestParseNvidiaSMIAdapters(t *testing.T) {
	out := []byte("NVIDIA GeForce RTX 3080, 10240\nNVIDIA GeForce RTX 4070, 12288\n")
	adapters := parseNvidiaSMIAdapters(out)
	if len(adapters) != 2 {
		t.Fatalf("expected 2 adapters, got %d", len(adapters))
	}
	if adapters[0].description != "NVIDIA GeForce RTX 3080" || adapters[0].dedicatedVideoMemory != 10240*1024*1024 {
		t.Errorf("unexpected adapter 0: %+v", adapters[0])
	}
	if adapters[1].description != "NVIDIA GeForce RTX 4070" || adapters[1].dedicatedVideoMemory != 12288*1024*1024 {
		t.Errorf("unexpected adapter 1: %+v", adapters[1])
	}
}

func TestIsSoftwareAdapter(t *testing.T) {
	tests := []struct {
		name        string
		vendorID    uint32
		description string
		want        bool
	}{
		{"microsoft vendor id", vendorMicrosoft, "Microsoft Basic Render Driver", true},
		{"basic render driver by name", 0x1414, "Microsoft Basic Render Driver", true},
		{"basic render driver case insensitive", 0x9999, "MICROSOFT BASIC RENDER DRIVER", true},
		{"nvidia hardware", vendorNVIDIA, "NVIDIA GeForce RTX 4070", false},
		{"intel hardware", vendorIntel, "Intel(R) UHD Graphics 770", false},
		{"amd hardware", vendorAMD, "AMD Radeon RX 7800 XT", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isSoftwareAdapter(tt.vendorID, tt.description); got != tt.want {
				t.Errorf("isSoftwareAdapter(%#x, %q) = %v, want %v", tt.vendorID, tt.description, got, tt.want)
			}
		})
	}
}

func TestParseNvidiaSMINames(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want []string
	}{
		{"empty output", "", nil},
		{"single gpu", "NVIDIA GeForce RTX 4070\n", []string{"NVIDIA GeForce RTX 4070"}},
		{"multiple gpus", "NVIDIA GeForce RTX 4070\nNVIDIA GeForce GTX 1080 Ti\n", []string{"NVIDIA GeForce RTX 4070", "NVIDIA GeForce GTX 1080 Ti"}},
		{"blank lines skipped", "\n  NVIDIA GeForce RTX 4070  \n\n", []string{"NVIDIA GeForce RTX 4070"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseNvidiaSMINames([]byte(tt.out))
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseNvidiaSMINames(%q) = %v, want %v", tt.out, got, tt.want)
			}
		})
	}
}

func TestParsePNPDeviceID(t *testing.T) {
	tests := []struct {
		pnpID        string
		wantVendorID uint32
		wantDeviceID uint32
	}{
		{"PCI\\VEN_8086&DEV_4690&SUBSYS_12348086&REV_0C\\3&11583659&0&10", vendorIntel, 0x4690},
		{"PCI\\VEN_1002&DEV_744C&SUBSYS_00001002&REV_C8\\4&22334455&0&00", vendorAMD, 0x744C},
		{"PCI\\VEN_10DE&DEV_2684&SUBSYS_123410DE&REV_A1\\4&12345678&0&00", vendorNVIDIA, 0x2684},
		{"ROOT\\BASICRENDER\\0000", 0, 0},
	}
	for _, tt := range tests {
		v, d := parsePNPDeviceID(tt.pnpID)
		if v != tt.wantVendorID || d != tt.wantDeviceID {
			t.Errorf("parsePNPDeviceID(%q) = (%#x, %#x), want (%#x, %#x)", tt.pnpID, v, d, tt.wantVendorID, tt.wantDeviceID)
		}
	}
}

func TestParseWMIAdapters(t *testing.T) {
	// Simulated PowerShell output with AMD GPU and Intel UHD 770
	psOut := "AMD Radeon RX 7800 XT|PCI\\VEN_1002&DEV_744C&SUBSYS_00001002|17179869184\n" +
		"Intel(R) UHD Graphics 770|PCI\\VEN_8086&DEV_4690&SUBSYS_12348086|134217728\n"

	adapters := parseWMIAdapterLines(psOut)
	if len(adapters) != 2 {
		t.Fatalf("expected 2 adapters, got %d", len(adapters))
	}
	if adapters[0].vendorID != vendorAMD || adapters[0].deviceID != 0x744C {
		t.Errorf("adapter 0 = %+v, want AMD", adapters[0])
	}
	if adapters[1].vendorID != vendorIntel || adapters[1].deviceID != 0x4690 {
		t.Errorf("adapter 1 = %+v, want Intel UHD 770", adapters[1])
	}

	var caps Capabilities
	applyAdapters(&caps, adapters)
	if !caps.GPU.HasAMD {
		t.Error("expected HasAMD = true")
	}
	if !caps.GPU.HasIntel {
		t.Error("expected HasIntel = true")
	}
}

func TestIsIntelCPUWithIntegratedGraphics(t *testing.T) {
	tests := []struct {
		cpu  string
		want bool
	}{
		{"14th Gen Intel(R) Core(TM) i5-14500", true},
		{"Intel(R) Core(TM) i5-14500", true},
		{"Intel(R) Core(TM) i5-14500F", false},
		{"Intel(R) Core(TM) i7-14700K", true},
		{"Intel(R) Core(TM) i7-14700KF", false},
		{"AMD Ryzen 7 7800X3D", false},
	}
	for _, tt := range tests {
		if got := isIntelCPUWithIntegratedGraphics(tt.cpu); got != tt.want {
			t.Errorf("isIntelCPUWithIntegratedGraphics(%q) = %v, want %v", tt.cpu, got, tt.want)
		}
	}
}
