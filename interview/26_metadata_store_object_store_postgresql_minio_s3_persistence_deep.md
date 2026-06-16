# 九、Metadata Store、Object Store、PostgreSQL、MinIO/S3 与持久化边界（深度）

这一组问题主要考察边界判断。回答时要抓住三层状态：shared log 是事件事实，metadata store 是当前视图，object store 是大对象和 snapshot 的承载层。PostgreSQL 和 MinIO 能增强持久化能力，但它们不能替代 shared log 的恢复语义。

## Q646. `MemoryStore` 的锁粒度如何影响并发性能？

当前 `MemoryStore` 用一个全局 `sync.RWMutex` 保护所有 map。读操作走 `RLock`，写操作走 `Lock`。

这个设计简单，也不容易写错。task、workflow、actor、worker、model 的所有状态都在同一个锁下修改，避免了交叉状态更新时出现数据竞争。对单机实验和小规模 demo 来说，这个锁粒度够用。

问题也很明显：所有写操作会互相串行。比如一个 actor command 在 `UpdateActor`，同时另一个普通 task 在 `CompleteTask`，再同时一个 worker 在 `Heartbeat`，它们都要抢同一把锁。读操作虽然可以并行，但 clone 返回值也发生在锁内，workflow 或 actor state 很大时，读锁持有时间会变长。

这会带来几个性能瓶颈：

- worker 心跳频繁时，会和 task lease、complete 抢锁。
- actor 热点调用多时，会影响 unrelated task 的 metadata 更新。
- dashboard 调 `ListTasks/ListWorkflows/ListActors` 时要遍历并 clone，一次读可能拖住写操作。
- `RequeueExpiredRunningTasks` 扫描所有 task，它持有写锁，任务数上来后影响更明显。

如果要继续优化，我会先做两件事。第一，把锁拆成几组：task lock、workflow lock、actor lock、worker lock、model lock。第二，对 task 和 actor 再做 per-key 或 sharded lock。这样普通 task 完成、actor 状态推进、worker heartbeat 就不会互相挡住。

但也不能一上来就把锁拆太碎。workflow step 完成时会同时更新 task、workflow、queue，actor command 完成时也会同时更新 task 和 actor。拆锁后必须规定锁顺序，否则容易引入死锁。当前全局锁的价值就是实现简单，语义清楚。

## Q647. `GetTask/GetWorkflow` 为什么要 clone 返回值？

因为 Go 里的 slice、map、指针字段都可能共享底层数据。`MemoryStore` 里的状态是内部 view，如果直接把内部对象返回给调用方，调用方就可以绕过锁修改 store。

以 task 为例，`Task.ResultJSON` 是 `[]byte`。如果 `GetTask` 直接返回内部 task，调用方拿到后改 `ResultJSON[0]`，metadata store 里的结果也会被改掉，而且这次修改不经过锁。

workflow 更危险。`workflow.State` 里有：

- `Definition.Steps`
- `Definition.ArgsJSON`
- `Steps map[string]StepState`
- step 的 `ResultJSON`
- `StepOrder`

如果这些字段不 clone，control 在调度 workflow 时可能拿到一个共享 map。外部代码改 map，内部 view 就会被污染，也可能触发并发 map 读写 panic。

actor 同理。`actor.State` 里有 `ClassSource`、`InitArgsJSON`、`StateJSON`。这些内容会被 replay、snapshot、actor task 构造使用，不能让调用方拿到内部引用后随便改。

clone 的代价是多一次内存复制。这个代价在小规模下可以接受。等状态变大后，可以考虑只读结构、copy-on-write、按字段 clone，或者把大字段改成 ref。但不能为了省复制把内部状态暴露出去，这会直接破坏 metadata view 的一致性。

## Q648. `CreateTask` 的 idempotency map 如何处理重复提交？

`CreateTask` 收到 task 和 idempotency key 后，先看 key 是否为空。如果 key 不为空，就查 `taskByIdemKey`。

如果 key 已经存在，它会找到对应的 `task_id`，返回已有 task，并把 `duplicate` 标记为 `true`。这意味着同一个 idempotency key 重复提交时，不会创建第二个 task。

如果 key 不存在，它会写入：

