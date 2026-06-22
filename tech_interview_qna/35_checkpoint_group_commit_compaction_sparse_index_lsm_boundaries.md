# 35. checkpoint、group commit、segment compaction、sparse index 与 LSM tree 追问链

这组问题继续按面试追问组织。checkpoint、group commit、segment compaction、sparse index 和 LSM tree 都很容易被一句话讲得过于轻松：听起来像“定期保存”“批量刷盘”“清理旧数据”“少建点索引”“顺序写”。真正的面试点在边界：什么时候可以承诺恢复，什么时候只是性能优化，什么时候后台任务会把前台尾延迟打穿。

## Q001. 面试官如果只问一个问题检验你是否理解 checkpoint，可能会问什么？

我会问：数据库已经有 WAL，为什么还需要 checkpoint？如果永远不 checkpoint，系统是不是仍然能恢复？

这个问题能把 checkpoint 的本质问出来。WAL 让系统在崩溃后可以从日志重放修改；checkpoint 不是替代 WAL，而是把一部分已经由 WAL 保护的脏状态推进到数据文件或快照里，然后记录一个恢复起点。这样重启时不用从很早以前的日志开始扫，也能释放一部分不再需要的 WAL 或 segment。

如果永远不 checkpoint，理论上只要从最早需要的位置开始保留完整 WAL，恢复仍然有可能完成。问题是成本会失控：WAL 越积越多，恢复时间越来越长，归档和复制压力越来越大，磁盘也可能被日志打满。checkpoint 是把“以后恢复要做的工作”提前还掉一部分。

好的回答会说出三个水位。第一，checkpoint record 或 manifest 记录了恢复从哪里开始。第二，checkpoint 之前哪些数据页、索引页、状态快照已经安全。第三，哪些旧 WAL 仍然不能删除，因为归档、复制、备份、慢消费者或快照还在用。只讲“定期保存当前状态”，会漏掉 WAL 保留边界。

还要说性能影响。checkpoint 要把 dirty page 写出去，可能制造大量后台 I/O。做得太频繁，前台持续被写回和 full-page logging 拖慢；做得太稀疏，恢复时间长，WAL 保留多。PostgreSQL 这类系统会把 checkpoint 写出摊开，避免在最后一次 fsync 时集中卡住。

结合 LogServe，可以把 checkpoint 讲成 replay 成本控制：shared log 仍然是事实来源，checkpoint cache 或 materialized view snapshot 只是把某个 LSN 之前的状态固化下来。重启时从最近安全 checkpoint 加后续日志恢复，而不是从创世记录开始扫。这个回答比“checkpoint 是备份”准确得多。

## Q002. checkpoint 的一句话定义是否容易误导，误导点在哪里？

常见定义是“checkpoint 是定期把内存状态保存到磁盘”。这句话能帮助入门，但面试里会误导。

第一个误导是把 checkpoint 说成完整备份。checkpoint 通常只保证某个恢复边界之前的脏数据已经推进到稳定位置，或者某个快照文件已经发布。它不等于可长期保存的备份，也不一定能跨机器、跨版本、跨故障域独立恢复。备份还要考虑拷贝数据文件、归档 WAL、校验、保留策略和恢复演练。

第二个误导是忽略 WAL。没有 WAL 语义，checkpoint 很难说明哪些修改已经提交、哪些需要重放、哪些要丢弃。成熟系统里的 checkpoint 通常会记录 redo point、LSN、manifest generation、snapshot id 或类似位置。这个位置才是恢复逻辑的核心。

第三个误导是以为 checkpoint 完成后所有旧日志都能删。能不能删，要看最保守的保留边界：恢复、归档、复制、备份、PITR、慢消费者、长事务、快照读者都可能把旧日志留住。checkpoint 只给出一个候选边界，不自动覆盖所有依赖。

第四个误导是把 checkpoint 当成纯后台任务。它虽然常在后台做，但会和前台请求争 I/O、锁、缓存和 CPU。checkpoint 写出太猛，fsync p99 会变坏；写得太慢，下一次 checkpoint 又追上来，系统进入持续还债状态。

更稳的一句话是：checkpoint 是系统在某个日志位置记录并持久化一份可恢复状态，使崩溃恢复可以从这个位置附近开始；它控制恢复成本和日志保留，但要靠 WAL、manifest、fsync 和保留策略共同成立。

## Q003. checkpoint 最常见的生产事故触发条件是什么？

最常见的是 checkpoint storm。写入高峰、bulk load、索引重建、compaction 或大批量任务让 dirty page 快速增加，系统被迫频繁 checkpoint。表面症状是前台 p99 变坏，fsync 延迟上升，磁盘 util 打满，WAL 生成量也跟着增加。很多人只看 QPS 下降，看不到后台 checkpoint 在集中写脏页。

第二类事故是 checkpoint 太稀疏。为了降低写 I/O，把 checkpoint interval 或 WAL 上限调得很大，平时性能看着好，重启恢复却要 replay 很久。更糟的是 WAL 保留增长，归档或复制稍微落后就把磁盘撑满。这个问题经常在“压测只看运行期吞吐，不测崩溃恢复时间”的系统里出现。

第三类是 checkpoint 完成标记先于真实持久化。系统写出 snapshot 或 data files 后，manifest 提前发布；或者没有 fsync 文件和目录；或者 checkpoint record 记录的 LSN 比实际落盘状态更靠后。崩溃后恢复程序相信了一个太新的 checkpoint，跳过必要 WAL，状态就会丢修改。

第四类是旧日志回收太激进。checkpoint 后清理旧 WAL 或旧 segment，却没考虑 standby、archive、PITR、replication slot、慢消费者、reader snapshot。等副本追赶或做时间点恢复时，发现需要的日志已经没了。checkpoint 和 retention 如果没有同一套安全水位，事故会很隐蔽。

第五类是 checkpoint 文件本身缺少校验和版本。状态快照半写、压缩损坏、manifest 指向不存在文件、schema 升级后老 checkpoint 读不懂，重启时才暴露。稳妥实现要有 magic、version、length、checksum、LSN、generation，并且能在坏 checkpoint 时回退到上一个安全点。

