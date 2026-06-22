# 34. page cache、rename atomicity、WAL 与 LSN 追问链

这组问题延续前两份“追问链”的写法：不把基础概念写成百科定义，而是围绕面试官最可能压的五个方向展开。page cache、rename atomicity、WAL 和 LSN 都和“写入是否真的成立”有关，但它们处在不同层：内核缓存、目录项发布、恢复日志、日志位置。混在一起讲，听起来像懂；分清边界，才像真正做过系统。

## Q001. 面试官如果只问一个问题检验你是否理解 page cache，可能会问什么？

我会问：`write()` 返回成功以后，另一个进程马上 `read()` 能读到新内容，但机器断电后这段内容可能没了。你怎么解释这个现象？

这个问题能一下子看出你是否把“可见性”和“持久性”分开了。普通 buffered I/O 下，`write()` 通常先把用户态 buffer 复制到内核 page cache，对应的页变成 dirty page。后续读同一段文件时，内核可以直接从 page cache 返回新内容，所以看起来写入已经成功。可这时数据可能还没有进入稳定存储。断电、内核崩溃、虚拟化层丢 flush、底层设备丢缓存，都可能让这段 dirty data 消失。

好的回答不会停在“page cache 是缓存”。我会继续说明：page cache 同时服务读和写。读路径用它减少块设备访问；写路径用它吸收小写、合并 I/O、延后写回。内核后台 writeback 线程会在 dirty page 变老、dirty ratio 达到阈值、内存压力、`fsync()`、`sync()`、文件关闭或文件系统自身策略下把 dirty page 写出去。这个过程不是应用请求的事务边界。

还要主动提到 `fsync()`。如果系统要对外承诺“成功返回后崩溃还能恢复”，通常不能只依赖 `write()`，要在正确的对象上执行 `fsync()` 或同等级别同步，并检查返回值。对新文件发布，还可能要 fsync 父目录；对 `mmap()` 修改，要考虑 `msync()` 或后续 `fsync()`；对 `O_DIRECT`，绕过 page cache 也不等于自动获得持久化语义。

结合 LogServe 这类 shared log，面试里可以这样说：append record 写进 page cache 后，进程内 replay 可能已经看见它，但 durable offset 不能因此推进。只有对应 WAL/log segment 被同步并且同步错误被处理后，系统才能把这条 record 计入可恢复前缀。page cache 提升吞吐，不给提交语义背书。

## Q002. page cache 的一句话定义是否容易误导，误导点在哪里？

常见定义是“page cache 是 Linux 用内存缓存文件内容”。这句话没错，但太短，容易把人带到几个误区。

第一个误区是只把它想成读缓存。很多人听到 cache，就想到读热点、命中率、LRU。实际写入路径也大量依赖 page cache。应用写入后，页会被标记为 dirty，后台再写回设备。线上很多 fsync 抖动、dirty throttling、checkpoint 卡顿，根子都在“写入债务”积累，而不是读缓存命中率低。

第二个误区是把 page cache 当应用缓存。它不是某个进程私有的 map，而是内核按 inode、offset 管理的文件页缓存。多个进程读写同一个文件，通常会通过同一批缓存页看到变化。`mmap(MAP_SHARED)` 的文件页也和 page cache 有密切关系。你在一个进程里改了映射，另一个进程从文件读到变化，这仍然不代表它已经落盘。

第三个误区是把缓存命中理解成正确性。page cache 可以让读到的数据更新，也可以让 benchmark 很好看。但正确性边界要看应用协议：record 是否完整、CRC 是否通过、LSN 是否连续、commit marker 是否持久、恢复扫描从哪里截断。page cache 只保留 bytes，不理解这些业务结构。

第四个误区是以为绕过 page cache 就更正确。`O_DIRECT` 常用于减少缓存污染、控制 I/O 和避免双缓存，但它带来对齐、短写、部分失败、和 buffered I/O 混用的一堆细节。很多场景仍然需要 `fsync()`、`O_DSYNC` 或设备 flush 语义。绕过缓存是性能和内存管理选择，不是自动可靠性开关。

更稳的一句话是：page cache 是内核对文件页的共享内存缓存层，读路径用它复用数据，写路径用它暂存 dirty data 并延后写回；它改善性能和可见性，但不定义崩溃后的持久化承诺。

## Q003. page cache 最常见的生产事故触发条件是什么？

最常见的触发条件是把 `write()` 成功当成 durable commit。系统压测时吞吐很好，因为请求只写进 page cache 就返回；真正掉电或 kill -9 以后，最后一批已经 ack 的日志、配置或任务状态没有恢复出来。这个事故很典型：性能数字来自缓存，可靠性承诺却假装来自磁盘。

第二类事故是 dirty page 积压。前台持续写入，后台 writeback 跟不上，Dirty 和 Writeback 长期升高。开始时请求延迟很低，后来突然出现大面积卡顿，因为内核开始 dirty throttling，或者某次 `fsync()` 被迫等待大量旧债。很多人只看业务平均延迟，看不到 p99 被后台写回打穿。

