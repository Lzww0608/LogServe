# 一、项目总览与开场问题（深度）

这一组问题更偏系统设计追问。回答时可以少背概念，多讲“代码里怎么做、失败时怎么收口、哪些边界还没生产化”。

## Q021. LogServe 的 source of truth 是什么？为什么不能把 metadata store 当 source of truth？

LogServe 的 source of truth 是 shared log。metadata store 是当前状态视图，也就是 materialized view。

我会这样解释：系统里真正不可丢的是事件。比如 `TaskSubmitted`、`WorkflowStarted`、`StepSucceeded`、`ActorCommandApplied`、`LLMCompleted` 这些事件记录了状态变化的原因和顺序。metadata store 只保存“现在看起来是什么状态”，比如某个 workflow 是 running 还是 completed，某个 actor 当前 owner 是谁，某个 worker 上报了哪些模型缓存。

不能把 metadata store 当 source of truth，主要有三个原因。

第一，metadata store 可能丢。项目里支持 memory store 和 PostgreSQL store，memory store 在 control 进程退出后肯定没了；PostgreSQL 虽然持久，但仍然可能因为迁移、误删、写入失败或局部更新失败导致 view 不完整。shared log 更适合作为恢复依据。

第二，metadata view 不保留完整因果。一个 workflow 现在是 completed，不代表你知道它经历了哪些 step、哪些 attempt、哪个 step 被 retry、哪个 worker 执行过。事件日志能解释状态怎么来的，metadata 只能告诉你当前状态。

第三，很多一致性检查必须靠 replay。代码里 `ReplayWorkflow` 会读 `wf:<workflow_id>`，用 `workflow.Replay()` 重建状态，再和 metadata 里的状态做 `workflow.Consistent()`。actor 也是类似，`ReplayActor` 会从 `actor:<actor_id>` 重建 actor 状态，再和 metadata 比。这个检查反过来说明：metadata 是投影，不是事实本身。

所以我会把 shared log 叫“事实记录”，metadata store 叫“服务当前查询和调度的缓存”。这个缓存很重要，但它不是最终依据。

## Q022. log-first 语义在代码路径上具体体现在哪里？

log-first 的意思不是口号，而是状态变化前先写事件。项目里有几条比较明确的代码路径。

普通任务提交在 `internal/control/service.go` 的 `enqueueTaskWithMetadata()`。它先构造 `TaskSubmitted` payload，调用 `s.appendLog()` 写到 `task:<task_id>`，append 成功后才调用 `s.meta.CreateTask()`，再把 task 放入本地 queue。这样如果 log append 失败，metadata 里不会出现一个只有 view、没有日志的任务。

workflow 提交在 `SubmitWorkflow()`。控制面先写 `WorkflowStarted` 到 `wf:<workflow_id>`，然后才 `CreateWorkflow()`。step 调度时，`scheduleReadySteps()` 会先通过 `enqueueTask()` 写 task 提交日志，再向 workflow stream 写 `StepScheduled`，之后才更新 workflow step 的 `TaskID`、attempt 和 input hash。step start、step success、workflow completed 也都是先写 workflow event，再更新 workflow metadata。

actor 创建在 `CreateActor()`。先写 `ActorCreated`，再 `CreateActor()` 到 metadata。actor command 也类似：`submitActorCommand()` 先写 `ActorCommandSubmitted`，再 enqueue actor task；完成时 `completeActorCall()` 先写 `ActorCommandApplied` 或 `ActorCommandFailed`，再更新 actor state。

模型和调度配置也是同一个模式。`RegisterModel()` 先写 `ModelRegistered` 到 `system:models`，再更新 model registry。`SetSchedulingPolicy()` 先写 `SchedulingPolicyChanged`，再改内存里的 scheduling policy。`SetBackpressure()` 先写 `BackpressureConfigured`，再改控制面配置。

worker 执行路径也有体现。`internal/worker/agent.go` 里，worker 在调用 `StartTask()` 前先向 `task:<task_id>` 写 `TaskStarted`；执行完成后先写 `TaskCompleted` 或 `TaskFailed`，再调用 `CompleteTask()`。这让 task stream 能解释 worker 本地执行发生过什么。

我会补一句边界：当前实现还有一些运行时 metadata，比如 task lease、worker heartbeat、running task count，本身更像调度用的临时状态，不全都当成 durable event 来管理。真正要求可恢复的 workflow、actor、model、scheduler、task submission 和 LLM stats 路径，才是 log-first 的重点。

## Q023. 当 log append 成功但 metadata 更新失败时，系统应该如何恢复？

这种情况在 log-first 系统里是可恢复的，因为事件已经写进了 shared log。

