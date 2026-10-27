package memory

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
)

type Policy struct {
	Enabled       bool
	AllowedScopes []Scope
	MinChars      int
	MaxChars      int
	EnabledKinds  []Kind

	RuleGate           bool
	LLMJudge           bool
	JudgeOnUnavailable string // reject | allow

	PII          string // reject | redact | allow
	DenyKeywords []string
	DenyPatterns []string

	SimilarityThreshold float64
	MergeThreshold      float64
	TopK                int
	MaxBytes            int
	LexicalWeight       float64
	Decay               DecayPolicy
}

func DefaultPolicy() Policy {
	return Policy{
		Enabled: false, AllowedScopes: []Scope{ScopeSession}, MinChars: 4, MaxChars: 2000,
		EnabledKinds: []Kind{KindFact, KindPreference, KindTaskState, KindLesson},
		RuleGate:     true, LLMJudge: true, JudgeOnUnavailable: "reject",
		PII: "redact", DenyKeywords: []string{},
		DenyPatterns:        []string{"sk-", "AKIA", "-----BEGIN"},
		SimilarityThreshold: 0.90, MergeThreshold: 0.96,
		TopK: 20, MaxBytes: 32768, LexicalWeight: 0.3, Decay: DefaultDecayPolicy(),
	}
}

type GateResult struct {
	Allowed bool
	Content string
	Reason  string
}

var (
	transientPatterns = []*regexp.Regexp{
		regexp.MustCompile(`\b[0-9a-fA-F]{32}\b`),
		regexp.MustCompile(`\b[0-9a-fA-F-]{36}\b`),
		// 毫秒时间戳。不要拦 11 位手机号 —— 那是 PII，交给合规闸门脱敏。
		regexp.MustCompile(`\b\d{13}\b`),
	}
	secretPatterns = []*regexp.Regexp{
		regexp.MustCompile(`sk-[A-Za-z0-9_\-]{8,}`),
		regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
		regexp.MustCompile(`-----BEGIN [A-Z ]+-----`),
		regexp.MustCompile(`(?i)(password|api[_-]?key)\s*[=:]\s*\S+`),
	}
	phonePattern = regexp.MustCompile(`\b1[3-9]\d{9}\b`)
	emailPattern = regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`)
	idPattern    = regexp.MustCompile(`\b\d{17}[\dXx]\b`)
	cardPattern  = regexp.MustCompile(`\b\d{16,19}\b`)
)

type LLM interface {
	Complete(ctx context.Context, system, prompt string, schema map[string]any, maxTokens int) (string, error)
}

type Gates struct {
	Policy Policy
	LLM    LLM
}

// Check 顺序：规则（零成本）→ LLM 判定 → 合规（硬否决）。
func (g Gates) Check(ctx context.Context, content string, kind Kind, scope Scope) GateResult {
	if g.Policy.RuleGate {
		r := g.ruleGate(content, kind, scope)
		if !r.Allowed {
			return r
		}
	}
	if g.Policy.LLMJudge && g.LLM != nil {
		ok, reason := g.llmJudge(ctx, content, kind)
		if !ok {
			return GateResult{Allowed: false, Reason: reason}
		}
	}
	return g.compliance(content)
}

func (g Gates) ruleGate(content string, kind Kind, scope Scope) GateResult {
	if len([]rune(content)) < g.Policy.MinChars {
		return GateResult{Reason: "内容过短（无信息）"}
	}
	if len([]rune(content)) > g.Policy.MaxChars {
		return GateResult{Reason: "内容过长（多半是整段原文）"}
	}
	allowedScope := false
	for _, s := range g.Policy.AllowedScopes {
		if s == scope {
			allowedScope = true
		}
	}
	if !allowedScope {
		return GateResult{Reason: "作用域 " + string(scope) + " 未在允许列表里"}
	}
	kindAllowed := false
	for _, k := range g.Policy.EnabledKinds {
		if k == kind {
			kindAllowed = true
		}
	}
	if !kindAllowed {
		return GateResult{Reason: "记忆类型 " + string(kind) + " 未启用"}
	}
	for _, re := range secretPatterns {
		if re.MatchString(content) {
			// 命中密钥**绝不送 LLM**。
			return GateResult{Reason: "命中密钥形态，拒绝写入"}
		}
	}
	for _, re := range transientPatterns {
		if re.MatchString(content) {
			return GateResult{Reason: "疑似临时信息（ID/时间戳），拒绝写入"}
		}
	}
	return GateResult{Allowed: true, Content: content}
}

func (g Gates) llmJudge(ctx context.Context, content string, kind Kind) (bool, string) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"should_store": map[string]any{"type": "boolean"},
			"reason":       map[string]any{"type": "string"},
		},
		"required": []string{"should_store", "reason"},
	}
	out, err := g.LLM.Complete(ctx,
		"你判断一条信息是否值得长期记住。只记稳定事实、偏好、任务状态、可复用的经验；不要记临时信息与原文。",
		"类型："+string(kind)+"\n内容："+content, schema, 256)
	if err != nil {
		if g.Policy.JudgeOnUnavailable == "allow" {
			return true, "判定不可用，按配置放行"
		}
		return false, "判定不可用，按配置拒绝（fail-closed）"
	}
	var parsed struct {
		ShouldStore bool   `json:"should_store"`
		Reason      string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(stripFence(out)), &parsed); err != nil {
		if g.Policy.JudgeOnUnavailable == "allow" {
			return true, "判定结果无法解析，按配置放行"
		}
		return false, "判定结果无法解析，按配置拒绝"
	}
	return parsed.ShouldStore, "LLM 判定：" + parsed.Reason
}

func (g Gates) compliance(content string) GateResult {
	for _, kw := range g.Policy.DenyKeywords {
		if kw != "" && strings.Contains(content, kw) {
			return GateResult{Reason: "命中合规禁记关键词 " + kw}
		}
	}
	for _, pattern := range g.Policy.DenyPatterns {
		if pattern != "" && strings.Contains(content, pattern) {
			return GateResult{Reason: "命中合规禁记模式 " + pattern}
		}
	}
	redacted := content
	switch g.Policy.PII {
	case "reject":
		if phonePattern.MatchString(content) || emailPattern.MatchString(content) ||
			idPattern.MatchString(content) || cardPattern.MatchString(content) {
			return GateResult{Reason: "命中 PII，按配置拒绝"}
		}
	case "redact":
		redacted = phonePattern.ReplaceAllString(redacted, "[PHONE]")
		redacted = emailPattern.ReplaceAllString(redacted, "[EMAIL]")
		redacted = idPattern.ReplaceAllString(redacted, "[ID]")
		redacted = cardPattern.ReplaceAllString(redacted, "[CARD]")
	}
	return GateResult{Allowed: true, Content: redacted}
}

func stripFence(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}
