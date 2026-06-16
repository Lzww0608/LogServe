# 四、Task Scheduling、Worker、Lease、Redelivery 与 Backpressure（拓展）

这一组更像系统设计追问。回答时可以从 LogServe 当前实现出发，再自然扩展到常见任务队列、Kubernetes、GPU 调度、日志采集、追踪和自动扩缩容。重点不是把所有功能都说成已经实现，而是讲清楚如果继续往生产化走，哪些协议和指标必须补上。

## Q286. 任务队列系统中 visibility timeout 和 LogServe lease epoch 有什么关系？

visibility timeout 是消息队列里的常见概念。worker 取走一条消息后，这条消息会在一段时间内对其他 worker 不可见。如果 worker 在超时前 ack，消息就完成；如果没有 ack，消息重新变得可见，被再次投递。

LogServe 的 redelivery timeout 扮演了类似角色。task 被 `PollTask` lease 给某个 worker 后，metadata 状态变成 `RUNNING`，不会再被其他 worker 拿到。超过 redelivery timeout 后，control 写 `TaskRedelivered`，把 task 改回 `QUEUED`，重新放回队列。

区别在于 LogServe 多了 `task_lease_epoch`。visibility timeout 只说明“这条消息重新可见了”，但不一定能区分旧 worker 的迟到 ack。LogServe 每次 lease 都递增 epoch，CompleteTask 必须带回当前 epoch。旧 worker 即使执行完成，也会因为 epoch 过期被拒绝。

所以可以这样类比：redelivery timeout 类似 visibility timeout，lease epoch 类似一次投递的 fencing token。前者决定何时重投，后者决定谁的完成结果有效。

## Q287. SQS、Celery、Temporal 对任务 redelivery 的处理和你的实现有什么不同？

SQS 更偏消息队列。消费者收到消息后，在 visibility timeout 内处理并删除消息。如果没删除，消息会重新可见。它主要管理消息投递，不理解 workflow step、actor ownership、task result 去重这些上层语义。

Celery 更偏分布式任务队列。broker 负责投递，worker 执行 Python task，结果可以写 backend。它支持 ack、retry、eta 等机制，但状态恢复通常不是从一条统一 shared log 重建 runtime，而是依赖 broker、result backend 和任务本身的幂等性。

Temporal 更像 durable workflow runtime。它把 workflow history 作为事实源，activity 可以重试，workflow replay 决定恢复后的状态。它的语义比普通队列更强，也更成熟，开发模型是 workflow/activity 分离。

LogServe 的实现更小，但主线接近 event-sourced runtime：task、workflow、actor、LLM 事件写入 shared log，metadata view 可以重建。redelivery 不只是消息重新投递，还要和 task lease epoch、workflow step、actor command_seq、LLM stats 这些状态配合。

一句话概括：SQS 关心消息可见性，Celery 关心任务分发，Temporal 关心 durable workflow history，LogServe 用 shared log 把任务、workflow、actor 和 LLM 调度放到同一条恢复主线上。

## Q288. at-least-once、at-most-once、effectively-once 在任务系统中分别如何实现？

at-least-once 的意思是任务至少执行一次。实现上通常是：worker 拿到任务后，如果没在规定时间内确认完成，系统会重新投递。LogServe 当前就是这个方向。worker 可能崩溃，任务会 redelivery；旧 worker 和新 worker 甚至可能都执行过，但最终结果提交由 lease epoch 控制。

at-most-once 的意思是任务最多执行一次。实现上可以在投递后立即从队列删除，或者不做 redelivery。这样重复少，但 worker 崩溃时任务可能直接丢失。LogServe 不选择这条路，因为它要支持失败恢复。

effectively-once 更现实。它承认底层可能至少执行一次，但通过幂等键、状态机、结果去重和 fencing，让系统对外表现得像只提交一次有效结果。LogServe 的 workflow step 结果就是这个思路：`workflow_id + step_id + input_hash` 去重，task 完成还要校验 worker_id 和 lease epoch。

面试时我会强调：不要轻易说 strict exactly-once。只要用户代码可能访问外部系统，runtime 就不能保证外部副作用只发生一次。能保证的是最终状态提交层的去重和可恢复。

