# 九、Metadata Store、Object Store、PostgreSQL、MinIO/S3 与持久化边界（拓展）

这一组问题会把 LogServe 放到更接近生产系统的语境里看。核心判断仍然不变：数据库可以做 view，对象存储可以放大对象，shared log 才负责解释系统历史。很多扩展问题的答案，最后都会回到这条边界。

## Q671. 数据库作为 materialized view 与作为 source of truth 的设计差异是什么？

差异在写入顺序、恢复方式和一致性边界。

如果数据库是 source of truth，客户端提交成功通常意味着数据库事务已经提交。系统恢复时直接读数据库表。日志、消息队列、缓存、索引都从数据库派生。这个模式下，数据库事务是状态变化的核心边界。

如果数据库是 materialized view，数据库只保存当前投影。事实来自事件日志。写入路径通常是先写 log，再把事件应用到数据库 view。数据库丢了可以从 log 重建。数据库和 log 不一致时，以 log 为准。

LogServe 当前属于后一种。PostgreSQL 保存 task、workflow、actor、worker、model 的当前视图，便于 dashboard 和 SQL 查询；shared log 保存 `TaskSubmitted`、`WorkflowStarted`、`ActorCommandApplied`、`LLMCompleted` 这些事件事实。

这会带来几个工程差异：

- metadata view 可以短暂落后于 log。
- PostgreSQL 写失败不等于业务事实丢失。
- control 重启可以通过 `BootstrapFromLog` 重建 view。
- 一致性检查要比较 replay state 和 metadata state。

如果把 PostgreSQL 改成 source of truth，很多代码路径都要变。提交任务要先开数据库事务，写 task row，再通过 outbox 或 CDC 发布事件。workflow/actor replay 也会从“事实恢复”变成“校验和审计”。这不是小改，是系统语义的转换。

## Q672. outbox pattern、transaction log tailing、CDC 可以如何用于本项目？

这三种方式都可以解决“数据库状态”和“外部事件发布”之间的一致性问题，但适用位置不同。

outbox pattern 的做法是：在同一个数据库事务里写业务表和 outbox 表。事务提交后，一个 relay 进程读取 outbox，把事件发到 shared log、NATS 或其他系统。这样不会出现“业务表写成功但事件没发出去”的问题。

如果 LogServe 将来让 PostgreSQL 成为 source of truth，outbox 就很合适。比如 `SubmitTask` 在一个事务里写 `task_instances` 和 `outbox_events`，relay 再 append `TaskSubmitted` 到 logd。当前 LogServe 是 log-first，所以 outbox 不是主路径。

transaction log tailing 通常指读取数据库 WAL/binlog。比如用 Debezium 订阅 PostgreSQL 变更，把表变更转成事件。这可以用于把 metadata view 同步到外部分析系统，但不适合作为 LogServe 的核心事件源。因为数据库行变更不一定表达完整业务语义：一行 workflow 状态从 running 变 completed，不等于完整解释了每个 step 的依赖和重试历史。

CDC 更适合旁路消费。比如把 PostgreSQL metadata view 的变化同步到 Elasticsearch、ClickHouse 或审计系统。它可以提升查询和报表能力，但不能替代 shared log replay。

对当前项目，我会这样定位：

- shared log 仍然是 source of truth。
- PostgreSQL 是 query view。
- CDC 可以把 query view 推到分析系统。
- outbox 只有在改成 DB-first 架构时才进入主链路。

## Q673. S3 的 eventual consistency 对 snapshot/result_ref 有什么影响？

先说边界：如果使用 AWS S3，当前官方语义已经提供强 read-after-write 和 list consistency。但 LogServe 不是只面向 AWS S3，也可能接 MinIO、私有对象存储、网关层、缓存层或跨地域复制。工程上不应该假设所有 S3-compatible 存储都有完全相同的一致性表现。

如果对象存储存在读后短暂不可见，影响主要在三处。

