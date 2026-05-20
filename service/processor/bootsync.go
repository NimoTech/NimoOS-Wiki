package processor

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/NimoTech/NimoOS-Wiki/service/scanner"
	"go.uber.org/zap"
)

// BootSyncRoot reconciles `.wiki.md` files against `wiki_nodes` at startup.
// For each enabled root's wiki_node, if the on-disk `.wiki.md` mtime is
// strictly greater than the DB's `last_flushed_mtime`, the file was edited
// while the service was down (or while running on a host without fsnotify
// coverage). We pull those edits into the DB synchronously via the same
// reverse-sync logic used during live operation.
//
// Returns the number of nodes that were actually synced. Failures on a single
// node are logged and counted as not-synced; the scan continues so a poison
// node can't block startup.
func (p *EventProcessor) BootSyncRoot(rootID string) (int, error) {
	nodes, err := p.nodes.List(rootID)
	if err != nil {
		return 0, err
	}
	synced := 0
	for _, n := range nodes {
		wikiMD := filepath.Join(n.Path, ".wiki.md")
		info, statErr := os.Stat(wikiMD)
		if statErr != nil {
			// Missing file is normal (never flushed, or user removed it).
			// Anything else (permission denied, network mount glitch) is worth
			// surfacing so a user with broken FS perms can diagnose.
			if !errors.Is(statErr, os.ErrNotExist) {
				p.log.Warn("boot sync: stat failed",
					zap.String("path", wikiMD), zap.Error(statErr))
			}
			continue
		}
		mt := info.ModTime().UnixMilli()
		if mt <= n.LastFlushedMtime {
			continue
		}
		task := scanner.UserNotesSyncTask{
			RootID: rootID, WikiMDPath: wikiMD, NodePath: n.Path,
		}
		if err := p.SyncUserNotesFromDisk(task); err != nil {
			p.log.Warn("boot sync failed for node",
				zap.String("path", n.Path), zap.Error(err))
			continue
		}
		synced++
	}
	return synced, nil
}
