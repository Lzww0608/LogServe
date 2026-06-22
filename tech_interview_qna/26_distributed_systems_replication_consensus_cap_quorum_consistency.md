# 26. 分布式系统基础：复制、共识、CAP、quorum 与一致性模型

这一章回答复制、共识、CAP、quorum 和一致性模型相关问题。面试里这类题很容易被答成几个口号：CAP 只能三选二、`R + W > N` 就一定读到最新值、eventual consistency 就是最终会一致。这样的回答太粗，真正要讲清楚的是定义、前提、失效场景和工程边界。

下面的回答主要参考 Gilbert 与 Lynch 对 CAP 的形式化讨论、Herlihy 与 Wing 的 linearizability 论文、Lamport 关于 happened-before 和 sequential consistency 的论文、Amazon Dynamo 论文、Cassandra 官方文档、PostgreSQL 事务隔离文档、Raft 论文，以及 Werner Vogels 对 eventual consistency 的整理。结合 LogServe 时，我会主动把边界说清楚：当前项目是单机多进程 shared-log AI runtime，用 log-first、replay、redelivery、actor epoch fencing 验证机制；它没有声称已经实现多机共识、跨机 quorum shared log 或跨 region 容灾。

## Q001. CAP 定理的准确表述是什么？

CAP 最准确的说法不是“分布式系统只能在一致性、可用性、分区容忍性里选两个”。这句话流传很广，但它会误导人，以为 P 是一个可以主动选择的功能开关。更准确的表述是：

```text
在可能发生通信故障的网络中，一个分布式服务不可能同时实现原子读写语义，并且保证每个请求都最终收到响应。
```

这里的“原子读写语义”基本对应 CAP 语境下的 consistency，也就是读写寄存器看起来像在一个单点上按顺序执行。后来的系统讨论里通常把它近似理解为 linearizability：一旦某个写操作成功返回，之后开始的读必须看到这个写，或者看到更新的写。

CAP 里的三个词要按形式化语义理解。

Consistency 指的是服务对外表现得像一个单副本对象。对于最简单的读写寄存器，读操作必须返回符合某个顺序执行历史的值，并且这个顺序还要尊重真实时间上的先后关系。它不是“所有副本任何时刻字节完全相同”，也不是数据库约束意义上的 ACID consistency。

Availability 指的是每个发给非故障节点的请求最终都要得到响应。注意这里没有说响应一定包含最新值。只要系统选择了可用性，它就不能在分区期间一直等待另一侧确认，否则请求就没有最终响应。

Partition tolerance 指的是网络可能把节点分成几个互相通信不了的部分，或者消息被无限期延迟、丢失。它不是业务层想不想要的能力，而是分布式环境里必须面对的故障模型。只要系统跨进程、跨机器、跨机房通信，就要考虑消息可能迟到、丢失、乱序、重复，或者一段时间内完全不可达。

因此 CAP 的真实含义是：当分区真的发生时，如果你还要求请求必须继续返回，就不能同时保证强一致读写；如果你要保证强一致读写，就必须让某些请求等待或失败，从而牺牲可用性。

可以用一个最小例子说明。假设一个 key 有两个副本 A 和 B，最初值是 0。客户端 C1 连到 A，把值写成 1，并且 A 返回成功。随后网络分区，A 和 B 不能通信。另一个客户端 C2 连到 B 发起读请求。

这时 B 面临两个选择：

```text
返回 0：C2 得到响应，系统保持可用，但读到了旧值，破坏强一致。
等待 A 恢复：不返回错误值，但 C2 一直得不到响应，破坏 CAP 意义上的可用性。
```

这个例子里的难点不是 B “不够聪明”。在异步网络里，B 无法区分“只是消息很慢”和“A 那边已经发生了成功写”。如果它必须响应，就可能错；如果它必须不错，就可能不响应。

面试时我会避免把 CAP 讲成静态标签，比如“这个系统是 CP”“那个系统是 AP”。更准确的说法是：系统在分区期间为不同操作选择了不同策略。有些系统对写选择拒绝，对读选择只读旧快照；有些系统允许两边继续写，分区恢复后再做冲突合并；有些系统把元数据放在强一致路径，把缓存、计数、推荐列表放在弱一致路径。大型系统通常不是整站一个 CAP 选择，而是每条数据路径有自己的取舍。

对 LogServe 来说，当前 shared log 是单机 logd 提供的 append-only 日志，不是多副本 quorum log。所以我不能说它已经解决了 CAP 问题。它验证的是 log-first、replay、worker redelivery 和 actor fencing 这些机制在单机多进程故障下能成立。如果将来把 logd 扩展成多副本 shared log，就需要选择 Raft/Paxos 这类共识，或者 Dynamo/Cassandra 风格的可调 quorum，两条路的 CAP 取舍完全不同。

## Q002. CAP 中的 consistency 和数据库 ACID consistency 是同一概念吗？

不是同一概念。它们都叫 consistency，但回答时必须拆开，否则很容易混淆。

CAP 中的 consistency 讲的是分布式对象对客户端的可见顺序。简单说，多个副本对外要像一个副本。一个写操作成功返回后，后续读不能绕过这个写去读旧值。它关心的是请求/响应历史、真实时间顺序和副本之间的协调。

ACID 中的 consistency 讲的是事务前后数据库是否满足约束和业务不变量。比如账户转账前后总金额不变，外键不能指向不存在的行，唯一索引不能重复，余额不能小于 0。它更像“一个正确事务把数据库从一个合法状态带到另一个合法状态”。其中很多不变量其实要靠应用事务逻辑保证，数据库只能用 constraint、trigger、foreign key、unique index 等机制辅助。

举个例子，一个单机 PostgreSQL 数据库可以很好地保证 ACID consistency。你在一个事务里从账户 A 扣 100，再给账户 B 加 100，提交后约束仍然成立。这个语义不需要多副本网络，也不涉及网络分区。

但 CAP consistency 问的是另一个问题：

```text
如果这个账户数据复制到多个节点，
写入节点 A 成功返回后，
客户端马上去节点 B 读，
B 是否必须看到 A 刚刚提交的写？
```

这是副本可见性问题，不是单个事务是否维护业务约束的问题。

还要注意，数据库里很多人把“强一致”混在事务隔离里说。严格讲，事务隔离的 Serializable 是说并发事务提交后的效果等价于某个串行顺序。PostgreSQL 文档也把 Serializable 描述为最严格事务隔离级别，效果像事务一个接一个执行。这个概念和 linearizability 有交集，但不相同。serializability 不一定要求尊重真实时间顺序；linearizability 要求如果操作 A 完成后操作 B 才开始，那么 B 的线性化顺序必须在 A 后面。

可以用这张表区分：

| 概念 | 主要对象 | 核心问题 | 典型检查方式 |
|---|---|---|---|
| ACID consistency | 一个数据库事务 | 事务提交后是否保持约束和业务不变量 | constraint、trigger、事务代码、应用校验 |
| Isolation/serializability | 一组并发事务 | 并发事务效果是否等价于某个串行执行 | 隔离级别、冲突检测、谓词锁、事务重试 |
| CAP consistency | 分布式服务的请求历史 | 多副本读写是否像单副本原子对象 | quorum、leader、共识、linearizability checker |
| Replica convergence | 多副本后台状态 | 如果没有新写，副本最终是否收敛 | read repair、anti-entropy、Merkle tree、hinted handoff |

所以面试时如果被问“CAP 的 C 和 ACID 的 C 是不是一个东西”，我会直接说：

```text
不是。ACID consistency 主要是事务前后数据库合法性和业务不变量；CAP consistency 是分布式读写历史是否像单副本原子对象。ACID 的 C 可以在单机数据库里讨论，CAP 的 C 必须放在多副本、网络故障和请求响应历史里讨论。事务隔离里的 Serializable 又是第三个概念，它关心并发事务是否等价于某个串行顺序，不自动等于 linearizability。
```

这个区分很实用。很多系统文档说“支持 ACID”，不代表它在跨 region 读写上提供 linearizability；很多 NoSQL 系统说“eventual consistency”，也不代表它完全没有事务约束。要看具体操作、具体 key、具体副本范围和具体隔离级别。

## Q003. 网络分区为什么是分布式系统必须面对的问题？

因为分布式系统的通信不是本地函数调用。只要节点之间靠网络传消息，消息就可能延迟、丢失、重复、乱序，链路也可能单向不可达。应用层看到的通常只是 timeout，而 timeout 不能证明对端没有执行请求。

这是分布式系统最麻烦的一点：观察者看到的是局部事实。客户端请求 leader 超时，可能有几种情况：

```text
请求根本没到 leader。
请求到了，leader 执行了，但响应丢了。
leader 写了本地日志，还没复制到多数派。
leader 已经提交了，但客户端连接断了。
leader 所在分区仍然能和多数派通信，只是客户端到 leader 的网络坏了。
leader 已经失去多数派，另一个分区选出了新 leader。
```

如果系统把这些情况都粗暴当成“失败后重试”，就会碰到重复写、迟到完成、双 leader、stale read、lost update。分布式系统设计里大量机制都是为这个问题服务的：幂等键、fencing token、epoch、term、lease、quorum、vector clock、read repair、共识日志、事务重试。

网络分区必须面对，还有一个工程原因：大规模系统里小概率事件会变成日常事件。Dynamo 论文说得很直接，大规模基础设施里总有一小部分服务器或网络组件正在失败。跨机房、跨可用区、跨 region 时，链路抖动、路由异常、防火墙误配、负载均衡问题、DNS 问题都可能表现成分区。

分区不一定是“整个机房断网”这种大事故。更常见的是局部分区：

```text
A 能访问 B，B 访问不了 A。
客户端能访问两个副本，但两个副本彼此不能通信。
leader 能和少数 follower 通信，不能和多数派通信。
某个机架到对象存储慢，其他机架正常。
跨 region 写路径超时，但本 region 读路径正常。
```

这就是 partial failure：系统不是一起成功或一起失败，而是某一部分失败、某一部分还在继续服务。单机程序里崩溃通常比较干脆；分布式系统里更危险的是“半死不活”。节点可能还在响应健康检查，却已经无法访问存储；worker 可能已经被控制面判定超时，但稍后又把旧结果提交回来；副本可能还活着，却落后了几分钟。

因此，一个认真设计的分布式系统不能把网络分区当成罕见异常。它至少要回答这些问题：

```text
分区期间哪一侧可以接收写？
不能确认最新状态时，读是返回旧值、报错、还是等待？
请求超时后客户端能否安全重试？
两个分区都接收写后，恢复时如何合并冲突？
旧 leader、旧 owner、旧 worker 恢复后还能不能写入？
系统怎样证明一个写已经提交，靠单节点 ack 还是多数派 ack？
```

如果系统选择强一致路径，通常会用共识或严格 quorum。比如 Raft 用多数派复制日志，只要多数派可通信就继续服务；失去多数派的一侧不能提交新日志。这样牺牲了一部分可用性，但保住了日志顺序和安全性。

如果系统选择高可用路径，通常会允许部分副本继续接受写，再用版本、冲突合并、read repair、anti-entropy 修复差异。Dynamo 和 Cassandra 这类系统就是这个方向。它们不会免费得到强一致，应用要理解 stale read、conflict resolution、inconsistency window。

LogServe 当前的边界要讲清楚：项目里的 worker timeout、redelivery、actor epoch fencing 属于单机多进程 partial failure 处理。它能说明我理解“timeout 不是失败证明，旧 worker 可能迟到提交”这个问题，所以用了 epoch/fencing 保护结果写入。但它还没有进入“多个 log 副本之间发生网络分区”的层面。多机化以后，shared log 本身就必须有共识或 quorum 语义。

## Q004. strong consistency、eventual consistency、causal consistency 的区别是什么？

这三个模型的差别，核心在于系统对“读应该看到哪些写”做了多强的承诺。

Strong consistency 在工程讨论里经常指 linearizability。它的意思是：写操作成功返回后，所有后续开始的读都必须看到这个写或更新的写。客户端不需要关心自己读的是哪个副本，系统对外像单副本。代价是副本之间要同步协调，分区或多数派不可用时，一些请求会等待或失败。

Eventual consistency 的承诺弱得多。它说：如果某个对象之后没有新写入，系统最终会收敛，所有副本最终会返回同一个最新值。它不保证写成功后马上可见，也不保证中间读不会看到旧值。它真正承诺的是最终收敛，不是任意时刻正确。

Causal consistency 介于两者之间。它要求系统保留因果顺序：如果写 B 因果依赖写 A，那么看到 B 的客户端也必须能看到 A；如果两个写没有因果关系，系统可以用不同顺序展示它们，或者让冲突合并逻辑处理。Lamport 的 happened-before 关系正是理解因果顺序的基础：同一进程内先后发生的事件有因果顺序，消息发送先于消息接收，因果关系可以传递。

一个评论系统的例子很直观。

```text
W1: Alice 发帖：“今天发布了新版本。”
W2: Bob 看到 W1 后回复：“这个版本修好了昨天的 bug。”
W3: Carol 没看到 W1，独立发帖：“我这边还有一个问题。”
```

W2 依赖 W1。因果一致系统不能让某个读者只看到 Bob 的回复却看不到 Alice 的原帖。W3 和 W1/W2 之间没有因果关系，系统可以先显示 W3，也可以后显示 W3，只要不破坏已经存在的依赖。

再看三种模型下的读：

| 模型 | 写成功后立即读 | 多副本中间状态 | 典型代价 |
|---|---|---|---|
| strong consistency | 必须看到最新已完成写 | 对外隐藏副本差异 | 协调成本高，分区时可能不可用 |
| eventual consistency | 可能读到旧值 | 允许短期不一致，最终收敛 | 应用要处理 stale read 和冲突 |
| causal consistency | 必须保留因果依赖 | 无因果关系的并发写可不同序 | 需要跟踪依赖，如版本向量、session token |

eventual consistency 还有一些实用变体，面试时可以顺手提到：

```text
read-your-writes：同一个客户端写完后自己不会读到旧值。
monotonic reads：同一个客户端读到某个版本后，以后不会倒退到更旧版本。
session consistency：在一个 session 内保证 read-your-writes 或 monotonic reads。
monotonic writes：同一个客户端的写按提交顺序生效。
```

这些变体很重要，因为纯 eventual consistency 对应用开发者太不友好。用户刚改完头像，下一页又看到旧头像，理论上可以说最终会收敛，但体验很差。很多系统会用 session stickiness、版本 token、读主副本、read repair 来提供比纯 eventual 更好一点的用户可见语义。

工程上选择哪一种，要看数据类型。

适合 strong consistency 的数据：

```text
锁、lease、leader election、权限变更、余额、库存扣减、唯一 ID 分配、任务 ownership。
```

适合 eventual consistency 的数据：

```text
搜索索引、推荐结果、浏览量计数、缓存副本、排行榜、离线报表、用户可容忍短暂延迟的展示字段。
```

适合 causal consistency 的数据：

```text
聊天消息、评论回复、协作文档操作、社交动态、带上下文依赖的用户操作流。
```

如果结合 LogServe，我会这样落地：control 面的 task 状态、actor ownership、command_seq、epoch fencing 更接近强一致路径，不能让旧 worker 覆盖新 owner 的状态；dashboard snapshot、统计指标、benchmark 汇总可以接受延迟和最终收敛。LLM checkpoint cache 的命中状态也不应该放在强一致核心路径里，因为它影响调度质量，不应该影响任务语义正确性。

## Q005. linearizability 是什么？

Linearizability 是并发对象的正确性条件。它要求每个操作看起来都在调用和返回之间的某个瞬间生效，这个瞬间通常叫 linearization point。所有操作按这些点排成一个顺序后，结果必须符合对象的顺序规格，并且这个顺序要尊重真实时间：如果操作 A 已经返回，操作 B 才开始，那么 A 必须排在 B 前面。

这句话可以拆成三层。

第一，操作有时间区间。一次读或写不是一个点，而是从 invocation 到 response 的一段时间。linearizability 允许并发操作在这段重叠时间里以任意合法顺序排列。

第二，每个操作要能找到一个生效点。比如一个并发队列的 `Enqueue(x)` 和 `Dequeue()` 重叠执行，如果 `Dequeue()` 返回了 `x`，那么可以认为 `Enqueue(x)` 在它返回前某个时刻已经生效。这个时刻必须在 `Enqueue` 的调用和返回之间。

第三，非重叠操作必须尊重真实时间。这个要求是 linearizability 比 sequential consistency 更强的地方。写 `x=1` 已经成功返回后，另一个客户端再读 `x`，就不能读到旧值 0。

一个寄存器例子：

```text
t1: C1 调用 write(x=1)
t2: C1 的 write 返回成功
t3: C2 调用 read(x)
t4: C2 的 read 返回
```

如果 `t2 < t3`，那么 `read(x)` 必须返回 1 或者更晚写入的值。返回 0 就不是 linearizable。

如果两个操作重叠，则系统有选择空间：

```text
t1: C1 调用 write(x=1)
t2: C2 调用 read(x)
t3: C2 的 read 返回 0
t4: C1 的 write 返回成功
```

这个历史可能是 linearizable，因为读和写重叠，可以把 read 的 linearization point 放在 write 生效前。客户端看到 read 返回 0，不违反真实时间，因为写还没返回。

面试里要强调，linearizability 是外部可观察语义，不是实现方法。你可以用单 leader、mutex、Raft、Paxos、严格 quorum、数据库锁来实现它；也可以实现失败。关键不是用了什么组件，而是请求历史能不能被解释成一个尊重真实时间的合法顺序。

Linearizability 的工程价值很大：

```text
用户模型简单：读写像访问单机对象。
组合推理更容易：对象行为可以按顺序规格理解。
适合协调类数据：锁、leader、lease、任务 ownership、actor command_seq。
便于测试：可以用 history checker 检查并发读写是否存在合法线性化。
```

代价也明显：

```text
需要协调副本，写延迟通常高于本地写。
分区时强一致侧可能拒绝请求。
跨 region linearizability 会把远距离 RTT 放进关键路径。
热点 key 会受单 leader、单 shard 或 quorum 写路径限制。
```

LogServe 里的 actor mailbox 可以拿来类比。单机版本中，同一个 actor 的 command 必须按 `command_seq` 串行应用；旧 epoch worker 的完成要被拒绝。这个设计目标接近“对同一个 actor 的状态更新呈现单序”。但因为它不是多副本服务，所以不能把它说成跨节点 linearizable actor store。正确表述是：当前实现用 command_seq 和 epoch fencing 在单机控制面内维护 actor 状态机顺序，为将来多副本线性化语义打基础。

## Q006. linearizability 和 serializability 的区别是什么？

Serializability 主要来自数据库事务语境。它说一组并发事务执行后的效果，等价于这些事务按某个串行顺序一个一个执行。它关心的是事务集合的结果是否可串行解释。

Linearizability 主要来自并发对象和分布式存储语境。它说每个操作要在调用和返回之间瞬时生效，整个历史要等价于某个合法顺序，并且要尊重真实时间。

两者最大的区别是：linearizability 有真实时间约束，serializability 通常没有。

举个例子。两个事务：

```text
T1: write(x=1)，提交成功
T2: read(x)，返回 0
```

如果 T1 和 T2 并发执行，serializability 可以把 T2 排在 T1 前面，所以 T2 读到 0 可能没问题。

但如果 T1 已经提交并返回，T2 才开始，那么 linearizability 不允许 T2 读到 0。Serializable 数据库如果还额外保证真实时间顺序，通常称为 strict serializability。很多分布式数据库宣传“external consistency”或“strict serializability”，说的就是 serializability 加真实时间顺序。

第二个区别是粒度。serializability 的单位通常是事务，一个事务可能读写多行、多表、多对象。linearizability 的经典定义通常先讨论单个对象或单个寄存器；它也可以扩展到多对象操作，但工程上常说“这个 key 的读写是 linearizable”“这个对象的操作是 linearizable”。

第三个区别是关注点。serializability 更关心并发控制和事务隔离，比如脏读、不可重复读、幻读、写偏斜、序列化异常。linearizability 更关心副本、请求响应历史、真实时间顺序和客户端观察。

可以用表格总结：

| 维度 | Serializability | Linearizability |
|---|---|---|
| 常见语境 | 数据库事务隔离 | 并发对象、分布式 KV、寄存器 |
| 操作单位 | transaction | object operation / request |
| 核心要求 | 等价于某个串行事务顺序 | 等价于某个顺序历史，并尊重真实时间 |
| 是否要求真实时间 | 默认不要求 | 要求 |
| 典型反例 | write skew、serialization anomaly | stale read after completed write |
| 加强版 | strict serializability | 本身已经带真实时间约束 |

PostgreSQL 的 Serializable 隔离级别是很好的数据库例子。它让成功提交的并发事务效果像串行执行，如果发现无法串行化的依赖，就让某个事务失败并要求重试。这个语义解决的是事务并发异常，不是自动保证你跨多个异步副本读取时总能看到最新副本。

面试时我会这样说：

```text
serializability 是事务调度正确性：并发事务结果能不能解释成某个串行顺序。linearizability 是对象操作的实时一致性：每个操作能不能放在调用和返回之间的某个点上，并且已经完成的操作必须排在后续操作之前。serializability 不默认尊重真实时间；linearizability 尊重真实时间。serializability 加真实时间约束，才接近 strict serializability。
```

这一区分在设计系统时很关键。一个数据库可以对单分片事务提供 serializable，但跨 region follower read 不一定 linearizable。一个 KV 系统可以对单 key 提供 linearizable read/write，但不一定支持多 key serializable transaction。把两个词混用，会高估系统能力。

## Q007. sequential consistency 和 linearizability 的区别是什么？

Sequential consistency 是 Lamport 在多处理器内存模型里提出的概念。它要求执行结果等价于所有处理器操作按某个顺序执行，并且每个处理器自己的程序顺序在这个全局顺序里保持不变。

Linearizability 比 sequential consistency 更强。它除了要求有一个合法顺序，还要求这个顺序尊重真实时间。也就是说，一个操作已经完成后，另一个操作才开始，后者不能被排到前者前面。

区别可以用同一个例子说明。初始 `x=0`：

```text
P1: write(x=1)  完成
P2: read(x)     在 write 完成之后才开始，返回 0
```

这个历史不是 linearizable，因为读开始时写已经完成，读不能返回旧值。

它是否 sequentially consistent？如果只看 P1 和 P2 各自的程序顺序，可以把 P2 的 read 排在 P1 的 write 前面。因为 P1 只有一个写，P2 只有一个读，各自程序顺序没有被破坏。所以它可以满足 sequential consistency。

再看一个同进程读写：

```text
P1: write(x=1) 完成
P1: read(x) 返回 0
```

这连 sequential consistency 也不满足，因为同一个进程内 write 在 read 前，任何保持 P1 程序顺序的全局顺序里，read 都应该看到 1 或更新值。

所以两者可以这样记：

```text
sequential consistency：尊重每个进程自己的顺序。
linearizability：尊重每个进程自己的顺序，还尊重跨进程的真实时间先后。
```

这个区别看起来细，但对客户端体验很重要。Sequential consistency 允许一个客户端 A 写成功后，客户端 B 稍后读到旧值，只要系统能找出某个不尊重真实时间的全局顺序解释它。对分布式存储用户来说，这通常不符合“写完就应该被后续读看到”的直觉。

Linearizability 更贴近用户直觉：已经返回成功的操作就像真的发生了。代价是实现要更保守。系统需要 leader lease、quorum read、read index、commit index、fencing token 等机制，避免读到未提交或过期副本。

面试回答可以这样落：

```text
Sequential consistency 和 linearizability 都要求结果能解释成某个顺序执行历史。区别是 sequential consistency 只保留单个进程的程序顺序，不要求跨进程真实时间顺序；linearizability 要求如果操作 A 在真实时间上完成于操作 B 开始之前，A 必须排在 B 前。因此 linearizability 更强，也更符合分布式服务用户对“写成功后再读”的直觉。
```

如果面试官继续追问“为什么还要 sequential consistency”，可以补一句：它在硬件内存模型和某些并发系统里仍然有价值，因为它比 linearizability 更容易实现，也能支持很多程序推理。但对外部存储服务来说，很多场景需要 linearizability 或 strict serializability，否则用户会看到违反直觉的旧读。

## Q008. quorum read/write 的基本思想是什么？

Quorum read/write 的基本思想是：一个数据项复制到 N 个副本上，写操作不必等所有副本都写完，只要等 W 个副本确认就算成功；读操作也不必读所有副本，只要读 R 个副本，然后根据版本选择最新值。只要读集合和写集合必然有交集，读就有机会看到已成功写入的版本。

三个参数：

```text
N: replica count，一个 key 有多少副本。
W: write quorum，写成功前至少要多少副本确认。
R: read quorum，读返回前至少要多少副本响应。
```

经典条件是：

```text
R + W > N
```

因为在一个大小为 N 的集合里，任意 W 个写副本和任意 R 个读副本必然至少重叠一个副本。这个重叠副本如果已经持久化了最新成功写，读操作又能比较版本，就能把最新值读出来。

常见配置：

| N | W | R | 含义 |
|---|---|---|---|
| 3 | 2 | 2 | 读写都走多数派，常见平衡配置 |
| 3 | 3 | 1 | 写慢读快，适合读多但要求读最新 |
| 3 | 1 | 3 | 写快读慢，读时需要扫所有副本 |
| 5 | 3 | 3 | 可容忍少数副本故障，读写多数派 |
| 3 | 1 | 1 | 高可用低延迟，但不能保证读到最新 |

Quorum 不是只数响应个数，还需要版本规则。读到 R 个响应后，如果副本返回了不同版本，协调者必须知道哪个版本更新。这个版本可以是单调递增的 log index、term/index、timestamp、hybrid logical clock、vector clock，或者应用可合并的版本集合。没有版本，读到多个值也不知道哪个是新。

写路径一般是：

```text
client -> coordinator
coordinator 生成版本 v
coordinator 把 value@v 写到 N 个副本中的若干个
收到 W 个成功 ack 后返回 client 成功
后台继续补齐剩余副本，或以后通过 read repair / anti-entropy 修复
```

读路径一般是：

```text
client -> coordinator
coordinator 向副本发 read
收到 R 个响应后比较版本
返回最高版本，或返回多个并发版本让应用合并
可选：把新版本 read repair 到落后副本
```

Quorum 的价值是让系统在一致性、延迟和可用性之间调参。写不等所有副本，延迟可以降低；读不扫所有副本，读延迟可以降低；只要 R/W 配得合适，又能保留交集语义。Cassandra 官方文档把这称为 tunable consistency，应用可以按操作选择 `ONE`、`QUORUM`、`ALL`、`LOCAL_QUORUM` 等级。

但 quorum 不是共识的完整替代。普通 Dynamo 风格 quorum 更像“读写副本交集 + 版本合并”。它不自动提供全局日志顺序，不自动解决并发写冲突，也不自动给多 key 事务 serializability。Raft/Paxos 这类共识通常是让多数派对同一条日志顺序达成一致，语义更强，代价也更集中。

面试时可以这样回答：

```text
quorum read/write 把一个 key 复制到 N 个副本。写只等 W 个副本确认，读只等 R 个副本响应。如果 R + W > N，任意成功写集合和后续读集合一定有交集。读协调者从 R 个响应里选择版本最高的值，并可做 read repair。它的核心不是魔法，而是集合交集加版本比较。这个结论依赖固定副本集合、可靠版本顺序、写 ack 后持久化、读会比较所有 R 个响应等前提。
```

结合 LogServe，如果未来 shared log 做多副本，有两条常见路：用 Raft 管理 replicated log，让日志条目按 leader 和多数派提交；或者做 quorum append/read，但必须定义 log record 的 index、term、冲突处理和 recovery。当前单机 logd 不能直接套 `R/W/N`，因为它只有一个持久化日志副本。

## Q009. R + W > N 为什么可以读到最新写？这个结论有哪些前提？

`R + W > N` 的数学部分很简单：在 N 个副本里，任意 W 个副本组成的写集合，和任意 R 个副本组成的读集合，必然有交集。否则两个集合总大小最多是 N；现在总大小大于 N，所以至少重叠一个副本。

用 `N=3, W=2, R=2` 举例：

```text
副本：A, B, C
写成功：写到了 A, B
后续读：读任意两个副本

读 A,B -> 看到新值
读 A,C -> A 有新值
读 B,C -> B 有新值
```

所以只要写成功的版本真的存在于 W 个副本上，后续读到 R 个副本时至少会碰到一个拥有该版本的副本。

但“可以读到最新写”有很多前提。面试里如果只背公式，很容易被追问倒。

第一个前提：读写必须针对同一个固定 replica set。写集合和读集合都要从同一个 N 里选。如果写时用了 sloppy quorum，把副本写到了临时替代节点；读时又从原始 replica set 读，交集就不一定存在。

第二个前提：写成功前，W 个副本已经持久保存了该版本。不能是 coordinator 收到内存 ack 就返回，随后副本崩溃丢失。否则数学上有交集，实际上交集副本没有值。

第三个前提：读协调者必须等待 R 个响应，并比较版本后再返回。读到第一个响应就返回，可能刚好读到落后副本。Quorum 的语义来自“R 个响应的集合”和“选择最新版本”，不是来自“发了 R 个请求但先回一个就返回”。

第四个前提：版本之间必须可比较。单写者场景可以用递增版本号。多写者场景要用 leader 分配序号、共识日志 index、timestamp/HLC、vector clock，或者返回 siblings 给应用合并。如果两个写并发发生，没有全序版本，读到“最新”这个词本身就不明确。

第五个前提：没有未处理的并发写冲突。假设两个客户端同时写：

```text
W1: x = 1 写到 A, B
W2: x = 2 写到 B, C
```

这两个写都可能成功。后续读 A,C 会看到 1 和 2。哪个是最新？如果系统有全序 timestamp，可以选一个；如果用 vector clock，可能发现它们并发，必须返回两个版本或做业务合并。`R + W > N` 保证读集合与每个成功写集合有交集，不保证并发写天然有单一正确值。

第六个前提：副本 membership 和拓扑视图一致。扩容、缩容、rebalance、跨机房复制时，如果不同协调者对 N 的理解不一致，读写可能落到不同集合。成熟系统要用 epoch、ring version、配置变更协议或 joint consensus 处理成员变更。

第七个前提：删除、TTL、tombstone 也要版本化。否则一个副本返回“没有这个 key”，另一个副本返回旧值，读协调者可能误把旧值当成有效值。Cassandra 这类系统里 tombstone、repair、gc_grace 之所以复杂，就是因为删除也是写，也要参与一致性和反熵。

第八个前提：跨数据中心的 local quorum 只在本地副本集合内成立。`LOCAL_QUORUM` 可以保证本 datacenter 的多数派交集，不等于全球强一致。另一个 region 的写如果还没复制过来，本地 quorum 读可能读不到。

因此，更严谨的结论应该这样说：

```text
在固定 N 副本、严格 quorum membership、写成功表示 W 个副本持久化、读等待 R 个响应、读端选择最高可比较版本、没有未解决并发写冲突、成员视图一致的前提下，R + W > N 能保证后续读集合和成功写集合相交，因此读端可以看到该成功写的版本。
```

这个结论不是说系统自动 linearizable。要达到 linearizability，还要处理读写并发、leader 变更、时钟不准、成员变更、旧 leader、重试和 duplicate request。如果系统只是 Dynamo 风格 quorum，通常更准确地说它提供“quorum-like consistency”或“tunable consistency”，不等于完整共识。

面试官如果问“那为什么 Cassandra `QUORUM` 读写还可能读到奇怪结果”，可以从几个方向答：last-write-wins 依赖 timestamp，时钟偏差会影响冲突解决；跨 datacenter 的 local quorum 不是 global quorum；repair 只是 best effort；并发写可能按 timestamp 覆盖；删除和 tombstone 有自己的传播窗口。公式给的是集合交集，系统语义还取决于版本、冲突、时间和修复机制。

## Q010. sloppy quorum 会牺牲什么语义？

Sloppy quorum 的目标是提高可用性。传统 quorum 要求读写发生在某个 key 的前 N 个负责副本上；如果其中一些副本不可达，写就可能失败。Sloppy quorum 放宽这个要求：当原本应该负责的副本不可用时，系统把写发给 preference list 里靠后的健康节点，由这些临时节点保存 hinted replica，等原副本恢复后再 hinted handoff 回去。

Dynamo 的典型例子是 `N=3`，key 本来应该存在 A、B、C。现在 A 不可达，写不直接失败，而是写到 B、C、D。其中 D 不是这个 key 的正常副本，它只是临时保存属于 A 的那份数据，并带着 hint：这份数据最终要交给 A。

这会牺牲几个语义。

第一，牺牲严格 quorum membership。传统 `R + W > N` 的交集证明依赖读写都在同一个固定 N 副本集合里。Sloppy quorum 写到了替代节点，后续读如果只读原始 A、B、C，未必能碰到临时节点 D。交集从“数学保证”变成“实现尽量找健康节点并靠 hinted handoff 修复”。

第二，牺牲“成功写马上可被正常副本 quorum 读到”的语义。写成功可能只是 B、C、D 收到了，其中 A 没收到。如果后续读路径因为拓扑、协调者、故障视图不同，没有读到 D，就可能看不到刚才成功的写。

第三，牺牲单调读和 read-your-writes 的直觉，除非额外做 session stickiness 或版本 token。用户写成功后马上读，如果读请求落到一个没有 hint、也没完成 handoff 的副本集合，可能读到旧值。应用可以通过读写同一 coordinator、携带版本、读更高 consistency level、或读 repair 降低风险，但 sloppy quorum 本身不免费提供这些语义。

