// Package builtin 提供内置工具：read / grep / write / bash。
//
// 每个工具自己声明副作用等级与类别；权限与幂等由 Executor 统一处理。
package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/besa-ai/besa-agent/internal/tool"
)

type Read struct{}

func NewRead() *Read { return &Read{} }

func (r *Read) Name() string { return "read" }

func (r *Read) Definition() tool.Definition {
	return tool.Definition{
		Name:        "read",
		Description: "读取文件内容。可选按行范围读（1 起、闭区间）。输出超过上限会被截断，且会明确标注。",
		SideEffect:  string(tool.SideEffectRead),
		Category:    "infra",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":       map[string]any{"type": "string", "description": "文件路径"},
				"start_line": map[string]any{"type": "integer", "description": "起始行（1 起，可省略）"},
				"end_line":   map[string]any{"type": "integer", "description": "结束行（闭区间，可省略）"},
			},
			"required": []string{"path"},
		},
	}
}

func (r *Read) Run(_ context.Context, args map[string]any, tc *tool.Context) tool.Result {
	raw, bad := tool.RequireText(args, "path", r.Name())
	if bad != nil {
		return *bad
	}
	target := raw
	if len(tc.AllowedPaths) > 0 && !filepath.IsAbs(raw) {
		target = filepath.Join(tc.AllowedPaths[0], raw)
	}
	target = tool.ResolvePath(target)
	if !tc.Within(target) {
		allowed := strings.Join(tc.AllowedPaths, ", ")
		if allowed == "" {
			allowed = "（未配置任何可读范围）"
		}
		return tool.Result{Outcome: tool.OutcomeRefused, ToolName: r.Name(), Error: "路径超出可读范围：" + target + "\n可读范围：" + allowed + "\n（范围默认拒绝：没配就是不许访问）", ErrorKind: tool.ErrorFatal, Recovery: tool.RecoveryReport}
	}
	info, err := os.Stat(target)
	if err != nil || info.IsDir() {
		return tool.Result{Outcome: tool.OutcomeRefused, ToolName: r.Name(), Error: "不是一个文件：" + target, ErrorKind: tool.ErrorParam, Recovery: tool.RecoveryAdvise}
	}
	data, err := os.ReadFile(target)
	if err != nil {
		rec := tool.ClassifyError(err, err.Error(), tool.SideEffectRead)
		return tool.Result{Outcome: tool.OutcomeFailed, ToolName: r.Name(), Error: "读取失败：" + err.Error(), ErrorKind: rec.Kind, Recovery: rec.Action, Hint: rec.Hint}
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.SplitAfter(text, "\n")
	start := tool.IntArg(args, "start_line", 1)
	end := tool.IntArg(args, "end_line", 0)
	if start > 1 || end > 0 {
		from := max(0, start-1)
		to := len(lines)
		if end > 0 {
			to = min(end, len(lines))
		}
		if from < to {
			text = strings.Join(lines[from:to], "")
		} else {
			text = ""
		}
	}
	out, truncated := tool.Truncate(text, tc.MaxOutputBytes, tc.MaxOutputLines)
	return tool.Result{Outcome: tool.OutcomeExecuted, ToolName: r.Name(), Output: out, Truncated: truncated}
}
