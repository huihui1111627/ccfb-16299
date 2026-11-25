package main

import (
	"fmt"

	"rca/internal/eval"
	"rca/internal/model"
)

func printSummary(s *eval.Summary) {
	line := func(title string, st *eval.Stats) {
		if st.Count == 0 {
			fmt.Printf("%-18s 样本 %3d  (无样本)\n", title, st.Count)
			return
		}
		fmt.Printf("%-18s 样本 %3d | RCA: Hit@1 %6.2f%%  Hit@3 %6.2f%% | 纯异常分: Hit@1 %6.2f%%  Hit@3 %6.2f%% | 提升 +%.2f/+%.2f pp\n",
			title, st.Count, st.Pct1(), st.Pct3(), st.BPct1(), st.BPct3(),
			st.Pct1()-st.BPct1(), st.Pct3()-st.BPct3())
	}
	fmt.Println("==================== 故障根因定位实验结果(固定种子,可复现) ====================")
	line("全部场景", &s.All)
	line("单故障", &s.Single)
	line("双独立故障", &s.Double)
	line("含反向传导", &s.Reverse)
	fmt.Println("-------------------------------- 按故障类型 --------------------------------")
	order := []struct {
		k    string
		name string
	}{
		{"latency_spike", "延迟飙升"},
		{"error_injection", "错误注入"},
		{"resource_exhaustion", "资源耗尽"},
		{"conn_pool_stall", "连接池打满"},
	}
	for _, o := range order {
		line(o.name, s.ByKind[model.FaultKind(o.k)])
	}
	fmt.Println("==============================================================================")
}
