package tool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/besa-ai/besa-agent/internal/foundation/errs"
)

// SpillStore 是大结果的对象存储出口。分布式部署注入 S3/MinIO；
// 单机退回本地 .tool_out 目录。
type SpillStore interface {
	Put(ctx context.Context, name string, data []byte, contentType string) (uri string, err error)
}

type ResultProcessor struct {
	Redact          bool
	SpillThreshold  int
	SpillQuotaBytes int64
	SpillDir        string
	SpillTTL        time.Duration
	Store           SpillStore
	mu              sync.Mutex
}

func NewResultProcessor(limits Limits, redact bool) *ResultProcessor {
	return &ResultProcessor{
		Redact: redact, SpillThreshold: limits.SpillThreshold,
		SpillQuotaBytes: int64(limits.SpillQuotaMB) * 1024 * 1024,
		SpillDir:        limits.SpillDir, SpillTTL: limits.SpillTTL,
	}
}

func (p *ResultProcessor) Process(ctx context.Context, result Result, tc *Context, idemKey string) Result {
	if p.Redact && result.Output != "" {
		redacted := errs.Redact(result.Output)
		if redacted != result.Output {
			result.Redacted = true
			result.Output = redacted
		}
		if result.Error != "" {
			result.Error = errs.Redact(result.Error)
		}
	}
	if !result.OK() || result.Output == "" {
		return result
	}

	threshold := p.SpillThreshold
	if threshold <= 0 {
		threshold = DefaultLimits().SpillThreshold
	}
	if len(result.Output) > threshold {
		uri, err := p.spill(ctx, result.ToolName, []byte(result.Output), idemKey, tc)
		if err == nil {
			preview := previewLines(result.Output, 40)
			totalLines := strings.Count(result.Output, "\n") + 1
			result.Output = fmt.Sprintf("%s\n…[全文共 %d 行 / %d 字节，已写入 %s]", preview, totalLines, len(result.Output), uri)
			result.SpilledPath = uri
			result.Truncated = false
			return result
		}
		// 落盘失败 → 明说，不静默降级。
		result.Output = fmt.Sprintf("[本该落盘但没能落：%v。下面是截断后的输出]\n%s", err, result.Output)
	}

	maxBytes, maxLines := tc.MaxOutputBytes, tc.MaxOutputLines
	if maxBytes <= 0 {
		maxBytes = DefaultLimits().MaxOutputBytes
	}
	if maxLines <= 0 {
		maxLines = DefaultLimits().MaxOutputLines
	}
	out, truncated := Truncate(result.Output, maxBytes, maxLines)
	result.Output = out
	result.Truncated = result.Truncated || truncated
	return result
}

func (p *ResultProcessor) spill(ctx context.Context, toolName string, data []byte, idemKey string, tc *Context) (string, error) {
	name := fmt.Sprintf("%s-%s-%s.txt", toolName, time.Now().Format("20060102-150405"), tail(idemKey, 24))
	if p.Store != nil {
		return p.Store.Put(ctx, name, data, "text/plain; charset=utf-8")
	}
	dir := p.SpillDir
	if dir == "" && len(tc.AllowedPaths) > 0 {
		dir = filepath.Join(tc.AllowedPaths[0], ".tool_out")
	}
	if dir == "" {
		return "", fmt.Errorf("没有可用的落盘目录")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if p.SpillQuotaBytes > 0 {
		if used := dirSize(dir); used+int64(len(data)) > p.SpillQuotaBytes {
			return "", fmt.Errorf("落盘配额已满（%d/%d 字节）", used, p.SpillQuotaBytes)
		}
	}
	p.cleanup(dir)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func (p *ResultProcessor) cleanup(dir string) {
	ttl := p.SpillTTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-ttl)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".txt") {
			continue
		}
		info, err := e.Info()
		if err == nil && info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

func dirSize(dir string) int64 {
	var total int64
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil && !e.IsDir() {
			total += info.Size()
		}
	}
	return total
}

func previewLines(text string, n int) string {
	lines := strings.Split(text, "\n")
	if len(lines) <= n {
		return text
	}
	return strings.Join(lines[:n], "\n")
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// ------------------------------ 失败分类与自愈 ------------------------------

type Recovery struct {
	Kind   ErrorKind
	Action RecoveryAction
	Hint   string
}

func ClassifyError(err error, message string, sideEffect SideEffect) Recovery {
	msg := strings.ToLower(message)
	switch {
	case containsAny(msg, "schema", "参数", "缺少必需", "不能为空", "超出", "不在", "范围", "path_traversal", "denied_path", "invalid"):
		return Recovery{Kind: ErrorParam, Action: RecoveryAdvise, Hint: "修正参数后重试；不要用同一份参数重复提交。"}
	case containsAny(msg, "超时", "timeout", "连接", "connection", "502", "503", "504", "unavailable"):
		if sideEffect == SideEffectRead {
			return Recovery{Kind: ErrorTransient, Action: RecoveryRetryOnce, Hint: "这是瞬时故障；只读操作可以重试一次。"}
		}
		return Recovery{Kind: ErrorTransient, Action: RecoveryAdvise, Hint: "瞬时故障，但写操作不自动重试（可能重复产生副作用）。"}
	case containsAny(msg, "not found", "不存在", "read-only", "只读", "permission denied", "no such file"):
		return Recovery{Kind: ErrorEnvironment, Action: RecoverySwitch, Hint: "检查路径/环境状态，或换一个等价的工具/路径。"}
	case containsAny(msg, "未授权", "未启用", "拒绝", "refused", "循环", "深度"):
		return Recovery{Kind: ErrorFatal, Action: RecoveryReport, Hint: "这一条不会被自动重试；需要修改配置或调用方式。"}
	}
	return Recovery{Kind: ErrorUnknown, Action: RecoveryReport}
}

func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// RetryBreaker：同一份参数连续失败达到阈值后直接拒绝。
// 它独立于幂等 —— 幂等挡“重复执行”，它挡“无效重试”。
type RetryBreaker struct {
	mu        sync.Mutex
	threshold int
	failures  map[string]int
}

func NewRetryBreaker(threshold int) *RetryBreaker {
	if threshold <= 0 {
		threshold = 3
	}
	return &RetryBreaker{threshold: threshold, failures: map[string]int{}}
}

func (b *RetryBreaker) ShouldRefuse(fingerprint string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.failures[fingerprint] >= b.threshold
}

func (b *RetryBreaker) Count(fingerprint string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.failures[fingerprint]
}

func (b *RetryBreaker) RecordFailure(fingerprint string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures[fingerprint]++
}

func (b *RetryBreaker) Clear(fingerprint string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.failures, fingerprint)
}
