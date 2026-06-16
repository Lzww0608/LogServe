# 十四、简历与答辩风险问题：建议主动准备的反问式解释点

这一组回答更像“主动控场”。面试官还没质疑之前，可以先把项目边界、设计主线和后续路线讲出来。这样做有两个好处：一是显得诚实，二是把讨论从“你是不是夸大了”拉回到“你为什么这样设计”。

## Q966. 你如何主动说明项目边界而不显得项目弱？

我会把边界放在介绍项目的前半段讲，而不是等面试官追问时才补。比较好的说法是：

> 这个项目我没有把它包装成生产级替代品。它是一个单机多进程实验环境下跑通的 AI runtime 原型，重点验证 shared log、log-first control plane、workflow replay、actor fencing 和 LLM locality scheduling 这些机制。当前没有做 logd 多副本、高可用 control、真实 GPU 压测和不可信 Python 沙箱。相应的代码里有测试、fault injection 和 benchmark 产物，可以说明这些机制已经跑通。

这样讲不会显得弱，因为边界后面跟着的是实证内容。重点放在“我选择先验证什么”，而不是反复解释“我没做什么”。比如我会立刻补上：

- shared log 有 segment/index/recovery/idempotent append 和 logical trim。
- workflow 有 worker 停止后从已完成 step 继续的测试。
- actor 有 1000 并发 `inc()` 的 mailbox 串行化测试，也有 snapshot replay。
- LLM 有 resource-only 和 locality-aware 的 ablation，也有 checkpoint cache cold/warm probe。

边界说得越早，后面反而越容易展开。面试官会知道我没有把 mock LLM 当真实 GPU，没有把单机多进程当多机集群，也没有把 logical trim 说成 physical compaction。这个态度比硬撑更可信。

## Q967. 你如何把项目定位为“机制验证型系统”，而不是生产替代品？

我会用“机制验证型系统”这个定位来解释项目取舍。

LogServe 的目标是把几组机制串起来并跑通：shared log 作为 source of truth，control plane 采用 log-first 路径，metadata view 可以 replay 重建，workflow 支持 DAG/retry/result ref，actor 支持 command log/snapshot/epoch fencing，LLM scheduling 能利用 worker-local model cache 和 checkpoint cache。

这和生产替代品的评价标准不一样。生产替代品需要多副本、高可用、权限、多租户、沙箱、SLO、真实 GPU、长期压测、灾备、升级兼容。LogServe 当前没有全部覆盖这些内容，所以我不会把它描述成 Temporal、Kafka、Ray、vLLM 的替代品。

我会这样组织回答：

> 我做的是机制验证型系统。它回答的是：如果把 shared log 放在 AI runtime 的核心位置，workflow、actor 和 LLM scheduling 能不能共享同一套恢复语义。这个问题和“能否直接替代 Temporal/Kafka/Ray/vLLM 上生产”不是同一个问题。前者我已经用测试和实验跑通，后者需要多副本日志、真实 GPU、安全沙箱和多机压测继续推进。

这个定位能保护项目，也能突出价值。因为面试官看到的是我知道工业系统的门槛，也知道自己这个项目验证了哪一层。

## Q968. 你如何解释为什么先做单机完整链路，再做多机复制？

我会说：先把语义闭环做对，再扩大部署规模。

分布式系统里，复制不会修复语义错误，只会把错误放大。如果单机里都没有定义清楚 log-first、idempotency、lease epoch、actor command_seq、snapshot replay、result_ref 这些语义，多机复制以后只会更难调试。

所以我的顺序是：

1. 先做 logstore 的 append/read/recovery，确认 shared log API 能承载事件。
2. 再做 control 的 log-first 路径，让 task/workflow/actor/LLM 状态都能 replay。
3. 接着做 worker、lease、redelivery、stale completion rejection，解决至少一次执行带来的状态污染。
4. 然后补 actor snapshot、logical trim、LLM checkpoint cache 和 locality-aware scheduling。
5. 最后再考虑 logd 多副本、control leader election、多机 worker 和真实对象存储。

面试时我会用一句话概括：

> 我先把“写了什么事件、如何重建状态、重复完成怎么处理、旧 worker 怎么 fencing”这些语义做扎实，再上多机复制。否则 Raft 复制的是一堆还没定义清楚的状态变化。

这属于工程顺序问题。多机复制下一步可以接 Raft 或直接换 Kafka/Pulsar 作为日志后端，但上层 workflow/actor/LLM 的事件语义应该先稳定。

## Q969. 你如何把 shared log、workflow、actor、LLM scheduling 串成一个完整故事？

我会从一个请求的生命周期讲，而不是从模块列表讲。

故事可以这样展开：

> 用户通过 Python SDK 提交任务、workflow、actor call 或 LLM request。control plane 收到请求后，先把事件写进 shared log，再更新 metadata view。worker 通过 poll 拿任务执行，执行过程继续写 started/completed/failure 或 LLM model load 事件。系统挂掉后，control 可以从 task stream、workflow stream、actor stream、LLM stream replay 出当前状态。

