# 2. Actor model、mailbox、状态机与快照

这一组问题讨论 actor 的基本语义：状态归谁所有，消息怎么排队，顺序怎么定义，mailbox 变长以后怎么反压，actor 崩溃以后状态如何恢复。

面试里要避免把 actor 讲成“一个对象加一个队列”这么轻。更准确的说法是：actor 把共享可变状态改成了“按地址发送消息，由拥有者顺序处理”。这个模型最重要的不是语法，而是 ownership。状态不再被任意线程直接读写，而是由 actor 自己在处理消息时修改。

可以先抓住几条边界：

```text
actor identity:
  外部通过 actor id / address / handle 找到 actor，不直接拿内部对象指针。
mailbox:
  多个 sender 的并发请求先进入队列，actor 一次处理一个消息。
state machine:
  actor 的内部状态只在消息处理路径里推进，每条消息相当于一个 command。
ordering:
  同一个 actor 内可以定义顺序，不同 actor 或不同 sender 之间通常没有全局顺序。
recovery:
  内存状态丢了以后，要靠事件日志、snapshot、checkpoint 或持久状态恢复。
backpressure:
  mailbox 不能无限长。队列长度、等待时间、deadline、优先级和拒绝策略都要设计。
```

Akka 官方文档把 actor 拆成 mailbox、behavior、messages、execution environment 和 address，并强调同一个 actor 一次只处理一个消息；Erlang 文档把进程消息加入 message queue 的顺序讲得很细；Ray 文档也说明同一个 actor 的方法按调用顺序串行执行，不同 actor 可以并行。LogServe 当前实现更接近一个持久化 actor runtime：`actor:<actor_id>` shared log 是状态真相，metadata 只是 materialized view，控制面用 `command_seq`、owner worker、epoch fencing、snapshot_ref 和 logical trim 来保证可恢复的顺序状态机。

## Q001. actor model 的基本思想是什么？

**回答：**

Actor model 的基本思想是：把并发系统拆成一组独立的 actor，每个 actor 有自己的身份、mailbox 和内部状态；外部不能直接改它的状态，只能给它发消息。Actor 收到消息后，按自己的逻辑修改状态、发送新消息，或者创建其他 actor。

这和普通共享内存并发的思路不一样。共享内存模型里，多个线程可能同时拿到同一个对象，然后靠锁、CAS、条件变量去保护状态。Actor model 换了一个方向：先把状态的 ownership 收回来，让一个 actor 成为这份状态的唯一修改入口。并发请求可以很多，但它们要变成消息，先进 mailbox，再由 actor 一个个处理。

一个最小的 actor 可以这样理解：

```text
Actor = identity + mailbox + behavior + private state

sender --message--> actor mailbox --one by one--> behavior(state, message)
```

这里的 `behavior` 不是普通函数那么简单。它代表 actor 在当前状态下如何响应消息。比如一个计数器 actor 收到 `Inc` 就把值加一，收到 `Get` 就返回当前值。这个状态只属于这个 actor，外部线程不能直接写 `counter.value`。

面试时可以这样说：

```text
actor model 的核心不是“开很多线程”，而是“把共享可变状态变成消息驱动的单所有者状态机”。Actor 有地址和 mailbox，外部通过异步消息交互；actor 自己顺序处理消息并更新内部状态。这样可以把很多数据竞争问题转成消息顺序、队列容量和失败恢复问题。
```

它解决的是“状态归属不清”带来的并发复杂度。比如用户余额、游戏房间、订单状态、workflow instance、在线会话、长生命周期模型实例，这些对象都有一个共同点：同一个 key 上的操作必须按某种顺序推进。Actor model 很适合表达这种“每个 key 一条串行状态线”的场景。

但 actor model 不是万能并发药。它只是把问题移动了。你少写了很多锁，但要认真处理 mailbox 积压、消息超时、重试去重、actor crash、状态持久化、跨 actor 事务、消息协议演进这些问题。

放到 LogServe 里，actor 的基本思想落在 `@actor` Python 类和控制面 actor runtime 上。SDK 创建一个 `Counter` actor 后，`counter.inc()` 会变成 actor command，写入 `actor:<actor_id>` stream，再进入任务调度。控制面给同一个 actor 的 command 分配单调 `command_seq`，worker 执行 actor method 后回传新的 `ActorStateJson`，控制面只有在顺序正确、owner/epoch 正确时才写 `ActorCommandApplied`。所以 LogServe 的 actor 不是一个普通 Python 对象，而是 shared log 驱动的可恢复状态机。

## Q002. actor 和普通对象的主要区别是什么？

**回答：**

普通对象和 actor 的区别，主要在调用方式、状态访问、并发边界和生命周期。

普通对象的调用是同步方法调用。调用方拿到对象引用后，直接执行：

```go
counter.Inc()
value := counter.Get()
```

这意味着调用方的线程进入了对象方法。只要多个线程拿到同一个对象引用，它们就可能同时进入方法，除非对象内部自己加锁。对象的正确性通常依赖“谁能拿到引用”“哪些方法加锁”“锁保护的范围是否完整”。

Actor 的调用更像发消息：

```text
counter <- Inc(replyTo)
counter <- Get(replyTo)
```

发送消息不会把调用方线程带进 actor 内部。调用方只是把请求交给 actor 的 mailbox，结果通常通过 reply message、future、promise 或 object ref 返回。Actor 自己决定什么时候处理消息。外部看不到 actor 的内部字段，也不应该绕过 mailbox 直接改状态。

几个关键差异可以这样整理：

```text
普通对象：
  通过引用直接调用方法；
  方法运行在调用方线程或当前执行流里；
  多线程并发调用时需要锁保护；
  对象位置通常是进程内内存地址；
  崩溃恢复不是对象模型本身的一部分。

actor：
  通过地址、id 或 handle 发送消息；
  actor 顺序处理自己的 mailbox；
  内部状态由 actor 独占修改；
  actor 可以被调度到不同线程、进程或节点；
  失败恢复、监督、持久化和迁移通常是 actor runtime 的一部分。
```

这个差异会改变很多工程细节。普通对象的方法返回值很自然；actor 里返回值不是直接返回，而是另一个消息或异步结果。普通对象的异常会沿调用栈传播；actor 之间没有共享调用栈，失败要变成错误消息、失败事件、supervision 或重试策略。普通对象如果被多个线程同时调用，需要锁；actor 内部通常不需要锁，但 mailbox 和调度器需要保证同一 actor 不被并发执行。

LogServe 里这个边界很明显。Python SDK 暴露出来的 `counter.inc()` 看起来像普通方法，但它不是本地对象方法调用。这个调用会提交到控制面，写 `ActorCommandSubmitted`，再作为 actor task 交给 owner worker。worker 在 Python runner 里执行 method，返回 result 和新的 actor state。控制面写 `ActorCommandApplied` 后，actor 的 materialized state 才推进。

所以面试里可以说：

```text
actor 可以长得像对象，但语义不是对象。普通对象是“拿引用直接调用”，actor 是“拿地址发送消息”。普通对象的状态保护靠调用方纪律和锁，actor 的状态保护靠 mailbox 串行化和 runtime 调度。分布式 actor 还多了 location、ownership、recovery 和 fencing 的问题。
```

最容易答错的是把 actor 说成“对象的异步版”。异步只是表面。Actor 的核心差异是隔离和所有权：外部不能任意进入 actor 内部状态，所有改变必须经过消息协议。

## Q003. actor mailbox 解决什么并发问题？

**回答：**

Mailbox 解决的是“多个并发请求同时修改同一份状态”的问题。它把并发进入变成排队进入，让同一个 actor 的状态更新有一个明确顺序。

假设没有 mailbox，有 1000 个并发请求同时调用：

```text
counter.value = counter.value + 1
```

如果这些请求真的并发读写同一个变量，就会出现 lost update。两个请求可能都读到 `41`，都写回 `42`，最后少加了一次。用锁当然能解决，但每个对象、每条路径、每个异常分支都要认真管理锁。Mailbox 的思路是让这些请求先排队：

```text
Inc #1
Inc #2
Inc #3
...
Inc #1000
```

Actor 每次只拿一个消息处理，处理完再拿下一个。这样 `value` 的变化就是：

```text
0 -> 1 -> 2 -> 3 -> ... -> 1000
```

它主要解决四类问题。

第一，数据竞争。外部请求不能直接同时写 actor state，actor 内部也不需要给每个字段加锁。

第二，不变量被打破。比如订单状态必须从 `CREATED` 到 `PAID` 再到 `SHIPPED`，不能两个线程一个取消订单、一个发货同时执行。Mailbox 让状态转换有顺序，状态机可以检查当前状态再决定是否接受消息。

第三，按 key 串行化。很多系统不是全局都要串行，而是同一个用户、同一个房间、同一个 actor 要串行。Mailbox 天然把串行范围缩到 actor 粒度，比一个全局大锁好。

第四，可恢复顺序。只要 mailbox 的输入顺序或 command sequence 被记录下来，系统就可以在 crash 后 replay。没有顺序，恢复时就不知道哪个状态变化先发生。

LogServe 的 mailbox 不是单纯的内存 channel。它有三层保护：

```text
提交阶段：
  submitActorCommand 对同一个 actor 拿短锁，分配单调 command_seq。

调度阶段：
  PollTask 调 actorMailboxReady，只把 command_seq == actor.command_count + 1
  的 actor task 发给 owner worker。

执行阶段：
  worker 侧 actor pool 还有 per-actor lock，避免同一 worker 内同一 actor 并发执行。
```

集成测试里有一个很直接的验证：1000 个并发 `inc()` 提交给同一个 `Counter` actor，最后 `get()` 返回 `1000`。这说明 mailbox 至少在当前单机验证范围内把同一个 actor 的并发修改串成了一条状态线。

不过 mailbox 不等于持久化队列，也不等于全局事务。Mailbox 解决的是 actor 内部状态的串行入口。它不自动解决下游副作用幂等、不自动清理无限积压、不自动保证多个 actor 之间的事务顺序。

## Q004. actor 是否天然避免所有并发 bug？

**回答：**

不会。Actor 能减少一类并发 bug，特别是同一份状态被多个线程同时修改的 bug，但它不会天然避免所有并发问题。

它比较擅长避免的是：

```text
同一个 actor 内部状态的数据竞争；
同一个 key 的 lost update；
状态机转换被并发打断；
锁顺序错误导致的部分死锁；
读写锁粒度不清导致的不变量破坏。
```

但 actor 会暴露另一批问题。

第一，消息顺序误解。很多人以为 actor 系统有全局顺序，其实通常没有。最多只能说某个 actor 的 mailbox 有接收顺序，或者同一个 sender 到同一个 receiver 有顺序保证。不同 sender 并发发来的消息，先后顺序取决于调度、网络、runtime 和接收端入队时刻。

第二，mailbox 无限增长。没有 backpressure 的 mailbox 只是把崩溃推迟。请求进来得比 actor 处理得快，队列会越来越长，最终表现为延迟暴涨、内存上涨、GC 压力、超时和重试风暴。

第三，跨 actor 协调。两个 actor 各自内部串行，不代表跨 actor 操作天然原子。转账、库存扣减、订单支付这类操作如果跨多个 actor，仍然要 saga、两阶段协议、幂等补偿或事务日志。

第四，阻塞和 reentrancy。Actor 处理消息时如果同步等待另一个 actor，而另一个 actor 又等回来，就可能形成逻辑死锁。某些 actor runtime 支持 reentrant call 或 async continuation，这会让“同一时刻只处理一个消息”的直觉变复杂。

第五，消息内容可变。如果发送的是共享指针、可变 slice、可变 map，发送方发完以后继续改，接收方看到的内容可能不是发送时的快照。Actor 模型通常假设消息是不可变值，或者跨进程时会序列化复制。进程内 actor 如果传共享引用，这个假设就破了。

第六，失败和副作用。Actor crash 后，消息是否重投？已经执行的外部副作用是否重复？旧 owner 是否还能写回结果？这些都不是 actor 语法自动解决的。

LogServe 的实现也体现了这个边界。它通过 `command_seq` 和 `actor.command_count + 1` 防止同一个 actor 的 command 乱序应用，通过 owner worker 和 epoch fencing 拒绝旧 worker 的 stale completion。但 README 里也明确说它提供的是 exactly-once-ish actor command application，不是严格分布式 exactly-once execution。worker 失败或 redelivery 之后，actor method 可能被执行多次；控制面尽量只接受一个有效顺序下的状态提交。

面试里可以这样答：

```text
actor 不是并发 bug 免疫系统。它把共享内存竞争变成了消息协议、mailbox 容量、顺序语义和恢复语义问题。Actor 内部状态通常更安全，但跨 actor 协调、无限队列、消息重复、超时、外部副作用和 crash recovery 仍然要单独设计。
```

这个回答比简单说“actor 避免锁”更稳。面试官通常想听到边界，而不是口号。

## Q005. actor 内部状态为什么通常只由单线程或单逻辑流修改？

**回答：**

因为 actor 想保护的是状态不变量，而不只是某个字段。单线程或单逻辑流修改，可以让 actor 的状态转换像一台状态机一样推进。

比如一个 actor 维护这些字段：

```text
balance
last_txn_id
status
reserved_amount
```

一次消息处理可能要同时检查余额、写交易号、修改状态、更新预留金额。如果多个线程同时改这些字段，即使每个字段都是 atomic，也可能破坏组合不变量。真正需要保护的不是单个变量，而是“这几个字段之间必须一致”。

单逻辑流修改有几个好处。

第一，状态转换容易推理。处理 `Pay` 消息时，actor 可以看当前 state，决定生成什么新 state。中间不会被另一个消息插进来。

第二，锁范围更清楚。actor 内部通常不用为每个字段加锁，也不用设计复杂的 lock ordering。只要 runtime 保证同一 actor 不并发处理两条消息，内部代码就像普通单线程代码。

第三，恢复更简单。状态变化可以表示成一串 command/event：

```text
S0 --m1--> S1 --m2--> S2 --m3--> S3
```

崩溃后从 snapshot 或初始状态开始 replay，就能回到同样的状态。前提是消息处理逻辑确定，event handler 没有不可重复的副作用。

第四，cache locality 更好。同一 actor 的状态通常被同一条执行流反复访问，不会在多个 CPU core 之间频繁抢 cache line。

这里说“单线程”不一定是固定一条 OS thread。很多 actor runtime 用线程池调度 actor：这一次消息可能在线程 A 上执行，下一次消息可能在线程 B 上执行。关键不是 thread identity，而是同一 actor 同一时刻只有一个 message handler 在运行。更准确的词是 single logical flow。

LogServe 里也是这个思路。Actor method 实际在 worker 的 Python runner 里执行，但控制面只接受下一条 `command_seq` 的结果。`completeActorCall` 会检查：

```text
request worker == current owner worker
request epoch == current actor epoch
command_seq == actor.command_count + 1
```

检查通过以后，控制面才写 `ActorCommandApplied`，然后更新 `CommandCount` 和 `StateJSON`。worker 本地还用 per-actor lock，防止同一个 worker 的 actor pool 同时执行同一个 actor。真正推进 actor 状态的路径很窄，这是它能恢复和验证顺序的原因。

面试里可以补一句：

```text
actor 内部状态不是因为“线程不安全”才单线程修改，而是因为状态机不变量需要一个清晰的推进点。只要有多个逻辑流同时改状态，就又回到了锁和事务的问题。
```

这个点很重要。Actor model 的价值不是逃避同步，而是把同步边界放在 mailbox 和调度器里。

## Q006. actor 消息顺序如何定义？

**回答：**

Actor 消息顺序要分层说。不能笼统说“actor 保证顺序”，要说明是哪一种顺序。

最基础的是 receiver mailbox order。消息到达某个 actor 后，会进入这个 actor 的 mailbox。Actor 通常按 mailbox 顺序一次处理一条消息。Akka 文档里描述的过程就是消息进入队列，actor 被调度，取出队首消息，修改内部状态，再结束本轮执行。

第二层是 sender 到 receiver 的顺序。很多 actor runtime 会保证同一个 sender 发给同一个 receiver 的消息保持发送顺序。Erlang 官方文档说，如果没有启用 priority message，message queue 的顺序反映接收到的 signal 顺序；同一个 sender 对应的消息也会按发送顺序进入队列。注意这里有条件：没有 priority message，且讨论的是同一个 sender 到同一个 receiver。

第三层是应用定义的 sequence。工程上如果真的关心顺序，不能只依赖网络到达顺序。更稳的做法是给消息加 `seq`、`version`、`epoch` 或 log position。Actor 应用消息时检查：

```text
message.seq == current_seq + 1
```

不满足就等待、拒绝、重排或触发恢复。

LogServe 采用的就是应用层 sequence。控制面在 `submitActorCommand` 里给每个 actor command 分配单调 `command_seq`。`PollTask` 调用 `actorMailboxReady`，只有当：

```text
task.ActorCommandSeq == actor.CommandCount + 1
```

时才把 actor task 发给 owner worker。完成时 `completeActorCall` 还会再次检查同样的顺序条件。这样即使某个后提交的任务先被 worker 看到，也不能越过前一个 command 更新 actor state。

这个顺序可以用一个例子讲清楚：

```text
Actor 当前 command_count = 7

mailbox 里有：
  command_seq=8  inc
  command_seq=9  inc
  command_seq=10 get

只有 seq=8 可以被 dispatch/apply。
seq=9 和 seq=10 即使已经在全局队列里，也必须等 seq=8 applied 后再执行。
```

Ray 官方文档也给了类似的直觉：不同 actor 的方法可以并行，同一个 actor 的方法按调用顺序串行执行，并共享状态。这个说法适合入门，但面试里最好再往下讲一层：真实系统必须说明“调用顺序”在哪里被定义，是 client handle 的发送顺序、服务端入队顺序，还是持久化日志里的 sequence。

所以可以这样回答：

```text
actor 消息顺序通常指单个 actor mailbox 内的处理顺序，不是整个系统的全局顺序。可靠系统会把顺序显式化，比如给每个 actor command 分配单调序号，并在应用状态时检查 current_seq + 1。LogServe 的 command_seq 就是这个角色。
```

## Q007. 不同 sender 发来的消息是否有全局顺序？

**回答：**

通常没有。不同 sender 发给同一个 actor 的消息，最终会在 receiver 的 mailbox 里形成一个顺序，但这个顺序不是全局物理时间顺序，也不是所有 sender 都共同认可的顺序。它只是接收端看到的入队顺序，或者应用层给它们分配出来的顺序。

举个例子：

```text
sender A 在 10:00:00.001 发送 A1
sender B 在 10:00:00.002 发送 B1
```

你不能因此断言 actor 一定先处理 A1 再处理 B1。A1 和 B1 可能走不同线程、不同连接、不同节点、不同网络路径。B1 可能先到 receiver，A1 可能排在后面。如果 actor runtime 或控制面在服务端统一分配 sequence，那最终顺序取决于哪个请求先拿到序号，而不是客户端本地时钟。

这也是为什么分布式系统里很少直接谈“真实全局顺序”。更常见的是几种弱一些但可实现的顺序：

```text
per-sender order:
  同一个 sender 发给同一个 receiver 的消息保持顺序。

per-actor order:
  某个 actor 自己的 mailbox 或 command log 有顺序。

per-partition order:
  同一个 key / shard / stream 内有顺序。

log order:
  谁先成功 append 到同一条 log，谁就排在前面。

causal order:
  如果 B 是看到 A 的结果后才发送的，那么 B causally after A。
```

面试官问这个问题，通常是在看你是否会把 actor 顺序夸大。正确说法是：actor 给单个 receiver 提供一个串行处理点，但不制造全系统全序。如果业务需要确定顺序，就要在业务入口定义它。

LogServe 的做法是：同一个 actor 的顺序由控制面分配 `command_seq`。多个客户端并发调用同一个 actor 时，谁先进入 `submitActorCommand` 的 per-actor short lock，谁拿到更小的 `command_seq`。这个顺序是 LogServe 承认的 actor command order。它不是客户端本地时间顺序，也不是不同 actor 之间的顺序。

这个边界很实用。比如两个请求同时对同一个 actor 调 `inc()`，最终结果一定会从 `n` 到 `n+1` 再到 `n+2`，但你不能仅凭两个 sender 的本地发送时间判断谁是第一个。如果调用方需要强因果关系，应该等第一个调用成功后再发第二个，或者在消息里带业务版本，让 actor 自己检查。

面试里可以这样总结：

```text
不同 sender 没有天然全局顺序。Actor 只能在接收端把消息排成一条本地顺序；如果业务需要确定顺序，要用服务端 sequence、log offset、version 或 causal dependency 明确表达。LogServe 对同一个 actor 用 command_seq 定义顺序，对不同 actor 不承诺全局顺序。
```

## Q008. mailbox 过长时会发生什么？

**回答：**

Mailbox 过长不是小问题。它说明到达速率已经超过 actor 的处理速率。短时间突刺可以靠队列吸收，长期过长就是过载。

最直接的后果是延迟上升。排在队尾的消息要等前面所有消息处理完。如果 actor 每秒只能处理 100 条消息，mailbox 里已经有 10,000 条，那么新消息即使本身只要 1ms 执行，也可能要等很久才轮到。用户看到的是超时，不是“系统仍然在努力处理”。

第二个后果是内存压力。Mailbox 存消息、payload、reply handle、deadline、trace context。消息越多，占用内存越多。GC 语言里还会放大 GC pause 和 heap scan 成本。更糟的是，消息里如果带大 payload，mailbox 会变成内存堆积点。

第三个后果是消息过期。很多消息有 deadline。等它排到队头时，调用方可能早就超时了。如果 actor 继续执行，系统会做无效工作，还可能写出调用方已经不需要的副作用。

第四个后果是 head-of-line blocking。慢消息排在前面，后面的快消息也被挡住。尤其是 actor 里混了读请求、写请求、慢 I/O、后台清理任务时，mailbox FIFO 会把长尾传播给所有消息。

第五个后果是恢复变慢。如果 mailbox 或 command log 很长，actor crash 后要判断哪些消息已应用、哪些还在等待、哪些需要重投。持久化 actor 还要 replay 事件或 snapshot tail。队列越长，恢复边界越难看清。

第六个后果是重试风暴。调用方超时后重试，新请求又进 mailbox，旧请求可能还没处理完。没有幂等 key 和去重时，actor 会处理一堆重复消息；有幂等但没有 admission control 时，系统仍然会被重复请求占满。

LogServe 当前用几种方式压住这个问题：

```text
全局队列：
  enqueueTaskWithMetadata 会检查 queue_high_watermark，超过高水位直接 backpressure。

worker 本地队列：
  taskQueue / llmQueue / actorQueue 都是 bounded channel，容量来自 worker capacity。

actor 调用：
  CallActor 有 TimeoutMs，调用方不会无限等结果。

actor 顺序：
  command_seq 确保后续 command 不会越过前面的 command 应用。
```

但也要说清楚边界：LogServe 现在没有一个独立的 per-actor mailbox depth 指标，也没有按 actor_id 分队列的调度索引。`docs/plan.md` 已经指出，当前全局 `queue []string` 在大 backlog 下会让 `PollTask` 扫描成本变高；actor 调度最好按 actorID 建 pending 队列，让 owner worker 只检查自己拥有的 actor 候选任务。

面试里可以这样答：

```text
mailbox 过长时，问题不是“队列还没满所以没事”，而是系统已经在用等待时间透支容量。线上症状通常是 p99 上升、超时增加、内存和 GC 上升、重复请求增加、actor 恢复变慢。处理方式是 bounded mailbox、deadline-aware drop、per-actor 水位、优先级隔离、幂等去重和明确的 backpressure。
```

