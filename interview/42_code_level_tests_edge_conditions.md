# 测试与边界条件

这一组问题面试时很容易被追问到代码细节。回答时不要停在“写一个单元测试”这层，最好能说清楚三件事：先搭什么状态，怎么触发边界条件，最后断言哪几个状态没有被错误推进。LogServe 这部分已经有不少测试可以直接引用，下面的回答会尽量按现有测试来组织。

## Q921. 如何测试 idempotency conflict？

我会分两类测。

第一类是“同一个幂等键、同一个语义 payload”，应该返回第一次提交的对象，而不是新建一个。比如 `SubmitTask` 第一次传入某个 `idempotency_key`，第二次传入等价的参数，即使 JSON 字段顺序不同，也应该得到同一个 `task_id`。这能验证 fingerprint 做的是规范化后的语义比较，而不是简单字符串比较。

第二类是 conflict。做法是：

1. 创建一个 control service，底层用内存 metadata 和可正常 append 的 log client。
2. 第一次调用 `SubmitTask`，传入 `idempotency_key=demo-key` 和参数 `{"x": 1}`。
3. 第二次继续用 `demo-key`，但参数改成 `{"x": 2}`。
4. 断言第二次返回错误，并且错误信息包含 `idempotency conflict`。
5. 再读 metadata，确认系统没有创建第二个 task。

项目里已有的覆盖点包括 `TestSubmitTaskIdempotencyKeyRejectsDifferentPayload`，并且 workflow、actor、LLM 也分别有类似测试。这个边界很重要，因为客户端超时后重试是正常行为，但“同一个幂等键换了语义”必须被拒绝，否则平台会把两个不同请求混成同一个结果。

## Q922. 如何测试 stale task lease rejected？

这个测试要制造“旧 lease 后到”的场景。

可以按集成测试的方式做：

1. 把 redelivery timeout 调短，比如 100ms。
2. 注册两个 worker：`worker-1` 和 `worker-2`。
3. 提交一个普通 task。
4. `worker-1` 先 `PollTask`，拿到 task 后调用 `StartTask`，记录第一次的 `task_lease_epoch`。
5. 等待超过 redelivery timeout，让控制面认为这个 task 可以重新投递。
6. `worker-2` 再 `PollTask`，拿到同一个 task，此时 lease epoch 应该递增。
7. 用旧的 epoch 去调用 `CompleteTask`。
8. 断言返回错误，错误信息包含 `stale task lease`。
9. 继续查询 task，确认它没有被旧 completion 写成成功状态，也没有写入旧结果。

项目中的 `TestStaleTaskCompletionRejectedAfterRedelivery` 已经覆盖了主路径。更严格一点还可以补一个变体：用旧 worker_id 加旧 epoch 完成任务，断言同样被拒绝。这样可以同时验证 worker 身份和 lease epoch 两个 fencing 条件。

## Q923. 如何测试 stale actor completion rejected？

actor 的 stale completion 要测 owner 和 epoch 两个维度。

一个直接的测试方法是：

1. 创建 actor，并让它的当前 owner 是 `worker-new`，当前 actor epoch 是 2。
2. 构造一个还带着旧 owner 或旧 epoch 的 actor task，比如 `owner_worker_id=worker-old`、`actor_epoch=1`。
3. 调用 `CompleteTask`，让它尝试提交 `ActorCommandApplied`。
4. 断言返回错误，错误信息包含 `stale actor completion rejected`。
5. 读取 actor metadata，确认 `command_count` 没有增加，actor state 没有被旧结果覆盖。
6. 读取 `actor:<actor_id>` stream，确认没有多出一条错误的 `ActorCommandApplied`。

这个测试本质上是在验证 actor 的 epoch fencing。actor 可以 failover 到新 worker，但旧 worker 如果后来恢复，不能继续提交旧 epoch 的状态变更。否则两个 worker 会同时推进同一个 actor 的内存状态，mailbox 串行化就失效了。

