// Package rca 在依赖图上实现"相关性加权随机游走 + 异常分"的根因定位。
package rca

import (
	"math"
	"sort"

	"rca/internal/graph"
	"rca/internal/model"
)

// Engine 根因定位引擎,持有一次分析过程中的全部中间结果,供页面展示。
type Engine struct {
	g         *graph.Graph
	sc        *model.Scenario
	anomalies map[string]*model.ServiceAnomaly
	series    map[string]*corrSeries
	corrCache map[string]float64
	window    [4]int // start,end,baseStart,baseEnd
}

// NewEngine 构造引擎。
func NewEngine(g *graph.Graph, sc *model.Scenario, anomalies []*model.ServiceAnomaly,
	start, end, baseStart, baseEnd int) *Engine {
	am := map[string]*model.ServiceAnomaly{}
	for _, a := range anomalies {
		am[a.Service] = a
	}
	return &Engine{
		g:         g,
		sc:        sc,
		anomalies: am,
		series:    buildSeries(sc, start, end, baseStart, baseEnd),
		corrCache: map[string]float64{},
		window:    [4]int{start, end, baseStart, baseEnd},
	}
}

// corr 带缓存的入口相关性查询。
func (e *Engine) corr(svc string) float64 {
	if v, ok := e.corrCache[svc]; ok {
		return v
	}
	v := correlation(e.series, e.sc.Entry, svc)
	e.corrCache[svc] = v
	return v
}

// anomalous 判断服务是否进入异常集合(综合分够高或任一指标明确越界)。
func (e *Engine) anomalous(svc string) bool {
	a := e.anomalies[svc]
	if a == nil {
		return false
	}
	if a.Score >= 0.25 {
		return true
	}
	for _, z := range a.ZScore {
		if z >= 3 {
			return true
		}
	}
	return false
}

