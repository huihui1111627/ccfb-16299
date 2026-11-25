package sim

import (
	"fmt"
	"math"
	"math/rand"
	"sort"

	"rca/internal/model"
)

const (
	// TotalTicks 每个场景模拟 140 个采样点(每点代表 10 秒,约 23 分钟)。
	TotalTicks = 140
	// FaultStart / FaultDuration 故障从第 80 tick 开始,持续 35 tick。
	FaultStart    = 80
	FaultDuration = 35
	// BaselineTicks 故障前用于建立基线的长度。
	BaselineTicks = 60
)

// edgeMeta 边的流量与传导参数(一个场景内固定)。
type edgeMeta struct {
	caller, callee string
	volume         float64 // 每 tick 调用量
	share          float64 // 该调用占 caller 总出站流量的比例
	attenLat       float64 // 延迟向上游传导的逐跳衰减抖动
	attenErr       float64
	attenConn      float64
}

// effect 某服务在单个 tick 上受到的故障增量(指标原始单位)。
type effect struct {
	lat, err, cpu, mem, conn float64
}

// baseline 服务的正常态参数。
type baseline struct {
	latBase, latAmp            float64
	errBase                    float64
	cpuBase, memBase, connBase float64
	cpuAmp, memAmp, connAmp    float64
}

// Simulator 故障场景模拟器。
type Simulator struct {
	rng      *rand.Rand
	services map[string]*model.Service
	order    []string // 拓扑序(上层在前)
	edges    []*edgeMeta
	outIdx   map[string][]*edgeMeta
	inIdx    map[string][]*edgeMeta
	base     map[string]*baseline
}

// New 根据种子构造模拟器并生成正常态拓扑与指标。
func New(seed int64) *Simulator {
	rng := rand.New(rand.NewSource(seed))
	raws := buildTopology(rng)

	s := &Simulator{
		rng:      rng,
		services: map[string]*model.Service{},
		edges:    []*edgeMeta{},
		outIdx:   map[string][]*edgeMeta{},
		inIdx:    map[string][]*edgeMeta{},
		base:     map[string]*baseline{},
	}

	// 建服务
	var all []string
	for ti, tier := range tiers {
		for _, name := range tier {
			svc := &model.Service{
				Name:      name,
				Tier:      tierName[tierLabel(ti)],
				Latency:   make([]float64, TotalTicks),
				ErrorRate: make([]float64, TotalTicks),
				CPU:       make([]float64, TotalTicks),
				Mem:       make([]float64, TotalTicks),
				ConnUsage: make([]float64, TotalTicks),
			}
			svc.Series = map[model.MetricKind][]float64{
				model.MetricLatency: svc.Latency,
				model.MetricError:   svc.ErrorRate,
				model.MetricCPU:     svc.CPU,
				model.MetricMem:     svc.Mem,
				model.MetricConn:    svc.ConnUsage,
			}
			s.services[name] = svc
			all = append(all, name)
		}
	}
	sort.Strings(all)
	s.order = all

	// 边与流量:网关给基础流量,沿拓扑向下按随机 share 分流
	for _, e := range raws {
		em := &edgeMeta{
			caller:    e.caller,
			callee:    e.callee,
			share:     0.25 + rng.Float64()*0.65,
			attenLat:  0.45 + rng.Float64()*0.25,
			attenErr:  0.40 + rng.Float64()*0.25,
			attenConn: 0.50 + rng.Float64()*0.30,
		}
		s.edges = append(s.edges, em)
		s.outIdx[e.caller] = append(s.outIdx[e.caller], em)
		s.inIdx[e.callee] = append(s.inIdx[e.callee], em)
	}
	// 拓扑序计算流量:网关 1000~1400 req/tick,下游 volume = 上游 volume × share 之和
	volume := map[string]float64{}
	for _, gw := range tiers[0] {
		volume[gw] = 1000 + rng.Float64()*400
	}
	for _, name := range s.order {
		// 先汇总入边贡献
		inSum := 0.0
		for _, e := range s.inIdx[name] {
			inSum += volume[e.caller] * e.share
		}
		if _, isGW := volume[name]; !isGW {
			volume[name] = inSum
		}
		for _, e := range s.outIdx[name] {
			e.volume = volume[name] * e.share
		}
	}

	// 正常态基线参数(越往下游延迟越高、资源水位越高)
	for _, name := range all {
		ti := tierIndexOf(name)
		b := &baseline{}
		switch ti {
		case 0:
			b.latBase, b.latAmp = 12+rng.Float64()*8, 4
			b.errBase = 0.002 + rng.Float64()*0.003
			b.cpuBase, b.cpuAmp = 0.28, 0.08
			b.memBase, b.memAmp = 0.35, 0.06
			b.connBase, b.connAmp = 0.30, 0.07
		case 1:
			b.latBase, b.latAmp = 25+rng.Float64()*20, 8
			b.errBase = 0.004 + rng.Float64()*0.006
			b.cpuBase, b.cpuAmp = 0.38, 0.10
			b.memBase, b.memAmp = 0.45, 0.08
			b.connBase, b.connAmp = 0.38, 0.09
		case 2:
			b.latBase, b.latAmp = 40+rng.Float64()*30, 10
			b.errBase = 0.005 + rng.Float64()*0.008
			b.cpuBase, b.cpuAmp = 0.45, 0.12
			b.memBase, b.memAmp = 0.52, 0.10
			b.connBase, b.connAmp = 0.42, 0.10
		default:
			b.latBase, b.latAmp = 18+rng.Float64()*14, 6 // DB 单次调用快,但易受排队影响
			b.errBase = 0.003 + rng.Float64()*0.005
			b.cpuBase, b.cpuAmp = 0.52, 0.12
			b.memBase, b.memAmp = 0.60, 0.10
			b.connBase, b.connAmp = 0.50, 0.12
		}
		s.base[name] = b
	}

	s.generateNormal()
	return s
}