比如 `WorkflowStarted` 写成功，但 `CreateWorkflow()` 更新 metadata 失败。短时间内客户端可能收到错误，或者 control 当前内存里看不到这个 workflow。但从恢复角度看，这不是永久丢失。control 重启后会跑 `BootstrapFromLog()`，其中 `bootstrapWorkflows()` 会列出 `wf:` 前缀的 stream，读取 workflow 事件，用 `workflow.Replay()` 重建 workflow state，再 `UpsertWorkflow()` 回 metadata。

actor 也是一样。`ActorCommandApplied` 写进 actor stream 后，如果 metadata update 失败，重启后 `bootstrapActors()` 会读取 `actor:` stream，用 actor replay 得到 command count、state JSON、snapshot ref、owner epoch 等状态。

LLM stats 也类似。`LLMCompleted` 事件已经在 `llm:<task_id>` stream 中，control 重启后 `bootstrapLLMStats()` 会扫描 `llm:` stream，重新 materialize EWMA stats。

所以这类失败的恢复策略是：不要试图相信半失败的 metadata；让控制面从 log bootstrap。metadata 是派生状态，丢了可以重新投影。

这也解释了为什么 log-first 要求 append 放在前面。只要 append 成功，后续 metadata update 失败只是 materialization 延迟或局部失败，而不是状态丢失。

## Q024. 当 metadata 更新成功但 log append 失败时，系统会有什么问题？你的实现如何避免？

这个情况比 Q023 严重得多。metadata 已经显示状态变化，但 shared log 没有对应事件。重启后 replay 会丢掉这次状态变化，metadata 和 log 的事实记录就分叉了。

举个例子，如果 `CreateWorkflow()` 先写 metadata，之后 `WorkflowStarted` append 失败，那么当前 control 可能能查到 workflow，但重启后从 log 里 replay 不出来。更糟的是，worker 可能已经执行了这个 workflow 的 step，而日志里没有完整起点。后续恢复、去重、dashboard 对比都会乱。

当前实现通过代码顺序避免这个问题。`log_first_test.go` 里专门测了几条路径：

- `SubmitWorkflowAppendFailureDoesNotCreateMetadataOnlyWorkflow`
- `CreateActorAppendFailureDoesNotCreateMetadataOnlyActor`
- `RegisterModelAppendFailureDoesNotUpdateRegistry`
- `SetSchedulingPolicyAppendFailureDoesNotChangePolicy`
- `RegisterWorkerAppendFailureDoesNotUpdateMetadata`
- `SetBackpressureAppendFailureDoesNotChangeConfig`
- `RedeliveryAppendFailureDoesNotRequeueTask`

这些测试的含义很直接：如果 append 失败，对应的 metadata 或运行时状态不能被更新。比如 logd 挂了，创建 actor 就应该失败，而不是创建一个 metadata-only actor。

我会承认一个边界：当前实现不是分布式事务，没有把 log append 和 metadata update 做成原子提交。它采用的是“append 成功后 materialize，append 失败就不改 view”的策略。append 成功但 view 更新失败靠 replay 修复；append 失败但 view 成功这种路径要通过代码顺序和测试挡住。

## Q025. 控制面重启时，哪些状态可以从 log 重建？哪些状态不能完全重建？

可以重建的主要是这些：

- models：`bootstrapModels()` 从 `system:models` 读取 `ModelRegistered`。
- workers 的注册信息：`bootstrapWorkers()` 从 `system:workers` 读取 `WorkerRegistered`。
- scheduler policy：`bootstrapScheduler()` 从 `system:scheduler` 读取最后一次 `SchedulingPolicyChanged`。
- backpressure config：`bootstrapBackpressure()` 从 `system:backpressure` 读取配置。
- plain task spec 和状态：`bootstrapTasks()` 读取 `task:` stream，通过 `TaskSubmitted`、`TaskStarted`、`TaskRedelivered`、`TaskCompleted`、`TaskFailed` 还原。
- workflow 状态：`bootstrapWorkflows()` 读取 `wf:` stream，用 `workflow.Replay()` 重建，并恢复还没结束的 step task。
- actor 状态：`bootstrapActors()` 读取 `actor:` stream，用 actor replay 从 snapshot 加 tail log 恢复。
- LLM stats：`bootstrapLLMStats()` 扫描 `llm:` stream，从 `LLMCompleted` 事件重建 EWMA stats。

不能完全重建的也要讲清楚。

第一，worker 的实时心跳不能从旧日志准确重建。worker 注册事件可以重建，但“这个 worker 此刻还活着”必须靠新的 heartbeat。重启后旧的 heartbeat 时间只能作为历史，不应该被当成强实时状态。

第二，worker 本地正在执行到哪一行不能重建。任务可以被恢复成 queued 或 running 后再 redeliver，但 Python 函数内部跑到哪一步、外部 side effect 是否已经发生，日志不一定知道。

