# 四、Task Scheduling、Worker、Lease、Redelivery 与 Backpressure（简单）

这一组问题主要看 worker 和 control plane 的协作。回答时抓住一条线：control 维护队列、租约和 metadata view；worker 主动 poll，拿到 lease 后本地执行；结果写回时用 worker_id 和 task_lease_epoch 做校验；队列和日志层压力过高时，control 会拒绝新的提交。

## Q231. worker 的注册流程是什么？

worker 启动后先连接 control plane 和 logd。连接成功后，它会创建本地 model cache 视图，然后调用 `RegisterWorker`。

注册请求里包含 `worker_id`、可选 `address`、labels、已缓存模型列表和 capacity。control 收到后先写 `system:workers` stream 的 `WorkerRegistered` 事件，append 成功后才把 worker 写入 metadata view。

metadata 里会保存 worker 的 capacity、cached models、last heartbeat、running tasks 等状态。capacity 如果没传，内存 metadata 会按 1 处理，避免 worker 没有可用容量。

注册完成后，worker 启动本地 executor pool，然后进入周期循环：发 heartbeat，按本地 capacity 主动 poll 任务，任务完成后回收 in-flight 计数。

## Q232. worker 心跳上报了哪些信息？

当前心跳请求比较轻，只上报两类信息：`worker_id` 和 `cached_models`。

`worker_id` 用来刷新 worker 的 `LastHeartbeat`。control 通过这个时间判断 worker 是否还活着，LLM scheduler 和 actor ownership 也会参考它。

`cached_models` 用来告诉 control 这个 worker 当前有哪些模型缓存。LLM locality-aware 调度会用它判断是否应该优先把某个模型请求分配给这个 worker。

注意，心跳没有每次写 shared log。它是高频租约视图，主要更新 metadata。worker 注册会写 `WorkerRegistered`，但 heartbeat 本身不作为完整事件历史保存。这是有意的取舍：心跳太频繁，全写日志会把 shared log 变成监控时序库。

## Q233. worker 如何从 control plane 获取任务？

worker 使用 pull 模式。每个 poll 周期先发 heartbeat，然后在本地 in-flight 数小于 capacity 时循环调用 `PollTask(worker_id)`。

control 收到 `PollTask` 后，会先尝试 redelivery 过期 running task。然后它拿队列锁，从 queue 里按顺序找可分配任务。

不是队列里第一个任务就一定会给这个 worker。control 会检查 task spec 是否存在、metadata 里是否有这条 task、actor mailbox 是否 ready、这个 worker 是否符合任务约束。LLM task 还要经过资源或 locality 策略判断。

找到可分配任务后，control 调用 `LeaseTask`。这个动作会把 task 状态改成 `RUNNING`，设置 `worker_id`，并递增 `task_lease_epoch`。然后 control 把带 lease epoch 的 `TaskSpec` 返回给 worker。

## Q234. worker 本地 executor pool 分成几类？

worker 本地 executor pool 分三类：普通 task pool、LLM pool、actor pool。

普通 task pool 使用 Python runner 执行用户函数。actor pool 也使用 Python runner，但会额外按 actor id 加锁，保证同一个 actor 的命令串行执行。LLM pool 不需要 Python runner，它走 mock LLM 或 vLLM adapter，并配合 model cache manager。

配置上对应 `TaskPoolSize`、`LLMPoolSize`、`ActorPoolSize`。如果配置小于等于 0，代码会用 1 作为 fallback。队列容量则按 worker capacity 设置，capacity 也至少按 1 处理。

这个设计解决了之前单 runner 串行执行的问题：普通 task 和 LLM task 可以并行，actor task 可以并行处理不同 actor，但同一个 actor 仍然保持顺序。

## Q235. 普通 task、LLM task、actor task 分别进入哪个队列？

worker 收到 `TaskSpec` 后，由 `localExecutorPool.Dispatch` 判断进入哪个本地队列。

如果 `TaskSpec.llm_model_name` 不为空，进入 LLM queue。它由 LLM pool 执行，负责模型加载、checkpoint cache、mock/vLLM 调用和 LLM event log。

如果不是 LLM task，但 `actor_id` 不为空，进入 actor queue。actor queue 由 actor pool 执行，执行前会拿 per-actor lock。

其他任务进入普通 task queue。普通 task queue 由 Python runner 执行用户的 `function_source + function_name + args_json`。

