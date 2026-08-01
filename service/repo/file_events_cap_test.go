package repo

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCountUnprocessedByRoot(t *testing.T) {
	d := openTestDB(t)
	ev := NewFileEvents(d)
	for i := 0; i < 3; i++ {
		mustInsert(t, ev, "rootA", fmt.Sprintf("/a/%d", i))
	}
	mustInsert(t, ev, "rootB", "/b/0")
	// a processed row shouldn't be counted
	e := FileEvent{ID: NewID(), RootID: "rootA", Path: "/a/x", Op: "create", DetectedAt: 1}
	require.NoError(t, ev.Insert(e))
	require.NoError(t, ev.MarkProcessed([]string{e.ID}, 2))

	m, err := ev.CountUnprocessedByRoot()
	require.NoError(t, err)
	require.Equal(t, map[string]int{"rootA": 3, "rootB": 1}, m)

	total, err := ev.CountAll()
	require.NoError(t, err)
	require.EqualValues(t, 5, total) // CountAll counts the whole table (including processed)
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
