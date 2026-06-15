# 六、Actor Runtime、Mailbox、Snapshot、Ownership 与 Epoch Fencing（拓展）

## Q451. Orleans、Akka、Ray actor 和 LogServe actor 的差异是什么？

它们都用了 actor 这个抽象，但主线不一样。

Orleans 的核心是 virtual actor。用户不太需要关心 actor 什么时候创建、什么时候销毁。运行时按 actor id 激活对象，空闲后可以 passivate。它适合写大规模云服务里的有状态实体，比如用户会话、游戏房间、IoT 设备。

Akka 更接近传统 actor model。它强调消息传递、actor hierarchy、supervision 和 failure handling。Akka 里的 actor 通常是显式创建的，开发者要更关注 actor 生命周期、父子关系、消息协议和监督策略。

Ray actor 更偏分布式 Python 计算。它把一个 Python class 变成远程有状态 worker，适合机器学习、数据处理、并行计算。Ray 关注资源调度、对象引用和并行执行效率。

LogServe actor 的主线是 shared log 恢复。它把 actor 的创建、ownership、命令提交、命令应用、snapshot 都写进 `actor:<actor_id>` stream。worker 崩溃后，系统能从 snapshot 和 tail log 恢复 actor state。它不追求覆盖 Orleans/Akka/Ray 的全部功能，而是把 actor 放进 LogServe 的 log-first runtime 里。

一句话总结：

- Orleans 强在虚拟 actor 和自动生命周期。
- Akka 强在消息模型和监督。
- Ray 强在 Python 分布式计算。
- LogServe 强在 actor 状态变更可重放、可恢复、可审计。

## Q452. actor model 的核心假设是什么？

Actor model 的核心假设是：状态属于 actor 自己，外部不能直接共享和修改这个状态，只能通过消息请求 actor 执行逻辑。

这带来几个基本规则：

- 每个 actor 有自己的 identity。
- 每个 actor 有自己的 private state。
- 外部通过 mailbox 发消息。
- 同一个 actor 内部按顺序处理消息。
- actor 之间不共享内存，只通过消息通信。

这样做的好处是并发更容易推理。普通多线程程序里，两个线程可能同时写同一个对象，要靠锁保护。Actor model 里，状态写入集中在 actor 自己的消息处理循环里，外部拿不到这个状态的可变引用。

LogServe 的实现也沿着这个方向走：actor state 不让多个 worker 同时写，命令通过 command_seq 排队，completion 前检查 owner 和 epoch。

## Q453. actor mailbox 如何避免共享内存并发问题？

Mailbox 的作用是把并发请求变成串行处理。

比如 1000 个客户端同时调用 `Counter.inc()`。在共享内存模型里，如果 1000 个线程同时读写 `value`，就要非常小心锁。少一个锁，结果就可能小于 1000。

Actor mailbox 换了一个思路。客户端可以并发提交，但对同一个 actor 来说，runtime 会给这些命令排一个顺序：

```text
inc #1
inc #2
inc #3
...
```

actor 一次只处理一条命令。第 2 条命令看到的是第 1 条命令后的 state，第 3 条命令看到的是第 2 条命令后的 state。

LogServe 里这个顺序不只存在内存中。`ActorCommandSubmitted` 写入 command_seq，`PollTask` 只放行下一条 command，worker 本地 per-actor lock 防止同 actor 并发执行，`completeActorCall` 最后再检查 command_seq。几层加起来，才把共享内存并发问题压成了顺序日志问题。

## Q454. actor 是否天然保证 exactly-once command execution？为什么？

不保证。

Actor model 只说同一个 actor 的消息按某种顺序处理。它不天然保证消息一定只执行一次。分布式系统里会有 worker 崩溃、网络超时、queue redelivery、客户端重试，这些都会让同一条命令被尝试执行多次。

LogServe 的 actor 也不宣称严格 exactly-once。它能保证的是结果提交层和状态推进层的 fencing：

