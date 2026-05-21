package scanner

import (
	"os"
	"path/filepath"
	"testing"

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

	require.NoError(t, rec.Reconcile("r", root))
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

	require.NoError(t, rec.Reconcile("r", root))
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
	require.NoError(t, rec.Reconcile("r", root))
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

	require.NoError(t, rec.Reconcile("r", root))
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

	require.NoError(t, rec.Reconcile("r", root))
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
	require.NoError(t, rec.Reconcile("r", root))

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

	require.NoError(t, rec.Reconcile("r", root))
	pending, _ := events.ListUnprocessed(100)
	ids := make([]string, 0, len(pending))
	for _, e := range pending {
		ids = append(ids, e.ID)
	}
	require.NoError(t, events.MarkProcessed(ids, 1))

	// Now delete the file on disk and reconcile. No opaque ancestor exists,
	// so the delete event MUST still fire (regression check).
	require.NoError(t, os.Remove(filepath.Join(root, "doomed.txt")))
	require.NoError(t, rec.Reconcile("r", root))

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
