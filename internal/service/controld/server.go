// Package controld 是控制面：REST 提交任务、查询状态、SSE 订阅事件。
//
// 它不执行 Agent 循环 —— 任务经 NATS 交给 agentd / orchestratord；
// 单机模式下才回退到进程内运行时。
package controld

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/nats-io/nats.go/jetstream"

	besav1 "github.com/besa-ai/besa-agent/api/proto/v1"
	"github.com/besa-ai/besa-agent/internal/agent"
	"github.com/besa-ai/besa-agent/internal/composition"
	ctxpkg "github.com/besa-ai/besa-agent/internal/context"
	"github.com/besa-ai/besa-agent/internal/domain"
	"github.com/besa-ai/besa-agent/internal/event"
	"github.com/besa-ai/besa-agent/internal/experience"
	"github.com/besa-ai/besa-agent/internal/foundation/ids"
	"github.com/besa-ai/besa-agent/internal/multiagent"
	"github.com/besa-ai/besa-agent/internal/store/postgres"
	"github.com/besa-ai/besa-agent/internal/strategy"
	"github.com/besa-ai/besa-agent/internal/transport"
	"github.com/besa-ai/besa-agent/internal/workflow"
)

type TaskStore interface {
	Create(ctx context.Context, task postgres.Task) (postgres.Task, error)
	Get(ctx context.Context, taskID string) (postgres.Task, error)
	UpdateStatus(ctx context.Context, taskID, status, runID, errMsg string) error
	Complete(ctx context.Context, taskID, status string, result map[string]any, errMsg string) error
	ListBySession(ctx context.Context, sessionID string, limit int) ([]postgres.Task, error)
}

type Clients struct {
	Agent        *transport.AgentClient
	Strategy     *transport.StrategyClient
	Experience   *transport.ExperienceClient
	Workflow     *transport.WorkflowClient
	Orchestrator *transport.OrchestratorClient
}

type Server struct {
	Base     *composition.Base
	Tasks    TaskStore
	Sessions composition.SessionStore
	NATS     *event.NATSBus // 事件总线 / SSE / outbox relay
	Jobs     *event.NATSBus // 作业投递；all 模式下为 nil，走进程内执行
	Local    *composition.LocalRuntime
	Clients  Clients
}

func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RealIP, middleware.Recoverer)
	r.Use(s.traceMiddleware)
	r.Use(middleware.Timeout(60 * time.Second))

	r.Get("/healthz", s.handleHealth)
	r.Get("/readyz", s.handleHealth)
	r.Get("/v1/doctor", s.handleDoctor)

	r.Post("/v1/tasks", s.handleSubmit)
	r.Get("/v1/tasks/{taskID}", s.handleGetTask)
	r.Get("/v1/tasks", s.handleListTasks)
	r.Post("/v1/runs/{runID}/cancel", s.handleCancel)
	r.Get("/v1/sessions/{sessionID}/messages", s.handleMessages)
	r.Get("/v1/events/stream", s.handleEventStream)

	r.Post("/v1/agent/run", s.handleRunSync)
	r.Post("/v1/workflow/validate", s.handleWorkflowValidate)
	r.Post("/v1/workflow/run", s.handleWorkflowRun)
	r.Post("/v1/orchestrate", s.handleOrchestrate)
	r.Post("/v1/strategy/select", s.handleStrategySelect)
	r.Get("/v1/strategy/list", s.handleStrategyList)
	r.Post("/v1/experience/recall", s.handleExperienceRecall)
	return r
}

// ------------------------------ 中间件 ------------------------------

type traceCtxKey struct{}

