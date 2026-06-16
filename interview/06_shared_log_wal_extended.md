# 二、Shared Log / WAL / LogStore（拓展：从 WAL 继续追问）

这一组适合面试官从 LogStore 继续往数据库、Kafka、Raft 和文件系统追。回答时要把边界说清楚：LogServe 的 shared log 借鉴了 WAL 和 event sourcing 的思想，但它不是数据库 WAL，也不是 Kafka 或 Raft 的完整替代品。

## Q126. 数据库 WAL 和 LogServe shared log 的共同点是什么？

共同点是：都把“可恢复的事实”先写到顺序日志里，再让别的状态从日志派生出来。

数据库 WAL 记录页更新或逻辑操作。事务提交前，相关 WAL 必须先落盘。数据库崩溃后，可以用 WAL redo 已提交事务，必要时 undo 未提交事务。

LogServe shared log 记录 runtime 事件，比如 `TaskSubmitted`、`WorkflowCompleted`、`ActorCommandApplied`、`LLMCompleted`。control 和 worker 可以重启，metadata view 可以丢，但只要 shared log 在，系统还能 replay 出 task、workflow、actor 和 LLM stats 的当前视图。

它们还都喜欢顺序写。顺序 append 比随机更新更容易做吞吐优化，也更容易解释恢复路径。两者都会关心 fsync、partial tail、checksum、checkpoint、truncation 这些问题。

差异也要马上补上。数据库 WAL 是数据库内部恢复机制，通常服务事务、页、锁和缓冲池。LogServe shared log 是应用层事件日志，事件本身有 runtime 语义，replay 逻辑在 LogServe 里。

## Q127. 数据库 WAL 的 redo/undo 与 LogServe replay 有什么区别？

数据库 WAL 的 redo/undo 是为了恢复数据库事务状态。redo 用来重放已经提交但还没刷到数据页的修改，undo 用来回滚崩溃前没提交的事务。它关注的是 ACID 里的原子性和持久性。

LogServe replay 更接近 event sourcing。它不是恢复 B+Tree page，也不是回滚半个 SQL 事务，而是按事件重建 materialized view。比如读 workflow stream 后，把 `StepScheduled`、`StepStarted`、`StepSucceeded` 依次应用到 workflow state。

LogServe 里通常没有数据库意义上的 undo。失败也是事件，比如 `TaskFailed` 或 `StepFailed`。系统不会把失败事件“撤销”，而是把它作为状态机转移的一部分。retry 会写新的 attempt 相关事件。

所以可以这样讲：WAL redo/undo 解决的是存储层事务恢复；LogServe replay 解决的是运行时状态重建。二者都依赖日志，但恢复对象不同。

## Q128. ARIES 中 LSN、pageLSN、checkpoint 的概念如何类比到 LogServe？

只能类比，不能说完全等价。

ARIES 里的 LSN 是 WAL 中每条日志记录的位置。它提供全局顺序。LogServe 当前没有全局 LSN，只有每个 stream 内的 `seq`。`actor:A` 的 seq 10 和 `wf:B` 的 seq 10 不能比较先后。如果要更像 ARIES，需要给每条 record 增加全局 log position。

pageLSN 表示某个数据页已经包含到哪个 LSN 的修改。LogServe 里可以类比成 materialized view 的 applied position，比如某个 workflow view 已经应用到 `wf:<id>` 的 seq 多少，某个 actor snapshot 覆盖到 command count 多少。当前实现没有把每个 view 的 applied seq 做成统一元数据，但 actor snapshot 和 trim 点已经有类似味道。

checkpoint 在 ARIES 里用来缩短恢复时间，告诉系统从哪里开始分析和 redo。LogServe 里 actor snapshot、LLM materialized stats、metadata bootstrap progress 都可以类比 checkpoint。它们的目标也是减少从头 replay 的成本。

面试里我会说：LogServe 有这些思想的简化版，但没有完整 ARIES 协议。

## Q129. WAL 的 write-ahead 原则在 LogServe 中对应哪条代码路径？

对应的是 log-first 路径：先写 shared log，再更新 metadata view。

比如普通 task 提交时，control 会先 append `TaskSubmitted` 到 `task:<task_id>`，append 成功后才创建 metadata task record 并入队。workflow、actor、model registry、scheduler policy、backpressure 配置也走类似路径。

worker 完成任务时也是这个方向。worker 或 control 先写 `TaskStarted`、`TaskCompleted`、`TaskFailed`、LLM 事件，再推进 metadata 状态。这样即使 metadata 更新失败，重启后也可以从 shared log 把 view 补回来。