不要把 mailbox 当成垃圾桶。队列的作用是吸收短抖动，不是隐藏长期过载。

## Q009. actor 如何做 backpressure？

**回答：**

Actor 做 backpressure 的目标，是在 actor 处理不过来时把压力挡在入口，而不是让 mailbox 无限增长。它可以在几个位置做。

第一，bounded mailbox。最直接的方法是给每个 actor 或每类 actor 设置 mailbox 上限。超过上限后，新消息不能无条件入队。可以返回错误、返回 retry-after、丢弃低价值消息，或者让调用方等待一个很短的时间。

第二，deadline-aware admission。消息如果已经快过期，就不要进 mailbox。比如请求只剩 20ms deadline，而 actor 当前 oldest message age 已经 2s，这个请求入队没有意义。更好的做法是直接拒绝，让上游快速失败。

第三，按 actor 做热点隔离。一个热点 actor 不应该把整个 actor runtime 拖死。可以设置 per-actor queue limit、per-tenant quota、per-key token bucket，或者把热点 actor 拆分成 shard actor。否则一个房间、一个用户、一个模型实例过热，会占满全局队列和 worker。

第四，按消息类型区分优先级。读请求、写请求、系统控制消息、恢复消息、心跳消息不一定应该混在一个 FIFO 里。比如 stop、snapshot、handoff 这类控制消息如果永远排在业务消息后面，过载时 actor 可能连自救都做不了。但优先级也不能滥用，Erlang 文档对 priority message 就提醒过：大量优先消息说明协议设计有问题。

第五，异步协议别无限等待。Ask pattern、future、promise 都要有 timeout。调用方超时后，reply channel 或 alias 要能失效，避免迟到回复继续占资源。

第六，外部副作用要限流。Actor 串行处理自己状态，不代表可以无限打数据库、对象存储、LLM 服务或第三方 API。下游慢时，actor 也要用 circuit breaker、rate limiter 或 bulkhead 控制副作用。

LogServe 里的 actor backpressure 目前是组合式的：

```text
全局 admission:
  queue_high_watermark 防止控制面任务队列无限增长。

本地 executor:
  worker 有 actorQueue 和 ActorPoolSize，避免本地无限启动 actor 执行。

调用 deadline:
  CallActor 使用 TimeoutMs，等待结果不会无限阻塞。

ownership:
  没有 active worker 时，waitActorOwner 只等到调用 deadline。

顺序 gating:
  future command 不会越过前一个 command 执行，避免过载时乱序推进状态。
```

更进一步的生产化设计，可以加：

```text
per_actor_mailbox_depth
per_actor_oldest_message_age
per_actor_reject_total{reason}
per_actor_inflight
per_actor_snapshot_lag
per_actor_owner_handoff_total
```

拒绝策略也要对调用方清楚。比如：

```text
RESOURCE_EXHAUSTED: actor mailbox full
DEADLINE_EXCEEDED: actor cannot finish before deadline
UNAVAILABLE: no active owner worker
ABORTED: actor epoch changed, retry with fresh routing
```

面试里可以这样说：

```text
actor 的 backpressure 不只是把 mailbox 设成 bounded queue。真正要做的是 admission control：看 per-actor 队列长度、等待时间、deadline、owner 状态和下游容量，决定接收、等待、拒绝还是降级。LogServe 当前有全局 queue high watermark、worker bounded actorQueue 和调用 timeout，但如果面向生产热点 actor，还需要 per-actor 水位和指标。
```

这个回答承认了已有机制，也说清了下一步边界。

## Q010. actor crash 后状态如何恢复？

**回答：**

Actor crash 后能不能恢复，取决于状态是否有持久化来源。只存在内存里的 actor，进程一挂状态就没了。要恢复，就要把 actor 看成持久化状态机。

常见方案有三类。

第一，event sourcing。Actor 处理 command 后，把产生的 event 追加到持久化日志。恢复时从初始状态开始 replay event：

```text
S0 + Event1 + Event2 + Event3 -> S3
```

Akka Persistence 的核心思路就是持久化 actor 产生的事件，恢复时 replay 已持久化事件。这样存的不是每一刻的完整状态，而是状态变化历史。

第二，snapshot + tail replay。事件很多以后，全量 replay 会慢，所以定期保存 snapshot。恢复时先加载最近 snapshot，再 replay snapshot 之后的事件：

```text
Snapshot(command_count=1000) + Event1001 + Event1002 -> current state
```

这能显著减少恢复时间，但 snapshot 本身要有版本、schema、校验和、原子发布语义。不能写一半 snapshot 就让恢复路径读到。

第三，durable state。直接持久化最新状态，恢复时读取最新状态。这比 event sourcing 简单，但审计、回放和历史诊断能力弱一些。很多系统会混用：事件日志作为真相，metadata / DB 作为当前视图。

恢复还必须处理 in-flight 消息。Crash 发生时，可能有几种状态：

```text
消息已经入队，但还没执行；
消息已经执行，但结果还没持久化；
事件已经写入，但 reply 还没发；
旧 worker 其实没死，只是网络分区，后来又写回结果。
```

所以恢复不只是“把 state 读回来”。还要有去重、租约、epoch fencing 和幂等提交。旧 owner 的迟到完成必须被拒绝，否则新 owner 已经恢复并处理了后续 command，旧结果再写回来会把状态打回去。

LogServe 的恢复路径是比较完整的面试素材：

```text
状态真相：
  actor:<actor_id> shared log。

事件链：
  ActorCreated
  ActorOwnershipGranted
  ActorCommandSubmitted
  ActorCommandApplied
  ActorSnapshotCreated

当前视图：
  metadata actor state 是 materialized view，不是 source of truth。

恢复：
  replayActor 读 actor stream，actor.Replay 优先加载 snapshot_ref，
  再回放 snapshot 之后的 ActorCommandApplied / ActorCommandFailed。

ownership：
  owner_worker_id + epoch 表示当前 owner。
  老 worker 或老 epoch 的 completion 会被 completeActorCall 拒绝。

顺序：
  completion.command_seq 必须等于 actor.command_count + 1。
```

集成测试覆盖了几个关键点：`Counter` actor 在第一个 worker 执行 100 次 `inc()` 后，第二个 worker 接管，`get()` 仍返回 `100`；replay 出来的 actor state 和 metadata 一致；snapshot replay 比 full replay 回放的 command 更少；stale actor completion 会因为 worker id 和 epoch fencing 被拒绝。

这里要强调一个边界：这不是严格 exactly-once 执行。worker 可能在 crash 前已经执行了 method，但完成请求没被控制面接受；redelivery 后新 worker 可能再执行一次。LogServe 保证的是 actor state application 的 exactly-once-ish，也就是同一个 actor command 只有符合 idempotent log key、owner/epoch 和 command_seq 的提交会推进状态。外部副作用仍然要靠业务幂等 key 或事务 outbox 处理。

面试里可以这样答：

```text
actor crash 后恢复，不能靠 mailbox 内存。可靠做法是把 actor 作为持久化状态机：command 进入日志或队列，状态变化以 event 或 snapshot 持久化，恢复时从 snapshot 加 tail log 重建状态；in-flight command 通过 lease、epoch fencing、idempotency key 和 command_seq 处理。LogServe 的 actor:<actor_id> stream、ActorSnapshotCreated、snapshot_ref、logical trim 和 owner epoch 就是这个思路。
```

一句话：actor 的内存可以丢，actor 的历史不能丢。只要历史和顺序还在，状态就能重建。

## Q011. actor snapshot 解决什么问题？

**回答：**

Actor snapshot 解决的核心问题是恢复成本。更具体一点，它解决的是“actor 的事件日志越来越长以后，每次恢复都要从头 replay”的问题。

如果一个 actor 是 event-sourced 的，它的状态可以从事件重建：

```text
ActorCreated
ActorCommandApplied #1
ActorCommandApplied #2
...
ActorCommandApplied #1000000
```

这套模型很干净，因为日志是事实来源。问题也很直接：actor 活得越久，事件越多，恢复越慢。一个长生命周期 actor，比如用户账户、游戏房间、会话状态、模型实例、workflow instance，可能积累几十万甚至更多事件。每次 owner 迁移、进程重启、节点恢复都从头扫，启动时间会越来越不可控。

Snapshot 的做法是定期保存某个时刻的完整状态：

```text
snapshot(command_count=100000)
tail events:
  ActorCommandApplied #100001
  ActorCommandApplied #100002
```

恢复时不再从 `ActorCreated` 开始，而是：

```text
load latest snapshot
replay events after snapshot
```

它主要带来几件事。

第一，缩短恢复时间。Akka Persistence 文档也把 snapshot 的主要价值放在这里：事件日志过长会拖慢恢复，snapshot 可以让恢复从 checkpoint 附近开始。

第二，降低重启时的 I/O 和 CPU。少读很多历史事件，少做很多 JSON/protobuf 解码，少跑很多 event handler。

第三，给日志保留和 compaction 一个安全锚点。有了 `snapshot_ref + snapshot_command_count`，snapshot 之前的历史可以进入 retention 或 logical trim 的候选集合。注意是候选，不等于马上物理删除。

第四，改善热点 actor 的迁移体验。owner worker crash 后，新 owner 如果要接手这个 actor，恢复速度直接影响调用方看到的 timeout 和 p99。

第五，提供状态诊断入口。snapshot 是某一刻 actor state 的完整视图，排查“为什么 actor 变成这个状态”时，可以从 snapshot 看当前形状，再用 tail log 看最近变化。

LogServe 里这个机制比较明确。`actor:<actor_id>` stream 是状态真相，metadata 是当前视图。Actor command 每次成功应用后会推进 `CommandCount` 和 `StateJSON`。当 `CommandCount % SnapshotEvery == 0` 时，控制面把 actor state 写到 result store，拿到 `snapshot_ref`，然后写 `ActorSnapshotCreated` 事件，事件里带：

```text
snapshot_ref
snapshot_command_count
class_name
class_source
init_args
snapshot_every
```

恢复时 `actor.Replay` 会优先找最新 snapshot record，加载 `snapshot_ref` 指向的对象，把 `CommandCount` 设置到 `SnapshotCommandCount`，然后只回放 snapshot 之后的 command。集成测试里也验证了 snapshot replay 比 full replay 需要处理的 command 更少。

面试里可以这样答：

```text
actor snapshot 不是为了替代事件日志，而是为了给长生命周期 actor 一个恢复检查点。没有 snapshot，actor 可以靠日志恢复，但恢复时间会随着历史线性增长；有 snapshot 后，恢复从 snapshot state 加 tail events 开始。LogServe 的 ActorSnapshotCreated 记录 snapshot_ref 和 snapshot_command_count，后续 replay 就不用从 ActorCreated 一路扫到当前状态。
```

一句话：snapshot 解决的是恢复和保留成本，不是顺序语义本身。顺序仍然要靠 command_seq、日志位置或事件序号保证。

## Q012. snapshot 频率过高或过低分别有什么问题？

**回答：**

Snapshot 频率本质上是在写入成本和恢复成本之间取平衡。

频率过高的问题是前台路径变重。每隔很少的 command 就写一次 snapshot，会带来序列化 CPU、对象存储写入、磁盘或网络 I/O、校验、元数据更新。Actor state 如果很大，snapshot 可能比普通 event 大很多。过高频率会把“每条 command 的处理成本”抬高，吞吐下降，p99 变差。

更麻烦的是写放大。比如 actor state 是 1 MB，每 10 条 command 存一次 snapshot，那么每 1000 条 command 就要写 100 MB snapshot。事件本身可能只有几 KB。写得太勤，不但浪费存储，还会让 object store、磁盘、compaction 和备份都变重。

还有一个隐蔽问题：snapshot 可能影响 actor 的调度。Akka 文档里提到，snapshot 触发时 incoming commands 会被 stash，直到 snapshot 保存完成，这样可以避免 mutable state 在异步保存过程中继续变化。这个设计是安全的，但如果 snapshot 太频繁，actor 会经常停下来等 snapshot 落盘。LogServe 当前没有 Akka 这种通用 stash 语义，但 snapshot 写入也在 actor command 完成路径附近；频率太高同样会增加完成路径压力。

频率过低的问题相反：恢复时 tail log 太长。actor crash 后要加载 snapshot，再 replay 很长一段 command。owner transfer 变慢，控制面 bootstrap 变慢，测试里的 replay consistency check 也会变慢。长期运行后，日志保留压力也更大，因为没有新的 snapshot 锚点，旧事件不敢清理。

频率过低还会放大坏 snapshot 的影响。如果最近可用 snapshot 很旧，恢复时要读大量 tail；如果 snapshot schema 已经过时，还可能需要复杂的兼容逻辑。最糟的是系统为了省 snapshot 写入，把恢复风险推迟到故障发生时才爆出来。

比较稳的策略不是固定一个永远正确的数字，而是按 actor 特征调：

```text
事件很小、状态很大：
  snapshot 不宜太频繁，先接受较长 tail。

事件很多、状态较小：
  可以更频繁 snapshot，降低 replay 成本。

热点 actor：
  根据 command_count、tail replay time、state size、owner handoff 频率动态调整。

冷 actor：
  可以少 snapshot，甚至只在 passivation / deactivation 时 snapshot。

终态 actor：
  到 terminal state 时做一次 snapshot 或 final event，再进入清理流程。
```

LogServe 当前 `CreateActor` 里 `SnapshotEvery` 默认为 25，测试里也会显式传不同的 snapshot interval。这个默认值适合演示机制，不代表生产配置。生产里更应该看指标：

```text
snapshot_bytes
snapshot_duration_ms
snapshot_fail_total
replay_full_commands
replay_snapshot_commands
actor_recovery_ms
actor_tail_events_after_snapshot
```

面试里可以这样答：

```text
snapshot 太频繁，会把正常写路径拖重，产生序列化和存储写放大；太稀疏，恢复时 tail log 太长，owner 迁移和重启变慢。合理频率要按状态大小、事件大小、恢复 SLO 和 actor 热度调。LogServe 现在用 SnapshotEvery 控制频率，默认 25，更像机制验证；生产化应该让 snapshot duration、tail replay commands 和 recovery latency 共同决定阈值。
```

## Q013. snapshot 和事件日志的一致性如何保证？

**回答：**

Snapshot 和事件日志的一致性，关键是把 snapshot 绑定到一个明确的日志位置或 command 序号。不能只说“这是最新状态”。最新是个很危险的词，因为写 snapshot 时，新事件可能正在产生。

一个可靠 snapshot 至少要包含：

```text
actor_id
snapshot_ref / snapshot bytes
last_included_command_seq 或 last_included_log_seq
state schema version
checksum 或 content hash
created_at
```

恢复时的规则也要明确：

```text
load snapshot at seq=N
replay events where event.seq > N
ignore events where event.seq <= N
```

这样即使日志里有很多历史事件，也不会重复应用 snapshot 已经包含的变化。

写入顺序上，常见做法是：

```text
1. command event 已经持久化并应用到 actor state。
2. 把 actor state 序列化为 snapshot object。
3. snapshot object 写成功后，写一条 SnapshotCreated 事件，带 snapshot_ref 和 last_included_seq。
4. 只有 SnapshotCreated 也持久化成功，恢复路径才承认这个 snapshot。
5. 旧日志或旧 snapshot 的删除必须发生在新 snapshot 可恢复之后。
```

这个顺序有一个好处：如果第 2 步写 object 成功、第 3 步写 log 失败，只是多了一个 orphan snapshot object，恢复不会看到它。反过来，如果日志里已经出现 `SnapshotCreated`，但对象读不到，那恢复要么失败并报警，要么在配置允许的情况下回退到更旧 snapshot 或 full replay。Akka snapshot 文档也提到 snapshot 加载失败通常会让恢复失败；只有在配置为 optional snapshot 时，才会忽略坏 snapshot 并从事件重放。并且如果旧事件已经删除，不能随便把 snapshot 当 optional，否则会恢复出错误状态。

LogServe 当前实现接近这个顺序。`createActorSnapshot` 先把 `state.StateJSON` 写入 result store，拿到 ref 后写 `ActorSnapshotCreated` 到 `actor:<actor_id>` stream，事件里带 `SnapshotCommandCount`。写完 snapshot 事件后，才调用 `TrimStream` 做 logical trim。这个顺序比“先 trim 再写 snapshot record”安全，因为 trim 之前日志里已经有恢复锚点。

恢复时 `actor.Replay` 会找 `ActorSnapshotCreated`，加载 `SnapshotRef`，设置：

```text
state.CommandCount = SnapshotCommandCount
state.SubmittedCommandCount = SnapshotCommandCount
```

然后在回放 `ActorCommandApplied` / `ActorCommandFailed` 时跳过 `CommandCount <= snapshotCommandCount` 的事件。这个跳过逻辑就是 snapshot 和 tail log 不重复应用的关键。

面试里要补一句边界：

```text
snapshot 本身不能单独证明状态正确，它必须和日志序号绑定。删除或隐藏旧事件之前，要确认 snapshot object、SnapshotCreated 记录和 tail log 都可读。否则恢复时不是重复应用，就是漏应用。
```

如果让我设计 production 版，我会再加几项：

```text
snapshot content hash；
snapshot schema version；
snapshot write temp object + atomic publish；
recovery 时校验 actor_id 和 command_count；
保留至少 N 个旧 snapshot；
snapshot optional 只在完整事件仍可 replay 时允许；
trim/physical compaction 后做恢复演练。
```

一句话：snapshot 一致性不是靠“同时写”保证的，而是靠“snapshot 声明自己覆盖到哪里，恢复只从那之后继续”保证的。

## Q014. actor 状态序列化失败时如何处理？

**回答：**

要先区分是哪一步序列化失败。不同位置的处理方式不一样。

第一种是 command 结果里的新 actor state 无法序列化。比如 actor method 执行完，返回了一个包含文件句柄、socket、闭包、不可 JSON 化对象的状态。这个失败发生在状态应用之前，原则上不能写 `ActorCommandApplied`。因为一旦写了 applied event，恢复路径就会认为这个 command 已经改变了 actor state，但又没有可靠 state 可以重建。

这类错误应该让当前 command 失败，写清楚错误原因：

```text
ActorCommandFailed:
  reason = state serialization failed
  command_seq = N
```

是否推进 `command_count`，要看语义。如果失败命令被认为“已经消耗顺序位”，可以推进到 N，让后续消息继续走；如果失败命令必须重试直到成功，就不能推进，但要避免它永远堵住 mailbox。很多系统会把业务异常和系统异常分开：业务异常可以作为失败结果提交，系统序列化异常通常要 poison actor、暂停 actor 或进入人工修复流程。

第二种是 snapshot 序列化失败。这个和 command applied 不一样。只要事件日志还完整，snapshot 是优化项，不是唯一状态来源。更好的处理是记录 `SnapshotFailed`、打指标、继续使用事件日志恢复，而不是回滚已经成功应用的 command。Akka 文档里也把 snapshot 保存失败作为一个会报告给 actor 的信号；默认不会因为保存 snapshot 失败就停止或重启 actor。

第三种是恢复时 snapshot 反序列化失败。这时要看旧事件是否还在。如果旧事件完整，可以忽略坏 snapshot，从更旧 snapshot 或 full log replay 恢复。如果旧事件已经被删除或 physical compact 掉，坏 snapshot 就是严重事故，因为没有足够历史可恢复。Akka 文档也明确提醒：如果事件已经删除，就不要把 snapshot load failure 当成可忽略，否则会得到错误状态。

LogServe 现在的状态表示是 JSON bytes。`actor.NormalizeJSON` 会尝试 compact JSON；如果 compact 失败，它保留原始 bytes。这让系统不会因为 JSON 格式化失败立刻丢状态，但也意味着生产化时最好增加更严格的 schema 校验，否则坏 JSON 可能晚一点才在 Python runner 或恢复逻辑里爆出来。Snapshot 写入 result store 如果失败，`createActorSnapshot` 会返回错误；当前路径会把这个错误冒泡到 `CompleteTask`。面试里可以承认这个边界：机制验证阶段这样能暴露 snapshot 问题，生产系统通常要把“command 已应用”和“snapshot 优化失败”拆开处理，避免 snapshot store 抖动导致 actor command 卡住。

比较稳的处理策略是：

```text
command state 序列化失败：
  不写 ActorCommandApplied；
  写失败事件或标记 actor unhealthy；
  返回明确错误；
  保留原状态或按失败语义推进 command_count。

snapshot 序列化/写入失败：
  不删除旧事件；
  记录 SnapshotFailed；
  command 可以继续完成；
  后台重试 snapshot；
  指标报警。

snapshot 读取失败：
  如果完整事件还在，回退到旧 snapshot 或 full replay；
  如果事件已删除，停止恢复并报警，不能编一个状态继续跑。
```

面试里可以这样答：

```text
actor 状态序列化失败不能一概而论。新状态无法序列化时，不能提交 applied event；snapshot 失败时，只要事件日志还在，可以把 snapshot 当优化失败处理；恢复时 snapshot 读不了，只有在事件完整时才能回退 replay。LogServe 当前用 JSON state 和 snapshot_ref，后续生产化应该把 state schema 校验、SnapshotFailed 事件和可回退恢复路径补得更细。
```

## Q015. actor 方法是否应该允许阻塞 I/O？

**回答：**

原则上不应该把阻塞 I/O 放在 actor 的默认执行路径里。Actor 的价值是快速处理 mailbox 中的消息，推进自己的状态机。如果一个 actor handler 里同步等数据库、HTTP、文件系统、对象存储或 LLM 推理，它占住的不只是一个函数调用，还占住了 actor 的处理回合和调度线程。

阻塞 I/O 会带来几个问题。

第一，mailbox 被拖住。同一个 actor 后面的消息都要等这个阻塞调用结束。一个慢数据库查询可能让后面很多轻量消息一起超时。

第二，dispatcher 或 worker pool 被拖住。Akka dispatchers 文档对这个问题说得很直接：默认 dispatcher 适合非阻塞 actor；如果默认 dispatcher 上的 actor 都阻塞，其他 actor 会因为拿不到线程而饥饿。

第三，故障传播更严重。下游服务慢，actor 也慢；actor 慢，mailbox 变长；mailbox 变长，调用方超时重试；重试再把下游打得更慢。这就是常见的级联过载。

第四，取消和超时更难。阻塞 I/O 如果不能响应 cancellation，actor runtime 想停也停不干净。

比较好的做法是：

```text
优先使用异步 I/O；
阻塞 I/O 放到专门 dispatcher 或独立 worker pool；
给每个外部调用设置 timeout；
用 bulkhead 隔离不同下游；
不要在 actor 内无限等待 future/result；
外部副作用要有幂等 key 或事务边界。
```

有些阻塞确实躲不开，比如老 JDBC driver、某些本地文件操作、Python 扩展库、GPU 推理调用。这时不是说完全不能做，而是要隔离。Akka 推荐用 dedicated dispatcher 管理阻塞；本质就是不要让阻塞任务占满 actor 默认调度资源。

LogServe 里的 actor method 是 Python 代码，运行在 worker 的 actor executor pool 里。worker 配置有 `ActorPoolSize`，本地也有 `actorQueue`，同一个 actor 还有 per-actor lock。这样至少不会无限启动 actor 执行。但如果某个 actor method 里做长时间阻塞 I/O，它会占住一个 actor runner；如果 `ActorPoolSize` 小，还会影响同 worker 上其他 actor。更重要的是，同一个 actor 的后续 command 会被 `command_seq` 和 per-actor lock 阻塞。

