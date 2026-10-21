package updater

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestHelperArgsRoundTrip proves the flag line SpawnSwapHelper builds is
// exactly what parseHelperFlags reads back — a garbled round trip would send
// the helper after the wrong files.
func TestHelperArgsRoundTrip(t *testing.T) {
	orig := SwapParams{
		Target:      `C:\app\slimmedia-agent.exe`,
		Staged:      `C:\Users\me\AppData\Roaming\slimmedia\updates\new.exe`,
		Backup:      `C:\app\slimmedia-agent.exe.old`,
		StateDir:    `C:\Users\me\AppData\Roaming\slimmedia`,
		PID:         4242,
		Args:        []string{"run", "--name", `weird "quoted" agent`},
		FromVersion: "1.4.0",
		ToVersion:   "1.5.0",
	}
	argsJSON := `["run","--name","weird \"quoted\" agent"]`
	p, err := parseHelperFlags(helperArgs(orig, argsJSON)[1:])
	if err != nil {
		t.Fatalf("parseHelperFlags: %v", err)
	}
	if p.Target != orig.Target || p.Staged != orig.Staged || p.Backup != orig.Backup ||
		p.StateDir != orig.StateDir || p.PID != orig.PID ||
		p.FromVersion != orig.FromVersion || p.ToVersion != orig.ToVersion {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", p, orig)
	}
	if len(p.Args) != 3 || p.Args[0] != "run" || p.Args[1] != "--name" || p.Args[2] != `weird "quoted" agent` {
		t.Fatalf("Args round trip = %q", p.Args)
	}
	if p.WaitTimeout != defaultWaitTimeout || p.GracePeriod != defaultGracePeriod {
		t.Fatalf("defaults not applied: wait=%v grace=%v", p.WaitTimeout, p.GracePeriod)
	}
}

func TestParseHelperFlagsRequiresPaths(t *testing.T) {
	if _, err := parseHelperFlags([]string{"-target", "/a"}); err == nil {
		t.Fatal("expected error for missing staged/backup")
	}
	if _, err := parseHelperFlags([]string{"--bogus"}); err == nil {
		t.Fatal("expected error for unknown flag")
	}
}

func TestTakeUpdateResult(t *testing.T) {
	stateDir := t.TempDir()

	res, err := TakeUpdateResult(stateDir)
	if err != nil || res != nil {
		t.Fatalf("absent marker = (%v, %v), want (nil, nil)", res, err)
	}

	want := UpdateResult{Status: UpdateResultApplied, FromVersion: "1.4.0", ToVersion: "1.5.0", Time: time.Now()}
	if err := WriteUpdateResult(stateDir, want); err != nil {
		t.Fatalf("WriteUpdateResult: %v", err)
	}
	res, err = TakeUpdateResult(stateDir)
	if err != nil {
		t.Fatalf("TakeUpdateResult: %v", err)
	}
	if res == nil || res.Status != want.Status || res.FromVersion != want.FromVersion || res.ToVersion != want.ToVersion {
		t.Fatalf("marker = %+v, want %+v", res, want)
	}

	// The marker is consumed: a second take finds nothing.
	if res, err := TakeUpdateResult(stateDir); res != nil || err != nil {
		t.Fatalf("marker not consumed: (%v, %v)", res, err)
	}

	// A corrupt marker is removed and reported, not replayed forever.
	if err := os.WriteFile(filepath.Join(stateDir, updateResultFile), []byte("{not json"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := TakeUpdateResult(stateDir); err == nil {
		t.Fatal("expected error for corrupt marker")
	}
	if _, err := os.Stat(filepath.Join(stateDir, updateResultFile)); !os.IsNotExist(err) {
		t.Fatal("corrupt marker was not removed")
	}
}

// The runSwap tests below exercise the full swap decision chain with shell
// scripts standing in for agent binaries. They launch real (short-lived)
// processes, so they stay off the Windows CI gate (which builds and vets but
// does not execute).

func writeScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}
}

func swapTestParams(t *testing.T, targetBody, stagedBody string) (SwapParams, string, string) {
	t.Helper()
	dir := t.TempDir()
	target := filepath.Join(dir, "agent")
	staged := filepath.Join(dir, ".staged")
	stateDir := filepath.Join(dir, "state")
	writeScript(t, target, targetBody)
	writeScript(t, staged, stagedBody)
	return SwapParams{
		Target:      target,
		Staged:      staged,
		Backup:      target + ".old",
		StateDir:    stateDir,
		GracePeriod: 600 * time.Millisecond,
	}, target, stateDir
}