在 LogStore 内部，`Append` 会先把 record bytes 写入 log file，再写 index file，最后更新内存 index、nextSeq 和 idempotency map。这里也体现了“事实先进入日志，派生结构后更新”的顺序。

## Q130. 为什么数据库通常先写 WAL 再刷数据页？LogServe 为什么先写事件再更新 metadata view？

数据库先写 WAL，是为了避免数据页上出现无法解释的修改。假设数据页先刷到了磁盘，但 WAL 没有持久化，数据库崩溃后就不知道这次修改属于哪个事务，也不知道该 redo 还是 undo。

LogServe 先写事件，也是同一个思路。如果 metadata 先更新成 `COMPLETED`，但 `TaskCompleted` 没写进 shared log，control 重启后从 log replay 会看不到完成事件，metadata 和 log 就分叉了。更糟的是，shared log 作为 source of truth 时，这个 metadata 更新没有历史依据。

所以 LogServe 的规则是：metadata 是 materialized view，不是事实源。view 可以晚一点更新，可以失败后重建，但不能比日志多出一段无法解释的状态。

这也是 log-first 的价值。它不是为了形式好看，而是为了让崩溃恢复有一条可信路径。

## Q131. fsync 到磁盘是否等于数据一定不会丢？硬件缓存和文件系统会有什么影响？

不能简单说“fsync 之后一定不会丢”。更准确的说法是：在操作系统和存储设备正确履行语义的前提下，`fsync` 要求把文件相关数据推到持久介质或设备承诺的持久层。

中间有很多细节。硬盘、SSD、RAID 卡可能有 volatile write cache。如果设备在没有电容保护的情况下先确认写入，再真正落盘，突然断电仍可能丢。文件系统也有不同 journaling 模式，数据和元数据的顺序不一定符合应用直觉。

还有目录项问题。创建新 segment、rename retention 文件后，只 fsync 文件本身不一定保证目录项持久。更严格的实现还要 fsync 目录。

所以面试里要说清楚：`fsync` 是本地持久性的基础手段，但不是魔法。生产环境还要看磁盘缓存策略、文件系统、挂载参数、云盘承诺、RAID 控制器和是否做多副本。

## Q132. O_APPEND 在并发写入下提供什么保证？跨平台语义是否一致？

在 POSIX 本地文件系统上，`O_APPEND` 的核心保证是每次 write 前由内核把文件 offset 移到文件末尾。多个进程并发写同一个 append-only 文件时，不会因为共享 offset 竞争而互相覆盖。

但它不等于“多次 write 组成一个事务”。如果一条 record 分多次 write，中间仍可能被别的 writer 插入。LogServe 当前每条 record 是一次 `Write(encoded)`，再加上 Store 的全局 mutex，所以不主要依赖 `O_APPEND` 来解决并发顺序。

跨平台要谨慎。不同文件系统、网络文件系统、Windows 文件 API 的 append 语义和原子性细节不完全一样。NFS 这类网络文件系统上，append 原子性尤其不能想当然。

LogServe 目前是单进程 logd 写文件，这比多进程直接写同一个 log 文件简单很多。多 writer 通过 gRPC 进入 logd，再由 logd 串行化。

## Q133. Linux page cache 对 append latency 和 crash consistency 有什么影响？

Linux 上普通文件写入通常先进入 page cache。`write()` 返回时，数据多数时候只是复制到了内核内存，还没真正到磁盘。这会让 append latency 很低，吞吐也高。

代价是 crash consistency。进程崩溃不一定丢 page cache，因为操作系统还活着；但机器断电或内核崩溃会丢掉没刷盘的 dirty pages。`fsync` 的作用就是把这些 dirty pages 推到存储设备，并等待完成。

page cache 还会影响 benchmark。batch/interval 看起来很快，是因为很多写入只进入缓存，不为每条记录等待磁盘。这个结果有价值，但要和 durability window 一起解释。

对 LogServe 来说，`FsyncAlways` 更接近“append 成功后本地可恢复”，batch/interval 更偏“先换吞吐，接受一小段可能丢失的窗口”。

## Q134. fdatasync 和 fsync 有什么区别？你会如何选择？

`fsync` 会同步文件数据和必要的文件元数据。`fdatasync` 更偏向同步文件数据，只同步恢复文件内容所必需的元数据，比如文件长度。

对 append-only log 来说，`fdatasync` 通常已经够用，因为我们关心的是 record bytes 和文件长度能恢复。访问时间、修改时间这类元数据没有必要每次刷。

但有两个地方要小心。第一，创建新 segment 文件后，目录项也要持久化，这不是 `fdatasync(logFile)` 能解决的。第二，rename `retention.json.tmp` 到 `retention.json` 后，如果要强断电语义，也要 sync 目录。