所以面试里可以这样说：

```text
actor 方法可以调用 I/O，但不应该在默认 actor 执行线程里做不可控阻塞。要么用异步 I/O，把结果作为后续消息回来；要么把阻塞调用隔离到专用 dispatcher/worker pool，并加 timeout、bulkhead 和 backpressure。LogServe 当前用 actorQueue 和 ActorPoolSize 隔离 actor 执行，但 Python actor method 如果长期阻塞，仍然会拖住该 actor 的 mailbox 和本地 actor runner。
```

一句话：actor handler 应该短、确定、可取消。长阻塞工作要离开 actor 的核心调度路径。

## Q016. actor 调用另一个 actor 时如何避免死锁或循环等待？

**回答：**

Actor 调另一个 actor 最容易出问题的地方，是把异步消息又写成了同步调用。A 处理消息时等 B，B 处理消息时又等 A，就会形成循环等待。

Orleans request scheduling 文档给了一个很典型的例子：grain activation 默认是单线程、非 reentrant 的，一个请求没处理完，下一个请求不会开始。如果 A 正在处理 `CallOther(B)` 并等待 B，而 B 同时处理 `CallOther(A)` 并等待 A，那么两个 grain 都忙着等对方，后续 `Ping` 请求进不了处理回合，最后只能等超时。

避免这类问题，可以从设计上做几件事。

第一，避免同步等待。Actor A 给 B 发消息后，不要阻塞当前 actor turn。把后续逻辑拆成 continuation：B 回复后，A 再收到一个消息继续处理。

```text
A receives Start
A sends Request to B with reply_to=A
A returns to mailbox
...
A receives ReplyFromB
A updates state
```

第二，调用图尽量有方向。比如订单 actor 可以调用库存 actor，库存 actor 不反过来同步调用订单 actor。能用 DAG 就不要做环。

第三，用 orchestrator actor 或 workflow 管协调。跨多个 actor 的流程，不要让两个业务 actor 互相等。让一个 coordinator 发命令、收结果、处理超时和补偿。

第四，所有 ask 都要有 timeout。没有 timeout 的等待就是潜在死锁。超时后要能释放等待状态，迟到回复要能识别并丢弃。

第五，避免持有内部锁或“半更新状态”等待外部 actor。actor 在等待期间如果状态已经改了一半，reentrancy 或重试会很难推理。

第六，如果 runtime 支持 reentrancy，要小心打开。Reentrancy 可以避免某些循环等待，但它把状态推理变复杂。只能让 read-only、幂等、不会破坏中间状态的调用 interleave。

LogServe 当前更偏保守。Actor command 按 `command_seq` 串行，worker 侧同一个 actor 有 per-actor lock。这个模型容易推理，但不适合在 actor method 里同步等待另一个 actor 再等待回来。如果未来要支持 actor-to-actor 调用，最好不要让 Python actor method 在持有本 actor 执行权时同步调用另一个 actor。更稳的是把跨 actor 流程放到 workflow 层，用 step、timeout、retry 和补偿表达。

面试里可以这样答：

```text
避免 actor 间死锁，核心是不要让 actor 在处理一条消息时同步等另一个 actor，同时还阻塞自己的 mailbox。用 reply_to/continuation、timeout、无环调用图、coordinator/workflow 和幂等补偿来组织跨 actor 协作。Reentrancy 可以缓解一部分循环等待，但它不是免费午餐，会让 actor 中间状态暴露给其他请求。
```

一句话：actor 间调用要像协议，不要写成互相拿锁的函数调用。

## Q017. actor reentrancy 是什么问题？

**回答：**

Actor reentrancy 指的是：一个 actor 在前一个请求还没有完全结束时，允许另一个请求进入并执行一部分逻辑。它通常发生在 async/await 场景里。

默认非 reentrant actor 的模型很简单：

```text
handle message A from start to finish
handle message B from start to finish
```

Reentrant actor 则可能变成：

```text
A starts
A awaits external call
B starts
B modifies state
A resumes
A continues with assumptions from before await
```

注意，这不一定是多线程并行。Orleans 文档说得很清楚：reentrant grain 仍然是 single-threaded，一次只执行一个 turn；问题在于不同请求的 continuation turn 可能交错。也就是说，代码没有并行跑，但状态观察点被插入了其他请求。

Reentrancy 解决的是活性和吞吐问题。非 reentrant actor 在等待异步 I/O 时，整个 activation 不能处理其他请求，可能造成低吞吐；A 等 B、B 等 A 时，还可能死锁。Reentrancy 允许 actor 在 await 期间处理别的请求，可以减少循环等待，也能提高 I/O 型 actor 的利用率。

但代价是 correctness 难很多。比如：

```text
func Transfer() {
    if balance >= 100 {
        await fraudCheck()
        balance -= 100
    }
}
```

如果 actor 在 `await fraudCheck()` 时允许另一个请求进来改 `balance`，恢复后继续扣款就可能基于过期判断。没有并行线程，也照样能出现逻辑竞态。

所以 reentrancy 的使用原则通常是：

```text
默认关闭；
只给 read-only 方法开放；
只给不会观察/修改中间状态的请求开放；
把 await 前后的状态假设重新校验；
对每个 interleavable 方法写清楚不变量；
用测试覆盖交错执行。
```

Orleans 提供了几种粒度：整个 grain 标记 `[Reentrant]`，单个方法 `[AlwaysInterleave]`，read-only 方法并发，以及用 `MayInterleave` predicate 按请求决定。这个设计说明 reentrancy 不是一个简单开关，而是一组调度语义。

LogServe 当前没有开放 reentrant actor 语义。它的 actor command 通过 `command_seq` 串行应用，worker 侧同 actor 也被 per-actor lock 保护。这个模型吞吐可能不如 reentrant actor，但好处是每个 actor 的状态线很清楚，replay 也简单。如果未来要加 reentrancy，我会先只允许只读方法或显式声明的 interleavable method，并要求它们不更新 `StateJSON`，或者用状态版本号在 resume 时重新校验。

面试里可以这样答：

```text
reentrancy 是 actor 在 await 或未完成请求之间允许其他请求插入执行。它能缓解死锁和提高 I/O 型 actor 吞吐，但会让“一个请求从头到尾独占 actor 状态”的假设失效。非 reentrant actor 容易推理，reentrant actor 要靠 read-only 标注、interleave predicate、版本校验和交错测试保证正确性。
```

## Q018. Akka、Orleans、Erlang actor 的模型差异是什么？

**回答：**

这三个系统都在 actor 附近，但它们的侧重点不一样。面试里不要只说“都是 mailbox + message”。那样太粗。

Akka 是 JVM 上的 actor toolkit。它强调 actor hierarchy、supervision、dispatcher、mailbox、typed behavior，也有 Akka Persistence 做 event-sourced actor。Akka actor 通常是显式创建的，有 parent-child 监督关系。失败时 supervisor 决定 restart、stop 等策略。Akka 的持久化不是每个 actor 自动都有，而是用 EventSourcedBehavior 或 Durable State Behavior 这类 API 显式建模。它给你的控制很多，代价是你要理解 dispatcher、mailbox、stash、persistence plugin、snapshot store。

Orleans 的关键词是 virtual actor，也叫 grain。它把 actor 做成逻辑上一直存在、始终可寻址的实体。调用方不太关心 grain 当前在哪个 server、是否已经加载到内存。runtime 会按需 activation、deactivation、placement 和恢复。Orleans grain 默认也是单线程执行模型，但它提供 reentrancy/interleaving 控制，也有 persistent state 和 opt-in distributed transactions。它更像面向云服务的分布式对象/actor runtime，隐藏了很多 placement 和生命周期细节。

Erlang 的 actor 更接近语言和 VM 原语。Erlang process 很轻量，有 pid、mailbox、`receive` pattern matching、link/monitor/supervision。OTP 在这个基础上提供 supervisor、gen_server 等行为。Erlang 的哲学偏“let it crash”：进程失败后由监督树处理。它默认不是 event-sourced actor，也不会自动给每个进程做 snapshot；持久化要靠应用协议、ETS/DETS、Mnesia、数据库或日志系统。

可以从几个维度比较：

```text
身份：
  Akka: ActorRef，通常显式创建，有层级。
  Orleans: grain id，virtual actor，逻辑上 always addressable。
  Erlang: pid 或 registered name，VM 级 process。

调度：
  Akka: dispatcher + mailbox，很多 actor 共享线程池。
  Orleans: grain activation 默认单线程处理请求，可配置 interleaving。
  Erlang: BEAM 调度轻量进程，receive 从 mailbox 取匹配消息。

失败处理：
  Akka: supervision hierarchy。
  Orleans: runtime 管 activation 和 placement，调用失败通过 Task/exception 表达。
  Erlang: link/monitor + OTP supervision。

持久化：
  Akka: Akka Persistence 支持 event sourcing 和 snapshot。
  Orleans: grain persistent state 和 opt-in transactions。
  Erlang: 语言 actor 本身不等于持久化，通常由 OTP 应用和存储系统组合。

reentrancy:
  Akka: actor 一次处理一条 message，async pipe/ask 需要自己设计。
  Orleans: 文档化支持 Reentrant、AlwaysInterleave、ReadOnly、MayInterleave。
  Erlang: process 自己写 receive loop，是否等待、选择性接收、协议如何设计由代码决定。
```

LogServe 更像把几种思想拆开后自己实现了一部分：像 Akka Persistence 一样有 actor event log 和 snapshot；像 Orleans 一样有稳定 actor id、owner worker 和接管；像 Erlang 一样强调消息顺序和 mailbox，但它不是 BEAM 进程模型。它当前验证范围是单机多进程，不应该把它说成完整的 Akka/Orleans/Erlang 替代品。

面试里可以这样答：

```text
Akka 更像可组合的 actor toolkit，supervision、dispatcher、persistence 都给你显式控制；Orleans 更像 virtual actor 平台，grain 逻辑上一直存在，runtime 管 activation、placement 和部分恢复；Erlang 把轻量进程和 mailbox 做进语言/VM，再用 OTP 组织 supervision。LogServe 的 actor runtime 借鉴的是持久化 actor 的核心机制：actor id、mailbox 顺序、snapshot replay 和 epoch fencing。
```

## Q019. actor 和 CSP/channel 模型有什么区别？

**回答：**

Actor 和 CSP/channel 都是消息传递模型，都试图少用共享内存。但它们抽象的中心不同。

Actor 的中心是“有身份的实体”。你给某个 actor 发消息：

```text
send actorA message
send actorB message
```

消息的目标是 actor 地址。Actor 有自己的 mailbox 和状态，收到消息后自己处理。状态通常和 actor identity 绑定在一起。一个 actor 可以创建其他 actor，也可以把自己的地址发给别人。

CSP/channel 的中心是“通信通道”。进程或 goroutine 通过 channel 发送和接收：

```go
jobs <- job
result := <-results
```

通信目标不是某个拥有状态的 actor，而是一个 channel。多个 sender、多个 receiver 可以共享同一个 channel。channel 可以是无缓冲的，发送和接收必须同时 ready；也可以是有缓冲的，容量决定什么时候阻塞。Go spec 明确说 channel 是并发函数通信的机制，buffer capacity 决定发送/接收是否阻塞；同一个 channel 对单 sender 到 receiver 的值有 FIFO 顺序。

几个差异很关键。

第一，identity 不同。Actor 有地址，channel 是通道。你通常给 actor 发消息；CSP 里你往 channel 发值，谁收由 channel 拓扑决定。

第二，状态归属不同。Actor 把状态封装在 actor 内部。CSP 不要求 channel 另一端一定有一个长期状态实体；它可以只是 pipeline 的下一段。

第三，backpressure 默认不同。很多 actor send 是异步入 mailbox，mailbox 如果不 bounded，就不会自然阻塞 sender。CSP 里的 unbuffered channel 天然 backpressure；buffered channel 满了也会阻塞 sender。

第四，选择机制不同。CSP/channel 常见 `select`，一个 goroutine 可以等多个 channel。Actor 通常是一个 mailbox，选择逻辑在消息协议或 mailbox priority 里。

第五，拓扑表达不同。Actor 擅长建模“很多有状态实体”：用户、订单、房间、worker、session。CSP/channel 擅长建模“数据流和同步点”：pipeline、fan-in/fan-out、worker queue、stage handoff。

两者可以组合。一个 actor 内部可以用 channel 管 worker；一个 channel pipeline 的某一段也可以是带状态的 actor。不要把它们对立成只能选一个。

LogServe 里两种味道都有。worker 本地 `taskQueue`、`llmQueue`、`actorQueue` 是 Go channel/bounded queue，用来把任务交给 executor pool；actor runtime 则是 actor id、`command_seq`、owner worker、snapshot 和 `actor:<actor_id>` stream。前者解决本地并发交接，后者解决有状态实体的顺序和恢复。

面试里可以这样答：

```text
actor 模型以 actor identity 和 private state 为中心，消息发给某个 actor，由它的 mailbox 串行处理；CSP/channel 以 channel 为中心，通信双方通过通道同步或排队。Actor 更适合 per-entity state machine，CSP 更适合 pipeline 和同步交接。Go channel 天然提供阻塞式 backpressure，而 actor mailbox 是否反压要看 runtime 和 mailbox 是否 bounded。
```

## Q020. actor 和数据库事务有什么边界？

**回答：**

Actor 和数据库事务解决的问题不一样。Actor 解决的是某个有状态实体的串行化和封装；数据库事务解决的是持久化数据上的原子性、一致性、隔离性和持久性。

一个 actor 可以保证：

```text
同一个 actor 的消息按某个顺序处理；
actor 内部状态一次只被一个逻辑流修改；
状态机转换有清晰入口；
内存状态和消息协议更容易推理。
```

但这不自动等于数据库事务。Actor 内部改了内存状态，再写数据库、发消息、调用第三方 API，这些副作用不会因为“在 actor 里执行”就自动原子。actor crash 在中间，仍然会有半完成问题。

数据库事务能保证的是另一层东西：

```text
多行、多表或多 key 更新要么一起提交，要么一起回滚；
并发事务之间按隔离级别互相影响；
提交后数据持久；
失败时数据库负责恢复到一致状态。
```

所以边界要这样看。

如果不变量只在单个 actor 内部，比如“同一个会话的 step 顺序”“同一个 counter 的递增”“同一个 room 的成员列表”，actor 串行化通常足够。你可以把 actor state 持久化为事件日志或 snapshot。

如果不变量跨多个 actor，比如转账涉及两个账户、库存扣减涉及商品 actor 和订单 actor、支付涉及账务和订单，那么 actor 串行化不够。你需要数据库事务、分布式事务、saga、outbox、幂等补偿，或者把不变量重新划到同一个 actor/partition 里。

Orleans 文档里有一个有意思的边界：它支持 opt-in distributed ACID transactions against persistent grain state。这个能力不是 actor model 自动送的，而是 Orleans 额外提供的事务系统。也就是说，成熟 actor runtime 如果要跨 grain 事务，也要单独实现事务协议。

Akka Persistence 也是类似。event-sourced actor 可以把事件持久化并 replay，但这不等于外部数据库和消息系统都跟着一个 ACID transaction 提交。通常要用 outbox、projection、at-least-once delivery、幂等 consumer 来处理边界。

LogServe 当前提供的是 actor command application 的 exactly-once-ish。`ActorCommandApplied` 用幂等 log key、`command_seq`、owner/epoch fencing 来避免旧结果或乱序结果推进 actor state。它不提供跨 actor ACID 事务，也不保证 actor method 内部外部副作用 exactly-once。如果 actor method 调了外部系统，仍然需要 idempotency key、唯一约束或业务补偿。

面试里可以这样答：

```text
actor 是并发控制和状态封装边界，数据库事务是持久化一致性边界。单 actor 内的不变量可以靠 mailbox 顺序维护；跨 actor、跨表、跨外部系统的不变量不能靠 actor 自动保证，要用事务、saga、outbox 或幂等补偿。LogServe 的 actor 通过 shared log、command_seq 和 epoch fencing 保证状态提交顺序，但它不是跨 actor ACID transaction manager。
```

一句话：actor 让“谁来改状态”更清楚，事务让“哪些持久化修改一起生效”更清楚。这两个边界不能混。

## Q021. actor 是否适合维护强一致共享状态？

**回答：**

Actor 适合维护“单个实体内部”的强顺序状态，不适合直接维护“很多实体共同共享的一份全局强一致状态”。这两句话差别很大。

如果状态天然属于一个 key，比如一个用户会话、一个订单、一个游戏房间、一个 workflow instance、一个计数器 actor，那么 actor 很适合。所有修改都进入同一个 mailbox，actor 按顺序处理：

```text
command #1 -> state v1
command #2 -> state v2
command #3 -> state v3
```

只要同时只有一个 active owner，且状态提交有持久化日志或事务保护，这个 actor 内部可以提供很清楚的线性状态推进。Ray 文档里也说，同一个 actor 的方法按调用顺序串行执行，不同 actor 可以并行。这个语义对 per-entity state machine 很有用。

但“强一致共享状态”往往指多个节点、多个 actor、多个客户端都想读写同一份逻辑状态，并且希望任何读都看到最新已提交写。这就超出了普通 actor mailbox 的能力。Actor 不是共识协议。它不能单靠一个 mailbox 解决网络分区、双主、跨副本复制、读写 quorum、持久化落盘和 leader fencing。

一个可靠的 actor 强一致设计，至少要有这些条件：

```text
single active owner:
  同一 actor id 同一时刻只能有一个 owner 可以提交状态。

durable log:
  状态变化要先进入可恢复日志或事务存储。

fencing:
  旧 owner、旧 lease、旧 epoch 的写入必须被拒绝。

ordering:
  command_seq / log_seq 必须单调，不能乱序应用。

read semantics:
  读请求要说明读 owner 内存、读持久化视图，还是走 quorum / linearizable read。

failover:
  owner 迁移后，新 owner 必须从持久状态恢复，再开始接新写。
```

LogServe 当前适合说成“单 actor 状态提交顺序明确”，不要说成“分布式强一致共享状态系统”。它通过 `owner_worker_id + epoch`、`command_seq == actor.command_count + 1`、`ActorCommandApplied` 和 snapshot replay，保护的是同一个 actor state 的提交顺序。旧 worker 或旧 epoch 的 completion 会被拒绝。这个机制能防止 stale completion 覆盖新状态。

边界也要讲清楚：LogServe 当前验证范围是单机多进程，不是多机共识系统。它没有 Raft/Paxos 这类复制共识，没有跨 actor transaction，也没有 quorum read。metadata 是 materialized view，shared log 才是状态真相。把它说成“强一致共享内存”会过度承诺。

面试里可以这样答：

```text
actor 适合维护单实体的强顺序状态：所有 command 进同一个 mailbox，由一个 owner 按序提交。它不天然适合维护跨多个 actor 或多个节点共享的全局强一致状态。要做到强一致，还需要 durable log、single active owner、epoch fencing、复制共识或事务协议。LogServe 目前实现的是 per-actor exactly-once-ish state application，不是跨 actor 的强一致事务系统。
```

一句话：actor 能把一致性边界缩到一个实体，但不能凭空替代数据库事务或共识协议。

## Q022. actor sharding 解决什么问题？

**回答：**

Actor sharding 解决的是“actor 数量太多、单机放不下、调用方又不想关心 actor 物理位置”的问题。

没有 sharding 时，所有 actor 都在一个进程或一个节点里。actor 少的时候很简单；actor 多了以后，内存、CPU、mailbox、snapshot、恢复和调度都压在一台机器上。更麻烦的是，调用方必须知道某个 actor 在哪里。如果 actor 因为扩容、缩容或故障迁移到别的节点，调用方的路由也要更新。

Sharding 把 actor id 映射到 shard，再把 shard 分配给集群里的节点：

```text
actor_id -> shard_id -> node
```

调用方只拿逻辑 id，比如 `user-123`、`room-9`、`order-456`。它把消息交给 sharding runtime，runtime 负责把消息路由到当前持有这个 shard 的节点。Akka Cluster Sharding 文档里说得很直接：它适用于需要把 actor 分布到多个节点，并且希望用逻辑标识交互而不用关心物理位置的场景；每个 entity actor 只在一个地方运行，消息通过 ShardRegion 路由到最终目的地。

Actor sharding 主要解决几件事。

第一，容量扩展。很多有状态 actor 总内存超过单机容量时，可以把 shard 分到多台机器上。

第二，位置透明。调用方按 actor id 发消息，不关心 actor 当前在哪个 worker/node。

第三，故障接管。某个节点挂了，属于它的 shard 可以迁到别的节点，actor 从持久化状态恢复。

第四，负载均衡。节点加入或离开时，shard 可以 rebalance。Akka 默认的 `LeastShardAllocationStrategy` 会把新 shard 放到 shard 数更少的节点，也会在新节点加入时从已有节点迁一部分 shard。

第五，热点隔离的基础。虽然 sharding 不会自动解决单 actor 热点，但它给了你按 key 观察和拆分的基础。你可以知道哪个 shard 热，哪个 actor 热，再做拆分、限流或迁移。

但是 sharding 也有成本。

```text
shard 数太少：
  节点利用不均，有些节点没有 shard。

shard 数太多：
  coordinator 管理成本、首次路由成本、rebalance 成本变高。

hash 不稳定：
  同一个 actor 可能被错误启动在多个地方。

网络分区：
  两边都以为自己可以启动同一个 shard，会产生 split-brain。

热点 actor：
  一个 actor 自己很热，sharding 也只能把它放在一个节点上，不能自动并行处理它的内部状态。
```

LogServe 当前还没有完整的 actor sharding。它有 actor id、owner worker、epoch、控制面 dispatch gating，已经具备 sharding 的一部分语义材料：actor 的逻辑身份和 owner 可以分开。但当前 owner 选择只是从 active workers 里选 worker，调度仍然在全局 queue 里扫描；`docs/plan.md` 也提到 actor pending command 应该按 actorID 建队列，让 owner worker 只检查自己拥有的 actor。真正的多节点 sharding 还要补 shard coordinator、稳定 hash、placement/rebalance、membership、split-brain handling 和跨节点恢复。

面试里可以这样答：

```text
actor sharding 解决的是大量有状态 actor 的分布式放置和位置透明路由。调用方只知道 actor id，runtime 把 actor id 映射到 shard，再把 shard 放到某个节点。它能扩展容量、支持迁移和 rebalance，但不能自动解决单 actor 热点，也不能替代 split-brain 处理和持久化恢复。LogServe 目前有 actor owner/epoch 和 per-actor 顺序语义，但还不是完整的 cluster sharding。
```

## Q023. actor placement 和迁移需要考虑哪些因素？

**回答：**

Actor placement 决定 actor 放在哪个节点；迁移决定 actor 什么时候从一个节点搬到另一个节点。这不是简单地“找一台空机器”。Actor 是有状态实体，放错位置会影响延迟、恢复、资源隔离和一致性。

需要考虑的因素很多，但可以按几类说。

第一，资源。actor 需要多少 CPU、内存、GPU、文件句柄、连接池、Python runner、模型缓存。Orleans placement 文档里把 CPU、内存、available memory、activation count 都列为 placement 策略可以考虑的信号。LogServe 的 actor 是 Python runner 执行，worker 还有 `ActorPoolSize`，所以 actor placement 至少要考虑 worker 的 actor 执行容量。