- `tasks[task.TaskID] = task`
- `taskByIdemKey[idempotencyKey] = task.TaskID`

这样后续同 key 的提交都能回到同一个 task。

有一个边界要讲清楚：`MemoryStore.CreateTask` 自己不比较 payload 是否一致。它只负责“同一个 key 映射到同一个 task”。payload fingerprint 的检查放在 control 层。

control 在 `enqueueTaskWithMetadata` 里会先计算 `taskSpecFingerprint`。如果同一个 idempotency key 已经存在，control 会调用 `ensureIdempotencyFingerprint`，检查新请求的 fingerprint 和旧请求是否一致。不一致就返回冲突，而不是把不同 payload 当成同一个 task。

所以幂等分成两层：

- metadata store 负责 key 到已有对象的查找。
- control 负责判断同 key 请求是否真的是同一份语义。

空 idempotency key 不参与去重。每次提交都会创建新 task。

## Q649. `CompleteTask` 如何保证 terminal status 不被覆盖？

`MemoryStore.CompleteTask` 开头会检查 task 当前状态。如果 task 已经是 `SUCCEEDED` 或 `FAILED`，它直接返回已有 task，不再改状态、结果、错误或 worker id。

这条规则很重要。worker 侧是 at-least-once 执行，`CompleteTask` 也可能因为网络超时被重试。没有 terminal guard 的话，后到的重复 completion 可能覆盖第一次 completion。

比如某个 task 已经成功写入结果，worker 因为 gRPC 超时又重试一次失败 completion。如果没有保护，状态会从成功变失败。当前实现不会这样做，因为 terminal task 直接返回已有状态。

redelivery 场景也是一样。旧 worker 执行慢了，task 已经被重新投递给新 worker。只要新 worker 先完成并进入 terminal，旧 worker 后来的 completion 就不能覆盖它。

control 层收到已 terminal task 的重复 completion 时，会把它当成幂等完成处理，返回 accepted。这里的 accepted 不是“重新写入成功”，而是“系统已经有终态了，这次请求不需要再改变状态”。

## Q650. `LeaseTask` 与 `CompleteTask` 的并发条件如何处理？

在 `MemoryStore` 内，`LeaseTask` 和 `CompleteTask` 都拿同一把写锁，所以它们不会同时修改同一个 task。并发竞争会被锁串行化。

`LeaseTask` 做的事是：

- 如果 task 已经 terminal，直接返回，不修改。
- 否则递增 `TaskLeaseEpoch`。
- 把状态改成 `RUNNING`。
- 写入 `WorkerID`。
- 更新时间。

`CompleteTask` 做的事是：

- 如果 task 已经 terminal，直接返回已有状态。
- 如果 task 是 `RUNNING`，检查 worker id 和 lease epoch。
- worker id 不匹配，拒绝。
- lease epoch 不匹配，拒绝。
- 校验通过后写入终态和结果。

这套逻辑处理了几个常见竞争。

第一，lease 先发生，completion 后发生。completion 必须带当前 worker id 和 epoch，否则就是 stale completion。

第二，completion 先发生，lease 后发生。lease 看到 task 已经 terminal，会直接返回，不会把它重新变成 running。

第三，redelivery 后旧 worker completion 到达。redelivery 会让 task 重新排队并在下一次 lease 时递增 epoch。旧 worker 带的是旧 epoch，`CompleteTask` 会拒绝。

真正的边界在 control 层。task 的日志事件、metadata 状态、queue 状态不是一个数据库事务包起来的。所以 LogServe 采用的是 log-first 加 lease epoch fencing，而不是宣称严格 exactly-once。

## Q651. Worker `RunningTasks` 在 control 重启后如何重建？

当前实现没有把 `RunningTasks` 当成强事件状态重建。

control 启动时会从 `system:workers` stream 读取 `WorkerRegistered` 事件，恢复 worker 的基础信息，比如 worker id、address、labels、cached models、capacity 和注册时的 heartbeat 时间。`RunningTasks` 默认是 0。

