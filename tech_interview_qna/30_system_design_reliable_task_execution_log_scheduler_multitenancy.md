# 30. 系统设计综合：可靠任务执行、日志系统、调度器与多租户平台

这一章把前面很多单点问题串起来：可靠任务执行、append-only log、异步重试、workflow、actor、LLM serving scheduler、大结果存储、metadata replay、状态机、控制面和数据面拆分。面试里这类综合题最容易答成“堆组件”：Kafka、Temporal、Redis、PostgreSQL、Kubernetes、对象存储。组件名可以讲，但更重要的是把语义说清楚：什么东西是事实来源，什么东西只是视图；worker 可以至少执行一次，但结果提交要幂等；控制面可以重启，但队列不能只活在内存里；多租户可以共享平台，但调度、限流、隔离和审计必须带 tenant 维度。

结合 LogServe 时，我会守住项目边界。LogServe 当前更像一个单机/多进程的机制验证系统：shared log 是主要事实来源，metadata 是可重建视图，worker 执行任务、workflow step、actor command 和 LLM request，控制面用 log-first 路径、lease、epoch fencing、result reference 和 replay 来验证恢复语义。它不是完整生产级分布式平台。答系统设计题时，可以把它作为机制样例，但不要把实验结论扩大成多节点生产能力。

## Q001. 如何设计一个可靠任务执行系统？

**回答：**

我会先把“可靠”拆开讲。可靠任务执行不是说任务一定只执行一次，也不是说 worker 崩了没有任何影响。更实际的目标是：提交不丢，状态可查，失败可重试，重复执行不会破坏结果，控制面重启后能恢复，最终能判断任务到底成功、失败、取消还是进入人工处理。

一个可靠任务系统通常有这几层：

1. 提交层。客户端提交任务时要带 `task_id` 或 `idempotency_key`，平台把 payload、租户、优先级、deadline、重试策略、结果大小限制写成任务规格。重复提交同一个 key 时，如果 payload 一样，返回同一个任务；如果 payload 不一样，直接报冲突。这个设计比“客户端自己不要重复点按钮”可靠得多。
2. 持久化层。任务进入系统后，必须先落到可靠存储，再让 worker 看见。可以是数据库表，也可以是 append-only log，再投影成 metadata view。不能只放进内存队列，否则控制面一重启，任务就消失了。
3. 调度层。调度器从可运行任务里选择 worker。选择依据包括任务类型、租户配额、优先级、worker 能力、当前负载、数据局部性、模型缓存、deadline 和 backpressure。多租户场景下不能只按 FIFO，否则一个大租户可以把平台占满。
4. 执行层。worker 通过 poll 或 push 接收任务，拿到的是带 lease 的执行权。任务执行可以超时，可以心跳续租，也可以因为 worker 崩溃被重新投递。
5. 完成层。worker 完成后不是随便改状态，而是用 `task_id + attempt + lease_epoch` 这类 token 做条件提交。控制面只接受当前有效 lease 对应的完成事件，旧 worker 或重复完成会被拒绝或返回已有结果。
6. 结果层。小结果可以直接放 metadata，大结果应该先写对象存储，再在完成事件里写 `result_ref`、checksum、size、content type 和可选的版本信息。日志和 metadata 里不要塞大对象。
7. 恢复层。控制面启动时从事实来源重建状态：哪些任务已经成功，哪些失败，哪些 running 但 lease 过期，哪些需要重新入队，哪些超过最大重试次数要进 DLQ 或人工队列。

这里的关键语义一般是 at-least-once execution，加 exactly-once-ish result commit。worker 可能执行一次以上，但系统要保证同一个任务的最终状态只被合法地提交一次。严格的 distributed exactly-once 在真实系统里很难，尤其任务有外部副作用时更难。面试里如果直接说“我保证 exactly once”，通常会被追问到支付、发邮件、写外部数据库这些副作用场景，然后很容易站不住。

状态机要先定好。最小形态可以是：

```text
SUBMITTED -> QUEUED -> LEASED/RUNNING -> SUCCEEDED
                                  \-> FAILED -> RETRY_WAIT -> QUEUED
                                  \-> CANCELED
                                  \-> DEAD_LETTER
```

每条边都要有触发条件。`QUEUED -> RUNNING` 必须产生 lease；`RUNNING -> SUCCEEDED` 必须校验 lease；`FAILED -> QUEUED` 必须检查 `attempt < max_attempts`；终态不能被普通 worker 改写。状态机看起来是文档问题，其实是防并发 bug 的核心。

结合 LogServe，我会这样说：LogServe 采用 log-first 控制面路径，先写 `TaskSubmitted`、`TaskStarted`、`TaskCompleted` 这类事件，再更新 materialized metadata view。worker 通过 poll 获取任务，控制面用 lease epoch 拒绝 stale completion。当前实现验证的是单机/多进程下的机制：worker kill recovery、queue redelivery、control restart probe 都是围绕“任务不靠内存队列作为事实来源”来做的。

多租户设计还要补几个字段：每个任务都带 `tenant_id`，调度队列按租户做 fair share 或 weighted fair queue；租户有并发上限、QPS 上限、存储配额和结果保留策略；日志、对象存储 key、metadata 查询都要带租户隔离；审计日志要能查出某个租户提交了什么、谁执行了、失败原因是什么。否则任务平台能跑起来，但很难安全地共享给多个团队。

面试里可以这样收束：

```text
我会把可靠任务系统设计成“持久任务记录 + 租约执行 + 幂等完成 + 可重建视图”。任务提交先落事实来源，worker 只拿到带 lease 的执行权，完成回写必须校验 attempt 和 lease epoch。worker 可以至少一次执行，但最终状态提交要幂等；控制面重启后从日志或数据库恢复 queued/running/terminal 状态；大结果写对象存储，日志只放引用。多租户再加 tenant_id、配额、公平调度和审计。
```

## Q002. 如何设计一个可恢复的 append-only log service？

**回答：**

append-only log 的核心不是“只追加文件”这么简单，而是要回答几个问题：一条记录写到一半崩溃怎么办；重启后怎么知道哪些记录有效；读者如何按 stream 和 offset 读取；重复 append 如何处理；日志怎么分段、建索引、截断和压缩；fsync 策略如何在吞吐和持久性之间取舍。

我会先设计 record 格式。每条记录至少包含：

```text
magic/version
record_length
stream_id
offset 或 sequence
append_time
idempotency_key 或 request_id
payload_length
payload_crc
header_crc 或 full_record_crc
payload
```

`record_length` 让恢复扫描知道下一条记录在哪里；`magic/version` 用来识别格式；checksum 用来判断尾部是否写坏；`stream_id + offset` 支持按流读取；`idempotency_key` 支持客户端超时后重试 append。真实系统还会记录 segment id、physical position、schema version、compression flag 和 trace id。

写入路径一般是：先序列化 record，追加到当前 segment，更新内存索引，再按策略 fsync。fsync 策略可以有三类：

1. `always`：每条或每批请求都 fsync，持久性最直接，吞吐最低。
2. `batch`：多个 append 合并 fsync，延迟略高，吞吐更好。
3. `interval`：按时间间隔刷盘，吞吐好，但崩溃时可能丢最近一小段已经返回或未返回的记录，语义要说清楚。

恢复路径要非常保守。服务启动时从 segment 开头顺序扫描，逐条验证 `length`、`magic`、checksum 和边界。如果遇到半条记录、checksum 不匹配、长度越界，就截断到上一条有效记录的末尾。不要试图“猜”坏记录后面还有没有好记录。append-only log 的恢复要宁可丢弃尾部不确定数据，也不要把坏数据当成事实。

索引不能作为事实来源。常见做法是 segment 文件保存 record 本体，索引保存 `stream_id -> offset -> physical position`，也可以保存每个 stream 的 latest offset。索引损坏时可以从 segment 重建。这样恢复慢一点，但系统语义干净。LogServe 的 shared log 就是这个方向：segment append-only log 加 rebuilt-on-start index，读路径通过 index 找到 record payload，而不是把所有 record body 放在内存里。

idempotent append 很重要。客户端 append 成功后网络断开，它不知道服务端是否已经写入。如果它用同一个 `idempotency_key` 重试，log service 应该返回已写入的位置，而不是写第二条内容不同的记录。如果 key 相同但 payload 不同，应返回冲突。否则上层任务提交、workflow event、actor command 都会因为客户端重试产生重复事实。

多 stream 设计要注意顺序边界。一个全局 LSN 可以表示整个 log 的物理顺序，`stream_id + offset` 表示某个对象自己的逻辑顺序。workflow、actor、task 通常更关心 per-stream 顺序；跨 stream 的全局顺序如果要作为语义依据，就要明确是否真的需要。很多系统不需要在所有 stream 之间建立强顺序，因为代价很高。

生产级 append-only log 还要补复制。单机 log 可以靠 fsync 和恢复扫描保证单节点 crash consistency，但机器丢了磁盘就没了。多副本日志通常要 leader、quorum、ISR 或 Raft/Paxos 这类复制协议，写入只有在达到提交条件后才算 committed。这里不能把“append-only 文件”讲成“天然高可用”。LogServe 当前的 logd 更适合说明单节点可恢复日志和控制面 replay，不等于 Kafka 或 Raft log 的完整复制能力。

面试里可以这样答：

```text
我会把 append-only log 做成 segment 文件 + 可重建索引 + 顺序恢复扫描。每条 record 带长度、版本、stream、offset、idempotency key 和 checksum；写入只追加，按 always/batch/interval 选择 fsync；启动时从头扫描，遇到半条或 checksum 错误就截断到最后一条有效记录。索引只是加速结构，坏了可以重建。客户端重试 append 要通过 idempotency key 返回同一条记录，不能写出第二套事实。单机恢复和多副本高可用要分开讲。
```

## Q003. 如何设计一个支持重试和幂等的异步任务平台？

**回答：**

异步任务平台里，重试和幂等必须一起设计。只设计重试，系统会把临时故障放大成重复副作用；只设计幂等，任务失败后又没有明确的重新调度策略。比较稳的思路是：平台承认任务可能被重复执行，但要求提交、调度、完成和外部副作用都能用 key 或 token 去重。

提交阶段先做幂等。客户端传 `idempotency_key`，平台保存 key、payload hash、task_id、创建时间、租户和结果。如果同一个 key 再次提交同一 payload，返回原 task；如果 payload hash 不同，返回 `409 conflict`。这样可以解决客户端超时、网关重试、浏览器重复提交等问题。

执行阶段用 attempt。每次调度生成一个 `attempt_id` 或 `attempt_number`，并绑定 lease。任务失败后，不是把同一个 running 记录随便改回 queued，而是记录一次 attempt 结果，再根据策略生成下一次 attempt。这样排查问题时能看到第几次失败、失败原因、运行时长、worker、退出码和日志位置。

重试策略要分错误类型：

1. 临时错误：网络抖动、依赖服务 503、对象存储短暂不可用，可以指数退避加 jitter。
2. 资源错误：排队超时、worker 被杀、lease 过期，可以快速 redelivery，但要限制频率。
3. 业务错误：参数不合法、权限不足、模型不存在，不应该盲目重试。
4. 不确定错误：worker 崩溃在外部副作用之后，平台无法知道副作用是否发生，这时要靠业务幂等键或补偿流程。

幂等完成也要单独设计。worker 完成时带 `task_id + attempt_id + lease_epoch + result_ref/result_hash`。控制面只接受当前 attempt 的完成。重复提交同一完成请求时返回已有结果；旧 attempt 的完成请求到达时，如果任务已经终态，就拒绝或返回 terminal 状态。这样能处理“完成写回成功但 worker 没收到响应”的网络超时。

外部副作用是最难的部分。比如任务要发邮件、扣款、调用第三方 API、写外部数据库。平台自身的幂等只能保护平台状态，不能自动保护外部系统。工程上通常要把 idempotency key 传给外部系统；外部系统不支持时，使用 outbox/inbox、业务唯一键、去重表、补偿任务或人工审核。面试里要承认这件事，不要说“平台有 exactly-once，所以外部副作用也 exactly-once”。

重试还要有上限。`max_attempts`、最大运行时间、最大排队时间、最大总耗时、DLQ、人工队列都要有。无限重试会吞掉资源，也会把坏任务反复打到依赖系统上。多租户平台还要按 tenant 统计重试消耗，不能让一个租户的失败任务一直消耗公共 worker。

结合 LogServe，可以说它的语义是 exactly-once-ish：worker 至少一次执行，控制面对 workflow step 用 `workflow_id + step_id + input_hash` 去重最终 step result；普通 task 和 actor command 用 lease epoch、command sequence 和 idempotent event key 控制最终提交。这个口径比“严格 exactly-once”更准确。

面试里可以这样答：

```text
我会把异步任务平台设计成 at-least-once execution，但让提交和完成都幂等。提交用 idempotency key 绑定 payload hash；每次调度生成 attempt 和 lease；失败按错误类型、backoff、jitter 和 max_attempts 重试；完成写回用 attempt_id 和 lease_epoch 做条件提交。外部副作用要靠业务幂等键、outbox 或补偿，平台不能凭空保证第三方系统 exactly once。
```

## Q004. 如何设计一个分布式 workflow engine？

**回答：**

workflow engine 解决的是长流程可靠执行。它不只是一个任务队列，因为 workflow 还要保存流程状态、step 依赖、定时器、重试、取消、补偿、事件历史和可恢复执行位置。一个任务失败可以单独重试；一个 workflow 失败要知道哪些 step 已经完成，哪些 step 可以继续，哪些 step 要跳过，哪些结果可以复用。

有两种常见建模方式。

第一种是 DAG workflow。用户定义 step 和依赖关系，控制面做拓扑调度。一个 step 完成后，依赖它的下游 step 如果依赖都满足，就进入 ready 队列。这个模型直观，适合批处理、ETL、RAG pipeline、机器学习流水线。LogServe 当前的 Python `@workflow` DSL 就更接近这个方向：SDK 把 `@task` 调用追踪成 DAG，Go 控制面调度 ready step，并把 step 结果替换到后续输入里。

第二种是 durable function / deterministic workflow。用户写看起来像普通代码的 workflow function，平台把外部调用、定时器、信号、活动结果记录到 event history。恢复时重新执行 workflow code，但遇到已经记录过的事件就从 history 取结果。Temporal 属于这个方向。这个模型表达力强，但要求 workflow code 确定性更高：不能随便在 workflow 逻辑里读当前时间、随机数、网络结果或非确定性全局状态，除非这些东西被平台事件化。

无论哪种模型，核心都离不开 event history。workflow 的 source of truth 应该是一串事件，例如：

```text
WorkflowSubmitted
StepScheduled
StepStarted
StepSucceeded
StepFailed
TimerFired
WorkflowCompleted
WorkflowFailed
WorkflowCanceled
```

metadata view 可以保存当前状态和查询索引，但不能成为唯一历史。否则控制面重启后只知道 workflow 现在是 running，却不知道哪些 step 已经完成、哪些结果引用还有效、下一步该调度谁。

调度逻辑要围绕“可恢复”写。控制面把 ready step 提交给任务平台，任务平台用 lease 交给 worker。step 成功后，worker 把结果写回；如果结果很大，先写对象存储，再写 `result_ref`。控制面在接受完成事件时更新 workflow state，判断下游 step 是否 ready。如果 worker 在 step 执行后崩溃，step 可以 redelivery；如果同一 step 出现重复完成，控制面用 `workflow_id + step_id + input_hash` 或 step attempt fencing 去重。

重试策略应该分层。workflow 自身可以有整体超时和取消；step/activity 可以有 `max_attempts`、backoff、timeout、heartbeat；外部副作用还要自己幂等。不要把所有失败都归结为 workflow failed。很多时候一个 activity 失败三次之后才让 workflow 失败；有些失败触发补偿 step；有些失败进入人工等待。

版本升级也要考虑。长 workflow 可能运行几天甚至几个月，代码发布后，老 workflow 的 replay 不能被新代码破坏。常见办法是 workflow definition version、step schema version、event schema evolution、feature flag 或显式的 workflow patch/version marker。DAG 模型也一样，已经提交的 workflow 应该绑定当时的 graph，不应被后续代码随意改形状。

结合 LogServe，可以这样讲边界：它实现了 DAG scheduling、ready step、retry、timeout、result ref、replay validation 和 exactly-once-ish step result dedup。它没有实现完整 Temporal 那种多年运行、信号、定时器、复杂版本 marker 和多节点历史服务。面试里把这个边界说出来，反而更可信。

面试里可以这样答：

```text
我会把 workflow engine 建在持久 event history 上。用户可以用 DAG 或 deterministic workflow function 表达流程；控制面只调度依赖已满足的 step；step 执行走任务平台的 lease 和重试；step 完成后写事件并更新 materialized view；大结果写对象存储，事件里放 result_ref。恢复时从 workflow event history replay 当前状态。worker 可以重复执行 step，但最终 step result 用 workflow_id、step_id 和 input_hash 去重。长流程还要考虑取消、超时、补偿和版本升级。
```

## Q005. 如何设计一个有状态 actor service？

**回答：**

actor service 的核心是把状态和消息顺序绑定到一个 actor id 上。每个 actor 有自己的状态，外部通过 message 或 method call 访问它，同一个 actor 内部按顺序处理消息。这样调用者不用直接拿锁改共享状态，平台保证同一个 actor 的命令串行化。

设计时先定义 actor identity。`actor_id` 必须稳定，不能跟 worker 进程绑定。worker 可以挂，可以迁移，可以扩缩容，但 `actor_id` 的状态要能从事实来源恢复。actor 的状态来源通常有两种：一种是每次 command applied 后保存最新状态；另一种是 append command/event，再通过 replay 恢复。为了恢复效率，通常会加 snapshot。

mailbox 是第二个关键点。actor 的并发不是“所有请求都排一个全局队列”，而是每个 actor 有自己的逻辑队列。不同 actor 可以并发执行，同一个 actor 的 command 要按顺序应用。常见记录是：

```text
ActorCreated
ActorOwnershipGranted
ActorCommandSubmitted(command_seq)
ActorCommandApplied(command_seq)
ActorSnapshotCreated(snapshot_ref)
```

`command_seq` 很重要。控制面给同一 actor 的每条 command 分配单调递增序号，只有 `command_seq == actor.command_count + 1` 的完成才能应用。这样可以防止第 5 条命令先于第 4 条命令进入状态。worker 本地锁也有用，但不能替代控制面的序号校验，因为 worker 崩溃、迁移、重复投递时，本地锁会消失。

ownership 和 fencing 也必须有。某个 actor 在某一时刻由一个 worker 持有，任务派发给 owner worker。owner 失去心跳后，控制面可以把 actor 转给另一个 worker，并提升 epoch。旧 worker 如果后来恢复，带旧 epoch 的完成请求必须被拒绝。没有 epoch fencing，就会出现两个 worker 都以为自己是 owner，并同时写 actor 状态。

snapshot 是为了控制 replay 成本。纯事件 replay 简单，但一个 actor 处理 100 万条 command 后，恢复时从头重放会太慢。平台可以定期写 `ActorSnapshotCreated(snapshot_ref, command_count)`，snapshot 放对象存储，日志里只放引用。恢复时先读最近 snapshot，再回放 snapshot 之后的 command。LogServe 的 actor snapshot replay 把恢复工作从 full replay 的多条 command 降到 snapshot 后的 tail log，这就是机制价值。

actor 的扩缩容问题要单独看。actor 数量很多时，可以按 consistent hashing、range 分片、目录服务或 placement table 分配到 worker；热点 actor 仍然只能串行处理，不能靠简单加副本解决。需要拆 actor、拆 shard、把只读查询走 snapshot/read model，或者把热点操作改成可交换、可聚合的命令。actor 模型解决状态封装，不自动解决所有吞吐问题。

结合 LogServe，可以说它支持 actor 创建、ownership、mailbox 串行化、`command_seq`、snapshot replay、logical trim 和 epoch fencing。边界是：当前主要验证单机/多进程恢复语义，不是完整的 Orleans/Akka 集群运行时，也没有把 placement、跨节点迁移、持久目录和生产级复制全部做完。

面试里可以这样答：

```text
我会把 actor service 设计成 actor_id 维度的有序命令流。控制面给每个 actor command 分配 command_seq，同一 actor 只允许下一条序号被执行和应用；worker 通过 ownership 拿到执行权，ownership 带 epoch，旧 owner 的完成会被 fencing 拒绝。actor 状态从 append-only command/applied log 加 snapshot 恢复，metadata 只是当前视图。不同 actor 可以并发，同一个 actor 串行；热点 actor 要拆分或改模型，不能指望多副本并发写同一份状态。
```
## Q006. 如何设计一个模型缓存感知的 LLM serving scheduler？

**回答：**

LLM serving scheduler 和普通任务调度器的区别在于：一个请求的成本不只取决于“哪个 worker 空闲”。模型是否已经加载、checkpoint 是否在本地、KV cache 或 prefix cache 是否命中、GPU 显存是否够、当前 prefill/decode 队列有多长，都会影响延迟和吞吐。模型缓存感知调度的目标是减少冷启动和重复加载，同时不能把所有请求都压到同一个缓存命中的 worker 上。

我会先定义 worker 上报的信息。worker 注册和心跳里带这些字段：

```text
worker_id
tenant/service labels
supported adapters
GPU/CPU/memory capacity
current running requests
local model cache entries: model_name, version, size, last_used, loaded/runnable
checkpoint cache entries: path/ref, size, checksum
KV/prefix cache summary if available
recent latency stats by model/version
queue wait and saturation
```

模型也要有 registry。registry 记录 `model_name`、`version`、adapter、checkpoint source、大小、最低资源需求、冷加载估算、租户授权和是否允许共享缓存。调度器不能只看名字，因为 `model-A:v1` 和 `model-A:v2` 是不同缓存对象；量化版本、LoRA adapter、tokenizer 版本不一致，也可能导致缓存不能复用。

最简单的策略是 resource-only：谁空闲就给谁。这在模型很小、请求很均匀时够用，但 LLM 场景里会导致频繁 cold start。更好的策略是 locality-aware：优先选择已经有目标模型或 checkpoint 的 worker，同时把当前队列长度、可用容量、租户配额和 deadline 算进去。再往前一步是 predicted-latency：为每个 `(model_name, version, worker_id)` 维护 EWMA 延迟、cache-hit rate、checkpoint fetch time、model load time，调度时估算：

```text
predicted_latency = ewma_total_latency + queue_penalty + cold_start_penalty + eviction_penalty
```

这个公式不要神化。它只是一个工程启发式，真正的效果要靠指标和 ablation 验证。LogServe 里 locality-aware 和 predicted-latency 都是基于 materialized stats 做 `O(number_of_workers)` 查询，不在热路径扫描所有 `llm:*` event stream。这个点很重要：调度器如果每次调度都重放历史日志，缓存感知会变成性能问题本身。

KV cache 和 prefix cache要单独看。模型权重缓存解决的是“模型是否已经加载”；prefix/KV cache 解决的是“请求前缀是否能复用”。长文档问答、多轮对话、共享系统提示词会从 prefix cache 受益；如果请求主要时间花在长输出 decode 上，或者请求之间没有共享前缀，prefix cache 就帮不上太多。调度器可以把会话、文档 id、prompt prefix hash 作为 locality signal，但不能为了命中 KV cache 牺牲租户公平性和尾延迟。

多租户问题很现实。共享模型缓存能提高资源利用率，但也会引入隔离和计费问题：A 租户预热了模型，B 租户是否可以复用；缓存命中是否泄露 workload 形状；高优先级租户是否可以抢占低优先级租户；一个租户的大模型是否可以把公共显存占满。平台需要 tenant quota、per-tenant concurrency、模型授权、cache namespace、eviction policy 和审计。

还要避免缓存抖动。比如三个大模型轮流请求，LRU 可能反复驱逐、反复加载。调度器可以用 pinned model、warm pool、最小驻留时间、cost-aware eviction、请求批量合并和 admission control 来缓解。对冷启动很贵的模型，直接拒绝或排队有时比马上抢占更好。

结合 LogServe，回答时可以说：系统实现了 model registry、worker model-cache 上报、file-backed checkpoint cache、mock/vLLM adapter、`RESOURCE_ONLY`、`LOCALITY_AWARE`、`PREDICTED_LATENCY` 三种策略。实验结果来自单机 Ubuntu、3 worker、mock LLM 和 file-backed checkpoint cache，能说明机制有效，但不能推出真实 GPU 集群下的生产性能。

面试里可以这样答：

```text
我会让 worker 心跳上报模型缓存、checkpoint cache、资源容量、队列长度和按模型聚合的延迟统计。调度器先检查模型版本和租户授权，再在候选 worker 上估算 cache hit、cold start、queue wait、显存和公平性成本。普通请求可以 resource-only，LLM 请求更适合 locality-aware 或 predicted-latency。KV/prefix cache 可以作为额外 locality 信号，但不能压过租户隔离、配额和尾延迟控制。
```

## Q007. 如何设计一个大结果存储和引用系统？

**回答：**

大结果不要直接写进任务日志或 metadata。原因很直接：日志是为了顺序写、恢复和 replay；metadata 是为了快速查询当前状态。把几十 MB、几百 MB 的结果塞进去，会让 replay 变慢、索引变大、备份变贵，也会让控制面 API 返回不可控的大 payload。

我会把结果拆成两层：对象存储保存 bytes，控制面保存引用。对象存储可以是本地文件、S3、MinIO、GCS、Azure Blob 或内部 blob store；引用一般长这样：

```text
result_ref: local://workflow/<workflow_id>/step/<step_id>/result
object_version or etag
size_bytes
content_type
checksum
created_at
tenant_id
producer_task_id
schema_version
expires_at or retention_class
```

写入流程要注意原子性。worker 执行成功后，先把结果写到对象存储，拿到 checksum 和对象版本，再把 `result_ref` 写进完成事件。完成事件一旦提交，控制面就认为这个结果可见。如果对象写成功但完成事件失败，worker 可以用同一个 attempt 重试完成；如果完成事件成功但 worker 没收到响应，重试完成应返回已有结果。这样对象存储里多一个孤儿对象可以后续 GC，不能因为怕孤儿对象就先提交完成再慢慢写结果。

结果引用要幂等。对象 key 可以包含 `task_id/attempt_id/result_hash`，也可以用内容寻址 hash。相同 attempt 重试写同一结果时，应该得到同一个 ref；如果同一个幂等 key 对应不同 bytes，要报冲突或生成新版本，不能静默覆盖。对象存储如果提供 etag/version id，也应该记录下来，避免后续读取到被覆盖的内容。

读取路径也要分层。控制面 `GetTask` 或 `GetWorkflow` 返回状态和 `result_ref`，真正下载结果走 result service 或对象存储签名 URL。这样可以做权限校验、限速、审计、范围读取和过期控制。多租户场景下，`tenant_id` 必须进入对象 key 或 bucket policy，不能只靠 UI 隐藏 ref。

生命周期管理不能省。任务完成后多久保留结果；失败任务的 partial result 要不要保留；workflow 删除后 step result 是否一起删除；actor snapshot 和普通 task result 是否同一套保留策略；用户是否可以 legal hold；对象存储 GC 如何防止删掉仍被 workflow event 引用的对象。这些都要由 metadata 和 log 共同决定。一个常见做法是先标记引用不可达，再延迟删除，避免和重放、恢复、读请求并发冲突。

大结果还会影响性能。小结果内联可以减少一次对象存储 round trip，但内联阈值要小而明确，比如几 KB 或几十 KB。大结果上传可以分片、多部分上传、压缩或流式写入。结果消费者如果只要前几行，最好支持 range read 或预览摘要，不要每次把整个对象拉回控制面。

结合 LogServe，workflow step result 和 actor snapshot 都通过 result-store interface 写引用。默认 local adapter 用 `local://`，Compose 边界可以接 MinIO/S3-compatible store。这个设计让 shared log 保存 `result_ref`，而不是保存大 payload，本质上是把“状态事实”和“大字节对象”分开。

面试里可以这样答：

```text
我会让大结果走对象存储，控制面和日志只保存 result_ref、size、checksum、content_type、版本和租户信息。worker 先写对象，再用同一个 attempt 幂等提交完成事件；完成提交成功后，客户端通过 ref 或签名 URL 读取。生命周期由引用关系、租户保留策略和延迟 GC 管理。这样 replay 和 metadata 查询不会被大 payload 拖垮，也能做权限、审计和限速。
```

## Q008. 如何设计一个支持 replay 的 metadata view？

**回答：**

支持 replay 的 metadata view，重点是承认 metadata 不是手工维护的一堆当前值，而是从事实来源投影出来的读模型。事实来源可以是 append-only log、event store、WAL 或数据库变更流。metadata view 用来让 API、调度器、dashboard 快速查询“现在是什么”。如果 view 丢了或落后，应该能从事件重新构造。

设计上先定义事件。事件要表达已经发生的事实，而不是模糊的命令。比如 `TaskSubmitted`、`TaskStarted`、`TaskCompleted` 比 `UpdateTaskStatus` 好，因为前者可以推导状态变化和审计上下文。事件至少带对象 id、事件类型、事件版本、发生时间、幂等 key、租户、producer 和必要 payload。

然后定义 reducer。reducer 是从事件到 view 的纯逻辑：读到 `TaskSubmitted` 创建 task 当前状态；读到 `TaskStarted` 设置 worker、attempt、lease；读到 `TaskCompleted` 进入终态；读到 `WorkerHeartbeat` 更新 worker load；读到 `ActorSnapshotCreated` 更新 actor snapshot ref。reducer 要尽量幂等，同一事件重复投影不能产生两条任务或两次计数。

replay 有全量和增量两种。全量 replay 从头扫描事件，重建所有 view；增量 replay 从上次 checkpoint/high watermark 后继续。为了加快启动，可以给投影器保存 `projection_checkpoint`，也可以给大对象使用 snapshot。需要注意 checkpoint 本身不是事实来源，它只是重放进度。checkpoint 坏了，大不了从更早位置重放。

