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

	// First boot builds the backlog index only after startupSweep, so a
	// first-boot-WITH-index schema is what the recreated schema must match.
	require.NoError(t, EnsureBacklogIndex(d))
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
	// RecreateFileEvents already built the backlog index, so ensuring it again
	// must be a no-op too.
	require.NoError(t, EnsureBacklogIndex(d))
	require.Equal(t, before, schemaOf(t, d))
}

func TestOpen_DoesNotBuildBacklogIndex(t *testing.T) {
	d, err := Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	count := func() int {
		var n int
		require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM sqlite_master
			WHERE type='index' AND name='idx_file_events_backlog'`).Scan(&n))
		return n
	}
	// The build costs minutes and GBs of WAL on a bloated table: migrations
	// must not do it before startupSweep has had its chance.
	require.Zero(t, count(), "db.Open must not build the backlog index")
	require.NoError(t, EnsureBacklogIndex(d))
	require.Equal(t, 1, count())
}

func TestRecreateFileEvents_MarksEnabledRootsNeedsReconcile(t *testing.T) {
	d, err := Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	ins := `INSERT INTO wiki_roots (id, path, level, watch_mode, storage_mode,
		enabled, scan_interval_s, created_at) VALUES (?,?,'space','auto','inline',?,600,1)`
	_, err = d.Exec(ins, "on1", "/a", 1)
	require.NoError(t, err)
	_, err = d.Exec(ins, "on2", "/b", 1)
	require.NoError(t, err)
	_, err = d.Exec(ins, "off", "/c", 0)
	require.NoError(t, err)

	require.NoError(t, RecreateFileEvents(d))

	needs := func(id string) int {
		var n int
		require.NoError(t, d.QueryRow(`SELECT needs_reconcile FROM wiki_roots WHERE id = ?`, id).Scan(&n))
		return n
	}
	// The mark rides in the DROP's transaction: a crash in between must not
	// lose the reconcile signal for the rows just destroyed.
	require.Equal(t, 1, needs("on1"))
	require.Equal(t, 1, needs("on2"))
	require.Zero(t, needs("off"), "disabled roots are not reconciled")
}

func TestOpen_TempStoreNotMemory(t *testing.T) {
	d, err := Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	var v int
	require.NoError(t, d.QueryRow(`PRAGMA temp_store`).Scan(&v))
	require.NotEqual(t, 2, v, "temp_store=MEMORY (2) puts sort/group temp b-trees on the heap")
}
