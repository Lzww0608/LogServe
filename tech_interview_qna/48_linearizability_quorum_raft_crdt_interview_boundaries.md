# 48. linearizability、quorum、Raft 与 CRDT 追问链

这一批放四个分布式系统面试里特别容易被说空的词：linearizability、quorum、Raft 和 CRDT。它们都和“多个副本如何看起来正确”有关，但边界完全不同：linearizability 是一种强正确性条件，quorum 是读写副本集合的交叠手段，Raft 是复制状态机的共识协议，CRDT 是弱协调或无协调更新下的收敛数据类型。

面试时最危险的答法，是把这些词都说成“一致性”。一致性不是一个单词能盖住的东西。你要能说明谁对谁一致、在哪个时间点一致、是否尊重真实时间顺序、是否允许旧读、是否需要多数派、是否需要 leader、是否能在网络分区时继续接收写入，以及业务不变量是不是仍然成立。

结合 LogServe 的口径也要稳。LogServe 可以用 append-only shared log、replay、lease、redelivery、idempotency key 和 checkpoint 解释机制验证；但它不是一个生产级多副本共识数据库，也不是自动满足 linearizability、Raft safety 或 CRDT convergence 的分布式存储。面试中主动把这个边界说清楚，反而更可信。

## Q001. 面试官如果只问一个问题检验你是否理解 linearizability，可能会问什么？

**回答：**

我会预期他问一个读写历史判断题，而不是问定义：

```text
客户端 A 调用 put(x=1)，收到成功返回。之后客户端 B 调用 get(x)，却读到旧值 0。系统说写入已经复制到多个副本，只是 B 读到了一个还没追上的 follower。这个历史是不是 linearizable？如果 A 的 put 和 B 的 get 是重叠的，答案会不会变？
```

这个问题能一下子看出你是否抓住 linearizability 的核心：它关心操作的调用时间、返回时间和对象的顺序语义。一个操作一旦完成，后面才开始的操作必须看见一个能放在它之后的状态。A 的写已经成功返回，B 的读在返回之后才开始，却读到旧值，这通常就破坏了 real-time order。系统内部有多个副本、follower 还没追上、缓存还没刷新，都不是免责理由。

如果两个操作在时间上重叠，情况就不同。linearizability 允许并发操作被排成某个合法的顺序，只要每个操作看起来像在调用和返回之间的某个瞬间生效。A 的 put 还没返回，B 的 get 已经开始并读到旧值，这可能可以解释为 get 的线性化点在 put 生效之前；也可能不行，取决于后续读写结果能不能组成一个合法顺序。

我会继续把它和 serializability 区分开。serializability 主要说一组事务的结果等价于某个串行执行，不一定尊重真实时间先后；linearizability 要尊重非重叠操作的真实时间先后。比如事务 T1 已经提交返回，T2 后来才开始，如果 T2 看不见 T1，很多只讲 serializable 的历史可能仍能找一个串行顺序解释，但 linearizable 的对象历史解释不了。

工程上真正的追问会落到读路径：读是从 leader 读、quorum 读、lease read、read index，还是随便读 follower？写成功返回点是在本地写入、复制到多数派、apply 到状态机，还是只是进队列？如果写返回早于 commit/apply，或者读绕开了确认 leader 新鲜性的屏障，就很容易出现“看似成功，但后续读看不到”的历史。

结合 LogServe，我会说：如果 shared log append 返回成功，后续 metadata replay 或 workflow query 是否必须看到这条 entry，要看系统有没有明确承诺线性化读。如果项目只是单机机制验证，读本地内存视图可能是合理实现；但不能把它包装成多副本 linearizable storage。面试里我会主动说，LogServe 的 source of truth 是日志，replay 后状态可恢复；这和“任意客户端跨节点读都线性一致”是两层承诺。

## Q002. linearizability 的一句话定义是否容易误导，误导点在哪里？

**回答：**

容易误导。常见一句话是：linearizability 就是系统表现得像只有一个副本，每个操作瞬间生效。这句话有帮助，但也很危险，因为它把几个关键条件藏掉了。

第一个误导是忘记“瞬间”必须落在调用和返回之间。不是系统最终能解释成某个顺序就够了，而是每个操作都要能找到自己的线性化点。已经返回成功的写，不能事后被放到后面；已经开始得更晚的读，也不能随便被放到更早的位置来掩盖旧读。

第二个误导是忘记 real-time order。linearizability 要保留非重叠操作的真实时间先后。A 的操作结束后 B 才开始，那么顺序历史里 A 必须在 B 之前。很多人只记住“等价于串行”，却漏掉真实时间约束，于是会把它和 serializability、sequential consistency 混在一起。

第三个误导是把它扩大成整个数据库事务语义。原始概念常用于并发对象。一个 key-value register、queue、counter 可以讨论 linearizable；跨多个对象、多行事务、约束检查、二级索引一致性，还涉及事务隔离、原子提交和可见性规则。单个对象 linearizable，不自动推出整个业务事务正确。

第四个误导是以为 linearizable 等于永远不读旧值。更精确地说，它禁止违反真实时间顺序的旧读。并发读写之间有重叠时，读到旧值可能合法；写还没返回时读不到它，也可能合法。面试里要能画出调用和返回区间，不要只说“读最新”。

第五个误导是把正确性和性能混起来。linearizability 是 safety 条件，不保证低延迟、高可用，也不保证网络分区时还能同时读写。为了实现它，读写路径往往需要 leader、quorum、read barrier、lease 或共识协议。这个成本不是定义的一部分，但工程上绕不开。

第六个误导是忽略失败和未知结果。客户端超时后，不知道操作有没有生效。linearizability 允许 pending operation 可能已经生效，也可能没有生效。业务要用幂等 key、operation id 或 fencing token 处理“不知道是否成功”的区间，不能靠重试时拍脑袋判断。

所以更稳的说法是：linearizability 要求每个完成操作都能被放在调用和返回之间的某个点，使整个历史等价于对象的某个合法顺序历史，并且所有非重叠操作的真实时间先后被保留。这个定义比“一份最新数据”啰嗦，但少很多坑。

## Q003. linearizability 最常见的生产事故触发条件是什么？

**回答：**

最常见的触发条件是读路径为了省延迟绕过了写路径的提交边界。写看起来已经成功，读却从一个没有追上的副本、缓存、物化视图或旧 leader 读取，于是出现违反 read-after-write 的历史。

第一类是 stale follower read。写入经 leader 或 quorum 返回，但读请求为了低延迟打到 follower。follower 的日志复制到了但还没 apply，或者复制都没追上，就会读到旧值。很多系统允许这种读，但必须把语义说成 eventual、bounded stale 或 follower read，不能叫 linearizable read。

