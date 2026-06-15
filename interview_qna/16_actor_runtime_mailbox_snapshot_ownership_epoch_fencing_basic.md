# 六、Actor Runtime、Mailbox、Snapshot、Ownership 与 Epoch Fencing（简单）

这一组问题讲 actor 这条链路。LogServe 的 actor 不是普通无状态 task，而是一个有身份、有内存状态、有 owner worker、有命令顺序的对象。它的状态变更都写进 `actor:<actor_id>` stream，worker 崩溃后可以靠 snapshot 和 tail log 恢复。

## Q391. LogServe 中 actor 是什么？

Actor 是 LogServe 里的有状态对象。用户用 Python `@actor` 定义一个 class，创建实例后得到 `actor_id`，后续通过 `CallActor(actor_id, method, args)` 调用它的方法。

普通 task 执行完就结束，下一次调用不会继承上一次的内存状态。actor 不一样。比如 Counter：

```python
@actor
class Counter:
    def __init__(self):
        self.value = 0

    def inc(self):
        self.value += 1
        return self.value

    def get(self):
        return self.value
```

第一次 `inc()` 后，actor state 变成 `{"value": 1}`。第二次 `inc()` 要从这个状态继续执行，而不是重新从 0 开始。

在 LogServe 里，actor 的状态来源不是某个 worker 的内存本身，而是 actor stream 里的事件和 snapshot。worker 内存只是运行时缓存，崩了可以重建。

## Q392. 为什么需要 stateful actor？

很多 AI runtime 场景不是纯函数 task 能自然表达的。

比如：

- 一个会话对象要保存上下文。
- 一个 agent 要维护当前计划和工具调用状态。
- 一个缓存管理器要记录已经加载的模型。
- 一个计数器、限流器、状态机要连续处理请求。

如果全部用普通 task，就要把状态放到外部数据库里，每次 task 自己读写。这样可以做，但用户代码会很重，也容易出现并发写乱序。

Actor 的价值是把“状态 + 顺序执行 + 恢复”打包成一个 runtime 语义。同一个 actor 的命令按顺序执行，状态变更进入 actor stream。这样用户写的是面向对象代码，系统底层仍然能 replay 和恢复。

## Q393. actor:<actor_id> stream 中有哪些事件？

当前 actor stream 主要记录这些事件：

- `ActorCreated`
- `ActorOwnershipGranted`
- `ActorCommandSubmitted`
- `ActorCommandApplied`
- `ActorCommandFailed`
- `ActorSnapshotCreated`

`ActorCreated` 表示 actor 实例被创建。`ActorOwnershipGranted` 表示某个 worker 拿到这个 actor 的执行权。`ActorCommandSubmitted` 表示一个方法调用已经进入 actor mailbox。`ActorCommandApplied` 表示命令执行成功，actor state 已经推进。`ActorCommandFailed` 表示命令执行失败，但命令序号仍然被消费。`ActorSnapshotCreated` 表示某个 command count 的状态已经保存成 snapshot。

这些事件按 stream 内 seq 排列。ReplayActor 就是读这个 stream，然后恢复 actor 的 owner、epoch、command_count、state_json 和 snapshot 信息。

## Q394. ActorCreated 记录什么？

`ActorCreated` 是 actor stream 的起点。它记录一个 actor 实例最基本的信息：

- actor_id
- class_name
- class_source
- init_args_json
- snapshot_every
- idempotency_key
- idempotency_fingerprint
- timestamp_ms

这里最重要的是 class source 和 init args。因为 worker 接手 actor 时，需要知道这个 actor 是哪个 Python class 创建出来的，以及初始化参数是什么。

`snapshot_every` 也在这里记录。比如默认 25，意思是每执行 25 条命令尝试生成一次 snapshot。

如果 `CreateActor` 的 metadata 更新失败，但 `ActorCreated` 已经写进 log，重启后仍然可以从 actor stream 重建 actor。这个路径仍然遵守 log-first。

## Q395. ActorOwnershipGranted 记录什么？

`ActorOwnershipGranted` 记录 actor 当前归哪个 worker 执行。

payload 里主要是：

- actor_id
- worker_id
- epoch
- timestamp_ms

