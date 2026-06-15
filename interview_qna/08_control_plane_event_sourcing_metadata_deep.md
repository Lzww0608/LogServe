# 三、Control Plane、Event Sourcing 与 Metadata View（深度）

这一组问题会追到 control plane 的正确性边界。回答时不要只说“log-first”，要讲清楚哪些路径确实先写日志，哪些状态只是高频租约视图，metadata 落后时系统怎样恢复，以及当前单 control 设计为什么不能直接 active-active。

## Q181. control plane 在哪些路径上先 append log，再更新 metadata？

主线是：凡是会改变可恢复业务状态的路径，都应该先写 shared log，再更新 metadata view。

普通 task 提交是最典型的路径。`SubmitTask` 最终会进入 `enqueueTaskWithMetadata`，先向 `task:<task_id>` 写 `TaskSubmitted`，append 成功后才 `CreateTask`，再把 task id 放进本地 queue。

workflow 也是 log-first。`SubmitWorkflow` 先写 `WorkflowStarted`，再创建 workflow metadata。step 调度时，control 先创建对应 task 的 `TaskSubmitted`，再往 workflow stream 写 `StepScheduled`，之后更新 workflow step 的 task id、attempt 等 view。step started、succeeded、failed、workflow completed/failed 也都是先写 workflow stream，再更新 workflow state。

actor 路径也类似。`CreateActor` 先写 `ActorCreated`，再创建 actor metadata。actor ownership 先写 `ActorOwnershipGranted`，再更新 owner worker 和 epoch。actor command 先写 `ActorCommandSubmitted`，再创建任务并更新 submitted command count。command applied/failed 先写 actor stream，再更新 actor state。snapshot 也是先写 `ActorSnapshotCreated`，再更新 snapshot ref 和 trim。

系统配置也走 log-first。`RegisterModel` 写 `ModelRegistered` 后更新 model registry；`SetSchedulingPolicy` 写 `SchedulingPolicyChanged` 后更新 policy；`RegisterWorker` 写 `WorkerRegistered` 后更新 worker metadata；`SetBackpressure` 写 `BackpressureConfigured` 后更新内存配置。redelivery 也是先写 `TaskRedelivered`，再把任务放回队列。

有一个边界要说清楚：worker 写 `TaskStarted`、`TaskCompleted`、`TaskFailed` 是 worker 直接写 log，然后再调用 control 的 `StartTask` 或 `CompleteTask`。control 不重复写这些 task lifecycle 事件。heartbeat 也不是每次写 log，它是高频租约状态，主要更新 metadata view。

## Q182. 如果 AppendLog 重试导致重复事件，control 如何处理？

低层依赖 logstore 的 idempotent append。`AppendLog` 带 `stream_id + idempotency_key`，同一组合重复写入时，logstore 返回 `duplicate=true`，不会新增一条 record。

control 上层还有 fingerprint 检查。task、workflow、actor create 都会对请求内容算 fingerprint。相同 idempotency key 重新提交时，如果 fingerprint 相同，就返回已有对象；如果 fingerprint 不同，就返回 conflict。

workflow step result 也有稳定幂等键。比如 step success 用 `workflow_id + step_id + input_hash + succeeded` 这类 key，避免 worker retry 或 redelivery 导致 final step result 写两次。

但不是所有内部事件都适合完全幂等。比如 `RegisterWorker`、`SetSchedulingPolicy`、`SetBackpressure` 里有些 idempotency key 带时间戳，语义上更像“配置变更事件”而不是“同一次请求重试”。如果客户端需要严格 retry-safe，这些入口也应该让客户端传稳定 request id。

所以回答要分层：logstore 能去重同一幂等键；control 用 fingerprint 防止同 key 不同内容；但生产级 API 还应把所有外部可重试操作都显式 request-id 化。

## Q183. 如果 metadata 更新失败，重启时 BootstrapFromLog 是否能修复？

多数情况下可以，因为事件已经在 shared log 里。