第二类是 leader lease 或时钟假设出错。系统用租约来避免每次读都走 quorum，但租约依赖时间边界。如果机器暂停、GC、时钟漂移、NTP 跳变、网络延迟超过假设，旧 leader 可能以为自己仍然有权服务读，而集群已经选出新 leader。这个事故很隐蔽，因为平时延迟很好，故障时才暴露。

第三类是写成功返回点定义太早。写入只是进入内存队列、本地 WAL、异步复制队列，或者只到达少数副本，就提前对客户端返回成功。之后 leader crash 或 failover，新的 leader 不包含这条写，后续读自然看不到。此时问题不在读，而在 ack 语义虚高。

第四类是 commit 和 apply 混淆。Raft 一类系统里，entry committed 说明多数派已经接受该日志位置，但业务状态机还要 apply。客户端如果在 apply 前返回，或者读直接读状态机但状态机落后 commit index，就会产生“日志上已提交、查询里没有”的错觉。严格实现通常要把客户端返回点放在命令被应用之后，或者让读路径等待 apply 到足够 index。

第五类是缓存和派生视图。主存储线性一致，不代表缓存、搜索索引、metadata projection、materialized view 也线性一致。用户写完立刻查列表，列表来自异步投影，就可能看不到。这个不一定是 bug，但必须在 API 语义里说清楚：查询读 source of truth，还是读 eventually consistent view。

第六类是客户端超时和重试。客户端没有收到成功，不知道写是否生效，重试又没有幂等 key，可能产生重复写、乱序写或覆盖。面试里要把 unknown outcome 单独拿出来讲。很多“线性一致性事故”表面是服务端旧读，本质是客户端没有稳定操作身份。

LogServe 里可以这样对应：shared log append、workflow state projection、metadata cache、worker lease 都是不同边界。日志 append 成功不自动等于所有读取视图立即更新；如果对外承诺 read-your-writes，就要让读等到 replay/apply 到对应 log offset。否则应该明确说这是异步视图，而不是 linearizable query。

## Q004. linearizability 的指标应该怎么设计才不会只看平均值？

**回答：**

linearizability 的指标不能只看平均读写延迟。平均延迟只能告诉你系统快不快，不能告诉你历史是否可线性化。指标要同时覆盖正确性探针、提交边界、读新鲜度和尾部延迟。

第一组是 read-after-write 探针。持续执行带唯一版本号的写，然后从不同客户端、不同节点、不同 region 读取，记录是否读到至少该版本。指标不是一个简单的 success rate 就完事，要按 key、partition、replica、read path、consistency level、leader term 拆开。发现一次违反真实时间顺序的旧读，就应该当正确性事件处理。

第二组是提交与可见性的距离。比如 leader commit index、last applied index、follower match index、follower apply index、metadata projection offset、cache invalidation offset。只看“写成功数”没有意义，要看写成功返回时系统承诺到哪个边界，读路径实际读到了哪个边界。

第三组是 stale read 相关分布。可以记录读请求命中的副本落后多少 log entries、多少 milliseconds、多少 bytes，p50/p95/p99/max 都要看。平均落后 10ms 但 p99 落后 30s，用户仍然会遇到严重旧读。

第四组是 leader 和 lease 风险。leader change count、term bump、lease remaining、clock offset、GC pause、heartbeat RTT、read barrier latency、read index latency 都要能对齐。很多 linearizable read 的尾部延迟不是业务逻辑慢，而是在确认 leader 仍然有效。

第五组是未知结果和重试。客户端超时、server-side cancellation、duplicate operation id、idempotency replay、fencing reject、ambiguous commit 都应该有指标。线性一致系统也会遇到客户端不知道操作是否生效的问题，指标要能区分“没写进去”和“写进去了但响应丢了”。

第六组是尾部延迟。linearizable write latency、linearizable read latency、quorum wait、leader apply wait、follower catch-up wait 都要看 histogram 和 p99/p999。不要只看平均值。强一致读写的事故通常先表现为 read barrier p99 抬高、quorum ack p99 抬高、apply lag p99 抬高。

第七组是检查器输出。像 Jepsen/Elle 风格的历史检查不能直接放成普通 latency metric，但可以把 violation count、checker run status、history window、operation count、unknown count、nemesis phase 作为测试结果归档。正确性验证要保存样本历史，不要只留一个绿色勾。

结合 LogServe，可以设计 `shared_log_append_duration_seconds`、`metadata_replay_lag_entries`、`workflow_query_min_visible_offset`、`lease_fencing_reject_total`、`idempotency_replay_total`、`recovery_replay_duration_seconds`。这些指标不是证明系统 linearizable，而是让“承诺边界”和“实际可见性”可以被观察。

## Q005. linearizability 的正确性边界和性能边界分别是什么？

**回答：**

linearizability 的正确性边界，是它只保证操作历史能按真实时间顺序解释成某个合法的顺序对象历史。它是 safety property，不是完整业务正确性。它可以告诉你一个 register 的读写没有违反实时顺序，但不能自动告诉你库存没有卖超、跨表约束没有破、事务没有 write skew、权限检查没有 TOCTOU。

它也常常是局部的。单个对象 linearizable，不代表跨对象组合就有事务原子性。两个 key 分别线性一致，转账仍可能在中间状态被观察到，除非系统额外提供多对象事务、原子提交或严格的事务隔离。面试里一定要把“对象级正确性”和“业务级不变量”分开。

失败边界也要说清楚。客户端超时、网络断开、leader crash 时，操作可能处于 unknown outcome。linearizability 不会替你判断客户端应不应该重试。业务层需要 request id、compare-and-swap、fencing token、幂等表或事务日志来消除重复副作用。

可用性边界是另一个重点。在网络分区下，如果仍要求线性一致，系统通常必须牺牲一部分可用性。少数派不能随便接受会被多数派历史覆盖的写；旧 leader 也不能继续服务强一致读。这个边界不是实现细节，而是强一致语义的代价。

性能边界主要来自协调。写通常要等 leader、本地持久化、复制到多数派、commit，再 apply；读要么走 leader，要么走 quorum，要么用 lease/read index 确认 leader 没过期。每多一个确认边界，就多一次网络、排队、fsync 或状态机 apply 的尾部风险。

跨 region 时成本更明显。多数派放在多个数据中心，linearizable write 的 p99 很容易被 WAN RTT 和最慢 quorum 成员支配；读如果要跨 region 确认真正最新，也会把本地低延迟优势吃掉。工程上常见做法是按数据归属 region 设 leader，或把强一致操作限制在少数关键路径。

