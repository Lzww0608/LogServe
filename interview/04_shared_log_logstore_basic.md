# 二、Shared Log / WAL / LogStore（简单）

这一组问题主要考 shared log 的基本功。回答时不要只说“日志可恢复”，要讲清楚 LogServe 里日志记录了什么、怎样落盘、怎样读回来、出错时怎么处理。面试官如果继续追问，重点落在 `log-first`、stream 内顺序、幂等写入、segment recovery 和 logical trim。

## Q061. LogServe 的 shared log 存储了哪些类型的事件？

LogServe 的 shared log 存的是运行时事件，不是业务数据本身。它记录系统状态怎样变化，用来恢复 task、workflow、actor、LLM serving 和调度相关状态。

常见事件可以分几类讲。

task 事件包括 `TaskSubmitted`、`TaskStarted`、`TaskCompleted`、`TaskFailed`、`TaskRedelivered`。这些事件解释一个任务从提交、被 worker 领取、执行完成或失败重投的过程。

workflow 事件包括 `WorkflowStarted`、`StepScheduled`、`StepStarted`、`StepSucceeded`、`StepFailed`、`WorkflowCompleted`、`WorkflowFailed`。workflow 当前状态可以从这些事件 replay 出来。

actor 事件包括 `ActorCreated`、`ActorOwnershipGranted`、`ActorCommandSubmitted`、`ActorCommandApplied`、`ActorCommandFailed`、`ActorSnapshotCreated`。actor 的内存状态恢复依赖这条 actor stream，snapshot 只是减少 replay 成本。

LLM serving 事件包括 `ModelLoadStarted`、`ModelLoaded`、`LLMCompleted`。它们记录模型加载、checkpoint fetch、cache hit、first token latency、total latency 等信息。调度器的 predicted-latency stats 也是从 `LLMCompleted` 增量维护出来的。

还有一些系统事件，比如模型注册、worker 注册、调度策略切换、backpressure 配置变更。它们不属于某个单独任务，但同样要进 shared log，这样 control 重启后能重建运行时配置。

## Q062. append-only log 的基本写入流程是什么？

基本流程是：调用方先构造 `AppendLogRequest`，里面带 `stream_id`、`event_type`、`idempotency_key` 和 `payload`。logd 收到请求后进入 `Store.Append`，先检查必要字段，再检查幂等键是否已经写过。

如果不是重复写入，LogStore 会给该 stream 分配下一个 `seq`，生成 `timestamp_ms`，把 record 编码成一段二进制数据。编码时会写 header、stream id、event type、idempotency key 和 payload，并对 body 计算 CRC32。

接着它判断当前 segment 是否还有空间。如果当前 segment 写满，就滚动到新的 segment。然后把 record 追加到 `segment-xxxxxxxx.log`，同时把索引项追加到对应的 `segment-xxxxxxxx.index`。索引项记录 stream、seq、segment id、offset、length。

最后根据 fsync 策略决定是否立刻 `Sync()`。写成功后，内存里的 `index`、`nextSeq`、`idempotency` 映射才会更新。调用方拿到返回值，其中包括 `seq`、`timestamp_ms`、`crc32` 和 `duplicate`。

这条路径的重点是 append-only：正常情况下不会原地修改历史 record。trim 也只是记录可裁剪点，不会把旧 record 当场从 segment 里删除。

## Q063. stream_id、seq、event_type、idempotency_key、payload 分别表示什么？

`stream_id` 是事件流的名字。LogServe 用它把不同对象的历史分开，比如 `task:<task_id>`、`wf:<workflow_id>`、`actor:<actor_id>`、`llm:<task_id>`。这样一个 workflow 的事件不会和另一个 actor 的事件混在一起。

`seq` 是 stream 内的递增序号。它从 1 开始，只在同一个 stream 里有顺序意义。不同 stream 之间没有全局 seq，也不应该依赖跨 stream 的 seq 比较。

`event_type` 是事件类型，比如 `TaskSubmitted` 或 `ActorCommandApplied`。replay 代码会根据这个字段决定怎样更新内存状态。

