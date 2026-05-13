package wikimd

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExtractUserNotes(t *testing.T) {
	src := `---
wiki_version: 1
---

<!-- BEGIN: system -->
foo
<!-- END: system -->

<!-- BEGIN: user-notes -->
## User Notes

我的笔记内容
多行
<!-- END: user-notes -->
`
	notes, ok := ExtractUserNotes(src)
	require.True(t, ok)
	require.Contains(t, notes, "我的笔记内容")
	require.Contains(t, notes, "多行")
	require.NotContains(t, notes, "BEGIN")
}

func TestExtractUserNotes_Missing(t *testing.T) {
	_, ok := ExtractUserNotes("no notes here")
	require.False(t, ok)
}

func TestExtractSystemRegion(t *testing.T) {
	src := "<!-- BEGIN: system -->\nhello\n<!-- END: system -->"
	sys, ok := ExtractSystemRegion(src)
	require.True(t, ok)
	require.Equal(t, src, sys)
}
