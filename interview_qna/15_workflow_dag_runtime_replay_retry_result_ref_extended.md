# 五、Workflow DAG Runtime、Replay、Retry 与 Result Ref（拓展）

## Q366. Temporal 的 workflow replay 与 LogServe 的 workflow replay 有什么不同？

Temporal 的 replay 和 LogServe 的 replay 解决的不是同一个层面的问题。

Temporal replay 的核心是 deterministic workflow execution。Workflow 代码本身会被重新执行，SDK 在重新执行时把历史事件喂给 workflow 代码。只要代码是确定性的，重新执行就能走到和历史一致的位置。Timer、activity result、signal 这些都来自 history，不允许 workflow 代码直接用不可控的时间、随机数、网络调用来影响分支。

LogServe 当前的 workflow replay 更像事件投影重建。它不会重新执行 Python workflow 函数，也不会重新跑用户 task。它读取 `wf:<workflow_id>` stream，把 `WorkflowStarted`、`StepScheduled`、`StepStarted`、`StepSucceeded`、`StepFailed`、`WorkflowCompleted` 这些事件按顺序应用到内存 state，再和 metadata view 对比。

可以这样区分：

- Temporal replay 是“用历史事件驱动 workflow 代码重放”。
- LogServe replay 是“用事件日志重建 workflow 状态”。

Temporal 的模型更强，能支持长时间 workflow、timer、signal、版本兼容等复杂语义。代价是用户 workflow 代码必须满足确定性约束。LogServe 当前模型更轻，DAG 在提交时已经展开，replay 只恢复 step 状态和结果引用，比较适合解释 task DAG 的执行历史。

面试里我会强调：LogServe 没有假装自己已经有 Temporal 那套 deterministic replay。它现在的 replay 目标是恢复控制面状态，并证明 metadata 不是 source of truth。

## Q367. Airflow DAG 和 LogServe DAG 在调度粒度上有什么差异？

Airflow 更偏离线调度和数据工程编排。它的 DAG 通常是提前定义好的，按 schedule interval、cron、dataset event 触发。调度粒度是 DAG run 和 task instance，重点是批处理任务的依赖、重试、补数、调度周期和运维可视化。

LogServe 的 DAG 是一次 workflow 提交时由 Python SDK trace 出来的。它不是先写一个长期挂在调度器里的 DAG 文件，再按周期运行，而是用户通过 SDK 提交一个 workflow request，控制面把这次请求的 definition 写入 workflow stream，然后调度 ready step。

差异主要在这里：

- Airflow DAG 更静态，适合周期性任务。
- LogServe workflow 更请求驱动，适合在线任务、RAG 流程、LLM 调用和 actor 交互。
- Airflow 的 task instance 往往是较粗粒度的作业，可能跑 Spark、SQL、脚本。
- LogServe 的 step 更接近 runtime task，可以和 worker lease、result_ref、LLM 调度、actor mailbox 结合。
- Airflow 的元数据库是核心状态存储；LogServe 的设计里 shared log 才是 source of truth，metadata view 可以 replay。

如果要一句话说：Airflow 更像数据平台的定时编排器，LogServe 更像面向 AI runtime 的事件驱动 DAG 执行层。

## Q368. Ray task DAG 和 LogServe workflow 有什么相似点？

Ray 和 LogServe 都可以把一组函数调用组织成依赖图。一个 task 的输出可以作为另一个 task 的输入，runtime 根据依赖关系调度 ready task。

相似点包括：

- 都把用户函数作为执行单元。
- 都有 task 之间的数据依赖。
- 都可以表达 fan-out/fan-in。
- 都需要处理 worker 执行、失败和结果传递。
- 都能把 Python 写法包装成分布式执行语义。

区别也很大。Ray 更关注分布式计算和对象引用，核心是把 Python task 和 actor 高效调度到集群上，object store 是它的关键组件。LogServe 更关注 log-first 语义、失败恢复、workflow replay、幂等提交和 AI serving 调度。

