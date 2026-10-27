package workflow

import (
	"fmt"
	"sort"
)

type ValidationContext struct {
	Pool             *Pool
	Allowed          []string
	ResourceCapacity map[string]int
	TaskInputs       map[string]FieldSpec
	Assets           []string
}

// Validate 是纯函数、fail-closed：任一 issue → 一个节点都不跑。
// 检查顺序：依赖闭合 → 无环 → IO 匹配 → 权限 → 资源。
func Validate(w Workflow, vc ValidationContext) []GraphIssue {
	var issues []GraphIssue
	nodeIDs := map[string]bool{}
	for _, n := range w.Nodes {
		nodeIDs[n.ID] = true
	}

	// ① 依赖闭合：每个节点的必需输入都必须有来源。
	for _, n := range w.Nodes {
		spec, err := vc.Pool.Get(n.Agent)
		if err != nil {
			issues = append(issues, GraphIssue{Kind: IssueDependency, NodeID: n.ID, Detail: err.Error(), Suggestion: "检查 node.agent 是否在 Agent Pool 中注册"})
			continue
		}
		for field, fs := range spec.Capability.Inputs {
			if !fs.Required {
				continue
			}
			provided := false
			if _, ok := n.Inputs[field]; ok {
				provided = true
			}
			if _, ok := vc.TaskInputs[field]; ok {
				provided = true
			}
			for _, e := range w.Upstream(n.ID) {
				if e.Kind == EdgeData && (e.Field == field || e.Field == "") {
					provided = true
				}
			}
			if !provided {
				issues = append(issues, GraphIssue{Kind: IssueDependency, NodeID: n.ID,
					Detail:     fmt.Sprintf("节点 %s 的必需输入 %q 没有来源", n.ID, field),
					Suggestion: "补一条数据依赖边，或把该字段从必需改为可选"})
			}
		}
		for _, src := range n.Inputs {
			if nodeIDs[src] {
				continue
			}
			// 非节点 id 视为输入资产。Assets==nil 表示只做结构校验（Validate RPC）。
			if vc.Assets != nil && !contains(vc.Assets, src) {
				issues = append(issues, GraphIssue{Kind: IssueDependency, NodeID: n.ID,
					Detail:     fmt.Sprintf("输入 %q 既不是节点也不是已声明资产", src),
					Suggestion: "在任务输入资产里声明它，或改成一个上游节点 id"})
			}
		}
	}

	// ② 无环（Kahn）。
	indeg := map[string]int{}
	adj := map[string][]string{}
	for _, n := range w.Nodes {
		indeg[n.ID] = 0
	}
	for _, e := range w.Edges {
		if e.Kind != EdgeData {
			continue
		}
		adj[e.Src] = append(adj[e.Src], e.Dst)
		indeg[e.Dst]++
	}
	queue := []string{}
	for id, d := range indeg {
		if d == 0 {
			queue = append(queue, id)
		}
	}
	sort.Strings(queue)
	visited := 0
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		visited++
		for _, next := range adj[cur] {
			indeg[next]--
			if indeg[next] == 0 {
				queue = append(queue, next)
			}
		}
	}
	if visited != len(w.Nodes) {
		var cyclic []string
		for id, d := range indeg {
			if d > 0 {
				cyclic = append(cyclic, id)
			}
		}
		sort.Strings(cyclic)
		issues = append(issues, GraphIssue{Kind: IssueCycle, Detail: "检测到环，涉及节点：" + fmt.Sprint(cyclic),
			Suggestion: "去掉形成环的那条数据依赖；流程必须能拓扑排序"})
	}

	// ③ IO 匹配。
	for _, e := range w.Edges {
		if e.Kind != EdgeData || e.Field == "" {
			continue
		}
		srcNode := w.Node(e.Src)
		dstNode := w.Node(e.Dst)
		if srcNode == nil || dstNode == nil {
			issues = append(issues, GraphIssue{Kind: IssueDependency, Detail: "边引用了不存在的节点：" + e.Src + " → " + e.Dst})
			continue
		}
		srcSpec, err1 := vc.Pool.Get(srcNode.Agent)
		dstSpec, err2 := vc.Pool.Get(dstNode.Agent)
		if err1 != nil || err2 != nil {
			continue
		}
		out, okOut := srcSpec.Capability.Outputs[e.Field]
		in, okIn := dstSpec.Capability.Inputs[e.Field]
		if !okOut || !okIn {
			issues = append(issues, GraphIssue{Kind: IssueIOMismatch, NodeID: e.Dst,
				Detail:     fmt.Sprintf("字段 %q 在 %s → %s 上未能同时找到输出与输入", e.Field, e.Src, e.Dst),
				Suggestion: "检查上下游的能力元数据中的 inputs/outputs"})
			continue
		}
		if out.Type != "" && in.Type != "" && out.Type != in.Type {
			issues = append(issues, GraphIssue{Kind: IssueIOMismatch, NodeID: e.Dst,
				Detail:     fmt.Sprintf("字段 %q 类型不匹配：%s 输出 %s，%s 需要 %s", e.Field, e.Src, out.Type, e.Dst, in.Type),
				Suggestion: "在中间加一个转换节点，或统一字段类型"})
		}
	}

	// ④ 权限。
	for _, n := range w.Nodes {
		if len(vc.Allowed) > 0 && !contains(vc.Allowed, n.Agent) {
			issues = append(issues, GraphIssue{Kind: IssuePermission, NodeID: n.ID,
				Detail: "本次运行未授权使用 Agent " + n.Agent, Suggestion: "从流程中移除该节点，或在任务权限里放行"})
		}
	}

	// ⑤ 资源冲突：容量为 1 的独占资源必须有一条串行链。
	users := map[string][]string{}
	for _, n := range w.Nodes {
		spec, err := vc.Pool.Get(n.Agent)
		if err != nil {
			continue
		}
		for _, r := range spec.Capability.ExclusiveResources {
			users[r] = append(users[r], n.ID)
		}
	}
	capacity := func(r string) int {
		if vc.ResourceCapacity != nil {
			if c, ok := vc.ResourceCapacity[r]; ok {
				return c
			}
		}
		return 1
	}
	for resource, ids := range users {
		cap := capacity(resource)
		if cap <= 0 || len(ids) <= cap {
			continue
		}
		serial := map[string]bool{}
		for _, e := range w.Edges {
			if e.Kind == EdgeResource {
				serial[e.Src+"→"+e.Dst] = true
			}
		}
		ok := true
		for i := 0; i < len(ids); i++ {
			for j := i + 1; j < len(ids); j++ {
				if !serial[ids[i]+"→"+ids[j]] && !serial[ids[j]+"→"+ids[i]] {
					ok = false
				}
			}
		}
		if !ok {
			sort.Strings(ids)
			issues = append(issues, GraphIssue{Kind: IssueResourceConflict,
				Detail:     fmt.Sprintf("资源 %q 容量 %d，但节点 %v 之间没有串行边", resource, cap, ids),
				Suggestion: "给这些节点补 kind=resource 的边，或在调度配置里提高容量"})
		}
	}
	return issues
}