然后把三条上层能力接上：

workflow 解决的是多 step 依赖。`@workflow` 被 SDK trace 成 DAG，control 根据依赖调度 ready step。某个 step 成功后写 `StepSucceeded`，下游 step 才 ready。worker 挂掉后，已经 succeeded 的 step 不重跑。

actor 解决的是有状态对象。actor 的每次命令先写 `ActorCommandSubmitted`，分配 `command_seq`，执行完成后写 `ActorCommandApplied`。同一 actor 的 command 通过 mailbox 串行化，owner 失联后用 epoch fencing 防止旧 worker 写状态。

LLM scheduling 解决的是 AI runtime 的 locality。LLM task 写 `ModelLoadStarted/ModelLoaded/LLMCompleted`，worker 上报 model cache，checkpoint cache 记录 cold/warm，scheduler 根据 cache 和历史 EWMA latency 做选择。

最后收束到主线：

> 这三个模块不是孤立 demo。workflow、actor 和 LLM 都把状态变化写进 shared log，都依赖 replay，都要处理重复执行和故障恢复。区别只是 workflow 关心 DAG，actor 关心状态串行化，LLM 关心模型缓存和冷启动。

## Q970. 你如何用一个故障场景展示系统价值？

我会用 `simple_rag` workflow 的恢复场景，因为它最容易讲清楚。

场景是：

```python
@workflow
def simple_rag(query: str):
    vec = embed(query)
    docs = search(vec)
    ans = generate_mock(query, docs)
    return ans
```

假设 worker 执行完 `embed` 后挂掉。普通脚本执行里，这个流程可能要从头跑，或者需要用户自己写恢复逻辑。LogServe 的做法是：`embed` 成功时已经写了 `StepSucceeded`，workflow stream 里有它的结果。control 重启或新 worker 接管后，workflow replay 能看到 `embed` 已经完成，于是只调度 `search` 和 `generate_mock`。

这能展示几个系统价值：

- shared log 保存了已经发生的状态变化。
- metadata view 丢失后可以重建。
- workflow engine 能根据 DAG 依赖继续调度 ready step。
- worker 至少一次执行不会导致已完成 step 无脑重跑。
- 最终结果仍然通过 step id 和 input hash 去重。

我会顺手提到测试：项目里有 worker 在 `embed` 后停止、另一个 worker 继续执行的集成测试，断言 `embed` attempts 仍然是 1，workflow 最终 completed。这个故障场景比单纯说“支持恢复”更有说服力。

## Q971. 你如何用一个实验结果展示机制有效？

我会优先讲 actor snapshot replay 的结果，因为它和机制的关系最直接。

实验里创建 `Counter` actor，连续执行 20 次左右的命令后生成 snapshot。没有 snapshot 时，actor replay 需要从头处理 21 条 command 相关记录；启用 snapshot 后，snapshot replay 只需要处理 1 条 tail command。这个结果说明 snapshot 改变了恢复路径：系统先从 result store 加载 snapshot state，再回放 snapshot 之后的 tail log。

这个实验可以这样讲：

> full replay 是从 actor stream 开头读完整历史，snapshot replay 是从最近 snapshot 加 tail log 恢复。实验里 replay command count 从 21 降到 1，说明 snapshot 已经接入 replay 路径，并非只写了一个旁路文件。

我也会补一句边界。这个实验的 actor state 很小，命令数不大，所以它只能证明机制生效，不能直接推出大规模 actor 的恢复时间下降多少倍。下一步要做 1k、10k、100k command 的曲线，报告 wall-clock recovery time、snapshot size、tail log 长度和 compactable bytes。

如果面试官更关心 LLM，我会换成 locality-aware 的实验：resource-only cache hit rate 0.833，locality-aware 1.0；resource-only p95 305ms，locality-aware 205ms。这个结果说明缓存感知调度减少了 cold start，但样本量只有 6 个请求级别，统计显著性还不够。

## Q972. 你如何说明你理解开源系统，而不是盲目造轮子？

我会主动把开源系统放进架构对照里讲。

Temporal 对应 workflow durable execution，Kafka/Pulsar 对应 shared log，Ray 对应分布式执行和 actor，vLLM/Ray Serve 对应 LLM serving，PostgreSQL 对应 materialized view，MinIO/S3 对应 result store。LogServe 没有试图把这些系统都重新做一遍，它是在一个小系统里把它们背后的关键机制串起来。

我的回答可以这样说：

> 如果是线上业务，我不会建议用 LogServe 替代 Temporal/Kafka/Ray/vLLM。这个项目的目标是把这些系统背后的几个核心问题拆开做一遍：日志作为状态源、view rebuild、workflow replay、actor fencing、模型缓存感知调度。做完以后，我更能理解为什么成熟系统要有 WAL、history、offset、lease、fencing token、consumer group 和 snapshot。

然后举替换关系：

- logstore 可以换 Kafka、Pulsar 或 Raft log。
- Python executor 可以换 Ray、Kubernetes Job 或容器池。
- mock LLM 可以换 vLLM、Ray Serve、Triton。
- local result store 可以换 S3/MinIO。
- in-memory metadata view 可以换 PostgreSQL。

