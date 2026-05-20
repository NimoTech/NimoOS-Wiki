package processor

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Wiki/pkg/db"
	"github.com/NimoTech/NimoOS-Wiki/pkg/ignore"
	"github.com/NimoTech/NimoOS-Wiki/service/repo"
	"github.com/stretchr/testify/require"
)

func setup(t *testing.T) (*EventProcessor, *repo.FileIndexRepo, *repo.FileEventsRepo, *repo.WikiNodesRepo, *sql.DB) {
	t.Helper()
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	files := repo.NewFileIndex(d)
	events := repo.NewFileEvents(d)
	nodes := repo.NewWikiNodes(d)
	parse := repo.NewParseStatus(d)
	p := New(d, files, events, nodes, parse, nil, nil, nil, nil)
	return p, files, events, nodes, d
}

func strPtr(s string) *string { return &s }

func TestProcessor_DebounceCoalesces(t *testing.T) {
	p, _, events, _, _ := setup(t)
	now := time.Now().UnixMilli()
	for i := 0; i < 5; i++ {
		require.NoError(t, events.Insert(repo.FileEvent{
			ID: repo.NewID(), RootID: "r", Path: "/x.txt", Op: "modify",
			DetectedAt: now + int64(i*50), // 5 events within 250ms
		}))
	}
	require.NoError(t, p.ProcessBatch(context.Background()))
	// All 5 should be marked processed
	unp, _ := events.ListUnprocessed(100)
	require.Len(t, unp, 0)
}

func TestProcessor_DirectoryRenameCascades(t *testing.T) {
	p, files, events, nodes, _ := setup(t)
	now := time.Now().UnixMilli()

	for _, item := range []struct {
		path  string
		isDir bool
	}{
		{"/DATA/ProjectA", true},
		{"/DATA/ProjectA/sub", true},
		{"/DATA/ProjectA/sub/a.go", false},
		{"/DATA/projecta/x.txt", false}, // case-different sibling
	} {
		require.NoError(t, files.Upsert(repo.FileIndex{
			ID: repo.NewID(), RootID: "r", Path: item.path,
			Parent: parentOf(item.path), IsDir: item.isDir, Status: "present", Mtime: 1,
		}))
	}
	require.NoError(t, nodes.Upsert(repo.WikiNode{
		ID: "n1", Path: "/DATA/ProjectA", Level: "project", UpdatedAt: 1,
	}))

	// Insert paired rename: Rename(/DATA/ProjectA) + Create(/DATA/Renamed) within 1s
	require.NoError(t, events.Insert(repo.FileEvent{
		ID: repo.NewID(), RootID: "r", Path: "/DATA/ProjectA", Op: "rename",
		IsDir: true, DetectedAt: now,
	}))
	require.NoError(t, events.Insert(repo.FileEvent{
		ID: repo.NewID(), RootID: "r", Path: "/DATA/Renamed", Op: "create",
		IsDir: true, DetectedAt: now + 50,
	}))

	require.NoError(t, p.ProcessBatch(context.Background()))

	// Cascaded: descendant should be at /DATA/Renamed/...
	got, _ := files.Get("r", "/DATA/Renamed/sub/a.go")
	require.NotNil(t, got, "subtree should have cascaded")
	// Case-different sibling untouched
	got2, _ := files.Get("r", "/DATA/projecta/x.txt")
	require.NotNil(t, got2, "lowercase sibling must NOT be touched")
	// wiki_node moved
	n, _ := nodes.Get("/DATA/Renamed")
	require.NotNil(t, n, "wiki_node should have been renamed")
}

func TestProcessor_NewFileInsertsPending(t *testing.T) {
	p, _, events, _, d := setup(t)
	now := time.Now().UnixMilli()
	require.NoError(t, events.Insert(repo.FileEvent{
		ID: repo.NewID(), RootID: "r", Path: "/foo.pdf", Op: "create",
		IsDir: false, DetectedAt: now,
	}))
	require.NoError(t, p.ProcessBatch(context.Background()))

	var status string
	err := d.QueryRow(`SELECT status FROM parse_status WHERE path = ?`, "/foo.pdf").Scan(&status)
	require.NoError(t, err)
	require.Equal(t, "pending", status)
}

