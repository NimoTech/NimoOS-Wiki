package repo

import (
	"database/sql"
	"strings"
)

type FileEventsRepo struct{ db *sql.DB }

func NewFileEvents(d *sql.DB) *FileEventsRepo { return &FileEventsRepo{d} }

func (r *FileEventsRepo) Insert(e FileEvent) error {
	if e.ID == "" {
		e.ID = NewID()
	}
	_, err := r.db.Exec(`INSERT INTO file_events
		(id, root_id, path, op, rename_to, is_dir, detected_at, processed_at, archived)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		e.ID, e.RootID, e.Path, e.Op, nullableStr(e.RenameTo), b2i(e.IsDir),
		e.DetectedAt, nullable(e.ProcessedAt), b2i(e.Archived))
	return err
}

const evCols = `id, root_id, path, op, COALESCE(rename_to,''), is_dir, detected_at,
	COALESCE(processed_at, 0), archived`

func (r *FileEventsRepo) scan(row interface{ Scan(...interface{}) error }) (*FileEvent, error) {
	e := &FileEvent{}
	var isDir, archived int
	err := row.Scan(&e.ID, &e.RootID, &e.Path, &e.Op, &e.RenameTo, &isDir,
		&e.DetectedAt, &e.ProcessedAt, &archived)
	if err != nil {
		return nil, err
	}
	e.IsDir = isDir == 1
	e.Archived = archived == 1
	return e, nil
}

func (r *FileEventsRepo) ListUnprocessed(limit int) ([]FileEvent, error) {
	rows, err := r.db.Query(`SELECT `+evCols+` FROM file_events
		WHERE processed_at IS NULL ORDER BY detected_at LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileEvent
	for rows.Next() {
		e, err := r.scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

// ListSince returns file events newer than sinceMs. An empty rootID means
// "all roots" — used by NimoOS-Parser's WikiConsumer (a single global cursor
// across every root) and by /v1/wiki/recent-changes when callers want a
// global feed.
func (r *FileEventsRepo) ListSince(rootID string, sinceMs int64, limit int) ([]FileEvent, error) {
	var rows *sql.Rows
	var err error
	if rootID == "" {
		rows, err = r.db.Query(`SELECT `+evCols+` FROM file_events
			WHERE archived = 0 AND detected_at > ?
			ORDER BY detected_at LIMIT ?`, sinceMs, limit)
	} else {
		rows, err = r.db.Query(`SELECT `+evCols+` FROM file_events
			WHERE root_id = ? AND archived = 0 AND detected_at > ?
			ORDER BY detected_at LIMIT ?`, rootID, sinceMs, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileEvent
	for rows.Next() {
		e, err := r.scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

func (r *FileEventsRepo) RecentForRoot(rootID string, limit int) ([]FileEvent, error) {
	rows, err := r.db.Query(`SELECT `+evCols+` FROM file_events
		WHERE root_id = ? ORDER BY detected_at DESC LIMIT ?`, rootID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileEvent
	for rows.Next() {
		e, err := r.scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

func (r *FileEventsRepo) RecentForNode(rootID, nodePath string, limit int) ([]FileEvent, error) {
	pattern := EscapeLikeArg(nodePath) + `/%`
	rows, err := r.db.Query(`SELECT `+evCols+` FROM file_events
		WHERE root_id = ? AND (path = ? OR path LIKE ? ESCAPE '\')
		ORDER BY detected_at DESC LIMIT ?`, rootID, nodePath, pattern, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileEvent
	for rows.Next() {
		e, err := r.scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

func (r *FileEventsRepo) MarkProcessed(ids []string, atMs int64) error {
	if len(ids) == 0 {
		return nil
	}
	q := `UPDATE file_events SET processed_at = ? WHERE id IN (?` + strings.Repeat(",?", len(ids)-1) + `)`
	args := make([]interface{}, 0, len(ids)+1)
	args = append(args, atMs)
	for _, id := range ids {
		args = append(args, id)
	}
	_, err := r.db.Exec(q, args...)
	return err
}

func (r *FileEventsRepo) ArchiveOlderThan(cutoffMs int64) (int64, error) {
	res, err := r.db.Exec(`UPDATE file_events SET archived = 1
		WHERE archived = 0 AND detected_at < ?`, cutoffMs)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (r *FileEventsRepo) PurgeOlderThan(cutoffMs int64) (int64, error) {
	res, err := r.db.Exec(`DELETE FROM file_events WHERE detected_at < ?`, cutoffMs)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (r *FileEventsRepo) RewritePathPrefix(tx *sql.Tx, rootID, oldPrefix, newPrefix string) error {
	pattern := EscapeLikeArg(oldPrefix) + `/%`
	subStart := len(oldPrefix) + 1
	_, err := tx.Exec(`UPDATE file_events
		SET path = ? || SUBSTR(path, ?)
		WHERE root_id = ? AND processed_at IS NULL
		  AND (path = ? OR path LIKE ? ESCAPE '\')`,
		newPrefix, subStart, rootID, oldPrefix, pattern)
	return err
}

func (r *FileEventsRepo) CountUnprocessedByRoot() (map[string]int, error) {
	rows, err := r.db.Query(`SELECT root_id, COUNT(*) FROM file_events
		WHERE processed_at IS NULL GROUP BY root_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

func (r *FileEventsRepo) CountAll() (int64, error) {
	var n int64
	err := r.db.QueryRow(`SELECT COUNT(*) FROM file_events`).Scan(&n)
	return n, err
}

// PurgeOldestOverCap deletes the oldest rows so the table holds at most
// maxRows, and returns the distinct root_ids of the deleted rows so callers
// can mark those roots needs_reconcile (spec §4.2: Wiki cannot know the
// Parser consumer's cursor, so every capped purge is treated as potentially
// destroying unconsumed rows and self-heals via reconcile).
func (r *FileEventsRepo) PurgeOldestOverCap(maxRows int64) (int64, []string, error) {
	total, err := r.CountAll()
	if err != nil || total <= maxRows {
		return 0, nil, err
	}
	over := total - maxRows
	// cutoff = detected_at of the last row to delete (oldest `over` rows)
	var cutoff int64
	err = r.db.QueryRow(`SELECT detected_at FROM file_events
		ORDER BY detected_at LIMIT 1 OFFSET ?`, over-1).Scan(&cutoff)
	if err != nil {
		return 0, nil, err
	}
	rows, err := r.db.Query(`SELECT DISTINCT root_id FROM file_events
		WHERE detected_at <= ?`, cutoff)
	if err != nil {
		return 0, nil, err
	}
	var roots []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, nil, err
		}
		roots = append(roots, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	res, err := r.db.Exec(`DELETE FROM file_events WHERE detected_at <= ?`, cutoff)
	if err != nil {
		return 0, nil, err
	}
	n, _ := res.RowsAffected()
	return n, roots, nil
}

func nullableStr(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}
