# 六、Actor Runtime、Mailbox、Snapshot、Ownership 与 Epoch Fencing（深度）

## Q411. submitActorCommand 为什么要对单个 actor 加短锁？

`submitActorCommand` 里对单个 actor 加短锁，是为了保护 command_seq 分配和命令提交这段临界区。

同一个 actor 可能同时收到很多 `CallActor`。如果不加锁，两个请求可能同时读到相同的 `SubmittedCommandCount` 和 `CommandCount`，然后都算出同一个 next command_seq。这样 mailbox 顺序就坏了。

这把锁保护的不是 actor method 执行。它只包住这些动作：

- 确认 actor owner。
- 必要时 replay actor state。
- 计算下一个 command_seq。
- 写 `ActorCommandSubmitted`。
- 创建 actor task。
- 更新 `SubmittedCommandCount`。

所以它应该是短锁。`CallActor` 后续同步等待结果时不能继续占着这把锁，否则第一个 actor call 没返回，后面的 call 都无法提交。当前测试里也专门覆盖了这一点：前一个调用还没完成时，第二个调用仍然可以提交 `ActorCommandSubmitted`，只是会在 mailbox 里按顺序等执行。

## Q412. SubmittedCommandCount 和 CommandCount 分别表示什么？

`SubmittedCommandCount` 表示已经提交到 actor mailbox 的最大 command_seq。

`CommandCount` 表示已经被 actor 状态机应用的最大 command_seq。

这两个数字不一定相等。比如 actor 当前已经应用到 10，但客户端连续提交了 11、12、13，这时：

```text
CommandCount = 10
SubmittedCommandCount = 13
```

这说明 11 到 13 已经进入队列，但还没有全部执行。

为什么不能只保留 CommandCount？因为 `CallActor` 可能超时。客户端超时不代表命令没有提交。如果下一次提交还只看 CommandCount，就可能重复分配 seq=11。`SubmittedCommandCount` 记录的是“已经占用过的序号”，避免超时调用把序号打乱。

Replay 时也会分别恢复这两个值。`ActorCommandSubmitted` 推进 SubmittedCommandCount，`ActorCommandApplied` 或 `ActorCommandFailed` 推进 CommandCount。

## Q413. command_seq 如何分配？

分配逻辑在 `submitActorCommand` 里。

控制面先读 actor state，然后取：

```text
submittedCount = max(SubmittedCommandCount, CommandCount)
commandSeq = submittedCount + 1
```

这样做是为了覆盖几种情况：

- 正常情况下，SubmittedCommandCount >= CommandCount，下一条命令接在已提交命令后面。
- 如果 replay 或恢复后 CommandCount 更大，就不能用旧的 SubmittedCommandCount。
- 如果前一条命令已经提交但客户端等待超时，下一条命令仍然要拿新的 seq。

分配完成后，控制面先写 `ActorCommandSubmitted`，然后创建 actor task，并把这个 command_seq 写入 task metadata。之后 `PollTask` 和 completion 校验都依赖这个字段。

## Q414. 为什么 completion.command_seq 必须等于 actor.command_count + 1？

这是 actor 状态顺序的底线。

Actor 的状态必须一条命令一条命令往前推进。假设当前 `CommandCount=5`，说明 actor state 已经包含前 5 条命令的效果。下一条能改变状态的命令只能是 seq=6。

如果 seq=7 先完成，不能直接应用。因为 seq=7 执行时应该看到 seq=6 之后的状态，而不是 seq=5 的状态。

所以 `completeActorCall` 会检查：

```text
commandSeq == state.CommandCount + 1
```

不满足就拒绝，报 out-of-order actor command。这个检查即使有 mailbox 也不能省。队列、worker、本地执行都可能因为恢复或重试出现乱序，最终写状态前必须再守一次门。

## Q415. 如果 command 2 比 command 1 先完成，系统如何处理？

正常调度下，command 2 不应该先被 dispatch。`actorMailboxReady` 会检查 command 2 的 seq 是否等于 `CommandCount + 1`。如果 command 1 还没应用，CommandCount 还是 0，seq=2 的 task 会被 poll 跳过。

