# 十三、代码级追问清单：Go 实现细节

这一组问题要按代码回答。面试时不要只讲设计图，最好能说出函数名、锁的范围、当前实现的取舍和还没生产化的边界。

## Q886. Store.Append 为什么持有全局 mutex？

`internal/logstore/store.go` 里的 `Store.Append` 持有的是 `Store.mu`，它保护的是整个单机 logstore 的可变状态。这里不是只写一个文件那么简单，append 时会同时修改 `activeSegmentBytes`、`activeSegmentID`、`nextSeq`、`index`、`idempotency`，还可能触发 segment rollover。任何一个状态和实际写入顺序错开，都会影响后续 replay。

全局锁的好处是实现简单，stream 内 seq 分配、segment offset、index entry 和幂等表更新都在一个临界区里完成。对当前单机实验和教学型实现来说，这个选择比较稳。

代价也很明显：多个 stream 并发 append 时会串行化。也就是说，即使 task stream、workflow stream、actor stream 彼此独立，现在仍然会抢同一把锁。生产化时可以改成更细粒度的结构，例如 per-segment 写锁、per-stream seq lock，再配合 group commit。但那会引入更多 crash consistency 和 index 一致性问题，所以当前先用全局锁把语义做扎实。

## Q887. Store.Read 为什么先复制 selected index entries 再释放锁？

`Store.Read` 先在锁内处理 `trimBefore`、扫描内存里的 `s.index[streamID]`，把要读的 `indexEntry` 复制到 `selected`，然后释放锁，再打开 segment 文件读取 record。

这样做是为了缩短锁持有时间。真正的磁盘读可能比较慢，如果读文件时还拿着 `Store.mu`，所有 append、trim、stats 都会被挡住。现在锁只保护内存索引的一次快照，读文件在锁外完成，读写并发会更好一点。

这里的前提是 index entry 指向的 segment 文件在读取期间不会被物理删除。当前系统实现的是 logical trim，没有 physical compaction，所以旧 segment 还在磁盘上，锁外读取是安全的。后面如果做真正的物理 compaction，就要加引用计数、读写代际保护，或者让 compactor 只删除不会被任何活跃 reader 使用的 segment。

## Q888. readIndexedRecordFromFile 为什么还要校验 stream_id、seq、length？

`readIndexedRecordFromFile` 已经拿到了 index entry，但它不会完全相信 index。它会调用 `readRecordAt(file, entry.Offset)` 读出 record，然后检查三个东西：record 的 `StreamID` 是否等于 entry 的 `StreamID`，`Seq` 是否一致，实际 record 长度是否等于 entry 里的 `Length`。

原因是 index file 只是加速结构，不是 source of truth。它可能损坏、落后，或者在崩溃前只写了一半。即使当前启动时会 rewrite index，读路径仍然做二次校验，可以避免 index 指错位置后返回错误 record。

`length` 校验也很重要。假设 offset 正好落在某条完整记录开头，但 index 里的 length 被破坏了，不校验 length 就可能把一条“看起来合法”的记录当成目标记录。这里直接返回 `errCorruptRecord`，比静默读错更好。

## Q889. encodeRecord 为什么使用 big endian？是否必要？

`encodeRecord` 用 `binary.BigEndian` 写 magic、version、各字段长度、seq、timestamp 和 CRC。对单机 Go 程序来说，big endian 不是功能上的硬要求，用 little endian 也能工作。

选择 big endian 的好处是格式更稳定，也更容易人工排查。网络协议和很多二进制文件格式喜欢用 big endian，因为字节序按高位到低位排列，dump 出来时更直观。只要 encode 和 decode 一致，系统就能正常工作。

真正必要的是“明确指定字节序”。不能用机器原生字节序，否则跨平台或未来用别的语言读日志时会出问题。这里用 big endian，相当于把 log record 格式固定下来。

## Q890. recoverSegment 遇到错误后 truncate 到 offset，这个 offset 是如何得到的？

`recoverSegment` 从 offset=0 开始循环调用 `readRecordAt(file, offset)`。每成功读出一条 record，`readRecordAt` 会返回 `nextOffset`，也就是当前 record 结束后的文件位置。恢复逻辑把当前 record 对应的 index entry 加到内存里，然后把 `offset = nextOffset`。

所以，当下一次读取出错时，当前的 `offset` 指向“最后一条已确认完整记录之后的位置”。这就是 truncate 的边界。`file.Truncate(offset)` 会把最后那段不完整或损坏的尾部裁掉，保留前面已经通过 magic、version、长度和 CRC 校验的记录。