- command_seq 保证应用顺序。
- owner epoch 拒绝旧 worker completion。
- actor call idempotency_key 避免客户端重试生成新命令。
- `ActorCommandApplied` 的 idempotency key 避免同一个 call 写两次 applied event。

但 worker 物理执行可能发生多次。尤其 actor method 有外部副作用时，旧 worker 即使最终被 fencing，它在外部系统里已经发出的请求也不会自动消失。

所以准确说法是：LogServe actor 提供 exactly-once-ish state application，不保证 actor method 物理执行 exactly once。

## Q455. actor 与数据库事务隔离级别有什么类比？

可以把单个 actor 看成一个串行化执行的对象。

对同一个 actor，mailbox 让命令按顺序应用，效果有点像单行记录上的 serial execution。每条命令看到前一条命令提交后的 state，不会出现两个命令同时改同一份 state 的 lost update。

但这个类比只能到单 actor 为止。

数据库事务隔离级别通常讨论多行、多表、多事务之间的可见性，比如 read committed、repeatable read、serializable。LogServe actor 当前没有跨 actor 事务。两个 actor 之间如果互相调用，系统不会自动给它们提供数据库 serializable 那种全局隔离。

所以我会这样讲：

- 单 actor 内部近似串行执行。
- 跨 actor 目前更接近分布式消息系统，需要显式协议。
- 如果要事务性更新多个 actor，需要两阶段协议、Saga 或把相关状态放到同一个 actor/shard 里。

## Q456. 如果两个 actor 之间互相调用，如何避免死锁？

最容易出问题的模式是同步互调。

比如 actor A 正在处理命令，期间同步调用 actor B；B 处理时又同步调用 A。A 在等 B，B 在等 A，就卡住了。

避免方式有几个：

- 不在 actor method 里做同步阻塞调用，改成异步消息。
- actor method 发出请求后立即返回，把后续处理拆成另一条 command。
- 规定调用方向，比如高层 actor 可以调用低层 actor，低层不能反向同步调用高层。
- 对跨 actor 调用设置 timeout，并把 timeout 当成业务失败处理。
- 对需要双向协作的场景，用 coordinator actor 管理流程。

LogServe 当前 actor API 更像同步 `CallActor`，所以跨 actor 同步调用要谨慎。更安全的扩展是提供 `SendActorCommand` 异步接口，让 actor 之间通过事件和回调推进，而不是互相占着 mailbox 等结果。

## Q457. 如果 actor command 需要事务性更新两个 actor，如何设计？

这不是单 actor mailbox 能解决的问题。

有三种常见设计。

第一，把相关状态放进同一个 actor。只要业务上能接受，这是最简单、最可靠的。单 actor 内部本来就是串行的。

第二，用 coordinator actor 做 Saga。它先给 actor A 发命令，再给 actor B 发命令。如果 B 失败，就给 A 发补偿命令。这个方式不保证强原子，但适合很多业务流程。

第三，引入真正的跨 actor 事务协议。比如 prepare/commit 两阶段：

- coordinator 写 `TransactionStarted`。
- A、B 分别进入 prepared 状态。
- 都 prepared 后写 commit。
- 任意失败则 abort。

这会让 actor 实现复杂很多。要处理 coordinator 崩溃、prepared 状态恢复、锁超时和重复提交。

LogServe 当前更适合第一种和第二种。第三种需要共享 log 支持多 stream 原子 append 或事务元数据，否则很难讲清楚一致性边界。

## Q458. 如果 actor 支持 reentrant call，会破坏哪些假设？

Reentrant call 指 actor 在处理一条消息时，允许同一个 actor 继续处理其他消息。

这样可以提高吞吐，也能减少某些等待场景的死锁。但它会破坏一个重要假设：同一个 actor 一次只处理一条命令。

一旦允许 reentrant，就会出现这些问题：

- actor method 执行到一半时，state 可能被另一条命令改变。
- command_seq 不再简单对应 state 变化顺序。
- `state.CommandCount + 1` 的检查要重新设计。
- snapshot 可能捕获到中间状态。
- 用户代码要面对并发状态访问。

