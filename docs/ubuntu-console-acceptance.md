# Ubuntu Console 验收

这份说明只覆盖新增的 Console 相关代码：`logserve-web` 后端、React/Vite 前端、Docker Compose 中的 `web` 服务，以及 Console HTTP API 到控制面的连通性。全项目验收仍然使用 `scripts/ubuntu_project_acceptance.sh`。

## 验收内容

`scripts/ubuntu_console_acceptance.sh` 会自动执行这些检查：

1. `go test -count=1 ./cmd/logserve-web ./internal/webapi`
2. `go vet ./cmd/logserve-web ./internal/webapi`
3. `cd web && npm ci && npm run build`
4. `python -m unittest discover tests/scripts`
5. Docker Compose 配置校验、镜像构建、启动 `postgres/nats/minio/logd/control/web/worker`
6. 探测 Console HTTP 表面：
   - `/api/healthz` 不带 token 可访问
   - `/api/dashboard` 不带 token 返回 401
   - `/api/dashboard` 带 bearer token 返回 dashboard JSON
   - `/` 和深链接返回前端页面
   - 通过 `/api/tasks?wait=true` 提交一个真实 Python task，并确认结果为 `3`
   - task detail、dashboard task 列表和 admin config 可读
   - 通过 `/api/workflows?wait=true` 提交两步 workflow，并确认 detail 里保留 `depends_on` DAG 依赖
   - 调用 workflow replay，并确认 `consistent_with_metadata=true`
   - 通过 `/api/models` 注册 mock model，通过 `/api/llm?wait=true` 提交 LLM 请求，并读取 LLM replay trace latency
   - 通过 `/api/workers` 确认 worker cache matrix 能看到已缓存模型
   - 通过 `/api/actors` 创建 Counter actor，调用 actor method，并读取 actor status
   - 通过 `/api/logs/streams` 和 `/api/logs/streams/{stream_id}` 查看 `system:functions`、`wf:<workflow_id>`、`actor:<actor_id>` stream 记录

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

如果 Docker 需要 sudo，请先把当前用户加入 docker 组并重新登录，或者用有 Docker 权限的用户运行脚本。

## 推荐运行方式

在 Ubuntu 服务器上进入仓库根目录后运行：

```bash
git pull
chmod +x scripts/ubuntu_console_acceptance.sh
LOGSERVE_DOCKER_GOPROXY=https://goproxy.cn,direct \
bash scripts/ubuntu_console_acceptance.sh
```

默认结果目录是：

```text
reports/ubuntu-console-latest
```

不要把 `LOGSERVE_CONSOLE_ACCEPTANCE_ID` 设置成日期或时间戳。需要保留多次结果时，用 `run-01`、`run-02`、`failed-01` 这类名字：

```bash
LOGSERVE_CONSOLE_ACCEPTANCE_ID=run-01 bash scripts/ubuntu_console_acceptance.sh
```

## 轻量检查

如果你暂时只想确认代码和前端能构建，不启动 Docker：

```bash
LOGSERVE_CONSOLE_RUN_DOCKER=0 bash scripts/ubuntu_console_acceptance.sh
```

这个模式不会证明 Console 能连通真实控制面，只适合作为快速预检。

## 查看页面

脚本默认把 Web 端口绑定到服务器本机的 `127.0.0.1`。如果需要从本地浏览器打开页面，先看结果目录里的 `server_environment.txt`，找到 `base_url` 对应端口，然后做 SSH 端口转发：

```bash
ssh -L 8080:127.0.0.1:<WEB_PORT> <user>@<server>
```

再打开：

```text
http://127.0.0.1:8080
```

如果想让脚本结束后保留 Compose 栈用于手工查看：

```bash
LOGSERVE_CONSOLE_KEEP_STACK=1 bash scripts/ubuntu_console_acceptance.sh
```

查看完后清理：

```bash
docker compose --env-file reports/ubuntu-console-latest/console.env \
  -p logserve-console-latest \
  -f deployments/docker-compose.yml down --remove-orphans --volumes
```

## 结果文件

脚本结束后重点看：

```text
reports/ubuntu-console-latest/acceptance_summary.md
reports/ubuntu-console-latest/acceptance_summary.json
reports/ubuntu-console-latest/command_status.jsonl
reports/ubuntu-console-latest/console_http_probe.json
reports/ubuntu-console-latest/console-acceptance-package.tar.gz
```

`acceptance_summary.json` 的 `verdict` 为 `PASS` 才表示 console 验收通过。现在的 PASS 会同时覆盖 Console 1-10：基础 dashboard/task 链路、workflow DAG、LLM playground、worker cache matrix、actor create/call/status，以及 log stream explorer。失败时先不要删目录，把 `console-acceptance-package.tar.gz` 和 `acceptance_summary.md/json` 发回来，我会根据里面的命令状态、探针结果和日志判断是否符合预期，以及失败点是环境问题还是代码问题。

## 本地判定

如果想在服务器上先按同一套规则自查：

```bash
python3 scripts/evaluate_console_acceptance.py reports/ubuntu-console-latest/console-acceptance-package.tar.gz
```

判定结果有三种：

- `PASS`：完整 Docker console 验收符合预期。
- `FAIL`：验收失败，输出里会列出失败命令或失败检查。
- `INCOMPLETE`：只跑了轻量模式，没有覆盖 Docker runtime 和真实 Console API 链路。

我收到结果包后，也会用这个脚本作为第一层判定，再结合日志看是否需要修代码或调整服务器环境。