# 三、Control Plane、Event Sourcing 与 Metadata View（简单）

这一组重点讲 control plane。回答时抓住一条主线：control 不是最终事实源，shared log 才是；control 负责接收请求、写事件、维护 metadata view、调度 worker，并在重启后从日志恢复。

## Q166. control service 提供哪些主要 RPC？

LogServe 的 control service 是系统的入口层。它暴露的 RPC 可以按功能分成几组。

任务相关：

```text
SubmitTask
GetTaskStatus
PollTask
StartTask
CompleteTask
```

`SubmitTask` 给 SDK 或命令行提交普通 task。`GetTaskStatus` 查当前任务状态。`PollTask` 是 worker 拉取任务。`StartTask` 表示 worker 已经开始执行。`CompleteTask` 表示 worker 把成功或失败结果交回来。

workflow 相关：

```text
SubmitWorkflow
GetWorkflowStatus
ReplayWorkflow
```

`SubmitWorkflow` 提交 DAG workflow 定义，control 会创建 workflow state 并调度 ready steps。`GetWorkflowStatus` 查当前 materialized view。`ReplayWorkflow` 从 shared log 读 workflow stream，重建一份状态，并和 metadata view 做一致性比较。

actor 相关：

```text
CreateActor
CallActor
GetActorStatus
ReplayActor
```

它们负责 actor 创建、actor 方法调用、状态查询和 replay 校验。

LLM serving 相关：

```text
RegisterModel
SetSchedulingPolicy
SubmitLLM
ReplayLLM
```

这部分负责模型注册、调度策略切换、LLM 请求提交和 LLM event replay。

worker 与系统观测相关：

```text
RegisterWorker
Heartbeat
SetBackpressure
GetDashboardSnapshot
```

`RegisterWorker` 和 `Heartbeat` 维护 worker view。`SetBackpressure` 配置限流参数。`GetDashboardSnapshot` 汇总队列、任务、workflow、actor、worker、model、log compactable bytes 等状态。

## Q167. SubmitTask 的主要流程是什么？

`SubmitTask` 的流程可以按 5 步讲。

第一步，校验请求。`task_name`、`function_name`、`function_source` 都必须存在。参数放在 `args_json` 里，幂等键放在 `idempotency_key`。

第二步，生成 `TaskSpec`。control 会分配一个新的 `task_id`，把函数名、源码、参数和幂等键放进 spec。

第三步，做幂等检查。control 会根据 task spec 算 fingerprint。如果 metadata 里已经有同一个 idempotency key 的 task，就比较 fingerprint。相同则返回旧 task，不重复提交；不同则报 idempotency conflict。

第四步，先写 shared log。control 会把 `TaskSubmitted` 写到 `task:<task_id>` stream。这个事件里保存 task spec。只有 append 成功后，control 才创建 metadata task record。

第五步，更新 materialized view 并入队。metadata 里保存 task 当前状态为 `QUEUED`，control 把 task id 放进本地队列，等待 worker `PollTask` 拉取。

这一条路径体现了 log-first：先有可 replay 的事件，再更新 metadata view。

## Q168. SubmitWorkflow 的主要流程是什么？

`SubmitWorkflow` 负责把一个 workflow 定义变成可调度的运行实例。

第一步，校验和解析 definition。请求里有 `workflow_name`、`definition_json` 和 `idempotency_key`。control 会用 workflow parser 把 JSON 解析成 DAG 定义，检查 step 列表是否为空，必要时把最后一个 step 作为 result step。

第二步，算 workflow fingerprint。相同幂等键再次提交时，如果 definition 没变，就返回已有 workflow；如果 definition 变了，就返回 idempotency conflict。

第三步，创建 workflow state。control 分配 `workflow_id`，把 step 初始化成 `SCHEDULED`，记录 created time、workflow name、result step 等信息。

第四步，先写 `WorkflowStarted` 事件。事件写入 `wf:<workflow_id>` stream，payload 里包含 workflow id、workflow name、definition JSON、幂等信息和时间戳。append 失败时，不更新 metadata。

