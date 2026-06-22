# WAL、append-only log、redo/undo 与恢复语义

这份题库整理 WAL、append-only log、redo/undo、checkpoint、LSN 和 group commit 相关问题。重点不是背术语，而是能说清楚：系统什么时候可以向用户承诺提交成功，崩溃后又靠什么把状态恢复到一致点。

## Q001. WAL 的核心思想是什么？

**回答：**

WAL 是 Write-Ahead Logging，核心思想很直接：在修改真正的数据页之前，先把“将要做什么修改”写进日志，并且在需要持久化提交时，先保证日志已经落到稳定存储。

它解决的是崩溃恢复问题。数据库运行时通常会把数据页缓存在内存里，事务提交时不可能每次都把所有被改过的数据页刷盘。那样太慢，也会产生大量随机 I/O。WAL 把持久化路径拆成两层：

- 事务提交时，只要求顺序写 WAL 并同步到磁盘。
- 数据页可以稍后由后台线程刷盘。
- 如果系统崩溃，重启后用 WAL 把缺失的数据页修改重放回来。

这背后有一条关键规则：

```text
日志必须先于被它保护的数据页持久化。
```

举个简单例子。事务 T 把账户 A 的余额从 100 改成 80：

1. 生成 WAL record：`T 修改 page P 上的 A: 100 -> 80`。
2. 把 WAL record 写到 WAL buffer。
3. commit 时把 WAL flush 到稳定存储。
4. 返回客户端“提交成功”。
5. 数据页 P 可以稍后再刷盘。

如果第 4 步之后机器崩溃，但数据页 P 还没刷盘，恢复时可以从 WAL 中看到这次修改，并 redo 到数据页。如果数据页 P 已经刷盘，也没问题，恢复时根据 pageLSN 或类似机制判断是否需要重复应用。

WAL 的价值主要有几个：

1. **把随机写变成顺序写**

   数据页可能分散在很多位置，直接刷数据页会产生随机 I/O。WAL 是 append-only 写入，通常更适合磁盘和 SSD。

2. **缩短提交路径**

   提交时只需要保证日志持久化，不必把所有数据页同步完成。多个事务还可以 group commit，共享一次 flush。

3. **支持崩溃恢复**

   崩溃后，系统可以从 checkpoint 后的 WAL 开始重放，恢复未刷盘的数据页修改。

4. **支持复制和时间点恢复**

   许多数据库会把 WAL 发送给副本，或者归档 WAL 做 point-in-time recovery。只要有基础备份和连续 WAL，就能恢复到某个时间点附近。

但 WAL 不是万能的。它不自动解决事务隔离，不替代锁或 MVCC；不自动解决数据损坏，需要 checksum 等机制；不自动解决分布式提交，需要复制协议和 quorum 语义。WAL 只是把本地持久化和恢复这件事做得可控。

一句面试回答可以这样说：WAL 的核心是先记录意图或变更，再修改数据页；提交只需要保证日志持久，崩溃后再用日志重放或回滚，让系统回到一致状态。

## Q002. 为什么数据库通常先写 WAL 再修改数据页？

**回答：**

严格说，数据库可以先在内存里修改数据页，但在把这个脏数据页写到磁盘之前，必须先把对应的 WAL 持久化。这个规则叫 write-ahead rule。很多面试题说“先写 WAL 再修改数据页”，想考的其实是这条持久化顺序。

原因是：如果数据页先落盘，而描述这次修改的 WAL 没有落盘，崩溃后数据库就解释不了这个数据页为什么变成了现在这样。

看一个反例：

1. 事务 T 修改 page P。
2. 数据页 P 被后台线程刷到了磁盘。
3. 对应 WAL record 还没落盘。
4. 机器崩溃。

重启后，磁盘上的 page P 已经包含 T 的修改，但 WAL 里没有这条记录。数据库不知道 T 到底提交了没有，也不知道如何 undo 或 redo。这个状态很危险：数据页包含了一个没有日志证明的修改。

WAL 规则要求：

```text
在 page P 被写回磁盘之前，page P 上最新修改对应的 WAL 至少要先持久化到该修改的 LSN。
```

这样崩溃后有两种情况：

1. **数据页没刷盘，WAL 已落盘**

   恢复时 redo WAL，把修改补到数据页。

2. **数据页已刷盘，WAL 也已落盘**

   恢复时看到数据页的 pageLSN 已经包含该修改，可以跳过或确认它已应用。

这也是为什么数据库可以采用 no-force 策略：事务提交时不强制刷所有数据页，只强制刷 WAL。它也能采用 steal 策略：未提交事务修改过的数据页可能被刷盘，但前提是有 undo 信息可以恢复。

先写 WAL 的工程收益很明显：

- 提交路径更短，只同步顺序日志。
- 后台可以按 I/O 策略刷数据页。
- checkpoint 可以批量推进持久化边界。
- 崩溃恢复有完整依据，不需要猜数据页状态。

如果没有 WAL，数据库要么每次提交都强制刷所有被改数据页，性能很差；要么崩溃后无法判断哪些修改成功、哪些修改只完成了一半。

所以关键不是“内存里绝对不能先改页”，而是“数据页落盘之前，保护它的日志必须先落盘”。这是 WAL 的底线。

## Q003. WAL 中的 redo log 和 undo log 有什么区别？

**回答：**

redo log 和 undo log 都是为了恢复，但方向不同。

- redo log 用来“把应该存在但还没落到数据页的修改补上”。
- undo log 用来“把不应该保留的修改撤销掉”。

可以从事务状态和崩溃场景理解。

**redo log**

redo 关注的是已提交修改。事务 commit 之后，如果数据页还没刷盘，机器崩溃会导致磁盘上的数据页缺少这次修改。redo log 保存了足够的信息，让恢复过程能重新应用这次修改。

例如：

```text
LSN 100: page 7, offset 20, old=100, new=80
LSN 110: transaction T commit
```

如果 commit record 已经持久化，但 page 7 还没刷盘，恢复时可以根据 redo 信息把 offset 20 改成 80。

redo 的目标是 durability：已提交事务不能因为数据页没及时刷盘而丢失。

**undo log**

undo 关注的是未提交修改。如果数据库允许未提交事务修改过的数据页被写回磁盘，也就是 steal 策略，那么崩溃后磁盘上可能有未提交事务的修改。undo log 保存旧值或反向操作，让恢复过程能撤销这些修改。

例如：

```text
LSN 120: transaction U update page 9, old=50, new=70
```

如果 U 没有 commit 就崩溃，而 page 9 已经被刷盘，恢复时需要把 70 改回 50，或者应用逻辑上的反向操作。

undo 的目标是 atomicity：未提交事务不能在崩溃后留下可见影响。

**redo 和 undo 的记录方式**

有些日志记录是 physical log，直接记录页号、偏移、旧值、新值。有些是 logical log，记录“插入 key K”“删除 row R”这样的逻辑操作。还有 physiological log，常见于 ARIES，页内物理、页间逻辑，兼顾恢复效率和结构变更。

常见记录可以包含：

- transaction id
- LSN
- page id
- old value
- new value
- previous LSN
- record type
- checksum

如果一个 record 同时包含 old value 和 new value，它既能支持 undo，也能支持 redo。实际系统里 redo log 和 undo log 不一定是两个物理文件，也可能是同一条 WAL record 的不同字段，或者分属不同日志体系。

**面试中的关键区别**

redo 问的是“应该做的修改有没有做完”；undo 问的是“不该留下的修改怎么撤掉”。redo 保护已提交事务，undo 清理未提交事务。WAL 系统通常同时需要二者，特别是在支持 steal/no-force 的数据库里。

## Q004. ARIES 恢复算法的大致阶段是什么？

**回答：**

ARIES 是经典的数据库恢复算法。它的恢复过程通常分成三个阶段：Analysis、Redo、Undo。

先说它背后的几个基本点。ARIES 假设数据库可以 no-force，也可以 steal：

- no-force：事务提交时不强制把数据页刷盘。
- steal：未提交事务修改过的数据页也可能被刷盘。

这两个策略性能好，但恢复复杂。no-force 要 redo 已提交但未落盘的修改，steal 要 undo 未提交但已经落盘的修改。ARIES 就是围绕这个问题设计的。

**第一阶段：Analysis**

Analysis 从最近的 checkpoint 附近开始扫描 WAL，重建崩溃时系统的大致内存状态。主要恢复两张表：

1. **Transaction Table**

   记录崩溃时哪些事务还活着，它们最后一条日志的 LSN 是多少。恢复时，这些事务通常是 loser transactions，需要 undo。

2. **Dirty Page Table**

   记录哪些数据页在崩溃前可能是脏页，以及它们第一次变脏的 LSN，也就是 recLSN。

Analysis 的结果告诉后续两个问题：

- Redo 应该从哪里开始。
- Undo 应该撤销哪些事务。

**第二阶段：Redo**

Redo 阶段的原则是 repeating history，也就是“重复历史”。它会从 Dirty Page Table 中最早的 recLSN 附近开始向前扫描 WAL，把崩溃前可能已经做过的操作重新做一遍，包括未提交事务的修改。

这听起来有点反直觉：未提交事务最后不是要撤销吗？为什么 redo 时还要重放？

原因是 ARIES 想先把数据库恢复到“崩溃瞬间的状态”，再统一 undo loser transactions。这样处理页状态、部分写入和重复崩溃会更清楚。

Redo 不是盲目重复。系统会检查 pageLSN：

```text
如果 page.pageLSN >= logRecord.LSN，说明这条修改已经在页上，不必重做。
如果 page.pageLSN < logRecord.LSN，说明页还没包含这条修改，需要 redo。
```

**第三阶段：Undo**

Undo 阶段撤销崩溃时尚未提交的事务。它通常从 loser transactions 的 lastLSN 开始，沿着每个事务的 prevLSN 反向回溯，逐条撤销。

ARIES 还有一个重要机制：CLR，Compensation Log Record。每撤销一步，系统会写一条 CLR，记录“这个 undo 已经做过”。如果恢复过程中再次崩溃，下次恢复不会重复 undo 同一个动作。

Undo 完成后，未提交事务的影响被撤销，已提交事务的影响通过 redo 保留下来。数据库回到一致状态。

**一句话总结**

ARIES 三阶段可以这样记：

- Analysis：找出崩溃时哪些事务活着，哪些页可能脏。
- Redo：从合适的 LSN 开始重复历史，把页面恢复到崩溃瞬间附近的状态。
- Undo：撤销 loser transactions，并用 CLR 保证恢复过程本身也可恢复。

这套设计的强点是能支持高性能运行时策略，同时让崩溃恢复有严格的、可重复的语义。

## Q005. checkpoint 在 WAL 系统中解决什么问题？

**回答：**

checkpoint 解决的是恢复成本问题。没有 checkpoint，数据库崩溃后可能要从 WAL 的开头一路重放到崩溃点。系统运行几天、几周后，WAL 会非常长，恢复时间不可接受。

checkpoint 的作用是建立一个较新的恢复基线：在某个点之前的数据页修改，已经以某种方式写入数据文件；崩溃恢复不必从更早的 WAL 开始。

在 WAL 系统里，checkpoint 通常会做几类事：

1. **记录恢复起点**

   checkpoint record 会写入 WAL，记录恢复需要从哪个 redo LSN 开始。这个 LSN 通常和 dirty page table 中最早的 recLSN 有关。

2. **推动脏页落盘**

   checkpoint 会把一批 dirty data pages 写回磁盘。这样，早于 checkpoint 的很多修改不再需要依赖 WAL redo。

3. **保存恢复元数据**

   某些系统会在 checkpoint 中记录 transaction table、dirty page table、active transaction 信息、最小 LSN 等。

4. **限制恢复时间**

   checkpoint 越近，崩溃后需要扫描和重放的 WAL 越少。恢复时间更可控。

5. **帮助回收 WAL 空间**

   当系统确认某些 WAL segment 早于恢复所需的最小 LSN，并且不再被复制、归档、备份需要，就可以删除或复用。

checkpoint 不是“暂停世界，把所有东西刷干净”这么简单。很多数据库使用 fuzzy checkpoint，也就是 checkpoint 期间系统仍然允许事务继续运行。这样不会长时间阻塞业务，但 checkpoint record 里必须记录足够信息，让恢复知道从哪里开始 redo。

checkpoint 的代价也不小：

- 会产生大量数据页写回。
- 可能和前台 WAL fsync 抢 I/O。
- 过于频繁会增加写放大。
- 间隔太长又会拉长恢复时间。

所以 checkpoint 是吞吐、尾延迟、磁盘空间和恢复时间之间的平衡点。它不是为了让 WAL 消失，而是为了让 WAL 的恢复范围有边界。

## Q006. checkpoint 是否可以删除所有旧 WAL？

**回答：**

不可以。checkpoint 之后只能删除“确定不再需要”的旧 WAL，而不是所有旧 WAL。

判断 WAL 能不能删，要看多个条件。最基本的是：崩溃恢复是否还需要它。如果某个 WAL segment 早于 checkpoint 的 redo 起点，而且相关数据页已经保证落盘，那么它可能不再被本地 crash recovery 需要。

但这还不够。很多系统还有其他依赖：

1. **redo 起点之前的 WAL 才可能回收**

   checkpoint record 会给出恢复需要的 redo 起点。包含 redo 起点的 WAL segment 以及之后的 segment 不能删。否则崩溃恢复会断。

2. **归档还没完成不能删**

   如果系统支持 WAL archiving 或 point-in-time recovery，旧 WAL 必须先成功归档。没有归档的 WAL 删除后，就无法恢复到对应时间点。

3. **副本还没追上不能删**

   主从复制里，如果 standby 或 replica 还需要某些 WAL，主库不能随便删除。否则副本会断流，只能重新做全量同步。

4. **replication slot 或保留策略可能阻止删除**

   某些数据库用 slot 记录下游消费进度。只要 slot 还没推进，相关 WAL 就要保留。slot 配错或下游长期挂掉，会导致 WAL 堆积甚至打满磁盘。

5. **长事务和旧快照可能间接影响回收**

   在 MVCC 系统里，长事务可能阻止清理旧版本。它不一定直接要求保留所有 WAL，但会影响 vacuum、checkpoint、归档和空间回收策略。

6. **备份基线会影响 WAL 保留**

   如果有一个基础备份，要做 PITR，就必须保留从备份开始之后的连续 WAL。缺一段就恢复不了。

因此 checkpoint 后能删除的是：

```text
早于本地恢复所需最小 LSN
并且已经归档
并且没有副本/slot/备份/恢复流程需要
的 WAL segment
```

面试里可以直接指出：checkpoint 缩短本地恢复范围，但 WAL 还可能服务于复制、归档、PITR 和增量备份。把 checkpoint 理解成“可以删除所有旧日志”的开关，是很危险的。

## Q007. WAL record 的 LSN 有什么作用？

**回答：**

LSN 是 Log Sequence Number，可以理解为 WAL 中每条记录的全局位置或全局顺序号。它是 WAL 系统里最重要的坐标之一。

LSN 的作用主要有这些：

1. **定义日志顺序**

   WAL 是 append-only 的，LSN 随日志写入单调增加。事务修改、commit、abort、checkpoint 都可以用 LSN 排序。恢复时按 LSN 顺序扫描，就能知道哪些操作先发生。

2. **建立数据页和 WAL 的关系**

   数据页通常会保存一个 pageLSN，表示“这个页已经包含到哪个 LSN 的修改”。恢复 redo 时，如果某条日志的 LSN 小于等于 pageLSN，说明这条修改已经在页面上，可以跳过。

3. **保证 write-ahead rule**

   当后台要刷某个数据页时，数据库会检查该页的 pageLSN。只有 WAL 已经 flush 到至少这个 LSN，数据页才能安全写回。否则就违反 WAL 规则。

4. **表示 durable boundary**

   数据库通常维护 flushedLSN 或 durableLSN，表示 WAL 已经持久化到哪个位置。事务 commit record 的 LSN 小于等于 durableLSN 时，事务才可以被认为本地持久提交。

5. **支持 checkpoint**

   checkpoint 会记录 redo LSN。恢复时从这个 LSN 附近开始扫描，而不是从 WAL 开头开始。

6. **支持复制**

   主从复制可以用 LSN 表示发送进度、接收进度、重放进度、确认进度。比如 primary 已发送到 LSN 1000，standby 已 replay 到 LSN 900。

7. **支持日志截断和保留判断**

   系统可以根据最小需要 LSN 判断哪些 WAL segment 可以删除或复用。复制 slot、归档、checkpoint 都会影响这个最小值。

8. **支持幂等 redo**

   redo 可能重复执行。LSN 配合 pageLSN，让 redo 变成幂等：已经应用过的修改不会重复改坏页面。

不要把 LSN 只看成“文件偏移”。很多系统的 LSN 确实可以编码成 WAL segment + offset，但它的语义更强：它表示日志全局顺序、恢复边界和持久化进度。

一句话：LSN 是 WAL 世界里的时间轴。事务提交、页面状态、checkpoint、复制和恢复，都靠它对齐。

## Q008. LSN、offset、sequence number、term 之间有什么差异？

**回答：**

这几个词都和“顺序”有关，但语义不同。混用它们会导致恢复和复制协议讲不清楚。

**LSN**

LSN 是 WAL 的日志序列号。它表示一条 WAL record 在日志流中的全局顺序或位置。数据库用它做恢复、pageLSN、flushedLSN、checkpoint redo LSN、replication progress。

LSN 的重点是恢复语义：

- 哪些日志已经持久化？
- 哪些页面包含哪些日志修改？
- redo 从哪里开始？
- 副本 replay 到哪里？

**offset**

offset 是文件内字节偏移。它更偏物理位置。例如某条 record 从 WAL 文件的第 8192 字节开始。

offset 可以用来实现 LSN，但不等于 LSN。原因是：

- WAL 可能分 segment。
- 文件可能轮转。
- LSN 可能编码 segment id + offset。
- 某些系统的逻辑 LSN 不直接暴露物理 offset。

offset 解决“字节在哪里”，LSN 解决“日志顺序和恢复边界是什么”。

**sequence number**

sequence number 是更泛化的序号。它可以用于 record 编号、请求编号、消息编号、版本编号。它不一定绑定 WAL，也不一定对应文件位置。

比如：

- 每条 append-only log record 一个递增 sequence。
- 每个客户端请求一个 request sequence。
- 每个分区内部一个 sequence。

sequence number 的语义取决于系统设计。它可能只在某个 shard 内单调，不一定全局单调；可能重启后继续，也可能只在一个 epoch 内有效。

**term**

term 常见于 Raft 这类共识协议。它表示 leader 任期。term 不是日志位置，而是领导权时代。Raft 日志通常用 `(term, index)` 判断日志新旧和冲突。

term 的核心作用是处理分布式领导权变化：

- 防止旧 leader 继续写入。
- 判断某条日志来自哪个 leader 任期。
- 选举时比较日志新旧。
- 处理 follower 与 leader 的日志冲突。

**放在一起看**

| 名称 | 主要含义 | 常见范围 | 典型用途 |
| --- | --- | --- | --- |
| LSN | WAL 日志顺序和恢复位置 | 数据库日志流 | redo、pageLSN、checkpoint、复制进度 |
| offset | 文件内物理字节位置 | 单个文件或 segment | seek、读取 record、定位字节 |
| sequence number | 通用递增编号 | 由系统定义 | record 顺序、请求去重、版本判断 |
| term | leader 任期 | 分布式共识集群 | leader fencing、日志冲突处理、选举 |

一个系统里可能同时存在这些值。比如 Raft 存储引擎可以有：

- Raft log index：共识日志位置。
- Raft term：leader 任期。
- WAL LSN：本地持久化日志位置。
- file offset：某条 WAL record 在 segment 中的位置。

面试时要强调：它们都能排序，但排序对象不同。LSN 是恢复坐标，offset 是存储坐标，sequence number 是业务或记录坐标，term 是分布式领导权坐标。

## Q009. WAL 如何处理事务的 commit record？

**回答：**

commit record 是事务从“可能提交”变成“恢复后必须视为已提交”的关键证据。WAL 系统通常把 commit record 写入日志，并在向客户端返回提交成功前，保证 commit record 已经持久化。

一个典型流程是：

1. 事务执行过程中写 update log records。
2. 用户发起 commit。
3. 数据库生成 commit record。
4. commit record append 到 WAL。
5. WAL flush 到至少 commit record 的 LSN。
6. flush 成功后，向客户端返回 commit success。
7. 后续可以写 end record 或清理事务状态。

这里最重要的是第 5 步。只有 commit record 持久化之后，恢复过程才能在日志中看到：这个事务已经提交。否则崩溃后只能把它当成未提交事务处理。

**崩溃时的几种情况**

1. **update records 在，commit record 不在**

   事务没有可靠提交证据。恢复时要把它当作 loser transaction，撤销它的影响，或者在某些 redo-only 系统中忽略未提交部分。

2. **commit record 在，但部分数据页没刷盘**

   事务已提交。恢复时 redo 它的 update records，把缺失的数据页修改补上。

3. **commit record 写入内核缓存但没持久化**

   如果机器断电后 commit record 丢失，恢复时不能把事务当成已提交。对客户端来说，如果数据库已经提前返回成功，这就是严重的持久性 bug。

4. **commit record 持久化了，但响应丢了**

   客户端可能超时重试。数据库恢复后应把事务视为已提交。上层需要 request id 或事务 id 处理重复请求。

**commit record 通常包含什么**

不同系统不同，但常见字段包括：

- transaction id
- commit LSN
- commit timestamp 或 logical time
- transaction status
- checksum
- previous LSN
- 可能还有参与分区、子事务、锁释放或可见性信息

**commit record 和数据页刷盘的关系**

commit record 持久化不要求所有数据页也立刻刷盘。这正是 WAL 的价值。已提交事务的修改可以晚点刷数据页，只要 WAL 足够恢复即可。

**group commit 中的 commit record**

多个事务可以各自写 commit record，然后共享一次 WAL flush。flush 成功后，commit record LSN 小于等于 flushedLSN 的事务都可以 ack。这样既保持了提交语义，又减少了 fsync 次数。

一句话：commit record 是恢复时判断事务是否成功提交的证据。提交成功的 ack 不能早于 commit record 的持久化。

## Q010. 为什么 group commit 能提升 WAL 写入吞吐？

**回答：**

group commit 能提升吞吐，是因为 WAL 提交路径里最贵的通常不是把几十或几百字节写进内存，而是把 WAL flush 到稳定存储。一次 fsync 或 fdatasync 可以覆盖多条 commit record，就能把同步成本摊到多个事务上。

假设没有 group commit：

```text
事务 T1 写 commit record -> fsync
事务 T2 写 commit record -> fsync
事务 T3 写 commit record -> fsync
```

如果一次 fsync 需要 2ms，那么每秒最多也就几百次提交，哪怕每个事务的日志只有几十字节。

group commit 的做法是：

```text
T1、T2、T3 的 commit record 都 append 到 WAL buffer
一次 flush 把 WAL 同步到 T3 的 LSN
T1、T2、T3 一起返回提交成功
```

这样一次同步操作提交了多个事务。吞吐上升的原因主要有几个：

1. **减少 fsync 次数**

   fsync 的固定成本很高。把 N 个事务合并成一次 fsync，平均每个事务承担的同步成本下降。

2. **顺序写更充分**

   WAL 本来就是 append-only。多个 commit record 连续写入，可以形成更大的顺序写，设备更容易合并。

3. **减少设备 flush 压力**

   每个事务都 flush 会让设备频繁打断缓存和调度。group commit 降低 flush 频率，I/O 队列更稳定。

4. **提高并发提交效率**

   一个 leader 负责 flush，其他事务作为 follower 等待同一批结果。flush 完成后，所有 LSN 不超过 flushedLSN 的事务都能完成。

5. **更适合高并发小事务**

   小事务日志很少，单独 fsync 浪费严重。并发越高，越容易在一个短窗口内收集到多个提交。

但 group commit 不是没有代价。它通常会引入一点等待时间，因为系统要给其他事务一个加入 batch 的机会。如果等待窗口太大，吞吐可能上升，但单个事务延迟也会上升；如果窗口太小，合并效果有限。

实现时有几个细节要注意：

- ack 必须发生在 flush 成功之后。
- flush 到的 LSN 必须覆盖该 batch 的所有 commit record。
- fsync 失败要通知整个 batch，不能只让 leader 看到错误。
- durableLSN 要单调推进。
- 后续事务不能越过前序未持久化 commit record 提前 ack。

所以 group commit 的本质不是降低持久性，而是合并持久化动作。它让多个事务共享一次 WAL flush，在不改变“commit record 先持久化再 ack”这个语义的前提下提高吞吐。

## Q011. WAL 的 durability 和 visibility 是否是同一个概念？

**回答：**

不是。durability 和 visibility 经常同时出现在提交路径里，但它们不是一个概念。

**durability 是持久性**

durability 关心的是：系统崩溃、重启、进程退出、机器断电后，这次修改还能不能恢复。

在 WAL 系统里，durability 通常由这些边界描述：

- WAL record 是否写入。
- commit record 是否写入。
- WAL 是否 flush 到稳定存储。
- flushedLSN 或 durableLSN 是否推进到 commit record 的 LSN。
- 底层 fsync/fdatasync 是否成功返回。

如果事务的 commit record 已经持久化，恢复过程就应该把这个事务视为已提交。即使数据页还没落盘，也可以通过 redo 补回来。

**visibility 是可见性**

visibility 关心的是：其他事务、查询、客户端、消费者现在能不能看到这个修改。

它通常由更高层的并发控制决定，例如：

- MVCC snapshot。
- transaction status。
- commit timestamp。
- isolation level。
- lock release。
- read view。
- replicated apply progress。

一个修改可以 durable，但暂时不可见。比如 prepared transaction 已经写了 WAL，但还没有最终 commit；或者主库 WAL 已经持久化，副本还没有 replay 到这个 LSN，副本读请求还看不到。

一个修改也可能可见但不 durable。比如系统使用 asynchronous commit，事务写入内存并对外可见，但 commit record 还没有真正 fsync。机器这时断电，用户曾经看到的修改可能消失。这个模式可以提高吞吐，但语义必须写清楚。

**几个常见边界**

| 概念 | 关心的问题 | 常见指标 |
| --- | --- | --- |
| writtenLSN | WAL 写到内核或 WAL buffer 到哪里 | write 进度 |
| flushedLSN | WAL 持久化到哪里 | fsync/fdatasync 进度 |
| commitLSN | 事务提交记录在哪里 | commit record |
| visibleLSN | 查询可见到哪里 | MVCC/read view/apply progress |
| appliedLSN | 数据页或副本状态应用到哪里 | redo/replay 进度 |

这几个值可能相等，也可能不等。面试时不要把“写入 WAL”“提交可见”“崩溃可恢复”“副本已应用”混成一件事。

一句话：durability 是崩溃后还能不能找回来，visibility 是当前读者能不能看见。WAL 主要解决 durability，visibility 由事务状态和并发控制决定。

## Q012. WAL 写成功但 metadata 更新失败时如何恢复？

**回答：**

先看 metadata 是什么。如果 metadata 是 index、manifest、checkpoint pointer、page table、segment list 这类派生状态，那么 WAL 写成功但 metadata 更新失败，通常应该相信 WAL，并在恢复时重建或重放 metadata。

典型场景是：

1. WAL record 已经写入并 fsync。
2. 数据或元数据的辅助结构准备更新。
3. 更新 metadata 时失败，可能是进程崩溃、rename 失败、目录 fsync 失败、磁盘满。

恢复时应该按这个原则处理：

```text
WAL 是提交事实的来源。
metadata 只有在能被 WAL 验证时才可信。
```

具体恢复流程通常是：

1. **从最近 checkpoint 或 manifest 开始**

   先找到一个已知可信的恢复起点。这个起点可能比较旧，但应该是自洽的。

2. **扫描 WAL**

   从恢复起点后的 WAL 开始顺序扫描，检查 magic、length、checksum、LSN、record type、commit record。

3. **根据 WAL 重建 metadata**

   如果 WAL 里说创建了 segment、插入了 key、更新了 page、提交了事务，就按日志重放，重新生成 index、page table、manifest 派生项。

4. **忽略未完成 metadata 更新**

   如果 metadata 文件写了一半、checksum 错、version 不匹配、指向不存在的 LSN，就丢弃它或回退到上一个版本。

5. **重新发布 metadata**

   重建完成后，用写临时文件、fsync、rename、fsync directory 的方式发布新的 metadata。

举个例子。LSM 写入时，WAL 已经记录了 memtable 中的更新，但 manifest 更新失败。恢复时可以读 WAL，把这些更新重放进 memtable，必要时重新 flush 或重新生成 manifest。旧 manifest 没记录这批更新，不代表更新丢了；只要 commit record 持久化，WAL 仍然能证明它们存在。

但有一个例外：如果 metadata 不是派生数据，而是唯一权威数据，那就不能简单丢弃。例如某个系统把唯一的 schema、唯一的加密 key version、唯一的外部对象引用只放在 metadata 里，WAL 里没有足够信息重建它。这样的设计本身就要谨慎。要么把 metadata 更新也写进 WAL，要么把 metadata 发布设计成事务性的。

所以答案可以分成两句：WAL 成功而 metadata 失败时，恢复通常从 WAL 重放，并把 metadata 当作可重建缓存；如果 metadata 是权威状态，就必须让它也受 WAL 或原子发布协议保护。

## Q013. metadata 更新成功但 WAL 写失败时为什么危险？

**回答：**

因为这违反了 WAL 的基本顺序。metadata 已经告诉系统“某个状态存在”，但 WAL 里没有足够证据解释这个状态。崩溃后，恢复逻辑不知道这个修改应该 redo、undo，还是丢弃。

危险点在于 metadata 通常会被系统当作入口或索引：

- manifest 说某个 SSTable 属于当前版本。
- index 指向某个 log offset。
- page table 记录某个 page 已经更新。
- transaction table 标记某个事务已提交。
- checkpoint pointer 指向一个新的恢复基线。

如果这些 metadata 已经持久化，而对应 WAL 没写成功或没 fsync，系统重启后会看到“新状态”，却找不到对应日志。几种坏情况会出现：

1. **无法 redo**

   metadata 引用了新数据，但 WAL 没有记录如何重放。数据页如果没写完整，恢复没办法补。

2. **无法 undo**

   未提交事务的影响可能已经通过 metadata 暴露出来，但 WAL 里没有 undo 信息，恢复无法撤销。

3. **错误可见**

   比如 commit status metadata 已经变成 committed，但 commit record 没有持久化。系统可能把一个没有提交证据的事务暴露给读者。

4. **索引指向不存在数据**

   index 更新成功，WAL 或 data record 没有写成功。查询走 index 时会读到空洞、旧数据、checksum mismatch 或越界。

5. **checkpoint 截断恢复路径**

   checkpoint metadata 成功写入，但 checkpoint 对应的 WAL 或数据页没有完成。系统可能错误地删除旧 WAL，导致真正需要恢复时日志断掉。

正确做法是让 metadata 带着 LSN，并且遵守：

```text
metadata 中引用的最大 LSN <= durableLSN
```

也就是说，metadata 只能发布已经由 WAL 持久化证明过的状态。恢复时也要校验 metadata：

- manifest 的版本是否有对应 WAL。
- index offset 是否落在 durable WAL 范围内。
- checkpoint redo LSN 是否仍有 WAL segment。
- pageLSN 是否不超过 flushed WAL。

一句话：WAL 是解释状态变化的证据。metadata 先成功、WAL 后失败，会让系统留下一个没有证据链的状态，这是 crash recovery 最怕的情况。

## Q014. WAL 与 event sourcing 的相同点和不同点是什么？

**回答：**

WAL 和 event sourcing 都使用 append-only 的日志思路，都可以通过重放日志恢复状态。但它们的目标层级不同。WAL 是存储引擎内部的恢复机制，event sourcing 是应用建模方式。

**相同点**

1. **都记录变化历史**

   WAL 记录数据页、行、索引或事务的变化。event sourcing 记录业务状态变化，例如 `OrderCreated`、`PaymentCaptured`、`UserEmailChanged`。

2. **都依赖顺序**

   日志顺序决定恢复顺序。WAL 用 LSN，event sourcing 通常用 stream version、global position、event sequence。

3. **都可以 replay**

   WAL replay 用来恢复数据库状态。event sourcing replay 用来重建聚合、投影、读模型或历史状态。

4. **都需要 snapshot/checkpoint**

   日志太长时，全量 replay 成本高。WAL 用 checkpoint 限制恢复范围；event sourcing 用 snapshot 或 projection checkpoint 降低重建成本。

5. **都要处理幂等和重复**

   崩溃、重试、重复投递都可能导致同一条记录被处理多次。系统需要 LSN、event id、version 或 dedup 机制。

**不同点**

1. **面向对象不同**

   WAL 面向数据库内部状态，常常记录 page、offset、old/new value、transaction id。event sourcing 面向业务语义，记录领域事件。

2. **读者不同**

   WAL 通常只给数据库自己读。业务代码不应该依赖 WAL 格式。event sourcing 的事件本身就是系统的业务事实，很多下游投影、审计和集成都要读它。

3. **日志格式稳定性不同**

   WAL 格式可以随存储引擎版本变化，只要数据库能升级恢复即可。event sourcing 的事件是长期业务契约，schema evolution 更难，不能随便改事件含义。

4. **恢复目标不同**

   WAL 恢复的是崩溃前一致的数据库状态。event sourcing 重建的是业务状态，还可能支持 temporal query、审计、回放修正、重新生成读模型。

5. **日志粒度不同**

   WAL 可以非常底层，例如“page 7 offset 20 从 A 改成 B”。event sourcing 应该表达业务事实，例如“订单已支付”，而不是“orders 表第 20 字节被修改”。

6. **undo 语义不同**

   WAL 可以有 undo 信息，用来撤销未提交事务。event sourcing 通常不直接修改历史事件，而是追加补偿事件，例如 `PaymentRefunded`。它更强调事实不可变。

7. **外部副作用处理不同**

   event sourcing replay 可能重新触发邮件、支付、发货等外部动作，因此需要把副作用和 replay 分开。WAL redo 通常只恢复数据库内部状态，不应该发业务外部请求。

可以这样总结：WAL 是数据库为了崩溃恢复而写的内部日志；event sourcing 是把业务事件作为系统事实来源。两者都能 replay，但一个服务存储正确性，一个服务业务建模和审计。

## Q015. WAL 与 Kafka log 的相同点和不同点是什么？

**回答：**

WAL 和 Kafka log 都是 append-only log，都追求顺序写、批量写和按 offset/LSN 读取。但 Kafka log 是分布式消息和事件流平台的存储抽象，WAL 是数据库内部的恢复日志。

**相同点**

1. **都是追加写**

   新 record 追加到日志尾部。这样写入路径更接近顺序 I/O，容易 batching，也方便用 segment 文件管理。

2. **都有位置概念**

   WAL 用 LSN。Kafka 在 partition 内用 offset。它们都用这个位置表示顺序、读取进度和恢复边界。

3. **都分 segment**

   长日志不会只放在一个大文件里。系统通常按大小或时间滚动 segment，便于删除、归档、索引和恢复。

