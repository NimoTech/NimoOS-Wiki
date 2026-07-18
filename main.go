package main

import (
	"context"
	_ "embed"
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
	mgr := roots.NewManager(rRoots, rNodes, bus)
	rec := scanner.NewReconciler(rFiles, rEvents, ig)
	wch := scanner.NewWatcher(rEvents, rNodes, ig, guard, zapLog)
	mgr.SetWatch(wch)
	proc := processor.New(d, rFiles, rEvents, rNodes, rParse, bus, ig, locks, guard, rRoots, zapLog)
	proc.SyncIn = wch.SyncOut
	if config.Cfg.EventDebounceMs > 0 {
		proc.EventDebounceMs = config.Cfg.EventDebounceMs
	}
	wri := writer.NewWriter(rNodes, rFiles, rEvents, bus, locks, rSummaries,
		time.Duration(config.Cfg.WikiWriteDebounceSec)*time.Second, zapLog)

	// Boot reconcile: replay drift BEFORE accepting traffic / starting watchers.
	if err := bootReconcile(ctx, rRoots, rec); err != nil {
		zapLog.Warn("boot reconcile failed (non-fatal)", zap.Error(err))
	}

	// Boot user-notes sync: pull in any .wiki.md edits the user made while the
	// service was stopped. Must run BEFORE watchers are registered so the live
	// path doesn't race with the disk-vs-DB comparison.
	if err := bootSyncWikiMD(rRoots, proc); err != nil {
		zapLog.Warn("boot user-notes sync failed (non-fatal)", zap.Error(err))
	}

	// Register watchers for each enabled root
	for _, root := range listEnabled(rRoots) {
		if root.WatchMode == "auto" {
			if err := wch.Watch(root.ID, root.Path); err != nil {
				zapLog.Warn("watch failed", zap.String("path", root.Path), zap.Error(err))
			}
		}
	}

	// Background goroutines
	go wch.Run(ctx)
	go proc.Run(ctx, 1*time.Second)
	go wri.Run(ctx)
	go runReconcilerLoop(ctx, rRoots, rec)
	go runArchiveJob(ctx, rEvents, config.Cfg.RecentChangesRetentionDays)

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

// bootReconcile reconciles each enabled Root once at startup, before
// Ready is signaled. This catches changes that happened while the service
// was down (Spec §6.3).
func bootReconcile(ctx context.Context, r *repo.WikiRootsRepo, rec *scanner.Reconciler) error {
	all, err := r.List()
	if err != nil {
		return err
	}
	for _, root := range all {
		if !root.Enabled {
			continue
		}
		if err := rec.Reconcile(root.ID, root.Path); err != nil {
			zapLog.Warn("reconcile failed", zap.String("path", root.Path), zap.Error(err))
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

// runReconcilerLoop polls each enabled Root every 30 seconds and reconciles
// those whose LastScanAt + ScanIntervalS has elapsed.
func runReconcilerLoop(ctx context.Context, r *repo.WikiRootsRepo, rec *scanner.Reconciler) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := time.Now().UnixMilli()
			for _, root := range listEnabled(r) {
				if root.LastScanAt+int64(root.ScanIntervalS)*1000 > now {
					continue
				}
				if err := rec.Reconcile(root.ID, root.Path); err != nil {
					zapLog.Warn("reconcile failed", zap.String("path", root.Path), zap.Error(err))
					continue
				}
				_ = r.UpdateLastScanAt(root.ID, time.Now().UnixMilli())
			}
		}
	}
}

// runArchiveJob runs hourly: archive file_events > keepDays old, purge events > 2x keepDays old.
func runArchiveJob(ctx context.Context, ev *repo.FileEventsRepo, keepDays int) {
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
			archiveCutoff := time.Now().Add(-time.Duration(keepDays) * 24 * time.Hour).UnixMilli()
			purgeCutoff := time.Now().Add(-time.Duration(keepDays*2) * 24 * time.Hour).UnixMilli()
			_, _ = ev.ArchiveOlderThan(archiveCutoff)
			_, _ = ev.PurgeOlderThan(purgeCutoff)
		}
	}
}
