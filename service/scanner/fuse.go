package scanner

import (
	"sort"
	"sync"
)

// StormGuard implements the two-level insert fuse (spec §4.1). Pure
// decision logic + a mutex-guarded storm set; no I/O. Watcher calls
// IsStorming on the hot path; the EventProcessor drives Update once per
// tick with fresh per-root unprocessed backlogs.
type StormGuard struct {
	perHigh, perLow       int
	globalHigh, globalLow int

	mu        sync.RWMutex
	perRoot   map[string]bool // roots stormed by their own backlog
	globalSel map[string]bool // roots stormed by the global greedy fuse
}

func NewStormGuard(perHigh, perLow, globalHigh, globalLow int) *StormGuard {
	return &StormGuard{
		perHigh: perHigh, perLow: perLow,
		globalHigh: globalHigh, globalLow: globalLow,
		perRoot: map[string]bool{}, globalSel: map[string]bool{},
	}
}

func (g *StormGuard) IsStorming(rootID string) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.perRoot[rootID] || g.globalSel[rootID]
}

// CountLimit is the saturation bound callers pass to
// FileEventsRepo.CountUnprocessedByRoot: twice the highest fuse threshold is
// enough to decide every hysteresis edge without counting the whole table.
func (g *StormGuard) CountLimit() int {
	m := g.perHigh
	if g.globalHigh > m {
		m = g.globalHigh
	}
	if m <= 0 {
		return 1
	}
	return 2 * m
}

// Update advances the storm state from fresh backlog counts and returns the
// roots that entered / exited storm mode in this step. Roots absent from
// backlogs are treated as backlog 0.
func (g *StormGuard) Update(backlogs map[string]int) (entered, exited []string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	before := g.stormSetLocked()

	// per-root hysteresis
	for id, b := range backlogs {
		if b > g.perHigh {
			g.perRoot[id] = true
		}
	}
	for id := range g.perRoot {
		if backlogs[id] < g.perLow { // absent → 0 → exits
			delete(g.perRoot, id)
		}
	}

	// global fuse: greedy largest-first until remaining < globalLow
	total := 0
	for _, b := range backlogs {
		total += b
	}
	if total > g.globalHigh {
		// Adequacy check first: if the CURRENT selection (per-root ∪
		// globalSel) still keeps the unselected backlog under globalLow,
		// leave it untouched. Rebuilding from scratch every tick made
		// near-boundary roots flap in/out of globalSel purely from small
		// backlog jitter, even when the existing selection was still
		// perfectly adequate (spec §4.1 review finding).
		excluded := map[string]bool{}
		for id := range g.perRoot {
			excluded[id] = true
		}
		for id := range g.globalSel {
			excluded[id] = true
		}
		adequate := total
		for id := range excluded {
			adequate -= backlogs[id]
		}
		if adequate >= g.globalLow {
			g.globalSel = map[string]bool{}
			remaining := total
			for id := range g.perRoot { // already silenced by per-root fuse
				remaining -= backlogs[id]
			}
			type rb struct {
				id string
				b  int
			}
			var rest []rb
			for id, b := range backlogs {
				if !g.perRoot[id] {
					rest = append(rest, rb{id, b})
				}
			}
			// Deterministic tie-break (backlog desc, then id asc) so
			// selection is reproducible across ticks/runs for ops/debugging.
			sort.Slice(rest, func(i, j int) bool {
				if rest[i].b != rest[j].b {
					return rest[i].b > rest[j].b
				}
				return rest[i].id < rest[j].id
			})
			for _, r := range rest {
				if remaining < g.globalLow {
					break
				}
				g.globalSel[r.id] = true
				remaining -= r.b
			}
		}
		// else: existing selection still adequate — keep as-is.
	} else if total < g.globalLow {
		g.globalSel = map[string]bool{}
	}
	// globalLow <= total <= globalHigh: keep current selection (hysteresis)

	after := g.stormSetLocked()
	for id := range after {
		if !before[id] {
			entered = append(entered, id)
		}
	}
	for id := range before {
		if !after[id] {
			exited = append(exited, id)
		}
	}
	return entered, exited
}

func (g *StormGuard) stormSetLocked() map[string]bool {
	s := make(map[string]bool, len(g.perRoot)+len(g.globalSel))
	for id := range g.perRoot {
		s[id] = true
	}
	for id := range g.globalSel {
		s[id] = true
	}
	return s
}