第三类是缓存污染。顺序扫描大文件、备份、compaction、日志归档、模型文件加载，都可能把真正热的文件页挤掉。读路径看起来“偶发变慢”，实际是 page cache 被冷数据扫了一遍。对存储引擎来说，compaction 还会同时制造读 I/O、写 I/O 和缓存污染，前台查询被夹在中间。

第四类是 mixed I/O 和 `mmap()` 边界没想清楚。同一个文件同时走 buffered I/O、`O_DIRECT`、`mmap` 修改，或者写完 mmap 后只以为进程内可见就算完成，容易出现读到旧数据、性能异常、恢复行为不符合预期。不是所有组合都错，但必须明确同步点和失效规则。

第五类是延迟错误被忽略。底层写回失败、ENOSPC、EDQUOT、EIO 不一定在最初 `write()` 暴露。错误可能延迟到后续 `write()`、`fsync()` 或 `close()`。如果代码只检查 append 返回，不检查 fsync 返回，durable offset 就可能越过真实持久化位置。

面试里我会把事故总结成一句话：page cache 让写入变快，也让风险延后。系统如果没有把 ack、dirty writeback、fsync、错误传播和恢复截断连起来，就会在压力或崩溃时暴露问题。

## Q004. page cache 的指标应该怎么设计才不会只看平均值？

page cache 指标不能只看“缓存命中率”或“平均 write 延迟”。这两个数都容易骗人。读命中率高，不说明 dirty writeback 健康；平均 write 延迟低，可能只是因为写入先堆在内存里。

我会先看 dirty writeback 的状态。Linux 上至少要采集 Dirty、Writeback、nr_dirty、nr_writeback、nr_dirtied、nr_written。更重要的是看差值和趋势：`nr_dirtied` 长期快于 `nr_written`，说明写入债务在扩大；Writeback 长期不降，说明设备或文件系统写回跟不上；Dirty 接近阈值时，前台写入很可能被 throttle。

第二类是同步路径指标。所有 `fsync`、`fdatasync`、目录 fsync 都要做 histogram：p50、p95、p99、p999、max，而不是平均值。还要记录每次同步覆盖的 bytes、record 数、等待者数、durable LSN 推进量、错误码。page cache 相关事故通常不发生在平均路径，而发生在某次集中还债。

第三类是缓存效率，但要分场景。读路径可以看 page cache hit/miss、major fault、minor fault、readahead 命中、按文件类型或业务路径分桶的读延迟。不要只放一个全局 hit ratio。一个大备份任务把全局命中率拉低，和核心 manifest 文件被挤掉，影响完全不同。

第四类是资源和压力。要看内存可用量、PSI、cgroup memory pressure、block device await、队列深度、flush/FUA 次数、云盘 throttle、容器写层大小。page cache 是内存和 I/O 的交界层，单看内核缓存页会漏掉设备背压。

第五类是语义风险窗口。对日志系统来说，应该有 `acknowledged_not_durable_bytes`、`durable_offset_lag`、`dirty_log_bytes`、`recovery_truncated_records` 这类指标。它们直接回答“已经告诉用户成功、但崩溃后还不一定恢复”的窗口有多大。这个窗口比平均缓存写入速度更接近正确性。

面试里可以说：page cache 的指标要同时看读缓存、写回债务、同步尾延迟和持久化风险窗口。只看平均值，等于只看它最漂亮的一面。

## Q005. page cache 的正确性边界和性能边界分别是什么？

page cache 的正确性边界先从一句话说清楚：它能提供内核文件视图里的可见性，不提供崩溃后的持久性。`write()` 后同机读取能看到新 bytes，通常是 page cache 的效果；断电后还能不能看到，要看这些 bytes 有没有通过文件系统和设备层同步到稳定存储。

它也不提供应用 record 边界。page cache 不知道一条日志从哪里开始、length 是否完整、CRC 是否匹配、commit record 是否出现、LSN 是否连续。恢复时必须由应用自己的格式扫描有效前缀。对 append-only log，尾部半条 record 可以截断；中间坏 record 不能随便跳过。这个判断不属于 page cache。

目录项也是边界。写新文件、`fsync(file)`、`rename`、`fsync(dir)` 是几个不同动作。page cache 主要处理文件页；文件名发布、目录项持久化和跨目录 rename 还要看目录和文件系统日志。只说“page cache 已经写回”不能证明新名字崩溃后一定存在。

性能边界来自内存、写回和缓存污染。page cache 可以合并小写、吸收突发、减少重复读，但它不是无限缓冲区。写入速度长期超过设备写回能力，就会形成 dirty debt；工作集超过内存，读缓存会抖；冷数据扫描会挤掉热页；后台 checkpoint 或 compaction 会把 p99 拉高。

优化要按边界做。可以通过 group commit 降低 fsync 次数，通过 checkpoint smoothing 平滑写回，通过 `posix_fadvise` 或分层存储减少缓存污染，通过 `O_DIRECT` 控制大顺序 I/O 的缓存影响。不能为了快就把 durable ack 改成 page-cache ack，除非接口明确接受可丢窗口。

