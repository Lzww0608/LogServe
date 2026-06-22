# 45. materialized view 与 CQRS 追问链

这一批只放两个主题：materialized view 和 CQRS。它们都和“读写路径怎么分工”有关，但层级不同。materialized view 是读模型或查询结构的一种实现方式；CQRS 是把命令路径和查询路径分开建模的一种架构选择。把二者混在一起讲，很容易说成“写一套，读一套，所以性能更好”。这个说法太粗。

面试时真正要讲清楚的是边界：谁是 source of truth，视图什么时候更新，视图落后时用户看到什么，命令有没有幂等和并发控制，查询读到的状态和刚写入的状态之间是否有承诺。LogServe 的口径也要稳：shared log 是更接近事实来源的记录，metadata 是可重建的当前状态视图；它验证的是 log-first、replay 和 materialization 机制，不是完整生产级分布式 CQRS/event store。

## Q001. 面试官如果只问一个问题检验你是否理解 materialized view，可能会问什么？

**回答：**

我会预期他问这个问题：

```text
你的系统把事件日志作为 source of truth，再把任务、workflow、actor 的当前状态投影到 metadata 表里，供 dashboard 和 scheduler 查询。某次 projector 崩溃后，日志里已经有 TaskCompleted，但 metadata 里还是 RUNNING。你怎么判断谁是对的？怎么修复这个 view？怎么证明修复过程不会重复触发外部副作用？
```

这个问题比问“materialized view 是什么”更有效。它把 source of truth、投影进度、可重建性、幂等、读写延迟和副作用边界都放到了一起。

materialized view 的核心不是“有一张预计算表”，而是“这张表是从别的权威数据派生出来，用来满足某种查询形状”。在数据库里，它可能是 PostgreSQL 的 materialized view，通过 `REFRESH MATERIALIZED VIEW` 重新生成内容；在事件驱动系统里，它可能是一张 read model 表，由事件流增量投影出来；在 LogServe 这样的控制面里，metadata store 也可以被看作 shared log 的当前状态投影。

所以这道题首先要回答谁说了算。如果日志是 source of truth，metadata 视图落后就应该被修复。修复方式可以是从上次安全 checkpoint 或 high watermark 之后 replay 事件，也可以直接全量 rebuild。关键是 replay 必须只更新派生状态，不能重新执行命令、不能重新发邮件、不能重新扣款、不能重新调外部 API。投影代码要像 reducer：同一批事实事件输入，应该得到同一个视图状态。

第二个要回答的是投影进度。一个成熟的 materialized view 不只保存业务列，还要保存它处理到哪里：event sequence、log offset、source version、last refresh time、last successful checkpoint。没有这些元数据，你只能看到 view 错了，却不知道错在漏事件、乱序、重复处理，还是 schema 迁移失败。

第三个要回答的是读语义。materialized view 可能落后。用户刚提交命令，立刻查询 read model，可能看不到最新状态。这个时候要么承认 eventual consistency，要么提供 read-your-writes 机制，比如让查询等待投影追到某个 sequence，或者直接从写侧返回命令结果。不能一边异步投影，一边对用户承诺强一致。

面试里可以这样答：materialized view 是从权威数据派生出来的查询视图。理解它不是会建一张表，而是能说明 source of truth、refresh 或 projection 的触发方式、投影进度、落后时的读语义、漂移检测和重建流程。LogServe 里 metadata 当前状态就是 shared log 的 materialized view；如果二者冲突，要相信 shared log，再用 replay 修复 metadata。

## Q002. materialized view 的一句话定义是否容易误导，误导点在哪里？

**回答：**

容易误导。常见定义是：materialized view 是把查询结果预先存下来，提高查询速度。这句话没错，但工程上只说到一半。

第一个误导是把它当成普通缓存。缓存通常可以按 key 失效、按 TTL 淘汰，也可能没有完整重建能力。materialized view 更强调查询形状和来源关系：它来自一份或多份源数据，结构可能经过聚合、反规范化、过滤或索引化，理论上应该能从源数据重建。Microsoft 的模式说明里也强调，materialized view 是 disposable 的，不应由应用直接更新。

