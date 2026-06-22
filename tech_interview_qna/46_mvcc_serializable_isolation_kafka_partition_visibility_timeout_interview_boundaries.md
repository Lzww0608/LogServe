# 46. MVCC、serializable isolation、Kafka partition 与 visibility timeout 追问链

这一批放四个主题：MVCC、serializable isolation、Kafka partition 和 visibility timeout。前两个在数据库并发控制里经常一起出现，后两个在消息系统和任务调度里经常一起出事故。它们的共同点是都容易被一句话讲得很漂亮：读写不阻塞、事务像串行执行、partition 保证顺序、timeout 防止重复消费。问题也正出在这里。面试官真正想听的不是定义，而是你能不能说清楚这些机制保护了什么、没有保护什么、出了问题应该看哪些指标、性能成本藏在哪里。

LogServe 的口径也要收住。它不是数据库内核，也不是 Kafka 或 SQS 的替代品。更合理的说法是：项目里有 shared log、metadata view、任务 lease、重投递和本地恢复这些机制，很多边界可以借 MVCC、隔离级别、partition 顺序和 visibility timeout 来解释，但不能把机制验证说成完整生产级基础设施。

## Q001. 面试官如果只问一个问题检验你是否理解 MVCC，可能会问什么？

**回答：**

我会预期他不直接问“MVCC 是什么”，而是给一个故障场景：

```text
一个数据库表里有一行任务状态，事务 T1 正在把 RUNNING 改成 DONE，事务 T2 同时在查询 RUNNING 任务列表。T2 应该看到旧值、新值，还是被 T1 阻塞？如果 T1 最后回滚，T2 的结果还算不算正确？如果有一个很长的只读事务一直不结束，系统会发生什么？
```

这道题很短，但能把 MVCC 的几个核心点都逼出来：版本、快照、提交可见性、回滚、垃圾回收和长事务成本。只答“读不阻塞写、写不阻塞读”是不够的，那只是表面效果。

我会先说 MVCC 解决的不是“多线程安全”这么宽泛的问题，而是并发事务在同一份逻辑数据上读写时，读者应该看哪个版本。写事务修改一行时，系统不是简单地原地覆盖到所有人都马上可见，而是保留新旧版本，并用事务 ID、提交状态、快照时间或逻辑时间判断某个事务能看到哪些版本。这样一个 SELECT 可以读到自己快照里的稳定世界，不会半路看到别人未提交的一半修改。

然后要把回滚说清楚。MVCC 允许旧版本存在，所以写事务回滚时，别的事务不应该已经依赖它的未提交版本。已经提交的版本才会进入别人的可见范围。不同数据库实现细节不一样：有的把旧版本放在 undo 里，有的行版本带创建和删除事务信息，有的用 timestamp 或 hybrid timestamp 做可见性判断。但面试时不用背存储格式，关键是说出“可见性由事务快照和提交状态决定，不是由最后一次物理写入决定”。

接着要说长事务。这是很多人漏掉的部分。MVCC 不是免费保留历史。只要有老快照还活着，数据库就不能随便清理它可能需要的旧版本。PostgreSQL 里会表现为 vacuum 回收受阻、dead tuple 增多、表和索引膨胀；InnoDB 里可能表现为 undo history 变长、purge 追不上、读取旧版本时要沿 undo 链回溯。业务上看起来可能只是一个报表查询开了很久，底层却让整个库的空间回收和版本链变差。

如果结合 LogServe，我会把它类比到 metadata view 和 shared log：metadata 是当前状态投影，shared log 是事实来源。我们没有实现数据库级 MVCC，但也有类似问题：读路径要知道自己读的是哪个状态版本，重放和投影要知道哪些旧状态还能被读者使用，不能一边改当前状态一边让恢复路径失去事实依据。

面试里可以这样收束：MVCC 的核心是多版本可见性，不是简单的“无锁读”。它让事务读到符合自己快照的已提交版本，同时让写入和清理承担版本维护成本。真正理解 MVCC，要能解释一次并发读写里谁看见哪个版本、回滚为什么不会污染读者、长事务为什么会拖住清理，以及隔离级别如何改变快照边界。

## Q002. MVCC 的一句话定义是否容易误导，误导点在哪里？

**回答：**

容易误导。最常见的一句话是：MVCC 通过保存多版本，让读写互不阻塞。这句话方向没错，但面试里只说到这里，基本等于把最难的部分跳过去了。

第一个误导是把 MVCC 当成“没有锁”。MVCC 减少了读写之间的冲突，不等于系统不用锁。写写冲突仍然要处理，唯一约束仍然要检查，索引结构仍然要保护，事务提交状态仍然要同步。PostgreSQL 即使用 MVCC，也有行锁、表锁、predicate lock、advisory lock；InnoDB 也有 record lock、gap lock、next-key lock。读不阻塞写，是在普通快照读这类路径上成立，不是所有 SQL 都不加锁。

第二个误导是把“多版本”理解成“读到历史任意版本”。大多数 OLTP 数据库保留旧版本是为了事务隔离和回收窗口，不是为了给应用做无限时间旅行。旧版本能保留多久，受活跃事务、undo、vacuum、purge、retention 和空间压力影响。你不能把 MVCC 当审计日志，也不能指望它替代 event sourcing。

第三个误导是忽略隔离级别。Read Committed 往往是每条语句拿一个新快照，Repeatable Read 或 Snapshot Isolation 通常让事务内多次查询看到同一个起点快照，Serializable 还要检测会不会产生不可串行化的结果。都叫 MVCC，读到的世界并不一样。只说“快照读”，不说快照什么时候建立，等于没说清楚语义。

第四个误导是把 MVCC 当作正确性终点。MVCC 只能控制数据库内部的版本可见性。业务不变量还要靠约束、事务边界、锁、唯一索引、版本号或 Serializable 检测来保护。典型例子是两个医生同时判断“至少还有一个医生值班”，各自把自己下线；每个事务在自己的快照里都看见还有别人，最后却没人值班。这类 write skew 不是“有没有旧版本”能自动解决的。

第五个误导是低估性能成本。多版本意味着额外写入、额外元数据、旧版本扫描、清理任务、索引膨胀、事务 ID 或 timestamp 管理。读不阻塞写，代价常常转移到了版本判断和后台清理上。系统轻载时看不出来，高并发、长事务、热点更新、批量删除时才会暴露。

更准确的一句话可以这样说：MVCC 是用多个已提交或未提交的数据版本，加上事务快照和可见性规则，来实现并发读写隔离；它减少读写阻塞，但仍需要冲突检测、锁、清理和隔离级别配合。这个定义没有那么顺口，但它不骗自己。

## Q003. MVCC 最常见的生产事故触发条件是什么？

**回答：**

最常见的触发条件不是“MVCC 算错了”，而是版本清理被长事务拖住，或者应用误解了快照语义。前者带来空间和性能事故，后者带来业务一致性事故。

第一类事故是长事务。一个导出报表、后台巡检、人工控制台查询，打开事务后迟迟不提交。它本身可能只是 SELECT，但它的快照会让数据库保留旧版本。随后业务持续更新，旧版本越积越多。PostgreSQL 里会看到 dead tuple、autovacuum 跟不上、表和索引膨胀、transaction ID wraparound 风险上升；InnoDB 里会看到 history list length 增长、purge lag、undo 空间压力和查询变慢。值班同学经常第一眼看到的是磁盘涨了、CPU 高了、查询慢了，根因却是一条老事务没结束。

