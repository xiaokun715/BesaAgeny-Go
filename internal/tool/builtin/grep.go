package builtin

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/besa-ai/besa-agent/internal/tool"
)

var skipDirs = map[string]bool{
	".git": true, "__pycache__": true, ".mypy_cache": true, ".pytest_cache": true,
	".ruff_cache": true, "node_modules": true, ".venv": true, "vendor": true,
}

const maxFileBytes = 2 * 1024 * 1024
const defaultMaxMatches = 200

type Grep struct{}

func NewGrep() *Grep { return &Grep{} }

func (g *Grep) Name() string { return "grep" }

func (g *Grep) Definition() tool.Definition {
	return tool.Definition{
		Name:        "grep",
		Description: "按正则搜索文件内容。返回 文件:行号: 内容。结果有上限，被截断时会明确标注。",
		SideEffect:  string(tool.SideEffectRead),
		Category:    "infra",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern":     map[string]any{"type": "string", "description": "正则表达式"},
				"path":        map[string]any{"type": "string", "description": "搜索根（省略则用第一个可读范围）"},
				"glob":        map[string]any{"type": "string", "description": "文件名过滤，如 *.go"},
				"max_matches": map[string]any{"type": "integer", "description": "命中上限"},
			},
			"required": []string{"pattern"},
		},
	}
}

func (g *Grep) Run(_ context.Context, args map[string]any, tc *tool.Context) tool.Result {
	patternText, bad := tool.RequireText(args, "pattern", g.Name())
	if bad != nil {
		return *bad
	}
	pattern, err := regexp.Compile(patternText)
	if err != nil {
		return tool.Result{Outcome: tool.OutcomeRefused, ToolName: g.Name(), Error: "正则非法：" + err.Error(), ErrorKind: tool.ErrorParam, Recovery: tool.RecoveryAdvise}
	}
	root := ""
	if v, ok := args["path"].(string); ok && v != "" {
		if filepath.IsAbs(v) {
			root = v
		} else if len(tc.AllowedPaths) > 0 {
			root = filepath.Join(tc.AllowedPaths[0], v)
		} else {
			root = v
		}
	} else if len(tc.AllowedPaths) > 0 {
		root = tc.AllowedPaths[0]
	} else {
		root = "."
	}
	root = tool.ResolvePath(root)
	if !tc.Within(root) {
		allowed := strings.Join(tc.AllowedPaths, ", ")
		return tool.Result{Outcome: tool.OutcomeRefused, ToolName: g.Name(), Error: "搜索根超出可读范围：" + root + "\n可读范围：" + allowed, ErrorKind: tool.ErrorFatal, Recovery: tool.RecoveryReport}
	}

	glob, _ := args["glob"].(string)
	limit := tool.IntArg(args, "max_matches", defaultMaxMatches)
	if limit <= 0 {
		limit = defaultMaxMatches
	}
	var matches []string
	truncated := false
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if len(matches) >= limit {
			truncated = true
			return filepath.SkipAll
		}
		if glob != "" {
			if ok, _ := filepath.Match(glob, d.Name()); !ok {
				return nil
			}
		}
		info, err := d.Info()
		if err != nil || info.Size() > maxFileBytes {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
		for i, line := range lines {
			if pattern.MatchString(line) {
				matches = append(matches, fmt.Sprintf("%s:%d: %s", path, i+1, strings.TrimRight(line, " \t")))
				if len(matches) >= limit {
					truncated = true
					return filepath.SkipAll
				}
			}
		}
		return nil
	})
	if walkErr != nil {
		rec := tool.ClassifyError(walkErr, walkErr.Error(), tool.SideEffectRead)
		return tool.Result{Outcome: tool.OutcomeFailed, ToolName: g.Name(), Error: "搜索失败：" + walkErr.Error(), ErrorKind: rec.Kind, Recovery: rec.Action}
	}
	body, cut := tool.Truncate(strings.Join(matches, "\n"), tc.MaxOutputBytes, tc.MaxOutputLines)
	return tool.Result{Outcome: tool.OutcomeExecuted, ToolName: g.Name(), Output: body, Truncated: truncated || cut}
}