Ray 的对象引用主要用于运行时数据传递。LogServe 的 `StepRef` 是提交时 trace 出来的逻辑引用，真正执行时通过 `ResolveArgs` 替换成上游结果或 `result_ref`。

所以相似点是 DAG task execution，差异是系统主线。Ray 的主线是分布式计算性能，LogServe 的主线是 shared log 驱动的可恢复 AI runtime。

## Q369. 如果 workflow 需要长时间等待外部事件，当前模型如何扩展？

当前模型适合“依赖完成后继续调度 step”。如果 workflow 要等几小时、几天，甚至等外部系统回调，现在还缺一类明确的 waiting 状态。

可以这样扩展：

1. 增加 step 状态 `WAITING` 或 workflow event `StepWaiting`。
2. 增加事件类型，比如 `ExternalWaitRegistered`，记录等待的 event key、超时时间、回调条件。
3. 控制面提供 `SignalWorkflow` 或 `CompleteExternalEvent` RPC。
4. 外部事件到达时，先 append `ExternalEventReceived` 到 workflow stream。
5. replay 时根据 wait registration 和 external event 恢复等待状态。
6. 条件满足后，把对应 step 重新变成 scheduled，让调度器继续跑下游 step。

重点仍然是 log-first。不能只是把等待信息放在内存或 metadata 里，否则控制面重启后就不知道在等什么。

这类能力做出来以后，LogServe 才更接近真正的 durable workflow。当前实现能表达 DAG task 执行，但还不是完整的 long-running orchestration。

## Q370. 如果 workflow 需要人工审批步骤，事件模型如何设计？

人工审批本质上也是外部事件，只是事件来源是人，而不是自动系统。

我会把审批建模成一种特殊 wait step：

- `ApprovalRequested`：记录审批 step_id、审批人或审批组、表单数据、过期时间。
- `ApprovalGranted`：记录审批人、审批时间、审批意见、request_id。
- `ApprovalRejected`：记录拒绝原因。
- `ApprovalTimedOut`：记录超时。
- `StepSucceeded` 或 `StepFailed`：审批结果最终映射回 step 状态。

审批请求不能只存在 dashboard 里。必须写进 workflow stream，这样重启后能知道哪些审批还没处理。审批结果也必须带 idempotency key，比如 `workflow_id + step_id + approval_request_id`，避免用户重复点击或前端重试造成多次审批。

如果审批会影响下游分支，当前静态 DAG 模型还不够，需要支持审批完成后动态追加 step，或者在提交时把两条分支都展开，再由审批结果决定哪条分支继续。

## Q371. 如果 workflow 支持 compensation/Saga，日志中需要记录什么？

Saga 的关键不是只知道哪个 step 成功了，还要知道失败后应该如何补偿。

日志里至少要记录这些信息：

- `CompensationRegistered`：某个 step 成功后，对应的补偿动作是什么。
- `CompensationScheduled`：workflow 失败后，某个补偿 step 被调度。
- `CompensationStarted`：补偿开始执行。
- `CompensationSucceeded`：补偿成功。
- `CompensationFailed`：补偿失败，需要人工处理或重试。
- `SagaCompleted`：所有需要补偿的动作处理完成。

还要记录补偿顺序。一般 Saga 需要按成功 step 的反序补偿。例如先扣库存、再扣款，如果后续失败，补偿时可能要先退款，再释放库存。

这里有一个现实问题：补偿不等于撤销。发出去的邮件没法真正收回，外部扣款也要依赖支付系统的退款接口。因此平台能做的是可靠调度补偿动作，不能保证业务世界完全回到原点。

LogServe 的 shared log 适合记录 Saga 历史，因为每一步成功、失败、补偿都能被解释。缺的是一层明确的 compensation DSL 和状态机。

## Q372. 如何支持 workflow cancellation？取消已经运行的 step 如何处理？

