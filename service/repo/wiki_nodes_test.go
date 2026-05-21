package repo

import (
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Wiki/pkg/db"
	"github.com/stretchr/testify/require"
)

func TestWikiNodes_UpsertAndGet(t *testing.T) {
	d := openTestDB(t)
	r := NewWikiNodes(d)
	require.NoError(t, r.Upsert(WikiNode{
		ID: "n1", Path: "/DATA", Level: "space", UpdatedAt: time.Now().UnixMilli(),
		UserNotes: "hi",
	}))
	n, err := r.Get("/DATA")
	require.NoError(t, err)
	require.NotNil(t, n)
	require.Equal(t, "hi", n.UserNotes)

	// Idempotent upsert (different ID — should NOT create a duplicate row, path is UNIQUE)
	require.NoError(t, r.Upsert(WikiNode{ID: "n1", Path: "/DATA", Level: "space", UserNotes: "bye"}))
	n2, _ := r.Get("/DATA")
	require.Equal(t, "bye", n2.UserNotes)
}

func TestWikiNodes_DirtyAndFlush(t *testing.T) {
	d := openTestDB(t)
	r := NewWikiNodes(d)
	_ = r.Upsert(WikiNode{ID: "n", Path: "/X", Level: "project", UpdatedAt: 1})
	require.NoError(t, r.SetDirty("/X", true))
	dirty, _ := r.ListDirty(10)
	require.Len(t, dirty, 1)
	require.NoError(t, r.RecordFlush("/X", "abc", 100, 100))
	dirty2, _ := r.ListDirty(10)
	require.Len(t, dirty2, 0)
}

func TestWikiNodes_RewritePathPrefix_CaseSensitive(t *testing.T) {
	d := openTestDB(t)
	r := NewWikiNodes(d)
	for _, p := range []string{"/DATA/Project", "/DATA/Project/sub", "/DATA/project"} {
		require.NoError(t, r.Upsert(WikiNode{ID: p, Path: p, Level: "project", UpdatedAt: 1}))
	}
	tx, err := d.Begin()
	require.NoError(t, err)
	require.NoError(t, r.RewritePathPrefix(tx, "/DATA/Project", "/DATA/Renamed"))
	require.NoError(t, tx.Commit())

	got, _ := r.Get("/DATA/Renamed")
	require.NotNil(t, got, "exact match should have been renamed")
	got2, _ := r.Get("/DATA/Renamed/sub")
	require.NotNil(t, got2, "subtree should have been renamed")
	got3, _ := r.Get("/DATA/project")
	require.NotNil(t, got3, "lowercase sibling must NOT be touched")
}

func TestWikiNodes_RewritePathPrefix_EscapesUnderscore(t *testing.T) {
	d := openTestDB(t)
	r := NewWikiNodes(d)
	_ = r.Upsert(WikiNode{ID: "1", Path: "/DATA/a_b", Level: "project", UpdatedAt: 1})
	_ = r.Upsert(WikiNode{ID: "2", Path: "/DATA/aXb", Level: "project", UpdatedAt: 1})
	tx, err := d.Begin()
	require.NoError(t, err)
	require.NoError(t, r.RewritePathPrefix(tx, "/DATA/a_b", "/DATA/renamed"))
	require.NoError(t, tx.Commit())
	got, _ := r.Get("/DATA/renamed")
	require.NotNil(t, got, "/DATA/a_b should rename")
	got2, _ := r.Get("/DATA/aXb")
	require.NotNil(t, got2, "/DATA/aXb must NOT be touched (underscore escaped)")
}

func TestWikiNode_AILabel_Roundtrip(t *testing.T) {
	d := openTestDB(t)
	r := NewWikiNodes(d)
	n := WikiNode{
		ID:                 "n1",
		Path:               "/DATA/Projects/x",
		Level:              "project",
		LastModified:       1700000000000,
		UserNotes:          "hello",
		UserNotesETag:      "e1",
		UserNotesUpdatedAt: 1700000005000,
		UpdatedAt:          1700000010000,
		AILabel:            "Go 微服务项目",
	}
	if err := r.Upsert(n); err != nil {
		t.Fatal(err)
	}
	got, err := r.Get("/DATA/Projects/x")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("nil node")
	}
	if got.AILabel != "Go 微服务项目" {
		t.Fatalf("AILabel=%q want %q", got.AILabel, "Go 微服务项目")
	}
	if got.LastModified != 1700000000000 {
		t.Fatalf("LastModified roundtrip: %d", got.LastModified)
	}
}

