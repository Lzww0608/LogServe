# 三、Control Plane、Event Sourcing 与 Metadata View（拓展）

这一组问题更偏架构追问。回答时要把概念落到 LogServe 的真实取舍上：shared log 是事实源，metadata view 是投影；控制面当前是单 active 实例；高可用、多租户、跨地域都需要在 fencing、日志复制和 schema 兼容上继续补齐。

## Q211. Event sourcing 和 CQRS 的关系是什么？LogServe 更接近哪一个？

Event sourcing 关注状态从哪里来。它的核心是把业务状态变化记录成事件，系统重启或迁移时可以从事件序列重建状态。

CQRS 关注读写模型分离。写路径接受命令、校验状态并产生事件；读路径使用为查询优化过的 view，不一定和写模型长得一样。

LogServe 两者都有，但更接近 event sourcing。它的 source of truth 是 shared log，task、workflow、actor、LLM scheduling 的状态都尽量从事件恢复。metadata store 和 dashboard 更像 CQRS 里的 read model，目的是让查询、调度和展示更快。

如果面试官追问，我会这样说：LogServe 的主线是 event-sourced runtime，CQRS 是它自然带出来的工程形态。写侧先 append log，读侧查 materialized metadata view。两者不冲突，但不能反过来说 metadata view 是事实源。

## Q212. materialized view 的 rebuild 与数据库索引重建有什么类似？

相似点是：二者都不是原始数据本身，都是为了查询效率生成的派生结构。

数据库索引可以从 table rows 重建。索引坏了，数据库理论上还可以扫描表来恢复索引。LogServe 的 metadata view 也一样，view 可以从 shared log 重放出来。比如 task row 丢了，只要 `TaskSubmitted`、`TaskStarted`、`TaskCompleted` 还在 log 中，就能重新算出 task 当前状态。

区别在于索引通常只服务查询，而 LogServe 的 view 会参与调度。queue、worker load、workflow step 状态、actor owner 都会影响下一步行为。所以 rebuild 不只是“让查询变快”，还关系到恢复后能不能继续执行。

另一个区别是顺序。数据库索引重建通常不关心业务事件顺序，只要最终 key 排好即可。LogServe replay 必须尊重每个 stream 内的 seq，因为状态机推进依赖事件顺序。

## Q213. 如何设计 outbox pattern 来保证 DB 和 log 的一致性？

如果系统里同时有业务数据库和事件发布，outbox pattern 的做法是：在同一个数据库事务里写业务状态和 outbox 表。事务提交后，再由后台 publisher 扫描 outbox，把事件发到 message broker 或 shared log。发布成功后标记 outbox row 已发送。

放到 LogServe 里，有两种设计。

第一种是继续坚持 shared log 为事实源。控制面先 append shared log，再更新 metadata。这个方案和当前实现一致，不需要 outbox 解决 log 和 view 的一致性，因为 view 本来就可以落后。

第二种是让 PostgreSQL 承担写侧事务。比如在一个 PostgreSQL transaction 里插入 `events` 表和更新 `tasks`、`workflows` 等 view，再由 outbox publisher 把事件复制到外部 log。这种做法适合把 PostgreSQL 当系统事实源的场景。

两种不能混着讲。如果 shared log 是事实源，outbox 主要用于跨服务事件发布。如果 PostgreSQL 是事实源，outbox 可以保证 DB 状态和对外事件发布不会出现“DB 已提交但事件丢了”的问题。

## Q214. 如果需要跨服务事件发布，transactional outbox 是否比直接 AppendLog 更合适？

要看事件的归属。

如果事件本身就是 LogServe 的内部状态变化，例如 `TaskSubmitted`、`WorkflowStarted`、`ActorCommandApplied`，直接 `AppendLog` 更合适。因为这些事件要参与 replay，必须先进 shared log。

如果事件是要通知外部系统，例如“workflow 已完成，请通知计费服务”或“actor 状态快照已生成，请通知审计系统”，transactional outbox 更稳。原因是外部发布通常有独立的 retry、dead letter、订阅者语义，不应该和控制面主链路绑死。

我会把它们分层：shared log 记录 runtime 事实；outbox 发布 integration events。前者服务恢复和调度，后者服务跨服务协作。这样边界清楚，出问题时也好定位。

