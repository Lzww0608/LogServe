# 四、Task Scheduling、Worker、Lease、Redelivery 与 Backpressure（深度）

这一组问题会追到调度和执行链路里的边界情况。回答时不要只背流程，要把几个不太舒服但真实存在的点讲清楚：任务被 poll 后已经拿到 lease；worker 侧先写生命周期事件再通知 control；本地 executor pool 会影响实际并发；redelivery timeout 配不好会带来重复执行或恢复过慢。

## Q251. PollTask 中选择任务的条件有哪些？

`PollTask` 第一件事是校验 `worker_id`，没有 worker id 直接返回错误。

然后它会先调用 `redeliverExpiredTasks`，把已经超时的 running task 重新放回队列。这个动作如果写 `TaskRedelivered` 日志失败，`PollTask` 会返回错误，不会继续分配新任务。

接着 control 拿 `queueMu`，按本地 queue 顺序扫描 task id。每个候选任务要过几道检查：

- `specs` 里必须有 TaskSpec；没有就从 queue 移除。
- metadata 里必须有 task；没有也从 queue 移除。
- 如果是 actor task，`actorMailboxReady` 必须返回 true。也就是 actor 有 owner，owner 是当前 worker，并且 command_seq 正好是下一个要执行的命令。
- `canAssignTaskToWorker` 必须返回 true。这里会处理 `target_worker_id`、LLM worker capacity 和调度策略。

都通过后，control 调用 `LeaseTask`。lease 成功后，如果任务之前是 queued，就给 worker 的 RunningTasks 加一；然后从 queue 中移除 task id，把带 `task_lease_epoch` 的 TaskSpec 返回给 worker。

## Q252. canAssignTaskToWorker 如何处理 target_worker_id？

`target_worker_id` 只对非 actor task 直接生效。

代码里的第一条判断是：如果不是 actor task，并且 TaskSpec 里有 `target_worker_id`，但它不等于当前 poll 的 worker id，就返回 false。

actor task 不走这条限制。actor 的分配由 actor ownership 和 mailbox 决定：只有 actor 当前 owner worker 才能拿到这个 actor 的命令，而且 command_seq 要连续。也就是说，actor 的目标 worker 来自 actor owner，不是普通 TaskSpec 的 target worker 字段。

如果 target worker 检查通过，并且不是 LLM task，`canAssignTaskToWorker` 会直接返回 true。LLM task 还要继续检查 worker 是否存在、是否有 capacity，以及当前调度策略是否认为这个 worker 是首选。

这个设计避免了普通任务被错误分配到其他 worker，同时也让 actor 任务服从 actor ownership，不会被一个静态 target 字段破坏。

## Q253. worker capacity 与本地 pool size 不一致时会发生什么？

capacity 和 pool size 管的是两层东西。

capacity 控制 worker 从 control plane 拉多少任务。worker 主循环里有 `inFlight < localCapacity` 的判断，所以 capacity 越大，一个 worker 可以同时持有的 lease 越多。本地队列 channel 的容量也按 capacity 设置。

pool size 控制实际执行槽。`task_pool_size`、`llm_pool_size`、`actor_pool_size` 分别决定三类任务有多少 executor goroutine 或 runner。

如果 capacity 大于本地 pool 的实际处理能力，worker 会拉到更多任务，但它们会堆在本地 queue 里，`local_queue_wait_ms` 会变高。control 这时已经认为这些 task 是 running，因为 lease 已经发出去了。

如果 pool size 大于 capacity，executor 可能空闲。因为 worker 最多只会拉 capacity 个 in-flight task，pool 再大也没有足够任务喂给它。

更麻烦的是类型不匹配。比如 capacity=8，普通 task pool=8，但 LLM pool=1。如果连续来 8 个 LLM task，它们会在 LLM queue 里排队，而普通 task runner 空着。这就是为什么实验里要分开看 task throughput、LLM latency 和 local queue wait。

## Q254. 本地 queue 满了时 Dispatch 会怎么表现？

