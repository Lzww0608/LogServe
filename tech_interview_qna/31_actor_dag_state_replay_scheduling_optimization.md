# 31. Actor/DAG、状态机 replay 与调度优化

这一组问题把 actor 和 DAG workflow 放在一起看。它们表面上一个是“长期有状态对象”，一个是“多步骤任务图”，但底层都在处理同一类问题：状态由谁推进，事件按什么顺序生效，崩溃后怎么恢复，重试会不会重复副作用，调度器怎样在正确性边界内榨出并行度。

面试里不要把 actor 讲成“对象加队列”，也不要把 DAG 讲成“画个有向无环图”。更准确的说法是：actor 用 mailbox 给单个实体建立串行状态线；DAG workflow 用依赖图给一组 step 建立偏序调度线。两者都需要持久化事实、确定性 replay、幂等边界和可观测性。

```text
actor:
  identity + mailbox + private state + ownership + recovery

DAG workflow:
  steps + dependencies + ready rule + result references + retry policy

shared concerns:
  event log, snapshot, deterministic replay, fencing, idempotency, critical path, trace
```

LogServe 里这条线很清楚：actor 侧有 `actor:<actor_id>` stream、`ActorCommandSubmitted`、`ActorCommandApplied`、`command_seq`、owner worker 和 epoch；workflow 侧有 `WorkflowStarted`、`StepScheduled`、`StepStarted`、`StepSucceeded`、`StepFailed`、`ResultRef` 和 `__step_ref__`。这些不是装饰字段，而是恢复和一致性的证据。

## Q001. 为什么 actor mailbox 可以简化单对象状态并发？

**回答：**

actor mailbox 简化的是“同一个对象的状态到底由谁改”这个问题。普通并发对象通常会被多个线程同时调用，状态字段要靠锁、CAS 或事务保护。actor 换了一个边界：外部不能直接改状态，只能把 command 发到这个 actor 的 mailbox；actor 一次处理一个消息，状态只在这条处理路径里推进。

最小模型可以写成：

```text
many senders -> actor mailbox -> one message at a time -> state transition
```

这会把很多细碎的并发问题收束掉。比如一个账户 actor 同时收到 `Debit(10)`、`Credit(5)`、`Freeze()`，外部请求可以并发到达，但 actor 内部看到的是一条顺序流。状态机只要处理“当前状态 + 当前消息 -> 新状态”，不用在每个字段上纠结锁粒度。

它简化的不是所有并发，只是单对象内部并发：

```text
简化了:
  单个 actor state 的并发写
  同一个 key 上的状态转移顺序
  不变量检查的临界区
  状态恢复时的 replay 顺序

没有自动解决:
  跨 actor 事务
  mailbox 积压
  外部 I/O 阻塞
  消息重复投递
  owner 迁移和 split-brain
```

Ray 的 actor 文档里，同一个 actor 的方法按调用顺序串行执行，不同 actor 可以并行。Akka 也强调 actor instance 一次处理一条消息，所以 actor 内部计数器这类状态不需要 `synchronized`。这些说法背后是同一个原则：状态有单一拥有者。

放到 LogServe，`command_seq` 就是把 mailbox 顺序持久化。一个 actor command 先写 `ActorCommandSubmitted`，控制面分配递增序号，worker 完成后只有符合当前 owner/epoch 且序号正确的结果才能写 `ActorCommandApplied`。这样即使 worker 并发、重启或迟到回包，actor 的状态线仍然是一条。

**一句话**

actor mailbox 把“多线程同时改一个对象”改成“多个请求排成一条对象私有的命令流”。它用单所有者和顺序处理降低了单对象状态并发复杂度，但跨对象一致性还要另外设计。

## Q002. actor 消息顺序是否等于全局事件顺序？

**回答：**

不等于。actor 消息顺序通常只在某个 actor 的 mailbox 内有意义。它说的是“这个 actor 先处理哪条消息”，不是“整个系统所有事件谁先谁后”。

一个简单例子：

```text
sender A -> actor X: x1
sender B -> actor Y: y1
sender A -> actor X: x2
sender B -> actor Y: y2
```

actor X 里可以定义 `x1 < x2`，actor Y 里可以定义 `y1 < y2`。但 `x1` 和 `y1` 谁先发生，取决于网络、调度、日志提交和观察点。除非系统额外引入全局日志、全局序列号、事务提交时间戳或共识顺序，否则不能说它们有一个可靠全局顺序。

即使是同一个 actor，也要问清楚顺序是哪一层：

```text
send order:
  调用方发消息的顺序

enqueue order:
  runtime 把消息放进 mailbox 的顺序

processing order:
  actor 实际开始处理的顺序

commit order:
  状态变化持久化生效的顺序
```

面试里最容易说错的是“actor 保证消息顺序”。更严谨的说法是：多数 actor runtime 会给单 actor 提供某种串行处理语义，但不同 sender、不同 actor、网络重试、优先级 mailbox、reentrancy、持久化恢复都会让“顺序”变得更具体。工程系统要把自己依赖的那种顺序显式写出来。

