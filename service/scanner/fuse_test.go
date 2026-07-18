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
	g := NewStormGuard(50000, 5000, 200000, 20000)
	// 三根 90k/80k/40k:无 per-root 触发,总 210k > 200k
	in, _ := upd(g, map[string]int{"a": 90000, "b": 80000, "c": 40000})
	// 贪心:选 a(剩 120k)→ 选 b(剩 40k)→ 选 c(剩 0 < 20k 停?先判后选:
	// 选完 b 剩 40k 仍 >= 20k → 继续选 c → 剩 0 < 20k 停)
	if len(in) != 3 {
		t.Fatalf("greedy picks: %v", in)
	}
}

func TestGlobalFuseFlatDistribution(t *testing.T) {
	// 评审边界:100 根 × 各 2001,无 per-root 触发,总 200100 > 200k。
	// 固定 EventFuseLow 过滤会一个都选不中;贪心必须选中若干根,
	// 且选完后剩余合计 < 20000。
	g := NewStormGuard(50000, 5000, 200000, 20000)
	b := map[string]int{}
	for i := 0; i < 100; i++ {
		b[fmt.Sprintf("r%03d", i)] = 2001
	}
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
