package writer

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Wiki/pkg/db"
	"github.com/NimoTech/NimoOS-Wiki/service/repo"
	"github.com/stretchr/testify/require"
)

// newTestWriter creates a Writer for tests, accepting nil for optional args.
// Use this whenever constructing a Writer in tests so call sites don't need
// to be updated when NewWriter's signature grows.
func newTestWriter(t *testing.T, nodes *repo.WikiNodesRepo, files *repo.FileIndexRepo,
	events *repo.FileEventsRepo, summaries *repo.WikiSummariesRepo) *Writer {
	t.Helper()
	return NewWriter(nodes, files, events, nil /*bus*/, nil /*locks*/, summaries,
		0 /*debounce*/, nil /*logger*/)
}

func setupWriter(t *testing.T) (*Writer, *repo.WikiNodesRepo, *repo.FileIndexRepo, *repo.FileEventsRepo) {
	t.Helper()
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	nodes := repo.NewWikiNodes(d)
	files := repo.NewFileIndex(d)
	events := repo.NewFileEvents(d)
	w := newTestWriter(t, nodes, files, events, nil)
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
	w := NewWriter(nodes, repo.NewFileIndex(d), repo.NewFileEvents(d), nil, nil, nil, 5*time.Second, nil)

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

func TestWriter_RendersSummaryFromRepo(t *testing.T) {
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	nodes := repo.NewWikiNodes(d)
	files := repo.NewFileIndex(d)
	events := repo.NewFileEvents(d)
	summaries := repo.NewWikiSummaries(d)

	rootID := "r"
	tmp := t.TempDir()
	require.NoError(t, nodes.Upsert(repo.WikiNode{
		ID: "n", RootID: &rootID, Path: tmp, Level: "project",
		Dirty: true, UpdatedAt: 1,
	}))
	require.NoError(t, summaries.Upsert(repo.WikiSummary{
		Path: tmp, Summary: "测试目录概要。",
		GeneratedAt: 1, BasedOnLastModified: 1, GeneratorVersion: "test",
	}))

	w := newTestWriter(t, nodes, files, events, summaries)
	require.NoError(t, w.FlushOne(tmp))

	wikiMD, err := os.ReadFile(filepath.Join(tmp, ".wiki.md"))
	require.NoError(t, err)
	require.Contains(t, string(wikiMD), "测试目录概要。",
		"Summary section should contain the wiki_summaries.summary content")
}

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestWriter_FlushUpdatesChildCount(t *testing.T) {
	d := openTestDB(t)
	nodes := repo.NewWikiNodes(d)
	files := repo.NewFileIndex(d)
	events := repo.NewFileEvents(d)
	summaries := repo.NewWikiSummaries(d)

	rootID := "r"
	tmp := t.TempDir()
	require.NoError(t, nodes.Upsert(repo.WikiNode{
		ID: "n", RootID: &rootID, Path: tmp, Level: "project",
		Dirty: true, ChildCount: 0, UpdatedAt: 1,
	}))
	// Insert 3 direct children into file_index
	for _, name := range []string{"a.txt", "b.md", "sub"} {
		require.NoError(t, files.Upsert(repo.FileIndex{
			ID: repo.NewID(), RootID: rootID,
			Path: filepath.Join(tmp, name), Parent: tmp,
			IsDir: name == "sub", Status: "present", Mtime: 1,
		}))
	}

	w := newTestWriter(t, nodes, files, events, summaries)
	require.NoError(t, w.FlushOne(tmp))

	got, _ := nodes.Get(tmp)
	require.Equal(t, 3, got.ChildCount,
		"child_count should be set to the number of direct children in file_index")
}
