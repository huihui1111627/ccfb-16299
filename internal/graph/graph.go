// Package graph 负责从调用记录聚合出服务依赖图,并提供图遍历辅助方法。
package graph

import (
	"sort"

	"rca/internal/model"
)

// Graph 服务依赖有向图。
type Graph struct {
	Nodes []string
	Edges []*model.Edge

	out map[string][]*model.Edge // 下游(我调用谁)
	in  map[string][]*model.Edge // 上游(谁调用我)
}

// Build 从调用记录自动构建依赖图(需求第 1 条)。
// 同一条 caller->callee 边的调用量、延迟、错误率按记录聚合。
func Build(records []model.CallRecord) *Graph {
	type key struct{ caller, callee string }
	agg := map[key]*model.Edge{}
	var tickSet = map[int]struct{}{}
	nodeSet := map[string]struct{}{}

	for _, r := range records {
		nodeSet[r.Caller] = struct{}{}
		nodeSet[r.Callee] = struct{}{}
		tickSet[r.Tick] = struct{}{}

		k := key{r.Caller, r.Callee}
		e, ok := agg[k]
		if !ok {
			e = &model.Edge{Caller: r.Caller, Callee: r.Callee}
			agg[k] = e
		}
		// 延迟/错误率按调用量加权累计,最后再求均值
		w := float64(r.Count)
		e.LatencyMs += r.LatencyMs * w
		e.ErrorRate += float64(r.ErrorCount)
		e.CallCount += r.Count
	}

	ticks := len(tickSet)
	if ticks == 0 {
		ticks = 1
	}
	nodes := make([]string, 0, len(nodeSet))
	for n := range nodeSet {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)

	edges := make([]*model.Edge, 0, len(agg))
	for _, e := range agg {
		if e.CallCount > 0 {
			e.LatencyMs /= float64(e.CallCount)
			e.ErrorRate /= float64(e.CallCount)
		}
		e.Volume = float64(e.CallCount) / float64(ticks)
		edges = append(edges, e)
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].Caller != edges[j].Caller {
			return edges[i].Caller < edges[j].Caller
		}
		return edges[i].Callee < edges[j].Callee
	})

	g := &Graph{
		Nodes: nodes,
		Edges: edges,
		out:   map[string][]*model.Edge{},
		in:    map[string][]*model.Edge{},
	}
	for _, e := range edges {
		g.out[e.Caller] = append(g.out[e.Caller], e)
		g.in[e.Callee] = append(g.in[e.Callee], e)
	}
	return g
}

// Out 返回某服务的所有出边(下游方向)。
func (g *Graph) Out(node string) []*model.Edge { return g.out[node] }

// In 返回某服务的所有入边(上游方向)。
func (g *Graph) In(node string) []*model.Edge { return g.in[node] }

// HasNode 判断节点是否存在。
func (g *Graph) HasNode(n string) bool {
	_, ok := g.out[n]
	if ok {
		return true
	}
	_, ok = g.in[n]
	return ok
}

// EdgeValue 取 caller->callee 边,不存在返回 nil。
func (g *Graph) EdgeValue(caller, callee string) *model.Edge {
	for _, e := range g.out[caller] {
		if e.Callee == callee {
			return e
		}
	}
	return nil
}

// ReachDown 判断 target 是否在 node 的下游(沿调用方向可达,含自身)。
func (g *Graph) ReachDown(node, target string) bool {
	if node == target {
		return true
	}
	visited := map[string]bool{node: true}
	queue := []string{node}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, e := range g.Out(cur) {
			if e.Callee == target {
				return true
			}
			if !visited[e.Callee] {
				visited[e.Callee] = true
				queue = append(queue, e.Callee)
			}
		}
	}
	return false
}

// ShortestPathDown 返回沿调用方向 from->to 的最短路径(节点名序列),不可达返回 nil。
func (g *Graph) ShortestPathDown(from, to string) []string {
	if from == to {
		return []string{from}
	}
	prev := map[string]string{from: ""}
	queue := []string{from}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, e := range g.Out(cur) {
			if _, seen := prev[e.Callee]; seen {
				continue
			}
			prev[e.Callee] = cur
			if e.Callee == to {
				var path []string
				for n := to; n != ""; n = prev[n] {
					path = append([]string{n}, path...)
				}
				return path
			}
			queue = append(queue, e.Callee)
		}
	}
	return nil
}

// UndirectedPath 忽略边方向的最短路径(用于反向传导证据:下游->上游)。
func (g *Graph) UndirectedPath(from, to string) []string {
	if from == to {
		return []string{from}
	}
	neighbors := map[string][]string{}
	for _, e := range g.Edges {
		neighbors[e.Caller] = append(neighbors[e.Caller], e.Callee)
		neighbors[e.Callee] = append(neighbors[e.Callee], e.Caller)
	}
	prev := map[string]string{from: ""}
	queue := []string{from}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, nb := range neighbors[cur] {
			if _, seen := prev[nb]; seen {
				continue
			}
			prev[nb] = cur
			if nb == to {
				var path []string
				for n := to; n != ""; n = prev[n] {
					path = append([]string{n}, path...)
				}
				return path
			}
			queue = append(queue, nb)
		}
	}
	return nil
}
