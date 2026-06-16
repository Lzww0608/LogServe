# 7. sequence number、lease、epoch、fencing 与逻辑时间

这一组问题讨论的是分布式系统里三个经常被混在一起的问题：

```text
顺序问题：这两个事件、请求、日志、版本，谁在谁前面？
资格问题：这个客户端、leader、writer，现在还有没有资格继续写？
时间问题：系统能不能只靠物理时钟判断先后、有效期和冲突？
```

`sequence number` 主要处理顺序、去重、补洞和重放位置；`lease` 主要处理带时间边界的资格；`epoch` 和 `fencing token` 主要处理旧持有者、旧 leader、旧 writer 继续写入的问题；Lamport clock、vector clock、HLC 则是在物理时钟不可靠或不够精确时，用逻辑时间表达 happens-before、并发关系和接近真实时间的排序。

面试时要先把边界说清楚：单调递增不等于真实时间顺序，lease 过期不等于旧持有者已经停下，lock 不等于 lease，logical clock 也不是物理时钟的简单替代品。

## Q001. sequence number 通常用于解决什么问题？

**回答：**

`sequence number` 通常用于把“发生过的一系列动作”放进一个可比较、可恢复、可校验的顺序里。它不是为了表示真实时间，而是为了给系统提供一个在某个作用域内单调推进的逻辑位置。

常见用途有几类。

1. **排序**

   如果一个 stream、partition、replica log 或客户端会话里有很多事件，sequence number 可以告诉系统：

   ```text
   seq = 10 的事件在 seq = 11 之前
   seq = 11 已经处理后，seq = 10 不应该再被当成新事件处理
   ```

   典型例子包括数据库 LSN、WAL offset、Kafka partition offset、Raft log index、etcd store revision、ZooKeeper zxid、消息队列的 per-partition position。

2. **去重和幂等**

   如果服务端知道某个 producer、client 或 session 的最新 sequence number，就可以识别重复请求：

   ```text
   last_seen_seq[client=A] = 100

   收到 seq=101：正常处理
   收到 seq=101：重复，返回已有结果或忽略
   收到 seq=99 ：旧请求，拒绝或忽略
   收到 seq=103：发现缺 seq=102，需要补洞或触发错误
   ```

   这里 sequence number 的价值不是“数字变大”，而是它和作用域绑定后能表达“这个客户端在这条请求链上已经推进到哪里”。

3. **gap detection**

   sequence number 可以发现丢消息、乱序提交、日志截断、复制落后：

   ```text
   expected = 42
   received = 44
   ```

   这不一定说明 `43` 永久丢了，也可能只是网络乱序、批处理拆分、并发提交还没到。但系统至少有机会发现“当前位置不连续”，而不是悄悄把状态推进到错误位置。

4. **重放、恢复和断点续传**

   日志系统、CDC、消息队列和复制协议通常都需要问：

   ```text
   我已经消费到哪里？
   崩溃前最后 durable 的位置在哪里？
   follower 应该从哪个位置继续拉？
   snapshot 覆盖到哪个 sequence？
   ```

   这时 sequence number 是恢复边界。没有它，系统只能靠扫描内容、比较时间戳或猜测状态，很难做到快速恢复。

5. **并发控制和条件更新**

   在 KV、配置中心、元数据服务里，sequence number 经常以 version、revision、generation 的形式出现。客户端读取版本 `v=10` 后更新时附带条件：

   ```text
   update key=x if version == 10
   ```

   如果实际版本已经变成 `11`，说明有人先改了，当前更新不能盲目覆盖。这就是乐观并发控制的基础。

6. **fencing**

   在 leader election、lease、分布式锁里，单靠“我拿到锁了”不够。旧 leader 可能因为 GC pause、网络分区、超时误判而继续工作。系统通常会给每次授权分配递增 token：

   ```text
   grant #7  -> writer A
   grant #8  -> writer B
   ```

   下游存储只接受 token 更大的写入，拒绝 token #7 的旧写入。这种 sequence number 就是 fencing token 或 epoch token。

**关键边界**

sequence number 必须说清楚作用域。没有作用域的 sequence number 很容易被误用。

```text
全局 sequence：整个系统共享一条顺序
per-stream sequence：每条 stream 内有顺序，stream 之间没有全局顺序
per-producer sequence：同一个 producer 内有顺序，不同 producer 之间没有顺序
per-key version：同一个 key 内有版本顺序，不同 key 之间不一定可比
```

还要说清楚它的语义是“分配顺序”“追加顺序”“提交顺序”“可见顺序”还是“应用顺序”。这些顺序在单机里可能刚好一致，在分布式系统里经常不一致。

一句话：sequence number 解决的是作用域内的逻辑位置问题，常用来做排序、去重、补洞、恢复、条件更新和 fencing；它不是物理时间，也不自动代表真实世界里的先后。

## Q002. 全局 sequence 和 per-stream sequence 的差异是什么？

**回答：**

全局 sequence 和 per-stream sequence 的核心差异是：它们定义的“顺序范围”不同。

全局 sequence 给整个系统一条总顺序：

```text
global_seq = 1001  -> tenant=A, stream=s1, event=x
global_seq = 1002  -> tenant=B, stream=s9, event=y
global_seq = 1003  -> tenant=A, stream=s2, event=z
```

只要比较 `global_seq`，就能得到所有事件之间的先后。per-stream sequence 只保证某条 stream 内部有顺序：

```text
stream=s1: seq=1, seq=2, seq=3
stream=s2: seq=1, seq=2, seq=3
```

`s1:seq=3` 和 `s2:seq=2` 谁先谁后，单看 per-stream sequence 没有答案。

**全局 sequence 的优点**

1. **语义简单**

   所有事件都能比较，审计、回放、全局 CDC、跨 key 串行化会更直接。

2. **恢复边界清晰**

   消费者可以说“我已经处理到 global_seq=123456”，不用为每个 stream 保存一个位置。

3. **适合全局提交日志**

   如果系统本来就是单 Raft group、单主库 binlog、单 WAL、单元数据日志，全局 sequence 和底层复制顺序天然一致。

**全局 sequence 的代价**

1. **分配点容易成为瓶颈**

   所有写入都要经过一个全局递增器、单 leader、中心化 sequencer 或强一致协议。吞吐、延迟、可用性都会受到影响。

2. **跨地域成本高**

   如果全局 sequence 要和提交顺序一致，跨 region 写入往往需要协调。全局顺序越强，系统越难横向扩展。

3. **容易过度承诺**

   很多业务只需要 per-user、per-account、per-partition 顺序，却引入了全局序列，最后把无关流量也绑到同一个热点上。

**per-stream sequence 的优点**

1. **可扩展**

   每个 stream、partition、tenant、shard 自己递增，不同流之间互不阻塞。

2. **贴近业务边界**

   用户订单、设备上报、topic partition、文件 append log 通常只要求单流内有序。跨流事件本来就是并发的，强行排全局顺序反而制造假因果。

3. **恢复状态更细**

   消费者可以分别记录：

   ```text
   stream A -> seq 100
   stream B -> seq 80
   stream C -> seq 900
   ```

   某条 stream 落后，不影响其他 stream 继续推进。

**per-stream sequence 的代价**

1. **跨 stream 合并需要额外规则**

   如果要把多条 stream 合成一个视图，必须有 tie-breaker 或额外时间信息：

   ```text
   (stream_id, seq)
   (physical_time, stream_id, seq)
   (hlc, stream_id, seq)
   (global_commit_seq, stream_id, seq)
   ```

   不能拿 `s1:seq=10` 和 `s2:seq=8` 直接比较。

2. **消费进度更复杂**

   消费者要保存 vector-like 的 position map。stream 数量很多时，checkpoint、压缩、迁移都更复杂。

3. **全局一致性难表达**

   如果业务要求“所有转账操作按一个全局顺序生效”，per-stream sequence 本身不够，还要事务协议、全局提交时间戳或统一日志。

**面试里可以这样比较**

| 维度 | 全局 sequence | per-stream sequence |
| --- | --- | --- |
| 顺序范围 | 所有事件可比 | 同一 stream 内可比 |
| 扩展性 | 较差，容易有中心热点 | 较好，可按 stream/shard 拆分 |
| 恢复位置 | 一个全局 offset | 每个 stream 一个 offset |
| 跨流查询 | 简单 | 需要合并规则 |
| 适用场景 | 单全局日志、审计、强顺序复制 | Kafka partition、用户事件流、分片日志 |
| 常见风险 | sequencer 成为瓶颈 | 误以为不同 stream 的 seq 可比较 |

**工程判断**

如果业务只要求“每个用户自己的事件顺序正确”，就不要上全局 sequence。全局 sequence 是强工具，代价也强。只有当系统确实需要全局可重放顺序、全局审计顺序、全局事务提交顺序时，才值得引入。

一句话：全局 sequence 提供系统级总顺序，但牺牲扩展性；per-stream sequence 只提供流内顺序，但更贴近分片和高并发系统。

## Q003. 单调递增是否等价于严格时间顺序？

**回答：**

不等价。单调递增只说明“某个分配器、某个 stream、某个逻辑时钟的值往前走”，不说明它严格等于真实世界里的发生时间。

先看几个反例。

**反例一：并发事件被人为排了顺序**

两个请求几乎同时到达不同节点：

```text
node A 收到 request x
node B 收到 request y
```

