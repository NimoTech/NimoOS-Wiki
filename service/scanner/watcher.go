package scanner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/NimoTech/NimoOS-Wiki/pkg/ignore"
	"github.com/NimoTech/NimoOS-Wiki/service/repo"
	"github.com/fsnotify/fsnotify"
	"go.uber.org/zap"
)

// UserNotesSyncTask is what the Watcher sends when it detects an external edit
// to a node's .wiki.md (i.e., something other than WikiWriter modified it).
// The EventProcessor consumes these from Watcher.SyncOut.
type UserNotesSyncTask struct {
	RootID     string
	WikiMDPath string // absolute path to the .wiki.md file
	// NodePath is the absolute path of the directory owning the wiki node.
	// IMPORTANT: this string is used as the key for nodelock.Locks.Lock(),
	// and MUST match exactly what WikiWriter.FlushOne uses as its nodePath
	// argument (which is wiki_nodes.Path from the DB). The invariant holds
	// today because both originate from filepath.Clean'd paths set in
	// roots.Manager.Create — break this invariant and the per-node mutex
	// silently stops serializing.
	NodePath string
}

type Watcher struct {
	events *repo.FileEventsRepo
	nodes  *repo.WikiNodesRepo
	ig     *ignore.Matcher
	log    *zap.Logger

	mu    sync.Mutex
	roots map[string]string // rootID -> rootPath
	fsw   *fsnotify.Watcher

	// SyncOut is read by the EventProcessor for user-notes reverse-sync tasks.
	SyncOut chan UserNotesSyncTask
}

func NewWatcher(events *repo.FileEventsRepo, nodes *repo.WikiNodesRepo, ig *ignore.Matcher, log *zap.Logger) *Watcher {
	if log == nil {
		log = zap.NewNop()
	}
	return &Watcher{
		events:  events,
		nodes:   nodes,
		ig:      ig,
		log:     log,
		roots:   map[string]string{},
		SyncOut: make(chan UserNotesSyncTask, 64),
	}
}

// Watch registers a Root with the watcher. Recursively adds inotify watches for
// every non-container dir below rootPath. Idempotent across calls.
func (w *Watcher) Watch(rootID, rootPath string) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.fsw == nil {
		fsw, err := fsnotify.NewWatcher()
		if err != nil {
			return err
		}
		w.fsw = fsw
	}
	w.roots[rootID] = rootPath

	return filepath.WalkDir(rootPath, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if p != rootPath && w.ig.IsContainerDir(filepath.Base(p)) {
			return filepath.SkipDir
		}
		return w.fsw.Add(p)
	})
}

// Unwatch removes a Root from the watcher. Events already in flight for paths
// under the root are discarded because rootIDFor no longer matches; inotify
// watches at or below the root are removed best-effort.
func (w *Watcher) Unwatch(rootID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	rootPath, ok := w.roots[rootID]
	if !ok {
		return
	}
	delete(w.roots, rootID)
	if w.fsw == nil {
		return
	}
	for _, p := range w.fsw.WatchList() {
		if p == rootPath || strings.HasPrefix(p, rootPath+"/") {
			_ = w.fsw.Remove(p)
		}
	}
}

// Run processes events until ctx is cancelled. Call from a goroutine.
func (w *Watcher) Run(ctx context.Context) {
	if w.fsw == nil {
		return
	}
	defer w.fsw.Close()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			w.handle(ev)
		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			w.log.Warn("fsnotify error", zap.Error(err))
		}
	}
}

func (w *Watcher) handle(ev fsnotify.Event) {
	base := filepath.Base(ev.Name)

	// PATH 1: discard tmp/system-noise events
	if w.ig.IsWikiTmpFile(base) || w.ig.IsSystemIgnoredBasename(base) {
		return
	}

	rootID := w.rootIDFor(ev.Name)
	if rootID == "" {
		return
	}

	// PATH 2: .wiki.md needs special routing to avoid WikiWriter self-loops
	// and to capture external edits for reverse sync.
	if w.ig.IsWikiFile(base) {
		w.handleWikiFile(rootID, ev)
		return
	}

	// PATH 3: regular file/dir

	// Defensive: ignore events whose parent is a container dir. Must come
	// before the Create-dir branch so we don't accidentally register a watch
	// on a child of a container dir.
	if w.parentIsContainer(ev.Name) {
		return
	}

	// If a new dir is created, register it (unless container, in which case
	// record one opaque event and don't add a recursive watch).
	if ev.Op&fsnotify.Create != 0 {
		if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
			if w.ig.IsContainerDir(base) {
				w.insertEvent(rootID, ev.Name, "create", true)
				return
			}
			_ = w.fsw.Add(ev.Name)
			// Walk the new dir to backfill events for any pre-existing
			// descendants (cp -r, mv, tar x). fsnotify only emits Create on
			// the dir itself; descendants need discovery via a walk.
			w.backfillNewDir(rootID, ev.Name)
			// Note: the top-level Create event for ev.Name itself still gets
			// recorded via the normal path-3 flow below.
		}
	}

	op := mapOp(ev.Op)
	if op == "" {
		return
	}
	isDir := false
	if info, err := os.Stat(ev.Name); err == nil {
		isDir = info.IsDir()
	}
	w.insertEvent(rootID, ev.Name, op, isDir)
}

