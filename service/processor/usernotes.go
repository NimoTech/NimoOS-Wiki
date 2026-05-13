package processor

import (
	"crypto/sha256"
	"encoding/hex"
	"os"

	"github.com/NimoTech/NimoOS-Wiki/common"
	"github.com/NimoTech/NimoOS-Wiki/pkg/wikimd"
	"github.com/NimoTech/NimoOS-Wiki/service/scanner"
)

// runUserNotesSync handles a UserNotesSyncTask emitted by the Watcher when
// it detects an external edit to a .wiki.md file (e.g., user typed in SMB).
// Extracts the user-notes block from the file and writes it to the DB.
// If the file's user-notes equal what's already in DB, only refresh the
// LastFlushedMtime so we don't keep retriggering on the same edit.
func (p *EventProcessor) runUserNotesSync(t scanner.UserNotesSyncTask) error {
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