func (s *Server) traceMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traceID := r.Header.Get("X-Trace-Id")
		if traceID == "" {
			traceID = ids.New("trace")
		}
		w.Header().Set("X-Trace-Id", traceID)
		ctx := ids.WithTrace(r.Context(), traceID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// ------------------------------ 任务 ------------------------------

type SubmitRequest struct {
	Goal      string             `json:"goal"`
	SessionID string             `json:"session_id"`
	Caller    string             `json:"caller"`
	Kind      string             `json:"kind"` // agent | workflow
	Stage     string             `json:"stage"`
	Strategy  string             `json:"strategy"`
	Mode      string             `json:"mode"`
	Template  string             `json:"template"`
	Workflow  *workflow.Workflow `json:"workflow,omitempty"`
	Input     map[string]any     `json:"input,omitempty"`
	Resume    bool               `json:"resume,omitempty"`
}

func (s *Server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	var req SubmitRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON："+err.Error())
		return
	}
	if strings.TrimSpace(req.Goal) == "" && req.Workflow == nil {
		writeError(w, http.StatusBadRequest, "goal 不能为空")
		return
	}
	if req.Kind == "" {
		req.Kind = "agent"
	}
	if req.Kind != "agent" && req.Kind != "workflow" {
		writeError(w, http.StatusBadRequest, "kind 只能是 agent 或 workflow")
		return
	}
	ctx := r.Context()
	if req.SessionID != "" {
		_ = s.Sessions.Ensure(ctx, req.SessionID, req.Caller, truncate(req.Goal, 80))
	}
	task, err := s.Tasks.Create(ctx, postgres.Task{
		SessionID: req.SessionID, Caller: req.Caller, Kind: req.Kind, Stage: req.Stage,
		Goal: req.Goal, Status: "queued",
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "创建任务失败："+err.Error())
		return
	}

	switch req.Kind {
	case "agent":
		job := transport.AgentJob{TaskID: task.TaskID, Goal: req.Goal, Stage: req.Stage, SessionID: req.SessionID,
			Caller: req.Caller, Strategy: req.Strategy, Resume: req.Resume}
		if s.Jobs != nil {
			if err := s.Jobs.PublishJob(ctx, "agent.run", task.TaskID, job); err != nil {
				_ = s.Tasks.UpdateStatus(ctx, task.TaskID, "failed", "", "投递作业失败："+err.Error())
				writeError(w, http.StatusServiceUnavailable, "投递作业失败："+err.Error())
				return
			}
		} else if s.Local != nil {
			go s.runLocalAgent(task.TaskID, job)
		} else {
			writeError(w, http.StatusServiceUnavailable, "没有 NATS 也没有本地运行时，无法执行任务")
			return
		}
	case "workflow":
		job := transport.WorkflowJob{TaskID: task.TaskID, Workflow: req.Workflow, Input: req.Input,
			SessionID: req.SessionID, Caller: req.Caller, Mode: req.Mode, Template: req.Template,
			Resume: req.Resume, Goal: req.Goal}
		if s.Jobs != nil {
			if err := s.Jobs.PublishJob(ctx, "workflow.run", task.TaskID, job); err != nil {
				writeError(w, http.StatusServiceUnavailable, "投递作业失败："+err.Error())
				return
			}
		} else if s.Local != nil {
			go s.runLocalWorkflow(task.TaskID, job)
		} else {
			writeError(w, http.StatusServiceUnavailable, "没有 NATS 也没有本地运行时，无法执行任务")
			return
		}
	}
	writeJSON(w, http.StatusAccepted, task)
}

