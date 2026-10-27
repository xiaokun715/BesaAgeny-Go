package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/besa-ai/besa-agent/internal/domain"
)

// CompatProvider 实现 OpenAI 兼容协议，是 openai / dashscope / vllm 的基准实现。
type CompatProvider struct {
	cfg       ModelConfig
	name      string
	defaults  domain.CapabilitySet
	transport Transport
	chat      *compatChat
	embed     *compatEmbedding
}

func NewCompatProvider(name string, cfg ModelConfig, transport Transport) *CompatProvider {
	if transport == nil {
		transport = NewHTTPTransport(cfg.BaseURL, cfg.APIKey, cfg.Retries, cfg.Backoff, cfg.BackoffJitter)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 120 * time.Second
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = 2048
	}
	p := &CompatProvider{cfg: cfg, name: name, transport: transport, defaults: DefaultCapabilities(name)}
	p.chat = &compatChat{p: p}
	p.embed = &compatEmbedding{p: p}
	return p
}

func (p *CompatProvider) Name() string { return p.name }
func (p *CompatProvider) Capabilities() domain.CapabilitySet {
	return p.cfg.Capabilities
}
func (p *CompatProvider) ChatModel() (ChatModel, error) {
	if !p.cfg.Capabilities[domain.CapRuntime] {
		return nil, &CapabilityError{Model: p.cfg.Key, Need: "runtime", Missing: []string{"runtime"}}
	}
	return p.chat, nil
}
func (p *CompatProvider) EmbeddingModel() (EmbeddingModel, error) {
	if !p.cfg.Capabilities[domain.CapEmbedding] {
		return nil, &CapabilityError{Model: p.cfg.Key, Need: "embedding", Missing: []string{"embedding"}}
	}
	return p.embed, nil
}
func (p *CompatProvider) Close() error { return p.transport.Close() }

// ------------------------------ Chat ------------------------------

type compatChat struct{ p *CompatProvider }

func (c *compatChat) Chat(ctx context.Context, req domain.ChatRequest) (domain.ChatResponse, error) {
	payload, structured, err := c.p.buildPayload(req, false)
	if err != nil {
		return domain.ChatResponse{}, err
	}
	raw, err := c.p.transport.PostJSON(ctx, "/chat/completions", payload, req.Timeout, req.TraceID)
	if err != nil {
		return domain.ChatResponse{}, c.p.wrapError(err, req.TraceID)
	}
	return c.p.parseChatResponse(raw, structured, req.TraceID)
}

func (c *compatChat) StreamChat(ctx context.Context, req domain.ChatRequest) (Stream, error) {
	if !c.p.cfg.Capabilities[domain.CapStream] {
		return nil, &CapabilityError{Model: c.p.cfg.Key, Need: "stream", Missing: []string{"stream"}}
	}
	payload, _, err := c.p.buildPayload(req, true)
	if err != nil {
		return nil, err
	}
	sse, err := c.p.transport.StreamSSE(ctx, "/chat/completions", payload, req.Timeout, req.TraceID)
	if err != nil {
		return nil, c.p.wrapError(err, req.TraceID)
	}
	return &compatStream{sse: sse}, nil
}

type compatStream struct{ sse SSEStream }

func (s *compatStream) Recv() (string, error) {
	for {
		data, err := s.sse.Next()
		if err != nil {
			return "", err
		}
		if string(data) == "[DONE]" {
			return "", io.EOF
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(data, &chunk); err != nil {
			continue
		}
		if chunk.Error != nil {
			return "", &Error{Kind: "http", Message: chunk.Error.Message, Retryable: false}
		}
		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
			return chunk.Choices[0].Delta.Content, nil
		}
	}
}
func (s *compatStream) Close() error { return s.sse.Close() }