## Q215. 控制面高可用通常需要 leader election 吗？

如果只有一个 active control 实例负责调度和写状态，通常需要 leader election。leader 负责接收写请求、分配任务、推进 workflow、管理 actor ownership。standby 可以热备，leader 挂了之后接管。

当前 LogServe 更接近单 active 控制面。它的 queue、specs map、worker load 都在进程内维护，直接启动两个 control 会带来重复调度、worker load 分裂、actor ownership 分裂等问题。

leader election 的价值是把“同一时刻谁能写”说清楚。它本身不解决所有问题，还要配合 fencing token。否则旧 leader 网络卡住后又恢复，仍可能继续写 log 或更新 metadata。

如果要做成生产级高可用，我会选一套明确的控制面任期机制：control 启动后先获得 lease，所有调度事件带 control epoch，logstore 或 metadata 层拒绝旧 epoch 写入。

## Q216. 如果多个 scheduler 竞争同一队列，如何实现 fencing？

需要给每个 scheduler 任期或租约编号，也就是 fencing token。谁拿到最新 token，谁才有权修改队列和任务状态。

任务被 scheduler 分配时，事件里要带 `scheduler_epoch` 或 `lease_epoch`。worker 完成任务时，也要把它拿到的 epoch 带回来。control replay 或 CompleteTask 处理 terminal event 时，只接受当前 epoch 对应的完成结果。

LogServe 已经在 task lease 和 actor epoch 上有类似思想。比如 task redelivery 后，旧 worker 的完成事件会因为 lease epoch 过旧而被忽略；actor ownership 也靠 epoch 防止旧 owner 继续写 actor stream。

如果扩展到多 scheduler，不能只靠本地 mutex。mutex 只能保护单进程内并发，不能保护跨进程竞争。要把 fencing 写进 log 语义里，让旧 scheduler 即使恢复了也写不动有效事件。

## Q217. 如果将 queue 放进 Redis、NATS、PostgreSQL，会改变哪些一致性假设？

会改变“谁负责保存待执行任务”的边界。

当前 queue 是 control 进程内的派生结构。它丢了没关系，因为可以从 shared log 和 metadata replay 出来。queue 不是事实源。

放到 Redis 后，队列可共享，多个 control 或 worker 更容易协作，但要处理 Redis 自己的持久化、ack、visibility timeout 和重复投递。Redis 如果配置不当，崩溃时可能丢队列项，所以仍不能把它当唯一事实源。

放到 NATS 这类消息系统后，redelivery、ack 和消费者组会更成熟，但事件顺序和 LogServe task 状态机要重新对齐。任务被投递不等于 task 状态已经进入 running，仍然要由 shared log 事件确认。

放到 PostgreSQL 后，可以用事务管理 queue row 和 metadata view，查询也方便。代价是吞吐会受数据库锁和事务影响。更重要的是，不能让 PostgreSQL queue 变成第二个事实源。最稳的设计是：shared log 仍记录任务事实，外部 queue 只是投递缓存，可以重建。

## Q218. 如果把 metadata view 作为缓存，如何处理 cache invalidation？

原则是让 cache 能被事件驱动刷新，并且能从 log 全量重建。

最简单的方式是每个 view row 记录它应用到的 log position，比如 stream seq 或 event id。收到新事件时，只更新对应对象的 view。查询时如果发现 view 的 applied seq 落后于调用方要求的 seq，可以等待 projector 追上，或者返回“当前 view 可能滞后”。

失效策略不能只靠 TTL。TTL 能缓解旧数据，但不能保证语义正确。LogServe 这类系统更适合基于事件的 invalidation：`TaskCompleted` 使 task status cache 失效，`WorkerHeartbeat` 更新 worker liveness cache，`ActorSnapshotCreated` 更新 actor snapshot cache。

当前实现里的 metadata view 更像内存投影，不是通用 cache。它的优势是简单，问题是 drift 需要靠 replay 检测和 bootstrap 修复。生产化后，我会给每个 view 加 `last_applied_seq`，让 stale view 可观察。

## Q219. 如果 replay 需要几十分钟，控制面启动期间如何提供部分服务？

可以分阶段启动。

