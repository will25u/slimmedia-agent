package singleinstance

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestSingleInstanceLock(t *testing.T) {
	tempDir := t.TempDir()
	stateDir := filepath.Join(tempDir, "state")

	guard1, err := Acquire(stateDir)
	if err != nil {
		t.Fatalf("first Acquire failed: %v", err)
	}
	defer guard1.Close()

	// Second acquire on the same stateDir must fail with ErrAlreadyRunning
	guard2, err := Acquire(stateDir)
	if !errors.Is(err, ErrAlreadyRunning) {
		if guard2 != nil {
			guard2.Close()
		}
		t.Fatalf("expected ErrAlreadyRunning, got %v", err)
	}

	// Release first lock
	if err := guard1.Close(); err != nil {
		t.Fatalf("failed to close guard1: %v", err)
	}

	// Third acquire must now succeed
	guard3, err := Acquire(stateDir)
	if err != nil {
		t.Fatalf("third Acquire after release failed: %v", err)
	}
	if err := guard3.Close(); err != nil {
		t.Fatalf("failed to close guard3: %v", err)
	}
}

func TestSingleInstanceLock_SeparateDirs(t *testing.T) {
	tempDir := t.TempDir()
	dir1 := filepath.Join(tempDir, "dir1")
	dir2 := filepath.Join(tempDir, "dir2")

	guard1, err := Acquire(dir1)
	if err != nil {
		t.Fatalf("Acquire dir1 failed: %v", err)
	}
	defer guard1.Close()

	guard2, err := Acquire(dir2)
	if err != nil {
		t.Fatalf("Acquire dir2 failed: %v", err)
	}
	defer guard2.Close()
}