Go 的 `os.File.Sync()` 通常对应 `fsync`。LogServe 当前用它，简单但可能比 fdatasync 更重。生产化时可以提供选项：log data 用 fdatasync，segment create/rename 时额外 fsync directory。

## Q135. group commit 如何提升 WAL 吞吐？代价是什么？

group commit 的思路是把多个 append 的刷盘合并成一次 fsync。

如果每条日志都 fsync，一秒能做多少次写入基本受磁盘 flush 次数限制。group commit 让多个客户端请求先进入队列，writer 一次写一批 record，然后一次 fsync。这样 fsync 成本被多条记录平摊，吞吐会明显提高。

代价是延迟和语义。请求可能要等到下一批才返回，低负载下反而多了一点等待。batch 太大，p99 latency 会变差。batch 太小，吞吐提升有限。

还有 durability window。如果系统选择“写入 page cache 就返回，后台 group fsync”，那客户端收到成功后仍可能丢日志。如果选择“等本批 fsync 完成再返回”，语义更强，但延迟更高。

所以 group commit 的关键不是简单批量，而是明确返回成功代表什么。

## Q136. Kafka 的 segment/index 与你的 logstore 设计有什么相似之处？

相似点不少。Kafka 把 partition log 切成 segment 文件，每个 segment 有数据文件和索引文件。LogServe 也有 `segment-00000001.log` 和 `segment-00000001.index`，并按配置大小滚动 segment。

两者都把数据顺序追加到 log 文件，用 index 加速读取。segment rolling 后，旧 segment 基本变成 sealed 文件，后续可以围绕 retention 和 compaction 做文章。

差异在于索引模型。Kafka 的 offset 是 partition 内顺序，索引围绕 offset 到 file position。LogServe 当前是多个 stream 共享一组物理 segment，index entry 带 `stream_id + seq + segment_id + offset + length`。它更像在一个物理 append log 上建了 per-stream 二级索引。

Kafka 的生产级能力也完整得多，包括 replication、ISR、consumer offset、log compaction、成熟的磁盘管理。LogServe 目前是实验系统，重点是把 runtime state 的 source of truth 做清楚。

## Q137. Kafka 的 offset 和 LogServe 的 per-stream seq 有什么差异？

Kafka offset 是 partition 内的全序位置。一个 partition 里的消息有严格递增 offset，消费者按 offset 消费。不同 partition 的 offset 不能比较。

LogServe 的 `seq` 是 stream 内顺序。`task:<id>`、`wf:<id>`、`actor:<id>`、`llm:<task_id>` 都有自己的 seq，从 1 开始。它更细粒度，贴合 runtime 对象。

如果把 LogServe stream 类比 Kafka partition，会发现差异：Kafka partition 通常是物理分片和消费并行度单位；LogServe stream 是逻辑对象历史。很多 stream 共享同一组 segment 文件。

当前 LogServe 没有暴露全局 offset。物理文件 offset 存在于 index entry 里，但它不是对外语义。

## Q138. Pulsar 的 ledger/bookie 模型和单机 logd 有什么差异？

Pulsar 把消息存储交给 BookKeeper。topic 的数据会切成 ledger，ledger 分布在多个 bookie 上，通过 quorum 写入保证可靠性。broker 可以相对无状态，存储层负责复制和恢复。

LogServe 当前是单机 logd，写本地 segment 文件。它没有 ledger ensemble、write quorum、ack quorum，也没有 bookie 故障后的自动恢复。

Pulsar 的好处是存储和服务分离，多副本能力更成熟。代价是系统复杂度高，需要元数据服务、bookie 管理、ledger recovery。

如果 LogServe 要走云原生生产路线，可以借鉴 Pulsar/BookKeeper 的分层：control/worker 不直接承担日志可靠性，log service 后面接一个复制存储层。

## Q139. Raft log 和 shared log 的角色有什么差异？

Raft log 是共识协议里的复制命令日志。它的目标是让多个节点以同样顺序应用同一批命令，最终得到一致的状态机。

LogServe shared log 是应用层事件日志。它记录的是 task、workflow、actor、LLM 的运行事件，用来恢复 runtime view。

二者可以组合。用 Raft 实现 logd 时，`AppendLog` 请求会先变成 Raft command，复制到多数节点并 commit；commit 后再把事件放入 LogServe shared log 的可读视图。此时 Raft log 解决多副本一致性，LogServe event log 解决运行时语义。

不能把二者混为一谈。Raft log 是实现机制，shared log 是 LogServe 暴露给 runtime 的事实记录。

