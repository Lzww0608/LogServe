# 九、Metadata Store、Object Store、PostgreSQL、MinIO/S3 与持久化边界

这一组问题适合用来解释 LogServe 的“状态分层”。面试时不要把 PostgreSQL、MinIO 和 shared log 混成一个东西。LogServe 的主线是：shared log 保存事件事实；metadata store 保存当前可查询视图；object store 保存大结果和 snapshot 这类不适合塞进日志的对象。三者都参与恢复，但承担的责任不一样。

## Q631. `metadata.Store` 接口包含哪些能力？

`metadata.Store` 是 control plane 看到的 metadata view 接口。它不是日志接口，也不是对象存储接口，主要负责保存系统的“当前状态”。

它包含几类能力。

第一类是 task 状态管理。包括 `CreateTask`、`GetTask`、`GetTaskByIdempotencyKey`、`ListTasks`、`LeaseTask`、`ValidateTaskLease`、`RequeueExpiredRunningTasks` 和 `CompleteTask`。这部分支撑普通 task、workflow step task、actor task、LLM task 的排队、租约、完成和 redelivery。

第二类是 model registry。包括 `RegisterModel`、`GetModel` 和 `ListModels`。LLM task 提交前会查 model registry，确认模型存在，并读取 adapter、路径、大小等信息。

第三类是 workflow view。包括 `CreateWorkflow`、`GetWorkflow`、`GetWorkflowByIdempotencyKey`、`ListWorkflows`、`UpdateWorkflow` 和 `UpsertWorkflow`。workflow engine 调度 step、更新 step 状态、写 final result，都通过这组接口维护当前 workflow view。

第四类是 worker view。包括 `UpsertWorker`、`GetWorker`、`ActiveWorkers`、`ListWorkers`、`Heartbeat`、`IncrementWorkerLoad` 和 `DecrementWorkerLoad`。调度器通过这里看到 worker 是否存活、容量多少、当前跑了多少 task、缓存了哪些模型。

第五类是 actor view。包括 `CreateActor`、`GetActor`、`GetActorByIdempotencyKey`、`ListActors`、`UpdateActor` 和 `UpsertActor`。actor ownership、epoch、command count、snapshot ref、state_json 都落在这条 view 上。

一句话概括：`metadata.Store` 保存的是 control plane 运行时需要快速读取和修改的当前视图。真正的事件历史仍然在 shared log。

## Q632. `MemoryStore` 保存哪些 map？

`MemoryStore` 是内存版 metadata view。它内部有一个读写锁和几组 map。

task 相关的是：

- `tasks`：按 `task_id` 保存 task 当前状态。
- `taskByIdemKey`：按 idempotency key 反查 task id。

workflow 相关的是：

- `workflows`：按 `workflow_id` 保存 workflow state。
- `workflowByIdemKey`：按 idempotency key 反查 workflow id。

actor 相关的是：

- `actors`：按 `actor_id` 保存 actor state。
- `actorByIdemKey`：按 idempotency key 反查 actor id。

worker 和 model 相关的是：

- `workers`：按 `worker_id` 保存 worker 的 capacity、running tasks、cached models、heartbeat 时间等。
- `models`：按 `name:version` 保存 model registry 信息。

有一个边界要说清楚：`MemoryStore` 不保存 control plane 的本地队列 `queue`，也不保存 `specs` map 和 LLM EWMA stats map。这些在 control service 里。control 重启后，需要通过 `BootstrapFromLog` 从 shared log 重建 task spec、workflow、actor、model、backpressure、LLM stats 等运行时视图。

## Q633. PostgreSQL metadata 的作用是什么？

PostgreSQL metadata 的作用是把 materialized metadata view 落到数据库表里，方便查询、展示和长期观察。Compose 模式下 control 会使用 `LOGSERVE_METADATA_STORE=postgres`，并通过迁移脚本创建 task、workflow、actor、worker、model、LLM request 等表。

但这里不能说 PostgreSQL 是 LogServe 的 source of truth。当前实现里的 `PostgresStore` 仍然包了一层 `MemoryStore`：运行时读写先落到内存 view，再把变更写入 PostgreSQL 表。PG 表更像是当前 view 的持久副本和 dashboard/query 友好的存储。

