package ignore

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestContainerDir(t *testing.T) {
	m := New([]string{"node_modules", ".git"})
	require.True(t, m.IsContainerDir("node_modules"))
	require.True(t, m.IsContainerDir(".git"))
	require.False(t, m.IsContainerDir("src"))
	require.True(t, m.IsContainerDirPath("/a/b/node_modules"))
	require.False(t, m.IsContainerDirPath("/a/b/node_modules/lodash"))
}

func TestSystemIgnore(t *testing.T) {
	m := New(nil)
	require.True(t, m.IsSystemIgnoredBasename(".DS_Store"))
	require.True(t, m.IsSystemIgnoredBasename("foo.tmp"))
	require.True(t, m.IsSystemIgnoredBasename(".#lock"))
	require.True(t, m.IsSystemIgnoredBasename("notes.txt~"))
	require.False(t, m.IsSystemIgnoredBasename("README.md"))
}

func TestWikiFile(t *testing.T) {
	m := New(nil)
	require.True(t, m.IsWikiFile(".wiki.md"))
	require.True(t, m.IsWikiTmpFile(".wiki.md.tmp"))
	require.False(t, m.IsWikiFile("notes.md"))
}

func TestMatcher_BaselineContainersAlwaysApplied(t *testing.T) {
	// Even when user supplies nil/empty, NAS noise dirs are containers.
	m := New(nil)
	for _, name := range []string{
		"immich", "@eaDir", "#recycle", ".AppleDouble", ".fseventsd",
		".Spotlight-V100", ".Trashes", ".DocumentRevisions-V100", "__MACOSX",
		"Network Trash Folder", "Temporary Items",
		"lost+found", ".snapshots", "@__thumb",
	} {
		if !m.IsContainerDir(name) {
			t.Errorf("baseline container %q should be opaque", name)
		}
	}
}

func TestMatcher_UserContainersStillWork(t *testing.T) {
	// User-supplied dirs are merged with baseline, not replaced.
	m := New([]string{"node_modules", ".git"})
	if !m.IsContainerDir("node_modules") {
		t.Error("user-supplied node_modules should be opaque")
	}
	if !m.IsContainerDir(".git") {
		t.Error("user-supplied .git should be opaque")
	}
	// Baseline still applies.
	if !m.IsContainerDir("immich") {
		t.Error("baseline immich should still be opaque alongside user list")
	}
}

func TestMatcher_TrashPrefixOpaque(t *testing.T) {
	m := New(nil)
	for _, name := range []string{".Trash-0", ".Trash-1000", ".Trash-99999"} {
		if !m.IsContainerDir(name) {
			t.Errorf("Linux freedesktop trash dir %q should be opaque (prefix match)", name)
		}
	}
	// Sanity: bare ".Trash" without suffix should NOT match the prefix
	// (it would need an exact entry, and we deliberately don't have one).
	// .Trashes (plural, macOS) IS a baseline exact-match — already covered.
	if m.IsContainerDir(".Trash-photos") {
		// Note: this WOULD silently swallow a user dir literally named
		// .Trash-photos. We accept this risk because such a name is
		// vanishingly rare; flag if you find a user who hits this.
		t.Log("note: .Trash-photos matches prefix — accepted trade-off")
	}
	// Bare "Trash" with no leading dot should NOT match.
	if m.IsContainerDir("Trash") {
		t.Errorf("bare 'Trash' should not match .Trash- prefix")
	}
}

func TestMatcher_NonContainerNotMarked(t *testing.T) {
	// Sanity check — random dirs aren't accidentally containers.
	m := New(nil)
	for _, name := range []string{"src", "docs", "Documents", "photos"} {
		if m.IsContainerDir(name) {
			t.Errorf("%q should NOT be a container", name)
		}
	}
}
