package main

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/NimoTech/NimoOS-Common/external"
	"github.com/NimoTech/NimoOS-Common/model"
	"github.com/NimoTech/NimoOS-Common/utils/file"
	"github.com/NimoTech/NimoOS-Common/utils/logger"
	"github.com/NimoTech/NimoOS-Wiki/common"
	"github.com/NimoTech/NimoOS-Wiki/pkg/config"
	"github.com/NimoTech/NimoOS-Wiki/pkg/db"
	"github.com/NimoTech/NimoOS-Wiki/pkg/ignore"
	"github.com/NimoTech/NimoOS-Wiki/pkg/nodelock"
	v1 "github.com/NimoTech/NimoOS-Wiki/route/v1"
	"github.com/NimoTech/NimoOS-Wiki/service/eventbus"
	"github.com/NimoTech/NimoOS-Wiki/service/processor"
	"github.com/NimoTech/NimoOS-Wiki/service/repo"
	"github.com/NimoTech/NimoOS-Wiki/service/roots"
	"github.com/NimoTech/NimoOS-Wiki/service/rootsync"
	"github.com/NimoTech/NimoOS-Wiki/service/scanner"
	"github.com/NimoTech/NimoOS-Wiki/service/writer"
	"github.com/coreos/go-systemd/daemon"
	"go.uber.org/zap"
)

