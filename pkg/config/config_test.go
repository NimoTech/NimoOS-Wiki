package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInit_WritesDefaultWhenAbsent(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "wiki.conf")
	require.NoError(t, Init(cfgPath, "[wiki]\nWikiWriteDebounceSec = 7\n"))
	_, err := os.Stat(cfgPath)
	require.NoError(t, err)
	require.Equal(t, 7, Cfg.WikiWriteDebounceSec)
}

func TestInit_DefaultsApplied(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "wiki.conf")
	require.NoError(t, Init(cfgPath, ""))
	require.Equal(t, "/var/run/nimoos", Cfg.RuntimePath)
	require.Equal(t, 5, Cfg.WikiWriteDebounceSec)
	require.Equal(t, 200, Cfg.EventDebounceMs)
	require.Equal(t, 50, Cfg.ChildMapAggregateThreshold)
}

func TestInit_ContainerDirsParsing(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "wiki.conf")
	require.NoError(t, Init(cfgPath, "[wiki]\nContainerDirs = a, b ,c\n"))
	require.Equal(t, []string{"a", "b", "c"}, Cfg.ContainerDirs)
}

func TestInit_DefaultContainerDirsIncludesNodeModules(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "wiki.conf")
	require.NoError(t, Init(cfgPath, ""))
	require.Contains(t, Cfg.ContainerDirs, "node_modules")
	require.Contains(t, Cfg.ContainerDirs, ".git")
}
