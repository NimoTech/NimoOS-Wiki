package repo

import (
	"fmt"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Wiki/pkg/db"
	"github.com/stretchr/testify/require"
)

func TestCountUnprocessedByRoot_Saturated(t *testing.T) {
	d := openTestDB(t)
	// db.Open no longer builds the backlog index (it is built after
	// startupSweep); the EXPLAIN assertion below needs it.
	require.NoError(t, db.EnsureBacklogIndex(d))
	ev := NewFileEvents(d)
	for i := 0; i < 30; i++ {
		mustInsert(t, ev, "rootA", fmt.Sprintf("/a/%d", i))
	}
	mustInsert(t, ev, "rootB", "/b/0")
	// a processed row shouldn't be counted
	e := FileEvent{ID: NewID(), RootID: "rootA", Path: "/a/x", Op: "create", DetectedAt: 1}
	require.NoError(t, ev.Insert(e))
	require.NoError(t, ev.MarkProcessed([]string{e.ID}, 2))

	m, err := ev.CountUnprocessedByRoot([]string{"rootA", "rootB", "rootC"}, 10)
	require.NoError(t, err)
	// rootA saturates at the limit, rootB is exact, rootC (no rows) is absent.
	require.Equal(t, map[string]int{"rootA": 10, "rootB": 1}, m)

	m, err = ev.CountUnprocessedByRoot([]string{"rootA"}, 1000)
	require.NoError(t, err)
	require.Equal(t, map[string]int{"rootA": 30}, m)

	// The per-root query must be index-backed: no temp b-tree, uses the backlog index.
	rows, err := d.Query(`EXPLAIN QUERY PLAN SELECT COUNT(*) FROM (SELECT 1 FROM file_events
		WHERE root_id = ? AND processed_at IS NULL LIMIT ?)`, "rootA", 10)
	require.NoError(t, err)
	defer rows.Close()
	var plan string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &notused, &detail))
		plan += detail + "\n"
	}
	require.NotContains(t, plan, "TEMP B-TREE")
	require.Contains(t, plan, "idx_file_events_backlog")
}

func TestPurgeOldestOverCap_RowidBatched(t *testing.T) {
	d := openTestDB(t)
	ev := NewFileEvents(d)
	// 120k rows > two 50k batches, so the loop must iterate.
	tx, err := d.Begin()
	require.NoError(t, err)
	stmt, err := tx.Prepare(`INSERT INTO file_events
		(id, root_id, path, op, is_dir, detected_at, archived) VALUES (?,?,?,?,0,?,0)`)
	require.NoError(t, err)
	for i := 1; i <= 120000; i++ {
		root := "rootA"
		if i%2 == 0 {
			root = "rootB"
		}
		_, err := stmt.Exec(fmt.Sprintf("id-%06d", i), root, fmt.Sprintf("/p/%d", i), "create", int64(i))
		require.NoError(t, err)
	}
	require.NoError(t, stmt.Close())
	require.NoError(t, tx.Commit())

	top, err := ev.MaxRowID()
	require.NoError(t, err)
	require.EqualValues(t, 120000, top)

	purged, err := ev.PurgeOldestOverCap(20000)
	require.NoError(t, err)
	require.EqualValues(t, 100000, purged)

	total, err := ev.CountAll()
	require.NoError(t, err)
	require.EqualValues(t, 20000, total)
	// Survivors are the NEWEST rows: smallest remaining rowid is 100001.
	var minRowid int64
	require.NoError(t, d.QueryRow(`SELECT MIN(rowid) FROM file_events`).Scan(&minRowid))
	require.EqualValues(t, 100001, minRowid)

	// no-op when under the cap
	purged, err = ev.PurgeOldestOverCap(100000)
	require.NoError(t, err)
	require.Zero(t, purged)

	// rowid upper bound (120000) > cap but actual rows (20000) <= cap after
	// the earlier purge: exact COUNT(*) makes this a no-op, no error.
	purged, err = ev.PurgeOldestOverCap(50000)
	require.NoError(t, err)
	require.Zero(t, purged)

	// The cutoff query must not sort: rowid order is the table's own b-tree.
	rows, err := d.Query(`EXPLAIN QUERY PLAN SELECT rowid FROM file_events ORDER BY rowid LIMIT 1 OFFSET ?`, 5)
	require.NoError(t, err)
	defer rows.Close()
	var plan string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &notused, &detail))
		plan += detail + "\n"
	}
	require.NotContains(t, plan, "TEMP B-TREE")
}

func TestPurgeAndArchiveOlderThan_Batched(t *testing.T) {
	d := openTestDB(t)
	ev := NewFileEvents(d)
	tx, err := d.Begin()
	require.NoError(t, err)
	stmt, err := tx.Prepare(`INSERT INTO file_events
		(id, root_id, path, op, is_dir, detected_at, archived) VALUES (?,?,?,?,0,?,0)`)
	require.NoError(t, err)
	for i := 1; i <= 60000; i++ {
		_, err := stmt.Exec(fmt.Sprintf("id-%06d", i), "rootA", fmt.Sprintf("/p/%d", i), "create", int64(i))
		require.NoError(t, err)
	}
	require.NoError(t, stmt.Close())
	require.NoError(t, tx.Commit())

	archived, err := ev.ArchiveOlderThan(55001) // rows 1..55000 → more than one 50k batch
	require.NoError(t, err)
	require.EqualValues(t, 55000, archived)
	var n int64
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM file_events WHERE archived = 1`).Scan(&n))
	require.EqualValues(t, 55000, n)

	purged, err := ev.PurgeOlderThan(55001)
	require.NoError(t, err)
	require.EqualValues(t, 55000, purged)
	total, err := ev.CountAll()
	require.NoError(t, err)
	require.EqualValues(t, 5000, total)
}

func TestCountAtMost(t *testing.T) {
	d := openTestDB(t)
	ev := NewFileEvents(d)

	n, err := ev.CountAtMost(10)
	require.NoError(t, err)
	require.Zero(t, n, "empty table")

	for i := 0; i < 30; i++ {
		mustInsert(t, ev, "rootA", fmt.Sprintf("/a/%d", i))
	}
	n, err = ev.CountAtMost(10)
	require.NoError(t, err)
	require.EqualValues(t, 10, n, "saturates at the limit")

	n, err = ev.CountAtMost(100)
	require.NoError(t, err)
	require.EqualValues(t, 30, n, "exact below the limit")
}

func mustInsert(t *testing.T, ev *FileEventsRepo, root, path string) {
	t.Helper()
	require.NoError(t, ev.Insert(FileEvent{
		ID: NewID(), RootID: root, Path: path, Op: "create",
		DetectedAt: time.Now().UnixMilli(),
	}))
}
