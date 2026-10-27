// Package dashscope 是 OpenAI 兼容协议的一个具名适配器。
//
// 适配器只决定协议形状；端点与凭据来自 models.<key>.vendor → providers.<vendor>。
package dashscope

import "github.com/besa-ai/besa-agent/internal/provider"

func New(cfg provider.ModelConfig) provider.Provider {
	return provider.NewCompatProvider("dashscope", cfg, nil)
}