例如 `TaskSubmitted` 写成功后，`CreateTask` 失败。control 当时的查询可能看不到这个 task，但重启后 `BootstrapFromLog` 会扫描 `task:` stream，从 `TaskSubmitted` 里恢复 TaskSpec，再把 task 放回 metadata 和 queue。

workflow、actor、model registry、scheduler policy、backpressure、LLM stats 也是这个思路。只要 log 里有事件，bootstrap 就能重建 materialized view。

但这不是魔法。第一，如果 metadata store 持续不可用，比如 PostgreSQL 一直连不上，bootstrap 也没法把 view 写进去。第二，heartbeat、running task load 这类租约状态不是完整事件历史，恢复时要重新估计。第三，如果事件写入和对象存储写入之间出现问题，比如 snapshot ref 指向的对象丢了，bootstrap 也会受影响。

所以准确说法是：log append 成功但 metadata 更新失败，系统有恢复路径；恢复成功还取决于 log、result store 和 metadata store 本身可用。

## Q184. BootstrapFromLog 的顺序为什么是 models、workers、scheduler、backpressure、tasks、workflows、actors、LLM stats？

这个顺序是为了先恢复“被后续恢复过程依赖的基础视图”，再恢复具体执行状态。

models 先恢复，是因为 LLM task 和 scheduler 可能要查 model registry。workers 先恢复，是因为 actor ownership、LLM locality、worker capacity 都依赖 worker view。

scheduler 和 backpressure 是运行时配置。恢复它们后，后续 schedule ready steps、redelivery、任务入队时能使用和崩溃前一致的策略。

tasks 先于 workflows，是因为普通 ad-hoc task 可以直接从 `task:` stream 恢复。workflow task 不在这一步恢复，`bootstrapTasks` 会跳过带 workflow id 或 actor id 的 task。workflow 后面会从 `wf:` stream replay 出 DAG 状态，再根据 step 状态重建需要继续执行的 workflow task spec。

actors 在 workflows 后面恢复，主要是它们有自己的 actor stream、snapshot 和 ownership 逻辑。LLM stats 最后恢复，因为它是调度优化用的 materialized stats，可以从 `llm:` streams 重建，不应该阻塞 task/workflow/actor 的基本恢复。

这个顺序不是唯一正确答案，但它遵循一个原则：先恢复配置和资源视图，再恢复需要调度的对象状态，最后恢复优化型统计。

## Q185. 如果 bootstrap 过程中读取到半恢复的 task stream，状态如何判定？

`replayTaskSpec` 会按事件顺序推导 task 状态。

如果没有 `TaskSubmitted`，说明没有可执行 spec，这条 stream 会被忽略。只要有 `TaskSubmitted`，默认状态是 `QUEUED`。

看到 `TaskStarted` 后，状态变成 `RUNNING`，并记录 `task_lease_epoch`。看到 `TaskRedelivered` 后，如果任务还不是 terminal，并且 redelivery 的 lease epoch 不比当前旧，状态会回到 `QUEUED`。

看到 `TaskCompleted` 或 `TaskFailed` 时，replay 不会无条件接受。它会调用 `taskTerminalEventApplies`，检查 terminal event 是否和当前 lease epoch 匹配。旧 worker 在 redelivery 之后补交的完成事件会被忽略。

最后还有一个保守处理：如果 replay 到末尾状态仍是 `RUNNING`，bootstrap 会把它恢复成 `QUEUED`。原因是 control 重启后无法相信旧 worker lease 仍然有效，宁愿让任务重新被 poll，也不能让它永远卡在 running。

## Q186. 为什么 running task 在重启后通常应该恢复为 queued？

因为 control 重启后，内存里的租约状态、队列状态和 worker 连接状态都丢了。一个 task 在崩溃前是 running，只能说明当时它被某个 worker 拿走过，不能说明这个 worker 现在还活着，也不能说明它最终会调用 CompleteTask。