第三，worker-local model cache 的真实文件状态不能只靠 log 保证。注册和 heartbeat 会带 cached models，checkpoint cache 也有本地文件和 manifest，但重启后最好让 worker 重新扫描本地 cache 并上报。

第四，result store 或 snapshot object 本身不能从 log 变出来。log 里只有 `result_ref` 或 `snapshot_ref`。如果本地 object store 或 MinIO 对象丢了，replay 能知道应该加载哪个 ref，但加载不到内容。

所以我会说：控制面可重建的是系统语义状态，不是所有机器上的临时执行状态。

## Q026. 为什么 task、workflow、actor、LLM 使用不同的 stream 命名？

不同 stream 是为了隔离语义，也为了让 replay 更简单。

现在项目里大概是这样命名：

```text
task:<task_id>
wf:<workflow_id>
actor:<actor_id>
llm:<task_id>
system:models
system:workers
system:scheduler
system:backpressure
```

task stream 记录单个 task 的生命周期。它关心 submitted、started、completed、failed、redelivered 这些事件。

workflow stream 记录 workflow 级别的状态转移。一个 workflow 里面会有多个 step，对应多个 task。把 workflow event 放在 `wf:<workflow_id>` 里，replay 时只要读一个 workflow stream，就能知道 DAG 走到哪了。

actor stream 记录某个 actor 的命令历史和状态变化。actor 的关键是顺序，所以 `actor:<actor_id>` 里保留 `ActorCommandSubmitted`、`ActorCommandApplied`、`ActorSnapshotCreated` 这种有序事件。这样 replay 一个 actor 不需要扫描全局日志。

LLM stream 用 `llm:<task_id>`，是因为一次 LLM 请求有自己的 model load、model loaded、completed 事件。它和 task stream 有关联，但语义不同：task stream 关心执行生命周期，LLM stream 关心模型加载、cache hit、first token、total latency。

system stream 用来放全局配置或注册信息，比如模型、worker、调度策略、backpressure。这样 control bootstrap 时可以按前缀或固定 stream 分别读取。

如果所有事件都塞进一个全局 stream，当然也能做，但 replay 会更重，隔离性也差。现在这种命名让每个 reducer 只读自己关心的 stream。

## Q027. 如何定义系统中的“事件”？事件 schema 如何演进？

在 LogServe 里，事件就是一次已经发生的状态变化。它至少包含这些信息：

```text
stream_id
seq
event_type
payload
timestamp_ms
idempotency_key
```

`stream_id` 决定事件属于哪条状态线；`seq` 决定同一 stream 内的顺序；`event_type` 决定 reducer 怎么解释 payload；payload 是 JSON 或 protobuf JSON 形式的业务字段。

比如 workflow 的 `StepSucceeded` payload 里有 `workflow_id`、`step_id`、`task_id`、`result_json` 或 `result_ref`、`latency_ms`。actor 的 `ActorCommandApplied` payload 有 `actor_id`、`call_id`、`command_seq`、`state_json`、`worker_id`、`epoch`。LLM 的 `LLMCompleted` payload 有模型名、worker、cache hit、checkpoint fetch、first token、total latency。

schema 演进我会按事件系统的常规规则处理。

第一，只做向后兼容的新增字段。旧 reducer 看不懂新字段也没关系，JSON unmarshal 会忽略未知字段。

第二，新字段要有默认值语义。比如 `timestamp_ms` 缺失时，代码会用 log record 自带的 timestamp。actor replay 里也会对缺失字段做兼容，比如 `CommandSeq` 缺失时用 `CommandCount`。

第三，不随便改老字段含义。字段名可以保守一点，一旦 event 写进 log，就要假设它会长期存在。

第四，如果以后真的要做破坏性变更，应该引入明确的 event schema 标记，或者新增 event type，而不是在同一个 event type 里悄悄改语义。当前项目主要靠 optional fields 和 event type 分流，还没做到完整的 schema registry。

面试时我会补一句：现在这个实现够支撑实验，但生产化需要更严格的 schema 演进策略，包括兼容性测试和老日志 replay 测试。

## Q028. materialized view 与事件日志发生不一致时，你如何检测？如何修复？

检测方式有两类。

第一类是在线 replay API。workflow 有 `ReplayWorkflow`，actor 有 `ReplayActor`。它们都会从 shared log 读事件，重建状态，再和 metadata 里的当前状态做一致性比较。代码里 workflow 用 `workflow.Consistent()`，actor 用 `actor.Consistent()`。测试里也有 replay 状态和 metadata 状态一致的检查。

第二类是重启 bootstrap。control 重启时跑 `BootstrapFromLog()`，会从 log 重建 metadata view。如果重启前 view 有问题，重启后通常会被日志投影覆盖掉。对于 memory metadata store，这其实就是正常恢复路径。

修复方式要看不一致方向。

如果 log 有事件，metadata 没跟上，修复比较简单：重新 replay 对应 stream，或者重启 control 让 bootstrap 重建 view。

