package scanner

import (
	"fmt"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

func upd(g *StormGuard, b map[string]int) (in, out []string) {
	in, out = g.Update(b)
	sort.Strings(in)
	sort.Strings(out)
	return
}

func TestPerRootFuseHysteresis(t *testing.T) {
	g := NewStormGuard(50000, 5000, 200000, 20000)
	in, _ := upd(g, map[string]int{"a": 50001})
	if len(in) != 1 || in[0] != "a" || !g.IsStorming("a") {
		t.Fatalf("enter: %v", in)
	}
	// dropping to between Low and High: stays storming (hysteresis)
	in, out := upd(g, map[string]int{"a": 30000})
	if len(in) != 0 || len(out) != 0 || !g.IsStorming("a") {
		t.Fatalf("hysteresis broken: in=%v out=%v", in, out)
	}
	// below Low: exits
	_, out = upd(g, map[string]int{"a": 4999})
	if len(out) != 1 || out[0] != "a" || g.IsStorming("a") {
		t.Fatalf("exit: %v", out)
	}
}

func TestGlobalFuseGreedySelection(t *testing.T) {
	// perHigh=50000, so all six backlogs below must stay under the
	// per-root fuse — otherwise they're captured there and the
	// multi-candidate greedy path in the global fuse never runs (this is
	// exactly the bug the previous version of this test had: 90k/80k both
	// tripped per-root, leaving only a single candidate for the greedy
	// loop). Values here are all < 50000 individually, but total 215500 >
	// globalHigh(200000), forcing the global fuse to pick among six.
	//
	// Hand-traced greedy (desc by backlog: a45000,b44000,c43000,d42000,
	// e41000,f500; remaining starts at total=215500, checked BEFORE each
	// pick):
	//   pick a: 215500 >= 20000 → remaining = 170500
	//   pick b: 170500 >= 20000 → remaining = 126500
	//   pick c: 126500 >= 20000 → remaining =  83500
	//   pick d:  83500 >= 20000 → remaining =  41500
	//   pick e:  41500 >= 20000 → remaining =    500
	//   f:         500  < 20000 → loop breaks, f NOT picked
	// So the greedy fuse selects {a,b,c,d,e} and leaves f (the smallest)
	// unselected, with a final unselected backlog of 500 < globalLow.
	g := NewStormGuard(50000, 5000, 200000, 20000)
	in, _ := upd(g, map[string]int{
		"a": 45000, "b": 44000, "c": 43000, "d": 42000, "e": 41000, "f": 500,
	})
	if len(in) != 5 {
		t.Fatalf("greedy picks: %v", in)
	}
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		if !g.IsStorming(id) {
			t.Fatalf("expected %s selected by greedy fuse", id)
		}
	}
	if g.IsStorming("f") {
		t.Fatalf("expected f (smallest) NOT selected by greedy fuse")
	}
	rem := 0
	for id, n := range map[string]int{"a": 45000, "b": 44000, "c": 43000, "d": 42000, "e": 41000, "f": 500} {
		if !g.IsStorming(id) {
			rem += n
		}
	}
	if rem >= 20000 {
		t.Fatalf("remaining unstormed backlog %d >= globalLow", rem)
	}
}