这个分类很直接：先看是不是 LLM，再看是不是 actor，剩下就是普通 task。

## Q236. local_queue_wait_ms 用来衡量什么？

`local_queue_wait_ms` 衡量任务在 worker 本地队列里等了多久。

任务从 control poll 回来后，不一定马上执行。如果 worker 本地 pool 已经忙，任务会先进入本地 queue。`Dispatch` 时记录 `enqueuedAt`，真正执行 `executeTask` 时用当前时间减去 `enqueuedAt`，得到 `local_queue_wait_ms`。

worker 写 `TaskStarted` 事件时会把这个指标放进 payload。它反映的是 worker 内部排队，而不是 control plane 全局队列等待。

这个指标很有用。全局 queue depth 低但 `local_queue_wait_ms` 高，说明任务已经被拉到 worker 本地，但 executor pool 不够用。反过来，本地等待很低但全局 backlog 高，瓶颈可能在调度或 worker 数量。

## Q237. task lease epoch 是什么？

`task_lease_epoch` 是任务租约编号。每次 control 把一个 task lease 给 worker，metadata 里的 `TaskLeaseEpoch` 都会递增。

worker 拿到任务时，`TaskSpec` 里会带这个 epoch。后面 worker 调用 `StartTask` 和 `CompleteTask` 都必须带回同一个 epoch。

它解决的是旧 worker 写回的问题。比如 worker-1 拿到 epoch=1 后卡住，control 过一段时间 redelivery，把任务重新排队，worker-2 拿到 epoch=2。此时 worker-1 如果又回来提交完成，control 可以发现它的 lease epoch 旧了，拒绝这次写回。

所以它不是全局时钟，也不是日志 seq。它只是某个 task 的租约代数。

## Q238. 为什么需要 StartTask？

`StartTask` 是 worker 表示“我已经开始执行这个 lease”的 RPC。它不是任务分配本身，分配发生在 `PollTask` 返回之前。

它有两个作用。

第一，校验租约。control 会用 `task_id + worker_id + task_lease_epoch` 调用 `ValidateTaskLease`，确认这个 worker 仍然持有当前租约。

第二，推进上层状态。普通 task 可能不需要额外动作，但 workflow task 开始执行时，control 会把对应 workflow step 标记为 started，并记录 started 时间。actor task 则主要依赖 actor stream 和 mailbox 逻辑，`StartTask` 对 actor 会直接接受。

worker 侧还有一个重要顺序：先向 task stream 写 `TaskStarted`，再调用 control 的 `StartTask`。这延续了 log-first 语义。

## Q239. 为什么 CompleteTask 需要 worker_id 和 lease_epoch？

因为完成结果不能只看 task_id。task_id 只能说明“这是哪个任务”，不能说明“谁现在有资格完成它”。

`worker_id` 用来确认提交结果的 worker 是当前持有者。`lease_epoch` 用来确认它持有的是当前这次租约，而不是过期租约。

这两个字段一起解决 redelivery 的竞争。旧 worker 即使执行出了结果，只要它的 worker id 或 lease epoch 和 metadata 当前状态不匹配，control 就会拒绝它，避免旧结果覆盖新结果。

对 actor task 还要再看 actor epoch。actor ownership 变更后，旧 owner 不能继续写 actor state。task lease 管任务执行权，actor epoch 管 actor 所有权，二者解决的问题不一样。

## Q240. worker 挂掉后任务如何 redelivery？

control 在 `PollTask` 时会调用 `redeliverExpiredTasks`。它扫描 metadata 中处于 `RUNNING` 的任务，如果 `UpdatedAtMs` 距离现在超过 `redelivery_timeout_ms`，就认为这次租约过期。

对每个过期任务，control 先写 `TaskRedelivered` 事件到对应 `task:<task_id>` stream。append 成功后，metadata 才把任务状态从 `RUNNING` 改回 `QUEUED`，清空 worker_id，再把 task id 放回 queue。

如果这个任务之前占用了某个 worker 的 RunningTasks，redelivery 时也会减少该 worker 的 load。

恢复后的任务会被后续 worker poll 到。旧 worker 如果只是网络慢、后来又提交结果，`CompleteTask` 会用 lease epoch 检查，把 stale completion 挡掉。

## Q241. MaxTasks 配置有什么用途？

