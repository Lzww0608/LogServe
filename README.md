# LogServe

LogServe 是一个基于 shared log 的 AI runtime 原型。它把 task、workflow、actor 和 LLM serving 的状态先写入追加日志，再从日志重建控制面视图。这个项目重点展示的是运行时机制：可恢复调度、可回放状态、actor 串行化、模型缓存感知调度、checkpoint、故障恢复和实验验证。

## 项目特色

- log-first control plane：控制面先写 `TaskSubmitted`、workflow、actor、LLM 等事件，再更新 metadata view。服务重启后可以从 shared log replay。
- workflow DAG runtime：Python `@workflow` 会被转成 DAG，控制面只调度依赖已满足的 step，并处理 retry、timeout、result ref 和结果去重。
- stateful actor runtime：actor 请求进入 mailbox，按 `command_seq` 串行应用；owner worker 失联后用 epoch fencing 拒绝旧完成。
- LLM serving：支持 mock LLM、vLLM OpenAI-compatible adapter、模型注册、worker cache 上报、file-backed checkpoint cache 和 locality-aware scheduling。
- 可验证优化：scheduler v2、worker batch poll/long-poll、PostgreSQL async materializer、metadata checkpoint、physical compaction、msgpack executor、CRC32C checksum、mmap read 都有测试或验收记录。

## 目录结构

```text
cmd/                  logd、control、worker、dev runner、CLI
proto/                gRPC 协议
internal/logstore     分段 append-only shared log
internal/control      控制面、调度、workflow/actor/LLM 状态机
internal/metadata     内存和 PostgreSQL materialized view
internal/worker       worker、本地 executor pool、Python 执行桥
internal/objectstore  本地和 S3-compatible result store
sdk/python/logserve   Python SDK
executor/python       Python 函数执行进程
deployments/          Docker Compose 与 Kubernetes 示例
scripts/              测试、实验、验收和汇总脚本
docs/                 项目文档
reports/              实验和验收结果
```

## 快速运行

本地开发不需要 Docker。先运行单进程 dev runner：

```powershell
go run ./cmd/logserve-dev
```

另开一个 PowerShell 执行 Python task 示例：

```powershell
$env:PYTHONPATH = "$PWD\sdk\python"
python .\examples\hello_task\add.py
```

也可以直接运行 smoke 脚本：

```powershell
powershell.exe -ExecutionPolicy Bypass -File .\scripts\smoke_task.ps1
powershell.exe -ExecutionPolicy Bypass -File .\scripts\smoke_workflow.ps1
powershell.exe -ExecutionPolicy Bypass -File .\scripts\smoke_actor.ps1
powershell.exe -ExecutionPolicy Bypass -File .\scripts\smoke_llm.ps1
```

Docker Compose 会启动 PostgreSQL、NATS JetStream、MinIO、logd、control 和 worker：

```powershell
docker compose -f deployments/docker-compose.yml up --build
```

## 常用验证

基础测试：

```powershell
go test ./...
```

Ubuntu 上的完整项目验收：

```bash
bash scripts/ubuntu_project_acceptance.sh
```

重点子验收：

```bash
bash scripts/run_experiment.sh
bash scripts/ubuntu_checkpoint_acceptance.sh
bash scripts/ubuntu_postgres_async_acceptance.sh
```

实验脚本会把结果写到 `reports/`。当前推荐阅读的结果是 `reports/ubuntu-project-accepted/acceptance_summary.md`。默认输出目录使用 `latest`，如果要保留多份结果，请使用 `run-01`、`accepted`、`failed-01` 这类无日期名字。

## 文档导航

| 想了解什么 | 文档 |
|---|---|
| 文档总入口 | `docs/README.md` |
| 项目技术架构 | `docs/architecture.md` |
| 已落地优化和后续优化路线 | `docs/optimizations.md` |
| 最新实验和验收结论 | `docs/report.md` |
| 简历和面试表述 | `docs/resume.md` |
| Ubuntu 顶层验收流程 | `docs/ubuntu-project-acceptance.md` |
| metadata checkpoint 验收 | `docs/ubuntu-checkpoint-acceptance.md` |
| PostgreSQL async materializer 验收 | `docs/ubuntu-postgres-async-test.md` |
| 原始深度优化备忘录 | `docs/plan.md` |
| benchmark 产物说明 | `benchmarks/README.md` |