第二类事故是热点行更新。比如所有 worker 都更新同一个调度游标、同一个租户计数、同一行全局配置。MVCC 让读者可以看旧版本，但写写冲突不会消失。热点行会产生锁等待、更新链膨胀、索引维护成本和事务重试。应用如果只看到“数据库支持 MVCC”，就把所有并发写压到一个 key 上，后面一定会痛。

第三类事故是把 Read Committed 当成事务级快照。一个事务里先 SELECT 一次，做了一些逻辑，再 SELECT 第二次，结果第二次看到了别的事务刚提交的数据。对数据库来说这是正常语义，对业务代码来说可能是惊吓。复杂查询和更新在 Read Committed 下还可能看到命令级别的混合效果。面试里要敢说：默认隔离级别不等于业务正确。

第四类事故是 write skew。两个事务读到相同快照，分别更新不同的行，单看每一行都没有冲突，合起来破坏了跨行不变量。MVCC 的快照让每个事务都觉得自己安全，直到提交后业务规则被破坏。解决这类问题要用 Serializable、显式锁、唯一约束、物化冲突行、版本检查，或者把不变量转化成数据库能检测的写写冲突。

第五类事故是清理策略和保留策略不匹配。批量删除或更新大量数据，以为事务提交后空间马上回来；实际上旧版本还要等没有事务需要它、后台清理赶上、索引和表完成回收。线上会出现“删除了 80% 数据，磁盘一点没降”的情况。这个时候不能只怪数据库，要看是否有长事务、replication slot、备份、逻辑复制或老快照挡住清理。

第六类事故是连接池和事务边界没管住。代码拿到连接后开启事务，调用远程服务、等用户输入、跑模型推理，最后才提交。数据库事务被拉成长业务流程，MVCC 版本窗口也被拉长。LogServe 这类系统如果未来接入真实数据库，应该避免把 worker 执行、LLM 调用、对象存储上传都包进同一个数据库事务；数据库事务只覆盖最小的元数据变更。

所以我会把 MVCC 事故总结成两句话：版本让读写并发更顺，但旧版本一定要有人清理；快照让读取更稳定，但业务不变量不一定自动安全。线上真正要盯的是长事务、热点更新、隔离级别误用、版本膨胀和清理滞后。

## Q004. MVCC 的指标应该怎么设计才不会只看平均值？

**回答：**

MVCC 的指标不能只看平均查询延迟。平均值很容易把最危险的东西盖住：一个长事务、一条热点更新链、一个 purge lag、一个 autovacuum 卡住的表，都会被全库平均数稀释掉。

第一组指标是事务年龄。要看 active transaction age、oldest snapshot age、idle in transaction duration、long-running read-only transaction、oldest xmin 或类似的全局最老读时间。这里要按连接、用户、应用名、SQL 指纹和租户拆开。真正挡住清理的往往不是平均事务，而是那一个忘了提交的会话。

第二组指标是版本堆积。PostgreSQL 里可以看 dead tuple、live tuple、n_dead_tup、vacuum 次数、autovacuum 延迟、表膨胀、索引膨胀、冻结进度；InnoDB 可以看 history list length、undo 表空间、purge 线程进度。重点不是数值孤立地大不大，而是增长速度和清理速度是否长期失衡。

第三组指标是可见性和扫描成本。MVCC 会让读取时多做可见性判断，旧版本多时，扫描同样的逻辑行可能碰到更多无效版本。要按 SQL 指纹看 p50、p95、p99、p999 延迟，rows scanned 与 rows returned 的比例，heap fetch、index-only scan 失败比例，buffer hit 和 I/O。只看平均查询延迟，会把慢报表和核心 OLTP 混在一起。

第四组指标是写写冲突和等待。包括 row lock wait、deadlock、serialization failure、update conflict、retry count、hot row update rate、事务提交 p99。MVCC 减少读写互相挡住，但写写冲突是另一回事。热点写入要单独拉出来看，不要混在“数据库 QPS 正常”的大盘里。

第五组指标是清理任务本身。看 vacuum/purge 的运行时长、失败次数、被取消次数、每秒清理版本数、每秒产生版本数、清理落后时间、清理时 I/O 和 WAL 压力。如果清理一直在跑但追不上，说明写入或长事务已经超过了系统当前维护能力。

第六组指标是隔离级别和重试。Serializable 或 snapshot isolation 场景下，要统计 serialization failure rate、retry attempts、retry success latency、只读事务和读写事务分布。失败率平均值不够，要按事务类型、表、热点 key、时间窗口拆。一个核心交易事务 5% 重试，和一个后台报表 5% 重试，不是同一个事故等级。

第七组指标是空间和年龄阈值告警。表大小、索引大小、undo 空间、WAL 生成速率、最老事务年龄、事务 ID 消耗速度，都要有阈值和趋势。MVCC 事故经常是慢慢来的，等磁盘报警才处理已经晚了。

面试里可以这样答：MVCC 指标要围绕“最老快照、版本堆积、清理滞后、写冲突、尾延迟和空间增长”设计。平均查询耗时只能说明体验的一小块，不能证明 MVCC 健康。真正该害怕的是一个老事务把整个库拖进版本泥潭。

## Q005. MVCC 的正确性边界和性能边界分别是什么？

**回答：**

MVCC 的正确性边界是事务可见性。它保证一个事务按自己的快照和隔离级别看到合适的数据版本，不看到未提交的脏数据，也不在普通读取中被并发写入撕裂成半新半旧的状态。但它不自动保证所有业务规则，也不自动保证所有执行结果都可串行化。

正确性上先看隔离级别。Read Committed 通常保证每条语句看到语句开始时已提交的数据；Repeatable Read 或 Snapshot Isolation 让事务内多次读更稳定；Serializable 试图保证成功提交事务的结果等价于某个串行顺序。MVCC 是实现材料，隔离级别才是对应用的承诺。面试里把这两层分开，基本就能超过很多背定义的人。

第二个边界是写冲突。两个事务更新同一行，数据库要决定等待、重试、报错或用新版本重新判断条件；两个事务更新不同的行但破坏同一个业务不变量，MVCC 本身未必能看出来。这个时候要靠唯一约束、外键、check constraint、显式锁、SELECT FOR UPDATE、Serializable 或应用级版本号。

第三个边界是外部副作用。数据库事务可以回滚行版本，不能回滚已经发出去的邮件、HTTP 请求、消息、对象存储写入。MVCC 保护数据库内部状态，不保护外部世界。LogServe 如果用数据库事务提交任务状态，也仍然要用 outbox、幂等 key 或可重放日志处理外部副作用。

性能边界则来自版本维护。读写不互相阻塞的代价是保存旧版本、判断可见性、维护 undo 或版本元数据、后台清理、索引膨胀和事务状态管理。短事务、高读并发、读写集合分散时，MVCC 很舒服；长事务、热点更新、大批量更新删除、清理滞后时，成本会集中爆出来。

还有一个边界是存储局部性。版本链太长会让读取旧版本变贵；dead tuple 太多会让扫描变贵；索引指向很多已经不可见的版本，会让 index scan 多做回表和可见性判断。MVCC 的性能问题不一定表现为锁等待，也可能表现为 CPU 花在可见性判断、I/O 花在扫描无效版本、后台清理抢资源。

