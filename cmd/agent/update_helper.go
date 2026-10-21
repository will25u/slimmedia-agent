package main

import (
	"fmt"
	"os"

	"github.com/will25u/slimmedia-agent/internal/updater"
)

// runUpdateHelper is the body of the hidden `update-helper` subcommand. The
// updater spawns it detached to swap a verified staged binary in for the
// running agent's locked executable after the agent exits, then relaunch the
// agent; every failure path restores the previous binary first. It is not
// listed in the usage text: operators use `slimmedia-agent update`, the
// dashboard uses AGENT_UPDATE, and this command only exists because Windows
// cannot rename over a running .exe.
func runUpdateHelper(args []string) {
	if err := updater.RunHelper(args, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "update-helper: %v\n", err)
		os.Exit(1)
	}
}
