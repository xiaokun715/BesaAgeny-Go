package composition

import (
	"context"
	"encoding/json"
	"os"
	"time"

	"github.com/besa-ai/besa-agent/internal/agent"
	ctxpkg "github.com/besa-ai/besa-agent/internal/context"
	"github.com/besa-ai/besa-agent/internal/domain"
	"github.com/besa-ai/besa-agent/internal/experience"
	"github.com/besa-ai/besa-agent/internal/gateway"
	"github.com/besa-ai/besa-agent/internal/memory"
	"github.com/besa-ai/besa-agent/internal/multiagent"
	"github.com/besa-ai/besa-agent/internal/store/artifact"
	"github.com/besa-ai/besa-agent/internal/store/memstore"
	"github.com/besa-ai/besa-agent/internal/store/postgres"
	"github.com/besa-ai/besa-agent/internal/tool"
	"github.com/besa-ai/besa-agent/internal/workflow"
)

type LocalRuntime struct {
	Base         *Base
	Gateway      *gateway.Gateway
	Tools        *ToolsBundle
	Memory       *memory.Manager
	MemoryProv   *LocalMemoryProvider
	Context      *ctxpkg.Manager
	Agent        *agent.Runtime
	Stages       *agent.StageRegistry
	Strategy     *StrategyBundle
	Experience   *experience.Manager
	Workflow     *WorkflowBundle
	Orchestrator *multiagent.Orchestrator
	Sessions     SessionStore
}

// BuildLocal 装配单机运行时（CLI / 单进程模式）。
func BuildLocal(ctx context.Context, opts Options) (*LocalRuntime, error) {
	base, err := Open(ctx, opts)
	if err != nil {
		return nil, err
	}
	out := &LocalRuntime{Base: base}
	fail := func(err error) (*LocalRuntime, error) {
		base.Close()
		return nil, err
	}

	gw, err := BuildGateway(base)
	if err != nil {
		return fail(err)
	}
	out.Gateway = gw

	var spill tool.SpillStore
	if endpoint := base.Config.String("artifact.endpoint", ""); endpoint != "" {
		access := os.Getenv(base.Config.String("artifact.access_key_env", "MINIO_ACCESS_KEY"))
		secret := os.Getenv(base.Config.String("artifact.secret_key_env", "MINIO_SECRET_KEY"))
		s3, err := artifact.OpenS3(ctx, endpoint, access, secret,
			base.Config.String("artifact.bucket", "besa-agent"),
			base.Config.String("artifact.prefix", "spill"),
			base.Config.Bool("artifact.use_ssl", false))
		if err != nil {
			base.Logger.Warn("对象存储不可用，大结果回退到本地目录", "err", err)
		} else {
			spill = s3
		}
	}

	tools, err := BuildTools(base, spill)
	if err != nil {
		return fail(err)
	}
	out.Tools = tools

	llm := &LocalLLM{Gateway: gw, Alias: base.Config.String("memory.gates.llm_judge.alias", "runtime.default")}
	embedder := &LocalEmbedder{Gateway: gw, Alias: base.Config.String("memory.write.retrieve.embedding_alias", "emb.default")}
	mem, err := BuildMemory(base, llm, embedder)
	if err != nil {
		return fail(err)
	}
	out.Memory = mem
	out.MemoryProv = &LocalMemoryProvider{Manager: mem}

	contextManager := BuildContext(base, SummarizerAdapter{LLM: llm}, out.MemoryProv, gw.ContextWindow)
	out.Context = contextManager

	checkpoints := BuildCheckpoints(base)
	finder := ToolFinderAdapter{Repo: tools.Repo}
	agentRuntime, err := BuildAgent(base, gw, tools.Executor, CatalogAdapter{Registry: tools.Registry}, contextManager, checkpoints, finder)
	if err != nil {
		return fail(err)
	}
	out.Agent = agentRuntime
	out.Stages = agentRuntime.Stages

	out.Strategy = BuildStrategy(base)
	for _, issue := range out.Strategy.Issues {
		base.Logger.Warn(issue)
	}
	out.Experience, err = BuildExperience(base)
	if err != nil {
		return fail(err)
	}

	out.Workflow = BuildWorkflow(base, func(stage string) workflow.NodeRunner {
		return LocalNodeRunner{Runtime: agentRuntime, Stage: stage}
	})

	members := make([]multiagent.Member, 0, len(agent.StageOrder))
	for _, stage := range agent.StageOrder {
		members = append(members, multiagent.Member{Name: stage, Stage: stage, Runtime: agentRuntime})
	}
	out.Orchestrator = multiagent.NewOrchestrator(members, llm, base.Events)
	out.Sessions = NewSessionStore(base)
	return out, nil
}

func (l *LocalRuntime) Close() {
	if l.Gateway != nil {
		_ = l.Gateway.Close()
	}
	l.Base.Close()
}