第一，control 先 `Put` 大结果，再 append `StepSucceeded`。如果 log 里已经有 `result_ref`，但 replay 马上 `Get` 这个 ref 时对象还不可见，就会出现短暂恢复失败。

第二，actor snapshot 也一样。`ActorSnapshotCreated` 写进 log 后，另一个 worker 接管 actor，马上根据 `snapshot_ref` 读取 snapshot。对象读不到时，actor replay 会失败或需要重试。

第三，GC 更危险。mark-and-sweep 如果依赖 `List`，对象刚写入但 list 还没看到，GC 可能误判。反过来，刚删除的对象还在 list 里，也会影响清理统计。

处理方式比较明确：

- `Put` 成功后再 append log ref。
- `Get` ref 失败时区分临时错误和永久缺失，短时间重试。
- GC 使用安全窗口，不清理刚写入的对象。
- 不把 list 结果当成强实时事实。
- ref 里保存 hash 和 size，读到对象后做校验。

这样即便对象存储不是强一致，系统也不会把短暂不可见当成状态事实改变。

## Q674. 如果对象存储支持版本化，`result_ref` 设计是否可以简化？

可以简化一部分，但不能完全省掉 ref 元数据。

对象存储版本化能解决几个问题。对象被覆盖时，旧版本还在；对象被删除时，通常会产生 delete marker，而不是马上物理删除所有历史版本。`result_ref` 如果带上 object version id，就能准确指向写入时的那一版。

这对 LogServe 有用。workflow result 和 actor snapshot 都应该是不可变对象。ref 指向固定版本后，即使同一个 key 后来被覆盖，replay 仍然能读取旧结果。

但版本化不能替代这些字段：

- `sha256`
- `size_bytes`
- `encoding`
- `ref_schema_version`
- `key_id` 或加密相关元数据

原因是版本化只能说明“对象存储里有某个版本”，不能证明内容没有损坏，也不能告诉 replay 如何解码对象。S3-compatible 存储之间对 version id 的行为也可能有差异。

所以更稳的设计是：ref descriptor 里同时保存 URI、version id、hash、size 和 schema。版本化降低误删和覆盖风险，hash 负责内容校验，schema 负责长期兼容。

## Q675. 如果 object store 有生命周期删除策略，如何避免删除仍被 log 引用的对象？

不能让对象存储的通用生命周期策略直接按时间删除 LogServe 对象。

LogServe 的对象是否可删，取决于 shared log 和 metadata view 是否还引用它。比如一个 actor snapshot 可能很老，但 actor stream 已经 logical trim 到这个 snapshot。此时它仍然是恢复起点，不能删。

更安全的做法是把 lifecycle 交给 LogServe 自己生成。

可以按这些规则做：

- 对象写入时打 tag：tenant、workflow_id、actor_id、object_type、created_at。
- log 和 metadata 中有 ref 的对象标记为 live。
- 只有未被引用、超过 grace period 的对象才能进入删除队列。
- actor 当前 snapshot 和最近几个 snapshot 强制保留。
- physical compaction 前检查 snapshot 对象是否已经持久可靠。

如果用 S3/MinIO lifecycle policy，也应该把它放在“已归档/可删除”前缀上，而不是直接作用于所有 `logserve-results` 对象。比如 LogServe GC 先把确认可删的对象移动或标记到 `trash/` 前缀，再由对象存储生命周期策略异步清理。

核心原则是：log 还引用的对象，生命周期策略不能删。

## Q676. 如何实现 reference counting 或 mark-and-sweep 清理对象？

reference counting 的思路是每写入一个 `result_ref` 或 `snapshot_ref`，就在一张 ref 表里加计数；对象不再被引用时减计数，计数为 0 后删除。

这个方法看起来直接，但在 LogServe 里不太好做。原因是系统是 append-only log。事件不会被原地修改，workflow 和 actor 的引用变化也不是简单的“加一减一”。重试、重复 append、replay、logical trim 都会让计数变复杂。计数表一旦漂移，还要靠全量扫描修。

