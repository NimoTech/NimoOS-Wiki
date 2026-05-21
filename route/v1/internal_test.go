package v1

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/NimoTech/NimoOS-Wiki/pkg/db"
	"github.com/NimoTech/NimoOS-Wiki/service/repo"
	"github.com/NimoTech/NimoOS-Wiki/service/roots"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

func setupInternalTest(t *testing.T) (*sql.DB, Deps) {
	t.Helper()
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	return d, Deps{
		WikiRoots: repo.NewWikiRoots(d),
		Nodes:     repo.NewWikiNodes(d),
		Files:     repo.NewFileIndex(d),
		Events:    repo.NewFileEvents(d),
		Summaries: repo.NewWikiSummaries(d),
		Roots:     roots.NewManager(repo.NewWikiRoots(d), repo.NewWikiNodes(d), nil),
	}
}

func TestInternalStub_Returns503(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/wiki/_internal/needs-summary", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	err := stubServiceUnavailable(c)
	require.Error(t, err)
	he, _ := err.(*echo.HTTPError)
	require.Equal(t, http.StatusServiceUnavailable, he.Code)
}

func TestNeedsSummary_ReturnsUnsummarizedNodes(t *testing.T) {
	_, mgrs := setupInternalTest(t)
	rootID := "r"
	require.NoError(t, mgrs.Nodes.Upsert(repo.WikiNode{
		ID: "n1", RootID: &rootID, Path: "/a", Level: "project",
		AILabel: "", LastModified: 100, UpdatedAt: 1, ChildCount: 5,
	}))
	require.NoError(t, mgrs.Nodes.Upsert(repo.WikiNode{
		ID: "n2", RootID: &rootID, Path: "/b", Level: "project",
		AILabel: "已生成", LastModified: 200, UpdatedAt: 1, ChildCount: 3,
	}))
	require.NoError(t, mgrs.Summaries.Upsert(repo.WikiSummary{
		Path: "/b", Summary: "x", GeneratedAt: 200,
		BasedOnLastModified: 200, GeneratorVersion: "v0",
	}))

	req := httptest.NewRequest("GET", "/v1/wiki/_internal/needs-summary?limit=10", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	InitRouter(mgrs).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Nodes []repo.NeedsSummaryRow `json:"nodes"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Nodes, 1)
	require.Equal(t, "/a", body.Nodes[0].Path)
	require.Equal(t, int64(100), body.Nodes[0].LastModifiedMs)
	require.Equal(t, 5, body.Nodes[0].ChildCount)
}

func TestNeedsSummary_EmptyQueue(t *testing.T) {
	_, mgrs := setupInternalTest(t)
	req := httptest.NewRequest("GET", "/v1/wiki/_internal/needs-summary", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	InitRouter(mgrs).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"nodes": []}`, rec.Body.String())
}
