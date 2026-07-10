package roots

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/NimoTech/NimoOS-Wiki/pkg/db"
	"github.com/NimoTech/NimoOS-Wiki/service/repo"
	"github.com/stretchr/testify/require"
)

type fakeBus struct {
	mu        sync.Mutex
	publishes []struct{ Event string }
}

func (f *fakeBus) Publish(ev string, _ any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.publishes = append(f.publishes, struct{ Event string }{ev})
}
func (f *fakeBus) Close() error { return nil }
func (f *fakeBus) countOf(event string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, p := range f.publishes {
		if p.Event == event {
			n++
		}
	}
	return n
}

func setupManager(t *testing.T) (*Manager, *repo.WikiRootsRepo) {
	t.Helper()
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	roots := repo.NewWikiRoots(d)
	nodes := repo.NewWikiNodes(d)
	return NewManager(roots, nodes, &fakeBus{}), roots
}

func TestCreate_WritableInlineSucceeds(t *testing.T) {
	m, roots := setupManager(t)
	tmp := t.TempDir()
	id, err := m.Create(CreateArgs{Path: tmp, Level: "space"})
	require.NoError(t, err)
	require.NotEmpty(t, id)
	r, _ := roots.Get(id)
	require.Equal(t, tmp, r.Path)
	require.Equal(t, "inline", r.StorageMode)
	require.True(t, r.Enabled)
}

func TestCreate_NonWritableInlineFails(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can write anywhere; skip for root user")
	}
	m, _ := setupManager(t)
	tmp := t.TempDir()
	require.NoError(t, os.Chmod(tmp, 0500))
	defer os.Chmod(tmp, 0755)

	_, err := m.Create(CreateArgs{Path: tmp, Level: "space"})
	require.ErrorIs(t, err, ErrPathNotWritable)
}

func TestCreate_RelativePathRejected(t *testing.T) {
	m, _ := setupManager(t)
	_, err := m.Create(CreateArgs{Path: "relative/path", Level: "space"})
	require.ErrorIs(t, err, ErrInvalidArgs)
}

func TestCreate_NonexistentPathRejected(t *testing.T) {
	m, _ := setupManager(t)
	_, err := m.Create(CreateArgs{Path: "/nonexistent-by-design-12345", Level: "space"})
	require.ErrorIs(t, err, ErrPathNotExist)
}

func TestCreate_InvalidLevelRejected(t *testing.T) {
	m, _ := setupManager(t)
	tmp := t.TempDir()
	_, err := m.Create(CreateArgs{Path: tmp, Level: "bogus"})
	require.ErrorIs(t, err, ErrInvalidArgs)
}

func TestCreate_DefaultsApplied(t *testing.T) {
	m, roots := setupManager(t)
	tmp := t.TempDir()
	id, err := m.Create(CreateArgs{Path: tmp, Level: "project"})
	require.NoError(t, err)
	r, _ := roots.Get(id)
	require.Equal(t, "inline", r.StorageMode)
	require.Equal(t, 21600, r.ScanIntervalS)
	// WatchMode is 'auto' for typical /tmp (ext4/tmpfs); could be scan_only if
	// the test runs on an exotic FS. Accept both.
	require.Contains(t, []string{"auto", "scan_only"}, r.WatchMode)
}

func TestDelete_RemovesNodesAndOptionallyFiles(t *testing.T) {
	m, roots := setupManager(t)
	tmp := t.TempDir()
	id, err := m.Create(CreateArgs{Path: tmp, Level: "space"})
	require.NoError(t, err)

	// Create a .wiki.md to simulate prior flushes
	require.NoError(t, os.WriteFile(filepath.Join(tmp, ".wiki.md"), []byte("body"), 0644))

	require.NoError(t, m.Delete(id, true))

	_, err = roots.Get(id)
	require.Error(t, err) // not found

	_, err = os.Stat(filepath.Join(tmp, ".wiki.md"))
	require.True(t, os.IsNotExist(err), "purgeFiles should remove .wiki.md")
}