## Q289. 如果任务执行有副作用，如何设计 idempotent task API？

任务有副作用时，API 里必须显式带幂等键。比如扣款任务要带 `payment_id`，发邮件任务要带 `message_id`，写外部对象要带 content hash 或 object key。

用户函数不应该每次重试都生成新的外部请求 id。它应该把 LogServe 的 task id、workflow id、step id 或业务幂等键传给外部系统。外部系统看到同一个 key，就返回已有结果，而不是重复扣款或重复发送。

还要把副作用和结果提交拆开。任务可以先尝试外部操作，再把外部操作返回的 transaction id 写入结果。重试时先查询这个 transaction id 或 idempotency key 是否已经完成。

更稳的做法是 outbox 或 saga。任务先写本地 outbox，后续由专门 publisher 执行外部副作用；或者每个副作用都有补偿动作。LogServe 本身能保护 task final result，不会自动保护外部世界。

## Q290. 如果需要取消任务，control 和 worker 需要什么协议？

取消不能只改 metadata 状态。worker 可能已经拿到 lease 并开始执行，所以 control 和 worker 之间需要显式取消协议。

control 侧要有 `CancelTask`。它先写 `TaskCancelRequested` 到 task stream，再把 task 标记为 canceling。如果任务还在 queued，可以直接从 queue 中移除并写 terminal cancel 事件。

如果任务已经 running，control 要通知持有 lease 的 worker。可以用 worker poll 时顺带拉 cancel 指令，也可以开一个 worker control stream。worker 收到后取消对应 task context，尝试中断 executor。

worker 完成取消后，再写 `TaskCanceled` 或 `TaskFailed`，并调用 control 完成状态推进。CompleteTask 仍然要带 worker_id 和 lease epoch，避免旧 worker 取消错任务。

当前 LogServe 已有 timeout，但没有完整用户取消协议。timeout 是 worker 本地根据 TaskSpec 执行；取消则需要 control 主动发出请求，并可观察地写入日志。

## Q291. 如果需要优先级队列，queue 数据结构如何变化？

当前 queue 是简单的 task id 列表，`PollTask` 按顺序扫描。要支持优先级，不能只用 FIFO。

最直接的做法是 priority heap。每个 task 有 priority、created_at、deadline、tenant 等字段，排序键可以是 `(priority desc, deadline asc, created_at asc)`。PollTask 每次从 heap 里取最合适的任务。

但有 actor 和 target worker 后，事情会复杂。某个高优先级 actor command 如果 mailbox 没 ready，不能一直挡住后面所有任务。需要支持“跳过暂不可调度任务”，或者按队列分桶。

更稳的设计是多级队列：高、中、低优先级各一组队列，PollTask 按权重取任务。actor、LLM、普通 task 也可以分 lane，避免高优先级但不可执行的任务造成 head-of-line blocking。

日志上也要保存 priority，否则重启后无法重建相同调度意图。TaskSubmitted payload 里应该有 priority 字段。

## Q292. 如果需要公平调度，多租户队列如何设计？

多租户公平调度的核心是不要让一个 tenant 把队列和 worker 都占满。

数据结构上可以按 tenant 分队列。每个 tenant 有自己的 pending queue、running count、rate limit 和权重。全局调度器用 weighted round-robin、deficit round-robin 或 token bucket 在 tenant 之间选择。

任务进入队列前先做 admission control。某个 tenant 超过 pending 上限或 running 上限，就拒绝或延迟这个 tenant 的新任务，而不是影响所有人。

worker 分配时也要考虑资源隔离。GPU、模型 cache、本地 SSD 这类资源可能按 tenant 配额划分。LLM task 不能因为某个 tenant 连续命中 cache，就一直抢占同一个 worker。

LogServe 当前是全局 queue，更适合单租户实验。扩展成多租户后，stream id、metadata row、idempotency key、dashboard 查询都要带 tenant id。

## Q293. 如果需要 rate limit，应该放在 SDK、control 还是 worker？

SDK 可以做轻量限流，但不能只放 SDK。用户可以绕过 SDK 直接调用 gRPC，多个客户端也无法靠本地 SDK 协调全局限额。

