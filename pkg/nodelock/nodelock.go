// Package nodelock gives per-path serialization for callers that need to
// coordinate read-modify-write sequences on a single wiki_node without taking
// a global lock. WikiWriter and the user-notes reverse-sync path both use it
// so neither can clobber the other's view of wiki_nodes.user_notes.
//
// Locks are minted lazily on first use and never reclaimed; for a typical NAS
// with O(1000) wiki nodes the memory cost is < 1 MB and irrelevant.
package nodelock

import "sync"

// Locks vends per-path mutexes.
type Locks struct {
	m sync.Map // string -> *sync.Mutex
}

// New constructs an empty Locks.
func New() *Locks { return &Locks{} }

// Lock acquires the mutex for path and returns its Unlock function so the
// caller can `defer unlock()`. Two callers locking the same path serialize;
// callers locking different paths run in parallel.
func (l *Locks) Lock(path string) func() {
	v, _ := l.m.LoadOrStore(path, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}
