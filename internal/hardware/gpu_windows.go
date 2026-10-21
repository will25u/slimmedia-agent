//go:build windows

package hardware

import (
	"context"
	"log"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// GPU probing on Windows uses the DXGI adapter enumeration API (the D3D11
// backend required by ADR-006): dxgi.dll ships with every supported Windows
// and reports all display adapters with PCI vendor IDs, headless-safe. The
// WMI (Win32_VideoController), registry, and nvidia-smi fallbacks cover machines
// where DXGI misses secondary/headless adapters or is unavailable.

var dxgiDLL = windows.NewLazySystemDLL("dxgi.dll")
var procCreateDXGIFactory = dxgiDLL.NewProc("CreateDXGIFactory")

// iidDXGIFactory is IID_IDXGIFactory.
var iidDXGIFactory = windows.GUID{
	Data1: 0x7B7166EC, Data2: 0x21C7, Data3: 0x44AE,
	Data4: [8]byte{0xB2, 0x1A, 0xC9, 0xAE, 0x32, 0x1A, 0xE3, 0x69},
}

// dxgiErrorNotFound is DXGI_ERROR_NOT_FOUND, returned by EnumAdapters when
// the adapter index is past the last adapter.
const dxgiErrorNotFound = 0x887A0002

// dxgiAdapterDesc mirrors DXGI_ADAPTER_DESC.
type dxgiAdapterDesc struct {
	description           [128]uint16
	vendorID              uint32
	deviceID              uint32
	subSysID              uint32
	revision              uint32
	dedicatedVideoMemory  uintptr
	dedicatedSystemMemory uintptr
	sharedSystemMemory    uintptr
	adapterLuid           [8]byte
}

// comMethod resolves a COM vtable slot to a callable address. A COM interface
// pointer starts with a pointer to its vtable, each of whose entries is one
// machine word.
func comMethod(obj unsafe.Pointer, slot int) uintptr {
	vtbl := *(*unsafe.Pointer)(obj)
	fn := *(*unsafe.Pointer)(unsafe.Add(vtbl, slot*int(unsafe.Sizeof(vtbl))))
	return uintptr(fn)
}

// probeWindowsGPU fills GPU capability flags. DXGI enumeration is primary;
func hiddenCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NO_WINDOW,
		HideWindow:    true,
	}
	return cmd
}

// WMI Win32_VideoController and registry adapter enumeration supplement DXGI
// to ensure secondary/headless adapters (like Intel UHD 770 alongside an AMD discrete GPU)
// are detected. nvidia-smi is the fallback for NVIDIA installs where DXGI is unavailable.
func probeWindowsGPU(caps *Capabilities) {
	applyAdapters(caps, enumDXGIAdapters())
	applyAdapters(caps, queryWMIAdapters())

	if caps.GPU.HasNvidia || caps.GPU.HasIntel || caps.GPU.HasAMD {
		return
	}
	ctxNvidia, cancelNvidia := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelNvidia()
	if out, err := hiddenCommand(ctxNvidia, "nvidia-smi", "--query-gpu=name,memory.total", "--format=csv,noheader,nounits").Output(); err == nil {
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
		if out, err := hiddenCommand(ctxNvidia2, "nvidia-smi", "--query-gpu=name", "--format=csv,noheader").Output(); err == nil {
			if names := parseNvidiaSMINames(out); len(names) > 0 {
				caps.GPU.HasNvidia = true
				if caps.GPU.Model == "" {
					caps.GPU.Model = names[0]
					caps.GPU.Architecture = ClassifyArchitecture(vendorNVIDIA, 0, names[0])
				}
			}
		} else if ctxNvidia2.Err() == context.DeadlineExceeded {
			log.Printf("nvidia-smi probe timed out after 3 seconds: %v", err)
		}
	}
}