control 是最重要的位置。所有任务提交都经过 control，它能按 tenant、API key、task type、model name 做全局 rate limit。超过限制时，在任务入队前拒绝，避免污染 queue 和 log。

worker 也需要限流，但它解决的是资源保护。比如某个 worker 的 LLM pool 已满、GPU 内存紧张、Python runner 重启频繁，就应该少拉或不拉任务。

所以完整答案是三层：SDK 做用户体验和早期退避，control 做权威限流和配额，worker 做本地资源保护。真正的安全边界在 control，不在 SDK。

## Q294. 如果需要 task affinity，调度器需要哪些标签和约束？

task affinity 是让某些任务更倾向或必须跑在某些 worker 上。

worker 需要上报标签，比如 region、zone、GPU type、CPU arch、本地 SSD、数据集位置、模型 cache、Python runtime、可访问的私有网络。当前 RegisterWorker 已经有 labels 和 cached models，是一个起点。

TaskSpec 需要表达约束。可以分成 hard constraint 和 soft preference。hard constraint 比如必须有 `gpu=a100`、必须在 `region=cn-east`。soft preference 比如优先有某个数据集缓存，或者优先和上一个 step 同 worker。

调度时先过滤 hard constraint，再对 soft preference 打分。LLM locality-aware 本质上就是一种 affinity：模型缓存是 worker 属性，模型请求对这个属性有偏好。

还要注意 anti-affinity。比如同一个 workflow 的多个副本不要放在同一节点，避免节点故障一起失败。这些都需要 scheduler decision record，方便解释为什么选了某个 worker。

## Q295. 如果 worker 是 Kubernetes Pod，worker identity 如何稳定？

不能简单用 Pod IP。Pod 重启后 IP 可能变，原来的 IP 也可能被别的 Pod 复用。

有几种做法。StatefulSet 可以用稳定 ordinal，比如 `worker-0`、`worker-1`。如果是 Deployment，可以在 worker 启动时生成 worker instance id，并把它和 Pod UID 一起注册。

更稳的是区分 logical worker id 和 worker incarnation id。logical id 表示一个工作槽位，incarnation id 表示这次进程实例。任务 lease 里最好带 incarnation 或 epoch，防止旧 Pod 恢复后冒充新 Pod。

LogServe 当前主要用 `worker_id`。如果放到 Kubernetes 上，建议把 worker_id 设置成稳定但不会冲突的值，并增加 worker epoch。Pod 重启后 epoch 变大，旧进程即使还活着，也不能提交旧结果。

## Q296. 如果 Pod 被杀，task lease epoch 如何防止旧 completion？

Pod 被杀后，最常见情况是进程直接没了，不会再提交 completion。control 等 redelivery timeout，把 task 重新入队，新 Pod 或其他 worker 拿到新的 lease，TaskLeaseEpoch 增加。

如果出现更复杂的情况，比如旧 Pod 网络卡住后恢复，或者 preStop 没完成但进程还短暂运行，它可能尝试提交旧 completion。CompleteTask 会带旧的 task_lease_epoch。

control 看到 metadata 中的 TaskLeaseEpoch 已经变成新值，就拒绝旧 completion。即使旧 worker 写了旧 epoch 的 TaskCompleted 事件，replay 也应该按 epoch 过滤，不让它覆盖新 lease 的结果。

这就是 lease epoch 的价值：Kubernetes 的 Pod 生命周期不一定干净，runtime 不能假设旧执行者一定安静退出。

## Q297. 如果 worker 所在节点磁盘缓存丢失，LLM scheduler 的缓存状态如何更新？

worker 重启后会重新构造本地 model cache 视图。如果缓存目录已经被清空，它上报的 cached models 就会变少。

control 通过 RegisterWorker 和 heartbeat 接收 cached models。下一次 heartbeat 后，metadata 里的 worker cache 状态会更新。locality-aware scheduler 再调度同一个模型时，就不会继续把这个 worker 当 warm cache 命中。

如果缓存是在 worker 运行中被外部清理，也需要 worker 定期重新扫描或在加载失败时主动更新 cache 状态。否则 control 可能短时间以为模型还在，导致一次错误的 locality 选择。