顺序边界必须讲清楚。按单个 task stream、workflow stream、actor stream 重放时，per-stream 顺序通常足够。跨 stream 如果有因果关系，要通过事件里的引用建立。例如 workflow step 成功事件引用 task completion；actor command applied 引用 command submitted。不要假设所有 stream 的物理扫描顺序都能表达业务因果，除非 log service 明确提供全局提交顺序，并且上层愿意承担这个约束。

schema evolution 也要考虑。view 支持 replay 意味着老事件可能保存很多年，新代码要能读老版本事件。常见做法是事件带 `schema_version`，reducer 对老版本做兼容，或者在 replay 前做 event upcaster。不能随便改字段含义，否则历史重放会生成和当年不一样的 view。

replay 结果要能校验。可以维护对象数量、终态数量、last offset、checksum、projection lag，也可以提供 `ReplayWorkflow(id)`、`ReplayActor(id)` 这类按对象重放接口，和 metadata 当前状态对比。LogServe 的 dashboard snapshot、workflow replay validation、actor replay 都是这个思路：不是只相信内存状态，而是能从 shared log 再算一遍。

多租户视角下，metadata view 要带 tenant 索引，replay 时也要保留租户边界。一次全量 replay 可能很重，不能因为重建一个租户的 view 就扫爆整个平台。可以按 tenant 分区、按 stream 前缀分片、按 projection 类型拆分，或者为大租户单独调度 replay 资源。

面试里可以这样答：

```text
我会把 metadata view 定义成从 append-only events 投影出来的读模型。事件是 source of truth，view 保存当前状态和查询索引；reducer 按事件类型幂等更新 view；启动时可以从头 replay，也可以从 projection checkpoint 增量追赶。view 丢了可以重建，checkpoint 只是优化。设计时要处理事件版本、per-stream 顺序、投影滞后、重复事件和 replay 校验。
```

## Q009. 如何设计任务状态机以避免非法转换？

**回答：**

任务状态机要先写成显式模型，而不是散落在代码里的 `if status == ...`。很多任务系统的问题不是状态太少，而是状态边界不清：已经成功的任务还能被失败覆盖；旧 worker 还能完成新 attempt；取消和完成同时发生时结果随机；控制面重启后 running 状态不知道该怎么处理。

一个可用的任务状态机可以从这些状态开始：

```text
SUBMITTED
QUEUED
LEASED 或 RUNNING
SUCCEEDED
FAILED
RETRY_WAIT
CANCEL_REQUESTED
CANCELED
DEAD_LETTER
```

这里不一定每个系统都需要全部状态。关键是每个状态都要有允许的进入和退出条件。例如：

```text
SUBMITTED -> QUEUED: 持久化任务后进入调度队列
QUEUED -> RUNNING: worker poll 成功，生成 lease/attempt
RUNNING -> SUCCEEDED: 当前 lease 的 worker 完成成功
RUNNING -> FAILED: 当前 lease 的 worker 返回可记录失败
RUNNING -> QUEUED: lease 过期并允许 redelivery
FAILED -> RETRY_WAIT: 还有剩余 attempt
RETRY_WAIT -> QUEUED: backoff 到期
任意非终态 -> CANCELED: 取消请求被接受并完成清理
SUCCEEDED/FAILED/CANCELED/DEAD_LETTER: 终态，不允许普通 worker 改写
```

防非法转换要靠条件更新。数据库里可以用 `UPDATE ... WHERE task_id=? AND status=? AND lease_epoch=? AND version=?`；日志系统里可以通过 append 事件时检查当前 projection version 或 stream expected offset；内存里也要有同样的 CAS 语义。状态机不能只靠前端按钮置灰。

lease epoch 是防旧 worker 的关键。假设 worker A 拿到 task，执行很慢，lease 过期后 task 被 redelivery 给 worker B。B 完成并提交成功。A 后来恢复，也提交成功。如果状态机只检查 `task_id`，A 可能覆盖 B。正确做法是完成请求必须带 `attempt_id` 和 `lease_epoch`，控制面只接受当前 running attempt 的完成。A 的 completion 会被识别为 stale。

重试不要复用同一个 attempt。一次 attempt 是一次执行尝试，有自己的 worker、开始时间、deadline、stderr、退出码、失败原因和 result/ref。如果失败后直接把 running 状态改回 queued，不记录 attempt，排查问题会很困难，也很难判断是否超过最大重试次数。

取消也要有语义。取消已经 queued 的任务可以直接进入 canceled；取消 running 的任务通常先进入 cancel requested，控制面通知 worker，worker 尽力停止。worker 不响应时，lease 到期后不再 redelivery，最终进入 canceled 或 failed。已经 succeeded 的任务一般不能再取消，只能做业务补偿。

状态机还要配测试。至少要测：终态不可覆盖；旧 lease completion 被拒绝；重复 completion 幂等；取消和完成并发时结果确定；control restart 后 running expired 会重新入队；max attempts 后进入 dead letter。LogServe 的 stale task completion rejected by task lease epoch 就是典型测试点。

面试里可以这样答：

```text
我会把任务状态机显式建模，并让所有状态更新走条件提交。worker 从 QUEUED 拿任务时生成 attempt 和 lease_epoch；完成时必须匹配当前 attempt 和 lease；终态不可覆盖；失败记录 attempt，再按 retry policy 进入 RETRY_WAIT 或 DEAD_LETTER；取消和 redelivery 也有明确边界。状态机不是文档装饰，它要落实到 CAS、expected version、expected stream offset 和测试用例里。
```

## Q010. 如何定义系统的 source of truth？

**回答：**

source of truth 是系统承认的权威事实来源。它不是“哪个存储最贵”或“哪个数据库最稳定”，而是冲突发生时系统相信谁。如果日志显示任务完成，但 metadata 表显示 running，系统应该听谁的？如果对象存储里有结果，但任务事件里没有完成，用户能不能看到？如果 worker 内存里说自己还拥有 actor，但控制面 epoch 已经提升，它还能不能写状态？这些问题的答案，就是 source of truth 的定义。

我会按对象分别定义。一个系统里不一定只有一个 truth：

1. 任务生命周期：可能以 append-only task events 为 truth，metadata 是 view。
2. workflow 状态：可能以 workflow event history 为 truth，step 表是 view。
3. actor 状态：可能以 actor command/applied log + snapshot 为 truth，当前 actor 表是 view。
4. 大结果 bytes：对象存储里的 immutable object 是 bytes truth，任务完成事件里的 `result_ref` 是可见性 truth。
5. worker 活性：心跳/lease 表是当前活性 truth，但它只是时间窗口内的判断，不是永恒事实。
6. 租户配额和权限：通常以配置数据库或 control-plane config store 为 truth，缓存只是加速。

定义 truth 后，要规定写入顺序。LogServe 的口径是 log-first：先写 shared log 事件，再更新 metadata view。这样 metadata 更新失败时，可以从 log replay 修复。如果反过来先改 metadata，再写 log，一旦写 log 失败，就出现一个无法从事件历史解释的状态。不是所有系统都必须 log-first；也可以选择数据库事务表作为 truth。但选择以后要一致。

truth 和 view 的区别要落到恢复流程。如果 view 丢了，可以重建；truth 丢了，只能从备份、复制副本或人工补偿恢复。如果某个字段只能存在 metadata view，不能从事件恢复，那这个字段其实也是 truth 的一部分，要么把它事件化，要么承认 metadata 是该字段的 truth。很多系统出事故，就是因为口头上说“日志是事实”，实际关键字段只写在数据库当前值里。

还要定义冲突处理。比如重复提交同一个 idempotency key，但 payload 不同，truth 应该记录第一次 payload，然后第二次返回冲突；同一 actor 的两个 command 都声称是 `command_seq=10`，expected offset 或唯一约束必须拒绝其中一个；两个 worker 同时完成任务，只有匹配当前 lease 的 completion 能变成 truth。

多租户平台里，source of truth 还要包含 tenant 边界。任务属于哪个 tenant、结果对象属于哪个 tenant、计费统计来自哪些事件、配额扣减以哪个事件为准，都要统一。否则 dashboard、账单、权限判断和恢复重放可能各自算出一套数字。

结合 LogServe，最准确的说法是：shared log 是机制验证中的主要事实来源，metadata store 是 materialized current view；PostgreSQL Compose 模式可以承载 metadata view，但如果表被删，control 重启后可以从 shared log bootstrap 重建。这是项目的优势，也是边界：它说明 replay 机制能工作，不说明已经实现多副本强一致 event store。

面试里可以这样答：

```text
我会先按对象定义 source of truth：任务和 workflow 的生命周期以事件日志为准，metadata 是可重建视图；大结果 bytes 以对象存储为准，但只有完成事件里的 result_ref 才让结果可见；worker 活性以 lease/heartbeat 当前窗口为准。定义后要固定写入顺序、恢复规则和冲突处理。如果某个字段无法从 truth 重建，就不能假装它只是 view。
```
## Q011. 如何处理控制面和数据面职责拆分？

**回答：**

控制面和数据面拆分，目的不是把代码目录拆成两个服务，而是把“决策和状态管理”和“实际执行与数据搬运”分开。拆清楚之后，系统才知道哪个组件可以水平扩展，哪个组件必须强一致，哪个路径可以降级，哪个路径不能绕过。

控制面通常负责这些事：

1. 接收任务、workflow、actor、LLM 请求。
2. 校验租户、权限、配额、payload 和幂等 key。
3. 写事实来源，比如 shared log 或事务数据库。
4. 维护 metadata view 和调度索引。
5. 做任务调度、lease、重试、取消、超时、backpressure。
6. 记录 worker 注册、心跳、能力、模型缓存、负载。
7. 暴露 status、dashboard、审计和管理 API。

数据面负责实际工作：worker 执行 Python 函数、跑 actor method、调用 LLM adapter、拉取 checkpoint、读写 result store、上传日志片段、执行本地资源隔离。数据面应该尽量无权决定全局事实。它可以报告“我完成了 attempt 3”，但能不能把任务置为成功，应由控制面根据 lease、状态机和幂等规则判断。

这个边界能防很多问题。比如 worker 本地执行成功后，不能直接把数据库状态改成 succeeded，否则两个 worker 重复执行时会抢写；应该走控制面的 `CompleteTask`，让控制面检查 lease epoch。再比如 LLM worker 可以上报自己缓存了 `model-A:v1`，但不能自己决定所有 `model-A` 请求都归它；控制面还要看租户配额、队列长度和其他 worker 状态。

拆分后还要设计通信协议。控制面到数据面可以是 worker poll，也可以是 push。poll 的好处是 worker 穿透网络简单，天然有 backpressure：worker 空了才来拿任务。push 的延迟更低，但控制面要处理连接状态和推送失败。LogServe 采用 worker polling，比较适合机制验证，也方便处理 worker kill 后的 redelivery。

控制面自己的高可用要谨慎。单实例控制面可以靠 log replay 恢复，但有停机窗口；多实例控制面要处理 leader election、分片调度、并发 lease、metadata store 事务和去重。不能简单把 control 副本数调到 3 就说高可用。Kubernetes 里的 leader election、Lease API 这类机制能解决“谁是当前 leader”，但业务状态写入仍然要靠自己的幂等和事务边界。

多租户平台里，控制面还承担隔离责任。数据面 worker 可以是共享池、租户专属池或混合池；控制面要决定哪个租户的任务进入哪个池，是否允许共享模型缓存，是否需要节点亲和，是否触发限流。数据面负责执行，不能绕过控制面的 tenant policy。

结合 LogServe，可以这样讲：`logd` 提供 shared log，`control` 做任务、workflow、actor、model 和 backpressure 的控制面，`worker` 做本地 executor pool、Python task、mock/vLLM 调用和 checkpoint cache。result store 属于数据承载边界，metadata view 属于控制面读模型。这个拆法清楚地表达了系统语义。

面试里可以这样答：

```text
我会让控制面负责准入、幂等、日志写入、metadata view、调度、lease、重试、worker 心跳和租户策略；数据面负责实际执行、模型加载、对象读写和本地资源隔离。worker 只能报告事件，不能绕过控制面直接改全局状态。控制面可以重启后从事实来源恢复，数据面可以横向扩展。多副本控制面要另做 leader election、并发控制和幂等写入，不能靠副本数自动获得正确性。
```

## Q012. 如何处理 worker 注册、心跳、失效和恢复？

**回答：**

worker 生命周期可以拆成四步：注册、心跳、失效检测、恢复/重新接管。它看起来像运维细节，其实直接影响任务是否重复执行、是否丢失、是否被错误地交给不具备能力的 worker。

注册时，worker 要告诉控制面自己是谁、能做什么、能承载多少。典型字段包括：

```text
worker_id
process_start_time 或 incarnation_id
supported task types
capacity / pool sizes
resource labels
current software version
model cache entries
checkpoint cache capacity
tenant or queue affinity
heartbeat interval
```

`worker_id` 不一定足够。一个旧进程崩溃后，新的进程可能复用同一个 worker id。为了区分进程代际，可以加 `incarnation_id`、`session_id` 或 registration epoch。否则旧连接、旧完成请求和新 worker 的状态容易混在一起。

心跳不是简单的“我还活着”。心跳可以携带当前 running 数、队列等待、资源使用、模型缓存、最近错误、draining 标志。控制面收到心跳后更新 worker view 和 lease deadline。心跳频率要在故障检测速度和控制面压力之间取舍。太慢，故障恢复延迟高；太快，大规模 worker 会打爆控制面。

失效检测通常基于 lease。控制面记录 `last_heartbeat_at` 或 `lease_expire_at`。如果超过 TTL 没有心跳，就把 worker 标记为 unavailable，并处理它持有的任务：未开始的可以重新入队；running 的要看任务 lease 是否过期；actor ownership 要提升 epoch；LLM 本地缓存状态要标记不可用。注意，心跳超时只说明控制面暂时看不到 worker，不证明 worker 进程已经死了。它可能还在运行，只是网络分区或 GC 卡顿。

恢复时要防旧 worker。假设 worker A 心跳超时，任务被派给 worker B。A 过一会儿恢复并提交完成。控制面必须用 task lease epoch、actor epoch 或 worker incarnation 拒绝 A 的旧完成。没有 fencing，失效检测越积极，重复写状态的风险越高。

worker 优雅下线也要设计。worker 进入 draining 后不再接新任务，继续完成已有任务；长任务可以续租或被取消；超过宽限期后控制面再 redelivery。Kubernetes 滚动发布、节点维护、进程升级都依赖这个路径。只靠 SIGKILL 测试 worker kill recovery 还不够，优雅关闭路径也要有。

多租户场景里，worker 可能属于某个租户、某类硬件或某个安全域。注册信息要表达这些边界。比如 GPU worker 只能执行 LLM 请求，某些租户只能跑在隔离 worker pool，不可信代码不能和高权限内部任务混跑。调度器选择 worker 时要用这些标签过滤。

结合 LogServe，worker 注册和心跳还包括模型缓存上报。LLM scheduler 根据 worker cache 和 materialized stats 做 locality-aware 或 predicted-latency 调度。actor ownership 通过 `owner_worker_id + epoch` 做 fencing。任务 lease epoch 用来拒绝 stale completion。这些都是 worker 生命周期和正确性绑定的例子。

面试里可以这样答：

```text
我会让 worker 注册时上报能力、容量、版本、租户标签和缓存状态；心跳刷新 lease，并携带负载和健康信息。控制面按 TTL 判断 worker 失效，把它持有的可恢复任务重新入队，把 actor ownership 提升 epoch。恢复或网络分区后的旧 worker 不能直接提交结果，必须通过 task lease epoch、actor epoch 或 worker incarnation fencing。优雅下线用 draining，故障下线用 lease 过期和 redelivery。
```

## Q013. 如何设计任务 lease 和 redelivery？

**回答：**

任务 lease 的作用是给 worker 一个有时间边界的执行权。它不是永久锁。worker 拿到任务后，在 lease 有效期内执行；如果 worker 成功完成，提交结果并释放任务；如果 worker 崩溃或失联，lease 到期后任务可以重新投递给其他 worker。这和消息队列里的 visibility timeout 很像。

一个 lease 至少包含：

```text
task_id
attempt_id
lease_id 或 lease_epoch
owner_worker_id
leased_at
lease_deadline
heartbeat/extend count
max_lease_duration
```

worker poll 到任务时，控制面从 `QUEUED` 条件更新到 `RUNNING`，写入 attempt 和 lease。完成时，worker 必须带回这些字段。控制面只接受当前 lease 的完成。如果 lease 已经过期但任务尚未 redelivery，是否接受完成要提前定规则。保守做法是：超过 deadline 后完成要么拒绝，要么要求控制面先做一次 lease validation，避免旧 worker 和新 worker 并发完成。

lease 时长不能拍脑袋。太短会导致长任务被重复投递；太长会让 worker 崩溃后恢复很慢。常见做法是初始 lease 覆盖 P99 执行时间的一部分，再让 worker 心跳续租。续租也要有最大总时长，避免一个卡死 worker 无限续租。任务如果天然长，比如大模型推理、批处理、视频转码，应支持阶段性 heartbeat 和 progress，而不是把 visibility timeout 设置到几个小时。

redelivery 不是简单地把任务放回队列。系统要记录 redelivery reason、attempt number、上次 worker、上次错误、lease 过期时间和 backoff。否则会出现任务在两个 worker 之间疯狂抖动。对于重复超时的任务，要进入 dead letter 或人工队列。对于明显不可重试错误，不要 redelivery。

还要处理 poll-before-start。worker poll 到任务后，还没真正开始执行就崩溃。lease 仍然会到期，任务应该重新入队。worker 已经开始执行但没写 `TaskStarted` 也类似。LogServe 的 fault injection 里覆盖了 poll-before-start worker loss redelivery，这个场景很适合面试讲，因为它说明队列交付和实际执行之间也有不确定窗口。

lease 和外部副作用的边界要讲清楚。lease 只能防止平台接受旧完成，不能阻止旧 worker 调用外部系统。如果任务要写外部数据库或发请求，外部也要有 idempotency key 或 fencing token。比如把 `task_id + attempt_id` 或业务 operation id 传给下游，让下游拒绝重复提交。

多租户场景下，redelivery 不能绕过配额。一个租户大量任务超时后重新入队，仍然要走租户并发和重试预算。否则失败风暴会挤掉健康租户。redelivery 事件也要可观测：按租户、队列、任务类型统计 lease timeout、stale completion、retry count 和 DLQ count。

面试里可以这样答：

```text
我会把 lease 设计成有 owner、attempt、epoch 和 deadline 的执行权。worker poll 时控制面条件更新任务状态并发放 lease；worker 完成或续租时必须带回 lease_epoch；lease 过期后任务按 retry/backoff 策略 redelivery。旧 worker 的完成通过 epoch fencing 拒绝。lease 只保护平台状态，不自动保护外部副作用，所以任务的外部写入也要带业务幂等键。
```

## Q014. 如何处理任务执行成功但完成回写失败？

**回答：**

这是任务系统里最常见的不确定窗口之一。worker 已经把函数跑完了，可能也把结果写到对象存储了，但调用 `CompleteTask` 时控制面不可用、网络超时、log append 失败或响应丢失。worker 此时不知道控制面到底有没有接受完成。

正确处理方式不是重新执行任务，而是重试完成回写。worker 要保留同一次 attempt 的完成上下文：`task_id`、`attempt_id`、`lease_epoch`、result hash、result ref、结束时间和错误/成功状态。再次调用 `CompleteTask` 时用同一组字段。控制面如果之前已经接受过同一完成，就返回已有 terminal 状态；如果还没接受，就写入完成事件；如果任务已经被其他有效 attempt 完成，就返回 stale 或 conflict。

结果写入顺序也要明确。大结果一般先写对象存储，再提交完成事件。这样完成事件一旦存在，`result_ref` 就应该可读。如果对象写成功但完成事件失败，会产生一个暂时孤儿对象；这个可以通过 GC 清理。反过来，如果先写完成事件再上传对象，用户可能查到 succeeded，却下载不到结果，这更糟。

worker 本地也不能无限保存完成上下文。可以做短期 outbox：完成后把待回写记录写到本地可靠队列，后台重试；如果 worker 被杀，本地 outbox 可能丢失，所以任务 lease 到期后仍然要能 redelivery。对于有外部副作用的任务，redelivery 可能再次执行，所以业务侧幂等仍然必要。

控制面要让 completion API 幂等。幂等 key 可以是 `task_id + attempt_id + completion_kind`，也可以是任务 stream 里的 expected event key。重复完成同一成功结果时返回 OK；同一 attempt 提交两个不同 result hash，要报冲突；旧 lease completion 要拒绝。这个规则比“worker 不要重试 CompleteTask”可靠。

如果完成回写失败是因为 log service 不可用，控制面应该 fail closed：没有写入事实来源，就不能告诉 worker 完成已经提交。worker 可以继续重试直到 lease 到期；lease 到期后可能 redelivery，最终靠 completion fencing 和业务幂等收敛。如果为了可用性允许 worker 本地缓存完成事件，必须保证缓存本身可靠、有顺序、有恢复路径，否则只是把问题换了地方。

结合 LogServe，可以说它强调 log-first。任务完成应该先写 `TaskCompleted` 或 workflow/actor/LLM 对应事件，再更新 metadata。如果 metadata 更新失败，replay 可以修复；如果 log append 失败，就不能宣称完成已经成为事实。

面试里可以这样答：

```text
任务执行成功但完成回写失败时，worker 应该用同一个 attempt 和 result_ref 重试 CompleteTask，而不是重新执行。控制面的完成接口要幂等：同一 attempt 同一结果重复提交返回已有终态，不同结果报冲突，旧 lease completion 拒绝。大结果先写对象存储，再提交完成事件；完成事件成功后结果才对用户可见。log 写不进去时不能假装成功，只能重试、等待 lease 处理或后续 redelivery。
```

## Q015. 如何处理完成回写成功但客户端超时？

**回答：**

这个场景和上一题相反：控制面已经成功写入完成事件，也可能已经更新 metadata，但 worker 或客户端没有收到响应。网络超时让调用方以为失败，如果接口不是幂等的，下一次重试就可能把同一个任务完成两次，甚至提交不同结果。

解决办法是让完成回写和状态查询都可重试。worker 重试 `CompleteTask(task_id, attempt_id, lease_epoch, result_ref, result_hash)`。控制面发现同一 completion 已经提交，就返回 `SUCCEEDED` 和同一个 result ref。不要把重复完成当成新事件，也不要因为“任务已经是 succeeded”就返回一个模糊错误，让 worker 不知道该不该继续重试。

客户端侧也要通过 `task_id` 或 `idempotency_key` 查询，而不是重新提交新任务。提交任务的 API 如果超时，客户端重试提交同一个 `idempotency_key`，平台返回原 task_id；完成等待接口超时，客户端调用 `GetTask(task_id)` 或 `GetWorkflow(workflow_id)`。异步系统里，提交和等待最好分开：提交返回 `202 Accepted + task_id`，后续通过状态接口或 callback/webhook 获取结果。

状态接口要能表达“不确定但可继续查询”。例如任务可能是 `RUNNING`、`SUCCEEDED`、`FAILED`、`RETRY_WAIT`、`CANCELED`。如果控制面完成写入成功但 metadata view 更新滞后，状态查询可以从 metadata 返回旧状态，也可以读日志确认终态。更常见的做法是 metadata 更新和完成事件在同一控制面路径里尽量同步，但仍然要有 replay/reconciliation 修复滞后。

如果完成回写成功，但响应在 worker 收到前丢了，worker 可能继续持有本地资源，比如临时文件、执行上下文、锁。它重试完成拿到 OK 后，可以清理本地资源。如果一直拿不到响应，lease 到期后控制面已经有终态，不应 redelivery。这里终态事件比 lease 过期更权威。

外部回调也一样。平台通知用户 webhook 成功写入结果后，webhook 请求超时，平台不知道用户是否收到。webhook 也要有 event id 和幂等语义，用户可以按 event id 去重，平台可以重试投递。不要把内部任务完成和外部通知送达混成一个事务。

面试里可以这样答：

```text
完成回写成功但调用方超时，本质是响应丢失。解决办法是把 CompleteTask 做成幂等接口：同一个 task_id、attempt_id、lease_epoch 和 result_hash 重试时返回同一个终态和 result_ref。客户端不要因为等待超时就重新提交任务，而是用 task_id 或 idempotency_key 查状态。异步 API 应该提交和等待分离，状态查询、完成回写和 webhook 投递都要支持重复调用去重。
```
## Q016. 如何处理同一任务被两个 worker 执行？

**回答：**

同一任务被两个 worker 执行，在可靠任务系统里不是罕见 bug，而是必须预期的故障场景。worker A 拿到任务后网络分区，控制面看不到它，lease 过期后把任务发给 worker B。A 其实还在跑。于是两个 worker 都执行了同一个任务。

平台层能做的第一件事是减少概率：设置合理 lease，支持心跳续租，worker 进入长任务时定期报告进度，控制面不要过早 redelivery。可这只能减少重复执行，不能消灭重复执行。网络分区、进程暂停、长 GC、机器时钟异常、控制面重启，都可能让旧 worker 和新 worker 在一段时间内并存。

第二件事是 fencing。worker 拿到的不只是 `task_id`，还有 `attempt_id` 和 `lease_epoch`。完成时，控制面只接受当前有效 attempt 的 completion。旧 worker A 即使执行成功，只要它的 lease 已经被 B 的新 lease 取代，A 的完成就会被拒绝。这样至少能保证平台最终状态不会被旧 worker 覆盖。

第三件事是业务幂等。fencing 只能保护控制面的结果提交，不能阻止 A 在执行过程中调用外部系统。如果任务是纯计算，重复执行浪费资源但问题不大；如果任务会发邮件、扣款、创建订单、写第三方系统，就必须把业务 operation id 传给外部系统。外部系统不支持幂等时，就要通过 outbox、去重表、唯一约束或补偿流程降低风险。

第四件事是尽量让 worker 可中断。控制面发现 lease 被替换或任务被取消，可以让 worker 在下一次 heartbeat/extend 时收到 cancel signal。对于 Python executor、LLM 请求、外部 API 调用，不一定能立即停止，但至少应该在可检查点退出。不能取消的任务，平台要把它当作可能继续产生副作用的旧执行。

第五件事是观测和审计。重复执行应该被记录成可见事件：同一 task 的多个 attempt、stale completion、lease timeout、duplicate execution duration、外部副作用是否使用幂等 key。多租户环境下还要把重复执行消耗算到哪个租户或平台成本里，避免平台故障让租户账单异常。

结合 LogServe，普通 task 和 workflow step 都承认 worker 可能至少执行一次。控制面用 lease epoch 拒绝 stale task completion；workflow step 用 `workflow_id + step_id + input_hash` 防止重复 successful completion 写出第二个最终结果。这个语义应该表述成 exactly-once-ish result commit，而不是 exactly-once execution。

面试里可以这样答：

```text
我会把同一任务被两个 worker 执行当成必须处理的场景。lease 和 heartbeat 只能减少概率，不能完全避免。控制面用 attempt_id 和 lease_epoch 做 fencing，只接受当前有效 lease 的完成；旧 worker 的 completion 被拒绝。平台状态能保证收敛，但外部副作用还要靠业务幂等键、唯一约束或补偿。对长任务还要支持取消信号和进度心跳，方便尽早停止旧执行。
```

## Q017. 如何处理同一任务被两个 worker 同时完成？

**回答：**

两个 worker 同时完成，是上一题的提交阶段版本。它可能来自 lease 过期后的 redelivery，也可能来自控制面 bug、worker 重试、网络延迟，或者同一个 completion 请求被发送了多次。处理原则是：终态提交必须是条件写入，不能最后写入者获胜。

控制面接受完成时要检查四件事：

```text
task_id 是否存在
任务是否处于 RUNNING 或允许完成的状态
attempt_id / lease_epoch 是否等于当前记录
completion payload 是否和已有 completion 幂等一致
```

如果第一个 worker 的完成合法，控制面写入 `TaskCompleted`，任务进入 `SUCCEEDED`。第二个 worker 的完成到达时，如果它带的是旧 lease，就返回 stale；如果它是同一个 attempt 的重复请求，并且 result hash 一样，就返回已有成功；如果同一个 attempt 提交不同 result hash，就返回 conflict 并记录异常。不要让第二个完成覆盖第一个完成。

数据库实现上可以用条件更新：

```sql
UPDATE tasks
SET status = 'SUCCEEDED', result_ref = ?, version = version + 1
WHERE task_id = ?
  AND status = 'RUNNING'
  AND attempt_id = ?
  AND lease_epoch = ?;
```

返回影响行数为 1 才算完成成功。事件日志实现上可以用 expected stream offset、idempotent event key 或控制面单线程 reducer 来保证同一个 task stream 只产生一个合法 terminal event。无论底层是什么，语义都必须是 compare-and-set。

如果两个完成都来自当前 lease，说明系统内部已经有更严重的问题：同一个 attempt 被两个 worker 执行，或者 lease 发放重复。此时不应该默默接受“谁先到谁赢”后当没事发生，而应该记录 invariant violation，触发告警，并把另一份结果保存为诊断信息或丢入冲突表。用户侧可以仍然看到第一个合法结果，但运维侧要能追查。

外部副作用还是另一条线。如果两个 worker 都已经执行完并写了外部系统，控制面只能选择一个平台结果，不能自动撤销另一个副作用。任务设计时要把外部操作也做成幂等。例如写订单表时以业务 operation id 做唯一键；调用支付网关时使用 idempotency key；发送通知时用 message id 去重。

