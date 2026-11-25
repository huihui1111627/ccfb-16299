package rca

import (
	"math"

	"rca/internal/detect"
	"rca/internal/model"
)

// 相关性与游走参数。
const (
	corrThreshold = 0.45 // 达到该相关性才认为与同一故障同源,用于故障簇扩张
	restartProb   = 0.08 // RWR 重启概率(低值让游走质量沿传导链深入下游根因)
	stayProb      = 0.08 // 停在原地的概率
	eps           = 1e-9
)

// corrSeries 保存每个服务相对基线的偏差序列(已平滑),供相关性计算复用。
type corrSeries struct {
	lat, err, cpu, mem, conn []float64
}

// buildSeries 取 [lo, hi) 区间,把各指标转成相对基线的标准化偏差并做轻平滑。
func buildSeries(sc *model.Scenario, lo, hi, baseStart, baseEnd int) map[string]*corrSeries {
	out := map[string]*corrSeries{}
	for _, svc := range sc.Services {
		cs := &corrSeries{}
		cs.lat = deviation(svc.Series[model.MetricLatency], lo, hi, baseStart, baseEnd)
		cs.err = deviation(svc.Series[model.MetricError], lo, hi, baseStart, baseEnd)
		cs.cpu = deviation(svc.Series[model.MetricCPU], lo, hi, baseStart, baseEnd)
		cs.mem = deviation(svc.Series[model.MetricMem], lo, hi, baseStart, baseEnd)
		cs.conn = deviation(svc.Series[model.MetricConn], lo, hi, baseStart, baseEnd)
		out[svc.Name] = cs
	}
	return out
}

func deviation(xs []float64, lo, hi, baseStart, baseEnd int) []float64 {
	st := detect.StatsOf(xs[baseStart:baseEnd])
	std := math.Max(st.Std, 1e-6)
	raw := make([]float64, hi-lo)
	for i, t := 0, lo; t < hi; i, t = i+1, t+1 {
		raw[i] = (xs[t] - st.Mean) / std
	}
	// 指数平滑 alpha=0.5,抑制采样噪声对相关性的干扰
	sm := make([]float64, len(raw))
	prev := 0.0
	for i, v := range raw {
		prev = 0.5*v + 0.5*prev
		sm[i] = prev
	}
	return sm
}

// pearson 皮尔逊相关系数;方差不足时返回 0。
func pearson(a, b []float64) float64 {
	n := len(a)
	if n != len(b) || n < 3 {
		return 0
	}
	sa, sb := 0.0, 0.0
	for i := 0; i < n; i++ {
		sa += a[i]
		sb += b[i]
	}
	ma, mb := sa/float64(n), sb/float64(n)
	cov, va, vb := 0.0, 0.0, 0.0
	for i := 0; i < n; i++ {
		da, db := a[i]-ma, b[i]-mb
		cov += da * db
		va += da * da
		vb += db * db
	}
	if va < eps || vb < eps {
		return 0
	}
	r := cov / math.Sqrt(va*vb)
	if math.IsNaN(r) {
		return 0
	}
	return r
}

// correlation 计算服务 svc 与入口服务的故障同源相关性。
// 延迟/错误权重最高;同一节点直接返回 1。
func correlation(all map[string]*corrSeries, entry, svc string) float64 {
	if entry == svc {
		return 1
	}
	e, ok1 := all[entry]
	s, ok2 := all[svc]
	if !ok1 || !ok2 {
		return 0
	}
	rLat := pearson(e.lat, s.lat)
	rErr := pearson(e.err, s.err)
	rCPU := pearson(e.cpu, s.cpu)
	rMem := pearson(e.mem, s.mem)
	rConn := pearson(e.conn, s.conn)
	// 取延迟与错误中的较强者为主,资源指标做补充,加 0.05 先验避免整体偏低
	r := 0.42*math.Max(rLat, 0) + 0.42*math.Max(rErr, 0) +
		0.06*math.Max(rCPU, 0) + 0.05*math.Max(rMem, 0) + 0.05*math.Max(rConn, 0)
	return math.Min(1, r+0.05)
}

// edgeCorrelation 一条边上两服务的逐指标相关性,用于证据展示。
func edgeCorrelation(all map[string]*corrSeries, a, b string) map[model.MetricKind]float64 {
	x, y := all[a], all[b]
	if x == nil || y == nil {
		return nil
	}
	return map[model.MetricKind]float64{
		model.MetricLatency: pearson(x.lat, y.lat),
		model.MetricError:   pearson(x.err, y.err),
		model.MetricCPU:     pearson(x.cpu, y.cpu),
		model.MetricConn:    pearson(x.conn, y.conn),
	}
}