这说明我理解组件边界。自研部分用来展示底层能力，生产化部分可以接成熟组件。

## Q973. 你如何说明哪些地方可以直接替换成成熟组件？

我会按层说。

shared log 层可以替换。当前 logd 是单机 logstore，后续可以换成 Kafka/Pulsar，或者把 logd 改成 Raft 多副本。只要保留 `AppendLog`、`ReadLog`、`ListStreams`、`TrimStream` 这类语义，上层 workflow/actor/LLM 的事件模型可以继续用。

执行层可以替换。当前 worker 用 Python runner 执行源码，也有本地 executor pool。生产里可以换成 Ray worker、Kubernetes Job、容器池、Firecracker microVM 或 gVisor sandbox。control plane 仍然负责任务状态、lease、redelivery 和 completion fencing。

LLM serving 可以替换。mock LLM 用于无 GPU 实验；真实部署可以接 vLLM OpenAI-compatible API、Ray Serve、Triton 或 KServe。LogServe 保留模型注册、cache 状态、checkpoint fetch、调度决策和 event log。

metadata view 可以替换。当前可以用 memory store 或 PostgreSQL。PostgreSQL 负责 query 和 dashboard，不承担 source of truth。以后也可以用 etcd、CockroachDB 或专门的 OLAP/TSDB 做不同读模型。

对象存储可以替换。local result store 适合本地实验，MinIO/S3 适合大结果和 actor snapshot。只要 result ref 能被 replay 阶段解析，后端可以替换。

这个回答的重点是：LogServe 的边界是清楚的。自研模块不是绑定死的，很多都能换成成熟组件。

## Q974. 你如何说明哪些地方是为了展示底层能力而自研？

我会点名三块。

第一块是 logstore。成熟方案可以用 Kafka/Pulsar/Raft log，但我自研了 segment、index、CRC、recovery、partial tail truncate、idempotent append、fsync policy 和 logical trim。这样做是为了展示我理解 WAL/shared log 的基本机制，而不是只会调 Kafka API。

第二块是 control plane 的 replay 和状态机。Temporal 已经把 workflow 做得很成熟，但我自己实现了 `TaskSubmitted/Started/Completed`、`StepScheduled/Started/Succeeded/Failed`、`ActorCommandSubmitted/Applied`、`LLMCompleted` 这些事件的 materialization 和 replay。这样能展示我理解 event sourcing、metadata view、幂等、terminal state、lease epoch 和 actor epoch。

第三块是 LLM locality scheduler。真实推理可以交给 vLLM，但我自己实现了 model registry、worker cached_models 上报、checkpoint cache、cold/warm 指标、LLM event replay 和 materialized EWMA stats。这样能展示我理解 AI infra 里模型缓存、冷启动和调度之间的关系。

我不会说这些自研实现都比成熟系统强。它们的作用是把底层能力显性化。面试时如果被问“为什么造轮子”，我会回答：

> 我自研的是最能体现理解深度的控制面和日志语义，不是为了替代成熟基础设施。真正生产化时，底层存储、执行器和 serving backend 都可以换成熟组件。

## Q975. 你如何说明下一步生产化 roadmap？

我会按优先级讲，不会把路线列成无限清单。

第一步是可靠性。logd 现在是单机，生产化要做多副本复制，可以接 Raft 或 Kafka/Pulsar。control plane 需要 leader election，避免两个 control 同时调度或 grant actor ownership。还要补 crash consistency 测试，覆盖 kill -9、磁盘满、fsync 慢、object store 超时和 metadata view drift。

第二步是执行安全。当前 Python function source 远程执行只适合可信实验环境。下一步要用容器、gVisor、Firecracker 或 Kubernetes Job 隔离用户代码，限制 CPU、内存、文件系统和网络出口。代码传输也要从 function_source 改成 code artifact：`code_ref + code_hash + runtime_env`。

第三步是真实 LLM serving。接真实 vLLM/GPU，记录 prompt tokens、output tokens、prefill/decode latency、batch size、GPU memory、OOM、token throughput。调度器要把 prompt length、max_tokens、GPU capacity 和 batching 状态纳入 predicted latency。

第四步是多机实验和可观测性。把单机多进程扩展到多节点，测试 worker 节点故障、control 重启、logd 恢复、model cache 丢失、对象存储延迟。接 Prometheus metrics 和 tracing，展示 append latency、queue depth、lease expired、actor backlog、cache hit、cold start、p95/p99。

第五步是产品化 API。proto 要做兼容策略，Dashboard 要分页和权限，多租户要有 namespace、ACL、quota 和审计。workflow 还需要 cancellation、pause/resume、DLQ、手动重跑 failed step、workflow versioning。

我会把这条 roadmap 收成一句话：

> 现在项目已经把机制链路跑通。下一步要减少功能扩张，把可靠性、安全隔离、真实 GPU、多机实验和 API 兼容补齐，让它从机制验证系统往可运行平台靠近。
