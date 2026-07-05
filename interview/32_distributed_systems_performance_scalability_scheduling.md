# 十一、分布式系统原理与生产化追问：性能、扩展性与调度

这一组问题主要考察 LogServe 从单机实验走向更大规模时会遇到的瓶颈。回答时要先承认当前实现的边界：control plane 里仍然是单个 `queue []string` 加 `queueMu`，`PollTask` 会扫描队列；workflow 调度有全局 `workflowMu`；logstore 是单 Store 全局锁；LLM 调度已经有 locality-aware 和 predicted-latency，但仍是轻量实现。然后再讲清楚该怎么扩展。

## Q776. 当前控制面单队列如何扩展到多分区队列？

当前实现里，control plane 用一个内存队列保存待调度的 task id。`PollTask` 拿到 `queueMu` 后，从头扫描队列，找到一个适合当前 worker 的任务，再调用 metadata 的 `LeaseTask`。这个设计简单，适合单机实验和小规模 demo，但扩展性会遇到两个问题：一个锁保护所有队列操作，所有 worker poll 都要竞争；队列里混着普通 task、LLM task、actor task，调度条件不同，扫描成本会变高。

扩展的第一步是把队列拆成多个 partition。partition key 可以按几类维度组合：

- `task_type`：普通 task、LLM task、actor task 分开排队。
- `tenant_id`：多租户场景下避免单个租户占满队列。
- `workflow_id` 或 `actor_id`：减少同一对象的跨分区协调。
- `hash(task_id)`：普通无状态任务可以直接哈希分片。

拆完后，每个分区有自己的队列、锁和指标。`PollTask` 不再扫全局队列，而是根据 worker 能力选择一组候选分区。比如普通 worker 只看 CPU task partition，LLM worker 优先看自己缓存模型对应的 partition，actor owner 只看自己拥有的 actor partition。

真正要注意的是语义。队列本身最好仍然只是调度 hint，任务状态以 metadata view 和 shared log 为准。即使某个队列分区丢了内存状态，control 重启后也能从 log 重建 queued task，再重新填充分区队列。这样扩展了吞吐，但不会把 source of truth 从 log 偷偷挪到队列里。

## Q777. task stream 按 task_id 分散是否利于并行 replay？

有利，但不是免费收益。

LogServe 当前把 task 事件写进 `task:<task_id>` stream。这样每个 task 的生命周期是独立的：`TaskSubmitted`、`TaskStarted`、`TaskCompleted` 或 `TaskFailed` 都在同一个 stream 内有序。恢复时，如果要重建大量 task 状态，可以把多个 task stream 分给不同 goroutine 并行 replay。单个 task stream 内保持顺序，stream 之间天然并行。

这个设计的代价是 stream 数量会变多。任务很多时，`ListStreams("task:")` 和大量小 stream 的读取会成为启动成本。每个 stream 可能只有几条事件，但打开、查找、反序列化的固定开销仍然存在。

生产化可以做两层优化。第一层是保留 `task:<task_id>` 的逻辑命名，但底层 logstore 按 segment 或 shard 批量读取，不要真的对每个 task 做一次昂贵的独立扫描。第二层是加 materialized checkpoint，例如记录“截至某个 log position 已经恢复的 task view”。重启时先加载 checkpoint，再 replay tail log。

所以结论是：按 task_id 分散适合并行恢复和隔离单 task 状态，但需要配合 stream 索引、批量读取和 checkpoint，不能只靠 `ListStreams` 扫所有 task。

## Q778. workflow stream 很长时如何加速 ReplayWorkflow？

workflow stream 变长主要出现在大 DAG、反复 retry、fan-out 很多、运行时间很长的场景。当前 `ReplayWorkflow` 是从 `wf:<workflow_id>` 的第 1 条记录开始读，逐条重建 workflow state。这个方法语义清楚，但 stream 越长，恢复越慢。

加速方法可以按成本从低到高推进。

第一种是 workflow checkpoint。每隔一定数量的 step event，或者 workflow 到达某些稳定点时，把完整 workflow state 写到 result store，再在 workflow stream 里写一条 `WorkflowCheckpointCreated(snapshot_ref, compacted_until_seq)`。之后 replay 先加载 checkpoint，再读 checkpoint 后面的 tail log。