LogServe 依赖的不是内存队列里偶然的顺序，而是 `command_seq` 和 `ActorCommandApplied` 的顺序。`ActorCommandSubmitted` 表示 command 进入可恢复记录；`ActorCommandApplied` 才表示状态推进。控制面会拒绝 out-of-order completion，这比“worker 先处理哪个”更接近业务事实。

**一句话**

actor 顺序是局部顺序，不是全局事件顺序。面试里要把 per-actor ordering、commit ordering 和 global ordering 分开讲，否则很容易把 mailbox 说成一个不存在的全局时钟。

## Q003. actor 方法阻塞会如何影响 mailbox？

**回答：**

如果 actor 方法在默认执行路径里长时间阻塞，它会把这个 actor 的 mailbox 卡住。因为 actor 一次只处理一个消息，当前消息不返回，后面的消息只能排队。这个问题叫 head-of-line blocking。

时间线大概是这样：

```text
mailbox: [slow_io, get_status, cancel, heartbeat]
actor starts slow_io
slow_io blocks 30s
get_status waits
cancel waits
heartbeat waits
```

这里最烦的是，后面的消息可能本来很轻，比如查询状态或取消请求，但它们也被前面的慢 I/O 挡住。actor 的单线程语义保护了状态，却把慢操作的代价集中到了这个 key 上。

常见后果包括：

- 同一个 actor 的 p99 上升。
- mailbox depth 和 queue age 增长。
- 调用方 timeout 后重试，队列更长。
- cancel 消息进不去，慢任务继续跑。
- owner 迁移或恢复变慢，因为 in-flight command 卡住。
- 一个热点 actor 拖住 worker pool，影响其他 actor。

处理方式要看阻塞的性质。短小的 CPU 计算可以直接做；不可控 I/O、第三方 API、长时间模型推理、文件上传，最好不要占住 actor 的串行执行线程。可以把它拆成两段：actor 记录意图并派发外部任务，外部任务完成后再用消息把结果送回 actor。

```text
actor receives StartPayment
actor records payment_pending
actor dispatches external payment task
actor returns / continues mailbox
payment task completes
actor receives PaymentCompleted
actor applies final state transition
```

有些 runtime 支持异步 actor 或 reentrancy，但这不是免费午餐。reentrancy 会让前一个请求还没完成时，后一个请求进入 actor，状态中间态就暴露出来。能不用就先不用；要用，就要给可交错的方法做白名单和版本检查。

LogServe 现在用 `actorQueue` 和 actor pool 隔离 actor 执行，但 Python actor method 如果长期阻塞，仍会拖住该 actor 的后续 command。生产化时要看 `actor_queue_wait_ms`、`actor_method_duration_ms`、`actor_mailbox_depth`，并给阻塞外部 I/O 明确的 timeout 和 bulkhead。

**一句话**

actor 方法阻塞会把 mailbox 的串行优势变成串行瓶颈。可靠做法是让 actor 只推进状态，把慢 I/O 隔离出去，用后续消息把结果带回来。

## Q004. actor 状态快照和事件日志如何协同恢复？

**回答：**

事件日志记录事实，snapshot 记录某个日志位置上的状态副本。恢复时不是二选一，而是先加载最近可用 snapshot，再 replay snapshot 之后的 tail log。

没有 snapshot 时，恢复路径很直接：

```text
ActorCreated
ActorCommandApplied #1
ActorCommandApplied #2
...
ActorCommandApplied #1000000

recover = replay all events from beginning
```

这在事件少的时候没问题。actor 活了很久以后，每次迁移或重启都从头 replay，恢复时间会越来越长。snapshot 就是在某个已知位置保存状态：

```text
snapshot at command_count = 900000
then replay events 900001..1000000
```

关键点是 snapshot 必须和日志位置绑定。只存一坨 state bytes 不够，还要知道它对应哪个 `sequence_nr`、`command_count`、event offset 或 version。否则恢复时不知道从哪里继续，会出现漏放或重复放。

一个安全的恢复流程通常是：

```text
1. 找到最新可读 snapshot
2. 校验 actor_id、schema_version、state_hash、snapshot_position
3. 加载 snapshot state
4. 扫描 snapshot_position 之后的事件
5. 按顺序应用 tail events
6. 得到 current state
7. 和 metadata/materialized view 做一致性检查
```

Akka Persistence 用 snapshot 降低长事件日志的恢复时间，snapshot 之后的 replay 消息会把 actor 恢复到当前状态。它还提醒一个边界：如果旧事件已经删除，就不能随便把 snapshot load failure 当成可忽略，否则会恢复出错误状态。

LogServe 的 actor 恢复也是这个结构。`ActorSnapshotCreated` 里有 `snapshot_ref` 和 `snapshot_command_count`；恢复时可以加载 snapshot，再 replay 后续 `ActorCommandApplied`。如果 snapshot 写失败，只要完整事件日志还在，正确性还在，只是恢复慢。危险的是 trim 了旧日志以后 snapshot 又不可读，这时就不是性能问题，而是恢复事实丢了。

**一句话**

事件日志是事实来源，snapshot 是恢复加速点。正确恢复必须是 snapshot position 加 tail replay，不能把 snapshot 当成替代事件日志的独立真相。

## Q005. actor ownership 转移为什么需要 fencing？

**回答：**

