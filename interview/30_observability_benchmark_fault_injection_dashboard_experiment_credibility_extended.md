# 十、Observability、Benchmark、Fault Injection、Dashboard 与实验可信度（拓展）

这一组问题已经接近生产化追问。回答时要把话说实：LogServe 当前有实验脚本、结构化日志、dashboard snapshot 和几类故障测试，但还没到完整线上观测平台。好的回答会把已有能力和缺口都讲清楚，说明下一步该补什么、为什么这些补充能让实验更可信。

## Q736. 系统 benchmark 和 microbenchmark 的区别是什么？

microbenchmark 测的是一个很小的部件。比如 LogStore benchmark 只测 append、read、recovery；一个 Python runner benchmark 只测函数执行；一个 checkpoint copy benchmark 只测本地文件复制。这类测试好处是干净，变量少，能快速定位某个模块的性能边界。

系统 benchmark 测的是完整链路。LogServe 的 runtime benchmark 会经过 SDK、control、shared log、metadata view、worker poll、executor、completion、dashboard/replay 等路径。它更接近用户真正感受到的延迟。

两者都需要。microbenchmark 告诉我“哪个零件慢”，系统 benchmark 告诉我“拼起来以后用户看到什么”。比如 LogStore append 很快，不代表 workflow 快；workflow 慢，也不一定是 logstore 慢，可能卡在 worker poll、Python runner、LLM mock、result store 或调度等待。

我会这样回答：microbenchmark 用来找局部瓶颈，系统 benchmark 用来验证端到端语义和用户感知。LogServe 现在两类都有，但系统 benchmark 的样本量还偏小，更像机制验证和回归基线。

## Q737. 如何设计端到端 benchmark 避免只测 mock？

要让端到端 benchmark 更接近真实系统，不能只跑 mock 函数。至少要把真实路径里的几个成本放进来。

第一，任务要有不同类型。普通 CPU task、I/O task、actor command、workflow fan-out、LLM request 都要覆盖。只跑一个 `ping()`，测到的大多是框架固定开销。

第二，结果大小要有分布。小结果 inline，大结果走 result store。这样才能看到 result_ref、object store latency 和 replay 读取的影响。

第三，LLM 要分层。没有 GPU 时可以继续保留 mock LLM，但要补 file-backed checkpoint、不同模型大小、不同 prompt length、不同 max_tokens。接入 GPU 后，再加入真实 vLLM adapter，记录 prefill、decode、first token、total tokens。

第四，请求要并发。当前默认 benchmark 串行提交，适合 smoke test。端到端压测应该支持并发客户端、固定 QPS、突发流量和长尾任务混合。

第五，要有失败和恢复。只测成功路径会高估系统表现。应在 benchmark 中插入 worker restart、control restart、queue redelivery、object store timeout 这类场景。

对 LogServe 来说，下一步可以保留现有 `examples/evaluation/benchmark.py` 作为 smoke benchmark，再加一个 `workload` 配置文件，把任务比例、并发度、模型分布、结果大小、故障注入点都参数化。

## Q738. 如何做 A/B 实验比较 scheduler policy？

调度策略的 A/B 实验要保证 workload 一致。

比较 `RESOURCE_ONLY`、`LOCALITY_AWARE`、`PREDICTED_LATENCY` 时，不能让每组请求不一样。最好先生成一份固定请求序列，比如模型名、prompt 长度、max_tokens、提交间隔、tenant、优先级都写进 workload 文件。每个 policy 都跑同一份 workload。

实验前还要固定初始状态。比如三台 worker 中，worker-1 缓存 `model-A`，worker-2 缓存 `model-B`，worker-3 不缓存目标模型。每轮实验前清理 runtime 目录，重新准备 checkpoint 和 cache，避免上一轮污染下一轮。

要记录的指标包括：

- cache hit rate
- cold start count
- p50/p95/p99 latency
- queue wait
- model load time
- checkpoint fetch time
- SLO violation rate
- 每个 worker 的请求分布