workflow cancellation 需要同时处理控制面状态和 worker 上正在运行的 task。

事件模型可以增加：

- `WorkflowCancellationRequested`
- `StepCancellationRequested`
- `StepCancelled`
- `WorkflowCancelled`

控制面收到取消请求后，应该先 append cancellation event，然后更新 metadata。之后调度器不再调度新的 ready step。

已经运行的 step 比较麻烦。可以分几种情况：

- 还在 queued：直接从队列里跳过或标记 cancelled。
- 已经 lease 给 worker，但 worker 还没开始：worker 下一次检查取消信号时停止执行。
- 已经在 Python runner 里执行：如果任务支持 cooperative cancellation，就传取消 context；如果不支持，只能等 timeout 或重启 runner。
- 已经执行出外部副作用：取消不能撤销副作用，只能走 compensation。

所以 cancellation 不能只做一个状态字段。它需要 task lease、worker heartbeat、executor context 和 workflow event 一起配合。对用户也要说清楚：取消是 best-effort，不是对已经发生副作用的强回滚。

## Q373. 如何支持 workflow versioning？老 workflow definition 如何继续运行？

这个问题可以分成两层：已经提交的 workflow 怎么继续跑，新提交的 workflow 怎么选择新定义。

对已经提交的 workflow，原则是 definition 必须随 workflow 一起持久化。当前 `WorkflowStarted` 里保存了 definition，所以老 workflow 恢复时不应该去读“当前最新代码”，而应该用当时提交的 definition 继续 replay 和调度。

对新提交的 workflow，可以引入显式的 definition revision：

- workflow name
- definition revision
- code artifact ref
- schema revision
- compatibility flags

客户端提交时要么指定 revision，要么由 registry 解析到默认 revision。控制面把解析结果写入 workflow stream。

老 workflow 继续运行时要注意：

- worker 需要还能执行老 artifact。
- 事件 payload 的 replay 逻辑要兼容旧字段。
- 新 worker 不能假设所有 task spec 都有新字段。
- 如果老定义里某个 task 函数已经删除，需要 artifact store 仍然保存老代码包。

Temporal 里有专门的 workflow 代码演进机制。LogServe 当前还没有完整这套能力，但最基本的保护已经有：workflow definition 是随提交入日志的，不是只存一个名字。

## Q374. 如何支持 workflow pause/resume？

pause/resume 和 cancellation 不一样。pause 是暂时不调度新 step，已经在跑的 step 可以继续完成；resume 后再继续调度 ready step。

可以增加这些事件：

- `WorkflowPaused`
- `WorkflowResumed`

控制面逻辑也要改：

- `scheduleReadySteps` 先检查 workflow 是否 paused。
- paused 状态下，不调度新的 ready step。
- 已经 running 的 step 完成后，状态照常更新。
- 如果所有 running step 都完成了，workflow 保持 paused，不自动继续。
- resume event 写入后，再调用 `scheduleReadySteps`。

metadata view 里要能显示 paused 状态。replay 时根据最后一个 pause/resume 事件恢复当前状态。

这个能力对人工审批、限流、运维暂停都很有用。实现难度不算大，难点在于把 paused 和 failed、completed、cancelled 这些 terminal 状态区分清楚。

## Q375. 如何支持 sub-workflow？

sub-workflow 可以有两种设计。

第一种是把 sub-workflow 当成一个特殊 task。父 workflow 的某个 step 调用 `SubmitWorkflow` 创建子 workflow，然后等子 workflow completed，把子 workflow result 作为该 step 的结果。这种方式改动小，父子之间通过 workflow_id 关联。

第二种是让 workflow definition 原生支持嵌套 DAG。父 workflow stream 里记录 `SubWorkflowStarted`、`SubWorkflowCompleted`、`SubWorkflowFailed`，子 workflow 有自己的 `wf:<child_id>` stream。

我更倾向第二种，因为它更清楚：