## Q924. 如何测试 duplicate successful completion 不写第二个 workflow final result？

这个测试应放在 workflow 已经成功之后再做重复提交。

具体做法是：

1. 提交一个简单 workflow，例如 `embed -> search -> generate_mock`。
2. 等待 workflow 进入 completed。
3. 找到 final step 对应的 task，再模拟一次重复的 `CompleteTask`，状态仍然是 succeeded，结果也相同。
4. 读取 `wf:<workflow_id>` stream。
5. 统计 `WorkflowCompleted` 事件数量，断言等于 1。
6. 同时断言 workflow final result 没有变化，`ReplayWorkflow` 与 metadata 仍然一致。

项目里的 `TestWorkflowSimpleRAGReplayAndDedup` 已经按这个思路做了。这里要强调的是，平台无法保证 worker 绝对只执行一次，但必须保证最终提交层去重。重复 completion 到达时，不能再写第二个 workflow final result。

## Q925. 如何测试 worker stops after embed 后重启从 search 继续？

这个测试要让第一个 worker 只完成 `embed`，然后停止。

测试步骤可以这样设计：

1. 定义一个三步 workflow：`embed(query)`、`search(vec)`、`generate_mock(query, docs)`。
2. 启动第一个 worker，并把它的执行上限设成 1，让它最多完成一个 task。
3. 提交 workflow。
4. 等到 `embed` step 状态变成 succeeded。
5. 停止第一个 worker。
6. 查询 workflow metadata，确认 `embed` 的 attempts 是 1。
7. 启动第二个 worker。
8. 等待 workflow completed。
9. 再次确认 `embed` attempts 仍然是 1，说明恢复后没有从头执行。
10. 确认 `search` 和 `generate_mock` 正常完成，最终结果符合预期。

项目里的 `TestWorkflowWorkerRecoveryContinuesAfterCompletedStep` 就是这个场景。这个测试能说明 workflow replay 和 ready step 调度是有效的：系统从 log 和 metadata 里知道 `embed` 已经成功，所以重启后只需要继续调度下游 step。

## Q926. 如何测试 actor 1000 concurrent inc 序列化？

这个测试要用并发提交制造压力，但断言应该落在 actor 的线性化结果上。

测试步骤：

1. 创建 `Counter` actor，初始 `value=0`。
2. 启动 worker。
3. 用 1000 个 goroutine 并发调用 `counter.inc()`。
4. 等所有调用返回，收集所有错误。
5. 再调用一次 `counter.get()`。
6. 断言返回值等于 `1000`。

项目里的 `TestActorConcurrentMailboxSerializes1000Increments` 覆盖了这个场景。为了让测试更有说服力，还可以额外读 `actor:<actor_id>` stream，检查 `ActorCommandSubmitted` 和 `ActorCommandApplied` 的 `command_seq` 是连续的，且 applied 的顺序没有跳号。最终 `get()==1000` 说明没有并发写丢失，连续 command_seq 则说明顺序是由 mailbox 维护出来的。

## Q927. 如何测试 snapshot replay command count 小于 full replay？

这个测试需要先让 actor 产生足够多的命令，再触发 snapshot。

可以这样做：

1. 创建 `Counter` actor，并设置 `snapshot_every=20`。
2. 连续调用 `inc()` 100 次。
3. 确认 actor metadata 中有 `snapshot_ref`，并且 `snapshot_command_count` 等于 100 或接近最后一次 snapshot 点。
4. 调用 `ReplayActor`。
5. 断言 `full_replay_commands` 大于 `snapshot_replay_commands`。
6. 再断言 replay 出来的 actor state 和 metadata state 一致。

项目里的 actor recovery 测试已经验证了这个结果，典型现象是 full replay 需要处理完整命令历史，而 snapshot replay 只需要加载 snapshot 后面的 tail log。更严谨的对比可以再跑一个关闭 snapshot 的 actor，确认它的 replay command count 不会下降。

## Q928. 如何测试 checkpoint cache restart 后 manifest 恢复？