结合 LogServe，最容易被问的是：如果 checkpoint cache 坏了怎么办？我会回答：checkpoint 只是加速恢复，不是唯一事实来源；shared log 仍然要能从最近可信 LSN 继续 replay。坏 checkpoint 应该被校验拒绝，再回退或全量重建，而不是把坏快照当成真实状态。

## Q004. checkpoint 的指标应该怎么设计才不会只看平均值？

checkpoint 指标不能只看平均耗时。真正要看的是写出量、尾延迟、恢复距离、WAL 保留和对前台的干扰。

第一组是 checkpoint 生命周期。记录 checkpoint started、completed、failed、forced、skipped；区分定时触发、WAL 大小触发、手动触发、恢复 restartpoint。耗时要用 histogram 看 p50、p95、p99、max。失败要按 EIO、ENOSPC、超时、manifest 发布失败、校验失败分类。

第二组是写出和同步成本。包括 dirty bytes written、pages written、files touched、fsync 次数和延迟、目录 fsync 延迟、checkpoint write throughput、checkpoint flush after 或类似平滑写出指标。只看完成时间，看不出它是否把前台 I/O 打穿。

第三组是恢复距离。要看 last checkpoint LSN、current durable LSN、redo distance bytes、estimated recovery time、replay bytes since checkpoint。这个距离持续增长，说明 checkpoint 没有及时降低恢复成本；距离太小但前台 p99 很差，可能是 checkpoint 太频繁。

第四组是日志保留。oldest required LSN、retained WAL bytes、archive lag、replica lag、consumer lag、checkpoint safe LSN。checkpoint 完成不等于 WAL 可删，指标必须能看到哪个依赖卡住了回收。

第五组是前台影响。checkpoint 期间请求 p99、fsync p99、write stall 时间、dirty throttling、block device queue depth、cache hit 下降、compaction/backup 同期事件。checkpoint 不是孤立后台数字，它最终要解释用户请求为什么慢。

结合 LogServe，我会展示 checkpoint generation、checkpoint LSN、snapshot write p99、manifest publish p99、replay distance、recovery used checkpoint/ignored checkpoint 次数、回退次数。面试里可以说：checkpoint 的指标要回答“它有没有缩短恢复”“有没有拖慢前台”“有没有安全释放日志”，平均耗时回答不了这三个问题。

## Q005. checkpoint 的正确性边界和性能边界分别是什么？

checkpoint 的正确性边界是恢复起点。它只能声明：某个 LSN 或 offset 之前的状态，已经以某种形式持久化，恢复可以从这里附近继续。这个声明必须能被校验：checkpoint 文件完整，manifest 指向的文件存在，文件内容 checksum 通过，记录的 LSN 不超过真正 durable 的日志位置。

它不能替代 WAL。checkpoint 之后仍然可能有新修改，需要从后续 WAL replay；checkpoint 之前也可能因为快照读者、复制或归档要求而不能马上删日志。恢复程序的权威链条通常是：找到可信 checkpoint，验证它，再扫描后续有效日志前缀。任何一步失败，都要能回退。

它也不能自动解决业务一致性。多个状态文件、索引文件、对象引用、materialized view、actor mailbox，如果一起构成 checkpoint，就要有 manifest 或事务性发布协议。只把几个文件分别写出去，不等于同一个时间点的状态一致。

性能边界来自后台写出和恢复距离的取舍。checkpoint 频繁，运行期 I/O 放大，full-page logging 或快照写出增加；checkpoint 稀疏，恢复时间和日志保留增加。平滑写出可以保护前台 p99，但拉长 checkpoint 也会让需要保留的 WAL 更多。

优化不能只说“调大 interval”。要结合恢复目标、磁盘容量、写入速率、归档速度、前台 SLO 和故障演练。可以做增量 checkpoint、copy-on-write manifest、后台限速、分层 checkpoint、按 shard 分片、冷热数据分开。前提是 checkpoint LSN 和持久化事实一致。

结合 LogServe，正确边界是：checkpoint cache 可以减少 actor/workflow/materialized view 的 replay 成本；shared log 仍然定义事实顺序。性能边界是：快照写太频繁会抢 I/O，写太少重启会慢。这个边界讲清楚，就不会把 checkpoint 夸成备份系统。

## Q006. 面试官如果只问一个问题检验你是否理解 group commit，可能会问什么？

我会问：10 个事务几乎同时提交，系统只做了一次 WAL fsync。你怎么保证这 10 个事务都没有被提前 ack？如果 fsync 失败，谁应该收到失败？

这个问题比“group commit 是批量提交”更准。group commit 的核心是摊薄同步成本，而不是偷换提交语义。多个事务可以把 commit record 写入 WAL buffer，然后排队等待同一个 flush leader。leader 执行一次 fsync 或等价同步，把这一批事务的 commit record 都推进到 durable LSN。只有 commit record 被 durable LSN 覆盖的事务，才能收到 durable success。

关键是水位。每个等待者都要知道自己的 commit end LSN。flush 完成后，系统推进 durable LSN；所有 `commit_lsn <= durable_lsn` 的等待者可以成功返回；超过这个水位的请求继续等下一轮。不能因为自己“加入了 batch”就返回，也不能因为 leader 开始 fsync 就返回。

错误传播也很重要。如果这次 fsync 返回 EIO 或 ENOSPC，不能只让 leader 失败。所有被这次 flush 覆盖、正在等待 durable commit 的事务都必须失败或进入不确定状态处理。否则客户端会以为事务已持久，恢复时却找不到对应 commit record。

group commit 还要讲等待窗口。窗口太小，batch size 太小，fsync 次数降不下来；窗口太大，单个事务提交延迟变坏。PostgreSQL 的 `commit_delay` 就是类似思路：leader 在合适条件下短暂等待，让其他提交者进入同一批。但它只在有并发提交、提交速率受同步限制时有意义。

结合 LogServe，如果 shared log 提供 always、batch、interval 三种 fsync 策略，我会把 group commit 放在 durable batch 里：请求进入等待队列，batch fsync 成功后按 durable offset 唤醒。它不同于 interval async；前者仍然等持久化，后者可能先 ack 再后台同步。这个区别必须说清。

## Q007. group commit 的一句话定义是否容易误导，误导点在哪里？

常见定义是“group commit 把多个事务合并成一次提交”。这句话有几个坑。