但系统要防更坏的情况，比如恢复时队列里已经有旧 task，或者某个 worker 异常提交了 seq=2 的 completion。这个时候 `completeActorCall` 还会检查 command_seq。如果当前只应用到 0，seq=2 completion 会被拒绝。

所以处理方式不是“缓存 command 2 的完成结果等 command 1”，而是直接拒绝乱序 completion。这样状态机简单，语义也清楚。worker 或客户端可以通过 redelivery/retry 重新走正确顺序。

## Q416. control-plane mailbox 和 worker-side per-actor lock 是否重复？为什么都需要？

不重复。它们挡的是不同层面的并发。

Control-plane mailbox 决定“哪条 actor command 现在可以被调度”。它看的是全局 actor state、owner worker 和 command_seq。这个判断在控制面做，目的是防止 command 2 在 command 1 之前被交给 worker。

Worker-side per-actor lock 决定“同一个 worker 本地是否可以并发执行同一 actor”。worker 有 actor executor pool，里面可能有多个 goroutine。如果没有 per-actor lock，两个已经进入本地 actorQueue 的同 actor task 仍可能并发跑。

两者关系是：

- 控制面保证跨 worker、跨队列的顺序。
- worker 本地锁保证同一个进程内部的顺序。
- completion 校验保证写状态前的最后防线。

只有控制面 mailbox，没有 worker lock，本地 executor pool 仍可能乱。只有 worker lock，没有控制面 mailbox，错误 worker 或未来 command 仍可能被提前投递。

## Q417. actor task poll 时为什么要填充最新 actor state、owner、epoch？

actor task 提交时会带一份 `ActorStateJson`，但这份 state 可能已经过期。因为命令可以先提交到队列，等前面的命令执行完以后才轮到它。

比如 seq=2 提交时，actor state 还是 value=0。seq=1 执行后，state 变成 value=1。等 seq=2 真正被 poll 出来时，它必须拿 value=1，而不是提交时那份旧 state。

所以 `leasedTaskSpec` 会在 poll 成功后重新读取 actor metadata，把最新的字段填进 TaskSpec：

- `TargetWorkerId`
- `ActorEpoch`
- `ActorStateJson`

这一步很重要。它让 actor task 执行时看到的是“即将执行前”的最新状态，而不是“命令提交时”的状态。

## Q418. actor state 放在 TaskSpec 中有什么风险？

把 actor state 放进 TaskSpec 简单直接，worker 不需要再去查控制面或对象存储，拿到 task 就能执行。

风险也很明显。

第一，state 可能变大。TaskSpec 会经过 gRPC、metadata、worker 队列，状态越大，传输和内存压力越高。

第二，state 是一个快照。如果 task 在队列里待了很久，提交时携带的 state 可能过期。当前实现通过 poll 时重填最新 state 来缓解这个问题。

第三，state 可能包含敏感内容。把完整 state 放进 TaskSpec，会扩大暴露面。日志、debug 输出、dashboard 都要避免把它随便打印出来。

第四，TaskSpec 中的 state 不是 source of truth。真正可信的是 actor stream 和 snapshot。TaskSpec 只是执行时的输入材料。

## Q419. 如果 actor state 很大，每次 task 都带 state 会有什么问题？

主要问题是读写放大。

每次 actor method 调用都携带完整 state，会导致：

- gRPC payload 变大。
- metadata 存储压力增加。
- worker 本地队列占用更多内存。
- Python runner 每次反序列化完整 state。
- `ActorCommandApplied` 如果也写完整 state_json，actor stream 会膨胀。

如果 actor state 达到几 MB 甚至几十 MB，这个模式就不合适了。

更好的方式是：

- 小状态继续 inline。
- 大状态放 result store，只在 TaskSpec 中放 state_ref。
- actor method 执行时按需加载 state。
- `ActorCommandApplied` 记录 state_ref 和 state hash。
- snapshot 存完整 state，tail log 只存增量或引用。

当前实现适合中小状态 actor。大状态 actor 需要引入 state ref 或增量日志。

## Q420. 如果 actor method 执行时间很长，后续命令如何排队？

后续命令可以继续提交，但不会越过正在执行的命令。