func (s *Server) runLocalAgent(taskID string, job transport.AgentJob) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	ctx = ids.WithTrace(ctx, ids.New("trace"))
	_ = s.Tasks.UpdateStatus(ctx, taskID, "running", "", "")
	_ = s.Local.Sessions.Ensure(ctx, job.SessionID, job.Caller, truncate(job.Goal, 80))
	history := s.loadHistory(ctx, job.SessionID)
	_, _ = s.Local.Sessions.Append(ctx, job.SessionID, domain.User(job.Goal))
	outcome := s.Local.Agent.RunOne(ctx, agent.RunRequest{Task: job.Goal, Stage: job.Stage, SessionID: job.SessionID, Caller: job.Caller, Resume: job.Resume, History: history})
	if outcome.Answer != "" {
		_, _ = s.Local.Sessions.Append(ctx, job.SessionID, domain.Message{Role: domain.RoleAssistant, Content: outcome.Answer})
	}
	result := map[string]any{
		"run_id": outcome.RunID, "status": outcome.Status, "answer": outcome.Answer,
		"steps": outcome.Steps, "tool_calls": outcome.ToolCalls, "elapsed_s": outcome.Elapsed.Seconds(),
		"cost": outcome.CostText(), "error": outcome.Error, "reason": outcome.Reason,
	}
	_ = s.Tasks.UpdateStatus(ctx, taskID, "running", outcome.RunID, "")
	_ = s.Tasks.Complete(ctx, taskID, string(outcome.Status), result, outcome.Error)
}

func (s *Server) runLocalWorkflow(taskID string, job transport.WorkflowJob) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	_ = s.Tasks.UpdateStatus(ctx, taskID, "running", "", "")
	if job.Workflow == nil {
		_ = s.Tasks.Complete(ctx, taskID, "failed", nil, "缺少 workflow 定义（auto 模式需要 orchestratord）")
		return
	}
	report, err := s.Local.Workflow.Engine.Run(ctx, *job.Workflow, job.Input, job.Resume)
	result := map[string]any{"workflow_run_id": report.WorkflowRunID, "status": report.Status, "error": report.Error}
	if err != nil {
		_ = s.Tasks.Complete(ctx, taskID, "failed", result, err.Error())
		return
	}
	status := "done"
	if report.Status == workflow.StateFailed {
		status = "failed"
	}
	_ = s.Tasks.Complete(ctx, taskID, status, result, report.Error)
}

func (s *Server) loadHistory(ctx context.Context, sessionID string) []ctxpkg.Message {
	if sessionID == "" {
		return nil
	}
	rows, err := s.Sessions.History(ctx, sessionID, 200)
	if err != nil {
		return nil
	}
	return composition.ToContextMessages(rows)
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	task, err := s.Tasks.Get(r.Context(), chi.URLParam(r, "taskID"))
	if err != nil {
		writeError(w, http.StatusNotFound, "任务不存在")
		return
	}
	writeJSON(w, http.StatusOK, task)
}

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("session_id")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	tasks, err := s.Tasks.ListBySession(r.Context(), sessionID, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks})
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runID")
	if s.Local != nil {
		s.Local.Agent.Cancel(runID)
	}
	if s.Base.Redis != nil {
		_ = s.Base.Redis.RequestCancel(r.Context(), runID, 30*time.Minute)
	}
	if s.Jobs != nil {
		_ = s.Jobs.PublishJob(r.Context(), "agent.cancel", "cancel-"+runID, transport.CancelJob{RunID: runID})
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"run_id": runID, "cancelled": true})
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "sessionID")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := s.Sessions.History(r.Context(), sessionID, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session_id": sessionID, "messages": rows})
}

// ------------------------------ SSE ------------------------------

func (s *Server) handleEventStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "服务器不支持流式响应")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	var ch <-chan event.Event
	var closeFn func()
	if s.NATS != nil {
		local := make(chan event.Event, 256)
		ch = local
		sub, err := s.NATS.SubscribeEvents("", func(ev event.Event) {
			select {
			case local <- ev:
			default:
			}
		})
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "订阅事件失败："+err.Error())
			return
		}
		closeFn = func() { _ = sub.Unsubscribe() }
	} else {
		ch = s.Base.InMemory.Subscribe("", 256)
		closeFn = func() {}
	}
	defer closeFn()

	sessionID := r.URL.Query().Get("session_id")
	traceID := r.URL.Query().Get("trace_id")
	runID := r.URL.Query().Get("run_id")

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case ev := <-ch:
			if sessionID != "" && ev.SessionID != sessionID {
				continue
			}
			if traceID != "" && ev.TraceID != traceID {
				continue
			}
			if runID != "" && ev.Subject != runID {
				continue
			}
			raw, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", ev.ID, ev.Name, raw)
			flusher.Flush()
		}
	}
}

