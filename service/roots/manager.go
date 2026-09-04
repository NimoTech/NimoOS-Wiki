// Package roots manages Wiki Root lifecycle: registration with write-test,
// FS-type detection for watch_mode auto-downgrade, and listing.
package roots

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/NimoTech/NimoOS-Wiki/common"
	"github.com/NimoTech/NimoOS-Wiki/pkg/ignore"
	"github.com/NimoTech/NimoOS-Wiki/service/eventbus"
	"github.com/NimoTech/NimoOS-Wiki/service/repo"
	"github.com/NimoTech/NimoOS-Wiki/service/rootsync"
	"github.com/NimoTech/NimoOS-Wiki/service/scanner"
	"go.uber.org/zap"
)

// pushTimeout is the per-call timeout for the manager's incremental push
// (Upsert/Delete) to core: core is a LAN-local service, so 3s is plenty; a
// timeout is treated as a failure too, leaving the caller to set
// needs_reconcile.
const pushTimeout = 3 * time.Second

// pusher is the manager's minimal dependency on the core authz push (makes it
// easy for tests to inject a fake pusher, and keeps the manager from
// depending on rootsync.Client's full implementation details).
type pusher interface {
	Upsert(ctx context.Context, g rootsync.Grant) error
	Delete(ctx context.Context, rootID string) error
}

type Manager struct {
	roots  *repo.WikiRootsRepo
	nodes  *repo.WikiNodesRepo
	files  *repo.FileIndexRepo
	events *repo.FileEventsRepo
	bus    eventbus.Bus
	watch  Watch
	ig     *ignore.Matcher

	// pusher pushes root lifecycle changes to the core authz source (the
	// authz-source-decoupling project); nil (tests, not-wired-up scenarios)
	// means the push is skipped, and the startup full Reconcile still covers
	// it.
	pusher pusher
	// log is used to Warn on push failures; defaults to no-op, main injects
	// the real logger via SetLogger.
	log *zap.Logger

	// authzDirty is the in-memory dirty flag set when pushDelete fails
	// (critical fix, option B): on delete failure the root row is already
	// gone, so there's no row left in the DB to persist a marker on — the
	// only way to tell main.go's dedicated retry loop "there's leftover authz
	// state to correct" is this in-memory flag, which the next tick covers
	// with a full idempotent Reconcile. On restart it's naturally covered by
	// bootReconcileRootSync's startup full reconcile, so it never needs to be
	// persisted. Atomic: pushDelete writes it from a business goroutine while
	// the retry loop reads/clears it from its own goroutine, avoiding a data
	// race.
	authzDirty atomic.Bool

	// PrecheckDirLimit / PrecheckTimeout bound the size precheck run in
	// Create (spec §4.4). Zero-value PrecheckDirLimit skips the precheck
	// entirely (keeps pre-Task-7 tests green); main injects both from config.
	PrecheckDirLimit int
	PrecheckTimeout  time.Duration
}

// NewManager wires the Manager. ig may be nil (tests) — countDirsQuick then
// falls back to counting every directory raw, including container dirs.
func NewManager(roots *repo.WikiRootsRepo, nodes *repo.WikiNodesRepo,
	files *repo.FileIndexRepo, events *repo.FileEventsRepo,
	bus eventbus.Bus, ig *ignore.Matcher) *Manager {
	if bus == nil {
		bus = eventbus.Noop{}
	}
	return &Manager{roots: roots, nodes: nodes, files: files, events: events, bus: bus, ig: ig, log: zap.NewNop()}
}

// SetPusher wires the core-authority push client (called once from main,
// after rootsync.New()). Nil (tests, CLI) means push is skipped.
func (m *Manager) SetPusher(p pusher) { m.pusher = p }

// SetLogger wires the process-wide zap logger (called once from main).
// A nil argument is ignored, keeping the no-op default from NewManager.
func (m *Manager) SetLogger(l *zap.Logger) {
	if l != nil {
		m.log = l
	}
}

