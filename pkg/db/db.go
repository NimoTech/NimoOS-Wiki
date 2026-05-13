package db

import (
	"database/sql"
	"fmt"

	_ "github.com/mattn/go-sqlite3"
)

const pragmas = `
PRAGMA journal_mode = WAL;
PRAGMA busy_timeout = 5000;
PRAGMA synchronous = NORMAL;
PRAGMA case_sensitive_like = ON;
PRAGMA foreign_keys = ON;
PRAGMA temp_store = MEMORY;
`

// Open opens (and creates if needed) the wiki sqlite DB.
// Uses DSN params for journal_mode/busy_timeout/foreign_keys as belt-and-suspenders
// for any connection-pool members that bypass the per-conn pragmas applied below.
// Forces a single open connection to avoid PRAGMA-init races across pool members.
func Open(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_busy_timeout=5000&_journal_mode=WAL&_foreign_keys=on", path)
	d, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	d.SetMaxOpenConns(1)
	d.SetMaxIdleConns(1)

	if _, err := d.Exec(pragmas); err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("pragmas: %w", err)
	}
	if err := runMigrations(d); err != nil {
		_ = d.Close()
		return nil, err
	}
	return d, nil
}
