package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	ctxpkg "github.com/besa-ai/besa-agent/internal/context"
)

type Candidate struct {
	Content    string
	Kind       Kind
	Confidence float64
	Importance float64
	Tags       []string
}

type Extractor struct {
	LLM           LLM
	MaxItems      int
	MaxInputChars int
	MaxItemChars  int
}

func NewExtractor(llm LLM, maxItems, maxInputChars int) *Extractor {
	if maxItems <= 0 {
		maxItems = 5
	}
	if maxInputChars <= 0 {
		maxInputChars = 8000
	}
	return &Extractor{LLM: llm, MaxItems: maxItems, MaxInputChars: maxInputChars, MaxItemChars: 2000}
}

// Extract 失败返回空 —— 记忆是增益，不是依赖。
func (e *Extractor) Extract(ctx context.Context, turns []ctxpkg.Turn, traceID string) []Candidate {
	if e.LLM == nil || len(turns) == 0 {
		return nil
	}
	var b strings.Builder
	for _, t := range turns {
		fmt.Fprintf(&b, "[%s] %s\n", t.Role, t.Content)
	}
	text := b.String()
	if len([]rune(text)) > e.MaxInputChars {
		text = string([]rune(text)[:e.MaxInputChars])
	}
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"items": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"content":    map[string]any{"type": "string"},
						"kind":       map[string]any{"type": "string", "enum": []string{"fact", "preference", "task_state", "lesson"}},
						"confidence": map[string]any{"type": "number"},
						"importance": map[string]any{"type": "number"},
						"tags":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					},
					"required": []string{"content", "kind", "confidence"},
				},
			},
		},
		"required": []string{"items"},
	}
	out, err := e.LLM.Complete(ctx,
		"从对话中提取值得长期记住的信息。只提取稳定事实、用户偏好、任务状态、可复用经验；不要提取寒暄与临时信息。",
		text, schema, 1024)
	if err != nil {
		slog.Warn("记忆提取失败（旁路）", "err", err)
		return nil
	}
	var parsed struct {
		Items []struct {
			Content    string   `json:"content"`
			Kind       string   `json:"kind"`
			Confidence float64  `json:"confidence"`
			Importance float64  `json:"importance"`
			Tags       []string `json:"tags"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(stripFence(out)), &parsed); err != nil {
		return nil
	}
	var candidates []Candidate
	for _, item := range parsed.Items {
		if strings.TrimSpace(item.Content) == "" {
			continue
		}
		content := item.Content
		if len([]rune(content)) > e.MaxItemChars {
			content = string([]rune(content)[:e.MaxItemChars])
		}
		kind := Kind(item.Kind)
		switch kind {
		case KindFact, KindPreference, KindTaskState, KindLesson:
		default:
			kind = KindFact
		}
		importance := item.Importance
		if importance <= 0 {
			importance = item.Confidence
		}
		candidates = append(candidates, Candidate{Content: content, Kind: kind, Confidence: item.Confidence, Importance: importance, Tags: item.Tags})
		if len(candidates) >= e.MaxItems {
			break
		}
	}
	return candidates
}
