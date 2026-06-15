# 一、项目总览与开场问题（拓展）

这一组问题适合在面试后半段用。回答时不要把 LogServe 说成已经具备完整平台能力，重点讲清楚：当前实现有什么，生产化还差什么，扩展时会改哪些模块。

## Q046. LogServe 和 event sourcing 架构有什么关系？

LogServe 的设计很接近 event sourcing，但不是一个通用 event sourcing 框架。

event sourcing 的基本思路是：系统不直接把当前状态当成唯一事实，而是把每次状态变化记录为事件。当前状态是事件 replay 后得到的结果。LogServe 也是这样做的。比如 workflow 的当前状态不是凭空写进 metadata store 的，而是由 `WorkflowStarted`、`StepScheduled`、`StepStarted`、`StepSucceeded`、`WorkflowCompleted` 这些事件推出来的。actor 的状态也是从 `ActorCreated`、`ActorOwnershipGranted`、`ActorCommandSubmitted`、`ActorCommandApplied`、`ActorSnapshotCreated` 推出来的。

两者的共同点是：

- 事件是事实记录。
- 当前状态是 materialized view。
- 状态可以通过 replay 重建。
- 历史事件可以解释“为什么现在是这个状态”。
- snapshot 可以减少 replay 成本。

LogServe 和传统 event sourcing 的差异也很明显。

第一，LogServe 的事件是运行时事件，不是业务领域事件。它记录的是 task、workflow、actor、LLM serving 的运行状态，比如 step succeeded、actor command applied、model loaded。传统 event sourcing 里经常记录订单创建、库存扣减、支付成功这类业务事件。

第二，LogServe 的事件直接服务调度和恢复。event sourcing 通常强调业务状态重建，LogServe 更关心 worker crash 后怎么继续、actor 状态怎么恢复、LLM 调度怎么利用历史 stats。

第三，LogServe 目前没有完整的 event sourcing 配套能力，比如 schema registry、事件迁移工具、多版本 projector 管理、审计权限模型。它有 event sourcing 的核心思想，但实现范围更窄。

面试里我会这样收口：LogServe 用 event sourcing 的方式管理 runtime 状态，但它的目标不是做业务事件平台，而是做可恢复的 AI runtime。

## Q047. LogServe 和 Kafka Streams、Flink、Temporal 的状态恢复理念有什么异同？

可以从“日志、状态、恢复”三个角度讲。

Kafka Streams 和 Flink 都是流处理系统。它们处理连续数据流，状态通常来自输入 topic 或 checkpoint。Kafka Streams 依赖 Kafka changelog topic 恢复 state store；Flink 依赖 checkpoint/savepoint 恢复算子状态。它们关心的是流计算任务在失败后继续处理数据，避免从头跑全量数据。

LogServe 也用日志恢复状态，但它的日志记录的是 runtime control events。它不是持续消费业务数据流，而是记录 workflow step、actor command、LLM request 这些运行时事件。恢复时，LogServe 不是恢复一个算子的 keyed state，而是恢复 workflow、actor、task queue、model registry、scheduler stats。

Temporal 更接近 LogServe。Temporal 的 workflow history 是 durable execution 的根，worker 重放 history 来恢复 workflow 决策状态。LogServe 的 workflow stream 也有类似味道，`ReplayWorkflow` 会从 `wf:<workflow_id>` 重建 workflow state。两者都强调：内存可以丢，history/log 不能丢。

差异在于成熟度和语义范围。

Temporal 的 workflow history 和 deterministic replay 做得很完整。它要求 workflow 代码可确定重放，有完整的 command/event 协议、task queue、activity retry、timer、signal、versioning 等机制。LogServe 的 workflow 更像 DAG runtime，replay 的是控制面记录的 step 状态，不是完整重放用户 workflow 代码。

Kafka Streams/Flink 的 checkpoint 更偏数据流状态恢复；Temporal 偏 durable workflow execution；LogServe 偏 AI runtime 里的 workflow、actor 和 LLM serving 状态恢复。它借鉴了这些系统的思路，但没有试图覆盖它们全部能力。

如果面试官追问，我会说 LogServe 更像“把 Temporal 的 durable history 思路、Ray 的 actor/task 形态、AI serving 的模型缓存调度，压缩进一个实验系统”。

## Q048. 如果要把 LogServe 做成多租户平台，需要改哪些模块？

