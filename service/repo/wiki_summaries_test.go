package repo

import (
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Wiki/pkg/db"
	"github.com/stretchr/testify/require"
)

func summariesSetup(t *testing.T) (*WikiSummariesRepo, *WikiNodesRepo) {
	t.Helper()
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	return NewWikiSummaries(d), NewWikiNodes(d)
}

func TestSummaries_GetMissing(t *testing.T) {
	s, _ := summariesSetup(t)
	got, err := s.Get("/no/such/path")
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestSummaries_UpsertAndGet(t *testing.T) {
	s, n := summariesSetup(t)
	rootID := "r"
	require.NoError(t, n.Upsert(WikiNode{
		ID: "n1", RootID: &rootID, Path: "/x", Level: "project", UpdatedAt: 1,
	}))
	now := time.Now().UnixMilli()
	require.NoError(t, s.Upsert(WikiSummary{
		Path: "/x", Summary: "hello", GeneratedAt: now,
		BasedOnLastModified: now - 1000, GeneratorVersion: "v0",
	}))
	got, err := s.Get("/x")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "hello", got.Summary)
	require.Equal(t, now-1000, got.BasedOnLastModified)

	require.NoError(t, s.Upsert(WikiSummary{
		Path: "/x", Summary: "world", GeneratedAt: now + 1,
		BasedOnLastModified: now, GeneratorVersion: "v1",
	}))
	got, _ = s.Get("/x")
	require.Equal(t, "world", got.Summary)
	require.Equal(t, "v1", got.GeneratorVersion)
}

func TestSummaries_ListNeedsSummary_AILabelEmpty(t *testing.T) {
	s, n := summariesSetup(t)
	rootID := "r"
	require.NoError(t, n.Upsert(WikiNode{
		ID: "n1", RootID: &rootID, Path: "/empty", Level: "project",
		AILabel: "", LastModified: 100, UpdatedAt: 1,
	}))
	require.NoError(t, n.Upsert(WikiNode{
		ID: "n2", RootID: &rootID, Path: "/labeled", Level: "project",
		AILabel: "已生成", LastModified: 50, UpdatedAt: 1,
	}))
	require.NoError(t, s.Upsert(WikiSummary{
		Path: "/empty", Summary: "x", GeneratedAt: 100,
		BasedOnLastModified: 100, GeneratorVersion: "v0",
	}))
	require.NoError(t, s.Upsert(WikiSummary{
		Path: "/labeled", Summary: "y", GeneratedAt: 50,
		BasedOnLastModified: 50, GeneratorVersion: "v0",
	}))

	rows, err := s.ListNeedsSummary(10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "/empty", rows[0].Path)
}

func TestSummaries_ListNeedsSummary_StaleByLastModified(t *testing.T) {
	s, n := summariesSetup(t)
	rootID := "r"
	require.NoError(t, n.Upsert(WikiNode{
		ID: "n1", RootID: &rootID, Path: "/stale", Level: "project",
		AILabel: "已生成", LastModified: 200, UpdatedAt: 1,
	}))
	require.NoError(t, s.Upsert(WikiSummary{
		Path: "/stale", Summary: "x", GeneratedAt: 100,
		BasedOnLastModified: 100, GeneratorVersion: "v0",
	}))
	rows, err := s.ListNeedsSummary(10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "/stale", rows[0].Path)
	require.Equal(t, int64(200), rows[0].LastModifiedMs)
}

func TestSummaries_ListNeedsSummary_NoSummary(t *testing.T) {
	s, n := summariesSetup(t)
	rootID := "r"
	require.NoError(t, n.Upsert(WikiNode{
		ID: "n1", RootID: &rootID, Path: "/fresh", Level: "project",
		AILabel: "已生成", LastModified: 200, UpdatedAt: 1,
	}))
	// node exists, no summary row → based_on_last_modified IS NULL → in queue
	rows, err := s.ListNeedsSummary(10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "/fresh", rows[0].Path)
}

func TestSummaries_ListNeedsSummary_OrderedByLastModifiedDesc(t *testing.T) {
	s, n := summariesSetup(t)
	rootID := "r"
	type row struct {
		path string
		lm   int64
	}
	for _, p := range []row{{"/old", 100}, {"/new", 300}, {"/mid", 200}} {
		require.NoError(t, n.Upsert(WikiNode{
			ID: "n_" + p.path, RootID: &rootID, Path: p.path,
			Level: "project", AILabel: "", LastModified: p.lm, UpdatedAt: 1,
		}))
	}
	rows, err := s.ListNeedsSummary(10)
	require.NoError(t, err)
	require.Len(t, rows, 3)
	require.Equal(t, "/new", rows[0].Path)
	require.Equal(t, "/mid", rows[1].Path)
	require.Equal(t, "/old", rows[2].Path)
}
