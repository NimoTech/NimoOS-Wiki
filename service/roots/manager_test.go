package roots

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Wiki/common"
	"github.com/NimoTech/NimoOS-Wiki/pkg/db"
	"github.com/NimoTech/NimoOS-Wiki/pkg/ignore"
	"github.com/NimoTech/NimoOS-Wiki/service/repo"
	"github.com/stretchr/testify/require"
)

type fakeBus struct {
	mu        sync.Mutex
	publishes []struct {
		Event   string
		Payload any
	}
}

func (f *fakeBus) Publish(ev string, payload any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.publishes = append(f.publishes, struct {
		Event   string
		Payload any
	}{ev, payload})
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

// lastPayload returns the payload of the most recent publish of event, or nil
// if it was never published.
func (f *fakeBus) lastPayload(event string) any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var last any
	for _, p := range f.publishes {
		if p.Event == event {
			last = p.Payload
		}
	}
	return last
}

// setupManager builds a Manager with a nil ignore.Matcher: countDirsQuick
// then counts every directory raw (pre-Task-7 / pre-fix-round-1 behavior),
// which keeps tests that don't care about container-dir skipping simple.
func setupManager(t *testing.T) (*Manager, *repo.WikiRootsRepo) {
	return setupManagerWithIgnore(t, nil)
}

func setupManagerWithIgnore(t *testing.T, ig *ignore.Matcher) (*Manager, *repo.WikiRootsRepo) {
	t.Helper()
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	roots := repo.NewWikiRoots(d)
	nodes := repo.NewWikiNodes(d)
	files := repo.NewFileIndex(d)
	events := repo.NewFileEvents(d)
	return NewManager(roots, nodes, files, events, &fakeBus{}, ig), roots
}

func TestCreate_WritableInlineSucceeds(t *testing.T) {
	m, roots := setupManager(t)
	tmp := t.TempDir()
	id, _, err := m.Create(CreateArgs{Path: tmp, Level: "space"})
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

	_, _, err := m.Create(CreateArgs{Path: tmp, Level: "space"})
	require.ErrorIs(t, err, ErrPathNotWritable)
}

func TestCreate_RelativePathRejected(t *testing.T) {
	m, _ := setupManager(t)
	_, _, err := m.Create(CreateArgs{Path: "relative/path", Level: "space"})
	require.ErrorIs(t, err, ErrInvalidArgs)
}

func TestCreate_NonexistentPathRejected(t *testing.T) {
	m, _ := setupManager(t)
	_, _, err := m.Create(CreateArgs{Path: "/nonexistent-by-design-12345", Level: "space"})
	require.ErrorIs(t, err, ErrPathNotExist)
}

func TestCreate_InvalidLevelRejected(t *testing.T) {
	m, _ := setupManager(t)
	tmp := t.TempDir()
	_, _, err := m.Create(CreateArgs{Path: tmp, Level: "bogus"})
	require.ErrorIs(t, err, ErrInvalidArgs)
}

func TestCreate_DefaultsApplied(t *testing.T) {
	m, roots := setupManager(t)
	tmp := t.TempDir()
	id, _, err := m.Create(CreateArgs{Path: tmp, Level: "project"})
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
	id, _, err := m.Create(CreateArgs{Path: tmp, Level: "space"})
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
	id, _, err := m.Create(CreateArgs{Path: tmp, Level: "space"})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(tmp, ".wiki.md"), []byte("x"), 0644))
	require.NoError(t, m.Delete(id, false))
	_, err = os.Stat(filepath.Join(tmp, ".wiki.md"))
	require.NoError(t, err, "without purge, .wiki.md must remain")
}

func TestRescan_ZerosLastScanAt(t *testing.T) {
	m, roots := setupManager(t)
	tmp := t.TempDir()
	id, _, err := m.Create(CreateArgs{Path: tmp, Level: "space"})
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
	mgr := NewManager(repo.NewWikiRoots(d), repo.NewWikiNodes(d), nil, nil, bus, nil)

	tmp := t.TempDir()
	id, _, err := mgr.Create(CreateArgs{Path: tmp, Level: "project"})
	require.NoError(t, err)
	require.NotEmpty(t, id)
	require.Equal(t, 1, bus.countOf("Wiki:RootEnabled"))
}

