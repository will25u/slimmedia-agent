package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStateDirEnvOverrideWins(t *testing.T) {
	override := filepath.Join(t.TempDir(), "custom-state")
	t.Setenv("SLIMMEDIA_STATE_DIR", override)
	if got := StateDir(); got != override {
		t.Errorf("StateDir() = %q, want env override %q", got, override)
	}
}

func TestStateDirIsNotCwdRelative(t *testing.T) {
	// Without SLIMMEDIA_STATE_DIR, the state dir must not silently resolve
	// to wherever the process happens to be running (the old <cwd>/bin
	// default) on platforms with a per-user state location. Force the
	// fallback by clearing the platform env knobs we know about.
	t.Setenv("SLIMMEDIA_STATE_DIR", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("APPDATA", filepath.Join(t.TempDir(), "appdata"))

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	got := StateDir()
	if got == filepath.Join(cwd, "bin") {
		t.Errorf("StateDir() fell back to cwd-relative %q", got)
	}
	if filepath.Base(got) != "slimmedia" {
		t.Errorf("StateDir() = %q, want a .../slimmedia path", got)
	}
}

func TestResolveConfigDefaultsUnderStateDir(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("SLIMMEDIA_STATE_DIR", stateDir)

	rc, err := ResolveConfig(Config{MediaDir: t.TempDir()})
	if err != nil {
		t.Fatalf("ResolveConfig: %v", err)
	}
	if rc.DataDir != stateDir {
		t.Errorf("DataDir = %q, want state dir %q", rc.DataDir, stateDir)
	}
	wantConfig := filepath.Join(stateDir, "config.json")
	if rc.ConfigPath != wantConfig {
		t.Errorf("ConfigPath = %q, want %q", rc.ConfigPath, wantConfig)
	}
}

func TestMaskUserPath(t *testing.T) {
	// Empty path
	if got := MaskUserPath(""); got != "" {
		t.Errorf("MaskUserPath(\"\") = %q, want \"\"", got)
	}

	// Unix HOME masking
	t.Setenv("LOCALAPPDATA", "")
	t.Setenv("APPDATA", "")
	t.Setenv("USERPROFILE", "")
	t.Setenv("HOME", "/home/testuser")

	if got := MaskUserPath("/home/testuser/.local/state/slimmedia/config.json"); got != "~/.local/state/slimmedia/config.json" {
		t.Errorf("MaskUserPath() = %q, want ~/.local/state/slimmedia/config.json", got)
	}
	if got := MaskUserPath("/home/testuser"); got != "~" {
		t.Errorf("MaskUserPath() = %q, want ~", got)
	}
	if got := MaskUserPath("/etc/slimmedia/config.json"); got != "/etc/slimmedia/config.json" {
		t.Errorf("MaskUserPath() = %q, want unchanged path", got)
	}

	// Windows LOCALAPPDATA masking
	t.Setenv("LOCALAPPDATA", `C:\Users\testuser\AppData\Local`)
	if got := MaskUserPath(`C:\Users\testuser\AppData\Local\slimmedia\config.json`); got != `%LOCALAPPDATA%\slimmedia\config.json` {
		t.Errorf("MaskUserPath() = %q, want %%LOCALAPPDATA%%\\slimmedia\\config.json", got)
	}

	// Windows APPDATA masking
	t.Setenv("LOCALAPPDATA", "")
	t.Setenv("APPDATA", `C:\Users\testuser\AppData\Roaming`)
	if got := MaskUserPath(`C:\Users\testuser\AppData\Roaming\slimmedia\config.json`); got != `%APPDATA%\slimmedia\config.json` {
		t.Errorf("MaskUserPath() = %q, want %%APPDATA%%\\slimmedia\\config.json", got)
	}
}