`idempotency_key` 是幂等键。它用来识别一次逻辑写入。客户端重试、网络超时、worker 重复上报时，如果带着同一个幂等键再 append，LogStore 会返回已有 record，而不是新增一条。

`payload` 是事件内容。它通常是 JSON 或序列化后的 protobuf 数据，包含状态转换所需字段。例如 task submitted payload 会保存 task spec；LLM completed payload 会保存 cache hit、latency、worker id 等指标。

可以把一条 log record 理解成一句话：在某个 stream 的第几个位置，发生了什么类型的事件，事件内容是什么，这次写入能不能被幂等识别。

## Q064. 为什么每个 stream 内需要单调递增 seq？

因为 replay 必须有稳定顺序。workflow、actor、task 这些对象的状态都依赖事件顺序。如果同一个 actor 的 `ActorCommandApplied` 顺序乱了，最终内存状态就可能不一样。

stream 内单调递增 seq 解决的是“同一个对象自己的历史怎么排序”。例如 `actor:counter-1` 里，seq 1 是创建 actor，seq 2 是第一次 inc，seq 3 是第二次 inc。恢复时按 seq 从小到大读，就能得到确定的状态。

它也让断点读取变得简单。control bootstrap 用 `from_seq` 分批读，读完一批后把下一次的 `from_seq` 设置成最后一条记录的 `seq + 1`。如果没有 seq，分页 replay 很容易重复读或漏读。

这里要注意边界：LogServe 只保证每个 stream 内部的 seq 单调，不提供全局事件顺序。跨 stream 的因果关系要靠业务事件字段或控制面逻辑表达，不能靠 seq 推断。

## Q065. idempotent append 是为了解决什么问题？

它解决的是“请求可能重试，但日志不能重复表达同一件事”的问题。

分布式系统里，调用方经常不知道请求到底有没有成功。比如 worker 执行完任务后调用 `CompleteTask`，网络超时了。这个时候 worker 可能会重试。如果重试又写一条 `TaskCompleted`，metadata、workflow final result、LLM stats 都可能被重复推进。

LogServe 的做法是在 append 层支持幂等键。同一个 `stream_id + idempotency_key` 已经写过时，`Append` 直接返回原 record，并设置 `duplicate=true`。这样重试方可以把它当作成功，不会在日志里多出一条等价事件。

它不是严格 exactly-once 执行。worker 仍可能把 Python 函数跑两次。LogServe 控制的是最终状态提交层：同一个逻辑事件不要重复写入，不要重复改变最终结果。

## Q066. 为什么日志记录需要 CRC32？

CRC32 用来发现 record body 是否损坏。LogStore 在写入 record 时，对 body 计算 CRC32，读回时再算一遍。如果结果不一致，就说明这条 record 不能信。

这个设计主要服务两个场景。

第一是进程或机器异常退出。append 可能只写了一半，segment 尾部留下不完整数据。恢复时如果没有校验，系统可能把半条记录当成合法事件 replay，状态会变得不可解释。

第二是磁盘或文件内容异常。CRC32 不是安全哈希，也不是防篡改机制，但它足够用来发现常见的 torn write、partial tail、body 长度不匹配这类问题。

当前实现里，`readRecordAt` 会检查 magic、格式号、长度和 CRC32。只要其中一项不对，就把 record 视为 corrupt。恢复时遇到 corrupt record，会把 segment 截断到上一条合法记录之后。

## Q067. segment file 的作用是什么？

segment file 是实际存放 log record 的文件。LogServe 会把日志按大小切成多个 `segment-00000001.log`、`segment-00000002.log` 这样的文件，而不是一直写一个无限增长的大文件。

这样做有几个直接好处。

首先，单个文件不会无限变大，后续做恢复、备份、迁移、压缩都会容易一些。其次，segment rolling 为将来的物理 compaction 留了空间。现在项目已经有 logical trim 和 compactable bytes 统计，真正删除旧 segment 可以在这个基础上继续做。再次，恢复时可以按 segment id 顺序扫描，找到最后一个合法 offset。