func (p *CompatProvider) buildPayload(req domain.ChatRequest, stream bool) (map[string]any, bool, error) {
	if len(req.Messages) == 0 {
		return nil, false, p.badRequest("消息列表不能为空")
	}
	for _, m := range req.Messages {
		if !m.Role.Valid() {
			return nil, false, p.badRequest(fmt.Sprintf("非法角色 %q", m.Role))
		}
		if m.Empty() && m.Role != domain.RoleTool && len(m.ToolCalls) == 0 {
			return nil, false, p.badRequest("消息内容为空")
		}
	}

	need := []domain.Capability{domain.CapRuntime}
	if stream {
		need = append(need, domain.CapStream)
	}
	if len(req.Tools) > 0 {
		need = append(need, domain.CapTools)
	}
	for _, m := range req.Messages {
		if m.HasImage() {
			need = append(need, domain.CapVision)
			break
		}
	}
	if missing := p.cfg.Capabilities.Missing(need); len(missing) > 0 {
		names := make([]string, len(missing))
		for i, c := range missing {
			names[i] = string(c)
		}
		return nil, false, &CapabilityError{Model: p.cfg.Key, Missing: names, Need: "本次请求"}
	}

	structured := true
	payload := map[string]any{
		"model":       p.cfg.Model,
		"messages":    toOpenAIMessages(req.Messages),
		"temperature": p.cfg.Temperature,
	}
	if req.Temperature != nil {
		payload["temperature"] = *req.Temperature
	}
	maxTokens := p.cfg.MaxTokens
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		maxTokens = *req.MaxTokens
	}
	payload["max_tokens"] = maxTokens
	if stream {
		payload["stream"] = true
	}
	if len(req.Tools) > 0 {
		payload["tools"] = toOpenAITools(req.Tools)
		if req.ToolChoice != "" {
			payload["tool_choice"] = req.ToolChoice
		}
	}
	if len(req.Stop) > 0 {
		payload["stop"] = req.Stop
	}

	switch req.ResponseFormat {
	case "", "text":
	case "json", "json_object":
		if !p.cfg.Capabilities[domain.CapJSON] {
			// json 能力缺失不拦截，交给适配器降级并标记非原生结构化。
			structured = false
		} else if p.cfg.UsesJSONObjectMode || req.JSONSchema == nil {
			payload["response_format"] = map[string]any{"type": "json_object"}
		} else {
			payload["response_format"] = map[string]any{
				"type": "json_schema",
				"json_schema": map[string]any{
					"name":   "response",
					"schema": req.JSONSchema,
					"strict": true,
				},
			}
		}
	case "json_schema":
		if req.JSONSchema == nil {
			return nil, false, p.badRequest("response_format=json_schema 时必须提供 json_schema")
		}
		if !p.cfg.Capabilities[domain.CapJSON] {
			structured = false
		} else {
			payload["response_format"] = map[string]any{
				"type": "json_schema",
				"json_schema": map[string]any{
					"name":   "response",
					"schema": req.JSONSchema,
					"strict": true,
				},
			}
		}
	default:
		return nil, false, p.badRequest("未知的 response_format: " + req.ResponseFormat)
	}
	for k, v := range p.cfg.Extra {
		if _, exists := payload[k]; !exists {
			payload[k] = v
		}
	}
	return payload, structured, nil
}