多租户不是加一个 `tenant_id` 字段就完了。要改的地方不少。

第一，所有资源 ID 都要带 tenant scope。task、workflow、actor、model、worker、result object、checkpoint cache、dashboard 查询都要能区分 tenant。stream 命名也要改，比如：

```text
tenant:<tenant_id>:task:<task_id>
tenant:<tenant_id>:wf:<workflow_id>
tenant:<tenant_id>:actor:<actor_id>
tenant:<tenant_id>:llm:<task_id>
```

或者保留当前 stream 名，但在 log record metadata 里加 tenant 字段。两种方式都可以，前者隔离直观，后者方便按全局维度查询。

第二，metadata store 要加租户维度。PostgreSQL 表需要 `tenant_id`，唯一索引也要从 `idempotency_key` 变成 `(tenant_id, idempotency_key)`。否则不同租户使用相同幂等键会冲突。

第三，调度器要做资源隔离。现在 worker 是全局资源，多租户后要支持 tenant quota、队列隔离、优先级、公平性、worker pool 绑定。不能让一个租户提交大量 LLM 请求把所有 worker 占满。

第四，result store 和 checkpoint cache 要隔离。workflow result、actor snapshot、模型 checkpoint 不能跨租户可见。object key 需要 tenant prefix，MinIO bucket policy 或访问凭证也要跟着设计。

第五，API 层要接入认证和授权。SDK 提交请求时要带 token，control 要校验 token 对 tenant 的权限。dashboard 也不能展示全局状态给普通用户。

第六，观测指标要按 tenant 拆分。queue depth、latency、error rate、cache hit rate、quota usage 都要能按 tenant 看。

我会优先改 control 和 metadata schema，因为这是隔离的根。UI、文档和高级调度可以后面跟，但 tenant scope 和权限模型一开始就要打进去。

## Q049. 如果接入真实线上服务，哪些 API 需要稳定版本化？

需要版本化的 API 分三类。

第一类是外部用户 API。Python SDK 暴露的 `@task`、`@workflow`、`@actor`、`llm_generate()`，以及 SDK 和 control 之间的 gRPC 请求，都需要稳定。比如 `SubmitTaskRequest`、`SubmitWorkflowRequest`、`CreateActorRequest`、`CallActorRequest`、`SubmitLLMRequest`。这些一旦被用户代码依赖，字段就不能随便改。

第二类是 worker 和 control 的协议。`RegisterWorker`、`Heartbeat`、`PollTask`、`StartTask`、`CompleteTask`、`TaskSpec` 都要稳定。线上环境里经常会出现新旧 worker 同时在线，control 不能因为加了字段就让旧 worker 全部挂掉。

第三类是日志事件 schema。`TaskSubmitted`、`WorkflowStarted`、`StepSucceeded`、`ActorCommandApplied`、`LLMCompleted` 这些事件写入 shared log 后会长期存在。新版本 control 必须能 replay 老日志。这里的版本化甚至比 gRPC API 更重要，因为日志不能像服务端代码一样随时替换。

还有 result store 和 snapshot 格式。actor snapshot 里保存的是 actor state JSON，workflow result ref 指向 object store。以后如果 result encoding 或 snapshot 格式变化，也需要版本字段或 metadata。

我会采用几个规则：

- protobuf 字段只新增，不复用旧 tag。
- JSON event payload 只新增 optional 字段，不改旧字段含义。
- TaskSpec 里新增能力时要有默认值。
- SDK 做语义版本，破坏性变更必须大版本。
- replay reducer 要保留老事件兼容测试。

线上系统最怕“新版本能写，老日志不能读”。所以版本化的重点不是好看，而是保证 replay 长期可用。

## Q050. 如果用户任务有外部副作用，比如扣款或发邮件，exactly-once-ish 还能成立吗？

要分层回答。

LogServe 的 exactly-once-ish 只针对系统内部状态提交，不覆盖用户函数里的外部副作用。

比如一个 task 扣款。worker 调用支付接口成功了，但在写 `TaskCompleted` 或调用 `CompleteTask` 前挂掉。control 看不到这个 task 已经完成，后面可能 redeliver。第二个 worker 再执行一次，就可能再次扣款。LogServe 无法自动知道外部支付系统已经发生过什么。

发邮件也是一样。邮件一旦发出，没有办法靠 LogServe 的 replay 撤回。重复执行就可能重复发送。