具体来说，`CallActor` 会先进入 `submitActorCommand`，拿到 command_seq，并写 `ActorCommandSubmitted`。如果前一条命令还没完成，后续命令会留在 task queue 中。

`PollTask` 会调用 `actorMailboxReady`。只有 `command_seq == CommandCount + 1` 的命令才会被 worker 拿走。长时间运行的 command 1 没完成时，command 2、3、4 都不会被提前执行。

这会带来一个现实问题：单个 actor 的吞吐被最慢命令限制。如果 actor method 经常跑很久，后面的调用会堆积。解决办法不是让同一 actor 并发执行，而是调整建模：拆 actor、把耗时工作放普通 task、actor 只保存状态和发起任务。

## Q421. CallActor 同步等待结果会带来什么伸缩性问题？

当前 `CallActor` 是同步等待：提交 actor command 后，控制面循环查 task 状态，直到成功、失败或超时。

这对 demo 和简单客户端很方便，但伸缩性一般。

问题包括：

- 控制面 goroutine 被长时间占用。
- 客户端连接也要一直保持。
- 大量慢 actor call 会增加 control plane 的内存和调度压力。
- 如果客户端超时断开，命令可能仍在系统里继续执行。

更适合生产的接口是异步模式：

1. `SubmitActorCommand` 立即返回 call_id。
2. 客户端用 `GetActorCallStatus(call_id)` 查询。
3. 或者通过 stream/watch/webhook 收到完成通知。

当前同步 `CallActor` 可以保留作为易用 API，但内部最好有异步基础模型。实际上当前事件和 task id 已经接近这个方向，缺的是对外暴露更完整的异步 RPC。

## Q422. 如果 CallActor 客户端超时但命令后来执行成功，语义是什么？

客户端超时只表示“客户端没有在等待窗口内拿到结果”，不表示命令取消。

如果 `ActorCommandSubmitted` 已经写入 actor stream，命令就已经进入 mailbox。后面 worker 仍可能执行它。执行成功后，控制面会写 `ActorCommandApplied`，actor state 也会推进。

这就是为什么 `SubmittedCommandCount` 很重要。即使客户端超时，已经提交的 command_seq 也不能重新分配给下一条命令。

对用户来说，语义是：

- CallActor 返回 timeout，结果未知。
- 如果使用了 idempotency_key，可以重试同一个调用，查询或复用已有 task。
- 如果不用 idempotency_key，重试可能提交一条新命令。

所以有副作用或重要状态变更的 actor call，客户端应该总是带稳定 idempotency_key。

## Q423. idempotency_key 对 actor call 有什么意义？

它让客户端重试安全。

网络请求可能超时，客户端不知道控制面是否已经收到 actor call。如果重试没有 idempotency_key，控制面会把它当成新命令，分配新的 command_seq。对 `inc()` 这种方法来说，重复提交就会多加一次。

带 idempotency_key 后，控制面可以用这个 key 查已有 task。如果同一个 key 的 fingerprint 一致，就返回已有任务，而不是创建新 actor command。

这对 actor 尤其重要，因为 actor call 通常会改变状态。普通查询重复执行可能只是浪费资源，actor mutation 重复执行会改变结果。

## Q424. 如果同一 actor call 重复提交，如何避免重复应用？

当前路径主要靠 task idempotency。

`submitActorCommand` 会把请求里的 idempotency_key 放进 TaskSpec。如果 metadata 里已经有同 key task，控制面会校验 fingerprint。一致就复用已有 task，不再写新的 `ActorCommandSubmitted`，也不再分配新的 command_seq。

如果没有传 idempotency_key，系统会用新生成的 call_id 作为 key。这样单次请求内部有唯一性，但客户端重试不会自动合并。

完成阶段也有保护。`ActorCommandApplied` 的 append key 是 `actor_id + actor_call_id + applied`。同一个 call_id 的 applied 事件重复 append，会被 logstore 去重。

要注意一个边界：如果客户端用同一个 idempotency_key 提交不同 method 或不同 args，应该报冲突，而不是复用旧结果。当前 fingerprint 校验就是为了防这个问题。

## Q425. ActorCommandApplied 的 idempotency key 为什么包含 actor_id 和 actor_call_id？

因为 actor command 的成功应用应该按“某个 actor 的某次 call”去重。

