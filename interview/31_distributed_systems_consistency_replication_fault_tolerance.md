# 十一、分布式系统原理与生产化追问：一致性、复制与容错

这一组问题会把 LogServe 从单机实验推到生产化讨论。回答时要分清当前实现和未来设计。当前项目里，`logd` 是单机 shared log，control 以 log-first 路径更新 metadata view，task 用 lease epoch 防旧 completion，actor 用 owner epoch 做 fencing。生产化时，最先要补的是多副本 logd、control leader/fencing、严格租约和更完整的事务边界。

## Q756. 当前 logd 单点会造成什么可用性问题？

当前 `logd` 是单机进程，shared log 又是 LogServe 的 source of truth，所以它是最明显的可用性瓶颈。

如果 `logd` 挂掉，control 不能安全地接受新的状态变更。比如 `SubmitTask`、`SubmitWorkflow`、`ActorCommandSubmitted`、`TaskRedelivered`、`LLMCompleted` 这类事件都应该先 append log。log append 失败时，如果 control 继续更新 metadata view，就会破坏 log-first 语义，重启后 replay 不出这些状态。

已有实现里，control 的写路径基本是先 `appendLog`，再更新 metadata。这个选择牺牲了一部分可用性，但保住了恢复语义。`logd` 不可用时，正确行为更接近 fail fast，而不是绕过 log 降级写 metadata。

单点还带来恢复时间问题。单机 `logd` 重启期间，已经在执行的 worker 可能还能运行用户代码，但 completion 写不进 log 或 control 无法完成状态推进。客户端看到的状态会卡住，直到 logd 恢复。

生产化要解决这个问题，需要把 logd 做成多副本复制日志，至少做到 leader 挂掉后可以选出新 leader，已提交日志不回滚。

## Q757. 如果 logd 做多副本，复制协议如何选择？

我会优先选 Raft。

原因很直接：LogServe 的 logd 需要提供顺序日志复制、leader 选举、quorum commit 和崩溃恢复。Raft 的模型和这个需求很贴近，实现和解释也比 Multi-Paxos 更容易。这个模块不需要做共识算法创新，核心是把 shared log 做可靠。

Raft 的好处是：

- 一个 leader 负责接收 append。
- follower 复制 leader 的 log entry。
- entry 达到 quorum 后才算 committed。
- leader 切换后，已 committed 的 entry 不应该丢。
- commit index 可以映射到 metadata view 的 apply 边界。

主从复制也能做，但如果只是一主多从、没有 quorum 和 leader election，就很难处理主节点崩溃后的脑裂和丢写。Multi-Paxos 可以，但实现和排障成本更高。

所以我的选择是：先用 Raft 做三副本 logd，把 AppendLog 的成功语义改成“entry 已写入多数派并被 leader commit”。后续如果要更高吞吐，再考虑 batching、pipeline append、分区 stream 和多 Raft group。

## Q758. Raft 中 leader、follower、candidate 分别对应 logd 的什么角色？

如果把 logd 改成 Raft 集群，三个角色可以这样理解。

leader 是对外服务的 logd 节点。control 和 worker 相关组件向 leader 发 `AppendLog`。leader 给日志 entry 分配位置，把 entry 复制给 follower，达到 quorum 后返回成功。

follower 是被动复制节点。它接收 leader 的 AppendEntries，把日志写到本地磁盘，并按 leader 的 commit index 应用已提交日志。follower 通常不处理写请求；如果收到客户端写入，可以重定向到 leader。

candidate 是选举中的临时状态。某个 follower 超过 election timeout 没收到 leader heartbeat，就变成 candidate，增加 term，向其他节点请求投票。拿到多数票后成为 leader。

放到 LogServe 里，Raft leader 负责保证 shared log 的顺序和 commit；follower 负责提供副本；candidate 只在 leader 故障时出现。control plane 不应该自己判断哪个 logd 节点“看起来像 leader”，而应该通过 logd client 或 Raft endpoint 获取当前 leader。

## Q759. Raft commit index 如何保证日志不会回滚？

Raft 的 commit index 表示“到这个位置为止的日志 entry 已经被多数派确认，可以对状态机生效”。

一条 entry 只有复制到多数节点后，leader 才能把它推进为 committed。多数派有交集，这是关键。后续即使 leader 挂掉，新 leader 的选举也要求候选者日志足够新。已经 committed 的 entry 至少存在于多数派中的某些节点，新 leader 不应该缺少这些已提交 entry。