`Dispatch` 会把 job 写入对应 channel：普通 task queue、LLM queue 或 actor queue。

它的 select 只有两个分支：`queue <- job` 和 `ctx.Done()`。也就是说，如果本地 queue 满了，并且 worker 的主 context 没取消，`Dispatch` 会阻塞等待队列出现空位。

这个点要讲清楚：任务在 `PollTask` 返回时已经被 control lease 给这个 worker，metadata 状态已经是 running。假如 worker 卡在 Dispatch，control 仍然认为任务在该 worker 上运行。

正常情况下，worker 主循环用 `inFlight < capacity` 限制拉取数量，本地 queue 容量也按 capacity 设置，所以不太容易无限堆积。但如果某一类队列特别慢，或者容量和 pool 配得很激进，Dispatch 阻塞会拖慢 worker 后续 heartbeat 和 poll。

生产化可以改成带超时的 Dispatch。超时后 worker 主动放弃 lease 或通知 control requeue，而不是一直拿着 task 不动。

## Q255. local executor pool 是否可能造成 head-of-line blocking？

会，主要有两种。

第一种是同一类队列内部的阻塞。普通 task queue、LLM queue、actor queue 都是 FIFO。某个长任务排在前面，会占住对应 pool 的执行槽。后面的短任务只能等，`local_queue_wait_ms` 会变高。

第二种是 actor 的 per-actor lock。actor pool 可以并行跑不同 actor，但同一个 actor 的命令必须串行。一个 actor 的慢命令会挡住同一 actor 后续命令。这是语义要求，不是实现错误。

分离三类 pool 可以减少跨类型阻塞。比如 LLM 冷启动不会占住普通 Python task runner；actor 慢命令也不会挡住普通 task。

但它不能消除所有 head-of-line blocking。要继续优化，可以做优先级队列、按任务类型设置不同 capacity、长短任务分 lane，或者让 control 在分配时考虑 worker 本地队列深度。

## Q256. LLM pool 和 task pool 分离解决什么问题？

它解决的是 LLM 请求拖慢普通任务的问题。

LLM task 可能要做模型加载、checkpoint fetch、mock sleep 或 vLLM HTTP 调用。这些操作耗时长，而且延迟分布和普通 Python 函数不一样。如果 LLM task 和普通 task 共用同一个 Python runner pool，几个冷启动就能把普通 task 堵住。

分离后，LLM task 进入 LLM queue，由 LLM worker goroutine 处理；普通 task 仍由 Python runner 处理。这样普通 task 的吞吐不会被模型冷启动直接拖垮。

另一个好处是观测指标更清楚。LLM pool 可以单独记录 cache hit、model load、first token、checkpoint fetch；普通 task 则关注执行时间和 Python runner 健康。

这也符合调度策略：LLM task 需要 locality-aware 或 predicted-latency，普通 task 主要看 worker capacity。

## Q257. actor pool 与 control-plane mailbox 的职责边界是什么？

control-plane mailbox 决定“哪个 actor command 可以被发出去”。它检查 actor owner、epoch 和 command_seq，保证同一个 actor 的命令按提交顺序进入执行。

worker actor pool 决定“本地怎么执行已经拿到的 actor task”。它用 actor queue 和 per-actor lock 保证同一个 actor 在本地不会并发执行。

两层都需要。只靠 control-plane mailbox，worker 本地多个 actor runner 仍可能同时取到同一 actor 的两个任务，尤其是在 command 快速连续提交时。只靠 worker lock，又不能防止错误 worker 拿到不属于自己的 actor command。

所以边界是：control 保证分配顺序和所有权，worker 保证本地执行串行化。

## Q258. task 被 LeaseTask 后状态如何变化？

`LeaseTask` 会读取 metadata 中的 task。

如果 task 已经是 terminal 状态，也就是 succeeded 或 failed，它直接返回当前 task，不再修改。正常情况下 terminal task 不应该还在 queue 里。