LogServe 里如果只在单机多进程场景验证 shared log 和 replay，性能边界主要是本地 append/fsync、锁、worker 调度和 replay。若未来扩展到多副本 linearizable log，就必须额外面对 quorum replication、leader failover、read barrier 和 membership change。不能把单机实验里的延迟直接外推到多副本强一致系统。
## Q006. 面试官如果只问一个问题检验你是否理解 quorum，可能会问什么？

**回答：**

我会预期他问一个带复制因子和一致性级别的场景题：

```text
一个 key 有 3 个副本 A、B、C。写请求在 A、B 成功后返回，也就是 W=2。后续读请求从 B、C 读，也就是 R=2。这个读一定能读到刚才的写吗？如果写用的是 sloppy quorum、跨机房 LOCAL_QUORUM、last-write-wins 时间戳，或者读 repair 没及时发生，答案会怎么变？
```

这个问题的重点不是算 `R + W > N`，而是你是否知道 quorum 只是集合交叠，不是完整一致性协议。理论上，在固定副本集合 N=3、W=2、R=2 且写入版本有可比较语义时，读集合和写集合至少交叠一个副本。读从 B、C 读，B 见过新写，协调者只要正确比较版本，就能返回新值。

但工程里有很多前提。第一，读写的副本集合必须是同一个复制组。如果系统为了可用性用了 sloppy quorum，把写临时写到不属于这个 key 的替代节点，后续正常 quorum 读未必能碰到那份写。第二，成员关系必须清晰。如果拓扑变更、节点替换、repair 不完整，所谓 N 不是一个简单常数。

第三，交叠副本上必须真的保存了写，并且读协调者要能识别新旧版本。Cassandra 这类系统会用 timestamp 和 last-write-wins 解决冲突，但这又把正确性带到时钟边界：客户端时钟漂移或协调者时钟异常，可能让旧写用更大的 timestamp 覆盖新写。quorum 交叠不自动解决版本裁决错误。

第四，LOCAL_QUORUM 只保证本地数据中心内多数派响应。多数据中心复制时，一个 region 的 LOCAL_QUORUM 写不一定马上被另一个 region 的 LOCAL_QUORUM 读看见，除非系统和操作级别明确要求跨 region 的交叠，比如 EACH_QUORUM 或更强协议。

第五，读的返回策略也重要。读请求可能只向足够副本发请求，也可能有 speculative retry；可能比较 digest 后再发 full data read，也可能因为超时只拿到某些副本响应。读 repair、hinted handoff、anti-entropy repair 都能推动收敛，但它们通常不是强一致读的替代品。

所以我会这样答：quorum 问题先算交叠，再查前提。固定成员、固定复制组、正确版本比较、写成功语义明确、读路径确实比较到交叠副本时，`R + W > N` 能帮助读到已确认写。只要 sloppy quorum、跨 DC、本地一致性级别、时钟 LWW、拓扑变更或异步修复介入，就不能只拿公式背书。

## Q007. quorum 的一句话定义是否容易误导，误导点在哪里？

**回答：**

容易误导。最常见的一句话是：quorum 就是多数派。这句话在很多场景下近似有用，但它会误导面试者把一个交叠技巧说成一个完整一致性模型。

第一个误导是 quorum 不一定等于简单多数。多数派是 quorum 的常见形式，因为任意两个多数集合必然相交。但 quorum 可以是读写集合设计，也可以是加权 quorum、按机房分层 quorum、按 rack/failure domain 约束的 quorum。Cassandra 的 `QUORUM` 是多数副本响应，`LOCAL_QUORUM` 是本地数据中心多数副本响应，语义明显不同。

第二个误导是只会背 `R + W > N`。这个公式讨论的是读集合和写集合是否交叠，不是说系统一定线性一致。你还需要知道写什么时候算成功、读是否真的收集并比较多个副本、版本冲突怎么裁决、失败节点上的旧值怎么修复、成员变更期间 N 怎么定义。

第三个误导是把可见性和收敛混在一起。quorum 读写可以提高读到新值的概率或在特定前提下保证交叠，但后台 repair、hinted handoff、anti-entropy 是收敛机制。它们让副本最终更一致，不等于当前这次读已经满足强一致。

第四个误导是忽略多数据中心。`LOCAL_QUORUM` 的优势是低延迟和区域容错，但它牺牲了跨区域立即可见性。一个 region 内的多数派不等于全球多数派。面试里如果只说“我们用 quorum 所以一致”，下一问通常就是“哪个 quorum？本地还是全局？”

第五个误导是忽略冲突语义。两个写都拿到了各自 quorum，或者网络抖动下写入顺序不一致，系统要用 timestamp、vector clock、version vector、compare-and-set 或 leader 顺序来裁决。quorum 只是让集合相交，不会自动告诉你冲突怎么解释。

第六个误导是把 quorum 当成高可用保证。更大的 quorum 提高一致性或新鲜度，但会降低可用性和提高尾部延迟。N=3、W=2 时，能容忍一个副本失败；如果两个副本慢或不可达，写就失败或超时。quorum 是一致性、可用性和延迟之间的旋钮，不是免费增强。

更准确的一句话是：quorum 是通过要求读写操作获得足够副本响应，使关键操作集合发生交叠的一种复制协议手段；它本身不是完整一致性模型，必须和成员关系、版本裁决、故障处理、修复机制和读写返回语义一起理解。

## Q008. quorum 最常见的生产事故触发条件是什么？

**回答：**

quorum 最常见的事故，不是公式算错，而是线上实际读写路径和你以为的 quorum 前提不一致。配置、拓扑、时钟、修复、重试、慢副本，只要有一项被忽略，就会把“理论交叠”变成“生产旧读”。

第一类是 consistency level 配错。写用 `ONE`，读也用 `ONE`，性能很好，但读到旧值并不奇怪。更隐蔽的是某些代码路径默认 `LOCAL_ONE`，某些路径用了 `QUORUM`，事故只发生在部分接口或部分租户。面试里要强调一致性级别是每次操作的语义，不是集群贴了一个标签就永久成立。

第二类是跨数据中心误判。写在 A 机房 `LOCAL_QUORUM` 成功，读在 B 机房 `LOCAL_QUORUM` 立刻查。两个本地多数派没有交叠，B 读不到 A 的写很正常。这个事故常出现在多活、灾备切流、流量调度或客户端 region 选择变化时。

第三类是 last-write-wins 依赖时钟。某个客户端或协调者时钟快了，写入了未来 timestamp；后续正常写虽然在真实时间更晚，却因为 timestamp 更小被覆盖。quorum 读写都成功，也挡不住错误的版本裁决。这个问题尤其容易和 NTP、虚拟机暂停、跨语言客户端自带 timestamp 混在一起。