结合 workflow，step completion 也要做类似处理。同一个 `workflow_id + step_id + input_hash` 的成功结果只能驱动下游一次。重复 completion 不能再次调度下游 step，否则一个上游重复完成会放大成整个 DAG 的重复执行。

面试里可以这样答：

```text
我会让完成提交走 CAS，而不是最后写入者获胜。CompleteTask 必须匹配当前 status、attempt_id 和 lease_epoch；第一个合法完成把任务推进终态；后续同一 completion 返回已有结果，旧 lease 返回 stale，不同结果返回 conflict。事件日志可以用 expected offset 或 idempotent event key 实现同样语义。两个 worker 都执行出的外部副作用，则必须靠业务幂等或补偿处理。
```

## Q018. 如何处理控制面重启时内存队列丢失？

**回答：**

如果控制面重启会导致内存队列丢失，那说明内存队列不能是 source of truth。它只能是从日志或数据库重建出来的调度缓存。控制面可以用内存队列提高 poll 性能，但任务是否存在、是否可运行、是否已经完成，必须能从持久事实恢复。

重启恢复一般分几步。

第一，加载事实来源。可以从 append-only log 扫描 `TaskSubmitted`、`TaskStarted`、`TaskCompleted`、`TaskFailed`，也可以从数据库任务表读取当前状态。如果系统采用 log-first，就先 replay log 重建 metadata view。

第二，重建调度索引。把 `QUEUED`、`RETRY_WAIT` 到期、`RUNNING` 但 lease 已过期的任务重新放回可调度集合。已经 `SUCCEEDED`、`FAILED`、`CANCELED`、`DEAD_LETTER` 的任务不能入队。

第三，处理 running 中间态。控制面重启时，worker 可能还在执行。保守做法是保留 lease 到原 deadline，让 worker 继续完成；如果控制面没有可靠保存 lease，就把 running 标记成需要校验或等待过期，不要立刻无条件重发。LogServe 的思路是从 shared log/bootstrap metadata view 恢复控制面状态，并让 lease/redelivery 规则处理 worker loss。

第四，恢复 timers。重试 backoff、workflow timeout、task deadline、lease expiry、actor ownership expiry 都可能原本挂在内存 timer 上。重启后要从持久状态重新计算下一次触发时间。不要把 `time.After` 或内存 heap 当成唯一事实。

第五，恢复 worker view。worker 心跳会重新建立当前活性；重启前的 worker load 可能已经过期。控制面不能拿旧的内存 worker 状态继续调度。对于模型缓存这类 worker-local 信息，重启后也要等 worker 注册/心跳重新上报。

第六，做幂等防重。重启过程中可能发生 worker completion 与 replay 并发。完成 API 仍然要校验 task 状态、lease epoch 和 terminal event，不能因为控制面刚启动、view 尚未追上，就重复接受完成。

多租户平台还要恢复租户队列和配额。比如每个租户的 running count、queued count、retry budget、fair scheduling deficit 都可能在内存里。running count 可以从任务状态重算；纯调度算法里的 deficit 如果丢了，最多影响短期公平，但不应影响 correctness。配额扣减和计费则必须来自持久事件或账单表。

面试里可以这样答：

```text
内存队列只能是调度缓存，不能是事实来源。控制面重启后要从日志或数据库重建 metadata view，再把 queued、retry 到期、lease 过期的 running 任务重新放入调度索引；终态任务不能入队。lease、retry timer、workflow timeout 和租户配额都要从持久状态重算。worker 心跳和缓存状态等重启后重新上报。这样控制面丢内存队列只影响恢复时间，不会丢任务。
```

## Q019. 如何处理日志服务不可用？

**回答：**

如果日志服务是 source of truth，日志不可用时系统要先 fail closed。也就是说，不能继续接受会改变事实的新请求，然后把它们只放在内存里等日志恢复。那样做短期看可用，实际是在制造一个无法可靠 replay 的第二套事实。

先区分读和写。写路径包括提交任务、开始任务、完成任务、workflow step 状态变化、actor command applied、LLM completion、result ref 可见化。这些如果必须先写日志，那日志不可用时就应该返回明确错误或进入受控排队。读路径可以降级：已有 metadata view 仍然可以提供 status 查询，但要标注数据可能滞后；dashboard 可以只读；worker poll 可以暂停；新的调度决策要看是否需要写 `TaskStarted` 事件。

受控排队不是随便内存缓存。如果业务确实需要在日志短暂不可用时接收请求，可以做 durable ingress buffer，比如本地 WAL、数据库 outbox、上游消息队列。这个 buffer 本身就变成临时 source of truth，必须有顺序、幂等、恢复和容量限制。没有这些保证，不如直接拒绝新写入。

worker completion 是最敏感的。worker 执行成功后，日志服务不可用，控制面不能承认任务完成。worker 可以重试 completion；lease 可能到期；任务可能后续 redelivery。为了减少重复执行，可以让控制面在日志不可用期间暂停新的 redelivery 或延长已有 lease，但这也要有上限。不能为了避免重复执行而无限挂起任务。

日志不可用还会影响 actor 和 workflow。actor command applied 如果不能写 log，就不能更新 actor 状态为事实；workflow step succeeded 如果不能写 history，就不能调度下游 step。否则重启后这些状态都丢失。这里要牺牲可用性保正确性。

恢复后要做 reconciliation。检查 metadata view 里是否有未落日志的状态，检查 worker 是否有待回写 completion，检查对象存储是否有孤儿 result，检查 projection lag。LogServe 的 log-first 设计意味着 metadata 是从 log 修复的；如果代码里出现“metadata 已更新但 log 没写”的路径，就破坏了这个假设。

生产级系统还会把日志服务本身做高可用：多副本、quorum commit、leader election、磁盘水位保护、fsync 策略、备份、监控和容量预警。单机 append-only log 只能说明 crash recovery，不能解决整台机器故障。面试里要把“日志不可用时的降级策略”和“日志服务自身的 HA 设计”分开说。

多租户场景下，日志服务不可用会变成全平台故障。可以按租户做限流和降级通知，但不能让某个高优租户绕过日志写入直接改 metadata。最多是给高优租户预留 durable buffer 或独立 log shard，这仍然要保持事实来源语义。

面试里可以这样答：

```text
如果日志是 source of truth，日志不可用时写路径要 fail closed：不能提交新任务、不能确认完成、不能应用 actor command 或 workflow step。读路径可以从已有 metadata view 降级提供只读状态。若必须吸收请求，需要一个同样持久、可恢复、容量受限的 ingress buffer，否则只是把事实藏进内存。日志恢复后要做 projection/reconciliation，清理孤儿 result 和待回写 completion。日志服务自身要通过复制和 quorum 提高可用性，但单机机制验证不能冒充生产级 HA。
```

## Q020. 如何处理元数据数据库不可用？

**回答：**

metadata 数据库不可用时怎么处理，取决于 metadata 在系统里的角色。如果 metadata 是 source of truth，比如任务表就是唯一权威状态，那么数据库不可用时写路径基本必须停止。否则任务状态无法可靠提交。如果 metadata 只是 materialized view，而 append-only log 才是 truth，系统可以考虑继续写日志，但调度和查询能力会受影响。

我会先把模式说清楚。

第一种模式：DB 是 truth。提交任务、领取任务、完成任务都在数据库事务里完成。DB 不可用时，控制面不能接受改变状态的请求，只能返回错误或进入上游 durable queue。恢复后从 DB 本身继续。这个模型简单，但 DB 是强依赖。

第二种模式：log 是 truth，DB 是 view。提交任务可以先写 log；DB 不可用时，如果控制面仍能写 log，就可以把事件持久化下来，等 DB 恢复后 replay projection。不过调度器如果依赖 DB 查询 queued tasks、worker load、租户配额，那在线调度可能要暂停或退回内存 view。这个模式的难点是：你必须确保所有需要恢复的字段都在 log 里，而不是只在 DB 里。

LogServe 更接近第二种口径：shared log 是主要事实来源，metadata 是 materialized current view。Compose 模式可以把 materialized dashboard/task/workflow/actor/model view 写到 PostgreSQL；如果 PostgreSQL 表丢了，control 可以从 shared log bootstrap 重建。这说明 metadata 不一定是不可替代的 truth。但当前项目边界仍然是机制验证，不是完整生产级数据库故障自治平台。

DB 不可用时，读路径可以分级降级：

```text
status 查询：如果内存 view 还在，可以返回可能滞后的状态
list/search/dashboard：通常降级或不可用
调度：如果缺少索引、租户配额或 worker load，应该暂停或保守限流
完成回写：如果完成事件已经写 log，可以稍后补投影；如果完成逻辑必须查 DB 条件，则需要 fail closed
```

这就是为什么设计时要避免“有些字段只在 DB，有些字段只在 log”。比如 lease epoch 如果只在 DB，DB 不可用时就无法判断 completion 是否 stale；租户配额如果只在 DB，继续调度可能突破配额；workflow ready step 如果只在 DB，log 有事件也不够调度。要么这些字段也事件化，要么承认 DB 是该语义的 truth。

恢复后要追 projection。DB 恢复不等于系统立刻正确。投影器要从最后成功 offset 开始补事件，重建索引，校验对象数量和终态数量，处理期间产生的新事件。可以让 API 暂时返回 `degraded` 或 `rebuilding`，等 projection lag 降到阈值再恢复完整调度。

多租户平台还要防止 DB 故障后的恢复风暴。DB 刚恢复时，如果所有租户的积压事件同时投影、所有任务同时重新调度，很容易二次打爆系统。应该按 tenant、队列、优先级分批恢复；对低优先级租户限速；对过期任务、超过 deadline 的任务直接失败或进入补偿队列。

面试里可以这样答：

```text
metadata DB 不可用时先看它是不是 source of truth。如果 DB 是 truth，写路径要停止或进入上游持久队列。如果 log 是 truth、DB 只是 materialized view，可以继续写 log，但调度、list 查询、dashboard 和配额判断可能降级；DB 恢复后从投影 checkpoint 追事件并校验 view。关键是不要让某些关键字段只存在 DB 却口头上说 DB 可重建。恢复时还要控制 projection 和 redelivery 风暴，尤其在多租户平台里要按租户和优先级分批恢复。
```

## Q021. 如何处理对象存储不可用？

**回答：**

对象存储不可用时，第一步不是马上设计一个“备用对象存储”，而是先判断对象存储在这条路径里承担什么角色。它可能只是保存大结果、actor snapshot、模型 checkpoint，也可能是审计归档、备份或跨区域复制的一部分。角色不同，降级方式完全不同。

在任务平台里，我通常会把对象存储分成两类依赖。

第一类是写入后才让状态可见的强依赖。比如 worker 生成了一个大结果，系统要求 `TaskCompleted` 事件里必须带 `result_ref`、size 和 checksum。这个时候对象存储写失败，任务不能被标记为成功。worker 可以重试上传，或者把结果写到一个受控的本地 outbox/临时 spool，再继续重试；但不能先把任务置为 succeeded，然后指望以后再补对象。否则用户会看到成功状态，却下载不到结果。

第二类是性能或恢复优化依赖。比如 actor snapshot 写失败，不一定要让 actor command 失败。可以继续保留 command log，只是下一次 replay 会更慢；checkpoint cache 拉取失败，LLM 请求可以选择排队、换 worker、冷启动失败或返回可重试错误。这里要根据业务选择：是延迟变高可以接受，还是必须失败。

对象存储写入要幂等。key 里最好带 `tenant_id/task_id/attempt_id/result_hash`，或者直接用内容 hash。worker 重试上传同一个结果时应该得到同一个 ref；同一个逻辑 key 如果出现不同 checksum，要报冲突。对象存储已经写成功但完成事件没写成功，会产生孤儿对象，这比“完成事件成功但对象不存在”容易处理。孤儿对象可以通过延迟 GC、引用扫描或生命周期规则清理。

读路径也要降级。用户下载 result 时对象存储不可用，可以返回 `RESULT_TEMPORARILY_UNAVAILABLE`，让状态仍然是 succeeded，但结果暂时不可读；如果系统有只读副本、跨区域复制或本地缓存，可以切到副本。切换副本要注意一致性和版本，读取时应校验 `etag/version/checksum`，不能随便拿一个同名对象当结果。

对对象存储 503、限流或慢请求，客户端要有超时、重试、指数退避和 jitter。大对象下载最好支持 range read 和 multipart，因为一个 2GB 对象失败后从头重传太浪费。对象存储官方实践里也强调监控 5xx/503、使用多连接、byte-range fetch 和 SDK 内置重试。面试里不用背厂商名，讲出这些机制就够了。

还要防止对象存储故障拖垮控制面。控制面不应该同步搬运大字节，只保存引用和元信息。worker 或 result service 负责对象读写。对象存储慢时，控制面最多看到 completion 重试增加、result read 失败增加，而不是 API 线程全被大上传堵住。

结合 LogServe，可以这样讲：LogServe 的 workflow result 和 actor snapshot 走 result-store interface，日志里保存 `result_ref`，默认本地 `local://`，Compose 边界可以接 MinIO/S3-compatible store。当前项目验证的是引用边界和恢复路径，不是对象存储本身的高可用。生产化时要补对象版本、checksum、bucket policy、生命周期、跨区域复制和对象存储故障注入。

面试里可以这样答：

```text
我会先看对象存储是不是当前路径的强依赖。大结果写入场景里，必须先成功写对象并拿到 checksum/version，再提交完成事件；对象写失败时任务不能标记 succeeded，只能重试或进入待回写队列。读取失败可以返回结果暂不可用，或者读副本并校验版本。对象存储慢或 503 时用超时、重试、退避、range/multipart 和限流，控制面只保存 result_ref，不搬运大 payload。
```

## Q022. 如何设计服务降级策略？

**回答：**

降级策略要从业务核心路径开始，而不是从技术组件开始。一个服务依赖日志、metadata、对象存储、模型缓存、监控、配置中心、外部 API。它们不是同等重要。日志如果是 source of truth，写不进去就不能承认状态变化；监控临时不可用时，业务可能还要继续跑，但要保留本地缓冲或至少暴露告警。降级的重点是把硬依赖和软依赖分清楚。

我会先把功能分成几类。

第一类是不能降级的正确性路径。比如任务完成事件写入、actor command applied、workflow step succeeded、幂等冲突检查、租户权限校验。这些失败时宁可返回错误，也不要制造不可恢复的状态。

第二类是可以延迟的路径。比如 dashboard 刷新、异步审计落仓、指标上报、对象存储 GC、冷数据归档。它们失败时可以重试、排队或标记 degraded，不应该影响主流程提交。

第三类是可以提供旧数据的路径。比如 dashboard 当前状态、模型缓存统计、租户用量报表、配置快照。依赖不可用时可以返回带时间戳的旧视图，但必须告诉调用方这是 stale view。不能把旧数据伪装成最新状态。

第四类是可以关掉的增强能力。比如 predicted-latency scheduler 依赖历史 EWMA；如果统计不可用，可以退回 locality-aware 或 resource-only。比如对象存储预览失败，仍然可以返回 result_ref。增强能力的降级要简单，别写一个比主路径还复杂的 fallback。

降级还需要触发条件。常见信号是错误率、超时率、p99 延迟、队列长度、retry storm、依赖健康检查、租户配额耗尽。触发后进入明确模式：只读、暂停调度、拒绝新提交、禁用低优先级租户、降低并发、关闭某个 adapter、启用本地配置快照。退出降级也要有条件，最好带滞后，避免来回抖动。

要小心“假降级”。比如数据库不可用时把写请求放进无限内存队列，看起来用户请求成功了，实际控制面一重启就丢。再比如日志不可用时继续更新 metadata，破坏了 log-first 语义。真正的降级应该让系统进入可解释、可恢复的状态，而不是把失败藏起来。

多租户平台里，降级最好按租户和优先级做。免费租户可以先限流，付费租户保留更高并发；内部 benchmark 任务可以暂停，用户在线 workflow 继续；大结果下载可以限速，任务完成写入不受影响。统一全站降级简单，但太粗糙。

结合 LogServe，可以说：如果 shared log 不可用，改变事实的写路径 fail closed；如果 dashboard snapshot 或 benchmark 输出失败，不影响任务语义；如果 LLM predicted stats 不可用，调度可以退回更简单策略；如果 metadata view 滞后，可以从 log replay 修复。这个回答比“加熔断、加限流、加缓存”更能体现系统边界。

面试里可以这样答：

```text
我会先把依赖分成正确性强依赖、可延迟依赖、可返回旧数据依赖和增强能力依赖。source of truth 写入失败时 fail closed；dashboard、指标、GC、报表可以排队或降级；读视图可以返回带时间戳的 stale data；高级调度可以退回简单策略。降级要有触发条件、退出条件和可观测指标，不能用无限内存队列伪装成功。
```

## Q023. 如何选择同步路径和异步路径？

**回答：**

同步和异步不是风格选择，而是语义选择。同步路径适合调用方必须马上知道结果、状态变化很小、延迟可控、失败能直接返回的操作。异步路径适合耗时长、可能重试、需要排队、可以稍后查询状态、或者要隔离下游抖动的操作。

我会先问四个问题。

第一，调用方是否必须立即拿到业务结果？如果只是提交任务、启动 workflow、请求 LLM 批处理，通常不需要同步等到全部执行完。返回 `task_id/workflow_id` 更稳。相反，权限校验、幂等冲突、任务是否被接受，这些应该同步返回。

第二，操作是否有外部副作用或长尾延迟？对象存储上传、大模型加载、第三方 API 调用、长 workflow step 都适合异步。同步等待这些操作会把客户端连接、网关超时和控制面线程绑死，最后变成雪崩。

第三，失败是否需要重试和补偿？需要重试的操作更适合异步，因为平台可以记录 attempt、backoff、DLQ 和审计。同步接口里做多次重试会把用户请求变慢，还可能在客户端超时后继续执行，制造不确定状态。

第四，是否影响 source of truth？有些小写入必须同步完成，比如 `TaskSubmitted` 写日志。这个同步不是为了等 worker 执行，而是为了确认任务已经进入事实来源。可以把“持久接受任务”同步，把“实际执行任务”异步。

一个可靠任务平台常见拆法是：

```text
同步：鉴权、参数校验、幂等 key 检查、写 TaskSubmitted、返回 task_id
异步：调度、worker 执行、重试、对象上传、completion、通知、GC
同步查询：GetTask/GetWorkflow 返回当前 view 或可解释的 degraded 状态
```

同步路径要短，最好不访问太多依赖。异步路径要有持久队列或事件日志，不能只靠 goroutine。同步路径里如果必须调用下游，也要设置短 timeout、明确错误码和幂等语义。异步路径里如果要通知客户端，webhook 也要当成另一个异步投递任务，带 event id 和去重。

在 LogServe 里，提交任务和写 shared log 是同步接受路径；worker poll、task execution、workflow step scheduling、actor command execution、LLM request 和 result store 读写属于异步执行路径。这个拆法让控制面能快速确认任务已经被系统接收，同时把长耗时和失败恢复交给 lease、retry 和 replay。

面试里可以这样答：

```text
我会把同步路径留给“必须马上决定且延迟可控”的动作，比如鉴权、幂等检查、写入任务提交事件、返回 task_id。真正耗时、可能失败重试、可能产生长尾延迟的执行放到异步路径，由任务状态机、lease、attempt、backoff 和 result_ref 管理。同步路径负责可靠接收，异步路径负责可靠完成。不要在同步 API 里等完整 workflow 跑完。
```

## Q024. 如何设计多租户隔离？

**回答：**

多租户隔离要同时看安全、资源、公平性、数据、运维和成本。只在表里加一个 `tenant_id` 不叫完整多租户，只能说明查询能过滤。真正的问题是：一个租户能不能读到别人的数据；一个租户能不能耗尽共享 worker；一个租户的坏任务会不会拖垮日志、对象存储、metadata；一个租户的审计和计费能不能单独解释。

我会从控制面隔离开始。所有控制面对象都带 `tenant_id`：task、workflow、actor、model、result_ref、worker pool、quota、audit event。API 鉴权后生成 tenant context，后续查询、状态转移、对象 key、日志 stream 都不能脱离这个上下文。数据库层最好有复合主键或索引，比如 `(tenant_id, task_id)`，避免跨租户 id 碰撞。

数据面隔离要按风险分层。低风险内部团队可以共享 worker pool，用配额和队列公平性隔离；不可信租户要独立 namespace、独立 worker pool、独立 node pool，甚至独立集群。Kubernetes 官方也把多租户隔离分成控制面和数据面：namespace、RBAC、quota、network policy、storage isolation、sandbox、node isolation 都是不同层级的工具。面试里要讲清楚：namespace 是管理边界，不是强安全边界。

存储隔离也要具体。对象存储 key 或 bucket 要带租户前缀，bucket policy/IAM 要限制访问；日志 stream 可以按 `tenant:<id>/task:<id>` 或至少在 event header 里带 tenant；metadata 查询必须默认带 tenant 条件；备份和导出要能按租户过滤。尤其是 result_ref，不要让用户拿到一个裸 `local://path` 或全局 S3 key 后绕过权限。

资源隔离包括 CPU、内存、GPU、并发、队列长度、对象存储容量、日志写入速率、metadata 查询 QPS、LLM 模型缓存容量。这里会遇到共享效率和隔离强度的取舍。共享模型缓存能省钱，但可能泄露访问模式，也可能让大租户长期占住显存。敏感模型或敏感租户就应该用专属池。

调度隔离要避免一个租户把队列占满。可以使用 per-tenant queue、weighted fair queue、token bucket、concurrency cap、priority class、deadline、preemption 和 admission control。不要只在入口限流，因为任务重试、redelivery、workflow fan-out 也会在系统内部制造负载，这些也要按 tenant 计入预算。

可观测性和审计也要按租户分维度。每个任务、attempt、result、worker decision、quota rejection、stale completion 都要能按 tenant 查询。多租户事故里，最糟糕的情况不是系统慢，而是你说不清是哪个租户造成的，也说不清受影响的是谁。

结合 LogServe，当前项目还不是生产级多租户平台，但第 30 章可以这样扩展：把 `tenant_id` 放入 task/workflow/actor/model/result 的控制面对象；调度器按租户维护并发和公平队列；result store key 带 tenant；shared log event 带 tenant；dashboard 和 benchmark 按租户聚合。这个是从机制验证走向平台化的自然路线。

面试里可以这样答：

```text
我会把多租户隔离分成控制面、数据面、存储、资源、调度和审计六层。控制面所有对象带 tenant_id；数据面按风险选择共享 worker、专属 pool、专属 node 或专属集群；对象存储和日志按租户分 key、policy 和 stream；资源按租户做并发、QPS、容量和缓存预算；调度用 fair queue 防止互相挤压；审计和指标必须能按 tenant 解释每一次状态变化和资源消耗。
```

## Q025. 如何设计租户级配额和限流？

**回答：**

租户级配额和限流要覆盖入口流量，也要覆盖系统内部放大的流量。很多平台只给 API Gateway 加 QPS 限流，结果一个合法请求启动 1000 个 workflow step、触发 1000 次对象读取和 1000 次 LLM 请求，内部资源照样被打爆。

我会把配额分成几类。

第一类是速率配额：提交任务 QPS、查询 QPS、完成回写 QPS、对象读写 QPS、日志 append QPS。通常用 token bucket 或 leaky bucket。token bucket 允许短 burst，但长期受平均速率限制，比较适合用户请求。

第二类是并发配额：running task 数、running workflow 数、actor command in-flight 数、LLM request in-flight 数、每模型并发、每 worker pool 并发。并发配额比 QPS 更能保护 worker 和 GPU，因为慢任务会占住资源。

第三类是容量配额：对象存储 bytes、日志 bytes、metadata 行数、actor snapshot 数、checkpoint cache 空间、审计保留空间。容量配额要有软硬阈值：接近上限先告警，超过硬上限拒绝新写入或执行清理策略。

第四类是预算配额：每天/每月 tokens、GPU 秒、CPU 秒、网络出站 GB、重试次数、失败任务预算。这类配额更接近计费和成本控制。

限流点要放在多个位置。入口提交时做 admission control；调度前检查租户 running count；worker poll 时控制每租户派发；completion 和 retry 也要扣预算；对象下载走 result service 时再限速。只在一个点限流很容易被绕过。

配额需要可解释。拒绝任务时返回 `TENANT_QUOTA_EXCEEDED`，告诉是哪个维度：`running_tasks`、`llm_tokens`、`result_storage_bytes`、`request_rate`。如果只是返回 429，用户不知道该减少并发、减少结果大小，还是申请更高额度。

还要区分硬限制和公平调度。硬限制是不能超过，比如每租户最多 100 个 running task。公平调度是在资源不足时决定谁先拿资源，比如 weighted fair queue。一个租户没达到硬限制，也可能因为其他租户权重更高而排队。这个差别要讲清楚。

Kubernetes 里的 ResourceQuota 是一个很好的类比：多个团队共享固定节点时，ResourceQuota 用来限制 namespace 的 aggregate resource consumption。LogServe 这种任务平台可以借同样思路，但维度要换成任务、workflow、actor、LLM、result bytes 和日志写入。

面试里可以这样答：

```text
我会把租户配额拆成速率、并发、容量和成本预算。入口用 token bucket 控制提交和查询；调度器检查 running task、workflow、LLM 和 actor 并发；对象存储、日志和 metadata 做容量配额；GPU 秒、token 数、网络出站和重试次数做预算。限流要在提交、调度、worker 派发、result download 和 retry 上同时生效，并返回可解释的拒绝原因。
```

## Q026. 如何处理 noisy neighbor？

**回答：**

noisy neighbor 的本质不是“某个租户请求量大”这么简单，而是一个租户的行为通过共享资源放大成了其他租户的尾延迟、错误率或者成本上升。共享资源可能是入口网关、控制面队列、日志追加、metadata DB、worker 池、对象存储带宽、LLM serving 的 GPU batch、模型缓存，甚至是观测系统的指标基数。

我会先把问题拆成三层：发现、隔离、处置。

发现层要做租户维度的指标，而不能只看全局平均值。关键指标包括 tenant 级 QPS、排队时间、任务运行时间、重试次数、lease 过期次数、redelivery 次数、log append latency、metadata 写入延迟、object storage 读写 5xx、结果下载带宽、worker CPU/GPU 使用率、模型缓存命中率、缓存淘汰次数。全局 p95 看起来正常时，某个小租户的 p99 可能已经被大租户拖垮，所以 noisy neighbor 的诊断一定要能按 tenant、workflow、model、priority 分组。

隔离层不能只依赖一个限流器。入口限流只能挡住新请求，挡不住已经进入系统的 fan-out、重试风暴和大结果下载。我会在几个位置都放控制点：入口按租户 token bucket；控制面按租户队列和并发上限；scheduler 用 weighted fair queuing 或 deficit round robin；worker 池按租户设置并发槽；对象存储下载按租户限速；LLM serving 按模型、租户和缓存 locality 做 admission control。这样即使一个租户提交了大量任务，也只能消耗自己配额内的队列和执行槽。

处置层要提供分级隔离。普通租户共享 worker pool，但有明确配额和公平调度；高价值租户可以有独立队列、独立 worker pool、独立对象存储前缀或独立模型缓存池；强隔离场景可以做到 namespace、node pool、数据库 schema、bucket、密钥都隔离。强隔离成本更高，所以不能默认所有租户都强隔离，而是按 SLA 和风险分层。

还要特别防止“内部放大”。例如一个租户的任务失败后重试 5 次，每次 workflow 又拆出 20 个子任务，表面上只有一个请求，实际占用了 100 次执行机会。配额计算不能只算外部 API 请求数，还要算内部派生任务、重试、对象存储 I/O、日志写入和模型 token。否则 noisy neighbor 会绕过入口限流，在系统内部被放大。

在 LogServe 这个项目里，我会把 tenant_id 作为任务、workflow、actor、result_ref 和审计事件的基本字段。调度层按 tenant 建队列或虚拟队列，worker 拉取时遵守租户并发上限，结果存储层按 tenant 做带宽和容量统计，LLM scheduler 则额外统计模型缓存命中、模型加载次数和 GPU batch 占用。当前项目更多是机制验证，不会声称已经具备生产级多租户隔离，但设计上要说明这些控制点可以怎样加进去。

面试里可以这样答：

```text
我会把 noisy neighbor 当成共享资源治理问题，而不是单点限流问题。
先按 tenant 观测队列等待、尾延迟、重试、worker 占用、对象存储带宽和模型缓存淘汰；
再在入口、队列、scheduler、worker、对象存储和 LLM serving 多个位置做配额、并发和公平调度；
最后按租户等级提供共享池、独立池、独立数据面等不同隔离级别。
重点是配额要覆盖内部 fan-out、重试和大结果下载，否则入口限流挡不住真正的资源放大。
```

## Q027. 如何保证升级过程中事件格式兼容？

**回答：**

只要系统采用 append-only log 或 event sourcing，事件格式兼容就不是一个普通序列化问题，而是恢复能力的核心问题。因为旧事件会长期保留，新版本控制面、worker、投影视图、审计工具都必须能够读取过去写入的事件。只要一次升级让旧事件无法 replay，source of truth 就会被破坏。

我会先定几条硬规则。

第一，事件类型和语义要稳定。`TaskSubmitted`、`TaskLeased`、`TaskCompleted` 这种事件一旦进入日志，就不能在原名字下改变含义。如果新语义和旧语义不兼容，应新增事件类型或新增 schema version，而不是悄悄改旧字段的解释。

第二，字段演进以“只增不改”为主。可以添加 optional 字段，可以添加带默认值的字段，但不能删除仍有读者依赖的字段，不能复用已经删除的字段编号或字段名，不能把字段类型从 string 改成 int，也不能把时间单位从毫秒改成纳秒却还叫同一个字段。使用 Protobuf 时，字段编号比字段名更重要，被删除的编号要 reserved；使用 JSON 时，也要明确 missing、null、空字符串、默认值之间的区别。