所以我会这样说：MVCC 的 correctness 边界是“按隔离级别定义版本可见性”，不是“业务永远正确”；performance 边界是“用版本存储和清理成本换读写并发”。适合短事务、快提交、可清理的 OLTP 工作负载；不适合让事务长时间挂着，也不适合把跨系统业务流程塞进一个数据库事务里。
## Q006. 面试官如果只问一个问题检验你是否理解 serializable isolation，可能会问什么？

**回答：**

我会预期他问一个 write skew 或聚合读写场景：

```text
有一张 on_call 表，要求每个科室至少有一名医生值班。Alice 和 Bob 各自开启事务，都看到对方还在值班，于是分别把自己改成 off call。两个事务更新的是不同的行，没有直接写写冲突。数据库在 serializable isolation 下应该允许两边都提交吗？如果不允许，它靠什么发现这个问题？应用该怎么处理失败？
```

这道题比问“Serializable 是什么”更有效。它能检查你是不是知道 serializable 保护的是事务集合的整体结果，而不只是单行锁、脏读、不可重复读、幻读这些教科书现象。

我会先回答结论：在真正的 serializable isolation 下，两个事务不能都成功提交。因为不存在一个串行顺序能解释这个结果。如果 Alice 先执行，Bob 后执行，Bob 应该看到 Alice 已经下线，不能再把自己下线；反过来也一样。两个事务都基于“对方还在线”的旧观察提交，结果破坏了不变量。这就是典型的 serialization anomaly。

然后要说实现不一定是全局大锁。Serializable 不是把所有事务物理排成一列执行。数据库可以用 strict two-phase locking，让读写锁持有到提交；也可以用 optimistic concurrency control，提交时验证读写集合；也可以像 PostgreSQL 那样在快照隔离基础上做 Serializable Snapshot Isolation，监控读写依赖和 predicate lock，发现可能形成不可串行化结果时让某个事务失败。实现不同，但对应用的承诺是相同的：成功提交的事务集合必须等价于某个串行执行。

第三步要说应用责任。Serializable 不是“永不失败”的隔离级别。恰恰相反，为了不提交错误结果，数据库会主动中止某些事务，返回 serialization failure。应用必须把整个事务从头重试，而不是只重试最后一条 SQL。只重试最后一条 SQL 会沿用已经失效的业务判断，等于绕过隔离级别。

第四步要说只读事务。很多人以为只读事务没有风险。一般只读事务不会修改数据，但它读到的结果如果来自一个最后 abort 的事务上下文，就不能拿出去当事实。PostgreSQL 里还有 deferrable read-only serializable transaction 这种模式，可以等到一个安全快照再开始读，减少后续冲突风险。这说明 serializable 不只是写入问题，也和读快照是否能被安全解释有关。

结合 LogServe，我会把它落到元数据更新上：如果 scheduler 根据“当前可运行任务数”和“worker 空闲数”做决策，又同时有多个事务领取任务，serializable 关心的是这些决策合起来能不能解释成某个顺序。LogServe 本身不实现数据库 Serializable，但如果把 metadata 放进真实数据库，关键状态变更应该用唯一约束、版本号、行锁或事务重试保护，而不是只靠读到的旧 snapshot。

面试里可以这样答：Serializable isolation 的重点是让成功提交的并发事务结果等价于某个串行顺序。它不等于没有并发，也不等于不需要重试。真正理解它，要能解释 write skew 为什么危险、数据库如何通过锁或依赖检测发现风险、serialization failure 为什么是正确性机制的一部分，以及应用为什么必须重试整个事务。

## Q007. serializable isolation 的一句话定义是否容易误导，误导点在哪里？

**回答：**

容易误导。常见定义是：Serializable 让并发事务的效果像按某个顺序一个一个执行。这句话很标准，但非常容易被误读。

第一个误导是把“效果像串行”理解成“执行时没有并发”。实际上很多数据库在 Serializable 下仍然并发执行事务。它们只是通过锁、读写依赖、版本验证或冲突检测，保证最终成功提交的结果能被解释成一个串行顺序。两个事务可以同时跑，只是其中一个可能在提交时失败。

第二个误导是以为 Serializable 自动提高性能稳定性。它是正确性级别，不是性能优化。为了阻止不可串行化结果，数据库可能增加锁等待、predicate lock 维护、依赖图检查、事务重试和内存开销。业务冲突越集中，重试越多，尾延迟越难看。把隔离级别调高，通常是在用吞吐和延迟换正确性边界。

第三个误导是把 Serializable 等同于防止脏读、不可重复读、幻读。那些是较低隔离级别用来描述现象的术语。Serializable 的核心不是逐项打勾，而是禁止 serialization anomaly：成功提交的事务集合不能和任何串行顺序矛盾。有些数据库在 Repeatable Read 下已经避免了传统 phantom read，但仍可能有 write skew，所以不能只看那张隔离级别表。

第四个误导是忽略应用重试。Serializable 下返回 serialization failure 不是数据库坏了，而是数据库阻止错误结果的方式。应用如果没有重试预算、没有幂等命令、没有区分可重试错误和业务拒绝，线上会把正确性保护变成用户可见错误。这里最常见的工程 bug 是捕获异常后只重试单条语句，或者在事务外已经发出外部副作用，导致事务重试时副作用重复。

第五个误导是以为所有业务都应该默认 Serializable。强隔离很诱人，但不是免费。简单单行 CRUD、有唯一约束保护的创建、天然幂等的状态更新，可能用 Read Committed 加约束就足够。跨行不变量、库存扣减、额度检查、调度抢占、账户转账、审批状态机这类场景，才更值得认真考虑 Serializable 或显式冲突点。

更稳的一句话是：Serializable isolation 保证所有成功提交事务的结果可以映射到某个串行执行顺序；实现可能仍然并发执行，但会通过锁、版本验证或依赖检测中止有风险的事务，所以应用必须准备重试。这个定义稍长，但不会把面试带到“数据库帮我全兜住”的错觉里。

## Q008. serializable isolation 最常见的生产事故触发条件是什么？

**回答：**

最常见触发条件是业务把“读出来再判断”的不变量放在较低隔离级别里，或者切到 Serializable 后没有处理重试。前者会提交错误结果，后者会把正确性保护变成线上错误率。

第一类事故是 write skew。值班医生、库存池、额度池、任务领取、审批人数、唯一业务条件，这些都可能是跨多行或范围的不变量。两个事务各自读到一个看似安全的快照，更新不同的行，最后一起提交。没有单行写写冲突，但业务已经错了。Serializable 能发现这类风险，Read Committed 或普通 Snapshot Isolation 未必能。

第二类事故是 predicate 范围没被保护。比如“如果不存在同名活跃任务就创建一个”“如果这个时间段没有预约就插入预约”“如果待处理任务数小于阈值就继续入队”。事务读的是一个范围，不是一行。另一个事务可以在这个范围里插入新行。没有唯一索引或 predicate lock，应用以为自己检查过了，实际检查的只是过去某个瞬间。

第三类事故是热点事务导致 serialization failure 暴涨。系统刚上线时并发不高，Serializable 很安静；流量一上来，所有请求都读写同一批账户、同一组调度状态或同一张计数表，提交时依赖冲突堆起来。用户看到的是大量 400/500、请求慢、重试风暴，数据库看到的是 abort 和锁等待。