`MaxTasks` 是 worker 侧的运行上限，主要用于实验、测试和受控执行。

worker 有两个计数：`dispatchedTasks` 和 `completedTasks`。如果 `MaxTasks > 0`，worker dispatch 到上限后就不再继续 poll 新任务；完成到上限后会退出。

这个配置在 benchmark 和故障注入里很方便。比如只让某个 worker 执行固定数量任务，然后退出，观察 control 是否能 redelivery 剩余任务。

它不是全局限流，也不是用户级配额。全局限流应该放在 control 的 backpressure 或调度策略里。

## Q242. capacity 和 task_pool_size 的关系是什么？

capacity 是 worker 对 control plane 声明的总体并发能力。control 和 worker 主循环会用它限制 in-flight 任务数量。比如 capacity=4，worker 最多同时从 control 拉 4 个任务放到本地执行中。

`task_pool_size` 是普通 Python task 的本地执行 goroutine/runner 数量。它只影响普通 task queue 的实际执行并发。

两者不是同一个概念。capacity 管“这个 worker 总共接多少任务”，task_pool_size 管“普通 task 有多少执行槽”。LLM 和 actor 还有自己的 `llm_pool_size`、`actor_pool_size`。

配置时要避免明显不匹配。比如 capacity 很大但各类 pool 都是 1，本地 queue wait 会升高；pool 很大但 capacity 很小，executor 资源又用不上。实验里通常会一起调这两个值，看 throughput 和 p95 latency 的变化。

## Q243. 为什么 actor pool 还需要 per-actor lock？

actor pool 可以有多个 runner。如果只靠 actor queue，多 goroutine 从同一个 queue 取任务时，可能同时执行同一个 actor 的两个 command。

这对 actor 是不允许的。actor 的核心语义是同一个 actor 内部状态串行修改。`Counter.inc()` 这类操作如果并发执行，就会出现读旧值、覆盖写、command_seq 乱序等问题。

所以 worker 在 actor pool 里加了 per-actor lock。执行 actor task 前，按 actor id 拿锁；执行完成后释放。这样不同 actor 可以并行，同一个 actor 仍然串行。

control 侧也有 mailbox 顺序检查，要求 command_seq 必须等于当前 command_count+1。worker 侧的 per-actor lock 是第二道保护，防止本地并发破坏 actor 状态。

## Q244. worker 为什么使用 Python runner 执行普通 task？

因为 LogServe 的 SDK 和用户函数主要面向 Python。用户通过 Python SDK 定义 task、workflow、actor，TaskSpec 里保存 `function_source`、`function_name` 和 `args_json`。worker 用 Python runner 执行这些函数，能直接复用 Python 生态和示例代码。

Go 后端适合做 control plane、logd、调度、状态机和并发控制。这些部分需要稳定的网络服务、明确的内存模型和较好的并发性能。

Python runner 则负责运行用户代码。它通过 stdin/stdout 的 JSON line 协议和 Go worker 通信，普通 task 返回 result，actor task 还会返回新的 state。

这个边界比较实用：Go 管 runtime，Python 管用户函数。缺点也明显，用户代码隔离还不够强，生产化需要更严格的 sandbox、资源限制和依赖环境管理。

## Q245. 任务 timeout 发生后 worker 如何处理 executor？

worker 会根据 `TaskSpec.timeout_ms` 创建带超时的 context。执行函数、actor 方法或 LLM 调用时都用这个 context。

如果执行超过 timeout，context 会变成 deadline exceeded。worker 把任务状态设为 `FAILED`，错误文本类似 `task timed out after <timeout_ms>ms`，然后写 `TaskFailed` 事件，再调用 `CompleteTask`。

对普通 Python task 和 actor task，如果 timeout 导致 Python runner 被杀掉，worker 会尝试 `Restart` 这个 runner。`pythonRunner.Execute` 在 context 取消时会 kill 当前 Python 进程，`Restart` 会重新启动一个干净的 runner。

LLM task 不走 Python runner，所以不会重启 runner。它主要依赖 context 取消打断 mock sleep、checkpoint fetch 或 vLLM 调用。

## Q246. 任务失败后 control plane 如何记录状态？

worker 先写 `TaskFailed` 事件到 task stream，payload 里有 task_id、worker_id、status、task_lease_epoch、timestamp 和 error。append 成功后，worker 调用 `CompleteTask`，status 传 `FAILED`。

