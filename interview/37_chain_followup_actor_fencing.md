# 十二、高频链式追问脚本：从 actor fencing 开始

这一组问题的主线是 actor 的单对象一致性。LogServe 里 actor 有三个安全边界：owner worker 决定命令应该发到哪里，epoch fencing 决定谁有资格提交状态，command_seq 决定命令按什么顺序推进。worker 本地锁能减少同一 actor 在本机并发执行，但真正的状态机边界在 control plane 和 actor stream。

## Q856. 为什么 actor 需要 owner worker？

actor 是有内存状态的对象。以 `Counter` 为例，`inc()` 不是普通无状态函数，它要读当前 `value`，加一，再把新状态写回去。如果每次调用都随机调度到不同 worker，worker 必须在每次执行前恢复 actor state，执行后再把 state 写回来。这样能做，但代价高，也更容易出现并发写乱序。

owner worker 的作用是给 actor 一个当前执行位置。LogServe 在 actor state 里保存 `owner_worker_id` 和 `epoch`。调用 actor 时，control plane 会把 actor task 的 `target_worker_id` 设成 owner，让同一个 actor 的命令优先落在同一个 worker 上执行。

这样有几个直接好处：

- actor 的热状态可以留在 owner worker 上。
- 同一 actor 的请求不需要每次都做完整冷恢复。
- worker 本地可以按 actor_id 加锁，避免同一 actor 在本机并发执行。
- control plane 可以用 owner + epoch 判断 completion 是否过期。
- actor 迁移时有明确的 ownership 变更事件，replay 能解释状态历史。

这里的 owner 不是永久绑定。owner worker 心跳超时后，control plane 可以把 actor grant 给另一个 active worker，并递增 epoch。旧 worker 后续即使恢复，也必须通过 epoch fencing 才能提交状态。

面试里可以简洁地说：

> actor 需要 owner，因为 actor 是有状态对象。owner 让调度、缓存、顺序执行和失效转移都有一个明确锚点。

## Q857. 为什么不让任意 worker 执行 actor command？

任意 worker 都执行 actor command，会把 actor 退化成“多个 worker 共同写同一份状态”。这会破坏 actor model 的基本假设。

假设 `Counter.value = 0`，两个 worker 同时执行 `inc()`。worker A 读到 0，worker B 也读到 0，它们都写回 1。两个命令都成功了，但最终状态只有 1。对用户来说，提交了两次 inc，结果却少了一次。

还会有更隐蔽的问题。actor method 可能不是简单加法，它可能维护队列、缓存、会话状态、模型上下文、连接池。并发 worker 同时跑同一个 actor method，状态写回顺序很难解释。你可以用分布式锁解决一部分问题，但那样每次执行都要抢锁、加载状态、提交状态，性能和复杂度都变差。

LogServe 的选择是让任意 worker 执行普通 task，让 actor command 只路由到 owner worker。普通 task 关注吞吐，actor 关注单对象顺序。两类任务的约束不同。

owner worker 只是第一层约束。真正提交 actor state 时，control plane 还会检查：

- completion 的 worker_id 是否等于当前 owner_worker_id；
- completion 的 actor_epoch 是否等于当前 epoch；
- completion 的 command_seq 是否等于 `command_count + 1`。

所以“只发给 owner”是调度层防线，“只接受当前 owner 当前 epoch 的 completion”才是提交层防线。

## Q858. command_seq 如何保证 mailbox 顺序？

`command_seq` 是 actor mailbox 的逻辑序号。每次提交 actor command 时，control plane 会拿 actor 当前的 `SubmittedCommandCount` 和 `CommandCount`，计算下一个序号。新命令进入 actor stream 时，会写 `ActorCommandSubmitted`，payload 里带 `command_seq`。

执行完成后，`completeActorCall` 会检查：

```text
completion.command_seq == actor.command_count + 1
```

只有满足这个条件，状态才能被 apply。

这条检查解决的是完成乱序。比如 command 1 和 command 2 都已经提交，worker 因为执行时间不同，command 2 先完成。没有 `command_seq` 时，command 2 可能先把状态写进去，然后 command 1 又覆盖它。加上 `command_seq` 后，当前 `command_count` 还是 0，command 2 的 seq 是 2，不等于 1，所以会被拒绝或等待后续重试路径处理。command 1 成功 apply 后，`command_count` 变成 1，command 2 才有资格推进。

LogServe 还在 poll 侧做了额外保护。测试里有一个场景：队列里只有 seq=2 的 actor task，但 actor 的 `CommandCount` 还是 0，`PollTask` 会跳过它，不会提前派发。这样可以减少无效执行。

所以 mailbox 顺序靠两处机制落地：

- 提交时分配单调递增 `command_seq`。
- 完成时只接受下一个期望序号。

## Q859. worker 本地锁和 control mailbox 谁才是核心保证？

核心保证在 control mailbox，也就是 control plane 里的 command_seq、owner、epoch 和 actor stream。worker 本地锁只是执行侧优化。