如果 metadata 有状态，log 没事件，这就麻烦。因为 source of truth 里没有这次变化。正确修复不是相信 metadata，而是把这类路径当成 bug，阻止它再次发生。项目里 `log_first_test.go` 就是在防这个问题。

如果 log 事件本身坏了，比如 payload 不可解析，当前 reducer 会返回错误或跳过部分 stream。这是更底层的数据损坏问题，生产系统应该有 dead-letter、人工修复工具或日志校验工具。当前项目还没有做这么完整。

## Q029. LogServe 的一致性边界在哪里？客户端提交成功代表什么？

客户端提交成功，代表控制面已经接受请求，并且关键提交事件已经写入 shared log，同时当前控制面 view 也已经创建出对应记录。

以普通 task 为例，`SubmitTask` 成功返回时，`TaskSubmitted` 已经写入 `task:<task_id>`，metadata 里也有 queued task，任务已经放入 control 的队列。它不代表 worker 已经执行，也不代表最终结果一定成功。

workflow 提交成功，代表 `WorkflowStarted` 写入了 workflow stream，metadata 里有 workflow state，并且控制面已经尝试调度 ready step。它不代表整个 workflow 完成。

actor 创建成功，代表 `ActorCreated` 已写入 actor stream，metadata 中有 actor。是否已经有 owner worker，要看当时有没有可用 worker。代码里如果没有 active worker，actor creation 仍然可以成功，后续 call 时会等待 owner。

LLM 提交成功，代表 LLM task 已经进入任务系统。模型加载是否命中 cache、是否 cold start、是否完成，要看 worker 执行后的 LLM events。

一致性边界可以这么说：控制面对“状态记录和调度决策”负责，worker 对“至少执行一次”负责，用户函数的外部 side effect 不在严格事务边界内。LogServe 保证的是日志和 materialized view 的可恢复关系，不是把任意 Python 函数变成分布式事务。

## Q030. 系统中哪些地方使用了幂等键？幂等键冲突如何处理？

幂等键分几层。

第一层是客户端提交级别。`SubmitTask`、`SubmitWorkflow`、`CreateActor`、`SubmitLLM` 都支持 `idempotency_key`。Python SDK 默认不自动生成幂等键，只有用户显式传入时才使用。这样可以避免“同样函数参数被误认为同一次业务请求”的问题。

第二层是 log append 级别。append log request 里也有 `IdempotencyKey`，比如 `task_id:submitted`、`workflow_id:started`、`actor_id:call_id:applied`。这用于避免日志层重复 append 同一个事件。

第三层是 workflow step 去重。workflow step 的 task idempotency key 使用类似：

```text
workflow_id:step_id:input_hash:attempt:n
```

step 成功事件的 key 使用：

```text
workflow_id:step_id:input_hash:succeeded
```

这让同一个 step、同一份输入的成功结果只会提交一次。失败 attempt 可以递增 attempt number 继续重试。

第四层是 actor command 应用。actor applied event 使用：

```text
actor_id:actor_call_id:applied
```

actor command 本身还有 `command_seq`，用于保证顺序。

冲突处理靠 fingerprint。`idempotency.go` 会对 task spec、workflow definition、actor create request 计算稳定 fingerprint。如果同一个 idempotency key 对应的 payload 一样，就返回已有对象；如果 payload 不一样，就返回 `idempotency conflict`。测试里覆盖了 task、workflow、actor、LLM 的冲突路径。

这个设计比“看到 key 就直接返回旧结果”安全。否则用户复用 key 但改了参数，系统会悄悄返回第一次结果，很难排查。

## Q031. 为什么你说 workflow/actor 是 exactly-once-ish，而不是 exactly-once？

因为 worker 执行本身不是严格 exactly-once。

worker 可能已经执行了函数，但在写 `TaskCompleted` 前挂了；也可能写了 `TaskCompleted`，调用 `CompleteTask` 时超时；也可能 control 已经完成状态更新，但 worker 没收到响应。消息重投或任务 redelivery 后，函数可能再次执行。

LogServe 能控制的是最终结果提交和状态应用。workflow 里，同一 `workflow_id + step_id + input_hash` 的成功结果不会重复写成多个最终 step 结果。actor 里，`ActorCommandApplied` 需要匹配当前 `command_seq == command_count + 1`，还要通过 owner worker 和 epoch 检查。旧 worker 或乱序 completion 会被拒绝。

所以准确说法是：

- execution：at-least-once
- state commit：idempotent / ordered
- external side effects：需要用户函数自己处理幂等

严格 exactly-once 要求执行、提交、外部副作用都只发生一次。这个系统没有、也不应该宣称这个能力。把语义说成 exactly-once-ish，更符合真实边界。

## Q032. worker 至少执行一次会带来什么副作用？你怎么把副作用限制在结果提交层？