func (p *CompatProvider) parseChatResponse(raw map[string]any, structured bool, traceID string) (domain.ChatResponse, error) {
	out := domain.ChatResponse{StructuredNative: structured, Raw: raw, TraceID: traceID}
	out.Model = stringOf(raw["model"])
	if out.Model == "" {
		out.Model = p.cfg.Model
	}
	choices, _ := raw["choices"].([]any)
	if len(choices) > 0 {
		choice, _ := choices[0].(map[string]any)
		out.FinishReason = stringOf(choice["finish_reason"])
		msg, _ := choice["message"].(map[string]any)
		out.Content = stringOf(msg["content"])
		out.Reasoning = stringOf(msg["reasoning_content"])
		if out.Reasoning == "" {
			out.Reasoning = stringOf(msg["reasoning"])
		}
		if calls, ok := msg["tool_calls"].([]any); ok {
			for _, item := range calls {
				call, _ := item.(map[string]any)
				fn, _ := call["function"].(map[string]any)
				argsRaw := stringOf(fn["arguments"])
				args, err := domain.ParseArguments(argsRaw)
				tc := domain.ToolCall{
					ID:           stringOf(call["id"]),
					Name:         stringOf(fn["name"]),
					ArgumentsRaw: argsRaw,
					Arguments:    args,
				}
				if err != nil {
					// 解析失败不丢原始串：模型给了坏 JSON 时，排障需要它。
					tc.Arguments = map[string]any{"__raw__": argsRaw}
				}
				out.ToolCalls = append(out.ToolCalls, tc)
			}
		}
	}
	out.Usage = parseUsage(raw["usage"])
	if out.Content == "" && out.FinishReason == "length" {
		slog.Warn("模型返回空正文且 finish_reason=length：思考过程可能吃光了 max_tokens",
			"model", p.cfg.Model, "max_tokens", p.cfg.MaxTokens)
	}
	return out, nil
}

func (p *CompatProvider) wrapError(err error, traceID string) error {
	var pe *Error
	if errors.As(err, &pe) {
		e := pe.WithTrace(traceID)
		if e.Provider == "" {
			e.Provider = p.name
		}
		if e.Model == "" {
			e.Model = p.cfg.Model
		}
		return e
	}
	return &Error{Kind: "network", Provider: p.name, Model: p.cfg.Model, Message: err.Error(), Retryable: ClassifyNetwork(err), Err: err, TraceID: traceID}
}

func (p *CompatProvider) badRequest(msg string) error {
	return &Error{Kind: "config", Provider: p.name, Model: p.cfg.Model, Message: msg, Retryable: false}
}

// ------------------------------ Embedding ------------------------------

type compatEmbedding struct{ p *CompatProvider }

func (c *compatEmbedding) MaxBatch() int {
	if c.p.cfg.MaxBatch > 0 {
		return c.p.cfg.MaxBatch
	}
	return 32
}

func (c *compatEmbedding) Embed(ctx context.Context, texts []string, batchSize int, traceID string) (domain.EmbeddingResult, error) {
	if len(texts) == 0 {
		return domain.EmbeddingResult{}, c.p.badRequest("texts 不能为空")
	}
	batch := c.MaxBatch()
	if batchSize > 0 && batchSize < batch {
		batch = batchSize
	}
	var vectors [][]float32
	usage := domain.Usage{}
	for start := 0; start < len(texts); start += batch {
		end := min(start+batch, len(texts))
		payload := map[string]any{"model": c.p.cfg.Model, "input": texts[start:end]}
		raw, err := c.p.transport.PostJSON(ctx, "/embeddings", payload, 0, traceID)
		if err != nil {
			return domain.EmbeddingResult{}, c.p.wrapError(err, traceID)
		}
		items, _ := raw["data"].([]any)
		type indexed struct {
			index int
			vec   []float32
		}
		batchVecs := make([]indexed, 0, len(items))
		for i, item := range items {
			m, _ := item.(map[string]any)
			idx := i
			if v, ok := m["index"].(float64); ok {
				idx = int(v)
			}
			vecRaw, _ := m["embedding"].([]any)
			vec := make([]float32, len(vecRaw))
			for j, f := range vecRaw {
				if fv, ok := f.(float64); ok {
					vec[j] = float32(fv)
				}
			}
			batchVecs = append(batchVecs, indexed{index: idx, vec: vec})
		}
		sort.Slice(batchVecs, func(i, j int) bool { return batchVecs[i].index < batchVecs[j].index })
		if len(batchVecs) != end-start {
			return domain.EmbeddingResult{}, &Error{
				Kind: "parse", Provider: c.p.name, Model: c.p.cfg.Model,
				Message: fmt.Sprintf("向量化返回条数不符：需要 %d，实际 %d", end-start, len(batchVecs)),
			}
		}
		for _, bv := range batchVecs {
			if c.p.cfg.Dimension > 0 && len(bv.vec) != c.p.cfg.Dimension {
				return domain.EmbeddingResult{}, &Error{
					Kind: "parse", Provider: c.p.name, Model: c.p.cfg.Model,
					Message: fmt.Sprintf("向量维度不符：配置 %d，实际 %d", c.p.cfg.Dimension, len(bv.vec)),
				}
			}
			vectors = append(vectors, bv.vec)
		}
		usage = usage.Add(parseUsage(raw["usage"]))
	}
	dim := 0
	if len(vectors) > 0 {
		dim = len(vectors[0])
	}
	return domain.EmbeddingResult{Vectors: vectors, Model: c.p.cfg.Model, Dimension: dim, Usage: usage, TraceID: traceID}, nil
}

