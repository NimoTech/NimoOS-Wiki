package processor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/NimoTech/NimoOS-Wiki/service/repo"
	"github.com/stretchr/testify/require"
)

func TestBootSync_PullsExternalEditIntoDB(t *testing.T) {
	p, _, _, nodes, _ := setup(t)
	tmp := t.TempDir()

	// Seed wiki_node with old user_notes and a low last_flushed_mtime
	require.NoError(t, nodes.Upsert(repo.WikiNode{
		ID: "n", RootID: strPtr("r"), Path: tmp, Level: "project",
		UserNotes: "STALE", LastFlushedMtime: 100, UpdatedAt: 1,
	}))

	// Write a .wiki.md with NEW user_notes and a later mtime
	wikiMD := filepath.Join(tmp, ".wiki.md")
	body := "---\nwiki_version: 1\npath: " + tmp + "\nlevel: project\n" +
		"generated_at: 2026-05-20T00:00:00Z\ngenerator: test\nchecksum: x\n---\n\n" +
		"<!-- BEGIN: system -->\n<!-- END: system -->\n\n" +
		"<!-- BEGIN: user-notes -->\n## User Notes\n\nNEW-from-disk\n\n<!-- END: user-notes -->\n"
	require.NoError(t, os.WriteFile(wikiMD, []byte(body), 0644))

	count, err := p.BootSyncRoot("r")
	require.NoError(t, err)
	require.Equal(t, 1, count, "exactly one node should have been synced")

	got, _ := nodes.Get(tmp)
	require.Contains(t, got.UserNotes, "NEW-from-disk")
}

func TestBootSync_SkipsNodesWithSameMtime(t *testing.T) {
	p, _, _, nodes, _ := setup(t)
	tmp := t.TempDir()

	wikiMD := filepath.Join(tmp, ".wiki.md")
	body := "---\nwiki_version: 1\npath: " + tmp + "\nlevel: project\n" +
		"generated_at: 2026-05-20T00:00:00Z\ngenerator: test\nchecksum: x\n---\n\n" +
		"<!-- BEGIN: system -->\n<!-- END: system -->\n\n" +
		"<!-- BEGIN: user-notes -->\n## User Notes\n\nKEEP\n\n<!-- END: user-notes -->\n"
	require.NoError(t, os.WriteFile(wikiMD, []byte(body), 0644))

	st, _ := os.Stat(wikiMD)
	mt := st.ModTime().UnixMilli()
	require.NoError(t, nodes.Upsert(repo.WikiNode{
		ID: "n", RootID: strPtr("r"), Path: tmp, Level: "project",
		UserNotes: "KEEP", LastFlushedMtime: mt, UpdatedAt: 1,
	}))

	count, err := p.BootSyncRoot("r")
	require.NoError(t, err)
	require.Equal(t, 0, count, "matching mtime → skip")
}

func TestBootSync_SkipsNodesWithNoFile(t *testing.T) {
	p, _, _, nodes, _ := setup(t)
	tmp := t.TempDir()
	require.NoError(t, nodes.Upsert(repo.WikiNode{
		ID: "n", RootID: strPtr("r"), Path: tmp, Level: "project", UpdatedAt: 1,
	}))
	count, err := p.BootSyncRoot("r")
	require.NoError(t, err)
	require.Equal(t, 0, count, "missing .wiki.md is not an error")
}
