# 二、Shared Log / WAL / LogStore（深度）

这一组更偏实现细节。回答时要敢讲当前实现的取舍，不要把原型说成生产级日志系统。LogServe 的 LogStore 已经有 segment、index、CRC、幂等 append、logical trim 和恢复扫描，但它仍是单机实现，没有多副本复制，也没有真正的物理压缩。

## Q086. 你当前日志 record 的二进制格式如何设计？header 中有哪些字段？

当前 record 是固定 36 字节 header 加变长 body。

header 使用 big endian 编码。字段布局是：

```text
0..4    magic              uint32
4..6    format_version     uint16
6..8    stream_len         uint16
8..10   event_type_len     uint16
10..12  idempotency_len    uint16
12..16  payload_len        uint32
16..24  seq                uint64
24..32  timestamp_ms       uint64
32..36  crc32              uint32
```

body 紧跟在 header 后面，顺序是：

```text
stream_id bytes
event_type bytes
idempotency_key bytes
payload bytes
```

这套格式的好处是读取时不需要分隔符。读到 header 后，根据几个长度字段就能切出 stream、event type、幂等键和 payload。`seq` 和 `timestamp_ms` 放在 header 里，读取时不用反序列化 payload 就能知道这条记录在 stream 里的位置和写入时间。

局限也很清楚：payload 对 log 层是 opaque bytes，logstore 不知道里面是 JSON 还是 protobuf。schema 兼容要由上层 replay 代码负责。

## Q087. CRC32 覆盖 header 还是 body？这种选择的后果是什么？

当前 CRC32 只覆盖 body，不覆盖 header。代码里是对 `buf[headerSize:]` 计算 `crc32.ChecksumIEEE`，再把结果写到 header 的最后 4 字节。

这样做能发现 body 损坏。比如 payload 中间有字节被改掉，header 还保持原样，读回时重新计算 body CRC 就会和 header 里的 expected CRC 不一致。

但它不能完整保护 header。magic、格式号、长度字段有额外检查，所以很多 header 损坏仍能被发现。可如果只坏了 `seq` 或 `timestamp_ms`，body CRC 仍然会通过。读路径如果有旧 index，会用 index entry 里的 stream 和 seq 校验 record，能发现 seq 不一致；但恢复扫描时 index 是从 log 重建出来的，它会信任 header 里的 seq。

生产级实现通常会把 header 的关键字段也纳入校验，或者使用 header CRC 和 body CRC 两段校验。当前实现对 partial tail 足够用，但对任意 bit flip 不是完整防护。

## Q088. 如果 header 被破坏但 body 没坏，系统如何识别？

要看 header 坏在哪。

如果 magic 坏了，`readRecordAt` 会直接返回 corrupt record。magic 是文件格式的第一道门槛。格式号坏了也一样，当前只接受格式号 1。

如果长度字段坏了，读取 body 时通常会出问题。长度变大，`ReadAt` 可能读不到足够字节；长度变小或错位，切出来的 body 和 header 里的 CRC32 对不上。这两种都会被当成 corrupt。

如果坏的是 `seq` 或 `timestamp_ms`，当前 CRC32 不一定能识别。用 index 读时，`readIndexedRecordFromFile` 会检查 record 的 stream 和 seq 是否等于 index entry。这个路径能抓住 seq 损坏。恢复扫描时没有可信 index，seq 损坏可能被接受。这是当前格式的一个边界。

面试里我会直接讲这个取舍：现在的 CRC 主要防 body 损坏和 partial tail，不是完整的 record 校验。要做得更稳，应把 header 关键字段纳入校验。

## Q089. 如果 body 被破坏但 header 没坏，系统如何识别？

这个场景当前能识别。

读取 record 时，LogStore 会根据 header 里的长度字段读出 body，然后重新计算 body 的 CRC32。只要 body 中的 stream id、event type、idempotency key 或 payload 任意字节被改，计算出来的 CRC32 就会和 header 里的值不同。

一旦 CRC 不匹配，`readRecordAt` 返回 `errCorruptRecord`。普通读路径会把错误返回给调用方。恢复路径会把它当作损坏记录处理，当前做法是把 segment 截断到上一条合法记录结束的位置。

这里也要补一句：CRC32 是错误检测，不是安全校验。它不防恶意篡改。如果要做安全审计，需要 HMAC、签名或更强的完整性链。

## Q090. 如果 index file 损坏但 log file 完整，系统如何恢复？

