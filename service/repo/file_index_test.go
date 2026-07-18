package repo

import (
	"sort"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Wiki/pkg/db"
	"github.com/stretchr/testify/require"
)

// TestFileIndex_ListByRootAfter_KeysetPagination is the streaming
// reconciler's dependency (spec §4.6): pages must cover every row exactly
// once, in path order, with no skips or duplicates across page boundaries —
// including when paths share string prefixes (e.g. "/root/a" vs
// "/root/a/b" vs "/root/a1"), which is exactly where a naive LIKE-prefix or
// off-by-one keyset clause would misbehave.
func TestFileIndex_ListByRootAfter_KeysetPagination(t *testing.T) {
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	defer d.Close()
	files := NewFileIndex(d)

	rootID := "r"
	paths := []string{
		"/root/a1",
		"/root/a",
		"/root/a/b",
		"/root/ab",
		"/root/b",
		"/root/a/b/c",
		"/root/a0",
	}
	for _, p := range paths {
		require.NoError(t, files.Upsert(FileIndex{
			ID: NewID(), RootID: rootID, Path: p, Parent: "/root",
			Status: "present", Mtime: 1,
		}))
	}
	// A row in a different root must never leak into rootID's pages.
	require.NoError(t, files.Upsert(FileIndex{
		ID: NewID(), RootID: "other-root", Path: "/root/a", Parent: "/root", Status: "present",
	}))

	sorted := append([]string(nil), paths...)
	sort.Strings(sorted)

	var got []string
	after := ""
	const pageSize = 3
	pages := 0
	for {
		batch, err := files.ListByRootAfter(rootID, after, pageSize)
		require.NoError(t, err)
		if len(batch) == 0 {
			break
		}
		pages++
		require.LessOrEqual(t, len(batch), pageSize)
		for _, f := range batch {
			require.Equal(t, rootID, f.RootID)
			got = append(got, f.Path)
		}
		after = batch[len(batch)-1].Path
	}

	require.Equal(t, sorted, got, "keyset pagination must return every row exactly once, in path order, no skips/dupes")
	require.Equal(t, 3, pages, "7 rows at page size 3 should take exactly 3 pages (3+3+1)")
}

func TestFileIndex_RewriteAndCaseSensitivity(t *testing.T) {
	d := openTestDB(t)
	r := NewFileIndex(d)
	for _, e := range []FileIndex{
		{ID: "1", RootID: "r", Path: "/DATA/ProjectA", Parent: "/DATA", IsDir: true, Status: "present"},
		{ID: "2", RootID: "r", Path: "/DATA/ProjectA/x.txt", Parent: "/DATA/ProjectA", Status: "present"},
		{ID: "3", RootID: "r", Path: "/DATA/projecta/y.txt", Parent: "/DATA/projecta", Status: "present"},
	} {
		require.NoError(t, r.Upsert(e))
	}
	tx, err := d.Begin()
	require.NoError(t, err)
	require.NoError(t, r.RewritePathPrefix(tx, "r", "/DATA/ProjectA", "/DATA/Renamed"))
	require.NoError(t, tx.Commit())

	got, _ := r.Get("r", "/DATA/Renamed/x.txt")
	require.NotNil(t, got)
	require.Equal(t, "/DATA/Renamed", got.Parent)
	got2, _ := r.Get("r", "/DATA/projecta/y.txt")
	require.NotNil(t, got2, "lowercase must not be touched")
	require.Equal(t, "/DATA/projecta", got2.Parent)
}

func TestFileIndex_ListEvidence_FiltersByExtAndSize(t *testing.T) {
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	defer d.Close()
	files := NewFileIndex(d)

	rootID := "r"
	now := time.Now().UnixMilli()
	for _, f := range []struct {
		path   string
		ext    string
		size   int64
		isDir  bool
		opaque bool
	}{
		{"/root/a.md", "md", 1024, false, false},
		{"/root/b.txt", "txt", 32768, false, false},
		{"/root/huge.md", "md", 100000, false, false},
		{"/root/doc.pdf", "pdf", 500000, false, false},
		{"/root/giant.pdf", "pdf", 10000000, false, false},
		{"/root/img.jpeg", "jpeg", 3000000, false, false},
		{"/root/vid.mov", "mov", 50000000, false, false},
		{"/root/immich", "", 0, true, true},
	} {
		require.NoError(t, files.Upsert(FileIndex{
			ID: NewID(), RootID: rootID, Path: f.path,
			Parent: "/root", IsDir: f.isDir, IsOpaque: f.opaque,
			Status: "present", Ext: f.ext, Size: f.size, Mtime: now,
		}))
	}

	got, err := files.ListEvidenceTextFiles(rootID, "/root", 10, 51200)
	require.NoError(t, err)
	paths := []string{}
	for _, r := range got {
		paths = append(paths, r.Path)
	}
	require.ElementsMatch(t, []string{"/root/a.md", "/root/b.txt"}, paths)

	pdfs, err := files.ListEvidencePDFs(rootID, "/root", 10, 5242880)
	require.NoError(t, err)
	pdfPaths := []string{}
	for _, r := range pdfs {
		pdfPaths = append(pdfPaths, r.Path)
	}
	require.ElementsMatch(t, []string{"/root/doc.pdf"}, pdfPaths)
}

func TestFileIndex_ListEvidence_ScopesToSubtree(t *testing.T) {
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	defer d.Close()
	files := NewFileIndex(d)
	rootID := "r"
	now := time.Now().UnixMilli()
	for _, p := range []string{"/inside/a.md", "/inside/sub/b.md", "/sibling/c.md"} {
		// helper for parent
		parent := "/"
		for i := len(p) - 1; i >= 0; i-- {
			if p[i] == '/' {
				if i == 0 {
					parent = "/"
				} else {
					parent = p[:i]
				}
				break
			}
		}
		require.NoError(t, files.Upsert(FileIndex{
			ID: NewID(), RootID: rootID, Path: p, Parent: parent,
			IsDir: false, IsOpaque: false, Status: "present",
			Ext: "md", Size: 100, Mtime: now,
		}))
	}
	got, err := files.ListEvidenceTextFiles(rootID, "/inside", 10, 51200)
	require.NoError(t, err)
	paths := []string{}
	for _, r := range got {
		paths = append(paths, r.Path)
	}
	require.ElementsMatch(t, []string{"/inside/a.md", "/inside/sub/b.md"}, paths,
		"should include direct children AND deeper descendants, exclude siblings")
}

func TestFileIndex_ListEvidence_ChildMap(t *testing.T) {
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	defer d.Close()
	files := NewFileIndex(d)
	rootID := "r"
	for _, p := range []struct {
		path  string
		isDir bool
	}{
		{"/x/file.md", false},
		{"/x/sub", true},
		{"/x/other.txt", false},
	} {
		require.NoError(t, files.Upsert(FileIndex{
			ID: NewID(), RootID: rootID, Path: p.path, Parent: "/x",
			IsDir: p.isDir, Status: "present", Mtime: 1,
		}))
	}
	got, err := files.ListEvidenceChildren(rootID, "/x", 100)
	require.NoError(t, err)
	require.Len(t, got, 3)
}