第四类是 hinted handoff 和 repair 被误解。hinted handoff 能在副本短暂不可用后补写，read repair 能在读路径修补不一致，anti-entropy repair 能后台对齐数据。但这些机制有延迟、有失败、有范围，不应该被当成“这次读一定最新”的保证。修复 backlog 积压时，旧值会持续更久。

第五类是拓扑变更。扩容、缩容、replace node、decommission、bootstrap、rebalance 期间，复制集合在变化。应用以为 RF=3、QUORUM=2，但某些 token range 正在迁移，某些副本还没完整数据，某些 repair 还没做完。这个阶段最需要观测 per range/per replica 的状态，而不是只看全局 healthy。

第六类是慢副本导致尾部超时。quorum 只等足够副本，听起来能躲慢节点；但如果多数副本里有一个慢，或者 coordinator 选择的副本组合刚好拥塞，p99 会急剧变差。为了降低延迟改低一致性级别，又可能引入旧读。很多事故就是在延迟压力下把一致性旋钮拧松。

第七类是读写语义不对齐。写路径通过 quorum 写 source of truth，读路径却读缓存、搜索索引、异步物化视图。底层存储满足 quorum，不代表上层 API 满足 read-after-write。这个在业务系统里比底层协议 bug 更常见。

如果用 LogServe 类比，我会说：如果未来 shared log 做多副本复制，不能只说“写到多数派”。还要定义哪些副本算复制组、成员变更怎么处理、append ack 是否等 fsync、query 是否读 committed+applied offset、metadata cache 是否允许旧视图。quorum 是一层边界，不是全部边界。

## Q009. quorum 的指标应该怎么设计才不会只看平均值？

**回答：**

quorum 的指标要围绕“这次操作等了哪些副本、达到了哪个一致性级别、有没有读到旧版本、尾部慢在哪里”来设计。只看平均读写延迟会漏掉几乎所有关键问题。

第一组是每种 consistency level 的请求量和结果。`ONE`、`QUORUM`、`LOCAL_QUORUM`、`ALL` 要分别统计 success、timeout、unavailable、read failure、write failure。不要把所有读写混在一起。事故时你要能看到是不是某条路径退化到了低一致性级别。

第二组是 quorum ack 分布。写请求要记录等待第几个副本成功、第几个副本超时、最快副本、达到 quorum 的耗时、全部副本完成的耗时。读请求要记录参与副本数、digest mismatch、data read retry、speculative retry。p95/p99 比平均值重要，因为 quorum 操作的尾部通常被慢副本支配。

第三组是副本落后与分歧。per replica 的 last mutation timestamp、log position、repair backlog、hint backlog、read repair count、Merkle mismatch、range repair age 都要有。平均落后不够，某个 token range 落后一天就可能影响核心 key。

第四组是版本冲突和裁决。last-write-wins 系统要看 clock skew、future timestamp、tombstone count、conflict overwrite、client timestamp usage。使用 vector clock 或多版本返回的系统要看 sibling count、conflict resolution rate。否则你只知道 quorum 成功，不知道成功返回的是不是业务想要的版本。

第五组是跨机房维度。LOCAL_QUORUM 的延迟、远端复制 lag、inter-DC queue、remote apply lag、region failover count 要拆开。全球平均延迟没有意义，A region 正常、B region 旧读严重的事故会被平均值盖住。

第六组是拓扑状态。每个 keyspace/table/range 的 RF、live replicas、pending ranges、bootstrap/decommission 状态、unreachable replicas、gossip convergence、membership version 都要能查。quorum 的前提是你知道 N 和副本集合是谁。

第七组是正确性探针。可以持续写入带版本的 key，然后按不同 consistency level、region、client route 读取，记录 stale read count、staleness age、staleness version gap。这个指标要按最坏值和分位数看，不要只看 stale rate 平均值。强语义里一次违反就值得调查。

对 LogServe 来说，如果只是单机日志，可以先观测 append latency、fsync latency、replay lag；如果扩成多副本，就要加 `replication_ack_count`、`quorum_commit_duration`、`follower_match_lag`、`read_visible_commit_index`、`membership_epoch`、`stale_projection_total`。指标要跟承诺边界绑定。

## Q010. quorum 的正确性边界和性能边界分别是什么？

**回答：**

quorum 的正确性边界是：它能提供集合交叠，但不自动提供线性一致、事务隔离、冲突语义或业务不变量。`R + W > N` 只能说明读集合和写集合至少有一个副本重叠，前提还是固定复制组和清晰成员关系。那个交叠副本上的版本是否被正确识别、是否已持久化、是否会被旧 timestamp 覆盖，是另外的问题。

如果系统是 Dynamo/Cassandra 风格的多主复制，quorum 通常配合版本冲突处理和后台修复，目标更多是可调一致性和最终收敛。它可以提高读到最新写的概率或在特定配置下保证读写交叠，但它不等于单 leader 严格顺序。两个并发写之间谁赢，要看版本裁决规则。

如果系统是 Raft/Paxos 风格的共识复制，多数派 quorum 是协议安全性的基础，但真正的正确性来自 term、log matching、leader completeness、commit rule 和 state machine apply。不能把“多数派响应”从协议里单独摘出来，当成自己写一个 quorum 就有了 Raft。

事务边界也要分清。单 key quorum 读写不等于多 key 原子事务。库存扣减、唯一索引、跨账户转账、权限变更这种业务不变量，如果需要全局串行化或条件写，通常还要 LWT、CAS、事务协议、锁或共识。quorum 不能替业务逻辑证明不变量。

性能边界主要来自第 k 快副本。N=3、W=2 时，写延迟不是最快副本，也不是最慢副本，而通常是第二快副本加上 coordinator 处理和网络。N=5、W=3 就看第三快副本。副本越多、quorum 越大，尾部延迟越容易被慢节点、跨 rack 网络、磁盘抖动拖住。

可用性边界也直接受 quorum 大小影响。更强一致性级别需要更多副本响应，因此更容易在节点故障、网络分区、GC 暂停、磁盘拥塞时超时。`ALL` 新鲜度最强但可用性最低；`ONE` 可用性和延迟好，但旧读风险更高。没有免费组合。

跨机房性能尤其明显。LOCAL_QUORUM 能把多数派限制在本地，延迟较好；EACH_QUORUM 或全球多数派会把 WAN RTT 放进写路径。业务要先决定它要本地低延迟、跨区灾备、还是全球强可见性，然后再选 quorum 级别。