actor ownership 转移需要 fencing，是为了防止旧 owner 在失去所有权后继续提交状态。没有 fencing，系统会出现 split-brain：两个 worker 都以为自己能推进同一个 actor。

典型故障窗口是这样的：

```text
T1 worker A owns actor-1 epoch=3
T2 A 执行 command #10，很慢
T3 控制面认为 A 失联，把 actor-1 转给 worker B epoch=4
T4 B 从 snapshot/log 恢复，执行 command #10 或 #11
T5 A 的旧结果迟到
```

如果 T5 的结果还能写入状态，actor 的单所有者语义就没了。旧 owner 可能覆盖新状态，重复提交外部结果，或者让 `command_seq` 回退。

fencing 的做法是给 ownership 分配单调 token，常见名字是 epoch、term、generation、lease version。每次 worker 完成 actor command 时，都必须带上自己拿到任务时的 epoch。控制面只接受当前 owner、当前 epoch 的完成：

```text
if completion.actor_epoch != current_actor_epoch:
  reject stale completion

if completion.worker_id != current_owner:
  reject stale owner

if completion.command_seq != expected_next_seq:
  reject out-of-order completion
```

lease 只能说明“在一段时间内大概率有效”，不能单独防止迟到写。fencing token 的价值在于持久化提交边界也检查它。旧 worker 就算还活着、网络恢复了、代码继续跑，它的 token 已经过期，写不进去。

LogServe 在 actor task 上带 `actor_epoch` 和 `actor_command_seq`，完成时会检查当前 actor owner 和 epoch，并拒绝 stale actor completion。这个设计比只在调度时选一个 owner 更硬，因为真正的正确性边界在 `ActorCommandApplied` 写入前。

**一句话**

ownership 转移不是改一条路由表。必须用 epoch/term 这类 fencing token 让旧 owner 的迟到结果失效，否则 actor 的“单活状态机”会在故障恢复时破掉。

## Q006. DAG workflow 为什么需要拓扑排序？

**回答：**

DAG workflow 需要拓扑排序，是因为 step 之间只有偏序，不是简单列表顺序。拓扑排序把“谁依赖谁”转成调度器能执行的 ready set：所有上游完成的节点可以运行，仍有未满足依赖的节点不能运行。

比如：

```text
extract -> normalize -> train
extract -> profile   -> report
normalize -> report
```

这里不是按文件声明顺序从上到下跑。`extract` 完成后，`normalize` 和 `profile` 都可能 ready；`report` 要等 `profile` 和 `normalize` 都完成。拓扑排序保证任何边 `A -> B` 中，A 都先于 B 生效。

更准确地说，workflow engine 里需要的不是一次性排出一个静态线性序列，而是持续维护 ready node：

```text
prepare graph
ready = nodes with no unfinished predecessors
schedule ready nodes
when a node succeeds:
  mark done
  release downstream nodes whose predecessors are all done
```

Python 的 `graphlib.TopologicalSorter` 提供了类似接口：`get_ready()` 返回当前所有 ready 节点，`done()` 标记节点完成后再释放新的 ready 节点。这个模型和真实调度器很接近。

拓扑排序还负责提前发现环。DAG 里如果有环：

```text
A depends on B
B depends on C
C depends on A
```

没有任何节点能最终满足依赖。调度器如果不在定义阶段拒绝它，运行时就会出现 workflow 永远卡住、没有 ready node、也没有明确失败原因。

LogServe 现有 workflow 里每个 `StepDefinition` 有 `DependsOn`，调度时扫描定义顺序，调用 `DependenciesSucceeded` 判断 step 是否 ready。这个实现是小规模可用的，但面试里要知道：生产级 workflow engine 通常会显式构建 dependency graph、检查环、维护 indegree/ready queue，而不是每次全量扫描。

**一句话**

拓扑排序让 workflow 从“按列表跑任务”变成“按依赖释放 ready step”。它同时保证正确性、暴露并行度，并在定义阶段发现环。

## Q007. DAG step 的结果引用如何构成依赖图？

**回答：**

DAG 的依赖不只来自 `depends_on`，也来自输入里的结果引用。一个 step 如果读取另一个 step 的输出，它们之间就有数据依赖。工程上最好让控制依赖和数据依赖一致，否则调度器可能提前运行一个输入还不存在的 step。

例如：

```json
{
  "step_id": "summarize",
  "depends_on": ["fetch"],
  "args": {
    "document": {"__step_ref__": "fetch"}
  }
}
```

这里 `summarize` 依赖 `fetch` 有两层意思：控制上要等 `fetch` 成功；数据上要把 `fetch` 的结果解析成 `summarize` 的输入。`__step_ref__` 这种字段不是普通 JSON，它是 workflow 引擎的引用语法。

如果只写结果引用，不写依赖，会有隐式依赖：

```text
B.args contains ref(A)
B.depends_on does not include A
```

调度器只看 `depends_on` 时，B 可能提前 ready。到解析输入时才发现 A 还没结果，这会把定义错误拖到运行时。更好的设计是在提交 workflow definition 时扫描所有 step ref，自动补依赖或直接拒绝不一致定义。

结果引用还决定存储边界。小结果可以内联在 workflow state 里；大结果应该落对象存储或 result store，只在状态里保存 `result_ref`。这样依赖图里流动的是“结果身份”，不是大 payload 本身。

