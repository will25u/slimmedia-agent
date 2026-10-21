package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestStartupShortcutPath(t *testing.T) {
	// Forward slashes: Win32 accepts them and they stay portable when the
	// test runs on Linux (filepath there does not treat "\" as a
	// separator).
	got := startupShortcutPath("C:/Users/alice/AppData/Roaming")

	slash := filepath.ToSlash(got)
	want := "C:/Users/alice/AppData/Roaming/Microsoft/Windows/Start Menu/Programs/Startup/SlimMedia Agent.lnk"
	if slash != want {
		t.Fatalf("startupShortcutPath = %q, want %q", slash, want)
	}
}

func TestStartupShortcutPathExtension(t *testing.T) {
	if ext := filepath.Ext(startupShortcutPath("x")); ext != ".lnk" {
		t.Fatalf("shortcut extension = %q, want %q", ext, ".lnk")
	}
}

func TestStartupShortcutPathEmptyAppData(t *testing.T) {
	// On Linux APPDATA is unset; the path degrades to a relative one that
	// os.Stat simply reports missing. It must still end in the Startup
	// folder with the .lnk name.
	got := startupShortcutPath("")
	slash := filepath.ToSlash(got)
	if !strings.HasSuffix(slash, "Startup/SlimMedia Agent.lnk") {
		t.Fatalf("startupShortcutPath(\"\") = %q, want Startup suffix", slash)
	}
}
