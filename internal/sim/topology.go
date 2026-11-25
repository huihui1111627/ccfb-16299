package sim

import (
	"math/rand"
	"sort"
)

// 服务分层:边缘网关 -> 业务服务 -> 能力服务 -> 数据/中间件。
// 场景设定为卡丁车场的计时、预约、会员、支付、直播等几十个服务。
var tiers = [][]string{
	{
		"api-gateway", "ws-gateway",
	},
	{
		"auth-svc", "user-svc", "booking-svc", "timing-svc", "member-svc",
		"payment-svc", "live-svc", "order-svc", "notify-svc", "kart-svc",
	},
	{
		"track-svc", "kart-fleet", "lap-engine", "score-svc", "pricing-svc",
		"coupon-svc", "wallet-svc", "refund-svc", "chat-svc", "stream-pusher",
	},
	{
		"mysql-cluster", "redis-cache", "mq-broker", "object-store", "cdn-edge",
		"config-center", "trace-collector", "timeseries-db", "search-engine", "id-generator",
	},
}

var tierName = map[string]string{
	"edge": "edge", "biz": "biz", "cap": "cap", "data": "data",
}

func tierIndexOf(svc string) int {
	for i, tier := range tiers {
		for _, s := range tier {
			if s == svc {
				return i
			}
		}
	}
	return -1
}

// rawEdge 仅描述调用关系骨架,调用量在拓扑确定后按流量传播计算。
type rawEdge struct{ caller, callee string }

// buildTopology 用固定随机源生成 32 个服务的调用图骨架(有向无环,跨层调用)。
func buildTopology(rng *rand.Rand) []rawEdge {
	edgeSet := map[rawEdge]bool{}
	add := func(a, b string) {
		if a == b || tierIndexOf(a) >= tierIndexOf(b) {
			return // 只允许从上层指向下层,保证 DAG
		}
		edgeSet[rawEdge{a, b}] = true
	}

	gateways := append([]string{}, tiers[0]...)
	biz := append([]string{}, tiers[1]...)
	capList := append([]string{}, tiers[2]...)
	data := append([]string{}, tiers[3]...)

	// 1) 网关 -> 业务: 每条边以 0.75 概率存在,保证每个业务至少被一个网关调用
	for _, gw := range gateways {
		for _, b := range biz {
			if rng.Float64() < 0.75 {
				add(gw, b)
			}
		}
	}
	for _, b := range biz {
		if !hasCaller(edgeSet, b) {
			add(gateways[rng.Intn(len(gateways))], b)
		}
	}

	// 2) 业务 -> 能力: 每个业务挂 2~4 个能力服务
	for _, b := range biz {
		k := 2 + rng.Intn(3)
		perm := rng.Perm(len(capList))
		for i := 0; i < k; i++ {
			add(b, capList[perm[i]])
		}
	}

	// 3) 能力 -> 数据: 每个能力挂 1~3 个数据/中间件
	for _, c := range capList {
		k := 1 + rng.Intn(3)
		perm := rng.Perm(len(data))
		for i := 0; i < k; i++ {
			add(c, data[perm[i]])
		}
	}

	// 4) 业务域内的横向依赖(仍落在同层时被 add 拒绝,因此指向能力层的具体服务),
	//    用几条贴近业务语义的固定边保证结构稳定可复现。
	add("booking-svc", "timing-svc") // 同层,会被拒绝;真正的横向依赖通过共享能力体现
	add("payment-svc", "order-svc")  // 同上
	for _, fixed := range []rawEdge{
		{"booking-svc", "pricing-svc"},
		{"order-svc", "pricing-svc"},
		{"payment-svc", "wallet-svc"},
		{"refund-svc", "wallet-svc"},
		{"live-svc", "stream-pusher"},
		{"chat-svc", "stream-pusher"},
		{"timing-svc", "lap-engine"},
		{"kart-svc", "kart-fleet"},
		{"member-svc", "coupon-svc"},
		{"notify-svc", "mq-broker"},
	} {
		add(fixed.caller, fixed.callee)
	}

	// 5) 少量跨层捷径(网关直连能力、业务直连数据),让图不完全规整
	for _, gw := range gateways {
		if rng.Float64() < 0.5 {
			add(gw, capList[rng.Intn(len(capList))])
		}
	}
	for _, b := range biz {
		if rng.Float64() < 0.3 {
			add(b, data[rng.Intn(len(data))])
		}
	}

	// 6) 保证每个数据节点至少被一个能力/业务使用,避免孤立节点
	for _, d := range data {
		if !hasCaller(edgeSet, d) {
			add(capList[rng.Intn(len(capList))], d)
		}
	}
	for _, c := range capList {
		if !hasCaller(edgeSet, c) {
			add(biz[rng.Intn(len(biz))], c)
		}
	}

	out := make([]rawEdge, 0, len(edgeSet))
	for e := range edgeSet {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].caller != out[j].caller {
			return out[i].caller < out[j].caller
		}
		return out[i].callee < out[j].callee
	})
	return out
}

func hasCaller(set map[rawEdge]bool, callee string) bool {
	for e := range set {
		if e.callee == callee {
			return true
		}
	}
	return false
}
