# segment、index、compaction、retention 与 LSM 思想

这份题库整理日志分段、索引重建、压缩合并、保留策略和 LSM tree 的核心思想。重点不是记住某个存储引擎的参数名，而是能说清楚：为什么 append-only 文件需要切段，为什么索引通常是可派生数据，为什么 compaction 既能清垃圾又会放大 I/O，以及 LSM 如何用顺序写换取后台整理成本。

## Q001. 为什么日志文件通常按 segment 滚动？

**回答：**

日志文件按 segment 滚动，本质上是在给一个无限增长的 append-only log 切出可管理的边界。单个文件如果一直写下去，恢复、删除、索引、备份、复制和 compaction 都会变得很难控制。

所谓 segment，可以理解成日志流中的一段连续区间：

```text
00000000000000000000.log
00000000000001000000.log
00000000000002000000.log
```

每个 segment 覆盖一段 offset、LSN 或 sequence number。系统不断向 active segment 追加；当大小、时间、record 数量或 epoch 边界达到条件时，关闭当前 segment，打开新的 segment。

常见原因有这些。

1. **控制单个文件大小**

   文件太大，恢复扫描慢，索引文件也大，工具处理困难。切成 segment 后，每次只处理某个范围。

2. **便于 retention**

   如果保留策略要求删除旧数据，segment 是天然删除单位。直接删除整个旧 segment 比在大文件中间挖洞简单得多。

3. **便于 compaction**

   compaction 通常会读取若干 sealed segment，写出新的 compacted segment，再原子切换引用。不可变 segment 很适合这种 copy-on-write 流程。

4. **便于恢复定位**

   崩溃恢复时，可以按 segment 名称排序，从 manifest、checkpoint 或 last committed offset 附近开始扫描。不需要从一个巨大的文件头开始。

5. **便于索引组织**

   每个 segment 可以有自己的 offset index、time index、key index 或 sparse index。索引小而局部，加载和重建都更快。

6. **减少文件系统风险**

   超大文件在备份、复制、校验、迁移时成本高。多个中等大小 segment 更容易并行处理，也更容易做失败重试。

7. **隔离 active 写入和历史读取**

   active segment 还在追加，需要处理 partial write、fsync、预分配、索引同步。sealed segment 已经不可变，可以被 reader 安全读取，也可以被缓存、校验、复制、压缩。

8. **支持并行**

   复制可以按 segment 发送，备份可以按 segment 上传，compaction 可以按 segment 范围调度。没有 segment 边界，这些任务就只能围绕一个大文件做复杂切片。

9. **支持故障恢复和清理**

   崩溃后，如果最后一个 active segment 尾部有半条 record，只需要修复尾 segment。已经 sealed 的 segment 理论上不应再变化。

面试里可以这样说：segment rolling 把“无限日志”变成“一串有序、有限、可校验、可删除的文件”。它不是为了逻辑正确性本身，而是为了让恢复、清理、索引、复制、备份和 compaction 都有边界。

## Q002. segment size 过大或过小分别有什么问题？

**回答：**

segment size 是典型的工程折中。过大和过小都能跑，但会把成本推到不同地方。

**segment 过大**

1. **retention 粒度太粗**

   假设一个 segment 1GB，保留策略已经允许删除其中前 900MB，但后 100MB 仍然有效。因为删除单位是整个 segment，这 900MB 不能马上释放。

2. **恢复扫描变慢**

   崩溃后要扫描最后一个 segment，或者从某个 checkpoint 附近找 record。segment 越大，最坏扫描范围越大。

3. **索引变大**

   每个 segment 的 sparse index、time index、key index 会随 segment 增大而变大。加载、mmap、cache 和重建成本上升。

4. **compaction 粒度太粗**

   compaction 如果以 segment 为输入，一个大 segment 可能包含很多冷热数据。为了清理少量垃圾，不得不重写大量仍然有效的数据。

5. **冷热数据混在一起**

   新数据和旧数据、热 key 和冷 key 被塞进同一个大文件，后续迁移、压缩、分层存储都不灵活。

6. **备份和复制重试成本高**

   传输一个大 segment 失败，重试成本高。对象存储上传大文件也更容易受超时影响。

7. **active 文件风险窗口大**

   active segment 越大，它处于“可写、可能有未完成尾部”的时间越长。恢复逻辑虽然能处理，但操作边界更宽。

**segment 过小**

1. **文件数量太多**

   文件系统目录项、inode、open file、mmap 区域、文件描述符都会增加。很多小文件会拖慢扫描和管理。

2. **索引元数据膨胀**

   每个 segment 都有 header、footer、index、checksum、manifest entry。segment 太小，元数据比例变高。

3. **顺序写被打碎**

   频繁滚动文件意味着频繁 close、open、rename、fsync directory、预分配。顺序写优势被削弱。

4. **compaction 调度成本高**

   compaction 需要管理大量小输入文件。调度、打开、合并、删除的开销变大。

5. **读路径变复杂**

   查询一个范围可能跨很多 segment。即使每个 segment 都小，整体 reader 要做更多 seek、index lookup 和文件切换。

6. **retention 和 manifest 更新频繁**

   小 segment 让删除更细，但也让 manifest、元数据和监控更新更频繁。

7. **page cache 命中更难稳定**

   大量小文件的访问模式分散，可能造成 cache 和预读效果变差。

**如何选择**

常见选择会考虑：

- 写入吞吐。
- 目标恢复时间。
- retention 粒度。
- compaction 输入规模。
- 索引加载成本。
- 文件系统能承受的文件数量。
- 对象存储或远程复制的分块大小。

面试回答可以落到一句：segment size 过大，管理粒度太粗；segment size 过小，元数据和文件管理成本太高。合适大小不是固定值，要按恢复时间、保留策略、compaction 粒度和文件系统开销一起定。

## Q003. active segment 和 sealed segment 的生命周期有什么区别？

**回答：**

active segment 是当前正在写入的 segment，sealed segment 是已经关闭、不可再追加的历史 segment。二者的生命周期不同，系统对它们的并发控制、校验、索引和清理策略也不同。

**active segment**

active segment 处在写入路径上。它的特点是：

1. **可追加**

   新 record 会写到 active segment 尾部。写入流程通常包括分配 offset、写 record header/payload、更新内存索引、必要时 fsync。

2. **尾部可能不完整**

   崩溃可能留下半条 record。恢复时需要扫描 active segment，找到最后一条 checksum、length、magic 都合法的 record，然后截断坏尾巴。

3. **索引可能还在内存里**

   active segment 的索引通常一边写一边维护。为了性能，不一定每条索引更新都同步落盘。崩溃后可以从 active segment 重建。

4. **需要写入锁或单 writer 协调**

   多线程写入时，要保证 record 不交叉、offset 单调、header 不被读到半写状态。

5. **可能预分配**

   系统可能提前分配文件空间，减少写入时的文件系统元数据开销。预分配空间在恢复时不能被当成有效 record。

6. **滚动条件触发后关闭**

   当大小、时间、record 数、epoch 或 flush 边界达到条件，active segment 会被 sealed，然后创建新的 active segment。

**sealed segment**

sealed segment 是历史文件，原则上不可再改。它的特点是：

1. **只读**

   sealed 后不再追加 record。reader 可以并发读它，复制和备份也可以处理它。

2. **索引可以最终化**

   offset index、time index、key index、Bloom filter、footer checksum 可以写完整并 fsync。

3. **可被 retention 删除**

   如果整个 segment 的数据已经过期、被截断或不再需要，可以直接删除。

4. **可作为 compaction 输入**

   compaction 读取 sealed segment，过滤旧版本、tombstone 或被覆盖的数据，写出新 segment。输入文件不被原地修改。

5. **可缓存和校验**

   sealed segment 内容稳定，适合做 checksum 校验、远程上传、冷热分层、压缩。

6. **删除要等 reader 退出**

   即使 manifest 已经不引用它，仍可能有 reader 持有文件句柄。系统要通过引用计数、snapshot 或延迟删除避免读到一半文件消失。

**生命周期对比**

| 维度 | active segment | sealed segment |
|---|---|---|
| 写入 | 允许追加 | 不允许追加 |
| 尾部 | 可能有 partial record | 应该完整 |
| 索引 | 增量维护，可重建 | 已最终化 |
| 并发 | 写读协调更复杂 | 多 reader 安全 |
| crash recovery | 重点检查尾部 | 通常只校验完整性 |
| retention | 一般不删除 active | 可按策略删除 |
| compaction | 通常不压缩 active | 常作为输入 |

一句话：active segment 面向写入和崩溃尾部修复，sealed segment 面向读取、复制、保留和 compaction。把这两个阶段分开，系统实现会简单很多。

## Q004. segment 文件名通常如何设计以便排序和恢复？

**回答：**

segment 文件名要服务两个目标：人和程序都能看出它在日志流中的位置；恢复时能靠文件名快速排序、定位和过滤。最常见的设计是用 segment 的 base offset、base LSN 或 first sequence number 命名。

例如：

```text
00000000000000000000.log
00000000000000000000.index
00000000000001048576.log
00000000000001048576.index
```

这个名字表示：该 segment 从 offset 0 或 1048576 开始。文件名使用固定宽度数字，字典序就是数值序。

设计时通常考虑这些点。

1. **使用 base offset 或 base LSN**

   不要用“第几个文件”这种易变编号。base offset/LSN 直接表达这个 segment 覆盖的日志区间。

2. **固定宽度补零**

   如果不用固定宽度，字符串排序会出错：

   ```text
   1.log
   10.log
   2.log
   ```

   固定宽度后：

   ```text
   00000000000000000001.log
   00000000000000000002.log
   00000000000000000010.log
   ```

3. **后缀区分文件类型**

   常见后缀包括：

   - `.log`：数据 segment。
   - `.index`：offset 或 key index。
   - `.timeindex`：时间索引。
   - `.tmp`：正在写的临时文件。
   - `.deleted`：等待删除。
   - `.swap`：compaction 替换中间状态。

4. **写入中使用临时名**

   compaction 或恢复重建索引时，先写：

   ```text
   00000000000001048576.index.tmp
   ```

   完成、fsync 后 rename 为正式名。这样恢复时可以忽略 `.tmp`。

5. **必要时加入 shard、partition、term 或 timeline**

   单个目录只属于一个 partition 时，文件名可以只放 offset。跨 shard 或跨 timeline 时，文件路径或 manifest 里要带上：

   - topic/partition。
   - shard id。
   - term/epoch。
   - timeline/incarnation。

   否则不同日志流的 offset 可能重名。

6. **不要只用 timestamp**

   timestamp 可以辅助排查，但不适合作为唯一排序依据。系统时钟可能回拨，多个 segment 可能同一毫秒创建，跨机器时间也不可靠。

7. **文件名只是索引线索，不是唯一真相**

   恢复时仍要读 segment header/footer，检查 magic、version、base offset、last offset、checksum。不能只相信文件名。

8. **manifest 比目录扫描更可靠**

   简单系统可以靠目录扫描。复杂系统通常会有 manifest，记录哪些 segment 是当前有效集合。目录里可能有旧文件、临时文件、compaction 输出文件。

**一个较稳妥的命名方案**

```text
segments/
  00000000000000000000.log
  00000000000000000000.index
  00000000000001048576.log
  00000000000001048576.index
manifest
```

segment header 内再写：

```text
magic
version
stream_id
base_offset
created_epoch
segment_id
```

一句话：segment 文件名最好表达日志顺序，固定宽度、可排序、可从 base offset 定位；但恢复时还要用文件内容和 manifest 校验，不能只靠名字。

## Q005. 为什么 index 可以从 log 中重建？

**回答：**

index 之所以可以从 log 中重建，是因为 index 通常是 log 的派生数据。log 保存了事实，index 只是为了加速查找这些事实。

以一个 append-only KV log 为例，每条 record 可能包含：

```text
magic
version
key
value_length
sequence
timestamp
tombstone flag
checksum
```

如果系统顺序扫描 log，就可以重新得到：

```text
key -> 最新 record 的 offset
```

也可以得到：

```text
offset -> file position
timestamp -> offset
sequence -> offset
```

这就是 index 可重建的基础。

**为什么 log 是权威数据**

log 记录了每次写入的原始变更。只要 record 完整、checksum 通过、commit 边界可判断，系统就能知道哪些写入有效。index 只是把这些写入整理成便于查询的结构。

例如：

```text
offset 100: put A = 1
offset 200: put B = 2
offset 300: put A = 3
offset 400: delete B
```

重建索引时从前往后扫描：

```text
A -> offset 300
B -> tombstone at offset 400
```

旧的 `A=1` 仍在 log 里，但索引指向最新版本。`B=2` 被 tombstone 覆盖。

**这样设计有什么好处**

1. **崩溃恢复更简单**

   如果 index 写了一半崩溃，可以丢弃 index，从 log 重建。

2. **index 可以更激进地缓存**

   内存 index 可以不每次同步落盘，因为丢了还能重建。

3. **减少一致性难题**

   如果 data 和 index 双写，双写顺序很容易出问题。把 log 作为权威，index 作为派生物，恢复逻辑更清楚。

4. **支持格式升级**

   新版本可以用旧 log 重建新格式 index。

5. **支持校验**

   定期扫描 log 重建 index，再和当前 index 对比，可以发现 index corruption。

**index 不是总能无成本重建**

可重建不代表重建便宜。问题在于：

- log 很大时扫描时间长。
- value 很大时，扫描 record header 也可能触发大量 I/O。
- 如果 record 没有 key、sequence、length、checksum，重建会困难。
- 如果 log 经过加密或压缩，重建需要对应 metadata。
- 如果 compaction 已经删除旧 record，重建要基于当前有效 segment 集合。

**什么时候 index 不能简单重建**

如果 index 里保存的是 log 中没有的信息，就不能只靠 log 重建。例如：

- 外部服务返回的分类结果。
- 人工修正的元数据。
- 不可重复计算的随机值。
- 未记录在 log 中的二级索引语义。

这类信息应该写进 log 或单独持久化，否则 index 就不是派生数据。

一句话：index 可以从 log 重建，是因为 log 保存了完整、顺序、可校验的事实；index 只是这些事实的加速视图。工程上要保证 record 内容足够完整，否则“可重建”只是口号。

## Q006. 稠密索引和稀疏索引的 trade-off 是什么？

**回答：**

稠密索引和稀疏索引的区别在于：索引项覆盖到什么粒度。稠密索引通常每条 record 或每个 key 都有索引项；稀疏索引只为部分 record、block 或 key range 建索引。

**稠密索引**

稠密索引类似：

```text
key A -> file offset 100
key B -> file offset 180
key C -> file offset 260
```

优点：

1. **查找快**

   找到 key 后可以直接定位到 record，少做扫描。

2. **适合点查**

   KV 存储、去重表、offset lookup 对点查敏感，稠密索引很直接。

3. **实现简单**

   对每条记录维护一个索引项，查询路径清楚。

缺点：

1. **内存占用大**

   key 多时，内存索引可能比数据还先成为瓶颈。

2. **更新成本高**

   每次写 record 都要更新索引。索引持久化也会带来写放大。

3. **恢复重建慢**

   重建时要为每条 record 生成索引项。

4. **cache 压力大**

   大索引可能挤占数据缓存。

**稀疏索引**

稀疏索引类似：

```text
key A000 -> block offset 0
key A500 -> block offset 4096
key B000 -> block offset 8192
```

查找 `A700` 时，先定位到 `A500` 对应 block，再在 block 内扫描或二分。

优点：

1. **索引小**

   只记录每个 block、每 N 条 record 或每个 key range 的入口，内存占用低。

2. **适合 sorted file**

   SSTable 中数据按 key 排序，稀疏索引加 block 内二分很自然。

3. **更适合磁盘索引**

   小索引更容易常驻内存，或者按页加载。

4. **写入成本低**

   只在 block 边界生成索引项。

缺点：

1. **查询要多读一点**

   定位到范围后，还要在 block 内扫描或二分。

2. **不适合无序 log 的任意 key 查找**

   如果 log 没按 key 排序，只靠稀疏 key index 很难定位单 key 最新值。

3. **范围大小不好选**

   太稀疏，读放大大；太密集，又接近稠密索引。

**如何选择**

- append-only commit log 按 offset 查找：适合 offset sparse index，每隔若干 bytes 记录一个物理位置。
- KV 最新值查询：内存里常用稠密 hash index 指向最新 record。
- SSTable：常用稀疏 block index，加 Bloom filter 和 data block。
- 时间序列 segment：常用时间稀疏索引，定位到时间范围后顺序扫描。

面试可以这样总结：稠密索引用空间换点查速度，稀疏索引用少量扫描换更小索引。只要数据文件本身有顺序，稀疏索引通常更划算；如果要无序 key 的精确最新位置，稠密索引更直接。

## Q007. 内存索引和磁盘索引分别适合保存什么？

**回答：**

内存索引适合保存热路径、体积可控、需要低延迟访问的信息；磁盘索引适合保存完整、较大、可按需加载、用于恢复或冷查询的信息。二者经常配合使用。

**内存索引适合保存什么**

1. **最新 key 到 record offset 的映射**

   append-only KV 常见结构：

   ```text
   key -> {segment_id, offset, length, sequence}
   ```

   查询 key 时直接找到最新 record。

2. **active segment 的写入状态**

   包括 current offset、last sequence、未刷盘范围、写入队列、尾部 checksum 状态。

3. **热 key 或热 range**

   热点数据常驻内存，减少磁盘随机读。

4. **Bloom filter 或 prefix filter**

   用于快速判断某个 segment/SSTable 一定不包含某个 key。

5. **small sparse index**

   如果每个 segment 的稀疏索引较小，可以启动时加载到内存。

6. **reader snapshot 的引用状态**

   哪些 segment 仍被 reader 持有，哪些 compacted 文件还不能删除。

内存索引的优点是快，缺点是容量有限、重启丢失、需要从 log 或磁盘索引恢复。

**磁盘索引适合保存什么**

1. **完整稀疏索引**

   例如每个 data block 的 first key、offset、size。查询时先读 index block，再读 data block。

2. **大 key 空间的二级索引**

   如果 key 数量太大，稠密索引不能全部放内存，可以做磁盘 B-tree、SSTable index 或分区索引。

3. **time index**

   时间戳到 offset 的映射通常不需要每次都全量加载，可按 segment 加载。

4. **segment footer / metadata**

   包括 min/max key、min/max timestamp、record count、checksum、compression、schema version。

5. **manifest**

   当前有效 segment、SSTable、level、compaction 输出文件集合等，需要持久化。

6. **冷数据索引**

   历史 segment 很少访问，不必把完整索引放内存。

**配合方式**

常见组合是：

```text
内存：
  hot key index
  segment metadata
  Bloom filter
  top-level sparse index

磁盘：
  full sparse index
  data block index
  manifest
  footer/checksum
```

查询时先看内存索引，命中后直接定位；未命中或需要更细粒度时，再读磁盘索引。

**恢复时的取舍**

如果磁盘索引损坏，但 log 完整，可以重建磁盘索引。如果内存索引丢了，也可以从磁盘索引或 log 重建。

需要注意：如果某些索引不能从 log 重建，就不能只放内存，也不能当普通 cache。它必须被当成权威数据持久化。

一句话：内存索引负责快，磁盘索引负责大和持久。好的设计会让权威事实在 log 或 manifest 中，索引尽量做成可重建的加速结构。

## Q008. 如果 index 和 data 不一致，恢复时应该以谁为准？

**回答：**

大多数存储系统里，恢复时应该以 data log 或 segment data 为准，把 index 当作可重建的派生数据。原因是 data 保存原始 record，index 只是加速访问的视图。

典型恢复策略是：

```text
1. 校验 data segment 的 magic、length、checksum。
2. 找到最后一条有效 record。
3. 丢弃或截断坏尾部。
4. 删除不可信 index。
5. 从有效 data 重新生成 index。
```

**为什么通常信 data**

1. **data 包含完整事实**

   record 中有 key、value、sequence、timestamp、tombstone、checksum。扫描 data 可以判断最新状态。

2. **index 可能落后**

   系统可能先写 data，再异步更新 index。崩溃时 index 少几条记录很常见。

3. **index 可能写了一半**

   index 文件没有 record 那样强的校验或事务边界时，更容易出现半写。

4. **双写一致性难**

   data 和 index 同时作为权威，会引入双写问题。恢复时必须有一个主次关系。

**哪些情况不能盲目信 data**

1. **data record checksum 失败**

   如果 data 本身损坏，就不能用它重建 index。要截断到 last good record，或者从副本/备份修复。

2. **data 缺少必要语义**

   如果 record 没有 key、sequence、tombstone，只保存裸 payload，index 里才有 key 信息，那么 index 就不是派生数据。这是设计风险。

3. **manifest 是权威集合**

   compaction 后，目录里可能同时存在旧 segment 和新 segment。不能扫描目录里所有 data 文件直接建索引，而要先看 manifest 哪些文件属于当前版本。

4. **index 是事务提交协议的一部分**

   少数系统可能把某些 index metadata 设计为权威状态，比如 B-tree 页本身就是数据结构的一部分。这时恢复要按该系统的 WAL/redo 规则处理，不是简单“data 优先”。

**合理恢复流程**

更完整的恢复顺序通常是：

1. 读取 manifest，确定当前有效 segment 集合。
2. 对每个 segment 扫描 record，校验 magic、length、checksum、sequence。
3. 对 active segment 修复尾部 partial record。
4. 检查 index 是否覆盖 data 范围、checksum 是否正确。
5. 如果 index 缺失或不一致，重建 index。
6. 如果 data 损坏落在已提交范围，停止恢复或从副本修复。

**线上处理原则**

- index mismatch：通常可以自动重建。
- data checksum mismatch：要谨慎，可能需要截断、只读启动、从副本恢复。
- manifest mismatch：不能随便猜，要选择最新完整 manifest 或回退到上一代。

一句话：如果 index 是由 data 派生出来的，就以 data 为准；如果 data 本身损坏，就不能靠坏 data 继续恢复。真正的工程重点是提前定义谁是权威、谁可重建。

## Q009. compaction 和 truncation 的区别是什么？

**回答：**

compaction 和 truncation 都可能减少日志或数据文件，但它们处理的对象和语义不同。

truncation 是按边界丢弃一段连续范围；compaction 是重写文件，只保留仍然有用的数据。

**truncation**

truncation 通常处理的是前缀或后缀：

```text
保留 offset >= 1000 的日志
删除 offset < 1000 的 segment
```

或者：

```text
发现尾部 partial record
truncate file to last_good_offset
```

它的特点是：

- 按 offset、LSN、时间、segment 边界删除。
- 不理解每个 key 的最新版本。
- 不会从文件中间挑出 live record。
- 对文件尾部截断或整段删除很有效。

适用场景：

- 删除已经被 checkpoint 覆盖的旧 WAL。
- follower 日志和 leader 冲突时截断未提交尾巴。
- 删除超过 retention 的整段历史。
- 修复 active segment 坏尾巴。

**compaction**

compaction 会读取旧文件，过滤无效 record，写出新文件：

```text
旧 segment:
  A=1
  B=2
  A=3
  delete B

compacted segment:
  A=3
  tombstone B 或直接省略 B
```

它的特点是：

- 理解 key、sequence、tombstone、snapshot 边界。
- 可以清理文件中间的过期数据。
- 会生成新文件，更新 manifest 或索引。
- 会产生额外读写 I/O。

适用场景：

- KV log 清理旧版本。
- LSM 合并 SSTable。
- 去掉被覆盖的 value。
- 在安全边界后删除 tombstone。
- 降低读放大和空间放大。

**核心区别**

| 维度 | truncation | compaction |
|---|---|---|
| 删除方式 | 按连续边界删除 | 读取、过滤、重写 |
| 是否理解 record 语义 | 通常不理解 | 必须理解 |
| 能否清理文件中间垃圾 | 不能 | 可以 |
| I/O 成本 | 通常低 | 较高 |
| 风险 | 边界错会丢数据 | 过滤规则错会丢数据或复活旧数据 |
| 典型用途 | 日志尾部修复、前缀删除 | LSM/SSTable/KV log 清理 |

一个简单例子：

```text
offset 0: A=1
offset 1: B=1
offset 2: A=2
offset 3: C=1
```

如果只想删除 offset < 2，可以 truncation 前缀。但如果想删除 `A=1` 保留 `B=1` 和 `C=1`，truncation 做不到，因为 `A=1` 在文件中间。需要 compaction。

一句话：truncation 是切掉一段连续日志，compaction 是重写数据集合。truncation 便宜但粗，compaction 昂贵但能真正清理旧版本和洞。

## Q010. logical trim 和 physical compaction 的区别是什么？

**回答：**

logical trim 是逻辑上声明某些数据不再可见；physical compaction 是物理上重写文件，把不可见数据从存储介质上移除。前者改变读语义，后者释放空间。

**logical trim**

logical trim 只更新元数据或逻辑边界。例如：

```text
trim_before_offset = 1000
```

意思是 offset 1000 之前的记录不再对普通 reader 可见。也可能是：

```text
key A is deleted at sequence 200
```

意思是 sequence 200 之后读取 A 应返回不存在。

logical trim 的特点：

- 快。
- 写少量 metadata。
- 通常不移动数据。
- 不一定释放磁盘空间。
- 可以立刻改变读路径。

典型例子：

- 设置 log start offset。
- 写入 tombstone。
- 标记某个 segment obsolete。
- 更新 manifest 不再引用旧文件。

**physical compaction**

physical compaction 会真正读旧数据、筛选 live record、写新文件：

```text
read old segments
filter obsolete records
write new segment
fsync new segment
publish new manifest
delete old segment when safe
```

它的特点：

- 成本高。
- 会产生读放大和写放大。
- 可以释放磁盘空间。
- 可以改善读路径。
- 必须处理 reader、snapshot 和 crash。

**区别表**

| 维度 | logical trim | physical compaction |
|---|---|---|
| 做什么 | 改可见性边界 | 重写物理文件 |
| 是否释放空间 | 通常不释放 | 可以释放 |
| 成本 | 低 | 高 |
| 生效速度 | 快 | 慢 |
| 风险 | 可见性边界错 | 数据过滤和切换错 |
| 例子 | tombstone、log start offset | LSM compaction、segment rewrite |

**二者经常配合**

删除一个 key 时，系统通常先写 tombstone，这是 logical trim。之后 compaction 看到 tombstone 已经覆盖所有旧版本，才会把旧 value 和 tombstone 一起清掉，这是 physical compaction。

日志保留也类似：

```text
logical trim: log_start_offset = 1000
physical cleanup: delete segments whose max_offset < 1000
```

一句话：logical trim 让数据“不再被看见”，physical compaction 让数据“真的从文件里消失”。面试时要强调这两个动作不要混，否则很容易误以为删除标记已经释放了磁盘空间。

## Q011. 为什么 logical trim 不能释放磁盘空间？

**回答：**

logical trim 不能释放磁盘空间，是因为它没有改变底层文件里的 bytes。它只是告诉读路径“不要再返回这些数据”，文件内容仍然存在。

举个例子：

```text
segment-1.log:
  offset 0: A=1
  offset 1: B=1
  offset 2: C=1

metadata:
  trim_before_offset = 2
```

逻辑上 offset 0 和 1 不可见了，但 `segment-1.log` 仍然占着同样大小的磁盘空间。文件系统不知道应用层的 `trim_before_offset` 含义。

常见原因有这些。

1. **文件仍然完整存在**

   只要没有 `unlink`、`truncate`、hole punching 或重写文件，磁盘块不会释放。

