package daemon

import (
	"context"
	"database/sql"
	"log"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/will25u/slimmedia-agent/internal/db"
)

// Retention policy for the agent's local SQLite registries (issue #231,
// review finding DB-L6). completed_transcodes only feeds transcode loop
// prevention, so dropping old rows merely means a re-pushed old file could
// be transcoded again. scanned_files feeds the server-push path-confinement
// allow-list, so its rows are removed only when the file no longer exists
// on disk — never by age alone, which could reject jobs for files that
// exist but simply have not been re-scanned recently.
const (
	completedTranscodeRetention = 90 * 24 * time.Hour
	maxCompletedTranscodes      = int64(5000)
)

// sqliteTimestampFormat matches the text format SQLite's CURRENT_TIMESTAMP
// writes, so cutoff strings compare lexicographically against stored values.
const sqliteTimestampFormat = "2006-01-02 15:04:05"

// pruneLocalRegistries reaps the agent's local SQLite registries. It runs
// right after a successful library scan, when existence information is
// freshest: scanned_files rows for deleted/renamed files are removed so the
// server-push confinement allow-list tracks reality, and completed_transcodes
// history is capped by age and count so the table cannot grow without bound.
func pruneLocalRegistries(agentDB *sql.DB) {
	if agentDB == nil {
		return
	}
	q := db.New(agentDB)
	ctx := context.Background()

	prunedScanned, err := pruneMissingScannedFiles(ctx, q)
	if err != nil {
		log.Printf("Failed to prune scanned files registry: %v", err)
	} else if prunedScanned > 0 {
		log.Printf("Pruned %d scanned file row(s) whose files no longer exist", prunedScanned)
	}

	if err := pruneCompletedTranscodeHistory(ctx, q); err != nil {
		log.Printf("Failed to prune completed transcode history: %v", err)
	}
}

// pruneMissingScannedFiles removes scanned_files rows whose path no longer
// exists on disk. Rows that cannot be statted for transient reasons
// (permissions, temporarily unmounted volumes) are kept rather than
// shrinking the allow-list on a false negative. Existence checks run concurrently
// across a bounded worker pool to avoid sequential latency on network shares.
func pruneMissingScannedFiles(ctx context.Context, q *db.Queries) (int, error) {
	rows, err := q.ListScannedFiles(ctx)
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}

	numWorkers := runtime.NumCPU() / 2
	if numWorkers < 2 {
		numWorkers = 2
	}
	if numWorkers > 4 {
		numWorkers = 4
	}
	if numWorkers > len(rows) {
		numWorkers = len(rows)
	}

	jobs := make(chan string, len(rows))
	for _, row := range rows {
		jobs <- row.FilePath
	}
	close(jobs)

	var (
		mu           sync.Mutex
		missingPaths []string
		wg           sync.WaitGroup
	)

	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range jobs {
				if ctx.Err() != nil {
					return
				}
				if _, statErr := os.Stat(p); os.IsNotExist(statErr) {
					mu.Lock()
					missingPaths = append(missingPaths, p)
					mu.Unlock()
				}
			}
		}()
	}

	wg.Wait()

	if ctx.Err() != nil {
		return 0, ctx.Err()
	}

	pruned := 0
	for _, p := range missingPaths {
		if err := q.DeleteScannedFile(ctx, p); err != nil {
			return pruned, err
		}
		pruned++
	}
	return pruned, nil
}

// pruneCompletedTranscodeHistory caps completed_transcodes by age and by
// total row count, keeping the most recently completed rows either way.
func pruneCompletedTranscodeHistory(ctx context.Context, q *db.Queries) error {
	cutoff := time.Now().Add(-completedTranscodeRetention).UTC().Format(sqliteTimestampFormat)
	if err := q.PruneCompletedTranscodesOlderThan(ctx, cutoff); err != nil {
		return err
	}
	return q.PruneOldestCompletedTranscodes(ctx, maxCompletedTranscodes)
}