对非 terminal task，`LeaseTask` 会做三件事：`TaskLeaseEpoch++`，状态改成 `RUNNING`，`WorkerID` 设置为当前 worker。它还会更新 `UpdatedAtMs`。

返回给 worker 的 TaskSpec 会带上新的 `task_lease_epoch`。如果是 actor task，还会额外注入 actor owner、actor epoch 和当前 actor state。

## Q259. LeaseTask 为什么要递增 TaskLeaseEpoch？

因为一个 task 可能被不同 worker 多次拿到。

第一次分配给 worker-1，epoch 是 1。如果 worker-1 挂了，control redelivery 后又把它分给 worker-2，epoch 变成 2。两个 worker 后续都可能尝试提交结果，但只有 epoch=2 的提交应该被接受。

如果没有 epoch，只靠 worker_id 也不够。网络抖动、worker 重启、worker id 复用都可能让旧执行者看起来像合法执行者。epoch 给每次 lease 一个单调编号，能把旧租约和新租约分开。

它是 exactly-once-ish 语义里很关键的一块：任务函数可能执行多次，但最终结果提交必须只接受当前 lease 对应的那一次。

## Q260. ValidateTaskLease 拒绝 stale completion 的条件是什么？

`ValidateTaskLease` 先检查 task 是否存在。不存在直接错误。

如果 task 已经是 terminal 状态，它会返回当前 task，不再拒绝。这是为了让重复的 StartTask/CompleteTask 更容易变成幂等返回。

如果 task 还不是 terminal，就检查两个条件。

第一，metadata 中 task.WorkerID 不为空，且请求里的 worker_id 不为空，两者不一致，就返回 `stale task lease rejected`。

第二，请求里的 leaseEpoch 大于 0，并且不等于 metadata 当前 TaskLeaseEpoch，也返回 `stale task lease rejected`。

这两个条件挡住了两类旧请求：旧 worker 写回，以及旧 epoch 写回。

## Q261. 如果旧 worker 完成了已经 redeliver 给新 worker 的任务，会发生什么？

如果新 worker 已经拿到新 lease，metadata 里的 worker_id 和 TaskLeaseEpoch 已经变了。

旧 worker 再调用 `CompleteTask` 时，metadata 的 `CompleteTask` 会检查 worker_id 和 lease epoch。如果不匹配，就返回 `stale task lease rejected`。control 不会更新结果，也不会推进 workflow 或 actor 状态。

如果旧 worker 在被 redelivery 后先写了 `TaskCompleted` 日志，日志里会留下一个旧 epoch 的 terminal event。replay 时会根据 lease epoch 判断这个 terminal event 是否 applies。旧 epoch 的完成事件不应该覆盖新 lease 的状态。

所以系统会接受“旧 worker 可能真的算出了结果”这个事实，但不接受它作为最终结果。

## Q262. 如果 CompleteTask 到达时 task 已经是 terminal 状态，系统如何处理？

metadata 的 `CompleteTask` 对 terminal task 会直接返回当前 task，不再覆盖结果。

control 里 actor task 还有一层显式判断：如果 existing actor task 已经 terminal，直接返回 `Accepted: true`。普通 task 路径会调用 metadata `CompleteTask`，得到已有 task。

这让重复 CompleteTask 比较安全。比如 worker 完成后 RPC 超时，客户端重试同一个 CompleteTask，control 不会写第二个最终状态。

需要注意的是，control 在普通 task 路径里会根据 `wasTerminal` 避免重复 materialize LLM stats。workflow step completion 也有幂等保护，避免重复写最终 step result。

## Q263. 如果 worker 执行成功但写 TaskCompleted 日志失败，CompleteTask 是否应该继续？

不应该继续。当前 worker 实现也是这样：先 `AppendLog(TaskCompleted 或 TaskFailed)`，append 成功后才调用 `CompleteTask`。

原因很简单：LogServe 的主线是 log-first。如果执行成功后直接更新 metadata，却没有对应日志，重启后从 log replay 会恢复不出这个完成状态。metadata 和 shared log 就分叉了。

