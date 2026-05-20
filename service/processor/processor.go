package processor

import (
	"context"
	"database/sql"
	"path/filepath"
	"sort"
	"time"

	"github.com/NimoTech/NimoOS-Wiki/common"
	"github.com/NimoTech/NimoOS-Wiki/pkg/ignore"
	"github.com/NimoTech/NimoOS-Wiki/pkg/nodelock"
	"github.com/NimoTech/NimoOS-Wiki/pkg/pathutil"
	"github.com/NimoTech/NimoOS-Wiki/service/eventbus"
	"github.com/NimoTech/NimoOS-Wiki/service/repo"
	"github.com/NimoTech/NimoOS-Wiki/service/scanner"
	"go.uber.org/zap"
)

// EventProcessor pulls events from file_events, debounces, pairs MOVED_FROM /
// MOVED_TO into rename ops, applies the diff to file_index, and marks
// containing wiki_nodes dirty so WikiWriter regenerates.
//
// SyncIn drains user-notes reverse-sync tasks from the Watcher.
type EventProcessor struct {
	db     *sql.DB
	files  *repo.FileIndexRepo
	events *repo.FileEventsRepo
	nodes  *repo.WikiNodesRepo
	parse  *repo.ParseStatusRepo
	bus    eventbus.Bus
	ig     *ignore.Matcher
	locks  *nodelock.Locks
	log    *zap.Logger

	SyncIn <-chan scanner.UserNotesSyncTask

	EventDebounceMs int // default 200
}

// New constructs an EventProcessor.
//
// The `ig` parameter may be nil (in which case create events never flag
// opaque); pass a real Matcher to propagate container-dir opacity into
// file_index during Watcher-driven creates.
//
// The `locks` parameter is the shared per-path mutex set used to serialize
// with WikiWriter.FlushOne on the same wiki node. Production callers MUST
// pass the same *nodelock.Locks instance both services share (wired in
// main.go). Passing nil falls back to a fresh local set — safe for tests,
// broken in production.
func New(d *sql.DB, files *repo.FileIndexRepo, events *repo.FileEventsRepo,
	nodes *repo.WikiNodesRepo, parse *repo.ParseStatusRepo,
	bus eventbus.Bus, ig *ignore.Matcher, locks *nodelock.Locks,
	log *zap.Logger) *EventProcessor {
	if log == nil {
		log = zap.NewNop()
	}
	if bus == nil {
		bus = eventbus.Noop{}
	}
	if locks == nil {
		locks = nodelock.New()
	}
	return &EventProcessor{
		db: d, files: files, events: events, nodes: nodes, parse: parse,
		bus: bus, ig: ig, locks: locks, log: log, EventDebounceMs: 200,
	}
}

// ProcessBatch drains a batch of unprocessed events, applies them, and marks them processed.
// Idempotent at the row level: events already in `file_events` will not be re-inserted.
//
// All raw event IDs from the batch (including those collapsed by debounce/pairing)
// are marked processed at the end — otherwise duplicates would re-surface on the
// next tick and starve forward progress.
func (p *EventProcessor) ProcessBatch(ctx context.Context) error {
	raw, err := p.events.ListUnprocessed(500)
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		return nil
	}

	allIDs := make([]string, 0, len(raw))
	for _, e := range raw {
		if e.ID != "" {
			allIDs = append(allIDs, e.ID)
		}
	}

	pending := p.pairRenames(raw)
	pending = p.debounce(pending, int64(p.EventDebounceMs))

	// touched collects root_ids that had at least one successfully processed
	// event in this batch. We dedup to publish exactly one RecentChanged per
	// root regardless of batch size. NB: if process() returns an error we
	// `continue` — that event's root is NOT added to touched, so a batch
	// where every event for a root errors out yields no publish for that
	// root. The events are still MarkProcessed below (no retry), so callers
	// must not rely on RecentChanged for at-least-once delivery; treat it as
	// a "something probably changed" hint.
	touched := make(map[string]struct{}, 4)
	for _, e := range pending {
		if err := p.process(ctx, e); err != nil {
			p.log.Warn("process event", zap.String("path", e.Path), zap.String("op", e.Op), zap.Error(err))
			continue
		}
		touched[e.RootID] = struct{}{}
	}

	if err := p.events.MarkProcessed(allIDs, time.Now().UnixMilli()); err != nil {
		return err
	}

	// Publish AFTER MarkProcessed so a publish failure can't cause re-
	// processing. Trade-off: a crash between MarkProcessed and the loop
	// below silently loses RecentChanged for this batch (events are already
	// marked processed, no retry). For a UI "something changed" notification
	// this is acceptable; subscribers should not rely on guaranteed delivery.
	for rootID := range touched {
		p.bus.Publish(common.EventRecentChanged, map[string]any{"root_id": rootID})
	}
	return nil
}