结合 LogServe，page cache 的合理用法是让 append 路径快、让恢复扫描受益于顺序读；正确性仍要落在 length、CRC、LSN、fsync 策略和恢复截断上。面试里把这两层分开，回答就不会飘。
## Q006. 面试官如果只问一个问题检验你是否理解 rename atomicity，可能会问什么？

我会问：写配置文件时，你先写 `config.tmp`，`fsync(config.tmp)`，再 `rename(config.tmp, config.json)`。进程运行期间读者会不会看到半个新文件？机器立刻断电后，新文件名一定还在吗？

这个问题很有效，因为它把 rename 的两个边界分开了。运行时，`rename` 在同一文件系统内提供路径名更新的原子性。读者通常不会在 `config.json` 这个路径上看到“半个旧文件半个新文件”。要么看到旧 inode，要么看到新 inode。已经打开旧文件的 fd 仍然指向旧对象，不会因为路径被替换而自动变成新内容。

但崩溃持久化是另一件事。`rename()` 返回成功，说明内核完成了目录项变更的系统调用语义，不等于父目录项已经持久化。稳妥的发布协议通常是：写临时文件，检查所有 write；`fsync(tmp_fd)`；`rename(tmp, final)`；打开父目录并 `fsync(dir_fd)`。跨目录时还要考虑源目录和目标目录。少了目录 fsync，掉电后可能看到旧名字、看不到新名字，或者恢复结果依赖文件系统和挂载参数。

还要讲同一文件系统这个条件。跨文件系统 rename 会失败为 `EXDEV`，很多库会退化成 copy + unlink。这个退化路径不是原子替换，读者可能看到中间状态，权限、xattr、fsync 顺序也可能出问题。面试官问 rename atomicity，候选人如果马上说“跨盘也一样”，基本就露馅了。

结合 LogServe，manifest、checkpoint、segment index 这类文件发布可以用 temp + fsync + rename + dir fsync。rename 负责让读者看到一个完整版本，fsync 负责让崩溃后这个版本还在。两者缺一块，协议就不完整。

## Q007. rename atomicity 的一句话定义是否容易误导，误导点在哪里？

常见定义是“rename 是原子的”。这句话太容易误导，因为它省掉了对象、范围和时间点。

第一个误导是没说“原子的是路径名变化”。rename 处理的是目录项，也就是名字到 inode 或文件对象的映射。它不是把文件内容写入变原子，也不是让多文件更新变成一个事务。你 rename 一个内容没 fsync 完的文件，读者可能运行时只看到完整路径切换，但崩溃恢复时仍然可能丢。

第二个误导是没说“同一文件系统内”。同一挂载点内的 rename 和跨文件系统的 copy + unlink 完全不是一回事。跨文件系统没有单个目录项事务可以覆盖两边，应用如果要支持这种路径，必须设计临时目录位置，或者明确拒绝跨设备发布。

第三个误导是把 atomic replace 当成 compare-and-swap。普通 rename 覆盖已有目标时，不会检查目标是否还是你刚才看到的那个版本。两个 writer 同时发布，后 rename 的可能直接覆盖先 rename 的。需要“不覆盖已存在”语义时，要看 `renameat2(RENAME_NOREPLACE)`、link-based publish、版本文件名或上层锁。还要确认底层文件系统支持相应 flag。

第四个误导是把运行时原子性当成崩溃原子性。rename 返回后，当前系统调用观察者通常不会看到目标路径消失的中间状态；但断电以后，目录项是否恢复到新状态，要看目录持久化。这个区别是很多持久化面试题的核心。

更准确的一句话是：rename 在同一文件系统内原子地改变路径名映射，常用于完整文件发布；它不自动持久化父目录，不保证文件内容已落盘，不提供多文件事务，也不解决并发 writer 的版本冲突。

## Q008. rename atomicity 最常见的生产事故触发条件是什么？

最常见的是忘记 fsync 父目录。代码写 temp 文件、fsync 文件、rename 成功，然后向上层宣布 manifest 或配置发布完成。断电后，文件内容可能在，但新名字不一定在；或者目标名仍然指向旧版本。这个事故很难在普通单元测试里复现，必须靠 crash test 或故障注入。

第二类是临时文件放错地方。开发时 temp 和 final 在同一个目录，线上有人把 temp 放到 `/tmp` 或另一个挂载点。rename 返回 `EXDEV`，库函数悄悄走 copy + unlink。读者可能看到部分拷贝文件，权限或所有者丢失，copy 途中崩溃还会留下脏目标。可靠发布协议最好把 temp 文件创建在目标目录里。

第三类是覆盖并发。两个进程都生成 `new.tmp` 或不同 temp，然后 rename 到同一个 final。普通 rename 的语义是后者覆盖前者，不关心业务版本。配置发布、checkpoint manifest、leader 写租约文件，如果没有 epoch、generation 或 CAS 约束，最后留下的不一定是业务上最新或合法的版本。