当前 index file 不是 source of truth。启动恢复时，LogStore 会扫描 `.log` segment，重新构建内存 index，然后调用 `rewriteIndex()` 删除旧 `.index` 文件并重写。

所以只要 log file 完整，index file 损坏或丢失都不影响最终恢复。最多影响的是启动前那次读路径。如果进程正在运行时 index file 损坏，内存 index 仍在，读路径一般还能工作；但下次启动会按 log 重建。

项目里有 `TestIndexRebuiltFromSegments` 专门覆盖这个点：写入若干 record 后删除 index 文件，再重新打开 store。恢复后可以读到完整记录，并重新生成 index segment。

这也是为什么我会把 index 定义成加速结构，而不是事实来源。

## Q091. 如果 log file 完整但 index file 丢失，读路径会怎样恢复？

重启后恢复流程会自动处理。`OpenWithOptions` 先 `recover()`，它按 segment id 扫描所有 `.log` 文件，把每条合法 record 转成内存 `indexEntry`。然后 `rewriteIndex()` 把 `.index` 文件重新写出来。

恢复完成后，读路径和正常情况一样。`Read(streamID, fromSeq, limit)` 先在内存 index 里选出满足条件的 entries，再按 offset 到 segment file 里读 payload。

如果 index 在进程运行时被外部删除，内存 index 还在，当前进程的读不依赖磁盘 index 文件。只是后续 append 仍会尝试往打开的 index file handle 写；如果底层文件句柄还有效，写入可能继续落到被 unlink 的文件。这个属于不受支持的外部破坏场景。正常恢复语义针对的是进程重启后的磁盘状态。

## Q092. 为什么启动时要 rewrite index？

因为 index 是派生数据。既然恢复时已经扫描了 log segment，就可以顺手把 index 重写成和 log 完全一致的状态。

这样做解决几个问题。

第一，index 文件可能丢失。重写可以补回来。

第二，index 文件可能比 log 多。比如 index entry 写成功了，但 log record 没有真正落盘。启动时直接信 index 会读到不存在的 record。当前实现会删除旧 index，按 log 重建，避免这个问题。

第三，index 文件可能比 log 少。比如 log record 写成功后 index 写失败。恢复扫描 log 时仍能发现这条 record，并重新写入 index。

所以 rewrite index 的思想很简单：用事实日志覆盖派生索引。代价是启动需要扫描 segment，日志越长恢复越慢。后续可以用 checkpointed index 或 segment footer 降低恢复成本。

## Q093. append log 和 append index 的原子性如何保证？

当前没有严格的两文件原子性。实现顺序是先写 log file，再写 index file，然后按 fsync 策略同步文件，最后才更新内存 index、nextSeq 和 idempotency map。

这能保证一个重要点：内存状态不会在 index 写失败前提前推进。也就是说，如果 `appendIndex` 返回错误，`Append` 会返回错误，内存里不会认为这条 record 已经成功。

但磁盘上可能已经有 log record。这个窗口不能被叫作原子提交。恢复时如果 log record 存在，扫描 log 会把它恢复出来。这对 log-first 语义是有利的，但对调用方来说，append 返回错误后仍可能在重启后看到这条事件。

生产级做法通常会更强硬：一旦 index 写失败，把 store 标记为 unhealthy，停止接收后续 append；或者干脆不把 index 放在写入关键路径，所有 index 都从 log 异步构建。当前实现是原型级的简化。

## Q094. 如果 log file 写成功、index file 写失败，会发生什么？

当前 `Append` 会返回错误，并且不会更新内存 index、nextSeq 和 idempotency map。调用方看到的是 append 失败。

但那条 record 的 bytes 可能已经进入 log file。如果进程随后重启，恢复扫描 `.log` 文件时会读到它，把它加入内存 index，并重写 index file。也就是说，这条事件可能在重启后变成有效事实。

这带来一个经典的 ambiguous append 问题：调用方看到失败，但系统之后可能恢复出成功写入的事件。幂等键可以缓解这个问题。调用方用同一个 idempotency key 重试，重启后会命中 duplicate，不会再写第二条。

不过当前实现还有一个边界：如果 index 写失败后进程不重启，内存 idempotency map 没更新，同一个请求马上重试可能再次 append。更稳的策略是 index 写失败后让 logstore fail-stop，要求重启恢复后再继续服务。

## Q095. 如果 index file 写成功、log file 没同步，崩溃恢复如何处理？

恢复时不会信任 index file。`rewriteIndex()` 会先删除旧 `.index` 文件，再根据 `.log` segment 重建。

