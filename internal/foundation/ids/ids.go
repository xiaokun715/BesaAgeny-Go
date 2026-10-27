// Package ids 生成可读的业务标识，并从 context 传播 trace_id。
package ids

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"
)

type traceKey struct{}

var sanitize = regexp.MustCompile(`[^a-z0-9]+`)

// New 生成 "<prefix>_<unixmilli>_<8hex>" 形式的 id。
func New(prefix string) string {
	buf := make([]byte, 4)
	_, _ = rand.Read(buf)
	return fmt.Sprintf("%s_%d_%s", prefix, time.Now().UnixMilli(), hex.EncodeToString(buf))
}

// Token 生成 32 位十六进制随机串（幂等 owner、事件 id）。
func Token() string {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// Slug 把名字规整成可做 id 前缀的形状。
func Slug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = sanitize.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

// WithTrace 把 trace_id 放进 context。
func WithTrace(ctx context.Context, traceID string) context.Context {
	return context.WithValue(ctx, traceKey{}, traceID)
}

// Trace 取当前 trace_id；没有则返回空串（不是 "-"，日志层负责兜底）。
func Trace(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(traceKey{}).(string)
	return v
}

// EnsureTrace 保证 context 里有 trace_id。
func EnsureTrace(ctx context.Context) (context.Context, string) {
	if id := Trace(ctx); id != "" {
		return ctx, id
	}
	id := New("trace")
	return WithTrace(ctx, id), id
}