第四类是把 watcher 当提交协议。文件监听器看到 rename 事件就立刻消费，但发布方其实还没 fsync 目录，或者新文件内容缺少 checksum、generation、完整性字段。运行时可能没问题，崩溃恢复或并发发布时就会出错。watch 只能提示“路径变了”，不能证明发布持久完成。

第五类是网络文件系统和特殊文件系统语义差异。NFS、FUSE、overlay、容器写层、对象存储挂载都可能让本地 POSIX 直觉不够用。rename 本身可能支持，但缓存一致性、错误报告、目录 fsync、close-to-open 行为会影响观察结果。跨机器协调时不能只靠单机 rename。

面试里我会说：rename 事故通常不是 rename 不原子，而是应用把它用成了“内容持久化 + 目录持久化 + 并发仲裁 + 分布式一致性”的替代品。它只负责其中一小段。

## Q009. rename atomicity 的指标应该怎么设计才不会只看平均值？

rename 指标如果只看平均耗时，基本抓不到真正风险。rename 很多时候很快，出事的是失败分类、发布协议步骤、目录 fsync 尾延迟和恢复后的版本异常。

我会把发布流程拆成步骤指标：temp write latency、temp fsync latency、rename latency、dir fsync latency、总发布耗时。每个都要 histogram，尤其看 p99 和 max。目录 fsync 往往被忽略，但它才是持久化发布里最容易在尾部变慢的步骤之一。

错误指标要按 errno 和路径分类：`EXDEV`、`EEXIST`、`ENOSPC`、`EDQUOT`、`EIO`、`ENOENT`、`EPERM`、`EINVAL`。其中 `EXDEV` 要单独告警，因为它意味着原子替换协议可能被迫退化。`EEXIST` 对 `RENAME_NOREPLACE` 路径不是普通错误，而是并发冲突信号。

还要有语义指标。比如 manifest generation 是否单调，发布后读回校验是否通过，旧版本是否被错误恢复，启动时发现 latest 指针缺失次数，恢复时回退到旧 manifest 的次数，临时文件残留数量，孤儿 segment 数量。rename 的可靠性最终要从恢复状态上看，而不是只从系统调用返回值看。

观察者侧也要打点。文件 watcher 收到 rename 到真正消费完成的延迟、消费时 generation mismatch、checksum mismatch、读取 ENOENT、打开旧 fd 的数量，都能反映发布协议是否清楚。很多线上问题不是 writer 失败，而是 reader 对 rename 的理解太乐观。

结合 LogServe，我会给 checkpoint 或 index manifest 发布放一组面板：发布步骤 p99、dir fsync p99、rename errno 分布、generation 冲突、启动恢复选择的 manifest generation、临时文件清理数量。面试里可以说：rename 指标要围绕“发布协议是否真的完成”设计，不只是围绕 `rename()` 调用了多久。

## Q010. rename atomicity 的正确性边界和性能边界分别是什么？

rename 的正确性边界是单个路径名映射的原子更新。通常在同一文件系统内，如果目标已经存在，rename 可以让目标路径从旧对象切到新对象，观察者不会看到半个目录项。打开的 fd 继续指向原对象；硬链接、符号链接、目录、权限和特殊文件还各有细节。

它不保证文件内容持久。文件内容要靠写入检查、`fsync(file)` 或相应同步机制。它不保证目录项崩溃后持久，目录要靠 `fsync(dir)`。它不保证多个文件一起切换，除非上层用一个 manifest 指针把多文件集合收敛成单文件发布。它也不保证并发 writer 的业务顺序，除非加 generation、epoch、CAS 或锁。

跨文件系统是明确边界。`EXDEV` 不是小问题，而是在告诉你没有同一个文件系统事务。可靠系统要么让 temp 和 final 位于同一目录，要么把跨设备发布设计成显式的多步协议，并承认它不是普通 rename atomicity。

性能边界通常不在目录项更新本身，而在元数据日志、目录 fsync、文件系统锁、目录规模、存储设备 flush 和并发发布。rename 可能很快，随后的目录 fsync 很慢；也可能在同一目录大量小文件发布时被 inode、dentry、journal 和锁竞争拖住。

优化时可以减少发布频率、合并 manifest、使用 generation 文件名、让 reader 读单个 latest 指针、清理临时文件、把 temp 放目标目录、把大数据文件和小 manifest 分开同步。不能为了省目录 fsync 就假装发布已经 crash-safe。如果承诺只是运行时原子切换，可以少同步；如果承诺崩溃后仍指向新版本，就要付持久化成本。

面试里的边界回答可以这样说：rename 是发布完整文件的好工具，但它只解决“名字怎么切”。内容是否完整、目录项是否持久、多个文件是否一致、多个 writer 谁赢，这些都要由协议补上。
## Q011. 面试官如果只问一个问题检验你是否理解 WAL，可能会问什么？