如果它们进入同一个 sequencer，sequencer 可能给 `x` 分配 `seq=10`，给 `y` 分配 `seq=11`。这只能说明在 sequencer 的分配顺序里 `x < y`，不能说明真实世界里用户一定先发起 x，再发起 y。

**反例二：分配顺序不等于提交顺序**

某些系统会先预分配 sequence，再执行事务：

```text
T1 分到 seq=100，执行很慢
T2 分到 seq=101，很快提交
```

如果系统没有规定“必须按 sequence 提交并可见”，那么 `seq=101` 可能先对外可见。此时单调分配不等于提交时间顺序。

**反例三：不同作用域的 sequence 不能比较**

```text
partition-0 offset=100
partition-1 offset=20
```

`100 > 20` 只是在各自 partition 内成立，不能推出 partition-0 的消息比 partition-1 的消息晚。

**反例四：Lamport clock 的单调性不是物理时间**

Lamport clock 保证的是：

```text
如果 a happens-before b，那么 C(a) < C(b)
```

但反过来不成立。`C(a) < C(b)` 只能说明逻辑时间较小，不能推出 `a happens-before b`，更不能推出 `a` 在物理时间上严格早于 `b`。

**严格时间顺序到底是什么**

面试里最好先定义：

```text
如果操作 A 的响应已经返回给客户端，
然后客户端才发起操作 B，
那么系统对外观察到的顺序必须是 A 在 B 之前。
```

这接近线性一致性里的 real-time order 要求。要满足这个要求，单调递增的数字还不够，系统还需要明确：

1. sequence 在什么时候分配；
2. sequence 是否持久化；
3. sequence 是否等同于 commit order；
4. commit 是否按 sequence 对外可见；
5. 跨节点、跨分片是否有统一仲裁；
6. 失败重试是否可能复用、跳过或回滚 sequence。

**什么时候单调递增可以代表某种顺序**

如果系统明确定义：

```text
所有写入都经过同一个 leader；
leader 在提交点分配 sequence；
sequence 和日志追加顺序一致；
只有提交后的 sequence 才对外可见；
读取也服从这个提交顺序。
```

那么 sequence 可以代表这个系统定义下的提交顺序。注意这仍然是系统内的逻辑提交顺序，不是普通物理时间戳。

一句话：单调递增是构造顺序的必要材料之一，但它本身不等价于严格时间顺序；只有当系统把 sequence 的分配点、提交点、可见性和作用域都定义清楚时，它才有明确的排序语义。

## Q004. 物理时间戳为什么不能完全替代 sequence number？

**回答：**

物理时间戳不能完全替代 sequence number，因为物理时钟回答的是“本机认为现在几点”，sequence number 回答的是“在这个系统定义的顺序里我处于哪个位置”。这两个问题不一样。

**物理时间戳的问题**

1. **时钟会偏移**

   不同机器的 clock 会有 skew。机器 A 的 `10:00:00.010` 不一定真的早于机器 B 的 `10:00:00.005`。NTP 可以减少偏差，但不能让所有机器拥有完美同步的物理时钟。

2. **时钟可能回拨或跳变**

   NTP 校时、虚拟机迁移、闰秒、手工改时间、宿主机问题，都可能让 wall clock 发生跳变。依赖 `now()` 做严格顺序，遇到回拨就会出错。

3. **时间戳分辨率有限**

   高并发写入里，多个事件可能落在同一个毫秒、微秒甚至纳秒时间戳上。时间戳相同后仍然需要 tie-breaker。

4. **时间戳不能表达缺口**

   sequence number 可以发现：

   ```text
   expected seq=10, received seq=12
   ```

   物理时间戳很难告诉你“中间少了一条事件”。`10:00:00.001` 到 `10:00:00.003` 之间没看到事件，可能是没有事件，也可能是丢了事件。

5. **时间戳不能自然表达消费位置**

   消费者记录 `last_seq=1000` 很清楚：下次从 `1001` 开始。记录 `last_time=10:00:00.001` 则会遇到同时间戳多事件、迟到事件、时钟偏差、分页重复/漏读等问题。

6. **时间戳不能直接做 fencing**

   旧 leader 拿着一个较大的本地时间戳继续写，不代表它还有资格。fencing 需要由授权方分配的递增 token，并且下游资源按 token 拒绝旧写入。

**物理时间戳适合什么**

物理时间戳仍然很重要，只是它适合的问题不同：

```text
日志展示
审计可读时间
TTL / retention
业务发生时间
延迟统计
按时间范围查询
近似排序
```

在日志系统里，通常会同时保存：

```text
offset / sequence: 用于精确读取、恢复、去重、复制
timestamp        : 用于时间查询、保留策略、观测和人类理解
```

**Spanner 和 HLC 给出的启发**

Google Spanner 没有简单地说“我们有物理时间，所以直接用时间戳就行”。它引入 TrueTime API，把时钟不确定性暴露出来，并通过协议等待来支持外部一致性。这恰好说明：要把物理时间用于强一致排序，必须处理 clock uncertainty，而不是把 `now()` 当成绝对事实。

HLC 的动机也类似。它把物理时间和逻辑计数结合起来：物理部分让时间戳接近真实时间，逻辑部分在消息传播、同一时间戳、时钟滞后时维持因果单调性。

**工程上的常见组合**

```text
sequence / offset / revision:
  精确顺序、恢复位置、gap detection、幂等、fencing

physical timestamp:
  业务时间、展示时间、过期时间、监控分析、时间范围过滤

logical clock / HLC:
  分布式因果顺序、近似真实时间排序、MVCC timestamp、跨节点快照
```

一句话：物理时间戳可以辅助排序和查询，但不能可靠替代 sequence number 的精确位置、补洞、重放、去重和 fencing 语义。

## Q005. Lamport clock 解决什么问题？

**回答：**

Lamport clock 解决的是：在没有全局同步物理时钟的分布式系统中，怎样给事件分配一个逻辑时间，使它尊重消息传递带来的 happens-before 关系。

Lamport 的出发点是，分布式系统里并不是任意两个事件都能说清楚谁先发生。两个节点之间没有消息往来时，它们的本地事件可能是并发的。系统真正能观察到的因果关系主要来自三条规则：

```text
同一个进程内，前一个事件 happens-before 后一个事件；
发送消息 happens-before 接收这条消息；
happens-before 具有传递性。
```

Lamport clock 用一个整数计数器实现这个关系。

**基本规则**

每个进程维护一个本地逻辑时钟 `C`：

```text
本地事件:
  C = C + 1
  event.timestamp = C

发送消息:
  C = C + 1
  message.timestamp = C

接收消息:
  C = max(local_C, message.timestamp) + 1
  receive_event.timestamp = C
```

这样可以保证：

```text
如果 a happens-before b，那么 C(a) < C(b)
```

这就是 Lamport clock 的核心价值。

**它解决了什么**

1. **不依赖物理时钟也能得到因果一致的逻辑时间**

   发送事件的时间戳一定小于接收事件。一个消息影响到后续事件后，这个影响会通过 `max(local, received)+1` 传播下去。

2. **可以构造全序**

   Lamport clock 本身可能相同，所以通常加上进程 ID 做 tie-breaker：

   ```text
   (lamport_time, node_id)
   ```

   这样可以给所有事件排出一个确定全序。这个全序常用于分布式互斥、日志合并、调试展示、确定性回放。

3. **能避免把物理时间当成因果**

   即使物理时间显示 `event x` 比 `event y` 早，如果两者没有消息链路，就不能说 x causally before y。Lamport clock 强迫系统只用可观察通信关系建立逻辑顺序。

**它不能解决什么**

最容易答错的是把 Lamport clock 说成“能判断两个事件有没有因果关系”。它不能完整判断。

Lamport clock 只保证单向蕴含：

```text
a happens-before b  =>  C(a) < C(b)
```

反过来不成立：

```text
C(a) < C(b)  不一定说明 a happens-before b
```

两个并发事件也可能被分到不同 Lamport timestamp。加上 node_id 后可以排全序，但这个全序是人为补出来的，不代表真实因果。

**面试例子**

```text
P1: a(send m) ----------------
                         \
P2:                      b(receive m) -> c
```

因为 `a` 是消息发送，`b` 是同一消息接收，所以 `a happens-before b`，Lamport clock 会保证 `C(a) < C(b)`。又因为 `b` 在同一进程内早于 `c`，所以 `C(b) < C(c)`，最终 `C(a) < C(c)`。

但如果另一个节点 P3 上有事件 `d`，它和 P1/P2 没有任何消息关系，那么 `C(d)` 可能小于、等于或大于 `C(c)`，这不表示真实因果。

一句话：Lamport clock 用逻辑计数器捕捉 happens-before 的必要顺序，适合建立因果一致的逻辑时间和确定全序；它不能从时间戳大小反推出完整因果，也不能判断所有并发关系。

## Q006. vector clock 相比 Lamport clock 能表达什么更多信息？

**回答：**

vector clock 比 Lamport clock 多表达的是：它可以区分“谁因果先于谁”和“两个事件是否并发”。Lamport clock 只能给出一个标量，vector clock 保存的是每个进程各自推进到哪里。

假设系统有三个进程 `P1, P2, P3`，每个事件携带一个三元组：

```text
VC = [time_of_P1, time_of_P2, time_of_P3]
```