至少执行一次的风险是用户函数可能重复做外部副作用。

比如一个 task 调用了外部 API、写了数据库、扣了余额、发了消息。如果 worker 执行完后在 CompleteTask 前挂掉，control 可能把任务重新投递。第二次执行时，外部 API 可能又被调用一次。LogServe 没办法自动撤销第一次外部副作用。

项目把副作用限制在“系统内部状态提交层”，主要靠几件事：

workflow step 成功结果用 `workflow_id + step_id + input_hash` 去重。重复 completion 不会导致 workflow 最终结果重复写。

actor command 用 `command_seq` 串行应用。重复或乱序 actor completion 不能随便推进 actor state。旧 worker 还要过 `owner_worker_id + epoch` 检查。

task lease epoch 用来拒绝 stale completion。任务 redelivery 后，旧 lease 的 completion 不应该覆盖新 lease。

log append 本身有 idempotency key。重复写同一事件可以被日志层处理。

但是用户函数里的外部副作用，还是需要业务层幂等。比如调用支付接口要带业务 idempotency key，写数据库要用唯一键或事务，发消息要有去重表。LogServe 能保证自己的状态不乱，不能保证任意外部系统也 exactly-once。

## Q033. 项目如何处理控制面、worker、logd 之间的失败组合？

可以按组件看。

worker 挂掉时，control 依赖 heartbeat 和 redelivery。running task 超过 redelivery timeout 后，`redeliverExpiredTasks()` 会先写 `TaskRedelivered`，再把任务重新放回 queue。workflow 已经成功的 step 不会重跑；未完成或 retryable 的 step 会继续调度。actor owner 失联后，`ensureActorOwner()` 会选一个 active worker，写 `ActorOwnershipGranted`，epoch 增加，旧 worker 的 completion 会被拒绝。

control 挂掉时，worker 可能还在执行。worker 写 task/LLM event 时需要 logd；完成后还要调用 control 的 `CompleteTask`。如果 control 不可用，CompleteTask 会失败或超时。control 重启后通过 `BootstrapFromLog()` 重建状态。已经写到 log 的 `TaskCompleted` 可以被 replay 成 terminal 状态；没有完成事件的 running task 会回到可重投递路径。

logd 挂掉时，control 和 worker 都应该停止推进 durable 状态。当前实现里很多路径 append 失败就返回错误，不更新 metadata。worker 在写 `TaskStarted` 或 `TaskCompleted` 失败后，也不会继续调用 `StartTask` 或 `CompleteTask`。这是保守策略：宁愿失败，也不要制造没有日志的状态。

三个组件同时有故障时，系统的原则是：只相信 log 里已经成功 append 的事件；没有 append 的操作当成没发生；in-flight 执行可能重跑。

## Q034. 如果 logd 挂掉，控制面应该 fail fast、重试，还是降级？

我倾向于 fail fast，最多在客户端或调用层做有限重试，不应该降级成“只写 metadata”。

原因很简单：LogServe 的 source of truth 是 shared log。如果 logd 不可用，控制面继续接受创建 workflow、actor、model 注册这些请求，就会产生 metadata-only 状态。这个状态在 control 重启后 replay 不出来，系统会自相矛盾。

当前实现也是这个方向。`appendLog()` 失败后，上层路径直接返回错误。`log_first_test.go` 里也验证了 append failure 不应该更新 workflow、actor、model、worker、scheduler、backpressure 等 metadata。

可以加重试，但重试必须发生在状态更新之前。比如 append log 超时，可以短暂 retry；retry 仍失败，就返回错误。不能先改 metadata 再补日志。

降级只有一种我能接受：只开放只读查询，或者返回“runtime unavailable”。写路径不应该降级。

## Q035. 如果控制面挂掉但 worker 还在执行任务，会发生什么？

worker 本地执行可能继续跑完，但它最终要向 control 调 `CompleteTask`。如果 control 挂了，CompleteTask 会失败或超时。

这时分几种情况。

如果 worker 在 control 挂掉前已经向 task stream 写了 `TaskStarted`，日志里能看到它开始执行了。执行结束后，如果 worker 还能写 `TaskCompleted` 到 logd，但 CompleteTask 打不到 control，那么日志里会有 terminal event，metadata 还没更新。control 重启后 `bootstrapTasks()` 可以从 task stream 还原 task 状态。

如果 worker 没写 `TaskCompleted`，control 重启后只能知道这个 task 曾经 started。`replayTaskSpec()` 对 running task 会保守地恢复成 queued，后续可能重投。也就是说用户函数可能再执行一次。

对于 workflow，控制面重启后会 `bootstrapWorkflows()`。已写入 workflow stream 的 `StepSucceeded` 不会丢；只在 task stream 有完成但 workflow stream 还没更新的情况，当前实现还需要依赖 control 后续补齐或重跑路径。这也是生产化时需要继续打磨的地方。

