package rootsync

import (
	"os"
	"strings"
)

// fallbackBaseURL is the fallback address used when the service-discovery
// file is missing or empty.
// Core defaults to listening on localhost port 80 (per the Gateway reverse
// proxy convention), and requests exempt from localhost checks also use this
// address.
const fallbackBaseURL = "http://127.0.0.1"

// resolveBaseURL re-reads the service-discovery file before every request to
// resolve core's base URL. It's not cached because core's listen port can
// change across restarts (nimoos.url is written by core at startup); reading
// the file on every request guarantees requests always hit the currently
// live core instance. A missing file, a read failure, or empty content all
// fall back to fallbackBaseURL, so a discovery failure never blocks the
// caller (the failure is covered by the caller setting needsReconcile).
func resolveBaseURL(discoveryFile string) string {
	b, err := os.ReadFile(discoveryFile)
	if err != nil {
		return fallbackBaseURL
	}
	base := strings.TrimSpace(string(b))
	if base == "" {
		return fallbackBaseURL
	}
	return base
}
