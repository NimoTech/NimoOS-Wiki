package db

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpen_AppliesPragmasAndMigrations(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "wiki.db")
	d, err := Open(dbPath)
	require.NoError(t, err)
	defer d.Close()

	var journal string
	require.NoError(t, d.QueryRow("PRAGMA journal_mode").Scan(&journal))
	require.Equal(t, "wal", journal)

	// PRAGMA case_sensitive_like is write-only in SQLite (cannot be read back),
	// so we verify its effect via a behavioral probe: a literal LIKE must be
	// case-sensitive when the pragma is ON.
	var caseLikeProbe int
	require.NoError(t, d.QueryRow(`SELECT CASE WHEN 'A' LIKE 'a' THEN 1 ELSE 0 END`).Scan(&caseLikeProbe))
	require.Equal(t, 0, caseLikeProbe, "case_sensitive_like should make 'A' LIKE 'a' false")

	rows, err := d.Query("SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")
	require.NoError(t, err)
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		names = append(names, n)
	}
	require.Contains(t, names, "wiki_roots")
	require.Contains(t, names, "wiki_nodes")
	require.Contains(t, names, "file_index")
	require.Contains(t, names, "file_events")
	require.Contains(t, names, "parse_status")
}

// CRITICAL regression test: case_sensitive_like must affect LIKE queries.
// Without this PRAGMA, /DATA/ProjectA/% would match /DATA/projecta rows,
// causing data corruption during directory rename cascades.
func TestLikeIsCaseSensitive(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "wiki.db"))
	require.NoError(t, err)
	defer d.Close()

	_, err = d.Exec(`INSERT INTO file_index (id, root_id, path, parent, is_dir, status)
		VALUES ('1','r','/DATA/ProjectA/x.txt','/DATA/ProjectA',0,'present'),
		       ('2','r','/DATA/projecta/x.txt','/DATA/projecta',0,'present')`)
	require.NoError(t, err)

	row := d.QueryRow(`SELECT COUNT(*) FROM file_index WHERE path LIKE '/DATA/ProjectA/%'`)
	var n int
	require.NoError(t, row.Scan(&n))
	require.Equal(t, 1, n, "LIKE must be case-sensitive after PRAGMA case_sensitive_like=ON")
}

func TestOpen_Idempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "wiki.db")
	d1, err := Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, d1.Close())
	d2, err := Open(dbPath)
	require.NoError(t, err)
	defer d2.Close()
	// Should still work — migrations use IF NOT EXISTS
}