第五步，写 metadata view，并调度 ready steps。control 把 workflow state 保存到 metadata，然后调用 `scheduleReadySteps`。没有依赖或依赖已满足的 step 会被转成 task，写 `TaskSubmitted`，再写 `StepScheduled`，最后进入队列。

所以 workflow 不是一次性把所有函数都跑完。它先变成一个可恢复的 DAG 状态机，然后 control 按依赖推进。

## Q169. RegisterWorker 和 Heartbeat 分别有什么作用？

`RegisterWorker` 是 worker 首次接入 control plane 时调用的。它告诉 control：我是谁、地址是什么、有哪些 labels、本地缓存了哪些模型、并发 capacity 是多少。

control 收到后会先写 `WorkerRegistered` 到 `system:workers` stream，再把 worker 写入 metadata view。这样 control 重启时可以从 log 里恢复 worker 注册信息。

`Heartbeat` 是 worker 运行期间周期性调用的。它刷新 `LastHeartbeat`，也会上报当前 cached models。control 用这个信息判断 worker 是否还活着，以及 LLM locality-aware scheduler 能不能把模型请求调到有缓存的 worker 上。

两者的区别是：`RegisterWorker` 建立或更新 worker 基本信息；`Heartbeat` 证明 worker 还在线，并持续刷新缓存状态。

当前实现里，heartbeat 主要更新 metadata view，没有每次都写入 log。这个取舍是为了避免心跳把 shared log 打爆。worker 注册是历史事实，心跳是高频租约状态，两者不适合完全一样地记录。

## Q170. PollTask 为什么由 worker 主动拉取，而不是控制面主动推送？

主动拉取更适合这个项目的实验环境，也更容易处理 worker 动态变化。

worker 主动 `PollTask`，control 不需要维护到每个 worker 的长连接，也不需要在 worker 临时离线时做复杂的推送重试。worker 只要活着，就定期来拿任务；拿不到就稍后再问。

拉取还有天然的背压效果。worker 只有空闲时才 poll，control 不会把任务硬塞给已经满载的 worker。配合 worker capacity 和 running tasks，control 可以在 poll 时判断这个 worker 是否还能接任务。

调度逻辑也更集中。control 在 `PollTask` 里检查队列、actor mailbox 顺序、target worker、LLM model cache、worker capacity 等条件。能分配就 lease task，不能分配就返回 `has_task=false`。

推送模型不是不行，但需要连接管理、消息确认、重投、worker 崩溃检测。对 LogServe 当前主线来说，pull 模式更简单，也更容易证明恢复语义。

## Q171. StartTask 和 CompleteTask 为什么分开？

因为“worker 拿到任务”和“任务最终完成”中间可能发生很多事。

`PollTask` 只是把 task lease 给 worker。`StartTask` 表示 worker 已经开始执行。workflow step 需要这个事件来记录 `StepStarted`，这样系统可以计算 step latency，也能在 replay 时知道 step 已经进入 started 状态。

`CompleteTask` 是最终提交结果。它只接受 `SUCCEEDED` 或 `FAILED`，并携带 result、error、actor state、task lease epoch 等信息。control 在这里更新 task terminal 状态，推进 workflow step，更新 LLM stats，或者应用 actor command。

分开后，系统能区分几种情况：

- worker poll 到任务后还没开始就挂了。
- worker 已经开始执行，但迟迟没 complete。
- worker complete 超时后重试，control 要靠 lease epoch 和幂等逻辑避免旧结果覆盖新结果。

如果只有 CompleteTask，系统就看不到 started 时间，也很难做 step latency 和运行中状态观测。

## Q172. metadata store 保存哪些状态？

metadata store 保存的是当前视图，也就是 materialized view。

task 方面，它保存 task id、task name、状态、结果、错误、worker id、workflow id、step id、actor id、LLM model、幂等键、lease epoch、创建和更新时间。