## Q140. 如果要让 logd 支持多副本复制，你会选择 Raft、Multi-Paxos 还是主从复制？

我会优先选 Raft。

原因很现实。Raft 的工程资料多，leader、term、commit index、snapshot、membership change 都有成熟实现和案例。对一个实验系统来说，Raft 更容易讲清楚，也更容易验证。

Multi-Paxos 能做到类似事情，但协议解释和工程调试更难。主从复制最简单，但如果没有 quorum 和明确的 failover 协议，leader 挂掉时容易丢已确认写入，或者产生 split-brain。

我的设计会是：每个 log shard 一个 Raft group，leader 接收 AppendLog，复制到多数节点，commit 后返回。以后 stream 可以按 hash 分到不同 shard，提高吞吐。

## Q141. Raft commit index 和 apply index 如何映射到 metadata view 更新？

Raft commit index 表示哪些 log entry 已经被多数节点确认，可以安全对外宣布。apply index 表示状态机已经把哪些已提交 entry 应用到本地状态。

映射到 LogServe，可以这样理解：commit index 对应 shared log 中已经 durable replicated 的事件边界；apply index 对应 control 的 materialized metadata view 已经处理到哪里。

AppendLog 应该在事件达到 commit index 后才返回成功。metadata view 更新应该发生在 apply 之后。也就是说，先 commit event，再让 projector 更新 task/workflow/actor/LLM stats。

如果 control 重启，它先从已 commit 的 shared log replay，重建 metadata view。未 commit 的 entry 不能被当作事实。这样能避免 leader 切换后出现 metadata 比 log 多的状态。

## Q142. 如果 logd leader 切换，客户端 idempotent append 如何避免重复？

幂等状态必须跟着复制日志一起恢复，不能只放在旧 leader 内存里。

客户端每次 append 带稳定 idempotency key。leader 把 `(stream_id, idempotency_key)` 作为状态机的一部分复制。新 leader 上任后，先应用已 commit 的日志，恢复 idempotency map。客户端重试同一请求时，新 leader 能识别 duplicate，并返回原 seq。

如果要更接近 Kafka 的 producer idempotence，还需要 producer id、producer epoch 和 per-stream sequence。epoch 用来 fencing 旧 producer，避免旧 leader 或旧客户端继续写。

简单说：idempotency key 解决“同一次逻辑请求重试”；producer epoch 解决“旧生产者复活后继续写”的问题。leader 切换场景通常两者都需要。

## Q143. 如果使用 quorum write，AppendLog 什么时候可以返回成功？

在强一致设计里，AppendLog 应该在日志 entry 被多数副本持久化并 commit 后返回成功。

更具体一点：leader 收到请求，分配 stream seq 或全局位置，把 entry 复制给 follower。多数节点确认写入后，leader 推进 commit index。只有到这一步，客户端才能拿到成功响应。

是否要求 follower fsync 后再 ack，是一个配置选择。最强语义是多数副本都 fsync 后 ack。弱一点是多数副本写入 page cache 就 ack，吞吐高一些，但断电风险更大。

我会把语义写进 API 文档：success 表示 entry 达到哪个 durability level。否则 benchmark 数字再好也不好解释。

## Q144. 如果 follower 落后，ReadLog 读 follower 是否可能读到旧数据？

会。落后的 follower 可能还没复制到最新 commit index，也可能复制了但还没 apply 到可读状态。

如果业务接受 eventually consistent read，可以允许读 follower，并在响应里带当前 applied index。客户端知道自己读到的不是最新。

如果要求强一致，读 follower 要做额外步骤。比如让 follower 向 leader 确认 read index，等自己 apply 到至少这个 index 后再返回。否则用户刚 append 成功，马上读 follower，可能看不到自己的写入。

LogServe 当前单机 logd 没这个问题。多副本后必须明确 read consistency。

## Q145. 如何在 shared log 上实现 linearizable read？

最直接的方法是所有强一致读都走 leader。leader 在处理 ReadLog 前，先确认自己仍是合法 leader，并且已经知道最新 commit index。Raft 里常见做法是 ReadIndex：leader 和多数节点通信确认 leadership，再等本地 apply 到对应 index，然后返回读结果。

也可以用 leader lease，但要非常小心时钟漂移。lease 适合优化，不能随便拿来当正确性证明。

读 follower 也可以做到 linearizable，但 follower 必须从 leader 获得 read index，并等待自己 apply 到该 index。否则就是 stale read。

对 LogServe 来说，可以把 `ReadLog` 分成两种语义：默认读可用性更好但可能旧；`ReadLog(linearizable=true)` 走 read-index 路径。这样使用者能按场景选择。