所以如果 index entry 已经落盘，但对应 log record 没有落盘，重启后这条 index entry 会被丢弃。没有 log record，就没有事件事实。

如果 log record 只落了一半，`readRecordAt` 会因为 header 不完整、长度不匹配或 CRC32 不匹配返回 corrupt。恢复逻辑会截断到上一条合法 record。旧 index 仍然不会被采用。

这个设计的核心是：index 可以比 log 新，也可以比 log 旧，但启动后必须以 log 为准。

## Q096. fsync logFile 和 fsync indexFile 的顺序有没有讲究？

当前实现是先 `logFile.Sync()`，再 `indexFile.Sync()`。这个顺序比反过来更合理，因为 index 指向 log 的 offset。让 index 比 log 更持久没有意义，甚至会产生悬空索引。

不过由于启动时会重写 index，顺序的风险被降低了。即使 index 比 log 多，重启后也会被丢掉。真正不能丢的是 log record。

严格来说，还缺少目录 fsync。创建新 segment 文件、重写 index 文件、rename `retention.json.tmp` 到 `retention.json` 后，如果要对断电场景给出更强保证，应该 sync 目录元数据。当前实现还没做到这一步。

所以我的回答会分两层：代码顺序上 log 先 sync，index 后 sync；从恢复语义上，log 是准绳，index 可重建。

## Q097. segment rollover 的触发条件是什么？

触发点在 `ensureWritableSegmentLocked(recordLen)`。

如果当前 segment 已经有内容，并且 `activeSegmentBytes + recordLen > SegmentSizeBytes`，就调用 `rollSegmentLocked()`。滚动时会先关闭当前 log/index 文件并 sync，然后 `activeSegmentID++`，把 `activeSegmentBytes` 置为 0，再打开下一组 segment 文件。

注意两个细节。

第一，只有当前 segment 非空时才滚动。如果单条 record 本身就大于 segment size，当前实现不会在空 segment 上无限滚动，它会把这条大 record 写进当前 segment，让这个 segment 超过配置大小。

第二，segment size 控制的是编码后的 record 长度，不只是 payload 大小。header、stream id、event type、idempotency key 都算进去。

## Q098. segment size 过小或过大分别有什么影响？

segment size 过小，会频繁 rollover。每次 rollover 都要关闭旧文件、sync、打开新文件，还会产生大量小文件。恢复时需要遍历更多 segment，文件系统压力也更大。小 segment 对测试 rolling 很方便，但不适合高吞吐运行。

segment size 过大，文件数量少，顺序写更舒服，吞吐通常更稳定。但恢复时扫描单个大文件的成本变高，后续做物理压缩也更麻烦。一个大 segment 里混着很多 stream 的 record，哪怕其中一小部分还不能删，整个 segment 都不能直接删除。

所以 segment size 是恢复成本、文件数量和压缩粒度之间的折中。当前默认是 64 MiB，实验脚本会用更小的值触发 rolling，方便验证跨 segment 读取和恢复。

## Q099. 单个 Store 用全局 mutex 会带来什么性能瓶颈？

当前 `Store` 里只有一个全局 `sync.Mutex`。append、读取 index、trim、stats、list streams 都会经过这把锁。

瓶颈主要在 append。多个 stream 并发写入时，哪怕它们互不相关，也要排队拿同一把锁。更要命的是，`Append` 在锁内做了编码、segment 判断、写 log、写 index、fsync 策略判断。`FsyncAlways` 下，锁会一直持有到磁盘同步结束，这会把并发 append 基本串行化。

读路径也会受到影响。`Read` 会在锁内扫描 stream 的 index entries，选出一批后才释放锁。虽然真正的文件读取在锁外做，但如果某个 stream index 很长，选取过程仍会阻塞 append。

这个设计简单，容易保证 `nextSeq` 单调，但吞吐扩展性有限。

## Q100. 如果多个 stream 并发 append，当前锁粒度是否足够？

从正确性看，足够。全局锁让 `nextSeq`、active segment、active offset、index 和 idempotency map 的更新都串行执行，不会出现两个 append 分到同一个 offset 或同一个 stream seq 的问题。

从性能看，不够。不同 stream 的 append 本来可以并行分配 seq，但当前都要排队。由于所有 stream 共享 active segment 文件，文件 offset 分配也被串行化了。

所以当前锁粒度适合原型和单机实验，不适合高并发日志服务。要提升吞吐，需要把“每个 stream 的 seq 管理”和“全局 segment offset 分配”分开设计，或者引入 group commit。