我更倾向 mark-and-sweep。

mark 阶段：

1. 扫描 shared log，收集所有事件里的 `result_ref` 和 `snapshot_ref`。
2. 扫描 metadata view，补充当前 workflow、step、actor 的 ref。
3. 根据 retention 策略判断哪些 ref 仍然 live。
4. 把 live ref 写入一张临时集合或 manifest。

sweep 阶段：

1. 扫描 object store 对象列表。
2. 对不在 live 集合里的对象，检查创建时间是否超过 grace period。
3. 超过安全窗口后删除。
4. 删除前记录 `ObjectDeleted` 或 GC report，方便审计。

当前 `objectstore.Store` 只有 `Put/Get`，还缺 `List/Delete/Stat`。要做 GC，接口需要扩展。也可以让对象写入时同步写 manifest 文件，GC 扫 manifest 而不是直接 list bucket。

## Q677. 如果数据需要加密，密钥管理放在哪？

密钥不能放在 shared log 里，也不能硬编码在 worker 或 control 配置里。

比较合理的分层是：

- shared log 只保存 ref、key id、算法、必要的 envelope metadata。
- object store 保存密文。
- KMS 或 Vault 保存主密钥。
- 每个对象使用数据密钥加密，数据密钥再由 KMS 包装。

对象写入时，control 或 worker 向 KMS 请求数据密钥，用它加密 result/snapshot。密文写入 object store，log event 只保存：

```json
{
  "ref": "s3://bucket/path/object",
  "sha256": "...",
  "key_id": "tenant-a/result-key",
  "encryption": "AES-GCM"
}
```

读取时，runtime 根据 `key_id` 调 KMS 解密数据密钥，再解密对象。worker 不应该拿到全局 root key；最好按 tenant、namespace、对象类型做最小权限。

密钥轮换也要提前设计。对象 ref 里要能表达它用的是哪把 key。旧对象可以继续用旧 key 读，也可以后台 re-encrypt 到新 key。GDPR 删除场景下，crypto shredding 也依赖这套设计：删除某个主体或 tenant 的 key，比在 append-only log 里物理删除所有记录更可控。

## Q678. 如果用户删除 workflow，日志、metadata、object store 应如何处理？

应该区分 soft delete 和 hard delete。

普通删除更适合 soft delete。control 先写一条 `WorkflowDeleted` 或 `WorkflowTombstoned` 事件到 workflow stream，再把 metadata view 标记为 deleted。dashboard 默认不展示 deleted workflow，但审计和 replay 仍然能解释它曾经存在过。

object store 里的 workflow result 不应该马上删。需要看 retention policy。比如保留 7 天方便恢复，或者等 GC 确认没有其他引用后再删。删除动作最好也写成事件或 GC report，避免日后不知道对象为什么没了。

hard delete 是另一回事。如果用户有合规删除要求，需要删除 object store 中包含个人数据的对象，metadata view 也要删除或脱敏。shared log 如果包含个人数据，就会和 append-only 设计冲突。更好的做法是从一开始就避免把敏感数据直接写入 log，只写 ref 和 hash。

所以我会这样设计删除：

- 默认 workflow delete 是 tombstone。
- object store 按 retention 和引用关系异步 GC。
- 含隐私数据的对象可 hard delete。
- log 里尽量不存隐私正文。
- 如果确实需要从 log 中移除敏感 payload，要走单独的 redaction/compaction 机制，并记录审计边界。

## Q679. 如果有 GDPR 删除要求，append-only log 与删除权如何冲突？

冲突点在于 append-only log 的价值是不可变历史，而 GDPR 删除要求强调个人数据可以被删除。两者天然有张力。

解决办法不是简单地说“日志永不删除”。生产系统通常会做数据最小化。

对 LogServe 来说，设计上应该尽量做到：