生产化还需要缓存一致性校验。比如 checkpoint manifest、文件大小、hash 都要检查。不能只看文件名存在，否则损坏的缓存会被误认为可用。

## Q298. 如果 worker 长时间 GC pause，heartbeat 超时是否会误判死亡？

会有可能。只要进程在较长时间内无法发送 heartbeat，control 就可能认为 worker 不活跃。

在 LogServe 当前设计里，heartbeat 主要影响 active worker 判断、LLM 调度和 actor ownership。task redelivery 主要看 running task 的 UpdatedAtMs 是否超过 redelivery timeout。长 pause 如果超过这些阈值，就可能触发 owner 转移或任务重投。

误判的后果是重复执行。旧 worker pause 结束后还以为自己持有任务，但 control 可能已经把任务发给新 worker。lease epoch 和 actor epoch 会防止旧结果生效。

缓解办法是阈值留足余量，heartbeat interval 要明显小于 lease timeout；还可以让 worker 上报 monotonic progress，或者把 pause 监控纳入 worker health。对 JVM/Go/Python 这类有停顿风险的进程，都不能把单次 heartbeat 超时当成绝对死亡证明。

## Q299. 如何区分 worker process 挂掉、executor 挂掉、网络分区、control 挂掉？

要看观测点。

worker process 挂掉：control 收不到 heartbeat，worker 日志停止，Pod 或进程状态显示退出。该 worker 持有的 running task 会到 redelivery timeout 后重投。

executor 挂掉：worker 进程还活着，heartbeat 正常，但 Python runner 返回错误、stdout 协议断开，或者 runner restart 指标上升。此时 task 会失败，worker 可以继续处理后续任务。

网络分区：worker 进程和 executor 可能都活着，但 control RPC 或 log RPC 失败。worker 本地日志会出现 heartbeat、poll、AppendLog、CompleteTask 错误；control 侧看到 heartbeat 消失。

control 挂掉：所有 worker 都无法 heartbeat/poll/complete，但 logd 可能还活着。worker 会持续报 control RPC 错误。control 重启后从 log bootstrap，running task 根据租约和 redelivery 继续处理。

所以需要分层指标：进程存活、executor 健康、control RPC、log RPC、heartbeat、新任务 poll、完成提交。只看一个信号很容易误判。

## Q300. 如何设计 worker health check 和 readiness probe？

health check 看进程是否活着，readiness 看 worker 是否应该接任务。

健康检查可以检查 worker 主循环是否运行、goroutine 没有死锁、最近一次 event loop tick 时间正常、Python runner 或 LLM worker 没有全部崩掉。

readiness 要更严格。worker 要能连接 control 和 logd，本地 executor pool 已启动，模型 cache 扫描完成，队列没有严重积压，内存和磁盘在安全范围内。只有 ready 才应该继续 poll task。

在 Kubernetes 里，liveness probe 失败可以重启 Pod；readiness probe 失败只把 Pod 从可调度目标中摘掉。对 LogServe worker 来说，readiness 失败时最好停止 poll，但允许已执行任务完成并提交。

还可以加 draining 状态。worker 进入 draining 后不再 poll 新任务，但继续处理已有 in-flight。这样升级和缩容时重复执行会少很多。

## Q301. 如果任务执行时间分布长尾严重，调度器如何处理 straggler？

第一步是识别 straggler。需要记录每类 task 的执行时间分布、p95/p99、worker 本地 queue wait、executor 类型、输入大小等信息。不能只看平均值。

第二步是避免把长任务和短任务混在同一个 FIFO 队列里。可以按预计耗时分 lane，短任务走短队列，长任务走长队列。这样一个慢任务不会挡住大量短任务。

第三步是动态调度。worker 上某类任务长尾严重时，调度器降低它的 score。LLM predicted-latency 的 EWMA 思路可以扩展到普通 task。

第四步是设置合理 timeout 和 retry。长尾任务如果只是慢，不应过早 redelivery；如果是卡死，就要及时失败或重投。

真正难的是区分“正常长任务”和“异常卡住任务”。这需要任务进度上报或 heartbeat with progress，而不只是一个固定 timeout。