4. **都可以 replay**

   WAL replay 用于数据库恢复。Kafka consumer 可以从旧 offset 重新消费，用于重放事件、修复下游、重建状态。

5. **都需要 retention 或 compaction**

   日志无限增长不可接受。WAL 依赖 checkpoint、归档和复制进度回收；Kafka 依赖时间/大小 retention、log compaction、消费者需求和副本进度。

**不同点**

1. **目标不同**

   WAL 目标是数据库 crash recovery。Kafka log 目标是消息持久化、发布订阅、流处理和消费者重放。

2. **可见性不同**

   WAL 通常不对业务消费者暴露。Kafka log 的核心就是让消费者按 offset 拉取消息。Kafka broker 要管理哪些消息对 consumer 可见，例如高水位线之后的消息不能暴露给普通消费者。

3. **记录内容不同**

   WAL record 可以是物理变更、逻辑变更、commit record、CLR、checkpoint record。Kafka record 通常是生产者写入的消息，broker 不理解业务语义。

4. **事务语义不同**

   WAL 直接参与数据库事务提交、redo/undo 和 pageLSN 判断。Kafka 也有事务和 exactly-once 相关能力，但它处理的是消息生产、消费和分区日志的一致性，不是数据库数据页恢复。

5. **复制模型不同**

   单机 WAL 可以不复制。Kafka log 通常按 partition 在多个 broker 上复制，有 leader、follower、ISR、高水位线、ack 策略和 leader 切换。

6. **消费者进度不同**

   WAL 的读取者通常是数据库恢复线程、复制线程或归档线程。Kafka 的消费者很多，每个 consumer group 可以有自己的消费 offset，broker 不因为某个消费者读完就立刻删除消息。

7. **删除依据不同**

   WAL 删除要保证本地恢复、备份、归档、复制都不再需要。Kafka 删除通常由 topic 的 retention、compaction、segment 状态和消费者语义决定。Kafka 不会因为所有消费者已经读过就必然删除。

一句话：WAL 是“数据库状态如何恢复”的日志；Kafka log 是“消息如何持久保存并被多个消费者读取”的日志。它们共享 append-only 和 offset 思路，但服务的系统层级不同。

## Q016. WAL 与 Raft log 的相同点和不同点是什么？

**回答：**

WAL 和 Raft log 都是有序日志，都可以重放状态。但 WAL 解决本地崩溃恢复，Raft log 解决多节点共识。这个差异很大。

**相同点**

1. **都有顺序**

   WAL 用 LSN 排序，Raft log 用 index 排序，并且每条 entry 带 term。顺序决定恢复或状态机应用的顺序。

2. **都需要持久化**

   WAL commit record 要持久化后才能承诺本地提交。Raft 节点也要持久化 current term、vote、log entries，避免重启后破坏安全性。

3. **都可以 replay**

   WAL redo/undo 用来恢复数据库页。Raft log apply 到状态机，用来让各节点得到同样状态。

4. **都需要 snapshot/checkpoint**

   日志无限增长会拖慢恢复和占用空间。WAL 用 checkpoint，Raft 用 snapshot 和 log compaction。

**不同点**

1. **核心目标不同**

   WAL 目标是单机或单副本内部的 crash recovery。Raft log 目标是让多个节点对同一串命令达成一致。

2. **提交条件不同**

   WAL 本地提交通常看 commit record 是否 flush 到本地稳定存储。Raft 的 committed 需要 leader 确认日志 entry 已复制到多数派，并满足 term 相关规则。

3. **日志冲突处理不同**

   WAL 一般是单写者或受控写入，不会出现多个 leader 产生冲突日志。Raft 必须处理旧 leader、新 leader、网络分区导致的日志分歧。它用 `(index, term)` 检查冲突，并让 follower 删除冲突后的日志。

4. **可见性边界不同**

   WAL commit 之后，数据库还要由 MVCC 或锁控制查询可见性。Raft entry committed 后，还要 apply 到 state machine，客户端通常要等命令被应用后才返回结果。

5. **故障模型不同**

   WAL 主要面对进程崩溃、机器断电、磁盘错误。Raft 面对节点宕机、网络延迟、消息丢失、重复、乱序、leader 切换和分区。

6. **日志内容不同**

   WAL entry 可能是物理数据页修改。Raft log entry 通常是状态机命令，例如 `put x=1`。状态机必须 deterministic，这样每个节点按同样顺序 apply 后得到同样结果。

7. **安全属性不同**

   WAL 关心 write-ahead rule、pageLSN、redo/undo。Raft 关心 election safety、leader append-only、log matching、leader completeness、state machine safety。

可以这样说：WAL 让一个节点崩溃后能解释自己的磁盘状态；Raft log 让多个节点在有故障和 leader 切换时仍然同意同一段历史。实际系统常常两者都用：Raft entry 先写入本地 WAL，再复制给其他节点，最终由 quorum 决定集群提交。

## Q017. 单机 WAL 和复制 WAL 的复杂度差异在哪里？

**回答：**

单机 WAL 的核心难点是本地持久化顺序和崩溃恢复。复制 WAL 在此基础上多了网络、多个副本进度、leader 切换、日志分歧和提交语义。复杂度不是线性增加，而是多出一整层协议。

**单机 WAL 要处理的问题**

- WAL record 格式。
- LSN 分配。
- WAL buffer 和 flush。
- commit record。
- pageLSN 和 write-ahead rule。
- checkpoint。
- redo/undo。
- partial write 和 checksum。
- fsync/fdatasync 错误。
- WAL segment 回收。

这些问题已经不简单，但它们大多发生在一个节点内。只要单写者顺序、持久化边界和恢复扫描正确，语义可以控制住。

**复制 WAL 多出来的问题**

1. **发送进度和接收进度**

   primary 写到 LSN 1000，不代表 replica 收到 1000，更不代表 replica 持久化或 replay 到 1000。系统需要区分 sentLSN、receivedLSN、flushedLSN、replayedLSN。

2. **提交语义**

   客户端 ack 是等 primary 本地 fsync，还是等一个副本收到，还是等多数派持久化？不同选择对应不同延迟和数据丢失窗口。

3. **副本落后**

   副本可能慢、断线、重启。主库要保留足够 WAL 让它追赶，但又不能无限保留。replication slot、保留窗口、重建副本都要设计。

4. **leader 切换**

   如果 primary 崩溃，哪个副本能提升为新 primary？它是否包含所有已 ack 的事务？如果旧 primary 复活，如何防止 split brain？

5. **日志分歧和截断**

   异步复制下，旧 leader 可能有一些未复制的 WAL。新 leader 上任后，这些记录可能要丢弃。旧节点回来时必须截断 divergent log，再从新 leader 拉取。

6. **顺序和幂等**

   网络会重复、乱序、丢包。复制协议必须让 WAL 应用保持顺序，并能安全重传。

7. **复制中的可见性**

   主库提交可见，不代表副本可见。读写分离时，要处理 read-your-writes、replica lag、stale read。

8. **备份、归档和 PITR**

   复制 WAL 还常被用于备份和时间点恢复。删除 WAL 前要考虑所有下游消费者。

9. **流控和背压**

   副本慢会拖住同步提交；如果不拖住，副本会落后。系统要在可用性、延迟和数据安全之间取舍。

**复杂度的本质**

单机 WAL 的问题是：

```text
这个节点崩溃后，我能不能恢复自己的状态？
```

复制 WAL 的问题是：

```text
多个节点对哪些 WAL 已经提交、哪些可以丢弃、谁能成为新主，有没有一致答案？
```

所以复制 WAL 必须显式定义 commit quorum、failover 规则、日志截断规则、保留规则和读可见性。只把 WAL 文件传到另一台机器，不等于有了正确的复制协议。

## Q018. append-only log 为什么适合顺序写？

**回答：**

append-only log 的写入模式天然接近顺序写：新数据总是追加到文件尾部，不需要在文件中间随机覆盖。对磁盘、SSD、文件系统和 page cache 来说，这种模式都更友好。

主要原因有这些：

1. **减少随机写**

   数据库事务可能修改很多不同数据页。如果直接刷数据页，写入位置分散。WAL 把这些修改先变成连续日志，提交路径就变成顺序 append。

2. **容易 batching**

   多条 record 可以聚合成一次 write，多个 commit record 可以共享一次 fsync。系统不需要为每个小修改做一次随机 I/O。

3. **文件系统更容易分配空间**

   顺序追加通常可以获得更连续的 extent，减少元数据更新和碎片。配合预分配 segment，效果更稳定。

4. **page cache 和 readahead 更有效**

   写入端连续产生 dirty pages，读恢复时也按顺序扫描。内核的 writeback、readahead、I/O 合并都更容易发挥作用。

5. **设备内部更容易优化**

   HDD 顺序写避免频繁寻道。SSD 和 NVMe 虽然随机性能强很多，但大块顺序写仍然有利于写合并、FTL 映射和降低写放大。

6. **锁和并发控制更简单**

   追加写可以由单 writer 分配 offset 或 LSN，其他线程把 record 放进队列。相比多线程随机更新多个位置，顺序追加更容易保证顺序。

7. **崩溃恢复边界清楚**

   append-only 文件损坏通常集中在尾部。恢复时扫描到最后一个完整 record，截断坏尾巴即可。随机覆盖文件的损坏位置更难判断。

一个典型 WAL 提交流程就是把很多“分散的数据页修改”转换成“连续的日志字节流”。这也是 WAL 能提高吞吐的根本原因之一。

但 append-only 不等于没有成本。segment 滚动、目录 fsync、索引维护、checkpoint、compaction、归档、尾部校验都还要做。它只是把热写路径变得更顺，不代表整个系统不需要清理和恢复协议。

## Q019. append-only log 为什么仍然需要 compaction？

**回答：**

append-only log 写入很快，但如果只追加不清理，空间、恢复时间和读取效率都会失控。compaction 的目标是把“历史上写过的所有变化”压缩成“现在仍然需要保留的状态或事件范围”。

常见原因有这些：

1. **旧版本太多**

   同一个 key 可能被更新很多次。append-only log 会保存每次更新，但查询当前值通常只需要最后一次有效更新。旧版本如果一直保留，会浪费空间。

2. **delete/tombstone 需要清理**

   删除在 append-only log 里通常不是原地删除，而是写一条 tombstone。tombstone 要保留一段时间，确保旧数据不会被恢复回来，但最终也要清理。

3. **恢复时间会变长**

   如果每次启动都从最早日志 replay 到现在，日志越长，恢复越慢。checkpoint、snapshot、compaction 都是在缩短恢复路径。

4. **读取会被历史拖慢**

   如果查询或扫描要跳过大量过期 record，读放大会变高。compaction 可以把有效数据重写到更紧凑的结构里。

5. **索引会膨胀**

   日志越长，offset index、key index、segment metadata 都会增长。即使数据本身还能放下，索引和 cache 也会变重。

6. **复制和备份成本上升**

   不清理日志会让新副本追赶、全量备份、校验扫描都变慢。系统可能把大量已经没有业务价值的历史反复搬运。

7. **长期审计和在线状态需求不同**

   有些日志必须长期保留用于审计；有些只需要保留当前状态。compaction 可以按 topic、表、keyspace 或时间窗口制定不同策略。

compaction 必须非常谨慎。它不是简单删除旧文件，而是要保证语义不变：

- 已提交数据不能被误删。
- 未被 checkpoint 覆盖的 WAL 不能删。
- 副本和消费者还需要的 segment 不能删。
- tombstone 要保留到足够安全的时间。
- compaction 输出的新文件要先写完、校验、fsync，再原子发布。
- 崩溃后要么看到旧版本，要么看到新版本，不能看到半 compacted 状态。

所以 append-only log 适合写入路径，compaction 适合长期运行。一个负责把写入变顺，一个负责把历史变短。

## Q020. WAL record 应该保存物理变更还是逻辑事件？

**回答：**

没有固定答案。WAL record 保存物理变更还是逻辑事件，要看它服务的目标：本地崩溃恢复、跨版本复制、审计、业务 replay、存储引擎调试，答案会不同。

**物理日志**

物理日志记录具体存储位置上的变化，例如：

```text
page_id = 7
offset = 128
old bytes = ...
new bytes = ...
```

优点：

- redo 快，直接改页。
- 结果确定，不需要重新执行复杂业务逻辑。
- 容易配合 pageLSN 判断是否已应用。
- 适合本地 crash recovery。

缺点：

- 和存储布局绑定很死。
- 跨版本兼容较难。
- 人不容易读。
- 对逻辑复制、审计和业务 replay 不友好。

**逻辑日志**

逻辑日志记录操作语义，例如：

```text
insert row into users(id=1, name='A')
delete key k
order paid
```

优点：

- 更接近业务或数据模型。
- 更适合逻辑复制、审计、跨系统消费。
- 对物理布局变化更宽容。
- 日志可能更小，尤其是一条逻辑操作影响很多物理位置时。

缺点：

- redo 时可能需要重新执行逻辑，成本更高。
- 必须保证重放确定性。
- 并发、约束、二级索引、副作用都要处理好。
- 如果依赖当前 schema，schema evolution 会变复杂。

**生理日志**

很多数据库采用中间方案，常叫 physiological logging。它通常用 page id 定位到某个页，但页内记录的是相对逻辑或局部物理操作。ARIES 就常被放在这个思路下理解。

这种方式比纯物理日志灵活，又比纯逻辑日志更容易高效 redo。

**怎么选择**

如果目标是本地数据库崩溃恢复，通常更偏物理或生理日志，因为恢复要快、确定、少依赖外部逻辑。

如果目标是业务审计、事件回放、跨服务集成，更适合逻辑事件，因为消费者需要理解事件含义。

如果目标是数据库复制，要看复制层需求：

- 物理复制：副本保持同样存储布局，恢复快，但版本和平台耦合强。
- 逻辑复制：传输行级或语义变更，更灵活，但冲突处理和 schema 兼容更复杂。

无论选哪种，WAL record 至少要满足几个条件：

- 能确定顺序，例如 LSN。
- 能校验完整性，例如 length、checksum。
- 能判断事务边界，例如 transaction id、commit record。
- 能支持幂等恢复，例如 pageLSN、record id、sequence。
- 能处理版本演进，例如 record type 和 format version。

面试里可以这样回答：WAL 如果服务存储引擎恢复，优先考虑物理或生理变更；如果服务业务回放和系统集成，优先考虑逻辑事件。不要只看日志长什么样，要先看它的读者是谁、恢复目标是什么、是否需要跨版本和跨系统。

## Q021. physical logging、logical logging、physiological logging 的区别是什么？

**回答：**

这三种 logging 的区别在于：日志记录的是“字节位置上的变化”，还是“业务/数据模型上的操作”，还是二者的折中。

**physical logging**

physical logging 记录物理位置上的变化。它关心的是某个 page、某个 offset、某段 bytes 发生了什么变化。

典型记录长这样：

```text
LSN=100
page_id=7
offset=128
old_bytes=...
new_bytes=...
```

优点是恢复非常直接。redo 时找到 page 7，在 offset 128 写入 new bytes；undo 时写回 old bytes。它不需要重新执行 SQL，不需要重新走索引查找，也不依赖当前业务逻辑。

缺点也很明显：它和物理布局绑定得很死。page size、record layout、slot directory、压缩格式、索引页结构一变，日志解释就会变困难。它适合本地 crash recovery，不太适合作为长期业务事件或跨版本逻辑复制格式。

**logical logging**

logical logging 记录逻辑操作。它不说改了哪个 page 的哪个 offset，而是说做了什么操作。

例如：

```text
insert into users(id=1, name='A')
delete key = k
transfer account A -> B amount 10
```

优点是更接近数据模型或业务语义。它对物理布局变化更宽容，更适合逻辑复制、审计、跨系统消费和 event sourcing。

缺点是 replay 更难。恢复时要重新执行逻辑操作，这要求操作必须 deterministic，还要处理约束、索引、并发、schema version、外部副作用等问题。一个逻辑操作可能影响很多物理页，恢复成本和复杂度都可能更高。

**physiological logging**

physiological logging 是折中方案。它通常用物理方式定位到一个 page，再用相对逻辑的方式描述 page 内操作。

例如：

```text
LSN=120
page_id=7
operation=insert record R into page slot
```

它不是纯物理，因为不一定记录完整 bytes diff；也不是纯逻辑，因为它指定了具体 page。ARIES 常被用来说明这种思想：页级别定位比较物理，页内操作可以有一定逻辑语义。

它的好处是：

- redo 比纯逻辑快。
- 和全局物理布局的耦合比纯 physical logging 小一些。
- 适合 B-tree、heap page 这类页式存储结构。
- 可以配合 pageLSN 做幂等恢复。

**对比表**

| 类型 | 记录内容 | 优点 | 缺点 | 常见用途 |
| --- | --- | --- | --- | --- |
| physical | page + offset + bytes | 恢复快，确定性强 | 强绑定物理格式 | 数据页 crash recovery |
| logical | SQL/命令/业务事件 | 易读，跨布局灵活 | replay 难，要求确定性 | 逻辑复制、审计、event sourcing |
| physiological | page 定位 + 页内逻辑操作 | 恢复效率和灵活性折中 | 仍依赖页结构 | ARIES 风格存储引擎 |

面试里可以这样说：physical 记录“哪里变成什么字节”，logical 记录“做了什么操作”，physiological 记录“在哪个页上做了什么页内操作”。选择哪种，要看日志的读者是恢复程序、复制系统，还是业务消费者。

## Q022. WAL 如何处理 partial record？

**回答：**

partial record 是 WAL 最常见的崩溃尾部形态：record 写到一半，机器断电；header 写完了，payload 没写完；payload 写完了，checksum 或 commit record 没写完。WAL 必须把这种尾巴识别出来，并且不能把它当作已提交数据。

常见设计是让每条 WAL record 自描述：

```text
magic
record_length
record_type
LSN
transaction_id
previous_LSN
payload
checksum
```

恢复扫描时按顺序读：

1. **读 header**

   如果连 header 都不完整，说明已经到达坏尾巴。停止扫描。

2. **检查 length**

   如果 `record_length` 小于最小 record 长度、超过单条最大长度、越过文件实际大小或 segment 边界规则，就认为后续不可信。

3. **读完整 payload**

   如果 header 声称有 4KB payload，但文件只剩 1KB，这是 partial record。恢复停在上一条完整 record。

4. **校验 checksum**

   checksum 覆盖 header 关键字段和 payload。checksum 失败时要看位置：如果失败发生在 WAL 尾部，通常按 partial/torn tail 处理；如果失败发生在中间，则更像持久化损坏。

5. **更新 last good LSN/offset**

   每成功解析一条完整 record，就推进 `last_good_offset` 或 `last_good_lsn`。遇到 partial record 后，只能恢复到这个位置。

6. **必要时 truncate**

   恢复程序可以把 WAL 文件截断到 `last_good_offset`，避免下次启动反复读到同一段坏尾巴。截断本身也要按文件系统语义处理。

不要尝试在 WAL 里“跳过几个字节继续找 magic”。这很危险。WAL 是有序日志，中间一条 record 坏了，后面的顺序和事务边界都不再可靠。只有在明确设计了 resync marker、segment-level index 或多副本校验时，才可能做更复杂的恢复。

partial record 还要和 commit record 结合看：

- update record 完整，但 commit record 不完整：事务不能算提交。
- commit record 完整并且 WAL flush 边界覆盖它：事务恢复时应视为已提交。
- commit record partial：按未提交处理，不能猜它“可能已经提交”。

一句话：WAL 处理 partial record 的原则是顺序扫描、完整校验、停在第一处不可信位置，恢复到最后一条完整可信 record。

## Q023. WAL 如何检测 torn write？

**回答：**

torn write 指一次写入只有一部分真正落盘，或者一个页面里混入了新旧两个版本。它可能发生在数据页，也可能发生在 WAL segment。WAL 系统要做两件事：检测 torn write，并确保检测到后有恢复路径。

**检测 WAL 自身的 torn write**

WAL record 通常靠这些字段检测：

1. **magic/version**

   用来确认当前位置看起来像合法 WAL record。

2. **length**

   检查 record 是否完整，是否越界，是否符合最大长度限制。

3. **LSN/offset 连续性**

   record 的 LSN 应该和扫描位置、上一条 record 的结束位置匹配。跳跃、回退、重复都可疑。

4. **checksum**

   checksum 覆盖 record header 和 payload。torn write 混入旧数据或零填充时，checksum 大概率失败。

5. **segment header 或 page header**

   有些系统把 WAL segment 分成固定 WAL page/block，每个 block 有 header、timeline、page address 等字段。恢复时可以检查 block 是否属于当前 WAL 流。

6. **zero tail 或预分配模式**

   如果 WAL segment 预先填零，恢复时遇到合法 record 后面的零区，通常可以判断为未写尾部。但不能把任意零都当成安全状态，仍要结合 length 和 checksum。

**检测数据页 torn write**

WAL 还要保护数据页。数据页如果在写盘时撕裂，只靠普通 row-level redo 可能不够，因为页上可能一半是旧版本、一半是新版本。PostgreSQL 的 `full_page_writes` 就是典型方案：checkpoint 后某个数据页第一次被修改时，把整页 image 写进 WAL。恢复时如果发现数据页可能 torn，可以用 WAL 中的 full-page image 恢复整页。

数据页也可以有 page checksum、pageLSN、page header：

- page checksum 检测页内容是否损坏。
- pageLSN 表示该页包含到哪个 WAL LSN。
- full-page image 提供恢复整页的材料。

**检测不等于修复**

检测到 WAL 尾部 torn write，通常可以截断到上一条完整 record。

检测到 WAL 中间 torn write，就严重得多。因为后续 WAL 顺序不再可信，恢复可能需要：

- 从归档 WAL 重新取该 segment。
- 从副本拉取正确 WAL。
- 使用备份恢复。
- 人工介入，避免继续启动造成二次损坏。

检测到数据页 torn write，如果 WAL 中有足够 full-page image 或 redo 信息，可以恢复；没有的话，可能只能从备份或副本修复。

所以 WAL 检测 torn write 的核心工具是 header、length、LSN 连续性、checksum 和 full-page image。checksum 负责发现问题，full-page image、归档和副本负责提供修复材料。

## Q024. WAL 是否需要 per-record checksum？

**回答：**

严肃的 WAL 设计通常需要 per-record checksum，或者至少需要等价的完整性校验机制。没有 checksum，只靠 magic 和 length 很难可靠地区分“合法 record”和“碰巧像 record 的损坏字节”。

per-record checksum 的作用主要有这些：

1. **检测 partial write**

   header 看起来完整，但 payload 只写了一部分。length 可能还在，checksum 会失败。

2. **检测 torn write**

   record 中一部分是新数据，一部分是旧数据。字段可能都能解析，但 checksum 能发现整体不一致。

3. **检测 bit flip 和 silent corruption**

   磁盘、内存、DMA、控制器、文件系统 bug 都可能导致静默损坏。checksum 能把静默损坏变成显式错误。

4. **避免误 replay**

   WAL replay 最怕把损坏 record 当成真实操作执行。checksum 是进入 replay 前的一道门。

5. **定位恢复边界**

   顺序扫描时，最后一条 checksum 通过的 record 可以成为 `last_good_lsn`。checksum 失败的 tail 可以被截断。

checksum 应该覆盖什么？

- record type
- length
- LSN 或 record position
- transaction id
- payload
- 关键 header 字段

通常 checksum 字段本身不参与 checksum，或者计算时置零。把 LSN/position 纳入校验，可以降低把一条 record 错放到另一个位置仍然通过校验的风险。

per-record checksum 也不是万能的：

- 它不能防恶意篡改，除非使用带密钥的 MAC。
- 它不能告诉你正确内容是什么，只能告诉你当前内容不可信。
- 它不能替代 fsync。
- 它不能保证业务事务语义正确。

有些系统还会做 per-block checksum、segment checksum、page checksum。per-record 和 per-block 不是互斥关系。per-record 更贴近恢复语义，per-block 更贴近 I/O 单位。两者结合，定位问题会更容易。

面试回答可以很明确：如果 WAL 是恢复依据，per-record checksum 很有必要。它的成本通常小于一次错误 replay 带来的数据损坏成本。

## Q025. 如果 WAL checksum 失败，恢复程序应该如何决策？

**回答：**

WAL checksum 失败时，恢复程序不能简单“跳过这条继续”。要先判断失败位置和语义边界。尾部失败和中间失败是两类完全不同的问题。

**第一步：判断失败位置**

1. **失败在 WAL 尾部**

   这通常是崩溃留下的 partial record 或 torn tail。恢复程序可以停止扫描，把 WAL 截断到上一条 checksum 正确、结构完整的 record。

   前提是：失败位置之后没有任何已经被系统承诺 durable 的 commit record。也就是说，不能截断已经 ack 的事务。

2. **失败在 WAL 中间**

   这通常是真正的日志损坏。不能跳过。因为 WAL 是顺序恢复依据，中间缺一条 record，后面的 pageLSN、事务状态、commit 边界都可能无法解释。

   正确处理通常是停止恢复，尝试从归档、副本或备份获取正确 WAL。

**第二步：判断是否超过 durable boundary**

如果系统记录了 durableLSN、checkpoint redo LSN、archived LSN、replication commit LSN，就要确认 checksum 失败的位置是否落在必须恢复的范围内。

- 如果失败位置在最后 durableLSN 之后，可能只是未完成尾巴。
- 如果失败位置小于等于 durableLSN，说明已经承诺持久化的日志损坏，必须报警并进入修复流程。

**第三步：检查替代来源**

可选来源包括：

- WAL archive。
- 同步副本。
- 异步副本。
- base backup + archive WAL。
- 本地镜像或 RAID。

如果能拿到同一 LSN 范围的正确 WAL，可以替换损坏 segment，再继续恢复。

**第四步：决定恢复策略**

常见策略：

1. **tail truncate**

   只适用于尾部 partial record。截断到 last good offset。

2. **restore from archive**

   适用于归档 WAL 完整的情况。

3. **fetch from replica**

   如果副本有正确 segment，可以拉取修复。

4. **fail fast**

   如果损坏在必须恢复的中间段，又没有替代来源，应停止启动。继续 replay 可能扩大损坏。

5. **人工恢复模式**

   某些系统提供强制跳过或 point-in-time recovery 到损坏前位置。这是灾难恢复手段，不能作为自动默认策略。

**第五步：记录清楚**

错误日志要包含：

- segment 文件名
- offset
- expected checksum
- actual checksum
- record LSN
- 是否在 durable range 内
- 当前 checkpoint redo LSN
- 可用归档/副本状态

一句话：WAL checksum 失败时，尾部可以保守截断，中间必须视为严重损坏。恢复程序的默认选择应是保护一致性，而不是尽量启动。

## Q026. 为什么 WAL 恢复通常要求幂等？

**回答：**

因为恢复过程可能重复执行。系统可能在正常恢复中 redo 一条已经落盘的修改，也可能在恢复到一半时再次崩溃，下一次启动又从较早的 LSN 开始。没有幂等性，重复 replay 会把数据改坏。

一个典型场景：

1. WAL record LSN 100 表示 page P 上插入一条记录。
2. 数据页 P 已经在崩溃前刷盘，pageLSN=100。
3. 重启后 redo 从 LSN 80 开始扫描。
4. 扫到 LSN 100 时，如果不判断 pageLSN，可能再次插入同一条记录。

幂等 redo 的常见方法是 pageLSN：

```text
if page.pageLSN >= record.LSN:
    skip
else:
    apply record
    page.pageLSN = record.LSN
```

这样同一条 redo record 执行多次也不会重复生效。

undo 也需要类似思想。ARIES 用 CLR 记录已经完成的 undo 操作。如果 undo 阶段崩溃，下一次恢复看到 CLR，就知道某一步已经撤销过，不会重复撤销。

WAL 恢复要求幂等，还有几个原因：

1. **checkpoint 是模糊的**

   fuzzy checkpoint 期间系统还在运行。checkpoint 后恢复从 redoLSN 开始，可能会扫到一些已经落盘的修改。

2. **数据页刷盘和 WAL 顺序不同步**

   WAL 是顺序写，数据页是后台异步写。恢复时不能假设某条 WAL 一定没应用到数据页。

3. **恢复可能被打断**

   机器可能在恢复过程中再次断电。下一次恢复要能从安全点重新来。

4. **复制 replay 可能重试**

   副本拉取 WAL 后 apply，网络或进程故障可能导致同一段 WAL 被重复发送或重复读取。

5. **用户请求可能重试**

   commit 成功但响应丢失，客户端重试。业务层也要有 request id 或事务 id 防重复。

幂等不等于所有操作天然可以重复执行。比如“余额减 10”重复执行就会错。WAL replay reducer 要么把操作设计成“设置到某个版本/值”，要么用 LSN、sequence、dedup table 判断是否已经执行。

一句话：恢复过程不能依赖“这条日志只会执行一次”。它必须能接受重复扫描、重复 redo、恢复中断后再恢复。

## Q027. WAL replay 的顺序为什么重要？

**回答：**

WAL replay 的顺序重要，是因为日志顺序本身就是系统状态变化的因果顺序。乱序 replay 会破坏事务边界、页面版本、索引一致性和可见性。

几个例子很直观：

1. **同一 page 的多次修改**

   LSN 100 把 key A 插入 page P。LSN 120 又删除 key A。按顺序 replay，最终没有 key A。反过来 replay，最终 key A 还在。

2. **事务 commit 边界**

   update record 必须在 commit record 之前解释。恢复时要先看到事务做了哪些修改，再根据 commit/abort 判断保留还是撤销。

3. **索引和数据页依赖**

   如果先 replay 索引更新，再 replay 数据页插入失败，索引可能指向不存在的记录。很多系统会通过 WAL 顺序和恢复阶段保证依赖关系。

4. **page split 和结构修改**

   B-tree split、page allocation、free list 更新都有结构依赖。后续 record 可能引用前面创建的新 page。乱序 replay 会找不到对象或破坏结构。

5. **checkpoint 和 redo 起点**

   redoLSN 之前的修改被认为已经由 checkpoint 覆盖。恢复从 redoLSN 往后顺序扫描，才能正确判断哪些页需要补。

6. **复制一致性**

   副本按 WAL 顺序 replay，才能和主库状态一致。如果副本乱序 apply，读请求可能看到主库从未出现过的状态。

顺序不一定意味着全局单线程。系统可以在满足依赖的前提下并行 replay。但逻辑顺序必须保留：

- 同一 page 的日志按 LSN 顺序。
- 同一 transaction 的日志按事务链顺序。
- 结构修改先于依赖它的修改。
- commit/apply 可见性不能早于对应数据变更。

所以 WAL replay 的顺序不是性能细节，而是恢复语义的一部分。可以优化执行方式，不能随意改变依赖顺序。

## Q028. WAL replay 能否并行化？

**回答：**

可以，但不能随便并行。WAL replay 的输入是有全局顺序的日志，恢复结果必须等价于按 LSN 顺序执行。并行化的前提是识别哪些 record 之间没有冲突，或者让并行执行仍然保持依赖顺序。

常见并行化思路：

1. **按 page 分区**

   如果 record 明确包含 page id，不同 page 的 redo 可以分给不同 worker。每个 page 内仍按 LSN 顺序 apply。

   难点是 B-tree split、page allocation、transaction status 这类操作可能跨 page。

2. **按 segment 或 LSN range 预读**

   replay 的 apply 可能要保持顺序，但读取 WAL、解析 record、校验 checksum、预取数据页可以并行。这样能减少 I/O wait。

3. **按数据分区**

   分库分表或 shard 架构里，不同 partition 的 WAL 本来就相对独立，可以并行恢复。每个 partition 内保持顺序。

4. **redo 并行，undo 串行或按事务并行**

   redo 通常更容易按 page 并行。undo 要沿事务链反向走，可能按 transaction 并行，但要小心多个事务改同一 page 的冲突。

5. **逻辑 replay 的 DAG 调度**

   如果日志记录了依赖关系，可以构建 DAG。没有依赖的操作并行，有依赖的按拓扑顺序执行。这更复杂，但适合某些 event/reducer 系统。

并行 replay 的限制：

- 同一 page 不能乱序。
- 同一 key 的更新不能乱序。
- page split、merge、allocation 要保序。
- commit visibility 不能早于它依赖的 update。
- CLR 和 undo 不能重复执行。
- 错误处理要能停止所有 worker，并报告一致的失败 LSN。

工程上常见的做法是先并行化“读”和“准备”：

- WAL 顺序扫描线程负责解析 record。
- 预取线程根据 record 预读数据页。
- worker 按 page/shard apply。
- 每个 page 有自己的 last applied LSN。
- 全局 durable/apply 进度按连续 LSN 推进，而不是按最快 worker 推进。

面试里可以这样回答：WAL replay 可以并行，但结果必须等价于顺序 replay。并行的基本单位通常是 page、partition 或无依赖的 LSN range；同一对象上的日志必须保持顺序。

## Q029. 如何设计一个可重复执行的 replay reducer？

**回答：**

replay reducer 是把日志事件重新应用到状态上的函数。可重复执行的 reducer 需要满足一个要求：同一批日志 replay 一次、两次，或者中途崩溃后从某个安全点再 replay，最终状态都应该一致。

核心设计可以从几个不变量开始。

1. **每条 record 有唯一 id**

   可以是 LSN、event id、transaction id + sequence、partition offset。reducer 要能判断某条 record 是否已经处理过。

2. **状态保存 last applied position**

   每个状态对象、page、partition 或 projection 保存 `last_applied_lsn`。replay 时：

```text
if record.lsn <= state.last_applied_lsn:
    skip
else:
    apply(record)
    state.last_applied_lsn = record.lsn
```

   这是最常见的幂等门槛。

3. **操作尽量写成 set，而不是 increment**

   `balance = 80` 比 `balance -= 20` 更容易幂等。后者重复执行会错。如果必须用增量操作，就要配合 record id 去重。

4. **外部副作用和 replay 分离**

   reducer replay 时不能重新发邮件、扣款、调用第三方 API。外部副作用应该由 outbox、effect log 或幂等下游处理。replay reducer 只重建内部状态。

5. **事务边界明确**

   reducer 不能把半个事务 apply 成可见状态。要么等 commit record 后再 publish，要么先 apply 到临时状态，再在 commit 时切换。

6. **schema version 明确**

   日志 record 要带 version。reducer 能处理旧版本 record，或者先做 migration。否则旧 WAL replay 到新代码时可能解释错。

7. **错误可停止**

   checksum 失败、未知 record type、依赖对象缺失时，reducer 应停在明确 LSN，不要跳过继续跑。跳过会制造更难排查的状态。

8. **输出状态可校验**

   每个 checkpoint 或 snapshot 可以带 checksum、record count、last applied LSN。replay 完成后校验这些值，能更早发现 reducer bug。

9. **顺序规则写清楚**

   如果 reducer 需要按 key 顺序、partition 顺序或全局 LSN 顺序执行，就要在代码和数据结构里体现。不能依赖 map iteration 这类不稳定顺序。

一个简化 reducer 可以这样设计：

