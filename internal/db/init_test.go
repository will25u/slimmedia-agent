package db

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInitDB_FilePermissions(t *testing.T) {
	t.Run("created owner-only", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "bin")
		sqlDB, err := InitDB(dir)
		if err != nil {
			t.Fatalf("InitDB failed: %v", err)
		}
		defer sqlDB.Close()

		info, err := os.Stat(filepath.Join(dir, "agent.db"))
		if err != nil {
			t.Fatalf("stat agent.db: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0600 {
			t.Fatalf("agent.db mode = %o, want 0600", perm)
		}

		dirInfo, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat dir: %v", err)
		}
		if perm := dirInfo.Mode().Perm(); perm != 0700 {
			t.Fatalf("dir mode = %o, want 0700", perm)
		}
	})

	t.Run("pre-existing lax permissions tightened", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "bin")
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		dbPath := filepath.Join(dir, "agent.db")
		// Simulate a database created by an older build (0644).
		if err := os.WriteFile(dbPath, []byte{}, 0644); err != nil {
			t.Fatalf("write agent.db: %v", err)
		}

		sqlDB, err := InitDB(dir)
		if err != nil {
			t.Fatalf("InitDB failed: %v", err)
		}
		defer sqlDB.Close()

		info, err := os.Stat(dbPath)
		if err != nil {
			t.Fatalf("stat agent.db: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0600 {
			t.Fatalf("agent.db mode = %o after InitDB, want 0600", perm)
		}

		dirInfo, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat dir: %v", err)
		}
		if perm := dirInfo.Mode().Perm(); perm != 0700 {
			t.Fatalf("dir mode = %o after InitDB, want 0700", perm)
		}
	})
}
