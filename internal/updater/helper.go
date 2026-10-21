package updater

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"
)

// Update result statuses recorded in the on-disk marker the swap helper
// leaves for the relaunched agent.
const (
	UpdateResultApplied    = "applied"
	UpdateResultRolledBack = "rolled_back"
)

// UpdateResult is the marker the swap helper writes under the state dir
// before relaunching the agent. The relaunched daemon reads it at startup
// (TakeUpdateResult) and reports the outcome on its events channel, which is
// how update-applied and update-rolled-back reach the tray (#79 seam).
type UpdateResult struct {
	Status      string    `json:"status"`
	FromVersion string    `json:"from_version,omitempty"`
	ToVersion   string    `json:"to_version,omitempty"`
	Time        time.Time `json:"time"`
}

// updateResultFile is the marker name inside the state dir.
const updateResultFile = "update_result.json"

// WriteUpdateResult records the outcome marker in stateDir. Best-effort by
// the helper: a lost marker only loses the applied/rolled-back event, never
// the binary state.
func WriteUpdateResult(stateDir string, res UpdateResult) error {
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return err
	}
	data, err := json.Marshal(res)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(stateDir, updateResultFile), data, 0644)
}

// TakeUpdateResult reads and removes the outcome marker left by the swap
// helper. It returns (nil, nil) when no marker exists, so daemon startup can
// treat "no marker" as the common case. A malformed marker is removed and
// reported as an error: the update outcome is unknown, but the marker must
// not fire again on every startup.
func TakeUpdateResult(stateDir string) (*UpdateResult, error) {
	path := filepath.Join(stateDir, updateResultFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	os.Remove(path)
	var res UpdateResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("could not parse update result marker: %w", err)
	}
	return &res, nil
}

// SwapParams configures the Windows swap-and-relaunch flow. Target is the
// running (locked) executable, Staged the verified replacement, Backup the
// rollback copy of Target. PID is the still-running agent to wait on; zero
// skips the wait. Args are passed to the relaunched agent.
type SwapParams struct {
	Target      string
	Staged      string
	Backup      string
	StateDir    string
	PID         int
	Args        []string
	FromVersion string
	ToVersion   string

	// WaitTimeout bounds how long the helper waits for the agent to exit
	// before giving up without touching any file. GracePeriod bounds how
	// long it watches the relaunched agent before concluding the swap
	// succeeded. Zero values select the defaults.
	WaitTimeout time.Duration
	GracePeriod time.Duration
}

const (
	defaultWaitTimeout = 2 * time.Minute
	defaultGracePeriod = 10 * time.Second
	pollInterval       = 250 * time.Millisecond
)

// errStillRunning signals that the supervised PID never exited within the
// wait budget; the helper leaves every file untouched in that case.
var errStillRunning = errors.New("process did not exit in time")

// SpawnSwapHelper launches the detached helper process that performs the
// actual swap: it waits for this process to exit, renames Target to Backup,
// renames Staged to Target, and relaunches the agent. The helper is this
// same binary running the hidden `update-helper` subcommand.
func SpawnSwapHelper(p SwapParams) error {
	self, err := ExecutablePath()
	if err != nil {
		return err
	}
	cmd, err := helperCommand(self, p)
	if err != nil {
		return err
	}
	return cmd.Start()
}

// RunHelper executes the swap-and-relaunch sequence. It is the body of the
// hidden `update-helper` subcommand and is separated from flag parsing so
// the whole decision chain is testable. Every failure path restores the
// previous binary before relaunching it, so a failed update never leaves
// the agent un-runnable; the rollback outcome is recorded in the marker.
func RunHelper(args []string, stderr io.Writer) error {
	p, err := parseHelperFlags(args)
	if err != nil {
		return err
	}
	if stderr == nil {
		stderr = os.Stderr
	}

	logger := log.New(stderr, "update-helper: ", log.LstdFlags|log.Lmsgprefix)
	if p.StateDir != "" {
		if logPath := helperLogPath(p.StateDir); logPath != "" {
			if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644); err == nil {
				defer f.Close()
				logger.SetOutput(io.MultiWriter(stderr, f))
			}
		}
	}

	return runSwap(p, logger)
}

// helperLogPath returns where the helper's diagnostics persist across its
// detached lifetime; empty means "no persistent log".
func helperLogPath(stateDir string) string {
	if stateDir == "" {
		return ""
	}
	return filepath.Join(stateDir, "logs", "update-helper.log")
}