LogServe 已经有这条语义：`args_json` 里可以出现 `__step_ref__`，`ResolveArgs` 会在调度前加载上游 step 的 `ResultJSON` 或 `ResultRef`。这也意味着 result store 的可用性会影响下游调度。上游 step 只是状态为 succeeded 还不够，下游真正运行前必须能解析到它需要的输入。

**一句话**

结果引用把数据流也变成依赖边。一个 step 读取谁的输出，就要等谁的结果可用；否则 DAG 看起来 ready，执行时却缺输入。

## Q008. workflow replay 为什么要求确定性？

**回答：**

workflow replay 要求确定性，是因为 replay 的目标不是重新执行外部世界，而是用历史事件恢复出同一个 workflow 决策过程。相同 history 重放时，workflow 代码必须产生相同的调度命令，否则引擎不知道该相信历史还是相信新代码。

Temporal 文档把这个边界说得很清楚：workflow code 必须在相同输入下，以相同顺序产生相同 Workflow API 调用；外部 API、数据库查询、LLM 调用这类非确定性操作应该放到 activity 里，因为 activity 在 replay 路径外。

一个错误例子：

```text
workflow code during first run:
  if random() < 0.5:
    schedule A
  else:
    schedule B

history records:
  Activity A scheduled

replay later:
  random() returns >= 0.5
  code wants to schedule B
```

这时 history 说 A，代码说 B。继续跑会破坏历史一致性；忽略代码又会让后续逻辑不可解释。正确做法是把随机数、当前时间、外部查询结果作为历史事件或 activity result 记录下来，replay 时读历史，不重新抽签。

确定性要求也影响代码升级。正在运行的 workflow 可能活几天、几个月甚至更久。如果你直接重排了 activity、改了 timer、换了 child workflow ID，旧 history replay 时可能对不上。生产 workflow engine 通常需要 versioning、patch marker 或 worker versioning，让旧实例按旧逻辑继续，新实例走新逻辑。

LogServe 的 workflow replay 目前更像事件归约：`workflow.Replay` 按 `WorkflowStarted`、`StepScheduled`、`StepStarted`、`StepSucceeded`、`StepFailed` 等事件重建状态。即便没有完整 Temporal 那种 workflow code replay，确定性仍然重要：同一批 log records 应该每次恢复出同一个 `WorkflowState`，`Consistent` 检查才有意义。

**一句话**

workflow replay 要求确定性，是为了让历史事件和代码决策对得上。replay 不是再跑一遍业务，而是用历史恢复状态并继续做兼容的下一步决策。

## Q009. actor replay 和 workflow replay 的共同点是什么？

**回答：**

actor replay 和 workflow replay 的共同点是：它们都把运行时状态当成历史事件的派生结果。内存里的状态可以丢，metadata view 可以重建，真正不能乱的是事件顺序和状态转移规则。

共同结构大概是：

```text
events/history/log -> deterministic reducer -> current state
```

actor replay 关注单个实体：

```text
ActorCreated
ActorCommandSubmitted
ActorCommandApplied
ActorSnapshotCreated
ActorCommandFailed
```

workflow replay 关注一组 step 和调度状态：

```text
WorkflowStarted
StepScheduled
StepStarted
StepSucceeded
StepFailed
WorkflowCompleted
WorkflowFailed
```

它们都需要解决几件事：

- 事件必须有稳定顺序。
- reducer 必须确定性。
- 已提交事实不能因为重试重复生效。
- snapshot 或 checkpoint 只能作为优化，不能改变事实。
- replay 后的状态要能和当前 materialized view 对比。
- schema 演进要兼容旧历史。

差别在状态形状。actor 通常是一个长期对象的私有状态，比如账户、会话、房间、模型实例。workflow 是一个执行实例的进度状态，比如哪些 step 已调度、哪些成功、最终结果来自哪个 step。actor 更像 per-key 状态机；workflow 更像带依赖图的任务状态机。

LogServe 里这两个 replay 路径都存在。actor 有 `actor.Replay` 和 snapshot replay count；workflow 有 `workflow.Replay` 和 `workflow.Consistent`。这说明系统设计的核心不是“内存里有个 map”，而是“log 可以重建 map”。

**一句话**

actor replay 和 workflow replay 都是事件历史归约。区别是 actor 归约单实体状态，workflow 归约多 step 调度状态；共同底线是确定性 reducer、稳定顺序和可校验恢复。

## Q010. 状态机 reducer 为什么应该无副作用？

**回答：**

reducer 应该无副作用，因为它会在 replay、恢复、测试、审计、迁移时被反复调用。只要 reducer 里发邮件、扣款、写外部系统、读当前时间或调用随机数，replay 就会改变外部世界，或者每次恢复出不同状态。

正确的 reducer 只做一件事：

```text
new_state = reduce(old_state, event)
```

它可以校验事件、更新字段、维护索引、计算派生状态，但不应该跨出状态机边界。外部副作用应该在 command handler、activity、worker task 或 outbox relay 里执行，并把结果作为事件写回。

一个危险写法：

```text
on PaymentApproved event:
  state.status = paid
  send_email(user)
```