func TestGlobalFuseSelectionStableAcrossJitter(t *testing.T) {
	g := NewStormGuard(50000, 5000, 200000, 20000)

	// tick 1: same six-root scenario as TestGlobalFuseGreedySelection —
	// greedy settles on {a,b,c,d,e}, f (500) left out (total 215500 >
	// globalHigh, unselected remainder 500 < globalLow).
	in1, out1 := upd(g, map[string]int{
		"a": 45000, "b": 44000, "c": 43000, "d": 42000, "e": 41000, "f": 500,
	})
	if len(out1) != 0 {
		t.Fatalf("tick1 unexpected exits: %v", out1)
	}
	sel1 := map[string]bool{}
	for _, id := range in1 {
		sel1[id] = true
	}

	// tick 2: every backlog jitters by ±50~150, total is still >
	// globalHigh(200000), and the OLD selection {a,b,c,d,e} is still
	// adequate under the new numbers (unselected remainder becomes
	// 550 < globalLow). Before the fix, Update rebuilt globalSel from
	// scratch on every such tick, so near-boundary roots could flap
	// in/out purely from this kind of jitter. After the fix, an adequate
	// existing selection must be left untouched — entered/exited both
	// empty.
	in2, out2 := upd(g, map[string]int{
		"a": 45050, "b": 43950, "c": 43050, "d": 41950, "e": 41050, "f": 550,
	})
	if len(in2) != 0 || len(out2) != 0 {
		t.Fatalf("selection flapped on jitter: entered=%v exited=%v", in2, out2)
	}
	for id := range sel1 {
		if !g.IsStorming(id) {
			t.Fatalf("root %s dropped out of stable selection after jitter", id)
		}
	}
}

func TestGlobalFuseFlatDistribution(t *testing.T) {
	// Boundary from review: 100 roots x 2001 each, no per-root trigger, total
	// 200100 > 200k. Filtering by a fixed EventFuseLow would select nothing;
	// the greedy algorithm must select some roots so that the remaining total
	// after selection is < 20000.
	b := map[string]int{}
	for i := 0; i < 100; i++ {
		b[fmt.Sprintf("r%03d", i)] = 2001
	}

	g := NewStormGuard(50000, 5000, 200000, 20000)
	in, _ := g.Update(b)
	if len(in) == 0 {
		t.Fatal("flat distribution selected nothing — global fuse is hollow")
	}
	rem := 0
	for id, n := range b {
		if !g.IsStorming(id) {
			rem += n
		}
	}
	if rem >= 20000 {
		t.Fatalf("remaining backlog %d >= GlobalLow", rem)
	}

	// Determinism (found in review): with a perfectly flat backlog, map
	// iteration order differs on every run, so the greedy sort is unstable
	// without an explicit tie-break — feeding the same map to two independent
	// fresh guards could select different root sets, which is bad for
	// reproducible ops troubleshooting. Assert the two guards select exactly
	// the same membership.
	g2 := NewStormGuard(50000, 5000, 200000, 20000)
	in2, _ := g2.Update(b)
	sort.Strings(in)
	sort.Strings(in2)
	if len(in) != len(in2) {
		t.Fatalf("nondeterministic selection size: %v vs %v", in, in2)
	}
	for i := range in {
		if in[i] != in2[i] {
			t.Fatalf("nondeterministic selection membership: %v vs %v", in, in2)
		}
	}
}

func TestGlobalFuseReleasesWhenDrained(t *testing.T) {
	g := NewStormGuard(50000, 5000, 200000, 20000)
	g.Update(map[string]int{"a": 150000, "b": 100000})
	_, out := g.Update(map[string]int{"a": 1000, "b": 500}) // total < GlobalLow
	if len(out) == 0 || g.IsStorming("a") || g.IsStorming("b") {
		t.Fatalf("global release failed: %v", out)
	}
}

func TestStormGuard_CountLimit(t *testing.T) {
	require.Equal(t, 40, NewStormGuard(20, 5, 10, 2).CountLimit())
	require.Equal(t, 60, NewStormGuard(10, 5, 30, 2).CountLimit())
	require.Equal(t, 1, NewStormGuard(0, 0, 0, 0).CountLimit())
}

func TestRootAbsentFromBacklogExits(t *testing.T) {
	// once the backlog drains to zero, CountUnprocessedByRoot no longer
	// returns that root (no row from GROUP BY) — absence is equivalent to
	// backlog 0, and the guard must still be able to exit the storm.
	g := NewStormGuard(50000, 5000, 200000, 20000)
	g.Update(map[string]int{"a": 60000})
	_, out := g.Update(map[string]int{})
	if len(out) != 1 || g.IsStorming("a") {
		t.Fatalf("absent root must exit: %v", out)
	}
}
