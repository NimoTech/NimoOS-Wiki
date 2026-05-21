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

// baselineContainerDirs are basenames that are ALWAYS treated as opaque
// container dirs, regardless of user configuration. These are well-known
// NAS / OS / app artifacts that never contain user-authored content the
// wiki should index:
//
//   - Synology system: @eaDir (indexer), #recycle (trash), @__thumb (thumbnails)
//   - macOS metadata: .AppleDouble, .fseventsd, .Spotlight-V100, .Trashes,
//     .DocumentRevisions-V100, __MACOSX (zip resource forks),
//     Network Trash Folder, Temporary Items
//   - Filesystem internals: lost+found (ext), .snapshots (btrfs/zfs)
//   - Linux freedesktop trash: .Trash-* (matched by prefix, see baselineContainerDirPrefixes)
//   - App data dirs: immich (Immich photo service data — has its own UI)
//
// If you genuinely want one of these indexed (e.g., archiving Immich data
// into the wiki), open a discussion — the baseline is intentionally
// non-configurable to keep noise out of every NimoOS install by default.
var baselineContainerDirs = []string{
	"@eaDir", "#recycle", "@__thumb",
	".AppleDouble", ".fseventsd", ".Spotlight-V100", ".Trashes",
	".DocumentRevisions-V100", "__MACOSX",
	"Network Trash Folder", "Temporary Items",
	"lost+found", ".snapshots",
	"immich",
}

// baselineContainerDirPrefixes are basename PREFIXES (not full names) that are
// always opaque. Used for variable-suffix conventions like Linux freedesktop
// trash dirs `.Trash-{uid}` (`.Trash-1000`, `.Trash-1001`, ...). Prefix match
// only applies to baseline; user-configured ContainerDirs are still exact.
var baselineContainerDirPrefixes = []string{
	".Trash-",
}

// New constructs a Matcher from a list of user-configured container directory
// basenames (e.g. "node_modules", ".git", "venv") merged with the built-in
// baseline of NAS / OS noise dirs (see baselineContainerDirs). User config
// can only ADD to the opaque set; it can't remove baseline entries.
func New(containerDirs []string) *Matcher {
	m := &Matcher{
		containerSet: make(map[string]struct{}, len(containerDirs)+len(baselineContainerDirs)),
	}
	for _, d := range containerDirs {
		m.containerSet[d] = struct{}{}
	}
	for _, d := range baselineContainerDirs {
		m.containerSet[d] = struct{}{}
	}
	return m
}

// IsContainerDir reports whether the basename matches a configured container.
// Exact-match for user dirs + baselineContainerDirs; prefix-match for
// baselineContainerDirPrefixes (Linux trash dirs like .Trash-1000).
func (m *Matcher) IsContainerDir(basename string) bool {
	if _, ok := m.containerSet[basename]; ok {
		return true
	}
	for _, p := range baselineContainerDirPrefixes {
		if strings.HasPrefix(basename, p) {
			return true
		}
	}
	return false
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