第三，读路径要比写路径更兼容。新版本 reader 必须能读旧事件，旧版本 reader 最好能忽略新字段。升级时通常先部署能读新旧两种格式的 reader，再切 writer 写新格式；如果需要回滚，旧版本 reader 也不能被新事件直接打死。比较稳的做法是双读单写、再双写或灰度写，最后再切换默认格式。

第四，要有 upcaster 或 migration 层。replay 时，读取旧事件后先经过一个转换层，把 v1 事件补齐成当前内部模型需要的结构。这样业务逻辑只面对当前版本对象，兼容逻辑集中在边界上，不会散落在状态机各处。upcaster 也要可测试，不能靠临时 if-else 堆在核心流程里。

第五，要把兼容性变成测试，而不是文档承诺。每次改事件格式，都要拿历史事件样本做 replay 测试，验证旧日志能重建相同状态；也要做 mixed-version 测试，验证新 control plane 和旧 worker、旧 control plane 和新 worker 的交互是否还能完成任务。对关键字段要做 golden file 测试，防止 JSON key、枚举值、时间单位被无意改掉。

在 LogServe 里，log-first 和 replayable metadata view 依赖的就是这套约束。任务状态、workflow 边、actor 消息、LLM 调度事件都应该有明确的 `event_type`、`schema_version`、`event_id`、`occurred_at` 和稳定的业务字段。旧事件可以通过 upcaster 补齐新字段；真正的破坏性变化则要定义新事件类型，并让投影逻辑在一段时间内同时支持新旧事件。

面试里可以这样答：

```text
我会把事件日志当成长期协议，而不是内部结构体。
升级原则是：事件类型语义稳定，字段只增不改，不复用删除字段，reader 先兼容新旧格式，writer 后切换；
必要时用 upcaster 把旧事件转换成当前内部模型。
每次格式变更都必须跑历史日志 replay、golden file 和 mixed-version 测试，证明旧事件还能恢复，新旧组件还能共存。
```

## Q028. 如何保证旧 worker 和新 control plane 可以共存？

**回答：**

旧 worker 和新 control plane 共存，本质是分布式协议升级问题。不能假设所有进程同时升级完成，也不能假设升级过程中没有任务正在运行。只要 worker 是独立进程、节点或容器，系统就一定会出现新旧版本混跑。

我会先把 worker 和 control plane 之间的交互定义成稳定协议，而不是直接暴露内部结构体。worker 注册时上报 `worker_version`、`protocol_version`、支持的 task type、支持的 event schema、资源能力、模型能力和可选 feature flags。control plane 根据 capability 做调度：旧 worker 没有声明支持的任务类型，不能发给它；旧 worker 不理解的新字段，control plane 不能要求它必须回传。

升级策略通常分两阶段。第一阶段先升级 control plane，让它能读旧 worker 的心跳、lease、completion、failure report，也能理解新 worker 的扩展字段，但默认仍按旧协议下发任务。第二阶段等大部分 worker 已经升级并注册新 capability 后，再灰度开启新任务类型、新事件格式或新调度策略。这样即使回滚，也不会因为日志里突然出现旧版本完全不认识的事件而卡死。

完成回写协议尤其要稳定。`task_id`、`attempt_id`、`lease_epoch`、`worker_id`、`completion_token` 这类字段不能随意改，因为它们直接决定是否接受完成结果。如果要新增幂等 token、结果引用或执行统计，也应作为可选字段加入，先让 control plane 能处理“旧 worker 没传”的情况，再逐步要求新 worker 传。

还要考虑 control plane 比 worker 新很多时的策略。太旧的 worker 可以允许继续完成已有任务，但不再分配新任务；更旧的 worker 可以被标记为 drain；超过兼容窗口的 worker 直接拒绝注册。这样兼容不是无限期负担，而是有明确版本窗口和迁移节奏。

在 LogServe 中，这个问题可以落在 worker registration 和 heartbeat 设计上。worker 注册事件里记录版本和能力，调度器只根据能力分配任务；control plane 接收完成事件时用 task state、attempt_id 和 lease_epoch 判断合法性，而不是信任 worker 版本。对 LLM serving scheduler 来说，旧 worker 可能不支持某种模型缓存指标，那它就不能参与依赖该指标的调度策略，但仍可执行普通任务。

面试里可以这样答：

```text
我会把 worker/control plane 交互当成版本化协议。
worker 注册时声明版本、协议和 capability；control plane 先做到能读旧 worker，再灰度使用新能力。
新任务类型、新事件格式和新完成字段都必须 feature-gated，不能发给未声明支持的 worker。
对太旧的 worker，可以允许完成存量任务、停止分配新任务，最后拒绝注册。
```

## Q029. 如何设计跨区域部署？

**回答：**

跨区域部署要先问目标是什么。是为了降低用户访问延迟，还是为了区域级高可用，还是为了灾备，还是为了数据合规？不同目标会导向不同架构。很多系统把“多 region”说得很轻，但真正困难的是一致性、故障切换、数据归属和成本，而不是把服务复制几份。

我会先给数据分类。任务日志和状态事件是 source of truth，要求最高；metadata view 是可重建的投影，可以从日志 replay；对象存储里的大结果需要持久化和校验；worker 本地缓存、模型缓存、临时文件通常可以丢；metrics 和 traces 重要但不应阻塞主路径；配置、密钥和权限策略属于控制面依赖，也必须有跨区域恢复方案。

常见部署模式有三类。第一类是单主区域加只读副本，所有写入进入主 region，其他 region 服务读流量或近端缓存。这种一致性简单，但主 region 故障时需要 failover。第二类是 active/passive，备用 region 平时保留最小容量或 warm standby，故障时接管。第三类是 active/active，不同 region 都接写入，延迟低但冲突处理、全局配额、全局幂等和日志顺序会复杂很多。

对可靠任务平台，我倾向于按租户或工作流做 region ownership，而不是所有 region 随便接所有写入。每个租户有 home region，任务提交、日志追加和状态转换在 home region 完成；其他 region 可以执行只读查询、结果下载或冷备恢复。需要跨区域执行时，也应通过明确的任务迁移或远程 worker 协议，而不是两个 region 同时修改同一任务状态。这样可以减少 split-brain。

日志层是关键。如果日志是 source of truth，就要明确跨区域复制是同步还是异步。同步复制能降低 RPO，但会增加写入延迟和依赖远端可用性；异步复制写入快，但故障时可能丢失最近事件。对象存储可以用跨区域复制和版本校验，但 metadata view 最好能从复制后的日志重建，避免两个 region 各自维护不可解释的状态。

控制面要支持区域失联。入口路由可以按租户 home region 转发，worker 尽量就近执行；当主 region 不可用时，备用 region 根据复制到的日志位置和对象存储状态接管。接管要有 fencing，防止原主 region 恢复后继续接受写入，造成双主。故障恢复后还要有 failback 流程，把写入所有权明确迁回，而不是自动互相覆盖。

在 LogServe 里，我不会声称当前单机实现已经具备跨区域能力，但可以把路线讲清楚：shared log 是状态恢复基础，metadata view 可以 replay，worker 尽量无状态，对象存储保存大结果。生产化跨区域版本需要复制日志、区域所有权、租户 home region、对象存储复制、控制面 fencing 和演练过的 failover/failback。

面试里可以这样答：

```text
我会先明确跨区域目标：低延迟、HA、DR 还是合规。
对任务平台，我更倾向按租户或工作流设置 home region，日志写入和状态转换只在 owner region 发生，其他 region 做只读、缓存或灾备。
跨区域复制要明确同步还是异步，因为它直接决定 RPO 和写延迟。
真正要防的是 split-brain，所以 failover 必须有 fencing，failback 也要有明确流程。
```

## Q030. 如何设计 disaster recovery？

**回答：**

disaster recovery 不是“有备份”就结束了，而是系统在严重故障后能否按目标恢复服务和数据。设计 DR 时，我会先列故障场景：单节点故障、可用区故障、区域故障、对象存储不可用、metadata DB 损坏、日志尾部损坏、误删数据、错误发布、凭证泄露、依赖服务大面积不可用。不同故障需要的恢复手段不一样。

DR 的核心指标是 RPO 和 RTO。RPO 决定最多能丢多少数据，RTO 决定最多能停多久。如果 RPO 接近 0，任务提交成功前就要保证日志事件已经可靠复制；如果 RTO 是分钟级，备用 region、基础设施、密钥、对象存储、日志服务和控制面都不能临时从零搭建，而要提前准备 warm standby 或自动化恢复。

恢复资料要分层准备。第一层是 source of truth，也就是 append-only log、对象存储结果、配置和密钥材料。这些必须有复制、版本化、校验和访问控制。第二层是可重建状态，例如 metadata view、搜索索引、缓存、调度队列，这些可以通过 replay 或重建恢复。第三层是临时状态，例如 worker 本地缓存、模型缓存、in-memory queue，原则上不应成为恢复前提。

备份策略要覆盖数据损坏和误操作。单纯异步复制不能解决“坏数据被快速复制到备用 region”的问题，所以还需要 point-in-time recovery、版本化对象、不可变备份、保留窗口和恢复演练。对象存储里的大结果也要有 checksum 或 content-addressed id，恢复后能确认引用没有指向损坏内容。

DR runbook 要写清楚具体步骤：宣布主 region 不可用，冻结或 fence 原主写入，确认日志复制位置，启动备用控制面和日志服务，恢复 metadata view，验证对象存储引用，开放入口流量，监控错误率和投影 lag，最后处理 failback。runbook 不能只存在文档里，还要定期演练，否则真正故障时会发现权限、脚本、依赖或 DNS 切换都没准备好。

在 LogServe 里，最自然的 DR 方案是围绕 shared log 做。只要已确认的任务事件和大结果对象能恢复，metadata view 就能 replay，调度队列也能从未完成任务重建，worker 可以重新注册并领取任务。当前项目更像机制验证，不是生产级 DR 系统；如果要往生产推进，我会优先补日志复制、对象存储版本化、元数据重建脚本、恢复演练和 RPO/RTO 文档。

面试里可以这样答：

```text
我会先定义故障场景和 RPO/RTO，再决定 DR 策略。
source of truth 包括日志、对象结果、配置和密钥，要有复制、版本化、校验和恢复演练；
metadata view、队列和缓存应当能从日志重建，不能成为恢复前提。
真正的 DR 设计还必须包含 fencing、failover、failback 和定期演练，否则有备份也不等于能恢复。
```
## Q031. RPO 和 RTO 目标如何影响架构？

**回答：**

RPO 和 RTO 会直接决定系统架构，而不是灾备文档里的两个指标。RPO 是最多能接受丢多少数据，RTO 是最多能接受停多久。一个系统如果说 RPO 是 0、RTO 是 5 分钟，它的设计复杂度和成本会远高于 RPO 1 小时、RTO 1 天的系统。

RPO 影响写路径。假设任务提交返回成功后绝不能丢，那么 `TaskSubmitted` 事件在返回客户端之前必须写入可靠介质，并且按照目标复制到足够的故障域。如果只是本机内存队列成功就返回，RPO 实际上接近“进程崩溃就丢”。如果 RPO 要跨区域接近 0，写入可能要等待远端复制或 quorum，这会增加延迟，并让远端故障反过来影响本地写入可用性。

RTO 影响恢复路径。RTO 如果是分钟级，就不能指望故障后临时申请机器、重建数据库、重新配置 DNS、手工恢复密钥。备用控制面、日志服务、对象存储访问、metadata rebuild、入口切流、监控告警都要提前准备，并且恢复脚本要演练过。RTO 如果是小时级或天级，可以更多依赖 backup/restore，但仍要保证备份可用。

两者还会影响哪些状态可以放在内存里。RPO 严格时，已接受任务、租户配额扣减、结果引用、状态转移都不能只存在内存里；RTO 严格时，replay 时间也要受控，不能让 metadata view 从一年日志从头扫起才恢复。通常需要 snapshot、checkpoint、分段日志和可并行 replay。

还要分组件定义，而不是全系统一个数字。任务提交日志可能要求 RPO 0；metrics 丢 5 分钟可以接受；对象存储结果可能要求不能丢，但可以更慢恢复；缓存可以 RPO 无限大，因为丢了可重建。不同数据类别混在一起，会导致架构过度设计或者关键数据保护不足。

在 LogServe 里，如果要求“任务提交成功后不丢”，那 ack 前必须保证 append-only log 已经 fsync 或复制完成；如果要求“控制面挂掉 1 分钟内恢复”，那队列必须能从日志和 metadata 重建，worker lease 要能过期后 redelivery。如果要求跨区域 RTO 很低，则需要备用 log service、对象存储副本和可快速 replay 的 metadata snapshot。当前项目可以展示这些机制，但不能把单机 fsync 说成跨区域 RPO 0。

面试里可以这样答：

```text
RPO 决定写成功前要把数据保护到什么程度，RTO 决定故障后要提前准备多少恢复能力。
RPO 越低，写路径越需要同步持久化、复制或 quorum；
RTO 越低，备用容量、自动化 failover、snapshot 和恢复演练越重要。
我会按数据类别分别定义目标：任务日志最严格，metadata 可重建，缓存可丢，metrics 可以有更宽松窗口。
```

## Q032. 如何估算存储成本、计算成本和网络成本？

**回答：**

成本估算不能只看“机器多少钱”，而要从工作负载模型出发。可靠任务平台的成本通常由计算、存储、网络、请求次数、观测数据和备用容量共同决定。多租户系统还要能把成本按 tenant、workflow、model 或业务线拆出来，否则配额和计费都没有依据。

存储成本可以按数据类别估算。日志成本大致是：每天事件数乘以单事件平均字节数，再乘以保留天数、复制因子和索引开销。对象存储成本是结果对象大小乘以保留时间、版本数量、跨区域副本和生命周期策略。metadata DB 成本包括主表、二级索引、历史记录、snapshot 和备份。观测数据也不能忽略，trace、结构化日志和高基数指标在高吞吐系统里会变成非常大的存储项。

计算成本要拆成控制面和数据面。控制面包括 API、scheduler、log service、metadata projector、quota service、audit service；数据面包括 worker CPU/GPU、任务运行时、模型加载、batch 推理、压缩、校验和对象存储上传下载。估算时不能只算成功任务，还要算失败任务、重试任务、超时任务和 redelivery，因为可靠任务系统为了恢复会产生额外工作。

网络成本包括客户端上传下载、worker 拉取输入、写入结果、跨可用区访问、跨区域复制、日志复制、对象存储复制和观测数据上报。大结果系统尤其容易低估网络成本：任务本身很小，但结果可能很大；客户端反复下载同一个 result_ref，或者 workflow 中多个步骤复制大对象，都会把网络费用放大。

LLM serving 还要单独建模。GPU 小时、模型加载时间、KV/cache 命中率、batch 利用率、token 数、上下文长度、冷启动次数都会影响成本。一个“模型缓存感知 scheduler”的价值，就体现在减少模型反复加载、提高 batch 利用率、减少跨节点数据移动。但如果 benchmark 用的是 mock LLM，就只能证明调度机制，不能直接声称 GPU 成本下降了多少。

估算公式可以先粗略写成：

```text
storage_cost ~= log_bytes_per_day * retention_days * replication_factor
             + result_bytes_per_day * retention_days * version_factor
             + metadata_index_bytes + backup_bytes + observability_bytes

compute_cost ~= control_plane_hours
             + worker_cpu_or_gpu_seconds_per_task * task_count
             + retry_overhead + standby_capacity

network_cost ~= ingress_bytes + egress_bytes
             + cross_az_bytes + cross_region_replication_bytes
             + observability_export_bytes
```

在 LogServe 里，我会用 benchmark 得到任务吞吐、日志字节、metadata 写入次数、结果大小分布和 replay 时间，再代入成本模型。这样回答会比较可信：哪些数字来自实测，哪些是按假设估算，哪些由于没有真实 GPU 或生产对象存储而不能下结论。

面试里可以这样答：

```text
我会从工作负载模型算成本，而不是先猜机器数。
存储看日志、结果对象、metadata、备份和观测数据；计算看控制面、worker、GPU、重试和备用容量；网络看上传下载、跨 AZ、跨 region、对象复制和观测上报。
多租户场景还要按 tenant 打标签，否则无法做配额、计费和 noisy neighbor 分析。
```

## Q033. 如何规划容量和扩容策略？

**回答：**

容量规划的第一步不是加机器，而是定义负载和 SLO。需要知道平均 QPS、峰值 QPS、任务运行时间分布、结果大小分布、租户数量、租户峰值差异、重试率、workflow fan-out、模型种类、GPU/CPU 占比，以及目标 p95/p99 延迟。没有这些输入，扩容策略很容易只是在猜。

然后要找瓶颈。可靠任务平台里可能的瓶颈包括：log append 的 fsync 延迟和 IOPS、metadata DB 的写入 QPS 和索引膨胀、scheduler 扫描队列的复杂度、worker CPU/GPU、对象存储上传下载带宽、结果序列化、控制面锁竞争、租户配额服务、观测系统写入。不同瓶颈对应不同扩容方式，不能一律水平扩 worker。

扩容策略要区分 stateless 和 stateful。API server、普通 worker、projector 通常可以水平扩；日志服务、metadata DB、队列分区、actor placement 这些有状态组件要考虑分片、复制、leader、rebalance 和一致性。对于任务系统，按 tenant、workflow、task type 或 region 分片都可以，但分片键要避免热点租户把一个 shard 打爆。

自动扩容要用合适指标。CPU 利用率适合一部分 worker，但对异步任务平台更关键的是 queue depth、oldest pending age、lease wait time、p95 scheduling latency、GPU utilization、model cache miss、object upload backlog。只看 CPU 可能会错过“队列已经排很久，但 worker 在等待对象存储”的情况。扩容也要有冷却时间和上限，避免重试风暴触发无限扩容。

容量规划还要考虑故障冗余。正常情况下 60% 利用率看起来浪费，但如果一个可用区故障后剩余容量要接住流量，就必须预留 headroom。多租户系统还要给关键租户保留最低容量，不能让整体扩容滞后影响高优先级任务。

在 LogServe 里，可以把单机 benchmark 当成容量模型的起点：测出每秒能 append 多少事件、metadata projection 每秒能处理多少事件、worker 并发提高后 tail latency 怎样变化、replay 100 万事件需要多久。然后再说明生产扩展要做 log sharding、metadata partition、worker autoscaling、按 tenant 的队列隔离和对象存储带宽控制。这样不会把实验结果夸成生产容量。

面试里可以这样答：

```text
我会先定义负载模型和 SLO，再定位瓶颈，然后选择扩容方式。
stateless API 和 worker 可以水平扩；log、metadata、actor placement 这类 stateful 组件要分片、复制和重平衡。
扩容指标不能只看 CPU，还要看 queue depth、oldest pending age、lease wait、projection lag、GPU 利用率和对象存储 backlog。
容量规划还要预留故障 headroom，否则平时够用，故障时就会崩。
```

## Q034. 如何在设计中定位单点故障？

**回答：**

定位单点故障时，我会先画依赖图和故障域，而不是直接列组件名字。对每个节点问三个问题：它挂了，系统是否还能接受新请求？是否还能完成已有任务？是否还能恢复已经确认的数据？如果任一答案是否定的，就要继续判断它是可接受的设计边界，还是必须消除的 SPOF。

常见 SPOF 包括进程、机器、磁盘、可用区、region、负载均衡器、DNS、证书、密钥管理、配置中心、数据库主节点、日志服务、对象存储 bucket、全局锁、全局序列号生成器、单分区队列、单个调度器、单个管理员账号、单条发布流水线、单份 runbook。很多 SPOF 不是代码组件，而是运维路径和权限路径。

对可靠任务系统，最关键的是 source of truth。假如 append-only log 是唯一真相，但 log service 只有单副本磁盘，那它就是最核心 SPOF。假如 metadata DB 挂了可以从日志 replay，那么它不是最终数据 SPOF，但可能是可用性 SPOF。假如对象存储保存大结果，而日志里只有 result_ref，那么对象存储不可恢复会导致结果永久丢失，它也是数据 SPOF。

还要找隐藏 SPOF。比如所有租户共享同一个调度队列，热点租户会让所有任务排队；所有 worker 都依赖一个凭证刷新服务，刷新服务挂掉后新任务都无法访问对象存储；所有跨区域 failover 都需要一个管理员手工点按钮，但这个账号在主 region 的身份系统里。设计评审时要把这些非显性依赖也列出来。

消除 SPOF 的手段包括复制、quorum、leader election、分片、多 AZ、备份、PITR、异步降级、静态配置兜底、手工 break-glass 权限、runbook 和演练。但不是所有 SPOF 都必须立刻消除。面试里要说清楚阶段边界：原型可以接受单节点日志，生产系统不能；低价值离线任务可以接受较长恢复，高价值控制面不能。

在 LogServe 里，当前 logd 单进程、metadata DB、对象存储实现和控制面都可以被指出为实验阶段 SPOF。项目亮点不是“没有 SPOF”，而是通过 log-first、幂等完成、lease/redelivery、metadata replay 证明了哪些状态可以恢复。生产化要把 log service 做复制或使用可靠存储，把 control plane 做多副本，把 object store 和 metadata 做备份/复制，并有恢复演练。

面试里可以这样答：

```text
我会画依赖图和故障域，逐个问：它挂了还能接新请求吗、还能完成已有任务吗、已确认数据还能恢复吗。
对任务系统，log、metadata、object store、scheduler、worker pool、DNS、配置、密钥和发布系统都要检查。
还要找隐藏 SPOF，比如全局锁、单分区队列、单个凭证服务和只能由一个人执行的 runbook。
原型可以说明边界，生产系统则要用复制、分片、备份、failover 和演练消除关键 SPOF。
```

## Q035. 如何设计审计能力？

**回答：**

审计能力要回答的是：谁在什么时间、从哪里、对什么资源、做了什么操作、结果如何、影响了哪些状态。它和普通 debug log 不一样。debug log 面向排障，可以比较嘈杂；audit log 面向追责、合规和安全调查，必须结构化、可检索、保留策略明确，而且不能被普通业务路径轻易篡改。

我会把审计事件设计成独立的 append-only 流，至少包含 `audit_event_id`、`tenant_id`、`actor_type`、`actor_id`、`source_ip`、`user_agent`、`request_id`、`trace_id`、`operation`、`resource_type`、`resource_id`、`decision`、`reason`、`occurred_at`、`control_plane_version`。对任务系统，还可以加 `task_id`、`workflow_id`、`worker_id`、`lease_epoch`、`result_ref`。

需要审计的操作不只是登录和管理员操作。任务提交、取消、重试、强制完成、结果读取、对象删除、配额修改、租户权限修改、worker 注册、模型注册、密钥轮换、schema 变更、feature flag 修改、跨区域 failover 都应该留下审计记录。尤其是“谁读取了大结果”和“谁修改了配额/权限”，在多租户平台里非常关键。

审计日志要防篡改。可以写入独立存储、启用对象锁或不可变保留、定期做 hash chain 或签名、限制删除权限，并把读写权限和业务管理员权限分开。审计系统本身也要被审计，否则管理员可以修改审计配置而不留痕。

同时要注意隐私和数据最小化。审计记录不应该直接存明文 payload、密钥、token、用户敏感内容或大结果内容。可以记录 hash、大小、MIME、result_ref、权限决策和访问结果。需要保留的字段要有明确 retention；涉及合规删除时，也要知道哪些字段能删、哪些字段因安全或法律要求必须保留。

在 LogServe 里，任务状态事件有一部分审计价值，但不能完全替代审计流。比如 `TaskCompleted` 能证明状态发生了变化，但未必说明哪个用户触发了取消、哪个管理员改了租户配额、哪个客户端读取了结果。因此生产设计里我会保留业务事件日志作为 source of truth，同时加一条独立 audit stream，用 request_id 和 trace_id 关联起来。

面试里可以这样答：

```text
审计日志要回答谁、何时、从哪里、对什么资源、做了什么、结果是什么。
我会把它设计成独立的 append-only 流，记录 tenant、actor、operation、resource、decision、trace_id 和关键业务 id。
它要和 debug log 分开，具备不可篡改、权限隔离、保留策略和隐私脱敏。
任务状态事件有审计价值，但不能替代完整审计流，因为权限变更、结果读取和管理员操作也必须被记录。
```
## Q036. 如何设计可观测性和告警？

**回答：**

可观测性要服务两个目标：故障时能定位问题，平时能判断系统是否正在逼近容量或可靠性边界。对可靠任务平台来说，只收集普通 HTTP 延迟和错误率不够，因为很多问题发生在异步路径、队列、lease、replay、对象存储和 worker 执行阶段。

我会用 metrics、logs、traces 三类信号。metrics 用来做聚合判断和告警，例如 QPS、错误率、p95/p99 latency、queue depth、oldest pending age、lease expiration count、redelivery count、completion conflict count、projection lag、log append latency、fsync latency、metadata write latency、object store 5xx、worker heartbeat age、worker busy ratio、model cache hit rate。logs 用来记录状态变化、拒绝原因、异常上下文和审计线索。traces 用来串起 submit、schedule、lease、execute、object upload、complete、metadata projection、client polling 的完整链路。

告警要围绕用户影响和 SLO，而不是每个内部错误都 paging。比如“任务提交 5 分钟错误预算消耗过快”“oldest pending age 超过 SLO”“projection lag 持续增长”“log append p99 超过阈值”“某租户被限流比例异常”“worker heartbeat 大面积过期”这些适合报警。单个 worker 重启、短暂对象存储重试、少量任务失败，可以进入工单或 dashboard，不一定叫醒人。

异步系统尤其要关注滞后指标。同步 API 可能返回正常，但后台队列已经堆积；metadata view 可能可读，但 projection lag 已经落后十分钟；worker 看起来在线，但 lease completion 冲突大量增加；对象存储上传重试被 SDK 吸收，客户端只看到任务变慢。告警要覆盖这些“还没变成 5xx，但已经在积累风险”的信号。

指标标签要控制基数。tenant_id、task_type、region、status、model、queue 可以作为指标维度，但 task_id、result_ref、user_id 这类高基数字段通常放到 logs 或 traces 里，不适合直接作为 metrics label。否则可观测性系统本身会被打爆，甚至成为 noisy neighbor 的来源。

在 LogServe 中，我会把 trace_id 从任务提交一直传到 worker 执行和完成回写；每次状态转换写结构化日志；关键指标包括 log append latency、task state transition count、lease timeout、redelivery、replay duration、projection lag、worker heartbeat age、object result write latency 和 benchmark throughput。当前项目可以展示 dashboard 和恢复路径指标，但生产还需要告警规则、on-call runbook、错误预算和合成探测。

面试里可以这样答：

```text
我会用 metrics、logs、traces 三类信号覆盖同步 API 和异步执行链路。
对任务平台，除了 latency、traffic、errors、saturation，还要看 queue depth、oldest pending age、lease 过期、redelivery、projection lag、log append p99、object store 5xx、worker heartbeat 和 model cache hit rate。
告警围绕 SLO 和用户影响设计，避免每个内部错误都 paging；高基数字段放 logs/traces，不直接做 metrics label。
```

## Q037. 如何设计 benchmark 证明系统能力？

**回答：**

benchmark 先要证明一个明确主张，而不是跑一个好看的吞吐数字。比如要证明可靠任务系统能力，可以分别验证任务提交吞吐、调度延迟、worker 并发能力、log append 延迟、metadata replay 速度、故障恢复时间、对象存储大结果吞吐、LLM scheduler 的缓存命中收益。每个主张都要对应指标和实验设计。

实验环境要可复现。需要记录硬件、CPU、内存、磁盘类型、网络、操作系统、Go 版本、依赖版本、配置参数、fsync 策略、worker 数量、任务 payload 大小、结果大小分布、租户数量、任务类型、是否启用真实对象存储、是否启用真实 LLM/GPU。没有这些上下文，benchmark 数字很难比较，也很难被别人复现。

指标要覆盖吞吐和尾延迟。吞吐可以看 tasks/s、events/s、bytes/s；延迟要看 p50、p95、p99，而不是只看平均值；资源要看 CPU、内存、磁盘 I/O、网络、goroutine、GC、allocation；异步路径要看 queue wait、schedule latency、execute time、completion latency、projection lag；可靠性要看 retry、redelivery、duplicate completion、illegal transition count。

还要做 baseline 和 ablation。比如比较“普通 FIFO 调度”和“模型缓存感知调度”；比较“每事件 fsync”和“batch fsync”；比较“无 snapshot 从头 replay”和“snapshot + 增量 replay”；比较“单租户负载”和“多租户 noisy neighbor 负载”。没有对照组，就很难说明系统设计到底带来了什么收益。

benchmark 不能只跑 happy path。可靠任务平台必须包含失败场景：worker kill、控制面重启、日志服务短暂不可用、metadata DB 慢查询、对象存储 5xx、任务超时、重复完成。可以把故障注入和 benchmark 结合，观察恢复过程中吞吐、尾延迟和数据正确性是否仍然满足目标。

在 LogServe 中，我会把 benchmark 结论表述为机制证明：例如单机环境下 append-only log 能支撑多少事件写入，replay 能在多少时间内重建 metadata，lease/redelivery 在 worker 崩溃后能否恢复任务，模型缓存调度在模拟负载下是否减少冷启动。没有真实多 region、真实生产对象存储和真实 GPU 时，就不应该声称生产规模或 GPU 成本收益已经被证明。

面试里可以这样答：

```text
我会先定义 benchmark 要证明的主张，再设计指标和对照组。
指标包括吞吐、p95/p99、queue wait、projection lag、resource usage、retry/redelivery 和恢复时间。
实验要记录硬件、版本、配置、数据规模和 workload，并做 baseline、ablation、多次运行和失败场景。
对 LogServe 这类项目，我会说 benchmark 证明机制能力，不夸大成生产规模证明。
```