第一个误导是把逻辑提交合并成一个事务。group commit 通常不会改变事务之间的隔离、冲突检测和 commit record。每个事务仍然有自己的提交结果，只是在物理 WAL flush 上共享一次同步成本。它不是把 10 个事务变成一个大事务。

第二个误导是把 group commit 当成异步提交。真正的 durable group commit 仍然要等本批次 WAL 同步成功后再 ack。异步提交可以更快，但它改变了承诺：客户端可能收到成功后，崩溃仍丢失最近窗口。两者都能提升吞吐，语义完全不同。

第三个误导是忽略队列边界。一个事务加入哪个 batch，取决于它的 commit LSN、进入等待队列的时间、当前 flush 水位和下一轮 flush 安排。实现上要防止后来的事务错误搭上已经开始或已经完成的 flush，也要防止 durable LSN 推进过头。

第四个误导是只看吞吐，不看尾延迟。group commit 会用等待时间换 fsync 合并。并发足够时收益明显；并发低或存储同步很快时，额外等待可能只会增加 p99。参数不应该靠直觉，要用真实 fsync 成本和工作负载验证。

更准确的一句话是：group commit 让多个已经到达提交点的事务共享一次 WAL 持久化操作，用小的排队等待换更少的 fsync；它不改变每个事务的提交语义，也不能在 durable flush 前提前返回。

## Q008. group commit 最常见的生产事故触发条件是什么？

最常见的是 batch 边界写错。某个请求以为自己在本轮 flush 中，其实 commit record 还没写入 WAL buffer，或者 LSN 大于本轮 flush 目标；系统却提前唤醒它。崩溃后这条记录不在 durable 前缀里，客户端已经收到成功。这是 group commit 里最严重的正确性 bug。

第二类是 fsync 失败没有广播。leader 线程感知到同步失败，但等待队列里的 follower 仍然按成功返回。或者部分等待者成功、部分失败，却没有清楚的规则。正确做法通常是让本轮覆盖范围内的等待者都看到同一个同步结果，再由上层决定重试、只读、panic 或关闭服务。

第三类是等待窗口调得过大。为了追求吞吐，把 batch delay 设置得很长，低并发时每个请求都白等；高并发时队列积压，p99 变差。吞吐曲线看起来更高，但用户感受到的是提交延迟上升。group commit 的收益点很窄，不能只靠平均吞吐调参。

第四类是锁设计不好。提交路径持有全局锁等待 fsync，或者 flush leader 在持有 page/manifest/segment 锁时睡眠，其他事务连写 WAL buffer 都被堵住。group commit 本来是减少同步成本，结果变成扩大锁竞争。

第五类是和复制语义混用。本地 group commit 成功，只说明本地 WAL durable。同步复制或 quorum commit 还要等副本确认。如果接口承诺多数派持久，却只等本机 group commit，failover 时会丢已经 ack 的事务。

第六类是指标缺失。只有总提交耗时，没有 batch size、batch wait、fsync latency、waiting requests、durable LSN lag。出问题时分不清是 fsync 慢、batch delay 太大、队列太长，还是等待者被错误唤醒。

面试里我会说：group commit 的事故多半不是“没批起来”，而是“批的边界和 ack 的边界没对齐”。吞吐优化一旦越过 durable LSN，就变成可靠性 bug。

## Q009. group commit 的指标应该怎么设计才不会只看平均值？

group commit 指标要拆成等待、同步、批大小和水位推进。只看平均 commit latency，会把真正的问题揉在一起。

第一组是 batch 形态。每次 flush 覆盖多少事务、多少 bytes、多少 WAL record、最大和最小 commit LSN、等待者数量。batch size 要看分布：p50、p95、p99、max。平均 batch size 可能还行，但低峰时很多 batch 只有一个请求，说明 delay 没带来收益。

第二组是延迟拆解。commit latency 应该拆成排队等待时间、batch delay、WAL write time、fsync time、唤醒时间、复制等待时间。每一项都要 histogram。p99 坏时，能立刻知道是等待策略、存储同步，还是下游复制。

第三组是 fsync 和 durable 水位。记录 fsync 次数、fsync p99、fsync error、durable LSN 推进量、commit LSN 到 durable LSN 的 lag、已进入等待但未 durable 的事务数。这个窗口越大，崩溃风险和排队风险都越高。

第四组是错误传播。fsync 失败影响了多少等待者，失败后 durable LSN 是否停止推进，是否进入只读，是否触发重试。没有这些指标，系统可能在错误时继续返回成功。

第五组是收益指标。writes per fsync、transactions per fsync、bytes per fsync、吞吐提升、p99 增加量。group commit 不是免费优化，指标要同时展示省了多少同步，也展示多等了多久。

结合 LogServe，可以看 batch append size、batch fsync p99、waiting appenders、durable offset lag、fsync error fanout、records per fsync。面试里可以说：group commit 指标的核心是“每次 fsync 保护了多少请求，以及这些请求等了多久”。平均提交耗时只是一层薄皮。

## Q010. group commit 的正确性边界和性能边界分别是什么？

group commit 的正确性边界是 durable flush 覆盖范围。每个请求都有自己的 commit LSN 或 log offset；只有同步成功并且 durable 水位越过它，才能返回 durable success。实现上要保证 commit record 已经写入、flush target 覆盖它、fsync 成功、错误传播到所有等待者。

它不改变事务隔离，也不改变复制级别。单机 group commit 成功，不等于副本持久；多个事务共享 fsync，不等于它们共享锁、共享回滚或共享业务原子性。把这些层混在一起，会让 failover 和并发语义变得不可解释。

性能边界来自等待窗口和同步成本的平衡。存储 fsync 很慢、并发提交很多时，group commit 能明显摊薄成本；fsync 很快、并发很低时，额外等待可能只会伤害延迟。窗口过小批不起来，窗口过大 p99 变坏。

还有一个边界是锁和队列实现。好的实现让事务快速写入 WAL buffer，然后在等待队列里睡眠；flush leader 做同步并推进水位。坏实现把太多工作放在全局锁里，或者让一个慢 fsync 阻塞新 record 插入，最终吞吐和延迟都变差。

优化可以包括自适应 batch delay、按 durable LSN 批量唤醒、多个 log shard、预分配 segment、减少锁持有时间、分离本地 durable 和复制等待。不能优化掉最关键的判定：ack 不能早于本请求所在位置的 durable 事实。

