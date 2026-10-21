-- name: InsertOfflineSummary :exec
INSERT INTO offline_summaries (payload) VALUES (?);

-- name: GetOfflineSummaries :many
SELECT id, payload FROM offline_summaries ORDER BY id ASC;

-- name: DeleteOfflineSummary :exec
DELETE FROM offline_summaries WHERE id = ?;

-- name: PruneExpiredOfflineSummaries :exec
DELETE FROM offline_summaries WHERE created_at < datetime('now', '-7 days');

-- name: PruneOldestOfflineSummaries :exec
DELETE FROM offline_summaries WHERE id NOT IN (
    SELECT id FROM offline_summaries ORDER BY id DESC LIMIT ?
);

-- name: UpsertScannedFile :exec
INSERT INTO scanned_files (file_path, scanned_at) VALUES (?, CURRENT_TIMESTAMP)
ON CONFLICT(file_path) DO UPDATE SET scanned_at = CURRENT_TIMESTAMP;

-- name: HasScannedFile :one
SELECT EXISTS(SELECT 1 FROM scanned_files WHERE file_path = ?);

-- name: ListScannedFiles :many
SELECT file_path, scanned_at FROM scanned_files;

-- name: DeleteScannedFile :exec
DELETE FROM scanned_files WHERE file_path = ?;

-- name: UpsertCompletedTranscode :exec
INSERT INTO completed_transcodes (file_path, file_hash, source_codec, target_codec, completed_at, output_size)
VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP, ?)
ON CONFLICT(file_path) DO UPDATE SET 
	file_hash = excluded.file_hash,
	source_codec = excluded.source_codec,
	target_codec = excluded.target_codec,
	completed_at = excluded.completed_at,
	output_size = excluded.output_size;

-- name: GetCompletedTranscode :one
SELECT file_path, file_hash, source_codec, target_codec, completed_at, output_size
FROM completed_transcodes
WHERE file_path = ?;

-- name: PruneCompletedTranscodesOlderThan :exec
-- The cutoff must be a TEXT value in SQLite's CURRENT_TIMESTAMP format
-- ("YYYY-MM-DD HH:MM:SS" UTC); the driver mis-compares bound time.Time
-- values against the text-stored timestamps.
DELETE FROM completed_transcodes WHERE completed_at < CAST(? AS TEXT);

-- name: PruneOldestCompletedTranscodes :exec
DELETE FROM completed_transcodes WHERE file_path NOT IN (
    SELECT file_path FROM completed_transcodes ORDER BY completed_at DESC, file_path DESC LIMIT ?
);

-- name: UpsertJobJournal :exec
INSERT INTO job_journal (job_id, file_path, state, updated_at)
VALUES (?, ?, ?, CURRENT_TIMESTAMP)
ON CONFLICT(job_id) DO UPDATE SET 
	state = excluded.state,
	updated_at = CURRENT_TIMESTAMP;

-- name: DeleteJobJournal :exec
DELETE FROM job_journal WHERE job_id = ?;

-- name: ListIncompleteJobs :many
SELECT job_id, file_path, state, updated_at FROM job_journal;

-- name: GetJobJournal :one
SELECT job_id, file_path, state, updated_at FROM job_journal WHERE job_id = ?;

-- name: UpsertJobLease :exec
INSERT INTO job_leases (job_id, lease_status, lease_expires_at, updated_at)
VALUES (?, ?, ?, CURRENT_TIMESTAMP)
ON CONFLICT(job_id) DO UPDATE SET 
	lease_status = excluded.lease_status,
	lease_expires_at = excluded.lease_expires_at,
	updated_at = CURRENT_TIMESTAMP;

-- name: DeleteJobLease :exec
DELETE FROM job_leases WHERE job_id = ?;

-- name: GetJobLease :one
SELECT job_id, lease_status, lease_expires_at, updated_at FROM job_leases WHERE job_id = ?;

-- name: ListActiveJobLeases :many
SELECT job_id, lease_status, lease_expires_at, updated_at FROM job_leases;
