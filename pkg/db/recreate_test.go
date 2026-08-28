package db

import (
	"database/sql"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

func schemaOf(t *testing.T, d *sql.DB) []string {
	t.Helper()
	rows, err := d.Query(`SELECT sql FROM sqlite_master WHERE tbl_name = 'file_events' AND sql IS NOT NULL`)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func TestRecreateFileEvents_EmptyTableSameSchema(t *testing.T) {
	d, err := Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	before := schemaOf(t, d)
	require.NotEmpty(t, before)
	_, err = d.Exec(`INSERT INTO file_events (id, root_id, path, op, detected_at) VALUES ('x','r','/p','create',1)`)
	require.NoError(t, err)

	require.NoError(t, RecreateFileEvents(d))

	var n int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM file_events`).Scan(&n))
	require.Zero(t, n)
	require.Equal(t, before, schemaOf(t, d), "recreated schema must match first-boot schema")
	// Re-running migrations afterwards must be a no-op (IF NOT EXISTS everywhere).
	require.NoError(t, runMigrations(d))
	require.Equal(t, before, schemaOf(t, d))
}

func TestOpen_TempStoreNotMemory(t *testing.T) {
	d, err := Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	var v int
	require.NoError(t, d.QueryRow(`PRAGMA temp_store`).Scan(&v))
	require.NotEqual(t, 2, v, "temp_store=MEMORY (2) puts sort/group temp b-trees on the heap")
}