如果只看一个平均延迟，很容易误判。比如某个策略平均延迟低，但 p99 很差，线上体验仍然会出问题。

LogServe 当前已经有 locality ablation 的雏形。要把它变成更可信的 A/B 实验，需要扩大请求数、多轮重复、输出每轮原始记录，并把调度决策原因保存下来。

## Q739. 如何用 flame graph 找出 control plane 瓶颈？

Flame graph 适合看 CPU 时间花在哪里。对 control plane 来说，它可以回答：CPU 是耗在 gRPC、metadata lock、workflow scheduling、JSON marshal、log append client，还是 dashboard snapshot 构造上。

做法大致是：

1. 给 `logserve-control` 开启 `net/http/pprof` 端点，最好只绑定 localhost 或管理端口。
2. 跑一段稳定 workload，比如并发提交 task、workflow、actor call 和 LLM request。
3. 用 `go tool pprof` 抓 CPU profile。
4. 生成 flame graph 或在浏览器里看 top、tree、web。

如果 flame graph 上 `encoding/json` 占比很高，说明 payload 编解码可能是瓶颈。如果锁相关函数或 runtime contention 占比高，要看 metadata store 或 queue lock。如果大量时间在 log client RPC 上，control 可能被 logd append latency 拖住。

Flame graph 只能解释 CPU 消耗。它看不到 I/O 等待的全部细节。control plane 如果慢在 logd fsync、worker 等待或数据库提交，CPU flame graph 可能很干净，这时要结合 tracing 和 latency breakdown。

## Q740. 如何用 pprof 分析 Go mutex contention？

Go 的 mutex profile 可以告诉我们哪些锁导致 goroutine 等待。

在 control plane 里，比较值得关注的锁包括 queue lock、workflow lock、actor lock、config lock、metadata store 内部锁。worker 侧则有 executor pool queue、actor per-lock、checkpoint cache lock。

接入时要做两步。第一，在程序里开启 mutex profiling，比如设置 `runtime.SetMutexProfileFraction(1)` 或一个较低采样比例。第二，通过 pprof 端点抓取 mutex profile。

分析时重点看：

- 哪些函数持锁时间长。
- 哪些锁等待次数多。
- 等待发生在 submit、poll、complete 还是 dashboard snapshot。
- 锁等待是否随着 worker 数和并发请求数增长而恶化。

如果发现 `workflowMu` 或 `queueMu` 很热，要看是否可以缩短持锁区间、把全局锁拆成 per-workflow/per-actor/per-stream 锁，或者把 dashboard 的全量扫描改成分页和缓存。

我不会一上来就把所有锁拆掉。先用 pprof 证明它是瓶颈，再改。否则很容易把实现复杂度拉高，还不一定提升端到端性能。

## Q741. 如何用 tracing 找出 p99 latency 来源？

Tracing 的关键是把一次请求拆成多个 span。

以 workflow 为例，可以把一次 workflow submit 作为根 trace。下面挂这些 span：

- SDK submit
- control append `WorkflowStarted`
- schedule ready step
- append `TaskSubmitted`
- worker poll
- worker local queue wait
- executor run
- result store put
- complete task
- append step completion
- schedule next step
- workflow complete

每个 span 都带 `trace_id`、`workflow_id`、`step_id`、`task_id`、`worker_id`。LLM span 再带 model name、cache hit、checkpoint fetch、model load、first token、total tokens。

查 p99 时，不要只看总时间。要按 span breakdown 看慢在哪里：

- 慢在 control queue，说明调度或 worker capacity 不够。
- 慢在 local queue，说明 worker executor pool 太小或任务太长。
- 慢在 log append，说明 logd 或 fsync policy 是瓶颈。
- 慢在 checkpoint fetch，说明缓存或对象存储是瓶颈。
- 慢在 model load，说明冷启动或显存压力是瓶颈。

LogServe 当前还没有完整 tracing，但已有 task/workflow/actor/LLM id。下一步把这些 id 接进 OpenTelemetry，基本就能从 dashboard 跳到 trace。

