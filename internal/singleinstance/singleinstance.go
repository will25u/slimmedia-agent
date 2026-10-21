package singleinstance

import (
	"errors"
	"io"
)

// ErrAlreadyRunning indicates that another agent instance is currently active.
var ErrAlreadyRunning = errors.New("another agent instance is already running")

// Guard holds an acquired single-instance lock.
type Guard interface {
	io.Closer
}