所以 exactly-once-ish 还能成立，但边界必须说清楚：

- workflow step result commit 可以去重。
- actor state application 可以按 command_seq 去重和排序。
- stale completion 可以被 lease epoch 或 actor epoch 拒绝。
- 外部 side effect 需要用户自己做幂等。

对扣款这类任务，正确做法是让业务侧也带幂等键。比如 payment request 使用 `business_order_id` 或 `idempotency_key`，支付系统保证同一个 key 只扣一次。发邮件可以用 outbox 表，先写业务事件，再由独立发送器按唯一键去重。

如果要在 LogServe 里进一步支持这类场景，可以提供官方模式：

```text
workflow step -> write outbox event -> external dispatcher with idempotency key
```

但不能在简历或面试里说“LogServe 保证外部副作用 exactly-once”。这句话会被追问穿。

## Q051. 如果任务执行结果很大，日志里存结果还是存引用？为什么？

大结果应该存引用，不应该直接塞进 shared log。

当前项目已经按这个方向做了。workflow step result 如果超过 inline threshold，就通过 result store 写到本地或 S3-compatible MinIO，workflow log 里只保留 `result_ref`。actor snapshot 也是一样，snapshot 内容写 result store，actor stream 里保留 `snapshot_ref`。

原因有几个。

第一，日志要保持可 replay。shared log 适合存事件，不适合存大对象。如果每个 step 都写几 MB、几十 MB 的 result，replay 会很慢，log segment 也会膨胀。

第二，事件应该描述状态变化，不应该变成对象存储。日志里保存 result ref，reducer 需要时再加载对象。这比每次读 log 都把大结果扫一遍更合理。

第三，大对象有自己的生命周期。结果可能需要过期、压缩、迁移、加密、冷热分层。这些更适合 object store 做，不适合 append-only log 做。

第四，日志复制和恢复成本会下降。logd 只关心事件顺序和持久化，小文件或大对象交给 MinIO 这类系统。

需要注意的是，存引用也带来新问题：日志和 object store 之间不再是单一原子写。如果 log 里有 `result_ref`，但 object 丢了，replay 会知道结果存在，却加载不到内容。生产化要补对象校验、引用清理、重试写入和生命周期管理。

## Q052. 如果模型 checkpoint 是几十 GB，当前 cache 策略会暴露什么问题？

几十 GB checkpoint 会把当前 mock cache 策略的问题放大。

第一，冷启动时间会很长。当前实验里 checkpoint 文件很小，fetch/load 只有毫秒级。真实模型如果几十 GB，从对象存储拉到本地磁盘可能要几十秒甚至几分钟。这个时候 locality-aware 调度收益会更明显，但冷启动路径也更容易成为瓶颈。

第二，磁盘容量会紧张。现在 cache manager 有 capacity 和 LRU eviction 的基本思路，但几十 GB 模型下，容量规划会变成核心问题。一个 worker 可能只能放少数几个模型，eviction 代价很高。频繁淘汰和重新拉取会让系统抖动。

第三，checkpoint fetch 不能简单同步阻塞。大模型下载应该有进度、并发限制、断点续传、校验、临时文件和原子 rename。否则下载一半失败可能留下坏 cache。

第四，多 worker 同时 cold start 会打爆源存储。很多请求同时需要同一个模型时，不能让每个 worker 都独立从 MinIO 拉几十 GB。要考虑预热、分发、限流，甚至节点本地共享缓存。

第五，调度器要把“加载中”也纳入状态。现在主要区分 cached 和 not cached。真实场景还要有 loading、failed、evicting、pinned 等状态。一个模型正在加载时，后续请求可以选择等待该 worker，而不是再触发新的 cold start。

第六，checksum 和版本一致性很重要。几十 GB checkpoint 不能只靠文件名判断命中，需要 manifest 记录 size、hash、model version、created time，启动时扫描 manifest 并验证文件。

所以我会说：当前 cache 策略能证明机制，但真实大 checkpoint 需要把 cache manager 做成独立子系统，而不是简单文件拷贝。

## Q053. 如果要支持 GPU 调度，你会把 GPU 拓扑和显存状态放在哪里？

我会把 GPU 状态放在 worker heartbeat 和 metadata view 里，但最终要让调度器读 materialized view，而不是每次直接探测机器。