每个进程发生本地事件时增加自己的分量；发送消息时带上整个 vector；接收消息时对每个分量取 max，再增加自己的分量。

**比较规则**

对两个 vector clock `A` 和 `B`：

```text
A <= B:
  A 的每个分量都 <= B 的对应分量

A < B:
  A <= B，并且至少一个分量严格小于 B

A || B:
  A 和 B 不可比较，也就是有的分量 A 大，有的分量 B 大
```

语义是：

```text
A < B      表示 A happens-before B
B < A      表示 B happens-before A
A || B     表示 A 和 B 并发
```

这正是 Lamport clock 做不到的地方。

**例子**

```text
event x: VC = [2, 0, 0]
event y: VC = [2, 1, 0]
```

`x < y`，说明 y 已经看到或间接看到 x 的影响。

再看：

```text
event a: VC = [3, 0, 0]
event b: VC = [1, 2, 0]
```

`a` 的第一个分量更大，`b` 的第二个分量更大，二者不可比较，所以它们是并发事件。Lamport clock 可能只给它们排成 `3 < 4` 或 `4 < 5`，但无法告诉你这是“人为顺序”还是“真实因果”。

**vector clock 的价值**

1. **冲突检测**

   多副本 KV 或 eventually consistent store 里，如果两个写入的 vector clock 并发，就说明它们不是互相覆盖关系，而是冲突版本，需要合并或让业务解决。

2. **因果一致性**

   客户端读到某些版本后再写，写入可以携带 causal context。系统可以判断某个副本是否已经具备依赖版本，避免读到违反因果的状态。

3. **调试和追踪**

   分布式 trace、事件重放、并发 bug 分析中，vector clock 能告诉你哪些事件必须先发生，哪些只是日志展示时被排到了一起。

**代价**

vector clock 的代价也明显：

```text
空间复杂度：O(N)，N 是进程/副本/参与者数量
消息开销：每次消息要携带 vector
动态成员：节点加入、退出、重启后如何维护 identity 很麻烦
高基数场景：按客户端、actor、device 建 vector 可能不可控
```

因此实际系统常用变体：

```text
version vector
dotted version vector
dependency vector
per-key vector
bounded causal metadata
HLC + tie-breaker
```

**和 Lamport clock 的边界**

| 维度 | Lamport clock | vector clock |
| --- | --- | --- |
| 数据结构 | 一个整数 | 一个向量 |
| 能否保证 happens-before 单向顺序 | 能 | 能 |
| 能否从时间戳判断 happens-before | 不能完整判断 | 可以通过向量比较判断 |
| 能否识别并发 | 不能 | 能 |
| 元数据成本 | 小 | 随参与者数量增长 |
| 适用场景 | 排序、互斥、日志合并 | 冲突检测、因果一致性 |

一句话：vector clock 相比 Lamport clock 多出的能力，是用更高的元数据成本换来对因果关系和并发关系的精确表达。

## Q007. hybrid logical clock 的设计动机是什么？

**回答：**

Hybrid Logical Clock，通常缩写为 HLC，设计动机是把物理时钟和逻辑时钟的优点合在一起：时间戳尽量接近真实时间，同时又能尊重 happens-before 的逻辑顺序。

单独使用物理时间有问题：

```text
机器之间有 clock skew；
NTP 可能抖动、回拨或短时不准；
同一时刻可能有多个事件；
物理时间不一定尊重消息因果。
```

单独使用 Lamport clock 也有问题：

```text
它只表达逻辑顺序；
数值和真实时间没有直接关系；
不适合直接做“读取某个时间点的数据”“按时间范围查询”；
系统重启、跨数据中心观察时不如物理时间直观。
```

vector clock 能表达并发，但元数据是 O(N)，在大规模数据库、KV、消息系统里通常太重。

HLC 的目标就是在这些点之间折中。

**HLC 一般长什么样**

常见形式是：

```text
HLC = (physical, logical)
```

其中：

```text
physical：来自本地物理时钟，尽量接近 wall clock
logical ：当物理时间没有前进、收到未来时间戳、同一物理时间内有多个事件时递增
```

本地事件大致按这个思路更新：

```text
now = physical_clock()
physical = max(now, physical)
if physical == old_physical:
    logical += 1
else:
    logical = 0
```

接收消息时还要把本地 HLC、消息 HLC、本地物理时间一起取 max，再决定 logical 部分如何递增。不同实现细节会不同，但目标相同：如果消息让本地知道了一个更大的逻辑时间，本地不能继续产生更小的时间戳。

**它想同时满足几件事**

1. **尊重因果**

   如果 `a happens-before b`，那么 HLC(a) 应小于 HLC(b)。这继承了 Lamport clock 的关键性质。

2. **接近真实时间**

   HLC 的 physical 部分来自物理时钟，所以它比纯 Lamport clock 更适合做 MVCC timestamp、近似时间范围查询、snapshot read、审计展示。

3. **元数据小**

   相比 vector clock，HLC 不需要为每个节点保存一个分量，通常是常数大小。

4. **对 NTP 抖动更稳**

   物理时间短暂不前进或回拨时，logical 部分可以继续推进，避免时间戳倒退。

**它不是什么**

HLC 不是 vector clock。它不能完整判断两个事件是否并发。通常只能保证：

```text
a happens-before b  =>  HLC(a) < HLC(b)
```

反过来仍然不可靠。`HLC(a) < HLC(b)` 不一定说明 a causally before b，可能只是物理时间较小或 tie-breaker 排在前面。

HLC 也不是 magic clock。它不能让一个没有时钟同步假设的系统突然获得严格外部一致性。如果系统要对外承诺 real-time order，仍然需要协议、等待、最大时钟偏差假设、提交规则和读写路径配合。

**典型适用场景**

```text
分布式数据库 MVCC timestamp
跨节点 snapshot read
多副本写入排序
事件日志的近似真实时间排序
CDC / changefeed 的时间戳
需要比 Lamport 更接近物理时间、又不想承担 vector clock 成本的场景
```

**和 Spanner TrueTime 的对比**

Spanner 通过 TrueTime 暴露时钟不确定性，并用 commit wait 等机制支持外部一致性。HLC 的路线更偏软件逻辑时间：它利用物理时钟，但用逻辑分量补上因果单调性。两者都说明一件事：在分布式系统里，时间戳不是简单调用 `now()`，而是时钟假设和协议语义共同构成的。

一句话：HLC 的设计动机，是在常数元数据成本下，让时间戳既接近物理时间，又遵守 happens-before 的逻辑单调性。

## Q008. lease 的基本语义是什么？

**回答：**

lease 的基本语义是：系统在一段有限时间内授予某个持有者某种权利，持有者必须在 lease 有效期内使用它，并在到期前续约；授权方在 lease 过期后可以把权利授予别人。

可以把 lease 理解成“带过期时间的资格”：

```text
holder A 获得 resource R 的 lease
TTL = 30s

在 lease 有效期内：
  A 可以认为自己有资格执行约定动作

如果 A 持续 keepalive / renew：
  lease 被延长

如果授权方在 TTL 内没有收到续约：
  lease 过期
  授权方可以释放资源或授予其他 holder
```

etcd 的 Lease API 就是典型例子：lease 有 TTL，服务端在 TTL 内没有收到 keepAlive 时会让 lease 过期；附着在 lease 上的 key 会在 lease 过期或撤销时被删除。

**lease 常用于什么**

1. **故障检测**

   如果客户端持续续约，说明它至少还和 lease 服务保持某种通信能力。断开太久后 lease 过期，系统可以清理它的成员身份。

2. **leader election**

   leader 持有 lease 并周期续约。其他节点观察到 lease 消失或过期后，才能竞争新的 leader 身份。

3. **服务发现和成员关系**

   服务实例注册一个带 lease 的 key。实例宕机或网络断开后，key 自动删除，消费者不再把它当成健康实例。

4. **缓存一致性**

   经典 lease 论文讨论的就是文件缓存一致性：服务端给客户端一段时间的缓存有效承诺，在 lease 有效期内减少回源校验。

5. **资源预约**

   例如临时任务 slot、分布式调度中的 ownership、分片迁移中的 owner 权利，都可以用 lease 表达。

**lease 的关键假设**

lease 必然涉及时间，所以必须说清楚时间由谁判断。

```text
服务端判断 TTL：客户端只负责续约，服务端决定是否过期
客户端本地判断 TTL：客户端根据本地时钟决定自己是否还能工作
双方都看时间：必须考虑 clock skew、网络延迟、GC pause
```

工程上更常见、也更稳的是服务端判断 lease 是否过期。客户端本地可以保守地提前停止或提前续约，但不能只凭自己的本地时钟宣称“我一定还有 lease”。

**lease 和安全边界**

lease 只说明授权服务对某个 holder 的资格判断，不代表整个世界都同步知道这个判断。尤其在分布式系统里：

```text
lease 服务认为 A 过期；
A 因网络分区或 GC pause 还没收到过期通知；
A 醒来后继续向数据库、对象存储、外部 API 写入。
```

如果下游系统不检查 fencing token，旧 holder 仍可能造成破坏。因此 lease 常常要和 epoch/fencing token 一起使用。

**面试里可以给出的定义**

```text
lease 是由授权方授予持有者的、具有有限有效期的能力。
它通过 TTL、renew/keepalive、expire/revoke 定义生命周期。
它能帮助系统在客户端失联后自动回收资源，但它不证明旧客户端已经停止执行。
```

