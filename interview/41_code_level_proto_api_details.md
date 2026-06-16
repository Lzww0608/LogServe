# 十三、代码级追问清单：Proto/API 细节

这一组问题主要围绕 `proto/control.proto`、`proto/log.proto` 和对应的 control/log service 实现。回答时可以直接承认：当前 proto 更偏项目演示和实验闭环，字段有意放得直观；如果要做长期稳定 API，需要补 oneof、分页、streaming、reserved tag 和错误模型。

## Q906. TaskSpec 为什么包含 workflow、actor、LLM 多类字段？是否应该拆成 oneof？

当前 `TaskSpec` 是一个统一任务载体。普通 Python task、workflow step、actor command、LLM request 最后都会进入同一个 control queue，被 worker poll 出去执行，所以 proto 里把这几类任务共用的字段放在一个 message 里。比如普通 task 用 `function_name/function_source/args_json`，workflow step 用 `workflow_id/step_id`，actor command 用 `actor_id/actor_call_id/actor_epoch/actor_state_json`，LLM task 用 `llm_model_name/llm_adapter/llm_max_tokens`。

这么做的好处是调度器和 worker 接口简单。`PollTaskResponse` 只返回一个 `TaskSpec`，worker 根据字段判断进入 task pool、actor pool 还是 LLM pool。control 侧的 queue、lease、redelivery、CompleteTask 也能复用一套任务状态机。

但从 API 设计看，它确实可以拆得更干净。生产化时我会把它改成：

```proto
message TaskSpec {
  string task_id = 1;
  string task_name = 2;
  string idempotency_key = 3;
  int64 timeout_ms = 4;
  uint64 task_lease_epoch = 5;

  oneof payload {
    PythonTaskSpec python = 10;
    WorkflowStepTaskSpec workflow_step = 11;
    ActorCommandTaskSpec actor_command = 12;
    LLMTaskSpec llm = 13;
  }
}
```

`oneof` 的价值是避免非法组合。比如同一个 task 同时带 `actor_id` 和 `llm_model_name`，当前只能靠业务代码约束；拆成 `oneof` 后，proto 层就表达了互斥关系。当前没拆主要是为了降低早期实现成本，先把 runtime 主链路跑通。

## Q907. TaskStatus 和 WorkflowStepStatus 为什么分开？

`TaskStatus` 描述的是 worker 执行层的状态：QUEUED、RUNNING、SUCCEEDED、FAILED。它回答的问题是“这个可执行单元在任务队列里处于什么状态”。

`WorkflowStepStatus` 描述的是 workflow DAG 里的 step 状态：SCHEDULED、STARTED、SUCCEEDED、FAILED。它回答的问题是“这个 DAG 节点在 workflow 语义里走到哪一步”。两者看起来接近，但语义边界不同。

举个例子，workflow step 失败后如果还有 retry 机会，底层 task 可能已经 FAILED，但 step 会重新回到 SCHEDULED，准备发起下一次 attempt。再比如某个 step 已经 SUCCEEDED，后面旧 attempt 的 task completion 迟到，也不能改变 step 的最终结果。

分开之后，任务队列可以专心处理 lease、redelivery、worker 负载；workflow runtime 可以处理 attempts、依赖、final result、step latency。两个状态机相关联，但不强行共用同一个 enum。

## Q908. StartTaskRequest 为什么包含 task_lease_epoch？

`StartTaskRequest` 带 `task_lease_epoch` 是为了确认 worker 正在启动的是当前 lease 下的任务。control 在 `PollTask` 中调用 `LeaseTask`，会递增任务的 lease epoch，并把这个 epoch 写回 `TaskSpec`。worker 后续调用 `StartTask` 时，必须带回同一个 epoch。

这样可以挡住旧 worker 的迟到请求。比如 worker-1 poll 到任务后卡住，任务过期被 redelivery 给 worker-2，lease epoch 从 1 变成 2。worker-1 后来才调用 `StartTask(task_id, worker-1, epoch=1)`，control 里的 `ValidateTaskLease` 会拒绝它。

这个字段对 workflow 也有意义。`StartTask` 里如果发现 task 属于 workflow，会调用 `markWorkflowStepStarted` 写 `StepStarted` 事件。只有当前 lease 能把 step 标记为 started，旧 lease 不能污染 workflow 状态。

## Q909. CompleteTaskRequest 为什么包含 actor_epoch 和 task_lease_epoch？

