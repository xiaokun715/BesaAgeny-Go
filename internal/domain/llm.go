package domain

import (
	"fmt"
	"time"

	"github.com/shopspring/decimal"
)

type Capability string

// 能力是**模型级事实**，不是厂商级事实。
const (
	CapRuntime   Capability = "runtime"
	CapStream    Capability = "stream"
	CapTools     Capability = "tools"
	CapJSON      Capability = "json"
	CapVision    Capability = "vision"
	CapEmbedding Capability = "embedding"
)

type CapabilitySet map[Capability]bool

func (s CapabilitySet) Has(c Capability) bool { return s[c] }

func (s CapabilitySet) SupportsAll(req []Capability) bool {
	for _, c := range req {
		if !s[c] {
			return false
		}
	}
	return true
}

func (s CapabilitySet) List() []Capability {
	order := []Capability{CapRuntime, CapStream, CapTools, CapJSON, CapVision, CapEmbedding}
	out := make([]Capability, 0, len(s))
	for _, c := range order {
		if s[c] {
			out = append(out, c)
		}
	}
	return out
}

func (s CapabilitySet) Missing(req []Capability) []Capability {
	var missing []Capability
	for _, c := range req {
		if !s[c] {
			missing = append(missing, c)
		}
	}
	return missing
}

type Usage struct {
	// nil 表示“上游没说”。**绝不能用 0 代替**：0 是免费，nil 是不知道。
	InputTokens       *int `json:"input_tokens,omitempty"`
	OutputTokens      *int `json:"output_tokens,omitempty"`
	CachedInputTokens *int `json:"cached_input_tokens,omitempty"`
}

func IntPtr(v int) *int { return &v }

func (u Usage) Known() bool { return u.InputTokens != nil || u.OutputTokens != nil }

func (u Usage) Total() *int {
	if u.InputTokens == nil && u.OutputTokens == nil {
		return nil
	}
	total := 0
	if u.InputTokens != nil {
		total += *u.InputTokens
	}
	if u.OutputTokens != nil {
		total += *u.OutputTokens
	}
	return &total
}

func (u Usage) Add(other Usage) Usage {
	return Usage{
		InputTokens:       addPtr(u.InputTokens, other.InputTokens),
		OutputTokens:      addPtr(u.OutputTokens, other.OutputTokens),
		CachedInputTokens: addPtr(u.CachedInputTokens, other.CachedInputTokens),
	}
}

func addPtr(a, b *int) *int {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	v := *a + *b
	return &v
}

type Cost struct {
	Currency string
	// Amount=nil 表示未知（价格未配置或用量未知），不是 0。
	Amount *decimal.Decimal
}

func (c Cost) Known() bool { return c.Amount != nil }
func (c Cost) String() string {
	if c.Amount == nil {
		return "unknown"
	}
	return fmt.Sprintf("%s %s", c.Amount.StringFixed(8), c.Currency)
}

type ChatRequest struct {
	Messages       []Message
	Temperature    *float64
	MaxTokens      *int
	Stream         bool
	ResponseFormat string
	JSONSchema     map[string]any
	Stop           []string
	Tools          []ToolSpec
	ToolChoice     string
	Timeout        time.Duration
	TraceID        string
}

type ChatResponse struct {
	Content          string
	Model            string
	FinishReason     string
	Reasoning        string
	ToolCalls        []ToolCall
	Usage            Usage
	StructuredNative bool
	Raw              map[string]any
	TraceID          string
}

type EmbeddingResult struct {
	Vectors   [][]float32
	Model     string
	Dimension int
	Usage     Usage
	TraceID   string
}

// StreamEvent 是网关流式调用的三种事件之一。
type StreamEventKind string

const (
	StreamKindChunk  StreamEventKind = "chunk"
	StreamKindDone   StreamEventKind = "done"
	StreamKindFailed StreamEventKind = "failed"
)

type StreamEvent struct {
	Kind     StreamEventKind
	Text     string
	Result   *GatewayResult
	Err      error
	Attempts []AttemptRecord
}

// AttemptRecord 保留每一次尝试。只报最后一条会掩盖真正的原因。
type AttemptRecord struct {
	ModelKey      string  `json:"model_key"`
	Provider      string  `json:"provider"`
	Outcome       string  `json:"outcome"` // success | failed | skipped
	Error         string  `json:"error,omitempty"`
	Retryable     bool    `json:"retryable,omitempty"`
	SkippedReason string  `json:"skipped_reason,omitempty"`
	ElapsedS      float64 `json:"elapsed_s,omitempty"`
}

type GatewayResult struct {
	Response  ChatResponse
	Embedding *EmbeddingResult
	Alias     string
	ModelKey  string
	Attempts  []AttemptRecord
	Degraded  bool
	Usage     Usage
	Cost      Cost
	TraceID   string
}

func (r GatewayResult) Content() string {
	if r.Embedding != nil {
		return ""
	}
	return r.Response.Content
}

func (r GatewayResult) ToolCalls() []ToolCall { return r.Response.ToolCalls }
