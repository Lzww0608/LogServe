# 十二、高频链式追问脚本：从 shared log / WAL 开始

这一组适合面试官沿着同一个点连续深挖。回答时可以按一条线展开：LogServe 把 shared log 作为事实来源，metadata view 只是从日志投影出来的状态；单机 logstore 已经实现 segment、index、CRC、恢复、逻辑 trim 和 fsync 策略；多机复制和物理压缩还属于后续生产化方向。

## Q836. 你说 shared log 是 source of truth，那它和数据库 WAL 的区别是什么？

它们的共同点是顺序追加，并且都服务于恢复。区别在语义层级。

数据库 WAL 主要是数据库内部机制。它记录的是数据库页、事务、redo/undo 相关信息，用来让数据库在崩溃后恢复到一致状态。应用一般不会直接依赖数据库 WAL 来解释业务状态。

LogServe 的 shared log 是系统对外定义的事件日志。里面记录的是 `TaskSubmitted`、`TaskStarted`、`TaskCompleted`、`StepSucceeded`、`ActorCommandApplied`、`LLMCompleted` 这类领域事件。控制面重启后，可以从这些事件重新构建 task、workflow、actor、LLM stats 和 dashboard view。

所以我说 shared log 是 source of truth，指的是 LogServe 自己的状态模型：事件日志比 metadata store 更基础。metadata store 是投影结果，丢了可以从 log 重建；log 丢了，很多状态就没有独立依据了。它不替代数据库 WAL，两者所处层级不同。

还有一个差异是顺序模型。数据库 WAL 通常围绕数据库事务和全局 LSN 展开。LogServe 当前是 per-stream 顺序，每个 `stream_id` 内有递增 `seq`，例如 `task:<task_id>`、`workflow:<workflow_id>`、`actor:<actor_id>`、`llm:<task_id>`。这和 workflow、actor、LLM 的恢复边界更贴合。

面试里我会这样总结：

> 数据库 WAL 是数据库恢复自己的内部日志；LogServe shared log 是 runtime 语义层的事件日志。它记录的是系统能 replay 的业务事件，而不是 page update。

## Q837. 既然类似 WAL，为什么不用数据库自带 WAL 或 Kafka？

数据库自带 WAL 不适合直接当 LogServe 的事件层。原因很简单：它不是稳定的应用 API。不同数据库 WAL 格式不同，升级后也可能变化；即使用 logical replication，也是在数据库表变更层面做 CDC，不是为 task lease、workflow replay、actor command、LLM scheduling 这些语义设计的。

如果把 metadata store 当成主事实来源，再从数据库 WAL 做恢复，就会把问题绕回数据库状态机本身。LogServe 的设计目标是先写事件，再更新 view。事件应该有清晰 schema、stream、seq、idempotency key 和 replay 语义，不能藏在数据库内部日志里。

Kafka 是另一种情况。Kafka 完全可以作为生产级 shared log 的候选，特别是多副本、消费位点、分区扩展这些能力已经很成熟。但在这个项目里，我实现了一个小型 logstore，有几个目的：

- 明确展示 log-first 语义，核心逻辑可以直接在项目代码里检查。
- 控制 per-stream seq、idempotent append、segment/index、CRC、tail truncate、logical trim 这些细节。
- 让 workflow、actor、LLM replay 和日志存储的边界能直接在代码里看到。
- 在单机实验环境里降低依赖，便于做 crash recovery、fsync policy、snapshot-aware retention 的实验。

如果走生产化，我不会排斥 Kafka 或者一个 Raft-backed log。当前自研 logstore 更像项目的最小可解释实现：它把这套 runtime 依赖的日志语义摊开了。

## Q838. 你的日志如何保证 crash consistency？

当前 logstore 的基本写入路径是：先把 record 编码成二进制格式，写入 segment `.log` 文件，再写入 `.index` 文件，然后按 fsync policy 决定是否同步到磁盘。`fsync=always` 下，每次 append 都会 sync log 和 index；`batch` 和 `interval` 会牺牲一部分崩溃后的确认语义，换吞吐。

record 里有 magic、version、长度字段、stream seq、timestamp 和 CRC32。恢复时不会信任 index 文件，而是扫描 segment log。每读到一条 record，就检查 magic/version、body 长度和 CRC。如果遇到半条记录、短读、CRC 不匹配，恢复逻辑会把文件截断到最后一条完整记录的位置，然后用扫描结果重建 index、next seq 和 idempotency map。

这套设计的重点是：log 文件是恢复依据，index 是派生数据。index 坏了可以重建，log tail 坏了可以截断到最后一个完整 record。

需要把边界讲清楚。`fsync=always` 下，AppendLog 返回成功后，系统尽力保证 record 已经落盘；真实机器上还会受文件系统、磁盘缓存、虚拟化层影响。`batch` 或 `interval` 下，AppendLog 可能在没有 sync 的情况下返回成功，进程或机器崩溃后有丢失最近记录的风险。这里体现的是 durability 和 throughput 的取舍。

## Q839. fsync=always 为什么慢？batch 为什么快？batch 会不会丢数据？

