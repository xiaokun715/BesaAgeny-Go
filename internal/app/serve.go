package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"google.golang.org/grpc"

	besav1 "github.com/besa-ai/besa-agent/api/proto/v1"
	"github.com/besa-ai/besa-agent/internal/agent"
	"github.com/besa-ai/besa-agent/internal/composition"
	ctxpkg "github.com/besa-ai/besa-agent/internal/context"
	"github.com/besa-ai/besa-agent/internal/domain"
	"github.com/besa-ai/besa-agent/internal/gateway"
	"github.com/besa-ai/besa-agent/internal/memory"
	"github.com/besa-ai/besa-agent/internal/multiagent"
	"github.com/besa-ai/besa-agent/internal/service/agentd"
	"github.com/besa-ai/besa-agent/internal/service/controld"
	"github.com/besa-ai/besa-agent/internal/service/experienced"
	"github.com/besa-ai/besa-agent/internal/service/gatewayd"
	"github.com/besa-ai/besa-agent/internal/service/memoryd"
	"github.com/besa-ai/besa-agent/internal/service/orchestratord"
	"github.com/besa-ai/besa-agent/internal/service/strategyd"
	"github.com/besa-ai/besa-agent/internal/service/toold"
	"github.com/besa-ai/besa-agent/internal/service/workflowd"
	"github.com/besa-ai/besa-agent/internal/store/artifact"
	"github.com/besa-ai/besa-agent/internal/store/postgres"
	"github.com/besa-ai/besa-agent/internal/tool"
	"github.com/besa-ai/besa-agent/internal/transport"
	"github.com/besa-ai/besa-agent/internal/workflow"
)

var validRoles = map[string]bool{
	"all": true, "control": true, "gateway": true, "tool": true, "agent": true,
	"memory": true, "strategy": true, "experience": true, "orchestrator": true, "workflow": true,
}

func (c *CLI) cmdServe(ctx context.Context, args []string) int {
	fs, env, configDir := c.baseFlags("serve")
	role := fs.String("role", "all", "服务角色")
	addr := fs.String("addr", "", "监听地址（覆盖配置）")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !validRoles[*role] {
		fmt.Fprintf(c.Stderr, "未知角色 %q（可用：all control gateway tool agent memory strategy experience orchestrator workflow）\n", *role)
		return 2
	}
	opts := compositionOptions(*env, *configDir)
	if err := c.runServe(ctx, *role, *addr, opts); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(c.Stderr, "服务退出：%v\n", err)
		return 1
	}
	return 0
}

func (c *CLI) runServe(ctx context.Context, role, addr string, opts composition.Options) error {
	switch role {
	case "all":
		return c.runAll(ctx, opts)
	case "control":
		return c.runControl(ctx, opts, addr)
	case "gateway":
		return c.runGateway(ctx, opts, addr)
	case "tool":
		return c.runTool(ctx, opts, addr)
	case "agent":
		return c.runAgent(ctx, opts, addr)
	case "memory":
		return c.runMemory(ctx, opts, addr)
	case "strategy":
		return c.runStrategy(ctx, opts, addr)
	case "experience":
		return c.runExperience(ctx, opts, addr)
	case "orchestrator", "workflow":
		return c.runOrchestrator(ctx, opts, addr, role == "workflow")
	}
	return fmt.Errorf("未知角色 %s", role)
}

func roleAddr(base *composition.Base, role, override string) string {
	if override != "" {
		return override
	}
	return base.Config.String("service."+role+"_addr", defaultAddr(role))
}

func defaultAddr(role string) string {
	return map[string]string{
		"control": ":8080", "gateway": ":9001", "tool": ":9002", "agent": ":9003",
		"memory": ":9004", "strategy": ":9005", "experience": ":9006",
		"orchestrator": ":9007", "workflow": ":9008",
	}[role]
}

// ------------------------------ all ------------------------------