```text
replay(record, state):
    validate checksum and version
    if record.lsn <= state.last_applied_lsn:
        return state
    if record.type == PUT:
        state.kv[record.key] = record.value
    if record.type == DELETE:
        state.kv.remove(record.key)
    state.last_applied_lsn = record.lsn
    return state
```

如果是事务日志，还要加上 commit table：

```text
先收集 transaction updates
看到 commit record 后再发布
看到 abort 或无 commit 的 loser transaction 就丢弃/undo
```

测试时要专门验证：

- replay 一次和两次结果相同。
- 在任意 LSN crash 后继续 replay，结果相同。
- record 乱序输入会被拒绝或正确排序。
- 重复 record 不会重复生效。
- 旧版本 record 能被解释。

一句话：可重复执行的 replay reducer，要把“是否已经应用”写进状态，把“应用动作”做成幂等，把“外部副作用”挪出 replay 路径。

## Q030. WAL 的截断、归档、保留策略如何设计？

**回答：**

WAL 生命周期策略要同时考虑本地恢复、复制、归档、备份、磁盘空间和恢复时间。不能只按“文件老了就删”处理。

可以按几个水位线设计。

1. **本地 crash recovery 水位线**

   checkpoint 会给出本地恢复需要的最小 redo LSN。早于这个 LSN 的 WAL，理论上不再被本地崩溃恢复需要。

   但这只是一个条件，不是删除许可。

2. **归档水位线**

   如果系统支持 PITR 或归档备份，WAL segment 必须先成功归档，才能从本地删除。归档命令失败时，宁可堆积 WAL，也不能假装归档成功。

3. **复制水位线**

   如果有副本，必须保留副本还没接收或还没确认的 WAL。同步复制、异步复制、replication slot、standby replay 进度都会影响这个水位线。

4. **备份水位线**

   基础备份期间或之后，需要保留从备份起点开始的连续 WAL。缺一段，备份就无法恢复到一致状态。

5. **逻辑解码/CDC 水位线**

   如果有 logical replication、CDC、订阅者消费 WAL，就要保留它们还没消费的部分。慢消费者可能导致 WAL 堆积。

最终可删除 LSN 通常取这些需求的最小值：

```text
removable_lsn = min(
    checkpoint_safe_lsn,
    archived_lsn,
    all_replica_required_lsn,
    backup_required_lsn,
    logical_consumer_required_lsn
)
```

只有早于 `removable_lsn` 的 segment 才能删除或复用。

**截断策略**

WAL 截断通常发生在尾部坏 record 或副本日志分歧时：

- 本地恢复遇到 partial tail，可以截断到 last good offset。
- Raft/Kafka 类复制日志中，follower 发现自己有 leader 不承认的尾部，要截断到共同前缀。
- 截断前要确认被截掉的部分没有对外承诺为 committed。

**归档策略**

归档要保证连续性：

- segment 完成后再归档。
- 归档成功必须有可验证结果。
- 归档文件名、timeline、segment number 不能冲突。
- 归档端最好有 checksum。
- 归档失败要报警并阻止删除。

**保留策略**

保留策略要有软硬边界：

- `min_wal_size` 或预留 segment，避免频繁创建文件。
- `max_wal_size` 作为触发 checkpoint 或清理的软目标。
- 磁盘空间硬阈值，超过后限流、暂停写入或进入只读。
- replication slot 最大保留量，防止坏掉的下游拖垮主库。
- 时间窗口，满足审计或回放需求。

**常见错误**

1. checkpoint 后立刻删除所有旧 WAL。
2. 归档命令失败但仍删除本地 WAL。
3. 忽略慢副本，导致副本断流。
4. replication slot 无上限，磁盘被 WAL 打满。
5. 只保留最新备份，不保留对应 WAL 链。
6. truncate 了已经 ack 的日志尾部。
7. 只按时间删除，不看 LSN 依赖。

一个稳妥的设计是：每个 WAL segment 都有状态机。

```text
open -> closed -> archived -> replicated/safe -> recyclable/removable
```

删除或复用只能发生在最后一个状态。这样恢复、复制和归档都能用同一套状态判断，而不是各删各的。

面试里可以这样概括：WAL 保留策略的核心不是节省空间，而是在节省空间前证明“没有任何恢复路径还需要它”。

## Q031. 为什么 WAL 可能成为系统恢复时间的瓶颈？

**回答：**

WAL 能让提交路径更快，但恢复时要把这笔账还回来。系统崩溃后，如果需要扫描、校验、读取、重放大量 WAL，恢复时间就会被 WAL 拖住。这个时间通常直接影响 RTO，也就是系统多久能重新对外服务。

WAL 成为恢复瓶颈，常见原因有这些：

1. **checkpoint 或 snapshot 太旧**

   如果最近一次 checkpoint 距离崩溃点很远，恢复程序必须从较早的 redo LSN 开始扫描。WAL 越长，扫描和 replay 越久。

2. **WAL record 数量太多**

   小事务、高频更新、每条 record 都很小，会让恢复阶段处理大量 record。即使总字节数不大，解析、checksum、事务状态判断也会消耗 CPU。

3. **redo 需要随机读取数据页**

   WAL 本身是顺序读，但 redo 可能要修改分散的数据页。恢复过程会把顺序日志变成大量数据页随机 I/O，尤其是冷启动时 page cache 为空。

4. **checksum、解压、解密成本**

   如果 WAL 开启压缩、加密、校验，恢复时每条 record 都要解析和验证。安全性和空间效率提升了，恢复 CPU 成本也会上升。

5. **恢复必须保序**

   很多日志不能随意并行 replay。同一 page、同一 key、同一事务链必须按顺序处理。顺序依赖会限制并行度。

6. **undo loser transactions 成本高**

   如果崩溃时有长事务未提交，恢复不仅要 redo，还要 undo。长事务修改范围越大，undo 越慢。

7. **归档 WAL 需要从远端拉取**

   如果本地 WAL 不完整，恢复要从对象存储、NFS、备份系统或副本拉取归档 WAL。网络延迟和下载吞吐会进入恢复时间。

8. **坏尾巴或损坏 WAL 触发人工处理**

   尾部 partial record 可以截断；中间 checksum 失败就麻烦。恢复可能要等待归档、备用副本或人工确认。

9. **重建派生结构**

   WAL replay 后还可能要重建 index、manifest、memtable、LSM metadata 或统计信息。严格说这不全是 WAL 成本，但恢复路径上用户会一起感受到。

10. **副本追赶也依赖 WAL**

   主库恢复后，副本可能还要 replay 大量 WAL。集群整体恢复时间不仅看 primary 起得多快，还看副本追上到什么位置。

优化方向通常有几类：

- 更频繁或更平滑的 checkpoint。
- 生成 snapshot，缩短 WAL replay 范围。
- 控制长事务。
- WAL record 做批量化，减少 record 数。
- 使用 pageLSN 跳过已应用 redo。
- 并行预读 WAL 和数据页。
- 把恢复时间纳入 benchmark，而不是只测正常写吞吐。

一句话：WAL 把提交时的数据页写入延后了，但恢复时要重放这些延后的修改。checkpoint、snapshot 和 replay 设计不好时，WAL 就会成为恢复时间瓶颈。

## Q032. 如何用 snapshot 缩短 WAL replay 时间？

**回答：**

snapshot 的作用是保存某个时间点的状态，让恢复不必从很早的 WAL 开始。恢复时先加载 snapshot，再从 snapshot 之后的 WAL 继续 replay。

基本模式是：

```text
snapshot covers state <= snapshot_lsn
recovery = load(snapshot) + replay WAL after snapshot_lsn
```

这样，WAL replay 范围从“从系统创建以来”缩短为“从最近 snapshot 以来”。

设计 snapshot 时要关心几个点：

1. **snapshot 必须带 LSN**

   snapshot 要记录它覆盖到哪个 LSN。没有这个值，恢复程序不知道从哪里开始 replay WAL。

2. **snapshot 内容要自洽**

   snapshot 中的数据文件、manifest、index、元数据必须属于同一个逻辑边界。不能一半来自 LSN 100，一半来自 LSN 200，却没有 WAL 修复范围。

3. **保留 snapshot 之后的连续 WAL**

   snapshot 不是单独可用的。它必须配合从 `snapshot_lsn` 之后开始的连续 WAL。中间缺一段，恢复就断了。

4. **snapshot 发布要原子**

   生成 snapshot 时通常写临时目录或临时文件，校验、fsync 后再发布 manifest。崩溃后要么看到旧 snapshot，要么看到新 snapshot，不能看到半个 snapshot。

5. **可以用 copy-on-write 或硬链接降低成本**

   LSM 系统里，SSTable 是不可变文件，做 snapshot/checkpoint 可以硬链接已有 SST 文件，再复制少量 WAL 和 manifest。RocksDB checkpoint 就是这种思路的典型例子。

6. **snapshot 太频繁也有成本**

   snapshot 会带来 I/O、空间占用、元数据管理和清理成本。频率太低恢复慢，频率太高正常写入受影响。

7. **snapshot 不能替代 WAL**

   snapshot 只覆盖它之前的状态。snapshot 生成之后的提交仍然要靠 WAL 恢复。除非能接受丢弃 snapshot 后的变更，否则 WAL 不能删太早。

一个简单恢复流程：

1. 找到最新完整 snapshot。
2. 校验 snapshot manifest、checksum、版本。
3. 读取 `snapshot_lsn`。
4. 找到从 `snapshot_lsn` 开始的连续 WAL。
5. 顺序 replay WAL 到目标 LSN。
6. 校验最终状态，并更新恢复后的 checkpoint。

snapshot 的核心价值是用空间换恢复时间。它把大量历史 replay 压缩成一次状态加载，把恢复成本限制在最近一段 WAL 上。

## Q033. WAL 与 snapshot 的一致性边界如何定义？

**回答：**

WAL 和 snapshot 的一致性边界，核心是一个 LSN：snapshot 到底包含到哪个日志位置，恢复又应该从哪个位置继续。

最常见的定义是：

```text
snapshot_lsn = snapshot 已经完整包含的最大 LSN
replay_start_lsn = snapshot_lsn 之后的第一条需要重放的 WAL
```

但现实里还要区分两类 snapshot。

**一致 snapshot**

一致 snapshot 表示：snapshot 中的状态已经包含所有 `<= snapshot_lsn` 的修改，并且不包含 `> snapshot_lsn` 的修改。加载它之后，从 `snapshot_lsn` 后继续 replay 就可以。

这种语义最清楚，但生成成本可能较高。系统可能需要短暂停写、使用 copy-on-write，或者让所有文件状态对齐到同一个边界。

**fuzzy snapshot**

fuzzy snapshot 生成期间系统仍然在写。snapshot 里可能有些页比较新，有些页比较旧。它不一定严格停在一个干净边界上。

这种情况下，snapshot 必须记录足够的 WAL 范围来修复不一致。恢复可能要从 `snapshot_start_lsn` 或 dirty page table 中的最小 recLSN 开始 redo，而不是简单从 snapshot 结束点开始。

换句话说，fuzzy snapshot 的规则是：

```text
snapshot 可以不完全一致，但必须保留足够 WAL，让恢复能把它修到一致状态。
```

**需要记录的元数据**

一个可靠 snapshot manifest 通常要包含：

- snapshot id
- snapshot format version
- start LSN
- end LSN 或 last included LSN
- replay start LSN
- 文件列表
- 每个文件大小和 checksum
- 创建时间
- 依赖的 WAL segment 范围
- 上一个 snapshot 或 base backup 信息

**一致性边界的几个常见错误**

1. snapshot 记录了 LSN，但实际文件没覆盖到该 LSN。
2. snapshot 是 fuzzy 的，却没有保留从 start LSN 开始的 WAL。
3. 删除了 snapshot 依赖的 WAL segment。
4. manifest 发布成功，但 snapshot 文件没 fsync。
5. snapshot 文件完整，但目录项或 manifest 没有持久化。
6. 恢复时从错误 LSN 开始 replay，导致漏应用或重复应用。

WAL 和 snapshot 的关系可以这样讲：snapshot 是某个 LSN 附近的状态压缩，WAL 是从这个边界继续演进状态的历史。二者中间不能有空洞。

## Q034. WAL 与双写问题有什么关系？

**回答：**

“双写问题”有两个常见语境。一个是存储引擎内部同时写 WAL 和数据页，另一个是应用同时写数据库和消息系统。WAL 和这两类问题都有关系，但解决范围不同。

**存储引擎内部的双写**

数据库运行时通常会写两份东西：

- WAL：记录修改历史。
- 数据页或 metadata：保存最终状态。

这看起来像双写，但 WAL 给它加了顺序和恢复语义：

```text
先保证 WAL 持久化
再允许数据页或 metadata 持久化
恢复时用 WAL 修复不一致
```

如果数据页写了但 WAL 没写，这很危险，因为恢复无法解释数据页变化。WAL 的 write-ahead rule 就是为了避免这种内部双写失序。

**应用层的双写**

应用层双写通常是：

```text
写数据库
发 Kafka 消息
```

或者：

```text
写本地状态
调用外部服务
```

这不是 WAL 自动能解决的问题。数据库自己的 WAL 只能保证数据库内部状态可恢复，不能保证 Kafka 消息也一起提交。如果数据库提交成功后进程崩溃，消息可能没发出去；如果消息发出后数据库提交失败，下游可能看到一条不存在的业务状态。

常见解决方法：

1. **transactional outbox**

   把业务数据和 outbox event 写进同一个数据库事务。事务提交后，通过 CDC 或后台 worker 从 outbox 表发送消息。这样至少数据库状态和“待发送消息”在一个事务里。

2. **CDC 读取 WAL/binlog**

   下游直接从数据库日志或变更流中读取提交后的变更。这样消息来源是数据库提交日志，而不是应用手写两次。

3. **两阶段提交**

   用 2PC 协调数据库和消息系统。语义强，但复杂度和可用性成本高。

4. **幂等和补偿**

   如果不能做原子提交，就用 request id、event id、去重表、补偿任务降低风险。

所以 WAL 对双写问题的关系是：它能解决数据库内部“日志和数据页”之间的可恢复顺序；它不能单独解决跨系统双写。应用层双写要靠 outbox、CDC、2PC 或幂等补偿。

## Q035. 为什么 doublewrite buffer 能解决部分 torn page 问题？

**回答：**

doublewrite buffer 解决的是数据页写入时的 torn page 风险。它的思路很朴素：把数据页写到最终位置之前，先写到一个安全的中间区域。这样如果最终位置写坏了，恢复时还能从中间区域找到完整页。

以 InnoDB 为例，数据页不是直接从 buffer pool 写到表空间最终位置，而是先写到 doublewrite buffer。大致流程是：

1. 把一批 dirty pages 写入 doublewrite buffer。
2. 确保 doublewrite buffer 中的页持久化。
3. 再把这些页写到各自最终的数据文件位置。

如果第 3 步写最终位置时发生 torn write，例如 16KB 页只写了一半，崩溃后数据文件里的页可能坏了。恢复时 InnoDB 可以检查页 checksum，如果发现最终位置的页损坏，就从 doublewrite buffer 中拿到完整副本，先修复数据页，再继续用 redo log 做恢复。

它能解决的是：

- 数据页写到最终位置时只完成一部分。
- 最终位置混入新旧页内容。
- 数据页 checksum 失败，但 doublewrite buffer 里有完整副本。

它不能解决：

- WAL/redo log 自身损坏。
- doublewrite buffer 和最终页都损坏。
- 写入顺序违反 WAL 规则。
- 逻辑错误，例如程序写错值。
- 恶意篡改。
- 磁盘丢失整个文件或整个设备。

doublewrite buffer 和 WAL 的关系是互补的：

- WAL/redo log 记录“怎么把页恢复到目标 LSN”。
- doublewrite buffer 保证“基础页不是半坏的”。

如果页已经 torn 到无法正确应用 redo，redo log 也很难救。doublewrite buffer 先把页修回完整页，再让 redo 继续前进。

代价是写放大。数据页要先写 doublewrite 区，再写最终位置。但 doublewrite 区通常可以批量顺序写，成本比随机双倍写要低一些。它不是免费功能，但对防 torn page 很有价值。

## Q036. WAL 在 PostgreSQL、MySQL InnoDB、RocksDB、SQLite 中有什么典型差异？

**回答：**

这些系统都用了 WAL 或类似 WAL 的机制，但目标和实现差别很大。面试时不要只说“都有日志”，要说清楚它们服务的存储结构。

**PostgreSQL**

PostgreSQL 的 WAL 服务于整个数据库集群的 crash recovery、复制、归档和 PITR。它记录 heap、index、事务状态、checkpoint 等变更。数据页有 pageLSN，恢复时可以根据 pageLSN 判断 redo 是否需要应用。

典型特点：

- WAL 是数据库恢复的核心日志。
- 支持 archiving 和 point-in-time recovery。
- 支持 streaming replication。
- `full_page_writes` 用 full-page image 降低 torn page 风险。
- `synchronous_commit`、`wal_level`、checkpoint 参数会影响持久性、复制和性能。

PostgreSQL 的 WAL 不只是本地恢复文件，也是复制和备份体系的一部分。

**MySQL InnoDB**

InnoDB 的 redo log 是磁盘上的数据结构，用于 crash recovery。它记录对数据页的 redo 信息。InnoDB 还有 undo log，用于事务回滚和 MVCC。MySQL 层还有 binary log，用于复制和 point-in-time recovery，两者不是一回事。

典型特点：

- redo log 保护 InnoDB 数据页恢复。
- undo log 支持 rollback 和一致性读。
- doublewrite buffer 处理部分 torn page 风险。
- binlog 主要服务复制、审计和恢复到时间点。
- redo log 和 binlog 的一致性涉及两阶段提交。

所以在 MySQL 里，说 WAL 时要问清楚是在说 InnoDB redo log，还是 MySQL binlog。

**RocksDB**

RocksDB 是 LSM-tree 存储引擎。写入通常先进入 WAL，再进入 memtable。memtable flush 成 SST 文件后，相关 WAL 可以被删除。RocksDB 的不可变 SST 文件和 manifest/checkpoint 机制，让它的 WAL 生命周期和 LSM flush/compaction 强相关。

典型特点：

- WAL 保护 memtable 中尚未 flush 到 SST 的写入。
- SST 是不可变文件，flush 后可以减少对旧 WAL 的依赖。
- 可以配置 disableWAL，但会牺牲崩溃恢复能力。
- Checkpoint 可以基于硬链接 SST 和复制少量日志生成一致快照。
- 多 column family 场景下 WAL 生命周期可能被最慢 flush 的 column family 影响。

RocksDB 的 WAL 更像“memtable 的持久化保护层”，不是关系数据库里那种覆盖所有事务语义的完整日志系统。

**SQLite**

SQLite 的 WAL mode 是一种事务日志模式。修改先进入 `-wal` 文件，读者可以继续读主数据库文件。checkpoint 时，WAL 中的变更被搬回主数据库文件。

典型特点：

- WAL 文件和数据库文件并存。
- commit 通过 WAL 中的提交记录体现。
- 多个 reader 可以和一个 writer 并发。
- reader 看到自己的 end mark，避免被后续写入影响。
- checkpoint 把 WAL 内容回写到数据库文件。
- WAL mode 通常不适合网络文件系统，因为需要共享内存 wal-index 等机制。

SQLite 的 WAL 很适合嵌入式单文件数据库的并发读写优化，但它的部署边界和服务器数据库不同。

**简表**

| 系统 | WAL 主要保护什么 | 典型特色 |
| --- | --- | --- |
| PostgreSQL | 数据库集群恢复、复制、PITR | WAL archiving、streaming replication、full_page_writes |
| InnoDB | 数据页 crash recovery | redo/undo 分工、doublewrite buffer、binlog 另算 |
| RocksDB | memtable 未 flush 的写入 | LSM、SST、manifest、checkpoint |
| SQLite | 单文件数据库 WAL mode | reader/writer 并发、checkpoint 回写 DB 文件 |

一句话：PostgreSQL 的 WAL 是数据库级恢复和复制骨架；InnoDB redo log 主要保护页恢复；RocksDB WAL 保护 memtable；SQLite WAL 是嵌入式数据库的事务日志模式。

## Q037. WAL 写入路径如何影响 p99 latency？

**回答：**

WAL 写入路径通常在事务提交的热路径上。平均延迟可能很好看，但 p99 很容易被 fsync、group commit、锁竞争、segment 切换、归档和复制放大。

影响 p99 的点有这些：

1. **WAL buffer 锁**

   多线程提交时要分配 LSN、写 WAL buffer、更新事务状态。全局锁或热点锁竞争会直接增加排队时间。

2. **WAL flush/fdatasync**

   提交必须等待 WAL flush 时，fsync 的长尾会传到事务 p99。云盘、NFS、SSD GC、设备队列都会放大这个长尾。

3. **group commit 等待窗口**

   group commit 提高吞吐，但请求可能要等 batch leader。等待窗口过大，p99 上升；窗口过小，fsync 次数多，吞吐下降。

4. **checkpoint 干扰**

   checkpoint 会写大量脏数据页，和前台 WAL flush 抢 I/O。checkpoint 设置不平滑时，提交延迟会周期性抖动。

5. **WAL segment switch**

   WAL 文件切换可能涉及创建新 segment、初始化、fsync 目录、归档旧 segment。segment 切换时的少数请求可能成为 p99。

6. **WAL archiving**

   如果归档逻辑和 WAL 回收、磁盘空间强相关，归档慢会造成 WAL 堆积。磁盘接近满时，提交路径可能被迫等待或失败。

7. **同步复制**

   如果提交需要等副本确认，p99 会包含网络 RTT、副本写 WAL、副本 fsync、replica 负载和 failover 状态。

8. **WAL 压缩、加密、checksum**

   单条 record 成本可能不大，但高并发下 CPU、内存拷贝和缓存 miss 会进入提交路径。加密还可能受密钥管理和 nonce 分配影响。

9. **长事务和大事务**

   大事务生成大量 WAL，commit 时可能需要 flush 很长一段日志。它会拖慢自己，也可能拖慢同批 group commit 中的其他事务。

10. **错误和重试**

   EIO、ENOSPC、归档失败、复制超时、leader 切换都会让少数请求非常慢，甚至失败。

排查 WAL p99 时，要拆开看：

- 等待 WAL lock 的时间。
- 写 WAL buffer 的时间。
- flush 等待时间。
- group commit batch size。
- bytes per flush。
- checkpoint 当前进度。
- WAL segment switch 次数。
- archive 延迟。
- replica flush/replay 延迟。
- block device await 和 queue depth。

一句话：WAL p99 不只是“磁盘慢”。它是提交队列、同步策略、文件系统、设备、归档和复制共同形成的长尾。

## Q038. WAL 如何支持 point-in-time recovery？

**回答：**

Point-in-time recovery，简称 PITR，靠的是基础备份加连续 WAL。基础备份提供某个时间点附近的数据文件，WAL 提供从备份开始之后的所有变更历史。恢复时先还原基础备份，再按 WAL 顺序 replay 到目标时间、目标 LSN 或目标事务。

基本流程是：

1. **做 base backup**

   备份数据文件，同时记录备份开始和结束相关的 WAL 位置。备份过程中数据库仍然可以写入，所以必须保留从备份开始起的 WAL。

2. **持续归档 WAL**

   每个 WAL segment 完成后复制到归档存储。归档必须连续，不能中间缺一段。

3. **发生故障或误操作**

   比如误删表、误更新数据、磁盘损坏。

4. **还原 base backup**

   把数据文件恢复到新目录或新机器。

5. **按顺序取回 WAL**

   从归档中取出备份之后的 WAL segment。

6. **replay 到目标点**

   恢复程序重放 WAL，直到指定时间、LSN、事务 id、restore point 或其他目标。

7. **打开数据库**

   到达目标点后停止恢复，并生成新的时间线或恢复状态。

PITR 的关键要求：

- base backup 和 WAL 链必须匹配。
- WAL 必须连续。
- 归档文件不能损坏。
- 时间线信息要正确。
- 恢复目标要明确。
- 不能提前删除备份需要的 WAL。

PITR 能解决的典型问题：

- 恢复到误操作之前。
- 从基础备份恢复到最近状态。
- 搭建 standby。
- 做审计或故障分析。

它不能解决所有问题。如果误操作发生后很久才发现，而旧 WAL 已经删除，PITR 就到不了那个点。如果应用写入了错误但又被其他正确写入依赖，恢复到某个时间点也可能带来业务补偿问题。

一句话：WAL 支持 PITR 的原因是它保存了基础备份之后的连续状态变化。base backup 是起点，WAL 是从起点走到目标点的路径。

## Q039. WAL 归档失败会对数据库造成什么风险？

**回答：**

WAL 归档失败不是“小告警”。如果系统依赖 WAL 做 PITR、备份或复制，归档失败会同时带来恢复风险和运行风险。

主要风险有这些：

1. **PITR 链断掉**

   如果某个 WAL segment 没有成功归档，而本地又被删除，基础备份之后的 WAL 链就断了。恢复只能到缺失段之前，不能恢复到更晚时间点。

2. **备份失效**

   基础备份本身可能是不够的。没有从备份开始之后的连续 WAL，备份无法恢复到一致状态，或者无法恢复到期望时间点。

3. **本地 WAL 堆积**

   可靠数据库通常不会在归档失败时删除本地 WAL。归档一直失败，WAL segment 会在本地堆积，最终可能打满磁盘。

4. **磁盘满导致数据库停写**

   WAL 所在磁盘满了，数据库无法继续写 WAL。很多系统会停止写事务，甚至进入不可用状态。相比悄悄丢 WAL，停写反而是更安全的行为。

5. **副本追赶受影响**

   如果副本需要从归档 WAL 补缺口，归档失败会导致副本无法追上，只能重新初始化。

6. **误操作恢复窗口缩短**

   归档失败期间的 WAL 不可靠，意味着这段时间内发生误删、误更新时，恢复选项减少。

7. **监控误判**

   数据库正常处理请求，不代表归档正常。归档失败可能在业务无感的情况下持续几个小时，直到磁盘被 WAL 打满才暴露。

8. **合规和审计风险**

   如果 WAL 归档用于审计或法规保留，归档缺口就是证据链缺口。

处理策略：

- 归档命令必须返回准确状态。
- 归档失败要报警。
- 归档延迟要有指标。
- 本地 WAL 目录要有容量水位线。
- 不要在归档失败时强行删除 WAL。
- 定期做恢复演练，验证归档可用。
- 对归档对象做 checksum 或大小校验。
- 监控最老未归档 segment。

一句话：WAL 归档失败会把“以后能恢复”这个承诺变成未知数，还可能因为 WAL 堆积把当前数据库拖停。

## Q040. WAL 压缩、加密、校验的顺序应该如何考虑？

**回答：**

WAL record 如果同时需要压缩、加密和校验，顺序要仔细设计。顺序错了，会导致压缩效果差、校验覆盖范围不对，或者恢复时无法判断损坏发生在哪里。

一个常见推荐顺序是：

```text
serialize -> compress -> encrypt/authenticate -> write
```

如果还需要非密码学 checksum，可以在明文逻辑层和存储层各有不同用途。

**为什么先压缩再加密**

加密后的数据看起来接近随机，几乎不可压缩。如果先加密再压缩，压缩率很差，还浪费 CPU。先压缩再加密，是大多数系统更自然的选择。

**checksum 放哪里**

要看 checksum 的目标。

1. **明文 checksum**

   对序列化后的逻辑 payload 或压缩前内容做 checksum，可以检测解密解压后数据是否符合 WAL record 语义。它更接近“逻辑内容校验”。

2. **密文 checksum 或 AEAD tag**

   对加密后的 bytes 做认证，可以在解密前发现篡改或损坏。更推荐用 AEAD，例如 AES-GCM、ChaCha20-Poly1305，让加密和认证绑定。

3. **存储层 checksum**

   对最终写入的 record bytes 或 block bytes 做 checksum，可以快速检测 torn write、bit flip、错位写入。它不一定替代 AEAD，因为 CRC32C 不防恶意篡改。

**header 怎么处理**

WAL header 里通常有：

- magic
- version
- record type
- LSN
- compressed length
- original length
- encryption algorithm id
- key id
- nonce/IV
- checksum/tag

有些 header 字段需要明文保存，否则恢复程序不知道怎么解密和解压。但这些明文字段也要被认证，不能让攻击者或损坏数据修改 length、LSN、algorithm id 后仍然通过校验。

**一种较稳的 record 结构**

```text
plaintext_payload = serialize(record)
compressed_payload = compress(plaintext_payload)
ciphertext, auth_tag = AEAD_encrypt(
    key,
    nonce,
    compressed_payload,
    associated_data = header_without_tag
)
storage_checksum = crc32c(header + ciphertext + auth_tag)
```

这里：

- compression 发生在 encryption 前。
- AEAD tag 防篡改。
- associated data 保护 header。
- storage checksum 快速发现传输/存储损坏。
- header 记录原始长度和压缩长度，防止解压越界。

**恢复时的顺序**

恢复读取时反过来：

1. 检查 magic、length、基本边界。
2. 检查 storage checksum。
3. 用 header 中的 key id 和 nonce 做 AEAD 验证和解密。
4. 解压。
5. 解析 WAL record。
6. 检查 LSN、type、transaction id、commit 边界。

**常见错误**

1. 先加密再压缩。
2. checksum 不覆盖 length 和 LSN。
3. 加密了 payload，但 header 没认证。
4. 重复使用 nonce。
5. 用 CRC 当作安全认证。
6. 解压前不检查长度，导致压缩炸弹或内存分配过大。
7. key rotation 后旧 WAL 无法恢复。
8. 只校验 segment，不校验单条 record，定位困难。

一句话：压缩为节省空间，加密为保密和认证，checksum 为发现损坏。推荐先压缩再加密认证，再对最终 bytes 做存储校验；同时把 header、长度、LSN、算法版本都纳入保护范围。

## Q041. WAL 是否适合作为审计日志？

**回答：**

WAL 可以辅助审计，但通常不适合直接当作审计日志。原因是 WAL 的第一目标是恢复，不是给人、合规系统或业务审计工具长期阅读。

WAL 适合作为审计线索的地方：

1. **能证明某些变更曾经发生**

   WAL 里有 LSN、事务边界、时间附近的信息、修改内容或页级变化。调查事故时，它可以帮助判断某个变更是否写入、是否提交、是否复制。

2. **顺序性强**

   WAL 是 append-only，LSN 单调递增。它天然能提供“先后顺序”的证据。

3. **和恢复路径一致**

   如果数据库能靠 WAL 恢复，说明 WAL 至少包含恢复所需的事实。对数据损坏、误操作、提交时序排查很有价值。

4. **可以作为 CDC 或逻辑审计的来源**

   许多系统不是直接让审计系统读原始 WAL，而是通过逻辑解码、binlog、CDC pipeline，把提交后的变更转换成稳定的审计事件。

但直接把 WAL 当审计日志有很多问题：

1. **格式内部化**

   WAL 格式服务存储引擎，可能随版本变化。审计日志通常需要长期稳定、可解释、可导出。

2. **语义太底层**

   WAL 可能记录 page、offset、bytes、redo record。审计需要的是“谁在什么时间对哪个业务对象做了什么”。两者不是同一层。

3. **可能包含未提交事务**

   WAL 中有 update record 不代表事务最终提交。审计系统如果不理解 commit/abort，就会把未提交修改误报成真实业务行为。

4. **可能包含敏感数据**

   WAL 为了恢复，可能保存旧值、新值、整页镜像、索引项。里面的数据范围往往比业务审计需要的更大。

5. **保留策略冲突**

   WAL 通常为了恢复和复制保留一段时间；审计日志可能要求更长保留。把 WAL 当审计日志会导致存储膨胀和隐私风险。

6. **访问控制不匹配**

   运维人员可能需要访问 WAL 排查恢复问题，但不应该因此看到所有用户敏感字段。审计系统需要更细粒度的权限和脱敏。

更稳妥的设计是：

- WAL 保留为恢复日志。
- 从 WAL、binlog 或事务提交路径派生审计事件。
- 审计事件使用稳定 schema。
- 审计事件只包含必要字段。
- 敏感字段做脱敏、哈希、tokenization 或引用化。
- 审计日志单独设置保留、加密、访问控制和不可篡改机制。

一句话：WAL 可以作为审计的原始证据或 CDC 来源，但不应该默认替代正式审计日志。恢复日志和审计日志的读者、保留周期、字段语义和权限模型都不同。

## Q042. WAL 的安全删除和合规保留有什么矛盾？

**回答：**

矛盾在于：安全删除希望数据尽快、彻底、不可恢复；合规保留希望数据在规定周期内完整、可查、不可篡改。WAL 又偏偏可能包含大量历史数据、旧值、新值和整页镜像，所以两边都会盯上它。

常见冲突有这些：

1. **删除请求 vs 恢复需要**

   用户要求删除某些个人数据，但 WAL 里可能还有旧值。数据库为了 PITR、复制、备份，需要保留这些 WAL 一段时间。立即物理删除单个 WAL record 通常不可行，因为会破坏日志连续性。

2. **最小化保留 vs 审计保留**

   隐私原则通常要求少收集、少保留。审计、财务、风控、安全调查可能要求保留足够长时间。WAL 如果含用户数据，就夹在中间。

3. **不可篡改 vs 可删除**

   审计和取证希望日志不可篡改。隐私删除希望某些数据能被清除。二者在设计上天然冲突。

4. **备份和归档更难删除**

   WAL 不只在本地，还可能在归档存储、冷备份、对象存储、灾备中心、测试恢复环境里。删除一个用户数据不等于只删主库。

5. **加密密钥生命周期**

   如果 WAL 加密，安全删除可以通过销毁相关密钥实现“加密删除”。但合规保留又要求在保留期内密钥可用，否则日志无法审计或恢复。

6. **恢复点和删除点冲突**

   如果做 PITR 恢复到删除请求之前，删除过的数据可能又出现。系统必须有恢复后的再删除流程，或者明确恢复环境的访问限制。

工程上常见做法：

- **分层保留**：在线 WAL 保留短周期，归档 WAL 按备份/PITR 需求保留，审计日志另行长期保留。
- **字段最小化**：不要把不必要的敏感字段写进业务审计事件；WAL 无法避免时，尽量缩短归档周期。
- **加密和密钥分域**：WAL、备份、审计日志使用独立密钥；按租户、时间窗口或数据域做密钥分割。
- **删除台账**：记录删除请求和完成状态。恢复旧备份后，要能重放删除请求。
- **访问隔离**：能访问 WAL 归档的人，不等于能看明文用户数据。
- **保留策略文档化**：明确 WAL、归档、备份、审计日志分别保留多久，为什么保留。
- **恢复演练包含隐私步骤**：不仅验证能恢复，还要验证恢复后如何重新应用删除或屏蔽策略。

所以答案不是“WAL 永远不能保留”或“为了合规永远不删”。真正可行的是按数据类型、恢复目标和法规要求设计保留层级，并把加密、密钥销毁、访问控制和恢复后再删除流程写进系统。

## Q043. 如果 WAL 记录包含用户数据，如何处理隐私与调试需求？

**回答：**

WAL 里包含用户数据很常见。只要 WAL 记录行变更、旧值、新值、整页镜像、索引项，就可能包含姓名、手机号、地址、token、订单、聊天内容等敏感信息。问题不是“能不能完全避免”，而是怎么把暴露面控制住。

