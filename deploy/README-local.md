# 本地部署（Windows + Docker Desktop Kubernetes）

本目录的 `k8s/local/` 是一套**自包含**的本地栈：Postgres(pgvector) + Redis + NATS JetStream +
BesaAgent 的 8 个角色。默认使用 mock 模型，不需要任何 API Key。

> 已在 Docker Desktop Kubernetes（4 节点，containerd 2.3）实际部署验证：
> 迁移 23 张表、NATS 任务链路、工具幂等、SSE 均跑通。

## 0. 前置

- Docker Desktop 已开启 Kubernetes（Settings → Kubernetes → Enable Kubernetes）
- `kubectl` 指向 `docker-desktop` context
- Go 1.26+（用于交叉编译 Linux 静态二进制）

```powershell
kubectl config use-context docker-desktop
kubectl get nodes
```

## 1. 构建镜像

本机 Docker 配置了 USTC 镜像源，当前拉取 Docker Hub 会 EOF。因此采用**无基础镜像**构建：
先用本机 Go 交叉编译静态二进制，再用 `scratch` 打包。无需拉取 `golang`/`alpine`。

```powershell
cd E:\aloha\AIComponents\besa-agent-go

$env:CGO_ENABLED="0"; $env:GOOS="linux"; $env:GOARCH="amd64"
New-Item -ItemType Directory -Force dist | Out-Null
go build -trimpath -ldflags "-s -w" -o dist/besa-linux ./cmd/besa

# CA 证书（真实 HTTPS 模型调用需要；mock 模式不影响）
if (!(Test-Path deploy/certs/ca-certificates.crt)) {
  New-Item -ItemType Directory -Force deploy/certs | Out-Null
  Invoke-WebRequest https://curl.se/ca/cacert.pem -OutFile deploy/certs/ca-certificates.crt
}

docker build -f deploy/Dockerfile.scratch -t besa-agent:v4 .
```

> **为什么镜像 tag 不是 `latest`**：Kubernetes 对 `IfNotPresent` 会复用 containerd 里已有的 tag。
> 重新构建后请递增 tag（或 `kubectl set image` + `rollout restart`），否则可能仍跑旧代码。

## 2. 准备基础设施镜像

如果 `docker pull` 因镜像源失败，可用 Docker Hub 真实域名直连，再打回短名：

```powershell
foreach ($img in @("library/redis:7-alpine","library/nats:2.10-alpine","pgvector/pgvector:pg16","library/busybox:1.36")) {
  docker pull "registry-1.docker.io/$img"
  docker tag  "registry-1.docker.io/$img" $img
}
```

Docker Desktop 的 Docker 镜像与 Kubernetes containerd 共享，构建/拉取后的镜像可直接被 Pod 使用
（`imagePullPolicy: IfNotPresent`）。

## 3. 部署

```powershell
kubectl apply -k deploy/k8s/local
kubectl -n besa-local get pods -w
```

顺序由 initContainers 保证：

```
postgres/redis/nats → gateway/tool → agent → memory → orchestrator → control
```

全部 Ready 大约 30–60 秒。检查：

```powershell
kubectl -n besa-local get pods
kubectl -n besa-local get pvc
kubectl -n besa-local logs deploy/besa-control --tail=20
```

## 4. 访问

控制面是 ClusterIP，本地用 port-forward：

```powershell
kubectl -n besa-local port-forward svc/besa-control 8080:8080
```

另开一个终端：

```powershell
curl http://localhost:8080/healthz
curl http://localhost:8080/v1/doctor
```

提交任务：

```powershell
curl -X POST http://localhost:8080/v1/tasks `
  -H "Content-Type: application/json" `
  -d '{"goal":"帮我为登录接口设计测试用例","session_id":"s1","stage":"test_case"}'

curl "http://localhost:8080/v1/tasks?session_id=s1"
```

SSE：

```powershell
curl -N "http://localhost:8080/v1/events/stream?session_id=s1"
```

## 5. 验证清单（已实际通过）