`actor_call_id` 标识一次 actor method 调用。`actor_id` 标识它属于哪个 actor。两者组合起来，能明确表示“这个 actor 的这次调用已经应用过”。

如果 completion 因为网络重试重复到达，worker 可能再次尝试 append `ActorCommandApplied`。相同 idempotency key 会让 logstore 返回 duplicate，不会写第二条 applied 事件。

不用单独的 command_seq 作为 key，是因为 command_seq 是 actor 内序号，语义上也能标识顺序，但 call_id 更贴近请求身份。command_seq 主要用来保证顺序，call_id 主要用来标识一次调用。

## Q426. ActorCommandFailed 是否应该增加 command_count？为什么？

当前实现会增加。

原因是失败命令也占用了 mailbox 的一个位置。比如 seq=5 的命令执行失败，如果不推进 CommandCount，seq=6 永远不能执行，因为系统一直在等 seq=5。

所以 `ActorCommandFailed` 会把 `CommandCount` 推到 command_seq。它表示这条命令已经结束，只是没有产生成功的 state mutation。

这有一个语义选择：失败是否改变 actor state？当前实现里失败不会写新的 state_json，但会推进命令序号。也就是说，它不改变业务状态，但会改变 actor 的执行历史。

如果以后需要 retry actor method，就要更细地设计：某些失败是否占用序号，是否重试同一序号，还是创建新序号。当前实现选择简单可恢复的模型：每条提交命令最终成功或失败，都会让 mailbox 往前走。

## Q427. actor failure 是否会改变 actor state？

当前失败路径不会更新 `StateJSON`。

`failActorCommand` 会写 `ActorCommandFailed`，记录 method、args、error、worker、epoch 和 command_count，然后更新 actor 的 CommandCount。它不会把 worker 返回的 actor state 写进 metadata。

这样做比较安全。方法失败时，用户代码可能执行到一半，状态是否可信不好判断。直接保存失败时的中间状态，可能把不完整状态写入 actor。

但失败会改变 actor 的历史位置：CommandCount 会前进。后面的命令可以继续执行。

如果某些 actor method 希望失败也保留状态，比如“尽力处理一部分”，应该由方法自己返回成功结果并显式写状态，而不是走 failed completion。

## Q428. actor snapshot 在什么时机创建？

成功应用 actor command 后创建。

流程是：

1. worker 执行 actor method，返回 result 和新的 state_json。
2. 控制面写 `ActorCommandApplied`。
3. metadata 更新 CommandCount 和 StateJSON。
4. 如果 `SnapshotEvery > 0` 且 `CommandCount % SnapshotEvery == 0`，调用 `createActorSnapshot`。

失败命令当前不会触发 snapshot，因为失败不会更新 state_json。

也就是说 snapshot 是基于成功应用后的稳定 state 生成的，不是在命令开始前生成，也不是定时后台生成。

## Q429. createActorSnapshot 为什么先写 result store，再 append ActorSnapshotCreated？

因为 `ActorSnapshotCreated` 事件里要保存 `snapshot_ref`。没有先写 result store，就没有可引用的对象位置。

正确顺序是：

1. 把当前 actor state 写入 result store。
2. 拿到 snapshot_ref。
3. append `ActorSnapshotCreated`，把 ref 和 snapshot_command_count 写进 actor stream。
4. 更新 metadata 的 snapshot_ref。

这还是 log-first 思路。metadata 可以失败，但只要 `ActorSnapshotCreated` 进了 log，重启 replay 就能恢复 snapshot 元信息。

这个顺序的代价是可能出现孤儿 snapshot object。也就是对象写成功了，但事件没写成功。它不影响状态正确性，只会浪费存储，需要后台清理。

## Q430. 如果 snapshot object 写成功但 ActorSnapshotCreated append 失败，会发生什么？

会留下一个没有日志引用的 snapshot object。

因为 `ActorSnapshotCreated` 没写成功，replay 时完全不知道这个 snapshot 存在。系统会继续从更早的 snapshot 或完整 tail log 恢复 actor。

这类对象叫孤儿对象。它不应该被当成有效 snapshot，因为 source of truth 里没有记录它。

处理方式是对象存储 GC：