2. **不可见数据可能在文件中间**

   文件系统通常擅长截断尾部或删除整个文件。要释放中间几条 record 的空间，只能重写文件或打洞。打洞还要求文件系统支持，并且会带来碎片化。

3. **segment 删除需要整个文件都过期**

   如果一个 segment 里还有一条有效 record，通常不能删除整个 segment。

4. **reader 可能还持有旧视图**

   某些 reader 或 snapshot 仍可能需要旧数据。即使逻辑上新读者看不到，也不能马上物理删除。

5. **tombstone 只是覆盖语义**

   tombstone 表示某个 key 被删除，但旧 value 仍在旧 segment/SSTable 里。只有 compaction 确认 tombstone 覆盖了所有旧版本，才能清理。

6. **manifest 不引用不等于磁盘立刻减少**

   manifest 切换后，旧文件可能仍被打开的 reader 持有。在类 Unix 系统里，文件被 unlink 后，如果还有文件描述符打开，空间也要等最后引用释放后才真正归还。

**什么时候可以释放空间**

通常需要这些动作之一：

- 删除整个过期 segment。
- truncate 文件尾部坏记录。
- compaction 重写 live data 到新文件，再删除旧文件。
- 文件系统 hole punching，但实现复杂且可能破坏顺序读特性。

一句话：logical trim 是读语义，磁盘空间由文件系统管理。只改元数据不移动、不截断、不删除文件，就不会真正释放空间。

## Q012. 物理 compaction 如何避免破坏正在读取的 reader？

**回答：**

物理 compaction 要避免破坏 reader，关键原则是：不要原地修改 reader 可能正在读的文件。成熟系统通常用 immutable segment、copy-on-write、新旧版本 manifest 和引用计数来实现。

基本流程是：

```text
reader 继续读旧文件
compactor 读取旧文件并写新文件
新文件写完并校验
原子发布新 manifest
新 reader 使用新文件
旧 reader 继续使用旧文件
最后一个旧 reader 退出后删除旧文件
```

常见机制有这些。

1. **sealed segment/SSTable 不可变**

   compaction 不在旧文件上原地删除 record，而是把 live record 写到新文件。旧文件保持不变。

2. **snapshot 或 read view**

   reader 开始读取时拿到一个元数据快照：

   ```text
   manifest generation = 10
   files = [A, B, C]
   ```

   即使 compaction 发布 generation 11，旧 reader 仍按 generation 10 读。

3. **引用计数**

   每个 segment/SSTable 记录当前有多少 reader 正在使用。manifest 不再引用它后，也要等 refcount 归零才能删除。

4. **延迟删除**

   compaction 完成后，旧文件先进入 obsolete list。后台清理线程确认没有 reader、没有 snapshot、没有备份任务引用后再删除。

5. **原子 manifest 切换**

   新文件集合先写好，然后通过 manifest generation 原子切换。reader 不会看到半新半旧的集合。

6. **文件名和临时文件隔离**

   compaction 输出先写 `.tmp` 文件。只有完整写入、fsync、校验通过后才进入 manifest。reader 不会读到未完成输出。

7. **范围 tombstone 和版本边界**

   compaction 不能删除仍可能被旧 snapshot 看到的数据。它必须知道最老 snapshot sequence，低于安全边界的数据才可物理清理。

**错误做法**

- 在旧 segment 中间原地覆盖。
- compaction 一完成就删除旧文件，不管 reader。
- 直接扫描目录作为当前文件集合，不看 manifest。
- tombstone 一出现就删除所有旧 value，不考虑旧 snapshot。

这些做法会导致 reader 读到半截文件、找不到文件、旧快照不一致，甚至出现“删除后又复活”的数据。

一句话：compaction 不应该改变 reader 手里的世界。它应该写出一个新世界，原子发布给新 reader，旧 reader 读完旧世界后再清理。

## Q013. copy-on-write compaction 的基本思路是什么？

**回答：**

copy-on-write compaction 的思路是：不在旧文件上修改，而是把仍然有效的数据复制到新文件，校验和持久化都完成后，再通过 metadata 切换让新文件生效。

流程可以写成：

```text
1. 选择输入 segment/SSTable。
2. 创建 compaction output 临时文件。
3. 顺序读取输入文件。
4. 过滤旧版本、过期数据、可删除 tombstone。
5. 把 live record 写入新文件。
6. 写新 index、filter、footer、checksum。
7. fsync 新文件。
8. 原子更新 manifest。
9. 新 reader 使用新文件。
10. 旧文件等 reader 释放后删除。
```

它的优点很直接。

1. **reader 安全**

   旧文件不变，旧 reader 不会被 compaction 打断。

2. **crash 安全**

   崩溃发生在新文件写入中途，旧 manifest 仍然指向旧文件。恢复时删除未完成临时文件即可。

3. **实现简单**

   不需要在一个文件中间移动 bytes，也不需要处理复杂空洞。

4. **适合顺序 I/O**

   输入顺序读，输出顺序写，符合 LSM 和 append-only 的设计取向。

5. **便于校验**

   新文件写完后可以生成完整 checksum、index、Bloom filter，再发布。

代价也很明显。

1. **写放大**

   live data 要被重新写一遍。多层 compaction 中，同一条数据可能被重写多次。

2. **临时空间放大**

   切换前，新旧文件同时存在。磁盘需要能容纳 compaction 输出。

3. **I/O 干扰前台**

   compaction 读旧文件、写新文件，会和前台读写争 I/O。

4. **metadata 切换必须可靠**

   manifest 一旦写错，可能丢文件引用或引用未完成文件。

一个简化例子：

```text
old segment:
  A=1
  B=1
  A=2
  delete B

new segment.tmp:
  A=2
```

当新 segment 和 index 都写完后，manifest 从：

```text
files = [old segment]
```

切换到：

```text
files = [new segment]
```

旧 segment 在没有 reader 后删除。

一句话：copy-on-write compaction 用额外空间和写放大换安全切换。它避免原地修改，让 crash recovery 和并发 reader 都更容易处理。

## Q014. segment compaction 如何处理 crash？

**回答：**

segment compaction 处理 crash 的核心原则是：compaction 完成发布之前，旧文件必须仍然可用；发布之后，新文件必须完整可用。中间任何崩溃，都不能留下一个半新半旧的可见状态。

一个可靠流程通常是：

```text
old manifest references old segments
write new compacted segments as tmp files
write and fsync new indexes
fsync new data files
write new manifest generation
fsync manifest
atomically publish manifest
cleanup old files later
```

**崩溃点和处理方式**

1. **选择输入后崩溃**

   没有写出新文件。恢复后旧 manifest 仍然有效，什么都不用做。

2. **新文件写到一半崩溃**

   目录里可能有 `.tmp` 文件。恢复时看 manifest 不引用它，直接删除。

3. **新文件写完但 index 没写完**

   新 data 文件不应被 manifest 引用。恢复时删除临时 data/index，或重建 index 后重新走发布流程。

4. **新文件和 index 都写完，但 manifest 没发布**

   旧 manifest 仍然有效。新文件是 orphan output，可以删除，也可以作为待完成任务重新校验后发布。简单做法是删除。

5. **manifest 写了一半**

   manifest 要有 generation、checksum、length、magic。恢复时选择最新完整且 checksum 正确的 generation。半写 manifest 不能使用。

6. **manifest 发布后崩溃**

   如果新 manifest 已经 durable，恢复后使用新文件集合。旧文件不再是当前版本，但可能还不能马上删除，先走 obsolete cleanup。

7. **删除旧文件时崩溃**

   旧文件可能删了一部分，也可能还在目录里。恢复时根据 manifest 判断：不被任何有效 manifest、reader、backup 引用的文件可以清理。

**必须有的保护**

- 输出文件先用临时名。
- 新文件完整校验后再进 manifest。
- manifest 原子切换。
- 新 manifest 持久化前不能删旧文件。
- 删除旧文件是可重试、幂等的后台清理。
- 恢复时以 manifest 为准，而不是简单扫描目录。

**为什么不能原地 compact**

如果在旧 segment 中间移动 record，崩溃后文件可能既不是旧版本，也不是新版本。恢复要判断哪些 record 已移动、哪些没移动，非常复杂。copy-on-write 避开了这个问题。

一句话：segment compaction 的 crash 安全靠“先写新文件，后发布元数据，最后删旧文件”。崩溃时要么回到旧版本，要么使用完整新版本，不能卡在中间态。

## Q015. compaction 过程中如何保证新旧文件切换的原子性？

**回答：**

compaction 的原子切换不是靠一次性改所有文件完成的，而是靠一个小而可靠的元数据更新。数据文件可以很多，但对外可见的“当前文件集合”必须由 manifest、CURRENT、version edit 或类似结构统一发布。

典型做法如下。

1. **旧文件保持不变**

   compaction 输入文件继续被旧 manifest 引用，reader 可以正常读。

2. **新文件写入临时路径**

   例如：

   ```text
   000123.sst.tmp
   ```

   写完数据、index、filter、footer、checksum 后 fsync。

3. **新文件改为正式文件名**

   rename 通常提供同目录内原子替换语义。但仅 rename 不够，目录项也需要 fsync directory 才能更可靠地抵抗崩溃。

4. **写 manifest edit**

   manifest 里追加一条版本变更：

   ```text
   add file new1
   add file new2
   delete file old1
   delete file old2
   ```

   这个 edit 要有 checksum、sequence/generation。

5. **fsync manifest**

   manifest 不持久，新版本就不能算发布成功。

6. **内存版本切换**

   manifest durable 后，内存中的 current version 指针从旧版本切到新版本。新 reader 使用新版本。

7. **旧文件延迟删除**

   old files 不再被新 manifest 引用，但要等旧 reader、snapshot、backup 都释放。

**原子性的关键**

对外原子的是 metadata 版本，不是每个文件逐个原子。reader 要么看到：

```text
version 10: old files
```

要么看到：

```text
version 11: new files
```

不能看到：

```text
old1 + new2 + missing old2
```

**常见实现细节**

- manifest append log：像 LevelDB/RocksDB 一样把文件集合变化作为 edit 追加到 manifest。
- CURRENT 文件：指向当前 manifest。
- generation number：恢复时选择最新完整版本。
- checksum：防止半写 metadata 被使用。
- atomic rename：发布 CURRENT 或 manifest 临时文件。
- directory fsync：确保 rename 的目录项持久。
- compare-and-swap：多 compaction 并发时，发布前确认输入文件仍属于当前版本。

**并发 compaction 要注意**

如果两个 compaction 同时运行，可能都读了同一个旧版本。发布时必须检查：

- 输入文件是否仍然存在于当前 version。
- key range 是否和其他 compaction 冲突。
- level 或 segment generation 是否匹配。

不满足就放弃输出文件，不能强行发布。

一句话：compaction 的原子切换靠 manifest/version edit。新文件先完整落盘，manifest 再一次性切换引用，旧文件最后清理。不要让 reader 通过目录扫描自己拼当前版本。

## Q016. LSM tree 的 memtable、SSTable、WAL 分别承担什么职责？

**回答：**

LSM tree 把写入路径拆成三类结构：WAL 负责崩溃恢复，memtable 负责内存中的最新有序数据，SSTable 负责磁盘上的不可变有序文件。

一条写入通常这样走：

```text
write request
  -> append WAL
  -> insert memtable
  -> ack
  -> memtable full
  -> flush to SSTable
  -> background compaction
```

**WAL**

WAL 是 Write-Ahead Log。它的职责是保证还没 flush 到 SSTable 的写入在崩溃后能恢复。

WAL 保存最近写入的变更：

```text
put key=A value=1 seq=100
delete key=B seq=101
```

如果进程崩溃，memtable 丢失，重启时读取 WAL，把 memtable 重建出来。等 memtable 成功 flush 成 SSTable 后，对应 WAL 就可以删除或归档。

WAL 的重点是 durability，不是查询效率。

**memtable**

memtable 是内存中的有序表，常见实现是 skiplist、红黑树、B-tree 或其他有序结构。

它的职责：

- 接收最新写入。
- 支持读最新数据。
- 按 key 有序，方便 flush 成 SSTable。
- 保存 tombstone 和 sequence 信息。

memtable 满了后会变成 immutable memtable，不再接收新写入。新的 memtable 继续服务写入，旧 memtable 后台 flush。

memtable 的重点是写入吞吐和读最新版本。

**SSTable**

SSTable 是 Sorted String Table，磁盘上的不可变有序文件。它通常包含：

- data blocks。
- index blocks。
- filter blocks。
- footer。
- key range。
- checksum。

SSTable 的职责：

- 存储已经 flush 的持久数据。
- 支持有序扫描和范围查询。
- 配合 Bloom filter 降低点查读放大。
- 作为 compaction 输入和输出。

SSTable 不原地更新。新的写入会进入 WAL 和 memtable；旧 SSTable 里的旧版本通过 compaction 清理。

**三者关系**

| 结构 | 位置 | 是否可变 | 主要职责 |
|---|---|---|---|
| WAL | 磁盘顺序日志 | 追加写 | 崩溃恢复 memtable |
| memtable | 内存 | 可变，满后冻结 | 接收新写入、服务热读 |
| SSTable | 磁盘文件 | 不可变 | 持久有序存储、查询和 compaction |

**读路径**

读一个 key 时，通常按新到旧查：

```text
mutable memtable
immutable memtable
L0 SSTables
L1/L2/... SSTables
```

因为同一个 key 可能在多个层级出现，sequence number 决定哪个版本更新。

一句话：WAL 保证写入不丢，memtable 吸收最新写入，SSTable 承担持久有序存储。LSM 的核心就是把随机写先变成内存更新和顺序日志，再通过后台 compaction 整理成适合查询的磁盘结构。

## Q017. level compaction 和 size-tiered compaction 有什么区别？

**回答：**

level compaction 和 size-tiered compaction 都是 LSM 的 compaction 策略。它们的核心差异是：磁盘上允许多少个 sorted run，以及什么时候把它们合并。

**level compaction**

level compaction 把 SSTable 分成多个 level。除 L0 外，同一 level 内文件的 key range 通常不重叠。每一层有目标大小，下一层通常比上一层大很多。

大致结构：

```text
L0: 可能重叠的小文件
L1: 不重叠 key range
L2: 更大的不重叠 key range
L3: 更大的不重叠 key range
```

当某一层超过大小阈值，就选取文件和下一层重叠范围合并，输出到下一层。

优点：

- 点查读放大较低，因为非 L0 层每层通常只查一个候选文件。
- 空间放大较低，旧版本更快被清掉。
- 范围查询更稳定。

缺点：

- 写放大较高。同一条数据可能被多次向下层重写。
- compaction 更频繁。
- 写入高峰时容易出现 compaction backlog。

适合读多、空间敏感、需要稳定查询延迟的场景。

**size-tiered compaction**

size-tiered compaction 又常被称为 tiered 或 universal 风格。它允许同一层或同一组里存在多个 sorted run。当若干 run 大小接近，或者 run 数量超过阈值时，把它们合并成一个更大的 run。

结构类似：

```text
run 1: 最近 flush
run 2: 较早 flush
run 3: 更早 flush
...
```

合并时常挑选大小相近的一批 run。

优点：

- 写放大较低，因为不需要频繁把数据和大层反复合并。
- 写入吞吐好，适合写多场景。
- compaction 决策相对局部。

缺点：

- 读放大较高，因为同一个 key 可能要查多个 run。
- 空间放大较高，旧版本和 tombstone 可能保留更久。
- range query 需要 merge 更多 run。

适合写多、读可接受多路查找、存储空间相对宽裕的场景。

**对比表**

| 维度 | level compaction | size-tiered compaction |
|---|---|---|
| 文件组织 | 多 level，非 L0 通常不重叠 | 多个 sorted run 可并存 |
| 写放大 | 较高 | 较低 |
| 读放大 | 较低 | 较高 |
| 空间放大 | 较低 | 较高 |
| compaction 频率 | 更频繁 | 相对少 |
| 适合场景 | 读多、空间敏感 | 写多、吞吐优先 |

面试里可以这样回答：leveled 用更多后台重写换更好的读和空间；size-tiered 用更多磁盘版本换更低写放大。没有绝对好坏，取决于读写比例、空间预算和尾延迟目标。

## Q018. compaction amplification 包括哪些类型？

**回答：**

compaction amplification 指 compaction 为了整理数据额外付出的放大成本。最常说的是 write amplification、read amplification 和 space amplification，但实际线上还会看到 CPU、cache 和 tail latency 放大。

**1. write amplification**

一条用户写入最终在存储介质上被写了多少次。

LSM 中数据先写 WAL，再写 memtable flush 出来的 SSTable，之后可能在多层 compaction 中反复被重写。用户写 1GB，设备可能写 5GB、10GB 或更多。

来源：

- WAL 写入。
- memtable flush。
- L0 -> L1 compaction。
- L1 -> L2 compaction。
- tombstone 和旧版本清理。

**2. read amplification**

一次查询为了得到结果，需要读取多少额外结构。

点查可能要查：

```text
memtable
immutable memtable
多个 L0 文件
L1
L2
...
```

如果没有 Bloom filter 或 compaction backlog 很严重，就会读很多文件和 index block。

范围查询还要 merge 多个 sorted run，也会产生读放大。

**3. space amplification**

磁盘占用和逻辑 live data 的比例。

如果逻辑上只有 100GB live data，但磁盘上因为旧版本、tombstone、compaction 输出临时文件占了 160GB，那么 space amplification 就是 1.6x。

来源：

- 旧版本还没 compact。
- tombstone 还不能删除。
- 新旧 compaction 文件同时存在。
- snapshot 或长事务阻止清理。
- 备份或 reader 引用旧文件。

**4. CPU amplification**

compaction 要做 merge sort、比较 key、解压、压缩、checksum、filter/index 构建。写入越多，后台 CPU 越高。

**5. cache amplification**

compaction 大量顺序读写可能污染 page cache 或 block cache，把前台热数据挤出去。结果是前台读请求突然变慢。

**6. tail latency amplification**

compaction 通常在后台，但它会争 I/O、CPU、锁和缓存。前台请求 p50 可能不变，p99/p999 明显变差。

**7. metadata amplification**

小 SSTable 或小 segment 太多时，manifest、file metadata、Bloom filter、index block、文件句柄都会膨胀。

这些 amplification 之间会互相交换。例如：

- 增大 Bloom filter 可以降低 read amplification，但增加内存。
- 使用 leveled compaction 降低 read/space amplification，但增加 write amplification。
- 使用 size-tiered compaction 降低 write amplification，但增加 read/space amplification。

一句话：compaction amplification 不是单一指标。面试时至少要说出写放大、读放大、空间放大，再补充 CPU、缓存和尾延迟，这样才像是在讲真实系统。

## Q019. write amplification、read amplification、space amplification 分别是什么？

**回答：**

这三个 amplification 是理解 LSM 和 compaction 的核心指标。

**write amplification**

write amplification 表示：用户写入 1 单位数据，底层存储实际写了多少单位数据。

公式可以粗略写成：

```text
write amplification = physical bytes written / logical bytes written
```

如果用户写 10GB，磁盘实际写 80GB，写放大就是 8x。

LSM 中写放大来源包括：

- WAL 写一遍。
- memtable flush 写一遍 SSTable。
- compaction 把同一条数据从 L0 写到 L1、L2、L3。
- 旧版本和 tombstone 被多次带着合并。
- compaction 输出临时文件。

写放大影响：

- SSD 寿命。
- 写入吞吐。
- 后台 I/O。
- p99 写延迟。
- 云盘费用。

**read amplification**

read amplification 表示：为了回答一次查询，系统需要读取多少额外数据或访问多少结构。

粗略说：

```text
read amplification = physical reads / logical reads
```

点查一个 key，理想情况下读一次就够。但在 LSM 中，可能要查：

- memtable。
- immutable memtable。
- 多个 L0 文件。
- 多层 SSTable。
- Bloom filter。
- index block。
- data block。

如果同一个 key 可能存在于多个 sorted run，系统要按新到旧查，直到找到最新版本或确认不存在。

读放大影响：

- 查询延迟。
- block cache 命中率。
- 磁盘 IOPS。
- range query 的 merge 成本。

Bloom filter、partitioned index、leveled compaction 可以降低读放大。

**space amplification**

space amplification 表示：物理占用空间和逻辑 live data 之间的比例。

公式：

```text
space amplification = physical bytes stored / live logical bytes
```

如果 live data 是 100GB，磁盘占用 150GB，空间放大是 1.5x。

空间放大来源包括：

- 旧版本还未清理。
- tombstone 保留。
- compaction 新旧文件同时存在。
- snapshot 阻止旧文件删除。
- retention 策略保留历史。
- segment 粒度导致部分过期数据无法释放。

空间放大影响：

- 磁盘成本。
- 满盘风险。
- compaction 空间需求。
- 恢复和备份时间。

**三者的关系**

这三个指标经常互相取舍：

| 策略 | 写放大 | 读放大 | 空间放大 |
|---|---|---|---|
| leveled compaction | 高 | 低 | 低 |
| size-tiered compaction | 低 | 高 | 高 |
| 更大 Bloom filter | 不直接增加 | 降低 | 增加内存 |
| 更频繁 compaction | 增加 | 降低 | 降低 |
| 更少 compaction | 降低 | 增加 | 增加 |

一句话：write amplification 是写了多少冤枉数据，read amplification 是读了多少冤枉结构，space amplification 是占了多少冤枉空间。LSM 调优就是在这三者之间做取舍。

## Q020. tombstone 在 LSM 中解决什么问题，又会带来什么问题？

**回答：**

tombstone 是删除标记。它在 LSM 中解决一个很现实的问题：磁盘上的 SSTable 是不可变的，删除一个 key 时不能直接去旧文件里把旧 value 擦掉，只能写入一条更新的“这个 key 已删除”记录。

例如：

```text
L2: A = old_value
memtable: tombstone(A, seq=100)
```

读 A 时，系统先看到更新的 tombstone，就知道 A 已经被删除，不能继续返回 L2 里的旧 value。

**tombstone 解决什么问题**

1. **表达删除**

   不可变文件不能原地删除，tombstone 用追加写表达删除。

2. **覆盖旧版本**

   同一个 key 的旧 value 可能存在于更老层级。tombstone 用更大的 sequence 覆盖它们。

3. **支持崩溃恢复**

   删除也是一条普通写入，可以进入 WAL、memtable、SSTable。崩溃后能恢复删除语义。

4. **支持复制和 CDC**

   删除事件可以作为 record 传播给副本或下游系统。

5. **支持范围删除**

   range tombstone 可以表达一段 key range 被删除，避免为每个 key 写单独 delete。

**tombstone 带来什么问题**

1. **占空间**

   tombstone 本身是一条记录。删除越多，tombstone 越多，空间不会立即下降。

2. **增加读放大**

   查询一个不存在的 key 时，系统可能要检查多个层级，确认是否有 tombstone 或旧 value。Bloom filter 能缓解，但不能消除所有成本。

3. **拖累 compaction**

   compaction 要判断 tombstone 是否可以删除。只有当它已经覆盖所有更老层级中可能存在的旧 value，并且没有 snapshot 需要它时，才可以丢弃。

4. **可能误删或复活数据**

   如果 tombstone 被过早删除，而更底层还有旧 value，之后查询可能看到旧 value，表现为“删除的数据复活”。

5. **长 snapshot 阻止清理**

   旧 snapshot 可能仍需要看到删除前的数据状态。系统不能随便清理 tombstone 和旧 value，空间会膨胀。

6. **范围 tombstone 复杂**

   range tombstone 要处理边界、重叠、sequence、分层、迭代器合并。实现比单 key tombstone 更容易出 bug。

7. **删除密集 workload 会恶化性能**

   大量删除会让系统看起来“数据少了”，但磁盘和读放大反而上升，直到 compaction 跟上。

**什么时候 tombstone 可以被删除**

通常要满足：

- tombstone 覆盖范围内的旧版本已经不存在，或在更老层级不会再被查到。
- 没有 snapshot/read view 需要删除前状态。
- 复制、备份、CDC 不再需要该删除事件。
- compaction 已经把相关层级合并到安全边界。

**面试里的回答重点**

不要只说 tombstone 是删除标记。要补一句：tombstone 是 LSM 用追加写表达删除的办法，它把删除变成可排序、可恢复、可复制的 record；代价是删除不会马上释放空间，还会增加读放大和 compaction 负担。

一句话：tombstone 防止旧值被读出来，但如果清理太早会导致旧值复活，清理太晚又会造成空间和读性能问题。

## Q021. 为什么 Kafka topic segment 可以按 offset 做索引？

**回答：**

严格说，不是整个 Kafka topic 按一个全局 offset 做索引，而是每个 topic partition 的日志按 offset 做索引。Kafka 的 offset 只在单个 partition 内有序、单调递增。segment 也是 partition 日志里的连续片段，所以一个 segment 可以用自己的 base offset 加上相对 offset 来定位消息在文件里的物理位置。

这个前提很重要。Kafka 的 topic 可以有很多 partition，不同 partition 的 offset 没有全局顺序。比如：

```text
topic = orders

partition 0: offset 0, 1, 2, 3 ...
partition 1: offset 0, 1, 2, 3 ...
```

`orders-0` 的 offset 100 和 `orders-1` 的 offset 100 不是同一条全局日志位置。它们只是各自 partition 内的顺序号。

Kafka 的 segment 通常用 base offset 命名：

```text
00000000000000000000.log
00000000000000000000.index
00000000000000100000.log
00000000000000100000.index
```

如果某个 segment 的 base offset 是 100000，那么这个 segment 里的 record batch offset 都大于等于 100000，并且小于下一个 segment 的 base offset。这样一来，系统先用目标 offset 找到对应 segment，再用 segment 自己的 offset index 找文件内位置。

Kafka 的 offset index 本质上是：

```text
relative_offset -> physical_position
```

例如：

```text
segment base offset = 100000

index entry:
relative offset 0     -> file position 0
relative offset 500   -> file position 32768
relative offset 1000  -> file position 65536
```

真实 offset 100500 在索引里可以存成相对 offset 500。这样做有两个好处：

1. **索引项更小**

   Kafka 的 `OffsetIndex` 使用相对 offset。只要一个 segment 覆盖的 offset 范围能放进 32 bit，相对 offset 就够用。segment 一旦太大，或者相对 offset 放不下，就需要滚动新 segment。

2. **索引天然有序**

   写入是 append-only，offset 随着写入单调增加。索引项也按 offset 增加，所以可以二分查找。

读取 offset 101234 时，流程大致是：

```text
1. 在 segment 表里找 base_offset <= 101234 的最后一个 segment。
2. 打开该 segment 的 offset index。
3. 在 index 里找 <= 101234 的最大索引项。
4. 从对应 physical position 开始顺序扫描。
5. 扫到包含 101234 的 record batch 或确认不存在。
```

为什么不是每条消息都建索引？因为没必要。Kafka 的索引是稀疏索引，不保证每个 offset 都有一条索引项。索引只负责把查找位置缩小到一个很小的文件范围，剩下的顺序扫描就够快。官方源码里的 `OffsetIndex` 也明确表达了这个设计：索引把 offset 映射到 segment 内物理位置，而且可以是 sparse 的。

按 offset 索引还有一个隐含好处：retention、replication、consumer fetch、事务可见性、leader epoch 截断都可以围绕 offset 边界说清楚。消费者说“我要从 offset 12345 开始读”，broker 不需要理解业务 key，也不需要按时间倒推，它只要在 partition 的日志空间里定位这个 offset。

不过这里有几个边界要说清楚：

1. **offset 不等于物理字节偏移**

   offset 是逻辑序号，file position 是物理字节位置。索引保存的是二者的映射。