// pushUpsert best-effort pushes the root's current state (id/path/enabled) to
// core. On failure (network error, core not ready, timeout, etc.) it only
// Warns and marks the root needs_authz_push (critical fix, option B: a
// dedicated field, no longer piggybacking on needs_reconcile's FS-rescan
// semantics) — it never returns an error or blocks the caller, following the
// same philosophy as the existing MessageBus soft dependency; the actual
// retry is done by main.go's dedicated authz push retry loop, which consumes
// this flag and corrects it via a full idempotent Reconcile.
func (m *Manager) pushUpsert(id, path string, enabled bool) {
	if m.pusher == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), pushTimeout)
	defer cancel()
	g := rootsync.Grant{RootID: id, Path: path, Enabled: enabled}
	if err := m.pusher.Upsert(ctx, g); err != nil {
		m.log.Warn("push root grant upsert failed; marked needs_authz_push",
			zap.String("root_id", id), zap.Error(err))
		_ = m.roots.SetNeedsAuthzPush(id, true)
	}
}

// pushDelete best-effort syncs a root deletion to core; on failure the root
// row has already been removed by m.roots.Delete, so there's no row left in
// the DB to mark needs_authz_push on — instead it sets the in-memory dirty
// flag authzDirty, leaving the retry loop to cover it via a full Reconcile
// (see the authzDirty field comment).
func (m *Manager) pushDelete(id string) {
	if m.pusher == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), pushTimeout)
	defer cancel()
	if err := m.pusher.Delete(ctx, id); err != nil {
		m.log.Warn("push root grant delete failed; marked authz dirty",
			zap.String("root_id", id), zap.Error(err))
		m.authzDirty.Store(true)
	}
}

// AuthzDirty reports whether there's a pending, non-persistable authz
// leftover to correct (raised when pushDelete failed), for main.go's
// dedicated retry loop to decide whether this tick needs to trigger a full
// Reconcile.
func (m *Manager) AuthzDirty() bool { return m.authzDirty.Load() }

// ClearAuthzDirty clears the in-memory dirty flag; called after a successful
// full Reconcile in the retry loop.
func (m *Manager) ClearAuthzDirty() { m.authzDirty.Store(false) }

// ConsumeAuthzDirty atomically reads and clears the in-memory dirty flag
// (swaps it to false), for the retry loop's consume-then-act hardening: the
// signal is "consumed" right at the start of the tick, instead of waiting for
// Reconcile to finish before doing a blanket clear — this way a signal raised
// in the window after consumption isn't swallowed by the current tick. If the
// tick ends up not actually completing the reconcile (List/Reconcile failed),
// the caller must use MarkAuthzDirty to re-raise the signal for the next tick
// to retry.
func (m *Manager) ConsumeAuthzDirty() bool { return m.authzDirty.Swap(false) }

// MarkAuthzDirty re-raises the in-memory dirty flag. Used when the retry loop
// has consumed the signal via ConsumeAuthzDirty but then failed to actually
// complete the reconcile (List/Reconcile failed), so the signal must be
// re-raised — otherwise the consume-then-act reordering would lose a pending
// authz drift correction.
func (m *Manager) MarkAuthzDirty() { m.authzDirty.Store(true) }

// Watch is the subset of scanner.Watcher the Manager drives when roots are
// created, deleted, enabled or disabled at runtime. Nil (tests, CLI) means
// DB-only: the reconciler still covers the root on its next tick.
type Watch interface {
	Watch(rootID, rootPath string) error
	Unwatch(rootID string)
}

// SetWatch wires the runtime fsnotify watcher (called once from main).
func (m *Manager) SetWatch(w Watch) { m.watch = w }

// DegradeToScanOnly flips a root to scan_only after an inotify watch-limit
// hit and announces it. Idempotent.
func (m *Manager) DegradeToScanOnly(rootID, reason string) {
	if err := m.roots.SetWatchMode(rootID, "scan_only"); err != nil {
		return
	}
	if m.watch != nil {
		m.watch.Unwatch(rootID)
	}
	hint := "raise fs.inotify.max_user_watches (recommended 524288)"
	if reason == "watch_error" {
		hint = "check the root directory's permissions"
	}
	m.bus.Publish(common.EventWatchDegraded, map[string]any{
		"root_id": rootID, "reason": reason, "hint": hint,
	})
}