LogServe 的面试回答可以落到一句：如果系统只有本地 shared log，谈 quorum 是未来多副本方案；如果要引入 quorum，必须同时设计 membership、commit、read visibility、repair、fencing 和 idempotency。quorum 只是让副本集合相交，不能替代日志协议和业务不变量设计。
## Q011. 面试官如果只问一个问题检验你是否理解 Raft，可能会问什么？

**回答：**

我会预期他问一个 leader crash 的日志题：

```text
Raft leader 收到客户端命令，把 entry 追加到自己的 log，也复制给了一个 follower，但还没有复制到多数派。leader 这时宕机。之后集群选出新 leader。这个 entry 能不能算 committed？客户端如果已经收到成功返回，会发生什么？如果 entry 已经复制到多数派但还没 apply，又该什么时候对客户端返回？
```

这个问题能检查你是否理解 Raft 的核心边界：日志存在不等于提交，提交不等于已经应用，leader 本地有不等于集群同意。Raft 的目标是让多个节点以同样顺序应用同一串状态机命令。只有满足 commit rule 的 entry，才进入未来 leader 必须保留的历史。

如果 entry 只在 leader 和一个 follower 上，N=5 时没有多数派，leader crash 后它可能被新 leader 的日志覆盖。这个 entry 不能算 committed。客户端如果已经收到成功，那就是实现错误，因为系统向客户端承诺了一个未来可能消失的命令。

如果 N=3，leader 把 entry 复制到自己和一个 follower，达到多数派，还要看 Raft 的提交规则。通常 leader 对当前 term 的日志项，一旦复制到多数派，就可以推进 commit index。然后 leader 把 commit index 通知 follower，各节点按顺序 apply 到状态机。客户端命令一般应等 entry committed 且应用到 leader 状态机后再返回，这样返回值来自确定的状态机执行结果。

为什么 apply 也重要？因为 committed 只是日志层面，不代表业务状态已经变了。一个 `Put x=1` entry committed 但还没 apply，如果读请求直接读状态机，可能仍看到旧值。要么读路径等待状态机 apply 到对应 index，要么把读本身也作为日志 entry，或者使用 read index 等机制保证读到的状态覆盖最新已提交历史。

然后我会补充 leader 读的问题。Raft 不是“只要我是 leader 就能随便读”。旧 leader 可能不知道自己已经被新 leader 取代。线性一致读需要额外步骤确认当前 leader 仍然拥有权威，例如和多数派交换心跳确认，或者使用满足安全假设的 lease read。否则就可能出现旧 leader 返回旧状态。

结合 LogServe，我会说：如果项目里的 shared log 是单机 append-only log，它可以解释 replay 和 crash recovery，但不能直接说它是 Raft log。Raft log entry 带 term/index，commit 来自多数派和 leader completeness，apply 要进入 deterministic state machine。LogServe 目前更适合说“用日志验证可恢复执行机制”，而不是说“已经实现了 Raft 共识”。

## Q012. Raft 的一句话定义是否容易误导，误导点在哪里？

**回答：**

容易误导。常见一句话是：Raft 是一个更容易理解的共识算法，用 leader 和多数派复制日志。这句话没错，但很容易让人以为 Raft 就是“leader 写日志，半数节点 ack”。真正的 Raft 比这句话严格得多。

第一个误导是把 leader 当成永久权威。Raft 的 leader 权威只在当前 term 和多数派联系成立时有效。term 是领导权时代，RPC 里携带 term，节点看到更大的 term 要退回 follower。旧 leader 如果网络隔离，不能继续对外提供强一致写和线性读。

第二个误导是忽略日志匹配。Raft 的 AppendEntries 不只是传数据，还带 `prevLogIndex` 和 `prevLogTerm`。follower 只有在前一条日志匹配时才接受后续 entry；如果同一 index 的 term 冲突，要删除冲突 entry 及其后缀。这个机制保证不同节点最终拥有共同前缀。

第三个误导是把“复制到多数派”说得太粗。leader 只能安全地用当前 term 的 entry 推进 commit；旧 term entry 的提交要通过后续当前 term entry 间接确认。这个细节是为了保证新 leader 一定包含已提交历史。面试里如果完全不知道 leader completeness，通常说明只记了表层。

第四个误导是把 committed 和 applied 混在一起。commit index 是日志层面的边界，last applied 是状态机执行边界。客户端返回、查询可见性、snapshot 生成都要清楚用哪个边界。很多实现 bug 就出在“日志已提交，但业务状态没应用”。

第五个误导是以为 Raft 自动解决所有分布式问题。Raft 解决的是非拜占庭故障模型下的复制状态机共识，不解决分片、跨 group 事务、二级索引、全局唯一约束、客户端 exactly-once、流控、热点 key、慢 snapshot、磁盘损坏检测、权限和业务幂等。

第六个误导是低估持久化要求。Raft 的 `currentTerm`、`votedFor`、log entries 要在响应相关 RPC 前落到稳定存储，否则节点重启后可能忘记自己投过票或接受过日志，破坏安全性。只在内存里跑通测试，不代表协议正确。

更准确的一句话是：Raft 是一个 leader-based 的复制状态机共识协议，通过 term、投票、日志匹配、多数派提交、leader completeness 和 state machine safety，让非拜占庭节点在故障和重选举下仍然按同一顺序应用同一串命令。

## Q013. Raft 最常见的生产事故触发条件是什么？

**回答：**

Raft 生产事故最常见的触发条件，是实现或运维把协议里的安全边界当成性能优化点绕开。Raft 论文里的规则看起来简洁，但真实系统里一旦遇到磁盘、暂停、网络、成员变更和大 snapshot，事故会很具体。

第一类是持久化顺序错误。节点在 `currentTerm`、`votedFor` 或 log entry 没有可靠落盘前就回复成功，重启后忘记投票或丢日志。表面上看是少数节点 crash，实质是 stable storage 语义不成立。Raft 的安全性假设不是“进程不挂”，而是挂了也记得该记住的承诺。

第二类是旧 leader 服务读写。网络分区或长时间 GC 后，旧 leader 以为自己还活着，继续接受请求。写通常会因为拿不到多数派而卡住或失败，但读如果没有 read index、lease 校验或多数派心跳确认，就可能返回 stale state。很多人以为读不改状态就安全，这是典型误区。

第三类是 election storm。磁盘抖动、GC pause、CPU throttling、网络丢包导致 follower 收不到 heartbeat，不断发起选举。term 一直涨，leader 频繁切换，写入吞吐下降，客户端看到大量超时。平均网络延迟可能还行，但 election timeout 与 p99 pause/RTT 配错就会出事。