结合 LogServe，group commit 适合解释 batch fsync 策略：它让多个 append 共享一次持久化，但仍然等待持久化成功。interval 模式如果先返回成功，应单独标注为 async durability。面试官听到这个区分，通常会继续问故障窗口，而不是怀疑你在偷换概念。

## Q011. 面试官如果只问一个问题检验你是否理解 segment compaction，可能会问什么？

我会问：一个 append-only KV 日志里，同一个 key 写了 10 次，又删除了 1 次。compaction 时你能不能只保留最后的 delete？什么时候连 delete 也能删？

这个问题直接触到 compaction 的语义边界。segment compaction 不是简单“压缩文件”，而是读取旧 segment，按规则判断哪些 record 仍然可见，写出新的 segment 或 SSTable，再用 manifest 或版本集合原子发布。对同一个 key，旧 value 通常可以被新 value 覆盖；delete marker 也就是 tombstone，要保留到所有可能看到旧 value 的读者、快照、复制、备份和下游消费者都越过安全点。

如果系统只有最新值查询，没有快照、没有外部消费者、没有复制延迟，tombstone 在确认所有更老 value 都被清掉后可以删除。现实里很少这么轻松。只要还有 reader snapshot 可能读旧版本，或者远端副本还没追上，或者下游 CDC 需要看到 delete，tombstone 就不能提前丢。

好的回答还会说发布协议。compaction 不能原地改旧 segment。稳妥做法是：选择输入 segment，读取并过滤，写新文件，校验新文件，fsync 新文件和索引，发布 manifest，fsync manifest/目录，等没有 reader 引用旧版本后再删除旧文件。这样崩溃时要么看到旧版本，要么看到新版本，不应该看到半个 compaction 结果。

还要讲为什么它会影响前台。compaction 会读旧文件、写新文件、更新索引、污染 cache、占用 CPU 和 I/O。LSM 和 append-only KV 把写入变成顺序追加，但把整理成本推到后台。后台跟不上时，会出现 pending compaction bytes 上升、L0 文件堆积、读放大增加，最后触发写入减速或停止。

结合 LogServe，segment compaction 可以用于清理旧 record、旧 checkpoint、旧 materialized view 事件或过期 result metadata。回答要谨慎：如果 shared log 是审计源或恢复源，不能随便 compaction 掉历史；只有可派生、可覆盖、过了保留边界的数据，才适合整理。

## Q012. segment compaction 的一句话定义是否容易误导，误导点在哪里？

常见定义是“segment compaction 是把旧 segment 合并，删除无效数据”。这句话方向对，但太粗。

第一个误导是把 compaction 当压缩。压缩是编码层减少 bytes，比如 zstd、snappy；compaction 是语义层重写数据，丢掉被覆盖版本、过期 tombstone 或无效 record。两者可以同时发生，但不是一回事。

第二个误导是把“无效数据”说得太随意。某条 record 对最新读无效，不代表对旧快照、慢消费者、审计、PITR、复制、CDC 也无效。compaction 的过滤规则必须和可见性规则、保留策略、快照生命周期一致。错删比不删严重得多。

第三个误导是忽略 manifest。compaction 输出新文件以后，要让 reader 从旧文件集合切到新文件集合。这个切换通常靠 manifest、version edit、CURRENT 指针或元数据事务。只把新文件写到目录里，不等于系统已经使用它；扫描目录里所有文件也可能把旧输出、临时文件和新版本混在一起。

第四个误导是以为 compaction 总能降低空间。compaction 运行期间会同时保留旧输入和新输出，瞬时空间可能更大。leveled compaction 还会带来写放大；tiered/universal compaction 写放大低一些，但读放大和空间放大可能高。没有一种策略对所有 workload 都占优。

更准确的一句话是：segment compaction 是按可见性和保留规则重写不可变数据段，用新文件集合替换旧文件集合，以减少旧版本、tombstone 或碎片；它依赖 manifest 原子发布，并用后台 I/O 换查询和空间治理。

## Q013. segment compaction 最常见的生产事故触发条件是什么？

最常见的是 tombstone 删早了。某个 key 被删除后，compaction 看到“当前没有 value”，就把 delete marker 也丢掉；更老层或更老 segment 里还残留旧 value。后续查询或恢复时，旧 value 被复活。这个事故在 LSM、append-only KV、CDC 和跨副本系统里都很典型。

第二类是 snapshot 和 reader 引用没处理。compaction 发布新文件后马上删除旧 segment，但仍有 iterator、reader、备份任务或复制线程拿着旧版本。轻则读失败，重则进程崩溃或返回不一致结果。成熟系统会用版本引用计数、epoch、hazard pointer 或延迟删除。

第三类是 manifest 半发布。新 segment 写完了，旧 segment 还在；manifest 写一半崩溃；CURRENT 指向新 manifest 但新文件没 fsync；目录里留下 `.tmp`、`.compacting`、`.deleted`。重启时如果只扫目录，不按 manifest 判断有效集合，就可能把新旧文件同时加载，出现重复 record 或旧数据复活。

第四类是 compaction 跟不上写入。L0 文件越来越多，pending compaction bytes 上升，读路径要查更多文件，写入开始 stall。RocksDB 这类系统会在 memtable 堆积、L0 文件过多或 pending compaction bytes 太大时减速甚至停止写入。这个保护难受，但比让系统无限积债更好。

第五类是后台 I/O 抢前台。compaction 读写大量文件，把 page cache 和块设备队列打满。业务 p99 变差，排查时却只看到“CPU 不高”。尤其是大范围 compaction、冷数据扫描、压缩重写和 checkpoint 同时发生时，尾延迟会很难看。

第六类是过滤规则和版本升级不兼容。老 record 没有新字段，compaction 代码按新 schema 判断过期；或者 merge operator、TTL、delete range 的语义变了，后台把数据整理成前台读不懂的形态。compaction 是写数据的后台程序，必须按生产写路径一样严肃对待。

面试里可以总结：compaction 的事故通常来自两个方向，一个是删错，另一个是来不及删。前者破坏正确性，后者拖垮性能。

## Q014. segment compaction 的指标应该怎么设计才不会只看平均值？

segment compaction 指标不能只看平均 compaction time。要看输入、输出、丢弃、积压、写放大、读放大和前台影响。