第一次运行会发一封邮件；恢复 replay 时又发一封；做审计回放时还会发。最后你根本分不清哪一封是业务邮件，哪一封是恢复过程制造的事故。

更安全的写法是：

```text
on PaymentApproved event:
  state.status = paid
  state.need_receipt_email = true

outbox relay:
  send receipt email once with email_operation_id
```

这样 reducer 只是把“需要发邮件”这个事实放进状态或 outbox，真正发送由可幂等、可重试、可观测的外部执行器完成。

LogServe 的 replay 函数也遵循这个方向：actor replay 和 workflow replay 根据 log record 重建状态，不应该在 replay 中重新执行 Python actor method、workflow step 或 LLM 调用。`ActorCommandApplied` 和 `StepSucceeded` 里记录的是结果，replay 只应用结果。

**一句话**

reducer 无副作用，replay 才安全。它只能根据历史事件算状态；外部世界的改变必须在 replay 路径外完成，并用事件把结果带回来。

## Q011. snapshot 频率如何影响写放大和恢复时间？

**回答：**

snapshot 频率是在写路径成本和恢复成本之间做取舍。snapshot 越频繁，恢复时 tail log 越短；但每次 snapshot 都要复制状态、序列化、压缩、写存储、记录 snapshot 事件，正常写路径会变重。snapshot 越稀疏，平时写得轻，崩溃恢复时要 replay 的事件越多。

可以把成本拆开：

```text
write amplification:
  command event write
  state serialization
  snapshot object write
  snapshot metadata/event write
  optional old snapshot/log cleanup

recovery time:
  load snapshot
  deserialize state
  scan tail log
  replay tail events
  rebuild materialized view
```

如果 `snapshot_every=1`，每条 command 都 snapshot，恢复当然快，但每条写都被 snapshot 拖住。状态大时尤其明显：一次小字段更新也要写完整 state。反过来，如果永远不 snapshot，一个长生命周期 actor 运行几百万条事件后，owner 迁移和进程重启会变成漫长 replay。

实际调参不要只看“每 N 条 snapshot”。更好的触发条件会同时看：

```text
event_count_since_snapshot
state_size_bytes
snapshot_write_ms
replay_tail_events
actor_recovery_ms
actor_hotness
storage_cost
schema_version_boundary
```

热 actor 和冷 actor 策略也不同。热 actor snapshot 太频繁会影响前台延迟；冷 actor passivation 前做一次 snapshot，可能很划算。大 state 可以考虑 delta snapshot、大字段外置或分片 actor，而不是单纯调大调小频率。

LogServe 当前有 `SnapshotEvery`，默认更像机制验证参数。面试里可以说：我会先用它证明 snapshot + tail replay 的正确性，再用 `snapshot_duration_ms`、`snapshot_bytes`、`actor_recovery_ms`、`tail_events_after_snapshot` 去调整策略。只说“每 100 条存一次”不够，得把恢复 SLO 和写放大一起算。

**一句话**

snapshot 频率越高，恢复越快但写放大越大；频率越低，写路径轻但恢复慢。合理策略要按 state 大小、事件速率、恢复 SLO 和存储成本一起定。

## Q012. 如果 step 可重试但不可幂等，会发生什么？

**回答：**

如果 step 可重试但不可幂等，workflow engine 会把一次业务意图变成多次外部副作用。纯计算 step 重试通常只是多花 CPU；带副作用的 step 重试可能重复扣款、重复建单、重复发邮件、重复创建云资源。

时间线很常见：

```text
T1 step attempt #1 starts
T2 step calls external API, side effect succeeds
T3 worker crashes before reporting StepSucceeded
T4 engine sees attempt timeout / failure
T5 engine schedules attempt #2
T6 external API is called again
```

从 workflow 看，第一次 attempt 没有成功完成；从外部系统看，副作用已经发生。没有幂等 key 或业务唯一约束时，第二次 attempt 就是第二次副作用。

所以 retry policy 必须和 step contract 绑定：

```text
retryable pure step:
  safe to retry, output derived from input

retryable idempotent side-effect step:
  retry with same operation_id / idempotency key

non-idempotent side-effect step:
  do not blind retry; query status, reconcile, or require manual decision
```

一个比较稳的 step 设计会显式分出 operation id：

```text
workflow_id + step_id + input_hash = operation identity
attempt = network/execution try
```

外部 API 支持幂等时，把 operation identity 传给它；不支持时，本地先记录 `sent_unknown`，timeout 后优先查询或对账，不要马上补发。对高风险业务，宁可 workflow 暂停在 `needs_review`，也别自动重复副作用。

LogServe 的 task idempotency key 里有 `workflowID:stepID:inputHash:attempt:n`，这能区分 attempt，但如果 step 内部副作用不可幂等，仍然要在业务层使用稳定 operation id。attempt id 不能替代业务幂等 key。

**一句话**

step retry 只说明引擎会再跑一次，不说明业务效果安全。不可幂等 step 盲目重试，会把恢复机制变成重复副作用制造器。

## Q013. 如何处理长尾 step 对整个 DAG critical path 的影响？

**回答：**

长尾 step 如果在 critical path 上，会直接拉长整个 workflow；如果不在 critical path 上，可能只是消耗资源。第一步不是加 worker，而是先确认它是不是关键路径。