// DisableForMissingPath disables a root whose on-disk path vanished at
// runtime (drive pulled, directory deleted) so WikiWriter stops retrying
// ENOENT forever (spec 2026-05-13 §9). wiki_nodes rows and user_notes are
// kept: re-enabling after the path returns restores everything. Idempotent;
// re-checks the path so a racing re-create is a no-op.
func (m *Manager) DisableForMissingPath(rootID string) {
	root, err := m.roots.Get(rootID)
	if err != nil || !root.Enabled {
		return
	}
	if _, statErr := os.Stat(root.Path); statErr == nil {
		return
	}
	if err := m.roots.SetEnabled(rootID, false); err != nil {
		return
	}
	if m.watch != nil {
		m.watch.Unwatch(rootID)
	}
	m.bus.Publish(common.EventRootDisabled, map[string]any{
		"root_id": rootID, "path": root.Path, "level": root.Level,
		"reason": "path_missing",
	})
	m.pushUpsert(rootID, root.Path, false)
	m.log.Warn("wiki root path missing; root disabled",
		zap.String("root_id", rootID), zap.String("path", root.Path))
}

type CreateArgs struct {
	Path          string
	Level         string // 'space' | 'project'
	WatchMode     string // 'auto' | 'scan_only'  (default 'auto')
	StorageMode   string // 'inline' | 'mirror'    (default 'inline')
	ScanIntervalS int
}

var (
	ErrPathNotWritable = errors.New("path is not writable (set storage_mode=mirror to use a central mirror)")
	ErrInvalidArgs     = errors.New("invalid arguments")
	ErrPathNotExist    = errors.New("path does not exist")
)

// Create registers a new Wiki Root after validation:
//  1. path must be absolute and exist
//  2. level must be 'space' or 'project'
//  3. for inline storage_mode: writeTest must succeed
//  4. FS type detection — nfs/cifs/fuse → force scan_only
//
// Returns the new Root ID and a modeReason: "" normally, or "large_root" when
// the size precheck (spec §4.4) degraded watch_mode to scan_only.
func (m *Manager) Create(args CreateArgs) (string, string, error) {
	if !filepath.IsAbs(args.Path) {
		return "", "", fmt.Errorf("%w: path must be absolute", ErrInvalidArgs)
	}
	args.Path = filepath.Clean(args.Path)

	info, err := os.Stat(args.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", ErrPathNotExist
		}
		return "", "", err
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("%w: path is not a directory", ErrInvalidArgs)
	}

	switch args.Level {
	case "space", "project":
	default:
		return "", "", fmt.Errorf("%w: level must be 'space' or 'project'", ErrInvalidArgs)
	}

	if args.WatchMode == "" {
		args.WatchMode = "auto"
	}
	if args.StorageMode == "" {
		args.StorageMode = "inline"
	}
	if args.ScanIntervalS <= 0 {
		args.ScanIntervalS = 21600
	}

	// FS-type-driven watch_mode downgrade
	if args.WatchMode == "auto" {
		switch DetectFSType(args.Path) {
		case "nfs", "nfs4", "cifs", "smb", "fuse", "fuseblk":
			args.WatchMode = "scan_only"
		}
	}

	// Size precheck (spec §4.4): a root too big to even count quickly gets
	// scan_only automatically — never rejected. Timeout counts as exceeded
	// (a tree we can't enumerate 20k dirs of in 2s is huge or on slow storage;
	// scan_only is the safe mode either way). The publish is deferred until
	// after the Root ID is allocated below, so the event carries a real root_id.
	var precheckExceeded bool
	var dirsCounted int
	if args.WatchMode == "auto" && m.PrecheckDirLimit > 0 {
		if n, exceeded := countDirsQuick(args.Path, m.PrecheckDirLimit, m.PrecheckTimeout, m.ig); exceeded {
			args.WatchMode = "scan_only"
			precheckExceeded = true
			dirsCounted = n
		}
	}

	if args.StorageMode == "inline" {
		if err := writeTest(args.Path); err != nil {
			return "", "", ErrPathNotWritable
		}
	}

	id := repo.NewID()
	now := time.Now().UnixMilli()
	if err := m.roots.Insert(repo.WikiRoot{
		ID: id, Path: args.Path, Level: args.Level,
		WatchMode: args.WatchMode, StorageMode: args.StorageMode,
		Enabled: true, ScanIntervalS: args.ScanIntervalS, CreatedAt: now,
	}); err != nil {
		return "", "", err
	}

	modeReason := ""
	if precheckExceeded {
		modeReason = "large_root"
		m.bus.Publish(common.EventWatchDegraded, map[string]any{
			"root_id": id, "path": args.Path, "reason": "large_root",
			"dirs_counted": dirsCounted,
		})
	}

	// Seed an initial wiki_node for the Root itself. Mark dirty so WikiWriter
	// produces an initial .wiki.md on first flush; otherwise the file only
	// appears after the first file change in the Root.
	rootIDCopy := id
	_ = m.nodes.Upsert(repo.WikiNode{
		ID: repo.NewID(), RootID: &rootIDCopy, Path: args.Path,
		Level: args.Level, Dirty: true, UpdatedAt: now,
	})

	// Fix: previously a root created at runtime only got fsnotify after a
	// service restart (the reconciler alone covered it, at up to 30s latency).
	if m.watch != nil && args.WatchMode == "auto" {
		if err := m.watch.Watch(id, args.Path); err != nil {
			switch {
			case errors.Is(err, scanner.ErrWatchLimit):
				m.DegradeToScanOnly(id, "watch_limit")
			case errors.Is(err, scanner.ErrWatchRootFailed):
				m.DegradeToScanOnly(id, "watch_error")
			}
		}
	}

	m.bus.Publish(common.EventRootEnabled, map[string]any{
		"root_id": id,
		"path":    args.Path,
		"level":   args.Level,
	})

	m.pushUpsert(id, args.Path, true)

	return id, modeReason, nil
}