第四，冲突更常见。分区期间不同侧都能接受写，恢复后可能出现 siblings 或 last-write-wins 覆盖。Dynamo 选择把很多冲突推给读路径和应用合并，例如购物车可以合并多个版本；但余额、库存、锁这类数据不能随便合并。

第五，durability 的含义变复杂。Sloppy quorum 写到临时节点可以提高短期 durability，因为原副本挂了也有替代节点保存数据。但如果 hint 所在节点在 handoff 前也坏了，或者 hint 存储没有足够持久化，数据仍可能丢。它提高的是某些故障下的写可用性和临时冗余，不是严格副本布局下的持久化承诺。

第六，运维和恢复语义更复杂。系统必须追踪 hint、重放 hint、处理目标副本恢复、处理重复 handoff、处理过期 hint、处理已经被更新或删除的数据。hinted handoff 失败后还要靠 anti-entropy repair、Merkle tree 等后台机制收敛。

可以用表格对比：

| 维度 | Strict quorum | Sloppy quorum |
|---|---|---|
| 写入目标 | key 的固定 N 个负责副本 | preference list 中前 N 个健康节点 |
| 交集证明 | `R + W > N` 可直接证明 | 交集依赖实际访问集合，证明变弱 |
| 分区期间写可用性 | 可能拒绝写 | 更倾向接受写 |
| 读最新值 | 条件满足时较强 | 可能读不到临时节点上的新值 |
| 冲突处理 | 相对少，但仍要处理并发写 | 更多依赖版本与合并 |
| 恢复机制 | repair/read repair | hinted handoff + repair |

面试时我会这样回答：

```text
sloppy quorum 用临时健康节点替代原始副本来接收读写，所以提高了分区或节点故障下的可用性和写成功率。它牺牲的是严格 replica-set quorum 的交集语义：R + W > N 不再自动保证读集合和写集合在原始 N 副本中相交。结果是成功写可能暂时只存在于替代节点上，后续正常读不一定马上看到；read-your-writes、monotonic read、linearizability 都需要额外机制。恢复时还要依赖 hinted handoff、read repair 和 anti-entropy，把临时副本交回原目标副本。
```

Sloppy quorum 适合“写不能轻易拒绝，冲突可以合并”的场景，比如购物车、用户偏好、某些缓存或日志型数据。不适合锁、leader election、账户余额、严格库存这类需要单一顺序的协调数据。后者更适合共识或严格事务路径。

对 LogServe 来说，如果未来为了高可用把 task result、actor command 或 shared log entry 写到 sloppy quorum，就必须非常谨慎。任务完成、actor 状态和日志顺序不是天然可合并的。更合理的路线是：核心日志顺序走共识或严格 quorum；可重建的缓存、指标、dashboard snapshot 可以使用更弱的一致性。

## Q011. read repair 和 hinted handoff 解决什么问题？

read repair 和 hinted handoff 都是在多副本系统里缩短不一致窗口的机制，但它们站的位置不同。read repair 在读路径上修，hinted handoff 在写路径或节点恢复路径上补。

read repair 的触发点是读请求。协调节点向多个副本读数据，可能先读一个完整数据，再向其他副本读 digest；如果 digest 或版本不一致，协调节点会拉取完整数据，选出最新版本返回给客户端，并把落后的副本修到新版本。Cassandra 文档里强调，blocking read repair 会在返回客户端前完成必要修复，用来支持 monotonic quorum reads，也就是连续两次 quorum 读不应该第二次比第一次更旧。

它解决的问题是：某些副本因为写入时短暂不可达、写只达到 quorum、后台 repair 未覆盖、跨机房复制延迟等原因落后了。热点 key 被读到时顺便修，能让热点数据更快收敛。

但 read repair 不是万能 repair。它通常只修本次读涉及的副本和数据范围，没有被读到的副本仍可能落后；blocking repair 会增加读延迟；如果一次写覆盖一个 partition 的多行，而读只读其中一行，按读范围修复还可能影响 partition-level write atomicity。所以 Cassandra 允许在 monotonic quorum read 和写原子性之间做表级配置取舍。

hinted handoff 的触发点是写请求。假设一个 key 应写到 A、B、C，C 短暂不可用。协调节点如果从 A、B 得到足够 ack，就可以先返回成功，并保存一个 hint：这条 mutation 原本属于 C。C 恢复后，协调节点把 hint 发送给 C，让它补上错过的写。

它解决的问题是：副本短时间离线时，不要让整个写失败，也不要等全量 repair 才能追平。它提高了短故障下的写可用性和恢复速度。

边界也要讲清楚。hint 有保留窗口，节点离线太久仍要 full repair、rebuild 或 bootstrap；hint 丢失、handoff 限流、协调节点故障都会延长不一致；hinted handoff 也不解决并发写冲突，最终仍要靠 timestamp、版本向量、tombstone 或应用合并。

面试可以这样答：

```text
read repair 是读时发现副本不一致后，把读到的落后副本修到较新版本；它改善热点数据收敛和 quorum 读单调性，但会增加读延迟，也只覆盖本次读范围。hinted handoff 是写时目标副本短暂不可用，协调节点先保存 hint，等副本恢复后补交 missed mutation；它提高短故障下的写可用性，但不是强一致协议，不能替代 full repair。
```

放到 LogServe 上，read repair 更像从 shared log 重建落后的 materialized view；hinted handoff 更像保存某个恢复后要补投递的事件。但当前 LogServe 的 shared log 是单机真相源，不是 Cassandra 式多副本 eventually consistent store，不能宣称已经实现这两套机制。

## Q012. leader-follower replication 的基本流程是什么？

Leader-follower replication 的基本思路是：写入口集中到 leader，leader 决定顺序，再把变更复制给 followers。followers 按 leader 的顺序重放日志或 WAL。这样系统把“谁来决定写顺序”这个问题收束到一个节点上。

典型流程是：

```text
1. client 把写请求发给 leader。
2. leader 校验请求，分配 LSN、log index、sequence 或 timestamp。
3. leader 追加本地日志，或写入 WAL。
4. leader 把日志记录发给 followers。
5. followers 写入自己的日志，并按顺序 apply 到状态机或数据文件。
6. leader 根据复制策略决定何时向客户端返回成功。
7. follower 掉线后从上次位置继续追赶；太落后时可能需要 snapshot 或 base backup。
```

PostgreSQL streaming replication 是数据库例子：primary 生成 WAL，standby 通过流复制接收 WAL 并 replay。Raft 是共识例子：leader 收到 command 后追加 log entry，发 AppendEntries 给 followers，entry 复制到多数派后推进 commit index，再应用到状态机。

这里有三个位置要分清：leader 已经写到哪里，follower 已经收到哪里，follower 已经 apply 到哪里。复制延迟可能发生在网络传输，也可能发生在 follower flush 或 replay。PostgreSQL 文档里用 primary 当前 WAL 位置、standby receive LSN、standby replay LSN 这类指标观察 lag。

写返回时机决定语义。异步复制里，leader 本地提交就返回，follower 后续追；延迟低，但 failover 时可能丢掉未复制的已确认写。同步复制里，leader 要等一个或多个 follower 确认收到、写入、flush 或 apply 后返回；数据安全性更好，但慢 follower 和网络 RTT 进入写路径。

Follower 读也要看语义。读 follower 可以扩展读吞吐，但可能读旧值。权限、库存、任务 ownership 这类强语义读通常不能随便打到异步 follower；报表、搜索、缓存类读可以接受延迟。

面试答法：

```text
leader-follower replication 是由 leader 接收写、决定日志顺序并复制给 followers。followers 按同一顺序 apply。leader 可以本地提交后异步返回，也可以等待一个或多个 follower 确认后同步返回。它简化了写顺序问题，但要处理 replication lag、failover、旧 leader fencing 和 follower stale read。
```

LogServe 当前 control/logd 更像单 leader 真相源：control 写 shared log，再维护 view。它还不是多节点 leader-follower replication；未来多副本化时，才需要真正处理 leader election、commit index、follower catch-up 和读写路由。

## Q013. 同步复制和异步复制的 trade-off 是什么？

同步复制和异步复制的取舍，可以用一句话概括：异步复制用更低写延迟和更高写可用性换取复制延迟和数据丢失窗口；同步复制用更高延迟和更强依赖换取更小 RPO 和更强读新鲜度。

异步复制的路径是：

```text
leader 本地提交 -> 返回客户端成功 -> 后台复制到 follower
```

它的优点是写延迟低，吞吐高，follower 慢或短暂断开时 leader 还能继续写。缺点是 leader 返回成功后，写可能还没到 follower。如果 leader 此时崩溃并 failover 到落后 follower，客户端已经收到成功的写可能消失。PostgreSQL 文档也明确说，streaming replication 默认异步，primary 崩溃时可能有已提交事务没复制到 standby，丢失量与 failover 时复制延迟有关。

同步复制的路径是：

```text
leader 本地写入 -> 发送给同步 follower -> 等确认 -> 返回客户端成功
```

确认级别还可以细分：收到、写到 OS、flush 到磁盘、apply 后可见。PostgreSQL 的 `remote_apply` 会等 standby replay 后确认，读 standby 时语义更强；`remote_write` 则比 flush/apply 弱。

同步复制的收益是缩小已确认写丢失窗口，甚至做到只有 primary 和同步 standby 同时故障才丢。代价是提交延迟至少增加网络往返；同步 standby 慢会拖慢业务写；同步 standby 断开时，事务可能等待很久甚至不可用；锁持有时间变长会放大竞争。跨 region 同步复制尤其昂贵，因为远距离 RTT 和网络抖动直接进入 commit path。

常见分层策略是：

```text
元数据、余额、权限、锁、leader 状态：同步复制、quorum 或共识。
搜索索引、缓存、报表、指标、推荐数据：异步复制。
普通业务写：按重要程度选择 per-transaction 或 per-table 同步级别。
```

面试可以这样答：

```text
异步复制延迟低、可用性好，但有 replication lag；leader failover 时可能丢掉已向客户端确认但未复制的写。同步复制要等 follower 确认后返回，能降低 RPO，也能让 follower 读更接近最新，但会增加写延迟，并把慢 follower、网络抖动和同步副本故障带进关键路径。工程上通常按数据重要性分层，而不是全系统一刀切。
```

对 LogServe，task/workflow/actor 事件是恢复真相源，未来多副本 shared log 应偏同步或共识；dashboard snapshot、benchmark 汇总、model cache hint 可以异步，因为它们可从日志重建。

## Q014. 主从复制延迟会影响哪些业务语义？

主从复制延迟不是单纯“从库慢一点”。它会让业务看到旧状态，进而破坏一些用户以为理所当然的语义。

第一是 read-your-writes。用户刚保存头像，下一次请求读到从库，从库还没 replay，于是又看到旧头像。用户会以为保存失败。

第二是 monotonic reads。第一次请求读到新状态，第二次被负载均衡到更落后的 follower，反而看到旧状态。订单状态、任务状态、审批流里这会非常怪。

第三是 causal consistency。先发生的原因没复制到某个 follower，后发生的结果却从另一个路径可见。比如先创建任务，再看到任务完成通知，但读从库查不到任务。

第四是权限和安全。管理员撤销权限后，如果鉴权读落后 follower，用户可能短时间继续访问。权限撤销、封禁、密钥轮换通常要走强一致路径或做缓存主动失效。

第五是唯一性和约束。用从库判断用户名是否存在，可能因为延迟没看到刚创建的用户名，进而放过重复创建。唯一约束必须在 leader 或强一致事务路径上判断。

第六是调度和状态机。leader 上任务已经 `SUCCEEDED`，follower 还显示 `RUNNING`，上层如果基于旧状态重试，可能导致重复执行或乱序状态转换。

第七是 failover 数据承诺。异步复制下，leader 已经返回成功但 follower 还没收到；leader 崩溃后提升落后 follower，确认写可能消失。这影响的是 RPO，不只是读旧。

缓解方式要按语义选：

| 语义 | 延迟造成的问题 | 常见做法 |
|---|---|---|
| read-your-writes | 写后读旧值 | 写后短期读 leader、session stickiness、携带 last-seen LSN |
| monotonic reads | 连续读倒退 | 固定副本、要求 follower replay 到某个 LSN |
| 权限 | 撤销不及时 | 鉴权读 leader、缓存短 TTL、主动失效 |
| 唯一性 | 旧读导致重复 | leader 事务、唯一索引、共识路径 |
| failover | 确认写丢失 | 同步复制、quorum commit、明确 RPO |
| 监控 | dashboard 落后 | 显示 snapshot 时间和 applied index |

面试答法：

```text
主从复制延迟会影响 read-your-writes、monotonic reads、因果一致性、权限撤销、唯一性检查、任务状态机、failover 数据丢失和监控解释。解决方式不是所有读都走主，而是按语义分层：关键判断走 leader 或强一致路径，普通展示读可以读 follower，但要接受并暴露 lag。
```

LogServe 如果未来有 follower query API，调度、actor ownership、task completion 不能基于 follower 旧 view 决策；dashboard 可以落后，但要显示 last applied log index 和 snapshot 时间。

## Q015. split-brain 是什么？

Split-brain 是集群出现多个节点同时认为自己是 primary、leader 或 owner，并且都对同一数据范围接受写。危险点不是“有多个副本”，而是“有多个写权威”。

典型场景是 primary A 和 standby B 之间网络断了。B 的 failover 系统认为 A 死了，把 B 提升为新 primary；但 A 其实还活着，客户端也还能写 A。于是 A 和 B 都接受写，日志开始分叉。PostgreSQL failover 文档明确提醒：旧 primary 重启后必须有机制知道自己不再是 primary，否则两个系统都以为自己是 primary，会造成混乱和数据丢失。

常见诱因：

```text
网络分区导致两边互相看不到。
心跳超时太短，把慢网络误判为死亡。
failover 提升了新主，但旧主没有被关闭或 fencing。
共享 VIP、共享磁盘或写入口迁移不严格。
两节点集群没有 witness 或 majority 仲裁。
人工运维误把旧主也接回流量。
```

危害有三类。第一是写冲突，同一 key 在两个 primary 上被不同写更新。第二是日志分叉，同一个 index 可能对应不同 command。第三是外部副作用重复，比如两边都发邮件、扣款、调度任务。数据最终选一边，也无法撤销外部世界已经发生的副作用。

防 split-brain 的核心是 fencing。选出新 leader 还不够，必须让旧 leader 不能继续写。常见手段包括多数派共识、term/epoch、fencing token、STONITH、lease、external coordinator，以及让下游存储只接受最新 epoch 的写。

面试答法：

```text
split-brain 是多个节点同时认为自己拥有写权，并对同一数据范围接受写。它通常来自网络分区、故障检测误判或 failover 缺少 fencing。防护重点不是只选新主，而是确保旧主失去写能力，常用 majority quorum、term/epoch、fencing token、STONITH、lease 或外部一致性协调器。
```

LogServe 的 actor epoch fencing 可以看成局部防脑裂：旧 worker 失联后，新 worker 获得 actor ownership；旧 worker 迟到提交时，因为 epoch 过期被拒绝。多 control 副本场景下，epoch 分配本身还需要共识保护。

## Q016. 为什么需要 leader election？

需要 leader election，是因为 leader 负责决定写顺序，但 leader 会失败。没有 election，leader 挂了系统就不可写；随便让多个节点自行变主，又会 split-brain。

Leader 通常负责：接收写请求、分配日志顺序、复制日志、推进 commit index、响应客户端、协调 membership change。它是系统的写顺序入口。

一个合格的 leader election 要同时满足两件事：

```text
可用性：旧 leader 不可用时，能选出新 leader。
安全性：同一任期或同一数据范围内，不能有两个有效 leader 同时提交写。
```

Raft 的流程很适合解释。节点有 follower、candidate、leader 三种状态。leader 定期发 heartbeat。follower 超过 election timeout 没听到 leader，就增加 term，变成 candidate，给自己投票并发 RequestVote。拿到多数派投票后成为 leader。

多数派的作用是避免同一 term 多 leader。任意两个多数派有交集，而一个节点同一 term 只投一票，所以两个 candidate 不可能同时拿到多数派。term 的作用是识别新旧领导权：节点看到更大 term 会更新并退回 follower，旧 term 请求会被拒绝。

Raft 还要求 candidate 的日志足够新。投票者会比较 candidate 的最后日志 term/index；如果 candidate 比自己旧，就拒绝投票。否则缺少 committed entry 的节点可能当选，覆盖已经提交的日志。

面试答法：

```text
leader election 用来在 leader 失败后恢复写可用性，同时防止多个节点同时当 leader。Raft 用 term、多数派投票、随机 election timeout 和 up-to-date log 检查实现：term 区分新旧领导权，多数派保证同一 term 最多一个 leader，日志新旧检查保证新 leader 不缺已提交 entry。
```

LogServe 当前是单 control，不需要真正的 leader election。未来多 control 副本时，如果没有 election 和 fencing，就可能两个 control 同时调度同一 task 或分配同一 actor ownership。

## Q017. Raft 的 term、log index、commit index 分别是什么？

Term、log index、commit index 分别对应领导权、日志位置和提交进度。

Term 是 Raft 的任期，也是逻辑时钟。每次选举都会进入新 term。一个 term 可能选出一个 leader，也可能因为 split vote 没选出 leader。RPC 里带 term；节点看到更大 term 就更新自己并退回 follower；看到旧 term 请求就拒绝。term 用来识别旧 leader、旧 candidate 和迟到消息。

Log index 是 entry 在日志里的位置。Raft 日志由一串 entry 组成，每个 entry 通常包含 index、term 和 command。例如：

```text
index=7, term=3, command=set x=1
```

index 定义状态机命令顺序。所有副本最终要在同一 index 应用同一 command，否则状态机会分叉。

Commit index 是当前节点知道已经 committed 的最高日志位置。一个 entry committed，表示它已经安全到可以应用到状态机，之后不会被覆盖。leader 会在 AppendEntries 里传播 commit index，followers 据此推进 apply。

还要区分 `commitIndex` 和 `lastApplied`：

```text
commitIndex：共识层已经确认安全提交到哪里。
lastApplied：本节点状态机实际执行到哪里。
```

如果 `commitIndex > lastApplied`，节点就按顺序继续 apply，直到追上。

面试答法：

```text
term 是任期和逻辑时钟，用来区分新旧 leader，并标记日志 entry 的创建任期。log index 是日志位置，用来定义状态机命令顺序。commit index 是已安全提交的最高日志位置，followers 根据它决定哪些 entry 可以 apply。term 管领导权，index 管顺序，commit index 管哪些顺序已经生效。
```

LogServe 里的 epoch、sequence/log offset、materialized view replay position 有类似影子：epoch 防旧 owner，sequence 定顺序，replay position 表示 view 应用到哪里。但当前它不是 Raft 实现，不能把这些词混用。

## Q018. Raft 如何保证日志匹配性质？

Raft 的日志匹配性质有两层：如果两个日志在同一个 index 和 term 上有 entry，那么这个 entry 的 command 相同；并且它们在这个 index 之前的所有 entry 也相同。

第一层靠“同一 term 最多一个 leader”和 leader append-only。一个 leader 在某个 term 的某个 index 只会写一个 entry；同一 term 又不可能有两个合法 leader，所以同一个 `(index, term)` 不会对应两个 command。

第二层靠 AppendEntries 的一致性检查。leader 给 follower 追加 entries 时，会带 `prevLogIndex` 和 `prevLogTerm`。Follower 只有在自己的日志中这个位置也有相同 term 时，才接受追加；否则拒绝。

leader 被拒绝后，会把该 follower 的 `nextIndex` 往前退，继续尝试。直到找到双方共同前缀。找到后，follower 删除冲突后缀，再追加 leader 的 entries。Raft 论文把这说成：leader 强制 follower 日志复制自己的日志。

例子：

```text
leader:   (1,t1) (2,t1) (3,t2) (4,t3)
follower: (1,t1) (2,t1) (3,t2) (4,t2)
```

leader 先用 `prevLogIndex=4, prevLogTerm=3` 追加，follower 发现 index 4 term 不匹配，拒绝。leader 后退到 index 3，`prevLogTerm=2` 匹配，于是 follower 删除自己的 index 4，再追加 leader 的 index 4。

还要配合投票时的 up-to-date 检查。已经 committed 的 entry 存在于多数派；新 leader 也必须拿多数派选票；两个多数派相交。投票者拒绝日志落后的 candidate，就能保证缺少 committed entry 的节点不能当选。

面试答法：

```text
Raft 通过 leader 唯一性、leader append-only、AppendEntries 的 prevLogIndex/prevLogTerm 检查和 nextIndex 回退来保证日志匹配。Follower 只有前缀匹配才接受追加；不匹配就拒绝，leader 回退直到找到共同前缀，然后覆盖 follower 冲突后缀。再加上选举时日志 up-to-date 检查，已提交日志不会被缺失它的新 leader 覆盖。
```

LogServe 单机 logd 通过 append-only 和恢复校验避免同一位置出现不同 record。多副本化后，就需要类似 Raft 的前缀匹配和 commit 规则，否则副本可能在同一 offset 分叉。

## Q019. Raft snapshot 解决什么问题？

Raft snapshot 解决日志无限增长和落后节点追赶成本问题。

Raft 状态机靠 replay 日志得到当前状态。系统运行越久，日志越长：磁盘占用增加，重启 replay 变慢，新节点加入要复制大量历史，落后 follower 追赶成本也高。Raft 论文明确说，日志不能无限增长，否则会影响可用性。

Snapshot 的做法是：状态机在某个已经 committed 的 index 生成完整状态快照，然后丢弃这个 index 之前的日志。快照至少要保存：

```text
状态机当前状态。
lastIncludedIndex：快照覆盖到的最后日志位置。
lastIncludedTerm：该位置 entry 的 term。
最新 membership configuration。
```

lastIncludedIndex/Term 很重要，因为快照替代了旧日志前缀，后续 AppendEntries 仍要能做 prev index/term 检查。

Snapshot 解决四个问题：减少磁盘占用；减少节点重启 replay 时间；让新节点或落后 follower 不必从创世日志追起；当 leader 已经 compact 掉 follower 需要的 entry 时，可以用 InstallSnapshot 让 follower 直接安装快照，再接 tail log。

它的安全边界是：只能 snapshot committed entries，不能把未提交日志固化进状态机。否则未提交 entry 本来可能被覆盖，snapshot 会把错误状态永久化。

实现时还要处理快照文件写一半崩溃、分块传输中断、snapshot 与 tail log 边界不匹配、生成快照时状态机还在 apply 新 entry 等问题。

面试答法：

```text
Raft snapshot 是日志压缩机制。日志随着请求增长，不能无限保存和 replay。节点把某个 committed index 的状态机完整状态写成 snapshot，记录 lastIncludedIndex、lastIncludedTerm 和配置，然后丢弃之前日志。落后 follower 如果需要的日志已被 compact，leader 用 InstallSnapshot 让它追上。snapshot 只覆盖已提交 entry，不改变 commit 语义。
```

LogServe 的 actor snapshot replay 思路类似：从 snapshot 加 tail log 恢复 actor，避免从第一条 command 全量 replay。但这是单机 actor 恢复优化；Raft snapshot 是共识日志压缩和 follower catch-up 机制。

## Q020. Paxos 和 Raft 的核心目标有什么相同点？

Paxos 和 Raft 的核心目标相同：在非拜占庭故障模型下，让多个节点对同一系列值或命令达成一致，从而实现 replicated state machine。只要状态机是确定性的，所有节点按相同顺序执行相同命令，就会得到相同状态，对外像一个可靠单服务。

Paxos Made Simple 把共识问题说成：多个进程可以提出值，算法保证最多只有一个值被选中；如果值被选中，进程最终能学习到它。安全性包括只能选择被提出的值、只能选择一个值、不能凭空学习一个未被选中的值。

Raft 也从 replicated state machine 出发。每个 server 存一份日志，日志里是状态机命令；共识模块负责让各日志包含相同请求和相同顺序，即使部分服务器失败。

共同点可以概括为：

```text
agreement：同一个日志位置不能决定两个不同值。
validity：决定的值来自合法 proposal 或客户端 command。
safety：任何时候都不能让两个节点对同一位置应用不同命令。
majority intersection：用多数派交集保留已决定信息。
fault tolerance：少数节点 crash、消息丢失、重复、延迟时仍保持安全。
replicated state machine：把单值共识扩展成一串日志位置的命令顺序。
```

差别主要在组织方式。Paxos 用 proposer、acceptor、learner、proposal number、prepare/promise、accept 来推导安全；Multi-Paxos 通常引入稳定 leader 优化正常路径。Raft 默认强 leader，把问题拆成 leader election、log replication、safety、membership change 和 snapshot，用 term/index/commitIndex 组织工程实现。

面试答法：

```text
Paxos 和 Raft 的目标一样，都是让多个节点在故障和异步消息下对值或日志顺序达成唯一决定，并用这个顺序复制状态机。它们都依赖多数派交集保证安全，都不是 Byzantine fault tolerant。区别主要是表达和工程结构：Paxos 更抽象，用 proposal number 和 acceptor promise 保证不会选出两个值；Raft 用 leader、term、AppendEntries、日志匹配和 commit index 把同一目标拆得更易实现和解释。
```

不要说 Raft “理论上比 Paxos 更强”。更准确的是：Raft 的设计目标之一是可理解性和工程可实现性；两者解决的是同一类 crash-fault consensus 问题。

放到 LogServe 上，Paxos/Raft 解决的是未来 shared log 多副本化时“同一日志位置到底是哪条事件”的问题。当前项目没有实现这个共识层，面试时要主动说清楚。

## Q021. 共识算法和分布式锁是什么关系？

分布式锁可以看成共识服务上的一个应用，但它不等于共识算法本身。

共识算法解决的是：多个节点在有 crash、消息延迟、重复、丢失的情况下，对某个值或某条日志位置达成唯一决定。Raft、Paxos 这类算法关心的是 replicated log、term、quorum、commit、leader election 和 safety。它们的输出通常是一个线性化的状态机：所有副本按同一顺序执行同一批命令。

分布式锁解决的是另一个上层问题：多个客户端争用某个资源时，同一时刻最多允许一个客户端进入临界区。实现分布式锁时，常见做法是把“创建锁记录”“续约”“删除锁记录”这些操作放到一个强一致存储里。ZooKeeper 用 ephemeral sequential znode 做锁，etcd 提供基于 lease 的 Lock API，背后都是通过强一致复制保证锁状态的线性化更新。

所以关系是：

```text
共识算法 -> 提供强一致复制日志或状态机
强一致 KV/协调服务 -> 在共识状态机上实现 create/delete/CAS/lease
分布式锁 -> 在这些原语上实现互斥、排队、超时释放
```

如果锁服务本身没有共识或等价强一致能力，就很难在分区和故障下保证互斥。Redis 官方 Redlock 文档也指出，基于异步主从 failover 的锁会破坏互斥：客户端 A 在 master 上拿到锁，master 还没复制给 replica 就崩溃，replica 被提升后客户端 B 也能拿到同一把锁。这不是实现细节小 bug，而是复制语义不够强。

不过，分布式锁不是共识的完整替代。锁只告诉你“某个客户端在锁服务里暂时排在第一”，它不自动保证共享资源真的只接受这个客户端的写。客户端可能暂停、网络包可能延迟、锁 lease 可能过期、旧客户端可能恢复后继续写。真正安全的设计还需要 fencing token，让共享资源拒绝旧 owner 的写。

面试时可以这样答：

```text
共识算法通常是分布式锁的底层基础。Raft/Paxos 提供线性化状态机，ZooKeeper/etcd 在这个状态机上实现临时节点、lease、CAS 和顺序号，分布式锁再用这些原语实现互斥。锁不是共识算法本身，也不能替代共识；它只是共识服务提供的协调抽象。拿到锁以后，如果共享资源不校验 fencing token，仍然可能被旧持有者写坏。
```

放到 LogServe，actor ownership 和 worker lease 就是锁/租约类问题。当前单机 control 可以用内存状态加 log-first 事件管理 owner；如果以后 control 多副本化，owner 分配和 epoch 递增就必须放到共识日志里，否则两个 control 可能同时授予同一 actor 或 task 的执行权。

## Q022. 为什么拿到分布式锁不等于拥有写入共享资源的绝对安全？

因为锁服务和共享资源不是同一个东西。锁服务认为你拿到了锁，只说明在锁服务的当前历史里，你曾经获得过某段时间的所有权；它不能保证你的进程没有暂停、你的网络包没有延迟、你的 lease 没有过期，也不能强迫共享资源拒绝旧请求。

最经典的问题是“暂停超过 lease”。客户端 A 拿到锁，开始准备写共享存储。它在写之前被 GC、page fault、宿主机调度、SIGSTOP 或长时间网络调用卡住。锁有 TTL，所以过期后客户端 B 拿到同一把锁，并完成写入。随后 A 恢复，继续把旧数据写回共享存储。如果共享存储只看“请求来自 A”，不看 A 的锁是否仍然新，这次写就会覆盖 B 的新结果。

这个问题不能靠“写前再检查一次锁”彻底解决。进程可能在检查之后、真正写之前暂停；网络请求也可能在锁有效期内发出，却在锁过期后才到达存储服务。Martin Kleppmann 的分布式锁分析里专门强调：包可以任意延迟，进程也可以在任意位置暂停，所以只靠时间判断会出错。

另一个问题是锁释放。Redis 单实例锁会要求 value 是唯一随机串，释放时只删除自己创建的锁，避免客户端 A 超时后误删客户端 B 的锁。这能解决“错误释放别人锁”的问题，但仍然不能解决“旧客户端带着过期所有权写共享资源”的问题。

还有一类问题来自锁服务故障或复制语义。异步复制的锁服务在 failover 时可能让两个客户端都认为自己拿到锁；基于本地时钟 TTL 的锁在时钟跳变、暂停、慢网络下也可能扩大危险窗口。即便锁服务本身是 ZooKeeper/etcd 这种强一致系统，客户端和共享资源之间仍然有异步边界。

安全写入共享资源需要共享资源参与校验。常见做法是：每次获得锁时，锁服务返回一个单调递增的 fencing token；客户端写共享资源时必须带上 token；共享资源记录自己见过的最大 token，拒绝更小 token 的写。这样旧客户端即使恢复，也只能带旧 token，被资源端挡住。

面试可以这样答：

```text
拿到分布式锁只说明锁服务曾经授予过你所有权，不说明你对共享资源的写一定安全。客户端可能暂停超过 lease，网络请求可能延迟到锁过期后才到达，锁服务 failover 也可能产生重复授权。共享资源如果不校验 fencing token，旧持有者仍能写入并覆盖新持有者的结果。因此分布式锁要和 fencing、幂等、版本检查或资源端 CAS 一起使用。
```

LogServe 里的旧 worker late completion 就是这个问题的项目化版本。worker 曾经拿到 task/actor 执行权，不代表它永远有权写 completion。control 必须用 attempt、lease epoch、actor owner epoch 拒绝迟到结果。

## Q023. fencing token 在分布式锁中为什么重要？

Fencing token 的作用是把“谁更新”变成“谁更新得更新”。它通常是一个由锁服务或共识服务生成的单调递增数字。每次客户端获得锁，都得到一个比过去更大的 token。客户端访问共享资源时带上 token；共享资源只接受大于已见最大 token 的请求，拒绝旧 token。

例子很直观：

```text
客户端 A 获取锁，token=33。
A 暂停很久，锁过期。
客户端 B 获取锁，token=34，并写入共享资源。
A 恢复，带 token=33 发起写。
共享资源已经见过 34，于是拒绝 33。
```

没有 fencing token，存储服务只看到两个普通写请求，很难知道哪个是旧 owner。即使锁服务很强，存储服务如果不参与校验，旧请求还是可能落地。

一个好的 fencing token 要满足几个条件：

```text
单调递增：后获得锁的 token 必须更大。
全局可比较：资源端能判断哪个 token 更新。
持久记录：资源端 crash/restart 后不能忘记最大 token。
绑定资源：不同资源可以各自维护 token，但同一资源必须一致。
资源端强制执行：客户端自觉检查不够，必须由共享资源拒绝旧 token。
```

ZooKeeper 的 zxid、znode version、ephemeral sequential node 序号常被用作 fencing token 的来源。etcd 的 revision、lease 相关 key 的 create revision 也能提供类似单调版本，具体要看锁库暴露什么。Redis Redlock 文档现在也提醒：对 correctness 敏感的场景应该实现 fencing token；随机 value 只能防误删锁，不能提供单调 fencing。

Fencing token 和 lease 的分工不同。Lease 负责让锁最终释放，避免客户端 crash 后永久占用。Fencing token 负责让旧客户端恢复后无法继续写。两者都需要。

面试答法：

```text
fencing token 是分布式锁真正保护共享资源的关键。锁服务每次授予锁时发一个单调递增 token，客户端写资源必须携带它；资源端记录最大 token，拒绝更小 token。这样即使旧持有者 GC 暂停、网络延迟或 lease 过期后恢复，它的旧 token 也会被挡住。没有资源端 fencing，锁只是在锁服务里互斥，不能阻止旧请求写坏外部资源。
```

LogServe 里 actor epoch 就是 fencing token。actor owner 每次切换 epoch 增加，旧 worker 的 completion 如果带旧 epoch，control 拒绝。这个机制比“worker 觉得自己还持有锁”可靠，因为判断发生在资源所有者 control 侧。

## Q024. 两阶段提交和共识算法解决的问题有什么不同？

两阶段提交（2PC）解决的是分布式事务提交问题：多个资源管理器已经参与同一个事务，现在要决定所有参与者一起 commit，还是一起 abort。它关心的是 atomic commit，不能出现一个数据库提交、另一个数据库回滚。

