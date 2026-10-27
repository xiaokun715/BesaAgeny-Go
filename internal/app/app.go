// Package app 是 CLI 与守护进程的入口编排。
//
// 入口只做三件事：解析参数、装配运行时、把活派给子命令。
package app

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/besa-ai/besa-agent/internal/agent"
	"github.com/besa-ai/besa-agent/internal/composition"
	ctxpkg "github.com/besa-ai/besa-agent/internal/context"
	"github.com/besa-ai/besa-agent/internal/domain"
	"github.com/besa-ai/besa-agent/internal/foundation/ids"
	"github.com/besa-ai/besa-agent/internal/strategy"
	"github.com/besa-ai/besa-agent/internal/workflow"
)

const Version = "1.0.0"

type CLI struct {
	Stdout io.Writer
	Stderr io.Writer
	Stdin  io.Reader

	globalEnv    string
	globalConfig string
}

func NewCLI() *CLI { return &CLI{Stdout: os.Stdout, Stderr: os.Stderr, Stdin: os.Stdin} }

func (c *CLI) Run(ctx context.Context, args []string) int {
	args = c.splitGlobals(args)
	if len(args) == 0 {
		c.usage()
		return 2
	}
	switch args[0] {
	case "serve":
		return c.cmdServe(ctx, args[1:])
	case "run":
		return c.cmdRun(ctx, args[1:])
	case "chat":
		return c.cmdChat(ctx, args[1:])
	case "doctor":
		return c.cmdDoctor(ctx, args[1:])
	case "strategy":
		return c.cmdStrategy(ctx, args[1:])
	case "workflow":
		return c.cmdWorkflow(ctx, args[1:])
	case "migrate":
		return c.cmdMigrate(ctx, args[1:])
	case "version", "--version", "-v":
		fmt.Fprintf(c.Stdout, "besa %s\n", Version)
		return 0
	case "help", "--help", "-h":
		c.usage()
		return 0
	default:
		fmt.Fprintf(c.Stderr, "未知命令 %q\n\n", args[0])
		c.usage()
		return 2
	}
}

func (c *CLI) usage() {
	fmt.Fprint(c.Stdout, `BesaAgent —— 面向软件测试全流程的多智能体平台（Go 分布式版）

用法：besa <command> [flags]

命令：
  serve      启动服务（--role all|control|gateway|tool|agent|memory|strategy|experience|orchestrator|workflow）
  run        跑一次任务：besa run "帮我为登录接口设计测试用例"
  chat       多轮对话
  doctor     显示模型、逻辑名、工具、策略与健康状态
  strategy   list | select
  workflow   validate <file.yaml> | run <file.yaml> --input '{"k":"v"}'
  migrate    执行数据库迁移
  version    版本

全局参数：--env <name>（默认 BESA_ENV 或 dev）、--config <dir>
`)
}

// ------------------------------ 全局参数 ------------------------------

// splitGlobals 允许 --env/--config 出现在命令之前或之后（与 Python 版一致）。
func (c *CLI) splitGlobals(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--env" && i+1 < len(args):
			c.globalEnv = args[i+1]
			i++
		case strings.HasPrefix(arg, "--env="):
			c.globalEnv = strings.TrimPrefix(arg, "--env=")
		case arg == "--config" && i+1 < len(args):
			c.globalConfig = args[i+1]
			i++
		case strings.HasPrefix(arg, "--config="):
			c.globalConfig = strings.TrimPrefix(arg, "--config=")
		default:
			out = append(out, arg)
		}
	}
	return out
}

func (c *CLI) baseFlags(name string) (*flag.FlagSet, *string, *string) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(c.Stderr)
	env := fs.String("env", c.globalEnv, "配置环境（configs/<env>.yaml）")
	configDir := fs.String("config", c.globalConfig, "配置目录")
	return fs, env, configDir
}

func compositionOptions(env, configDir string) composition.Options {
	return composition.Options{Env: env, ConfigDir: configDir}
}

// ------------------------------ run ------------------------------

func (c *CLI) cmdRun(ctx context.Context, args []string) int {
	fs, env, configDir := c.baseFlags("run")
	stage := fs.String("stage", "", "指定阶段")
	session := fs.String("session", "", "会话 id")
	caller := fs.String("caller", "", "调用方")
	resume := fs.Bool("resume", false, "续跑")
	runID := fs.String("run-id", "", "运行 id（续跑时用）")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	task := strings.Join(fs.Args(), " ")
	if strings.TrimSpace(task) == "" {
		fmt.Fprintln(c.Stderr, "任务不能为空")
		return 2
	}
	local, err := composition.BuildLocal(ctx, compositionOptions(*env, *configDir))
	if err != nil {
		fmt.Fprintf(c.Stderr, "装配失败：%v\n", err)
		return 2
	}
	defer local.Close()

	sessionID := *session
	if sessionID == "" {
		sessionID = ids.New("session")
	}
	_ = local.Sessions.Ensure(ctx, sessionID, *caller, truncate(task, 80))
	history := c.loadHistory(ctx, local, sessionID)
	_, _ = local.Sessions.Append(ctx, sessionID, domain.User(task))

	outcome := local.Agent.RunOne(ctx, agent.RunRequest{
		Task: task, Stage: *stage, SessionID: sessionID, Caller: *caller,
		RunID: *runID, Resume: *resume, History: history,
	})
	if outcome.Answer != "" {
		_, _ = local.Sessions.Append(ctx, sessionID, domain.Message{Role: domain.RoleAssistant, Content: outcome.Answer})
	}
	fmt.Fprintln(c.Stdout, outcome.String())
	if outcome.Answer != "" {
		fmt.Fprintln(c.Stdout)
		fmt.Fprintln(c.Stdout, outcome.Answer)
	}
	switch outcome.Status {
	case agent.StatusDone:
		return 0
	case agent.StatusNeedsReview:
		return 3
	default:
		return 1
	}
}

