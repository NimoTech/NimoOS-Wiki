// Package route provides shared HTTP middleware used by the v1 router.
package route

import (
	"crypto/ecdsa"
	"net/http"
	"strconv"
	"strings"

	"github.com/NimoTech/NimoOS-Common/external"
	"github.com/NimoTech/NimoOS-Common/utils/jwt"
	"github.com/labstack/echo/v4"
	echo_middleware "github.com/labstack/echo/v4/middleware"
)

// isLocalhostRealIP returns true when the *originating* caller is on this host.
// Uses echo's RealIP() which prefers X-Forwarded-For / X-Real-IP over
// RemoteAddr. The gateway scrubs spoofed X-Forwarded-For from external
// requests (NimoOS-Gateway/route/gateway_route.go rewriteRequestSourceIP) and
// httputil.ReverseProxy appends the immediate client IP — so for proxied
// external traffic RealIP() resolves to the real external client (not
// 127.0.0.1), and only genuine in-host callers (Agent via gateway, CLI direct,
// internal workers) hit the localhost branch.
func isLocalhostRealIP(c echo.Context) bool {
	host := c.RealIP()
	return host == "127.0.0.1" || host == "::1" || host == "localhost"
}

// JWTConfig returns echo's JWT middleware configured for NimoOS.
// Localhost exempt for in-host services (Agent, CLI); external requests still
// require JWT. Localhost-exempt requests honor X-NimoOS-User-ID on
// c.Request().Header — the same place ParseTokenFunc writes the claim-derived
// value on the JWT path, so downstream handlers read identity uniformly.
func JWTConfig(runtimePath string) echo_middleware.JWTConfig {
	return echo_middleware.JWTConfig{
		Skipper: func(c echo.Context) bool {
			if !isLocalhostRealIP(c) {
				return false
			}
			// Localhost: trust the in-host caller's X-NimoOS-User-ID header
			// (already on c.Request().Header — no copy needed).
			return true
		},
		ParseTokenFunc: func(token string, c echo.Context) (interface{}, error) {
			valid, claims, err := jwt.Validate(token, func() (*ecdsa.PublicKey, error) {
				return external.GetPublicKey(runtimePath)
			})
			if err != nil || !valid {
				return nil, echo.ErrUnauthorized
			}
			c.Request().Header.Set("X-NimoOS-User-ID", strconv.Itoa(claims.ID))
			c.Request().Header.Set("X-NimoOS-User-Name", claims.Username)
			return claims, nil
		},
		TokenLookupFuncs: []echo_middleware.ValuesExtractor{
			func(c echo.Context) ([]string, error) {
				auth := c.Request().Header.Get(echo.HeaderAuthorization)
				return []string{strings.TrimPrefix(auth, "Bearer ")}, nil
			},
		},
	}
}

// LocalhostOnly rejects any request not originating from 127.0.0.1 or ::1.
// Use for /v1/wiki/_internal/* routes.
func LocalhostOnly(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if !isLocalhostRealIP(c) {
			return echo.NewHTTPError(http.StatusForbidden, "internal endpoint")
		}
		return next(c)
	}
}
