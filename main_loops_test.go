package main

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Wiki/pkg/db"
	"github.com/NimoTech/NimoOS-Wiki/pkg/ignore"
	"github.com/NimoTech/NimoOS-Wiki/service/repo"
	"github.com/NimoTech/NimoOS-Wiki/service/scanner"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func openMainTestDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestArchiveSweepEnforcesRowCap(t *testing.T) {
	d := openMainTestDB(t)
	ev := repo.NewFileEvents(d)
	roots := repo.NewWikiRoots(d)

	now := time.Now().UnixMilli()
	require.NoError(t, roots.Insert(repo.WikiRoot{
		ID: "rootA", Path: "/a", Level: "space", WatchMode: "auto",
		StorageMode: "inline", Enabled: true, ScanIntervalS: 600, CreatedAt: now,
	}))
	require.NoError(t, roots.Insert(repo.WikiRoot{
		ID: "rootB", Path: "/b", Level: "space", WatchMode: "auto",
		StorageMode: "inline", Enabled: true, ScanIntervalS: 600, CreatedAt: now,
	}))

	// 10 rows, alternating roots, all "recent" (near now) so the age-based
	// archive/purge cutoffs (90/180 days back) never touch them — only the
	// row cap should fire.
	for i := 0; i < 10; i++ {
		root := "rootA"
		if i%2 == 1 {
			root = "rootB"
		}
		require.NoError(t, ev.Insert(repo.FileEvent{
			ID: repo.NewID(), RootID: root, Path: fmt.Sprintf("/p/%d", i),
			Op: "create", DetectedAt: now + int64(i),
		}))
	}

	archiveSweep(ev, roots, 90, 6, zap.NewNop())

	total, err := ev.CountAll()
	require.NoError(t, err)
	require.LessOrEqual(t, total, int64(6))

	gA, err := roots.Get("rootA")
	require.NoError(t, err)
	require.True(t, gA.NeedsReconcile, "rootA should be marked needs_reconcile")

	gB, err := roots.Get("rootB")
	require.NoError(t, err)
	require.True(t, gB.NeedsReconcile, "rootB should be marked needs_reconcile")
}

func TestReconcileTickDrainsOneNeedsReconcilePerTick(t *testing.T) {
	d := openMainTestDB(t)
	files := repo.NewFileIndex(d)
	ev := repo.NewFileEvents(d)
	roots := repo.NewWikiRoots(d)
	rec := scanner.NewReconciler(files, ev, ignore.New(nil))

	now := time.Now().UnixMilli()
	type rootSpec struct {
		id      string
		backlog int
	}
	// Backlogs 30/20/10; ScanIntervalS is large and LastScanAt=now so the
	// regular interval-due pass never fires — only the needs_reconcile queue
	// should move.
	specs := []rootSpec{{"r30", 30}, {"r20", 20}, {"r10", 10}}
	for _, s := range specs {
		dir := t.TempDir()
		require.NoError(t, roots.Insert(repo.WikiRoot{
			ID: s.id, Path: dir, Level: "space", WatchMode: "auto",
			StorageMode: "inline", Enabled: true, ScanIntervalS: 3600,
			CreatedAt: now, LastScanAt: now, NeedsReconcile: true,
		}))
		for i := 0; i < s.backlog; i++ {
			require.NoError(t, ev.Insert(repo.FileEvent{
				ID: repo.NewID(), RootID: s.id, Path: fmt.Sprintf("%s/f%d", dir, i),
				Op: "create", DetectedAt: now,
			}))
		}
	}

	order := []string{"r30", "r20", "r10"} // largest backlog first, per tick
	cleared := map[string]bool{}
	for tick, want := range order {
		reconcileTick(context.Background(), roots, rec, nil, ev, zap.NewNop())
		cleared[want] = true
		for _, s := range specs {
			g, err := roots.Get(s.id)
			require.NoError(t, err)
			if cleared[s.id] {
				require.Falsef(t, g.NeedsReconcile, "tick %d: %s should be drained", tick+1, s.id)
				require.Greaterf(t, g.LastScanAt, now, "tick %d: %s last_scan should advance", tick+1, s.id)
			} else {
				require.Truef(t, g.NeedsReconcile, "tick %d: %s should still be queued", tick+1, s.id)
				require.Equalf(t, now, g.LastScanAt, "tick %d: %s last_scan should be untouched", tick+1, s.id)
			}
		}
	}
}

func TestReconcileTickSkipsStormingRoot(t *testing.T) {
	d := openMainTestDB(t)
	files := repo.NewFileIndex(d)
	ev := repo.NewFileEvents(d)
	roots := repo.NewWikiRoots(d)
	rec := scanner.NewReconciler(files, ev, ignore.New(nil))

	now := time.Now().UnixMilli()
	dir := t.TempDir()
	require.NoError(t, roots.Insert(repo.WikiRoot{
		ID: "storm", Path: dir, Level: "space", WatchMode: "auto",
		StorageMode: "inline", Enabled: true, ScanIntervalS: 3600,
		CreatedAt: now, LastScanAt: now, NeedsReconcile: true,
	}))
	require.NoError(t, ev.Insert(repo.FileEvent{
		ID: repo.NewID(), RootID: "storm", Path: dir + "/f0",
		Op: "create", DetectedAt: now,
	}))

	// perHigh=0 → any positive backlog forces per-root storm.
	guard := scanner.NewStormGuard(0, -1, 1000000, 0)
	guard.Update(map[string]int{"storm": 1})
	require.True(t, guard.IsStorming("storm"))

	reconcileTick(context.Background(), roots, rec, guard, ev, zap.NewNop())

	g, err := roots.Get("storm")
	require.NoError(t, err)
	require.True(t, g.NeedsReconcile, "storming root must not be drained")
	require.Equal(t, now, g.LastScanAt, "storming root's last_scan must be untouched")
}