这个策略适合处理 partial tail，比如进程崩溃时只写了半条 record。它对 segment 中间损坏更保守：当前实现也会从损坏点 truncate，后面的记录即使物理上完整也会丢掉。生产化时可以把非尾部损坏单独隔离出来，而不是简单截断。

## Q891. appendIndex 只写 JSON line，为什么不使用二进制 index？

当前 `appendIndex` 调用 `writeIndexEntry`，每条 index entry 写成一行 JSON。这样做的优点是调试方便，文件可以直接打开看，测试和恢复也容易写。这个项目的主线是 shared log 的语义和恢复链路，JSON line index 能降低实现复杂度。

缺点是性能和空间都不如二进制 index。JSON 有字段名，编码解码也更慢；如果 record 很多，index 文件会膨胀。更高性能的版本可以把 index entry 做成固定长度二进制结构，比如 stream id hash、seq、segment id、offset、length，再配合 per-stream sparse index。

当前还有一个现实因素：启动恢复会扫描 log 并 rewrite index，所以 index 不是唯一恢复依据。JSON line 的可读性在这个阶段比极致性能更值钱。

## Q892. syncForPolicyLocked 中 interval policy 如何更新 lastSync？

`syncForPolicyLocked` 里，`FsyncInterval` 会检查 `time.Since(s.lastSync) >= s.options.FsyncInterval`。如果达到间隔，就调用 `syncFilesLocked()`。

`lastSync` 不在 `syncForPolicyLocked` 里直接更新，而是在 `syncFilesLocked` 里更新。`syncFilesLocked` 会依次对 `logFile` 和 `indexFile` 调用 `Sync()`，如果没有错误，就把 `s.lastSync = time.Now()`。这样可以保证 `lastSync` 表示最近一次成功 sync 的时间，而不是最近一次尝试 sync 的时间。

这点会影响 durability 语义。interval 策略下，append 返回成功不等于已经刷盘；只有达到 interval 并且 sync 成功后，`lastSync` 才前进。

## Q893. Close(sync=true) 为什么要 sync files？

`Store.Close` 调用 `closeActiveFilesLocked(true)`，也就是关闭前先 sync。这样做是为了让正常关闭路径尽量把 log 和 index 都刷到磁盘，减少重启后的尾部截断概率。

这里的 sync 覆盖 `logFile` 和 `indexFile`。如果是 `FsyncBatch` 或 `FsyncInterval`，平时 append 可能没有每次 sync；Close 时补一次 sync，可以让 graceful shutdown 更接近 durable shutdown。

这不是对 crash 的保证。如果进程被 `kill -9` 或机器断电，Close 根本不会执行。真正的崩溃语义仍然取决于 append 时的 fsync policy。

## Q894. persistRetentionLocked 使用 Rename 是否在所有文件系统上原子？

`persistRetentionLocked` 先写 `retention.json.tmp`，再用 `os.Rename(tmpPath, path)` 覆盖正式文件。这个模式在同一目录、同一文件系统内通常是原子的，POSIX 文件系统一般能保证 rename 后要么看到旧文件，要么看到新文件。

但“所有文件系统”这个说法太满了。跨文件系统 rename 不成立；某些网络文件系统、Windows 特殊场景、挂载参数、崩溃时目录项没有 fsync，都可能影响严格语义。当前代码没有对目录 fd 做 fsync，所以它更像是普通本地文件系统上的 best effort 原子替换。

这对 logical trim metadata 已经够用，因为 trim point 丢失通常只会让 replay 多读一点旧日志，不会直接破坏 log 的 source-of-truth。生产化版本可以补目录 fsync、校验字段和双文件 generation。

## Q895. BootstrapFromLog 中 bootstrapReadLimit=1000 是否可能不够？

单次 `ReadLog` 的 limit 是 1000，但 `readAllLog` 会循环读。它从 `fromSeq=1` 开始，每次读 1000 条，然后把 `fromSeq` 更新为最后一条记录的 `seq + 1`。只要返回数量等于 1000，就继续读；少于 1000 才结束。

所以 `bootstrapReadLimit=1000` 不是总上限，只是分页大小。stream 超过 1000 条也能读完。

它的风险在另一个地方：`readAllLog` 会把整个 stream 累积到内存里的 slice。对于很长的 actor stream、workflow stream 或 llm stream，bootstrap 内存压力会变大。后续可以改成流式 replay，一边读一边 apply event。

## Q896. readAllLog 如何处理超过 limit 的 stream？

