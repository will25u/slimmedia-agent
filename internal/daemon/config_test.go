package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/will25u/slimmedia-agent/internal/transport"
)

// TestLoadConfigMalformedJSON pins that a corrupt config file surfaces an
// error instead of being silently ignored (ResolveConfig skips unreadable
// saved configs, so only a clear error tells the user the file is broken).
func TestLoadConfigMalformedJSON(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte("{not json"), 0600); err != nil {
		t.Fatalf("write malformed config: %v", err)
	}
	if _, err := LoadConfig(configPath); err == nil {
		t.Fatal("LoadConfig accepted malformed JSON")
	}
}

// TestSaveConfigTightensExistingPerms pins the remediation for config files
// loosened by an older release: rewriting must force 0600 even when the
// existing file is looser (os.WriteFile alone only applies the mode on
// create).
func TestSaveConfigTightensExistingPerms(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"api_key":"old"}`), 0644); err != nil {
		t.Fatalf("write loose config: %v", err)
	}
	if err := SaveConfig(configPath, &LocalConfig{APIKey: "new-key"}); err != nil {
		t.Fatalf("SaveConfig failed: %v", err)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("config mode = %o, want 0600", perm)
	}
	dirInfo, err := os.Stat(filepath.Dir(configPath))
	if err != nil {
		t.Fatalf("stat config dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0700 {
		t.Fatalf("config dir mode = %o, want 0700", perm)
	}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if cfg.APIKey != "new-key" {
		t.Fatalf("APIKey = %q, want new-key", cfg.APIKey)
	}
}

// TestResolveConfig pins the configuration precedence the daemon exposes to
// both the Linux CLI and the Windows tray: flag > environment > saved config
// > default, with the transport policy enforced before credentials are used.
func TestResolveConfig(t *testing.T) {
	// Each subtest gets a pristine environment for the config vars.
	clearEnv := func(t *testing.T) {
		t.Helper()
		for _, key := range []string{"SLIMMEDIA_SERVER", "SLIMMEDIA_AGENT_NAME", "SLIMMEDIA_API_KEY", "SLIMMEDIA_MEDIA_DIR", "SLIMMEDIA_TEMP_DIR"} {
			t.Setenv(key, "")
		}
	}

	t.Run("media dir required", func(t *testing.T) {
		clearEnv(t)
		_, err := ResolveConfig(Config{DataDir: t.TempDir(), ConfigPath: filepath.Join(t.TempDir(), "config.json")})
		if err == nil || !strings.Contains(err.Error(), "media directory is required") {
			t.Fatalf("expected media directory is required error, got %v", err)
		}
	})

	t.Run("defaults", func(t *testing.T) {
		clearEnv(t)
		dataDir := t.TempDir()
		rc, err := ResolveConfig(Config{
			MediaDir:   "/media/default",
			DataDir:    dataDir,
			ConfigPath: filepath.Join(t.TempDir(), "config.json"),
		})
		if err != nil {
			t.Fatalf("ResolveConfig failed: %v", err)
		}
		if rc.Server != "http://localhost:8082" || rc.WSURL != "ws://localhost:8082" {
			t.Fatalf("default server = (%q, %q)", rc.Server, rc.WSURL)
		}
		if rc.Name == "" {
			t.Fatal("default name is empty; want hostname fallback")
		}
		if rc.MediaDir != "/media/default" {
			t.Fatalf("media dir = %q, want /media/default", rc.MediaDir)
		}
		wantScratch := filepath.Join(dataDir, "scratch")
		if rc.TempDir != wantScratch {
			t.Fatalf("default temp dir = %q, want %q", rc.TempDir, wantScratch)
		}
		if rc.APIKey != "" {
			t.Fatalf("APIKey = %q without any source, want empty", rc.APIKey)
		}
	})

	t.Run("environment fallback", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("SLIMMEDIA_SERVER", "media.example.com")
		t.Setenv("SLIMMEDIA_AGENT_NAME", "env-agent")
		t.Setenv("SLIMMEDIA_API_KEY", "env-key")
		t.Setenv("SLIMMEDIA_MEDIA_DIR", "/media/env")
		t.Setenv("SLIMMEDIA_TEMP_DIR", "/temp/env")
		rc, err := ResolveConfig(Config{DataDir: t.TempDir(), ConfigPath: filepath.Join(t.TempDir(), "config.json")})
		if err != nil {
			t.Fatalf("ResolveConfig failed: %v", err)
		}
		if rc.Server != "https://media.example.com" || rc.WSURL != "wss://media.example.com" {
			t.Fatalf("env server = (%q, %q)", rc.Server, rc.WSURL)
		}
		if rc.Name != "env-agent" || rc.APIKey != "env-key" || rc.MediaDir != "/media/env" || rc.TempDir != "/temp/env" {
			t.Fatalf("env resolution = %+v", rc)
		}
	})

	t.Run("flag beats environment", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("SLIMMEDIA_SERVER", "env.example.com")
		t.Setenv("SLIMMEDIA_AGENT_NAME", "env-agent")
		t.Setenv("SLIMMEDIA_MEDIA_DIR", "/media/env")
		t.Setenv("SLIMMEDIA_TEMP_DIR", "/temp/env")
		rc, err := ResolveConfig(Config{
			Server:     "flag.example.com",
			Name:       "flag-agent",
			MediaDir:   "/media/flag",
			TempDir:    "/temp/flag",
			DataDir:    t.TempDir(),
			ConfigPath: filepath.Join(t.TempDir(), "config.json"),
		})
		if err != nil {
			t.Fatalf("ResolveConfig failed: %v", err)
		}
		if rc.Server != "https://flag.example.com" || rc.Name != "flag-agent" || rc.MediaDir != "/media/flag" || rc.TempDir != "/temp/flag" {
			t.Fatalf("flag did not win over env: %+v", rc)
		}
	})

	t.Run("saved config fallback", func(t *testing.T) {
		clearEnv(t)
		configPath := filepath.Join(t.TempDir(), "config.json")
		if err := SaveConfig(configPath, &LocalConfig{
			APIKey:   "saved-key",
			Server:   "http://localhost:9999",
			Name:     "saved-agent",
			MediaDir: "/media/saved",
			TempDir:  "/temp/saved",
		}); err != nil {
			t.Fatalf("SaveConfig failed: %v", err)
		}
		rc, err := ResolveConfig(Config{DataDir: t.TempDir(), ConfigPath: configPath})
		if err != nil {
			t.Fatalf("ResolveConfig failed: %v", err)
		}
		if rc.APIKey != "saved-key" || rc.Server != "http://localhost:9999" || rc.Name != "saved-agent" || rc.MediaDir != "/media/saved" || rc.TempDir != "/temp/saved" {
			t.Fatalf("saved config resolution = %+v", rc)
		}
	})

	t.Run("flag beats saved config", func(t *testing.T) {
		clearEnv(t)
		configPath := filepath.Join(t.TempDir(), "config.json")
		if err := SaveConfig(configPath, &LocalConfig{APIKey: "saved-key", Name: "saved-agent", MediaDir: "/media/saved"}); err != nil {
			t.Fatalf("SaveConfig failed: %v", err)
		}
		rc, err := ResolveConfig(Config{
			APIKey:     "flag-key",
			Name:       "flag-agent",
			MediaDir:   "/media/flag",
			DataDir:    t.TempDir(),
			ConfigPath: configPath,
		})
		if err != nil {
			t.Fatalf("ResolveConfig failed: %v", err)
		}
		if rc.APIKey != "flag-key" || rc.Name != "flag-agent" || rc.MediaDir != "/media/flag" {
			t.Fatalf("flag did not win over saved config: %+v", rc)
		}
	})

	t.Run("flag server beats saved server while saved key loads", func(t *testing.T) {
		clearEnv(t)
		configPath := filepath.Join(t.TempDir(), "config.json")
		if err := SaveConfig(configPath, &LocalConfig{
			APIKey:   "saved-key",
			Server:   "http://localhost:9999",
			MediaDir: "/media/saved",
		}); err != nil {
			t.Fatalf("SaveConfig failed: %v", err)
		}
		rc, err := ResolveConfig(Config{
			Server:     "flag.example.com",
			DataDir:    t.TempDir(),
			ConfigPath: configPath,
		})
		if err != nil {
			t.Fatalf("ResolveConfig failed: %v", err)
		}
		if rc.Server != "https://flag.example.com" {
			t.Fatalf("Server = %q, want flag server to win over saved http://localhost:9999", rc.Server)
		}
		if rc.APIKey != "saved-key" {
			t.Fatalf("APIKey = %q, want saved-key to still load alongside the flag server", rc.APIKey)
		}
	})

	t.Run("environment beats saved config", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("SLIMMEDIA_AGENT_NAME", "env-agent")
		configPath := filepath.Join(t.TempDir(), "config.json")
		if err := SaveConfig(configPath, &LocalConfig{APIKey: "saved-key", Name: "saved-agent", MediaDir: "/media/saved"}); err != nil {
			t.Fatalf("SaveConfig failed: %v", err)
		}
		rc, err := ResolveConfig(Config{DataDir: t.TempDir(), ConfigPath: configPath})
		if err != nil {
			t.Fatalf("ResolveConfig failed: %v", err)
		}
		if rc.APIKey != "saved-key" {
			t.Fatalf("APIKey = %q, want saved-key", rc.APIKey)
		}
		if rc.Name != "env-agent" {
			t.Fatalf("Name = %q, want env-agent to win over saved config", rc.Name)
		}
	})

	t.Run("pairing ignores saved config", func(t *testing.T) {
		clearEnv(t)
		configPath := filepath.Join(t.TempDir(), "config.json")
		if err := SaveConfig(configPath, &LocalConfig{APIKey: "saved-key"}); err != nil {
			t.Fatalf("SaveConfig failed: %v", err)
		}
		rc, err := ResolveConfig(Config{
			PairingCode: "PAIR-1234",
			MediaDir:    "/media/pair",
			DataDir:     t.TempDir(),
			ConfigPath:  configPath,
		})
		if err != nil {
			t.Fatalf("ResolveConfig failed: %v", err)
		}
		if rc.APIKey != "" {
			t.Fatalf("APIKey = %q despite pairing code, want empty", rc.APIKey)
		}
		if rc.PairingCode != "PAIR-1234" {
			t.Fatalf("PairingCode = %q, want PAIR-1234", rc.PairingCode)
		}
	})

	t.Run("plaintext non-loopback refused", func(t *testing.T) {
		clearEnv(t)
		_, err := ResolveConfig(Config{
			Server:     "http://media.example.com",
			MediaDir:   "/media/test",
			DataDir:    t.TempDir(),
			ConfigPath: filepath.Join(t.TempDir(), "config.json"),
		})
		if err == nil {
			t.Fatal("plaintext non-loopback server accepted without --insecure")
		}
		if !errors.Is(err, transport.ErrPlaintextTransport) {
			t.Fatalf("error = %v, want ErrPlaintextTransport", err)
		}
	})

	// The startup warning condition: --insecure plaintext to a non-loopback
	// host warns, loopback plaintext and TLS do not.
	t.Run("plaintext credential exposure warning condition", func(t *testing.T) {
		cases := []struct {
			name     string
			apiURL   string
			insecure bool
			want     bool
		}{
			{"insecure plaintext non-loopback", "http://192.168.1.10:8080", true, true},
			{"insecure plaintext hostname non-loopback", "http://nas.lan:8080", true, true},
			{"insecure plaintext loopback", "http://127.0.0.1:8080", true, false},
			{"insecure plaintext localhost", "http://localhost:8080", true, false},
			{"secure https non-loopback", "https://media.example.com", true, false},
			{"plaintext non-loopback without insecure", "http://192.168.1.10:8080", false, false},
			{"plaintext loopback without insecure", "http://localhost:8082", false, false},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if got := exposesPlaintextCredentials(tc.apiURL, tc.insecure); got != tc.want {
					t.Fatalf("exposesPlaintextCredentials(%q, %v) = %v, want %v", tc.apiURL, tc.insecure, got, tc.want)
				}
			})
		}
	})

	// ResolveConfig must not refuse the insecure plaintext endpoint (the
	// operator opted in) but must resolve it so the warning fires at startup.
	t.Run("insecure plaintext non-loopback resolves for warning", func(t *testing.T) {
		clearEnv(t)
		rc, err := ResolveConfig(Config{
			Server:     "http://192.168.1.10:8080",
			Insecure:   true,
			MediaDir:   "/media/test",
			DataDir:    t.TempDir(),
			ConfigPath: filepath.Join(t.TempDir(), "config.json"),
		})
		if err != nil {
			t.Fatalf("ResolveConfig refused the insecure opt-in: %v", err)
		}
		if rc.Server != "http://192.168.1.10:8080" || rc.WSURL != "ws://192.168.1.10:8080" {
			t.Fatalf("insecure resolution = (%q, %q)", rc.Server, rc.WSURL)
		}
		if !exposesPlaintextCredentials(rc.Server, rc.Insecure) {
			t.Fatal("resolved insecure plaintext non-loopback config does not trigger the warning condition")
		}
	})
}

// TestUsesPlaintextTransport pins the startup-warning condition (issue #147
// follow-up): every plaintext http/ws endpoint warns, loopback included;
// TLS endpoints never do.
func TestUsesPlaintextTransport(t *testing.T) {
	cases := []struct {
		name   string
		apiURL string
		want   bool
	}{
		{"https non-loopback", "https://media.example.com", false},
		{"https loopback", "https://localhost:8082", false},
		{"http loopback", "http://localhost:8082", true},
		{"http loopback address", "http://127.0.0.1:8082", true},
		{"http non-loopback", "http://192.168.1.10:8080", true},
		{"http non-loopback hostname", "http://nas.lan:8080", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := usesPlaintextTransport(tc.apiURL); got != tc.want {
				t.Fatalf("usesPlaintextTransport(%q) = %v, want %v", tc.apiURL, got, tc.want)
			}
		})
	}
}