var (
	commit = "private build"
	date   = "private build"

	//go:embed build/sysroot/etc/nimoos/wiki.conf.sample
	_confSample string

	// zapLog is a process-wide *zap.Logger for services that take one
	// explicitly. NimoOS-Common's logger package only exposes Info/Error
	// helpers, so we keep our own structured logger here for Warn and for
	// passing into services. Initialized in main().
	zapLog *zap.Logger
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	configFlag := flag.String("c", "", "config file path")
	versionFlag := flag.Bool("v", false, "version")
	flag.Parse()

	if *versionFlag {
		fmt.Printf("v%s\n", common.WikiVersion)
		os.Exit(0)
	}
	fmt.Println("git commit:", commit, "  build date:", date)

	if err := config.Init(*configFlag, _confSample); err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(config.Cfg.DataPath, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir data: %v\n", err)
		os.Exit(1)
	}
	logger.LogInit(config.Cfg.LogPath, "nimoos-wiki", "log")

	zapLog, _ = zap.NewProduction()
	defer func() { _ = zapLog.Sync() }()

	// DB
	d, err := db.Open(filepath.Join(config.Cfg.DataPath, "wiki.db"))
	if err != nil {
		logger.Error("open db", zap.Error(err))
		os.Exit(1)
	}
	defer d.Close()

	// Repos
	rRoots := repo.NewWikiRoots(d)
	rNodes := repo.NewWikiNodes(d)
	rFiles := repo.NewFileIndex(d)
	rEvents := repo.NewFileEvents(d)
	rParse := repo.NewParseStatus(d)
	rSummaries := repo.NewWikiSummaries(d)

	// Services
	bus := eventbus.New(config.Cfg.RuntimePath)
	ig := ignore.New(config.Cfg.ContainerDirs)
	locks := nodelock.New()
	guard := scanner.NewStormGuard(config.Cfg.EventFuseHigh, config.Cfg.EventFuseLow,
		config.Cfg.GlobalFuseHigh, config.Cfg.GlobalFuseLow)
	mgr := roots.NewManager(rRoots, rNodes, rFiles, rEvents, bus, ig)
	mgr.PrecheckDirLimit = config.Cfg.PrecheckDirLimit
	mgr.PrecheckTimeout = time.Duration(config.Cfg.PrecheckTimeoutMs) * time.Millisecond
	// 授权源解耦:核心是唯一的授权权威,root 生命周期变化(增/删/启停)需增量
	// 推给核心;discoveryFile 是核心启动时写入的服务发现文件,记录当前监听地址。
	rsClient := rootsync.New("/var/run/nimoos/nimoos.url")
	mgr.SetPusher(rsClient)
	mgr.SetLogger(zapLog)
	rec := scanner.NewReconciler(rFiles, rEvents, ig)
	rec.BatchSize = config.Cfg.ReconcileBatchSize
	rec.ThrottleEvery = config.Cfg.WalkThrottleEvery
	rec.ThrottleSleep = time.Duration(config.Cfg.WalkThrottleSleepMs) * time.Millisecond
	wch := scanner.NewWatcher(rEvents, rNodes, ig, guard, zapLog)
	mgr.SetWatch(wch)
	// Runtime watch-limit hits (new dirs created after startup) degrade the
	// root the same way a registration-time hit below does.
	wch.OnWatchLimit = func(rootID string) { mgr.DegradeToScanOnly(rootID, "watch_limit") }
	proc := processor.New(d, rFiles, rEvents, rNodes, rParse, bus, ig, locks, guard, rRoots, zapLog)
	proc.SyncIn = wch.SyncOut
	if config.Cfg.EventDebounceMs > 0 {
		proc.EventDebounceMs = config.Cfg.EventDebounceMs
	}
	wri := writer.NewWriter(rNodes, rFiles, rEvents, bus, locks, rSummaries,
		time.Duration(config.Cfg.WikiWriteDebounceSec)*time.Second, zapLog)

	// Boot user-notes sync: pull in any .wiki.md edits the user made while the
	// service was stopped. Must run BEFORE watchers are registered so the live
	// path doesn't race with the disk-vs-DB comparison.
	if err := bootSyncWikiMD(rRoots, proc); err != nil {
		zapLog.Warn("boot user-notes sync failed (non-fatal)", zap.Error(err))
	}

	// 授权源解耦 Task 5:启动时把当前全部 root 状态整体推给核心做一次全量对账,
	// 弥补运行期间任何一次增量推送(Create/SetEnabled/Delete)失败的窗口。核心
	// 不可达也只 Warn、不阻塞 readiness——单次调用内部已带 3s 超时兜底。
	if err := bootReconcileRootSync(ctx, rRoots, rsClient); err != nil {
		zapLog.Warn("boot root-grant reconcile failed (non-fatal)", zap.Error(err))
	}

	// Register watchers for each enabled root
	for _, root := range listEnabled(rRoots) {
		if root.WatchMode == "auto" {
			if err := wch.Watch(root.ID, root.Path); err != nil {
				switch {
				case errors.Is(err, scanner.ErrWatchLimit):
					mgr.DegradeToScanOnly(root.ID, "watch_limit")
				case errors.Is(err, scanner.ErrWatchRootFailed):
					mgr.DegradeToScanOnly(root.ID, "watch_error")
				}
				zapLog.Warn("watch failed", zap.String("path", root.Path), zap.Error(err))
			}
		}
	}

	// Background goroutines
	go wch.Run(ctx)
	go proc.Run(ctx, 1*time.Second)
	go wri.Run(ctx)
	go func() {
		// Boot reconcile runs in the reconciler goroutine so at most one
		// reconcile executor exists at a time (same invariant as before,
		// minus the startup blocking).
		if err := bootReconcile(ctx, rRoots, rec); err != nil {
			zapLog.Warn("boot reconcile failed (non-fatal)", zap.Error(err))
		}
		runReconcilerLoop(ctx, rRoots, rec, guard, rEvents)
	}()
	go runArchiveJob(ctx, rEvents, rRoots, config.Cfg.RecentChangesRetentionDays, config.Cfg.EventMaxRows)
	// 授权源解耦 Task 5 Critical 修复(方案 B):与上面的 FS 重扫 reconcileTick
	// 完全独立的专用重试循环,只消费 needs_authz_push / manager 内存脏标。
	go runAuthzPushRetryLoop(ctx, rRoots, rsClient, mgr)

	// Listener — random localhost port
	listener, err := net.Listen("tcp", net.JoinHostPort(common.Localhost, "0"))
	if err != nil {
		panic("failed to listen: " + err.Error())
	}
	urlFilePath := filepath.Join(config.Cfg.RuntimePath, common.URLFileName)
	if err := file.CreateFileAndWriteContent(urlFilePath, "http://"+listener.Addr().String()); err != nil {
		logger.Error("write url file", zap.Error(err))
	}

	// Gateway registration
	gw, err := external.NewManagementService(config.Cfg.RuntimePath)
	if err != nil {
		panic("connect to Gateway: " + err.Error())
	}
	for _, p := range []string{common.V1APIPath, common.V1DocPath} {
		if err := gw.CreateRoute(&model.Route{
			Path:   p,
			Target: "http://" + listener.Addr().String(),
		}); err != nil {
			panic("register route " + p + ": " + err.Error())
		}
	}

	handler := v1.InitRouter(v1.Deps{
		Roots:       mgr,
		WikiRoots:   rRoots,
		Nodes:       rNodes,
		Files:       rFiles,
		Events:      rEvents,
		Summaries:   rSummaries,
		RuntimePath: config.Cfg.RuntimePath,
	})

	// systemd Ready
	if _, err := daemon.SdNotify(false, daemon.SdNotifyReady); err != nil {
		zapLog.Warn("sdnotify", zap.Error(err))
	}
	logger.Info("NimoOS-Wiki listening", zap.String("addr", listener.Addr().String()))

	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}

	// Signal handling: SIGTERM/SIGINT → graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		logger.Info("shutdown signal; flushing dirty nodes")
		flushTimeout := time.Duration(config.Cfg.ShutdownFlushTimeoutSec) * time.Second
		if flushTimeout > 0 {
			wri.FlushAll(flushTimeout)
		}
		shutdownCtx, c := context.WithTimeout(context.Background(), 3*time.Second)
		defer c()
		_ = srv.Shutdown(shutdownCtx)
		cancel()
	}()

	if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
		logger.Error("serve", zap.Error(err))
	}
}