当前实现的 rolling 条件由 `SegmentSizeBytes` 控制。追加 record 前，如果当前 segment 容量不够，就关闭旧 segment，创建下一个 segment 继续写。

## Q068. index file 的作用是什么？

index file 是读日志的加速结构。每写一条 record，LogStore 会在 `.index` 文件里追加一条 JSON 索引，记录 `stream_id`、`seq`、`segment_id`、`offset` 和 `length`。

有了索引，`ReadLog(stream_id, from_seq, limit)` 不需要从头扫描所有 segment。它先从内存 index 里找到这个 stream 对应的 entries，再按 offset 去 segment file 里读具体 record body。

这里有一个实现上的取舍：record body 不常驻内存，内存里主要保留索引。这样对大 payload 和长时间运行更友好。读的时候再按需从 segment 文件读取 payload。

index file 不是 source of truth。项目里有 `TestIndexRebuiltFromSegments`，会故意删除 `.index` 文件，然后重新打开 store。恢复过程会扫描 `.log` segment，把 index 重建出来。真正的事实仍然是 log record 本身。

## Q069. 为什么不能把所有 record body 都放在内存里？

因为日志是长期增长的，payload 也可能很大。如果把所有 record body 都留在内存里，系统跑得越久内存占用越高，最后会把 control 或 logd 拖垮。

LogServe 的设计是让内存保存必要的定位信息，而不是保存所有内容。`index` 记录每条 record 在哪个 segment、哪个 offset、长度多少。真正的 payload 留在 segment file 里，`Read` 时按需读取。

这也符合项目里的另一个原则：大结果不直接塞进日志。workflow 大结果会写到 result store，日志里只放 `result_ref`。shared log 需要解释状态变化，不应该变成一个无边界的对象存储。

## Q070. ReadLog 为什么要支持 from_seq 和 limit？

`from_seq` 和 `limit` 是为了让 replay 可以分批进行。

如果一个 stream 很长，一次把所有记录读出来会产生大响应，占用内存，也容易让 control bootstrap 卡住。`limit` 可以控制每次最多读多少条，`from_seq` 表示从哪个 seq 开始读。

control 重启时就是这样用的：先 `ListStreams` 找到相关 stream，然后对每个 stream 从 `from_seq=1` 开始读。读完一批后，把下一次 `from_seq` 改成最后一条记录的 `seq + 1`。直到返回记录数少于 limit，说明这个 stream 读完了。

它还有一个好处：调用方可以从中间位置继续读。比如 dashboard 或调试工具只想看某个 stream 后半段事件，不需要每次从头扫。

## Q071. ListStreams 的使用场景是什么？

`ListStreams` 用来发现当前有哪些 stream。它支持按 prefix 过滤，比如找所有 `task:`、`wf:`、`actor:`、`llm:` 开头的 stream。

最典型的使用场景是 control 启动恢复。control 不可能提前知道所有 task id、workflow id、actor id，所以先向 logd 查询 stream 列表，再逐条读取并 replay。

它也服务调试和 dashboard。比如想展示系统里有哪些 actor stream、哪些 workflow stream，或者统计某类 stream 的 compactable records，都需要先枚举 stream。

这里的边界也要讲清楚：`ListStreams` 不是高性能查询引擎。当前实现适合单机实验和中等规模运行时状态恢复。如果做成生产级多租户平台，需要更完整的 stream catalog、权限控制和分页能力。

## Q072. TrimStream 是做什么的？

`TrimStream(stream_id, before_seq)` 是 logical trim。它把某个 stream 标记为“`before_seq` 之前的记录已经可以被压缩或忽略”。

当前实现不会立刻删除 segment 文件。它会把 trim 点写进 `retention.json`，并在内存里记录 `trimBefore[stream_id]`。之后 `ReadLog` 如果从更早的 seq 读，会自动把起点推进到 trim 点。

actor snapshot 是主要使用场景。actor 写完 `ActorSnapshotCreated` 后，旧的 `ActorCreated` 和早期 `ActorCommandApplied` 已经可以由 snapshot 替代。此时调用 `TrimStream`，后续 actor replay 就从 snapshot 后的 tail log 开始，成本会下降。