func TestManager_PublishesRootDisabledOnDelete(t *testing.T) {
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	bus := &fakeBus{}
	mgr := NewManager(repo.NewWikiRoots(d), repo.NewWikiNodes(d), nil, nil, bus, nil)

	tmp := t.TempDir()
	id, _, err := mgr.Create(CreateArgs{Path: tmp, Level: "project"})
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
	id, _, err := m.Create(CreateArgs{Path: tmp, Level: "space"})
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
	id, _, err := m.Create(CreateArgs{Path: tmp, Level: "space"})
	require.NoError(t, err)
	require.NoError(t, m.Delete(id, false))
	require.Equal(t, []string{id}, fw.unwatched)
}

func TestDegradeToScanOnly(t *testing.T) {
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	bus := &fakeBus{}
	rootsRepo := repo.NewWikiRoots(d)
	m := NewManager(rootsRepo, repo.NewWikiNodes(d), nil, nil, bus, nil)
	fw := &fakeWatch{}
	m.SetWatch(fw)

	tmp := t.TempDir()
	id, _, err := m.Create(CreateArgs{Path: tmp, Level: "space"})
	require.NoError(t, err)

	m.DegradeToScanOnly(id, "watch_limit")

	r, err := rootsRepo.Get(id)
	require.NoError(t, err)
	require.Equal(t, "scan_only", r.WatchMode)
	require.Contains(t, fw.unwatched, id)

	require.Equal(t, 1, bus.countOf(common.EventWatchDegraded))
	payload, ok := bus.lastPayload(common.EventWatchDegraded).(map[string]any)
	require.True(t, ok)
	require.Equal(t, id, payload["root_id"])
	require.Equal(t, "watch_limit", payload["reason"])

	// Idempotent: calling again should not error or double-publish oddly.
	m.DegradeToScanOnly(id, "watch_limit")
	require.Equal(t, 2, bus.countOf(common.EventWatchDegraded))
}

func TestCreateLargeRootAutoScanOnly(t *testing.T) {
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	bus := &fakeBus{}
	roots := repo.NewWikiRoots(d)
	m := NewManager(roots, repo.NewWikiNodes(d), nil, nil, bus, nil)
	m.PrecheckDirLimit = 3
	m.PrecheckTimeout = 2 * time.Second

	tmp := t.TempDir()
	for i := 0; i < 5; i++ {
		require.NoError(t, os.Mkdir(filepath.Join(tmp, fmt.Sprintf("sub%d", i)), 0755))
	}

	id, modeReason, err := m.Create(CreateArgs{Path: tmp, Level: "space"})
	require.NoError(t, err)
	require.Equal(t, "large_root", modeReason, "create response must carry mode_reason=large_root")

	r, err := roots.Get(id)
	require.NoError(t, err)
	require.Equal(t, "scan_only", r.WatchMode)

	// Fix 3: the large_root event must carry the real root_id (previously
	// published before the ID was allocated, so root_id was always "").
	payload, ok := bus.lastPayload(common.EventWatchDegraded).(map[string]any)
	require.True(t, ok)
	require.Equal(t, id, payload["root_id"])
	require.Equal(t, "large_root", payload["reason"])
}

func TestCreateSmallRootStaysAuto(t *testing.T) {
	m, roots := setupManager(t)
	// countDirsQuick's WalkDir counts the root itself, so with 2 subdirs the
	// walk sees 3 dirs total; use limit=4 so that stays under the threshold.
	m.PrecheckDirLimit = 4
	m.PrecheckTimeout = 2 * time.Second

	tmp := t.TempDir()
	for i := 0; i < 2; i++ {
		require.NoError(t, os.Mkdir(filepath.Join(tmp, fmt.Sprintf("sub%d", i)), 0755))
	}

	id, modeReason, err := m.Create(CreateArgs{Path: tmp, Level: "space"})
	require.NoError(t, err)
	require.Equal(t, "", modeReason, "a normal create must not carry a mode_reason")

	r, err := roots.Get(id)
	require.NoError(t, err)
	require.Equal(t, "auto", r.WatchMode)
}

