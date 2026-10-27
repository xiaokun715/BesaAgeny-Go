package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/besa-ai/besa-agent/internal/foundation/errs"
)

// Error 是所有上游错误的统一形状。
//
// Retryable 是 gateway 唯一需要读的字段：网络层故障与 429/5xx 为真，
// 其余 4xx 为假。**provider 自己只在传输层重试网络错误**，HTTP 状态码一律上抛 ——
// 跨模型的降级决策必须由 gateway 做，两层的重试相乘会形成重试风暴。
type Error struct {
	Kind       string // network | timeout | http | parse | capability | config
	StatusCode int
	Retryable  bool
	Provider   string
	Model      string
	Message    string
	Raw        string
	TraceID    string
	Err        error
}

func (e *Error) Error() string {
	parts := []string{}
	if e.Provider != "" {
		parts = append(parts, e.Provider)
	}
	if e.Model != "" {
		parts = append(parts, e.Model)
	}
	head := strings.Join(parts, "/")
	msg := e.Message
	if head != "" {
		msg = head + ": " + msg
	}
	if e.StatusCode > 0 {
		msg = fmt.Sprintf("%s (HTTP %d)", msg, e.StatusCode)
	}
	return errs.Redact(msg)
}
func (e *Error) Unwrap() error { return e.Err }

func (e *Error) WithTrace(traceID string) *Error {
	c := *e
	c.TraceID = traceID
	return &c
}

// CapabilityError 表示本地能力校验失败 —— **零网络调用**。
type CapabilityError struct {
	Model   string
	Missing []string
	Need    string
}

func (e *CapabilityError) Error() string {
	return fmt.Sprintf("模型 %s 不支持 %s（缺少能力：%s）", e.Model, e.Need, strings.Join(e.Missing, ", "))
}

func IsRetryable(err error) bool {
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Retryable
	}
	return false
}

// ClassifyNetwork 判断传输层错误是否可重试。仅连接失败 / 读超时。
func ClassifyNetwork(err error) bool {
	if err == nil {
		return false
	}
	// 单次请求预算到期属于网络层超时，可重试；调用方主动取消则不可。
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		var inner net.Error
		if errors.As(urlErr.Err, &inner) {
			return inner.Timeout()
		}
		s := urlErr.Err.Error()
		return strings.Contains(s, "connection reset") ||
			strings.Contains(s, "connection refused") ||
			strings.Contains(s, "broken pipe") ||
			strings.Contains(s, "unexpected EOF")
	}
	s := err.Error()
	return strings.Contains(s, "connection reset") ||
		strings.Contains(s, "connection refused") ||
		strings.Contains(s, "broken pipe") ||
		strings.Contains(s, "unexpected EOF")
}

// StatusRetryable：429 与 5xx 可交给 gateway 跨模型决策。
func StatusRetryable(status int) bool {
	return status == 429 || status == 408 || status >= 500
}