## Q101. 如何把当前 logstore 改成 per-stream lock 或 group commit？

per-stream lock 可以先从 seq 和幂等表拆起。每个 stream 有自己的锁，负责 `nextSeq[stream]` 和该 stream 的 idempotency keys。这样不同 stream 分配 seq 不必互相阻塞。

但写 segment offset 仍然需要全局协调，因为多个 stream 最终写到同一个 active log file。可以保留一个 append lock，只保护 offset 分配和文件写入，把 payload 编码、幂等检查、上层校验放到 per-stream lock 或无锁路径里。

group commit 是另一个方向。append 请求先进入内存队列，由一个 writer goroutine 批量写 log 和 index，再按策略统一 fsync。调用方等待自己的 record 被写入或被 fsync 到指定级别。这样能减少锁竞争和 fsync 次数。

更进一步可以按 shard 拆 segment：比如 stream hash 到 N 个 shard，每个 shard 一组 segment 和 writer。这样牺牲全局单文件顺序，换取更高写入并发。LogServe 当前只需要 stream 内顺序，所以这种改法是可行的。

## Q102. 当前 idempotency map 放内存，重启后如何恢复？

恢复时从 log record 重建。

`recoverSegment` 扫描每条合法 record。如果 record 有 `IdempotencyKey`，就把 `stream_id + idempotency_key` 放回内存 idempotency map。这样 logd 重启后，之前已经成功 append 的幂等键仍然有效。

这里有一个实现细节：idempotency map 里保存的是一个精简 record，payload 会被置空。因为 append duplicate 的响应只需要 seq、timestamp、crc 和 duplicate 标记，不需要返回 payload。这样可以避免 idempotency map 把所有 payload 常驻内存。

所以幂等恢复依赖 log 本身。只要 log record 在，idempotency map 可以重建；如果 log record 丢了，幂等历史也跟着丢。

## Q103. idempotency key 的作用域为什么需要带 stream_id？

因为同一个幂等键在不同 stream 里可能代表不同逻辑操作。

比如两个 task 都用 `submitted` 作为幂等键。如果不带 stream scope，那么 `task:A` 的 `submitted` 会和 `task:B` 的 `submitted` 冲突，第二个任务会被误判为重复写入。

当前实现的 key 是 `stream_id + "\x00" + idempotency_key`。用 stream_id 做作用域后，幂等只在同一条事件流内生效。它符合 LogServe 的状态模型：一个 task、一个 workflow、一个 actor、一个 LLM request 都有自己的 stream。

如果以后要引入租户，还要把 tenant 也纳入作用域。否则不同租户使用相同 stream id 和幂等键会互相影响。

## Q104. 如果同一个 idempotency_key 携带不同 payload，logstore 层是否能发现？control 层是否能发现？

logstore 层当前发现不了。它只看 `stream_id + idempotency_key`。如果命中重复，直接返回已有 record 的元信息，不会比较新旧 payload。更重要的是，idempotency map 里不保存 payload，所以它也没有足够信息做 payload diff。

control 层对主要用户入口做了 fingerprint 检查。task、workflow、actor create 都会根据请求内容算 SHA-256 fingerprint，并把它存到 metadata。相同 idempotency key 再次提交时，如果 fingerprint 不一样，会返回 idempotency conflict。

但这个检查不是 logstore 的通用保证。直接调用 LogService 的低层客户端，仍然可以用同一个幂等键带不同 payload，logstore 会按重复请求处理。

更稳的做法是把 payload hash 写进 log record 或 idempotency map。重复 append 时，如果同一幂等键但 payload hash 不同，就返回 conflict，而不是 duplicate。

## Q105. read path 如何通过 index 读取 segment 中的 payload？

`Read(streamID, fromSeq, limit)` 先拿锁，读取内存里的 `index[streamID]`。它会跳过 `seq < fromSeq` 的 entry，选出最多 `limit` 条 `indexEntry`，然后释放锁。

`indexEntry` 里有 `SegmentID`、`Offset` 和 `Length`。释放锁后，读路径按 segment 打开对应的 `.log` 文件，用 `ReadAt(offset)` 读取 record header 和 body。

读出来后还会校验：record 的 `stream_id`、`seq` 必须和 index entry 一致，record 长度也必须等于 index entry 的 length。校验通过后才把 payload 返回给调用方。

这个设计让 payload 不需要常驻内存。内存保存定位信息，磁盘保存完整 record。