## Q146. 如何在 shared log 上实现 snapshot 和 log truncation？

多副本 shared log 的 snapshot 要包含两类信息。

第一是 Raft 或复制层状态机 snapshot。它记录状态机已经 apply 到哪个 log index，方便落后节点快速追上。

第二是 LogServe 语义层 snapshot，比如 actor snapshot、workflow materialized state checkpoint、LLM stats checkpoint、stream trim point。这些告诉 replay 不再需要某些旧事件。

truncation 不能只看单个节点。必须保证所有副本都已经安装了覆盖旧日志的 snapshot，或者可以从对象存储/备份拿到 snapshot。还要保证所有消费者的 min applied position 已经过了 truncation point。

当前 LogServe 已经有 actor snapshot 和 logical trim，但还没有复制层 snapshot，也没有物理删除 segment。生产化时要把这两层接起来。

## Q147. 如果多个 producer 向同一个 stream append，如何保证顺序？

需要一个唯一的 sequencer。当前单机 logd 通过 Store 的全局 mutex 串行化 append，并由服务端分配 `seq`。所以多个 producer 同时写同一个 stream，最后顺序由 logd 接收和处理顺序决定。

多副本后，sequencer 通常是 leader。所有同一 stream 的 append 都到 leader，leader 按日志顺序分配 seq。这样不会出现两个 producer 拿到同一个 seq。

如果要让客户端参与顺序控制，可以支持 `expected_seq`。客户端说“我期望下一条是 42”，服务端只有当前 nextSeq 等于 42 才接受。这个方式适合乐观并发控制，但会增加重试。

actor 这种有状态对象更严格。即使提交并发，actor mailbox 也要按 command_seq 串行应用。

## Q148. 如果需要 exactly-once producer，除了 idempotency_key 还需要 producer epoch 吗？

需要，至少在 producer 会重启、网络会分区、旧实例可能复活的场景下需要。

idempotency key 能识别同一次逻辑请求的重试。它解决的是“我刚才那次 append 到底成功没有”。但它不能天然阻止旧 producer 继续提交新的幂等键。

producer epoch 用来 fencing。每次 producer session 或 ownership 变更，epoch 增加。服务端只接受当前 epoch 的请求，拒绝旧 epoch。这样旧 worker、旧 actor owner、旧 leader 恢复后，不能继续写入。

LogServe actor ownership 里已经有 epoch fencing 的思想。把这个思想下沉到 producer append 协议，可以让日志写入语义更接近严格 producer exactly-once。

## Q149. Kafka producer idempotence 与你的 idempotency map 有什么区别？

Kafka producer idempotence 是协议级能力。broker 给 producer 分配 producer id，producer 带 epoch 和 per-partition sequence number。broker 能检测重复 batch、乱序 batch，并用 epoch fencing 旧 producer。

LogServe 当前 idempotency map 更简单。它只记录 `stream_id + idempotency_key` 是否已经出现。命中就返回 duplicate。它不理解 producer session，不检查 producer sequence，也不处理 max in-flight request 带来的乱序问题。

优点是实现简单，适合 SDK 请求重试。缺点是语义较弱。它能防同一个逻辑事件重复写，不能完整表达“某个 producer 的第 N 条消息必须只写一次并保持顺序”。

如果要升级，可以引入 `producer_id`、`producer_epoch`、`producer_seq`，把 idempotency map 从 key lookup 扩展成 producer state table。

## Q150. LSM-tree 和 append-only log 都是顺序写，它们适合解决的问题有何不同？

LSM-tree 适合做 key-value 状态存储。写入先进入 WAL 和 memtable，后续通过 compaction 合并到 SSTable。它擅长 point lookup、range scan、按 key 更新和删除。

append-only log 适合保存事件历史。它强调顺序追加、按 offset 或 stream seq replay、保留“发生过什么”。它不擅长直接回答“某个 key 当前值是什么”，除非再建 materialized view。

LogServe 需要 shared log 保存事实，也需要 metadata store 保存当前视图。前者更像 append-only event log，后者可以用 PostgreSQL、RocksDB、Pebble 这类状态存储。

如果把 event log 直接塞进 LSM，也能做，但要小心：LSM compaction 会不断改写底层文件，天然不保留人类可理解的物理 append history。

## Q151. WAL、binlog、event log、audit log、message queue 的语义边界分别是什么？

WAL 是存储系统内部恢复日志。它主要服务 crash recovery，外部用户通常不直接消费。