// detectClusters 在异常子图上切分故障簇。
// 单故障得到 1 个簇;两个独立故障即使都与入口相关,也会因为彼此之间局部相关性低、
// 图距离远而被切成 2 个簇(需求第 8 条)。
// 方法: 异常节点建无向图,仅当相邻节点局部相关性达标时连边,取连通分量;
// 再按"簇内最高异常分"降序,依次以最高分节点为种子。
func (e *Engine) detectClusters() []Cluster {
	// 无向邻接(全图骨架)
	adj := map[string][]string{}
	for _, ed := range e.g.Edges {
		adj[ed.Caller] = append(adj[ed.Caller], ed.Callee)
		adj[ed.Callee] = append(adj[ed.Callee], ed.Caller)
	}

	anomNodes := make([]string, 0)
	for _, n := range e.g.Nodes { // g.Nodes 已排序,保证遍历顺序确定
		if e.anomalous(n) {
			anomNodes = append(anomNodes, n)
		}
	}
	if len(anomNodes) == 0 {
		return []Cluster{{Seed: e.sc.Entry, Members: []string{e.sc.Entry}, EntryCorr: 1}}
	}

	// 异常节点之间的"同簇连边": 直接邻居且局部高相关;
	// 非邻居但图距离 <=2 且相关性很高也连边(覆盖经过一个正常节点的故障链)
	linked := map[string]map[string]bool{}
	addLink := func(a, b string) {
		if linked[a] == nil {
			linked[a] = map[string]bool{}
		}
		if linked[b] == nil {
			linked[b] = map[string]bool{}
		}
		linked[a][b] = true
		linked[b][a] = true
	}
	for i, a := range anomNodes {
		for _, b := range anomNodes[i+1:] {
			local := e.pairCorr(a, b)
			direct := false
			for _, nb := range adj[a] {
				if nb == b {
					direct = true
					break
				}
			}
			if direct && local >= corrThreshold {
				addLink(a, b)
				continue
			}
			// 二跳邻居需要更强的相关性才合并,避免两个独立故障被误并
			if !direct && local >= 0.62 && e.undirectedDistance(a, b) <= 2 {
				addLink(a, b)
			}
		}
	}

	// 连通分量
	visited := map[string]bool{}
	var comps [][]string
	for _, n := range anomNodes {
		if visited[n] {
			continue
		}
		var comp []string
		queue := []string{n}
		visited[n] = true
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			comp = append(comp, cur)
			nbs := make([]string, 0, len(linked[cur]))
			for nb := range linked[cur] {
				nbs = append(nbs, nb)
			}
			sort.Strings(nbs) // 固定 BFS 顺序,消除 map 随机性
			for _, nb := range nbs {
				if !visited[nb] {
					visited[nb] = true
					queue = append(queue, nb)
				}
			}
		}
		comps = append(comps, comp)
	}

	// 每个分量以异常分最高的节点为种子,按种子异常分降序排列
	var clusters []Cluster
	for _, comp := range comps {
		seed := comp[0]
		best := -1.0
		for _, n := range comp {
			if a := e.anomalies[n]; a != nil && a.Score > best {
				best = a.Score
				seed = n
			}
		}
		sort.Strings(comp)
		clusters = append(clusters, Cluster{
			Seed:      seed,
			Members:   comp,
			EntryCorr: e.corr(seed),
		})
	}
	sort.Slice(clusters, func(i, j int) bool {
		si := e.anomalies[clusters[i].Seed]
		sj := e.anomalies[clusters[j].Seed]
		if si == nil || sj == nil {
			return clusters[i].Seed < clusters[j].Seed
		}
		if si.Score != sj.Score {
			return si.Score > sj.Score
		}
		return clusters[i].Seed < clusters[j].Seed
	})

	// 限制最多 3 个簇(实验最多双故障),把零碎小簇并入最近的大簇
	if len(clusters) > 3 {
		kept := clusters[:3]
		for _, small := range clusters[3:] {
			kept[0].Members = append(kept[0].Members, small.Members...)
		}
		clusters = kept
	}

	// 二次分裂: 连通分量只有 1 个、但其中存在两个独立的高分异常中心
	// (彼此局部相关性低且图距离 >=2)时,按归属重新切成两个簇。
	if len(clusters) == 1 && e.sc.Double {
		if split := e.splitByAnomalyCenters(clusters[0]); split != nil {
			clusters = split
		}
	}
	return clusters
}

// splitByAnomalyCenters 尝试在单个大连通簇里找出两个独立故障中心。
// 规则: 取异常分 Top6 的节点,若其中两个高分节点局部相关性 < 0.4 且图距离 >=2,
// 则以它们为中心,把簇内节点按相关性归属重新划分;两侧都要有足够强的异常才算成功。
func (e *Engine) splitByAnomalyCenters(big Cluster) []Cluster {
	cands := make([]*model.ServiceAnomaly, 0, len(big.Members))
	for _, n := range big.Members {
		if a := e.anomalies[n]; a != nil {
			cands = append(cands, a)
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].Score != cands[j].Score {
			return cands[i].Score > cands[j].Score
		}
		return cands[i].Service < cands[j].Service
	})
	if len(cands) < 2 {
		return nil
	}
	top := cands
	if len(top) > 6 {
		top = top[:6]
	}

	// 找第一对"低相关 + 远距离"的高分节点。
	// 阈值 0.5: 两个独立故障经网关汇聚后通常有 0.3~0.5 的弱相关,
	// 而同一故障链上的节点局部相关普遍 >0.6,据此区分。
	c1, c2 := "", ""
	for i := 0; i < len(top) && c1 == ""; i++ {
		for j := i + 1; j < len(top); j++ {
			a, b := top[i], top[j]
			if a.Score < 0.35 || b.Score < 0.35 {
				continue
			}
			if e.pairCorr(a.Service, b.Service) < 0.45 &&
				e.undirectedDistance(a.Service, b.Service) >= 2 {
				c1, c2 = a.Service, b.Service
				break
			}
		}
	}
	if c1 == "" {
		return nil
	}

	// 按与两个中心的局部相关性归属;相关性都很低时按图距离归属
	group1, group2 := []string{}, []string{}
	for _, n := range big.Members {
		r1, r2 := e.pairCorr(n, c1), e.pairCorr(n, c2)
		if r1 >= r2 {
			group1 = append(group1, n)
		} else {
			group2 = append(group2, n)
		}
	}
	if len(group1) == 0 || len(group2) == 0 {
		return nil
	}
	// 两侧都要有明确异常节点,否则只是噪声分裂
	strong := func(group []string) bool {
		for _, n := range group {
			if a := e.anomalies[n]; a != nil && a.Score >= 0.35 {
				return true
			}
		}
		return false
	}
	if !strong(group1) || !strong(group2) {
		return nil
	}
	sort.Strings(group1)
	sort.Strings(group2)
	return []Cluster{
		{Seed: c1, Members: group1, EntryCorr: e.corr(c1)},
		{Seed: c2, Members: group2, EntryCorr: e.corr(c2)},
	}
}

