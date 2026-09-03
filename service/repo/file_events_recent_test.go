package repo

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

func explain(t *testing.T, d *sql.DB, q string, args ...any) string {
	t.Helper()
	rows, err := d.Query(`EXPLAIN QUERY PLAN `+q, args...)
	require.NoError(t, err)
	defer rows.Close()
	var plan string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &notused, &detail))
		plan += detail + "\n"
	}
	return plan
}

// RecentForNode runs from the WikiWriter's 1s ticker (up to 50 dirty nodes
// per tick). It must be served by idx_file_events_archive_q and never open a
// sorter, or a large table turns the ticker into a full-scan loop.
func TestRecentForNode_UsesArchiveIndex_NoTempBTree(t *testing.T) {
	d := openTestDB(t)
	plan := explain(t, d, recentForNodeSQL, "rootA", "/DATA/x", "/DATA/x/%", 20)
	require.NotContains(t, plan, "TEMP B-TREE", plan)
	require.Contains(t, plan, "idx_file_events_archive_q", plan)
}

func TestRecentForRoot_UsesArchiveIndex_NoTempBTree(t *testing.T) {
	d := openTestDB(t)
	plan := explain(t, d, recentForRootSQL, "rootA", 20)
	require.NotContains(t, plan, "TEMP B-TREE", plan)
	require.Contains(t, plan, "idx_file_events_archive_q", plan)
}

func TestRecentForNode_SkipsArchivedRows(t *testing.T) {
	d := openTestDB(t)
	ev := NewFileEvents(d)
	_, err := d.Exec(`INSERT INTO file_events (id, root_id, path, op, is_dir, detected_at, archived) VALUES
		('a','rootA','/DATA/x/live.txt','create',0,200,0),
		('b','rootA','/DATA/x/old.txt','create',0,100,1)`)
	require.NoError(t, err)
	got, err := ev.RecentForNode("rootA", "/DATA/x", 20)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "/DATA/x/live.txt", got[0].Path, "archived (retention-expired) rows are not 'recent'")
}