第一步先恢复配置和租约类基础信息，比如 model registry、scheduler policy、backpressure、worker registration。这个阶段不急着对外接写请求。

第二步按对象分区恢复。比如 task stream、workflow stream、actor stream 可以按 prefix 或 shard 并行扫描。恢复完某个对象后，就把它标记为 query-ready。dashboard 可以先展示“部分恢复中”，而不是等全量 replay 完才有响应。

第三步开放读服务。已恢复对象可以查状态，未恢复对象返回 `rebuilding`。这比直接返回 not found 好，因为 not found 会误导用户以为对象不存在。

第四步谨慎开放写服务。新 task submit 可以先进入 log，但调度要等相关 worker、scheduler policy、actor ownership 恢复完成。actor 写请求尤其要小心，必须确认该 actor stream 已恢复到 tail，否则可能破坏 command_seq。

所以我的答案是：可以提供部分服务，但不能假装系统已经完全 ready。读可以分区开放，写要按对象恢复边界开放。

## Q220. 如果事件量很大，bootstrap 是否应该并行化？如何保持每个 stream 顺序？

应该并行化，但并行单位不能打破 stream 内顺序。

shared log 的 seq 是 stream 内单调递增。对同一个 `task:<id>`、`wf:<id>`、`actor:<id>` stream，replay 必须从小 seq 到大 seq 顺序处理。否则状态机可能被算错，例如先看到 `TaskCompleted` 再看到 `TaskStarted`。

可以并行的是不同 stream。task A 和 task B 没有顺序依赖，可以由不同 worker goroutine replay。workflow stream 和 actor stream 也可以按 stream id hash 到不同 replay shard。

需要注意全局依赖。比如 model registry、scheduler policy、backpressure 属于配置 stream，最好先恢复。LLM stats 可以最后恢复，因为它是调度优化数据。workflow 恢复可能要重建 step task spec，所以 tasks 和 workflows 的恢复边界要设计清楚。

工程上可以做成两层：先 list streams，按 prefix 分类；每个 stream 内顺序读取；不同 stream 进入 worker pool；最后做一次 reconciliation，把 ready workflow step、queued task、actor pending command 放回应有队列。

## Q221. 如果某个事件 replay 失败，系统应该跳过、停止，还是隔离该 stream？

不能一概而论。

如果是日志物理损坏，比如 CRC 校验失败，应该先由 logstore 的 recovery 处理。tail 部分损坏可以 truncate；非 tail 损坏更严重，不能轻易跳过，因为后续事件可能依赖前面的状态。

如果是 payload schema 不兼容，可以隔离该 stream，并把错误暴露出来。比如某个 actor stream 解析失败，不应该拖垮整个 control plane，但这个 actor 不能继续接收写请求。它应该进入 `replay_failed` 或 `quarantined` 状态，等待人工修复或 migration。

如果是单个可忽略的观测事件失败，比如某条 metrics 事件缺字段，可以记录错误并跳过。但 task、workflow、actor command 这类会影响状态机的事件不能随便跳。

我会按事件等级处理：核心状态事件失败就隔离对象或停止启动；派生统计事件失败可以降级；未知事件如果带有可识别的 schema_revision，可以按兼容规则处理，否则进入 quarantine。

## Q222. 如何设计 metadata schema migration 与 event schema migration？

metadata schema migration 和 event schema migration 要分开。

metadata 是 view，可以重建，所以它的 migration 更自由。可以加列、改索引、拆表，也可以选择清空 view 后从 log 重建。风险主要是 rebuild 时间和线上查询兼容。

event schema 更保守，因为老事件已经写进 log，不能回头改。事件 payload 需要带 schema_revision 或 event_type 的稳定含义。新代码 replay 老事件时，要有 upgrader，把旧 payload 转成当前内存结构。

比较稳的流程是：先让 reader 支持新旧格式，再让 writer 写新格式，最后等老 writer 下线。不要先改 writer，否则老 control 或 worker 读到新事件会解析失败。

LogServe 当前 payload 多为 JSON bytes，灵活但也容易散。后续应该给每类事件维护明确 schema，至少要有 event_type、schema_revision、required fields、default rules 和 deprecation 说明。

