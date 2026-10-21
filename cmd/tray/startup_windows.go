//go:build windows

package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// This file talks to the Windows shell through raw COM vtables instead of
// github.com/go-ole/go-ole (which was considered and rejected: the two
// calls needed here do not justify a dependency, and golang.org/x/sys/windows
// plus syscall.SyscallN cover them). The vtable layouts below follow the
// MS-SHLLINK / MS-DCOM reference layouts so every offset is explicit.

var (
	ole32                = windows.NewLazySystemDLL("ole32.dll")
	procCoCreateInstance = ole32.NewProc("CoCreateInstance")
)

// CLSID_ShellLink identifies the ShellLink COM object (explorer shortcut).
var clsidShellLink = windows.GUID{
	Data1: 0x00021401,
	Data2: 0x0000,
	Data3: 0x0000,
	Data4: [8]byte{0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46},
}

// iidIShellLinkW is the IShellLinkW interface ID.
var iidIShellLinkW = windows.GUID{
	Data1: 0x000214F9,
	Data2: 0x0000,
	Data3: 0x0000,
	Data4: [8]byte{0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46},
}

// iidIPersistFile is the IPersistFile interface ID.
var iidIPersistFile = windows.GUID{
	Data1: 0x0000010B,
	Data2: 0x0000,
	Data3: 0x0000,
	Data4: [8]byte{0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46},
}

// iShellLinkVtbl mirrors the IShellLinkW vtable: IUnknown (3 entries)
// followed by the 18 documented methods. The layout matches the
// reference ABI (MS-SHLLINK / shobjidl_core.h) exactly.
type iShellLinkVtbl struct {
	queryInterface      uintptr
	addRef              uintptr
	release             uintptr
	getPath             uintptr
	getIDList           uintptr
	setIDList           uintptr
	getDescription      uintptr
	setDescription      uintptr
	getWorkingDirectory uintptr
	setWorkingDirectory uintptr
	getArguments        uintptr
	setArguments        uintptr
	getHotkey           uintptr
	setHotkey           uintptr
	getShowCmd          uintptr
	setShowCmd          uintptr
	getIconLocation     uintptr
	setIconLocation     uintptr
	setRelativePath     uintptr
	resolve             uintptr
	setPath             uintptr
}

// iPersistFileVtbl mirrors the IPersistFile vtable: IUnknown (3 entries)
// plus GetClassID, IsDirty, Load, Save, SaveCompleted, GetCurFile. Save is
// the 7th entry (index 6).
type iPersistFileVtbl struct {
	queryInterface uintptr
	addRef         uintptr
	release        uintptr
	getClassID     uintptr
	isDirty        uintptr
	load           uintptr
	save           uintptr
	saveCompleted  uintptr
	getCurFile     uintptr
}

// comObject is a raw COM interface pointer: the first word points at the
// interface's vtable.
type comObject struct {
	vtbl *uintptr
}

// ensureAutostart makes the tray app launch on sign-in by creating or
// refreshing the Startup folder shortcut so it points to the active executable.
// It is best-effort: failures are logged, never fatal.
func ensureAutostart() {
	shortcut := startupShortcutPath(os.Getenv("APPDATA"))
	if _, err := os.Stat(shortcut); err != nil {
		if err := enableAutostart(); err != nil {
			log.Printf("Autostart setup failed (continuing without it): %v", err)
		}
		return
	}

	// Shortcut already exists: verify its target points to the active executable.
	// If it was created without a target (prior vtable bug) or points to an older
	// download (e.g. slimmedia-agent-windows (16).exe instead of (17).exe), refresh it.
	exe, err := os.Executable()
	if err != nil {
		return
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return
	}

	target, err := getShortcutTarget(shortcut)
	if err != nil || !strings.EqualFold(filepath.Clean(target), filepath.Clean(exe)) {
		if err := enableAutostart(); err != nil {
			log.Printf("Autostart refresh failed: %v", err)
		}
	}
}