worker 方面，它保存 worker id、地址、labels、cached models、capacity、running tasks、last heartbeat。

workflow 方面，它保存 workflow state，包括 workflow id、workflow name、状态、DAG definition、每个 step 的状态、attempts、task id、结果、错误、latency、最终结果等。

actor 方面，它保存 actor id、class name、owner worker、epoch、command count、snapshot ref、state JSON、幂等信息等。

model 方面，它保存 model name、version、size、path、adapter。

要注意一句：metadata store 不是 source of truth。它是为了查询、调度和 dashboard 快速读取。真正能解释状态变化的是 shared log。

## Q173. memory metadata 和 PostgreSQL metadata 的定位有什么不同？

memory metadata 是开发和单机测试用的当前视图。它实现简单，速度快，适合 `go test`、本地 demo 和无外部依赖的实验。缺点也明显：control 进程退出后，内存 view 会丢，需要从 shared log bootstrap。

PostgreSQL metadata 是 Compose 或更接近部署环境下的当前视图。它把 task、workflow、actor、worker、model 等 view 持久化到 PostgreSQL 表里。control 重启后，查询和 dashboard 可以依赖数据库里的 view，同时仍然可以用 shared log 做重建和校验。

当前 PostgresStore 的实现内部仍维护一个 MemoryStore，再把更新持久化到 PostgreSQL。这让接口复用比较简单，也方便测试。但它仍然遵循同一条主线：PostgreSQL 是 materialized metadata view，不是最终事实源。

一句话总结：memory store 方便开发，PostgreSQL store 方便部署和查询；二者都不应该取代 shared log。

## Q174. BootstrapFromLog 是为了解决什么问题？

`BootstrapFromLog` 解决的是 control 重启或 metadata view 丢失后的恢复问题。

如果 control 挂掉，内存里的队列、task spec、workflow state、actor state、scheduler config、LLM stats 都会消失。只要 shared log 还在，control 启动时就可以调用 `BootstrapFromLog` 重建这些状态。

当前 bootstrap 会恢复多类内容：

- `system:models` 里的模型注册。
- `system:workers` 里的 worker 注册信息。
- `system:scheduler` 里的调度策略。
- `system:backpressure` 里的限流配置。
- `task:` streams 里的普通 task spec 和状态。
- `wf:` streams 里的 workflow state。
- `actor:` streams 里的 actor state。
- `llm:` streams 里的 LLM materialized stats。

它还会把仍需执行的 task 放回队列。比如 running task 在 control 重启后会被恢复成 queued，等待 worker 重新 poll。这样系统不会因为 control 崩溃丢掉未完成工作。

## Q175. DashboardSnapshot 展示哪些状态？

`DashboardSnapshot` 是面向观测的聚合视图。

它包含队列状态：`queue_depth` 和 `queue_high_watermark`。

它包含 backpressure 和调度状态：`redelivery_timeout_ms`、`scheduling_policy`、`last_log_append_ms`、`log_append_slow_ms`。

它包含 task 列表。每个 task 展示 task id、task name、status、worker id、workflow id、step id、actor id、LLM model name/version、created/updated time。

它包含 workflow 列表。每个 workflow 展示 workflow id、name、status 和 step 状态。

它包含 actor、worker、model。actor 用 `GetActorStatusResponse` 的形状展示；worker 展示 capacity、running tasks、cached models、last heartbeat；model 展示 model registry 里的模型信息。

它还展示 shared log retention 指标：`compactable_log_records` 和 `compactable_log_bytes`。这能看到 logical trim 后有多少日志已经可以考虑物理压缩。

## Q176. backpressure 配置包括哪些参数？

backpressure 当前有三个参数。

`queue_high_watermark` 控制队列最大积压。队列长度达到这个阈值后，新 task 会被拒绝，避免 control 内存队列无限增长。