第二个误导是把它当成 source of truth。视图里有数据，不代表它就是事实来源。真正发生冲突时，要看系统定义相信谁。比如事件日志显示 workflow step 成功，但 dashboard 视图还没更新，这时视图只是落后。如果反过来把 dashboard 表当权威，重启后就可能出现无法从事件解释的状态。

第三个误导是忽略新鲜度。materialized view 可以快，是因为预先计算了结果；代价是它可能不是最新。它是同步刷新、定时刷新、按事件增量投影，还是手工重建，都会影响读语义。只说“查询更快”，不说“能忍受多旧的数据”，就是没讲完。

第四个误导是低估维护成本。视图不是免费表。源数据变化后要更新它；更新可能锁表、占 I/O、产生 WAL、竞争 CPU、拖慢写路径。PostgreSQL 的 `REFRESH MATERIALIZED VIEW` 会替换视图内容；`CONCURRENTLY` 可以避免阻塞读，但需要满足唯一索引等条件，而且同一个 view 仍然不能并发跑多个 refresh。

第五个误导是以为所有查询都适合物化。读得少、源数据变化快、查询简单、强一致要求高，物化视图可能得不偿失。你会花大量资源维护一个很少被用、还总是落后的结构。

更准确的定义是：materialized view 是从权威数据源派生并持久化的查询模型，用存储和维护成本换取读路径性能、查询便利性或跨存储整合；它通常不由业务直接修改，可能短暂落后，并且应该能从源数据重建。

## Q003. materialized view 最常见的生产事故触发条件是什么？

**回答：**

最常见触发条件是投影视图和事实来源漂移，但系统没有观测到，也没有可靠修复路径。很多事故不是视图创建失败，而是视图悄悄错了。

第一类事故是 source 和 view 写入顺序反了。命令处理时先更新 read model，再写事件或主表；如果后一步失败，系统就暴露了一个没有事实依据的状态。用户和下游已经看见“成功”，重放日志却找不到对应事实。这类错误比普通延迟更严重，因为它破坏了可恢复性。

第二类事故是 projector 不幂等。事件消费通常至少一次投递，网络超时、消费者重启、offset 提交失败都会让同一事件重复进来。如果投影逻辑每次都 `count += 1`，却没有 event_id 去重或 sequence 检查，统计值会被重复加。看起来只是 dashboard 数字不准，严重时 scheduler 会基于错误负载做决策。

第三类事故是乱序事件。一个 task 的 `Completed` 先被投影，`Started` 后到；或者跨 partition 消费时同一 entity 的事件顺序被打散。如果 reducer 没有状态机约束，视图可能从 completed 回退到 running。materialized view 不是把事件“收到就写”，它也需要状态转移规则。

第四类事故是 refresh 策略错误。全量 refresh 跑太久，读请求被阻塞；并发 refresh 条件不满足，线上才发现缺唯一索引；定时刷新间隔太长，用户看到旧数据；刷新任务失败后没人报警，视图停在昨天。数据库里的 materialized view 和事件投影里的 read model，本质上都有这个问题：刷新不是一次性配置，而是持续运行路径。

第五类事故是 schema 演进漏投影。源表或事件 schema 增加字段，旧 projector 不认识；新视图字段需要从历史事件回填，却只处理了新事件。结果是新旧数据混在一起，查询结果看似完整，某些行却永远是默认值。

第六类事故是高基数和宽视图拖垮存储。为了让查询方便，把所有字段、聚合、反查索引都塞进 view。短期查询快，长期刷新慢、磁盘大、索引膨胀、vacuum 或 compaction 压力上升。

第七类事故是读语义没说清。用户提交命令后立刻刷新页面，read model 还没追上，就以为操作失败，又重复提交。结果写侧产生重复命令，读侧过一会儿显示两个结果。这个问题不能靠“视图 eventually consistent”一句话糊过去，需要产品和 API 语义配合。

