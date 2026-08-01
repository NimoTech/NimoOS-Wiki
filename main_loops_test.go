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
	"github.com/NimoTech/NimoOS-Wiki/service/roots"
	"github.com/NimoTech/NimoOS-Wiki/service/rootsync"
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

func TestRootDueAtBoot(t *testing.T) {
	now := time.Now().UnixMilli()
	cases := []struct {
		name string
		root repo.WikiRoot
		want bool
	}{
		{"never scanned", repo.WikiRoot{Enabled: true, ScanIntervalS: 600}, true},
		{"fresh", repo.WikiRoot{Enabled: true, ScanIntervalS: 600, LastScanAt: now - 1000}, false},
		{"overdue", repo.WikiRoot{Enabled: true, ScanIntervalS: 600, LastScanAt: now - 700_000}, true},
		{"disabled overdue", repo.WikiRoot{Enabled: false, ScanIntervalS: 600, LastScanAt: now - 700_000}, false},
		{"needs_reconcile is tick's job", repo.WikiRoot{Enabled: true, ScanIntervalS: 600, NeedsReconcile: true}, false},
	}
	for _, c := range cases {
		require.Equal(t, c.want, rootDueAtBoot(c.root, now), c.name)
	}
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

// --- authzPushRetryTick coverage (critical fix, option B: dedicated retry
// field + dedicated retry loop). Built the same way as the reconcileTick
// tests above: in-memory sqlite + fake dependencies.

// fakeAuthzReconciler records the args of every Reconcile call (number of
// grants), so tests can assert whether/when the retry loop fires;
// errToReturn controls the success/failure branch.
type fakeAuthzReconciler struct {
	calls       []int // grants length on each call
	errToReturn error
}

func (f *fakeAuthzReconciler) Reconcile(_ context.Context, grants []rootsync.Grant) error {
	f.calls = append(f.calls, len(grants))
	return f.errToReturn
}

// failingPusher is a test double for roots.Manager's pusher dependency, where
// both Upsert/Delete fail, used to drive the manager into raising
// needs_authz_push / authzDirty signals.
type failingPusher struct{ err error }

func (p failingPusher) Upsert(context.Context, rootsync.Grant) error { return p.err }
func (p failingPusher) Delete(context.Context, string) error         { return p.err }

func TestAuthzPushRetryTick_NoOpWhenNothingPending(t *testing.T) {
	d := openMainTestDB(t)
	rRoots := repo.NewWikiRoots(d)
	mgr := roots.NewManager(rRoots, repo.NewWikiNodes(d), repo.NewFileIndex(d), repo.NewFileEvents(d), nil, nil)

	fr := &fakeAuthzReconciler{}
	authzPushRetryTick(context.Background(), rRoots, fr, mgr, zap.NewNop())

	require.Empty(t, fr.calls, "Reconcile should not fire when nothing is pending")
}

func TestAuthzPushRetryTick_DBFlagTriggersReconcileAndClearsOnSuccess(t *testing.T) {
	d := openMainTestDB(t)
	rRoots := repo.NewWikiRoots(d)
	mgr := roots.NewManager(rRoots, repo.NewWikiNodes(d), repo.NewFileIndex(d), repo.NewFileEvents(d), nil, nil)

	now := time.Now().UnixMilli()
	require.NoError(t, rRoots.Insert(repo.WikiRoot{
		ID: "r1", Path: "/a", Level: "space", WatchMode: "auto",
		StorageMode: "inline", Enabled: true, ScanIntervalS: 600, CreatedAt: now,
	}))
	require.NoError(t, rRoots.SetNeedsAuthzPush("r1", true))

	fr := &fakeAuthzReconciler{}
	authzPushRetryTick(context.Background(), rRoots, fr, mgr, zap.NewNop())

	require.Len(t, fr.calls, 1, "a needs_authz_push row should trigger one full Reconcile")
	require.Equal(t, 1, fr.calls[0])

	g, err := rRoots.Get("r1")
	require.NoError(t, err)
	require.False(t, g.NeedsAuthzPush, "the marker should be cleared after a successful Reconcile")
}

// TestAuthzPushRetryTick_ReconcileFailureConsumesDBFlagAndRemarksMemoryDirty
// is the new semantics of the "keep marker on failure" case after the
// consume-then-act hardening: the DB's needs_authz_push marker is already
// blanket-cleared in step 1 (consuming the signal), so it's no longer
// determined by whether Reconcile succeeds or fails; on Reconcile failure the
// in-memory dirty flag authzDirty is re-raised instead, leaving the next tick
// to trigger a full Reconcile retry via it.
func TestAuthzPushRetryTick_ReconcileFailureConsumesDBFlagAndRemarksMemoryDirty(t *testing.T) {
	d := openMainTestDB(t)
	rRoots := repo.NewWikiRoots(d)
	mgr := roots.NewManager(rRoots, repo.NewWikiNodes(d), repo.NewFileIndex(d), repo.NewFileEvents(d), nil, nil)

	now := time.Now().UnixMilli()
	require.NoError(t, rRoots.Insert(repo.WikiRoot{
		ID: "r1", Path: "/a", Level: "space", WatchMode: "auto",
		StorageMode: "inline", Enabled: true, ScanIntervalS: 600, CreatedAt: now,
	}))
	require.NoError(t, rRoots.SetNeedsAuthzPush("r1", true))

	fr := &fakeAuthzReconciler{errToReturn: fmt.Errorf("core unreachable")}
	authzPushRetryTick(context.Background(), rRoots, fr, mgr, zap.NewNop())

	require.Len(t, fr.calls, 1)
	g, err := rRoots.Get("r1")
	require.NoError(t, err)
	require.False(t, g.NeedsAuthzPush,
		"consume-then-act: the DB marker was already consumed and cleared in step 1, no longer determined by Reconcile's success/failure")
	require.True(t, mgr.AuthzDirty(),
		"a Reconcile failure should re-raise the in-memory dirty flag, for the next tick to retry via it")
}

// windowRaceReconciler simulates, in the callback invoked when Reconcile is
// called, "another independent push failure happening in the window": the
// real-world scenario is another goroutine (pushUpsert) failing to push for
// a different root — raising a new signal — right after this tick consumed
// its signal but before Reconcile returned. Used to verify that after the
// hardening, the new signal isn't swallowed by this tick's blanket clear.
type windowRaceReconciler struct {
	rRoots    *repo.WikiRootsRepo
	newRootID string
	calls     int
}

func (f *windowRaceReconciler) Reconcile(_ context.Context, _ []rootsync.Grant) error {
	f.calls++
	_ = f.rRoots.SetNeedsAuthzPush(f.newRootID, true)
	return nil
}

// TestAuthzPushRetryTick_WindowSignalNotSwallowedByBlanketClear is the core
// regression test for this consume-then-act hardening: verifies that a new
// signal raised in the window "after the snapshot, before the clear" is not
// swallowed by this tick's clear action — it must survive to be resolved by
// the next tick.
func TestAuthzPushRetryTick_WindowSignalNotSwallowedByBlanketClear(t *testing.T) {
	d := openMainTestDB(t)
	rRoots := repo.NewWikiRoots(d)
	mgr := roots.NewManager(rRoots, repo.NewWikiNodes(d), repo.NewFileIndex(d), repo.NewFileEvents(d), nil, nil)

	now := time.Now().UnixMilli()
	require.NoError(t, rRoots.Insert(repo.WikiRoot{
		ID: "r1", Path: "/a", Level: "space", WatchMode: "auto",
		StorageMode: "inline", Enabled: true, ScanIntervalS: 600, CreatedAt: now,
	}))
	require.NoError(t, rRoots.Insert(repo.WikiRoot{
		ID: "r2", Path: "/b", Level: "space", WatchMode: "auto",
		StorageMode: "inline", Enabled: true, ScanIntervalS: 600, CreatedAt: now,
	}))
	require.NoError(t, rRoots.SetNeedsAuthzPush("r1", true))

	fr := &windowRaceReconciler{rRoots: rRoots, newRootID: "r2"}
	authzPushRetryTick(context.Background(), rRoots, fr, mgr, zap.NewNop())

	require.Equal(t, 1, fr.calls)

	g1, err := rRoots.Get("r1")
	require.NoError(t, err)
	require.False(t, g1.NeedsAuthzPush, "the old signal consumed this round should be cleared normally")

	g2, err := rRoots.Get("r2")
	require.NoError(t, err)
	require.True(t, g2.NeedsAuthzPush,
		"core regression: a new signal raised in the window (after consumption) should not be swallowed by this tick's blanket clear, "+
			"it must survive to be resolved by the next tick")
}

func TestAuthzPushRetryTick_MemoryDirtyFlagTriggersReconcileAndClearsOnSuccess(t *testing.T) {
	d := openMainTestDB(t)
	rRoots := repo.NewWikiRoots(d)
	mgr := roots.NewManager(rRoots, repo.NewWikiNodes(d), repo.NewFileIndex(d), repo.NewFileEvents(d), nil, nil)

	dir := t.TempDir()
	id, _, err := mgr.Create(roots.CreateArgs{Path: dir, Level: "space"})
	require.NoError(t, err)

	mgr.SetPusher(failingPusher{err: fmt.Errorf("boom")})
	require.NoError(t, mgr.Delete(id, false))
	require.True(t, mgr.AuthzDirty(), "a delete push failure should set the in-memory dirty flag")

	fr := &fakeAuthzReconciler{}
	authzPushRetryTick(context.Background(), rRoots, fr, mgr, zap.NewNop())

	require.Len(t, fr.calls, 1, "the in-memory dirty flag should also trigger one full Reconcile")
	require.False(t, mgr.AuthzDirty(), "the in-memory dirty flag should be cleared after a successful Reconcile")
}