- shared log 不写用户原始 prompt、文档正文、个人身份信息。
- log 里只保存 id、状态、hash、ref、时间戳。
- 敏感内容放 object store，并按 tenant/subject 加密。
- metadata view 中的敏感字段可脱敏或 tombstone。
- 删除请求到来时，删除对象或删除密钥，让内容不可恢复。

这叫 crypto shredding。append-only log 还在，但里面只剩不可逆 hash、ref 和状态事件。只要 log 里没有个人数据正文，删除权压力就小很多。

如果历史 log 里已经写入了个人数据，就要引入 redaction log 或 physical compaction。比如追加 `DataErased` 事件让正常读路径隐藏敏感字段；后台 compactor 生成新的 segment，把敏感 payload 改成 tombstone。这样会削弱审计完整性，所以要记录红action report，并限制操作权限。

面试里可以坦白说：append-only log 和删除权不是天然兼容，需要靠数据最小化、对象化、加密和受控 compaction 来协调。

## Q680. 如何实现 tenant 级别的存储配额？

先要让所有资源都有 tenant 维度。

stream id 要带 tenant namespace，比如：

```text
tenant:<tenant_id>:wf:<workflow_id>
tenant:<tenant_id>:actor:<actor_id>
```

object store prefix 也要带 tenant：

```text
tenants/<tenant_id>/workflows/...
tenants/<tenant_id>/actors/...
```

metadata 表要有 `tenant_id`，并在 task、workflow、actor、model、worker、object ref 上统一使用。

配额可以分几类：

- log bytes
- object store bytes
- active workflow 数
- queued task 数
- running task 数
- actor 数
- model cache bytes
- 每秒提交速率

enforcement 放在 control plane。提交任务或 workflow 前，control 先读 tenant usage view。如果超过硬配额，直接拒绝，不写 log。接近软配额时可以接受但打 warning 或降低优先级。

usage view 可以从 log 和 object manifest materialize 出来。为了避免每次提交都全量扫描，需要维护 per-tenant counters。计数本身也要可重建，不能只存在内存。

对象存储层也要配合。MinIO/S3 可以按 bucket 或 prefix 做隔离；更强的隔离是每个 tenant 一个 bucket。bucket 多了管理复杂，prefix 简单但权限边界要自己做好。

## Q681. 如何为 result store 做幂等 `Put`？

当前本地和 S3 adapter 已经用了内容哈希命名。也就是说，同一份 data 会得到同一个 `<sha256>.json` key。这是幂等 Put 的基础。

如果同一个对象重复写入，路径相同，内容相同，结果 ref 也相同。对 workflow retry 或 completion 重试来说，这很好。

但还可以做得更严谨。

第一，写入前计算 hash 和 size。ref 里保存 hash。读回时重新校验。

第二，对 S3 使用条件写入。比如对象不存在时才写；如果对象已存在，就读元数据确认 hash 和 size 一致。一致则返回成功，不一致则报 conflict。

第三，hash 应该基于最终存储的字节。如果启用压缩或加密，必须明确 hash 是 plaintext hash 还是 ciphertext hash。两种都可以，但 ref descriptor 要写清楚。

第四，Put 不应该依赖随机 object key。随机 key 会让重试产生多个对象，后续只能靠 GC 清理。内容地址化能把重试天然收敛到同一个对象。

所以幂等 Put 的关键不是“重复调用不报错”，而是重复调用能得到同一个可校验 ref。

## Q682. 如何避免大对象写入阻塞 control plane？

当前 LogServe 的大结果 materialization 在 control plane 里同步完成。worker 调 `CompleteTask`，control 发现结果超过阈值，就调用 result store `Put`。对象写完后，control 才 append `StepSucceeded` 或 `WorkflowCompleted`。

这个路径简单，但对象存储慢时会拖住 control。大结果写入耗时会直接进入 workflow step completion latency。

可以有几种优化。

