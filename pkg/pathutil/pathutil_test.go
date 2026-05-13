package pathutil

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClean(t *testing.T) {
	require.Equal(t, "/a/b", Clean("/a/b/"))
	require.Equal(t, "/a/b", Clean("/a//b"))
	require.Equal(t, "/a", Clean("/a/./"))
}

func TestIsUnder(t *testing.T) {
	require.True(t, IsUnder("/DATA/Project", "/DATA"))
	require.True(t, IsUnder("/DATA", "/DATA"))
	require.False(t, IsUnder("/DATA/Project", "/DATAX"))
	require.False(t, IsUnder("/DATA/Project", "/data"))
}

func TestParent(t *testing.T) {
	require.Equal(t, "/a", Parent("/a/b"))
	require.Equal(t, "/", Parent("/a"))
	require.Equal(t, "/", Parent("/"))
}