对 LogServe 来说，`AppendLog` 返回成功应当对应 Raft commit，而不是只写到 leader 本地磁盘。metadata view 的 apply 也应该基于 commit index：只有 committed entry 才能被 control replay 和 materialize。

如果一个 entry 只写到 leader，还没达到 quorum，这条 entry 不能算 committed。leader 崩溃后，新 leader 可能没有它。客户端也不应该看到“成功”。否则客户端认为 workflow 已提交，重启后 log 里却没有 `WorkflowStarted`，这会直接破坏系统语义。

## Q760. 如果 AppendLog 写到 leader 但未达 quorum 就崩溃，客户端是否应该看到成功？

不应该。

在多副本 logd 里，`AppendLog` 的成功语义必须清楚。只写到 leader 本地，最多说明 leader 接收过这个请求；没有达到 quorum，就不能保证 leader 崩溃后这条日志还在。

所以正确行为是：leader 只有在 entry 达到 quorum 并推进 commit index 后，才向客户端返回成功。若 leader 在 quorum 前崩溃，客户端应该收到超时、连接断开或错误。客户端再用同一个 idempotency key 重试。

重试时可能出现两种情况。第一，旧 entry 实际没有 commit，新 leader 会重新 append。第二，旧 leader 在返回前其实已经 commit，只是响应丢了，新 leader 或集群中的 idempotency 记录应该返回 duplicate 或已有 seq。

这也是 idempotency key 的价值：客户端不需要猜上一次到底有没有成功，只要用同一个 key 查询或重试，系统返回同一条已提交结果或明确冲突。

## Q761. 如果 control plane 更新 metadata view 后崩溃，replay 能否保证最终恢复？

要看 log append 是否已经成功。

LogServe 的主线是 log-first。只要事件已经成功 append 到 shared log，metadata view 更新后崩溃也没关系。control 重启时可以 `BootstrapFromLog`，重新读取 task、workflow、actor、LLM、model、worker 等 stream，把 metadata view materialize 出来。

比如 `SubmitWorkflow` 写入 `WorkflowStarted` 后，control 创建 metadata state，再调度 ready step。如果 control 在 metadata 更新后崩溃，重启时从 workflow stream 能看到 `WorkflowStarted`，可以恢复 workflow state，然后继续调度。

反过来，如果 metadata 已更新但 log append 没成功，就不能靠 replay 恢复。这正是项目反复强调 log-first 的原因。metadata view 是投影，不是事实源。

所以回答可以很明确：log 已提交时能恢复；log 没提交时不应该让 metadata view 先发生不可恢复变化。

## Q762. 如果两个 control 同时调度一个 task，如何避免重复 lease？

当前实现默认是单 control。两个 control 同时调度时，现有内存队列和 metadata lock 不能跨进程生效，会有重复 lease 风险。

要支持多 control，有几种办法。

最简单是 control leader election。只有 leader control 可以调度和写状态，其他 control 做 standby 或只读查询。leader 失效后，新 control 接管。

如果要 active-active，就需要把 lease 操作变成带条件的原子写。比如在 PostgreSQL 里用事务：

```sql
UPDATE tasks
SET worker_id = ?, lease_epoch = lease_epoch + 1, status = 'RUNNING'
WHERE task_id = ?
  AND status = 'QUEUED'
RETURNING lease_epoch;
```

只有一个 control 能更新成功。另一个 control 拿不到 row，就不能把任务发给 worker。

更 log-first 的做法是把 lease 本身也写成 log entry：`TaskLeaseGranted(task_id, worker_id, lease_epoch)`。log 的顺序决定哪个 lease 生效。control 可以竞争 append，但最终 replay 只接受最新且合法的 lease。

生产化时我会先做 leader control，再逐步把关键状态迁到带 CAS 的 store 或 log-serialized 状态机里。

## Q763. 分布式锁、lease、fencing token 的区别是什么？

这三个词容易混在一起，但作用不一样。

分布式锁解决“谁现在有资格进入临界区”。比如某个 worker 拿到 actor ownership，理论上它可以执行这个 actor 的命令。

lease 是带过期时间的锁。持有者必须续约。它解决进程崩溃后锁永远不释放的问题。比如 worker 长时间不 heartbeat，control 可以认为它的 lease 过期，把 actor 或 task 转交给其他 worker。

fencing token 是单调递增的版本号，用来防旧持有者继续写。即使旧 worker 因为网络分区还以为自己持有锁，它提交结果时也必须带 token。接收方检查 token 是否仍然是最新的。旧 token 会被拒绝。

