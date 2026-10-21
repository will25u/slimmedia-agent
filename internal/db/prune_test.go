package db

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func setupPruneTestDB(t *testing.T) (*Queries, context.Context) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0755); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	sqlDB, err := InitDB(filepath.Join(dir, "bin"))
	if err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	return New(sqlDB), context.Background()
}

func TestListAndDeleteScannedFile(t *testing.T) {
	q, ctx := setupPruneTestDB(t)

	for _, path := range []string{"/media/movie.mkv", "/media/show.mkv"} {
		if err := q.UpsertScannedFile(ctx, path); err != nil {
			t.Fatalf("UpsertScannedFile(%q) failed: %v", path, err)
		}
	}

	rows, err := q.ListScannedFiles(ctx)
	if err != nil {
		t.Fatalf("ListScannedFiles failed: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	for _, row := range rows {
		if row.FilePath == "" || row.ScannedAt.IsZero() {
			t.Fatalf("row missing fields: %+v", row)
		}
	}

	if err := q.DeleteScannedFile(ctx, "/media/movie.mkv"); err != nil {
		t.Fatalf("DeleteScannedFile failed: %v", err)
	}

	rows, err = q.ListScannedFiles(ctx)
	if err != nil {
		t.Fatalf("ListScannedFiles after delete failed: %v", err)
	}
	if len(rows) != 1 || rows[0].FilePath != "/media/show.mkv" {
		t.Fatalf("expected only /media/show.mkv to remain, got %+v", rows)
	}

	// Deleting a path that is not present must not error.
	if err := q.DeleteScannedFile(ctx, "/media/movie.mkv"); err != nil {
		t.Fatalf("DeleteScannedFile of absent path failed: %v", err)
	}
}

func TestPruneCompletedTranscodesOlderThan(t *testing.T) {
	q, ctx := setupPruneTestDB(t)

	// Seed three rows; backdate two of them via raw SQL since the upsert
	// always stamps CURRENT_TIMESTAMP.
	seed := []struct {
		path string
		ts   string
	}{
		{"/media/old-a.mkv", "2026-06-01 10:00:00"},
		{"/media/old-b.mkv", "2026-07-05 10:00:00"},
		{"/media/recent.mkv", "2026-10-09 10:00:00"},
	}
	for _, s := range seed {
		if err := q.UpsertCompletedTranscode(ctx, UpsertCompletedTranscodeParams{
			FilePath: s.path, FileHash: "h", SourceCodec: "h264", TargetCodec: "av1", OutputSize: 1,
		}); err != nil {
			t.Fatalf("UpsertCompletedTranscode(%q) failed: %v", s.path, err)
		}
		if _, err := q.db.ExecContext(ctx,
			"UPDATE completed_transcodes SET completed_at = ? WHERE file_path = ?", s.ts, s.path); err != nil {
			t.Fatalf("backdating %q failed: %v", s.path, err)
		}
	}

	// Cutoff between the two old rows and the recent one.
	if err := q.PruneCompletedTranscodesOlderThan(ctx, "2026-08-01 00:00:00"); err != nil {
		t.Fatalf("PruneCompletedTranscodesOlderThan failed: %v", err)
	}

	if _, err := q.GetCompletedTranscode(ctx, "/media/old-a.mkv"); err == nil {
		t.Fatalf("expected old-a to be pruned")
	}
	if _, err := q.GetCompletedTranscode(ctx, "/media/old-b.mkv"); err == nil {
		t.Fatalf("expected old-b to be pruned")
	}
	if _, err := q.GetCompletedTranscode(ctx, "/media/recent.mkv"); err != nil {
		t.Fatalf("expected recent to survive, got error: %v", err)
	}
}

func TestPruneOldestCompletedTranscodes(t *testing.T) {
	q, ctx := setupPruneTestDB(t)

	// Six rows with distinct completion timestamps, inserted out of order.
	for i, ts := range []string{
		"2026-07-04 10:00:00",
		"2026-07-01 10:00:00",
		"2026-07-06 10:00:00",
		"2026-07-02 10:00:00",
		"2026-07-05 10:00:00",
		"2026-07-03 10:00:00",
	} {
		path := "/media/f" + string(rune('0'+i)) + ".mkv"
		if err := q.UpsertCompletedTranscode(ctx, UpsertCompletedTranscodeParams{
			FilePath: path, FileHash: "h", SourceCodec: "h264", TargetCodec: "av1", OutputSize: 1,
		}); err != nil {
			t.Fatalf("UpsertCompletedTranscode(%q) failed: %v", path, err)
		}
		if _, err := q.db.ExecContext(ctx,
			"UPDATE completed_transcodes SET completed_at = ? WHERE file_path = ?", ts, path); err != nil {
			t.Fatalf("backdating %q failed: %v", path, err)
		}
	}

	// Cap at 3: the three newest by completed_at (f2/f4/f0, from 07-06/05/04)
	// must survive.
	if err := q.PruneOldestCompletedTranscodes(ctx, 3); err != nil {
		t.Fatalf("PruneOldestCompletedTranscodes failed: %v", err)
	}

	survivors := map[string]bool{"/media/f0.mkv": true, "/media/f2.mkv": true, "/media/f4.mkv": true}
	for i := 0; i < 6; i++ {
		path := "/media/f" + string(rune('0'+i)) + ".mkv"
		_, err := q.GetCompletedTranscode(ctx, path)
		if survivors[path] && err != nil {
			t.Fatalf("expected %q to survive, got error: %v", path, err)
		}
		if !survivors[path] && err == nil {
			t.Fatalf("expected %q to be pruned", path)
		}
	}

	// A cap larger than the table is a no-op.
	if err := q.PruneOldestCompletedTranscodes(ctx, 100); err != nil {
		t.Fatalf("PruneOldestCompletedTranscodes with generous cap failed: %v", err)
	}
	for path := range survivors {
		if _, err := q.GetCompletedTranscode(ctx, path); err != nil {
			t.Fatalf("expected %q to survive generous cap, got error: %v", path, err)
		}
	}
}
