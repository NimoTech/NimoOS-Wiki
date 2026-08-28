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

// DefaultBacklogCountLimit bounds CountUnprocessedByRoot when no StormGuard is
// configured (tests / fuse disabled). Large enough to rank roots by backlog,
// small enough to stay O(limit) on a bloated table.
const DefaultBacklogCountLimit = 200000

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
	// Authz-source decoupling: core is the sole authorization authority, so any
	// root lifecycle change (create/delete/enable/disable) must be pushed to
	// core incrementally; discoveryFile is the service-discovery file core
	// writes at startup, recording its current listen address.
	rsClient := rootsync.New(filepath.Join(config.Cfg.RuntimePath, external.NimoOSURLFilename))
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
	// A flush ENOENT whose node dir is gone means the root path vanished —
	// disable the root instead of retrying forever.
	wri.SetOnRootGone(func(rootID string) { mgr.DisableForMissingPath(rootID) })

	// Boot user-notes sync: pull in any .wiki.md edits the user made while the
	// service was stopped. Must run BEFORE watchers are registered so the live
	// path doesn't race with the disk-vs-DB comparison.
	if err := bootSyncWikiMD(rRoots, proc); err != nil {
		zapLog.Warn("boot user-notes sync failed (non-fatal)", zap.Error(err))
	}

	// Authz-source decoupling Task 5: at startup, push the full current root
	// state to core for a one-shot reconcile, covering any window where an
	// incremental push (Create/SetEnabled/Delete) failed during runtime. Core
	// being unreachable only Warns and never blocks readiness — the single
	// call already has a 3s timeout fallback internally.
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
	// Authz-source decoupling Task 5 critical fix (option B): a dedicated retry
	// loop, fully independent from the FS rescan reconcileTick above, that only
	// consumes needs_authz_push / the manager's in-memory dirty flag.
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

// bootReconcileRootSync reads every root (including disabled ones), builds a
// rootsync.Grant list, and pushes it to core for a one-shot reconcile (POST
// .../reconcile; core treats this as authoritative and syncs all rows with
// source="wiki"). This is the eventual-consistency fallback for any
// incremental push (pushUpsert/pushDelete) that failed during runtime, so it
// covers every root rather than just the needs_reconcile subset.
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

// authzPushRetryInterval is the tick interval for the dedicated authz push
// retry loop. 60s converges core-side authz drift quickly enough without
// adding noticeable DB load when there's nothing pending to push (each tick
// costs a single COUNT/EXISTS query).
const authzPushRetryInterval = 60 * time.Second

// authzReconciler is the retry loop's minimal dependency on rootsync (same
// decoupling rationale as roots.pusher): tests use a fake that records call
// count/args without spinning up an httptest server. *rootsync.Client
// naturally satisfies this interface.
type authzReconciler interface {
	Reconcile(ctx context.Context, grants []rootsync.Grant) error
	// EnabledRoots returns core's currently granted root_ids, used by the
	// quiet-tick drift probe (see authzDriftDetected).
	EnabledRoots(ctx context.Context) ([]string, error)
}

// authzDirtyChecker is the minimal read/write surface roots.Manager exposes
// to the retry loop (TOCTOU hardening, consume-then-act semantics):
// ConsumeAuthzDirty atomically reads and clears the in-memory dirty flag (set
// when pushDelete fails, since that root row is already gone with nowhere to
// persist in the DB); MarkAuthzDirty re-raises the signal when the current
// tick consumed it but the reconcile itself didn't actually succeed
// (List/Reconcile failed).
type authzDirtyChecker interface {
	ConsumeAuthzDirty() bool
	MarkAuthzDirty()
}

// runAuthzPushRetryLoop is a goroutine fully independent from
// runReconcilerLoop (the FS rescan): every authzPushRetryInterval it checks
// whether there's a pending root-authz-repush signal (a row in the DB with
// needs_authz_push set, or the manager's in-memory dirty flag — corresponding
// to pushUpsert / pushDelete failures respectively); if so, it triggers one
// full idempotent Reconcile that corrects both kinds of drift.
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

// authzDriftDetected reports whether core's granted root_ids disagree with
// wiki's roots: an enabled wiki root core no longer grants, or a wiki root
// that is disabled here yet still granted there. Root ids core holds that
// wiki has never heard of (the virtual "photos" root, other sources) are
// not drift — only rows wiki owns are judged.
func authzDriftDetected(wikiRoots []repo.WikiRoot, coreRoots []string) bool {
	granted := make(map[string]struct{}, len(coreRoots))
	for _, id := range coreRoots {
		granted[id] = struct{}{}
	}
	for _, root := range wikiRoots {
		_, ok := granted[root.ID]
		if root.Enabled != ok {
			return true
		}
	}
	return false
}

