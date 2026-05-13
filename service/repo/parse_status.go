package repo

import "database/sql"

type ParseStatusRepo struct{ db *sql.DB }

func NewParseStatus(d *sql.DB) *ParseStatusRepo { return &ParseStatusRepo{d} }

func (r *ParseStatusRepo) InsertPending(path string) error {
	_, err := r.db.Exec(`INSERT OR IGNORE INTO parse_status (id, path, status)
		VALUES (?, ?, 'pending')`, NewID(), path)
	return err
}

func (r *ParseStatusRepo) SetStatus(path, status, parserVersion, errMsg string, atMs int64) error {
	_, err := r.db.Exec(`UPDATE parse_status
		SET status = ?, indexed_at = ?, parser_version = ?, error = ?
		WHERE path = ?`, status, nullable(atMs), parserVersion, errMsg, path)
	return err
}

// CountByPrefix returns the count of rows with the given status whose path starts with rootPrefix.
func (r *ParseStatusRepo) CountByPrefix(rootPrefix string, status string) (int, error) {
	pattern := EscapeLikeArg(rootPrefix) + `/%`
	row := r.db.QueryRow(`SELECT COUNT(*) FROM parse_status
		WHERE status = ? AND (path = ? OR path LIKE ? ESCAPE '\')`,
		status, rootPrefix, pattern)
	var n int
	return n, row.Scan(&n)
}