这两个 epoch 解决的是两层不同的问题。

`task_lease_epoch` 是任务队列层的 fencing token。它防止旧 worker 在任务 redelivery 后继续提交 completion。`CompleteTask` 会调用 `ValidateTaskLease(task_id, worker_id, task_lease_epoch)`，只有当前持有 lease 的 worker 才能完成任务。

`actor_epoch` 是 actor ownership 层的 fencing token。actor 绑定 owner worker，owner 转移时 epoch 会递增。actor task 完成时，`completeActorCall` 会检查 `req.worker_id == state.owner_worker_id` 且 `req.actor_epoch == state.epoch`。旧 owner 即使还在执行，也不能写入新的 actor state。

简单说，`task_lease_epoch` 管“这个 task 现在归谁执行”，`actor_epoch` 管“这个 actor 现在归谁持有”。actor command 同时涉及 task 调度和 actor 状态变更，所以两个 token 都要带。

## Q910. RegisterWorkerRequest 中 capacity 是什么语义？

`capacity` 表示 control plane 眼里这个 worker 可以承载的并发任务额度。worker 注册时把 `capacity` 上报给 control，metadata 里保存为 `Worker.Capacity`。如果 capacity 为 0，`MemoryStore.UpsertWorker` 会把它归一化为 1。

调度时，control 会根据 worker 的 `RunningTasks` 和 `Capacity` 判断是否还有空位。worker 本地也有 `cfg.Capacity`，用于限制 `Run` 主循环里 `inFlight < localCapacity` 的 poll 数量。理想情况下，control capacity 和 worker 本地 capacity 应该一致。

这个字段现在是粗粒度容量，CPU task、LLM task、actor task 没有分开计。项目后面已经有本地 executor pool 的 `task_pool_size`、`llm_pool_size`、`actor_pool_size`，但 proto 的 worker capacity 仍然是总额度。生产化时可以把 capacity 扩展成多维资源，比如 CPU slots、LLM slots、actor slots、GPU memory、model cache disk bytes。

## Q911. HeartbeatRequest 中 cached_models 为什么重复上报？

`cached_models` 在 `RegisterWorkerRequest` 和 `HeartbeatRequest` 里都会出现。注册时上报的是初始缓存状态；心跳里重复上报的是最新缓存状态。

LLM cache 是会变化的。worker 可能冷启动加载了一个新模型，也可能 checkpoint cache 发生 eviction，还可能进程重启后扫描本地 cache 目录得到新的实际状态。如果只在注册时上报一次，control 的 locality scheduler 很快就会拿到过期视图。

当前 `Heartbeat` 不写 shared log，只更新 metadata view 里的 worker cache。这个选择让心跳很轻，但也意味着 worker cache 的每次变化没有完整审计历史。真正需要 replay 出模型缓存历史时，系统依赖 LLM event stream 里的 `ModelLoadStarted/ModelLoaded/LLMCompleted`，以及 control bootstrap 时的 materialized LLM stats。生产化时可以把 cache diff 也写成低频事件，但不适合每个 heartbeat 都写 log。

## Q912. WorkflowStepState 为什么同时有 result_json 和 result_ref？

这是为了同时支持小结果 inline 和大结果外置存储。`WorkflowStepState` 里有 `result_json` 和 `result_ref`，workflow final response 也有同样的组合。control 里的 `materializeResult` 会检查结果大小：结果不超过 `resultInlineThreshold` 时直接放 `result_json`；超过阈值时写入 result store，只在状态和日志里放 `result_ref`。

这样做有两个好处。小结果读起来方便，dashboard 和 SDK 不用再访问对象存储。大结果不会把 shared log 和 metadata view 撑爆，replay 时只需要保存引用。

workflow 参数解析也兼容这两种情况。`workflow.ResolveArgs` 遇到上游 step 只有 `result_ref` 时，会通过 loader 读取 result store。也就是说，`result_ref` 不是展示字段，它是 replay 和后续 step 解析的一部分。

边界也要讲清楚：如果 result store 对象丢失，log 里的 `result_ref` 仍然存在，但结果加载会失败。后续要给对象加 checksum、生命周期策略和一致性检查。

## Q913. ReplayActorResponse 为什么暴露 full_replay_commands 和 snapshot_replay_commands？