func (c *CLI) runAll(ctx context.Context, opts composition.Options) error {
	local, err := composition.BuildLocal(ctx, opts)
	if err != nil {
		return err
	}
	defer local.Close()
	base := local.Base

	var wg sync.WaitGroup
	errCh := make(chan error, 16)
	start := func(name string, fn func(context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(ctx); err != nil && ctx.Err() == nil {
				errCh <- fmt.Errorf("%s: %w", name, err)
			}
		}()
	}

	servers := map[string]*grpc.Server{}
	startGRPC := func(role string, register func(*grpc.Server)) {
		srv := transport.NewServer(base.Logger)
		servers[role] = srv
		addr := roleAddr(base, role, "")
		start(role, func(ctx context.Context) error {
			return transport.Serve(ctx, base.Logger, addr, srv, register)
		})
	}
	startGRPC("gateway", func(s *grpc.Server) { besav1.RegisterGatewayServiceServer(s, gatewayd.New(local.Gateway)) })
	startGRPC("tool", func(s *grpc.Server) {
		besav1.RegisterToolServiceServer(s, toold.New(local.Tools.Executor, local.Tools.Registry))
	})
	startGRPC("agent", func(s *grpc.Server) { besav1.RegisterAgentServiceServer(s, agentd.New(local.Agent)) })
	startGRPC("memory", func(s *grpc.Server) { besav1.RegisterMemoryServiceServer(s, memoryd.New(local.Memory)) })
	startGRPC("strategy", func(s *grpc.Server) {
		besav1.RegisterStrategyServiceServer(s, strategyd.New(local.Strategy.Selector, local.Strategy.Registry))
	})
	startGRPC("experience", func(s *grpc.Server) { besav1.RegisterExperienceServiceServer(s, experienced.New(local.Experience)) })
	startGRPC("orchestrator", func(s *grpc.Server) {
		besav1.RegisterOrchestratorServiceServer(s, orchestratord.New(local.Orchestrator))
	})
	startGRPC("workflow", func(s *grpc.Server) { besav1.RegisterWorkflowServiceServer(s, workflowd.New(local.Workflow.Engine)) })

	tasks := taskStore(base)
	ctrl := &controld.Server{
		Base: base, Tasks: tasks, Sessions: local.Sessions,
		NATS: base.NATS, Jobs: nil, Local: local,
	}
	ctrl.StartOutboxRelay(ctx)
	ctrl.LogStart()
	start("control", func(ctx context.Context) error {
		return serveHTTP(ctx, base.Logger, roleAddr(base, "control", ""), ctrl.Handler())
	})
	startUsageFlusher(ctx, base, local.Gateway)

	select {
	case <-ctx.Done():
		_ = ctx
	case err := <-errCh:
		return err
	}
	wg.Wait()
	return nil
}

// ------------------------------ control ------------------------------

func (c *CLI) runControl(ctx context.Context, opts composition.Options, addr string) error {
	opts.RequireNATS = true
	base, err := composition.Open(ctx, opts)
	if err != nil {
		return err
	}
	defer base.Close()
	tasks := taskStore(base)
	ctrl := &controld.Server{
		Base: base, Tasks: tasks, Sessions: composition.NewSessionStore(base),
		NATS: base.NATS, Jobs: base.NATS,
		Clients: dialClients(base),
	}
	ctrl.StartOutboxRelay(ctx)
	ctrl.LogStart()
	return serveHTTP(ctx, base.Logger, roleAddr(base, "control", addr), ctrl.Handler())
}

// ------------------------------ gateway ------------------------------

func (c *CLI) runGateway(ctx context.Context, opts composition.Options, addr string) error {
	base, err := composition.Open(ctx, opts)
	if err != nil {
		return err
	}
	defer base.Close()
	gw, err := composition.BuildGateway(base)
	if err != nil {
		return err
	}
	defer gw.Close()
	if base.DB != nil {
		startUsageFlusher(ctx, base, gw)
	}
	srv := transport.NewServer(base.Logger)
	errCh := make(chan error, 1)
	go func() {
		errCh <- transport.Serve(ctx, base.Logger, roleAddr(base, "gateway", addr), srv, func(s *grpc.Server) {
			besav1.RegisterGatewayServiceServer(s, gatewayd.New(gw))
		})
	}()
	select {
	case <-ctx.Done():
		time.Sleep(100 * time.Millisecond)
		return nil
	case err := <-errCh:
		return err
	}
}

// ------------------------------ tool ------------------------------

func (c *CLI) runTool(ctx context.Context, opts composition.Options, addr string) error {
	opts.RequireDB = true
	base, err := composition.Open(ctx, opts)
	if err != nil {
		return err
	}
	defer base.Close()
	spill := buildSpill(base)
	bundle, err := composition.BuildTools(base, spill)
	if err != nil {
		return err
	}
	srv := transport.NewServer(base.Logger)
	return transport.Serve(ctx, base.Logger, roleAddr(base, "tool", addr), srv, func(s *grpc.Server) {
		besav1.RegisterToolServiceServer(s, toold.New(bundle.Executor, bundle.Registry))
	})
}

// ------------------------------ agent ------------------------------