一句话：lease 的本质是时间受限的资格，不是永久所有权，也不是旧持有者停止工作的证明。

## Q009. lease 和 lock 的区别是什么？

**回答：**

lease 和 lock 都可以用于“谁有资格访问资源”，但它们的核心边界不同：lock 强调互斥，lease 强调有期限的授权。

**lock 的语义**

lock 通常表达：

```text
某个资源同时最多被一个 holder 持有；
holder release 之前，其他竞争者不能进入临界区。
```

它关心的是 mutual exclusion。传统单机 mutex 是典型 lock：线程拿到锁后执行临界区，释放锁后别人才能进入。

分布式锁也追求类似语义，但实现更复杂。ZooKeeper recipes 里用 ephemeral sequential node 实现锁：每个客户端创建临时顺序节点，序号最小者获得锁，其他客户端 watch 自己前一个节点，避免所有等待者同时醒来。

**lease 的语义**

lease 表达：

```text
某个 holder 在一段时间内拥有某种资格；
如果它不续约，资格会自动过期；
过期后授权方可以把资格给别人。
```

它关心的是 time-bounded ownership。etcd lease 是典型例子：客户端通过 keepAlive 延续 TTL，TTL 内没有续约则 lease 过期。

**核心区别**

| 维度 | lock | lease |
| --- | --- | --- |
| 关注点 | 互斥进入临界区 | 有期限的资格 |
| 生命周期 | 通常依赖显式 release | 到期、续约、撤销 |
| 故障处理 | 持有者死掉可能导致锁不释放，除非底层有 session/TTL | 不续约会自动过期 |
| 时间假设 | 单机锁几乎不依赖物理时间；分布式锁常依赖 session | 明确依赖 TTL/续约 |
| 主要风险 | 死锁、锁泄漏、羊群效应、错误释放别人锁 | 过期边界、时钟偏差、旧 holder 继续执行 |
| 常见补充 | owner id、session、CAS、ephemeral node | fencing token、epoch、提前续约、保守停止 |

**它们经常组合在一起**

很多所谓“分布式锁”其实是 lock + lease/session 的组合：

```text
互斥：只有一个竞争者是当前 owner
过期：owner 失联后 session/lease 到期，锁节点自动删除
顺序：竞争者按 sequence node 或 revision 排队
fencing：每次获得锁时拿到递增 token
```

如果没有 lease，分布式锁遇到 holder 崩溃可能长期不释放。如果没有 fencing，lease 过期后旧 holder 醒来仍可能写坏外部资源。

**最容易混淆的地方**

1. **“拿到 lock”不等于“对所有资源都有写权限”**

   锁服务只能控制自己知道的状态。如果真正的数据写入发生在另一个数据库、文件系统或外部 API，下游也要配合检查 owner 或 fencing token。

2. **“lease 还没过期”不等于“没有别人认为你过期”**

   客户端本地时钟、服务端 TTL、网络延迟、GC pause 会让双方对边界时刻的感知不同。客户端应在接近过期前保守停止或续约，不要压线执行关键写。

3. **“lock 自动过期”本质上就是引入了 lease 语义**

   一旦锁有 TTL，它就不再是纯粹的永久互斥锁。你必须处理持有者在 TTL 后继续运行的情况。

**面试回答**

可以这样说：

```text
lock 解决“同一时刻谁能进入临界区”的互斥问题；
lease 解决“某个授权在有限时间内是否有效”的生命周期问题。
分布式环境中二者常组合使用，但只靠 lock/lease 仍不足以保护外部副作用，关键写路径还需要 fencing token 或条件写。
```

一句话：lock 是互斥语义，lease 是时间受限的授权语义；带 TTL 的分布式锁本质上已经包含 lease，因此必须处理过期后旧持有者继续执行的边界。

## Q010. lease 过期是否一定代表持有者已经停止工作？

**回答：**

不一定。lease 过期只代表“授权方认为这个 lease 已经过期，或者在 TTL 内没有收到有效续约”。它不代表旧持有者进程已经停止，也不代表旧持有者不会继续发起写入。

这是 lease 题里最重要的边界。

**为什么过期不等于停止**

1. **网络分区**

   持有者 A 和 lease 服务断开，但 A 仍然能访问某些下游资源：

   ```text
   A -> lease service: 不通
   A -> database     : 仍然通
   ```

   lease 服务会让 A 的 lease 过期，并把资格授予 B。A 如果不知道自己过期，仍可能继续写 database。

2. **GC pause / stop-the-world**

   A 持有 lease 后发生长时间 STW pause。lease 服务收不到 keepAlive，于是过期并选出 B。A 醒来后如果没有重新检查 lease，就可能带着旧身份继续执行。

3. **客户端线程卡住**

   keepAlive 线程、业务线程、网络线程不是同一个。可能 keepAlive 已经失败，但业务线程还在处理旧请求。

4. **通知延迟**

   ZooKeeper 文档里也能看到类似边界：session expiration 由服务端集群管理，客户端分区期间看不到过期通知，直到重新连接后才收到 expired 事件。也就是说，过期事实先在服务端发生，旧客户端未必立刻知道。

5. **本地时钟误判**

   如果客户端根据本地时间判断 lease 是否还有效，clock skew 或回拨可能让它误以为自己仍在有效期内。

**会造成什么问题**

典型线上事故是 split-brain writer：

```text
T1: A 获得 lease，成为 leader
T2: A 网络抖动或 GC pause，续约失败
T3: lease 服务授予 B 新 lease
T4: B 开始写入
T5: A 恢复，也继续写入
```

如果下游只相信“谁自称是 leader”，就会出现两个 leader 同时写，导致覆盖、重复扣款、重复调度、元数据回滚、文件被旧 writer 截断等问题。

**正确做法：fencing**

每次授予 lease 时，同时授予一个单调递增的 token：

```text
lease grant to A -> token = 7
lease grant to B -> token = 8
```

所有下游写入都必须带 token：

```text
write(resource, value, token=7)
write(resource, value, token=8)
```

下游保存见过的最大 token：

```text
if token < max_seen_token:
    reject stale writer
else:
    accept and update max_seen_token
```

这样即使 A 醒来继续写，它的 token #7 也会被拒绝。fencing 的关键点是：检查必须发生在真正产生副作用的地方。只在锁服务里保存 token，而数据库、对象存储、任务执行器不检查，不能防止旧写入。

**客户端也要保守**

除了 fencing，客户端本身还应该：

```text
提前续约，不要贴着 TTL 边界工作；
续约失败后停止接受新请求；
关键写前重新确认 lease 或 epoch；
长任务分段检查 lease；
在 lease lost / session expired 后进入只读或退出；
业务线程和 keepAlive 状态之间有清晰同步。
```

但这些只能降低风险，不能替代 fencing。因为客户端可能暂停、失联、bug 或被误配置，不能把正确性完全寄托在旧 holder 自觉停止上。

**面试回答**

可以直接说：

```text
lease 过期是授权方视角的事实，不是持有者进程状态的事实。
过期说明系统可以把资格授予别人，但旧持有者可能仍在运行，甚至仍能访问部分下游资源。
所以 lease 只能解决自动回收和重新选主的一部分问题；要防旧写入，必须给每次授权分配递增 fencing token，并让真正的写入目标拒绝旧 token。
```

一句话：lease 过期不证明旧持有者已经停止，只证明它不再应该被信任；防止旧持有者继续造成副作用，要靠 fencing、条件写和客户端保守停止共同完成。

## Q011. 为什么 GC pause、网络抖动、时钟漂移会影响 lease 设计？

**回答：**

因为 lease 的语义刚好站在三个不稳定因素的交界处：进程是否还在执行、网络是否还能及时续约、时间判断是否可靠。GC pause、网络抖动、时钟漂移分别打在这三个点上。

先把 lease 的基本模型写出来：

```text
client 获得 lease
client 周期性 renew / keepalive
lease service 根据 TTL 判断 lease 是否仍然有效
client 在 lease 有效期内执行某些动作
lease 过期后，lease service 可以把资格授予别人
```

这个模型看起来简单，但它隐含了几件事：

```text
client 能及时运行续约逻辑；
续约请求能及时到达 lease service；
lease service 和 client 对“还剩多少时间”不会产生危险分歧；
client 在失去 lease 后不会继续产生副作用。
```

GC pause、网络抖动、时钟漂移会逐个破坏这些假设。

**GC pause 的影响**

GC pause，尤其是 stop-the-world pause，会让进程在一段时间内完全不调度业务线程和续约线程。

典型时间线是：

```text
T0: client A 获得 lease，TTL = 10s
T1: A 进入 30s GC pause
T2: lease service 收不到 keepalive，认为 A 的 lease 过期
T3: client B 获得新 lease
T4: A 从 GC pause 恢复，继续执行 pause 前排队的写请求
```

从 A 的角度看，它只是“睡了一会儿”。从 lease service 的角度看，A 已经过期。危险就在这里：进程暂停不等于进程死亡，暂停恢复后还会继续跑旧代码。

所以 lease 设计里不能只写：

```text
if hasLease:
    write()
```

因为 `hasLease` 可能是 pause 前的旧判断。关键写入前要重新确认本地 lease 状态，更重要的是资源端要检查 fencing token。

**网络抖动的影响**

