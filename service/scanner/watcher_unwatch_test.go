package scanner

import (
	"testing"

	"github.com/NimoTech/NimoOS-Wiki/pkg/db"
	"github.com/NimoTech/NimoOS-Wiki/pkg/ignore"
	"github.com/NimoTech/NimoOS-Wiki/service/repo"
	"github.com/stretchr/testify/require"
)

func TestUnwatchRemovesRootAndWatches(t *testing.T) {
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	w := NewWatcher(repo.NewFileEvents(d), repo.NewWikiNodes(d), ignore.New(nil), nil, nil)
	root := t.TempDir()
	require.NoError(t, w.Watch("r1", root))
	require.Equal(t, "r1", w.rootIDFor(root+"/sub.txt"))
	require.NotEmpty(t, w.fsw.WatchList())

	w.Unwatch("r1")
	require.Equal(t, "", w.rootIDFor(root+"/sub.txt"))
	require.Empty(t, w.fsw.WatchList())

	// unknown id is a no-op
	w.Unwatch("missing")
}