2. **offset 可能有空洞**

   事务、批量消息、log compaction、删除保留策略都可能让“每个 offset 都有可返回 record”这个直觉失效。索引只保证能定位附近位置，不保证目标 offset 一定存在。

3. **offset 只在 partition 内有意义**

   面试里不要说“Kafka topic 有一个全局递增 offset”。正确说法是：Kafka partition 是有序日志，segment 是 partition 日志的连续切片，所以 segment 可以按 offset 建局部索引。

4. **索引可以重建**

   Kafka 的 offset index 是加速结构。崩溃后如果索引损坏，可以从 `.log` 文件扫描 record batch 重建。真正保存消息事实的是 log segment，不是 index。

一句话：Kafka segment 能按 offset 做索引，是因为 partition 日志是 append-only 且 offset 在 partition 内单调递增；segment 以 base offset 切分连续区间，索引用相对 offset 映射到文件位置，再从附近顺序扫描。

## Q022. Kafka 的 retention by time 和 retention by size 分别有什么风险？

**回答：**

Kafka 的 delete retention 常见有两个维度：按时间删旧数据，按大小删旧数据。它们都不是“精确删除某一条消息”，而是以 segment 为清理单位。这个事实决定了很多风险。

**retention by time 的风险**

`retention.ms` 表示日志最多保留多久。它看起来像一个时间 SLA：消费者必须在这段时间内读完，否则旧 segment 可能被删除。

风险主要有这些：

1. **慢消费者会丢数据**

   如果消费者停机、积压或回放太慢，落后超过保留时间，就可能读不到旧 offset。Kafka 会返回 offset out of range，消费者只能跳到 earliest 或 latest，业务上就出现数据缺口。

   这也是 Kafka 官方文档把 `retention.ms` 描述成消费者读取时限的原因。它不是备份承诺，而是“你要在这个窗口内消费”的约束。

2. **删除粒度是 segment，不是 record**

   某条消息已经超过保留时间，不代表它立刻被删除。只有整个 segment 满足删除条件，broker 才会删。低流量 topic 可能很久不滚动 segment，旧数据会保留更久；大 segment 也会让删除更粗。

3. **时间判断可能受 timestamp 语义影响**

   如果系统按 record timestamp 或 segment 最大 timestamp 估算年龄，就要小心生产者时间、broker 时间和事件时间的差异。生产者时钟严重偏移时，按时间保留可能表现得很怪。工程上常见做法是明确使用 append time，或者对客户端时间做限制。

4. **设置太短会牺牲可恢复性**

   运维维护、下游故障、重放历史、重新构建物化视图，都依赖保留窗口。`retention.ms` 太短，系统吞吐看起来很好，但事故恢复空间很小。

5. **设置太长会推高存储和恢复成本**

   保留时间越长，本地磁盘占用越大，broker 重启扫描、迁移、分区重分配、故障节点恢复都会变慢。没有分层存储时，长保留会把 Kafka broker 变成昂贵的长期存储节点。

**retention by size 的风险**

`retention.bytes` 控制每个 partition 的日志最多占多少空间。官方配置说明里也强调，这个限制在 partition 级别生效，topic 总保留量要乘以 partition 数。

风险主要是：

1. **高峰流量会压缩保留时间**

   按大小保留时，真实保留时间不是固定值，而是：

   ```text
   可保留时间 ~= retention.bytes / 写入速率
   ```

   写入速率暴涨时，同样的空间只能保留更短时间。消费者没有变慢，也可能因为上游流量变大而追不上保留窗口。

2. **不同 partition 的保留时间不一致**

   如果 topic 分区流量不均，热 partition 会更快触发大小删除，冷 partition 则保留更久。业务以为“topic 保留 7 天”，实际上热分区可能只保留几个小时。

3. **容易误算总容量**

   `retention.bytes` 是 partition 级别。如果 topic 有 200 个 partition，每个 partition 配 100GB，理论上这个 topic 可以占 20TB，再乘副本数，磁盘预算很容易算错。

4. **segment 粒度导致超限或释放滞后**

   删除以 segment 为单位。假设 retention.bytes 只超了 100MB，但最老 segment 是 1GB，删掉它会一下释放 1GB；如果 active segment 还不能删，也可能短时间超过目标大小。

5. **按大小删除不理解业务重要性**

   热门业务、冷门业务、审计数据、普通指标数据，只要混在同一个 partition 保留策略里，大小压力来时都会按 offset 顺序删。它不会知道哪些消息更重要。

6. **和时间保留是独立维度**

   Kafka 配置里 `retention.bytes` 和 `retention.ms` 独立工作。实际清理通常是谁先满足谁触发。你以为时间能保留 7 天，但大小限制可能先把旧 segment 删掉。

**两者共同的风险**

1. **都以 segment 为单位**

   所以保留策略不是精确到消息。segment size、segment.ms、写入速率都会影响实际删除时机。

2. **都会影响消费者 offset**

   旧 segment 一旦删除，低于 log start offset 的读取就不再可用。消费者保存的 offset 不等于数据一定还在。

3. **都不是备份**

   retention 只是在线日志保留，不是数据归档。合规、审计、灾备和长期分析通常需要外部存储、分层存储或独立备份链路。

4. **和 compaction 混用时语义更复杂**

   delete policy 关心保留窗口，compact policy 关心 key 的最新值。两者组合时，tombstone、delete retention、segment 清理时机都要单独设计。

面试里可以这样答：time retention 的核心风险是“消费者必须在窗口内读完，窗口太短就丢数据，太长就压垮存储”；size retention 的核心风险是“保留时间随写入速率变化，热分区会更快丢历史”。两者都受 segment 粒度影响，所以都不是精确删除。

## Q023. 日志系统如何支持从任意 offset 快速读取？

**回答：**

从任意 offset 快速读取，靠的不是在一个巨型文件里从头扫，而是两级定位：

```text
offset -> segment -> file position
```

第一层找 segment，第二层找 segment 内的物理位置。找到附近位置后，再顺序扫描少量 record。

一个典型设计长这样：

```text
manifest / segment table
  segment A: [0, 10000)
  segment B: [10000, 20000)
  segment C: [20000, 30000)

segment B offset index
  10000 -> pos 0
  10500 -> pos 32768
  11000 -> pos 65536
```

读取 offset 10880：

```text
1. 在 segment table 里找 [10000, 20000)。
2. 打开 segment B 的 index。
3. 找 <= 10880 的最大索引项，例如 10500 -> pos 32768。
4. 从 pos 32768 开始读 record batch。
5. 扫到 offset 10880 或扫过目标位置。
```

如果 segment 表和 index 都是有序结构，复杂度大概是：

```text
O(log segment_count) + O(log index_entries) + O(index_interval_scan)
```

最后一项是稀疏索引带来的顺序扫描成本，可以通过 `indexIntervalBytes` 或类似参数控制。

**segment 表怎么组织**

内存里通常维护按 base offset 排序的结构，例如：

```text
TreeMap<base_offset, Segment>
```

这样可以做 floor lookup：找不大于目标 offset 的最大 base offset。Kafka 源码里的 `LogSegments` 就提供类似 `floorSegment(offset)` 的能力。

持久化时，segment 表来自 manifest、目录扫描加校验，或者二者结合：

- manifest 记录当前有效 segment 集合。
- segment 文件名用 base offset 排序。
- segment header/footer 保存 base offset、last offset、generation、checksum。
- 启动时恢复内存表。

**segment 内怎么定位**

segment 内通常用稀疏 offset index：

```text
relative_offset -> physical_position
```

索引不必记录每条 record。只要能把扫描范围控制在几十 KB 或几百 KB，顺序扫描就很便宜。Kafka 的 `LogSegment.translateOffset()` 就是先查 offset index，再从对应文件位置开始搜索目标 offset。

**为什么还要顺序扫描**

因为索引通常指向 record batch，而不是每条 record；也因为稀疏索引只保证“目标 offset 附近”的位置。顺序扫描可以顺便校验：

- batch header。
- magic/version。
- length。
- checksum。
- base offset 和 last offset。
- transaction visibility。

这比每条消息都建索引更省空间，也更符合日志系统顺序读的特性。

**要处理哪些边界**

1. **offset 小于 log start offset**

   说明数据已被 retention、truncation 或 compaction 清掉。应该返回 offset out of range，而不是从最早数据偷偷读。

2. **offset 大于 log end offset**

   如果是普通 read，可以返回 EOF；如果是 tailing read，可以挂起等待新数据。

3. **offset 落在空洞里**

   compaction 或事务控制可能导致某些 offset 没有可返回 record。系统要定义语义：返回下一条大于等于该 offset 的可见 record，还是报找不到。Kafka fetch 更接近从目标 offset 开始返回后续可见批次。

4. **active segment 正在写**

   reader 不能读到半条 record，也不能读到未提交事务或超过 high watermark 的数据。实现上会传入 max position、high watermark 或 last stable offset。

5. **index 损坏**

   index 可以删除并从 data segment 重建。恢复时不要相信损坏 index 里的 file position。

6. **冷热分层**

   如果 offset 对应远端 segment，先查 remote metadata，再获取远端 index。远端 index 最好缓存到本地，否则每次读旧数据都会产生额外网络请求。

**简化版伪代码**

```text
readFrom(offset, maxBytes):
  if offset < logStartOffset:
      return OFFSET_OUT_OF_RANGE

  if offset >= logEndOffset:
      return EMPTY_OR_WAIT

  seg = segmentTable.floor(offset)
  if seg == null:
      return OFFSET_OUT_OF_RANGE

  idxEntry = seg.offsetIndex.floor(offset)
  pos = idxEntry.position

  records = []
  while records.size < maxBytes and seg != null:
      batch = seg.readBatchAtOrAfter(pos)
      if batch == EOF:
          seg = segmentTable.next(seg)
          pos = 0
          continue
      if batch.lastOffset < offset:
          pos = batch.nextPosition
          continue
      if isVisible(batch):
          records.append(batch)
      pos = batch.nextPosition

  return records
```

一句话：任意 offset 快读靠“有序 segment 表 + segment 内稀疏 offset index + 小范围顺序扫描”。不要把 offset 直接等同于文件字节位置，也不要为了快读给每条消息都建索引。

## Q024. 如何处理跨 segment 的 range read？

**回答：**

跨 segment 的 range read 不需要复杂合并，因为日志按 offset 排序，segment 之间也是按 offset 串起来的。核心做法是：先定位起始 segment，然后按 segment 顺序读，直到达到结束 offset、max bytes、时间限制或可见性边界。

比如要读 `[1500, 3700)`：

```text
segment A: [0, 1000)
segment B: [1000, 2000)
segment C: [2000, 3000)
segment D: [3000, 4000)
```

流程是：

```text
1. 找到包含 offset 1500 的 segment B。
2. 在 segment B 内用 offset index 定位到 1500 附近。
3. 读到 segment B 结束或达到 max bytes。
4. 继续读 segment C。
5. 继续读 segment D，直到 offset >= 3700 或达到返回限制。
```

这里不需要像 LSM range query 那样 merge 多个 sorted run。日志 segment 是时间和 offset 上的连续拼接，range read 只是分段串联。

**实际实现要处理几个细节**

1. **第一个 segment 从目标 offset 附近开始**

   第一个 segment 不能从头读，否则随机读会退化。要用 offset index 找到不大于 start offset 的索引项，再顺序扫到目标位置。

2. **中间 segment 通常从头读**

   对完整覆盖的中间 segment，直接从 position 0 顺序读就行。可以使用 sendfile、pread、mmap 或普通 buffered read，取决于系统实现。

3. **最后一个 segment 要截断到 end offset**

   不能把超过范围的数据返回给调用方。注意 record batch 可能跨越目标 end offset：如果 batch 内含多个 record，要么过滤 batch 内 record，要么按协议返回整个 batch 但用 offset 边界约束可见性。这个要在协议层定义清楚。

4. **max bytes 优先级**

   很多日志系统的 range read 不只按 offset 结束，还受 `maxBytes` 限制。读到 max bytes 就返回，让客户端继续从下一 offset fetch。

5. **至少返回一条的语义**

   如果第一条 record batch 比 max bytes 还大，系统要决定是返回空，还是允许超出 max bytes 返回第一条。Kafka 里有类似 `minOneMessage` 的处理思路，用来避免客户端永远读不动一个大 batch。

6. **active segment 的可见边界**

   读到 active segment 时，不能读到 writer 还没写完的尾部。复制日志还要考虑 high watermark；事务日志还要考虑 last stable offset。实现上通常给 read 传一个 max position 或 max visible offset。

7. **segment 被删除或 compacted**

   range read 开始时应该拿到一个稳定的 manifest/version snapshot，并对要读的 segment 增加引用。否则读到一半，retention 或 compaction 把文件删了，会出现间歇性失败。

8. **跨冷热层**

   如果 range 横跨本地热 segment 和远端冷 segment，reader 要拆成两段：本地读一段，远端拉一段。远端读通常要更大的预取窗口，否则请求延迟会很高。

**稳定版本很关键**

不要让 reader 每读完一个 segment 就重新扫描目录判断下一个文件。目录里可能有：

- compaction 输出的临时文件。
- 已从 manifest 移除但还未删除的旧文件。
- 上传远端中的半成品。
- recovery 留下的 orphan 文件。

更稳的做法是：

```text
read version = manifest.currentVersion()
segments = read version.segmentsInRange(start, end)
pin segments
read them in order
unpin segments
```

这样 reader 看到的是一个一致的文件集合。compaction 可以生成新版本，但不会破坏旧 reader。

**错误和返回语义**

range read 可能遇到这些情况：

- start offset 太老：返回 offset out of range，并带上当前 log start offset。
- end offset 超过 log end：读到当前 end 就停，或者等待新数据。
- 中间 segment checksum 错：返回 corruption error，触发副本修复或远端重拉。
- 远端 segment 暂时不可用：返回可重试错误，不要悄悄跳过。
- 范围内有 offset 空洞：按协议返回后续可见 record，或者报告缺口。不要伪造空 record。

一句话：跨 segment range read 的核心是“先固定一个 manifest 版本，再按 offset 顺序串读多个 segment”。第一段靠 index 定位，中间段顺序读，最后一段按边界截断。

## Q025. segment header 是否应该包含起止 offset、时间范围和校验信息？

**回答：**

应该包含，但最好分清 header、footer 和 manifest 各自保存什么。active segment 还在写，很多信息没法提前知道；sealed segment 已经固定，才适合写完整的结束 offset、时间范围和全文件校验。

我会这样设计：

```text
segment header: 写入开始时确定，尽量不改
segment footer: seal 时写入，描述最终范围和校验
manifest entry: 发布时写入，作为当前版本的元数据索引
```

**header 适合放什么**

header 应该保存打开文件时就确定的信息：

- magic number。
- format version。
- stream/topic/partition id。
- tenant id 或 namespace。
- segment id。
- base offset。
- writer epoch / leader epoch / term。
- create time。
- record format version。
- compression/encryption 标识。
- header checksum。

这些字段的作用是让恢复程序快速判断：

- 这是不是合法 segment。
- 它属于哪个日志流。
- 它从哪个 offset 开始。
- 它由哪个 epoch 的 writer 创建。
- 当前程序能不能理解这个格式。

**footer 适合放什么**

footer 在 segment sealed 时写入，因为这时文件内容已经稳定：

- last offset 或 next offset。
- min timestamp / max timestamp。
- record count。
- data size。
- index 起始位置或索引文件摘要。
- per-block checksum 摘要。
- whole-segment checksum。
- footer length。
- footer checksum。

为什么不把这些都写在 header？因为 header 在文件头。active segment 写入时，last offset 和 max timestamp 会不断变化。频繁回写 header 会引入额外随机写，也会增加崩溃时 header 和数据不一致的概率。

**manifest 适合放什么**

manifest 是快速定位和版本发布用的。它可以重复保存 header/footer 中的一部分字段：

```text
segment_id
base_offset
next_offset
min_timestamp
max_timestamp
local_path / remote_object_key
data_checksum
index_checksum
state
generation
```

重复看起来浪费，但有价值。manifest 让系统不用打开每个 segment 就能做 range lookup、retention、冷热分层和恢复初筛。真正读取 segment 时，再用文件内 header/footer 校验 manifest 是否匹配。

**校验信息怎么放**

校验最好分层：

1. **record 或 batch checksum**

   定位到具体损坏 record，读取时就能发现。

2. **block checksum**

   对大 segment 更友好，远端 range read 时只校验读到的 block。

3. **index checksum**

   index 是派生数据，但损坏会导致错读或读慢。可以校验，坏了就重建。

4. **whole segment checksum**

   适合 sealed segment 上传远端、备份、审计和冷数据定期巡检。

**起止 offset 要怎么表达**

我更倾向于在元数据中保存：

```text
base_offset
next_offset
```

而不是只保存 `last_offset`。`[base_offset, next_offset)` 这种半开区间更适合做范围判断：

```text
offset >= base_offset && offset < next_offset
```

如果日志里允许 offset 空洞，`next_offset - base_offset` 不一定等于 record 数，所以 record count 要单独保存。

**时间范围要注意什么**

时间范围很有用，尤其是：

- timestamp 查询。
- time index。
- retention by time。
- 监控和排障。
- 冷热分层迁移。

但要明确它是什么时间：

- event time。
- producer create time。
- broker append time。
- segment create/seal time。

不同语义不能混用。用 event time 做 retention，遇到生产者时钟偏移会有风险；用 broker append time 更稳定，但不等于业务事件时间。

**面试里的取舍**

可以这样说：segment header 应该放稳定身份信息和 base offset；起止 offset、时间范围、全文件 checksum 更适合在 seal 时写入 footer，并同步到 manifest。这样既能快速恢复和校验，又避免 active segment 频繁改文件头。

一句话：要有这些元数据，但不要把所有东西都塞进可变 header。header 负责识别，footer 负责封口校验，manifest 负责版本管理和快速查找。

## Q026. 如何设计 segment manifest？

**回答：**

segment manifest 是日志系统的“当前文件集合说明书”。它回答几个问题：

- 哪些 segment 属于当前有效版本。
- 每个 segment 覆盖哪个 offset 范围。
- segment 在本地还是远端。
- 对应 index、checksum、时间范围在哪里。
- 哪些旧文件已经被 compaction 或 retention 淘汰。

没有 manifest，系统只能靠扫描目录猜当前状态。目录扫描在简单系统里能用，但一旦有 compaction、冷热分层、异步删除、临时文件、对象存储，就很容易猜错。

**manifest 里的全局字段**

一个 manifest 可以先有全局头：

```text
magic
format_version
cluster_id
stream_id
tenant_id
topic
partition
manifest_generation
writer_epoch
log_start_offset
high_watermark
log_end_offset
created_at
previous_manifest_id
checksum
```

不是每个系统都需要这些字段，但几个概念要有：

- 身份：它属于哪个日志流。
- 版本：它是哪一代 manifest。
- 边界：当前可读和可保留的 offset 范围。
- 校验：能判断文件是否完整。

**每个 segment 的 entry**

每个 segment entry 至少要有：

```text
segment_id
base_offset
next_offset
min_timestamp
max_timestamp
state
local_path
remote_object_key
data_size
record_count
data_checksum
offset_index_ref
time_index_ref
index_checksum
compression
encryption_key_id
created_epoch
sealed_epoch
```

`state` 很关键。一个 segment 不只是“存在或不存在”，它可能处在这些状态：

```text
active
sealed_local
uploading_remote
remote_available
local_evicted
obsolete
delete_pending
deleted
```

有了状态，retention、upload、reader、recovery 才能协调。

**manifest 的写入方式**

常见有两种。

第一种是完整快照：

```text
MANIFEST-000010  保存当前所有 segment entries
CURRENT          指向 MANIFEST-000010
```

优点是恢复快；缺点是每次更新都要写一个大文件。

第二种是 append-only edit log：

```text
MANIFEST:
  add segment A
  add segment B
  remove segment A
  add segment C
```

LevelDB/RocksDB 的 MANIFEST 就是这类思路。它把“文件集合变化”作为 version edit 追加，恢复时从头或从 snapshot 开始 replay。

工程上经常结合两者：

```text
manifest snapshot + 后续 edit log
```

edit 多了就写新 snapshot，避免启动时 replay 太长。

**发布流程**

一个稳妥的 segment 发布流程是：

```text
1. 写 segment.tmp。
2. 写 index.tmp。
3. fsync segment 和 index。
4. 校验 checksum。
5. rename 为正式文件名。
6. append manifest edit: add segment。
7. fsync manifest。
8. 原子更新 CURRENT 或 manifest generation。
```

compaction 发布也类似：

```text
1. 旧 segments 仍在 manifest 中。
2. 写新 compacted segments。
3. 新 segments 校验通过。
4. manifest edit 同时 add 新文件、remove 旧文件。
5. 新 manifest durable 后，reader 才能看到新版本。
6. 旧文件等无 reader 引用后清理。
```

关键点是：数据文件可以很多，但对外可见的当前版本只能通过一个小的元数据更新发布。

**并发和一致性**

manifest 要防止两个后台任务互相覆盖。常见办法：

- manifest generation 单调递增。
- 发布时做 compare-and-swap。
- compaction 输入文件必须仍属于当前版本。
- segment range 不能和已有 entry 冲突。
- 每条 edit 有 length、crc、sequence。
- reader 使用某个 manifest version snapshot。

如果 compaction 任务拿的是 generation 10，发布时发现当前已经是 generation 12，就不能强行发布。它必须重新检查输入 segment 是否仍有效。

**本地和远端都要覆盖**

如果支持对象存储，manifest 不能只写本地路径，还要写远端对象信息：

```text
remote_bucket
remote_key
remote_etag_or_checksum
remote_size
upload_completed_at
remote_index_key
remote_state
```

不能靠 `LIST objects` 当 manifest。对象存储 LIST 可能有成本、延迟和权限问题，而且目录里有对象不代表它已经属于当前可读版本。远端数据也需要先上传完整，再发布 metadata。

一句话：segment manifest 应该像一个小型元数据日志，记录当前有效 segment 集合及其版本变化。数据文件先完整写好，manifest 再原子发布引用，reader 只相信 manifest version，不自己扫描目录拼状态。

## Q027. manifest 损坏时系统如何恢复？

**回答：**

manifest 损坏时，第一原则是不要猜出一个看起来能跑、但会复活旧数据或丢掉新数据的版本。恢复要按可信度分层：先找最新完整 manifest，再回退到旧 generation；如果 manifest 全部不可用，才扫描 segment 文件重建候选状态，并把不确定部分隔离出来。

**正常恢复路径**

如果 manifest 是 append-only edit log，恢复流程通常是：

```text
1. 读取 CURRENT，找到当前 MANIFEST 文件名。
2. 顺序读取 MANIFEST。
3. 每条 edit 校验 magic、length、crc、sequence。
4. replay 到最后一条完整 edit。
5. 遇到半条 edit 或 checksum 错误就停止。
6. 用 replay 结果重建当前 segment set。
7. 清理不被引用的临时文件和 obsolete 文件。
```

如果最后一条 edit 写了一半，直接丢弃半条即可。append-only manifest 的好处就在这里：坏尾巴不影响前面已经完整落盘的版本。

如果 manifest 是快照文件，恢复时要选择最新的完整 generation：

```text
MANIFEST-000010  checksum ok
MANIFEST-000011  checksum ok
MANIFEST-000012  checksum broken

使用 MANIFEST-000011
```

**CURRENT 损坏怎么办**

`CURRENT` 只是指针，不应该是唯一真相。LevelDB 的做法是 `CURRENT` 指向最新 MANIFEST。工程上可以保留多个 generation：

```text
CURRENT
CURRENT.bak
MANIFEST-000010
MANIFEST-000011
MANIFEST-000012
```

如果 `CURRENT` 损坏，可以扫描 manifest 文件名，按 generation 从大到小尝试打开，选择最新完整且 checksum 正确的一代。恢复后重新写 `CURRENT`。

**segment 和 index 如何处理**

manifest 恢复出当前 segment 集合后，再检查每个 segment：

1. 打开 segment header，确认 stream id、base offset、epoch 匹配。
2. 读取 footer 或扫描尾部，确认 next offset、timestamp、checksum。
3. 如果 offset index 存在且校验通过，加载 index。
4. 如果 index 缺失或损坏，从 data segment 重建 index。
5. 如果 active segment 尾部有半条 record，截断到最后一条完整 record。

data segment 是 record 事实来源，index 是派生加速结构。manifest 是“当前哪些 segment 有效”的来源。这三者不要混淆。

**manifest 全坏怎么办**

这是最麻烦的情况。可以扫描目录或对象前缀重建候选 manifest，但要非常保守。

步骤大致是：

```text
1. 扫描本地 segment 文件和远端 segment metadata。
2. 跳过 .tmp、.deleted、.swap、uploading 等中间状态文件。
3. 校验每个 segment 的 header/footer/checksum。
4. 按 stream id、tenant id、partition、epoch 分组。
5. 按 base offset 排序。
6. 检查 offset range 是否重叠、断裂、跨 epoch 冲突。
7. 对 index 缺失的 segment 重建 index。
8. 生成 candidate manifest。
9. 如果存在歧义，进入人工修复或只读模式。
```

歧义主要来自 compaction 和 retention。比如目录里同时有：

```text
old segment: [0, 1000)
new compacted segment: [0, 1000)
```

如果 manifest 丢了，系统不能只因为两个文件都存在就都纳入当前版本。那会让旧数据复活，或者同一 offset 被读两遍。必须依靠 generation、compaction marker、footer metadata、事务性 edit、远端 metadata 或人工确认来判断哪一组文件是当前版本。

**不要做的事**

1. **不要把目录里所有 `.log` 都加载**

   这会把已经 compacted 或 retained 掉的旧 segment 重新暴露出来。

2. **不要相信没有 checksum 的 manifest**

   半写 manifest 可能引用了尚未写完的新文件。

3. **不要把损坏 index 当数据损坏**

   index 坏了可以重建。data segment 坏了才是真正危险。

4. **不要在不确定时继续接受写入**

   元数据不确定时继续写，会把恢复问题变成数据一致性问题。更稳妥的是只读、隔离、人工确认。

**设计上如何降低恢复难度**

- manifest 每条 edit 带 length 和 checksum。
- 保留多个 manifest generation。
- `CURRENT` 使用临时文件加原子 rename 发布。
- segment header/footer 带 stream id、base offset、next offset、epoch、checksum。
- compaction 输出先用 tmp 名，发布后再进入 manifest。
- 删除旧文件延迟执行，并保留 tombstone edit。
- 定期写 manifest snapshot，减少 replay 长度。
- 远端存储保留独立 remote metadata log，不靠对象 LIST 当唯一依据。

一句话：manifest 损坏时，先找最新完整元数据版本；找不到就用 segment 文件重建候选，但不能随便把所有文件都纳入当前版本。恢复的底线是“不复活已删除数据，不引用半成品，不静默丢掉已提交 segment”。

## Q028. 冷热数据分层存储如何影响 segment 管理？

**回答：**

冷热分层会把 segment 管理从“本地文件生命周期”变成“本地加远端的状态机”。active segment 仍然在本地写，sealed segment 可能被上传到对象存储或 HDFS，之后本地副本可以被清理，历史读再从远端取。

一个典型状态流转是：

```text
active_local
  -> sealed_local
  -> uploading_remote
  -> remote_available
  -> local_evictable
  -> local_evicted
  -> remote_retained
  -> remote_deleted
```

这比单机本地日志复杂很多。

**retention 被拆成两层**

