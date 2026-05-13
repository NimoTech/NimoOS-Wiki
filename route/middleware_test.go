package route

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

func TestLocalhostOnly_Localhost_Allowed(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	called := false
	h := LocalhostOnly(func(c echo.Context) error { called = true; return c.NoContent(http.StatusOK) })
	require.NoError(t, h(c))
	require.True(t, called)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestLocalhostOnly_RemoteHost_Forbidden(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = "10.0.0.1:54321"
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	h := LocalhostOnly(func(c echo.Context) error { return c.NoContent(http.StatusOK) })
	err := h(c)
	require.Error(t, err)
	if he, ok := err.(*echo.HTTPError); ok {
		require.Equal(t, http.StatusForbidden, he.Code)
	}
}

func TestLocalhostOnly_IPv6_Allowed(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = "[::1]:54321"
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	h := LocalhostOnly(func(c echo.Context) error { return c.NoContent(http.StatusOK) })
	require.NoError(t, h(c))
}
