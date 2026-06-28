# Ubuntu Console 前端/Admin/Functions 验收

这份说明用于在一台 Ubuntu 服务器上验证这次新增的 Console 前端改动，重点覆盖 Admin backpressure 配置、Function Registry 页面/API、前端深链和自动汇总结果。脚本会用 Docker Compose 在单机上启动 `postgres`、`nats`、`minio`、`logd`、`control`、`web` 和 `worker`，再通过真实 Console HTTP API 做端到端探针。

这不是多机生产性能测试，只验证单机多进程机制链路和新增 Console 功能是否可用。

## 覆盖范围

`scripts/ubuntu_console_frontend_acceptance.sh` 会复用完整 Console 验收，并额外在报告中输出 `frontend_admin_functions` 分组。该分组要求下面检查全部通过：

- `/admin`、`/functions`、`/submit/task?function_hash=...` 深链返回前端应用。
- `/api/functions` 无 token 返回 401，带 token 可以看到已注册函数。
- `/api/functions/{function_hash}` 可以读取函数详情。
- `/api/admin/config` 无 token 返回 401，带 token 返回 scheduling policy、backpressure、metadata materializer stats、compactable log records/bytes。
- `/api/admin/backpressure` 无 token 返回 401。
- 非法 backpressure 值返回 400。
- 合法 backpressure 值写入后，重新读取 `/api/admin/config` 立即反映新值。

## Ubuntu 准备

服务器需要这些命令可用：

```bash
git --version
go version
python3 --version
node --version
npm --version
docker --version
docker compose version
```

如果 Docker 需要 sudo，建议先把当前用户加入 docker 组并重新登录：

```bash
sudo usermod -aG docker "$USER"
```

## 推荐运行方式

进入仓库根目录后执行：

```bash
git pull
chmod +x scripts/ubuntu_console_frontend_acceptance.sh scripts/ubuntu_console_acceptance.sh
LOGSERVE_DOCKER_GOPROXY=https://goproxy.cn,direct \
bash scripts/ubuntu_console_frontend_acceptance.sh
```

默认结果目录是：

```text
reports/ubuntu-console-frontend-latest
```

需要保留多次结果时，不要使用日期或时间戳，用无日期名称：

```bash
LOGSERVE_CONSOLE_FRONTEND_ACCEPTANCE_ID=run-01 \
LOGSERVE_DOCKER_GOPROXY=https://goproxy.cn,direct \
bash scripts/ubuntu_console_frontend_acceptance.sh
```

## 轻量预检

如果你只想先检查 Go/Web/Python 测试和前端构建，不启动 Docker：

```bash
LOGSERVE_CONSOLE_RUN_DOCKER=0 bash scripts/ubuntu_console_frontend_acceptance.sh
```

这个模式会生成汇总，但 `frontend_admin_functions.verdict` 应为 `INCOMPLETE`，不能作为新增前端功能通过结论。

## 自动汇总结果

脚本结束后重点查看：

```text
reports/ubuntu-console-frontend-latest/acceptance_summary.md
reports/ubuntu-console-frontend-latest/acceptance_summary.json
reports/ubuntu-console-frontend-latest/console_http_probe.json
reports/ubuntu-console-frontend-latest/console-acceptance-package.tar.gz
```

通过标准：

```text
verdict = PASS
frontend_admin_functions.verdict = PASS
features_6_10.verdict = PASS
```

也可以在服务器上再跑一次本地判定器：

```bash
python3 scripts/evaluate_console_acceptance.py \
  reports/ubuntu-console-frontend-latest/console-acceptance-package.tar.gz \
  --json-out reports/ubuntu-console-frontend-latest/evaluation.json \
  --md-out reports/ubuntu-console-frontend-latest/evaluation.md
```

## 发回给我判断

测试完成后，把下面任意一种结果发给我：

1. `reports/ubuntu-console-frontend-latest/console-acceptance-package.tar.gz`
2. 或者 `acceptance_summary.md`、`acceptance_summary.json`、`console_http_probe.json` 三个文件内容

我会先看 `verdict` 和 `frontend_admin_functions.verdict`，再结合失败命令、probe 细节和 Compose 日志判断是否符合预期，以及失败点属于服务器环境、Docker 构建、服务启动，还是功能代码问题。