可以从几个层面处理。

1. **减少写入敏感数据**

   能不写进 WAL 的敏感信息，不要写。比如调试字段、冗余明文、完整请求体、外部 token，不应该随手塞进事务日志。WAL 需要的是恢复所需信息，不是完整业务上下文。

2. **字段级保护**

   对敏感字段做加密、tokenization、哈希或引用化。WAL 里即使出现，也尽量不是可直接读的明文。

   但要注意：如果数据库页里存的就是明文，WAL 的 page image 或 redo record 也可能带出明文。只在日志层脱敏不一定够。

3. **全量 WAL 加密**

   WAL 文件落盘、归档、传输都应该加密。加密要配合密钥管理、轮换、访问审计和恢复演练。不能只说“加密了”，结果恢复时找不到旧密钥。

4. **限制访问**

   WAL 归档不是普通日志文件。能读取 WAL 的人，可能看到比应用日志更多的敏感数据。访问要分角色、分环境、分审批，并记录审计。

5. **调试用派生日志**

   排查问题时，不一定要直接看原始 WAL。可以生成脱敏后的 logical change log、trace summary、LSN 范围统计、record type 分布、checksum 结果、事务状态摘要。

6. **最小化导出**

   给开发或供应商排查时，不要复制整段 WAL。优先导出最小 LSN 范围、最小表/分区、脱敏样本，或者在受控环境里让他们查看结果而不是拿走文件。

7. **恢复环境隔离**

   从 WAL 恢复出来的环境也包含真实数据。测试恢复、事故分析、离线调试环境要有同样的权限控制，不能因为是“临时环境”就放松。

8. **保留周期区分**

   调试需要通常是短期的；审计或 PITR 可能是中长期的。不要为了“以后也许要排查”无限期保留明文 WAL。

9. **删除请求处理**

   如果用户数据已进入 WAL、归档和备份，要有清楚策略：保留期到期删除，密钥销毁，恢复旧备份后重放删除请求，或者在恢复环境中屏蔽。

10. **日志工具安全**

   `wal_dump`、binlog parser、CDC 工具、内部诊断工具都要按敏感数据工具管理。输出默认脱敏，明文模式需要显式授权。

一个好设计会把调试需求拆开：

- 调恢复正确性：看 LSN、checksum、record type、事务边界，不一定看用户值。
- 调业务错误：看脱敏后的逻辑事件，必要时按审批查看明文字段。
- 调安全事件：保留不可篡改审计链，但限制明文访问。

一句话：WAL 里的用户数据要按生产数据处理。调试需要不能成为绕过隐私边界的理由。

## Q044. WAL 格式升级如何保证老版本可恢复？

**回答：**

WAL 格式升级最怕两件事：新版本写了旧版本看不懂的日志，旧版本回滚后无法恢复；或者新版本读旧 WAL 时解释错字段。保证老版本可恢复，要先定义升级和回滚边界。

常见设计原则：

1. **record 带 format version**

   每条 WAL record 或每个 segment header 都要有版本号。恢复程序先看版本，再选择解析逻辑。不能靠 record 长度或 type 猜格式。

2. **record type 稳定分配**

   老 type 不要改变语义。新增操作使用新 type 或新版本字段。复用旧编号会让老恢复程序把新 record 当成旧语义执行，后果很危险。

3. **向后读取**

   新版本通常必须能读取旧版本 WAL。否则升级后只要崩溃，就可能无法 replay 升级前留下的日志。

4. **向前兼容要谨慎**

   旧版本是否需要读新 WAL，取决于是否支持 downgrade。如果支持回滚，就不能让新版本立即写旧版本完全不认识的 WAL，除非先进入不可回滚阶段。

5. **升级 barrier**

   在写新格式 WAL 前，可以要求：

   - checkpoint 完成。
   - 旧 WAL 已经不再需要。
   - 所有数据页已达到某个版本边界。
   - manifest 记录 storage format version。

   这样旧格式恢复和新格式恢复有明确分界。

6. **feature flag**

   新格式不要随二进制启动就自动写。用 feature flag 或 catalog/storage version 控制。只有集群所有节点、备份工具、恢复工具都支持后，才启用。

7. **工具链同步**

   `wal_dump`、备份工具、归档校验、复制节点、CDC parser 都要支持新格式。只升级数据库主进程不够。

8. **未知字段可跳过**

   如果 record 是 TLV 或 length-delimited，可以让新字段被旧解析器跳过。但这只适合非关键字段。影响恢复语义的字段不能被旧版本无声跳过。

9. **未知 type 默认失败**

   对恢复程序来说，遇到未知关键 record type，默认应该 fail fast，而不是跳过。跳过可能让数据库启动到错误状态。

10. **升级前后做恢复测试**

   测试矩阵至少包括：

   - 旧版本写 WAL，新版本恢复。
   - 新版本写旧格式 WAL，旧版本恢复。
   - 开启新格式后崩溃，新版本恢复。
   - 升级中途崩溃。
   - 备份恢复到升级前/升级后 LSN。

实际系统通常会把“二进制版本升级”和“存储格式升级”分开。先部署能读新旧格式的二进制，再切换存储格式。一旦写了不可回滚的新 WAL，就要明确禁止回滚到旧版本。

一句话：WAL 格式升级要靠版本号、兼容解析、升级 barrier、feature flag 和恢复测试保证。恢复程序宁愿拒绝未知关键日志，也不能猜着 replay。

## Q045. WAL replay 出现未知事件类型时应该失败、跳过还是降级？

**回答：**

默认应该失败，特别是这个事件类型可能影响数据状态、事务边界、索引结构或恢复进度时。WAL replay 是恢复数据库一致性的过程，不是普通消息消费。跳过未知事件，往往比启动失败更危险。

可以按事件类型分级。

**必须失败的情况**

遇到下面这些未知 record type，应该停止恢复：

- 修改数据页。
- 修改索引结构。
- 修改事务状态。
- commit、abort、prepare。
- checkpoint。
- manifest 或 metadata 变更。
- page allocation/free。
- undo/CLR。
- 格式升级 marker。

原因很简单：恢复程序不知道这条 record 做了什么，就无法保证后续状态正确。跳过它继续 replay，可能造成数据缺失、索引错乱、事务可见性错误。

**可以跳过的情况**

只有在格式明确设计为可跳过时才可以跳过，例如：

- record 带 length。
- record 标记为 optional。
- 文档说明旧版本可忽略。
- 该 record 不影响恢复语义。
- checksum、LSN 连续性仍然可验证。

比如统计信息、调试 annotation、性能 hint、非关键 trace marker，可能可以跳过。

**可以降级的情况**

降级不是“忽略”，而是有明确替代语义。例如：

- 新 record 包含 full payload，同时提供旧字段。
- 新压缩算法不可用时，可以读取未压缩 fallback。
- 新索引 hint 不认识时，可以回退到全量扫描重建。

降级必须由格式设计支持，不能由恢复程序临时猜。

**恢复程序应该怎么做**

遇到未知 type 时，应输出足够信息：

- record type
- LSN
- segment
- offset
- format version
- database/storage version
- 是否标记 optional
- 当前恢复阶段

然后按策略：

```text
if record.type is known:
    replay
elif record.is_optional_and_skippable:
    skip safely
else:
    fail recovery
```

面试里可以这样回答：WAL 是强一致恢复路径，未知关键事件默认 fail fast。只有格式显式声明可跳过、且不影响恢复语义时，才能跳过或降级。

## Q046. redo log 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

redo log 的核心目标是保证已提交修改在崩溃后可以恢复。它主要解决正确性问题，也顺带改善性能。

正确性体现在 durability 上：事务提交后，如果数据页还没刷盘，机器崩溃了，redo log 可以把这些修改重新应用到数据页。没有 redo log，要么提交时强制刷所有数据页，要么崩溃后丢已提交数据。

性能体现在 no-force 策略上：提交时只需要同步顺序日志，不必把分散的数据页都写回磁盘。数据页可以由后台 checkpoint、flush thread 或 compaction 机制慢慢处理。

redo log 通常记录：

- 哪个 page 或对象被修改。
- 修改对应的 LSN。
- redo 所需的新值、操作或 page image。
- transaction id 或上下文。
- checksum 和 length。

恢复时，系统从 checkpoint 的 redo 起点开始扫描 redo log。如果发现某个 page 的 pageLSN 小于 record LSN，就应用 redo；如果 pageLSN 已经更大或相等，就跳过。

redo log 不是安全机制。它不能防止恶意修改，也不能认证操作者身份。它也不是审计日志，虽然可能辅助审计。它对可维护性有帮助，因为有明确恢复语义的系统更容易推理，但它的第一目标不是代码结构，而是崩溃恢复。

一句话：redo log 让系统可以先承诺提交，再稍后刷数据页；崩溃后用 redo 把已提交但未落盘的修改补回来。

## Q047. redo log 的典型适用场景和不适用场景分别是什么？

**回答：**

redo log 适合“状态可以通过重放修改恢复”的系统。数据库、KV 存储、文件系统 journal、LSM memtable 持久化都属于这个范围。

典型适用场景：

1. **事务数据库**

   提交时写 redo，数据页稍后刷盘。崩溃后 redo 已提交修改。

2. **页式存储引擎**

   B-tree、heap page、索引页都可以用 pageLSN 和 redo record 做幂等恢复。

3. **LSM memtable 恢复**

   写入先进入 WAL 和 memtable。崩溃后 replay WAL 重建 memtable，再继续 flush。

4. **元数据更新**

   manifest、page allocation、extent map、free list 这类状态可以通过 redo 恢复到一致点。

5. **复制或热备**

   redo log 可以作为物理复制输入，让副本跟随主库页级状态变化。

6. **checkpoint 优化**

   checkpoint 只需保证某个 LSN 前的数据页状态足够持久。redo log 覆盖 checkpoint 之后的修改。

不适用或要谨慎的场景：

1. **需要业务审计语义**

   redo log 可能太底层。审计需要“谁做了什么业务动作”，而不是“page 7 offset 128 写了 bytes”。

2. **外部副作用**

   发送邮件、扣款、调用第三方 API 不能靠 redo 重放。重复执行会造成严重后果。

3. **非确定性逻辑**

   如果 redo record 只记录“重新执行某个函数”，而函数依赖当前时间、随机数、外部状态，就不安全。redo 应记录足够确定的结果或输入。

4. **跨系统原子提交**

   redo log 只能保护本系统内部恢复。数据库和消息队列双写，需要 outbox、CDC、2PC 或幂等补偿。

5. **长期业务事件回放**

   redo log 格式可能随存储引擎变化，不适合作为业务事件长期契约。

判断是否适合 redo log，可以问：崩溃后是否只需要把某些内部状态修改重新应用？如果是，redo log 很合适。如果要表达业务事实、合规审计或跨系统副作用，就需要更高层日志。

## Q048. redo log 和相近概念最容易混淆的边界在哪里？

**回答：**

redo log 最容易和 WAL、undo log、binlog、audit log、event log 混在一起。它们都叫 log，但读者和语义不同。

1. **redo log vs WAL**

   WAL 是原则：先写日志，再让数据页落盘。redo log 是一种具体日志内容，用来重放已提交修改。一个 WAL 体系里可能同时包含 redo、undo、commit、checkpoint、CLR 等 record。

2. **redo log vs undo log**

   redo 是“把应该保留的修改补上”。undo 是“把不该保留的修改撤掉”。redo 保护已提交事务的持久性，undo 保护未提交事务的原子性。

3. **redo log vs binlog**

   以 MySQL 为例，InnoDB redo log 主要用于存储引擎 crash recovery；MySQL binlog 主要用于复制、PITR 和逻辑变更记录。二者都重要，但不是同一层。

4. **redo log vs审计日志**

   redo log 可以证明某些底层修改发生过，但它不是面向审计人员的业务日志。审计日志需要用户、时间、业务对象、动作、来源和权限上下文。

5. **redo log vs event sourcing**

   event sourcing 的事件是业务事实，长期稳定，面向业务 replay。redo log 是内部恢复材料，可能记录物理页修改。

6. **redo log vs replication log**

   复制日志可以是物理 redo，也可以是逻辑事件。redo log 关注恢复一个副本的状态，复制协议还要处理网络、顺序、quorum、leader 切换。

7. **redo log vs checkpoint**

   checkpoint 是恢复基线，redo log 是从基线往后补齐状态的历史。checkpoint 不能替代 checkpoint 之后的 redo log。

8. **redo log vs snapshot**

   snapshot 是某个点的状态，redo log 是状态变化。恢复通常是加载 snapshot，再 replay redo。

一句话：redo log 的边界是“重放内部修改，让状态到达已提交结果”。它不是事务提交协议的全部，也不是业务审计、复制共识或外部事件系统。

## Q049. redo log 在高并发场景下可能出现哪些隐藏问题？

**回答：**

高并发下，redo log 会成为提交路径、内存路径和恢复路径的共同热点。问题不一定表现为错误，更多时候先表现为 p99 抖动、吞吐下降、恢复变慢。

常见隐藏问题：

1. **LSN 分配竞争**

   多线程同时生成 redo record，需要分配全局单调 LSN。实现粗糙时，全局锁会成为瓶颈。

2. **redo buffer 争用**

   WAL/redo buffer 是共享结构。并发写入需要预留空间、拷贝 record、更新 tail、处理 wrap。锁竞争或 cache line bouncing 会抬高延迟。

3. **group commit 队列抖动**

   高并发小事务依赖 group commit。batch 太小，fsync 次数多；batch 太大，等待变长。leader/follower 唤醒也可能带来抖动。

4. **大事务阻塞小事务**

   大事务生成大量 redo，占用 buffer、flush 带宽和 LSN 区间。小事务可能排在后面，被迫等待同一次 flush。

5. **checkpoint 与 redo flush 互相影响**

   checkpoint 写脏页时要保证对应 redo 已持久化。redo flush 慢会拖住 checkpoint；checkpoint I/O 又会拖慢 redo fsync。

6. **日志空间耗尽**

   redo log 空间有限时，如果 checkpoint 推进慢，日志不能复用。新写入会被阻塞，表现为突然的写入停顿。

7. **pageLSN 更新竞争**

   多线程修改同一 page 时，不仅要改 page 内容，还要更新 pageLSN。页锁或 latch 竞争会放大。

8. **刷盘错误广播困难**

   一次 redo fsync 失败影响一批事务。系统必须把错误传播给所有等待者，不能只有 flush 线程知道。

9. **副本或归档拖慢**

   如果 redo log 同时用于复制、归档，慢副本或归档失败会影响日志回收，间接阻塞前台写。

10. **恢复并行度被写入模式限制**

   高并发写入如果集中在少数 hot page 或 hot key，恢复时也难并行。写入时的热点会变成 replay 时的热点。

常见缓解方式：

- 分段 WAL buffer。
- 原子预留 LSN 区间。
- group commit。
- 大事务拆分或限流。
- 平滑 checkpoint。
- 独立 WAL 设备。
- 预分配 log segment。
- 监控 redo 写入速率、flush 延迟、checkpoint age、log space usage。

一句话：高并发 redo log 的难点不是“能不能追加写”，而是如何让 LSN、buffer、flush、checkpoint、错误传播和回收都不成为串行瓶颈。

## Q050. redo log 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

redo log 的边界条件集中在一个问题上：哪些修改已经承诺必须恢复，哪些只是写到一半或尚未提交。崩溃、重启、超时、重试都会把这个边界拉出来。

常见边界条件如下：

1. **commit record 是否持久化**

   update redo record 存在，不代表事务提交。恢复时要看 commit record 或事务状态。没有提交证据的事务不能直接视为成功。

2. **redo record 完整性**

   崩溃可能留下 partial record。恢复必须通过 length、checksum、LSN 连续性判断最后可信位置，不能把坏尾巴 replay。

3. **pageLSN 是否可信**

   数据页可能已经包含某条 redo，也可能没有。pageLSN 是幂等 redo 的关键。pageLSN 损坏或 torn page 会让恢复更复杂，需要 page checksum、full-page image 或 doublewrite buffer。

4. **checkpoint 是否完整**

   如果 checkpoint metadata 写了一半，恢复不能盲信它。要回退到上一个完整 checkpoint，或者根据 WAL 重新分析。

5. **redo fsync 超时不等于失败**

   应用层等待 fsync 超时后，底层 I/O 可能稍后成功。客户端重试可能导致同一业务操作重复。需要 transaction id、request id 或幂等逻辑。

6. **提交响应丢失**

   commit record 已持久化，但响应还没发给客户端就崩溃。客户端重试时，系统要识别这次事务其实已经提交。

7. **恢复过程中再次崩溃**

   redo replay 到一半又宕机，下一次恢复会再次 replay。同一条 redo 必须可重复执行，不能重复插入或重复扣减。

8. **日志和数据页版本不匹配**

   升级、回滚、备份恢复时，redo log 格式和数据页格式必须兼容。否则新 redo 无法应用到旧页，或者旧恢复程序看不懂新日志。

9. **归档或复制缺口**

   本地 redo 不完整时，需要归档或副本补齐。缺一段日志，恢复到目标点就不可靠。

10. **redo 后仍需 undo**

   在支持 steal/no-force 的系统里，redo 可能会重复历史，包括未提交事务的修改。redo 完成后还要 undo loser transactions。只做 redo 不一定得到事务一致状态。

11. **超时后的外部副作用**

   如果事务提交还伴随发消息、调用外部服务，redo 只能恢复数据库内部状态。外部副作用必须通过 outbox、幂等 key 或补偿流程处理。

恢复程序应该依赖磁盘上的事实：完整 redo record、commit record、checkpoint、pageLSN、checksum、durable LSN。不能依赖崩溃前内存里的“我已经准备提交了”。

一句话：redo log 能保证已提交修改可恢复，但前提是边界清楚、record 完整、replay 幂等、错误不被吞掉。

## Q051. redo log 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

redo log 的瓶颈通常先来自 I/O 和锁竞争，但具体要看系统形态。单机数据库常见瓶颈是 WAL/redo buffer、LSN 分配、fsync/fdatasync、checkpoint age；分布式系统还会把网络和副本确认放进提交路径。

可以分层看。

**CPU**

CPU 不是 redo log 的唯一瓶颈，但高并发下很容易出现：

- record 序列化和反序列化。
- checksum 计算。
- 压缩和加密。
- 内存拷贝。
- transaction table 更新。
- pageLSN 更新。
- 日志解析和恢复 replay。

如果 redo record 很小但 QPS 很高，单条 record 的固定 CPU 成本会被放大。恢复阶段也一样，扫描几千万条小 redo record，CPU 可能先于磁盘成为瓶颈。

**内存**

内存瓶颈主要在 redo buffer 和 page cache：

- redo buffer 太小，线程频繁等待 flush。
- 大事务占用大量日志 buffer。
- checkpoint 慢导致 dirty pages 堆积。
- page cache 写回和 redo flush 抢内存带宽。
- 恢复时数据页冷读导致 cache miss。

内存不是只影响“缓存命中率”，还会影响提交路径是否能顺利把 record 写入 buffer。

**锁竞争**

这是高并发 redo log 的常见瓶颈：

- LSN 分配锁。
- redo buffer reservation 锁。
- log write mutex。
- group commit 队列锁。
- transaction state 锁。
- page latch。
- checkpoint/flush 协调锁。

表现通常是：磁盘不满、CPU 不满，但事务 p99 很高。线程在 mutex、condition variable、latch 上排队。

**I/O**

I/O 是 redo log 最经典的瓶颈：

- WAL fsync/fdatasync 延迟高。
- 设备 flush 慢。
- WAL 所在磁盘和数据页 checkpoint 共用设备。
- 云盘 IOPS 或吞吐达到上限。
- WAL segment 创建、预分配、切换造成抖动。
- 归档或复制导致 WAL 无法及时回收。

如果业务要求每次 commit 等 redo 持久化，redo I/O 长尾会直接进入事务 p99。

**网络**

单机 redo log 通常没有网络瓶颈。分布式或云环境里，网络会进入路径：

- 同步复制等待副本收到或落盘。
- 云盘本身是网络块设备。
- WAL archive 写对象存储。
- 远端备份或 CDC 消费拖住保留策略。
- leader/follower 复制延迟影响提交。

同步复制场景下，redo log 的提交延迟可能是：

```text
本地写 WAL + 本地 flush + 网络 RTT + 副本写 WAL + 副本 flush
```

所以排查 redo 性能，不要只问 CPU、内存、I/O 哪个是瓶颈。更准确的问法是：当前 workload 的提交路径卡在哪一段。

实用指标包括：

- redo bytes/s
- redo records/s
- fsync latency p99
- bytes per flush
- group commit batch size
- log buffer wait
- checkpoint age
- dirty page flush rate
- log space usage
- mutex/latch wait
- replica flush/replay lag

一句话：redo log 是一条从内存并发结构到稳定存储的提交流水线。瓶颈可能在 CPU、锁、I/O 或网络，但 p99 往往来自这些层的排队叠加。

## Q052. redo log 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

redo log 的测试要分清三类目标。correctness test 验证恢复语义，stress test 验证高压和故障下不乱，benchmark 衡量吞吐、延迟和恢复成本。只测写入 QPS，不能证明 redo log 设计正确。

**correctness test 应该测什么**

1. **已提交事务可恢复**

   写 update record 和 commit record，flush redo log，模拟崩溃。恢复后确认已提交修改存在。

2. **未提交事务不会可见**

   写 update record，但没有 commit record。崩溃恢复后，系统不能把它当作已提交事务。支持 undo 的系统还要撤销其影响。

3. **pageLSN 幂等**

   构造数据页已经包含某条 redo 的场景，恢复时应跳过；构造 pageLSN 落后的场景，恢复时应补做。

4. **partial record**

   构造半条 redo record、损坏 checksum、非法 length。恢复必须停在 last good LSN，不能 replay 坏尾巴。

5. **checkpoint 边界**

   从 checkpoint redo LSN 开始恢复，验证不会漏掉 checkpoint 后的修改，也不会错误依赖已删除 WAL。

6. **redo 后 undo**

   对 steal/no-force 系统，恢复要先 redo history，再 undo loser transactions。测试不能只验证 redo 成功。

7. **格式兼容**

   旧版本 redo、新版本恢复；升级中崩溃；未知 record type；record version 变化。

**stress test 应该测什么**

1. **高并发小事务**

   多线程同时写 redo、commit、group commit。看 LSN 是否单调，是否有重复/缺口，是否出现死锁。

2. **大事务**

   单个事务生成大量 redo，观察 log buffer、flush、checkpoint、rollback/recovery 是否稳定。

3. **checkpoint 压力**

   让 dirty pages 大量积压，验证 redo log 空间不会被耗尽，或者耗尽时系统能安全限流。

4. **故障注入**

   kill 进程、断电、fsync 失败、磁盘满、partial write、torn page、归档失败、复制延迟。

5. **恢复中再次崩溃**

   redo replay 到一半再次崩溃，下一次恢复结果仍然正确。

6. **热点 page**

   多线程更新同一个 page 或少量 page，验证 pageLSN、latch 和 redo 顺序。

**benchmark 应该测什么**

1. **提交延迟分布**

   平均值不够，要看 p50、p95、p99、p999、max。

2. **吞吐**

   redo bytes/s、transactions/s、records/s。

3. **flush 行为**

   bytes per flush、fsync 次数、group commit batch size、flush wait。

4. **checkpoint 影响**

   checkpoint 开启、推进、结束时的提交延迟波动。

5. **恢复时间**

   崩溃后扫描 WAL、redo、undo、重建 index 的时间。恢复时间必须进入 benchmark。

6. **不同策略对比**

   always fsync、group commit、异步提交、不同 log buffer 大小、不同 checkpoint 周期、独立 WAL 盘 vs 共享盘。

7. **资源指标**

   CPU、内存、mutex wait、I/O await、队列长度、云盘额度、replica lag。

一个好的 redo log 测试报告应该同时回答：正常写有多快，崩溃恢复是否正确，故障注入时是否保守失败，恢复要多久。

## Q053. 如果要求从零实现一个简化版 redo log，你会先定义哪些不变量？

**回答：**

从零实现 redo log，先定义不变量，比先写 append 文件更重要。redo log 一旦进入提交路径，任何模糊语义都会变成恢复 bug。

我会先定义这些不变量：

1. **LSN 单调递增**

   每条 redo record 有唯一 LSN，LSN 按日志顺序递增。不能重复，不能倒退。

2. **record 自描述**

   每条 record 至少包含 magic、version、type、length、LSN、payload、checksum。恢复程序不能靠猜测解析。

3. **write-ahead rule**

   某个数据页被写回磁盘之前，该页 pageLSN 对应的 redo log 必须已经持久化。

4. **commit boundary 清楚**

   update redo record 不等于事务提交。事务是否提交，要看 commit record 或 transaction status record。

5. **pageLSN 幂等**

   每个数据页保存 pageLSN。redo 时只有 `pageLSN < record.LSN` 才应用。应用后推进 pageLSN。

6. **flushLSN 单调递增**

   系统维护 `flushedLSN` 或 `durableLSN`。commit ack 不能越过 `flushedLSN`。

7. **partial record 不可 replay**

   恢复扫描遇到不完整 header、非法 length、checksum 失败的尾部 record，必须停在上一条完整 record。

8. **错误不能被吞掉**

   redo write、fsync、归档失败必须暴露给提交路径或运维系统。不能只在后台打印日志。

9. **checkpoint 不可越过未持久化 redo**

   checkpoint 只能声明已经可以从某个 redo LSN 恢复。不能删除或覆盖仍被恢复需要的 redo。

10. **replay 结果等价于顺序执行**

   即使内部并行，最终结果必须等价于按 LSN 顺序 replay。

11. **恢复可重复**

   恢复过程中再次崩溃，下次恢复仍能得到同样结果。redo 不能依赖“只执行一次”。

12. **格式版本可识别**

   恢复程序遇到未知关键版本或未知关键 type，要 fail fast。

一个简化数据结构可以是：

```text
RedoRecord
  magic
  version
  type
  lsn
  txn_id
  page_id
  payload_len
  payload
  checksum

Page
  page_id
  data
  page_lsn

RedoState
  next_lsn
  flushed_lsn
  checkpoint_lsn
```

简化写入流程：

```text
reserve LSN
append redo record to log buffer
apply change to in-memory page
set page.page_lsn = record.lsn
on commit: append commit record
flush log to commit LSN
ack only if flush succeeds
```

简化恢复流程：

```text
load checkpoint
scan redo from checkpoint_lsn
validate record
if page.page_lsn < record.lsn:
    apply redo
    page.page_lsn = record.lsn
stop at first invalid tail record
undo loser transactions if needed
```

这套不变量不追求完整数据库功能，但能抓住 redo log 的根：顺序、完整性、持久边界、幂等恢复。

## Q054. redo log 的常见误用是什么，误用后通常会产生什么线上症状？

**回答：**

redo log 的误用通常来自两个方向：一是把 redo 当成提交语义的全部，二是只写日志但没有真正建立恢复协议。

常见误用和症状如下：

1. **写 redo 但不 fsync 就 ack**

   症状：进程崩溃可能没事，机器断电后丢已确认事务。用户看到“提交成功”，重启后数据消失。

2. **没有 commit record**

   症状：恢复时无法区分已提交和未提交事务。系统可能保留未提交修改，或者丢掉已提交修改。

3. **没有 pageLSN**

   症状：redo 重复执行导致重复插入、重复扣减、索引重复项。恢复一遍和恢复两遍结果不一样。

4. **checkpoint 删除 WAL 太早**

   症状：崩溃后恢复缺日志。轻则恢复失败，重则数据库启动到不一致状态。

5. **只记录逻辑操作但 replay 不确定**

   症状：恢复结果和崩溃前不一致。原因可能是 replay 依赖当前时间、随机数、外部服务、不同 schema 或不同索引状态。

6. **checksum 缺失或覆盖范围太小**

   症状：损坏 record 被当作合法 redo replay，造成二次数据损坏。或者出现 checksum mismatch 但无法定位是哪条 record。

7. **忽略 fsync 错误**

   症状：磁盘满或 EIO 后仍继续提交。之后恢复发现 WAL 缺口、坏尾巴或 commit record 丢失。

8. **redo 和 metadata 发布顺序反了**

   症状：manifest、index、checkpoint 指向 redo 中不存在的状态。重启后读到不存在文件、越界 offset 或 schema mismatch。

9. **把 redo log 当审计日志**

   症状：审计系统读不懂底层 record；升级后解析失败；未提交事务被误报；敏感数据暴露。

10. **复制中直接传 redo 但没有提交协议**

   症状：副本收到某段 redo 不代表集群已提交。failover 后出现已 ack 数据丢失或旧 leader 覆盖新 leader。

11. **恢复代码没覆盖升级路径**

   症状：线上升级后第一次崩溃才发现旧 WAL 新程序不能读，或新 WAL 旧程序不能回滚。

这些症状常常不是马上出现，而是在断电、满盘、升级、归档失败、长事务、恢复演练时暴露。redo log 的质量不能只靠正常压测判断，必须靠 crash test 和恢复测试证明。

## Q055. redo log 在单机和分布式环境中的语义有什么差异？

**回答：**

单机 redo log 的语义是本地崩溃恢复；分布式环境里的 redo log 只是副本状态的一部分。集群提交还要看复制协议、quorum、leader 任期和 failover 规则。

**单机语义**

在单机数据库中，redo log 回答的是：

```text
机器崩溃后，已提交修改能不能从本地日志恢复？
```

关键边界是：

- commit record 是否持久化。
- flushedLSN 是否覆盖 commit LSN。
- checkpoint redo LSN 是否正确。
- 数据页 pageLSN 是否能判断 redo 是否已应用。

如果这些成立，单机就能恢复到一个事务一致状态。

**分布式语义**

在分布式系统中，本地 redo 成功不等于集群提交成功。还要问：

- 这条日志是否复制到足够多副本？
- follower 是否持久化？
- leader 是否仍然有效？
- 新 leader 是否包含这条日志？
- commit index 是否推进？
- 客户端 ack 的语义是什么？

比如 leader 本地 redo 已经 fsync，但还没复制到多数派就宕机。新的 leader 可能没有这条记录。对集群来说，这条记录不一定 committed。

**复制方式差异**

分布式系统可能有不同模式：

- 异步复制：主库本地 redo 持久化后 ack，副本慢慢追。延迟低，但主库故障可能丢最近提交。
- 同步复制：等一个或多个副本确认后 ack。延迟高，但数据丢失窗口小。
- 共识复制：多数派确认后推进 commit index。语义强，但协议复杂。

**恢复差异**

单机恢复只需要本地 redo、checkpoint、undo。

分布式恢复还要处理：

- 日志分歧。
- follower 追赶。
- 旧 leader 回归。
- term/epoch fencing。
- quorum commit。
- 副本截断未提交尾巴。
- 读写分离的一致性。

所以 redo log 在单机里是提交恢复的核心证据；在分布式里，它只是“某个副本本地保存了什么”。真正的提交语义来自复制协议。

一句话：单机 redo 解决“我能不能恢复自己”；分布式 redo 还要回答“其他节点是否也同意这段历史”。

## Q056. undo log 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

undo log 的核心目标是撤销不应该保留的修改，并支持读一致性。它主要解决正确性问题。

undo log 解决两类经典问题：

1. **事务回滚**

   事务执行了一半失败、用户显式 rollback、死锁被选为 victim，都需要把该事务已经做过的修改撤销。undo log 保存旧值或反向操作，让系统能回到事务开始前的状态。

2. **崩溃恢复中的 loser transaction**

   如果数据库采用 steal 策略，未提交事务修改过的数据页可能已经被刷到磁盘。崩溃后恢复时，这些事务没有 commit record，必须 undo 掉。

在 MVCC 系统中，undo log 还常用于一致性读。比如 InnoDB 的 undo log record 保存如何撤销最新修改的信息；如果另一个事务需要看旧版本，可以根据 undo log 找回未修改的数据版本。

它主要是正确性机制：

- 保证 atomicity：事务要么全部生效，要么不生效。
- 支持 rollback：失败事务可以撤销。
- 支持一致性读：旧快照可以看到旧版本。
- 支持恢复：崩溃后清理未提交修改。

它也有性能影响。因为有 undo log，系统可以允许未提交脏页被写回，也可以让读者不阻塞写者，通过 MVCC 读旧版本。但 undo log 本身也会带来写放大、空间占用、purge 成本。

undo log 不是安全机制，也不是审计日志。它保存旧值，反而可能增加敏感数据保留范围。它对可维护性有帮助，但第一目标仍然是事务正确性。

一句话：redo log 负责把已提交修改补回来，undo log 负责把未提交或回滚的修改撤掉。

## Q057. undo log 的典型适用场景和不适用场景分别是什么？

**回答：**

undo log 适合需要事务回滚、MVCC 旧版本、崩溃后撤销未提交修改的系统。它不适合当作业务撤销、审计日志或外部副作用补偿机制。

典型适用场景：

1. **事务 rollback**

   事务执行失败或用户主动 rollback 时，undo log 提供反向修改。

2. **deadlock victim 回滚**

   数据库检测到死锁后，会选择一个事务回滚。undo log 让回滚可执行。

3. **savepoint**

   事务内部设置 savepoint 后，只回滚到某个中间点。undo log 可以沿事务链撤销部分操作。

4. **MVCC consistent read**

   读事务需要看到旧版本时，可以通过 undo log 找回修改前的数据。

5. **crash recovery**

   崩溃时未提交事务的修改如果已经落盘，恢复阶段要 undo。

6. **long transaction isolation**

   长事务持有旧 snapshot，需要 undo log 保留历史版本，直到它结束。

不适用或要谨慎的场景：

1. **业务层撤销按钮**

   用户点击“撤销订单取消”，不等于数据库 undo log。业务撤销通常要追加新的业务事件，并处理外部副作用。

2. **外部系统副作用**

   邮件、支付、发货、推送不能靠 undo log 反向执行。需要 outbox、补偿事务、幂等 key。

3. **长期审计**

   undo log 可能包含旧值，但格式内部化，生命周期受 purge 控制，不适合当审计日志。

4. **跨系统事务**

   undo log 只能撤销本数据库内部修改，不能自动撤销消息队列、缓存、搜索索引、第三方服务状态。

5. **无限期历史查询**

   MVCC undo log 通常不会无限保留。需要时间旅行查询或历史审计时，应使用专门历史表、event log 或归档机制。

6. **逻辑损坏修复**

   如果应用写入了错误但已提交，undo log 不一定还能或应该直接回滚。通常要用业务补偿或 PITR。

所以 undo log 是事务内部机制。它擅长撤销未提交修改和支持旧快照，不擅长表达业务反悔、合规审计或跨系统补偿。

## Q058. undo log 和相近概念最容易混淆的边界在哪里？

**回答：**

undo log 最容易和 redo log、rollback、MVCC version、审计日志、业务补偿混在一起。

1. **undo log vs redo log**

   redo 是重放修改，让已提交数据回来。undo 是撤销修改，让未提交或回滚的数据消失。redo 面向 durability，undo 面向 atomicity 和旧版本。

