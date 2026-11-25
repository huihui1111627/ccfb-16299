// Package detect 对服务指标做基于基线的异常检测,并确定故障时间窗口。
package detect

import (
	"math"

	"rca/internal/model"
	"rca/internal/sim"
)

// 越界阈值: 与基线均值相差 3 个标准差即视为异常(噪声尖峰一般 < 3)。
const breachZ = 3.0

// 窗口检测用更灵敏的 2.5σ 阈值并要求多次命中,兼顾灵敏度与抗噪。
const detectZ = 2.5

// Stats 一段时序的均值与标准差。
type Stats struct {
	Mean, Std float64
}

// StatsOf 导出给 rca 包复用的时序统计。
func StatsOf(xs []float64) Stats { return statsOf(xs) }

func statsOf(xs []float64) Stats {
	n := float64(len(xs))
	if n == 0 {
		return Stats{}
	}
	sum := 0.0
	for _, x := range xs {
		sum += x
	}
	mean := sum / n
	ss := 0.0
	for _, x := range xs {
		d := x - mean
		ss += d * d
	}
	std := math.Sqrt(ss / n)
	return Stats{Mean: mean, Std: std}
}

// DetectWindow 检测故障时间窗口。
// 策略: 先在全图找最早的多点越界时刻(根因自身指标最早最强,不依赖传导到入口的
// 弱信号),再要求入口服务在其后 6 tick 内确认;入口完全无反应时信任全图扫描结果。
// 基线固定取故障前的平稳区间 [5, 70)。
func DetectWindow(sc *model.Scenario) (start, end, baseStart, baseEnd int) {
	baseStart = 5
	baseEnd = sim.FaultStart - 10
	entry := sc.Services[0]
	for _, svc := range sc.Services {
		if svc.Name == sc.Entry {
			entry = svc
			break
		}
	}

	// 全图扫描: 每个服务各自的检测起点,取最早者
	globalStart := math.MaxInt
	for _, svc := range sc.Services {
		if t := detectStart(svc, baseStart, baseEnd); t >= 0 && t < globalStart {
			globalStart = t
		}
	}
	if globalStart == math.MaxInt {
		globalStart = sim.FaultStart // 理论上不会发生,兜底
	}
	entryStart := detectStart(entry, baseStart, baseEnd)
	switch {
	case entryStart < 0:
		start = globalStart // 入口未报警,信任全图
	case entryStart-globalStart > 6:
		start = globalStart // 入口反应太慢(传导衰减),以根因侧时间为准
	default:
		start = min(entryStart, globalStart)
	}

	// 窗口结束: 从 start 之后找连续 5 tick 全部回归
	end = -1
	quiet := 0
	for t := start; t < sc.TotalTicks; t++ {
		if anyBreach(entry, t, baseStart, baseEnd) {
			quiet = 0
		} else {
			quiet++
			if quiet >= 5 {
				end = t - 4
				break
			}
		}
	}
	if end < 0 || end <= start {
		end = min(start+sim.FaultDuration, sc.TotalTicks)
	}
	return start, end, baseStart, baseEnd
}

func detectStart(svc *model.Service, baseStart, baseEnd int) int {
	// 4 个 tick 窗口内出现 2 次越界即判定故障开始(容忍单点抖动,又能快速触发)
	const winLen, need = 4, 2
	hits := 0
	// 不早于注入前 6 tick 报警: 基线区偶发噪声尖峰不应被当成故障起点
	earliest := sim.FaultStart - 6
	for t := baseEnd + 1; t < len(svc.Latency); t++ {
		if t < earliest {
			continue
		}
		if t >= baseEnd+1+winLen {
			old := t - winLen
			if old >= earliest && anyBreach(svc, old, baseStart, baseEnd) {
				hits--
			}
		}
		if anyBreach(svc, t, baseStart, baseEnd) {
			hits++
			if hits >= need {
				// 回溯到窗口内第一次越界的 tick 作为起点
				for k := t - winLen + 1; k <= t; k++ {
					if k >= earliest && anyBreach(svc, k, baseStart, baseEnd) {
						return k
					}
				}
				return t
			}
		}
	}
	return -1
}

func anyBreach(svc *model.Service, t, baseStart, baseEnd int) bool {
	return breachAt(svc, model.MetricLatency, t, baseStart, baseEnd, detectZ) ||
		breachAt(svc, model.MetricError, t, baseStart, baseEnd, detectZ) ||
		breachAt(svc, model.MetricConn, t, baseStart, baseEnd, detectZ)
}

func breach(svc *model.Service, m model.MetricKind, t, baseStart, baseEnd int) bool {
	return breachAt(svc, m, t, baseStart, baseEnd, breachZ)
}