func TestDelete_NoPurgeKeepsFiles(t *testing.T) {
	m, _ := setupManager(t)
	tmp := t.TempDir()
	id, err := m.Create(CreateArgs{Path: tmp, Level: "space"})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(tmp, ".wiki.md"), []byte("x"), 0644))
	require.NoError(t, m.Delete(id, false))
	_, err = os.Stat(filepath.Join(tmp, ".wiki.md"))
	require.NoError(t, err, "without purge, .wiki.md must remain")
}

func TestRescan_ZerosLastScanAt(t *testing.T) {
	m, roots := setupManager(t)
	tmp := t.TempDir()
	id, err := m.Create(CreateArgs{Path: tmp, Level: "space"})
	require.NoError(t, err)
	require.NoError(t, roots.UpdateLastScanAt(id, 999))
	require.NoError(t, m.Rescan(id))
	r, _ := roots.Get(id)
	require.Equal(t, int64(0), r.LastScanAt)
}

func TestDetectFSType_ReturnsString(t *testing.T) {
	// Just sanity — should return something non-empty for "/"
	s := DetectFSType("/")
	require.NotEmpty(t, s)
}

func TestManager_PublishesRootEnabledOnCreate(t *testing.T) {
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	bus := &fakeBus{}
	mgr := NewManager(repo.NewWikiRoots(d), repo.NewWikiNodes(d), bus)

	tmp := t.TempDir()
	id, err := mgr.Create(CreateArgs{Path: tmp, Level: "project"})
	require.NoError(t, err)
	require.NotEmpty(t, id)
	require.Equal(t, 1, bus.countOf("Wiki:RootEnabled"))
}

func TestManager_PublishesRootDisabledOnDelete(t *testing.T) {
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	bus := &fakeBus{}
	mgr := NewManager(repo.NewWikiRoots(d), repo.NewWikiNodes(d), bus)

	tmp := t.TempDir()
	id, err := mgr.Create(CreateArgs{Path: tmp, Level: "project"})
	require.NoError(t, err)

	require.NoError(t, mgr.Delete(id, false))
	require.Equal(t, 1, bus.countOf("Wiki:RootDisabled"))
}

type fakeWatch struct {
	watched   []string
	unwatched []string
}

func (f *fakeWatch) Watch(rootID, rootPath string) error {
	f.watched = append(f.watched, rootID)
	return nil
}
func (f *fakeWatch) Unwatch(rootID string) { f.unwatched = append(f.unwatched, rootID) }

func TestSetEnabled_TogglesAndDrivesWatcher(t *testing.T) {
	m, rootsRepo := setupManager(t)
	fw := &fakeWatch{}
	m.SetWatch(fw)

	tmp := t.TempDir()
	id, err := m.Create(CreateArgs{Path: tmp, Level: "space"})
	require.NoError(t, err)
	require.Equal(t, []string{id}, fw.watched) // Create now watches immediately

	require.NoError(t, m.SetEnabled(id, false))
	got, err := rootsRepo.Get(id)
	require.NoError(t, err)
	require.False(t, got.Enabled)
	require.Equal(t, []string{id}, fw.unwatched)

	require.NoError(t, m.SetEnabled(id, true))
	got, err = rootsRepo.Get(id)
	require.NoError(t, err)
	require.True(t, got.Enabled)
	require.Equal(t, int64(0), got.LastScanAt) // marked overdue for reconciler catch-up
	require.Equal(t, []string{id, id}, fw.watched)

	// same-state call is a no-op
	require.NoError(t, m.SetEnabled(id, true))
	require.Equal(t, []string{id, id}, fw.watched)

	// unknown id
	require.ErrorIs(t, m.SetEnabled("nope", false), repo.ErrNotFound)
}

func TestDelete_Unwatches(t *testing.T) {
	m, _ := setupManager(t)
	fw := &fakeWatch{}
	m.SetWatch(fw)
	tmp := t.TempDir()
	id, err := m.Create(CreateArgs{Path: tmp, Level: "space"})
	require.NoError(t, err)
	require.NoError(t, m.Delete(id, false))
	require.Equal(t, []string{id}, fw.unwatched)
}