2. **undo log vs rollback**

   rollback 是操作或过程，undo log 是 rollback 所需的数据。没有 undo log，rollback 不知道怎么撤销已经做过的修改。

3. **undo log vs MVCC 版本链**

   在某些系统里，undo log 同时承担旧版本存储。读事务通过 undo log 找回旧版本。但概念上，MVCC 是并发控制模型，undo log 是实现材料之一。

4. **undo log vs savepoint**

   savepoint 是事务内部的回滚标记。undo log 提供从当前状态回退到 savepoint 的反向记录。

5. **undo log vs audit log**

   undo log 可能有旧值，但它不是给审计使用的。它可能被 purge，格式也不稳定，未必包含操作者、来源、业务上下文。

6. **undo log vs compensation transaction**

   undo log 撤销数据库内部未提交修改。补偿事务是业务层的新操作，用来抵消已提交业务动作。比如退款不是 undo log，而是一笔新的业务交易。

7. **undo log vs event sourcing**

   event sourcing 通常不修改历史事件，而是追加补偿事件。undo log 则是存储引擎内部为了回滚和旧版本读取保存的反向信息。

8. **undo log vs binlog**

   binlog 记录提交后的逻辑变更，服务复制和恢复。undo log 记录如何撤销事务修改，通常不直接给外部消费。

一句话：undo log 的边界是“让数据库撤销内部修改或读取旧版本”。它不是业务历史，不是审计证据，也不是外部副作用回滚器。

## Q059. undo log 在高并发场景下可能出现哪些隐藏问题？

**回答：**

undo log 在高并发下的问题，通常不是“能不能写 undo record”，而是版本保留、purge、空间、锁竞争和长事务之间的相互影响。

常见隐藏问题：

1. **undo 空间膨胀**

   高并发 update/delete 会生成大量 undo record。如果 purge 跟不上，undo tablespace 或 rollback segment 会增长。

2. **长事务阻止 purge**

   一个长时间未结束的读事务可能需要旧版本。系统不能删除这些 undo record。结果是历史链越来越长，空间和读取成本上升。

3. **consistent read 变慢**

   读事务要沿 undo 链找旧版本。链太长时，读延迟上升，甚至把本来简单的查询拖慢。

4. **rollback segment/undo slot 竞争**

   高并发读写事务需要分配 undo log 和 undo slot。资源不足或分配热点会造成等待。MySQL 文档也明确提到 rollback segment undo slots 会限制并发读写事务能力。

5. **大事务回滚时间长**

   大事务生成大量 undo。事务失败或被 kill 后，回滚可能跑很久，并继续占用资源。

6. **purge 与前台写互相干扰**

   purge 线程清理旧版本要消耗 I/O 和 CPU。purge 太慢会堆积，太猛又会影响前台业务。

7. **二级索引清理复杂**

   update/delete 可能涉及二级索引旧版本。清理时要考虑可见性和并发读者，不能简单删除。

8. **热点行更新**

   多事务更新同一行，会形成长版本链，还会引发锁等待、死锁、回滚和 undo 竞争。

9. **临时表 undo 语义不同**

   InnoDB 对临时表 undo 的处理和普通表不同。普通表 undo 要考虑 crash recovery；临时表 undo 只需运行时 rollback，可能不 redo-log。这种差异如果理解错，会误判持久性。

10. **监控不足**

   undo 问题常常先表现为磁盘涨、查询变慢、purge lag、history list length 变大，而不是直接报错。

常见缓解方式：

- 控制长事务。
- 监控 undo tablespace、history list、purge lag。
- 限制大事务。
- 优化 purge 线程和 I/O capacity。
- 避免热点行频繁更新。
- 把批量 delete/update 拆小。
- 为读写混合 workload 设置合理隔离级别。

一句话：高并发 undo log 的核心风险是历史版本清不掉。它会从空间问题慢慢变成读延迟、写延迟和恢复问题。

## Q060. undo log 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

undo log 的边界条件围绕“哪些修改要撤销、undo 记录本身是否可靠、undo 是否已经完成”展开。崩溃和重启会把这些问题直接暴露出来。

常见边界条件：

1. **未提交事务修改已经落盘**

   如果系统允许 steal，未提交事务修改的数据页可能已经写入磁盘。崩溃恢复时必须靠 undo log 撤销。没有 undo，原子性就破了。

2. **undo log 本身必须可恢复**

   普通表的 undo log 如果参与 crash recovery，就必须受 redo/WAL 保护。否则恢复时需要撤销，却找不到撤销信息。

3. **临时对象 undo 语义不同**

   临时表的 undo 可能只用于运行时 rollback，不需要 crash recovery。系统必须区分临时和持久对象，不能混用语义。

4. **rollback 过程中再次崩溃**

   事务回滚到一半宕机，下次恢复要继续回滚，而不是从头重复出错。ARIES 通过 CLR 处理类似问题，其他系统也需要可重复 undo 机制。

5. **commit/abort 状态不明**

   崩溃时事务状态可能处于中间。恢复要根据 commit record、abort record、prepare record、transaction table 判断事务是 winner、loser 还是 prepared。

6. **prepared transaction**

   两阶段提交中，prepared 事务不能随便 undo。它可能已经对外部协调者承诺准备提交，需要等待最终 commit/abort 决议。

7. **超时不等于事务失败**

   客户端超时后，数据库事务可能仍在执行、提交或回滚。客户端重试时，要防止重复写入或误以为旧事务已撤销。

8. **大事务 rollback 很慢**

   用户 kill 一个大事务后，数据库可能要长时间应用 undo。期间资源仍被占用，重启后也可能继续恢复/回滚。

9. **purge 不能早于读者**

   如果旧快照还需要某些 undo 版本，purge 不能清理。崩溃恢复后，也要正确恢复 purge 进度和可见性边界。

10. **undo 空间损坏**

   undo tablespace 或 undo segment 损坏会影响 rollback 和 consistent read。恢复程序可能无法安全撤销 loser transactions，只能进入只读、强制恢复或从备份修复。

11. **业务补偿不能依赖 undo**

   已提交事务的业务副作用不能靠 undo log 回滚。比如支付成功后超时，重试逻辑要用业务幂等，而不是期待数据库 undo 外部动作。

恢复时要保守。系统宁愿停下来要求人工处理，也不能在缺少 undo 信息时把未提交修改当成已提交状态继续运行。

一句话：undo log 的故障边界比 redo 更接近事务原子性。redo 负责补上已提交，undo 负责清掉不该留下的。

## Q061. undo log 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

undo log 的瓶颈通常来自内存、锁竞争和 I/O，CPU 也会参与放大问题。单机数据库里，网络一般不是 undo log 的直接瓶颈；到了分布式事务、复制数据库或云存储环境，网络才会进入提交、回滚或副本恢复路径。

更准确地说，undo log 的性能问题很少单独停在某一层。它经常表现为一条链：

```text
高并发写入
  -> 产生大量 undo record
  -> 长事务阻止 purge
  -> undo 历史版本堆积
  -> buffer pool 和磁盘被占用
  -> consistent read 要沿更长版本链回溯
  -> 前台读写变慢
```

**CPU 瓶颈**

CPU 主要消耗在这些地方：

1. **生成 undo record**

   每次 update/delete 可能都要保存旧值、row id、transaction id、roll pointer、字段变更信息。行越宽，修改列越多，undo record 构造成本越高。

2. **MVCC 可见性判断**

   读请求需要判断当前版本对自己是否可见。如果不可见，就要沿 undo 链找旧版本。版本链越长，CPU 消耗越高。

3. **purge 清理**

   purge 线程要判断哪些 undo record 已经不再被任何 read view 需要，然后清理旧版本、二级索引标记、undo segment。它不是简单删除文件。

4. **大事务 rollback**

   大事务回滚时，要反向应用大量 undo。这个过程会占用 CPU，也会继续产生锁、I/O 和缓存压力。

5. **校验、解析和格式转换**

   如果 undo record 带 checksum、压缩、变长字段、版本兼容逻辑，恢复和 purge 时都要付出解析成本。

CPU 瓶颈常见症状是：磁盘看起来没打满，但查询 CPU 飙高；大量时间花在 MVCC visibility、purge、row version reconstruction 上。

**内存瓶颈**

undo log 会占用两类内存：

1. **undo 页和相关元数据**

   undo log 本身通常存放在 undo tablespace 或类似结构里，但访问时仍会进入 buffer pool/page cache。高并发更新会把大量 undo 页带进缓存。

2. **事务和快照元数据**

   活跃事务表、read view、rollback segment 状态、undo slot、history list 等都要占用内存。

内存压力的典型表现是：

- buffer pool 被 undo 页挤占，热点数据页命中率下降。
- 长事务持有旧快照，导致旧版本不能清理。
- purge lag 增大，history list length 持续上升。
- 看起来是读慢，根因却是读请求要不断重建旧版本。

**锁竞争瓶颈**

undo log 是事务系统的一部分，高并发下容易碰到共享结构竞争：

1. **undo slot 分配竞争**

   每个读写事务需要分配 undo 空间或 undo slot。实现如果过度依赖全局锁，事务启动和写入都会排队。

2. **rollback segment 热点**

   rollback segment 数量不足或分配不均时，很多事务集中写同一组结构。

3. **事务表和 read view 竞争**

   创建 read view、检查活跃事务、推进 purge 边界，都可能访问共享事务状态。

4. **热点行更新**

   多个事务更新同一行，会生成一条很长的版本链，还会叠加行锁等待、死锁检测和 rollback 成本。

5. **purge 与前台写冲突**

   purge 清理旧版本时也要访问页、索引和元数据。清理太慢会堆积，清理太猛又会影响前台请求。

锁竞争的线上表现通常是吞吐上不去、p99 抖动、事务等待时间增加，但磁盘和 CPU 单看都不像满载。

**I/O 瓶颈**

I/O 是 undo log 的常见硬瓶颈，尤其在 update/delete 密集场景：

1. **undo 写入放大**

   一次业务 update 不只改数据页，还要写 undo record，并且 undo record 本身通常还要被 redo/WAL 保护。

2. **undo tablespace 膨胀**

   purge 跟不上时，undo 空间增长。磁盘空间接近上限后，前台写入可能被迫限流甚至失败。

3. **purge 产生读写 I/O**

   purge 要读旧 undo 页、访问数据页和索引页、释放空间。它会和前台业务争 I/O。

4. **崩溃恢复扫描**

   如果崩溃前有大量未完成事务，恢复时要读取 undo 并执行 rollback。undo 越大，恢复时间越长。

5. **云盘或网络盘延迟**

   数据库进程在本地，持久化设备在远端，undo I/O 延迟就带有网络因素。

I/O 瓶颈的症状包括 undo 文件增长、checkpoint 或 purge 跟不上、写入延迟突然上升、恢复时间变长。

**网络瓶颈**

单机 undo log 通常不直接走网络。下面这些场景会让网络进入语义边界：

1. **分布式事务**

   参与者的 undo 是本地的，但是否 rollback 取决于协调者的 commit/abort 决议。网络分区会让 prepared transaction 卡住。

2. **同步复制数据库**

   如果 undo 相关日志也要复制到 quorum，提交或回滚延迟会包含网络往返。

3. **远程存储**

   数据库进程在本地，持久化设备在远端，undo I/O 延迟就带有网络因素。

4. **跨节点恢复**

   failover 后，新节点可能要根据复制日志、undo/redo 状态决定事务命运。网络落后会影响恢复边界。

面试回答可以这样总结：undo log 的瓶颈不是“CPU 还是 I/O”这种单选题。短事务高并发常见瓶颈是 undo slot、rollback segment、事务表和 buffer pool；长事务场景常见瓶颈是 purge lag 和 undo 空间；大事务失败时，瓶颈会变成 rollback I/O 和恢复时间。网络只在分布式事务、同步复制或远程存储里成为主要因素。

一句话：redo log 的瓶颈常在提交持久化路径，undo log 的瓶颈常在旧版本保留和清理路径。

## Q062. undo log 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

undo log 的测试要分三层：correctness test 证明事务语义正确，stress test 证明高压和故障下不乱，benchmark 衡量代价和退化曲线。只测正常提交吞吐，不能说明 undo log 是可靠的。

**correctness test 测什么**

correctness test 关注“结果是否对”。它应该覆盖事务回滚、MVCC 读、崩溃恢复和 purge 边界。

1. **单事务 rollback**

   开启事务，执行 insert/update/delete，然后 rollback。验证数据恢复到事务前状态。

   重点检查：

   - update 是否恢复旧值。
   - delete 是否恢复被删行。
   - insert 是否被移除。
   - 索引和数据行是否一致。

2. **commit 后不能被 undo**

   事务提交后，即使后续有其他事务 rollback，也不能把已提交修改撤掉。

3. **savepoint 部分回滚**

   事务内设置 savepoint，之后做多次修改，只回滚到 savepoint。验证 savepoint 前的修改保留，savepoint 后的修改撤销。

4. **多条修改的反向顺序**

   同一事务先 update A，再 update B，再 delete C。rollback 必须按相反顺序执行，否则可能破坏索引或约束。

5. **MVCC consistent read**

   事务 T1 开启快照读。T2 修改并提交同一行。T1 再读时应看到旧版本，T2 或新事务应看到新版本。

6. **长事务阻止 purge**

   有长读事务持有旧快照时，purge 不能删除它还需要的 undo record。长事务结束后，purge 才能清理。

7. **崩溃恢复中的 loser transaction**

   模拟 steal 策略：未提交事务的脏页已经写盘，然后崩溃。恢复后未提交修改必须被 undo。

8. **崩溃发生在 rollback 中途**

   大事务 rollback 到一半时崩溃。重启后应继续 rollback，最终状态和一次完整 rollback 相同。

9. **undo record 损坏**

   构造 checksum 错、length 错、链断裂、roll pointer 指向非法位置。恢复程序应保守失败，不能把不可信 undo 当成有效记录。

10. **索引一致性**

   update/delete 后 rollback，不只检查主表，还要检查二级索引、唯一索引、外键或内部索引结构是否回到一致状态。

11. **隔离级别边界**

   在 read committed、repeatable read、serializable 等隔离级别下，undo/MVCC 的可见性可能不同。测试要覆盖对应语义。

correctness test 的目标不是跑得快，而是能回答：任意事务失败、崩溃、重启后，已提交的保留，未提交的撤销，快照读看到它应该看到的版本。

**stress test 测什么**

stress test 关注“压力下是否还能保持语义”。它要故意制造长事务、大事务、热点更新、空间压力和故障注入。

1. **高并发 update/delete**

   多线程持续修改大量行，观察 undo 空间、history list、purge lag、死锁率、事务等待。

2. **热点行更新**

   很多事务反复更新同一行或少量行。验证版本链、行锁、deadlock rollback、undo slot 分配不会出现错误。

3. **长读事务 + 高频写入**

   一个长事务持有旧 snapshot，其他线程持续更新。验证 purge 不会提前清理旧版本，也要观察空间膨胀是否可控。

4. **大事务失败**

   构造百万行 update 后 rollback，或事务中途 kill。验证 rollback 可完成，系统不会长期不可用。

5. **磁盘空间接近耗尽**

   undo tablespace 或日志目录逼近满盘。系统应明确报错或限流，不能写出半一致状态。

6. **频繁死锁**

   让多个事务按相反顺序更新资源，触发 deadlock victim rollback。检查 undo 是否释放锁、回滚数据、清理事务状态。

7. **crash injection**

   在这些点 kill 进程或模拟断电：

   - undo record 写入后。
   - 数据页写入后。
   - commit record 写入前。
   - rollback 进行中。
   - purge 进行中。
   - checkpoint 进行中。

8. **并发 purge**

   purge 与前台读写同时运行。验证不会删除仍可见版本，也不会和前台事务死锁。

9. **版本升级兼容**

   用旧版本生成 undo，再用新版本恢复；或者升级后回滚未完成事务。undo 格式兼容性必须被测到。

stress test 的关键指标不是只有 QPS，还包括错误率、恢复成功率、最大 rollback 时间、history list 峰值、空间增长速度和系统是否能优雅限流。

**benchmark 测什么**

benchmark 关注“代价有多大”。它应该把 undo log 的成本拆开，而不是只报一个整体 TPS。

1. **正常提交吞吐**

   测 insert/update/delete 在不同事务大小下的吞吐和延迟。update/delete 往往比纯 insert 更能暴露 undo 成本。

2. **rollback 吞吐和延迟**

   测不同规模事务的 rollback 时间。比如 1 行、100 行、10 万行、百万行事务。

3. **consistent read 成本**

   控制版本链长度，测读旧版本的延迟。版本链从 1、10、100、1000 增长时，延迟曲线很有价值。

4. **purge 吞吐**

   测 purge 每秒能清理多少 undo record、多少页面、多少索引项。

5. **空间放大**

   统计每次业务 update 产生多少 undo bytes、redo bytes、数据页写入和索引写入。

6. **p99 和 p999 延迟**

   undo 问题往往先体现在尾延迟。平均值可能很好看，但某些事务因为分配 undo slot、等待 purge 或回滚而拖很久。

7. **恢复时间**

   构造不同数量的未提交事务和不同大小的 undo log，测重启恢复耗时。

8. **资源分解**

   同时记录 CPU、buffer pool 命中率、I/O await、fsync 延迟、锁等待、undo tablespace size。

9. **配置敏感性**

   改 rollback segment 数量、purge 线程数、buffer pool 大小、I/O capacity、事务批大小，看吞吐和尾延迟怎么变化。

面试里可以用一个简洁判断：correctness test 问“语义对不对”，stress test 问“高压和故障下会不会乱”，benchmark 问“代价在哪、退化曲线长什么样”。

## Q063. 如果要求从零实现一个简化版 undo log，你会先定义哪些不变量？

**回答：**

从零实现 undo log，先定义不变量。undo log 不变量比代码结构更重要，因为事务失败、崩溃恢复、MVCC 读旧版本都依赖这些边界。

一个简化版 undo log 可以先不支持完整 SQL、不支持复杂索引，但下面这些不变量不能含糊。

1. **需要回滚的修改必须先有 undo record**

   对任何可能需要撤销的修改，在修改数据页之前，必须生成足够的 undo 信息。

   例如：

   ```text
   update key=A old=10 new=20
   ```

   undo record 至少要能表达：

   ```text
   rollback 时把 key=A 从 20 恢复为 10
   ```

2. **undo record 必须能被解析和校验**

   每条 undo record 至少包含：

   - magic 或 record type。
   - version。
   - length。
   - transaction id。
   - sequence 或 undo LSN。
   - payload。
   - previous undo pointer。
   - checksum。

   恢复程序不能靠猜测解析 undo record。遇到非法 length、checksum 错误、未知版本，要有明确失败策略。

3. **同一事务的 undo record 形成可反向遍历的链**

   每个事务要保存 `lastUndoPtr`。每次生成新 undo record，都指向前一条。

   结构类似：

   ```text
   tx.lastUndoPtr -> undo3 -> undo2 -> undo1
   ```

   rollback 时从 `undo3` 开始反向执行，直到链尾或 savepoint 边界。

4. **rollback 顺序必须与修改顺序相反**

   同一事务内的撤销必须后进先出。原因很简单：后面的修改可能依赖前面的修改。

   例子：

   ```text
   insert row R
   update row R
   delete row R
   ```

   rollback 时如果顺序错了，可能先恢复一个并不存在的行，或者破坏索引状态。

5. **事务状态必须持久且可恢复**

   系统要能区分：

   - active
   - committed
   - aborting
   - aborted
   - prepared

   崩溃后不能只靠内存判断事务状态。事务表、commit/abort record、undo 链头这些信息必须能恢复出来。

6. **未提交修改落盘时，undo 必须更早可恢复**

   如果系统允许 steal，也就是未提交事务修改过的数据页可以写回磁盘，那么对应 undo record 必须已经持久化，或者被 redo/WAL 保护到可恢复。

   否则会出现最危险的情况：未提交脏数据已经在磁盘上，但系统找不到撤销信息。

7. **commit 不等于立即删除 undo**

   事务提交后，它的 undo record 不再用于回滚该事务，但可能仍被旧快照读需要。只有确认没有 read view 需要这些旧版本后，purge 才能删除。

8. **purge 不能早于最老读者**

   设系统中最老活跃快照为 `oldestReadTs` 或 `lowWatermarkTxnId`。任何可能被该快照读取的 undo record 都不能被清理。

   伪代码：

   ```text
   if undo.version_needed_by_any_active_snapshot:
       keep
   else:
       purge
   ```

9. **rollback 必须可重复执行**

   系统可能在 rollback 到一半时再次崩溃。重启后继续 rollback，结果必须和一次完成 rollback 一样。

   常见做法：

   - 给 undo 进度写日志。
   - 给补偿操作写 CLR 或类似记录。
   - 让每个撤销动作可检查是否已经执行过。

10. **savepoint 必须对应 undo 链上的稳定边界**

   savepoint 不是一个字符串标记而已。它要记录当时事务的 undo 链位置。

   ```text
   savepoint S = tx.lastUndoPtr
   rollback to S: undo until current == S
   ```

11. **索引和数据必须一起恢复一致**

   如果 update/delete 影响二级索引，undo 不能只恢复主表值，还要恢复索引结构。简化实现可以先不支持二级索引；一旦支持，就必须把索引恢复纳入不变量。

12. **undo 只撤销本地内部修改**

   undo log 不负责撤销邮件、支付、消息发送、缓存更新。实现层面要明确：外部副作用必须由业务幂等、outbox 或补偿事务处理。

13. **空间使用必须可计量**

   undo 不能无限增长而系统毫无感知。至少要维护：

   - 当前 undo bytes。
   - 每个事务 undo bytes。
   - purge 进度。
   - oldest active snapshot。
   - 可回收空间。

14. **错误处理必须保守**

   如果恢复时发现 undo 链断裂、校验失败、事务状态不明，不能继续假装恢复成功。宁愿只读启动、停止恢复、报警或从备份恢复，也不要把未提交数据当成已提交状态。

一个简化实现的流程可以这样写：

```text
begin transaction
for each update/delete:
    create undo record with old value
    append undo record
    link it to transaction.lastUndoPtr
    apply data change
on commit:
    write commit marker
    keep undo until no snapshot needs it
on rollback:
    walk undo chain backward
    apply inverse changes
    write abort marker
purge:
    remove undo records older than all active snapshots
```

一句话：undo log 的核心不变量是“先记录怎么撤销，再允许修改变得可见或可落盘；只要事务没提交或旧快照还需要，撤销信息就不能丢”。

## Q064. undo log 的常见误用是什么，误用后通常会产生什么线上症状？

**回答：**

undo log 的误用通常来自把它当成更高层语义：当作业务撤销、审计日志、历史查询、跨系统补偿，或者把它当成可以随便清理的临时垃圾。线上症状往往不会立刻爆炸，而是在长事务、回滚、崩溃恢复、空间压力和升级时暴露。

常见误用如下。

1. **把 undo log 当业务撤销**

   误用：用户取消订单、撤销付款、回滚发货时，期待数据库 undo log 自动处理。

   症状：

   - 数据库内状态回滚了，外部系统已经发货或扣款。
   - 重试后出现重复退款、重复消息、重复发货。
   - 业务流水缺一笔补偿事件，审计对不上。

   正确做法是业务补偿、outbox、幂等 key、状态机，而不是依赖存储引擎 undo。

2. **把 undo log 当审计日志**

   误用：认为 undo log 里有旧值，所以可以给审计系统读取。

   症状：

   - purge 后审计数据消失。
   - 升级后 undo 格式变了，审计解析失败。
   - 缺少操作者、IP、权限、业务原因等上下文。
   - 敏感旧值被内部日志意外保留，合规风险增加。

3. **把 undo log 当长期历史查询**

   误用：希望通过 undo log 支持任意时间点查询旧版本。

   症状：

   - 长事务或旧快照让 undo 无法清理，磁盘持续增长。
   - 一旦 purge 发生，历史查询失效。
   - 查询旧版本越来越慢，线上读延迟上升。

   时间旅行查询应设计专门的历史表、归档或事件日志。

4. **提交后还期待 undo 能撤回**

   误用：事务已经 commit，应用层发现业务错了，还想用 undo log 回到之前。

   症状：

   - 有些数据能在底层找到旧值，有些已经 purge，行为不稳定。
   - 外部副作用无法同步撤销。
   - 已提交状态被内部工具强行改回，复制、副本、审计全部乱掉。

5. **purge 过早**

   误用：为了节省空间，忽略活跃 read view，提前清理 undo。

   症状：

   - 长查询报错，提示找不到旧版本。
   - repeatable read 读到不该看到的新版本。
   - 快照一致性被破坏。

6. **purge 太慢或没人管**

   误用：只关注前台 TPS，不监控 undo history、purge lag、undo tablespace。

   症状：

   - undo 文件持续膨胀。
   - 磁盘满。
   - 查询越来越慢。
   - 重启恢复时间变长。
   - 删除大量数据后空间迟迟不释放。

7. **undo record 没有被 redo/WAL 保护**

   误用：数据页可以落盘，但对应 undo record 只在内存或普通缓冲里。

   症状：

   - 崩溃后发现未提交修改已经在数据页上，却找不到撤销信息。
   - 恢复只能失败、只读启动或从备份恢复。
   - 更糟糕的情况是系统误把未提交数据当成已提交。

8. **rollback 不幂等**

   误用：rollback 过程中没有记录进度，也不能检测某条 undo 是否已经应用。

   症状：

   - 回滚中途崩溃后，重启再次回滚导致重复删除、重复插入、索引损坏。
   - 同一个崩溃镜像恢复两次结果不同。

9. **把客户端超时等同于事务失败**

   误用：客户端请求超时后，认为数据库事务肯定回滚。

   症状：

   - 客户端重试写入，数据库里出现重复业务记录。
   - 原事务其实已经 commit，新请求又提交一次。
   - 应用层用错误状态覆盖正确状态。

   超时只说明客户端没有拿到结果，不说明事务已经 abort。

10. **大事务无限制**

   误用：批量 update/delete 放在一个巨大事务里，不限制行数和时间。

   症状：

   - rollback 要跑几个小时。
   - undo 空间暴涨。
   - 长时间持锁，拖慢其他请求。
   - 机器重启后继续恢复，服务迟迟不可写。

11. **混淆临时对象和持久对象的 undo**

   误用：把临时表 undo、普通表 undo、运行时 rollback、crash recovery 语义混在一起。

   症状：

   - 临时数据被错误纳入恢复路径，拖慢恢复。
   - 持久数据的 undo 没有正确持久化，崩溃后无法撤销。

12. **忽略敏感旧值**

   误用：认为删除或更新敏感字段后，旧值就不存在了。

   症状：

   - undo log、备份或快照里仍保留旧值。
   - 数据脱敏、删除请求、合规保留策略和实际存储不一致。

面试里可以这样答：undo log 只保证数据库内部事务撤销和旧版本读取，不承担业务补偿、审计证明、长期历史查询和外部副作用回滚。误用后的症状多半是磁盘涨、长查询慢、大事务回滚久、崩溃恢复失败、以及业务状态和外部系统对不上。

## Q065. undo log 在单机和分布式环境中的语义有什么差异？

**回答：**

单机 undo log 的语义是本地事务原子性和 MVCC；分布式环境中的 undo log 只是某个节点、某个分片、某个参与者的本地撤销材料。全局事务是否提交或回滚，要看协调协议，而不是看某个节点有没有 undo record。

**单机语义**

在单机数据库里，undo log 回答的是：

```text
这个本地事务失败或崩溃后，如何把本地数据恢复到应有状态？
```

它承担几件事：

- rollback 当前事务。
- 撤销崩溃时未提交的 loser transaction。
- 支持 MVCC consistent read。
- 支持 savepoint。
- 配合 redo/WAL 保证 steal/no-force 策略下的事务正确性。

单机语义的边界比较清楚：

- commit record 持久化后，事务不能被本地 undo 当成未提交事务撤销。
- abort 或未提交事务需要沿 undo 链撤销。
- purge 取决于本机活跃 read view。
- 外部系统副作用不属于 undo log。

**分布式语义**

分布式系统里，同一个业务事务可能跨多个节点：

```text
T 修改 shard A
T 修改 shard B
T 发送消息或更新缓存
```

每个 shard 可以有自己的 undo log，但这并不自动形成全局原子性。

分布式环境要额外回答：

1. **谁决定 commit/abort**

   可能是 2PC coordinator、共识 leader、事务管理器，或者业务 saga 编排器。单个参与者不能只根据本地 undo 自己决定最终结果。

2. **prepared transaction 能不能 undo**

   两阶段提交中，参与者进入 prepared 状态后，已经承诺可以提交。此时不能因为本地重启就随便 undo，必须等待 coordinator 的 commit/abort 决议。

3. **网络超时不等于失败**

   节点 A 等 coordinator 超时，不代表全局事务 abort。可能 coordinator 已经决定 commit，只是消息延迟。

4. **不同副本的 undo 进度要和复制协议一致**

   如果数据库有主从复制或共识复制，undo/redo/事务状态也要通过日志复制保持一致。副本不能根据落后的本地状态独立执行错误的 rollback。

5. **failover 后的事务命运**

   新 leader 必须知道哪些事务 committed、prepared、aborted、in-doubt。否则可能把旧 leader 已承诺的事务撤销，或者把未提交事务当成已提交。

6. **跨系统副作用需要业务补偿**

   undo log 只能撤销本数据库分片的内部数据。消息队列、支付系统、搜索索引、缓存更新，需要 outbox、inbox、幂等消费、saga compensation。

**单机和分布式的关键差异**

| 维度 | 单机 undo log | 分布式环境 |
|---|---|---|
| 决策范围 | 本地事务 | 全局事务或分片事务 |
| 失败类型 | 进程崩溃、机器断电、本地 I/O 错误 | 网络分区、leader 切换、消息重复、部分参与者失败 |
| commit 判断 | 本地 commit record / durable state | coordinator 决议、quorum、term/epoch、commit index |
| rollback 判断 | 本地事务未提交或 abort | 必须尊重全局决议，prepared 状态不能随意回滚 |
| purge 边界 | 本机活跃快照 | 还可能受复制槽、备份、只读副本、分布式快照影响 |
| 外部副作用 | 不负责 | 仍然不负责，需要业务补偿 |

**举个例子**

一个分布式事务 T 修改两个分片：

```text
shard A: prepared
shard B: prepared
coordinator: 已决定 commit，但 commit 消息只发到 shard A
shard B: 崩溃重启
```

shard B 重启后看到本地事务没有最终 commit record，不能直接 undo。它处于 in-doubt 状态，必须询问 coordinator 或从复制/共识日志中恢复最终决议。否则 shard A 提交、shard B 回滚，全局状态就分裂了。

**复制数据库里的另一个差异**

如果系统使用 Raft/Paxos 一类共识日志，本地 undo log 还要服从复制日志：

- 只有 committed log entry 对应的状态才能对外承诺。
- follower 的本地 undo/redo 进度不能超过复制协议允许的 apply 进度。
- leader 切换后，未提交尾巴可能被截断，对应的本地事务状态也要被处理。

一句话：单机 undo log 解决“我这个数据库如何撤销本地不该保留的修改”；分布式环境还要解决“所有参与者是否对同一段历史达成一致”。本地 undo 只是材料，不是全局事务协议。

## Q066. LSN 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

LSN 的核心目标是给日志流提供一个稳定、单调、可比较的位置和顺序坐标。它主要解决正确性问题，也会带来性能和可维护性收益，但它不是安全机制。

LSN 通常表示 Log Sequence Number。可以把它理解成 WAL 世界里的“坐标”。这个坐标不只是为了定位字节，更是为了回答恢复系统里最重要的几个问题：

- 这条日志在全局顺序里排在哪里？
- 这个数据页已经包含到哪条日志？
- WAL 已经持久化到哪里？
- checkpoint 覆盖到哪里？
- snapshot 对应哪个日志边界？
- 副本发送、接收、flush、replay 到哪里？
- 哪些日志还能删除，哪些必须保留？

**它首先解决正确性**

1. **恢复顺序**

   WAL replay 必须按正确顺序执行。LSN 给每条 record 一个可比较位置，恢复程序可以从 checkpoint redo LSN 开始顺序扫描。

2. **pageLSN 幂等 redo**

   数据页保存 `pageLSN`，表示这个页已经包含到哪个日志位置。恢复时：

   ```text
   if page.pageLSN >= record.LSN:
       skip
   else:
       apply redo
   ```

   没有这个判断，redo 可能重复执行，把数据改坏。

3. **write-ahead rule**

   数据页写回磁盘前，对应的 WAL 至少要 flush 到该页的 pageLSN。也就是：

   ```text
   flushedLSN >= page.pageLSN
   ```

   这个不变量是 WAL 正确性的基础。

4. **提交持久边界**

   事务 commit record 的 LSN 小于等于 durableLSN 时，事务才可以被认为本地持久提交。

5. **checkpoint 起点**

   checkpoint 记录 redo LSN。崩溃恢复时从这个位置附近开始，而不是从日志开头扫描。redo LSN 如果错了，可能漏恢复。

6. **日志截断和保留**

   系统要知道早于哪个 LSN 的 WAL 已经不再被本地恢复、归档、复制、备份需要。删错会导致恢复失败。

**它也改善性能**

LSN 带来的性能收益来自“少做无用功”和“可以批量处理”：

- 从 checkpoint redo LSN 开始恢复，减少扫描范围。
- 用 pageLSN 跳过已经应用的 redo。
- group commit 可以一次 flush 到某个 LSN，让多个事务一起完成。
- 复制可以按 LSN 增量发送，不必全量同步。
- 清理 WAL 时可以按保留 LSN 回收 segment。

这些是性能收益，但前提仍然是正确性语义清楚。

**它也改善可维护性**

有了 LSN，工程排查会清晰很多：

- “主库发送到 LSN A，备库 replay 到 LSN B。”
- “checkpoint redo LSN 是 C。”
- “checksum 失败发生在 LSN D。”
- “这个数据页 pageLSN 超过可用 WAL，说明元数据不一致。”

LSN 让日志、页面、快照、复制和恢复都可以用同一套坐标对齐。

**它不解决安全性**

LSN 不是签名、不是认证、不是防篡改机制。攻击者可以伪造或修改 LSN，除非系统另有 checksum、MAC、签名、权限控制、加密认证等机制。

也不要把 LSN 当作：

- 用户可见 ID。
- 随机 token。
- 权限凭证。
- 业务幂等键。
- 密码学 nonce。

一句话：LSN 的第一目标是正确性。它把“日志顺序、页面状态、持久化进度、恢复起点、复制进度”放到同一条坐标轴上。

## Q067. LSN 的典型适用场景和不适用场景分别是什么？

**回答：**

LSN 适合用来表达日志流中的顺序、恢复边界、持久化进度和复制进度。不适合当业务 ID、安全 token、墙上时间或跨独立系统的全局顺序。

**典型适用场景**

1. **WAL record 排序**

   每条 WAL record 分配 LSN。恢复时按 LSN 顺序扫描，保证状态演进和崩溃前一致。

2. **pageLSN**

   数据页头保存 pageLSN，表示该页已经应用到哪个日志位置。redo 时用它判断是否需要重放。