只靠锁不够，因为旧持有者可能在锁过期后才恢复网络。只靠 lease 也不够，因为外部系统不知道哪个 lease 更新。fencing token 才能在写入点裁决谁是新 owner。

LogServe 里 task 的 `TaskLeaseEpoch` 和 actor 的 `Epoch` 就是 fencing token 的思路。

## Q764. actor epoch fencing 与 task lease epoch 有什么共同点？

共同点是：它们都用单调递增的 epoch 拒绝旧执行者的写入。

task 这边，control 把 task lease 给某个 worker 时，会递增 `TaskLeaseEpoch`。worker 在 `StartTask` 和 `CompleteTask` 时必须带上这个 epoch。任务被 redelivery 后，新 worker 拿到更大的 epoch。旧 worker 后来再提交 completion，会因为 epoch 不匹配被拒绝。

actor 这边，actor ownership 绑定 `owner_worker_id + epoch`。新 worker 接管 actor 时，epoch 增加。旧 worker 如果还拿着旧 actor state 执行命令，completion 里带的 actor epoch 会落后，control 不应该接受它对 actor state 的更新。

两者解决的都是“旧执行者晚到”的问题。区别在粒度：task lease epoch 保护单个 task attempt；actor epoch fencing 保护一个 actor instance 的 ownership 和状态推进。

## Q765. 如果系统时钟不可靠，基于 LastHeartbeat 的活跃判断有什么风险？

当前 worker 活跃判断依赖 metadata 里的 `LastHeartbeat`。这在单机实验里够用，但在分布式环境里要小心。

如果 control 使用自己的本地时间记录 heartbeat 到达时间，风险相对小一些，因为比较都在 control 本地完成。问题出在 control 节点切换、机器暂停、GC pause、虚拟机时钟跳变、NTP 调整这些场景。时间突然向前跳，可能误判 worker 失联；时间向后跳，可能让失联 worker 看起来还活着。

如果系统使用 worker 上报的时间戳，风险更大。不同机器时钟可能有偏差，worker 的时间不能直接作为租约判断依据。

误判的后果不轻。control 可能把 task redelivery 给新 worker，或把 actor ownership 转给新 worker。旧 worker 其实还在执行，网络恢复后就会出现两个 worker 都认为自己有效的情况。此时必须靠 lease epoch 或 actor epoch 来挡住旧 completion。

所以 heartbeat 只能做活跃性的近似判断，不能单独作为写入安全边界。

## Q766. 如何用租约而不是心跳时间戳做更严格的 ownership？

更严格的做法是让 ownership 变成显式 lease，而不是只看最后一次 heartbeat 时间。

以 actor ownership 为例，可以设计一张 lease 记录或 log entry：

```text
actor_id
owner_worker_id
epoch
lease_start_ms
lease_expire_ms
renew_deadline_ms
```

worker 必须定期 `RenewActorLease(actor_id, epoch)`。control 或 lease service 只有在当前 epoch 匹配时才允许续约。过期后，新 owner 获取 ownership 时必须递增 epoch，并把 `ActorOwnershipGranted` 写入 actor stream。

这里的重点在续约和接管路径：它们都要通过同一个一致性点。这个一致性点可以是 Raft logd、etcd、PostgreSQL CAS，或者 control leader 内部状态机。

worker 执行 actor command 时带 epoch。control 完成 actor state 更新前检查 epoch。旧 worker 即便还在跑，也写不进新 epoch 的 actor state。

这样 heartbeat 变成健康信号，lease 才是 ownership 的授权。

## Q767. 如何处理网络分区下的 worker completion？

网络分区下要接受一个事实：worker 可能还在执行，但 control 已经把任务转交给别人。

处理原则是 completion 必须带 lease 信息。LogServe 当前 task completion 带 `worker_id` 和 `task_lease_epoch`。control 会检查当前 task 的 worker 和 epoch。如果任务已经 redelivery 给新 worker，旧 worker 的 completion 会被拒绝。

actor 也类似。actor command completion 必须带 actor epoch 和 command sequence。旧 owner 的 epoch 过期后，它的状态更新不能应用。

客户端看到的语义是 at-least-once execution：旧 worker 可能真的执行过用户代码，新 worker 也可能重新执行。但 result commit 要去重，状态推进只接受当前 lease/epoch 的 completion。

如果任务有外部副作用，平台没法自动回滚旧 worker 已经做过的事。用户必须使用幂等 API，或者把外部副作用放到带业务 idempotency key 的下游系统里。平台能挡住 LogServe 内部状态被旧 completion 污染，挡不住所有外部世界的副作用。