所以 TaskCompleted append 失败时，worker 返回错误，不调用 CompleteTask。任务会保持 running，后续 redelivery timeout 到了以后被重新投递。

代价是可能重复执行用户函数。这个系统选择的是保护可恢复状态，而不是保证用户代码只执行一次。外部副作用要靠用户函数自己的幂等键或补偿机制。

## Q264. worker 是先写 TaskStarted 日志还是先调用 StartTask？为什么？

worker 先写 `TaskStarted` 日志，再调用 `StartTask`。

代码里 `executeTask` 先 append `TaskStarted`，payload 带 task_id、worker_id、task_lease_epoch 和 local_queue_wait_ms。append 成功后才调用 control 的 `StartTask`。

这么做是为了保持 log-first：workflow step 从 scheduled 进入 started，是一个可恢复状态变化，必须先有日志事实，再更新 metadata view。

还有一个好处是排队指标不会丢。`local_queue_wait_ms` 写进 TaskStarted 事件后，后续 replay 或审计能知道任务在 worker 本地等了多久。

## Q265. worker 是先写 TaskCompleted/TaskFailed 日志还是先 CompleteTask？为什么？

worker 先写 `TaskCompleted` 或 `TaskFailed`，再调用 `CompleteTask`。

这是完成路径上的 log-first。完成结果是任务状态机里的 terminal transition，如果先更新 metadata，AppendLog 后失败，系统重启后会把任务恢复成未完成状态，和 metadata 冲突。

先写日志的代价是：日志写成功但 CompleteTask 失败时，metadata 可能短时间落后。但这个方向是可修复的，因为重启或 replay 能从日志看到 terminal event。

所以两种失败里，LogServe 更愿意接受“log 有、view 没追上”，不接受“view 有、log 没有”。

## Q266. 如果 TaskStarted 写成功但 StartTask 失败，replay 会如何看待这个任务？

replay 会看到 `TaskStarted`，把任务状态推进到 running，并记录对应 lease epoch。

如果后面没有 `TaskCompleted` 或 `TaskFailed`，bootstrap 到最后会把 running task 恢复为 queued。因为 control 重启后不能相信旧 worker lease 仍然有效，让任务重新投递比让它卡在 running 更安全。

这意味着 `TaskStarted` 写成功但 `StartTask` 失败不会永久丢任务。它最多让日志里留下一个“曾经启动过”的事实，后续靠 redelivery 或 bootstrap 继续执行。

如果这是 workflow step，StartTask 失败可能导致 workflow metadata 没有标记 step started。但日志里已有 TaskStarted，任务状态恢复仍有依据。生产化时可以让 workflow projector 也从 task stream 或 workflow stream 对齐这个状态，减少短暂不一致。

## Q267. 如果 CompleteTask 成功但 TaskCompleted 事件未写，重启后会不会丢完成状态？

如果真的发生这种顺序，就会有问题。重启后 `BootstrapFromLog` 以 shared log 为事实源，看不到 TaskCompleted，就不会恢复出完成状态。metadata 里的完成状态会变成无法解释的 view。

当前 worker 的正常代码避免了这个顺序。它必须先写 `TaskCompleted` 或 `TaskFailed`，成功后才调用 `CompleteTask`。

所以这个问题的准确回答是：设计上不允许 CompleteTask 成功但 terminal event 未写。只要所有写路径都遵守 worker 侧 log-first，这个状态不会从正常代码产生。

如果未来有人绕过 worker 直接调用 CompleteTask，就可能破坏语义。生产化应该在 control 的 `CompleteTask` 内部也检查或补写事件，或者只暴露一个由 control 统一写 terminal event 的 API。

## Q268. 当前任务完成事件写在 worker 侧，这和 control 侧写事件有什么区别？

worker 侧写事件的好处是执行者离事实最近。worker 知道本地 queue wait、执行结果、错误信息、LLM timing，也能在调用 CompleteTask 前把这些信息写进 task stream。

