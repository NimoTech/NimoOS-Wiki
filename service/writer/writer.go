package writer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/NimoTech/NimoOS-Wiki/common"
	"github.com/NimoTech/NimoOS-Wiki/pkg/childmap"
	"github.com/NimoTech/NimoOS-Wiki/pkg/nodelock"
	"github.com/NimoTech/NimoOS-Wiki/pkg/wikimd"
	"github.com/NimoTech/NimoOS-Wiki/service/eventbus"
	"github.com/NimoTech/NimoOS-Wiki/service/repo"
	"go.uber.org/zap"
)

// Writer flushes dirty wiki_nodes to disk. Implements the spec §6.5 ordering:
//   1. read node → render
//   2. checksum equal? skip write, clear dirty
//   3. write .wiki.md.tmp
//   4. T = time.Now()
//   5. os.Chtimes(.tmp, T, T)  ← lock mtime BEFORE rename
//   6. DB UPDATE last_flushed_mtime=T, dirty=0   ← MUST commit before rename
//   7. rename(.tmp → .wiki.md)  ← only NOW does fsnotify fire
//   8. publish Wiki:NodeUpdated
//
// This ordering ensures the Watcher's handle-wiki-file logic (which queries
// last_flushed_mtime via DB on receiving the rename's fsnotify event) sees
// the up-to-date timestamp and correctly identifies the event as our own write.
type Writer struct {
	nodes              *repo.WikiNodesRepo
	files              *repo.FileIndexRepo
	events             *repo.FileEventsRepo
	bus                eventbus.Bus
	locks              *nodelock.Locks
	summaries          *repo.WikiSummariesRepo
	debounceWindow     time.Duration
	aggregateThreshold int
	log                *zap.Logger
	onRootGone         func(rootID string)
}

// NewWriter constructs a Writer. The `locks` parameter is the shared per-path
// mutex set used to serialize with EventProcessor.SyncUserNotesFromDisk on
// the same wiki node. Production callers MUST pass the same *nodelock.Locks
// instance both services share (wired in main.go). Passing nil falls back to
// a fresh local set — safe for tests, broken in production.
func NewWriter(nodes *repo.WikiNodesRepo, files *repo.FileIndexRepo,
	events *repo.FileEventsRepo, bus eventbus.Bus, locks *nodelock.Locks,
	summaries *repo.WikiSummariesRepo,
	debounceWindow time.Duration, log *zap.Logger) *Writer {
	if log == nil {
		log = zap.NewNop()
	}
	if bus == nil {
		bus = eventbus.Noop{}
	}
	if locks == nil {
		locks = nodelock.New()
	}
	return &Writer{
		nodes: nodes, files: files, events: events, bus: bus, locks: locks,
		summaries: summaries,
		debounceWindow: debounceWindow, aggregateThreshold: 50, log: log,
	}
}

// SetOnRootGone wires the callback fired when a flush hits ENOENT and the
// node's directory itself is gone (root path vanished). Wired to
// roots.Manager.DisableForMissingPath in main. Nil = disabled (tests).
func (w *Writer) SetOnRootGone(fn func(rootID string)) { w.onRootGone = fn }

// maybeRootGone inspects a flush error; if it is ENOENT and the node path
// no longer exists, the node's root has vanished — tell the roots manager.
func (w *Writer) maybeRootGone(n repo.WikiNode, err error) {
	if w.onRootGone == nil || n.RootID == nil || !os.IsNotExist(err) {
		return
	}
	if _, statErr := os.Stat(n.Path); statErr == nil || !os.IsNotExist(statErr) {
		return
	}
	w.onRootGone(*n.RootID)
}

// FlushOne renders + writes one node's .wiki.md.
func (w *Writer) FlushOne(nodePath string) error {
	unlock := w.locks.Lock(nodePath)
	defer unlock()

	node, err := w.nodes.Get(nodePath)
	if err != nil {
		return err
	}
	if node == nil || !node.Dirty {
		return nil
	}

	now := time.Now().UnixMilli()
	if w.debounceWindow > 0 && node.LastFlushedAt > 0 &&
		now-node.LastFlushedAt < w.debounceWindow.Milliseconds() {
		return nil
	}

	doc, childCount, err := w.buildDoc(node)
	if err != nil {
		return err
	}
	body, hash := wikimd.Render(doc)

	if node.ChecksumSystem == hash {
		// Content unchanged — zero-I/O skip
		return w.nodes.SetDirty(nodePath, false)
	}

	target := filepath.Join(nodePath, ".wiki.md")
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0644); err != nil {
		return err
	}

	T := time.Now()
	if err := os.Chtimes(tmp, T, T); err != nil {
		_ = os.Remove(tmp)
		return err
	}

	// CRITICAL: DB commit MUST happen BEFORE rename.
	// rename fires fsnotify immediately; Watcher.handleWikiFile reads
	// last_flushed_mtime from DB. If DB hasn't been updated, Watcher misclassifies
	// our own write as an external edit and triggers a useless reverse-sync.
	mt := T.UnixMilli()
	if err := w.nodes.RecordFlush(nodePath, hash, mt, mt); err != nil {
		_ = os.Remove(tmp)
		return err
	}

	// Best-effort: update cached child_count. Failure is non-fatal — the
	// .wiki.md file is already written and RecordFlush succeeded.
	if err := w.nodes.SetChildCount(nodePath, childCount); err != nil {
		w.log.Warn("SetChildCount failed", zap.String("path", nodePath), zap.Error(err))
	}

	if err := os.Rename(tmp, target); err != nil {
		// DB already updated but file is in inconsistent state.
		// Re-dirty so next flush retries.
		_ = w.nodes.SetDirty(nodePath, true)
		return err
	}

	rootID := ""
	if node.RootID != nil {
		rootID = *node.RootID
	}
	w.bus.Publish(common.EventNodeUpdated, map[string]any{
		"path":    nodePath,
		"root_id": rootID,
	})
	return nil
}