第一组是 compaction backlog。pending compaction bytes、L0 file count、待整理 segment 数、每层 size/score、compaction queue length、最老未 compact segment age。backlog 的趋势比单次耗时更重要；它持续上升，说明后台处理能力落后于写入。

第二组是 I/O 和放大。compaction read bytes、write bytes、dropped bytes、key input、key dropped、tombstone kept/dropped、write amplification、space amplification、read amplification。没有这些指标，就不知道 compaction 是真的清掉垃圾，还是一直重写活数据。

第三组是延迟分布。compaction duration、file publish latency、manifest fsync latency、old file deletion lag，都要看 p95、p99、max。某次大 compaction 卡住，平均时间不会提醒你；读写请求却会被它拖住。

第四组是前台影响。write stall count/time、slowdown time、foreground write latency during compaction、read p99 during compaction、block cache hit drop、device await/queue depth。compaction 本来是后台任务，但用户只感受到前台慢。

第五组是正确性和恢复。manifest publish failures、compaction output checksum failures、reader old-version references、stale file deletion failures、recovery ignored temp outputs、compaction rollback count。没有这些指标，半发布和坏输出只能等重启时发现。

结合 LogServe，我会看 compacted segment input/output bytes、live record ratio、tombstone retained count、old reader reference count、manifest generation、pending compaction bytes、compaction 对 append/fsync p99 的影响。面试里可以说：compaction 指标要能回答“积债多少、清掉多少、放大多少、拖慢谁、有没有发布成功”。平均耗时太粗。

## Q015. segment compaction 的正确性边界和性能边界分别是什么？

segment compaction 的正确性边界是可见性。它只能删除对所有需要的读者都不可见的数据。这里的读者不只是当前 point lookup，还包括快照读、range scan、复制、备份、CDC、审计、PITR 和恢复流程。任何一个依赖还需要旧版本，旧 record 或 tombstone 就不能删。

第二个边界是发布原子性。compaction 输出应该是新文件集合加新 manifest 的切换，不应该原地改旧文件。崩溃后，系统要么继续使用旧集合，要么使用完整新集合。临时文件、半写输出、未 fsync manifest 都不能被当成有效状态。

第三个边界是索引和数据一致。新 segment 通常伴随 sparse index、Bloom filter、metadata、checksum。data 写完但 index 没写完，或者 manifest 指向了缺 index 的文件，reader 就会出错。索引可以可派生，但发布时必须知道哪些索引是权威、哪些可以重建。

性能边界来自写放大、读放大和空间放大之间的取舍。leveled compaction 空间放大较低，但会重写下层数据；tiered/universal 写放大较低，但读时可能要查更多 sorted run，瞬时空间也可能更高。segment compaction 没有免费 lunch，只是在不同成本之间移动。

还有后台资源边界。compaction 消耗 CPU、I/O、cache 和线程池。限速太强，积压变多，读写都会被拖慢；限速太弱，前台 p99 被打穿。合适策略要按写入速率、热点分布、设备能力、恢复目标和保留要求调。

结合 LogServe，segment compaction 的正确边界是 shared log 的事实不能被错误折叠；可重建索引、过期中间态、旧 checkpoint、已安全覆盖的 materialized view 可以整理。性能边界是整理越积极，后台 I/O 越重；整理太慢，恢复和查询成本上升。

## Q016. 面试官如果只问一个问题检验你是否理解 sparse index，可能会问什么？

我会问：一个 SSTable 按 key 排序，但 index 里不是每个 key 都有 entry。现在查 key = `user:10086`，你怎么定位？如果 index entry 指向的是某个 block 的最大 key，而不是最小 key，会影响什么？

这个问题能测出 sparse index 的核心：它不是直接给出每条记录的位置，而是把查询带到一个较小范围，然后在范围内继续二分或顺序扫描。LevelDB 的 table format 里，data blocks 按 key 排序，index block 一般是一项对应一个 data block，value 是 data block 的 offset/size。索引 key 选在 block 边界附近，用来判断目标 key 可能落在哪个 block。

回答时我会先说查找流程。内存里可能先查 memtable 和 Bloom filter；对某个 SSTable，先查 index block，找到第一个边界 key 大于等于目标 key 的 data block；再读取这个 data block，在 block 内二分或按 restart point 查找；最后比较真实 key。没找到，才能说这个文件里没有，不能只凭 sparse index 没有精确 entry 就返回不存在。

最重要的不变量是排序。sparse index 能工作，是因为底层数据文件有顺序：按 offset、按 key、按 timestamp 或按其他单调维度。没有顺序，稀疏 entry 只能告诉你几个采样点，无法安全缩小搜索范围。append-only offset index、SSTable block index、时间序列 segment index 都是这个思路。

还要提 index key 的语义。它可以是 block first key、last key、separator key、最大 key 或某种上界。只要写入端和读取端一致，并且比较规则和 data block 顺序一致，都能工作。最怕的是自定义 comparator、prefix transform、编码变化后，index 的边界顺序和 data 的真实顺序不一致。

结合 LogServe，如果 segment 按 offset 追加，可以每隔 N bytes 或 N records 建 sparse offset index。查某个 offset 时先找到不大于目标 offset 的最近 entry，再从该物理位置顺序扫描到目标 record。它降低索引大小，但读路径要接受一段扫描成本。

## Q017. sparse index 的一句话定义是否容易误导，误导点在哪里？

常见定义是“sparse index 不是每条记录都有索引”。这句话对，但只说了表面。

第一个误导是忽略数据必须有顺序。稀疏索引依赖底层文件按某个维度排序或单调增长。没有这个前提，缺失的 index entry 无法通过相邻 entry 推断位置。对无序 key-value 日志，按 key 做 sparse index 通常不够；对按 offset 追加的日志，按 offset 做 sparse index 就很自然。

第二个误导是把 sparse index 当成不精确索引。它不是 probabilistic structure。Bloom filter 可能有 false positive；sparse index 的定位应该是确定的，只是定位到范围而不是单条 record。范围内扫描后，结果仍然要精确比较真实 key 或 offset。

第三个误导是把 index 粒度当固定值。每个 data block 一个 index entry、每 4KB 一个 entry、每 4096 条 record 一个 entry、每 1MB 一个 entry，都是选择。粒度越稀，索引越小，读放大越大；粒度越密，点查更快，内存和更新成本更高。

