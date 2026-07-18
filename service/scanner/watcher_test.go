package scanner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Wiki/pkg/db"
	"github.com/NimoTech/NimoOS-Wiki/pkg/ignore"
	"github.com/NimoTech/NimoOS-Wiki/service/repo"
	"github.com/stretchr/testify/require"
)

func setupWatcher(t *testing.T) (*Watcher, *repo.FileEventsRepo, *repo.WikiNodesRepo, string) {
	t.Helper()
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	files := repo.NewFileIndex(d)
	_ = files
	events := repo.NewFileEvents(d)
	nodes := repo.NewWikiNodes(d)
	root := t.TempDir()
	w := NewWatcher(events, nodes, ignore.New([]string{"node_modules"}), nil, nil)
	require.NoError(t, w.Watch("r", root))
	return w, events, nodes, root
}

func TestWatcher_CreateProducesEvent(t *testing.T) {
	w, events, _, root := setupWatcher(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	require.NoError(t, os.WriteFile(filepath.Join(root, "x.txt"), []byte("hi"), 0644))
	require.Eventually(t, func() bool {
		evs, _ := events.ListUnprocessed(10)
		for _, e := range evs {
			if e.Op == "create" && filepath.Base(e.Path) == "x.txt" {
				return true
			}
		}
		return false
	}, 2*time.Second, 50*time.Millisecond)
}

func TestWatcher_WikiMdTmpDiscarded(t *testing.T) {
	w, events, _, root := setupWatcher(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	require.NoError(t, os.WriteFile(filepath.Join(root, ".wiki.md.tmp"), []byte("x"), 0644))
	time.Sleep(200 * time.Millisecond)
	evs, _ := events.ListUnprocessed(10)
	for _, e := range evs {
		require.NotEqual(t, ".wiki.md.tmp", filepath.Base(e.Path), ".wiki.md.tmp must be discarded")
	}
}

func TestWatcher_WikiMdSelfWriteIgnored(t *testing.T) {
	w, events, nodes, root := setupWatcher(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	// Create a node for the root with last_flushed_mtime = future
	require.NoError(t, nodes.Upsert(repo.WikiNode{
		ID: "n", Path: root, Level: "space",
		LastFlushedMtime: time.Now().Add(5 * time.Minute).UnixMilli(),
		UpdatedAt:        time.Now().UnixMilli(),
	}))

	// Write a .wiki.md whose mtime is "now" — DB says future, so this is
	// less-than-or-equal → drop. (Self-write semantics: DB equals or is greater than file mtime.)
	require.NoError(t, os.WriteFile(filepath.Join(root, ".wiki.md"), []byte("body"), 0644))
	time.Sleep(300 * time.Millisecond)

	// No sync task enqueued
	select {
	case <-w.SyncOut:
		t.Fatal("self-write should not enqueue sync task")
	default:
	}
	// No file_event row for .wiki.md
	evs, _ := events.ListUnprocessed(10)
	for _, e := range evs {
		require.NotEqual(t, ".wiki.md", filepath.Base(e.Path))
	}
}

func TestWatcher_WikiMdExternalEditTriggersSync(t *testing.T) {
	w, _, nodes, root := setupWatcher(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	// Node says last_flushed at distant past
	require.NoError(t, nodes.Upsert(repo.WikiNode{
		ID: "n", Path: root, Level: "space",
		LastFlushedMtime: 1, // ms
		UpdatedAt:        time.Now().UnixMilli(),
	}))

	require.NoError(t, os.WriteFile(filepath.Join(root, ".wiki.md"), []byte("new edits"), 0644))

	select {
	case task := <-w.SyncOut:
		require.Equal(t, root, task.NodePath)
		require.Equal(t, "r", task.RootID)
	case <-time.After(2 * time.Second):
		t.Fatal("expected user-notes sync task")
	}
}

func TestWatcher_WikiMdDeleteMarksDirty(t *testing.T) {
	w, _, nodes, root := setupWatcher(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	require.NoError(t, nodes.Upsert(repo.WikiNode{
		ID: "n", Path: root, Level: "space",
		LastFlushedMtime: time.Now().UnixMilli(),
		UpdatedAt:        time.Now().UnixMilli(),
	}))

	// Create then immediately delete .wiki.md
	wikiMD := filepath.Join(root, ".wiki.md")
	require.NoError(t, os.WriteFile(wikiMD, []byte(""), 0644))
	time.Sleep(100 * time.Millisecond)
	require.NoError(t, os.Remove(wikiMD))

	require.Eventually(t, func() bool {
		n, _ := nodes.Get(root)
		return n != nil && n.Dirty
	}, 2*time.Second, 50*time.Millisecond)
}

func TestWatcher_NewDirWithContents_BackfillEvents(t *testing.T) {
	w, events, _, root := setupWatcher(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	// Simulate `cp -r` / `mv non_container_dir into_watched`:
	// build the dir tree outside the watched root, then rename into place atomically.
	staging := filepath.Join(t.TempDir(), "staging")
	require.NoError(t, os.MkdirAll(filepath.Join(staging, "sub"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(staging, "a.txt"), []byte("x"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(staging, "sub", "b.txt"), []byte("y"), 0644))

	target := filepath.Join(root, "moved")
	require.NoError(t, os.Rename(staging, target))

	require.Eventually(t, func() bool {
		evs, _ := events.ListUnprocessed(50)
		hasA := false
		hasB := false
		for _, e := range evs {
			if e.Path == filepath.Join(target, "a.txt") {
				hasA = true
			}
			if e.Path == filepath.Join(target, "sub", "b.txt") {
				hasB = true
			}
		}
		return hasA && hasB
	}, 3*time.Second, 100*time.Millisecond)
}

func TestWatcher_NewDirContainingContainer_RecordedOpaque(t *testing.T) {
	w, events, _, root := setupWatcher(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	// Move-in a tree that has a node_modules inside it
	staging := filepath.Join(t.TempDir(), "staging")
	require.NoError(t, os.MkdirAll(filepath.Join(staging, "node_modules", "lodash"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(staging, "node_modules", "lodash", "x.js"), []byte(""), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(staging, "main.go"), []byte(""), 0644))

	target := filepath.Join(root, "proj")
	require.NoError(t, os.Rename(staging, target))

	require.Eventually(t, func() bool {
		evs, _ := events.ListUnprocessed(50)
		sawOpaqueNm := false
		sawMainGo := false
		for _, e := range evs {
			if e.Path == filepath.Join(target, "node_modules") && e.IsDir {
				sawOpaqueNm = true
			}
			if e.Path == filepath.Join(target, "main.go") {
				sawMainGo = true
			}
			// descendants of node_modules must NOT be present
			if strings.Contains(e.Path, filepath.Join("node_modules", "lodash")) {
				t.Fatalf("descendant of container dir leaked into events: %s", e.Path)
			}
		}
		return sawOpaqueNm && sawMainGo
	}, 3*time.Second, 100*time.Millisecond)
}

func TestWatcher_ContainerDirSkipped(t *testing.T) {
	w, events, _, root := setupWatcher(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	require.NoError(t, os.Mkdir(filepath.Join(root, "node_modules"), 0755))
	time.Sleep(200 * time.Millisecond)
	require.NoError(t, os.WriteFile(filepath.Join(root, "node_modules", "x.js"), []byte(""), 0644))
	time.Sleep(300 * time.Millisecond)

	evs, _ := events.ListUnprocessed(20)
	for _, e := range evs {
		require.NotContains(t, e.Path, filepath.Join("node_modules", "x.js"),
			"events inside container dir must be skipped")
	}
}

func TestWatcherDropsEventsWhileStorming(t *testing.T) {
	w, events, _, _ := setupWatcher(t)

	g := NewStormGuard(1, 0, 1000, 100)
	g.Update(map[string]int{"root1": 2}) // backlog 2 > high 1 → storm
	w.Guard = g

	w.insertEvent("root1", "/tmp/x", "create", false)
	// non-storming root: inserted as normal
	w.insertEvent("root2", "/tmp/y", "create", false)

	evs, err := events.ListUnprocessed(20)
	require.NoError(t, err)
	require.Len(t, evs, 1, "storming root's event must be dropped")
	require.Equal(t, "root2", evs[0].RootID)
	require.Equal(t, "/tmp/y", evs[0].Path)
}