如果一定要支持 reentrant，需要非常严格的规则。比如只允许 read-only 方法重入，或把 reentrant 调用限制在同一个 request context 内。否则 actor 的简单顺序语义会被打破。

LogServe 当前不支持 reentrant，这个选择是保守的，但更容易保证状态恢复和 replay 正确。

## Q459. 如果 actor 状态需要持久化到外部 DB，shared log 和 DB 如何保持一致？

这是典型的双写问题。

如果 actor method 先写 DB，再写 LogServe actor stream，中间崩溃后 DB 已经变了，但 log 没记录。重启 replay 不知道这次状态变化。

如果先写 actor stream，再写 DB，中间崩溃后 log 里说状态变了，但 DB 没变。外部查询会看到不一致。

常见做法是 outbox pattern。actor command 先把状态变化和要发给 DB 的外部操作写进同一个 durable log 或同一个数据库事务。后台 worker 再按 outbox 记录去更新外部 DB，并用幂等键保证重复投递安全。

在 LogServe 的主线里，shared log 应该仍然是 actor 状态的 source of truth。外部 DB 可以是投影 view。这样 DB 落后时可以重放 log 修复。

如果业务要求 DB 是强事实来源，那就要反过来让 DB 事务成为 source of truth，LogServe 只能做执行层。不能两边都宣称自己是最终事实。

## Q460. 如果 actor 需要 sharding，actor_id 到 worker 的映射如何维护？

可以用 placement table 维护映射。

最简单的表结构是：

```text
actor_id -> owner_worker_id, epoch, shard_id, updated_at
```

如果 actor 数很多，不能每次都全表扫描。可以先把 actor_id 映射到 shard，再把 shard 分配给 worker：

```text
actor_id -> shard_id -> worker_id
```

映射方式可以是 consistent hashing，也可以是固定 shard 数。worker 扩缩容时，只迁移部分 shard，而不是重新分配所有 actor。

LogServe 当前是按 actor 直接选 owner worker，并写 `ActorOwnershipGranted`。这适合实验和中小规模。百万级 actor 更适合引入 shard owner，让 placement 从单 actor 粒度变成 shard 粒度。

## Q461. 如果 actor 数量百万级，metadata view 如何扩展？

百万级 actor 下，单机内存 map 会吃紧，启动 replay 也会慢。

需要几类改造：

- metadata 按 tenant 或 shard 分区。
- actor state 冷热分层，活跃 actor 在内存，冷 actor 只保留索引。
- owner mapping 单独建表或 KV。
- dashboard 查询必须分页，不能一次 ListActors 全量返回。
- bootstrap 不能全量阻塞启动，要按 shard 增量恢复。
- actor stream stats 需要按前缀和 shard 聚合。

还要引入 passivation。大多数 actor 不是一直活跃的。冷 actor 不应该一直占用 worker 内存和控制面热状态。收到调用时再 activation，从 snapshot 和 tail log 恢复即可。

当前 LogServe 的 actor metadata 适合实验规模。扩到百万级，核心是分区、懒加载和冷热分层。

## Q462. 如果某个 actor 成为热点，单 mailbox 串行化如何扩容？

单个 actor 的串行 mailbox 天然有吞吐上限。这个上限来自 actor method 平均耗时，而不是 worker 数量。

能不能扩容，要看业务是否真的需要一个线性化对象。

如果必须保持单调顺序，比如一个账户余额，单 actor 串行化是正确成本。可以优化 method 执行时间、减少外部调用、把耗时工作移出 actor，但不能让同一状态并发写。

如果业务可以拆，就把热点 actor 拆成多个 shard。例如 Counter 可以拆成 100 个 shard，每个 shard 维护部分计数，读取时聚合。这样写吞吐上去了，但读需要 fan-in，且不再是每次 inc 后立刻有一个全局线性化值。

还有一种做法是 command batching。把多个 inc 合成一批，在 actor 内一次应用。这能提升吞吐，同时保留顺序语义。