第四个误导是把它和 cache 混淆。index entry 是定位结构，cache 是保存读过的数据或 block。index block 可以被缓存，data block 也可以被缓存，但命中 cache 不代表索引变稠密。排查时要分清 index miss、block cache miss、Bloom filter false positive 和 data block scan。

更准确的一句话是：sparse index 是建立在有序数据文件上的范围定位结构，它只为部分边界记录位置，查询时先缩小到一个 block 或区间，再读取原始数据做精确判断。

## Q018. sparse index 最常见的生产事故触发条件是什么？

最常见的是边界 key 规则不一致。写入端用 block last key 做 index，读取端按 first key 理解；写入端用了 separator key，读取端按真实最大 key 比较；自定义 comparator 升级后，旧 index 的顺序不再符合新比较规则。结果是查找跳到错误 block，出现假 miss 或读放大异常。

第二类是 block 范围和 offset 不一致。compaction 重写 data block 后，index 仍指向旧 offset；segment truncate 后，sparse index 里还有越界位置；压缩算法改变后，index 记录的是未压缩 offset，读取端按压缩文件 offset 用。索引如果可派生，恢复时要能重建；如果持久化，就要和 data 一起发布。

第三类是粒度选错。entry 太稀，点查要扫很长一段，p99 变差；entry 太密，index block 变大，占内存、污染 cache，compaction 或 segment rollover 时索引重写成本上升。很多系统一开始用小数据集调参，线上数据量放大后才暴露。

第四类是没有处理重复 key 和 tombstone。LSM 中同一个 key 可能出现在多个 level 或多个 file；新版本在上层，旧版本在下层。sparse index 只能定位到某个文件的某个范围，不负责跨文件版本裁决。读路径还要按 level、新旧序列号、snapshot 和 tombstone 规则判断。

第五类是和 Bloom filter 的关系说不清。Bloom filter 返回“可能存在”后，仍要查 sparse index 和 data block；Bloom filter 返回“不存在”才可以跳过这个文件。Bloom filter 错配、prefix bloom 用错、filter 没覆盖某些 block，会让 sparse index 被过度访问或错误跳过。

结合 LogServe，offset sparse index 最危险的是崩溃后 index 指向半条 record 或坏尾部。恢复时应该以 data record 的 length/CRC/commit 规则为准，截断坏尾巴，再丢弃或重建越界 index，而不是让 index 决定日志有效前缀。

## Q019. sparse index 的指标应该怎么设计才不会只看平均值？

sparse index 指标要围绕“定位后还要扫多少”设计。平均查找时间太粗，会把少数大范围扫描和缓存失效藏起来。

第一组是定位质量。记录每次查询命中的 index entry 到目标 record 的扫描 bytes、扫描 records、读取 data block 数、block 内比较次数。看 p50、p95、p99、max。稀疏索引的核心成本就在这里。

第二组是索引大小和缓存。index bytes per segment、index entries count、index block cache hit/miss、data block cache hit/miss、Bloom filter 命中和 false positive 估计。要分文件类型、level、segment age、tenant 或 workload，不要只看全局命中率。

第三组是错误和恢复。index checksum failure、index offset out of range、index/data mismatch、index rebuild count、rebuild duration、recovered truncated index entries、unsupported index version。可派生索引也要有指标，否则恢复时间会被低估。

第四组是粒度效果。按 index interval 分桶，看 point lookup p99、range scan throughput、index memory、compaction index rewrite bytes。如果改了 index 粒度，只看平均读延迟，很可能看不出内存和 p99 的代价。

第五组是跨结构联动。Bloom filter 让多少文件被跳过，sparse index 让多少 data block 被读取，range scan 是否顺序预读，compaction 后 index 是否重新加载。读路径由多个结构组成，单独看 sparse index 命中率意义有限。

结合 LogServe，我会看每个 segment 的 sparse index entry 数、平均/尾部扫描 bytes、offset lookup p99、index rebuild time、坏尾部截断导致的 index entry 丢弃数。面试里可以说：sparse index 的好坏不是“有没有命中”，而是“命中后还要付多少扫描成本”。

## Q020. sparse index 的正确性边界和性能边界分别是什么？

sparse index 的正确性边界是有序范围定位。它只在底层数据按同一 comparator 或单调维度组织时成立。index entry 的 key、offset、size、block handle、压缩状态、版本和 comparator 都要和数据文件一致。只要定位到范围，最终结果仍然必须从原始 record 或 data block 精确判断。

它不负责版本裁决。LSM 的新旧版本、tombstone、snapshot sequence、range delete、merge operand，都不是 sparse index 自己能决定的。它最多告诉你去哪里找候选记录；候选记录是否对当前读可见，要由上层可见性规则判断。

它也不应该成为唯一恢复依据。崩溃后 index 文件可能比 data 旧，也可能指向已经截断的位置。稳妥策略是让 data/log/manifest 更权威，index 可以校验、丢弃、重建。对必须持久化的 index，要和 data 一起走原子发布协议。

性能边界来自索引粒度。entry 越少，索引更小、缓存更友好、写入和 compaction 成本更低，但点查和小范围查要扫描更多；entry 越多，定位更准，但内存、磁盘、重建和缓存压力上升。这个取舍没有固定答案。

另一个边界是 workload。顺序 range read 对 sparse index 友好，因为定位一次后可以顺序扫；随机 point lookup 对粒度敏感；小 value、高 QPS、低延迟场景可能需要 Bloom filter、partitioned index、prefix index 或 dense index 辅助。不要把 sparse index 当成所有查询的唯一结构。

结合 LogServe，offset sparse index 的正确边界是辅助 reader 快速接近目标 offset；record 完整性仍靠 length、CRC 和恢复规则。性能边界是 entry 间隔越大，查找时扫描越多；entry 越密，segment 索引更大，启动加载和 compaction 重写更贵。

## Q021. 面试官如果只问一个问题检验你是否理解 LSM tree，可能会问什么？

我会问：LSM tree 为什么写入快？代价被转移到了哪里？如果 compaction 跟不上，会先从哪些现象暴露出来？