第二，数据和缓存 locality。如果 actor 经常访问某个本地缓存、模型 checkpoint、对象存储代理、数据库分片，放在离数据近的节点可以减少延迟。LogServe 的 LLM serving 已经有 locality-aware scheduling；actor placement 如果将来要做，也可以借鉴这个思路。

第三，通信关系。频繁互相调用的 actor 如果放得太远，会增加网络延迟；但放在一起也可能造成热点。要看调用图，而不是只看单个 actor。

第四，负载均衡。新 actor 应该尽量放到负载低的节点；迁移也应该避免一次搬太多。Akka sharding 文档提到 rebalance 数量要限制，否则很多 event-sourced entity 同时启动会给系统增加额外负载。

第五，恢复成本。迁移 actor 前要考虑 snapshot 是否足够新、tail log 多长、state 多大、恢复需要多久。一个 actor state 500 MB，不应该轻易来回迁。

第六，单活语义。迁移期间不能让 old owner 和 new owner 同时提交状态。需要 owner lease、epoch、fencing token 或 coordinator 确认。否则迁移就是制造 split-brain。

第七，亲和和反亲和。有些 actor 要靠近某类资源；有些 actor 不能放一起，比如同一租户的主备、同一故障域的副本、同一热点组。

第八，冷启动和依赖。actor code、Python 环境、模型、配置、密钥、外部连接是否已经在目标节点准备好。迁移不是只复制内存状态，执行环境也要能跑。

迁移流程可以抽象成这样：

```text
1. stop assigning new commands to old owner
2. wait for in-flight command to finish or mark it expired
3. persist last state / snapshot / log position
4. grant ownership to new owner with higher epoch
5. new owner loads snapshot + tail log
6. new owner starts accepting command_seq = last_count + 1
7. old owner completion is rejected by epoch fencing
```

LogServe 当前 `ensureActorOwner` 会检查当前 owner 的心跳；如果 owner 超过 `actorOwnerLease`，控制面选择另一个 active worker，写 `ActorOwnershipGranted`，epoch 加一。`leasedTaskSpec` 会在 poll 时注入当前 owner、epoch 和最新 actor state。`completeActorCall` 会拒绝 worker id 或 epoch 不匹配的完成。这已经覆盖了 ownership 转移里最关键的一段：新 owner 接管后，旧 owner 不能再写状态。

但生产化还需要更多指标和策略：

```text
actor_recovery_ms
actor_state_bytes
actor_tail_events
actor_owner_handoff_total
actor_owner_handoff_failure_total
actor_placement_reason
actor_migration_inflight_commands
```

面试里可以这样答：

```text
actor placement 要考虑资源、数据 locality、通信关系、负载均衡、恢复成本、故障域和单活语义。迁移不是把 actor id 指到另一个节点就完了，必须停止旧 owner 分配、处理 in-flight、持久化状态、提升 epoch、让新 owner 从 snapshot/log 恢复，并用 fencing 拒绝旧 owner 的迟到提交。LogServe 当前已经有 owner_worker_id + epoch 的核心 fencing，但还没有完整的多节点 placement/rebalance 策略。
```

## Q024. actor ownership 转移如何避免 split-brain？

**回答：**

Split-brain 的本质是：两个地方都以为自己拥有同一个 actor，并且都能处理消息、提交状态。对 actor 来说，这比普通服务多实例更危险，因为 actor 的价值正是“同一份状态只有一个 owner 顺序修改”。两个 owner 同时写，就会产生两条历史。

避免 split-brain，要从判定、授权、提交三个点做防护。

第一，不能只靠本地超时做最终判断。网络分区和机器 crash 对观察者来说很像。Akka Split Brain Resolver 文档也强调，网络分区、机器崩溃、长 GC、CPU 饥饿都可能表现为 heartbeat 没回应；如果两边都按超时把对方 down 掉，就会形成两个独立 cluster。Akka 文档还明确警告：如果 Cluster Sharding 配合错误 downing 策略，两个分区里可能各自启动同一个 sharded entity。

第二，owner 授权要有中心化或共识边界。可以是 shard coordinator、Raft leader、数据库 lease、ZooKeeper/etcd lease、Akka lease、或者一个可线性化的 owner table。重点不是“谁觉得自己是 owner”，而是“谁拿到了可验证的 fencing token”。

第三，状态提交路径必须检查 fencing token。即使旧 owner 还在执行，它提交时也要带 epoch/lease version；存储层或控制面只接受当前 epoch。旧 epoch 的写入必须失败，而不是由调用方自觉停止。

一个典型规则是：

```text
actor_ownership(actor_id):
  owner = worker-2
  epoch = 17

completion must include:
  actor_id
  worker_id = worker-2
  epoch = 17
  command_seq = current_count + 1
```

只要其中任何一个不匹配，就不能写 `ActorCommandApplied`。

第四，迁移要处理 in-flight。旧 owner 可能已经拿到 command 并正在执行。新 owner 接管后，旧 owner 的结果可能晚到。没有 epoch fencing，晚到结果会覆盖新状态；有 fencing，它只能被拒绝。

第五，不要让读路径绕过 owner。如果某些读直接读旧 owner 内存，split-brain 时读到的是旧世界。强一致读要走当前 owner 或可线性化存储。

LogServe 的这部分是一个很好的例子。Actor ownership 是 `owner_worker_id + epoch`。`ensureActorOwner` 在 owner 心跳过期后选择新 worker，并写 `ActorOwnershipGranted`，epoch 增加。`completeActorCall` 检查：

```text
req.worker_id == actor.owner_worker_id
req.actor_epoch == actor.epoch
command_seq == actor.command_count + 1
```

不满足就拒绝。`internal/control/actor_test.go` 里有 stale completion 测试：actor 当前 owner 是 `new-worker`、epoch 是 2，旧 worker 用 epoch 1 完成会被拒绝，而且不会修改 `CommandCount` 或 `StateJSON`。

边界也要讲清楚。LogServe 当前是单控制面/单机验证语义。真正多控制面或多节点环境下，`ActorOwnershipGranted` 本身也要由强一致日志或共识系统保护。否则两个控制面可能各自发一个更高 epoch。也就是说，epoch fencing 要配合一个可信的 epoch 分配者。

面试里可以这样答：

```text
避免 actor ownership split-brain，不能只靠 heartbeat 超时。要有一个可靠的 owner 授权边界，给每次 ownership 颁发单调 epoch/lease；所有状态提交都必须带这个 epoch，并由控制面或存储层拒绝旧 epoch。LogServe 当前用 owner_worker_id + epoch + command_seq 做 fencing，能拒绝旧 worker 的迟到完成；如果扩展到多控制面，还需要把 epoch 分配放进共识日志或线性化存储里。
```

一句话：split-brain 不能靠“旧 owner 会乖乖停”解决，要靠“旧 owner 就算不停也写不进去”解决。

## Q025. actor 的消息去重应该放在哪里？

**回答：**

消息去重要放在“语义边界”上，而不是只放在 worker 或 mailbox 里。最稳的位置通常是 actor command 被接受、分配序号、写入日志之前或同一事务内。

原因很简单：重复消息可能来自很多地方。

```text
客户端超时后重试；
网关重发；
控制面 redelivery；
worker crash 后重跑；
reply 丢失导致调用方以为没成功；
actor owner 迁移时旧请求和新请求并存。
```

如果去重只放在 worker 本地内存，worker 一重启就没了。如果只放在 mailbox，消息已经进入持久化日志或任务表后再去重，会留下半任务。如果只放在 actor method 里，外部副作用可能已经发生，状态序号也可能被消耗。

比较稳的分层是：

```text
ingress / control plane:
  用 idempotency_key + request fingerprint 做接收去重。
  同一个 key、同一个 payload 返回同一个结果或同一个 task/command。
  同一个 key、不同 payload 直接报冲突。

actor command log:
  用 actor_id + actor_call_id / command_id 做 applied 去重。
  防止重复 completion 写两次 ActorCommandApplied。

actor state machine:
  对业务语义再做一层去重，比如 payment_id、order_event_id、message_id。

external side effect:
  调数据库、支付、消息队列时继续带 idempotency key 或唯一约束。
```

去重表要保存什么？至少保存：

```text
idempotency_key
request_fingerprint
actor_id
command_seq / command_id
status
result_ref 或 error
created_at / expires_at
```

这里有个关键点：去重不是只看 key。还要看 fingerprint。否则调用方复用同一个 idempotency key 发送不同 payload，系统如果静默返回旧结果，会造成很难查的错误。LogServe 的普通 task 和 actor create 都有 fingerprint 检查，重复 key 但 payload 不一致会被拒绝。

LogServe 里 actor 的去重有几处。`CreateActor` 支持 `IdempotencyKey` 和 fingerprint；`submitActorCommand` 如果请求没给 idempotency key，就用新的 call id；如果给了 key，会走 `GetTaskByIdempotencyKey` 和 fingerprint 校验。Actor 状态提交用 `actor_id + actor_call_id + applied` 作为 append log 的幂等 key，避免同一 actor call 被应用多次。README 里也明确说，LogServe 提供 exactly-once-ish actor command application，而不是外部副作用 exactly-once。

面试里可以这样答：

```text
actor 消息去重应该放在控制面接收和 actor command log 提交边界，而不是只放在 worker 本地。接收时用 idempotency_key + fingerprint 防止重复提交和冲突复用；应用时用 actor_id + command_id/applied key 防止重复完成写两次状态；业务外部副作用还要带自己的幂等 key。LogServe 当前就是用任务 idempotency 和 actor applied log key 做 exactly-once-ish 的状态提交去重。
```

## Q026. actor 的 exactly-once message processing 为什么困难？

**回答：**

因为“处理一条消息”不是一个动作，而是一串动作：

```text
收到消息
入队
分配 command_seq
执行 actor method
修改内存状态
写事件日志
写 snapshot
返回结果
调用外部系统
ack 调用方
```

任何两个步骤之间都可能 crash、超时、网络分区或重试。Exactly-once 要求系统在所有这些故障窗口里都表现得像只处理了一次，这非常难。

几个典型窗口：

第一，执行成功但 ack 丢了。actor 已经处理了消息，调用方没收到结果，于是重试。系统必须认出这是同一条消息，并返回旧结果或跳过重复执行。

第二，执行成功但提交失败。actor method 已经调用了外部 API，但 `ActorCommandApplied` 没写成功。重试时 method 会再跑一次，外部副作用可能重复。

第三，提交成功但回复失败。状态已经推进，调用方认为失败。重试必须返回已提交结果，不能再推进一次状态。

第四，worker crash 后不确定它执行到哪。控制面只看到租约过期，不知道 worker 是否已经调用了外部系统。

第五，owner 转移后旧 owner 迟到提交。没有 fencing，旧结果可能覆盖新 owner 的状态。

Ray 官方文档把 actor task 语义讲得很现实：默认是 at-most-once；也可以配置 at-least-once。at-least-once 下，重试方法可能执行两次，关键状态恢复要应用自己 checkpoint。这说明成熟系统也通常不轻易承诺 exactly-once execution。

更靠谱的说法是把 exactly-once 拆开：

```text
exactly-once delivery:
  几乎做不到，网络可能丢、重复、延迟。

exactly-once execution:
  很难证明，尤其有 crash 和外部副作用。

exactly-once state application:
  可以通过 idempotency、sequence、fencing、事务日志做到接近。

effectively-once business effect:
  通过幂等 key、唯一约束、事务 outbox、去重 consumer 实现。
```

LogServe 的边界正是 third one：exactly-once-ish actor command application。它不保证 actor method 只执行一次，但控制面会用 `command_seq`、actor applied 幂等 key、owner/epoch fencing 控制状态提交。旧 worker、旧 epoch、乱序 command 都不能推进 `StateJSON`。这比“我保证消息只处理一次”更准确。

面试里可以这样答：

```text
actor exactly-once message processing 困难，是因为执行、提交、外部副作用和回复不是一个原子动作。系统可以做到 at-most-once 或 at-least-once，也可以通过幂等 key、command_seq、durable log 和 epoch fencing 做 exactly-once-ish state application。但只要 actor method 里有外部副作用，就必须让业务侧也幂等。LogServe 承诺的是状态提交去重，不是严格 exactly-once execution。
```

一句话：不要把“状态只应用一次”说成“消息只执行一次”。这两个差很远。

## Q027. actor 状态膨胀时如何做拆分？

**回答：**

Actor 状态膨胀通常有两种：state 太大，或者单 actor 太热。处理方式也不一样。

State 太大时，表现是 snapshot 变慢、replay 变慢、每次 command 都携带大 `StateJSON`、GC 压力上升、网络传输变大。单 actor 太热时，表现是 mailbox 长、p99 高、command_seq 后面的请求排队、单 owner CPU 打满。

拆分前先问一个问题：这个 actor 里面有哪些不变量必须一起维护？不能为了拆而拆，把必须原子更新的状态拆到不同 actor 后，又用复杂事务补回来。

常见拆法有几种。

第一，按子实体拆。比如 `RoomActor` 里有几万个用户，可以拆成：

```text
RoomActor:
  管 room lifecycle、全局配置、成员索引。

RoomMemberActor(room_id, user_id):
  管单个成员状态、游标、连接信息。
```

第二，按功能拆。一个大 actor 同时处理配置、统计、消息、权限，可以把统计和日志这类弱一致状态拆出去。核心 actor 只保留强顺序状态。

第三，冷热拆分。热状态留在 actor 内存，冷历史放对象存储、数据库或事件日志。actor 里只保存最近窗口、索引和引用。

第四，按 shard 拆。一个账号下有很多子资源，可以按 resource id hash 到多个 child actor。父 actor 负责路由和聚合。

第五，CQRS 拆分。写 actor 只处理 command 和事件，读模型由 projection 异步构建。不要让写 actor 背负复杂查询状态。

第六，delta 或增量 snapshot。状态很大但每次只改一点时，不一定每次都复制完整状态。可以保存 base snapshot + delta log，或者把大字段外置，只在 snapshot 中保存 ref。

拆分会引入新问题：

```text
跨 actor 不变量变难；
跨 actor 查询需要聚合；
消息顺序从一个序列变成多个序列；
恢复要处理父子 actor 的版本关系；
删除和 passivation 要级联；
热点可能只是从父 actor 转移到某个子 actor。
```

LogServe 当前 actor state 是完整 `StateJSON`，worker 执行 actor method 后把新的完整 state 返回控制面，snapshot 也保存完整 state。`docs/plan.md` 已经指出，大 result/snapshot 会带来内存 profile 压力，也提到 actor state 可以考虑 delta snapshot 或 command batch apply。如果 actor state 变大，LogServe 生产化方向应该是：大字段外置到 result store，state 里保存 ref；常变字段和冷字段分离；对高频 command 做批量或 delta；按 actor 子实体拆分。

面试里可以这样答：

```text
actor 状态膨胀不能只靠加大 snapshot 间隔。先识别哪些不变量必须在一个 actor 内维护，再按子实体、功能、冷热、读写模型或 shard 拆分。拆分后要重新设计跨 actor 一致性、聚合查询和恢复顺序。LogServe 当前每次 actor command 返回完整 StateJSON，适合机制验证；如果状态变大，应把大字段外置、引入 delta snapshot，必要时拆成父子 actor 或 sharded child actor。
```

## Q028. actor 如何处理长时间未访问的 passivation？

**回答：**

Passivation 是把长时间不用的 actor 从内存里移走，保留它的逻辑身份和持久化状态。下一次有人访问时，再重新激活并恢复状态。

这解决的是内存和调度资源问题。Virtual actor 或 sharded actor 系统里，逻辑 actor 可能有几百万个，但活跃的只是一小部分。如果每个 actor 都常驻内存，系统迟早被冷 actor 占满。

Orleans activation collection 文档把这个说得很清楚：grain activation 是 runtime 按需创建的内存实例；activation collection 会扫描长时间 idle 的 activation，调用 `OnDeactivateAsync()`，然后从 silo 数据结构里移除，让 .NET GC 回收。Akka Cluster Sharding 也有 passivation：entity 可以因为 idle 或 active entity limit 被 passivate；在 passivation 和 entity 终止之间，新消息会由 shard buffer，之后投递给新的 entity incarnation。

一个靠谱的 passivation 流程应该是：

```text
1. 判断 actor idle：
   一段时间没有收到外部消息，或者 active entity limit 超限。

2. 停止接新执行：
   标记 passivating，不再启动新的长任务。

3. drain 或处理 in-flight：
   当前 command 完成；不能无限等，要有 deadline。

4. 持久化状态：
   写 event、snapshot、last_command_seq、timer/reminder 元数据。

5. 释放内存资源：
   关闭连接、释放本地缓存引用、停止 runner 中的实例。

6. 保留路由：
   actor id 仍然可寻址；下一条消息触发重新激活。

7. 重新激活：
   从 snapshot + tail log 恢复，再继续 command_seq。
```

Passivation 最容易出错的地方，是消息丢失。Akka 文档特意提到，如果 entity 自己直接 stop，mailbox 里已经排队的消息会被丢；使用 `ClusterSharding.Passivate` 时，shard 会在 passivation 到 termination 之间 buffer incoming messages，并在新 incarnation 启动后投递。这个细节很重要：passivation 不是简单 `stop(actor)`。

还有几个边界：

```text
actor 有未完成外部 I/O：
  要么等到 deadline，要么取消并让 command 重试。

actor 有 timer/reminder：
  timer 是否算活跃？Orleans 文档里 timer event 不算 activation collection 的活跃信号。

actor 有本地资源：
  passivate 时释放；重新激活时重建。

actor snapshot 很旧：
  passivate 前最好做一次 snapshot，降低下次激活成本。

actor 很快又被访问：
  idle timeout 太短会造成 thrashing，反复激活/释放。
```

LogServe 当前没有 passivation runtime。Actor state 不以常驻 Go 对象形式长期运行，更多是每次 actor task 把 `ActorStateJson` 注入 Python runner，执行后返回新 state；控制面 metadata 保留当前视图，shared log 和 snapshot 支持恢复。如果未来支持常驻 actor instance 或多节点 sharding，passivation 就会变得重要：要记录 last access time、in-flight、last command count、snapshot freshness，并确保 passivation 期间新 command 不丢。

面试里可以这样答：

```text
passivation 是释放冷 actor 的内存实例，不是删除 actor。正确流程是 idle 检测、停止新执行、drain in-flight、持久化 state/snapshot、释放本地资源，并保留 actor id 的路由；下一次消息到来时重新激活并从 snapshot/log 恢复。Akka 和 Orleans 都把 passivation/activation collection 做成 runtime 机制。LogServe 当前还没有常驻 actor passivation，未来如果做 sharding，需要特别处理 passivation 期间消息 buffer 和 snapshot freshness。
```

## Q029. actor snapshot 中是否应该包含外部资源句柄？

**回答：**

不应该。Actor snapshot 应该包含可序列化、可迁移、可长期保存的逻辑状态，不应该包含外部资源句柄，比如：

```text
文件描述符
socket / TCP connection
数据库连接
HTTP client live connection
线程对象
goroutine / coroutine handle
GPU context
Python object pointer
mutex / condition variable
timer runtime handle
本地临时文件句柄
```

这些东西有几个共同问题。

第一，它们只在当前进程有效。actor 恢复到另一台机器时，句柄没有意义。

第二，它们不能可靠序列化。即使能把某个 id 写进去，也不能代表底层资源还活着。

第三，它们会把 snapshot 和运行时环境绑死。snapshot 本来应该支持迁移、重启、回滚和长期保存，句柄会破坏这些能力。

第四，它们可能泄漏资源或权限。把连接、token、临时路径、进程 id 写进 snapshot，可能造成安全和清理问题。

正确做法是保存“重建资源所需的描述符”，而不是保存资源本身：

```text
不要保存 db connection；
保存 database name、tenant id、logical config version。

不要保存 socket；
保存 peer id、session id、last ack offset。

不要保存 file handle；
保存 object ref、path、etag、offset、checksum。

不要保存 GPU context；
保存 model name、version、checkpoint ref、device preference。

不要保存 timer handle；
保存 timer deadline、timer id、payload。
```

恢复时用这些描述符重新打开资源。重新打开失败要作为 activation failure 或 degraded state 处理，而不是假装 snapshot 里有可用句柄。

Akka Cluster Sharding 文档里有一个相近的提示：behavior factory 在每个节点本地执行，可以注入 node-local `ActorRef` 或无法序列化的对象；这类东西适合在激活时注入，不适合放进持久化 state/event。Ray actor fault tolerance 文档也提醒，如果 checkpoint 保存到外部存储，要保证整个 cluster 可访问；这说明 checkpoint/snapshot 应该是跨节点可读的状态，不是本地进程资源。

LogServe 当前 snapshot 保存的是 `StateJSON`，也就是 JSON bytes。这个设计天然要求 actor state 可序列化。Python actor 如果把 socket、file handle、模型对象直接塞进 state，就不适合放入 `StateJSON`。更好的方式是 state 里保存 logical ref，实际资源在 worker 激活 actor 或执行 method 时重建。

面试里可以这样答：

```text
actor snapshot 不应该包含外部资源句柄。Snapshot 是为了跨重启、跨迁移恢复逻辑状态，必须可序列化、可校验、可长期保存。外部资源要保存 descriptor/ref，比如 object key、connection config、offset、model version，恢复时重新打开。LogServe 的 StateJSON/snapshot_ref 也要求 actor state 是可 JSON 化的逻辑状态，而不是 Python 运行时对象或本地句柄。
```

## Q030. actor 单元测试和恢复测试分别应该覆盖什么？

**回答：**

Actor 单元测试和恢复测试关注点不同。单元测试主要看“状态机逻辑是否正确”；恢复测试主要看“崩溃、重启、迁移后状态是否还能正确继续”。

单元测试应该覆盖这些内容：

```text
command -> state transition:
  给定初始 state 和 command，得到预期新 state / event / reply。

invalid command:
  状态不允许时拒绝，比如已关闭订单不能再支付。

message order:
  同一 actor 多条消息按顺序处理，后一个能看到前一个的状态。

idempotency:
  重复 command_id / idempotency_key 不重复推进状态。

serialization:
  state/event/command 可以序列化和反序列化，版本兼容。

timeout/cancel:
  长命令取消后不会留下半更新状态。

mailbox/backpressure:
  mailbox 满、deadline 过期、低优先级消息被拒绝或降级。

side-effect boundary:
  event handler/replay path 不做外部副作用。
```

这些测试最好尽量不依赖真实集群。可以直接测 reducer、command handler、状态转移函数。LogServe 的 `internal/actor/model.go` 这种 replay reducer 就很适合单元测：给一串 `ActorCreated/ActorCommandApplied/ActorSnapshotCreated`，看最终 `State` 是否正确。

恢复测试要更重一些。它应该覆盖故障窗口：

```text
full replay:
  没有 snapshot 时，从 ActorCreated + 全部 applied events 恢复。

snapshot replay:
  有 snapshot 时，加载 snapshot_ref，只 replay tail events。

trimmed replay:
  logical trim 后，从 ActorSnapshotCreated + snapshot object + tail log 恢复。

worker crash:
  old owner crash 后，新 owner 接管，状态不丢。

stale completion:
  旧 worker / 旧 epoch 完成被拒绝。

out-of-order completion:
  command_seq != command_count + 1 被拒绝。

duplicate completion:
  同一个 command applied 不写两次。

in-flight redelivery:
  执行中 crash 后重试，状态最多应用一次。

snapshot failure:
  snapshot 写失败、读失败、schema 不兼容时走预期路径。

passivation/reactivation:
  冷 actor 被释放后，再访问能恢复并继续处理下一条 command。

split-brain simulation:
  两个 owner 竞争时，只有持有当前 epoch 的 owner 能提交。
```