func (c *CLI) runAgent(ctx context.Context, opts composition.Options, addr string) error {
	opts.RequireDB = true
	opts.RequireNATS = true
	opts.RequireRedis = true
	base, err := composition.Open(ctx, opts)
	if err != nil {
		return err
	}
	defer base.Close()

	gatewayConn, err := transport.Dial(base.Config.String("clients.gateway", "127.0.0.1:9001"))
	if err != nil {
		return err
	}
	defer gatewayConn.Close()
	toolConn, err := transport.Dial(base.Config.String("clients.tool", "127.0.0.1:9002"))
	if err != nil {
		return err
	}
	defer toolConn.Close()
	gatewayClient := transport.NewGatewayClient(gatewayConn)
	gatewayClient.ChatAlias = base.Config.String("agent.alias", "runtime.default")
	gatewayClient.EmbedAlias = base.Config.String("memory.write.retrieve.embedding_alias", "emb.default")
	toolClient := transport.NewToolClient(toolConn)

	var memoryProvider ctxpkg.MemoryProvider
	if conn, err := transport.Dial(base.Config.String("clients.memory", "127.0.0.1:9004")); err == nil {
		defer conn.Close()
		memoryProvider = transport.NewMemoryClient(conn)
	} else {
		base.Logger.Warn("记忆服务不可用，agent 将不召回/沉淀长期记忆", "err", err)
	}
	contextManager := composition.BuildContext(base, composition.SummarizerAdapter{LLM: gatewayClient}, memoryProvider, func(alias string) *int {
		// 分布式下窗口由 gateway 服务提供；这里用本地配置兜底，避免多一次 RPC。
		w := base.Config.Int("agent.context.window", 0)
		if w > 0 {
			return &w
		}
		return nil
	})
	checkpoints := composition.BuildCheckpoints(base)
	finder := composition.ToolFinderAdapter{Repo: &postgres.ToolExecutionRepo{DB: base.DB}}
	catalog := remoteCatalog(ctx, base, toolClient)
	rt, err := composition.BuildAgent(base, gatewayClient, toolClient, catalog, contextManager, checkpoints, finder)
	if err != nil {
		return err
	}
	sessions := composition.NewSessionStore(base)
	tasks := taskStore(base)
	worker := &Worker{Base: base, Runtime: rt, Sessions: sessions, Tasks: tasks, Redis: base.Redis,
		Runs: &postgres.RunRepo{DB: base.DB}}
	if conn, dialErr := transport.Dial(base.Config.String("clients.experience", "127.0.0.1:9006")); dialErr == nil {
		worker.Experience = transport.NewExperienceClient(conn)
	}
	go func() {
		if err := worker.StartAgent(ctx); err != nil && ctx.Err() == nil {
			base.Logger.Error("agent worker 退出", "err", err)
		}
	}()
	srv := transport.NewServer(base.Logger)
	return transport.Serve(ctx, base.Logger, roleAddr(base, "agent", addr), srv, func(s *grpc.Server) {
		besav1.RegisterAgentServiceServer(s, agentd.New(rt))
	})
}

// ------------------------------ memory ------------------------------

func (c *CLI) runMemory(ctx context.Context, opts composition.Options, addr string) error {
	opts.RequireDB = true
	base, err := composition.Open(ctx, opts)
	if err != nil {
		return err
	}
	defer base.Close()
	conn, err := transport.Dial(base.Config.String("clients.gateway", "127.0.0.1:9001"))
	if err != nil {
		return err
	}
	defer conn.Close()
	gwClient := transport.NewGatewayClient(conn)
	gwClient.ChatAlias = base.Config.String("memory.gates.llm_judge.alias", "runtime.default")
	gwClient.EmbedAlias = base.Config.String("memory.write.retrieve.embedding_alias", "emb.default")
	manager, err := composition.BuildMemory(base, gwClient, gwClient)
	if err != nil {
		return err
	}
	go maintenanceLoop(ctx, base, manager)
	srv := transport.NewServer(base.Logger)
	return transport.Serve(ctx, base.Logger, roleAddr(base, "memory", addr), srv, func(s *grpc.Server) {
		besav1.RegisterMemoryServiceServer(s, memoryd.New(manager))
	})
}

// ------------------------------ strategy / experience ------------------------------

func (c *CLI) runStrategy(ctx context.Context, opts composition.Options, addr string) error {
	base, err := composition.Open(ctx, opts)
	if err != nil {
		return err
	}
	defer base.Close()
	bundle := composition.BuildStrategy(base)
	for _, issue := range bundle.Issues {
		base.Logger.Warn(issue)
	}
	srv := transport.NewServer(base.Logger)
	return transport.Serve(ctx, base.Logger, roleAddr(base, "strategy", addr), srv, func(s *grpc.Server) {
		besav1.RegisterStrategyServiceServer(s, strategyd.New(bundle.Selector, bundle.Registry))
	})
}