第四类是 apply lag。日志复制和 commit 正常，但状态机执行慢，last applied 远落后于 commit index。写请求如果等 apply，就 p99 暴涨；读请求如果不等 apply，就可能读旧状态。大事务、慢磁盘、下游副作用、snapshot restore 都会造成 apply lag。

第五类是 snapshot 和 log compaction。follower 落后太多，leader 不再保留旧日志，只能发 snapshot。snapshot 很大时会占网络、磁盘和 CPU；安装 snapshot 时如果没有正确处理 last included index/term、并发 AppendEntries、状态机替换和崩溃恢复，就会出现数据回退或重复 apply。

第六类是 membership change。扩容缩容不是简单改配置。旧配置和新配置之间要保证 quorum 交叠，否则可能出现两个不同多数派各自提交历史。Raft 的 joint consensus 或等价安全机制就是为了解决这个问题。很多自研系统事故发生在“临时下线一个节点”这种看似运维动作里。

第七类是客户端重试没有去重。leader 收到命令后提交了，但响应丢了；客户端重试到新 leader。如果状态机命令不是幂等的，没有 client id + sequence 去重，就可能扣款两次、创建两次、发两封通知。Raft 保证日志顺序，不保证业务副作用天然 exactly-once。

LogServe 里对应的提醒是：worker lease、任务重投递、日志 replay 都会遇到重复执行边界，所以项目里强调 idempotency key 和 log-first recovery 是合理的。但这不是 Raft 本身。若未来引入 Raft，必须把持久化、leader 读、membership、snapshot、client dedup 单独设计出来。

## Q014. Raft 的指标应该怎么设计才不会只看平均值？

**回答：**

Raft 的指标要能回答四个问题：谁是 leader，term 是否稳定，日志复制到哪里，状态机应用到哪里。平均请求延迟只能说明用户感觉慢不慢，不能说明共识层正在发生什么。

第一组是 leader 和 term。leader changes、current term、election timeout、election duration、vote granted/rejected、pre-vote rejected、leadership transfer、leader uptime 都要有。一个健康集群不应该频繁换 leader。term 突然上涨，通常比平均 latency 更早暴露网络或暂停问题。

第二组是 proposal 生命周期。client propose 到 leader append、本地 fsync、发送 AppendEntries、达到多数派、commit index 推进、apply 到状态机、响应客户端，每段都要有 histogram。Raft 写入 p99 要拆开看，否则不知道是 fsync 慢、网络慢、follower 慢，还是状态机慢。

第三组是复制进度。每个 follower 的 match index、next index、replication lag entries、replication lag bytes、AppendEntries reject count、heartbeat RTT、inflight append、snapshot send progress 都要可见。不能只看集群平均 lag，某个 follower 落后很久会在故障切换时变成问题。

第四组是 commit/apply gap。`commitIndex - lastApplied`、apply duration、state machine queue length、snapshot apply duration、read index wait、linearizable read latency 都要看 p95/p99/max。很多用户感知问题发生在 apply 层，而不是复制层。

第五组是稳定存储。WAL append latency、fsync latency、log write bytes、disk queue、snapshot write/read、compaction duration、write stall 都要和 Raft 指标对齐。Raft 安全依赖 stable storage，磁盘尾部抖动会直接影响选举和提交。

第六组是请求结果分类。propose success、not leader、leader changed、timeout、dropped proposal、too many pending proposals、read index timeout、apply timeout、duplicate command replay，都要单独统计。把它们都合进 error rate，会掩盖协议层症状。

第七组是成员变更和配置。configuration index、joint consensus phase、voter/learner 列表、quorum size、unstarted member、unhealthy voter、learner catch-up lag 都要暴露。成员变更事故不能靠普通 RPC latency 看出来。

第八组是正确性测试产物。单元测试和集成测试要覆盖 crash/restart、partition、reorder、duplicate RPC、snapshot install、membership change、客户端重试。线上指标不能证明 Raft 正确，但能告诉你是否接近已知危险区间。协议正确性还是要靠模型测试、故障注入和历史检查补充。

如果拿 LogServe 类比，现阶段更应该关注 shared log append/fsync、workflow replay lag、checkpoint restore、lease redelivery、worker queue age。等引入 Raft 这类复制协议后，再加 term、commit index、match index、read index、snapshot install 这些共识层指标，避免把两层混起来。

## Q015. Raft 的正确性边界和性能边界分别是什么？

**回答：**

Raft 的正确性边界，是在非拜占庭故障模型下，让多个节点对同一条日志历史达成一致，并按同样顺序应用到确定性状态机。它保证的是复制状态机安全性，不是所有分布式应用正确性。

它不处理恶意节点。节点如果伪造消息、故意撒谎、磁盘返回错误但不报错、网络设备篡改数据，普通 Raft 不具备拜占庭容错能力。实际系统仍要用 TLS、认证、checksum、磁盘校验、权限控制和运维隔离降低风险，但这不是 Raft 协议安全证明的一部分。

它不自动处理业务 exactly-once。Raft 能保证命令在日志里有顺序，但客户端超时重试可能把同一个业务动作追加两次。状态机要用 client id、request sequence、dedup table 或幂等语义处理重复。否则共识层完全正确，业务仍然可能重复扣费。

它不自动处理跨 shard 事务。一个 Raft group 内的 key 可以顺序执行，多个 Raft group 之间要额外协议。全局唯一约束、跨账户转账、二级索引一致性，通常需要事务协调、两阶段提交、per-key routing、全局时间戳或更高层设计。

它不自动让读线性一致。leader 读必须确认自己仍是 leader，并且状态机已经 apply 到足够新。常见做法是 ReadIndex、lease read 或把读也写进日志。不同做法性能和安全假设不同，尤其 lease read 要小心时钟和暂停。

性能边界首先是单 leader。所有写入经过 leader 排序，leader 的 CPU、网络、磁盘、WAL、状态机 apply 都可能成为瓶颈。Raft 简化了理解，也把写入路径集中到 leader。扩展吞吐通常要分 shard、多 Raft group 或批量写，而不是让一个 group 无限扩。

第二个性能边界是多数派复制和持久化。写入延迟通常包括 leader 本地持久化、发给 follower、等多数派响应、commit、apply。慢 follower 不一定阻塞多数派，但多数派内任一关键副本慢都会影响 p99。跨机房部署会把 WAN RTT 放进 commit 路径。

第三个性能边界是 snapshot 和恢复。日志无限增长不可接受，snapshot 又会消耗 IO、网络和 CPU。follower 长时间落后时安装 snapshot，可能影响正常复制。状态机大、snapshot 慢、反序列化慢，都会让恢复和扩容变成尾部延迟问题。