DAG latency 的下界由 critical path 决定：

```text
critical_path = longest dependency chain by duration
workflow_latency >= sum(duration on critical_path)
```

如果一个 30 秒 step 后面接着最终结果 step，它在关键路径上；旁路的 30 秒审计 step 如果不影响结果，就不应该阻塞主结果。调度优化要先把这两种情况分开。

处理手段有几类：

```text
缩短 step 本身:
  优化代码、缓存输入、批量请求、换更近的数据源

拆分 step:
  fan-out 成多个 shard，再 fan-in 汇总

改变依赖:
  去掉不必要的串行依赖，让无关 step 并行

隔离资源:
  给慢 step 独立 worker pool，别占住短 step

减少尾部风险:
  timeout、retry budget、speculative execution、straggler detection

改变结果语义:
  可选 step 不阻塞主结果，迟到结果异步补充
```

speculative execution 要小心。它适合纯计算或幂等读，不适合会写外部系统的 step。否则你为了对冲长尾，反而启动两个副作用 attempt。

trace 是定位关键路径的好工具。每个 step 一个 span，span 上带 `workflow_id`、`step_id`、`attempt`、`queue_wait_ms`、`run_ms`、`dependency_wait_ms`、`result_ref_bytes`。这样能看出慢是排队慢、执行慢、依赖等待慢，还是 retry/backoff 把时间拉长。

LogServe 当前 status 里有 step latency，后续如果要做更完整的 DAG 瓶颈分析，可以在 `StepScheduled` 到 `StepStarted` 之间记录 queue wait，在 `StepStarted` 到 `StepSucceeded/Failed` 之间记录 run time，再按 DAG 边回推 critical path。

**一句话**

长尾 step 要先看是不是 critical path。关键路径上的慢要缩短或拆分；非关键路径上的慢要隔离或降级，别让它无意义地阻塞最终结果。

## Q014. actor 和 workflow 哪个更适合表达长期有状态对象？

**回答：**

长期有状态对象更适合 actor。workflow 更适合表达一个有开始、有结束、由多个 step 组成的业务过程。两者都能保存状态，但状态的形状不一样。

actor 的中心是 identity：

```text
user-session-123
order-456
device-789
game-room-abc
model-instance-x
```

它可以长期存在，持续接收消息，状态随着 command 慢慢演进。你关心的是这个对象当前处于什么状态，下一个命令能不能被接受，owner 在哪里，崩溃后怎么恢复。

workflow 的中心是 execution：

```text
workflow run: train-model-2026-06-20-001
steps: fetch -> preprocess -> train -> evaluate -> publish
```

它通常有明确终态：completed、failed、canceled。你关心的是哪些 step ready，哪些 step failed，重试到第几次，最终结果来自哪个 step。

所以判断标准可以很简单：

```text
更像 actor:
  同一个实体长期在线
  后续请求围绕同一份状态不断推进
  状态没有天然结束时间
  per-key 顺序很重要

更像 workflow:
  有明确业务流程和终态
  多个 step 有依赖关系
  需要 fan-out/fan-in、重试、取消、超时
  结果由某个流程实例产出
```

订单是个有趣例子。订单本身可以是 actor，因为订单状态会长期接收付款、发货、退款、取消等 command；一次“创建订单并付款发货”的过程可以是 workflow，因为它由库存、支付、物流、通知多个 step 组成。

LogServe 同时提供 actor 和 workflow，说明它们不是互斥抽象。actor 适合 `@actor` 的长期 Python 对象和 per-actor 状态线；workflow 适合 DAG step 的任务编排和结果引用。面试里别硬选一个，要按建模对象区分。

**一句话**

长期有状态对象优先用 actor；有明确开始结束和依赖图的过程优先用 workflow。订单这类业务常常两者都需要：订单 actor 管状态，workflow 管一次跨系统流程。

## Q015. workflow engine 和 actor runtime 是否可以组合？

**回答：**

可以，而且很多复杂系统最后都会组合。workflow engine 负责跨 step 的依赖、重试、取消和结果汇聚；actor runtime 负责某个长期实体的串行状态和 ownership。它们的组合点是：workflow step 可以给 actor 发 command，actor 的状态变化也可以触发 workflow。

一个常见结构：

```text
workflow: onboarding-user
  step1 validate profile
  step2 call user actor: ReserveUsername
  step3 call billing actor: CreateTrial
  step4 send welcome email
```

这里 workflow 管的是流程，actor 管的是实体状态。`ReserveUsername` 这类操作必须围绕某个用户或命名空间串行化，用 actor 很合适；但整个 onboarding 过程有多个外部步骤，用 workflow 更清楚。

组合时要小心边界。workflow 调 actor 不是普通函数调用，它是跨状态机通信。必须回答：

```text
workflow step 的 operation id 是什么？
actor command 的 idempotency key 是什么？
actor 成功但 workflow 没收到响应怎么办？
workflow retry 会不会重复发 actor command？
actor command 成功后是否写出事件供 workflow 观察？
取消 workflow 是否需要取消 actor 中的 pending operation？
```

最稳的方式是让 workflow step 和 actor command 共用同一个稳定业务 operation id。workflow retry 同一个 step 时，不生成新的 actor command 语义，而是查询或重放同一个 command 的结果。