3. **flushedLSN / durableLSN**

   WAL buffer 写入磁盘并完成 fsync 后，系统推进 durableLSN。事务 commit record 的 LSN 被 durableLSN 覆盖后，才能安全 ack。

4. **checkpoint redo LSN**

   checkpoint 记录恢复起点。崩溃恢复从 redo LSN 开始扫描，减少恢复时间。

5. **snapshot 边界**

   snapshot 可以记录 `snapshot_lsn`，表示这个快照包含到哪个日志位置。恢复时从对应 LSN 之后继续 replay。

6. **PITR**

   point-in-time recovery 可以恢复到某个时间，也可以恢复到某个 LSN 或 restore point。LSN 是比时间更精确的恢复坐标。

7. **复制进度**

   主库和备库可以用 LSN 表示：

   - sentLSN
   - receivedLSN
   - flushedLSN
   - replayedLSN

   这能清楚区分“收到日志”和“已经应用日志”。

8. **WAL 归档和清理**

   归档、备份、复制 slot、checkpoint 都可能要求保留某个最小 LSN 之后的日志。系统用这些 LSN 的最小值决定能删到哪里。

9. **故障定位**

   恢复失败、checksum mismatch、unknown record type、复制延迟，都可以用 LSN 精确定位范围。

**不适用场景**

1. **用户可见全局唯一 ID**

   LSN 可能暴露写入量、系统内部拓扑和时间顺序。它也可能在备份恢复、分片、重建、逻辑导入时不适合作为稳定业务 ID。

2. **安全 token 或访问控制依据**

   LSN 可预测、可比较，不具备不可伪造性。不能用它判断用户权限或身份。

3. **墙上时间**

   LSN 表示日志顺序，不表示真实时间。两个事务 LSN 相邻，不代表它们在业务时间上间隔很短。

4. **跨独立集群的全局顺序**

   每个数据库实例、每个 shard、每条 WAL 流都可能有自己的 LSN 空间。没有全局协调时，不能拿不同系统的 LSN 直接比较。

5. **业务版本号**

   业务对象的 version 通常描述某个对象被修改了几次。LSN 描述整个日志流的位置。对象 version 和 LSN 可以关联，但不是同一个概念。

6. **共识 term 或 epoch**

   term/epoch 表示领导权时代，LSN 表示日志位置。分布式系统里两者常一起出现，但不能互相替代。

7. **Kafka offset 的直接替代**

   Kafka offset 是 partition 内消息位置。LSN 是数据库 WAL 位置。两者都能排序，但所属系统和语义不同。

8. **审计事实**

   LSN 能证明某条底层日志的位置，不能单独回答“谁做了什么业务动作、是否授权、来源是什么”。

面试里可以用一个判断标准：如果问题是“日志到哪里、页面到哪里、恢复从哪里、复制追到哪里”，LSN 很适合；如果问题是“用户是谁、业务对象是什么、是否可信、是否全局唯一”，LSN 不够。

## Q068. LSN 和相近概念最容易混淆的边界在哪里？

**回答：**

LSN 最容易和 offset、sequence number、transaction id、timestamp、commit timestamp、Raft index/term、Kafka offset、对象版本号混淆。它们都能排序，但排序对象不同。

可以先抓住一句话：LSN 是日志流里的恢复坐标，不是所有顺序概念的统称。

| 概念 | 排序对象 | 常见用途 | 和 LSN 的区别 |
|---|---|---|---|
| LSN | WAL/log record | 恢复、pageLSN、checkpoint、复制进度 | 关注日志顺序和恢复边界 |
| file offset | 文件字节位置 | 文件读写定位 | 只是物理位置，不一定有恢复语义 |
| sequence number | 业务记录或消息序号 | 去重、业务排序、版本推进 | 范围和含义由业务定义 |
| transaction id | 事务身份 | MVCC 可见性、锁、事务状态 | 标识事务，不等于日志位置 |
| timestamp | 墙上时间 | 展示、审计、过期判断 | 可能不单调，不等于写入顺序 |
| commit timestamp | 事务提交时间 | 时间查询、审计辅助 | 表示时间，不一定是日志物理顺序 |
| Raft log index | 共识日志位置 | 状态机复制 | 属于共识日志，不必等同数据库 WAL LSN |
| Raft term/epoch | leader 任期 | 防止旧 leader 写入 | 表示领导权时代，不表示字节位置 |
| Kafka offset | partition 内消息位置 | 消费进度 | 只在一个 partition 内有序 |
| object version | 单个对象版本 | 乐观锁、CAS | 只描述对象自身，不描述全局 WAL |

**LSN vs file offset**

很多系统会把 LSN 编码成 segment id + offset。于是容易误以为 LSN 就是文件偏移。

二者区别是：

- offset 说明 bytes 在文件哪里。
- LSN 说明这条日志在恢复顺序里处于哪里。

在简单实现里，LSN 可以等于 offset；在复杂系统里，LSN 可能跨 segment、跨 timeline、带逻辑编码，甚至不直接暴露物理文件位置。

**LSN vs transaction id**

transaction id 标识事务，LSN 标识日志位置。一个事务可能产生多条 WAL record：

```text
T100:
  LSN 100 update page A
  LSN 120 update page B
  LSN 140 commit
```

所以不能说事务 T100 的 LSN 只有一个。更准确的说法是：事务有 commit LSN、last LSN、begin LSN 或相关日志范围。

**LSN vs commit timestamp**

commit timestamp 是时间，LSN 是日志顺序。时间可能受系统时钟、NTP 调整、时区、精度影响。LSN 通常由日志写入路径分配，单调性更适合恢复。

如果两个事务时间戳一样，LSN 仍然可以区分顺序。

**LSN vs Raft index/term**

Raft index 是共识日志中的位置，term 是 leader 任期。数据库 LSN 是存储引擎 WAL 位置。

在某些系统里，Raft log entry 内部可能包含数据库 WAL 或状态机命令。它们可以建立映射：

```text
Raft index 500 -> apply command -> database LSN 9000
```

但不能默认相等。Raft 解决复制一致性，LSN 解决本地日志恢复和页面状态对齐。

**LSN vs Kafka offset**

Kafka offset 是 partition 内单调递增的位置。多个 partition 之间 offset 不能直接比较。

LSN 通常描述数据库 WAL 流。如果数据库有多个 WAL stream 或多个 shard，也要问清楚 LSN 是否全局可比。

**LSN vs object version**

对象版本号常用于乐观锁：

```text
update table set value=?, version=version+1 where id=? and version=?
```

这个 version 只属于某一行或某个对象。LSN 属于日志流。一个 LSN 可能影响多个对象，一个对象也会对应多个 LSN。

**不同 LSN 名称也容易混**

同一个系统里还可能有：

- writeLSN：写到日志缓冲或文件的位置。
- flushLSN：已经刷到持久存储的位置。
- durableLSN：对崩溃恢复可靠的位置。
- replayLSN：恢复或副本已经应用的位置。
- checkpointLSN：checkpoint 覆盖的位置。
- restartLSN：恢复或复制需要保留的位置。

这些名字都带 LSN，但语义不同。面试时最好说全：是“写到哪里”，还是“刷到哪里”，还是“应用到哪里”。

一句话：LSN 是恢复坐标。offset 是物理位置，事务 id 是身份，timestamp 是时间，Raft term 是领导权，Kafka offset 是分区消息位置。它们可能互相映射，但不能混用。

## Q069. LSN 在高并发场景下可能出现哪些隐藏问题？

**回答：**

LSN 在高并发下的问题，往往不是“能不能递增”，而是递增、发布、持久化、应用、回收这几条进度线是否一致。只要其中一条线被误解，就会出现很隐蔽的提交、恢复或复制 bug。

常见问题如下。

1. **全局 LSN 分配成为热点**

   多线程同时写 WAL，需要分配单调 LSN。如果每条 record 都抢同一个全局锁，吞吐很快会到顶。

   缓解方式通常是：

   - 批量预留 LSN 范围。
   - per-core buffer 聚合后统一发布。
   - group commit。
   - 单 writer 顺序落盘，多 producer 提交 record。

2. **LSN 预留和实际写入乱序**

   线程 A 预留了 LSN 100，线程 B 预留了 LSN 120。B 先写完，A 卡住。

   系统不能因为 B 写完就把 durableLSN 推到 120，因为 100 到 119 之间可能还有洞。durableLSN 必须按连续前缀推进。

3. **writtenLSN、flushedLSN、durableLSN 混淆**

   高并发下，很多线程会看到不同进度：

   - WAL 已经复制到内存 buffer。
   - WAL 已经 write 到文件描述符。
   - WAL 已经 fsync。
   - WAL 已经被副本收到。
   - WAL 已经被副本 replay。

   如果把这些都叫“写到了 LSN X”，提交语义就容易错。最危险的是把 writtenLSN 当 durableLSN。

4. **pageLSN 发布顺序错误**

   修改页面和写 WAL 之间要有明确顺序。页面 pageLSN 不能发布到一个尚未可恢复的 LSN，然后又被后台刷盘。

   错误顺序可能导致：

   ```text
   page.pageLSN = 200
   data page flushed
   WAL only durable to 150
   crash
   ```

   这违反 WAL 规则。恢复时页面声称自己包含 LSN 200，但 WAL 里没有 200。

5. **内存可见性和发布屏障**

   在多核系统中，线程写 WAL record、更新长度、更新 LSN、发布给 flusher，需要正确的内存屏障或锁保护。

   否则 flusher 可能看到 LSN 已发布，但 record payload 还没完全写好。

6. **group commit 中的等待队列错误**

   多个事务等待同一次 flush。flush 到 LSN 500 后，只能唤醒 commit LSN <= 500 的事务。

   如果唤醒范围算错：

   - 提前唤醒会承诺未持久事务。
   - 漏唤醒会造成事务无故卡住。

7. **并行 replay 的全局进度假推进**

   WAL replay 可以按 page 或 partition 并行，但全局 replayLSN 必须按连续安全点推进。

   worker 处理完 LSN 1000，不代表 LSN 900 已完成。对外报告 replay 到 1000 可能误导读请求或 failover。

8. **复制进度语义混乱**

   主库发送到 LSN 1000，不代表备库已经持久化或应用到 1000。

   高并发复制里要区分：

   - sentLSN
   - receivedLSN
   - flushedLSN
   - replayedLSN
   - appliedLSN

   如果监控或 failover 逻辑用错，会出现读到旧数据、丢已 ack 数据、错误提升落后副本。

9. **日志保留被慢消费者拖住**

   checkpoint 可能允许删除早期 WAL，但复制 slot、备份、归档、慢副本还需要旧 LSN。高并发写入下，WAL 生成速度快，慢消费者会导致日志目录迅速膨胀。

10. **LSN wraparound 或整数类型不足**

   短期面试题很少关注这个，但长期运行系统必须考虑。32-bit LSN 很快不够，64-bit 也要定义比较、wrap、timeline、epoch 语义。

11. **分片 LSN 误比较**

   多 shard 系统可能每个 shard 都有自己的 LSN。拿 shard A 的 LSN 1000 和 shard B 的 LSN 1000 比较，没有意义，除非系统定义了全局日志或全局时间戳。

12. **指标命名误导排查**

   监控里只写 `lsn=xxx`，不写是什么 LSN。排障时没人知道这是 write、flush、replay 还是 checkpoint。

线上症状通常是：

- p99 提交延迟抖动。
- group commit 偶发卡住。
- 复制延迟指标互相矛盾。
- crash recovery 报 pageLSN 超过 WAL。
- failover 后丢失最近已 ack 的事务。
- WAL 不能回收，磁盘持续增长。

一句话：高并发下 LSN 的难点是“连续前缀”和“语义分层”。分配出去的 LSN、写完的 LSN、刷盘的 LSN、应用的 LSN、可删除的 LSN，不是一回事。

## Q070. LSN 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

LSN 在故障场景下会暴露一个核心问题：系统声称到达某个 LSN，是否真的有足够的持久化数据支撑这个声明。崩溃、重启、超时和重试会把 written、flushed、durable、checkpoint、replay 这些边界全部拉出来。

常见边界条件如下。

1. **尾部 partial record**

   崩溃可能发生在 WAL record 写到一半时。恢复程序扫描到尾部，要通过 length、checksum、magic、LSN 连续性判断 last good LSN。

   处理原则：

   - 最后未完成的 record 可以截断。
   - 已承诺 durable 的 LSN 范围内不能有坏 record。
   - 不能跳过坏 record 继续 replay 后面的日志。

2. **durableLSN 和实际磁盘不一致**

   内存里 durableLSN 已经推进，但崩溃前并没有真正 fsync 成功。重启后只能相信磁盘上的 WAL，而不是崩溃前内存变量。

   如果系统曾经对客户端 ack 到某个 LSN，但磁盘恢复不到该 LSN，就是持久性 bug。

3. **pageLSN 超过可用 WAL**

   数据页写回时违反 write-ahead rule，可能出现：

   ```text
   page.pageLSN = 500
   WAL valid only to 450
   ```

   重启后恢复程序发现页面声称包含更晚的修改，但 WAL 不存在。这通常说明刷页、WAL flush 或存储屏障有严重问题。

4. **checkpoint LSN 过新**

   checkpoint 写入 metadata，声称可以从 LSN 1000 开始恢复，但实际上某些脏页只覆盖到 LSN 900，且 900 到 1000 的 WAL 已删除。

   结果是恢复漏 redo。正确做法是 checkpoint 的 redo LSN 必须保守，且相关 WAL 在 checkpoint 安全前不能删除。

5. **checkpoint LSN 过旧**

   过旧一般不破坏正确性，但会拖慢恢复。系统会从更早 LSN 扫描更多 WAL。

   面试里要区分：过旧影响性能，过新可能破坏正确性。

6. **snapshot LSN 与文件内容不匹配**

   snapshot 声称包含到 LSN 2000，但文件只是一组模糊拷贝，没有保留足够 WAL 修复到 2000。

   恢复时可能出现：

   - 漏应用部分修改。
   - 重复应用部分修改。
   - 索引和数据文件来自不同 LSN。

   所以 snapshot 必须记录一致性边界，并保留从正确 replay start LSN 开始的 WAL。

7. **复制 failover 的 LSN 边界**

   客户端收到主库 ack，不代表所有副本都 replay 到该 LSN。failover 时要看提交策略：

   - 异步复制可能丢最近 ack 的事务。
   - 同步复制要确认 quorum 或指定副本已经持久化。
   - 新 leader 必须包含已承诺 LSN 范围。

   不能只看某个副本 receivedLSN 高，就认为它能安全提升。

8. **超时不等于没有到达某个 LSN**

   客户端提交请求超时，事务可能处于多种状态：

   - WAL 还没写。
   - WAL 写了但没 fsync。
   - commit record 已 fsync，但响应丢了。
   - 主库提交了，副本还没 replay。

   客户端重试时，不能只靠超时判断旧请求失败。需要事务 id、幂等 key、唯一约束或查询 commit 状态。

9. **重试导致同一逻辑操作多次进入日志**

   如果客户端或上层服务重试同一业务操作，而系统没有幂等键，WAL 会出现多条不同 LSN 的记录。

   对数据库来说它们是不同修改；对业务来说可能是重复下单、重复扣款。LSN 只能说明顺序，不能替业务做去重。

10. **恢复到目标 LSN 的包含关系**

   PITR 或测试恢复时，要明确“恢复到 LSN X”是包含 X，还是停在 X 之前。

   这个边界如果没定义清楚，结果会差一条事务：

   ```text
   replay records with LSN <= targetLSN
   ```

   或：

   ```text
   replay records with LSN < targetLSN
   ```

   系统必须固定语义并写进工具说明。

11. **timeline/epoch 切换**

   备份恢复、主从切换、fork 新历史后，同一个数值 LSN 可能属于不同 timeline。只保存裸 LSN 不够，可能还要保存 timeline id、epoch、term 或 incarnation。

12. **日志截断后的重试**

   某个消费者要求从旧 LSN 继续读取，但 WAL 已经被回收。系统要明确报错，让它重新做 base backup 或 snapshot，而不是返回错误范围的数据。

13. **LSN 写入 metadata 的原子性**

   manifest、checkpoint 文件、index 文件里常保存 LSN。崩溃可能发生在 metadata 更新中途。重启后要能选择上一个完整 metadata 版本，不能读到半写入的 LSN。

14. **监控恢复后进度回退**

   重启后，内存里的 writtenLSN、flushedLSN、replayLSN 可能回到磁盘实际值。监控看到 LSN 回退不一定是 bug，可能是崩溃前有未持久内存进度。关键要看系统是否曾经对外承诺过那些 LSN。

面试里可以这样回答：LSN 在故障场景下的核心边界是“哪些 LSN 是已经写入、哪些已经持久、哪些已经应用、哪些已经对外承诺”。恢复程序只能相信持久介质和可校验日志，不能相信崩溃前内存里的进度变量。

一句话：LSN 是恢复坐标，但坐标必须和持久化事实一致。只推进数字，不保证数字背后的日志和页面真实存在，反而会制造更隐蔽的数据损坏。

## Q071. LSN 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

LSN 本身只是一个序号，真正的性能瓶颈不在“比较一个数字”上，而在分配、发布、持久化、回收和跨节点同步这些动作上。单机里常见瓶颈是 CPU cache line、原子操作、锁竞争、WAL buffer 和 fsync；分布式系统里，网络和 quorum 确认会变成额外瓶颈。

可以把 LSN 看成提交路径上的一组进度线：

```text
allocate LSN
write WAL record
publish writtenLSN
fsync and advance durableLSN
apply page or replica state
advance replayLSN/checkpointLSN
```

每一步都可能卡住。

**CPU 瓶颈**

CPU 瓶颈通常来自高频原子操作和缓存一致性。

1. **全局计数器争用**

   多线程同时生成 WAL record，如果每条 record 都对同一个全局 LSN counter 做原子加法，这个 counter 所在 cache line 会在多个 CPU core 之间来回迁移。

   症状是 CPU 使用率高，但业务吞吐上不去。profile 里可能看到大量时间花在 atomic add、spin、mutex、memory barrier 上。

2. **LSN 比较和可见性判断**

   pageLSN、flushedLSN、replayLSN、checkpointLSN 到处都要比较。单次比较便宜，但在高并发 replay、复制、脏页刷写、MVCC 检查中累计起来也会有成本。

3. **record 编码和 checksum**

   LSN 通常会进入 WAL record header 和 checksum 范围。写日志时要编码 header、计算校验、对齐 record，这些都消耗 CPU。

CPU 层的优化常见做法是批量预留 LSN、减少全局原子操作、分区写 buffer、批量 checksum、减少共享 cache line 抖动。

**内存瓶颈**

LSN 的内存瓶颈主要来自 WAL buffer、等待队列和进度表。

1. **WAL buffer 容量不足**

   LSN 分配很快，但 WAL buffer 写不出去，会造成后续线程等待空间。

2. **等待者过多**

   group commit 中，大量事务等待 flush 到自己的 commit LSN。等待队列维护、唤醒、超时处理都会消耗内存和调度资源。

3. **进度元数据膨胀**

   复制 slot、归档任务、备份任务、并行 replay worker 都可能维护自己的 LSN。慢消费者越多，进度表和保留状态越复杂。

4. **缓存污染**

   高速写 WAL 时，日志 buffer、脏页队列、checkpoint 元数据可能挤占业务数据缓存。

内存瓶颈常见表现是 WAL buffer full、提交线程排队、dirty page 激增、内存分配频繁或 GC 压力变高。

**锁竞争瓶颈**

LSN 在高并发数据库里很容易成为串行点。

1. **LSN 分配锁**

   粗糙实现会用一把大锁保护 LSN 分配、WAL record 写入和 flush 队列。线程越多，锁等待越明显。

2. **WAL 插入锁**

   即使 LSN 分配可以并行，真正把 record 放入 WAL buffer 时也可能需要协调空间、对齐、segment 切换。

3. **flush 状态锁**

   多个事务等同一个 flush，flusher 推进 durableLSN 后要唤醒等待者。这里如果锁粒度太粗，会直接影响 p99。

4. **checkpoint 和前台写竞争**

   checkpoint 要读取 dirty page 表、推进 redo LSN、判断可回收 WAL。前台写同时分配新 LSN，二者如果共用锁，会互相影响。

5. **复制进度锁**

   主库要维护每个副本的 sent/received/flushed/replayed LSN。副本多、ack 高频时，复制进度更新也会成为热点。

锁竞争的典型症状是：CPU 没满、磁盘没满、网络也没满，但事务提交 p99 很高，线程栈集中在 WAL insert、LSN allocation、flush wait、checkpoint wait。

**I/O 瓶颈**

I/O 是 LSN 语义落地的地方。LSN 可以在内存里飞快推进，但对外承诺通常只能推进到持久化边界。

1. **fsync/fdatasync**

   commit record 的 LSN 必须被 durableLSN 覆盖后才能承诺本地持久提交。fsync 延迟会直接决定提交尾延迟。

2. **WAL segment 切换**

   日志文件切换、预分配、归档、回收都可能让写入短暂停顿。

3. **checkpoint I/O**

   checkpoint 要把脏页刷下去，才能让 redo 起点向前推进。如果刷脏页速度跟不上，WAL 不能回收，前台写可能被限流。

4. **恢复 replay I/O**

   崩溃后从某个 LSN 开始扫描 WAL，读取日志、读取数据页、应用 redo。LSN 范围越大，恢复 I/O 越重。

I/O 瓶颈的症状很直接：fsync 延迟升高、WAL 写带宽打满、checkpoint age 增大、WAL 文件堆积、恢复时间变长。

**网络瓶颈**

单机 LSN 不需要网络。分布式系统里，LSN 常常要和复制确认、共识提交、远程存储绑定。

1. **同步复制**

   主库提交到 LSN X 后，可能要等副本收到或持久化到 X。网络 RTT 进入提交路径。

2. **quorum commit**

   如果系统要求多数副本确认，提交延迟取决于多数派中较慢的节点。

3. **远程 WAL 存储**

   一些云数据库把日志写到远程持久化服务。LSN 的 durable 边界由远程服务确认，网络抖动会反映到提交延迟。

4. **跨区域复制**

   跨可用区、跨地域复制会放大 LSN 推进延迟。sentLSN 可能很靠前，replayedLSN 可能落后很多。

面试可以这样回答：LSN 的性能瓶颈不是数字本身，而是围绕这个数字建立的串行化路径。单机主要看原子分配、WAL buffer、flush、checkpoint；分布式还要看复制确认和 failover 语义。

## Q072. LSN 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

LSN 的测试要围绕一个核心问题展开：所有进度变量是否单调、可解释、可恢复，并且不会把“写入”“刷盘”“应用”“承诺”混成一件事。correctness test 验证语义，stress test 验证并发和故障下不乱，benchmark 衡量 LSN 机制对提交、恢复和复制的成本。

**correctness test 测什么**

1. **唯一性和单调性**

   并发分配大量 LSN，验证没有重复、没有倒退。若设计要求连续，还要验证没有不可解释的洞。

2. **record 顺序**

   WAL 文件中的 record 顺序必须和 LSN 顺序一致。扫描时不能出现：

   ```text
   LSN 100
   LSN 80
   LSN 120
   ```

3. **LSN 和物理位置映射**

   如果 LSN 编码了 segment id + offset，就要测试 segment 切换、文件预分配、尾部截断后映射仍然正确。

4. **pageLSN 幂等 redo**

   构造 pageLSN 小于、等于、大于 record LSN 的场景：

   - 小于时应 apply redo。
   - 等于或大于时应跳过。
   - pageLSN 超过可用 WAL 时应报错或进入保守恢复。

5. **write-ahead rule**

   刷数据页之前，对应 WAL 必须 flush 到 pageLSN。测试可以故意插入断点，确认系统不会先刷页后刷 WAL。

6. **durableLSN 边界**

   写到内核缓冲不等于 durable。测试要区分 writtenLSN、flushedLSN、durableLSN。commit ack 只能发生在 commit LSN 被 durableLSN 覆盖后。

7. **checkpoint redo LSN**

   checkpoint 记录的 redo LSN 不能过新。恢复从该 LSN 开始，必须能恢复所有已提交修改。

8. **WAL 截断和保留**

   删除早期 WAL 前，要确认本地恢复、备份、复制、归档都不再需要。测试慢副本和备份同时存在的情况。

9. **PITR 目标 LSN**

   恢复到目标 LSN 时，要验证包含关系明确。比如 `<= targetLSN` 和 `< targetLSN` 不能混用。

10. **timeline/epoch**

   备份恢复或主从切换后，裸 LSN 可能不够。测试 timeline id、term、epoch 是否参与比较和校验。

**stress test 测什么**

1. **高并发 LSN 分配**

   多线程同时生成 WAL record，验证 LSN 无重复、无倒退，等待队列不死锁。

2. **预留 LSN 后乱序完成**

   线程 A 预留低 LSN 但卡住，线程 B 预留高 LSN 并先完成。系统不能把 durableLSN 推过 A 的空洞。

3. **group commit 压力**

   大量事务等待同一批 flush。验证唤醒范围准确，不提前 ack，也不漏唤醒。

4. **checkpoint 并发**

   前台持续写入，后台 checkpoint 推进 redo LSN。验证 checkpoint 不会删除仍需要的 WAL。

5. **并行 replay**

   多 worker replay 不同 page 或 partition。验证全局 replayLSN 只按连续完成范围推进。

6. **复制延迟**

   制造慢副本、网络抖动、ack 乱序。验证 sent/received/flushed/replayed LSN 不混淆。

7. **crash injection**

   在 LSN 分配后、record 写一半、fsync 前、fsync 后 ack 前、checkpoint metadata 写一半时崩溃。恢复后检查 last good LSN 和事务状态。

8. **wraparound 或大 LSN**

   用接近边界的 LSN 测比较函数。不能用普通有符号减法随便比较，尤其是有 epoch/timeline 时。

**benchmark 测什么**

1. **LSN 分配成本**

   单线程和多线程下，每秒能分配多少 LSN，每次分配的 ns/op，随着线程数增加是否线性退化。

2. **WAL insert 延迟**

   从拿到 LSN 到 record 放入 WAL buffer 的耗时，观察 p50/p99。

3. **commit 等待时间**

   commit LSN 到 durableLSN 覆盖之间的等待时间。这个指标比平均 TPS 更能反映用户延迟。

4. **group commit 效果**

   不同 batch size、flush interval 下，吞吐和 p99 如何变化。

5. **checkpoint 推进速度**

   redo LSN 推进速度、checkpoint age、WAL 回收速度。

6. **replay 速度**

   每秒 replay 多少 WAL bytes、多少 record、多少 page，pageLSN 跳过比例是多少。

7. **复制 LSN 延迟**

   主库 current LSN 和副本 replayedLSN 的差距。按 bytes 和时间两个维度看。

8. **资源归因**

   同时记录 CPU、锁等待、WAL 写带宽、fsync 延迟、网络 RTT、慢副本数量。

一个好的 LSN 测试报告应该能回答：顺序是否正确，持久边界是否可信，高并发下是否有洞，崩溃后能否找到 last good LSN，以及这个机制给提交和恢复增加了多少成本。

## Q073. 如果要求从零实现一个简化版 LSN，你会先定义哪些不变量？

**回答：**

从零实现 LSN，先不要急着写一个自增整数。LSN 是日志系统的坐标，必须先定义它和 WAL record、数据页、flush、checkpoint、复制之间的关系。

一个简化版 LSN 至少要有这些不变量。

1. **LSN 全局单调**

   在同一条 WAL stream 内，新分配的 LSN 必须大于之前分配的 LSN。不能倒退，不能重复。

2. **LSN 属于明确的作用域**

   必须说清楚这个 LSN 是：

   - 单个进程内全局。
   - 单个数据库实例全局。
   - 单个 shard 内全局。
   - 单条 partition/WAL stream 内全局。

   没有全局协调时，不同 shard 的 LSN 不能直接比较。

3. **每条 WAL record 有确定 LSN**

   record header 至少包含：

   - magic。
   - version。
   - type。
   - length。
   - LSN。
   - previous LSN 或 record start/end。
   - checksum。

   恢复扫描时可以验证 record 是否处在它声明的 LSN 位置。

4. **LSN 与物理位置有可解释映射**

   简化实现可以让 LSN 等于 WAL 文件 offset，也可以用：

   ```text
   LSN = segment_id * segment_size + offset
   ```

   不管怎么设计，都要能从 LSN 找到 record 附近的位置，或者能顺序扫描到它。

5. **record 不能跨越未定义边界**

   如果 record 跨 segment，要明确允许还是禁止。简单实现可以禁止跨 segment，segment 剩余空间不足就 padding，再从新 segment 开始。

6. **writtenLSN、flushedLSN、durableLSN 分开**

   这三个概念不能混：

   - writtenLSN：已经写入 WAL buffer 或文件。
   - flushedLSN：已经提交给操作系统或设备 flush。
   - durableLSN：崩溃后可恢复的边界。

   简化实现里也要明确 commit ack 使用哪个边界。

7. **durableLSN 只能按连续前缀推进**

   即使高 LSN record 先写完，只要低 LSN 有洞，durableLSN 就不能跳过去。

   ```text
   complete: 100, 120
   missing: 110
   durableLSN cannot become 120
   ```

8. **pageLSN 与 WAL 持久化遵守 write-ahead rule**

   数据页可以带 pageLSN。刷页前必须满足：

   ```text
   durableLSN >= page.pageLSN
   ```

   否则崩溃后页面可能包含找不到日志的修改。

9. **redo 幂等依赖 pageLSN**

   replay 时：

   ```text
   if page.pageLSN < record.LSN:
       apply record
       page.pageLSN = record.LSN
   else:
       skip
   ```

   这条不变量让恢复可以重复执行。

10. **checkpointLSN 必须保守**

   checkpoint 声称“从这个 LSN 开始恢复就够了”。它不能超过实际需要的最早 redo LSN。过旧只是慢，过新会丢恢复信息。

11. **WAL 删除受最小需要 LSN 约束**

   可删除边界通常是多个需求的最小值：

   ```text
   min(checkpointRedoLSN, archiveLSN, replicaRestartLSN, backupLSN)
   ```

   任何一个消费者还需要旧 WAL，就不能删。

12. **LSN 比较函数唯一**

   不要到处用裸整数比较。应该封装：

   ```text
   compareLSN(a, b)
   ```

   将来加入 timeline、epoch、wraparound 时，不会把比较逻辑散落在代码里。

13. **崩溃后只相信持久介质**

   重启时，内存中的 nextLSN、writtenLSN、durableLSN 都丢了。系统必须通过扫描 WAL、读取 checkpoint metadata、校验 record 来恢复进度。

14. **错误处理保守**

   如果扫描到 checksum 错、LSN 不连续、length 非法：

   - 发生在尾部未承诺区域，可以截断。
   - 发生在已承诺 durable 区域，必须报错并进入修复流程。

15. **对外暴露要带语义名**

   监控和 API 不要只叫 `lsn`。要叫 `current_lsn`、`flush_lsn`、`replay_lsn`、`checkpoint_lsn`。否则排障时很容易误判。

一个最小实现可以这样组织：

```text
reserve LSN range
write WAL record with LSN
publish writtenLSN when bytes are complete
fsync WAL
advance durableLSN to contiguous flushed range
allow commit ack if commitLSN <= durableLSN
allow page flush if pageLSN <= durableLSN
checkpoint records conservative redoLSN
recovery scans from checkpoint redoLSN to last good LSN
```

一句话：LSN 不变量的核心是“顺序、连续、持久边界、页面边界、恢复边界都能互相解释”。如果这些关系没定义，LSN 只是一个看起来递增的数字。

## Q074. LSN 的常见误用是什么，误用后通常会产生什么线上症状？

**回答：**

LSN 的误用通常来自把它当成更通用的东西：业务 ID、时间戳、安全 token、跨集群全局顺序，或者把不同语义的 LSN 混成一个值。误用后的症状可能很隐蔽，常常在崩溃恢复、主从切换、备份恢复、WAL 清理时才暴露。

常见误用如下。

1. **把 writtenLSN 当 durableLSN**

   误用：WAL record 写进内存 buffer 或调用 write 成功后，就认为事务持久了。

   症状：

   - 进程崩溃可能没事，机器断电后丢已 ack 事务。
   - 恢复扫描找不到客户端已经收到成功的 commit record。
   - 线上表现为“偶发丢数据”，很难复现。

2. **把 receivedLSN 当 replayedLSN**

   误用：副本收到日志，就认为副本已经应用到这个位置。

   症状：

   - 读请求路由到副本后读不到刚写入的数据。
   - failover 提升了收到但没应用的副本，启动后还要长时间 replay。
   - 监控显示复制“追上了”，用户仍然读旧值。

3. **把 LSN 当业务全局 ID**

   误用：把数据库内部 LSN 暴露为订单号、对象 ID、事件 ID。

   症状：

   - 备份恢复、逻辑导入、分片迁移后 ID 语义变得尴尬。
   - 暴露内部写入速率和数据库拓扑。
   - 跨 shard 无法比较或去重。

4. **把 LSN 当安全凭证**

   误用：认为知道某个 LSN 就能证明访问权限或请求真实性。

   症状：

   - 攻击者猜测或重放 LSN。
   - 权限判断绕过。
   - 审计里只有位置，没有身份和授权信息。

   LSN 可比较、通常可预测，不具备安全语义。

5. **跨 shard 直接比较 LSN**

   误用：shard A 的 LSN 1000 大于 shard B 的 LSN 900，所以认为 A 更新更晚。

   症状：

   - 全局排序错误。
   - 跨分片一致性检查误报。
   - CDC 合并流乱序。

   没有全局时钟或全局日志时，不同 shard 的 LSN 只在各自作用域内有意义。

6. **把 LSN 当 wall-clock time**

   误用：用 LSN 推断真实时间间隔。

   症状：

   - 写入高峰时 LSN 增长很快，低峰时增长很慢。
   - 两个 LSN 相近不代表时间相近。
   - 审计报告或 SLA 统计失真。

7. **checkpoint LSN 推得太靠前**

   误用：checkpoint metadata 记录了一个过新的 redo LSN，随后删除了旧 WAL。

   症状：

   - 崩溃后恢复缺日志。
   - 某些已提交修改丢失。
   - 数据页和索引状态不一致。

8. **WAL 回收只看本地 checkpoint**

   误用：本地恢复不需要旧 WAL 就删除，忽略归档、备份、复制 slot、慢副本。

   症状：

   - 备库断流后无法追赶。
   - PITR 缺日志。
   - base backup 无法恢复到一致点。

9. **把 LSN 当幂等键**

   误用：客户端重试时用新产生的 LSN 判断业务操作是否重复。

   症状：

   - 同一业务请求重试生成多条不同 LSN，数据库认为是不同操作。
   - 重复下单、重复扣款、重复消息。

   幂等应该用业务 request_id、唯一约束或 dedup table。

10. **恢复到目标 LSN 的包含边界不清楚**

   误用：工具没有说明恢复到 LSN X 是否包含 X。

   症状：

   - 恢复结果差一条事务。
   - 演练和生产恢复结果不一致。
   - 排查事故时各方对“恢复到这个 LSN”理解不同。