缺点是控制面不能完全约束事件写入。比如 worker 可能写了 TaskCompleted，但 CompleteTask 因网络失败没有到 control；或者旧 worker 写了旧 epoch 的完成事件，需要 replay 再过滤。

如果改成 control 侧写 terminal event，所有状态转换都集中在 control。control 可以在同一段逻辑里校验 lease、写 log、更新 metadata，语义更收敛。

代价是 worker 要把结果先发给 control，control 再写事件。大结果、超时重试、CompleteTask RPC 失败时的处理会更复杂。

当前实现更像“worker 先写事实，control materialize view”。这符合 log-first，但对 worker 的正确性要求更高。面试时可以承认这是一个后续可收敛的设计点。

## Q269. task redelivery timeout 太短会造成什么问题？太长会造成什么问题？

太短会导致误判。正常运行的长任务还没完成，就被 control 认为过期并重新投递。这样会增加重复执行，旧 worker 和新 worker 可能同时跑同一个 task。虽然 lease epoch 能保护最终结果提交，但用户代码的外部副作用可能已经发生两次。

太长会导致恢复慢。worker 真挂了以后，任务会长时间停在 running，后续 workflow step 或 actor command 被拖住，用户看到的是系统迟迟不推进。

所以 timeout 要结合任务类型设置。普通短任务可以短一些；LLM 冷启动和长 workflow step 要长一些；actor command 如果需要低延迟恢复，也要注意 owner lease 和 command timeout 的配合。

当前配置是全局 `redelivery_timeout_ms`。生产化更适合按 task class、expected duration、tenant 或队列设置不同超时。

## Q270. worker heartbeat interval 与 redelivery timeout 如何配合？

heartbeat interval 反映 control 观察 worker 活性的频率。redelivery timeout 反映 task running 多久没进展后可以重新投递。

redelivery timeout 应明显大于 heartbeat interval。否则 worker 只是一次 heartbeat 慢了，任务就可能被误 redelivery。

但当前 redelivery 的判断主要看 task 的 `UpdatedAtMs`，也就是 lease 或状态更新时间，不是直接看 heartbeat。heartbeat 更多影响 worker active 判断、LLM scheduling、actor ownership。即便 worker 还在 heartbeat，一个长时间没有完成的 running task 也可能到 redelivery timeout。

更细的设计可以让 worker 对 running task 续租，或者在 heartbeat 里带 running task progress。这样 control 能区分“worker 活着但任务卡住”和“worker 已经失联”。

## Q271. RunningTasks 计数如果异常不归零，调度会受什么影响？

RunningTasks 会影响 worker capacity 判断。LLM 调度里的 `workerHasCapacity` 用的是 `RunningTasks < Capacity`。如果计数异常偏高，scheduler 会认为这个 worker 忙，不把 LLM task 分给它。

对普通 task，当前 `PollTask` 的通用路径没有统一检查 worker capacity，主要由 worker 自己按本地 capacity 控制 poll。但 LLM task 和 locality-aware 调度会明显受 RunningTasks 影响。

偏高的 RunningTasks 还会影响 dashboard。用户会看到 worker 一直忙，但实际可能已经空闲。

异常偏低也有风险。control 可能把太多 LLM task 分给同一个 worker，导致本地 LLM queue 堆积，p95 latency 变差。

当前系统在 CompleteTask 和 redelivery 时会 decrement worker load，但如果某些边界路径没走到，就可能漂移。更稳的做法是定期从 task metadata 重算每个 worker 的 running count，作为 repair。

## Q272. 如何设计 worker graceful shutdown，避免任务丢失或重复执行？

第一步是停止 poll。worker 收到 shutdown 信号后，先不再拉新任务。

第二步是等待本地 in-flight 任务完成。给一个 grace period，让已经开始的任务正常写 TaskCompleted/TaskFailed 并调用 CompleteTask。

