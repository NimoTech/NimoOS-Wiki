package v1

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NimoTech/NimoOS-Wiki/pkg/db"
	"github.com/NimoTech/NimoOS-Wiki/service/repo"
	"github.com/NimoTech/NimoOS-Wiki/service/roots"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

func setupTestRouter(t *testing.T) (Deps, *echo.Echo) {
	t.Helper()
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	dep := Deps{
		Roots:     roots.NewManager(repo.NewWikiRoots(d), repo.NewWikiNodes(d), nil),
		WikiRoots: repo.NewWikiRoots(d),
		Nodes:     repo.NewWikiNodes(d),
		Files:     repo.NewFileIndex(d),
		Events:    repo.NewFileEvents(d),
	}
	// Bypass JWT for tests: build a minimal echo with the public group sans JWT.
	e := echo.New()
	e.HideBanner = true
	g := e.Group("/v1/wiki")
	g.GET("/roots", listRoots(dep))
	g.POST("/roots", createRoot(dep))
	g.DELETE("/roots/:id", deleteRoot(dep))
	g.GET("/node", getNode(dep))
	g.PUT("/user-notes", putUserNotes(dep))
	return dep, e
}

func TestCreateRoot_ReturnsCreated(t *testing.T) {
	_, e := setupTestRouter(t)
	tmp := t.TempDir()
	body := `{"Path":"` + tmp + `","Level":"space"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/wiki/roots", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code)
	var resp map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotEmpty(t, resp["id"])
}

func TestCreateRoot_NonexistentPath404(t *testing.T) {
	_, e := setupTestRouter(t)
	body := `{"Path":"/nope-12345","Level":"space"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/wiki/roots", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestPutUserNotes_ETagOptimisticLock(t *testing.T) {
	dep, e := setupTestRouter(t)
	tmp := t.TempDir()
	// Seed node
	require.NoError(t, dep.Nodes.Upsert(repo.WikiNode{
		ID: "n", Path: tmp, Level: "space", UserNotesETag: "abc123",
		UpdatedAt: 1,
	}))

	// First PUT with no If-Match — should succeed and return new etag.
	req := httptest.NewRequest(http.MethodPut, "/v1/wiki/user-notes?path="+tmp, bytes.NewReader([]byte("first")))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	newETag := resp["etag"]
	require.NotEmpty(t, newETag)

	// PUT with stale (old "abc123") If-Match → 409.
	req2 := httptest.NewRequest(http.MethodPut, "/v1/wiki/user-notes?path="+tmp, bytes.NewReader([]byte("second")))
	req2.Header.Set("If-Match", "abc123")
	rec2 := httptest.NewRecorder()
	e.ServeHTTP(rec2, req2)
	require.Equal(t, http.StatusConflict, rec2.Code)

	// PUT with correct If-Match → 200.
	req3 := httptest.NewRequest(http.MethodPut, "/v1/wiki/user-notes?path="+tmp, bytes.NewReader([]byte("third")))
	req3.Header.Set("If-Match", newETag)
	rec3 := httptest.NewRecorder()
	e.ServeHTTP(rec3, req3)
	require.Equal(t, http.StatusOK, rec3.Code)
}

func TestGetNode_MissingReturns404(t *testing.T) {
	_, e := setupTestRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/wiki/node?path=/nope", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestGetNode_IncludesAILabel(t *testing.T) {
	dep, _ := setupTestRouter(t)
	root := "r1"
	dep.Nodes.Upsert(repo.WikiNode{
		ID: "n1", RootID: &root, Path: "/a/b", Level: "project",
		AILabel: "labeled", UpdatedAt: 1, LastModified: 1,
	})

	e := echo.New()
	g := e.Group("/v1/wiki")
	g.GET("/node", getNode(dep))

	req := httptest.NewRequest(http.MethodGet, "/v1/wiki/node?path=/a/b", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["ai_label"] != "labeled" {
		t.Fatalf("ai_label=%v want labeled", resp["ai_label"])
	}
}

func TestGetTree_IncludesAILabelAndTimestamps(t *testing.T) {
	dep, _ := setupTestRouter(t)
	root := "r1"
	dep.Nodes.Upsert(repo.WikiNode{
		ID: "n1", RootID: &root, Path: "/a", Level: "space",
		AILabel: "spaceA", LastModified: 100, UserNotesUpdatedAt: 200, UpdatedAt: 300,
	})

	e := echo.New()
	g := e.Group("/v1/wiki")
	g.GET("/tree", getTree(dep))

	req := httptest.NewRequest(http.MethodGet, "/v1/wiki/tree?root_id="+root, nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp) != 1 {
		t.Fatalf("len=%d", len(resp))
	}
	n := resp[0]
	if n["ai_label"] != "spaceA" {
		t.Errorf("ai_label=%v", n["ai_label"])
	}
	if s, _ := n["last_modified"].(string); s == "" {
		t.Errorf("last_modified empty, want formatted timestamp")
	}
	if s, _ := n["user_notes_updated_at"].(string); s == "" {
		t.Errorf("user_notes_updated_at empty, want formatted timestamp")
	}
}

func TestGetTree_NoRootID_ReturnsAllRoots(t *testing.T) {
	dep, _ := setupTestRouter(t)
	rootA := "rA"
	rootB := "rB"
	dep.Nodes.Upsert(repo.WikiNode{
		ID: "a", RootID: &rootA, Path: "/A", Level: "space",
		LastModified: 1, UpdatedAt: 1,
	})
	dep.Nodes.Upsert(repo.WikiNode{
		ID: "b", RootID: &rootB, Path: "/B", Level: "space",
		LastModified: 1, UpdatedAt: 1,
	})

	e := echo.New()
	g := e.Group("/v1/wiki")
	g.GET("/tree", getTree(dep))

	req := httptest.NewRequest(http.MethodGet, "/v1/wiki/tree", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp) != 2 {
		t.Fatalf("len=%d want 2 (both roots' nodes when no root_id given)", len(resp))
	}
}

func TestGetRecentChanges_AcceptsSinceMsAndLimit(t *testing.T) {
	dep, _ := setupTestRouter(t)
	root := "r1"
	// Seed three events at different timestamps
	for i, ts := range []int64{100, 200, 300} {
		dep.Events.Insert(repo.FileEvent{
			ID: fmt.Sprintf("e%d", i), RootID: root,
			Path: "/x", Op: "create", DetectedAt: ts,
		})
	}

	e := echo.New()
	g := e.Group("/v1/wiki")
	g.GET("/recent-changes", getRecentChanges(dep))

	// since_ms=150 → expect 2 events (ts 200, 300)
	req := httptest.NewRequest(http.MethodGet, "/v1/wiki/recent-changes?root_id="+root+"&since_ms=150&limit=10", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp []map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp) != 2 {
		t.Fatalf("len=%d want 2", len(resp))
	}

	// limit clamping: limit=500 should clamp to 200 (not error)
	req2 := httptest.NewRequest(http.MethodGet, "/v1/wiki/recent-changes?root_id="+root+"&limit=500", nil)
	rec2 := httptest.NewRecorder()
	e.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("clamp code=%d", rec2.Code)
	}
}
