//go:build !windows

package singleinstance

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

type fileLock struct {
	file *os.File
	path string
}

// Acquire acquires an exclusive advisory flock on a lock file in stateDir.
// If another instance already holds the lock, it returns ErrAlreadyRunning.
func Acquire(stateDir string) (Guard, error) {
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create state directory for lock: %w", err)
	}

	lockPath := filepath.Join(stateDir, "slimmedia-agent.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("failed to open single-instance lock file: %w", err)
	}

	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrAlreadyRunning
		}
		return nil, fmt.Errorf("failed to lock single-instance lock file: %w", err)
	}

	// Record PID into the lock file
	_ = f.Truncate(0)
	_, _ = f.Seek(0, 0)
	_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
	_ = f.Sync()

	return &fileLock{file: f, path: lockPath}, nil
}

func (l *fileLock) Close() error {
	if l.file == nil {
		return nil
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	err := l.file.Close()
	_ = os.Remove(l.path)
	l.file = nil
	return err
}
