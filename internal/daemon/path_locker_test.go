package daemon

import (
	"testing"
)

func TestPathLocker_TryLockAndUnlock(t *testing.T) {
	locker := NewPathLocker()

	paths := []string{"/media/movies/test.mkv", "/scratch/test.mkv"}
	if err := locker.TryLock("job-1", paths); err != nil {
		t.Fatalf("TryLock failed for job-1: %v", err)
	}

	// Attempting to lock an overlapping path with another job must fail
	if err := locker.TryLock("job-2", []string{"/media/movies/test.mkv"}); err == nil {
		t.Fatal("expected TryLock to fail for job-2 on duplicate path, but got nil")
	}

	// Unrelated paths for job-2 must succeed
	if err := locker.TryLock("job-2", []string{"/media/movies/other.mkv"}); err != nil {
		t.Fatalf("expected TryLock to succeed for unrelated path, got: %v", err)
	}

	// Unlocking job-1 releases its paths
	locker.Unlock("job-1")

	// Now job-3 can lock the previously locked path
	if err := locker.TryLock("job-3", []string{"/media/movies/test.mkv"}); err != nil {
		t.Fatalf("expected TryLock to succeed after unlock, got: %v", err)
	}
}

func TestPathLocker_ReentrantLocking(t *testing.T) {
	locker := NewPathLocker()

	paths := []string{"/media/movies/test.mkv", "/scratch/test.mkv"}
	if err := locker.TryLock("job-1", paths); err != nil {
		t.Fatalf("initial TryLock failed for job-1: %v", err)
	}

	// Re-entrant lock by the exact same job ID on the same paths must succeed
	if err := locker.TryLock("job-1", paths); err != nil {
		t.Fatalf("re-entrant TryLock for job-1 on identical paths failed: %v", err)
	}

	// Re-entrant lock by the same job ID with additional paths must succeed
	if err := locker.TryLock("job-1", []string{"/media/movies/test.mkv", "/segments/test"}); err != nil {
		t.Fatalf("re-entrant TryLock for job-1 with additional path failed: %v", err)
	}

	// Conflicting job-2 on any path owned by job-1 must fail
	if err := locker.TryLock("job-2", []string{"/segments/test"}); err == nil {
		t.Fatal("expected TryLock for job-2 on /segments/test to fail, but got nil")
	}

	// Unlocking job-1 releases all paths including the re-entrant additions
	locker.Unlock("job-1")

	if err := locker.TryLock("job-2", []string{"/segments/test", "/media/movies/test.mkv"}); err != nil {
		t.Fatalf("expected job-2 to acquire paths after job-1 unlock, got: %v", err)
	}
}