`readAllLog` 用分页方式处理。它每轮调用 `ReadLog(streamID, fromSeq, bootstrapReadLimit)`，把返回记录 append 到 `out`，再根据最后一条记录的 seq 推进 `fromSeq`。如果这一轮返回 1000 条，说明可能还有下一页；如果少于 1000 条，就认为读到尾部。

这种实现简单，也能保证单个 stream 内按 seq 顺序 replay。它依赖 `ReadLog` 返回的是从 `fromSeq` 开始的连续记录。logical trim 后，如果 `fromSeq` 小于 trim point，logstore 会把起点抬到 `trimBefore`，所以 bootstrap 读到的是 retention 之后的可见日志。

边界是内存占用。`readAllLog` 返回完整 slice，bootstrapTasks、bootstrapActors、bootstrapLLMStats 都会先拿到一整个 stream 的记录再处理。小规模没问题，大规模要改成 iterator。

## Q897. workflowMu 是全局锁还是 per-workflow lock？有什么影响？

`workflowMu` 是 `Service` 上的一把全局 `sync.Mutex`，不是 per-workflow lock。`scheduleReadySteps`、`markWorkflowStepStarted`、`completeWorkflowStep` 都会拿这把锁。

好处是实现简单，可以避免同一个 workflow 的 step 调度、开始、完成并发交错。比如两个 step 同时完成后触发 ready step 调度，有全局锁就不容易重复调度同一个 step。

影响是所有 workflow 都被串行化了。A workflow 的 step completion 会挡住 B workflow 的调度。当前实验规模不大，这个代价可接受；生产化时应该做成 per-workflow lock，或者用 metadata store 的条件更新来保证单个 workflow 内部一致性。

## Q898. actorLocks 是如何创建和释放的？会不会内存增长？

control 侧的 `actorLocks` 是 `map[string]*sync.Mutex`，通过 `Service.actorLock(actorID)` 懒创建。函数先拿 `actorLocksMu`，查不到就创建一把新的 mutex 放进 map，然后返回这把锁。

当前代码没有释放 actor lock。也就是说，只要某个 actor id 被调用过，它的锁就会留在 map 里。对实验规模没问题，但 actor 数量很大时会慢慢增长。

worker 侧的 `localExecutorPool` 也有一个 `actorLocks` map，用来保证同一 worker 内同一个 actor 的任务串行执行，同样是懒创建，当前也没有清理。生产化版本可以在 actor 删除、passivation 或一段时间无访问后回收；更稳的办法是把 actor mailbox 做成有生命周期的对象，随 actor metadata 一起管理。

## Q899. llmStatsMu 与 configMu 是否可能造成死锁？

从当前代码看，没有看到固定的双锁反向获取路径。`configMu` 主要保护 scheduling policy、backpressure 等配置；`llmStatsMu` 保护 materialized LLM stats。`SetSchedulingPolicy` 写 log 后只拿 `configMu`。`materializedLLMStats`、`llmStatsForWorker`、`materializeLLMCompleted` 只拿 `llmStatsMu`。`getSchedulingPolicy` 只拿 `configMu`。

所以当前实现里这两把锁发生死锁的概率很低，因为没有常见的“先拿 A 再拿 B”和“先拿 B 再拿 A”的交叉路径。

不过以后改调度器时要小心。比如如果在持有 `configMu` 时读取 llm stats，又在 materialize LLM stats 时读取配置，就会形成潜在死锁。更稳的规则是：热路径先复制配置，再读取 stats；不要在持有一把锁时做 RPC、appendLog 或进入另一个复杂模块。

## Q900. control service 中 appendLog 记录 lastLogAppendMs，如果 AppendLog 返回错误也会更新吗？

会。`Service.appendLog` 先记录 `start := time.Now()`，调用 `s.log.AppendLog(ctx, req)`，然后无论 `err` 是否为空，都会计算 elapsedMs 并写入 `s.lastLogAppendMs`，最后返回 `resp, err`。

这样做的含义是：`lastLogAppendMs` 表示最近一次 log append RPC 的耗时，不表示最近一次成功 append 的耗时。失败请求如果卡了很久，也会把这个值推高，backpressure 就能感知到 log service 慢或不可用。

这个选择对保护系统有用，但指标命名要讲清楚。它记录的是 last attempt latency，不等同于 success latency。要更精细的话，可以拆成 `last_log_append_attempt_ms`、`last_log_append_success_ms`、`log_append_error_count`。

## Q901. CompleteTask 中 actor task 路径和普通 task 路径为什么分开？

`CompleteTask` 先查 metadata。如果 existing task 是 actor task，就走 actor 专用路径：先检查终态，再 `ValidateTaskLease`，然后调用 `completeActorCall`，最后才 `meta.CompleteTask`。