`fsync=always` 慢，因为每次 append 都要等待存储设备确认数据和元数据已经同步。这个等待通常比内存写、page cache 写慢几个数量级。更要命的是，它把很多小写放大成很多次同步等待，吞吐会被单次 fsync latency 限制住。

`batch` 快，是因为多条 append 可以共享一次 flush 成本。写入先进入 page cache，系统不在每条记录后马上等磁盘完成同步。这样 append path 主要是在做内存拷贝和顺序写，吞吐会高很多。

batch 会不会丢数据？会，至少当前语义下要承认这个风险。如果服务端在 batch 还没 sync 时崩溃，最近已经返回成功的 append 可能没有真正持久化。项目实验里 batch/interval 的吞吐明显高于 always，这个结果正是用耐久性换来的。

生产上可以有两种讲法。

一种是把 batch 定义成 weaker durability mode，文档里明确说明：返回成功表示服务端已接收并写入 OS 缓存，不等价于同步落盘。

另一种是做 group commit：一批请求一起等待同一次 fsync，fsync 成功后再给这些请求返回成功。这样比 always 吞吐高，但仍保留确认后的持久性。代价是单条请求可能多等一个 batch window。

## Q840. 如果只写了一半 record，恢复时怎么处理？

恢复时扫描 segment。读到一半 record 通常有几种表现：header 不完整、body 长度不够、CRC 对不上。当前实现会把它视为 corrupt tail，然后 truncate 到最后一个完整 record 的 offset。

举个例子，segment 里已经有 seq 1 到 seq 10，seq 11 写到一半时进程崩溃。重启后扫描到 seq 10 都合法，读 seq 11 时发现短读或 CRC 错误，就把文件截到 seq 10 结束的位置。接着 index 会从 seq 1 到 seq 10 重建，下一次 append 从 seq 11 继续。

这种处理对 tail corruption 是合理的，因为崩溃最常见的位置就在文件尾部。它避免了 index 指向不存在 payload，也避免了后续读取读到脏数据。

有一个边界必须承认：如果 corrupt record 出现在旧 segment 中间，而后面还有完整记录，当前实现按保守策略从 corrupt offset 截断，会丢掉同一 segment 后续记录。单机项目里这能让恢复路径简单可靠；生产系统更应该区分 tail corruption 和 middle corruption。middle corruption 更适合停止恢复、隔离 segment、从副本修复，或者进入人工修复流程。

## Q841. CRC32 能防止所有数据损坏吗？

不能。CRC32 主要用来发现意外损坏，比如半写、短读、磁盘或传输中的随机 bit flip。它不提供安全意义上的防篡改能力，也不适合抵抗恶意修改。

当前实现里 CRC32 覆盖的是 record body，也就是 stream id、event type、idempotency key 和 payload 这些变长内容。header 里的 magic、version、长度、seq、timestamp、crc 字段本身不在 CRC 覆盖范围内。header 损坏时，magic/version 和长度检查能发现一部分问题，但不是完整保护。

这带来一个现实边界：如果 body 被破坏，CRC 很容易发现；如果 header 被改坏，系统可能通过 magic/version、长度异常、读取失败发现，也可能出现更难解释的错误。生产化时我会把 checksum 覆盖范围扩大到 header 的稳定字段加 body，或者引入更强的 hash。

如果目标是审计防篡改，还要再往前走一步。CRC 不够，需要 HMAC、签名、hash chain 或 Merkle tree，并且密钥管理和日志归档也要跟上。CRC 是 crash recovery 里的完整性检查，不是安全审计方案。

## Q842. index 文件坏了怎么办？log 文件坏了怎么办？

index 文件坏了相对好处理，因为它是派生数据。LogServe 启动恢复时会扫描 `.log` segment，从 record 里重新拿到 stream、seq、offset、length，再 rewrite index。只要 log 文件完整，index 丢失或损坏不会改变事实来源。

index 的作用是加速 read path。读某个 stream 的记录时，系统可以通过 index 找到 segment offset，不需要每次从头扫所有 log body。但恢复时不能把 index 当权威来源，否则 log 和 index 不一致时会放大错误。

log 文件坏了就麻烦得多。log 是 source of truth。当前实现能处理尾部损坏：发现 partial tail 或 CRC 错误，就截断到最后一条完整记录。这样能覆盖进程崩溃、机器掉电时最常见的半写问题。

如果 log 文件中间损坏，当前单机实现会从损坏处截断。这个策略简单，但会丢掉后面的记录。生产环境里不能满足于这个处理。更稳的做法是：

- 遇到 middle corruption 先停止自动截断，避免扩大损失。
- 把坏 segment 隔离出来，保留现场。
- 从副本或备份恢复该 segment。
- 对受影响 stream 做一致性检查，确认 replay 缺口。
- 如果允许审计模式，保留坏记录的证据和修复记录。

所以回答时要分清：index 坏是性能结构损坏，可以重建；log 坏是事实来源损坏，只能在 tail 场景自动修复，中间损坏需要副本、备份或人工介入。

