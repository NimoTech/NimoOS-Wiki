package repo

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

type FileIndexRepo struct{ db *sql.DB }

func NewFileIndex(d *sql.DB) *FileIndexRepo { return &FileIndexRepo{d} }

func (r *FileIndexRepo) Upsert(f FileIndex) error {
	_, err := r.db.Exec(`INSERT INTO file_index
		(id, root_id, path, parent, is_dir, is_opaque, mtime, size, inode, status, ext)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(root_id, path) DO UPDATE SET
			parent=excluded.parent, is_dir=excluded.is_dir, is_opaque=excluded.is_opaque,
			mtime=excluded.mtime, size=excluded.size, inode=excluded.inode,
			status=excluded.status, ext=excluded.ext`,
		f.ID, f.RootID, f.Path, f.Parent, b2i(f.IsDir), b2i(f.IsOpaque),
		nullable(f.Mtime), nullable(f.Size), nullable(f.Inode), f.Status, f.Ext)
	return err
}

const fileIndexCols = `id, root_id, path, parent, is_dir, is_opaque,
	COALESCE(mtime,0), COALESCE(size,0), COALESCE(inode,0), status, COALESCE(ext,'')`

func (r *FileIndexRepo) scan(row interface{ Scan(...interface{}) error }) (*FileIndex, error) {
	f := &FileIndex{}
	var isDir, isOpaque int
	err := row.Scan(&f.ID, &f.RootID, &f.Path, &f.Parent, &isDir, &isOpaque,
		&f.Mtime, &f.Size, &f.Inode, &f.Status, &f.Ext)
	if err != nil {
		return nil, err
	}
	f.IsDir = isDir == 1
	f.IsOpaque = isOpaque == 1
	return f, nil
}

func (r *FileIndexRepo) Get(rootID, path string) (*FileIndex, error) {
	row := r.db.QueryRow(`SELECT `+fileIndexCols+` FROM file_index WHERE root_id = ? AND path = ?`, rootID, path)
	f, err := r.scan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return f, err
}

func (r *FileIndexRepo) ListByParent(rootID, parent string) ([]FileIndex, error) {
	rows, err := r.db.Query(`SELECT `+fileIndexCols+` FROM file_index
		WHERE root_id = ? AND parent = ? ORDER BY path`, rootID, parent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileIndex
	for rows.Next() {
		f, err := r.scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *f)
	}
	return out, rows.Err()
}

func (r *FileIndexRepo) ListAllByRoot(rootID string) ([]FileIndex, error) {
	rows, err := r.db.Query(`SELECT `+fileIndexCols+` FROM file_index
		WHERE root_id = ? AND status = 'present'`, rootID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileIndex
	for rows.Next() {
		f, err := r.scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *f)
	}
	return out, rows.Err()
}

// ListByRootAfter returns up to limit rows with path > afterPath, ordered by
// path — keyset pagination for the streaming reconciler (spec §4.6).
func (r *FileIndexRepo) ListByRootAfter(rootID, afterPath string, limit int) ([]FileIndex, error) {
	rows, err := r.db.Query(`SELECT `+fileIndexCols+` FROM file_index
		WHERE root_id = ? AND path > ? ORDER BY path LIMIT ?`,
		rootID, afterPath, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileIndex
	for rows.Next() {
		f, err := r.scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *f)
	}
	return out, rows.Err()
}

func (r *FileIndexRepo) DeleteByPath(rootID, path string) error {
	_, err := r.db.Exec(`DELETE FROM file_index WHERE root_id = ? AND path = ?`, rootID, path)
	return err
}

var evidenceTextExts = []string{
	"md", "txt", "json", "csv", "yaml", "yml", "toml", "ini",
	"go", "py", "ts", "tsx", "js", "rs", "java", "c", "h", "cpp", "sh", "sql",
}

