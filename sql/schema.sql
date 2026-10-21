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

-- Canonical absolute paths of media files discovered by the local scanner.
-- The agent refuses server-pushed transcode jobs whose resolved input path is
-- not present in this table, so a compromised server cannot point the agent at
-- arbitrary host files.
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