第二种是 snapshot-aware retention。checkpoint 已经覆盖的老事件，对普通 replay 来说不再需要从头扫描。log 层可以记录 logical trim point，例如 `TrimStream("wf:<id>", before_seq)`。这样普通 replay 从 trim point 之后读，审计读再走另一套带历史权限的接口。

第三种是给 workflow state 建增量索引。比如按 step_id 记录最近一次 `StepScheduled`、`StepStarted`、`StepSucceeded`。这对查询单个 step 很有用，但不能替代完整 replay，因为 workflow 的最终状态仍然要遵守事件顺序。

我会优先做 checkpoint + tail replay。它和 actor snapshot 的思路一致，代码路径也容易解释：log 还是 source of truth，snapshot 是加速恢复的物化结果。

## Q779. actor 热点如何扩容？

actor 热点是 actor 模型的经典问题。LogServe 的 actor mailbox 保证同一个 actor 内部命令串行执行，`command_seq` 也要求按顺序 apply。这个语义很强，但它也意味着单个 actor 的写吞吐上限基本等于一个串行执行器的吞吐。

如果某个 actor 真的很热，最直接的扩容方式是拆分 actor。比如把一个全局 Counter 拆成 `CounterShard-0` 到 `CounterShard-N`，请求按 key 或 hash 分发。读取总值时再聚合所有 shard。这适合计数、统计、限流这类可拆分状态。

如果状态不能随便拆，就只能优化单 actor 内部执行。可以做 command batching，把 100 个 `inc()` 合并成一次状态更新；可以把只读命令走 read cache 或 follower view；也可以让 actor method 支持小范围并行，但这会削弱 mailbox 的简单语义，需要用户声明哪些方法只读、哪些方法可交换。

还有一种选择是承认这个对象已经不像 actor，更像数据库里的热点行。此时可以把状态迁到专门的存储，比如带事务的 KV 或数据库，把 actor 变成访问这个存储的封装。面试里我会明确说：actor 热点不能靠“给同一个 actor 加更多 worker”解决。只要保持严格顺序，横向扩容就必须改变状态建模方式。

## Q780. LLM cache locality 与负载均衡之间如何权衡？

LLM 调度有两个目标会互相拉扯：尽量把请求打到已有模型缓存的 worker，减少 cold start；同时不能让一个 cached worker 被打满，导致排队时间超过冷启动节省的时间。

当前实现里，locality-aware 策略会给缓存命中的 worker 较高分数，也会考虑 `RunningTasks` 和容量。如果存在 cached worker 且任务排队时间还没超过 `localityQueueWait`，cold worker 会被明显降权。这个策略适合模型加载成本高、请求量不大的场景。

生产化时，我会把它改成预测成本比较：

```text

predicted_latency =
  queue_wait
  + model_load_or_checkpoint_fetch
  + expected_prefill_decode_time
  + eviction_risk

```

如果 cached worker 的 queue wait 已经很长，而 cold worker 加上模型加载后仍然更快，就应该把请求发给 cold worker。反过来，如果 cold start 很贵，哪怕 cached worker 忙一点，也值得等。

所以 locality 应该作为一项成本进入模型，而不是变成硬规则。调度器要比较总延迟，不要只看缓存命中。

## Q781. 如果所有请求都偏向 cached worker，会不会饿死其他 worker？

会有这个风险。

如果调度器只看 cache hit，某个 worker 缓存了热门模型，就会一直收到请求。其他 worker 即使空闲，也可能没有任务。短期看 cache hit rate 很好，长期看会出现几个问题：cached worker 排队变长，p95/p99 延迟变差；其他 worker 的缓存永远热不起来；一旦热门 worker 掉线，系统会突然经历大量 cold start。

解决方法是给 locality 加边界。比如：

- cached worker 的 queue wait 超过阈值后，允许 cold worker 接单。
- 给每个 worker 设置最大 inflight LLM 数。
- 对同一模型保留多个 warm replica，不把所有请求压到一个 worker。
- 调度分数里加入 fairness penalty，近期拿到太多请求的 worker 会降权。
- 空闲 worker 可以做模型预热，提前把热门模型拉到本地。

当前 LogServe 已经有 `localityQueueWait` 这个思路，只是还比较粗。下一步可以把它和 materialized LLM stats、worker executor queue、cache capacity 结合起来。这样既保留 cache locality 的收益，又不会把负载均衡完全牺牲掉。

## Q782. 调度器如何引入公平性？

公平性要先定义对象。是 worker 之间公平，租户之间公平，workflow 之间公平，还是 task type 之间公平？不同定义会导致不同调度策略。

