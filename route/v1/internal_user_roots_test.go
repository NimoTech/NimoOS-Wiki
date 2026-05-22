package v1

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/NimoTech/NimoOS-Wiki/service/repo"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

func TestUserRootsReturnsEnabledOnly(t *testing.T) {
	_, deps := setupInternalTest(t)

	// Seed 2 enabled + 1 disabled
	require.NoError(t, deps.WikiRoots.Insert(repo.WikiRoot{
		ID: "r1", Path: "/data/a", Level: "project", WatchMode: "polling",
		StorageMode: "physical", Enabled: true, ScanIntervalS: 0,
		CreatedAt: 1, LastScanAt: 0,
	}))
	require.NoError(t, deps.WikiRoots.Insert(repo.WikiRoot{
		ID: "r2", Path: "/data/b", Level: "project", WatchMode: "polling",
		StorageMode: "physical", Enabled: true, ScanIntervalS: 0,
		CreatedAt: 1, LastScanAt: 0,
	}))
	require.NoError(t, deps.WikiRoots.Insert(repo.WikiRoot{
		ID: "r3", Path: "/data/c", Level: "project", WatchMode: "polling",
		StorageMode: "physical", Enabled: false, ScanIntervalS: 0,
		CreatedAt: 1, LastScanAt: 0,
	}))

	e := echo.New()
	g := e.Group("/v1/wiki")
	g.GET("/_internal/user-roots", getInternalUserRoots(deps))

	req := httptest.NewRequest(http.MethodGet,
		"/v1/wiki/_internal/user-roots?user_id=u123", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var body struct {
		RootIDs []string `json:"root_ids"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	require.ElementsMatch(t, []string{"r1", "r2"}, body.RootIDs)
}
