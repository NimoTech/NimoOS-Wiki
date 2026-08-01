package roots

// This file tests whether the manager's three lifecycle methods
// (Create/SetEnabled/Delete), after successfully writing to the DB, also
// correctly push the core authz grant (authz-source-decoupling project
// Task 5). Uses fakePusher to assert call count and args, without depending
// on the real rootsync HTTP client.

import (
	"context"
	"testing"

	"github.com/NimoTech/NimoOS-Wiki/pkg/db"
	"github.com/NimoTech/NimoOS-Wiki/service/repo"
	"github.com/NimoTech/NimoOS-Wiki/service/rootsync"
	"github.com/stretchr/testify/require"
)

// fakePusher is a test double for the pusher interface, recording the args
// of each call for assertions.
type fakePusher struct {
	upserts []rootsync.Grant
	deletes []string
}

func (f *fakePusher) Upsert(_ context.Context, g rootsync.Grant) error {
	f.upserts = append(f.upserts, g)
	return nil
}

func (f *fakePusher) Delete(_ context.Context, id string) error {
	f.deletes = append(f.deletes, id)
	return nil
}

// newManagerWithFakePusher builds a manager backed by a temp in-memory
// wiki.db + fakePusher; DB setup is copied from manager_test.go's
// setupManagerWithIgnore.
func newManagerWithFakePusher(t *testing.T) (*Manager, *fakePusher) {
	t.Helper()
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	roots := repo.NewWikiRoots(d)
	nodes := repo.NewWikiNodes(d)
	files := repo.NewFileIndex(d)
	events := repo.NewFileEvents(d)
	m := NewManager(roots, nodes, files, events, &fakeBus{}, nil)
	fp := &fakePusher{}
	m.SetPusher(fp)
	return m, fp
}

func TestCreate_PushesUpsert(t *testing.T) {
	m, fp := newManagerWithFakePusher(t)
	dir := t.TempDir()

	id, _, err := m.Create(CreateArgs{Path: dir, Level: "space"})
	require.NoError(t, err)

	require.Len(t, fp.upserts, 1)
	require.Equal(t, id, fp.upserts[0].RootID)
	require.Equal(t, dir, fp.upserts[0].Path)
	require.True(t, fp.upserts[0].Enabled)
}

func TestSetEnabled_PushesUpsert(t *testing.T) {
	m, fp := newManagerWithFakePusher(t)
	dir := t.TempDir()
	id, _, err := m.Create(CreateArgs{Path: dir, Level: "space"})
	require.NoError(t, err)
	fp.upserts = nil // only care about this SetEnabled call

	require.NoError(t, m.SetEnabled(id, false))

	require.Len(t, fp.upserts, 1)
	require.Equal(t, id, fp.upserts[0].RootID)
	require.False(t, fp.upserts[0].Enabled)
}

func TestDelete_PushesDelete(t *testing.T) {
	m, fp := newManagerWithFakePusher(t)
	dir := t.TempDir()
	id, _, err := m.Create(CreateArgs{Path: dir, Level: "space"})
	require.NoError(t, err)
	fp.upserts = nil

	require.NoError(t, m.Delete(id, false))

	require.Len(t, fp.deletes, 1)
	require.Equal(t, id, fp.deletes[0])
}

// pusherErr is a fake pusher that always fails, used to verify the failure
// path: it only sets needs_reconcile, never returns an error or blocks the
// caller.
type pusherErr struct{ err error }

func (p pusherErr) Upsert(context.Context, rootsync.Grant) error { return p.err }
func (p pusherErr) Delete(context.Context, string) error         { return p.err }

// TestCreate_PushFailureMarksNeedsAuthzPushButDoesNotFail covers the option B
// fix: an upsert push failure should set needs_authz_push (the dedicated
// retry field), not incorrectly reuse needs_reconcile's FS-rescan semantics
// (an old bug, now fixed).
func TestCreate_PushFailureMarksNeedsAuthzPushButDoesNotFail(t *testing.T) {
	m, rootsRepo := setupManager(t)
	m.SetPusher(pusherErr{err: context.DeadlineExceeded})
	dir := t.TempDir()

	id, _, err := m.Create(CreateArgs{Path: dir, Level: "space"})
	require.NoError(t, err) // a push failure doesn't affect Create's own success

	r, err := rootsRepo.Get(id)
	require.NoError(t, err)
	require.True(t, r.NeedsAuthzPush)
	require.False(t, r.NeedsReconcile, "an upsert push failure should no longer incorrectly set the FS-rescan marker")
}

// TestSetEnabled_PushFailureMarksNeedsAuthzPush covers that SetEnabled also
// goes through the pushUpsert failure path, likewise only setting
// needs_authz_push.
func TestSetEnabled_PushFailureMarksNeedsAuthzPush(t *testing.T) {
	m, rootsRepo := setupManager(t)
	dir := t.TempDir()
	id, _, err := m.Create(CreateArgs{Path: dir, Level: "space"})
	require.NoError(t, err)

	m.SetPusher(pusherErr{err: context.DeadlineExceeded})
	require.NoError(t, m.SetEnabled(id, false))

	r, err := rootsRepo.Get(id)
	require.NoError(t, err)
	require.True(t, r.NeedsAuthzPush)
}

// TestDelete_PushFailureMarksAuthzDirtyButDoesNotFail covers the separate
// delete-push-failure path: at that point the root row has already been
// deleted, so there's nowhere in the DB to persist a marker — the only
// option is to set the manager's in-memory dirty flag (authzDirty), leaving
// the retry loop to cover it via a full Reconcile.
func TestDelete_PushFailureMarksAuthzDirtyButDoesNotFail(t *testing.T) {
	m, _ := setupManager(t)
	dir := t.TempDir()
	id, _, err := m.Create(CreateArgs{Path: dir, Level: "space"})
	require.NoError(t, err)

	require.False(t, m.AuthzDirty(), "should not be dirty initially")
	m.SetPusher(pusherErr{err: context.DeadlineExceeded})

	require.NoError(t, m.Delete(id, false)) // a push failure doesn't affect Delete's own success
	require.True(t, m.AuthzDirty(), "a delete push failure should set the in-memory dirty flag")

	m.ClearAuthzDirty()
	require.False(t, m.AuthzDirty())
}
