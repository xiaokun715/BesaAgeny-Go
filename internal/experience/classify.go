package experience

import "strings"

// Classify 一级类别走规则表；可恢复性独立判定，基于证据而不是类别。
func Classify(f Failure) (ErrorCategory, Recoverability) {
	text := strings.ToLower(f.ErrorType + " " + f.Message + " " + f.Stage)
	var category ErrorCategory
	switch {
	case containsAny(text, "requirement", "需求", "ambigu", "歧义", "constraint"):
		category = CatRequirement
	case containsAny(text, "retrieval", "检索", "recall", "召回", "version_mismatch"):
		category = CatRetrieval
	case containsAny(text, "test_case", "用例", "schema", "format", "coverage"):
		category = CatTestCase
	case containsAny(text, "environment", "环境", "resource", "resource_unavailable", "version"):
		category = CatEnvironment
	case containsAny(text, "tool", "工具", "permission", "timeout", "参数"):
		category = CatTool
	case containsAny(text, "assert", "断言", "test_failure", "被测"):
		category = CatTestFailure
	case containsAny(text, "diagnosis", "诊断", "evidence", "hypothesis"):
		category = CatDiagnosis
	case containsAny(text, "workflow", "orchestration", "dag", "dependency", "编排"):
		category = CatOrchestration
	default:
		category = CatTool
	}
	// 可恢复性看证据与错误类型，不看类别。
	var recover Recoverability
	switch {
	case f.Retryable != nil && *f.Retryable:
		recover = RecoverAuto
	case containsAny(text, "auth", "授权", "权限", "requires approval", "wait", "初始化"):
		recover = RecoverExternal
	case containsAny(text, "fatal", "unrecoverable", "需求冲突", "contradiction"):
		recover = RecoverUnrecoverable
	default:
		recover = RecoverExternal
	}
	return category, recover
}

func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}