如果是 worker 公平，可以在调度分数里加入最近窗口的 assignment count、running tasks、executor queue length。近期被分配太多任务的 worker 降权，空闲 worker 加权。LLM 场景还要保留 cache locality，比较合适的规则是：延迟成本相近时，偏向更少使用的 worker。

如果是租户公平，需要把队列按 tenant 拆分，调度时做 weighted round-robin 或 deficit round-robin。每个租户有自己的 quota 和 backlog。一个租户提交大量 fan-out workflow 时，只能消耗自己的份额，不能把系统队列全部占满。

如果是 task type 公平，需要 CPU task、LLM task、actor task 独立队列和资源池。worker 侧已经有 `taskQueue`、`llmQueue`、`actorQueue`，control 侧也应该对应拆分，否则上游队列还是会混在一起。

我会优先做 tenant + task type 两层公平。worker fairness 可以作为调度打分的一项。这样既能解释清楚，也最接近真实生产问题。

## Q783. 调度器如何处理优先级和抢占？

优先级可以先做非抢占式，再考虑抢占式。

非抢占式比较简单。队列从 FIFO 改成 priority queue，或者每个优先级一条队列。`PollTask` 时先看高优先级队列，再看低优先级队列。为了避免低优先级饿死，可以做 aging：任务等待时间越久，优先级分数越高。

抢占式要复杂很多。普通 task 一旦 worker 开始执行，平台不一定能安全中断用户代码。Python runner 可以超时重启，但这更像失败重试，不是干净的抢占。LLM 请求如果已经进入 vLLM，取消也要和下游 serving 系统协作。actor command 更麻烦，因为 mailbox 顺序不能乱。

所以我会把抢占分成三类：

- 排队阶段抢占：高优先级任务插队，这个最安全。
- 未开始执行的本地队列抢占：worker 本地 queue 中的任务可以重新排序。
- 已执行任务取消：需要任务函数支持 cancellation，或者把 task 标记为 cancellation requested，由 worker 协作退出。

LogServe 当前更适合先做排队阶段优先级。要做真正抢占，必须补 TaskCancelled 事件、worker cancel RPC、任务幂等和补偿语义。

## Q784. 如果任务很多，PollTask 线性扫描 queue 的复杂度是否可接受？

小规模可以接受，大规模不行。

当前 `PollTask` 拿到 `queueMu` 后遍历 `s.queue`。每个候选任务还要取 spec、查 metadata、判断 actor mailbox、判断 LLM worker 是否合适。队列很短时这很直接。队列达到几万甚至几十万时，复杂度就是问题了。更糟的是锁持有期间在扫描，多个 worker 高频 poll 会互相阻塞。

优化方向很明确：

第一，把队列拆分。worker 不扫全局队列，只扫自己可能执行的分区。比如 LLM worker 扫模型相关队列，actor owner 扫自己拥有 actor 的队列。

第二，维护 ready index。比如 `ready_by_type`、`ready_by_model`、`ready_by_worker`。提交任务或状态变化时把 task id 放到对应索引，poll 时直接取候选集合。

第三，避免在锁内做重逻辑。锁内只取候选 task id，锁外查 metadata 和调度打分。如果发现任务不再可用，再回到队列做修正。

第四，把队列持久化或半持久化。内存队列适合 demo，生产里可以使用 PostgreSQL、NATS、Redis Streams 或基于 shared log materialized queue 的结构。

所以线性扫描是当前实现的可接受简化，不是长期方案。

## Q785. 如何为不同 task type 建立独立队列？

可以从 `TaskSpec` 的字段判断 task type。普通 task 没有 `llm_model_name` 和 `actor_id`；LLM task 有 `llm_model_name`；actor task 有 `actor_id` 和 `actor_call_id`。

control 侧可以维护三类队列：

```text

taskQueue[partition]
llmQueue[model_key][partition]
actorQueue[actor_id]

```

普通 task 走 CPU worker 的队列。LLM task 走模型维度队列，方便 locality-aware 调度直接找到请求某个模型的任务。actor task 走 actor mailbox 队列，队列头必须满足 `command_seq == command_count + 1`，并且只能被 owner worker poll 到。

worker 侧已经有类似结构：`taskQueue`、`llmQueue`、`actorQueue` 三个本地 channel，以及不同 executor pool。control 侧补齐同样的拆分后，上下游模型就一致了。