binlog 通常是数据库对外复制或 CDC 日志，记录数据变更，用来主从复制、订阅变更、恢复到某个时间点。MySQL binlog 就是典型例子。

event log 是应用层事实记录。它记录业务或运行时事件，当前状态可以从事件 replay 得到。LogServe shared log 属于这一类。

audit log 是审计日志，重点是“谁在什么时候做了什么”。它强调不可抵赖、可查询、合规保留，不一定用于恢复系统状态。

message queue 是解耦生产者和消费者的传递系统。它可以持久化，也可以保留历史，但核心语义是投递和消费，不一定是 source of truth。

这些系统会重叠，但面试时要按用途区分。

## Q152. CRC、checksum、hash、MAC 在日志完整性校验中分别解决什么问题？

CRC 是面向误码检测的校验，比如磁盘 torn write、传输错误、bit flip。它快，但不防恶意篡改。

checksum 是泛称，可以指 CRC，也可以指更简单或更复杂的校验和。说 checksum 时最好说明算法。

cryptographic hash，比如 SHA-256，强调抗碰撞和抗篡改。攻击者很难构造另一个内容得到同样 hash。但如果攻击者可以同时改内容和 hash，本身仍然不够。

MAC，比如 HMAC-SHA256，用密钥计算。它能证明内容由持有密钥的一方生成，能防止不知道密钥的人伪造。审计、安全日志更需要 MAC 或签名。

LogServe 当前用 CRC32，定位是发现非恶意损坏。它不是防篡改审计机制。

## Q153. 如果要防篡改审计，需要 Merkle tree 或 hash chain 吗？

需要类似机制，至少要有 hash chain。

最简单的是每条 record 带 `prev_hash`，当前 hash 覆盖 header、payload 和 prev_hash。这样删除、插入、篡改任意一条记录，后续 hash 都对不上。

Merkle tree 适合批量证明。比如每个 segment 生成 Merkle root，把 root 写到外部可信存储或签名服务。之后要证明某条记录属于某个 segment，可以给出 Merkle proof。

真正防篡改还要考虑密钥和外部锚定。如果攻击者能改所有 segment 和所有 root 文件，单机 hash chain 也救不了。可以把 root 定期写到独立系统，或者用 KMS 签名。

对 LogServe 当前目标来说，CRC32 足够做恢复校验；审计合规是另一条能力线。

## Q154. 如果记录需要加密，应该加密 payload 还是整个 record？索引如何处理？

如果只加密 payload，stream_id、event_type、seq、timestamp 仍然可见。好处是 logstore 可以继续建索引、按 stream 读取、按 event type 调试。缺点是元数据会泄漏，比如哪个 actor 活跃、哪个模型被调用。

如果加密整个 record，隐私更强，但 logstore 很难做索引。它至少需要知道 stream_id 和长度，否则无法支持 `ReadLog(stream_id)`。实际工程里通常会保留少量路由字段明文，把敏感业务内容放到加密 payload。

更稳的方案是 envelope encryption。每个租户或 stream 有数据密钥，数据密钥由 KMS 管理。header 明文字段作为 AEAD 的 AAD 参与认证，payload 加密并认证。这样 header 被改也能在解密时发现。

索引要按明文字段建。如果连 stream_id 都敏感，可以存 stream_id 的稳定 hash，但调试和权限管理会复杂很多。

## Q155. 如果日志非常大，replay 时间过长，checkpoint 应该放在哪一层？

应该分层放。

log 层可以放索引 checkpoint、segment manifest、trim metadata，帮助快速定位和跳过已裁剪记录。但 log 层不应该理解每种业务状态。

runtime 层要放语义 snapshot。actor 有 actor snapshot，workflow 可以有 workflow state checkpoint，LLM scheduler 可以有 materialized stats checkpoint。恢复时先加载 snapshot，再 replay tail log。

metadata view 层也可以记录 applied position。比如某个 projector 已经处理到哪个 stream seq 或全局 LSN。重启时从 checkpoint 后继续，而不是从头扫。

经验上，checkpoint 应该尽量贴近状态所有者。actor 知道自己的内存状态怎么序列化，workflow engine 知道 DAG 状态怎么压缩，logstore 只负责保存和裁剪。

## Q156. 如果启用 physical compaction，如何保证 replay 不再需要被删除的事件？

核心条件是：被删除事件的效果已经被可靠 snapshot 覆盖，并且所有需要这些事件的消费者都已经越过对应位置。

对 actor 来说，要先确认 snapshot 对象存在、`ActorSnapshotCreated` 事件已写入并可恢复、trim point 持久化成功。只有 seq 小于 trim point 的命令才可删除。