## Q463. 能否把一个 actor 拆成多个 shard？一致性如何保证？

可以，但要承认一致性语义会变。

如果把 Counter 拆成 10 个 shard，每个 shard 负责一部分 inc，写入吞吐会提高。`get()` 时需要读取 10 个 shard 的值再求和。

问题是：这个 get 看到的是不是同一个时间点的全局状态？如果没有全局 snapshot 或事务，答案通常不是。它看到的是多个 shard 各自某个时刻的状态。

保证方式有几种：

- 接受最终一致，get 返回近似实时值。
- 给每个 shard 的更新带 logical timestamp，读取时按水位线聚合。
- 用 coordinator 发起 barrier，让所有 shard 到达同一 sequence 后再读。
- 对强一致需求，把状态仍放在单 actor，shard 只做缓存或预聚合。

拆 shard 是扩吞吐手段，不是免费午餐。面试里讲清楚语义变化，比直接说“可以水平扩展”更可靠。

## Q464. 如果 actor 状态很大，增量 snapshot 如何设计？

增量 snapshot 可以减少每次 snapshot 的写入成本。

一种设计是 base snapshot + delta chain：

- 每隔较长周期写一个 full snapshot。
- 中间每 N 条命令写 delta snapshot。
- delta 只保存 state 变化部分。
- replay 时加载最近 full snapshot，再按顺序应用 delta 和 tail log。

另一种设计是按 state key 分块。比如 actor state 是一个大 map，snapshot store 按 key range 或 chunk hash 存。只有变化的 chunk 会重写。

要注意几个问题：

- delta chain 不能太长，否则恢复仍然慢。
- 每个 delta 要带 base snapshot id 和 command_count。
- 需要内容 hash 校验，避免某个 delta 损坏后悄悄恢复出错。
- compaction 时要能合并 delta，生成新的 full snapshot。

当前 LogServe 用完整 state snapshot，简单但不适合超大 actor state。增量 snapshot 是后续优化，不应过早引入。

## Q465. 如果 snapshot store 是 S3，读取延迟会如何影响 failover？

会直接拉长 actor 接管时间。

worker 失联后，新 owner 要恢复 actor state。如果 state 在本地 metadata 里还完整，接管很快。如果必须从 S3 读 snapshot，再读 tail log，failover latency 至少包含：

- S3 get 延迟。
- snapshot 大小带来的下载时间。
- tail log replay 时间。
- Python actor 初始化时间。

如果 snapshot 很大，S3 读取会成为主要瓶颈。

优化办法包括：

- worker 本地缓存最近 snapshot。
- 控制面或 worker 预取热点 actor snapshot。
- snapshot 分块，按需读取。
- 给 snapshot 加压缩。
- 对热点 actor 使用更短 tail log，减少 snapshot 后 replay。
- 多副本对象存储或同机房对象存储，降低网络延迟。

所以 snapshot store 的位置和性能，会直接影响 actor recovery 指标。

## Q466. 如果 snapshot 被删除或损坏，是否还能从 full log 恢复？logical trim 后呢？

如果 full log 还完整，可以从 `ActorCreated` 开始 replay，绕过损坏 snapshot。

但 logical trim 后，默认 `ReadLog` 已经隐藏 snapshot 之前的事件。如果 snapshot 又被删除或损坏，默认 replay 就会失败。因为它既拿不到 snapshot state，也看不到更早的 command history。

所以在允许 physical compaction 或删除旧日志之前，必须确认 snapshot 可靠：

- snapshot object 存在。
- hash 校验通过。
- 至少有足够副本。
- `ActorSnapshotCreated` 元信息仍在 log 中。
- 最好保留上一个可用 snapshot，避免最新 snapshot 损坏。

如果只做 logical trim，没有物理删除，理论上还可以提供 audit/full-read 模式读旧事件恢复。可一旦 physical compaction 删除了旧 segment，就只能依赖 snapshot。

