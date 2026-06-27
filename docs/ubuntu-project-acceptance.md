# Ubuntu 顶层项目验收

这份文档说明如何在一台 Ubuntu 服务器上跑 LogServe 的顶层验收。它会同时验证基础测试、Docker Compose 端到端实验、metadata checkpoint、PostgreSQL async materializer 和 physical compaction。

这个验收适合项目交付前使用。通过后可以说明单机多进程机制健康；不能把它解释成多机生产性能结论。

## 环境准备

安装基础工具：

```bash
sudo apt-get update
sudo apt-get install -y git curl ca-certificates build-essential python3 python3-venv tar docker.io docker-compose-plugin
sudo usermod -aG docker "$USER"
```

把用户加入 `docker` 组后需要重新登录，或者确保当前 shell 能访问 Docker daemon。

脚本要求这些命令可用：

- `go`
- `python3`
- `bash`
- `git`
- `tar`
- `docker compose` 或 `docker-compose`

## 完整运行

在仓库根目录执行：

```bash
bash scripts/ubuntu_project_acceptance.sh
```

默认会跑：

- 环境检查和服务器信息采集。
- Python virtualenv、SDK dependency install。
- `go test -count=1 ./...`。
- `go vet ./...`。
- physical compaction 专项测试。
- `internal/logstore` race tests。
- `internal/control`、`internal/metadata`、`internal/worker` race tests。
- Python script tests、SDK tests 和 compileall。
- Docker Compose 端到端实验。
- metadata checkpoint acceptance。
- PostgreSQL async materializer acceptance。
- 自动生成汇总和打包结果。

## 快速 smoke run

只检查服务器环境和基础路径时，可以跳过较重的子套件：

```bash
LOGSERVE_PROJECT_RUN_COMPOSE=0 \
LOGSERVE_PROJECT_RUN_CHECKPOINT=0 \
LOGSERVE_PROJECT_RUN_POSTGRES_ASYNC=0 \
bash scripts/ubuntu_project_acceptance.sh
```

如果服务器资源较小，可以只跳过 PostgreSQL async 对比：

```bash
LOGSERVE_PROJECT_RUN_POSTGRES_ASYNC=0 bash scripts/ubuntu_project_acceptance.sh
```

保留 Compose 实验但跳过 benchmark runtime：

```bash
LOGSERVE_RUN_BENCHMARK=0 bash scripts/ubuntu_project_acceptance.sh
```

## 输出文件

脚本会生成：

```text
reports/ubuntu-project-latest/
```

重要文件：

- `acceptance_summary.md`：人读的顶层结果。
- `acceptance_summary.json`：机器可读结果。
- `command_status.jsonl`：每条命令的退出码和耗时。
- `server_environment.txt`：Ubuntu、Go、Python、Docker 和 git snapshot。
- `compose_experiment/summary.md`：Compose 端到端实验。
- `checkpoint_acceptance/acceptance_summary.md`：checkpoint 子验收。
- `postgres_async_acceptance/acceptance_summary.md`：PostgreSQL async 子验收。
- `ubuntu-project-acceptance-package.tar.gz`：打包后的日志和汇总。

如果验收失败，优先看 `acceptance_summary.md` 中失败命令对应的 log。

## 预期通过项

完整运行时，顶层 verdict 应为 `PASS`，这些 checks 应为 `pass`：

- `go_baseline_tests`
- `physical_compaction_tests`
- `logstore_race_tests`
- `python_script_tests`
- `python_compileall`
- `compose_experiment_pass`
- `checkpoint_acceptance_pass`
- `postgres_async_acceptance_pass`

## 已通过结果（2026-06-24）

最新通过的完整验收：

```text
Result directory: /home/lab2439/Work/lzww/LogServe/reports/ubuntu-project-accepted
Verdict: PASS
Package: /home/lab2439/Work/lzww/LogServe/reports/ubuntu-project-accepted/ubuntu-project-acceptance-package.tar.gz
```

顶层命令：

| Command | Status | Seconds |
|---|---:|---:|
| `go_test_all` | PASS | 27 |
| `go_vet` | PASS | 0 |
| `go_test_physical_compaction` | PASS | 1 |
| `go_race_logstore` | PASS | 1 |
| `go_race_core` | PASS | 3 |
| `python_script_tests` | PASS | 0 |
| `python_sdk_tests` | PASS | 0 |
| `python_compileall` | PASS | 0 |
| `compose_experiment` | PASS | 80 |
| `checkpoint_acceptance` | PASS | 2 |
| `postgres_async_acceptance` | PASS | 116 |

关键证据：

- Compose 实验通过，dashboard 观测到 3 个 worker，dashboard replay consistency 为 true。
- Actor snapshot replay 从 21 条 command 降到 1 条 command。
- actor log 报告 45 条 compactable records 和 18,382 compactable bytes。
- metadata checkpoint 子验收中，checkpoint-plus-tail replay 读取 71 条 records，full replay 读取 614 条 records，156 个对象一致性检查通过。
- PostgreSQL async materializer 将 transaction rate 从 72.382 tx/s 降到 1.304 tx/s，将 row writes rate 从 100.519 rows/s 降到 16.570 rows/s，同时 task throughput 和 p99 在验收阈值内没有退化。
- physical compaction 专项测试和 logstore race tests 在同一轮验收中通过。

这轮结果可以作为当前实现的单机项目验收门禁。它验证机制正确性和非退化，不验证多节点生产性能。