没有分层存储时，retention 主要约束本地 segment：

```text
本地保留 N 天或 M bytes
```

有分层存储后，通常会变成：

```text
本地热数据保留：几个小时或几天
远端冷数据保留：几天、几个月，甚至更久
```

Kafka KIP-405 的思路也是这样：本地 tier 保留较短窗口，远端 tier 保存已完成的 log segment，用来支持 backfill、故障恢复和更长 retention。

关键约束是：本地 segment 不能在远端 copy 完成并发布 metadata 之前被删除。否则一旦 broker 本地盘损坏或消费者需要旧数据，系统没有可用副本。

**manifest 要记录位置**

segment entry 不再只是一个本地路径，而要记录：

```text
local_path
remote_object_key
remote_state
remote_checksum
remote_index_key
upload_started_at
upload_completed_at
local_retention_deadline
remote_retention_deadline
```

reader 根据 offset 查到 segment 后，还要判断：

- 本地是否还在。
- 远端是否可用。
- index 是否在本地缓存。
- 需要从远端拉 data 还是只拉 index。

**index 管理更重要**

远端 segment 如果每次读取都先下载完整 index，会很慢，也贵。更好的做法是：

- 上传 data segment 时一起上传 offset index、time index、transaction index、leader epoch cache 等辅助文件。
- 本地保留一个 remote index cache。
- cache miss 时先拉 index，再做 range read。
- index 有 checksum，损坏可从远端 data 重建，但成本高。

KIP-405 里也提到，远端 segment 会连同 offset/time/transaction/producer snapshot/leader epoch 等索引和辅助状态一起复制。

**读取路径会分裂**

热读：

```text
consumer fetch near log end -> local segment -> page cache -> low latency
```

冷读：

```text
consumer fetch old offset -> remote metadata -> remote index cache -> object range read -> higher latency
```

所以分层存储会改变几个参数：

- fetch batch size 可能要更大。
- 预取更重要。
- 超时和重试要区分本地错误与远端错误。
- p99 延迟会比热读差很多。
- 监控要分别统计 local fetch 和 remote fetch。

**compaction 会变复杂**

如果日志支持 compaction，远端冷 segment 怎么 compact 是个难题：

- 把远端数据拉回本地 compact，成本高。
- 在远端生成新 compacted segment，需要远端 manifest 原子切换。
- 已经上传的旧 segment 如果被 compact 掉，远端 metadata 也要删除或标记 obsolete。
- tombstone 保留窗口要跨本地和远端一致。

所以很多系统会先只支持 delete retention 的 tiered storage，或者限制 compacted topic 的远端分层。面试里可以补一句：分层存储和 compaction 不是不能共存，但实现复杂度明显上升。

**故障处理也会变化**

1. **上传失败**

   segment 继续保留本地，后台重试。不能因为本地 retention 到期就删除。

2. **metadata 发布失败**

   data object 可能已经上传，但 manifest 不引用。恢复时要把它当 orphan object，后续清理或重新发布。

3. **远端删除失败**

   manifest 可以先标记 obsolete，后台继续删除。否则 delete 路径会阻塞前台。

4. **远端不可用**

   热读可以继续，本地没有的冷读失败或降级。系统要把这类错误暴露给调用方，而不是返回空数据。

5. **broker 替换**

   新 broker 不需要立即恢复所有冷 segment，只要能通过 remote metadata 服务旧读即可。本地只拉热数据或按需懒加载。

**成本模型会变化**

对象存储按容量、请求数、出站流量收费。segment 太小会造成对象数和请求数爆炸；segment 太大又会让 range read、重试和生命周期管理变粗。冷热分层后，segment size 不只由本地恢复和 page cache 决定，还要考虑：

- multipart upload 大小。
- object count。
- range GET 成本。
- 远端 index cache 命中率。
- 生命周期转储规则。
- 跨区域复制和恢复时间。

一句话：冷热分层让 sealed segment 变成远端不可变对象，本地只保留热窗口。它能降低 broker 本地存储压力，但要求 manifest、index、retention、读取路径和故障恢复都区分 local state 和 remote state。

## Q029. 为什么对象存储适合保存 sealed segment？

**回答：**

对象存储适合 sealed segment，不是因为它像本地文件系统一样快，而是因为 sealed segment 的访问模式刚好符合对象存储的长处：写一次、读多次、很少修改、可以按完整对象做生命周期管理。

sealed segment 有几个特点：

- 内容已经不可变。
- 文件通常比较大。
- 可以独立校验。
- 可以用 base offset 或 segment id 命名。
- 可以和 index、metadata 一起上传。
- 删除通常按整个 segment 进行。

这些都很适合对象存储。

**对象存储擅长什么**

以 S3 这类对象存储为例，它提供的是：

- bucket + key 的对象模型。
- 高扩展容量。
- 跨可用区持久性。
- PUT/GET/DELETE 对象。
- range GET。
- multipart upload。
- lifecycle 转储和过期。
- versioning、replication、Object Lock 等管理能力。

这和 sealed segment 的生命周期很匹配：

```text
seal local segment
upload segment object
upload index objects
verify checksum
publish manifest
eventually delete local copy
```

**为什么不适合 active segment**

active segment 需要不断 append，可能还要处理 fsync、尾部 partial record、索引同步和崩溃截断。对象存储通常没有低成本的原地 append，也不提供 POSIX rename 语义。把 active segment 放在对象存储上，会让每次追加都变成重写对象或复杂 multipart 协调，延迟和一致性都不合适。

所以更合理的边界是：

```text
active segment: 本地磁盘
sealed segment: 对象存储
```

**sealed segment 如何上传更稳**

推荐流程：

```text
1. 本地 segment seal。
2. 计算 data checksum 和 index checksum。
3. 以临时 key 或内容地址 key 上传 data。
4. 上传 offset/time/txn/epoch index。
5. HEAD 或读取 metadata 校验大小和 checksum。
6. 发布 remote manifest entry。
7. 本地 segment 进入可清理状态。
```

manifest 发布必须在对象完整可读之后。否则 reader 可能根据 manifest 去远端读取一个还没上传完的对象。

**对象 key 怎么设计**

对象 key 应该避免覆盖写。常见做法：

```text
tenant/topic/partition/epoch/baseOffset-segmentId.log
tenant/topic/partition/epoch/baseOffset-segmentId.index
```

也可以把 checksum 或 UUID 放进去：

```text
segments/{stream}/{partition}/{baseOffset}-{uuid}.log
```

这样即使重试上传，也不会误覆盖一个已经发布的对象。是否把 key 设计成可读路径，要看权限、成本和列表需求。

**风险点**

1. **对象存储不是文件系统**

   不要依赖原子 rename、目录锁、追加写、低延迟小 IO。发布一致性要靠 manifest 或外部 metadata store。

2. **LIST 不能当强元数据层**

   即使 S3 已经提供强一致读写，LIST 仍然有成本和分页问题；很多 S3 兼容对象存储的一致性语义也不完全相同。系统应该通过 remote log metadata 管理当前可见对象，而不是每次 LIST 前缀来判断。

3. **小对象太多会贵**

   segment 太小会造成 PUT、GET、LIST、HEAD 请求数上升。对象存储适合较大的 sealed segment，不适合海量 tiny segment。

4. **range read 延迟高**

   对象存储支持 range GET，但网络延迟比本地 page cache 高得多。冷读要用更大批量、预取和 index cache。

5. **删除不是立刻省钱的唯一问题**

   还要考虑 lifecycle、版本化对象、Object Lock、跨区域复制和未完成 multipart upload。删除策略要和合规策略一致。

6. **校验和很重要**

   上传、复制、归档、恢复都要能证明对象内容和 manifest 记录一致。仅靠对象 key 不够。

**为什么 sealed segment 比单条 record 更适合**

如果把每条 record 存成一个对象：

- 对象数量爆炸。
- 请求成本极高。
- 顺序读变成大量小 GET。
- metadata 管理复杂。

sealed segment 把很多 record 合成一个较大的不可变对象，既保留了日志顺序，又降低了对象管理成本。读取旧数据时再用 index 做 range read。

一句话：对象存储适合保存 sealed segment，因为 sealed segment 是不可变的大块数据，适合一次上传、按 key 寻址、按生命周期管理；active append 和强一致发布仍然应该放在本地日志和 manifest 体系里处理。

## Q030. 多租户日志系统中 segment 是否应该按 tenant 隔离？

**回答：**

答案不是绝对的。逻辑隔离一定要有，物理 segment 是否按 tenant 隔离，要看租户规模、合规要求、retention 差异、成本模型和性能目标。简单说：大租户、强隔离租户、合规租户更适合独立 segment；大量小租户可以共享 segment，但共享要有明确边界。

**为什么想按 tenant 隔离**

按 tenant 独立 segment 有这些好处：

1. **retention 独立**

   A 租户保留 7 天，B 租户保留 90 天。如果混在同一个 segment，短保留租户的数据不能单独删除，长保留租户又会拖住整个文件。

2. **删除和合规更简单**

   租户要求删除、导出、冻结、法律保留时，独立 segment 可以按 tenant 操作。混合 segment 需要重写文件才能移除某个租户的数据。

3. **加密密钥清晰**

   如果每个租户有独立 KMS key，segment 按 tenant 隔离最自然。混合 segment 要么用共享 key，要么在 record 层做复杂加密。

4. **配额和计费准确**

   segment 大小、请求数、冷存储成本、compaction 成本都能直接归属到租户。

5. **故障和热点隔离**

   一个租户的高写入、高读取、坏数据、compaction backlog，不容易拖累其他租户。

6. **冷热分层更清楚**

   不同租户可以有不同远端 bucket/prefix、生命周期、复制策略和访问权限。

**为什么不总是按 tenant 隔离**

如果租户很多，而且多数租户流量很小，按 tenant 独立 segment 会带来明显成本：

1. **小文件太多**

   每个租户都滚自己的 segment，会生成大量小 segment、小 index、小 manifest entry。文件系统和对象存储都会受影响。

2. **批量写优势下降**

   append-only log 的性能来自批量顺序写。每个小租户单独写，会降低 batching、压缩和 page cache 效率。

3. **compaction 调度膨胀**

   后台任务数量随租户数增长，调度、锁、manifest 更新和监控都会变复杂。

4. **运维对象太多**

   备份、迁移、恢复、巡检、远端 lifecycle 都要面对更多对象。

**共享 segment 的风险**

共享不是不能做，但要知道代价：

- 一个租户的 retention 不能自然删除。
- 一个租户的敏感数据和另一个租户在同一物理文件里。
- 单租户导出或删除需要重写 segment。
- billing 要按 record 级统计。
- 某个租户的热点读会把共享 segment 拉进 cache，影响其他租户。
- compaction 必须理解 tenant id，否则可能错误合并或清理。
- 对象存储权限不能只靠 prefix 隔离，因为同一个对象里混了多个 tenant。

所以共享 segment 只适合隔离要求较低、retention 类似、流量很小的租户集合。

**常见折中方案**

可以按租户分层治理：

```text
大租户 / 付费高级租户:
  独立 topic/partition/segment

中等租户:
  按 tenant group 或 shard 隔离

小租户:
  共享 segment，但 record header 带 tenant_id，并建立 tenant-aware index
```

也可以按策略隔离，而不是纯 tenant 隔离：

```text
retention=7d 的租户放一组 segment
retention=90d 的租户放另一组 segment
需要独立 KMS key 的租户单独 segment
高吞吐租户单独 partition
```

这比“所有租户混一起”稳，也比“每个租户都独立 segment”省成本。

**如果共享，必须补哪些机制**

1. **record header 带 tenant id**

   每条 record 都能归属租户。

2. **tenant-aware index**

   只按 offset 索引不够。按租户查询、删除、统计时，要有额外索引或元数据。

3. **tenant quota**

   写入速率、存储量、读取 QPS、远端请求数都要限额。

4. **retention group**

   不要把 retention 差异很大的租户混在一个 segment。

5. **加密边界**

   如果合规要求租户独立密钥，就不要共享物理 segment，除非 record-level encryption 已经设计成熟。

6. **compaction 和 deletion 语义**

   compaction key 至少要包含 tenant id，避免不同租户同名 key 互相覆盖。

7. **监控和成本归因**

   共享 segment 也要能回答“哪个租户占了多少空间、制造了多少读写、触发了多少 compaction”。

**面试里的判断标准**

可以用几个问题快速判断：

- 租户 retention 是否不同？
- 是否需要租户级删除、导出、冻结？
- 是否要求租户独立加密密钥？
- 是否有大租户会造成热点？
- 是否需要按租户计费？
- segment 数量和对象数是否能承受？

如果这些问题里有多个答案是“是”，就倾向按 tenant 隔离 segment。否则可以共享，但要把 tenant id、quota、retention group 和安全边界补齐。

一句话：多租户日志系统至少要逻辑隔离；物理 segment 隔离适合大租户、强合规和差异化 retention。小租户可以共享 segment，但不能把安全、删除、计费和 compaction 这些问题留到以后再补。

## Q031. segment GC 如何避免误删仍被 snapshot 或 reader 引用的数据？

**回答：**

核心原则是：GC 不能只看“这个 segment 已经被新版本替代了”，还要看“有没有任何读视图还可能看到它”。换句话说，segment GC 应该分成两步：

1. **逻辑删除**

   先在新的 manifest 或元数据版本里把旧 segment 标记为 obsolete，使新的读请求不再选择它。

2. **物理删除**

   等确认没有 snapshot、reader、replica、remote upload、compaction job 还引用它之后，再真正删除文件或远端对象。

这和 LSM 系统删除旧 SSTable 的思路很像。compaction 产生新文件以后，旧文件从最新版本里消失，但旧版本可能还被 iterator、snapshot、正在执行的 Get 或 compaction 持有，所以不能马上 unlink。

**为什么会误删**

一个典型场景是：

```text
t1: reader R 获取 manifest version=10，准备读 segment S1
t2: compaction 把 S1 和 S2 合并成 S3
t3: manifest version=11 发布，S1/S2 在最新版本中不可见
t4: GC 只看最新 manifest，发现 S1/S2 已经 obsolete，于是删除
t5: reader R 继续按 version=10 读 S1，读到 ENOENT
```

这里的问题不是 compaction 错了，而是 GC 没有尊重旧读视图。只要系统允许读请求拿着一个旧版本继续跑，旧版本里的文件就必须被保护。

**常见保护机制**

1. **版本化 manifest**

   每次 compaction、retention、segment rolling 都不直接修改当前结构，而是生成一个新 manifest 版本：

   ```text
   manifest v10: S1, S2, S4
   manifest v11: S3, S4
   ```

   reader 开始时绑定某个 manifest 版本。只要这个版本还活着，里面引用的 segment 都不能物理删除。

2. **引用计数**

   每个 segment 或 manifest version 维护 refcount：

   ```text
   open_reader:
     version.ref++

   close_reader:
     version.ref--

   gc:
     if segment not in latest_manifest
        and segment not in any referenced_version
        and segment.ref == 0:
       delete(segment)
   ```

   真实系统里通常不会只给每个文件一个裸 refcount，而是让 reader pin 住一个 version，再由 version 引用一组文件。这样元数据更清晰。

3. **snapshot sequence 或 read timestamp**

   如果系统支持 snapshot read，就要记录 snapshot 的可见范围，例如：

   ```text
   snapshot_id
   read_sequence
   min_offset
   max_offset
   create_time
   owner
   ttl
   ```

   compaction 可以移除对最新版本不可见的数据，但不能移除仍被某个 snapshot 可见的数据。RocksDB 的 snapshot 也是这个思路：snapshot 固定一个 sequence number，读请求按这个 sequence number 看到当时的数据库状态；compaction 在处理旧版本记录时必须考虑这些 snapshot。

4. **epoch 或 generation**

   给 manifest 和 segment 都带 generation，防止 ABA 问题：

   ```text
   segment_id = topic/partition/base_offset/generation
   manifest_generation = 1842
   ```

   这样即使文件名或 base offset 被重复使用，也不会把新旧对象混淆。工程上更推荐 segment id 永不复用。

5. **删除状态机**

   segment 不应该只有 live/deleted 两种状态。更稳妥的是：

   ```text
   live -> obsolete -> delete_pending -> deleting -> deleted
   ```

   - `live`：当前 manifest 可见。
   - `obsolete`：被新版本替代，但可能仍被旧 reader 或 snapshot 引用。
   - `delete_pending`：已经不可见，且当前判断可以删除，等待后台删除线程执行。
   - `deleting`：正在删除本地文件或远端对象。
   - `deleted`：删除完成，并记录删除结果。

   如果删除失败，可以从 `deleting` 回到 `delete_pending` 重试。删除动作必须是幂等的。

**reader 的正确打开流程**

读路径要避免“先查到 segment，后面 segment 被删”的窗口。一个安全流程是：

```text
1. 读取当前 manifest 指针
2. pin 住 manifest version，或者增加 version refcount
3. 在这个 version 内查找 segment
4. 打开 segment 文件，必要时增加 segment refcount
5. 执行读取
6. 关闭文件，释放 segment/version 引用
```

不能这样做：

```text
1. 查当前 manifest，得到 segment path
2. 释放 manifest 锁
3. 过一会儿再 open(segment path)
```

第二种做法中间没有 pin，GC 可能已经把文件删了。

**snapshot 如何参与 GC**

snapshot 本质上是一个长期 reader。普通 reader 生命周期通常是毫秒到秒级，snapshot 可能持续分钟、小时，甚至更久。设计时要明确：

- snapshot 是否有 TTL。
- snapshot 是否可以跨重启保留。
- snapshot 是否可以被管理员强制释放。
- snapshot 保留的是 manifest version，还是保留 sequence/offset 范围。
- snapshot 是否会阻塞 compaction 删除 tombstone。
- snapshot 占用空间是否需要计入 quota。

如果允许无限期 snapshot，就必须接受一个事实：旧 segment 可能长期不能被删除，磁盘空间和读放大都会上升。很多存储系统会给 snapshot 做租户级配额、TTL 和告警，避免一个忘记关闭的 snapshot 把整个集群拖住。

**崩溃重启后的难点**

内存里的 refcount 重启后会丢失，所以不能把 refcount 当成唯一事实来源。重启恢复时一般要保守：

1. 读取最新 manifest 和可恢复的旧 manifest。
2. 读取持久化 snapshot metadata。
3. 扫描本地 segment 文件。
4. 把所有仍被 manifest 或 snapshot 引用的文件标为 live。
5. 对不被引用的文件先进入 `delete_pending`，可以加一个 grace period。
6. 对 `.tmp`、`.compacting`、`.uploading` 这类临时文件按状态机处理。

RocksDB 删除 stale files 时也有类似考虑：运行期可以靠版本引用计数保护旧文件；重启后引用计数不在了，就要从 MANIFEST 和文件编号边界重新判断哪些文件安全可删。

**对象存储上的 GC**

远端 segment 更适合用“元数据先行、异步删除”的方式：

```text
manifest v11 不再引用 remote object O1
GC metadata 记录 O1 delete_pending
后台 worker 调用 DeleteObject(O1)
删除成功后记录 deleted
```

不要让读路径直接依赖“对象是否存在”来判断可见性。对象存在但 manifest 不引用，应该不可读；manifest 引用但对象不存在，应该视为数据损坏或上传未完成。

**面试里可以抓住的重点**

- GC 的判断依据不是最新目录列表，而是所有活跃读视图。
- compaction 只负责产生新文件和发布新版本，不应该直接删除仍可能被引用的旧文件。
- snapshot 是长期 reader，必须进入空间治理和告警体系。
- 引用计数可以在内存里做，但恢复必须能从持久化 manifest 和 snapshot metadata 重建。
- 删除要异步、幂等、可重试，并且要有状态机。

一句话：segment GC 要用 MVCC 思路做，先把旧 segment 从新 manifest 里移除，再等所有 snapshot、reader 和后台任务都释放旧版本后再物理删除；只看“最新版本不用了”就删，是最容易出事故的做法。

## Q032. 如何衡量 compaction 对前台请求的影响？

**回答：**

不要只看 compaction 自己跑得快不快，而要看它对前台读写造成了多少额外代价。compaction 是后台任务，但它会抢磁盘带宽、IOPS、CPU、cache、锁、线程池和内存。衡量影响时，核心指标应该是：

```text
前台请求指标 = compaction 开启时的表现 - compaction 不开启或低负载时的基线
```

**前台请求侧指标**

最重要的是这些：

1. **延迟**

   - append latency p50/p95/p99/p999。
   - fetch latency p50/p95/p99/p999。
   - fsync latency。
   - index lookup latency。
   - remote read latency。

   compaction 对平均值的影响可能不明显，但对尾延迟很明显。面试里最好强调 p99 和 p999，而不是只说平均延迟。

2. **吞吐**

   - 前台写入 QPS 或 bytes/s。
   - 前台读取 QPS 或 bytes/s。
   - 每个 partition、tenant、topic 的吞吐。
   - broker 或节点级吞吐。

3. **错误和超时**

   - request timeout。
   - append rejected。
   - read timeout。
   - follower lag 或 replication lag。
   - consumer fetch lag。

4. **排队时间**

   - 请求在网络线程、写线程、读线程、磁盘队列里的等待时间。
   - segment lock 等待时间。
   - manifest lock 等待时间。
   - compaction scheduler queue 对前台 worker 的影响。

很多时候请求本身执行不慢，慢在排队。只看处理时间会低估 compaction 的影响。

**compaction 侧指标**

后台任务自身也要打点：

1. **工作量**

   ```text
   compaction_bytes_read
   compaction_bytes_written
   compaction_input_segments
   compaction_output_segments
   compaction_records_in
   compaction_records_out
   compaction_tombstones_dropped
   ```

2. **放大**

   ```text
   write_amplification = 后台写入字节 / 前台写入字节
   read_amplification  = 读请求平均需要访问的 segment 或 index 数
   space_amplification = 物理占用 / 逻辑有效数据
   ```

3. **积压**

   ```text
   pending_compaction_bytes
   obsolete_segment_bytes
   tombstone_bytes
   l0_like_segment_count
   compaction_score
   oldest_uncompacted_age
   ```

4. **耗时**

   ```text
   compaction_duration_seconds
   compaction_queue_wait_seconds
   compaction_pause_seconds
   compaction_retry_count
   compaction_cancel_count
   ```

RocksDB 的 compaction stats 里会输出每层文件数、大小、score、读写 GB、写放大、读写 MB/s、compaction 秒数、次数、平均耗时、stall 秒数和 key drop 情况，这类指标非常适合作为设计参考。

**资源侧指标**

compaction 影响前台，往往是通过资源竞争体现出来的：

- 磁盘读写带宽。
- 磁盘 IOPS。
- 磁盘队列深度。
- fsync 或 fdatasync 延迟。
- CPU 使用率，特别是压缩、解压、校验和计算。
- page cache 命中率。
- block cache 或 index cache 命中率。
- 内存分配和 GC 暂停。
- 网络带宽，特别是远端 segment 上传、下载和复制。
- 文件描述符数量。
- mmap 数量和 page fault。

如果 compaction 大量顺序读写，把 page cache 里的热点 index 和 active segment 挤出去，前台读延迟会突然升高。这类问题只看磁盘吞吐未必能发现，必须看 cache hit rate 和 major page fault。

**怎么做实验**

比较可靠的方式是做受控实验：

```text
场景 A: 前台负载固定，关闭 compaction 或把 compaction 降到很低
场景 B: 前台负载固定，开启正常 compaction
场景 C: 前台负载固定，模拟 backlog 很大时的 compaction
场景 D: 前台负载固定，开启限速、低优先级 IO 或分时调度
```

每个场景都记录同一组指标，然后比较：

- p99 延迟增加了多少。
- 写吞吐下降了多少。
- 超时率是否上升。
- disk busy 是否接近 100%。
- backlog 是否真的下降。
- compaction 消耗的资源是否换来了读放大或空间放大的改善。

只看单次压测不够，因为 compaction 是周期性和突发性的。最好把 compaction job 的开始、结束、输入文件、输出文件、字节数打到事件日志里，再和前台延迟曲线对齐。

**面试里容易漏掉的点**

1. **影响不一定发生在写请求上**

   compaction 是写盘任务，但它也会读旧 segment、占 page cache、重建 index，所以读请求也会被影响。

2. **影响不一定立刻出现**

   compaction 把缓存冲掉以后，前台读可能在几秒或几十秒后变慢。

3. **平均值经常骗人**

   后台 compaction 对 p50 可能没影响，但 p99 会抖。

4. **要看净收益**

   compaction 牺牲前台资源，是为了降低未来读放大和空间放大。如果 compaction 让前台 p99 翻倍，但读放大只降了一点，策略可能不值得。

一句话：衡量 compaction 影响，要把前台延迟、吞吐、错误、排队时间和后台 compaction 字节、放大、积压、资源占用放在同一张时间线上看；只说“compaction 吞吐多少 MB/s”是不够的。

## Q033. 后台 compaction 如何做限速？

**回答：**

后台 compaction 限速的目标不是让 compaction 越慢越好，而是在不压垮前台请求的前提下，把 backlog 控制在可接受范围内。常见做法是 token bucket、低优先级 IO、后台线程数控制、任务分级和自适应调节。

**最基本的 token bucket**

可以按字节限速：

```text
rate = 200 MB/s
bucket 每 100ms 补充 20MB token
compaction 每读写一批数据前申请 token
token 不足就等待
```

伪代码：

```text
while compacting:
  chunk = read_input(max_chunk)
  limiter.acquire(bytes=len(chunk), io_type=compaction_read)
  process(chunk)
  limiter.acquire(bytes=len(output), io_type=compaction_write)
  write_output(output)
```

RocksDB 的 Rate Limiter 也是类似思路，用 `rate_bytes_per_sec` 控制 flush 和 compaction 的写速率，并通过 refill period 和 fairness 调节等待与公平性。它还支持运行期调整速率。

**限速要分资源类型**

只限制写入字节不够。一个 segment compaction 可能同时消耗：

- 读带宽。
- 写带宽。
- CPU。
- 压缩线程。
- 校验和计算。
- cache。
- 网络上传带宽。
- 远端请求数。

所以更完整的限速可以分成：

```text
local_read_bytes_per_sec
local_write_bytes_per_sec
remote_read_bytes_per_sec
remote_write_bytes_per_sec
compression_cpu_quota
compaction_worker_count
per_tenant_compaction_quota
```

如果系统有冷热分层，本地 compaction 和远端上传最好不要共用一个粗糙的限速器，否则本地 backlog 和远端上传会互相影响。

**线程数限速**

有些瓶颈不是带宽，而是并发数。可以限制：

- 同一磁盘上的 compaction job 数。
- 同一 partition 的 compaction job 数。
- 同一 tenant 的 compaction job 数。
- 全局后台 job 数。
- 同时打开的 segment 文件数。

例如：

```text
global_compaction_workers = 4
per_disk_workers = 1
per_tenant_workers = 1
per_partition_workers = 1
```

这样可以避免一个大 tenant 或一个热点 partition 把整个后台线程池占满。

**优先级和调度**

后台任务不应该全都同一优先级。可以分层：

```text
P0: 前台读写
P1: flush active data 或必要 checkpoint
P2: 防止磁盘打满的紧急 compaction
P3: 普通 compaction
P4: 冷数据整理、远端归档、低优先级校验
```

当资源紧张时，低优先级任务让出资源。对于普通 compaction，可以在每处理一批记录后检查是否需要 yield：

```text
if foreground_latency_p99 > threshold:
  compaction.pause(short_interval)
```