`TrimStream` 的返回值还会告诉调用方 compactable records 和 compactable bytes。dashboard 可以用这些指标显示还有多少日志已经可压缩。

## Q073. logical trim 和 physical compaction 有什么区别？

logical trim 是“标记旧记录可以不用读”。它改变的是读取视图和保留元数据，不会马上回收磁盘空间。LogServe 当前实现的 `TrimStream` 就属于 logical trim。

physical compaction 是“真正回收空间”。它需要删除整个旧 segment，或者重写 segment，把仍然需要保留的 record 搬到新文件里。这个操作复杂得多，因为一个 segment 里可能混着多个 stream 的 record。某个 actor 已经 trim 到 seq 100，不代表同一个 segment 里的 workflow record 也能删。

所以项目先做 logical trim 是合理的：它已经能减少 replay 成本，也能暴露 compactable bytes。等后续要解决磁盘长期增长，再基于这些 trim 点做物理 compaction。

面试里我会明确说：当前已经实现 snapshot-aware logical retention，但还没有做真正的 segment garbage collection。

## Q074. fsync always、batch、interval 分别代表什么？

这三个策略控制 append 后多久把文件刷到磁盘。

`always` 是每次 append 后都同步 log file 和 index file。它最保守，单条记录返回成功时，落盘语义最强，但吞吐最低。

`batch` 在当前实现里不会每次 append 都 sync，而是在关闭文件、滚动 segment 或关闭 store 时统一 sync。它等价于把多次写入合并成一批刷盘，吞吐高很多，但异常掉电时最近一批写入存在丢失窗口。

`interval` 是按时间间隔 sync。默认间隔是 100ms。append 时如果距离上次 sync 已经超过配置间隔，就执行一次 sync；否则先返回。它介于 `always` 和 `batch` 之间，可以控制一个大致的风险窗口。

这不是一个“谁一定最好”的选择。实验环境想看吞吐差异，可以用 batch/interval；如果强调每条提交返回后的本地持久性，就选 always。

## Q075. 为什么 fsync always 吞吐量会低？

因为 `fsync` 很贵。普通写入通常先进入操作系统页缓存，速度比较快；`fsync` 要求操作系统把文件数据和元数据推到持久介质，并等待这个动作完成。

在 `always` 策略下，每写一条 log record，LogStore 都要对 log file 和 index file 做 `Sync()`。这会把 append 路径变成“写一条、等一次磁盘”。哪怕单条 payload 很小，等待磁盘 flush 的固定成本也躲不掉。

你的实验结果也能说明这个问题：同样是 20,000 条记录、16 个 stream、256 字节 payload，`always` 的 append throughput 大约是 1.7k records/s，而 batch 和 interval 都到了二十多万 records/s。这个差距主要来自 fsync 频率，而不是编码逻辑本身。

## Q076. 为什么 batch/interval fsync 的 append throughput 更高？

因为它们把多次 append 的同步成本合并了。

`batch` 不在每次 append 后 sync，多个 record 可以连续写入页缓存。关闭 store 或滚动 segment 时再统一 sync。`interval` 则把 sync 控制在固定时间间隔附近。两者都减少了系统调用和磁盘 flush 次数。

吞吐提高的代价是 durability window。也就是说，append 返回后，如果机器立刻断电，最近一批尚未 sync 的记录可能没有真正落到磁盘。进程正常关闭时会 sync，所以测试里的 recover 能读回来；但生产语义不能把 batch/interval 说成和 always 一样强。

面试时这点要说实在：batch/interval 是性能和持久性窗口之间的权衡，不是免费优化。

## Q077. 恢复时为什么要扫描 segment？

因为进程重启后，内存里的 `index`、`nextSeq`、`idempotency` 映射都没了。LogStore 需要从磁盘上的 segment file 重建这些结构。

恢复过程会按 segment id 顺序扫描 `.log` 文件，从 offset 0 开始一条条调用 `readRecordAt`。每读到一条合法 record，就把它加入内存 index，更新该 stream 的 `nextSeq`，并把带幂等键的 record 放回 idempotency map。