// parseHelperFlags decodes the command line SpawnSwapHelper built. Unknown
// or malformed flags are a hard error: the helper would rather do nothing
// than swap the wrong file.
func parseHelperFlags(args []string) (SwapParams, error) {
	var p SwapParams
	fs := flag.NewFlagSet("update-helper", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&p.Target, "target", "", "path of the running executable to replace")
	fs.StringVar(&p.Staged, "staged", "", "path of the verified staged binary")
	fs.StringVar(&p.Backup, "backup", "", "path for the rollback copy of the target")
	fs.StringVar(&p.StateDir, "state-dir", "", "per-user state dir for the result marker and helper log")
	fs.IntVar(&p.PID, "pid", 0, "PID of the agent process to wait on before swapping")
	fs.StringVar(&p.FromVersion, "from-version", "", "agent version being replaced")
	fs.StringVar(&p.ToVersion, "to-version", "", "agent version being installed")
	var argsJSON string
	fs.StringVar(&argsJSON, "args-json", "", "JSON array of arguments for the relaunched agent")
	if err := fs.Parse(args); err != nil {
		return p, fmt.Errorf("invalid helper arguments: %w", err)
	}
	if p.Target == "" || p.Staged == "" || p.Backup == "" {
		return p, fmt.Errorf("target, staged, and backup are all required")
	}
	if argsJSON != "" {
		if err := json.Unmarshal([]byte(argsJSON), &p.Args); err != nil {
			return p, fmt.Errorf("invalid args-json: %w", err)
		}
	}
	if p.WaitTimeout <= 0 {
		p.WaitTimeout = defaultWaitTimeout
	}
	if p.GracePeriod <= 0 {
		p.GracePeriod = defaultGracePeriod
	}
	return p, nil
}

// runSwap is the testable core of the helper: wait for the agent to exit,
// swap the binary with rollback on every failure, relaunch, and record the
// outcome marker. When the agent never exits the helper returns nil without
// changing anything — the running agent is untouched and remains the
// fallback by construction.
func runSwap(p SwapParams, logger *log.Logger) error {
	if p.PID > 0 {
		if err := waitForProcessExit(p.PID, p.WaitTimeout); err != nil {
			if errors.Is(err, errStillRunning) {
				logger.Printf("agent (pid %d) still running after %s; leaving everything untouched", p.PID, p.WaitTimeout)
				return nil
			}
			logger.Printf("could not wait for agent (pid %d): %v", p.PID, err)
			return nil
		}
	}

	// The agent has exited; its executable can now be replaced. First move
	// it aside so any failure below can restore the exact previous binary.
	_ = os.Remove(p.Backup)
	if err := os.Rename(p.Target, p.Backup); err != nil {
		logger.Printf("could not back up %s: %v (is the agent still running?)", p.Target, err)
		return nil
	}
	restore := func() {
		_ = os.Remove(p.Target)
		if err := os.Rename(p.Backup, p.Target); err != nil {
			logger.Printf("ROLLBACK FAILED: could not restore %s from %s: %v", p.Target, p.Backup, err)
		}
	}

	_ = os.Remove(p.Target)
	if err := os.Rename(p.Staged, p.Target); err != nil {
		logger.Printf("could not install staged binary: %v", err)
		restore()
		markResult(p, UpdateResultRolledBack, logger)
		return fmt.Errorf("could not install staged binary: %w", err)
	}

	markResult(p, UpdateResultApplied, logger)
	pid, exited, err := launchDetached(p.Target, p.Args)
	if err != nil {
		logger.Printf("could not relaunch agent: %v", err)
		restore()
		if _, _, relaunchErr := launchDetached(p.Target, p.Args); relaunchErr != nil {
			logger.Printf("could not relaunch previous binary either: %v", relaunchErr)
		}
		markResult(p, UpdateResultRolledBack, logger)
		return fmt.Errorf("could not relaunch agent: %w", err)
	}

	if !watchProcess(exited, p.GracePeriod) {
		logger.Printf("relaunched agent (pid %d) exited within %s; rolling back to previous binary", pid, p.GracePeriod)
		restore()
		if _, _, relaunchErr := launchDetached(p.Target, p.Args); relaunchErr != nil {
			logger.Printf("could not relaunch previous binary: %v", relaunchErr)
		}
		markResult(p, UpdateResultRolledBack, logger)
		return fmt.Errorf("relaunched agent exited immediately")
	}

	logger.Printf("update to %s installed and agent relaunched (pid %d)", p.ToVersion, pid)
	return nil
}

func markResult(p SwapParams, status string, logger *log.Logger) {
	if p.StateDir == "" {
		return
	}
	res := UpdateResult{Status: status, FromVersion: p.FromVersion, ToVersion: p.ToVersion, Time: time.Now()}
	if err := WriteUpdateResult(p.StateDir, res); err != nil {
		logger.Printf("could not write update result marker: %v", err)
	}
}