我会问：事务已经把数据页改在内存里了，为什么还必须保证对应 WAL 先于脏数据页落盘？如果反过来会发生什么？

这个问题比“WAL 是 Write-Ahead Log”更有区分度。WAL 的核心不是“日志文件存在”，而是 write-ahead rule：在数据页修改被写回稳定存储之前，描述这次修改的 WAL record 必须已经持久化。内存里先改数据页并不违反 WAL；危险的是把没有日志保护的数据页刷到磁盘。

反例很简单。事务 T 改了 page P，后台线程先把 page P 刷到了磁盘，但对应 WAL record 还没落盘。机器崩溃后，磁盘上的 page P 已经包含 T 的修改，WAL 里却没有这次修改的证据。恢复程序不知道这个修改是否提交，也不知道如何 redo 或 undo。这个状态比“数据页没刷盘”更坏，因为它让磁盘状态失去日志解释。

正确路径是：修改生成 WAL record，分配 LSN；提交时把 commit record 以及之前必要 WAL flush 到稳定存储；脏数据页可以之后再刷。若崩溃发生在数据页刷盘前，恢复用 WAL redo；若数据页已经包含修改，恢复用 pageLSN 或类似机制判断是否需要跳过。WAL 让数据页写回和事务提交解耦，但没有放弃顺序约束。

好的回答还会提 group commit。多个事务可以共享一次 WAL flush，只要每个事务收到成功前，它的 commit record 已经包含在 durable LSN 以内。这样吞吐更好，但 ack 语义不变。为了性能提前 ack，再由后台慢慢 flush，那就从 durable commit 变成 async commit，必须明说可能丢哪些已 ack 事务。

结合 LogServe，我会说 shared log 的 append record 只有进入 durable prefix 后，才能作为恢复时可信的状态机输入。WAL 不是把 bytes 写进一个文件那么简单，它定义了“哪些修改在崩溃后仍有证据”。

## Q012. WAL 的一句话定义是否容易误导，误导点在哪里？

常见定义是“WAL 就是先写日志再写数据”。这句话适合入门，但面试里很容易误导。

第一个误导是把“先写”理解成所有内存修改前都必须先写磁盘。实际数据库通常会先在内存里修改 buffer page，同时生成 WAL record。关键约束是持久化顺序：数据页落盘前，保护它的 WAL 必须先落盘；事务对外提交前，提交所需 WAL 必须先落盘。

第二个误导是忽略 record 边界。WAL 不是一串随便 append 的文本。成熟 WAL 要定义 record header、length、type、prev pointer 或连续性、CRC、LSN、事务 id、page id、payload、padding、segment 切换和尾部半写处理。恢复程序相信的是有效 WAL 前缀，不是文件大小。

第三个误导是把 WAL 和 redo 混成一件事。WAL 是写前日志原则；具体恢复可以有 redo-only、undo/redo、logical log、physical log、physiological log、commit marker、compensation log record。ARIES、PostgreSQL、InnoDB、RocksDB、SQLite WAL mode 的细节都不一样。只说“有 WAL 就能恢复”太粗。

第四个误导是以为 WAL 解决所有一致性。WAL 解决的是本地崩溃恢复和提交持久化的一部分。事务隔离要靠锁或 MVCC；跨节点提交要靠复制和 quorum；外部副作用要靠 outbox、幂等或补偿；数据损坏还要靠 checksum 和 page 校验。WAL 不能替它们干活。

更准确的一句话是：WAL 是一种把修改先记录到可恢复日志、再允许数据页异步落盘的持久化协议；它用 LSN、flush 边界和恢复扫描定义哪些修改在崩溃后仍然成立。

## Q013. WAL 最常见的生产事故触发条件是什么？

最常见的是 ack 位置错了。代码把 WAL record 写进进程 buffer 或 page cache 后就返回成功，后台再 fsync。平时性能很好，断电后发现最近一批“已提交”事务没了。这个问题不是不能用异步提交，而是接口没有承认可丢窗口。

第二类是没有处理坏尾巴。WAL segment 末尾可能出现半个 header、半个 payload、CRC 不匹配、length 超界、segment 切换中断。恢复程序如果不会停在最后一个有效 record，就可能启动失败，或者把坏 bytes 当合法 record replay。正确做法通常是扫描到第一个不可证明有效的位置，然后截断、隔离或进入人工恢复流程。

第三类是 checkpoint 和 retention 算错。checkpoint 记录说某个 LSN 之前的数据页都安全了，系统就可能回收旧 WAL。若 checkpoint 实际没有把必要数据页落盘，或者复制、归档、慢消费者还需要旧 WAL，却被 retention 删除，恢复和副本追赶都会断链。日志系统最怕“为了省空间删掉恢复证据”。

第四类是 full page write 或 torn page 问题处理不足。数据页写入可能只写了一半，尤其在崩溃时。数据库常用 full-page image、doublewrite buffer、page checksum 等机制降低风险。如果只记录增量修改，而崩溃后基础页本身撕裂，redo 也可能没法正确应用。

