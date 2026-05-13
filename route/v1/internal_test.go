package v1

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

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