## Q742. 如何模拟真实 LLM 请求分布，包括 prompt length 和 output tokens？

不能用固定短 prompt 代表真实 LLM 请求。真实请求的 prompt length 和 output tokens 往往是长尾分布。

我会从三类 workload 开始：

- 短请求：几十到几百 tokens，适合聊天和简单分类。
- 中等请求：几千 tokens，适合 RAG、摘要、代码问答。
- 长请求：上万 tokens，适合长文档分析和复杂上下文。

每类请求再给不同的 `max_tokens`。比如 64、256、1024、2048。真实 serving 中，prompt prefill 和 decode 的瓶颈不同。prompt 长会压 prefill，output 长会压 decode。

实验输入可以用两种方式生成。简单做法是用固定文本片段拼接到指定 token 长度。更真实的做法是收集一批脱敏 prompt 样本，按长度分桶后重放。

LogServe 的 benchmark 应该把这些字段写进 workload：

```json
{
  "model": "model-A",
  "prompt_tokens": 2048,
  "max_tokens": 512,
  "arrival_ms": 120,
  "tenant": "default"
}
```

mock LLM 可以按 prompt_tokens 和 max_tokens 模拟不同延迟。真实 vLLM 实验再记录实际 token usage，用来校准 mock 模型。

## Q743. 如何设计 soak test 检查长时间运行内存泄漏？

soak test 不是跑几分钟。它要让系统在稳定负载下运行很久，比如 6 小时、24 小时，甚至更长。

LogServe 的 soak test 可以这样设计：

1. 启动 logd、control 和多个 worker。
2. 持续提交混合 workload：task、workflow、actor command、LLM request。
3. 固定 QPS，另加少量突发流量。
4. 周期性导出 dashboard snapshot。
5. 周期性抓 pprof heap、goroutine、mutex profile。
6. 记录 RSS、Go heap、goroutine 数、open file 数、log segment 数、object store 对象数。

判断泄漏时不能只看内存上升。很多系统在 warm-up 阶段会正常增长。真正可疑的是：在吞吐稳定、队列稳定、请求完成后，heap、goroutine 或文件句柄仍然持续上升。

LogServe 特别要关注这些地方：

- workflow/actor metadata map 是否无限增长。
- idempotency map 是否有生命周期。
- LLM stats store 是否按模型和 worker 无界增长。
- worker Python runner 是否残留进程。
- checkpoint cache manifest 是否泄漏。
- dashboard snapshot 是否一次性复制过大对象。

soak test 最后应该输出趋势图，而不是只给一个 PASS/FAIL。

## Q744. 如何用 chaos engineering 测试 worker/control/logd 组合故障？

chaos engineering 不能靠随机搞破坏。更稳的做法是先写出假设，再按假设注入故障。

可以先列出假设：

- worker 掉线后，running task 会 redelivery。
- control 重启后，metadata view 能从 log bootstrap。
- logd 短暂不可用时，control 不绕过 log 写 metadata。
- object store 慢时，大结果和 actor snapshot 会失败或重试，不会写出不可恢复状态。

然后按组合注入：

- kill worker
- kill control
- kill logd
- 同时 kill worker 和 control
- logd 慢 append
- worker 与 control 网络断开
- object store 超时
- PostgreSQL 慢查询

每个故障都要有检查项。比如 worker kill 后检查 task 最终状态、lease epoch、是否有 stale completion；control restart 后检查 dashboard state 和 replay state 是否一致；logd 不可用时检查 SubmitTask 是否 fail fast。

LogServe 当前已有几类 fault injection。更完整的 chaos 实验可以用脚本驱动，比如每 30 秒随机杀一个组件，同时持续提交 workload，最后跑 consistency checker。

## Q745. 如何在 CI 中运行轻量 fault injection？

CI 里的 fault injection 要轻，不然每个 PR 都会太慢。

我会把它分成两层。

PR 必跑层：

- Go 单元测试。
- control/worker 的关键 integration tests。
- poll-before-start redelivery。
- stale completion rejection。
- control bootstrap from log。
- actor snapshot replay 小样本。