func TestProcessor_DirNotInsertedPending(t *testing.T) {
	p, _, events, _, d := setup(t)
	now := time.Now().UnixMilli()
	require.NoError(t, events.Insert(repo.FileEvent{
		ID: repo.NewID(), RootID: "r", Path: "/dir", Op: "create",
		IsDir: true, DetectedAt: now,
	}))
	require.NoError(t, p.ProcessBatch(context.Background()))
	var n int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM parse_status WHERE path = ?`, "/dir").Scan(&n))
	require.Equal(t, 0, n, "dir should NOT get a parse_status row")
}

func TestProcessor_CreateDir_ContainerMarkedOpaque(t *testing.T) {
	// Regression for follow-up fix #2: Watcher-driven create events for
	// container dirs (e.g. /path/node_modules from backfillNewDir) must
	// land in file_index with is_opaque=1 so Child Map renders correctly.
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	files := repo.NewFileIndex(d)
	events := repo.NewFileEvents(d)
	nodes := repo.NewWikiNodes(d)
	parse := repo.NewParseStatus(d)
	ig := ignore.New([]string{"node_modules", ".git"})
	p := New(d, files, events, nodes, parse, nil, ig, nil, nil)

	now := time.Now().UnixMilli()
	require.NoError(t, events.Insert(repo.FileEvent{
		ID: repo.NewID(), RootID: "r", Path: "/proj/node_modules", Op: "create",
		IsDir: true, DetectedAt: now,
	}))
	require.NoError(t, events.Insert(repo.FileEvent{
		ID: repo.NewID(), RootID: "r", Path: "/proj/main.go", Op: "create",
		IsDir: false, DetectedAt: now,
	}))
	require.NoError(t, p.ProcessBatch(context.Background()))

	nm, _ := files.Get("r", "/proj/node_modules")
	require.NotNil(t, nm)
	require.True(t, nm.IsOpaque, "container dir basename should be flagged opaque")

	src, _ := files.Get("r", "/proj/main.go")
	require.NotNil(t, src)
	require.False(t, src.IsOpaque)
}

func TestProcessor_DeleteRemovesFromIndex(t *testing.T) {
	p, files, events, _, _ := setup(t)
	require.NoError(t, files.Upsert(repo.FileIndex{
		ID: repo.NewID(), RootID: "r", Path: "/gone.txt",
		Parent: "/", Status: "present", Mtime: 1,
	}))
	require.NoError(t, events.Insert(repo.FileEvent{
		ID: repo.NewID(), RootID: "r", Path: "/gone.txt", Op: "delete", DetectedAt: 2,
	}))
	require.NoError(t, p.ProcessBatch(context.Background()))
	got, _ := files.Get("r", "/gone.txt")
	require.Nil(t, got)
}

func parentOf(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			if i == 0 {
				return "/"
			}
			return p[:i]
		}
	}
	return "/"
}

// fakeBus records publishes for test assertions.
type fakeBus struct {
	mu        sync.Mutex
	publishes []struct {
		Event   string
		Payload any
	}
}

func (f *fakeBus) Publish(ev string, payload any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.publishes = append(f.publishes, struct {
		Event   string
		Payload any
	}{ev, payload})
}
func (f *fakeBus) Close() error { return nil }

func (f *fakeBus) countOf(event string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, p := range f.publishes {
		if p.Event == event {
			n++
		}
	}
	return n
}

func TestProcessor_RecentChangedAggregatedPerRoot(t *testing.T) {
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	files := repo.NewFileIndex(d)
	events := repo.NewFileEvents(d)
	nodes := repo.NewWikiNodes(d)
	parse := repo.NewParseStatus(d)
	bus := &fakeBus{}
	p := New(d, files, events, nodes, parse, bus, nil, nil, nil)

	now := time.Now().UnixMilli()
	// 5 modifies on the same root + 3 on a second root within one batch
	for i := 0; i < 5; i++ {
		require.NoError(t, events.Insert(repo.FileEvent{
			ID: repo.NewID(), RootID: "rA", Path: "/a/" + string(rune('a'+i)) + ".txt",
			Op: "modify", DetectedAt: now + int64(i),
		}))
	}
	for i := 0; i < 3; i++ {
		require.NoError(t, events.Insert(repo.FileEvent{
			ID: repo.NewID(), RootID: "rB", Path: "/b/" + string(rune('a'+i)) + ".txt",
			Op: "modify", DetectedAt: now + int64(100+i),
		}))
	}

	require.NoError(t, p.ProcessBatch(context.Background()))

	require.Equal(t, 2, bus.countOf("Wiki:RecentChanged"),
		"exactly one RecentChanged per distinct root_id")
}
