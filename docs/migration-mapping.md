# Python → Go 迁移映射

原项目 `../besa-agent`（Python，27k 行，185 个模块）→ 本仓库（Go）。
映射按**职责**而非文件一一对应；同一条纪律只允许有一个实现。

## 1. 包级映射

| Python | Go | 状态 | 说明 |
|---|---|---|---|
| `src/foundation/{settings,errors,ids,clock,logging}` | `internal/foundation/{config,errs,ids,clock,logx}` | ✅ | 配置支持 `<env>.yaml` 深度合并、`${VAR:-default}`、点分键含点的最长匹配 |
| `src/foundation/{db,database}` + `apps/*/storage` | `internal/store/postgres` | ✅ | pgx + 内嵌 SQL 迁移；CLI 用 `memstore` |
| `src/provider/{types,base,errors}` | `internal/domain` + `internal/provider/{types,errors}` | ✅ | 类型与适配器拆分 |
| `src/provider/{openai,dashscope,vllm,mock}` | `internal/provider/{openai,dashscope,vllm,mock}` + `compat.go` | ✅ | OpenAI 兼容为基准；mock 确定性 |
| `src/gateway/*` | `internal/gateway` | ✅ | registry/router/retry/fallback/health/rate_limit/cost/usage/errors |
| `src/agent/{base,state,stages,router,executor,runtime}` + 7 个 prompt | `internal/agent` | ✅ | 阶段提示词、路由、循环、预算、续跑、prepare 钩子 |
| `src/context/*` | `internal/context` | ✅ | budget / turn&step 切分 / L1–L3 / dedupe / 组装 |
| `src/tool/*` | `internal/tool` + `builtin` | ✅ | 权限/注入/幂等/租约/脱敏/落盘/schema |
| `src/tool/idempotency.py` | `internal/tool/idempotency.go` + `store/redis` + `store/postgres` | ✅ | 两阶段 claim；Redis 快路径 |
| `src/memory/*` | `internal/memory` + `store/postgres/memory.go` | ✅ | 三道闸门、四分支、衰减、维护、pgvector |
| `src/strategy/*` | `internal/strategy` | ✅ | 注册/契约/模板/选择器/评测记录 |
| `src/multiagent/*`（空桩） | `internal/multiagent` | ✅ | 并行/委员会/辩论/层级 |
| `src/workflow/*`（仅设计） | `internal/workflow` | ✅ | Pool / Planner / Validator / Scheduler / Engine |
| `src/experience/*`（仅设计） | `internal/experience` | ✅ | 分类、恢复、门控、经验对象、检索、降级 |
| `src/repo/*` | `internal/store/postgres` + 接口 | ✅ | 会话/消息/运行/检查点/工具执行/用量/事件/记忆/经验/工作流 |
| `src/event/*`（空桩） | `internal/event` | ✅ | 统一事件形状、内存总线、NATS、outbox |
| `src/skill/*`（空桩） | — | ⏳ | 未实现；接口位置已在 tool/编排层预留 |
| `apps/cli` | `internal/app` + `cmd/besa` | ✅ | run/chat/doctor/strategy/workflow/migrate/serve |
| `apps/server`（待实现） | `internal/service/controld` | ✅ | REST + SSE |
| `apps/mcp`（待实现） | — | ⏳ | 未实现 |
| `configs/` | `configs/{base,dev,test,prod}.yaml` + `workflows/` | ✅ | 新增 nats/artifact/service 段 |
| `migrations/`（Alembic） | `migrations/*.sql` + `embed.go` | ✅ | 单一 DDL 来源 |

## 2. 关键类型映射

| Python | Go |
|---|---|
| `Message(role, content, tool_calls, tool_call_id)` | `domain.Message`（`Parts` 承载多模态） |
| `Usage(input_tokens: int \| None)` | `domain.Usage{InputTokens *int}` |
| `Cost(amount: Decimal \| None)` | `domain.Cost{Amount *decimal.Decimal}` |
| `GatewayResult` | `domain.GatewayResult`（content/model 在 `Response` 里） |
| `AttemptRecord` | `domain.AttemptRecord` |
| `ToolDefinition` | `domain.ToolDefinition`（tool 包内是别名） |
| `ToolInvocation / ToolResult` | `tool.Invocation / tool.Result` |
| `TaskRun / AgentStep / RunOutcome` | `agent.TaskRun / AgentStep / RunOutcome` |
| `CheckpointView` | `agent.Checkpoint` |
| `MemoryItem` | `memory.Item` |
| `AgentProfile / SelectionDecision` | `strategy.AgentProfile / SelectionDecision` |
| `Workflow / Node / Edge` | `workflow.Workflow / Node / Edge` |
| `Experience / Hypothesis` | `experience.Experience / Hypothesis` |