这两个字段是为了让 actor snapshot 的效果可观测。`ReplayActorResponse` 里返回 `full_replay_commands` 和 `snapshot_replay_commands`，用户可以看到如果从头 replay 要处理多少命令，如果从 snapshot + tail log replay 要处理多少命令。

这个指标比单纯返回“replay 成功”更有价值。actor snapshot 的目标就是降低恢复成本，`snapshot_replay_commands` 明显小于 `full_replay_commands` 时，说明 snapshot 在恢复路径上真正生效。之前实验里出现过 21 vs 1 的对比，就是靠这个字段写进报告的。

它也能帮助调参。`snapshot_every` 太大，snapshot replay 仍然要读很多 tail command；太小，snapshot 写入频繁，会增加 result store 和 log 负担。暴露这两个数后，可以用实验数据选择更合适的 snapshot 周期。

## Q914. LLMEvent 为什么包含 cache_used_bytes 和 eviction_count？

LLM 调度不能只看 cache hit。`LLMEvent` 里记录 `cache_used_bytes`、`cache_capacity_bytes` 和 `eviction_count`，是为了让调度器和实验报告看到 cache 压力。

`cache_used_bytes/cache_capacity_bytes` 能说明 worker 的本地 cache 是否接近满。一个 worker 虽然当前命中了模型，但如果 cache 已经满，下一次加载新模型可能触发 eviction。`eviction_count` 记录最近一次 checkpoint cache 操作带来的淘汰次数，它可以进入 predicted-latency 的 penalty。

这些字段也方便 replay。`ReplayLLM` 会把 `ModelLoaded` 和 `LLMCompleted` 里的 cache 指标重建到响应里，control 侧的 materialized LLM stats 也可以基于 `LLMCompleted` 更新。这样实验里能写出 cold/warm、cache hit、checkpoint fetch、eviction 的完整链路。

真实 GPU 场景还要补更多字段，比如 prompt tokens、output tokens、prefill latency、decode latency、batch size、GPU memory used、OOM count。当前这些字段先覆盖模型 checkpoint cache 这条主线。

## Q915. DashboardSnapshot 是否应该分页？

应该。当前 `DashboardSnapshot` 一次返回 queue depth、backpressure 配置、tasks、workflows、actors、workers、models、compactable log stats。`GetDashboardSnapshot` 直接从 metadata view 里 `ListTasks/ListWorkflows/ListActors/ListWorkers/ListModels`，再组装成一个大响应。

这个方式适合单机实验和小规模 demo。它的优点是前端或实验脚本一次 RPC 就能拿到完整状态，便于生成 `dashboard_snapshot.json`。

但规模上来以后会有问题。任务几万条、workflow 上千个、actor 上百万个时，一个 snapshot 响应会太大，gRPC message size、control CPU、序列化时间、dashboard 首屏都会受影响。生产化 API 应该拆成多个 query：

- `ListTasks(page_size, page_token, filter)`
- `ListWorkflows(page_size, page_token, status)`
- `ListActors(page_size, page_token, actor_id_prefix)`
- `ListWorkers()`
- `GetSystemSummary()`

`DashboardSnapshot` 可以保留成小规模 summary，但不应该承担全量查询。

## Q916. SetBackpressure 是否应该持久化到 log？当前如何持久化？

应该持久化到 log。backpressure 配置会影响提交和 redelivery 行为，如果 control 重启后丢掉配置，系统行为会突然变化。

当前实现已经走 log-first。`SetBackpressure` 会先读取当前配置，把请求里大于 0 的字段覆盖进去，然后向 `system:backpressure` stream append `BackpressureConfigured` 事件。事件 payload 里有 `queue_high_watermark`、`redelivery_timeout_ms`、`log_append_slow_ms` 和 `timestamp_ms`。append 成功后，才更新 `configMu` 保护下的内存配置。

重启时 `BootstrapFromLog` 会调用 `bootstrapBackpressure`，从 `system:backpressure` 读出历史事件，恢复最后一次配置。这条路径也有测试覆盖：如果 append 失败，SetBackpressure 不应该改内存配置。

当前的小问题是 idempotency key 用了当前时间，语义偏“每次配置都是新事件”。如果要支持客户端重试幂等，可以让请求带 `idempotency_key`，或者把配置内容 hash 放进 key。

## Q917. AppendLogResponse 中 duplicate 字段如何被上层使用？

`duplicate=true` 表示 logstore 收到的 `stream_id + idempotency_key` 以前已经 append 成功过，这次没有写入新 record，而是返回已有 record 的 seq、timestamp 和 crc。