注意不要在持有全局 manifest 锁、segment 写锁或文件 rename 关键区时 sleep。限速等待应该发生在可中断的小批处理边界。

**自适应限速**

固定限速很容易失效。白天业务高峰时 200 MB/s 可能太高，凌晨低峰时又太低。自适应策略可以根据反馈调整：

```text
if foreground_p99_latency > SLO:
  reduce compaction rate
  reduce workers

elif compaction_backlog growing and disk_idle:
  increase compaction rate
  increase workers within cap

elif disk_free below emergency_threshold:
  prioritize space-reclaim compaction
  apply write backpressure if needed
```

这里的关键是同时看前台 SLO 和 backlog。只看前台延迟会让 compaction 长期跑不动；只看 backlog 会把前台打爆。

**按租户限速**

多租户系统里，compaction 限速要能归因到租户：

```text
tenant A backlog 大 -> A 的后台 compaction 消耗 A 的 quota
tenant B 正常 -> B 的前台请求不能被 A 的 compaction 长期挤压
```

如果 shared segment 里混了多个 tenant，就要在 record 或 segment group 级别做归因，不然很难公平限速。

**常见坑**

1. **只限写不限读**

   compaction 读旧 segment 也会抢磁盘和 cache。读不限速时，前台读一样会抖。

2. **一次 chunk 太大**

   每次申请 1GB token 会造成长时间占用和大 burst。chunk 应该足够小，让限速器有机会平滑调节。

3. **限速时持锁**

   compaction 拿着 manifest lock 等 token，会把前台元数据操作也堵住。

4. **没有紧急通道**

   如果磁盘快满了，普通限速可能让系统来不及回收空间。需要有受控的 emergency mode。

5. **没有可观测性**

   限速器本身要暴露等待时间、授予 token 数、被拒次数和当前速率，否则很难解释延迟变化。

一句话：后台 compaction 限速通常用 token bucket 控制读写字节，用 worker 数控制并发，用优先级保护前台请求，再根据前台 SLO、磁盘空闲和 backlog 做动态调节。

## Q034. compaction backlog 过大时系统如何降级？

**回答：**

compaction backlog 过大说明后台整理速度跟不上前台写入或删除速度。它不是单纯的后台问题，最后会变成前台问题：读放大变大、空间放大变大、磁盘打满、写入变慢，严重时系统必须限流或拒绝写入。

**先定义什么叫 backlog 过大**

可以用这些指标判断：

```text
pending_compaction_bytes
obsolete_segment_bytes
tombstone_bytes
uncompacted_segment_count
oldest_uncompacted_age
space_amplification
read_amplification
disk_free_percent
compaction_score
foreground_latency_p99
```

不同系统阈值不一样，但可以分级：

```text
Level 0: 正常，backlog 可控
Level 1: backlog 增长，但前台 SLO 正常
Level 2: backlog 很大，读放大或空间放大明显上升
Level 3: backlog 威胁磁盘空间或前台 SLO
Level 4: 磁盘接近打满，必须保护数据安全
```

**Level 1：加快后台处理**

如果前台延迟正常、磁盘和 CPU 还有余量，可以提高 compaction 能力：

- 增加 compaction worker。
- 提高 compaction rate limit。
- 提高 backlog 大的 partition 优先级。
- 把小任务合并成更有效的批处理。
- 优先处理能释放最多空间或降低最多读放大的任务。

这一层不应该影响前台请求。

**Level 2：牺牲部分后台质量**

如果 backlog 继续变大，可以降低一些非关键后台任务：

- 暂停低优先级冷数据重排。
- 暂停全量校验、预热、低价值重写。
- 延后不紧急的压缩格式升级。
- 降低远端归档并发，给本地 compaction 让路。
- 选择更便宜的 compaction 策略，例如先做空间回收收益最高的 segment。

这里不是停止 compaction，而是让 compaction 做最有收益的部分。

**Level 3：对前台写入施加 backpressure**

如果 backlog 已经影响读放大、空间放大或磁盘空间，就要让写入端感知压力：

```text
if pending_compaction_bytes > soft_limit:
  increase append latency artificially
  reduce producer quota
  reject low-priority writes
```

RocksDB 的 write stall 就是类似机制：当 flush 或 compaction 跟不上时，写入会被减速甚至停止，避免 L0 文件过多、pending compaction bytes 过大或 memtable 堆积继续恶化。对调用方来说，这比把磁盘写满再失败更可控。

降级可以从轻到重：

1. 增加写入延迟。
2. 降低 tenant 或 topic 的写入 quota。
3. 对低优先级写入返回 retryable error。
4. 对超大 batch 做拆分或拒绝。
5. 暂停非核心 topic 的写入。

**Level 4：保护磁盘和一致性**

磁盘快满时，系统的第一目标是不要损坏数据。可以做：

- 拒绝新写入，保留读能力。
- 停止创建新的大文件，避免半写文件堆积。
- 优先删除已经确认无引用的 obsolete segment。
- 清理失败上传、临时文件和过期快照。
- 对 remote tier 已经安全保存的 sealed segment 做本地清理。
- 把集群状态标为 degraded，触发运维告警。

不能做的是：

- 为了腾空间删除仍被 snapshot pin 住的 segment。
- 提前丢 tombstone，导致旧值复活。
- 直接删 active segment 或未 checkpoint 的 WAL。
- 跳过 manifest 原子切换。

**读路径如何降级**

backlog 大时，读放大也会变大。读路径可以：

- 对超大 range read 做分页。
- 限制低优先级历史查询。
- 降低 backfill 并发。
- 优先服务近期热数据。
- 对冷数据读取使用更大的 batch 和预取。
- 对跨很多 segment 的查询返回可重试限流。

如果不限制历史扫描，一个后台 backlog 已经很大的系统可能被 backfill 再压一轮。

**调度策略**

backlog 很大时，compaction 不能简单 FIFO。更好的优先级通常是：

```text
1. 能避免磁盘打满的任务
2. 能显著降低读放大的任务
3. 能释放最多 obsolete bytes 的任务
4. 年龄太老、影响 retention 的任务
5. 普通整理任务
```

也可以按租户隔离：

```text
tenant A backlog 爆了 -> 限制 A 的写入，优先清 A 的 backlog
tenant B 正常 -> 不应被 A 完全拖垮
```

**面试里可以这样总结**

compaction backlog 降级不是一个开关，而是一组渐进策略：

- backlog 轻微增长时，加后台处理能力。
- 前台还稳时，优先消化收益最大的任务。
- backlog 影响 SLO 时，对写入做 backpressure。
- 磁盘风险出现时，拒绝部分写入，保护已提交数据。
- 全程不能破坏 snapshot、manifest 和 tombstone 的正确性。

一句话：compaction backlog 过大时，系统应该先提速后台，再压缩低价值任务，随后对写入和历史读做分级限流；真正危险时宁可拒绝新写入，也不能误删旧数据或让磁盘打满。

## Q035. 如何为 segment log 设计快速启动恢复？

**回答：**

快速启动恢复的关键是：不要在每次重启时全量扫描所有 segment。系统应该把恢复范围限制在“最后几个可能不完整的文件”和“上次 checkpoint 之后的 tail”。sealed segment 已经不可变，理论上不需要每次重扫。

**需要持久化哪些元数据**

至少要有：

```text
current_manifest_pointer
manifest_generation
log_start_offset
log_end_offset
last_stable_offset
last_sealed_segment
active_segment_id
active_segment_base_offset
recovery_point_offset
index_checkpoint
snapshot_metadata
```

其中 `recovery_point_offset` 很重要。它表示在这个 offset 之前的数据和索引已经完成必要的持久化校验，重启后可以从这里之后开始扫描。

**segment 文件本身要支持恢复**

每个 segment 最好有 header 和 footer：

```text
segment header:
  magic
  version
  segment_id
  base_offset
  create_time
  codec
  header_crc

record batch:
  base_offset_delta
  record_count
  length
  timestamp_range
  payload
  batch_crc

segment footer:
  end_offset
  max_timestamp
  index_crc
  data_crc_or_checksum_tree
  footer_crc
  sealed_marker
```

active segment 可能没有 footer，因为它还没 seal。sealed segment 应该有 footer，这样恢复时可以快速判断它是否完整。

**启动流程**

一个比较稳的流程是：

```text
1. 读取 CURRENT 或等价指针，找到最新 manifest
2. 校验 manifest checksum 和 generation
3. 加载 manifest 中的 segment metadata
4. 对 sealed segment 做轻量校验
5. 找到 active segment 和 recovery point
6. 从 recovery point 扫描 active segment tail
7. 截断最后一条不完整 record batch
8. 重建或修补 active segment 的 index
9. 清理 orphan tmp 文件和未发布文件
10. 发布恢复后的当前状态
```

这里的关键是第 6 步：只扫描 tail，不扫描全量历史。

**如何避免全量重建 index**

index 可以从 log 重建，但不代表每次都应该重建。可以做：

1. **index checkpoint**

   定期记录：

   ```text
   segment_id
   indexed_until_offset
   indexed_until_position
   index_file_size
   index_crc
   ```

   重启时从 checkpoint 之后补索引。

2. **sealed segment index 不变**

   sealed segment 的 data 和 index 都不可变，只要校验通过，就直接 mmap 或 lazy load index。

3. **active segment 增量扫描**

   active segment 只有尾部可能不一致，所以只从上次安全点继续。

4. **缺失 index 懒加载重建**

   如果某个冷 segment 的 index 缺失，不一定阻塞启动。可以先启动服务，把这个 segment 标成 `index_rebuilding`，真正读到它时再同步或异步补建。

**处理不一致**

重启时常见几种不一致：

1. **data 有，index 没有**

   以 data 为准，扫描 data 补 index。

2. **index 指向 data 尾部之后**

   index 是脏的，截断 index 或重建 index。

3. **最后一个 record batch 不完整**

   截断到最后一个完整 batch。

4. **manifest 引用了不存在的 segment**

   如果是 active segment，可能需要从副本恢复；如果是 sealed segment，说明发布或删除流程有严重问题，不能静默跳过。

5. **目录里有 manifest 未引用的 segment**

   先当 orphan 文件处理，不要立即删除。确认不被旧 manifest、snapshot、upload job 引用后再 GC。

**如何让 manifest 加速启动**

manifest 应该存足够的 segment metadata：

```text
segment_id
base_offset
end_offset
min_timestamp
max_timestamp
data_path
index_path
size_bytes
record_count
sealed
checksum
remote_location
```

这样启动时可以直接建立内存里的 offset 到 segment 映射，不需要从每个 segment 文件名和文件内容里猜。

**并行恢复**

如果 segment 很多，可以并行做轻量校验：

```text
主线程:
  加载 manifest，恢复 active tail，尽快对外提供服务

后台线程:
  校验冷 segment checksum
  重建缺失 index
  清理 orphan 文件
  预热热点 index
```

不过 active segment、manifest 指针和 log end offset 这些核心状态必须先恢复正确，不能为了快而让读写看到不一致状态。

**和 LevelDB/RocksDB 的类比**

LevelDB 有 MANIFEST、CURRENT、log file、SSTable 和 recovery 流程。它启动时通过 CURRENT 找 manifest，再根据 manifest 恢复文件集合，之后重放必要的 log。segment log 也可以借鉴这个思路：用 manifest 记录稳定文件集合，用 active tail scan 处理最后未封口的一小段。

一句话：segment log 快速启动靠 manifest、checkpoint、sealed segment 不可变和 active tail 增量扫描；全量扫描所有 segment 只能作为兜底修复手段，不应该是正常启动路径。

## Q036. segment rolling 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

segment rolling 的核心目标是把无限增长的 append-only log 切成一段段有限、可管理、通常不可变的文件。它不是为了一个单点目标存在，而是同时服务正确性、性能、安全性和可维护性；如果要选主目标，我会说它主要解决的是**性能和可维护性边界**，同时为正确性和安全性提供更清晰的工程边界。

**为什么不能一直写一个大文件**

如果一个 partition 永远只有一个 log 文件，后果很明显：

- index 会越来越大。
- 崩溃恢复需要扫描很长 tail，甚至扫描整个文件。
- retention 删除旧数据只能重写大文件，不能按块删除。
- compaction 输入过大，重写成本不可控。
- 复制、备份、上传远端都没有自然边界。
- 校验、修复和迁移都很笨重。
- 单个文件损坏的影响范围过大。

rolling 把问题变成：

```text
active segment: 接收新写入
sealed segment: 不再追加，只读、可复制、可压缩、可上传、可删除
```

这个边界非常重要。

**它对正确性的帮助**

segment rolling 本身不是日志正确性的唯一来源。即使没有 rolling，一个 append-only log 也可以通过 WAL、checksum、fsync 和 offset 顺序保证正确性。但 rolling 能让正确性边界更清楚：

- active segment 允许尾部不完整，恢复时扫描并截断 tail。
- sealed segment 应该完整、有 footer、有 checksum。
- retention 只删除完整 sealed segment，不碰 active segment。
- compaction 以 sealed segment 为输入，输出新 sealed segment。
- reader 可以知道某个 segment 的 offset 范围和时间范围。

所以它不是“没有就不正确”，而是“有了以后更容易把正确性做稳”。

**它对性能的帮助**

性能收益更直接：

1. **索引更小**

   每个 segment 一个局部 index，offset 查找先定位 segment，再在小 index 内二分。

2. **恢复更快**

   sealed segment 不需要重扫，只扫 active tail。

3. **删除更便宜**

   retention 可以删除整个旧 segment，而不是重写大文件。

4. **读写更可控**

   active segment 顺序写，sealed segment 只读，冷热数据可以分开管理。

5. **远端上传更自然**

   sealed segment 是对象存储的天然上传单位。

**它对安全性的帮助**

安全性不是 rolling 的第一目标，但 rolling 可以提供边界：

- 每个 segment 可以有独立 checksum。
- 每个 segment 可以有独立加密元数据。
- 可以按 segment 做合规保留和删除审计。
- 损坏范围可以限制在单个 segment。
- 远端对象可以和 manifest 校验绑定。

如果系统需要密钥轮换，segment rolling 也能作为密钥轮换边界：

```text
segment A 使用 key version 1
segment B 使用 key version 2
```

不过这属于 rolling 带来的附加能力，不是它最初的核心理由。

**它对可维护性的帮助**

可维护性是 rolling 的大头：

- retention 可以按文件执行。
- compaction 可以按 segment 调度。
- manifest 可以按 segment 管理元数据。
- 冷热分层可以按 segment 迁移。
- 修复可以针对单个 segment。
- 运维可以按 segment 观察大小、时间范围和 checksum。
- 测试可以围绕 active/sealed 状态机展开。

**面试里的分类**

可以这样回答：

```text
核心目标:
  把无限日志切成有限、可封闭、可索引、可删除、可迁移的物理单元。

主要解决:
  性能和可维护性。

同时支撑:
  正确性边界、恢复边界、安全校验边界和生命周期管理。
```

一句话：segment rolling 不是简单“日志轮转”，而是把无限追加流切成可管理的不可变单元；它最主要改善性能和可维护性，也让恢复、校验、retention、compaction 和分层存储有了清晰边界。

## Q037. segment rolling 的典型适用场景和不适用场景分别是什么？

**回答：**

segment rolling 适合有持续追加、顺序读取、按时间或 offset 保留、需要快速恢复和后台整理的数据流。不适合很小、随机更新为主、必须单记录即时物理删除，或者文件边界没有实际意义的场景。

**典型适用场景**

1. **消息队列和事件日志**

   例如 Kafka topic partition。数据按 offset 追加，consumer 按 offset 拉取，retention 按时间或大小删除旧 segment。rolling 是天然选择。

2. **WAL 和变更日志**

   数据库 WAL、CDC log、replication log 都需要持续追加和快速恢复。rolling 可以限制单个 WAL 文件大小，也方便归档和清理。

3. **审计日志**

   审计日志通常 append-only，按时间保留，读取多为范围扫描。segment 可以按小时、天或大小滚动。

4. **时序数据和指标数据**

   指标按时间写入，历史数据逐渐变冷。segment rolling 可以配合时间范围索引、压缩和冷存储。

5. **LSM 的文件化结构**

   memtable flush 成不可变 SSTable，后台 compaction 再合并。虽然 SSTable 不完全等同于 log segment，但“不可变文件 + manifest + compaction”的思想很接近。

6. **对象存储分层**

   sealed segment 可以上传到 S3 这类对象存储。本地只保留热数据，远端保存冷数据。

7. **多副本复制**

   sealed segment 是复制、校验和追赶的自然单位。落后副本可以按 segment 拉取，而不是逐条 record 补。

**适用的前提**

通常需要满足几条：

- 数据有单调递增的 offset、sequence 或 timestamp。
- 写入主要是追加。
- 读取经常按范围进行。
- 保留策略可以按较大块执行。
- 后台 compaction 或归档可以接受异步完成。
- 文件边界可以作为恢复、复制、校验或删除边界。

如果这些条件都不满足，rolling 的收益会下降。

**不适用或收益很低的场景**

1. **数据量很小**

   如果一天只有几 KB 日志，rolling 只会制造更多文件、更多元数据和更多测试复杂度。

2. **强随机更新**

   如果数据经常按 key 原地更新，且读取也是随机点查，append-only segment 不一定合适。更适合 B+Tree、page store 或 KV 引擎。

3. **必须单记录立即物理删除**

   如果合规要求某条记录删除后立刻从物理文件消失，segment 共享很多记录会带来麻烦。你要么做 record-level encryption 并销毁密钥，要么重写 segment，要么选择别的存储布局。

4. **超低延迟小型嵌入场景**

   如果系统只在本地进程里写少量数据，rolling、manifest、index、GC 可能比业务本身还复杂。

5. **没有范围读取和生命周期管理需求**

   如果只需要覆盖写一个当前状态文件，rolling 意义不大。

6. **事务必须跨任意位置原地修改**

   segment rolling 适合追加历史，不适合把同一个大文件当成随机页池来频繁改写。

**容易误用的场景**

1. **把 rolling 当成压缩**

   rolling 只是切文件，不会减少数据量。空间回收要靠 retention 或 compaction。

2. **把 rolling 当成可靠性保证**

   rolling 关闭文件不等于数据已经安全落盘。仍然需要 checksum、fsync、manifest 原子发布和恢复逻辑。

3. **按时间滚动但时间不可靠**

   如果机器时间回拨，按时间 rolling 可能产生不符合预期的 segment。需要同时用 size、offset 或 monotonic clock 做保护。

4. **segment 太小**

   太小会导致文件数、对象数、index 数暴涨，影响启动和 GC。

5. **segment 太大**

   太大会拖慢恢复、上传、删除和 compaction。

一句话：segment rolling 适合持续追加、范围读取、可按块保留和后台整理的日志型数据；如果 workload 是小数据、随机更新、单记录强删除或没有生命周期边界，rolling 可能只是增加复杂度。

## Q038. segment rolling 和相近概念最容易混淆的边界在哪里？

**回答：**

最容易混淆的地方是把 segment rolling、log rotation、compaction、retention、truncation、flush、checkpoint、partition 这些词混在一起。它们经常出现在同一个系统里，但解决的问题不同。

**segment rolling vs log rotation**

log rotation 通常指运维层面的日志轮转：

```text
app.log -> app.log.1 -> app.log.2.gz
```

它更关注文件大小、时间、压缩和清理。

segment rolling 是存储引擎内部的物理布局：

```text
00000000000000000000.log
00000000000001048576.log
00000000000002097152.log
```

它关注 offset 范围、索引、manifest、reader 可见性、retention、compaction 和恢复。两者都叫 rolling，但 segment rolling 是数据结构的一部分，不只是运维脚本。

**segment rolling vs compaction**

rolling 是切分：

```text
一个正在增长的 active segment -> 多个有边界的 sealed segment
```

compaction 是重写和整理：

```text
旧 segment A + B -> 新 segment C
丢弃被覆盖值、过期值或 tombstone
```

rolling 不会主动删除重复记录。compaction 才会改变物理数据布局并回收空间。

**segment rolling vs retention**

rolling 产生 segment，retention 删除 segment。

```text
rolling:
  写满或到时间 -> 关闭当前 segment，创建新 segment

retention:
  超过时间或大小 -> 删除旧 sealed segment
```

没有 rolling，retention 很难高效按块删除；但 rolling 本身不代表旧数据已经过期。

**segment rolling vs truncation**

truncation 是按 offset 截断日志，常见于副本回退、leader epoch 修正或恢复：

```text
truncate_to_offset(10500)
```

它可能截断 active segment，也可能删除后续 segment。rolling 只是自然创建新文件，不表示日志语义回退。

**segment rolling vs flush/fsync**

flush 或 fsync 解决持久化：

```text
把内核缓冲区或用户态缓冲区的数据刷到稳定存储
```

rolling 解决文件边界：

```text
当前文件不再追加，后续写新文件
```

关闭一个 segment 不等于它已经持久化，除非流程里明确包含 fsync data、fsync index、fsync manifest 和必要的 directory fsync。

**segment rolling vs checkpoint**

checkpoint 记录恢复点：

```text
recovery_point_offset = 123456
```

rolling 创建新 segment：

```text
new active segment base_offset = 123456
```

二者可以相关，但不是同一件事。系统可以在 rolling 时写 checkpoint，也可以按时间或字节单独 checkpoint。

**segment rolling vs partition/shard**

partition 是逻辑流：

```text
topic A partition 0
topic A partition 1
```

segment 是 partition 内的物理文件：

```text
partition 0:
  segment 0
  segment 1
  segment 2
```

增加 partition 会改变并行度和顺序语义；rolling segment 不改变 partition 的逻辑顺序。

**segment rolling vs snapshot**

snapshot 是读视图：

```text
在某个 sequence 或 offset 看见一致状态
```

rolling 是写入文件切换。rolling 之后，新 reader 可以读新 segment，旧 snapshot 仍可能需要旧 segment。所以 rolling 不能替代 snapshot，也不能绕过 snapshot 的 GC 保护。

**segment rolling vs SSTable flush**

二者都可能产生不可变文件，但语义不同：

- log segment 通常是按追加顺序存放 record。
- SSTable 通常是按 key 排序的不可变表。
- log segment 的索引常按 offset。
- SSTable 的索引常按 key block。
- log segment rolling 是追加文件滚动。
- memtable flush 是内存有序结构落成磁盘有序文件。

**面试里可以用一句分类**

```text
rolling: 创建边界
retention: 删除过期边界
compaction: 重写并合并边界
truncation: 按语义截断边界
checkpoint: 记录恢复边界
partition: 定义逻辑顺序边界
snapshot: 固定读视图边界
fsync: 保证持久化边界
```

一句话：segment rolling 最容易和“日志轮转、压缩、删除、截断、刷盘、分区”混淆；判断边界时只要问它是在创建文件边界、回收数据、重写数据、改变日志语义，还是记录恢复状态，就能分清。

## Q039. segment rolling 在高并发场景下可能出现哪些隐藏问题？

**回答：**

高并发下，segment rolling 最怕的不是“什么时候滚”这个判断，而是滚动过程中多个写线程、读线程、GC、compaction、replication 和 manifest 更新同时发生。隐藏问题通常出现在状态切换和可见性上。

**多个 writer 同时触发 rolling**

例如当前 active segment 快满了，两个写线程同时发现需要 rolling：

```text
writer A: size + batch > limit，准备 roll
writer B: size + batch > limit，也准备 roll
```

如果没有单 writer、roll lock 或 CAS 状态机，可能出现：

- 创建两个新 active segment。
- 两个新 segment 使用同一个 base offset。
- 一个 writer 继续写旧 segment，另一个 writer 写新 segment。
- manifest 只发布了其中一个 segment，另一个变成 orphan。
- offset 分配重复或出现空洞。

常见做法是把 active segment 状态做成原子状态机：

```text
ACTIVE -> ROLLING -> SEALED
```

只有一个线程能把 `ACTIVE` 改成 `ROLLING`。其他线程要么等待，要么重新读取新的 active segment。

**offset 分配和写入提交的竞态**

如果 offset 先分配，写入后提交，那么 rolling 时要处理：

```text
offset 100-199 分配给 batch A
batch A 写入失败
batch B 拿到 offset 200-299 并写入成功
```

系统要决定是否允许 offset gap。很多消息日志不能接受已提交日志里的空洞，所以 offset 分配、写入、索引更新和可见 log end offset 的推进必须有明确顺序。

**index 和 data 的可见性不一致**

一个危险顺序是：

```text
1. data 还没完全落盘
2. index 已经写入 offset -> position
3. reader 通过 index 读到未完整的 data
```

或者反过来：

```text
1. data 写完
2. index 没写完
3. reader 找不到刚写入的数据
```

后者通常可以通过 index 重建修复；前者可能让 reader 读到脏尾部。设计上要让 reader 只看 committed offset 范围，并用 record batch checksum 验证数据。

**reader 和 rolling 的竞态**

reader 开始读 active segment 时，后台刚好把它 seal：

```text
t1: reader 定位 active segment S
t2: writer seal S，创建 S2
t3: GC 或 compaction 看到 S 已 sealed，准备处理
t4: reader 继续读 S
```

正确做法是 reader pin 住 segment 或 manifest version。sealed 不代表可删除，也不代表不能读。

**manifest 更新成为瓶颈**

高并发下，如果每次 rolling 都需要拿全局 manifest 锁，很多 partition 同时 rolling 会造成：

- 写入等待 manifest 锁。
- reader 定位 segment 等待 manifest 锁。
- compaction 发布新文件等待 manifest 锁。
- GC 扫描等待 manifest 锁。

可以用分区级 manifest、append-only manifest log、copy-on-write 元数据或批量发布来降低争用。

**rolling herd**

如果所有 partition 都按同一个时间间隔滚动，例如每小时整点 rolling，会出现：

- 同时创建大量文件。
- 同时写 footer 和 index。
- 同时 fsync directory。
- 同时上传远端对象。
- 同时触发 compaction 和 GC。

解决方式是加 jitter：

```text
segment.ms = 1h ± random_jitter
```

或者把 rolling 条件设计成 size/time 双条件，避免所有 partition 同步。

**文件描述符和 mmap 压力**

高并发 rolling 会制造大量新文件：

- 新 data file。
- 新 index file。
- 新 time index。
- 新 transaction index。
- 临时文件。

如果 reader 还 pin 着旧 segment，FD 和 mmap 数量会上升。系统需要有 FD cache、index lazy load、reader 超时和资源告警。

**和 compaction/GC 的竞态**

compaction 不应该处理仍在写的 active segment。GC 不应该删除刚 seal 但还被 reader 或 upload job 引用的 segment。rolling 状态必须被后台任务正确识别：

```text
ACTIVE: 不可 compaction，不可 retention delete
ROLLING: 不可读作 sealed，不可删除
SEALED: 可读，可作为 compaction 输入，可上传
OBSOLETE: 新 manifest 不可见，等待引用释放
```

**多租户公平性**

某个大 tenant 高频 rolling，可能导致：

- manifest 更新频繁。
- 小 segment 过多。
- compaction backlog 增大。
- 远端上传队列被占满。
- 其他 tenant 的读写被拖慢。

所以 rolling 也要纳入 tenant quota 和调度，不只是 append 请求需要限流。

一句话：高并发下 segment rolling 的隐藏问题集中在“只有一个线程能切换 active segment、reader 必须 pin 住旧视图、index/data/manifest 可见性顺序要一致、后台任务不能误判状态”这几件事上。

## Q040. segment rolling 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

segment rolling 的边界条件主要出现在“旧 segment 还没完全 seal，新 segment 已经创建，manifest 还没发布，index 可能只写了一半”的中间状态。正常路径看起来只是关闭旧文件、打开新文件；故障路径里，每一步都可能只完成一半。