反过来也可以：actor 在处理某个 command 时启动 workflow，比如订单 actor 收到 `Submit` 后启动 fulfillment workflow。这里 actor 应该记录 workflow id，后续 workflow completion 再作为消息回到 actor，推动订单状态。

LogServe 现在已经有两套机制，但它们更像并列能力。面试里可以说后续可以组合：workflow step 可以提交 actor command；actor state 可以保存 `workflow_id`；两边都通过 shared log、idempotency key、result_ref 和 epoch/lease 边界保持可恢复。

**一句话**

workflow 和 actor 可以组合：workflow 编排过程，actor 保护实体状态。组合时不要把 actor call 当本地函数，必须显式设计 operation id、重试、响应丢失和取消语义。

## Q016. 如何为 actor 和 workflow 定义一致性不变量？

**回答：**

actor 和 workflow 的一致性不变量要写成可以检查的规则，而不是写成“系统状态正确”。好的不变量应该能在单元测试、恢复测试、压测和事故排查里直接验证。

actor 侧通常围绕单实体状态线定义：

```text
single active owner:
  同一 actor 在同一 epoch 只能有一个 owner 可以提交状态。

monotonic command sequence:
  ActorCommandApplied 的 command_seq 必须连续递增。

state transition validity:
  每个 command 只能从允许的状态迁移到允许的新状态。

idempotent completion:
  同一个 command_id 或 call_id 的重复 completion 不能重复推进状态。

snapshot alignment:
  snapshot_command_count 必须对应已应用日志位置。
```

workflow 侧通常围绕 step 依赖和终态定义：

```text
ready rule:
  step 只有在所有 depends_on step succeeded 后才能被调度。

single terminal state:
  workflow 只能进入一个最终状态，不能既 completed 又 failed。

step terminal stability:
  step succeeded 后，重复 completion 不能把它改成 failed。

result availability:
  succeeded step 的 ResultJSON 或 ResultRef 必须可解析给下游。

attempt bound:
  step attempts 不能超过定义的 max_attempts，除非有人工重置事件。
```

跨 actor 和 workflow 的不变量更要具体。例如 workflow step 调用 actor command：

```text
workflow_step_operation_id == actor_command_idempotency_key
actor command succeeded => workflow step may succeed
workflow retry same step => same actor command semantic identity
stale actor epoch completion must not complete workflow step
```

面试里可以说，我会把不变量分成“状态顺序”“副作用边界”“恢复一致性”“可观测检查”四类。每类都能落到测试：并发提交、owner 切换、重复 completion、snapshot 损坏、workflow replay、result_ref 丢失、step retry 等。

LogServe 已经有一些这样的检查：actor completion 会校验 epoch 和 command sequence；workflow replay 后可以和 metadata 做 `Consistent` 比对；step 成功后保存 `ResultRef` 或 inline result。继续加强的话，可以给 DAG 定义阶段加环检测、隐式 step ref 校验，以及 actor/workflow 组合时的 operation id 不变量。

**一句话**

一致性不变量要能被程序检查。actor 关注单 owner、单调 command_seq 和合法状态迁移；workflow 关注 ready rule、终态稳定、结果可用和 attempt 边界。

## Q017. 如何压测 mailbox 串行化带来的吞吐上限？

**回答：**

压测 mailbox 吞吐上限时，不能只看总 QPS。actor runtime 最大的特点是“跨 actor 可以并行，单 actor 内部串行”。所以压测要分别测热点单 actor、多 actor 均匀分布、偏斜分布和恢复场景。

第一组是单 actor 极限：

```text
one actor
N concurrent clients
same command type
small payload
no external I/O
measure:
  commands/s
  mailbox_depth
  queue_wait_ms
  execution_ms
  commit_ms
  p50/p99 latency
```

这能测出单条状态线的物理上限。即使你加到 100 个 worker，同一个 actor 也不能无限并行；它的上限来自 actor 方法耗时、序列化、日志写入、锁和调度开销。

第二组是多 actor 扩展性：

```text
actors = 1, 10, 100, 1000
commands distributed evenly
workers = 1, 2, 4, 8, 16
```

如果模型健康，actor 数增加后吞吐应该能随 worker 扩展到某个瓶颈点。瓶颈可能是全局 queue lock、metadata store、log append、snapshot store 或 Python runner 数量。

第三组是偏斜流量：

```text
90% traffic -> top 1% actors
10% traffic -> remaining actors
```

这比均匀分布更接近线上。它能暴露 hot actor、队列不公平、调度饥饿和优先级反转。看平均延迟没用，要看 top actor 的 queue age 和冷 actor 是否被饿死。

第四组是故障和恢复压测：

- worker 执行中崩溃。
- owner epoch 切换。
- 旧 completion 迟到。
- snapshot 写入变慢。
- 日志 tail 很长。
- 重复 command 和重复 completion 混入。

指标要拆成三段：

```text
queue_wait_ms:
  command 在 mailbox/ready queue 里等多久

run_ms:
  actor method 真正执行多久

commit_ms:
  完成结果写日志和 metadata 用多久
```

