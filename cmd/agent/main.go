package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/will25u/slimmedia-agent/internal/daemon"
	"github.com/will25u/slimmedia-agent/internal/licenses"
	"github.com/will25u/slimmedia-agent/internal/singleinstance"
	"github.com/will25u/slimmedia-agent/internal/logging"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: slimmedia-agent <command> [args]")
		fmt.Println()
		fmt.Println("Commands:")
		fmt.Println("  run           - Start the agent daemon")
		fmt.Println("  start         - Alias for run")
		fmt.Println("  audit <path>  - Run system audit and media estimation")
		fmt.Println("  update        - Download and install the latest agent binary from the server")
		fmt.Println("  license       - Display open-source licenses, GPLv3 notices, and FFmpeg source offers")
		fmt.Println()
		fmt.Println("run/start flags (each also falls back to an env var or the saved config under the per-user state dir):")
		fmt.Println("  --api-key <key>    SaaS API key (visible in ps output; prefer SLIMMEDIA_API_KEY env var or --pair)")
		fmt.Println("  --pair <code>      Pairing code for first-time setup; exchanges the code for an API key")
		fmt.Println("  --server <url>     SaaS server URL (https:// default; plaintext http:// requires --insecure")
		fmt.Println("                     except for loopback hosts)  [env SLIMMEDIA_SERVER]")
		fmt.Println("  --name <name>      Agent name shown in the dashboard  [env SLIMMEDIA_AGENT_NAME]")
		fmt.Println("  --media-dir <dir>  Media directory to scan and sync  [env SLIMMEDIA_MEDIA_DIR]")
		fmt.Println("  --temp-dir <dir>   Transcode temporary directory for scratch files  [env SLIMMEDIA_TEMP_DIR]")
		fmt.Println("  --library-id <id>  Target library ID on the server (optional)  [env SLIMMEDIA_LIBRARY_ID]")
		fmt.Println("  --insecure         Allow plaintext http:// transport to non-loopback servers")
		fmt.Println("                     (never disables TLS certificate verification)")
		fmt.Println()
		fmt.Println("audit flags:")
		fmt.Println("  --server <url>     SaaS server URL to upload the report to (optional)")
		fmt.Println("  --api-key <key>    API key for uploading the report (optional)")
		fmt.Println("  --insecure         Allow plaintext http:// transport to non-loopback servers")
		fmt.Println("                     (server/key fall back to the saved config if omitted)")
		fmt.Println()
		fmt.Println("update flags (server falls back to the saved config under the per-user state dir):")
		fmt.Println("  --server <url>     SaaS server URL to download the update from  [env SLIMMEDIA_SERVER]")
		fmt.Println("  --api-key <key>    API key, only needed if the server sets AGENT_DOWNLOAD_AUTH_REQUIRED")
		fmt.Println("  --insecure         Allow plaintext http:// transport to non-loopback servers")
		fmt.Println("  --force            Reinstall even if the server reports no newer version")
		fmt.Println()
		fmt.Println("license flags:")
		fmt.Println("  --full             Display the full GNU General Public License v3.0 text")
		fmt.Println()
		fmt.Println("The agent binary can also be updated from the dashboard: the server pushes an")
		fmt.Println("AGENT_UPDATE command over the agent's WebSocket, and the running daemon downloads,")
		fmt.Println("checksum-verifies, and re-execs itself. The CLI `update` command above is the")
		fmt.Println("on-demand equivalent and does not restart a running daemon.")
		return
	}

	command := os.Args[1]
	switch command {
	case "run", "start":
		runCmd := flag.NewFlagSet("run", flag.ExitOnError)
		apiKeyFlag := runCmd.String("api-key", "", "SaaS API key (visible in ps output; prefer SLIMMEDIA_API_KEY env var or --pair)")
		serverFlag := runCmd.String("server", "", "SaaS server URL (https:// default; plaintext http:// requires --insecure except for loopback hosts)")
		nameFlag := runCmd.String("name", "", "Agent name")
		pairFlag := runCmd.String("pair", "", "Pairing code for first-time setup")
		mediaDirFlag := runCmd.String("media-dir", "", "Media directory to scan and sync")
		tempDirFlag := runCmd.String("temp-dir", "", "Transcode temporary directory for scratch files")
		libraryIDFlag := runCmd.String("library-id", "", "Target library ID on the server (optional)")
		insecureFlag := runCmd.Bool("insecure", false, "Allow plaintext http:// transport to non-loopback servers (never disables TLS certificate verification)")
		runCmd.Parse(os.Args[2:])

		warnSecretFlagInArgv("api-key", "SLIMMEDIA_API_KEY")

		logDir := defaultLogDir()
		_, logCloser, _ := logging.SetupLogger("slimmedia-agent", logDir)
		if logCloser != nil {
			defer logCloser.Close()
		}

		instanceLock, err := singleinstance.Acquire(daemon.StateDir())
		if errors.Is(err, singleinstance.ErrAlreadyRunning) {
			log.Println("WARNING: Another agent instance is already active")
			return
		}
		if err != nil {
			log.Printf("WARNING: Failed to acquire single-instance lock: %v", err)
		} else {
			defer instanceLock.Close()
		}

		// Enforce the transport policy and merge flags/env/saved config
		// before anything credential-bearing happens.
		rc, err := daemon.ResolveConfig(daemon.Config{
			Server:      *serverFlag,
			APIKey:      *apiKeyFlag,
			Name:        *nameFlag,
			MediaDir:    *mediaDirFlag,
			TempDir:     *tempDirFlag,
			LibraryID:   *libraryIDFlag,
			PairingCode: *pairFlag,
			Insecure:    *insecureFlag,
		})
		if err != nil {
			log.Fatalf("%v", err)
		}

		if rc.PairingCode != "" {
			saveTempDir := *tempDirFlag
			if saveTempDir == "" {
				saveTempDir = os.Getenv("SLIMMEDIA_TEMP_DIR")
			}
			key, err := daemon.Pair(rc.Server, rc.PairingCode, rc.Name, rc.ConfigPath, rc.MediaDir, saveTempDir)
			if err != nil {
				log.Fatalf("Pairing failed: %v", err)
			}
			rc.APIKey = key
			if loadedCfg, err := daemon.LoadConfig(rc.ConfigPath); err == nil && loadedCfg != nil {
				rc.LibraryID = loadedCfg.LibraryID
				rc.Libraries = loadedCfg.Libraries
			}
		}

		d, err := daemon.Start(context.Background(), rc)
		if err != nil {
			log.Fatalf("%v", err)
		}

		circuitBreakerChan := make(chan struct{}, 1)
		go func() {
			for ev := range d.Events() {
				if ev.Type == daemon.EventError && strings.Contains(ev.Message, "session usurped") {
					slog.Error("Exiting: session usurped by another active agent instance")
					select {
					case circuitBreakerChan <- struct{}{}:
					default:
					}
				}
			}
		}()

		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, shutdownSignals()...)
		select {
		case sig := <-sigChan:
			slog.Info("Shutdown signal received", "signal", sig.String())
		case <-circuitBreakerChan:
			slog.Info("Shutting down daemon due to session usurpation")
		}
		if err := d.Shutdown(context.Background()); err != nil {
			slog.Error("Agent shutdown incomplete", "error", err)
		}
	case "update":
		updateCmd := flag.NewFlagSet("update", flag.ExitOnError)
		updateServerFlag := updateCmd.String("server", "", "SaaS server URL to download the update from  [env SLIMMEDIA_SERVER]")
		updateKeyFlag := updateCmd.String("api-key", "", "SaaS API key (only needed if the server sets AGENT_DOWNLOAD_AUTH_REQUIRED)")
		updateInsecureFlag := updateCmd.Bool("insecure", false, "Allow plaintext http:// transport to non-loopback servers (never disables TLS certificate verification)")
		updateForceFlag := updateCmd.Bool("force", false, "Reinstall even if the server reports no newer version")
		updateCmd.Parse(os.Args[2:])

		warnSecretFlagInArgv("api-key", "SLIMMEDIA_API_KEY")

		if err := daemon.RunUpdate(*updateServerFlag, *updateKeyFlag, *updateInsecureFlag, *updateForceFlag); err != nil {
			log.Fatalf("Update failed: %v", err)
		}
	case "update-helper":
		// Hidden: spawned detached by the updater to swap a staged binary in
		// for the running agent's locked executable (Windows); see
		// agent/internal/updater/helper.go. Not an operator command.
		runUpdateHelper(os.Args[2:])
	case "audit":
		auditCmd := flag.NewFlagSet("audit", flag.ExitOnError)
		auditServerFlag := auditCmd.String("server", "", "SaaS server URL to report results to (optional)")
		auditKeyFlag := auditCmd.String("api-key", "", "SaaS API key for reporting results (optional)")
		auditInsecureFlag := auditCmd.Bool("insecure", false, "Allow plaintext http:// transport to non-loopback servers (never disables TLS certificate verification)")
		auditCmd.Parse(os.Args[2:])
		if auditCmd.NArg() < 1 {
			fmt.Println("Usage: slimmedia-agent audit <path> [--server <url>] [--api-key <key>] [--insecure]")
			return
		}
		auditServer, auditKey := auditConfigFallback(*auditServerFlag, *auditKeyFlag)
		runAudit(auditCmd.Arg(0), auditServer, auditKey, *auditInsecureFlag)

	case "license", "--license", "-license":
		licCmd := flag.NewFlagSet("license", flag.ExitOnError)
		fullFlag := licCmd.Bool("full", false, "Display the full GNU General Public License v3.0 text")
		licCmd.Parse(os.Args[2:])
		licenses.PrintNotice(os.Stdout, *fullFlag)

	default:
		fmt.Printf("Unknown command: %s\n", command)
	}
}

// defaultLogDir resolves where agent logs live: $LOG_DIR if set, otherwise a
// fixed per-user state directory (never cwd-relative).
func defaultLogDir() string {
	if d := os.Getenv("LOG_DIR"); d != "" {
		return d
	}
	return filepath.Join(daemon.StateDir(), "logs")
}

// warnSecretFlagInArgv flags secrets passed on the command line, which are
// visible to any local user via ps. The flag keeps working for compatibility;
// the env var (or --pair) is the recommended path.
func warnSecretFlagInArgv(flagName, envVar string) {
	for _, arg := range os.Args[1:] {
		if arg == "--"+flagName || strings.HasPrefix(arg, "--"+flagName+"=") {
			log.Printf("WARNING: --%s on the command line is visible in process listings (ps); prefer the %s environment variable or --pair", flagName, envVar)
			return
		}
	}
}
