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

// ListSinceSeq is keyset pagination over (detected_at, rowid): strictly after
// the (sinceMs, afterSeq) cursor. Fixes the lost-events bug where a burst of
// same-millisecond events (one reconciler round shares a single `now`) larger
// than one page was skipped forever by consumers advancing a detected_at-only
// cursor. ListSince keeps the legacy semantics for callers that don't send a
// seq (recent-changes, old Parsers).
func (r *FileEventsRepo) ListSinceSeq(rootID string, sinceMs, afterSeq int64, limit int) ([]FileEvent, error) {
	q := `SELECT rowid, ` + evCols + ` FROM file_events
		WHERE archived = 0 AND (detected_at > ? OR (detected_at = ? AND rowid > ?))`
	args := []any{sinceMs, sinceMs, afterSeq}
	if rootID != "" {
		q += ` AND root_id = ?`
		args = append(args, rootID)
	}
	q += ` ORDER BY detected_at, rowid LIMIT ?`
	args = append(args, limit)

	rows, err := r.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileEvent
	for rows.Next() {
		e := &FileEvent{}
		var isDir, archived int
		if err := rows.Scan(&e.Seq, &e.ID, &e.RootID, &e.Path, &e.Op, &e.RenameTo, &isDir,
			&e.DetectedAt, &e.ProcessedAt, &archived); err != nil {
			return nil, err
		}
		e.IsDir = isDir == 1
		e.Archived = archived == 1
		out = append(out, *e)
	}
	return out, rows.Err()
}