- 子 workflow 有独立 stream，可以单独 replay。
- 父 workflow 只保存 child workflow id 和最终结果引用。
- dashboard 可以展开父子关系。
- 子 workflow 可以有自己的 retry、timeout、cancellation。

要注意的是幂等。父 workflow 重试创建子 workflow 时，必须用稳定 key，比如 `parent_workflow_id + step_id + child_definition_hash`，否则控制面重启后可能创建多个子 workflow。

## Q376. 如何支持 fan-out/fan-in 大规模并行 step？

小规模 fan-out 现在就能表达：Python trace 时循环创建很多 step，下游 merge step 依赖它们。

但大规模 fan-out，比如一次展开十万 step，不能简单把所有 step 全塞进一个 definition 和一个内存 state。需要分层设计：

- 支持 map step，把“对一个集合的每个元素运行同一个 task”作为一等语义。
- 不一次性展开所有 child step，而是按窗口分批 materialize。
- fan-in 结果用 result store 或分片聚合，不把所有结果都塞进 workflow event。
- 每个 shard 有自己的进度事件，例如 `MapShardCompleted`。
- dashboard 展示汇总进度，而不是列出十万个节点。

这样 workflow stream 仍然能解释历史，但不会被细粒度事件压垮。

当前 LogServe 的 DAG 更适合几十到几百个 step 的实验和 demo。大规模 fan-out 需要专门的 map/reduce 执行模型。

## Q377. 如何避免大规模 fan-out 造成 queue 爆炸？

核心是不要一次性把所有 child task 都提交到全局队列。

可以用几种手段：

- 分批调度：每次只放 N 个 ready task。
- per-workflow concurrency limit：一个 workflow 最多同时运行 K 个 step。
- per-tenant queue limit：防止一个用户把整个系统打满。
- backpressure：queue depth、executor queue、log append latency 高时暂停 fan-out。
- work stealing：worker 空闲时再拉取更多 shard。
- map shard：一个 task 处理一批元素，而不是一个元素一个 task。

对 fan-in 也要小心。所有子 step 同时完成后，如果 merge step 一次性加载全部结果，可能打爆内存。更好的做法是做树形聚合：先局部合并，再逐层合并。

所以大规模 fan-out 的问题不是 DAG 表达不了，而是调度、队列和结果聚合都要有节流。

## Q378. 如何对 workflow 做 critical path latency 分析？

critical path 是决定 workflow 端到端耗时的最长依赖路径。

LogServe 已经记录了 step started_at、completed_at、latency_ms，也知道 depends_on。分析时可以把 workflow DAG 当成带权有向图：

1. 每个 step 的权重是它的执行延迟，也可以加上 queue wait。
2. 按拓扑顺序计算每个 step 的 earliest finish。
3. 对每个 step，取所有上游的最大 finish time，再加自己的耗时。
4. result step 或 workflow completed 对应的最大路径就是 critical path。

这个分析能回答两个问题：

- workflow 慢主要慢在哪条链路上。
- 增加并行度有没有用。

比如一个 workflow 总耗时 800ms，但 critical path 上只有 embed 和 generate，search 虽然多但不在最长路径上，那么优化 search 并不一定改善端到端 p95。

实现上可以把 critical path 写进 benchmark report，也可以在 dashboard 上把关键路径 step 高亮出来。

## Q379. 如何把 workflow DAG 可视化到 dashboard？

dashboard 可视化需要从 metadata view 或 replay state 拿到这几类数据：

- workflow_id、workflow_name、status
- step_id、task_name、status
- depends_on
- attempts
- worker_id 或 task_id
- started_at、completed_at、latency_ms
- result_ref 是否存在
- error

前端可以把 step 当作节点，把 depends_on 当作边。节点颜色表示状态：

- scheduled：灰色
- started：蓝色
- succeeded：绿色
- failed：红色

节点上可以显示 task name、attempts、latency。点开节点后展示 task_id、input_hash、result_ref 和错误信息。