worker 本地锁的价值是很实际的。一个 worker 可能有 actor executor pool，pool 里多个 goroutine 或多个 Python runner 并行执行任务。如果同一个 actor 的两个 command 同时进入这个 worker，本地按 actor_id 加锁，可以避免它们在同一进程里并发跑同一个 actor 对象。

但本地锁有明显边界：

- 它只在单个 worker 进程内有效。
- worker 崩溃后锁就没了。
- 网络分区时，旧 worker 的本地锁无法约束新 owner。
- 两个 worker 上的本地锁互相不知道对方存在。

control mailbox 的约束不依赖 worker 内存。`ActorCommandSubmitted`、`ActorCommandApplied` 写入 actor stream；`command_seq` 记录命令顺序；`epoch` 记录 ownership 代次；replay 时可以从这些事件还原 actor 状态。

所以我会这样回答：

> 本地锁让执行更干净，control mailbox 才定义正确性。没有本地锁，系统可能多做无效执行；没有 control mailbox，系统就无法证明 actor 状态按顺序推进。

## Q860. old owner 在网络分区后恢复，为什么不能继续提交状态？

因为 actor ownership 已经可能转移了。

典型过程是这样的：worker-1 是 actor owner，epoch=1。后来 worker-1 和 control plane 之间网络断开，control 收不到心跳。超过 `actorOwnerLease` 后，control 认为它不可用，于是把 actor grant 给 worker-2，并把 epoch 提升到 2。

这时 worker-1 可能还在本地执行旧 command。网络恢复后，它试图提交结果。如果系统只看 actor_id，不看 owner 和 epoch，worker-1 的旧结果就可能覆盖 worker-2 已经推进的新状态。

LogServe 用 epoch fencing 处理这个问题。`CompleteTaskRequest` 里带 `worker_id` 和 `actor_epoch`。control plane 当前 actor state 里也有 `owner_worker_id` 和 `epoch`。`completeActorCall` 会比较两者：

```text
req.worker_id == state.owner_worker_id
req.actor_epoch == state.epoch
```

只要 worker 或 epoch 不匹配，就拒绝 completion，返回 stale actor completion rejected。旧 owner 恢复后可以重新参与集群，继续作为普通 worker 接任务，但不能拿旧 epoch 的执行结果改 actor state。

这就是 fencing token 的意义：旧 worker 可能还在运行代码，但它不能提交过期结果。

## Q861. epoch fencing 如何防止 stale completion？

epoch 可以理解成 actor ownership 的代次。每次 control plane 重新 grant owner，epoch 都递增。actor task 被创建时，会把当时的 epoch 写进 `TaskSpec.ActorEpoch`；worker 完成时，再把这个 epoch 放进 `CompleteTaskRequest.ActorEpoch`。

提交状态时，control plane 只接受当前 epoch 的 completion。代码路径在 `completeActorCall`：

```text
if req.worker_id != state.owner_worker_id || req.actor_epoch != state.epoch:
    reject stale actor completion
```

举个例子：

- worker-1 拿到 actor，epoch=1。
- worker-1 执行 command 10。
- worker-1 心跳断开。
- control grant worker-2，epoch=2。
- worker-2 执行并提交 command 10。
- worker-1 恢复，也提交 command 10，但请求里还是 epoch=1。
- control 看到当前 actor epoch=2，拒绝 worker-1 的 completion。

这套机制还和 command_seq 配合。epoch 解决“谁有资格提交”，command_seq 解决“提交的是不是下一个命令”。一个 completion 必须同时满足 owner、epoch、seq 三个条件，才能写 `ActorCommandApplied`。

这比单纯依赖心跳时间更可靠。心跳只能告诉你“我认为某个 worker 可能失联”，epoch 才能在旧 worker 重新出现时裁决它的结果是否还有效。

## Q862. 如果两个 control 同时 grant owner，epoch 还安全吗？

在当前单 control 设计里，这个问题不会发生。LogServe 现在的控制面不是 active-active，`ensureActorOwner` 由单个 control 实例执行，actor state 和 actor stream 的更新都走这条路径。

如果未来部署多个 active control，单靠本地 `state.Epoch + 1` 就不够安全。两个 control 可能同时读到 epoch=1，然后都决定 grant 新 owner，并各自写 epoch=2。结果就是 split-brain：两个 worker 都拿到了看似合法的 epoch。

要让 epoch 在 active-active 下安全，需要把 grant owner 做成线性化操作。可以有几种方案：

- 通过 Raft-backed shared log 让 `ActorOwnershipGranted` 串行提交，由 log leader 分配 epoch。
- 在 metadata store 里对 actor row 做 CAS：`UPDATE actors SET epoch=epoch+1, owner=? WHERE actor_id=? AND epoch=?`。
- 用 etcd/ZooKeeper 这类带 revision 的 lease，把 revision 当 fencing token。
- 让 scheduler/control 只有一个 leader，所有 ownership grant 都由 leader 发起。

不管选哪种，核心条件都一样：同一个 actor 的 ownership grant 必须有全局单调 token，不能由多个 control 在本地各自加一。

所以回答要坦白：

> 当前 epoch fencing 在单 control 假设下成立。active-active control 要再引入 leader election、CAS 或 Raft log，否则两个 control 同时 grant owner 时会产生相同 epoch。