// PurgeByRootExceptDeletes drops a root's create/modify/rename events — moot
// once the root is being deleted. op='delete' rows are kept: the Parser's
// global cursor may not have consumed them yet and they are its only signal
// to drop paths that vanished before the root did.
func (r *FileEventsRepo) PurgeByRootExceptDeletes(rootID string) (int64, error) {
	res, err := r.db.Exec(`DELETE FROM file_events WHERE root_id = ? AND op != 'delete'`, rootID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// InsertBatch inserts events in a single transaction (prepared statement) —
// used by root-deletion cascade which may emit tens of thousands of
// tombstones; row-at-a-time inserts would fsync per event. Fills empty IDs.
func (r *FileEventsRepo) InsertBatch(events []FileEvent) error {
	if len(events) == 0 {
		return nil
	}
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO file_events
		(id, root_id, path, op, rename_to, is_dir, detected_at, processed_at, archived)
		VALUES (?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	for i := range events {
		e := &events[i]
		if e.ID == "" {
			e.ID = NewID()
		}
		if _, err := stmt.Exec(e.ID, e.RootID, e.Path, e.Op, nullableStr(e.RenameTo),
			b2i(e.IsDir), e.DetectedAt, nullable(e.ProcessedAt), b2i(e.Archived)); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			return err
		}
	}
	_ = stmt.Close()
	return tx.Commit()
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

// purgeBatch bounds every maintenance UPDATE/DELETE to one statement-sized
// transaction so a bloated table is trimmed without a minutes-long write lock
// or an unbounded WAL (spec §3.2).
const purgeBatch = 50000

// MaxRowID is an O(1) upper bound on the row count (rowid is monotonic; gaps
// from earlier deletes only make the bound conservative).
func (r *FileEventsRepo) MaxRowID() (int64, error) {
	var n sql.NullInt64
	if err := r.db.QueryRow(`SELECT MAX(rowid) FROM file_events`).Scan(&n); err != nil {
		return 0, err
	}
	return n.Int64, nil
}

// execBatches runs stmt — which must end with `LIMIT ?)` — repeatedly with
// purgeBatch appended to args until a batch affects fewer than purgeBatch
// rows. Returns the total rows affected.
func (r *FileEventsRepo) execBatches(stmt string, args ...interface{}) (int64, error) {
	var total int64
	for {
		full := append(append([]interface{}{}, args...), purgeBatch)
		res, err := r.db.Exec(stmt, full...)
		if err != nil {
			return total, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		total += n
		if n < purgeBatch {
			return total, nil
		}
	}
}

func (r *FileEventsRepo) ArchiveOlderThan(cutoffMs int64) (int64, error) {
	return r.execBatches(`UPDATE file_events SET archived = 1 WHERE rowid IN (
		SELECT rowid FROM file_events WHERE archived = 0 AND detected_at < ? LIMIT ?)`, cutoffMs)
}

func (r *FileEventsRepo) PurgeOlderThan(cutoffMs int64) (int64, error) {
	return r.execBatches(`DELETE FROM file_events WHERE rowid IN (
		SELECT rowid FROM file_events WHERE detected_at < ? LIMIT ?)`, cutoffMs)
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

// CountUnprocessedByRoot returns a SATURATED per-root backlog: for each id in
// rootIDs, the number of unprocessed rows capped at limitPerRoot. Callers
// (storm fuse, reconcile picker) only need "over threshold?" and "which is
// largest?", so a bounded count is enough — and it keeps the cost O(limit)
// instead of O(table) on the once-per-second hot path (spec §3.1). Roots with
// zero backlog are absent from the map (same contract as the old GROUP BY).
func (r *FileEventsRepo) CountUnprocessedByRoot(rootIDs []string, limitPerRoot int) (map[string]int, error) {
	if limitPerRoot <= 0 {
		limitPerRoot = 1
	}
	out := make(map[string]int, len(rootIDs))
	for _, id := range rootIDs {
		var n int
		err := r.db.QueryRow(`SELECT COUNT(*) FROM (SELECT 1 FROM file_events
			WHERE root_id = ? AND processed_at IS NULL LIMIT ?)`, id, limitPerRoot).Scan(&n)
		if err != nil {
			return nil, err
		}
		if n > 0 {
			out[id] = n
		}
	}
	return out, nil
}

// CountAtMost returns min(rows, limit) — a saturated row count that stops after
// `limit` rows instead of walking the whole table. Used by startupSweep to decide
// "bloated or not" without an O(n) COUNT(*) on a 150M-row table.
func (r *FileEventsRepo) CountAtMost(limit int64) (int64, error) {
	var n int64
	err := r.db.QueryRow(`SELECT COUNT(*) FROM (SELECT 1 FROM file_events LIMIT ?)`, limit).Scan(&n)
	return n, err
}

func (r *FileEventsRepo) CountAll() (int64, error) {
	var n int64
	err := r.db.QueryRow(`SELECT COUNT(*) FROM file_events`).Scan(&n)
	return n, err
}

// PurgeOldestOverCap deletes the oldest rows so the table holds at most
// maxRows. "Oldest" is by rowid (monotonic ≈ detected_at) so the cutoff is
// found by walking the table b-tree — no ORDER BY sort, no temp store. The
// row count must be EXACT (CountAll): MaxRowID is only an upper bound and
// using it here deletes the whole table when bound-cap equals the real row
// count. A NULL cutoff is kept as a defensive no-op.
// Callers must treat any purge as potentially destroying unconsumed rows and
// mark roots needs_reconcile (spec §3.2).
func (r *FileEventsRepo) PurgeOldestOverCap(maxRows int64) (int64, error) {
	total, err := r.CountAll() // exact: COUNT(*) is an index b-tree walk, O(1) memory
	if err != nil || total <= maxRows {
		return 0, err
	}
	var cutoff sql.NullInt64
	err = r.db.QueryRow(`SELECT rowid FROM file_events ORDER BY rowid LIMIT 1 OFFSET ?`,
		total-maxRows-1).Scan(&cutoff)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !cutoff.Valid {
		return 0, nil
	}
	return r.execBatches(`DELETE FROM file_events WHERE rowid IN (
		SELECT rowid FROM file_events WHERE rowid <= ? LIMIT ?)`, cutoff.Int64)
}

func nullableStr(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}
