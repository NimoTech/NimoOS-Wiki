package scanner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Wiki/pkg/db"
	"github.com/NimoTech/NimoOS-Wiki/pkg/ignore"
	"github.com/NimoTech/NimoOS-Wiki/service/repo"
	"github.com/stretchr/testify/require"
)

func openDB(t *testing.T) (files *repo.FileIndexRepo, events *repo.FileEventsRepo) {
	t.Helper()
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	return repo.NewFileIndex(d), repo.NewFileEvents(d)
}

func TestReconciler_DetectsCreateModifyDelete(t *testing.T) {
	files, events := openDB(t)
	rec := NewReconciler(files, events, ignore.New(nil))

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0644))

	require.NoError(t, rec.Reconcile(context.Background(), "r", root))
	evs, _ := events.ListUnprocessed(100)
	require.Len(t, evs, 1)
	require.Equal(t, "create", evs[0].Op)
	require.Equal(t, filepath.Join(root, "a.txt"), evs[0].Path)

	// Mark processed
	ids := []string{evs[0].ID}
	require.NoError(t, events.MarkProcessed(ids, 1))

	// Modify + delete + create
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.txt"), []byte("xxxx"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "b.txt"), []byte("y"), 0644))

	require.NoError(t, rec.Reconcile(context.Background(), "r", root))
	news, _ := events.ListUnprocessed(100)
	ops := map[string]bool{}
	for _, e := range news {
		ops[e.Op+":"+filepath.Base(e.Path)] = true
	}
	require.True(t, ops["modify:a.txt"], "a.txt should be marked modified")
	require.True(t, ops["create:b.txt"], "b.txt should be marked created")

	// Mark processed, then delete a.txt
	var newIDs []string
	for _, e := range news {
		newIDs = append(newIDs, e.ID)
	}
	require.NoError(t, events.MarkProcessed(newIDs, 2))

	require.NoError(t, os.Remove(filepath.Join(root, "a.txt")))
	require.NoError(t, rec.Reconcile(context.Background(), "r", root))
	dels, _ := events.ListUnprocessed(100)
	require.Len(t, dels, 1)
	require.Equal(t, "delete", dels[0].Op)
}

func TestReconciler_SkipsContainerDirs(t *testing.T) {
	files, events := openDB(t)
	rec := NewReconciler(files, events, ignore.New([]string{"node_modules"}))

	root := t.TempDir()
	nm := filepath.Join(root, "node_modules")
	require.NoError(t, os.MkdirAll(filepath.Join(nm, "lodash"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(nm, "lodash", "index.js"), []byte(""), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.go"), []byte(""), 0644))

	require.NoError(t, rec.Reconcile(context.Background(), "r", root))
	all, _ := files.ListAllByRoot("r")
	hasOpaque := false
	hasInside := false
	for _, f := range all {
		if f.Path == nm {
			hasOpaque = true
			require.True(t, f.IsOpaque)
		}
		if f.Path == filepath.Join(nm, "lodash") || f.Path == filepath.Join(nm, "lodash", "index.js") {
			hasInside = true
		}
	}
	require.True(t, hasOpaque, "container dir should be recorded")
	require.False(t, hasInside, "container contents should NOT be recorded")
}

func TestReconciler_IgnoresWikiMd(t *testing.T) {
	files, events := openDB(t)
	rec := NewReconciler(files, events, ignore.New(nil))

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, ".wiki.md"), []byte("# ..."), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "real.txt"), []byte(""), 0644))

	require.NoError(t, rec.Reconcile(context.Background(), "r", root))
	all, _ := files.ListAllByRoot("r")
	for _, f := range all {
		require.NotEqual(t, ".wiki.md", filepath.Base(f.Path), ".wiki.md must NOT be indexed (system output)")
	}
}

func TestReconciler_SilentlyPurgesRowsUnderNewlyOpaqueAncestor(t *testing.T) {
	files, events := openDB(t)

	root := t.TempDir()
	immich := filepath.Join(root, "immich")
	library := filepath.Join(immich, "library")
	marker := filepath.Join(library, ".immich")
	require.NoError(t, os.MkdirAll(library, 0755))
	require.NoError(t, os.WriteFile(marker, []byte(""), 0644))

	// Simulate "legacy" file_index state: rows were indexed BEFORE immich
	// was a baseline container. We hand-write them in so we don't need a
	// fake ignore.Matcher (Reconciler.ig is a concrete *ignore.Matcher,
	// not an interface — overriding it would require refactoring).
	for _, item := range []struct {
		path  string
		isDir bool
	}{
		{immich, true},
		{library, true},
		{marker, false},
	} {
		require.NoError(t, files.Upsert(repo.FileIndex{
			ID: repo.NewID(), RootID: "r", Path: item.path,
			Parent: filepath.Dir(item.path), IsDir: item.isDir,
			Status: "present", Mtime: 1,
		}))
	}

	// Now reconcile with immich as a container. The walk visits /root/immich,
	// marks it opaque, SkipDirs. The two leftover rows under it MUST be
	// silently purged (no delete events emitted).
	rec := NewReconciler(files, events, ignore.New([]string{"immich"}))
	require.NoError(t, rec.Reconcile(context.Background(), "r", root))

	// Assertion 1: file_index now contains exactly one row — the opaque
	// immich dir itself. Any other path means either the orphans weren't
	// purged, or the setup never inserted rows in the first place.
	all, _ := files.ListAllByRoot("r")
	require.Len(t, all, 1, "expected exactly one row (the opaque immich dir) after silent purge")
	require.Equal(t, immich, all[0].Path, "the surviving row should be immich itself")
	require.True(t, all[0].IsOpaque, "/root/immich should be marked opaque")

	// Assertion 2: NO delete events were emitted for the purged rows.
	newEvents, _ := events.ListUnprocessed(100)
	for _, e := range newEvents {
		if e.Op == "delete" && (e.Path == library || e.Path == marker) {
			t.Errorf("expected silent purge of %q, but got delete event %+v", e.Path, e)
		}
	}
}