所以我会把触发条件总结成一句：materialized view 事故通常来自“派生状态被当成权威状态”，再叠加投影不幂等、进度不可见、刷新失败不报警和读一致性承诺不清。

## Q004. materialized view 的指标应该怎么设计才不会只看平均值？

**回答：**

materialized view 的指标要围绕“新鲜度、正确性、刷新成本、读收益”设计。只看查询平均延迟，最多说明视图读起来快，不说明视图是否可信。

第一组是新鲜度指标。包括 source_high_watermark、view_high_watermark、projection_lag_events、projection_lag_seconds、last_successful_refresh_time、oldest_unprojected_event_age。事件投影视图要看 offset 或 sequence；定时刷新视图要看距离上次成功刷新多久。不要只看 projector 进程是否存活。

第二组是正确性指标。包括 drift_check_failures、source_vs_view_sample_mismatch、duplicate_event_skipped、out_of_order_event、invalid_state_transition、rebuild_checksum、row_count_delta。最好有后台 reconciler 抽样或分区对账，证明 view 能被 source 解释。

第三组是刷新和投影成本。看 refresh_duration、refresh_rows_read、refresh_rows_written、incremental_updates、full_rebuild_count、CPU、I/O、WAL bytes、lock wait、memory spill、index build time。物化视图最怕“读很快，但每次刷新把数据库打满”。

第四组是查询收益。要按 query type 看 p50/p95/p99、rows returned、cache hit、index hit、fallback_to_source_count、view_miss_count。视图的价值是服务某些查询形状，不是让所有查询都绕一圈。

第五组是可用性指标。包括 projector_restart、projection_stuck、refresh_failed、refresh_retry、dead_letter_events、schema_upcast_failures、checkpoint_commit_failures。投影卡住时，读路径可能仍然返回旧数据，所以 HTTP 200 不代表系统健康。

第六组是读一致性体验。看 read_after_write_miss、wait_for_projection_timeout、user_retry_after_command、command_result_query_gap。很多 materialized view 的用户痛感不是“慢”，而是“我刚改完怎么没变”。

第七组是存储和索引膨胀。包括 view_size_bytes、index_size_bytes、bloat、partition_count、compaction/vacuum duration、retention cleanup。视图如果不可控增长，最终会把写入和刷新路径拖垮。

面试里可以这样答：materialized view 的指标不能只看平均查询延迟。我会同时看投影 lag、high watermark、刷新耗时、锁等待、对账 mismatch、重复/乱序事件、读后写缺失、查询 p99、存储膨胀和重建时间。一个快但落后的视图，不是健康视图。

## Q005. materialized view 的正确性边界和性能边界分别是什么？

**回答：**

materialized view 的正确性边界是：它必须能从定义好的 source of truth 派生出来，并且在不一致时可以被修复或重建。它不应该产生 source 里不存在的事实，也不应该靠人工补写变成第二套事实来源。

正确性上有几个底线。

第一，source of truth 要明确。可以是事件日志，可以是主表，可以是对象元数据，但不能同时让两个地方都自称权威。冲突发生时，系统要知道相信谁。

第二，投影逻辑要确定。给定同一批源数据，rebuild 出来的 view 应该一致。投影代码不能依赖当前时间、随机数、外部 API 或无法重放的隐式状态。需要这些信息时，应把它们变成事件或源数据的一部分。

第三，投影要幂等。重复事件、重试 refresh、projector 崩溃恢复，都不应该让 view 多加一次、少删一次。event_id、source version、sequence、unique key、upsert 语义都可以用来保护这一点。

第四，投影进度要持久化。只保存 view 内容，不保存处理到哪里，恢复时就不知道该从哪里继续。checkpoint 必须和 view 更新有清楚的提交顺序，否则会出现“checkpoint 已前进，view 没写完”的洞。

第五，replay 不产生外部副作用。重建 materialized view 只能更新派生状态，不能重发通知、重跑任务、重扣款。这是 event-sourcing 和 log-first 系统里最容易出错的底线。