func tierLabel(ti int) string {
	return []string{"edge", "biz", "cap", "data"}[ti]
}

// generateNormal 生成无故障的正常态时间序列(含日周期、AR(1) 噪声、偶发良性抖动)。
func (s *Simulator) generateNormal() {
	for _, name := range s.order {
		svc := s.services[name]
		b := s.base[name]
		// AR(1) 状态
		latN, cpuN, memN, connN, errN := 0.0, 0.0, 0.0, 0.0, 0.0
		for t := 0; t < TotalTicks; t++ {
			phase := float64(t) * 2 * math.Pi / 48 // 半天一个周期
			latN = 0.6*latN + s.rng.NormFloat64()*2.2
			cpuN = 0.65*cpuN + s.rng.NormFloat64()*0.018
			memN = 0.8*memN + s.rng.NormFloat64()*0.010
			connN = 0.6*connN + s.rng.NormFloat64()*0.020
			errN = 0.5*errN + s.rng.NormFloat64()*0.0015

			svc.Latency[t] = clamp(b.latBase+b.latAmp*math.Sin(phase)+latN, 1, 5000)
			svc.CPU[t] = clamp01(b.cpuBase + b.cpuAmp*math.Sin(phase+0.7) + cpuN)
			svc.Mem[t] = clamp01(b.memBase + b.memAmp*math.Sin(phase+1.3) + memN)
			svc.ConnUsage[t] = clamp01(b.connBase + b.connAmp*math.Sin(phase+0.4) + connN)
			svc.ErrorRate[t] = clamp(b.errBase+errN, 0.0001, 1)

			// 偶发良性尖峰(真实系统里到处都是的噪声,考验算法鲁棒性)
			if s.rng.Float64() < 0.006 {
				svc.Latency[t] *= 1.3 + s.rng.Float64()*0.3
			}
			if s.rng.Float64() < 0.004 {
				svc.ErrorRate[t] += 0.01 + s.rng.Float64()*0.01
			}
			if s.rng.Float64() < 0.004 {
				svc.CPU[t] = clamp01(svc.CPU[t] + 0.12)
			}
		}
	}
}