func TestReconciler_DeleteEventStillEmittedWhenNoOpaqueAncestor(t *testing.T) {
	files, events := openDB(t)
	rec := NewReconciler(files, events, ignore.New(nil))

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "doomed.txt"), []byte("x"), 0644))

	require.NoError(t, rec.Reconcile(context.Background(), "r", root))
	pending, _ := events.ListUnprocessed(100)
	ids := make([]string, 0, len(pending))
	for _, e := range pending {
		ids = append(ids, e.ID)
	}
	require.NoError(t, events.MarkProcessed(ids, 1))

	// Now delete the file on disk and reconcile. No opaque ancestor exists,
	// so the delete event MUST still fire (regression check).
	require.NoError(t, os.Remove(filepath.Join(root, "doomed.txt")))
	require.NoError(t, rec.Reconcile(context.Background(), "r", root))

	news, _ := events.ListUnprocessed(100)
	sawDelete := false
	for _, e := range news {
		if e.Op == "delete" && filepath.Base(e.Path) == "doomed.txt" {
			sawDelete = true
		}
	}
	if !sawDelete {
		t.Error("normal-path delete event should still be emitted when no opaque ancestor exists")
	}
}

// TestStreamingReconcilePagesThroughIndex exercises the two-pass streaming
// reconcile with BatchSize=2 against 5 pre-seeded file_index rows (3 gone
// from disk, 1 mtime-changed, 1 unchanged) plus 2 brand-new files on disk.
// BatchSize=2 against 5 rows forces exactly 3 keyset pages through pass A,
// which is what actually exercises ListByRootAfter's paging.
func TestStreamingReconcilePagesThroughIndex(t *testing.T) {
	files, events := openDB(t)
	rec := NewReconciler(files, events, ignore.New(nil))
	rec.BatchSize = 2

	root := t.TempDir()

	keepPath := filepath.Join(root, "keep.txt")
	modPath := filepath.Join(root, "mod.txt")
	require.NoError(t, os.WriteFile(keepPath, []byte("keep"), 0644))
	require.NoError(t, os.WriteFile(modPath, []byte("mod"), 0644))
	keepInfo, err := os.Stat(keepPath)
	require.NoError(t, err)

	seed := func(path string, mtime, size int64) {
		require.NoError(t, files.Upsert(repo.FileIndex{
			ID: repo.NewID(), RootID: "r", Path: path, Parent: filepath.Dir(path),
			IsDir: false, Status: "present", Mtime: mtime, Size: size,
		}))
	}
	seed(keepPath, keepInfo.ModTime().UnixMilli(), keepInfo.Size()) // unchanged
	seed(modPath, 1, 0)                                             // stale mtime/size → modify

	delPaths := []string{
		filepath.Join(root, "del1.txt"),
		filepath.Join(root, "del2.txt"),
		filepath.Join(root, "del3.txt"),
	}
	for _, p := range delPaths {
		seed(p, 1, 0) // rows with nothing on disk → delete
	}

	create1 := filepath.Join(root, "create1.txt")
	create2 := filepath.Join(root, "create2.txt")
	require.NoError(t, os.WriteFile(create1, []byte("c1"), 0644))
	require.NoError(t, os.WriteFile(create2, []byte("c2"), 0644))

	require.NoError(t, rec.Reconcile(context.Background(), "r", root))

	evs, err := events.ListUnprocessed(100)
	require.NoError(t, err)
	var nDelete, nModify, nCreate int
	for _, e := range evs {
		switch e.Op {
		case "delete":
			nDelete++
		case "modify":
			nModify++
		case "create":
			nCreate++
		}
	}
	require.Equal(t, 3, nDelete, "the 3 phantom rows should be deleted")
	require.Equal(t, 1, nModify, "mod.txt's stale mtime/size should trigger a modify")
	require.Equal(t, 2, nCreate, "create1/create2 should be created")

	all, err := files.ListAllByRoot("r")
	require.NoError(t, err)
	byPath := make(map[string]repo.FileIndex, len(all))
	for _, f := range all {
		byPath[f.Path] = f
	}
	require.Len(t, all, 4, "final state: keep, mod, create1, create2 — deletes gone")
	require.Contains(t, byPath, keepPath)
	require.Contains(t, byPath, modPath)
	require.Contains(t, byPath, create1)
	require.Contains(t, byPath, create2)
	for _, p := range delPaths {
		require.NotContains(t, byPath, p, "deleted row must not survive reconcile")
	}
}