11. **日志损坏后跳过坏 LSN**

   误用：扫描到中间 record 坏了，直接跳过继续 replay。

   症状：

   - 恢复出一个看似可用但内部不一致的数据库。
   - pageLSN、事务状态、索引状态对不上。
   - 后续错误比原始 checksum mismatch 更难排查。

12. **监控只展示一个 `lsn`**

   误用：监控面板只有 `lsn=xxx`，没有说明是 current、flush、replay 还是 checkpoint。

   症状：

   - 值班人员误判复制延迟。
   - 以为副本已应用，其实只是收到。
   - 以为 WAL 可删除，其实归档还没完成。

一句话：LSN 是恢复和复制坐标，不是业务语义。误用后最常见的线上症状是丢已 ack 数据、读副本旧值、WAL 清理过早、恢复缺日志、failover 后状态倒退。

## Q075. LSN 在单机和分布式环境中的语义有什么差异？

**回答：**

单机环境中，LSN 主要是本地 WAL 的顺序和恢复坐标；分布式环境中，LSN 通常只是某个节点、某个 shard 或某条日志流的本地进度。全局提交语义还要看复制协议、term/epoch、quorum、commit index 和 failover 规则。

**单机语义**

在单机数据库里，LSN 回答这些问题：

- WAL record 的顺序是什么？
- 数据页 pageLSN 到哪里？
- WAL durable 到哪里？
- checkpoint 从哪里恢复？
- 哪些 WAL 可以删除？
- 崩溃后 replay 到哪里？

单机里只要 WAL、数据页、checkpoint metadata 都在同一台机器或同一个存储语义下，LSN 的解释相对直接：

```text
commitLSN <= durableLSN -> 本地持久提交
pageLSN <= durableLSN -> 数据页可以安全刷盘
recovery starts at checkpointRedoLSN
```

它主要服务本地 crash recovery 和本地持久性。

**分布式语义**

分布式系统多了几个问题：

1. **LSN 是否全局可比**

   如果每个 shard 独立分配 LSN，那么：

   ```text
   shardA LSN 1000
   shardB LSN 1000
   ```

   这两个值不能直接比较。它们只在各自日志流里有序。

2. **本地 durable 不等于集群 committed**

   leader 本地 WAL fsync 到 LSN 1000，只说明 leader 本地可恢复。是否可以对外承诺，还要看复制策略：

   - 异步复制：可能本地 durable 后就 ack。
   - 同步复制：可能要等一个或多个副本 durable。
   - 共识复制：可能要多数派确认并推进 commit index。

3. **副本进度有多条线**

   分布式复制通常至少有：

   - leader current LSN。
   - sent LSN。
   - follower received LSN。
   - follower flushed LSN。
   - follower replayed LSN。
   - cluster committed LSN 或 commit index。

   这些不能混。收到不等于刷盘，刷盘不等于应用，应用不等于对外可读。

4. **term/epoch 决定领导权边界**

   LSN 只表示位置，不表示谁有权写。旧 leader 即使能生成更大的 LSN，也不能代表这些日志有效。分布式系统需要 term、epoch、lease 或 fencing 防止旧 leader 写入。

5. **failover 语义**

   新 leader 必须包含已承诺的日志范围。只比较 LSN 大小不够，还要确认它属于正确 term/timeline，并且经过复制协议承诺。

6. **跨节点快照**

   单机 snapshot 可以用一个 snapshot LSN 描述。分布式 snapshot 可能需要每个 shard 一个 LSN，或者使用全局一致性协议生成一组边界：

   ```text
   shardA: LSN 1200
   shardB: LSN 980
   shardC: LSN 3100
   ```

   这组 LSN 才共同描述一个分布式快照。

**对比表**

| 维度 | 单机 LSN | 分布式 LSN |
|---|---|---|
| 作用域 | 本地 WAL stream | 节点、shard、partition 或复制组 |
| 提交判断 | commitLSN <= durableLSN | 还要看 quorum、term、commit index、同步策略 |
| 恢复目标 | 本机 crash recovery | 副本恢复、leader 切换、日志截断、分布式快照 |
| 可比性 | 同一 WAL 内可比 | 跨 shard 默认不可比 |
| 风险 | durable/written 混淆、checkpoint 过新 | failover 丢数据、旧 leader 写入、读副本落后 |
| 辅助字段 | pageLSN、checkpointLSN | term、epoch、timeline、replica progress、commit index |

**一个典型例子**

leader 本地 durable 到 LSN 1000，客户端也收到 ack。但如果系统是异步复制，leader 立刻故障，新的 leader 只收到 LSN 900，那么 LSN 1000 的事务可能丢失。这里不是 LSN 失效，而是提交语义只保证了 leader 本地持久，没有保证集群多数派持久。

共识系统会把语义改成：

```text
entry replicated to majority
commit index advanced
state machine applied
then reply to client
```

这时数据库 LSN 可能仍然存在，但它要和共识日志 index、term 建立映射。

一句话：单机 LSN 是本地恢复坐标；分布式 LSN 还要放进复制和领导权协议里解释。只说“LSN 到了 1000”不够，必须说清楚哪个节点、哪个日志流、是否持久、是否复制、是否已提交、是否已应用。

## Q076. checkpoint 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

checkpoint 的核心目标是给恢复建立一个可靠基线，让系统不必从日志开头开始 replay。它同时解决正确性和性能问题：正确性体现在恢复有明确起点和一致边界，性能体现在缩短恢复时间、推动 WAL 回收、限制日志无限增长。它不是安全机制。

在 WAL 系统里，如果一直只写日志而不做 checkpoint，崩溃后恢复就要从很早的位置扫描和 replay。系统运行越久，恢复越慢。checkpoint 做的事情可以概括为：

```text
把某个日志位置之前的状态，尽量固化到数据文件或稳定快照中；
记录恢复还需要从哪个 LSN 开始；
允许更早的 WAL 在满足复制、归档、备份约束后被回收。
```

**它解决正确性**

checkpoint 不是简单写一个“我到这里了”的标记。它必须保证：如果崩溃发生，恢复程序从 checkpoint 指定的位置开始，仍然能恢复到一致状态。

正确性体现在：

1. **恢复起点可靠**

   checkpoint 记录 redo LSN。这个 LSN 必须不晚于所有可能需要 redo 的最早修改。过新会漏恢复。

2. **脏页状态可解释**

   fuzzy checkpoint 期间系统还在写入。checkpoint 要么记录 dirty page table，要么用保守 redo LSN 覆盖这些并发修改。

3. **事务状态可恢复**

   如果系统支持事务，checkpoint 可能要记录活跃事务、lastLSN、undo 边界等信息，帮助恢复判断 winner/loser。

4. **metadata 原子性**

   checkpoint metadata 自身不能写一半就被当成有效。通常需要 checksum、version、双文件、rename 或日志化 metadata。

5. **WAL 保留安全**

   checkpoint 完成前，不能删除仍可能被恢复需要的 WAL。

**它改善性能**

checkpoint 的性能价值很明显：

1. **缩短恢复时间**

   恢复从最近可靠 checkpoint 附近开始，而不是从系统创建时开始。

2. **推动 WAL 回收**

   checkpoint 向前推进后，早期 WAL 可能不再被本地恢复需要。结合归档和复制进度，系统可以回收空间。

3. **控制 dirty page 数量**

   checkpoint 会推动脏页写回，避免崩溃时需要处理过大的脏页集合。

4. **限制 checkpoint age**

   数据库常用 checkpoint age 控制“最老未固化修改”距离当前 WAL 的距离，防止恢复窗口无限扩大。

**它也帮助可维护性**

有 checkpoint 后，运维和排障能看到清晰边界：

- 上次 checkpoint 在哪个 LSN。
- redo 要从哪里开始。
- checkpoint 多久一次。
- checkpoint 写了多少脏页。
- WAL 为什么不能回收。

这些指标让恢复问题更容易解释。

**它不是安全机制**

checkpoint 不能防恶意篡改，不能证明操作者身份，也不能替代审计日志。它的 metadata 可以被校验，但那是完整性检查，不是权限控制。

一句话：checkpoint 是恢复基线。它让系统把“必须从头 replay 所有 WAL”变成“从一个可靠 LSN 之后 replay”，同时帮助控制恢复时间和 WAL 空间。

## Q077. checkpoint 的典型适用场景和不适用场景分别是什么？

**回答：**

checkpoint 适合用在任何“状态由日志驱动，但不能无限 replay 日志”的系统里。数据库、KV 存储、LSM 引擎、文件系统 journal、流处理状态后端都会用 checkpoint 或类似机制。它不适合当作事务提交证明、备份本身、审计日志或跨系统一致性的全部方案。

**典型适用场景**

1. **数据库 crash recovery**

   数据库通过 WAL 记录修改，通过 checkpoint 限定恢复起点。崩溃后从 checkpoint redo LSN 开始 replay。

2. **LSM/KV 存储**

   memtable flush、manifest 更新、SST 文件集合稳定后，可以形成恢复基线。WAL 只需要覆盖未 flush 的 memtable。

3. **文件系统 journal**

   journal 记录 metadata 或 data 变化，checkpoint 把 journal 中的已提交修改写回主文件系统区域，然后回收 journal 空间。

4. **流处理状态**

   流处理框架会周期性保存 operator state。失败后从 checkpoint 状态恢复，再从对应输入 offset 继续消费。

5. **复制和快照**

   副本可以从某个 checkpoint/snapshot 加后续日志追赶，而不是从最早日志开始同步。

6. **PITR 和备份边界**

   checkpoint 可以辅助确定备份恢复需要的 WAL 范围。注意它不是备份本身，但会参与恢复边界计算。

7. **控制恢复时间目标**

   如果系统要求重启恢复在 30 秒内完成，就需要根据写入速率、WAL replay 速度和 checkpoint 间隔来设计 checkpoint 策略。

**不适用或不能单独依赖的场景**

1. **不能把 checkpoint 当事务 commit**

   事务提交取决于 commit record 和持久化策略。checkpoint 只是之后可能把这些修改固化到数据文件。

2. **不能把 checkpoint 当完整备份**

   checkpoint 可能只是一个恢复起点，不一定包含所有数据文件的一致拷贝。真正备份还需要数据文件、WAL 范围、manifest 和校验。

3. **不能把 checkpoint 当审计日志**

   checkpoint 记录恢复边界，不记录完整业务语义。它不能回答“谁在什么上下文做了什么操作”。

4. **不能替代复制协议**

   分布式系统里，某个节点 checkpoint 了，不代表其他节点同意这段历史。全局提交还要看复制或共识。

5. **不能修复已经损坏的数据**

   checkpoint 可能把错误状态固化下来。它不是纠错机制。需要 checksum、备份、副本、scrub、恢复工具来发现和修复损坏。

6. **不适合过于频繁地强制执行**

   checkpoint 太频繁会造成大量脏页写回，拖慢前台写入。尤其在写密集系统里，频繁 checkpoint 会制造 I/O 峰值。

7. **不适合过于稀疏**

   checkpoint 太少会导致 WAL 积压、恢复时间过长、磁盘空间压力增大。

判断 checkpoint 是否适合，可以问一句：系统是否有一条持续增长的日志，并且需要把某个点之前的状态压缩成可恢复基线？如果是，checkpoint 很可能需要。如果只是要记录业务事实、做权限判断或证明用户行为，就不是 checkpoint 的职责。

## Q078. checkpoint 和相近概念最容易混淆的边界在哪里？

**回答：**

checkpoint 最容易和 snapshot、backup、commit、fsync、flush、WAL truncate、compaction、savepoint、restore point 混在一起。它们都和“某个点”有关，但语义差别很大。

| 概念 | 核心含义 | 和 checkpoint 的边界 |
|---|---|---|
| checkpoint | 恢复基线，记录从哪里开始 redo | 不一定是完整数据副本 |
| snapshot | 某个时间点的状态视图或状态副本 | snapshot 可以作为 checkpoint 材料，但不等同 |
| backup | 可离线保存和迁移的恢复材料 | backup 需要数据、WAL、manifest、校验 |
| commit | 事务提交边界 | commit 是事务语义，checkpoint 是恢复优化 |
| fsync | 把文件数据刷到稳定存储 | fsync 是 I/O 动作，checkpoint 是恢复协议 |
| flush | 把脏页或日志写出 | flush 是动作，checkpoint 是带语义的边界 |
| WAL truncate | 删除或复用旧 WAL | 只有在 checkpoint 和其他依赖允许后才能做 |
| compaction | 重写/合并数据布局 | compaction 改存储形态，不一定定义恢复起点 |
| savepoint | 事务内部回滚点 | savepoint 面向单个事务，不是系统恢复基线 |
| restore point | 用户命名的恢复目标 | restore point 是恢复目标，不一定固化数据页 |

**checkpoint vs snapshot**

snapshot 是某个状态视图，checkpoint 是恢复协议中的基线。某些系统会用 snapshot 实现 checkpoint，比如状态机保存一份状态快照，然后日志从 snapshot LSN 后继续 replay。

但 checkpoint 可以是 fuzzy 的：它记录脏页表和 redo LSN，并不要求所有文件在同一瞬间物理一致。恢复靠 WAL 修正。

**checkpoint vs backup**

backup 要能被带走、保存、恢复。checkpoint 通常只服务当前系统的 crash recovery。

一个数据库 checkpoint 完成，不代表你已经有备份。机器整盘坏了，checkpoint metadata 也可能一起丢。

**checkpoint vs commit**

commit 是事务对外承诺。checkpoint 是后台把状态固化，缩短恢复。

事务可以在 checkpoint 前提交，也可以在 checkpoint 后提交。提交成功不要求等到数据页被 checkpoint 写回，只要求日志持久化满足提交策略。

**checkpoint vs fsync**

fsync 是系统调用或 I/O 语义。checkpoint 可能调用很多 fsync，也可能依赖存储引擎自己的刷盘机制。

说“做了 fsync”不等于“完成 checkpoint”。checkpoint 还要写 metadata、记录 redo LSN、确保 WAL 保留边界正确。

**checkpoint vs WAL truncate**

checkpoint 推进后，旧 WAL 可能可以删除，但不是马上一定能删。还要看：

- 归档是否完成。
- 副本是否追上。
- 备份是否仍需要。
- 复制 slot 是否保留旧位置。
- PITR 策略是否要求保留。

**checkpoint vs compaction**

LSM compaction 会把 SST 文件合并、删除旧版本、改善读性能。它可能影响恢复材料，但不是 checkpoint 本身。checkpoint 关心“恢复从哪里开始”，compaction 关心“数据文件如何重写和组织”。

**checkpoint vs savepoint**

savepoint 是事务内部回滚点，只影响一个事务。checkpoint 是系统级恢复基线，影响崩溃恢复和 WAL 保留。

一句话：checkpoint 的边界是“系统级恢复起点”。凡是事务语义、业务历史、完整备份、外部复制承诺，都不能只靠 checkpoint 解释。

## Q079. checkpoint 在高并发场景下可能出现哪些隐藏问题？

**回答：**

checkpoint 在高并发场景下最容易出问题的地方，是它一边要推动脏页落盘，一边不能阻塞前台写入，还要保证 redo LSN 保守正确。写入越猛，checkpoint 越容易从后台维护任务变成前台尾延迟来源。

常见隐藏问题如下。

1. **checkpoint I/O 峰值**

   checkpoint 要刷大量脏页。如果一次刷得太集中，会把磁盘写带宽占满，前台 WAL fsync 和数据读写都变慢。

   症状是周期性 p99 抖动：每到 checkpoint 附近，写延迟明显升高。

2. **checkpoint age 过大**

   前台写入速度超过 checkpoint 刷脏页速度，当前 LSN 和 checkpoint redo LSN 的距离越来越大。

   后果：

   - 恢复时间变长。
   - WAL 占用空间增长。
   - 日志空间接近上限后，前台写入被迫等待 checkpoint。

3. **脏页刷写和热点页竞争**

   checkpoint 刷页时需要读取页面状态、pageLSN、checksum，可能和前台修改同一批热点页竞争 latch。

4. **fuzzy checkpoint 边界复杂**

   checkpoint 期间系统还在写。dirty page table、recLSN、transaction table 必须一致地记录。并发更新如果处理不好，redo LSN 可能过新。

5. **WAL 回收被慢副本拖住**

   本地 checkpoint 已经推进，但慢副本、归档、备份仍需要旧 WAL。高并发写入下 WAL 生成很快，慢消费者会导致日志目录膨胀。

6. **checkpoint metadata 热点**

   多线程更新 dirty page table、flush queue、pageLSN、checkpoint progress，如果结构设计粗糙，会出现锁竞争。

7. **full-page write 放大**

   某些数据库在 checkpoint 后第一次修改页面会写 full-page image，以防 torn page。checkpoint 频繁时，full-page image 可能增加 WAL 写放大。

8. **后台刷脏页策略不平滑**

   如果系统平时刷得太少，到 checkpoint 截止点才集中刷，会产生 I/O 尖峰。更好的方式是持续、平滑地推进。

9. **大事务拖慢 checkpoint**

   大事务生成大量 WAL 和脏页，还可能持有事务状态。checkpoint 要保留足够日志和 undo/redo 信息，不能简单推进。

10. **监控只看 checkpoint 次数**

   checkpoint 次数正常，不代表健康。更重要的是 checkpoint age、sync time、write time、dirty page 数、WAL retained bytes、redo distance、checkpoint failure。

11. **写限流触发太晚**

   如果等 WAL 空间快满才限流，前台请求会突然集体卡住。高并发系统通常要提前根据 checkpoint age 做渐进限流。

12. **分布式 checkpoint 不一致**

   多 shard 系统如果各自 checkpoint，没有全局一致边界，恢复到“同一个业务时间点”会很困难。需要全局 snapshot、barrier 或每个 shard 的 LSN 集合。

一句话：高并发 checkpoint 的难点不是“能不能写一个 checkpoint record”，而是如何平滑刷脏页、保守推进 redo LSN、避免 WAL 堆积，并且不把后台恢复优化变成前台延迟尖刺。

## Q080. checkpoint 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

checkpoint 在故障场景下暴露的核心问题是：这个 checkpoint 到底完整不完整、可信不可信、能不能作为恢复起点。崩溃可能发生在 checkpoint 的任何一步，所以恢复程序必须能识别最后一个有效 checkpoint，而不是盲目相信最新文件。

常见边界条件如下。

1. **checkpoint metadata 写了一半**

   崩溃发生在 checkpoint 文件或 manifest 写入中途。重启后可能看到半个文件、旧 checksum、新长度、旧版本号混在一起。

   正确做法是使用 checksum、generation、双文件、临时文件加 rename，或者把 checkpoint metadata 本身写入 WAL。

2. **checkpoint record 写入了，但脏页没刷完**

   如果 checkpoint record 过早声明完成，恢复可能从过新的 redo LSN 开始，漏掉未落盘修改。

   checkpoint 只能在满足它声明的恢复条件后，才发布为有效。

3. **fuzzy checkpoint 中途崩溃**

   fuzzy checkpoint 允许系统继续写入。崩溃时，checkpoint 可能只完成了一部分。恢复程序要么使用上一个完整 checkpoint，要么使用本次 checkpoint 中已经可靠记录的 dirty page table 和 redo LSN。

4. **旧 checkpoint 仍然要可用**

   新 checkpoint 完整落盘前，旧 checkpoint 不能删除。否则新 checkpoint 坏了、旧 checkpoint 也没了，恢复起点丢失。

5. **WAL 先删后确认**

   如果系统在 checkpoint 真正安全前删除旧 WAL，崩溃后可能发现恢复缺日志。

   正确顺序是：checkpoint 完整可信，再根据归档、复制、备份边界回收 WAL。

6. **checkpoint LSN 和 WAL timeline 不匹配**

   主从切换、备份恢复、fork 新历史后，checkpoint 记录的 LSN 必须带 timeline/epoch。否则可能拿旧历史的 LSN 去解释新历史的 WAL。

7. **数据文件来自不同 checkpoint**

   崩溃或备份时，部分数据文件是新 checkpoint 后的状态，部分还是旧状态。WAL 必须覆盖这种不一致，否则恢复后索引和数据可能对不上。

8. **超时不代表 checkpoint 失败或成功**

   运维命令触发 checkpoint 后超时，后台 checkpoint 可能仍在运行，也可能已经完成但响应丢了。重试时要能查询 checkpoint generation 或 last checkpoint LSN，而不是盲目并发启动多个重任务。

9. **重试造成 checkpoint 风暴**

   监控或控制面看到 checkpoint 超时后频繁重试，多个 checkpoint 任务叠加，造成 I/O 风暴，前台写入延迟更高。

10. **重启后 checkpoint 进度回退**

   崩溃前内存里 checkpoint 已经推进到较新 LSN，但 metadata 没持久化。重启后回到上一个完整 checkpoint 是正常行为。关键是系统不能曾经删除上一个 checkpoint 需要的 WAL。

11. **checkpoint 和备份交错**

   备份过程中 checkpoint 可能推进，WAL 也可能回收。备份必须记录自己需要的 start/end LSN，并阻止相关 WAL 被删除。

12. **checkpoint 期间出现 I/O 错误**

   某些脏页写失败，checkpoint 不能装作完成。否则恢复会依赖不存在的持久状态。系统应记录失败、重试、报警或进入保护模式。

13. **partial page write**

   checkpoint 刷数据页时可能发生 torn page。恢复要依赖 page checksum、full-page image、doublewrite buffer 或类似机制。否则 checkpoint 可能把坏页固化到数据文件。

14. **恢复时选择错误 checkpoint**

   如果有多个 checkpoint 文件，恢复程序要选择最新且完整、checksum 正确、版本兼容、WAL 范围可用的 checkpoint。不能只按修改时间选最新。

面试里可以这样答：checkpoint 故障边界主要是“发布顺序”。只有当 checkpoint 声明的状态真的持久可恢复时，才能把它作为恢复起点；旧 checkpoint 和旧 WAL 要保留到新 checkpoint 被证明可用之后。

## Q081. checkpoint 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

checkpoint 的性能瓶颈最常来自 I/O，其次是内存和锁竞争。CPU 通常不是第一瓶颈，但在 checksum、压缩、page 遍历、脏页管理很重时会出现。网络只在分布式 checkpoint、远程存储、云盘、跨节点快照里成为主要因素。

**CPU 瓶颈**

checkpoint 会扫描脏页、计算 checksum、压缩页面、更新 metadata。有些系统还要生成 full-page image、校验 pageLSN、维护 dirty page table。

CPU 成本高时，常见现象是：

- checkpoint 线程 CPU 高。
- 脏页扫描慢。
- WAL 生成速度超过 checkpoint 推进速度。
- replay/checkpoint 相关后台线程占用前台 CPU。

不过多数 OLTP 系统里，checkpoint 更容易先卡在 I/O。

**内存瓶颈**

checkpoint 和 buffer pool/page cache 关系很紧。

1. **脏页太多**

   脏页积压越多，checkpoint 要处理的页面越多，恢复窗口也越大。

2. **clean page 不足**

   如果内存里大多是脏页，前台读写需要可替换页面时会被迫等待刷脏。

3. **dirty page table 过大**

   系统要维护 page id、recLSN、pageLSN、flush 状态。高并发写入下，这些元数据会膨胀。

4. **缓存污染**

   checkpoint 顺序扫描或刷写冷脏页，可能把热数据挤出缓存。

内存层面的症状是 buffer pool 命中率下降、dirty page ratio 高、前台请求等待 page cleaner。

**锁竞争瓶颈**

checkpoint 会访问很多共享结构：

- dirty page list。
- flush queue。
- buffer descriptor。
- page latch。
- WAL insert/flush 状态。
- checkpoint metadata。

如果 checkpoint 线程拿锁时间太长，前台事务修改页面、分配 LSN、刷 WAL 都可能被拖慢。

典型症状：

- p99 写延迟周期性升高。
- 线程等待 page latch 或 checkpoint lock。
- checkpoint 开始后，普通 update 变慢。
- 高并发下 checkpoint 完成时间越来越不稳定。

**I/O 瓶颈**

I/O 是 checkpoint 最常见瓶颈。

1. **大量随机脏页写回**

   checkpoint 要把分散在数据文件中的脏页写回。HDD 上随机写很慢，SSD/NVMe 好很多，但仍可能和前台 WAL fsync 争带宽。

2. **fsync 数据文件和目录**

   写完数据页后，还要确保相关文件状态持久。文件系统和存储设备的 flush 延迟会影响 checkpoint 完成时间。

3. **WAL 和数据页争用**

   前台提交需要 WAL fsync，checkpoint 需要刷数据页。二者共用设备时，checkpoint 会放大提交尾延迟。

4. **writeback 节奏不平滑**

   如果平时积累太多脏页，到 checkpoint 末尾集中刷，会出现 I/O 峰值。

5. **云盘抖动**

   云盘延迟不稳定时，checkpoint 时间会抖动，进而影响 WAL 回收和前台写入。

I/O 指标上可以看 checkpoint write time、sync time、I/O await、device utilization、WAL fsync latency、dirty pages written。

**网络瓶颈**

单机本地盘 checkpoint 通常没有网络瓶颈。下面这些场景会有：

1. **远程块存储**

   云盘或网络块设备把写入确认放到远端，checkpoint flush 延迟受网络影响。

2. **分布式 checkpoint**

   多节点需要对齐 checkpoint barrier，最慢节点会拖慢整体完成。

3. **远程对象存储**

   checkpoint/snapshot 上传到对象存储时，带宽、延迟、重试都会影响完成时间。

4. **跨区域复制**

   如果 checkpoint 还要保证远端副本或归档完成，网络 RTT 和带宽会成为关键。

**调优方向**

常见调优不是“把 checkpoint 关掉”，而是让它更平滑：

- 控制 checkpoint 间隔和最大 WAL 大小。
- 使用后台 page cleaner 平滑刷脏。
- 根据 checkpoint age 做渐进限流。
- 避免过高 dirty page ratio。
- 把 WAL 和数据文件放在更合适的存储上。
- 监控 checkpoint write time、sync time、WAL retained bytes、recovery time。
- 分布式场景中避免所有 shard 同时 checkpoint。

一句话：checkpoint 的瓶颈大多不是计算，而是“把大量已经发生的修改安全地推到稳定存储”。I/O 是主战场，锁和内存决定它会不会把前台请求一起拖下水。

## Q082. append-only 为什么适合高吞吐顺序写？

**回答：**

append-only 适合高吞吐，原因不是“追加”这个词听起来简单，而是它把存储设备最讨厌的随机覆盖，变成了更容易批处理的顺序追加。对 WAL、Kafka partition、LSM 的 memtable flush、对象 manifest 这类系统来说，这一点很实用。

普通随机更新会遇到几个问题：先定位旧位置，再读改写，可能还要更新索引和元数据；如果写入很小，设备和文件系统还要为很多细碎 I/O 付账。append-only 反过来处理：新记录只往文件尾部写，旧记录不原地覆盖。这样写入路径可以长得很短：

```text
encode record
append to current segment
update in-memory tail offset / LSN
按策略 fsync 或 group commit
```

它快在几个地方。

第一，顺序写更适合设备。HDD 上顺序写减少寻道；SSD 和 NVMe 虽然没有机械寻道，但也更喜欢较大、连续、可合并的写入。很多小写被攒成一批，提交成本就摊薄了。

第二，page cache 和文件系统更容易帮忙。buffered write 可以把尾部追加先吸收到 page cache，再由 writeback 合并写出。文件系统分配 extent 时，也更容易拿到连续空间。

第三，锁模型更简单。一个 WAL writer 或 partition leader 控制尾部 offset，不需要对大量旧位置做细粒度覆盖锁。并发请求可以先进入队列，最终按 log 顺序落下去。

第四，恢复路径清楚。扫描日志时，只要从头或从 checkpoint 后开始读 record，读到第一条不完整或校验失败的尾巴就停。相比“到处都有原地覆盖”的文件格式，append-only 更容易定义有效前缀。

但 append-only 不等于没有成本。它把更新成本推迟到了后面：旧版本会占空间，索引要指向最新 record，删除通常要写 tombstone，后台还要 retention、compaction 或 snapshot。也就是说，它是把前台写入路径做短，把清理和重写放到后台。

面试里我会这样总结：append-only 的吞吐优势来自顺序写、批量提交、少随机覆盖、易恢复。它不是免费午餐，代价是空间膨胀和后台 compaction；系统设计要证明后台能追上前台写入。

## Q083. WAL 为什么是很多存储系统的恢复基础？

**回答：**

WAL 能成为恢复基础，是因为它把“系统做过哪些修改”按确定顺序保存下来。崩溃后，内存没了，page cache 没了，后台 flush 可能只完成了一半；此时磁盘上的数据文件不一定完整，但 WAL 给恢复程序一个可以信任的顺序来源。

数据库的核心规则是 write-ahead：数据页持久化之前，描述这次修改的 WAL record 必须先持久化。这样崩溃后有两种情况都能处理：

```text
WAL 已落盘，数据页没落盘 -> redo
WAL 已落盘，数据页也落盘 -> 根据 pageLSN 跳过或确认
```

如果没有 WAL，系统要么每次提交都强制把所有数据页刷盘，要么崩溃后不知道哪些修改完成、哪些只完成一半。前者太慢，后者不可靠。

WAL 还有几个工程上的好处。

第一，它把提交路径压缩成“顺序写日志并同步”。数据页可以稍后刷，checkpoint 可以慢慢推进。前台事务不必等待所有随机 data page 写完。

第二，它给恢复提供了边界。checkpoint 记录“到哪里为止数据文件已经可靠”，WAL 从这个位置之后继续 replay。没有这个边界，恢复可能要扫很久，甚至不知道从哪里开始。

第三，它可以支撑复制和备份。很多数据库把 WAL 发给 standby，或者把 WAL 归档起来做 point-in-time recovery。只要 base backup 加连续 WAL 可用，就能恢复到某个目标时间点或 LSN 附近。

第四，它把错误处理具体化。partial record、CRC 失败、LSN 跳跃、commit record 缺失，都能变成恢复程序可以判定的状态，而不是靠猜。

不过 WAL 不是完整系统一致性的全部。它不替代事务隔离，不替代分布式共识，不替代业务幂等，也不替代 checksum。WAL 解决的是：本地持久化顺序和崩溃恢复依据。上层还要定义哪些事务算提交、哪些副本算提交、哪些外部副作用可以重放。

一句话回答：WAL 是恢复基础，因为它把易丢的内存修改，变成了稳定存储上的有序、可校验、可重放记录。恢复程序不需要相信崩溃时的数据文件状态，只需要相信 WAL 的有效前缀。

## Q084. redo log 和 event log 的语义有什么差异？

**回答：**

redo log 和 event log 都是日志，但服务对象不同。redo log 面向存储恢复，event log 面向业务事实。这个差异会影响格式、保留时间、可读性、兼容性和 replay 方式。

redo log 记录的是“为了让存储状态恢复一致，需要怎样重做一次修改”。它可以很底层，比如 page id、offset、旧值、新值、slot 修改、B-tree 分裂，也可以是 physiological logging：逻辑上定位某个页，物理上描述页内变化。redo log 通常不追求业务可读性，它追求恢复准确、快速、幂等。

event log 记录的是业务已经发生的事实，比如：

```text
OrderCreated
PaymentCaptured
InventoryReserved
ActorCommandApplied
WorkflowStepSucceeded
```

这些事件应该能被业务投影、审计、异步消费者理解。它们不只是为了修复磁盘页，而是系统对外语义的一部分。

几个差异很关键：

1. **抽象层级不同**

   redo log 可以记录“page 42 offset 128 写入 8 个字节”。event log 应该记录“订单已支付”。前者依赖存储布局，后者依赖领域模型。

2. **保留策略不同**

   redo log 在 checkpoint、安全归档、复制确认后可以删除或回收。event log 往往要保留更久，因为它可能是审计、回放、重建投影和外部订阅的事实来源。

3. **兼容要求不同**

   redo log 只要数据库内核能读懂即可；event log 可能被多个服务、多个版本、多个语言消费，schema evolution 更难。

4. **重放目标不同**

   redo replay 目标是把存储页恢复到一致状态。event replay 目标是重建业务状态或投影，不能重新触发邮件、扣款、发货这类外部副作用。

5. **可压缩性不同**

   redo log 可以在恢复边界之后删除。event log 如果承担审计语义，就不能随便丢历史；如果只用于当前状态投影，才可以结合 snapshot 或 compaction 缩短重放。

面试中我会提醒一个常见坑：不要把 event sourcing 的事件写成数据库 redo log。事件如果只描述“把余额字段从 100 改成 80”，业务含义就丢了；反过来，也不要拿业务事件直接当页级恢复日志，因为它未必包含足够的物理恢复信息。

## Q085. WAL record 是物理变更还是逻辑事件会影响什么？

**回答：**

WAL record 的抽象层级，会直接影响恢复速度、格式兼容、跨版本升级、复制方式和调试难度。

物理日志记录更接近磁盘布局。比如：

```text
page_id=7, offset=128, len=16, bytes=...
```

它的好处是恢复直接，redo 时找到页和偏移，把字节写回去即可。对崩溃恢复来说，这很可靠，也容易做 pageLSN 判断。缺点是它强依赖页面格式和存储布局。页面结构变了、压缩格式变了、索引组织变了，历史 WAL 的解释就会变复杂。

逻辑日志记录的是操作意图或领域事件。比如：

```text
insert key K value V
delete key K
transfer account A -> B amount 20
```

它更可读，也更容易跨物理布局变化传播。逻辑复制、CDC、event sourcing 更偏这种形式。缺点是 replay 时必须重新执行逻辑，可能依赖当前索引、约束、并发状态和业务代码。若执行环境和当时不一致，结果可能不一致。

很多数据库采用中间形态，也就是 physiological logging。它逻辑上定位到一个 page 或 record，物理上描述页内变化。ARIES 系统里常见这种设计：既避免完全物理日志过大，又避免完全逻辑日志 replay 太不确定。

这个选择会影响：

- **恢复确定性**：物理日志更确定，逻辑日志更依赖执行环境。
- **日志大小**：物理 full-page image 可能很大，逻辑事件可能更小，但不总是。
- **schema evolution**：逻辑事件要长期兼容；物理日志通常要求内核版本能读旧格式。
- **复制**：物理复制更像复制存储状态，逻辑复制更像复制业务变更。
- **审计**：逻辑事件适合审计，物理页改动很难给业务解释。
- **幂等方式**：物理日志常用 pageLSN 跳过重复应用；逻辑事件常用 event id、版本号、去重表或条件更新。

面试里可以这样答：WAL record 的层级越低，恢复越确定但越绑定存储实现；层级越高，语义越清楚但 replay 越依赖业务环境。工程上要先问“这个日志是给 crash recovery 用，还是给复制、审计、event sourcing 用”，再决定格式。

## Q086. LSN 如何串联写入顺序、恢复顺序和 checkpoint？

**回答：**

LSN 可以理解成 WAL 中的全局位置。它既是写入顺序的编号，也是恢复顺序的坐标，还是 checkpoint 判断“从哪里开始恢复”的依据。

