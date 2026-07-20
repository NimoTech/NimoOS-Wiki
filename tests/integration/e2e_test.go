//go:build integration

package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Wiki/pkg/db"
	"github.com/NimoTech/NimoOS-Wiki/pkg/ignore"
	"github.com/NimoTech/NimoOS-Wiki/pkg/nodelock"
	"github.com/NimoTech/NimoOS-Wiki/service/eventbus"
	"github.com/NimoTech/NimoOS-Wiki/service/processor"
	"github.com/NimoTech/NimoOS-Wiki/service/repo"
	"github.com/NimoTech/NimoOS-Wiki/service/roots"
	"github.com/NimoTech/NimoOS-Wiki/service/scanner"
	"github.com/NimoTech/NimoOS-Wiki/service/writer"
	"github.com/stretchr/testify/require"
)

// harness wires up the full Wiki service in-process and runs the same
// goroutines main.go does. Cancel ctx to stop.
type harness struct {
	files     *repo.FileIndexRepo
	events    *repo.FileEventsRepo
	nodes     *repo.WikiNodesRepo
	roots     *repo.WikiRootsRepo
	parse     *repo.ParseStatusRepo
	summaries *repo.WikiSummariesRepo
	mgr       *roots.Manager
	watch     *scanner.Watcher
	rec       *scanner.Reconciler
	proc      *processor.EventProcessor
	wri       *writer.Writer
	wg        *sync.WaitGroup

	ctx    context.Context
	cancel context.CancelFunc
	// goroutines lazily started after the first watcher.Watch call so that
	// scanner.Watcher's internal fsnotify handle is non-nil when Run starts
	// (Run early-exits if w.fsw == nil — matching main.go's "Watch then Run"
	// startup order).
	started bool
}

func newHarness(t *testing.T) (*harness, context.CancelFunc) {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "wiki.db"))
	require.NoError(t, err)

	h := &harness{
		files:     repo.NewFileIndex(d),
		events:    repo.NewFileEvents(d),
		nodes:     repo.NewWikiNodes(d),
		roots:     repo.NewWikiRoots(d),
		parse:     repo.NewParseStatus(d),
		summaries: repo.NewWikiSummaries(d),
	}
	bus := eventbus.Noop{}
	ig := ignore.New([]string{"node_modules", ".git"})
	h.mgr = roots.NewManager(h.roots, h.nodes, h.files, h.events, bus, ig)
	h.rec = scanner.NewReconciler(h.files, h.events, ig)
	h.watch = scanner.NewWatcher(h.events, h.nodes, ig, nil, nil)
	locks := nodelock.New()
	h.proc = processor.New(d, h.files, h.events, h.nodes, h.parse, bus, ig, locks, nil, h.roots, nil)
	h.proc.SyncIn = h.watch.SyncOut
	// 0 debounce window so tests don't wait 5s
	h.wri = writer.NewWriter(h.nodes, h.files, h.events, bus, locks, h.summaries, 0, nil)

	ctx, cancel := context.WithCancel(context.Background())
	h.ctx = ctx
	h.cancel = cancel
	h.wg = &sync.WaitGroup{}

	t.Cleanup(func() {
		cancel()
		// Don't wait — runs are best-effort here; underlying tempdir GC catches anything left
		// h.wg.Wait()  // intentionally skipped to keep test runtime bounded
		_ = d.Close()
	})
	return h, cancel
}

// startGoroutines spins up watcher/processor/writer Run loops. Must be called
// AFTER the first watcher.Watch() so the watcher's fsnotify handle exists.
func (h *harness) startGoroutines() {
	if h.started {
		return
	}
	h.started = true
	h.wg.Add(3)
	go func() { defer h.wg.Done(); h.watch.Run(h.ctx) }()
	go func() { defer h.wg.Done(); h.proc.Run(h.ctx, 100*time.Millisecond) }()
	go func() { defer h.wg.Done(); h.wri.Run(h.ctx) }()
}

func (h *harness) addRoot(t *testing.T, path string) string {
	t.Helper()
	// roots.Manager.Create seeds the wiki_node with Dirty=true so
	// WikiWriter produces the initial .wiki.md without any extra prompting.
	id, _, err := h.mgr.Create(roots.CreateArgs{Path: path, Level: "space"})
	require.NoError(t, err)
	require.NoError(t, h.watch.Watch(id, path))
	h.startGoroutines()
	return id
}

// readWikiMD reads the .wiki.md at dir, returning empty string if absent.
func readWikiMD(t *testing.T, dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, ".wiki.md"))
	if err != nil {
		return ""
	}
	return string(b)
}

