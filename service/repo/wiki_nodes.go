package repo

import (
	"database/sql"
	"errors"
	"time"
)

type WikiNodesRepo struct{ db *sql.DB }

func NewWikiNodes(d *sql.DB) *WikiNodesRepo { return &WikiNodesRepo{d} }

func (r *WikiNodesRepo) Upsert(n WikiNode) error {
	if n.UpdatedAt == 0 {
		n.UpdatedAt = time.Now().UnixMilli()
	}
	_, err := r.db.Exec(`INSERT INTO wiki_nodes
		(id, root_id, path, level, child_count, last_modified, checksum_system,
		 user_notes, user_notes_etag, user_notes_updated_at, dirty,
		 last_flushed_at, last_flushed_mtime, updated_at, ai_label)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(path) DO UPDATE SET
			root_id=excluded.root_id,
			level=excluded.level,
			child_count=excluded.child_count,
			last_modified=excluded.last_modified,
			checksum_system=excluded.checksum_system,
			user_notes=excluded.user_notes,
			user_notes_etag=excluded.user_notes_etag,
			user_notes_updated_at=excluded.user_notes_updated_at,
			dirty=excluded.dirty,
			last_flushed_at=excluded.last_flushed_at,
			last_flushed_mtime=excluded.last_flushed_mtime,
			updated_at=excluded.updated_at,
			ai_label=excluded.ai_label`,
		n.ID, n.RootID, n.Path, n.Level, n.ChildCount, nullable(n.LastModified),
		n.ChecksumSystem, n.UserNotes, n.UserNotesETag, nullable(n.UserNotesUpdatedAt),
		b2i(n.Dirty), nullable(n.LastFlushedAt), nullable(n.LastFlushedMtime), n.UpdatedAt, n.AILabel)
	return err
}

func (r *WikiNodesRepo) scan(row interface{ Scan(...interface{}) error }) (*WikiNode, error) {
	n := &WikiNode{}
	var rootID sql.NullString
	var dirty int
	err := row.Scan(&n.ID, &rootID, &n.Path, &n.Level, &n.ChildCount,
		&n.LastModified, &n.ChecksumSystem, &n.UserNotes, &n.UserNotesETag,
		&n.UserNotesUpdatedAt, &dirty, &n.LastFlushedAt, &n.LastFlushedMtime, &n.UpdatedAt, &n.AILabel)
	if err != nil {
		return nil, err
	}
	if rootID.Valid {
		n.RootID = &rootID.String
	}
	n.Dirty = dirty == 1
	return n, nil
}

const nodeSelectCols = `id, root_id, path, level, child_count,
	COALESCE(last_modified,0), COALESCE(checksum_system,''),
	user_notes, user_notes_etag, COALESCE(user_notes_updated_at,0),
	dirty, COALESCE(last_flushed_at,0), COALESCE(last_flushed_mtime,0), updated_at,
	COALESCE(ai_label,'')`

func (r *WikiNodesRepo) Get(path string) (*WikiNode, error) {
	row := r.db.QueryRow(`SELECT `+nodeSelectCols+` FROM wiki_nodes WHERE path = ?`, path)
	n, err := r.scan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return n, err
}

func (r *WikiNodesRepo) GetByID(id string) (*WikiNode, error) {
	row := r.db.QueryRow(`SELECT `+nodeSelectCols+` FROM wiki_nodes WHERE id = ?`, id)
	n, err := r.scan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return n, err
}

func (r *WikiNodesRepo) List(rootID string) ([]WikiNode, error) {
	var rows *sql.Rows
	var err error
	if rootID == "" {
		rows, err = r.db.Query(`SELECT ` + nodeSelectCols + ` FROM wiki_nodes`)
	} else {
		rows, err = r.db.Query(`SELECT `+nodeSelectCols+` FROM wiki_nodes WHERE root_id = ?`, rootID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WikiNode
	for rows.Next() {
		n, err := r.scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *n)
	}
	return out, rows.Err()
}

func (r *WikiNodesRepo) ListDirty(limit int) ([]WikiNode, error) {
	rows, err := r.db.Query(`SELECT `+nodeSelectCols+`
		FROM wiki_nodes WHERE dirty = 1 ORDER BY updated_at LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WikiNode
	for rows.Next() {
		n, err := r.scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *n)
	}
	return out, rows.Err()
}

// ListSubwikis returns direct child wiki node paths (one level below parent).
func (r *WikiNodesRepo) ListSubwikis(parentPath string) ([]string, error) {
	prefix := parentPath
	if prefix == "/" {
		prefix = ""
	}
	pattern := EscapeLikeArg(prefix) + `/%`
	rows, err := r.db.Query(`SELECT path FROM wiki_nodes
		WHERE path LIKE ? ESCAPE '\' AND path != ?
		ORDER BY path`, pattern, parentPath)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		// keep only direct children (one extra path segment)
		rel := p[len(prefix)+1:]
		if !contains(rel, '/') {
			out = append(out, p)
		}
	}
	return out, rows.Err()
}

func contains(s string, c byte) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return true
		}
	}
	return false
}

func (r *WikiNodesRepo) SetDirty(path string, dirty bool) error {
	_, err := r.db.Exec(`UPDATE wiki_nodes SET dirty = ?, updated_at = ? WHERE path = ?`,
		b2i(dirty), time.Now().UnixMilli(), path)
	return err
}

func (r *WikiNodesRepo) SetUserNotes(path, notes, etag string, at int64) error {
	_, err := r.db.Exec(`UPDATE wiki_nodes
		SET user_notes = ?, user_notes_etag = ?, user_notes_updated_at = ?,
		    last_flushed_mtime = ?, updated_at = ?
		WHERE path = ?`, notes, etag, at, at, time.Now().UnixMilli(), path)
	return err
}

func (r *WikiNodesRepo) RecordFlush(path, checksum string, flushAt, mtime int64) error {
	_, err := r.db.Exec(`UPDATE wiki_nodes
		SET checksum_system = ?, last_flushed_at = ?, last_flushed_mtime = ?,
		    dirty = 0, updated_at = ?
		WHERE path = ?`, checksum, flushAt, mtime, time.Now().UnixMilli(), path)
	return err
}

func (r *WikiNodesRepo) Delete(path string) error {
	_, err := r.db.Exec(`DELETE FROM wiki_nodes WHERE path = ?`, path)
	return err
}

// RewritePathPrefix performs a case-sensitive cascade rename inside the provided tx.
// CRITICAL: Uses EscapeLikeArg + ESCAPE '\' to prevent _ / % in oldPrefix from
// matching unintended paths. Relies on PRAGMA case_sensitive_like=ON (set by Open).
// For path==oldPrefix: SUBSTR(oldPrefix, len+1) returns "" -> new path = newPrefix.
func (r *WikiNodesRepo) RewritePathPrefix(tx *sql.Tx, oldPrefix, newPrefix string) error {
	pattern := EscapeLikeArg(oldPrefix) + `/%`
	subStart := len(oldPrefix) + 1
	_, err := tx.Exec(`UPDATE wiki_nodes
		SET path = ? || SUBSTR(path, ?)
		WHERE path = ? OR path LIKE ? ESCAPE '\'`,
		newPrefix, subStart, oldPrefix, pattern)
	return err
}