具体做法是扩展 worker 上报信息。现在 worker 注册和 heartbeat 已经会上报 capacity、labels、cached models。可以继续加：

```text
gpu_count
gpu_devices: [
  id,
  uuid,
  model,
  total_memory_bytes,
  free_memory_bytes,
  utilization,
  numa_node,
  mig_profile,
  running_model_ids
]
interconnect: nvlink / pcie
driver_version
cuda_version
```

control 的 metadata store 保存这些 GPU resource view。调度器选择 worker 时，不只看 worker capacity，还要看目标模型需要多少显存、是否支持 tensor parallel、是否需要同机多卡、GPU 是否已有同模型实例。

GPU 拓扑可以放在 worker resource report 中，因为 worker 最了解本机硬件。control 不应该自己登录机器查 GPU。worker 可以周期性通过 NVML 或 DCGM 采集显存、利用率、温度、错误状态，再 heartbeat 给 control。

调度器还需要区分静态信息和动态信息。GPU 型号、显存总量、NUMA、NVLink 拓扑相对静态；free memory、utilization、active model、queue wait 是动态的。静态信息可以注册时上报，动态信息 heartbeat 更新。

如果要和 Kubernetes 集成，还可以把节点 GPU 资源、device plugin 信息、pod placement 放进同一个 resource view。LogServe 自己做模型调度，但底层资源发现可以借 K8s。

我不会把 GPU 状态写进 workflow stream 或 actor stream。它是调度资源状态，应该在 worker/system 视图里；只有调度决策和 LLM 完成事件需要进入可 replay 的事件流。

## Q054. 如果要支持跨机 actor 迁移，shared log 需要提供哪些额外保证？

跨机 actor 迁移的难点是：同一 actor 的状态不能被两个 worker 同时推进。

shared log 至少要提供这些保证。

第一，同一 actor stream 内事件有严格顺序。`ActorCommandSubmitted`、`ActorOwnershipGranted`、`ActorCommandApplied` 必须按 seq 排列，replay 时能得到唯一状态。

第二，append 要支持 per-stream conditional append。比如“只有当前 owner epoch 还是 5 时，才允许写 epoch 6 的 ownership grant”。否则两个 control 或两个迁移流程可能同时授予不同 worker。

第三，idempotent append 要可靠。迁移过程中网络超时很常见，同一个 ownership event 重试不能写出两份不同结果。

第四，要有 fencing token。LogServe 当前用 `owner_worker_id + epoch` 拒绝旧 worker completion。跨机迁移时这个机制更重要。新 owner 必须拿到更高 epoch，旧 owner 即使还活着，也不能继续写 `ActorCommandApplied`。

第五，snapshot 和 tail log 要有一致边界。迁移时新 worker 可以从 snapshot 加 tail log 恢复 actor state，但它必须知道 snapshot 覆盖到哪个 command count，之后要从哪里继续 replay。

第六，任务分发要和 ownership 同步。不能出现 ownership 已经迁移到 worker-2，但 command 还被 worker-1 poll 到的情况。control 的 `actorMailboxReady()` 和 target worker 检查要基于最新 owner/epoch。

如果继续强化，我会给 actor stream 加 compare-and-append 或 stream-level lease。现在单控制面下可以靠 control 内部锁和 epoch 管住；多控制面或跨机迁移要把这个约束下沉到 log 或一致性存储里。

## Q055. 如果要支持 SLA/SLO，调度器需要采集哪些指标？

要支持 SLA/SLO，调度器不能只看“有没有空 worker”。它至少要知道请求排队、执行、模型加载和错误情况。

任务级指标：

- queue wait time
- execution latency
- end-to-end latency
- retry count
- timeout count
- failure rate
- redelivery count

workflow 级指标：

- workflow end-to-end latency
- step latency
- critical path latency
- step retry distribution
- completed/failed/running counts

actor 级指标：

- mailbox depth
- per-actor queue wait
- command latency
- actor recovery time
- snapshot replay commands
- ownership transfer count
- stale completion rejected count

LLM 级指标：

- cache hit rate
- cold start latency
- checkpoint fetch latency
- model load latency
- first token latency
- total latency
- tokens/sec
- model eviction count
- GPU memory usage

worker 级指标：

- running tasks
- local executor queue depth
- local queue wait
- CPU/memory/GPU usage
- heartbeat age
- model cache used/capacity