共识算法解决的是复制状态机一致问题：多个副本要对某个值、某条日志位置或一串命令顺序达成唯一决定。它关心的是在故障和消息延迟下，多个副本不能对同一位置决定不同值。

2PC 的流程是：

```text
prepare 阶段：协调者询问所有参与者能否提交。
commit/abort 阶段：如果所有参与者都 vote commit，协调者发送 commit；只要有一个 abort 或超时，就发送 abort。
```

它的核心约束是“所有参与者都同意才能 commit”。任何一个资源管理器不能提交，整个事务都应该 abort。

共识的流程因算法而异，但核心是多数派或等价 quorum 对一个值形成决定。Paxos 里 proposal 被多数 acceptor 接受后 chosen；Raft 里 log entry 被复制到多数派后 committed。它不要求所有节点都同意，只要求满足 quorum 安全条件。

最大的工程差别是阻塞。经典 2PC 有单协调者。如果所有参与者都 prepared，协调者在写下 commit/abort 决定后崩溃，参与者可能不知道最终决定，只能阻塞等待协调者恢复。Gray 和 Lamport 的 Paxos Commit 论文明确指出，经典 2PC 在 coordinator failure 时会阻塞；Paxos Commit 用多个协调者和 Paxos 来让多数派工作时继续推进。

另一个差别是参与者语义。2PC 的参与者是资源管理器，每个参与者掌握事务的一部分数据，并且每个参与者都有否决权。共识副本通常复制同一个状态机或同一条日志，副本之间是冗余关系，不是每个副本持有事务的一部分业务资源。

可以这样对比：

| 维度 | 2PC | 共识算法 |
|---|---|---|
| 目标 | 多资源事务原子提交 | 多副本对值或日志顺序达成一致 |
| 决策规则 | 所有参与者 prepared 才 commit | 多数派或 quorum 决定 |
| 参与者含义 | 不同资源管理器，各自有业务数据 | 同一状态机的多个副本 |
| 故障表现 | 协调者故障可能阻塞 | 多数派可用时可继续推进 |
| 典型用途 | 跨数据库/队列的事务提交 | replicated log、leader election、配置、锁服务 |

面试答法：

```text
2PC 解决 atomic commit：多个资源管理器要么都提交，要么都回滚；所有参与者都有否决权，经典 2PC 在协调者故障时可能阻塞。共识算法解决 replicated decision：多个副本对一个值或日志位置达成唯一决定，通常多数派可用即可继续。2PC 可以用共识增强，例如 Paxos Commit，但 2PC 本身不是容错共识算法。
```

LogServe 如果以后要把 task completion 同时写 shared log 和外部数据库，2PC 可能会出现；如果要让多个 logd 副本决定同一条 log entry，才是共识问题。两者不要混在一起说。

## Q025. 一致性哈希解决什么问题？

一致性哈希解决的是节点集合变化时，如何把 key 分配到节点，并尽量少搬迁数据。

普通取模哈希很简单：

```text
node = hash(key) % N
```

问题是 N 一变，几乎所有 key 的取模结果都会变。比如从 10 个节点扩到 11 个节点，大量 key 会重新映射。对缓存来说，这会造成大面积 cache miss；对分布式存储来说，会造成大规模数据迁移和恢复压力。

一致性哈希把 hash 空间看成一个环。节点和 key 都映射到环上。一个 key 顺时针找到的第一个节点，就是它的归属节点。新增节点时，只影响它在环上前一个节点到自己之间的那段 key；删除节点时，只影响原本归它负责的那段 key。其他 key 不需要移动。

它主要解决三个问题：

```text
扩容缩容时减少 key remapping。
让客户端在节点视图略有差异时，仍然大体把 key 指向少数几个节点。
把数据分片、缓存分片、请求路由和副本选择做成可计算规则，减少中心调度。
```

Karger 等人的一致性哈希论文把几个性质说得很清楚：smoothness 表示节点集合小变动只导致少量对象迁移；spread 表示不同客户端视图下，一个对象不会被分散到太多不同节点；load 表示单个节点不会被分配过多对象。

但一致性哈希不解决一致性语义。名字里有“一致性”，容易误导。它解决的是 key placement，不是 linearizability，也不是 quorum，也不是复制冲突。一个 key 被路由到哪个节点，不代表这个节点上的数据一定最新。

它也不自动解决热点 key。一个非常热的 key 仍然可能压垮它的归属节点。需要副本、请求合并、缓存层、热点拆分、读写分离或应用层限流。

面试答法：

```text
一致性哈希解决节点增删时的大规模重映射问题。普通 hash(key)%N 在 N 改变时会让大量 key 换节点；一致性哈希把节点和 key 放到 hash ring 上，key 顺时针归属到下一个节点。新增或删除节点只影响环上的相邻区间，因此迁移量小。它解决的是分片和路由稳定性，不解决强一致、复制冲突或热点 key。
```

LogServe 的 model cache locality、object/result placement、未来 shard routing 都可能用一致性哈希；但 task 状态真相源不能只靠一致性哈希保证一致性，仍要靠日志或共识。

## Q026. 虚拟节点如何改善一致性哈希的负载均衡？

在最简单的一致性哈希里，每台真实机器在环上只有一个点。问题是随机点分布可能不均匀：有的节点负责很长一段区间，有的节点只负责很短一段。节点数少时尤其明显。结果就是负载倾斜。

虚拟节点的做法是：一台真实机器不只放一个点，而是在环上放很多点。每个点代表一个 virtual node 或 token。key 先映射到虚拟节点，再由虚拟节点映射到真实机器。

这样有几个好处。

第一，负载更均匀。一个真实节点负责很多小区间，而不是一个大区间。随机误差会被平均掉。虚拟节点越多，单个真实节点负责的总区间长度越接近期望值。

第二，扩容更平滑。新增机器时，可以给它分配多个虚拟节点，从许多旧机器各接一小段数据，而不是只从环上的一个邻居搬一大段。这样迁移压力分摊得更均匀。

第三，支持异构容量。大机器可以分配更多虚拟节点，小机器分配更少虚拟节点。这样 key 数量和请求量大致按容量比例分布。

第四，故障恢复更平滑。一个真实节点下线后，它的多个虚拟节点分散在环上，对应数据会分摊到多个后继节点，不会只砸到一个邻居。

直观例子：

```text
没有虚拟节点：A、B、C 各一个点，A 可能刚好负责 60% 的环。
有虚拟节点：A1..A100、B1..B100、C1..C100 分散在环上，A 的总负责区间更接近 1/3。
```

虚拟节点也有代价。路由表更大，membership 变化时要维护更多 token；数据迁移计划更复杂；虚拟节点太多会增加元数据和调度开销；虚拟节点数量也不能替代热点处理，因为单个 hot key 仍然只落到一个主分片。

面试答法：

```text
虚拟节点把一台真实机器映射成 hash ring 上的多个点。这样每台机器负责许多小区间，随机分布误差会被平均，负载更均匀。扩容时新机器可以从多个旧机器各接一点数据；机器故障时负载也能分散给多个后继节点。它还支持按机器容量分配不同数量的虚拟节点。代价是路由元数据和迁移管理更复杂。
```

如果把它放到 LogServe，虚拟节点适合做 result object、checkpoint cache、model shard 的放置。它不应该被用来决定 actor command 的全局顺序；顺序仍要由 log/owner/fencing 维护。

## Q027. gossip 协议适合传播什么信息？

Gossip 协议适合传播“最终大家知道就行、单条信息不要求强顺序、可重复、可合并、短时间不一致可接受”的信息。

典型做法是每个节点周期性随机找几个节点交换信息。信息像传染一样扩散。SWIM 把 failure detection 和 membership dissemination 拆开：随机探测成员是否存活，membership update 通过 infection-style dissemination piggyback 在 ping/ack 消息上传播。这种方式的优点是每个节点消息负载接近常数，规模大时比 all-to-all heartbeat 更可扩展。

适合 gossip 的信息包括：

```text
membership：节点加入、离开、疑似失败、恢复。
健康状态：alive、suspect、dead、incarnation number。
负载摘要：CPU、内存、队列长度、可用容量。
缓存位置：某模型或某对象大概在哪些节点有副本。
反熵摘要：Merkle tree root、版本向量、数据范围 checksum。
配置提示：非关键 feature flag、限流建议、路由 hint。
统计指标：近似 QPS、延迟分位、错误率摘要。
```

不适合 gossip 的信息也要说清楚：

```text
账户扣款、库存扣减这类必须单序的写。
leader election 的最终决定。
锁 ownership 的唯一授权。
schema migration 的强顺序步骤。
权限撤销这类要求立即生效的安全决策。
```

原因是 gossip 只有最终传播，不保证所有节点同一时刻看到同一顺序。消息可能重复、乱序、延迟，两个节点可能短时间内对 membership 有不同视图。对“hint”和“摘要”这很好；对“唯一决定”就不够。

Gossip 系统常配 incarnation number 或 version，避免旧消息覆盖新消息。比如节点 A 被怀疑失败后，如果 A 还活着，它可以用更高 incarnation 宣告 alive；接收方用版本规则决定 suspect、alive、dead 哪个更新。

面试答法：

```text
gossip 适合传播最终一致的元信息，例如 membership、节点健康、负载摘要、缓存位置、反熵摘要和统计指标。它的优势是去中心化、消息负载低、规模扩展好，对丢包和节点故障比较鲁棒。它不适合传播必须线性化的决定，比如锁授权、扣款、leader 提交、权限撤销。gossip 传 hint，共识定事实。
```

LogServe 未来如果有多 worker、多 model cache，worker load、model cache presence、checkpoint cache hint 可以 gossip；task assignment、actor ownership 和 completion commit 不应只靠 gossip。

## Q028. membership 变化为什么困难？

Membership 变化困难，是因为“谁算集群成员”会直接影响 quorum、leader election、日志提交和故障判断。成员列表不是普通配置文件，它本身是共识协议安全性的一部分。

如果所有节点不能原子地同时切换配置，就会出现中间状态。有些节点按旧配置投票，有些节点按新配置投票。Raft 论文里举过直接从旧配置切到新配置的危险：旧配置可能选出一个 leader，新配置也可能选出另一个 leader，两个多数派互不相交，就会产生 split-brain。

成员变化还会改变多数派大小。3 节点集群增加到 4 个成员，如果新成员还没启动但已经被计入 quorum，原来 2 个节点可提交，现在可能需要 3 个；如果新成员 peer URL 配错，集群可能直接丢 quorum。etcd 文档也提醒，添加成员要一次一个，确认启动正确后再继续；新成员被计入 quorum 但不可达，会导致 quorum loss。etcd 因此支持 learner，先让新节点追日志，不计入投票，追上后再 promote。

困难点主要有这些：

```text
配置切换不能原子发生，各节点看到配置的时间不同。
quorum 集合变化，旧多数派和新多数派可能不相交。
新节点日志落后，立刻计入多数派会拖慢提交或破坏可用性。
移除节点后，被移除节点可能继续发起选举或发送旧消息。
leader 可能不在新配置里，需要安全退位。
运维配置错误会变成协议层不可用。
```

安全做法通常有两类。Raft 原论文描述 joint consensus：先进入旧配置和新配置联合状态，提交和选举需要旧配置多数派和新配置多数派都同意；联合配置提交后，再提交新配置。这样没有任何时刻旧配置和新配置能各自独立做决定。

另一类是工程上的 sequential reconfiguration 和 learner。etcd 的运行时重配置要求变更前有 quorum，成员变化按步骤执行；新增成员可以先作为 learner，追上 leader 日志后再升为 voting member，减少不可达新节点导致 quorum 损失的风险。

面试答法：

```text
membership 变化难，是因为成员集合决定 quorum 和 leader election。旧配置和新配置如果直接切换，各节点看到配置的时间不同，可能出现两个互不相交的多数派，各自选 leader。新增节点还可能没追上日志，移除节点还可能继续发旧消息。安全做法是把 membership change 本身写进共识日志，用 joint consensus 或 learner/sequential reconfiguration，保证新旧配置过渡期间多数派有交集。
```

LogServe 现在没有多副本 membership 问题；worker register/heartbeat 只是执行资源视图。未来如果 control/logd 多副本化，副本 membership 必须是共识层配置，不应和普通 worker membership 混为一谈。

## Q029. 故障检测器为什么不可能完美？

因为在异步分布式系统里，观察不到响应有两种解释：对方真的宕机了，或者消息/调度/网络/GC 太慢。只靠外部观察无法严格区分这两件事。

FLP 论文的模型里，消息可以任意延迟，进程速度没有上界，也没有同步时钟。在这种模型下，一个进程无法判断另一个进程是停止了，还是只是运行得非常慢。论文也正是利用这一点证明：即使只有一个进程可能 crash，确定性异步共识也不能保证总能终止。

故障检测器通常有两个理想属性：

```text
completeness：真正故障的节点最终会被怀疑或检测出来。
accuracy：健康节点不会被误判为故障。
```

在真实网络里，这两个目标冲突。超时时间设短，检测快，但误报多；超时时间设长，误报少，但故障发现慢。网络抖动、stop-the-world GC、CPU steal、磁盘卡顿、路由故障、丢包、队列积压都会制造误报。

所以工程上的故障检测器不是“真相机器”，而是“怀疑机制”。它给出 suspect，而不是证明 dead。系统要围绕这个事实设计：

```text
把 timeout 当 suspect，不当最终事实。
用 lease、epoch、fencing 防止误判后的双写。
先 suspect，再 confirm，给被怀疑节点自证 alive 的机会。
用多点观测降低单边网络问题导致的误判。
把故障检测结果和共识/quorum 结合，而不是单节点说了算。
```

SWIM 就是典型设计。它随机 ping 成员，失败时可以通过间接探测降低误报；membership 信息里有 suspect、alive、failed 以及 incarnation number。它承认误报会发生，只是通过协议降低误报率并最终传播更新。

面试答法：

```text
完美故障检测器在异步系统里不存在，因为没有响应不能证明节点死了，可能只是网络慢、消息延迟、进程暂停或本地时钟错。故障检测只能在检测速度和误报率之间取舍。工程上应把超时视为 suspect，再用 quorum、epoch、fencing、间接探测和 incarnation number 控制误判影响，而不是把心跳超时当成死亡证明。
```

LogServe 的 worker heartbeat timeout 也只能说明 worker 可疑。正确处理是 redelivery 加 attempt/epoch fencing，而不是认定旧 worker 永远不会回来。

## Q030. 心跳超时如何区分机器宕机和网络慢？

严格来说，心跳超时无法单独区分机器宕机和网络慢。它只能说明：在当前超时时间内，本观察者没有收到对方响应。至于原因，可能是机器宕机、进程 hang、GC pause、网络拥塞、单向链路故障、包排队、本机调度延迟，甚至是观察者自己卡住。

所以面试里不要说“心跳超时说明节点挂了”。更准确的说法是：心跳超时把节点标记为 suspect，然后系统用更多证据决定怎么处理。

常见增强手段有：

```text
多次失败再怀疑：避免单个丢包触发误判。
自适应 timeout：根据 RTT 分布、p99/p999、抖动动态调整。
间接探测：A ping 不通 C，让 B、D 帮忙 ping C，区分局部链路问题。
多通道观测：结合 TCP 连接、应用 ping、磁盘/进程指标、node agent。
suspect 阶段：先广播怀疑，给目标节点用更高 incarnation number 宣告 alive。
quorum 判断：关键动作不由单个观察者决定，而由多数派或协调服务决定。
fencing：即使误判导致新 owner 产生，旧 owner 后续写也会被 token 拒绝。
```

但这些手段只能降低误判，不能消除不确定性。网络慢到超过 timeout 时，检测器仍会怀疑；机器宕机但还没超过 timeout 时，检测器也不会立刻知道。这是物理和模型限制，不是实现不够努力。

心跳超时的业务动作也要分级。对读流量摘除，可以激进一点；对提升新 primary、转移 actor ownership、删除副本、触发不可逆外部动作，要保守得多。因为误判成本不同。

可以这样设计状态机：

```text
healthy -> suspect -> unhealthy -> removed
```

`healthy -> suspect` 可以由单次或少量心跳超时触发。`suspect -> unhealthy` 需要更多探测、更多观察者或更长时间。`unhealthy -> removed` 通常需要人工确认、共识配置变更或安全迁移。

面试答法：

```text
心跳超时不能严格区分宕机和网络慢，只能说明观察者在期限内没收到响应。工程上把它当 suspect：通过多次失败、自适应 timeout、间接探测、多观察者、suspect/alive incarnation、quorum 判断来降低误报。关键资源还要配 fencing token，因为即使误判了，旧节点恢复后也不能继续写。超时是信号，不是证明。
```

LogServe 可以说得很具体：worker heartbeat 超时后可以触发 redelivery，但旧 worker 的 completion 必须带 attempt/epoch 校验；actor owner 超时后可以转移 ownership，但旧 epoch 写回必须被拒绝。这样不需要心跳完美，也能保护状态机。

## Q031. clock skew 会影响哪些分布式协议？
clock skew 指不同机器的物理时钟读数不一致，或者时钟推进速度有偏差。它影响的不是所有分布式协议。像 Raft 这类共识协议，安全性不依赖物理时钟；论文里明确把时钟错误和极端消息延迟放在“最多影响可用性，不应破坏日志一致性”的范畴。但很多工程协议会把物理时间拿来做判断，这时 clock skew 就会进入安全边界。

最典型的是 lease、TTL 锁和基于过期时间的 ownership。Redis Redlock 文档在算法里要求本地时钟推进速率大体一致，并且锁有效期要扣掉获取锁耗时和 clock drift；这说明它的互斥语义是有时间窗口的。一个节点如果时钟慢，可能以为 lease 还没过期；另一个节点如果时钟快，可能已经发放了新 lease。两个 owner 同时写共享资源，就会变成 split-brain 写入。前面讲 fencing token，就是为了在这种情况下让资源侧按单调 token 拒绝旧 owner。

第二类是基于时间戳的冲突解决。Cassandra 文档说每个 mutation 都带 timestamp，冲突用 last-write-wins 解决，并提醒 correctness 依赖时钟同步。如果客户端 A 真实时间上后写，但它的时钟落后，LWW 可能把它当成旧写丢掉；反过来，一个时钟超前的写会“占据未来”，导致后续正常写难以覆盖。很多“最后修改时间”“按时间排序”“TTL 删除”的业务问题，本质也一样。

第三类是事务时间戳和快照读。Spanner 的 TrueTime 文档举过类似问题：如果系统只用普通本地时钟，后提交的事务可能拿到更小时间戳，快照读就可能看见后一个事务而看不见先发生的事务。TrueTime 试图把物理时间变成带误差界的 API，让系统知道“现在大概在这个区间内”，再通过等待或选择时间戳来保证外部一致性。

第四类是故障检测、重试、消息可见性和任务租约。心跳超时通常用本地计时器判断。如果计时器受 GC pause、CPU steal、NTP step、容器冻结影响，检测器会误判。消息队列里的 visibility timeout、任务调度里的 lease、幂等 key 的过期窗口，也都依赖时间。时钟偏差不会必然破坏安全，但会放大重复执行、过早重投、过早删除去重记录等问题。

可以按这张表记：

```text
物理时间只做性能优化：clock skew 主要影响延迟和误报。
物理时间参与安全判断：clock skew 可能造成双主、丢写、乱序和重复执行。
协议安全性不依赖物理时间：clock skew 不应破坏一致性，但可能影响选主速度、lease read 可用性和超时体验。
```

面试里不要把“时钟同步了”说成绝对安全。NTP、PTP、GPS/原子钟都只能降低误差，不能让普通分布式系统得到一个没有误差的全局时钟。更稳的说法是：安全协议尽量用逻辑时钟、term、epoch、log index、quorum 和 fencing；物理时间用于限界 stale read、lease 优化、过期清理和观测指标时，要明确误差预算。

LogServe 里如果未来做 worker lease 或 actor ownership lease，不能只靠本地时间判断“我仍然拥有任务”。更可靠的做法是给任务分配 attempt/epoch，完成写回时带上 epoch，由控制面或日志状态机拒绝旧 epoch。这样即使某个 worker 因暂停醒来后继续执行，它也不能覆盖新 owner 的结果。

## Q032. TrueTime 试图解决什么问题？
TrueTime 解决的不是“所有机器时钟完全一样”这个问题。它解决的是：在全球分布式数据库里，怎样用物理时间给事务分配时间戳，并让时间戳顺序符合外部可观察的先后关系。

Spanner 的目标是 external consistency。意思是，如果事务 T1 已经提交完成，之后 T2 才开始提交，那么所有客户端都不能看到一种状态：包含 T2 的效果，却不包含 T1 的效果。这比普通 serializability 更强，因为 serializability 只要求有一个串行顺序，不一定尊重真实时间。

普通本地时钟做不到这一点。假设 A 数据中心处理 T1，B 数据中心处理 T2。T1 真实时间上先提交，但 B 的机器时钟落后或 A 的时钟超前，T2 可能拿到更小的时间戳。MVCC 系统如果按时间戳读快照，就可能读出违反真实先后关系的结果。

TrueTime 的核心思想是把“当前时间”返回成一个区间，而不是一个点：

```text
TT.now() = [earliest, latest]
真实时间在这个区间里。
```

如果系统知道误差上界，就可以做两件事。第一，给写事务选择一个合适的 commit timestamp。第二，在返回提交成功前等待足够久，也就是常说的 commit wait，直到系统确信真实时间已经晚于这个提交时间戳。这样，下一个事务在真实时间上开始时，拿到的时间戳就会大于前一个已经完成事务的时间戳。

TrueTime 的代价也很直接：它把时钟不确定性变成等待时间和基础设施成本。Google 用 GPS 和原子钟把误差控制在较小范围，Spanner 才能把这种等待压到可接受水平。普通业务系统如果只有 NTP，同样可以设计“带误差界”的协议，但误差界更大，等待会更长，时钟 step 或配置错误也更危险。

TrueTime 也不替代共识。Spanner 仍然需要 Paxos/Raft 一类复制协议来决定副本之间的日志和数据。TrueTime 解决的是事务时间戳和外部一致性读写顺序；共识解决的是多个副本对同一条日志、同一个提交决定达成一致。两者配合，才得到跨机房事务的强语义。

面试里可以这么答：

```text
TrueTime 不是魔法全局时钟，而是一个带误差界的时间 API。Spanner 用它给事务分配符合真实提交顺序的时间戳，并通过等待误差窗口来保证 external consistency。它减少了跨分区强一致读的通信成本，但不能替代复制共识；它的正确性依赖时钟误差界被持续满足。
```

## Q033. 幂等、去重、共识分别解决不同层面的哪些问题？
这三个词经常一起出现，但它们解决的不是同一个问题。

幂等解决的是“同一个操作被执行多次，结果不要变坏”。它是业务语义或接口语义。比如创建订单请求因为网络超时重试了两次，幂等设计要求最终只创建一个订单，或者第二次返回第一次的结果。Stripe 的 idempotency key 文档就是这种模式：客户端给 POST 请求带唯一 key，服务端保存第一次请求的状态码和响应体，后续同 key 请求返回同一结果。

去重解决的是“系统能识别哪些输入已经见过”。它是传输层、队列层或服务端接入层的机制。AWS SQS FIFO 的 message deduplication ID 是典型例子：在去重窗口内，重复发送同一个 deduplication ID 的消息，不会在队列里引入重复消息。去重需要保存历史 ID，因此天然有窗口、存储成本和碰撞风险。窗口外的重复请求，系统通常就不再认识。

共识解决的是“多个副本对同一批操作的顺序和提交结果达成一致”。Raft 论文里的 replicated log 就是这个层面：所有状态机按相同顺序执行相同命令，因此得到相同状态。共识本身不会自动理解业务上的“同一个订单请求”。Raft 论文在 client interaction 部分也指出：如果 leader 已经提交日志但还没回复客户端就崩溃，客户端重试可能导致命令执行两次；解决办法是客户端给每条命令带唯一序号，状态机记录每个客户端已处理的最大序号和响应。

三者关系可以这样分层：

```text
幂等：定义重复执行时业务结果应该是什么。
去重：在某个范围和窗口内识别重复输入，尽量不让重复进入后续系统。
共识：让副本对“哪条输入以什么顺序提交”形成一致决定。
```

所以“用了 Raft 就 exactly-once”这句话不严谨。Raft 能保证日志顺序一致和提交安全，但客户端超时重试、服务端响应丢失、外部副作用已经发生等问题仍然存在。要接近 exactly-once effect，需要把 request id、client id、sequence number、响应缓存、事务写入和外部副作用边界一起设计。

也不要把去重当成幂等。去重窗口过期、去重表丢失、请求参数不一致、ID 冲突时，系统仍要有业务幂等兜底。Stripe 文档特别提到同一个 idempotency key 的参数不一致会报错，就是为了避免客户端误把不同操作伪装成同一个操作。

LogServe 的语境里，幂等是 step/actor/LLM 调用的业务设计；去重是 task_id、attempt_id、message_id、result key 的存储和检查；共识如果未来多副本化，则负责控制日志或元数据日志的提交顺序。三层都要有，不能互相替代。

## Q034. 分布式事务为什么比单机事务复杂？
单机事务的难点主要在并发控制、WAL、崩溃恢复和隔离级别。虽然实现也复杂，但它有一个巨大优势：日志、锁、缓存和数据页都在一个故障域里，事务管理器可以用本地 WAL 原子地记录“我要提交”以及“已经提交”。

分布式事务多了几个问题。

第一，参与者可能部分成功。一个事务要同时改库存库、订单库和支付库，订单库 prepare 成功后，支付库可能超时，协调者可能崩溃，网络可能分区。单机上 commit record 一落盘就能恢复；分布式场景里，不同参与者看到的阶段不一样。

第二，消息结果不确定。RPC 超时不代表对方没执行。对方可能已经提交，只是响应包丢了。协调者重试 commit 或 abort 时，需要参与者把 prepare/commit/abort 结果持久化，并且让重复消息幂等。

第三，锁和资源持有时间变长。两阶段提交的参与者进入 prepared 状态后，通常要保留锁和 undo/redo 信息，等待协调者的最终决定。协调者挂掉时，参与者可能长期不知道该 commit 还是 abort。Gray 和 Lamport 的事务提交论文也把传统 2PC 的 blocking problem 作为核心问题：协调者失败会让参与者阻塞等待。

第四，隔离和原子提交跨越多个独立系统。单机数据库可以用统一锁表或 MVCC 快照；分布式事务要在多个节点之间维护一致快照、死锁检测、写写冲突、读写冲突和提交顺序。跨地域时延还会直接进入提交路径。

第五，外部副作用难以回滚。数据库内的写可以 undo，发出去的邮件、扣过的第三方支付、调用过的物流接口，通常不能靠数据库 rollback 撤销。这也是 saga 和 outbox 模式存在的原因。

所以可以把复杂性总结成：

```text
单机事务：一个日志、一个事务管理器、一个故障域。
分布式事务：多个日志、多个参与者、消息不确定、部分失败、协调者恢复、跨节点隔离和外部副作用。
```

工程上常见取舍有三类。需要强原子性时，用 2PC、Paxos Commit、Spanner 这类协议，把复杂度放在数据库或事务层。业务流程长、参与方自治时，用 saga，把原子 rollback 改成可补偿的前向恢复。只需要事件最终送达时，用 outbox/inbox、幂等消费和重试，把一致性边界收缩到本地事务加消息投递。

LogServe 当前更适合后一类思路：本地日志先记录状态变化，再让执行器按日志恢复或重试。它不应被描述成通用分布式事务系统；更准确的说法是，它用日志和幂等恢复把“单节点多进程”的执行状态做得可重放。

## Q035. saga 补偿为什么不等价于 rollback？
rollback 是数据库事务内部的撤销。事务还没有提交，外部观察者原则上看不到它的中间结果；失败时，数据库用 undo log 或 MVCC 丢弃未提交修改，状态回到事务开始前。

saga 的补偿不是这个语义。Saga 原论文把 long-lived transaction 拆成一串可以独立提交的子事务 T1、T2、T3。每个子事务提交后，它的结果已经对外可见。如果后面的步骤失败，系统执行对应的补偿事务 C2、C1。论文说得很清楚：补偿从语义角度撤销 T 的动作，但不一定把数据库恢复到先前的物理状态。

举几个例子就很明显。

```text
扣库存 -> 补偿可以加回库存，但中间别人可能已经看到库存减少。
创建订单 -> 补偿可以取消订单，但订单号、审计记录、通知记录通常会保留。
发送邮件 -> 补偿不能“撤回已读邮件”，只能再发一封更正邮件。
调用三方支付 -> 补偿可能是退款，不是让原扣款从未发生。
```

这说明 saga 没有提供传统事务的 isolation。T1 提交后，其他事务可能基于 T1 的结果做了新的决策；后来 C1 再补偿，会产生业务上的二次影响。比如用户已经收到“抢票成功”通知，补偿只能告诉他“订单取消并退款”，不能让用户从没看到过成功。

saga 也不保证补偿一定成功。补偿本身可能失败、超时、重复执行，或者遇到新的业务约束。例如退款接口不可用、账户被冻结、库存商品已经下架。严肃的 saga 设计要把补偿步骤也做成可重试、幂等、可观测，并且准备人工处理路径。

所以面试回答要抓住这句话：

```text
saga 是业务语义上的反向操作或前向修正，不是数据库物理 rollback。它牺牲隔离性和瞬时原子性，换取长事务、跨服务、跨外部系统场景下的可恢复性。
```

这也是为什么 saga 适合“订酒店、订机票、租车”这类长流程，不适合强约束的账户余额扣减核心账本。核心账本更适合单库事务、强一致分布式事务，或者把账本设计成追加式不可变事件，再用冲正事件表达补偿。

## Q036. 多副本日志如何处理冲突写？
要先区分两类系统。

第一类是 leader-based replicated log，比如 Raft。它的原则是：正常情况下只有 leader 接收写请求，leader 决定日志位置，followers 只复制 leader 的日志。这样冲突写不会在多个节点独立决定顺序。两个客户端同时写，leader 给它们排成 log index i 和 i+1；这个顺序一旦提交，所有副本按同一顺序执行。

Raft 真正要处理的冲突，通常来自 leader 崩溃和重新选主。旧 leader 可能把某些日志发给了少数副本，还没提交；新 leader 可能没有这些未提交日志，并在同一 index 写入不同 term 的条目。Raft 的处理方式是：新 leader 通过 AppendEntries 带上 prevLogIndex 和 prevLogTerm。follower 如果找不到匹配前缀，就拒绝；leader 回退 nextIndex，找到共同前缀后，让 follower 删除冲突条目以及之后的条目，再追加 leader 的日志。已经提交的条目不能被覆盖，因为选举限制保证新 leader 包含所有已提交条目。

这里的关键是：多副本日志不会“合并两个写”。它选择一个全局顺序；未提交的分叉会被截断；提交后的日志成为事实。

第二类是 leaderless 或 multi-master 系统，比如 Dynamo/Cassandra 风格。多个副本都能接受写，同一个 key 可能产生并发版本。处理方式有几种：

```text
LWW：按时间戳选最大版本，简单但依赖时钟，可能丢并发更新。
vector clock/version vector：保留并发分支，读时交给客户端或应用合并。
CRDT：把数据类型设计成可合并结构，让并发更新按数学规则收敛。
CAS/LWT/Paxos：对单 key 或小范围写走条件更新，避免并发写同时成功。
```

Cassandra 文档说它使用 mutation timestamp 和 last-write-wins 解决冲突，并提醒正确性依赖时钟同步。这种方案吞吐高、实现简单，但不适合“两个并发更新都不能丢”的业务。比如购物车可以用集合合并，账户余额就不该用 LWW 覆盖。

所以面试里可以这样答：

```text
Raft/Paxos 日志通过 leader、term、index、quorum 和日志匹配性质把冲突写排序，未提交分叉会被新 leader 覆盖。Dynamo/Cassandra 这类 leaderless 系统允许并发版本存在，再用 LWW、vector clock、CRDT 或条件写解决冲突。前者把冲突压到协议层排序，后者把冲突暴露给版本和业务合并策略。
```

LogServe 如果未来做多副本控制日志，更适合走 leader-based log：对 workflow state、actor ownership、task attempt 这类状态，用日志顺序裁决，不要用物理时间 LWW 直接覆盖。

## Q037. 最终一致系统如何向用户解释 stale read？
stale read 不是“读错了”，而是“读到了某个过去时刻的正确版本”。这个区别很重要。如果用户看到的是旧库存、旧订单状态、旧排行榜，要让产品和接口明确告诉用户：这是延迟副本或缓存视图，不是强实时视图。

对用户最有用的解释不是 CAP、quorum，而是具体承诺：

```text
这个页面最多延迟 30 秒。
刚提交的内容可能需要几秒同步。
状态正在更新，最终结果以订单详情页为准。
这个统计每 5 分钟刷新一次。
```

Spanner 文档把 strong read 和 stale read 分得很清楚：strong read 保证看到读开始前已经提交的数据；stale read 是在过去时间戳读取，如果应用能接受旧数据，可以换取性能收益。即使系统不是 Spanner，也可以借这个表达方式：告诉用户读的是“当前视图”还是“历史快照/缓存视图”。

最终一致系统里，stale read 的用户体验问题主要有几个：

```text
用户刚写完却看不到自己的修改。
用户刷新页面后数据倒退。
列表页和详情页状态不一致。
通知说成功，但查询页仍显示处理中。
不同设备看到不同状态。
```

解释是一部分，设计更重要。常见做法是：

```text
写后短时间内对该用户走主库或强读，保证 read-your-writes。
页面展示“更新中”“同步中”“最后更新时间”。
对金额、权限、库存扣减、订单支付这类强语义操作，不使用弱读做最终裁决。
列表页允许旧，详情页强一些；统计页允许旧，交易页强一些。
使用单调读，避免用户第二次刷新看到比第一次更旧的数据。
```

