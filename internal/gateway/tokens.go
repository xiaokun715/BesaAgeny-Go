package gateway

import "github.com/besa-ai/besa-agent/internal/domain"

// CharsPerToken 是粗略估算：4 字符 ≈ 1 token。
// 口径只在这里定义一次 —— context 也用它，两边各写一份必然漂移。
const CharsPerToken = 4

func EstimateTextTokens(text string) int {
	n := 0
	for range text {
		n++
	}
	return (n + CharsPerToken - 1) / CharsPerToken
}

func EstimateMessagesTokens(messages []domain.Message) int {
	total := 0
	for _, m := range messages {
		total += EstimateTextTokens(m.Content) + 4
		for _, tc := range m.ToolCalls {
			total += EstimateTextTokens(tc.Name) + EstimateTextTokens(tc.ArgumentsRaw) + 8
		}
		for _, p := range m.Parts {
			total += EstimateTextTokens(p.Text)
			if p.ImageURL != "" || p.ImageData != "" {
				total += 256 // 图片按固定占位估算
			}
		}
	}
	return total
}