- snapshot 路径带 actor_id 和时间或随机对象名。
- 定期扫描 actor stream 中仍被引用的 snapshot_ref。
- 对没有任何事件引用、且超过安全时间的对象做清理。

不要反过来因为对象存在就补写日志。除非有明确的恢复工具和人工确认，否则对象存储不能反向改 source of truth。

## Q431. 如果 ActorSnapshotCreated append 成功但 metadata 更新失败，重启能否恢复？

可以恢复。

`ActorSnapshotCreated` 已经写入 actor stream，里面有 snapshot_ref、snapshot_command_count、class source、init args 等信息。重启时 `bootstrapActors` 会读取 `actor:` stream，调用 actor replay。

Replay 看到 snapshot 事件后，会通过 `LoadResult(snapshot_ref)` 读取 snapshot state，再 replay snapshot 之后的 tail log。最后把恢复出的 actor state upsert 到 metadata。

所以 metadata 更新失败不是致命问题。这正是 log-first 的价值：metadata 是 view，actor stream 才是事实来源。

## Q432. snapshot 后调用 TrimStream(before_seq=snapshot_seq) 的意义是什么？

它是在做 logical trim。

actor snapshot 已经包含了 snapshot_command_count 时刻的完整 state。理论上，replay 不再需要 snapshot 之前的所有 command 事件。`TrimStream(before_seq=snapshot_seq)` 的意思是：这个 stream 在默认读取时，可以隐藏 snapshot_seq 之前的记录。

这样做的直接收益是：

- replay 读取的 tail log 更短。
- dashboard 可以看到 compactable records/bytes。
- 长期运行的 actor stream 不会在默认路径里无限增长。

当前 trim 是 logical trim，不一定马上释放磁盘空间。它先改变读取视图和统计，后续 physical compaction 才真正删 segment。

## Q433. 为什么 tail log 需要从 ActorSnapshotCreated 开始保留？

因为 snapshot 事件本身告诉 replay 该从哪里恢复。

如果把 `ActorSnapshotCreated` 也隐藏了，默认 replay 只看到 snapshot 之后的 command，却不知道 snapshot_ref 是什么，也不知道 snapshot_command_count 是多少。这样就没法从 snapshot state 开始，只能报缺 create event 或状态不完整。

所以 trim 的边界要保留 snapshot event 本身。当前实现调用的是 `TrimStream(before_seq=snapshotResp.Seq)`，也就是隐藏 snapshot seq 之前的记录，保留 snapshot seq 及之后的记录。

这点很细，但很关键。snapshot 不是单独存在的，它需要日志里的 checkpoint metadata 来解释。

## Q434. logical trim 后 full replay 还能看到 snapshot 之前的事件吗？

默认 `ReadLog` 看不到。

logstore 的 `Read` 会检查 stream 的 trim point。如果请求的 from_seq 小于 trimBefore，就从 trimBefore 开始读。也就是说，logical trim 后，默认读取路径会隐藏 trim point 之前的事件。

所以这里的 “full replay” 要分清楚语境：

- 在未 trim 的完整 stream 上，full replay 可以从 ActorCreated 开始。
- 在 logical trim 后的默认读取上，所谓 full replay 也只能从 trim point 开始。

当前 actor replay 已经按 snapshot-aware 模型设计：保留 `ActorSnapshotCreated`，然后从 snapshot 加 tail log 恢复。

如果要做真正审计，不能只依赖默认 ReadLog。需要增加能读取 trimmed 历史的审计接口，或者把审计日志另存一份。

## Q435. ReadLog 默认隐藏 trim point 之前记录会不会影响审计？

会影响。

对 replay 来说，隐藏旧记录是好事，因为 snapshot 已经够恢复状态。对审计来说，旧记录可能还很重要。用户可能想知道 actor 第 3 次命令是谁发的、参数是什么、什么时候执行的。

如果默认 `ReadLog` 隐藏 trim point 之前的事件，审计工具就看不到完整历史。

这不是 bug，而是 API 语义要分开：

- replay read：只读恢复状态需要的最小日志。
- audit read：读完整历史，或者读归档历史。