第四类事故是事务太长。有人把远程调用、文件上传、LLM 推理、用户交互、批量报表都放进 Serializable 事务。事务越长，和别人重叠的窗口越大，读集合越大，发生冲突或被判定有风险的概率越高。长事务还会占住连接、内存、predicate lock 和版本，连无关请求都被拖慢。

第五类事故是重试不幂等。事务失败后应用从头重试，但事务内部或之前已经调用了外部 API、发送消息、写对象存储、发通知。数据库回滚不了这些副作用。Serializable 要求“失败就重试整个事务”，这和外部副作用天然冲突。正确做法通常是事务内只写事实和 outbox，事务提交后由幂等消费者处理外部副作用。

第六类事故是以为 ORM 默认事务就足够。ORM 可能默认 Read Committed，可能把每条语句各自包成事务，也可能在异常后悄悄重试部分操作。业务同学以为“用了事务”，实际上没有保护跨语句判断。面试里要把 transaction、isolation level 和 business invariant 分开说。

第七类事故是监控只看死锁，不看 serialization failure。Serializable 的冲突不一定表现为死锁。PostgreSQL 的 SSI predicate lock 不用于阻塞写入，也不会像普通锁那样直接造成死锁；它更多是用来识别依赖，最后中止事务。如果只盯 lock wait，很多正确性压力会漏掉。

我会总结成一句：Serializable 事故要么来自没用它却需要它，要么来自用了它但没按它的失败模型写应用。上线前要明确哪些不变量需要串行化保护，事务多短，哪些错误可重试，重试是否幂等，外部副作用是否被隔离。

## Q009. serializable isolation 的指标应该怎么设计才不会只看平均值？

**回答：**

Serializable 的指标要把“事务成功提交”和“用户一次业务成功”分开看。平均 SQL 延迟、平均事务耗时都不够，因为最关键的成本常常出现在 abort、重试和尾延迟里。

第一组指标是 serialization failure。要看失败总数、失败率、按事务类型拆分的失败率、按表和索引拆分的失败率、按租户或业务 key 拆分的失败率。一个低频后台任务 10% 失败，和支付路径 1% 失败，处理优先级完全不同。

第二组指标是重试。统计 retry attempts、retry success rate、max retry exceeded、first_try_success_rate、user_visible_failure_rate、retry_added_latency。Serializable 的失败如果被应用正确重试，用户可能只看到慢；如果重试耗尽，用户才看到错。只看数据库 abort 数，不看应用重试结果，会误判影响面。

第三组指标是事务尾延迟。要按事务类型看 p50、p95、p99、p999，从 begin 到 commit 统计，不只看单条 SQL。很多冲突发生在提交前后，单条 SELECT 很快不代表事务快。长尾事务要带 SQL 指纹、调用方、连接池、事务内语句数和是否有外部等待。

第四组指标是读写集合大小。Serializable 的成本和事务读了多少范围、写了多少行、访问了哪些索引有关。可以看 rows read、rows written、range scan 次数、predicate lock 数、SIReadLock 数、锁粒度提升次数、被合并到 page 或 relation 级别的次数。读范围越粗，冲突误伤越多。

第五组指标是锁和等待。虽然 Serializable 不一定靠阻塞实现，但仍要看 row lock wait、table lock wait、deadlock、connection pool wait、active transaction count、idle in transaction。很多系统里 Serializable 和普通锁、唯一约束、外键检查一起工作，等待和 abort 会混在一起出现。

第六组指标是长事务和安全快照。看 oldest serializable transaction age、read-only deferrable wait time、overlap window、事务内远程调用耗时。只读事务如果很多，可以把普通只读、read only deferrable、报表查询分开，不要和写事务混在一张平均图里。

第七组指标是业务不变量保护效果。比如重复预约数、库存负数、任务重复领取、额度超扣、状态机非法转移。这些不是数据库内部指标，但能验证隔离级别是否真的保护了业务。Serializable 的目标不是让数据库图表好看，而是让这些错误不提交。

第八组指标是容量成本。CPU、内存、predicate lock 表大小、事务状态缓存、WAL、buffer churn、连接数，都要和事务冲突率一起看。隔离级别提高后吞吐下降，不一定是某条 SQL 变慢，可能是重试和冲突检测让有效提交数下降。

面试里可以这样答：Serializable 指标要围绕 abort、retry、尾延迟、读写集合、锁等待、长事务和业务不变量设计。平均响应时间只能告诉你“系统大多数时候还行”，不能告诉你“高冲突时是不是还正确，用户是不是被重试拖死”。

## Q010. serializable isolation 的正确性边界和性能边界分别是什么？

**回答：**

Serializable 的正确性边界是成功提交的事务集合。只要事务成功提交，数据库承诺这些事务的结果可以解释成某个串行顺序。失败或被中止的事务不在这个承诺里，应用不能拿它读到的中间结果继续做业务决策。

这个边界有几层含义。第一，Serializable 保护的是数据库内事务，不保护事务外副作用。事务中止后，数据库可以回滚写入，但不能自动撤回已经发出的消息、邮件、HTTP 请求或对象存储写入。第二，它保护的是参与同一个隔离控制的数据。如果你的不变量跨了两个数据库、一个 Redis 和一个外部 API，单库 Serializable 只能保护其中一段。第三，它要求所有相关事务都用兼容的隔离策略。如果一部分路径绕过事务或用较低隔离级别直接修改数据，整体保证就被打破了。

第四，它仍然需要应用表达不变量。数据库不会凭空知道“每个科室至少一个医生值班”。事务必须读到相关范围，或者通过约束、索引、锁、冲突行把不变量变成数据库能观察到的读写依赖。你不读、不锁、不约束，数据库就没有依据替你判断。

性能边界则来自冲突检测和保守中止。Serializable 可以用阻塞换正确性，也可以用乐观执行加提交时验证换并发。无论哪种，冲突越多、事务越长、读写范围越大，成本越高。读多写少、事务短、访问 key 分散时，它可能表现很好；热点写、范围查询、长事务、跨分片事务多时，abort 和等待会快速上升。

对单机数据库，成本常见在锁等待、predicate lock 内存、读写依赖跟踪、WAL、索引和事务重试。对分布式数据库，还会叠加时间戳分配、共识复制、两阶段提交、跨地域 RTT、clock uncertainty、读刷新和提交等待。把 Serializable 从单机搬到分布式，性能边界会更硬。

所以我会这样说：Serializable 的 correctness 是“成功提交结果可串行化”，不是“所有事务都能成功”；performance 是“用等待、检测、abort 和重试换跨事务不变量安全”。它适合保护真正需要全局一致判断的最小事务边界，不适合包住长业务流程，也不适合替代幂等、唯一约束和外部副作用隔离。
## Q011. 面试官如果只问一个问题检验你是否理解 Kafka partition，可能会问什么？

**回答：**

我会预期他问一个顺序和扩容混在一起的题：

```text
你有一个 order-events topic，按 order_id 作为 key 写入 Kafka。现在某个 partition lag 很高，团队想把 partition 数从 12 增加到 48。这样做会不会影响同一个订单的事件顺序？历史数据和新数据会怎么分布？消费者并行度、offset、重试和重复处理会受什么影响？
```

