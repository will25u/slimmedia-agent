package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// An agent paired before 1.3.0 keeps its config.json (API key) in the
// cwd-relative bin directory; LoadSavedConfig must still find it after the
// upgrade, running from an unrelated working directory.
func TestLoadSavedConfigFallsBackToLegacyCwdBin(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("SLIMMEDIA_STATE_DIR", stateDir)

	legacyDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(legacyDir, "bin"), 0755); err != nil {
		t.Fatalf("mkdir legacy dir: %v", err)
	}
	cfg := &LocalConfig{APIKey: "sm_legacy_key_789"}
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(legacyDir, "bin", "config.json"), data, 0600); err != nil {
		t.Fatalf("write legacy config: %v", err)
	}

	origWd, _ := os.Getwd()
	if err := os.Chdir(legacyDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer os.Chdir(origWd)

	saved, err := LoadSavedConfig()
	if err != nil {
		t.Fatalf("LoadSavedConfig: %v", err)
	}
	if saved.APIKey != "sm_legacy_key_789" {
		t.Errorf("APIKey = %q, want legacy key", saved.APIKey)
	}

	// The new location wins when both exist.
	newCfg := &LocalConfig{APIKey: "sm_new_key"}
	data, _ = json.Marshal(newCfg)
	if err := os.WriteFile(filepath.Join(stateDir, "config.json"), data, 0600); err != nil {
		t.Fatalf("write new config: %v", err)
	}
	saved, err = LoadSavedConfig()
	if err != nil {
		t.Fatalf("LoadSavedConfig (new location): %v", err)
	}
	if saved.APIKey != "sm_new_key" {
		t.Errorf("APIKey = %q, want new-location key to win", saved.APIKey)
	}
}

// An upgraded agent whose agent.db still lives in the legacy cwd-relative
// bin directory must keep using that directory as its DataDir.
func TestResolveConfigKeepsLegacyDataDirWithExistingDB(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("SLIMMEDIA_STATE_DIR", stateDir)

	legacyDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(legacyDir, "bin"), 0755); err != nil {
		t.Fatalf("mkdir legacy dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "bin", "agent.db"), []byte("db"), 0600); err != nil {
		t.Fatalf("write legacy db: %v", err)
	}

	origWd, _ := os.Getwd()
	if err := os.Chdir(legacyDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer os.Chdir(origWd)

	rc, err := ResolveConfig(Config{MediaDir: t.TempDir()})
	if err != nil {
		t.Fatalf("ResolveConfig: %v", err)
	}
	if rc.DataDir != filepath.Join(legacyDir, "bin") {
		t.Errorf("DataDir = %q, want legacy dir %q", rc.DataDir, filepath.Join(legacyDir, "bin"))
	}

	// With a database already in the new location, the state dir wins.
	if err := os.WriteFile(filepath.Join(stateDir, "agent.db"), []byte("db"), 0600); err != nil {
		t.Fatalf("write state db: %v", err)
	}
	rc, err = ResolveConfig(Config{MediaDir: t.TempDir()})
	if err != nil {
		t.Fatalf("ResolveConfig (state db): %v", err)
	}
	if rc.DataDir != stateDir {
		t.Errorf("DataDir = %q, want state dir %q", rc.DataDir, stateDir)
	}
}
