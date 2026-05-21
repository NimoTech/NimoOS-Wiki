// Package v1 wires the /v1/wiki HTTP API: public (JWT-protected) routes for
// roots, wiki content, and internal (localhost-only, no-JWT) endpoints.
package v1

import (
	"github.com/NimoTech/NimoOS-Wiki/route"
	"github.com/NimoTech/NimoOS-Wiki/service/repo"
	"github.com/NimoTech/NimoOS-Wiki/service/roots"
	"github.com/labstack/echo/v4"
	echo_middleware "github.com/labstack/echo/v4/middleware"
)

// Deps bundles the collaborators the v1 router needs from main.
type Deps struct {
	Roots       *roots.Manager
	WikiRoots   *repo.WikiRootsRepo
	Nodes       *repo.WikiNodesRepo
	Files       *repo.FileIndexRepo
	Events      *repo.FileEventsRepo
	Summaries   *repo.WikiSummariesRepo
	RuntimePath string
}

// InitRouter wires v1 routes. The /v1/wiki/_internal group bypasses JWT
// and is restricted to localhost (Parser/Summary worker IPC).
func InitRouter(d Deps) *echo.Echo {
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true

	// Public group — JWT required.
	g := e.Group("/v1/wiki")
	g.Use(echo_middleware.JWTWithConfig(route.JWTConfig(d.RuntimePath)))

	// A: Root management
	g.GET("/roots", listRoots(d))
	g.POST("/roots", createRoot(d))
	g.DELETE("/roots/:id", deleteRoot(d))
	g.POST("/roots/:id/rescan", rescanRoot(d))
	g.GET("/candidates", listCandidates(d))

	// B: Wiki content
	g.GET("/tree", getTree(d))
	g.GET("/node", getNode(d))
	g.GET("/raw", getRaw(d))
	g.PUT("/user-notes", putUserNotes(d))
	g.GET("/recent-changes", getRecentChanges(d))

	// C: internal — no JWT, localhost only.
	i := e.Group("/v1/wiki/_internal", route.LocalhostOnly)
	i.GET("/needs-summary", getInternalNeedsSummary(d))
	i.GET("/node-evidence", getInternalNodeEvidence(d))
	i.POST("/summary", stubServiceUnavailable)
	i.GET("/file-events", getInternalFileEvents(d))
	i.POST("/index-status", stubServiceUnavailable)

	return e
}