第一，worker 直接上传结果。worker 拿到预签名 URL 或受限凭证，把大结果写到 object store，再把 ref、hash、size 交给 control。control 校验对象存在后 append log。

第二，异步 materializer。control 先把 step 标记为 `RESULT_PENDING`，把对象写入任务交给后台 pool。对象写完后再 append `StepSucceeded`。这个设计更复杂，因为 workflow 下游 step 不能在结果可读前启动。

第三，限制 control 内的对象写入并发。用专门的 result-store worker pool，避免大量大对象 Put 把 gRPC handler 全部占住。

第四，对大对象使用 streaming 或 multipart upload，减少内存峰值。

第五，把 result store 和 control/worker 部署在同一区域或同一节点网络里，减少延迟抖动。

当前项目为了语义清楚，选择同步写。后续如果要跑大结果 benchmark，这会是很明显的优化点。

## Q683. 是否应该把 object store `Put` 下沉到 worker？会改变什么语义？

可以下沉，但语义会变。

现在是 worker 把 result bytes 交给 control，control 决定 inline 还是写 result store。好处是持久化策略集中，worker 不需要对象存储权限，control 能保证 `Put` 成功后再 append log ref。

如果下沉到 worker，worker 执行完任务后先把结果上传到 object store，再调用 `CompleteTask`，请求里带 `result_ref`、hash、size。这样 control 不用传输和写入大对象，completion 延迟可能更低。

代价是：

- worker 需要对象存储写权限。
- 用户代码所在环境和对象存储边界更近，权限要收紧。
- control 必须验证 ref，不能盲信 worker。
- completion 请求 schema 要扩展，支持 inline result 或 ref result。
- worker 上传成功但 CompleteTask 失败时，会产生 orphan object。

我会倾向折中做法：control 生成预签名 URL 或 upload token，worker 只能写某个 namespace 下的对象。worker 上传后，control 用 HEAD/Get 校验对象 hash 和 size，然后 append log。这样减轻 control 大对象写入压力，同时不把对象存储权限完全交给 worker。

## Q684. 如果 result store 高延迟，workflow p99 会如何变化？

workflow p99 会被 result store 延迟直接拉高，尤其是大结果 step 和 final result。

当前路径里，大结果 step 成功时，control 必须先完成 object store `Put`，然后才能 append `StepSucceeded`。下游 step 依赖这个成功事件，所以 object store 延迟会进入 critical path。

如果一个 workflow 有多个并行 step，p99 还会被最慢的那个大结果 Put 影响。fan-out 越大，遇到慢对象写入的概率越高。

actor snapshot 也会受影响。actor command 执行到 snapshot interval 时，control 写 snapshot 对象，再 append `ActorSnapshotCreated`。对象存储慢会让这个 command 的完成时间变长，后续 mailbox 命令也会排队。

可以用几个指标定位：

- result store Put latency p50/p95/p99
- workflow step completion latency
- time from worker finish to `StepSucceeded`
- object bytes per step
- result store error rate

缓解方法包括调高 inline threshold、worker-side upload、异步 materialization、对象写入池、同区域部署 MinIO/S3、对超大对象使用 multipart upload，以及对 result store 慢进行 backpressure。

## Q685. 如何对 PostgreSQL view 做 schema index 优化？

索引要围绕查询路径建，不要为了“看起来完整”把所有字段都索引。

当前比较常见的查询有几类。

task 查询：

- 按 status 查 queued/running/failed。
- 按 worker_id 查某个 worker 的任务。
- 按 workflow_id 和 step_id 查 workflow step task。
- 按 actor_id 查 actor command task。
- 按 created_at/updated_at 做时间排序。

workflow 查询：

- 按 status 查 running/failed/completed。
- 按 created_at 或 updated_at 分页。
- 按 idempotency_key 查重复提交。

actor 查询：

- 按 owner_worker_id 查 worker 拥有的 actor。
- 按 status 查 active/unavailable。
- 按 command_count 或 updated_at 找热点。