第三步是处理还在本地 queue 但没开始执行的任务。这些任务已经被 lease 给当前 worker，却没有真正执行。比较好的做法是显式调用一个 `ReleaseTask` 或 `NackTask`，由 control 写 requeue 事件并把它放回队列。当前实现没有这个 RPC，只能等 redelivery timeout。

第四步是对无法在 grace period 内完成的任务做标记。可以写 `TaskLeaseReleased` 或 `WorkerDraining` 事件，让 control 更快 redelivery，而不是等超时。

现在的实现更偏简单：context 取消后 worker 退出，未完成任务靠 redelivery 恢复。实验足够，生产化需要 graceful drain。

## Q273. 任务 timeout 后重启 Python runner 的原因是什么？

Python runner 是长驻进程，同一个 runner 会执行多个任务。

如果某个任务 timeout，runner 里可能留下不确定状态。比如用户代码卡住、全局变量改了一半、stdout 协议输出被打断、解释器状态异常。继续复用这个进程，下一次任务可能被污染。

所以 `pythonRunner.Execute` 在 context 取消时会 kill 当前 Python 进程。`executeTask` 发现 deadline exceeded 后，会对普通 task 和 actor task 调用 `runner.Restart`。

这不是为了让已经超时的任务恢复，而是为了保护后续任务。timeout 的任务会被标记为 failed，runner 重启后处理下一条任务。

## Q274. 如果 Python task 卡在 C 扩展或系统调用中，context timeout 能否中断？

Go 的 context 不能直接中断 Python 代码本身。它只能让 Go 侧知道超时了。

当前实现的做法是：`pythonRunner.Execute` 等待 stdout 响应时，如果 context done，就 kill Python 子进程。对普通 Python 代码、阻塞 C 扩展、系统调用来说，杀进程通常是最直接的中断方式。

但也有边界。进程 kill 不是优雅取消，任务没有机会清理资源。某些子进程、文件锁、外部连接可能残留。如果用户代码又启动了自己的子进程，只 kill runner 主进程可能不够。

生产化要用更强的隔离单元，比如每个 task 独立进程组、容器、cgroup，timeout 后杀整个进程组，并清理临时目录和网络连接。

## Q275. 如何隔离用户代码的 CPU、内存、文件系统和网络访问？

当前 Python runner 是本地子进程，隔离能力有限。它适合实验和 demo，不适合直接跑不可信用户代码。

生产化可以分几层做。

CPU 和内存用 cgroup 或容器限制。每个 task 或每个 worker sandbox 设置 CPU quota、memory limit、oom 策略。

文件系统用临时工作目录、只读挂载和白名单路径。任务只能访问自己的 workspace，不能读宿主机敏感目录。

网络用 namespace、iptables 或容器网络策略。默认禁止出网，只允许访问声明过的服务，比如对象存储或内部 API。

依赖和系统调用还要控制。可以用容器镜像、seccomp、AppArmor 或 gVisor/Firecracker 这类更强隔离。LogServe 当前没有做到这一步，所以面试时要承认它是生产化技术债。

## Q276. 如果任务有 stdout/stderr，需要如何收集和展示？

当前 Python runner 的 stdout 被用作 JSON line 协议：每个任务执行完成后，runner 从 stdout 返回一行 JSON。因此用户函数随便 print 到 stdout 会破坏协议。stderr 目前被收集到 locked buffer，runner 出错时会作为错误信息的一部分返回。

更稳的设计是把协议通道和用户日志通道分开。比如 runner 的 stdout 只保留协议，用户 stdout/stderr 重定向到单独文件或日志 collector。

日志事件可以写成 `TaskLogAppended`，但不应该把大段日志全塞进 shared log。更好的方式是写 object store 或 log sink，task stream 里只放 log ref、offset、size。

dashboard 展示时按 task id 拉取日志片段，支持 tail、搜索和下载。还要做大小限制，防止一个任务打爆日志存储。

## Q277. 如果任务依赖第三方 Python 包，依赖环境如何管理？