当前实现更偏 replay 和 retention。后续如果要强调审计能力，就要给 ReadLog 增加模式，不能让 dashboard 或审计报表误以为默认 read 就是全量历史。

## Q436. 如果要支持审计读和 replay 读两种模式，ReadLog API 如何设计？

可以给 ReadLog 增加一个 read_mode。

例如：

```text
READ_MODE_REPLAY
READ_MODE_AUDIT
```

`REPLAY` 是默认模式，遵守 trim point，只返回恢复状态需要的记录。

`AUDIT` 尽量返回完整历史。这里要看底层是否还保留旧 segment。如果只是 logical trim，旧记录还在磁盘上，可以读出来。如果 physical compaction 已经删除，就只能去归档存储查。

还可以更明确一点：

- `ReadLog(stream_id, from_seq, limit)` 默认 replay 语义。
- `ReadAuditLog(stream_id, from_seq, limit, include_trimmed=true)` 用于审计。
- `ArchiveLog` 或 `ExportLog` 用于导出长期审计数据。

关键是不要把 retention 和 audit 混成一个接口。恢复系统状态和满足审计追溯，是两种不同需求。

## Q437. actor owner lease 设置为 750ms 是否合理？受哪些因素影响？

750ms 对本地实验是合理的，因为 worker、control、logd 都在单机或低延迟环境，heartbeat 抖动小，快速 failover 可以让故障恢复测试更明显。

但生产环境不能直接照搬。

owner lease 受这些因素影响：

- worker heartbeat interval。
- control 和 worker 之间的网络延迟。
- GC pause 或 Python executor 卡顿。
- control plane 的调度延迟。
- 机器负载。
- 是否跨机房。
- actor method 平均执行时间。

lease 太短，容易误判 worker 失联，导致 owner 频繁迁移。lease 太长，真实故障后恢复慢。

更稳的设置方式是：lease 至少大于几倍 heartbeat interval，再加上 p99 网络延迟和调度抖动。比如 heartbeat 每 1 秒一次，lease 可能要 5 到 10 秒，而不是 750ms。

所以我会说当前 750ms 是实验参数，不是生产默认值。

## Q438. worker heartbeat 延迟导致误转移 owner 会发生什么？

如果 heartbeat 延迟超过 owner lease，control 会认为旧 owner 失联，然后给 actor 分配新 owner，epoch 加 1。

如果旧 worker 其实还活着，它可能继续执行旧 task。但它提交 completion 时带的是旧 epoch。控制面看到当前 actor epoch 已经变了，会拒绝这个 completion。

结果是：

- actor 状态不会被旧 worker 写坏。
- 旧 worker 做的那次执行会浪费。
- 如果误转移频繁，系统吞吐会下降，日志里会出现更多 ownership granted 和 stale completion。

这说明 epoch fencing 能保护正确性，但不能消除误判成本。要降低误判，就要调大 lease、改进 heartbeat、区分 worker 进程卡顿和网络分区。

## Q439. 旧 worker 和新 worker 同时认为自己是 owner 时，epoch fencing 如何裁决？

裁决权在 control plane 当前 metadata 和 actor stream。

假设旧 worker 拿的是 epoch=1，新 worker 拿的是 epoch=2。两边都执行了 actor method，并都来提交 completion。

控制面处理 completion 时检查：

```text
request.worker_id == current.owner_worker_id
request.actor_epoch == current.epoch
```

只有匹配当前 owner 和 epoch 的 completion 能通过。旧 worker 即使完成得更早，只要 epoch 不匹配，也会被拒绝。

这就是 fencing 的本质：不争论谁“以为自己是 owner”，只看谁持有最新 epoch。

## Q440. 如果两个 control 实例同时 grant ownership，会不会产生 split-brain？

当前单 control 假设下不会。但如果直接跑两个 active control，又没有 leader election 或事务保护，就有风险。

两个 control 可能同时读到旧 actor state，比如 epoch=1，然后各自选择不同 worker，分别写 `ActorOwnershipGranted(epoch=2)`。如果 logstore 对同一 stream 只提供 append 顺序，但不提供 compare-and-set 语义，那么 stream 里可能出现两个 epoch=2 的 ownership 事件。

这会让 replay 很难判断哪个是合法 owner。最后一个事件可能覆盖前一个，但两个 control 都可能已经发出了任务。

