package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/besa-ai/besa-agent/internal/tool"
)

type Write struct{}

func NewWrite() *Write { return &Write{} }

func (w *Write) Name() string { return "write" }

func (w *Write) Definition() tool.Definition {
	return tool.Definition{
		Name:        "write",
		Description: "写入文件。mode=overwrite 覆盖，mode=append 追加（默认 overwrite）。目标必须在可写范围内。",
		SideEffect:  string(tool.SideEffectWrite),
		Category:    "infra",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":        map[string]any{"type": "string", "description": "目标文件路径"},
				"content":     map[string]any{"type": "string", "description": "要写入的内容"},
				"mode":        map[string]any{"type": "string", "enum": []string{"overwrite", "append"}},
				"create_dirs": map[string]any{"type": "boolean", "description": "是否自动创建父目录"},
			},
			"required": []string{"path", "content"},
		},
	}
}

func (w *Write) Run(_ context.Context, args map[string]any, tc *tool.Context) tool.Result {
	raw, bad := tool.RequireText(args, "path", w.Name())
	if bad != nil {
		return *bad
	}
	content, _ := args["content"].(string)
	mode := "overwrite"
	if m, ok := args["mode"].(string); ok && m != "" {
		mode = strings.ToLower(m)
	}
	if mode != "overwrite" && mode != "append" {
		return tool.Result{Outcome: tool.OutcomeRefused, ToolName: w.Name(), Error: "未知的 mode=" + mode + "；已知：append, overwrite", ErrorKind: tool.ErrorParam, Recovery: tool.RecoveryAdvise}
	}

	target := raw
	if len(tc.AllowedPaths) > 0 && !filepath.IsAbs(raw) {
		target = filepath.Join(tc.AllowedPaths[0], raw)
	}
	// 父目录必须先解析：否则 `可写目录/link -> /etc/` 能逃出范围。
	target = filepath.Join(tool.ResolvePath(filepath.Dir(target)), filepath.Base(target))
	if !tc.Within(target) {
		allowed := strings.Join(tc.AllowedPaths, ", ")
		return tool.Result{Outcome: tool.OutcomeRefused, ToolName: w.Name(), Error: "路径超出可写范围：" + target + "\n可写范围：" + allowed + "\n（范围默认拒绝：没配就是不许访问）", ErrorKind: tool.ErrorFatal, Recovery: tool.RecoveryReport}
	}
	if create, _ := args["create_dirs"].(bool); create {
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			rec := tool.ClassifyError(err, err.Error(), tool.SideEffectWrite)
			return tool.Result{Outcome: tool.OutcomeFailed, ToolName: w.Name(), Error: "创建目录失败：" + err.Error(), ErrorKind: rec.Kind, Recovery: rec.Action}
		}
	}

	var err error
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if mode == "append" {
		flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	f, err := os.OpenFile(target, flags, 0o644)
	if err == nil {
		_, err = f.WriteString(content)
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
	}
	if err != nil {
		rec := tool.ClassifyError(err, err.Error(), tool.SideEffectWrite)
		return tool.Result{Outcome: tool.OutcomeFailed, ToolName: w.Name(), Error: "写入失败：" + err.Error(), ErrorKind: rec.Kind, Recovery: rec.Action, Hint: rec.Hint}
	}
	return tool.Result{Outcome: tool.OutcomeExecuted, ToolName: w.Name(), Output: "已" + mode + " " + target + "（" + itoa(len([]byte(content))) + " 字节）"}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
