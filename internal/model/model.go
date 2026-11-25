// Package model 定义整个根因定位工具共用的数据结构。
package model

// MetricKind 指标类型。
type MetricKind string

const (
	MetricLatency MetricKind = "latency" // 平均延迟(ms)
	MetricError   MetricKind = "error"   // 错误率(0~1)
	MetricCPU     MetricKind = "cpu"     // CPU 使用率(0~1)
	MetricMem     MetricKind = "mem"     // 内存使用率(0~1)
	MetricConn    MetricKind = "conn"    // 连接池使用率(0~1)
)

// AllMetrics 参与异常检测与相关性计算的全部指标。
var AllMetrics = []MetricKind{MetricLatency, MetricError, MetricCPU, MetricMem, MetricConn}

// MetricWeight 各指标在综合异常分数中的权重:
// 延迟/错误是面向请求的直接故障信号,资源指标更偏诱因,权重略低。
var MetricWeight = map[MetricKind]float64{
	MetricLatency: 0.30,
	MetricError:   0.30,
	MetricCPU:     0.15,
	MetricMem:     0.15,
	MetricConn:    0.10,
}

// CallRecord 一次服务间调用的采样记录(模拟 RPC 框架/trace 系统落库的调用明细)。
// 依赖图就是从这些记录聚合出来的。
type CallRecord struct {
	Tick       int     // 采样时刻(每个 tick 代表 10 秒)
	Caller     string  // 调用方服务
	Callee     string  // 被调用方服务
	Count      int     // 该窗口内调用次数
	LatencyMs  float64 // 平均延迟
	ErrorCount int     // 错误调用次数
}

// Edge 依赖图中的一条有向边: Caller -> Callee。
type Edge struct {
	Caller    string
	Callee    string
	CallCount int     // 统计周期内的累计调用量
	Volume    float64 // 平均每个 tick 的调用量,用于随机游走的先验
	LatencyMs float64 // 平均延迟
	ErrorRate float64 // 平均错误率
}

// Service 服务节点及其指标时间序列。
// 每条序列长度等于模拟的总 tick 数。
type Service struct {
	Name      string
	Tier      string
	Latency   []float64 // ms
	ErrorRate []float64 // 0~1
	CPU       []float64 // 0~1
	Mem       []float64 // 0~1
	ConnUsage []float64 // 0~1
	Series    map[MetricKind][]float64
}

// FaultKind 注入的故障类型。
type FaultKind string

const (
	FaultLatency  FaultKind = "latency_spike"       // 延迟飙升
	FaultError    FaultKind = "error_injection"     // 错误注入
	FaultResource FaultKind = "resource_exhaustion" // 资源耗尽(CPU/内存)
	FaultConnPool FaultKind = "conn_pool_stall"     // 连接池打满(模拟下游变慢传导)
)

// InjectedFault 一次注入的故障(注入记录,即"标准答案")。
type InjectedFault struct {
	Service  string
	Kind     FaultKind
	Start    int     // 故障开始 tick
	End      int     // 故障结束 tick
	Severity float64 // 强度 0~1
	// Reverse 为 true 时强化"反向传导":下游变慢先占满上游连接池,
	// 让上游错误率先于/强于根因自身报警,专门用于检验需求第 6 条。
	Reverse bool
}

// Scenario 一次完整的故障场景:注入记录 + 模拟出来的全量观测数据。
type Scenario struct {
	ID         int
	Seed       int64
	Services   []*Service
	Records    []CallRecord
	Faults     []InjectedFault // 单故障长度 1,双故障长度 2
	Entry      string          // 告警入口服务(随机游走起点)
	TotalTicks int
	FaultStart int // 实际故障开始 tick(取最早)
	FaultEnd   int
	Double     bool
}

// ServiceAnomaly 单个服务在故障窗口内的异常检测结果。
type ServiceAnomaly struct {
	Service     string
	Tier        string
	Score       float64                // 综合异常分数 0~1
	MetricScore map[MetricKind]float64 // 各指标异常分数
	ZScore      map[MetricKind]float64 // 各指标窗口均值相对基线的 z 值
	Baseline    map[MetricKind]float64 // 基线均值
	WindowMean  map[MetricKind]float64 // 故障窗口均值
	FirstBreach int                    // 首个指标越界 tick(用于判断谁先异常)
	Breaches    map[MetricKind]int     // 各指标首次越界 tick
}

// MetricEvidence 一条指标级证据。
type MetricEvidence struct {
	Metric MetricKind
	Z      float64
	Score  float64
	Detail string
}

// RankedCandidate 根因排名中的一个候选服务。
type RankedCandidate struct {
	Rank            int
	Service         string
	Tier            string
	Suspicion       float64 // 嫌疑分数 = 稳定访问概率 × 自身异常程度(含来源修正)
	VisitProb       float64 // 随机游走稳定访问概率
	AnomalyScore    float64 // 异常检测分数
	RootScore       float64 // 来源修正后的异常程度
	Origin          string  // self(自身首发) / propagated(下游传导) / reverse(反向传导)
	MetricEvidence  []MetricEvidence
	PropagationPath []string // 从入口到该服务的传导路径
	PathMetrics     []string // 与路径逐跳对应的相关性说明
	IsInjected      bool     // 是否为真实注入服务(评估时用)
}

// ClusterEvidence 一个故障簇的检测信息(双故障场景会有两个簇)。
type ClusterEvidence struct {
	Seed      string
	Members   []string
	EntryCorr float64 // 该簇种子与入口指标的相关性
}

// Report 一次场景的完整根因分析报告,同时也是 Web API 的 JSON 载荷。
type Report struct {
	ScenarioID    int
	Seed          int64
	Entry         string
	TotalTicks    int
	DetectedStart int // 检测到的故障窗口起点
	DetectedEnd   int
	BaselineStart int
	BaselineEnd   int
	TrueStart     int
	TrueEnd       int
	WindowError   int // 检测窗口与真实窗口的起点偏差(tick)
	Double        bool

	Nodes     []*ServiceAnomaly
	Edges     []Edge
	VisitProb map[string]float64
	Ranking   []*RankedCandidate
	Clusters  []ClusterEvidence

	Faults []InjectedFault
	// 逐条核对结果
	TrueRoots    []string
	HitAt1       bool
	HitAt3       bool
	BaselineHit  bool // 纯异常分排序是否命中第一(对照实验)
	BaselineHit3 bool // 纯异常分排序是否命中前三(对照实验)
	Notes        []string
}
