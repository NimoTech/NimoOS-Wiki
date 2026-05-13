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

	// Remaining entries in seen = deleted on disk
	for path, old := range seen {
		_ = r.events.Insert(repo.FileEvent{
			ID: repo.NewID(), RootID: rootID, Path: path, Op: "delete",
			IsDir: old.IsDir, DetectedAt: now,
		})
		_ = r.files.DeleteByPath(rootID, path)
	}
	return nil
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
