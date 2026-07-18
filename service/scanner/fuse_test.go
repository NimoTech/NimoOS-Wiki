package scanner

import (
	"fmt"
	"sort"
	"testing"
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
	// 降到 Low 与 High 之间:保持风暴(滞回)
	in, out := upd(g, map[string]int{"a": 30000})
	if len(in) != 0 || len(out) != 0 || !g.IsStorming("a") {
		t.Fatalf("hysteresis broken: in=%v out=%v", in, out)
	}
	// 低于 Low:退出
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
	// 评审边界:100 根 × 各 2001,无 per-root 触发,总 200100 > 200k。
	// 固定 EventFuseLow 过滤会一个都选不中;贪心必须选中若干根,
	// 且选完后剩余合计 < 20000。
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

	// 确定性(评审发现):完全打平的积压下 map 遍历顺序每次运行都不同,
	// 贪心排序若无显式 tie-break 就不稳定 —— 同样的 map 喂给两个独立的
	// fresh guard 可能选出不同的根集合,不利于可复现的运维排障。
	// 断言两个 guard 选出完全一致的成员。
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
	_, out := g.Update(map[string]int{"a": 1000, "b": 500}) // 总 < GlobalLow
	if len(out) == 0 || g.IsStorming("a") || g.IsStorming("b") {
		t.Fatalf("global release failed: %v", out)
	}
}

func TestRootAbsentFromBacklogExits(t *testing.T) {
	// 积压清零后 CountUnprocessedByRoot 不再返回该根(GROUP BY 无行)——
	// 缺席等价于 backlog 0,必须能退出风暴。
	g := NewStormGuard(50000, 5000, 200000, 20000)
	g.Update(map[string]int{"a": 60000})
	_, out := g.Update(map[string]int{})
	if len(out) != 1 || g.IsStorming("a") {
		t.Fatalf("absent root must exit: %v", out)
	}
}