对 workflow 来说，要有 workflow state checkpoint，里面包含所有已完成 step、结果引用、失败状态和 retry 信息。否则删掉早期 step 事件后，恢复会丢状态。

还要处理多消费者。dashboard、审计工具、离线分析可能仍想读完整历史。如果这些是产品需求，就不能直接物理删除，或者要先导出到冷存储。

所以 physical compaction 需要一个明确的 compaction barrier：哪些状态已经被 snapshot 覆盖，哪些消费者已经确认不再需要旧事件。

## Q157. 如果支持事务性 append 多个 stream，logstore 需要什么协议？

需要让多个 stream 的事件具备原子可见性：要么都出现，要么都不出现。

单机单 logstore 可以用一个事务 envelope。比如写 `TxnBegin(txn_id)`，再写多条 `TxnRecord`，最后写 `TxnCommit`。读路径只有看到 commit 才暴露这些 records。恢复时遇到没有 commit 的 txn，直接忽略。

如果多个 stream 分布在不同 shard，就需要分布式事务协议。最基本是两阶段提交：prepare、commit。每个 shard 先持久化 prepare，协调者决定 commit 后再写 commit marker。

还需要处理幂等和恢复。协调者崩溃后，参与者看到 prepared 事务不能随便丢，也不能随便提交。要有事务状态日志或共识层托管事务决定。

当前 LogServe 没有跨 stream 原子 append。它依赖上层事件设计避免强跨 stream 事务。

## Q158. 如果 AppendLog 需要跨 stream 原子性，二阶段提交是否足够？

二阶段提交能提供原子提交，但不解决所有问题。

它的经典问题是阻塞。如果协调者在参与者 prepare 之后崩溃，参与者不知道最终该 commit 还是 abort，只能等待协调者恢复，除非还有额外的共识机制保存决定。

在单机 logstore 内，其实不一定需要 2PC。一个全局 append lock 加事务 commit marker 就能实现多 stream 原子可见。

跨多个 log shard 时，2PC 是起点，但为了可用性，协调者状态最好放进 Raft 这样的共识组。否则协调者单点会把系统卡住。

所以回答可以是：2PC 对原子性有帮助，但单独的 2PC 不够生产化。还要有持久事务状态、恢复流程、幂等事务 ID 和超时处理。

## Q159. 如果要支持高并发读写，mmap、io_uring、direct I/O 是否值得引入？

先不要急着引入。当前 LogStore 的瓶颈更可能是全局 mutex、单 active segment、fsync 策略和读 index 的线性扫描。先把这些改掉，收益更确定。

mmap 适合读多写少的 segment，能减少 read syscall，但 crash consistency 和边界管理更麻烦。写路径用 mmap 也要处理 msync、page fault、文件扩展。

io_uring 适合大量异步 I/O，能降低 syscall 开销。但 LogServe 当前是小规模单机实验，复杂度可能超过收益。

direct I/O 绕过 page cache，适合数据库自己管理缓存。append-only 小 record 场景下，它会带来对齐、批量和 buffer 管理成本，不一定划算。

我的路线会是：先做 writer goroutine、group commit、二分 read index、segment file handle cache。等这些做完还被系统调用和 page cache 卡住，再考虑 io_uring 或 mmap。

## Q160. 如果把 logstore 换成 RocksDB、Badger、Pebble，会失去或获得什么？

会获得成熟 KV 存储的能力。比如 crash recovery、WAL、block cache、Bloom filter、compaction、范围查询、批量写。Pebble/RocksDB 对工程边界处理得比自研文件格式成熟得多。

但会失去一部分可解释的 append-only 日志形态。LogServe 现在的 segment file 很直观：事件按写入顺序落在 log 里，index 可以重建，partial tail 可以解释。换成 LSM 后，底层文件会被 compaction 改写，物理历史不再直接对应事件历史。

可以折中：用 Pebble 存 metadata view、idempotency table、stream index；event log 仍然自己维护 append-only segment。也可以把每条事件作为 KV value 存进去，key 是 `(stream_id, seq)`，但这样更像事件表，不再是顺序物理 log。

如果目标是展示系统原理，自研 logstore 更有价值。如果目标是快速生产化，成熟 KV 会减少很多底层风险。

## Q161. 如果使用 PostgreSQL 作为事件日志，和自研 logstore 相比有什么取舍？

PostgreSQL 的好处是可靠。它已经有 WAL、事务、索引、备份、复制、权限、监控和 SQL 查询。用一张 append-only events 表保存 `stream_id, seq, event_type, idempotency_key, payload`，可以很快得到可用系统。

它还天然支持跨表事务。比如 append event 和更新 metadata view 可以在一个数据库事务里完成。这样开发简单，操作也熟悉。

