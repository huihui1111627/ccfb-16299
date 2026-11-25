// rca: 卡丁车场微服务故障根因定位工具
//
// 用法:
//
//	go run ./cmd/rca run                # 跑全部 420 个故障场景并输出命中率对照
//	go run ./cmd/rca run --retain 60    # 保留 60 个场景的详细报告供页面查看(默认 42)
//	go run ./cmd/rca server             # 启动可视化页面(默认 :8080)
//	go run ./cmd/rca server --addr :9000
package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"

	"rca/internal/eval"
	"rca/internal/model"
)

//go:embed all:web
var webFS embed.FS

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		cmdRun(os.Args[2:])
	case "server":
		cmdServer(os.Args[2:])
	case "debug":
		if len(os.Args) < 3 {
			log.Fatal("用法: debug <场景ID>")
		}
		var id int
		fmt.Sscanf(os.Args[2], "%d", &id)
		debugScenario(id)
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: %s\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Println(`rca — 微服务故障根因定位工具

子命令:
  run     生成调用图并注入几百次故障,统计根因命中率,与纯异常分排序对照
  server  启动 Web 页面查看依赖图、随机游走概率、根因排名与注入对照

参数:
  run    [--retain N] [--out file.json]
  server [--addr :8080] [--retain N]`)
}

func cmdRun(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	retain := fs.Int("retain", 42, "保留详细报告的场景数(供 Web 页面查看)")
	out := fs.String("out", "", "把详细报告写入指定 JSON 文件")
	_ = fs.Parse(args)

	sum := eval.Run(*retain)
	printSummary(sum)

	if *out != "" {
		data, err := json.MarshalIndent(struct {
			Summary map[string]any  `json:"summary"`
			Reports []*model.Report `json:"reports"`
		}{summaryMap(sum), sum.Reports}, "", "  ")
		if err != nil {
			log.Fatalf("序列化失败: %v", err)
		}
		if err := os.WriteFile(*out, data, 0o644); err != nil {
			log.Fatalf("写入 %s 失败: %v", *out, err)
		}
		fmt.Printf("\n详细报告已写入 %s\n", *out)
	}

	if len(sum.CheckErrors) > 0 {
		fmt.Printf("\n⚠ 逐条核对发现 %d 条问题:\n", len(sum.CheckErrors))
		for _, e := range sum.CheckErrors {
			fmt.Println("  - " + e)
		}
		os.Exit(1)
	}
	fmt.Println("\n✔ 全部场景与注入记录逐条核对一致")
}

func cmdServer(args []string) {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "监听地址")
	retain := fs.Int("retain", 42, "保留详细报告的场景数")
	_ = fs.Parse(args)

	fmt.Println("正在生成故障场景并执行根因分析(固定种子,可复现)...")
	sum := eval.Run(*retain)
	printSummary(sum)
	if len(sum.CheckErrors) > 0 {
		fmt.Printf("⚠ 核对发现 %d 条问题,请先检查:\n", len(sum.CheckErrors))
		for _, e := range sum.CheckErrors {
			fmt.Println("  - " + e)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/summary", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, summaryMap(sum))
	})
	mux.HandleFunc("GET /api/scenarios", func(w http.ResponseWriter, r *http.Request) {
		// 列表只回传轻量字段,避免首屏过大
		type item struct {
			ID           int                   `json:"id"`
			Double       bool                  `json:"double"`
			Entry        string                `json:"entry"`
			Faults       []model.InjectedFault `json:"faults"`
			TrueRoots    []string              `json:"trueRoots"`
			HitAt1       bool                  `json:"hitAt1"`
			HitAt3       bool                  `json:"hitAt3"`
			BaselineHit  bool                  `json:"baselineHitAt1"`
			BaselineHit3 bool                  `json:"baselineHitAt3"`
			WindowError  int                   `json:"windowError"`
			Top5         []string              `json:"top5"`
		}
		items := make([]item, 0, len(sum.Reports))
		for _, rep := range sum.Reports {
			var top5 []string
			for i, c := range rep.Ranking {
				if i >= 5 {
					break
				}
				top5 = append(top5, c.Service)
			}
			items = append(items, item{
				ID: rep.ScenarioID, Double: rep.Double, Entry: rep.Entry,
				Faults: rep.Faults, TrueRoots: rep.TrueRoots,
				HitAt1: rep.HitAt1, HitAt3: rep.HitAt3,
				BaselineHit: rep.BaselineHit, BaselineHit3: rep.BaselineHit3,
				WindowError: rep.WindowError, Top5: top5,
			})
		}
		writeJSON(w, items)
	})
	mux.HandleFunc("GET /api/scenarios/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		for _, rep := range sum.Reports {
			if fmt.Sprintf("%d", rep.ScenarioID) == id {
				writeJSON(w, rep)
				return
			}
		}
		http.Error(w, "场景不存在(可能超出 --retain 保留范围)", http.StatusNotFound)
	})
	mux.Handle("GET /", http.FileServer(http.FS(subFS())))

	fmt.Printf("\n可视化页面: http://localhost%s\n", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

func subFS() fs.FS {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatal(err)
	}
	return sub
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func summaryMap(s *eval.Summary) map[string]any {
	kindMap := map[string]any{}
	for k, st := range s.ByKind {
		kindMap[string(k)] = statsMap(st)
	}
	checkErrors := s.CheckErrors
	if checkErrors == nil {
		checkErrors = []string{} // 序列化为 [] 而非 null,前端可直接取 length
	}
	return map[string]any{
		"all":         statsMap(&s.All),
		"single":      statsMap(&s.Single),
		"double":      statsMap(&s.Double),
		"reverse":     statsMap(&s.Reverse),
		"byKind":      kindMap,
		"checkErrors": checkErrors,
		"retained":    len(s.Reports),
		"masterSeed":  eval.MasterSeed,
	}
}

func statsMap(st *eval.Stats) map[string]any {
	return map[string]any{
		"count":      st.Count,
		"hitAt1":     st.HitAt1,
		"hitAt3":     st.HitAt3,
		"baseHitAt1": st.BaseHitAt1,
		"baseHitAt3": st.BaseHitAt3,
		"pctAt1":     round2(st.Pct1()),
		"pctAt3":     round2(st.Pct3()),
		"basePctAt1": round2(st.BPct1()),
		"basePctAt3": round2(st.BPct3()),
	}
}

func round2(v float64) float64 { return float64(int(v*100+0.5)) / 100 }