## Q223. 如何支持 tenant 级别的 log namespace 和 metadata partition？

要先把 tenant 变成一等字段，而不是只放在 payload 里。

log 层可以把 stream id 设计成 `tenant:<tenant_id>:task:<task_id>` 这类 namespace，或者在 record header 里增加 tenant id。前者实现简单，后者更适合做权限检查和 scan 优化。

metadata 层要按 tenant partition。task、workflow、actor、model registry、worker pool、scheduler policy 都要带 tenant id。查询必须默认带 tenant filter，避免跨租户数据泄露。

控制面也要隔离资源。backpressure、queue high watermark、worker capacity、model cache 配额都应该支持 tenant 级别配置。否则一个 tenant 的大批任务会把公共 queue 挤满。

最后是幂等键作用域。`idempotency_key` 不能全局比较，应该至少是 `tenant_id + stream_id + idempotency_key`。不同租户使用同一个 request id 不应冲突。

## Q224. 如何为 control plane 设计 admin API 和只读 query API？

我会把它们拆开。

只读 query API 面向普通用户和 dashboard，比如 `GetTask`、`GetWorkflow`、`GetActor`、`DashboardSnapshot`、`ListWorkers`、`ReplayWorkflow`。这些接口应该可以水平扩展，必要时读只读副本或 replay cache。

admin API 面向运维和系统管理，比如 `SetBackpressure`、`SetSchedulingPolicy`、`TrimStream`、`ForceRedeliverTask`、`QuarantineStream`、`RebuildMaterializedView`、`RegisterModel`。这些接口会改变系统行为，必须有鉴权、审计和更严格的幂等语义。

危险操作要带 dry-run。比如 trim 之前先返回会影响哪些 stream、compactable records/bytes、是否存在 snapshot。rebuild view 也应提供计划和进度，而不是一个黑盒操作。

如果上生产，我还会给 admin API 增加审批或至少二次确认。尤其是 retention、quarantine、force ownership transfer 这类操作，写错一次会影响恢复路径。

## Q225. 如何从 event log 生成审计报表？

event log 天然适合生成审计报表，但不能直接把内部日志当最终审计产品。

可以先定义审计视图：谁在什么时候提交了 task，哪个 worker 执行了它，经历了几次 redelivery，最终结果是什么；workflow 哪些 step 成功或失败；actor command_seq 如何推进；模型请求是否命中 cache，冷启动耗时多少。

生成方式有两种。离线方式是定期扫描 shared log，把事件投影到审计表或 Parquet 文件。在线方式是 projector 订阅 log，把审计字段实时写入 query store。

要注意脱敏。TaskSpec 可能包含 function source、input 参数甚至用户数据。审计报表应该只暴露必要字段，比如 task id、event type、worker id、timestamp、status、error code。payload 原文要有权限控制。

如果面试官问能不能直接复用 shared log，我会回答：可以作为审计事实来源，但需要专门的审计投影。内部恢复日志和用户可读审计日志不是同一个产品形态。

## Q226. 如何检测 materialized view drift？

最直接的方法是 replay-based checker。

checker 从 shared log 读取某个范围内的 stream，重建一份临时状态，然后和 metadata view 对比。比如 task 状态、attempt、lease epoch、result ref；workflow step status；actor command count、snapshot ref、owner epoch；LLM stats 的 request count 和 cache hit count。

检测可以分层做。快速检查只比对计数和 last_applied_seq。深度检查按 stream replay 全量状态。线上可以用抽样：每分钟抽一些 task/workflow/actor stream，比对 view 是否一致。

发现 drift 后要记录 drift 类型。log 有而 view 没有，通常是 projector 落后或 metadata 写失败。view 有而 log 没有，更严重，说明破坏了 log-first 或有手工写库。字段值不同则要看是不是旧事件被错误应用，比如 stale terminal event 没被 lease epoch 过滤。

LogServe 当前已经有从 log replay workflow/actor 的能力，适合继续扩展成 consistency checker。真正上生产时，还需要把检查结果暴露到 dashboard 和告警。

## Q227. 如果 view drift 发生，自动修复和人工修复边界是什么？

能从 log 确定唯一正确状态的，可以自动修复。