网络抖动会影响两个方向：

1. keepalive 请求可能延迟或丢失；
2. 业务写请求可能在网络里延迟很久后才到达资源端。

第一类问题会造成误过期：

```text
A 还活着，但 keepalive 连续丢了
lease service 认为 A 失联
lease 被授予 B
```

第二类问题更隐蔽：

```text
A 在 lease 有效期内发出 write(token=7)
write 在网络里延迟
A 的 lease 过期，B 获得 token=8 并写入成功
A 的旧 write(token=7) 之后才到达资源端
```

如果资源端不检查 token，旧写入可能覆盖新 owner 的写入。注意这时 A 发出请求时可能真的还持有 lease，但请求到达时已经过期。客户端本地检查无法覆盖这个窗口。

**时钟漂移的影响**

lease 一定涉及时间。问题是，分布式系统没有完美同步的物理时钟。

如果 client 用本地 wall clock 判断 lease 是否有效，可能发生：

```text
client 时钟慢：以为 lease 还没过期，实际服务端已经过期
client 时钟快：过早放弃 lease，造成可用性下降
clock 回拨 ：本地有效期被错误延长
clock 跳变 ：续约调度、deadline、超时判断全部抖动
```

更稳的设计是让 lease service 作为过期判断的权威。客户端本地只做保守估计：

```text
服务端返回 TTL 或 server-side deadline
客户端设置本地安全截止时间
安全截止时间要扣掉 clock skew、RTT、调度延迟、GC pause 预算
到达本地安全截止时间后，即使还没收到明确 expired，也停止关键写
```

但这仍然只是客户端自我约束，不是最终正确性保证。

**对 lease 参数的影响**

TTL 不能凭感觉选。至少要考虑：

```text
renew interval
P99 / P999 网络 RTT
网络抖动和丢包
GC pause 分布
调度延迟
clock skew 上界
lease service 的处理延迟
故障检测延迟预算
误过期能不能接受
```

常见做法是：

```text
renew_interval = TTL / 3 或 TTL / 2
client 在 TTL 剩余较多时就续约
连续续约失败后进入 suspect 状态
本地安全时间到达后停止写
资源端用 fencing token 拒绝旧 owner
```

TTL 太短，网络抖动和 GC pause 会导致频繁误过期、频繁重新选主、可用性差。TTL 太长，真实故障后恢复慢，旧 owner 残留时间长，资源释放慢。这个取舍不是单纯性能问题，也会影响正确性边界。

**和 Raft 的对比**

Raft 论文里有一个很重要的设计态度：一致性安全不依赖时钟，异常的消息延迟和坏时钟最多影响可用性。lease 设计如果把正确性完全压在时间判断上，就比 Raft 这种共识协议脆弱得多。

这不是说 lease 不能用，而是要承认它的边界：

```text
lease 可以做故障检测、leader hint、缓存有效期、资源自动回收；
lease 不应该单独承担“旧持有者绝不再写”的正确性保证；
需要 fencing token、资源端条件写、epoch 校验兜住旧写入。
```

一句话：GC pause 让持有者“活着但停顿”，网络抖动让续约和写入“到达时间不可控”，时钟漂移让本地有效期判断“不可信”；lease 设计必须把这些都当成常态，而不是罕见异常。

## Q012. fencing token 解决什么问题？

**回答：**

fencing token 解决的是 stale owner 继续写入的问题。更具体一点，它解决的是：旧的 lock holder、lease holder、leader、writer 在失去资格后仍然向资源端发起写入，而资源端需要一种办法识别并拒绝它。

典型事故长这样：

```text
T1: client A 获得锁，token = 7
T2: A 发生长时间 GC pause
T3: A 的 lease 过期
T4: client B 获得锁，token = 8
T5: B 写资源成功
T6: A 恢复，继续用旧身份写资源
```

如果资源端只知道“请求来自 A”，或者只相信“客户端说自己持有锁”，那它无法判断 A 已经过期。fencing token 给每次授权一个单调递增的编号：

```text
grant A -> token 7
grant B -> token 8
grant C -> token 9
```

资源端保存它见过的最大 token：

```text
max_token[resource] = 8
```

收到写请求时：

```text
if request.token < max_token[resource]:
    reject
else:
    accept
    max_token[resource] = request.token
```

这样 A 的旧请求 `token=7` 即使迟到，也会被拒绝。

**它解决的不是互斥本身**

fencing token 不负责“谁能拿到锁”。拿锁、续约、过期、选主，仍然由 lock service、lease service 或共识系统处理。

fencing token 负责的是另一层问题：

```text
即使旧 owner 因为 pause、网络延迟、bug 继续发请求，资源端也能挡住它。
```

所以 fencing token 是 lease/lock 的安全补丁，不是 lease/lock 的替代品。

**token 必须满足什么性质**

1. **单调递增**

   新授权的 token 必须大于旧授权。随机 UUID 不行。随机值可以用于“释放锁时确认是不是自己创建的锁”，但不能判断新旧顺序。

2. **作用域清楚**

   token 是全局的，还是 per-resource、per-shard、per-tenant 的，要提前定义。资源端比较 token 时必须在同一作用域内比较。

3. **持久化**

   资源端保存的 `max_token` 不能在重启、恢复 snapshot、主从切换后倒退。否则旧 token 可能重新被接受。

4. **和写入原子校验**

   token 检查和资源修改要在同一个原子操作里完成。否则两个请求并发到达时，可能都通过检查，然后后写覆盖前写。

5. **由可信授权方生成**

   不能让客户端自己随便填 token。token 应由共识系统、锁服务、元数据服务、单调序列分配器生成，并且生成过程本身要有一致性保证。

**它和几个相近概念的区别**

| 概念 | 主要作用 | 和 fencing token 的差异 |
| --- | --- | --- |
| idempotency key | 防止同一请求重复执行 | 不表达 owner 新旧 |
| request id | 跟踪请求或去重 | 通常不单调，不能 fencing |
| lock random value | 防止误删别人持有的锁 | 不能判断过期后谁更新 |
| lease id | 标识某次 lease | 如果不单调，不能拒绝旧写 |
| epoch/term | 表示一代领导权或所有权 | 可以作为 fencing token 使用 |
| version/CAS | 防止覆盖并发修改 | 可以和 fencing token 组合 |

**例子：任务调度**

假设一个调度器负责执行分片任务：

```text
shard-1 owner = worker A, epoch = 12
```

A 暂停后 lease 过期，worker B 接手：

```text
shard-1 owner = worker B, epoch = 13
```

任务执行结果写回时必须带 epoch：

```sql
UPDATE shard_result
SET result = ?, owner_epoch = 13
WHERE shard_id = 'shard-1'
  AND owner_epoch <= 13;
```

更严格的写法会要求当前 owner 也是 B，或者要求写入表单独保存 `max_seen_epoch`。关键点是：A 带着 epoch 12 回来时，数据库不能接受。

一句话：fencing token 解决的是“旧持有者失去资格后仍然写入”的问题，它通过单调递增授权编号，让真正的资源端能拒绝过期 owner 的副作用。

## Q013. 为什么分布式锁没有 fencing token 可能不安全？

**回答：**

因为分布式锁服务只能管理“锁状态”，不一定能管理“真实资源的副作用”。没有 fencing token 时，旧持有者在锁过期后仍然可能写数据库、写文件、调用外部 API。锁服务认为锁已经给了新 owner，但资源端如果不知道这一点，就会接受旧 owner 的请求。

先看一个完整场景：

```text
1. A 获得分布式锁，开始处理 order-123
2. A 发生 GC pause，持续 60 秒
3. 锁 TTL 只有 10 秒，锁服务删除 A 的锁
4. B 获得同一把锁，处理 order-123，并写入数据库
5. A 恢复，从 pause 前的位置继续执行，也写入数据库
```

锁服务没有做错。A 的锁确实过期了，B 也确实合法获得了锁。问题在于数据库不知道 A 的写入已经过期。

**没有 fencing token 时，常见防线都不够**

1. **客户端检查锁是否还存在，不够**

   A 可以在检查后立刻 pause：

   ```text
   check lock: OK
   pause 60s
   write database
   ```

   检查和写入之间有窗口。只要这个窗口里发生 pause、调度延迟、网络延迟，旧写入就可能越过边界。

2. **锁 TTL 自动过期，不够**

   TTL 只能让锁服务把锁让给别人，不能强制旧进程停止。旧进程可能还在运行，也可能有已经发出的网络请求在路上。

3. **锁里放随机 value，不够**

   Redis 风格的随机 value 可以避免“释放锁时删掉别人锁”的问题：

   ```text
   delete lock only if value == my_random_value
   ```

   但随机 value 没有新旧顺序。资源端看到两个随机值，不知道哪个更新。

4. **锁服务线性一致，也不够**

   即使锁服务本身完全正确，外部资源仍然可能被旧 holder 写入。正确的锁状态没有自动传播到数据库、文件系统、对象存储。

**fencing token 补的正是这个缺口**

带 fencing token 后，场景变成：

```text
A 获得锁 -> token=100
B 获得锁 -> token=101

B 写数据库: token=101，接受
A 恢复后写数据库: token=100，拒绝
```

数据库只需要记住当前见过的最大 token。旧请求迟到、重放、恢复、并发到达，都能通过 token 顺序识别。

**什么时候没有 fencing token 也许可以接受**