代价是失去对日志存储路径的控制。segment rolling、partial tail、fsync policy、physical compaction、index rebuild 这些底层能力就变成 PostgreSQL 的内部机制。吞吐和尾延迟也受数据库配置影响。

对 LogServe 这个项目来说，自研 logstore 能展示 shared log 的核心原理。对真实线上服务，PostgreSQL event table 可能是更稳的第一版，后面再按瓶颈换专用 log service。

## Q162. 如何设计 WAL replay 的模糊测试和 crash consistency 测试？

我会先做一个内存 oracle。测试随机生成 append、read、trim、restart、duplicate append、corrupt tail 等操作。每次操作后，把 LogStore 的可见结果和 oracle 对比。

模糊测试要覆盖 payload 大小、stream 数量、segment size、fsync policy、idempotency key 是否重复、trim point 前后读取。特别要测跨 segment 的边界。

crash consistency 要在关键点注入崩溃：log write 后、index write 后、sync 前、sync 后、retention tmp 写完后、rename 前后。每个点都模拟进程退出，再重新 Open，检查恢复结果是否符合该 fsync policy 的承诺。

更接近真实的做法是独立进程跑 workload，外部用 kill -9 随机杀，再启动校验。断电语义更难，需要虚拟机快照、dm-flakey、文件系统 fault injection 或专门的 crash monkey。

测试目标不是证明永远不丢，而是证明“在文档承诺的 durability level 下，不会出现不可解释的状态”。

## Q163. 如何证明 partial tail truncate 不会破坏已确认写入的记录？

先定义“已确认写入”。在 `FsyncAlways` 语义下，AppendLog 返回成功应意味着该 record 的 log bytes 和 index sync 已完成。partial tail truncate 只能发生在最后一条完整记录之后，恢复扫描从 offset 0 开始，只有读到 corrupt record 才截断到当前 offset。

如果前面的 record 完整且 CRC 通过，扫描 offset 会推进到它的结尾。truncate 的位置是第一条坏记录的起点，不会覆盖前一条合法 record。

证明依赖两个条件。第一，record framing 能准确找到每条记录边界。第二，坏记录只出现在 tail，而不是 segment 中间。当前测试 `TestRecoveryTruncatesPartialTail` 覆盖的是这个场景。

如果 fsync policy 是 batch，Append 返回成功不等于 record 已经持久化。机器崩溃后最近的“成功 append”可能根本不在文件里。此时不能承诺 partial tail truncate 保留所有已返回成功的记录，只能按 batch 语义解释。

## Q164. 如果 fsync=batch，客户端收到成功后进程崩溃可能丢日志吗？这对语义意味着什么？

如果只是进程崩溃、操作系统还活着，page cache 里的数据可能仍会被写回磁盘。但如果是机器断电、内核崩溃或存储设备丢缓存，batch 模式下最近的成功 append 可能丢。

当前 batch 策略不会每次 append 后 sync，而是在关闭 store 或滚动文件等时机 sync。它的语义是提高吞吐，接受 durability window。

这对 LogServe 很重要。因为 shared log 是 source of truth。如果客户端收到成功，但日志在断电后丢了，replay 就恢复不出这次事件。metadata view 也不能被当作最终事实。

所以文档里要明确：如果要让 AppendLog 成功代表本地持久化，应使用 always 或明确配置强 durability。batch/interval 的成功更接近“logd 已接受并写入内核缓存”，不是严格持久提交。

## Q165. 如何向面试官准确描述 durability 与 throughput 的权衡？

我会直接用三句话讲。

`fsync=always`：每次 append 都等待磁盘同步，durability 语义强，但吞吐低、尾延迟高。

`fsync=batch`：多条日志共享一次刷盘成本，吞吐高，但客户端成功返回和真实持久化之间有窗口。崩溃类型不同，丢失风险不同。

`fsync=interval`：在固定时间间隔附近刷盘，把风险窗口控制在配置范围内。它是 always 和 batch 之间的折中。

然后拿实验结果支撑：在你的单机实验里，同样 20,000 条记录、16 个 stream、256 字节 payload，always 的 append throughput 约 1.7k records/s，batch/interval 到了二十多万 records/s。这个差距不是白来的，它换走的是每条 append 的强同步保证。

最后收口时不要说“哪个最好”。正确说法是：恢复语义强弱、用户能接受的丢失窗口、吞吐目标和硬件条件决定 fsync 策略。LogServe 把这个开关暴露出来，是为了让实验能看到这条曲线，而不是假装同时拿到最高吞吐和最强持久性。