这些测试不需要跑真实长时间 workload，几十秒内完成比较合适。

Nightly 或手动层：

- 多轮 benchmark。
- race test 更大范围。
- 进程级 kill。
- logd crash consistency。
- object store timeout。
- soak test。

CI 输出要保留 artifact，包括 `command_status.jsonl`、fault report、失败组件日志、dashboard snapshot。否则失败了也不好查。

LogServe 当前的 `scripts/run_experiment.sh` 已经能产出结构化目录。可以把它拆成 `ci-smoke` 和 `ci-nightly` 两个模式：PR 跑轻量，夜间跑重的。

## Q746. 如何为磁盘满、网络抖动、S3 慢、PostgreSQL 慢设计故障注入？

这四类故障要分层处理。

磁盘满：可以在测试环境里把 logstore 放到小容量 loopback 文件系统，或者用 faulting file writer 在第 N 次 write 返回 `ENOSPC`。检查点是 log append 是否返回错误，control 是否停止更新 metadata view，错误是否进入 structured log。

网络抖动：可以用 Linux `tc netem` 给 control-worker、control-logd、worker-objectstore 之间加 delay、jitter、packet loss。单机测试也可以在 client wrapper 里注入延迟和超时。检查 redelivery 和 retry 是否符合预期。

S3 慢：用一个 object store wrapper，让 `Put/Get` sleep 或 timeout。大结果、actor snapshot、checkpoint fetch 都要覆盖。要确认 snapshot object 写失败时不会 append `ActorSnapshotCreated`。

PostgreSQL 慢：如果 metadata store 使用 PostgreSQL，可以在 store 层加 sleep，或用数据库侧锁表/慢查询模拟。由于 LogServe 的 source of truth 是 shared log，metadata 慢应该影响查询和 view 更新，但不能让系统产生 log 里解释不了的状态。

每个故障注入都要有错误分类。磁盘满和 log append 失败是系统错误；S3 超时可能是外部依赖错误；用户代码异常不应该和这些混在一起。

## Q747. 如何区分系统错误、用户代码错误和外部服务错误？

要从错误来源和可重试性来区分。

系统错误来自 LogServe 自己的组件，比如 logd append 失败、metadata update panic、worker executor pool 崩溃、lease validation bug。这类错误通常需要平台告警，用户重试未必能解决。

用户代码错误来自用户 task 或 actor method，比如 Python 语法错误、函数抛异常、返回值不能 JSON 序列化、超时、依赖包缺失。它应该体现在 task failed 或 actor command failed 里，错误信息要给用户看，但不要把整段内部栈和敏感环境暴露出去。

外部服务错误来自 MinIO/S3、PostgreSQL、vLLM、网络、第三方 API。它们可能是临时的，也可能是配置错误。处理时要带依赖名称、错误码、超时信息和 retry 次数。

代码层面可以定义错误类别：

```text
SYSTEM_ERROR
USER_CODE_ERROR
DEPENDENCY_ERROR
TIMEOUT
CANCELLED
IDEMPOTENCY_CONFLICT
```

日志、metrics、dashboard 和 API response 都应该带这个分类。这样告警不会被用户代码异常淹没，用户也不会看到一堆平台内部错误却不知道该改哪里。

## Q748. dashboard 是否应该展示 error budget？

应该展示，但要先有明确 SLO。

error budget 的意思是：在一段时间内，系统允许多少比例请求违反 SLO。比如 LLM request 的 SLO 是 95% 请求在 250ms 内完成，那么超出 250ms 的请求会消耗 error budget。

Dashboard 可以展示：

- 当前窗口的 SLO violation rate。
- error budget remaining。
- 最近 1 小时、24 小时、7 天的 burn rate。
- 哪类请求消耗最多：workflow、actor、LLM、log append。
- 哪个 worker、模型或 tenant 贡献最多 violation。

LogServe 当前 benchmark 已经有 `slo_violation_rate` 的雏形，但它还只是实验报告字段。要进入 dashboard，需要把 latency metrics 按时间窗口聚合。

面试里我会强调：error budget 不是为了让 dashboard 好看，它能指导工程取舍。比如 locality-aware 提高 cache hit rate 后，error budget burn 降低，这比单看平均延迟更有说服力。

## Q749. 如何把 Prometheus metrics 加入系统？

做法是给 logd、control、worker 各自暴露 `/metrics`。

control 侧可以导出：

- queue depth
- task submit count
- task completion count
- redelivery count
- log append latency
- workflow latency
- actor mailbox backlog
- scheduler decision count

worker 侧可以导出：

- running tasks
- local queue depth
- local queue wait
- executor run time
- Python runner restart count
- model cache hit/miss
- checkpoint fetch latency

logd 侧可以导出：

- append latency
- append errors
- read latency
- segment count
- compactable bytes
- fsync duration
- recovery duration

实现上可以使用 Go Prometheus client，注册 counter、gauge、histogram。gRPC handler 和关键 runtime 路径打点，HTTP server 暴露 metrics endpoint。

要控制 label 数量。`task_id`、`workflow_id` 这种高基数字段不能直接做 Prometheus label。可以把它们放在日志和 tracing 里。metrics label 只放低基数字段，比如 status、event_type、worker_id、model_name、policy。

## Q750. 核心 metrics 应该包括哪些：append latency、queue depth、lease expired、cache hit、actor backlog？

这些都应该包括，而且要按组件分。

logd：

- append latency histogram
- append error count
- read latency histogram
- fsync latency histogram
- segment count
- compactable bytes
- recovered records

control：

- submit request count
- queue depth
- queue wait histogram
- lease expired count
- redelivery count
- stale completion rejection count
- metadata update error count
- bootstrap duration

worker：

- running tasks
- local queue depth
- local queue wait histogram
- executor runtime histogram
- task timeout count
- Python runner restart count

workflow：

- workflow end-to-end latency
- step latency
- retry count
- failed workflow count

actor：

- mailbox backlog
- command apply latency
- owner transfer count
- stale epoch rejection count
- snapshot creation latency
- replay commands count

LLM：

- cache hit rate
- cold start count
- checkpoint fetch latency
- model load latency
- first token latency
- total latency
- eviction count

如果只能先做一批，我会先做 append latency、queue depth、redelivery/stale completion、actor backlog、LLM cache hit 和 p95 latency。这些最能解释系统是否健康。

## Q751. 如何设置 alert？哪些告警最重要？

告警要少而准。太多告警会把人淹没。

我会先设这些：

- log append error rate > 0：这是 source of truth 写入失败，优先级最高。
- log append p99 超过阈值持续 5 分钟：可能触发 backpressure。
- queue depth 持续超过 high watermark：control 调度或 worker capacity 不够。
- redelivery count 突增：worker 不稳定、网络分区或任务超时。
- stale completion rejection 突增：lease timeout 可能太短，或 worker 卡顿严重。
- actor mailbox backlog 持续增长：某个 actor 成为热点或 owner worker 太慢。
- LLM cache hit rate 大幅下降：模型缓存状态不对或调度策略退化。
- checkpoint fetch latency 突增：对象存储或磁盘变慢。
- worker heartbeat missing：worker 掉线或网络异常。

告警规则要有时间窗口。瞬间抖动不一定需要叫醒人。比如 queue depth 超阈值 10 秒可以记录 warning，持续 5 分钟才触发 page。

不同环境阈值也不同。实验环境可以宽松一些，生产环境要按 SLO 和 error budget 设置。

## Q752. 如何让实验报告自动生成图表？

先让 benchmark 输出机器可读的原始数据，再从原始数据生成图。

当前 `summary.json` 已经有聚合结果，但画图最好保留每次请求的 raw records，比如：

```json
{"policy":"LOCALITY_AWARE","request_id":"...","latency_ms":205,"cache_hit":true,"worker_id":"worker-1"}
```

