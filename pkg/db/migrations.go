package db

import (
	"database/sql"
	"fmt"
)

var migrations = []string{
	`CREATE TABLE IF NOT EXISTS wiki_roots (
		id TEXT PRIMARY KEY,
		path TEXT UNIQUE NOT NULL,
		level TEXT NOT NULL,
		watch_mode TEXT NOT NULL,
		storage_mode TEXT NOT NULL,
		enabled INTEGER NOT NULL,
		scan_interval_s INTEGER NOT NULL,
		created_at INTEGER NOT NULL,
		last_scan_at INTEGER
	)`,
	`CREATE TABLE IF NOT EXISTS wiki_nodes (
		id TEXT PRIMARY KEY,
		root_id TEXT,
		path TEXT UNIQUE NOT NULL,
		level TEXT NOT NULL,
		child_count INTEGER NOT NULL DEFAULT 0,
		last_modified INTEGER,
		checksum_system TEXT,
		user_notes TEXT NOT NULL DEFAULT '',
		user_notes_etag TEXT NOT NULL DEFAULT '',
		user_notes_updated_at INTEGER,
		dirty INTEGER NOT NULL DEFAULT 0,
		last_flushed_at INTEGER,
		last_flushed_mtime INTEGER,
		updated_at INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_wiki_nodes_root ON wiki_nodes(root_id)`,
	`CREATE INDEX IF NOT EXISTS idx_wiki_nodes_dirty ON wiki_nodes(dirty) WHERE dirty = 1`,
	`CREATE TABLE IF NOT EXISTS file_index (
		id TEXT PRIMARY KEY,
		root_id TEXT NOT NULL,
		path TEXT NOT NULL,
		parent TEXT NOT NULL,
		is_dir INTEGER NOT NULL,
		is_opaque INTEGER NOT NULL DEFAULT 0,
		mtime INTEGER,
		size INTEGER,
		inode INTEGER,
		status TEXT NOT NULL,
		ext TEXT,
		UNIQUE(root_id, path)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_file_index_parent ON file_index(root_id, parent)`,
	`CREATE INDEX IF NOT EXISTS idx_file_index_path_prefix ON file_index(root_id, path)`,
	`CREATE TABLE IF NOT EXISTS file_events (
		id TEXT PRIMARY KEY,
		root_id TEXT NOT NULL,
		path TEXT NOT NULL,
		op TEXT NOT NULL,
		rename_to TEXT,
		is_dir INTEGER NOT NULL DEFAULT 0,
		detected_at INTEGER NOT NULL,
		processed_at INTEGER,
		archived INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX IF NOT EXISTS idx_file_events_unprocessed ON file_events(detected_at) WHERE processed_at IS NULL`,
	`CREATE INDEX IF NOT EXISTS idx_file_events_archive_q ON file_events(root_id, detected_at) WHERE archived = 0`,
	`CREATE TABLE IF NOT EXISTS parse_status (
		id TEXT PRIMARY KEY,
		path TEXT UNIQUE NOT NULL,
		status TEXT NOT NULL,
		indexed_at INTEGER,
		parser_version TEXT,
		error TEXT
	)`,
	`CREATE INDEX IF NOT EXISTS idx_parse_status_pending ON parse_status(status) WHERE status IN ('pending', 'failed')`,
}

func runMigrations(d *sql.DB) error {
	for _, stmt := range migrations {
		if _, err := d.Exec(stmt); err != nil {
			head := stmt
			if len(head) > 60 {
				head = head[:60]
			}
			return fmt.Errorf("migration failed (%s): %w", head, err)
		}
	}
	return nil
}