// rootDueAtBoot decides whether a root gets the startup reconcile pass.
// Fresh roots are skipped so a routine deploy restart doesn't re-walk every
// root (offline drift in that short window is caught when the root becomes
// overdue). needs_reconcile roots are left to reconcileTick's storm-aware
// drain — one per tick, skipping storming roots — which boot must not bypass.
func rootDueAtBoot(root repo.WikiRoot, nowMs int64) bool {
	if !root.Enabled || root.NeedsReconcile {
		return false
	}
	return root.LastScanAt == 0 || nowMs-root.LastScanAt >= int64(root.ScanIntervalS)*1000
}

// bootReconcile reconciles roots that actually need it at startup (never
// scanned / overdue — see rootDueAtBoot). Runs in the reconciler goroutine
// BEFORE the periodic loop, no longer blocking startup (spec §6.3's offline
// catch-up intent is preserved for long downtime: those roots are overdue).
func bootReconcile(ctx context.Context, r *repo.WikiRootsRepo, rec *scanner.Reconciler) error {
	all, err := r.List()
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	for _, root := range all {
		if !rootDueAtBoot(root, now) {
			continue
		}
		if err := rec.Reconcile(ctx, root.ID, root.Path); err != nil {
			zapLog.Warn("boot reconcile failed", zap.String("path", root.Path), zap.Error(err))
			continue
		}
		_ = r.UpdateLastScanAt(root.ID, time.Now().UnixMilli())
	}
	return nil
}