logd 和 control 也要采集：

- log append latency
- read latency
- append error rate
- queue high watermark
- control RPC latency
- metadata update latency

调度策略可以用这些指标做几件事：发现某个 worker 队列太深就少派；SLO 快要超时就优先调度；LLM 请求优先选择 warm cache；actor mailbox 太长时限制同一 actor 的提交速率。

我会避免说“采集越多越好”。真正有用的是能进入调度决策和告警的指标。否则只是 dashboard 上好看。

## Q056. 如果引入权限系统，哪些资源需要做 ACL？

至少这些资源需要 ACL。

任务和 workflow。用户只能提交、查询、取消自己 tenant 下的 task/workflow。管理员可以看全局或跨 tenant 状态。

actor。actor 有状态，可能保存用户会话、agent memory、业务上下文。`CreateActor`、`CallActor`、`GetActorStatus`、`ReplayActor` 都要做权限检查。

模型。`RegisterModel`、使用某个模型、查看模型路径和大小，都需要权限。某些模型可能只允许特定团队使用。

worker 和资源池。普通用户不应该随便指定 worker，也不应该看到所有 worker 的地址、labels、GPU 信息。资源池可以按 tenant 或项目隔离。

shared log。直接读 log 的权限要非常谨慎。log 里可能有 prompt、参数、错误信息、result ref。`ReadLog`、`ListStreams`、`ReplayWorkflow` 这类接口要按 tenant 和资源 ID 校验。

result store 和 snapshot。workflow result、actor snapshot、checkpoint artifact 都可能有敏感内容。object key 要有 tenant prefix，访问时要检查权限，不能只靠知道 ref 就能读。

dashboard 和 benchmark report。dashboard 里有任务状态、worker、model cache、错误信息。多租户下要按权限过滤。

backpressure、scheduler policy、model registry 这些系统配置应该只给管理员。

我会把权限模型放在 control 的 API 层，而不是让 SDK 自己决定。SDK 只能携带 token；真正的鉴权必须在服务端做。

## Q057. 如果要给用户提供“审计日志”，现有 shared log 能否直接复用？

可以复用一部分，但不能直接把 shared log 原样当审计日志给用户。

shared log 的优点是它已经记录了很多事实：谁提交了 task，workflow 哪一步成功，actor command 什么时候应用，LLM 请求在哪个 worker 完成。这些确实很适合做审计来源。

但直接暴露 shared log 有几个问题。

第一，shared log 是内部事件格式，不一定适合用户看。比如 `StepSucceeded`、`ActorCommandApplied` 对系统有意义，但用户可能更想看“某个用户在某个时间调用了哪个 workflow，输入是什么，结果在哪里”。

第二，日志里可能有敏感 payload。prompt、args_json、result_json、error message 都可能包含用户数据。审计日志需要脱敏和权限过滤。

第三，审计日志需要稳定查询接口。shared log 当前更偏 replay 和恢复，不一定支持按用户、时间、资源类型、操作类型高效查询。

第四，审计日志不能被 logical trim 影响。actor stream 做 snapshot-aware retention 后，旧 command 可能被默认隐藏或标记 compactable。审计日志通常需要保留更久，甚至有合规要求。

我会这样设计：shared log 作为审计来源之一，control 另外 materialize 一份 audit view 或 audit stream。audit event 可以包含：

```text
tenant_id
user_id
operation
resource_type
resource_id
request_id
status
timestamp
client_ip
redacted_payload_hash
```

这样既能利用已有事件，又不会把内部恢复日志直接暴露给用户。

## Q058. 如果事件 schema 变更，老日志 replay 如何兼容？

老日志兼容要靠几个原则。

第一，新增字段必须可选。老日志没有这个字段时，reducer 要有默认值。比如 `timestamp_ms` 没有就用 log record 的 timestamp；actor command 没有 `CommandSeq` 时，可以退回到 `CommandCount`。

第二，不复用字段含义。比如一个字段原来叫 `worker_id`，就不能在新版本里改成 owner id 或 executor id。要改就新增字段。

第三，新增 event type 时，老 reducer 可以忽略它，新 reducer 能处理它。比如以后新增 `ActorCommandCancelled`，老版本不知道这个 event type，最多不支持新语义，但不能因为未知事件直接崩掉。