task bootstrap 时，如果从 task log 看到 task 处于 `RUNNING`，`replayTaskSpec` 会把它恢复为 `QUEUED`。这是一个有意的选择：control 重启后，不应该相信旧的 running 状态仍然有效。原 worker 可能还在跑，也可能已经断开。把它放回 queue，再靠 lease epoch 和 completion 校验处理重复结果，语义更稳。

所以重启后 `RunningTasks` 通常从 0 开始。后续 worker 重新 `PollTask`，control lease task 时调用 `IncrementWorkerLoad`，task 完成时调用 `DecrementWorkerLoad`。这个计数会在运行过程中重新收敛。

如果面试官问“能不能从 log 精确恢复 RunningTasks”，答案是当前没有。因为 heartbeat、worker 本地执行队列、control 内部 running 计数都不是完整事件流。当前把 `RunningTasks` 当调度视图，而不是 source of truth。

## Q652. 如果 metadata view 重建后 `RunningTasks` 不准确，会影响什么？

主要影响调度和观测。

如果 `RunningTasks` 偏低，control 可能认为 worker 还有空闲 capacity，把更多 task lease 给它。结果是这个 worker 被过度分配，本地 executor queue 变长，task latency 上升。

如果 `RunningTasks` 偏高，control 可能认为 worker 忙，少给它任务。结果是 worker 空闲但调度器不用它，吞吐下降。

对 LLM 调度也有影响。locality-aware 和 predicted-latency 策略会考虑 worker 是否有容量、队列等待、模型缓存等信息。`RunningTasks` 不准时，调度器可能错过有缓存的 worker，或者把请求打到已经拥堵的 worker。

dashboard 也会受影响。用户看到的 running task 数、worker load、queue depth 可能和真实执行情况有偏差。

当前设计把这个问题控制在“调度视图偏差”，而不是“状态恢复错误”。task 的最终状态仍然由 task log、lease epoch、terminal guard 决定。`RunningTasks` 不准会影响性能和展示，不应该影响 task 是否最终完成。

生产化时我会加几类修正信号：

- worker heartbeat 上报本地 running task ids。
- control 定期按 task metadata 重新计算 worker load。
- task lease 超时后强制扣减旧 worker load。
- dashboard 标记 worker load 是否来自心跳确认。

## Q653. PostgreSQL store 是否应该用事务包裹状态更新？

应该，但要分情况看。

当前 `PostgresStore` 里，`persistWorkflow` 已经使用事务。它要同时写 `workflow_instances` 和多条 `workflow_steps`，这必须放在一个事务里。否则 workflow 主表更新成功、step 表更新一半，dashboard 查询会看到不完整 view。

`persistWorker` 也用了事务。它要写 `workers`，还要删除并重写 `worker_model_cache`。如果没有事务，worker cache 表可能短暂缺失或只写一半。

`persistTask` 当前是两步：先 upsert `task_instances`，如果是 LLM task，再 upsert `llm_requests`。这块最好也用事务。否则 task row 已经更新，llm_requests 写失败，PG view 里的 LLM 请求就不完整。由于 PG 不是 source of truth，系统不会因此丢事件，但 dashboard 和 SQL 查询会漂移。

`persistActor` 当前写的是单表 `actor_instances`，事务收益不大。但如果后续把 `actor_commands` 也真正接入写路径，actor state 和 command row 就应该放进同一个事务。

还有一个更深的点：当前 `PostgresStore` 对 PG 写失败的处理是 `remember(err)`，也就是记录最近错误，但不会让 metadata 接口返回失败。这符合“PG 是 view，不是事实源”的定位。生产化时可以继续保持这个定位，但要把 `LastError` 暴露到 health check 和 dashboard，不能让 PG 持久化失败悄悄发生。

## Q654. PostgreSQL 与 shared log 的双写一致性如何保证？

当前没有分布式事务，也不需要把它包装成严格双写一致。

LogServe 的策略是 log-first：业务事件先 append 到 shared log，append 成功后才更新 metadata view。PostgreSQL metadata 是 view 的持久副本。只要 log 写成功，PG 写失败也不破坏事实，重启后可以从 log 重建 PG view。