func TestRunSwapAppliesAndRelaunches(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("launches shell scripts")
	}
	dir := t.TempDir()
	proof := filepath.Join(dir, "relaunch-proof")
	// The staged "agent" proves its launch (args preserved) and then lingers
	// like a real daemon, so the grace watch sees a healthy process.
	p, target, stateDir := swapTestParams(t, "echo old-ran >> "+proof, "echo new-ran \"$@\" >> "+proof+"; sleep 30")
	p.Args = []string{"run", "--name", "agent"}

	var logBuf bytes.Buffer
	if err := runSwap(p, newTestLogger(&logBuf)); err != nil {
		t.Fatalf("runSwap: %v\nlog: %s", err, logBuf.String())
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if !strings.Contains(string(got), "new-ran") {
		t.Fatalf("target was not swapped; content: %q", got)
	}
	proofData, err := os.ReadFile(proof)
	if err != nil {
		t.Fatalf("relaunch proof missing (agent was not relaunched): %v", err)
	}
	if !strings.Contains(string(proofData), `new-ran run --name agent`) {
		t.Fatalf("relaunch args not preserved: %q", proofData)
	}
	res, err := TakeUpdateResult(stateDir)
	if err != nil || res == nil || res.Status != UpdateResultApplied {
		t.Fatalf("result marker = (%v, %v), want applied", res, err)
	}
}

func TestRunSwapRollbackWhenStagedMissing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("launches shell scripts")
	}
	p, target, stateDir := swapTestParams(t, "echo old", "echo new")
	os.Remove(p.Staged)

	var logBuf bytes.Buffer
	if err := runSwap(p, newTestLogger(&logBuf)); err == nil {
		t.Fatal("expected swap failure")
	}

	got, _ := os.ReadFile(target)
	if string(got) != "#!/bin/sh\necho old\n" {
		t.Fatalf("previous binary not restored, content: %q", got)
	}
	res, err := TakeUpdateResult(stateDir)
	if err != nil || res == nil || res.Status != UpdateResultRolledBack {
		t.Fatalf("result marker = (%v, %v), want rolled_back", res, err)
	}
}

func TestRunSwapRollbackWhenRelaunchCrashes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("launches shell scripts")
	}
	proof := filepath.Join(t.TempDir(), "proof")
	// The staged binary exits immediately; the grace watch must notice and
	// roll back to the previous binary, which it relaunches.
	p, target, stateDir := swapTestParams(t, "echo restored >> "+proof, "exit 1")

	var logBuf bytes.Buffer
	if err := runSwap(p, newTestLogger(&logBuf)); err == nil {
		t.Fatal("expected crash rollback error")
	}

	got, _ := os.ReadFile(target)
	if !strings.Contains(string(got), "restored") {
		t.Fatalf("previous binary not restored, content: %q", got)
	}
	res, err := TakeUpdateResult(stateDir)
	if err != nil || res == nil || res.Status != UpdateResultRolledBack {
		t.Fatalf("result marker = (%v, %v), want rolled_back", res, err)
	}
}

func TestRunSwapLeavesEverythingWhenParentNeverExits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX sleep")
	}
	p, target, stateDir := swapTestParams(t, "echo old", "echo new")
	p.WaitTimeout = 400 * time.Millisecond

	// A live child process stands in for the still-running agent: the helper
	// must give up without touching any file.
	sleep := startSleep(t)
	p.PID = sleep

	before, _ := os.ReadFile(target)
	var logBuf bytes.Buffer
	if err := runSwap(p, newTestLogger(&logBuf)); err != nil {
		t.Fatalf("runSwap should give up quietly, got: %v", err)
	}
	after, _ := os.ReadFile(target)
	if string(before) != string(after) {
		t.Fatal("target changed even though the agent never exited")
	}
	if _, err := os.Stat(p.Backup); !os.IsNotExist(err) {
		t.Fatal("backup created even though the agent never exited")
	}
	if _, err := TakeUpdateResult(stateDir); err != nil {
		t.Fatalf("marker written even though nothing happened: %v", err)
	}
}

func startSleep(t *testing.T) int {
	t.Helper()
	cmd := helperSleepCommand()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() { cmd.Process.Kill() })
	return cmd.Process.Pid
}

func TestRunSwapOverwritesExistingFilesCleanly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("launches shell scripts")
	}
	p, target, stateDir := swapTestParams(t, "echo old; sleep 30", "echo new; sleep 30")
	// Pre-create backup to ensure runSwap removes existing backup before rename
	if err := os.WriteFile(p.Backup, []byte("stale backup"), 0644); err != nil {
		t.Fatalf("write backup: %v", err)
	}

	var logBuf bytes.Buffer
	if err := runSwap(p, newTestLogger(&logBuf)); err != nil {
		t.Fatalf("runSwap failed with existing files: %v\nlog: %s", err, logBuf.String())
	}

	got, _ := os.ReadFile(target)
	if string(got) != "#!/bin/sh\necho new; sleep 30\n" {
		t.Fatalf("target was not updated to new binary: %q", got)
	}
	res, err := TakeUpdateResult(stateDir)
	if err != nil || res == nil || res.Status != UpdateResultApplied {
		t.Fatalf("result marker = (%v, %v), want applied", res, err)
	}
}