当前 runner 使用配置里的 `PythonPath` 和 `ExecutorPath`，本质上依赖 worker 机器上的 Python 环境。适合单机实验，但不适合多用户、多依赖组合。

更成熟的方式是给任务指定 runtime environment。比如 image、requirements lock、conda env、uv/venv lockfile，或者预构建 wheel cache。

worker 启动任务前，根据 runtime key 找本地环境。环境不存在就拉取或构建，并把构建结果缓存。TaskSpec 里不应该塞一堆临时安装脚本，而应该引用一个可复现的环境描述。

还要考虑安全。动态安装第三方包可能执行任意 setup code。生产化应使用可信镜像、私有包仓库、依赖扫描和只读环境。

## Q278. 如果多个任务共享同一个 Python runner，状态泄漏如何避免？

共享 runner 最大的问题是进程级状态会复用。Python 的全局变量、模块缓存、随机种子、工作目录、环境变量都可能影响下一次任务。

当前实现通过多个长期 runner 提高效率，但没有彻底隔离每个普通 task 的进程状态。timeout 后会重启 runner，这是处理异常污染；正常任务之间仍可能共享状态。

避免状态泄漏有几种办法。

最强的是每个 task 一个独立进程或容器，执行完销毁。隔离最好，但开销大。

折中方案是 runner pool 按 function/runtime key 隔离，执行一定任务数后 recycle。还可以在 runner 内执行前后清理 `sys.modules`、cwd、env，但这很难完全正确。

面试时我会说：当前设计偏向实验性能和实现简单；生产化应引入 runner recycling 或 per-task sandbox。

## Q279. worker 进程重启后本地 model cache 如何恢复？普通 task 状态如何恢复？

model cache 是本地状态，worker 重启后会重新创建 `modelCache`，扫描或初始化配置中的 cache dir 和 checkpoint 信息。然后在 RegisterWorker 和 heartbeat 里上报当前 cached models。control 用这些信息做 LLM locality 调度。

如果缓存文件还在，worker 可以继续把它当 warm cache 用。如果本地磁盘被清空，control 下次 heartbeat 会看到 cached models 变化，后续 LLM task 就可能触发 cold start 或 checkpoint fetch。

普通 task 状态不靠 worker 本地恢复。worker 挂了以后，control 里 running task 会在 redelivery timeout 后重新入队；control 重启时也能从 task stream bootstrap。worker 本地内存里的 in-flight 计数、队列内容丢了没关系，最多导致任务重跑。

这就是两类状态的区别：model cache 是性能优化状态，丢了影响冷启动；task 状态是 runtime 正确性状态，要靠 shared log 和 control metadata 恢复。

## Q280. backpressure 只看 queue depth 和 log append latency 是否足够？

不够，只能算一个最小版本。

queue depth 能看到 control plane 积压，log append latency 能看到 shared log 写入变慢。这两个信号都很关键，因为 LogServe 的所有状态推进都依赖 log，队列积压也直接影响端到端延迟。

但它们看不到 worker 本地情况。比如 control queue 很低，因为任务都被 worker poll 走了；但 worker 本地 LLM queue 堆满，`local_queue_wait_ms` 很高。只看 control queue 就会误判系统很健康。

它们也看不到内存、对象存储、Python runner、gRPC、模型 checkpoint 源等瓶颈。所以当前 backpressure 适合单机实验和保护入口，不能说明生产级 admission control 已经完整。

## Q281. 还应该加入哪些 backpressure 信号，比如 executor queue、memory、object store、gRPC latency？

我会至少加六类信号。

第一，worker 本地 executor queue depth，包括 task、LLM、actor 三类队列。第二，`local_queue_wait_ms` 的滑动窗口 p95/p99。第三，worker CPU、内存、磁盘和 Python runner 重启次数。

第四，object store 指标，比如 result store put/get latency、error rate、snapshot 写入失败数。第五，gRPC 指标，比如 PollTask、CompleteTask、AppendLog 的 p95/p99 和错误率。