// Delete removes a Root. If purgeFiles is true, also removes the .wiki.md files
// under the Root for all its wiki_nodes (best-effort).
func (m *Manager) Delete(id string, purgeFiles bool) error {
	root, err := m.roots.Get(id)
	if err != nil {
		return err
	}

	if purgeFiles {
		nodes, _ := m.nodes.List(id)
		for _, n := range nodes {
			_ = os.Remove(filepath.Join(n.Path, ".wiki.md"))
		}
	}

	nodes, _ := m.nodes.List(id)
	for _, n := range nodes {
		_ = m.nodes.Delete(n.Path)
	}

	// Cascade cleanup (2026-07-20 follow-up): without this, deleting a root
	// leaves every file_index row behind and — because the Parser only learns
	// about removals through delete events — permanently leaks the root's
	// records and Qdrant vectors on the Parser side.
	if m.files != nil && m.events != nil {
		now := time.Now().UnixMilli()
		// 1) A dying root's pending create/modify/rename events are moot.
		//    Its op='delete' events are kept for the Parser's cursor.
		if _, err := m.events.PurgeByRootExceptDeletes(id); err != nil {
			return err
		}
		// 2) One root_removed event. The Parser retires every record under
		//    this root_id in a single pass (service_retire.retire_root); the
		//    previous per-file delete fan-out queued one job per tracked file
		//    (40k on an OS-overlay root) and was almost entirely no-ops.
		//    Pre-marked processed_at: our own processor must not consume it
		//    and it must not count as backlog for the storm fuse; the
		//    internal feed returns processed rows anyway.
		if err := m.events.InsertBatch([]repo.FileEvent{{
			RootID: id, Path: "", Op: "root_removed", IsDir: false,
			DetectedAt: now, ProcessedAt: now,
		}}); err != nil {
			return err
		}
		// 3) Drop the root's file_index rows.
		if _, err := m.files.DeleteByRoot(id); err != nil {
			return err
		}
	}

	if err := m.roots.Delete(id); err != nil {
		return err
	}

	if m.watch != nil {
		m.watch.Unwatch(id)
	}

	m.bus.Publish(common.EventRootDisabled, map[string]any{
		"root_id": id,
		"path":    root.Path,
		"level":   root.Level,
	})

	m.pushDelete(id)

	return nil
}