## 3. 语义对照（最容易改错的地方）

| 语义 | Python | Go |
|---|---|---|
| 「未知」用量/成本 | `None` | `nil` 指针；**禁止用零值代替** |
| 流式调用 | `async for` | `<-chan domain.StreamEvent`；选链在调用瞬间完成 |
| 并发隔离的调用栈 | `contextvars` | `context.Context` 上的 `CallState` |
| 取消 | `asyncio.CancelledError` + 进程内集合 | `context.Context` + Redis 取消键 + 进程内 `Cancel` |
| 工具不确定性 | 读取时按租约判定 | 同左；`ExecutionRepo.Find` 计算，不写回 |
| 运行内压缩 | `prepare` 钩子 + `unit="step"` | `RunHooks.Prepare` + `Unit:"step"` |
| 预算 | `_Budget` + `remaining_s` 回调 | `runBudget` + `RunHooks.Remaining/StopReason` |
| 路由打分 | 命中数 / 固定饱和数 | 同左 |
| 阶段是否数据驱动 | 配置 + 内置 prompt | 同左；`BuildStages` |
| 收尾 | 快照 + 事件（其余未落地） | 快照 + 事件 + `Context.Settle`（旁路） |
| 会话历史 | 进程内（未落地 repo） | Postgres `sessions/messages`；单机 `memstore` |

## 4. 有意的差异与增强

1. **真正的消息落库**：原 Python 的 `repo/session`、`repo/message` 是空桩；Go 版落库，
   于是跨进程续跑、重启后历史、`message_id` 回捞都成立。
2. **事件表兼 outbox**：原项目事件只在内存缓冲后批量落库；Go 版新增 `published_at`，
   由 relay 发布到 NATS，支持跨进程 SSE 与至少一次投递。
3. **NATS 作业队列**：原项目只有进程内并发；Go 版用 durable consumer 支持多副本 worker。
4. **取消跨进程**：Redis 取消键 + NATS `agent.cancel` + 轮询，agentd 会在下一个检查点退出。
5. **对象存储**：工具大结果不再只写本地路径（换节点读不到），可写 MinIO/S3。
6. **工作流真正实现**：Pool/Planner/Validator/Scheduler/Engine 从设计文档落到代码，
   并保留 fail-closed、局部重试、可选节点跳过、图级检查点。
7. **多 agent 真正实现**：并行/委员会/辩论/层级，均只依赖单 agent 的 `RunOne`。
8. **经验闭环真正实现**：分类/恢复/门控/检索/降级；未验证假设默认不入库。
9. **成本语义修正**：无工具调用时预算不会把“未知成本”压成 0（Go 版显式区分）。
10. **路由默认值**：按原文档实现期修订，`on_unknown` 默认 `ask`（最保守）+ 告警。

## 5. 尚未迁移

| 项目 | 原因 | 建议 |
|---|---|---|
| `skill` 包 | 原项目为空桩，需求在 `docs/tool` 里 | 按 `skill` 表 + 版本化定义实现，执行仍走 tool |
| MCP server | 原 `apps/mcp` 为空 | 可在 `controld` 加 MCP 适配层 |
| 精确 tokenizer | 原项目一期不做 | 接入厂商 tokenizer 后替换 `gateway.EstimateTextTokens` |
| BM25 / SQL / 查询分解检索 | 原策略登记为 unavailable | 需要倒排索引/指标库 schema |
| 交叉验证 | 原策略登记为 unavailable | 启用 multiagent 后接 `workflow` |
| 审批流 | 原策略登记为 unavailable | 接外部工单系统 |
| 策略离线评测 worker | 选择器已就绪 | 消费 `strategy.eval.requested`，把结果写 `strategy_eval` |

## 6. 迁移时保留的“坑注释”

原项目注释里记录了大量实现期踩坑（例如 `AgentExecutor` 的钩子必须按调用传参、
`needs_review` 必须能被 `unfinished()` 查到、路由分母用饱和数、Windows GBK 控制台不能输出 ✓）。
这些结论已经体现在 Go 代码的结构与注释中，建议阅读：

- `internal/agent/runtime.go` / `executor.go`：并发、预算、续跑；
- `internal/tool/idempotency.go`：两阶段与 read-time uncertain；
- `internal/tool/executor.go`：顺序约束与“权限先于幂等”；
- `internal/context/compact.go`：为什么按步切、为什么 L1 不删消息；
- `internal/gateway/gateway.go`：唯一计数点、降级判定顺序、流式已提交。