// ------------------------------ 适配器 ------------------------------

type LocalMemoryProvider struct {
	Manager *memory.Manager
}

func (p *LocalMemoryProvider) Recall(ctx context.Context, query string, topK int, kinds []string, sessionID string) ([]ctxpkg.MemoryItem, error) {
	return p.Manager.ContextRecall(ctx, query, topK, kinds, sessionID)
}

func (p *LocalMemoryProvider) Settle(ctx context.Context, turns []ctxpkg.Turn, traceID, sessionID string) error {
	return p.Manager.ContextSettle(ctx, turns, traceID, sessionID)
}

// ------------------------------ 会话存储 ------------------------------

type StoredMessage struct {
	MessageID  string
	SessionID  string
	Seq        int
	Role       domain.Role
	Content    string
	Name       string
	ToolCallID string
	ToolCalls  []domain.ToolCall
	CreatedAt  time.Time
}

type SessionStore interface {
	Ensure(ctx context.Context, sessionID, caller, title string) error
	Append(ctx context.Context, sessionID string, msg domain.Message) (StoredMessage, error)
	History(ctx context.Context, sessionID string, limit int) ([]StoredMessage, error)
}

func NewSessionStore(base *Base) SessionStore {
	if base.DB != nil {
		return &pgSessions{repo: &postgres.SessionRepo{DB: base.DB}}
	}
	return &memSessions{store: memstore.NewSessions()}
}

type pgSessions struct{ repo *postgres.SessionRepo }

func (p *pgSessions) Ensure(ctx context.Context, sessionID, caller, title string) error {
	return p.repo.Ensure(ctx, sessionID, caller, title)
}
func (p *pgSessions) Append(ctx context.Context, sessionID string, msg domain.Message) (StoredMessage, error) {
	row, err := p.repo.Append(ctx, sessionID, msg)
	if err != nil {
		return StoredMessage{}, err
	}
	return StoredMessage{MessageID: row.MessageID, SessionID: row.SessionID, Seq: row.Seq, Role: row.Role,
		Content: row.Content, Name: row.Name, ToolCallID: row.ToolCallID, ToolCalls: row.ToolCalls, CreatedAt: row.CreatedAt}, nil
}
func (p *pgSessions) History(ctx context.Context, sessionID string, limit int) ([]StoredMessage, error) {
	rows, err := p.repo.History(ctx, sessionID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]StoredMessage, len(rows))
	for i, row := range rows {
		out[i] = StoredMessage{MessageID: row.MessageID, SessionID: row.SessionID, Seq: row.Seq, Role: row.Role,
			Content: row.Content, Name: row.Name, ToolCallID: row.ToolCallID, ToolCalls: row.ToolCalls, CreatedAt: row.CreatedAt}
	}
	return out, nil
}

type memSessions struct{ store *memstore.Sessions }

func (m *memSessions) Ensure(ctx context.Context, sessionID, caller, title string) error {
	return m.store.Ensure(ctx, sessionID, caller, title)
}
func (m *memSessions) Append(ctx context.Context, sessionID string, msg domain.Message) (StoredMessage, error) {
	row, err := m.store.Append(ctx, sessionID, msg)
	if err != nil {
		return StoredMessage{}, err
	}
	return StoredMessage{MessageID: row.MessageID, SessionID: row.SessionID, Seq: row.Seq, Role: row.Role,
		Content: row.Content, Name: row.Name, ToolCallID: row.ToolCallID, ToolCalls: row.ToolCalls, CreatedAt: row.CreatedAt}, nil
}
func (m *memSessions) History(ctx context.Context, sessionID string, limit int) ([]StoredMessage, error) {
	rows, err := m.store.History(ctx, sessionID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]StoredMessage, len(rows))
	for i, row := range rows {
		out[i] = StoredMessage{MessageID: row.MessageID, SessionID: row.SessionID, Seq: row.Seq, Role: row.Role,
			Content: row.Content, Name: row.Name, ToolCallID: row.ToolCallID, ToolCalls: row.ToolCalls, CreatedAt: row.CreatedAt}
	}
	return out, nil
}

// ToContextMessages 把会话历史转成上下文层消息，保留 message_id 供回捞。
func ToContextMessages(rows []StoredMessage) []ctxpkg.Message {
	out := make([]ctxpkg.Message, 0, len(rows))
	for _, row := range rows {
		m := ctxpkg.Message{MessageID: row.MessageID, Role: row.Role, Content: row.Content,
			Name: row.Name, ToolCallID: row.ToolCallID}
		for _, tc := range row.ToolCalls {
			m.ToolCalls = append(m.ToolCalls, ctxpkg.ToolCallRef{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments, ArgumentsRaw: tc.ArgumentsRaw})
		}
		out = append(out, m)
	}
	return out
}

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }
