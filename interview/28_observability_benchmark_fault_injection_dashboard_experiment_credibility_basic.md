# 十、Observability、Benchmark、Fault Injection、Dashboard 与实验可信度（基础）

这一组问题主要考察项目有没有可验证的证据。回答时不要只说“我做了 benchmark”，而要说清楚测了什么、怎么跑、结果文件在哪里、结论边界是什么。LogServe 这部分的主线是：用结构化日志、dashboard snapshot、故障注入和 benchmark，把 shared-log runtime 的恢复能力、调度收益和实验限制讲清楚。

## Q691. 项目包含哪些 benchmark？

LogServe 里主要有四类 benchmark 或实验脚本。

第一类是 LogStore benchmark。它由 `scripts/logstore_benchmark.sh` 调用 `cmd/logserve-logbench`，专门测 shared log 的 append、read 和 recovery。这个 benchmark 会比较不同 fsync 策略，比如 `always`、`batch`、`interval`，用来说明持久化强度和写入吞吐之间的取舍。

第二类是 runtime benchmark。它在 `examples/evaluation/benchmark.py` 里，覆盖 workflow latency、普通 task throughput、actor snapshot replay ablation、LLM cold/warm start、LLM locality scheduling ablation。这一组更接近系统级验证，因为它会启动 control、logd 和多个 worker，然后通过 Python SDK 走完整链路。

第三类是 checkpoint cache probe。它在 `examples/evaluation/checkpoint_cache.py` 里，专门验证 file-backed checkpoint cache 是否真的落到 worker-local cache 目录。它会提交同一个模型的 cold request 和 warm request，对比 `cache_hit`、`checkpoint_fetch_ms`、`cache_used_bytes` 和本地 artifact。

第四类是故障注入和 dashboard 导出。严格说它们不都是性能 benchmark，但它们是实验证据的一部分。`scripts/run_experiment.sh` 会跑 fault-injection integration tests，并导出 `dashboard_snapshot.json`、`summary.json` 和 `summary.md`。这样一次实验不是只留下终端输出，而是留下可以复查的结果目录。

## Q692. logstore benchmark 测了哪些指标？

LogStore benchmark 重点测 shared log 的三件事：写入速度、读取速度和崩溃恢复成本。

输出里会记录这些字段：

- `records`：总写入记录数。
- `streams`：参与写入的 stream 数量。
- `payload_bytes`：每条记录的 payload 大小。
- `policy`：fsync 策略，常见是 `always`、`batch`、`interval`。
- `segment_size_bytes` 和 `segment_count`：segment 配置和最终生成的 segment 数。
- `append_duration_ms`：写入全部记录耗时。
- `read_duration_ms`：读回记录耗时。
- `recover_ms`：重新打开 store 并恢复 index、seq、idempotency 状态的时间。
- `append_records_sec`：每秒 append 记录数。
- `read_records_sec`：每秒读取记录数。
- `read_records`：实际读回的记录数，用来检查没有漏读。

面试里我会强调，这个 benchmark 不是只看一个吞吐数字。`always` fsync 会更慢，但 durability 边界更强；`batch` 和 `interval` 吞吐更高，但客户端收到成功后如果进程或机器崩溃，可能存在更大的 durability window。这个取舍要和系统语义一起解释。

## Q693. workflow benchmark 测了哪些指标？

Workflow benchmark 测的是端到端 latency，而不是单个 Python 函数的运行时间。

`examples/evaluation/benchmark.py` 里有一个 `rag_with_llm` workflow，大致链路是：

```python
vec = embed(query)
docs = search(vec)
prompt = build_prompt(query, docs)
return llm_generate("model-A", prompt, version="v1", adapter="mock")
```

benchmark 会多次提交这个 workflow，并记录：

- `requests`：提交了多少个 workflow。
- `median_ms`：端到端中位延迟。
- `p95_ms`：端到端 p95 延迟。
- `p99_ms`：端到端 p99 延迟。

这里的“端到端”包括 SDK 提交、control 写 log、DAG step 调度、worker poll、step 执行、LLM mock 调用、step completion、workflow final completion 等路径。它不只是 `embed()`、`search()` 这些函数本身的耗时。

单机实验里 workflow p95/p99 约为 823 ms。这个数字可以说明链路跑通了，也能看出调度和 RPC 的实际开销，但样本数较小，不能直接当成生产 p99 性能承诺。

## Q694. actor snapshot ablation 证明了什么？

Actor snapshot ablation 证明 snapshot 能减少 actor crash recovery 时需要回放的命令数量。

实验里会创建两个 actor：

- 一个开启 snapshot，比如 `snapshot_every=5`。
- 一个基本等同于关闭 snapshot，比如 `snapshot_every=100000`。

然后对两个 actor 执行相同数量的 `inc()` 命令，再分别调用 `replay_actor()`。对比的核心指标是：

- `full_replay_commands`：如果从头扫 actor stream，需要解释多少条命令。
- `snapshot_replay_commands`：如果从 snapshot 开始恢复，只需要回放多少条 snapshot 之后的 tail log。
- `trimmed_replay_commands`：配合 logical trim 后，replay 看到的有效 tail log 命令数。

在已有单机实验里，20 次 command 后，full replay 大约需要解释 21 条命令，而 snapshot replay 只需要 1 条。这个结果说明 actor snapshot 不是装饰功能，它确实改变了恢复成本。

我会谨慎表述这个结论：当前实验直接证明的是“需要回放的命令数显著下降”，不是严格证明所有场景下恢复耗时都按同等比例下降。真正的耗时还会受到 snapshot object 读取、状态大小、对象存储延迟和机器负载影响。

## Q695. LLM locality ablation 证明了什么？

LLM locality ablation 证明：只按 worker 空闲资源调度 LLM 请求不够，模型缓存命中情况会明显影响 cold start 和尾延迟。

实验会比较三种策略：

- `RESOURCE_ONLY`：主要看 worker 是否有空闲容量。
- `LOCALITY_AWARE`：优先选择已经有目标模型缓存的 worker，同时考虑 queue delay 和容量。
- `PREDICTED_LATENCY`：使用 materialized LLM stats，结合历史 EWMA latency、queue penalty、cold start penalty 和 eviction penalty 做选择。

已有单机实验中，`RESOURCE_ONLY` 的 cache hit rate 约为 0.833，p95 latency 约为 305 ms；`LOCALITY_AWARE` 和 `PREDICTED_LATENCY` 的 cache hit rate 达到 1.0，p95 latency 约为 205 ms。这个结果说明 locality-aware 策略确实避开了不必要的 cold start。

但这个实验的边界也要讲清楚。它是在单机、3 worker、mock LLM、小样本请求下跑出来的。它能证明调度机制和指标链路是有效的，不能直接证明真实 GPU 集群里也会得到同样比例的收益。真实场景还要补 prompt length、max tokens、batching、GPU 显存、vLLM queue、网络带宽和大 checkpoint 的实验。

## Q696. checkpoint cache probe 验证了什么？

`checkpoint_cache_probe.py` 验证的是 checkpoint cache 是否真的从 source 目录复制到 worker-local cache，并且 warm request 是否能命中本地缓存。

它会做几件事：

1. 注册一个模型，默认是 `model-D:v1`。
2. 提交第一次 LLM 请求，作为 cold request。
3. 提交第二次 LLM 请求，作为 warm request。
4. 调用 `replay_llm()` 读取两次请求的事件指标。
5. 检查本地 cache artifact 是否存在，比如 `runtime/model-cache/worker-1/model-D-v1.checkpoint` 和 manifest 文件。

正确结果应该满足：

- cold request 的 `cache_hit=false`。
- warm request 的 `cache_hit=true`。
- cold request 有 `checkpoint_fetch_ms`。
- `cache_used_bytes` 大于 0。
- `cache_capacity_bytes` 大于 0。
- `validation_errors` 为空。
- 本地 `model-cache` 目录里能看到对应 checkpoint 文件和 manifest。

你之前的实验结果里，cold request 的 `checkpoint_fetch_ms=1`，warm request 的 `checkpoint_fetch_ms=0`，`cache_used_bytes=3145728`，`cache_capacity_bytes=16777216`，并且 `validation_errors=[]`。这说明 file-backed checkpoint cache 路径是跑通的。

如果 cold 和 warm 总延迟很接近，也不代表 probe 错了。因为实验里的 checkpoint 文件很小，source 和 cache 都在本机磁盘上，fetch 成本本来就低。这个 probe 的重点是验证 cache artifact、cache hit/miss 和 replay 指标，而不是证明大模型 checkpoint 的真实冷启动收益。

## Q697. fault injection 覆盖哪些故障？

当前故障注入主要覆盖 worker、queue redelivery、control restart 和 logstore recovery 相关路径。

`scripts/run_experiment.sh` 会跑一组 integration tests，测试名包括：

- `TestWorkflowWorkerRecoveryContinuesAfterCompletedStep`
- `TestActorCounterRecoverySnapshotAndReplay`
- `TestRunningTaskIsRedeliveredAfterWorkerLeaseExpires`
- `TestPolledTaskIsRedeliveredWhenWorkerDiesBeforeStart`
- `TestStaleTaskCompletionRejectedAfterRedelivery`
- `TestOrdinaryTaskSurvivesControlRestartFromTaskSpecLog`
- `TestControlRestartBootstrapsWorkflowAndModelStateFromLog`

这些测试覆盖的语义包括：

- worker 执行到一半挂掉后，任务能被 redelivery。
- workflow 中已经完成的 step 不会从头重复执行。
- worker poll 到任务但还没 StartTask 就挂掉，任务仍能重新投递。
- 旧 worker 在 lease 过期后提交 completion，会被 lease epoch 拒绝。
- control 重启后能从 task/workflow/model log 重建 metadata view。
- actor 可以通过 snapshot 和 tail log 恢复状态。

`fault_injection.json` 会把结果整理成 `worker_kill_recovery`、`queue_redelivery`、`control_restart_probe` 和 `logd_restart_probe`。其中 logd restart 目前主要由 logstore recovery 测试和进程日志覆盖，还不是完整的多副本 log 服务可用性测试。

## Q698. dashboard snapshot 包含哪些对象？

Dashboard snapshot 是 control plane 对当前 materialized view 的一次导出。它不是直接扫所有日志，而是读取 control 维护的 metadata view，再补充 logstore compactable stats。

当前 snapshot 主要包含：

- `queue_depth`：control 内部待调度队列长度。
- `queue_high_watermark`：backpressure 的队列水位阈值。
- `redelivery_timeout_ms`：任务 redelivery 超时时间。
- `scheduling_policy`：当前 LLM 调度策略。
- `tasks`：任务列表，包括 task id、状态、worker、workflow id、step id、actor id、LLM model 等。
- `workflows`：workflow 列表和每个 workflow 的 step 状态。
- `actors`：actor 状态，包括 owner、epoch、command count、snapshot ref 等。
- `workers`：worker 列表，包括 capacity、running tasks、cached models、last heartbeat。
- `models`：已注册模型。
- `last_log_append_ms`：最近一次 log append 耗时。
- `log_append_slow_ms`：backpressure 里配置的慢 append 阈值。
- `compactable_log_records` 和 `compactable_log_bytes`：logical trim 后可压缩日志的规模。

面试里可以这样说：dashboard snapshot 的价值不是做一个漂亮 UI，而是把 runtime 的关键状态导出成可检查、可写报告、可对比的 JSON。它能帮助解释任务为什么排队、actor 在哪个 worker 上、LLM 请求是否命中缓存、snapshot-aware retention 产生了多少可压缩日志。

## Q699. structured logs 记录哪些字段？

LogServe 的结构化日志由 `internal/observability/log.go` 统一输出。每条日志本质上是一行 JSON。

基础字段包括：

- `ts`：UTC 时间戳，使用 RFC3339Nano。
- `level`：日志级别，当前主要是 `info` 和 `error`。
- `event`：事件名，比如 `control_started`、`worker_registered`、`workflow_completed`。
- `error`：错误日志中会带这个字段。

不同模块会追加自己的业务字段。比如：

- control 启动日志会记录 `addr`、`log_addr`、`metadata_store`。
- worker 注册和执行日志会记录 `worker_id`、`task_id`。
- executor pool 启动会记录 `capacity`、`task_pool_size`、`llm_pool_size`、`actor_pool_size`。
- workflow step 调度、成功、重试会记录 workflow id、step id、task id、attempt、latency 等信息。
- actor command applied 会记录 actor id、call id、command sequence、worker 和 epoch。
- LLM stats materialize 失败会记录 task id 和 worker id。
- log stats 读取失败会记录错误。

结构化日志的好处是实验后可以用脚本过滤和聚合，而不是靠人肉读一大段文本。它也让 dashboard 和 benchmark 之外的排障更直接：看到一个 `event`，就能沿着 task id、workflow id、actor id 或 worker id 去查。

## Q700. go test、go vet、race test 分别验证什么？

这三个命令验证的层次不同。

`go test -count=1 ./...` 验证 Go 代码的功能正确性。它会跑所有 Go 单元测试和集成测试，包括 logstore、control、worker、workflow、actor、LLM scheduling、resilience 等路径。`-count=1` 是为了避免 Go test cache 让结果看起来通过但实际上没有重新跑。

`go vet ./...` 做静态检查。它不是完整的 linter，但能发现一些 Go 里常见的危险写法，比如格式化参数不匹配、不可达代码、错误的 struct tag、拷贝锁等问题。

`go test -race -count=1 ./internal/control ./internal/worker` 用 race detector 检查 control 和 worker 这两个并发最重的包。LogServe 里有 queue、worker heartbeat、actor mailbox、executor pool、LLM stats materializer，这些地方如果锁没处理好，很容易出现数据竞争。race test 通过，说明这组测试覆盖到的并发路径没有被 Go race detector 发现问题。

它们不能证明系统没有所有 bug，但能给出三层证据：功能路径能跑、明显静态问题较少、关键并发路径没有暴露数据竞争。

## Q701. 为什么需要 Python unittest 和 compileall？

因为 LogServe 不是纯 Go 项目。它的用户入口在 Python SDK，普通 task、workflow 和 actor 也会通过 Python executor 执行。只跑 Go test，无法覆盖 SDK 的很多问题。

Python unittest 主要验证 SDK 行为，比如：

- `@task`、`@workflow`、`@actor` 装饰器行为。
- workflow tracing 和 StepRef 编码。
- gRPC transport 和 CLI fallback 的请求构造。
- idempotency key 和 payload fingerprint 相关逻辑。
- actor handle 把方法调用转换成 actor command。

`python -m compileall -q sdk/python/logserve` 更像一个快速语法和导入检查。它能发现语法错误、缩进错误、缺失模块、生成代码不匹配等问题。这个检查很快，但对跨语言项目很有用，因为 Go 测试不一定会 import 每个 Python 文件。

实验脚本里还会跑 `python -c "import grpc; import google.protobuf"`，用来确认 gRPC SDK 依赖存在。否则 Python SDK 可能会退回 CLI 或直接失败，实验结果就不干净。

## Q702. 实验运行目录中 environment.txt 的作用是什么？

`environment.txt` 是实验上下文记录。它告诉之后看报告的人：这次实验是在什么机器、什么代码状态、什么参数下跑出来的。

`scripts/run_experiment.sh` 会写入这些信息：

- `run_id`
- `root`
- `run_dir`
- `log_addr`
- `control_addr`
- `checkpoint_source_dir`
- `checkpoint_cache_bytes`
- `worker_capacity`
- `task_pool_size`
- `llm_pool_size`
- `actor_pool_size`
- `generated_at_utc`
- `uname -a`
- `go version`
- `python --version`
- `git rev-parse --short HEAD`
- `git status --short`

这个文件解决的是复现实验的问题。比如同样的 benchmark 数字，在 Windows 本机、Linux 实验机、不同 Go 版本、不同 worker pool size 下都可能不一样。没有 `environment.txt`，别人只能看到数字，看不到数字的来源。

## Q703. command_status.jsonl 的作用是什么？

`command_status.jsonl` 是整次实验的命令执行账本。

它每行是一条 JSON，字段包括：

- `name`：命令名称，比如 `go_test_all`、`go_vet`、`benchmark`、`checkpoint_cache_probe`。
- `exit_code`：退出码，0 表示成功。
- `duration_sec`：运行耗时。
- `log`：对应的日志文件名。

它的价值有三点。

第一，快速判断实验是否整体通过。不需要打开十几个 log 文件，只要看每行 `exit_code` 是否为 0。

第二，定位失败入口。如果 `checkpoint_cache_probe` 失败，就直接去看 `checkpoint_cache_probe.stderr.log`；如果 `go_race_control_worker` 失败，就看对应 race log。

第三，方便脚本汇总。`scripts/summarize_experiment.py` 会读取 `command_status.jsonl`，生成 `summary.json` 和 `summary.md`。JSONL 格式也适合追加写入，中途某一步失败时，前面已经完成的命令状态不会丢。

## Q704. 为什么要保存 summary.md 和 summary.json？

这两个文件服务于不同读者。

`summary.json` 是给程序看的。它把命令状态、benchmark highlights、logstore highlights、checkpoint cache probe、fault injection 和 dashboard snapshot 汇总成结构化数据。后续如果要做趋势分析、CI artifact 对比、实验结果自动检查，可以直接读这个 JSON。

`summary.md` 是给人看的。它把同样的信息整理成表格和列表，适合放进实验报告、项目汇报或者面试材料。看的人不需要知道每个原始文件在哪里，也能快速理解这次实验跑了什么、哪些通过了、核心指标是多少。

保留这两个文件还有一个好处：原始日志很详细，但阅读成本高；summary 文件信息密度更高。真正排障时再回到 `go_test_all.log`、`benchmark.stderr.log`、`worker_1.log`、`control.log` 这些原始文件。

## Q705. 为什么实验结论要说明单机环境边界？

因为实验数字离不开环境。不说明边界，很容易把“单机机制验证”说成“生产级性能结论”，这在面试里反而会减分。

LogServe 当前实验是在单机 Ubuntu 环境里完成的，典型设置是一个 logd、一个 control、三个 worker、mock LLM、file-backed checkpoint cache。它能很好地验证这些问题：

- log-first 路径是否跑通。
- worker 挂掉后任务是否能 redelivery。
- workflow 是否能从已完成 step 后继续。
- actor 是否能通过 snapshot 和 tail log 恢复。
- checkpoint cache 是否真的生成本地 artifact。
- locality-aware scheduling 是否比 resource-only 更容易命中模型缓存。
- dashboard 和 summary 是否能解释系统状态。

但它不能直接证明这些问题：

- 多机网络抖动下的可用性。
- 多副本 logd 的一致性和 leader 切换。
- 真实 GPU/vLLM 的吞吐和尾延迟。
- 几十 GB checkpoint 下的冷启动收益。
- 长时间运行后的 compaction、磁盘压力和对象存储生命周期。
- 大样本 workload 下的稳定 p95/p99。

所以更准确的结论是：当前实验能证明 LogServe 的核心机制在单机环境下可以端到端跑通，并且 ablation 能看到预期趋势；它还不是生产性能报告。这样讲更可信，也给后续扩展留下清楚方向。