扫描结束后，LogStore 会重写 `.index` 文件。这就是为什么 index file 可以被看作缓存，而不是事实本身。只要 segment file 还在，索引可以重建。

还要加载 `retention.json`。它保存 logical trim 点。没有这一步，重启后 `ReadLog` 又会看到已经 trim 掉的旧记录，actor snapshot replay 的优化就失效了。

## Q078. 遇到 partial tail 时为什么要 truncate？

partial tail 指 segment 文件尾部只有半条 record，常见原因是进程崩溃或机器断电时正在写入。

恢复时如果读到 partial tail，继续保留它没有意义。它既不能通过 header、长度和 CRC32 校验，也不能被 replay。更危险的是，如果不截断，后续 append 可能接在这段坏数据后面，整个 segment 的后半部分都变得难处理。

LogServe 的做法是：扫描 segment 时遇到 corrupt record，就把文件截断到上一条合法 record 结束的位置，然后停止扫描这个 segment。这样恢复后的 log 只包含完整、可校验的 record。

`TestRecoveryTruncatesPartialTail` 专门覆盖了这个场景：先写一条合法 record，再手动往 segment 尾部追加几个无效字节。重新打开 store 后，仍然只能读到那条合法 record。

## Q079. append 成功返回 duplicate=true 代表什么？

它代表这次 append 请求没有新增 record，而是命中了之前已经写过的同一个幂等键。

具体判断条件是 `stream_id + idempotency_key`。如果 LogStore 发现这个组合已经存在，就返回原 record 的 `seq`、`timestamp_ms` 和 `crc32`，并设置 `duplicate=true`。

对调用方来说，这通常应该被当作成功处理，而不是错误。比如客户端提交 task 后超时，再用同一个幂等键重试。返回 `duplicate=true` 说明第一次提交其实已经进入 log，不需要再创建一个 task。

但它也提醒调用方：幂等键不能乱复用。如果两个不同逻辑事件用了同一个幂等键，第二个事件会被当作重复请求吞掉。SDK 和控制面要把幂等键设计成能唯一代表一次逻辑操作。

## Q080. 为什么 shared log 可以作为系统状态恢复的基础？

因为它保存的是状态变化的历史，而不是某一刻的临时视图。

metadata store 里的 task status、workflow status、actor owner、model registry 都是当前视图。它们读起来快，但如果更新失败、进程重启、数据库表被清空，单靠当前视图很难知道过去发生过什么。

shared log 不一样。只要事件按 log-first 写入，系统就能从 `TaskSubmitted`、`TaskStarted`、`TaskCompleted` 这类事件重建 task 状态；从 workflow stream 重建 DAG step 状态；从 actor stream 加 snapshot 重建 actor 内存状态；从 LLM stream 重建模型加载和请求执行历史。

这也是 LogServe 的主线：log 是 source of truth，metadata 是 materialized view。metadata 可以丢，可以重建；log 丢了，系统就失去解释和恢复状态的根。

## Q081. 当前 shared log 是单机还是多副本？

当前实现是单机 logd，不是多副本 shared log。

logd 是独立进程，数据落在本地目录里的 segment file 和 index file。它可以和 control、worker 分开启动，也可以在 Docker Compose 里作为一个服务运行。但它没有 Raft、Paxos 或多副本 quorum 写入。

这意味着当前系统适合单机实验、教学展示和小规模原型。它能演示 log-first、replay、idempotent append、partial tail recovery、logical trim 等机制，但不能宣称已经具备生产级高可用日志存储。

如果要生产化，shared log 至少要补多副本复制、leader 选举、故障转移、读写一致性协议、磁盘坏块处理和备份恢复策略。

## Q082. 当前 shared log 和数据库 WAL 是一回事吗？

不是一回事。

数据库 WAL 是数据库内部的写前日志，主要服务事务恢复和存储页恢复。应用通常不会把业务语义直接建立在数据库 WAL 上，也不会让 workflow replay 去读 PostgreSQL WAL。