`worker_id` 是新的 owner。`epoch` 是 ownership 的任期号，每次换 owner 都递增。

这个事件解决两个问题。第一，actor task 应该发给哪个 worker。第二，旧 worker 如果失联后又回来，不能继续拿旧 epoch 写状态。

控制面在 `ensureActorOwner` 里检查当前 owner 的 heartbeat。如果 owner 还活着，就继续用它。如果 owner 超时，就从活跃 worker 中选一个新的 owner，写 `ActorOwnershipGranted`，epoch 加 1。

## Q396. ActorCommandSubmitted 和 ActorCommandApplied 有什么区别？

`ActorCommandSubmitted` 表示命令已经进入 actor mailbox，但还没修改 actor state。

它记录的是：

- 这次调用的 call_id
- method_name
- args_json
- command_seq
- owner worker
- epoch

`ActorCommandApplied` 表示命令已经执行完成，并且 actor state 已经推进。它会记录：

- 同一个 call_id
- 同一个 command_seq
- method_name 和 args
- result_json
- 最新 state_json
- worker_id
- epoch
- command_count

简单说，Submitted 是“排队了”，Applied 是“状态已经变了”。

这个拆分很关键。调用可能已经提交，但客户端等待超时；后续命令仍然要拿到新的 command_seq，不能因为客户端超时就把序号重复用掉。当前测试里也覆盖了 timed-out submitted command 后 command_seq 继续递增。

## Q397. command_seq 是什么？

`command_seq` 是 actor 命令的逻辑序号。

对同一个 actor 来说，第一条命令是 1，第二条是 2，后面依次递增。控制面在提交 actor command 时，根据 `SubmittedCommandCount` 和 `CommandCount` 计算下一个序号。

它有两个作用。

第一，定义 actor mailbox 的顺序。即使多个客户端并发调用 `inc()`，它们也会被分配成 1、2、3、4 这样的序号。

第二，完成时防乱序。`completeActorCall` 会检查本次 completion 的 `command_seq` 是否等于 `state.CommandCount + 1`。如果当前 actor 只应用到 1，却收到了 seq=3 的 completion，就拒绝。

没有 command_seq，只靠队列顺序是不够的。队列会 redelivery，worker 会重启，网络会重试。command_seq 把“应该第几个执行”写成了可恢复的事实。

## Q398. mailbox 串行化解决什么问题？

Mailbox 串行化解决同一 actor 内部状态并发写乱序的问题。

还是 Counter 例子。假设当前 value=0，同时来了两个 `inc()`。如果两个 worker 同时读到 0，各自加 1，然后都写回 1，最终结果就是 1，而不是 2。

LogServe 通过几层机制避免这个问题：

- `submitActorCommand` 对同一个 actor 加锁，分配递增的 command_seq。
- `PollTask` 只放行 `command_seq == command_count + 1` 的 actor task。
- actor task 只能给 owner worker 执行。
- worker 本地 actor pool 虽然可以有多个 goroutine，但同一个 actor_id 有 per-actor lock。
- completion 时再次检查 command_seq，乱序完成会被拒绝。

所以 mailbox 不只是一个内存队列。它是 control plane 顺序、worker 本地锁、事件日志和 completion 校验一起组成的。

## Q399. owner_worker_id 表示什么？

`owner_worker_id` 表示当前拥有 actor 执行权的 worker。

Actor 的方法调用不能随便发到任意 worker，因为 actor state 要连续。虽然 state 可以从 log 恢复，但同一时刻最好只有一个 worker 对某个 actor 执行命令。

在 metadata 里，actor state 保存了 `OwnerWorkerID`。在 RPC 响应和 dashboard 里，也会看到这个字段。

当 worker 心跳正常时，控制面继续把 actor task 路由给这个 owner。owner 超时后，控制面会给 actor 分配新 owner，并递增 epoch。

## Q400. epoch fencing 是为了解决什么问题？

Epoch fencing 用来挡住旧 owner。

最典型的故障是网络分区。控制面认为 worker-1 已经失联，于是把 actor 交给 worker-2，epoch 从 1 变成 2。但 worker-1 可能没有真的死，它只是暂时连不上。过一会儿它恢复网络，拿着 epoch=1 的旧 task 来提交 completion。

