package daemon

import (
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"

	"github.com/will25u/slimmedia-agent/internal/transport"
)

// LibraryMapping pairs a server library ID with its local filesystem mount path.
type LibraryMapping struct {
	LibraryID string `json:"library_id"`
	LocalPath string `json:"local_path"`
}

// LocalConfig is the persisted agent configuration written after pairing
// (and read back on later starts when flags/env vars don't override it).
type LocalConfig struct {
	APIKey    string           `json:"api_key"`
	Server    string           `json:"server,omitempty"`
	Name      string           `json:"name,omitempty"`
	MediaDir  string           `json:"media_dir,omitempty"`
	TempDir   string           `json:"temp_dir,omitempty"`
	LibraryID string           `json:"library_id,omitempty"`
	Libraries []LibraryMapping `json:"libraries,omitempty"`
}

// LoadConfig reads the saved configuration from configPath. Any error
// (missing file, malformed JSON) is returned to the caller.
func LoadConfig(configPath string) (*LocalConfig, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	var cfg LocalConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// SaveConfig persists the configuration to configPath with owner-only
// permissions; the API key inside is a credential. os.WriteFile only applies
// the mode to newly created files, so an existing config that an older
// release loosened is explicitly re-chmodded to 0600.
func SaveConfig(configPath string, cfg *LocalConfig) error {
	dir := filepath.Dir(configPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	_ = os.Chmod(dir, 0700)
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		return err
	}
	return os.Chmod(configPath, 0600)
}

// Config carries the raw run parameters as given by the caller (typically
// CLI flags). Every field may be empty; ResolveConfig fills the gaps from
// the environment, the saved config file, and built-in defaults.
type Config struct {
	// Server is the SaaS server URL (https:// default; plaintext http://
	// requires Insecure except for loopback hosts).
	Server string
	// APIKey is the SaaS API key. Empty means: fall back to the environment,
	// then the saved config, then -- PairingCode must be set.
	APIKey string
	// Name is the agent name shown in the dashboard. Defaults to the hostname.
	Name string
	// MediaDir is the media directory to scan and sync. Required (must be specified
	// via flag, environment, or saved config).
	MediaDir string
	// TempDir is the transcode temporary directory for scratch files.
	// Defaults to <DataDir>/scratch.
	TempDir string
	// LibraryID identifies the target library on the server for syncing.
	LibraryID string
	// Libraries carries explicit mapped libraries.
	Libraries []LibraryMapping
	// PairingCode exchanges for an API key on first-time setup and is then
	// persisted to ConfigPath.
	PairingCode string
	// Insecure allows plaintext http:// transport to non-loopback servers
	// (never disables TLS certificate verification).
	Insecure bool
	// DataDir overrides where the database, FFmpeg, scratch, and quarantine
	// live. Defaults to the per-user state dir (SLIMMEDIA_STATE_DIR, then
	// XDG state home / %AppData%; see StateDir).
	DataDir string
	// ConfigPath overrides where the paired config is persisted. Defaults to
	// <StateDir>/config.json.
	ConfigPath string
	// UpdateConsentHandler, when non-nil, is registered before the engine
	// connects so a server-pushed AGENT_UPDATE can never race ahead of the
	// consent gate (the tray app passes it here; see
	// Daemon.SetUpdateConsentHandler).
	UpdateConsentHandler func(version string) bool
	// UIAttached indicates whether a desktop/tray GUI is running in-process.
	// When false (headless), unpairing logs FATAL and gracefully exits.
	UIAttached bool
}

// ResolvedConfig is the outcome of ResolveConfig: every value the daemon
// needs, with precedence (flag > environment > saved config > default)
// already applied and the server URLs policy-checked.
type ResolvedConfig struct {
	// Server is the normalized API URL (policy-checked).
	Server string
	// WSURL is the normalized WebSocket URL.
	WSURL string
	// APIKey is empty when PairingCode is set; the caller runs Pair and
	// fills it in before Start.
	APIKey      string
	PairingCode string
	Name        string
	MediaDir    string
	TempDir     string
	LibraryID   string
	Libraries   []LibraryMapping
	Insecure    bool
	DataDir     string
	ConfigPath  string
	// UpdateConsentHandler mirrors Config.UpdateConsentHandler; Start
	// registers it before the websocket client connects.
	UpdateConsentHandler func(version string) bool
	// UIAttached indicates whether a desktop/tray GUI is running in-process.
	UIAttached bool
}

// exposesPlaintextCredentials reports whether the resolved configuration
// sends agent credentials over unencrypted transport to a non-loopback host:
// plaintext (http/ws) scheme, the explicit --insecure opt-in, and a remote
// target. Loopback plaintext (local dev) and every TLS endpoint are excluded.
func exposesPlaintextCredentials(apiURL string, insecure bool) bool {
	if !insecure {
		return false
	}
	u, err := url.Parse(apiURL)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "ws" {
		return false
	}
	return !transport.IsLoopbackHost(u.Hostname())
}

// usesPlaintextTransport reports whether the resolved API URL speaks
// unencrypted http/ws at all — loopback development servers included. Any
// plaintext transport gets a prominent startup warning (issue #147 follow-up):
// the traffic is readable by every process on the machine, and operators
// often forget they pointed the agent at http://.
func usesPlaintextTransport(apiURL string) bool {
	u, err := url.Parse(apiURL)
	if err != nil {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "ws"
}

// ResolveConfig merges flag values with environment variables, the saved
// config file, and defaults, then enforces the transport policy before
// anything credential-bearing happens: a plaintext non-loopback endpoint is
// a hard refusal here, before pairing codes or API keys leave the machine.
func ResolveConfig(cfg Config) (*ResolvedConfig, error) {
	dataDir := cfg.DataDir
	if dataDir == "" {
		dataDir = defaultDataDir()
	}
	configPath := cfg.ConfigPath
	if configPath == "" {
		configPath = filepath.Join(StateDir(), "config.json")
	}

	server := cfg.Server
	serverEnv := os.Getenv("SLIMMEDIA_SERVER")
	if server == "" {
		server = serverEnv
	}
	if server == "" {
		// Local dev default. Plaintext is permitted here because the
		// host is loopback; see the transport package docs.
		server = "http://localhost:8082"
	}
	name := cfg.Name
	nameEnv := os.Getenv("SLIMMEDIA_AGENT_NAME")
	if name == "" {
		name = nameEnv
	}
	if name == "" {
		if h, err := os.Hostname(); err == nil && h != "" {
			name = h
		} else {
			name = "slimmedia-agent"
		}
	}

	mediaDir := cfg.MediaDir
	mediaDirEnv := os.Getenv("SLIMMEDIA_MEDIA_DIR")
	if mediaDir == "" {
		mediaDir = mediaDirEnv
	}
	tempDir := cfg.TempDir
	tempDirEnv := os.Getenv("SLIMMEDIA_TEMP_DIR")
	if tempDir == "" {
		tempDir = tempDirEnv
	}
	libraryID := cfg.LibraryID
	if libraryID == "" {
		libraryID = os.Getenv("SLIMMEDIA_LIBRARY_ID")
	}

	var libraries []LibraryMapping
	if len(cfg.Libraries) > 0 {
		libraries = append(libraries, cfg.Libraries...)
	}

	apiKey := cfg.APIKey
	if apiKey == "" {
		apiKey = os.Getenv("SLIMMEDIA_API_KEY")
	}
	if apiKey == "" && cfg.PairingCode == "" {
		loadSaved := func() (*LocalConfig, error) {
			if cfg.ConfigPath != "" {
				return LoadConfig(configPath)
			}
			return LoadSavedConfig()
		}
		if savedCfg, err := loadSaved(); err == nil && savedCfg.APIKey != "" {
			apiKey = savedCfg.APIKey
			// A saved value only applies when neither the flag nor the
			// environment supplied one.
			if cfg.Server == "" && serverEnv == "" && savedCfg.Server != "" {
				server = savedCfg.Server
			}
			if cfg.Name == "" && nameEnv == "" && savedCfg.Name != "" {
				name = savedCfg.Name
			}
			if mediaDir == "" && savedCfg.MediaDir != "" {
				mediaDir = savedCfg.MediaDir
			}
			if tempDir == "" && savedCfg.TempDir != "" {
				tempDir = savedCfg.TempDir
			}
			if libraryID == "" && savedCfg.LibraryID != "" {
				libraryID = savedCfg.LibraryID
			}
			if len(libraries) == 0 && len(savedCfg.Libraries) > 0 {
				libraries = append(libraries, savedCfg.Libraries...)
			}
			log.Printf("Loaded saved API key from %s", MaskUserPath(configPath))
		}
	}

	if len(libraries) == 0 && mediaDir != "" {
		libraries = append(libraries, LibraryMapping{
			LibraryID: libraryID,
			LocalPath: mediaDir,
		})
	}

	if mediaDir == "" {
		return nil, fmt.Errorf("media directory is required: specify via --media-dir, SLIMMEDIA_MEDIA_DIR, or saved config")
	}
	if tempDir == "" {
		tempDir = filepath.Join(dataDir, "scratch")
	}

	apiURL, wsURL, err := transport.ResolveServerURLs(server, cfg.Insecure)
	if err != nil {
		return nil, fmt.Errorf("Invalid --server URL: %w", err)
	}

	// Say so loudly once at startup whenever the resolved transport is
	// plaintext — loopback dev servers included. Non-loopback plaintext (only
	// reachable via --insecure) additionally exposes every credential the
	// agent carries (API key, pairing code, session tokens) to anything on
	// the network path.
	if usesPlaintextTransport(apiURL) {
		host := apiURL
		if u, err := url.Parse(apiURL); err == nil {
			host = u.Host
		}
		if exposesPlaintextCredentials(apiURL, cfg.Insecure) {
			slog.Warn("WARNING: plaintext HTTP transport in use to " + host + " — credentials and data are unencrypted and exposed to anything on the network path. Prefer https://; only use --insecure on a network you fully trust.")
		} else {
			slog.Warn("WARNING: plaintext HTTP transport in use to " + host + " (loopback) — credentials and data are unencrypted; acceptable only for local development, use https:// otherwise")
		}
	}

	return &ResolvedConfig{
		Server:               apiURL,
		WSURL:                wsURL,
		APIKey:               apiKey,
		PairingCode:          cfg.PairingCode,
		Name:                 name,
		MediaDir:             mediaDir,
		TempDir:              tempDir,
		LibraryID:            libraryID,
		Libraries:            libraries,
		Insecure:             cfg.Insecure,
		DataDir:              dataDir,
		ConfigPath:           configPath,
		UpdateConsentHandler: cfg.UpdateConsentHandler,
		UIAttached:           cfg.UIAttached,
	}, nil
}