func (c *CLI) runExperience(ctx context.Context, opts composition.Options, addr string) error {
	base, err := composition.Open(ctx, opts)
	if err != nil {
		return err
	}
	defer base.Close()
	manager, err := composition.BuildExperience(base)
	if err != nil {
		return err
	}
	srv := transport.NewServer(base.Logger)
	return transport.Serve(ctx, base.Logger, roleAddr(base, "experience", addr), srv, func(s *grpc.Server) {
		besav1.RegisterExperienceServiceServer(s, experienced.New(manager))
	})
}

// ------------------------------ orchestrator / workflow ------------------------------

func (c *CLI) runOrchestrator(ctx context.Context, opts composition.Options, addr string, workflowOnly bool) error {
	opts.RequireDB = true
	opts.RequireNATS = true
	base, err := composition.Open(ctx, opts)
	if err != nil {
		return err
	}
	defer base.Close()
	agentConn, err := transport.Dial(base.Config.String("clients.agent", "127.0.0.1:9003"))
	if err != nil {
		return err
	}
	defer agentConn.Close()
	agentClient := transport.NewAgentClient(agentConn)

	gwConn, err := transport.Dial(base.Config.String("clients.gateway", "127.0.0.1:9001"))
	if err != nil {
		return err
	}
	defer gwConn.Close()
	gwClient := transport.NewGatewayClient(gwConn)
	gwClient.ChatAlias = base.Config.String("agent.alias", "runtime.default")

	bundle := composition.BuildWorkflow(base, func(stage string) workflow.NodeRunner {
		return transport.NodeRunner{Client: agentClient, Stage: stage}
	})
	worker := &Worker{Base: base, Tasks: taskStore(base), Sessions: composition.NewSessionStore(base),
		Engine: bundle.Engine, Pool: bundle.Pool}
	go func() {
		if err := worker.StartWorkflow(ctx); err != nil && ctx.Err() == nil {
			base.Logger.Error("workflow worker 退出", "err", err)
		}
	}()

	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	wfSrv := transport.NewServer(base.Logger)
	wg.Add(1)
	go func() {
		defer wg.Done()
		errCh <- transport.Serve(ctx, base.Logger, roleAddr(base, "workflow", ""), wfSrv, func(s *grpc.Server) {
			besav1.RegisterWorkflowServiceServer(s, workflowd.New(bundle.Engine))
		})
	}()
	if !workflowOnly {
		members := make([]multiagent.Member, 0, len(agent.StageOrder))
		for _, stage := range agent.StageOrder {
			members = append(members, multiagent.Member{Name: stage, Stage: stage, Runtime: agentClient})
		}
		orchestrator := multiagent.NewOrchestrator(members, gwClient, base.Events)
		orchSrv := transport.NewServer(base.Logger)
		wg.Add(1)
		go func() {
			defer wg.Done()
			errCh <- transport.Serve(ctx, base.Logger, roleAddr(base, "orchestrator", addr), orchSrv, func(s *grpc.Server) {
				besav1.RegisterOrchestratorServiceServer(s, orchestratord.New(orchestrator))
			})
		}()
	}
	select {
	case <-ctx.Done():
	case err := <-errCh:
		if err != nil {
			return err
		}
	}
	wg.Wait()
	return nil
}

// ------------------------------ 公共辅助 ------------------------------

// dialClients 尽力连接各服务；连不上的留空，对应接口返回 503，
// 而不是让控制面起不来 —— 控制面先活下来，排障才有入口。
func dialClients(base *composition.Base) controld.Clients {
	var clients controld.Clients
	dial := func(key, defaultAddr string) *grpc.ClientConn {
		addr := base.Config.String("clients."+key, defaultAddr)
		conn, err := transport.Dial(addr)
		if err != nil {
			base.Logger.Warn("服务不可达", "service", key, "addr", addr, "err", err)
			return nil
		}
		return conn
	}
	if conn := dial("agent", "127.0.0.1:9003"); conn != nil {
		clients.Agent = transport.NewAgentClient(conn)
	}
	if conn := dial("strategy", "127.0.0.1:9005"); conn != nil {
		clients.Strategy = transport.NewStrategyClient(conn)
	}
	if conn := dial("experience", "127.0.0.1:9006"); conn != nil {
		clients.Experience = transport.NewExperienceClient(conn)
	}
	if conn := dial("workflow", "127.0.0.1:9008"); conn != nil {
		clients.Workflow = transport.NewWorkflowClient(conn)
	}
	if conn := dial("orchestrator", "127.0.0.1:9007"); conn != nil {
		clients.Orchestrator = transport.NewOrchestratorClient(conn)
	}
	return clients
}