要注意一点：独立队列不能破坏统一的 task 状态机。无论 task 进入哪条队列，`TaskSubmitted`、lease、start、complete、redelivery 还是同一套事件和 metadata 逻辑。队列只影响调度候选，不改变 source of truth。

## Q786. 如何避免 workflow fan-out 造成控制面压力？

workflow fan-out 会带来两个压力：短时间创建大量 task，打满 control 的队列和 log append；大量 step 同时 ready，`scheduleReadySteps` 会在一个循环里不断提交 task。

可以从几个位置限流。

第一，在 workflow definition 上加并发上限。比如 `max_parallel_steps` 或 `fanout_limit`。同一个 workflow 同时最多调度 N 个 running step，剩余 ready step 留在 ready set 里。

第二，引入 ready step 队列。`scheduleReadySteps` 不必一次性把所有 ready step 都转成 task。它可以只标记 ready，再由单独调度循环按 token 发放。

第三，按 tenant 或 workflow 做 backpressure。某个 workflow 的 fan-out 太大时，只限制它自己，不影响其他 workflow。

第四，批量写 log。fan-out 提交 task 时，如果每个 step 都单独 append `TaskSubmitted` 和 `StepScheduled`，logd 会承受很多小写。后续可以做 batch append 或事务性 append。

第五，worker 侧也要有队列容量反馈。只看 control queue 不够，worker 本地 executor queue 满了，control 也应该降低投递速度。

我会优先做 workflow-level concurrency limit，因为它改动小，语义清楚，也能直接解释为什么 fan-out 不会把控制面压垮。

## Q787. 如何缓存 ready steps？

当前 ready step 是临时计算出来的。`scheduleReadySteps` 读 workflow state，遍历 definition 中所有 step，检查状态和依赖是否满足。DAG 小时没问题，DAG 大时每次完成一个 step 都全量扫一遍，成本会变高。

可以在 workflow state 里维护两个结构：

```text

remaining_deps[step_id] = count
dependents[step_id] = []step_id
ready_steps = queue or set

```

workflow 创建时根据 DAG 计算每个 step 的入度。入度为 0 的 step 放进 ready set。某个 step 成功后，只更新它的下游 step，把下游的 `remaining_deps` 减 1。减到 0 时加入 ready set。这样每次 step completion 只处理局部边，不用扫完整 DAG。

这套结构也要写进事件或 checkpoint。最稳妥的做法是 replay 时可以从事件重建 ready set，metadata view 里保存它只是为了加速。即使 ready cache 丢失，也能重新遍历 DAG 恢复。

还要处理 retry 和失败。失败但还没超过 max_attempts 时，当前 step 自己重新进入 ready set；超过 max_attempts 后，workflow 进入 failed，下游 step 不再 ready。这个规则必须和事件 replay 保持一致。

## Q788. 如何减少 metadata lock contention？

当前 MemoryStore 为了简单，很多 map 操作受同一类锁保护。control 里还有 `queueMu`、`workflowMu`、`specMu`、`llmStatsMu` 等锁。小规模测试没问题，高并发时锁竞争会集中在提交、poll、complete、workflow 调度这几条路径。

减少锁竞争可以分层做。

第一，拆锁粒度。task map、workflow map、actor map、worker map、idempotency map 分开锁。workflow 可以按 workflow_id 建锁，actor 已经有 per-actor lock 的思路，task 也可以按 shard lock。

第二，缩短锁持有时间。不要在锁内做 log append、对象存储写入、复杂调度打分。锁内只读写内存状态，外部 I/O 放到锁外。

第三，把全局队列改成分区队列。每个 partition 自己有锁，worker poll 不再抢同一个 `queueMu`。

第四，读多写少的数据用 RLock 或 copy-on-write。比如 model registry、worker cached models、LLM stats 可以做快照读，调度器拿快照打分，不必长时间占锁。

第五，PostgreSQL store 要靠数据库事务和索引，而不是 Go 内存锁。比如 `LeaseTask` 用条件更新，`CompleteTask` 用 task_id + lease_epoch 条件提交。

优化顺序上，我会先处理 queue 和 workflow 锁。因为它们直接卡调度热路径。

## Q789. 如何把 log append 做 batch？

当前 control 和 worker 多数事件是单条 `AppendLog`。这让语义清楚，但每条事件都要经历一次 gRPC、编码、写 segment、写 index、按 fsync policy 同步，吞吐会被固定开销限制。