**崩溃在写 record 中间**

场景：

```text
旧 active segment 正在写最后一个 batch
进程崩溃
batch 只写了一半
```

恢复时要扫描 tail，找到最后一个完整 record batch，校验 length 和 checksum，然后截断半条数据。不能把半条数据暴露给 reader。

**崩溃在 data 写完、index 未写完之后**

场景：

```text
data file 有 offset 1000-1099
index 只记录到 offset 1049
```

恢复时以 data 为准补 index。这个情况一般可修复，因为 data 是事实来源。

**崩溃在 index 写完、data 未持久化之后**

场景：

```text
index 指向 offset 1099 的 position
data 实际只持久化到 offset 1049
```

恢复时必须校验 index 指向的位置是否有完整 batch。如果没有，截断 index 或重建 index。不能相信 index 比 data 更可靠。

**崩溃在 seal 旧 segment 中间**

seal 可能包含：

```text
写 footer
写最后的 index entry
trim index 文件
fsync data
fsync index
rename tmp -> final
更新 manifest
fsync manifest
```

崩溃后可能看到：

- data 完整但 footer 缺失。
- footer 有但 checksum 不对。
- index 完整但 manifest 没发布。
- manifest 发布了但 directory fsync 没做。
- 旧 segment 状态不确定。

恢复策略是：manifest 决定可见集合，文件 header/footer/checksum 决定文件是否完整。对状态不确定的 segment，宁可回到 active tail 扫描或标记 repair，也不要直接当成 sealed。

**崩溃在创建新 active segment 之后、manifest 发布之前**

场景：

```text
旧 segment S1 seal 完成
新 segment S2 创建成功
进程崩溃
manifest 仍指向 S1 为 active，或没有记录 S2
```

恢复时可能发现 S2 是空文件或只有 header。处理方式：

- 如果 S2 没被 manifest 引用且没有 committed data，可以删除或作为 orphan 处理。
- 如果 S2 有 committed data，但 manifest 未引用，必须根据 offset、checksum 和提交点决定是接纳还是进入修复流程。
- 不能简单按文件名最大就认为它是最新 active segment。

**崩溃在 manifest 发布之后、旧状态清理之前**

场景：

```text
manifest v11 已经发布
旧 manifest v10 还在
旧临时文件还在
旧 segment 还没加入 GC 队列
```

这通常是可接受的。恢复时读取 CURRENT 指向的最新完整 manifest，把其他文件当作旧版本或 orphan。旧文件后续由 GC 清理。

**超时和重试导致重复 rolling**

如果 rolling 被封装成一个内部 RPC 或后台任务，调用方可能超时重试：

```text
roll(segment=S1, new_base_offset=2000) 超时
调用方重试同一个 roll
```

如果操作不是幂等的，可能创建两个 base offset 相同的新 segment。解决方式是给 rolling 操作稳定 id：

```text
roll_id = partition_id + old_segment_id + new_base_offset + generation
```

重复执行同一个 `roll_id`，应该返回同一个结果，而不是创建新文件。

**远端上传重试**

sealed segment 上传对象存储时也会遇到：

- 上传成功，但本地没记录成功。
- 本地记录成功，但 manifest 没发布。
- multipart upload 未完成。
- 重试上传了同一个 segment 的两个对象。

比较稳的做法是：

- 远端 key 包含 segment id 和 checksum。
- 上传完成后校验对象长度和 checksum。
- manifest 只引用完整上传并校验通过的对象。
- 未完成 multipart upload 由后台清理。
- 发布 manifest 是最后一步。

**磁盘满或权限错误**

rolling 时如果新 segment 创建失败，不能先把旧 active 关闭。否则系统既不能继续写旧文件，也没有新文件可写。更稳的顺序是：

```text
1. 预创建新 segment tmp
2. 写 header
3. fsync 必要元数据
4. 确认可用后，再 seal 旧 segment 并切换 active
```

如果磁盘不足，要对前台写入 backpressure 或拒绝，而不是进入半切换状态。

**时间回拨**

按时间 rolling 时，如果机器时间回拨，可能出现：

- segment 时间范围倒退。
- retention 判断错误。
- segment 文件名按时间排序异常。
- 同一时间戳生成重复名称。

所以时间条件最好只是 rolling 触发条件之一，文件身份仍然使用 base offset、generation 或 sequence。

**恢复时的一条总规则**

故障恢复时优先级可以这样排：

```text
已提交 offset 和完整 record batch > index > 文件名 > 目录扫描猜测
manifest 完整版本 > 未完成 manifest
checksum/footer > 单纯文件大小
```

一句话：segment rolling 的故障边界集中在 data、index、footer、manifest 和新旧 active 切换的持久化顺序上；要靠 checksum、幂等 roll id、manifest 原子发布、tail scan 和 orphan 清理把每个半完成状态都收敛到一致状态。

## Q041. segment rolling 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

segment rolling 本身看起来只是“关掉当前文件，打开下一个文件”，但线上瓶颈通常不是单一来源。粗略排序可以这样看：

```text
最常见: I/O 和锁竞争
次常见: mmap、FD、page cache 这类内存/内核资源
视实现而定: CPU
分层存储场景中: 网络可能成为滚动后的连带瓶颈
```

如果把 rolling 做得很轻，只创建新文件、切换 active 指针、更新少量元数据，那么 CPU 不是主瓶颈。真正容易把前台请求打抖的是 fsync、目录元数据、索引文件 remap、全局锁和远端上传。

**I/O 瓶颈**

rolling 常见的 I/O 动作包括：

- active segment 写完最后一批数据。
- offset index、time index 写入最后条目。
- index 预分配文件 trim 到有效大小。
- data file、index file、manifest 或 checkpoint flush。
- 创建新 segment 文件和新 index 文件。
- 必要时 fsync data、index 和目录。
- 删除或重命名旧临时文件。

这些动作本身不一定大，但很容易集中爆发。Kafka 的 `segment.jitter.ms` 就是为了避免大量 partition 同一时间 rolling，形成 thundering herd。一个系统如果每小时整点统一滚 segment，磁盘元数据操作、page cache 写回和远端上传会同时起来，p99 延迟会很难看。

I/O 瓶颈的症状通常是：

```text
append p99/p999 突然上升
fsync latency 上升
disk await 或 queue depth 上升
dirty page 写回变多
segment roll duration 变长
```

**锁竞争**

rolling 修改的是 active segment 指针和 segment 列表，通常要拿锁。高并发写入时，瓶颈经常出在：

- append lock。
- segment roll lock。
- manifest lock。
- index append lock。
- segment map 或 interval tree 的写锁。
- remote metadata 发布锁。

Kafka 的 `LogSegment.append` 注释里明确假设调用方已经在锁内，因为它不是线程安全的。这个细节很有代表性：rolling 和 append 的边界不是随便并发执行的，必须由上层保证写入顺序。

锁竞争的症状：

```text
CPU 不高，但请求排队时间高
append throughput 上不去
roll duration 本身不长，但等待 roll lock 的时间长
大量线程阻塞在同一个 partition 或 manifest 锁上
```

解决思路不是简单“加线程”，而是缩小临界区。比如先在锁外预创建文件，锁内只做 active 指针切换；远端上传不要放在写锁里。

**内存和内核资源**

segment rolling 会制造新文件，也会让旧文件从 active 状态变成 sealed 状态。关联资源包括：

- 文件描述符。
- mmap 映射。
- index cache。
- page cache。
- 目录项和 inode cache。
- 后台 reader pin 住的旧 segment。

如果 segment 太小，文件数暴涨，FD 和 mmap 会成为瓶颈。Kafka 的 offset index 文件会预分配，roll 后再缩小到有效大小；这类设计能减少运行时频繁扩容，但也意味着 rolling 会触发 index 文件 resize、unmap/remap 或 trim。

内存侧症状：

```text
open file 数量接近上限
mmap 数量很高
major page fault 增多
index cache 命中率下降
启动加载 segment metadata 变慢
```

**CPU 瓶颈**

rolling 本身不一定吃 CPU，但下面这些实现会让 CPU 变成问题：

- rolling 时计算整段 checksum。
- rolling 时压缩或重压缩 segment。
- rolling 时同步构建较重的二级索引。
- rolling 时扫描 segment 尾部做验证。
- rolling 后立即触发 compaction。
- rolling 后做加密封装或签名。

这些工作最好从 rolling 临界路径里拆出去。active 切换只做必要动作，完整 checksum、上传、冷数据索引构建可以异步跑，但要用 manifest 状态区分“已 seal”和“已归档完成”。

**网络瓶颈**

单机本地日志里，network 通常不是 rolling 的直接瓶颈。分布式和分层存储里就不一样：

- sealed segment 可能立即上传对象存储。
- follower 可能按 segment 追赶。
- remote log metadata 需要发布。
- 跨机复制或校验可能被 rolling 触发。

如果把远端上传同步放进 rolling 路径，前台 append 延迟会被网络尾延迟拖住。更稳的做法是：

```text
roll 完成本地 seal
manifest 标记 segment 可上传
后台上传 sealed segment
上传校验通过后发布 remote metadata
```

**怎么判断真实瓶颈**

面试里可以说，不猜，直接拆指标：

```text
roll_total_duration
roll_wait_lock_duration
roll_create_file_duration
roll_flush_duration
roll_index_trim_duration
roll_manifest_publish_duration
roll_remote_enqueue_duration
append_latency_during_roll
active_segment_count
fd_count
mmap_count
```

如果 `roll_wait_lock_duration` 高，是锁。`roll_flush_duration` 高，是 I/O。`remote_enqueue` 不高但上传队列堆积，是网络或远端服务。CPU 高且压缩线程满，说明 rolling 路径里混入了计算任务。

一句话：segment rolling 的瓶颈最常见来自 I/O 和锁竞争；内存资源会在小 segment 或大量 partition 下放大问题；CPU 和网络通常不是 rolling 的本体瓶颈，但一旦把 checksum、压缩、上传、索引构建塞进同步路径，它们就会变成前台延迟来源。

## Q042. segment rolling 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

这三类测试目标不同，不能混在一起看。

```text
correctness test: 证明语义对不对
stress test: 找并发、故障和资源边界下的隐藏 bug
benchmark: 量化性能和参数 trade-off
```

correctness test 不关心极限吞吐，benchmark 也不能替代崩溃恢复测试。线上 segment rolling 的事故通常不是一个 happy path 单测能挡住的。

**correctness test 测什么**

正确性测试要围绕不变量写。最少要覆盖：

1. **offset 范围**

   ```text
   segment[i].base_offset < segment[i].end_offset
   segment[i].end_offset == segment[i+1].base_offset
   没有重叠，没有空洞
   ```

2. **active segment 唯一**

   任意时刻每个 partition 只能有一个 active segment。sealed segment 不再追加。

3. **roll 条件**

   - 超过 `segment.bytes` 后滚动。
   - 超过 `segment.ms` 后滚动。
   - index 满后滚动。
   - 单条 batch 大于剩余空间时要么允许越界写入后滚动，要么先滚动再写，规则必须固定。

4. **index 正确**

   稀疏 index 查到的是不大于目标 offset 的最近位置。然后从这个位置顺序扫描，必须能找到目标 offset 或确认目标不存在。

5. **读写并发语义**

   rolling 过程中，reader 不能读到半条 record，不能因为旧 active 变 sealed 就读失败。

6. **恢复语义**

   崩溃后重启：

   - 半条 record 被截断。
   - data 有但 index 没有时能补 index。
   - index 有但 data 无效时能丢弃 index。
   - orphan segment 不会被当成已提交数据。

7. **retention 和 GC**

   retention 只能删除安全的 sealed segment，不能删除 active segment，也不能删除仍被 reader 或 snapshot 引用的 segment。

这类测试最好用小 segment size 和小 index interval，让测试在几百条 record 内触发多次 rolling。

**stress test 测什么**

stress test 要制造混乱：

1. **高并发写入**

   多个 producer 同时写同一个 partition，看是否会出现：

   - 两个 active segment。
   - 重复 base offset。
   - offset 空洞。
   - index 倒序。
   - log end offset 回退。

2. **读写和 rolling 并发**

   一边滚动，一边随机 range read、point read、tail read，检查读到的数据是否和模型一致。

3. **故障注入**

   在这些点 kill 进程：

   ```text
   data append 后
   index append 后
   footer 写入后
   新 segment 创建后
   manifest 发布前
   manifest 发布后
   旧文件 GC 前
   ```

4. **I/O 异常**

   模拟 ENOSPC、EIO、权限错误、rename 失败、fsync 失败、目录不可写。

5. **时间异常**

   模拟时间回拨、时间跳跃、多个 partition 同时按时间 rolling。

6. **资源压力**

   限制 FD、降低磁盘速度、制造 page cache 压力、让大量 reader pin 住旧 segment。

stress test 的输出不是单纯 pass/fail，还应该保留随机种子、操作日志、崩溃点和恢复后的 segment manifest，方便复现。

**benchmark 测什么**

benchmark 要回答参数怎么选。至少测：

1. **写入吞吐和尾延迟**

   ```text
   append MB/s
   append p50/p95/p99/p999
   roll 发生时的 latency spike
   ```

2. **segment size 的影响**

   - 小 segment：更多文件、更频繁 rolling、更快 retention。
   - 大 segment：文件少、rolling 少，但恢复、删除、上传更慢。

3. **index interval 的影响**

   - index 越密，随机 seek 越快，index 文件越大。
   - index 越稀，index 小，但定位后扫描更长。

4. **启动恢复时间**

   ```text
   clean shutdown recovery time
   unclean shutdown recovery time
   active tail scan bytes
   index rebuild time
   ```

5. **读路径性能**

   - point read。
   - range read。
   - 跨 segment read。
   - 冷热混合读。

6. **后台任务影响**

   rolling 后如果触发上传、compaction、GC，要测它们对前台延迟的影响。

**一个简化测试矩阵**

```text
segment.bytes: 1MB / 64MB / 1GB
index.interval.bytes: 512B / 4KB / 64KB
producer concurrency: 1 / 16 / 128
reader concurrency: 0 / 16 / 128
crash mode: clean / kill -9 / fsync fail / disk full
remote tier: off / async upload / sync upload
```

不用一开始就跑全矩阵，但这些维度要想清楚。

一句话：correctness test 查不变量和恢复语义，stress test 用并发、故障和资源压力找竞态，benchmark 量化 segment size、index interval、rolling 频率对吞吐、尾延迟、恢复时间和读放大的影响。

## Q043. 如果要求从零实现一个简化版 segment rolling，你会先定义哪些不变量？

**回答：**

我会先写不变量，再写代码。segment rolling 的代码不难，难的是每次切换 active segment 后，读、写、恢复、GC 都还能说得清楚。一个简化版至少要有这些不变量。

**offset 和 segment 范围不变量**

每个 segment 负责一个半开区间：

```text
segment.range = [base_offset, end_offset)
```

必须满足：

```text
base_offset < end_offset，空 active segment 除外
segment 按 base_offset 严格递增
sealed segment 之间不重叠
sealed segment 之间没有已提交 offset 空洞
active segment 的 base_offset 等于最后一个 sealed segment 的 end_offset
log_end_offset 等于下一条待分配 offset
```

半开区间比闭区间清楚。`end_offset` 表示下一条 offset，而不是最后一条 offset。

**active segment 不变量**

```text
每个 partition 最多一个 active segment
active segment 可以 append
sealed segment 不能 append
active segment 可以没有 footer
sealed segment 必须有完整 footer 或等价 sealed marker
```

rolling 的状态机可以写成：

```text
ACTIVE -> ROLLING -> SEALED
               \
                -> ROLL_FAILED -> ACTIVE
```

没有成功创建新 active segment 之前，不要把旧 active 切死。

**写入提交不变量**

写入要区分“分配了 offset”和“对 reader 可见”：

```text
reserved_offset <= written_offset <= committed_offset <= log_end_offset
```

简化实现可以不暴露 reserved 状态，只在 batch 完整写入 data、通过 checksum、必要 index 更新完成后推进 committed offset。reader 只能读到 committed offset 之前。

**index 不变量**

稀疏 index 不需要覆盖每条记录，但每条 index entry 必须满足：

```text
entry.offset >= segment.base_offset
entry.offset < segment.end_offset
entry.position 是 record batch 起始位置
entry.offset 单调递增
entry.position 单调递增
lookup(target) 返回 offset <= target 的最大 entry
```

index 不能指向未提交数据。index 缺失可以降级扫描，index 错指向无效位置必须能被检测并重建。

**文件持久化不变量**

data 是事实来源，index 是加速结构：

```text
data 可以重建 index
index 不能反过来证明 data 有效
```

这和 Kafka `OffsetIndex` 的设计一致：index 文件本身不做 checksum，崩溃后可以重建。简化版也应该把 checksum 放在 record batch 或 segment data 上，而不是把 index 当成权威。

**manifest 不变量**

如果有 manifest，必须满足：

```text
manifest 只引用完整 segment
manifest generation 单调递增
current pointer 要么指向旧完整 manifest，要么指向新完整 manifest
不能指向半写 manifest
```

发布顺序：

```text
写新 segment / 新 index
校验
写新 manifest tmp
fsync manifest tmp
atomic rename
fsync directory
切换 current pointer
```

简化实现可以不用复杂 manifest，但至少要有可恢复的 segment 列表和 active 标记。

**reader 不变量**

reader 看到的是某个稳定视图：

```text
reader 开始时绑定 segment list version
读过程中这个 version 里的 segment 不会被删除
reader 不能读 committed_offset 之后的数据
```

如果暂时不做 MVCC，也要用读写锁保证 rolling 和 reader 定位 segment 不会互相踩。

**retention 和 GC 不变量**

```text
active segment 不能被 retention 删除
仍被 reader pin 住的 segment 不能删除
manifest 最新版本不可见，不等于可以立即物理删除
删除必须幂等
```

**恢复不变量**

重启后必须收敛到一个合法状态：

```text
只有完整 record batch 可见
半条 record 被截断
orphan tmp 文件不会进入可见日志
index 可以从 data 重建
log_end_offset 根据最后一条完整 committed record 推导
```

**最小实现顺序**

我会先写这几个类型：

```text
SegmentId
SegmentMetadata
SegmentState
SegmentIndex
SegmentManifest
Log
```

再让测试围绕不变量跑，而不是围绕实现细节跑。

一句话：从零实现 segment rolling，先定义 offset 连续性、唯一 active、sealed 不可变、index 只加速不权威、manifest 原子发布、reader pin 视图、GC 不误删、崩溃后只暴露完整 record 这些不变量；代码只是把这些不变量落地。

## Q044. segment rolling 的常见误用是什么，误用后通常会产生什么线上症状？

**回答：**

segment rolling 最常见的误用，是把它当成一个简单的文件轮转参数，而不是存储引擎的生命周期边界。参数配错、语义理解错、同步路径放错，线上症状会很明显。

**segment 太小**

误用：

```text
segment.bytes 配得很小
segment.ms 配得很短
每个小 topic、小 tenant 都独立滚 segment
```

症状：

- 文件数暴涨。
- FD 和 mmap 接近上限。
- 启动扫描慢。
- GC 和 retention 线程忙。
- 对象存储里小对象数量爆炸。
- compaction job 数量过多。
- p99 延迟出现周期性抖动。

很多系统一开始为了“删除更精细”把 segment 设得很小，最后付出的代价是元数据和后台任务爆炸。

**segment 太大**

误用：

```text
为了减少文件数，把 segment.bytes 配得过大
```

症状：

- retention 粒度太粗，过期数据迟迟删不掉。
- 单个 segment 上传远端很慢。
- 崩溃恢复扫描尾部时间变长。
- compaction 输入过大，任务不可预测。
- 单文件损坏影响范围更大。
- 删除一个大 segment 会造成明显 I/O 抖动。

Kafka 的 `segment.bytes` 文档也点出了这个取舍：更大的 segment 文件更少，但 retention 控制更粗。

**把 rolling 当成 fsync**

误用：

```text
认为 segment roll 之后数据就一定安全落盘
```

症状：

- 断电后最后一个 sealed segment 缺数据。
- index 指向不存在的位置。
- manifest 引用的文件未真正持久化。
- 重启后 log end offset 回退。

rolling 只是边界切换。持久性还要靠 record checksum、fsync、目录 fsync、manifest 发布顺序和恢复扫描。

**把 rolling 当成 compaction**

误用：

```text
以为滚动后旧数据自然释放空间
```

症状：

- 磁盘占用持续增长。
- tombstone 长期存在。
- 读放大越来越大。
- retention 看似配置了，但 active segment 或大 segment 阻止删除。

rolling 只产生文件边界。空间回收要靠 retention 或 compaction。

**所有 partition 同时 rolling**

误用：

```text
segment.ms = 1h
所有 partition 从整点开始计时
没有 jitter
```

症状：

- 每小时整点延迟尖刺。
- 磁盘写回尖刺。
- 远端上传队列尖刺。
- manifest 更新尖刺。
- follower fetch 或 backfill 抖动。

Kafka 提供 `segment.jitter.ms`，就是为了避免这种滚动羊群效应。

**rolling 路径里同步做太多事**

误用：

```text
roll 时同步压缩、同步上传、同步全量 checksum、同步构建冷数据索引
```

症状：

- append p99 被网络或 CPU 拖住。
- roll duration 很长。
- 写线程长时间持有锁。
- producer timeout 增多。

正确做法是把本地 seal 和后续归档拆开，状态机里区分 `SEALED`、`UPLOADING`、`REMOTE_READY`。

**不区分 active 和 sealed**

误用：

```text
retention 或 compaction 扫描目录，看到旧文件就处理
```

症状：

- active segment 被误删。
- reader 读到 ENOENT。
- compaction 输入包含正在写的文件。
- offset range 断裂。

**稀疏 index 配错**

误用：

```text
index.interval.bytes 过大或过小
```

症状：

- 过大：seek 后扫描太长，随机读延迟高。
- 过小：index 文件大，mmap 和 page cache 压力高。

Kafka 的 `index.interval.bytes` 文档说明了这个权衡：更频繁的索引让读取跳得更近，但 index 文件更大。

一句话：segment rolling 误用后，线上症状通常集中在文件数爆炸、启动慢、retention 不生效、p99 周期性尖刺、远端上传积压、index/cache 压力和崩溃恢复不一致；看到这些症状时，要先查 segment size、roll interval、jitter、同步路径和 active/sealed 状态机。

## Q045. segment rolling 在单机和分布式环境中的语义有什么差异？

**回答：**

单机环境里，segment rolling 主要是本地文件布局问题。分布式环境里，它仍然是物理布局，但会被 leader/follower、high watermark、epoch、remote tier、replication lag 和元数据发布放大。最重要的一点是：**segment 边界不应该成为客户端可依赖的分布式语义，offset 和 commit 才是语义边界。**

**单机语义**

单机日志里，rolling 影响这些本地状态：

```text
active segment
sealed segment list
offset index
time index
manifest 或目录元数据
log_start_offset
log_end_offset
```

只要本机保证：

- offset 连续。
- active segment 唯一。
- index 可重建。
- retention 不误删。
- 崩溃后能从本地文件恢复。

系统就能闭环。读写都在一台机器上，rolling 的可见性问题相对简单。

**分布式语义**

分布式日志里还要考虑：

- leader 分配 offset。
- follower 复制 record。
- high watermark 决定哪些 offset 已提交。
- leader epoch 处理切主和截断。
- follower 可能落后或需要回退。
- remote tier 可能保存 sealed segment。
- controller 或 metadata quorum 可能记录远端 segment 状态。

这时 rolling 不能改变复制语义。一个 record 是否对 consumer 可见，取决于提交边界，而不是它所在 segment 是否 sealed。

**leader 和 follower 的差异**

leader 的 rolling 往往由本地条件触发：

```text
segment.bytes
segment.ms
index full
offset relative range overflow
```

follower 复制的是 leader 的数据流，但它本地是否在同一个 offset 边界 rolling，要看实现。很多系统会让同一 partition 的副本使用一致配置，尽量形成相同 segment 边界，方便复制和修复。但协议语义上，客户端不应该假设 segment 边界一致。

如果系统需要按 segment 复制或远端归档，那就要把 segment metadata 变成复制协议或远端元数据的一部分，否则 follower 只能按 offset 流重放。

**high watermark 的影响**

分布式环境里，一个 segment sealed 不代表里面所有数据都已提交。可能出现：

```text
leader 本地 segment S sealed
S 的后半部分还没有被足够副本复制
high watermark 还没越过 S.end_offset
```

这时：

- consumer 不能读未提交部分。
- remote upload 可以先做，但 remote metadata 对读路径可见要谨慎。
- retention 不能只看本地 rolling 状态，还要看提交和复制状态。

Kafka `UnifiedLog` 里 high watermark 会受 log start 和 log end 边界约束，`logStartOffset` 又可能由 retention、truncation、recovery 等更新。这说明在分布式日志里，segment 文件边界只是其中一个状态。

**切主和截断**

单机 rolling 后，一般不会有人要求你把日志回退到旧 epoch。分布式里会发生：

```text
leader A 写到 offset 1000 并滚了 segment
leader A 挂掉
leader B 成为新 leader，但只复制到 offset 900
旧 leader 恢复后必须截断到 900
```

这会暴露很多边界：

- 截断点在 active segment 中间。
- 截断点在 sealed segment 中间。
- index 要跟着截断。
- segment footer 可能失效。
- remote 已上传的 segment 可能包含未提交数据。

所以分布式环境里，rolling 不能只考虑本地“文件已经 seal”，还要考虑 epoch 和 quorum 提交。

**remote tier 的差异**

单机只有本地文件，分布式分层存储里还会有：

```text
local sealed segment
remote object
remote segment metadata
remote log start offset
local retention
remote retention
```

Kafka tiered storage 的实现思路是把 completed log segment 复制到远端，并让本地和远端形成一个统一视图。这里 rolling 是远端上传的触发边界，但远端可读还需要 metadata 发布成功。

**故障域差异**

单机：

```text
一次 rolling 失败，影响本机这个 log
```

分布式：

```text
一次 rolling 或远端 metadata 错误，可能影响副本追赶、leader 切换、远端读取、跨 AZ 恢复
```

所以分布式环境里，rolling 操作要具备：

- 幂等 segment id。
- 可重试远端发布。
- leader epoch 校验。
- follower 截断修复。
- remote metadata 原子可见。
- 本地和远端 retention 协调。

一句话：单机 segment rolling 是本地文件生命周期管理；分布式环境中，它还要和 offset 提交、leader epoch、复制、截断、远端 metadata 和多副本恢复配合。客户端语义应该绑定 offset 和 high watermark，而不是绑定某个 segment 是否已经滚动。

## Q046. sparse index 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

sparse index 的核心目标是用更小的索引，把读取定位到“足够接近目标的位置”，再顺序扫描一小段数据找到目标。它主要解决的是**性能问题**，具体说是降低索引文件大小、内存占用和 page cache 压力，同时保留较快的 seek 能力。

它不是正确性的根基。正确性应该由 log data、record batch length、offset、checksum 和恢复逻辑保证。sparse index 错了，可以重建；data 错了，问题就大得多。

**dense index 和 sparse index 的差别**

dense index：

```text
每条 record 都有一条 index entry
offset -> physical_position
```

sparse index：

```text
每隔 N 字节或 N 条 record 建一条 entry
offset -> physical_position
lookup(target) 找 <= target 的最近 entry
从这个 physical_position 开始顺序扫描
```