面试里我会强调：control 挂掉不应该导致已写日志的事实丢失，但 in-flight worker 执行不保证 exactly-once。

## Q036. 如果 worker 执行完成但 CompleteTask 超时，客户端看到什么状态？

这取决于超时发生在哪一步。

worker 执行完成后，会先写 `TaskCompleted` 或 `TaskFailed` 到 `task:<task_id>`，然后调用 control 的 `CompleteTask()`。如果 `CompleteTask` 超时，worker 这次执行会返回错误，客户端如果正在同步等待，可能看到超时或任务仍然处于 running/queued 状态。

但日志里可能已经有 `TaskCompleted`。如果 control 后来重启或 bootstrap，plain task 可以通过 `bootstrapTasks()` 读 task stream，把状态恢复成 succeeded 或 failed。workflow/actor 还要看对应 workflow stream 或 actor stream 是否已经写了语义事件。比如 workflow 的 `StepSucceeded` 是 control 在 `completeWorkflowStep()` 里写的，如果 CompleteTask 根本没有进入 control，那 workflow stream 可能还没有 `StepSucceeded`，后续可能需要 redelivery 重新执行。

所以我会这么回答：客户端看到的是控制面当前 view，不一定立刻等于 worker 本地已经发生的事实。LogServe 用日志和 replay 收敛状态，但 CompleteTask 超时这类边界仍然可能导致至少一次执行。

对面试官可以补一句：如果要进一步强化，这里可以把 worker 的 terminal task event 和 control 的 workflow/actor semantic event 做更紧密的恢复桥接，让 control 在 bootstrap 时根据 terminal task event 补 workflow step 状态。

## Q037. 这个系统的状态机有哪些？分别如何保证单调推进？

主要有四类状态机。

task 状态机：

```text
QUEUED -> RUNNING -> SUCCEEDED | FAILED
```

redelivery 会把超时的 RUNNING 任务重新放回 QUEUED，但这是带 lease epoch 的。旧 lease 的 completion 会被拒绝。terminal 状态是吸收态，`CompleteTask()` 看到已经 terminal 会直接返回已有状态。

workflow 状态机：

```text
RUNNING -> COMPLETED | FAILED
```

step 状态机是：

```text
SCHEDULED -> STARTED -> SUCCEEDED | FAILED
```

失败后如果还没超过 `max_attempts`，step 会回到 SCHEDULED 并清空 task id，等待下一次 attempt。成功 step 不会被重复 completion 覆盖。workflow 只有所有 step 都 succeeded 才 completed。

actor 状态机更像 command log：

```text
ActorCreated
ActorOwnershipGranted
ActorCommandSubmitted
ActorCommandApplied / ActorCommandFailed
ActorSnapshotCreated
```

它的单调性靠 `command_seq` 和 `command_count`。只有 `command_seq == command_count + 1` 的 completion 可以应用。ownership 用 epoch 单调递增，旧 epoch 不能写入。

LLM 请求本质上是 task 加模型事件：

```text
ModelLoadStarted -> ModelLoaded -> LLMCompleted
```

LLM stats 只从 `LLMCompleted` materialize。request count 递增，EWMA 用新样本更新，不需要反向修改历史事件。

这几个状态机的共同点是：日志事件按 stream 顺序追加，metadata 只按 reducer 规则向前投影。

## Q038. 在你当前实现中，哪些路径是强一致，哪些是最终一致？

强一致要限定在单个控制面进程内、单次 RPC 路径里说。

比如 `SubmitTask` 返回成功时，`TaskSubmitted` 已写入 log，metadata 里也创建了 task，并且 task 被放进队列。`SubmitWorkflow` 返回成功时，`WorkflowStarted` 已写入，workflow view 已创建。`CreateActor` 返回成功时，`ActorCreated` 已写入，actor view 已创建。这个范围内可以说是同步一致。

actor command 应用也比较强：completion 时会检查 owner、epoch、command sequence，通过后先写 `ActorCommandApplied`，再更新 actor state。这里同一 actor 内部顺序是强约束的。

最终一致的地方更多。

metadata view 和 log 之间整体是最终一致。append 成功但 metadata update 失败，可以通过 replay 修复。

LLM predicted-latency stats 是最终一致。它由 `LLMCompleted` 事件 materialize，control 重启后也要重新扫描 `llm:` stream。

worker liveness 和 model cache 是最终一致。worker heartbeat 更新当前状态，但不是 durable log 的严格事实。worker 本地 cache 也要靠 worker 上报。

dashboard 是 view，不是事实源。它展示当前 materialized 状态，可能落后于日志。

跨组件的外部副作用不在强一致范围里。用户 Python 函数如果写外部数据库，需要自己做业务幂等。

## Q039. 你如何避免“功能堆砌”，让项目有一条清晰主线？