## Q106. 读一个 stream 的 tail 时复杂度是多少？如何优化？

当前实现读 tail 时，选 entry 这一步是线性扫描。`entries := s.index[streamID]` 后，从头遍历，跳过 `entry.Seq < fromSeq`，直到收集到 limit 条。所以复杂度是 `O(number_of_records_in_stream + limit)`，严格说 limit 已经包含在前面的扫描里。

如果 stream 很长，而调用方只想读最后几条，这个实现会浪费时间。比如 actor stream 有 100 万条命令，读 seq 999900 之后的 tail，仍然要从 slice 开头跳过大量 entry。

优化很直接：因为同一个 stream 的 entries 按 seq 递增，可以用二分查找找到第一个 `seq >= fromSeq` 的位置，复杂度变成 `O(log n + limit)`。再进一步，可以为每个 stream 维护稀疏索引或 tail cache。

如果读 tail 是高频操作，还可以缓存最近 segment file handle，减少反复 open/close 的成本。

## Q107. 如果 stream 很多，ListStreams(prefix) 会有什么复杂度问题？

当前 `ListStreams(prefix)` 会遍历 `s.index` 的所有 key，逐个判断 `strings.HasPrefix`，最后排序返回。复杂度大约是 `O(total_streams + matched_streams log matched_streams)`。

stream 少的时候没问题。stream 很多时，control bootstrap、dashboard 或管理工具频繁调用 `ListStreams("llm:")`、`ListStreams("actor:")`，就会变成明显开销。

可以优化成按 prefix 建 catalog。例如维护 `task:`、`wf:`、`actor:`、`llm:` 四类集合，或者用 trie / radix tree。另一个现实做法是给 `ListStreams` 加分页游标，避免一次返回几十万 stream。

多租户场景还要加 ACL。否则 ListStreams 会变成跨租户枚举入口。

## Q108. logical trim 后 ReadLog 从哪里开始读？

从 `max(request.from_seq, trimBefore[stream_id])` 开始读。

当前 `Trim(streamID, beforeSeq)` 语义是：`beforeSeq` 之前的记录可以被忽略。比如 trim 到 4，seq 1、2、3 会被隐藏，seq 4 仍然可读。

代码里 `Read` 会先检查 `trimBefore[streamID]`。如果 trim 点大于请求的 fromSeq，就把 fromSeq 提升到 trim 点。然后再从 index 里选择 `entry.Seq >= fromSeq` 的记录。

这就是为什么 actor snapshot 后，普通 `ReadLog` 看不到早期 actor creation 和 command applied 事件。它们还在磁盘上，但被 logical view 隐藏了。

## Q109. trimBefore 持久化到 retention.json 会有什么一致性问题？

`retention.json` 是 trim metadata，不是 log record 本身。它和 snapshot 对象、ActorSnapshotCreated 事件不是一个原子事务。

如果 snapshot 已经写好，`ActorSnapshotCreated` 也写入 log，但 `retention.json` 写失败，系统仍然正确，只是恢复时会多 replay 一些旧日志。这个方向的问题是性能退化。

反过来更麻烦。如果 `retention.json` 记录了更靠后的 trim 点，但对应 snapshot 对象丢失，或者 snapshot event 没有成功写入，那么恢复时可能看不到必要的早期事件。当前 actor 路径是先写 snapshot event，再调用 TrimStream，这降低了风险，但对象存储、log 和 retention 之间仍不是单一事务。

还有两个小问题：`retention.json` 本身没有 CRC 或备份副本，文件损坏会让 `Open` 返回错误；rename 后没有目录 fsync，断电场景下目录项持久性不能说得太满。

## Q110. retention.json 写入使用临时文件 rename，这解决什么问题？

它解决的是“写一半文件”的问题。

如果直接覆盖 `retention.json`，进程在写到一半时崩溃，磁盘上可能留下半个 JSON。下次启动解析失败，trim 信息也没法用。

当前做法是先写 `retention.json.tmp`，写完整后再 `Rename` 到 `retention.json`。在常见文件系统上，同目录 rename 是原子的。启动时要么看到旧的完整文件，要么看到新的完整文件，不太会看到半个文件。

它没有解决所有问题。比如没有 sync 临时文件和目录，突然断电时新文件是否一定持久，取决于文件系统和挂载参数。对实验系统够用，生产级日志存储要补更严格的 fsync。

## Q111. 为什么 logical trim 不等于释放磁盘空间？