// bootSyncWikiMD reconciles on-disk `.wiki.md` against wiki_nodes at startup,
// so edits made while the service was down don't get clobbered by the first
// WikiWriter flush. Non-fatal: a failure on one root logs and continues.
func bootSyncWikiMD(r *repo.WikiRootsRepo, proc *processor.EventProcessor) error {
	all, err := r.List()
	if err != nil {
		return err
	}
	for _, root := range all {
		if !root.Enabled {
			continue
		}
		n, err := proc.BootSyncRoot(root.ID)
		if err != nil {
			zapLog.Warn("boot user-notes sync root failed",
				zap.String("root_id", root.ID),
				zap.String("path", root.Path), zap.Error(err))
			continue
		}
		if n > 0 {
			zapLog.Info("boot user-notes sync",
				zap.String("root_id", root.ID),
				zap.String("path", root.Path), zap.Int("synced", n))
		}
	}
	return nil
}

// bootReconcileRootSync 读取全部 root(含 disabled)组装为 rootsync.Grant 列表,
// 推给核心做一次全量对账(POST .../reconcile,核心以此为准同步 source="wiki"
// 的全部行)。是运行期间任何一次增量推送(pushUpsert/pushDelete)失败后的
// 最终一致性兜底,因此覆盖全部 root 而非仅 needs_reconcile 的子集。
func bootReconcileRootSync(ctx context.Context, r *repo.WikiRootsRepo, rs *rootsync.Client) error {
	all, err := r.List()
	if err != nil {
		return err
	}
	grants := make([]rootsync.Grant, 0, len(all))
	for _, root := range all {
		grants = append(grants, rootsync.Grant{RootID: root.ID, Path: root.Path, Enabled: root.Enabled})
	}
	return rs.Reconcile(ctx, grants)
}

// authzPushRetryInterval 是专用授权推送重试循环的 tick 间隔。60s 足够快地
// 收敛核心侧的授权漂移窗口,又不会在无待推信号时给 DB 增加明显负担(每 tick
// 只有一条 COUNT/EXISTS 查询)。
const authzPushRetryInterval = 60 * time.Second

// authzReconciler 是重试循环对 rootsync 的最小依赖(同 roots.pusher 的解耦
// 理由):测试用 fake 记录调用次数/参数,无需起 httptest 服务器。
// *rootsync.Client 天然满足此接口。
type authzReconciler interface {
	Reconcile(ctx context.Context, grants []rootsync.Grant) error
}

// authzDirtyChecker 是 roots.Manager 暴露给重试循环的最小读写面(TOCTOU 加固,
// consume-then-act 语义):ConsumeAuthzDirty 原子读取并清空内存脏标(pushDelete
// 失败时置位,该 root 行已被删、DB 里无处落盘),MarkAuthzDirty 用于本轮 tick
// 消费了信号却未能真正对账成功(List/Reconcile 失败)时把信号找补回去。
type authzDirtyChecker interface {
	ConsumeAuthzDirty() bool
	MarkAuthzDirty()
}

// runAuthzPushRetryLoop 是与 runReconcilerLoop(FS 重扫)完全独立的 goroutine:
// 每 authzPushRetryInterval 检查一次是否存在待重推的 root 授权信号(DB 里
// needs_authz_push 置位的行,或 manager 内存脏标——分别对应 pushUpsert /
// pushDelete 失败),存在则触发一次全量幂等 Reconcile 同时纠正两类漂移。
func runAuthzPushRetryLoop(ctx context.Context, r *repo.WikiRootsRepo, rs authzReconciler, mgr authzDirtyChecker) {
	t := time.NewTicker(authzPushRetryInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			authzPushRetryTick(ctx, r, rs, mgr, zapLog)
		}
	}
}