## Q038. 如何设计 fault injection 证明恢复能力？

**回答：**

fault injection 的目标不是制造混乱，而是验证系统在预期故障下是否仍然满足不变量。对可靠任务平台来说，不变量通常包括：已确认提交的任务不能丢；任务状态不能非法跳转；同一个 attempt 的完成只能生效一次；lease 过期后可以 redelivery；metadata view replay 后和在线状态一致；大结果引用不能指向不存在或损坏对象；控制面重启不能丢失 source of truth。

我会先建立 failure matrix。进程类故障包括 worker kill、control plane restart、log service restart、projector restart；存储类故障包括 metadata DB unavailable、object store 5xx、磁盘慢写、日志尾部损坏；网络类故障包括超时、断连、重复请求、乱序完成；协议类故障包括旧 worker 上报新字段缺失、重复 completion、stale lease、非法状态转换；资源类故障包括 CPU 饱和、队列堆积、模型加载失败。

注入方式要尽量可控。单元测试里可以用 fake clock、fake storage、forced error、deterministic scheduler；集成测试里可以 kill 进程、暂停网络、让对象存储返回 503、让 metadata DB 写入超时；演练环境里可以做更接近真实的 chaos test。每个故障都要有 expected outcome，而不是只看进程最后还在不在。

故障注入要检查恢复后状态。比如 worker 执行成功但完成回写失败，系统应该允许同 attempt 或新 attempt 幂等完成，不能永远卡在 running；控制面重启后，pending/running 任务应该从日志和 lease 恢复；对象存储不可用时，小结果可以走降级策略，大结果任务应进入 retryable failure，而不是写入一个假 result_ref；日志尾部损坏时，应能截断到最后一个完整记录并重放。

还要覆盖组合故障，但顺序要控制。先验证单故障，再做组合故障，比如“worker 完成后网络断开 + control plane 重启”“object store 503 + retry storm”“metadata DB 不可用 + projection lag 增长”。组合故障容易产生误判，所以要先有清晰的不变量和观测指标。

在 LogServe 中，fault injection 可以围绕 worker、control plane、logd、metadata view、object store mock 来做。好的展示方式不是说“我做了容错”，而是列出几个具体用例：kill worker 后 lease 到期 redelivery；kill control plane 后从日志重建队列；让 completion 重复提交时只有一次状态转换生效；让 metadata DB 暂时不可用时日志继续保留 source of truth，恢复后投影追上。

面试里可以这样答：

```text
我会先定义恢复不变量，再按 failure matrix 注入故障。
故障包括 worker kill、control plane restart、log service restart、metadata DB 不可用、object store 503、网络超时、重复 completion、stale lease 和日志尾部损坏。
每个测试都要断言恢复结果：任务不丢、状态不非法、完成幂等、lease 可重投递、metadata replay 一致。
只制造故障但不检查不变量，不能证明恢复能力。
```

## Q039. 如何评估系统是否已经达到生产可用？

**回答：**

生产可用不是“demo 跑通”，也不是“压测数字好看”。我会从可靠性、安全性、数据保护、可运维性、可升级性、多租户治理和成本控制几个维度评估。只要其中一个关键维度缺失，就只能说是原型或内部试用，不能说生产可用。

可靠性方面，要有明确 SLO、错误预算、容量模型、限流降级、重试和幂等、故障注入结果、恢复演练、RPO/RTO 目标。数据保护方面，要证明 source of truth 持久可靠，备份和 restore 真的跑通过，日志和对象存储有校验，metadata view 可重建，误删和数据损坏有恢复办法。

安全方面，要有认证、授权、租户隔离、密钥管理、最小权限、审计日志、敏感数据脱敏、供应链和镜像安全。多租户系统还要验证 noisy neighbor、配额、限流、对象存储隔离、观测数据隔离和管理员权限边界。没有这些，系统即使功能正确，也可能不能给外部租户使用。

可运维性方面，要有 dashboard、告警、runbook、on-call 流程、发布回滚、schema migration、版本兼容、容量扩容、成本监控、故障复盘流程。生产系统要能在凌晨出问题时让值班人定位和止血，而不是依赖作者现场读代码。

可升级性方面，要能滚动升级、灰度发布、回滚，旧 worker 和新 control plane 能共存，事件格式兼容，数据库迁移可逆或有明确前滚策略。对于 event sourcing 系统，还必须有历史日志 replay 测试，防止新版本把旧事件读坏。

如果评价 LogServe，我会说它可以作为机制验证和面试项目亮点，但还不是生产平台。它展示了 shared log、可靠任务状态机、lease/redelivery、metadata replay、workflow/actor 思路和 LLM scheduler 的设计能力；但生产化还需要 replicated log、HA control plane、真实对象存储和 metadata 运维、强多租户隔离、安全审计、真实 GPU benchmark、跨区域 DR 和完整 on-call 体系。这样回答更可信，因为它既说清价值，也没有夸大边界。

面试里可以这样答：

```text
我会用生产 readiness checklist，而不是用 demo 成功来判断。
关键维度包括 SLO、RPO/RTO、备份恢复、故障注入、容量、观测告警、安全、审计、多租户隔离、升级回滚、成本和 on-call runbook。
对 LogServe，我会明确说它证明了可靠任务和 replay 机制，但生产化还需要复制日志、HA 控制面、强隔离、真实对象存储/GPU benchmark 和完整运维体系。
```

## Q040. 如果只能保留一个复杂模块作为项目亮点，你会选择哪一个技术主题来讲？

**回答：**

如果只能保留一个复杂模块，我会选择“以 shared append-only log 作为 source of truth，并基于 replay 构建可靠任务、workflow 和 actor 状态”这个主题。原因是它能把项目里最有系统设计含量的部分串起来：可靠任务执行、幂等完成、lease/redelivery、状态机合法性、metadata view、控制面恢复、workflow 编排、actor 状态恢复、大结果引用和审计线索，都可以围绕同一个核心机制展开。

我不会优先选择“LLM scheduler”作为唯一亮点，除非面试官明确是 AI infra 方向。LLM scheduler 很有吸引力，但如果没有真实 GPU、真实模型负载和生产集群数据，很容易被追问到证据边界。shared log + replay 这个主题更扎实，因为它可以用代码、状态机、故障注入和 replay 测试来证明，而且能自然讨论分布式系统里的正确性问题。

讲这个亮点时，我会按四层展开。

第一层是问题：异步任务平台最难的不是把任务放进队列，而是处理 worker 崩溃、控制面重启、完成回写失败、重复执行、重复完成、metadata 落后、结果很大、升级兼容这些边界情况。

第二层是设计：所有关键状态转换先进入 append-only log，日志是 source of truth；metadata view 和调度队列是从日志投影出来的派生状态；worker 通过 lease 获取任务，完成时带 attempt_id 和 lease_epoch；大结果只在日志里存 result_ref；控制面重启后通过 replay 重建状态。

第三层是正确性：任务状态机限制非法转换；completion 用 CAS 或条件更新保证只有一个完成生效；lease 过期后允许 redelivery；重复 completion 通过幂等 key 或 completion token 去重；metadata DB 不可用时不丢 source of truth；对象存储不可用时不写假引用。

第四层是验证：用 benchmark 证明 append、replay、调度和恢复性能；用 fault injection 证明 kill worker、control plane restart、重复 completion、metadata DB 不可用时不变量仍成立；用历史事件 replay 测试证明事件格式升级兼容。这样讲，项目不只是功能堆叠，而是有清楚的可靠性主线。

最后要主动讲边界。这个模块不是要声称自己替代 Kafka、Temporal 或生产级数据库，而是证明自己理解可靠任务系统的关键机制，并用一个可运行项目把这些机制连起来。生产化还需要复制日志、跨区域 DR、安全审计、多租户隔离和真实 workload 验证。主动说边界通常比夸大项目更有说服力。

面试里可以这样答：

```text
我会选择 shared append-only log + replayable state 作为唯一亮点。
因为它能统一解释可靠任务执行、状态机、lease/redelivery、幂等完成、workflow、actor、metadata view 和控制面恢复。
LLM scheduler 可以作为延伸，但如果只能留一个主题，我会讲 log-first 的可靠性主线：关键状态先写日志，派生状态可重建，worker 通过 lease 执行，完成用 attempt/epoch 保证幂等，故障后通过 replay 恢复。
同时我会明确边界：这是机制验证，不是声称替代 Kafka 或 Temporal；生产化还需要复制、HA、DR、多租户隔离和真实负载验证。
```
## Q041. 可靠任务执行的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

可靠任务执行首先解决的是正确性问题。它要保证一件事：只要平台已经接受了任务，就不能因为 worker 崩溃、控制面重启、网络超时或客户端重试而让任务变成一笔说不清的账。任务可以失败，可以重试，可以被取消，也可以进入人工处理，但状态必须可解释，结果提交必须有规则，恢复后不能靠猜。

我会把核心目标拆成几条不变量：

1. 已确认提交的任务不能悄悄丢失。客户端拿到成功响应后，任务规格、幂等键、租户、重试策略和初始状态必须已经进入 durable storage 或 append-only log。
2. 状态转换要合法。任务不能从 `SUCCEEDED` 又回到 `RUNNING`，不能由已经失效的 worker 写入完成结果，也不能在没有 lease 的情况下被标记为正在运行。
3. 执行可以至少一次，完成要尽量幂等。worker 可能重复执行同一任务，但控制面要让最终结果只被合法提交一次。对外部副作用，则要靠业务 idempotency key、outbox 或补偿逻辑控制。
4. 崩溃后能恢复。控制面重启后，要能从日志或数据库重建 queued、running、terminal、retry_wait、dead_letter 等状态。
5. 观察结果要可信。客户端查询状态时，不能看到互相矛盾的状态；运维人员排障时，能追到任务为什么重试、谁执行、哪个 attempt 生效。

性能当然重要，但它不是可靠任务执行的第一性目标。一个任务平台吞吐很高，却会丢任务或重复写结果，这不是可靠系统。更合理的说法是：先定义正确性语义，再在这个语义范围内优化性能。比如 batch fsync、批量调度、worker poll 长轮询、metadata cache 都是优化；它们不能破坏“已确认任务不丢”和“完成提交幂等”。

安全性也相关，但通常不是这个模块单独解决的全部问题。可靠任务执行会带 tenant_id、权限校验、审计、结果引用权限，这些是多租户平台必须要有的。但认证、授权、密钥、网络隔离、供应链安全属于更大的平台安全设计。

可维护性是结果，不是核心目标。清晰状态机、append-only event、幂等 API、可 replay 的 metadata view 会让系统更容易维护，因为故障时可以从事实来源解释状态。但这些设计首先是为了正确性和恢复性服务的。

结合 LogServe，我会说：这个项目里可靠任务执行的主线是 log-first control plane。`TaskSubmitted`、`TaskStarted`、`TaskCompleted` 这类事件先进入 shared log，再投影成 metadata view。worker 可以被 kill，control 可以重启，队列可以从日志恢复。项目目前验证的是单机/多进程下的机制，不会声称已经达到生产级分布式 exactly-once。

面试里可以这样答：

```text
可靠任务执行首先解决正确性和恢复性。它要保证已接受任务不丢、状态转换合法、重复执行不会重复提交最终结果、控制面重启后能恢复。
性能、安全和可维护性都重要，但它们围绕这个语义展开。性能优化不能破坏持久化和幂等；安全要结合租户隔离和审计；可维护性来自清晰状态机和可 replay 的事实来源。
```

## Q042. 可靠任务执行的典型适用场景和不适用场景分别是什么？

**回答：**

可靠任务执行适合那些“可以异步完成，但不能丢、不能说不清状态”的工作。典型例子包括文件转码、报表生成、批处理导入、数据清洗、搜索索引构建、ML/LLM 推理任务、workflow step、邮件或通知发送、备份任务、跨系统同步、长时间外部 API 调用。这类任务的共同点是：执行时间可能比较长，失败可能来自外部依赖，客户端不应该一直挂着等待，系统需要记录状态和结果。

它也适合有恢复要求的内部平台。比如一个 workflow 有多个 step，其中某一步失败后只重试这一步；一个 actor command 需要按顺序应用；一个 LLM 任务需要调度到缓存命中的 worker；一个大结果需要写对象存储并留下 result_ref。这些场景都要求平台清楚地区分“任务已接受”“已调度”“正在执行”“结果已提交”。

不适用场景也要讲清楚。第一类是不需要持久语义的超短本地任务。比如一次 HTTP 请求里计算一个简单函数，失败后让客户端重试即可；硬塞进任务平台反而增加排队、持久化和调度开销。

第二类是强实时或微秒级延迟路径。可靠任务系统通常会写日志、查 metadata、拿 lease、做调度，这些步骤带来固定成本。交易撮合、内核数据面、实时控制环这类路径更适合专门的低延迟架构，而不是通用异步任务平台。

第三类是无法幂等、无法补偿、又不能接受重复执行的外部副作用。比如直接扣款、直接给用户发不可撤销凭证、直接操作第三方系统。如果外部系统没有 idempotency key，也没有查询/撤销/补偿接口，可靠任务平台只能减少重复提交的概率，不能凭空保证严格 exactly once。

第四类是本质上需要持续流处理的负载。可靠任务平台可以处理离散任务，但如果数据是无限流、需要窗口计算、watermark、backpressure 和状态算子，那么 Flink、Kafka Streams 这类流处理系统更贴近问题。任务平台可以提交流处理作业，但不一定适合自己实现流计算语义。

LogServe 的适用边界也类似。它适合展示 workflow step、actor command、LLM request、大结果引用、重试和恢复，不适合被描述成低延迟交易系统、生产级多区域任务云，或者严格 exactly-once 外部副作用平台。把边界讲清楚，反而更像真实工程判断。

面试里可以这样答：

```text
可靠任务执行适合长耗时、可异步、需要状态可查和失败恢复的工作，例如转码、批处理、workflow step、LLM 推理、索引构建和跨系统同步。
它不适合微秒级低延迟路径、简单本地计算、无法幂等也无法补偿的外部副作用，以及需要完整流处理语义的无限数据流。
关键判断标准是：任务是否需要持久状态、重试、幂等完成和恢复解释。如果不需要，这个平台可能太重；如果需要，就不能只靠内存队列。
```

## Q043. 可靠任务执行和相近概念最容易混淆的边界在哪里？

**回答：**

最容易混淆的是“消息队列”和“任务执行平台”。消息队列负责把消息从 producer 交给 consumer，提供 ack、nack、重投递、顺序或分区等能力。可靠任务执行还要管理任务规格、状态机、attempt、lease、超时、结果、重试策略、幂等完成、审计和查询接口。队列可以是任务平台的底层组件，但队列本身不等于任务平台。

第二个边界是“任务执行”和“workflow engine”。单个任务通常是一段可重试的工作；workflow 关心多个 step 之间的依赖、条件分支、补偿、长时间等待、信号和 replay。可靠任务执行是 workflow engine 的基础能力之一，但 workflow 还需要更高层的编排语义。LogServe 里 workflow DAG step 最终还是落到任务和事件上，但 DAG 依赖、step 幂等和 workflow replay 是额外逻辑。

第三个边界是“任务平台”和“定时调度器”。cron 或 scheduler 关注某个时间点触发任务；可靠任务执行关注任务被触发后如何持久化、执行、重试和完成。定时调度可以把任务提交进可靠任务系统，但只会按时间触发，不会自动解决 worker 崩溃后的幂等结果提交。

第四个边界是“任务执行”和“actor”。actor 强调某个有状态对象的串行消息处理和所有权，可靠任务强调离散工作的执行与完成。actor command 可以被实现成一种特殊任务，但它还要求 mailbox 顺序、command_seq、snapshot 和 epoch fencing。把 actor 当普通任务并发跑，很容易把状态写坏。

第五个边界是“可靠执行”和“严格 exactly-once”。很多系统实际提供的是 at-least-once execution 加幂等结果提交。worker 可能执行两次，外部 API 可能收到两次请求，只是最终状态只有一次生效。严格 exactly-once 需要把所有副作用都纳入同一个事务边界，现实里经常做不到。面试中如果不区分这两者，很容易被追问支付、邮件、第三方 API 后露出问题。

第六个边界是“append-only log”和“任务队列”。日志保存事实，队列提供可运行项视图。日志通常保留历史，可以 replay；队列通常关心当前待消费集合。可靠任务系统可以从日志投影出队列，但不能把内存队列当作唯一事实来源。

面试里可以这样答：

```text
我会把边界分清：消息队列负责投递，任务平台负责任务状态、lease、attempt、重试、结果和查询；workflow engine 在任务之上处理依赖和 replay；定时调度器只负责触发时间；actor 强调有状态对象的串行命令；append-only log 是事实来源，不是内存队列。
最重要的边界是不要把 at-least-once execution 说成严格 exactly-once。真实系统通常靠幂等完成和业务侧 idempotency key 控制重复副作用。
```

## Q044. 可靠任务执行在高并发场景下可能出现哪些隐藏问题？

**回答：**

高并发下的第一个隐藏问题是状态竞争。多个 worker 可能同时拿到同一任务，或者一个 worker lease 已经过期但仍然回写完成，另一个 worker 已经开始新的 attempt。如果完成回写没有 `attempt_id`、`lease_epoch` 或 CAS 条件，旧结果就可能覆盖新结果。

第二个问题是队列热点。很多系统一开始用一个全局 pending queue，看起来简单，负载一高就会出现锁竞争、扫描成本上升、单租户占满队列、优先级反转。多租户场景下还会出现 noisy neighbor：一个租户的大量任务把其他租户的 pending age 拉高，但全局平均延迟还不一定明显。

第三个问题是重试放大。外部依赖短暂故障时，如果所有任务立刻重试，会把依赖打得更差，也会让 worker pool 被失败任务占满。指数退避、最大尝试次数、jitter、按错误类型区分是否重试、dead letter 都不是装饰，它们是高并发下防止雪崩的基本手段。

第四个问题是幂等表或去重索引变成瓶颈。每次提交都查 idempotency key，每次完成都做条件更新，这些操作如果集中在一个表、一个索引或一个锁上，会成为吞吐上限。解决方式可能是按 tenant 或 shard 分区、减少热路径扫描、用 append log 加异步 projection，但语义必须保持一致。

第五个问题是观测系统被高基数打爆。任务系统天然有 task_id、attempt_id、workflow_id、result_ref。如果这些字段直接作为 metrics label，指标系统可能比业务系统先出问题。高基数字段应该进日志和 trace，不要随手放进聚合指标。

第六个问题是结果存储和完成路径耦合。大结果上传慢时，worker 线程可能被对象存储阻塞；对象存储 5xx 又可能导致任务重复执行，进一步增加网络和存储压力。比较稳的做法是大结果先写对象存储并校验，再用 result_ref 幂等完成；上传失败时任务仍在可解释状态，而不是半成功。

第七个问题是控制面恢复时间被日志规模拖垮。高并发系统每天产生大量事件，如果每次重启都从头 replay，RTO 会越来越差。需要 snapshot、checkpoint、分段 replay、按 stream 恢复和 projection lag 监控。

在 LogServe 中，这些问题可以对应到 shared log append、metadata projection、worker poll、lease/redelivery、LLM model cache 统计和 result store。当前 benchmark 是单机实验，可以说明机制和瓶颈方向，但不能直接外推到多节点生产吞吐。

面试里可以这样答：

```text
高并发下隐藏问题主要在状态竞争、队列热点、重试放大、幂等索引瓶颈、对象存储阻塞、观测高基数和 replay 变慢。
我会用 attempt/lease_epoch/CAS 防 stale completion，用租户队列和公平调度防 noisy neighbor，用退避和 jitter 防重试风暴，用 snapshot 控制恢复时间。
吞吐优化要围绕这些正确性约束做，不能为了快把 lease 和幂等条件去掉。
```

## Q045. 可靠任务执行在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

这些场景会把任务系统里的模糊语义全部逼出来。最典型的边界条件是：任务执行成功了，但完成回写失败。worker 可能已经把结果写到对象存储，也可能已经调用了外部 API，但控制面没有收到 `TaskCompleted`。这时不能简单地重新执行整个任务，否则外部副作用可能重复；也不能直接标记成功，因为缺少可信完成事件。比较稳的设计是结果写入带 content hash 或 idempotency key，完成回写可以重试，控制面用 attempt 和 result_ref 做幂等提交。

第二个边界是完成回写成功，但客户端超时。客户端不知道任务是否成功，如果再次提交相同请求，平台应该通过 idempotency key 返回已有任务或已有结果；如果 payload 不一致，要报冲突。这也是为什么幂等键不能只是可选优化，而是 API 语义的一部分。

第三个边界是 worker 崩溃。控制面看到 lease 或 heartbeat 过期后，可以把任务重新投递。但旧 worker 如果只是网络分区，可能过一会儿又回来提交完成。因此完成路径必须校验 lease_epoch，不能只看 task_id。

第四个边界是控制面重启。如果 pending/running 队列只在内存里，重启后任务就没了。可靠系统要从数据库或 append-only log 重建：terminal 任务保持终态；queued 任务重新入队；running 任务如果 lease 未过期可以等待，过期后 redelivery；retry_wait 任务按 next_retry_at 恢复。

第五个边界是超时和取消并发。用户取消任务时，worker 可能正在完成；超时触发重试时，旧 attempt 可能马上返回成功。状态机必须定义谁赢。常见规则是终态通过条件更新产生，取消和完成按版本或事件顺序决定，已经生效的终态不能被后来的普通事件覆盖。

第六个边界是重试错误分类。网络超时、对象存储 503、临时数据库连接失败可以重试；参数非法、权限不足、payload schema 错误通常不应该无限重试。把永久错误当临时错误，会产生成本浪费和队列堆积；把临时错误当永久错误，会降低成功率。

第七个边界是日志尾部损坏或 metadata 落后。log service 重启时要能识别半条记录并截断；metadata view 落后时，查询结果可能暂时不是最新，但不能制造从未发生过的状态。LogServe 的 log-first 设计就是为了让控制面重启后有一个可 replay 的事实来源。

面试里可以这样答：

```text
这些场景会暴露几类边界：执行成功但完成回写失败、完成成功但客户端超时、worker 崩溃后旧 completion 又回来、控制面重启丢内存队列、取消和完成并发、超时和重试并发、永久错误被无限重试。
我的处理原则是：提交和完成都带幂等键，worker 执行权用 lease_epoch fencing，控制面从 durable log/db 恢复，终态用条件更新保护，错误按可重试和不可重试分类。
```
## Q046. 可靠任务执行的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

可靠任务执行的瓶颈很少只有一个来源。它通常是一条链路：提交写入事实来源，调度读取可运行任务，worker 执行，结果写对象存储，完成回写状态，metadata view 更新，客户端查询。哪一段最慢，取决于任务类型和系统实现。

如果任务本身很重，比如视频转码、模型推理、压缩、数据清洗，那么瓶颈可能在 worker CPU 或 GPU。此时控制面吞吐不一定是问题，关键是 worker 利用率、并发数、资源隔离、任务粒度和调度策略。LLM 场景还要看模型加载、checkpoint fetch、KV/cache 命中率和 batch 利用率。

如果任务本身很轻，瓶颈往往转到控制面和存储。每个任务都要写 `Submitted/Started/Completed`，再更新 metadata，可能还要查幂等键、租户配额、重试策略和 worker 状态。这里最常见的瓶颈是 I/O：日志 append 的 fsync、metadata DB 写入、索引更新、对象存储请求。PostgreSQL WAL 这类设计之所以强调顺序写，就是因为顺序写和合并 fsync 能显著降低小事务的持久化成本。

锁竞争也很常见。单个全局队列锁、全局调度器锁、全局 worker map、单个租户配额锁、单个 idempotency key 表热点，都可能在高并发下变成上限。很多任务系统低并发时看不出问题，一到大量短任务就卡在锁上，而不是卡在业务执行上。

内存瓶颈通常来自队列堆积、任务 payload 过大、result 误放 metadata、replay 时一次性加载太多事件、metrics label 高基数、worker 本地缓存无限增长。可靠任务系统应该让大 payload 和大结果走对象存储，日志和 metadata 只放引用、checksum、size、content type。否则内存和 GC 会被很快拖垮。

网络瓶颈常见于跨节点 worker、对象存储、大结果下载、跨区域复制和观测数据导出。异步任务平台经常低估网络，因为任务规格很小，但结果对象很大；或者 task fan-out 后多个 step 反复拉取同一份输入。网络还会放大 tail latency：对象存储偶发慢请求会阻塞 worker，进而触发超时和重试。

LogServe 的实验数据也能说明这种差异。shared log benchmark 中，`always fsync` 和 `batch/interval fsync` 的吞吐差距很大，说明日志持久化策略直接影响 append 性能；task throughput 和 workflow latency 则受 worker 数、mock LLM、控制面调度和 result path 共同影响。回答时要把“我测到了什么”和“生产里还需要测什么”分开。

面试里可以这样答：

```text
可靠任务执行的瓶颈取决于任务粒度。
重任务通常瓶颈在 worker CPU/GPU、模型加载和外部依赖；轻任务常卡在控制面、日志 fsync、metadata 写入、幂等索引和锁竞争；大结果场景还会卡对象存储和网络。
我会用分段指标定位：submit latency、schedule latency、queue wait、execute time、result upload、completion commit、projection lag，而不是只看整体平均延迟。
```

## Q047. 可靠任务执行的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

这三类测试的目的不同。correctness test 证明语义正确，stress test 找并发和恢复边界，benchmark 量化性能。把它们混在一起，会得到很漂亮但没有解释力的测试结果。

correctness test 要测不变量。比如重复提交同一个 idempotency key 且 payload 相同时返回同一任务；同 key 不同 payload 报冲突；没有有效 lease 的 completion 被拒绝；旧 lease_epoch 的 completion 不能覆盖新 attempt；终态不能被普通事件改写；任务失败后只有在 `attempt < max_attempts` 时进入 retry；控制面 replay 后状态和在线执行一致。这里更像状态机测试和 API 语义测试，不需要很高并发。

stress test 要制造并发冲突和故障。比如 100 个 worker 同时 poll，多个 worker 争同一个任务，worker 在完成前后被 kill，control plane 重启，metadata DB 短暂不可用，object store 503，客户端提交成功后断线重试，旧 worker 在 lease 过期后提交结果。stress test 的重点是系统不能出现非法状态、任务丢失、重复终态、无限重试风暴或死锁。

benchmark 要测可量化指标。典型指标包括 submit throughput、schedule throughput、tasks/s、events/s、p50/p95/p99 latency、queue wait、completion commit latency、projection lag、replay time、CPU、内存、磁盘 I/O、网络、分配次数和 GC。benchmark 必须记录硬件、配置、worker 数量、任务大小、结果大小、fsync 策略和租户分布，否则数字没法比较。

还要做对照实验。比如无 lease 和有 lease 的正确性差异，FIFO 和 fair queue 的多租户延迟差异，always fsync 和 batch fsync 的吞吐差异，full replay 和 snapshot replay 的恢复时间差异，resource-only scheduler 和 locality-aware scheduler 的缓存命中差异。对照组能说明设计选择的收益，不只是说明系统能跑。

在 LogServe 中，correctness 可以围绕 task state、workflow step 幂等、actor epoch fencing、result_ref、replay 一致性写测试；stress 可以围绕 worker kill、queue redelivery、control restart、重复 completion；benchmark 则报告 task throughput、workflow p95/p99、log append records/s、actor replay command 数和 locality-aware scheduler 的 cache hit。要明确这些是单机 mock LLM 环境的结果。

面试里可以这样答：

```text
correctness test 测语义不变量：幂等提交、lease fencing、合法状态机、终态保护、replay 一致。
stress test 测并发和故障：多 worker 抢任务、worker kill、control restart、网络超时、重复 completion、依赖不可用。
benchmark 测性能：吞吐、p95/p99、queue wait、projection lag、replay time、CPU/内存/I/O/网络，并记录硬件和配置。
三者不能互相替代；压测跑通不等于语义正确。
```

## Q048. 如果要求从零实现一个简化版可靠任务执行，你会先定义哪些不变量？

**回答：**

我会先定义不变量，再写 API 和数据结构。任务系统一旦进入并发、重试和恢复场景，靠“流程大概是这样”很快会出问题。不变量写清楚，后面的状态机、存储表、日志事件、测试都能围绕它展开。

第一条是提交不变量。只要 `SubmitTask` 返回成功，任务规格必须已经持久化，并且能通过 `task_id` 或 `idempotency_key` 找回。相同幂等键和相同 payload 返回同一个任务；相同幂等键但 payload 不同返回冲突。

第二条是状态机不变量。任务只能沿允许的边转换，终态不可被普通事件覆盖。比如 `SUBMITTED -> QUEUED -> RUNNING -> SUCCEEDED` 合法，`SUCCEEDED -> RUNNING` 不合法，`FAILED -> QUEUED` 必须满足 retry 条件。

第三条是 lease 不变量。worker 只有拿到当前有效 lease 才能执行和完成任务。完成事件必须带 `task_id`、`attempt_id`、`lease_epoch` 或 completion token。控制面只接受当前有效 lease 对应的完成。

第四条是结果不变量。一个任务最多有一个生效的最终结果。重复 completion 可以返回已有结果，但不能产生两个互相矛盾的 result_ref。大结果必须先写入对象存储并校验，日志或 metadata 只保存引用和校验信息。

第五条是恢复不变量。控制面重启后，从 durable storage 或 append-only log 重建出的状态，必须和崩溃前已经确认的事件一致。内存队列、worker 本地状态、缓存都不能作为唯一事实来源。

第六条是重试不变量。每次执行都有 attempt 序号；可重试错误进入 retry_wait，超过最大次数进入 dead_letter；不可重试错误直接终止或等待人工处理。重试不能无限创建同一个任务的并发执行。