LLM 查询：

- 按 model_name、model_version 查请求。
- 按 worker_id 查模型服务情况。
- 按 completed_at 做时间范围统计。

可以加一些组合索引，比如 `(status, updated_at)`、`(workflow_id, step_id)`、`(actor_id, updated_at)`、`(model_name, model_version, completed_at)`。

JSONB 字段要谨慎。只有确实要按 `definition_json`、`state_json`、`labels` 内部字段查询时，才考虑 GIN index。否则会增加写放大。

PostgreSQL view 是高写入、高更新的场景，索引越多，upsert 成本越高。优化目标不是“索引多”，而是让 dashboard 和常用 query 不扫全表。

## Q686. 如何设计 metadata query 的分页和筛选？

不能让 dashboard 或 SDK 一次拉全量 metadata。现在 `ListTasks/ListWorkflows/ListActors` 在内存里全量返回，实验规模没问题，规模上来会吃内存和锁时间。

API 应该支持筛选和 cursor 分页。

task query 可以支持：

- status
- worker_id
- workflow_id
- actor_id
- model_name/model_version
- created_at range
- updated_at range
- limit
- cursor

workflow query 可以支持：

- status
- workflow_name
- created_at range
- completed_at range
- limit
- cursor

actor query 可以支持：

- owner_worker_id
- status
- class_name
- updated_at range
- command_count range

分页最好用 cursor，不用 offset。offset 在大表上会越来越慢，也容易在数据变动时跳页。cursor 可以用 `(created_at_ms, id)` 或 `(updated_at_ms, id)`。

还要支持 projection。dashboard 列表页不需要完整 `state_json`、`definition_json`、大 `result_json`。列表 API 返回摘要，详情 API 再读完整对象。

对 PostgreSQL 实现，筛选直接下推到 SQL。对 MemoryStore，可以先做简单过滤，但要避免持锁期间做太多排序和复制。更好的做法是把 query 接口下沉到 metadata.Store，而不是让 control 先 `ListAll` 再过滤。

## Q687. 如果 replay 出来的 view 和 PostgreSQL view 不一致，自动重建需要停服吗？

不一定，但要看 drift 的范围。

如果只是 PostgreSQL view 漂移，而 control 内存 view 仍然正确，可以在线修。做法是用 replay 结果生成一个 shadow view，写入临时表或直接 upsert 修正 PG 表。修复完成后再跑一致性检查。

如果 control 内存 view 也漂移，继续调度可能会扩大问题。比如 actor command_count 错了，新的 actor command 可能按错误序号执行。这种情况应让 control 进入维护模式：停止接收新提交，暂停调度，按 shared log 重建内存 view 和 PostgreSQL view，然后恢复服务。

还有一种是局部 drift。比如某个 workflow 的 PG row 不一致，但别的 stream 正常。可以做 per-stream repair：只 replay `wf:<workflow_id>`，修正对应 workflow row 和 steps row。

我会把自动重建设计成三个级别：

- read-only repair：不中断查询，只修 PG view。
- degraded repair：暂停新提交和调度，但允许查询。
- full restart rebuild：停止 control，清空或覆盖 view，从 shared log bootstrap。

是否停服不应该靠人工猜。`doctor` 工具要能给出 drift 类型：PG-only、control-memory、object-ref-missing、log-corruption。不同类型走不同修复流程。

## Q688. 如何用 checksum 或 event count 快速判断 view 是否 stale？

可以给每个 materialized stream 维护 watermark。

比如对每个 workflow、actor、task stream，在 metadata view 里记录：

- `last_applied_seq`
- `event_count`
- `last_event_type`
- `rolling_hash`
- `updated_at_ms`

logstore 的 `GetStreamStats` 可以提供 `next_seq`。如果 `last_applied_seq < next_seq - 1`，view 肯定落后。