// handleWikiFile implements Spec §6.1 path 2.
//
// DELETE / RENAME (source side): user removed our .wiki.md. Mark the node dirty
// so WikiWriter regenerates from DB (user_notes is preserved in DB).
//
// CREATE / MODIFY / MOVED_TO: stat the file and compare mtime against
// wiki_nodes.last_flushed_mtime. If mtime equals what we recorded last time
// we wrote, this event is the echo of our own write — drop it (this is what
// prevents WikiWriter self-loops). Otherwise, enqueue a reverse-sync task.
func (w *Watcher) handleWikiFile(rootID string, ev fsnotify.Event) {
	wikiMDPath := ev.Name
	nodePath := filepath.Dir(wikiMDPath)
	node, err := w.nodes.Get(nodePath)
	if err != nil {
		w.log.Warn("wiki node lookup failed", zap.String("path", nodePath), zap.Error(err))
		return
	}
	if node == nil {
		// User dropped a .wiki.md in a directory that isn't a wiki node.
		// Treat as noise.
		return
	}

	if ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
		_ = w.nodes.SetDirty(nodePath, true)
		w.log.Info("wiki.md removed externally; marking dirty",
			zap.String("path", nodePath))
		return
	}

	info, err := os.Stat(wikiMDPath)
	if err != nil {
		// File vanished between event and stat — treat as a delete echo.
		w.log.Warn("wiki.md stat failed", zap.String("path", wikiMDPath), zap.Error(err))
		return
	}
	mtime := info.ModTime().UnixMilli()
	if mtime <= node.LastFlushedMtime {
		// This is the echo of our own write (or older).
		return
	}

	select {
	case w.SyncOut <- UserNotesSyncTask{RootID: rootID, WikiMDPath: wikiMDPath, NodePath: nodePath}:
	default:
		w.log.Warn("user-notes sync queue full; dropping task", zap.String("path", nodePath))
	}
}

func (w *Watcher) insertEvent(rootID, p, op string, isDir bool) {
	_ = w.events.Insert(repo.FileEvent{
		ID: repo.NewID(), RootID: rootID, Path: p, Op: op,
		IsDir: isDir, DetectedAt: time.Now().UnixMilli(),
	})
}

func mapOp(op fsnotify.Op) string {
	switch {
	case op&fsnotify.Create != 0:
		return "create"
	case op&fsnotify.Write != 0:
		return "modify"
	case op&fsnotify.Remove != 0:
		return "delete"
	case op&fsnotify.Rename != 0:
		return "rename"
	}
	return ""
}

func (w *Watcher) rootIDFor(p string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	for id, root := range w.roots {
		if p == root || strings.HasPrefix(p, root+"/") {
			return id
		}
	}
	return ""
}

func (w *Watcher) parentIsContainer(p string) bool {
	parent := filepath.Dir(p)
	return w.ig.IsContainerDir(filepath.Base(parent))
}

// backfillNewDir handles dirs that may have contained entries at creation time
// (e.g., cp -r, mv, tar x). fsnotify only emits Create on the dir itself;
// descendants need to be discovered via a walk.
func (w *Watcher) backfillNewDir(rootID, dir string) {
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if p == dir {
			return nil // already handled by caller
		}
		base := filepath.Base(p)
		if w.ig.IsSystemIgnoredBasename(base) || w.ig.IsWikiFile(base) || w.ig.IsWikiTmpFile(base) {
			return nil
		}
		if d.IsDir() && w.ig.IsContainerDir(base) {
			w.insertEvent(rootID, p, "create", true)
			return filepath.SkipDir
		}
		if d.IsDir() {
			if err := w.fsw.Add(p); err != nil {
				w.log.Warn("backfill: fsw.Add failed", zap.String("path", p), zap.Error(err))
			}
		}
		w.insertEvent(rootID, p, "create", d.IsDir())
		return nil
	})
}