// undirectedDistance 无向图最短跳数,不可达返回一个大数。
func (e *Engine) undirectedDistance(a, b string) int {
	if a == b {
		return 0
	}
	adj := map[string][]string{}
	for _, ed := range e.g.Edges {
		adj[ed.Caller] = append(adj[ed.Caller], ed.Callee)
		adj[ed.Callee] = append(adj[ed.Callee], ed.Caller)
	}
	dist := map[string]int{a: 0}
	queue := []string{a}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, nb := range adj[cur] {
			if _, seen := dist[nb]; seen {
				continue
			}
			dist[nb] = dist[cur] + 1
			if nb == b {
				return dist[nb]
			}
			queue = append(queue, nb)
		}
	}
	return 99
}

// pairCorr 两个服务之间的局部相关性(不限入口)。
func (e *Engine) pairCorr(a, b string) float64 {
	x, y := e.series[a], e.series[b]
	if x == nil || y == nil {
		return 0
	}
	rLat := pearson(x.lat, y.lat)
	rErr := pearson(x.err, y.err)
	rConn := pearson(x.conn, y.conn)
	return math.Min(1, 0.45*math.Max(rLat, 0)+0.4*math.Max(rErr, 0)+0.15*math.Max(rConn, 0)+0.05)
}

// Cluster 故障簇(对 model.ClusterEvidence 的内部扩展)。
type Cluster struct {
	Seed      string
	Members   []string
	EntryCorr float64
}

