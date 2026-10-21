package daemon

import (
	"os"
	"strings"
)

// updateActivateParams bundles what the platform update installer needs:
// where to fetch the verified binary from and, on Windows, where to stage it
// and which version it carries (recorded in the swap result marker).
type updateActivateParams struct {
	apiURL       string
	downloadPath string
	checksum     string
	signature    string
	apiKey       string
	stateDir     string
	version      string
}

// relaunchArgs preserves the daemon's original arguments when relaunching
// after an update, so a `run` started with flags comes back with them.
// One-time setup flags like --pair are stripped to avoid re-registration
// errors against expired pairing codes. A non-run invocation falls back to `run`,
// which resolves credentials and settings from the saved config.
func relaunchArgs() []string {
	return cleanRelaunchArgs(os.Args)
}

// cleanRelaunchArgs processes raw CLI arguments for an agent restart,
// ensuring the command is run/start and removing one-time flags.
func cleanRelaunchArgs(rawArgs []string) []string {
	if len(rawArgs) > 1 && (rawArgs[1] == "run" || rawArgs[1] == "start") {
		return stripPairingArgs(rawArgs[1:])
	}
	return []string{"run"}
}

// stripPairingArgs removes any --pair / -pair flags and their values from args.
func stripPairingArgs(args []string) []string {
	var cleaned []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--pair" || arg == "-pair" {
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
			}
			continue
		}
		if strings.HasPrefix(arg, "--pair=") || strings.HasPrefix(arg, "-pair=") {
			continue
		}
		cleaned = append(cleaned, arg)
	}
	return cleaned
}
