package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/besa-ai/besa-agent/internal/domain"
	"github.com/besa-ai/besa-agent/internal/gateway"
)

type RoutingDecision struct {
	Stage      string
	Reason     string
	Confidence float64
	Candidates []string
	NeedsInput bool
	Scores     map[string]float64
}

type ChatClient interface {
	Chat(ctx context.Context, alias string, messages []domain.Message, opts gateway.ChatOptions) (domain.GatewayResult, error)
}

type StageRouter struct {
	Stages        *StageRegistry
	OnUnknown     string // ask | default | reject
	DefaultStage  string
	UseLLM        bool
	RuleThreshold float64
	Saturation    int
	Alias         string
	Chat          ChatClient
}

func NewStageRouter(stages *StageRegistry, onUnknown, defaultStage string, useLLM bool, threshold float64, saturation int, chat ChatClient, alias string) *StageRouter {
	if onUnknown == "" {
		onUnknown = "ask"
	}
	if threshold <= 0 {
		threshold = 0.5
	}
	if saturation <= 0 {
		saturation = 2
	}
	return &StageRouter{Stages: stages, OnUnknown: onUnknown, DefaultStage: defaultStage, UseLLM: useLLM, RuleThreshold: threshold, Saturation: saturation, Chat: chat, Alias: alias}
}

// Route 顺序：显式 → 规则 → LLM → on_unknown。每一层都带理由。
func (r *StageRouter) Route(ctx context.Context, task, stage, hint string) (RoutingDecision, error) {
	if stage != "" {
		if _, err := r.Stages.Get(stage); err != nil {
			return RoutingDecision{}, err
		}
		return RoutingDecision{Stage: stage, Reason: "调用方显式指定", Confidence: 1.0}, nil
	}

	if decision, ok := r.byRules(task, hint); ok {
		return decision, nil
	}

	if r.UseLLM && r.Chat != nil {
		if decision, ok := r.byLLM(ctx, task); ok {
			return decision, nil
		}
	}

	return r.onUnknown(task)
}

func (r *StageRouter) byRules(task, hint string) (RoutingDecision, bool) {
	text := strings.ToLower(task + "\n" + hint)
	type score struct {
		stage string
		value float64
		hits  int
	}
	var scores []score
	all := r.Stages.All()
	for _, a := range all {
		hits := 0
		for _, kw := range a.Keywords {
			if kw != "" && strings.Contains(text, strings.ToLower(kw)) {
				hits++
			}
		}
		// 分母是**固定饱和数**，不是关键词总数 ——
		// 用总数会得到一个荒谬的激励：关键词写全反而更选不中。
		s := float64(hits) / float64(r.Saturation)
		if s > 1 {
			s = 1
		}
		scores = append(scores, score{stage: a.Stage, value: s, hits: hits})
	}
	sort.SliceStable(scores, func(i, j int) bool { return scores[i].value > scores[j].value })
	if len(scores) == 0 || scores[0].value < r.RuleThreshold {
		return RoutingDecision{}, false
	}
	if len(scores) > 1 && scores[1].value == scores[0].value {
		return RoutingDecision{}, false // 并列 → 交给下一层
	}
	candidates := make([]string, 0, len(scores)-1)
	scoreMap := map[string]float64{}
	for i, s := range scores {
		scoreMap[s.stage] = s.value
		if i > 0 {
			candidates = append(candidates, s.stage)
		}
	}
	return RoutingDecision{
		Stage: scores[0].stage, Confidence: scores[0].value, Candidates: candidates, Scores: scoreMap,
		Reason: fmt.Sprintf("命中关键词 %d 个（得分 %.2f）", scores[0].hits, scores[0].value),
	}, true
}

func (r *StageRouter) byLLM(ctx context.Context, task string) (RoutingDecision, bool) {
	names := r.Stages.Names()
	system := "你是测试流程路由器。只能从给定阶段中选择一个，并给出 0..1 的置信度与简短理由。"
	prompt := fmt.Sprintf("可用阶段：%s\n\n任务：%s", strings.Join(names, ", "), task)
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"stage":      map[string]any{"type": "string", "enum": toAny(names)},
			"confidence": map[string]any{"type": "number"},
			"reason":     map[string]any{"type": "string"},
		},
		"required": []string{"stage", "confidence", "reason"},
	}
	result, err := r.Chat.Chat(ctx, r.Alias, []domain.Message{domain.System(system), domain.User(prompt)}, gateway.ChatOptions{
		ResponseFormat: "json", JSONSchema: schema,
	})
	if err != nil {
		slog.Warn("LLM 阶段路由失败，退到 on_unknown 策略", "err", err)
		return RoutingDecision{}, false
	}
	var parsed struct {
		Stage      string  `json:"stage"`
		Confidence float64 `json:"confidence"`
		Reason     string  `json:"reason"`
	}
	if err := json.Unmarshal([]byte(stripCodeFence(result.Content())), &parsed); err != nil {
		return RoutingDecision{}, false
	}
	if parsed.Stage == "" || parsed.Confidence < r.RuleThreshold {
		return RoutingDecision{}, false
	}
	if _, err := r.Stages.Get(parsed.Stage); err != nil {
		return RoutingDecision{}, false
	}
	return RoutingDecision{Stage: parsed.Stage, Confidence: parsed.Confidence, Reason: "LLM 判断：" + parsed.Reason}, true
}

func (r *StageRouter) onUnknown(task string) (RoutingDecision, error) {
	switch r.OnUnknown {
	case "ask":
		return RoutingDecision{
			Reason: "无法从任务推断阶段，请显式指定 stage", NeedsInput: true,
			Candidates: r.Stages.Names(),
		}, nil
	case "default":
		if r.DefaultStage == "" {
			return RoutingDecision{}, fmt.Errorf("agent.routing.on_unknown=default 但没有配置 default_stage")
		}
		if _, err := r.Stages.Get(r.DefaultStage); err != nil {
			return RoutingDecision{}, fmt.Errorf("默认阶段不存在：%w", err)
		}
		slog.Warn("阶段路由失败，使用兜底阶段", "stage", r.DefaultStage)
		return RoutingDecision{Stage: r.DefaultStage, Reason: "路由失败，使用配置的兜底阶段", Confidence: 0, Candidates: r.Stages.Names()}, nil
	case "reject":
		return RoutingDecision{}, fmt.Errorf("无法从任务推断阶段，且配置为 reject（可用：%s）", strings.Join(r.Stages.Names(), ", "))
	default:
		return RoutingDecision{}, fmt.Errorf("未知的 agent.routing.on_unknown=%q", r.OnUnknown)
	}
}

func toAny(items []string) []any {
	out := make([]any, len(items))
	for i, s := range items {
		out[i] = s
	}
	return out
}

func stripCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(s, "```")
	}
	return strings.TrimSpace(s)
}