这个问题比“LSM 是 Log-Structured Merge Tree”有效。LSM 的基本思路是把随机写先变成内存结构和顺序写：写请求进入 WAL，更新 memtable；memtable 满了以后 flush 成不可变 SSTable；后台再把多个 sorted run 合并到更低层。前台写入少做随机落盘，写吞吐就上来了。

代价没有消失，只是换了地方。读路径可能要查 memtable、immutable memtable、L0 多个文件、L1/L2 更低层文件，还要依赖 Bloom filter、block cache、sparse index 降低读放大。后台 compaction 会持续读旧 SST、写新 SST、丢掉旧版本和 tombstone，还要更新 manifest。LSM 用顺序写换取后台整理成本，这是它的核心取舍。

面试里要主动讲三种 amplification：write amplification、read amplification、space amplification。leveled compaction 通常控制空间放大和读放大较好，但会重写下层数据，写放大更明显；tiered 或 universal compaction 写放大低一些，但读时可能面对更多 run，空间放大也可能更高。不同策略是在三者之间调权重。

如果 compaction 跟不上，症状会很具体：L0 文件数增加，pending compaction bytes 上升，读路径要查更多文件，Bloom filter 和 block cache 压力变大，写入开始 slowdown 或 stall，磁盘空间被旧版本和 tombstone 占住，p99 读写延迟同时变差。RocksDB 这类系统会在 memtable、L0 文件或 pending compaction bytes 超阈值时主动减速，避免继续把债滚大。

还要讲正确性边界。LSM 的文件通常不可变，靠 sequence number、level 顺序、snapshot 和 tombstone 决定哪个版本可见。compaction 不能随便丢旧值或 delete marker；manifest 发布要原子；旧文件要等 reader 不再引用后才能删除。否则写入再快，也会出现旧值复活或读到不一致版本。

结合 LogServe，如果被问为什么了解 LSM，对项目的连接点是：shared log、segment、sparse index、checkpoint、compaction 这些机制都能在 LSM 里看到影子。LogServe 不需要自称实现了生产级 LSM；更稳的说法是，项目用 append-only log 和可重放状态展示了同一类思想：前台顺序写，后台整理和快照控制恢复成本。面试官通常更看重你能不能把收益和债务都讲出来。

## Q022. LSM tree 的一句话定义是否容易误导，误导点在哪里？

容易误导。最常见的一句话是：LSM tree 是一种写优化的数据结构，先写内存，再把有序文件逐层合并。这句话方向没错，但如果面试只停在这里，会把几个真正决定系统行为的点全部遮住。

第一个误导点是把 LSM tree 理解成“树”。它不像 B+Tree 那样主要靠一棵原地更新的页结构维护查找路径。它更像一组按版本组织的有序组件：内存里的 mutable memtable、不可变的 immutable memtable、磁盘上的多个 SSTable、每个文件的 key range、各 level 的布局，以及把这些组件组合成一个可见版本的元数据。读一个 key 的时候，系统不是沿着一条固定树边走到底，而是在“当前版本”里按新到旧、按层级、按文件范围、按 filter 逐步排除和查找。

第二个误导点是把“写优化”理解成写入免费。LSM 把随机写变成顺序追加和后台合并，前台写路径通常可以很短，但代价被推迟到了 flush、compaction、空间放大和读放大上。写入越快，后台越要有能力消化这些新数据；后台消化不了，L0 文件堆积、pending compaction bytes 变大、写停顿就会出现。所谓写优化，是用后台 IO 和额外空间换前台写延迟，不是消灭写成本。

第三个误导点是忽略版本语义。LSM 里同一个 key 可以同时存在多个版本，也可以存在删除标记。用户看到的是某个 sequence number 或 snapshot 下的逻辑结果，底层文件里还可能保留旧值、tombstone、range deletion marker。compaction 不是简单“把文件合并一下”，它要在不破坏快照、不复活旧值、不丢失删除语义的前提下丢弃过期版本。

第四个误导点是忽略 WAL 和 manifest。memtable 还没 flush 成 SSTable 时，真正支撑崩溃恢复的是 WAL；新的 SSTable 能否成为可见状态，依赖版本元数据的安全发布。如果只说“先内存后磁盘”，很容易漏掉崩溃发生在写 WAL、更新 memtable、flush、写 MANIFEST、删除旧文件之间不同位置时，系统应当恢复到哪个状态。

所以我会把 LSM tree 定义得更精确一些：它是一种把更新追加到内存和不可变有序文件中，再通过受控合并维护逻辑最新视图的数据组织方式。它的核心不是“树形外观”，而是版本、顺序文件、后台合并、删除语义和恢复元数据共同构成的读写协议。

## Q023. LSM tree 最常见的生产事故触发条件是什么？

最常见的触发条件不是某个单点算法写错，而是后台维护速度跟不上前台写入速度，最后表现成写停顿、读尾延迟抖动、磁盘暴涨或者恢复时间变长。

最典型的一类是 compaction debt 积累。写入先进入 memtable，flush 后进入 L0。L0 文件通常范围重叠，读路径要检查更多文件；如果 flush 速度大于 compaction 消化速度，L0 文件数、pending compaction bytes、level score 会不断上升。到达阈值以后，系统会从“变慢”进入“强制限速或写停顿”。这种事故在批量导入、热点租户突增、日志型大写入、压缩变慢、磁盘带宽被其他任务抢占时很常见。

第二类是 tombstone 和旧版本处理错误。删除不是立即从所有 SSTable 中抹掉旧值，而是先写删除标记，再等 compaction 证明旧版本已经没有可见路径时才回收。如果 tombstone 被过早丢弃，老 level 里的旧值可能被重新读出来；如果 tombstone 长期无法丢弃，读路径会不断撞到删除标记，空间和 compaction 成本也会越来越高。长事务、长 snapshot、慢副本、CDC 消费滞后，都可能把旧版本钉住。

第三类是版本元数据和文件生命周期出错。LSM 的正确性依赖“当前版本”对 SSTable 集合的引用。一个文件写完但没有安全发布，崩溃后不应被当成有效数据；一个文件还被某个版本、迭代器、snapshot 或 compaction 引用时，也不能被清理。生产事故里常见的是清理线程、备份任务、手工删文件、磁盘告警脚本和引擎自己的 obsolete file 回收逻辑边界不一致，导致要么误删活文件，要么旧文件越积越多。