最危险的顺序是反过来：PG 先写成功，log append 失败。这样 metadata 里出现了一个没有事件来源的状态，replay 无法解释它。当前主要入口会避免这个顺序。比如 `SubmitWorkflow` 先写 `WorkflowStarted`，再 `CreateWorkflow`。`enqueueTaskWithMetadata` 先写 `TaskSubmitted`，再 `CreateTask`。actor create、ownership grant、step succeeded 等路径也遵循类似顺序。

严格说，metadata 内存更新和 PG upsert 也不是和 log append 同一个事务。当前接受这种边界，因为 shared log 是 source of truth，metadata 可以重建。

如果以后要把 PostgreSQL 也当强一致读模型，有两个选择。一个是 transactional outbox：先在 PG 事务里写业务 row 和 outbox row，再由 relay append log。另一个是让 log 成为唯一写入口，PG 只由独立 materializer 消费 log 更新。对 LogServe 现在的主线，我更倾向后者，语义更干净。

## Q655. 如果 object store `Put` 成功但 log append 失败，孤儿对象如何清理？

当前 workflow 大结果和 actor snapshot 都是先 `Put` object store，再 append 对应事件。比如 workflow step 成功时，先把大结果写到 result store，再写 `StepSucceeded`。actor snapshot 也是先写 snapshot 对象，再写 `ActorSnapshotCreated`。

如果 `Put` 成功但 log append 失败，object store 里会留下一个没有任何 log ref 指向的对象。这个对象叫 orphan object。

这不会破坏状态一致性。因为 shared log 里没有 `result_ref` 或 `snapshot_ref`，replay 不会认为这个对象存在。问题是空间泄漏，时间长了会浪费本地磁盘或 MinIO bucket。

清理方式应该做成后台 GC：

1. 扫描 shared log，收集所有 `result_ref` 和 `snapshot_ref`。
2. 扫描 metadata view，收集当前仍可见的 result/snapshot ref。
3. 扫描 object store 对象列表。
4. 对没有被任何 ref 引用、并且创建时间早于安全窗口的对象，删除。

当前 `objectstore.Store` 只有 `Put/Get`，没有 `List/Delete`，所以还不能做完整 GC。后续要补 `List(namespace)` 和 `Delete(ref)`，或者在写对象时同时写 manifest，GC 根据 manifest 做清理。

内容哈希命名能缓解一部分问题。相同内容重复 Put 会得到同一个 hash 文件名，不会无限产生随机文件。但它不能替代 orphan GC。

## Q656. 如果 log 里有 `result_ref` 但 object store 对象丢失，系统如何发现？

系统会在真正加载对象时发现。

workflow 有两条路径会加载 result ref。第一条是 `ResolveArgs()`。如果下游 step 参数引用了上游 step，而上游 step 只有 `ResultRef`，它会调用 `LoadResult(ref)`。对象丢失时，`LoadResult` 返回错误，workflow 调度就会失败。

第二条是 `completeWorkflow()`。如果 final result step 只有 ref 没有 inline result，control 会先 `LoadResult`，再决定 final result 是 inline 还是继续写 ref。对象丢失时，workflow 无法正常完成。

actor 也会遇到这个问题。`ReplayActor` 使用 snapshot 时，会读取 `ActorSnapshotCreated` 里的 `SnapshotRef`。如果 snapshot 对象没了，actor replay 会返回错误，接管 worker 就无法从 snapshot 恢复状态。

现在的发现方式偏被动：用到对象时才发现。更好的做法是提供一致性检查工具，定期扫描 log 和 metadata 中的所有 ref，对每个 ref 调 `Get`，并校验大小和 hash。这样对象丢失可以在恢复事故前暴露出来。

## Q657. 是否需要为 object store 对象写 checksum？

需要。当前本地和 S3 adapter 的对象名已经包含 SHA-256，例如 `<sha256>.json`，这是一种内容寻址。但当前 `Get` 路径没有明确把读回来的内容重新计算 hash 并和 ref 对比。

如果对象存储或磁盘发生静默损坏，只能靠 JSON 解析失败、业务结果异常或底层存储自己的校验来间接发现。这个不够。

更稳的做法是把对象元信息做成结构化 ref 或 manifest：

```json
{
  "uri": "s3://logserve-results/workflows/wf-1/result/abc.json",
  "sha256": "abc...",
  "size_bytes": 1048576,
  "content_type": "application/json"
}
```