恢复成 queued 能保证任务不会丢。worker 如果已经崩溃，新 worker 可以重新执行。如果旧 worker其实还在跑，后面它提交结果时，control 会用 worker id 和 task lease epoch 判断是否接受。

这对应的是至少一次执行语义。系统宁愿让任务有机会重跑，也不要因为一个过期 running 状态把任务永久挂住。

对有外部副作用的任务，这意味着用户函数自己也要有幂等设计。LogServe 可以保护最终结果提交层，但不能保证用户代码本身只执行一次。

## Q187. TaskStarted、TaskCompleted、TaskFailed 在 replay 中如何影响任务状态？

`TaskStarted` 表示任务被某个 worker 开始执行。replay 后状态变成 `RUNNING`，并更新当前 lease epoch。

`TaskCompleted` 表示成功完成。只有 terminal event 的 lease epoch 和当前运行租约匹配，或者事件没有 lease epoch 的旧格式，replay 才会把状态改成 `SUCCEEDED`。

`TaskFailed` 表示执行失败。判断逻辑和 completed 类似，通过 lease epoch 防止旧失败事件覆盖新 lease。

还要看 `TaskRedelivered`。如果任务被 redeliver 回队列，后面旧 lease 的 completed/failed 不再适用。这个规则在故障恢复测试里很关键：旧 worker 被 kill 或超时后，任务重新调度，旧结果不能覆盖新的执行路径。

最终如果 replay 状态还是 `RUNNING`，bootstrap 会把它降回 `QUEUED`，让系统重新调度。

## Q188. stale terminal event 为什么需要根据 lease epoch 判断是否 applies？

因为 worker 可能在失联后又恢复。

假设 worker-1 拿到 task，lease epoch 是 1。它执行很慢，control 认为它超时，于是写 `TaskRedelivered`，把 task 放回队列。worker-2 拿到同一个 task，lease epoch 变成 2。

如果 worker-1 这时把旧结果交回来，不能直接接受。否则 epoch 1 的旧结果会覆盖 epoch 2 的新执行，workflow step、actor state 或 task result 都可能被写错。

lease epoch 的作用就是区分“这次完成属于哪次租约”。terminal event 只有和当前 lease 对得上，才 applies。

这也是 LogServe 的 exactly-once-ish 语义之一：任务执行可以至少一次，但最终结果提交要经过租约和幂等检查。

## Q189. workflow 和 actor 的恢复为什么不只依赖 task stream？

task stream 只知道某个 task 的执行历史，不知道更高层对象的完整状态。

workflow 需要 DAG 定义、step 依赖、每个 step 的 attempts、input hash、result ref、workflow final status。这些信息在 `wf:<workflow_id>` stream 里。只看 task stream，无法知道某个 task 是哪个 step 的第几次 attempt，也无法判断下一个 step 是否 ready。

actor 更不能只看 task stream。actor 的核心状态是 command log 和内存状态演进。`actor:<actor_id>` stream 记录 `ActorCreated`、`ActorOwnershipGranted`、`ActorCommandSubmitted`、`ActorCommandApplied`、snapshot 等事件。task stream 只是执行 actor method 的外壳，不足以解释 actor state。

所以恢复要分层：task stream 恢复执行单元，workflow stream 恢复 DAG 状态，actor stream 恢复有状态对象。它们互相引用，但不能互相替代。

## Q190. materialized metadata view 是否可能落后于 log？落后时用户查询看到什么？

会，而且这是 log-first 设计允许的状态。

最常见的情况是：事件已经 append 成功，但 metadata 更新还没完成，或者 metadata 更新失败。此时 shared log 里已经有事实，metadata view 还没追上。

用户通过 `GetTaskStatus`、`GetWorkflowStatus`、`GetActorStatus`、`GetDashboardSnapshot` 查到的是 metadata view，所以可能看到旧状态、缺少新对象，或者 workflow step 还没更新。

这类落后不是数据丢失。重启后 `BootstrapFromLog` 可以把 view 追到 log。对 workflow 和 actor，还有 `ReplayWorkflow`、`ReplayActor` 可以直接从 log 重建状态并和 metadata 比较。

