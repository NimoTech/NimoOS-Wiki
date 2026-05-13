package writer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/NimoTech/NimoOS-Wiki/common"
	"github.com/NimoTech/NimoOS-Wiki/pkg/childmap"
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
	debounceWindow     time.Duration
	aggregateThreshold int
	log                *zap.Logger
}

func NewWriter(nodes *repo.WikiNodesRepo, files *repo.FileIndexRepo,
	events *repo.FileEventsRepo, bus eventbus.Bus,
	debounceWindow time.Duration, log *zap.Logger) *Writer {
	if log == nil {
		log = zap.NewNop()
	}
	if bus == nil {
		bus = eventbus.Noop{}
	}
	return &Writer{
		nodes: nodes, files: files, events: events, bus: bus,
		debounceWindow: debounceWindow, aggregateThreshold: 50, log: log,
	}
}

// FlushOne renders + writes one node's .wiki.md.
func (w *Writer) FlushOne(nodePath string) error {
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

	doc, err := w.buildDoc(node)
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
func (w *Writer) buildDoc(node *repo.WikiNode) (wikimd.Doc, error) {
	rootID := ""
	if node.RootID != nil {
		rootID = *node.RootID
	}

	// Child map from file_index (direct children only)
	children, err := w.files.ListByParent(rootID, node.Path)
	if err != nil {
		return wikimd.Doc{}, err
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
	}, nil
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
			return fmt.Sprintf("%d 个文件 (已跳过)", g.ChildFileCount)
		}
		if g.ChildFileCount > 0 {
			return fmt.Sprintf("%d 个文件", g.ChildFileCount)
		}
		return "目录"
	}
	if g.IsAggregate {
		return fmt.Sprintf("%d 个 .%s", g.Count, g.Ext)
	}
	return "文件"
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
