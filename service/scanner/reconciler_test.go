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