这道题能区分“会用 Kafka”和“理解 Kafka partition”。partition 不是一个随便调大的并行度参数，它同时是日志分片、顺序边界、复制单位、leader 选举单位和 consumer group 分配单位。改它会影响生产、消费、运维和业务语义。

我会先说顺序边界。Kafka 保证的是同一个 topic-partition 内，消费者按写入顺序读取记录。它不保证一个 topic 全局有序。使用 key 时，默认分区器会把同一个 key 的事件路由到同一个 partition，所以同一个 order_id 的事件可以在该 partition 内保持顺序。注意这是“同一个 key 在同一套 partition 规则下”。如果你增加 partition 数，key 到 partition 的哈希取模结果可能改变，新事件可能去新的 partition，历史事件还留在旧 partition。于是同一个 order_id 的历史和未来事件可能分散到不同 partition，跨 partition 就没有总顺序了。

第二步说 offset。offset 是 partition 内的位置，不是 topic 全局序号。`orders-3` 的 offset 100 和 `orders-7` 的 offset 100 没有大小关系。消费者提交 offset，也是按 partition 提交。一个 partition 里某条记录处理失败，通常会挡住这个 partition 后续记录的安全提交；其他 partition 可以继续前进。这是 Kafka 和传统 per-message 队列很不一样的地方。

第三步说消费并行度。在经典 consumer group 模型里，一个 partition 同一时刻只能分配给组内一个 consumer 实例，所以组内有效并行度上限就是 partition 数。consumer 数多于 partition 数，多出来的实例会空闲。反过来，partition 太多也不是免费：每个 partition 都有日志段、索引、leader、follower、文件句柄、fetch 状态、controller 元数据和 rebalance 成本。

第四步说复制。partition 有 leader 和 follower，生产者写 leader，follower 复制日志。高可用不是靠 partition 本身，而是靠 replication factor、ISR、acks、min.insync.replicas、unclean leader election 等配置和 broker 健康。partition leader 挂掉后可以选新 leader，但如果 ISR 不足或配置不当，就会在可用性、延迟和数据安全之间做取舍。

第五步说热 key。按 order_id 分区能保证每个订单内顺序，但如果少数大客户或大订单产生大量事件，会形成 hot partition。总 topic QPS 看起来不高，某个 partition 却 lag 爆炸。这个时候盲目加 consumer 没用，因为同一个 hot partition 仍然只能由一个 consumer 处理。要么改 key 设计，要么拆 topic，要么在业务层放松顺序边界，要么对热 key 做单独处理。

面试里可以这样答：Kafka partition 是 topic 的有序日志分片。它给你分区内顺序、横向读写并行、复制和故障隔离，但不提供 topic 全局顺序，也不让你随意扩 partition 而不改业务语义。理解它，要能解释 key 到 partition 的映射、offset 作用域、consumer group 分配、rebalance、hot partition 和复制安全边界。

## Q012. Kafka partition 的一句话定义是否容易误导，误导点在哪里？

**回答：**

容易误导。常见定义是：Kafka partition 是 topic 的分片，用来提高吞吐和并行度。这个定义没错，但如果只记这一句，线上很容易踩坑。

第一个误导是忽略顺序边界。partition 不是普通 sharding。它是 Kafka 顺序保证的最小单位。只要事件在不同 partition，Kafka 就不承诺它们的相对顺序。很多业务说“我要订单事件有序”，真正应该问的是“按什么 key 有序”。按 order_id 有序、按 user_id 有序、按商户有序、全局有序，是四种完全不同的成本。

第二个误导是把 partition 数当成可以无限增加的并行开关。partition 多了，producer 和 consumer 可以更并行，但 broker 也要维护更多日志段、索引文件、leader/follower 状态、fetch 请求、replication 流和元数据。controller、rebalance、恢复时间、打开文件数都会受影响。吞吐瓶颈如果在下游数据库或单个 hot key，加 partition 不会救你。

第三个误导是忽略 key 映射变化。增加 partition 数通常会改变 key 到 partition 的映射。新消息按新规则写，老消息不搬家。这样同一个 key 的历史事件和新事件可能不在同一个 partition，跨 partition 读取时顺序无法直接比较。很多团队扩 partition 前没有评估这一点，后来才发现订单状态机偶发倒序。

第四个误导是把 replication 和 partition 混在一起。partition 是分片；replica 是副本。一个 topic 可以有多个 partition，每个 partition 又有多个 replica。吞吐、顺序、容灾分别由不同配置影响。只说“我有 24 个 partition，所以很可靠”，这句话没有意义。可靠性要看 replication factor、ISR、acks、min.insync.replicas、broker 分布和故障恢复。

第五个误导是以为 consumer 提交 offset 后就等于业务完成。offset 是消费位置，不是业务事务。业务写数据库成功但 offset 提交失败，会重复处理；offset 提交成功但业务写失败，会丢业务效果。Kafka partition 只给你日志位置，端到端正确性还要靠幂等、事务、outbox、去重表或可重放处理。

第六个误导是以为 Kafka 像 SQS 一样有单条消息 visibility timeout。经典 Kafka consumer 没有 per-message visibility timeout。consumer 拉取一批记录，处理后提交 offset；消费者崩溃或 rebalance 后，会从已提交 offset 之后继续读。失败重试通常会挡住该 partition 的后续提交，或者需要把失败记录送到重试 topic / DLQ。把 SQS 的单条消息模型搬到 Kafka，会设计出很别扭的消费者。

更准确的定义是：Kafka partition 是 topic 内一段可追加、可复制、带独立 offset 的有序日志；它定义了顺序、存储、复制和 consumer group 并行度的基本边界。这样说不如“分片”简单，但面试里更安全。

## Q013. Kafka partition 最常见的生产事故触发条件是什么？

**回答：**

最常见触发条件是 partition 设计和业务顺序边界不一致，或者某个 partition 变热后团队只按总吞吐排查。Kafka 事故经常不是集群完全挂掉，而是某几个 partition 悄悄拖住整个业务。

第一类事故是 hot partition。key 分布不均，少数 key 占了大部分流量。比如一个大租户、一场促销、一个热门直播间、一个全局默认 key。topic 总 lag 看着还能接受，但某个 partition 的 lag、oldest record age、consumer processing time 持续上升。consumer 扩容后没有改善，因为那个 partition 仍然只能给一个 consumer 处理。

第二类事故是错误分区键。生产者为了方便把 key 留空，默认分区器可能按批次或粘性策略分发，结果同一个业务实体的事件散到多个 partition。状态机消费者按到达顺序处理，就会看到 `OrderPaid` 先到、`OrderCreated` 后到。反过来，所有消息都用同一个 key，又会把 topic 压成单 partition 的性能。

第三类事故是扩 partition 破坏 key 顺序。原来 12 个 partition 时，某个 order_id 一直写到 partition 5；扩到 48 后，同一个 order_id 新事件映射到 partition 17。消费者如果并行读两个 partition，就可能先处理新事件再处理旧 backlog。这个问题不一定立刻爆，通常在扩容、重放、补数据时暴露。

第四类事故是 rebalance 频繁。consumer 心跳超时、处理时间超过 poll 间隔、部署滚动太频繁、实例数抖动，都会导致 partition ownership 变化。rebalance 期间消费暂停，已经拉取未提交的记录可能被新 owner 再次处理。症状是 lag 锯齿、重复处理增加、某些 partition 间歇停顿。