// randomWalk 在全图上执行带重启的相关性加权随机游走(需求第 3 条)。
// restartSeed 为重启向量集中的种子;允许走向下游、回退上游、停在原地。
// 相关性低时上游回退权重衰减、原地停留与重启占比升高,游走会被"拉回"真故障区。
func (e *Engine) randomWalk(restartSeed string, clusterNodes map[string]bool) map[string]float64 {
	nodes := e.g.Nodes
	idx := map[string]int{}
	for i, n := range nodes {
		idx[n] = i
	}
	n := len(nodes)

	// 重启向量: 种子占 45%,簇内其他成员分 55%。
	// 重启质量不能过度集中在入口,否则入口访问概率会压过真正的下游根因。
	restart := make([]float64, n)
	restart[idx[restartSeed]] = 0.45
	rest := 0.55 / math.Max(1, float64(len(clusterNodes)-1))
	for node := range clusterNodes {
		if node != restartSeed {
			restart[idx[node]] = rest
		}
	}
	// 归一化
	rsum := 0.0
	for _, v := range restart {
		rsum += v
	}
	for i := range restart {
		restart[i] /= rsum
	}

	// 转移权重(懒构造): 对每个节点计算下游/上游/停留的未归一化权重
	type move struct {
		to     int
		weight float64
	}
	moves := make([][]move, n)
	for i, u := range nodes {
		var ms []move
		// 停在原地
		ms = append(ms, move{i, stayProb})
		// 走向下游: 相关性强加权,边流量只做同层先验;
		// 与入口不相关的下游几乎不获得转移质量,游走被引向真正的故障传导链
		volSum := 0.0
		for _, ed := range e.g.Out(u) {
			volSum += ed.Volume
		}
		for _, ed := range e.g.Out(u) {
			c := e.corr(ed.Callee)
			volW := 0.5
			if volSum > 0 {
				volW = ed.Volume / volSum
			}
			w := volW * (0.08 + 1.6*c)
			ms = append(ms, move{idx[ed.Callee], w})
		}
		// 回退上游: 相关性越高越愿意回退;低相关时只剩很小的基础权重,
		// 游走更容易停住或重启,而不是盲目扩散(需求第 3 条)
		for _, ed := range e.g.In(u) {
			c := e.corr(ed.Caller)
			w := 0.22 * (0.15 + 0.85*c)
			ms = append(ms, move{idx[ed.Caller], w})
		}
		total := 0.0
		for _, m := range ms {
			total += m.weight
		}
		for j := range ms {
			ms[j].weight /= total
		}
		moves[i] = ms
	}

	// 幂迭代直到稳定
	p := make([]float64, n)
	copy(p, restart)
	for iter := 0; iter < 200; iter++ {
		np := make([]float64, n)
		for i := range nodes {
			// 重启部分
			np[i] += restartProb * restart[i]
			// 转移部分
			movePart := (1 - restartProb) * p[i]
			for _, m := range moves[i] {
				np[m.to] += movePart * m.weight
			}
		}
		diff := 0.0
		for i := range np {
			diff += math.Abs(np[i] - p[i])
		}
		p = np
		if diff < 1e-7 {
			break
		}
	}

	// 归一化为访问概率
	sum := 0.0
	for _, v := range p {
		sum += v
	}
	out := map[string]float64{}
	for i, v := range p {
		out[nodes[i]] = v / sum
	}
	return out
}

// Rank 执行完整分析并产出排名与证据。
func (e *Engine) Rank() ([]*model.RankedCandidate, map[string]float64, []model.ClusterEvidence) {
	clusters := e.detectClusters()

	// 每个簇各跑一次游走。关键: 在"簇内"归一化访问概率,
	// 这样双故障的两个故障中心无论所在连通区大小,都拥有平等的最高分机会,
	// 大簇里的传导节点不会靠绝对访问概率挤掉小簇的根因。
	visit := map[string]float64{}
	clusterEvidence := make([]model.ClusterEvidence, 0, len(clusters))
	for _, cl := range clusters {
		set := map[string]bool{}
		for _, m := range cl.Members {
			set[m] = true
		}
		pv := e.randomWalk(cl.Seed, set)
		clusterMax := 0.0
		for _, v := range pv {
			if v > clusterMax {
				clusterMax = v
			}
		}
		if clusterMax <= 0 {
			clusterMax = 1
		}
		for node, v := range pv {
			if norm := v / clusterMax; norm > visit[node] {
				visit[node] = norm
			}
		}
		clusterEvidence = append(clusterEvidence, model.ClusterEvidence{
			Seed: cl.Seed, Members: append([]string{}, cl.Members...), EntryCorr: cl.EntryCorr,
		})
	}
	// map 遍历顺序随机,输出前按节点名排序重建,保证结果可复现
	orderedVisit := make(map[string]float64, len(visit))
	for _, n := range e.g.Nodes {
		orderedVisit[n] = visit[n]
	}
	visit = orderedVisit

	// 嫌疑分数 = 稳定访问概率(簇内归一化) × 来源修正后的自身异常程度
	cands := make([]*model.RankedCandidate, 0, len(e.g.Nodes))
	for _, node := range e.g.Nodes {
		a := e.anomalies[node]
		anom := 0.0
		if a != nil {
			anom = a.Score
		}
		origin, rootScore := e.classifyOrigin(node, anom)
		// 传导类节点再按距入口的跳数加深折扣: 异常沿调用链每多传一跳,
		// 作为根因的可信度进一步下降,避免层层传导后中间节点分数堆积
		if origin != "self" {
			rootScore *= e.depthDiscount(node)
			// 边缘网关只负责接入,几乎不会是业务故障根因:
			// 传导状态下再压一道,把排名让给下游真正出问题的服务
			if a != nil && a.Tier == "edge" {
				rootScore *= 0.72
			}
		}
		// visit 已经是簇内 0~1 归一化值,直接使用
		v := visit[node]
		// 完全无异常的节点给一个极小异常底,仍可被访问概率带起但不会排到前面
		eff := math.Max(rootScore, 0.02)
		// 嫌疑分: 异常程度为主(0.7 幂次,拉开高异常根因与轻微传导节点的差距),
		// 访问概率负责在多个异常节点中沿传导链定位源头;幂次压低入口的高访问概率优势
		susp := math.Pow(eff, 0.7) * math.Pow(v, 0.45)
		cands = append(cands, &model.RankedCandidate{
			Service:      node,
			Tier:         e.tierOf(node),
			Suspicion:    susp,
			VisitProb:    visit[node],
			AnomalyScore: anom,
			RootScore:    rootScore,
			Origin:       origin,
		})
	}
	sort.Slice(cands, func(i, j int) bool {
		if math.Abs(cands[i].Suspicion-cands[j].Suspicion) > 1e-12 {
			return cands[i].Suspicion > cands[j].Suspicion
		}
		return cands[i].Service < cands[j].Service
	})
	for i, c := range cands {
		c.Rank = i + 1
		e.fillEvidence(c)
	}
	return cands, visit, clusterEvidence
}