## Q843. logical trim 不释放磁盘，那你为什么说能降低 replay 成本？

logical trim 的作用是给 stream 记录一个 trim point，例如 `TrimStream(stream_id, before_seq)`。ReadLog 时，如果调用方从更早的 seq 开始读，系统会把起点提升到 trim point。这样 replay 不再处理 trim point 之前的事件。

它确实不释放磁盘空间。segment 文件还在，旧 record 也还在。但对 replay path 来说，读取和状态转换的工作量已经降下来了。actor 是最直观的例子：Counter actor 已经做了 snapshot，snapshot 表示前 100 次 command 的状态都被压进一个对象。恢复时先加载 snapshot，再读 snapshot 之后的 tail log，不需要重新 apply 100 次 `inc()`。

workflow 也类似。已经有 checkpoint 或 terminal 状态的 stream，可以把 replay 起点推到安全位置。这样控制面重启、dashboard rebuild、actor failover 的成本会下降。

logical trim 还有一个作用是给物理压缩提供输入。系统可以统计 `compactable_records` 和 `compactable_bytes`，告诉你哪些数据已经在逻辑上不需要 replay。它是 physical compaction 前面的安全标记，不等于磁盘清理本身。

面试里可以这样说：

> logical trim 降的是 replay 的逻辑工作量，不是立即降磁盘占用。磁盘回收要等 physical compaction，但 compaction 必须依赖 trim point 和 snapshot 元数据。

## Q844. 如果要做 physical compaction，你如何保证不会删掉还需要的事件？

第一步是定义“还需要”的标准。对 actor 来说，只有在 snapshot 已经持久化，并且 `ActorSnapshotCreated` 事件已经写入 log 后，snapshot 覆盖范围之前的 command 才能被视为可压缩。对 workflow 来说，也要有对应 checkpoint 或 terminal 状态表明旧事件不再参与 replay。

第二步是按 stream 判断，不能按文件名粗暴删除。一个 segment 里可能混有多个 stream 的记录。即使 `actor:a` 已经 trim 到 seq 100，segment 里可能还有 `workflow:b` 的 seq 3，而这个 workflow 还没完成。只看 segment 时间或大小删除会破坏别的 stream。

安全做法是 rewrite live records：

1. 读取旧 segment。
2. 对每条 record 判断是否小于该 stream 的 trim point。
3. 已被 snapshot/checkpoint 覆盖的 record 可以丢弃。
4. 仍需要 replay 或审计保留的 record 写入新 segment。
5. 新 segment 和新 index fsync 成功后，原子切换 manifest。
6. 确认没有读者依赖旧文件后，再删除旧 segment。

这里还要考虑审计需求。默认 `ReadLog` 可以隐藏 trim point 之前的记录，但审计读可能要求看到完整历史。如果要同时支持 replay retention 和 audit retention，就不能把物理删除和逻辑 trim 绑死。可以把旧 segment 移到 archive，或者按 tenant、stream、保留策略分层管理。

最重要的是不要让 compaction 走捷径。只要有一个 stream 仍然需要某条事件，就不能删除那条事件所在的数据，除非已经把同 segment 里的其他 live record 安全搬走。

## Q845. 如果要多机部署 shared log，你会怎么做复制和 leader election？

我会先把 logd 做成 Raft 复制组。原因是 LogServe 的 shared log 需要明确的写入顺序、leader election、quorum commit 和故障切换，这些都和 Raft 的模型贴合。

基本设计是：

- 多个 logd 节点组成一个 Raft group。
- leader 接收 AppendLog。
- leader 为每个 stream 分配下一个 seq。
- append 请求连同 stream、seq、event type、idempotency key、payload 一起进入 Raft log。
- 复制到多数派后才算 committed。
- committed 后再 apply 到本地 segment/index/materialized logstore。
- AppendLog 只在 commit 后返回成功。

leader election 由 Raft 处理。leader 挂掉后，follower 发起选举，新的 leader 只能从拥有已提交日志的节点中产生。这样已经 committed 的记录不会被回滚。未达到 quorum 的写入不能对客户端返回成功；如果客户端超时重试，则用 `stream_id + idempotency_key` 去重。

读路径也要分级。普通 dashboard 或 replay 可以接受 follower 上的 eventually consistent read，但对提交后立刻读取的强一致场景，应该读 leader，或者用 Raft read-index/lease read 确认当前节点已经看到最新 commit。

单个 Raft group 会成为吞吐瓶颈，后续可以按 stream_id 分片成多个 Raft group。比如 `task:*`、`workflow:*`、`actor:*`、`llm:*` 可以进一步按 hash 分 shard。跨 shard workflow 如果需要原子 append，就要引入 transaction marker 或更复杂的提交协议。当前项目还没走到这一步，单机 logd 的重点是把 log-first 和 replay 语义跑通。

面试里我会这样收束：

> 多机版 shared log 的重点在于 quorum commit 语义。leader 负责分配顺序，Raft 负责选主和提交，idempotency key 负责客户端重试时不重复生成事件，文件同步只是落地手段。