性能边界则是用写入和存储成本换读取速度。视图越贴近查询，读越快；但字段越多、聚合越重、刷新越频繁，维护成本越高。全量 refresh 简单但重；增量投影高效但复杂；同步更新新鲜度好但增加写延迟；异步更新写路径轻，但读路径会有 lag。

在 PostgreSQL 这类数据库里，materialized view 的性能还受 refresh 方式、索引、锁、排序、WAL、磁盘 I/O 影响。`CONCURRENTLY` 保护读路径，但不是免费午餐；它需要合适的唯一索引，同一视图一次也只能跑一个 refresh。

所以我会这样收束：correctness 上，materialized view 是可重建的派生状态；performance 上，它用预计算、反规范化和索引换查询速度，代价是刷新、存储、锁、投影 lag 和运维复杂度。它适合读路径明确、源数据可追溯、可接受一定新鲜度窗口的场景。

## Q006. 面试官如果只问一个问题检验你是否理解 CQRS，可能会问什么？

**回答：**

我会预期他问这个问题：

```text
你把系统设计成 CQRS：命令写入 shared log，查询读取 metadata view。用户提交一个 StartWorkflow 命令后马上查询 workflow 状态，读模型还没更新。你给用户什么响应？如果命令重试、投影延迟、读模型漂移、查询要分页和过滤，你怎么设计 command id、version、projection lag 和 read-your-writes？
```

这道题能判断你是否把 CQRS 理解成真实系统，而不是 PPT 上的“读写分离”。

CQRS 的核心是把修改状态的模型和读取状态的模型分开。command 表达业务意图，进入写侧，写侧校验不变量、处理并发、追加事件或更新权威状态；query 走读侧，用适合查询的结构返回数据。读侧可能是 materialized view、搜索索引、宽表、缓存、dashboard snapshot。写侧和读侧可以在同一个数据库里，也可以完全分开。CQRS 不要求一定有两个物理库。

这道题要先说命令语义。`StartWorkflow` 是一个 command，不是“插入一行状态”。它要有 command_id 或 idempotency key，重复提交时能返回同一个结果或明确冲突。写侧要判断 workflow 是否已存在、状态能否转移、调用者是否有权限、是否满足配额。命令成功的标准应该是权威写入成功，比如事件追加成功，而不是读模型已经更新。

然后说查询语义。查询读 metadata view，如果 projection 还没追上，就可能返回旧状态。系统可以提供几种选择：直接返回 202/accepted 和 command sequence，让前端轮询；查询接口支持 `wait_until_sequence`，在一定时间内等 projection 追上；或者写命令响应里带上足够的当前状态，满足 read-your-writes。不能默认异步投影，却让用户以为查询是强一致。

再说故障边界。命令成功但投影失败时，写侧事实不能丢；读模型可以补。投影重复消费事件时，read model 不能重复加。投影卡住时，查询要暴露 lag，不能继续假装新鲜。读模型漂移时，要能 replay 或 rebuild。

面试里可以这样答：CQRS 不是简单把数据库拆成读库写库，而是把 command 的状态改变语义和 query 的读取形状分开。理解 CQRS 要能说明命令幂等、写侧不变量、读侧投影、eventual consistency、read-your-writes、投影 lag 和漂移修复。

## Q007. CQRS 的一句话定义是否容易误导，误导点在哪里？

**回答：**

容易误导。最常见的一句话是：CQRS 就是读写分离。这个定义太容易把 CQRS 和主从复制、读写库拆分、缓存架构混在一起。

第一个误导是把“读写分离”理解成数据库拓扑。一个系统可以有主库写、从库读，但仍然不是 CQRS，因为它的写模型和读模型可能是同一套 CRUD 表，只是复制到不同节点。CQRS 关注的是模型和职责分离：command model 用来处理意图和不变量，query model 用来服务读取和展示。

第二个误导是把 CQRS 等同于 event sourcing。它们经常一起出现，但不是一回事。CQRS 可以用普通状态表作为写模型，也可以用事件日志。Event sourcing 是把状态变化保存为事件流；CQRS 是把写入和读取模型分开。你可以只用 CQRS，不用 event sourcing；也可以 event sourcing 后仍然设计很差的 query model。