## Q302. 是否需要 speculative execution？引入后如何处理重复 completion？

speculative execution 可以解决长尾问题，但代价很高。它的做法是：某个任务跑得异常慢时，再启动一个副本，让两个 worker 竞争，谁先完成用谁的结果。

LogServe 的 lease epoch 已经能挡住旧 completion，但 speculative execution 需要更细的模型。因为两个执行副本可能都是系统主动发出的，不是一个旧租约、一个新租约那么简单。

可以设计 attempt id。每个 speculative attempt 都有 attempt_id，control 记录当前 accepted attempt。第一个成功的 attempt 写入 final result 后，其他 attempt 的 completion 返回 accepted but ignored，或者明确 rejected。

对有外部副作用的 task，默认不应该 speculative。只有纯计算、幂等读、可重复执行的任务才适合。

所以我的回答是：可以支持，但要先有 attempt-level fencing、结果去重和任务副作用声明。否则 speculative execution 会把重复副作用问题放大。

## Q303. 如何对任务执行链路做 tracing？

需要从 SDK 开始生成 trace id。用户提交 task、workflow 或 actor command 时，SDK 带上 trace id 和 span id。control 在 SubmitTask、AppendLog、入队、PollTask、LeaseTask、StartTask、CompleteTask 各处创建 span。

worker 拿到 TaskSpec 后继续传播 trace context。执行 Python runner 时，把 trace context 放进环境变量或请求 JSON，让用户代码也能上报子 span。

关键 span 包括：SDK submit、log append TaskSubmitted、queue wait、poll wait、local queue wait、TaskStarted、executor run、LLM model load、checkpoint fetch、first token、TaskCompleted、metadata update。

trace id 也应该写进事件 payload 或 metadata。这样 dashboard 看到一个慢 workflow 时，可以直接跳到 trace，看慢在 control queue、worker queue、模型加载还是用户代码。

注意不要把大 payload 放进 tracing。trace 记录 id、时间、状态、错误摘要和资源标签就够了。

## Q304. 如果任务执行日志很大，应该写入 shared log 还是对象存储？

大日志不应该写入 shared log。

shared log 的职责是记录状态变化和可恢复事件。任务 stdout/stderr 如果很大，会拖慢 append、增大 replay 成本，还会让 log compaction 更难做。

更好的做法是写对象存储或专门日志系统。worker 把 stdout/stderr 分块写入 object store、文件日志或 Loki/Elastic 这类系统。task stream 里只写 `TaskLogRefCreated` 或在 TaskCompleted payload 里放 log_ref、size、hash、tail preview。

dashboard 展示时按 ref 拉日志，支持分页和 tail。出于面试角度，我会强调：状态日志和执行日志要分开。状态日志要小而可靠，执行日志可以大，但需要生命周期管理。

## Q305. 如果任务运行需要 GPU/NUMA/本地 SSD，capacity 模型如何扩展？

单一 capacity 不够。要变成多维资源模型。

worker 上报 CPU cores、memory、GPU type、GPU memory、NUMA node、本地 SSD 容量和 I/O、模型 cache、网络带宽等资源。TaskSpec 也要声明 resource requests 和 limits。

调度时不能只判断 RunningTasks < Capacity，而要做资源匹配。比如一个 worker 有 2 张 GPU，每个 LLM task 需要 1 张 GPU 和 20GB 显存；另一个 task 需要本地 SSD 上的数据集。scheduler 要判断资源是否满足，并在 lease 后预留资源。

NUMA 和本地 SSD 更偏 affinity。任务不只是“能不能跑”，还要看跑在哪个节点性能最好。

LogServe 当前 capacity 是简单并发槽位，适合单机实验。扩展 GPU 调度时，worker heartbeat 和 dashboard 都要增加资源快照，PollTask 也要按资源维度过滤。

## Q306. 如何设计 worker 自动扩缩容？

自动扩缩容要先定义信号。常见信号是 queue depth、oldest queued age、任务到达率、worker utilization、local_queue_wait_ms、LLM cold start、SLO violation rate。