// getShortcutTarget reads the target path stored inside an existing .lnk file.
func getShortcutTarget(shortcut string) (string, error) {
	if hr := windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED); hr != nil {
		if errno, ok := hr.(syscall.Errno); !ok || (errno != changedMode && errno != sFalse) {
			return "", fmt.Errorf("CoInitializeEx failed: %w", hr)
		}
	} else {
		defer windows.CoUninitialize()
	}

	obj, err := coCreateInstance(&clsidShellLink, &iidIShellLinkW)
	if err != nil {
		return "", err
	}
	linkVtbl := (*iShellLinkVtbl)(unsafe.Pointer(obj.vtbl))
	defer syscall.SyscallN(linkVtbl.release, uintptr(unsafe.Pointer(obj)))

	var pfRaw unsafe.Pointer
	if r, _, _ := syscall.SyscallN(linkVtbl.queryInterface, uintptr(unsafe.Pointer(obj)), uintptr(unsafe.Pointer(&iidIPersistFile)), uintptr(unsafe.Pointer(&pfRaw)), 0); r != 0 {
		return "", fmt.Errorf("QueryInterface(IPersistFile) failed: HRESULT 0x%X", r)
	}
	pf := (*comObject)(pfRaw)
	pfVtbl := (*iPersistFileVtbl)(unsafe.Pointer(pf.vtbl))
	defer syscall.SyscallN(pfVtbl.release, uintptr(unsafe.Pointer(pf)))

	shortcutPtr, err := windows.UTF16PtrFromString(shortcut)
	if err != nil {
		return "", err
	}
	if r, _, _ := syscall.SyscallN(pfVtbl.load, uintptr(unsafe.Pointer(pf)), uintptr(unsafe.Pointer(shortcutPtr)), 0); r != 0 {
		return "", fmt.Errorf("IPersistFile.Load failed: HRESULT 0x%X", r)
	}

	var buf [windows.MAX_PATH]uint16
	if r, _, _ := syscall.SyscallN(linkVtbl.getPath, uintptr(unsafe.Pointer(obj)), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0, 0); r != 0 {
		return "", fmt.Errorf("IShellLinkW.GetPath failed: HRESULT 0x%X", r)
	}
	return windows.UTF16ToString(buf[:]), nil
}