// TestCreateSkipsContainerDirsInPrecheck is the Fix-1 regression test: the
// precheck must not count (or descend into) container dirs, so a root whose
// bulk lives under a container dir (e.g. /DATA/.system_data) is not
// false-positived into scan_only.
func TestCreateSkipsContainerDirsInPrecheck(t *testing.T) {
	ig := ignore.New(nil) // baseline only — ".system_data" is in baselineContainerDirs
	m, roots := setupManagerWithIgnore(t, ig)
	m.PrecheckDirLimit = 5
	m.PrecheckTimeout = 2 * time.Second

	tmp := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(tmp, "sub0"), 0755))
	require.NoError(t, os.Mkdir(filepath.Join(tmp, "sub1"), 0755))

	container := filepath.Join(tmp, ".system_data")
	require.NoError(t, os.Mkdir(container, 0755))
	for i := 0; i < 10; i++ {
		require.NoError(t, os.Mkdir(filepath.Join(container, fmt.Sprintf("nested%d", i)), 0755))
	}

	// Without the container-dir skip, the walk would see root+2 subdirs+
	// .system_data+10 nested = 14 dirs, blowing past limit=5 into scan_only.
	id, modeReason, err := m.Create(CreateArgs{Path: tmp, Level: "space"})
	require.NoError(t, err)
	require.Equal(t, "", modeReason)

	r, err := roots.Get(id)
	require.NoError(t, err)
	require.Equal(t, "auto", r.WatchMode)
}

func TestDeleteCascadesFileIndexAndEmitsTombstones(t *testing.T) {
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	rRoots := repo.NewWikiRoots(d)
	rNodes := repo.NewWikiNodes(d)
	rFiles := repo.NewFileIndex(d)
	rEvents := repo.NewFileEvents(d)
	m := NewManager(rRoots, rNodes, rFiles, rEvents, nil, nil)

	dir := t.TempDir()
	id, _, err := m.Create(CreateArgs{Path: dir, Level: "space"})
	require.NoError(t, err)

	for _, f := range []repo.FileIndex{
		{ID: repo.NewID(), RootID: id, Path: dir + "/x.txt", Parent: dir, Status: "present"},
		{ID: repo.NewID(), RootID: id, Path: dir + "/y.pdf", Parent: dir, Status: "present"},
		{ID: repo.NewID(), RootID: id, Path: dir + "/sub", Parent: dir, IsDir: true, Status: "present"},
	} {
		require.NoError(t, rFiles.Upsert(f))
	}
	require.NoError(t, rEvents.Insert(repo.FileEvent{
		ID: repo.NewID(), RootID: id, Path: dir + "/x.txt", Op: "create",
		DetectedAt: time.Now().UnixMilli(),
	}))

	require.NoError(t, m.Delete(id, false))

	// file_index is cascade-cleared
	rows, err := rFiles.ListByRootAfter(id, "", 100)
	require.NoError(t, err)
	require.Empty(t, rows)

	// old create events are purged; each file (excluding directories) gets one
	// pre-marked-processed delete tombstone
	evs, err := rEvents.ListSince(id, 0, 100)
	require.NoError(t, err)
	var tombs []repo.FileEvent
	for _, e := range evs {
		require.Equal(t, "delete", e.Op, "non-delete events must be purged")
		tombs = append(tombs, e)
	}
	require.Len(t, tombs, 2) // x.txt + y.pdf, not sub/
	for _, e := range tombs {
		require.NotZero(t, e.ProcessedAt, "tombstones must be pre-marked processed")
	}
}

func TestCountDirsQuickTimeoutTreatedAsExceeded(t *testing.T) {
	// The deadline is only re-checked every 256 dirs, so the tree needs to be
	// big enough to reach that checkpoint before countDirsQuick can notice
	// the (already-expired) zero timeout.
	tmp := t.TempDir()
	for i := 0; i < 300; i++ {
		require.NoError(t, os.Mkdir(filepath.Join(tmp, fmt.Sprintf("sub%d", i)), 0755))
	}

	n, exceeded := countDirsQuick(tmp, 1_000_000, 0, nil)
	require.True(t, exceeded)
	require.Less(t, n, 1_000_000)
}
