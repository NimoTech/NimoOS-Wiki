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

func TestProcessor_SyncUserNotesFromDisk_UpdatesDB(t *testing.T) {
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

	require.NoError(t, p.SyncUserNotesFromDisk(scanner.UserNotesSyncTask{
		RootID: "r", WikiMDPath: wikiMD, NodePath: tmp,
	}))

	n, _ := nodes.Get(tmp)
	require.NotNil(t, n)
	require.Contains(t, n.UserNotes, "my new notes")
	require.False(t, n.Dirty, "user-notes sync should not mark dirty (system region unchanged)")

	info, _ := os.Stat(wikiMD)
	require.Equal(t, info.ModTime().UnixMilli(), n.LastFlushedMtime)
}

func TestProcessor_SyncUserNotesFromDisk_NoopWhenUnchanged(t *testing.T) {
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
	require.NoError(t, p.SyncUserNotesFromDisk(scanner.UserNotesSyncTask{
		RootID: "r", WikiMDPath: wikiMD, NodePath: tmp,
	}))
	n, _ := nodes.Get(tmp)
	require.Equal(t, "unchanged", n.UserNotes)
	// Even with no diff, LastFlushedMtime should advance to suppress retriggers
	info, _ := os.Stat(wikiMD)
	require.Equal(t, info.ModTime().UnixMilli(), n.LastFlushedMtime)
}

func TestProcessor_SyncUserNotesFromDisk_Exported(t *testing.T) {
	// Smoke check: the symbol exists with the new exported name and runs.
	p, _, _, nodes, _ := setup(t)
	tmp := t.TempDir()
	nodePath := tmp
	wikiMD := filepath.Join(tmp, ".wiki.md")
	body := "---\nwiki_version: 1\npath: " + nodePath + "\nlevel: project\n" +
		"generated_at: 2026-05-20T00:00:00Z\ngenerator: test\nchecksum: x\n---\n\n" +
		"<!-- BEGIN: system -->\n<!-- END: system -->\n\n" +
		"<!-- BEGIN: user-notes -->\n## User Notes\n\nfrom-disk\n\n<!-- END: user-notes -->\n"
	require.NoError(t, os.WriteFile(wikiMD, []byte(body), 0644))

	require.NoError(t, nodes.Upsert(repo.WikiNode{
		ID: "n", Path: nodePath, Level: "project", UserNotes: "old", UpdatedAt: 1,
	}))

	require.NoError(t, p.SyncUserNotesFromDisk(scanner.UserNotesSyncTask{
		RootID: "r", WikiMDPath: wikiMD, NodePath: nodePath,
	}))

	n, _ := nodes.Get(nodePath)
	require.Contains(t, n.UserNotes, "from-disk")
}

// Sanity: time.Sleep wins racing the test
var _ = time.Now