event count 能做快速检查，但只能发现少应用或多应用事件，不能发现 payload 应用错了。rolling hash 更强一点。每应用一条事件，就把上一轮 hash、seq、event_type、payload hash 合起来算新 hash。replay 时重新计算一次，和 metadata 里的 rolling hash 对比。

对 workflow 和 actor 这种状态机，event count 加 rolling hash 很有用。它比每次深度比较完整 state 快，也能快速定位哪个 stream drift。

不过它不能替代全量一致性检查。原因是 object store 对象可能丢失，metadata hash 仍然对；PG row 可能被人手动改，hash 字段也可能没更新。所以快速 stale check 用来发现大部分漂移，定期还要做 full replay + object ref 校验。

## Q689. 如果部署在 Kubernetes，PV、emptyDir、hostPath 分别适合哪些数据？

要按数据是否可丢来选。

logstore 数据不能用普通 `emptyDir`。logd 的 segment log、index、retention metadata 是系统事实来源，应该放 PV。单机实验可以用 hostPath，但生产环境要用可靠的 PersistentVolume，最好再加备份或复制。

PostgreSQL 数据也要放 PV。虽然它是 materialized view，可以从 log 重建，但 PG 丢失会增加恢复时间，也会影响 dashboard 和查询。Compose 里用 volume；Kubernetes 里应使用 StatefulSet 加 PVC。

MinIO 数据要放 PV，或者直接用外部对象存储。workflow 大结果和 actor snapshot 依赖这些对象。用 `emptyDir` 会导致 Pod 重启后对象丢失，replay 可能断。

worker model cache 可以根据目标选择。实验环境可以用 `emptyDir`，Pod 重启后重新拉 checkpoint。为了模型 locality 和冷启动优化，可以用 hostPath 或 local PV，把 cache 留在节点上。hostPath 适合单机或受控实验，不适合跨节点迁移。

worker 临时执行目录、Python runner 临时文件、短期 stdout/stderr 缓冲可以用 `emptyDir`。这些数据丢了最多影响正在执行的 task，task 可以 redelivery。

简单归类：

- logstore：PV。
- PostgreSQL：PV。
- MinIO：PV 或外部对象存储。
- worker model cache：emptyDir、hostPath 或 local PV，取决于是否追求缓存保留。
- 临时执行文件：emptyDir。

## Q690. 如何为 logstore 和 object store 做灾备？

灾备要同时考虑 logstore 和 object store。只备 PostgreSQL 不够，因为 PostgreSQL 是 view。

logstore 灾备有几种层级。

最低层级是定期文件系统快照。备份 segment `.log`、index、`retention.json`。index 可以从 segment 重建，但一起备份能加快恢复。快照时最好停 logd 或使用支持一致性快照的存储层，避免拷到一半的 active segment。

更好的做法是远端复制。logd 每写一个 segment 或每隔一段时间，把 sealed segment 上传到远端对象存储。active segment 可以定期 checkpoint。恢复时先拉回 segment，再让 logstore recover 扫描并截断 partial tail。

生产级方案是多副本 log。用 Raft 或主从复制，让 append 成功至少表示多数副本持久化。这个项目当前还是单机 logd，所以实验报告里不能宣称具备这种容灾级别。

object store 灾备要启用 bucket 版本化、跨盘或跨地域复制、对象锁或保留策略。workflow result 和 actor snapshot 一旦丢失，log 里只有 ref，恢复会缺数据。

恢复顺序也很关键：

1. 恢复 logstore 到某个一致 checkpoint。
2. 恢复 object store 到不早于该 log checkpoint 的时间点。
3. 启动 logd。
4. 启动 PostgreSQL，可以空库。
5. 启动 control，让它从 log bootstrap metadata view。
6. 跑一致性检查，验证 ref 都可读。

如果 log 恢复到了 10:00，但 object store 只恢复到 9:50，中间 10 分钟写入的 result_ref 可能读不到。灾备计划要用“log seq + object manifest”做恢复边界，而不是只看时间。