写入时记录 hash 和 size。读取时重新计算 SHA-256，大小也要对上。不一致就返回 corruption error，而不是把损坏内容交给 workflow 或 actor replay。

S3 的 ETag 不能完全替代应用层 checksum。分片上传、服务端加密、兼容实现都会让 ETag 语义变复杂。LogServe 自己维护 SHA-256 更直接。

## Q658. 是否需要为 `result_ref` 加版本或内容 hash？

需要。当前 `local://` 和 `s3://` ref 的路径里有内容 hash，这已经是一个不错的起点。但 ref 本身还没有明确的 schema 版本。

没有版本会带来迁移问题。比如以后 result store 从 JSON 对象改成压缩对象，或者从单对象改成 manifest 加多个 shard，老 ref 和新 ref 的解析方式就不一样。只靠字符串前缀很容易把兼容逻辑写散。

我会把 ref 设计成两层。

日志事件里保存结构化 result descriptor：

```json
{
  "ref_version": 1,
  "uri": "s3://logserve-results/workflows/wf-1/result/abc.json",
  "sha256": "abc...",
  "size_bytes": 12345,
  "encoding": "json"
}
```

SDK 和 dashboard 展示时可以把它压缩成一个字符串，但 replay 内部应该按结构化字段处理。

内容 hash 的作用也很实际。它让对象不可变。只要同一个 ref 指向的内容 hash 不变，replay 就可以相信读到的是当时写入的结果。如果对象被覆盖，hash 校验会立刻失败。

## Q659. large result 的生命周期和 retention 如何管理？

large result 的生命周期不能只按对象创建时间删。它要和 workflow、actor、log retention 一起看。

workflow result 至少要保留到这些条件都满足：

- workflow 已经 terminal。
- 用户不再需要查询 workflow 输出。
- 没有下游 workflow step 或外部引用依赖这个 result。
- shared log 中引用它的事件仍然可解释，或者已经有更高层的归档结果。

actor snapshot 更敏感。只要 actor replay 还依赖某个 `snapshot_ref`，这个对象就不能删。尤其是 logical trim 之后，snapshot 之前的 actor commands 默认读不到了，snapshot 对象就成了恢复起点。

合理的 GC 策略应该是引用驱动：

1. 从 shared log 收集所有 live refs。
2. 从 metadata view 收集当前 live refs。
3. 按 retention policy 判断哪些 workflow/actor 已经过保留期。
4. 只删除没有 live ref 指向、且超过 grace period 的对象。

还可以做分层策略。近期结果保留在热存储，老结果转冷存储；actor 只保留最近 N 个 snapshot；workflow terminal 后保留 X 天。无论策略怎么变，有一条底线不能变：删除对象之前，要确认 replay 不再需要它。

## Q660. actor snapshot 对象是否可以在 physical compaction 前删除？

当前活跃 snapshot 不能删。

actor snapshot 和 log trim 是配合使用的。`ActorSnapshotCreated` 写入后，control 会调用 `TrimStream(actor:<actor_id>, before_seq=snapshot_seq)`。普通 `ReadLog` 会隐藏 trim point 之前的记录。也就是说，replay actor 时会从 snapshot 加 tail log 恢复，而不是从最早的 `ActorCreated` 一条条回放。

如果这时把 snapshot 对象删掉，actor stream 里仍然有 `ActorSnapshotCreated`，但 `snapshot_ref` 读不到。replay 就断了。

只有两种情况下可以删旧 snapshot。

第一，已经有更新的 snapshot 成为恢复起点，且所有 replay 路径都不再引用旧 snapshot。比如 stream 保留的 tail 从新 snapshot 开始，旧 snapshot 的事件也已经不在普通 replay 视图里。

第二，完整历史仍可用，而且 replay API 支持跳过 snapshot 从 full log 恢复。但当前 logical trim 后普通 `ReadLog` 默认隐藏 trim 前记录，所以不能默认依赖这条路。

physical compaction 前更要谨慎。compaction 一旦删除旧 segment，full log 恢复路径就没了。删除 snapshot 必须晚于“新 snapshot 已持久化、对应事件已写入 log、trim/compaction 策略确认不再需要旧 snapshot”。