如果没有 fencing，旧 worker 的 completion 可能覆盖新 owner 已经推进的状态。

LogServe 的做法是：actor completion 必须带 worker_id 和 actor_epoch。控制面检查它们是否等于当前 actor 的 owner_worker_id 和 epoch。不匹配就拒绝，错误里会说明 stale actor completion rejected。

这就是 epoch fencing。它不是防止旧 worker 执行代码，而是防止旧 worker 的结果写入 actor 状态。

## Q401. actor task 为什么只能路由到 owner worker？

因为 actor 是有状态对象，同一个 actor 同一时间只能有一个合法执行者。

actor task 的 TaskSpec 里带有：

- actor_id
- actor_call_id
- actor_class_source
- actor_method
- actor_state_json
- actor_epoch
- target_worker_id

控制面 poll task 时会先检查 `actorMailboxReady`。这个函数要求当前 worker 必须等于 actor 的 owner worker，而且 command_seq 必须刚好是下一条要执行的命令。

这意味着 worker-2 不能偷走 worker-1 拥有的 actor task。只有当 worker-1 心跳超时，控制面写入新的 `ActorOwnershipGranted` 后，worker-2 才能成为新 owner。

这样做牺牲了一点负载均衡弹性，但换来 actor 状态一致性。对 actor 来说，顺序比均摊更重要。

## Q402. actor snapshot 是什么？

Actor snapshot 是某个 command_count 时刻的完整 actor state。

如果没有 snapshot，actor 恢复时要从 `ActorCreated` 开始，把所有 `ActorCommandApplied` 事件重放一遍。命令少的时候没问题。命令很多时，恢复会越来越慢。

有 snapshot 后，恢复可以从最近的 snapshot state 开始，只 replay snapshot 之后的 tail log。

在当前实现里，snapshot 内容写到 result store，actor stream 里写 `ActorSnapshotCreated`，里面保存 snapshot_ref 和 snapshot_command_count。

所以 snapshot 不是替代 log。它是 replay 加速点。log 仍然保留“这个 snapshot 对应哪个 command_count”的元信息。

## Q403. snapshot_every 如何控制 snapshot 频率？

`snapshot_every` 表示每执行多少条 actor command 生成一次 snapshot。

默认值是 25。Python SDK 的 `@actor(snapshot_every=...)` 可以设置，`CreateActorRequest` 里也有这个字段。控制面如果收到 0，会按默认值处理。

actor 命令成功应用后，控制面会检查：

```text
CommandCount % SnapshotEvery == 0
```

如果条件成立，并且 result store 可用，就把当前 `state_json` 写成 snapshot，然后 append `ActorSnapshotCreated`。

比如 `snapshot_every=10`，第 10、20、30 条命令后会尝试生成 snapshot。

频率太高会增加写 result store 的开销。频率太低会让 crash 后 replay 时间变长。实验里可以通过 `full_replay_commands` 和 `snapshot_replay_commands` 对比这个收益。

## Q404. snapshot_ref 存在哪里？

`snapshot_ref` 存两处。

第一处是 actor stream 的 `ActorSnapshotCreated` 事件。事件里有：

- snapshot_ref
- snapshot_command_count
- snapshot_every
- class_name
- class_source
- init_args_json

第二处是 metadata view 里的 actor state。`GetActorStatus` 会返回 `snapshot_ref` 和 `snapshot_command_count`。

真正的 snapshot 内容不在 log 里，而在 result store。`snapshot_ref` 是定位这个对象的引用。

这个设计和 workflow 的大结果引用类似：log 里放可恢复的引用，具体大对象放 result store。

## Q405. ReplayActor 会输出哪些信息？

`ReplayActor` 会从 actor stream 重建 actor 状态，并返回几个关键信息：

- actor_id
- class_name
- status
- owner_worker_id
- epoch
- command_count
- snapshot_ref
- snapshot_command_count
- state_json
- created_at_ms
- updated_at_ms
- consistent_with_metadata
- full_replay_commands
- snapshot_replay_commands

其中 `consistent_with_metadata` 表示 replay 出来的 state 是否和当前 metadata view 一致。

如果这个字段是 false，我会优先怀疑 metadata view 落后或更新失败，因为设计上 actor stream 才是 source of truth。

