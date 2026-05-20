package processor

import (
	"crypto/sha256"
	"encoding/hex"
	"os"

	"github.com/NimoTech/NimoOS-Wiki/common"
	"github.com/NimoTech/NimoOS-Wiki/pkg/wikimd"
	"github.com/NimoTech/NimoOS-Wiki/service/scanner"
)

// SyncUserNotesFromDisk pulls a node's user-notes from the on-disk .wiki.md
// into the database. Called from two places:
//
//   - EventProcessor.Run, when the Watcher detects an external edit
//     (the .wiki.md mtime advanced past wiki_nodes.last_flushed_mtime).
//   - main.go at startup, to recover edits made while the service was down.
//
// Preconditions: t.WikiMDPath must be a readable file, t.NodePath must be an
// existing wiki_nodes row. If the file's user-notes match what's already in
// the DB, only last_flushed_mtime is bumped (suppresses repeat sync triggers).
// If they differ, user_notes / etag / updated_at are written and a
// Wiki:NodeUpdated event is published.
func (p *EventProcessor) SyncUserNotesFromDisk(t scanner.UserNotesSyncTask) error {
	data, err := os.ReadFile(t.WikiMDPath)
	if err != nil {
		return err
	}
	info, err := os.Stat(t.WikiMDPath)
	if err != nil {
		return err
	}
	mtime := info.ModTime().UnixMilli()

	notes, ok := wikimd.ExtractUserNotes(string(data))
	if !ok {
		notes = ""
	}

	node, err := p.nodes.Get(t.NodePath)
	if err != nil {
		return err
	}
	if node == nil {
		return nil
	}

	if notes != node.UserNotes {
		h := sha256.Sum256([]byte(notes))
		etag := hex.EncodeToString(h[:8])
		if err := p.nodes.SetUserNotes(t.NodePath, notes, etag, mtime); err != nil {
			return err
		}
		p.bus.Publish(common.EventNodeUpdated, map[string]any{"path": t.NodePath})
	} else {
		// Same content — refresh LastFlushedMtime to suppress repeated sync triggers
		_ = p.nodes.RecordFlush(t.NodePath, node.ChecksumSystem, node.LastFlushedAt, mtime)
	}
	return nil
}
