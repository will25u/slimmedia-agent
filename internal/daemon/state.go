package daemon

import (
	"os"
	"path/filepath"
	"strings"
)

// LoadSavedConfig reads the saved configuration from the per-user state
// dir. As a migration aid for agents paired before 1.3.0 (whose config
// lived in the cwd-relative bin directory), the legacy location is used
// when the new one has no config yet.
func LoadSavedConfig() (*LocalConfig, error) {
	cfg, err := LoadConfig(filepath.Join(StateDir(), "config.json"))
	if err == nil {
		return cfg, nil
	}
	cwd, cwdErr := os.Getwd()
	if cwdErr != nil {
		return nil, err
	}
	return LoadConfig(filepath.Join(cwd, "bin", "config.json"))
}

// defaultDataDir resolves the DataDir default: the per-user state dir,
// unless an agent upgraded from a pre-1.3.0 release still has its database
// in the legacy cwd-relative bin directory — in that case the legacy
// directory keeps serving so the existing scan state, FFmpeg binaries, and
// quarantine remain reachable.
func defaultDataDir() string {
	state := StateDir()
	if _, err := os.Stat(filepath.Join(state, "agent.db")); err == nil {
		return state
	}
	if cwd, err := os.Getwd(); err == nil {
		legacy := filepath.Join(cwd, "bin")
		if _, err := os.Stat(filepath.Join(legacy, "agent.db")); err == nil {
			return legacy
		}
	}
	return state
}

// StateDir returns the fixed per-user state root that holds the agent's
// config, SQLite database, FFmpeg binaries, scratch, and quarantine.
//
// Resolution order:
//  1. SLIMMEDIA_STATE_DIR environment override (always wins),
//  2. the platform's per-user state location (XDG state home on Unix,
//     %AppData% on Windows) — independent of the launch working directory,
//  3. <cwd>/bin as a last-resort fallback.
func StateDir() string {
	if d := os.Getenv("SLIMMEDIA_STATE_DIR"); d != "" {
		return d
	}
	if root := stateRoot(); root != "" {
		return filepath.Join(root, "slimmedia")
	}
	cwd, _ := os.Getwd()
	return filepath.Join(cwd, "bin")
}

// MaskUserPath masks the user's home directory or Windows app data directory
// in path so raw usernames are not printed in logs.
func MaskUserPath(path string) string {
	if path == "" {
		return ""
	}
	cleanPath := filepath.Clean(path)

	// Windows: mask %LOCALAPPDATA%, %APPDATA%, or %USERPROFILE%
	if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
		cleanLocal := filepath.Clean(localAppData)
		if strings.HasPrefix(strings.ToLower(cleanPath), strings.ToLower(cleanLocal)) {
			return "%LOCALAPPDATA%" + cleanPath[len(cleanLocal):]
		}
	}
	if appData := os.Getenv("APPDATA"); appData != "" {
		cleanApp := filepath.Clean(appData)
		if strings.HasPrefix(strings.ToLower(cleanPath), strings.ToLower(cleanApp)) {
			return "%APPDATA%" + cleanPath[len(cleanApp):]
		}
	}
	if userProfile := os.Getenv("USERPROFILE"); userProfile != "" {
		cleanProfile := filepath.Clean(userProfile)
		if strings.HasPrefix(strings.ToLower(cleanPath), strings.ToLower(cleanProfile)) {
			return "%USERPROFILE%" + cleanPath[len(cleanProfile):]
		}
	}

	// Unix / cross-platform: mask user's home directory to ~
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.Getenv("HOME")
	}
	if home != "" {
		cleanHome := filepath.Clean(home)
		if cleanPath == cleanHome {
			return "~"
		}
		prefix := cleanHome + string(filepath.Separator)
		if strings.HasPrefix(cleanPath, prefix) {
			return "~" + string(filepath.Separator) + cleanPath[len(prefix):]
		}
		prefixSlash := filepath.ToSlash(cleanHome) + "/"
		cleanPathSlash := filepath.ToSlash(cleanPath)
		if strings.HasPrefix(cleanPathSlash, prefixSlash) {
			return "~/" + cleanPathSlash[len(prefixSlash):]
		}
	}

	return path
}