## Q406. full_replay_commands 与 snapshot_replay_commands 有什么区别？

`full_replay_commands` 是从头 replay 需要处理的命令数。它基本等于从 actor 创建以来应用过的命令数量。

`snapshot_replay_commands` 是使用 snapshot 后还需要处理的 tail command 数。比如 actor 已经执行 100 条命令，最近 snapshot 在第 90 条，那么 snapshot replay 只需要处理第 91 到 100 条，也就是 10 条。

这两个指标用来证明 snapshot 是否真的生效。

如果没有 snapshot，两者会接近。若 snapshot 生效，`snapshot_replay_commands` 应该明显小于 `full_replay_commands`。你之前实验里看到 snapshot replay commands 比 full replay commands 小，就是这个指标在起作用。

## Q407. actor owner worker 停止心跳后会发生什么？

控制面不会马上删除 actor，也不会认为 actor 状态丢了。

当下一次需要 actor owner 时，`ensureActorOwner` 会检查当前 owner 的 heartbeat。如果超过 actor owner lease，说明这个 owner 不能继续被信任。控制面会从活跃 worker 里选一个新的 worker，写 `ActorOwnershipGranted`，epoch 加 1，然后更新 metadata。

之后新的 actor task 会路由到新 owner。新 owner 执行 task 时拿到的是当前 actor state。如果 metadata state 不完整，还可以从 actor stream replay 恢复。

所以 worker 停止心跳后的核心动作是“换 owner + 增 epoch”，不是重建 actor id。

## Q408. 旧 owner 的 completion 为什么要拒绝？

因为旧 owner 的结果可能基于旧状态。

假设 worker-1 是 epoch=1 的 owner，它执行 `inc()` 时看到 value=10。后来 worker-1 失联，控制面把 actor 交给 worker-2，epoch=2。worker-2 又执行了几条命令，value 变成 15。

这时 worker-1 恢复，提交一个“value=11”的 completion。如果接受它，actor 状态会倒退。

所以 `completeActorCall` 会检查：

- completion 的 worker_id 是否等于当前 owner_worker_id
- completion 的 actor_epoch 是否等于当前 epoch

任意一个不匹配，就拒绝。这个拒绝不是为了报错好看，而是 actor 状态安全的底线。

## Q409. 1000 concurrent inc() 为什么最终结果应该是 1000？

因为这 1000 次并发调用在 actor 内部不会并发改同一个状态。

提交时，每个 `inc()` 会拿到独立的 command_seq：1 到 1000。调度时，控制面只放行当前 `command_count + 1` 的命令。执行时，worker 本地还有 per-actor lock，避免同一个 owner worker 内部多个 actor runner 同时执行同一 actor。完成时，控制面再次检查 command_seq 是否正好接上。

所以虽然客户端是并发提交的，actor 看到的是一条确定的命令序列：

```text
inc #1
inc #2
...
inc #1000
```

每条命令都从上一条命令的 state 开始执行。Counter 初始是 0，成功应用 1000 条 `inc()` 后，`get()` 应该返回 1000。

如果最终不是 1000，通常说明 mailbox 顺序、completion fencing 或 actor state 注入有 bug。

## Q410. actor runtime 和普通 task runtime 最大区别是什么？

最大区别是：普通 task 是无状态执行，actor 是有状态顺序执行。

普通 task 的核心问题是调度、执行、retry、结果写回。它不关心上一次 task 在 worker 内存里留下了什么，也不要求同一类 task 总在一个 worker 上跑。

Actor runtime 多了几层语义：

- actor_id 标识一个长期存在的对象。
- state_json 代表对象当前状态。
- mailbox 保证同一 actor 的命令串行。
- owner_worker_id 决定 actor 当前在哪个 worker 上执行。
- epoch fencing 防止旧 owner 写入。
- actor stream 记录所有状态变更。
- snapshot 降低恢复成本。

所以 actor 更接近“有恢复能力的分布式对象”。普通 task 更接近“把一个函数拿去跑一次”。

这也是 LogServe 从 workflow runtime 往 AI runtime 走时必须补上的能力。很多 AI 应用不是一串纯函数就能解释清楚，它们需要长期状态、顺序语义和故障恢复。