## Q467. physical compaction 前如何保证 snapshot 持久可靠？

physical compaction 删除的是恢复可能还需要的旧日志，所以必须非常谨慎。

删除前至少要检查：

- 对应 `ActorSnapshotCreated` 事件已经 durable。
- snapshot_ref 指向的对象存在。
- snapshot 内容 hash 和记录的 hash 匹配。
- snapshot_command_count 覆盖了即将删除的 command。
- tail log 从 snapshot event 开始完整。
- snapshot store 的保留策略不会比 log 更短。

更稳的做法是两阶段：

1. 先把旧 log 标记为 compactable。
2. 后台 verifier 读取 snapshot，做一次恢复校验。
3. 校验通过后再物理删除旧 segment。

这样 compaction 不是“看到 trim point 就删”，而是“确认 snapshot 可恢复后再删”。

## Q468. epoch fencing 与 lease fencing token 在分布式锁中的关系是什么？

它们是同一类思想。

分布式锁里，拿到锁的客户端通常会得到一个 fencing token。每次写外部资源时，都带上这个 token。资源端只接受 token 更大的写入，拒绝旧 token。

LogServe actor 的 epoch 就是 actor ownership 的 fencing token。每次 ownership 转移，epoch 增加。worker 完成 actor command 时必须带 actor_epoch。控制面只接受当前 epoch 的 completion。

这解决的是“旧 owner 复活”的问题。旧 owner 也许还以为自己有锁，但它的 token 已经过期。只要写入端检查 token，旧 owner 就不能覆盖新 owner 的状态。

## Q469. ZooKeeper/etcd lease 和 actor epoch 有什么相似点？

相似点是都在表达“谁现在拥有某个权利”。

ZooKeeper/etcd 的 lease/session 常用于服务注册、leader election、分布式锁。客户端必须维持 lease。如果 lease 过期，系统认为它不再持有对应权利。

LogServe actor 里，worker 通过 heartbeat 维持活跃状态。owner 超过 lease 时间没有 heartbeat，control 可以把 actor 转给新 worker，并递增 epoch。

差异是：ZooKeeper/etcd 自己是强一致协调系统。LogServe 当前 actor ownership 由 control plane 和 shared log 管理，还没有把 ownership 做成独立的强一致协调服务。

所以类比可以讲，但不能说它已经等价于 etcd lease。当前实现更轻，适合实验；生产化要补 leader election、CAS 和更严格的 lease 语义。

## Q470. 如果系统时钟漂移，owner lease 判断会有什么风险？

当前 owner lease 判断依赖 control 看到的 worker `LastHeartbeat` 和当前时间差。

如果都在同一个 control 进程里记录时间，风险相对小，因为 heartbeat 到达时用 control 本地时间戳。但如果多个 control 实例、跨机器 timestamp 或 DB 写入时间参与判断，时钟漂移就会麻烦。

风险包括：

- 误判 worker 已过期，提前转移 owner。
- 误判 worker 仍活跃，故障恢复变慢。
- 不同 control 对同一个 worker 是否活跃判断不一致。

所以 lease 判断最好少依赖远端机器上报的 wall-clock 时间。让接收 heartbeat 的 control 用自己的单调时间记录 last seen，更稳一些。

## Q471. 如何用 monotonic clock 降低 lease 判断风险？

单调时钟不会因为 NTP 校时突然跳回去或跳到未来，适合算时间间隔。

在 Go 里，`time.Now()` 携带 monotonic clock 信息，用 `time.Since(t)` 计算间隔时可以利用这一点。但如果把时间转成 Unix milliseconds 存进 metadata，monotonic 部分就没了。

更稳的设计是：

- 内存里保存 worker lastSeen time.Time，用 `time.Since(lastSeen)` 判断活跃。
- 持久化时可以保存 wall-clock timestamp，用于展示和重启恢复。
- 不同节点之间不要直接比较对方本地生成的 monotonic timestamp。

