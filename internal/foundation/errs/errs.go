// Package errs 定义跨模块错误码与脱敏工具。
//
// 传输层用 Code() 决定 gRPC/HTTP 状态；业务层用 errors.Is/As 判断类别。
// 错误信息不得包含 API Key —— Redact 是所有对外错误文本的最后一道关。
package errs

import (
	"errors"
	"fmt"
	"regexp"
)

type Code string

const (
	CodeInvalid      Code = "invalid"
	CodeNotFound     Code = "not_found"
	CodeConflict     Code = "conflict"
	CodeUnauthorized Code = "unauthorized"
	CodeForbidden    Code = "forbidden"
	CodeUnavailable  Code = "unavailable"
	CodeRateLimited  Code = "rate_limited"
	CodeBudget       Code = "budget_exhausted"
	CodeUpstream     Code = "upstream"
	CodeInternal     Code = "internal"
	CodeCancelled    Code = "cancelled"
)

type Error struct {
	Code    Code
	Message string
	Op      string
	Err     error
}

func (e *Error) Error() string {
	if e.Op == "" {
		return e.Message
	}
	return e.Op + ": " + e.Message
}
func (e *Error) Unwrap() error { return e.Err }

func E(code Code, msg string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(msg, args...)}
}

func Wrap(code Code, err error, msg string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(msg, args...), Err: err}
}

func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return CodeInternal
}

func IsCode(err error, code Code) bool { return CodeOf(err) == code }

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-[A-Za-z0-9_\-]{8,}`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`(?i)(authorization:\s*bearer\s+)[^\s]+`),
	regexp.MustCompile(`(?i)(api[_-]?key\s*[=:]\s*)[^\s,;]+`),
	regexp.MustCompile(`-----BEGIN [A-Z ]+-----[\s\S]*?-----END [A-Z ]+-----`),
}

// Redact 抹掉疑似密钥。**这是尽力而为**，不是安全边界。
func Redact(s string) string {
	for _, re := range secretPatterns {
		s = re.ReplaceAllStringFunc(s, func(m string) string {
			if sub := re.FindStringSubmatch(m); len(sub) > 1 && sub[1] != "" {
				return sub[1] + "***"
			}
			return "***"
		})
	}
	return s
}
