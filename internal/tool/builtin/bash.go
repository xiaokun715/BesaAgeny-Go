package builtin

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/besa-ai/besa-agent/internal/tool"
)

// 最低限度的兜底。**它不是安全边界** —— 真正的边界是：
// 默认关闭 + destructive 需要显式授权 + 超时/输出上限 + 执行记录可审计。
var denylist = []string{
	"rm -rf /", "rm -rf /*", "mkfs", "dd if=/dev/zero", ":(){ :|:& };:", "> /dev/sda",
}

type Bash struct{}

func NewBash() *Bash { return &Bash{} }

func (b *Bash) Name() string { return "bash" }

func (b *Bash) Definition() tool.Definition {
	return tool.Definition{
		Name:        "bash",
		Description: "执行 shell 命令并返回输出。有超时与输出上限。**这个工具能改变系统状态，默认不启用。**",
		SideEffect:  string(tool.SideEffectDestructive),
		Category:    "infra",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command":   map[string]any{"type": "string", "description": "要执行的命令"},
				"cwd":       map[string]any{"type": "string", "description": "工作目录（必须在可写范围内）"},
				"timeout_s": map[string]any{"type": "number", "description": "超时秒数"},
			},
			"required": []string{"command"},
		},
	}
}

func (b *Bash) Run(ctx context.Context, args map[string]any, tc *tool.Context) tool.Result {
	command, bad := tool.RequireText(args, "command", b.Name())
	if bad != nil {
		return *bad
	}
	for _, badCmd := range denylist {
		if strings.Contains(command, badCmd) {
			return tool.Result{Outcome: tool.OutcomeRefused, ToolName: b.Name(), Error: fmt.Sprintf(
				"命令命中拒绝清单（%q）。\n注意：拒绝清单**不是安全边界**，它只挡手滑。", badCmd), ErrorKind: tool.ErrorFatal, Recovery: tool.RecoveryReport}
		}
	}

	cwd := ""
	if raw, ok := args["cwd"].(string); ok && raw != "" {
		candidate := raw
		if !filepath.IsAbs(raw) && len(tc.AllowedPaths) > 0 {
			candidate = filepath.Join(tc.AllowedPaths[0], raw)
		}
		candidate = tool.ResolvePath(candidate)
		if !tc.Within(candidate) {
			return tool.Result{Outcome: tool.OutcomeRefused, ToolName: b.Name(), Error: "工作目录超出授权范围：" + candidate, ErrorKind: tool.ErrorFatal, Recovery: tool.RecoveryReport}
		}
		cwd = candidate
	} else if len(tc.AllowedPaths) > 0 {
		cwd = tc.AllowedPaths[0]
	}

	timeout := tc.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	if secs := tool.IntArg(args, "timeout_s", 0); secs > 0 && time.Duration(secs)*time.Second < timeout {
		timeout = time.Duration(secs) * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(runCtx, "cmd", "/C", command)
	} else {
		cmd = exec.CommandContext(runCtx, "/bin/sh", "-c", command)
	}
	if cwd != "" {
		cmd.Dir = cwd
	}
	cmd.Env = cleanEnv()

	maxBytes := tc.MaxOutputBytes
	if maxBytes <= 0 {
		maxBytes = 262144
	}
	out := &limitedBuffer{max: maxBytes, cancel: cancel}
	cmd.Stdout = out
	cmd.Stderr = out

	started := time.Now()
	err := cmd.Run()
	elapsed := time.Since(started)
	text := out.String()

	if out.limitHit {
		return tool.Result{Outcome: tool.OutcomeFailed, ToolName: b.Name(), Output: text, Error: fmt.Sprintf(
			"命令输出超过上限（%d 字节），已终止。注意：终止前它可能已经产生了副作用。", maxBytes),
			Truncated: true, Elapsed: elapsed, ErrorKind: tool.ErrorTransient, Recovery: tool.RecoveryAdvise}
	}
	if runCtx.Err() == context.DeadlineExceeded {
		return tool.Result{Outcome: tool.OutcomeFailed, ToolName: b.Name(), Output: text, Error: fmt.Sprintf(
			"命令超时（%s），已终止。**注意：超时前它可能已经产生了副作用。**", timeout),
			Elapsed: elapsed, ErrorKind: tool.ErrorTransient, Recovery: tool.RecoveryAdvise}
	}
	if err != nil {
		code := -1
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		}
		rec := tool.ClassifyError(err, text+" "+err.Error(), tool.SideEffectDestructive)
		return tool.Result{Outcome: tool.OutcomeFailed, ToolName: b.Name(), Output: text,
			Error: fmt.Sprintf("命令以退出码 %d 结束", code), Elapsed: elapsed,
			ErrorKind: rec.Kind, Recovery: rec.Action, Hint: rec.Hint}
	}
	body, truncated := tool.Truncate(text, maxBytes, tc.MaxOutputLines)
	return tool.Result{Outcome: tool.OutcomeExecuted, ToolName: b.Name(), Output: body, Truncated: truncated || out.limitHit, Elapsed: elapsed}
}

type limitedBuffer struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	max      int
	written  int
	limitHit bool
	cancel   context.CancelFunc
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.limitHit {
		return len(p), nil
	}
	remaining := l.max - l.written
	if remaining <= 0 {
		l.limitHit = true
		if l.cancel != nil {
			l.cancel()
		}
		return len(p), nil
	}
	if len(p) > remaining {
		l.buf.Write(p[:remaining])
		l.written = l.max
		l.limitHit = true
		if l.cancel != nil {
			l.cancel()
		}
		return len(p), nil
	}
	l.buf.Write(p)
	l.written += len(p)
	return len(p), nil
}

func (l *limitedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// cleanEnv 只保留必要变量。继承宿主环境会让 `bash env` 打出全部密钥。
func cleanEnv() []string {
	keep := map[string]bool{}
	if runtime.GOOS == "windows" {
		for _, k := range []string{"PATH", "PATHEXT", "COMSPEC", "SYSTEMROOT", "WINDIR", "TEMP", "TMP", "USERPROFILE", "NUMBER_OF_PROCESSORS", "HOMEDRIVE", "HOMEPATH"} {
			keep[k] = true
		}
	} else {
		for _, k := range []string{"PATH", "HOME", "LANG", "LC_ALL", "TMPDIR", "TEMP", "TMP", "USER"} {
			keep[k] = true
		}
	}
	var out []string
	for _, pair := range os.Environ() {
		if i := strings.IndexByte(pair, '='); i > 0 && keep[strings.ToUpper(pair[:i])] {
			out = append(out, pair)
		}
	}
	return out
}