第三个误导是认为 CQRS 必然提高性能。它可能提高读性能，因为读模型可以反规范化、索引化、按页面组织；也可能拖慢系统，因为你多了事件、投影、同步、版本、补偿、监控和一致性语义。Martin Fowler 对 CQRS 的提醒很直接：如果领域不适合，加入 CQRS 会带来显著复杂度和项目风险。

第四个误导是忽略一致性。读写模型分开以后，读侧常常落后于写侧。用户刚写完却读不到，是设计结果，不是偶然 bug。你必须定义哪些查询可以 eventual consistent，哪些需要读写一致，哪些可以等待投影，哪些应该直接从写侧读。

第五个误导是把所有操作都包装成 command/query 就算完成。真正的 command 要表达业务意图，比如 `ApproveOrder`、`StartWorkflow`、`LeaseTask`；不是把 `UpdateRow` 改名叫 command。真正的 query 要没有业务副作用，最多记录访问日志或指标，不能偷偷改状态。

更准确的一句话是：CQRS 是用不同的模型处理状态修改和状态读取；写侧保护业务不变量和事实记录，读侧按查询形状优化，二者之间需要明确投影、一致性、幂等和恢复边界。

## Q008. CQRS 最常见的生产事故触发条件是什么？

**回答：**

最常见触发条件是团队只拆了读写路径，却没有设计读写之间的协议。结果写侧、事件、投影、读侧各自运行，看起来解耦，出了问题没人知道谁对。

第一类事故是命令幂等缺失。客户端超时后重试，网关重试，worker 重投递，同一个 command 被处理两次。写侧如果没有 command_id、idempotency key、expected version，就会重复创建 workflow、重复扣库存、重复触发下游。读模型后面再幂等也救不了已经发生的副作用。

第二类事故是读模型落后导致用户重复操作。命令已经成功，read model 还没更新，前端刷新看到旧状态，于是用户再点一次。系统如果没有“命令已接受”的明确响应、没有查询等待 sequence、没有按钮状态保护，就会把 eventual consistency 变成业务重复提交。

第三类事故是投影漂移。事件消费失败、schema 变更、乱序、重复消费、dead letter 未处理，读模型慢慢偏离写侧。因为查询还返回 200，问题常常到报表、对账或客户投诉时才发现。

第四类事故是把 query 写出副作用。为了方便，有人在查询接口里“顺便修复状态”“顺便更新 last_seen”“顺便补发任务”。CQRS 的查询路径本来应该可缓存、可重试、可扩容；一旦带副作用，缓存、重试和 replay 都变危险。

第五类事故是写侧不变量泄漏到读侧。命令 handler 只做简单写入，把业务校验放到 read model 或前端。并发时两个命令都读到旧视图，都以为可以执行，最后写出冲突状态。写侧必须基于权威状态校验，不应依赖可能落后的查询模型。

第六类事故是过早使用 CQRS。领域很简单，CRUD 足够，团队却引入 command bus、event bus、projection、read store、versioning、saga。开发速度下降，排障路径变长，最后大家绕过架构直接改表。这个不是技术失败，是适用性判断失败。

第七类事故是多 read model 没有一致的生命周期。一个 dashboard view 更新了，一个搜索索引没更新，一个报表库卡在旧 schema。不同查询返回不同答案，用户不知道该相信哪个。CQRS 允许多个读模型，但每个读模型都要有 owner、lag、rebuild 和弃用策略。

所以我会总结：CQRS 事故通常来自“分离了组件，没定义契约”。命令幂等、写侧版本、投影进度、读后写语义、query 无副作用、漂移检测和重建流程缺一块，都会在线上变成一致性问题。

## Q009. CQRS 的指标应该怎么设计才不会只看平均值？

**回答：**

CQRS 的指标要把 command path、projection path、query path 分开看，再把它们串起来。平均 API 延迟没有意义，因为写成功、投影成功、读到最新状态是三件事。