func breachAt(svc *model.Service, m model.MetricKind, t, baseStart, baseEnd int, zth float64) bool {
	xs := svc.Series[m][baseStart:baseEnd]
	st := statsOf(xs)
	floor := floorStd(m, st.Mean)
	std := math.Max(st.Std, floor)
	return math.Abs(svc.Series[m][t]-st.Mean)/std > zth
}

// floorStd 给低基数指标(错误率、利用率)的标准差设下限,
// 避免基线过于平稳时 z 值爆炸(比如错误率基线 std 接近 0)。
func floorStd(m model.MetricKind, mean float64) float64 {
	switch m {
	case model.MetricError:
		return math.Max(0.004, mean*0.5)
	case model.MetricCPU, model.MetricMem, model.MetricConn:
		return 0.03
	default:
		return math.Max(1.5, mean*0.08)
	}
}

// Analyze 对全部服务做异常检测,输出每个服务的综合异常分数与逐指标证据。
func Analyze(sc *model.Scenario, start, end, baseStart, baseEnd int) []*model.ServiceAnomaly {
	out := make([]*model.ServiceAnomaly, 0, len(sc.Services))
	for _, svc := range sc.Services {
		a := &model.ServiceAnomaly{
			Service:     svc.Name,
			Tier:        svc.Tier,
			MetricScore: map[model.MetricKind]float64{},
			ZScore:      map[model.MetricKind]float64{},
			Baseline:    map[model.MetricKind]float64{},
			WindowMean:  map[model.MetricKind]float64{},
			Breaches:    map[model.MetricKind]int{},
		}

		total := 0.0
		maxScore := 0.0
		first := math.MaxInt
		for _, m := range model.AllMetrics {
			base := statsOf(svc.Series[m][baseStart:baseEnd])
			win := statsOf(svc.Series[m][start:end])
			std := math.Max(base.Std, floorStd(m, base.Mean))
			// 单尾: 只关心指标升高的故障(本模拟器的故障全部表现为升高)
			z := (win.Mean - base.Mean) / std
			z = math.Max(0, z)
			score := sigmoid(z - breachZ) // z=3 时约 0.5,z=5 时约 0.88,z>7 接近 1
			a.MetricScore[m] = score
			a.ZScore[m] = z
			a.Baseline[m] = base.Mean
			a.WindowMean[m] = win.Mean
			// 加权均值体现"多指标同时异常",max 项保证单指标极端故障
			// (如根因延迟 z=60)不会被固定权重压到 0.3,排名反低于轻微异常的上游
			total += model.MetricWeight[m] * score
			if score > maxScore {
				maxScore = score
			}

			// 该指标在整个观测期内的首次越界 tick(窗口起点之后才算,
			// 基线期的偶发噪声尖峰不算故障越界)
			bt := firstBreachAfter(svc, m, start)
			if bt >= 0 {
				a.Breaches[m] = bt
				if bt < first {
					first = bt
				}
			}
		}
		// 45% 加权均值 + 55% 最强单指标: 单指标打爆的根因(延迟 z=60)能拿高分,
		// 多指标轻度异常的传导服务不会靠指标数量堆出更高分
		a.Score = 0.45*total + 0.55*maxScore
		if first == math.MaxInt {
			a.FirstBreach = -1
		} else {
			a.FirstBreach = first
		}
		out = append(out, a)
	}
	return out
}

// firstBreachAfter 只统计检测窗口起点之后的越界,排除基线期噪声尖峰的干扰。
func firstBreachAfter(svc *model.Service, m model.MetricKind, fromTick int) int {
	// breach 内部自带基线参数,这里用 fromTick-65/fromTick-5 作为基线区间,
	// 与主分析保持同一基线(调用方保证 fromTick 即检测起点)
	baseStart, baseEnd := 5, fromTick-10
	if baseEnd <= baseStart {
		baseStart, baseEnd = 0, fromTick-1
	}
	for t := fromTick; t < len(svc.Series[m]); t++ {
		if breach(svc, m, t, baseStart, baseEnd) {
			return t
		}
	}
	return -1
}

// BaselineRanking 对照组:只按异常分数排序(不做随机游走)。
func BaselineRanking(anomalies []*model.ServiceAnomaly) []string {
	type pair struct {
		name  string
		score float64
	}
	pairs := make([]pair, len(anomalies))
	for i, a := range anomalies {
		pairs[i] = pair{a.Service, a.Score}
	}
	// 稳定排序,固定次序保证可复现
	for i := 1; i < len(pairs); i++ {
		for j := i; j > 0 && pairs[j].score > pairs[j-1].score; j-- {
			pairs[j], pairs[j-1] = pairs[j-1], pairs[j]
		}
	}
	out := make([]string, len(pairs))
	for i, p := range pairs {
		out[i] = p.name
	}
	return out
}

func sigmoid(x float64) float64 { return 1 / (1 + math.Exp(-x)) }
