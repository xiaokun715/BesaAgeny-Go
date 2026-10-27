package gateway

import (
	"crypto/sha256"
	"encoding/binary"
	"math"
	"sort"
)

// Select 把候选变成有序尝试链。策略按配置顺序执行；
// capability 过滤由结构保证 —— 链上的每个候选都满足本次能力。
func Select(candidates []ModelSpec, ctx RoutingContext, strategy []string) ([]ModelSpec, []string) {
	type rejected struct {
		key    string
		reason string
	}
	var pool []ModelSpec
	var skipped []rejected

	// 注册期不可用的模型在选链阶段就被过滤 —— 它们**不算 degraded**。
	for _, c := range candidates {
		if !c.Available() {
			skipped = append(skipped, rejected{c.Key, c.UnavailableReason})
			continue
		}
		pool = append(pool, c)
	}

	if len(strategy) == 0 {
		strategy = []string{"capability", "priority"}
	}
	for _, step := range strategy {
		switch step {
		case "capability":
			kept := pool[:0]
			for _, c := range pool {
				if missing := c.Capabilities.Missing(ctx.Required); len(missing) > 0 {
					names := make([]string, len(missing))
					for i, m := range missing {
						names[i] = string(m)
					}
					skipped = append(skipped, rejected{c.Key, "缺少能力 " + join(names, ",")})
					continue
				}
				kept = append(kept, c)
			}
			pool = kept
		case "priority":
			// 稳定排序：同优先级保留配置顺序。
			sort.SliceStable(pool, func(i, j int) bool { return pool[i].Priority > pool[j].Priority })
		case "weight":
			pool = weightedOrder(pool, ctx.SessionID)
		case "cost":
			sort.SliceStable(pool, func(i, j int) bool {
				a, aok := ctx.CostOf(pool[i].Key)
				b, bok := ctx.CostOf(pool[j].Key)
				// 价格未知排最后，**不当 0**。
				if !aok {
					return false
				}
				if !bok {
					return true
				}
				return a < b
			})
		case "health":
			if ctx.Health != nil {
				rank := func(s CircuitState) int {
					switch s {
					case StateClosed:
						return 0
					case StateHalfOpen:
						return 1
					default:
						return 2
					}
				}
				sort.SliceStable(pool, func(i, j int) bool {
					return rank(ctx.Health.State(pool[i].Key)) < rank(ctx.Health.State(pool[j].Key))
				})
			}
		}
	}

	reasons := make([]string, 0, len(skipped))
	for _, s := range skipped {
		reasons = append(reasons, s.key+": "+s.reason)
	}
	return pool, reasons
}

// weightedOrder 用 Efraimidis–Spirakis 加权采样。
// 同一会话的随机数由 session_id + 模型键派生 —— 同一会话稳定命中。
func weightedOrder(pool []ModelSpec, sessionID string) []ModelSpec {
	type scored struct {
		spec  ModelSpec
		score float64
	}
	items := make([]scored, 0, len(pool))
	for _, c := range pool {
		w := c.Weight
		if w <= 0 {
			w = 1
		}
		sum := sha256.Sum256([]byte(sessionID + "\x00" + c.Key))
		u := (float64(binary.LittleEndian.Uint64(sum[:8])) + 1) / (float64(^uint64(0)) + 1)
		items = append(items, scored{spec: c, score: math.Pow(u, 1/w)})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].score > items[j].score })
	out := make([]ModelSpec, len(items))
	for i, it := range items {
		out[i] = it.spec
	}
	return out
}

func join(items []string, sep string) string {
	out := ""
	for i, item := range items {
		if i > 0 {
			out += sep
		}
		out += item
	}
	return out
}