LogServe 现在已经有几类关键测试。`tests/integration/actor_runtime_test.go` 覆盖 `Counter` actor 100 次 `inc()` 后 worker 切换、第二个 worker 接管后 `get()` 返回 `100`、snapshot replay 比 full replay 少、1000 并发 `inc()` 最终返回 `1000`。`internal/control/actor_test.go` 覆盖 stale completion rejection、out-of-order command rejection、future command 不提前 dispatch、poll 时注入最新 actor state、recovered owner 接收队列中的 actor command。

还可以补的测试包括：

```text
snapshot object 丢失后的恢复行为；
snapshot schema version 不兼容；
actor state 很大时的 snapshot/replay memory profile；
passivation 期间新消息 buffer；
两个 control plane 竞争颁发 ownership 的模拟；
业务 idempotency key 重复但 payload 冲突；
actor method 外部副作用失败后的补偿路径。
```

面试里可以这样答：

```text
actor 单元测试测状态机：command 到 state/event/reply 的转换、非法状态、幂等、序列化和顺序。恢复测试测故障窗口：full replay、snapshot replay、trimmed replay、worker crash、owner transfer、stale completion、out-of-order command、duplicate completion 和 passivation/reactivation。LogServe 现有测试已经覆盖 mailbox 串行化、snapshot replay、epoch fencing 和 recovered owner dispatch，后续生产化可以补 snapshot 损坏、split-brain 和 passivation 场景。
```

## Q031. actor mailbox 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

Actor mailbox 的核心目标，是把同一个 actor 的并发输入收束成一条可定义顺序的消息流。外部可以有很多 sender 并发发消息，但 actor 处理自己状态时，看到的是一条队列，而不是一堆线程同时进来改字段。

所以 mailbox 首先解决的是正确性和安全性问题，不是单纯性能优化。

它解决的正确性问题包括：

```text
single writer:
  同一个 actor 的内部状态一次只被一个消息处理逻辑推进。

ordering:
  同一个 actor 内部可以定义消息处理顺序，常见是 FIFO 或应用层 command_seq。

atomic state transition:
  一条消息处理完成后，状态从 S(n) 变成 S(n+1)，中间状态不暴露给其他消息。

protocol boundary:
  外部不能直接修改 actor state，只能通过消息协议表达意图。
```

安全性问题也很明显。没有 mailbox，调用方可能直接共享 actor 内部状态；有 mailbox 后，状态访问边界变清楚了。你可以在 actor method 里维护不变量，比如余额不能为负、订单状态不能从 `CANCELLED` 回到 `PAID`。只要所有修改都经过同一个 mailbox，这些不变量就有一个统一检查点。

性能是第二层目标。Mailbox 可以减少业务代码里的锁，让 actor 内部逻辑保持普通顺序代码。很多 actor 可以分布在不同线程或节点上并行执行。但是单个 actor 本身不会因为 mailbox 变快。热点 actor 仍然是串行瓶颈。如果所有请求都打到一个 actor，mailbox 只是把并发竞争变成排队。

可维护性是第三层收益。Mailbox 把并发边界显式化以后，代码更容易解释：

```text
所有外部请求 -> actor mailbox -> actor behavior -> state transition
```

这比在很多地方加锁、解锁、条件变量、CAS 更容易审计。但这个收益有前提：消息协议要清楚，队列容量要有上限，超时、重试、去重、失败恢复都要设计。否则 mailbox 会变成隐藏复杂度的地方。

放到 LogServe，mailbox 的核心目标不是“有一个 Go channel”。它体现在控制面的 actor command 顺序上：

```text
CallActor
  -> ActorCommandSubmitted
  -> command_seq 单调递增
  -> PollTask 只放行 command_seq == command_count + 1
  -> CompleteTask 只接受当前 owner/epoch + 下一条 command_seq
  -> ActorCommandApplied
```

这说明 LogServe 的 mailbox 目标是 per-actor state application 的顺序正确性。README 里也写得很清楚：控制面用 per-actor 短锁分配 `command_seq`，再用 dispatch-time gating 确保 worker 只能拿到下一条可执行 actor task。`internal/control/actor_test.go` 里还专门测了第二个 actor call 不会因为第一个 call 等结果而阻塞入 mailbox，说明提交路径和执行等待被分开了。

面试里可以这样答：

```text
actor mailbox 的核心目标是把同一个 actor 的并发调用变成一条可验证的顺序输入流。它主要解决正确性和安全性：单所有者修改状态、消息顺序明确、状态转移可恢复。性能收益是副作用，因为 actor 内部少写锁，多个 actor 可以并行；但单个热点 actor 仍然串行。LogServe 里的 mailbox 不是普通内存队列，而是 command_seq、owner/epoch fencing 和 ActorCommandApplied 共同定义的可恢复顺序边界。
```

## Q032. actor mailbox 的典型适用场景和不适用场景分别是什么？

**回答：**

Actor mailbox 适合“同一个 key 上的操作必须按顺序推进”的场景。这个 key 可以是用户、订单、房间、workflow、会话、设备、文档、模型实例，或者任何有长期状态的实体。

典型适用场景有几类。

第一类是 per-entity state machine。比如订单状态：

```text
Created -> Paid -> Shipped -> Delivered
Created -> Cancelled
```

这些状态转移不能乱序。`Ship` 不能跑在 `Pay` 前面，`Cancel` 不能在已经 `Delivered` 后随便生效。把一个订单做成 actor 后，所有 command 进入这个订单 actor 的 mailbox，状态转移逻辑集中在一个地方。

第二类是 session 或连接状态。比如在线用户会话、WebSocket 连接、游戏玩家状态、IoT 设备连接。它们通常有内存上下文、心跳、订阅关系、临时缓存。Mailbox 可以保证同一个会话的登录、刷新 token、断线重连、关闭等动作按顺序处理。

第三类是 workflow instance。一个 workflow run 可能有很多 step、timer、callback、retry。把 workflow instance 做成 actor，可以让所有事件进入同一个 mailbox，然后由状态机判断下一步该调度什么。

第四类是有状态资源代理。比如一个数据库连接代理、模型实例代理、浏览器会话代理、外部 API rate-limit key。Mailbox 能把并发访问排队，避免资源被多个线程同时乱用。

不适合的场景也要说清楚。

第一，纯无状态高吞吐任务不一定适合。比如图片缩放、独立 RPC 转发、日志解析、批量 CPU 计算，这些任务之间没有同一个实体状态。用 worker pool 或数据并行更直接。硬套 actor mailbox，可能只是多了一层排队。

第二，单个超热点 key 不适合只靠一个 actor。比如全站计数器、全局排行榜、单个热门直播间弹幕流。如果所有请求都进同一个 mailbox，吞吐上限就是一个 actor 的处理速度。需要 shard、分桶、CRDT、批处理、近似统计，或者把读写拆开。

第三，强跨实体事务不适合只靠 mailbox。两个账户转账、多个库存同时扣减、跨订单一致性，这些不变量跨多个 actor。单 actor mailbox 只能保护自己的状态，跨 actor 仍然需要事务、saga、outbox 或补偿。

第四，长时间阻塞 I/O 不适合直接放在 actor 处理路径里。Actor method 如果一直等待外部 HTTP、数据库或 GPU 任务，后面的消息会被挡住。更好的做法是把慢 I/O 丢给专门 executor，actor 收到完成事件后再推进状态。

第五，大 payload 堆积不适合直接塞进 mailbox。消息里带大文件、大 JSON、大向量，mailbox 会变成内存仓库。应传引用、对象存储地址或批次 id。

LogServe 的适用边界比较清楚。`Counter` actor、workflow-like 有状态任务、Python actor 状态恢复，都适合 mailbox。它当前已经验证了 1000 个并发 `inc()` 能串行化到同一个 actor。但如果要跑大量无状态函数，LogServe 还有普通 task queue 和 worker pool；如果要跑真实 LLM/GPU serving，README 也把那部分和 actor mailbox 分开，不能把 mailbox 说成 GPU 调度方案。

面试里可以这样答：

```text
actor mailbox 适合 per-key 有状态对象：订单、会话、workflow instance、游戏房间、设备状态、长生命周期资源代理。它不适合纯无状态批处理、单个全局热点、跨多个 actor 的强事务、大 payload 堆积，或者在 actor 内直接跑长时间阻塞 I/O。判断标准很简单：这个请求是否必须围绕某个实体状态按顺序推进。如果不是，worker pool、channel pipeline、数据库事务或分布式队列可能更合适。
```

## Q033. actor mailbox 和相近概念最容易混淆的边界在哪里？

**回答：**

Actor mailbox 最容易和六个概念混在一起：消息队列、worker pool 队列、锁、channel、event log、backpressure。

先看 mailbox 和消息队列。Kafka、RabbitMQ、SQS 这类消息队列通常是系统间传递消息的基础设施，关心 durable delivery、consumer group、offset、ack、重投、吞吐和跨服务解耦。Actor mailbox 是某个 actor 的输入缓冲区，语义重点是“这个 actor 的状态由哪些消息按什么顺序推进”。它可以是内存的，也可以持久化；可以由 MQ 承载，也可以不是 MQ。不能因为底层用了 Kafka，就说 Kafka partition 等于 actor mailbox。Kafka partition 提供分区内顺序，但 actor 还要处理状态、幂等、owner、恢复和业务协议。

再看 mailbox 和 worker pool 队列。Worker pool queue 是把一批独立任务分给多个 worker，目标是并行执行。Actor mailbox 是把同一个 actor 的消息串行化，目标是保护这个 actor 的状态。Worker pool 追求“多 worker 抢任务”；actor mailbox 反而要避免同一 actor 被多个 worker 同时执行。

第三，mailbox 和锁。锁保护临界区，调用方拿锁后进入共享对象。Mailbox 改变了访问方式：调用方不进入 actor 内部，只提交消息。锁是同步机制，mailbox 是并发模型边界。它们也可以共存，比如 LogServe 在提交 actor command 时用 per-actor 短锁分配 `command_seq`，但 actor 状态推进仍然靠 mailbox 顺序，而不是靠调用方持锁执行 actor method。

第四，mailbox 和 Go channel / CSP。Channel 是通信原语，发送方和接收方围绕 channel 同步或排队；actor mailbox 绑定 actor identity 和 private state。一个 actor 可以内部用 channel 实现 mailbox，但 channel 本身不知道 actor 的 owner、epoch、snapshot、恢复状态。

第五，mailbox 和 event log。Mailbox 关心待处理消息；event log 关心已发生事实。持久化 actor 里，两者会靠得很近：command submitted 可能写入日志，command applied 也写入日志。但 submitted 不是 applied。LogServe 的 `ActorCommandSubmitted` 表示命令进入顺序边界，`ActorCommandApplied` 才表示状态已经推进。这个区分很重要。

第六，mailbox 和 backpressure。Mailbox 是排队结构；backpressure 是系统在压力下限制输入、放慢上游或拒绝请求的策略。一个 unbounded mailbox 没有真正的 backpressure，只是把压力堆到内存里。Akka typed 文档也明确提醒默认 mailbox 是无界的，如果 producer 快于 consumer，最终可能 OOM；bounded mailbox 才会把压力反馈给发送方或调度策略。

还有一个细边界是 stash。Stash 不是普通 mailbox，它通常表示“这条消息现在还不能处理，先暂存，等状态变化后再拿回来”。Stash 用错了，也会导致内存堆积和顺序困惑。

LogServe 里这些边界都能对应上：

```text
shared log:
  actor:<actor_id> 是状态真相和恢复材料，不只是 mailbox。

task queue:
  control plane 的 queue 是调度候选集合，不等于单 actor mailbox 语义。

command_seq:
  定义 actor command order。

owner/epoch:
  定义谁能执行和提交。

ActorCommandApplied:
  定义状态真正推进。
```

面试里可以这样答：

```text
actor mailbox 不是 Kafka topic、不是 worker pool queue、不是锁，也不是 backpressure 本身。它是某个 actor 的输入顺序边界，服务于这个 actor 的私有状态机。消息队列解决跨服务投递，worker pool 队列解决并行分发，锁解决临界区互斥，event log 解决恢复事实，backpressure 解决压力反馈。LogServe 里 mailbox 语义落在 command_seq 和 dispatch gating 上，ActorCommandSubmitted 和 ActorCommandApplied 不能混为一谈。
```

## Q034. actor mailbox 在高并发场景下可能出现哪些隐藏问题？

**回答：**

高并发下，actor mailbox 的问题通常不是马上报错，而是延迟、内存和调度公平性慢慢变差。

第一个隐藏问题是 unbounded growth。发送方持续比 actor 处理得快，mailbox 会越来越长。短期看只是排队，长期看是内存增长、GC 压力、p99 延迟暴涨，最后 OOM。很多 actor 系统默认发送是异步的，调用方以为“send 成功”就是系统能处理，实际上只是消息进了队列。

第二个问题是 head-of-line blocking。FIFO mailbox 里，只要前面有一条慢消息，后面的快消息也被挡住。比如同一个 actor 里既有 `GetStatus` 又有 `RebuildIndex`，如果 `RebuildIndex` 排在前面，读请求会一起慢。解决办法可以是拆 actor、拆 mailbox、优先级队列、把慢任务异步化，或者把读路径做成快照读。

第三个问题是 hot actor。Actor model 容易让人以为“我有很多 actor，所以能横向扩展”。但负载如果集中在一个 actor id 上，所有消息还是串行。Sharding 能扩展 actor 数量，不能自动拆单个 actor 的状态线。热门房间、热门账号、全局计数器都容易踩这个坑。

第四个问题是公平性。一个 actor 的 mailbox 很长，可能占住调度器大量时间；一个 worker poll 全局队列时，可能反复扫到还没 ready 的 actor command。LogServe `docs/plan.md` 就提到当前 `queue []string` 在线性扫描时复杂度是 `O(queue_depth * polling_workers)`，混合 actor/LLM/普通任务时会成为控制面 CPU 热点。更好的结构是按 actorID 建 `actorPending`，owner worker 只检查自己拥有的 actor 候选任务。

第五个问题是 lock contention。Mailbox 提交路径如果用全局锁，高并发 sender 会在入队时竞争。LogServe 用 per-actor short lock 分配 `command_seq`，这比全局 actor lock 更好，但热点 actor 上这把锁仍然会成为入队瓶颈。

第六个问题是 deadline 和重试风暴。请求在 mailbox 里排了太久，调用方超时后重试。旧请求可能后来仍然执行，新请求也进来了。如果没有 idempotency key 和去重，状态会重复推进；即便有去重，重复请求也会占队列和 CPU。

第七个问题是 payload retention。消息里带大对象、trace context、reply channel、future handle。队列越长，保留的内存越多。排队消息还可能引用本该释放的上下文，造成隐性泄漏。

第八个问题是 priority inversion。你给 mailbox 加优先级后，低优先级消息可能长期处理不到；不加优先级，紧急消息又被慢消息挡住。优先级不是免费午餐，必须配合 aging、quota 或 deadline。

第九个问题是观测缺失。没有 per-actor mailbox depth、oldest message age、enqueue latency、processing latency、drop/reject count，你只能看到“接口超时”。但真正问题可能是排队，不是 actor method 慢。

面试里可以这样答：

```text
actor mailbox 在高并发下最危险的是把压力藏起来。典型问题有无界增长、head-of-line blocking、hot actor、调度不公平、入队锁竞争、deadline 后重试风暴、payload 内存滞留、优先级反转和观测不足。LogServe 当前用 command_seq 保证顺序，但 docs/plan.md 也指出全局 queue 线性扫描在大 backlog 下会成为控制面热点，后续应按 actorID 建 pending queue，并补 per-actor depth 和 queue age 指标。
```

## Q035. actor mailbox 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

这些场景会把 mailbox 从“排队结构”逼成“恢复协议”。面试里要按状态切开看。

第一种边界是消息已经入 mailbox，但 actor 还没开始执行就崩溃。内存 mailbox 会丢消息；持久化 mailbox 或 command log 可以恢复。这里要问：`submitted` 事件是否已经落盘？如果没有，调用方只能重试；如果有，系统重启后应该重新调度。

第二种边界是 actor 正在执行，worker 崩溃。控制面不知道 actor method 是否已经产生外部副作用。Ray 文档里也把 actor failure 和 task retry 讲得很清楚：默认更接近 at-most-once，开启 retry 后会变成 at-least-once 风险，同一个方法可能执行多次。工程上不能承诺“只执行一次”，只能用幂等 key、fencing 和状态提交去重来收敛结果。

第三种边界是 command 已执行，状态提交时崩溃。比如 worker 已经算出新 state，但 `ActorCommandApplied` 没写成功。重试后 method 可能再执行一次。只要提交路径用 idempotency key 和 `command_seq` 检查，状态最多应用一次；但 method 内部的外部副作用仍然要业务自己幂等。

第四种边界是调用方超时，但 actor 后来执行成功。调用方看到 timeout，不代表 command 没生效。这个坑很常见。API 需要返回 call id，让调用方可以查询结果；或者让客户端用 idempotency key 重试，服务端返回同一个 command 的最终结果。不能把 timeout 当 rollback。

第五种边界是重试消息和原消息同时存在。它们可能有相同业务 idempotency key，也可能只是相同 payload。去重必须基于稳定业务 key，而不是“内容看起来一样”。同一个 `transfer(100)` 重试可以去重；两个真实的 `transfer(100)` 不能被误删。

第六种边界是 old owner 迟到完成。Actor ownership 转移后，旧 worker 可能还在执行旧 task。没有 fencing，旧结果会覆盖新状态。LogServe 用 `owner_worker_id + epoch` 拒绝这种 stale completion，`completeActorCall` 里先检查 worker id 和 actor epoch，再检查 `command_seq == command_count + 1`。

第七种边界是 future command 提前被调度。比如 `command_seq=2` 已经在队列里，但 `command_seq=1` 还没 applied。LogServe 有 `TestPollTaskSkipsFutureActorCommandUntilMailboxReady`，明确测试 future command 不会提前 dispatch。

第八种边界是 snapshot 和 mailbox tail 不一致。如果 snapshot 只覆盖到 command 100，恢复时就只能从 101 开始 replay。snapshot_ref、command_count、trim point 必须一致。否则会漏应用或重复应用。

第九种边界是 poison message。一条消息每次处理都 panic 或失败，如果没有失败策略，会卡住整个 actor mailbox。处理方式通常是记录失败、进入 dead letter / failed state、跳过或人工修复，具体取决于业务是否允许越过这条 command。

第十种边界是 shutdown。系统正在 drain mailbox 时又来了新消息。要先关 admission，再等 in-flight 完成，再持久化可恢复边界。否则很容易丢任务或重复执行。

LogServe 当前能比较准确地描述成：

```text
ActorCommandSubmitted 落日志后，表示 command 进入可恢复顺序。
ActorCommandApplied 落日志后，表示 state 真正推进。
owner/epoch fencing 拒绝旧 worker 迟到结果。
command_seq gating 拒绝乱序结果和 future dispatch。
timeout 不等于 command 未生效。
```

面试里可以这样答：

```text
崩溃和重试会暴露 mailbox 的真实语义：消息是只在内存里，还是已经持久化；执行是否可能重复；状态提交是否幂等；timeout 是否可能晚成功；owner 转移后旧结果如何拒绝；snapshot 和 tail log 是否对齐。LogServe 的边界是 exactly-once-ish state application：method 可能重跑，但 ActorCommandApplied 通过 idempotency key、command_seq 和 epoch fencing 控制状态最多按顺序应用一次。
```

## Q036. actor mailbox 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

答案不能只选一个。Actor mailbox 的瓶颈位置取决于消息大小、actor method 类型、是否持久化、调度器结构和部署环境。

CPU 瓶颈常见在三处。第一是 actor behavior 本身，比如状态机逻辑重、序列化反序列化重、JSON diff 重。第二是调度器扫描，比如从一个大队列里反复找 ready command。第三是消息协议处理，比如权限校验、路由、priority、deadline 检查。LogServe 当前一个明确的 CPU 风险就是 `docs/plan.md` 里提到的全局 `queue []string` 线性扫描。

内存瓶颈来自排队消息。Mailbox 存 payload、headers、reply handle、trace context、deadline、retry metadata。消息越大、等待越久，内存越容易涨。GC 语言里，这会变成 heap scan 和 pause。大 state actor 还会把每次任务的 `ActorStateJson` 携带成本放大。

锁竞争常见在入队、出队、ready 检查和状态表更新。一个全局 queue lock 会把所有 actor 的入队/出队串起来；per-actor lock 能降低范围，但热点 actor 还是会竞争。LogServe 的 per-actor submission lock 比全局锁更窄，但 command_seq 分配、metadata update、queue scanning 都仍然需要测。

I/O 瓶颈通常出现在持久化 actor。每条 command 如果都写 `ActorCommandSubmitted` 和 `ActorCommandApplied`，日志写入延迟会直接影响吞吐。snapshot 写对象存储、读 snapshot、logical trim、metadata persistence 也会变成 I/O 边界。没有持久化时，I/O 少，但 crash recovery 语义也弱。

网络瓶颈出现在分布式 actor。消息要跨节点，actor placement 不稳定，reply 要回调用方，snapshot 或 state 要跨存储系统。网络延迟会拉长 queue wait，也会影响 owner heartbeat 和 lease 判断。网络分区还会把性能问题变成正确性问题。

实际排查要把延迟拆成几段：

```text
enqueue_latency:
  从 API 收到请求到写入 mailbox/command log。

queue_wait:
  从进入 mailbox 到被 worker 拿到。

execution_time:
  actor method 真正执行时间。

commit_time:
  ActorCommandApplied / state update / snapshot 写入时间。

reply_latency:
  结果回到调用方的时间。
```

如果 queue wait 高、execution time 低，是排队或调度瓶颈；如果 execution time 高，是 actor method 或外部 I/O 慢；如果 commit time 高，是日志、数据库或对象存储慢；如果 enqueue latency 高，可能是 admission、入队锁或日志写入慢。

LogServe 现有 README 说 workflow 和 actor 命令会打结构化日志，actor 命令包括 actor id、call id、epoch、command count，replay 会暴露 full replay 和 snapshot replay command counts。后续如果要把 mailbox 性能讲完整，应补 per-actor mailbox depth、oldest pending age、ready/blocked command count、queue scan iterations、owner poll hit rate、stale completion count。

面试里可以这样答：

```text
actor mailbox 的瓶颈可能在 CPU、内存、锁、I/O 或网络。CPU 常在调度扫描和序列化，内存在排队 payload，锁在入队出队和状态表，I/O 在持久化 log/snapshot，网络在分布式路由和 owner lease。不要只看接口 p99，要拆 queue_wait、execution_time 和 commit_time。LogServe 当前除了 actor method 本身，还要重点关注全局 queue 扫描、command log 写入、StateJSON 携带和 per-actor lock 热点。
```

## Q037. actor mailbox 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

这三类测试不要混。Correctness test 测语义对不对；stress test 测高压和故障下会不会露出竞态；benchmark 测性能曲线和瓶颈在哪里。

Correctness test 要先覆盖最小不变量：