Kafka 的 `OffsetIndex` 就是这个思路。源码注释说明它把 offset 映射到 segment 内的物理位置，并且这个 index 可以是稀疏的；lookup 时通过二分找不大于目标 offset 的最大 entry。`index.interval.bytes` 控制 Kafka 往 offset index 添加 entry 的频率。

**为什么主要是性能问题**

如果没有 sparse index，读取任意 offset 有两种极端方案：

1. **没有 index**

   从 segment 开头扫到目标 offset。index 空间省了，但 seek 慢。

2. **dense index**

   定位很快，但 index 很大。大量 segment 下，index 文件、mmap、page cache 和启动加载都会吃资源。

sparse index 取中间：

```text
定位成本 = 二分 index + 小段顺序扫描
空间成本 = dense index 的一小部分
```

对日志系统很合适，因为日志数据本来就是顺序写、顺序读友好的。磁盘和 page cache 对短距离顺序扫描比较友好，没必要为每条 record 都建索引。

**它对正确性的关系**

sparse index 不能改变可见数据。正确读流程应该是：

```text
1. 找到目标 offset 所在 segment
2. 在 sparse index 里 lookup(target_offset)
3. 得到 offset <= target_offset 的 physical position
4. 从该位置顺序扫描 record batch
5. 用 record batch 的 offset 和 checksum 验证
6. 找到目标或确认不存在
```

如果第 2 步错了，最多导致多扫、少扫或读失败；系统应该能通过校验发现并重建 index。不能因为 index 指向某个位置，就不校验 data。

**它对安全性的关系**

sparse index 本身不解决安全性。它不会提供访问控制、加密、完整性证明。完整性要靠 checksum、签名、Merkle tree 或对象存储校验；权限要靠 ACL 和租户隔离。

**它对可维护性的关系**

可维护性是附带收益：

- index 文件小，备份和恢复简单。
- index 可以重建，崩溃处理简单。
- segment rolling 时只需 trim 有效 index entry。
- 调参可以通过 `index.interval.bytes` 控制空间和读取延迟。

一句话：sparse index 主要是性能结构，用较小的索引把读取定位到目标附近，再靠顺序扫描和 data 校验完成读取；它提升 seek 性能并降低索引成本，但不应该承担数据正确性或安全性。

## Q047. sparse index 的典型适用场景和不适用场景分别是什么？

**回答：**

sparse index 适合“数据按某个单调维度排列，读取可以先跳到附近再顺序扫一小段”的场景。不适合“必须一次命中精确位置，或者数据没有可利用顺序”的场景。

**典型适用场景**

1. **append-only offset log**

   Kafka 这类日志里，record 按 offset 递增写入。offset index 不需要记录每条消息，只要每隔一段字节记录一个 offset 到物理位置的映射。

2. **时间序列文件**

   数据按 timestamp 大致递增，time index 可以记录部分 timestamp 到文件位置。查某个时间点时先定位到附近，再顺序扫描。

3. **SSTable block index**

   LSM 里的 SSTable 通常按 key 排序。block index 指向 block，而不是每个 key。查 key 时先定位 block，再在 block 内查。

4. **大文件内的 range read**

   如果读取经常是范围读，sparse index 足够。定位到起点附近后，后面本来就要顺序读。

5. **冷热分层日志**

   冷数据放对象存储时，index 可以更稀疏一些，配合 range GET 按块读取，减少 index 元数据成本。

**适用前提**

通常需要：

- key、offset 或 timestamp 单调。
- 文件内物理顺序和查询维度大致一致。
- 允许定位后扫描一小段。
- record 或 batch 有自描述长度。
- data 有 checksum，可验证扫描结果。
- index 可以从 data 重建。

**不适用场景**

1. **强随机点查，延迟预算很小**

   如果每次都要亚毫秒级查一条记录，且不能接受定位后扫描，dense index、hash index 或 B+Tree 更合适。

2. **数据无序写入**

   如果 offset、key 或 timestamp 与文件顺序没有关系，sparse index 的 lower bound 查找就没有意义。

3. **按高基数字段查询**

   例如按 user_id、trace_id、tenant_id 随机查日志，而文件按 offset 排列。offset sparse index 帮不了多少，需要二级索引或倒排索引。

4. **记录大小极端不稳定**

   如果某些 batch 很大，某些很小，按字节间隔建立 sparse index 时，最坏扫描距离可能很难控制。可以改成按 record 数、按 block 或混合策略。

5. **压缩块不可切入**

   如果 index 指向压缩块中间，而解压必须从块头开始，index entry 必须对齐压缩块边界。否则定位了也读不了。

6. **合规要求单记录定位和删除**

   sparse index 只能帮你找到附近，不提供单记录物理删除能力。

**如何判断能不能用**

问三个问题就够：

```text
查询维度和文件顺序是否一致？
定位后最多扫描多少字节能接受？
data 是否能自校验并支持 index 重建？
```

如果答案都清楚，sparse index 很合适。如果第二个问题答不上来，线上尾延迟可能会出问题。

一句话：sparse index 适合有顺序、有范围读、能接受短扫描的日志和有序文件；如果查询是无序随机点查、必须精确命中、或者扫描上界不可控，就不要只靠 sparse index。

## Q048. sparse index 和相近概念最容易混淆的边界在哪里？

**回答：**

sparse index 最容易和 dense index、Bloom filter、time index、checkpoint、summary、二级索引混在一起。判断边界时，看它回答的问题是什么。

**sparse index vs dense index**

sparse index 回答：

```text
目标 offset 附近从哪里开始扫？
```

dense index 回答：

```text
目标 record 的精确位置在哪里？
```

sparse index 省空间，但需要扫描。dense index 查得准，但空间和维护成本高。

**sparse index vs Bloom filter**

Bloom filter 回答：

```text
某个 key 可能存在吗？
```

sparse index 回答：

```text
如果要找某个有序 key/offset，从文件哪个位置开始？
```

Bloom filter 有 false positive，不给物理位置。sparse index 给位置，但通常不回答“是否存在”。

**sparse index vs time index**

time index 是按 timestamp 建的索引，offset index 是按 offset 建的索引。二者都可以是 sparse 的。

```text
offset sparse index: offset -> position
time sparse index: timestamp -> offset 或 position
```

不要把“按什么字段索引”和“索引是否稀疏”混为一谈。

**sparse index vs checkpoint**

checkpoint 记录恢复进度：

```text
recovery_point_offset = 100000
```

sparse index 加速读取：

```text
offset 98304 -> file position 742391
```

checkpoint 不负责随机读取，sparse index 不负责说明哪些数据已经安全恢复。

**sparse index vs segment manifest**

manifest 帮你找到 segment：

```text
offset 100000 在 segment S3
```

sparse index 帮你在 segment 内定位：

```text
在 S3 内，从 position 8192 开始扫
```

两层索引不要混在一起。manifest 是 segment 级元数据，sparse index 是 segment 内部定位结构。

**sparse index vs 二级索引**

二级索引通常按业务字段查：

```text
tenant_id -> offsets
trace_id -> offsets
key -> latest offset
```

sparse offset index 只服务 offset 顺序读取。拿 offset sparse index 去支持 tenant 查询，最后会变成全 segment 扫描。

**sparse index vs LSM block index**

SSTable block index 和 sparse index 很像，因为它也不是每个 key 一条索引，而是定位到 block。差异在于：

- log sparse index 通常按 offset 或时间。
- SSTable block index 通常按 key。
- log segment 内记录是追加顺序。
- SSTable 内记录是 key 排序。

思想相似，语义不同。

**一句分类**

```text
manifest: 找哪个 segment
sparse index: 找 segment 内大概位置
dense index: 找精确 record 位置
Bloom filter: 判断可能不存在
checkpoint: 找恢复起点
time index: 按时间定位
secondary index: 按业务字段定位
```

一句话：sparse index 的边界是“有序文件内的近似定位”。它不是存在性判断，不是恢复点，不是业务字段索引，也不是精确 record 地址表。

## Q049. sparse index 在高并发场景下可能出现哪些隐藏问题？

**回答：**

sparse index 在高并发下的问题，主要来自读写并发、mmap 可见性、index 和 data 的提交顺序、rolling 时的 remap/truncate，以及多个 writer 对单调性的破坏。index 文件小，不代表并发语义简单。

**writer 并发破坏单调性**

offset index 的基本要求是 entry offset 单调递增。两个 writer 同时 append index entry，如果没有串行化，可能出现：

```text
writer A append offset 200
writer B append offset 180
index entry 变成 200, 180
```

二分查找依赖有序。一旦 index 乱序，lookup 结果就不可信。Kafka 的 `OffsetIndex.append` 会拒绝不大于 last offset 的 append，上层还要保证同一 segment append 串行。

**reader 看到半条 index entry**

一个 offset index entry 通常包含：

```text
relative_offset
physical_position
```

如果 writer 写完 offset 还没写 position，reader 就开始二分，可能读到半条 entry。解决方式：

- index append 在锁内完成。
- entries count 在 entry 完整写入后再推进。
- reader 只读取 committed entries 范围。
- mmap remap 和读通过读写锁协调。

Kafka `AbstractIndex` 里有 mutation lock 和 remap lock，就是为了解决 index 内部状态修改和 mmap 可见性问题。

**index 先于 data 可见**

危险顺序：

```text
1. 写 index entry
2. data append 还没完成或没提交
3. reader 通过 index 跳到未提交 data
```

正确做法是 data 先写入并通过 batch 校验，再让 index entry 和 log end offset 对 reader 可见。reader 也必须受 committed offset 限制。

**data 有了但 index 还没更新**

这个方向通常可以接受：

```text
data 已提交
index 还没来得及写
```

reader 可能从更早的 index entry 开始扫，慢一点，但不应该读错。sparse index 的一个好处就在这里：少一条 entry 只是性能问题，不是数据丢失。

**rolling 时 index 被 trim 或 remap**

segment rolling 时，index 可能从预分配大小 trim 到有效大小；新 active segment 又创建新 index。此时并发 reader 如果还拿着旧 mmap，会遇到：

- mmap 被 unmap。
- index file size 变化。
- reader 使用旧 entries count。
- Windows 上文件被 mmap 时删除或改名失败。

需要 reader pin 住 segment/index，或者用 remap 读写锁保证旧 mmap 不会在读过程中被释放。

**index full 触发 rolling 的竞态**

sparse index 也可能满。两个线程同时发现 index 接近满：

```text
writer A: index 还有 1 个 slot
writer B: index 还有 1 个 slot
```

如果没有串行化，一个线程写入后另一个线程还认为可以写，可能越界或触发重复 rolling。index full 应该进入统一的 roll decision，而不是每个 writer 各自处理。

**多 reader 下的 cache 抖动**

高并发随机读会让 sparse index 和 data 扫描产生组合压力：

- index mmap page fault。
- data 从 lower bound 开始扫描，放大读字节。
- 过稀的 index 导致每次扫描距离长。
- 大量 segment 的 index 争夺 page cache。

这不是 correctness bug，但会体现为 p99 读延迟抖动。

**和 truncation/compaction 并发**

如果 follower 回退或 retention 截断 index，而 reader 正在读：

```text
reader lookup offset 1000
truncation 删除 >= 900 的 index entry
reader 继续按旧 position 读
```

reader 必须绑定稳定视图，或者读取后用当前 segment range、committed offset、leader epoch 再校验一次。

一句话：sparse index 的高并发问题集中在 entry 原子可见、offset 单调、data/index 提交顺序、mmap remap、index full rolling 和 reader 稳定视图；只给 index 加一个数组锁，不足以覆盖这些边界。

## Q050. sparse index 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

sparse index 的故障边界比 data log 更宽松一点，因为它通常可以重建。但宽松不等于随便写。崩溃后必须能判断 index 哪些 entry 有效，哪些要截断，什么时候从 data 重建。

**崩溃在 index entry 写一半**

场景：

```text
relative_offset 写入了
physical_position 没写完
进程崩溃
```

恢复时如果只看文件大小，可能把半条 entry 当成有效 entry。处理方式：

- index 文件长度必须是 entry size 的整数倍。
- entries count 不能只靠预分配文件大小推导。
- 加载时做 sanity check。
- 不确定时从 data 重建 index。

Kafka 的 `OffsetIndex.sanityCheck` 会检查 index 文件大小是否是 entry size 的整数倍；源码也说明 index 文件崩溃后可以重建。

**崩溃在 data 写完、index 没写之后**

这是最常见也最容易处理的情况：

```text
data 有 offset 1000-1099
index 只有到 900 的 entry
```

恢复时扫描 data，按 index interval 补 entry。即使不立即补，读取也能从 900 附近开始扫，只是慢一点。

**崩溃在 index 写完、data 没持久化之后**

这个更危险：

```text
index 指向 position P
position P 上没有完整 record batch
```

恢复时不能相信 index。要从 data 的最后完整 batch 推导 log end offset，再截断或重建 index。index 指向 data 尾部之后，就是脏 index。

**预分配 index 的边界**

很多实现会预分配 index 文件，roll 后再 trim。崩溃时可能看到：

- index 文件很大，但有效 entry 很少。
- mmap position 没有持久化。
- 尾部全是零。
- trim 做了一半。

所以恢复不能把 index 文件长度等同于 entry 数。要有有效 entry 计数、最后 offset 校验，或者直接按 data 重扫。

**重启后 index 和 segment range 不一致**

可能出现：

```text
segment range = [1000, 2000)
index entry offset = 2500
```

这说明 index 来自错误文件、旧文件、重复 base offset 或截断失败。处理方式不是跳过这条 entry 继续跑，而是标记 index corrupt，重建或让 segment 进入 repair。

**超时和重试导致重复 append**

如果 index append 操作被封装成异步任务，调用方超时后重试：

```text
append_index(offset=1200, position=8192)
timeout
retry append_index(offset=1200, position=8192)
```

index append 要么幂等，要么能识别重复 offset。Kafka 的 `OffsetIndex.append` 要求新 offset 大于 last offset，因此重复 append 会被拒绝。自研实现里可以选择：

- 相同 offset 和 position 的重复 append 视为成功。
- 相同 offset 但 position 不同，报错并触发重建。
- offset 小于 last offset，拒绝。

**lookup 超时和重试**

读请求 lookup 超时后重试，不能依赖第一次拿到的旧 position。因为期间可能发生：

- segment rolling。
- truncation。
- retention。
- index rebuild。
- remote/local tier 切换。

重试时应该重新绑定 segment view，再做 lookup，并检查目标 offset 是否仍在可读范围。

**远端 index 下载失败**

冷数据在对象存储里时，data object 和 index object 可能分开保存。边界包括：

- data 下载成功，index 下载失败。
- index 是旧版本，data 是新版本。
- index checksum 不匹配。
- range GET 只拿到部分 index。

可选策略：

```text
index 缺失 -> 从 data 顺序扫描或后台重建
index checksum 错 -> 丢弃 index，重建
data checksum 错 -> 数据损坏，不能靠 index 修复
```

**恢复优先级**

故障恢复时可以按这个顺序判断：

```text
record batch checksum 和 length
segment base/end offset
manifest 中的 segment metadata
index sanity check
index lookup 结果
```

index 排在后面。它可以加速恢复，但不能凌驾于 data 校验之上。

一句话：sparse index 的崩溃边界主要是半条 entry、预分配尾部、index 比 data 新、data 比 index 新、重复 append 和远端 index 不一致。稳妥设计是把 index 当缓存结构：能校验就用，不能校验就截断或重建。

## Q051. sparse index 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

sparse index 的瓶颈通常不在 CPU。它的核心操作是二分查找加少量顺序扫描，CPU 成本很低。真正容易出问题的是内存、I/O 和锁竞争；如果 index 或 data 放到远端对象存储，网络也会被放大。

可以按路径拆：

```text
lookup:
  segment 定位
  sparse index 二分
  从 lower-bound position 顺序扫描 data

append:
  写 data
  按间隔追加 index entry
  更新 entries / lastOffset
```

**CPU**

CPU 主要花在：

- 二分查找。
- record batch header 解析。
- checksum 校验。
- 解压缩。
- offset 比较。

二分查找本身很便宜。一个 segment 里即使有几十万条 index entry，二分也只要二十次左右比较。CPU 真正上来，通常不是 index 查找，而是定位后要扫描的数据太多，或者每个 batch 都要解压、校验。

如果看到 CPU 高，要看：

```text
scan_after_lookup_bytes
decompress_bytes
checksum_bytes
records_scanned_per_lookup
```

不要只盯着 `index_lookup_ns`。

**内存和 page cache**

sparse index 依赖 index 文件和 data 文件的局部性。常见内存瓶颈是：

- index mmap 太多。
- page cache 被 data scan 挤掉。
- segment 太多导致 index metadata 占内存。
- index interval 太小，index 文件变大。
- index interval 太大，查到位置后扫描太多 data。

Kafka 的 offset index 使用 mmap，index 文件预分配，roll 后再 shrink。这个设计让 lookup 很快，但也意味着大量 segment 会带来 mmap 和 page cache 压力。

症状：

```text
major page fault 增加
index cache hit rate 下降
随机读 p99 抖动
FD / mmap 数量接近上限
启动加载 segment metadata 变慢
```

**I/O**

sparse index 最大的 I/O 风险在“定位后扫描”。index 越稀，扫描越长。对于本地 SSD，扫几十 KB 通常可接受；对于 HDD 或远端对象存储，额外扫描会变贵。

典型问题：

```text
index lookup 很快
fetch latency 仍然高
disk read bytes / request 很大
range read 多次跨 segment
冷数据读取触发多个 range GET
```

所以 sparse index 的调参不是只看 index 文件大小，还要看定位后扫描字节数。

**锁竞争**

读路径通常可以无锁或轻锁，写路径必须维护 entry 顺序。Kafka `OffsetIndex.append` 要求新 offset 大于 last offset，并在内部锁里写 entry。高并发写同一个 segment 时，竞争点在：

- index append lock。
- segment append lock。
- mmap remap lock。
- rolling 时的 trim/resize。
- truncation 和 reader lookup 的协调。

如果 index entry 很密，每写一小段 data 就要追加 index，锁竞争会增加。过密的 sparse index 在并发写入下可能变成 dense index 的一半问题。

**网络**

本地 sparse index 不太受网络影响。分层存储场景里，网络会明显影响：

- 远端 index 下载。
- 远端 data range GET。
- index 缺失时从远端 data 扫描重建。
- 多 segment range read。

远端读取时 index entry 的粒度要和对象存储 range GET 匹配。index 太稀，会多拉很多无用字节；index 太密，远端 index 对象变大，缓存压力上升。

**怎么定位瓶颈**

建议把指标拆成：

```text
index_lookup_latency
index_page_faults
index_entries
scan_after_lookup_bytes
scan_after_lookup_records
data_read_bytes
data_read_latency
index_append_latency
index_append_lock_wait
index_rebuild_duration
remote_index_fetch_latency
remote_data_range_get_latency
```

如果 lookup 快但 fetch 慢，看 I/O 和扫描距离。如果写入慢，看 index append 频率和锁。如果 p99 抖，看 page cache、mmap 和远端请求。

一句话：sparse index 的 CPU 成本通常很低，瓶颈主要来自 index/data 的内存局部性、定位后的顺序扫描 I/O、写入时的 index 锁竞争，以及远端读取时的 range GET 放大。

## Q052. sparse index 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

sparse index 的三类测试要分开看。correctness test 证明查找语义对；stress test 找并发和崩溃边界；benchmark 量化 index interval、segment size 和冷热路径的成本。

**correctness test**

正确性测试先围绕 lookup 语义写：

```text
lookup(target) 返回 offset <= target 的最大 index entry
如果 target 小于第一条 entry，返回 segment base position
如果 target 大于最后一条 entry，返回最后一条 entry
从返回位置顺序扫描，必须找到 target 或确认 target 不存在
```

还要覆盖：

- 空 index。
- 只有一条 entry。
- target 正好命中 entry。
- target 落在两条 entry 中间。
- target 落在 segment base offset。
- target 超过 segment end offset。
- index entry offset 单调递增。
- index entry position 单调递增。
- index 与 data 不一致时能检测并重建。

Kafka `OffsetIndex.lookup` 的行为就是找不大于目标 offset 的最大 offset-position pair。测试要把这个 lower-bound 语义固定住。

**恢复正确性**

恢复类 correctness test 要覆盖：

```text
data 有，index 缺失 -> 重建 index
index 有，data 缺失 -> 丢弃脏 index
index 文件长度不是 entry size 整数倍 -> corrupt
index entry offset 超出 segment range -> corrupt
index entry position 指到半条 batch -> corrupt
```

这里的原则是 data 比 index 权威。Kafka offset index 注释里也说 index 文件不做 checksum，崩溃后重建。

**stress test**

stress test 要故意制造竞态：

1. **并发 append 和 lookup**

   写线程不断追加 data 和 index entry，读线程随机 lookup。读线程只能看到 committed offset 范围内的数据。

2. **rolling 和 lookup 并发**

   旧 index 被 trim，新 index 被创建，reader 仍在旧 segment 上读。不能出现 mmap 被释放后继续访问的问题。

3. **truncation 和 lookup 并发**

   follower 回退、恢复截断或测试主动 truncate index。reader 要么绑定旧视图，要么检测到范围变化并重试。

4. **故障注入**

   在这些点 kill：

   ```text
   data append 后
   index offset 写入后
   index position 写入后
   entries count 推进前
   index trim 中
   mmap remap 中
   ```

5. **资源压力**

   限制 FD、制造 page fault、让大量 segment 同时打开 index、用小 segment 触发大量 index 文件。

stress test 的 oracle 可以是一个内存模型：所有 committed record 放在数组里，随机读结果必须和模型一致。

**benchmark**

benchmark 要回答参数怎么选：

```text
index.interval.bytes = 512B / 4KB / 64KB / 1MB
segment.bytes = 16MB / 256MB / 1GB
record size = 小 / 中 / 大 / 混合
read pattern = point / range / tail / cold random
```

重点指标：

- index 文件大小。
- lookup latency。
- lookup 后扫描字节数。
- fetch p99。
- index append 开销。
- page fault。
- index rebuild time。
- 启动加载时间。
- 远端 range GET 次数和字节数。

如果 benchmark 只报 “index lookup ns”，意义不大。真正影响用户的是从请求 offset 到返回 record 的总时间。

一句话：sparse index 的 correctness test 固定 lower-bound 查找和可重建语义，stress test 打并发 append、lookup、rolling、truncate 和崩溃点，benchmark 则量化 index 稀疏度对空间、扫描距离、读尾延迟和恢复时间的影响。

## Q053. 如果要求从零实现一个简化版 sparse index，你会先定义哪些不变量？

**回答：**

我会先定义这些不变量，再写二分查找。sparse index 的实现代码不复杂，真正要守住的是“索引可以少，但不能错；索引可以坏，但必须能识别并重建”。

**文件范围不变量**

每个 sparse index 只属于一个 segment：

```text
index.segment_id == segment.segment_id
index.base_offset == segment.base_offset
entry.offset >= segment.base_offset
entry.offset < segment.end_offset
entry.position >= 0
entry.position < segment.size_bytes
```

index 不能跨 segment。跨 segment 先用 manifest 或 segment map 定位，再进入 segment 内 index。

**单调性不变量**

entry 必须按 offset 和 position 递增：

```text
entry[i].offset < entry[i+1].offset
entry[i].position < entry[i+1].position
```

如果 offset 单调但 position 不单调，说明 data 文件顺序和 offset 顺序不一致，二分后顺序扫描语义会坏掉。

**lower-bound 语义**

lookup 语义要写死：

```text
lookup(target):
  返回 offset <= target 的最大 entry
  如果不存在，返回 (segment.base_offset, 0)
```

注意它返回的不是目标 offset 的精确位置，而是扫描起点。调用方必须继续扫描 data。

**entry 原子可见不变量**

一条 entry 至少包含：

```text
relative_offset
physical_position
```

必须完整写入后才能推进 `entry_count`。reader 只读 `entry_count` 范围内的 entry，不能靠文件长度读预分配尾部。

**data 权威不变量**

```text
data 是事实来源
index 是加速结构
index 可删除、可重建
index 不得让未提交 data 对 reader 可见
```

恢复时：

```text
如果 data 和 index 冲突，以 data 的完整 record batch 和 checksum 为准
```

**稀疏度不变量**

可以按字节或 record 数建立 entry，例如：

```text
bytes_since_last_index_entry >= index_interval_bytes
```

但这不是硬 correctness 条件。漏写一条 entry 只会让扫描更长；错写一条 entry 才是 correctness 风险。

**持久化不变量**

最小版本可以这样做：

```text
append data batch
校验 batch 完整
必要时 append index entry
推进 committed offset
```

重启后：

```text
读取 segment data
扫描完整 batch
按相同 interval 重建 index
截断旧 index
```

如果要持久化 index 文件，也要保证文件长度是 entry size 的整数倍，并且 entry_count 不超过文件可容纳数量。

**并发不变量**

简化实现可以先规定：

```text
同一个 segment 只有一个 writer
多个 reader 可以并发 lookup
truncate / rebuild / remap 与 lookup 互斥
reader 只能读 committed offset 之前
```

这个约束会让第一版简单很多。等语义稳定后，再优化读写锁或 lock-free 快照。

一句话：从零实现 sparse index，先定义 segment 归属、entry 单调、lower-bound lookup、entry 原子可见、data 权威、稀疏度上界、恢复重建和并发可见性这些不变量；二分查找只是最后一步。

## Q054. sparse index 的常见误用是什么，误用后通常会产生什么线上症状？

**回答：**

sparse index 最常见的误用，是把它当成精确索引，或者只看 index 文件大小，不看定位后的扫描成本。

**把 sparse index 当 dense index 用**

误用：

```text
lookup(offset) 后直接按返回 position 读一条 record
不继续扫描和校验
```

症状：

- 读到小于目标 offset 的 record。
- range read 起点不准。
- consumer 重复消费或漏消费。
- offset not found 的判断错误。

sparse index 返回的是扫描起点，不是精确命中。

**index interval 配得过大**

误用：

```text
为了省内存，把 index.interval.bytes 配得很大
```

症状：

- point read 延迟高。
- fetch 请求读放大大。
- 冷数据 range GET 多拉很多无用字节。
- p99 随 record size 波动。
- backfill 和随机读拖慢前台。

这时 index 本身很小，但 data scan 很贵。

**index interval 配得过小**

误用：

```text
为了读得快，每几条 record 就建 index
```

症状：

- index 文件变大。
- mmap 和 page cache 压力上升。
- index append 锁竞争增加。
- segment rolling 时 index trim 更频繁。
- 启动加载更多 index metadata。

Kafka 文档对 `index.interval.bytes` 的描述就是这个取舍：更频繁的 index 让读取更接近精确位置，但 index 文件更大。

**不校验 data**

误用：

```text
相信 index 指向的位置一定有效
```

症状：

- 崩溃后读到半条 batch。
- index 指向旧位置时返回错误数据。
- 远端 index 和 data 版本不一致时静默错读。

正确做法是读 record batch header、length、offset 和 checksum。

**用 offset index 查业务字段**

误用：

```text
用 offset sparse index 支持 tenant_id / user_id / trace_id 查询
```

症状：

- 查询退化成全 segment 扫描。
- CPU 和 I/O 被历史查询打满。
- 热点 tenant 把共享 segment 拉进 cache。

业务字段查询需要二级索引、倒排索引或按 tenant 分段，不是 offset sparse index 能解决的。

**index 和 segment 生命周期脱节**

误用：

```text
data segment 删除了，index 还在
index 上传了，data 没上传
manifest 指向 data 新版本和 index 旧版本
```

