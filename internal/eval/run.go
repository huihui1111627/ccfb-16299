// Package eval 批量生成故障场景、跑根因定位、统计命中率并与基线方法对照。
package eval

import (
	"fmt"
	"math/rand"
	"sort"

	"rca/internal/detect"
	"rca/internal/graph"
	"rca/internal/model"
	"rca/internal/rca"
	"rca/internal/sim"
)

// MasterSeed 全部实验的固定总种子,保证结果可复现。
const MasterSeed int64 = 20261126

// 场景规模: 单故障 300 个(四类各 75),双故障 120 个,共 420 个。
const (
	SingleScenarios = 300
	DoubleScenarios = 120
)

// Stats 一组场景的命中率统计。
type Stats struct {
	Count        int
	HitAt1       int
	HitAt3       int
	BaseHitAt1   int
	BaseHitAt3   int
	WindowErrors []int
}

func (s *Stats) Add(hit1, hit3, base1, base3 bool, windowErr int) {
	s.Count++
	if hit1 {
		s.HitAt1++
	}
	if hit3 {
		s.HitAt3++
	}
	if base1 {
		s.BaseHitAt1++
	}
	if base3 {
		s.BaseHitAt3++
	}
	s.WindowErrors = append(s.WindowErrors, windowErr)
}

func (s Stats) Pct1() float64  { return pct(s.HitAt1, s.Count) }
func (s Stats) Pct3() float64  { return pct(s.HitAt3, s.Count) }
func (s Stats) BPct1() float64 { return pct(s.BaseHitAt1, s.Count) }
func (s Stats) BPct3() float64 { return pct(s.BaseHitAt3, s.Count) }

func pct(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) * 100 / float64(b)
}

// Summary 全部实验的汇总结果。
type Summary struct {
	All         Stats
	Single      Stats
	Double      Stats
	Reverse     Stats // 含反向传导标记的场景
	ByKind      map[model.FaultKind]*Stats
	Reports     []*model.Report
	CheckErrors []string // 逐条核对中发现的不一致(应为空)
}

// Plan 单个场景的故障计划(固定生成,保证可复现)。
type Plan struct {
	ID      int
	Double  bool
	Faults  []model.FaultKind
	Reverse []bool
	seed    int64
}

// BuildPlans 用固定总种子生成全部 420 个场景的计划,供 Run 与调试命令共用。
func BuildPlans() []Plan {
	rootRNG := rand.New(rand.NewSource(MasterSeed))
	var plans []Plan
	id := 0
	kinds := []model.FaultKind{
		model.FaultLatency, model.FaultError, model.FaultResource, model.FaultConnPool,
	}
	// 单故障: 四类轮转各 75 个
	for i := 0; i < SingleScenarios; i++ {
		k := kinds[i%len(kinds)]
		reverse := false
		// 延迟飙升/连接池类故障更常触发反向传导,约 35%
		if (k == model.FaultLatency || k == model.FaultConnPool) && rootRNG.Float64() < 0.45 {
			reverse = true
		} else if rootRNG.Float64() < 0.15 {
			reverse = true
		}
		plans = append(plans, Plan{ID: id, Double: false, Faults: []model.FaultKind{k}, Reverse: []bool{reverse}})
		id++
	}
	// 双故障: 随机两类组合,120 个
	for i := 0; i < DoubleScenarios; i++ {
		k1 := kinds[rootRNG.Intn(len(kinds))]
		k2 := kinds[rootRNG.Intn(len(kinds))]
		r1 := rootRNG.Float64() < 0.35
		r2 := rootRNG.Float64() < 0.35
		plans = append(plans, Plan{ID: id, Double: true, Faults: []model.FaultKind{k1, k2}, Reverse: []bool{r1, r2}})
		id++
	}
	// 场景种子紧跟计划生成,顺序与 Run 主循环保持一致
	for i := range plans {
		plans[i].seed = rootRNG.Int63()
	}
	return plans
}