func TestWikiNode_List_EmptyRootID_ReturnsAllNodes(t *testing.T) {
	d := openTestDB(t)
	r := NewWikiNodes(d)

	rootA := "root-a"
	rootB := "root-b"
	for _, spec := range []struct {
		id, path, level string
		root            *string
	}{
		{"id-a-1", "/A", "space", &rootA},
		{"id-a-2", "/A/proj", "project", &rootA},
		{"id-b-1", "/B", "space", &rootB},
		{"id-b-2", "/B/proj", "project", &rootB},
	} {
		if err := r.Upsert(WikiNode{
			ID: spec.id, RootID: spec.root, Path: spec.path, Level: spec.level,
			LastModified: 1, UpdatedAt: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := r.List("")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("List(\"\") len=%d want 4 (all nodes across both roots)", len(got))
	}

	// Sanity: List(rootA) still scopes correctly
	gotA, err := r.List(rootA)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotA) != 2 {
		t.Fatalf("List(rootA) len=%d want 2", len(gotA))
	}
}

func TestWikiNodes_SetDirtyAndTouch_AdvancesLastModified(t *testing.T) {
	d := openTestDB(t)
	r := NewWikiNodes(d)

	rootID := "r"
	require.NoError(t, r.Upsert(WikiNode{
		ID: "n1", RootID: &rootID, Path: "/x", Level: "project",
		LastModified: 1000, UpdatedAt: 1,
	}))
	require.NoError(t, r.SetDirtyAndTouch("/x", 5000))
	got, err := r.Get("/x")
	require.NoError(t, err)
	require.True(t, got.Dirty)
	require.Equal(t, int64(5000), got.LastModified)
}

func TestWikiNodes_SetDirtyAndTouch_DoesNotRegress(t *testing.T) {
	d := openTestDB(t)
	r := NewWikiNodes(d)

	rootID := "r"
	require.NoError(t, r.Upsert(WikiNode{
		ID: "n1", RootID: &rootID, Path: "/x", Level: "project",
		LastModified: 5000, UpdatedAt: 1,
	}))
	require.NoError(t, r.SetDirtyAndTouch("/x", 3000))
	got, _ := r.Get("/x")
	require.Equal(t, int64(5000), got.LastModified, "MAX guard must prevent regression")
}

func TestWikiNodes_SetDirtyAndTouch_HandlesNullLastModified(t *testing.T) {
	d := openTestDB(t)
	r := NewWikiNodes(d)

	rootID := "r"
	require.NoError(t, r.Upsert(WikiNode{
		ID: "n1", RootID: &rootID, Path: "/x", Level: "project", UpdatedAt: 1,
	}))
	require.NoError(t, r.SetDirtyAndTouch("/x", 2000))
	got, _ := r.Get("/x")
	require.Equal(t, int64(2000), got.LastModified)
}

func TestWikiNodes_SetChildCount(t *testing.T) {
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	defer d.Close()
	r := NewWikiNodes(d)

	rootID := "r"
	require.NoError(t, r.Upsert(WikiNode{
		ID: "n1", RootID: &rootID, Path: "/x", Level: "project",
		ChildCount: 0, UpdatedAt: 1,
	}))
	require.NoError(t, r.SetChildCount("/x", 7))

	got, _ := r.Get("/x")
	require.Equal(t, 7, got.ChildCount)
}

func TestWikiNodes_SetChildCount_DoesNotChangeOtherFields(t *testing.T) {
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	defer d.Close()
	r := NewWikiNodes(d)

	rootID := "r"
	require.NoError(t, r.Upsert(WikiNode{
		ID: "n1", RootID: &rootID, Path: "/x", Level: "project",
		AILabel: "标签", LastModified: 5000, UpdatedAt: 100,
	}))
	require.NoError(t, r.SetDirty("/x", true))
	require.NoError(t, r.SetChildCount("/x", 3))

	got, _ := r.Get("/x")
	require.Equal(t, 3, got.ChildCount)
	require.Equal(t, "标签", got.AILabel, "ai_label must not change")
	require.Equal(t, int64(5000), got.LastModified, "last_modified must not change")
	require.True(t, got.Dirty, "dirty must not be cleared by SetChildCount")
}

func TestWikiNode_List_ReturnsAILabelAndTimestamps(t *testing.T) {
	d := openTestDB(t)
	r := NewWikiNodes(d)
	root := "rootA"
	for _, p := range []string{"/DATA", "/DATA/Projects/x", "/DATA/Projects/y"} {
		n := WikiNode{
			ID:                 "id-" + p,
			RootID:             &root,
			Path:               p,
			Level:              "project",
			LastModified:       1700000000000,
			UserNotesUpdatedAt: 1700000005000,
			UpdatedAt:          1700000010000,
			AILabel:            "label-" + p,
		}
		if err := r.Upsert(n); err != nil {
			t.Fatal(err)
		}
	}
	got, err := r.List(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("List len=%d want 3", len(got))
	}
	for _, n := range got {
		if n.AILabel != "label-"+n.Path {
			t.Errorf("AILabel=%q for path=%q", n.AILabel, n.Path)
		}
		if n.LastModified != 1700000000000 {
			t.Errorf("LastModified=%d for path=%q", n.LastModified, n.Path)
		}
	}
}