batch append 可以在 logd 暴露一个新接口：

```text

AppendBatch(records[])

```

请求里每条 record 仍然有 stream_id、event_type、idempotency_key、payload。logd 在一个临界区里给每个 stream 分配 seq，把多条 record 顺序写入同一个 segment，再一次性写 index，最后按 fsync policy 同步。

batch 的关键是返回值要逐条对应。某些 record 可能 duplicate，某些 record 可能新写入。客户端需要知道每条事件的 seq 和 duplicate 状态。

workflow fan-out 是 batch 的典型场景。一次调度多个 ready step 时，可以把多个 `TaskSubmitted` 或 `StepScheduled` 合并写。LLM model load 事件也可以批量写，但收益没 workflow fan-out 明显。

如果需要跨 stream 原子性，batch append 还不够，需要事务 marker。普通 batch 只减少系统调用和 fsync 次数，不自动提供跨 stream 原子提交。

## Q790. 如何将 logstore 改成异步 flush？

当前 logstore 支持 `always`、`batch`、`interval` 三种 fsync policy。要做更完整的异步 flush，可以把 append 和 sync 解耦。

写路径可以这样设计：

1. append 线程把 record 写进 OS page cache 和内存 index。
2. record 进入 pending sync 队列。
3. 后台 flush goroutine 按时间、条数或字节阈值调用 `fsync`。
4. flush 完成后更新 durable position。

这里有两种返回语义。

第一种是 `AppendLog` 等待 record 被写入内核缓冲区就返回。这种吞吐高，但进程或机器崩溃时可能丢掉已经返回成功的 record。

第二种是 `AppendLog` 等待后台 flush 确认 durable position 覆盖自己的 record 再返回。这仍然可以 batch 多个请求的 fsync，吞吐比 always 好，同时成功语义更强。它接近 group commit。

我更倾向第二种作为默认生产语义。这样客户端看到成功时，至少可以认为当前 fsync policy 下这条记录已经 durable。第一种可以作为 `acks=memory` 或 `unsafe_async` 模式，用于 benchmark 或可丢场景。

## Q791. 异步 flush 后客户端成功语义如何变化？

语义取决于 `AppendLog` 在哪个点返回。

如果写入内存或 page cache 后就返回，客户端成功只表示 logd 接收了请求，并把 record 交给了本地写路径。它不表示 fsync 完成。logd 进程崩溃、操作系统崩溃、机器断电时，成功返回的记录可能丢失。此时系统只能说吞吐更高，持久性更弱。

如果等待 group commit 后返回，客户端成功表示 record 已进入 batch，并且该 batch 的 fsync 已完成。多个请求共享一次 fsync，但每个请求仍然等到 durable 再返回。这种语义更适合 LogServe，因为 shared log 是 source of truth。

所以配置里要把语义写清楚：

```text

acks=durable   append returns after fsync/group commit
acks=memory    append returns after local write buffer accepts the record

```

对于 control plane 的 log-first 路径，我会要求 `acks=durable`。如果 `SubmitWorkflow` 先返回成功但事件还没 durable，机器崩溃后 workflow 消失，用户会很难接受。对于纯实验压测，可以开放 `acks=memory`，但报告里必须标注它的持久性边界。

## Q792. 如何设计多 log shard？stream 如何分片？

多 log shard 的目标是把 append、read、replay 压力分散到多个 logd 实例或多个 Store。最简单的分片方法是按 stream_id 做一致性哈希：

```text

shard = hash(stream_id) % shard_count

```

这样同一个 stream 永远落到同一个 shard，stream 内 seq 仍然单调递增。`task:<task_id>`、`wf:<workflow_id>`、`actor:<actor_id>`、`llm:<task_id>` 都可以按这个规则分布。

分片表需要可变。生产里不能简单 `% shard_count`，因为扩容会让大量 stream 迁移。更好的办法是 consistent hashing 或 virtual nodes。control 和 SDK 通过 shard map 找到 stream 对应的 logd。shard map 本身要有版本，最好由一个配置服务或 Raft 元数据管理。

读路径也要知道 shard。`ReadLog(stream_id)` 直接路由到对应 shard。`ListStreams(prefix)` 会变成 fan-out 查询：向所有 shard 请求 prefix，再合并结果。这也是为什么大规模系统要避免频繁全局 `ListStreams`。