不要把 stale read 包装成“高性能优化”来搪塞用户。用户关心的是能不能做决定。如果旧数据不会影响决策，比如点赞数、阅读量、异步分析结果，可以接受；如果会影响付款、权限、额度、库存，就要用强读或在操作前重新校验。

面试中可以这样说：

```text
最终一致不是让用户自己承担不一致，而是系统要把不一致窗口产品化：标明刷新时间、限制旧读使用场景、为关键路径提供强读或二次确认，并至少保证 read-your-writes 和 monotonic reads，避免用户看到“我刚改的没了”或“越刷新越旧”。
```

## Q038. 读己之写 read-your-writes 如何实现？
read-your-writes 的语义是：同一个客户端写入成功后，后续读一定能看到自己的写，或者看到更新的值。它不要求所有用户立刻看到这个写，所以比 strong consistency 便宜，但能解决很大一类体验问题。

实现方式取决于系统架构。

最直接的方法是写后读主。用户刚提交资料修改，后续几秒内把这个用户的读路由到 leader/primary。主从复制系统里，这样可以绕开 replica lag。缺点是主库压力增加，而且多地域读会变慢。

第二种是 session stickiness。把同一个用户会话固定到同一个副本或同一个区域，并保证这个副本接收了该会话之前的写。适合有区域亲和的系统，但故障切换和扩缩容时要小心，不能把用户切到更旧副本。

第三种是携带版本水位。每次写成功后，服务端返回 commit timestamp、LSN、log index、vector clock 或 session token。客户端后续读请求带上这个 token；读服务只从已经追到该水位的副本读取。如果本地副本没追上，要么等待，要么转发到更新副本，要么返回明确的“稍后再读/正在同步”。Doug Terry 的材料也用类似思路解释 read-my-writes：系统记录客户端已经执行过哪些写，然后找一个已经看见这些写的服务器来回答读。

第四种是客户端缓存自己的写。比如用户刚发评论，前端先把评论插入本地列表，后台异步确认。这能改善体验，但它只是 UI 层的 read-your-writes；如果后端后续读仍然旧，跨设备或刷新后可能露馅。关键业务不能只靠这个。

第五种是 quorum 策略。如果写 W、读 R 满足 R + W > N，并且没有 sloppy quorum、没有冲突版本被错误覆盖，读集合会和写集合相交。这样能读到最新写的副本。不过这不是会话级专用方案，成本更高，而且跨数据中心延迟明显。

可以用一个简化协议描述：

```text
write(key, value) -> commit_index = 105
client 保存 session_min_index = 105
read(key, session_min_index=105)
server 选择 applied_index >= 105 的副本；否则等待或转主。
```

这里的坑也不少。版本 token 必须按分区管理，否则一个全局水位会拖慢所有读；客户端丢 token 后语义会退化；负载均衡器不能随意把请求打到落后副本；读缓存也要理解版本水位，不能返回更旧缓存。

LogServe 里可以把这个思路映射到 workflow run：提交某个控制操作后，查询状态至少要读到包含该操作的日志偏移。否则用户会看到“刚启动的 step 不存在”或“刚取消的任务还在运行”。

## Q039. 单调读 monotonic reads 如何实现？
monotonic reads 解决的是“越读越旧”的问题。它允许第一次读是旧的，但要求同一客户端后续读不能比第一次更旧。Doug Terry 的材料把它归为 session guarantee：客户端可以读到任意旧数据，但必须看到越来越新的数据；如果第一次读到版本 v5，下一次至少还是 v5，不能退回 v3。

实现上最常见的办法还是维护会话读水位。

每次读返回数据时，服务端同时返回版本信息：

```text
partition = user_profile
version = log_index 105 或 commit_timestamp 2026-06-20T10:00:03Z
```

客户端把这个版本记入 session token。后续再读同一分区时带上 token，服务端只能选择已经追到该版本的副本。如果当前就近副本只有 v100，要么等待它追到 v105，要么转发到拥有 v105 以上版本的副本。

如果数据分区很多，token 通常不能只有一个全局数值。更常见的是：

```text
每个分区一个最低版本。
每个对象或 key range 一个最低版本。
用 vector/session token 压缩多个分区水位。
```

另一种办法是 sticky session：同一用户一直读同一个副本。只要这个副本自身版本单调前进，用户就不会看到倒退。但这对故障切换不够稳。副本挂了以后，如果切到另一个更落后的副本，单调读仍会破坏。所以更可靠的 sticky session 也要配版本水位。

还有一种产品层办法是缓存最高版本结果。客户端或 BFF 如果发现后端返回了更旧版本，就继续展示本地较新版本，或者提示“同步中”。这适合页面展示，但不适合需要后端裁决的业务操作。

单调读和 read-your-writes 不一样。read-your-writes 关注“我写过的内容我能读到”；monotonic reads 关注“我已经看过的版本不要倒退”。一个只读用户没有写过任何东西，也会需要单调读。例如他第一次看到订单状态是“已发货”，刷新后不应该变回“待发货”。除非业务明确展示的是不同视图，否则这会让用户以为系统坏了。

面试里可以这么答：

```text
单调读通常靠 session token/version watermark 实现。每次读把返回版本记下来，后续读要求副本至少追到这个版本；如果追不到，就等待、换副本或返回同步状态。sticky session 是简化版，但故障切换时仍要靠版本水位兜底。
```

LogServe 查询 workflow 状态也适用：用户已经看到 run 进入 `running`，后续查询不应该因为读了旧 materialized view 又显示 `pending`。解决办法是让状态查询带上上次看到的 log offset，视图没追上时等待或明确返回“view lagging”。

## Q040. 会话一致性 session consistency 解决什么体验问题？
session consistency 解决的是一个很实际的问题：全局强一致太贵，但完全最终一致又会让单个用户觉得系统反复失忆。它把一致性范围收缩到“同一个用户、同一个客户端、同一个会话”内，至少保证这个用户的操作和观察是连贯的。

典型体验问题有四类。

第一，写后看不到。用户修改头像后，页面刷新仍然是旧头像。这是 read-your-writes 要解决的问题。

第二，越刷新越旧。用户第一次看到订单“已支付”，第二次看到“待支付”。这是 monotonic reads 要解决的问题。

第三，写入顺序错乱。用户先改地址，再提交订单；系统如果让订单读取到旧地址，就破坏了 writes-follow-reads 或 monotonic writes 一类会话语义。

第四，多页面视图互相打架。列表显示任务已完成，详情页显示任务运行中；或者手机端看到取消成功，网页端又显示未取消。完全避免要强一致，但 session consistency 至少能让同一用户自己的操作路径不倒退。

Doug Terry 的 baseball 文章给了一个好用的视角：不同读者对同一份复制数据需要不同保证。记分员需要 read-my-writes 就足够得到近似强读效果；广播员不一定要最新比分，但不能先播 2-5，半小时后又播 1-3。这个例子说明，会话一致性不是理论装饰，它直接对应用户是否信任系统。

工程实现通常是 session token。服务端把客户端已经写过或读过的版本编码进 token，客户端每次请求带回来。读路径用 token 选择足够新的副本，写路径用 token 保证写入基于不旧于用户已读版本的状态。云数据库、KV 系统、BFF 层都可以实现这个思路。

它的边界也要说清楚：

```text
session consistency 不保证所有用户立刻看到同一结果。
session token 丢失、换浏览器、跨设备未同步时，语义可能退化。
跨多个分区维护 token 会增加复杂度。
它解决用户体验连续性，不替代金融账本、权限变更、库存扣减等强一致裁决。
```

面试回答可以收成一句话：

```text
session consistency 是在最终一致和强一致之间给单个用户加的一层连续性保证。它让用户至少看到自己的写、不会读到比自己已见版本更旧的数据，并能让后续写基于自己已读到的状态执行。实现上通常靠 session token、版本水位、sticky routing 和必要时等待副本追赶。
```

放到 LogServe，最自然的例子是控制台查询和操作 workflow。用户刚取消 run，下一次刷新不能又看到它可运行；用户已经看到某个 step 完成，后续页面不能退回 pending。即使底层视图是异步构建的，也应该用 log offset/session watermark 把用户体验做成单调的。
## Q041. CRDT 解决什么类型的冲突？
CRDT 解决的是“多个副本在没有同步协调的情况下，各自接受了并发更新，之后怎样自动收敛”的问题。它不解决所有冲突，也不替代事务。它只适合那些可以给出明确合并语义的数据类型。

CRDT 的关键前提是：更新可以在本地先执行，副本之间异步传播；当两个副本最终收到了同一批更新，它们必须用确定性规则得到同一个状态。Shapiro、Preguiça、Baquero 对 CRDT 的定义强调了两点：任何副本可以不协调地修改；收到同一组更新后，副本按数学规则确定性收敛。

典型适用场景是这些：

```text
计数器：多个副本并发 increment/decrement，最终值可以合并。
集合：一个副本 add，另一个副本 remove，需要定义 add-wins、remove-wins 或 observed-remove。
购物车：多个设备同时加商品，可以把新增项合并，避免丢失 add。
协作文档：多端插入文本，靠位置标识和因果关系合并。
点赞、关注、标签、在线状态：业务能接受最终收敛和明确冲突语义。
```

CRDT 的价值在于把“冲突解决”从模糊的业务补丁变成数据类型的一部分。比如 OR-Set 会给每次 add 生成唯一 tag，remove 只删除自己观察到的 tag。这样，一个副本没有见过的并发 add 不会被另一个副本的 remove 顺手删掉。这个语义适合“不要误删别人刚加的元素”的场景。

它不适合语义本身无法自动合并的冲突。两个人同时预订同一个会议室，CRDT 可以记录“两个人都申请了”，但不能替业务决定谁有权使用会议室。两个账户同时扣同一笔余额，也不能靠简单 CRDT 自动保证余额不为负。此时需要事务、条件写、锁、共识日志，或者把业务改成追加式账本再异步对账。

CRDT 还有一个容易被忽略的边界：它解决的是收敛，不等于强一致。收敛前，副本仍可能读到不同结果；合并语义正确，也不代表用户体验一定正确。例如 remove-wins set 会避免被并发 add 复活，但可能让用户刚添加的项目消失；add-wins set 会保留并发 add，但删除过的元素可能短暂或永久“回来”。这不是实现 bug，而是语义选择。

面试里可以这样答：

```text
CRDT 解决的是弱一致、异步复制下的并发更新合并问题。它要求数据类型本身定义好并发语义，使副本不经过协调也能最终收敛。它适合计数器、集合、购物车、协作文档这类可合并对象；不适合唯一性约束、余额扣减、库存独占、权限变更这类需要即时裁决的冲突。
```

放到 LogServe 语境，如果只是统计任务完成次数、worker 心跳摘要、可合并标签，CRDT 思路可以用；如果是 workflow step 是否完成、actor owner 是谁、某个 attempt 是否还能提交结果，就不能只靠 CRDT 合并，必须有日志顺序或 epoch/fencing 裁决。

## Q042. LWW register 有什么数据丢失风险？
LWW register，也就是 last-writer-wins register，只保留“最后一次写”的值。问题在于：它把并发写压成一个赢家，输掉的写会直接消失。这个“最后”通常由时间戳或某个全序规则决定，不一定等于真实业务上的最后。

CRDT 文献里把 register 分成 multi-value register 和 LWW register。multi-value register 会保留并发写出的多个值，让读者或应用层合并；LWW register 只留下全序中最大的那个写。这个选择简单、便宜，但代价就是丢掉其他并发值。

风险主要有几类。

第一，时钟偏斜导致新写被旧写覆盖。Cassandra 文档说每个 mutation 都带 timestamp，冲突按 last-write-wins 解决，并明确提醒 correctness 依赖时钟同步。如果客户端或协调节点时钟不准，一个真实时间上更早的写可能带着更大的 timestamp，把后来的有效写覆盖掉。

第二，并发更新没有合并机会。比如用户 A 在手机上把收货地址改成“公司”，用户 B 在网页上把电话改成新号码。如果整个 profile 是一个 LWW register，最后写入的一整个 profile 会覆盖另一个，地址或电话会丢。更好的做法是按字段做版本，或者用 map CRDT、merge patch、条件写。

第三，删除也会赢。很多 LWW 系统把 delete 当作带时间戳的 tombstone。如果某个节点时钟超前，它发出的 delete 可能压住后续正常 update；如果 tombstone 过早清理，旧值又可能在反熵同步中回来。Cassandra 文档里也提到 mutation timestamp 包括 delete，这正是 LWW 删除语义需要谨慎的原因。

第四，业务语义被“最后”偷换。购物车里两个并发 add，本来应该都保留；LWW 只留下一个版本，就会丢商品。Dynamo 论文特意把购物车作为例子：它宁可暴露多个版本给应用合并，也要保证 add-to-cart 不丢；代价是删除项可能复现。这说明很多业务宁愿处理重复或复活，也不能接受静默丢写。

LWW 的合理使用场景通常是“覆盖就是业务语义”的数据，比如用户昵称、最后在线时间、某个缓存值、可重新计算的物化视图。即便如此，也最好让写入带条件版本，避免旧客户端覆盖新客户端。

面试回答可以收成这样：

```text
LWW register 的风险是把并发写压成单值，未获胜的写没有合并机会。它依赖时间戳或全序规则；时钟偏斜、网络延迟、客户端离线重放都会让旧写覆盖新写。适合最后状态类字段，不适合购物车、余额、库存、权限、复合对象更新这类不能丢并发信息的业务。
```

在 LogServe 里，不应该用 LWW 直接覆盖 step result 或 actor state。更稳的是 append-only event 加 attempt/epoch 校验：旧 attempt 的结果来了也保留审计信息，但不让它覆盖当前状态。

## Q043. vector clock 如何帮助冲突检测？
vector clock 的作用是记录“这个版本看见过哪些副本的哪些更新”，从而判断两个版本是祖先关系，还是并发分支。它不是用来给所有事件排一个全局顺序，而是用来检测因果关系。

一个 vector clock 可以理解成一组 `(node, counter)`：

```text
[(A, 3), (B, 1)]
```

它表示这个版本至少包含 A 的前三次更新和 B 的第一次更新。比较两个 vector clock 时，如果 V1 在每个节点上的计数都小于等于 V2，并且至少有一个小于，那么 V1 是 V2 的祖先，V2 已经包含 V1 的内容。此时旧版本可以被新版本取代。

如果两个 clock 互相都不小于等于对方，它们就是并发分支：

```text
V1 = [(A, 3), (B, 1)]
V2 = [(A, 2), (B, 2)]
```

V1 看到了 A 的第三次更新，但没看到 B 的第二次；V2 看到了 B 的第二次，但没看到 A 的第三次。系统不能说谁覆盖谁，只能把它们标记为冲突，交给应用合并，或者按业务规则保留多值。

Dynamo 论文把这个讲得很具体：每个对象版本都带 vector clock。系统可以判断两个版本在同一条因果链上，还是在平行分支上。如果一个 clock 的所有计数都小于等于另一个 clock，旧版本可以丢弃；否则两个版本冲突，需要 reconciliation。读请求如果拿到多个无法自动合并的分支，会把所有 leaf 版本和版本信息返回给客户端；客户端合并后再写回，分支才折叠成一个新版本。

vector clock 的好处是不会像 LWW 那样把并发写静默丢掉。它能告诉你：“这两个更新不是先后关系，你不能假装其中一个自然覆盖另一个。”这对购物车、文档编辑、配置合并很有用。

它的代价也明显。元数据会随参与写入的节点数量增长；Dynamo 论文也提到需要截断 vector clock，截断后 descendant 关系可能不再完全准确。客户端还要理解多版本返回，应用要写合并逻辑。换句话说，vector clock 只负责检测冲突，不负责替你决定业务答案。

面试里可以这么说：

```text
vector clock 通过每个副本的递增计数记录因果历史。比较两个 vector clock，可以判断一个版本是否包含另一个版本，还是两个版本并发产生。它帮助系统避免把并发写误认为覆盖关系；检测到并发后，仍然需要应用、CRDT 或人工规则做语义合并。
```

LogServe 如果未来多副本接收控制命令，vector clock 可以帮助发现“两个控制面分支都接受过不同命令”；但最终 workflow 状态仍要靠日志顺序或人工/协议裁决，不能只停在“发现冲突”。

## Q044. 分布式系统中为什么要优先定义 failure model？
failure model 是系统设计的地基。它回答的是：我们假设什么东西会坏、会怎样坏、坏到什么程度、系统还要保证什么。如果不先定义它，后面的协议讨论很容易变成空话。

比如同样说“节点故障”，含义可能完全不同：

```text
crash-stop：节点挂了就停止，不再发消息。
crash-recovery：节点会重启，可能从持久化状态恢复。
omission：消息可能丢、重复、乱序或延迟。
partition：一部分节点互相不可达。
Byzantine：节点可能撒谎、伪造、给不同对象发不同内容。
```

Raft 论文的安全说明建立在非 Byzantine 条件上：网络延迟、分区、丢包、重复、乱序都可以出现；服务器按 crash-recovery 模型失败，稳定存储会保留必要状态；只要多数节点可用并能通信，系统可用。这个模型下，Raft 不需要处理恶意节点伪造日志、签名密钥泄漏、leader 故意给不同 follower 发送不同提交证明等问题。

如果 failure model 变了，协议结论也会变。处理 crash fault，3 个节点能容忍 1 个节点宕机，因为多数派 2 个还能工作；处理 Byzantine fault，经典口头消息模型下要容忍 f 个恶意节点，通常需要至少 3f+1 个节点。把 Raft 用在 Byzantine 环境里，然后期待它给出 BFT 安全性，就是模型错配。

failure model 还决定工程成本。是否要 fsync 每条日志？是否允许异步复制？是否接受 clock skew？是否要跨 AZ 放副本？是否要签名所有消息？是否要防止管理员误操作？这些不是协议之外的小问题，它们都来自故障假设。

更实际一点，failure model 也决定 SLA 话术。一个系统可以说“单 AZ 故障不丢已提交数据”，也可以说“任意两个节点宕机仍能读”，但不能含糊地说“高可用”。高可用必须绑定故障范围、恢复时间、数据丢失窗口和降级行为。

面试回答可以这样组织：

```text
分布式协议的正确性只在它声明的 failure model 内成立。先定义 failure model，才能确定副本数、quorum、持久化、超时、重试、fencing、安全认证和降级策略。否则我们无法判断系统到底是在处理 crash、partition、clock skew，还是 Byzantine 行为；也无法判断某个“保证”是否真的成立。
```

LogServe 当前应该明确说是单节点/多进程机制验证，主要处理进程崩溃、重启、重复执行、超时和本地持久化恢复；不要把它说成已经处理多节点网络分区或 Byzantine 故障。边界说清楚，反而更可信。

## Q045. Byzantine fault 和 crash fault 的区别是什么？
crash fault 是“节点停止工作”。它可能挂掉、重启、失去响应，但不会继续以任意方式作恶。其他节点最多看到超时、连接断开、消息没回来。Raft、Paxos、ZooKeeper、etcd 这类常见工程共识系统主要按 crash fault 或 crash-recovery 模型设计。

Byzantine fault 更强。节点可能发送错误信息，也可能对不同节点发送互相矛盾的信息。Lamport、Shostak、Pease 的 Byzantine Generals Problem 抽象的正是这种情况：故障组件会给系统不同部分提供冲突信息。论文里说，在只使用“口头消息”的模型中，要让忠诚节点达成一致，忠诚者必须超过三分之二；三个节点里有一个叛徒时无法解决。

用工程例子看，区别很直观：

```text
crash fault：leader 挂了，不再发 AppendEntries。
Byzantine fault：leader 给 A 发送日志 x，给 B 发送同一 index 的日志 y，还都声称已提交。

crash fault：磁盘坏了，节点读不出数据。
Byzantine fault：节点返回伪造数据，或者对不同客户端返回不同结果。

crash fault：网络包丢了。
Byzantine fault：消息被篡改、重放、伪造，或者节点故意违反协议。
```

处理 crash fault 的协议可以用多数派交集保证安全。因为诚实节点不会签署两个互斥决定，也不会故意隐瞒已提交日志。处理 Byzantine fault 时，光有多数派不够，因为故障节点可能同时给多个分支投票。协议需要更高副本数、认证消息、签名或 MAC、视图变更、审计证据，有时还要考虑非确定性执行、客户端欺骗和状态转移安全。

不要把 Byzantine fault 简化成“黑客攻击”。它当然包括恶意行为，但也可能来自严重软件 bug、内存损坏、磁盘固件 bug、编译器 bug、配置污染、时钟异常，或者某个中间层对不同接收方产生不一致结果。核心不是动机，而是故障表现没有约束。

面试里可以这样答：

```text
crash fault 假设节点最多停止或恢复，不会继续发送任意错误消息；Byzantine fault 假设节点可能任意作恶，包括撒谎、伪造、对不同节点说不同的话。前者通常用多数派和日志复制处理，后者需要 BFT 协议、更多副本和消息认证，成本明显更高。
```

## Q046. 为什么多数工程系统不处理 Byzantine fault？
多数工程系统不处理 Byzantine fault，主要不是因为工程师不知道它存在，而是因为成本和威胁模型不匹配。

第一，部署环境通常不是开放敌对网络。Dynamo 论文在系统假设里直接说，它运行在 Amazon 内部服务环境中，假设运行环境 non-hostile，最初没有认证和授权需求。很多数据库、配置中心、消息队列也类似：节点由同一组织运维，网络有 ACL、TLS、IAM、审计、镜像签名和发布流程。系统更常见的故障是宕机、重启、丢包、分区、磁盘满、配置错、GC pause，而不是节点主动伪造协议消息。

第二，BFT 成本高。容忍 f 个 Byzantine 节点，常见 BFT 协议需要 3f+1 级别的副本，还要多轮消息、签名或 MAC、视图变更和更复杂的状态转移。对一个普通 KV、数据库主从复制、服务发现系统来说，这会增加延迟、带宽、存储、实现复杂度和运维难度。

第三，很多 Byzantine 风险可以在其他层压低。比如用 TLS 防中间人篡改，用身份认证防陌生节点加入，用只读镜像和供应链扫描减少二进制被替换，用审计日志和权限隔离减少内部误操作，用校验和发现磁盘腐败。这些不是 BFT 共识，但在封闭生产环境里，性价比常常更高。

第四，BFT 不会自动解决所有安全问题。协议能处理“少数节点发送冲突消息”，但不能防止管理员把所有节点一起升级成错误版本，也不能防止业务逻辑把错误请求当成合法请求。很多公司真正需要的是变更管理、备份恢复、权限治理、灾备演练，而不是把每个内部复制协议都改成 BFT。

第五，可观测和排障难度会明显上升。Crash fault 下，节点挂了就重启、替换或重放日志；Byzantine 场景下，节点可能只对一部分对端撒谎，问题更难复现。对普通业务团队，复杂协议本身反而可能成为新的风险源。

所以更准确的说法是：多数系统不处理 Byzantine fault，是因为它们选择了 crash/partition failure model，然后用安全边界降低 Byzantine 发生概率。如果系统处在开放联盟链、多方不互信结算、跨组织共识、无人可信硬件环境里，BFT 才会变成核心需求。

面试回答可以这样说：

```text
多数工程系统默认节点由同一组织控制，威胁模型是 crash、network partition 和 operator error。BFT 需要更多副本、更多通信、认证消息和复杂实现，延迟与成本都高。工程上通常用 TLS、身份认证、权限、审计、供应链控制和数据校验来降低 Byzantine 风险，而不是让核心复制协议承受 BFT 成本。
```

LogServe 也应该按这个边界回答：它验证的是本地日志、重放、幂等、attempt fencing 这些 crash/retry 模型下的机制，不处理恶意 worker 伪造状态或控制面串通作恶。

## Q047. 如何设计多可用区部署下的 quorum？
多可用区 quorum 的目标不是“副本平均撒到 AZ 就行”，而是让多数派在常见故障下仍然有交集、能提交，并且不会因为某个 AZ 故障同时丢可用性和安全性。先定 failure model：要容忍一个 AZ 故障，还是一个 AZ 加一个节点故障？要保证写可用，还是只保证读可用？是否允许降级成只读？

最常见的设计是 3 个 AZ、3 个投票副本，每个 AZ 一个副本，quorum=2。这样任意一个 AZ 故障后，剩下两个 AZ 还能组成多数派；任意两次成功写的多数派必然相交，安全性靠交集保持。Spanner 的区域配置也采用类似思路：基础 regional 配置有 3 个 read-write replicas，每个在不同 availability zone；一个副本失败后，另外两个仍能形成 write quorum。

如果要容忍 2 个副本故障，可以用 5 个投票副本，quorum=3。但 5 副本不是免费午餐。写要等 3 个投票副本，慢副本、跨 AZ RTT、尾延迟都会进入提交路径。存储成本、网络复制成本、rebalancing 成本也会上升。很多业务真正需要的是容忍 1 个 AZ 故障，而不是在任何情况下都扛住 2 个 AZ 同时故障。

不要把 2 个 AZ + 2 副本当成强可用 quorum。2 副本如果 quorum=2，任意一个 AZ 故障就不能写；如果 quorum=1，又会在网络分区时产生双写风险。2 AZ 可以做主备或异步灾备，但不是一个舒服的多数派复制拓扑。

设计时还要考虑副本角色。Spanner 文档区分 read-write、read-only、witness：read-only 不参与写投票，可以扩展读而不增加写 quorum；witness 参与投票但不存完整数据，能帮助形成写 quorum，减少存储成本。这个思路很实用：不要让所有副本都参与写投票，否则每加一个远端读副本都可能放大写延迟。

常见方案可以这样记：

```text
3 AZ / 3 voters / quorum=2：最常见，容忍 1 AZ 或 1 voter 故障。
3 AZ / 5 voters / quorum=3：更高容错，但写延迟和成本更高。
2 AZ / 2 voters：不推荐做强多数派写入，要么不可用，要么有分裂风险。
3 AZ + read-only replicas：读扩展，不增加写 quorum。
3 AZ + witness：帮助投票，降低完整副本成本，但不能服务读。
```

还要避免相关性故障。副本不能都在同一个机架、同一个电源域、同一个 Kubernetes node pool、同一个网络设备后面。AZ 级别的“隔离”也不是绝对的，依赖云厂商实现；关键系统还要做故障演练，确认一个 AZ 被切掉时客户端、DNS、连接池、限流和重试不会把剩余 AZ 打垮。

面试答法：

```text
多 AZ quorum 先定容错目标。常见做法是 3 AZ 放 3 个 voting replicas，quorum=2，保证任意一个 AZ 故障后仍可写，并让任意两个提交多数派有交集。读扩展用 read-only replicas，不让它们增加写 quorum；需要降低成本时可以考虑 witness。不要用 2 AZ/2 副本假装强可用多数派。
```

## Q048. 跨地域复制为什么会显著影响写延迟？
跨地域复制影响写延迟，根本原因是强一致写要等远端确认。网络光速、运营商路径、跨洲 RTT、排队、TLS、拥塞控制、云骨干调度都会进入提交路径。单机写日志是微秒到毫秒级，同城 AZ 是低毫秒到十几毫秒，跨洲 RTT 往往是几十到一百多毫秒。协议再聪明，也不能绕过物理距离。

以 Spanner 的写路径为例，文档说客户端写请求先到 leader replica；leader 记录写入后，并行转发给有投票资格的副本；多数 voting replicas 同意后才 commit。文档还直接说明：每次写都需要 voting replicas 之间通信；要降低延迟，就要减少 voting replicas 数量，并把它们放得尽量近。多地域配置之所以引入 read-only 和 witness，也是为了在全球读延迟、写 quorum、存储成本之间取平衡。

这就解释了为什么“加一个远端副本”不一定影响写，但“加一个远端投票副本”会影响写。如果远端只是 async follower 或 read-only replica，它可以落后，不在写提交路径里；如果它是 write quorum 的一部分，leader 必须等它或等另一个远端 voter 的 ack。

跨地域写延迟还会被尾延迟放大。quorum 不是等所有副本，但要等足够多的副本。假设 5 副本 quorum=3，只要第 3 快的 voter 在远端，写延迟就被远端 RTT 限住。网络偶发抖动、远端限流、磁盘 fsync 慢、leader 到某个区域链路拥塞，都会出现在 p99/p999 写延迟里。

还有 leader 位置问题。客户端如果在非 leader region 发起写，请求要先到 leader，再由 leader 找 quorum。Spanner 文档提到，读写事务总是先由 leader replica 处理；如果应用在非 leader 区域，leader-aware routing 可以降低一部分延迟，但不能消除跨地域复制本身的通信成本。

常见取舍有这些：

```text
强同步跨地域写：RPO 小，故障切换干净，但写延迟高。
本地 leader + 异步远端复制：写延迟低，但远端灾难时可能丢最近写。
多地域 read-only 副本：读近，写不一定变慢，但只能服务 stale/只读路径。
按用户分区放 leader：本地用户写快，跨区事务变慢。
```

面试里可以这样答：

```text
跨地域复制显著影响写延迟，是因为强一致写必须让 leader 和多数投票副本通信。只要 write quorum 跨地域，提交路径就至少包含一次或多次跨地域 RTT，并且 p99 受远端尾延迟影响。异步副本和 read-only 副本可以改善读和灾备，但不能提供同样的同步写语义。
```

LogServe 如果未来做多地域复制，控制日志如果要求跨地域同步提交，就会直接增加 workflow 调度延迟；如果只做异步灾备，就必须承认故障时有 RPO 和重复/回放边界。

## Q049. 一致性、可用性、延迟、成本之间如何取舍？
这四个目标不能同时拉满。分布式系统设计的难点，很多时候就是把它们摆到业务语义里排序。

一致性强，通常意味着更多协调。linearizable read/write 要找 leader、quorum 或时间戳协调；跨分区事务要锁、验证、提交协议。结果是延迟升高，可用性在分区时下降，成本也上升。Spanner 用 TrueTime、Paxos/复制和多地域拓扑给出强语义，但它也需要专门的时间基础设施、投票副本、leader 路由和跨区域通信。

可用性高，通常意味着允许更多本地决策。Dynamo 论文的取舍很明确：为了 always-on 体验，在某些故障场景下牺牲一致性，用对象版本、vector clock、sloppy quorum、hinted handoff 和应用合并来处理分歧。这样写入更容易成功，但读者或应用要面对冲突版本和 stale read。

低延迟要求会逼迫系统减少远程协调。读路径可以用缓存、stale read、read-only replica、local quorum；写路径可以用单 leader 就近部署、异步复制、批量提交、分区 leader 贴近用户。但只要你减少协调，就要说明失去什么语义：读可能旧，写可能冲突，故障时可能丢最近数据，或者跨分区事务不再原子。

成本包括机器、存储、网络、运维和复杂度。5 副本比 3 副本更抗故障，但存储、网络和修复成本更高；跨地域同步比单地域贵；BFT 比 crash fault 协议贵；保留完整审计日志比 LWW 覆盖贵。复杂度也是成本，协议越复杂，团队越需要测试、监控、演练和专家经验。

可以用一个表述帮助面试：

```text
金融扣款、权限变更、库存独占：优先一致性和可审计性，接受更高延迟和成本。
用户动态、点赞数、排行榜：优先可用性和低延迟，接受短暂旧读。
配置中心、锁服务、主节点选举：优先线性一致和 fencing，不追求跨洲低延迟。
日志分析、报表、推荐特征：优先吞吐和成本，允许批处理延迟。
```

取舍不是只在数据库层做。产品也要配合：哪些页面可以显示“几分钟前更新”，哪些按钮需要二次确认，哪些写失败必须阻塞，哪些操作可以异步完成。工程上要把语义写进接口：strong read、stale read、session token、idempotency key、consistency level、RPO/RTO，而不是让调用方猜。

面试回答可以这样收束：

```text
先按业务风险排序，再选一致性模型和复制协议。强一致提升正确性但增加协调、延迟和成本；高可用低延迟通常要接受旧读、冲突或异步恢复；更高容灾需要更多副本和跨地域通信。成熟设计不是追求 CAP 三选二口号，而是把每条业务路径的正确性要求、延迟预算、故障窗口和成本预算说清楚。
```

LogServe 的合理取舍是：控制状态和恢复日志更偏正确性和可重放；LLM 调用、结果缓存、观测指标可以更偏异步和最终一致。不要把所有路径都设计成同一档语义。

## Q050. CAP 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？
CAP 的核心目标是刻画网络分区下 consistency 和 availability 不能同时保证的正确性边界。它主要讨论的是分布式系统的正确性问题，具体说，是 safety 和 liveness 在不可靠网络里的冲突。它不是性能定理，也不是安全定理，更不是可维护性方法论。

Gilbert 和 Lynch 的回顾文章把这个关系说得很清楚：CAP 中的 consistency 是 safety property，意思是每个响应都要符合服务规格；availability 是 liveness property，意思是请求最终要收到响应；partition tolerance 表示服务器之间通信可能不可靠，消息可能延迟或丢失。在这种模型下，系统无法同时保证强一致响应和所有请求都得到响应。

所以 CAP 解决的是这个问题：

```text
当网络把副本隔开时，系统应该拒绝/阻塞一部分请求来保护一致性，还是继续响应来保护可用性，并接受读旧或写冲突？
```

这显然是正确性问题。选择 CP，系统在分区中拒绝某些请求，不是为了性能更好，而是为了避免返回违反线性一致性的结果。选择 AP，系统继续响应，也不是因为它“更快”这么简单，而是它把一致性降级成最终一致、会话一致或应用合并。CAP 帮你看清这个语义代价。