如果分布式锁只用于效率优化，而不是正确性边界，风险小一些。例如：

```text
防止多个 worker 重复做同一份可丢弃的缓存预热；
减少重复的后台扫描；
降低重复刷新配置的流量。
```

这些场景里，即使锁失效，最多多做一些工作。真正的数据正确性不依赖这把锁。

如果锁保护的是下面这些东西，就不能忽略 fencing：

```text
扣款
库存扣减
任务只执行一次
主从切换中的写权限
文件 truncate / rename
对象存储覆盖
元数据 owner 更新
分片迁移
```

**面试里要强调的点**

分布式锁有两层语义：

```text
锁服务层：同一时刻谁被认为持有锁
资源端层：谁的写入真正能生效
```

没有 fencing token 时，这两层之间断开了。锁服务做出的“B 已经是新 owner”这个事实，资源端并不会自动知道。

一句话：没有 fencing token 的分布式锁只能证明锁服务里的 ownership 曾经成立，不能阻止旧 owner 在锁过期后继续写外部资源；只要锁保护正确性，就需要资源端强制校验 fencing token。

## Q014. epoch 和 term 在分布式系统中通常表示什么？

**回答：**

`epoch` 和 `term` 通常表示“第几代权威”。它们不是物理时间，而是逻辑代数，用来区分新旧 leader、新旧 owner、新旧配置、新旧租约。

可以把它们理解成 generation number：

```text
epoch = 12: 第 12 代 owner / leader / config
epoch = 13: 第 13 代 owner / leader / config
```

系统看到 `epoch=12` 的消息，就知道它属于旧一代；看到 `epoch=13`，就知道它更新。

**常见用途**

1. **leader election**

   Raft 里叫 `term`。每次开始新一轮选举，term 增加。一个 term 里最多选出一个 leader。RPC 里带 term，节点看到更大的 term 会更新自己并退回 follower。

2. **lease grant**

   每次授予某个资源的 lease，可以生成新的 lease epoch：

   ```text
   shard-7 owner=A epoch=20
   shard-7 owner=B epoch=21
   ```

   旧 owner A 之后带着 epoch 20 写入，就应该被拒绝。

3. **配置版本**

   集群成员变化、路由表变化、分片分配变化，常用 config epoch 表示：

   ```text
   config_epoch=5: shard-1 在 node A
   config_epoch=6: shard-1 在 node B
   ```

   节点用 epoch 判断自己看到的是不是旧配置。

4. **writer incarnation**

   同一个 worker 重启后，进程 ID 可能一样，连接地址可能一样，但它已经是新的 incarnation。epoch 可以区分“同名进程的不同生命期”。

5. **复制协议**

   主从复制、日志复制、对象存储 multipart writer、元数据 master，都会用 epoch/term 防止旧主继续提交。

**epoch/term 的基本性质**

通常要求：

```text
同一作用域内单调递增；
新权威的 epoch 大于旧权威；
消息、写入、心跳都携带 epoch；
接收端拒绝旧 epoch；
epoch 的生成和持久化不能回退；
看到更大 epoch 时，本地旧 owner 要停止或降级。
```

这里最重要的是“作用域”。Raft term 是一个 Raft group 内的逻辑时钟。某个 shard 的 lease epoch 通常只对这个 shard 或这个资源有效。不同作用域的 epoch 不能随便比较。

**epoch 和 sequence number 的关系**

epoch 可以看作一种特殊的 sequence number，但它不是每个请求都递增，而是在“权威换代”时递增。

```text
request sequence:
  每个请求、每条日志、每条消息递增

epoch / term:
  每次 leader、owner、配置、lease generation 变化时递增
```

两者经常组合：

```text
(epoch, seq)
```

含义是：第 `epoch` 代 owner 发出的第 `seq` 个操作。比较时通常先比 epoch，再比 seq。旧 epoch 的再大 seq 也不能压过新 epoch。

**不要把 epoch 当时间戳**

`epoch=20` 不代表真实时间比 `epoch=19` 晚多少，也不代表持有者活了多久。它只说明系统承认了一个更新的 generation。

一句话：epoch 和 term 是分布式系统里的逻辑代数，用来给 leader、owner、lease、配置换代排序；它们的作用是识别旧权威、拒绝旧消息和旧写入。

## Q015. Raft term 和 lease epoch 有什么相似之处？

**回答：**

Raft term 和 lease epoch 都是在表达“这一代权威是谁”。它们都用单调递增的逻辑代数，把旧 leader、旧 owner、旧 lease holder 和新的一代区分开。

相似点主要有几类。

**第一，都是 generation number**

Raft term：

```text
term=5: 某一轮选举和这一轮选出的 leader
term=6: 更新一轮选举和更新 leader 权威
```

lease epoch：

```text
epoch=5: resource R 的 owner 是 A
epoch=6: resource R 的 owner 是 B
```

二者都不是物理时间，也不是持续时长。它们只是“第几代”的编号。

**第二，都能识别旧消息**

Raft RPC 会携带 term。接收方发现请求里的 term 小于自己的 `currentTerm`，就拒绝；发现对方 term 更大，就更新本地 term 并转成 follower。

lease epoch 也类似。资源端或元数据端看到：

```text
write(epoch=5)
current_epoch=6
```

就应该拒绝，因为这是旧 owner 的写入。

**第三，都要求单调和持久化**

Raft 的 `currentTerm` 是持久化状态，不能重启后回退。lease epoch 如果用于 fencing，也必须持久化。否则系统重启后忘了最新 epoch，旧 owner 就可能重新被接受。

**第四，都用于压住 stale owner**

Raft 里，旧 leader 发现更大的 term 后必须退位。lease 系统里，旧 holder 发现更大的 epoch 或续约失败后也应该停止关键写。

但只靠旧 holder 自觉停止还不够，所以 lease epoch 常常还要作为 fencing token，由资源端强制校验。

**差异也要说清楚**

Raft term 和 lease epoch 相似，但不能混为一谈。

| 维度 | Raft term | lease epoch |
| --- | --- | --- |
| 所属协议 | 共识协议的一部分 | lease/lock/ownership 协议的一部分 |
| 生成方式 | 选举时由候选人递增，通过多数派投票建立权威 | 通常由 lease service、元数据服务或 sequencer 授予 |
| 安全依赖 | Raft 安全性不依赖准确时钟 | lease 通常依赖 TTL、renew 和时间假设 |
| 作用域 | 一个 Raft group | 某个资源、shard、tenant、lock 或 owner |
| 主要用途 | leader election、日志复制安全 | owner fencing、旧写拒绝、资源资格 |
| 是否等同于锁 | 不是 | 也不是，通常是锁/lease 的一部分 |

Raft term 本身也不表示“leader 一定还活着”。leader 是否有效，要看选举、心跳、日志复制和多数派通信。lease epoch 也不表示“holder 一定正在运行”，它只表示授权方给过这一代资格。

**面试里可以这样回答**

```text
Raft term 和 lease epoch 都是逻辑代数，都用来区分新旧权威。
旧 term 的 Raft 消息会被更高 term 的节点拒绝；旧 epoch 的写入也应该被资源端拒绝。
区别在于，Raft term 是共识协议安全性的一部分，靠多数派和日志规则建立；lease epoch 通常来自租约授权，必须配合 TTL、续约和 fencing 才能防止旧 owner 写入。
```

一句话：Raft term 和 lease epoch 的共同点是“代际 fencing”，差异是 Raft term 属于共识协议，lease epoch 属于时间受限 ownership 协议，后者更需要资源端校验来补上旧持有者继续执行的风险。

## Q016. stale owner 为什么危险？

**回答：**

stale owner 危险，是因为它曾经合法，后来失去资格，但它手里还保留着旧状态、旧连接、旧缓存、旧任务和旧身份。很多系统最怕的不是“完全陌生的非法请求”，而是“以前真的合法、现在已经过期的请求”。

典型 stale owner 包括：

```text
旧 leader
旧 shard owner
旧 lock holder
旧 lease holder
旧 primary
旧调度器
旧任务执行器
旧配置版本下的 writer
```

**危险一：split-brain write**

两个 owner 同时写同一份资源：

```text
A: 旧 owner，epoch=10
B: 新 owner，epoch=11
```

如果资源端不校验 epoch，A 和 B 的写入可能交错。结果通常不是简单失败，而是状态被悄悄污染。

**危险二：覆盖新状态**

B 已经基于新配置写入：

```text
balance = 100
owner_epoch = 11
```

A 恢复后用旧快照写回：

```text
balance = 80
owner_epoch = 10
```

如果没有条件写，旧状态会覆盖新状态。这类 bug 事后很难排查，因为日志里 A 以前确实拿到过 owner 身份。

**危险三：重复外部副作用**

stale owner 可能重复执行不可回滚动作：

```text
发邮件
扣款
发券
提交外部任务
删除对象存储文件
触发下游工作流
```

这些副作用不一定能靠数据库回滚。即使主系统发现 A 已经过期，外部系统可能已经接受了请求。

**危险四：破坏恢复流程**

在恢复、failover、shard migration 中，新 owner 可能已经完成：

```text
加载 snapshot
回放 WAL
接管 routing
开始服务新请求
```

旧 owner 如果继续写，会让恢复后的状态再次偏离。最糟糕的是，它写入的是旧格式、旧 schema、旧 config 下的数据。