在写入时，系统给每条 WAL record 分配一个递增 LSN。LSN 通常对应 WAL 文件中的字节位置或逻辑位置。事务提交时，commit record 也有自己的 LSN。系统维护几个重要水位：

```text
insert_lsn   已经分配/写入 WAL buffer 的位置
write_lsn    已经写到内核或文件的位置
flush_lsn    已经 fsync 到稳定存储的位置
durable_lsn  对外可以承诺持久的最高位置
replay_lsn   恢复或副本已经重放到的位置
```

不同系统名字不一样，但思想类似。

恢复时，LSN 决定顺序。WAL replay 必须按 LSN 从小到大处理，因为后面的 record 可能依赖前面的 record。一个数据页上通常也会保存 pageLSN，表示这个页已经包含到哪个 WAL 修改。恢复时如果看到：

```text
page.pageLSN >= record.LSN
```

这条 redo 很可能已经应用过，可以跳过。这样 replay 就能幂等。

checkpoint 也依赖 LSN。checkpoint 会记录一个 redo 起点，意思是：在这个 LSN 之前，相关数据页已经刷到一个可恢复边界；崩溃后不必从创世日志开始扫，而是从 checkpoint 指定的位置附近开始。数据库还会用 dirty page table 的 recLSN 找到最早仍可能需要 redo 的修改。

LSN 还把复制和归档串起来。standby 可以报告 replay_lsn，归档系统可以确认哪些 segment 已经安全保存，主库可以根据最小需要位置决定哪些 WAL 能回收。如果某个 replication slot 或归档任务落后，老 WAL 就不能删。

容易混淆的是：LSN 的“已写入”“已持久”“已提交”“已应用”不是一回事。record 分配了 LSN，不代表 fsync 了；WAL fsync 了，不代表事务一定 commit；commit record 持久了，也不代表数据页已经刷盘；副本收到 LSN，也不代表已经 replay。

面试里我会用一句话收束：LSN 是恢复协议的尺子。写入按它排序，数据页用它判断是否已应用，checkpoint 用它缩短恢复范围，复制和归档用它管理保留边界。

## Q087. partial record 如何通过 length、magic、CRC 检测？

**回答：**

partial record 是 WAL 最常见的崩溃边界之一。系统写一条 record 时，可能只写了 header，或者 body 写了一半，或者数据到了 page cache 但没到稳定存储。恢复程序必须能识别“有效前缀到哪里结束”，不能把坏尾巴当成真记录。

常见 record 格式会包含这些字段：

```text
magic
version
header_length
record_length
LSN / sequence
record_type
payload
CRC
```

`magic` 用来确认当前位置像一条 record 的起点。它不能单独证明 record 正确，但可以快速排除明显错位的数据。如果扫描到的位置 magic 不对，恢复程序通常认为有效日志到这里结束，或者在某些格式中尝试有限 resync。

`length` 用来判断 record 是否完整。比如 header 说 payload 是 4KB，但当前 segment 剩余只有 2KB，或者读取到 EOF，说明这是尾部 partial record。对 append-only WAL 来说，尾部 partial 通常可以截断；中间出现 partial 就严重得多，可能说明文件损坏或写入协议错误。

`CRC` 用来判断内容是否被写完整、是否 torn write、是否被误读。只检查 length 不够，因为 length 字段本身也可能是坏的。CRC 通常要覆盖 header 的关键字段和 payload，至少要覆盖 record type、length、LSN、payload，避免把错位内容误判为有效记录。

更稳妥的恢复逻辑一般是：

```text
read header
check magic/version/header length
check record_length 合理，不超过最大值
read full payload
check CRC
check LSN 连续或符合 segment 边界
record valid -> apply or enqueue replay
record invalid at tail -> stop and truncate tail
record invalid in middle -> fail recovery or进入人工修复
```

为什么 tail 可以截断？因为 append-only log 的恢复不变量通常是“有效前缀”。崩溃发生在最后一条或最后几条还没提交的 record 上，这些 record 没有跨过持久化边界，丢掉可以接受。中间坏掉不一样：如果前后都有有效记录，中间缺一段，顺序历史断了，继续 replay 会制造假状态。

还要注意 commit marker。某条数据 record CRC 正确，不代表事务已经提交。恢复时要看 commit record、durable LSN 或 group commit 边界。否则会把未提交修改也恢复出来。

面试里可以说：length 判断“够不够长”，magic 判断“像不像起点”，CRC 判断“内容有没有坏”。三者一起，才能把崩溃尾巴和真实记录区分开。

## Q088. 为什么 index 通常可以从 WAL 重建？

**回答：**

因为 WAL 或 append-only log 才是事实来源，index 只是为了快速查询而维护的派生结构。只要日志里保存了足够的信息，恢复时从头或从 checkpoint 后扫描 WAL，就能重新生成 index。

一个简单 KV log 的 record 可能长这样：

```text
Put(key, value, seq)
Delete(key, seq)
```

内存 index 可以是：

```text
key -> segment_id + offset + length + seq + deleted
```

如果进程崩溃，index 文件丢了，并不一定致命。恢复程序扫描 WAL：遇到 Put 就把 key 指向最新 offset；遇到 Delete 就标记 tombstone；遇到 seq 更旧的 record 就跳过。扫描结束后，内存 index 就回来了。

这也是很多存储系统把 index 当成 materialized view 的原因。index 很重要，但它不是源数据。源数据是 WAL 中按顺序排列的 record。只要 record 有 key、operation、sequence、length、CRC，index 就可重建。

当然，“可以重建”有前提：

- WAL record 必须包含构建 index 所需的字段。
- replay 顺序必须确定，通常按 LSN 或 seq。
- 删除要有 tombstone，否则恢复后无法区分“没出现过”和“出现后被删”。
- partial record 要能检测并截断。
- compacted segment 发布要有安全协议，不能让 index 指向还没持久化的新 segment。
- 如果 index 持久化到磁盘，它也要有版本和校验，不能盲目信任。

为什么还要保存 index 文件？因为全量扫描 WAL 成本高。生产系统通常会保存 checkpoint、snapshot、SSTable index、segment sparse index 等结构，加快启动。但这些结构坏了，应该有从日志或数据文件重建的路径。

面试里我会这样答：index 是加速结构，不应该比 WAL 更可信。恢复时优先相信 WAL 的有效前缀，用 WAL 重建 index；如果持久化 index 和 WAL 冲突，通常要以 WAL 和更高层的 manifest/commit marker 为准。

## Q089. WAL replay 为什么要幂等？

**回答：**

WAL replay 必须幂等，因为恢复过程本身也可能崩溃，而且数据页可能已经包含了一部分修改。恢复程序不能假设“每条日志从未应用过”。它要能安全地重复执行，重复执行后状态仍然正确。

最典型的场景是：

```text
record LSN=100 已经应用到 page P
page P 刷盘成功
系统崩溃
恢复时又从 checkpoint 扫到 LSN=100
```

如果 redo 操作不是幂等的，比如“余额减 20”被执行两次，状态就坏了。正确做法是让数据页带 pageLSN：如果 pageLSN 已经大于等于 record LSN，说明这个修改已经体现在页上，replay 跳过。

幂等也可以通过业务序列号实现。比如 actor command 有 `command_seq`，KV record 有 `seq`，event 有 `event_id`。replay 时先判断当前状态是否已经处理过这个序号，再决定是否应用。

WAL replay 不幂等会暴露在很多地方：

- 恢复到一半又崩溃，下次重复 replay。
- checkpoint 记录保守，导致从更早 LSN 开始 replay。
- data page 已经刷了，但 checkpoint 没更新。
- 主从复制中 follower 收到重复日志。
- 用户重试导致相同 idempotency key 的写入再次出现。

不过要分清内部状态和外部副作用。修改内存表、数据页、materialized view 可以设计成幂等；发邮件、扣款、调用外部 HTTP 不能在 replay 时直接重做。外部副作用要用 outbox、effect log、幂等键和投递状态隔离。

面试里可以说：WAL replay 的目标不是“刚好执行一次”，而是“执行一次或多次都到同一个结果”。存储恢复靠 pageLSN、LSN、sequence 和 checksum 保证这一点；业务事件 replay 还要避免重复触发外部动作。

## Q090. snapshot 和 checkpoint 如何减少 replay 成本？

**回答：**

snapshot 和 checkpoint 都是在告诉系统：不用从最早的日志开始重放了。区别是 checkpoint 更偏恢复边界，snapshot 更偏状态物化。两者经常一起出现。

checkpoint 的含义通常是：某个 LSN 之前的数据页或状态已经持久到一个可恢复位置。崩溃后，从 checkpoint 记录的 redo 起点之后开始扫描 WAL 就可以。它减少的是数据库恢复时需要检查和 redo 的日志范围。

snapshot 的含义是：把某个对象或整个状态机在某个 LSN/offset/index 的状态直接保存下来。恢复时先加载 snapshot，再 replay snapshot 之后的日志。比如 actor 系统里，前 10000 条命令已经折叠成一个 actor snapshot，重启时不必从第一条命令开始跑。

一个安全 snapshot 至少要带这些信息：

```text
snapshot_id
covered_stream / partition / actor_id
last_included_lsn 或 last_included_index
term / epoch / schema_version
checksum
生成时间和格式版本
```

没有 last_included_lsn，恢复程序不知道从哪里接着 replay；没有 checksum，不能判断 snapshot 是否写完整；没有格式版本，升级后很难读历史 snapshot。

发布顺序也重要。典型流程是：

```text
write snapshot.tmp
fsync snapshot.tmp
rename snapshot.tmp -> snapshot
fsync parent directory
write checkpoint/manifest 指向 snapshot
fsync checkpoint/manifest
```

如果先发布 manifest，再写完 snapshot，崩溃后 manifest 会指向一个不存在或半写的快照。

checkpoint 和 snapshot 的成本也不能忽略。它们减少 replay，却引入后台 I/O、CPU 序列化、锁竞争和存储占用。snapshot 太频繁，前台被拖慢；snapshot 太少，恢复很慢。好的系统会按日志长度、状态大小、恢复 SLO、后台 I/O 负载动态调整。

面试里可以这样答：checkpoint 缩短“从哪里开始恢复”，snapshot 缩短“恢复时要重建多少状态”。二者都必须带明确的日志位置，并通过原子发布协议保证不会指向半成品。

## Q091. log compaction 和 snapshot 的关系是什么？

**回答：**

log compaction 和 snapshot 都是在减少历史日志成本，但手段不同。compaction 重写日志，保留仍然有用的记录；snapshot 把状态直接物化，然后允许删除被覆盖的日志前缀。

以 KV 存储为例，日志里可能有：

```text
Put A=1
Put B=2
Put A=3
Delete B
Put C=4
```

如果系统只关心当前状态，compaction 后可以保留：

```text
Put A=3
Delete B   或在 tombstone 安全期后删除
Put C=4
```

它还是日志，只是去掉了被后续记录覆盖的旧版本。

snapshot 则会生成一个状态文件：

```text
snapshot at LSN=500: {A=3, C=4}
```

恢复时加载 snapshot，再 replay LSN 500 之后的新日志。旧日志可以在确认没有备份、复制、审计、PITR、慢消费者依赖之后回收。

二者的关系可以这样理解：

- compaction 适合保留“每个 key 的最新事实”或“仍需 replay 的最小日志集”。
- snapshot 适合保留“某一时刻完整状态”。
- compaction 后仍然可能需要 replay 一段日志。
- snapshot 后也可能继续对后续日志做 compaction。
- 两者都要保护安全删除边界，不能删除仍被恢复、复制、消费者或审计需要的历史。

Kafka 的 log compaction 是按 key 保留较新的值，适合 changelog topic 或状态表恢复；Raft 的 snapshot 是把状态机应用到某个 index 后的状态保存下来，并用 last included index/term 替代旧日志前缀；数据库 checkpoint 更像恢复边界，不一定保存完整业务状态。

面试中要避免一句“compaction 就是 snapshot”。不是。compaction 仍在日志层工作，snapshot 直接在状态层工作。它们都减少 replay 成本，但对审计、PITR、慢消费者和格式兼容的影响不同。

## Q092. Kafka log 的 offset 和数据库 WAL 的 LSN 有什么相似点？

**回答：**

Kafka offset 和数据库 WAL LSN 都是日志位置。它们把一串追加记录变成可以定位、恢复、重放和确认进度的序列。没有这个位置概念，系统就很难回答“我读到哪里了”“我恢复到哪里了”“哪些日志可以删除”。

相似点主要有这些：

1. **都是单调推进的位置**

   Kafka partition 内 offset 单调增加。数据库 WAL 中 LSN 也随日志写入向前推进。它们都能表达顺序。

2. **都能作为 replay 起点**

   Kafka consumer 从某个 offset 开始拉取；数据库恢复从 checkpoint 指定的 LSN 附近开始 redo。

3. **都能作为 checkpoint 进度**

   Kafka consumer 提交 offset，表示这个消费组处理到了哪里。数据库记录 checkpoint LSN、flush LSN、replay LSN，表示恢复和持久化进度。

4. **都影响 retention**

   Kafka 要考虑 retention、compaction、消费者滞后；数据库 WAL 要考虑 checkpoint、归档、replication slot、standby replay 进度。最慢的一方会拖住日志删除。

5. **都能帮助定位问题**

   出现重复消费、恢复卡住、复制延迟、日志膨胀时，offset/LSN 是排查坐标。

差异也要讲清楚。

Kafka offset 是 partition 内的公开消费位置。它主要服务消息读取、消费者进度和 broker 存储布局。不同 partition 的 offset 不能直接比较全局先后。

数据库 LSN 通常是数据库内部 WAL 的全局或实例级位置。它和事务提交、pageLSN、checkpoint、flush、redo、归档、复制都有关系。LSN 不只是“读到哪里”，还表示哪些数据页修改受哪些 WAL record 保护。

Kafka offset 提交通常是消费者语义：业务处理完再提交，或者批量提交。数据库 flush LSN 是持久化语义：WAL 到这个位置已经落到稳定存储。把这两者混在一起会出错。

一句话：Kafka offset 和 WAL LSN 都是日志坐标；Kafka offset 更偏消息消费进度，WAL LSN 更偏存储恢复和持久化顺序。

## Q093. Raft log 的 term/index 和 WAL 的 sequence 有什么差异？

**回答：**

WAL sequence 或 LSN 主要解决单机顺序和恢复；Raft 的 term/index 解决复制日志的一致性。它们都在给日志编号，但语义层级不一样。

单机 WAL 里，LSN 通常表示“这条记录在本机日志中的位置”。只要本机按顺序写、按顺序恢复，就能用 LSN 串联 pageLSN、checkpoint、flush_lsn、replay_lsn。它不需要处理多个 leader，也不需要证明多数节点都同意这个位置的内容。

Raft log 里，index 表示日志槽位，term 表示产生该条日志时的 leader 任期。两者一起用于解决这些问题：

- 某个 index 上的记录是不是同一个 leader 任期产生的。
- follower 的日志是否和 leader 匹配。
- 旧 leader 的未提交日志能否被新 leader 覆盖。
- 候选人的日志是否足够新，能不能赢得选举。
- 某条日志是否已经被多数派复制并 committed。

Raft 的 term 很关键。只有 index 不够，因为不同 leader 可能在同一个 index 上写过不同内容。term 让系统知道“这个位置的历史属于哪个领导任期”。Raft 的 log matching property 也依赖 index + term：如果两份日志在同一 index 上 term 相同，那么该 index 之前的日志也应该一致。

WAL sequence 一般不处理这种冲突。单机 WAL 没有多个 leader 同时写同一条日志的场景。如果系统做主从复制但没有共识，LSN 也只能说明某个主库的本地顺序，不能天然说明多数派承认。

所以面试里可以这样答：WAL LSN 是本地恢复坐标；Raft term/index 是分布式一致性坐标。Raft 关心“集群是否同意这个位置的命令”，WAL 关心“本机崩溃后能否按这个顺序恢复”。两者可以组合，但不能互相替代。

## Q094. Raft log 解决复制一致性，单机 WAL 解决崩溃恢复，这两者如何组合？

**回答：**

实际系统里常常两者都要。Raft 保证多个节点对同一串命令达成一致；本地 WAL 保证每个节点重启后不丢掉自己已经承诺过的日志和状态。一个负责跨节点一致性，一个负责单节点崩溃恢复。

一个简化写入路径是：

```text
client -> leader
leader append Raft entry to local stable log
leader send AppendEntries to followers
followers append to local stable log and reply
leader sees majority replicated -> mark committed
leader apply to state machine
leader reply client
```

这里的“append to stable log”在实现上就需要本地 WAL 或等价机制。否则 follower 回复成功后自己宕机，重启却找不到那条日志，Raft 的承诺就不稳。

组合时要分清几个位置：

- local append LSN：节点本地把 Raft entry 写到哪里。
- local fsync/durable：节点是否把这条 entry 持久化。
- replicated index：leader 已经发给哪些 follower。
- commit index：是否被多数派承认。
- applied index：状态机是否已经应用到这条命令。
- snapshot index：旧日志前缀是否被快照替代。

单机 WAL 只回答“这个节点重启后能不能找回本地日志”。Raft commit 回答“多数派是否保存了这条命令，并且未来 leader 必须包含它”。如果 leader 本地 fsync 成功但没有多数派，客户端通常不能认为提交；如果多数派内存接收但没有本地持久化，节点同时掉电后也可能丢。这取决于系统具体的持久化策略，但高可靠设计通常会要求参与确认的副本先持久化再 ack。

状态机也需要恢复。节点重启后，先读取本地 Raft log/WAL，恢复 currentTerm、votedFor、log entries、commit index 相关状态，再根据 committed entries 重新 apply 或加载 snapshot 后继续 apply。apply 也要幂等，避免 crash 后重复执行状态机修改。

面试里我会这样说：Raft 不是 fsync 的替代品，WAL 也不是 quorum 的替代品。Raft 把“哪条日志算集群提交”说清楚；WAL 把“每个副本重启后还记不记得自己说过的话”说清楚。可靠复制系统需要两层语义一起成立。

## Q095. event sourcing 为什么要求事件表示事实而不是命令？

**回答：**

因为 replay 时事件会被当作已经发生的历史来重建状态。如果事件记录的是命令，也就是“请做某件事”，重放时系统就会重新做决策，结果可能和当时不同。事件应该记录“已经发生了什么”，不是“希望发生什么”。

命令是意图：

```text
PlaceOrder
ReserveInventory
ChargeCreditCard
CancelWorkflow
```

它可能成功，也可能失败。处理命令时要检查余额、库存、权限、幂等键、当前状态，还可能调用外部系统。

事件是事实：

```text
OrderPlaced
InventoryReserved
PaymentCaptured
WorkflowCancelled
```

它表示系统当时已经接受了这个结果。replay 时不应该再次判断库存够不够，也不应该再次扣款；只需要把事实应用到状态上。

如果把命令当事件存，问题很快出现：

- 当时库存够，现在库存不够，replay 结果变了。
- 当时风控通过，现在规则变了，replay 失败。
- 当时外部支付成功，replay 又发起一次扣款。
- 当时命令被拒绝，但日志里只有命令，看不出业务事实。
- 投影无法区分“收到请求”和“请求已生效”。

事件还应该用过去式命名，带上决定后的结果和必要上下文。例如 `PaymentCaptured(amount=100, provider_txn_id=...)` 比 `CapturePayment(amount=100)` 更适合做事件。前者是事实，后者是动作请求。

面试里可以这样答：event sourcing 的日志是系统事实账本。命令可以被拒绝，事件不能被重新裁决。把事件写成事实，replay 才是重建历史；把事件写成命令，replay 就变成重新跑业务流程，结果不可控。

## Q096. event sourcing replay 如何避免重放外部副作用？

**回答：**

核心原则很简单：replay 只能重建内部状态，不能重新执行外部动作。发邮件、扣款、发货、调用第三方 API、推送消息，这些都不能因为重放历史事件而再来一次。

常见做法是把状态变化和外部副作用拆开。

事件处理器可以分成两类：

```text
pure projector: event -> internal state / read model
effect dispatcher: event -> outbox -> external system
```

replay 时只运行 pure projector。它更新本地状态、物化视图、缓存、索引，不访问外部系统。effect dispatcher 在正常在线处理时工作，并且要记录投递状态。

更稳妥的模式是 outbox：业务事务写入 event，同时写入 outbox record。后台 dispatcher 读取 outbox，给外部系统发送请求。每个请求带 idempotency key，比如 event_id 或 business_id。发送成功后记录 delivered 状态。replay event log 时，不重新生成未受控的外部请求；如果需要重建 outbox，也要根据 delivered 状态和目标系统幂等能力处理。

还有几条工程规则：

- event handler 默认应是纯函数，输入事件，输出状态变化。
- 外部调用必须有幂等键，目标端也要支持去重或幂等覆盖。
- replay 进程要有明确模式，比如 `replay=true`，禁止副作用 handler 注册。
- 已发出的 effect 要有独立日志，不能只靠内存标记。
- 如果事件中包含外部结果，比如 payment provider transaction id，replay 只使用这个结果，不重新请求支付。
- 投影失败可以重建；外部副作用失败要走补偿、重试或人工处理，不能靠无限 replay 硬打。

面试里我会说：event sourcing 的 replay 是“再计算”，不是“再执行世界”。内部状态可以重复计算，外部世界不能随便重复操作。把 outbox、幂等键、投递日志和 replay 模式分开，是避免事故的基本做法。

## Q097. WAL 归档如何支持 point-in-time recovery？

**回答：**

Point-in-time recovery 依赖两样东西：一个可用的 base backup，以及从这个 backup 之后连续可读的 WAL。base backup 给你一个历史时刻附近的物理数据目录，WAL 让你把它向前重放到目标时间、目标 LSN 或目标事务附近。

流程可以简化成：

```text
定期做 base backup
持续归档 WAL segment
恢复时还原某个 base backup
配置 restore_command 拉取归档 WAL
replay WAL 到目标时间点 / LSN / transaction
停止恢复并打开数据库
```

为什么只靠 base backup 不够？因为 base backup 可能持续一段时间，备份过程中数据文件不断变化，内部状态并不是一个完美瞬时快照。WAL 可以把这段时间以及之后的修改补齐，让恢复结果一致。

WAL 归档的关键要求是连续。只要中间缺一个 segment，恢复就可能断在那。系统要监控：

- archive_command 是否成功。
- 归档目录是否有完整 segment。
- 归档延迟是否超过 RPO。
- old WAL 是否在归档前被回收。
- restore_command 找不到文件时是正常结束还是异常缺失。
- timeline history 是否保存，避免 failover 后恢复到错误时间线。

PITR 的目标点也要小心。恢复到“某个时间”不是业务上的绝对精确，取决于 WAL 记录的时间戳、事务提交顺序和恢复目标设置。更严格时会用 LSN、restore point 或事务 ID。

面试里可以这样答：WAL 归档把数据库从“只能恢复到最近备份”提升到“可以从备份继续向前重放”。base backup 是起点，连续 WAL 是时间轴；缺任意一段 WAL，这条时间轴就断了。

## Q098. WAL 格式变更如何影响历史可恢复性？

**回答：**

WAL 格式变更会直接影响历史日志还能不能被读取。崩溃恢复、PITR、复制、备份校验都依赖“当前恢复程序能解释历史 WAL”。如果新版本不再认识旧 record，或者同一个 record type 的含义变了，历史可恢复性就会出问题。

安全的 WAL 格式通常会带这些字段：

```text
magic
format_version
record_type
record_length
feature_flags
schema_version 或 payload_version
CRC
```

版本字段不是摆设。恢复程序读到旧版本 record 时，要么知道如何解析，要么明确拒绝并给出升级边界。最危险的是静默误读：字段位置变了，CRC 又没有覆盖足够内容，恢复程序把旧 payload 当成新 payload 应用，数据就会坏得很隐蔽。

格式升级要考虑几个场景。

第一，进程升级后立即崩溃，磁盘上可能同时有旧格式和新格式 WAL。恢复程序必须能处理这个混合区间，或者升级时先创建明确的 checkpoint/upgrade record，保证边界清楚。

第二，PITR 可能要恢复几个月前的 base backup。新版本数据库能不能用旧 WAL 恢复？如果不能，文档和运维工具要要求使用匹配版本恢复，再做升级。

第三，复制链路中可能有旧 standby。主库写了新 WAL record，旧 standby 读不懂，就会断复制。需要版本协商或禁止不兼容拓扑。

第四，event log 的兼容更久。业务事件可能被审计、投影、外部消费者长期读取。这里不能只保留一个内核 decoder，通常要做 schema evolution、默认值、向前/向后兼容和迁移工具。

比较稳妥的策略是：

- 新增 record type，不随意改变旧 type 语义。
- payload 自描述或带版本。
- CRC 覆盖版本、类型、长度和 payload。
- 在 checkpoint 或 snapshot 里记录生成版本。
- 升级前确保旧 WAL 已 checkpoint 或归档策略明确。
- 保留旧 decoder，至少保留到所有可能恢复窗口结束。
- 对未知 record 默认 fail fast，除非它被明确标记为可跳过。

面试里可以说：WAL 是恢复协议的一部分，不是普通文件格式。格式变更如果没有版本和兼容策略，就是在赌历史永远不会被重放。可靠系统不能这样赌。

## Q099. append-only 带来的存储膨胀如何通过 retention 和 compaction 控制？

**回答：**

append-only 不覆盖旧数据，所以写得越久，历史版本、删除标记、过期事件、旧 segment 都会堆起来。控制空间通常靠两类手段：retention 决定“哪些日志可以删”，compaction 决定“哪些历史可以折叠”。

retention 是保留策略。常见维度有：

- 按时间保留，比如保留 7 天。
- 按大小保留，比如每个 topic 或 WAL 最多保留 1TB。
- 按 checkpoint 保留，checkpoint 之前且不再需要恢复的 WAL 可以回收。
- 按消费者进度保留，慢消费者没读到的位置之前不能删。
- 按复制进度保留，standby 或 follower 没追上的日志不能删。
- 按归档状态保留，PITR 需要的 WAL 未归档前不能删。
- 按合规保留，审计日志即使没技术需要也不能提前删。

compaction 是重写策略。对 KV/changelog 来说，旧的 `Put(key)` 可以被新的 `Put(key)` 覆盖，`Delete(key)` 可以用 tombstone 表示，等所有消费者都越过安全点后再清理 tombstone。对 LSM 来说，compaction 会把多个 SSTable 合并，丢掉被覆盖版本和过期 tombstone。对 Raft 来说，snapshot 可以替代已提交并已应用的日志前缀。

两者必须受安全边界约束。不能因为磁盘快满就删掉还没归档的 WAL，也不能因为消费者慢就无限保留直到打爆磁盘。系统要有 backpressure、告警和降级策略。

一个比较稳的清理判断是：

```text
可删除位置 = min(
  checkpoint_safe_lsn,
  archived_lsn,
  all_required_replica_lsn,
  all_required_consumer_offset,
  legal_retention_boundary,
  snapshot_last_included_lsn
)
```

实际系统不一定所有项都有，但思想是取最保守的那个边界。

面试里可以这样答：append-only 把写入变简单，把空间治理变复杂。retention 负责删不再需要的历史，compaction 负责把仍需保留的状态压缩成更小形式；两者都必须尊重恢复、复制、消费和审计边界。

## Q100. 日志系统为什么需要 backpressure？

**回答：**

日志系统看起来只是不断 append，但它不是无限黑洞。只要生产速度长期超过 fsync、复制、归档、消费、compaction 或 snapshot 的处理速度，积压就会变成磁盘占满、恢复时间暴涨、p99 抖动，最后整个系统不可用。backpressure 的作用就是在还没崩之前把压力传回上游。

积压可能出现在很多地方：

- WAL fsync 变慢，commit 等待队列变长。
- follower 或 standby 复制落后，主库不能回收旧 WAL。
- archive_command 失败，归档目录缺 segment，`pg_wal` 越积越大。
- Kafka consumer 落后，broker 要保留更多 segment。
- compaction 跟不上写入，旧版本和 tombstone 堆积。
- snapshot 太慢，actor 或 Raft log replay 成本不断上升。
- 磁盘接近满，任何一次 segment rollover 都可能失败。

没有 backpressure，系统通常会先表现为延迟升高，然后是日志膨胀，再到写入失败。更糟的是，磁盘满常常会破坏恢复路径：新 WAL 写不进去，checkpoint 写不完，manifest 发布失败，归档也没空间。

backpressure 可以有很多层：

```text
限制 producer 写入速率
限制每租户 bytes/s 或 records/s
当 follower lag 超阈值时降低 leader 接收速度
当 archive lag 超阈值时阻塞高风险写入
当 compaction debt 过高时暂停低优先级写
当磁盘水位过高时拒绝新写入或切只读
让客户端收到明确的 retryable error
```

关键是不要只在最后一刻 ENOSPC。太晚了。更好的做法是分水位：warning、throttle、reject、read-only。每一档都有明确指标和恢复条件。

面试里我会说：日志系统的可靠性不只在写入成功那一刻，也在系统能否长期排掉写入债务。backpressure 是保护持久化路径的机制，不是单纯的性能限速。

## Q101. 如何在面试中把 WAL 从一个文件写入问题讲成系统一致性问题？

**回答：**

我会先承认 WAL 从表面看就是文件追加，但马上把问题拉到三个边界：提交边界、恢复边界、复制边界。只讲 `write()`、`fsync()`，最多是文件 I/O；讲清楚这三个边界，才是在讲系统一致性。

第一步，说明提交边界。客户端什么时候能收到成功？

```text
write 到 page cache 后？
WAL fsync 后？
commit record 持久后？
多数副本持久后？
外部副作用完成后？
```

不同答案对应不同一致性承诺。WAL 的价值是把本地 durable commit 收敛到“日志已持久到某个 LSN”。如果系统为了性能先 ack 再 fsync，就必须承认可丢失窗口。

第二步，说明恢复边界。崩溃后系统相信谁？成熟回答不是“重启后读文件”，而是：扫描 WAL 有效前缀，校验 length/magic/CRC，找到 checkpoint，按 LSN replay，利用 pageLSN 或 sequence 保证幂等，截断坏尾巴，重建 index 或 materialized view。

第三步，说明复制边界。单机 WAL 只能保证本机恢复；分布式系统还要回答多数派、leader epoch、Raft term/index、replication slot、consumer offset。Kafka offset、数据库 LSN、Raft index 都是日志位置，但它们分别服务消费进度、本地恢复和集群共识。

第四步，说明历史治理。append-only 让前台写快，但会带来 retention、compaction、snapshot、归档、PITR、schema evolution、backpressure。一个只会写日志、不知道何时删日志的系统，迟早会被日志拖垮。

可以用这段口述收尾：

```text
WAL 不是单纯把 bytes 追加到文件。它定义了系统从“请求进入内存”到“崩溃后仍可证明”的路径。LSN 给出顺序，fsync 给出本地持久化边界，checkpoint 和 snapshot 控制恢复成本，CRC 和长度处理坏尾巴，复制协议把本地日志提升成集群提交。性能优化可以做 group commit、batch、compaction，但不能偷换 ack 语义。
```

如果结合 LogServe 这类项目，我会说得更谨慎：它可以用 shared log 展示 log-first 控制面、replay、actor/workflow/LLM 状态恢复和不同 fsync 策略的 trade-off；但单机机制验证不等于生产级分布式数据库。这样答既能把技术链路讲完整，也不会夸大项目边界。

## 参考和校验点

- [PostgreSQL Documentation: Write-Ahead Logging](https://www.postgresql.org/docs/current/wal-intro.html) 说明 WAL 的核心规则：数据文件修改必须在对应 WAL record flush 到永久存储之后才能写入，并说明 WAL 如何支持 redo 和减少提交写入成本。
- [PostgreSQL Documentation: WAL Configuration](https://www.postgresql.org/docs/current/wal-configuration.html) 说明 checkpoint、redo record、WAL segment 回收、group commit、`commit_delay`、WAL flush 和 fsync 相关配置。
- [PostgreSQL Documentation: Write Ahead Log settings](https://www.postgresql.org/docs/current/runtime-config-wal.html) 说明 `fsync`、`synchronous_commit`、`wal_level`、`full_page_writes`、WAL archiving、checkpoint 和 WAL 保留相关配置，其中 `full_page_writes` 用于处理 crash 期间 page write 只完成一部分的风险。
- [C. Mohan et al., ARIES: A Transaction Recovery Method Supporting Fine-Granularity Locking and Partial Rollbacks Using Write-Ahead Logging](https://doi.org/10.1145/128765.128770) 是 ARIES 的经典论文，提出 Analysis、Redo、Undo、repeating history、CLR 等恢复机制。
- [Martin Fowler: Event Sourcing](https://martinfowler.com/eaaDev/EventSourcing.html) 说明 event sourcing 通过事件序列保存应用状态变化，并可用事件日志重建应用状态。
- [Kafka: a Distributed Messaging System for Log Processing](https://notes.stephenholiday.com/Kafka.pdf) 介绍 Kafka 的 topic partition、segment 文件、offset、顺序消费、page cache 和 retention 等设计。
- [LinkedIn Engineering: Intra-cluster Replication in Apache Kafka](https://engineering.linkedin.com/kafka/intra-cluster-replication-apache-kafka) 介绍 Kafka partition replica、leader/follower、ISR、high watermark 和 committed message 的复制语义。
- [Diego Ongaro and John Ousterhout: In Search of an Understandable Consensus Algorithm](https://raft.github.io/raft.pdf) 介绍 Raft 通过 replicated log、leader election、log replication、term、majority commit 和 state machine apply 来实现共识。
- [PostgreSQL Documentation: Continuous Archiving and Point-in-Time Recovery](https://www.postgresql.org/docs/current/continuous-archiving.html) 说明 base backup、WAL archive、continuous archiving 和 PITR 的关系。
- [MySQL 8.4 Reference Manual: The InnoDB Redo Log](https://dev.mysql.com/doc/refman/8.4/en/innodb-redo-log.html) 说明 InnoDB redo log 是用于 crash recovery 的磁盘数据结构。
- [MySQL 8.4 Reference Manual: Undo Logs](https://dev.mysql.com/doc/refman/8.4/en/innodb-undo-logs.html) 说明 InnoDB undo log record 保存如何撤销事务对 clustered index record 的最新修改，也用于 consistent read 获取未修改数据。
- [MySQL 8.4 Reference Manual: Doublewrite Buffer](https://dev.mysql.com/doc/refman/8.4/en/innodb-doublewrite-buffer.html) 说明 InnoDB doublewrite buffer 如何在页写入最终位置前保存副本，以便恢复 partial page write。
- [RocksDB Wiki: Write-Ahead Log](https://github.com/facebook/rocksdb/wiki/Write-Ahead-Log-%28WAL%29) 说明 RocksDB WAL、memtable、column family 和 WAL 生命周期的关系。
- [RocksDB Wiki: Checkpoints](https://github.com/facebook/rocksdb/wiki/Checkpoints) 说明 RocksDB checkpoint 如何为运行中的 RocksDB 数据库创建一致快照。
- [SQLite Documentation: Write-Ahead Logging](https://www.sqlite.org/wal.html) 说明 SQLite WAL mode、reader end mark、checkpoint 和 WAL 文件机制。