第六，LLM 专属指标，比如 checkpoint fetch latency、model cache hit rate、eviction count、cold start 数量。

这些信号可以汇总成 admission decision。比如 log 慢时拒绝新任务；某个 worker 本地 LLM queue 高时不再分配 LLM task；object store 慢时限制大结果 workflow。

## Q282. 调度器如何避免把所有任务打到同一个 worker？

首先要准确维护 worker 的 RunningTasks 和 capacity。LLM 调度里的 `workerHasCapacity` 会过滤掉已满 worker，resource-only 策略也会用 available capacity 和 running count 计算 score。

其次，调度器不能只看 cache hit。locality-aware 会偏向有模型缓存的 worker，但如果所有请求都打到同一个 cached worker，它的 queue delay 会上升。predicted-latency 策略就应该把 EWMA latency、queue penalty、cold start penalty 一起算进去。

普通 task 当前更多依赖 worker 自己按 capacity poll。谁有空，谁来拉任务。这个方式简单，但不够精细。如果多个 worker poll 频率不同，或者某个 worker 网络更快，它可能拿到更多任务。

后续可以加入公平调度：按 worker load 做轮转，按 tenant 做配额，按任务类型做 lane，或者让 PollTask 返回前考虑 worker 最近分配数。

## Q283. 如果 control 与 worker 网络分区，系统如何表现？

如果 worker 连不上 control，它无法 heartbeat、无法 poll 新任务，也无法调用 StartTask 或 CompleteTask。

已经 poll 到本地的任务可能还在执行。执行完成后，如果网络还没恢复，worker 写 log 或 CompleteTask 也可能失败。当前 worker 会把错误记下来，任务不会在 control metadata 中完成。

control 侧看不到 worker heartbeat 后，会逐渐认为 worker 不活跃。running task 到 redelivery timeout 后，会被写 `TaskRedelivered` 并重新入队。新 worker 可以接手执行。

如果网络恢复，旧 worker 可能尝试提交旧结果。lease epoch 会挡住 stale completion。这个情况下可能发生重复执行，但最终状态应该由当前 lease 决定。

## Q284. 如果 worker poll 到任务但还没 Dispatch 就崩溃，任务如何重新投递？

任务在 `PollTask` 返回前已经被 `LeaseTask` 改成 running，并绑定到这个 worker。也就是说，worker poll 到任务后即使还没 Dispatch，control 也已经认为任务在运行。

如果 worker 立刻崩溃，这个 task 不会马上回到队列。control 要等下一次 `redeliverExpiredTasks` 扫描到它，并且 `UpdatedAtMs` 超过 redelivery timeout。

到期后，control 先写 `TaskRedelivered`，再把任务状态改回 queued，清空 worker_id，放回 queue。后续其他 worker 可以 poll 到它。

这个边界说明 redelivery timeout 不只是处理“执行中崩溃”，也处理“拿到 lease 但还没真正开始”的崩溃。更好的设计是 worker poll 后尽快 Dispatch，或者支持 worker 在 graceful shutdown 时释放未开始任务。

## Q285. 如果 worker 已 Dispatch 但 executor 未开始执行，local_queue_wait_ms 如何反映排队？

`Dispatch` 时会记录 `enqueuedAt`。job 进入本地队列后，可能要等 executor goroutine 有空才开始执行。

真正执行 `executeTask` 时，worker 用 `time.Since(enqueuedAt)` 计算 `local_queue_wait_ms`。然后在 `TaskStarted` 事件里写入这个值。

所以如果任务已经 Dispatch 但 executor 还没开始，等待时间会体现在 `local_queue_wait_ms` 里。这个指标越高，说明本地 executor pool 越忙，或者某类队列有 head-of-line blocking。

如果 worker 在 Dispatch 后、executor 开始前崩溃，就不会写 `TaskStarted`，因此也没有 `local_queue_wait_ms`。这类任务只能靠 redelivery timeout 回到队列。