系统真正用于解释历史和恢复语义的是 shared log。比如 `TaskSubmitted`、`WorkflowStarted`、`ActorCommandApplied`、`ActorSnapshotCreated` 这些事件都在 log 里。PG 表被清掉后，只要 shared log 和必要的 object store 对象还在，control 重启时可以通过 bootstrap/replay 把 view 重新建出来。

所以面试里可以这样回答：PostgreSQL 提高 metadata view 的可见性和持久性，但不改变 log-first 设计。它保存当前状态，不替代事件日志。

## Q634. Result store 用来存什么？

Result store 用来存“不适合直接放进 shared log 的大对象”。当前主要有两类。

第一类是 workflow step result 和 workflow final result。小结果可以 inline 放进 workflow event payload；如果结果超过 `resultInlineThreshold`，control 会把完整 JSON 写到 result store，只在 `StepSucceeded` 或 `WorkflowCompleted` 事件里保存 `result_ref`。

第二类是 actor snapshot。actor 每执行到一定 command count，会把当前 `state_json` 写成 snapshot。snapshot 内容走 result store，actor stream 里写 `ActorSnapshotCreated`，payload 里放 `snapshot_ref`、`snapshot_command_count` 和必要的 class/init metadata。

这层设计的好处很直接：shared log 保持轻量，只记录事件和引用；大对象交给对象存储管理。replay 时如果需要完整结果，再通过 `result_ref` 读取对象。

## Q635. 为什么大 workflow result 不直接放到 log 里？

主要是为了控制日志体积和 replay 成本。

shared log 的职责是记录事件顺序和状态变化。如果一个 workflow step 返回几 MB 甚至几十 MB 的结果，直接塞进 `StepSucceeded` 事件会带来几个问题。

第一，append 延迟会上升。日志写入本来应该尽量短，结果 payload 太大，会拖慢 control 的 log-first 路径。

第二，replay 会变重。很多时候 replay 只需要知道某个 step 已经成功、结果引用是什么，不一定每次都要把大结果读进内存。

第三，日志保留和结果生命周期会绑死。日志是系统恢复链路的一部分，大结果更适合走对象存储，后续可以独立做生命周期、权限、冷热分层和清理策略。

当前实现的做法是：小结果 inline，大结果写 result store，日志只保存 `result_ref`。这比“一律进日志”更接近真实系统的边界。

## Q636. actor snapshot 为什么通过 result store 存储？

actor snapshot 本质上是某个 command count 时刻的完整状态。这个状态可能很小，比如 Counter 的 `{"value":100}`；也可能很大，比如一个 agent session、缓存索引、会话上下文或中间计划。

如果每次 snapshot 都直接写入 actor stream，actor stream 会被大块状态撑大。后面做 logical trim 或 replay 时，日志读放大会更明显。

所以当前实现把 snapshot 对象写到 result store，再在 actor stream 写一条 `ActorSnapshotCreated` 事件。事件里保存：

- `snapshot_ref`
- `snapshot_command_count`
- `snapshot_every`
- `class_name`
- `class_source`
- `init_args_json`
- `timestamp_ms`

replay actor 时，如果发现 snapshot，就先通过 `snapshot_ref` 读取状态，再应用 snapshot 后面的 tail log。这样既保留了恢复能力，也减少了从头 replay command 的成本。

## Q637. `local://` result ref 是什么？

`local://` 是本地文件系统 result store 返回的对象引用。

本地 adapter 会把对象内容做 SHA-256，文件名类似：

```text
<sha256>.json
```

对象会放在配置的 object store 根目录下，并按 namespace 分目录。比如 actor snapshot 可能长这样：

```text
local://actors/<actor_id>/snapshots/<sha256>.json
```

workflow step result 可能长这样：

```text
local://workflows/<workflow_id>/steps/<step_id>/<sha256>.json
```

这个 ref 不是普通文件路径，而是 LogServe result store 的引用协议。`LocalStore.Get()` 会检查 `local://` 前缀，并做路径清理，避免引用逃出 store 根目录。

实验机单机环境下，`local://` 很实用：不需要 MinIO，也能跑完整 result ref 和 actor snapshot replay 链路。

