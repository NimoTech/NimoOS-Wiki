// Package ignore decides which files/dirs the scanner should treat specially:
// container dirs (opaque rollups), system noise (.DS_Store, editor temps), and
// the wiki sidecar files themselves.
package ignore

import (
	"path/filepath"
	"strings"
)

// Matcher holds the runtime configuration for ignore rules. It is safe for
// concurrent reads after construction.
type Matcher struct {
	containerSet map[string]struct{}
}

// New constructs a Matcher from a list of container directory basenames
// (e.g. "node_modules", ".git", "venv"). Container dirs are rolled up into a
// single opaque child-map entry instead of being descended.
func New(containerDirs []string) *Matcher {
	m := &Matcher{containerSet: make(map[string]struct{}, len(containerDirs))}
	for _, d := range containerDirs {
		m.containerSet[d] = struct{}{}
	}
	return m
}

// IsContainerDir reports whether the basename matches a configured container.
func (m *Matcher) IsContainerDir(basename string) bool {
	_, ok := m.containerSet[basename]
	return ok
}

// IsContainerDirPath reports whether the final segment of p is a container dir.
// Note: it does NOT report true for paths *inside* a container — only for the
// container directory itself.
func (m *Matcher) IsContainerDirPath(p string) bool {
	return m.IsContainerDir(filepath.Base(p))
}

// IsSystemIgnoredBasename reports whether a basename should be filtered out
// regardless of configuration: OS junk, editor swap/backup/lock files.
func (m *Matcher) IsSystemIgnoredBasename(name string) bool {
	switch name {
	case ".DS_Store", "Thumbs.db":
		return true
	}
	if strings.HasSuffix(name, ".tmp") || strings.HasSuffix(name, ".swp") || strings.HasSuffix(name, "~") {
		return true
	}
	// Emacs lock files: ".#something"
	if strings.HasPrefix(name, ".#") {
		return true
	}
	// Emacs autosave: "#something#"
	if strings.HasPrefix(name, "#") && strings.HasSuffix(name, "#") && len(name) > 1 {
		return true
	}
	return false
}

// IsWikiFile reports whether the basename is the wiki sidecar file itself.
func (m *Matcher) IsWikiFile(name string) bool { return name == ".wiki.md" }

// IsWikiTmpFile reports whether the basename is the writer's temp file.
func (m *Matcher) IsWikiTmpFile(name string) bool { return name == ".wiki.md.tmp" }