面试里我会说：LogServe 选择让查询偶尔落后于 log，也不让 metadata 领先于 log。前者可恢复，后者会破坏 source of truth。

## Q191. 如果两个 control 实例同时运行，当前设计会出现什么问题？

当前设计默认单 control writer。两个 control 同时运行会出问题。

第一，本地 queue 不是共享的。两个 control 各自有自己的 `queue` 和 `specs` map。同一个 task 可能只在某个 control 的队列里，连到另一个 control 的 worker poll 不到。

第二，调度可能重复。两个 control 都可能从同一批 log 或 metadata 里判断某个 workflow step ready，然后各自创建 task。部分幂等键能挡住重复结果，但不能完整挡住所有调度副作用。

第三，worker load 和 capacity view 会分叉。`RunningTasks` 是 metadata view，但每个 control 的队列和 poll 流程不共享锁，两个实例可能同时给同一个 worker 分配超出 capacity 的任务。

第四，actor ownership 有 split-brain 风险。两个 control 都可能认为需要给 actor 重新分配 owner，并写不同 epoch 的 ownership event。epoch 能帮助 fencing 旧 worker，但 control 本身没有 leader 协议，仍然会产生复杂竞争。

所以当前系统可以多 worker，但不应该直接多 active control。

## Q192. 如何让 control plane 支持 active-active？

要把本地内存里的“写路径状态”移到共享且有并发控制的地方。

第一，queue 不能是本地 slice。可以用 PostgreSQL `FOR UPDATE SKIP LOCKED`、NATS JetStream、Redis stream，或者基于 shared log 的 durable queue。关键是 task leasing 必须是原子操作。

第二，所有状态转移要有 compare-and-swap。比如 task 从 queued 到 running，必须检查当前 status、lease epoch、worker capacity，然后原子更新。两个 control 同时抢同一 task，只有一个能成功。

第三，workflow 调度要有幂等的 step scheduling。可以让 `workflow_id + step_id + input_hash + attempt` 成为唯一键，保证多个 control 即使同时发现 ready，也只会创建一个有效 task。

第四，actor 要有单 owner 和 epoch fencing。actor command 要按 command_seq 串行，ownership 变更必须通过一个线性化的路径。

第五，可以选择 leader election 或 sharding。最简单是 active-passive：一个 leader control 写，其他只读。更复杂的是按 workflow/actor/task shard 分配 ownership，每个 shard 一个 writer。

active-active 的核心不是多起几个进程，而是把 queue、lease、ownership、idempotency、metadata update 都变成可并发验证的协议。

## Q193. 控制面中的全局 queueMu 会成为瓶颈吗？

会，在任务量上来后会成为瓶颈。

当前 queue 是一个本地 slice，`queueMu` 保护入队、出队和扫描。`PollTask` 会拿锁遍历 queue，查 specs map、metadata、actor mailbox、worker capacity。任务多时，单个 worker poll 可能持锁较久，其他提交和 poll 都要等。

队列删除也是 slice splice，复杂度不低。队列很长时，频繁从中间删除会产生额外拷贝。

当前实现适合单机实验，逻辑清楚，方便 debug。要提高吞吐，可以做几件事：按任务类型或模型分 shard queue；用 heap 或 deque 存 ready task；把 actor mailbox 单独排队；把队列放到外部 broker；或者让 worker poll 使用数据库行级锁。

## Q194. queue 中存 taskID 而不是完整 TaskSpec 的优缺点是什么？

优点是队列轻。queue 只存 task id，真正的 TaskSpec 放在 `specs` map，当前状态放在 metadata。这样队列移动成本低，也避免同一份 spec 在多个地方复制。

另一个好处是状态判断更统一。PollTask 拿到 task id 后，会查 metadata 判断 status、worker、actor、LLM 等信息，而不是相信队列里的旧快照。

缺点是多了一层一致性问题。queue 里有 task id，但 specs map 里没有 spec，worker 就不能执行。当前 PollTask 遇到这种情况会把这个 task id 从队列里移除。

