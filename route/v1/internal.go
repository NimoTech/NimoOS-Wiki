package v1

import (
	"net/http"
	"path/filepath"
	"strconv"
	"time"

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

type postSummaryBody struct {
	Path                  string `json:"path"`
	AILabel               string `json:"ai_label"`
	Summary               string `json:"summary"`
	BasedOnLastModifiedMs *int64 `json:"based_on_last_modified_ms"`
	GeneratorVersion      string `json:"generator_version"`
}

func postInternalSummary(d Deps) echo.HandlerFunc {
	return func(c echo.Context) error {
		var b postSummaryBody
		if err := c.Bind(&b); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid JSON: "+err.Error())
		}
		if b.Path == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "missing path")
		}
		if b.AILabel == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "missing ai_label")
		}
		if len(b.AILabel) > 80 {
			return echo.NewHTTPError(http.StatusBadRequest, "ai_label > 80 bytes")
		}
		if len(b.Summary) > 600 {
			return echo.NewHTTPError(http.StatusBadRequest, "summary > 600 bytes")
		}
		if b.BasedOnLastModifiedMs == nil {
			return echo.NewHTTPError(http.StatusBadRequest, "missing based_on_last_modified_ms")
		}

		node, err := d.Nodes.Get(b.Path)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		if node == nil {
			return echo.NewHTTPError(http.StatusNotFound, "wiki_node not found")
		}

		now := time.Now().UnixMilli()
		// Upsert the summary row FIRST so WikiWriter (triggered by the
		// dirty=1 from SetAILabel below) reads the new row when it
		// re-renders .wiki.md. If we did SetAILabel first, the Writer
		// could race ahead, read no summary, render blank, then clear
		// dirty before our Upsert lands.
		if err := d.Summaries.Upsert(repo.WikiSummary{
			Path:                b.Path,
			Summary:             b.Summary,
			GeneratedAt:         now,
			BasedOnLastModified: *b.BasedOnLastModifiedMs,
			GeneratorVersion:    b.GeneratorVersion,
		}); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		if err := d.Nodes.SetAILabel(b.Path, b.AILabel, now); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{"ok": true})
	}
}

func getInternalNeedsSummary(d Deps) echo.HandlerFunc {
	return func(c echo.Context) error {
		limit, _ := strconv.Atoi(c.QueryParam("limit"))
		if limit <= 0 {
			limit = 10
		}
		if limit > 50 {
			limit = 50
		}
		nodes, err := d.Summaries.ListNeedsSummary(limit)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		if nodes == nil {
			nodes = []repo.NeedsSummaryRow{}
		}
		return c.JSON(http.StatusOK, map[string]any{"nodes": nodes})
	}
}

func getInternalFileEvents(d Deps) echo.HandlerFunc {
	return func(c echo.Context) error {
		rootID := c.QueryParam("root_id")
		sinceMs, _ := strconv.ParseInt(c.QueryParam("since"), 10, 64)
		limit, _ := strconv.Atoi(c.QueryParam("limit"))
		if limit <= 0 || limit > 1000 {
			limit = 100
		}
		var evs []repo.FileEvent
		var err error
		if seqStr := c.QueryParam("after_seq"); seqStr != "" {
			afterSeq, _ := strconv.ParseInt(seqStr, 10, 64)
			evs, err = d.Events.ListSinceSeq(rootID, sinceMs, afterSeq, limit)
		} else {
			evs, err = d.Events.ListSince(rootID, sinceMs, limit)
		}
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		if evs == nil {
			evs = []repo.FileEvent{}
		}
		return c.JSON(http.StatusOK, map[string]any{"events": evs})
	}
}

type evidenceChildEntry struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	IsDir bool   `json:"is_dir"`
	Ext   string `json:"ext"`
}

type evidenceFileEntry struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	MtimeMs int64  `json:"mtime_ms"`
	Ext     string `json:"ext"`
}

type evidenceSkippedEntry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Ext    string `json:"ext"`
	Reason string `json:"reason"`
}

func getInternalNodeEvidence(d Deps) echo.HandlerFunc {
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
			return echo.NewHTTPError(http.StatusNotFound, "wiki_node not found")
		}
		rootID := ""
		if node.RootID != nil {
			rootID = *node.RootID
		}
		textLimit := clampInt(c.QueryParam("text_limit"), 10, 1, 20)
		pdfLimit := clampInt(c.QueryParam("pdf_limit"), 5, 1, 10)

		text, err := d.Files.ListEvidenceTextFiles(rootID, path, textLimit, 51200)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		pdfs, err := d.Files.ListEvidencePDFs(rootID, path, pdfLimit, 5242880)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		children, err := d.Files.ListEvidenceChildren(rootID, path, 100)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		skipped, err := d.Files.ListEvidenceSkippedSample(rootID, path, 20)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}

		body := map[string]any{
			"node_path":      path,
			"child_map":      buildChildMap(children),
			"text_files":     buildFileEntries(text),
			"pdf_files":      buildFileEntries(pdfs),
			"skipped_sample": buildSkippedEntries(skipped),
		}
		return c.JSON(http.StatusOK, body)
	}
}

func buildChildMap(rows []repo.FileIndex) []evidenceChildEntry {
	out := make([]evidenceChildEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, evidenceChildEntry{
			Name:  filepath.Base(r.Path),
			Size:  r.Size,
			IsDir: r.IsDir,
			Ext:   r.Ext,
		})
	}
	return out
}

func buildFileEntries(rows []repo.FileIndex) []evidenceFileEntry {
	out := make([]evidenceFileEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, evidenceFileEntry{
			Path: r.Path, Size: r.Size, MtimeMs: r.Mtime, Ext: r.Ext,
		})
	}
	return out
}

func buildSkippedEntries(rows []repo.FileIndex) []evidenceSkippedEntry {
	out := make([]evidenceSkippedEntry, 0, len(rows))
	for _, r := range rows {
		reason := "binary"
		switch r.Ext {
		case "jpeg", "jpg", "png", "heic":
			reason = "image"
		case "mov", "mp4":
			reason = "video"
		case "zip", "7z", "tar", "gz", "rar", "dmg", "iso":
			reason = "archive"
		}
		out = append(out, evidenceSkippedEntry{
			Path: r.Path, Size: r.Size, Ext: r.Ext, Reason: reason,
		})
	}
	return out
}

func clampInt(s string, def, lo, hi int) int {
	v, err := strconv.Atoi(s)
	if err != nil || v <= 0 {
		return def
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