```text
order:
  同一个 actor 的 command_seq 单调递增。

single active execution:
  同一个 actor 不能同时执行两个会修改状态的 command。

ready gating:
  command_seq = n+1 之前，command_seq = n+2 不能先 dispatch。

state transition:
  每条 applied command 都从旧状态生成新状态。

idempotency:
  同一个 command 重复完成，状态不能重复推进。

fencing:
  旧 owner、旧 epoch、旧 lease 的 completion 被拒绝。

timeout:
  调用方 timeout 不破坏 mailbox 内部顺序。

recovery:
  full replay 和 snapshot replay 得到同一个 state。
```

LogServe 现有测试已经覆盖了很多关键点：1000 并发 `inc()` 最终为 1000；future command 不提前 dispatch；poll 时注入最新 actor state；recovered owner 能接收队列中的 command；stale completion 被拒绝；out-of-order command 被拒绝；`ActorCommandSubmitted` 在 `ActorCommandApplied` 前面。

Stress test 要把系统推到容易出错的边界：

```text
many producers -> one actor:
  1,000 / 10,000 个并发 sender 打同一个 actor。

many actors -> many workers:
  大量 actor id 混合调度，看公平性和锁竞争。

hot + cold mix:
  一个热点 actor 和很多冷 actor 混在一起，看冷 actor 是否饿死。

timeout + retry:
  大量调用方超时后重试，看重复消息是否占满 mailbox。

crash injection:
  在 submitted 后、dispatch 后、execution 中、applied 前、snapshot 中分别 crash。

slow command:
  慢消息挡住快消息，验证 HOL blocking 指标和降级策略。

large payload / large state:
  看内存、GC、snapshot、replay 和网络传输。
```

Benchmark 要固定变量，测曲线，而不是只报一个 QPS。至少应该有：

```text
single actor throughput:
  一个 actor 的最大 command/s。

multi actor throughput:
  actor 数从 1、10、100、1000 增加时吞吐变化。

enqueue latency:
  入 mailbox 或写 submitted log 的延迟。

queue wait:
  pending 到 dispatch 的等待时间。

execution latency:
  actor method 真实执行时间。

commit latency:
  applied log / metadata update / snapshot 写入时间。

memory per pending message:
  mailbox depth 增加时 heap 变化。

lock wait / scan cost:
  per-actor lock、global queue lock、queue scan iteration。
```

Correctness test 通常用 deterministic fake clock、fake log、fake worker；stress test 用并发、随机延迟、故障注入；benchmark 要关掉不必要日志，固定 payload 大小、actor 数、worker 数、snapshot 频率，并分别测内存 mailbox 和持久化 mailbox。

面试里可以这样答：

```text
correctness test 测语义：顺序、单活、ready gating、幂等、fencing、timeout 和 recovery。stress test 测压力下的竞态：多 producer、热点 actor、冷热混合、超时重试、crash injection、大 payload。benchmark 测性能曲线：单 actor 吞吐、多 actor 扩展、enqueue latency、queue wait、execution time、commit time、内存和锁等待。LogServe 现有测试已经覆盖 command_seq、future dispatch、stale completion 和并发 inc，下一步应补混合 backlog、队列扫描成本和故障注入压测。
```

## Q038. 如果要求从零实现一个简化版 actor mailbox，你会先定义哪些不变量？

**回答：**

我会先定义不变量，而不是先写队列代码。因为 mailbox 最难的不是 `push` 和 `pop`，而是哪些状态永远不能被破坏。

最小版本可以先定义这些不变量：

```text
identity invariant:
  每条消息必须属于一个明确 actor_id。

order invariant:
  同一个 actor 的消息有单调 sequence，处理顺序只能是 last_applied_seq + 1。

single-consumer invariant:
  同一个 actor 同一时刻最多只有一个会修改状态的消息 in-flight。

durability invariant:
  如果承诺 crash recovery，消息在 dispatch 前必须先持久化。

apply invariant:
  只有消息处理成功并提交后，last_applied_seq 才能前进。

idempotency invariant:
  同一个 message_id / command_id 重复提交，只能产生一个 accepted result。

fencing invariant:
  分布式或可迁移 actor 中，只有当前 owner/epoch 可以提交结果。

boundedness invariant:
  mailbox depth、bytes、oldest age 或 total pending 至少有一个上限。

deadline invariant:
  已过期消息不能无限占住队列；过期策略必须明确。

observability invariant:
  mailbox depth、oldest age、in-flight、rejected、retried 必须可观测。
```

如果只做单机内存版，可以先简单一些：

```text
map[actorID]*Mailbox
Mailbox:
  pending []Message
  inFlight bool
  nextSeq uint64
  appliedSeq uint64
```

提交消息时分配 `seq = nextSeq`，然后 `nextSeq++`。调度器只在 `inFlight == false` 且队首 `seq == appliedSeq + 1` 时把消息交给 worker。worker 完成后，如果 `seq == appliedSeq + 1`，推进 `appliedSeq`，清掉 `inFlight`，再唤醒下一条。

如果要支持恢复，就必须把 `MessageSubmitted` 和 `MessageApplied` 写到 durable log：

```text
submit:
  append MessageSubmitted(actor_id, command_id, seq, payload)
  enqueue pending

complete:
  check owner/epoch/seq
  append MessageApplied(actor_id, command_id, seq, result, state_delta)
  update applied_seq
```

如果要支持 retry，就要把 reply 和 execution 分开。调用方 timeout 只代表等待结果超时，不代表消息自动取消。取消也要变成 mailbox 里的消息或状态标记，而不是随手删 pending。

如果要支持 bounded mailbox，就要提前定义满了怎么办：

```text
reject:
  直接返回 overloaded。

block:
  让 sender 等待容量。

drop newest / drop oldest:
  只适合允许丢弃的 telemetry。

priority:
  允许高优先级插队，但要防 starvation。
```

LogServe 已经把几个关键不变量写进实现了：`command_seq` 单调；`ActorCommandSubmitted` 先于任务入队；`PollTask` 只 dispatch 下一条 command；`CompleteTask` 检查 owner/epoch 和 `command_seq`；`ActorCommandApplied` 用 idempotency key；snapshot replay 用 `command_count` 对齐。

面试里可以这样答：

```text
从零实现 actor mailbox，我会先定不变量：每条消息属于一个 actor；同 actor sequence 单调；一次最多一个修改状态的 command in-flight；只处理 applied_seq+1；提交结果必须检查 message id、owner/epoch 和 sequence；mailbox 有容量或年龄上限；timeout、cancel、retry 都有明确语义；如果要恢复，dispatch 前先持久化 submitted，状态推进写 applied。队列数据结构可以换，不变量不能丢。
```

## Q039. actor mailbox 的常见误用是什么，误用后通常会产生什么线上症状？

**回答：**

第一种误用是把 mailbox 当无限缓冲。发送方一直 `tell`，系统不拒绝、不限速、不暴露 depth。线上症状是 p99 延迟先涨，随后内存和 GC 涨，最后出现 OOM、请求超时和重试风暴。最麻烦的是，业务方一开始会以为“actor 还活着”，但其实队列已经不可恢复地变长了。

第二种误用是在 actor method 里做长时间阻塞 I/O。比如直接等数据库慢查询、HTTP 调用、模型推理、大文件上传。线上症状是同一个 actor 后面的所有消息都慢，表现为局部热点、head-of-line blocking、健康检查正常但业务超时。

第三种误用是把一个 actor 当全局锁。比如全站所有库存、所有用户余额、所有任务调度都塞进一个 actor。短期代码很简单，长期就是单点瓶颈。线上症状是单 actor mailbox depth 很高，CPU 只忙一个 worker，横向扩容没效果。

第四种误用是把大 payload 或大 state 放进 mailbox。线上症状是内存异常高、GC pause、网络包大、snapshot 慢、replay 慢。排队消息还会把本来可以释放的大对象引用住。

第五种误用是不定义幂等和去重。调用方 timeout 后重试，actor 收到两条业务上相同的命令。线上症状是重复扣款、重复发邮件、重复创建资源，或者状态推进两次。相反，如果去重 key 定义太粗，又会误删真实请求。

第六种误用是把 mailbox 顺序当全局顺序。不同 actor 之间没有全局顺序，不同 sender 的消息顺序也不等于真实发生时间。线上症状是跨 actor 读到不一致状态，或者 A actor 认为事件已经发生，B actor 还没处理。

第七种误用是忽略 poison message。一条消息永远失败，却一直排在队首重试。线上症状是 actor 卡死、后续消息全阻塞、日志里重复同一个错误。

第八种误用是随便开启 reentrancy 或并行处理同一 actor。这样会把 mailbox 的单线程状态机语义打穿。线上症状通常不是马上 crash，而是偶发状态倒退、lost update、重复回复、版本号跳变。

第九种误用是没有 dead letter / rejected / expired 指标。消息被丢、过期、拒绝、重试，调用方只看到 timeout。线上排查时只能看业务日志，无法判断是排队、执行慢、提交慢还是丢消息。

第十种误用是把 actor mailbox 当成持久化保证。内存 mailbox 一重启就没了；如果没有 durable submitted log，不能说 actor command 可恢复。线上症状是重启后少处理一批消息，或者调用方重试后出现重复处理。

LogServe 的文档已经规避了一些说法。它没有说严格 exactly-once execution，而是说 exactly-once-ish actor command application；没有说 actor mailbox 是完整分布式队列，而是说 actor source of truth 是 shared log stream。当前还需要继续补的是 admission control、per-actor depth 指标、混合 backlog 下的调度优化和 poison command 策略。

面试里可以这样答：

```text
actor mailbox 常见误用包括无界缓冲、在 actor 内做阻塞 I/O、把一个 actor 当全局锁、塞大 payload、没有幂等、误以为有全局顺序、忽略 poison message、随便 reentrant、没有队列指标、把内存 mailbox 当持久化队列。线上症状通常是 p99 暴涨、内存和 GC 上升、热点 actor 卡死、横向扩容无效、重复副作用、状态偶发错乱、重启后丢消息或重复处理。
```

## Q040. actor mailbox 在单机和分布式环境中的语义有什么差异？

**回答：**

单机 actor mailbox 和分布式 actor mailbox 最大的差异，是失败模型完全不同。

单机内存 mailbox 里，消息入队、出队、状态更新通常发生在同一个进程或同一台机器上。你可以用 mutex、channel、condvar、本地调度器保证顺序。时钟、网络分区、owner 迁移这些问题基本不出现。缺点也明显：进程崩溃后，内存 mailbox 和内存 state 都没了，除非你额外写日志或 snapshot。

单机多进程会复杂一点。worker 可能崩溃，控制面还活着；或者控制面重启，worker 还以为自己持有任务。这个时候需要 task lease、heartbeat、epoch、幂等 completion。LogServe 当前验证范围更接近这里：单机多进程，控制面用 shared log、metadata、owner worker、epoch fencing 和 `command_seq` 维护 actor state application 顺序。

分布式环境会多出几类问题。

第一，消息可能丢失、重复、乱序或延迟很久。网络不是本地函数调用。发送成功不代表对方处理成功；超时不代表对方没处理；重试可能造成重复执行。

第二，actor location 会变化。调用方不应该直接知道 actor 在哪个节点，否则迁移和扩容会把路由打碎。需要 actor id 到 placement 的映射、shard coordinator、directory 或 naming service。

第三，ownership 必须可证明。两个节点都以为自己拥有同一个 actor，就是 split-brain。必须有 lease、epoch、fencing token，且 epoch 分配本身要在线性化存储或共识日志里。Akka Cluster Sharding 文档也提醒，split brain 下如果处理不当，会出现同一个 sharded entity 在两个分区同时运行的风险。

第四，持久化语义要更清楚。内存 mailbox 在分布式里不够。至少要明确 submitted command、applied event、snapshot、trim point 存在哪里，谁负责 replay，replay 后从哪个 sequence 继续。

第五，backpressure 要跨网络传播。单机 bounded queue 可以阻塞 sender；分布式 actor 中，sender 可能已经在另一个节点，阻塞、拒绝、drop、shed load 都要变成协议。否则只会把压力从一个节点转移到另一个节点。

第六，观测要按 actor、node、shard 拆开。单机只看 queue depth 还勉强够；分布式要看 per-shard mailbox depth、owner migration count、remote send latency、dropped/dead letters、lease renew latency、split-brain/downing events。

第七，时间语义更危险。依赖本地 clock 判断顺序通常不可靠。顺序最好来自 log offset、command_seq、version、epoch，而不是“我这里的时间戳更早”。

可以这样对比：

```text
single-process mailbox:
  顺序靠内存队列和锁。
  crash 后默认丢消息。
  没有网络分区和 ownership 迁移。

single-node multi-process mailbox:
  需要 lease、heartbeat、worker crash recovery。
  可以用本机共享控制面和 durable log 收敛状态。

distributed mailbox:
  需要 placement、routing、durable log、fencing、split-brain handling、跨节点 backpressure。
  通常只能承诺 at-least-once + idempotent state application，或者明确的 at-most-once。
```

LogServe 的表述要保持克制。它已经有 actor id、owner worker、epoch、command_seq、snapshot replay，这些是分布式 actor runtime 的重要材料；但当前 README 和测试证明的是单机多进程范围，不是完整 cluster sharding 或多副本强一致 actor 系统。面试里这样说更稳：

```text
单机 mailbox 主要是本地并发问题，靠队列、锁和调度器保证同 actor 串行。分布式 mailbox 是协议问题，要处理消息丢失/重复/乱序、actor placement、owner 迁移、split-brain、持久化恢复和跨节点 backpressure。LogServe 当前实现了单机多进程下的 shared-log actor mailbox：command_seq 定义顺序，epoch fencing 拒绝旧 owner，snapshot replay 做恢复；如果扩成多节点，还要补 shard coordinator、共识保护的 ownership 和跨节点压力控制。
```

## Q041. actor reentrancy 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

Actor reentrancy 的核心目标不是让 actor “多线程并行”，而是在一个请求因为 `await`、远程调用或 I/O 暂停时，允许同一个 actor 继续处理某些其他请求。它主要解决活性和性能问题，顺带改善一部分可用性；但它会显著增加正确性风险。

非 reentrant actor 的语义最容易推理：

```text
request A starts
request A finishes
request B starts
request B finishes
```

Reentrant actor 会允许这种交错：

```text
request A starts
request A awaits DB / another actor / remote call
request B starts
request B changes actor state
request B finishes
request A resumes
request A continues from old assumptions
```

Orleans 官方文档对这个点讲得很直接：grain activation 默认是 single-threaded，请求从开始到结束处理完才处理下一条；reentrancy/interleaving 允许请求在异步等待期间交错执行。它还特别警告，reentrancy 是高级特性，错误使用会导致 race condition、state corruption 和难诊断的 bug。这里的 race 很多不是传统数据竞争，而是逻辑竞态。

所以 reentrancy 的收益和风险要分开说：

```text
它解决活性：
  A 等 B，B 又回调 A 时，非 reentrant actor 可能形成循环等待。

它改善 I/O 利用率：
  actor 等数据库、对象存储、另一个 actor 时，可以处理只读请求或互不影响的请求。

它提高尾延迟：
  快速 read-only 请求不一定要排在慢 I/O 后面。

它牺牲简单正确性：
  一个请求从 await 恢复时，actor state 可能已经被别的请求改过。
```

它不是主要解决安全性。恰恰相反，默认 non-reentrant 的 actor 更安全，因为状态转移边界清楚。Reentrancy 如果做得好，可以避免死锁和资源占用；做得不好，会把 actor 原本最有价值的“单条状态线”打碎。

它也不是纯粹的可维护性工具。对简单业务，reentrancy 往往降低可维护性，因为读代码时必须脑内枚举所有 await 点和交错路径。只有当请求协议已经明确区分 read-only、mutating、idempotent、call-chain reentrant 等类型时，reentrancy 才会让系统更好维护。

LogServe 当前没有开放 reentrant actor。它更偏保守：同一个 actor 的 command 通过 `command_seq` 串行应用，worker 侧也有 per-actor lock，控制面只接受 `command_seq == command_count + 1` 的结果。这个模型吞吐不一定最高，但恢复、测试和面试解释都更稳。可以说它优先选择了 correctness 和 recoverability，而不是追求 actor 内交错执行。

面试里可以这样答：

```text
actor reentrancy 的核心目标是活性和性能：在一个请求 await 时，允许 actor 处理其他可交错请求，避免循环等待，也减少 I/O 等待造成的空转。它不是默认正确性增强，反而会暴露中间状态，要求 read-only 标注、interleave predicate、版本校验和交错测试。LogServe 当前没有做 reentrant actor，而是用 command_seq 串行应用状态，这更利于恢复和 exactly-once-ish state application。
```

## Q042. actor reentrancy 的典型适用场景和不适用场景分别是什么？

**回答：**

Reentrancy 适合 I/O 等待占主要时间、并且请求之间可以证明不会破坏同一个不变量的 actor。它不适合“所有方法都会读写同一份状态”的 actor。

典型适用场景有几类。

第一类是 read-only 查询。比如一个 user grain 有 `GetProfile()`、`GetDisplayName()`、`GetPreferences()`，这些方法只读内存状态，不修改版本，也不触发外部副作用。Orleans 用 `[ReadOnly]` 表达这个意思：只读方法可以和其他只读方法并发处理，从而避免被慢读挡住。

第二类是 call-chain reentrancy。A 调 B，B 需要回调 A 取一个只读信息。如果 A 不允许这条调用链 re-enter，就可能死锁。Orleans 的 `AllowCallChainReentrancy()` 就是为这种细粒度场景准备的，它只允许调用链下游在作用域内回调，而不是把整个 grain 标成随便 reentrant。

第三类是 actor 内部有明显独立子资源。比如一个 actor 管多个互不相关的 session slot，方法带 `slot_id`，不同 slot 可以交错，同一 slot 仍然串行。这里的关键不是“打开 reentrancy”，而是把冲突域定义清楚。

第四类是 I/O 型 actor。比如 actor 作为外部 API 代理，大部分时间在等网络。只要本地状态只是缓存或统计，不影响关键不变量，允许一定交错能提高吞吐。

第五类是健康检查、metrics、debug query。Ray 的 concurrency groups 文档给了一个很实用的例子：健康检查方法可以有独立并发配额，不要和请求服务方法抢同一个队列。这个思想和 actor reentrancy 很接近：把不影响业务状态的控制面请求单独放行。

不适用场景也很明确。

第一，金融账本、库存扣减、订单状态机这类强不变量 actor，不应该轻易 reentrant。它们通常要求“检查条件”和“修改状态”之间不能被插入别的写请求。

第二，method 在 await 前读了状态，await 后继续使用这个假设。如果没有版本校验，就不适合 reentrant。典型例子是：

```text
if balance >= amount:
  await risk_check()
  balance -= amount
```

`await` 后必须重新检查 `balance`，否则就是过期判断。

第三，actor method 有外部副作用，而且副作用和本地状态提交不是一个原子动作。比如发邮件、扣第三方账、调用支付网关。Reentrancy 会让副作用顺序更难讲清楚。

第四，状态很大或 snapshot 很频繁。Reentrancy 会让“什么时候可以安全 snapshot”变复杂，因为 actor 可能有多个未完成 continuation。Snapshot 必须知道哪些修改已经正式提交，哪些只是某个请求的中间变量。

第五，业务团队很难写出交错测试。这个理由很实际。如果没有测试能力，reentrancy 只是把线上偶发 bug 提前埋好。

LogServe 当前更适合非 reentrant 场景：每个 actor command 返回新的完整 `StateJSON`，控制面按序提交。它适合机制验证、恢复验证、snapshot replay，不适合直接拿来证明 reentrant actor 的吞吐优势。如果未来要加，优先从 read-only method 或 query method 开始，而不是让 `inc()`、`transfer()` 这种写方法随便交错。

面试里可以这样答：

```text
reentrancy 适合 read-only 查询、受限 call-chain 回调、I/O 等待型 actor、互不冲突的子资源和健康检查这类控制面请求。不适合账本、库存、订单状态机、await 前后依赖同一份状态假设的方法，也不适合外部副作用顺序很敏感的 actor。默认 non-reentrant，按方法或调用链逐步放开，是比较稳的路线。
```

## Q043. actor reentrancy 和相近概念最容易混淆的边界在哪里？

**回答：**

Reentrancy 最容易和并行、异步、线程池、优先级、batching、read-only cache、锁重入混在一起。

先说 reentrancy 和并行。Reentrant 不一定是多线程。Orleans 文档明确说 reentrant grain 仍然是 single-threaded，一次只执行一个 turn；只是不同请求的 continuation turn 可以交错。也就是说，不是两段 grain 代码同时在两个 CPU 核上跑，而是：

```text
A turn 1
B turn 1
A turn 2
B turn 2
```

Ray 的 async actor 也有类似边界：它在单个 Python event loop 里 multiplex 多个 coroutine，`await` 才会让出执行权；如果你在 async actor method 里调用阻塞的 `ray.get` 或 `ray.wait`，会卡住 event loop。这里的“并发”更多是协作式调度，不是随便共享状态的线程并行。

再说 reentrancy 和 async。`async/await` 只是语法和调度机制，不自动意味着 actor 是 reentrant。一个 actor 可以写 async 方法，但 runtime 仍然规定同一时间只处理一个请求直到完成。反过来，一个非 async runtime 也可能用线程池做并发执行。要看的是“未完成请求期间是否允许另一条请求进入同一个 actor 的逻辑状态边界”。

第三，reentrancy 和线程池不同。线程池并行会引入真正的数据竞争，需要锁保护字段。Reentrancy 可能仍是单线程，但会引入 interleaving bug。它们的 bug 长得不一样：线程池 bug 是两个线程同时写；reentrancy bug 是 A 在 await 前读到旧状态，B 改了状态，A 恢复后继续用旧假设。

第四，reentrancy 和优先级队列不同。优先级是改变消息处理顺序；reentrancy 是允许未完成请求的后续 turn 和其他请求交错。一个 actor 可以有优先级但不 reentrant，也可以 reentrant 但没有优先级。

第五，reentrancy 和 batching 不同。Batching 是把多条消息合成一次处理，通常仍然在一个明确事务边界里推进状态。Reentrancy 是多个请求的执行片段互相穿插。Batching 往往让顺序更清楚，reentrancy 让顺序更复杂。

第六，reentrancy 和 read-only cache 不同。只读查询可以安全交错，是因为它不改变 actor state。Reentrant 写请求要难得多。很多线上事故来自把“这个方法大多数时候只是读”误标成 read-only。

第七，reentrancy 和可重入锁不是一回事。可重入锁是同一个线程重复进入同一把锁；actor reentrancy 是一个 actor 在一个请求未完成时允许其他请求进入。名字相近，语义完全不同。

LogServe 当前应该明确说“不支持 actor reentrancy”。它有 async call 等待，但 mailbox 入队和状态应用仍然是 `command_seq` 顺序；它也有 worker 并发，但不是同一个 actor 内的 reentrant method 交错。把这几件事混为一谈，会高估实现能力。

面试里可以这样答：

```text
reentrancy 不是多线程并行，也不是有 async 就自动 reentrant。它指一个 actor 的请求没有完成时，允许其他请求或 continuation turn 进入同一个 actor 的执行边界。它和线程池、优先级队列、batching、read-only 查询、可重入锁都不同。LogServe 目前是非 reentrant actor：并发请求可以入 mailbox，但状态应用仍按 command_seq 串行。
```

## Q044. actor reentrancy 在高并发场景下可能出现哪些隐藏问题？

**回答：**

高并发下，reentrancy 的问题通常不是简单的 data race，而是交错路径数量爆炸。每个 `await` 都可能变成一个状态被别人插入修改的点。

