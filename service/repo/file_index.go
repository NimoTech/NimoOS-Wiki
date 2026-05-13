package repo

import (
	"database/sql"
	"errors"
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

func (r *FileIndexRepo) DeleteByPath(rootID, path string) error {
	_, err := r.db.Exec(`DELETE FROM file_index WHERE root_id = ? AND path = ?`, rootID, path)
	return err
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
