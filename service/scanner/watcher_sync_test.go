package scanner

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// A full SyncOut must not drop tasks any more: they land in the pending
// buffer and get re-delivered once the channel drains.
func TestSendSyncBuffersWhenFullAndRetries(t *testing.T) {
	w := NewWatcher(nil, nil, nil, nil, zap.NewNop())
	w.SyncOut = make(chan UserNotesSyncTask, 1)

	t1 := UserNotesSyncTask{RootID: "r1", NodePath: "/a", WikiMDPath: "/a/.wiki.md"}
	t2 := UserNotesSyncTask{RootID: "r1", NodePath: "/b", WikiMDPath: "/b/.wiki.md"}

	w.sendSync(t1) // fills the channel
	w.sendSync(t2) // channel full -> must buffer, not drop

	require.Len(t, w.SyncOut, 1)
	w.pendingMu.Lock()
	require.Len(t, w.pendingSync, 1)
	w.pendingMu.Unlock()

	<-w.SyncOut // drain
	w.retryPendingSync()

	require.Len(t, w.SyncOut, 1)
	got := <-w.SyncOut
	require.Equal(t, "/b", got.NodePath)
	w.pendingMu.Lock()
	require.Empty(t, w.pendingSync)
	w.pendingMu.Unlock()
}

// Same node buffered twice keeps only the latest task (map key = NodePath).
func TestSendSyncDedupesByNodePath(t *testing.T) {
	w := NewWatcher(nil, nil, nil, nil, zap.NewNop())
	w.SyncOut = make(chan UserNotesSyncTask) // unbuffered + no reader = always full

	w.sendSync(UserNotesSyncTask{RootID: "r1", NodePath: "/a", WikiMDPath: "old"})
	w.sendSync(UserNotesSyncTask{RootID: "r1", NodePath: "/a", WikiMDPath: "new"})

	w.pendingMu.Lock()
	defer w.pendingMu.Unlock()
	require.Len(t, w.pendingSync, 1)
	require.Equal(t, "new", w.pendingSync["/a"].WikiMDPath)
}