它也让本地内存更关键。control 进程内如果 specs map 丢了但 queue 还在，任务会暂时不可执行。重启后可以通过 BootstrapFromLog 重建一部分 spec，但这说明 queue 不是 durable queue。

## Q195. specs map 和 metadata task 之间如何保持一致？

正常路径里，它们在同一个 enqueue 流程里更新。

`enqueueTaskWithMetadata` 先写 `TaskSubmitted`，再 `CreateTask` 写 metadata，然后把 `specs[taskID] = cloneSpec(spec)`，最后把 task id append 到 queue。

如果进程在这些步骤中间崩溃，内存里的 specs 和 queue 都可能没更新完整。恢复依赖 shared log。Bootstrap 会从 `TaskSubmitted` payload 里拿回 TaskSpec，再补回 specs map 和 queue。

运行中如果 queue 里有 task id，但 specs map 没有，PollTask 会把这个坏队列项删除。这能避免 worker 收到空 spec，但也意味着这个 task 需要依赖 bootstrap 或重新调度恢复。

生产化时，我会把 spec 和 task state 存到同一个持久 view 里，或者把 queue 本身做成 durable queue，减少本地 map 的一致性压力。

## Q196. 如果 specs map 丢失但 task 还在 metadata 中，worker 能否执行？

不能直接执行。worker `PollTask` 返回的是 `TaskSpec`，如果 control 找不到 spec，就没有函数源码、函数名、args、LLM 参数或 actor 参数，worker 没法跑。

当前恢复能力分几类。

普通 ad-hoc task 可以从 `TaskSubmitted` payload 恢复 spec。`bootstrapTasks` 会读 `task:` stream，把 spec 放回 specs map，并把未完成任务放回 queue。

workflow task 可以从 workflow definition 和 step state 重建。`bootstrapWorkflows` replay workflow 后，会根据 step 的 task id、attempt、args 重新构造 TaskSpec。

actor pending call 的恢复更复杂。actor stream 能恢复 actor state 和 command history，task stream 里也有 TaskSubmitted，但当前 bootstrap 对 actor task 的恢复没有普通 task 和 workflow task 那么完整。这是一个需要继续生产化的边界。

所以回答要实在：specs map 丢了，当前进程不能执行；重启 bootstrap 能恢复主要路径，但 actor pending call 的 durable queue 恢复还应继续加强。

## Q197. 为什么 TaskSubmitted payload 需要保存 TaskSpec？

因为 TaskSpec 是 worker 执行任务的最小信息集合。

metadata task 只保存 task id、状态、worker、workflow id、step id、actor id、模型名等当前视图。它不一定保存完整函数源码、args、timeout、LLM adapter、actor class source 这些执行细节。

如果 `TaskSubmitted` 事件里不保存 TaskSpec，control 重启后只能知道“有一个 task 还没完成”，但不知道怎么执行它。

把 TaskSpec 放进 log 后，BootstrapFromLog 可以重建 specs map 和 queue。对 event sourcing 来说，这很重要：事件不只是状态标签，还要包含未来恢复所需的输入。

代价是日志会变大，尤其 `function_source` 很长时。后续可以把源码和大参数放到 object store，事件里只放 content hash 和 ref。

## Q198. TaskSpec 中存 function_source 有哪些风险？

第一是安全风险。function_source 是要被 worker 执行的代码。如果没有 sandbox、权限隔离和依赖控制，用户代码可以做危险操作。

第二是日志膨胀。每个 task 都把源码放进 TaskSpec，会让 shared log 变大。workflow 多次调度同一个函数时，源码会重复出现。

第三是泄密风险。用户可能把 token、路径、内部配置写进源码。源码进入 log 后，保留时间通常比一次请求更长，后续审计和权限要更严格。

第四是兼容风险。源码依赖 Python 环境、包版本、文件路径。今天能跑的 source，换一台 worker 未必能跑。

