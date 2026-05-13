package repo

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFileIndex_RewriteAndCaseSensitivity(t *testing.T) {
	d := openTestDB(t)
	r := NewFileIndex(d)
	for _, e := range []FileIndex{
		{ID: "1", RootID: "r", Path: "/DATA/ProjectA", Parent: "/DATA", IsDir: true, Status: "present"},
		{ID: "2", RootID: "r", Path: "/DATA/ProjectA/x.txt", Parent: "/DATA/ProjectA", Status: "present"},
		{ID: "3", RootID: "r", Path: "/DATA/projecta/y.txt", Parent: "/DATA/projecta", Status: "present"},
	} {
		require.NoError(t, r.Upsert(e))
	}
	tx, err := d.Begin()
	require.NoError(t, err)
	require.NoError(t, r.RewritePathPrefix(tx, "r", "/DATA/ProjectA", "/DATA/Renamed"))
	require.NoError(t, tx.Commit())

	got, _ := r.Get("r", "/DATA/Renamed/x.txt")
	require.NotNil(t, got)
	require.Equal(t, "/DATA/Renamed", got.Parent)
	got2, _ := r.Get("r", "/DATA/projecta/y.txt")
	require.NotNil(t, got2, "lowercase must not be touched")
	require.Equal(t, "/DATA/projecta", got2.Parent)
}
