package daemon

import (
	"fmt"
	"sync"
)

// PathLocker manages exclusive locks on filesystem paths across active transcode jobs.
type PathLocker interface {
	TryLock(jobID string, paths []string) error
	Unlock(jobID string)
}

type inMemoryPathLocker struct {
	mu         sync.Mutex
	pathOwners map[string]string   // canonical path -> jobID
	jobPaths   map[string][]string // jobID -> []canonical path
}

// NewPathLocker constructs an in-memory PathLocker.
func NewPathLocker() PathLocker {
	return &inMemoryPathLocker{
		pathOwners: make(map[string]string),
		jobPaths:   make(map[string][]string),
	}
}

func (l *inMemoryPathLocker) TryLock(jobID string, paths []string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Deduplicate input paths for this job
	seen := make(map[string]struct{}, len(paths))
	var uniquePaths []string
	for _, p := range paths {
		if p == "" {
			continue
		}
		if _, ok := seen[p]; !ok {
			seen[p] = struct{}{}
			uniquePaths = append(uniquePaths, p)
		}
	}

	// Verify no path is already locked by a different job
	for _, p := range uniquePaths {
		if owner, locked := l.pathOwners[p]; locked && owner != jobID {
			return fmt.Errorf("path %q is already locked by active job %q", p, owner)
		}
	}

	// Lock all paths
	for _, p := range uniquePaths {
		if l.pathOwners[p] != jobID {
			l.pathOwners[p] = jobID
			l.jobPaths[jobID] = append(l.jobPaths[jobID], p)
		}
	}

	return nil
}

func (l *inMemoryPathLocker) Unlock(jobID string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	for _, p := range l.jobPaths[jobID] {
		delete(l.pathOwners, p)
	}
	delete(l.jobPaths, jobID)
}
