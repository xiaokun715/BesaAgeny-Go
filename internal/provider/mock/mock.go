// Package mock 是确定性的假实现 —— CI 与默认配置的黑盒路径。
//
// 它不发任何网络请求，输出由输入唯一决定（不随机），因此可用于断言。
package mock

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"io"

	"github.com/besa-ai/besa-agent/internal/domain"
	"github.com/besa-ai/besa-agent/internal/provider"
)

type Provider struct {
	cfg provider.ModelConfig
}

func New(cfg provider.ModelConfig) provider.Provider {
	if cfg.Dimension <= 0 {
		cfg.Dimension = 768
	}
	return &Provider{cfg: cfg}
}

func (p *Provider) Name() string                       { return "mock" }
func (p *Provider) Capabilities() domain.CapabilitySet { return p.cfg.Capabilities }
func (p *Provider) Close() error                       { return nil }

func (p *Provider) ChatModel() (provider.ChatModel, error) {
	return &chatModel{p: p}, nil
}

func (p *Provider) EmbeddingModel() (provider.EmbeddingModel, error) {
	return &embeddingModel{p: p}, nil
}

type chatModel struct{ p *Provider }

func (c *chatModel) Chat(ctx context.Context, req domain.ChatRequest) (domain.ChatResponse, error) {
	if err := ctx.Err(); err != nil {
		return domain.ChatResponse{}, err
	}
	answer := c.answer(req)
	in := estimateTokens(req.Messages)
	out := max(1, len([]rune(answer))/4)
	return domain.ChatResponse{
		Content:          answer,
		Model:            c.p.cfg.Model,
		FinishReason:     "stop",
		Usage:            domain.Usage{InputTokens: &in, OutputTokens: &out},
		StructuredNative: req.ResponseFormat == "",
		Raw:              map[string]any{"mock": true},
	}, nil
}

func (c *chatModel) StreamChat(ctx context.Context, req domain.ChatRequest) (provider.Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &mockStream{answer: []rune(c.answer(req))}, nil
}

func (c *chatModel) answer(req domain.ChatRequest) string {
	// 已经拿到工具结果 → 给最终答案，避免演示时无限调用工具。
	for _, m := range req.Messages {
		if m.Role == domain.RoleTool {
			return "[mock] 已收到工具结果，任务完成。"
		}
	}
	last := ""
	for _, m := range req.Messages {
		if m.Role == domain.RoleUser {
			last = m.Content
		}
	}
	if last == "" {
		last = "（无输入）"
	}
	return "[mock] " + truncateRunes(last, 200)
}

type mockStream struct {
	answer []rune
	pos    int
}

func (s *mockStream) Recv() (string, error) {
	if s.pos >= len(s.answer) {
		return "", io.EOF
	}
	end := min(s.pos+8, len(s.answer))
	chunk := string(s.answer[s.pos:end])
	s.pos = end
	return chunk, nil
}
func (s *mockStream) Close() error { return nil }

type embeddingModel struct{ p *Provider }

func (e *embeddingModel) MaxBatch() int {
	if e.p.cfg.MaxBatch > 0 {
		return e.p.cfg.MaxBatch
	}
	return 32
}

func (e *embeddingModel) Embed(ctx context.Context, texts []string, batchSize int, traceID string) (domain.EmbeddingResult, error) {
	if err := ctx.Err(); err != nil {
		return domain.EmbeddingResult{}, err
	}
	dim := e.p.cfg.Dimension
	if dim <= 0 {
		dim = 768
	}
	vecs := make([][]float32, len(texts))
	for i, text := range texts {
		vecs[i] = deterministicVector(text, dim)
	}
	tokens := 0
	for _, t := range texts {
		tokens += estimateTokens([]domain.Message{{Role: domain.RoleUser, Content: t}})
	}
	return domain.EmbeddingResult{
		Vectors:   vecs,
		Model:     e.p.cfg.Model,
		Dimension: dim,
		Usage:     domain.Usage{InputTokens: &tokens},
		TraceID:   traceID,
	}, nil
}

func deterministicVector(text string, dim int) []float32 {
	out := make([]float32, dim)
	seed := sha256.Sum256([]byte(text))
	var sum float64
	for i := 0; i < dim; i++ {
		block := seed[(i*4)%len(seed):]
		var buf [8]byte
		copy(buf[:], block)
		v := float64(binary.LittleEndian.Uint32(buf[:4]))/float64(1<<32) - 0.5
		out[i] = float32(v)
		sum += v * v
	}
	norm := float32(1)
	if sum > 0 {
		norm = float32(1 / sqrt(sum))
	}
	for i := range out {
		out[i] *= norm
	}
	return out
}

func sqrt(x float64) float64 {
	if x <= 0 {
		return 0
	}
	z := x
	for i := 0; i < 20; i++ {
		z = (z + x/z) / 2
	}
	return z
}

func estimateTokens(messages []domain.Message) int {
	total := 0
	for _, m := range messages {
		total += len([]rune(m.Content)) + len(m.ToolCalls)*16 + 4
	}
	return max(1, total/4)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
