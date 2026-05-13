package v1

import (
	"net/http"
	"strconv"

	"github.com/NimoTech/NimoOS-Wiki/service/repo"
	"github.com/labstack/echo/v4"
)

// stubServiceUnavailable returns 503 for endpoints reserved for the future
// Parser/Summary worker — they exist in the route table so the worker has
// a stable contract, but the implementation has not landed yet.
func stubServiceUnavailable(c echo.Context) error {
	return echo.NewHTTPError(http.StatusServiceUnavailable,
		"endpoint not yet implemented; reserved for future Parser/Summary worker")
}

func getInternalFileEvents(d Deps) echo.HandlerFunc {
	return func(c echo.Context) error {
		rootID := c.QueryParam("root_id")
		sinceMs, _ := strconv.ParseInt(c.QueryParam("since"), 10, 64)
		limit, _ := strconv.Atoi(c.QueryParam("limit"))
		if limit <= 0 || limit > 1000 {
			limit = 100
		}
		evs, err := d.Events.ListSince(rootID, sinceMs, limit)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		if evs == nil {
			evs = []repo.FileEvent{}
		}
		return c.JSON(http.StatusOK, map[string]any{"events": evs})
	}
}