第一个隐藏问题是 stale assumption。请求 A 在 await 前检查了条件，await 后继续执行；中间请求 B 改了状态。A 的代码看起来是顺序的，但逻辑前提已经过期。账户余额、库存余量、状态机 phase 都会这样出问题。

第二个问题是 lost update。两个请求都读到同一个旧值，各自 await，然后都基于旧值写回。即使没有两个线程同时运行，最后仍然丢更新。

第三个问题是 invariant 被中间状态打穿。比如 actor 在处理 A 时先写 `status = Processing`，await 外部任务，最后写 `status = Done`。如果 B 在 `Processing` 期间进来，并且业务没定义这个状态能不能被观察，就会出现奇怪分支。

第四个问题是 starvation。允许 read-only 或 high-priority 方法一直 interleave，写请求可能长期等不到完整执行窗口。系统表面吞吐很高，关键状态却不推进。

第五个问题是 concurrency cap 失控。Ray async actor 默认可以让很多 coroutine 挂在 event loop 上；如果每个 coroutine 都持有 payload、ObjectRef、trace context、临时 buffer，内存会很快涨。Orleans 里如果把整个 grain 标成 `[Reentrant]`，也会扩大交错范围。

第六个问题是外部系统压力被放大。非 reentrant actor 本来串行地调用数据库；reentrant 以后，同一个 actor 可以挂起很多 DB 请求。actor 本地不忙了，但下游连接池被打满。

第七个问题是消息顺序直觉失效。调用方看到 A 先发、B 后发，但 actor 内部可能 A 开始、B 完成、A 再恢复。对只读方法可能没问题，对写方法就很危险。

第八个问题是 debug 很难。日志按请求 id 看都正常，按 actor id 看才发现交错。没有 turn id、state version、request id、await point，很难复现。

第九个问题是 snapshot 边界变复杂。非 reentrant actor 在一条 command applied 后 snapshot 很自然；reentrant actor 里可能有多个未完成请求。snapshot 不能捕获半完成请求的局部变量，只能捕获已经提交的 actor state。

第十个问题是重试与 interleaving 叠加。A 超时后客户端重试，A 的原请求还可能恢复；B 又插进来改状态。没有幂等和版本校验，线上症状会像“偶发重复执行”。

LogServe 现在不引入 reentrancy，避开了这些隐藏问题。它的瓶颈会落在 per-actor 串行吞吐和 queue wait 上，但换来的是 replay reducer 更简单、`ActorCommandApplied` 更容易验证、snapshot command count 更清楚。

面试里可以这样答：

```text
reentrancy 在高并发下最怕交错路径失控。常见问题是 await 后状态假设过期、lost update、中间状态被观察、read-only 请求饿死写请求、挂起 coroutine 占内存、下游 I/O 被放大、日志难复现、snapshot 不知道哪些状态已正式提交。解决办法不是一键打开 reentrant，而是按方法声明、限制并发、状态版本校验、await 后重读状态，并给每个 await 点做交错测试。
```

## Q045. actor reentrancy 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

Reentrancy 一遇到故障，最难回答的问题是：一个已经开始但还没完成的请求，到底有没有改变 actor 的正式状态？

第一种边界是 await 前已经修改了内存状态，但还没有持久化。actor 崩溃后，这段修改丢失；如果外部副作用已经发生，就会出现外部世界和 actor 状态不一致。reentrant actor 更容易这样，因为它鼓励在 await 之间拆成多个 turn。

第二种边界是 await 前发出了外部请求，actor 重启后原 continuation 没了。外部请求可能晚点成功，但 actor 已经不知道这件事。可靠设计通常要把“发出外部请求”也变成持久事件，或者用 outbox/command id 去查询、补偿。

第三种边界是调用方超时后重试。原请求可能还在 actor 里挂着，重试请求又进来了。非 reentrant actor 至少顺序清楚；reentrant actor 可能让原请求和重试请求交错执行。没有 idempotency key 和 request version，就可能重复提交。

第四种边界是 actor ownership 转移。旧 activation 里还有未完成 continuation，新 activation 已经在别的 worker 上恢复。旧 continuation 如果能提交状态，就会覆盖新状态。分布式 reentrancy 必须配合 epoch fencing。LogServe 当前的 owner/epoch 检查能拒绝旧 owner completion，但它没有 continuation-level reentrancy，因此问题空间小很多。

第五种边界是 call-chain reentrancy scope 丢失。Orleans 的 call-chain reentrancy 是作用域内允许回调。如果请求超时、重试或跨节点恢复，scope 不能被误当成长期权限。否则本来只允许一次回调，变成任意请求都能插队。

第六种边界是 snapshot 捕获半完成状态。Snapshot 只能记录 actor 已提交逻辑状态，不能记录某个 async 方法的本地栈、await 中的 future、未完成 DB call。恢复后这些 continuation 不会自然回来，除非你把它们建模成持久 command 或 workflow step。

第七种边界是异常处理顺序。A await 后抛异常，B 在 A 等待期间已经改了状态。A 的 catch/finally 如果尝试回滚，可能会把 B 的修改一起回滚掉。补偿逻辑必须基于版本和 request id，而不是直接恢复旧 state。

Ray 的 actor fault tolerance 文档也能说明这个边界：默认 actor task 更接近 at-most-once，开启 retry 后会出现方法可能执行两次的情况；如果 actor 有关键状态，应用需要自己 checkpoint 和恢复。Reentrancy 只会让这件事更难，因为“执行到哪里”不是一个单一位置。

面试里可以这样答：

```text
reentrancy 下的故障边界是：await 前后的修改是否已经正式提交，外部副作用是否可幂等，超时重试是否会和原请求交错，旧 activation 的 continuation 是否会在 owner 转移后迟到提交，snapshot 是否只捕获已提交状态。可靠做法是把 request id、state version、epoch、outbox 和 submitted/applied event 做明确，不要把内存 continuation 当成可恢复状态。
```

## Q046. actor reentrancy 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

Reentrancy 的初衷通常是解决 I/O 等待和循环等待，但打开以后，瓶颈可能转移到内存、调度器、下游 I/O 或状态校验。

I/O 是最常见的原始瓶颈。actor method 等数据库、远程 actor、对象存储、HTTP 或模型服务时，non-reentrant actor 会空等。Reentrancy 让 actor 在等待期间处理别的请求，所以对 I/O-bound actor 有帮助。但这不代表 I/O 压力消失了，它只是从 actor mailbox 转移到下游连接池、远程服务和网络。

内存是第二个瓶颈。每个挂起请求都保留 continuation、参数、reply handle、trace context、部分中间结果。并发越高，挂起对象越多。Ray async actor 文档提到 async actor 可以设置 `max_concurrency`，默认并发度很高；如果方法携带大 payload，内存会先出问题。

CPU 瓶颈通常来自调度和序列化。大量 coroutine/turn 在 actor 内部切换，runtime 要维护 ready queue、future completion、request context。业务如果每次恢复都做 JSON 序列化、版本校验、权限检查，也会消耗 CPU。

锁竞争在两种实现里出现。单线程 reentrant actor 本身不需要锁保护 actor state，但如果你给不同 interleaving group 加了局部锁，或者在 actor 外还有 metadata store、result store、scheduler lock，就会有竞争。线程池式 actor 更明显，字段访问必须加锁或用 immutable state。

网络瓶颈在分布式 reentrancy 里更明显。actor A 等 actor B 时释放执行权，B 又回调 A。每个交错 turn 可能是一次网络 round trip。call-chain reentrancy 如果跨节点传播，还要携带上下文和权限边界。

还有一个隐形瓶颈是版本校验。如果每个 await 后都要重新读状态、检查 version、重算 diff，CPU 和 I/O 都会上升。这个成本是正确性成本，不能随便省。

对 LogServe 来说，当前没有 reentrant actor，所以它的 actor 性能瓶颈更多在 queue wait、worker 执行、完整 `StateJSON` 传输、snapshot 写入和全局 queue 扫描。假如未来加 reentrancy，瓶颈会新增几类指标：

```text
inflight_turns_per_actor
awaiting_requests_per_actor
interleaving_reject_count
state_version_conflict_count
continuation_resume_latency
downstream_io_concurrency
```

面试里可以这样答：

```text
reentrancy 通常是为了缓解 I/O 等待，但瓶颈不一定消失。它可能把压力转到挂起 continuation 的内存、event-loop 调度、下游连接池、远程 actor round trip 和 await 后的版本校验。CPU-bound actor 开 reentrancy 收益很有限；I/O-bound actor 有收益，但必须配并发上限、下游限流和状态版本检查。
```

## Q047. actor reentrancy 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

Reentrancy 的测试重点不是“并发请求都返回成功”，而是“所有允许的交错都不破坏不变量”。这比普通 actor mailbox 测试难一档。

Correctness test 要先写出具体交错：

```text
stale assumption:
  A 读 state 后 await，B 修改 state，A 恢复时必须重新校验。

lost update:
  A/B 都基于旧版本计算，只有一个能提交，另一个重试或失败。

read-only interleaving:
  read-only 方法交错执行，不能改变 state、version、pending effect。

call-chain reentrancy:
  A -> B -> A 的受限回调可以通过，但其他请求不能借这个 scope 插队。

reentrant + snapshot:
  snapshot 只能包含已提交 state，不能包含半完成 request。

exception path:
  A await 后失败，不得回滚 B 已提交的更新。

timeout + retry:
  原请求和重试请求交错时，幂等 key 收敛到一个结果。
```

这些测试最好用 deterministic scheduler。手工控制每个 await 点，让测试按指定顺序推进：

```text
A reaches await point 1
B runs and commits
A resumes
assert final state
```

Stress test 要随机化交错。大量请求打同一个 actor，方法里插入随机 sleep、随机外部调用延迟、随机异常和超时。目标不是追求吞吐，而是逼出状态版本冲突、starvation、内存增长和偶发死锁。可以把每次状态变化记录成 event，再用 checker 验证 invariants。

Benchmark 要分三组测。

第一组是 non-reentrant baseline。单 actor 串行处理同样 workload，得到 p50/p99、queue wait 和吞吐。

第二组是 read-only reentrancy。大量读加少量写，看读延迟是否下降，写延迟是否被饿死。

第三组是 I/O-bound reentrancy。method 里模拟数据库或远程 actor，调整并发上限，看吞吐从哪里开始不再增长，并观察内存和下游 I/O。

指标要包括：

```text
throughput
p50/p95/p99 latency
queue_wait
await_time
resume_latency
state_version_conflicts
retry_count
starvation_time
inflight_requests
memory_per_inflight
downstream_error_rate
```

LogServe 如果未来实现 reentrant actor，现有测试需要扩展。现在的 `TestActorConcurrentMailboxSerializes1000Increments` 证明非 reentrant mailbox 能把写请求串起来；reentrant 版本则要证明只读方法能交错、写方法不能乱序、await 后版本冲突会被拒绝或重试。

面试里可以这样答：

```text
correctness test 要测具体交错：await 前后状态变化、lost update、read-only 标注、call-chain scope、异常回滚、timeout retry 和 snapshot 边界。stress test 用随机调度、随机延迟和故障注入找偶发交错 bug。benchmark 要和 non-reentrant baseline 比，分别测 read-only workload 和 I/O-bound workload，不能只看吞吐，还要看 version conflict、starvation、in-flight 内存和下游错误率。
```

## Q048. 如果要求从零实现一个简化版 actor reentrancy，你会先定义哪些不变量？

**回答：**

我不会先写调度器，而是先定哪些请求允许交错、哪些状态不能跨 await 暴露。最小不变量可以这样定。

```text
turn invariant:
  即使 reentrant，actor 也按 turn 执行。一个 turn 内不能被打断，只有 await/yield 点可以切换。

interleave policy invariant:
  每个方法必须声明 interleave 策略：none、read_only、call_chain、keyed、always。

state version invariant:
  每个 mutating request 在 await 后提交前，必须检查 state_version 是否仍符合预期。

commit invariant:
  actor state 只能在 commit 点推进，不能把半完成 request 的局部状态写成正式状态。

read-only invariant:
  read-only 方法不得修改 state、version、outbox、timer、snapshot metadata。

call-chain invariant:
  call-chain reentrancy 只在 scope 内有效，不能变成长期权限。

bounded concurrency invariant:
  每个 actor、每个方法组、每个 downstream 依赖都有并发上限。

fencing invariant:
  分布式 actor 中，continuation 提交也必须携带 owner epoch / activation id。

snapshot invariant:
  snapshot 只覆盖已 committed state_version，不包含 pending continuation。

observability invariant:
  每次 interleave、拒绝、版本冲突、重试、长时间挂起都要有指标。
```

一个简化实现可以把请求拆成 turn：

```text
Request {
  request_id
  actor_id
  method
  interleave_policy
  expected_version
  continuation
}

ActorRuntime:
  ready_turns
  awaiting_turns
  state
  state_version
  in_flight_by_policy
```

调度器只在两个条件满足时运行一个 turn：

```text
policy 允许它与当前 pending request 交错；
并发上限没有超过。
```

mutating method 的写入要变成显式 commit：

```text
if request.expected_version != actor.state_version:
  reject / retry / reload
else:
  apply mutation
  actor.state_version++
```

如果要支持 call-chain reentrancy，scope 要带 token：

```text
token = {root_request_id, allowed_actor_id, expires_at, depth}
```

只有 token 匹配的回调能插队。这样比全局 `[Reentrant]` 安全得多。

LogServe 当前的设计更简单：没有 continuation turn，只有 actor command task；完成时检查 owner/epoch 和 command_seq。若要扩展 reentrancy，我会保留 `command_seq` 作为提交顺序边界，再引入 read-only fast path。写方法仍然按 `command_seq` 提交；只读方法可以基于某个 `state_version` 快速返回，或在 state 版本变化后重读。

面试里可以这样答：

```text
从零实现 reentrancy，我先定义 turn、interleave policy、state_version、commit point、read-only 约束、call-chain token、并发上限、epoch fencing 和 snapshot 边界。实现上只允许在 await/yield 点切换；写请求提交前必须做版本校验；snapshot 只抓已提交版本。默认策略仍然是 non-reentrant，按方法逐步开放。
```

## Q049. actor reentrancy 的常见误用是什么，误用后通常会产生什么线上症状？

**回答：**

第一种误用是把整个 actor 标成 reentrant。这样最快，但也是最危险的。线上症状是偶发状态错乱、订单状态倒退、计数不准、余额偶发为负，复现困难。

第二种误用是把写方法标成 read-only 或 always-interleave。短期看 p99 下降，长期会出现 lost update。尤其是方法“看起来只是读”，但顺手更新 last_access、cache、metrics、timer，就已经不是只读。

第三种误用是 await 前检查条件，await 后不重查。线上症状是超卖、重复扣款、重复审批、状态机越过非法边界。

第四种误用是在 reentrant actor 中做阻塞调用。Ray 文档明确提醒 async actor method 里阻塞 `ray.get` 或 `ray.wait` 会阻塞 event loop。线上表现是明明开了 async/reentrant，吞吐却掉得更厉害，所有 coroutine 卡住。

第五种误用是没有并发上限。大量请求挂起等待下游，actor 本地看似不忙，但数据库连接池、HTTP client、对象存储请求数被打爆。线上症状是下游 429/5xx 增加，actor 内存上涨。

第六种误用是没有版本号。没有 `state_version`，就无法判断 await 后看到的状态是否还是原来的状态。线上日志看每个请求都成功，最终状态却不满足不变量。

第七种误用是让 compensation 基于旧状态回滚。A await 后失败，把 actor state 恢复到 A 开始前；但 B 已经在中间成功提交。线上症状是别人的更新被回滚。

第八种误用是把 reentrancy 当成解决所有死锁的办法。它只能解决某些 actor call cycle，不能解决数据库锁、外部 API 阻塞、线程池耗尽，也不能自动保证业务幂等。

第九种误用是缺少交错日志。没有 request id、turn id、state version、await point，线上只能看到“偶尔错”。这个问题比普通并发 bug 更难查。

LogServe 现在可以在面试里把边界说清楚：它没有为了吞吐打开 reentrancy，所以不会声称 read-only interleaving 或 call-chain reentrancy。它的风险更多在队列积压和完整 StateJSON 成本，而不是 interleaving bug。这样讲比硬说“我们也支持 reentrant actor”更可信。

面试里可以这样答：

```text
reentrancy 最常见误用是全局打开、把写方法当只读、await 后不重查状态、没有并发上限、阻塞 event loop、没有版本号、错误回滚旧状态、用它掩盖调用环设计问题。线上症状是 p99 偶尔变好但状态偶发错乱、lost update、下游连接池爆、内存上涨、日志难复现。正确做法是默认关闭，只对可证明安全的方法或调用链开放。
```

## Q050. actor reentrancy 在单机和分布式环境中的语义有什么差异？

**回答：**

单机 reentrancy 主要是调度语义问题；分布式 reentrancy 还会变成消息、ownership、恢复和超时协议问题。

单机单线程 event loop 里，reentrancy 通常是协作式的。只有在 `await`、`yield` 或 future completion 时，另一个 request turn 才能运行。只要没有阻塞 event loop，所有代码仍然在一个线程里执行。这个模型没有传统数据竞争，但有中间状态交错。

单机线程池 actor 不一样。多个 actor method 可能真的在不同线程上跑，字段访问要加锁或用 immutable state。Ray threaded actor 就更接近这个方向。这里既有 reentrancy 的逻辑交错，也有真正并发读写风险。

分布式环境多出几层语义。

第一，call-chain reentrancy 要跨节点传播。A 调 B，B 回调 A，允许回调的 token 要跟着 RPC 走。token 的作用域、过期时间、最大深度都要定义，不然容易变成任意回调都能插队。

第二，超时和重试更复杂。A 以为 B 超时，客户端重试；原请求可能还在另一个节点继续跑。reentrant actor 如果允许两个相关请求交错，就必须靠 request id 和 idempotency 收敛。

第三，ownership 转移会影响 continuation。旧节点上挂起的 continuation 不能在新 owner 接管后提交状态。它必须带 activation id 或 epoch，提交时被 fencing。

第四，placement 变化会影响本地资源。一个 reentrant actor 可能有很多 pending await 指向本地连接、socket、模型对象。迁移后这些 continuation 不可恢复，除非它们被建模成持久 workflow step。

第五，分布式 tracing 更重要。单机看 event loop 顺序就够；分布式要把 actor id、request id、turn id、call-chain token、epoch、state version 串起来，否则很难解释一次交错。

LogServe 当前处在更保守的位置：单机多进程，非 reentrant actor，状态提交靠 control plane。它已经有 owner/epoch fencing，因此如果未来做分布式 reentrancy，可以沿用 epoch 作为提交边界。但现有实现还没有 call-chain token、continuation persistence、read-only interleaving 和 reentrant snapshot 语义。

面试里可以这样答：

```text
单机 reentrancy 多半是 event loop 或线程池调度问题；分布式 reentrancy 是协议问题。跨节点要传 call-chain token，处理超时重试、owner 迁移、旧 continuation 迟到提交、placement 变化和 tracing。LogServe 当前是非 reentrant、单机多进程的 actor runtime，owner/epoch fencing 能保护迟到完成，但还没有实现分布式 reentrant actor 所需的 continuation 和 call-chain 语义。
```

## Q051. actor snapshot 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

Actor snapshot 的核心目标，是在某个已知日志位置保存 actor 的完整逻辑状态，让恢复时不用从头 replay 所有事件或命令。它首先解决恢复性能和可用性问题，同时服务于正确性；但 snapshot 本身不应该取代事件日志或命令日志。

一个持久化 actor 通常有两类材料：

```text
event / command log:
  记录状态是怎么一步步变化的。

snapshot:
  记录某个 command_count / sequence_nr 时 actor 的状态长什么样。
```

没有 snapshot 时，恢复要这样：

```text
ActorCreated
apply event 1
apply event 2
...
apply event 1000000
```

有 snapshot 后，恢复变成：

```text
load snapshot at command_count = 990000
replay event 990001..1000000
```

Akka snapshot 文档把这个目标讲得很清楚：event sourced actor 可能积累很长的 event log，恢复时间会变长；snapshot 可以显著减少恢复时间。它还强调，恢复时使用最新 snapshot 初始化状态，然后 replay snapshot 之后的事件。

正确性来自 snapshot 和日志位置的绑定。Snapshot 不是随便缓存一份 state，它必须说明自己覆盖到哪个 sequence/command count。否则恢复时不知道 tail 从哪里开始，很容易漏应用或重复应用。

安全性不是 snapshot 的主要目标，但 snapshot 会影响安全。snapshot 里不能放外部资源句柄、临时 token、未加密敏感信息。它通常会长期存储，可能跨节点读取，所以要考虑加密、权限、校验和 schema evolution。

可维护性方面，snapshot 能减少恢复代码成本，但也引入新复杂度：snapshot schema、兼容性、清理策略、损坏回退、对象存储一致性、trim 边界。没有这些设计，snapshot 会变成另一个难排查的状态来源。

LogServe 的 snapshot 目标很明确：减少 actor replay 工作量。创建 actor 时 `SnapshotEvery` 默认是 25；每次 `ActorCommandApplied` 后，如果 `CommandCount % SnapshotEvery == 0`，控制面把完整 `StateJSON` 写入 result store，再写 `ActorSnapshotCreated` 事件，并记录 `SnapshotCommandCount`。集成测试里 100 次 `inc()` 后会检查 snapshot 存在，`ReplayActor` 也会比较 snapshot replay 和 full replay，要求 snapshot replay 的命令数更少。

面试里可以这样答：

```text
actor snapshot 的核心目标是降低恢复成本：把某个 command_count 或 sequence_nr 上的 actor state 保存下来，恢复时先加载 snapshot，再 replay tail log。它主要解决性能和可用性，同时要求正确性：snapshot 必须和日志位置绑定，不能漏重放或重复重放。LogServe 的 snapshot 保存完整 StateJSON 和 snapshot_ref，ActorSnapshotCreated 记录 snapshot_command_count，恢复时用 snapshot + tail events 对齐状态。
```

## Q052. actor snapshot 的典型适用场景和不适用场景分别是什么？

**回答：**

Snapshot 适合“事件很多、恢复慢、但状态本身可以安全序列化”的 actor。不适合把所有状态问题都塞进去。

典型适用场景有几类。

第一，长生命周期 event-sourced actor。比如账户、订单聚合、workflow instance、游戏房间、设备状态。事件一直增长，完全 replay 会越来越慢。定期 snapshot 可以把恢复时间控制住。

第二，passivation 和 reactivation。冷 actor 被释放后，下次消息来时要快速恢复。snapshot 越新，重新激活越快。

第三，owner 迁移。actor 从一个 worker 换到另一个 worker 时，新 owner 需要快速加载状态。snapshot 可以减少 tail replay，让迁移成本更稳定。

第四，状态计算昂贵。某些 actor 的状态是大量事件折叠后的结果，比如索引、聚合统计、规则引擎上下文。每次从头算一遍不现实。

第五，日志保留和压缩。snapshot 成功后，旧事件可以进入 logical trim 或物理清理流程。不过这必须非常小心：清理以后，snapshot 就成了恢复必需品。

不适用场景也很多。

第一，事件很少或恢复很快。snapshot 会带来额外写 I/O、序列化和存储成本，收益不大。

第二，状态不可稳定序列化。比如 socket、file handle、线程、GPU context、内存指针、Python 对象闭包。这些应该在 activation 时重建，snapshot 里只放 logical descriptor。