第四类是读路径配置失衡。Bloom filter、block cache、index/filter block、分层大小、压缩策略如果和 workload 不匹配，平均 QPS 可能看起来还行，但 range scan、miss-heavy lookup、冷热 key 混合、宽 keyspace 查询会把读放大放大到尾延迟上。面试里我会特别强调：LSM 的读性能不是只看“是否有 Bloom filter”，还要看每次读触碰多少个文件、多少个 level、多少次磁盘 IO，以及 filter 误判和 cache miss 在尾部请求上的分布。

第五类是磁盘和恢复边界被忽略。WAL、immutable memtable、未完成 compaction 的输出文件、旧 SSTable、manifest 都可能短时间共存。磁盘使用率接近上限时，LSM 没有足够空间完成“写新文件再切换版本再删旧文件”的过程，可能越压越慢。重启恢复时，如果 WAL 太多、manifest 太大、文件数量太多，恢复时间也会明显拉长。

所以 LSM tree 的生产事故通常不是“写入慢一点”这么简单，而是前台写、后台 compaction、读放大、空间回收、版本发布和崩溃恢复之间的平衡被打破。

## Q024. LSM tree 的指标应该怎么设计才不会只看平均值？

LSM tree 的指标要按组件、层级、阶段和租户拆开看。只看平均写延迟或平均读延迟，往往会把真正的问题藏起来，因为 LSM 最先坏掉的通常是尾部、后台债务和局部层级。

写路径要看 logical write latency 的 p50、p95、p99、max，也要单独看 WAL append、WAL sync、memtable insert、write batch queue、write stall time。平均写延迟很容易被大量小请求稀释，真正影响用户的是 compaction 压力上来以后，某些写请求被限速、排队或等待 immutable memtable flush 的时间。还要看 stall count、stall duration、delayed write count，以及每次停顿的原因是 memtable 太多、L0 文件太多，还是 pending compaction bytes 太大。

后台维护要按 level 看。至少要有每个 level 的文件数、字节数、score、输入输出字节、compaction 次数、compaction duration、compaction throughput、compaction pending bytes、flush duration、flush backlog。L0 要单独列出来，因为 L0 文件范围重叠，对读放大和写停顿最敏感。只看全库 compaction 平均吞吐没有意义：L1 到 L2 很顺，L0 堆积照样会把前台拖住。

读路径要看每次查询触碰的文件数、level 数、block read 数、磁盘 read bytes、Bloom filter checked count、filter useful count、filter false positive、block cache hit/miss、index/filter block 命中率。点查、范围查、前缀查、miss lookup 要分开。很多系统的平均读延迟稳定，是因为大多数热 key 在 cache 里；一旦看 p99 或 miss-heavy 请求，就会发现它们在多个 level 之间反复探测。

空间和版本要看 write amplification、read amplification、space amplification、obsolete file bytes、stale file count、tombstone count、oldest snapshot age、oldest pinned sequence、live SSTable bytes、WAL retained bytes。尤其是 tombstone 和 snapshot：它们可能不直接抬高平均延迟，却会让 compaction 无法丢弃旧版本，最终变成磁盘、水位和恢复问题。

还要按 column family、tenant、partition、key range 或业务表拆分。LSM 的压力经常不是均匀分布的，一个热点租户、一个宽范围扫描、一个 TTL 到期批次，就能让局部 keyspace 的 compaction 和 cache 行为完全不同。指标面板如果只有全局平均值，会得出“系统正常”的错觉。

我会把指标设计成四层：前台请求尾延迟、后台债务、读写放大、版本和空间安全水位。只有这四层同时看，才能判断 LSM tree 是暂时抖动，还是已经进入会持续恶化的状态。

## Q025. LSM tree 的正确性边界和性能边界分别是什么？

LSM tree 的正确性边界主要在“同一个逻辑 key 的哪个版本对哪个读者可见”。只要这个边界说不清，LSM 就很容易从性能优化问题变成数据正确性问题。

正确性上，写入需要先有可恢复的记录，再进入内存结构；memtable flush 出来的 SSTable 必须是不可变、校验完整、范围元数据正确的文件；新的文件集合要通过版本元数据原子发布；旧文件只有在没有任何当前版本、snapshot、迭代器、compaction、备份或复制过程引用时才能删除。删除操作也同样有边界：tombstone 在所有可能遮蔽的旧值都安全不可见之前不能随便丢弃。

快照是另一个核心边界。一个 snapshot 看到的是某个 sequence number 的稳定视图，后续写入不能影响它，compaction 也不能为了节省空间把它仍然需要的旧版本删掉。这意味着 LSM 的正确性不是“磁盘上只有最新值”，而是“每个读者在自己的可见点上看到正确的值”。如果系统还有副本、CDC、备份或增量恢复，还要把它们的安全推进点纳入旧版本回收条件。

比较器、编码和 merge/range delete 语义也属于正确性边界。SSTable 的有序性依赖 comparator；一旦比较器或 key 编码升级不兼容，旧文件和新文件之间的顺序关系可能被破坏。merge operator、range deletion、TTL 这类功能看起来是局部逻辑，实际会改变 compaction 丢弃旧版本的判断条件，必须非常谨慎。

性能边界则是另一回事。LSM 擅长把随机写转成顺序追加，适合写多、更新多、可以接受后台合并成本的场景。但它的性能不是无限扩展的：写入速度不能长期超过 flush 和 compaction 的消化能力；读路径不能长期承受过高的文件探测和 block miss；磁盘不能缺少完成 compaction 所需的临时空间；压缩、校验、filter、cache 都会改变 CPU、IO 和内存之间的成本分配。

不同 compaction 策略的边界也不同。leveled compaction 通常读放大和空间放大较可控，但写放大可能更高；tiered 或 universal compaction 对写更友好，但在某些读和空间场景下代价更明显。没有一种策略对所有 workload 都最优，选择要看点查、范围扫、写入批量、更新覆盖率、删除比例和可接受的尾延迟。

所以我会把边界总结为：正确性由 WAL、sequence number、snapshot、tombstone、manifest 和文件生命周期共同保证；性能由前台写入速率、后台 compaction 能力、读放大、空间放大和缓存命中共同决定。面试里只要能把这两条线分清，就说明不是把 LSM tree 当成一个“写很快的树”在背。