因为 logical trim 只改变读取视图，不改 segment 文件。

trim 后，`ReadLog` 会跳过旧 seq，`GetStreamStats` 会报告 compactable records 和 compactable bytes。但旧 record 的 bytes 仍然在 `segment-xxxxxxxx.log` 里。磁盘占用不会因为 `TrimStream` 立刻下降。

原因是 segment 里混着多个 stream 的记录。某个 actor stream 的旧记录可以删，不代表同一个 segment 里的 task、workflow、LLM 记录也可以删。如果直接删除整个 segment，可能会误删别的 stream 仍然需要的事件。

所以 logical trim 是 snapshot-aware retention 的第一步。它告诉系统“哪些字节可以考虑回收”，真正回收要靠 physical compaction。

## Q112. 如何实现真正的 physical compaction？

我会按 segment 级重写来做，而不是直接在原文件中间挖洞。

基本流程是：先冻结一个 compaction horizon，只处理这个 horizon 之前的 sealed segments，避免和正在写的 active segment 互相影响。然后扫描候选 segment，判断每条 record 是否仍然可见。规则是：如果 record 的 `seq < trimBefore[stream_id]`，可以丢弃；否则写入新的 compacted segment。

写完新 segment 后，生成新的 index 文件。新文件 fsync 完成后，再用一个 manifest 原子切换当前有效 segment 集合。最后删除旧 segment。

恢复逻辑也要认识这个 manifest。否则 compaction 过程中崩溃，可能出现新旧 segment 同时存在。恢复必须能判断哪个集合是已提交的，哪个只是临时产物。

这比 logical trim 复杂很多，因为它不只是标记，而是改写物理文件布局。

## Q113. physical compaction 如何避免删除仍被其他 stream 需要的 segment？

不能只看某个 stream 的 trim 点。必须逐条 record 判断，或者维护 segment 级引用统计。

一个 segment 可以删除的条件是：这个 segment 里的所有 record 都已经不可见。也就是对每条 record，都满足 `record.seq < trimBefore[record.stream_id]`。只要里面还有一条 workflow 或 task record 没有被 trim，整个 segment 就不能直接删除。

如果不想逐条扫描，可以维护 segment stats：每个 segment 记录有哪些 stream、各自 seq 范围、可见 record 数。trim 点变化后更新可删除判断。但这些 stats 也要能从 log 重建，不能成为新的单点事实。

更通用的办法是 segment rewrite。把仍然需要的 record 复制到新 segment，旧 segment 删除。这样即使一个大 segment 只有 10% 的记录可删，也能回收一部分空间。

## Q114. compactable_records 和 compactable_bytes 指标如何计算？

当前计算在 `streamStatsLocked` 里完成。

对某个 stream，遍历它的 index entries。如果 `trimmedBefore > 0` 且 `entry.Seq < trimmedBefore`，这条 entry 就算 compactable record。`compactable_records++`，`compactable_bytes += entry.Length`。

这里的 bytes 是编码后的 record 长度，不只是 payload 长度。它包含 header、stream id、event type、idempotency key 和 payload。

这个指标表示“从这个 stream 的视角看，多少记录和字节已经可以被物理压缩”。它不等于立刻可删除的 segment 大小。真正能不能删 segment，还要看同一个 segment 里其他 stream 的记录是否也 compactable。

## Q115. 恢复过程中如何处理 segment 中间 corrupt record？直接 truncate 是否安全？

当前实现遇到任何 corrupt record 都会 truncate 到当前 offset，然后停止扫描这个 segment。这个做法对 partial tail 是安全的，因为尾部坏数据后面没有有效记录。

如果 corrupt record 出现在 segment 中间，就不一定安全。它会丢掉这个 segment 中 corrupt record 后面的所有 bytes，即使后面某些 record 实际上还是完整的。当前格式没有 footer、同步标记或可信外部索引来帮助重新定位下一条 record，所以它选择了简单的截断。

这属于当前实现的边界。更稳的做法是区分 active segment 和 sealed segment：active segment 尾部 corrupt 可以 truncate；sealed segment 中间 corrupt 应该 fail fast，报告数据损坏，而不是静默截断。

如果要做 salvage，需要给每条 record 加更强的 framing，或者给 segment 加 block checksum 和 restart points。

## Q116. 如果 corrupt record 出现在非最后一个 segment，truncate 会不会丢失后续完整记录？

会，至少会丢失同一个 segment 里 corrupt record 后面的记录。