// ------------------------------ 同步接口 ------------------------------

func (s *Server) handleRunSync(w http.ResponseWriter, r *http.Request) {
	var req transport.RunRequestJSON
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	runReq := agent.RunRequest{Task: req.Task, Stage: req.Stage, Hint: req.Hint, SessionID: req.SessionID,
		Caller: req.Caller, RunID: req.RunID, Resume: req.Resume}
	if req.SessionID != "" {
		_ = s.Sessions.Ensure(r.Context(), req.SessionID, req.Caller, truncate(req.Task, 80))
		runReq.History = s.loadHistory(r.Context(), req.SessionID)
		_, _ = s.Sessions.Append(r.Context(), req.SessionID, domain.User(req.Task))
	}
	var outcome agent.RunOutcome
	var err error
	if s.Clients.Agent != nil {
		outcome = s.Clients.Agent.RunOne(r.Context(), runReq)
	} else if s.Local != nil {
		outcome = s.Local.Agent.RunOne(r.Context(), runReq)
	} else {
		writeError(w, http.StatusServiceUnavailable, "没有 agent 客户端，也没有本地运行时")
		return
	}
	if outcome.Answer != "" && req.SessionID != "" {
		_, _ = s.Sessions.Append(r.Context(), req.SessionID, domain.Message{Role: domain.RoleAssistant, Content: outcome.Answer})
	}
	_ = err
	writeJSON(w, http.StatusOK, outcome)
}

func (s *Server) handleWorkflowValidate(w http.ResponseWriter, r *http.Request) {
	var def workflow.Workflow
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&def); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var issues []workflow.GraphIssue
	var err error
	if s.Clients.Workflow != nil {
		issues, err = s.Clients.Workflow.Validate(r.Context(), def)
	} else if s.Local != nil {
		issues = s.Local.Workflow.Engine.Validate(def, nil, nil)
	} else {
		writeError(w, http.StatusServiceUnavailable, "workflow 服务不可用")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"issues": issues})
}