扩容时，根据任务类型启动不同 worker。普通 task 多就扩普通 worker；LLM 请求多就扩带模型缓存或 GPU 的 worker；actor 压力大则要考虑 actor ownership 和 mailbox，而不是盲目扩。

缩容更难。worker 缩容前要进入 draining：停止 poll 新任务，等待 in-flight 完成，释放或 redelivery 未开始的任务。不能直接杀，否则会制造大量重复执行。

还要防抖。queue depth 短时间抖动不应该马上扩缩容。可以用滑动窗口和冷却时间。

LogServe 当前提供了 dashboard 指标和 backpressure 基础，后续可以把这些指标接到 Kubernetes HPA/KEDA 或自定义 autoscaler。

## Q307. 如果 executor 是容器而不是长驻 Python 进程，会有什么取舍？

容器 executor 的隔离更好。每个任务有独立文件系统、环境、依赖和资源限制。CPU、内存、网络、挂载都可以控制。用户代码状态泄漏也少。

缺点是冷启动成本高。每个任务启动容器，拉镜像、创建 namespace、挂载 volume 都要时间。短任务会很吃亏。

长驻 Python runner 的优点是快。进程已经启动，依赖已经加载，适合高频小任务。缺点是隔离弱，状态泄漏和安全风险更大。

折中方案是 warm container pool。worker 预先启动一批容器，任务来了直接复用；执行一定次数后销毁。这样比每任务启动容器快，也比长驻单 Python 进程安全。

## Q308. 如果每个任务启动一个容器，冷启动成本如何优化？

第一，镜像预拉取。worker 节点提前拉常用镜像，避免任务到来时才下载。

第二，使用小镜像和分层缓存。把基础依赖放在公共 base image，任务代码或配置放在小层里。

第三，warm pool。提前创建一批空闲容器或 sandbox，任务来了注入参数执行，结束后清理再复用。

第四，按 runtime key 缓存环境。相同依赖的任务共享同一批 warm containers，不要每次重新安装包。

第五，分级执行。短小可信任务可以用长驻 runner，不可信或重资源任务才用容器。不是所有任务都需要最高隔离。

对于 LLM task，还可以预热模型 cache。容器冷启动和模型冷启动叠在一起会很慢，必须分别优化。

## Q309. 如何把 backpressure 与 autoscaling 联动？

backpressure 和 autoscaling 不能各做各的。backpressure 说明系统已经吃紧，autoscaling 决定是否增加处理能力。

一个合理流程是：queue depth、oldest queued age、local_queue_wait_ms 上升时，先触发扩容。如果扩容需要时间，control 同时提高 admission 门槛，限制低优先级或非幂等任务进入。

如果 log append latency 或 object store latency 很高，不能盲目扩 worker。因为更多 worker 会产生更多 completion 和日志写入，可能让瓶颈更严重。这时 backpressure 应该优先保护 log 和对象存储。

扩容成功后，backpressure 阈值可以逐步放松。缩容时反过来：先停止接收低优先级任务，等队列下降，再 drain worker。

所以联动的关键是分清瓶颈在入口、执行层、日志层还是对象存储。不是所有压力都靠加 worker 解决。

## Q310. 如果任务在 worker 本地已经开始但控制面重启，worker 是否应该继续执行？

应该继续执行，但要接受结果可能需要重试提交。

worker 已经拿到 lease 并开始执行时，停止执行反而会增加重复和浪费。control 重启期间，worker 可以继续跑本地 task。等 control 恢复后，worker 再写 terminal event 并调用 CompleteTask。

问题在于 control 重启会从 log bootstrap。它可能把 running task 恢复为 queued，或者 redelivery 给其他 worker。如果原 worker之后提交结果，lease epoch 会决定它是否还有效。

如果原 lease 仍然有效，CompleteTask 会成功。如果任务已经被新 worker lease，旧 worker 的 completion 会被拒绝。这样可能浪费一次执行，但不会覆盖新结果。

更好的协议是 worker 在 control 恢复后先做 lease refresh 或 StartTask retry，确认自己仍持有 lease，再继续执行或提交结果。当前实现偏简单，依赖 CompleteTask 的 lease 校验兜底。