// Run 执行完整实验。retainReports 控制保留多少份详细报告供页面查看。
func Run(retainReports int) *Summary {
	plans := BuildPlans()
	sum := &Summary{ByKind: map[model.FaultKind]*Stats{}}
	for _, k := range []model.FaultKind{
		model.FaultLatency, model.FaultError, model.FaultResource, model.FaultConnPool,
	} {
		sum.ByKind[k] = &Stats{}
	}

	for _, p := range plans {
		rep := RunOne(p)
		if rep == nil {
			sum.CheckErrors = append(sum.CheckErrors, fmt.Sprintf("场景 %d 分析失败", p.ID))
			continue
		}

		// 逐条核对: 报告记录的真实根因必须与注入记录完全一致
		injected := map[string]bool{}
		for _, f := range rep.Faults {
			injected[f.Service] = true
		}
		for _, r := range rep.TrueRoots {
			if !injected[r] {
				sum.CheckErrors = append(sum.CheckErrors,
					fmt.Sprintf("场景 %d: TrueRoots 含未注入服务 %s", p.ID, r))
			}
		}
		for svc := range injected {
			found := false
			for _, r := range rep.TrueRoots {
				if r == svc {
					found = true
				}
			}
			if !found {
				sum.CheckErrors = append(sum.CheckErrors,
					fmt.Sprintf("场景 %d: 注入服务 %s 未进入 TrueRoots", p.ID, svc))
			}
		}
		// 排名中标记的注入标记必须与注入记录一致
		for _, c := range rep.Ranking {
			if c.IsInjected != injected[c.Service] {
				sum.CheckErrors = append(sum.CheckErrors,
					fmt.Sprintf("场景 %d: %s 注入标记错误", p.ID, c.Service))
			}
		}
		// 窗口检测偏差超过 8 tick 视为检测异常,记录但不算致命错误
		if abs(rep.WindowError) > 8 {
			sum.CheckErrors = append(sum.CheckErrors,
				fmt.Sprintf("场景 %d: 窗口偏差过大 %d tick", p.ID, rep.WindowError))
		}

		sum.All.Add(rep.HitAt1, rep.HitAt3, rep.BaselineHit, rep.BaselineHit3, rep.WindowError)
		if p.Double {
			sum.Double.Add(rep.HitAt1, rep.HitAt3, rep.BaselineHit, rep.BaselineHit3, rep.WindowError)
		} else {
			sum.Single.Add(rep.HitAt1, rep.HitAt3, rep.BaselineHit, rep.BaselineHit3, rep.WindowError)
			sum.ByKind[p.Faults[0]].Add(rep.HitAt1, rep.HitAt3, rep.BaselineHit, rep.BaselineHit3, rep.WindowError)
		}
		if hasReverse(rep.Faults) {
			sum.Reverse.Add(rep.HitAt1, rep.HitAt3, rep.BaselineHit, rep.BaselineHit3, rep.WindowError)
		}

		if p.ID < retainReports {
			sum.Reports = append(sum.Reports, rep)
		}
	}
	return sum
}