真正有用的 dashboard 不只是画图，还要能回答问题：

- 哪个 step 卡住了。
- 哪个 step 重试最多。
- 哪条路径是 critical path。
- 哪个结果在 result store 里。
- replay state 和 metadata 是否一致。

所以 DAG 可视化最好和 replay API、task status、result_ref 检查结合，而不是只画一张静态图。

## Q380. 如何实现 deterministic workflow replay？Python 代码中的时间、随机数、网络调用如何处理？

要做到 Temporal 那种 deterministic replay，就不能让 workflow 代码随便调用时间、随机数和网络。

基本规则是：

- workflow 代码可以写控制流，但所有非确定性输入都要变成事件。
- 当前时间不能直接用 `time.time()`，要通过 runtime API，返回值写入 history。
- 随机数不能直接用 `random.random()`，要由 runtime 生成并记录。
- 网络调用不能在 workflow 代码里直接做，要放到 task/activity 里。
- workflow replay 时，SDK 不重新生成时间或随机数，而是从 history 读取旧值。

LogServe 当前没有实现这种 deterministic workflow interpreter。它的 Python workflow 只在提交时 trace 一次，之后 replay 不重跑 Python 代码。

如果未来要做 deterministic replay，需要大改 SDK：

- workflow 函数不能只是 trace 成静态 DAG。
- SDK 要拦截 timer、signal、random、side effect。
- history event 要能驱动 workflow 代码重新执行。
- 代码演进要有兼容机制。

这条路很强，但复杂度也高。LogServe 目前更务实：先把 DAG workflow 的状态恢复、retry、result_ref 和调度链路做好。

## Q381. 如果 task 有副作用，workflow retry 应该由平台控制还是用户声明？

不能一刀切。

对纯计算 task，平台自动 retry 很合适。输入一样，输出应该一样，重复执行成本只是多消耗资源。

对有外部副作用的 task，比如扣款、发邮件、创建工单，平台不应该默认无脑 retry。这里至少需要用户声明：

- 这个 task 是否幂等。
- 幂等键应该传给哪个外部系统。
- 哪些错误可以 retry。
- 哪些错误必须直接失败。
- 是否有 compensation。

比较合理的 API 是：

```python
@task(retries=3, idempotent=True)
def charge(...):
    ...
```

或者更细一点，区分 retry policy 和 side-effect policy。

LogServe 当前能保证结果提交层去重，不能保证外部系统只执行一次。所以有副作用的 task 必须由用户和外部系统一起承担幂等。平台可以提供框架，但不能替业务系统做承诺。

## Q382. 如何实现 step-level caching？与 idempotency 有什么区别？

idempotency 和 caching 很容易混，但它们不是一回事。

idempotency 是防重复提交。比如同一个 `workflow_id + step_id + input_hash` 的成功结果已经写过了，重复 completion 不应该再写第二份。

caching 是复用历史计算。比如另一个 workflow 里也要执行同一个 task，输入完全相同，平台可以直接复用之前的结果，不再执行 worker。

step-level caching 需要一个缓存 key，通常包括：

- task name
- function fingerprint
- resolved args hash
- runtime environment hash
- dependency artifact hash

缓存命中后，控制面可以直接写一个 `StepSucceeded` 事件，result_ref 指向已有对象，或者写 `StepCacheHit` 再转成成功状态。

区别是：

- idempotency 的作用域通常是一次请求或一个 workflow 内的逻辑 step。
- caching 可以跨 workflow、跨请求。
- idempotency 是正确性机制。
- caching 是性能优化。

做 caching 时要特别小心副作用。只有声明为 pure 或 cacheable 的 task 才能复用结果。

## Q383. 如何实现 workflow result memoization？

workflow result memoization 是把整个 workflow 的最终结果缓存起来。用户再次提交同样 definition 和同样输入时，系统直接返回之前的 completed workflow result。

实现需要一个 workflow cache key：