第三，snapshot 被拿来当审计日志。Snapshot 只告诉你某一刻状态是什么，不告诉你为什么变成这样。审计、溯源、合规通常仍然需要事件日志。

第四，状态巨大但每次只改一点。全量 snapshot 可能比 replay 更贵。应该考虑 delta snapshot、分片状态、大字段外置，或者拆 actor。

第五，强事务边界不在单 actor 内。Snapshot 只能恢复一个 actor 的状态，不能保证多个 actor 的 snapshot 同时一致。跨 actor 一致性要用事务、saga、barrier 或全局 checkpoint。

第六，schema 变化很频繁且缺少兼容策略。snapshot 往往比 event 更大、更难迁移。Akka 文档也提到，如果 snapshot 序列化格式不兼容，可以选择不使用 snapshot 恢复，改为从事件重放；但如果事件已经删除，这条路就断了。

LogServe 当前适合用 snapshot 证明机制：`Counter` actor 100 次 `inc()` 后 snapshot replay 明显少于 full replay。它还不适合声称已经解决了所有生产 snapshot 问题，因为 `docs/plan.md` 里已经指出大 result/snapshot 的内存 profile、streaming store、temp rename/fsync、物理 compaction 还需要加强。

面试里可以这样答：

```text
snapshot 适合长生命周期、事件很多、恢复慢、状态可序列化的 actor，也适合 passivation、迁移和日志保留。它不适合小状态短日志、不稳定运行时对象、审计需求、全量 state 巨大但变化很小、跨 actor 一致性，或者没有 schema evolution 策略的系统。LogServe 当前用完整 StateJSON snapshot 验证恢复优化，生产化还要补大 snapshot、流式存储和损坏回退。
```

## Q053. actor snapshot 和相近概念最容易混淆的边界在哪里？

**回答：**

Actor snapshot 最容易和 event log、checkpoint、cache、materialized view、backup、compaction 混在一起。

先看 snapshot 和 event log。Event log 是事实序列，记录发生了什么；snapshot 是某个位置折叠后的状态。Event log 是 source of truth，snapshot 是恢复加速器。删掉 snapshot，只要事件还在，通常还能从头恢复；删掉事件，只留 snapshot，就失去了完整历史。

第二，snapshot 和 checkpoint。很多场景里两者接近，但 checkpoint 往往更广，可以包括执行进度、外部 offset、operator state、workflow cursor。Actor snapshot 更强调某个 actor 的逻辑 state。如果 checkpoint 包含运行时栈、future、线程状态，那就已经不是普通 actor snapshot。

第三，snapshot 和 cache。Cache 可以过期、可以丢，丢了重算。Snapshot 一旦和日志 trim 绑定，就不能随便丢。LogServe 的 `snapshot_ref` 如果已经成为 trimmed actor stream 的恢复入口，就不是普通 cache。

第四，snapshot 和 materialized view。Materialized view 通常是为了查询，比如把 actor 当前状态投影到 metadata 表。Snapshot 是为了恢复，通常存对象存储或 snapshot store。LogServe README 明确说 actor source of truth 是 `actor:<actor_id>` shared log，metadata 是 materialized current view；snapshot 保存的是恢复用的 `StateJSON`，不是查询视图本身。

第五，snapshot 和 backup。Backup 是灾备维度，通常覆盖一批数据、一个时间点、一个存储系统。Actor snapshot 是应用语义维度，绑定 actor id 和 command count。Backup 可以备份 snapshot store，但两者不是一回事。

第六，snapshot 和 compaction。Compaction 是清理或压缩旧日志的动作；snapshot 是 compaction 的前提之一。Akka 文档提醒，事件删除会丢失历史，而且是否真正从存储删除取决于 journal 实现。LogServe 目前也是 logical trim：`ReadLog` 默认隐藏 trim point 前记录，但 segment 文件没有物理删除。这是 snapshot-aware retention，不是完整磁盘 compaction。

第七，snapshot 和数据库事务。写 snapshot 对象、写 `ActorSnapshotCreated` 事件、更新 metadata、trim stream 往往不是一个数据库事务。必须设计 crash window，而不是假设它们原子成功。

面试里可以这样答：

```text
snapshot 是某个 actor 在某个 log position 上的状态副本，用来加速恢复。它不是 event log，不是 cache，不是查询 view，也不是 backup。Event log 记录历史事实，materialized view 服务查询，cache 可以丢，backup 做灾备，compaction 清理旧数据。LogServe 里 shared log 是 source of truth，metadata 是当前视图，snapshot_ref 指向恢复状态，logical trim 只是隐藏旧记录，不等于物理删除。
```

## Q054. actor snapshot 在高并发场景下可能出现哪些隐藏问题？

**回答：**

高并发下，snapshot 的问题通常来自“状态还在变，但你要保存一个一致切片”。

第一个问题是 snapshot 捕获移动中的状态。actor 正在处理 command，snapshot 同时读取 state。如果没有单线程边界、copy-on-write 或 stashing，snapshot 可能保存半更新状态。Akka snapshot 文档里有一个重要细节：snapshot 被触发时，incoming commands 会被 stash，直到 snapshot 保存完成；这样状态在异步序列化和存储期间不会被新 event 更新。

第二个问题是 snapshot 阻塞业务。为了安全，snapshot 期间可能要暂停新 command。状态越大，序列化和写存储越慢，mailbox queue wait 越高。这个问题会在热点 actor 上放大。

第三个问题是 snapshot storm。很多 actor 同时达到 `snapshotEvery`，同时写对象存储或数据库。结果是 snapshot store 写延迟上升，actor command latency 也跟着上升。

第四个问题是全量复制内存峰值。actor state 本来 200 MB，snapshot 时再复制一份、序列化一份、压缩一份，瞬间可能占用几倍内存。`docs/plan.md` 里也提到大 result/snapshot 当前会整体进内存，LocalStore 缺少 temp rename 和 fsync，S3Store 缺少 streaming/multipart。

第五个问题是 trim race。snapshot 刚写完对象，还没写 `ActorSnapshotCreated`；或者事件写了，但 trim 先发生；或者 metadata 还没更新。恢复路径必须能处理这些中间状态。

第六个问题是读写竞争。恢复线程正在读 snapshot，清理线程删除旧 snapshot。如果 retention 没有引用计数或保留策略，可能删掉正在使用的 snapshot。

第七个问题是 stale snapshot 误用。高并发下最新 metadata 可能已经到 command 1200，但某个恢复流程读到 command 1000 的 snapshot。只要 tail log 完整还好；如果 tail log 被错误 trim，就会丢状态。

第八个问题是序列化版本和滚动发布。部分 worker 写新 schema snapshot，部分 worker 还只认识旧 schema。高并发滚动升级时，这类问题很常见。

LogServe 当前通过 actor command 串行应用降低了 snapshot 一致性难度。`createActorSnapshot` 接收的是 already updated actor state，把完整 `StateJSON` 写 result store，再写 `ActorSnapshotCreated`，再 logical trim。风险点也清楚：snapshot 写对象和写日志不是同一原子事务，大 state 会带来内存和 I/O 压力，logical trim 还不是物理 compaction。

面试里可以这样答：

```text
高并发下 snapshot 的难点是保存一致切片。常见问题包括 snapshot 捕获半更新状态、snapshot 阻塞业务、snapshot storm、全量复制导致内存峰值、trim race、清理线程删掉仍需使用的 snapshot、旧 snapshot 和 tail log 不匹配、schema 滚动升级失败。可靠系统要绑定 command_count/log position，snapshot 成功前不能 trim 依赖事件，并控制 snapshot 并发和大小。
```

## Q055. actor snapshot 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

Snapshot 的故障边界要按写入步骤拆开看。LogServe 的顺序大致是：

```text
1. ActorCommandApplied 已经提交，actor state 到 command_count = N
2. resultStore.Put 写入 StateJSON，得到 snapshot_ref
3. append ActorSnapshotCreated(actor_id, snapshot_ref, snapshot_command_count=N)
4. TrimStream(before snapshot event seq)
5. metadata 更新 SnapshotRef / SnapshotCommandCount
```

每一步崩溃，语义都不同。

第一，`resultStore.Put` 成功，但 `ActorSnapshotCreated` 没写。对象存储里会有一个 orphan snapshot。恢复时不能凭对象存在就使用，因为没有日志事件证明它对应哪个 command_count。清理任务可以后续删除孤儿对象。

第二，`ActorSnapshotCreated` 写成功，但 metadata 没更新。恢复仍然应该能从 log 找到 snapshot event 并加载 snapshot；metadata 只是 current view，不能成为唯一来源。LogServe 的 `Replay` reducer 会扫描 `ActorSnapshotCreated`，这条路是对的。

第三，`ActorSnapshotCreated` 写成功，TrimStream 失败。正确结果是恢复仍然没问题，只是保留更多旧日志。LogServe 当前 trim 失败只打 error log，不让 snapshot 创建整体失败，这属于偏可用性的选择。

第四，TrimStream 成功，但 snapshot 对象后来损坏或丢失。如果旧事件只是 logical trim 还在底层，可能还能 fallback；如果物理删除了旧事件，而 snapshot 又不可读，就无法恢复正确状态。Akka 文档也提醒：如果 snapshot optional，但事件已经删除，snapshot load 失败后从事件恢复会得到错误状态。

第五，snapshot 写超时。超时不一定表示没写成功。重试时要用幂等 snapshot key，比如 `actor_id:snapshot:command_count`。LogServe 的 `ActorSnapshotCreated` idempotency key 就是 `actorID:snapshot:commandCount`。

第六，重复 snapshot。两个 worker 或两次 retry 都试图为 command_count=N 创建 snapshot。只要 snapshot event 幂等，最终应该只有一个有效 `ActorSnapshotCreated`，或者多个等价对象但只有一个日志位置被承认。

第七，重启后加载 snapshot schema 失败。选择有两个：禁用 snapshot，回放完整事件；或者停止 actor，等待人工迁移。前者要求旧事件还在；如果旧事件已删，只能迁移 snapshot 或从备份恢复。

第八，snapshot 对应的 state 和 tail log 不一致。比如 snapshot_command_count 写错，恢复会跳过不该跳过的事件，或者重复应用旧事件。测试必须覆盖这个问题。

第九，actor reentrancy 下有 pending continuation。snapshot 只能覆盖 committed state，不能试图恢复内存 continuation。未完成外部请求要靠 durable command/outbox 重建。

面试里可以这样答：

```text
snapshot 的故障边界包括：对象写成功但 snapshot event 没写形成 orphan；event 写成功但 metadata 没更新；trim 失败只影响保留成本；trim 成功后 snapshot 丢失会影响恢复；写超时后重试必须幂等；schema 不兼容要么 full replay，要么迁移；snapshot_command_count 错会导致漏放或重复 replay。核心原则是：只有日志中有 ActorSnapshotCreated，并且 snapshot_ref 可读、command_count 对齐，snapshot 才能作为恢复入口。
```

## Q056. actor snapshot 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

Snapshot 的瓶颈几乎可能来自所有层，但最常见的是内存和 I/O。

CPU 瓶颈来自序列化、压缩、校验和、加密、schema 转换。JSON state 尤其明显：状态越大，marshal/unmarshal 越贵。恢复时还要反序列化 snapshot，再 replay tail log。

内存瓶颈来自全量 state copy。很多实现为了得到一致 snapshot，会复制 state，再序列化成 bytes，然后交给存储层。一次 snapshot 可能同时存在：

```text
actor in-memory state
snapshot copy
serialized bytes
compressed/encrypted bytes
storage client buffer
```

这就是为什么大 snapshot 会导致 GC 和 OOM 风险。LogServe 当前 `resultStore.Put(ctx, namespace, data []byte)` 接收完整 bytes，`docs/plan.md` 已经指出大 result/snapshot 会整体进内存，后续应做 streaming/multipart。

锁竞争来自 snapshot 一致性。为了避免 state 在 snapshot 中途变化，actor 可能 stash 新命令、持有锁、暂停写请求。状态越大，暂停时间越长。Akka 的处理是 snapshot 触发时 stash incoming commands，保存完成后再继续，这能保正确性，但会增加等待时间。

I/O 瓶颈来自 snapshot store。写本地磁盘要考虑 temp file、rename、fsync；写 S3/MinIO 要考虑网络、multipart、重试、吞吐限制；写数据库要考虑事务日志和行大小。读取 snapshot 时，冷数据从对象存储拉回来，也会影响恢复时间。

网络瓶颈出现在分布式部署。snapshot 可能从 actor 所在节点传到远端对象存储，恢复时又从对象存储传到新 owner。状态越大，迁移和恢复越慢。

还有一个容易忽略的瓶颈是 tail replay。snapshot 太旧时，读取 snapshot 很快，但 tail log 很长，恢复仍然慢。snapshot 太频繁时，正常路径变慢。频率要根据恢复 SLO 和写入成本一起定。

性能指标应该拆开：

```text
snapshot_serialize_ms
snapshot_write_ms
snapshot_bytes
snapshot_copy_bytes
snapshot_stall_ms
snapshot_failure_count
snapshot_read_ms
tail_replay_commands
recovery_total_ms
```

LogServe 的 README 里有实验数字：actor snapshot replay commands 是 1，对比 full replay 21。这个数字说明 snapshot 能减少 replay work，但不能说明大 state 下的 snapshot 写入已经生产可用。面试时要把这两个结论分开。

面试里可以这样答：

```text
snapshot 的瓶颈通常先是内存和 I/O，其次是 CPU、锁和网络。全量 state 会复制、序列化、压缩、写对象存储；为了一致性还可能暂停新 command。分布式场景下 snapshot 读写跨网络，恢复时还要 replay tail log。LogServe 当前保存完整 StateJSON，适合机制验证；如果 state 变大，需要 streaming store、delta snapshot、大字段外置和 snapshot 并发限流。
```

## Q057. actor snapshot 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

Snapshot 测试要证明两件事：恢复出来的状态对，恢复成本确实下降。前者是 correctness，后者是 benchmark。不要混成一个测试。

Correctness test 要覆盖：

```text
full replay == snapshot replay:
  同一组事件，全量 replay 和 snapshot + tail replay 得到同一 StateJSON。

snapshot boundary:
  snapshot_command_count=N 时，恢复只跳过 <=N 的 applied event，必须 replay N+1 之后。

latest snapshot:
  多个 snapshot 存在时，选择最新可用 snapshot。

missing snapshot:
  snapshot_ref 不存在时，策略是 full replay、停止 actor，还是报恢复失败。

corrupt snapshot:
  反序列化失败、checksum 不匹配、schema 不兼容时走预期路径。

trimmed log:
  旧事件被隐藏或删除后，snapshot 必须足以恢复。

duplicate snapshot:
  同一个 command_count 重复 snapshot，不产生两个互相冲突的恢复入口。

metadata stale:
  metadata 没更新时，replay 仍能从 log 发现 snapshot。
```

LogServe 已经有一个很好的基础测试：100 次 `inc()` 后检查 `SnapshotRef` 和 `SnapshotCommandCount`，然后 worker 切换，`get()` 仍然是 100；`ReplayActor` 要求 snapshot replay commands 少于 full replay commands；trim 后 `ActorCreated` 被隐藏，tail 里只保留必要的 submitted/applied 和 snapshot event。

Stress test 要加故障注入：

```text
crash after Put before ActorSnapshotCreated
crash after ActorSnapshotCreated before metadata update
trim failure
snapshot store temporary failure
snapshot store slow write
large StateJSON
many actors snapshot at same time
schema rolling upgrade
snapshot cleanup racing with recovery
```

Benchmark 要单独测：

```text
snapshot write latency by state size
snapshot read latency by state size
full replay time by event count
snapshot replay time by tail length
memory peak during snapshot
actor command latency during snapshot
object store throughput
trim cost
recovery p50/p99
```

一个容易犯的错是只测 “snapshot replay 比 full replay 快”，但不测 snapshot 写入对正常请求的影响。真正的成本在两边：写 snapshot 会拖慢正常路径，读 snapshot 会加速恢复路径。要看系统的恢复 SLO 和正常请求 SLO 哪个更紧。

面试里可以这样答：

```text
correctness test 证明 snapshot + tail replay 和 full replay 一致，覆盖边界 command_count、最新 snapshot、缺失/损坏 snapshot、trimmed log、重复 snapshot 和 stale metadata。stress test 做 snapshot 写入各阶段 crash、对象存储失败、大 state、snapshot storm、schema 升级和清理竞争。benchmark 分别测 snapshot 写入成本、恢复收益、内存峰值、tail replay 长度和正常请求延迟。LogServe 现有测试已经覆盖 snapshot replay 减少工作量，后续应补故障注入和大 snapshot profile。
```

## Q058. 如果要求从零实现一个简化版 actor snapshot，你会先定义哪些不变量？

**回答：**

从零实现 snapshot，我会先定义恢复不变量，而不是先决定文件格式。

最核心的不变量是：

```text
position invariant:
  每个 snapshot 必须绑定 actor_id 和 applied command_count / log sequence。

committed-state invariant:
  snapshot 只能来自已提交 actor state，不能包含未完成 command 的局部变量。

durability invariant:
  snapshot_ref 被写入日志前，snapshot 对象必须已经 durable。

discoverability invariant:
  只有日志里的 SnapshotCreated 事件能让恢复流程发现 snapshot。

tail invariant:
  恢复从 snapshot_command_count + 1 开始 replay tail events。

trim invariant:
  旧事件只有在 snapshot 可读、SnapshotCreated 已提交后才能被 trim。

idempotency invariant:
  同一个 actor_id + command_count 重复创建 snapshot，结果必须等价。

schema invariant:
  snapshot 带 schema_version，并有兼容、迁移或 fallback 策略。

integrity invariant:
  snapshot 有 checksum/size/content_type，读取时校验。

retention invariant:
  至少保留一个可恢复 snapshot，清理不能删除正在使用的 snapshot。
```

一个最小实现可以这样：

```text
on command applied:
  state.command_count = N
  if N % snapshot_every == 0:
    bytes = serialize(state)
    ref = store.put(actor_id, N, bytes, checksum)
    append SnapshotCreated(actor_id, N, ref, schema_version, checksum)
    update metadata snapshot_ref=N/ref
    trim log before snapshot event if policy allows
```

恢复流程：

```text
records = read actor log
snapshot_event = latest SnapshotCreated that passes policy
if snapshot_event exists:
  bytes = store.get(snapshot_event.ref)
  verify checksum/schema
  state = deserialize(bytes)
  replay events with command_count > snapshot_event.N
else:
  replay from ActorCreated
```

要提前决定 snapshot load 失败怎么办。Akka 文档里给了一个清晰边界：可以把 snapshot loading 配成 optional，失败后 full replay；但如果旧事件已经删除，这样会恢复出错误状态。所以从零实现时我会把策略写死得保守一点：

```text
if old events still available:
  snapshot load failure -> full replay + alert
else:
  snapshot load failure -> stop actor + alert
```

还要决定 snapshot 是否阻塞新命令。简化实现可以先让 actor 在 snapshot 期间暂停写 command，或者复制 immutable state 后异步写。前者简单但影响延迟；后者性能好，但要求 state copy 是一致的。

LogServe 当前实现已经具备这些不变量的一部分：snapshot 绑定 `SnapshotCommandCount`，snapshot object 先写 result store，再写 `ActorSnapshotCreated`；replay reducer 用 snapshot_ref 加载 `StateJSON`，并跳过 snapshot command count 之前的 applied/failed event；`ActorSnapshotCreated` 用 `actorID:snapshot:commandCount` 做幂等 key。还可以补 checksum、schema_version、snapshot load fallback、orphan cleanup、streaming write 和物理 compaction。

面试里可以这样答：

```text
从零实现 actor snapshot，我会先定义 position、committed state、durability、discoverability、tail replay、trim、idempotency、schema、checksum 和 retention 不变量。snapshot 必须绑定 actor_id 和 command_count；对象 durable 后才能写 SnapshotCreated；恢复只能使用日志承认的 snapshot_ref；tail 从 command_count+1 replay；旧事件不能早于可恢复 snapshot 被删除。LogServe 当前已有 snapshot_ref、SnapshotCommandCount 和 idempotent SnapshotCreated，生产化还要补 checksum、schema version、fallback 和流式存储。
```

## 参考资料

- Akka documentation: How the Actor Model Meets the Needs of Modern, Distributed Systems https://doc.akka.io/libraries/akka-core/current/typed/guide/actors-intro.html
- Akka documentation: Typed actor mailboxes, default unbounded mailbox and bounded mailbox https://doc.akka.io/libraries/akka-core/current/typed/mailboxes.html
- Akka documentation: Persistence, Event Sourcing and snapshot recovery https://doc.akka.io/libraries/akka-core/current/typed/persistence.html
- Akka documentation: Snapshotting, snapshot failures, event deletion and retention https://doc.akka.io/libraries/akka-core/current/typed/persistence-snapshot.html
- Akka documentation: Dispatchers and managing blocking operations https://doc.akka.io/libraries/akka-core/current/typed/dispatchers.html
- Akka documentation: Cluster Sharding, shard allocation, passivation and leases https://doc.akka.io/libraries/akka-core/current/typed/cluster-sharding.html
- Akka documentation: Split Brain Resolver and Cluster Sharding safety https://doc.akka.io/libraries/akka-core/current/split-brain-resolver.html
- Erlang System Documentation: Processes, signals and message queue ordering https://www.erlang.org/doc/system/ref_man_processes.html
- Microsoft Learn: Orleans overview and virtual actor model https://learn.microsoft.com/en-us/dotnet/orleans/overview
- Microsoft Learn: Orleans grain placement strategies https://learn.microsoft.com/en-us/dotnet/orleans/grains/grain-placement
- Microsoft Learn: Orleans activation collection and idle grain deactivation https://learn.microsoft.com/en-us/dotnet/orleans/host/configuration-guide/activation-collection
- Microsoft Learn: Orleans request scheduling and reentrancy https://learn.microsoft.com/en-us/dotnet/orleans/grains/request-scheduling
- Microsoft Learn: Orleans grain persistence and storage provider semantics https://learn.microsoft.com/en-us/dotnet/orleans/grains/grain-persistence/
- Microsoft Learn: Orleans transactions https://learn.microsoft.com/en-us/dotnet/orleans/grains/transactions
- Ray documentation: Actors, stateful workers and serial execution per actor https://docs.ray.io/en/latest/ray-core/actors.html
- Ray documentation: AsyncIO and concurrency for actors https://docs.ray.io/en/latest/ray-core/actors/async_api.html
- Ray documentation: Limiting actor concurrency per method with concurrency groups https://docs.ray.io/en/latest/ray-core/actors/concurrency_group_api.html
- Ray documentation: Actor fault tolerance, restarts, retries and checkpointing https://docs.ray.io/en/latest/ray-core/fault_tolerance/actors.html
- Go language specification: channel types, buffered/unbuffered channels and FIFO behavior https://go.dev/ref/spec#Channel_types
- Effective Go: share memory by communicating and CSP background https://go.dev/doc/effective_go#channels
- LogServe README: Actor Semantics, actor stream, command sequence, ownership, snapshot and logical trim ../README.md
- LogServe source: control-plane actor runtime ../internal/control/actor.go
- LogServe source: actor replay reducer ../internal/actor/model.go
- LogServe tests: actor recovery, mailbox serialization, snapshot replay and epoch fencing ../tests/integration/actor_runtime_test.go
- LogServe tests: stale completion rejection and command sequence gating ../internal/control/actor_test.go
