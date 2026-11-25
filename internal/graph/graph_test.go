package graph

import (
	"testing"

	"rca/internal/model"
)

func TestBuildAggregatesEdges(t *testing.T) {
	records := []model.CallRecord{
		{Tick: 0, Caller: "a", Callee: "b", Count: 100, LatencyMs: 20, ErrorCount: 5},
		{Tick: 1, Caller: "a", Callee: "b", Count: 300, LatencyMs: 40, ErrorCount: 9},
		{Tick: 0, Caller: "b", Callee: "c", Count: 50, LatencyMs: 10, ErrorCount: 0},
	}
	g := Build(records)

	if len(g.Nodes) != 3 {
		t.Fatalf("节点数 = %d, 期望 3", len(g.Nodes))
	}
	if len(g.Edges) != 2 {
		t.Fatalf("边数 = %d, 期望 2", len(g.Edges))
	}
	e := g.EdgeValue("a", "b")
	if e == nil {
		t.Fatal("边 a->b 未构建")
	}
	if e.CallCount != 400 {
		t.Errorf("调用量 = %d, 期望 400", e.CallCount)
	}
	// 加权平均延迟: (100*20 + 300*40)/400 = 35
	if d := absF(e.LatencyMs - 35); d > 0.01 {
		t.Errorf("加权延迟 = %.2f, 期望 35", e.LatencyMs)
	}
	// 错误率: 14/400
	if d := absF(e.ErrorRate - 14.0/400.0); d > 0.001 {
		t.Errorf("错误率 = %.4f, 期望 %.4f", e.ErrorRate, 14.0/400.0)
	}
	// 每 tick 平均量: 400/2 = 200
	if d := absF(e.Volume - 200); d > 0.01 {
		t.Errorf("平均调用量 = %.2f, 期望 200", e.Volume)
	}
}

func TestReachAndPaths(t *testing.T) {
	records := []model.CallRecord{
		{Tick: 0, Caller: "a", Callee: "b", Count: 10, LatencyMs: 1, ErrorCount: 0},
		{Tick: 0, Caller: "b", Callee: "c", Count: 10, LatencyMs: 1, ErrorCount: 0},
		{Tick: 0, Caller: "x", Callee: "c", Count: 10, LatencyMs: 1, ErrorCount: 0},
	}
	g := Build(records)

	if !g.ReachDown("a", "c") {
		t.Error("a 应能沿调用方向到达 c")
	}
	if g.ReachDown("c", "a") {
		t.Error("c 不应能反向到达 a")
	}
	path := g.ShortestPathDown("a", "c")
	if len(path) != 3 || path[0] != "a" || path[2] != "c" {
		t.Errorf("最短路径错误: %v", path)
	}
	// 无向路径忽略边方向: c -> a
	up := g.UndirectedPath("c", "a")
	if len(up) != 3 {
		t.Errorf("无向路径错误: %v", up)
	}
}

func absF(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