## Q768. 如果客户端重试 SubmitWorkflow，如何保证不会重复创建 workflow？

靠 idempotency key 和 fingerprint。

Python SDK 的 `submit_workflow()` 支持传入 `idempotency_key`。control 收到 `SubmitWorkflow` 后，会计算 workflow definition 的 fingerprint，包括 workflow name、definition、输入等语义字段。然后先查 metadata view 里是否已有同一个 idempotency key。

如果 key 已存在，并且 fingerprint 一致，control 返回已有 workflow id。客户端重试不会创建第二个 workflow。

如果 key 已存在，但 fingerprint 不一致，就应该返回冲突。不能把同一个 key 的不同 payload 当成同一个请求，否则用户可能拿到完全不相关的 workflow 结果。

更生产化的版本还需要把 idempotency 事实写入 shared log 或可从 log 恢复。当前项目已经把 idempotency key 和 fingerprint 放进 workflow event payload，control bootstrap 可以重建 view。多副本和多 control 场景下，这个 key 的检查必须走同一个一致性边界，不能只依赖某个 control 进程的本地内存。

## Q769. 如果 gRPC 超时但服务端已经成功处理，客户端如何判断状态？

客户端不能只根据超时判断失败。gRPC 超时只说明客户端没收到响应，不代表服务端没处理。

正确做法是：客户端用同一个 idempotency key 重试，或者用返回前已知的业务 id 查询状态。

比如 `SubmitWorkflow` 超时，客户端如果第一次请求带了 `idempotency_key`，就可以再次提交同样请求。服务端如果已经成功创建 workflow，会返回已有 workflow id。如果服务端没处理成功，会新建一次。如果 payload 不同，会返回 idempotency conflict。

如果客户端已经拿到了 workflow id，但等待结果时超时，可以调用 `GetWorkflowStatus` 或 `ReplayWorkflow`。task、actor call、LLM request 也类似，用 task id、actor call id 或 idempotency key 查询。

这就是分布式系统里常见的“unknown outcome”。超时只说明结果未知，不能直接当成失败。idempotency key 和状态查询 API 是处理 unknown outcome 的主要手段。

## Q770. 幂等键在分布式系统中为什么重要？

因为网络会丢包，客户端会重试，服务端也可能在返回响应前崩溃。

没有幂等键，重试可能变成重复提交。比如用户点击一次提交 workflow，客户端超时后重试，control 可能创建两个 workflow。actor command 也一样，重复 `inc()` 会把 counter 加两次。

幂等键给系统一个判断标准：同一个业务请求重复到达时，应该返回同一个结果，而不是再执行一遍。LogServe 在 task、workflow、actor create、actor call、LLM submit、log append 这些路径都用到了 idempotency key 或类似 key。

不过幂等键还要配 fingerprint。只看 key 不看 payload，会把不同请求误认为同一个请求。正确做法是同 key 同 fingerprint 返回已有结果；同 key 不同 fingerprint 返回冲突。

它不能让执行本身 exactly-once，但能让结果提交和对象创建更接近 effectively-once。

## Q771. exactly-once delivery 是否真实存在？通常如何实现 effectively-once？

严格的 exactly-once delivery 在真实分布式系统里很难成立。网络重试、进程崩溃、分区、客户端超时都会让“到底执行过没有”变得不可见。

通常能做到的是 effectively-once。意思是底层可能 at-least-once 执行，但对外可观察结果只提交一次。

常见做法有几类：

- 幂等 producer：客户端带 idempotency key。
- 幂等 consumer：消费端记录处理过的 message id。
- 事务性写入：消费消息和更新状态在一个事务里完成。
- fencing token：拒绝旧 owner 或旧 attempt 的写入。
- 去重结果提交：同一个 workflow step 或 actor command 只接受一次最终结果。

LogServe 的语义接近 exactly-once-ish。worker 可能执行多次，但 task completion、workflow final result、actor state apply 会通过 idempotency key、step id、command sequence、lease epoch 和 actor epoch 去重或拒绝旧写。

面试里要避免说“严格 exactly-once”。更准确的说法是：执行层至少一次，提交层做去重和 fencing。

## Q772. LogServe 哪些路径是 at-least-once execution？哪些路径是 deduplicated result commit？

at-least-once execution 主要在 worker 执行层。