## Q863. snapshot 为什么能降低 replay 成本？

actor replay 的成本主要来自重放 command log。一个 actor 如果执行了 100 万次 command，而系统每次恢复都从 `ActorCreated` 开始读，再逐条 apply `ActorCommandApplied`，恢复时间会越来越长。

snapshot 把某个 command_count 时刻的 actor state 存成对象。之后 replay 可以这样走：

1. 找到最新的 `ActorSnapshotCreated`。
2. 从 `snapshot_ref` 读取 state_json。
3. 把 `CommandCount` 设成 `snapshot_command_count`。
4. 只重放 snapshot 之后的 tail log。

比如 Counter 已经执行了 100 次 inc，snapshot_command_count=100。重启后不用再从 0 开始 apply 100 次 inc，直接加载 value=100 的 snapshot，再处理后续新 command。

项目实验里也验证了这个方向：actor snapshot ablation 里，snapshot replay 的 command 数明显少于 full replay。之前实验结果里有 `snapshot_replay_commands=1`、`full_replay_commands=21` 这种对比，说明 snapshot 已经把大部分历史折叠掉了。

snapshot 的价值不只在性能。它还给 logical trim 提供依据。`ActorSnapshotCreated` 写入 log 后，系统可以 `TrimStream(before_seq=snapshot_seq)`，让默认 replay 读路径跳过更早事件。磁盘不一定立刻释放，但恢复路径已经少读少算。

## Q864. snapshot 写入和 log append 之间如何保证一致性？

当前实现采用“先写 snapshot object，再写 `ActorSnapshotCreated` 事件”的顺序。

原因是 replay 看到 `ActorSnapshotCreated` 后，会根据 `snapshot_ref` 去 result store 读取对象。如果先写 log，再写对象，中间崩溃时，replay 会看到一个指向不存在对象的 snapshot_ref，恢复会失败。先写对象，再写日志，至少能保证：只要 log 里出现 snapshot 事件，对象大概率已经存在。

这条路径仍然有一个边界：如果 object store Put 成功，但 `ActorSnapshotCreated` append 失败，会留下孤儿 snapshot 对象。这个对象没有被 log 引用，replay 不会使用它。它浪费存储，但不会破坏状态正确性。后续可以用 mark-and-sweep 清理：扫描 log 里仍被引用的 snapshot_ref，删除没有引用的对象。

如果 `ActorSnapshotCreated` append 成功，但 metadata 更新失败，重启后可以从 actor stream replay 出 snapshot metadata。因为 log 是事实来源，metadata view 只是投影。

当前实现还会在 snapshot event append 成功后调用 `TrimStream`，把 actor stream 的可 replay 起点推进到 snapshot event 附近。这个 trim 是逻辑 trim，失败时只记录错误，不影响 snapshot 本身的正确性。

更强的生产方案可以增加：

- snapshot object checksum；
- snapshot_ref 带内容 hash；
- object store Put 幂等；
- snapshot manifest；
- compaction 前验证 snapshot 可读；
- 后台清理孤儿对象。

面试里可以这样收束：

> 顺序上先对象后日志。对象成功、日志失败只产生孤儿对象；日志成功、对象不存在会破坏 replay，所以要避免。

## Q865. 如果 actor 状态很大，你的 state_json 方案还能撑住吗？

撑不久。`state_json` 适合 Counter 这类小状态，或者面试 demo 里的轻量 actor。状态变大后，它会暴露几个问题。

第一，每次 actor command completion 都可能带完整 `actor_state_json`。如果 actor state 有几十 MB，每次方法调用都把完整状态从 worker 传回 control，再写入 `ActorCommandApplied` payload，网络、日志、metadata 都会被放大。

第二，snapshot 也会变重。大 state 写 result store 是对的，但 snapshot 写入、读取、校验都会影响 failover 时间。对象存储延迟高时，actor 接管会变慢。

第三，日志膨胀。当前 `ActorCommandApplied` 记录里包含 `state_json`。这让 replay 简单：不用重新执行 actor method，直接把事件里的 state_json 当作新状态。但大状态下，每个 command 都写完整状态，空间成本很高。

我会把后续设计改成分层状态：

- 小状态继续 inline JSON，简单直接。
- 中等状态写 `state_ref`，log 里放 ref、hash、size。
- 大状态做增量 snapshot，只记录 changed keys 或 patch。
- 对 map/list 这类结构做分片，按 shard 写 snapshot。
- command log 里记录 method、args、result、state_delta，而不是完整 state。
- actor runtime 限制单次 state 大小，超过阈值要求用户改成外部 state store。

这里要注意一个取舍。如果日志里只记录 delta，replay 需要按顺序 apply delta；如果只记录 method 和 args，replay 可能要重新执行用户代码，那会引入非确定性问题。当前项目选择写完整 state_json，是为了让 replay 成为事件重放，不重新跑用户函数。它牺牲空间，换来了恢复语义简单。

所以我会这样回答：

> 当前 state_json 方案适合小到中等 actor 状态。大状态场景要引入 state_ref、增量 snapshot、分片状态和大小限制。不能让每次 command 都把完整大对象塞进日志。
