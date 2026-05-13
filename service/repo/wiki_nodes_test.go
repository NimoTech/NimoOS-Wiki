package repo

import (
	"testing"
	"time"

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