func (c *CLI) loadHistory(ctx context.Context, local *composition.LocalRuntime, sessionID string) []ctxpkg.Message {
	rows, err := local.Sessions.History(ctx, sessionID, 200)
	if err != nil {
		return nil
	}
	return composition.ToContextMessages(rows)
}

// ------------------------------ chat ------------------------------

func (c *CLI) cmdChat(ctx context.Context, args []string) int {
	fs, env, configDir := c.baseFlags("chat")
	session := fs.String("session", "", "会话 id")
	stage := fs.String("stage", "", "固定阶段")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	local, err := composition.BuildLocal(ctx, compositionOptions(*env, *configDir))
	if err != nil {
		fmt.Fprintf(c.Stderr, "装配失败：%v\n", err)
		return 2
	}
	defer local.Close()
	sessionID := *session
	if sessionID == "" {
		sessionID = ids.New("session")
	}
	_ = local.Sessions.Ensure(ctx, sessionID, "", "chat")
	fmt.Fprintf(c.Stdout, "会话 %s。输入 /exit 退出。\n", sessionID)

	scanner := bufio.NewScanner(c.Stdin)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for {
		fmt.Fprint(c.Stdout, "> ")
		if !scanner.Scan() {
			return 0
		}
		input := strings.TrimSpace(scanner.Text())
		if input == "" {
			continue
		}
		if input == "/exit" || input == "/quit" {
			return 0
		}
		history := c.loadHistory(ctx, local, sessionID)
		_, _ = local.Sessions.Append(ctx, sessionID, domain.User(input))
		outcome := local.Agent.RunOne(ctx, agent.RunRequest{Task: input, Stage: *stage, SessionID: sessionID, History: history})
		if outcome.Answer != "" {
			_, _ = local.Sessions.Append(ctx, sessionID, domain.Message{Role: domain.RoleAssistant, Content: outcome.Answer})
		}
		fmt.Fprintln(c.Stdout, outcome.Answer)
		fmt.Fprintln(c.Stdout, outcome.String())
	}
}

// ------------------------------ doctor ------------------------------

func (c *CLI) cmdDoctor(ctx context.Context, args []string) int {
	fs, env, configDir := c.baseFlags("doctor")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	local, err := composition.BuildLocal(ctx, compositionOptions(*env, *configDir))
	if err != nil {
		fmt.Fprintf(c.Stderr, "装配失败：%v\n", err)
		return 2
	}
	defer local.Close()

	fmt.Fprintf(c.Stdout, "env=%s  config=%s\n\n", local.Base.Env(), local.Base.Config.Dir())
	fmt.Fprintln(c.Stdout, "模型：")
	for _, m := range local.Gateway.Registry().Models() {
		status := "available"
		if !m.Available() {
			status = "UNAVAILABLE: " + m.UnavailableReason
		}
		fmt.Fprintf(c.Stdout, "  %-24s %-10s %-24s %-18s %s\n", m.Key, m.Provider, m.Model, strings.Join(capStrings(m.Capabilities.List()), ","), status)
	}
	fmt.Fprintln(c.Stdout, "\n逻辑名：")
	for _, a := range local.Gateway.Registry().Aliases() {
		fmt.Fprintf(c.Stdout, "  %-24s → %v  策略=%v\n", a.Alias, a.Candidates, a.Strategy)
	}
	fmt.Fprintf(c.Stdout, "\n工具：%v\n", local.Tools.Registry.Names())
	fmt.Fprintf(c.Stdout, "记忆：%v\n", local.Memory != nil)
	fmt.Fprintf(c.Stdout, "存储：postgres=%v redis=%v nats=%v\n", local.Base.DB != nil, local.Base.Redis != nil, local.Base.NATS != nil)
	if local.Base.DB == nil {
		fmt.Fprintln(c.Stdout, "  [degraded] 会话历史只活在进程里；进程重启后 agent 会忘记之前聊过什么")
	}
	fmt.Fprintln(c.Stdout, "\n策略：")
	for _, spec := range local.Strategy.Registry.All() {
		status := "ok"
		if !spec.Available {
			status = "unavailable: " + spec.UnavailableReason
		}
		fmt.Fprintf(c.Stdout, "  %-22s %-24s %-14s %s\n", spec.Kind, spec.Name, spec.Version, status)
	}
	if len(local.Strategy.Issues) > 0 {
		fmt.Fprintln(c.Stdout, "\n策略配置告警：")
		for _, issue := range local.Strategy.Issues {
			fmt.Fprintln(c.Stdout, "  - "+issue)
		}
	}
	fmt.Fprintf(c.Stdout, "\n熔断状态：%v\n", local.Gateway.Health().Snapshot())
	return 0
}