## Q661. 如果 MinIO/S3 临时不可用，workflow result materialization 应该失败还是降级 inline？

默认应该失败，不应该悄悄降级 inline。

原因很简单：`resultInlineThreshold` 本来就是为了防止大结果进入 shared log。如果 MinIO 挂了就把几十 MB 的结果塞进 log，可能把 log append 拖慢，甚至影响整个 control plane。

当前代码也是失败优先。`materializeResult()` 如果发现结果超过阈值，会调用 result store 的 `Put`。`Put` 返回错误时，`materializeResult` 把错误返回给上层，不会自动 inline。

这个语义更安全。对象存储故障应该暴露出来，让 workflow step completion 失败或等待重试，而不是改变持久化策略。

可以设计一个显式的降级模式，但要有硬限制。比如：

- 只有结果小于某个 emergency inline hard limit 才允许 inline。
- 事件里标记 `materialization_degraded=true`。
- dashboard 报警。
- 大于 hard limit 的结果仍然失败。

在当前项目里，保持 fail fast 更符合 log-first 主线。对象存储恢复后，worker 或控制面重试 completion，再把 result ref 补上。

## Q662. `resultInlineThreshold` 过低或过高分别有什么影响？

阈值过低，会让很多本来很小的结果也走 object store。影响是：

- 每个 step 多一次对象写入。
- 下游 `ResolveArgs` 多一次对象读取。
- 本地实验会产生很多小文件。
- MinIO/S3 场景下，小对象数量上升，list/GC/元数据成本变高。

阈值过高，问题反过来：

- shared log payload 变大。
- append latency 上升。
- fsync 成本变高。
- replay 时更容易把大结果读进内存。
- log segment 更快滚动，compactable bytes 也更难管理。

默认 4096 字节是一个保守值。它让普通 JSON 小结果直接 inline，避免过度访问对象存储；稍大的结果进入 result store，避免污染 shared log。

生产环境应该按 workload 调整。比如 RAG 文档片段可能几十 KB，LLM 输出可能几 KB 到几十 KB，embedding 向量也可能很大。不同任务类型可以有不同阈值，甚至由用户在 workflow step 上声明。

## Q663. metadata migrations 如何与 event replay 兼容？

迁移要服务 replay，而不是要求老日志适配新表。

当前 PostgreSQL migration 使用了很多 `CREATE TABLE IF NOT EXISTS` 和 `ALTER TABLE ... ADD COLUMN IF NOT EXISTS`。这是对的。control 启动时先 apply migrations，再 `BootstrapFromLog`。这样新列存在，replay 代码可以把旧事件 materialize 到新 schema 里。

兼容要注意几条。

第一，新增字段要有默认值。比如 actor 的 `submitted_command_count`、task 的 `task_lease_epoch` 都应该允许旧事件缺字段时正常恢复。

第二，event replay 代码要容忍旧 payload 缺字段。比如老的 task lifecycle event 没有 lease epoch 时，代码要能按保守规则处理。

第三，不能随便删除列或改变列语义。metadata view 可以重建，但 dashboard、SQL 查询、测试可能依赖旧字段。

第四，事件 schema 变化要和 metadata schema 分开看。表结构升级不应该要求重写 shared log。老事件 replay 到新表时，由 reducer 做补默认值、字段映射和兼容判断。

如果后续引入多版本 worker，还要在事件 payload 里加 `schema_version`，并让 replay reducer 按版本解析。

## Q664. 如果多个 control 实例写 PostgreSQL，需要什么锁或隔离级别？

当前实现不支持多个 control 实例 active-active 写同一套状态。原因不在 PostgreSQL 本身，而在 control plane 还有内存 queue、`specs` map、worker lease、actor ownership、LLM stats 等本地状态。

如果两个 control 同时运行，会有几类风险：

- 两个实例都从自己的内存 queue lease task。
- 两个实例都给 actor grant ownership。
- 两个实例都更新 PostgreSQL view，后写覆盖先写。
- worker 连接不同 control，lease epoch 视图不一致。

要支持多 control，至少需要这些机制。