actor task 需要额外处理 actor state、command_seq、owner epoch 和 ActorCommandApplied/Failed 事件。普通 task 只要完成 task 状态即可；workflow task 还要推进 workflow step；LLM task 可能要 materialize LLM stats。actor 的状态提交要先经过 actor runtime 的顺序和 fencing 校验，所以不能直接复用普通 task 完成逻辑。

分开写还有一个好处：actor command 的“状态变更”与 task 的“执行完成”不是同一件事。actor command 应用成功后，actor 内存状态和 actor stream 才是核心；task metadata 只是这次调用的执行记录。

## Q902. taskTerminalEventApplies 为什么 terminal status 后忽略后续 terminal event？

`taskTerminalEventApplies` 一开始就检查 `isTerminalTaskStatus(status)`，如果当前状态已经是 SUCCEEDED 或 FAILED，就返回 false。这样 replay 时第一条生效的 terminal event 会固定任务终态，后面的重复完成、迟到完成不会覆盖它。

这和系统的 lease epoch 语义有关。worker 至少执行一次，CompleteTask 或日志投递都有可能重复。终态一旦写入，就应该单调推进，不能从 SUCCEEDED 被后来的 FAILED 覆盖，也不能从 FAILED 被旧 attempt 的 SUCCEEDED 覆盖。

它还会检查 event 里的 `task_lease_epoch`。带 epoch 的 terminal event 只有在任务处于 RUNNING 且 epoch 等于当前 lease epoch 时才生效。这就是防 stale completion 的关键。

## Q903. worker Run 中 inFlight 与 control metadata RunningTasks 是否可能不一致？

可能不一致。worker `Run` 里的 `inFlight` 是本地计数，只表示这个 worker 已经 dispatch 到本地 executor pool、还没从 `pool.results` 收到结果的任务数。control metadata 里的 RunningTasks 来自 lease/start/complete 等 RPC，是控制面的视图。

几种情况会导致短暂不一致：worker poll 到任务后 Dispatch 失败，control 已经 lease 但 worker 本地没成功入队；worker 本地任务完成了，但 CompleteTask RPC 超时；control 重启后 metadata 从 log bootstrap，running task 通常会回到 queued；worker 进程重启后本地 inFlight 归零，但 control 还要等 redelivery timeout。

这是分布式系统里本地视图和控制面视图的正常差异。系统靠 lease epoch、redelivery、terminal event 幂等来收敛。要做得更细，可以让 worker 上报 local executor queue depth、inFlight task ids，并在 dashboard 里区分 control running 和 worker local running。

## Q904. localExecutorPool Close 关闭 channel 后 goroutine 如何退出？

`localExecutorPool.Close` 用 `closeOnce` 保证只执行一次，然后关闭 `taskQueue`、`llmQueue`、`actorQueue`，最后 `wg.Wait()`。

每个 worker goroutine 都在循环里 `select`：要么收到 `ctx.Done()`，要么从 queue 读 job。读 queue 时会检查 `ok`。channel 被关闭后，`ok=false`，goroutine 直接 return。`runPythonWorker` 还会 `defer runner.Close()`，所以 Python runner 也会退出。

这里的行为有一个边界：关闭 channel 后，已经在 channel 里的 job 可能仍会被 goroutine 读出并执行，直到队列耗尽或 ctx 取消。如果 ctx 已经取消，select 可能直接走 ctx.Done。当前 Close 用在 worker Run 退出的 defer 场景，语义可以接受。生产化 graceful shutdown 可以更明确地区分 drain 和 cancel：drain 模式等已入队任务完成，cancel 模式尽快停。

## Q905. pythonRunner scanner 如果收到超长 JSON line 会怎样？

`startPythonRunner` 用 `bufio.NewScanner(stdout)` 读取 Python executor 的一行 JSON 响应，并设置了 `scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)`。这表示初始 buffer 是 64KB，最大 token 大小是 16MB。

如果 Python executor 输出的单行 JSON 超过 16MB，`scanner.Scan()` 会返回 false，`scanner.Err()` 通常会是 `bufio.Scanner: token too long`。`pythonRunner.Execute` 会把这个错误返回给 worker，任务会被标记为失败，并写 `TaskFailed`。

这个限制能防止 executor 输出无限大的一行把 worker 内存打爆。代价是大结果不能直接走 stdout JSON line。项目里已经有 result store 的思路，大结果应该写对象存储，log 和 task result 里只放引用。更生产化的 executor 协议可以改成 length-prefixed framing，或者让 executor 直接上传 result object，再返回 `result_ref`。