control 收到 `CompleteTask` 后，先检查状态必须是 `SUCCEEDED` 或 `FAILED`。然后 metadata store 校验 worker_id 和 task_lease_epoch。校验通过后，把 task 状态更新为 `FAILED`，保存 error，更新时间戳，并减少 worker 的 RunningTasks。

如果这是 workflow step，control 还会推进 workflow 状态：step 标记 failed，必要时触发 retry；如果达到 retry 上限，workflow 会进入 failed。actor task 失败时，会走 actor command failed 逻辑，把失败也写入 actor stream。

重复失败提交不会反复改变 terminal task。metadata 的 `CompleteTask` 对已 terminal 的任务会返回已有状态，避免重复写最终状态。

## Q247. 任务成功后结果放在哪里？

普通 task 的结果通过 `CompleteTask.result_json` 写回，metadata task row 会保存 `ResultJSON`，`GetTaskStatus` 可以直接返回。

worker 在调用 `CompleteTask` 前，也会先把 `TaskCompleted` 写入 task stream。payload 里如果有结果，会带 `result_json`。这让 task stream 自己可以解释任务从开始到完成的历史。

workflow step 的大结果有额外处理。control 在完成 workflow step 时会判断 inline threshold。小结果可以直接保存在 workflow step 的 `result_json`；大结果会写 result store，本地开发是 `local://`，部署时可以接 S3-compatible MinIO，然后 workflow log 和 metadata 里保存 `result_ref`。

所以不能简单说“所有结果都进日志”或“所有结果都进 MinIO”。当前实现是：普通 task result 直接在 task status 中可见；workflow 大结果和 actor snapshot 走 result store，日志保留引用。

## Q248. backpressure 在提交任务时如何触发？

backpressure 在 `enqueueTaskWithMetadata` 里触发，也就是任务真正入队前触发。

第一类信号是 log append 慢。control 记录最近一次 append log 的耗时 `lastLogAppendMs`。如果配置了 `log_append_slow_ms`，并且最近 append 耗时超过阈值，新的 task 提交会被拒绝。

第二类信号是 queue backlog。control 会看当前 queue 长度，如果 `queue_high_watermark` 大于 0 且 queue 长度已经达到或超过阈值，就拒绝新任务。

有一个细节：幂等重复请求会先查已有 task。如果同一个 idempotency key 已经提交过，control 会返回已有 task，而不是因为 backpressure 拒绝这次重试。这对客户端重试很重要。

## Q249. queue backlog 高时为什么要拒绝新任务？

因为 queue backlog 高说明系统接收任务的速度已经超过处理速度。继续接新任务只会让排队时间变长，最后 p95/p99 latency 失控。

对 LogServe 这种 log-first runtime，backlog 高还会放大恢复成本。队列里积压越多，控制面重启、bootstrap、redelivery 后要处理的 pending task 就越多。workflow 和 actor 还会出现连锁影响：一个 step 或 command 卡住，后面的步骤也会跟着等。

拒绝新任务看起来不友好，但它让系统保持可恢复。客户端可以用幂等键重试，或者等 dashboard 上 queue depth 降下来再提交。

更成熟的做法不是一刀切拒绝，而是按 tenant、任务类型、优先级做 admission control。当前实现用 queue high watermark，是单机实验环境下足够清楚的保护线。

## Q250. worker poll 模式相比 push 模式有什么好处？

poll 模式的好处是控制面更简单。control 不需要维护到每个 worker 的长连接，也不用主动推送任务。worker 自己按 capacity 拉任务，断线后自然停止 poll。

第二个好处是 backpressure 更自然。worker 忙时 in-flight 达到 capacity，就不会继续 poll。control 看到的就是“没有 worker 来拿任务”，队列会增长，然后触发 backpressure 或 redelivery。

第三个好处是故障处理简单。worker 挂掉后不会再 poll，之前拿走的 running task 过了 redelivery timeout 会重新入队。control 不需要在推送连接断开的一瞬间判断任务是否真的丢了。

代价是有轮询延迟。poll interval 太大，任务启动会慢；太小，control RPC 压力会上升。LogServe 目前用这个方式是合适的，因为它让单机实验和多 worker demo 更稳定，也更容易解释 lease/redelivery 语义。