## Q638. MinIO/S3 adapter 的用途是什么？

MinIO/S3 adapter 是 result store 的部署版实现。它让 workflow 大结果和 actor snapshot 不再依赖 control 本地磁盘，而是写入 S3-compatible 对象存储。

当前 `objectstore.OpenFromEnv()` 会根据 `LOGSERVE_RESULT_STORE` 选择实现：

- 空值或 `local`：使用本地文件系统。
- `minio` 或 `s3`：使用 S3-compatible adapter。

S3 adapter 读取这些环境变量：

- `LOGSERVE_S3_ENDPOINT` 或 `MINIO_ENDPOINT`
- `LOGSERVE_S3_BUCKET`
- `LOGSERVE_S3_REGION`
- `LOGSERVE_S3_ACCESS_KEY` 或 `MINIO_ROOT_USER`
- `LOGSERVE_S3_SECRET_KEY` 或 `MINIO_ROOT_PASSWORD`
- `LOGSERVE_S3_CREATE_BUCKET`

写入成功后返回的 ref 是：

```text
s3://<bucket>/<namespace>/<sha256>.json
```

这层适配的意义是把本地实验和接近生产的对象存储边界统一起来。代码上层只关心 `Put/Get`，不需要知道对象最终落在本地磁盘还是 MinIO。

## Q639. Compose 中 PostgreSQL、NATS、MinIO 分别承担什么？

`deployments/docker-compose.yml` 里启动了 PostgreSQL、NATS、MinIO、logd、control 和 worker。

PostgreSQL 用来保存 materialized metadata view。Compose 里 control 设置了：

```text
LOGSERVE_METADATA_STORE=postgres
LOGSERVE_POSTGRES_DSN=postgres://logserve:logserve@postgres:5432/logserve?sslmode=disable
```

所以 task、workflow、actor、worker、model 等当前状态会被写入 PostgreSQL 表。

MinIO 是 S3-compatible object store。Compose 里 control 设置了：

```text
LOGSERVE_RESULT_STORE=minio
LOGSERVE_S3_ENDPOINT=http://minio:9000
LOGSERVE_S3_BUCKET=logserve-results
```

它承接 workflow 大结果和 actor snapshot。

NATS 在 Compose 文件里以 JetStream 模式启动，但当前核心 task 调度路径不是靠 NATS 实现的。worker 仍然通过 gRPC 主动 `PollTask`；control 内部维护队列和 lease。也就是说，NATS 更像部署环境里预留的消息系统组件，不是当前 LogServe source of truth，也不是当前 task queue 的核心实现。

logd 才是 shared log 服务。PostgreSQL 和 MinIO 都可以辅助持久化，但事件事实仍然在 logd 管理的 shared log 里。

## Q640. 如果 PostgreSQL 表被删除，如何通过 shared log 恢复 view？

恢复思路是重启 control，让它重新连接 logd，然后执行 `BootstrapFromLog`。

启动时，control 会先打开 metadata store。PostgreSQL 模式下会执行 migration，把表重新建出来。接着 control 调 `BootstrapFromLog` 扫描 shared log，把事件重新 materialize 回 metadata view。

大致过程是：

1. 从 system streams 恢复 model registry、worker 注册信息、scheduler/backpressure 配置。
2. 从 `task:<task_id>` stream 恢复 task spec 和 task 状态。
3. 从 `workflow:<workflow_id>` stream 恢复 workflow state、step 状态、result ref。
4. 从 `actor:<actor_id>` stream 恢复 actor state、owner、epoch、command count、snapshot ref。
5. 从 `llm:<task_id>` stream 恢复 LLM 执行历史和 stats。

PG 表被删不等于系统历史丢失。只要 shared log 没丢，metadata view 可以重建。真正比较麻烦的是 object store 里的对象丢失：日志里还保留 `result_ref` 或 `snapshot_ref`，但 replay 读不到对象，会影响大结果解析或 actor snapshot 恢复。

## Q641. object store 对象命名如何组织 namespace？

对象命名由上层传入 namespace，底层用内容哈希生成文件名或 object key。

workflow step result 的 namespace 是：