第七条是可观测不变量。每个状态转换都能被追踪到原因和触发者。至少要有 request_id、task_id、attempt_id、worker_id、tenant_id、trace_id 和时间戳。否则线上出了问题，系统虽然“可能正确”，但没人能证明。

简化版实现可以只用一张数据库表加一个 worker poll API，但这些不变量仍然要在。更进一步可以把状态变化写成 append-only event，再投影出 metadata view。LogServe 选择后者，是为了展示 log-first 和 replay 语义。

面试里可以这样答：

```text
我会先定义七类不变量：提交不丢且幂等、状态机合法、只有当前 lease 能完成、最终结果唯一、重启后可恢复、重试有 attempt 和上限、每次转换可观测。
有了这些不变量，再决定是用数据库表、队列还是 append-only log 实现。实现可以简化，语义不能含糊。
```

## Q049. 可靠任务执行的常见误用是什么，误用后通常会产生什么线上症状？

**回答：**

最常见的误用是把内存队列当成事实来源。控制面收到任务后先放进 channel 或 Redis list，稍后再写数据库。平时看起来没问题，进程重启、机器宕机或写库失败时，就会出现“客户端明明提交成功，后台却查不到任务”的事故。

第二个误用是把消息投递成功当成任务完成。队列 ack 只说明 worker 收到了消息或处理到了某个阶段，不一定说明业务结果已经可靠提交。RabbitMQ 这类系统提供 ack/nack 和 requeue，但业务任务是否完成、结果是否幂等、外部副作用是否成功，仍然要由任务平台自己定义。

第三个误用是没有幂等键。客户端超时后重试提交，系统创建两个任务；worker 完成回写超时后重试，系统写出两个结果；workflow step 重放时，外部 API 被调用两次。线上症状通常是重复扣费、重复发信、重复生成报告、重复消耗 GPU。

第四个误用是无限重试。所有错误都按 transient 处理，没有最大次数、没有退避、没有 dead letter。外部依赖一抖动，队列开始堆积，worker 全部忙于失败任务，正常任务也被拖慢。监控上会看到 retry rate、pending age、error rate 和成本一起上升。

第五个误用是任务 payload 和 result 过大。把大输入、大输出直接塞进 metadata DB 或日志，短期省了对象存储，长期会导致数据库膨胀、备份变慢、replay 变慢、内存和网络成本上升。大对象应该走 result store，任务系统保存引用和 checksum。

第六个误用是任务粒度不合理。任务太细，控制面和日志写入成本压过业务执行；任务太粗，失败后只能从头重跑，恢复成本高，也无法公平调度。线上会看到吞吐低、tail latency 高、重试成本大，或者某些长任务长期占住 worker。

第七个误用是把可靠任务平台当严格分布式事务。任务平台可以减少重复和丢失，但不能让不支持幂等的第三方 API 自动变成 exactly once。如果业务不愿意提供幂等键、查询接口或补偿逻辑，平台只能给出风险边界。

在 LogServe 的表达里，要避免把它说成“已经解决所有生产任务可靠性”。更准确的说法是：它实现和验证了 log-first、lease/redelivery、replay、exactly-once-ish result commit 这些机制；外部副作用、生产多租户、安全和多节点复制仍然是后续生产化工作。

面试里可以这样答：

```text
常见误用包括：内存队列当事实来源、队列 ack 当业务完成、没有幂等键、无限重试、大结果塞 metadata、任务粒度过细或过粗、把任务平台当分布式事务。
线上症状通常是任务丢失、重复执行、重复副作用、retry storm、队列 pending age 飙升、数据库膨胀、replay 变慢和成本失控。
```

## Q050. 可靠任务执行在单机和分布式环境中的语义有什么差异？

**回答：**

单机环境里，很多问题可以靠进程内锁、单个数据库事务、本地文件 fsync 和单调计数器解决。比如一个控制面进程负责调度，一个本地 logd 负责 append，一个 metadata view 在本机更新。只要机器不丢盘，崩溃恢复语义相对清楚。

分布式环境里，问题会多一层。worker、control plane、log service、metadata DB、对象存储、网络都可能独立失败。你看到 worker 没有心跳，不知道它是死了、慢了、网络分区，还是控制面自己收不到消息。旧 worker 可能在 lease 过期后继续执行；两个 control plane 实例可能同时调度；客户端可能打到不同 region；日志复制可能在 leader 切换时出现未提交尾部。

单机里的“顺序”也不等于分布式顺序。单机可以用一个 mutex 或一个文件 offset 定义全局顺序；分布式系统通常只能在某个 shard、partition、stream 或 consensus group 内定义顺序。跨 shard 的全局顺序成本很高，很多任务平台不应该默认需要它。

单机里的“持久化成功”通常指本地文件或数据库已经确认；分布式里的“成功”要说明复制条件：写到 leader 就返回，还是多数派提交后返回，还是跨区域复制后返回。这个差异直接影响 RPO、延迟和可用性。

单机里的 lease fencing 可以用内存版本号或数据库条件更新；分布式里需要更严格的 epoch、term、generation、CAS、leader fencing。否则网络分区恢复后，旧 owner 可能继续写状态，造成 split-brain。

单机 benchmark 也不能直接外推到分布式。LogServe 的单机 Ubuntu 实验能说明 log-first、replay、redelivery、actor recovery、snapshot 和 locality scheduling 的机制跑通；它不能证明多节点复制、跨区域恢复、生产对象存储和真实 GPU 负载下的表现。面试时这样讲更稳。

面试里可以这样答：

```text
单机环境可以依赖本地锁、本地 fsync 和单个控制面来定义顺序和恢复；分布式环境必须处理网络分区、重复调度、旧 worker 回写、leader 切换、复制提交点和跨节点时钟差异。
所以分布式语义要额外定义 lease epoch、fencing、quorum commit、shard 内顺序、RPO/RTO 和幂等完成。
单机实验可以证明机制，但不能直接证明生产级分布式语义。
```
## Q051. append-only log service 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

append-only log service 的核心目标是保存一串可恢复、可排序、可重放的事实。它先解决正确性和持久性，再通过顺序写、批量刷盘和分段文件获得性能收益。不要把它理解成“只能追加所以性能高”的文件封装；真正有价值的是它给上层提供了清楚的语义：哪些事件已经被接受，顺序是什么，崩溃后能恢复到哪里，读者可以从哪个 offset 继续。

从正确性角度看，log service 要回答几件事。第一，append 成功返回后，这条记录是否保证不会丢；第二，同一个 stream 内记录顺序是否稳定；第三，重启后如何识别最后一条完整记录；第四，客户端超时后重试 append 会不会写出重复事实；第五，索引损坏时能否从日志本体重建。

从性能角度看，append-only log 的优势来自顺序 I/O 和批量提交。数据库 WAL 这类系统会先写日志，再延后写数据页，因为顺序写和合并 fsync 比随机刷很多数据页便宜。对于任务平台，log service 可以把大量状态事件顺序追加，再异步投影成 metadata view，减少热路径上的复杂更新。

安全性不是 append-only log 的默认结果。日志只追加，并不自动意味着防篡改。如果需要审计级安全，还要加访问控制、不可变保留、checksum、hash chain、签名、对象锁、权限隔离和独立审计。否则管理员仍可能删除 segment 或改历史文件。

可维护性来自 replay。状态出问题时，可以从日志重建 metadata view，比较在线状态和 replay 状态，也可以分析某个任务为什么进入当前状态。但 replay 能力要求事件格式兼容、事件语义稳定、日志保留策略合理。只追加一堆无法解释的 JSON，并不会自动变成好维护的系统。

在 LogServe 中，append-only shared log 是 task、workflow、actor、LLM 事件的事实来源。metadata view、dashboard、调度队列都是派生状态。项目里的 logd 支持分段日志、idempotent append、恢复扫描和不同 fsync 策略，用来证明 log-first control plane 和 replay 机制。生产级多副本复制是另一个层次，不能直接由单机 append-only log 推导出来。

面试里可以这样答：

```text
append-only log service 的核心目标是保存可排序、可恢复、可 replay 的事实。它首先解决正确性和持久性：append 成功后语义清楚，崩溃后能恢复，索引能重建，客户端重试不会写出重复事实。
性能收益来自顺序写、批量 fsync 和异步 projection；安全和可维护性需要额外设计，例如权限、校验、防篡改和事件兼容。
```

## Q052. append-only log service 的典型适用场景和不适用场景分别是什么？

**回答：**

append-only log 适合记录“发生过的事实”。典型场景包括数据库 WAL、event sourcing、任务状态事件、workflow event history、actor command log、审计日志、CDC、复制日志、消息流、物联网事件、支付状态流、配置变更历史。共同点是：写入以追加为主，历史对恢复或审计有价值，读者可以按 offset 或时间顺序消费。

它也适合把复杂状态拆成“事实 + 投影”。例如任务平台把 `TaskSubmitted`、`TaskLeased`、`TaskCompleted` 写到日志，再投影出任务表；actor 系统把 command 写到日志，再 replay 出 actor state；workflow 系统保存 event history，再用 replay 恢复执行状态。这样 metadata view 坏了可以重建，不必把每次状态更新都当作唯一事实。

不适合的场景也很明确。第一类是高频随机更新和复杂查询。比如用户资料表、库存表、权限关系查询，这些更适合数据库和索引。日志可以记录变更，但不应该直接承担任意条件查询。

第二类是大对象存储。日志里塞视频、模型 checkpoint、大结果 payload，会让 segment 膨胀、恢复变慢、缓存命中变差。大对象应该写对象存储，日志记录 result_ref、checksum、size 和 metadata。

第三类是只需要当前值、不关心历史的临时状态。比如 worker 当前内存水位、短期限流计数、本地缓存条目，不一定需要进入长期 append-only log。滥用日志会造成存储成本和 replay 成本上涨。

第四类是强隐私或高敏感明文内容。日志保留时间长、复制范围广、被多个系统消费，一旦写入敏感明文，后续删除和合规处理会很麻烦。可以写引用、hash、脱敏字段和审计元数据，不要随手写 payload。

第五类是需要强事务查询的业务主库。append-only log 可以作为变更来源，但用户查询、唯一约束、二级索引、事务隔离、复杂过滤仍要由数据库或物化视图承担。日志服务不是关系数据库的替代品。

在 LogServe 中，task/workflow/actor/LLM 的状态事件适合进 shared log；大结果、actor snapshot、model checkpoint 不适合直接塞进 log，所以用 result store 或 checkpoint cache 保存，日志只放引用。这条边界很重要，否则 replay 和 benchmark 会被大对象拖偏。

面试里可以这样答：

```text
append-only log 适合记录事实和变更历史，例如 WAL、event sourcing、workflow history、actor command、审计、CDC 和复制日志。
它不适合做任意复杂查询、高频随机更新、大对象存储、短期临时状态和敏感明文 payload。
比较健康的模式是：日志保存事实，数据库或 metadata view 提供查询，大对象放对象存储，日志只保存引用和校验信息。
```

## Q053. append-only log service 和相近概念最容易混淆的边界在哪里？

**回答：**

第一个边界是 log service 和消息队列。消息队列强调投递和消费进度，通常有 ack、nack、requeue、consumer group。append-only log 强调持久历史、offset 和 replay。Kafka 这类系统同时具备日志和消息系统的特征，所以更容易让人混淆。面试里要说清楚：队列关注“谁还没处理”，日志关注“事实按什么顺序发生过”。

第二个边界是 WAL 和 event log。WAL 主要服务于存储引擎恢复，记录的是如何 redo/undo 数据页或事务；event log 服务于业务语义，记录 `TaskSubmitted`、`OrderPaid` 这类领域事件。两者都可能 append-only，但读者、格式、保留策略和兼容性要求不同。不能因为数据库有 WAL，就说业务天然具备 event sourcing。

第三个边界是 audit log。审计日志关注谁做了什么、权限判断是什么、是否可追责。业务 append-only log 记录状态事实，但未必记录操作者、来源 IP、权限策略和管理操作。任务完成事件有审计价值，但不等于完整审计能力。

第四个边界是 object store。对象存储可以存日志 segment，也可以存大结果，但它不自动提供 per-stream offset、幂等 append、读者进度和恢复扫描。把对象存储当 segment 后端可以，但 log service 仍要自己定义 record 格式、index、commit 语义和读接口。

第五个边界是 immutable 和 append-only。append-only 表示正常写路径只追加，不表示永远不能压缩、截断、归档或逻辑 trim。Kafka 有 retention 和 compaction，数据库 WAL 会归档和删除，LogServe 的 actor stream 也有 logical trim。关键是 trim 不能破坏 replay 语义，要有 snapshot 或 checkpoint 承接。

第六个边界是 offset 和业务版本。offset 表示日志位置，业务版本表示对象状态版本。一个 actor command_seq、task attempt、workflow step version 不一定等于物理 offset。把这些概念混在一起，会导致恢复和并发控制都变得脆弱。

面试里可以这样答：

```text
我会区分几组概念：队列关注投递，日志关注历史和 replay；WAL 服务存储恢复，event log 服务业务语义；业务事件不等于完整审计；对象存储只是保存介质，不提供日志语义；append-only 也不等于永不 trim，关键是 trim 后仍能从 snapshot + tail log 恢复。
另外 offset 是日志位置，业务版本是状态语义，不能混用。
```

## Q054. append-only log service 在高并发场景下可能出现哪些隐藏问题？

**回答：**

高并发下最明显的问题是写入串行点。append-only log 看起来只需要顺序追加，但当前 segment、offset 分配、fsync、index 更新、idempotency map 都可能在一把锁下完成。短记录、高 QPS 时，业务处理很轻，锁竞争和 syscall 成本就会成为主瓶颈。

第二个问题是 fsync 策略。每条记录都 fsync，持久语义最直观，但吞吐会被磁盘同步延迟限制；批量 fsync 吞吐高，但会增加 tail latency；interval fsync 更快，却要解释崩溃时最近一段记录的确认语义。不能只报 append records/s，不说哪些记录在崩溃后一定保留。

第三个问题是热 stream。某个 tenant、workflow 或 actor 的事件量远大于其他 stream，会让 per-stream offset、index、锁和读者都变成热点。全局 log 也可能因为一个热 stream 把所有写入都拖慢。需要分区、shard、per-stream batching 或按租户隔离。

第四个问题是索引内存膨胀。为了按 stream 和 offset 快速读，系统往往维护内存索引。如果每条记录都建索引，stream 数量和事件数量上来后，内存会比日志数据更早成为瓶颈。可以用稀疏索引、segment index、冷热分层和按需重建，但要接受读取时多扫一小段。

第五个问题是 segment roll 和 retention 抖动。日志分段滚动、索引落盘、旧 segment 删除或归档都可能和 append 热路径抢 I/O。高并发时如果这些后台任务没有限速，会让 append p99 突然抬高。

第六个问题是 reader lag。写入很快，projection、consumer、replica 跟不上，系统表面上 append 成功，metadata view 却越来越落后。对任务平台来说，这会表现为提交成功但查询不到最新状态，或者调度器看不到可运行任务。需要监控 per-reader lag 和 projection lag。

第七个问题是 idempotency 存储变成热点。客户端超时后重试很多，log service 要快速判断同一个 request_id 是否已经 append。这个索引如果只在内存里，重启后语义丢失；如果每次查数据库，吞吐又受影响。比较稳的做法是把 idempotency key 写入 record，并在恢复时重建近期或全量去重索引。

在 LogServe 的单机 benchmark 中，fsync 策略对 append throughput 影响很大，这是一个很好的讲法。但还要补一句：多节点生产场景还会出现复制 lag、leader 瓶颈、follower 落后、网络抖动和 quorum commit 延迟，这些不在单机 benchmark 覆盖范围内。

面试里可以这样答：

```text
高并发下 append-only log 的隐藏问题包括：append 锁竞争、fsync 策略导致吞吐和 p99 取舍、热 stream、索引内存膨胀、segment roll 抖动、reader/projection lag、idempotency 索引热点。
单机还主要看锁和磁盘；分布式还要看复制、leader、quorum 和网络。
所以 benchmark 必须把 fsync 策略、record size、stream 数、reader lag 和恢复时间一起报出来。
```

## Q055. append-only log service 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

第一个边界是半条记录。进程或机器在写 segment 时崩溃，文件尾部可能只有 record header，没有完整 payload；也可能 length 写了，但 checksum 对不上。恢复时必须顺序扫描到最后一条完整且校验通过的记录，遇到坏尾部就截断。不能把尾部残留字节当成有效事实。

第二个边界是 append 成功响应和 fsync 之间的窗口。如果服务先返回成功再异步 fsync，崩溃后可能丢掉客户端以为成功的记录。这不是一定错误，但语义必须写清楚。若 API 承诺 committed append，就应该在达到持久化或复制条件后再返回。

第三个边界是客户端超时重试。客户端发出 append，请求在服务端已经写入，但响应丢了。客户端用同一个 request_id 重试时，服务应该返回同一个 stream offset 或 physical position；如果 payload 不同，要返回冲突。否则日志里会出现重复或分叉事实。

第四个边界是索引损坏。segment 文件还在，index 文件缺失或损坏。此时 log service 应该能从 segment 顺序扫描重建索引。索引是加速结构，不能成为唯一事实来源。

第五个边界是 segment roll 中断。当前 segment 写满，系统准备切新 segment，刚写完 metadata 或刚创建新文件就崩溃。恢复时要判断哪个 segment 有效、尾部是否完整、下一个 offset 应该是多少。segment manifest 或文件命名规则要足够简单，避免恢复时依赖复杂状态。

第六个边界是 reader offset 落后。服务重启后，reader 可能拿着旧 offset 来读；如果旧 segment 已归档或 trim，服务要返回明确错误，或者提供从 snapshot 恢复的路径。不能静默从更晚 offset 开始读，否则 replay 会漏事件。

第七个边界是分布式复制里的未提交尾部。leader 写了本地日志但还没多数派提交就挂掉，新 leader 不一定包含这条记录。这时客户端是否已经收到成功响应，决定了系统是否违反承诺。分布式 log 必须区分 appended、replicated、committed，不要把本地写入当作全局提交。

LogServe 的 logd 当前重点覆盖单机恢复：record 格式、segment、idempotent append、启动恢复和 fsync 策略。回答时可以说它能证明单机 crash recovery 和 replay 机制，但多副本 leader change 和 quorum commit 需要额外设计。

面试里可以这样答：

```text
这些场景主要暴露七个边界：半条记录怎么截断、成功响应是否已经 fsync、客户端超时重试是否幂等、索引损坏能否重建、segment roll 中断怎么恢复、reader offset 落后怎么处理、分布式复制中本地 append 和 committed 的差异。
我的原则是 record 带 length/checksum，恢复顺序扫描到最后一条有效记录；索引可重建；append API 明确 committed 语义；客户端重试用 request_id 去重。
```
## Q056. append-only log service 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

append-only log 的性能瓶颈通常先看 I/O 和锁竞争，再看 CPU、内存和网络。单机 log service 如果 record 很小，业务处理很轻，最容易卡在 fsync、系统调用、segment 文件写入、锁和索引更新；如果 record 很大，瓶颈会转向内存拷贝、checksum、压缩和磁盘带宽；如果是多副本日志，网络和 quorum commit 会变成关键路径。

I/O 是最直接的瓶颈。`write` 可以被 page cache 吸收，但 `fsync` 要把数据推进到稳定介质，延迟受磁盘、文件系统、队列深度和同步策略影响。`always fsync` 适合语义最保守的场景，但吞吐会低；batch fsync 和 group commit 可以让一次同步覆盖多条记录，提高吞吐，但增加等待时间。

锁竞争来自 append 串行化。每次写入要分配 offset、追加 buffer、更新内存索引、记录 idempotency key、判断 segment 是否要 roll。如果这些操作都在同一把 mutex 下，高并发短记录时 CPU 可能没满，吞吐已经上不去。可以用批处理、单写线程、多 shard log、无锁队列或缩短临界区，但不能破坏 offset 顺序。

CPU 主要消耗在序列化、checksum、压缩、加密和协议处理上。日志服务为了恢复可靠，通常会给 record 加 CRC 或 hash；为了节省存储和网络，可能做压缩；为了安全，可能做 TLS 或静态加密。这些都不是免费功能。尤其是小 record 高 QPS 时，固定开销占比很高。

内存瓶颈来自 page cache、写缓冲、读缓冲、索引、去重表和 reader backlog。很多 log service 的读性能依赖 page cache；如果写入和读取工作集超过内存，read amplification 会变明显。索引如果太密，也会抢走 page cache 空间。

网络瓶颈主要出现在分布式复制和远程读。leader 要把 record 发给 follower，等待多数派或 ISR；跨 AZ/region 会增加延迟；reader 或 projection 在远端拉日志，也会消耗出口带宽。多副本日志的 p99 往往由慢 follower、网络抖动或 leader 负载决定。

LogServe 的 logstore benchmark 能说明单机 fsync 策略差异：batch/interval 明显高于 always。这个结果适合解释“顺序写和批量同步为什么重要”。但如果面试官问生产分布式瓶颈，就要补充复制、quorum、网络和 leader 热点。

面试里可以这样答：

```text
单机 append-only log 常见瓶颈是 fsync、segment I/O、append 锁、索引更新和 checksum；record 大时还会卡内存拷贝、压缩和磁盘带宽。
分布式日志还要加上网络复制、quorum commit、leader 热点和 follower lag。
我会按 record size、stream 数、fsync policy、reader 数和 replication factor 分开测，不用一个吞吐数字概括所有场景。
```

## Q057. append-only log service 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

correctness test 要证明日志语义。最基本的是 append 后能按 stream 和 offset 读回；offset 单调递增；同一个 request_id 重试返回同一条记录；request_id 相同但 payload 不同返回冲突；record checksum 错误会被识别；index 删除后可以重建；segment roll 后读写连续；logical trim 后从合法起点读取，不会漏掉恢复所需事件。

崩溃恢复测试也属于 correctness。可以构造半条记录、坏 checksum、截断 segment、丢 index、segment manifest 不完整，然后启动 log service，验证它只保留最后一条有效记录，并且重建出的 stream offset 正确。这个测试不需要很高吞吐，但要覆盖文件尾部和恢复状态机。

stress test 要制造并发压力和竞争。比如多个客户端同时 append 同一 stream，多个 stream 混写，大量重复 request_id，高频 segment roll，reader 一边读一边 writer 追加，projection 故意变慢，后台 retention 和 foreground append 同时进行。目标是找数据竞争、死锁、offset 分配错误、index 不一致、reader 读到半条记录、内存膨胀和 p99 抖动。

benchmark 则要量化吞吐和延迟。指标包括 append records/s、append bytes/s、p50/p95/p99 append latency、read records/s、recovery time、segment roll cost、index rebuild time、CPU、内存、磁盘写入、fsync latency、page cache 命中、reader lag。配置必须写清楚：record size、stream 数、client 并发、fsync policy、segment size、checksum/压缩是否开启、磁盘类型和操作系统。

分布式版本还要测复制。比如 replication factor、quorum size、leader 切换时间、follower lag、网络延迟、跨 AZ 写入、未提交尾部处理、读 committed 和读 latest 的差异。单机 benchmark 不能证明这些。

LogServe 已有 logstore benchmark 可以作为回答素材：20,000 records、16 streams、256-byte payload、不同 fsync policy 下比较 append/read/recover。更完整的测试体系还可以加 crash fuzz、坏尾部注入、重复 append golden test 和长时间 soak test。

面试里可以这样答：

```text
correctness test 测 append/read 顺序、幂等 append、checksum、segment roll、index rebuild、trim 和崩溃恢复。
stress test 测多 writer、多 stream、重复 request_id、reader lag、segment roll、retention 并发和资源膨胀。
benchmark 测 records/s、bytes/s、p95/p99、fsync latency、read throughput、recovery time、index rebuild time 和资源使用。
如果是分布式 log，还要单独测复制、quorum、leader failover 和 follower lag。
```

## Q058. 如果要求从零实现一个简化版 append-only log service，你会先定义哪些不变量？

**回答：**

我会先定义日志不变量，因为 log service 一旦写错，所有上层状态都会被污染。简化版可以功能少，但不能让 append、read 和 recovery 的语义含糊。

第一条是 append 原子性。一条 record 要么完整可读，要么在恢复时被丢弃，不能出现读者读到半条业务记录。record 格式必须有 length、version、payload length 和 checksum。

第二条是顺序不变量。同一个 stream 内 offset 单调递增且不重复。append 返回的 offset 一旦 committed，就不能在恢复后指向另一条 payload。

第三条是幂等不变量。同一个 stream 下，同一个 request_id 或 idempotency key 重试时，如果 payload 一样，返回同一条记录；如果 payload 不一样，返回冲突。客户端超时不应制造重复事实。

第四条是恢复不变量。服务重启后，通过扫描 segment 可以恢复到最后一条完整有效记录；索引、缓存、去重表都可以从日志重建，不能作为唯一事实来源。

第五条是可读性不变量。读者从某个 stream offset 开始读取时，要么返回连续记录，要么返回明确的 offset 不存在、被 trim 或未提交错误。不能静默跳过缺失记录。

第六条是提交语义不变量。API 要明确 append 返回时意味着什么：只是写入 page cache、已经 fsync、还是已经复制到多数派。不同模式可以并存，但响应字段和文档要说清楚。

第七条是 trim 不变量。任何 retention、logical trim 或 compaction 都不能破坏上层声明的 replay 能力。要么保留完整日志，要么有 snapshot/checkpoint 作为新的恢复起点。

第八条是格式兼容不变量。record version 和 event schema version 要分开。record version 服务于日志文件格式，event schema version 服务于业务事件解析。以后升级时不能让旧 segment 无法读取。

在 LogServe 里，最小实现就是 segment append、per-stream offset、checksum、idempotent append、启动恢复和可重建索引。加上这些不变量，才有资格把它作为任务状态的事实来源。

面试里可以这样答：

```text
我会先定义八个不变量：record 原子可读、stream offset 单调唯一、append 幂等、重启可扫描恢复、索引可重建、读取不静默跳洞、append 返回语义明确、trim 不破坏 replay、格式版本兼容。
简化版可以没有复制和复杂查询，但这些不变量不能省。
```

## Q059. append-only log service 的常见误用是什么，误用后通常会产生什么线上症状？

**回答：**

第一个误用是把 append-only log 当普通数据库用。所有查询都扫日志，按用户、时间、状态、租户过滤，最后发现查询越来越慢，replay 越来越重。日志应该保存事实，查询应该走投影、索引或数据库视图。

第二个误用是把大对象写进日志。任务结果、模型 checkpoint、图片、压缩包都塞进 record，短期实现简单，长期会让 segment 巨大、恢复慢、复制慢、备份贵。线上症状是 log disk 膨胀、read throughput 下降、projection lag 增长，甚至因为单条 record 太大导致内存峰值异常。

第三个误用是没有 idempotent append。客户端超时后重试，日志里出现两条相同业务事件；更糟的是同一个 request_id 对应不同 payload。上层会表现为重复任务、重复 workflow step、actor command 重放两次、审计记录不可信。

第四个误用是忽视 fsync 语义。系统配置成异步刷盘，却对上层承诺 append 成功不丢。崩溃后丢了最近几条记录，团队才发现“成功返回”只是写进 page cache。这个问题不是性能调优问题，而是语义欺骗。

第五个误用是把索引当事实来源。为了启动快，只保存 index，不保留足够的 segment 或不验证 checksum。索引损坏后无法重建，或者重建出和业务状态不一致的 offset。可靠 log 的基本原则是 segment 本体能恢复索引。

第六个误用是 retention 乱删。为了省空间直接删除旧 segment，却没有 snapshot 或 checkpoint。结果某个 actor、workflow 或 metadata view 需要 replay 时发现历史断了。线上表现是恢复失败、投影状态缺字段、老任务无法解释。

第七个误用是把单机 append-only 当高可用。单机 fsync 只能防进程崩溃和部分机器崩溃，不能防磁盘丢失、节点丢失、区域故障。生产系统如果要求高可用，要有复制、备份、restore 和故障切换。

LogServe 里已经避开了一部分误用：大结果走 result store，metadata 是可重建视图，log benchmark 明确区分 fsync policy。但仍要承认当前 logd 是单机机制验证，不是生产多副本日志。

面试里可以这样答：

```text
常见误用包括：把日志当查询数据库、大对象塞日志、没有幂等 append、异步 fsync 却承诺不丢、索引当事实来源、retention 删除导致 replay 断裂、单机日志冒充高可用。
线上症状是查询慢、磁盘膨胀、重复事件、崩溃后成功记录丢失、索引损坏无法恢复、metadata replay 失败和恢复时间失控。
```

## Q060. append-only log service 在单机和分布式环境中的语义有什么差异？

**回答：**

单机 append-only log 的语义主要围绕本地崩溃恢复。它关心 record 是否完整、checksum 是否正确、fsync 何时发生、segment 如何截断、索引如何重建。只要本机磁盘还在，恢复路径比较直接：扫描 segment，停在最后一条有效记录，重建 offset 和索引。

分布式 append-only log 多了复制和提交语义。leader 本地 append 成功，不等于这条记录已经对整个集群 committed。系统必须说明什么时候返回成功：写入 leader 就返回，复制到 follower 后返回，还是多数派确认后返回。不同选择会改变延迟、吞吐、RPO 和可用性。

单机只有一个写入顺序，分布式通常有多个层次的顺序。一个 partition、shard、stream 或 Raft group 内可以有稳定顺序；跨 partition 的全局顺序通常没有，或者成本很高。业务如果要求跨 stream 全局顺序，需要明确说明为什么需要，以及愿意付出什么代价。

单机恢复处理的是坏尾部；分布式恢复还要处理 leader 切换和日志分叉。旧 leader 可能写了未提交记录，新 leader 不包含这些记录；follower 可能落后；网络分区后可能出现 stale leader。需要 term、epoch、commit index、fencing 和日志匹配规则来保证不会提交两套历史。

