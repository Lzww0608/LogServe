# Ubuntu PostgreSQL Async Materializer 验收

这份文档说明如何验证 PostgreSQL async materializer。LogServe 的 source of truth 是 shared log，PostgreSQL 只是 metadata 的 materialized view。async 模式的目标是降低同步 SQL 写入压力，同时不让 task throughput 和 p99 退化。

## 环境准备

```bash
sudo apt-get update
sudo apt-get install -y git curl ca-certificates build-essential python3 python3-venv
```

还需要：

- `go`
- `python3`
- `docker`
- `docker compose` 或 `docker-compose`

确认当前用户能访问 Docker：

```bash
docker info
docker compose version || docker-compose --version
```

## 运行

在仓库根目录执行：

```bash
bash scripts/ubuntu_postgres_async_acceptance.sh
```

脚本会跑：

- 环境检查。
- `go test -count=1 ./...`。
- `go test -race -count=1 ./internal/metadata ./internal/control`。
- Python SDK unittest 和 compileall。
- Docker Compose sync PostgreSQL metadata run。
- Docker Compose async PostgreSQL metadata run。
- 自动比较和打包结果。

快速 smoke run：

```bash
LOGSERVE_SERVER_SKIP_BASELINE=1 \
LOGSERVE_COMPARE_BENCH_TASKS=16 \
LOGSERVE_COMPARE_BENCH_WORKFLOWS=2 \
LOGSERVE_COMPARE_BENCH_LLM_REQUESTS=4 \
LOGSERVE_COMPARE_BENCH_ACTOR_COMMANDS=10 \
bash scripts/ubuntu_postgres_async_acceptance.sh
```

更强的最终运行：

```bash
LOGSERVE_COMPARE_BENCH_TASKS=128 \
LOGSERVE_COMPARE_BENCH_WORKFLOWS=10 \
LOGSERVE_COMPARE_BENCH_LLM_REQUESTS=20 \
LOGSERVE_COMPARE_BENCH_ACTOR_COMMANDS=100 \
bash scripts/ubuntu_postgres_async_acceptance.sh
```

## 输出文件

脚本会生成：

```text
reports/ubuntu-postgres-async-latest/
```

重要文件：

- `acceptance_summary.md`
- `acceptance_summary.json`
- `postgres_async_compare/summary.md`
- `postgres_async_compare/comparison.json`
- `ubuntu-acceptance-package.tar.gz`

失败时优先看 `acceptance_summary.md` 中标出的失败 log。

## 验收标准

这些 checks 应该通过：

- async task throughput within tolerance，默认阈值 `LOGSERVE_COMPARE_TASK_THROUGHPUT_MIN_RATIO=0.99`。
- async task submit p99 within tolerance，默认阈值 `LOGSERVE_COMPARE_TASK_P99_MAX_RATIO=1.0`。
- async PostgreSQL transaction rate lower than sync。
- async PostgreSQL row-write rate lower than sync。
- async materializer mode observed in dashboard。
- async materializer flush errors equal to zero。

通过不等于吞吐一定提升。只有 summary 里的 strict-improvement 指标为 true，才可以说对应指标严格改善。

## 已通过结果

最新嵌套在顶层项目验收里的结果：

```text
reports/ubuntu-project-accepted/postgres_async_acceptance/postgres_async_compare
Acceptance: PASS
```

| Metric | Sync | Async | Async/Sync |
|---|---:|---:|---:|
| Task throughput tps | 5.0 | 5.0 | 1.0 |
| Task submit p99 ms | 209 | 207 | 0.9904 |
| PostgreSQL tx/s | 72.382 | 1.304 | 0.018 |
| PostgreSQL row writes/s | 100.519 | 16.57 | 0.1648 |

Materializer 状态：

| Item | Sync | Async |
|---|---:|---:|
| mode | sync | async |
| pending deltas | None | 6 |
| flush errors | 0 | 0 |
| eventual lag ms | None | 746 |

通过的 checks：

- `task_throughput_within_tolerance`: pass
- `task_submit_p99_within_tolerance`: pass
- `postgres_transactions_per_sec_reduced`: pass
- `postgres_row_writes_per_sec_reduced`: pass
- `async_materializer_mode_observed`: pass
- `async_materializer_flush_errors_zero`: pass

结论：async materializer 把 PostgreSQL transaction/write rate 降了很多，task throughput 和 p99 没有退化。不要把这轮结果写成“吞吐显著提升”，因为 `task_throughput_strictly_improved=false`。