性能和 CAP 有关系，但不是 CAP 的核心。跨地域强一致往往慢，弱一致往往快，这是工程后果；CAP 本身不告诉你 p99 是多少，不告诉你 CPU、存储、带宽怎么配，也不告诉你 quorum 应该放在哪些 AZ。PACELC 这类模型才进一步提醒：即使没有 partition，系统也要在 latency 和 consistency 之间取舍。

安全性也不是 CAP 的主题。CAP 的 partition tolerance 不是防攻击，consistency 也不是认证授权或机密性。Byzantine fault、签名、TLS、访问控制、密钥管理属于安全和恶意故障模型，不能用 CAP 一句话覆盖。

可维护性更不是 CAP 的直接目标。CAP 不会告诉你如何拆服务、如何灰度、如何观测、如何迁移 schema。它最多提醒你：系统对分区时读写行为的承诺必须明确，否则维护者和调用方会对“可用”“一致”产生不同理解。

面试里可以这样答：

```text
CAP 的核心目标是说明在网络分区存在时，强一致性和可用性不能同时作为绝对保证。它主要是正确性定理：consistency 是 safety，availability 是 liveness，partition 是通信故障模型。性能、成本、安全性、可维护性会受 CAP 选择影响，但不是 CAP 本身要解决的问题。
```

结合前面的问题，CAP 也不是“分布式系统只能三选二”的万能句子。真实系统更常见的是按操作、按数据、按故障阶段做细粒度选择：控制面 CP，数据面可降级；核心账本强一致，统计报表最终一致；本地读 session consistency，跨地域读 stale。CAP 的价值在于逼你把这些选择说清楚。
## Q051. CAP 的典型适用场景和不适用场景分别是什么？
CAP 最适合用来分析“共享数据被复制到多个节点，并且网络可能把这些节点隔开时，系统还承诺什么语义”。它讨论的是分区发生时，一次读写请求到底要继续响应，还是为了保持强一致而拒绝或等待。

典型适用场景有几类。

第一，多副本 KV、文档库、宽表存储。一个 key 有多个副本，客户端可以从不同副本读写。网络分区时，如果两边都接受写，就可能产生冲突版本；如果只允许有多数派的一边写，另一边要降级或报错。Dynamo、Cassandra 这一类系统就是最容易拿 CAP 来讨论的对象。

第二，分布式锁、leader election、配置中心、元数据服务。这些系统通常更偏 CP。分区发生时，宁愿让一部分客户端拿不到锁、读不到最新配置、无法选主，也不能让两个 leader 同时存在。ZooKeeper、etcd、Raft/Paxos 复制日志都适合从 CAP 角度解释：它们牺牲分区少数派的可用性来保护线性一致。

第三，跨机房或多地域复制的数据系统。跨地域网络更容易出现高延迟、抖动、链路故障。强一致写要等 quorum，分区时会拒绝一侧写；可用性优先的系统会让各地本地写，再用 read repair、版本合并或补偿处理冲突。

第四，移动端、边缘节点、离线优先应用。设备经常离线，这本质上就是长期分区。系统如果允许离线编辑，就要接受后续同步时的冲突解决；如果要求强一致，就必须让离线端无法修改关键状态。

CAP 不适用或不该直接套用的场景也很重要。

```text
单机数据库：没有分布式副本通信，CAP 不是主要分析工具。
没有共享可变状态的服务：纯计算服务、无状态 API 网关，CAP 不是核心问题。
只讨论 CPU、内存、锁、磁盘 I/O 的性能瓶颈：这是性能工程，不是 CAP。
只讨论 ACID 的 C：数据库约束一致性和 CAP consistency 不是同一概念。
只讨论安全攻击、鉴权、加密：这是安全模型，不是 CAP。
只讨论最终一致的后台任务：如果没有分区下的可用性承诺，CAP 解释力有限。
```

还有一种常见误用：把“系统很慢”说成 CAP 问题。正常网络下的读写延迟，更多是 PACELC 里的 ELC：没有 partition 时，系统也会在 latency 和 consistency 之间取舍。Abadi 的 PACELC 论文特别指出，CAP 只限制某些故障下的系统能力；正常运行时的一致性-延迟权衡是另一个问题。

面试里可以这样回答：

```text
CAP 适合分析复制数据系统在网络分区下的正确性选择：继续响应但可能返回旧值/产生冲突，还是拒绝部分请求来保持线性一致。它不适合解释单机事务、普通性能瓶颈、安全认证、ACID 约束一致性，也不适合替代更细的一致性模型和故障模型。
```

放到 LogServe，CAP 目前更多是未来多节点控制日志的设计问题。当前单节点/多进程版本谈的是本地日志、崩溃恢复、重试和幂等，不应该硬说自己解决了 CAP。

## Q052. CAP 和相近概念最容易混淆的边界在哪里？
CAP 最容易混淆的地方，是把它当成一个万能分类标签。实际上它的边界很窄：网络分区存在时，强一致和可用性不能同时作为绝对保证。

第一，CAP consistency 不是 ACID consistency。CAP 里的 C 接近 linearizability/atomic consistency，关注复制对象对外表现是否像一个单副本。ACID 里的 C 是事务前后是否满足数据库约束，比如唯一键、外键、余额不为负。一个系统可以满足 ACID 约束但不是线性一致，也可以线性一致地保存一个违反业务约束的值。

第二，availability 不是“99.99% SLA”。Gilbert 和 Lynch 讨论的 availability 是非故障节点收到请求后必须最终响应，而且不是随便响应，要响应一个符合规格的结果。工程里的可用性 SLA 包含负载、维护窗口、错误率、超时阈值、容量和运维流程。把这两个混在一起，会把定理语义说歪。

第三，partition tolerance 不是一个可随意选择的产品特性。真实分布式系统无法阻止网络分区，只能决定分区时怎么做。所谓“CA 系统”在严格 CAP 语境下只适合没有分区模型的环境；只要系统跨节点通信，就不能把“不要 P”当成工程方案。

第四，CAP 不是 eventual consistency 的定义。AP 系统经常提供 eventual consistency，但 CAP 只说分区时不能同时保证线性一致和可用。最终一致还要解释收敛条件、冲突解决、反熵、read repair、会话保证和读写路径。

第五，CAP 不是 quorum 公式。`R + W > N` 可以帮助读写集合相交，但要成立还需要严格 quorum、版本可比较、冲突规则正确、失败模型匹配。sloppy quorum、LWW 时钟偏差、并发写都可能让“看似 quorum”的系统暴露出额外语义问题。

第六，CAP 不是 PACELC。CAP 关心 partition 发生时 C/A 的取舍；PACELC 补上了 else 分支：没有分区时，复制系统仍要在 latency 和 consistency 之间取舍。很多生产系统默认弱读，不是因为正在分区，而是因为低延迟和高吞吐更重要。

第七，CAP 不是 Byzantine fault tolerance。CAP 默认的分区和消息延迟模型不等于恶意节点模型。Byzantine 节点会撒谎、伪造、对不同节点发不同消息。这个问题要靠 BFT 协议、签名、身份和信任边界，不是 CAP 的 C/A/P 能覆盖的。

面试回答可以用一句话先定边界：

```text
CAP 是分区故障下的复制对象正确性定理，不是数据库事务完整理论，也不是性能模型、安全模型或运维 SLA 模型。它和 ACID、PACELC、quorum、eventual consistency、BFT 都有关联，但每个概念解决的问题不同。
```

如果面试官追问“那为什么大家还老用 CAP”，可以说：因为它抓住了一个必须面对的事实，分区时不能既让所有请求成功，又让复制状态表现得像单副本。但真正做设计时，还要把一致性模型、读写路径、超时、重试、冲突解决和业务语义继续展开。

## Q053. CAP 在高并发场景下可能出现哪些隐藏问题？
高并发本身不是 CAP 定理的前提，但会把 CAP 选择的后果放大。很多系统在低并发下看起来“最终能对”，一到高并发就出现丢写、乱序、热点、尾延迟和冲突爆炸。

第一，AP/弱一致系统会出现更多并发冲突。两个客户端同时写同一个 key，不同副本都接受了写，恢复后要靠 LWW、vector clock、CRDT 或业务合并。并发越高，siblings 越多，read repair 和合并压力越大。Dynamo 论文提到购物车要保留 add-to-cart，不让更新丢掉；这个语义在高并发下尤其重要，因为静默 LWW 会把问题藏起来。

第二，CP/强一致系统会把并发压到 leader、锁或 quorum 路径上。所有写都要排序，热点 key 会让 leader 的日志追加、锁管理、事务验证、复制 ack 成为瓶颈。CAP 只说分区时可用性和一致性的冲突，不告诉你正常情况下 leader 能扛多少并发。

第三，尾延迟会被 quorum 放大。写要等 W 个响应，读要等 R 个响应；高并发下排队、磁盘 fsync、网络拥塞、GC pause 会让第 R 个或第 W 个响应变慢。Dynamo 论文也指出 get/put 的延迟受最慢的 R 或 W 个副本影响。强一致路径常常不是平均延迟坏，而是 p99/p999 抖动大。

第四，重试会制造放大效应。客户端超时后重试，原请求可能仍在执行；高并发下重试风暴会让 leader、协调者、锁服务和副本队列更拥堵。系统如果没有 idempotency key、request id、dedup、attempt/epoch，就会出现重复写、重复扣减、重复完成。

第五，读写隔离语义可能露出缝隙。最终一致读在高并发写入下可能长期落后；读修复可能和新写交错；缓存失效消息可能乱序；异步物化视图可能反复倒退。用户看到的不是简单的“旧一点”，而是列表、详情、通知、搜索索引之间互相矛盾。

第六，热点分区会让理论上的可用性失真。系统整体 AP，不代表某个热点 key 也能一直响应；系统整体 CP，也不代表所有分区都同样慢。高并发经常集中在少数 key、少数租户、少数 leader 上，CAP 分类看不出这种局部拥塞。

可以这样总结：

```text
CAP 告诉你分区时 C/A 不能兼得，但高并发会暴露另一层问题：冲突率、leader 热点、quorum 尾延迟、重试风暴、缓存/索引滞后、读写路径排队。这些不是 CAP 定理本身的内容，却会决定系统在真实流量下能不能活下来。
```

LogServe 里对应的风险是：大量 step 完成、重试和查询同时发生时，不能只说“日志是事实来源”。还要控制重复 completion、attempt fencing、状态视图追赶、查询水位和执行器 backpressure，否则高并发下用户会看到旧状态或重复结果。

## Q054. CAP 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？
CAP 主要讨论网络分区，但真实故障往往表现成崩溃、重启、超时和重试。它们会把“分区时选择一致性还是可用性”变成一堆更细的边界条件。

崩溃时，关键问题是 acknowledged write 到底有没有持久化。如果 leader 回复成功前崩溃，客户端不知道写是否提交；如果 follower 收到日志但还没 fsync 就宕机，恢复后可能丢掉未持久化条目。Raft 这类协议要求把 current term、votedFor、log 等关键状态持久化，就是为了让 crash-recovery 不破坏安全性。CAP 不替你处理这些恢复细节。

重启时，旧节点可能带着旧身份回来。它可能以为自己仍是 leader、仍持有 lease、仍拥有 actor 或 shard。这里必须靠 term、epoch、fencing token、lease expiry 和 quorum 重新确认身份。否则系统表面上选择了 CP，实际上因为旧 owner 复活继续写，还是会破坏一致性。

超时时，系统无法区分对方宕机、网络慢、GC pause、磁盘卡顿还是响应丢失。CAP 的 partition 在工程里经常就是“超过 timeout 没收到消息”。timeout 设短，可用性切换更快但误判更多；设长，误判少但恢复慢。故障检测器只能给 suspect，不能给绝对事实。

重试时，重复请求会进入系统。一个写请求可能已经在 leader 提交，只是响应丢了；客户端重试后，如果没有请求去重，可能把同一命令提交两次。Raft 论文在 client interaction 里也强调，客户端命令需要唯一序号，状态机要记录已处理命令，才能避免重复执行。

这些场景会暴露几类边界：

```text
ack 边界：什么时候可以告诉客户端成功？
durability 边界：成功前后哪些状态必须 fsync？
ownership 边界：重启后的旧 owner 是否还能写？
timeout 边界：超时是 suspect 还是最终失败？
retry 边界：重复请求如何去重、幂等或返回旧结果？
read 边界：故障切换后读到的是新 leader 还是旧副本？
```

AP 系统还有额外问题。临时故障时可能使用 sloppy quorum 和 hinted handoff，写先落到临时代持节点；节点恢复后再回放 hint。这个过程提高可用性，但读路径、冲突解决、反熵修复都要处理“写已经成功但目标副本暂时不知道”的状态。

面试里可以这样答：

```text
CAP 只给出分区下 C/A 的上层取舍，崩溃、重启、超时和重试会把取舍落实到持久化点、提交点、身份 epoch、故障检测和请求去重上。没有这些边界，系统即使口头选择 CP 或 AP，也会在恢复和重试路径上破坏自己的承诺。
```

LogServe 的对应说法是：任务完成写回必须带 attempt/epoch；控制状态要先写日志再对外可见；超时只能触发 redelivery，不能证明旧 worker 死了；重复 completion 要返回同一结果或被拒绝，而不是再次推进状态机。

## Q055. CAP 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？
如果严格谈 CAP，最核心的瓶颈通常是网络，而不是 CPU、内存、锁竞争或本地 I/O。因为 CAP 的触发条件就是副本之间通信不可靠：消息延迟、丢失、分区。强一致系统为了保持单副本语义，要等 leader、quorum、时间戳协商或复制确认；AP 系统为了继续可用，会减少等待，但把成本转移到冲突解决、反熵修复和读路径合并。

正常运行时，性能瓶颈要分层看。

第一层是网络 RTT 和尾延迟。跨 AZ、跨地域、leader 路由、quorum ack、strong read timestamp negotiation，都会让请求等网络往返。Spanner 文档说写请求先到 leader，再由 leader 转发给投票副本，写在多数派同意后提交；这说明强一致写的延迟下限直接受投票副本之间通信影响。

第二层是磁盘和持久化。共识日志、WAL、事务提交、hinted handoff、反熵修复都要落盘。高并发下，fsync、日志刷盘、compaction、checkpoint 会影响尾延迟。CAP 不直接讨论 I/O，但实现 CP 或高 durability AP 时，I/O 很快会进入提交路径。

第三层是锁和串行化点。强一致系统往往有 leader、primary、锁表、事务验证器、单 key 顺序器。热点 key 会让锁竞争和队列排队成为瓶颈。这个瓶颈不属于 CAP 定理本身，但属于“为了一致性而协调”的工程后果。

第四层是 CPU 和内存。序列化、压缩、校验、加密、合并 siblings、CRDT merge、Merkle tree、read repair 都吃 CPU；缓存、去重表、版本向量、会话 token、未提交日志、连接池都吃内存。通常它们不是 CAP 的第一瓶颈，但在高吞吐系统里会成为实际限制。

所以更准确的回答不是五选一，而是：

```text
CAP 层面的瓶颈主要来自网络通信和分区下不可达；强一致实现还会把网络等待扩展成 quorum 尾延迟、leader 排队、日志 fsync 和锁竞争。CPU/内存/I/O 是实现成本，网络是 CAP 语义冲突的根源。
```

Abadi 的 PACELC 论文补充了一个关键点：即使没有 partition，复制系统也存在 consistency 和 latency 的取舍。也就是说，很多你在生产里看到的“CAP 性能问题”，其实更准确地说是 PACELC 的 latency-consistency 问题。CAP 解释分区故障下为什么不能同时保证 C/A；PACELC 解释正常运行时为什么强一致复制也会慢。

面试里可以给一个具体判断法：

```text
如果慢在跨副本通信、quorum、leader routing、强读协商，主要是网络/一致性协调。
如果慢在 fsync、WAL、compaction，是持久化路径。
如果慢在热点 key、leader 单线程、事务锁，是串行化和锁竞争。
如果慢在 merge、反序列化、加密、压缩，是 CPU。
如果慢在版本缓存、去重表、连接池，是内存或资源管理。
```

LogServe 当前更可能遇到的是本地 I/O、锁竞争、队列积压和状态视图追赶，不是严格 CAP。只有当控制日志和状态复制到多节点后，网络 quorum 才会成为 CAP/PACELC 意义上的主要性能项。
## Q056. CAP 的 correctness test、stress test 和 benchmark 应该分别测什么？
这三个测试不要混在一起。correctness test 问的是“系统有没有违反自己承诺的语义”；stress test 问的是“在坏条件和高压力下会不会露出边界 bug”；benchmark 问的是“在某个语义档位下，成本、吞吐和延迟是多少”。三者都重要，但结论不能互相替代。

Correctness test 首先要写清楚模型。比如你测试的是一个线性一致 register、一个 eventually consistent KV、一个 session consistent 读路径，还是一个 quorum-based 多副本存储。Jepsen 的 linearizability 模型说得很直接：每个操作要像在某个符合真实时间顺序的单点瞬间生效。测试时要收集客户端操作历史：调用时间、返回时间、操作类型、参数、返回值、失败/超时状态，然后用模型检查这段历史是否存在一个合法串行解释。

对 CAP 相关系统，correctness test 至少要覆盖这些不变量：

```text
CP 路径：分区少数派不能接受会破坏线性一致的写。
CP 路径：已提交的写在新 leader 选出后不能丢。
AP 路径：已确认写不能静默消失；如果无法合并，必须暴露冲突版本或按声明的规则解决。
quorum 路径：成功读写的集合交集、版本比较、冲突规则要符合文档承诺。
会话路径：read-your-writes、monotonic reads 不应倒退。
恢复路径：crash/restart 后不能复活旧 leader、旧 epoch、旧 lease。
重试路径：同一请求不会重复扣减、重复提交、重复完成。
```

Correctness test 要主动注入故障。只测健康集群没有意义。Jepsen 的分析页强调过，它测试真实二进制、真实集群，在网络故障、时钟不同步、部分失败等条件下生成并发历史，再按模型检查。这一点很适合 CAP 类系统：分区、丢包、延迟、节点暂停、进程崩溃、重启、磁盘满、时钟跳变、leader 切换，都要进入测试矩阵。

Stress test 不一定证明正确性，它更像是在找系统的薄弱点。它关心长时间、高并发、故障叠加、资源耗尽下会发生什么。比如 1000 个客户端同时写热点 key，同时制造网络分区和 leader 重启；或者在 read repair、hinted handoff、compaction、snapshot、membership change 进行时打入读写流量。它要观察的是错误率、队列积压、重试放大、内存增长、goroutine/thread 泄漏、日志膨胀、恢复时间、是否进入无法自愈状态。

Stress test 的坏味道包括：测试结束后数据最终对了，但期间出现过双主；测试吞吐很高，但后台 repair 永远追不上；客户端超时后重试，服务端重复执行；分区恢复后 CPU 打满在合并 siblings；leader 反复抖动，所有请求都在排队。这些不一定都能被线性一致检查器直接抓住，但它们会在线上变成事故。

Benchmark 则必须先固定语义档位。不要把 `LOCAL_ONE`、`QUORUM`、`ALL`、strong read、stale read、async replication 混在一张吞吐图里比较，然后说“某系统更快”。不同一致性级别测的是不同产品。Benchmark 至少要报告：副本数 N、R/W 或 consistency level、是否跨 AZ/跨地域、写是否同步复制、读是否强读、是否有持久化 fsync、是否启用 repair、是否有故障注入、数据分布是否有热点。

Benchmark 指标也要分开：

```text
吞吐：成功读写 ops/s，按语义档位分别统计。
延迟：p50/p95/p99/p999，尤其看 quorum 第 R/W 个响应的尾延迟。
可用性：故障期间成功率、拒绝率、超时率、恢复时间。
一致性成本：强读比弱读慢多少，QUORUM 比 ONE 慢多少，跨地域同步比本地提交慢多少。
资源成本：CPU、内存、磁盘写放大、网络带宽、后台 repair 流量。
恢复成本：分区恢复后多久收敛，hinted handoff/replay/snapshot 需要多少资源。
```

面试里可以这样答：

```text
correctness test 用模型和历史检查语义是否被破坏；stress test 在高并发和故障叠加下找恢复、资源和边界问题；benchmark 在明确一致性级别下测吞吐、延迟、可用性和成本。三者不能互相替代：benchmark 快不代表正确，stress 没崩不代表线性一致，correctness 通过也不代表高负载下能稳定运行。
```

LogServe 如果未来多节点化，可以把 correctness test 放在“日志顺序、attempt fencing、状态机重放”；stress test 放在“大量 step、worker crash、redelivery、视图追赶”；benchmark 则分开测本地日志、异步视图、强查询水位、重试恢复，不要只给一个平均吞吐。

## Q057. 如果要求从零实现一个简化版 CAP，你会先定义哪些不变量？
严格说，CAP 不是一个可以实现的模块。更合理的说法是：实现一个简化的多副本 KV/register，用它演示分区下 CP 和 AP 两种策略。开始写代码前，先定义不变量，否则很容易做出一个“看起来能跑，但语义说不清”的系统。

第一个不变量是对象模型。先别做数据库，做一个 key-value register 就够了：`put(k, v)`、`get(k)`、可选 `cas(k, expected_version, v)`。每个值带版本，例如 `(term, index)`、`timestamp`、`vector clock` 或 `(epoch, sequence)`。没有版本，后面没法判断读到的是新值、旧值还是并发值。

第二个不变量是 failure model。节点是 crash-stop 还是 crash-recovery？消息会丢、重复、乱序、延迟吗？磁盘写成功后会不会丢？时钟能不能信？是否考虑 Byzantine？如果只做简化版，建议声明：只处理 crash-recovery、网络分区、消息延迟/丢失/重复，不处理 Byzantine，不信任物理时钟做安全裁决。

第三个不变量是 CP 模式的安全性：

```text
任意时刻最多只有一个 leader 能提交某个 key 的新版本。
只有获得多数派确认的写才算 committed。
任意两个 committed quorum 必须相交。
新 leader 必须包含所有已经 committed 的版本，或者能从 quorum 恢复它们。
少数派分区不能成功提交写，只能拒绝、超时或只读降级。
读如果声明 strong，就必须读到不早于最新 committed 版本的值。
```

这组不变量对应 Raft/Paxos 的基本思路。真正实现时可以简化，但不能跳过“多数派交集”和“已提交值不丢”这两个核心点。否则分区一恢复，就会发现两边都有各自认为成功的写。

第四个不变量是 AP 模式的语义边界：

```text
可达副本可以接受写，但每次写都生成可比较或可检测并发的版本。
系统不能静默丢弃已确认写。
如果两个版本并发，读必须返回 siblings，或者按声明的 CRDT/LWW/CAS 规则处理。
如果用 LWW，必须明说它可能因时钟偏差丢写。
分区恢复后，反熵或 repair 最终传播所有未过期版本。
```

AP 模式的核心不是“永远成功就行”，而是“成功之后怎么解释冲突”。没有冲突模型的 AP，只是把错误延后到读路径或业务层。

第五个不变量是客户端语义。至少要定义：超时算成功、失败还是 unknown？客户端重试是否带 request id？服务端是否记录已处理请求？如果一次写已经提交但响应丢了，重试会不会写两次？这些边界不写清楚，correctness test 很难判定系统错没错。

第六个不变量是恢复语义：

```text
节点重启后必须先加载持久化的 term/epoch/log/version，再对外服务。
旧 leader 或旧 owner 恢复后不能继续写，除非重新获得 quorum/epoch。
hint、repair、snapshot、log replay 不能让旧值覆盖新值。
删除要有 tombstone 或版本边界，不能让旧副本把删除的数据复活。
```

第七个不变量是可观测性。每个响应最好能带上版本、leader/replica、consistency level、是否 stale、是否 fallback。否则测试时看不到系统到底走了 CP 路径、AP 路径，还是误打误撞返回了某个副本的旧值。

面试里可以这样组织答案：

```text
我不会先写网络和存储，而会先定义对象模型、failure model、版本规则、CP 提交不变量、AP 冲突不变量、客户端重试语义和恢复不变量。CAP 演示系统的核心不是代码能收发 RPC，而是分区时每个成功响应到底代表什么承诺。
```

LogServe 的类比是：如果把控制日志做成多副本，第一批不变量应该是“同一 workflow 的状态转移全序”“旧 attempt 不能覆盖新 attempt”“完成事件幂等”“重放得到同一状态”“查询水位不倒退”。这些比先写 RPC 框架更重要。

## Q058. CAP 的常见误用是什么，误用后通常会产生什么线上症状？
CAP 最常见的误用，是把它当成一句“我们系统是 AP/CP”的标签，而不说清楚操作、数据、故障和语义。线上出问题时，症状通常不是“CAP 失效”，而是读写路径没有兑现自己暗示过的承诺。

第一种误用：把 AP 理解成“永远可写，之后自然会好”。如果没有版本、冲突检测、read repair、反熵、业务合并，分区期间接受的写会在恢复后互相覆盖。线上症状是：用户刚写的数据消失、购物车少商品、配置回滚、订单状态倒退、同一个对象出现多个互相矛盾版本。

第二种误用：把 CP 理解成“强一致且高可用”。CP 系统在分区少数派或 quorum 不足时必须拒绝一部分请求。线上症状是：某个 AZ 或机房网络抖动时，大量写超时、锁服务不可用、leader election 变慢、客户端重试风暴放大故障。如果调用方没准备好降级，就会把“保护一致性”误认为“系统挂了”。

第三种误用：把 CAP consistency 和数据库约束一致性混在一起。团队可能以为用了事务或唯一键，就解决了跨副本线性一致；或者以为用了 Raft，就自动保证所有业务约束。线上症状是：单库约束没问题，但读副本返回旧值；多服务流程里局部事务都成功，全局状态却不一致。

第四种误用：把 quorum 公式当成完整语义。`R + W > N` 如果配上 sloppy quorum、LWW timestamp、跨机房 local quorum、异步 repair，就不再自动等价于“读到最新值”。线上症状是：QUORUM 读仍看到旧值；跨 DC 用户读不到刚写；时钟偏差导致旧写覆盖新写；删除后数据复活。

第五种误用：把 benchmark 当成 CAP 证明。健康集群下 `ONE` 读写很快，不说明分区时语义正确；强一致测试只测低并发，不说明高峰期 leader 和 quorum 扛得住。线上症状是：压测很好看，一遇到网络抖动、重启、扩容、compaction，错误率和尾延迟一起飙升。

第六种误用：把 CAP 当成系统级单一属性。真实系统常常是混合的：控制面 CP，数据面最终一致；单 key 强一致，跨 key 不保证；本地读 session consistency，跨地域读 stale。线上症状是调用方按“全局强一致”理解接口，结果列表页、详情页、搜索页、统计页互相打架。

第七种误用：把超时当失败事实。客户端超时后直接重试非幂等写，服务端可能已经提交了第一次请求。线上症状是重复扣款、重复创建订单、重复执行任务、旧 owner 恢复后继续写。

可以这样收束：

```text
CAP 误用后，线上症状通常表现为数据丢失、stale read、读写倒退、split-brain、重试风暴、尾延迟暴涨、冲突版本堆积、少数派不可写被误判为故障。根因往往不是 CAP 定理本身，而是系统没有把分区、版本、重试、冲突和降级语义写清楚。
```

LogServe 如果未来对外说“我们保证一致”，就要避免这种误用。应该具体说：控制日志状态转移是单序列；worker completion 通过 attempt/epoch fencing；查询视图可能滞后但可以带水位；LLM 调用可能重试但结果写回幂等。这样调用方才知道哪些读写可以信到什么程度。

## Q059. CAP 在单机和分布式环境中的语义有什么差异？
在单机环境里，CAP 基本不是主分析工具。因为 CAP 的 P 是网络分区，讨论的是多个副本之间通信失败时还能不能同时保证强一致和可用。单机当然也会崩溃、磁盘坏、进程暂停、锁竞争、I/O 卡顿，但这些问题不等同于“副本之间分区”。

单机里的一致性更多指 ACID、并发控制和崩溃一致性。比如事务提交前后是否满足约束，WAL 是否能恢复，fsync 是否真的落盘，锁和 MVCC 是否保证隔离级别。可用性则更多取决于进程是否活着、磁盘是否可用、主线程是否被卡住、连接池是否耗尽。这里的取舍通常是性能、持久性、隔离级别、恢复时间，而不是 CAP 的 C/A/P 三角。

分布式环境里，语义会变。数据有多个副本，客户端可能访问不同副本；网络可能把副本分成两个互相不可达的集合。此时“写成功”不再只是本地落盘，还要问：成功写到了哪些副本？是否达到 quorum？失败的一侧能否继续接受写？读请求会访问 leader、quorum、任意副本还是本地缓存？这些问题才是 CAP 的范围。

一个简单对比：

```text
单机：写成功 = 本地事务提交，主要担心崩溃恢复和持久化。
分布式：写成功 = 在复制协议定义的提交点成功，主要担心 quorum、分区、leader、冲突和可见性。

单机：读旧值通常来自事务隔离、缓存或快照。
分布式：读旧值还可能来自 replica lag、stale read、local read、异步复制、分区后的旧副本。

单机：锁失效通常来自进程崩溃或代码 bug。
分布式：锁还要面对 lease 过期、clock skew、网络分区、旧 owner 复活和 fencing。
```

还有一个差异是“可用”的含义。单机系统只要进程响应，就可能算可用；分布式 CAP 里的 availability 更严格：非故障节点收到请求必须最终返回响应。CP 系统在少数派分区里可能健康运行，但它必须拒绝写，因为它不能确认自己仍在多数派里。对用户来说这是不可用；对协议来说，这是保护一致性的正确行为。

单机也可以模拟一些 CAP 相关现象，比如把一个本地服务拆成多个进程、用 IPC 通信、让其中一部分超时。但只要没有复制共享状态和网络分区，讨论 CAP 就容易夸大。更准确的说法是：单机可以练习日志、幂等、恢复和并发控制；分布式才真正面对 CAP 的分区下 C/A 取舍。

面试回答可以这样说：

```text
单机环境主要讨论事务隔离、WAL、fsync、锁和崩溃恢复；CAP 讨论的是多副本分布式系统在网络分区下的复制语义。单机的“不可用”多是进程或资源不可用，分布式的“不可用”可能是协议主动拒绝请求以保护一致性。把单机故障直接叫 CAP 问题，通常会混淆边界。
```

LogServe 当前是单节点/多进程机制验证，所以更接近单机语义：本地日志、重放、worker 超时、重复完成、状态视图滞后。只有当它把控制日志复制到多个节点，并且允许不同节点在网络分区下接收读写时，CAP 才会成为核心问题。
## Q060. linearizability 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

linearizability 的核心目标是给并发对象一个最容易推理的正确性语义：每个操作看起来都在调用和返回之间的某个瞬间生效，而且所有非并发操作的先后关系要尊重真实时间。Herlihy 和 Wing 的原论文把它描述为一种 illusion：并发进程调用对象操作时，每个操作好像瞬时发生在 invocation 与 response 之间的某一点。Jepsen 也用类似方式概括：如果操作 A 在操作 B 开始前已经完成，那么 B 在逻辑上必须排在 A 之后。

所以它首先解决的是 correctness，不是性能、安全性或可维护性。更准确地说，它解决的是“并发读写结果能不能被解释成某个合法的单线程执行”。如果一个计数器支持 `inc()` 和 `get()`，linearizability 要保证所有客户端看到的结果能放进一个全局顺序里，并且这个顺序符合计数器的顺序规范。它不直接保证吞吐量高，也不防止未授权访问，更不会自动让代码结构更好。

不过它会间接改善可维护性，因为开发者可以用顺序对象的思维理解并发对象。原论文也强调了这一点：linearizability 允许程序员使用顺序领域里的规范和推理方式来理解并发对象。比如你在一个线性一致的 KV 上做 `put(k, v)` 后再 `get(k)`，只要读操作开始时间晚于写操作完成时间，就不需要猜读请求去了哪台副本，也不需要考虑复制延迟的中间状态。

但这个语义不是免费的。分布式环境中，线性一致读写通常要经过 leader、lease、quorum 或共识协议；高并发时还会引入排队、锁竞争、日志复制和跨机网络往返。etcd 的文档就说得很直接：默认线性化访问依赖 live consensus，因此有性能成本；如果客户端选择 serializable read，可以降低延迟和提升吞吐，但可能读到相对于 quorum 过期的数据。

面试中可以这样答：

```text
linearizability 是并发对象的正确性条件。它让每个操作看起来在调用和返回之间某个瞬间生效，并且尊重真实时间顺序。它主要解决 correctness：读写结果是否能被解释成一个合法的单线程历史。它不是性能优化，也不是安全机制；恰恰相反，为了得到这个语义，系统经常要付出共识、锁、quorum、leader 往返和持久化成本。
```

放到 LogServe 的语境里，当前的 shared log 更接近“单节点顺序日志带来的可重放状态机语义”。如果未来把日志复制到多节点，再声称 workflow 状态查询是 linearizable，就必须定义查询的线性化点：是日志 append commit、状态机 apply、视图追上某个 index，还是 leader read-index 返回。这个点不定义清楚，所谓“强一致查询”就只是口号。

## Q061. linearizability 的典型适用场景和不适用场景分别是什么？

linearizability 适合用在“读到旧值会造成明显错误”的共享状态上。典型例子是分布式锁、leader election、配置中心、元数据服务、唯一 ID/序列号分配、账户余额、库存扣减、幂等请求记录、任务 ownership、schema/version 管理。这些场景的共同点是：业务逻辑依赖一个全局顺序，旧值不是“体验稍差”，而是会导致双主、重复消费、超卖、重复扣款或错误调度。

