package repo

import (
	"database/sql"
	"errors"
)

var ErrNotFound = errors.New("not found")

type WikiRootsRepo struct{ db *sql.DB }

func NewWikiRoots(db *sql.DB) *WikiRootsRepo { return &WikiRootsRepo{db} }

func (r *WikiRootsRepo) Insert(w WikiRoot) error {
	_, err := r.db.Exec(`INSERT INTO wiki_roots
		(id, path, level, watch_mode, storage_mode, enabled, scan_interval_s, created_at, last_scan_at, needs_reconcile)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		w.ID, w.Path, w.Level, w.WatchMode, w.StorageMode, b2i(w.Enabled),
		w.ScanIntervalS, w.CreatedAt, nullable(w.LastScanAt), b2i(w.NeedsReconcile))
	return err
}

func (r *WikiRootsRepo) Get(id string) (*WikiRoot, error) {
	row := r.db.QueryRow(`SELECT id, path, level, watch_mode, storage_mode, enabled,
		scan_interval_s, created_at, COALESCE(last_scan_at, 0), needs_reconcile
		FROM wiki_roots WHERE id = ?`, id)
	w := &WikiRoot{}
	var enabled, needsReconcile int
	err := row.Scan(&w.ID, &w.Path, &w.Level, &w.WatchMode, &w.StorageMode, &enabled,
		&w.ScanIntervalS, &w.CreatedAt, &w.LastScanAt, &needsReconcile)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	w.Enabled = enabled == 1
	w.NeedsReconcile = needsReconcile == 1
	return w, nil
}

func (r *WikiRootsRepo) List() ([]WikiRoot, error) {
	rows, err := r.db.Query(`SELECT id, path, level, watch_mode, storage_mode, enabled,
		scan_interval_s, created_at, COALESCE(last_scan_at, 0), needs_reconcile FROM wiki_roots ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WikiRoot
	for rows.Next() {
		var w WikiRoot
		var enabled, needsReconcile int
		if err := rows.Scan(&w.ID, &w.Path, &w.Level, &w.WatchMode, &w.StorageMode, &enabled,
			&w.ScanIntervalS, &w.CreatedAt, &w.LastScanAt, &needsReconcile); err != nil {
			return nil, err
		}
		w.Enabled = enabled == 1
		w.NeedsReconcile = needsReconcile == 1
		out = append(out, w)
	}
	return out, rows.Err()
}

func (r *WikiRootsRepo) Delete(id string) error {
	_, err := r.db.Exec(`DELETE FROM wiki_roots WHERE id = ?`, id)
	return err
}

func (r *WikiRootsRepo) SetEnabled(id string, enabled bool) error {
	_, err := r.db.Exec(`UPDATE wiki_roots SET enabled = ? WHERE id = ?`, b2i(enabled), id)
	return err
}

func (r *WikiRootsRepo) UpdateLastScanAt(id string, at int64) error {
	_, err := r.db.Exec(`UPDATE wiki_roots SET last_scan_at = ? WHERE id = ?`, at, id)
	return err
}

func (r *WikiRootsRepo) SetNeedsReconcile(id string, v bool) error {
	_, err := r.db.Exec(`UPDATE wiki_roots SET needs_reconcile = ? WHERE id = ?`, b2i(v), id)
	return err
}

func (r *WikiRootsRepo) SetWatchMode(id, mode string) error {
	_, err := r.db.Exec(`UPDATE wiki_roots SET watch_mode = ? WHERE id = ?`, mode, id)
	return err
}