LogServe 的 shared log 是应用层事件日志。它存的是 `TaskSubmitted`、`WorkflowCompleted`、`ActorCommandApplied`、`LLMCompleted` 这类有运行时语义的事件。replay 代码读这些事件，可以直接恢复 LogServe 的 metadata view 和运行时状态。

可以说二者思想相似：都先记录日志，再恢复状态。但层次不同。数据库 WAL 保护数据库自己的存储一致性；LogServe shared log 保护 runtime 语义和状态恢复能力。

## Q083. 日志记录中 timestamp_ms 主要用于什么？

`timestamp_ms` 主要用于观测、调试和指标计算。

比如 task 事件、workflow step 事件、LLM 事件里都有时间戳，系统可以用它们计算 end-to-end latency、step latency、model load latency、first token latency 等。dashboard 也可以用 timestamp 展示事件发生时间和延迟。

它还方便排查问题。看一条 task stream 时，`seq` 告诉你事件顺序，`timestamp_ms` 告诉你这些事件之间隔了多久。比如 `TaskStarted` 和 `TaskCompleted` 相隔很久，就可以判断执行慢还是排队慢。

但顺序语义主要靠 `seq`，不是 timestamp。不同机器的时钟可能有偏差，同一个 logd 内的 timestamp 也不能替代 stream seq。面试里不要把 `timestamp_ms` 说成一致性机制，它是观测字段。

## Q084. 为什么 log service 独立成 logd 进程？

把 log service 独立出来，是为了让 durability 和控制面解耦。

control 负责调度、metadata view、workflow engine、actor ownership、LLM scheduler。logd 负责 append-only 存储、segment rolling、fsync、recovery、trim stats。两者分开后，control 重启时可以连接已有 logd，从 shared log 重建状态。

这也让多个组件通过同一个 gRPC LogService 写入或读取日志。control 可以 append task/workflow/actor 事件，worker 可以 append task started/completed 和 LLM 事件，调试工具可以 read log。所有组件不需要直接操作 segment 文件。

当然，当前 logd 仍然是单点。独立进程不是高可用本身，它只是把职责边界先拆清楚。后面要做多副本 logd，才算把可用性问题继续往前推进。

## Q085. 你如何测试 append/read/recovery 是否正确？

我会分层测试，而不是只跑一个 end-to-end demo。

第一层是 LogStore 单元测试。`TestAppendReadAndIdempotency` 覆盖首次 append、重复幂等 append、读取记录数和 seq。`TestSegmentRollingRecoverAndReadAcrossSegments` 覆盖 segment rolling 后跨 segment 读取。`TestIndexRebuiltFromSegments` 覆盖删除 index 后能从 segment 重建索引。`TestRecoveryTruncatesPartialTail` 覆盖 corrupt tail 截断。`TestLogicalTrimFiltersReadsAndReportsCompactableBytes` 覆盖 logical trim、compactable records/bytes 和 trim 点重启恢复。`TestFsyncPoliciesAppendAndRecover` 覆盖 batch/interval 策略下正常关闭后的恢复。

第二层是 control 和 runtime 集成测试。task 测试会读取 `task:<task_id>` stream，检查事件链是 `TaskSubmitted -> TaskStarted -> TaskCompleted`。workflow 测试会检查 worker 崩溃恢复后不会重跑已经成功的 step，并验证 replay 状态和 metadata 状态一致。actor 测试会检查 snapshot 后 trim 生效，旧的 actor creation 事件不会再从普通 `ReadLog` 视图读出来。

第三层是实验脚本。`logserve-logbench` 会在不同 fsync 策略下写入大量 record，记录 append throughput、read throughput、recovery time 和 segment count。故障注入实验覆盖 worker kill、queue redelivery、control restart 等场景。

如果面试官问“怎么证明不是 README 设计”，我会直接说：这部分有 Go 单测、集成测试和实验报告三类证据。单测证明 LogStore 基本语义，集成测试证明 runtime 使用日志恢复，benchmark 说明 fsync 和 segment 策略的性能边界。