// Rescan touches last_scan_at to 0 so the next reconciler tick treats this
// Root as overdue. Caller-side, the main loop wakes on its ticker; this call
// makes the next tick reconcile immediately.
func (m *Manager) Rescan(id string) error {
	return m.roots.UpdateLastScanAt(id, 0)
}

// SetEnabled flips a root's enabled flag with immediate effect: disabling
// stops its fsnotify watcher (events for the root are discarded from now on);
// enabling re-registers the watcher and marks the root overdue so the
// reconciler catches up on changes made while it was disabled.
// Returns repo.ErrNotFound for an unknown id.
func (m *Manager) SetEnabled(id string, enabled bool) error {
	root, err := m.roots.Get(id)
	if err != nil {
		return err
	}
	if root.Enabled == enabled {
		return nil
	}
	if err := m.roots.SetEnabled(id, enabled); err != nil {
		return err
	}
	payload := map[string]any{"root_id": id, "path": root.Path, "level": root.Level}
	if enabled {
		_ = m.roots.UpdateLastScanAt(id, 0)
		if m.watch != nil && root.WatchMode == "auto" {
			if err := m.watch.Watch(root.ID, root.Path); err != nil {
				switch {
				case errors.Is(err, scanner.ErrWatchLimit):
					m.DegradeToScanOnly(root.ID, "watch_limit")
				case errors.Is(err, scanner.ErrWatchRootFailed):
					m.DegradeToScanOnly(root.ID, "watch_error")
				}
			}
		}
		m.bus.Publish(common.EventRootEnabled, payload)
	} else {
		if m.watch != nil {
			m.watch.Unwatch(root.ID)
		}
		m.bus.Publish(common.EventRootDisabled, payload)
	}

	m.pushUpsert(id, root.Path, enabled)

	return nil
}

func writeTest(path string) error {
	f := filepath.Join(path, ".nimoos-wiki-write-test")
	if err := os.WriteFile(f, []byte("test"), 0644); err != nil {
		return err
	}
	return os.Remove(f)
}

// countDirsQuick walks path counting directories, stopping early once limit
// is reached or timeout elapses (checked every 256 dirs to keep the deadline
// check cheap). exceeded is true if the count hit limit or the walk timed
// out before finishing — both cases mean "too big/slow to safely watch".
//
// Container dirs (spec §4.4 "skip container directories", e.g. /DATA/.system_data) are
// skipped entirely — neither counted nor descended into — so their bulk
// never false-positives an otherwise-small root into scan_only. ig may be
// nil (tests, or callers with no matcher wired), in which case every
// directory is counted raw.
func countDirsQuick(path string, limit int, timeout time.Duration, ig *ignore.Matcher) (int, bool) {
	deadline := time.Now().Add(timeout)
	n := 0
	timedOut := false
	_ = filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if d.IsDir() && p != path && ig != nil && ig.IsContainerDir(filepath.Base(p)) {
			return filepath.SkipDir
		}
		n++
		if n >= limit {
			return filepath.SkipAll
		}
		if n%256 == 0 && time.Now().After(deadline) {
			timedOut = true
			return filepath.SkipAll
		}
		return nil
	})
	return n, n >= limit || timedOut
}

// DetectFSType reads /proc/mounts and finds the FS type for the longest
// mountpoint prefix matching path. Returns "unknown" on any error.
func DetectFSType(path string) string {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return "unknown"
	}
	bestMP := ""
	bestType := "unknown"
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		mp, fstype := fields[1], fields[2]
		// Match: exact or descendant
		if (path == mp || strings.HasPrefix(path, mp+"/")) && len(mp) > len(bestMP) {
			bestMP = mp
			bestType = fstype
		}
	}
	return bestType
}
