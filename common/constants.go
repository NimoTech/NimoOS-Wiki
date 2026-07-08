package common

var WikiVersion = "dev"

const (
	URLFileName = "wiki.url"
	Localhost   = "127.0.0.1"

	// API paths
	V1APIPath = "/v1/wiki"
	V1DocPath = "/doc/v1/wiki"

	// MessageBus event types
	EventNodeUpdated    = "Wiki:NodeUpdated"
	EventRecentChanged  = "Wiki:RecentChanged"
	EventPendingChanged = "Wiki:PendingChanged"
	EventRootEnabled    = "Wiki:RootEnabled"
	EventRootDisabled   = "Wiki:RootDisabled"
	EventWriteFailed    = "Wiki:WriteFailed"
)