这个测试应该放在 worker 的 model cache 层做，因为这里最关心的是本地磁盘 manifest 是否能被重新扫描出来。

测试步骤：

1. 创建临时 checkpoint source 目录和 worker cache 目录。
2. 写入一个模型 checkpoint，比如 `model-A:v1`。
3. 构造第一个 `modelCheckpointCache`，调用 `ensureCheckpoint`，让它把 checkpoint 拷到本地 cache，并写出 manifest。
4. 重新构造一个新的 `modelCheckpointCache`，使用同一个 cache 目录，模拟 worker 重启。
5. 调用内部的 `entries()` 或等价查询。
6. 断言 `model-A:v1` 已经存在，路径指向本地 cache 文件，大小和 manifest 记录一致。

项目里的 `TestModelCheckpointCacheReportsExistingCheckpointOnStartup` 已经覆盖这个点。集成层还可以补一个 worker restart 测试：worker 重启后 heartbeat 里应继续上报已缓存模型，调度器才能继续把对应 LLM task 优先派给它。

## Q929. 如何测试 predicted-latency 选择历史更快 worker？

这个测试要避免真实执行带来的噪声，直接种入历史观测值更稳定。

测试方法：

1. 注册两个 worker，并注册同一个模型。
2. 往 LLM stats 里写入历史完成事件或直接通过 helper 种入 materialized stats。
3. 设置 `worker-1` 历史 total latency 为 200ms，设置 `worker-2` 历史 total latency 为 20ms。
4. 把 scheduler policy 切到 `PREDICTED_LATENCY`。
5. 提交一个同模型的 LLM task。
6. 让 `worker-1` poll，断言拿不到任务。
7. 让 `worker-2` poll，断言拿到任务。

项目里的 `TestLLMPredictedLatencySchedulerUsesObservedHistory` 就是这个思路。这个测试验证的是调度器使用 materialized LLM stats，而不是每次扫描所有 `llm:*` stream。补充边界可以包括：历史样本为空时走默认估计、两个 worker 预测值相同时按负载或 worker id 稳定决策、cached worker 很忙时 queue penalty 能改变选择。

## Q930. 如何测试 logical trim 不影响 actor replay？

这个测试要同时看两个结果：trim 后读不到旧事件，但 replay 仍然能恢复正确状态。

步骤如下：

1. 创建 actor，设置 snapshot 策略。
2. 执行一批命令，让系统生成 `ActorSnapshotCreated`。
3. snapshot 创建后调用 `TrimStream`，把 snapshot 之前的 actor stream 标记为可裁剪。
4. 从 `actor:<actor_id>` 的 seq 1 开始读，确认 `ActorCreated` 等早期事件已经被 logical trim 过滤。
5. 确认 tail log 中仍能看到 `ActorSnapshotCreated` 和 snapshot 之后的 command 事件。
6. 调用 `ReplayActor`，断言状态与 metadata 一致。
7. 读取 stream stats，断言 `compactable_records` 和 `compactable_bytes` 大于 0。

项目里的 actor recovery 测试已经把这几个断言连在一起。这里的重点是，logical trim 只是改变 replay/read 的起点和 compactable 统计，不代表立刻物理删除 segment。只要 snapshot_ref 可靠，actor replay 就可以从 snapshot 加 tail log 恢复。

## Q931. 如何测试 logstore partial tail truncation？

partial tail 要在磁盘文件层制造。

测试步骤：

1. 创建一个 logstore，正常 append 一条 record。
2. 关闭 store。
3. 直接打开当前 segment log 文件，在尾部追加几个无效字节，比如 3 个字节，模拟崩溃时只写了一半 header。
4. 重新 `Open` logstore。
5. recovery 应该扫描到尾部坏记录，并 truncate 到最后一条完整 record 的 offset。
6. 调用 `Read`，断言只能读到那条完整 record，seq 和 payload 都正确。

