package repo

import (
	"fmt"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Wiki/pkg/db"
	"github.com/stretchr/testify/require"
)

func TestFileEvents_ArchiveAndPurge(t *testing.T) {
	d := openTestDB(t)
	r := NewFileEvents(d)
	now := time.Now().UnixMilli()
	day := int64(24 * 3600 * 1000)
	_ = r.Insert(FileEvent{RootID: "r", Path: "/old", Op: "create", DetectedAt: now - 100*day})
	_ = r.Insert(FileEvent{RootID: "r", Path: "/older", Op: "create", DetectedAt: now - 200*day})
	_ = r.Insert(FileEvent{RootID: "r", Path: "/fresh", Op: "create", DetectedAt: now})

	n, _ := r.ArchiveOlderThan(now - 90*day)
	require.Equal(t, int64(2), n, "two events archived")

	n2, _ := r.PurgeOlderThan(now - 180*day)
	require.Equal(t, int64(1), n2, "one event purged")
}

func TestInsertBatchAndPurgeByRootExceptDeletes(t *testing.T) {
	d := openTestDB(t)
	r := NewFileEvents(d)

	now := time.Now().UnixMilli()
	require.NoError(t, r.InsertBatch([]FileEvent{
		{RootID: "r1", Path: "/a", Op: "create", DetectedAt: now},
		{RootID: "r1", Path: "/b", Op: "modify", DetectedAt: now},
		{RootID: "r1", Path: "/c", Op: "delete", DetectedAt: now},
		{RootID: "r2", Path: "/d", Op: "create", DetectedAt: now},
	}))
	evs, err := r.ListSince("", 0, 100)
	require.NoError(t, err)
	require.Len(t, evs, 4)
	for _, e := range evs {
		require.NotEmpty(t, e.ID) // InsertBatch fills missing IDs
	}

	n, err := r.PurgeByRootExceptDeletes("r1")
	require.NoError(t, err)
	require.EqualValues(t, 2, n) // create+modify gone, delete kept, r2 untouched

	evs, _ = r.ListSince("", 0, 100)
	require.Len(t, evs, 2)
}

func TestFileEvents_RecentForNode_CaseSensitive(t *testing.T) {
	d := openTestDB(t)
	r := NewFileEvents(d)
	_ = r.Insert(FileEvent{RootID: "r", Path: "/DATA/ProjectA/x.go", Op: "create", DetectedAt: 1})
	_ = r.Insert(FileEvent{RootID: "r", Path: "/DATA/projecta/y.go", Op: "create", DetectedAt: 2})
	out, _ := r.RecentForNode("r", "/DATA/ProjectA", 10)
	require.Len(t, out, 1)
	require.Equal(t, "/DATA/ProjectA/x.go", out[0].Path)
}

func TestListSinceSeqPagesThroughSameMillisecond(t *testing.T) {
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	r := NewFileEvents(d)

	now := time.Now().UnixMilli()
	var batch []FileEvent
	for i := 0; i < 300; i++ { // 同一毫秒 300 条,单页 200 装不下
		batch = append(batch, FileEvent{
			RootID: "r", Path: fmt.Sprintf("/f%03d", i), Op: "create", DetectedAt: now,
		})
	}
	require.NoError(t, r.InsertBatch(batch))

	seen := map[string]bool{}
	sinceMs, afterSeq := int64(0), int64(0)
	for {
		page, err := r.ListSinceSeq("", sinceMs, afterSeq, 200)
		require.NoError(t, err)
		if len(page) == 0 {
			break
		}
		for _, e := range page {
			require.False(t, seen[e.Path], "duplicate %s", e.Path)
			seen[e.Path] = true
			require.NotZero(t, e.Seq)
		}
		last := page[len(page)-1]
		sinceMs, afterSeq = last.DetectedAt, last.Seq
	}
	require.Len(t, seen, 300, "cursor must not skip same-millisecond events")
}