第四个边界是 membership change。为了安全，配置变更不能随便并发、不能让两个配置的 quorum 不相交。节点变更频繁时，集群可用性和吞吐都会受影响。运维上要把 learner catch-up、voter promotion、decommission 节奏控制住。

面试里可以落到一句：Raft 把“多个正常但可能宕机、丢包、重启的节点如何同意同一串命令”说清楚；它不替代事务系统、业务幂等、分片方案和安全体系。性能上，它用 leader 和多数派换取可理解的安全边界，代价就是协调、持久化和 leader 瓶颈。
## Q016. 面试官如果只问一个问题检验你是否理解 CRDT，可能会问什么？

**回答：**

我会预期他问一个离线并发更新的合并题：

```text
两个机房在网络分区期间都允许用户修改同一个集合。A 机房执行 add(x)，B 机房同时执行 remove(x)。网络恢复后，这个集合里到底有没有 x？如果你回答“CRDT 会自动解决冲突”，那具体是哪种 CRDT？add-wins、remove-wins、LWW，还是 OR-Set？业务能不能接受这个语义？
```

这个问题能检查你是否知道 CRDT 不是“随便合并都正确”，而是每个数据类型必须定义并发语义。add 和 remove 并发时，集合里有没有 x，并没有天然正确答案。add-wins set 会保留 x，remove-wins set 会删除 x，last-writer-wins set 会按时间戳或总序裁决，OR-Set 会通过观察到的 add tag 解释 remove。不同语义对应不同业务含义。

CRDT 的核心是允许副本在不协调或弱协调情况下本地接收更新，之后通过满足数学性质的 merge 或可交换操作让副本收敛。状态型 CRDT 通常要求状态形成 join-semilattice，更新单调推进，merge 取 least upper bound；操作型 CRDT 则依赖可靠传播和操作可交换，很多设计还要求因果顺序。

所以我会先问业务语义。购物车里 add-wins 可能合理，因为用户加了商品不希望被另一个副本的并发 remove 随便覆盖；权限系统里 remove-wins 可能更安全，因为撤销权限优先；协作文档要保留并发编辑，但还要处理顺序、光标、重复应用和 tombstone；库存扣减不能简单用普通 PN-Counter，因为“不能卖成负数”是全局不变量。

我也会主动说 CRDT 不保证 linearizability。网络分区期间两个副本都能本地写，读本地副本就可能看到不同状态。CRDT 保证的是在更新最终传播、系统满足交付假设、merge 正确实现后，副本最终收敛到同一状态。它牺牲的是强即时一致，换来高可用和低本地延迟。

面试里好的回答不是背“无冲突复制数据类型”，而是把数据类型说出来：G-Counter 只能增长；PN-Counter 用两个增长计数器表达增减；OR-Set 用唯一 tag 处理 add/remove；LWW Register 用总序裁决但会丢并发值；MV-Register 保留并发值让上层解决。每一种都有业务代价。

结合 LogServe，我会说：如果 workflow 状态和任务执行需要严格一次、顺序 replay 和明确失败恢复，普通 CRDT 不一定适合直接替代 shared log。某些指标、缓存、可合并统计、节点心跳摘要可以用 CRDT 思路；但任务状态机、lease fencing、结果提交这些有顺序和不变量的路径，仍需要日志、幂等和协调边界。

## Q017. CRDT 的一句话定义是否容易误导，误导点在哪里？

**回答：**

容易误导。常见一句话是：CRDT 是一种能自动解决冲突、最终一致的数据结构。这个说法最大的问题是“自动解决冲突”听起来像业务冲突自动消失了，其实 CRDT 只是在预先定义好的并发语义下保证收敛。

第一个误导是把收敛当成正确。两个副本最后值一样，只说明它们收敛了，不说明这个值符合业务期望。会议室预约里两个用户并发预约同一时段，一个 add-wins set 可能保留两个预约；这在 CRDT 语义上可以收敛，在业务上却是冲突。

第二个误导是忽略数据类型限制。不是任意对象都能随手做成 CRDT。设计者要定义状态偏序、merge、更新单调性，或者定义可交换的操作和可靠传播条件。复杂 JSON、文本、图结构可以有 CRDT 设计，但不是把两个 JSON 做深度合并就完事。

第三个误导是忽略并发语义。add-wins、remove-wins、multi-value、last-writer-wins、escrow counter 都是不同选择。你必须告诉面试官：并发 add/remove 怎么办，并发 write/write 怎么办，remove 是否需要 tombstone，重复消息是否幂等，因果关系如何记录。

第四个误导是忽略元数据成本。很多 CRDT 需要 vector clock、dot、version vector、tombstone、operation log、causal context。小规模演示很漂亮，生产热 key 下元数据可能比业务数据还大。删除也不一定真的删除，tombstone 什么时候安全清理是难题。

第五个误导是把 CRDT 和 LWW 混为一谈。last-writer-wins 容易实现，但它依赖时钟或总序，可能悄悄丢掉并发写。它能收敛，不代表语义好。对用户内容、权限、资金、库存，LWW 往往是危险的默认值。

第六个误导是忘记交付假设。CRDT 不是魔法。状态型 CRDT 要让状态或 delta 最终传播到其他副本；操作型 CRDT 要可靠投递，可能还要因果顺序。消息丢失、delta compaction 错误、节点永久离线、错误清理 tombstone，都会破坏收敛或语义。

更准确的一句话是：CRDT 是一类为复制场景设计的抽象数据类型，它允许副本本地接收更新，并在满足传播和合并条件后按预先定义的并发语义确定性收敛；它解决的是收敛和部分并发语义，不自动解决所有业务冲突。

## Q018. CRDT 最常见的生产事故触发条件是什么？

**回答：**

CRDT 最常见的事故，是业务把“可以无协调写入并最终收敛”理解成“所有约束都能无协调保证”。结果系统确实收敛了，但收敛到一个业务不接受的状态。

第一类是全局不变量被破坏。库存不能为负、用户名必须唯一、一个订单只能支付一次、会议室同一时间只能被一个人预订，这些约束通常不能靠普通 CRDT 无协调保证。可以用 escrow、reservation、bounded counter 等技术把部分资源预分配给副本，但那已经是更复杂的协调设计，不是普通计数器自动搞定。

第二类是删除语义和 tombstone 爆炸。集合、文档、图结构为了让 remove 在并发和乱序传播下可解释，常常需要 tombstone 或 causal metadata。长期运行后 tombstone 不清理会拖慢 merge、增大存储；清理太早又可能让迟到的 add/remove 复活旧数据。