第四，破坏性变更要做迁移。可以加 `schema_version`，也可以写一个 offline migration 工具，把老事件转换成新格式后写入新 stream。这个成本高，所以最好少做。

第五，CI 里要有老日志 replay 测试。保存一些真实或样例 log records，新版本每次改 reducer 都跑 replay，确保老 workflow、actor、LLM stream 还能重建。

当前 LogServe 主要靠 JSON optional field 和 reducer 默认值来兼容。对实验项目够用。生产化时，我会补 event schema 文档、兼容性测试和示例日志 corpus。

## Q059. 如果多个版本 worker 同时在线，事件和 TaskSpec 如何兼容？

多版本 worker 同时在线时，最重要的是 control 不能把新 worker 才懂的任务发给旧 worker。

我会先给 worker 注册信息加 capability。比如 worker heartbeat 或 register 时上报：

```text
worker_version
supported_task_spec_fields
supported_adapters
supports_actor
supports_llm
supports_checkpoint_cache
python_executor_protocol
```

control 在调度时根据 TaskSpec 的需求过滤 worker。比如一个 task 需要 LLM checkpoint cache，就不能派给不支持 cache 的旧 worker；一个 actor task 需要 `ActorCommandSeq`，旧 worker 如果不知道这个字段，就不应该接。

TaskSpec 本身要向后兼容。新增字段要有默认值，旧 worker 忽略未知字段时仍能执行普通任务。比如普通 Python task 不需要 LLM 字段，也不需要 actor 字段。

事件也要兼容。worker 写 `TaskStarted`、`TaskCompleted` 时，如果新版本多写了 `local_queue_wait_ms`，老 control 应该可以忽略；新 control 读老 worker 写的事件时，也要能接受字段缺失。

升级策略上，我会推荐先升级 control，再滚动升级 worker。control 要能理解新旧 worker；等 worker 全部升级后，才启用必须依赖新 capability 的功能。

如果要更稳，可以在 model registry 或 task submission 里声明 minimum worker capability。调度器做硬过滤，不满足就让任务保持 queued 或直接返回错误。

## Q060. 如果要开源这个项目，你会优先完善哪些文档、测试和 CI？

我会先补三类东西。

文档方面，优先写清楚怎么跑，而不是先写大而全的架构故事。

- 快速开始：本地 dev runner、hello task、simple_rag、actor counter、RAG LLM。
- 架构文档：SDK、control、worker、logd、metadata view、shared log 的关系。
- 语义文档：log-first、source of truth、exactly-once-ish、idempotency、actor mailbox、epoch fencing。
- 运维文档：如何运行实验脚本、如何看 reports、如何启动 Docker Compose、如何配置 MinIO/PostgreSQL。
- 限制说明：单机实验边界、mock LLM 边界、log retention 目前是 logical trim。

测试方面，优先保证核心语义不回退。

- log-first failure tests：append 失败不能产生 metadata-only 状态。
- workflow recovery tests：worker kill 后不重跑已成功 step。
- actor ordering tests：并发 inc 后结果正确，stale epoch completion 被拒绝。
- idempotency tests：同 key 同 payload 返回旧对象，不同 payload 报 conflict。
- LLM scheduling tests：resource-only、locality-aware、predicted-latency 的选择行为。
- checkpoint cache tests：cold fetch、warm hit、manifest restart scan、eviction。
- replay compatibility tests：保存样例日志，确保新版本 reducer 能 replay。

CI 方面，我会做成几层。

第一层每次提交都跑：

```text
go test ./...
go vet ./...
python -m unittest discover sdk/python/tests
python -m compileall -q sdk/python/logserve scripts examples
```

第二层在 Linux runner 上跑集成测试和 race：

```text
go test -race ./internal/control ./internal/worker
go test ./tests/integration
```

第三层定时跑 benchmark，不把性能波动当成普通 PR 阻塞，但保存结果趋势。比如 nightly 跑 logstore benchmark、workflow latency、actor recovery、LLM locality ablation。

第四层跑文档和脚本检查。PowerShell 脚本、bash 脚本、Markdown 链接、示例命令至少要有 smoke validation。

如果要让别人愿意看这个项目，README 第一屏要能说明“这是什么、为什么有意思、怎么 5 分钟跑起来”。然后用 docs/report.md 和 interview_qna 这类材料解释细节。开源项目最怕只有设计，没有可复现路径。
