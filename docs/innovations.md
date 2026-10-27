# 三个创新点在 Go 版中的落点

原项目的三个创新点分别对应组件层、编排层、优化层。这里给出每一点在代码中的具体实现位置，
以及“哪些是真正跑起来的、哪些还只是元数据”。

## 创新点 1：组件策略库与 Agent 动态配置

**目标**：同一个 Agent 框架适配不同任务；策略可注册、可校验、可选择、可评测，而不是几个开关。

| 能力 | 实现 |
|---|---|
| 组件七类 | `internal/strategy/types.go` 的 `ComponentKind` |
| 策略六要素 | `StrategySpec`：接口、版本、输入、提供、前置条件、成本、场景、指标 |
| 注册/不可用 | `Registry.Register / MarkUnavailable`；不可用必须给原因 |
| 组合契约 | `contracts.go` 五条规则：读写冲突、状态压缩、交叉验证、候选比较、无状态记忆 |
| 模板 | `templates.go`：baseline 打底 + 模板覆盖 + 输入资产裁剪 |
| 选择顺序 | `selector.go`：高风险合法集 → 模板 → 规则 → 评测 →（拒绝或降级） |
| 评测数据 | `strategy_eval` 表；版本不匹配视为无数据 |
| 服务化 | `strategyd` gRPC；控制面 `/v1/strategy/select`、`/v1/strategy/list` |
| 安全降级 | 高风险无“合法 ∩ 已验证”组合时 **refused**，不降级 |

> 当前策略的**实现**仍在 context / memory / tool / agent 各自包内；
> 策略层只产出 `AgentProfile`（绑定 + 版本），不构造实例。这避免了“策略层变成第二个工厂”。

## 创新点 2：Agent 池驱动的动态 Workflow

**目标**：先理解任务，再根据可用能力构建/裁剪流程；而不是先定流程再塞任务。

| 能力 | 实现 |
|---|---|
| Agent Pool | `internal/workflow/types.go`：能力标签、IO 契约、前置条件、资产、并行性、副作用、独占资源、成本估计 |
| 三段漏斗 | `Pool.Filter`（硬过滤）→ `Rank`（分数）→ `max_candidates` |
| DAG 模型 | `Node / Edge / NodeState`；数据边与资源边分开 |
| Planner | `planner.go`：`PlanAuto` 生成候选图；模板走 `PlanTemplate` |
| Validator | `validate.go`：依赖闭合 → 无环 → IO → 权限 → 资源；**fail-closed** |
| Scheduler | `engine.go`：就绪判定、并行额度、资源计数、重试、可选节点跳过、下游跳过传播 |
| 检查点 | 图级节点写 `checkpoints(node_id="<wf>_<node>")`；恢复时可定位到节点 |
| 三种模式 | 预定义（YAML）/ 模板 / auto（默认关闭，可配置放行） |
| 服务化 | `workflowd` gRPC + NATS `workflow.run` |

**与“让 LLM 输出一张流程图”的区别**：能力匹配、依赖推导、DAG 校验、资源调度、局部恢复
都是确定性代码；LLM 只可能出现在 Planner 的候选生成里，且必须过 Validator。

## 创新点 3：失败案例驱动的检索与经验协同

**目标**：失败 → 分类 → 证据 → 归因 → 恢复 → 经验 → 检索 → 效果验证的闭环。

| 能力 | 实现 |
|---|---|
| 失败检测 | `experienced.OnFailure`；**needs_review 不算失败** |
| 分层错误分类 | `classify.go` 八类 + 可恢复性（自动/外部动作/不可恢复） |
| 证据 | `Evidence{kind, source, detail, weight}`；必须可回指 |
| 假设分层 | `correlation → hypothesis → verified`；Critic 不能直接写 verified |
| 定向恢复 | `recover.go`：retry / wait_recheck / adjust / rollback / skip / request_auth / abort；重试不是默认 |
| 经验对象 | `types.go` 的 `Experience`：场景、症状、条件、证据、根因、恢复、适用范围、成功率、失效条件 |
| 门控 | 去重、证据、范围、质量、版本失效；未验证默认不入库 |
| 检索 | 只返回 `verified`；scope 匹配；按成功率排序；冲突表达预留 |
| 回收 | 连续无效应用后 `degraded → deactivated`，不删除 |
| 服务化 | `experienced` gRPC；控制面 `/v1/experience/recall` |

**闭环的验收**：经验带 `samples / success_rate / reproducible`；应用后记录
`experience_applications` 并回写成功率；无效时降权。没有这几步，“经验总结”只是一段文本。

## 三者如何协同

```
任务 → Strategy.Select(创新点1) 得到 AgentProfile
     → Workflow.Plan/Validate/Run(创新点2) 选择节点、建图、调度
     → 每个节点是一次 AgentRuntime.RunOne
     → 失败 → Experience.OnFailure(创新点3) 给出恢复动作
              → 成功后 Settle 为经验，下一次 Recall 注入上下文
     → 评测记录写 strategy_eval，反过来影响创新点1的选择
```

依赖方向是单向的：编排调用单 agent；单 agent 不知道被编排；经验只消费结构化失败与轨迹，
不反向修改编排逻辑。