| 检查 | 命令 | 期望 |
|---|---|---|
| 8 服务 + 3 基础设施 Ready | `kubectl -n besa-local get pods` | 11 个 Running |
| 迁移与扩展 | `kubectl -n besa-local exec deploy/postgres -- psql -U besa -d besa_agent -Atc "select tablename from pg_tables where schemaname='public'"` | 含 `tool_executions`/`checkpoints`/`embeddings_768` 等 23 表 |
| 异步任务 | POST `/v1/tasks` 后轮询 | `status=done`，`run_id` 非空 |
| 工具幂等 | 连续两次相同 write | `executed` → `reused` |
| 路径逃逸 | read `/etc/passwd` | `refused`（path_traversal） |
| SSE | 订阅 `session_id` | 收到 agent + gateway 事件 |
| 工作流 | POST `/v1/workflow/run` | `SUCCEEDED` |

## 6. 常用运维

```powershell
# 滚动重启（改了代码并重新 build 同 tag 后）
kubectl -n besa-local rollout restart deploy/besa-gateway deploy/besa-tool deploy/besa-agent `
  deploy/besa-memory deploy/besa-strategy deploy/besa-experience deploy/besa-orchestrator deploy/besa-control

# 查看某服务日志
kubectl -n besa-local logs deploy/besa-agent --tail=100 -f

# 进入 Postgres
kubectl -n besa-local exec -it deploy/postgres -- psql -U besa -d besa_agent

# 进入 Redis
kubectl -n besa-local exec -it deploy/redis -- redis-cli

# 清理（注意：PVC 默认保留数据）
kubectl delete -k deploy/k8s/local
kubectl -n besa-local delete pvc --all
```

## 7. 常见问题

| 现象 | 原因 | 处理 |
|---|---|---|
| `docker build` 报 `docker.mirrors.ustc.edu.cn ... EOF` | 本机 Docker Hub 镜像源不可用 | 用 `Dockerfile.scratch`（不拉基础镜像）；或 `docker pull registry-1.docker.io/...` 直连后 retag |
| Pod `ImagePullBackOff` | 镜像不在共享存储 / tag 不存在 | 确认 `docker images | grep besa-agent`；递增 tag 并 `kubectl apply -k` |
| 改了代码但 Pod 还是旧行为 | `IfNotPresent` 复用了旧 tag | 递增 tag，或 `kubectl rollout restart` 前先 `docker tag` 新镜像 |
| `wait-postgres` 一直 waiting | 早期用 `nc` 检查时对端未就绪/超时 | 清单已改用 `pg_isready`；确认 postgres Pod Ready |
| Task 一直 `queued` | agentd 没消费 NATS | `kubectl logs deploy/besa-agent`，确认 NATS 与 durable consumer |
| Control 返回 503 | 对应服务客户端未就绪 | 先 `get pods`；控制面用懒连接，服务起来后恢复 |
| `runs` 表为空 | v2 之前的镜像未写入 | 使用 v4 镜像；worker 会 `RunRepo.Create/Finish` |
| 数据在 Pod 重建后丢失 | local-path 卷是节点本地 | 本地演示可接受；生产用托管 PG/对象存储 |

## 8. 配置

- 配置来自镜像内 `configs/prod.yaml`（`BESA_ENV=prod`），覆盖 `base.yaml`。
- 环境变量来自 `k8s/local/20-besa-config.yaml` 的 ConfigMap/Secret。
- 默认候选链是 `runtime-mock` / `emb-mock`；接真实模型：
  1. 在 Secret 填 `DEEPSEEK_API_KEY` 等；
  2. 修改 `configs/prod.yaml` 的 `gateway.aliases.runtime.default.candidates`；
  3. 重新构建镜像并递增 tag/滚动更新。
- 未部署 MinIO：`BESA_S3_ENDPOINT=""`，工具大结果落 `besa-tool` 的 `/workspace/.tool_out`
  （同 Pod 内可回读；Pod 重建丢失）。需要持久化时加 MinIO 并设置 endpoint。