第一组是 command 指标。包括 command_received、command_validated、command_committed、command_rejected、idempotency_hit、idempotency_conflict、expected_version_conflict、command_duration_p99、side_effect_attempt、side_effect_dedup。写侧指标要证明不变量被保护，而不是只看 HTTP 200。

第二组是权威写入指标。如果写侧是 event store，要看 append_latency、append_failure、stream_revision_conflict、event_size、fsync/WAL latency、per-stream sequence gap。如果写侧是数据库状态表，要看事务冲突、锁等待、deadlock、commit p99。CQRS 的根不能飘。

第三组是 projection 指标。包括 projection_lag_events、projection_lag_seconds、last_projected_sequence、dead_letter_count、duplicate_event_skipped、out_of_order_event、projection_retry、rebuild_duration、checkpoint_commit_failure。读模型健康主要看这里。

第四组是 query 指标。按 read model、查询类型、过滤条件、分页大小统计 p50/p95/p99、error、stale_read、fallback_to_source、cache hit、index hit、rows scanned。CQRS 的收益要体现在具体查询形状上，而不是总体平均。

第五组是读后写体验。统计 command_commit_to_visible_ms、read_your_writes_wait_ms、wait_until_sequence_timeout、user_retry_after_command、frontend_poll_count。用户关心的是“我刚做的操作什么时候能看到”。这个指标比普通 query p99 更接近真实体验。

第六组是漂移和对账。定期对 source of truth 与 read model 做抽样或分区校验，记录 mismatch_count、missing_row、extra_row、wrong_status、reconciliation_fixed。没有对账，读模型漂移只会在事故后被发现。

第七组是复杂度和成本。command handler 数、read model 数、projection owner、event schema version、rebuild time、storage size、operational alerts。CQRS 引入了结构性成本，要承认它，并观察它是否还值得。

面试里可以这样答：CQRS 指标要按三段看：command 是否正确提交，projection 是否及时且可重放，query 是否快且读到符合承诺的新鲜度。只看平均响应时间，会把写侧冲突、投影 lag、读模型漂移和用户重复提交全盖住。

## Q010. CQRS 的正确性边界和性能边界分别是什么？

**回答：**

CQRS 的正确性边界在写侧。命令处理必须基于权威状态校验业务不变量，提交事实或状态变化，并通过幂等、版本、事务或 stream revision 防止并发冲突。读侧可以落后，可以重建，可以有多个形态，但不能替写侧裁决事实。

正确性上有几个底线。

第一，command 和 query 的职责要分开。command 可以改变状态，但必须有明确结果和幂等语义；query 应该读取状态，不应隐藏业务副作用。

第二，写侧不变量不能依赖落后 read model。比如任务是否可 lease、workflow step 是否可 start、actor command sequence 是否连续，都要在权威写侧判断。read model 只能辅助显示和查询。

第三，读侧落后必须被承认。系统要定义 eventual consistency、read-your-writes、wait until sequence、直接返回 command result 等策略。不能在异步投影架构里承诺所有查询强一致。

第四，投影要可恢复。读模型损坏、落后、schema 变更后，应该能从写侧事实重建。重建不能产生外部副作用。

第五，命令要幂等。至少要有 command_id、idempotency key 或 expected version。否则重试和超时会直接变成重复业务效果。

性能边界则取决于读写是否真的有不同需求。CQRS 的好处是写侧可以围绕不变量、事务和顺序设计，读侧可以围绕查询、索引、聚合、缓存和分页设计。高读低写、复杂查询、不同展示模型、多种 read model 的系统会更受益。

但 CQRS 的性能收益不是免费的。它增加了投影延迟、事件或消息处理、额外存储、schema 演进、rebuild、监控和运维成本。写侧提交快了，用户可能还在等读侧可见；读侧查询快了，后台 projector 可能已经堆积。性能优化不能只看局部。

所以我会这样说：CQRS 的 correctness 由写侧事实和命令不变量保证，read model 是派生视图；performance 则来自读写模型可以分别优化。它适合读写形态明显不同、能接受并管理投影延迟的系统，不适合用来包装简单 CRUD。