更稳的做法是把 function_source 变成 artifact ref。SDK 上传代码包，log 里记录 hash、ref、入口函数、依赖信息。worker 按 ref 拉取并校验 hash。

## Q199. idempotency fingerprint 是为了解决什么问题？

它解决“同一个 idempotency key 被不同请求复用”的问题。

如果只看 idempotency key，用户第一次提交 `query=A`，第二次误用同一个 key 提交 `query=B`，系统可能直接返回第一次的 task 或 workflow。这对用户来说很危险，因为第二次请求被静默吞掉了。

fingerprint 会把请求关键内容做稳定哈希。task fingerprint 包含 task name、function name、function source、args、workflow/actor/LLM 相关字段、timeout 等。workflow 和 actor create 也有自己的 fingerprint。

同 key 同 fingerprint，说明是同一次逻辑请求的重试，可以返回已有对象。同 key 不同 fingerprint，说明请求内容变了，应该报 conflict。

这比单纯 duplicate 更安全，也更适合面试里解释 idempotency 的严谨性。

## Q200. 如果同一个 idempotency_key 但 payload 不同，应该返回已有结果还是冲突？

应该返回冲突。

返回已有结果只适合同一个逻辑请求的重试。payload 不同，说明这已经不是同一个请求了。继续返回旧结果会掩盖调用方 bug，也可能让用户误以为新请求已经执行。

LogServe 当前在 control 层用 fingerprint 做这个判断。相同 key 但 fingerprint 不同，会返回 `idempotency conflict`。

logstore 低层只知道 `stream_id + idempotency_key`，不比较 payload。所以 payload conflict 应该在 control 或 SDK 层拦住。更进一步可以把 payload hash 放进 log append 协议，让 logstore 也能识别冲突。

## Q201. 控制面如何判断 worker active？heartbeat 超时阈值如何设置？

control 主要看 worker 的 `LastHeartbeat`。

worker 注册后会定期调用 `Heartbeat`，control 更新 metadata 里的 last heartbeat 和 cached models。`ActiveWorkers(maxAge)` 会返回最近 `maxAge` 内有 heartbeat 的 worker。

当前代码里有两个常量值得记住：actor owner lease 是 750ms，scheduler worker lease 是 5s。actor ownership 对失联更敏感，因为同一个 actor 不能有两个 owner；调度器对普通 worker active 的判断可以稍微宽一点。

阈值设置要和 heartbeat 间隔、网络抖动、GC pause、任务执行时间配合。太短会误判 worker 死亡，导致 actor ownership 抖动或任务重投；太长会让故障恢复变慢。

实践里我会从 `3 到 5 倍 heartbeat interval` 起步，再根据实验里的 false positive 和 recovery latency 调整。对 actor 这种状态对象，宁愿快一点 fencing；对普通 task，可以稍微保守。

## Q202. worker 的 RunningTasks 何时增加、何时减少？异常情况下会不会泄漏？

RunningTasks 在 `PollTask` 成功 lease 一个 queued task 给 worker 时增加。代码里只有当 task 原来是 `QUEUED`，才 `IncrementWorkerLoad(workerID)`。

它在 `CompleteTask` 成功处理 running task 后减少。redelivery 也会减少旧 worker 的 load：过期 running task 被放回队列时，如果原来有 worker id，就 `DecrementWorkerLoad`。

异常情况下可能暂时泄漏。比如 worker 拿到任务后崩溃，redelivery_timeout 没开或设置太长，RunningTasks 会一直占着 capacity。CompleteTask 调用失败但 worker 没重试，也会让 view 停留在 running。

control 重启后，memory view 会丢，bootstrap 恢复 worker 注册时 RunningTasks 通常回到 0，running task 会被恢复成 queued。这能清掉一部分泄漏，但也说明 RunningTasks 是调度 view，不是不可变事实。

生产化时可以把 worker load 设计成由 task leases 聚合出来，而不是单独维护一个容易漂移的计数。