分布式协调系统就是最常见的工程落点。ZooKeeper 的 recipes 用临时顺序节点构造锁、选主、barrier 和 queue；etcd 在 Raft 状态机上提供 KV、lease、lock、election 等原语。应用真正依赖的不是“某个节点响应很快”，而是所有客户端对协调状态有可推理的顺序。锁服务如果不线性一致，两个客户端可能都认为自己拿到了锁；leader election 如果不线性一致，两个 leader 可能同时写共享资源。

它也适合做控制面，不太适合做所有数据面的默认读写语义。控制面状态通常体积小、写频低、正确性要求高，比如服务发现中的租约、集群 membership、分片 ownership、任务调度游标。数据面可能是大规模日志、指标、Feed、搜索索引、缓存、推荐特征、用户浏览记录，这些数据更关注吞吐、局部可用性和延迟，很多时候读到几百毫秒前的值可以接受。

不适用场景通常有几类。第一类是天然可合并或可补偿的业务，例如点赞数、浏览量、计数指标、日志收集、可重放事件流。第二类是跨地域低延迟写入，如果每次写都要跨洲拿 quorum，尾延迟会非常难看。第三类是用户体验允许 stale read 的场景，例如商品详情页缓存、排行榜、搜索结果、异步报表。第四类是需要多对象事务语义的场景，仅有 per-key linearizability 不够；这时要讨论 strict serializability、事务隔离和约束维护。

还要注意一个边界：linearizability 是对象级语义。Jepsen 的说明特别提醒，单个 key 的线性一致不等于多个表、多个 key、多个数据库之间也线性一致。很多 KV 系统可以保证单 key 的 CAS 是线性一致的，但不能保证“读 A 再读 B”看到的是一个全局一致快照。如果业务不小心把多对象不变量拆到多个线性一致对象上，仍然会出现跨对象约束破坏。

LogServe 里适合争取线性一致的地方，是 workflow control log 的 append 顺序、actor epoch fencing、task claim、completion 去重和状态机 checkpoint index。不一定需要线性一致的地方，是只读观测面、统计报表、调试 trace、异步 UI 状态、LLM token 流展示。面试时把这两类分开，比泛泛说“我们保证强一致”更可信。

## Q062. linearizability 和相近概念最容易混淆的边界在哪里？

第一组混淆是 linearizability 和 sequential consistency。两者都要求并发执行能解释成某个顺序执行，但 linearizability 多了真实时间约束：如果 A 已经返回后 B 才开始，那么 B 必须排在 A 后面。sequential consistency 只要求每个进程自己的程序顺序被保留，不要求不同进程之间的真实完成时间被保留。也就是说，sequential consistency 可以让一个后开始的读看起来排到之前已完成写的前面，只要每个进程本地顺序没被破坏。

第二组混淆是 linearizability 和 serializability。serializability 主要来自事务系统，关心并发事务的效果是否等价于某个串行事务顺序；它不一定尊重真实时间。linearizability 关心单个对象或单次操作的实时可见性。严格一点说，数据库里“既 serializable 又尊重真实时间顺序”的事务语义通常叫 strict serializability 或 external consistency，而不是普通 serializability。

第三组混淆是 linearizability 和 ACID consistency。ACID 的 consistency 是“事务前后保持业务约束”，比如外键、唯一约束、余额非负。linearizability 是并发历史的可见性和排序条件。一个系统可以在单节点事务里保持约束，却因为异步复制让另一个副本读到旧值；也可以让单 key 读写线性一致，但完全不理解跨 key 的业务约束。

第四组混淆是 linearizability 和“读主库”。读 leader 是实现线性一致读的一种常见路径，但不是充分条件。leader 可能已经过期，lease 可能因为时钟或暂停失效，read path 可能绕过了 commit index，状态机可能没有 apply 到读所需的日志位置。Raft 系统里常见做法是通过 ReadIndex、leader lease 或 no-op entry 确认当前 leader 仍有多数派支持，并且状态机已经应用到对应 index。

第五组混淆是 linearizability 和“强一致”。强一致不是一个足够精确的规范。有人用它表示 linearizability，有人用它表示 sequential consistency，有人用它表示 read-your-writes，有人只想表达“主从延迟很低”。面试和设计文档里最好直接写清：对象范围是什么，操作集合是什么，是否尊重 real-time order，读是否走 quorum/leader，超时后客户端能否知道操作结果。

第六组混淆是 linearizability 和 linearizable watch/notification。etcd 文档很典型：KV API 提供强保证，但 watch 事件是异步到达的，文档明确说 watch 不保证 linearizability，用户需要通过 revision 与其他操作排序。很多线上 bug 就出在这里：写操作是线性一致的，但监听、缓存刷新、UI 推送、二级索引更新不是同一个保证。

## Q063. linearizability 在高并发场景下可能出现哪些隐藏问题？

高并发下，第一个隐藏问题是队列化。linearizability 要给操作找一个全局顺序，落到实现里常常变成 leader 串行化、互斥锁、单分片日志、CAS 热点或全局版本号。并发越高，排队越明显。平均延迟可能还好，p99、p999 会先出问题，用户看到的是偶发超时、请求抖动和重试放大。

第二个问题是 hot key。即使系统整体吞吐很高，只要某个 key 被大量 CAS、扣减或读改写，它就会成为单点串行瓶颈。线性一致并不禁止并发实现，但同一个对象上的冲突操作不能随便重排。库存扣减、全局计数器、leader election key、分布式锁 key 都容易这样。扩容机器不能线性提升这个 key 的吞吐，因为冲突顺序本身就需要协调。

第三个问题是读路径被低估。很多人以为“读不改状态，所以读很便宜”。在线性一致系统里，读也要确认自己没有落在旧 leader、旧 lease、旧 commit index 或落后副本上。etcd 文档明确提到，线性化请求依赖 live consensus；如果切到 serializable read 才能降低线性化访问的性能成本，那就说明读路径确实有协议成本。

第四个问题是 thundering herd 和 retry storm。线性一致服务常被放在控制面核心位置，一旦 leader 切换、磁盘抖动或 quorum 网络变慢，大量客户端会同时超时重试。重试如果没有 backoff、deadline 和幂等 token，会把本来只是慢的服务打成不可用。更糟的是，客户端看到 timeout 并不知道操作是否已经提交，盲目重试会制造重复请求。

第五个问题是内部派生状态不线性一致。主状态可能在线性一致日志里，但缓存、watch、索引、materialized view、异步通知、metrics 都可能滞后。高并发下这种滞后更明显：用户刚写完配置，直接读 KV 没问题，但通过缓存层、订阅层或搜索索引读到旧值。系统对外如果没有标注哪些接口是线性一致的，用户会把所有读路径都当成同一个语义。

第六个问题是公平性和饥饿。linearizability 只规定合法历史，不保证谁先排队、谁不会长期失败，也不保证锁服务在高并发下公平。一个锁可以是线性一致的，但客户端因为网络、会话过期、排队策略或重试策略长期拿不到锁。正确性过关，不代表体验和调度质量过关。

LogServe 如果未来多节点化，热点很可能出现在 workflow state、actor mailbox、task claim 和 LLM request dedup key 上。控制面可以强一致，但要避免把每个 token、每条 trace、每个 UI 刷新都压到同一个线性一致路径里。

## Q064. linearizability 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

最重要的边界是 timeout 不等于失败。客户端发起写请求后超时，可能是请求没到服务端，也可能是服务端已经提交但响应丢了，还可能是提交过程中 leader 切换。etcd 文档在“operation completed”里也提醒：客户端收到响应时知道操作完成；如果超时或客户端与成员之间网络中断，客户端可能无法确定操作状态。这个 unknown 状态是分布式系统里非常常见的坑。

崩溃和重启会考验持久化顺序。一个操作如果已经对外返回成功，重启后必须还能被读到；如果只写内存、只刷部分日志、先响应后 fsync，就可能破坏线性一致历史。正确做法一般是先达到协议定义的 commit 条件，再对外返回。Raft 里就是日志条目复制到多数派并被 leader 判定 committed，状态机 apply 和读路径还要继续处理对应的 index 边界。

leader 切换会暴露旧 leader 问题。旧 leader 可能还活着，只是和多数派断开。它如果继续接受写，或继续用旧 lease 服务线性一致读，就会让客户端看到分叉历史。工程上要用 term、epoch、lease 校验、quorum 确认、fencing token 等手段把旧 leader 挡住。客户端拿到旧 leader 的成功响应也要看它是不是发生在合法任期和 commit 条件下。

重试会暴露幂等边界。假设客户端执行 `CreateOrder` 超时后重试，如果服务端没有 request id 或幂等表，两个请求都可能线性一致地成功，但业务上仍然重复下单。linearizability 只能保证每个操作有一个合法顺序，不能自动把“语义上同一次请求”的多次重试合并。要解决这个问题，需要幂等键、去重记录、CAS 条件或事务。

读重试也有边界。一个读请求超时后换到另一个副本读，如果第二个副本只提供 stale/serializable read，客户端可能看到时间倒退。要维持 linearizability，读重试必须走同等强度的读路径，或者带上最小 revision、read index、水位要求。否则客户端会觉得系统“刚才确认成功，刷新一下又没了”。

快照和日志截断也有边界。快照必须包含所有已经 committed 的状态，重启时不能从旧快照覆盖新日志；日志 compaction 不能丢掉恢复或 watch resume 需要的历史；副本追赶时不能把较旧状态 apply 到较新状态之上。很多线性一致 bug 不是出在正常读写，而是出在恢复路径、迁移路径和压缩路径。

LogServe 现在虽然不是多机共识系统，但同类边界仍然存在：step 完成响应丢了以后 worker 重试，必须靠 attempt id、task id 和日志中的完成记录去重；进程重启后要以 shared log 为准重放；旧 actor attempt 不能覆盖新 attempt 的状态。这些是之后升级到分布式 linearizability 前必须先做扎实的基本功。

## Q065. linearizability 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

在分布式系统里，最大瓶颈通常先是网络和 I/O，然后是锁竞争与排队；CPU 和内存也会影响，但多数时候不是第一个限制项。线性一致写往往要跨节点复制日志、等待 quorum、写 WAL、推进 commit index，再让状态机 apply。跨 AZ 时是一次或多次机房间 RTT，跨地域时就是几十到上百毫秒级别的物理延迟。这个成本不是换一种语言就能消掉的。

网络瓶颈来自 quorum/leader 路径。客户端可能先到 follower，再转发给 leader；leader 再复制给 followers，等多数派确认后返回。线性一致读如果需要 ReadIndex 或类似机制，也要确认 leader 仍在多数派中。网络抖动会直接放大尾延迟，少数慢副本可能不影响多数派提交，但 leader 到多数派中最快几个节点的延迟会成为关键路径。

I/O 瓶颈来自持久化。强一致系统通常不能只把成功写放在内存里，至少要让日志落到足够副本的稳定存储或满足系统定义的 durability 条件。WAL fsync、磁盘队列、云盘抖动、snapshot、compaction 都会影响写延迟。很多系统的吞吐不是被业务代码算力打满，而是被日志刷盘和复制窗口限制住。

锁竞争和单线程执行也很常见。即使协议层能批量复制，状态机 apply、MVCC 版本分配、事务冲突检测、内存索引更新、watch fan-out 也可能串行化。hot key CAS 会把并发请求压成一个队列；大量 watcher 订阅同一个前缀，会让一次写触发很多回调。此时 CPU 可能表现为高占用，但根因往往是共享数据结构争用和串行关键区。

内存瓶颈主要体现在历史版本、watch backlog、未 apply 日志、连接缓冲、请求去重表和快照构建上。linearizability 本身不要求保存无限历史，但 MVCC、watch、幂等和恢复通常需要保留一段窗口。窗口太短会影响恢复和 watch resume，窗口太长会增加内存和 compaction 压力。

CPU 瓶颈通常出现在序列化、校验、加密、压缩、状态机 apply 和冲突检测上。它当然重要，但面试里如果只答“CPU 优化”就偏了。linearizability 的核心成本在协调：为了让多个客户端看到一个尊重真实时间的顺序，系统要付出通信、持久化和排队成本。

所以回答可以落到一句话：同机并发对象的瓶颈常是锁竞争、cache line 争用和内存屏障；多机线性一致服务的瓶颈常是 leader/quorum 网络往返、WAL fsync、hot key 串行化和恢复/compaction 的后台干扰。不同层次要分开看。

## Q066. linearizability 的 correctness test、stress test 和 benchmark 应该分别测什么？

correctness test 要测“历史是否线性一致”。做法不是只看最后状态，而是记录每个客户端操作的 invocation time、response time、操作类型、参数、返回值、错误和超时，然后交给模型检查器判断是否存在一个合法的顺序历史。Jepsen 的 linearizability 模型可以拆成三个约束：存在单一顺序、尊重真实时间、返回值符合对象的单线程语义。测试 register 时，读必须返回某个已线性化写的值；测试 queue 时，出队顺序必须符合 FIFO；测试 CAS 时，成功/失败要符合版本变化。

correctness test 还要主动打故障。只在健康集群里跑并发读写，证明不了多少。应该覆盖网络分区、单向丢包、延迟、进程暂停、leader kill、follower crash、重启、磁盘慢、快照、日志压缩、时钟跳变、客户端超时和重试。每个故障都要和历史检查绑定：比如写成功返回后，后续开始的强读不能读到旧值；leader 切换后，已提交写不能丢；旧 leader 不能继续确认新写。

stress test 测的是实现边界，不只看模型是否报错。它要把并发数、连接数、key 热点、请求大小、watcher 数量、快照频率、compaction 频率、GC pause、磁盘抖动、网络抖动叠在一起，观察有没有死锁、goroutine/thread 泄漏、连接泄漏、队列无限增长、apply 卡住、watch backlog 爆炸、leader 频繁抖动、重试风暴。stress test 的价值在于逼出恢复路径和资源路径的问题。

benchmark 测的是在明确一致性档位下的成本。必须先写清楚：测的是 linearizable read、serializable/stale read，还是 eventual read？写是单 key put、CAS、事务、多 key 更新还是锁续约？然后报告吞吐、p50/p95/p99/p999 延迟、错误率、超时率、leader CPU、WAL fsync、网络 RTT、磁盘 I/O、compaction 影响、恢复时间。把 stale read 和 linearizable read 混在一个平均延迟里，是很常见的误导。

正确的实验组织可以是这样：

```text
correctness test：检查并发历史是否存在合法线性化顺序。
stress test：在高并发和故障叠加下找死锁、资源泄漏、恢复卡死和尾延迟失控。
benchmark：在明确读写语义和故障假设下测吞吐、延迟、可用性和资源成本。
```

LogServe 的版本也可以照这个结构。correctness test 检查 shared log 顺序、workflow 状态重放、attempt fencing、completion 去重；stress test 注入 worker crash、重复投递、慢 LLM、日志膨胀和视图追赶；benchmark 分开测 append、replay、query watermark、async view 和 retry recovery。不要用一个“每秒处理多少 step”覆盖所有语义。

## Q067. 如果要求从零实现一个简化版 linearizability，你会先定义哪些不变量？

我会先把问题缩小到一个对象，比如单 key register 或单 key CAS KV。不要一上来做完整数据库。linearizability 的不变量依赖对象的顺序语义，所以第一步是写清对象规范：`write(v)` 返回 OK，`read()` 返回最近一次成功写入的值，`cas(expected, new)` 只有在当前值等于 expected 时成功。没有这个顺序规范，后面谈线性化点没有意义。

第二个不变量是每个成功操作必须有且只有一个线性化点。这个点要落在调用和响应之间。单机锁保护的 map 里，线性化点可能是持锁修改 map 的那一行；CAS register 里，可能是原子 compare-and-swap 成功那一刻；Raft KV 里，可能是日志条目被 commit 并对外可见的点。失败操作也要定义线性化点，比如 CAS 失败通常在线性读取当前值时生效。

第三个不变量是实时顺序。若操作 A 的 response 发生在操作 B 的 invocation 之前，那么 A 的线性化点必须早于 B。这个不变量会直接约束读：如果写已经成功返回，之后开始的读不能绕到旧副本或旧缓存上。只要允许这种读，就不再是 linearizable read。

第四个不变量是返回值合法。对 register 来说，读只能返回线性化顺序中最近一次写的值；对 queue 来说，dequeue 不能凭空返回未 enqueue 的元素，也不能重复返回同一个元素；对 lock 来说，不能同时让两个未过期持有者都成功。很多实现看起来顺序没问题，最后坏在返回值无法被对象规范解释。

第五个不变量是提交持久性。成功返回的写在崩溃恢复后仍然存在，或者至少满足系统文档定义的 durability。否则历史在进程运行时能线性化，重启后就断了。简化实现里也要规定：响应成功前必须把日志写到本地 WAL，或在复制版里写到多数派。

第六个不变量是 epoch/term fencing。任何可能过期的执行者在写共享状态前都要证明自己仍然有效。旧 leader、旧 lock holder、旧 actor attempt、旧 session 都不能只靠“我以前拿到过权限”继续写。分布式实现里这通常靠 term、lease revision、zxid、fencing token 或 CAS version。

第七个不变量是超时和重试语义。客户端超时后的状态是 unknown，不是失败。服务端要能用 request id 去重，或者让客户端用 CAS/revision 自己判断请求是否已经生效。没有这个不变量，系统可能每个底层操作都线性一致，但业务请求仍然重复执行。

如果做一个最小版本，我会先实现单机并发对象，用互斥锁或原子操作明确线性化点，再写历史检查测试。然后扩到复制日志：leader 接收请求、追加日志、复制到多数派、commit、apply、返回；读要么走同一个日志路径，要么用 ReadIndex 类机制确认读点。每扩一层，都要重新指出线性化点在哪里。

对应到 LogServe，核心不变量可以写成：每个 workflow state transition 在 shared log 中有唯一位置；状态机只按日志顺序 apply；同一 task attempt 的完成记录只能生效一次；较旧 attempt 不能覆盖较新 attempt；查询如果声称强一致，必须声明它至少看到了哪个 log index。

## Q068. linearizability 的常见误用是什么，误用后通常会产生什么线上症状？

最常见的误用是把“单个接口线性一致”扩大成“整个系统线性一致”。例如 KV 的单 key put/get 是线性一致的，但二级索引、缓存、搜索、watch 推送、报表视图都异步更新。线上症状是用户刚写完就刷新列表看不到，直接查详情能看到；或者 API A 返回新值，API B 返回旧值。团队内部会争论“数据库明明强一致”，但真正的问题在派生读路径。

第二个误用是把 per-key linearizability 当成跨 key 事务。两个 key 各自线性一致，不代表 `A + B = 100` 这种跨 key 约束一直成立。转账、库存加订单、用户状态加权限、任务状态加索引，都可能需要事务或更高层的状态机顺序。线上症状是局部读都没错，全局报表或约束检查却发现负库存、重复 ownership、孤儿记录。

第三个误用是把“读 leader”当成充分条件。leader 如果已经失去多数派，或者读没有确认当前 term/commit index，就可能返回旧值。线上症状是故障切换期间短时间读到回退状态，或者旧主仍然接受请求，恢复后这些请求消失。这个问题在手写主从、基于 Redis/数据库 lease 的选主、没有 fencing 的调度器里很常见。

第四个误用是忽略 timeout 的 unknown 状态。客户端把超时当失败，立刻重试创建订单、扣款、发消息、领取任务。结果底层每次写入都能排进一个线性一致顺序，但业务上重复执行了两次。线上症状是重复订单、重复通知、重复任务完成、幂等表缺口，排查时会发现两次请求都有合法成功记录。

第五个误用是把 linearizability 当作性能指标。有人说“我们是强一致，所以更快/更可靠”，这句话很危险。linearizability 是正确性条件，不承诺低延迟，也不承诺高可用。网络分区或 quorum 不足时，正确的 CP 系统可能拒绝请求。线上症状是少数派机房应用还活着，但锁、配置或写请求全部超时；业务误以为这是系统坏了，其实这是系统在保护语义。

第六个误用是把安全性和正确性混在一起。线性一致的锁服务不会自动解决权限、认证、越权写、恶意客户端问题。一个未授权客户端如果能调用成功，它的操作也可以被线性化。线上症状是审计发现非法配置变更，但一致性日志本身完全合法。解决它要靠认证、授权、审计和隔离，不是靠 linearizability。

第七个误用是用 benchmark 代替 correctness test。健康集群下跑出很高吞吐，不代表分区、崩溃、重启、快照、时钟跳变时仍然线性一致。线上症状往往只在发布、扩容、磁盘满、leader 抖动、网络故障时出现；平时压测看不出来。

LogServe 的表述也要避免这种误用。可以说“shared log 让单节点内的状态转移有统一顺序，便于重放和去重”，但不应该说“系统已经具备分布式线性一致”。如果将来实现多节点复制，也要逐个接口标注：append 是否线性一致，query 是否线性一致，watch/stream 是否只是按 revision 异步追赶。

## Q069. linearizability 在单机和分布式环境中的语义有什么差异？

单机环境里，linearizability 通常讨论并发数据结构和共享内存对象。多个线程同时访问 queue、stack、map、counter、register，正确性要求是这些操作能解释成某个合法的顺序执行。线性化点往往是一次加锁保护的临界区、一次原子 CAS、一次内存屏障保护下的状态切换。主要风险是数据竞争、锁粒度、ABA、内存重排、错误的无锁算法和回调重入。

分布式环境里，对象不在同一块内存里，而是被复制到多个节点上。客户端可能连到不同副本，消息可能延迟、丢失、乱序，节点可能暂停、崩溃、重启，网络可能分区。这里的 linearizability 要求更重：成功写一旦返回，之后开始的强读不管打到哪个合法入口，都不能读到更旧的状态。实现通常要依赖共识、quorum、leader lease、read index、版本号和持久化日志。

单机里的“实时顺序”相对容易观察。线程 A 的函数返回后，线程 B 再调用，进程内同步原语可以建立 happens-before 或互斥关系。分布式里没有共享时钟，也不能只靠客户端时间戳判断顺序。系统要用协议事件来确定顺序：日志 index、term、revision、zxid、commit timestamp、quorum ack。物理时间只用于描述调用区间，不能随便拿本地时钟当线性化顺序。

崩溃语义也不同。单机对象如果进程挂了，内存状态通常直接丢失；如果要求崩溃恢复，就要引入 WAL、fsync 和恢复协议。分布式对象即使某个节点挂了，其他副本可能还在服务。于是问题变成：哪些副本承认这个写？新 leader 是否包含旧 leader 已提交的日志？旧副本恢复后会不会把旧状态带回来？这些都是复制协议的一部分。

性能差异也很大。单机线性一致对象的成本多是锁竞争、cache line bouncing、内存屏障和调度；分布式线性一致对象还要付出网络 RTT、quorum、WAL、序列化、leader 排队、快照和成员变化成本。单机上一个 mutex 可能是纳秒到微秒级；跨 AZ quorum 是毫秒级；跨 region 可能是几十毫秒以上。

语义表达也要更谨慎。单机里说“这个 map 的操作是 linearizable”，范围通常比较清楚。分布式里必须写清对象边界和入口边界：是单 key、单表、单分片、整个数据库，还是某个事务范围？是普通 get 也线性一致，还是只有带 `consistency=linearizable` 的读才是？watch、cache、read replica、analytics query 是否在保证范围内？不写清楚，用户会自然地把最强语义套到所有路径上。

LogServe 当前更接近单机/多进程语义：通过本地 shared log 让 workflow、actor、LLM 调用状态可以按一个顺序恢复。它已经具备一些 linearizability 思维里的元素，比如单一日志顺序、attempt fencing、幂等完成记录。但分布式 linearizability 还需要多副本日志、quorum commit、leader fencing、read index、成员变化和分区下的可用性取舍。面试里主动说出这个差异，反而更显得边界清楚。
## Q070. serializability 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

serializability 的核心目标是让一组并发事务的最终效果等价于某个串行执行。换句话说，事务可以并发跑，但只要它们都成功提交，数据库对外表现就应该像这些事务一个接一个执行过。这个语义关心的是事务之间有没有产生无法串行解释的结果，而不是每条 SQL 实际有没有排队执行。

它首先解决 correctness。典型问题是跨多行、多表、多谓词的业务不变量：账户转账前后总余额不变，会议室不能被重复预订，医生排班不能让同一个人同时在两个地方值班，库存不能被两个订单同时扣到负数。只靠单行锁或单 key linearizability，很容易漏掉“我读的是一组满足条件的行”这种谓词约束。serializability 要求提交后的历史能放进一个合法串行顺序里，所以业务可以先证明“每个事务单独执行时保持不变量”，再让数据库负责并发组合的正确性。

它不是性能目标。很多实现为了接近串行效果，要使用两阶段锁、谓词锁、MVCC 冲突检测、SSI 依赖图、时间戳排序、分布式提交协议。PostgreSQL 文档对 Serializable 的描述很典型：它会监控并发事务集合是否可能产生无法串行化的结果，发现风险时触发 serialization failure，让应用重试。也就是说，系统宁可中止某些事务，也不能让错误结果成功提交。

它也不是安全性目标。serializability 不负责认证、授权、审计、加密，也不能阻止恶意用户发起合法但不该被允许的事务。一个越权事务如果通过权限检查并提交，它仍然可以被串行化。安全性要靠访问控制、最小权限、审计日志和输入校验处理。

它会间接提升可维护性。开发者不用把所有并发 interleaving 都在脑子里走一遍，可以把事务当成“单独执行时正确”的程序片段来设计。PostgreSQL 文档也强调过类似好处：如果一个事务单独运行时做对了，Serializable 隔离下与其他 Serializable 事务混跑时，要么仍然等价于某个串行结果，要么其中某个事务提交失败。这个承诺对业务代码很值钱。

面试回答可以这样说：

```text
serializability 是事务隔离的正确性语义。它保证所有成功提交的并发事务，其效果等价于某个串行顺序。它主要解决 correctness，尤其是跨多行、多表、谓词查询和业务不变量的并发异常。它不是性能优化，也不是安全机制；为了实现它，系统通常要付出锁、冲突检测、事务重试、日志和分布式提交成本。
```

放到 LogServe 里，如果以后把 workflow 状态、actor mailbox、task claim、LLM request dedup 放进事务数据库，serializability 可以保护“同一个任务只能被一个 attempt 领取”“完成记录和状态推进同时生效”这类跨记录不变量。但它不能替代工作流幂等、外部 LLM 调用去重、日志重放和 actor epoch fencing。

## Q071. serializability 的典型适用场景和不适用场景分别是什么？

serializability 适合用在跨对象不变量明显、并发错误代价高的事务场景。比如金融转账、库存扣减、订单创建、支付状态推进、会议室预订、权限变更、账务结算、唯一资源分配、任务调度 ownership、工作流状态迁移。这些业务不是读旧值这么简单，而是多个读写之间存在约束：读 A、读 B、根据两者判断、再写 C 和 D。如果两个事务都用旧快照做判断，就可能各自看起来没错，合在一起却破坏约束。

它也适合数据库内部维护复杂约束。外键、唯一索引、二级索引、物化视图、账户余额、库存流水、订单状态机，这些东西经常跨多条记录。只靠 Read Committed 容易出现不可重复读、读写偏斜和 phantom；只靠 Repeatable Read 或 Snapshot Isolation，在某些数据库里仍可能允许 write skew。Serializable 的价值就在这里：让“查询谓词读到的一组数据”和后续写入之间的关系也被纳入并发控制。

不适合的第一类场景是天然可交换、可合并、可近似的数据。点赞数、浏览量、日志收集、监控指标、推荐特征、搜索索引、异步报表，通常不需要每次更新都进入全局串行事务。用 CRDT、分区计数、批处理、幂等事件流、最终一致视图，成本更低，吞吐更高。

不适合的第二类场景是长业务流程。用户下单、商家接单、仓库出库、物流配送、退款审核，这些流程可能跨分钟、小时甚至天。把整个流程塞进一个 serializable 数据库事务，会长时间持有锁或读写集合，冲突率和资源占用都不可接受。这里更常用 saga、状态机、outbox/inbox、补偿动作和幂等重试。

不适合的第三类场景是包含不可回滚外部副作用的操作。事务里调用支付网关、发短信、发邮件、调用 LLM、写外部对象存储，如果数据库后来因为 serialization failure 回滚，外部副作用不会自动回滚。serializability 只能约束数据库内部状态，不能让外部世界跟着事务撤销。工程上通常要用 outbox，把外部副作用放到事务提交之后异步执行，并用幂等键去重。

不适合的第四类场景是高冲突热点。全局计数器、秒杀库存、单个队列头、全局递增编号，如果每个请求都要走同一个 serializable 事务，系统会频繁等待、重试或中止。不是不能做，而是需要分片、预扣、令牌桶、批量分配或队列化，否则性能会很差。

LogServe 里适合用 serializable 事务保护的是控制面元数据：任务领取、attempt 变更、workflow 状态推进、幂等完成记录、checkpoint 指针。不适合的是高频 trace、token streaming、日志采集、观测指标和模型输出片段。这些读写可以异步落库或按 append-only 方式处理，不必都要求串行化事务。

## Q072. serializability 和相近概念最容易混淆的边界在哪里？

第一组边界是 serializability 和 linearizability。serializability 是事务模型，事务可以包含多次读写和多个对象；它要求所有成功提交事务的效果等价于某个串行顺序。linearizability 通常是单对象操作模型，要求每个操作看起来在调用和返回之间的某个点瞬时生效，并且尊重真实时间。serializability 本身不要求真实时间顺序；如果 T1 已经提交返回后 T2 才开始，普通 serializability 仍可能允许一个等价串行顺序把 T2 放到 T1 前面。要加上真实时间约束，通常叫 strict serializability 或 strong serializability。

第二组边界是 serializability 和 strict serializability。Jepsen 对这两个模型的区分很清楚：serializability 只要求事务看起来按某个总顺序发生；strict/strong serializability 还要求这个顺序符合真实时间，如果 A 完成后 B 才开始，A 必须排在 B 前面。很多分布式数据库宣传 external consistency，本质上是在说比普通 serializability 更强的语义。

第三组边界是 serializability 和 snapshot isolation。Snapshot Isolation 让事务读一个稳定快照，写写冲突通常会被检测，但它不一定阻止 write skew。PostgreSQL 文档也把 Repeatable Read 与 Snapshot Isolation 联系起来，并指出它仍可能不等价于所有并发事务的某个串行执行。Serializable 需要额外监控读写依赖，发现无法串行化的组合时中止事务。

第四组边界是 serializability 和 ACID consistency。ACID consistency 是事务前后业务约束保持合法；serializability 是隔离级别，描述并发事务之间的可串行解释。一个事务单独写错业务逻辑，即使用 Serializable 也会稳定地写错；反过来，一个业务逻辑正确的事务，如果隔离级别太弱，也可能在并发下破坏约束。

第五组边界是 serializability 和“串行执行”。Serializable 不等于数据库真的一次只跑一个事务。好的实现会尽量并发执行，然后用锁、版本、谓词锁或依赖图证明结果等价于串行。PostgreSQL Serializable 就不是简单全局大锁；它通过监控读写依赖和 predicate locking 发现可能的 serialization anomaly。

第六组边界是 serializability 和“两阶段提交”。2PC 解决的是多个参与者对提交/回滚决定达成一致，避免一部分提交一部分回滚。serializability 解决的是并发事务的隔离顺序。一个系统可以用 2PC 提交非 serializable 事务，也可以在单机数据库里提供 Serializable 而不涉及跨节点 2PC。分布式事务通常同时需要原子提交和隔离控制，但两者不是同一个问题。

面试里最好避免说“Serializable 就是最强一致”。更准确的说法是：它是强事务隔离级别，但是否尊重真实时间、是否覆盖跨分片事务、是否包括只读副本、是否包含外部副作用，要看系统文档和实现范围。

## Q073. serializability 在高并发场景下可能出现哪些隐藏问题？

第一个隐藏问题是 serialization failure 激增。Serializable 系统为了保护正确性，会中止一部分无法安全串行化的事务。并发越高、读写集合越大、热点越集中，冲突越容易形成。应用如果没有统一重试机制，就会把数据库正确地拒绝提交误判成线上错误。PostgreSQL 对 Serializable 的要求很直接：应用必须准备好在 serialization failure 后重试整个事务。

第二个问题是重试风暴。很多事务失败后立刻重试，重试事务又读写同一批热点数据，冲突继续发生，系统吞吐反而下降。正确做法通常包括指数退避、抖动、限制并发、缩短事务、拆分热点、把只读事务标成 read only，必要时把强冲突资源队列化处理。

第三个问题是长事务拖慢全局并发。长事务读了很多行或范围，就会保留更大的读集合、更多历史版本、更多 predicate lock 或 SSI 依赖。它本身可能只是慢查询，但会让后续写事务更容易被判定冲突。线上症状是某个报表查询、管理后台导出、人工审核事务挂着不提交，导致业务写入频繁 serialization failure 或 MVCC 膨胀。

第四个问题是谓词查询和 phantom 成本。serializability 不能只盯住已存在的行，还要处理“满足某个条件的一组行”。比如医生值班表里查询“某时段是否已有排班”，另一个事务插入一条新排班，这不是同一行写冲突，却可能破坏约束。数据库需要 predicate lock、range lock、索引范围锁或 SSI 依赖检测。查询计划如果没走合适索引，锁或依赖范围可能被放大，冲突率也会升高。

第五个问题是唯一约束和应用协议不匹配。PostgreSQL 文档提醒过，即使在 Serializable 下，如果一些事务先检查 key 是否存在再插入，而另一些事务直接插入，仍可能出现看起来不像串行执行中会出现的 unique constraint violation。Serializable 保护的是事务历史，不替业务统一访问协议。所有写同一类约束的路径要遵守同样的读写顺序。

