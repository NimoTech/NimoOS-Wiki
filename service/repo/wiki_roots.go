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
		(id, path, level, watch_mode, storage_mode, enabled, scan_interval_s, created_at, last_scan_at, needs_reconcile, needs_authz_push)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		w.ID, w.Path, w.Level, w.WatchMode, w.StorageMode, b2i(w.Enabled),
		w.ScanIntervalS, w.CreatedAt, nullable(w.LastScanAt), b2i(w.NeedsReconcile), b2i(w.NeedsAuthzPush))
	return err
}

func (r *WikiRootsRepo) Get(id string) (*WikiRoot, error) {
	row := r.db.QueryRow(`SELECT id, path, level, watch_mode, storage_mode, enabled,
		scan_interval_s, created_at, COALESCE(last_scan_at, 0), needs_reconcile, needs_authz_push
		FROM wiki_roots WHERE id = ?`, id)
	w := &WikiRoot{}
	var enabled, needsReconcile, needsAuthzPush int
	err := row.Scan(&w.ID, &w.Path, &w.Level, &w.WatchMode, &w.StorageMode, &enabled,
		&w.ScanIntervalS, &w.CreatedAt, &w.LastScanAt, &needsReconcile, &needsAuthzPush)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	w.Enabled = enabled == 1
	w.NeedsReconcile = needsReconcile == 1
	w.NeedsAuthzPush = needsAuthzPush == 1
	return w, nil
}

func (r *WikiRootsRepo) List() ([]WikiRoot, error) {
	rows, err := r.db.Query(`SELECT id, path, level, watch_mode, storage_mode, enabled,
		scan_interval_s, created_at, COALESCE(last_scan_at, 0), needs_reconcile, needs_authz_push FROM wiki_roots ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WikiRoot
	for rows.Next() {
		var w WikiRoot
		var enabled, needsReconcile, needsAuthzPush int
		if err := rows.Scan(&w.ID, &w.Path, &w.Level, &w.WatchMode, &w.StorageMode, &enabled,
			&w.ScanIntervalS, &w.CreatedAt, &w.LastScanAt, &needsReconcile, &needsAuthzPush); err != nil {
			return nil, err
		}
		w.Enabled = enabled == 1
		w.NeedsReconcile = needsReconcile == 1
		w.NeedsAuthzPush = needsAuthzPush == 1
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

// SetNeedsAuthzPush marks (or clears) a single root's pending-authz-push-retry
// state (authz-source-decoupling Task 5 critical fix: an independent field
// from SetNeedsReconcile, the two don't affect each other).
func (r *WikiRootsRepo) SetNeedsAuthzPush(id string, v bool) error {
	_, err := r.db.Exec(`UPDATE wiki_roots SET needs_authz_push = ? WHERE id = ?`, b2i(v), id)
	return err
}

// HasNeedsAuthzPush reports whether at least one root's authz grant is
// pending repush, for the retry loop to decide whether this tick needs to
// trigger a full Reconcile (zero-cost skip when nothing is marked).
func (r *WikiRootsRepo) HasNeedsAuthzPush() (bool, error) {
	var n int
	if err := r.db.QueryRow(`SELECT COUNT(1) FROM wiki_roots WHERE needs_authz_push = 1`).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// ClearAllNeedsAuthzPush bulk-clears every needs_authz_push marker; called
// after a successful full Reconcile in the retry loop — corrects all
// currently pending rows at once, instead of clearing them one by one.
func (r *WikiRootsRepo) ClearAllNeedsAuthzPush() error {
	_, err := r.db.Exec(`UPDATE wiki_roots SET needs_authz_push = 0 WHERE needs_authz_push = 1`)
	return err
}

func (r *WikiRootsRepo) SetWatchMode(id, mode string) error {
	_, err := r.db.Exec(`UPDATE wiki_roots SET watch_mode = ? WHERE id = ?`, mode, id)
	return err
}