```text
workflows/<workflow_id>/steps/<step_id>
```

workflow final result 的 namespace 是：

```text
workflows/<workflow_id>/result
```

actor snapshot 的 namespace 是：

```text
actors/<actor_id>/snapshots
```

本地 store 会把 namespace 清理后变成目录；S3 store 会把 namespace 变成 object key 的前缀。文件名使用结果内容的 SHA-256：

```text
<sha256>.json
```

这样命名有两个好处。第一，相同内容天然复用同一个哈希名，不容易重复写出一堆随机对象。第二，按 workflow、step、actor 分层后，排查实验结果时比较容易定位对象属于哪条链路。

## Q642. `resultInlineThreshold` 的作用是什么？

`resultInlineThreshold` 是结果内联阈值。control 在 `materializeResult()` 里判断：

```text
len(resultJSON) > resultInlineThreshold
```

如果结果大小超过阈值，并且 result store 已配置，就把结果写到 object store，返回 `result_ref`；否则直接把结果字节 inline 放进 metadata 和 log event payload。

当前默认值是 4096 字节。也就是说，小结果直接放日志里，读状态和 replay 都很方便；大结果只放引用，避免日志膨胀。

这个阈值是一个工程折中。阈值太小，会让很多普通小结果也走 object store，增加一次 Put/Get 开销。阈值太大，shared log payload 会变重，append 和 replay 都受影响。

## Q643. metadata view 和 object store 数据哪个更难恢复？

一般来说，object store 数据更难恢复。

metadata view 是 current state。它可以从 shared log 重新 materialize。比如 PostgreSQL 表没了，control 重启后可以通过 `TaskSubmitted`、`WorkflowStarted`、`StepSucceeded`、`ActorCommandApplied` 等事件重建 view。

object store 不一样。shared log 只保存 `result_ref` 或 `snapshot_ref`，不会保存完整大对象。如果 object store 里的对象被删了，日志仍然能说明“这里曾经有一个大结果”，但拿不到结果内容。workflow replay 可能能恢复 step 已成功的事实，却无法解析上游大结果；actor replay 如果依赖 snapshot，也可能因为 snapshot 对象缺失而失败。

所以持久化边界可以这样分：

- metadata view 丢失：通常可由 shared log 重建。
- object store 对象丢失：只能靠对象存储自己的备份、复制或保留策略恢复。
- shared log 丢失：系统会失去事件事实，这是最严重的。

## Q644. dashboard 读取的是 metadata view 还是 log？

dashboard 主要读取 metadata view。

`GetDashboardSnapshot` 会从 metadata store 取：

- task 列表
- workflow 列表
- actor 列表
- worker 列表
- model 列表

它还会读取 control 内部的 queue depth、调度策略、backpressure 配置、最近一次 log append 延迟。

有一项和 log 直接相关：compactable log stats。control 会调用 log service 的 `GetStreamStats`，汇总 `compactable_records` 和 `compactable_bytes`，用来展示 logical trim 后理论上可 compact 的日志量。

所以 dashboard 不是每次打开都 replay 全量日志。它展示的是当前 materialized view，加上一些 log service 的统计指标。这样读取快，也符合 dashboard 的定位。

## Q645. 为什么结果引用需要能被 replay 阶段解析？

因为 replay 不只是看状态名，有时还要把下游输入重新拼出来。

workflow 里，某个 step 的参数可能依赖上游 step 的结果。`ResolveArgs()` 遇到 `StepRef` 时，会读取上游 step 的 `ResultJSON`。如果上游结果没有 inline，而是只有 `ResultRef`，它就必须通过 `LoadResult(ref)` 把对象取回来，再把结果填进下游参数。

actor 也一样。`ReplayActor` 如果使用 snapshot，会先读 `ActorSnapshotCreated` 里的 `snapshot_ref`，再通过 result store 加载 snapshot 内容，然后从 snapshot 后面的 tail log 继续 replay。

所以 `result_ref` 不能只是给人看的字符串。它必须能被运行时解析，且对应对象必须还存在。否则系统会出现一种尴尬状态：日志知道发生过什么，但恢复时拿不到必要的数据。当前的 `local://` 和 `s3://` ref 都是为这个目的设计的。