// GenerateScenario 在当前正常态之上注入故障并让异常沿调用链传导。
func (s *Simulator) GenerateScenario(id int, faults []model.InjectedFault) *model.Scenario {
	// 复制正常序列,避免污染后续场景
	svcs := make(map[string]*model.Service, len(s.services))
	for name, src := range s.services {
		cp := &model.Service{
			Name:      src.Name,
			Tier:      src.Tier,
			Latency:   append([]float64{}, src.Latency...),
			ErrorRate: append([]float64{}, src.ErrorRate...),
			CPU:       append([]float64{}, src.CPU...),
			Mem:       append([]float64{}, src.Mem...),
			ConnUsage: append([]float64{}, src.ConnUsage...),
		}
		cp.Series = map[model.MetricKind][]float64{
			model.MetricLatency: cp.Latency,
			model.MetricError:   cp.ErrorRate,
			model.MetricCPU:     cp.CPU,
			model.MetricMem:     cp.Mem,
			model.MetricConn:    cp.ConnUsage,
		}
		svcs[name] = cp
	}

	start, end := faults[0].Start, faults[0].End
	for _, f := range faults[1:] {
		if f.Start < start {
			start = f.Start
		}
		if f.End > end {
			end = f.End
		}
	}

	// 每个故障有 2 tick 爬坡,模拟故障从发生到满载
	for t := start; t < end; t++ {
		effs := map[string]*effect{}
		getEff := func(n string) *effect {
			e, ok := effs[n]
			if !ok {
				e = &effect{}
				effs[n] = e
			}
			return e
		}

		for _, f := range faults {
			if t < f.Start || t >= f.End {
				continue
			}
			ramp := 1.0
			if t == f.Start {
				ramp = 0.6
			}
			// 反向传导场景:根因自身的错误信号延后 1 tick,
			// 让上游连接池饱和导致的错误率先冒头(需求第 6 条)
			rootErrDelay := 0
			if f.Reverse {
				rootErrDelay = 1
			}
			sev := f.Severity * ramp
			root := getEff(f.Service)
			switch f.Kind {
			case model.FaultLatency:
				root.lat += (150 + 150*sev) * sev / f.Severity // 约 +90~+300ms
			case model.FaultError:
				if t-f.Start >= rootErrDelay {
					root.err += 0.22 + 0.25*sev
				}
			case model.FaultResource:
				root.cpu += (0.35 + 0.30*sev) * sev / f.Severity
				root.mem += (0.25 + 0.25*sev) * sev / f.Severity
				root.lat += 60 * sev
			case model.FaultConnPool:
				root.conn += (0.40 + 0.30*sev) * sev / f.Severity
				root.lat += 100 * sev
				if t-f.Start >= rootErrDelay {
					root.err += 0.10 * sev
				}
			}

			// 沿入边反向 BFS 向上游传导(下游故障 -> 上游变慢/报错/连接池占满)
			type hop struct {
				node           string
				lat, err, conn float64
			}
			queue := []hop{{f.Service, root.lat, root.err, root.conn}}
			visited := map[string]bool{f.Service: true}
			for len(queue) > 0 {
				cur := queue[0]
				queue = queue[1:]
				for _, e := range s.inIdx[cur.node] {
					if visited[e.caller] {
						continue
					}
					visited[e.caller] = true
					up := getEff(e.caller)
					latUp := cur.lat * e.share * e.attenLat
					errUp := cur.err * e.share * e.attenErr
					connUp := (cur.lat/320.0 + cur.conn*0.5) * e.share * e.attenConn
					if f.Reverse {
						// 反向传导强化:连接池更快被占满,饱和后直接产生错误
						connUp *= 1.7
					}
					up.lat += latUp
					up.err += errUp
					up.conn += connUp
					queue = append(queue, hop{e.caller, latUp, errUp, connUp})
				}
			}
		}

		// 物理饱和:连接池超过 0.85 后,排队直接转化为超时错误;
		// 反向场景中这让上游错误率与根因同 tick 甚至更早越界。
		// 节点按固定拓扑序处理,保证随机数消耗顺序确定、结果可复现。
		for _, node := range s.order {
			e, ok := effs[node]
			if !ok || e.conn <= 0 {
				continue
			}
			svc := svcs[node]
			projected := svc.ConnUsage[t] + e.conn
			if projected > 0.85 {
				e.err += (projected - 0.85) * 0.9
			}
		}

		// 叠加到序列上,再加观测噪声,并做量纲裁剪
		for _, node := range s.order {
			e, ok := effs[node]
			if !ok {
				continue
			}
			svc := svcs[node]
			svc.Latency[t] = clamp(svc.Latency[t]+e.lat+s.rng.NormFloat64()*2.0, 1, 5000)
			svc.ErrorRate[t] = clamp(svc.ErrorRate[t]+e.err+s.rng.NormFloat64()*0.004, 0.0001, 1)
			svc.CPU[t] = clamp01(svc.CPU[t] + e.cpu + s.rng.NormFloat64()*0.01)
			svc.Mem[t] = clamp01(svc.Mem[t] + e.mem + s.rng.NormFloat64()*0.008)
			svc.ConnUsage[t] = clamp01(svc.ConnUsage[t] + e.conn + s.rng.NormFloat64()*0.01)
		}
	}

	// 依据受影响后的服务指标生成逐 tick 调用记录
	records := make([]model.CallRecord, 0, len(s.edges)*TotalTicks)
	for _, e := range s.edges {
		caller, callee := svcs[e.caller], svcs[e.callee]
		for t := 0; t < TotalTicks; t++ {
			count := int(math.Round(e.volume * (1 + s.rng.NormFloat64()*0.05)))
			if count < 1 {
				count = 1
			}
			lat := callee.Latency[t]*0.75 + caller.Latency[t]*0.25 + s.rng.NormFloat64()*1.5
			errRate := callee.ErrorRate[t]*0.7 + caller.ErrorRate[t]*0.3
			errCnt := int(math.Round(float64(count) * clamp01(errRate)))
			records = append(records, model.CallRecord{
				Tick: t, Caller: e.caller, Callee: e.callee,
				Count: count, LatencyMs: math.Max(1, lat), ErrorCount: errCnt,
			})
		}
	}

	list := make([]*model.Service, 0, len(svcs))
	for _, svc := range svcs {
		list = append(list, svc)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })

	entry := s.pickEntry(faults[0].Service)
	return &model.Scenario{
		ID:         id,
		Seed:       s.rng.Int63(),
		Services:   list,
		Records:    records,
		Faults:     faults,
		Entry:      entry,
		TotalTicks: TotalTicks,
		FaultStart: start,
		FaultEnd:   end,
		Double:     len(faults) > 1,
	}
}