- workflow name
- workflow definition fingerprint
- input args hash
- runtime config
- 可能还包括 tenant id

命中后有两种做法：

- 直接返回旧 workflow_id 和结果。
- 创建一个新的 workflow_id，但写 `WorkflowMemoized` 事件，result_ref 指向旧结果。

我更倾向第二种。因为用户每次提交仍然有自己的 workflow 记录，审计和 dashboard 更清楚，同时结果对象可以复用。

边界还是副作用。如果 workflow 中包含发邮件、写数据库、actor mutation，这种 workflow 不能 memoize。只有纯查询、纯计算、确定性的 RAG mock 流程才适合。

所以 memoization 必须是显式打开的能力，不能默认对所有 workflow 生效。

## Q384. 如果 result_ref 指向 S3，如何处理权限和生命周期？

如果 result_ref 指向 S3，不能把裸 bucket 路径随便暴露给所有用户。

需要考虑几件事：

- 权限：result_ref 应该带 tenant namespace，读取时由 control plane 检查 ACL。
- 临时访问：用户下载结果时可以发短期 signed URL，而不是给永久凭证。
- 加密：对象可以使用服务端加密，敏感数据还要考虑应用层加密。
- 生命周期：completed workflow 保留多久，failed workflow 的中间结果保留多久，孤儿对象多久清理。
- 完整性：对象 metadata 里保存 content hash、大小和创建时间。
- 引用计数：多个 workflow memoization 复用同一个对象时，不能随便删除。

日志里最好不要存 signed URL，因为它会过期。日志里应该存稳定 ref，例如 `s3://bucket/tenant/workflows/...` 或内部 object key。真正访问时再由服务端换成临时 URL。

## Q385. 如何对 workflow 做分布式 tracing？

分布式 tracing 要把一次 workflow 的执行链路串起来。可以用 OpenTelemetry 这一类模型。

建议的 trace 结构是：

- 一个 workflow 是一个 trace。
- 每个 step 是一个 span。
- task queue wait、worker local queue wait、Python execution、result store put/get 可以是子 span。
- LLM 调用可以有 model load、checkpoint fetch、first token、total latency 子 span。
- actor call 可以记录 mailbox wait 和 execution latency。

trace context 要从 SDK 提交开始生成，然后放进 TaskSpec 或 metadata。worker 执行 task 时继续使用同一个 trace id。

日志事件里也可以记录 trace_id 和 span_id。这样 dashboard 能从 workflow DAG 跳到 trace，trace 也能反查 workflow_id 和 step_id。

注意不要把大 payload、prompt 全量、用户隐私直接塞进 span attribute。trace 适合放 id、状态、耗时和有限标签。

## Q386. 如何将 workflow 状态导出给外部监控系统？

可以从两条路径导出。

第一条是 metrics。控制面和 worker 暴露 Prometheus 指标：

- workflow submitted/completed/failed count
- workflow end-to-end latency
- step latency
- retry count
- queue depth
- result store put/get latency
- replay consistency check result

第二条是 event export。把 workflow stream 中的事件投递到外部系统，比如 Kafka、ClickHouse 或日志平台，用来做审计和离线分析。

两者用途不同。metrics 适合看当前系统健康，event export 适合追历史和做报表。

导出时要保持一个原则：外部监控系统不是 source of truth。它可以落后，也可以丢部分统计数据，但不能反过来驱动 workflow 状态。

## Q387. 如何支持 workflow 输入输出的 schema 校验？

schema 校验可以放在三个位置。

第一是 SDK 提交前。Python 可以通过 type hints、Pydantic 或 dataclass 校验输入，把明显错误挡在客户端。

第二是 control plane 接收时。不能完全信任 SDK，所以控制面应该校验 definition JSON、step args、timeout、retry、depends_on 等字段。

第三是 worker 执行结果返回时。task result 要符合声明的 output schema，才能写 `StepSucceeded`。不符合就写失败，错误里说明 schema mismatch。