我给这个项目定的主线是 log-first runtime，而不是“我做了很多功能”。

所有模块都围绕这条线展开：

- task 是最小执行单元，证明 SDK、control、worker、logd 能跑通。
- workflow 是 task 的 DAG 化，证明多步骤状态可以 replay，失败后不从头跑。
- actor 是有状态对象，证明同一对象的命令顺序和状态恢复可以靠日志解释。
- LLM serving 是 AI runtime 的调度场景，证明模型缓存、冷启动和 LLM event log 可以进入同一个 runtime。
- dashboard、benchmark、fault injection 不是独立功能，而是用来验证前面这些语义。

我会避免把项目讲成“支持很多东西”。更好的讲法是：shared log 是状态源，control 是 materializer 和 scheduler，worker 是执行器；workflow、actor、LLM 都是这个模型上的不同 workload。

如果面试官觉得功能多，我会画出一条链路：

```text
append event -> update view -> schedule work -> execute -> append completion -> replay check
```

只要围绕这条链路讲，项目就不会散。

## Q040. 你会如何画一张架构图解释请求从 SDK 到 worker 完成的全过程？

我会画成四层：SDK、control、logd、worker。

可以这样画：

```text
Python SDK
  |
  | SubmitTask / SubmitWorkflow / CallActor / SubmitLLM
  v
Control Plane
  | 1. append event
  v
Shared Log (logd)
  ^
  | 2. append ok
  |
Control Plane
  | 3. update metadata view, enqueue ready work
  v
Worker Poll Loop
  | 4. PollTask
  v
Worker Local Executor Pool
  | 5. Python executor / actor method / mock-vLLM adapter
  v
Worker writes TaskStarted / TaskCompleted / LLM events
  |
  v
Control CompleteTask
  |
  v
Control appends workflow/actor semantic events, updates view
```

如果是 workflow，我会在 control 旁边画一个 DAG scheduler：只有依赖完成的 step 才进入 queue。step 完成后 control 根据 workflow state 判断是否调度后续 step。

如果是 actor，我会在 queue 前画 mailbox：同一个 `actor_id` 只有下一个 `command_seq` 能被 worker poll 到。

如果是 LLM，我会在 worker 旁边画 model cache：worker heartbeat 上报 cached models，scheduler 根据 cache hit、queue delay、EWMA latency 选 worker。

这张图不需要复杂。面试里图越清楚越好：谁写日志、谁更新 view、谁执行任务、失败后从哪里恢复。

## Q041. 如果面试官质疑“AI workflow runtime”这个定义，你如何证明 LLM scheduling 与 workflow/actor 不是孤立 demo？

我会用代码路径和 demo 说明它们不是孤立的。

LLM 请求本身走的是同一套 task runtime。`SubmitLLM()` 最后调用 `enqueueTask()`，生成的是一个带 `LlmModelName`、`LlmModelVersion`、adapter 和 max tokens 的 task spec。worker poll 到以后，`runExecutor()` 根据 `task.GetLlmModelName()` 分支到 `runLLMExecutor()`。

workflow 也能调用 LLM。RAG workflow 里 `llm_generate()` 是一个 workflow step，控制面照样把它当 step 调度，只是这个 step 带模型元数据。也就是说 LLM serving 不是旁路 API，而是 workflow DAG 里的可调度 step。

actor 和 LLM 共享 control、worker、queue、metadata view 和 logd。actor 使用 actor stream，LLM 使用 LLM stream，但它们都通过 task lease、worker polling、completion 和 dashboard 这条运行时链路。

scheduler 也不是单独 demo。worker 注册和 heartbeat 会上报 cached models；LLM task poll 时 `canAssignTaskToWorker()` 会根据 resource-only、locality-aware 或 predicted-latency 策略决定这个 worker 能不能拿任务。`LLMCompleted` 事件又会更新 materialized LLM stats，影响后续 predicted-latency 调度。

我会承认：现在真实 GPU serving 的实验还没做，主要用 mock LLM 和 file-backed checkpoint cache。但架构上 LLM 不是孤立脚本，它已经接进 workflow、worker、log、scheduler 和 benchmark。

## Q042. LogServe 的技术债有哪些？哪一个最影响生产化？

技术债我会分几类说。

第一，shared log 还没有完整 physical compaction。现在有 segment rolling、logical trim、compactable records/bytes，但没有真正删除 compacted segment。长期运行时，日志空间还是会增长。

第二，control 还不是 HA。当前实验主要是单控制面。control 可以从 log bootstrap，但没有 leader election、并发 control fencing、分布式锁这些生产系统需要的东西。

第三，metadata store 和 log 之间不是事务。实现通过 log-first 顺序和测试挡住 metadata-only 状态，但 append 和 metadata update 没有一个真正的原子提交协议。