// TestReconcileAbortsOnNonNotExistStatError verifies the strict Lstat error
// discrimination: an EACCES (or any error other than fs.ErrNotExist) MUST
// abort the whole round rather than being treated as "gone" — a flapping or
// permission-denied mount must never look like a mass delete.
func TestReconcileAbortsOnNonNotExistStatError(t *testing.T) {
	files, events := openDB(t)
	rec := NewReconciler(files, events, ignore.New(nil))

	root := t.TempDir()
	d := filepath.Join(root, "d")
	f := filepath.Join(d, "f")
	require.NoError(t, os.MkdirAll(d, 0755))
	require.NoError(t, os.WriteFile(f, []byte("x"), 0644))

	require.NoError(t, files.Upsert(repo.FileIndex{
		ID: repo.NewID(), RootID: "r", Path: f, Parent: d,
		IsDir: false, Status: "present", Mtime: 1, Size: 1,
	}))

	// Strip search permission on d so Lstat(f) fails with EACCES, not ENOENT.
	require.NoError(t, os.Chmod(d, 0))
	t.Cleanup(func() { _ = os.Chmod(d, 0755) })

	err := rec.Reconcile(context.Background(), "r", root)
	require.Error(t, err, "a non-ENOENT stat error must abort the round")

	evs, _ := events.ListUnprocessed(100)
	nDelete := 0
	for _, e := range evs {
		if e.Op == "delete" {
			nDelete++
		}
	}
	require.Equal(t, 0, nDelete, "an aborted round must not emit any delete events")

	got, gerr := files.Get("r", f)
	require.NoError(t, gerr)
	require.NotNil(t, got, "the file_index row must survive an aborted round")
}

// TestReconcileCancellable verifies the throttle is cancellable mid-round:
// with a tiny ThrottleEvery/ThrottleSleep over a large tree, a full run would
// take tens of seconds, but cancelling the context shortly after starting
// must make Reconcile return promptly with context.Canceled.
func TestReconcileCancellable(t *testing.T) {
	files, events := openDB(t)
	rec := NewReconciler(files, events, ignore.New(nil))
	rec.ThrottleEvery = 10
	rec.ThrottleSleep = 50 * time.Millisecond

	root := t.TempDir()
	const n = 10000
	for i := 0; i < n; i++ {
		f, err := os.Create(filepath.Join(root, fmt.Sprintf("f%05d.txt", i)))
		require.NoError(t, err)
		require.NoError(t, f.Close())
	}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(500*time.Millisecond, cancel)

	start := time.Now()
	err := rec.Reconcile(ctx, "r", root)
	elapsed := time.Since(start)

	require.ErrorIs(t, err, context.Canceled)
	// An uninterrupted full run needs roughly (n/ThrottleEvery)*ThrottleSleep
	// ≈ 50s of throttle sleep alone. A prompt cancel must return in a small
	// fraction of that floor — assert behavior, not a tight wall-clock bound,
	// so this survives -race and slow CI.
	require.Less(t, elapsed, 2*time.Second, "cancel should abort promptly, not run to completion")
}

// TestSilentPurgeUnderContainerAncestor verifies pass A's silent-purge branch
// directly: a stale file_index row living under a container-named ancestor
// (e.g. node_modules) that is now entirely gone from disk must be deleted
// WITHOUT emitting a delete event.
func TestSilentPurgeUnderContainerAncestor(t *testing.T) {
	files, events := openDB(t)
	rec := NewReconciler(files, events, ignore.New([]string{"node_modules"}))

	root := t.TempDir()
	nm := filepath.Join(root, "node_modules")
	x := filepath.Join(nm, "x")

	// Nothing on disk at all — not even node_modules itself — simulating a
	// container dir removed wholesale since the last reconcile.
	require.NoError(t, files.Upsert(repo.FileIndex{
		ID: repo.NewID(), RootID: "r", Path: x, Parent: nm,
		IsDir: false, Status: "present", Mtime: 1, Size: 1,
	}))

	require.NoError(t, rec.Reconcile(context.Background(), "r", root))

	all, err := files.ListAllByRoot("r")
	require.NoError(t, err)
	require.Empty(t, all, "stale row under a container ancestor must be purged")

	evs, _ := events.ListUnprocessed(100)
	for _, e := range evs {
		require.NotEqual(t, x, e.Path, "silent purge must not emit an event for the purged row")
	}
}