// ListEvidenceTextFiles returns up to `limit` text-class files under
// nodePath (inclusive of subtree), each with size ≤ maxBytes, sorted by
// most-recent mtime first. Opaque container subtrees are not indexed in
// file_index so they're naturally excluded.
func (r *FileIndexRepo) ListEvidenceTextFiles(rootID, nodePath string, limit int, maxBytes int64) ([]FileIndex, error) {
	pattern := EscapeLikeArg(nodePath) + `/%`
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(evidenceTextExts)), ",")
	args := []any{rootID, nodePath, pattern, maxBytes}
	for _, e := range evidenceTextExts {
		args = append(args, e)
	}
	args = append(args, limit)

	q := fmt.Sprintf(`SELECT `+fileIndexCols+` FROM file_index
		WHERE root_id = ?
		  AND (path = ? OR path LIKE ? ESCAPE '\')
		  AND is_dir = 0
		  AND is_opaque = 0
		  AND status = 'present'
		  AND COALESCE(size, 0) <= ?
		  AND ext IN (%s)
		ORDER BY COALESCE(mtime, 0) DESC
		LIMIT ?`, placeholders)

	rows, err := r.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileIndex
	for rows.Next() {
		f, err := r.scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *f)
	}
	return out, rows.Err()
}

func (r *FileIndexRepo) ListEvidencePDFs(rootID, nodePath string, limit int, maxBytes int64) ([]FileIndex, error) {
	pattern := EscapeLikeArg(nodePath) + `/%`
	rows, err := r.db.Query(`SELECT `+fileIndexCols+` FROM file_index
		WHERE root_id = ?
		  AND (path = ? OR path LIKE ? ESCAPE '\')
		  AND is_dir = 0
		  AND is_opaque = 0
		  AND status = 'present'
		  AND COALESCE(size, 0) <= ?
		  AND ext = 'pdf'
		ORDER BY COALESCE(mtime, 0) DESC
		LIMIT ?`, rootID, nodePath, pattern, maxBytes, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileIndex
	for rows.Next() {
		f, err := r.scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *f)
	}
	return out, rows.Err()
}

// ListEvidenceChildren returns direct children of nodePath ordered by path.
func (r *FileIndexRepo) ListEvidenceChildren(rootID, nodePath string, limit int) ([]FileIndex, error) {
	rows, err := r.db.Query(`SELECT `+fileIndexCols+` FROM file_index
		WHERE root_id = ?
		  AND parent = ?
		  AND status = 'present'
		ORDER BY path
		LIMIT ?`, rootID, nodePath, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileIndex
	for rows.Next() {
		f, err := r.scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *f)
	}
	return out, rows.Err()
}

// ListEvidenceSkippedSample returns up to `limit` binary files (images,
// videos, archives) under nodePath, so the LLM can see "what's here that I
// didn't read".
func (r *FileIndexRepo) ListEvidenceSkippedSample(rootID, nodePath string, limit int) ([]FileIndex, error) {
	pattern := EscapeLikeArg(nodePath) + `/%`
	rows, err := r.db.Query(`SELECT `+fileIndexCols+` FROM file_index
		WHERE root_id = ?
		  AND (path = ? OR path LIKE ? ESCAPE '\')
		  AND is_dir = 0
		  AND is_opaque = 0
		  AND status = 'present'
		  AND ext IN ('jpeg','jpg','png','heic','mov','mp4','zip','7z','tar','gz','rar','dmg','iso')
		ORDER BY COALESCE(mtime, 0) DESC
		LIMIT ?`, rootID, nodePath, pattern, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileIndex
	for rows.Next() {
		f, err := r.scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *f)
	}
	return out, rows.Err()
}

func (r *FileIndexRepo) RewritePathPrefix(tx *sql.Tx, rootID, oldPrefix, newPrefix string) error {
	pattern := EscapeLikeArg(oldPrefix) + `/%`
	subStart := len(oldPrefix) + 1
	// Path rewrite
	if _, err := tx.Exec(`UPDATE file_index
		SET path = ? || SUBSTR(path, ?)
		WHERE root_id = ? AND (path = ? OR path LIKE ? ESCAPE '\')`,
		newPrefix, subStart, rootID, oldPrefix, pattern); err != nil {
		return err
	}
	// Parent rewrite
	_, err := tx.Exec(`UPDATE file_index
		SET parent = ? || SUBSTR(parent, ?)
		WHERE root_id = ? AND (parent = ? OR parent LIKE ? ESCAPE '\')`,
		newPrefix, subStart, rootID, oldPrefix, pattern)
	return err
}