// RunOne 执行单个场景的完整分析流水线(模拟 → 建图 → 检测 → 游走 → 排名 → 核对)。
func RunOne(p Plan) *model.Report {
	id := p.ID
	seed := p.seed
	simulator := sim.New(seed)
	rng := rand.New(rand.NewSource(seed ^ 0x5DEECE66D))

	var faults []model.InjectedFault
	used := map[string]bool{}
	for i, k := range p.Faults {
		target := simulator.RandomFaultTarget(rng)
		for used[target] {
			target = simulator.RandomFaultTarget(rng)
		}
		used[target] = true
		faults = append(faults, sim.FaultSpec(target, k, p.Reverse[i], rng))
	}
	sc := simulator.GenerateScenario(id, faults)

	// 需求第 1 条: 依赖图从调用记录自动构建
	g := graph.Build(sc.Records)
	if len(g.Edges) == 0 || len(g.Nodes) != len(sc.Services) {
		return nil
	}

	start, end, baseStart, baseEnd := detect.DetectWindow(sc)
	anomalies := detect.Analyze(sc, start, end, baseStart, baseEnd)
	engine := rca.NewEngine(g, sc, anomalies, start, end, baseStart, baseEnd)
	ranking, visit, clusters := engine.Rank()

	trueRoots := make([]string, 0, len(faults))
	injected := map[string]bool{}
	for _, f := range faults {
		trueRoots = append(trueRoots, f.Service)
		injected[f.Service] = true
	}
	for _, c := range ranking {
		c.IsInjected = injected[c.Service]
	}

	// 命中判定
	need := len(trueRoots)
	top1 := topN(ranking, min(need, len(ranking)))
	top3 := topN(ranking, min(need+2, len(ranking))) // 双故障取前 3,单故障也取前 3
	hit1 := containsAll(top1, trueRoots)
	hit3 := containsAll(top3, trueRoots)

	// 基线对照: 纯异常分排序
	baseOrder := detect.BaselineRanking(anomalies)
	baseTop1 := baseOrder[:min(need, len(baseOrder))]
	baseHit1 := containsAll(baseTop1, trueRoots)
	baseHit3 := containsAll(baseOrder[:min(need+2, len(baseOrder))], trueRoots)

	notes := buildNotes(p, faults, start, sc.FaultStart, hit1, hit3, baseHit1, baseHit3)

	return &model.Report{
		ScenarioID:    id,
		Seed:          seed,
		Entry:         sc.Entry,
		TotalTicks:    sc.TotalTicks,
		DetectedStart: start,
		DetectedEnd:   end,
		BaselineStart: baseStart,
		BaselineEnd:   baseEnd,
		TrueStart:     sc.FaultStart,
		TrueEnd:       sc.FaultEnd,
		WindowError:   start - sc.FaultStart,
		Double:        p.Double,
		Nodes:         anomalies,
		Edges:         edgeValues(g),
		VisitProb:     visit,
		Ranking:       ranking,
		Clusters:      clusters,
		Faults:        faults,
		TrueRoots:     trueRoots,
		HitAt1:        hit1,
		HitAt3:        hit3,
		BaselineHit:   baseHit1,
		BaselineHit3:  baseHit3,
		Notes:         notes,
	}
}

func buildNotes(p Plan, faults []model.InjectedFault, detected, truth int, h1, h3, b1, b3 bool) []string {
	var notes []string
	for _, f := range faults {
		notes = append(notes, "注入: "+sim.DescribeFault(f))
	}
	notes = append(notes, fmt.Sprintf("检测窗口起点 tick %d,真实起点 tick %d,偏差 %d tick",
		detected, truth, detected-truth))
	notes = append(notes, fmt.Sprintf("RCA 命中: Hit@1=%v Hit@3=%v; 纯异常分对照: Hit@1=%v Hit@3=%v",
		h1, h3, b1, b3))
	if p.Double {
		notes = append(notes, "双独立故障场景: 两个注入服务需同时进入前列才算命中")
	}
	return notes
}

func edgeValues(g *graph.Graph) []model.Edge {
	out := make([]model.Edge, 0, len(g.Edges))
	for _, e := range g.Edges {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Caller != out[j].Caller {
			return out[i].Caller < out[j].Caller
		}
		return out[i].Callee < out[j].Callee
	})
	return out
}

func topN(ranking []*model.RankedCandidate, n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n && i < len(ranking); i++ {
		out = append(out, ranking[i].Service)
	}
	return out
}

func containsAll(top, roots []string) bool {
	set := map[string]bool{}
	for _, t := range top {
		set[t] = true
	}
	for _, r := range roots {
		if !set[r] {
			return false
		}
	}
	return true
}

func hasReverse(faults []model.InjectedFault) bool {
	for _, f := range faults {
		if f.Reverse {
			return true
		}
	}
	return false
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
