// Package provider 收敛各家厂商 API 的形状差异。
//
// 它只回答“这个请求在该厂商的 API 上怎么写、怎么读”，**不选模型、不重试、
// 不降级、不熔断、不记账** —— 那些是 gateway 的职责。
package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/besa-ai/besa-agent/internal/domain"
)

// ModelConfig 是模型在 provider 视角下的投影。
type ModelConfig struct {
	Key       string // 模型键（models.<key>）
	Adapter   string // 协议适配器：openai / dashscope / vllm / mock
	Vendor    string // 端点与凭据来源（providers.<vendor>）
	Model     string // 上游模型名
	BaseURL   string
	APIKey    string
	APIKeyEnv string

	Temperature   float64
	MaxTokens     int
	Timeout       time.Duration
	Retries       int // **只重试网络层**
	Backoff       time.Duration
	BackoffJitter float64

	Capabilities domain.CapabilitySet

	Dimension int
	MaxBatch  int

	SupportsJSONNative bool
	UsesJSONObjectMode bool
	RequireAPIKey      bool
	Extra              map[string]any
}

func (c ModelConfig) String() string {
	// 不输出 APIKey。
	return fmt.Sprintf("%s(%s/%s)", c.Key, c.Adapter, c.Model)
}

// ChatModel 是“一次对话补全”的能力。
type ChatModel interface {
	Chat(ctx context.Context, req domain.ChatRequest) (domain.ChatResponse, error)
	// StreamChat **必须在调用瞬间完成校验**并返回流；不要把错误推迟到 Recv。
	StreamChat(ctx context.Context, req domain.ChatRequest) (Stream, error)
}

// Stream 产出正文增量。Close 幂等。
type Stream interface {
	Recv() (string, error) // io.EOF 表示正常结束
	Close() error
}

type EmbeddingModel interface {
	Embed(ctx context.Context, texts []string, batchSize int, traceID string) (domain.EmbeddingResult, error)
	MaxBatch() int
}

// Provider 是一族模型的适配器实例（共享 HTTP 连接池与凭据）。
type Provider interface {
	Name() string
	Capabilities() domain.CapabilitySet
	ChatModel() (ChatModel, error)
	EmbeddingModel() (EmbeddingModel, error)
	Close() error
}

// Transport 是唯一允许直接碰 HTTP 的契约。
type Transport interface {
	PostJSON(ctx context.Context, path string, payload any, timeout time.Duration, traceID string) (map[string]any, error)
	StreamSSE(ctx context.Context, path string, payload any, timeout time.Duration, traceID string) (SSEStream, error)
	Close() error
}

type SSEStream interface {
	Next() ([]byte, error) // 返回一个 data: 载荷；io.EOF 结束
	Close() error
}

// DefaultCapabilities 是各适配器的默认能力集。
//
// 能力是**模型**的事实：embedding / vision 默认都不在里面。
func DefaultCapabilities(adapter string) domain.CapabilitySet {
	switch adapter {
	case "mock":
		return domain.CapabilitySet{
			domain.CapRuntime: true, domain.CapStream: true, domain.CapTools: true,
			domain.CapJSON: true, domain.CapVision: true, domain.CapEmbedding: true,
		}
	case "vllm":
		return domain.CapabilitySet{domain.CapRuntime: true, domain.CapStream: true}
	default: // openai / dashscope
		return domain.CapabilitySet{
			domain.CapRuntime: true, domain.CapStream: true,
			domain.CapTools: true, domain.CapJSON: true,
		}
	}
}

// ResolveCapabilities 实现“完整声明 > 增量覆盖 > 厂商默认”。
func ResolveCapabilities(raw any, defaults domain.CapabilitySet) domain.CapabilitySet {
	out := domain.CapabilitySet{}
	for k, v := range defaults {
		out[k] = v
	}
	switch v := raw.(type) {
	case []any: // 列表 = 完整声明，替换默认
		out = domain.CapabilitySet{}
		for _, item := range v {
			out[domain.Capability(fmt.Sprint(item))] = true
		}
	case []string:
		out = domain.CapabilitySet{}
		for _, item := range v {
			out[domain.Capability(item)] = true
		}
	case map[string]any: // 映射 = 增量覆盖
		for key, val := range v {
			out[domain.Capability(key)] = truthy(val)
		}
	case map[any]any:
		for key, val := range v {
			out[domain.Capability(fmt.Sprint(key))] = truthy(val)
		}
	}
	return out
}

func truthy(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case string:
		return b == "true" || b == "1" || b == "yes"
	case int:
		return b != 0
	case float64:
		return b != 0
	}
	return false
}

func HasCapability(set domain.CapabilitySet, c domain.Capability) bool { return set[c] }