如果 control 重启，内存里的 monotonic lastSeen 会丢失。这时可以保守处理：要求 worker 重新 heartbeat 后再认为它 active，或者用持久化 wall-clock 做临时判断，但阈值要更宽。

## Q472. 如果 GC pause 超过 actorOwnerLease，会不会发生错误 owner transfer？

会。

如果 worker 因为 GC pause、CPU 饥饿或进程卡顿，超过 750ms 没发 heartbeat，control 可能认为它失联，然后把 actor 转给另一个 worker。

旧 worker pause 结束后，可能继续执行手上的 actor task。但 completion 会因为 epoch 过期被拒绝。状态不会写坏，但执行资源浪费了，客户端也可能看到超时或重试。

这说明短 lease 的误判成本不低。750ms 适合本地实验，生产环境应该根据 heartbeat interval、GC pause 分布和网络延迟调大。还可以让 worker 在执行长任务时发送更稳定的 heartbeat，或者把 lease 和 task execution 状态结合起来判断。

## Q473. 如何让 actor ownership transfer 更稳健？

可以从几个方向加强。

第一，调大 lease，并用多次 missed heartbeat 才判定 owner 失联。

第二，转移前做 health probe。比如 control 发现 heartbeat 超时后，先尝试一次轻量 RPC ping。ping 失败再转移。

第三，ownership grant 使用 CAS。只有当前 epoch 仍等于预期值，才能写新的 ownership event。

第四，转移后给旧 owner 一个 drain/stop 信号。旧 owner 收到后停止 poll 和提交 completion。

第五，dashboard 展示 owner transfer 次数和 stale completion 次数。如果这些指标升高，说明 lease 太短或 worker 卡顿。

第六，热点 actor 可以做 sticky placement，避免因为轻微抖动频繁迁移。

这些增强不会改变核心语义：最终仍然要靠 epoch fencing 防旧 owner 写入。

## Q474. 如果 actor command 有外部副作用，旧 worker 执行副作用后被 fencing 还能回滚吗？

不能自动回滚。

Epoch fencing 只拦住旧 worker 把 completion 写回 LogServe actor state。它挡不住旧 worker 在外部系统已经做过的事，比如发邮件、扣款、调用第三方 API。

这就是 exactly-once-ish 的边界。

有外部副作用时，需要业务层配合：

- actor method 调外部系统时传 idempotency_key。
- 外部系统按 key 去重。
- 对可补偿操作提供 compensation。
- 对不可补偿操作，避免在可能 stale 的 worker 上执行，或者先确认 lease 仍有效。

更严格的做法是把外部副作用改成 outbox：actor command 只写“准备发送外部操作”的事件，由专门的 dispatcher 按 fencing token 和 idempotency_key 执行外部调用。

## Q475. 如何要求用户 actor method 幂等？

可以从 API、文档和运行时三层约束。

API 层可以要求 mutation method 声明幂等策略：

```python
@actor_method(idempotent=True)
def charge(self, request_id, amount):
    ...
```

文档层要说清楚：如果方法会调用外部系统，必须把 actor_call_id 或业务 request_id 传给外部系统，外部系统也要支持去重。

运行时可以提供：

- 自动注入 call_id。
- 在 actor state 里维护 processed request ids。
- 对同一个 idempotency_key 的重复 call 返回旧结果。
- 对同 key 不同参数直接报冲突。

不过不能把所有方法都强行变成幂等。比如 `inc()` 本身就是“每次调用都加一”，重复提交就是不同命令。幂等的对象应该是同一次业务请求的重试，而不是所有同名方法调用。

## Q476. 如何为 actor 添加 passivation/activation？

Passivation 是把空闲 actor 从 worker 内存里卸载。Activation 是下一次调用时再恢复。

设计可以这样做：

- actor metadata 里记录 last_access_ms。
- worker 定期上报本地活跃 actor。
- control 或 worker 对空闲 actor 写 `ActorPassivated`。
- passivation 前确保 state 已经通过 command log 或 snapshot 持久化。
- 下一次 CallActor 时，control 写 `ActorActivated` 或直接 grant owner。
- 新 owner 从 snapshot + tail log 恢复 state。