第五类是 group commit 错误传播。多个请求等同一次 fsync，fsync 失败时必须让这一批等待者都失败，不能只让执行 fsync 的线程感知错误。否则 durable LSN 没推进，客户端却收到了成功。

第六类是 WAL 目录膨胀到磁盘满。归档失败、replication slot 卡住、standby 长期落后、checkpoint 太远、compaction 跟不上，都会让 WAL 无法回收。磁盘满以后，新 WAL 写不进去，checkpoint 也可能写不完，系统会在最需要恢复证据的时候失去写入能力。

面试里我会把这些归成一句：WAL 事故不是“忘了写日志”这么简单，更多是提交边界、有效前缀、回收边界和错误传播没定义清楚。

## Q014. WAL 的指标应该怎么设计才不会只看平均值？

WAL 指标要围绕 LSN 水位和同步尾延迟设计。只看平均 WAL 写入耗时没有意义，因为事务感受到的是等待 flush 的尾部，恢复风险看的是 durable LSN 和已 ack LSN 的差距。

第一组是位置指标：insert LSN、write LSN、flush LSN、commit/durable LSN、checkpoint LSN、redo start LSN、oldest required LSN、archive LSN、replica replay LSN。不同系统名字不一样，但思想一样：插入、写出、刷盘、提交、检查点、归档、复制不是一个位置。

第二组是延迟和批量指标。WAL write latency、WAL fsync latency、commit wait latency 都要 histogram，至少看 p50、p95、p99、p999、max。group commit 要看 batch size、batch wait time、每次 flush 覆盖的 bytes 和事务数、等待队列长度。平均值看不出某次 flush 卡住几百个请求。

第三组是吞吐和债务指标。WAL bytes/s、records/s、segment rollover 次数、pending WAL bytes、flush lag bytes、checkpoint age、redo distance、retained WAL bytes、archive lag、replication slot retained bytes。WAL 系统健康不只是能写，还要能同步、归档、复制和回收。

第四组是恢复和损坏指标。启动恢复扫描了多少 bytes，replay 到哪个 LSN，截断了多少 tail records，CRC mismatch 发生在尾部还是中间，遇到 unsupported record version 多少次，checkpoint 恢复耗时 p99。没有这些指标，系统平时看起来健康，真正重启时才知道恢复路径烂了。

第五组是错误传播指标。fsync EIO、ENOSPC、EDQUOT、archive failure、segment preallocation failure、WAL write short write、group commit failed waiters。还要记录失败后是否停止推进 durable LSN、是否进入只读、是否拒绝新写入。

结合 LogServe，我会给 shared log 放这些面板：append throughput、WAL fsync p99、durable offset lag、batch 覆盖 record 数、bad tail 截断数量、segment retained bytes、恢复 replay 耗时。面试里可以说：WAL 平均写得快不代表系统可靠，真正要看尾部同步、LSN 差距和恢复证据是否完整。

## Q015. WAL 的正确性边界和性能边界分别是什么？

WAL 的正确性边界是本地恢复协议。事务或事件什么时候算提交，要看它的 commit record 或等价标记是否包含在 durable WAL 前缀里。崩溃后系统从 checkpoint 或快照开始，扫描有效 WAL，按 LSN 顺序 redo 或 undo，并用 pageLSN、sequence、checksum、record length 等机制保证幂等和截断。

它不是分布式提交边界。单机 WAL flush 成功，只能说明本机有恢复证据；副本是否收到、多数派是否持久、leader 是否仍然有效、外部对象存储是否提交，要看复制协议和上层状态机。把单机 WAL 说成“强一致分布式提交”，面试官很容易继续追问到崩。

它也不是事务隔离边界。WAL 记录修改如何恢复，不决定并发事务读到什么。隔离级别、锁、MVCC、冲突检测、唯一约束和幂等键都在别的层。WAL 可以记录它们的结果，但不能替代它们的判定。

性能边界来自顺序写和同步 flush。WAL 的优势是把提交路径变成顺序 append，并允许 group commit；瓶颈则常在设备 flush、WAL buffer 锁、segment 创建、CRC、压缩、复制发送、归档和 checkpoint I/O。高吞吐系统里，WAL 本身写得快，fsync 和下游保留边界反而更贵。

优化要守住 ack 语义。可以做 group commit、batch fsync、预分配 segment、压缩、异步归档、checkpoint smoothing、WAL 和数据文件分盘、后台限速。也可以提供 async 模式。但只要客户端收到的是 durable success，就不能在 WAL 还没进入 durable prefix 时返回成功。

结合 LogServe，WAL 或 shared log 的边界可以说得很清楚：单机实验能展示 append、fsync 策略、replay 和恢复截断；生产级多机可靠性还需要复制、quorum、故障域和 leader fencing。这样讲不夸大项目，也能说明你知道 WAL 的力气用在哪里。
## Q016. 面试官如果只问一个问题检验你是否理解 LSN，可能会问什么？

