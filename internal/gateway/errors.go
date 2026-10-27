package gateway

import (
	"fmt"
	"strings"

	"github.com/besa-ai/besa-agent/internal/domain"
	"github.com/besa-ai/besa-agent/internal/foundation/errs"
)

func errInvalid(msg string) error { return &errs.Error{Code: errs.CodeInvalid, Message: msg} }

type UnknownAliasError struct {
	Alias     string
	Available []string
}

func (e *UnknownAliasError) Error() string {
	return fmt.Sprintf("未知的逻辑名 %q（可用：%s）", e.Alias, strings.Join(e.Available, ", "))
}

type NoCapableModelError struct {
	Alias   string
	Missing map[string]string // model key → 原因
}

func (e *NoCapableModelError) Error() string {
	parts := make([]string, 0, len(e.Missing))
	for k, reason := range e.Missing {
		parts = append(parts, k+": "+reason)
	}
	return fmt.Sprintf("逻辑名 %q 没有满足本次请求能力的模型（%s）", e.Alias, strings.Join(parts, "；"))
}

type AllCandidatesFailedError struct {
	Alias    string
	Attempts []domain.AttemptRecord
	Reason   string
}

func (e *AllCandidatesFailedError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "逻辑名 %q 的所有候选均失败（%s）", e.Alias, e.Reason)
	for _, a := range e.Attempts {
		if a.Outcome == "skipped" {
			fmt.Fprintf(&b, "\n- %s 跳过：%s", a.ModelKey, a.SkippedReason)
		} else if a.Outcome == "failed" {
			fmt.Fprintf(&b, "\n- %s 失败：%s", a.ModelKey, a.Error)
		}
	}
	return b.String()
}

type BudgetExhaustedError struct {
	Alias    string
	Reason   string // attempts | deadline
	Attempts []domain.AttemptRecord
}

func (e *BudgetExhaustedError) Error() string {
	label := map[string]string{"attempts": "重试次数", "deadline": "时间预算"}[e.Reason]
	return fmt.Sprintf("逻辑名 %q 的%s已耗尽", e.Alias, label)
}

type StreamCommittedError struct {
	Alias   string
	Message string
}

func (e *StreamCommittedError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("逻辑名 %q 的流式响应已输出正文，无法重试或降级", e.Alias)
}

// ErrCode 供传输层映射 gRPC/HTTP 状态，不改变错误语义。
type ErrCode string

const (
	CodeUnknownAlias ErrCode = "unknown_alias"
	CodeNoCapable    ErrCode = "no_capable_model"
	CodeBudget       ErrCode = "budget_exhausted"
	CodeAllFailed    ErrCode = "all_candidates_failed"
	CodeStream       ErrCode = "stream_committed"
	CodeInternal     ErrCode = "internal"
)

func CodeOf(err error) ErrCode {
	if err == nil {
		return ""
	}
	switch err.(type) {
	case *UnknownAliasError:
		return CodeUnknownAlias
	case *NoCapableModelError:
		return CodeNoCapable
	case *BudgetExhaustedError:
		return CodeBudget
	case *AllCandidatesFailedError:
		return CodeAllFailed
	case *StreamCommittedError:
		return CodeStream
	}
	return CodeInternal
}
