package context

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"

	"github.com/besa-ai/besa-agent/internal/domain"
)

// Summarizer 只负责“把一段文本压成摘要”。它由组合根接到 gateway；
// context 不认识 provider。
type Summarizer interface {
	Complete(ctx context.Context, system, prompt string, maxTokens int) (string, error)
}

type Compactor struct {
	Summarizer     Summarizer
	MaxLevel       Level
	L1MaxToolChars int
	MaxLLMCalls    int
	PromptVersion  string

	mu    sync.Mutex
	cache map[string]string
}

func NewCompactor(summarizer Summarizer, maxLevel string, l1MaxToolChars, maxCalls int, promptVersion string) *Compactor {
	level := LevelL2
	switch strings.ToUpper(maxLevel) {
	case "L1":
		level = LevelL1
	case "L3":
		level = LevelL3
	}
	if l1MaxToolChars <= 0 {
		l1MaxToolChars = 2000
	}
	if maxCalls <= 0 {
		maxCalls = 2
	}
	if promptVersion == "" {
		promptVersion = "v1"
	}
	return &Compactor{Summarizer: summarizer, MaxLevel: level, L1MaxToolChars: l1MaxToolChars, MaxLLMCalls: maxCalls, PromptVersion: promptVersion, cache: map[string]string{}}
}

func (c *Compactor) Compact(ctx context.Context, messages []Message, opts CompactOptions) CompactResult {
	if len(messages) == 0 {
		return CompactResult{LevelUsed: LevelL0}
	}
	unit := opts.Unit
	if unit == "" {
		unit = "turn"
	}
	keep := opts.KeepRecent
	if keep < 0 {
		keep = 0
	}
	groups := groupBy(messages, unit)
	if len(groups) <= keep {
		blocks := blocksForMessages(groups, LevelL0, opts.Segment)
		return CompactResult{Blocks: blocks, LevelUsed: LevelL0}
	}

	head := groups[:len(groups)-keep]
	tail := groups[len(groups)-keep:]
	headMessages := flatten(head)
	before := sumMessageTokens(headMessages) + sumMessageTokens(flatten(tail))

	// ① L1：零成本。先去掉噪声、截断过长的工具输出，**不删除消息**。
	headL1 := make([]Message, 0, len(headMessages))
	for _, m := range headMessages {
		mm := m
		mm.Content = denoise(mm.Content)
		if mm.Role == domain.RoleTool && len([]rune(mm.Content)) > c.L1MaxToolChars {
			runes := []rune(mm.Content)
			mm.Content = string(runes[:c.L1MaxToolChars]) + fmt.Sprintf("…[L1 截断：原文 %d 字符，这里只保留前 %d]", len(runes), c.L1MaxToolChars)
		}
		headL1 = append(headL1, mm)
	}
	blocks := blocksForMessages(groupBy(headL1, unit), LevelL1, opts.Segment)
	blocks = append(blocks, blocksForMessages(tail, LevelL0, opts.Segment)...)
	afterL1 := sumTokens(blocks)
	entries := []LedgerEntry{{
		Level: LevelL1, Segment: opts.Segment, Before: before, After: afterL1,
		Reason: opts.Reason, Note: "去噪 + 截断长工具输出（消息不删除，配对保持完整）",
	}}
	levelUsed := LevelL1
	entries[0].MessageIDs = messageIDs(headMessages)

	target := opts.TargetTokens
	if target <= 0 {
		return CompactResult{Blocks: blocks, LevelUsed: levelUsed, Entries: entries, Insufficient: true, Note: "没有给出目标 token 预算"}
	}
	if afterL1 <= target {
		return CompactResult{Blocks: blocks, LevelUsed: levelUsed, Entries: entries}
	}
	if c.MaxLevel == LevelL1 || c.Summarizer == nil {
		return CompactResult{Blocks: blocks, LevelUsed: levelUsed, Entries: entries,
			Insufficient: true, Note: fmt.Sprintf("L1 后 %d token 仍超过目标 %d", afterL1, target)}
	}

	// ② L2：把旧段交给 LLM 压成“已完成的操作与结论”。
	callsLeft := c.MaxLLMCalls
	summary, cacheKey, err := c.summarize(ctx, headMessages, strings.ToUpper(string(LevelL2)))
	if err == nil && strings.TrimSpace(summary) != "" {
		callsLeft--
		summaryBlock := Block{
			Content: summary, Role: domain.RoleUser, Segment: opts.Segment, Level: LevelL2,
			MessageIDs: messageIDs(headMessages), Note: opts.Reason,
		}
		blocks = append([]Block{summaryBlock}, blocksForMessages(tail, LevelL0, opts.Segment)...)
		afterL2 := sumTokens(blocks)
		entries = append(entries, LedgerEntry{
			Level: LevelL2, Segment: opts.Segment, MessageIDs: messageIDs(headMessages),
			Before: afterL1, After: afterL2, Reason: opts.Reason, CacheKey: cacheKey,
		})
		levelUsed = LevelL2
		if afterL2 <= target {
			return CompactResult{Blocks: blocks, LevelUsed: levelUsed, Entries: entries}
		}
		if c.MaxLevel == LevelL3 && callsLeft > 0 {
			digest, digestKey, derr := c.digest(ctx, summary)
			if derr == nil && strings.TrimSpace(digest) != "" {
				blocks = append([]Block{{
					Content: digest, Role: domain.RoleUser, Segment: opts.Segment, Level: LevelL3,
					MessageIDs: messageIDs(headMessages), Note: opts.Reason,
				}}, blocksForMessages(tail, LevelL0, opts.Segment)...)
				afterL3 := sumTokens(blocks)
				entries = append(entries, LedgerEntry{Level: LevelL3, Segment: opts.Segment,
					MessageIDs: messageIDs(headMessages), Before: afterL2, After: afterL3,
					Reason: opts.Reason, CacheKey: digestKey})
				levelUsed = LevelL3
				if afterL3 <= target {
					return CompactResult{Blocks: blocks, LevelUsed: levelUsed, Entries: entries}
				}
				return CompactResult{Blocks: blocks, LevelUsed: levelUsed, Entries: entries,
					Insufficient: true, Note: fmt.Sprintf("L3 后 %d token 仍超过目标 %d", afterL3, target)}
			}
		}
		return CompactResult{Blocks: blocks, LevelUsed: levelUsed, Entries: entries,
			Insufficient: true, Note: fmt.Sprintf("L2 后 %d token 仍超过目标 %d", afterL2, target)}
	}

	return CompactResult{Blocks: blocks, LevelUsed: levelUsed, Entries: entries,
		Insufficient: true, Note: "摘要失败，保持在 L1"}
}

