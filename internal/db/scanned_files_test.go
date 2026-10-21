package db

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestScannedFilesRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0755); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	sqlDB, err := InitDB(filepath.Join(dir, "bin"))
	if err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}
	defer sqlDB.Close()

	q := New(sqlDB)
	ctx := context.Background()

	// Table exists after InitDB and starts empty.
	exists, err := q.HasScannedFile(ctx, "/media/movie.mkv")
	if err != nil {
		t.Fatalf("HasScannedFile failed: %v", err)
	}
	if exists {
		t.Fatalf("expected 0 matches, got true")
	}

	if err := q.UpsertScannedFile(ctx, "/media/movie.mkv"); err != nil {
		t.Fatalf("UpsertScannedFile failed: %v", err)
	}
	// Upsert is idempotent and refreshes rather than erroring.
	if err := q.UpsertScannedFile(ctx, "/media/movie.mkv"); err != nil {
		t.Fatalf("second UpsertScannedFile failed: %v", err)
	}

	exists, err = q.HasScannedFile(ctx, "/media/movie.mkv")
	if err != nil {
		t.Fatalf("HasScannedFile failed: %v", err)
	}
	if !exists {
		t.Fatalf("expected 1 match after upsert, got false")
	}

	// Different path must not match.
	exists, err = q.HasScannedFile(ctx, "/media/other.mkv")
	if err != nil {
		t.Fatalf("HasScannedFile failed: %v", err)
	}
	if exists {
		t.Fatalf("expected 0 matches for other path, got true")
	}
}
