# Ubuntu Console 功能 6-10 验收

这份说明用于在一台 Ubuntu 服务器上验证新增 Console 功能 6-10。脚本会用 Docker Compose 在单机上启动 `postgres`、`nats`、`minio`、`logd`、`control`、`web` 和 `worker`，然后通过真实 Console HTTP API 跑端到端探针。

它验证的是单机多进程机制链路，不代表多机生产性能，也不代表真实 GPU/vLLM 性能。

## 覆盖范围

| 功能 | 验收点 |
|---|---|
| 功能 6：workflow DAG | 提交两步 workflow，确认第二步保留 `depends_on=["first"]`，并通过 workflow replay 一致性检查。 |
| 功能 7：LLM Playground | 注册 mock model，提交 LLM 请求，读取 LLM replay trace，并检查 `model_name`、`model_version`、`LLMCompleted` 和 latency 字段。 |
| 功能 8：worker cache matrix | 读取 `/api/workers`，确认执行 LLM 的 worker 上报了对应模型缓存。 |
| 功能 9：actor 控制台 | 创建 Counter actor，调用 actor method，读取 actor status。 |
| 功能 10：log stream explorer | 读取 `system:functions`、`wf:<workflow_id>` 和 `actor:<actor_id>` stream，检查 stream id、stats、record stream id 和关键事件类型。 |

## 环境准备

服务器需要有这些命令：

```bash
git --version
go version
python3 --version
node --version
npm --version
docker --version
docker compose version
```

如果 Docker 需要 sudo，先把当前用户加入 docker 组并重新登录：

```bash
sudo usermod -aG docker "$USER"
```

## 推荐运行方式

进入仓库根目录后运行：

```bash
git pull
chmod +x scripts/ubuntu_console_features_6_10_acceptance.sh scripts/ubuntu_console_acceptance.sh
LOGSERVE_DOCKER_GOPROXY=https://goproxy.cn,direct \
bash scripts/ubuntu_console_features_6_10_acceptance.sh
```

默认结果目录是：

```text
reports/ubuntu-console-features-6-10-latest
```

需要保留多次结果时，不要使用日期或时间戳，使用无日期名字：

```bash
LOGSERVE_CONSOLE_FEATURES_ACCEPTANCE_ID=run-01 \
LOGSERVE_DOCKER_GOPROXY=https://goproxy.cn,direct \
bash scripts/ubuntu_console_features_6_10_acceptance.sh
```

## 轻量预检

如果你只想先确认 Go/Web/Python 脚本测试能通过，不启动 Docker：

```bash
LOGSERVE_CONSOLE_RUN_DOCKER=0 bash scripts/ubuntu_console_features_6_10_acceptance.sh
```

这个模式会生成汇总，但 `features_6_10.verdict` 应为 `INCOMPLETE`，不能作为功能 6-10 通过结论。

## 自动汇总结果

脚本结束后重点看：

```text
reports/ubuntu-console-features-6-10-latest/acceptance_summary.md
reports/ubuntu-console-features-6-10-latest/acceptance_summary.json
reports/ubuntu-console-features-6-10-latest/console_http_probe.json
reports/ubuntu-console-features-6-10-latest/console-acceptance-package.tar.gz
```

`acceptance_summary.json` 里这些字段最关键：

```text
verdict = PASS
features_6_10.verdict = PASS
features_6_10.features.*.state = PASS
```

本地也可以再跑一次判定器：

```bash
python3 scripts/evaluate_console_acceptance.py \
  reports/ubuntu-console-features-6-10-latest/console-acceptance-package.tar.gz \
  --json-out reports/ubuntu-console-features-6-10-latest/evaluation.json \
  --md-out reports/ubuntu-console-features-6-10-latest/evaluation.md
```

## 发回给我判断

测试完成后，把下面任意一种发给我即可：

1. `reports/ubuntu-console-features-6-10-latest/console-acceptance-package.tar.gz`
2. 或者 `acceptance_summary.md`、`acceptance_summary.json`、`console_http_probe.json` 三个文件内容

我会先看 `features_6_10` 分组 verdict，再结合失败命令、probe 细节和 Compose 日志判断是否符合预期，以及失败点属于服务器环境、Docker 构建、服务启动，还是功能代码问题。