第五类事故是 offset 提交时机错误。业务处理前提交 offset，失败时消息不会再被正常读取；业务处理后提交 offset，提交失败时会重复处理。两种窗口都存在，不能靠“Kafka 很可靠”掩盖。正确做法通常是让业务处理幂等，或者把消费、业务写入和 offset 记录放进同一个外部事务边界里，至少能在恢复时判断处理到哪里。

第六类事故是复制安全配置不匹配。`acks=1`、ISR 频繁缩小、`min.insync.replicas` 太低、unclean leader election 打开，都会在 broker 故障时把“写入成功”的理解变得危险。生产者以为 ack 了就是多副本安全，实际可能只有 leader 收到了。相反，强配置会提高安全性，但在 ISR 不足时写入可用性下降。

第七类事故是大消息和慢消费者。单条消息太大，会拖慢 fetch、压缩、磁盘和网络；消费者处理一条坏消息卡住，后续同 partition 记录无法安全提交。Kafka partition 的顺序保证意味着 poison record 会变成 partition 级阻塞，处理策略要提前设计：跳过、DLQ、修复后重放，还是停住等待人工。

我会把它总结成一句：Kafka partition 事故通常不是“Kafka 不工作”，而是顺序、key、offset、rebalance、复制和下游幂等没对齐。排查时先按 partition 看，不要只看 topic 总平均。

## Q014. Kafka partition 的指标应该怎么设计才不会只看平均值？

**回答：**

Kafka partition 的指标必须按 partition 维度看。topic 平均 lag 是很危险的指标。99 个 partition 没积压，1 个 hot partition 堆了几百万条，平均值可能还挺好看，但业务已经卡死在那一个 key 上。

第一组是 per-partition lag。看 current offset、committed offset、log end offset、records lag、records lag max、oldest unprocessed record age。lag 要同时看条数和时间。10 万条小消息和 10 万条大消息不是一回事；1 分钟的 lag 和 6 小时的 lag也不是一回事。

第二组是 key skew。统计每个 partition 的 bytes in、records in、top key、key cardinality、最大 key 占比。Kafka 自己不一定直接给你 top key，需要在 producer、stream processor 或采样日志里补。没有 key skew 指标，就只能在事故后猜哪个租户把 partition 打热。

第三组是 producer 指标。按 topic-partition 看 produce latency、batch size、compression ratio、record error、record retry、out-of-order error、request timeout、throttle time。还要看 `acks`、idempotent producer、in-flight request 和 retry 设置对顺序的影响。生产侧已经重试乱了，消费侧很难补救。

第四组是 broker 和复制指标。看 leader 分布、under-replicated partitions、offline partitions、ISR shrink/expand、replication lag、leader election、unclean election、disk usage per broker、network in/out、page cache 命中。partition 不是孤立逻辑对象，它最终落在 broker 磁盘和网络上。

第五组是 consumer group 指标。看 rebalance 次数、rebalance 时长、assigned partitions、poll interval、heartbeat failure、commit latency、commit failure、records consumed rate、processing latency。一个 consumer group 健康，不只是 lag 低，还要 ownership 稳定。

第六组是处理语义指标。包括 duplicate_processed、idempotency_hit、business_write_failure_after_poll、offset_commit_failure_after_write、DLQ count、retry topic backlog、poison record count。Kafka 的 offset 和业务状态是两套东西，指标也要分别看。

第七组是尾部和分布。每个 partition 的 p95/p99 produce latency、fetch latency、processing latency、commit latency 都要看。不要把所有 partition 混成一个 p99，因为热 partition 的延迟可能被大盘稀释。告警也应该能直接指出 topic、partition、consumer group、consumer instance 和可能的 key。

第八组是扩容和变更指标。增加 partition、滚动部署、broker 下线、leader reassign、consumer 扩缩容时，要看 lag 是否重新分布、rebalance 是否变长、old key 是否跨 partition 乱序、某些 partition 是否无人消费。Kafka 的很多事故发生在“改配置以后”。

面试里可以这样答：Kafka partition 指标要按 partition、key、consumer group 和 broker 拆开。平均 lag、平均吞吐只能做背景。真正能救事故的是 hot partition、oldest record age、rebalance、offset commit、ISR、leader 分布和业务幂等指标。

## Q015. Kafka partition 的正确性边界和性能边界分别是什么？

**回答：**

Kafka partition 的正确性边界首先是分区内顺序。给定一个 topic-partition，Kafka 可以让消费者按写入顺序读取记录。这个保证不跨 partition，不跨 topic，也不自动覆盖业务处理完成顺序。消费者并行处理、异步写库、失败重试、DLQ 回灌，都可能改变业务观察到的顺序。

第二个正确性边界是 key。只有同一业务实体稳定映射到同一个 partition，分区内顺序才对这个实体有意义。key 为空、key 选择错误、partition 数变化、生产者分区器变化，都可能破坏你以为存在的顺序。业务如果要全局顺序，Kafka 多 partition 通常不是免费答案；要么接受单 partition 成本，要么重新定义顺序边界。

第三个边界是 offset。offset 表示日志位置，不表示业务成功。Kafka 可以保存记录、复制记录、让 consumer 从某个 offset 继续读，但它不知道你的数据库事务是否提交、邮件是否发出、对象是否上传成功。端到端 exactly-once 需要生产、处理、状态存储、offset 提交一起设计，不能靠 partition 单独完成。

第四个边界是复制和持久化。生产者收到 ack 的含义取决于 `acks`、idempotence、ISR、min.insync.replicas 和 broker 故障模式。partition replica 能提高容灾，但如果配置为了可用性牺牲一致性，某些故障下仍可能丢已确认数据或产生截断。正确性要看配置组合，不是看“Kafka 有副本”这几个字。

性能边界则来自并行度和资源成本。partition 数越多，理论并行度越高，但每个 partition 都要消耗 broker 文件、索引、内存、网络复制、controller 元数据和 consumer 管理成本。partition 太少会限制吞吐和消费并行；partition 太多会拖慢恢复、rebalance、leader election 和运维操作。

第二个性能边界是 hot key。总 partition 数再多，如果业务 key 极度倾斜，热点仍然集中在一个 partition。这个时候瓶颈不是 Kafka 集群总容量，而是单 partition leader、单 consumer、单 key 下游处理能力。解决方式通常要回到业务顺序要求，而不是继续堆机器。

第三个性能边界是端到端处理。Kafka 写入很快，不代表业务链路快。消费者处理、数据库写入、对象存储、外部 API、幂等去重表、重试 topic 都可能成为瓶颈。partition 只是把日志分开，不能替下游扩容，也不能消除顺序阻塞。

所以我会这样说：Kafka partition 的 correctness 是“分区内有序、offset 可定位、复制按配置提供持久性”，不是“topic 全局有序或业务 exactly-once”；performance 是“用更多 partition 换并行读写”，但受 hot key、consumer group、broker 资源和下游处理约束。设计 partition 时，先定顺序边界，再谈吞吐。
## Q016. 面试官如果只问一个问题检验你是否理解 visibility timeout，可能会问什么？

**回答：**

我会预期他问一个慢 worker 和重复执行的场景：