`redelivery_timeout_ms` 控制 running task 多久没完成后可以被重投。control 会扫描 running task，如果更新时间超过这个阈值，就写 `TaskRedelivered` 事件，并把任务放回队列。

`log_append_slow_ms` 控制 log append 慢到什么程度时开始拒绝新 task。control 每次 append log 后记录 `last_log_append_ms`。如果最近一次 log append 耗时超过阈值，新 task 会被 backpressure 拒绝。

配置本身也是 log-first 的。调用 `SetBackpressure` 时，control 先把 `BackpressureConfigured` 写到 `system:backpressure`，再更新内存配置。重启时 bootstrap 会从 log 恢复最后一次配置。

## Q177. last_log_append_ms 的意义是什么？

`last_log_append_ms` 是最近一次 control 调用 logd `AppendLog` 的耗时。

它有两个作用。

第一，用来观测 shared log 是否变慢。LogServe 是 log-first 系统，几乎所有状态推进都要先写 log。如果 append log 变慢，task 提交、workflow 调度、actor command、LLM completion 都可能被拖慢。

第二，用来触发 backpressure。配置了 `log_append_slow_ms` 后，SubmitTask 会检查最近一次 log append 耗时。如果 `last_log_append_ms` 大于等于阈值，就拒绝新的 task，避免在日志层已经吃紧时继续堆积工作。

这个指标比较简单，只看最近一次 append。它适合实验和 dashboard 展示。生产化时我会改成滑动窗口 p95/p99，因为单次值容易抖动。

## Q178. queue_high_watermark 保护什么？

它保护 control 的本地任务队列和整个系统的排队延迟。

如果用户提交任务速度远高于 worker 消费速度，队列会不断增长。没有上限时，control 内存会被 task id、spec、metadata 和调度状态逐渐撑大，系统延迟也会越来越不可控。

`queue_high_watermark` 设置了一个硬阈值。SubmitTask 在真正写 `TaskSubmitted` 前会检查队列长度。如果队列已经达到阈值，就直接返回 backpressure 错误，不让新 task 进入系统。

它保护的是入口压力，不是执行中压力。已经 running 的 task 仍由 worker capacity、running tasks 和 redelivery 逻辑管理。

## Q179. redelivery_timeout_ms 解决什么问题？

它解决 worker 拿到任务后失联的问题。

任务被 `PollTask` 分配给 worker 后，metadata 状态会变成 `RUNNING`，并记录 worker id、lease epoch、updated time。如果 worker 崩溃、进程被 kill、网络断开，它可能永远不会调用 `CompleteTask`。

control 在每次 PollTask 前会调用 `redeliverExpiredTasks`。如果某个 running task 的更新时间超过 `redelivery_timeout_ms`，control 会写 `TaskRedelivered` 事件，然后把 task 重新置为 queued，放回队列。

这让系统具备至少一次执行语义。旧 worker 后面如果又返回结果，control 会用 worker id 和 task lease epoch 做校验，拒绝 stale completion，避免旧结果覆盖新 lease。

所以 redelivery_timeout_ms 是故障恢复和任务租约语义的一部分。

## Q180. 为什么控制面需要知道 worker capacity 和 running tasks？

因为 control 要做调度，不能只知道 worker 是否在线。

`capacity` 表示 worker 声明自己最多能并发跑多少任务。`running_tasks` 表示 control 当前认为这个 worker 已经被分配了多少任务。两者一起决定 worker 是否还有空位。

普通 task 调度时，control 可以避免把任务分配给已经满载的 worker。LLM 调度时更需要这个信息：locality-aware scheduler 不只看模型缓存，还要看 worker 是否有 capacity。一个 worker 缓存了 model-A，但已经跑满了，继续把所有请求塞给它会拉高排队延迟。

running tasks 还用于 dashboard。面试官看图时能直接看到每个 worker 的负载，而不是只能看到 worker alive。

这也是 control plane 的职责之一：它不执行用户代码，但要维护足够的运行时视图，才能做队列、租约、重投、模型 locality 和 backpressure。