## Q203. redeliverExpiredTasks 如果和原 worker 完成同时发生，如何避免双写？

靠 lease epoch、状态检查和 replay 规则一起处理。

redelivery 会先写 `TaskRedelivered`，再把过期 running task requeue。worker 完成时会写 `TaskCompleted` 或 `TaskFailed`，payload 里带 `task_lease_epoch`，然后调用 `CompleteTask`。

如果旧 worker 的 complete 发生在 redelivery 之后，它的 lease epoch 已经过期。metadata 的 `ValidateTaskLease` 会拒绝 stale completion；replay 时 `taskTerminalEventApplies` 也会忽略旧 lease 的 terminal event。

如果 complete 先成功把 task 变成 terminal，redelivery 扫描 running task 时就不应该再处理它。即使日志里出现竞争顺序，最终 replay 也按事件和 epoch 判定。

当前实现不是一个跨 log 和 metadata 的单事务，所以极端并发下仍要依赖重试和 replay 收敛。生产化可以把 redelivery 的 compare-and-swap 放进 metadata 事务，条件是 status 仍为 running 且 epoch 未变。

## Q204. backpressure 应该在提交前、排队前还是执行前生效？

三处都可以有 backpressure，但作用不同。

入口提交前最重要。LogServe 当前在 `enqueueTaskWithMetadata` 里，写 `TaskSubmitted` 之前检查 queue high watermark 和 log append slow。这样可以避免 shared log 接受一批系统已经处理不了的新任务。

排队前也需要检查。任务一旦进入 queue，就会占用内存和调度资源。queue high watermark 正是在入队前挡住过量请求。

执行前也要有控制。worker capacity、RunningTasks、actor mailbox、LLM locality 都属于执行前调度约束。它们不一定拒绝请求，但会决定任务什么时候被 worker 拿走。

当前设计还有一个细节：幂等重复请求应该先于 backpressure 返回已有对象。否则用户重试查询同一个提交结果时，可能因为队列满被拒绝，这不合理。

## Q205. last log append latency 作为 backpressure 信号是否充分？

不充分，但有用。

它有用是因为 LogServe 是 log-first。append log 慢，意味着所有状态推进都会慢。用 `last_log_append_ms` 做早期限流，能防止日志层已经吃紧时继续接入新任务。

不足也明显。它只看最近一次 append，容易被偶发抖动影响，也可能错过持续尾延迟。比如最近一次很快，但过去一分钟 p99 很高，系统仍然不健康。反过来，单次慢 append 也可能误伤一批新请求。

更好的信号应该包括滑动窗口 p95/p99、append error rate、logd queue depth、fsync latency、segment rollover latency、control queue depth、worker capacity 使用率。

所以我会说：当前指标适合实验和最小 backpressure；生产化要换成窗口化指标。

## Q206. DashboardSnapshot 从 materialized view 读，是否可能与日志不一致？

可能。

DashboardSnapshot 主要读 metadata view：tasks、workflows、actors、workers、models、queue depth、RunningTasks 等。它不会每次请求都 replay shared log。

如果 log append 成功但 metadata 更新失败，dashboard 会落后于 log。如果 control 正在 bootstrap，dashboard 也可能看到部分恢复状态。

这不是 dashboard 的 bug，而是 materialized view 的性质。它读起来快，但可能滞后。需要强校验时，应使用 `ReplayWorkflow`、`ReplayActor` 或专门的 consistency checker，从 log 重建状态后和 metadata 对比。

当前 DashboardSnapshot 里也会从 log service 读 compactable stats，所以它是 metadata view 加一点 log stats 的混合视图，不是严格日志 replay 结果。

## Q207. 控制面日志 append 慢但 worker 空闲时，调度器该优先保护谁？

应该优先保护 shared log 和系统入口，而不是为了填满 worker 继续接新任务。

LogServe 的状态推进依赖 log。log append 慢时，如果还继续大量提交新 task，队列会涨，metadata 会滞后，故障恢复成本也会变高。worker 空闲只是计算资源空闲，不代表系统可以安全接入更多状态变更。

