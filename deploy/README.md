# 部署

## 本地 / 单机

```bash
docker compose up --build -d          # 全套依赖 + 8 个服务
```

或直接：

```bash
besa --env prod serve --role all      # 一个进程里跑全部角色
```

## Kubernetes

`k8s/besa.yaml` 提供最小可运行清单（Deployment + Service + HPA）。
它假设以下**外部依赖**已经就绪：

- PostgreSQL 15+ 且启用 `pgvector`
- Redis 7+
- NATS 2.10+ 且开启 JetStream
- S3/MinIO（工具大结果与产物）

部署前修改：

1. `Secret/besa-secrets`：模型密钥、Postgres DSN、Redis/NATS URL、MinIO 凭据；
2. `ConfigMap/besa-config`：`BESA_ENV=prod` 与（可选）`BESA_WORKDIR`；
3. `image: besa-agent:local` → 你的镜像仓库地址；
4. `besa-tool` 的 `emptyDir` 工作目录：生产建议挂共享 PVC，或只允许对象存储落盘。

```bash
kubectl apply -f deploy/k8s/besa.yaml
kubectl -n besa get pods,svc
```

### 扩容要点

| 角色 | 可复制性 | 注意 |
|---|---|---|
| `agent` | ✅ 多副本竞争 JetStream durable | 状态在 Postgres；同 durable 名 |
| `gateway` | ✅ | 熔断表按副本独立；限流走 Redis |
| `tool` | ✅ | 工作目录/对象存储是共享资源 |
| `control` | ✅ | 多副本共享 tasks 表；SSE 各自订阅 NATS |
| `orchestrator` | ⚠ | 同一工作流运行应单调度器（未做跨副本租约） |
| `memory` / `strategy` / `experience` | ✅ | 记忆维护任务建议单副本或加分布式锁 |

### 探针

- 控制面：`GET /healthz`（检查 Postgres 连通）
- gRPC 服务：`tcpSocket` 探针（端口 9001–9008）
- 启动顺序：`gateway/tool/memory/strategy/experience` → `agent`/`orchestrator` → `control`
  （`agent` 会重试拉取工具目录 30 秒，短暂乱序不会致命）
