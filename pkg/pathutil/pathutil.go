package pathutil

import (
	"path/filepath"
	"strings"
)

// Clean canonicalizes a path. An empty or "." result becomes "/".
// Note: this uses filepath.Clean which is OS-specific. NimoOS-Wiki targets
// Linux, so forward-slash semantics apply.
func Clean(p string) string {
	c := filepath.Clean(p)
	if c == "." {
		return "/"
	}
	return c
}

// IsUnder reports whether child is the same as parent or lives beneath it.
// Comparison is case-sensitive (Linux semantics).
func IsUnder(child, parent string) bool {
	child = Clean(child)
	parent = Clean(parent)
	if child == parent {
		return true
	}
	if !strings.HasSuffix(parent, "/") {
		parent += "/"
	}
	return strings.HasPrefix(child, parent)
}

// Parent returns the parent directory of p. The parent of "/" is "/".
func Parent(p string) string {
	p = Clean(p)
	if p == "/" {
		return "/"
	}
	return Clean(filepath.Dir(p))
}
