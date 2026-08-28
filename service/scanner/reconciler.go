package scanner

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/NimoTech/NimoOS-Wiki/pkg/ignore"
	"github.com/NimoTech/NimoOS-Wiki/pkg/pathutil"
	"github.com/NimoTech/NimoOS-Wiki/service/repo"
)

// Reconciler diffs a Root's filesystem tree against file_index in two
// STREAMING passes with O(batch) memory (spec §4.6) — no longer loads the
// whole root into a map, so multi-million-file roots are safe:
//
//	pass A: page through existing file_index rows (keyset on path, BatchSize
//	        at a time), Lstat each one. Gone → delete; changed → modify.
//	pass B: throttled WalkDir over rootPath; entries missing from file_index
//	        → create.
//
// Both passes are throttled (sleep every ThrottleEvery entries) and
// ctx-cancellable between/within sleeps.
//
// Containers are recorded as a single is_opaque=true row in file_index and
// not recursed into.
//
// SILENT PURGE: when a previously-indexed row falls under a directory that is
// NOW recognized as a container (e.g., immich just got added to the container
// baseline), the row is removed from file_index WITHOUT emitting a delete
// event. This prevents a one-shot baseline change from flooding Recent
// Changes with delete events the user never asked for. This is detected in
// pass A via hasContainerAncestor, since containers are always fully
// materialized as file_index rows in the ancestor chain.
type Reconciler struct {
	files  *repo.FileIndexRepo
	events *repo.FileEventsRepo
	ig     *ignore.Matcher

	BatchSize     int           // keyset page size, default 5000
	ThrottleEvery int           // sleep every N processed entries, default 1000
	ThrottleSleep time.Duration // default 50ms
}

func NewReconciler(files *repo.FileIndexRepo, events *repo.FileEventsRepo, ig *ignore.Matcher) *Reconciler {
	return &Reconciler{
		files: files, events: events, ig: ig,
		BatchSize: 5000, ThrottleEvery: 1000, ThrottleSleep: 50 * time.Millisecond,
	}
}

// throttle sleeps every ThrottleEvery-th call, cancellably (spec §4.6).
type throttle struct {
	r *Reconciler
	n int
}

func (t *throttle) tick(ctx context.Context) error {
	t.n++
	if t.r.ThrottleEvery <= 0 || t.n%t.r.ThrottleEvery != 0 {
		return ctx.Err()
	}
	select {
	case <-time.After(t.r.ThrottleSleep):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Reconcile diffs rootPath against file_index in two streaming passes with
// O(batch) memory (spec §4.6):
//
//	pass A: page through file_index rows (keyset on path); Lstat each —
//	        gone → delete (STRICTLY fs.ErrNotExist; any other error aborts
//	        the whole round so a flapping mount can never mass-delete),
//	        changed → modify.
//	pass B: throttled WalkDir; entries missing from file_index → create.
//
// On error, callers MUST NOT treat the round as successful (e.g. must not
// advance last_scan) — an aborted round means the root's file_index state
// wasn't fully verified against disk, so it needs to be retried, not marked
// caught-up.
func (r *Reconciler) Reconcile(ctx context.Context, rootID, rootPath string) error {
	now := time.Now().UnixMilli()
	th := &throttle{r: r}

	// ---- pass A: existing rows ----
	after := ""
	for {
		batch, err := r.files.ListByRootAfter(rootID, after, r.BatchSize)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			break
		}
		for i := range batch {
			old := &batch[i]
			if err := th.tick(ctx); err != nil {
				return err
			}
			// Silent purge applies regardless of whether the row still
			// physically exists: once an ancestor is (now) a container, we
			// no longer recurse into it, so any row nested under it is a
			// stale artifact — either from before the ancestor became a
			// container, or from disk removal. Either way, no delete event.
			if r.hasContainerAncestor(rootPath, old.Path) {
				_ = r.files.DeleteByPath(rootID, old.Path)
				continue
			}
			info, statErr := os.Lstat(old.Path)
			switch {
			case statErr == nil:
				mtime := info.ModTime().UnixMilli()
				size := info.Size()
				isOpaque := info.IsDir() && r.ig.IsContainerDir(filepath.Base(old.Path))
				if old.Mtime != mtime || old.Size != size || old.IsOpaque != isOpaque {
					_ = r.events.Insert(repo.FileEvent{
						ID: repo.NewID(), RootID: rootID, Path: old.Path, Op: "modify",
						IsDir: info.IsDir(), DetectedAt: now,
					})
					upd := *old
					upd.Mtime, upd.Size, upd.IsOpaque, upd.IsDir = mtime, size, isOpaque, info.IsDir()
					_ = r.files.Upsert(upd)
				}
			case errors.Is(statErr, fs.ErrNotExist):
				_ = r.events.Insert(repo.FileEvent{
					ID: repo.NewID(), RootID: rootID, Path: old.Path, Op: "delete",
					IsDir: old.IsDir, DetectedAt: now,
				})
				_ = r.files.DeleteByPath(rootID, old.Path)
			default:
				// EIO / EACCES / network mount flap: abort this round.
				// last_scan is NOT updated by callers on error, so the root
				// retries next cycle (spec §4.6 anti-mass-delete rule).
				return fmt.Errorf("reconcile %s: stat %s: %w", rootID, old.Path, statErr)
			}
		}
		after = batch[len(batch)-1].Path
	}

	// ---- pass B: new entries on disk ----
	return filepath.WalkDir(rootPath, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entry: skip (creation pass only)
		}
		if p == rootPath {
			return nil // don't index the root itself
		}
		if terr := th.tick(ctx); terr != nil {
			return terr
		}
		base := filepath.Base(p)

		// Skip system noise and wiki output files
		if !d.IsDir() && (r.ig.IsSystemIgnoredBasename(base) || r.ig.IsWikiFile(base) || r.ig.IsWikiTmpFile(base)) {
			return nil
		}

		isOpaque := d.IsDir() && r.ig.IsContainerDir(base)
		existing, gerr := r.files.Get(rootID, p)
		if gerr != nil {
			return gerr
		}
		if existing == nil {
			info, statErr := d.Info()
			var mtime, size int64
			if statErr == nil {
				mtime = info.ModTime().UnixMilli()
				size = info.Size()
			}
			_ = r.events.Insert(repo.FileEvent{
				ID: repo.NewID(), RootID: rootID, Path: p, Op: "create",
				IsDir: d.IsDir(), DetectedAt: now,
			})
			_ = r.files.Upsert(repo.FileIndex{
				ID: repo.NewID(), RootID: rootID, Path: p,
				Parent: pathutil.Parent(p), IsDir: d.IsDir(), IsOpaque: isOpaque,
				Mtime: mtime, Size: size, Status: "present",
				Ext: strings.ToLower(strings.TrimPrefix(filepath.Ext(p), ".")),
			})
		}
		if isOpaque {
			return filepath.SkipDir
		}
		return nil
	})
}

// hasContainerAncestor reports whether any path segment strictly between
// rootPath and p is a container dir per the matcher — the streaming
// equivalent of the old walk-collected opaqueDirs silent-purge check.
func (r *Reconciler) hasContainerAncestor(rootPath, p string) bool {
	return hasContainerAncestor(r.ig, rootPath, p)
}

// Run periodically reconciles a single Root until ctx is cancelled.
// Use from a goroutine.
func (r *Reconciler) Run(ctx context.Context, rootID, rootPath string, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = r.Reconcile(ctx, rootID, rootPath)
		}
	}
}