LogServe 当前 actor task 每次都带 state 给 Python runner，并不依赖 worker 长期持有 actor 内存，所以已经有一点“天然可激活”的味道。但如果以后 worker 真正缓存 actor 对象，就需要显式 passivation，避免百万 actor 全常驻内存。

## Q477. 如何为 actor 添加 placement policy？

Placement policy 决定 actor 应该放在哪个 worker 上。

可以支持几类策略：

- random：随机选择活跃 worker。
- least-loaded：选择 actor 数或 running tasks 最少的 worker。
- hash：按 actor_id consistent hashing。
- affinity：和某个 model cache、tenant、数据分片放在同一 worker。
- anti-affinity：同一租户的热点 actor 分散到不同 worker。
- resource-aware：考虑 CPU、内存、GPU、本地 SSD。

Actor 创建或 owner 转移时，`ensureActorOwner` 不再简单选 sorted active worker 的第一个，而是调用 placement scorer。

更重要的是解释决策。dashboard 应该能显示某个 actor 为什么被放到 worker-2：因为 owner 失联、worker-2 load 最低、符合 tenant affinity。调度黑盒会让排障很难。

## Q478. 如何对 actor mailbox 长度做 backpressure？

Actor mailbox backlog 高时，继续无限接收命令只会放大延迟。

可以做几层 backpressure：

- per-actor pending command limit。
- per-tenant actor command limit。
- actor method 级别 rate limit。
- 如果 `SubmittedCommandCount - CommandCount` 超过阈值，拒绝新 mutation。
- 对 read-only 方法可以走单独策略，或者返回 stale read。
- dashboard 暴露 mailbox backlog 和 oldest pending age。

返回给客户端的错误要明确，比如 `actor mailbox overloaded`，并带 retry-after。

这里不能只看全局 queue depth。一个 actor 可能已经积压 10 万条命令，但全局队列还没到高水位。backpressure 必须有 per-actor 维度。

## Q479. 如何在 dashboard 中展示 actor 热点和 mailbox backlog？

dashboard 应该展示 actor 维度的几个指标：

- command_count
- submitted_command_count
- mailbox_backlog = submitted_command_count - command_count
- owner_worker_id
- epoch
- last_command_latency_ms
- snapshot_command_count
- replay_tail_commands
- stale_completion_count
- ownership_transfer_count

热点 actor 可以按 backlog、调用 QPS、平均延迟、p95 延迟排序。

更直观的展示是 actor detail 页：

- 当前 owner worker。
- 最近 N 条 ActorCommandSubmitted/Applied/Failed。
- mailbox backlog 曲线。
- snapshot 时间线。
- ownership transfer 时间线。
- 是否出现过 stale completion。

这样面试官问“你怎么发现 actor 热点”时，不只是说看日志，而是能讲出具体指标和页面。

## Q480. 如何测试 actor 并发提交的线性化顺序？

测试思路是：并发提交很多命令，但最终日志和状态必须能排成一条合法顺序。

可以写一个 Counter actor，然后并发提交 1000 次 `inc()`。验收点包括：

- 最终 `get()` 返回 1000。
- actor stream 中 command_seq 从 1 到 1000 连续无缺口。
- 每个 `ActorCommandApplied` 都有对应的 `ActorCommandSubmitted`。
- applied 的 command_seq 不早于 submitted。
- 没有两个 applied 使用同一个 command_seq。
- ReplayActor 得到的 state 和 metadata 一致。

还要加故障场景：

- command 1 执行慢，确认 command 2 不会先 dispatch。
- 旧 owner completion 被拒绝。
- queue redelivery 后不会重复应用同一 command。
- 客户端 timeout 后再次提交，带 idempotency_key 时不会新增 command。

线性化测试的关键不是只看最终值。最终值对了还不够，还要检查日志里的顺序证据。LogServe 的优势就是这些证据在 actor stream 里能查出来。
