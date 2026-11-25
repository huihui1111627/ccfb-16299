// 诊断命令: 打印指定场景的检测窗口、异常分、访问概率与排名,
// 直接复用 eval.BuildPlans/RunOne,保证与正式实验逐字节一致。
package main

import (
	"fmt"
	"sort"

	"rca/internal/eval"
	"rca/internal/model"
)

func debugScenario(scenarioID int) {
	plans := eval.BuildPlans()
	if scenarioID < 0 || scenarioID >= len(plans) {
		fmt.Printf("场景 ID 超出范围 [0,%d)\n", len(plans))
		return
	}
	rep := eval.RunOne(plans[scenarioID])
	if rep == nil {
		fmt.Println("分析失败")
		return
	}

	fmt.Printf("场景 %d  入口=%s  真实窗口=%d~%d 检测窗口=%d~%d (偏差 %d)  double=%v\n",
		rep.ScenarioID, rep.Entry, rep.TrueStart, rep.TrueEnd,
		rep.DetectedStart, rep.DetectedEnd, rep.WindowError, rep.Double)
	for _, f := range rep.Faults {
		fmt.Printf("  注入: %s %s reverse=%v severity=%.2f\n", f.Service, f.Kind, f.Reverse, f.Severity)
	}
	for _, cl := range rep.Clusters {
		fmt.Printf("  簇 seed=%s corr=%.2f members=%v\n", cl.Seed, cl.EntryCorr, cl.Members)
	}

	am := map[string]*model.ServiceAnomaly{}
	for _, a := range rep.Nodes {
		am[a.Service] = a
	}
	fmt.Println("\nTop10 排名:")
	for i, c := range rep.Ranking {
		if i >= 10 {
			break
		}
		mark := ""
		for _, r := range rep.TrueRoots {
			if c.Service == r {
				mark = " ★注入"
			}
		}
		fmt.Printf("  %2d %-16s susp=%.4f visit=%.4f anom=%.3f root=%.3f %-10s%s\n",
			c.Rank, c.Service, c.Suspicion, c.VisitProb, c.AnomalyScore, c.RootScore, c.Origin, mark)
	}

	fmt.Println("\n注入服务的详细指标:")
	for _, r := range rep.TrueRoots {
		a := am[r]
		fmt.Printf("  %s score=%.3f firstBreach=%d z: lat=%.1f err=%.1f cpu=%.1f mem=%.1f conn=%.1f\n",
			r, a.Score, a.FirstBreach, a.ZScore["latency"], a.ZScore["error"],
			a.ZScore["cpu"], a.ZScore["mem"], a.ZScore["conn"])
	}

	fmt.Println("\n异常分 Top8 服务(纯检测视角):")
	sorted := append([]*model.ServiceAnomaly{}, rep.Nodes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Score > sorted[j].Score })
	for i, a := range sorted {
		if i >= 8 {
			break
		}
		fmt.Printf("  %-16s score=%.3f first=%d\n", a.Service, a.Score, a.FirstBreach)
	}

	maxV := 0.0
	for _, v := range rep.VisitProb {
		if v > maxV {
			maxV = v
		}
	}
	fmt.Println("\n访问概率 Top8:")
	type vp struct {
		n string
		v float64
	}
	var vps []vp
	for n, v := range rep.VisitProb {
		vps = append(vps, vp{n, v})
	}
	sort.Slice(vps, func(i, j int) bool { return vps[i].v > vps[j].v })
	for i, x := range vps {
		if i >= 8 {
			break
		}
		fmt.Printf("  %-16s visit=%.4f (norm=%.2f) anom=%.3f\n", x.n, x.v, x.v/maxV, am[x.n].Score)
	}
}