第一，leader election。最简单是只有 leader control 能调度和写 metadata，follower 只提供只读查询或等待接管。可以用 etcd、PostgreSQL advisory lock、Kubernetes Lease。

第二，fencing token。leader 每次变更要带 epoch，PG 更新和 log append 都要检查 epoch，旧 leader 不能继续写。

第三，数据库事务和条件更新。比如 task completion 必须带当前 `task_lease_epoch`，SQL 层也要 `WHERE task_lease_epoch = ?`，不能只靠内存校验。

第四，queue 外置或从 log 派生。active-active 下，内存 queue 不够，需要 NATS/Redis/PostgreSQL queue，或者所有 scheduler 都消费同一个 log-derived queue，并通过 CAS 抢 lease。

隔离级别上，很多路径至少要 read committed 加条件更新；涉及所有权转移、actor command sequence 的路径更适合 serializable 或显式行锁。

## Q665. `DashboardSnapshot` 中 compactable log bytes 来自哪里？

来自 logstore 的 stream stats，不是 PostgreSQL。

control 的 `GetDashboardSnapshot` 会调用 `compactableLogStats()`。这个函数请求 log service 的 `GetStreamStats`。logd 收到后调用 store 的 `Stats`。

logstore 内部有每个 stream 的 index entries 和 `trimBefore`。`streamStatsLocked` 会遍历 stream 的 index。如果某条记录的 `seq < trimBefore`，它就被计入：

- `CompactableRecords`
- `CompactableBytes`

`CompactableBytes` 累加的是 index entry 里的 record length，也就是日志记录在 segment 里的字节长度。

所以这个指标表达的是：logical trim 之后，理论上已经不被普通 read 需要的日志记录有多少。它不是磁盘已经释放的空间。当前还没有真正 physical compaction，segment 文件还在，磁盘空间不会因为这个数字变大而自动减少。

dashboard 汇总所有 stream 的 compactable records/bytes，用来告诉用户“有多少日志已经可以被后续 compaction 处理”。

## Q666. NATS JetStream 在 Compose 中的定位是什么？当前核心路径是否依赖它？

Compose 里启动了 NATS JetStream，但当前核心执行路径不依赖它。

现在 task 调度是 control plane 内部 queue 加 worker 主动 `PollTask`。worker 通过 gRPC 向 control 拉任务，拿到 lease 后本地执行，再通过 `StartTask/CompleteTask` 回写状态。shared log 由 logd 提供，metadata view 由 control 维护。

NATS 在当前 Compose 里更像一个预留组件。它可以用于后续把 task delivery、事件订阅、dashboard streaming 或 worker 通知做成消息系统，但现在不是 source of truth，也不是 task queue 的核心。

这一点面试时要说准。不能因为 Compose 里有 NATS，就说 LogServe 当前已经用 JetStream 做任务队列。当前核心路径是 gRPC poll + shared log + metadata view。

## Q667. 如果将任务队列换成 NATS，shared log 还需要吗？

还需要。

NATS 可以解决的是消息投递问题：怎么把 task 通知给 worker，怎么做消费者组、ack、redelivery、backlog。shared log 解决的是另一件事：系统发生过什么，以及如何从事件历史恢复 workflow、actor、LLM、metadata view。

如果把 task queue 换成 NATS，我会这样拆：

- shared log 仍然记录 `TaskSubmitted`、`TaskStarted`、`TaskCompleted` 等事件。
- NATS 只承载可投递的 task message。
- control 先 append `TaskSubmitted`，再 publish 到 NATS。
- 如果 publish 失败，可以从 log 扫描未投递 task 重新 publish。

也就是说，NATS 是 delivery plane，shared log 是 recovery plane。两者不能互相替代。

如果只用 NATS，不保留 shared log，control 重启后很难解释 workflow step 为什么到了某个状态，actor command 执行到了第几个，LLM cache stats 从哪里来。消息队列的 backlog 不是完整事件历史。

## Q668. 如果将 metadata store 换成 etcd，能解决哪些问题？

etcd 适合保存小规模、强一致、需要 watch/lease 的元数据。换成 etcd 后，能改善几类问题。