第六个问题是连接池和 idle in transaction。Serializable 事务打开后不提交，会占住快照、读依赖和资源。连接池过大也会把数据库推入过高并发，冲突和重试放大。PostgreSQL 的性能建议里也提到要控制活跃连接数量，不要把事务做得比维护完整性所需更长，不要让连接长期 idle in transaction。

第七个问题是外部副作用被重试放大。事务失败后重试本来是正常路径，但如果事务里已经调用过外部 API，重试就可能重复扣款、重复发消息、重复调用模型。Serializable 并不能自动撤销这些副作用。高并发下 serialization failure 增多，这类 bug 会更容易暴露。

LogServe 如果用 Serializable 数据库做调度元数据，高并发下要特别注意 task claim 的热点、workflow 状态大事务、后台查询和 worker 重试。把每个 step 的外部 LLM 调用放进数据库事务里是不合适的；更稳的设计是先提交本地状态转移或 outbox，再由幂等 worker 执行外部动作。

## Q074. serializability 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

第一个边界是 commit 结果不明。客户端提交事务时连接断开或超时，事务可能已经提交，也可能已经回滚，也可能还在恢复过程中。serializability 只描述成功提交事务之间的隔离顺序，不替客户端判断“这次提交到底成没成”。工程上要用事务状态查询、幂等请求 ID、业务唯一键或 outbox 记录来消除不确定性。

第二个边界是必须重试整个事务，而不是只重试最后一条 SQL。Serializable 下的失败通常意味着这个事务基于旧读集合做出的决策不能安全提交。PostgreSQL 文档在 Repeatable Read 和 Serializable 部分都强调，收到 serialization failure 后应中止当前事务，并从头重试。只把失败的 `UPDATE` 再执行一次，可能沿用过期业务判断。

第三个边界是读到的数据在事务提交前不能对外承诺。PostgreSQL 文档专门提醒：依赖 Serializable 防异常时，事务里读到的永久表数据，在该事务成功提交前不应被认为有效；如果事务之后因为 serialization failure 回滚，之前读出的判断也要丢弃。这个点很容易被忽略，比如事务里先查“可售库存足够”，马上把结果返回给用户，最后提交失败，就会造成体验和状态不一致。

第四个边界是崩溃恢复必须保持提交原子性和隔离元数据。数据库重启后，已经提交的事务要在 WAL/redo 中恢复；未提交事务要回滚；并发控制所需的版本、锁、提交时间戳或事务状态不能让恢复后的历史无法串行化。单机数据库主要靠 WAL、undo/redo、事务状态日志；分布式数据库还要靠复制日志、事务记录、2PC 状态、participant recovery 和 coordinator recovery。

第五个边界是重试与外部副作用。事务失败后重试是 Serializable 的常规使用方式，但事务内已经发出去的消息、邮件、支付请求、LLM 请求不会随数据库回滚。正确边界通常是：数据库事务只写状态和 outbox；事务提交成功后再由异步 worker 发送外部请求；外部请求必须带幂等键；worker 处理结果再以幂等方式写回数据库。

第六个边界是跨节点 failover 后的时间和顺序。普通 serializability 不要求真实时间顺序，但数据库仍必须保证恢复后的已提交事务集合能串行解释。分布式场景如果主从异步复制丢了已提交事务，或者新主缺少旧主的提交记录，就不只是可用性问题，还可能破坏客户端对事务结果的理解。若系统声称 strict serializability，failover 还要保留 real-time order。

第七个边界是客户端重试的幂等性。一个转账事务提交时超时，客户端重试同一转账，如果没有业务请求号，可能产生两笔合法的 serializable 转账。底层隔离级别没有错，错在业务没有定义“这两次请求是否同一次”。所以支付、订单、任务完成这类场景必须把 request id、operation id 或自然唯一约束放进事务里。

LogServe 的对应问题是：worker 完成 step 后写状态超时，不能简单认为失败再写一次；应该用 task id、attempt id、completion id 做幂等。外部 LLM 调用也不能包在一个会被数据库自动重试的事务里，否则 serialization failure 可能导致重复调用。

## Q075. serializability 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

单机数据库里，serializability 的主要瓶颈常来自锁竞争、冲突检测、内存中的版本/依赖跟踪和 I/O；分布式数据库里还会叠加网络和原子提交协议。CPU 当然会参与，比如执行计划、谓词判断、冲突图维护、序列化/反序列化，但更常见的瓶颈是“为了证明或维护可串行化顺序，系统必须协调读写”。

锁竞争很直观。两阶段锁实现 Serializable 时，读锁、写锁、范围锁可能持有到事务结束。热点行、热点索引范围、全表扫描、`SELECT FOR UPDATE` 都会让事务互相等待。等待时间变长以后，事务寿命变长，进一步扩大冲突窗口。

MVCC/SSI 实现减少了部分阻塞，但把成本转移到版本保留和依赖检测上。PostgreSQL Serializable 使用 predicate locking 和读写依赖监控来发现 serialization anomaly；这些锁不一定阻塞写入，但要占内存、要被维护，还可能因为内存不足从细粒度合并成粗粒度，导致 serialization failure 增加。也就是说，冲突不一定表现为锁等待，也可能表现为提交时被中止。

I/O 瓶颈来自 WAL、事务日志、索引维护、undo/redo、checkpoint、fsync。Serializable 本身是隔离语义，但成功提交的事务仍要满足 durability。高并发写入下，日志刷盘和索引更新会成为关键路径。长事务还会阻碍旧版本回收，让存储膨胀，间接增加 I/O。

网络瓶颈主要出现在分布式事务里。跨分片事务需要读写多个参与者，提交时可能需要 2PC；如果还要求 strict serializability，就要共识复制、时间戳协调或 commit wait。跨 AZ、跨地域时，每多一个协调轮次都会反映到尾延迟上。Spanner 这类系统把外部一致性建立在复制协议和时间 API 上，代价就是投票副本通信、leader 路由和时间不确定性等待。

内存瓶颈来自活跃事务表、MVCC 历史版本、predicate lock 表、冲突依赖图、连接上下文、排序/hash 中间结果。连接池开太大时，每个事务看似只占一点，乘起来就是很大的内存和调度压力。

所以比较稳的回答是：如果是单机 OLTP，优先看热点锁、事务长度、索引范围、WAL/fsync、MVCC 膨胀和 serialization failure；如果是分布式 OLTP，再看跨分片事务比例、2PC 轮次、共识复制延迟、跨地域 RTT 和时间戳分配。不要只说 CPU，也不要只说“数据库慢”。

## Q076. serializability 的 correctness test、stress test 和 benchmark 应该分别测什么？

correctness test 要测“所有成功提交事务是否存在一个合法串行顺序”。这类测试不能只看最后行数，也不能只跑单线程单事务。要构造会触发并发异常的业务模型：转账总额守恒、医生排班、会议室预订、库存扣减、唯一资源分配、队列领取、跨表状态机。每个事务记录读写集合、开始/提交/失败状态、返回值和业务不变量，然后检查成功提交集合是否可串行解释。

具体异常要覆盖 dirty read、non-repeatable read、phantom、lost update、write skew、read skew、predicate anomaly。Berenson 等人的经典论文之所以重要，就是因为只用 SQL 标准里的少数现象很难准确刻画真实隔离级别；测试时不能只测“脏读有没有”，还要测 Snapshot Isolation 下常见的 write skew 和谓词读写冲突。

correctness test 还要测失败语义。Serializable 系统允许中止事务，所以测试不能把 serialization failure 当作正确性失败。真正要看的是：失败事务的写是否完全不可见，成功事务是否保持不变量，客户端重试后是否能成功，事务内读到但最后回滚的数据有没有被外部使用。对于分布式数据库，还要测 coordinator crash、participant crash、leader failover、网络分区、提交响应丢失后的恢复结果。

stress test 测实现边界。它要提高并发数、热点比例、事务长度、读写集合大小、谓词查询范围、索引缺失比例、连接数、长只读事务、后台 vacuum/compaction/checkpoint、主从切换、网络抖动。观察指标包括 serialization failure 率、死锁率、锁等待、事务重试次数、连接池排队、MVCC 版本膨胀、predicate lock 内存、WAL 延迟、p99/p999 事务耗时。

benchmark 测性能时必须先固定隔离级别和事务模型。Read Committed、Repeatable Read、Snapshot Isolation、Serializable 的结果不能混在一起比较。还要区分只读事务、单行点写、跨行事务、跨表事务、谓词事务、跨分片事务。报告时不只给 TPS，还要给提交成功率、abort/retry 率、尾延迟、资源占用、冲突热点和恢复时间。Serializable 系统的有效吞吐应该看“最终成功提交的业务事务数”，而不是把失败后重试的每次尝试都算成功吞吐。

一个面试里的简洁版本可以这样说：

```text
correctness test 检查成功提交事务是否可串行化，并验证业务不变量；stress test 在高冲突、长事务、故障和恢复下观察失败率、锁等待、重试风暴和资源膨胀；benchmark 在固定隔离级别和事务形状下测 TPS、尾延迟、abort/retry 率、WAL/I/O/锁/网络成本。三者不能互相替代。
```

LogServe 如果用数据库事务实现控制面，可以把 correctness test 设计成：并发 worker 抢同一 task、并发完成同一 attempt、并发推进同一 workflow、并发写 dedup 记录，最后检查没有双领取、双完成、状态倒退。stress test 再加入 worker crash、提交超时、重试、长查询和 checkpoint。benchmark 则分开测控制事务和日志 append，不要把观测写入混到核心事务里。

## Q077. 如果要求从零实现一个简化版 serializability，你会先定义哪些不变量？

我会先定义事务模型，而不是马上写锁。事务至少要有 begin、read、write、commit、abort；事务的读写集合要明确；成功提交和失败回滚的可见性要明确；外部副作用不在事务模型内。对象也要选小一点，比如一个内存 KV 或一个账户表，不要一开始做 SQL 优化器。

第一个不变量是 atomicity：事务要么所有写都提交，要么所有写都不可见。不能出现转账扣了 A 没加 B，不能出现状态表更新了但去重表没写。简化实现可以用单线程提交锁、WAL redo/undo 或 copy-on-write map 来保证。

第二个不变量是 isolation/serial order：所有成功提交事务必须能排成一个串行顺序，并且每个事务读到的值来自这个顺序中它之前的事务。这里要特别处理事务内 read-your-writes：同一事务写了 x 再读 x，应该读到自己的写，而不是旧提交版本。

第三个不变量是 conflict 处理。最简单实现是全局大锁：事务执行或提交时完全串行，这样一定 Serializable，但并发差。稍微进阶一点，可以做 strict two-phase locking：事务获取读写锁，直到 commit/abort 才释放。再进阶是 MVCC + OCC/SSI：事务读快照，提交时验证读写冲突或依赖图，发现不可串行化就 abort。

第四个不变量是 predicate/range 保护。只保护已经读到的 key 不够。事务执行 `count(where room=R and time overlaps T)` 后再插入预订，另一个事务插入新的冲突预订，这就是谓词冲突。简化实现可以用粗粒度表锁或范围锁；真实数据库会用索引范围锁、predicate lock 或 SSI 依赖检测。

第五个不变量是 commit point。事务什么时候算成功？响应返回前，提交记录必须已经进入持久化日志或满足系统定义的复制提交条件。客户端超时后，事务状态可能 unknown，但系统内部必须能恢复出 commit/abort 结果。

第六个不变量是 abort 清理。失败事务的写不能被其他事务读到，持有的锁要释放，临时版本要丢弃，外部可见副作用不能在事务提交前发生。如果实现了重试，重试必须从新的 begin 开始，重新读取数据。

第七个不变量是 starvation 和资源边界。虽然 serializability 是安全属性，但实现不能让冲突图无限增长、锁表无限增长、旧版本永远不回收、某类事务永远被中止。需要事务超时、最大重试、连接池限制、读写集合上限和后台清理策略。

如果我从零做一个教学版，会先写全局提交锁版本，用它作为 correctness oracle；再写 2PL 或 OCC 版本，用随机事务和模型检查对比 oracle 的结果。这样比一开始就写复杂 SSI 更可靠。

LogServe 的简化版不变量可以是：同一 workflow 的状态迁移事务必须原子提交；同一 task 只能有一个有效 owner；同一 attempt 的 completion 只能生效一次；事务提交前不能触发不可回滚的 LLM 外部调用；提交失败后 worker 必须按 operation id 重试或查询结果。

## Q078. serializability 的常见误用是什么，误用后通常会产生什么线上症状？

第一个误用是以为打开 Serializable 就不用写重试。很多数据库用中止事务来保持 serializability，PostgreSQL 就明确要求应用处理 SQLSTATE `40001`。如果业务没有统一重试层，线上症状就是高并发时随机报错、用户提交失败、后台任务卡住。数据库其实在保护正确性，应用却没有按这个隔离级别的使用方式接住失败。

第二个误用是把 Snapshot Isolation 或 Repeatable Read 当成 Serializable。它们在很多场景下表现很强，读也稳定，但仍可能出现 write skew 或谓词异常。线上症状是没有脏读、没有不可重复读，单条记录看起来都对，但跨行约束破了：两个医生都请假成功、两个订单都扣到了最后一件库存、两个调度器都认为自己可以领取同一类任务。

第三个误用是把 serializability 当成 strict serializability。普通 serializability 不一定尊重真实时间，也不一定提供跨会话的“我刚提交完你必定读到”。如果业务依赖外部顺序，比如用户完成支付后立刻在另一个服务读状态，就要确认系统有没有 strict serializability、read-your-writes、session consistency 或显式读主/读时间戳机制。线上症状是刚提交成功，另一个入口短暂读不到或看到旧顺序。

第四个误用是把数据库内部事务和外部副作用放在一起。事务里发消息、扣外部款、调用 LLM，然后数据库因为 serialization failure 回滚。线上症状是数据库没有记录，但外部已经执行；重试后又执行一次。解决办法不是降低隔离级别，而是把外部动作放到事务提交后的 outbox worker，并使用幂等键。

第五个误用是事务太大。把整个请求链路、远程调用、用户交互、批量导入都塞进 Serializable 事务，线上会出现锁等待、连接耗尽、MVCC 膨胀、serialization failure 增多。Serializable 适合保护最小完整性边界，不适合包住所有业务流程。

第六个误用是所有访问路径没有统一协议。有的代码先查再插，有的代码直接插；有的事务 Serializable，有的事务 Read Committed；有的路径绕过数据库写缓存或搜索索引。线上症状是偶发 unique violation、重复资源、缓存和主库状态不一致、难以复现的约束破坏。隔离级别必须和访问协议一起设计。

第七个误用是用 benchmark 证明隔离正确。压测 TPS 很高只能说明某个负载下跑得快，不能说明没有 write skew、phantom 或恢复异常。真正要证明 serializability，需要并发历史、业务不变量和故障注入测试。

LogServe 里最危险的误用是说“用了 Serializable 数据库，所以 workflow exactly-once 就解决了”。Serializable 可以保护数据库里的状态转移，但不能保证 worker 不重复执行外部动作，不能保证消息只投递一次，也不能保证 LLM 调用可回滚。仍然要靠 shared log、attempt fencing、幂等 completion 和 replay 设计兜住运行时语义。

## Q079. serializability 在单机和分布式环境中的语义有什么差异？

单机数据库里，serializability 主要是事务隔离问题。所有数据、锁表、MVCC 版本、WAL 和事务状态都在一个数据库实例或共享存储控制范围内。实现可以用 2PL、MVCC + SSI、OCC、时间戳排序等方式，让成功提交事务等价于某个串行顺序。性能瓶颈多来自锁、版本、WAL、索引、连接数和冲突重试。

分布式数据库里，问题会多一层：事务可能跨分片、跨副本、跨地域。系统不仅要保证每个分片内部可串行化，还要保证跨分片事务的全局顺序和原子提交。这里通常需要事务协调器、2PC、共识复制、全局时间戳、事务状态记录、参与者恢复。只说“每个分片都 Serializable”不够，因为跨分片读写仍可能组合出不可串行化历史。

单机环境的失败边界相对集中：进程崩溃、磁盘故障、连接中断、事务回滚。分布式环境还要面对 coordinator crash、participant crash、leader 切换、网络分区、异步复制丢失、跨地域延迟和成员变化。commit 结果不明会更常见，恢复协议也更复杂。

普通 serializability 在两种环境中都不自动等于 real-time order。单机数据库由于客户端通常连同一个主库，很多人感觉“提交后再读就该看到”，但这往往来自实现、会话和读路径，而不是 serializability 这个词本身。分布式系统如果有 follower read、read replica、缓存、异步索引，读路径更容易把这个假设打破。要得到真实时间顺序，需要 strict serializability、external consistency、linearizable read 或明确的时间戳/水位机制。

性能差异也很明显。单机上 Serializable 的额外成本通常是锁和冲突检测；分布式上还要加网络 RTT、跨分片协调、2PC、复制 quorum、commit wait 和故障恢复。跨地域事务尤其贵，因为串行顺序和原子提交都要跨远距离通信。

语义范围也要写清。单机数据库里的“Serializable”通常指该数据库内的事务。分布式系统要问：是否覆盖所有 key range？是否覆盖跨表事务？是否覆盖 secondary index？只读事务是否同等隔离？备库读是否包括在内？异步 CDC、搜索索引、缓存和报表是否包括在内？这些边界不清楚，用户会把最强事务语义错误套到所有派生视图上。

LogServe 当前是单机多进程机制验证，更多是在本地 shared log 上建立状态顺序和重放语义。如果它以后把状态放进单机 PostgreSQL Serializable 事务，可以解决一部分本地并发元数据问题；如果要升级成多节点 shared log，就要额外设计复制、quorum、leader fencing、分布式事务或状态机复制。serializability 能保护数据库事务历史，但不是完整分布式运行时语义的替代品。
## Q080. quorum 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

Quorum 的核心目标是在多副本系统里用“足够多副本参与”来避免单个副本的状态决定整体结果。它不是一个单独产品，也不是一种数据库类型，而是一类复制读写规则：写要得到 W 个副本确认，读要得到 R 个副本响应；如果读集合和写集合必然相交，读端就有机会看到最近一次成功写入的版本。

它主要解决 correctness 和 availability/latency 的取舍。正确性来自集合交集：在固定 N 个副本里，如果 `R + W > N`，任何一次成功写的 W 个副本和后续一次成功读的 R 个副本至少有一个重叠节点。这个重叠节点应该带着那次写的版本。读协调者比较多个响应，选择最新版本，必要时触发 read repair。这个推理就是 quorum 最朴素的价值。

它也服务性能，但它本身不是性能优化算法。Dynamo 论文说得很直接：`R + W > N` 会形成 quorum-like system，而一次 get/put 的延迟由最慢的 R/W 个副本决定，所以 R 和 W 通常小于 N，以降低延迟。也就是说，quorum 的性能价值在于“不必等所有副本”，但你仍然要等到第 R 个读响应或第 W 个写确认，尾延迟会被慢副本、网络抖动、磁盘 fsync 放大。

它不是安全机制。Quorum 不负责认证、授权、恶意节点、数据加密、审计，也不默认处理 Byzantine fault。一个未授权写如果被 W 个副本接受，从 quorum 角度看仍然是一个成功写。多数工程 quorum 默认 crash fault 或 omission fault：节点慢、挂、丢消息，但不会故意说谎。

它会间接改善可维护性，因为系统可以把不同业务操作映射到不同 consistency level。Cassandra 把这叫 tunable consistency：`ONE`、`QUORUM`、`ALL`、`LOCAL_QUORUM`、`EACH_QUORUM` 等级允许应用按读写路径选择延迟、可用性和一致性的平衡。问题是，这种灵活性也会增加理解成本：不同接口如果用了不同 consistency level，系统对外语义就不再是一句话能说清。

面试里可以这样答：

```text
quorum 的目标是让多副本读写不用等所有副本，也不完全依赖单个副本。它通过读写集合交集提高正确性，通过选择 R/W 降低延迟或提高可用性。它不是完整共识，不自动提供全局顺序、事务隔离或安全性；它只是复制协议里的一种交集机制，真正语义还取决于副本集合、版本比较、冲突解决、持久化和故障恢复。
```

放到 LogServe 上，当前单机 shared log 还没有 quorum，因为只有一个日志真相源。未来如果做多副本 log，可以选择 Raft/Paxos 多数派提交，也可以设计 quorum append/read。前者给日志顺序，后者必须额外定义 index、term、版本比较和冲突处理。不能只说“用了 quorum”，就默认得到可重放的全序日志。

## Q081. quorum 的典型适用场景和不适用场景分别是什么？

Quorum 适合多副本、单 key 或单分区读写，并且业务能接受清晰的一致性档位。典型场景是 Dynamo/Cassandra 风格的 key-value、宽表、用户偏好、购物车、会话、商品状态、设备状态、配置缓存、日志索引、局部元数据。共同特点是：数据天然按 key 分片，每个 key 有固定 replica set，读写可以只联系一部分副本，系统可以通过版本、timestamp、vector clock、read repair 或 anti-entropy 修复副本差异。

它也适合控制可用性和延迟的业务。比如 `N=3` 时，`W=2,R=2` 比 `W=3,R=3` 更快，也能容忍一个副本慢或不可达；`W=1,R=1` 延迟更低，但 stale read 风险更高；`LOCAL_QUORUM` 可以让同一个 datacenter 内读写多数派相交，避免每次访问都跨地域。Cassandra 官方文档里对这些 consistency level 的描述就是这种思路：`QUORUM` 要多数副本响应，`LOCAL_QUORUM` 要本地数据中心多数副本响应，`EACH_QUORUM` 要每个数据中心多数副本响应。

它适合“数据正确性可以通过版本比较或业务合并恢复”的场景。Dynamo 论文的购物车例子很有代表性：如果分区期间出现多个版本，应用可以合并购物车内容，避免用户加到购物车的商品被静默丢掉。这里 quorum 不是为了让冲突永远不发生，而是为了控制冲突窗口，并把冲突暴露给读路径或应用。

不适合的第一类场景是必须有唯一全局顺序的协调对象。分布式锁、leader election、任务 ownership、租约、schema 变更、全局队列头、严格状态机日志，这些场景通常需要 Raft/Paxos/ZooKeeper/etcd 这类共识或线性一致服务。普通 Dynamo-style quorum 不自动给你“同一时刻只有一个 owner”，也不自动给所有命令排一个全序。

不适合的第二类场景是跨多 key 事务和复杂约束。`R + W > N` 的交集证明通常针对同一个 key 的 replica set。转账、库存加订单、跨表唯一性、工作流状态加任务表，如果跨多个 key 或多个分片，只靠每个 key 的 quorum 不足以保证整体 serializability。这里要用事务、共识状态机或业务补偿。

不适合的第三类场景是不能容忍冲突合并或 LWW 丢失的业务。账户余额、精确库存、支付状态、权限变更，如果两个并发写都成功，事后靠 timestamp 选一个“赢家”可能直接丢钱、超卖或造成越权。Cassandra 使用 LWW timestamp，文档也提醒正确性依赖时钟同步；这类数据要慎用弱冲突规则。

不适合的第四类场景是副本拓扑高度不稳定、成员视图不一致或跨地域读写语义没定义清楚的系统。Quorum 的数学证明依赖“大家讨论的是同一个 N”。如果 membership 正在变、sloppy quorum 写到了临时代持节点、读写走不同 region 的 local quorum，交集语义就会变得很脆。

LogServe 里适合弱一点 quorum 思路的是可重建的缓存、状态视图、指标、trace、只读索引。不适合的是 shared log 的顺序、actor epoch、task claim、workflow transition。这些核心控制面更像 replicated state machine 问题，需要强顺序和 fencing。

## Q082. quorum 和相近概念最容易混淆的边界在哪里？

第一组混淆是 quorum 和 consensus。Quorum 是“多少副本参与一次读写”的交集规则；consensus 是多个节点对同一个值或同一条日志顺序达成不可分叉的决定。Raft/Paxos 也用多数派 quorum，但它们还包含 leader/ballot/term、日志匹配、提交规则、选举安全和恢复规则。普通 `R + W > N` 只能说明读写集合相交，不能推出全局日志顺序。

第二组混淆是 quorum 和 linearizability。严格设计下，quorum 可以作为实现线性一致读写的一部分，但不是充分条件。还需要处理并发读写、版本顺序、旧 leader、成员变化、读到未提交值、重试去重等问题。Dynamo-style quorum 更准确地说是 quorum-like consistency 或 tunable consistency；它常常允许并发版本和读时合并，不等于线性一致对象。

第三组混淆是 strict quorum 和 sloppy quorum。Strict quorum 要读写都发生在同一个 key 的固定 N 个负责副本里。Sloppy quorum 在原副本不可达时把写放到临时代持节点上，靠 hinted handoff 以后交还。这样可用性更高，但 `R + W > N` 的固定集合交集证明不再直接成立。成功写可能暂时只在代持节点上，后续正常读不一定马上看到。

第四组混淆是 global quorum 和 local quorum。`LOCAL_QUORUM` 只保证本 datacenter 内多数派响应，它不等于全球所有副本多数派。跨地域系统里，一个 region 的 local quorum 读可能看不到另一个 region 刚提交、尚未复制过来的写。`EACH_QUORUM` 更强，但写延迟和可用性代价也更高。

第五组混淆是 quorum 和 replication factor。Replication factor 是一份数据应该有多少副本；quorum 是一次操作需要多少副本响应。`RF=3` 不代表每次读写都联系 3 个副本；`QUORUM` 通常是多数派，例如 3 副本里需要 2 个响应。面试里如果把 RF、R、W 混着说，很容易暴露概念不清。

第六组混淆是 quorum 和 durability。写达到 W 个副本确认，不一定等于所有副本都持久化，也不一定等于跨机房灾难下不丢。要看 ack 的含义：收到内存、写 commit log、fsync、apply、复制到远端、还是只是 coordinator 保存 hint。不同系统对“确认”的定义不同，durability 也不同。

第七组混淆是 quorum 和 read repair/anti-entropy。Quorum 负责当前读写要等多少副本；read repair 和 anti-entropy 负责发现并修复副本差异。Cassandra 文档里 blocking read repair 可以支持 monotonic quorum reads，但也会影响写原子性和读延迟。它们是配套机制，不是同一个概念。

一句话总结：quorum 是交集工具，不是完整一致性语义。说清楚副本集合、R/W、版本规则、冲突处理、是否 sloppy、是否 local、ack 持久化级别，才算把边界讲清。

## Q083. quorum 在高并发场景下可能出现哪些隐藏问题？

第一个隐藏问题是尾延迟被第 R/W 个响应决定。Dynamo 论文明确指出，get/put 的延迟由最慢的 R 个读副本或 W 个写副本决定。高并发下，副本排队、GC pause、磁盘 flush、网络拥塞都会让“第 2 个响应”或“第 3 个响应”变慢。平均延迟可能还可以，p99/p999 先崩。

第二个问题是协调节点成为热点。即使副本分散，某个热门 key 的读写仍可能集中到少数 coordinator、少数 replica set 或少数 token range 上。Coordinator 要 fan-out 请求、收集响应、比较版本、合并结果、触发 read repair，还要处理超时和重试。热点 key 下，quorum 不是把一个写变成免费并行，而是把一次操作变成多副本协调。

第三个问题是冲突版本增加。多副本系统允许多个副本独立接受写时，高并发并发写会产生 siblings、LWW 覆盖或 timestamp 竞争。Dynamo 用 vector clock 暴露 causally unrelated versions；Cassandra 用 timestamp LWW 解决冲突。前者把复杂度交给应用，后者可能静默丢掉一个并发写。流量越大，冲突窗口越容易被打穿。

第四个问题是 read repair 放大读延迟。一次 quorum read 如果发现副本不一致，可能要拉取完整数据、比较版本、向落后副本写修复。Cassandra 的 blocking read repair 为了 monotonic quorum reads，会在返回前等待修复达到相应 consistency level。热点数据会更快收敛，但读路径也会承担更多写入工作。

第五个问题是重试风暴。某些副本慢，客户端或 coordinator 超时后重试；重试又发起更多 fan-out；副本队列更长，导致更多超时。高并发下如果没有 deadline、hedged request 限制、退避、熔断和 backpressure，quorum 系统会把局部慢放大成集群抖动。

第六个问题是跨地域 local quorum 的语义被误用。业务为了降低延迟选择 `LOCAL_QUORUM`，高并发写分散在多个 region，复制延迟和冲突会更明显。用户在 A 区写完，到 B 区读，可能读不到；两个 region 同时写同一 key，LWW 或合并规则会决定结果。这不是公式算错，而是 quorum 范围本来就是 local。

第七个问题是 repair/backfill/compaction 和在线流量互相干扰。反熵修复、hint replay、节点替换、bootstrap、decommission、compaction 都会消耗网络、磁盘和 CPU。它们可能让正常 quorum 的第 R/W 个响应变慢，造成暂时性的可用性下降。

LogServe 如果未来做多副本日志，最危险的是把每个 step/token 都放在同一条 quorum 写路径上。高并发下应该区分核心控制日志和可异步派生数据。核心日志需要强顺序，观测数据可以批量、异步或弱一致处理。

## Q084. quorum 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

第一个边界是写成功的持久化含义。客户端收到成功，意味着 W 个副本做了什么？只是收到请求，写入内存，写 commit log，fsync，还是 apply 到可读状态？如果 ack 太早，副本崩溃重启后可能丢掉已经确认的写，quorum 交集也帮不了你。工程文档必须明确 ack point。

第二个边界是超时后的 unknown 状态。写请求超时，不代表写失败；可能有 0 个副本写入，也可能有 W-1 个副本写入，也可能已经达到 W 但响应丢了。客户端如果简单重试，就可能产生重复写、并发版本或 timestamp 覆盖。解决办法包括幂等 request id、条件写、CAS、版本 token、客户端重读确认。

第三个边界是 failed write 的可见性。Cassandra read repair 文档提到一个很实际的情况：一次写没有达到 quorum，但已经写到少数副本；后续某次 quorum read 可能读到它并触发修复。Blocking read repair 用来维持 monotonic quorum reads，避免第二次 quorum read 比第一次更旧。但这也说明“写失败”不等于“没有任何副本写入”。

第四个边界是节点重启后的 hint、repair 和反熵。节点宕机期间错过写入，恢复后可能靠 hinted handoff 补交，也可能靠 read repair 或 anti-entropy repair 收敛。恢复窗口里，某些副本仍然旧；如果读写 consistency level 太低，就会暴露 stale read。Hint 所在节点如果在 handoff 前也失败，还要考虑 hint 的持久性和过期策略。

第五个边界是 membership 变化。节点加入、移除、替换、token range 迁移时，“这个 key 的 N 个副本是谁”会变化。若读写双方 membership 视图不一致，可能一个请求按旧 replica set 算 quorum，另一个按新 replica set 算 quorum，交集证明就不稳。严格系统要用配置版本、joint consensus、repair/bootstrap 完成条件来收紧这个边界。

第六个边界是 tombstone 和删除。删除也是写，需要版本化和复制。如果一个副本错过 tombstone，另一个副本保留旧值，read repair 或 repair 配置不当可能导致 deleted data reappears。Cassandra 这类系统的 tombstone、gc grace、repair 之所以麻烦，就是因为删除必须参与 quorum 和反熵语义。

第七个边界是重试与业务幂等。Quorum 只能说明副本响应数，不知道业务请求是否同一次。一次“完成任务”写成功但响应丢失，worker 重试又提交一次，如果没有 operation id 或 compare-and-set，就会得到两个合法写。底层副本没有错，业务语义错了。

LogServe 对应的设计边界是：未来如果 completion 写 quorum 超时，worker 不能简单假设失败再写一条。它应该携带 task id、attempt id、completion id，读回确认状态，或者用 CAS/fencing 保证旧 attempt 和重复 completion 不会生效。

## Q085. quorum 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

Quorum 的第一瓶颈通常是网络，尤其是尾延迟。一次 quorum read/write 至少要和多个副本通信，延迟由第 R 个或第 W 个响应决定。跨 AZ、跨机架、跨地域时，网络 RTT、丢包、拥塞、连接排队都会直接进入用户请求路径。Spanner 文档说写入需要多数 voting replicas 同意才能提交，并且每次写都需要 voting replicas 之间通信；这类强 quorum 写的延迟下限就是网络距离。

第二个瓶颈是 I/O。副本确认写之前是否要写 commit log、WAL、SSTable memtable、fsync，决定了写 quorum 的稳定性和延迟。低延迟系统可能先写内存或 OS buffer，但 durability 语义就要说清。高写入下，磁盘 flush、commit log segment、compaction、snapshot、repair 会影响第 W 个副本的响应。

第三个瓶颈是协调者 CPU 和内存。Coordinator 要做 token routing、请求 fan-out、response aggregation、digest comparison、version reconciliation、read repair、hint 记录、超时管理。大对象、多版本、宽分区、大量并发连接会增加 CPU 和内存压力。读到多个 siblings 时，返回给应用或合并版本也需要额外开销。

第四个瓶颈是热点锁和队列。副本内部可能有 memtable 锁、partition/key-level 并发控制、commit log append 锁、connection write queue、compaction backlog。Quorum 让一个请求访问多个副本，所以任何一个关键副本上的排队都可能影响整体尾延迟。

第五个瓶颈是后台修复流量。Read repair、hinted handoff、anti-entropy repair、bootstrap、decommission、rebalance 都会和线上 quorum 读写抢网络和磁盘。高并发下，如果后台流量没有限速，用户请求的第 R/W 个响应会变慢，进而引发超时和重试。

