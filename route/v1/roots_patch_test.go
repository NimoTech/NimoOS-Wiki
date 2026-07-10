package v1

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NimoTech/NimoOS-Wiki/service/repo"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

func TestPatchRoot_TogglesEnabled(t *testing.T) {
	_, deps := setupInternalTest(t)
	require.NoError(t, deps.WikiRoots.Insert(repo.WikiRoot{
		ID: "r1", Path: "/data/a", Level: "space", WatchMode: "auto",
		StorageMode: "inline", Enabled: true, ScanIntervalS: 21600,
		CreatedAt: 1, LastScanAt: 5,
	}))

	e := echo.New()
	e.PATCH("/v1/wiki/roots/:id", patchRoot(deps))

	req := httptest.NewRequest(http.MethodPatch, "/v1/wiki/roots/r1",
		strings.NewReader(`{"enabled": false}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code)

	got, err := deps.WikiRoots.Get("r1")
	require.NoError(t, err)
	require.False(t, got.Enabled)
}

func TestPatchRoot_NotFoundAndBadBody(t *testing.T) {
	_, deps := setupInternalTest(t)
	e := echo.New()
	e.PATCH("/v1/wiki/roots/:id", patchRoot(deps))

	req := httptest.NewRequest(http.MethodPatch, "/v1/wiki/roots/nope",
		strings.NewReader(`{"enabled": true}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)

	req = httptest.NewRequest(http.MethodPatch, "/v1/wiki/roots/nope",
		strings.NewReader(`{}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}