比如 task view 缺失，但 log 里有完整 `TaskSubmitted` 到 `TaskCompleted` 事件。系统可以重建 task row。workflow step 状态落后，也可以按 workflow stream 自动补齐。actor snapshot ref 和 command count 如果 log 和对象存储都完整，也可以修复。

需要人工介入的情况主要有三类。

第一，log 自身有损坏或缺口。此时系统无法确定事实，只能隔离 stream，查备份或人工决定。第二，payload 解析失败，且没有明确 migration 规则。第三，外部副作用已经发生，比如用户任务发了邮件或扣了款，LogServe 只能修复自身 view，不能自动判断外部世界是否需要补偿。

所以边界是：内部派生状态可以自动修；事实源不完整、schema 无法解释、外部副作用不确定时要人工处理。

## Q228. 如何在控制面暴露调度决策原因，便于 debug？

调度器应该输出结构化 decision record，而不是只在日志里写一句“selected worker-1”。

对普通 task，可以记录候选 worker 列表、每个 worker 的 capacity、running tasks、queue wait、是否 active、被过滤原因。最终选择哪个 worker，也要记录 score。

对 LLM task，还要记录 model cache hit、checkpoint cache 状态、预计 cold start、worker 上该模型的历史 EWMA latency、queue penalty、eviction penalty。这样才能解释 locality-aware 或 predicted-latency 为什么选某个 worker。

对 actor command，要记录 actor id、owner worker、epoch、mailbox depth、command_seq、是否发生 ownership transfer。actor 的问题常常不是“没执行”，而是卡在 mailbox 或 owner lease 上。

接口上可以做 `ExplainTaskScheduling(task_id)` 或在 DashboardSnapshot 里展示最近 N 条 decision record。注意这类信息可能包含租户和任务细节，需要权限控制和采样，否则调度器本身会被 debug 日志拖慢。

## Q229. 如果面试官问 CAP，你会如何定位 LogServe 当前实现？

先说明 CAP 只在网络分区下讨论，不要把所有一致性问题都塞进 CAP。

当前 LogServe 是单机实验环境下的 shared log 和单 active control，不是一个多副本分布式一致性系统。在这个范围内，它主要讨论的是 crash recovery、event sourcing、幂等和 replay，而不是跨节点 quorum。

如果扩展成多副本 logd，LogServe 应该更偏 CP。也就是说，当 log quorum 不可用时，控制面应该拒绝新写入，而不是继续接受任务并让不同副本产生分叉。因为 shared log 是事实源，一旦分叉，workflow/actor replay 就会失去确定性。

可用性可以在读侧和已恢复 view 上做降级。比如 dashboard 读旧 view，worker 完成事件排队等待 log 恢复。但新的状态变更不能绕过 log。这个取舍和 LogServe 的主线一致：宁愿 fail fast，也不要产生无法 replay 的状态。

## Q230. 如果要支持跨地域部署，control plane 和 logd 需要怎么改？

跨地域首先要改 logd。单机 append-only log 不够，需要多副本复制、leader 选举、quorum commit、跨地域 read policy。写成功的定义也要变成“事件复制到多数副本并提交”，而不是本地文件 append 成功。

control plane 需要地域感知。scheduler 要知道 worker 所在地域、模型 cache 所在地域、数据所在地域、网络 RTT、跨地域传输成本。LLM 和 actor 尤其明显：模型 checkpoint 不适合频繁跨地域拉取，actor 也不适合在高延迟链路上频繁迁移。

actor ownership 要加全局 fencing。不同地域的 control 不能同时认为自己拥有同一个 actor。可以把 actor owner lease 写入强一致 log，或者给每个 actor 分配 home region，跨地域迁移走显式 transfer 流程。

metadata view 可以区域内本地化。每个地域维护自己的 read model，从全局 log 或本地 shard log 投影。查询可以读本地 view，但要标注 freshness。需要强一致查询时，读 leader 或 quorum。

最后是故障策略。跨地域部署不能只追求“哪里都能写”。LogServe 的 workflow 和 actor 依赖确定 replay，比较稳的做法是按 tenant 或 actor/workflow shard 划分 home region，异地做灾备和只读查询。等复制和 fencing 成熟后，再考虑多地域 active 写入。
