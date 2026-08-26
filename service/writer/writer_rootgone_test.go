package writer

import (
	"os"
	"testing"

	"github.com/NimoTech/NimoOS-Wiki/service/repo"
	"github.com/stretchr/testify/require"
)

// A flush ENOENT on a node whose directory is gone must fire OnRootGone
// exactly with the node's root id; other errors must not.
func TestMaybeRootGone(t *testing.T) {
	w, _, _, _ := setupWriter(t)

	tmp := t.TempDir()
	rootID := "root-1"
	n := repo.WikiNode{RootID: &rootID, Path: tmp}

	var fired []string
	w.SetOnRootGone(func(id string) { fired = append(fired, id) })

	// Path exists -> not fired even for an ENOENT-looking error.
	w.maybeRootGone(n, os.ErrNotExist)
	require.Empty(t, fired)

	// Path gone + ENOENT -> fired once.
	require.NoError(t, os.RemoveAll(tmp))
	w.maybeRootGone(n, os.ErrNotExist)
	require.Equal(t, []string{rootID}, fired)

	// Non-ENOENT error -> not fired.
	w.maybeRootGone(n, os.ErrPermission)
	require.Equal(t, []string{rootID}, fired)
}
