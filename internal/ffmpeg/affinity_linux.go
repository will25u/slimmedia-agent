//go:build linux

package ffmpeg

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// pinRunningProcess restricts the running process to the given logical CPUs.
// The pin lands just after process start, before the encoder opens its thread
// pools; the argv-side pools=/threads steering already caps oversubscription
// in that window. A failed pin is surfaced to the caller, which logs and
// continues.
func pinRunningProcess(p *os.Process, cpus []int) error {
	if p == nil {
		return fmt.Errorf("no process to pin")
	}
	var set unix.CPUSet
	for _, c := range cpus {
		if c >= 0 && c < len(set)*64 {
			set.Set(c)
		}
	}
	return unix.SchedSetaffinity(int(p.Pid), &set)
}