// ------------------------------ 转换 ------------------------------

func toOpenAIMessages(messages []domain.Message) []map[string]any {
	out := make([]map[string]any, 0, len(messages))
	for _, m := range messages {
		item := map[string]any{"role": string(m.Role)}
		if len(m.Parts) == 0 && m.Content == "" && len(m.ToolCalls) > 0 {
			// 只有工具调用、没有正文：多数厂商要求 content 为 null 而不是 ""。
			item["content"] = nil
		} else if len(m.Parts) > 0 {
			parts := make([]map[string]any, 0, len(m.Parts))
			for _, p := range m.Parts {
				if p.ImageURL != "" || p.ImageData != "" {
					url := p.ImageURL
					if p.ImageData != "" {
						media := p.MediaType
						if media == "" {
							media = "image/png"
						}
						url = "data:" + media + ";base64," + p.ImageData
					}
					parts = append(parts, map[string]any{
						"type":      "image_url",
						"image_url": map[string]any{"url": url, "detail": p.Detail},
					})
				} else {
					parts = append(parts, map[string]any{"type": "text", "text": p.Text})
				}
			}
			item["content"] = parts
		} else {
			item["content"] = m.Content
		}
		if m.Name != "" {
			item["name"] = m.Name
		}
		if m.ToolCallID != "" {
			item["tool_call_id"] = m.ToolCallID
		}
		if len(m.ToolCalls) > 0 {
			calls := make([]map[string]any, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				args := tc.ArgumentsRaw
				if args == "" {
					raw, _ := json.Marshal(tc.Arguments)
					args = string(raw)
				}
				calls = append(calls, map[string]any{
					"id":   tc.ID,
					"type": "function",
					"function": map[string]any{
						"name":      tc.Name,
						"arguments": args,
					},
				})
			}
			item["tool_calls"] = calls
		}
		out = append(out, item)
	}
	return out
}

func toOpenAITools(specs []domain.ToolSpec) []map[string]any {
	out := make([]map[string]any, 0, len(specs))
	for _, s := range specs {
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        s.Name,
				"description": s.Description,
				"parameters":  s.Parameters,
			},
		})
	}
	return out
}

func parseUsage(raw any) domain.Usage {
	m, _ := raw.(map[string]any)
	if m == nil {
		return domain.Usage{}
	}
	var u domain.Usage
	if v, ok := intOf(m["prompt_tokens"]); ok {
		u.InputTokens = &v
	}
	if v, ok := intOf(m["completion_tokens"]); ok {
		u.OutputTokens = &v
	}
	if details, ok := m["prompt_tokens_details"].(map[string]any); ok {
		if v, ok := intOf(details["cached_tokens"]); ok {
			u.CachedInputTokens = &v
		}
	}
	if u.CachedInputTokens == nil {
		if v, ok := intOf(m["cached_tokens"]); ok {
			u.CachedInputTokens = &v
		}
	}
	return u
}

func intOf(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}

func stringOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func JoinErrors(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	parts := make([]string, 0, len(errs))
	for _, e := range errs {
		parts = append(parts, e.Error())
	}
	return errors.New(strings.Join(parts, "; "))
}
