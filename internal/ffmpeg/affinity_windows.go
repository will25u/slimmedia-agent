//go:build windows

package ffmpeg

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

var procSetProcessAffinityMask = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetProcessAffinityMask")

// pinRunningProcess restricts the running process to the given logical CPUs.
// The handle is opened from the child's PID immediately after Start, so the
// pin always lands before the encoder opens its thread pools; the argv-side
// pools=/threads steering caps any oversubscription in that window anyway.
// Processor groups beyond the first 64 CPUs are not expressible through a
// single affinity mask and are rejected (the encode proceeds unpinned, argv
// steering intact).
func pinRunningProcess(p *os.Process, cpus []int) error {
	if p == nil {
		return fmt.Errorf("no process to pin")
	}
	var mask uintptr
	for _, c := range cpus {
		if c < 0 || c >= 64 {
			return fmt.Errorf("cpu %d outside processor group 0; affinity unsupported", c)
		}
		mask |= uintptr(1) << uint(c)
	}
	if mask == 0 {
		return fmt.Errorf("empty CPU set")
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_INFORMATION, false, uint32(p.Pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	r, _, err := procSetProcessAffinityMask.Call(uintptr(h), mask)
	if r == 0 {
		return err
	}
	return nil
}