我会问：主库有 insert LSN、write LSN、flush LSN，备库有 receive LSN、replay LSN。客户端问“我的事务是不是已经安全了”，你该看哪个 LSN？复制延迟又该怎么算？

这个问题能测出你是否知道 LSN 不是一个泛泛的“版本号”。LSN 是日志流里的位置。不同 LSN 表示不同阶段：写入 WAL buffer 的逻辑末尾、写到内核或文件的末尾、flush 到稳定存储的位置、备库接收到的位置、备库 replay 到数据页的位置、业务 apply 到状态机的位置。名字相近，语义差很多。

如果问题是本机 durable commit，要看事务 commit record 是否已经包含在 flush/durable LSN 以内。只到 insert LSN，说明日志 record 被分配并放入内存结构；只到 write LSN，可能说明已经写出到操作系统，但还不一定稳定；到 flush LSN，才接近“崩溃后本机可恢复”的边界。具体系统名字不同，但这个分层要能讲清楚。

如果问题是复制延迟，要先问“延迟指什么”。网络接收落后，可以比较 primary write/flush LSN 和 standby receive LSN；持久化落后，要看 standby flush LSN；查询可见或状态机执行落后，要看 replay/apply LSN。把这些都叫 replication lag，会让排障变得很乱。

LSN 差值通常是 bytes，不是秒。PostgreSQL 的 `pg_wal_lsn_diff` 返回两个 WAL 位置之间的字节差。要把 bytes lag 转成时间，要结合当前 WAL 生成速率、是否突发写入、是否有大事务、是否卡在 replay。100MB lag 在高峰时可能几秒，低峰时可能很久。

结合 LogServe，LSN 可以对应 shared log offset 或 durable offset。调度器要区分 appended offset、fsynced offset、replayed offset、materialized view applied offset。面试里说清楚这些水位，远比说“我们用 LSN 保证顺序”有说服力。

## Q017. LSN 的一句话定义是否容易误导，误导点在哪里？

常见定义是“LSN 是 Log Sequence Number，表示日志序列号”。这句话太松，容易让人误会成自增 id、事务 id、时间戳或业务版本号。

第一个误导是“number”。在很多系统里，LSN 更像日志字节流的位置，而不是每条记录加一的整数。PostgreSQL 的 `pg_lsn` 内部是 64 位位置，打印成两个十六进制数，中间有斜杠；两个 LSN 可以比较，也可以相减得到字节距离。你不能只按十进制自增 id 的直觉处理它。

第二个误导是忽略 record 边界。一个 WAL record 可能有 start LSN 和 end LSN。事务提交时，真正用于判断 durable 的通常是 commit record 覆盖到的位置，而不是某个随便取的起点。恢复扫描也要按完整 record 前进，不能在任意 byte offset 截断后还说那是合法 LSN。

第三个误导是把 LSN 当全局时间。LSN 能表示同一条日志流里的顺序，但不能直接表示物理时间。大事务、空闲期、批量写入、checkpoint、压缩、不同 timeline 都会让 LSN 和时间关系变得不直观。两个系统的 LSN 也不能裸比较，除非它们属于同一日志流、同一 epoch、同一 timeline 或有明确映射。

第四个误导是把 LSN 当业务可见版本。数据页的 pageLSN 可以告诉恢复程序这个 page 已经应用到哪个 WAL 位置；复制的 replay LSN 可以告诉备库重放到哪里；但业务读是否可见还受事务隔离、snapshot、apply 线程、materialized view 更新影响。LSN 是底层顺序，不自动等于上层语义完成。

更准确的一句话是：LSN 是日志流中的有序位置，用来标记 WAL record、durable 边界、恢复进度和复制进度；它必须和日志流身份、record 边界、flush/replay 阶段一起解释。

## Q018. LSN 最常见的生产事故触发条件是什么？

最常见的是把不同阶段的 LSN 混用。比如用 insert LSN 向客户端确认提交，实际 flush LSN 还没追上；用 standby receive LSN 判断查询已经可见，实际 replay LSN 卡住；用 write LSN 判断归档安全，实际 durable flush 还没完成。这类错误平时不显眼，崩溃或 failover 时会直接变成数据丢失或读旧数据。

第二类是 failover 后 timeline 或 epoch 没带上。主库切换后，新主可能从某个共同点分叉。裸 LSN 数字看起来更大，不代表和旧主同一条历史。没有 timeline、term、epoch、leader generation 这类上下文，系统可能把旧 leader 的日志位置和新 leader 的日志位置错误比较。

第三类是 retention 边界算错。系统根据 checkpoint LSN 删除旧 WAL，却忘了 replication slot、归档、慢消费者、备份、PITR 或某个 materialized view 还需要更早位置。等故障发生，需要从旧 LSN 恢复时，证据已经被删了。LSN 不只是提交水位，也是“最早还不能删”的安全边界。

第四类是 off-by-one 和 segment 边界。WAL segment 文件名、segment offset、record end LSN、下一条 record 起点，很容易写错。某些函数返回“刚切完 segment 的结束位置加一”，某些系统记录“最后已应用 record 的 end offset”。边界没写清楚，就会出现重复 replay 一条 record、漏 replay 一条 record、或者归档缺一个 segment。

