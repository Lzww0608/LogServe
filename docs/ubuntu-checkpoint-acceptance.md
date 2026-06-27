# Ubuntu Metadata Checkpoint 验收

这份文档说明如何验证 control-plane metadata checkpoint。checkpoint 的作用是减少重启时需要读取的历史日志：先加载 `system:checkpoints` 中的状态，再从各 stream 的 `last_seq+1` 读取 tail。

这个验收用单机 in-memory shared-log harness 构造 task、workflow、actor 和 LLM metadata history。Docker 不是必需项。

## 环境准备

```bash
sudo apt-get update
sudo apt-get install -y git curl ca-certificates build-essential python3 tar
```

脚本要求：

- `go`
- `python3`
- `bash`
- `tar`

Docker 只有在设置 `LOGSERVE_REQUIRE_DOCKER=1` 时才强制需要。

## 运行

在仓库根目录执行：

```bash
bash scripts/ubuntu_checkpoint_acceptance.sh
```

默认会跑：

- 环境检查。
- `go test -count=1 ./...`。
- `internal/control` checkpoint-focused race tests。
- Python script tests 和 compileall。
- checkpoint acceptance workload。
- 自动生成 summary 和打包结果。

快速 smoke run：

```bash
LOGSERVE_CHECKPOINT_SKIP_BASELINE=1 \
LOGSERVE_CHECKPOINT_ACCEPTANCE_TASKS=40 \
LOGSERVE_CHECKPOINT_ACCEPTANCE_WORKFLOWS=4 \
LOGSERVE_CHECKPOINT_ACCEPTANCE_ACTORS=4 \
LOGSERVE_CHECKPOINT_ACCEPTANCE_LLM_STREAMS=12 \
bash scripts/ubuntu_checkpoint_acceptance.sh
```

更强的最终运行：

```bash
LOGSERVE_CHECKPOINT_ACCEPTANCE_TASKS=1000 \
LOGSERVE_CHECKPOINT_ACCEPTANCE_WORKFLOWS=80 \
LOGSERVE_CHECKPOINT_ACCEPTANCE_ACTORS=80 \
LOGSERVE_CHECKPOINT_ACCEPTANCE_LLM_STREAMS=300 \
bash scripts/ubuntu_checkpoint_acceptance.sh
```

也可以只跑子套件：

```bash
bash scripts/checkpoint_acceptance.sh
```

## 输出文件

脚本会生成：

```text
reports/ubuntu-checkpoint-latest/
```

重要文件：

- `acceptance_summary.md`
- `acceptance_summary.json`
- `checkpoint_acceptance/summary.md`
- `checkpoint_acceptance/summary.json`
- `checkpoint_acceptance/checkpoint_acceptance.json`
- `ubuntu-checkpoint-acceptance-package.tar.gz`

失败时优先看 `acceptance_summary.md` 中标出的失败 log。

## 验收标准

这些 checks 应该通过：

- `checkpoint_created`
- `checkpoint_replay_consistent`
- `checkpoint_read_records_reduced`
- `checkpoint_tail_only_reads`
- `corrupt_checkpoint_fallback`
- `checkpoint_retention`

最关键的数字是 `checkpoint_records_over_full`。它应小于 `1.0`，表示 checkpoint replay 读取的历史 records 少于 full replay。duration 可以作为辅助观察，但单机小样本容易抖动，不应只看耗时。

## 已通过结果

最新嵌套在顶层项目验收里的结果：

```text
Run directory: reports/ubuntu-project-accepted/checkpoint_acceptance/checkpoint_acceptance
Verdict: PASS
Generated at UTC: 2026-06-24T02:59:07Z
```

Workload：

| Item | Count |
|---|---:|
| Tasks | 120 |
| Workflows | 12 |
| Actors | 12 |
| LLM streams | 40 |
| Tail events | 68 |

Replay work：

| Metric | Full replay | Checkpoint replay | Checkpoint/Full |
|---|---:|---:|---:|
| Records read | 614 | 71 | 0.1156 |
| ReadLog calls | 224 | 201 | 0.8973 |
| Duration ms | 6.463 | 5.506 | 0.8519 |

Checkpoint 内容：

| Item | Count |
|---|---:|
| Streams | 196 |
| Tasks | 132 |
| Workflows | 12 |
| Actors | 12 |
| LLM stats entries | 2 |
| Consistency checked objects | 156 |

通过的检查：

- `checkpoint_created`: pass
- `checkpoint_read_records_reduced`: pass
- `checkpoint_replay_consistent`: pass
- `checkpoint_retention`: pass
- `checkpoint_tail_only_reads`: pass
- `corrupt_checkpoint_fallback`: pass

结论：checkpoint-plus-tail replay 保持 metadata 一致，同时把读取 records 从 614 降到 71。`ReadLog` calls 只从 224 降到 201，因为重启路径仍要检查各 stream tail。写报告时应强调“减少历史 records 读取”，不要写成“消除所有 stream 访问”。