// queryWMIAdapters queries Win32_VideoController via PowerShell or WMIC to detect
// all display adapters installed in Windows, including headless iGPUs.
func queryWMIAdapters() []adapterInfo {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	cmd := hiddenCommand(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command",
		`Get-CimInstance Win32_VideoController | ForEach-Object { "$($_.Name)|$($_.PNPDeviceID)|$($_.AdapterRAM)" }`)
	if out, err := cmd.Output(); err == nil {
		if adapters := parseWMIAdapterLines(string(out)); len(adapters) > 0 {
			return adapters
		}
	}

	ctxWmic, cancelWmic := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelWmic()
	cmdWmic := hiddenCommand(ctxWmic, "wmic", "path", "win32_videocontroller", "get", "Name,PNPDeviceID,AdapterRAM", "/format:csv")
	if out, err := cmdWmic.Output(); err == nil {
		if adapters := parseWMIAdapterCSV(string(out)); len(adapters) > 0 {
			return adapters
		}
	}

	return queryRegistryDisplayAdapters()
}

// queryRegistryDisplayAdapters inspects the Windows display adapter registry class
// to enumerate all installed video controllers.
func queryRegistryDisplayAdapters() []adapterInfo {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SYSTEM\CurrentControlSet\Control\Class\{4d36e968-e325-11ce-bfc1-08002be10318}`, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return nil
	}
	defer k.Close()

	subkeys, err := k.ReadSubKeyNames(-1)
	if err != nil {
		return nil
	}

	var adapters []adapterInfo
	for _, sub := range subkeys {
		if len(sub) != 4 {
			continue
		}
		sk, err := registry.OpenKey(k, sub, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		desc, _, _ := sk.GetStringValue("DriverDesc")
		matchingID, _, _ := sk.GetStringValue("MatchingDeviceId")
		sk.Close()

		if desc == "" {
			continue
		}
		vendorID, deviceID := parsePNPDeviceID(matchingID)
		if vendorID == 0 {
			lower := strings.ToLower(desc)
			if strings.Contains(lower, "intel") || strings.Contains(lower, "uhd") || strings.Contains(lower, "iris") {
				vendorID = vendorIntel
			} else if strings.Contains(lower, "nvidia") || strings.Contains(lower, "geforce") {
				vendorID = vendorNVIDIA
			} else if strings.Contains(lower, "amd") || strings.Contains(lower, "radeon") {
				vendorID = vendorAMD
			}
		}
		adapters = append(adapters, adapterInfo{
			description: desc,
			vendorID:    vendorID,
			deviceID:    deviceID,
			software:    isSoftwareAdapter(vendorID, desc),
		})
	}
	return adapters
}

// enumDXGIAdapters enumerates display adapters via CreateDXGIFactory +
// EnumAdapters + GetDesc, translating each descriptor into adapterInfo.
// Software adapters (WARP, Microsoft Basic Render Driver) are flagged so
// classification can exclude them from hardware eligibility.
func enumDXGIAdapters() []adapterInfo {
	var factory unsafe.Pointer
	r, _, _ := procCreateDXGIFactory.Call(
		uintptr(unsafe.Pointer(&iidDXGIFactory)),
		uintptr(unsafe.Pointer(&factory)),
	)
	if r != 0 || factory == nil {
		return nil
	}
	defer syscall.SyscallN(comMethod(factory, 2), uintptr(factory)) // Release

	var adapters []adapterInfo
	for i := uint32(0); ; i++ {
		var adapter unsafe.Pointer
		r, _, _ := syscall.SyscallN(comMethod(factory, 3), // EnumAdapters
			uintptr(factory), uintptr(i), uintptr(unsafe.Pointer(&adapter)))
		if r == dxgiErrorNotFound || adapter == nil {
			break
		}
		if r != 0 {
			break
		}

		var desc dxgiAdapterDesc
		hr, _, _ := syscall.SyscallN(comMethod(adapter, 4), // GetDesc
			uintptr(adapter), uintptr(unsafe.Pointer(&desc)))
		syscall.SyscallN(comMethod(adapter, 2), uintptr(adapter)) // Release
		if hr != 0 {
			continue
		}

		description := windows.UTF16ToString(desc.description[:])
		adapters = append(adapters, adapterInfo{
			description:           description,
			vendorID:              desc.vendorID,
			deviceID:              desc.deviceID,
			dedicatedVideoMemory:  uint64(desc.dedicatedVideoMemory),
			dedicatedSystemMemory: uint64(desc.dedicatedSystemMemory),
			sharedSystemMemory:    uint64(desc.sharedSystemMemory),
			software:              isSoftwareAdapter(desc.vendorID, description),
		})
	}
	return adapters
}