第一，leader election 更自然。control 可以用 etcd lease 选 leader，旧 leader lease 失效后不能继续写。

第二，worker heartbeat 和 liveness 可以放到 etcd lease 上。worker session 过期后，watcher 能较快感知。

第三，actor ownership 可以用 compare-and-swap 和 revision 做 fencing。比如只有当前 epoch 匹配时才能更新 owner。

第四，多 control 下的配置和调度视图更容易共享。backpressure、scheduler policy、部分 worker metadata 可以被 watch。

但 etcd 不能替代所有东西。它不适合保存大结果，不适合保存大量事件历史，也不适合当高吞吐 shared log。workflow/actor replay 仍然需要 shared log；大结果和 snapshot 仍然需要 object store。

我会把 etcd 放在 control-plane coordination 层，而不是把它当 PostgreSQL 或 logstore 的直接替代品。

## Q669. 如何备份和恢复 logstore、PostgreSQL、MinIO 三类状态？

要按重要性和依赖顺序来备份。

第一类是 logstore。它最关键，保存事件事实。需要备份 segment log、index、retention metadata。如果 index 丢了可以重建，但 segment 丢了就很难恢复。备份时最好先停止 logd 或做文件系统快照，避免拷到一半的 segment。fsync policy 如果不是 always，还要理解“已返回成功的数据是否一定落盘”这个边界。

第二类是 MinIO/S3。它保存大 result 和 actor snapshot。需要备份 bucket，最好开启 versioning、对象锁或跨盘复制。因为 log 里只保存 ref，object store 对象丢失后，部分 replay 会断。

第三类是 PostgreSQL metadata。它是 materialized view，可以用 `pg_dump` 或 volume snapshot 备份。PG 丢失时可以从 log 重建，所以它的重要性低于 logstore 和 MinIO。但保留 PG 备份能加快恢复，也能保留一些 dashboard 查询便利。

恢复顺序我会这样安排：

1. 先恢复 logstore。
2. 再恢复 MinIO/S3 bucket。
3. PostgreSQL 可以恢复备份，也可以空库启动。
4. 启动 logd。
5. 启动 control，让它 apply migrations 并 `BootstrapFromLog`。
6. 启动 worker。
7. 跑一致性检查，确认 workflow、actor、object refs、metadata view 都能对上。

如果只能保一个备份，优先保 logstore 和 object store。PG view 可以重建；事件和对象丢了，系统就缺事实或缺数据。

## Q670. 如何定义系统的一致性检查工具？

我会做一个 `logserve doctor` 或 `logserve check-consistency`。

它要检查四层东西。

第一层是 log replay 和 metadata view 是否一致。对每个 workflow 调 `ReplayWorkflow`，比较 replay state 和 metadata state。对每个 actor 调 `ReplayActor`，比较 command count、epoch、snapshot ref、state_json。对 task stream，重放 `TaskSubmitted/Started/Completed/Failed/Redelivered`，和 metadata task status 对比。

第二层是 object refs 是否可读。扫描 workflow events、actor snapshot events、metadata view 中的 `result_ref` 和 `snapshot_ref`。逐个调用 result store `Get`。如果 ref 里带 hash，就重新计算 hash。缺对象、hash 不一致、JSON 解析失败都要报出来。

第三层是调度视图是否合理。检查 queue 里的 task 是否存在，queued/running task 是否有 spec，running task 的 worker 是否还 active，worker `RunningTasks` 是否和 task view 大致匹配，actor command sequence 是否连续。

第四层是 logstore retention。检查 trim point 是否小于等于 next seq，compactable bytes 是否和 index 计算一致，actor stream 是否至少保留当前 snapshot 事件和 tail log。

输出应该是机器可读的 JSON，同时给人看的摘要也要清楚。比如：

```json
{
  "ok": false,
  "workflow_mismatches": 1,
  "missing_objects": 2,
  "orphan_objects": 5,
  "stale_running_tasks": 3
}
```

这个工具不应该默认自动修复。检查和修复要分开。安全的修复可以提供显式参数，比如重建 metadata view、重算 worker load、删除超过 grace period 的 orphan objects。涉及删除 log segment、删除 snapshot、改 actor state 的操作，必须要求人工确认。

