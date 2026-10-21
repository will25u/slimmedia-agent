package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// InitDB initializes the SQLite database in WAL mode and sets up the schema.
// The database file (and its WAL sidecars) are created owner-only and any
// pre-existing lax permissions are tightened.
func InitDB(dataDir string) (*sql.DB, error) {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create data directory: %w", err)
	}
	_ = os.Chmod(dataDir, 0700)
	dbPath := filepath.Join(dataDir, "agent.db")

	// Create the file owner-only up front (the driver would create it with
	// umask-derived permissions), and fix databases created by older builds.
	f, err := os.OpenFile(dbPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("failed to create database file: %w", err)
	}
	_ = f.Close()
	_ = os.Chmod(dbPath, 0600)

	dsn := dbPath + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)

	// WAL sidecars inherit the driver's creation permissions; tighten them
	// best-effort (they may not exist until the first transaction).
	_ = os.Chmod(dbPath+"-wal", 0600)
	_ = os.Chmod(dbPath+"-shm", 0600)

	// Create tables if they don't exist
	schema := `
	CREATE TABLE IF NOT EXISTS completed_transcodes (
		file_path     TEXT NOT NULL,
		file_hash     TEXT NOT NULL,
		source_codec  TEXT NOT NULL,
		target_codec  TEXT NOT NULL,
		completed_at  TIMESTAMP NOT NULL,
		output_size   BIGINT NOT NULL,
		PRIMARY KEY (file_path)
	);
	CREATE TABLE IF NOT EXISTS offline_summaries (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		payload TEXT NOT NULL,
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	);
	CREATE TABLE IF NOT EXISTS scanned_files (
		file_path  TEXT NOT NULL,
		scanned_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (file_path)
	);
	CREATE TABLE IF NOT EXISTS job_journal (
		job_id TEXT PRIMARY KEY,
		file_path TEXT NOT NULL,
		state TEXT NOT NULL,
		updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	);
	CREATE TABLE IF NOT EXISTS job_leases (
		job_id TEXT PRIMARY KEY,
		lease_status TEXT NOT NULL,
		lease_expires_at TIMESTAMP,
		updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	);
	`
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("failed to create tables: %w", err)
	}

	return db, nil
}
