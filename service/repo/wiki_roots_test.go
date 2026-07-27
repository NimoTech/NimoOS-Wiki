package repo

import (
	"database/sql"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Wiki/pkg/db"
	"github.com/stretchr/testify/require"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestWikiRootsCRUD(t *testing.T) {
	d := openTestDB(t)
	r := NewWikiRoots(d)
	now := time.Now().UnixMilli()
	require.NoError(t, r.Insert(WikiRoot{
		ID: "id1", Path: "/DATA", Level: "space", WatchMode: "auto",
		StorageMode: "inline", Enabled: true, ScanIntervalS: 600, CreatedAt: now,
	}))
	g, err := r.Get("id1")
	require.NoError(t, err)
	require.Equal(t, "/DATA", g.Path)
	require.True(t, g.Enabled)
	all, err := r.List()
	require.NoError(t, err)
	require.Len(t, all, 1)
	require.NoError(t, r.SetEnabled("id1", false))
	g2, _ := r.Get("id1")
	require.False(t, g2.Enabled)
	require.NoError(t, r.Delete("id1"))
	_, err = r.Get("id1")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestSetNeedsReconcileRoundtrip(t *testing.T) {
	d := openTestDB(t)
	r := NewWikiRoots(d)
	now := time.Now().UnixMilli()
	require.NoError(t, r.Insert(WikiRoot{
		ID: "id2", Path: "/DATA2", Level: "space", WatchMode: "auto",
		StorageMode: "inline", Enabled: true, ScanIntervalS: 600, CreatedAt: now,
	}))
	g, err := r.Get("id2")
	require.NoError(t, err)
	require.False(t, g.NeedsReconcile)

	require.NoError(t, r.SetNeedsReconcile("id2", true))
	g, err = r.Get("id2")
	require.NoError(t, err)
	require.True(t, g.NeedsReconcile)

	require.NoError(t, r.SetNeedsReconcile("id2", false))
	g, err = r.Get("id2")
	require.NoError(t, err)
	require.False(t, g.NeedsReconcile)
}

func TestSetNeedsAuthzPushRoundtrip(t *testing.T) {
	d := openTestDB(t)
	r := NewWikiRoots(d)
	now := time.Now().UnixMilli()
	require.NoError(t, r.Insert(WikiRoot{
		ID: "id4", Path: "/DATA4", Level: "space", WatchMode: "auto",
		StorageMode: "inline", Enabled: true, ScanIntervalS: 600, CreatedAt: now,
	}))
	g, err := r.Get("id4")
	require.NoError(t, err)
	require.False(t, g.NeedsAuthzPush)

	require.NoError(t, r.SetNeedsAuthzPush("id4", true))
	g, err = r.Get("id4")
	require.NoError(t, err)
	require.True(t, g.NeedsAuthzPush)

	require.NoError(t, r.SetNeedsAuthzPush("id4", false))
	g, err = r.Get("id4")
	require.NoError(t, err)
	require.False(t, g.NeedsAuthzPush)
}

func TestHasNeedsAuthzPush(t *testing.T) {
	d := openTestDB(t)
	r := NewWikiRoots(d)
	now := time.Now().UnixMilli()
	require.NoError(t, r.Insert(WikiRoot{
		ID: "id5", Path: "/DATA5", Level: "space", WatchMode: "auto",
		StorageMode: "inline", Enabled: true, ScanIntervalS: 600, CreatedAt: now,
	}))

	pending, err := r.HasNeedsAuthzPush()
	require.NoError(t, err)
	require.False(t, pending)

	require.NoError(t, r.SetNeedsAuthzPush("id5", true))
	pending, err = r.HasNeedsAuthzPush()
	require.NoError(t, err)
	require.True(t, pending)
}

func TestClearAllNeedsAuthzPush(t *testing.T) {
	d := openTestDB(t)
	r := NewWikiRoots(d)
	now := time.Now().UnixMilli()
	require.NoError(t, r.Insert(WikiRoot{
		ID: "id6", Path: "/DATA6", Level: "space", WatchMode: "auto",
		StorageMode: "inline", Enabled: true, ScanIntervalS: 600, CreatedAt: now,
	}))
	require.NoError(t, r.Insert(WikiRoot{
		ID: "id7", Path: "/DATA7", Level: "space", WatchMode: "auto",
		StorageMode: "inline", Enabled: true, ScanIntervalS: 600, CreatedAt: now,
	}))
	require.NoError(t, r.SetNeedsAuthzPush("id6", true))
	require.NoError(t, r.SetNeedsAuthzPush("id7", true))

	require.NoError(t, r.ClearAllNeedsAuthzPush())

	pending, err := r.HasNeedsAuthzPush()
	require.NoError(t, err)
	require.False(t, pending)
	g6, err := r.Get("id6")
	require.NoError(t, err)
	require.False(t, g6.NeedsAuthzPush)
	g7, err := r.Get("id7")
	require.NoError(t, err)
	require.False(t, g7.NeedsAuthzPush)
}

func TestSetWatchMode(t *testing.T) {
	d := openTestDB(t)
	r := NewWikiRoots(d)
	now := time.Now().UnixMilli()
	require.NoError(t, r.Insert(WikiRoot{
		ID: "id3", Path: "/DATA3", Level: "space", WatchMode: "auto",
		StorageMode: "inline", Enabled: true, ScanIntervalS: 600, CreatedAt: now,
	}))
	require.NoError(t, r.SetWatchMode("id3", "scan_only"))
	g, err := r.Get("id3")
	require.NoError(t, err)
	require.Equal(t, "scan_only", g.WatchMode)

	require.NoError(t, r.SetWatchMode("id3", "auto"))
	g, err = r.Get("id3")
	require.NoError(t, err)
	require.Equal(t, "auto", g.WatchMode)
}