更合理的策略是分优先级。新提交请求可以被 backpressure 拒绝；已经开始的任务 completion、actor command applied、redelivery 这类恢复和收尾事件应尽量优先写 log。也就是说，保护系统一致性和已接收任务的完成，比追求新任务吞吐更重要。

后续可以做 log append priority queue：terminal events 和 recovery events 高优先级，new submissions 低优先级。当前实现还比较简单，只用最近 append latency 限制新 task。

## Q208. 如果 metadata store 是 PostgreSQL，事务边界如何设计？

如果 shared log 仍是独立 logd，我会把事务边界设计成：先 append log，append 成功后在 PostgreSQL 事务里更新 metadata view，并记录 applied event position。

这样 metadata 落后于 log 时，可以靠 projector 或 bootstrap 补齐。PostgreSQL transaction 只保证 view 内部一致，比如 task row、worker load、workflow step row 一起更新。

不能先开 PostgreSQL 事务写 metadata，再 append log。那样 AppendLog 失败时会产生 metadata-only 状态，破坏 log-first。

如果把事件日志也放进 PostgreSQL，另一种设计是同一个 DB transaction 里插入 event row 和更新 materialized view。这会简单很多，但 shared log 就变成 PostgreSQL event table，不再是当前独立 logd 的架构。

当前 PostgresStore 更像 memory view 的持久化镜像，更新时先改 memory，再异步式调用 persist 方法记录到 PostgreSQL。生产化时应让 DB 更新错误参与请求返回或重试队列，而不是只记 LastError。

## Q209. 如果 PostgreSQL commit 成功但 AppendLog 失败，会破坏 log-first 吗？

会。这正是 log-first 要避免的情况。

如果 metadata 先 commit，log append 后失败，系统就有了无法从 shared log 解释的状态。control 重启后按 log bootstrap，会恢复不出这次 metadata 更新。此时 PostgreSQL view 和 source of truth 分叉。

当前代码有专门的 log-first 测试，覆盖 SubmitWorkflow、CreateActor、RegisterModel、SetSchedulingPolicy、RegisterWorker、SetBackpressure、redelivery 等路径：append 失败时，不应该更新 metadata 或配置。

正确顺序是 AppendLog 成功在前，PostgreSQL commit 在后。后者失败时，view 落后但可修复；前者失败时，不能产生 view 更新。

如果业务要求 log 和 metadata 完全原子，要么把两者放进同一个数据库事务，要么引入分布式事务。但 LogServe 当前选择的是 event log 为事实源，metadata 可以重建。

## Q210. 如何实现控制面读写的幂等和重试安全？

第一，所有外部写请求都要有稳定 idempotency key。客户端超时后用同一个 key 重试，control 能返回同一个 task、workflow、actor 或 LLM 请求，而不是创建新对象。

第二，idempotency key 要配 fingerprint。同 key 同内容才是重试；同 key 不同内容要返回 conflict。

第三，日志事件本身也要有稳定幂等键。`TaskSubmitted`、`StepSucceeded`、`ActorCommandApplied` 这类事件不能因为 RPC redelivery 写两次。logstore 的 `duplicate=true` 应该被当作成功重试处理。

第四，状态机要单调。task 从 queued 到 running 到 terminal；workflow step 从 scheduled 到 started 到 succeeded/failed；actor command_seq 只能按顺序推进。旧事件不能让状态倒退。

第五，要有 fencing。task lease epoch 防 stale completion，actor epoch 防旧 owner 写 actor state。active-active 时还需要 producer epoch 或 control epoch。

第六，读路径要接受 metadata 可能滞后。用户查状态读 metadata，必要时可以提供 replay-based consistency check。这样重试后即使马上查不到最新 view，也能通过 log 恢复。

一句话总结：幂等键解决重复请求，fingerprint 解决同 key 不同内容，lease/epoch 解决旧执行者复活，event log 解决重启恢复。缺一块，重试安全都会打折。
