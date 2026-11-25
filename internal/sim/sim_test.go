package sim

import (
	"testing"

	"rca/internal/model"
)

// 固定种子下拓扑结构必须稳定可复现。
func TestTopologyDeterministic(t *testing.T) {
	s1 := New(12345)
	s2 := New(12345)
	if len(s1.services) != len(s2.services) {
		t.Fatal("同种子服务数不一致")
	}
	if len(s1.edges) != len(s2.edges) || len(s1.edges) == 0 {
		t.Fatalf("同种子边数不一致或为空: %d vs %d", len(s1.edges), len(s2.edges))
	}
	for i, e := range s1.edges {
		if e.caller != s2.edges[i].caller || e.callee != s2.edges[i].callee {
			t.Fatalf("同种子拓扑结构不一致 @%d", i)
		}
	}
	// 服务数量在"几十个"量级
	if len(s1.services) < 25 || len(s1.services) > 40 {
		t.Errorf("服务数 %d 不在预期的几十个范围", len(s1.services))
	}
	// 所有边必须从上层指向下层(DAG)
	for _, e := range s1.edges {
		if tierIndexOf(e.caller) >= tierIndexOf(e.callee) {
			t.Errorf("边 %s->%s 不是自上而下的调用方向", e.caller, e.callee)
		}
	}
}

// 不同种子应产生不同的拓扑细节。
func TestDifferentSeedsVary(t *testing.T) {
	s1 := New(1)
	s2 := New(2)
	diff := false
	for _, e1 := range s1.edges {
		found := false
		for _, e2 := range s2.edges {
			if e1.caller == e2.caller && e1.callee == e2.callee {
				found = true
				break
			}
		}
		if !found {
			diff = true
			break
		}
	}
	if !diff {
		t.Error("不同种子的拓扑完全相同,随机性可能失效")
	}
}

// 注入延迟故障后,根因延迟必须显著高于基线,且上游也出现传导增量。
func TestFaultPropagation(t *testing.T) {
	s := New(777)
	target := "redis-cache"
	rng := newTestRNG()
	fault := FaultSpec(target, model.FaultLatency, false, rng)
	sc := s.GenerateScenario(0, []model.InjectedFault{fault})

	var root *model.Service
	for _, svc := range sc.Services {
		if svc.Name == target {
			root = svc
		}
	}
	if root == nil {
		t.Fatal("找不到注入服务")
	}
	base := mean(root.Latency[10:70])
	win := mean(root.Latency[fault.Start+2 : fault.End])
	if win <= base*1.8 {
		t.Errorf("根因延迟未显著升高: 基线 %.1f -> 窗口 %.1f", base, win)
	}

	// 至少有一个直接上游的窗口延迟高于自身基线(传导发生)
	propagated := false
	for _, em := range s.inIdx[target] {
		up := findService(sc, em.caller)
		if up == nil {
			continue
		}
		if mean(up.Latency[fault.Start+2:fault.End]) > mean(up.Latency[10:70])*1.3 {
			propagated = true
		}
	}
	if !propagated {
		t.Error("未观察到故障向上游传导")
	}
}

// 反向传导场景: 上游连接池使用率应明显抬升。
func TestReversePropagationConnPool(t *testing.T) {
	s := New(999)
	target := "mq-broker"
	rng := newTestRNG()
	fault := FaultSpec(target, model.FaultConnPool, true, rng)
	sc := s.GenerateScenario(1, []model.InjectedFault{fault})

	root := findService(sc, target)
	if mean(root.ConnUsage[fault.Start+2:fault.End]) <
		mean(root.ConnUsage[10:70])+0.2 {
		t.Error("根因连接池未显著打满")
	}
	upRaised := false
	for _, em := range s.inIdx[target] {
		up := findService(sc, em.caller)
		if up != nil && mean(up.ConnUsage[fault.Start+2:fault.End]) >
			mean(up.ConnUsage[10:70])+0.15 {
			upRaised = true
		}
	}
	if !upRaised {
		t.Error("反向传导未抬升上游连接池")
	}
}

// 双故障注入后两个目标服务都应出现异常增量。
func TestDoubleFault(t *testing.T) {
	s := New(2024)
	rng := newTestRNG()
	f1 := FaultSpec("mysql-cluster", model.FaultResource, false, rng)
	f2 := FaultSpec("search-engine", model.FaultError, false, rng)
	sc := s.GenerateScenario(2, []model.InjectedFault{f1, f2})
	if !sc.Double || len(sc.Faults) != 2 {
		t.Fatal("双故障标记错误")
	}
	mysql := findService(sc, "mysql-cluster")
	if mean(mysql.CPU[f1.Start+2:f1.End]) <= mean(mysql.CPU[10:70])+0.15 {
		t.Error("资源耗尽故障未提升 mysql CPU")
	}
	se := findService(sc, "search-engine")
	if mean(se.ErrorRate[f2.Start+2:f2.End]) <= mean(se.ErrorRate[10:70])+0.1 {
		t.Error("错误注入未提升 search-engine 错误率")
	}
}

func mean(xs []float64) float64 {
	sum := 0.0
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

func findService(sc *model.Scenario, name string) *model.Service {
	for _, svc := range sc.Services {
		if svc.Name == name {
			return svc
		}
	}
	return nil
}