第四，worker 执行是 at-least-once。系统能保护内部状态提交，但用户函数的外部 side effect 需要业务自己幂等。

第五，LLM 侧还缺真实 GPU 压测。mock LLM 能验证调度和 cache 机制，但不能代表 vLLM 在真实模型、显存压力、batching 下的表现。

第六，安全和多租户还很薄。生产化要补认证、权限、隔离、审计、资源配额。

最影响生产化的，我会选 control HA 和 log/metadata 的一致性工程。因为只要 control 还是单点，系统就算能恢复，也会有可用性短板；只要 metadata 和 log 没有更系统的 materialization 机制，复杂场景下排障成本会高。

## Q043. 这个项目里面最值得放在简历第一条的技术点是什么？为什么？

我会把“基于 shared log 的 log-first AI runtime”放在第一条。

原因是它能把项目串起来。单独写 workflow DAG、actor、LLM scheduling 都可以，但容易看成几个功能点。写 log-first runtime，面试官会自然追问：source of truth 是什么？怎么 replay？怎么处理 worker crash？怎么做幂等？actor 顺序怎么保证？LLM 调度为什么要接进 runtime？

一条比较好的简历表述可以是：

> 设计并实现基于 shared log 的 AI runtime：控制面采用 log-first 语义，task/workflow/actor/LLM 事件先写 append-only log，再 materialize metadata view；支持 workflow replay、actor snapshot recovery、epoch fencing、LLM cache-aware scheduling 和故障注入验证。

这句话的好处是有系统设计、有一致性语义、有 AI workload，也有验证。它比单写“实现了任务队列”强很多。

如果面试官继续问，我可以展开 `BootstrapFromLog()`、`ReplayWorkflow()`、`ReplayActor()`、`LLMCompleted` materialized stats、idempotency fingerprint 这些代码细节。

## Q044. 哪些指标能够说明这个系统真的改善了某个问题？

我会分问题看指标。

如果问题是 workflow 失败恢复，那指标是“已完成 step 是否重跑”和 workflow end-to-end latency。故障恢复测试里，worker 在 `embed` 后退出，重启后 workflow 从 `search` 或后续 step 继续，不重新执行 `embed`。这说明 replay 和 step 去重起作用。

如果问题是 actor recovery，那指标是 full replay commands、snapshot replay commands、trimmed replay commands。实验里 actor full replay 是 21 条 command，snapshot replay 和 trimmed replay 是 1 条。这说明 snapshot 和 logical trim 确实降低 replay 工作量。

如果问题是 LLM 冷启动，那指标是 cache hit rate、cold start rate、p95/p99 latency、checkpoint fetch ms、model load ms。单机实验里 resource-only cache hit rate 是 0.833，locality-aware 是 1.000；resource-only p95 是 305 ms，locality-aware p95 是 205 ms。checkpoint cache probe 里 cold request `cache_hit=false`，warm request `cache_hit=true`，并且 worker-local cache 里有真实 checkpoint artifact。

如果问题是 logstore 性能，那指标是 append records/s、read records/s、recover ms、segment count。实验里 batch/interval fsync 的 append throughput 明显高于 always fsync，说明 fsync policy 对写入吞吐有影响。

如果问题是系统是否可解释，那指标不是单个数字，而是 replay consistency、dashboard snapshot、fault injection 结果。比如 `ReplayWorkflow` 和 metadata 一致，fault injection 里 worker kill recovery、queue redelivery、control restart probe 通过。

## Q045. 当前 benchmark 的可信度边界是什么？

当前 benchmark 的可信度边界要主动讲清楚。

可信的部分是机制对比。比如 locality-aware 比 resource-only 更容易命中已有模型缓存；snapshot replay 比 full replay 回放更少 command；batch/interval fsync 比 always fsync 写入吞吐更高。这些结论和系统机制直接相关，单机实验可以支撑。

不该过度外推的部分是生产性能。当前实验是在单机 Ubuntu 环境，3 个 worker，mock LLM，checkpoint 文件比较小。它不能代表多节点网络、真实 GPU、真实 vLLM serving、大模型 checkpoint、复杂租户负载下的延迟和吞吐。

样本规模也比较小。比如 workflow p95/p99 如果请求数只有几个，统计意义有限，更适合作为 smoke benchmark 或机制验证，不适合作为严肃性能论文里的结论。

还有一个边界是实现成熟度。当前 logstore benchmark 能说明 fsync policy、segment recovery 的基本表现，但没有覆盖多 producer、高并发写、多磁盘、长期 compaction 后的退化情况。

所以我会这样总结：这些 benchmark 能证明“代码跑通了，机制方向有收益”，不能证明“系统已经具备生产级性能”。如果要提高可信度，下一步应该扩大 workload，固定随机种子，跑多轮取置信区间，上真实 vLLM/GPU，增加多节点和长时间运行实验。