恢复逻辑会对每个 segment 调用 `recoverSegment`。某个 segment 中间出现 corrupt record 时，该 segment 被截断到 corrupt offset，后面的内容被丢弃。然后外层恢复流程仍会继续扫描后续 segment。

这意味着两件事。

第一，同一个坏 segment 内 corrupt record 之后的完整记录会丢。第二，后续 segment 仍可能被恢复，这会造成 stream seq 出现缺口。当前实现没有专门检测 seq gap，也没有把旧 segment 中间损坏当成不可恢复错误。

生产化时我会改这里：如果非最后 segment 出现 corrupt，直接让 logstore 启动失败，并要求人工恢复或备份恢复。truncate 更适合处理最后一个 active segment 的 partial tail。

## Q117. AppendLog 返回 seq 是否足以作为全局顺序？还是只是 stream 内顺序？

只是 stream 内顺序。

`seq` 来自 `nextSeq[streamID]`。每个 stream 自己从 1 递增。`task:A` 的 seq 5 和 `actor:B` 的 seq 5 没有先后关系。

如果只 replay 单个 task、workflow、actor 或 LLM request，这个 seq 足够。LogServe 的设计也主要依赖 stream 内顺序。

如果要解释跨 stream 因果关系，不能直接比较 seq。比如 workflow stream 的 `StepScheduled` 和 task stream 的 `TaskCompleted` 哪个先发生，要靠事件 payload 里的 workflow id、step id、timestamp 或控制面逻辑关联。

## Q118. 如果需要全局 total order，当前设计需要怎么改？

需要引入全局 log position。

最简单的做法是在 Store 里维护一个全局递增 `log_offset_seq` 或 `global_seq`，每次 append 时分配。record header 或 index entry 同时保存 stream seq 和 global seq。这样单 stream replay 用 stream seq，全局审计或跨 stream replay 用 global seq。

如果做多副本，还要把这个全局顺序交给共识层，比如 Raft log index。不能靠多个 logd 节点各自分配。

另一个选择是把所有事件写入同一条物理 ordered log，同时用 stream_id 建二级索引。当前 segment file 实际上已经是单机顺序 append，但没有把物理 offset 暴露成稳定的全局 order，也没有保证恢复后对外语义。

我会谨慎设计这个能力。全局 total order 会降低并发写扩展性，但对审计、全局 replay、跨对象事务有价值。

## Q119. 当前 shared log 是否支持 snapshot？snapshot 是在上层 actor 实现还是 log 层实现？

当前 log 层不保存通用 snapshot。它支持的是 logical trim 和 stream stats。

actor snapshot 是上层实现。actor runtime 会把 actor 状态序列化到对象存储或本地 result store，写一条 `ActorSnapshotCreated` 事件，事件里带 snapshot ref 和 snapshot command count。然后控制面调用 `TrimStream`，把 actor stream 的早期命令标记为可忽略。

所以 snapshot 的语义在 actor 层：知道 actor state 怎么序列化，知道 snapshot ref 怎么恢复。log 层只知道某个 stream 可以 trim 到哪个 seq，以及 trim 后有多少 compactable bytes。

这种分层是合理的。logstore 不应该理解 Counter actor 的状态结构，它只负责保存事件和 trim metadata。

## Q120. 日志记录 payload 使用 bytes，有什么 schema 管理问题？

payload 是 bytes，给了上层很大自由，也带来 schema 管理问题。

logstore 不知道 payload 结构。它不能判断某个 `TaskSubmitted` payload 是否缺字段，也不能做字段兼容检查。事件能不能 replay，完全取决于上层 decoder。

当前很多 payload 是 JSON。JSON 的好处是调试方便，新增字段通常不影响旧 decoder。但字段改名、删除字段、改变类型，都会让老日志 replay 出问题。比如旧日志里是 `model_load_ms`，新 replay 代码只认 `load_time_ms`，历史 LLM 事件就可能丢指标。

要做长期演进，应该给事件加 schema revision，给每类 event_type 写兼容 decoder。更成熟的做法是引入 schema registry 或 protobuf oneof，让事件格式受控演进。

## Q121. 如果 payload JSON schema 变化，replay 如何处理？

目前靠 replay 代码自己兼容。

新增字段比较简单。Go 的 JSON unmarshal 默认忽略未知字段，所以老代码读新字段时通常不会失败，新代码读旧日志时缺字段会得到零值。前提是零值语义要合理。