有了 raw records，可以自动生成：

- latency CDF
- p50/p95/p99 bar chart
- cache hit rate 对比图
- cold start count 对比图
- actor replay commands 对比图
- logstore fsync policy throughput 图
- compactable bytes 趋势图

实现上可以加一个 Python 脚本，比如 `scripts/render_experiment_report.py`。它读取 `benchmark.json`、`logstore_latest.json`、`checkpoint_cache_probe.json` 和 `dashboard_snapshot.json`，用 matplotlib 或 plotly 生成 PNG/HTML，再把图插入 `summary.md`。

要注意图表标题不要夸张。比如写“单机 mock workload 下 locality-aware 降低 p95”，不要写成“locality-aware 全面优于 resource-only”。

## Q753. 如何避免 benchmark 被 debug log 影响？

debug log 会影响 benchmark，尤其是高频请求和小任务场景。

处理方式有几个：

- benchmark 时把日志级别固定，比如只输出 warn/error。
- structured log 写到文件，不写到阻塞的终端。
- 避免在热路径打印每个 task 的大 payload。
- 日志采样，比如每 N 次请求记录一次调试信息。
- 把 benchmark 的结果输出和组件运行日志分开。
- 固定日志配置，不能一轮开 debug、一轮关 debug 后直接比较。

LogServe 的实验脚本已经把 logd、control、worker 日志分开写到文件里，比如 `control.log`、`worker_1.log`。这是对的。下一步如果加更细的 debug log，应该提供 `LOGSERVE_LOG_LEVEL`，并在 `environment.txt` 里记录。

如果做严肃性能报告，我会跑两组：一组 minimal logging 用于性能数字，一组 debug logging 用于排障。不要混着用。

## Q754. 如何用基线系统对比，比如 Celery/Temporal/Ray？

基线对比要先选清楚比较面。Celery、Temporal、Ray 不是同一类系统。

Celery 更像任务队列。可以比较普通 task throughput、retry、worker failure redelivery、result backend 开销。

Temporal 更强在 durable workflow。可以比较 workflow recovery、history replay、activity retry、长时间等待、control 重启后的恢复。

Ray 更偏分布式计算和 actor。可以比较 task DAG、actor throughput、actor failover、对象存储传输。

LogServe 的对比不应该说“全面超过谁”。更合理的对比是选几条场景：

- shared-log replay 后 workflow 状态是否可重建。
- actor snapshot 后恢复命令数。
- LLM locality scheduling 的 cache hit rate。
- checkpoint cache cold/warm latency。
- fault injection 后最终状态是否一致。

每个系统都用它的推荐方式实现同一 workload。比如 Celery 用 Redis/RabbitMQ，Temporal 用官方 server 和 activity，Ray 用 Ray task/actor。然后统一 workload、机器、并发、请求数和报告格式。

这样对比才像工程实验。否则只是拿不同系统的默认 demo 跑一遍，结论很难站住。

## Q755. 如何把“实验不代表生产性能”讲得既诚实又不削弱项目价值？

我会直接说：这些实验不是生产性能报告，但它们证明了系统主链路。

单机实验的价值在于把几个难点跑通了：log-first 写入、metadata bootstrap、workflow replay、actor snapshot、lease redelivery、LLM locality scheduling、checkpoint cache 和 dashboard export。这些不是 README 里的概念，仓库里有代码、测试和实验结果。

不能外推的地方也要讲：

- 没有多机网络。
- 没有多副本 logd。
- mock LLM 不能代表 GPU。
- checkpoint 文件太小。
- 默认样本量偏小。
- fault injection 还没覆盖所有断电和磁盘故障。

这不会削弱项目价值，反而让项目边界更清楚。面试官通常不怕你承认限制，怕的是你把 smoke test 说成 production benchmark。

我会用一句比较稳的结论收尾：LogServe 当前已经完成了可恢复 AI runtime 的主路径验证，实验说明机制方向成立；要证明生产性能，还需要多机、真实 GPU、大样本、长时间运行和更强故障注入。
