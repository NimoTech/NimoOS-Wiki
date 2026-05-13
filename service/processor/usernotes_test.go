package processor

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Wiki/service/repo"
	"github.com/NimoTech/NimoOS-Wiki/service/scanner"
	"github.com/stretchr/testify/require"
)

func TestRunUserNotesSync_UpdatesDB(t *testing.T) {
	p, _, _, nodes, _ := setup(t)
	tmp := t.TempDir()
	wikiMD := filepath.Join(tmp, ".wiki.md")
	require.NoError(t, os.WriteFile(wikiMD, []byte(`---
wiki_version: 1
---

<!-- BEGIN: system -->
foo
<!-- END: system -->

<!-- BEGIN: user-notes -->
## User Notes

my new notes
<!-- END: user-notes -->
`), 0644))
	require.NoError(t, nodes.Upsert(repo.WikiNode{
		ID: "n", Path: tmp, Level: "project", UpdatedAt: 1,
	}))

	require.NoError(t, p.runUserNotesSync(scanner.UserNotesSyncTask{
		RootID: "r", WikiMDPath: wikiMD, NodePath: tmp,
	}))

	n, _ := nodes.Get(tmp)
	require.NotNil(t, n)
	require.Contains(t, n.UserNotes, "my new notes")
	require.False(t, n.Dirty, "user-notes sync should not mark dirty (system region unchanged)")

	info, _ := os.Stat(wikiMD)
	require.Equal(t, info.ModTime().UnixMilli(), n.LastFlushedMtime)
}

func TestRunUserNotesSync_NoChange_NoUpdate(t *testing.T) {
	p, _, _, nodes, _ := setup(t)
	tmp := t.TempDir()
	wikiMD := filepath.Join(tmp, ".wiki.md")
	require.NoError(t, os.WriteFile(wikiMD, []byte(`<!-- BEGIN: user-notes -->
## User Notes

unchanged
<!-- END: user-notes -->
`), 0644))
	require.NoError(t, nodes.Upsert(repo.WikiNode{
		ID: "n", Path: tmp, Level: "project", UserNotes: "unchanged",
		UpdatedAt: 1,
	}))
	require.NoError(t, p.runUserNotesSync(scanner.UserNotesSyncTask{
		RootID: "r", WikiMDPath: wikiMD, NodePath: tmp,
	}))
	n, _ := nodes.Get(tmp)
	require.Equal(t, "unchanged", n.UserNotes)
	// Even with no diff, LastFlushedMtime should advance to suppress retriggers
	info, _ := os.Stat(wikiMD)
	require.Equal(t, info.ModTime().UnixMilli(), n.LastFlushedMtime)
}

// Sanity: time.Sleep wins racing the test
var _ = time.Now