要支持多 control，需要额外机制：

- control leader election。
- actor ownership grant 使用 CAS：只有当前 epoch 仍等于预期值才能写。
- 或者 log append 支持 expected_seq / expected_epoch。
- ownership epoch 必须全局单调，不能两个控制面生成同一个 epoch。

所以当前 actor ownership 安全建立在单 active control 上。多控制面是后续高可用改造点。

## Q441. actor ownership selection 为什么选择 sorted active worker 的第一个？是否公平？

当前实现用 sorted active worker 的第一个，主要是简单和确定。

确定性选择有好处：测试稳定，replay 和 debug 也容易理解。只要活跃 worker 列表一样，选择结果就一样。

但它不公平。排序第一个 worker 可能拿到太多 actor，后面的 worker 很闲。actor 数量一多，热点会明显。

更公平的策略可以是：

- 选择 actor 数最少的 worker。
- 按 worker load 打分。
- consistent hashing，把 actor_id 映射到 worker。
- 考虑 worker 标签、CPU、内存、本地缓存。
- 对高负载 owner 做迁移。

当前策略适合实验，不适合大规模多 actor 负载均衡。

## Q442. actor 迁移时是否需要把 state 主动传给新 worker？

当前不需要主动传。

控制面在 worker poll actor task 时，会把最新 `ActorStateJson` 填进 TaskSpec。新 owner 拿到 task 后，直接用这份 state 初始化 Python actor 对象并执行方法。

如果 metadata 里的 StateJSON 不完整，`submitActorCommand` 里也有一段保护：当 CommandCount > 0 但 StateJSON 为空时，会先 replay actor，把 state 补回来。

所以当前模型是“按 task 携带 state”，不是“迁移时把 actor 内存搬过去”。

这种方式简单，适合单机实验和中小状态。大状态场景下，更好的方式是让新 worker 按 snapshot_ref 拉取状态，TaskSpec 只带引用。

## Q443. 如果 actor state 只能从 snapshot store 读取，新 worker 如何获取最新状态？

需要两步。

第一步，新 worker 或控制面从 snapshot_ref 读取 snapshot state。这个 state 对应 snapshot_command_count。

第二步，读取 actor stream 中 snapshot 之后的 tail log，把后续 `ActorCommandApplied` 事件依次应用，得到最新 state。

也就是：

```text
latest_state = snapshot_state + tail_log_after_snapshot
```

当前实现里 replayActor 已经是这个思路。`ActorSnapshotCreated` 事件提供 snapshot_ref，`Store.LoadResult` 读取 snapshot 内容，后面的 applied/failed event 推进 command count。

如果要让 worker 自己加载，就要把 snapshot_ref、tail start seq 或 state hash 放进 TaskSpec，或者提供一个 `GetActorState(actor_id)` RPC。否则 worker 不知道从哪里恢复。

## Q444. actor replay 为什么需要 Store.LoadResult？

因为 snapshot 的完整 state 不在 actor log payload 里，而在 result store 里。

`ActorSnapshotCreated` 事件只保存 `snapshot_ref`。ReplayActor 看到这个事件时，需要调用 `LoadResult(snapshot_ref)` 把 snapshot state 读出来，然后再应用 tail log。

如果没有 `LoadResult`，replay 只能看到“这里有一个 snapshot”，但拿不到 snapshot 内容，也就无法恢复 state。

这和 workflow result_ref 是同一类问题：日志保存引用，具体大对象在对象存储。replay 要完整恢复状态，就必须能通过 ref 读对象。

## Q445. actor state JSON normalization 的目的是什么？

JSON normalization 是为了让状态比较更稳定。

同一个 JSON 对象可能有不同的空格和换行：

```json
{"value":1}
```

和：

```json
{
  "value": 1
}
```

语义一样，但字节不同。ReplayActor 和 metadata 做一致性比较时，如果直接比原始字节，就会误判不一致。

`NormalizeJSON` 会用 `json.Compact` 去掉无意义空白。这样 replay state 和 metadata state 更容易得到一致比较结果。

