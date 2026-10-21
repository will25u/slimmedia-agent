//go:build !linux && !windows

package ffmpeg

import "os"

func pinRunningProcess(_ *os.Process, _ []int) error { return nil }
