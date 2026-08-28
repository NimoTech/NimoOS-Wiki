package repo

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCountUnprocessedByRoot_Saturated(t *testing.T) {
	d := openTestDB(t)
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

func TestPurgeOldestOverCap(t *testing.T) {
	d := openTestDB(t)
	ev := NewFileEvents(d)
	// 10 rows, detected_at 1..10, alternating root
	for i := 1; i <= 10; i++ {
		root := "rootA"
		if i%2 == 0 {
			root = "rootB"
		}
		require.NoError(t, ev.Insert(FileEvent{
			ID: NewID(), RootID: root, Path: fmt.Sprintf("/p/%d", i),
			Op: "create", DetectedAt: int64(i),
		}))
	}
	purged, roots, err := ev.PurgeOldestOverCap(6)
	require.NoError(t, err)
	require.EqualValues(t, 4, purged) // deletes the 4 oldest rows (detected_at 1..4)
	require.ElementsMatch(t, []string{"rootA", "rootB"}, roots)
	total, _ := ev.CountAll()
	require.EqualValues(t, 6, total)
	// no-op when under the cap
	purged, roots, err = ev.PurgeOldestOverCap(100)
	require.NoError(t, err)
	require.Zero(t, purged)
	require.Empty(t, roots)
}

func mustInsert(t *testing.T, ev *FileEventsRepo, root, path string) {
	t.Helper()
	require.NoError(t, ev.Insert(FileEvent{
		ID: NewID(), RootID: root, Path: path, Op: "create",
		DetectedAt: time.Now().UnixMilli(),
	}))
}
