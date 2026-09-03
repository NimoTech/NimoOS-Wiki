package scanner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/NimoTech/NimoOS-Wiki/pkg/ignore"
	"github.com/NimoTech/NimoOS-Wiki/service/repo"
	"github.com/fsnotify/fsnotify"
	"go.uber.org/zap"
)

// ErrWatchLimit is returned by Watch when the kernel refuses more inotify
// watches (fs.inotify.max_user_watches exhausted). Callers should degrade
// the root to scan_only (spec §4.3).
var ErrWatchLimit = errors.New("inotify watch limit reached")

// ErrWatchRootFailed means the root directory itself could not be watched
// (EACCES etc. — not a watch-limit). Child-dir failures stay best-effort,
// but a root we can't even watch must not silently sit in watch_mode=auto
// pretending to have live events.
var ErrWatchRootFailed = errors.New("cannot watch root directory")

func isWatchLimit(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EMFILE) ||
		errors.Is(err, syscall.ENFILE)
}

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

	// Guard is the two-level storm fuse (spec §4.1). When IsStorming(rootID)
	// is true, insertEvent/backfillNewDir drop events for that root instead
	// of inserting them — reconcile owns the truth while the fuse is open.
	// Nil is a safe no-op: nil Guard never storms.
	Guard *StormGuard

	// SyncOut is read by the EventProcessor for user-notes reverse-sync tasks.
	SyncOut chan UserNotesSyncTask

	// pendingSync buffers UserNotesSyncTasks that could not be delivered
	// because SyncOut was full. Keyed by NodePath so repeated external edits
	// to the same node collapse to the latest task. Retried from Run on a
	// ticker; bounded naturally by the number of wiki nodes (= roots).
	pendingMu   sync.Mutex
	pendingSync map[string]UserNotesSyncTask

	// OnWatchLimit is invoked (if non-nil) when a runtime fsw.Add hits the
	// inotify watch-limit while handling a Create event for rootID. Callers
	// should degrade the root to scan_only; the callback must be idempotent
	// since it may fire more than once for the same root.
	OnWatchLimit func(rootID string)

	// ExcludePrefixes are absolute, Clean'd directories whose subtree is never
	// watched and whose events are always dropped (spec §3.5) — e.g. the
	// wiki's own DataPath, so writes to wiki.db-wal cannot feed back into
	// file_events even when DataPath physically lives under a root
	// (/DATA/.system_data/nimoos/wiki on stock installs). Set before Watch.
	ExcludePrefixes []string
}

func NewWatcher(events *repo.FileEventsRepo, nodes *repo.WikiNodesRepo, ig *ignore.Matcher, guard *StormGuard, log *zap.Logger) *Watcher {
	if log == nil {
		log = zap.NewNop()
	}
	return &Watcher{
		events:      events,
		nodes:       nodes,
		ig:          ig,
		log:         log,
		roots:       map[string]string{},
		Guard:       guard,
		SyncOut:     make(chan UserNotesSyncTask, 64),
		pendingSync: map[string]UserNotesSyncTask{},
	}
}

// Watch registers a Root with the watcher. Recursively adds inotify watches for
// every non-container dir below rootPath. Idempotent across calls.
func (w *Watcher) Watch(rootID, rootPath string) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if err := w.ensureFSWLocked(); err != nil {
		return err
	}
	w.roots[rootID] = rootPath

	var hitLimit bool
	var rootAddErr error
	walkErr := filepath.WalkDir(rootPath, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if p == rootPath {
				rootAddErr = err
			}
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if p != rootPath && (w.ig.IsContainerDir(filepath.Base(p)) || w.isExcluded(p)) {
			return filepath.SkipDir
		}
		if err := w.fsw.Add(p); err != nil {
			if isWatchLimit(err) {
				hitLimit = true
				return filepath.SkipAll
			}
			if p == rootPath {
				rootAddErr = err
			}
			w.log.Warn("fsw.Add", zap.String("path", p), zap.Error(err))
		}
		return nil
	})
	if walkErr != nil {
		return walkErr
	}
	if hitLimit {
		return ErrWatchLimit
	}
	if rootAddErr != nil {
		return fmt.Errorf("%w: %v", ErrWatchRootFailed, rootAddErr)
	}
	return nil
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

// ensureFSWLocked lazily creates the fsnotify watcher. Caller holds w.mu.
// The watcher is created once and never replaced, so Run can read
// w.fsw.Events without the lock after this returns.
func (w *Watcher) ensureFSWLocked() error {
	if w.fsw != nil {
		return nil
	}
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	w.fsw = fsw
	return nil
}

