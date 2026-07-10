package v1

import (
	"errors"
	"net/http"

	"github.com/NimoTech/NimoOS-Wiki/service/repo"
	"github.com/NimoTech/NimoOS-Wiki/service/roots"
	"github.com/labstack/echo/v4"
)

func listRoots(d Deps) echo.HandlerFunc {
	return func(c echo.Context) error {
		all, err := d.WikiRoots.List()
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		return c.JSON(http.StatusOK, all)
	}
}

func createRoot(d Deps) echo.HandlerFunc {
	return func(c echo.Context) error {
		var body roots.CreateArgs
		if err := c.Bind(&body); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		id, err := d.Roots.Create(body)
		switch {
		case errors.Is(err, roots.ErrPathNotWritable):
			return echo.NewHTTPError(http.StatusConflict, err.Error())
		case errors.Is(err, roots.ErrPathNotExist):
			return echo.NewHTTPError(http.StatusNotFound, err.Error())
		case errors.Is(err, roots.ErrInvalidArgs):
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		case err != nil:
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		return c.JSON(http.StatusCreated, map[string]string{"id": id})
	}
}

func deleteRoot(d Deps) echo.HandlerFunc {
	return func(c echo.Context) error {
		id := c.Param("id")
		purge := c.QueryParam("purge_files") == "true"
		if err := d.Roots.Delete(id, purge); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		return c.NoContent(http.StatusNoContent)
	}
}

func rescanRoot(d Deps) echo.HandlerFunc {
	return func(c echo.Context) error {
		id := c.Param("id")
		if err := d.Roots.Rescan(id); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		return c.NoContent(http.StatusAccepted)
	}
}

func listCandidates(d Deps) echo.HandlerFunc {
	return func(c echo.Context) error {
		cands, err := roots.QueryLocalStorage(d.RuntimePath)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		if cands == nil {
			cands = []roots.Candidate{}
		}
		return c.JSON(http.StatusOK, cands)
	}
}

func patchRoot(d Deps) echo.HandlerFunc {
	return func(c echo.Context) error {
		var body struct {
			Enabled *bool `json:"enabled"`
		}
		if err := c.Bind(&body); err != nil || body.Enabled == nil {
			return echo.NewHTTPError(http.StatusBadRequest, `body must be {"enabled": <bool>}`)
		}
		err := d.Roots.SetEnabled(c.Param("id"), *body.Enabled)
		switch {
		case errors.Is(err, repo.ErrNotFound):
			return echo.NewHTTPError(http.StatusNotFound, err.Error())
		case err != nil:
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		return c.NoContent(http.StatusNoContent)
	}
}
