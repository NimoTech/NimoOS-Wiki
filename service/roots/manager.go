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

// pushTimeout 是 manager 向核心增量推送(Upsert/Delete)单次调用的超时:核心是
// 内网服务,3s 足够;超时也按失败处理,交由调用方置 needs_reconcile。
const pushTimeout = 3 * time.Second

// pusher 是 manager 对核心授权推送的最小依赖(便于测试注入 fake pusher,
// 也避免 manager 直接依赖 rootsync.Client 的完整实现细节)。
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

	// pusher 推送 root 生命周期变化给核心授权源(授权源解耦项目);nil(测试、
	// 未接线场景)意味着跳过推送,启动时的全量 Reconcile 仍会兜底。
	pusher pusher
	// log 用于推送失败时的 Warn;默认 no-op,main 通过 SetLogger 注入真实 logger。
	log *zap.Logger

	// authzDirty 是 pushDelete 失败时的内存脏标(Critical 修复,方案 B):delete
	// 失败时该 root 行已经被删,DB 里无处落盘持久标记,只能靠这个内存标志告诉
	// main.go 的专用重试循环"存在待纠正的授权残留",下一轮 tick 用全量幂等
	// Reconcile 兜底;重启则天然由 bootReconcileRootSync 的启动全量对账覆盖,
	// 无需持久化。原子操作:pushDelete 在业务 goroutine 里写,重试循环在自己的
	// goroutine 里读/清,避免数据竞争。
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

// pushUpsert 尽力而为地把 root 的当前状态(id/path/enabled)推给核心。推送
// 失败(网络错误、核心未就绪、超时等)仅记录 Warn 并把该 root 标记为
// needs_authz_push(Critical 修复,方案 B:独立字段,不再误用 FS 重扫语义的
// needs_reconcile)——不返回 error、不阻塞调用方,与既有 MessageBus 软依赖同一
// 哲学;真正的重试由 main.go 的专用授权推送重试循环消费该标记,统一走全量
// 幂等 Reconcile 纠正。
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

// pushDelete 尽力而为地把 root 删除同步给核心;失败时该 root 行已被
// m.roots.Delete 删除,DB 里没有行可落 needs_authz_push 标记,于是改置内存脏标
// authzDirty,交给重试循环靠全量 Reconcile 兜底(见 authzDirty 字段注释)。
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

// AuthzDirty 报告是否存在因 pushDelete 失败而产生的、无法持久化的授权残留
// 待纠正信号,供 main.go 的专用重试循环判断本轮 tick 是否需要触发一次全量
// Reconcile。
func (m *Manager) AuthzDirty() bool { return m.authzDirty.Load() }

// ClearAuthzDirty 清除内存脏标,在重试循环里的全量 Reconcile 成功后调用。
func (m *Manager) ClearAuthzDirty() { m.authzDirty.Store(false) }

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
		// 2) One delete tombstone per indexed file so the Parser drops its
		//    records (content-addressed refcounting keeps shared content
		//    alive; real vector deletion happens after its 24h GC grace).
		//    Pre-marked processed_at: our own processor must not re-consume
		//    them, and they must not count as backlog for the storm fuse —
		//    the internal file-events feed returns processed rows anyway
		//    (ListSince filters on archived only). Same-millisecond bursts
		//    are safe for the Parser via seq keyset pagination.
		after := ""
		for {
			batch, err := m.files.ListByRootAfter(id, after, 5000)
			if err != nil {
				return err
			}
			if len(batch) == 0 {
				break
			}
			evs := make([]repo.FileEvent, 0, len(batch))
			for _, f := range batch {
				if f.IsDir || f.Status != "present" {
					continue
				}
				evs = append(evs, repo.FileEvent{
					RootID: id, Path: f.Path, Op: "delete",
					DetectedAt: now, ProcessedAt: now,
				})
			}
			if err := m.events.InsertBatch(evs); err != nil {
				return err
			}
			after = batch[len(batch)-1].Path
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