// authzPushRetryTick 是重试循环的一次 tick,按 consume-then-act 顺序执行
// (TOCTOU 加固,修复"快照 t2 之后、清位 t4 之前"窗口期内新信号被 blanket
// clear 误吞的竞态):
//
//  1. 先消费信号:原子 Swap 内存脏标 + 批量清除 DB 的 needs_authz_push
//     标记。二者皆无则零成本空转(仅一条 COUNT 查询)。
//  2. 再取快照:roots.List() 组装全量 Grant。
//  3. 后对账:调用一次全量幂等 Reconcile。
//  4. 失败重挂:List/Reconcile 失败 → 重新置位内存脏标,交给下个 tick 重试
//     (仅 Warn,不阻塞、不 panic);成功则什么都不用清——信号已在第 1 步消费。
//
// 这样为什么是对的:所有置信号的写路径——pushUpsert 的 SetNeedsAuthzPush、
// pushDelete 的 authzDirty.Store(true)——对应的 DB 状态变更(Create/
// SetEnabled 的行写入、Delete 的行删除)都发生在"推送失败→置信号"之前;
// 因此第 1 步消费到的任何信号,其 DB 状态必然已经提交,必然会被第 2 步的
// roots.List() 快照覆盖到,不会漏纠正。而窗口期(第 1 步之后)才新产生的
// 信号不会被本轮消费,自然原样保留到下个 tick,由下个 tick 用届时更新过的
// 快照重新对账。崩溃场景(消费了信号但 Reconcile 还没成功就挂了)由启动时
// bootReconcileRootSync 的全量对账兜底。
func authzPushRetryTick(ctx context.Context, r *repo.WikiRootsRepo, rs authzReconciler,
	mgr authzDirtyChecker, log *zap.Logger) {
	// 第 1 步:消费信号(顺序不影响正确性,内存脏标与 DB 标记是两个独立信号源)。
	dirty := mgr.ConsumeAuthzDirty()
	hadFlags, err := r.HasNeedsAuthzPush()
	if err != nil {
		log.Warn("authz push retry: query pending failed", zap.Error(err))
		// 查询失败:DB 侧信号本就没被动过,但内存脏标已经被上面 Swap 掉了,
		// 若直接 return 会白白丢失该信号,所以找补回去,下个 tick 重新判断。
		if dirty {
			mgr.MarkAuthzDirty()
		}
		return
	}
	if hadFlags {
		if err := r.ClearAllNeedsAuthzPush(); err != nil {
			log.Warn("authz push retry: clear needs_authz_push failed", zap.Error(err))
		}
	}
	if !dirty && !hadFlags {
		return
	}

	// 第 2 步:取快照。
	all, err := r.List()
	if err != nil {
		log.Warn("authz push retry: list roots failed", zap.Error(err))
		mgr.MarkAuthzDirty()
		return
	}
	grants := make([]rootsync.Grant, 0, len(all))
	for _, root := range all {
		grants = append(grants, rootsync.Grant{RootID: root.ID, Path: root.Path, Enabled: root.Enabled})
	}

	// 第 3 步:后对账。
	if err := rs.Reconcile(ctx, grants); err != nil {
		// 第 4 步:失败重挂——信号已在第 1 步消费,这里只需重新置位内存脏标,
		// 下个 tick 会因 dirty=true 再次触发全量 Reconcile。
		log.Warn("authz push retry: reconcile failed; will retry next tick", zap.Error(err))
		mgr.MarkAuthzDirty()
		return
	}
	// 成功:两类信号都已在第 1 步消费完毕,这里无需再做任何清理。
}

func listEnabled(r *repo.WikiRootsRepo) []repo.WikiRoot {
	all, _ := r.List()
	var out []repo.WikiRoot
	for _, w := range all {
		if w.Enabled {
			out = append(out, w)
		}
	}
	return out
}

// runReconcilerLoop polls every 30 seconds and calls reconcileTick: at most
// one needs_reconcile root is drained first (largest backlog, spec §4.2
// staggering), then regular interval-due reconciles run.
func runReconcilerLoop(ctx context.Context, r *repo.WikiRootsRepo, rec *scanner.Reconciler,
	guard *scanner.StormGuard, ev *repo.FileEventsRepo) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			reconcileTick(ctx, r, rec, guard, ev, zapLog)
		}
	}
}

