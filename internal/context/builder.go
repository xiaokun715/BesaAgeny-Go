package context

import (
	"strings"

	"github.com/besa-ai/besa-agent/internal/domain"
)

func FromDomain(m domain.Message) Message {
	out := Message{Role: m.Role, Content: m.Content, Name: m.Name, ToolCallID: m.ToolCallID}
	for _, tc := range m.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, ToolCallRef{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments, ArgumentsRaw: tc.ArgumentsRaw})
	}
	return out
}

func ToDomain(m Message) domain.Message {
	out := domain.Message{Role: m.Role, Content: m.Content, Name: m.Name, ToolCallID: m.ToolCallID}
	for _, tc := range m.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, domain.ToolCall{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments, ArgumentsRaw: tc.ArgumentsRaw})
	}
	return out
}

func FromDomainAll(messages []domain.Message) []Message {
	out := make([]Message, len(messages))
	for i, m := range messages {
		out[i] = FromDomain(m)
	}
	return out
}

// Rebuild 把块还原成消息序列。**工具调用与结果必须同进同出** ——
// 这是压缩只能按“步”切的原因。
func Rebuild(blocks []Block) []domain.Message {
	out := make([]domain.Message, 0, len(blocks))
	for _, b := range blocks {
		if b.Message != nil {
			out = append(out, ToDomain(*b.Message))
			continue
		}
		out = append(out, domain.User(b.ToModelText()))
	}
	return out
}

func BuildMessages(system, current string, blocks []Block) []domain.Message {
	out := make([]domain.Message, 0, len(blocks)+2)
	if system != "" {
		out = append(out, domain.System(system))
	}
	for _, b := range sortBlocksForAssembly(blocks) {
		if b.Message != nil {
			m := ToDomain(*b.Message)
			if m.Role == domain.RoleTool && m.ToolCallID == "" {
				out = append(out, domain.User("[缺少配对 id 的工具结果]\n"+m.Content))
				continue
			}
			if m.Role == domain.RoleSystem {
				continue
			}
			out = append(out, m)
			continue
		}
		if b.Segment == SegmentSystem || b.Segment == SegmentCurrent {
			continue
		}
		out = append(out, domain.User(b.ToModelText()))
	}
	if current != "" {
		out = append(out, domain.User(current))
	}
	return out
}

var assemblyOrder = []Segment{SegmentLongTerm, SegmentSessionSummary, SegmentTransition, SegmentWindow}

func sortBlocksForAssembly(blocks []Block) []Block {
	rank := map[Segment]int{}
	for i, s := range assemblyOrder {
		rank[s] = i
	}
	out := append([]Block{}, blocks...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && rank[out[j].Segment] < rank[out[j-1].Segment]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Dedupe 跨段去重：只比较长度 ≥ 40 的块，避免“好的/收到”被误删。
// 保留优先级：window > transition > long_term > session_summary（原文优先于摘要）。
func Dedupe(blocks []Block) ([]Block, []string) {
	seen := map[string]int{} // fingerprint -> index in kept
	kept := make([]Block, 0, len(blocks))
	var dropped []string
	for _, b := range blocks {
		text := strings.Join(strings.Fields(strings.ToLower(b.Content)), " ")
		if b.Message != nil {
			text = strings.Join(strings.Fields(strings.ToLower(b.Message.Content)), " ")
		}
		if len([]rune(text)) < 40 {
			kept = append(kept, b)
			continue
		}
		if idx, ok := seen[text]; ok {
			if segmentRank(b.Segment) < segmentRank(kept[idx].Segment) {
				dropped = append(dropped, string(kept[idx].Segment)+":"+short(kept[idx].Content))
				kept[idx] = b
			} else {
				dropped = append(dropped, string(b.Segment)+":"+short(b.Content))
			}
			continue
		}
		seen[text] = len(kept)
		kept = append(kept, b)
	}
	return kept, dropped
}

func short(s string) string {
	r := []rune(s)
	if len(r) <= 40 {
		return s
	}
	return string(r[:40]) + "…"
}

func sumTokens(blocks []Block) int {
	total := 0
	for _, b := range blocks {
		if b.Message != nil {
			total += EstimateTokens(b.Message.Content)
		} else {
			total += EstimateTokens(b.Content)
		}
	}
	return total
}

// Fit 整块保留或整块丢弃，不切半个。
func Fit(blocks []Block, quota int) ([]Block, []string) {
	used := 0
	out := make([]Block, 0, len(blocks))
	var notes []string
	for _, b := range blocks {
		tokens := EstimateTokens(b.Content)
		if b.Message != nil {
			tokens = EstimateTokens(b.Message.Content)
		}
		if used+tokens > quota {
			notes = append(notes, "块超出配额被整块丢弃："+short(b.Content))
			continue
		}
		used += tokens
		out = append(out, b)
	}
	return out, notes
}