func capStrings(caps []domain.Capability) []string {
	out := make([]string, len(caps))
	for i, c := range caps {
		out[i] = string(c)
	}
	return out
}

// ------------------------------ strategy ------------------------------

func (c *CLI) cmdStrategy(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(c.Stderr, "用法：besa strategy list|select")
		return 2
	}
	switch args[0] {
	case "list":
		fs, env, configDir := c.baseFlags("strategy list")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		local, err := composition.BuildLocal(ctx, compositionOptions(*env, *configDir))
		if err != nil {
			fmt.Fprintf(c.Stderr, "%v\n", err)
			return 2
		}
		defer local.Close()
		for _, spec := range local.Strategy.Registry.All() {
			status := "ok"
			if !spec.Available {
				status = "unavailable: " + spec.UnavailableReason
			}
			fmt.Fprintf(c.Stdout, "%-22s %-24s %-14s %s\n", spec.Kind, spec.Name, spec.Version, status)
		}
		valid, total := strategy.EnumerateValid(local.Strategy.Registry, strategy.DefaultContracts())
		fmt.Fprintf(c.Stdout, "\n合法组合：%d / %d\n", len(valid), total)
		return 0
	case "select":
		fs, env, configDir := c.baseFlags("strategy select")
		taskType := fs.String("task-type", "用例生成", "任务类型")
		risk := fs.String("risk", "low", "low|high")
		assets := fs.String("assets", "", "输入资产，逗号分隔")
		template := fs.String("template", "", "模板名")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		local, err := composition.BuildLocal(ctx, compositionOptions(*env, *configDir))
		if err != nil {
			fmt.Fprintf(c.Stderr, "%v\n", err)
			return 2
		}
		defer local.Close()
		features := strategy.TaskFeatures{TaskType: *taskType, Risk: strategy.RiskLevel(*risk), Template: *template}
		if *assets != "" {
			features.InputAssets = strings.Split(*assets, ",")
		}
		decision := local.Strategy.Selector.Select(ctx, features)
		raw, _ := json.MarshalIndent(decision, "", "  ")
		fmt.Fprintln(c.Stdout, string(raw))
		return 0
	default:
		fmt.Fprintln(c.Stderr, "未知子命令："+args[0])
		return 2
	}
}

// ------------------------------ workflow ------------------------------

func (c *CLI) cmdWorkflow(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(c.Stderr, "用法：besa workflow validate|run <file.yaml>")
		return 2
	}
	sub := args[0]
	fs, env, configDir := c.baseFlags("workflow " + sub)
	inputJSON := fs.String("input", "{}", "任务输入 JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(c.Stderr, "需要提供 workflow 文件")
		return 2
	}
	def, err := workflow.LoadTemplate(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(c.Stderr, "读取流程失败：%v\n", err)
		return 2
	}
	local, err := composition.BuildLocal(ctx, compositionOptions(*env, *configDir))
	if err != nil {
		fmt.Fprintf(c.Stderr, "%v\n", err)
		return 2
	}
	defer local.Close()
	switch sub {
	case "validate":
		issues := local.Workflow.Engine.Validate(def, nil, nil)
		if len(issues) == 0 {
			fmt.Fprintln(c.Stdout, "校验通过")
			return 0
		}
		for _, issue := range issues {
			fmt.Fprintf(c.Stdout, "[%s] %s: %s\n  → %s\n", issue.Kind, issue.NodeID, issue.Detail, issue.Suggestion)
		}
		return 1
	case "run":
		var input map[string]any
		if err := json.Unmarshal([]byte(*inputJSON), &input); err != nil {
			fmt.Fprintf(c.Stderr, "input 不是合法 JSON：%v\n", err)
			return 2
		}
		report, err := local.Workflow.Engine.Run(ctx, def, input, false)
		if err != nil {
			fmt.Fprintf(c.Stderr, "运行失败：%v\n", err)
			return 1
		}
		raw, _ := json.MarshalIndent(report, "", "  ")
		fmt.Fprintln(c.Stdout, string(raw))
		if report.Status == workflow.StateFailed {
			return 1
		}
		return 0
	default:
		fmt.Fprintln(c.Stderr, "未知子命令："+sub)
		return 2
	}
}

// ------------------------------ migrate ------------------------------

func (c *CLI) cmdMigrate(ctx context.Context, args []string) int {
	fs, env, configDir := c.baseFlags("migrate")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	base, err := composition.Open(ctx, composition.Options{Env: *env, ConfigDir: *configDir, RequireDB: true})
	if err != nil {
		fmt.Fprintf(c.Stderr, "迁移失败：%v\n", err)
		return 1
	}
	defer base.Close()
	fmt.Fprintln(c.Stdout, "迁移完成")
	return 0
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
