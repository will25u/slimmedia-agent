//go:build windows

package singleinstance

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

type mutexLock struct {
	handle windows.Handle
}

// Acquire acquires a named mutex on Windows.
// If another instance already created the mutex, it returns ErrAlreadyRunning.
func Acquire(stateDir string) (Guard, error) {
	cleanDir := strings.ToLower(filepath.Clean(stateDir))
	h := sha256.Sum256([]byte(cleanDir))
	mutexName := fmt.Sprintf(`Local\SlimMediaAgent_%x`, h[:8])

	namePtr, err := windows.UTF16PtrFromString(mutexName)
	if err != nil {
		return nil, fmt.Errorf("invalid mutex name: %w", err)
	}

	handle, err := windows.CreateMutex(nil, false, namePtr)
	if handle == 0 {
		return nil, fmt.Errorf("failed to create single-instance mutex: %w", err)
	}

	if err == windows.ERROR_ALREADY_EXISTS || errors.Is(err, windows.ERROR_ALREADY_EXISTS) || windows.GetLastError() == windows.ERROR_ALREADY_EXISTS {
		windows.CloseHandle(handle)
		return nil, ErrAlreadyRunning
	}

	return &mutexLock{handle: handle}, nil
}

func (l *mutexLock) Close() error {
	if l.handle != 0 {
		err := windows.CloseHandle(l.handle)
		l.handle = 0
		return err
	}
	return nil
}