schema 本身也要持久化。否则 replay 时只知道结果 JSON，不知道当时按什么 schema 验证的。可以在 workflow definition 里保存 input_schema_ref 和 output_schema_ref，或者保存 schema hash。

这样做的好处是 dashboard、SDK 和外部调用方都能知道输入输出结构，不用靠 README 猜。

## Q388. 如何在多租户场景隔离 workflow 资源？

多租户不是加一个 `tenant_id` 字段就完事了。它会影响整个系统。

至少要隔离：

- stream namespace，比如 `tenant:<id>:wf:<workflow_id>`。
- metadata 查询，所有 Get/List API 都带 tenant scope。
- task queue，避免一个租户占满全局队列。
- worker pool，可以共享，也可以按租户独占。
- result store 路径和权限。
- model cache 使用配额。
- rate limit 和 backpressure 阈值。
- dashboard 可见范围。

调度上也要支持公平性。比如每个租户有自己的 queue，高优先级租户有更高权重，但不能饿死其他租户。

安全上要防止 tenant A 猜到 tenant B 的 workflow_id 后读取状态。因此所有 RPC 都要先鉴权，再按 tenant_id 查询，而不是只靠随机 id。

## Q389. 如果 workflow 同时包含 CPU task、GPU LLM、actor call，调度器如何联合优化？

这时调度器不能只看“哪个 worker 空闲”。不同 step 对资源和状态的要求不一样。

CPU task 主要看：

- worker task pool 空闲度
- CPU 和内存
- 本地队列等待时间

GPU LLM step 主要看：

- worker 是否有目标模型缓存
- GPU 显存是否够
- checkpoint 是否已在本地
- 预测首 token 延迟
- 当前 LLM 队列等待时间

actor call 主要看：

- actor 当前 owner worker
- actor mailbox 排队长度
- actor epoch fencing
- 是否需要迁移或恢复 actor

联合优化可以先做约束过滤，再做打分：

1. 过滤不满足硬约束的 worker，比如没有 GPU 或不是 actor owner。
2. 对剩余 worker 计算 predicted latency。
3. 加入 queue penalty、cold start penalty、迁移成本。
4. 对同一 workflow 的关键路径 step 提高优先级。

这样 CPU、LLM、actor 都能用同一个调度框架，但每类 step 有自己的 scoring 插件。LogServe 现在已经有 task、LLM、actor 三条执行路径，后续要做的是把调度决策统一起来，而不是每条路径各写一套孤立策略。

## Q390. 如何证明 workflow retry 没有造成重复最终结果？

证明要靠设计和测试两部分。

设计上有几道防线：

- step 成功事件的幂等键不带 task_id，而是绑定 `workflow_id + step_id + input_hash`。
- retry attempt 的 task dispatch key 带 attempt，避免把有意重试当成重复提交。
- task completion 带 worker_id 和 lease_epoch，旧 worker 的 stale completion 会被拒绝。
- step 已经 succeeded 后，重复 completion 不会覆盖结果。
- workflow completed 事件也有稳定 idempotency key，重复触发不会写多份最终结果。

测试上可以专门构造这些场景：

- worker 完成 step 后，重复发送同一个 CompleteTask。
- step 失败后 retry，同时让旧 attempt 延迟提交成功。
- queue redelivery 后两个 worker 都尝试完成同一个 task。
- result step 成功后重复调用 completeWorkflow。
- 控制面重启后 replay，再继续完成 workflow。

验收指标很直接：

- workflow stream 里 `WorkflowCompleted` 只有一个。
- result step 的 `StepSucceeded` 对同一 input_hash 只有一个有效结果。
- final result_ref 或 inline result 与 metadata 中一致。
- `ReplayWorkflow` 返回的状态和 metadata view 一致。

这里证明的不是“没有重复执行”。重复执行在至少一次模型里可能发生。证明的是重复执行没有变成重复最终结果，这才是 LogServe 当前 exactly-once-ish 的真实边界。