```text
一个消费者从队列里拿到消息，visibility timeout 是 30 秒。它处理了 45 秒才写数据库并 ack。第 30 秒时消息重新可见，被另一个消费者拿走。两个消费者最后都可能写数据库。这个系统应该怎么设计，才能既不丢消息，也不把同一任务执行两次造成错误？
```

这道题能直接测出你是不是理解 visibility timeout 的本质。它不是锁，也不是 ack，也不是“这段时间内绝对不会重复”。它只是消息被某个消费者 receive 之后，队列暂时不把这条消息交给其他消费者。消费者必须在 timeout 之前完成处理并删除或确认消息；否则消息会重新可见，进入下一轮投递。

我会先说状态机。消息在队列里通常有 visible、in-flight、deleted 或 dead-letter 这些状态。receive 之后进入 in-flight，对其他消费者不可见；处理成功后调用 DeleteMessage 或 ack，消息才算从队列角度完成；如果消费者崩溃、网络断开、处理太慢、delete 失败，visibility timeout 到期后消息重新变成 visible。这个机制解决的是“消费者死了以后消息不能永远卡住”，不是“消费者活着就一定不会重复”。

然后说关键窗口。业务写数据库成功但 delete/ack 失败，消息会重投，必须靠业务幂等避免重复效果。delete/ack 成功但业务写失败，队列认为消息完成了，业务结果丢了。timeout 到期时旧 worker 可能还活着，新 worker 又开始处理，于是并发执行同一任务。这个窗口只能靠幂等键、任务状态 CAS、fencing token、唯一约束、去重表或事务性 outbox 缩小损害，不能靠调大 timeout 根治。

第三步说续租。长任务不应该简单把 visibility timeout 设置成几个小时。更稳的做法是设置一个覆盖常见处理时间的初始 timeout，然后 worker 定期 heartbeat，通过 ChangeMessageVisibility 或类似机制延长可见性。续租要有最大总时长，避免卡死 worker 无限占着任务。任务如果天然很长，应该拆成阶段，或者把队列消息当“启动信号”，真正进度放在可恢复的任务表里。

第四步说标准队列和 FIFO 队列差异。标准队列通常是 at-least-once，visibility timeout 期间仍不能绝对保证消息不会被重复投递，所以应用必须能处理重复。FIFO 队列里，同一个 message group 内前一条消息 in-flight 时，后面的消息不会正常交付；这保证组内顺序，但也意味着一条慢消息会拖住整个 group。

结合 LogServe，可以把 visibility timeout 类比为任务 lease：worker 拿到 task 后在 lease 期内执行，成功后提交结果并释放；worker 崩溃或失联后 lease 到期，scheduler 可以重新投递。LogServe 的关键不是“timeout 配多少秒”，而是任务完成提交是否幂等、旧 worker 恢复后是否会被 fencing、重投递是否能从日志里解释。

面试里可以这样答：visibility timeout 是消息被领取后的临时不可见窗口。它让失败任务有机会重新投递，但不保证绝不重复，也不等于业务成功。正确设计要把处理、幂等提交、ack/delete、续租、DLQ 和旧 worker fencing 放在一起讲。

## Q017. visibility timeout 的一句话定义是否容易误导，误导点在哪里？

**回答：**

容易误导。常见定义是：visibility timeout 是消息被消费者取走后，对其他消费者不可见的一段时间。这句话本身没错，但很容易让人以为它是分布式锁。

第一个误导是把不可见当成互斥保证。很多队列在正常情况下会避免其他消费者拿到这条消息，但 at-least-once 系统不承诺绝不重复。服务端复制、网络重试、delete 丢失、消费者超时、队列内部恢复，都可能让同一条消息被再次投递。visibility timeout 只能减少并发处理概率，不能替代幂等。

第二个误导是把 timeout 当成处理成功。消息不可见，不代表业务完成；消息重新可见，也不代表上一轮一定失败。上一轮消费者可能只是慢，可能刚写完数据库还没 ack，可能 ack 请求在网络里丢了。系统不知道真实业务状态，只知道“这条消息在规定时间内没有被删除”。

第三个误导是以为调大就安全。timeout 太短会导致早退场，长任务被重复执行；timeout 太长会导致真正失败的消息很久才重试，还会占用 in-flight 配额。SQS 标准队列有 in-flight 数量上限，卡住的消息太多时，短轮询可能拿到 OverLimit，长轮询可能只是拿不到新消息。调大 timeout 其实是在用恢复时间和容量换重复概率。

第四个误导是忽略动态续租。任务耗时有分布，不能只拿平均值配 timeout。P50 处理 5 秒，P99 处理 3 分钟，固定 30 秒就会让长尾重复；固定 10 分钟又会让失败恢复太慢。更合理的是 heartbeat 续租，并把续租失败、续租次数和最大执行时长纳入任务协议。

第五个误导是把 visibility timeout 和 retry delay 混为一谈。visibility timeout 控制 in-flight 消息什么时候重新可见；retry policy 控制失败后什么时候重试、重试几次、是否退避、是否进 DLQ。你可以把 timeout 改成 0 让消息马上可见，也可以延长 timeout 继续处理，但这不是完整的错误分类和退避策略。

第六个误导是忘记 FIFO group 阻塞。FIFO 队列按 message group 保序，同组前一条消息未删除或未重新可见时，后续消息不会被交付。一个过长 timeout 或卡住任务，会把整个 group 后面的消息都拖住。这里的性能问题不是“消费者不够”，而是顺序边界挡住了并行。

更准确的定义是：visibility timeout 是消息被领取后的临时隐藏期限；期限内消费者应该完成业务并确认删除，期限到期未确认则消息可被再次投递；它提供失败恢复窗口，不提供 exactly-once，也不替代幂等和租约 fencing。

## Q018. visibility timeout 最常见的生产事故触发条件是什么？

**回答：**

最常见触发条件是 timeout 和真实处理时间不匹配，再叠加消费者不幂等。短了会重复执行，长了会恢复慢。两个方向都能出事故。

第一类事故是 timeout 太短。任务平时 10 秒完成，偶尔因为下游数据库抖动、对象存储慢、模型推理排队，处理时间涨到 60 秒。visibility timeout 还是 30 秒，消息到期重新可见，被另一个消费者拿走。旧消费者和新消费者同时执行，最后可能重复扣款、重复发通知、重复写结果。日志里看起来像“同一任务被两个 worker 处理”，根因是 timeout 没覆盖长尾，业务提交也没有 fencing。

第二类事故是 timeout 太长。消费者拿到消息后进程卡死，消息要等很久才重新可见。队列 backlog 变大，oldest message age 上升，用户觉得任务不动。更糟的是大量消息都处于 in-flight，达到服务端配额后，新的 receive 拿不到消息。此时系统不是没有消息，而是消息都被“隐身”占住了。

第三类事故是 ack/delete 顺序和业务提交顺序错了。先 ack 后写业务，写失败会丢结果；先写业务后 ack，ack 失败会重复处理。这是消息系统里最经典的窗口。visibility timeout 到期只是把窗口放大出来。解决方式不是找一个完美顺序，而是让业务写入幂等，并能在重复投递时判断“这条消息对应的业务结果已经提交过”。

第四类事故是没有 heartbeat 续租。长任务开始时拿到 30 秒 timeout，处理过程没有延长；或者续租线程和主处理线程分离，主线程已经卡死，续租还在继续，把坏任务无限占住。续租必须和真实进度绑定，最好写入可恢复进度，并设置最大租约时间。

