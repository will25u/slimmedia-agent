package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/will25u/slimmedia-agent/internal/db"
	"github.com/will25u/slimmedia-agent/internal/scanner"
)

// TestPruneLocalRegistries verifies that after a library scan, rows for
// deleted files are dropped from the scanned-files confinement allow-list
// while surviving files stay listed, and that completed-transcode history
// is pruned by retention age (issue #231, review finding DB-L6).
func TestPruneLocalRegistries(t *testing.T) {
	sqlDB := setupTestDB(t)
	q := db.New(sqlDB)
	ctx := context.Background()

	mediaDir := t.TempDir()
	keptPath := filepath.Join(mediaDir, "kept.mkv")
	deletedPath := filepath.Join(mediaDir, "deleted.mkv")
	for _, p := range []string{keptPath, deletedPath} {
		if err := os.WriteFile(p, []byte("media"), 0600); err != nil {
			t.Fatalf("creating %q failed: %v", p, err)
		}
	}

	recordScannedFiles(sqlDB, []scanner.ScannedFile{{FilePath: keptPath}, {FilePath: deletedPath}}, mediaDir)

	if err := os.Remove(deletedPath); err != nil {
		t.Fatalf("removing %q failed: %v", deletedPath, err)
	}

	// Seed transcode history: one recent completion and one older than the
	// 90-day retention window (backdated via raw SQL; the upsert stamps
	// CURRENT_TIMESTAMP).
	if err := q.UpsertCompletedTranscode(ctx, db.UpsertCompletedTranscodeParams{
		FilePath: keptPath, FileHash: "h1", SourceCodec: "h264", TargetCodec: "av1", OutputSize: 100,
	}); err != nil {
		t.Fatalf("UpsertCompletedTranscode failed: %v", err)
	}
	oldPath := filepath.Join(mediaDir, "old.mkv")
	if err := q.UpsertCompletedTranscode(ctx, db.UpsertCompletedTranscodeParams{
		FilePath: oldPath, FileHash: "h2", SourceCodec: "h264", TargetCodec: "av1", OutputSize: 100,
	}); err != nil {
		t.Fatalf("UpsertCompletedTranscode failed: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx,
		"UPDATE completed_transcodes SET completed_at = '2026-06-01 10:00:00' WHERE file_path = ?", oldPath); err != nil {
		t.Fatalf("backdating old transcode failed: %v", err)
	}

	pruneLocalRegistries(sqlDB)

	exists, err := q.HasScannedFile(ctx, keptPath)
	if err != nil || !exists {
		t.Fatalf("expected surviving file to remain in allow-list: exists=%v err=%v", exists, err)
	}
	exists, err = q.HasScannedFile(ctx, deletedPath)
	if err != nil {
		t.Fatalf("HasScannedFile failed: %v", err)
	}
	if exists {
		t.Fatalf("expected deleted file to be pruned from the confinement allow-list")
	}

	if _, err := q.GetCompletedTranscode(ctx, oldPath); err == nil {
		t.Fatalf("expected transcode older than retention to be pruned")
	}
	if _, err := q.GetCompletedTranscode(ctx, keptPath); err != nil {
		t.Fatalf("expected recent transcode to survive pruning: %v", err)
	}
}

func TestPruneLocalRegistriesNilDB(t *testing.T) {
	pruneLocalRegistries(nil) // must not panic
}

func TestPruneMissingScannedFilesConcurrent(t *testing.T) {
	sqlDB := setupTestDB(t)
	q := db.New(sqlDB)
	ctx := context.Background()

	mediaDir := t.TempDir()
	totalFiles := 24
	var items []scanner.ScannedFile
	for i := 0; i < totalFiles; i++ {
		p := filepath.Join(mediaDir, "movie_"+string(rune('a'+i))+".mkv")
		if err := os.WriteFile(p, []byte("content"), 0600); err != nil {
			t.Fatalf("write file failed: %v", err)
		}
		items = append(items, scanner.ScannedFile{FilePath: p})
	}

	recordScannedFiles(sqlDB, items, mediaDir)

	// Delete half the files
	deletedCount := 0
	for i := 0; i < totalFiles; i += 2 {
		if err := os.Remove(items[i].FilePath); err != nil {
			t.Fatalf("remove failed: %v", err)
		}
		deletedCount++
	}

	pruneLocalRegistries(sqlDB)

	for i := 0; i < totalFiles; i++ {
		exists, err := q.HasScannedFile(ctx, items[i].FilePath)
		if err != nil {
			t.Fatalf("HasScannedFile failed: %v", err)
		}
		if i%2 == 0 {
			if exists {
				t.Fatalf("expected deleted file %q to be pruned", items[i].FilePath)
			}
		} else {
			if !exists {
				t.Fatalf("expected surviving file %q to remain", items[i].FilePath)
			}
		}
	}
}