第三类是 LWW 丢更新。为了简单，系统用 timestamp 解决冲突。两个用户并发编辑同一字段，最后一个 timestamp 赢，另一个用户的内容消失。更糟的是时钟漂移会让“未来写”长期压制正常写。用户看到的是数据丢失，不会接受“系统最终一致”。

第四类是因果上下文丢失。OR-Set、MV-Register 等设计需要知道哪些更新发生在之前，哪些是并发。如果同步协议丢了 causal context、dot、version vector，remove 可能删错对象，读可能少显示并发值，merge 可能不再幂等。

第五类是消息去重和幂等没做好。状态型 merge 通常要求幂等；操作型 CRDT 对重复投递、乱序、漏投递更敏感。一次 increment 操作如果被重复应用两次，计数就错了。操作型设计要么依赖 exactly-once-like 的去重身份，要么让 effector 幂等。

第六类是热点对象元数据膨胀。一个大协作文档、一个热门购物车集合、一个全局 presence map，可能产生巨大 operation log、tombstone、vector。merge CPU、网络同步、序列化和压缩都会变成 p99 问题。

第七类是 UI 或 API 隐藏并发冲突。MV-Register 返回多个并发值，上层却随便拿第一个；协作文档出现并发删除和插入，UI 没有解释；权限冲突被 add-wins 保留，导致撤权不生效。CRDT 把并发语义下放给数据类型，但产品层仍要面对用户可理解性。

LogServe 里如果用 CRDT 思路做 worker heartbeat、节点能力摘要、统计计数，风险可控；如果拿它直接做 workflow 状态提交、任务结果写入、lease 所有权，那就要非常谨慎。workflow 状态更需要可重放顺序、fencing 和幂等，而不是“最后大家收敛成某个状态”。

## Q019. CRDT 的指标应该怎么设计才不会只看平均值？

**回答：**

CRDT 的指标要同时看收敛速度、元数据增长、冲突语义和业务不变量。只看平均 merge latency 或平均同步延迟，很容易把最危险的 key、对象或副本隐藏掉。

第一组是 convergence lag。每个对象或分区的最大未传播更新年龄、delta backlog、operation log backlog、replica version gap、anti-entropy round duration，都要看 p95/p99/max。平均 1 秒收敛没有用，某个热门对象 30 分钟不收敛才是事故。

第二组是 divergence probe。持续对同一对象从多个副本读取摘要，比较 hash、version vector、dot context、element count、value set。记录 divergent object count、oldest divergence age、max replica gap。不要只看“同步任务成功率”，成功同步不代表所有对象都一致。

第三组是元数据体积。tombstone count、causal context size、version vector length、dot count、operation log bytes、delta bytes、merge input size、serialized object size 都要有分布。CRDT 的性能事故经常来自元数据，而不是业务值本身。

第四组是冲突语义事件。add-wins conflict、remove-wins conflict、multi-value register conflict count、LWW overwrite count、clock skew discard、future timestamp write、manual resolution count，都应该被统计。业务要知道系统到底自动裁决了多少冲突，而不是只看最后是否收敛。

第五组是 merge 和同步尾部。merge duration、delta generation duration、state serialization duration、state transfer bytes、apply operation duration、compaction duration 要看 p99。大对象或热对象的尾部 merge 会直接影响用户写入或后台同步。

第六组是不变量探针。库存、额度、唯一性、权限撤销、生效状态这类业务约束要有专门 checker。CRDT 收敛指标不能替代业务正确性指标。比如 bounded counter 要看 available rights、escrow transfer failures、negative counter reject、coordination fallback latency。

第七组是副本健康和传播拓扑。每个副本的 outbound queue、inbound queue、peer sync failures、last successful sync time、per peer lag、dropped delta、compaction watermark 都要有。状态型 CRDT 依赖最终传播，传播图断了就无法收敛。

LogServe 如果用 CRDT 处理可合并统计，可以记录 per-worker counter divergence、merge lag、heartbeat map size；但 workflow 执行链路仍应关注 log offset、task lease、checkpoint 和 replay。指标设计要先问“这条数据到底允许并发合并吗”。

## Q020. CRDT 的正确性边界和性能边界分别是什么？

**回答：**

CRDT 的正确性边界，是在给定数据类型、给定并发语义和给定传播假设下，副本最终会收敛到同一状态。它不保证线性一致，不保证读到最新值，不保证跨对象事务，不保证业务不变量自动成立。

CRDT 最擅长的是可以自然合并的状态：计数、集合、presence、购物车、协作文档的局部编辑、偏好设置、离线优先应用的一部分数据。它的优势是分区期间仍能本地写，用户低延迟，网络恢复后收敛。代价是读可能看到本地视图，其他副本暂时不同。

它最不擅长的是强约束：唯一用户名、库存不能超卖、付款只能一次、权限撤销必须立刻全局生效、任务只能被一个 worker 拥有。这些场景需要协调、锁、fencing、事务、共识或 escrow 设计。可以把某些约束拆成可合并资源，但那是专门建模，不是普通 CRDT 的默认能力。

单对象边界也要说清楚。一个 CRDT 对象内部可以收敛，不代表多个对象之间有原子性。更新用户资料和更新索引、扣库存和创建订单、改变权限和刷新缓存，如果分成多个 CRDT，没有额外事务协议，就会出现中间状态和跨对象不一致。

性能边界首先是元数据。vector、dot、tombstone、operation id、causal context 都要存、传、合并。对象越热、并发越高、离线时间越长，元数据越容易膨胀。删除并不一定释放空间，安全 compaction 需要知道所有相关副本都不再需要旧因果信息。

第二个性能边界是同步流量。状态型 CRDT 直接传完整状态会很重，delta-state 可以降低流量，但要正确管理 delta 丢失、重传和 compaction。操作型 CRDT 传操作更轻，但对可靠投递、去重和因果顺序要求更高。不同方案的成本不一样。

第三个性能边界是 merge CPU 和尾部对象。平均对象很小，不代表热门对象小。一个协作文档或大集合可能让 merge、序列化、压缩、持久化都变慢。要按对象大小和热度看 p99，而不是只看全局平均 merge 时间。

第四个边界是用户体验。CRDT 允许并发状态存在，产品要解释这些状态。MV-Register 显示多个值，协作文档显示冲突编辑，权限撤销的 remove-wins 语义，都需要用户能理解。技术上收敛，用户觉得数据乱，也是失败。

面试里我会把 CRDT 总结成一句：它用明确的数据类型语义和可合并数学结构换取分区可用与最终收敛；它不是强一致存储的廉价替代品。能合并的状态适合它，需要全局顺序和强不变量的路径，要回到日志、事务、fencing 或共识。