第五类事故是重试风暴。下游服务故障 5 分钟，成千上万条消息都处理失败或超时。timeout 一到，它们同时重新可见，消费者再次打向同一个下游，下游还没恢复就被压垮。没有 backoff、jitter、全局限流和错误分类，visibility timeout 会把局部故障变成周期性风暴。

第六类事故是 DLQ 策略缺失或误用。某条 poison message 每次都会失败，timeout 到期就回来，反复占用消费者。没有 max receive count 和 DLQ，它会永远循环；有 DLQ 但 redrive 时不修数据，还是会再次打爆主队列。DLQ 不是垃圾桶，是隔离和诊断通道。

第七类事故是队列语义和业务顺序冲突。FIFO message group 里一条慢消息卡住，后续同组消息都不交付；标准队列里同一业务实体的消息并发到达，应用又没有按实体加幂等和顺序控制。visibility timeout 只管单条消息是否可见，不会理解你的业务状态机。

第八类事故是 worker 退出不优雅。Kubernetes 滚动发布或进程收到 SIGTERM 后，worker 继续拿新消息，或者已经处理的消息没来得及 ack。硬杀之后消息重投，短时间内重复率升高。队列消费者要先停止拉新，再处理或释放手头消息，最后退出。

我会总结成一句：visibility timeout 事故来自“用时间猜测处理是否完成”。只要处理时间有长尾、下游会抖、ack 会失败、worker 会重启，就必须把幂等、续租、最大投递次数、DLQ、退避和 graceful shutdown 一起设计。

## Q019. visibility timeout 的指标应该怎么设计才不会只看平均值？

**回答：**

visibility timeout 的指标要围绕消息生命周期设计。只看平均处理耗时没有用，因为真正危险的是长尾、in-flight 堆积、重复投递和最老消息年龄。

第一组是队列状态。看 visible messages、not visible / in-flight messages、delayed messages、oldest visible message age、oldest in-flight age。可见消息多，说明 backlog；不可见消息多，说明消费者拿了但没完成；最老消息年龄高，说明用户体验已经受影响。

第二组是处理时长分布。按消息类型、队列、租户、handler 统计 processing time p50/p95/p99/p999，不要只看平均值。visibility timeout 至少要和长尾有关，最好用“处理耗时 / 当前 timeout”的比例告警。p99 已经接近 timeout 时，重复执行只是时间问题。

第三组是 timeout 和续租。统计 visibility_timeout_expired、change_visibility_success、change_visibility_failure、heartbeat_late、lease_extension_count、max_extension_reached。续租失败和续租过多都值得告警：前者会早重投，后者可能是任务卡住。

第四组是重复投递。看 receive count 分布、duplicate delivery rate、same message concurrent processing、idempotency hit、fencing rejected old worker。重复不一定是事故，但重复后没有被幂等拦住就是事故。最好能按 message id、business id 和 worker id 关联。

第五组是 ack/delete。统计 delete success、delete failure、delete latency、ack after business commit failure、business commit success but ack failure。很多系统只记“处理成功”，不记 ack 是否成功，事故后就解释不了为什么消息又回来了。

第六组是 DLQ 和错误分类。看 retryable error、non-retryable error、DLQ count、DLQ age、redrive count、redrive success rate。DLQ 暴涨说明有数据或下游问题；DLQ 长期没人处理，说明失败被藏起来了。

第七组是 in-flight 配额和消费者健康。看 in-flight quota usage、receive empty rate、OverLimit、consumer heartbeat、graceful shutdown duration、SIGTERM drain success、worker crash count。队列里明明有消息却收不到，可能就是 in-flight 被占满，而不是没有生产。

第八组是下游背压。visibility timeout 常常被下游拖累，所以要把数据库写入 p99、对象存储 p99、外部 API 错误率、连接池等待、限流次数和消息处理指标放在同一张排障视图里。只盯队列，会把根因看成“消费者慢”。

第九组是 FIFO group 或业务 key 维度。如果有顺序组，要看 per-group backlog、oldest group age、blocked group count、top blocked group。平均队列延迟正常，不代表某个 VIP 租户的 group 没被一条坏消息卡住。

面试里可以这样答：visibility timeout 指标要看可见、不可见、最老年龄、处理长尾、续租、重复投递、ack/delete、DLQ、in-flight 配额和下游背压。平均处理时间只能用来估算，不适合做正确性判断。

## Q020. visibility timeout 的正确性边界和性能边界分别是什么？

**回答：**

visibility timeout 的正确性边界很窄：它只控制消息在一段时间内是否对其他消费者可见。它不证明消费者还活着，不证明业务已经成功，不保证消息不会重复，也不保证旧消费者不会在 timeout 之后继续提交结果。

正确性上第一条底线是业务幂等。消费者必须假设同一消息可能被处理多次。可以用 message id、业务 idempotency key、唯一约束、任务状态版本、CAS、fencing token 或去重表来保护提交。没有幂等，visibility timeout 只能降低重复概率，不能让系统正确。

第二条底线是提交顺序要可恢复。理想状态是业务结果和消息处理进度在同一个事务边界里提交；做不到时，至少要能在重复投递时检查业务结果是否已经存在。ack/delete 不能作为业务事实来源。队列说“消息删了”，不等于数据库里结果一定写了；数据库说“结果写了”，不等于队列一定不会再投。

第三条底线是旧 worker 要被挡住。timeout 到期后，新 worker 可能接手；旧 worker 如果稍后醒来，不能用过期租约写入最终结果。任务表里的 attempt、generation、lease token、version 都可以作为 fencing。没有 fencing，visibility timeout 只是口头约定。

第四条底线是失败要有终点。可重试错误可以退避，不可重试错误要进 DLQ 或失败表。无限重投会拖垮队列，也会掩盖数据质量问题。DLQ 里的消息 redrive 前必须修复根因，否则只是把事故重新播放一遍。

性能边界则是 timeout 对恢复时间、重复率和容量的三角取舍。timeout 短，失败恢复快，但慢任务更容易重复；timeout 长，重复少一些，但 worker 崩溃后恢复慢，in-flight 占用时间长。没有一个全局最佳值，必须按任务类型、处理时长分布和下游容量配置。

第二个性能边界是续租成本。heartbeat 太频繁会增加队列 API 调用和控制面压力；太稀疏会在网络抖动时误超时。续租还可能掩盖卡死任务，所以要和 progress、最大执行时长、取消信号绑定。

第三个性能边界是顺序阻塞。FIFO group、单任务 key、或者应用层按业务实体串行处理时，一条慢消息会挡住后续消息。提高消费者数量不一定有用，因为顺序边界限制了并行度。这个问题和 Kafka hot partition 很像：瓶颈在 key，不在机器数。

第四个性能边界是下游容量。队列让任务重新可见，不代表下游恢复了。没有 backoff 和限流时，visibility timeout 会制造重试波峰。性能设计要把消费者并发、下游连接池、QPS 限制、重试间隔和 DLQ 策略一起算。

所以我会这样说：visibility timeout 的 correctness 是“失败后可重投递的时间窗口”，不是 exactly-once；performance 是“在重复概率、失败恢复时间、in-flight 容量和 API 成本之间取平衡”。它适合做任务租约和失败恢复，但必须配合幂等、fencing、续租、DLQ 和背压。单独一个 timeout 参数救不了系统。