package rca

import (
	"math/rand"
	"testing"

	"rca/internal/detect"
	"rca/internal/graph"
	"rca/internal/model"
	"rca/internal/sim"
)

// buildEngine 用模拟器构造一个故障场景并装配 RCA 引擎。
func buildEngine(t *testing.T, seed int64, target string, kind model.FaultKind, reverse bool) *Engine {
	t.Helper()
	simulator := sim.New(seed)
	rng := rand.New(rand.NewSource(seed ^ 0x5DEECE66D))
	fault := sim.FaultSpec(target, kind, reverse, rng)
	sc := simulator.GenerateScenario(0, []model.InjectedFault{fault})
	g := graph.Build(sc.Records)
	start, end, bs, be := detect.DetectWindow(sc)
	anoms := detect.Analyze(sc, start, end, bs, be)
	return NewEngine(g, sc, anoms, start, end, bs, be)
}

// 单故障: 根因进前三,排名完整,候选附带证据。
func TestRankingFindsRootWithEvidence(t *testing.T) {
	cases := []struct {
		target string
		kind   model.FaultKind
	}{
		{"redis-cache", model.FaultLatency},
		{"mysql-cluster", model.FaultResource},
		{"search-engine", model.FaultError},
		{"mq-broker", model.FaultConnPool},
	}
	for _, c := range cases {
		eng := buildEngine(t, 4242, c.target, c.kind, false)
		ranking, visit, clusters := eng.Rank()
		if len(clusters) == 0 {
			t.Fatalf("%s: 未检测到故障簇", c.target)
		}
		if len(ranking) != len(eng.g.Nodes) {
			t.Fatalf("%s: 排名长度 %d != 节点数 %d", c.target, len(ranking), len(eng.g.Nodes))
		}
		found := -1
		for i, cand := range ranking {
			if cand.Service == c.target {
				found = i + 1
			}
			if cand.AnomalyScore > 0.3 && len(cand.MetricEvidence) == 0 {
				t.Errorf("%s: 高异常候选 %s 缺少指标证据", c.target, cand.Service)
			}
			if len(cand.PropagationPath) == 0 {
				t.Errorf("%s: 候选 %s 缺少传导路径", c.target, cand.Service)
			}
		}
		if found < 0 || found > 3 {
			t.Errorf("%s: 根因名次 = %d, 期望进入前三", c.target, found)
		}
		// 簇内归一化后访问概率应全部落在 [0,1]
		for n, v := range visit {
			if v < 0 || v > 1.0001 {
				t.Errorf("%s: 节点 %s 访问概率越界 %.4f", c.target, n, v)
			}
		}
	}
}

// 反向传导: 连接池饱和的上游应被标为 reverse,且根因仍排在它前面。
func TestReversePropagationClassification(t *testing.T) {
	eng := buildEngine(t, 9090, "mq-broker", model.FaultConnPool, true)
	ranking, _, _ := eng.Rank()
	pos := map[string]int{}
	origin := map[string]string{}
	for i, c := range ranking {
		pos[c.Service] = i + 1
		origin[c.Service] = c.Origin
	}
	if pos["mq-broker"] > 3 {
		t.Errorf("反向场景根因 mq-broker 名次 %d 未进前三", pos["mq-broker"])
	}
	reverseCount := 0
	for _, o := range origin {
		if o == "reverse" {
			reverseCount++
		}
	}
	if reverseCount == 0 {
		t.Error("反向传导场景未识别出任何 reverse 节点")
	}
}

// 双故障: 两个独立根因都应进入前三。
func TestDoubleFaultBothRanked(t *testing.T) {
	seed := int64(31337)
	simulator := sim.New(seed)
	rng := rand.New(rand.NewSource(seed ^ 0x5DEECE66D))
	f1 := sim.FaultSpec("mysql-cluster", model.FaultResource, false, rng)
	f2 := sim.FaultSpec("stream-pusher", model.FaultLatency, false, rng)
	sc := simulator.GenerateScenario(1, []model.InjectedFault{f1, f2})
	g := graph.Build(sc.Records)
	start, end, bs, be := detect.DetectWindow(sc)
	anoms := detect.Analyze(sc, start, end, bs, be)
	eng := NewEngine(g, sc, anoms, start, end, bs, be)
	ranking, _, clusters := eng.Rank()

	top3 := map[string]bool{}
	for i := 0; i < 3 && i < len(ranking); i++ {
		top3[ranking[i].Service] = true
	}
	for _, root := range []string{"mysql-cluster", "stream-pusher"} {
		if !top3[root] {
			var names []string
			for i := 0; i < 5 && i < len(ranking); i++ {
				names = append(names, ranking[i].Service)
			}
			t.Errorf("双故障根因 %s 未进前三, 前五=%v (簇数=%d)", root, names, len(clusters))
		}
	}
}

// 来源判定只能取三种合法值。
func TestOriginValuesValid(t *testing.T) {
	eng := buildEngine(t, 5555, "redis-cache", model.FaultLatency, false)
	ranking, _, _ := eng.Rank()
	for _, c := range ranking {
		switch c.Origin {
		case "self", "propagated", "reverse":
		default:
			t.Errorf("非法来源标记 %q", c.Origin)
		}
	}
}
