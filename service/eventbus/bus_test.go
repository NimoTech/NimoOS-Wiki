package eventbus

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSplitEventType(t *testing.T) {
	s, n := splitEventType("Wiki:NodeUpdated")
	require.Equal(t, "Wiki", s)
	require.Equal(t, "NodeUpdated", n)

	s2, n2 := splitEventType("OrphanEvent")
	require.Equal(t, "Wiki", s2)
	require.Equal(t, "OrphanEvent", n2)
}

func TestNoopBus_DoesNotPanic(t *testing.T) {
	var b Bus = Noop{}
	b.Publish("Wiki:NodeUpdated", map[string]string{"path": "/x"})
	require.NoError(t, b.Close())
}

func TestSocketBus_FailsSilentlyWithoutSocket(t *testing.T) {
	// No MessageBus socket exists at /tmp/message-bus.sock in tests; Publish should
	// swallow the error and return without panicking.
	b := New("/tmp/nonexistent")
	require.NotPanics(t, func() {
		b.Publish("Wiki:NodeUpdated", map[string]string{"path": "/x"})
	})
}