这个字段对上层最有价值的地方是重试安全。比如 control 或 worker 调用 AppendLog 超时后重试，如果第一次其实已经写成功，第二次可以拿到 duplicate 响应。上层就知道这不是新的事件，不应该再假设日志多了一条。

当前很多 control 路径只关心“append 是否成功”，没有大量分支判断 duplicate。原因是幂等 key 已经让 log 层不会重复写事件；metadata 更新也有自己的幂等和终态保护。比如 `TaskSubmitted`、`StepSucceeded`、`ActorCommandApplied` 都使用稳定 idempotency key，重复 append 不会制造第二条最终事件。

如果以后要做更严格的状态机审计，可以把 duplicate 暴露到 observability，或者在上层针对 duplicate 做“跳过重复 materialize”的明确分支。

## Q918. ReadLogResponse 如果 records 很多，是否应该支持 streaming RPC？

应该。当前 `ReadLog` 是 unary RPC，请求里有 `from_seq` 和 `limit`，响应里一次返回 `repeated LogRecord records`。这对分页读取和小规模 replay 足够，`readAllLog` 也是靠 limit=1000 循环读取。

问题在大 stream。actor stream、workflow stream、llm stream 长时间运行后会很长，unary response 太大时会碰到 gRPC message size、内存占用和响应延迟问题。服务端要把一批 records 全部组装好再返回，客户端也要一次性接收。

更合适的接口是 server streaming：

```proto
rpc ReadLogStream(ReadLogRequest) returns (stream LogRecord);
```

或者定义 batch streaming：

```proto
rpc ReadLogBatches(ReadLogRequest) returns (stream ReadLogResponse);
```

streaming 的好处是 replay 可以边读边 apply，内存压力低，也更适合长 stream。当前保留 unary 是为了实现简单，后续要扩展 replay 和 dashboard 时应该补 streaming。

## Q919. LogService 是否应该支持 tail/follow？

应该支持，尤其是 dashboard、调试工具和在线 materializer 会需要它。当前 `LogService` 只有 `AppendLog`、`ReadLog`、`ListStreams`、`TrimStream`、`GetStreamStats`。如果想看某个 workflow 或 actor 的实时事件，只能轮询 `ReadLog(from_seq=last+1)`。

tail/follow 可以做成：

```proto
message TailLogRequest {
  string stream_id = 1;
  uint64 from_seq = 2;
  bool follow = 3;
}

rpc TailLog(TailLogRequest) returns (stream LogRecord);
```

`follow=false` 时读到当前 tail 就结束；`follow=true` 时像 `tail -f` 一样等待新事件。这对 dashboard DAG 实时刷新、actor mailbox 调试、LLM event timeline 都很有用。

实现上要注意背压和断线恢复。客户端断开后服务端要清理 watcher；客户端重连时用最后看到的 seq 继续；如果 stream 被 logical trim，服务端要返回明确错误或把 from_seq 抬到 trim point。

## Q920. proto 字段删除或重命名会有什么兼容性问题？

proto 字段真正的 wire identity 是 field number，不是字段名。比如 `TaskSpec.task_id = 1`，线上消息里编码的是 tag 1。重命名字段但保留 tag，对二进制兼容通常没问题；删除字段或复用 tag 风险很大。

最危险的是复用旧 tag。假设以前 `field 18` 是 `llm_model_name`，后来删除后把 `field 18` 分配给另一个含义，老 worker 发来的消息会被新 control 按新含义解释，状态会乱。正确做法是不用的字段号要 `reserved`，字段名也可以 `reserved`：

```proto
reserved 18;
reserved "llm_model_name";
```

删除字段还有一个问题：老客户端仍然会发送这个字段，新服务端可能忽略它；新客户端依赖新字段，老服务端也会忽略它。这种“静默忽略未知字段”对向前兼容有帮助，但也会让业务语义缺失。比如新客户端以为 `task_lease_epoch` 被校验，老服务端如果没有这个字段，就不能提供 fencing 语义。

所以稳定 API 的规则是：只追加字段，不复用 tag；enum 新值要考虑老客户端看到 unknown value 的行为；语义变更要新增字段或新增 RPC；删除字段前先让所有客户端停止使用，再保留 reserved。LogServe 当前还处于项目阶段，proto 可以调整；如果对外发布，就要把这些兼容规则写进贡献文档和 CI 检查。