func (p *EventProcessor) process(ctx context.Context, e repo.FileEvent) error {
	switch e.Op {
	case "create":
		isOpaque := false
		if e.IsDir && p.ig != nil && p.ig.IsContainerDir(filepath.Base(e.Path)) {
			isOpaque = true
		}
		if err := p.files.Upsert(repo.FileIndex{
			ID: repo.NewID(), RootID: e.RootID, Path: e.Path, Parent: pathutil.Parent(e.Path),
			IsDir: e.IsDir, IsOpaque: isOpaque, Status: "present", Mtime: e.DetectedAt,
		}); err != nil {
			return err
		}
		if !e.IsDir {
			_ = p.parse.InsertPending(e.Path)
		}
		p.markNearestWikiNodeDirty(e.Path)

	case "modify":
		p.markNearestWikiNodeDirty(e.Path)

	case "delete":
		if err := p.files.DeleteByPath(e.RootID, e.Path); err != nil {
			return err
		}
		p.markNearestWikiNodeDirty(e.Path)

	case "rename":
		if e.RenameTo == "" {
			// Unpaired rename (source-only) — treat as delete
			if err := p.files.DeleteByPath(e.RootID, e.Path); err != nil {
				return err
			}
			p.markNearestWikiNodeDirty(e.Path)
			return nil
		}
		if !e.IsDir {
			// File rename = delete + create
			if err := p.files.DeleteByPath(e.RootID, e.Path); err != nil {
				return err
			}
			if err := p.files.Upsert(repo.FileIndex{
				ID: repo.NewID(), RootID: e.RootID, Path: e.RenameTo,
				Parent: pathutil.Parent(e.RenameTo), Status: "present",
				Mtime: e.DetectedAt,
			}); err != nil {
				return err
			}
			p.markNearestWikiNodeDirty(e.Path)
			p.markNearestWikiNodeDirty(e.RenameTo)
			return nil
		}
		// Directory rename → cascade UPDATE inside a single transaction
		tx, err := p.db.Begin()
		if err != nil {
			return err
		}
		if err := p.files.RewritePathPrefix(tx, e.RootID, e.Path, e.RenameTo); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := p.nodes.RewritePathPrefix(tx, e.Path, e.RenameTo); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := p.events.RewritePathPrefix(tx, e.RootID, e.Path, e.RenameTo); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		p.markNearestWikiNodeDirty(e.Path)
		p.markNearestWikiNodeDirty(e.RenameTo)
		p.bus.Publish(common.EventNodeUpdated, map[string]any{"path": e.RenameTo})
	}
	return nil
}

// markNearestWikiNodeDirty walks up the directory tree from the event path,
// marking the nearest existing wiki_node dirty (which triggers regeneration).
func (p *EventProcessor) markNearestWikiNodeDirty(path string) {
	cur := pathutil.Parent(path)
	for {
		n, err := p.nodes.Get(cur)
		if err == nil && n != nil {
			_ = p.nodes.SetDirty(cur, true)
			return
		}
		if cur == "/" {
			return
		}
		cur = pathutil.Parent(cur)
	}
}

// pairRenames groups a Rename event with a subsequent Create event on the
// same root, within 1 second, where the Create is on a directory and the
// Rename had no RenameTo. The pair becomes one rename event with
// IsDir=true and RenameTo set. Standalone Renames remain as-is (treated as
// delete by process()).
//
// NOTE: This is a best-effort temporal pair. fsnotify on Linux does not
// expose inotify cookies; cross-watch renames cannot be detected. This
// works for the common case of `mv watched_dir new_watched_dir`.
func (p *EventProcessor) pairRenames(in []repo.FileEvent) []repo.FileEvent {
	if len(in) < 2 {
		return in
	}
	// Sort by detected_at ascending so we can pair within a forward sliding window
	sort.SliceStable(in, func(i, j int) bool {
		return in[i].DetectedAt < in[j].DetectedAt
	})
	skip := make(map[int]bool, 4)
	merged := make([]repo.FileEvent, 0, len(in))
	for i, e := range in {
		if skip[i] {
			continue
		}
		if e.Op == "rename" && e.RenameTo == "" {
			// Look forward for a matching Create within 1s, same root
			for j := i + 1; j < len(in); j++ {
				if skip[j] {
					continue
				}
				c := in[j]
				if c.DetectedAt-e.DetectedAt > 1000 {
					break
				}
				if c.RootID == e.RootID && c.Op == "create" && c.IsDir {
					m := e
					m.RenameTo = c.Path
					m.IsDir = true
					merged = append(merged, m)
					skip[i] = true
					skip[j] = true
					break
				}
			}
			if !skip[i] {
				merged = append(merged, e)
				skip[i] = true
			}
		} else {
			merged = append(merged, e)
		}
	}
	return merged
}

// debounce collapses duplicate (root, path, op) events within windowMs.
func (p *EventProcessor) debounce(in []repo.FileEvent, windowMs int64) []repo.FileEvent {
	if len(in) < 2 {
		return in
	}
	type k struct{ root, path, op string }
	last := map[k]int64{}
	out := make([]repo.FileEvent, 0, len(in))
	for _, e := range in {
		key := k{e.RootID, e.Path, e.Op}
		if lastT, ok := last[key]; ok && e.DetectedAt-lastT < windowMs {
			continue
		}
		last[key] = e.DetectedAt
		out = append(out, e)
	}
	return out
}

// Run processes events on a timer and drains user-notes sync tasks.
// Call from a goroutine; cancel ctx to stop.
func (p *EventProcessor) Run(ctx context.Context, tickEvery time.Duration) {
	t := time.NewTicker(tickEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := p.ProcessBatch(ctx); err != nil {
				p.log.Warn("ProcessBatch", zap.Error(err))
			}
		case task := <-p.SyncIn:
			if err := p.SyncUserNotesFromDisk(task); err != nil {
				p.log.Warn("user-notes sync", zap.String("path", task.NodePath), zap.Error(err))
			}
		}
	}
}
