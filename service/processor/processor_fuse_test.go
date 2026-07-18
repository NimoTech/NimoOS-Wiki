package processor

import (
	"sync"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Wiki/pkg/db"
	"github.com/NimoTech/NimoOS-Wiki/service/repo"
	"github.com/NimoTech/NimoOS-Wiki/service/scanner"
	"github.com/stretchr/testify/require"
)

// recordingBus is a tiny fake eventbus.Bus that records every Publish call so
// tests can assert on emitted events without a real MessageBus socket.
type recordingBus struct {
	mu     sync.Mutex
	events []recordedEvent
}

type recordedEvent struct {
	eventType string
	payload   any
}

func (b *recordingBus) Publish(eventType string, payload any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, recordedEvent{eventType, payload})
}

func (b *recordingBus) Close() error { return nil }

func (b *recordingBus) find(eventType string) []recordedEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []recordedEvent
	for _, e := range b.events {
		if e.eventType == eventType {
			out = append(out, e)
		}
	}
	return out
}

func TestProcessorDrivesGuardAndMarksReconcile(t *testing.T) {
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	files := repo.NewFileIndex(d)
	events := repo.NewFileEvents(d)
	nodes := repo.NewWikiNodes(d)
	parse := repo.NewParseStatus(d)
	roots := repo.NewWikiRoots(d)
	bus := &recordingBus{}

	require.NoError(t, roots.Insert(repo.WikiRoot{
		ID: "root1", Path: "/DATA/root1", Level: "space",
		WatchMode: "auto", StorageMode: "local", Enabled: true,
		ScanIntervalS: 60, CreatedAt: time.Now().UnixMilli(),
	}))

	guard := scanner.NewStormGuard(5, 1, 1000, 100)
	p := New(d, files, events, nodes, parse, bus, nil, nil, guard, roots, nil)

	// 1. Six unprocessed events for root1 — backlog 6 > perHigh 5 → storm.
	now := time.Now().UnixMilli()
	ids := make([]string, 0, 6)
	for i := 0; i < 6; i++ {
		id := repo.NewID()
		ids = append(ids, id)
		require.NoError(t, events.Insert(repo.FileEvent{
			ID: id, RootID: "root1", Path: "/DATA/root1/f" + string(rune('a'+i)) + ".txt",
			Op: "create", DetectedAt: now + int64(i),
		}))
	}

	// 2. Drive the fuse.
	p.updateFuse()

	require.True(t, guard.IsStorming("root1"), "backlog 6 > perHigh 5 should storm root1")

	root, err := roots.Get("root1")
	require.NoError(t, err)
	require.True(t, root.NeedsReconcile, "storm entry must persist needs_reconcile=1")

	enters := bus.find("Wiki:IndexStorm")
	require.Len(t, enters, 1)
	payload, ok := enters[0].payload.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "root1", payload["root_id"])
	require.Equal(t, "enter", payload["state"])

	// 3. Mark everything processed, then drive the fuse again — expect exit.
	require.NoError(t, events.MarkProcessed(ids, time.Now().UnixMilli()))
	p.updateFuse()

	require.False(t, guard.IsStorming("root1"), "no backlog left; should exit storm")

	exits := bus.find("Wiki:IndexStorm")
	// find() re-scans the full recorded history, so filter for the exit entry.
	var exitPayload map[string]any
	for _, e := range exits {
		if pl, ok := e.payload.(map[string]any); ok && pl["state"] == "exit" {
			exitPayload = pl
			break
		}
	}
	require.NotNil(t, exitPayload, "expected an exit event after backlog clears")
	require.Equal(t, "root1", exitPayload["root_id"])

	// needs_reconcile is left set at 1 — reconcile itself clears it, not
	// covered by this task.
	root, err = roots.Get("root1")
	require.NoError(t, err)
	require.True(t, root.NeedsReconcile)
}