它不是完整的 canonical JSON。比如对象 key 顺序如果不同，Compact 不会排序。当前 actor state 大多来自同一个 Python runner 序列化路径，key 顺序问题不明显。更严格的生产实现可以使用 canonical JSON 或内容 hash。

## Q446. 如果 actor method 非确定性，replay 是否会重新执行 method？

不会。

LogServe 的 actor replay 是事件重放，不是函数重放。它不会重新调用 Python actor method，也不会重新生成随机数、重新读时间、重新访问外部服务。

如果 actor method 当时返回了一个随机值，`ActorCommandApplied` 里已经记录了执行后的 state_json 和 result_json。Replay 只使用这些事件中的结果恢复状态。

所以非确定性不会影响 replay 得到同一份 state。它影响的是 retry 或重复执行。如果同一命令因为故障被执行两次，非确定性方法可能产生两个不同结果。最终能否写入状态，要看 command_seq、epoch fencing 和 idempotency。

## Q447. actor replay 是事件重放还是函数重放？为什么？

是事件重放。

原因很简单：actor method 可能有副作用，也可能依赖时间、随机数、外部服务。如果 replay 时重新执行函数，就可能发第二封邮件、再扣一次款、读到不同外部状态。

所以 actor stream 里记录的是命令执行后的事实：

- command_seq
- result_json
- state_json
- error
- epoch
- worker_id

Replay 只应用这些事实，不再运行用户代码。

这和 workflow 当前 replay 的思路一致：replay 是重建 runtime 状态，不是重跑业务代码。

## Q448. 如果 ActorCommandApplied 事件中的 state_json 很大，日志膨胀如何解决？

当前直接写 state_json，适合小状态。大状态会让 actor stream 变得很重。

解决方向有几种：

- state 大于阈值时写 result store，事件里放 state_ref。
- `ActorCommandApplied` 只记录增量 patch，而不是完整 state。
- 定期 snapshot，tail log 只保留 snapshot 后少量事件。
- logical trim 配合 physical compaction，释放旧事件占用的磁盘。
- 对 state_json 做压缩。

其中 state_ref 和 snapshot-aware retention 最重要。只做压缩还不够，因为 replay 仍然要读很多大事件。

要注意，增量 patch 会让 replay 更复杂。完整 state 每条事件都自解释，patch 需要从某个 base state 开始按顺序应用。工程上可以先做 state_ref，再考虑 patch。

## Q449. snapshot_every 太小或太大分别有什么代价？

`snapshot_every` 太小，snapshot 很频繁。

代价是：

- result store 写入更多。
- `ActorSnapshotCreated` 事件更多。
- snapshot object GC 压力变大。
- actor command 完成路径变慢。

好处是 crash 后 replay tail 很短。

`snapshot_every` 太大，snapshot 很少。

代价是：

- actor stream tail 变长。
- 重启恢复要 replay 更多命令。
- dashboard 或 ReplayActor 查询更慢。
- 长期运行 actor 的恢复时间越来越不可控。

好处是正常执行路径开销小。

所以它是一个恢复时间和写入开销之间的参数。实验里可以通过 `full_replay_commands` 和 `snapshot_replay_commands` 看收益，再结合 actor command latency 看成本。

## Q450. actor snapshot 和 database checkpoint 有什么相似点？

两者都在解决同一个问题：日志无限增长后，从头 replay 太慢。

数据库 checkpoint 会把某个时刻的脏页状态刷到稳定存储，并记录 checkpoint LSN。崩溃恢复时，不必从数据库创建之初开始 redo，而是从 checkpoint 附近开始。

Actor snapshot 也是类似思路。它把某个 command_count 时刻的 actor state 写到 result store，并在 actor stream 里记录 `ActorSnapshotCreated`。恢复时先加载 snapshot，再 replay snapshot 之后的 tail log。

相似点是：

- 都是恢复加速点。
- 都要记录“这个快照覆盖到哪里”。
- 都不能随便删除快照之后仍需要的日志。
- 都要处理快照写成功但元信息写失败的边界。

不同点是，数据库 checkpoint 面向 page/WAL，Actor snapshot 面向单个 actor 的对象状态。LogServe 当前的 snapshot 也更轻量，没有数据库那种复杂的 buffer pool、dirty page 和 redo/undo 协议。