普通 task 被 redelivery 后，旧 worker 可能已经执行过，新 worker 也可能重新执行。workflow step 底层也是 task，所以 step 函数可能被执行多次。actor command 在极端故障下也可能被旧 owner 执行过，但旧 completion 应该被 epoch fencing 拒绝。LLM task 如果请求超时或 worker 崩溃，也可能被重发到另一个 worker。

deduplicated result commit 发生在控制面状态推进层。

普通 task completion 通过 task id、worker id、task lease epoch 检查，terminal 状态不会被重复覆盖。workflow step success 用 workflow id、step id、input hash、attempt 等语义去重，避免重复写 final result。actor command 用 command_seq 和 actor epoch 保证同一 actor 状态按序推进。SubmitWorkflow、CreateActor、SubmitTask 这类创建操作用 idempotency key 和 fingerprint 避免重复对象。

所以 LogServe 的边界是：不保证用户代码只执行一次，但尽量保证平台内部状态和最终结果只按合法顺序提交一次。

## Q773. 如果外部副作用不可幂等，平台能否保证 exactly-once？

不能。

如果用户 task 里直接扣款、发邮件、调用不可幂等的外部 API，LogServe 无法在 worker 崩溃或网络分区后把外部世界恢复到没发生过的状态。旧 worker 可能已经完成扣款，但还没来得及 CompleteTask。control redelivery 后，新 worker 再执行一次，就可能扣两次。

平台能做的是给用户提供工具和约束：

- 要求外部调用带业务 idempotency key。
- 推荐把副作用写到支持幂等的下游系统。
- 提供 outbox pattern，把副作用请求记录成事件，再由专门 worker 发送。
- 对不可重复副作用要求用户声明 `max_attempts=1` 或手动 compensation。
- 在文档里明确 at-least-once execution 边界。

所以 external side effect 是 exactly-once-ish 语义的边界。平台可以保证内部状态不重复提交，但不能替外部系统实现它没有的幂等能力。

## Q774. 如果需要强事务，LogServe 应该引入哪种 transaction coordinator？

要看事务范围。

如果只是 control metadata 和 shared log 的一致性，最好不要引入传统 2PC，而是让 shared log 作为唯一写入事实源，metadata view 通过 replay 或 apply 更新。这是当前 LogServe 的方向。

如果需要跨多个外部资源做原子提交，比如同时写 shared log、PostgreSQL、对象存储和第三方服务，2PC 理论上可以做，但工程代价很高。参与者要支持 prepare/commit/abort，还要处理 coordinator 崩溃后的阻塞问题。对象存储和外部 API 通常不支持真正 2PC。

更实用的选择是：

- shared log 内部事务：用 log transaction marker。
- DB 与事件发布：用 transactional outbox。
- 长事务和外部副作用：用 Saga/compensation。
- 强一致元数据：用 Raft/etcd/PostgreSQL serializable transaction 做 coordinator。

如果 LogServe 要支持跨 stream 原子 append，我会优先在 logd 内部实现 transaction coordinator，避免一上来就引入通用分布式事务。事务状态也写进 shared log，通过 `Prepare`、`Commit`、`Abort` 或 commit marker 来解释。

## Q775. 如果多个 stream 需要原子 append，如何设计 commit marker？

可以把跨 stream append 拆成 data records 和 commit marker。

假设一次操作需要同时写 `wf:<id>` 和 `task:<id>`。可以先写带同一个 `txn_id` 的 pending records：

```text
wf:123    StepScheduled(txn_id=T1, pending=true)
task:456  TaskSubmitted(txn_id=T1, pending=true)
```

然后写一个 commit marker：

```text
txn:T1    TransactionCommitted(streams=[wf:123, task:456])
```

replay 时，读到 pending record 不能马上生效。只有看到对应的 commit marker，才把这批 records 应用到 materialized view。如果看到 abort marker，就丢弃 pending records。如果只有 pending 没有 commit，说明事务未完成，恢复时不应用。

更严格的设计还要处理顺序和可见性：

- commit marker 必须进入一个全局有序日志，或进入所有参与 stream 都能检查到的事务 stream。
- ReadLog 默认要隐藏未 committed records，或者返回时带 `pending` 状态。
- idempotency key 要绑定 txn_id，避免客户端重试产生两组 pending records。
- compaction 前要保留 commit marker，否则 replay 不能判断旧 pending records 是否生效。

这个设计的代价是读路径复杂了。当前 LogServe 没做跨 stream 原子事务，主要靠 log-first 和单对象 stream 保持语义简单。只有当 workflow scheduling 或多 actor 原子更新真的需要时，才值得引入 commit marker。
