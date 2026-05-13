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
