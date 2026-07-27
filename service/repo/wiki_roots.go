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

// SetNeedsAuthzPush 标记(或清除)单个 root 的授权推送待重试状态(授权源解耦
// Task 5 Critical 修复:与 SetNeedsReconcile 是两个独立字段,互不影响)。
func (r *WikiRootsRepo) SetNeedsAuthzPush(id string, v bool) error {
	_, err := r.db.Exec(`UPDATE wiki_roots SET needs_authz_push = ? WHERE id = ?`, b2i(v), id)
	return err
}

// HasNeedsAuthzPush 报告是否存在至少一行待重推的 root 授权记录,供重试循环
// 判断本轮 tick 是否需要触发一次全量 Reconcile(无标记时零成本跳过)。
func (r *WikiRootsRepo) HasNeedsAuthzPush() (bool, error) {
	var n int
	if err := r.db.QueryRow(`SELECT COUNT(1) FROM wiki_roots WHERE needs_authz_push = 1`).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// ClearAllNeedsAuthzPush 批量清除所有 needs_authz_push 标记,在重试循环里的
// 全量 Reconcile 成功后调用——一次性纠正当前全部待推行,而不是逐条清除。
func (r *WikiRootsRepo) ClearAllNeedsAuthzPush() error {
	_, err := r.db.Exec(`UPDATE wiki_roots SET needs_authz_push = 0 WHERE needs_authz_push = 1`)
	return err
}

func (r *WikiRootsRepo) SetWatchMode(id, mode string) error {
	_, err := r.db.Exec(`UPDATE wiki_roots SET watch_mode = ? WHERE id = ?`, mode, id)
	return err
}