第五类是把 LSN lag bytes 当时间告警。复制落后 1GB 不一定永远严重，落后 8MB 也不一定安全。要看写入速率、replay 速率、是否持续增长、是否有 apply lock、是否卡在单个大事务、是否阻塞 WAL 回收。单点数字不够。

结合 LogServe，如果 durable offset、replay offset、materialized view offset 使用同一个字段名，迟早会出事故。它们都像 LSN，但阶段不同。字段名应该把阶段写进去，比如 `append_lsn`、`flush_lsn`、`apply_lsn`，否则排障时没人知道系统到底卡在哪里。

## Q019. LSN 的指标应该怎么设计才不会只看平均值？

LSN 指标最不应该做成一个平均 replication lag。平均值会把慢副本、卡住的 apply 线程、归档积压、retention 风险全部盖掉。

我会先设计水位面板。每个日志流展示 insert、write、flush、durable、checkpoint、archive、replica receive、replica flush、replica replay、consumer apply。不是所有系统都有这些名字，但要把“已经生成、已经写出、已经持久、已经复制、已经应用”分开。水位之间的 gap 比单个 LSN 更有用。

第二类是 lag 分布。按副本、分区、stream、tenant、consumer group 分桶，看 bytes lag 的 p50、p95、p99、max，也看持续时间。一个副本短暂落后 500MB，和一个副本 30 分钟不动，是两种不同问题。最好同时记录 lag bytes 和 lag age，但要承认 age 依赖心跳或采样点。

第三类是推进速率。WAL 生成 bytes/s、flush bytes/s、replay bytes/s、apply bytes/s、archive bytes/s。只看当前差距，不看速率，就不知道系统是在追上、持平还是越欠越多。恢复时间估算也要靠剩余 bytes 和实际 replay throughput，而不是靠平均写入吞吐。

第四类是安全边界。oldest required LSN、oldest replication slot LSN、oldest backup LSN、oldest consumer LSN、last checkpoint LSN、retained WAL bytes、可回收 LSN。告警要围绕“离磁盘满还有多久”和“是否已经删到某个需要的位置”，不是只看最新 LSN 多大。

第五类是异常指标。LSN 倒退、timeline mismatch、record gap、duplicate apply、replay checksum failure、apply blocked duration、flush LSN 长时间不动、commit LSN 超过 flush LSN 的已 ack 请求数。对可靠系统来说，水位不单调或阶段倒挂比普通延迟更危险。

结合 LogServe，我会展示 append offset、fsynced offset、scheduler replay offset、actor/materialized view apply offset、oldest retained segment offset、recovery start/end offset。面试里可以说：LSN 指标要用多个水位解释系统状态，平均 lag 只能当入口，不能当答案。

## Q020. LSN 的正确性边界和性能边界分别是什么？

LSN 的正确性边界是同一日志历史内的有序位置。它可以帮助系统判断 record 顺序、恢复进度、page 是否需要 redo、哪些 WAL 可以回收、复制或消费追到哪里。前提是 LSN 属于同一条日志流，并且带上必要的 timeline、epoch、term、partition 或 shard 身份。

它不自动证明 record 有效。某个 LSN 只是位置，位置上的 record 仍要通过 length、CRC、magic、version、commit marker、权限和业务校验。恢复时不能因为 offset 单调就信任所有 bytes。LSN 给顺序，record 格式给可解析性，fsync 给本地持久化边界，状态机给业务含义。

它也不自动提供幂等。pageLSN 可以帮助数据页避免重复 redo；consumer offset 可以帮助消费者知道读到哪里；但业务操作是否幂等，还要靠 request id、dedup table、状态机版本、fencing token。把 LSN 暴露给业务层时，要小心不要让它承担业务唯一性之外的责任。

性能边界看起来很轻，比较 LSN、相减、推进原子水位都很便宜。真正成本来自围绕它的同步和保留：频繁发布 durable LSN 可能增加锁或原子竞争；太保守的 oldest required LSN 会保留大量 WAL；太激进的推进会破坏恢复；按每条 record 都刷指标会制造高基数和热路径开销。

另一个性能边界是 lag 解释。LSN bytes 差距本身只是字节数。把它转成时间、容量、恢复 ETA，需要 WAL 生成速率、replay 速率、segment 大小、压缩、I/O 限速和大事务行为。错误的换算会导致告警太早、太晚，或者在高峰期疯狂误报。

结合 LogServe，LSN 或 log offset 的正确用法是定义几个清楚水位：append 到哪里、fsync 到哪里、恢复扫描确认到哪里、状态机 apply 到哪里、最早还要保留哪里。性能优化可以降低水位发布频率、批量推进、按 segment 汇总指标，但不能让 durable offset 跑到真正 fsync 位置前面。这个边界说清楚，LSN 才是工程工具，不是装饰性术语。