// authzPushRetryTick is one tick of the retry loop, executed in
// consume-then-act order (TOCTOU hardening, fixing the race where a new
// signal raised in the window "after the t2 snapshot, before the t4 clear"
// would otherwise get swallowed by a blanket clear):
//
//  1. Consume signals first: atomically swap the in-memory dirty flag and
//     bulk-clear the DB's needs_authz_push markers. If neither is set, this
//     is a zero-cost no-op (just one COUNT query).
//  2. Then take a snapshot: roots.List() assembles the full Grant set.
//  3. Then reconcile: call one full idempotent Reconcile.
//  4. On failure, re-raise: if List/Reconcile fails, re-set the in-memory
//     dirty flag for the next tick to retry (Warn only, never blocks or
//     panics); on success, nothing needs clearing — the signal was already
//     consumed in step 1.
//
// Why this is correct: every write path that raises a signal — pushUpsert's
// SetNeedsAuthzPush, pushDelete's authzDirty.Store(true) — has its
// corresponding DB state change (Create/SetEnabled writing the row,
// Delete removing it) happen before "push failed → raise signal". So any
// signal consumed in step 1 is guaranteed to have its DB state already
// committed, and is guaranteed to be covered by step 2's roots.List()
// snapshot — nothing gets missed. Signals raised in the window after step 1
// simply aren't consumed this round and carry over untouched to the next
// tick, which reconciles against its own later, updated snapshot. The crash
// scenario (signal consumed but Reconcile never completed before the process
// died) is covered by bootReconcileRootSync's full reconcile at startup.
func authzPushRetryTick(ctx context.Context, r *repo.WikiRootsRepo, rs authzReconciler,
	mgr authzDirtyChecker, log *zap.Logger) {
	// Step 1: consume signals (order doesn't affect correctness — the
	// in-memory dirty flag and the DB marker are two independent signal
	// sources).
	dirty := mgr.ConsumeAuthzDirty()
	hadFlags, err := r.HasNeedsAuthzPush()
	if err != nil {
		log.Warn("authz push retry: query pending failed", zap.Error(err))
		// Query failed: the DB-side signal was never touched, but the
		// in-memory dirty flag was already swapped out above — returning
		// directly here would lose that signal for nothing, so re-raise it
		// and let the next tick re-decide.
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

	// Step 2: take a snapshot.
	all, err := r.List()
	if err != nil {
		log.Warn("authz push retry: list roots failed", zap.Error(err))
		if dirty || hadFlags {
			mgr.MarkAuthzDirty()
		}
		return
	}

	// Step 2b: quiet tick — no signal of our own. Our markers only ever
	// record OUR failed pushes; they say nothing about core being rewritten
	// behind our back (an isolated test wiki's boot reconcile did exactly
	// that on 2026-08-24 and silently scoped every search down to photos).
	// So probe core's granted list and treat disagreement as a signal. A
	// failed probe is not drift: just try again next tick, without raising
	// the dirty flag (that would turn every core hiccup into a Reconcile).
	if !dirty && !hadFlags {
		coreRoots, err := rs.EnabledRoots(ctx)
		if err != nil {
			log.Debug("authz drift probe: core unreachable; will retry next tick", zap.Error(err))
			return
		}
		if !authzDriftDetected(all, coreRoots) {
			return
		}
		log.Warn("authz drift detected: core root grants disagree with wiki roots; re-reconciling",
			zap.Int("wiki_roots", len(all)), zap.Strings("core_roots", coreRoots))
	}

	grants := make([]rootsync.Grant, 0, len(all))
	for _, root := range all {
		grants = append(grants, rootsync.Grant{RootID: root.ID, Path: root.Path, Enabled: root.Enabled})
	}

	// Step 3: reconcile.
	if err := rs.Reconcile(ctx, grants); err != nil {
		// Step 4: re-raise on failure — the signal was already consumed in
		// step 1, so just re-set the in-memory dirty flag; the next tick will
		// trigger a full Reconcile again because dirty=true.
		log.Warn("authz push retry: reconcile failed; will retry next tick", zap.Error(err))
		mgr.MarkAuthzDirty()
		return
	}
	// Success: both signal kinds were already consumed in step 1, nothing
	// left to clean up here.
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

func rootIDs(roots []repo.WikiRoot) []string {
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		out = append(out, r.ID)
	}
	return out
}

func backlogLimit(guard *scanner.StormGuard) int {
	if guard == nil {
		return DefaultBacklogCountLimit
	}
	return guard.CountLimit()
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
	backlogs, _ := ev.CountUnprocessedByRoot(rootIDs(enabled), backlogLimit(guard))

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
	purged, err := ev.PurgeOldestOverCap(maxRows)
	if err != nil {
		log.Warn("row-cap purge", zap.Error(err))
		return
	}
	if purged > 0 {
		log.Warn("file_events over row cap: purged oldest", zap.Int64("purged", purged))
		markAllEnabledNeedsReconcile(roots)
	}
}

// markAllEnabledNeedsReconcile is the conservative reaction to any cap purge
// (spec §3.2): Wiki cannot know which roots' unconsumed rows were destroyed,
// and computing the affected set is itself a whole-table DISTINCT, so mark
// every enabled root and let the 30s reconcile loop drain them one at a time.
func markAllEnabledNeedsReconcile(roots *repo.WikiRootsRepo) {
	for _, r := range listEnabled(roots) {
		_ = roots.SetNeedsReconcile(r.ID, true)
	}
}