**危险五：读写混合造成假一致**

stale owner 不一定只写。它还可能继续对外提供读：

```text
旧 leader 返回旧配置下的读结果
旧 cache owner 返回过期缓存
旧 primary 告诉客户端写成功但没有复制到新主
```

客户端看到的是“成功响应”，系统内部却已经切到新 owner。后续排查会变成两套历史互相打架。

**为什么 stale owner 很难靠客户端规约解决**

因为 stale owner 往往不是恶意的。它可能只是：

```text
GC pause 后恢复；
网络分区后重新连上部分资源；
线程调度延迟；
进程挂起后被恢复；
续约响应丢失；
本地状态没有及时同步；
旧请求在网络里迟到。
```

你不能指望它一定能“意识到自己已经旧了”。正确性必须由接收写入的一方强制判断。

**工程上的防线**

```text
所有权写入带 epoch / term / fencing token；
资源端保存 max_seen_epoch；
写入时原子校验 token；
旧 epoch 写入返回明确错误；
客户端收到 stale 错误后停止服务并刷新状态；
外部副作用通过支持条件写的代理或 outbox 统一发出；
长任务分阶段检查 owner epoch。
```

一句话：stale owner 危险，是因为它拥有历史合法性和继续执行能力；如果资源端不识别新旧代际，它就可能用旧身份覆盖新状态、制造重复副作用或破坏 failover。

## Q017. 如何拒绝旧 epoch 的写入？

**回答：**

拒绝旧 epoch 写入的核心做法是：资源端保存当前接受过的最大 epoch 或当前 owner epoch，每次写入都必须携带 epoch，并且“检查 epoch”和“执行写入”必须原子完成。

一个最小模型是：

```text
resource_state:
  resource_id
  value
  owner_id
  current_epoch
```

写请求携带：

```text
request:
  resource_id
  owner_id
  epoch
  payload
```

资源端处理：

```text
if request.epoch < current_epoch:
    reject stale write
else:
    apply write
    current_epoch = max(current_epoch, request.epoch)
```

实际系统通常更严格，还会校验 owner：

```text
if request.epoch != current_epoch:
    reject
if request.owner_id != current_owner:
    reject
apply write
```

选择 `epoch >= current_epoch` 还是 `epoch == current_epoch`，取决于授权流程。如果写请求本身允许推进 epoch，可以用 `>=`；如果 epoch 只能由元数据服务先变更，普通写路径应要求 `==`。

**数据库里的写法**

可以用条件更新：

```sql
UPDATE shard_state
SET value = :value,
    updated_at = :now
WHERE shard_id = :shard_id
  AND owner_id = :owner_id
  AND owner_epoch = :epoch;
```

如果影响行数是 0，就说明 owner 或 epoch 已经不匹配，返回 stale owner 错误。

也可以把 fencing token 作为单调上界：

```sql
UPDATE resource
SET value = :value,
    max_token = :token
WHERE resource_id = :resource_id
  AND max_token <= :token;
```

这里要小心同一个 token 下的并发重复写。通常还需要 request sequence 或 idempotency key：

```text
(epoch, request_seq)
```

否则同一代 owner 内的重试和并发写可能互相覆盖。

**KV/CAS 系统里的写法**

如果底层 KV 支持 compare-and-swap，可以这样表达：

```text
compare:
  current_epoch == request.epoch
  current_owner == request.owner
then:
  update value
else:
  return StaleOwner
```

etcd、ZooKeeper 这类系统还可以借助 revision、version、zxid、znode version 等元数据做条件更新。关键不在于字段叫什么，而在于比较和写入必须由同一个线性一致资源完成。

**对象存储或文件系统怎么办**

对象存储有时没有直接的“拒绝旧 token”语义。可以用几种办法：

```text
把元数据写入支持 CAS 的元数据服务，数据对象只按元数据引用生效；
对象名包含 epoch，新 epoch 写新对象，提交指针时 CAS；
通过一个强制校验 token 的写代理访问对象存储；
使用支持条件请求的接口，如 If-Match / generation match；
不能校验时，不把分布式锁用于强正确性写路径。
```

**错误语义**

旧 epoch 写入不要伪装成普通网络失败。它应该是明确的语义错误：

```text
StaleOwner
NotLeader
LeaseExpired
FencingTokenRejected
PreconditionFailed
```

HTTP 风格可以映射成：

```text
409 Conflict
412 Precondition Failed
403 Forbidden
```

具体用哪个不重要，重要的是客户端收到后不能继续重试同一个旧 epoch。它应该刷新 owner 状态、停止当前任务或重新参与选主。

**恢复时的坑**

拒绝旧 epoch 的状态必须参与持久化和恢复：

```text
max_seen_epoch 不能因为重启丢失；
snapshot 不能回滚 max_seen_epoch；
主从切换后新主必须知道最大 epoch；
日志重放要先恢复 fencing 状态再接受写；
备份恢复到旧时间点后，要避免旧 token 重新有效。
```

一句话：拒绝旧 epoch 写入，就是让资源端把 epoch 当成写入前置条件，并把“比较 epoch”和“产生副作用”做成一个原子、持久化的动作。

## Q018. 为什么 fencing 需要由资源端强制校验？

**回答：**

因为真正产生副作用的是资源端。锁服务、lease 服务、客户端本地状态都只能说明“谁应该有资格”，但只有数据库、文件系统、对象存储、任务执行器、外部 API 网关知道“这次写入是否真的生效”。

如果 fencing 不在资源端校验，会留下一个无法关闭的窗口：

```text
client A 检查 lease：有效
client A 发出写请求
写请求在网络里延迟
lease 过期，client B 获得新 token
client B 写入成功
client A 的旧写请求到达资源端
```

客户端检查发生在发送前，锁服务状态变化发生在中间，资源端接收发生在最后。只有资源端能在最后一刻判断：

```text
这个 token 是否已经落后？
```

**锁服务不能替资源端完成校验**

锁服务可能知道：

```text
current_owner = B
current_token = 8
```

但旧请求可能完全绕过锁服务，直接到数据库：

```text
A -> database write(token=7)
```

数据库如果不检查 token，就会接受。锁服务无法拦截这条已经发出去的 TCP 包，也无法撤销外部 API 已经执行的副作用。

**客户端也不能替资源端完成校验**

客户端检查有三个问题：

1. **检查和写入不是原子操作**

   检查后可能 pause，写入时已经过期。

2. **客户端可能不知道自己过期**

   网络分区时，服务端已经让 lease 过期，客户端还没收到通知。

3. **客户端不是可信边界**

   代码 bug、旧版本客户端、误配置、恶意请求，都可能跳过检查。

资源端校验把正确性放在最终生效点，而不是放在调用方自律上。

**资源端校验必须原子**

资源端不能这样做：

```text
read max_token
if token ok:
    write value
    write max_token
```

如果两个请求并发执行，这个拆开的流程会有 race。正确做法是使用事务、CAS、行锁、单线程 actor、Raft log、数据库约束等机制，把检查和写入放在同一个串行化点。

```sql
UPDATE resource
SET value = :value,
    max_token = :token
WHERE id = :id
  AND max_token <= :token;
```

或者：

```text
append to resource log only if token >= max_token
```

**资源端不支持 fencing 怎么办**

那就不能直接依赖分布式锁保护强正确性。常见替代方案：

```text
在资源前加一个支持 fencing 的代理；
把外部副作用先写入本地 outbox，由单 owner 串行发送；
用支持条件写的元数据层控制可见指针；
让操作天然幂等，重复执行不会改变业务结果；
把锁用途降级为效率优化，不承诺正确性。
```

比如对象存储不支持按 token 拒绝覆盖时，可以不要覆盖原对象，而是写：

```text
object/resource_id/epoch=12/data
object/resource_id/epoch=13/data
```

然后用支持 CAS 的元数据记录“当前可见对象是 epoch=13”。旧 epoch 写了对象也没关系，只要提交可见指针时被拒绝，它就不会生效。

一句话：fencing 必须在资源端强制校验，因为资源端是副作用发生的地方；不在最终写入点校验 token，就无法阻止旧 owner 的迟到请求。

## Q019. 只在客户端检查 lease 是否过期为什么不够？

**回答：**

只在客户端检查 lease 是否过期不够，因为检查结果只在“检查那一刻”成立，而写入发生在之后。两者之间可能隔着 GC pause、线程调度、网络延迟、重试队列、缓冲区和下游处理时间。

一个很短的反例：

```text
if lease_not_expired():
    write_to_db()
```

问题在于，代码看起来连续，真实执行不连续：

```text
T1: lease_not_expired() 返回 true
T2: 进程 pause 30 秒
T3: lease 过期，别人获得新 lease
T4: 原进程恢复，继续执行 write_to_db()
```

客户端检查没有错，但它已经过期了。

**不够的原因**

1. **检查和写入之间有时间窗口**

   即使只有几微秒，也可能发生线程切换、系统调用阻塞、网络排队。分布式系统里这个窗口没有严格上界。

2. **本地时钟不权威**

   客户端如果用本地时间判断 TTL，clock skew、回拨、跳变都可能让判断错误。服务端认为 lease 已过期，客户端可能还以为有效。

3. **续约状态可能滞后**

   keepalive 线程失败了，业务线程未必立刻知道。或者 keepalive 成功响应还在队列里，业务线程看到的是旧状态。