func (s *Server) handleWorkflowRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Workflow workflow.Workflow `json:"workflow"`
		Input    map[string]any    `json:"input"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var report workflow.RunReport
	var err error
	if s.Clients.Workflow != nil {
		report, err = s.Clients.Workflow.Run(r.Context(), req.Workflow, req.Input)
	} else if s.Local != nil {
		report, err = s.Local.Workflow.Engine.Run(r.Context(), req.Workflow, req.Input, false)
	} else {
		writeError(w, http.StatusServiceUnavailable, "workflow 服务不可用")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (s *Server) handleOrchestrate(w http.ResponseWriter, r *http.Request) {
	var req orchestrateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.Clients.Orchestrator != nil {
		outcome, err := s.Clients.Orchestrator.Run(r.Context(), multiagentRequest(req))
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, outcome)
		return
	}
	if s.Local != nil {
		outcome, err := s.Local.Orchestrator.Run(r.Context(), multiagentRequest(req))
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, outcome)
		return
	}
	writeError(w, http.StatusServiceUnavailable, "orchestrator 服务不可用")
}

func (s *Server) handleStrategySelect(w http.ResponseWriter, r *http.Request) {
	var features strategy.TaskFeatures
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&features); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.Clients.Strategy != nil {
		decision, err := s.Clients.Strategy.Select(r.Context(), features)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, decision)
		return
	}
	if s.Local != nil {
		writeJSON(w, http.StatusOK, s.Local.Strategy.Selector.Select(r.Context(), features))
		return
	}
	writeError(w, http.StatusServiceUnavailable, "strategy 服务不可用")
}

func (s *Server) handleStrategyList(w http.ResponseWriter, r *http.Request) {
	if s.Clients.Strategy != nil {
		resp, err := s.Clients.Strategy.Client.List(r.Context(), &besav1.Empty{})
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}
	if s.Local != nil {
		writeJSON(w, http.StatusOK, map[string]any{"strategies": s.Local.Strategy.Registry.All()})
		return
	}
	writeError(w, http.StatusServiceUnavailable, "strategy 服务不可用")
}

func (s *Server) handleExperienceRecall(w http.ResponseWriter, r *http.Request) {
	var q experience.Query
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&q); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.Clients.Experience != nil {
		retrieved, err := s.Clients.Experience.Recall(r.Context(), q)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, retrieved)
		return
	}
	if s.Local != nil {
		writeJSON(w, http.StatusOK, s.Local.Experience.Recall(r.Context(), q))
		return
	}
	writeError(w, http.StatusServiceUnavailable, "experience 服务不可用")
}

// ------------------------------ health / doctor ------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	status := map[string]any{"ok": true, "env": s.Base.Env(), "time": time.Now().UTC()}
	if s.Base.DB != nil {
		if err := s.Base.DB.Ping(r.Context()); err != nil {
			status["postgres"] = err.Error()
			status["ok"] = false
		} else {
			status["postgres"] = "ok"
		}
	}
	status["redis"] = s.Base.Redis != nil
	status["nats"] = s.NATS != nil
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleDoctor(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"env": s.Base.Env(), "config_dir": s.Base.Config.Dir()}
	if s.Local != nil {
		models := []map[string]any{}
		for _, m := range s.Local.Gateway.Registry().Models() {
			models = append(models, map[string]any{
				"key": m.Key, "provider": m.Provider, "model": m.Model,
				"capabilities": m.Capabilities.List(), "available": m.Available(),
				"unavailable_reason": m.UnavailableReason,
			})
		}
		aliases := []map[string]any{}
		for _, a := range s.Local.Gateway.Registry().Aliases() {
			aliases = append(aliases, map[string]any{"alias": a.Alias, "candidates": a.Candidates, "strategy": a.Strategy})
		}
		out["models"] = models
		out["aliases"] = aliases
		out["tools"] = s.Local.Tools.Registry.Names()
		out["health"] = s.Local.Gateway.Health().Snapshot()
		out["strategy_issues"] = s.Local.Strategy.Issues
		out["strategy_register_errors"] = errStrings(s.Local.Strategy.Errors)
	}
	out["postgres"] = s.Base.DB != nil
	out["redis"] = s.Base.Redis != nil
	out["nats"] = s.NATS != nil
	writeJSON(w, http.StatusOK, out)
}

// ------------------------------ 工具函数 ------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func errStrings(errs []error) []string {
	out := make([]string, 0, len(errs))
	for _, e := range errs {
		if e != nil {
			out = append(out, e.Error())
		}
	}
	return out
}

type orchestrateRequest struct {
	Task         string   `json:"task"`
	Strategy     string   `json:"strategy"`
	Participants []string `json:"participants"`
	Rounds       int      `json:"rounds"`
	SessionID    string   `json:"session_id"`
	Caller       string   `json:"caller"`
	Risk         string   `json:"risk"`
}

func multiagentRequest(req orchestrateRequest) multiagent.Request {
	return multiagent.Request{Task: req.Task, Strategy: req.Strategy, Participants: req.Participants,
		Rounds: req.Rounds, SessionID: req.SessionID, Caller: req.Caller, Risk: req.Risk}
}

// ------------------------------ 内存任务表 ------------------------------

type MemTasks struct {
	mu    sync.Mutex
	items map[string]postgres.Task
}

func NewMemTasks() *MemTasks { return &MemTasks{items: map[string]postgres.Task{}} }

func (m *MemTasks) Create(_ context.Context, task postgres.Task) (postgres.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if task.TaskID == "" {
		task.TaskID = ids.New("task")
	}
	now := time.Now().UTC()
	task.CreatedAt, task.UpdatedAt = now, now
	if task.Status == "" {
		task.Status = "queued"
	}
	m.items[task.TaskID] = task
	return task, nil
}

func (m *MemTasks) Get(_ context.Context, id string) (postgres.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.items[id]
	if !ok {
		return postgres.Task{}, fmt.Errorf("task not found")
	}
	return task, nil
}

func (m *MemTasks) UpdateStatus(_ context.Context, id, status, runID, errMsg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.items[id]
	if !ok {
		return fmt.Errorf("task not found")
	}
	task.Status = status
	if runID != "" {
		task.RunID = runID
	}
	task.Error = errMsg
	task.UpdatedAt = time.Now().UTC()
	m.items[id] = task
	return nil
}

func (m *MemTasks) Complete(_ context.Context, id, status string, result map[string]any, errMsg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.items[id]
	if !ok {
		return fmt.Errorf("task not found")
	}
	task.Status = status
	task.Result = result
	task.Error = errMsg
	task.UpdatedAt = time.Now().UTC()
	m.items[id] = task
	return nil
}

func (m *MemTasks) ListBySession(_ context.Context, sessionID string, limit int) ([]postgres.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []postgres.Task
	for _, task := range m.items {
		if sessionID == "" || task.SessionID == sessionID {
			out = append(out, task)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// PGTaskStore 适配 postgres.TaskRepo。
type PGTaskStore struct{ Repo *postgres.TaskRepo }

func (p *PGTaskStore) Create(ctx context.Context, task postgres.Task) (postgres.Task, error) {
	return p.Repo.Create(ctx, task)
}
func (p *PGTaskStore) Get(ctx context.Context, id string) (postgres.Task, error) {
	return p.Repo.Get(ctx, id)
}
func (p *PGTaskStore) UpdateStatus(ctx context.Context, id, status, runID, errMsg string) error {
	return p.Repo.UpdateStatus(ctx, id, status, runID, errMsg)
}
func (p *PGTaskStore) Complete(ctx context.Context, id, status string, result map[string]any, errMsg string) error {
	return p.Repo.Complete(ctx, id, status, result, errMsg)
}
func (p *PGTaskStore) ListBySession(ctx context.Context, sessionID string, limit int) ([]postgres.Task, error) {
	return p.Repo.ListBySession(ctx, sessionID, limit)
}

// StartOutboxRelay 把事件表里未发布的行发到 NATS。
// 事件表是 outbox，NATS 是传输；两者都是至少一次，消费方按 event_id 去重。
func (s *Server) StartOutboxRelay(ctx context.Context) {
	if s.Base.EventRepo == nil || s.NATS == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				rows, err := s.Base.EventRepo.FetchUnpublished(ctx, 200)
				if err != nil {
					continue
				}
				if len(rows) == 0 {
					continue
				}
				var ids []int64
				var failed []int64
				for _, row := range rows {
					raw, err := json.Marshal(row.Event)
					if err != nil {
						continue
					}
					subject := s.NATS.Prefix + ".events." + row.Event.Name
					// 用 JetStream 发布：事件可回放，且 Nats-Msg-Id 提供发布侧去重。
					if _, err := s.NATS.JS.Publish(ctx, subject, raw, jetstream.WithMsgID(row.Event.ID)); err != nil {
						failed = append(failed, row.ID)
						continue
					}
					ids = append(ids, row.ID)
				}
				if len(ids) > 0 {
					_ = s.Base.EventRepo.MarkPublished(ctx, ids)
				}
				if len(failed) > 0 {
					_ = s.Base.EventRepo.MarkPublishFailed(ctx, failed, "publish failed")
				}
			}
		}
	}()
}

// 确保日志器存在（服务启动时打印一次）。
func (s *Server) LogStart() {
	slog.Info("控制面启动", "addr", s.Base.Config.String("service.control_addr", ":8080"), "env", s.Base.Env())
}