症状：

- reader 定位成功，读取失败。
- 远端冷读报 checksum mismatch。
- GC 留下大量 orphan index。

一句话：sparse index 误用后，线上症状通常是随机读尾延迟高、range read 起点错误、冷读多拉无用字节、index cache 抖动、业务字段查询退化扫描，以及崩溃后 index 指向无效 data。

## Q055. sparse index 在单机和分布式环境中的语义有什么差异？

**回答：**

单机环境里，sparse index 只是本地 segment 的加速结构。分布式环境里，它仍然不应该成为协议语义，但会影响副本追赶、远端读取、索引版本发布和故障恢复。

**单机语义**

单机里可以这么理解：

```text
segment data: 权威数据
sparse index: 本地加速文件
恢复: index 坏了就从 data 重建
```

只要满足：

- index entry 不越界。
- lookup 后扫描能找到目标。
- index 不暴露未提交 data。
- 崩溃后能重建。

单机语义就比较清楚。

**分布式语义**

分布式系统里还要问：

- leader 和 follower 的 index 是否必须完全一致？
- follower 可否本地重建 index？
- remote tier 的 index 由谁生成？
- index metadata 何时发布？
- index 版本是否和 data segment checksum 绑定？

通常更稳的做法是：复制协议复制 data log，index 可以由每个副本本地生成。这样 index 不进入一致性协议，坏了也不会影响数据正确性。

**leader/follower 差异**

leader 分配 offset，follower 复制 record。只要 follower 的 data 和 leader 一致，follower 本地 index 可以不逐字节相同。例如：

```text
leader index interval = 4KB
follower 恢复后重建 index，entry 边界略有差异
```

只要 lookup 语义正确，客户端不应该感知差异。问题出在系统把 index 文件也当作复制对象，并且没有绑定 data checksum。

**high watermark 和可见性**

分布式日志里，index 可能已经有某个 offset 的 entry，但该 offset 尚未越过 high watermark。reader 仍然不能读它。也就是说：

```text
index 可定位 != record 可对 consumer 暴露
```

consumer 可见性取决于提交边界，而不是 index 是否存在。

**远端存储**

远端场景里，index 和 data 可能作为两个对象保存：

```text
segment.data
segment.index
remote manifest
```

这时需要额外约束：

- index object 记录 data checksum 或 segment id。
- manifest 同时记录 index checksum。
- data 和 index 版本匹配后才对读路径可见。
- index 缺失时可以降级为 data scan 或后台重建。

否则会出现 data 是新对象、index 是旧对象，lookup 跳到错误位置。

**跨副本修复**

当某个副本 index 损坏，分布式系统不一定要从 leader 拉 index。更简单的修复是：

```text
校验本地 data segment
从 data 重建 sparse index
如果 data 也坏，再从 leader 或 remote tier 拉 data
```

这能降低复制协议复杂度。

一句话：单机 sparse index 是本地可重建加速结构；分布式里也应保持这个定位，不让 index 决定提交语义。跨副本、远端和恢复路径要用 segment id、checksum、manifest version 把 index 和 data 对齐。

## Q056. dense index 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

dense index 的核心目标是为每条记录、每个 key 或每个可查询项保存一条精确定位信息，让查询不需要先跳到附近再扫描。它主要解决的是性能问题，尤其是随机点查和精确定位的延迟。

在日志系统里，dense offset index 可以理解为：

```text
offset -> physical_position
每个 offset 或每个 record batch 都有 entry
```

和 sparse index 相比：

```text
sparse index: 找附近位置，再扫描
dense index: 直接找到精确位置
```

**为什么主要是性能结构**

dense index 减少的是读取路径的扫描成本：

- point read 更快。
- 随机读尾延迟更稳定。
- 小范围读取可以更准确地截取。
- 不需要从 lower-bound 扫很多 batch。

但它不应该承担数据正确性。即使 dense index 指向某个位置，reader 仍然要校验 record length、offset 和 checksum。index 是定位结构，不是数据本身。

PostgreSQL 官方文档解释普通索引时也用了同一个基本思想：没有索引时要逐行扫描，有索引时可以用更高效的路径定位匹配行。dense index 在日志里的语义更窄，但目标一样，都是减少全量扫描。

**代价**

dense index 的代价也更重：

- index 文件大。
- 写入每条记录都要维护 index。
- cache 压力高。
- 恢复或重建更慢。
- compaction 或 truncation 要更新更多 entry。
- 分布式复制或远端上传时 metadata 成本更高。

如果每条 record 很小，dense index 甚至可能接近 data 文件大小的可观比例。

**它解决什么，不解决什么**

它解决：

```text
随机点查定位慢
稀疏索引扫描距离不可控
小范围读取起点不精确
按 offset 精确定位成本高
```

它不解决：

```text
数据损坏
权限隔离
重复写入
事务提交
retention
compaction correctness
```

一句话：dense index 是为精确定位和随机读性能服务的结构，用空间、写放大和维护成本换更低的点查延迟；它不是正确性或安全性的来源。

## Q057. dense index 的典型适用场景和不适用场景分别是什么？

**回答：**

dense index 适合“点查多、延迟要求严、每条记录都值得被单独定位”的场景。不适合吞吐写为主、范围读为主、记录极小且数量巨大、或者 index 维护成本会压过收益的场景。

**适用场景**

1. **随机点查很多**

   例如按 offset 精确读取单条消息，且请求分布很随机。sparse index 每次都要扫一段，dense index 可以直接定位。

2. **记录大小变化很大**

   如果 batch 大小差异巨大，sparse index 的最坏扫描距离不好控制。dense index 能把扫描距离压到最低。

3. **低延迟读取比写入吞吐更重要**

   例如调试日志、审计查询、在线点查服务。写入慢一点可以接受，读取必须稳定。

4. **内存索引**

   如果数据集小，或者只索引热窗口，把 dense index 放内存里很实用。

5. **业务字段精确查询**

   如果按 key、trace id、request id 查单条记录，dense 二级索引可能比 offset sparse index 更合适。

**不适用场景**

1. **高吞吐 append-only 日志**

   每条 record 都写 index，会增加写放大和锁竞争。Kafka 这类系统默认使用 sparse offset index，就是为了避免 index 过大。

2. **主要是顺序 range read**

   如果 consumer 总是从某个 offset 往后批量读，dense index 的精确定位收益不大。

3. **记录很小**

   如果 record 只有几十字节，一条 dense entry 可能就 8 到 16 字节。index/data 比例太高。

4. **冷数据对象存储**

   远端保存 dense index 会增加对象大小、下载时间和缓存压力。冷数据常更适合 sparse index 加较大 range read。

5. **频繁 compaction 或重写**

   compaction 重写 data 后，dense index 也要大量重写。维护成本高。

6. **只需要近似定位**

   如果定位到附近再扫几十 KB 已经满足 SLO，dense index 是浪费。

**判断标准**

可以用这几个问题：

```text
点查占比有多高？
定位后扫描的 p99 字节数是否超 SLO？
index 大小是否能进 cache？
写入路径是否能承受每条 record 写 index？
恢复时能否快速重建这么大的 index？
```

一句话：dense index 适合随机点查和严格尾延迟场景；如果 workload 是高吞吐追加、批量顺序读、冷数据扫描或记录很小，sparse index 往往更合算。

## Q058. dense index 和相近概念最容易混淆的边界在哪里？

**回答：**

dense index 容易和 sparse index、hash index、B+Tree、primary index、secondary index、Bloom filter、cache 混在一起。它们都能让查询更快，但语义不一样。

**dense index vs sparse index**

```text
dense index:
  每条 record / key 有 entry
  精确定位
  空间大，写入维护重

sparse index:
  每隔一段建 entry
  近似定位后扫描
  空间小，读取有扫描成本
```

区别不在数据结构一定是什么，而在 entry 覆盖粒度。

**dense index vs hash index**

hash index 说的是组织方式：

```text
hash(key) -> position
```

dense index 说的是覆盖粒度：

```text
每个 key 都有 entry
```

一个 hash index 可以是 dense 的，也可以只索引部分 key。二者不是同一维度。

**dense index vs B+Tree**

B+Tree 是索引结构。dense index 可以用数组、hash table、B+Tree、LSM table 来实现。不要把 dense 等同于 B+Tree。

MySQL InnoDB 的 clustered index 和 secondary index 是 B+Tree 体系下的索引；日志系统里的 dense offset index 可能只是一个定长数组。

**dense index vs primary index**

primary index 通常指按主键组织或定位数据的索引。dense index 指每个数据项是否都有索引 entry。

例如：

```text
主键索引可以是 dense
主键索引也可以在有序文件里做 sparse block index
```

要看具体存储布局。

**dense index vs secondary index**

secondary index 按非主顺序字段查，例如：

```text
trace_id -> offset
tenant_id -> offsets
```

它通常会更接近 dense，因为需要覆盖可查询项。但 dense offset index 不是 secondary index，它仍然按 offset 定位。

**dense index vs Bloom filter**

Bloom filter 判断“可能存在”，不给位置。dense index 给精确位置。Bloom filter 可以和 dense index 配合：

```text
Bloom filter 判断 key 不存在 -> 不查 index
Bloom filter 认为可能存在 -> 查 dense index
```

**dense index vs cache**

cache 是临时加速层，可以丢。dense index 如果被 manifest 或数据结构视为正式索引，就必须能恢复、校验和维护。

一句话：dense index 的边界是“覆盖粒度足够密，能精确定位每个可查询项”。它不是某一种数据结构，也不等同于主键索引、二级索引、hash index、B+Tree 或 cache。

## Q059. dense index 在高并发场景下可能出现哪些隐藏问题？

**回答：**

dense index 在高并发下的问题比 sparse index 更尖锐，因为每条记录都要维护 index。写入越密，索引越容易进入前台临界路径。

**写入放大和锁竞争**

每条 record 都写 index，会带来：

- index append lock 高频争用。
- offset 分配和 index 写入绑定更紧。
- cache line 抖动。
- mmap position 或 entry_count 更新频繁。
- batch 写入被拆成大量小 index 操作。

如果一个 segment 有多个 writer，必须保证 index entry 顺序和 data 顺序一致：

```text
data:  offset 100, 101, 102
index: offset 100, 101, 102
```

不能出现 data 顺序和 index 顺序错开。

**reader 看到未提交 entry**

dense index entry 多，半写 entry 的概率也更高。需要：

- entry 完整写入后再推进 visible count。
- reader 只查 visible count 范围。
- index entry 指向的 data 必须已提交。

否则 reader 可能精确跳到一条未提交或半写 record。

**更新和删除更复杂**

如果 dense index 索引的是 key，而不是 offset，就会遇到：

```text
put key=A at offset 10
put key=A at offset 20
delete key=A at offset 30
```

index entry 是保留历史、指向最新，还是保留多版本？这会牵涉 MVCC、snapshot 和 tombstone。高并发更新下，dense secondary index 比 dense offset index 更难。

**compaction 并发**

compaction 重写 data 时，dense index 也要重写。并发 reader 可能拿到旧 index entry：

```text
旧 index: key A -> old_segment position 100
新 index: key A -> new_segment position 80
```

必须用版本化 manifest 或 copy-on-write index 发布，不能原地把 index 改一半。

**内存抖动**

dense index 很大，高并发随机读会造成：

- index cache miss。
- page cache 被 index 占满。
- data 反而被挤出 cache。
- GC 或 allocator 压力上升。

如果 index 放内存，扩容和 rehash 也会造成停顿。

**热点 key**

dense secondary index 如果按业务 key 查，热点 key 会造成：

- 同一个 index bucket 或 B+Tree page 争用。
- 版本链变长。
- 锁等待和死锁风险上升。
- 单 key 查询拖慢写入。

**分布式副本不一致**

如果 dense index 作为复制对象，而不是本地重建结构，高并发下还要保证所有副本 index 更新顺序一致。否则 data 一致但 index 不一致，会导致不同副本读到不同位置。

一句话：dense index 的高并发问题集中在每条记录维护索引带来的锁竞争、写放大、半写 entry 可见、compaction 版本切换、热点 key 争用和副本 index 顺序一致性。

## Q060. dense index 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

dense index 在故障场景下暴露的问题比 sparse index 多，因为 entry 数量更多，而且调用方往往把它当成精确位置使用。恢复策略仍然应该坚持一条：data 和提交日志比 index 权威。

**崩溃在 entry 写一半**

dense index entry 可能是：

```text
offset -> position
key -> segment_id + position
key -> latest_offset
```

崩溃时可能只写了 key，没写 position，或者写了 position 没写 checksum。恢复要能识别半条 entry。常见办法：

- 定长 entry 加 entry count。
- 变长 entry 加 length 和 crc。
- 页式结构加 page checksum。
- WAL 记录 index update。
- 直接从 data 重建。

**data 和 dense index 顺序不一致**

场景：

```text
data 写入 offset 100 成功
index 没写
```

这会导致点查 miss，但 data 还在。恢复可以补 index。

反过来：

```text
index 写入 offset 100 -> position P
data 没持久化
```

这会导致脏命中。恢复必须校验 position P 上是否有完整 record。

**重复重试**

超时后重试 index append：

```text
append_index(offset=100, position=P)
timeout
retry append_index(offset=100, position=P)
```

如果 index 不幂等，可能出现重复 entry。对 dense offset index，重复相同映射可以视为成功；相同 offset 不同 position 必须报错并进入修复。

**latest-key index 的重试问题**

如果 dense index 维护 key 的最新位置：

```text
key A -> offset 20
```

重试旧请求可能把 index 回退：

```text
new write: A -> offset 30
old retry: A -> offset 20
```

所以 entry 更新必须带 sequence 或 offset 条件：

```text
only update if new_offset > current_offset
```

**重启重建成本**

dense index 可以从 data 重建，但成本可能很高。重启时要考虑：

- 是否全量重建。
- 是否加载 checkpoint。
- 是否 lazy rebuild。
- 是否只重建热窗口。
- 是否允许服务降级启动。

如果每次重启都扫描 TB 级 segment 重建 dense index，恢复时间会不可接受。

**compaction 半发布**

compaction 产生新 data 和新 dense index，崩溃点可能在：

```text
新 data 写完
新 index 写一半
manifest 未发布
manifest 已发布
旧 index 未删
```

恢复要用 manifest generation 判断哪个版本可见。不能混用新 data 和旧 index，也不能混用旧 data 和新 index。

**远端 dense index**

远端场景里，dense index 大，上传时间长。边界包括：

- data 上传成功，index 上传失败。
- index 上传成功，manifest 未发布。
- index object checksum 不匹配。
- range GET 只取到部分 index page。

远端 manifest 要记录 data checksum、index checksum、entry count 和 index version。

一句话：dense index 的故障边界主要是半写 entry、data/index 顺序不一致、重复重试、latest-key 回退、重建时间过长和 compaction 半发布。稳妥方案是让 index update 幂等、有版本，并能从 data 或 checkpoint 恢复。

## Q061. dense index 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

dense index 的瓶颈通常先来自内存和锁竞争，然后是 I/O。CPU 是否成为瓶颈取决于索引结构。网络只在分布式复制和远端索引场景里明显。

**内存**

dense index 每条记录都有 entry，规模很快变大：

```text
100M records * 16 bytes/entry = 1.6GB
```

这还没算对象头、哈希表空洞、B+Tree page、allocator 元数据和缓存开销。内存瓶颈表现为：

- index cache 放不下。
- page fault 增加。
- GC 或 allocator 压力高。
- NUMA remote access 增多。
- 数据页被 index 挤出 cache。

如果 dense index 放内存，扩容和重建也会造成大停顿。

**锁竞争**

每条写入都改 index，锁竞争比 sparse index 高：

- append lock。
- hash bucket lock。
- B+Tree page latch。
- manifest/index version lock。
- entry count 原子变量。

写入吞吐高时，dense index 常见瓶颈不是磁盘，而是很多线程抢同一段索引结构。

**I/O**

I/O 来自：

- index 文件写入。
- index page flush。
- WAL 或 redo。
- compaction 重写 index。
- 启动加载 index。
- checkpoint index。

如果 index 不落盘，恢复慢；如果每条 index update 都同步落盘，前台写入慢。通常要用 batch、WAL、checkpoint 或异步 flush 折中。

**CPU**

数组式 offset dense index 的 CPU 很低：

```text
position = base + (offset - base_offset) * entry_size
```

hash index 的 CPU 花在 hash、比较和冲突处理。B+Tree 的 CPU 花在 page 查找、比较、分裂、合并。加上 checksum、压缩、加密后，CPU 才会更明显。

**网络**

网络主要出现在：

- 复制 dense index。
- 上传 dense index 到远端。
- 远端读取 dense index page。
- 分布式查询拉取 index shard。

如果 index 可以本地重建，不建议把每条 index update 都放进复制协议。复制 data，让副本本地建 index，通常更简单。

**指标**

要看：

```text
index_memory_bytes
index_cache_hit_rate
index_update_latency
index_lock_wait
index_wal_bytes
index_flush_latency
index_rebuild_time
index_page_faults
index_remote_fetch_latency
```

一句话：dense index 最大成本是“每条记录都被索引”带来的内存占用和写入路径锁竞争；I/O 来自持久化和重建，CPU 通常是结构复杂后才冒出来，网络只在复制或远端索引时成为主瓶颈。

## Q062. dense index 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

dense index 的测试要比 sparse index 更强调“每条记录都有映射”。漏一条 entry 在 sparse index 里可能只是慢，在 dense index 里通常就是点查 miss。

**correctness test**

要测：

```text
每个 committed record 都有 index entry
每个 index entry 都指向一条完整 committed record
offset -> position 精确
key -> position 不丢、不倒退
删除或 tombstone 语义正确
truncate 后 index 不指向被截断数据
compaction 后旧 index 不再对新 reader 可见
```

如果是 offset dense index，还要测：

- offset 连续时数组定位正确。
- offset 有空洞时不能误算位置。
- segment base offset 变化后相对 offset 正确。
- range read 起止边界正确。

如果是 key dense index，还要测：

- 重复 key。
- tombstone。
- snapshot read。
- latest read。
- 多版本读。

**恢复 correctness**

覆盖：

- data 有 index 无。
- index 有 data 无。
- index entry crc 错。
- index page 半写。
- checkpoint 旧。
- WAL replay 重复。
- compaction manifest 半发布。

恢复后点查结果必须和 data model 一致。

**stress test**

stress test 要打：

1. 高并发 put/read/delete。
2. 热点 key 反复更新。
3. compaction 与 point read 并发。
4. truncation 与 point read 并发。
5. index rebuild 与 read 并发。
6. kill -9 故障注入。
7. 磁盘满、fsync 失败、rename 失败。
8. 远端 index 上传失败和重试。

oracle 仍然用内存模型。对每个 offset 或 key，模型记录当前可见位置和版本，查询结果必须一致。

**benchmark**

benchmark 要测 dense index 是否值得：

```text
point read latency
range read latency
write throughput
index update latency
index memory bytes
index/data size ratio
recovery rebuild time
compaction index rewrite time
cache hit rate
```

要和 sparse index 做对照：

```text
dense: 更低 point read latency，更高写入和内存成本
sparse: 更低 index 成本，更高扫描成本
```

benchmark 还要覆盖 record size。record 越小，dense entry 的相对成本越高。

一句话：dense index 的 correctness test 要证明每条可见记录都有精确映射，stress test 要打热点更新、并发读写、compaction 和崩溃恢复，benchmark 要量化点查收益是否抵得过写放大、内存占用和恢复成本。

## Q063. 如果要求从零实现一个简化版 dense index，你会先定义哪些不变量？

**回答：**

从零实现 dense index，我会先明确它索引的是 offset 还是 key。两者不变量不一样。这里先给一个简化版 offset dense index，再补 key index 的额外要求。

**offset dense index 的不变量**

每条 committed record 都有 entry：

```text
for offset in [segment.base_offset, committed_offset):
  index[offset] exists
```

每条 entry 都必须有效：

```text
entry.offset == offset
entry.position 是 record batch 或 record 起点
entry.position < segment.size_bytes
data[entry.position].offset == offset
data[entry.position] checksum valid
```

如果按 record batch 建 dense index，就要把语义写清楚：

```text
每个 batch 有 entry
还是每条 record 有 entry
```

不要一会儿按 batch，一会儿按 record。

**数组寻址不变量**

如果 offset 连续，可以用数组：

```text
slot = offset - segment.base_offset
index[slot] = position
```

必须保证：

```text
slot >= 0
slot < entry_count
entry_count == committed_offset - base_offset
```

如果 offset 允许空洞，就不能用裸数组假装连续，要加 bitmap 或 map。

**可见性不变量**

```text
data 完整写入并提交后，index entry 才可见
index entry 可见后，reader 仍要校验 data
reader 不读 committed_offset 之后的 entry
```

entry_count 或 visible_offset 必须在 entry 完整写完后推进。

**持久化不变量**

可以选两种策略：

1. **index 可重建**

   ```text
   data 落盘
   index 内存维护
   checkpoint 定期落盘
   崩溃后从 data 重建缺口
   ```

2. **index 也持久化**

   ```text
   index update 写 WAL
   index page flush
   recovery replay WAL
   ```

简化版建议先做可重建，因为语义更干净。

**truncate 不变量**

```text
truncate_to(offset):
  删除 offset >= target 的 index entry
  committed_offset = target
  index 不再指向被截断 data
```

先截断可见 index，再处理 data，还是先截断 data，再重建 index，要写成固定流程，并用测试覆盖崩溃点。

**compaction 不变量**

dense index 不要原地改：

```text
旧 data + 旧 index 仍服务旧 reader
新 data + 新 index 构建完成
manifest 原子切换到新版本
旧版本等 reader 释放后 GC
```

这和 segment compaction 的 copy-on-write 思路一致。

**key dense index 的额外不变量**

如果索引 key：

```text
key -> latest_offset / position
```

还要定义：

- 重复 key 是保留最新还是保留多版本。
- tombstone 是否删除 index entry。
- snapshot 读是否能看到旧版本。
- update 是否必须满足 new_offset > old_offset。
- compaction 何时能丢旧 key 版本。

没有这些规则，key dense index 很容易在重试和并发更新下回退。

一句话：简化版 dense index 先守住每条 committed record 都有精确 entry、entry 指向完整 data、entry 可见性晚于 data 提交、truncate 不留下旧指针、compaction 用新旧版本原子切换；如果索引 key，还要额外定义重复 key、tombstone 和 snapshot 语义。

## 参考和校验点

- [Apache Kafka `OffsetIndex`](https://github.com/apache/kafka/blob/trunk/storage/src/main/java/org/apache/kafka/storage/internals/log/OffsetIndex.java) 说明 Kafka offset index 将 offset 映射到 segment 内物理位置，索引可以是稀疏的，并使用相对 offset。
- [Apache Kafka `AbstractIndex`](https://github.com/apache/kafka/blob/trunk/storage/src/main/java/org/apache/kafka/storage/internals/log/AbstractIndex.java) 展示 index 文件预分配、mmap、mutation lock、remap lock、resize、flush、rename 和 trimToValidSize 等实现细节。
- [Apache Kafka `LogSegment`](https://github.com/apache/kafka/blob/trunk/storage/src/main/java/org/apache/kafka/storage/internals/log/LogSegment.java) 展示 segment 追加、滚动、offset index 写入、time index 写入和 offset 到文件位置的转换流程。
- [Apache Kafka `LocalLog`](https://github.com/apache/kafka/blob/trunk/storage/src/main/java/org/apache/kafka/storage/internals/log/LocalLog.java) 展示 active segment append、log end offset 更新、segment 文件集合和本地日志生命周期管理。
- [Apache Kafka `LogSegments`](https://github.com/apache/kafka/blob/trunk/storage/src/main/java/org/apache/kafka/storage/internals/log/LogSegments.java) 展示按 base offset 管理 segment、`floorSegment` 定位和按 offset range 枚举 segment 的做法。
- [Apache Kafka `TopicConfig`](https://github.com/apache/kafka/blob/trunk/clients/src/main/java/org/apache/kafka/common/config/TopicConfig.java) 说明 `segment.bytes`、`segment.ms`、`segment.index.bytes`、`retention.bytes`、`retention.ms`、`local.retention.ms` 和 `local.retention.bytes` 等配置语义。
- [KIP-405: Kafka Tiered Storage](https://cwiki.apache.org/confluence/display/KAFKA/KIP-405%3A%2BKafka%2BTiered%2BStorage) 说明 Kafka 分层存储把 completed log segments 复制到远端，并区分 local tier 和 remote tier 的保留与读取路径。
- [LevelDB implementation notes](https://github.com/google/leveldb/blob/main/doc/impl.md) 说明 log file、memtable、SSTable、MANIFEST、CURRENT、recovery、compaction 和 obsolete file cleanup 的基本设计。
- [RocksDB Wiki: Leveled Compaction](https://github.com/facebook/rocksdb/wiki/Leveled-Compaction) 说明 L0、level 组织方式、leveled compaction 触发、不同 level 的目标大小和并行 compaction。
- [RocksDB Wiki: Universal Compaction](https://github.com/facebook/rocksdb/wiki/Universal-Compaction) 说明 universal/tiered compaction 面向较低写放大，并会在读放大和空间放大之间做权衡。
- [RocksDB Wiki: Snapshot](https://github.com/facebook/rocksdb/wiki/Snapshot) 说明 snapshot 固定 point-in-time 读视图，compaction 在丢弃旧版本时必须考虑 snapshot 可见性。
- [RocksDB Wiki: Delete Stale Files](https://github.com/facebook/rocksdb/wiki/Delete-Stale-Files) 说明旧 SST 文件不能在被 iterator、Get、compaction 或旧版本引用时立即删除，运行期依赖版本引用计数，重启后再根据 manifest 和文件状态清理。
- [RocksDB Wiki: Rate Limiter](https://github.com/facebook/rocksdb/wiki/Rate-Limiter) 说明用 `rate_bytes_per_sec`、refill period 和 fairness 控制 flush/compaction 后台写入速率，并支持运行期调节。
- [RocksDB Wiki: Write Stalls](https://github.com/facebook/rocksdb/wiki/Write-Stalls) 说明当 flush 或 compaction 跟不上时，系统会减速或停止写入，避免 L0 文件、pending compaction bytes 或 memtable 堆积继续恶化。
- [RocksDB Wiki: Compaction Stats and DB Status](https://github.com/facebook/rocksdb/wiki/Compaction-Stats-and-DB-Status) 说明 compaction 统计、写放大、读写吞吐、stall 时间、key drop 和 DB 状态指标，适合作为衡量后台整理影响的参考。
- [PostgreSQL Documentation: Indexes introduction](https://www.postgresql.org/docs/current/indexes-intro.html) 说明索引可以避免逐行扫描，但表修改时系统也要维护索引，因此索引会给写入带来额外开销。
- [MySQL 8.4 Reference Manual: InnoDB clustered and secondary indexes](https://dev.mysql.com/doc/refman/8.4/en/innodb-index-types.html) 说明 InnoDB clustered index 与 secondary index 的基本语义，可作为 dense/secondary index 讨论的数据库实现参照。
- [Amazon S3 User Guide: strong consistency and object model](https://docs.aws.amazon.com/AmazonS3/latest/userguide/Welcome.html) 说明 S3 对象、key、强读后写一致性、生命周期、Object Lock、Replication 等对象存储能力。
- [Amazon S3 multipart upload](https://docs.aws.amazon.com/AmazonS3/latest/userguide/mpuoverview.html) 说明大对象分片上传流程，适合作为 sealed segment 上传设计的参考。