LogServe 当前可以围绕 `actorQueue`、`ActorCommandSubmitted`、`ActorCommandApplied`、`ActorCommandSeqRejectsOutOfOrderCompletion` 这些边界设计压测。压测结论也不要只说“吞吐多少”，要说明瓶颈在哪：单 actor 串行、全局队列扫描、log I/O、StateJSON 大小，还是 worker pool 数量。

**一句话**

mailbox 压测要分清单 actor 上限和多 actor 扩展性。单 actor 吞吐受串行状态线限制，多 actor 才能靠并行扩展；指标必须拆 queue wait、run time 和 commit time。

## Q018. 如何用 trace 显示 DAG 的瓶颈路径？

**回答：**

用 trace 显示 DAG 瓶颈路径，关键是让每个 workflow run、每个 step、每次 attempt 都在 trace 里有位置，并且能从 span 时间和 DAG 边还原 critical path。只画一串 HTTP 调用不够，因为 DAG 有并行分支和异步调度。

一个可用的 trace 结构是：

```text
root span: workflow run
  span: scheduler decision
  span: step A attempt 1
  span: step B attempt 1
  span: step C attempt 1
  span: fan-in wait for A,B,C
  span: step D attempt 1
```

每个 step span 至少带这些属性：

```text
workflow.id
workflow.name
step.id
step.attempt
depends_on
queue_wait_ms
run_ms
result_ref
input_hash
worker.id
retry_reason
```

OpenTelemetry 里 span 表示一个有开始和结束的工作单元，span attributes 可以挂元数据，span links 可以表达异步因果关系。workflow 调度很适合用 links：调度 span 和 worker 执行 span 可能不在同一个调用栈里，但它们有同一个 workflow/step 因果关系。

瓶颈路径不是“最长的单个 step”，而是从入口到终态的最长依赖链。分析时要把每个 step 的耗时拆开：

```text
step_total = dependency_wait + queue_wait + run + result_materialize + retry_backoff
```

如果 critical path 上主要是 queue_wait，说明 worker 或资源池不够；如果主要是 run，说明 step 本身慢；如果 retry_backoff 占比大，说明失败重试在拉长路径；如果 fan-in wait 长，说明某个分支是 straggler。

展示上可以给每个 step 标注 earliest start、actual start、finish、slack：

```text
slack = latest_allowed_finish - actual_finish
```

slack 接近 0 的节点在关键路径上。非关键路径 step 即使很慢，只要有 slack，不一定影响最终延迟。这个判断能避免盲目优化无关 step。

LogServe 现在有 workflow status 和 step latency，后续如果接 OpenTelemetry，可以在 `StepScheduled` 创建 producer/internal span，在 worker 执行时创建 consumer/internal span，用 `workflow_id` 和 `step_id` 关联。这样 UI 上不只看到“workflow 慢”，还能看到慢在依赖等待、队列、执行还是结果存储。

**一句话**

DAG trace 要把 step attempt、依赖边、队列等待、执行耗时和 retry/backoff 都画出来。瓶颈路径是最长依赖链，不是肉眼看到的单个慢 span。

## Q019. 如何在面试中区分状态恢复、任务重试和业务补偿？

**回答：**

这三个概念经常混在一起，但它们解决的问题完全不同。面试里最好先给边界。

```text
状态恢复:
  系统崩溃后，根据日志、snapshot、checkpoint 把内存状态恢复出来。

任务重试:
  某个执行 attempt 失败或结果未知后，再尝试完成同一个技术任务。

业务补偿:
  业务步骤已经生效，但后续流程失败，于是执行反向或修正动作。
```

状态恢复不应该重新产生外部副作用。actor replay、workflow replay 都应该读取历史事件，重建状态。它回答的是：我现在应该处于什么状态？比如 LogServe 读取 `ActorCommandApplied` 和 `StepSucceeded` 重建 metadata view。

任务重试回答的是：这个任务还要不要再执行一次？它需要 retry policy、deadline、attempt、幂等 key 和错误分类。一个 step 因 worker crash 没有上报成功，引擎可能重试；但如果 step 已经调用过外部支付，就不能把重试当成普通函数重跑。

业务补偿回答的是：已经发生的业务效果要怎么处理？比如库存已预留但支付失败，释放库存是补偿；邮件已发出但订单取消，发更正邮件是补偿；扣款成功后发现订单失败，退款或冲正是补偿。补偿不是 rollback，它本身也是新的业务动作，也会失败，也要幂等。

可以用一个例子串起来：

```text
workflow 执行到 pay step
worker 崩溃

状态恢复:
  replay history，知道 pay step 处于 started 或 unknown

任务重试:
  根据 retry policy 决定是否再次调度 pay step
  如果支付有 operation_id，就用同一个 id 查询或重试

业务补偿:
  如果支付已成功但后续发货失败，走退款/冲正流程
```

面试里我会主动说：恢复是系统内部状态问题，重试是执行可靠性问题，补偿是业务语义问题。把三者分开，才能避免用 replay 重做副作用、用 retry 代替对账、用补偿假装数据库回滚。

**一句话**

状态恢复让系统知道“发生过什么”；任务重试尝试“把同一个技术动作完成”；业务补偿处理“已经发生的业务效果如何修正”。三者不能互相替代。
