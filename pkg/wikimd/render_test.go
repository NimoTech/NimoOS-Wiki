package wikimd

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRender_FullExample(t *testing.T) {
	d := Doc{
		Version:     1,
		RootID:      "rrr",
		Path:        "/DATA/X",
		Level:       "project",
		GeneratedAt: time.Date(2026, 5, 13, 10, 30, 0, 0, time.UTC),
		Generator:   "nimoos-wiki/0.1.0",
		ChildMap: []ChildEntry{
			{Name: "src", Description: "源代码 (127 个文件)"},
			{Name: "node_modules", Description: "13428 个文件 (已跳过)", IsOpaque: true},
		},
		RecentChanges: []ChangeEntry{
			{When: time.Date(2026, 5, 13, 10, 25, 0, 0, time.UTC), Op: "modified", Path: "src/main.go"},
		},
		PendingCount: 3,
		UserNotes:    "我的备注\n",
	}
	out, hash := Render(d)
	require.Contains(t, out, "<!-- BEGIN: system -->")
	require.Contains(t, out, "<!-- END: system -->")
	require.Contains(t, out, "<!-- BEGIN: user-notes -->")
	require.Contains(t, out, "我的备注")
	require.Contains(t, out, "node_modules")
	require.Contains(t, out, "src/main.go")
	require.Contains(t, out, "checksum: "+hash)
	out2, hash2 := Render(d)
	require.Equal(t, hash, hash2)
	require.Equal(t, out, out2)
}

func TestRender_EmptySectionsShowPlaceholder(t *testing.T) {
	d := Doc{Version: 1, Path: "/X", Level: "system", Generator: "nimoos-wiki/0.1.0"}
	out, _ := Render(d)
	require.Contains(t, out, "## Summary")
	require.Contains(t, out, "_暂未生成")
	require.Contains(t, out, "## Key Sources")
	require.True(t, strings.Contains(out, "<!-- BEGIN: user-notes -->"))
}