单机读取通常可以读到最新本地记录；分布式读取要区分 read latest、read committed、linearizable read、follower read。读 latest 延迟低，但可能读到之后被回滚的未提交记录；read committed 更稳，但可能慢；linearizable read 还要经过 leader 或 quorum。

单机 retention 只影响本地 replay；分布式 retention 还会影响落后副本、落后消费者、跨区域复制和灾备恢复。删除旧 segment 前要确认 snapshot、归档、副本和消费者进度，否则某个副本可能永远追不上。

LogServe 当前更接近单机语义：它能展示 segment recovery、idempotent append、fsync policy、metadata replay 和 task control plane 恢复。分布式版本需要新增复制协议、commit index、leader election、follower catch-up、read committed 语义和跨节点故障测试。

面试里可以这样答：

```text
单机 log 主要定义本地持久化和崩溃恢复：record 完整性、checksum、fsync、segment 截断、索引重建。
分布式 log 还要定义复制和提交：leader append 不等于 committed，必须说明 quorum、commit index、leader epoch、fencing、读 committed 和 follower lag。
单机可以证明 crash recovery；分布式还要证明 leader 切换、日志分叉处理、复制落后和跨节点恢复。
```
## Q061. workflow engine 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

workflow engine 的核心目标是把一组有依赖关系、可能长时间运行、可能失败重试的步骤，变成一个可恢复、可解释、可重放的执行过程。它首先解决正确性和恢复性，其次才是性能、安全性和可维护性。

正确性体现在几个地方。第一，依赖关系不能错：一个 step 没有满足前置条件时不能被调度；前置 step 失败后，后续 step 要么停止，要么走补偿，要么按定义进入失败分支。第二，状态不能乱：`SCHEDULED`、`STARTED`、`SUCCEEDED`、`FAILED`、`CANCELED` 这些状态要有合法边，终态不能被普通事件覆盖。第三，重试不能重跑已经成功的步骤。第四，workflow 恢复后要得到和故障前一致的逻辑状态。

Temporal 的设计里，workflow 依赖 event history 来恢复执行状态；workflow replay 要求代码在相同历史下做相同决策，外部 API、数据库查询、LLM 调用、文件 I/O 这类非确定性操作应放到 activity 里。这个思路可以概括成一句话：workflow engine 不是简单任务队列，它要保存足够的历史，让系统在崩溃、重启、等待、信号和重试之后还能继续解释业务流程。

性能也重要，但它不能压过语义。一个 workflow engine 如果把所有 step 都塞进内存队列，调度很快，但控制面一重启就不知道哪些 step 已经完成，这就不是可靠 workflow。性能优化应该围绕语义做，例如 ready step materialization、增量投影、snapshot、批量调度、worker 本地 executor pool，而不是牺牲 replay 和幂等。

安全性通常不是 workflow engine 单独完成的，但 workflow 必须携带租户、权限、审计和数据访问边界。一个 workflow 可能跨多个系统读写数据，谁启动、谁取消、谁查看结果、step 运行时拿什么凭证，都要被控制。否则 workflow 会变成绕过权限系统的“超级脚本”。

可维护性来自可解释性。好的 workflow engine 会让人看到 DAG、step 状态、失败原因、重试次数、输入 hash、result ref、事件历史和 replay 结果。线上出问题时，不需要只看散落的日志猜测流程跑到哪一步。

结合 LogServe，我会把它讲成 shared-log-backed workflow DAG runtime：Python `@workflow` DSL 生成 DAG step model，控制面根据依赖调度 ready steps，step 支持 retry、timeout、result ref 和 replay 校验。语义是 exactly-once-ish result commit，不是严格 distributed exactly-once。当前实验是单机/多进程机制验证，不是生产级 Temporal 替代品。

面试里可以这样答：

```text
workflow engine 首先解决正确性和恢复性：依赖关系要正确，step 状态机要合法，失败和重试要可解释，控制面重启后能从事件历史恢复。
性能、安全和可维护性都围绕这个目标展开。性能靠 ready step 投影、snapshot 和批量调度；安全靠租户、权限和审计；可维护性靠 DAG、事件历史、result ref 和 replay。
```

## Q062. workflow engine 的典型适用场景和不适用场景分别是什么？

**回答：**

workflow engine 适合多步骤、长耗时、有依赖、有失败恢复要求的业务流程。典型例子包括订单处理、支付后履约、数据导入清洗、报表生成、视频处理 pipeline、机器学习训练/评估流程、RAG 离线索引构建、LLM agent 多步工具调用、审批流程、备份恢复任务。共同点是：单个请求无法在一个短 HTTP handler 里可靠完成，流程中间状态需要被保存，失败后要知道从哪一步继续。

它特别适合“部分成功有价值”的场景。比如一个 workflow 有十个 step，前六个已经成功，第七个访问对象存储失败。恢复时只重试第七个，而不是从头跑完整流程。再比如一个 actor snapshot 已经写好，下游任务失败后可以继续从 snapshot 和 tail log 恢复。

它也适合人机混合或长时间等待。流程可能等待外部信号、定时器、人工审批、异步回调。普通 RPC 不适合持有连接等几个小时，workflow engine 可以把等待记录成事件，之后恢复执行。

不适用场景也不少。第一个是不需要跨步骤恢复的短请求。一次简单查询、一个纯内存计算、一个小的同步事务，用 workflow engine 会增加调度、持久化和观测成本。

第二个是超低延迟路径。workflow engine 要写事件、调度 step、管理状态，它不是微秒级请求处理框架。撮合、在线广告竞价热路径、内核数据面、强实时控制回路，不应放进通用 workflow engine。

第三个是强交互式、状态变化极快但历史价值低的 UI 流程。比如用户拖动滑块产生的大量临时状态，不适合把每个动作都写成 durable workflow event。

第四个是完全不可幂等、不可补偿的外部副作用。如果某个 step 一旦重复调用就造成不可撤销损失，而外部系统又不支持 idempotency key 或查询确认，workflow engine 只能降低风险，不能魔法般保证 exactly once。

在 LogServe 中，workflow DAG 适合表达“任务 A 产出 result_ref，任务 B/C 依赖它，失败后只重试失败 step”。它不适合被描述成生产级审批系统或替代成熟 workflow 平台。项目价值在于用 shared log、replay 和 step 幂等证明机制。

面试里可以这样答：

```text
workflow engine 适合多步骤、长耗时、有依赖、有重试恢复要求的流程，例如订单履约、数据 pipeline、报表、训练流程、RAG 索引和 LLM agent 工具链。
它不适合简单同步请求、微秒级低延迟路径、临时 UI 状态，以及无法幂等也无法补偿的外部副作用。
判断标准是：流程中间状态是否需要持久化，失败后是否要从某一步继续，历史是否有解释和审计价值。
```

## Q063. workflow engine 和相近概念最容易混淆的边界在哪里？

**回答：**

第一个边界是 workflow engine 和任务队列。任务队列负责把一个 work item 交给 worker；workflow engine 负责多个 step 的依赖、状态、重试、等待、信号、补偿和 replay。一个 workflow step 可以落到任务队列里执行，但队列不知道整个 DAG 的业务语义。

第二个边界是 workflow engine 和 scheduler。scheduler 负责“什么时候调度哪个 step 到哪个 worker”，workflow engine 还要知道“这个 step 为什么可以运行、运行完会解锁谁、失败后是否重试、workflow 是否已经进入终态”。调度器是 workflow engine 的一部分，不是完整 workflow engine。

第三个边界是 workflow engine 和 cron。cron 只负责按时间触发，workflow engine 负责触发后的持久流程。每天凌晨启动一个导入流程可以由 cron 或 schedule 发起，但导入流程内部的依赖和恢复仍属于 workflow engine。

第四个边界是 workflow engine 和 stream processing。workflow 通常处理一条业务流程实例，步骤有限或可界定；流处理处理无限事件流，关心 window、watermark、stateful operator、backpressure。把无限流塞进 workflow engine，会让事件历史和状态膨胀。

第五个边界是 workflow engine 和 saga。saga 是跨服务事务的一种模式，强调局部事务和补偿；workflow engine 可以承载 saga，但还可以承载数据 pipeline、审批、批处理、LLM agent 流程。不要把 workflow engine 缩窄成“只做补偿事务”。

第六个边界是 workflow engine 和 actor service。workflow 是流程实例，通常有 DAG 或状态机；actor 是有身份的状态对象，强调消息串行化和状态封装。LogServe 同时有 workflow 和 actor，是因为它们共享 log-first/replay 底座，但上层语义不同。

第七个边界是 replay 和重新执行。workflow replay 是用历史事件恢复决策状态，不应该重新调用已经完成的外部 activity。把 replay 当成“从头再跑一遍所有外部操作”，会导致重复发请求、重复扣费、重复生成结果。

面试里可以这样答：

```text
workflow engine 容易和任务队列、调度器、cron、流处理、saga、actor 混淆。
我的边界是：队列负责投递，调度器负责资源选择，cron 负责时间触发，流处理负责无限事件流，saga 是一种补偿事务模式，actor 是有身份的状态对象。
workflow engine 的核心是流程实例的依赖、状态、等待、重试、补偿和 replay。尤其要区分 replay 和重新执行外部副作用。
```

## Q064. workflow engine 在高并发场景下可能出现哪些隐藏问题？

**回答：**

高并发 workflow 的第一个问题是 ready step 爆炸。一个 workflow 扇出几千个 step，或者很多 workflow 同时进入同一层依赖，调度器会突然看到大量可运行 step。没有租户配额、并发上限和 backpressure 时，worker pool、metadata DB 和对象存储会被一起打满。

第二个问题是依赖解析成本。简单实现可能每次 step 完成都扫描整个 DAG，找哪些 step ready。小 workflow 没问题，大量 workflow 或大 DAG 下会变成 O(N^2) 热点。更好的做法是维护 indegree、reverse dependency index、ready queue 和增量投影。

第三个问题是幂等键热点。step 级去重要按 `workflow_id + step_id + input_hash` 或类似键控制。高并发下，这个索引如果集中在一个表或一把锁上，会影响完成吞吐。没有这个索引又会导致重复成功结果。

第四个问题是大结果和中间数据移动。workflow step 之间如果直接传大 payload，metadata 会膨胀，网络也会被拖慢。更稳的做法是 step 输出写对象存储，后续 step 只拿 result_ref 和 checksum。

第五个问题是长尾 step 拖住整个 workflow。DAG 里一个慢 step 可能让下游全部等待。全局平均延迟看起来还行，但 workflow 完成时间 p99 很差。需要按 critical path、oldest blocked workflow、per-step retry、dependency wait time 观测。

第六个问题是 retry storm。某个外部依赖挂了，所有 workflow 的同类 step 同时失败并重试，队列迅速堆积。必须有指数退避、jitter、按错误类型分类、租户级并发上限和熔断。

第七个问题是 event history 膨胀。每个 step 的 schedule、start、complete、retry、timeout 都写事件，大 workflow 或长时间 workflow 会生成大量历史。恢复时如果每次从头 replay，RTO 会越来越差。需要 snapshot、continue-as-new、history partition 或 compacted metadata，具体做法取决于系统。

在 LogServe 中，workflow benchmark 只在单机、有限 worker、mock 负载下证明 DAG、retry、timeout、replay 能跑通。面试里可以把这些高并发问题作为生产化路线，而不是把当前实验夸成已经覆盖。

面试里可以这样答：

```text
高并发 workflow 的隐藏问题包括 ready step 爆炸、依赖解析变慢、step 幂等索引热点、大结果传递、长尾 step 拖慢 critical path、retry storm、event history 膨胀和 projection lag。
我会用增量依赖索引、租户并发上限、result_ref、退避和 jitter、snapshot/replay 优化来处理，同时按 workflow p99、blocked age、ready queue depth 和 history size 做观测。
```

## Q065. workflow engine 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

第一个边界是 step 成功但完成事件没写入。worker 可能已经把大结果写到对象存储，但控制面没有记录 `StepSucceeded`。恢复后如果直接重跑 step，可能浪费资源，甚至重复外部副作用。比较稳的做法是 result_ref 带 checksum 和幂等键，完成回写可重试，控制面按 step 幂等键接受一次最终结果。

第二个边界是 workflow 控制面重启。内存里的 ready queue、blocked set、running set 都会丢。可靠 workflow engine 必须从 event history 或数据库重建：哪些 step 已完成，哪些 step ready，哪些 running step 的 lease 已过期，哪些 timer 需要恢复。

第三个边界是超时和完成并发。控制面刚把 step 标记为 timeout，worker 同时回写成功。状态机要定义谁生效：通常用 attempt、lease_epoch、event order 或条件更新决定，已经进入终态的 step 不能被旧 attempt 改写。

第四个边界是取消和子 step 并发。用户取消 workflow 时，部分 step 正在运行，部分 step 已经调度，部分 step 还没解锁。取消语义要明确：是否尽力取消 running step，已经成功的 step 是否保留，后续 step 是否阻止调度，补偿是否触发。

第五个边界是 replay 非确定性。workflow 代码如果在 replay 时直接读当前时间、随机数、环境变量、数据库或 LLM，会走出和历史不同的路径。成熟 workflow 系统一般要求 workflow 决策代码确定性，把外部副作用放到 activity，并把 activity 结果记录到历史里。

第六个边界是重试范围。应该重试 failed activity/step，还是重试整个 workflow？很多情况下重试整个 workflow 会重复已完成步骤，成本高，还可能重复副作用。更合理的是按 step 或 activity 设重试策略，只有 workflow 级别确实无状态或可幂等时才整体重试。

第七个边界是版本升级。workflow 可能跑几天或几个月，期间代码升级了。旧 workflow history replay 到新代码时，如果分支逻辑变了，就可能无法匹配历史。需要 patch/version marker、兼容 reader，或者让旧 workflow 继续跑旧定义。

LogServe 的边界比较清楚：它支持 DAG step 状态、retry、timeout、result ref 和 replay 校验，但当前不是完整 Temporal。可以说它展示了核心机制，生产级 workflow 还要补长期 history 管理、代码版本兼容、信号/定时器完整语义和多节点恢复。

面试里可以这样答：

```text
这些场景会暴露 step 成功但完成事件丢失、控制面重启丢 ready queue、超时和完成并发、取消和 running step 并发、workflow replay 非确定性、重试范围选错、代码升级后旧 history 无法 replay 等问题。
我会用 event history 做事实来源，用 step 幂等键和 attempt/lease_epoch 控制完成，用确定性 workflow 决策和 activity 记录外部副作用，用版本标记处理长期 workflow 升级。
```

## Q066. workflow engine 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

workflow engine 的瓶颈取决于 workflow 的形态。如果 step 很重，瓶颈在 worker 的 CPU、GPU 或外部依赖；如果 step 很轻，瓶颈通常转到控制面：依赖解析、事件写入、metadata 更新、ready queue 维护和调度锁。

CPU 瓶颈常见于 DAG 解析、表达式求值、序列化、状态机转移、replay 和投影。大 DAG 如果每次事件都全量扫描，会消耗很多 CPU。replay 如果反复从头读大 history，也会拖慢恢复和查询。

内存瓶颈来自大量活跃 workflow、ready step、blocked step、timer、history cache、result metadata 和 dashboard view。workflow engine 很容易为了查询方便把整张 DAG 和所有 step 状态放内存里。规模上来后，GC 和内存峰值会先出问题。

锁竞争常见于全局 ready queue、全局 workflow map、调度器主循环、租户配额、幂等索引和 worker registry。单机实验里这些锁可能不明显，多 worker、多租户、大量短 step 时会变成 p99 的来源。

I/O 瓶颈来自 event history append、metadata DB 写入、snapshot、result store、audit log 和 projection。每个 step 至少产生 schedule/start/complete 事件，workflow 越细，I/O 放大越明显。大结果如果不走对象存储，metadata DB 会很快变成瓶颈。

网络瓶颈来自远程 worker、对象存储、跨服务 activity、跨区域复制和观测导出。workflow 的关键路径通常由最慢 step 决定，网络抖动会直接体现在 workflow p99 上。

LogServe 的 workflow p95/p99 是单机 mock 环境数字，适合证明路径可跑；如果要评估生产瓶颈，还要分开测：DAG 解析时间、ready step 调度吞吐、event append latency、metadata projection lag、result store latency、worker 执行时间和 replay 时间。

面试里可以这样答：

```text
workflow engine 的瓶颈不能只说 CPU 或 I/O。重 step 卡 worker CPU/GPU 或外部依赖；轻 step 卡控制面事件写入、metadata 更新、依赖解析、幂等索引和调度锁；大结果卡对象存储和网络；长 history 卡 replay。
我会按 submit、dependency resolution、schedule、execute、result upload、completion commit、projection、replay 分段测。
```
## Q067. workflow engine 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

correctness test 要测 workflow 语义，不是只测某个 step 能不能跑。最基本的是 DAG 依赖：前置 step 没成功时，下游 step 不能 ready；多个前置都成功后，下游只 ready 一次；失败 step 根据策略进入 retry、failed 或补偿路径。还要测状态机合法性：终态不能被旧事件覆盖，timeout 和 success 并发时只有一个结果生效。

幂等和 replay 是 correctness 的重点。重复提交同一个 workflow id 应返回已有实例或冲突；重复完成同一个 step 不应产生两个 result_ref；控制面重启后从 event history 重建出的 DAG 状态应和在线状态一致；已经成功的 step 在 replay 中不能重新执行外部副作用。对 LogServe，可以重点测 `workflow_id + step_id + input_hash` 去重是否生效。

stress test 要测并发和故障。比如大量 workflow 同时提交，大 DAG 扇出，大量 step 同时完成，worker 被 kill，control 重启，metadata view 落后，对象存储 503，重复 completion，旧 attempt 完成，取消和完成并发。stress test 的通过标准不是“进程没挂”，而是没有非法状态、没有重复终态、没有丢失已确认 workflow。

benchmark 要测吞吐、延迟和恢复成本。指标包括 workflow submit/s、step schedule/s、step completion/s、workflow p50/p95/p99、critical path latency、ready queue depth、oldest blocked age、event append latency、projection lag、replay time、snapshot recovery time、CPU、内存、I/O、网络。还要记录 workflow DAG 形态：链式、宽扇出、菱形依赖、混合 DAG，这些负载的瓶颈完全不同。

还要做 ablation。比如全量扫描依赖 vs 增量 indegree，full replay vs snapshot，FIFO 调度 vs 租户公平调度，小 result inline vs result_ref，resource-only worker selection vs locality-aware。没有对照组，就很难说明设计选择的价值。

面试里可以这样答：

```text
correctness test 测 DAG 依赖、状态机、step 幂等、timeout/cancel 并发和 replay 一致性。
stress test 测大并发提交、大扇出、worker kill、control restart、重复 completion、对象存储失败和 projection lag。
benchmark 测 workflow/step 吞吐、p95/p99、critical path、ready queue、projection lag、replay time 和资源使用，并区分链式、扇出、菱形等 DAG 形态。
```

## Q068. 如果要求从零实现一个简化版 workflow engine，你会先定义哪些不变量？

**回答：**

我会先定义 workflow 不变量，再决定用数据库、日志还是队列。workflow engine 的 bug 往往不是语法层面的，而是“某个 step 被不该调度时调度了”“某个已经成功的 step 被重跑了”“重启后 DAG 状态不一致”。这些问题只能靠不变量压住。

第一条是实例唯一性。`workflow_id` 或业务幂等键唯一；重复提交相同输入返回已有实例；重复 key 但输入不一致返回冲突。

第二条是依赖不变量。一个 step 只有在所有 required upstream 达到指定状态后才能 ready。ready 事件最多产生一次，除非明确进入新的 attempt 或 retry 轮次。

第三条是 step 状态机不变量。step 只能沿合法边转换，终态不可被旧 attempt 覆盖。`SCHEDULED -> STARTED -> SUCCEEDED/FAILED/TIMED_OUT` 这种边要清楚，`SUCCEEDED -> STARTED` 不允许。

第四条是结果不变量。每个 step 的生效结果最多一个，结果通过 result_ref、checksum、size 和 input_hash 关联。下游 step 读到的必须是已提交的上游结果。

第五条是重试不变量。每次 retry 都有 attempt 序号、原因、next_retry_at 和最大次数。可重试错误和不可重试错误要分开。

第六条是 replay 不变量。从 event history 或 shared log 重建出的 workflow state，要和在线 materialized view 一致。metadata view 可以丢，event history 不能丢。

第七条是外部副作用不变量。workflow 决策代码必须可 replay；非确定性操作和外部 I/O 应该放在 activity/step 中，完成结果写入历史，replay 时复用结果，不重新调用外部系统。

第八条是取消和终止不变量。workflow 进入 `CANCELED`、`FAILED`、`SUCCEEDED` 这类终态后，普通 ready/schedule 事件不能继续产生。running step 的后续完成要按版本或 lease 判断是否接受。

LogServe 的简化版正是围绕这些点展开：DAG step model、ready step 调度、step 状态机、retry、timeout、result_ref、replay 校验和 step 级幂等。它没有把所有 Temporal 语义都实现，但核心不变量是可讲的。

面试里可以这样答：

```text
我会先定义实例唯一、依赖满足后才 ready、step 状态机合法、每个 step 只有一个生效结果、retry 有 attempt 和上限、event history 可 replay、外部副作用不在 workflow replay 中重做、终态不再调度新 step。
简化版实现可以少，但这些不变量不能少。
```

## Q069. workflow engine 的常见误用是什么，误用后通常会产生什么线上症状？

**回答：**

第一个误用是把 workflow 当普通函数调用。开发者在 workflow 决策代码里直接读当前时间、随机数、数据库、HTTP API 或 LLM。恢复时 replay 走出不同分支，历史匹配不上；更糟的是外部 API 被重复调用。线上症状是 replay failure、重复副作用、状态无法解释。

第二个误用是把所有失败都重试整个 workflow。某个 step 失败后从头跑，导致已成功 step 重复执行，成本暴涨，外部系统收到重复请求。应该优先重试失败 step 或 activity，除非整个 workflow 明确无状态且可幂等。

第三个误用是 step 粒度不合理。step 太细，会产生大量事件、调度和 metadata 写入；step 太粗，失败后只能重跑很大一段，无法观察内部进度。线上表现是 event history 膨胀、调度开销高，或者失败恢复成本过大。

第四个误用是把大 payload 在 step 之间直接传。metadata DB、日志和 RPC 都被大对象拖慢。正确做法是大结果走对象存储，workflow event 保存 result_ref 和校验信息。

第五个误用是没有版本策略。长期运行的 workflow 跨越发布周期，新代码无法 replay 旧 history。线上表现是升级后老 workflow 大面积卡死，只能回滚或人工修数据。

第六个误用是把 workflow 当实时调度器。大量短小、低延迟、高频任务都塞进 workflow，控制面被事件写入和调度开销淹没。实时路径应该走更轻的服务或专用调度器。

第七个误用是忽视取消语义。用户点取消后，running step 继续写结果，下游 step 还继续调度。线上会看到“已取消 workflow 仍然产生结果”或者“取消后成本还在继续增长”。

在 LogServe 里，比较好的表述是：项目用 workflow DAG 展示可靠编排机制，但不会把它说成完整生产 workflow engine。面试时主动讲这些误用，说明自己知道边界。

面试里可以这样答：

```text
常见误用包括：workflow 代码里做非确定性 I/O、失败后重试整个 workflow、step 粒度过细或过粗、大 payload 直接传递、没有版本策略、把 workflow 当实时调度器、取消语义不清。
线上症状是 replay 失败、重复副作用、event history 膨胀、恢复慢、老 workflow 升级后卡死、取消后仍继续执行。
```

## Q070. workflow engine 在单机和分布式环境中的语义有什么差异？

**回答：**

单机 workflow engine 可以用本地锁、本地日志和一个控制面进程维护 ready queue。崩溃恢复主要处理本地 event history、metadata view 和 worker 进程。只要磁盘还在，恢复路径相对直接。

分布式 workflow engine 要处理更多不确定性。多个 control plane 实例可能同时调度，worker 分布在不同节点，metadata view 可能分片，日志可能复制，网络可能分区，clock 也不可靠。一个 step 是否 timeout、一个 completion 是否 stale、一个 timer 是否触发，都不能只靠本机内存判断。

单机里 ready queue 可以是一个内存结构加恢复逻辑；分布式里 ready queue 要么持久化，要么从日志投影，并且要防止两个 scheduler 重复发同一 step。通常需要 lease、shard ownership、leader election、CAS 或任务版本号。

单机里 workflow history 有一个本地顺序；分布式里要明确顺序边界。一个 workflow 实例的 history 最好落在同一个 shard 或同一个 owner，避免跨 shard 全局排序。跨 workflow 的全局顺序通常不需要，也不该轻易承诺。

单机里 worker 崩溃容易判断；分布式里 heartbeat 丢失可能是网络问题、GC pause、节点故障或控制面分区。完成事件必须带 attempt 和 lease_epoch，旧 worker 恢复后不能覆盖新 attempt。

单机实验的 benchmark 也不能外推到分布式。LogServe 的单机 workflow p95/p99 和 control restart probe 能证明机制，但分布式生产还要证明多副本日志、跨节点调度、shard rebalance、leader failover、history 复制和跨区域恢复。

面试里可以这样答：

```text
单机 workflow 主要处理本地日志、内存队列恢复和 worker 崩溃；分布式 workflow 还要处理多控制面重复调度、网络分区、shard ownership、leader failover、复制提交点和 stale completion。
所以分布式语义要加 workflow shard owner、lease_epoch、CAS、持久 ready queue、history 复制和 timer 恢复。单机能证明机制，不能直接证明多节点语义。
```

## Q071. actor service 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

actor service 的核心目标是让有身份、有私有状态的对象，通过消息串行化来安全地处理并发。它首先解决正确性和并发模型问题：外部世界可以并发发送很多请求，但同一个 actor 的状态更新必须按清楚的顺序发生。

Actor 模型的价值在于把共享内存锁竞争变成消息边界。Akka 文档里强调 actor 一次处理一条消息，因此 actor 内部状态通常不需要 `synchronized` 或 `AtomicInteger` 这种并发保护。Orleans 的 grain reference 则把逻辑身份和物理位置分开：调用方持有的是逻辑引用，不需要知道 actor 当前在哪台机器上。

正确性体现在三件事。第一，同一个 actor 的 command 要串行处理，不能两个 worker 同时改同一份状态。第二，状态恢复要有事实来源，例如 command log、snapshot 或 durable state。第三，ownership 要有 fencing，旧 owner 或旧 epoch 不能在失联后继续提交状态。

性能是第二层目标。actor service 可以把状态局部化，减少数据库读写，提升局部缓存命中；也可以按 actor id 分片，把不同 actor 并行处理。但同一个 actor 内部通常是串行的，不能把 actor 当成提高单对象并行度的工具。

安全性涉及 actor id、tenant_id、消息权限、状态隔离和审计。多租户 actor service 不能让一个租户猜到 actor id 后访问另一个租户的状态，也不能让内部 actor 消息绕过权限检查。

可维护性来自封装。actor 把状态和处理逻辑放在一个边界里，比到处传锁和共享 map 更容易解释。但滥用 actor 也会让系统变成一团互相发消息的黑盒，所以 actor 的协议、生命周期和观测要清楚。

LogServe 的 actor runtime 可以这样讲：每个 actor 以 `actor:<actor_id>` stream 为状态真相；command 有单调 `command_seq`；mailbox 保证同一 actor 串行化；snapshot 写 result store；replay 从 snapshot 加 tail log 恢复；`owner_worker_id + epoch` 用来拒绝旧 worker 的写入。这个设计展示机制，不等于完整 Orleans/Akka 集群。

面试里可以这样答：

```text
actor service 首先解决有状态对象的并发正确性：同一个 actor 的消息串行处理，状态更新有顺序，恢复有 command log 或 snapshot，ownership 用 epoch fencing 防止旧 owner 写入。
性能收益来自状态局部性和跨 actor 并行；安全要靠 tenant、权限和审计；可维护性来自清晰的 actor 协议和生命周期。
```

## Q072. actor service 的典型适用场景和不适用场景分别是什么？

**回答：**

actor service 适合“有身份、有状态、请求围绕同一个对象串行发生”的场景。比如游戏房间、玩家会话、协作文档、设备状态、IoT 设备控制、购物车、聊天房间、订单状态机、租户配额对象、模型缓存管理对象。这些场景的共同点是：状态和对象身份强绑定，同一个对象内部需要顺序，不同对象之间可以并行。

它也适合减少共享锁的系统。与其让很多 goroutine 或线程同时改一个全局 map，不如把每个 key 的状态归到一个 actor，由 mailbox 串行处理。这样并发边界更清楚，状态修改也更容易审计和 replay。

actor 还适合需要局部恢复的状态对象。比如 LogServe 的 actor command 可以从 snapshot 和 tail log 恢复，不需要整个系统重放所有任务历史。actor 粒度选得好，恢复成本可控。

不适用场景也要清楚。第一类是无状态请求。纯计算、简单查询、静态文件服务，用 actor 只会增加路由、mailbox 和生命周期管理成本。

第二类是单个对象需要高并行写入的场景。actor 的核心是单对象串行处理，如果一个 actor 变成超级热点，例如全站唯一库存、全局排行榜、单个租户全量配额对象，就会变成吞吐瓶颈。需要拆分、分片、CRDT、数据库事务或专用计数器，而不是硬扛一个 actor。

第三类是复杂关系查询。actor 擅长封装单对象状态，不适合做任意 join、聚合、全文搜索和跨对象分析。这些应该由数据库、搜索引擎或投影视图承担。

