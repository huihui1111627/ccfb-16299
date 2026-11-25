package eval

import (
	"testing"

	"rca/internal/detect"
)

// 窗口检测: 全部 420 个场景的起点偏差都应在 8 tick 以内。
func TestWindowDetectionAccuracy(t *testing.T) {
	plans := BuildPlans()
	largeBias := 0
	for _, p := range plans {
		rep := RunOne(p)
		if rep == nil {
			t.Fatalf("场景 %d 分析失败", p.ID)
		}
		d := rep.WindowError
		if d < 0 {
			d = -d
		}
		if d > 8 {
			largeBias++
			if largeBias <= 3 {
				t.Errorf("场景 %d 窗口偏差 %d tick 超过 8", p.ID, rep.WindowError)
			}
		}
	}
	if largeBias > 0 {
		t.Errorf("共 %d 个场景窗口偏差超过 8 tick", largeBias)
	}
}

// 异常分数必须落在 [0,1],且故障窗口内至少有一个服务明显异常。
func TestAnomalyScoresBounded(t *testing.T) {
	rep := RunOne(BuildPlans()[0])
	maxScore := 0.0
	for _, n := range rep.Nodes {
		if n.Score < 0 || n.Score > 1 {
			t.Fatalf("服务 %s 异常分越界: %.3f", n.Service, n.Score)
		}
		if n.Score > maxScore {
			maxScore = n.Score
		}
		for m, z := range n.ZScore {
			if z < 0 {
				t.Errorf("服务 %s 指标 %s 出现负 z 值 %.2f", n.Service, m, z)
			}
		}
	}
	if maxScore < 0.3 {
		t.Errorf("故障窗口内最大异常分仅 %.3f,检测可能失效", maxScore)
	}
}

// 基线排序必须是稳定的全序,包含全部服务。
func TestBaselineRankingComplete(t *testing.T) {
	rep := RunOne(BuildPlans()[1])
	order := detect.BaselineRanking(rep.Nodes)
	if len(order) != len(rep.Nodes) {
		t.Fatalf("基线排序长度 %d != 服务数 %d", len(order), len(rep.Nodes))
	}
	seen := map[string]bool{}
	for _, n := range order {
		if seen[n] {
			t.Fatalf("基线排序出现重复服务 %s", n)
		}
		seen[n] = true
	}
}

// RCA 方法必须显著优于纯异常分基线(回归保护,防止算法调参后性能退化)。
func TestRCAOutperformsBaseline(t *testing.T) {
	sum := Run(0)
	if sum.All.Pct1() < 75 {
		t.Errorf("整体 Hit@1 = %.2f%% 低于回归门限 75%%", sum.All.Pct1())
	}
	if sum.All.Pct3() < 90 {
		t.Errorf("整体 Hit@3 = %.2f%% 低于回归门限 90%%", sum.All.Pct3())
	}
	if sum.All.Pct1() <= sum.All.BPct1()+15 {
		t.Errorf("RCA 相对基线提升不足: %.2f%% vs %.2f%%", sum.All.Pct1(), sum.All.BPct1())
	}
	if sum.Single.Pct1() < 90 {
		t.Errorf("单故障 Hit@1 = %.2f%% 低于门限 90%%", sum.Single.Pct1())
	}
	if sum.Double.Pct3() < 85 {
		t.Errorf("双故障 Hit@3 = %.2f%% 低于门限 85%%", sum.Double.Pct3())
	}
}

// 每个场景的排名都必须完整覆盖全部服务,且访问概率归一化在 [0,1]。
func TestRankingCompleteness(t *testing.T) {
	for _, p := range BuildPlans()[:20] {
		rep := RunOne(p)
		if len(rep.Ranking) != len(rep.Nodes) {
			t.Fatalf("场景 %d 排名长度 %d != 服务数 %d", p.ID, len(rep.Ranking), len(rep.Nodes))
		}
		for _, c := range rep.Ranking {
			if c.VisitProb < 0 || c.VisitProb > 1.0001 {
				t.Errorf("场景 %d 服务 %s 访问概率越界 %.4f", p.ID, c.Service, c.VisitProb)
			}
			if c.Origin != "self" && c.Origin != "propagated" && c.Origin != "reverse" {
				t.Errorf("场景 %d 服务 %s 来源标记非法: %s", p.ID, c.Service, c.Origin)
			}
			if len(c.PropagationPath) == 0 {
				t.Errorf("场景 %d 服务 %s 缺少传导路径", p.ID, c.Service)
			}
		}
		// 排名必须严格降序
		for i := 1; i < len(rep.Ranking); i++ {
			if rep.Ranking[i].Suspicion > rep.Ranking[i-1].Suspicion {
				t.Fatalf("场景 %d 排名非降序 @%d", p.ID, i)
			}
		}
	}
}

// 双故障场景: 两个真实根因都应被标记,且 Hit@3 达标率高。
func TestDoubleFaultBothRootsRecovered(t *testing.T) {
	plans := BuildPlans()
	hit3, total := 0, 0
	for _, p := range plans {
		if !p.Double {
			continue
		}
		rep := RunOne(p)
		total++
		if rep.HitAt3 {
			hit3++
		}
		// 两个根因都必须出现在 TrueRoots 且注入标记正确
		injected := map[string]bool{}
		for _, f := range rep.Faults {
			injected[f.Service] = true
		}
		if len(rep.TrueRoots) != 2 {
			t.Errorf("场景 %d TrueRoots 数量 = %d, 期望 2", p.ID, len(rep.TrueRoots))
		}
		for _, c := range rep.Ranking {
			if c.IsInjected != injected[c.Service] {
				t.Errorf("场景 %d 服务 %s 注入标记错误", p.ID, c.Service)
			}
		}
	}
	rate := float64(hit3) * 100 / float64(total)
	if rate < 85 {
		t.Errorf("双故障 Hit@3 = %.2f%% 低于门限 85%%", rate)
	}
}

// 可复现性: 连续两次全量实验的命中率必须完全一致。
func TestDeterministicReproducibility(t *testing.T) {
	s1 := Run(0)
	s2 := Run(0)
	if s1.All.HitAt1 != s2.All.HitAt1 || s1.All.HitAt3 != s2.All.HitAt3 {
		t.Fatal("两次实验命中数不一致,种子可复现性失效")
	}
	if s1.Double.HitAt1 != s2.Double.HitAt1 {
		t.Fatal("双故障结果不可复现")
	}
}
