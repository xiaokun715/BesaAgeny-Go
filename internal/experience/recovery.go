package experience

import "fmt"

// Decide：重试不是默认动作；预算/次数到顶必须换动作；高风险即使可自动恢复也要确认。
func Decide(in RecoveryInput) RecoveryDecision {
	category, recover := Classify(Failure{ErrorType: in.ErrorType, Message: in.ErrorType, Retryable: boolPtr(true)})
	_ = category
	reasonParts := []string{
		fmt.Sprintf("error_type=%s", in.ErrorType),
		fmt.Sprintf("retry_count=%d/%d", in.RetryCount, in.MaxRetries),
		fmt.Sprintf("risk=%s", in.Risk),
	}
	if in.RemainingBudget != nil {
		reasonParts = append(reasonParts, fmt.Sprintf("remaining_budget=%.2f", *in.RemainingBudget))
	}
	reason := "依据：" + join(reasonParts, "；")

	if in.Risk == "high" {
		return RecoveryDecision{Action: ActionRequestAuth, Reason: reason + "；高风险动作需要确认"}
	}
	if in.RetryCount >= in.MaxRetries {
		return RecoveryDecision{Action: ActionAbort, Reason: reason + "；重试次数已到顶"}
	}
	if in.RemainingBudget != nil && *in.RemainingBudget <= 0 {
		return RecoveryDecision{Action: ActionAbort, Reason: reason + "；预算已耗尽"}
	}
	switch recover {
	case RecoverAuto:
		if containsAny(in.ErrorType, "环境", "environment", "初始化", "ready") {
			return RecoveryDecision{Action: ActionWaitRecheck, Reason: reason + "；先等待再复查环境状态"}
		}
		return RecoveryDecision{Action: ActionRetry, Reason: reason + "；错误可自动恢复"}
	case RecoverExternal:
		if containsAny(in.ErrorType, "auth", "授权", "权限") {
			return RecoveryDecision{Action: ActionRequestAuth, Reason: reason + "；需要申请授权"}
		}
		return RecoveryDecision{Action: ActionAdjust, Reason: reason + "；需要外部动作或修正输入"}
	default:
		return RecoveryDecision{Action: ActionAbort, Reason: reason + "；不可恢复，停止而不是重复消耗"}
	}
}

func boolPtr(v bool) *bool { return &v }

func join(items []string, sep string) string {
	out := ""
	for i, item := range items {
		if i > 0 {
			out += sep
		}
		out += item
	}
	return out
}