func (c *Compactor) summarize(ctx context.Context, messages []Message, level string) (string, string, error) {
	var b strings.Builder
	for _, m := range messages {
		fmt.Fprintf(&b, "[%s] %s\n", m.Role, m.Content)
	}
	content := b.String()
	key := cacheKey(c.PromptVersion, level, content)
	c.mu.Lock()
	if cached, ok := c.cache[key]; ok {
		c.mu.Unlock()
		return cached, key, nil
	}
	c.mu.Unlock()

	system := "你是对话压缩器。只保留已完成的操作、结论、仍未解决的问题与关键事实；不要编造，不要丢时间顺序。"
	prompt := "把下面这段早期的 Agent 执行记录压缩成简明摘要：\n\n" + content
	summary, err := c.Summarizer.Complete(ctx, system, prompt, 512)
	if err != nil {
		return "", key, err
	}
	c.mu.Lock()
	if len(c.cache) > 512 {
		c.cache = map[string]string{}
	}
	c.cache[key] = summary
	c.mu.Unlock()
	return summary, key, nil
}

func (c *Compactor) digest(ctx context.Context, summary string) (string, string, error) {
	key := cacheKey(c.PromptVersion, "L3", summary)
	c.mu.Lock()
	if cached, ok := c.cache[key]; ok {
		c.mu.Unlock()
		return cached, key, nil
	}
	c.mu.Unlock()
	system := "你是摘要提炼器。把摘要再压成不超过 8 条要点，只保留结论与未决问题。"
	out, err := c.Summarizer.Complete(ctx, system, summary, 256)
	if err != nil {
		return "", key, err
	}
	c.mu.Lock()
	c.cache[key] = out
	c.mu.Unlock()
	return out, key, nil
}

func cacheKey(version, level, content string) string {
	sum := sha256.Sum256([]byte(version + "\x00" + level + "\x00" + content))
	return hex.EncodeToString(sum[:16])
}

func blocksForMessages(groups [][]Message, level Level, segment Segment) []Block {
	var out []Block
	for _, group := range groups {
		for i := range group {
			m := group[i]
			copied := m
			out = append(out, Block{Content: m.Content, Role: m.Role, Segment: segment, Level: level, Message: &copied, MessageIDs: []string{m.MessageID}})
		}
	}
	return out
}

func denoise(text string) string {
	for strings.Contains(text, "\n\n\n") {
		text = strings.ReplaceAll(text, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(text)
}

func sumMessageTokens(messages []Message) int {
	total := 0
	for _, m := range messages {
		total += EstimateTokens(m.Content)
	}
	return total
}

func messageIDs(messages []Message) []string {
	out := make([]string, 0, len(messages))
	for _, m := range messages {
		if m.MessageID != "" {
			out = append(out, m.MessageID)
		}
	}
	return out
}
