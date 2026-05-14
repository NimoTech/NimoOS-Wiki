package v1

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"
)

type nodeResponse struct {
	Path          string            `json:"path"`
	Level         string            `json:"level"`
	AILabel       string            `json:"ai_label"`
	Summary       *string           `json:"summary"`
	ChildMap      []nodeChildEntry  `json:"child_map"`
	KeySources    []string          `json:"key_sources"`
	RecentChanges []nodeRecentEntry `json:"recent_changes"`
	PendingCount  int               `json:"pending_count"`
	UserNotes     string            `json:"user_notes"`
	ParentWiki    string            `json:"parent_wiki,omitempty"`
	Subwikis      []string          `json:"subwikis"`
	ETag          string            `json:"etag"`
}

type nodeChildEntry struct {
	Name           string `json:"name"`
	FileCount      int    `json:"file_count,omitempty"`
	LastModifiedMS int64  `json:"last_modified_ms,omitempty"`
	IsOpaque       bool   `json:"is_opaque,omitempty"`
}

type nodeRecentEntry struct {
	Path string `json:"path"`
	Op   string `json:"op"`
	AtMS int64  `json:"at_ms"`
}

func getNode(d Deps) echo.HandlerFunc {
	return func(c echo.Context) error {
		path := c.QueryParam("path")
		if path == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "missing path")
		}
		node, err := d.Nodes.Get(path)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		if node == nil {
			return echo.NewHTTPError(http.StatusNotFound, "node not found")
		}

		rootID := ""
		if node.RootID != nil {
			rootID = *node.RootID
		}

		children, _ := d.Files.ListByParent(rootID, node.Path)
		cms := make([]nodeChildEntry, 0, len(children))
		for _, ch := range children {
			cms = append(cms, nodeChildEntry{
				Name:           filepath.Base(ch.Path),
				LastModifiedMS: ch.Mtime,
				IsOpaque:       ch.IsOpaque,
			})
		}

		evs, _ := d.Events.RecentForRoot(rootID, 20)
		recents := make([]nodeRecentEntry, 0, len(evs))
		for _, e := range evs {
			recents = append(recents, nodeRecentEntry{Path: e.Path, Op: e.Op, AtMS: e.DetectedAt})
		}

		subwikis, _ := d.Nodes.ListSubwikis(node.Path)
		if subwikis == nil {
			subwikis = []string{}
		}

		return c.JSON(http.StatusOK, nodeResponse{
			Path:          node.Path,
			Level:         node.Level,
			AILabel:       node.AILabel,
			Summary:       nil,
			ChildMap:      cms,
			KeySources:    []string{},
			RecentChanges: recents,
			PendingCount:  0,
			UserNotes:     node.UserNotes,
			Subwikis:      subwikis,
			ETag:          node.UserNotesETag,
		})
	}
}

func getTree(d Deps) echo.HandlerFunc {
	return func(c echo.Context) error {
		rootID := c.QueryParam("root_id")
		nodes, err := d.Nodes.List(rootID)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		type sk struct {
			Path               string `json:"path"`
			Level              string `json:"level"`
			AILabel            string `json:"ai_label"`
			UserNotesUpdatedAt int64  `json:"user_notes_updated_at"`
			LastModifiedMS     int64  `json:"last_modified_ms"`
		}
		out := make([]sk, 0, len(nodes))
		for _, n := range nodes {
			out = append(out, sk{
				Path:               n.Path,
				Level:              n.Level,
				AILabel:            n.AILabel,
				UserNotesUpdatedAt: n.UserNotesUpdatedAt,
				LastModifiedMS:     n.LastModified,
			})
		}
		return c.JSON(http.StatusOK, out)
	}
}

func getRaw(d Deps) echo.HandlerFunc {
	return func(c echo.Context) error {
		path := c.QueryParam("path")
		if path == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "missing path")
		}
		f, err := os.Open(filepath.Join(path, ".wiki.md"))
		if err != nil {
			if os.IsNotExist(err) {
				return echo.NewHTTPError(http.StatusNotFound, ".wiki.md not found")
			}
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		defer f.Close()
		c.Response().Header().Set(echo.HeaderContentType, "text/markdown; charset=utf-8")
		_, err = io.Copy(c.Response().Writer, f)
		return err
	}
}

func putUserNotes(d Deps) echo.HandlerFunc {
	return func(c echo.Context) error {
		path := c.QueryParam("path")
		if path == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "missing path")
		}
		ifMatch := c.Request().Header.Get("If-Match")

		node, err := d.Nodes.Get(path)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		if node == nil {
			return echo.NewHTTPError(http.StatusNotFound, "node not found")
		}
		if ifMatch != "" && ifMatch != node.UserNotesETag {
			return echo.NewHTTPError(http.StatusConflict, "etag mismatch")
		}

		body, err := io.ReadAll(c.Request().Body)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		notes := string(body)
		h := sha256.Sum256(body)
		newETag := hex.EncodeToString(h[:8])

		now := time.Now().UnixMilli()
		if err := d.Nodes.SetUserNotes(path, notes, newETag, now); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		if err := d.Nodes.SetDirty(path, true); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]string{"etag": newETag})
	}
}

func getRecentChanges(d Deps) echo.HandlerFunc {
	return func(c echo.Context) error {
		rootID := c.QueryParam("root_id")
		sinceMs, _ := strconv.ParseInt(c.QueryParam("since_ms"), 10, 64)
		limit, _ := strconv.Atoi(c.QueryParam("limit"))
		if limit < 1 {
			limit = 50
		}
		if limit > 200 {
			limit = 200
		}
		evs, err := d.Events.ListSince(rootID, sinceMs, limit)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		return c.JSON(http.StatusOK, evs)
	}
}