// Run processes events until ctx is cancelled. Call from a goroutine.
//
// Run creates the fsnotify watcher itself if no root has been registered yet
// (fresh install, or every root disabled at boot). Previously it returned
// immediately in that case; a root added later through the API then created
// the watcher lazily inside Watch, but nothing was reading its Events channel,
// so inotify delivery stalled and live watching was dead until restart.
func (w *Watcher) Run(ctx context.Context) {
	w.mu.Lock()
	err := w.ensureFSWLocked()
	w.mu.Unlock()
	if err != nil {
		w.log.Error("fsnotify: cannot create watcher; live file watching disabled, reconcile only", zap.Error(err))
		return
	}
	defer w.fsw.Close()
	retry := time.NewTicker(10 * time.Second)
	defer retry.Stop()
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
		case <-retry.C:
			w.retryPendingSync()
		}
	}
}

func (w *Watcher) handle(ev fsnotify.Event) {
	base := filepath.Base(ev.Name)

	// PATH 1: discard tmp/system-noise events
	if w.ig.IsWikiTmpFile(base) || w.ig.IsSystemIgnoredBasename(base) {
		return
	}

	// PATH 1b: never record anything under an excluded prefix (own DataPath)
	// or below a container dir — even if a stale kernel watch delivers it.
	if w.isExcluded(ev.Name) {
		return
	}
	rootID := w.rootIDFor(ev.Name)
	if rootID == "" {
		return
	}
	if hasContainerAncestor(w.ig, w.rootPathFor(rootID), ev.Name) {
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
			if err := w.fsw.Add(ev.Name); err != nil && isWatchLimit(err) && w.OnWatchLimit != nil {
				w.OnWatchLimit(rootID)
			}
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

	w.sendSync(UserNotesSyncTask{RootID: rootID, WikiMDPath: wikiMDPath, NodePath: nodePath})
}

func (w *Watcher) insertEvent(rootID, p, op string, isDir bool) {
	if w.Guard != nil && w.Guard.IsStorming(rootID) {
		return // fuse open: events are droppable, reconcile owns the truth
	}
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

func (w *Watcher) isExcluded(p string) bool {
	for _, ex := range w.ExcludePrefixes {
		if p == ex || strings.HasPrefix(p, ex+"/") {
			return true
		}
	}
	return false
}

func (w *Watcher) rootPathFor(rootID string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.roots[rootID]
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
		if w.Guard != nil && w.Guard.IsStorming(rootID) {
			return filepath.SkipAll // fuse open: abandon the whole backfill, reconcile owns the truth
		}
		if p == dir {
			return nil // already handled by caller
		}
		if w.isExcluded(p) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
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
				if isWatchLimit(err) && w.OnWatchLimit != nil {
					w.OnWatchLimit(rootID)
				} else {
					w.log.Warn("backfill: fsw.Add failed", zap.String("path", p), zap.Error(err))
				}
			}
		}
		w.insertEvent(rootID, p, "create", d.IsDir())
		return nil
	})
}

// sendSync delivers a reverse-sync task, buffering it when SyncOut is full
// instead of dropping it (external edits must eventually reach the DB).
func (w *Watcher) sendSync(t UserNotesSyncTask) {
	select {
	case w.SyncOut <- t:
	default:
		w.pendingMu.Lock()
		w.pendingSync[t.NodePath] = t
		w.pendingMu.Unlock()
		w.log.Warn("user-notes sync queue full; task buffered for retry",
			zap.String("path", t.NodePath))
	}
}

// retryPendingSync re-attempts buffered tasks without blocking. Tasks that
// still don't fit stay buffered for the next tick.
func (w *Watcher) retryPendingSync() {
	w.pendingMu.Lock()
	defer w.pendingMu.Unlock()
	for k, t := range w.pendingSync {
		select {
		case w.SyncOut <- t:
			delete(w.pendingSync, k)
			w.log.Debug("buffered user-notes sync delivered", zap.String("path", k))
		default:
			return // channel full again; keep the rest
		}
	}
}

// hasContainerAncestor reports whether any path segment strictly between
// rootPath and p is a container dir per the matcher. Paths outside rootPath
// never match (Rel yields ".."; ".." is not a container dir).
func hasContainerAncestor(ig *ignore.Matcher, rootPath, p string) bool {
	rel, err := filepath.Rel(rootPath, filepath.Dir(p))
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return false
	}
	for _, seg := range strings.Split(rel, string(filepath.Separator)) {
		if ig.IsContainerDir(seg) {
			return true
		}
	}
	return false
}
