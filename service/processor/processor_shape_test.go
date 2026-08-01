package processor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Wiki/pkg/ignore"
	"github.com/NimoTech/NimoOS-Wiki/service/repo"
	"github.com/NimoTech/NimoOS-Wiki/service/scanner"
	"github.com/stretchr/testify/require"
)

// Root cause 1: the file_index row written by a create event must have the
// same shape as reconciler pass B (real mtime/size + ext), otherwise the
// first reconcile pass fires a spurious modify for every file.
func TestCreateEventWritesReconcilerShape(t *testing.T) {
	p, files, events, _, _ := setup(t)
	dir := t.TempDir()
	fp := filepath.Join(dir, "a.txt")
	require.NoError(t, os.WriteFile(fp, []byte("hello world"), 0644))
	info, err := os.Lstat(fp)
	require.NoError(t, err)

	require.NoError(t, events.Insert(repo.FileEvent{
		ID: repo.NewID(), RootID: "r", Path: fp, Op: "create",
		DetectedAt: time.Now().UnixMilli() + 12345, // deliberately != real mtime
	}))
	require.NoError(t, p.ProcessBatch(context.Background()))

	row, err := files.Get("r", fp)
	require.NoError(t, err)
	require.NotNil(t, row)
	require.Equal(t, "txt", row.Ext)
	require.Equal(t, info.Size(), row.Size)
	require.Equal(t, info.ModTime().UnixMilli(), row.Mtime)
}

func TestReconcileAfterCreateEmitsNoModify(t *testing.T) {
	p, files, events, _, _ := setup(t)
	dir := t.TempDir()
	fp := filepath.Join(dir, "b.md")
	require.NoError(t, os.WriteFile(fp, []byte("# doc"), 0644))

	require.NoError(t, events.Insert(repo.FileEvent{
		ID: repo.NewID(), RootID: "r", Path: fp, Op: "create",
		DetectedAt: time.Now().UnixMilli(),
	}))
	require.NoError(t, p.ProcessBatch(context.Background()))

	rec := scanner.NewReconciler(files, events, ignore.New(nil))
	require.NoError(t, rec.Reconcile(context.Background(), "r", dir))

	evs, err := events.ListSince("r", 0, 1000)
	require.NoError(t, err)
	for _, e := range evs {
		require.NotEqual(t, "modify", e.Op,
			"first reconcile after create must not emit spurious modify for %s", e.Path)
	}
}