// buildDoc populates wikimd.Doc from node + child_map + recent events.
// Also returns the direct-child count (len of ListByParent result) so the
// caller can persist it to wiki_nodes.child_count.
func (w *Writer) buildDoc(node *repo.WikiNode) (wikimd.Doc, int, error) {
	rootID := ""
	if node.RootID != nil {
		rootID = *node.RootID
	}

	// Child map from file_index (direct children only)
	children, err := w.files.ListByParent(rootID, node.Path)
	if err != nil {
		return wikimd.Doc{}, 0, err
	}
	cmEntries := make([]childmap.Entry, 0, len(children))
	for _, c := range children {
		cmEntries = append(cmEntries, childmap.Entry{
			Name:     filepath.Base(c.Path),
			IsDir:    c.IsDir,
			IsOpaque: c.IsOpaque,
			Ext:      c.Ext,
			Mtime:    c.Mtime,
		})
	}
	groups := childmap.Aggregate(cmEntries, w.aggregateThreshold)
	cms := make([]wikimd.ChildEntry, 0, len(groups))
	for _, g := range groups {
		cms = append(cms, wikimd.ChildEntry{
			Name:        groupDisplayName(g),
			Description: describeGroup(g),
			IsOpaque:    g.IsOpaque,
		})
	}

	// Recent changes (top 20) under this node's subtree
	recent, _ := w.events.RecentForNode(rootID, node.Path, 20)
	rcs := make([]wikimd.ChangeEntry, 0, len(recent))
	for _, e := range recent {
		rel, err := filepath.Rel(node.Path, e.Path)
		if err != nil {
			rel = e.Path
		}
		rcs = append(rcs, wikimd.ChangeEntry{
			When: time.UnixMilli(e.DetectedAt),
			Op:   e.Op,
			Path: rel,
		})
	}

	var summaryText string
	if w.summaries != nil {
		if s, err := w.summaries.Get(node.Path); err == nil && s != nil {
			summaryText = s.Summary
		}
	}

	return wikimd.Doc{
		Version:       1,
		RootID:        rootID,
		Path:          node.Path,
		Level:         node.Level,
		GeneratedAt:   time.Now(),
		Generator:     "nimoos-wiki/" + common.WikiVersion,
		ChildMap:      cms,
		RecentChanges: rcs,
		UserNotes:     node.UserNotes,
		Summary:       summaryText,
	}, len(children), nil
}

func groupDisplayName(g childmap.Group) string {
	if g.IsDir || g.Name != "" {
		return g.Name
	}
	if g.IsAggregate {
		return g.Ext
	}
	return g.Name
}

func describeGroup(g childmap.Group) string {
	if g.IsDir {
		if g.IsOpaque {
			return fmt.Sprintf("%d files (skipped)", g.ChildFileCount)
		}
		if g.ChildFileCount > 0 {
			return fmt.Sprintf("%d files", g.ChildFileCount)
		}
		return "directory"
	}
	if g.IsAggregate {
		if g.Ext == childmap.OtherBucket {
			return fmt.Sprintf("%d other files", g.Count)
		}
		return fmt.Sprintf("%d .%s files", g.Count, g.Ext)
	}
	return "file"
}

// Run flushes dirty nodes on a ticker until ctx is cancelled.
func (w *Writer) Run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			dirty, err := w.nodes.ListDirty(50)
			if err != nil {
				w.log.Warn("list dirty", zap.Error(err))
				continue
			}
			for _, n := range dirty {
				if err := w.FlushOne(n.Path); err != nil {
					w.log.Warn("flush failed", zap.String("path", n.Path), zap.Error(err))
					w.maybeRootGone(n, err)
				}
			}
		}
	}
}

// FlushAll synchronously flushes all dirty nodes; used at graceful shutdown.
// Caps at deadline; remaining dirty state stays persisted for next startup.
func (w *Writer) FlushAll(deadline time.Duration) {
	stop := time.Now().Add(deadline)
	for time.Now().Before(stop) {
		dirty, err := w.nodes.ListDirty(50)
		if err != nil || len(dirty) == 0 {
			return
		}
		for _, n := range dirty {
			if time.Now().After(stop) {
				return
			}
			_ = w.FlushOne(n.Path)
		}
	}
}
