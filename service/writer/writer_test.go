package writer

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Wiki/pkg/db"
	"github.com/NimoTech/NimoOS-Wiki/service/repo"
	"github.com/stretchr/testify/require"
)

func setupWriter(t *testing.T) (*Writer, *repo.WikiNodesRepo, *repo.FileIndexRepo, *repo.FileEventsRepo) {
	t.Helper()
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	nodes := repo.NewWikiNodes(d)
	files := repo.NewFileIndex(d)
	events := repo.NewFileEvents(d)
	w := NewWriter(nodes, files, events, nil, 0, nil)
	return w, nodes, files, events
}

func TestWriter_BasicFlush(t *testing.T) {
	w, nodes, _, _ := setupWriter(t)
	tmp := t.TempDir()
	require.NoError(t, nodes.Upsert(repo.WikiNode{
		ID: "n", Path: tmp, Level: "project", Dirty: true, UpdatedAt: 1,
	}))
	require.NoError(t, w.FlushOne(tmp))

	body, err := os.ReadFile(filepath.Join(tmp, ".wiki.md"))
	require.NoError(t, err)
	require.Contains(t, string(body), "<!-- BEGIN: system -->")
	require.Contains(t, string(body), "## User Notes")

	n, _ := nodes.Get(tmp)
	require.False(t, n.Dirty)
	require.NotEmpty(t, n.ChecksumSystem)

	info, _ := os.Stat(filepath.Join(tmp, ".wiki.md"))
	require.Equal(t, info.ModTime().UnixMilli(), n.LastFlushedMtime,
		"file mtime must exactly match last_flushed_mtime (Chtimes + DB-before-rename)")
}

func TestWriter_DebounceSkipsRecentFlush(t *testing.T) {
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	defer d.Close()
	nodes := repo.NewWikiNodes(d)
	w := NewWriter(nodes, repo.NewFileIndex(d), repo.NewFileEvents(d), nil, 5*time.Second, nil)

	tmp := t.TempDir()
	now := time.Now().UnixMilli()
	require.NoError(t, nodes.Upsert(repo.WikiNode{
		ID: "n", Path: tmp, Level: "project", Dirty: true,
		LastFlushedAt: now, UpdatedAt: 1,
	}))

	require.NoError(t, w.FlushOne(tmp))

	// File should NOT exist (debounce kicked in)
	_, err = os.Stat(filepath.Join(tmp, ".wiki.md"))
	require.True(t, os.IsNotExist(err), "debounce should skip the flush")

	// Dirty preserved
	n, _ := nodes.Get(tmp)
	require.True(t, n.Dirty)
}

func TestWriter_ChecksumUnchanged_SkipsIO(t *testing.T) {
	w, nodes, _, _ := setupWriter(t)
	tmp := t.TempDir()

	// First flush computes and stores checksum
	require.NoError(t, nodes.Upsert(repo.WikiNode{
		ID: "n", Path: tmp, Level: "project", Dirty: true, UpdatedAt: 1,
	}))
	require.NoError(t, w.FlushOne(tmp))
	n1, _ := nodes.Get(tmp)
	firstChecksum := n1.ChecksumSystem
	require.NotEmpty(t, firstChecksum)

	// Mark dirty again with same content; flush again should skip the write
	require.NoError(t, nodes.SetDirty(tmp, true))

	stat0, _ := os.Stat(filepath.Join(tmp, ".wiki.md"))
	require.NoError(t, w.FlushOne(tmp))
	stat1, _ := os.Stat(filepath.Join(tmp, ".wiki.md"))
	// mtime should NOT have changed
	require.Equal(t, stat0.ModTime(), stat1.ModTime(),
		"identical-content flush should skip I/O entirely")

	n2, _ := nodes.Get(tmp)
	require.False(t, n2.Dirty)
	require.Equal(t, firstChecksum, n2.ChecksumSystem)
}

func TestWriter_NonDirtyIsNoOp(t *testing.T) {
	w, nodes, _, _ := setupWriter(t)
	tmp := t.TempDir()
	require.NoError(t, nodes.Upsert(repo.WikiNode{
		ID: "n", Path: tmp, Level: "project", Dirty: false, UpdatedAt: 1,
	}))
	require.NoError(t, w.FlushOne(tmp))
	_, err := os.Stat(filepath.Join(tmp, ".wiki.md"))
	require.True(t, os.IsNotExist(err))
}

func TestWriter_MtimeRecordedMatchesFile(t *testing.T) {
	// Direct regression test for review2 #2: after FlushOne,
	// info.ModTime().UnixMilli() MUST equal node.LastFlushedMtime.
	// This is what prevents the Watcher from misclassifying our own write.
	w, nodes, _, _ := setupWriter(t)
	tmp := t.TempDir()
	require.NoError(t, nodes.Upsert(repo.WikiNode{
		ID: "n", Path: tmp, Level: "project", Dirty: true, UpdatedAt: 1,
	}))
	require.NoError(t, w.FlushOne(tmp))

	info, err := os.Stat(filepath.Join(tmp, ".wiki.md"))
	require.NoError(t, err)
	n, _ := nodes.Get(tmp)
	require.Equal(t, info.ModTime().UnixMilli(), n.LastFlushedMtime)
}
