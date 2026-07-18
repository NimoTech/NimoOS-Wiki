// Package roots manages Wiki Root lifecycle: registration with write-test,
// FS-type detection for watch_mode auto-downgrade, and listing.
package roots

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/NimoTech/NimoOS-Wiki/common"
	"github.com/NimoTech/NimoOS-Wiki/pkg/ignore"
	"github.com/NimoTech/NimoOS-Wiki/service/eventbus"
	"github.com/NimoTech/NimoOS-Wiki/service/repo"
	"github.com/NimoTech/NimoOS-Wiki/service/scanner"
)

type Manager struct {
	roots *repo.WikiRootsRepo
	nodes *repo.WikiNodesRepo
	bus   eventbus.Bus
	watch Watch
	ig    *ignore.Matcher

	// PrecheckDirLimit / PrecheckTimeout bound the size precheck run in
	// Create (spec §4.4). Zero-value PrecheckDirLimit skips the precheck
	// entirely (keeps pre-Task-7 tests green); main injects both from config.
	PrecheckDirLimit int
	PrecheckTimeout  time.Duration
}

// NewManager wires the Manager. ig may be nil (tests) — countDirsQuick then
// falls back to counting every directory raw, including container dirs.
func NewManager(roots *repo.WikiRootsRepo, nodes *repo.WikiNodesRepo, bus eventbus.Bus, ig *ignore.Matcher) *Manager {
	if bus == nil {
		bus = eventbus.Noop{}
	}
	return &Manager{roots: roots, nodes: nodes, bus: bus, ig: ig}
}

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
	m.bus.Publish(common.EventWatchDegraded, map[string]any{
		"root_id": rootID, "reason": reason,
		"hint": "raise fs.inotify.max_user_watches (recommended 524288)",
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
		if err := m.watch.Watch(id, args.Path); err != nil && errors.Is(err, scanner.ErrWatchLimit) {
			m.DegradeToScanOnly(id, "watch_limit")
		}
	}

	m.bus.Publish(common.EventRootEnabled, map[string]any{
		"root_id": id,
		"path":    args.Path,
		"level":   args.Level,
	})

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
			_ = m.watch.Watch(root.ID, root.Path)
		}
		m.bus.Publish(common.EventRootEnabled, payload)
	} else {
		if m.watch != nil {
			m.watch.Unwatch(root.ID)
		}
		m.bus.Publish(common.EventRootDisabled, payload)
	}
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
// Container dirs (spec §4.4: "跳过容器目录", e.g. /DATA/.system_data) are
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