第六个瓶颈是跨地域拓扑。`LOCAL_QUORUM` 可以降低跨地域等待，但牺牲全球最新可见性；`EACH_QUORUM` 或 global quorum 语义更强，但每次写要等多个 datacenter 多数派，尾延迟和不可用窗口都会扩大。Spanner 文档也解释了为什么多地域配置不会让所有副本都成为 read-write voting replicas：增加 voting replicas 会增大 write quorum，并增加地理分布下的网络延迟。

因此面试回答可以分层：单机副本内部看 I/O、锁和 CPU；同机房 quorum 看网络尾延迟和热点副本；跨 AZ/region quorum 看 RTT、leader/voting replica 位置和后台修复流量。Quorum 不是“多数派所以一定慢”，也不是“只等多数所以一定快”，它的性能取决于第 R/W 个响应在真实拓扑里的成本。

LogServe 当前更容易遇到的是本地 log append I/O、队列积压和 replay 视图延迟。只有多副本化以后，quorum 的网络尾延迟、写确认点、repair/backfill 和 leader/voting replica 布局才会成为主瓶颈。

## Q086. quorum 的 correctness test、stress test 和 benchmark 应该分别测什么？

这三个测试经常被混在一起。面试里如果只说“我会压测一下 quorum”，基本是不够的。Correctness test 问的是语义有没有错，stress test 问的是高并发和故障交织时会不会暴露竞态，benchmark 问的是在明确配置下成本是多少。三者都重要，但它们回答的不是同一个问题。

Correctness test 要从不变量出发。对于一个最小 quorum key-value/register，可以先定义：

```text
同一个 membership view 下，只要 R + W > N，
一次已经成功返回的写，后续 quorum read 的候选副本集合必须和它至少有一个交集。
读操作拿到多个版本时，必须按版本规则返回最新可见值，或者把并发冲突显式交给上层处理。
```

测试要覆盖的是这些语义是否真的成立，而不是“请求没有报错”。最基本的 case 是单 key 写后读、并发写后读、少数副本落后后读、某个副本返回旧版本、读修复后再读。更难的是失败边界：写请求达到 W 个副本但响应丢失，客户端看到 timeout；写只到 W-1 个副本，后续 read repair 是否可能让失败写短暂可见；副本崩溃重启后是否丢失已经 ack 的写；删除 tombstone 是否能压过旧值；membership view 变化时，新旧副本集合的 quorum 是否还有交集。这里最好记录完整 history：每个操作的 invoke/ok/fail/info、key、value、version、client id、request id、开始和结束时间。然后用模型检查，比如单对象可以按 linearizability 或 monotonic quorum read 去检查，事务对象则要换成 serializability 或 stronger model。Jepsen/Knossos 这一类工具的价值就在这里：它不是看日志里有没有 panic，而是把实际历史拿去问“有没有一个合法串行解释”。

Correctness test 还要故意制造“不舒服”的场景。比如 coordinator 写到两个副本后崩溃，第三个副本没有收到；客户端重试同一个 request id；两个 coordinator 用不同 timestamp 写同一个 key；一个副本时钟跳到未来；read repair 正在修复时另一个写进来；hinted handoff 的目标副本恢复后又马上重启。真正的 quorum bug 很少出现在健康路径，更多出现在“操作结果未知，但系统继续向前跑”的缝隙里。

Stress test 的目标不是证明正确，而是把这些缝隙放大。它应该把并发、故障、重试、后台修复和热点放在一起打。典型配置可以这样设：

```text
RF=3，R=2，W=2
80% read，20% write
一部分 key 均匀分布，一部分 key 极热
随机注入网络延迟、单向丢包、节点重启、磁盘慢写、GC pause、hint replay、repair
客户端带 deadline 和 retry，但必须携带 request id
```

观察指标不能只看 QPS。要看 stale read 次数、monotonic read 违反次数、读到未确认写的次数、lost update、siblings 数量、read repair 触发率、hint backlog、coordinator timeout、retry amplification、单副本队列长度、digest mismatch、后台 repair 吞吐和 p99/p999 延迟。Stress test 的结果如果只是“跑了一小时没挂”，价值有限；更有用的是能回答“在 30% 请求超时、一个副本反复重启、热点 key 持续写入时，系统有没有违反我们承诺的读写语义”。

Benchmark 则要把故障关掉或固定住，稳定地测成本。Benchmark 必须把配置写清楚：RF、R/W、consistency level、value size、key 分布、读写比例、客户端并发、连接数、是否开启 read repair、是否开启 hint、是否跨 AZ/region、磁盘类型、fsync 策略、压缩/compaction、是否预热、数据集是否超过内存。没有这些上下文，QPS 和 p99 没法比较。

Benchmark 最少应该分四组：

| 组别 | 目的 | 关注指标 |
|---|---|---|
| healthy path | 没有故障时 quorum 的基本成本 | throughput、p50/p95/p99/p999、CPU、I/O、网络 |
| degraded path | 少数副本慢或不可用时是否仍能服务 | timeout、retry、tail latency、成功率 |
| consistency-level comparison | `ONE`、`QUORUM`、`ALL` 或 `LOCAL_QUORUM` 的代价差异 | 延迟/可用性/陈旧读的变化 |
| repair/backfill interference | 后台修复和前台读写互相影响 | repair backlog、读写 p99、磁盘和网络占用 |

Cassandra 官方的 `cassandra-stress` 就是 benchmark/load-test 工具，而不是 correctness checker。它可以压 write、read、mixed 和自定义 CQL schema，很适合回答“这个数据模型和 consistency level 在这组机器上能跑到什么水平”。但它不会替你证明 `R + W > N` 的实现真的满足线性一致，也不会替你处理 timeout 后的 unknown 语义。Dynamo 论文里反复看 99.9th percentile，也说明 benchmark 不能只报平均值。Quorum 的代价常常藏在尾延迟里。

所以我会把三类测试分开汇报：correctness test 证明语义边界有没有被打破；stress test 找高并发和故障组合下的竞态；benchmark 给出在固定配置下的吞吐、尾延迟和资源成本。对于 LogServe，如果未来做多副本 shared log，正确性测试应先围绕 append index 唯一性、committed entry 不可变、旧 epoch writer 被 fencing、timeout 后 completion 幂等这些不变量展开；benchmark 要等语义先定下来再做，否则很容易测出一组漂亮但没有意义的数字。

## Q087. 如果要求从零实现一个简化版 quorum，你会先定义哪些不变量？

我不会先写网络代码。Quorum 系统最容易写成“请求发给 N 个节点，等到 R/W 个响应就返回”，这只是外壳。真正要先定的是不变量，因为后面的编码、测试、日志和排障都要围绕它们转。

第一条是 membership/view 不变量。对某个 key 来说，当前 view 下负责它的副本集合必须明确：`replicas(key, view) = {n1, n2, n3}`。读写请求都要带 view 或能从 coordinator 上确定同一个 view。否则读按旧副本集合算 quorum，写按新副本集合算 quorum，两边可能完全不相交。简化版可以先不支持动态 membership；如果支持，至少要用配置版本，并禁止读写在未确认的新旧 view 之间随意穿越。etcd 的运行时重配置文档要求多数成员可用、变更按顺序做，本质上就是在保护这个边界。

第二条是 quorum intersection 不变量。对于同一个 key、同一个 view，任意一个成功写 quorum 和任意一个读 quorum 必须相交：

```text
for all Qw, Qr:
  Qw subset replicas(key, view)
  Qr subset replicas(key, view)
  |Qw| >= W
  |Qr| >= R
  require Qw intersect Qr != empty
```

最常见的选择是 majority quorum，也就是 `R = W = floor(N/2) + 1`。Dynamo/Cassandra 风格还允许调 R/W，但只要想得到“成功写对后续 quorum read 可见”的语义，就要满足 `R + W > N`，并且 N 说的是同一个固定副本集合。这个“同一个”很重要，sloppy quorum、local quorum、membership change 都会削弱这个证明。

第三条是版本单调不变量。每个写必须有可比较的版本，读到多个副本值时不能随机挑一个。简化版最好用 `(epoch, counter, nodeID)` 或 leader 分配的单调 version，而不是裸 wall-clock timestamp。墙上时钟会漂移，两个 coordinator 也可能同时写。版本规则要回答三个问题：谁更新版本，版本如何比较，两个版本无法比较时是报冲突、保留 siblings，还是用 LWW 覆盖。Dynamo 用 vector clock 暴露并发分支，Cassandra 用 timestamp/LWW 收敛。两种都可以，但语义完全不同。

第四条是 ack durability 不变量。客户端收到写成功，意味着什么？至少要写进 W 个副本的稳定状态，还是只写到了内存？如果副本 ack 后立刻崩溃重启，已经确认的写能不能回来？简化版也要把 ack point 写清楚。否则 correctness test 会出现很尴尬的结果：quorum 数学上相交了，但相交的那个副本重启后把值丢了。

第五条是 read merge 不变量。一次 quorum read 收到多个响应后，要返回满足版本规则的结果。如果有一个副本返回版本 10，另一个返回版本 8，读不能因为版本 8 先到就返回旧值。若读到了两个并发版本，系统要么保留并返回冲突，要么调用确定的 merge 规则。读修复也要服从这个不变量：它只能把更新的、合法的版本传播出去，不能把局部旧值写回覆盖新值。

第六条是 timeout unknown 不变量。写超时不是写失败。它可能没有到达任何副本，也可能已经到了 W 个副本但响应丢了。客户端重试时必须携带 request id 或 operation id，服务端要能去重，或者提供 CAS/fencing 让业务层判断旧 attempt 不能再生效。没有这条，不变量会在业务层被打破：底层 quorum 每次都“正确”写入，但用户看到重复扣款、重复完成任务、计数器跳两次。

第七条是 no-ghost-value 不变量。系统不能返回从来没有被成功写入或处于可解释 unknown 状态的值。网络乱序、read repair、hint replay、旧 coordinator 重试，都不能凭空制造一个没有来源的版本。实现上要给每个版本保存 origin：client request id、coordinator、version、write timestamp、view、ack 状态。排查线上问题时，这些字段比一句“quorum read returned X”有用得多。

第八条是 delete/tombstone 不变量。删除不是物理删除，而是一个带版本的写。只要还有可能存在旧副本，tombstone 就不能过早丢弃。否则旧值会通过 repair 或 hint replay 回来，看起来像“删除的数据复活”。很多 Dynamo/Cassandra 系统里的删除复杂性都来自这里。

第九条是 failure model 不变量。简化版可以先声明只处理 crash-stop/crash-recovery，不处理 Byzantine；网络可以延迟、丢包、乱序、重复，但节点不会恶意伪造值；磁盘如果 ack 后丢数据，算实现 bug 或硬件超出模型。这个边界要提前写下来。否则面试官问“节点返回假数据怎么办”，你会被迫临时扩展到 Byzantine quorum，那是另一套系统。

第十条是可观测性不变量。每次读写都要记录 key、view、coordinator、目标副本、成功副本、失败副本、版本、request id、deadline、返回值来源。Quorum bug 排查最怕只有客户端错误码，没有副本级证据。你要能回答：这次读到底读了哪两个副本？它们各自返回了什么版本？为什么 coordinator 选了这个值？

如果把它落到 LogServe 的未来多副本 shared log，我会再加三条更强的不变量：同一个 log index 只能提交一个 entry；committed entry 一旦对外可见就不能被覆盖；任何旧 epoch 的 writer 即使后来恢复，也不能再提交新的控制面 entry。这三条更接近 consensus/replicated log 的语义，不能只靠普通 `R + W > N` 糊过去。

## Q088. quorum 的常见误用是什么，误用后通常会产生什么线上症状？

第一个误用是把 `R + W > N` 当成线性一致的充分条件。这个公式只证明读写集合有交集，不证明版本选择、并发写、失败写、membership change、读修复、重试幂等都正确。线上症状通常是：刚写成功马上读，有时能读到旧值；两个客户端并发写，同一个 key 最后丢了一次更新；用户刷新页面时状态从新变旧；日志里看不到异常，因为每次操作都拿到了足够副本响应。

第二个误用是把 sloppy quorum 当 strict quorum。Sloppy quorum 会把写临时放到非首选副本上，再靠 hinted handoff 交还。它适合提高写可用性，但不能拿 strict quorum 的固定集合交集证明去套。误用后常见症状是：故障窗口里写成功，故障恢复前正常读路径看不到；hint replay backlog 很长；某些 key 在节点恢复后突然出现旧版本或冲突版本；业务方觉得“系统明明返回成功，为什么另一个机房读不到”。

第三个误用是混淆 local quorum 和 global quorum。`LOCAL_QUORUM` 只保证本 datacenter 内多数派响应，不等于所有 region 的多数派。Cassandra 文档也把 `LOCAL_QUORUM` 和 `EACH_QUORUM` 分开定义。线上表现很直观：用户在 A 区写完，流量被切到 B 区后读不到；跨 region 灰度时订单状态、会话状态、配置状态出现短暂倒退；排查时每个 region 内看起来都“符合 quorum”，但全局体验不一致。

第四个误用是用 wall-clock timestamp 做 LWW，却没有处理时钟漂移。一个节点时钟快几分钟，它写出的版本会压过后续正常写；一个节点时钟慢，又可能让自己的更新永远输掉。删除也会受影响，未来时间的 tombstone 可能长时间压制新写，或者旧值在 tombstone 过期后被 repair 带回来。线上症状是 lost update、数据复活、某个节点恢复后大量 LWW 冲突、用户看到字段被旧请求覆盖。

第五个误用是把 timeout 当失败。写 quorum 超时后，客户端直接重试一个新的业务操作，没有 request id，没有 CAS，也没有读回确认。这样会产生重复订单、重复任务完成、计数器多加、状态机跳过中间状态。更麻烦的是，底层日志会显示“两次写都是合法的”，真正错的是业务层把 unknown 当 fail 处理了。

第六个误用是 membership 变更太随意。扩容、缩容、替换节点时，如果读写双方看到的副本集合不同，quorum 交集可能不存在。线上症状是扩容期间偶发 stale read，某个 token range 上写成功但读不到，repair/backfill 后数据又突然出现。严重时会表现成 split-brain：两个集合都觉得自己拿到了多数派。

第七个误用是 ack 太早。副本收到请求就 ack，但还没有写 WAL/commit log；coordinator 拿到 W 个 ack 后返回成功；随后几个副本崩溃，确认过的写没了。线上症状是“成功写丢失”，这比 stale read 更难解释，因为客户端手里有成功响应。Quorum 只能保护已持久化的副本集合，保护不了提前承诺。

第八个误用是只做 read repair，不做完整 anti-entropy repair。Read repair 依赖读路径触发，冷 key 长时间没人读就可能一直不一致。Cassandra hints 文档也说 hint 是 best effort，不能替代 anti-entropy repair。线上症状是热数据看起来很快收敛，冷数据在备份、扫描、迁移或大查询时突然暴露旧值；repair backlog 越积越大，后台流量一跑就把前台 p99 打高。

第九个误用是把 quorum 当分布式锁或 leader election。多数副本响应不等于你拥有共享资源的独占权。没有 lease 语义、fencing token、session 过期和旧 owner 隔离，旧 holder 在网络恢复后仍可能继续写。线上症状是两个 worker 同时处理同一任务，两个 leader 同时发调度命令，或者旧 coordinator 延迟到达的写覆盖新 leader 的决策。

第十个误用是 benchmark 和语义对不上。测试时用 `ONE`，上线宣传“quorum”；健康路径 benchmark 里关掉 repair、hint replay、compaction，线上故障后 p99 暴涨；只报平均延迟，不报 p999 和 timeout。症状不是立刻错，而是容量评估失真：节点一慢，请求 fan-out 和 retry 把集群拖进雪崩。

我在面试里会把这些症状说具体：stale read、monotonic read violation、lost update、duplicate write、siblings 暴涨、hint backlog、digest mismatch、read repair 激增、跨 region 读己之写失败、删除数据复活、扩缩容期间局部不可用。Quorum 的误用通常不是“系统完全挂了”，而是用户看到状态偶尔倒退、偶尔重复、偶尔丢。正是这种偶发性，让它比普通 crash 更难排。

## Q089. quorum 在单机和分布式环境中的语义有什么差异？

单机环境里的“quorum”通常只是一个类比。比如一台机器上写三份本地文件，至少两份成功才返回；或者一个进程里维护三个内存副本，多数投票决定读值。这可以提高对局部介质损坏、单个线程 bug 或单个文件损坏的容忍度，但它不是分布式 quorum 的完整语义。因为故障相关性完全不同：同一台机器断电、内核 panic、文件系统 bug、进程地址空间被破坏，可能同时影响所有副本。

单机里没有真正的网络分区。线程慢、锁竞争、磁盘慢写当然会发生，但进程可以用 mutex、atomic、WAL、fsync、rename、file lock 这些本地机制建立强顺序。超时也更容易解释：一个 goroutine 卡住了，可以通过调度、堆栈和锁状态定位；一个本地文件写失败，错误通常来自明确的 syscall。分布式环境里，超时只表示你没在 deadline 内拿到响应，不能区分节点死了、网络慢了、响应丢了、对方已经处理但回包丢了。

分布式 quorum 的核心是跨故障域的交集。副本在不同进程、机器、机架、AZ 或 region；消息可能延迟、丢失、重复、乱序；节点可能崩溃后带着旧状态回来；不同 coordinator 可能同时处理同一个 key；membership view 还可能变化。这里的 quorum 不只是“多数表决”，而是在一个异步、部分失败的环境里，用集合交集为读写可见性提供最低限度的证据。

单机环境里，全局顺序通常便宜。你可以让一个锁保护 map，让一个 WAL append 决定顺序，让一个线程串行消费队列。分布式环境里，全局顺序要贵得多。要么用 leader/consensus，把写排成一条日志；要么用 Dynamo/Cassandra 风格，让多个副本接受写，再用版本、冲突合并、read repair 和 anti-entropy 收敛。两条路都可能用到 quorum，但语义不一样。前者的 quorum 是提交日志项的一部分，后者的 quorum 是可调一致性和可用性之间的取舍。

单机里的 durability 也更窄。三份本地文件写到同一块盘，不能防机器丢失；写到同一文件系统，不能防文件系统级 bug；写到同一 OS buffer，不能防掉电。分布式 quorum 至少要求副本跨独立故障域，否则 `N=3` 只是数字好看。真正设计时要问：这三个副本是不是在不同机器？不同电源？不同 rack？不同 AZ？ack 前有没有落到稳定存储？这些问题决定了 quorum 的承诺是否有工程意义。

测试方法也不同。单机 correctness test 可以相对确定：构造输入、检查输出、跑 race detector、模拟 crash recovery。分布式 correctness test 要记录历史，注入网络故障，打乱消息顺序，让节点在不同时间崩溃和恢复，然后再检查模型。很多 bug 在单机 mock 里永远不会出现，因为 mock 默认消息可靠、时间同步、所有节点共享同一个真相。

LogServe 当前的 shared log 是单机多进程机制验证，不是多机 quorum log。它可以讨论 log-first、replay、redelivery、actor epoch fencing，但不能说已经提供跨机器 quorum 语义。如果以后把 logd 做成多副本，语义会变：append 成功要定义需要几个副本 ack，旧 leader 如何 fencing，membership 如何变更，读是走 leader、majority read 还是 follower stale read。这个变化不是把本地 log 写三份那么简单，而是从本地恢复问题进入分布式一致性问题。

所以回答这个问题时，我会直接区分：单机 quorum 更像本地冗余和投票，主要防局部损坏；分布式 quorum 是跨故障域的读写交集和可见性协议，必须面对分区、乱序、超时、membership 和副本恢复。两者都可以用“多数”这个词，但不能共享同一套正确性假设。

## Q090. sloppy quorum 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

Sloppy quorum 的核心目标是提高可用性，尤其是写可用性。更准确地说，它想解决的是：某个 key 的首选副本里有节点暂时不可用时，系统仍然尽量接受写入，并把这次写暂存在其他健康节点上，等原副本恢复后再通过 hinted handoff 交还。

Dynamo 论文把 sloppy quorum 和 hinted handoff 放在一起，用来处理 temporary failures。Cassandra 的 hints 文档也说，副本不可用时 coordinator 会把 hint 暂存在本地，稍后回放给目标副本；同时它也明确提醒 hint 是 best effort，不能像 anti-entropy repair 那样保证最终一致。这个边界很关键。Sloppy quorum 不是把 strict quorum 变得更强，而是承认原副本集合暂时不完整，然后用临时代持换取写路径继续可用。

可以用一个小例子说明。某个 key 的首选副本是 A、B、C，`N=3, W=2`。如果 C 不可用，strict quorum 仍然可以在 A、B 上成功；如果 B、C 都不可用，strict quorum 写不了。Sloppy quorum 会沿着 preference list 找到下一个健康节点 D，把本该属于 B/C 的写临时放到 D，并记录这个 hint。客户端可能仍然得到成功响应。等 B/C 恢复，D 或 coordinator 再把 hint 交回去。

它主要解决的不是强正确性问题。强正确性关心的是线性一致、单调读、提交顺序、冲突是否可解释。Sloppy quorum 反而会削弱 strict quorum 的集合交集证明，因为写可能落在临时节点上，后续读如果只读首选副本，未必马上看到这次写。为了让系统仍然可用，设计者必须接受更长的一致性窗口，并准备处理多版本、冲突、读修复和反熵修复。

它也不是安全性方案。Sloppy quorum 不处理恶意节点、伪造响应、权限绕过或 Byzantine 行为。节点只是 temporarily unavailable，不是 hostile。Dynamo 原始假设也是非恶意内部服务环境。把 sloppy quorum 用在安全决策上，比如权限状态、资金状态、锁所有权，本身就很危险。

它对性能有影响，但性能不是它最核心的目标。健康路径下，sloppy quorum 可能通过就近健康节点减少等待，也可能因为 preference list、hint 写入、冲突版本和后台 replay 增加成本。故障路径下，它把“直接失败”变成“先接受、以后修复”，用户请求成功率更高，但后台系统会背上债：hint backlog、repair 流量、更多版本和更复杂的读路径。换句话说，它改善的是可用性曲线，不是免费降低延迟。

可维护性也不是主要目标。实际情况甚至相反：sloppy quorum 增加了运维要看的东西。hint 存在哪里、保留多久、目标副本恢复后如何限速回放、hint 节点自己失败怎么办、repair 多久跑一次、冲突版本谁合并，这些都需要监控和 runbook。系统更可用，但也更难解释。

所以我会把 sloppy quorum 归类为 availability-first 的容错技术。它在弱一致、可合并、业务能容忍短暂陈旧的场景里很有价值；它不提供线性一致，也不替代 consensus。面试时如果让我在“正确性、性能、安全性、可维护性”里选，我会说：第一目标是可用性；它维护的是一种较弱正确性合同，即 accepted write 尽量不丢并最终交还；性能可能改善也可能变差；安全性基本不是它要解决的问题；可维护性成本会上升。

对于 LogServe，sloppy quorum 如果将来出现，比较适合放在派生数据上，比如 trace、指标、可重建索引、对象缓存。shared log、actor epoch、task claim、workflow transition 这些控制面数据不适合 sloppy quorum，因为它们需要明确顺序和 fencing。这里宁可在分区时拒绝一部分写，也不能让两个执行者都觉得自己拿到了“成功”。

## Q091. sloppy quorum 的典型适用场景和不适用场景分别是什么？

Sloppy quorum 适合的第一类场景是“可用性比立即读到最新值更重要，而且冲突可以合并”。Dynamo 论文里的购物车例子很典型：用户在故障期间添加商品，系统更愿意接受这次更新，而不是告诉用户稍后再试。即使后来出现两个购物车版本，应用也可以把商品集合合并，再让业务规则处理数量、删除和展示问题。会话偏好、用户设置、推荐特征、最近访问记录、点赞计数、购物车草稿，都有类似空间。

第二类是短暂故障很多、长期分裂较少的环境。Sloppy quorum 对“一个副本重启几分钟”“某个 rack 网络抖动”“滚动升级期间部分节点不可用”比较友好。Hinted handoff 能缩短副本不一致的时间窗口。Cassandra 文档里 hint 默认也有保留窗口，超过窗口后就要靠 read repair 或 anti-entropy repair。也就是说，它假设目标副本会在可接受时间内回来，并且系统还有后续修复机制。

第三类是对象粒度简单、跨 key 事务要求弱的存储。Dynamo 原始模型就是 primary-key blob，不提供跨对象事务和隔离。Sloppy quorum 在这种模型里容易讲清楚：一个 key 有多个版本，读时合并或返回冲突。如果业务要求一次更新多个 key，并且必须原子可见，sloppy quorum 会把问题复杂度放大很多。

第四类是可以接受最终收敛的派生数据。比如缓存、搜索索引、统计视图、trace 采样、异步 materialized view、可重放的中间结果。这些数据即使短时间旧一点，核心事实仍然在更强的系统里。LogServe 里如果把 LLM 输出对象做成 content-addressed blob，或者把观测指标做成可补偿写入，sloppy quorum 有讨论空间；但前提是主日志里仍能重建真相。

不适合的第一类场景是强顺序控制面。Leader election、分布式锁、任务 claim、workflow 状态迁移、actor epoch、共享日志 append、配置发布，这些都不能只追求“尽量写进去”。它们需要回答谁有权写、哪个版本生效、旧 owner 是否被 fencing。Sloppy quorum 会让旧写、临时写、hint replay 和新 owner 的操作交织在一起，排错成本很高，语义也容易破。

不适合的第二类是资金、库存、配额、权限这类不能默默合并的状态。库存不能因为两个 region 都可用就卖出两份；权限撤销不能因为目标副本不可用就延迟很久才生效；余额扣减不能靠 LWW 覆盖。这里即使牺牲可用性，也通常要用事务、条件写、共识日志、强约束或业务补偿，把边界写死。

不适合的第三类是冲突不可解释，或者团队没有能力处理冲突的场景。很多系统嘴上说 eventual consistency，实际应用代码只会处理单版本。等读到 siblings 或 LWW 覆盖时，要么丢数据，要么把内部冲突暴露给用户。Sloppy quorum 把复杂性从写路径转移到读路径和业务合并逻辑；如果业务没有合并语义，就不要选它。

不适合的第四类是长时间分区、hint 不可靠、repair 不稳定的环境。Hint 节点自己也会失败，hint 有过期窗口，后台 repair 也会消耗大量资源。Cassandra 文档明确说 hint 是 best effort，不保证最终一致；真正要兜底还得靠 anti-entropy repair。如果运维上没有稳定 repair、监控和容量预留，sloppy quorum 会把短故障变成长时间数据分歧。

不适合的第五类是跨 region 强读己之写体验。用户在 A 区写完马上到 B 区读，如果 B 区必须立刻看到，sloppy quorum/local quorum 都不够。你可以做 session stickiness，让用户短时间回到 A 区；也可以做 global consensus，付出更高延迟；还可以调整产品语义，告诉用户同步中。不能一边要求全球立即可见，一边用 sloppy quorum 承诺高可用低延迟。

我会用一句话收束：sloppy quorum 适合“宁可稍后合并，也不要当场拒绝”的数据；不适合“必须现在唯一、现在有序、现在撤销生效”的数据。把这个边界讲清楚，才不会把一个 availability 技术误包装成强一致方案。

## 参考资料

- Seth Gilbert and Nancy Lynch, [Perspectives on the CAP Theorem](https://groups.csail.mit.edu/tds/papers/Gilbert/Brewer2.pdf)
- Maurice P. Herlihy and Jeannette M. Wing, [Linearizability: A Correctness Condition for Concurrent Objects](https://cs.brown.edu/~mph/HerlihyW90/p463-herlihy.pdf)
- Leslie Lamport, [Time, Clocks, and the Ordering of Events in a Distributed System](https://lamport.azurewebsites.net/pubs/time-clocks.pdf)
- Leslie Lamport, [How to Make a Multiprocessor Computer That Correctly Executes Multiprocess Programs](https://lamport.azurewebsites.net/pubs/multi.pdf)
- Giuseppe DeCandia et al., [Dynamo: Amazon's Highly Available Key-value Store](https://www.allthingsdistributed.com/files/amazon-dynamo-sosp2007.pdf)
- Werner Vogels, [Eventually Consistent - Revisited](https://www.allthingsdistributed.com/2008/12/eventually_consistent.html)
- Apache Cassandra Documentation, [Dynamo](https://cassandra.apache.org/doc/stable/cassandra/architecture/dynamo.html)
- Apache Cassandra Documentation, [Guarantees](https://cassandra.apache.org/doc/stable/cassandra/architecture/guarantees.html)
- PostgreSQL Documentation, [Transaction Isolation](https://www.postgresql.org/docs/current/transaction-iso.html)
- Hal Berenson et al., [A Critique of ANSI SQL Isolation Levels](https://arxiv.org/abs/cs/0701157)
- Diego Ongaro and John Ousterhout, [In Search of an Understandable Consensus Algorithm](https://raft.github.io/raft.pdf)
- Leslie Lamport, [Paxos Made Simple](https://lamport.azurewebsites.net/pubs/paxos-simple.pdf)
- Apache Cassandra Documentation, [Read repair](https://cassandra.apache.org/doc/stable/cassandra/managing/operating/read_repair.html)
- Apache Cassandra Documentation, [Hints](https://cassandra.apache.org/doc/stable/cassandra/managing/operating/hints.html)
- Apache Cassandra Documentation, [Cassandra Stress](https://cassandra.apache.org/doc/stable/cassandra/tools/cassandra_stress.html)
- PostgreSQL Documentation, [Failover](https://www.postgresql.org/docs/current/warm-standby-failover.html)
- PostgreSQL Documentation, [Log-Shipping Standby Servers](https://www.postgresql.org/docs/current/warm-standby.html)
- Apache ZooKeeper Documentation, [ZooKeeper Recipes and Solutions](https://zookeeper.apache.org/doc/current/recipes.html)
- Apache ZooKeeper Documentation, [Programmer's Guide: Consistency Guarantees](https://zookeeper.apache.org/doc/current/zookeeperProgrammers.html#ch_zkGuarantees)
- etcd Documentation, [API reference: concurrency](https://etcd.io/docs/v3.5/dev-guide/api_concurrency_reference_v3/)
- etcd Documentation, [API guarantees](https://etcd.io/docs/v3.5/learning/api_guarantees/)
- etcd Documentation, [Runtime reconfiguration](https://etcd.io/docs/v3.5/op-guide/runtime-configuration/)
- Redis Documentation, [Distributed Locks with Redis](https://redis.io/docs/latest/develop/clients/patterns/distributed-locks/)
- Martin Kleppmann, [How to do distributed locking](https://martin.kleppmann.com/2016/02/08/how-to-do-distributed-locking.html)
- Jim Gray and Leslie Lamport, [Consensus on Transaction Commit](https://www.microsoft.com/en-us/research/wp-content/uploads/2016/02/tr-2003-96.pdf)
- David Karger et al., [Consistent Hashing and Random Trees](https://www.cs.princeton.edu/courses/archive/fall09/cos518/papers/chash.pdf)
- Abhinandan Das et al., [SWIM: Scalable Weakly-consistent Infection-style Process Group Membership Protocol](https://www.cs.cornell.edu/projects/Quicksilver/public_pdfs/SWIM.pdf)
- Michael J. Fischer, Nancy A. Lynch, and Michael S. Paterson, [Impossibility of Distributed Consensus with One Faulty Process](https://groups.csail.mit.edu/tds/papers/Lynch/jacm85.pdf)
- Google Research, [Spanner: Google's Globally-Distributed Database](https://research.google/pubs/spanner-googles-globally-distributed-database/)
- Google Cloud Spanner Documentation, [TrueTime and external consistency](https://docs.cloud.google.com/spanner/docs/true-time-external-consistency)
- Google Cloud Spanner Documentation, [Reads outside of transactions](https://docs.cloud.google.com/spanner/docs/reads)
- Doug Terry, [Replicated Data Consistency Explained Through Baseball](https://www.microsoft.com/en-us/research/wp-content/uploads/2011/10/ConsistencyAndBaseballReport.pdf)
- Hector Garcia-Molina and Kenneth Salem, [Sagas](https://www.cs.cornell.edu/andru/cs711/2002fa/reading/sagas.pdf)
- Stripe Documentation, [Idempotent requests](https://docs.stripe.com/api/idempotent_requests)
- Amazon SQS Documentation, [Exactly-once processing in Amazon SQS](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/FIFO-queues-exactly-once-processing.html)
- Nuno Preguiça, Carlos Baquero, and Marc Shapiro, [Conflict-free Replicated Data Types (CRDTs)](https://arxiv.org/abs/1805.06358)
- Leslie Lamport, Robert Shostak, and Marshall Pease, [The Byzantine Generals Problem](https://lamport.azurewebsites.net/pubs/byz.pdf)
- Google Cloud Spanner Documentation, [Replication](https://docs.cloud.google.com/spanner/docs/replication)
- Amazon Aurora Documentation, [Amazon Aurora storage](https://docs.aws.amazon.com/AmazonRDS/latest/AuroraUserGuide/Aurora.Overview.StorageReliability.html)
- Daniel J. Abadi, [Consistency Tradeoffs in Modern Distributed Database System Design](https://www.cs.umd.edu/~abadi/papers/abadi-pacelc.pdf)
- Jepsen, [Linearizability](https://jepsen.io/consistency/models/linearizable)
- Jepsen, [Serializability](https://jepsen.io/consistency/models/serializable)
- Jepsen, [Strong Serializability](https://jepsen.io/consistency/models/strong-serializable)
- Jepsen, [Analyses](https://jepsen.io/analyses)
- Jepsen, [Knossos](https://github.com/jepsen-io/knossos)