4. **网络请求可能迟到**

   客户端在 lease 有效期内发出的请求，不保证在有效期内到达资源端。网络可以任意延迟包。

5. **多线程会放大问题**

   一个线程检测 lease，另一个线程执行写入。如果同步不严格，写入线程可能拿到过期缓存。

6. **客户端不是可信边界**

   强正确性不能依赖“所有客户端都写对了”。旧版本客户端、脚本、运维工具、bug 都可能绕过本地检查。

**客户端检查有什么用**

客户端检查仍然有价值：

```text
减少明显过期后的无意义写；
在续约失败后尽快停止接新请求；
降低资源端拒绝压力；
缩短旧 owner 活跃窗口；
帮助本地任务做优雅退出。
```

但它是优化和自我约束，不是最终正确性边界。

**正确组合**

更稳的组合是：

```text
客户端：
  提前续约
  维护本地安全截止时间
  续约失败进入 suspect/read-only
  超过安全截止时间停止写

资源端：
  校验 fencing token / epoch
  原子拒绝旧写
  返回明确 stale owner 错误
```

客户端检查让系统少犯错，资源端 fencing 让错误不能生效。

**面试里可以这样答**

```text
只在客户端检查 lease 过期，本质上是 TOCTOU 问题。
check 发生时 lease 可能有效，use 发生时 lease 已经过期。
由于 GC pause、网络延迟、时钟漂移和多线程调度都不可控，客户端检查只能作为保守退出机制，不能作为正确性保证。
真正的写入目标必须校验 epoch 或 fencing token。
```

一句话：客户端检查 lease 是必要的自律，但不是可信仲裁；防旧写必须在资源端完成。

## Q020. lease renewal 的失败语义如何定义？

**回答：**

lease renewal 的失败语义不能简单定义成“续约失败就是没续上”。分布式系统里，续约请求失败、超时、连接断开，很多时候表示的是 unknown：客户端不知道服务端到底有没有收到、有没有处理、有没有延长 TTL。

所以要把 renewal 结果分成几类。

**第一类：明确成功**

服务端返回成功，并给出新的 TTL、revision、lease epoch 或 server-side deadline：

```text
RenewOK(
  lease_id,
  epoch,
  ttl_remaining,
  response_revision
)
```

客户端可以继续工作，但仍要更新本地安全截止时间。不要只用本地 `now + ttl` 粗暴计算，最好扣掉安全余量：

```text
local_safe_until = now_monotonic + ttl_remaining - safety_margin
```

**第二类：明确失败**

服务端明确返回：

```text
lease not found
lease expired
lease revoked
epoch stale
not owner
permission denied
```

这类结果表示客户端已经不应继续以该 lease 身份执行关键写。正确动作通常是：

```text
停止接收新写；
取消或暂停本地任务；
让正在执行的任务尽快到达检查点；
刷新 owner 状态；
必要时重新竞选或重新获取 lease；
旧 epoch 的请求不能继续重试。
```

**第三类：超时或网络错误**

这是最容易出错的一类：

```text
renew request timeout
connection reset
deadline exceeded
unknown server error
client context canceled
```

这些结果不能直接解释成“续约失败”，也不能解释成“续约成功”。它们只说明客户端没有拿到可靠结果。

处理原则是：

```text
在本地安全截止时间之前，可以重试 renew；
超过本地安全截止时间后，必须停止关键写；
不能因为一次 timeout 就立刻释放别人可见的资源；
也不能因为 timeout 前发出过 renew 就假设自己续约成功。
```

也就是说，timeout 的语义是 unknown，客户端要用本地 deadline 决定自己还能否继续工作。

**第四类：成功响应迟到**

还有一种边界：续约响应迟到了。

```text
T1: client 发送 renew(epoch=7)
T2: client 超时，进入 suspect
T3: client 又发现本地 safe_until 已过，停止写
T4: T1 的 renew 成功响应迟到
```

迟到成功能不能让客户端恢复？一般不应该直接恢复。客户端需要重新确认：

```text
这个 lease 是否仍是当前 epoch；
服务端返回的 TTL 是否仍然足够；
期间是否已经观察到更高 epoch；
本地是否已经释放或转移了任务；
资源端是否仍接受该 token。
```

工程上更简单的规则是：一旦本地状态进入 `LOST`，迟到的旧 renew 成功响应不能把它改回 `ACTIVE`，必须重新获取 lease。

**推荐状态机**

可以把客户端 lease 状态定义成：

```text
ACTIVE:
  最近一次 renew 成功，本地安全时间未到

SUSPECT:
  renew 失败或超时，但本地安全时间还没到
  可以继续有限工作，最好停止接新写，并加快续约或确认

LOST:
  服务端明确拒绝，或者本地安全时间已过
  必须停止关键写，旧 epoch 请求不能继续发

REACQUIRING:
  尝试重新获取 lease，成功后获得新 epoch/token
```

是否允许 `SUSPECT` 状态继续写，取决于业务风险。很多强一致写路径会选择进入 `SUSPECT` 后立刻停止新写，只允许已有操作快速收尾；更保守的系统会直接转 `LOST`。

**续约协议里应该返回什么**

一个好的 renewal 响应不只返回 OK：

```text
lease_id
epoch / fencing_token
ttl_remaining
server_revision
current_owner
server_time 或逻辑时间
```

这样客户端能判断自己看到的是不是旧响应，也能把日志和指标对齐。

**可观测指标**

lease renewal 失败语义要配套指标，否则线上只能看见“偶发抖动”：

```text
renew_success_total
renew_timeout_total
renew_rejected_total
renew_latency_ms
ttl_remaining_at_renew
lease_suspect_transitions
lease_lost_transitions
stale_write_rejected_total
max_gc_pause_ms
network_rtt_to_lease_service
```

这些指标能回答：到底是 GC pause 太长、网络抖动、lease service 卡顿，还是 TTL 选得太短。

**面试回答**

可以这样说：

```text
lease renewal 的失败要区分 definite failure 和 unknown。
服务端明确说 expired/revoked/stale，客户端必须停止旧 lease 下的关键写。
如果只是 timeout 或网络错误，语义是未知：请求可能成功也可能失败。客户端只能在本地安全 deadline 之前重试或降级，超过 deadline 就必须放弃旧 lease。
不管客户端怎么处理，资源端仍然要用 fencing token 拒绝旧 epoch 写入。
```

一句话：renewal failure 的核心语义不是“失败了怎么办”，而是“哪些失败是确定失效，哪些只是未知；未知状态下客户端最多保守继续，不能假设自己仍然安全”。

## 参考和校验点

1. Leslie Lamport, [Time, Clocks, and the Ordering of Events in a Distributed System](https://lamport.azurewebsites.net/pubs/time-clocks.pdf)：happened-before、Lamport logical clock、clock condition、用逻辑时钟扩展为全序。
2. Sandeep Kulkarni 等，[Logical Physical Clocks and Consistent Snapshots in Globally Distributed Databases](https://cse.buffalo.edu/tech-reports/2014-04.pdf)：HLC 同时接近物理时钟并保持逻辑时钟的因果单调性，也对 Lamport clock、vector clock、physical time 的能力边界做了对比。
3. Google Research, [Spanner: Google's Globally-Distributed Database](https://research.google/pubs/spanner-googles-globally-distributed-database-2/)：TrueTime 暴露 clock uncertainty，并用于支持 externally-consistent distributed transactions。
4. Cary G. Gray, David R. Cheriton, [Leases: An Efficient Fault-Tolerant Mechanism for Distributed File Cache Consistency](https://web.stanford.edu/class/cs240/readings/leases.pdf)：lease 是有限期限授权，常用于在故障和通信不可靠条件下处理缓存一致性。
5. etcd 文档，[etcd API - Revisions / Lease API](https://etcd.io/docs/v3.5/learning/api/)：store revision 是集群级逻辑时钟，lease 通过 TTL 和 keepAlive 表达客户端存活与过期。
6. Apache ZooKeeper 文档，[Programmer's Guide](https://zookeeper.apache.org/doc/current/zookeeperProgrammers.html)：zxid 提供 ZooKeeper 状态变化的全序；sequence node 是父 znode 下唯一递增计数；session expiration 由集群管理，客户端分区期间未必立刻知道。
7. Apache ZooKeeper 文档，[Recipes and Solutions - Locks](https://zookeeper.apache.org/doc/current/recipes.html#sc_recipes_Locks)：用 ephemeral sequential nodes 实现分布式锁，并通过 watch 前一个节点避免 herd effect。
8. Google Research, [The Chubby lock service for loosely-coupled distributed systems](https://research.google/pubs/the-chubby-lock-service-for-loosely-coupled-distributed-systems/)：Chubby 是面向粗粒度分布式协调的锁服务，强调可用性和可靠性，而不是高吞吐数据路径。
9. Diego Ongaro, John Ousterhout, [In Search of an Understandable Consensus Algorithm](https://raft.github.io/raft.pdf)：Raft term 是随选举单调增加的逻辑时钟，RPC 携带 term，节点看到更高 term 会更新并转为 follower。
10. Martin Kleppmann, [How to do distributed locking](https://martin.kleppmann.com/2016/02/08/how-to-do-distributed-locking.html)：用 GC pause、网络延迟示例说明 lease holder 可能过期后继续写，fencing token 必须由 storage/resource service 主动校验。