// enableAutostart creates the per-user Startup shortcut pointing at the
// running executable. The COM calls run on the calling goroutine, which
// must CoInitialize first (this goroutine does; callers that live on a
// thread already pumping a message loop must instead spawn a fresh
// goroutine, as the tray menu toggle does).
func enableAutostart() error {
	if hr := windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED); hr != nil {
		// S_FALSE (already initialized, same apartment) is fine; only a
		// genuinely different mode (RPC_E_CHANGED_MODE) or another error is
		// fatal. CoUninitialize below only runs when this call succeeded, so
		// a prior init count on this thread is never unbalanced.
		if errno, ok := hr.(syscall.Errno); !ok || (errno != changedMode && errno != sFalse) {
			return fmt.Errorf("CoInitializeEx failed: %w", hr)
		}
	} else {
		defer windows.CoUninitialize()
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot locate own executable: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return fmt.Errorf("cannot resolve own executable path: %w", err)
	}

	obj, err := coCreateInstance(&clsidShellLink, &iidIShellLinkW)
	if err != nil {
		return err
	}
	linkVtbl := (*iShellLinkVtbl)(unsafe.Pointer(obj.vtbl))
	defer syscall.SyscallN(linkVtbl.release, uintptr(unsafe.Pointer(obj)))

	pathPtr, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return err
	}
	if r, _, _ := syscall.SyscallN(linkVtbl.setPath, uintptr(unsafe.Pointer(obj)), uintptr(unsafe.Pointer(pathPtr)), 0); r != 0 {
		return fmt.Errorf("IShellLinkW.SetPath failed: HRESULT 0x%X", r)
	}

	workDirPtr, err := windows.UTF16PtrFromString(filepath.Dir(exe))
	if err != nil {
		return err
	}
	if r, _, _ := syscall.SyscallN(linkVtbl.setWorkingDirectory, uintptr(unsafe.Pointer(obj)), uintptr(unsafe.Pointer(workDirPtr)), 0); r != 0 {
		return fmt.Errorf("IShellLinkW.SetWorkingDirectory failed: HRESULT 0x%X", r)
	}

	descPtr, err := windows.UTF16PtrFromString("SlimMedia Agent")
	if err != nil {
		return err
	}
	if r, _, _ := syscall.SyscallN(linkVtbl.setDescription, uintptr(unsafe.Pointer(obj)), uintptr(unsafe.Pointer(descPtr)), 0); r != 0 {
		return fmt.Errorf("IShellLinkW.SetDescription failed: HRESULT 0x%X", r)
	}

	// QueryInterface for IPersistFile to persist the shortcut.
	var pfRaw unsafe.Pointer
	if r, _, _ := syscall.SyscallN(linkVtbl.queryInterface, uintptr(unsafe.Pointer(obj)), uintptr(unsafe.Pointer(&iidIPersistFile)), uintptr(unsafe.Pointer(&pfRaw)), 0); r != 0 {
		return fmt.Errorf("QueryInterface(IPersistFile) failed: HRESULT 0x%X", r)
	}
	pf := (*comObject)(pfRaw)
	pfVtbl := (*iPersistFileVtbl)(unsafe.Pointer(pf.vtbl))
	defer syscall.SyscallN(pfVtbl.release, uintptr(unsafe.Pointer(pf)))

	shortcut := startupShortcutPath(os.Getenv("APPDATA"))
	shortcutPtr, err := windows.UTF16PtrFromString(shortcut)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(shortcut), 0755); err != nil {
		return fmt.Errorf("cannot create Startup folder path: %w", err)
	}
	// Save(pszFileName, fRemember=TRUE): persist to this exact path.
	if r, _, _ := syscall.SyscallN(pfVtbl.save, uintptr(unsafe.Pointer(pf)), uintptr(unsafe.Pointer(shortcutPtr)), 1, 0); r != 0 {
		return fmt.Errorf("IPersistFile.Save failed: HRESULT 0x%X", r)
	}

	return nil
}

// disableAutostart removes the Startup folder shortcut.
func disableAutostart() error {
	err := os.Remove(startupShortcutPath(os.Getenv("APPDATA")))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing startup shortcut: %w", err)
	}
	return nil
}

// changedMode is RPC_E_CHANGED_MODE: the calling thread was already
// initialized in a different COM apartment model.
const changedMode = syscall.Errno(0x80010106)

// sFalse is S_FALSE: COM reports "already initialized" as an HRESULT of 1;
// x/sys/windows surfaces it as a non-nil errno.
const sFalse = syscall.Errno(1)

// coCreateInstance instantiates a COM object and returns the requested
// interface. x/sys/windows does not export CoCreateInstance, so the call
// goes straight to ole32.dll via syscall.SyscallN.
func coCreateInstance(clsid, iid *windows.GUID) (*comObject, error) {
	var obj unsafe.Pointer
	// CoCreateInstance(rclsid, pUnkOuter=NULL, dwClsContext=INPROC_SERVER,
	// riid, ppv)
	r, _, _ := procCoCreateInstance.Call(
		uintptr(unsafe.Pointer(clsid)),
		0,
		windows.CLSCTX_INPROC_SERVER,
		uintptr(unsafe.Pointer(iid)),
		uintptr(unsafe.Pointer(&obj)),
	)
	// The HRESULT contract: r holds the return code; GetLastError is not
	// meaningful for this API, so only r is checked.
	if r != 0 {
		return nil, fmt.Errorf("CoCreateInstance failed: HRESULT 0x%X", r)
	}
	if obj == nil {
		return nil, fmt.Errorf("CoCreateInstance returned a nil interface")
	}
	return (*comObject)(obj), nil
}