第四类是强事务跨多个 actor 的操作。两个 actor 之间转账、多个库存同时扣减，如果需要严格原子性，actor 消息本身不够。可以用 saga、两阶段提交、事务存储或业务补偿，但不能假装多 actor 消息天然事务。

第五类是状态没有明确 owner 的共享资源。比如全局 GPU 调度、对象存储带宽、数据库连接池，这些更像资源调度器，不一定适合按 actor 建模。

LogServe 里 actor 适合讲有状态对象恢复、mailbox 串行化、snapshot 和 epoch fencing。不适合把所有任务都变成 actor，也不适合声称 actor 解决了跨对象事务。

面试里可以这样答：

```text
actor service 适合有身份、有私有状态、同一对象需要顺序处理的场景，例如游戏房间、设备状态、聊天房间、购物车、订单状态机、租户配额和模型缓存对象。
它不适合无状态请求、单对象超高并发写、复杂关系查询、严格跨 actor 事务和没有明确 owner 的共享资源。
关键判断是：状态是否能按 actor id 封装，同一 actor 串行是否可接受，不同 actor 是否能并行。
```
## Q073. actor service 和相近概念最容易混淆的边界在哪里？

**回答：**

第一个边界是 actor 和普通对象。普通对象只是内存里的结构，调用方可以直接方法调用；actor 有身份、mailbox、消息协议和生命周期。actor 的调用通常是异步消息，调用方不应该直接访问 actor 内部状态。

第二个边界是 actor 和线程。actor 不是一个线程，也不应该假设一个 actor 永远占一个线程。actor runtime 会把很多 actor multiplex 到少量线程或 worker 上。把 actor 当线程用，会创建过多 actor、阻塞 dispatcher，最后性能很差。

第三个边界是 actor 和任务。任务是离散工作，执行完就结束；actor 是有身份的长期状态对象，可以连续处理很多 command。actor command 可以由任务执行器承载，但 actor 的状态顺序、ownership、snapshot 和 replay 是额外语义。

第四个边界是 actor 和 workflow。workflow 关注流程依赖和长时间执行；actor 关注某个对象的状态封装和消息串行化。一个 workflow 可以调用 actor，一个 actor 也可以触发 workflow，但两者不能混成一个概念。

第五个边界是 actor 和数据库行。数据库行提供持久状态和事务查询，actor 提供运行时行为和消息处理。可以用数据库持久化 actor state，但 actor 不适合替代数据库的任意查询、索引和事务。

第六个边界是 actor 和锁。actor 可以减少显式锁，但不代表没有并发问题。跨 actor 消息仍然可能乱序、丢失、重试；actor 内部如果阻塞，会堵住 mailbox；多个 actor 的一致性仍然需要协议。

第七个边界是 actor 和微服务。微服务是部署和业务边界，actor 是运行时并发模型。一个服务内可以有很多 actor；把每个 actor 做成独立微服务通常会造成运维和网络开销失控。

LogServe 的 actor 模型要强调“同一 actor 的 command_seq 和 mailbox”，不要泛化成“所有状态都应该 actor 化”。项目里的 actor service 是为了展示有状态对象恢复和 fencing，不是替代数据库、workflow 或微服务边界。

面试里可以这样答：

```text
actor 容易和普通对象、线程、任务、workflow、数据库行、锁和微服务混淆。
我的边界是：actor 是有身份、有 mailbox、有消息协议和生命周期的状态对象；它不是线程，不是一次性任务，不负责复杂查询，也不能自动解决跨 actor 事务。
它解决的是单 actor 内部状态顺序和封装，跨 actor 一致性还要额外设计。
```

## Q074. actor service 在高并发场景下可能出现哪些隐藏问题？

**回答：**

第一个隐藏问题是热点 actor。actor 模型允许不同 actor 并行，但同一个 actor 通常串行处理。一个全局 actor、一个大租户 actor、一个热门房间 actor，都会把所有请求排进同一个 mailbox。系统整体 worker 很空，但某个 actor 的延迟爆炸。

第二个问题是 mailbox 膨胀。调用方并发发送消息，actor 处理不过来，mailbox 越积越多。内存增长、GC 变慢、消息超时、调用方重试，最后可能形成放大循环。需要 mailbox 上限、backpressure、丢弃策略、优先级或分片。

第三个问题是阻塞 actor。actor 处理消息时直接做慢 I/O、调用外部 API、跑重 CPU 计算，会堵住后续消息。actor 内部应该尽量快地更新状态，把慢操作交给 task/activity，然后通过回调消息继续推进。

第四个问题是跨 actor 调用死锁或等待链。actor A 等 B，B 等 C，C 又等 A；或者 actor 在处理消息时同步等待另一个 actor 回复。线上会表现为 mailbox 不断增长、请求超时，但 CPU 不高。

第五个问题是 placement 不均衡。actor 按 hash 分配到 worker，如果热门 actor 集中到某些节点，集群资源利用率会很差。需要 actor placement、迁移、分片或按负载 rebalance。

第六个问题是 snapshot 和 replay 压力。高频 actor 每条 command 都写日志，snapshot 太少会导致恢复慢；snapshot 太频繁又会压对象存储和 I/O。logical trim 也要确保 snapshot 已经持久化，否则会断掉恢复链。

第七个问题是消息去重和顺序。网络重试可能让同一 command 到达两次，或者旧 epoch 的 owner 继续提交结果。必须有 command_id、command_seq、epoch fencing 和幂等 apply。

LogServe 中这些问题可以对应到 mailbox 串行化、command_seq、snapshot replay、logical trim、epoch fencing。当前实验中 actor snapshot replay 从 21 条 command 降到 1 条 command，说明 snapshot 机制有效；生产高并发还要测热点 actor、mailbox 上限和 placement。

面试里可以这样答：

```text
actor 高并发下最容易出问题的是热点 actor、mailbox 膨胀、actor 内部阻塞、跨 actor 等待链、placement 不均衡、snapshot/replay 压力和重复消息。
解决思路是按 actor id 分片，限制 mailbox，慢 I/O 外移，避免同步等待链，做负载感知 placement，用 command_seq、command_id 和 epoch fencing 保证顺序和去重。
```

## Q075. actor service 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

第一个边界是 actor owner 崩溃。某个 worker 持有 actor ownership，正在处理 command，突然失联。系统要判断 ownership 何时过期，新的 worker 如何接管，旧 worker 如果恢复后继续提交结果是否会被拒绝。没有 epoch fencing，就容易出现两个 owner 同时写状态。

第二个边界是 command 已执行但 apply 事件没写入。actor 可能已经做了外部副作用，但 `ActorCommandApplied` 没有写入日志。恢复时如果重放 command，外部副作用可能重复。对外部副作用要使用 idempotency key，或者把外部操作放在可重试任务里，actor 只在确认结果后提交状态。

第三个边界是 snapshot 写了一半。snapshot 先写对象存储，再写 `ActorSnapshotCreated` 事件；如果对象写失败，不能 trim 日志；如果事件写失败，snapshot 对恢复不可见。恢复时必须验证 snapshot_ref、checksum 和 snapshot 对应的 command_seq。

第四个边界是 command 超时。调用方认为超时后重试，但 actor 可能稍后完成原 command。系统要能用 command_id 或 client request id 去重，避免同一个意图应用两次。

第五个边界是 mailbox 中的消息在重启后怎么办。如果 mailbox 只是内存队列，重启后未处理 command 会丢。可靠 actor service 应该把 command submitted 事件或 pending command 持久化，恢复后按 command_seq 继续。

第六个边界是跨 actor 消息。actor A 发消息给 actor B 后崩溃，B 是否收到、是否处理、A 是否记录了这个意图，都可能不清楚。需要 outbox、消息 id、ack、重试和幂等接收。

第七个边界是 actor 代码升级。旧 snapshot 或旧 command log 要能被新代码读取；如果状态 schema 改了，要有版本和 migration。否则重启恢复时会失败。

LogServe 的 actor runtime 用 `owner_worker_id + epoch` 防旧 worker，command_seq 防乱序，snapshot + tail log 优化恢复。面试里要补一句：当前实现验证的是单机机制，多节点 actor placement、跨 actor 消息可靠性和长期 schema migration 仍是生产化工作。

面试里可以这样答：

```text
actor 故障场景会暴露 owner 崩溃、旧 owner 回写、command 执行成功但 apply 事件丢失、snapshot 半成功、调用方超时重试、内存 mailbox 丢失、跨 actor 消息不确定和状态 schema 升级问题。
我会用 ownership epoch fencing、command_seq、command_id 去重、snapshot checksum、持久 command log、outbox 和 schema version 处理这些边界。
```

## Q076. actor service 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

actor service 的瓶颈通常先看热点 actor 和 mailbox，而不是先看机器整体 CPU。actor 模型的并行性来自不同 actor 并行；同一个 actor 内部串行。如果负载集中在少数 actor，扩 worker 数也不一定有用。

CPU 瓶颈来自 actor 消息处理逻辑、序列化、反序列化、状态转换、snapshot 编码、replay 和 placement 计算。如果 actor 内部做重 CPU 工作，会堵住 mailbox；更好的做法是把重 CPU 任务交给 executor，actor 只记录状态和接收结果。

内存瓶颈来自大量 actor activation、mailbox backlog、状态缓存、snapshot buffer、pending request map 和去重表。Orleans 这类 virtual actor 系统会涉及 activation/deactivation，目的是不用把所有逻辑 actor 永远驻留内存。简化实现也要考虑冷 actor 的释放和恢复。

锁竞争可能出现在全局 actor registry、placement table、mailbox map、ownership map、snapshot manager、metrics collector。actor 内部少锁不代表 runtime 没锁。大量短消息会把这些中心结构打热。

I/O 瓶颈来自 command log、snapshot store、durable state、result store 和审计日志。每条 command 都写日志，snapshot 又写对象存储，I/O 策略会直接影响吞吐和恢复时间。

网络瓶颈来自远程 actor 调用、跨节点 placement、跨 actor 消息、snapshot 读取、客户端连接。actor 调用如果变成细粒度远程聊天，会产生大量小 RPC，延迟和网络开销会很高。

LogServe 的 actor benchmark 重点展示 snapshot replay 成本下降。生产性能还要看热点 actor p99、mailbox depth、commands/s、snapshot bytes/s、activation 数、ownership handoff latency 和 stale completion rejection。

面试里可以这样答：

```text
actor service 的瓶颈往往是热点 actor 和 mailbox backlog。CPU 用在消息处理、序列化和 replay；内存用在 activation、mailbox、状态缓存和去重表；锁竞争在 actor registry、placement 和 ownership；I/O 在 command log 和 snapshot；网络在远程 actor 调用和跨节点消息。
扩容前要先看负载是否集中在少数 actor，否则加节点也救不了单 actor 串行瓶颈。
```

## Q077. actor service 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

correctness test 要测单 actor 顺序和恢复语义。比如 command_seq 必须单调；只有 `command_seq == current + 1` 的 command 可以应用；重复 command_id 不会重复生效；旧 epoch owner 的完成被拒绝；snapshot 后 replay 得到和 full replay 相同的状态；logical trim 不会破坏恢复。

还要测 actor 协议。非法消息要被拒绝或进入明确错误；actor 不存在时是否自动创建要按系统语义决定；actor 删除后是否还能收消息；状态 schema 升级后旧 snapshot 是否能读。actor service 很怕“协议没定义但实现碰巧能跑”。

stress test 要测并发和故障。比如大量客户端同时给同一个 actor 发 command，大量 actor 同时活跃，worker kill，owner handoff，snapshot 写入时崩溃，重复消息，旧 owner 恢复后回写，mailbox 积压，placement rebalance。通过标准是状态顺序不乱、不会双 owner 写入、不会丢 pending command。

benchmark 要分单 actor 和多 actor。单 actor 测串行处理上限、mailbox p99、command apply latency、snapshot/replay 时间；多 actor 测 actor/s、commands/s、placement 均衡、worker CPU、内存、网络、snapshot throughput。只报全局 commands/s 会掩盖热点 actor 的尾延迟。

还要做恢复 benchmark。比如 full replay 100、1,000、10,000 条 command 的时间；snapshot 间隔对恢复时间和写放大的影响；snapshot + tail log 和 full replay 的对比。LogServe 的 actor snapshot replay 从 21 条降到 1 条就是一个小规模 ablation。

面试里可以这样答：

```text
correctness test 测 command_seq、重复 command 去重、epoch fencing、snapshot/full replay 一致、logical trim 安全和协议错误处理。
stress test 测同一 actor 高并发、海量 actor、worker kill、owner handoff、snapshot 中断、旧 owner 回写、mailbox backlog。
benchmark 要分单 actor 与多 actor：单 actor 看串行上限和 mailbox p99，多 actor 看 commands/s、placement 均衡、内存、I/O、snapshot/replay 时间。
```

## Q078. 如果要求从零实现一个简化版 actor service，你会先定义哪些不变量？

**回答：**

我会先定义 actor 身份和顺序。每个 actor 有稳定的 `actor_id`，同一 `actor_id` 在任意时刻最多有一个有效 owner；每条 command 有唯一 `command_id` 和单调 `command_seq`；状态只能按 command_seq 顺序推进。

第二条是 mailbox 不变量。提交成功的 command 不能只存在内存里；恢复后未应用 command 仍能被找到。对于不要求强持久的 actor 可以放松，但可靠 actor service 必须说清楚语义。

第三条是 ownership 不变量。owner 变更必须增加 epoch。任何 apply、snapshot、completion 都要带 epoch；旧 epoch 的写入必须被拒绝。这是防 split-brain 的底线。

第四条是 apply 不变量。一个 command 最多生效一次。重复 command_id 返回已有结果或当前状态，不重复修改 actor state。

第五条是恢复不变量。actor 状态可以从 snapshot + tail log 或 full command log 重建。snapshot 必须标明覆盖到哪个 command_seq，并带 checksum 或版本。

第六条是 trim 不变量。只有在 snapshot 可读且覆盖到某个 command_seq 后，才能逻辑 trim 之前的日志。trim 后 replay 起点要明确，不能让 actor 历史断掉。

第七条是协议不变量。actor 接受哪些消息、返回什么结果、哪些错误可重试、哪些错误终止，都要在协议中定义。不能靠运行时反射随便发任意 payload。

第八条是租户隔离不变量。actor_id 必须和 tenant_id 绑定；不同租户不能通过猜 actor_id 访问对方状态；审计要记录谁提交了 command。

LogServe 的 actor runtime 基本围绕这些不变量：actor stream、command_seq、mailbox 串行化、snapshot replay、epoch fencing。简化版可以不实现复杂 placement，但这些状态语义要先定。

面试里可以这样答：

```text
我会先定义 actor_id 稳定、单 actor 单有效 owner、command_seq 单调、command_id 幂等、mailbox 可恢复、owner epoch fencing、snapshot + tail log 可重建、trim 不破坏恢复、消息协议明确、tenant 隔离。
实现可以从单机开始，但这些不变量决定它以后能不能扩展到分布式。
```
## Q079. actor service 的常见误用是什么，误用后通常会产生什么线上症状？

**回答：**

第一个误用是把所有东西都建成 actor。每个请求、每个临时对象、每个小函数都变成 actor，最后 runtime 在创建、路由、序列化和 mailbox 管理上花掉大量成本。线上会看到 actor 数量暴涨、内存高、GC 慢、调度开销大。

第二个误用是在 actor 内部做阻塞 I/O。actor 一次处理一条消息，里面直接访问慢数据库、对象存储、第三方 API 或跑重 CPU，后续消息全被堵住。症状是 mailbox depth 持续增长，单 actor p99 很高，但机器 CPU 不一定满。

第三个误用是用一个全局 actor 管所有状态。这样确实避免了锁，但把整个系统变成单线程瓶颈。比如全局配额 actor、全局排行榜 actor、全局调度 actor。高峰时所有请求排队，扩容无效。

第四个误用是忽视跨 actor 一致性。开发者以为 actor 消息串行就等于全局事务，结果两个 actor 之间转账、库存扣减、订单状态同步出现一半成功一半失败。跨 actor 操作需要 saga、outbox、幂等消息或事务存储。

第五个误用是没有 backpressure。调用方无限发送消息，actor 处理不过来，mailbox 堆积；调用方超时后重试，消息更多。最终内存上涨、延迟上升、重复 command 增多。

第六个误用是没有持久化策略。actor state 只在内存里，worker 重启后状态丢；或者 snapshot 做了但没有 command log，恢复后无法解释 snapshot 之后发生了什么。可靠 actor 至少要说清楚哪些状态可丢，哪些状态必须从日志或存储恢复。

第七个误用是 actor 协议太随意。消息 payload 没版本、没 command_id、没 tenant_id，后续升级和排障都困难。actor 看起来封装了状态，实际上把复杂性藏进了不透明消息。

LogServe 的答法要主动避开这些坑：它用 actor 展示有状态对象恢复，不是把所有任务都 actor 化；它有 command_seq、epoch fencing 和 snapshot replay，但生产化还要加 mailbox limit、placement、跨 actor outbox 和 schema migration。

面试里可以这样答：

```text
actor 常见误用包括：滥建 actor、actor 内阻塞 I/O、全局热点 actor、把跨 actor 消息当事务、没有 backpressure、状态只放内存、消息协议没版本和幂等键。
线上症状是 actor 数量爆炸、mailbox 堆积、单 actor p99 高、扩容无效、跨对象状态不一致、重启丢状态和升级后消息解析失败。
```

## Q080. actor service 在单机和分布式环境中的语义有什么差异？

**回答：**

单机 actor service 的语义比较直接。actor registry、mailbox、owner、状态缓存都在一个进程或一台机器内，顺序可以靠本地队列和锁保证。崩溃恢复主要依赖本地 command log、snapshot 和 worker 重启。

分布式 actor service 多了位置透明和 ownership 问题。调用方拿到 actor 引用，不应该关心 actor 当前在哪个节点；runtime 要负责 placement、路由、迁移和故障接管。Orleans 的 grain reference 就体现了这个思路：引用代表逻辑身份，和物理位置解耦。

单机里一个 actor 不太会出现双 owner；分布式里网络分区、心跳延迟、GC pause、leader 切换都可能让两个节点都以为自己拥有 actor。必须用 epoch、lease、term 或 fencing token 拒绝旧 owner 写入。

单机 mailbox 是本地队列；分布式 mailbox 要决定是否持久化、是否迁移、是否允许重投递。节点宕机时，已经投递但未 apply 的消息是否丢失，取决于 command 是否已经写入 durable log。

单机 actor 调用是本地消息；分布式 actor 调用是网络 RPC，可能超时、重试、乱序、重复。调用方不能因为超时就认为 actor 没处理；接收方也不能因为收到两次就应用两次。

单机 snapshot 只要本地可读；分布式 snapshot 要考虑对象存储、跨节点访问、版本、checksum 和权限。actor 迁移到新节点后，必须能从共享存储或复制状态恢复。

LogServe 当前更接近单机/多进程机制验证。它可以说明 actor stream、mailbox、command_seq、snapshot replay 和 epoch fencing 的机制；分布式生产还要补 placement service、membership、跨节点 routing、持久 mailbox、actor migration 和多副本日志。

面试里可以这样答：

```text
单机 actor 主要靠本地 mailbox 和本地状态顺序；分布式 actor 要处理位置透明、placement、routing、membership、双 owner、网络超时、重复消息和跨节点恢复。
所以分布式语义必须有 owner epoch fencing、持久 command log、幂等 command_id、共享 snapshot store 和 actor migration 机制。
单机实现能证明串行化和恢复思路，但不能直接证明集群语义。
```

## Q081. LLM scheduler 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

LLM scheduler 的核心目标是在有限 GPU、内存、模型缓存和请求 SLO 之间做取舍，把请求放到更合适的 worker 或 batch 上。它主要解决性能和资源效率问题，同时也会影响正确性、安全性和可维护性。

性能目标最直接。LLM serving 里，一次请求的成本不只是生成 token，还包括模型是否已加载、checkpoint 是否在本地、KV cache 是否可复用、prefill 长度、decode 阶段批处理、GPU memory 是否足够、是否会触发 preemption。vLLM 的文档里，chunked prefill 就是为了把大 prefill 切小，与 decode 请求一起 batch，从而平衡 compute-bound 的 prefill 和 memory-bound 的 decode。

调度器还要感知缓存。自动 prefix caching、模型 checkpoint cache、worker model cache 都会影响延迟。同样的请求发到已经有模型和前缀缓存的 worker，可能比发到空闲但冷的 worker 更快。简单看 CPU/GPU utilization 不够，必须结合 locality。

正确性体现在请求不能被错误路由。模型名、版本、adapter、租户权限、上下文长度、量化配置、工具能力、数据隔离都要匹配。调度器如果把请求发到错误模型版本，延迟再低也是错的。

安全性体现在租户隔离和缓存隔离。KV cache、prefix cache、checkpoint cache、prompt、embedding、中间结果都可能含敏感数据。多租户场景下不能为了缓存命中把不同租户的私有前缀混用，也不能让一个租户通过 timing 或 cache hit 推断另一个租户的请求内容。

可维护性来自可解释的决策。调度器应该能说明为什么选这个 worker：模型已加载、cache hit、队列短、预计延迟低、租户配额允许、没有违反 SLO。否则线上 p99 出问题时，只能猜测。

LogServe 的 LLM scheduler 是一个适合面试的机制样例：`RESOURCE_ONLY`、`LOCALITY_AWARE`、`PREDICTED_LATENCY` 三种策略；worker 心跳上报 model cache；checkpoint cache 支持 cold/warm；`PREDICTED_LATENCY` 使用 `LLMCompleted` 增量维护 EWMA stats，不在热路径扫描日志。边界也要讲清：当前主要是 mock LLM 和单机实验，vLLM adapter 已实现，但没有真实 GPU 负载结论。

面试里可以这样答：

```text
LLM scheduler 主要解决性能和资源效率：在 GPU、KV cache、模型加载、prefix cache、checkpoint cache、队列等待和 SLO 之间做选择。
但它也有正确性边界：模型版本、租户权限、上下文长度和 adapter 必须匹配；安全边界是缓存不能跨租户泄露；可维护性来自可解释的调度原因和指标。
```

## Q082. LLM scheduler 的典型适用场景和不适用场景分别是什么？

**回答：**

LLM scheduler 适合多模型、多 worker、多租户、请求形态差异大的推理服务。比如同一平台服务多个模型版本，有些 worker 已经加载了模型 A，有些 worker 有模型 B 的 checkpoint；不同请求有不同 prompt 长度、输出长度、SLO、租户优先级和缓存命中可能。此时调度器的选择会明显影响 TTFT、ITL、吞吐和 GPU 利用率。

它也适合有明显 locality 的场景。比如 RAG 系统里很多请求共享系统 prompt 或文档前缀；agent 系统里同一会话反复调用同一模型；企业租户有固定模型和固定工具集；同一个 worker 已经有模型 checkpoint 或 prefix cache。调度器如果能识别这些局部性，就能减少冷启动和重复 prefill。

它还适合资源昂贵的环境。GPU 内存、KV cache、模型权重加载时间都很贵，调度器能通过 batching、preemption 控制、chunked prefill、cache-aware routing 改善资源利用率。

不适用场景也有。第一类是单模型、单 worker、低 QPS。没有选择空间，复杂调度器只会增加开销。

第二类是强隔离租户。某些租户要求独占 GPU、独占缓存、独占模型副本，此时 locality-aware 的共享调度空间很小，重点变成容量预留和隔离，而不是全局混排。

第三类是对延迟极端敏感但请求很短的场景。如果调度器为了 batch 等待太久，可能吞吐上去了，用户延迟反而变差。需要根据 SLO 选择 batch window 和优先级，不是越合批越好。

第四类是缓存不可复用或风险太高的场景。随机 prompt、低重复率、敏感跨租户 prompt、频繁模型版本切换，都可能让 cache-aware 调度收益很低，甚至增加错误路由和泄露风险。

LogServe 的适用场景是展示模型缓存感知调度机制：3 worker、缓存不均匀、mock LLM 下 locality-aware 把 cache hit rate 提高、p95 降低。不能把这个结果直接说成真实 GPU 集群收益，只能说机制方向成立，真实收益要用 vLLM/GPU workload 验证。

面试里可以这样答：

```text
LLM scheduler 适合多模型、多 worker、多租户、有缓存局部性和 GPU 资源紧张的推理服务。它能利用模型已加载、checkpoint cache、prefix cache、队列长度和 SLO 做路由。
它不适合单模型单 worker、低 QPS、强隔离独占租户、极短低延迟请求或缓存复用率很低的场景。
关键是有调度选择空间，而且缓存命中带来的收益大于调度开销和隔离风险。
```

## Q083. LLM scheduler 和相近概念最容易混淆的边界在哪里？

**回答：**

第一个边界是 LLM scheduler 和负载均衡器。普通负载均衡器通常按连接数、延迟、权重、健康状态转发请求；LLM scheduler 还要理解模型版本、GPU memory、KV cache、prefix cache、batch、prefill/decode 阶段、上下文长度、租户 SLO。它不是简单的 round-robin。

第二个边界是 LLM scheduler 和推理引擎。vLLM、TensorRT-LLM、SGLang 这类引擎负责实际推理、batching、KV cache 管理、attention kernel、并行策略。scheduler 可以调用这些引擎，也可以在服务层做 worker 选择，但不等于自己实现 attention kernel。

第三个边界是模型缓存和 KV cache。模型缓存通常指权重、checkpoint 或 adapter 是否在本地；KV cache/prefix cache 指某些 prompt token 的注意力缓存是否可复用。两者都影响延迟，但生命周期、隔离风险和命中条件不同。

第四个边界是 admission control。调度器选择“去哪儿跑”，admission control 决定“现在是否接”。当 GPU memory 不够、队列过长、租户超配额时，正确动作可能是拒绝、排队或降级，而不是硬调度。

第五个边界是 workflow/task scheduler。普通 task scheduler 关心 CPU、内存、worker 可用性和任务类型；LLM scheduler 还要考虑 token 级别成本、prefill/decode 不同阶段、batch 组成和缓存复用。把 LLM 请求当普通 CPU task 调度，会低估 GPU 内存和 KV cache。

第六个边界是 RAG retrieval。RAG 的 retrieval/rerank 决定给模型什么上下文；LLM scheduler 决定生成请求发到哪里、怎样 batch、是否命中缓存。两者可以协同，但职责不同。

LogServe 的边界是清楚的：它实现的是 model registry、mock/vLLM adapter、worker model cache 上报、checkpoint cache 和三种调度策略；没有声称自己实现了 vLLM 内部 continuous batching 或 kernel 优化。

面试里可以这样答：

```text
LLM scheduler 不等于普通负载均衡器，也不等于推理引擎。
它负责根据模型版本、worker cache、KV/prefix cache、GPU memory、队列、租户 SLO 和预计延迟选择执行位置；推理引擎负责实际 batching、KV 管理和 kernel；admission control 决定是否接收；RAG retrieval 决定上下文。
模型缓存和 KV cache 也要分开讲，命中条件和隔离风险不同。
```

## Q084. LLM scheduler 在高并发场景下可能出现哪些隐藏问题？

**回答：**

第一个隐藏问题是只看 GPU 利用率。GPU utilization 高不代表调度好，可能 decode 请求被长 prefill 阻塞，ITL 变差；也可能 batch 很满但 TTFT 很高。vLLM 的 chunked prefill 之所以有意义，就是因为 prefill 和 decode 的资源特征不同，调度要平衡两者。

第二个问题是 KV cache 压力。高并发长上下文请求会快速吃掉 KV cache，触发 preemption、recompute、swap 或拒绝。vLLM 文档也建议通过调整 GPU memory utilization、max_num_seqs、max_num_batched_tokens、并行策略等手段减少 preemption。调度器如果不看 KV cache，只看 worker 空闲，会把请求塞进即将抖动的节点。

第三个问题是缓存命中和公平性冲突。为了 cache hit，调度器可能持续把某个租户或某类请求发到同一 worker，导致局部热点；或者热门模型占满缓存，冷门租户的模型不断被驱逐。需要在 locality、fairness、SLO 和 eviction cost 之间做权衡。

第四个问题是请求长度差异。短请求和长请求混在一起，长 prompt 会拖慢短请求，长输出会占住 decode slot。调度器需要估计 prompt tokens、max output tokens、历史速度和 deadline，不能只按请求数排队。

第五个问题是队列位置不透明。网关队列、scheduler 队列、engine 内部 waiting queue、GPU batch queue 都可能排队。只观察入口延迟会漏掉内部 backlog；只观察 engine metrics 又不知道租户和业务优先级。

第六个问题是缓存隔离。高并发多租户下，prefix cache 或 checkpoint cache 的 key 设计如果缺少 tenant、model version、adapter、tokenizer、system prompt hash，就可能错误复用或泄露信息。为了性能跨租户共享缓存，风险很高，必须有明确策略。

第七个问题是观测高基数。每个 prompt hash、session id、request id、tenant、model version 都想打指标，最后 metrics 系统被打爆。高基数字段应放 trace/log，聚合指标保留 tenant、model、worker、cache_hit、preemption_count、TTFT、ITL、queue_wait 这些受控维度。

第八个问题是调度热路径过重。LogServe 曾把 predicted latency 从调度时扫描 `llm:*` 日志，改成基于 `LLMCompleted` 维护 materialized EWMA stats，这个改动就很典型。高并发下，调度器不能每次请求都全量扫描历史。

面试里可以这样答：

```text
LLM scheduler 高并发下的隐藏问题包括：只看 GPU 利用率、KV cache 不足导致 preemption、cache locality 和公平性冲突、长短请求互相影响、多层队列不可见、跨租户缓存隔离风险、指标高基数、调度热路径扫描历史。
我会同时看 TTFT、ITL、throughput、queue wait、KV cache usage、preemption、cache hit、cold start、tenant fairness 和 SLO violation，而不是只看 QPS 或 GPU utilization。
```