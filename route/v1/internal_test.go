package v1

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
		Roots:     roots.NewManager(repo.NewWikiRoots(d), repo.NewWikiNodes(d), nil, nil, nil, nil),
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
		AILabel: "generated", LastModified: 200, UpdatedAt: 1, ChildCount: 3,
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

func TestNodeEvidence_FromFileIndex(t *testing.T) {
	_, mgrs := setupInternalTest(t)
	rootID := "r"
	require.NoError(t, mgrs.WikiRoots.Insert(repo.WikiRoot{
		ID: rootID, Path: "/root", Level: "project",
		WatchMode: "auto", StorageMode: "inline", Enabled: true,
	}))
	require.NoError(t, mgrs.Nodes.Upsert(repo.WikiNode{
		ID: "n", RootID: &rootID, Path: "/root", Level: "project", UpdatedAt: 1,
	}))
	now := time.Now().UnixMilli()
	for _, f := range []struct {
		path  string
		ext   string
		size  int64
		isDir bool
	}{
		{"/root/a.md", "md", 1024, false},
		{"/root/doc.pdf", "pdf", 500000, false},
		{"/root/IMG.jpeg", "jpeg", 3000000, false},
		{"/root/sub", "", 0, true},
	} {
		require.NoError(t, mgrs.Files.Upsert(repo.FileIndex{
			ID: repo.NewID(), RootID: rootID, Path: f.path, Parent: "/root",
			IsDir: f.isDir, Status: "present", Ext: f.ext, Size: f.size, Mtime: now,
		}))
	}

	req := httptest.NewRequest("GET",
		"/v1/wiki/_internal/node-evidence?path=/root&text_limit=10&pdf_limit=5", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	InitRouter(mgrs).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		NodePath      string           `json:"node_path"`
		ChildMap      []map[string]any `json:"child_map"`
		TextFiles     []map[string]any `json:"text_files"`
		PDFFiles      []map[string]any `json:"pdf_files"`
		SkippedSample []map[string]any `json:"skipped_sample"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "/root", body.NodePath)
	require.Len(t, body.TextFiles, 1)
	require.Equal(t, "/root/a.md", body.TextFiles[0]["path"])
	require.Len(t, body.PDFFiles, 1)
	require.Len(t, body.SkippedSample, 1)
	require.Equal(t, "image", body.SkippedSample[0]["reason"], "jpeg should be classified as image")
	require.Len(t, body.ChildMap, 4)
	names := []string{}
	for _, c := range body.ChildMap {
		names = append(names, c["name"].(string))
	}
	require.Contains(t, names, "a.md")
	require.Contains(t, names, "sub")
}

func TestNodeEvidence_BadPath(t *testing.T) {
	_, mgrs := setupInternalTest(t)
	req := httptest.NewRequest("GET",
		"/v1/wiki/_internal/node-evidence?path=/does/not/exist", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	InitRouter(mgrs).ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestPostSummary_WritesBothTablesAndDirty(t *testing.T) {
	_, mgrs := setupInternalTest(t)
	rootID := "r"
	require.NoError(t, mgrs.Nodes.Upsert(repo.WikiNode{
		ID: "n", RootID: &rootID, Path: "/x", Level: "project",
		AILabel: "", LastModified: 100, UpdatedAt: 1,
	}))

	body := `{
		"path": "/x",
		"ai_label": "AI 论文",
		"summary": "这是一个目录摘要。",
		"based_on_last_modified_ms": 100,
		"generator_version": "wiki-summary-worker/0.1.0+gpt-4o-mini"
	}`
	req := httptest.NewRequest("POST", "/v1/wiki/_internal/summary",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	InitRouter(mgrs).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	n, _ := mgrs.Nodes.Get("/x")
	require.Equal(t, "AI 论文", n.AILabel)
	require.True(t, n.Dirty, "POST /summary must set dirty=1 to trigger WikiWriter")

	s, _ := mgrs.Summaries.Get("/x")
	require.NotNil(t, s)
	require.Equal(t, "这是一个目录摘要。", s.Summary)
	require.Equal(t, int64(100), s.BasedOnLastModified)
	require.Equal(t, "wiki-summary-worker/0.1.0+gpt-4o-mini", s.GeneratorVersion)
}

func TestPostSummary_RejectsTooLongFields(t *testing.T) {
	_, mgrs := setupInternalTest(t)
	rootID := "r"
	require.NoError(t, mgrs.Nodes.Upsert(repo.WikiNode{
		ID: "n", RootID: &rootID, Path: "/x", Level: "project", UpdatedAt: 1,
	}))
	body := `{"path":"/x","ai_label":"` + strings.Repeat("X", 100) +
		`","summary":"ok","based_on_last_modified_ms":1,"generator_version":"v"}`
	req := httptest.NewRequest("POST", "/v1/wiki/_internal/summary",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	InitRouter(mgrs).ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPostSummary_404ForUnknownPath(t *testing.T) {
	_, mgrs := setupInternalTest(t)
	body := `{"path":"/nope","ai_label":"X","summary":"x","based_on_last_modified_ms":1,"generator_version":"v"}`
	req := httptest.NewRequest("POST", "/v1/wiki/_internal/summary",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	InitRouter(mgrs).ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestPostSummary_RejectsMissingBasedOn(t *testing.T) {
	_, mgrs := setupInternalTest(t)
	rootID := "r"
	require.NoError(t, mgrs.Nodes.Upsert(repo.WikiNode{
		ID: "n", RootID: &rootID, Path: "/x", Level: "project", UpdatedAt: 1,
	}))
	body := `{"path":"/x","ai_label":"X","summary":"x","generator_version":"v"}`
	req := httptest.NewRequest("POST", "/v1/wiki/_internal/summary",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	InitRouter(mgrs).ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestInternalFileEvents_ExposesArchiveHorizon(t *testing.T) {
	d, dep := setupInternalTest(t)
	_ = d
	dep.Archive = repo.NewArchiveState(90)
	dep.Archive.MarkArchived()
	e := echo.New()
	e.GET("/v1/wiki/_internal/file-events", getInternalFileEvents(dep))

	req := httptest.NewRequest(http.MethodGet, "/v1/wiki/_internal/file-events?since=0&after_seq=0&limit=10", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Events          []repo.FileEvent `json:"events"`
		ArchiveCutoffMs int64            `json:"archive_cutoff_ms"`
		HasArchived     bool             `json:"has_archived"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.True(t, body.HasArchived)
	require.InDelta(t, time.Now().Add(-90*24*time.Hour).UnixMilli(), body.ArchiveCutoffMs, 5000)
}

func TestInternalFileEvents_NoArchiveStateStillHasFields(t *testing.T) {
	_, dep := setupInternalTest(t)
	e := echo.New()
	e.GET("/v1/wiki/_internal/file-events", getInternalFileEvents(dep))
	req := httptest.NewRequest(http.MethodGet, "/v1/wiki/_internal/file-events?since=0&limit=10", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Contains(t, body, "archive_cutoff_ms")
	require.Equal(t, false, body["has_archived"])
}

func TestInternalFiles_PagesPresentFilesAndRejectsUnknownRoot(t *testing.T) {
	_, dep := setupInternalTest(t)
	rootID := "root-files"
	require.NoError(t, dep.WikiRoots.Insert(repo.WikiRoot{ID: rootID, Path: "/DATA", Level: "space", Enabled: true}))
	for _, f := range []repo.FileIndex{
		{ID: repo.NewID(), RootID: rootID, Path: "/DATA/a.md", Parent: "/DATA", Status: "present", Mtime: 111, Size: 5},
		{ID: repo.NewID(), RootID: rootID, Path: "/DATA/b.md", Parent: "/DATA", Status: "present", Mtime: 222, Size: 6},
		{ID: repo.NewID(), RootID: rootID, Path: "/DATA/dir", Parent: "/DATA", IsDir: true, Status: "present"},
	} {
		require.NoError(t, dep.Files.Upsert(f))
	}
	e := echo.New()
	e.GET("/v1/wiki/_internal/files", getInternalFiles(dep))

	req := httptest.NewRequest(http.MethodGet, "/v1/wiki/_internal/files?root_id="+rootID+"&limit=1", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var page struct {
		Files []struct {
			Path    string `json:"path"`
			MtimeMs int64  `json:"mtime_ms"`
			Size    int64  `json:"size"`
		} `json:"files"`
		NextAfter string `json:"next_after"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	require.Len(t, page.Files, 1)
	require.Equal(t, "/DATA/a.md", page.Files[0].Path)
	require.EqualValues(t, 111, page.Files[0].MtimeMs)
	require.Equal(t, "/DATA/a.md", page.NextAfter)

	req = httptest.NewRequest(http.MethodGet, "/v1/wiki/_internal/files?root_id="+rootID+"&after=/DATA/a.md&limit=10", nil)
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	require.Len(t, page.Files, 1, "dir must not appear")
	require.Equal(t, "/DATA/b.md", page.Files[0].Path)
	require.Equal(t, "", page.NextAfter, "short page ends the listing")

	req = httptest.NewRequest(http.MethodGet, "/v1/wiki/_internal/files?root_id=nope", nil)
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestInternalFiles_DBErrorIs500NotNotFound(t *testing.T) {
	d, dep := setupInternalTest(t)
	require.NoError(t, d.Close())

	e := echo.New()
	e.GET("/v1/wiki/_internal/files", getInternalFiles(dep))

	req := httptest.NewRequest(http.MethodGet, "/v1/wiki/_internal/files?root_id=any", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
}