// pickEntry 从能沿调用方向到达根因的网关/最上游服务里随机选一个作为告警入口。
func (s *Simulator) pickEntry(root string) string {
	candidates := []string{}
	for _, gw := range tiers[0] {
		if s.reaches(gw, root) {
			candidates = append(candidates, gw)
		}
	}
	if len(candidates) == 0 {
		// 根因本身就在边缘层时,用它自己
		return root
	}
	return candidates[s.rng.Intn(len(candidates))]
}

func (s *Simulator) reaches(from, to string) bool {
	visited := map[string]bool{from: true}
	queue := []string{from}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, e := range s.outIdx[cur] {
			if e.callee == to {
				return true
			}
			if !visited[e.callee] {
				visited[e.callee] = true
				queue = append(queue, e.callee)
			}
		}
	}
	return false
}

// FaultSpec 生成故障注入参数的便捷封装。
func FaultSpec(svc string, kind model.FaultKind, reverse bool, rng *rand.Rand) model.InjectedFault {
	return model.InjectedFault{
		Service:  svc,
		Kind:     kind,
		Start:    FaultStart,
		End:      FaultStart + FaultDuration,
		Severity: 0.55 + rng.Float64()*0.45,
		Reverse:  reverse,
	}
}

// RandomFaultTarget 随机挑一个非边缘层服务作为故障目标(边缘层也保留少量概率)。
func (s *Simulator) RandomFaultTarget(rng *rand.Rand) string {
	pool := append([]string{}, tiers[1]...)
	pool = append(pool, tiers[2]...)
	pool = append(pool, tiers[3]...)
	if rng.Float64() < 0.1 {
		pool = append(pool, tiers[0]...)
	}
	return pool[rng.Intn(len(pool))]
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clamp01(v float64) float64 { return clamp(v, 0.0001, 0.999) }

// DescribeFault 给注入故障生成中文描述,供页面对照展示。
func DescribeFault(f model.InjectedFault) string {
	kindCN := map[model.FaultKind]string{
		model.FaultLatency:  "延迟飙升",
		model.FaultError:    "错误注入",
		model.FaultResource: "资源耗尽",
		model.FaultConnPool: "连接池打满",
	}[f.Kind]
	rev := ""
	if f.Reverse {
		rev = "(反向传导强化:上游连接池先饱和)"
	}
	return fmt.Sprintf("%s 在 %s 上注入【%s】, 强度 %.0f%%, tick %d~%d %s",
		f.Service, f.Service, kindCN, f.Severity*100, f.Start, f.End, rev)
}