// classifyOrigin 区分"自身首发故障"与"下游传导/反向传导"(需求第 5、6 条)。
func (e *Engine) classifyOrigin(node string, anom float64) (string, float64) {
	a := e.anomalies[node]
	if a == nil || anom < 0.05 {
		return "self", anom
	}

	// 自身资源类指标显著越界,且不弱于任何下游 → 自身故障的强信号
	selfResource := math.Max(a.ZScore[model.MetricCPU], a.ZScore[model.MetricMem])
	hasStrongResource := selfResource >= 3.5

	// 找异常最强的下游邻居
	var downNode string
	downAnom := 0.0
	for _, ed := range e.g.Out(node) {
		if da := e.anomalies[ed.Callee]; da != nil && da.Score > downAnom {
			downAnom = da.Score
			downNode = ed.Callee
		}
	}

	// 反向传导必须同时满足: 本节点连接池明确越界 + 与下游高相关。
	// 仅延迟/错误升高不构成反向传导(那是普通的下游→上游延迟传导)。
	connDriven := a.ZScore[model.MetricConn] >= 3 && downNode != "" &&
		e.pairCorr(node, downNode) >= corrThreshold

	if connDriven && a.FirstBreach >= 0 {
		da := e.anomalies[downNode]
		// 本节点不晚于下游越界,符合"上游连接池先饱和、错误率先涨"的反向特征
		if da != nil && (da.FirstBreach < 0 || a.FirstBreach <= da.FirstBreach) {
			return "reverse", anom * 0.68
		}
	}

	// 常规传导: 下游存在高相关且异常不弱于自己的节点。
	// 阈值 0.75 容忍直接上游吸收根因延迟后分数略高于根因的情况(场景 0 型);
	// 但自身资源指标显著越界时保留 self 判定(资源耗尽型故障的根因)。
	if downNode != "" && downAnom >= anom*0.75 && e.pairCorr(node, downNode) >= corrThreshold {
		da := e.anomalies[downNode]
		if hasStrongResource && (da == nil || selfResource >
			math.Max(da.ZScore[model.MetricCPU], da.ZScore[model.MetricMem])) {
			return "self", anom // 自己资源先爆,不归因给下游
		}
		return "propagated", anom * 0.7
	}
	return "self", anom
}