func taskStore(base *composition.Base) controld.TaskStore {
	if base.DB != nil {
		return &controld.PGTaskStore{Repo: &postgres.TaskRepo{DB: base.DB}}
	}
	return controld.NewMemTasks()
}

func buildSpill(base *composition.Base) tool.SpillStore {
	endpoint := base.Config.String("artifact.endpoint", "")
	if endpoint == "" {
		return nil
	}
	access := os.Getenv(base.Config.String("artifact.access_key_env", "MINIO_ACCESS_KEY"))
	secret := os.Getenv(base.Config.String("artifact.secret_key_env", "MINIO_SECRET_KEY"))
	s3, err := artifact.OpenS3(context.Background(), endpoint, access, secret,
		base.Config.String("artifact.bucket", "besa-agent"),
		base.Config.String("artifact.prefix", "spill"),
		base.Config.Bool("artifact.use_ssl", false))
	if err != nil {
		base.Logger.Warn("对象存储不可用，大结果回退到本地目录", "err", err)
		return nil
	}
	return s3
}

func serveHTTP(ctx context.Context, logger interface{ Info(string, ...any) }, addr string, handler http.Handler) error {
	srv := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	logger.Info("控制面已启动", "addr", addr)
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

func startUsageFlusher(ctx context.Context, base *composition.Base, gw *gateway.Gateway) {
	if base.DB == nil {
		return
	}
	repo := &postgres.UsageRepo{DB: base.DB}
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		flush := func() {
			records := gw.Ledger().Drain()
			dropped := gw.Ledger().DrainDropped()
			flushCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := repo.Record(flushCtx, records); err != nil {
				base.Logger.Warn("用量落库失败（旁路）", "err", err)
			}
			if err := repo.RecordDrops(flushCtx, dropped); err != nil {
				base.Logger.Warn("用量丢弃计数落库失败", "err", err)
			}
		}
		for {
			select {
			case <-ctx.Done():
				flush()
				return
			case <-ticker.C:
				flush()
			}
		}
	}()
}

// ------------------------------ 远程工具目录 ------------------------------

type staticCatalog struct {
	defs []domain.ToolDefinition
}

func (s staticCatalog) Select(categories, only []string) []domain.ToolDefinition {
	catSet := map[string]bool{}
	for _, c := range categories {
		catSet[c] = true
	}
	onlySet := map[string]bool{}
	for _, o := range only {
		onlySet[o] = true
	}
	var out []domain.ToolDefinition
	for _, def := range s.defs {
		if len(catSet) > 0 && !catSet[def.Category] {
			continue
		}
		if len(onlySet) > 0 && !onlySet[def.Name] {
			continue
		}
		out = append(out, def)
	}
	return out
}

func remoteCatalog(ctx context.Context, base *composition.Base, client *transport.ToolClient) staticCatalog {
	// 分布式启动有先后：工具服务可能还没就绪。重试到 30 秒再放弃，
	// 否则 agent 会带着空目录启动，表现为“模型从不用工具”。
	var resp *besav1.ListToolsResponse
	var err error
	for attempt := 0; attempt < 15; attempt++ {
		callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		resp, err = client.Client.ListTools(callCtx, &besav1.ListToolsRequest{})
		cancel()
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return staticCatalog{}
		case <-time.After(2 * time.Second):
		}
	}
	if err != nil {
		base.Logger.Warn("拉取工具目录失败，agent 将看不到任何工具", "err", err)
		return staticCatalog{}
	}
	defs := make([]domain.ToolDefinition, 0, len(resp.Tools))
	for _, d := range resp.Tools {
		defs = append(defs, domain.ToolDefinition{Name: d.Name, Description: d.Description,
			Parameters: transport.StructToMap(d.Parameters), SideEffect: d.SideEffect, Category: d.Category})
	}
	base.Logger.Info("工具目录已加载", "count", len(defs))
	return staticCatalog{defs: defs}
}

func maintenanceLoop(ctx context.Context, base *composition.Base, manager *memory.Manager) {
	go func() {
		run := func() {
			runCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			report, err := manager.Maintain(runCtx, "default")
			if err != nil {
				base.Logger.Warn("记忆维护失败（旁路）", "err", err)
				return
			}
			base.Logger.Info("记忆维护完成", "scanned", report.Scanned, "tombstoned", report.Tombstoned)
		}
		run()
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				run()
			}
		}
	}()
}
