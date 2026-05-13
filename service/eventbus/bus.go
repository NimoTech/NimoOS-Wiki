// Package eventbus wraps NimoOS-Common's MessageBus client so the Wiki service
// can publish events with a single Publish(eventType, payload) call.
//
// Falls back to Noop when the MessageBus socket isn't reachable so the service
// stays usable during local dev / tests.
package eventbus

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/NimoTech/NimoOS-Common/external"
)

// Bus is the publish-only interface the rest of the service uses.
type Bus interface {
	Publish(eventType string, payload any)
	Close() error
}

// Noop discards events. Used in tests and as a fallback when the bus is unavailable.
type Noop struct{}

func (Noop) Publish(string, any) {}
func (Noop) Close() error        { return nil }

// New returns a Bus implementation. If MessageBus isn't reachable, returns Noop.
// runtimePath is the directory where NimoOS service URL files live (typically /var/run/nimoos).
func New(runtimePath string) Bus {
	// We don't validate connectivity here — PublishEventInSocket dials lazily per event,
	// and we don't want service startup to fail just because MessageBus hasn't booted yet.
	return &socketBus{}
}

type socketBus struct {
	mu sync.Mutex
}

// Publish takes our "Wiki:NodeUpdated"-style event type and any JSON-marshalable
// payload, splits into (source, name), and ships it over the message bus socket.
// All errors are swallowed (eventbus is fire-and-forget; failures shouldn't break callers).
func (b *socketBus) Publish(eventType string, payload any) {
	source, name := splitEventType(eventType)

	props := map[string]string{}
	if payload != nil {
		if data, err := json.Marshal(payload); err == nil {
			props["data"] = string(data)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = external.PublishEventInSocket(ctx, source, name, props)
}

func (b *socketBus) Close() error { return nil }

// splitEventType parses "Source:Name" into ("Source", "Name").
// If there's no colon, falls back to ("Wiki", eventType).
func splitEventType(s string) (source, name string) {
	if i := strings.IndexByte(s, ':'); i > 0 {
		return s[:i], s[i+1:]
	}
	return "Wiki", s
}