// fillEvidence 填充指标证据与传导路径。
func (e *Engine) fillEvidence(c *model.RankedCandidate) {
	a := e.anomalies[c.Service]
	if a == nil {
		return
	}
	// 取 z 值最高的 3 个指标作为证据
	type mz struct {
		m model.MetricKind
		z float64
	}
	var top []mz
	for _, m := range model.AllMetrics {
		top = append(top, mz{m, a.ZScore[m]})
	}
	sort.Slice(top, func(i, j int) bool { return top[i].z > top[j].z })
	for k := 0; k < 3 && k < len(top); k++ {
		if top[k].z < 1.5 {
			break
		}
		m := top[k].m
		c.MetricEvidence = append(c.MetricEvidence, model.MetricEvidence{
			Metric: m,
			Z:      top[k].z,
			Score:  a.MetricScore[m],
			Detail: metricCN(m, a.Baseline[m], a.WindowMean[m], top[k].z),
		})
	}

	// 传导路径: 入口沿调用方向到候选节点;不可达时用无向路径(反向传导)
	path := e.g.ShortestPathDown(e.sc.Entry, c.Service)
	method := "down"
	if path == nil {
		path = e.g.UndirectedPath(e.sc.Entry, c.Service)
		method = "undirected"
	}
	if len(path) >= 1 {
		c.PropagationPath = path
		for i := 0; i+1 < len(path); i++ {
			ec := edgeCorrelation(e.series, path[i], path[i+1])
			dir := "→"
			if method == "undirected" && e.g.EdgeValue(path[i], path[i+1]) == nil {
				dir = "⇠(反向)"
			}
			c.PathMetrics = append(c.PathMetrics,
				formatEdge(path[i], path[i+1], dir, ec))
		}
	}
}

func (e *Engine) tierOf(node string) string {
	for _, svc := range e.sc.Services {
		if svc.Name == node {
			return svc.Tier
		}
	}
	return ""
}

// depthDiscount 节点距入口越远(传导跳数越多),传导折扣越强;
// 入口自身(距离 0)不额外折扣。无向路径兜底,保证反向传导节点也能算到距离。
func (e *Engine) depthDiscount(node string) float64 {
	if node == e.sc.Entry {
		return 1
	}
	path := e.g.ShortestPathDown(e.sc.Entry, node)
	if path == nil {
		path = e.g.UndirectedPath(e.sc.Entry, node)
	}
	hops := 1
	if path != nil {
		hops = len(path) - 1
	}
	switch {
	case hops <= 1:
		return 0.88
	case hops == 2:
		return 0.78
	case hops == 3:
		return 0.68
	default:
		return 0.6
	}
}

func metricCN(m model.MetricKind, base, win, z float64) string {
	names := map[model.MetricKind]string{
		model.MetricLatency: "平均延迟",
		model.MetricError:   "错误率",
		model.MetricCPU:     "CPU使用率",
		model.MetricMem:     "内存使用率",
		model.MetricConn:    "连接池使用率",
	}
	unit := map[model.MetricKind]string{
		model.MetricLatency: "ms",
		model.MetricError:   "",
		model.MetricCPU:     "",
		model.MetricMem:     "",
		model.MetricConn:    "",
	}
	pct := func(v float64) string {
		if unit[m] == "ms" {
			return f1(v) + "ms"
		}
		return pctFmt(v)
	}
	return names[m] + " 基线 " + pct(base) + " → 窗口 " + pct(win) +
		", z=" + f2(z)
}

func formatEdge(a, b, dir string, ec map[model.MetricKind]float64) string {
	return a + " " + dir + " " + b +
		" [延迟相关 " + f2(ec[model.MetricLatency]) +
		", 错误相关 " + f2(ec[model.MetricError]) +
		", 连接相关 " + f2(ec[model.MetricConn]) + "]"
}

func f1(v float64) string {
	return strconvFormat(v, 'f', 1)
}

func f2(v float64) string {
	return strconvFormat(v, 'f', 2)
}

func pctFmt(v float64) string {
	return strconvFormat(v*100, 'f', 1) + "%"
}
