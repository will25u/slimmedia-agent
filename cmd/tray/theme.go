//go:build windows

package main

import (
	"strings"
	"syscall"
	"unsafe"

	"github.com/lxn/walk"
	"github.com/lxn/win"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

type themeColors struct {
	isDark       bool
	bg           walk.Color
	bgBrush      walk.Brush
	editBg       walk.Color
	editBgBrush  walk.Brush
	textPrimary  walk.Color
	textMuted    walk.Color
	textError    walk.Color
	textStarting walk.Color
	connOk       walk.Color
	connWarn     walk.Color
	connErr      walk.Color
}

var (
	procSetPreferredAppMode    uintptr
	procAllowDarkModeForApp    uintptr
	procAllowDarkModeForWindow uintptr
	procRefreshImmersiveColors uintptr
	procShouldAppsUseDarkMode  uintptr
	modUxTheme                 = windows.NewLazySystemDLL("uxtheme.dll")
	procSetWindowTheme         = modUxTheme.NewProc("SetWindowTheme")
)

func initThemeSupport() {
	h, err := windows.LoadLibrary("uxtheme.dll")
	if err != nil {
		return
	}
	if p, err := windows.GetProcAddressByOrdinal(h, 135); err == nil {
		procSetPreferredAppMode = p
	} else if p, err := windows.GetProcAddressByOrdinal(h, 133); err == nil {
		procAllowDarkModeForApp = p
	}
	if p, err := windows.GetProcAddressByOrdinal(h, 132); err == nil {
		procAllowDarkModeForWindow = p
	}
	if p, err := windows.GetProcAddressByOrdinal(h, 104); err == nil {
		procRefreshImmersiveColors = p
	}
	if p, err := windows.GetProcAddressByOrdinal(h, 138); err == nil {
		procShouldAppsUseDarkMode = p
	}

	mode := uintptr(0)
	if isSystemDarkMode() {
		mode = 1 // AllowDark
	}
	if procSetPreferredAppMode != 0 {
		_, _, _ = syscall.SyscallN(procSetPreferredAppMode, mode)
	} else if procAllowDarkModeForApp != 0 {
		_, _, _ = syscall.SyscallN(procAllowDarkModeForApp, mode)
	}
	if procRefreshImmersiveColors != 0 {
		_, _, _ = syscall.SyscallN(procRefreshImmersiveColors)
	}
}

func isSystemDarkMode() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`, registry.QUERY_VALUE)
	if err == nil {
		defer k.Close()
		if val, _, err := k.GetIntegerValue("AppsUseLightTheme"); err == nil {
			return val == 0
		}
	}
	if procShouldAppsUseDarkMode != 0 {
		r, _, _ := syscall.SyscallN(procShouldAppsUseDarkMode)
		return r != 0
	}
	return false
}

func currentTheme() *themeColors {
	dark := isSystemDarkMode()
	if dark {
		bgBrush, _ := walk.NewSolidColorBrush(walk.RGB(32, 32, 32))
		editBrush, _ := walk.NewSolidColorBrush(walk.RGB(45, 45, 45))
		return &themeColors{
			isDark:       true,
			bg:           walk.RGB(32, 32, 32),
			bgBrush:      bgBrush,
			editBg:       walk.RGB(45, 45, 45),
			editBgBrush:  editBrush,
			textPrimary:  walk.RGB(240, 240, 240),
			textMuted:    walk.RGB(160, 160, 160),
			textError:    walk.RGB(255, 107, 107),
			textStarting: walk.RGB(200, 200, 200),
			connOk:       walk.RGB(74, 222, 128),
			connWarn:     walk.RGB(251, 191, 36),
			connErr:      walk.RGB(248, 113, 113),
		}
	}
	bgBrush, _ := walk.NewSolidColorBrush(walk.RGB(243, 243, 243))
	editBrush, _ := walk.NewSolidColorBrush(walk.RGB(255, 255, 255))
	return &themeColors{
		isDark:       false,
		bg:           walk.RGB(243, 243, 243),
		bgBrush:      bgBrush,
		editBg:       walk.RGB(255, 255, 255),
		editBgBrush:  editBrush,
		textPrimary:  walk.RGB(30, 30, 30),
		textMuted:    walk.RGB(110, 110, 110),
		textError:    walk.RGB(200, 0, 0),
		textStarting: walk.RGB(0, 0, 0),
		connOk:       walk.RGB(0, 150, 0),
		connWarn:     walk.RGB(200, 140, 0),
		connErr:      walk.RGB(200, 0, 0),
	}
}

func applyDWMTheme(hwnd win.HWND, dark bool) {
	var val uint32
	if dark {
		val = 1
	}
	const DWMWA_USE_IMMERSIVE_DARK_MODE = 20
	const DWMWA_USE_IMMERSIVE_DARK_MODE_BEFORE_20H1 = 19

	h := windows.HWND(hwnd)
	if err := windows.DwmSetWindowAttribute(h, DWMWA_USE_IMMERSIVE_DARK_MODE, unsafe.Pointer(&val), uint32(unsafe.Sizeof(val))); err != nil {
		_ = windows.DwmSetWindowAttribute(h, DWMWA_USE_IMMERSIVE_DARK_MODE_BEFORE_20H1, unsafe.Pointer(&val), uint32(unsafe.Sizeof(val)))
	}
	if procAllowDarkModeForWindow != 0 {
		var a uintptr
		if dark {
			a = 1
		}
		_, _, _ = syscall.SyscallN(procAllowDarkModeForWindow, uintptr(hwnd), a)
	}
}

func applyTheme(hwnd win.HWND) {
	applyDWMTheme(hwnd, isSystemDarkMode())
}

func controlThemeSubAppForClass(cls string, dark bool) string {
	if strings.EqualFold(cls, "Edit") || strings.EqualFold(cls, "ComboBox") {
		if dark {
			return "DarkMode_CFD"
		}
		return "CFD"
	}
	if dark {
		return "DarkMode_Explorer"
	}
	return "Explorer"
}

func controlThemeSubApp(hwnd win.HWND, dark bool) string {
	var buf [64]uint16
	n, _ := win.GetClassName(hwnd, &buf[0], len(buf))
	cls := windows.UTF16ToString(buf[:n])
	return controlThemeSubAppForClass(cls, dark)
}

func setControlTheme(hwnd win.HWND, dark bool) {
	if procAllowDarkModeForWindow != 0 {
		var a uintptr
		if dark {
			a = 1
		}
		_, _, _ = syscall.SyscallN(procAllowDarkModeForWindow, uintptr(hwnd), a)
	}
	setWindowTheme(hwnd, controlThemeSubApp(hwnd, dark))
}

func setWindowTheme(hwnd win.HWND, subApp string) {
	if procSetWindowTheme.Find() != nil {
		return
	}
	var ptr *uint16
	if subApp != "" {
		ptr = windows.StringToUTF16Ptr(subApp)
	}
	_, _, _ = procSetWindowTheme.Call(uintptr(hwnd), uintptr(unsafe.Pointer(ptr)), 0)
}

func applyControlThemes(hwnd win.HWND, dark bool) {
	setControlTheme(hwnd, dark)
	for child := win.GetWindow(hwnd, win.GW_CHILD); child != 0; child = win.GetWindow(child, win.GW_HWNDNEXT) {
		applyControlThemes(child, dark)
	}
}