多 shard 后，单 stream 语义仍然简单；跨 stream 原子性会变难。这个边界必须提前讲清楚。

## Q793. 跨 shard workflow 需要什么一致性保证？

如果一个 workflow 的所有事件都在 `wf:<workflow_id>` stream，workflow 自身恢复并不需要跨 shard 一致性。问题出在 workflow step 会创建 task，而 task 事件在 `task:<task_id>` stream，可能落到另一个 shard。

最低要求是引用关系可恢复。`StepScheduled` 事件里要有 task_id，`TaskSubmitted` 里要有 workflow_id 和 step_id。即使它们在不同 shard，replay 也能把两边 join 起来。

更强一点，需要处理半成功状态。比如 `TaskSubmitted` 写到了 task shard，但 `StepScheduled` 写 workflow shard 失败。当前单机实现里也有类似双写顺序问题，只是概率和恢复复杂度小。跨 shard 后，最好引入跨 stream transaction marker，或者把 workflow scheduling 的关键事实收敛到一个 stream。

可选设计有两种。

第一，把 workflow 相关 task 事件也写到 workflow shard。task stream 作为辅助索引，可以从 workflow stream 重建。这样 workflow 一致性强，但 task 查询要多做映射。

第二，保留 task shard 分散，用 transaction marker 表示 `TaskSubmitted + StepScheduled` 同一事务提交。replay 只应用 committed transaction。

如果要先保证工程可落地，我会选择第一种：workflow 的控制状态尽量集中在 workflow stream，task stream 记录执行生命周期。跨 shard 原子事务等需求明确后再补。

## Q794. 如何用 consistent hashing 分配 actor 到 worker？

actor placement 可以用 consistent hashing，把 `actor_id` 映射到 worker ring：

```text

owner = ring.Lookup(actor_id)

```

每个 worker 在 ring 上有多个 virtual nodes。worker 加入或离开时，只有一部分 actor 需要迁移，不会像普通取模那样大面积重分布。

不过 LogServe 的 actor 还有 ownership epoch，所以不能只在内存里算出 owner 就直接执行。正确流程应该是：

1. control 根据 ring 算出候选 owner。
2. 给 actor stream 写 `ActorOwnershipGranted(actor_id, worker_id, epoch)`。
3. metadata view 更新 owner_worker_id 和 epoch。
4. worker poll actor task 时必须匹配 owner 和 epoch。

consistent hashing 解决“应该放哪台 worker”的问题，epoch fencing 解决“旧 owner 还能不能写”的问题。两者不能互相替代。

还要考虑负载。纯 hash 不知道某个 actor 是否很热。生产里可以做 weighted consistent hashing，把 worker capacity、actor 热度、worker 本地缓存都纳入权重。热点 actor 仍然需要拆分或迁移，不能指望 hash 自动解决单 actor 串行瓶颈。

## Q795. 如何做 worker autoscaling？

worker autoscaling 要看两类信号：需求是否增加，以及增加哪种 worker。

基础指标包括 control queue depth、task arrival rate、task completion rate、executor queue wait、running tasks、CPU/内存、LLM cache hit rate、LLM p95/p99 latency、actor mailbox backlog。只看全局 queue depth 不够，因为 CPU task 堆积和 LLM task 堆积需要不同 worker。

扩容逻辑可以按 task type 拆开：

- CPU task backlog 高，增加普通 task worker。
- LLM queue wait 高且 cached worker 忙，增加带目标模型预热的 LLM worker。
- actor mailbox backlog 高，要先判断是很多 actor 分散堆积，还是单 actor 热点。前者可以加 worker，后者要 sharding 或 batching。

缩容也要谨慎。worker 退出前需要进入 draining 状态：停止 poll 新任务，等待本地任务完成，释放或迁移 actor ownership，上报最后一次 cache 状态。对于 LLM worker，本地模型缓存有价值，缩容可能降低后续 cache hit rate，所以要把 cache warmness 纳入缩容策略。

在 Kubernetes 里，可以让 worker 作为 Deployment 或 StatefulSet。HPA/KEDA 根据 Prometheus 指标扩缩容。更理想的是 LogServe 自己暴露 scheduler metrics，由 autoscaler 读取具体 backlog 和 SLO violation，再决定扩哪一类 worker。

我会把 autoscaling 的目标说成“让队列等待时间回到 SLO 内”，而不是简单追求 worker 数量变化。worker 多不代表系统快，关键是瓶颈对应的资源是否真的增加。