危险的是字段重命名、类型变化和必填字段缺失。比如一个字段原来是 int，后来变成 object，旧日志 replay 就可能失败。或者字段缺失时零值被误认为真实值，状态会悄悄变错。

我会按事件类型写显式迁移逻辑：先读取一个通用 map 或带 revision 的 envelope，再根据 schema revision 转成当前内部结构。旧日志永远不要直接丢。无法识别的事件应该进入 quarantine 或启动失败，而不是静默跳过。

当前项目还没有完整 schema evolution 框架。这是生产化前要补的一块。

## Q122. 你如何设计 log corruption 的单元测试？

我会分 header、body、tail、index 四类测。

header corruption：写一条合法 record 后，打开 segment 文件，把 magic、格式号、长度字段、seq 分别改掉。magic 和格式号应该导致恢复报 corrupt 或截断。长度字段应该导致 EOF、长度不匹配或 CRC mismatch。seq 损坏要单独测，因为当前 CRC 不覆盖 seq，恢复可能接受它。这个测试能暴露当前实现边界。

body corruption：改 payload 中一个字节，header 不动。读取或恢复时应该出现 CRC mismatch。

partial tail：在合法 record 后追加几个随机字节，恢复后应该只保留合法 record，并把尾部截掉。项目已有这个测试。

index corruption：删除或破坏 `.index` 文件，重启后应该能从 `.log` 重建 index。项目也已有删除 index 后恢复的测试。还可以补一个“index 指向错误 offset”的测试，验证启动重写 index 后错误索引不再生效。

如果要更严格，还应测试非最后 segment 中间 corrupt。当前实现会 truncate，这个测试可以作为已知限制记录下来，后续改成 fail fast。

## Q123. 你如何 benchmark fsync policy 对 append latency p99 的影响？

现有 `logserve-logbench` 主要统计总 append duration、records/s、read duration、recover time 和 segment count。它能说明吞吐差异，但不能直接给 append latency p99。

要测 p99，需要在每次 `store.Append` 前后记录耗时，把每条 append latency 放进数组或 HDR histogram。每个 fsync policy 分开跑，固定 records、streams、payload size、segment size 和 fsync interval。

实验时要注意几点。

先做 warmup，避免第一次打开文件、创建目录影响结果。每个 policy 跑多轮，取中位数或报告波动范围。记录 p50、p95、p99、max，不只看平均值。还要区分 append 返回延迟和 durable latency：batch/interval 的 append 返回快，不代表每条都已经 sync 到磁盘。

我会把结果写成表：policy、throughput、p50、p95、p99、recover_ms、durability window。这样能把性能收益和语义代价放在一起讲。

## Q124. log append 慢时如何反馈给 backpressure？

control 里所有 log append 走 `Service.appendLog` 包装函数。它会记录一次 `AppendLog` RPC 的耗时，把毫秒数写到 `lastLogAppendMs`。

backpressure 配置里有 `log_append_slow_ms`。提交普通 task 时，control 会读取最近一次 log append 耗时。如果 `lastLogAppendMs >= logAppendSlowLimit`，就直接拒绝新的 task，返回类似 `backpressure: last log append latency ... exceeds slow threshold ...` 的错误。

dashboard 也会展示 `LastLogAppendMs` 和 `LogAppendSlowMs`。这样实验时可以看到系统是否因为日志写慢开始限流。

这个机制比较简单，只看最近一次 append。更稳的做法是看滑动窗口 p95/p99，避免一次偶发抖动导致拒绝，也避免最近一次很快掩盖持续尾延迟。

## Q125. 当前 logstore 的最大可用性瓶颈是什么？

最大瓶颈是单机、单副本 logd。

LogStore 的很多细节已经有了：segment rolling、CRC、index rebuild、partial tail truncation、logical trim、fsync 策略。但这些都建立在一个本地 logd 进程和一份本地磁盘数据上。logd 挂掉时，control 和 worker 的 log-first 路径会失败；磁盘损坏时，也没有副本可以接管。

第二个瓶颈是恢复策略对中间损坏比较粗糙。partial tail truncate 没问题，但非最后 segment 中间 corrupt 不应该静默截断。生产系统需要更清晰的 fail-fast、备份恢复或多副本修复。

第三个瓶颈是写入并发。全局 mutex 加单 active segment，让实现简单，但高并发 append 下会被锁和 fsync 卡住。

如果只选一个最影响生产化的点，我会选多副本复制。因为 log 是 source of truth，source of truth 一旦只有一份，系统整体可用性和数据可靠性就有硬上限。
