package route

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	echo_middleware "github.com/labstack/echo/v4/middleware"
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

func TestJWT_LocalhostExempt(t *testing.T) {
	tmp := t.TempDir()
	cfg := JWTConfig(tmp)

	e := echo.New()
	e.Use(echo_middleware.JWTWithConfig(cfg))
	e.GET("/x", func(c echo.Context) error { return c.NoContent(204) })

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	// No Authorization header — pure localhost no-JWT request
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != 204 {
		t.Fatalf("localhost exempt path code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestJWT_LocalhostPassesThroughUserIDHeader(t *testing.T) {
	// Identity convention in this repo: written to / read from the request
	// header (matches what ParseTokenFunc does on the JWT path). On localhost
	// we trust whatever the in-host caller supplied — no extra copying needed,
	// the header is already on c.Request().Header.
	tmp := t.TempDir()
	cfg := JWTConfig(tmp)

	e := echo.New()
	e.Use(echo_middleware.JWTWithConfig(cfg))
	var seenUserID string
	e.GET("/x", func(c echo.Context) error {
		seenUserID = c.Request().Header.Get("X-NimoOS-User-ID")
		return c.NoContent(204)
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-NimoOS-User-ID", "42")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != 204 {
		t.Fatalf("code=%d", rec.Code)
	}
	if seenUserID != "42" {
		t.Fatalf("seenUserID=%q want 42", seenUserID)
	}
}

func TestJWT_NonLocalhostStillRequiresJWT(t *testing.T) {
	tmp := t.TempDir()
	cfg := JWTConfig(tmp)

	e := echo.New()
	e.Use(echo_middleware.JWTWithConfig(cfg))
	e.GET("/x", func(c echo.Context) error { return c.NoContent(204) })

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = "10.0.0.5:33333"
	// No Authorization header — non-localhost, no JWT
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	// The empty JWKS dir means the JWT validator can't load the key, but
	// ParseTokenFunc returns echo.ErrUnauthorized on any validation failure,
	// so we expect 401 regardless of key-loading outcome.
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("non-localhost code=%d want 401", rec.Code)
	}
}