项目里的 `TestRecoveryTruncatesPartialTail` 覆盖了这个场景。更细一点可以加两个变体：一个是 partial header，一个是 header 完整但 body 不完整。还可以在 reopen 后检查文件大小，确认坏尾巴确实被截掉。

## Q932. 如何测试 segment rollover 边界？

segment rollover 需要把 segment size 设得很小。

测试可以这样写：

1. 用很小的 `SegmentSizeBytes` 打开 store，比如 220 字节。
2. append 多条 payload 较大的 record，让单个 segment 放不下全部数据。
3. 检查数据目录里生成了多个 `.log` segment。
4. 关闭再重新打开 store。
5. 从 stream 的 seq 1 开始读。
6. 断言所有 record 数量正确，seq 连续，payload 没变。

项目里的 `TestSegmentRollingRecoverAndReadAcrossSegments` 已经覆盖了跨 segment 读和恢复。还可以补一个边界测试：某条 record 刚好写满当前 segment 时不 rollover，下一条才 rollover。这个能防止 `>=` 和 `>` 条件写错导致空 segment 或过早切段。

## Q933. 如何测试 fsync interval policy？

interval policy 的测试要分清楚“功能路径”和“崩溃持久性”。

已有测试 `TestFsyncPoliciesAppendAndRecover` 会用 `FsyncInterval` append 一条记录，然后 close、reopen、read，确认 interval policy 下基本 append/recover 没问题。这个测试能保证配置路径可用，但它不是严格的 crash consistency 证明，因为 close 时通常还会 flush。

更强的测试可以这样补：

1. 设置 `FsyncPolicy=interval`，interval 设成 1ms。
2. append 第一条 record，记录 store 内部的 `lastSync`。
3. sleep 超过 interval。
4. append 第二条 record。
5. 断言 `lastSync` 已经推进，说明第二次 append 触发了 interval sync。
6. close、reopen 后确认两条 record 都能读到。

如果要测崩溃语义，需要单独做进程级测试：子进程 append 后不正常退出，父进程重新打开 store 检查记录。这个测试在不同文件系统和硬件缓存下会有差异，所以结论应该写成 durability policy 的行为验证，而不是承诺 interval 模式下每条成功 append 都绝对落盘。

## Q934. 如何测试 queue high watermark backpressure？

这个测试要让队列积压起来。

步骤如下：

1. 配置 backpressure，把 `queue_high_watermark` 设成 1。
2. 不启动 worker，或者让 worker 不消费任务。
3. 提交第一个 task，让它进入 queued 状态。
4. 再提交第二个不同 task。
5. 断言第二次提交失败，错误信息包含 `backpressure` 或 queue watermark 相关信息。
6. 查询 metadata，确认第二个 task 没有被创建。

项目里的 `TestBackpressureRejectsNewTaskWhenQueueBacklogExceedsWatermark` 覆盖了这条路径。另一个重要边界是幂等重复请求：`TestBackpressureAllowsIdempotentDuplicateWhenQueueIsFull` 验证了同一个 idempotency key 的重复提交会返回已有 task，而不会被 queue watermark 错误拒绝。

## Q935. 如何测试 log append slow backpressure？

log append slow backpressure 关注的是 shared log 变慢时，控制面要停止继续接收新写入压力。

现有测试的做法比较直接：

1. 通过 `SetBackpressure` 把 `log_append_slow_ms` 设置成很低，比如 1ms。
2. 由于配置写入本身会 append log，control 会记录最近一次 log append latency。
3. 再提交一个新 task。
4. 如果最近 append latency 超过阈值，提交应被拒绝。
5. 断言错误信息包含 `last log append latency`。

项目里的 `TestLogAppendSlowBackpressureRejectsNewTask` 覆盖了这条路径。更精确的测试可以用 fake log client 注入固定延迟：当延迟 50ms、阈值 10ms 时拒绝；当阈值 100ms 时放行。还要保留一个幂等重复提交的边界：如果请求是已经成功提交过的 idempotent duplicate，控制面应先命中已有结果，再考虑 backpressure，避免客户端重试被误伤。