func TestE2E_BasicFlow_FileCreate(t *testing.T) {
	h, _ := newHarness(t)
	root := t.TempDir()
	h.addRoot(t, root)

	// .wiki.md should be generated initially (node is created dirty by mgr.Create)
	require.Eventually(t, func() bool {
		body := readWikiMD(t, root)
		return strings.Contains(body, "<!-- BEGIN: system -->")
	}, 4*time.Second, 100*time.Millisecond, "initial .wiki.md should appear")

	// Now write a file and verify the child appears in regenerated .wiki.md
	require.NoError(t, os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hi"), 0644))

	require.Eventually(t, func() bool {
		body := readWikiMD(t, root)
		return strings.Contains(body, "hello.txt")
	}, 6*time.Second, 100*time.Millisecond, "hello.txt should appear in regenerated .wiki.md")
}

func TestE2E_DirectoryRenameCascade_NoCaseLeakage(t *testing.T) {
	h, _ := newHarness(t)
	root := t.TempDir()
	h.addRoot(t, root)

	// Set up two case-different siblings with content
	pa := filepath.Join(root, "ProjectA")
	pb := filepath.Join(root, "projecta") // lowercase sibling
	require.NoError(t, os.MkdirAll(pa, 0755))
	require.NoError(t, os.MkdirAll(pb, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(pa, "x.go"), []byte(""), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(pb, "y.go"), []byte(""), 0644))

	// Wait for events to settle
	time.Sleep(500 * time.Millisecond)
	// Force reconcile to populate file_index using the actual root id
	all, _ := h.roots.List()
	require.Len(t, all, 1)
	rootID := all[0].ID
	require.NoError(t, h.rec.Reconcile(h.ctx, rootID, root))
	time.Sleep(300 * time.Millisecond)

	// Rename ProjectA → Renamed
	require.NoError(t, os.Rename(pa, filepath.Join(root, "Renamed")))

	// Wait for the rename to propagate
	require.Eventually(t, func() bool {
		got, _ := h.files.Get(rootID, filepath.Join(root, "Renamed", "x.go"))
		return got != nil
	}, 6*time.Second, 100*time.Millisecond, "x.go should be at Renamed/ after rename")

	// CRITICAL: lowercase sibling must NOT be affected
	gotSib, _ := h.files.Get(rootID, filepath.Join(root, "projecta", "y.go"))
	require.NotNil(t, gotSib, "lowercase sibling /projecta/y.go must remain untouched after case-different rename")
}

func TestE2E_ExternalUserNotesEdit_SyncedToDB(t *testing.T) {
	h, _ := newHarness(t)
	root := t.TempDir()
	h.addRoot(t, root)

	// Wait for initial .wiki.md
	require.Eventually(t, func() bool {
		return readWikiMD(t, root) != ""
	}, 4*time.Second, 100*time.Millisecond)

	// Simulate external SMB edit: rewrite the user-notes block
	body := readWikiMD(t, root)
	newBody := strings.Replace(body,
		"（在这里写任何你想让 AI 记住的笔记。系统不会修改这部分内容。）",
		"用户写的笔记", 1)
	require.NoError(t, os.WriteFile(filepath.Join(root, ".wiki.md"), []byte(newBody), 0644))

	// Verify DB picked it up
	require.Eventually(t, func() bool {
		n, _ := h.nodes.Get(root)
		return n != nil && strings.Contains(n.UserNotes, "用户写的笔记")
	}, 6*time.Second, 100*time.Millisecond, "DB should have absorbed external user-notes edit")
}

func TestE2E_WikiWriter_NoSelfLoop(t *testing.T) {
	h, _ := newHarness(t)
	root := t.TempDir()
	h.addRoot(t, root)

	// Wait for initial .wiki.md
	require.Eventually(t, func() bool {
		return readWikiMD(t, root) != ""
	}, 4*time.Second, 100*time.Millisecond)
	time.Sleep(500 * time.Millisecond) // settle

	// Drain SyncOut just in case
	drainSync(h)
	// Snapshot file mtime
	info, err := os.Stat(filepath.Join(root, ".wiki.md"))
	require.NoError(t, err)
	firstMtime := info.ModTime()

	// Trigger a regeneration by touching a file
	require.NoError(t, os.WriteFile(filepath.Join(root, "trigger.txt"), []byte(""), 0644))
	time.Sleep(2 * time.Second)

	// Wait for .wiki.md to be regenerated
	require.Eventually(t, func() bool {
		newInfo, err := os.Stat(filepath.Join(root, ".wiki.md"))
		if err != nil {
			return false
		}
		return newInfo.ModTime() != firstMtime
	}, 4*time.Second, 100*time.Millisecond, ".wiki.md should be regenerated after file add")

	// After regeneration, NO sync task should be queued
	// (WikiWriter writes don't trigger user-notes sync because of Chtimes + DB-before-rename)
	time.Sleep(1 * time.Second)
	select {
	case task := <-h.watch.SyncOut:
		t.Fatalf("unexpected sync task after WikiWriter regeneration: %+v", task)
	default:
		// ok — no spurious sync
	}
}

func TestE2E_ContainerDir_RecordedOpaque(t *testing.T) {
	h, _ := newHarness(t)
	root := t.TempDir()
	h.addRoot(t, root)

	// Move-in a tree that contains node_modules
	staging := filepath.Join(t.TempDir(), "staging")
	require.NoError(t, os.MkdirAll(filepath.Join(staging, "node_modules", "lodash"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(staging, "node_modules", "lodash", "x.js"), []byte(""), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(staging, "main.go"), []byte(""), 0644))

	target := filepath.Join(root, "proj")
	require.NoError(t, os.Rename(staging, target))

	// The Watcher's backfillNewDir walks the moved-in tree, emits a single
	// opaque create event for node_modules (no recursion inside), and
	// EventProcessor (with the ignore.Matcher injected) sets file_index.IsOpaque=1.
	// No explicit reconcile needed.
	rootID := currentRootID(h)
	require.Eventually(t, func() bool {
		all, _ := h.files.ListAllByRoot(rootID)
		hasOpaque := false
		hasInside := false
		for _, f := range all {
			if filepath.Base(f.Path) == "node_modules" && f.IsOpaque {
				hasOpaque = true
			}
			if strings.Contains(f.Path, filepath.Join("node_modules", "lodash")) {
				hasInside = true
			}
		}
		return hasOpaque && !hasInside
	}, 6*time.Second, 100*time.Millisecond, "node_modules should be opaque; contents excluded")
}

// helpers

func drainSync(h *harness) {
	for {
		select {
		case <-h.watch.SyncOut:
		default:
			return
		}
	}
}

func currentRootID(h *harness) string {
	all, _ := h.roots.List()
	if len(all) == 0 {
		return ""
	}
	return all[0].ID
}
