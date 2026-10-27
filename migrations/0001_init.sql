-- BesaAgent 分布式版初始 schema。
-- 迁移服务是 DDL 唯一来源；CREATE EXTENSION vector 需要部署前置（首次需超级用户）。

CREATE EXTENSION IF NOT EXISTS vector;

-- ---------------------------------------------------------------------------
-- 会话与消息（Python 版只有设计，这里落地）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS sessions (
    session_id text PRIMARY KEY,
    caller     text,
    title      text,
    status     text NOT NULL DEFAULT 'active',
    metadata   jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS messages (
    id           bigserial PRIMARY KEY,
    message_id   text NOT NULL UNIQUE,
    session_id   text NOT NULL REFERENCES sessions(session_id) ON DELETE CASCADE,
    seq          integer NOT NULL,
    role         text NOT NULL,
    content      text NOT NULL DEFAULT '',
    name         text,
    tool_call_id text,
    tool_calls   jsonb,
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (session_id, seq)
);
CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id, seq);

-- ---------------------------------------------------------------------------
-- 运行与步骤（run/step 是 checkpoint 的索引层）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS runs (
    run_id        text PRIMARY KEY,
    session_id    text,
    caller        text,
    stage         text,
    goal          text,
    status        text NOT NULL DEFAULT 'running',
    started_at    timestamptz NOT NULL DEFAULT now(),
    finished_at   timestamptz,
    cost_amount   numeric(18,8),
    cost_currency text,
    format_version integer NOT NULL DEFAULT 1,
    meta          jsonb NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX IF NOT EXISTS idx_runs_session ON runs(session_id, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_runs_status ON runs(status, started_at DESC);

CREATE TABLE IF NOT EXISTS run_steps (
    run_id     text NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
    step_index integer NOT NULL,
    node_id    text,
    summary    text,
    tools_used jsonb,
    idem_keys  jsonb,
    status     text,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, step_index)
);

-- append-only 快照。needs_review 必须能被 unfinished() 查到。
CREATE TABLE IF NOT EXISTS checkpoints (
    id             bigserial PRIMARY KEY,
    run_id         text NOT NULL,
    format_version integer NOT NULL DEFAULT 1,
    step_index     integer NOT NULL DEFAULT 0,
    node_id        text,
    status         text NOT NULL,
    state          jsonb NOT NULL DEFAULT '{}'::jsonb,
    artifacts      jsonb NOT NULL DEFAULT '{}'::jsonb,
    session_id     text,
    caller         text,
    created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_checkpoints_run ON checkpoints(run_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_checkpoints_status ON checkpoints(status, created_at DESC);

-- ---------------------------------------------------------------------------
-- 工具执行权威记录。Redis 只是快路径。
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS tool_executions (
    id               bigserial PRIMARY KEY,
    scope            text NOT NULL,
    idem_key         text NOT NULL,
    tool_name        text NOT NULL,
    side_effect      text,
    args_digest      text,
    state            text NOT NULL,
    owner            text,
    trace_id         text,
    session_id       text,
    caller           text,
    started_at       timestamptz NOT NULL DEFAULT now(),
    finished_at      timestamptz,
    result           text,
    result_truncated boolean NOT NULL DEFAULT false,
    error            text,
    output_bytes     integer NOT NULL DEFAULT 0,
    artifact_uri     text,
    UNIQUE (scope, idem_key)
);
CREATE INDEX IF NOT EXISTS idx_tool_exec_state ON tool_executions(state, started_at);

-- ---------------------------------------------------------------------------
-- 用量与事件（事件表兼作 transactional outbox）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS usage (
    id                  bigserial PRIMARY KEY,
    trace_id            text,
    alias               text,
    model_key           text,
    provider            text,
    model               text,
    input_tokens        integer,
    output_tokens       integer,
    cached_input_tokens integer,
    cost_amount         numeric(18,8),
    currency            text,
    degraded            boolean NOT NULL DEFAULT false,
    failed              boolean NOT NULL DEFAULT false,
    attempt_index       integer NOT NULL DEFAULT 0,
    session_id          text,
    caller              text,
    occurred_at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_usage_session ON usage(session_id, occurred_at DESC);
CREATE INDEX IF NOT EXISTS idx_usage_model ON usage(model_key, occurred_at DESC);
CREATE INDEX IF NOT EXISTS idx_usage_alias ON usage(alias, occurred_at DESC);

CREATE TABLE IF NOT EXISTS usage_drops (
    id             bigserial PRIMARY KEY,
    dropped_count  integer NOT NULL,
    reason         text NOT NULL DEFAULT 'ledger_overflow',
    occurred_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS events (
    id               bigserial PRIMARY KEY,
    event_id         text NOT NULL UNIQUE,
    name             text NOT NULL,
    trace_id         text,
    session_id       text,
    caller           text,
    alias            text,
    subject          text,
    outcome          text,
    attempt_index    integer NOT NULL DEFAULT 0,
    payload          jsonb NOT NULL DEFAULT '{}'::jsonb,
    occurred_at      timestamptz NOT NULL DEFAULT now(),
    published_at     timestamptz,
    publish_attempts integer NOT NULL DEFAULT 0,
    last_error       text,
    partition_key    text
);
CREATE INDEX IF NOT EXISTS idx_events_trace ON events(trace_id, occurred_at);
CREATE INDEX IF NOT EXISTS idx_events_session ON events(session_id, occurred_at);
CREATE INDEX IF NOT EXISTS idx_events_name ON events(name, occurred_at);
CREATE INDEX IF NOT EXISTS idx_events_subject ON events(subject, outcome);
CREATE INDEX IF NOT EXISTS idx_events_unpublished ON events(occurred_at) WHERE published_at IS NULL;

-- ---------------------------------------------------------------------------
-- 大结果 / 产物
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS artifacts (
    artifact_id  text PRIMARY KEY,
    owner_kind   text NOT NULL,
    owner_id     text NOT NULL,
    sha256       text NOT NULL,
    size_bytes   bigint NOT NULL,
    content_type text,
    storage_uri  text NOT NULL,
    preview      text,
    truncated    boolean NOT NULL DEFAULT false,
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (owner_kind, owner_id, sha256)
);

-- ---------------------------------------------------------------------------
-- 记忆与向量
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS memory (
    id                bigserial PRIMARY KEY,
    memory_id         text NOT NULL UNIQUE,
    tenant_id         text NOT NULL DEFAULT 'default',
    scope             text NOT NULL,
    owner_id          text,
    schema_version    integer NOT NULL DEFAULT 1,
    session_id        text,
    caller            text,
    content           text NOT NULL,
    content_hash      text NOT NULL,
    summary           text,
    kind              text NOT NULL,
    category          text,
    tags              jsonb NOT NULL DEFAULT '[]'::jsonb,
    sensitivity       text,
    source_kind       text,
    source_session_id text,
    source_agent      text,
    source_trace_id   text,
    confidence        double precision NOT NULL DEFAULT 0.5,
    importance        double precision NOT NULL DEFAULT 0.5,
    state             text NOT NULL DEFAULT 'active',
    version           integer NOT NULL DEFAULT 1,
    supersedes_id     text,
    superseded_by     text,
    merged_into       text,
    valid_from        timestamptz NOT NULL DEFAULT now(),
    valid_to          timestamptz,
    access_count      integer NOT NULL DEFAULT 0,
    used_count        integer NOT NULL DEFAULT 0,
    evidence_count    integer NOT NULL DEFAULT 0,
    weight            double precision NOT NULL DEFAULT 1.0,
    last_accessed_at  timestamptz,
    last_decayed_at   timestamptz,
    meta              jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_memory_scope_hash ON memory(tenant_id, scope, content_hash) WHERE state = 'active';
CREATE INDEX IF NOT EXISTS idx_memory_query ON memory(tenant_id, scope, kind, state, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_memory_decay ON memory(state, last_accessed_at);
CREATE INDEX IF NOT EXISTS idx_memory_session ON memory(source_session_id);

CREATE TABLE IF NOT EXISTS memory_op_log (
    id              bigserial PRIMARY KEY,
    op_id           text NOT NULL UNIQUE,
    memory_id       text,
    op              text NOT NULL,
    before_hash     text,
    after_hash      text,
    reason          text,
    actor           text,
    source_trace_id text,
    scope           text,
    tenant_id       text NOT NULL DEFAULT 'default',
    payload         jsonb NOT NULL DEFAULT '{}'::jsonb,
    occurred_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_memory_op_log_memory ON memory_op_log(memory_id, occurred_at);
CREATE INDEX IF NOT EXISTS idx_memory_op_log_op ON memory_op_log(op, occurred_at);
CREATE INDEX IF NOT EXISTS idx_memory_op_log_scope ON memory_op_log(scope, occurred_at);

CREATE TABLE IF NOT EXISTS vector_spaces (
    space         text PRIMARY KEY,
    dim           integer NOT NULL,
    metric        text NOT NULL DEFAULT 'cosine',
    model_key     text,
    index_options jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS embeddings_768 (
    id         bigserial PRIMARY KEY,
    space      text NOT NULL,
    owner_kind text NOT NULL,
    owner_id   text NOT NULL,
    model_key  text,
    embedding  vector(768) NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (space, owner_kind, owner_id)
);
CREATE INDEX IF NOT EXISTS idx_embeddings_768_hnsw ON embeddings_768 USING hnsw (embedding vector_cosine_ops) WITH (m = 16, ef_construction = 64);

CREATE TABLE IF NOT EXISTS embeddings_1024 (
    id         bigserial PRIMARY KEY,
    space      text NOT NULL,
    owner_kind text NOT NULL,
    owner_id   text NOT NULL,
    model_key  text,
    embedding  vector(1024) NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (space, owner_kind, owner_id)
);
CREATE INDEX IF NOT EXISTS idx_embeddings_1024_hnsw ON embeddings_1024 USING hnsw (embedding vector_cosine_ops) WITH (m = 16, ef_construction = 64);

-- ---------------------------------------------------------------------------
-- 控制面任务与工作流
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS tasks (
    task_id      text PRIMARY KEY,
    session_id   text,
    caller       text,
    kind         text NOT NULL DEFAULT 'agent',
    stage        text,
    goal         text NOT NULL,
    status       text NOT NULL DEFAULT 'queued',
    run_id       text,
    workflow_id  text,
    result       jsonb,
    error        text,
    lease_owner  text,
    lease_until  timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status, created_at);
CREATE INDEX IF NOT EXISTS idx_tasks_session ON tasks(session_id, created_at DESC);

CREATE TABLE IF NOT EXISTS workflow_runs (
    workflow_run_id text PRIMARY KEY,
    workflow_id     text NOT NULL,
    version         text,
    origin          text NOT NULL DEFAULT 'predefined',
    status          text NOT NULL DEFAULT 'running',
    input           jsonb NOT NULL DEFAULT '{}'::jsonb,
    result          jsonb,
    created_at      timestamptz NOT NULL DEFAULT now(),
    finished_at     timestamptz
);

CREATE TABLE IF NOT EXISTS workflow_nodes (
    workflow_run_id text NOT NULL REFERENCES workflow_runs(workflow_run_id) ON DELETE CASCADE,
    node_id         text NOT NULL,
    agent           text NOT NULL,
    status          text NOT NULL DEFAULT 'pending',
    attempt         integer NOT NULL DEFAULT 0,
    inputs          jsonb NOT NULL DEFAULT '{}'::jsonb,
    outputs         jsonb,
    reason          text,
    started_at      timestamptz,
    finished_at     timestamptz,
    PRIMARY KEY (workflow_run_id, node_id)
);

CREATE TABLE IF NOT EXISTS resource_leases (
    resource   text NOT NULL,
    holder     text NOT NULL,
    expires_at timestamptz NOT NULL,
    PRIMARY KEY (resource, holder)
);

-- ---------------------------------------------------------------------------
-- 策略评测与经验库
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS strategy_eval (
    id             bigserial PRIMARY KEY,
    profile_key    text NOT NULL,
    versions       jsonb NOT NULL DEFAULT '{}'::jsonb,
    quality        double precision NOT NULL,
    cost           double precision NOT NULL DEFAULT 0,
    latency        double precision NOT NULL DEFAULT 0,
    samples        integer NOT NULL DEFAULT 1,
    benchmark_set  text NOT NULL DEFAULT 'default',
    measured_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_strategy_eval_profile ON strategy_eval(profile_key, measured_at DESC);

CREATE TABLE IF NOT EXISTS experiences (
    experience_id      text PRIMARY KEY,
    category           text NOT NULL,
    scenario           text NOT NULL,
    symptom            text NOT NULL,
    conditions         jsonb NOT NULL DEFAULT '{}'::jsonb,
    evidence           jsonb NOT NULL DEFAULT '[]'::jsonb,
    root_cause         text,
    cause_state        text NOT NULL DEFAULT 'unverified',
    recovery_strategy  text,
    scope              jsonb NOT NULL DEFAULT '{}'::jsonb,
    validation         jsonb NOT NULL DEFAULT '{}'::jsonb,
    state              text NOT NULL DEFAULT 'unverified',
    expires_when       jsonb,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    last_applied_at    timestamptz
);
CREATE INDEX IF NOT EXISTS idx_experiences_query ON experiences(state, category, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_experiences_scenario ON experiences(scenario);

CREATE TABLE IF NOT EXISTS experience_applications (
    id            bigserial PRIMARY KEY,
    experience_id text NOT NULL,
    trace_id      text,
    outcome       text NOT NULL,
    metric_before double precision,
    metric_after  double precision,
    applied_at    timestamptz NOT NULL DEFAULT now()
);
