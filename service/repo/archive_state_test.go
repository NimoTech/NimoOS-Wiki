package repo

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/NimoTech/NimoOS-Wiki/pkg/db"
)

func TestArchiveState_CutoffDefaultsTo90Days(t *testing.T) {
	s := NewArchiveState(0)
	now := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	require.Equal(t, now.Add(-90*24*time.Hour).UnixMilli(), s.CutoffMs(now))
	require.False(t, s.HasArchived())
	s.MarkArchived()
	require.True(t, s.HasArchived())
}

func TestArchiveState_InitFromRepoSeesArchivedRows(t *testing.T) {
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	ev := NewFileEvents(d)

	s := NewArchiveState(30)
	require.NoError(t, s.InitFromRepo(ev))
	require.False(t, s.HasArchived(), "empty table: never archived")

	require.NoError(t, ev.Insert(FileEvent{RootID: "r", Path: "/a", Op: "create", DetectedAt: 1}))
	n, err := ev.ArchiveOlderThan(2)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)

	s2 := NewArchiveState(30)
	require.NoError(t, s2.InitFromRepo(ev))
	require.True(t, s2.HasArchived())
}
