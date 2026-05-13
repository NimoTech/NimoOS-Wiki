package v1

import (
	"bytes"
	"encoding/json"
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
		Roots:     roots.NewManager(repo.NewWikiRoots(d), repo.NewWikiNodes(d)),
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
