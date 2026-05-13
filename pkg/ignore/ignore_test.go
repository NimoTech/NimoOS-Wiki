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