// reconcileTick is one 30s step of the reconciler loop: first drain AT MOST
// ONE needs_reconcile root (largest backlog first, skipping storming roots —
// spec §4.2 staggering), then run regular interval-due reconciles (also
// skipping storming roots). Regular reconciles may be delayed while the
// needs_reconcile queue drains; that is the intended degraded pace.
func reconcileTick(ctx context.Context, r *repo.WikiRootsRepo, rec *scanner.Reconciler,
	guard *scanner.StormGuard, ev *repo.FileEventsRepo, log *zap.Logger) {
	now := time.Now().UnixMilli()
	enabled := listEnabled(r)
	backlogs, _ := ev.CountUnprocessedByRoot()

	// 1) one needs_reconcile root per tick
	var pick *repo.WikiRoot
	for i := range enabled {
		root := &enabled[i]
		if !root.NeedsReconcile || (guard != nil && guard.IsStorming(root.ID)) {
			continue
		}
		if pick == nil || backlogs[root.ID] > backlogs[pick.ID] {
			pick = root
		}
	}
	if pick != nil {
		if err := rec.Reconcile(ctx, pick.ID, pick.Path); err != nil {
			log.Warn("needs-reconcile failed", zap.String("path", pick.Path), zap.Error(err))
		} else {
			_ = r.SetNeedsReconcile(pick.ID, false)
			_ = r.UpdateLastScanAt(pick.ID, time.Now().UnixMilli())
		}
	}

	// 2) regular interval-due reconciles
	for _, root := range enabled {
		if pick != nil && root.ID == pick.ID {
			continue
		}
		if guard != nil && guard.IsStorming(root.ID) {
			continue
		}
		if root.LastScanAt+int64(root.ScanIntervalS)*1000 > now {
			continue
		}
		if err := rec.Reconcile(ctx, root.ID, root.Path); err != nil {
			log.Warn("reconcile failed", zap.String("path", root.Path), zap.Error(err))
			continue // last_scan NOT updated → retried next cycle (strict-Lstat abort path)
		}
		_ = r.UpdateLastScanAt(root.ID, time.Now().UnixMilli())
	}
}

// runArchiveJob runs hourly: archive file_events > keepDays old, purge events
// > 2x keepDays old, plus the hard row cap (spec §4.2) via archiveSweep.
func runArchiveJob(ctx context.Context, ev *repo.FileEventsRepo, roots *repo.WikiRootsRepo,
	keepDays int, maxRows int64) {
	if keepDays <= 0 {
		keepDays = 90
	}
	t := time.NewTicker(1 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			archiveSweep(ev, roots, keepDays, maxRows, zapLog)
		}
	}
}

// archiveSweep is one pass of the hourly retention job: age-based archive /
// purge (existing behavior) plus the hard row cap (spec §4.2). Roots whose
// rows were cap-purged are marked needs_reconcile — Wiki cannot know the
// Parser cursor, so treat every capped purge as destroying unconsumed rows.
func archiveSweep(ev *repo.FileEventsRepo, roots *repo.WikiRootsRepo,
	keepDays int, maxRows int64, log *zap.Logger) {
	archiveCutoff := time.Now().Add(-time.Duration(keepDays) * 24 * time.Hour).UnixMilli()
	purgeCutoff := time.Now().Add(-time.Duration(keepDays*2) * 24 * time.Hour).UnixMilli()
	_, _ = ev.ArchiveOlderThan(archiveCutoff)
	_, _ = ev.PurgeOlderThan(purgeCutoff)
	if maxRows <= 0 {
		return
	}
	purged, affected, err := ev.PurgeOldestOverCap(maxRows)
	if err != nil {
		log.Warn("row-cap purge", zap.Error(err))
		return
	}
	if purged > 0 {
		log.Warn("file_events over row cap: purged oldest",
			zap.Int64("purged", purged), zap.Strings("roots", affected))
		for _, id := range affected {
			_ = roots.SetNeedsReconcile(id, true)
		}
	}
}
