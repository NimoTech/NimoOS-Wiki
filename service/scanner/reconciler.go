package scanner

import (
	"context"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"github.com/NimoTech/NimoOS-Wiki/pkg/ignore"
	"github.com/NimoTech/NimoOS-Wiki/pkg/pathutil"
	"github.com/NimoTech/NimoOS-Wiki/service/repo"
)

// Reconciler walks a Root's filesystem tree, diffs against file_index, and
// inserts file_events for the differences. Memory model: load all rows for the
// Root into a map; remainder after walking = deletes (spec §6.2).
type Reconciler struct {
	files  *repo.FileIndexRepo
	events *repo.FileEventsRepo
	ig     *ignore.Matcher
}

func NewReconciler(files *repo.FileIndexRepo, events *repo.FileEventsRepo, ig *ignore.Matcher) *Reconciler {
	return &Reconciler{files: files, events: events, ig: ig}
}

// Reconcile diffs rootPath against file_index for rootID.
// Inserts file_events for: create/modify/delete; updates file_index accordingly.
// Containers are recorded as a single is_opaque=true row in file_index and not recursed into.
//
// SILENT PURGE: when a previously-indexed row falls under a directory that is
// NOW recognized as opaque (e.g., immich just got added to the container
// baseline), the row is removed from file_index WITHOUT emitting a delete
// event. This prevents a one-shot baseline change from flooding Recent
// Changes with delete events the user never asked for.
func (r *Reconciler) Reconcile(rootID, rootPath string) error {
	existing, err := r.files.ListAllByRoot(rootID)
	if err != nil {
		return err
	}
	seen := make(map[string]*repo.FileIndex, len(existing))
	for i := range existing {
		seen[existing[i].Path] = &existing[i]
	}

	now := time.Now().UnixMilli()
	opaqueDirs := make([]string, 0, 8) // populated during walk

	walkFn := func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if p == rootPath {
			return nil // don't index the root itself
		}
		base := filepath.Base(p)

		// Skip system noise and wiki output files
		if !d.IsDir() && (r.ig.IsSystemIgnoredBasename(base) || r.ig.IsWikiFile(base) || r.ig.IsWikiTmpFile(base)) {
			return nil
		}

		isOpaque := d.IsDir() && r.ig.IsContainerDir(base)
		if isOpaque {
			opaqueDirs = append(opaqueDirs, p)
		}

		info, statErr := d.Info()
		var mtime, size int64
		var inode int64
		if statErr == nil {
			mtime = info.ModTime().UnixMilli()
			size = info.Size()
			// inode left at 0; portable detection via syscall is unnecessary for diffing
		}

		curr := repo.FileIndex{
			ID:       repo.NewID(),
			RootID:   rootID,
			Path:     p,
			Parent:   pathutil.Parent(p),
			IsDir:    d.IsDir(),
			IsOpaque: isOpaque,
			Mtime:    mtime,
			Size:     size,
			Inode:    inode,
			Status:   "present",
			Ext:      strings.ToLower(strings.TrimPrefix(filepath.Ext(p), ".")),
		}

		old, exists := seen[p]
		if exists {
			delete(seen, p)
			// Preserve the existing ID rather than minting a new one
			curr.ID = old.ID
			if old.Mtime != mtime || old.Size != size || old.IsOpaque != isOpaque {
				_ = r.events.Insert(repo.FileEvent{
					ID: repo.NewID(), RootID: rootID, Path: p, Op: "modify",
					IsDir: d.IsDir(), DetectedAt: now,
				})
				_ = r.files.Upsert(curr)
			}
		} else {
			_ = r.events.Insert(repo.FileEvent{
				ID: repo.NewID(), RootID: rootID, Path: p, Op: "create",
				IsDir: d.IsDir(), DetectedAt: now,
			})
			_ = r.files.Upsert(curr)
		}

		if isOpaque {
			return filepath.SkipDir
		}
		return nil
	}

	if err := filepath.WalkDir(rootPath, walkFn); err != nil {
		return err
	}

	// Remaining entries in seen = no longer present in walk.
	// Two cases:
	//   1. Path is under a now-opaque ancestor → silent purge (no event).
	//   2. Otherwise → genuine delete, emit a delete event.
	//
	// NOTE on edge case: if the user actually deletes a previously-opaque dir
	// from disk (e.g., `rm -rf /root/immich`), WalkDir never visits it, so it
	// is NOT in opaqueDirs. The dir itself + everything under it falls into
	// case 2 — a delete event fires for /root/immich AND for every descendant
	// that hadn't already been rolled up. For a 1000-file install that's
	// 1000 delete events, which is the same flooding shape silent purge was
	// designed to avoid — accepted as a trade-off because real deletions
	// SHOULD be visible to downstream consumers (UI cache invalidation, etc),
	// even if noisy. The silent purge intentionally covers only the "I became
	// opaque" migration artifact, not user-initiated deletions.
	for path, old := range seen {
		if hasOpaqueAncestor(path, opaqueDirs) {
			_ = r.files.DeleteByPath(rootID, path)
			continue
		}
		_ = r.events.Insert(repo.FileEvent{
			ID: repo.NewID(), RootID: rootID, Path: path, Op: "delete",
			IsDir: old.IsDir, DetectedAt: now,
		})
		_ = r.files.DeleteByPath(rootID, path)
	}
	return nil
}

// hasOpaqueAncestor reports whether path lives strictly under any dir in
// opaqueDirs (i.e., path starts with `<dir>/`). The opaque dir itself doesn't
// count — it has its own opaque row that we want to keep.
func hasOpaqueAncestor(path string, opaqueDirs []string) bool {
	for _, dir := range opaqueDirs {
		if strings.HasPrefix(path, dir+"/") {
			return true
		}
	}
	return false
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
			_ = r.Reconcile(rootID, rootPath)
		}
	}
}
