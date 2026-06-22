# 19. 对象存储、S3 语义、大对象 result reference 与数据生命周期

这一章讨论 workflow runtime 里很容易被低估的一条边界：控制面日志和元数据只应该记录状态、索引和引用，大对象本体要放到 result store、对象存储或文件系统边界里。对 LogServe 这样的系统来说，这不是“把 JSON 放哪里更方便”的小问题，而是 replay 成本、日志可维护性、恢复速度、生命周期治理和故障语义的问题。

下面的回答参考了 Amazon S3 官方文档、S3 Mountpoint 语义说明，以及 LogServe 当前的 `internal/objectstore`、workflow completion 和项目文档。面试时要把几件事分开讲：事件日志是不是可 replay，result reference 是不是稳定，S3 对单 key 的一致性和原子性是什么，对象 key 是否适合长期治理，以及对象存储到底能不能当作 append-only log 使用。

## Q001. 为什么大结果通常不直接放在元数据或事件日志里？

**回答：**

大结果不直接放在元数据或事件日志里，核心原因是控制面和数据面要分开。元数据和事件日志负责描述“发生了什么”，例如 task 是否完成、workflow step 是哪个、尝试次数是多少、结果在哪里。大结果本体可能是几 MB、几百 MB，甚至更大。把它塞进日志或数据库行里，会把原本轻量的控制面拖成一个低效的数据搬运通道。

可以先看一个 workflow step 完成事件。如果结果很小，事件里带一点 inline JSON 没问题：

```json
{
  "event_type": "StepSucceeded",
  "workflow_id": "wf-123",
  "step_id": "summarize",
  "result_json": {"answer": "ok"}
}
```

但如果结果是一份 300 MB 的模型输出、一批向量检索结果、一个压缩包、一个 actor snapshot，事件应该长这样：

```json
{
  "event_type": "StepSucceeded",
  "workflow_id": "wf-123",
  "step_id": "summarize",
  "result_ref": "s3://logserve-results/workflows/wf-123/steps/summarize/sha256....json",
  "result_size": 314572800,
  "result_sha256": "..."
}
```

事件日志最怕被大 payload 污染。原因有几个。

第一，写入路径会变慢。事件日志通常在任务完成、状态转移、恢复点确认这些路径上同步写入。大对象进入日志后，每次 append 都要承担序列化、压缩、网络传输、磁盘写入、fsync、复制和校验成本。原来一次状态事件可能只有几百字节，变成几百 MB 后，控制面吞吐和 p99 延迟都会被拖垮。

第二，replay 会变慢。LogServe 的一个核心思路是从 shared log replay workflow、actor 和 LLM 状态。replay 需要快速扫事件，恢复 materialized view。如果日志里塞了大结果，恢复时即使只想知道 step 是否成功，也要读取和跳过大量无关字节。结果是：控制面启动慢、故障恢复慢、dashboard materialization 慢，日志压缩和 retention 也变难。

第三，元数据存储不适合大 blob。PostgreSQL、SQLite、etcd、Redis 这类系统可以存二进制字段，但它们不是低成本大对象存储。大字段会放大 WAL、复制流、backup、vacuum、checkpoint、cache miss 和内存占用。你会发现数据库磁盘增长很快，但真正需要强查询语义的字段只有 workflow_id、step_id、status、attempt、result_ref 这几个。

第四，重复写入会放大故障成本。任务完成事件可能因为网络超时、控制面重试、worker 重投递而重复提交。大结果跟着事件重写，代价很高。用 result reference 时，结果对象可以用内容哈希或幂等 key 写一次；事件里只重复写一个短引用，幂等判断也更轻。

第五，生命周期不同。事件日志通常要按审计、replay、故障恢复策略保存；大结果可能只需要保留 7 天、30 天，或者按租户策略转到冷存储。把它们混在同一份日志里，会让生命周期策略互相牵制：为了 replay 留日志，就被迫保留大结果；为了删大结果，又影响日志完整性。

第六，权限和合规边界不同。元数据可能可以给运维、调度器、dashboard 看；大结果可能包含用户输入、模型输出、PII、业务文件或训练样本。把大结果放到对象存储，可以用 bucket policy、KMS、对象标签、生命周期规则、访问日志和预签名 URL 单独治理。日志里只留引用，泄漏面会小很多。

第七，事件日志应该保持“可解释”，而不是变成文件仓库。一个好的事件应该让人一眼看出状态转移：

```text
StepScheduled -> StepStarted -> StepSucceeded(result_ref=...)
```

如果 `StepSucceeded` 里嵌入 200 MB 结果，排障时反而更难看清楚真正的状态。

LogServe 当前就是按这个边界做的。`Service.materializeResult` 会根据 inline threshold 判断：小结果直接写进事件和 metadata；大结果通过 `objectstore.Store.Put` 写到本地或 S3-compatible store，再在 workflow 事件和 metadata 里保存 `result_ref`。`docs/report.md` 也明确写了：大结果和 actor snapshot 通过本地/S3-compatible MinIO 边界保存，日志只保留引用。

这里有一个容易被问到的反驳：日志不是 append-only 吗？既然日志本来就是写数据，为什么不能写大结果？

我的回答是：可以写，但要付出很高的系统代价。Kafka、Pulsar、S3-backed log、LSM storage 都可以承载较大的消息或 blob，但 workflow 控制面日志的目标不是当通用 blob store。控制面日志需要小、快、可 replay、可压缩、可索引。大对象存储需要高吞吐、低成本、生命周期规则、分层存储和按需读取。这是两套优化目标。

面试里可以这样答：

```text
大结果不直接放在元数据或事件日志里，是因为日志和 metadata 是控制面数据，应该小、稳定、容易 replay。大 payload 会放大 append、fsync、复制、数据库 WAL、backup、replay、dashboard 和 retention 成本，还会把结果生命周期和日志生命周期绑死。更合理的做法是：小结果 inline，大结果写入 result store 或 S3，事件里保存 result_ref、size、checksum、content type 和必要版本信息。LogServe 当前也是这个边界：workflow 日志保存 result_ref，result store 负责对象本体。
```

## Q002. result reference 模式解决什么问题？

**回答：**

result reference 模式解决的是“结果本体很大，但控制面只需要知道结果在哪里”的问题。它把结果写入对象存储、本地 result store、MinIO、S3 或其他 blob store，然后在事件日志、metadata、workflow state 里保存一个短引用。

最简单的结构是：

```text
result object:
  s3://logserve-results/workflows/wf-1/steps/step-2/<sha256>.json

metadata / event:
  result_ref = "s3://logserve-results/workflows/wf-1/steps/step-2/<sha256>.json"
  result_size = 10485760
  result_sha256 = "..."
  content_type = "application/json"
```

它解决的第一个问题是控制面瘦身。调度器、workflow replay、dashboard、metadata store 只传短字符串，不传大对象。绝大多数时候，系统只需要知道 step 是否成功、输出是否存在、下游 step 能不能开始。只有真正消费结果的下游任务，才需要 dereference。

第二个问题是失败恢复。事件日志可以稳定记录“结果已经 materialize 到某个引用”。控制面重启时，不需要重新执行上游 step，也不需要把大结果从日志读一遍。只要 `result_ref` 可读，下游就可以加载。LogServe 的 `completeWorkflow` 就有这种逻辑：如果 workflow result step 只有 `ResultRef` 而没有 inline `ResultJSON`，控制面会通过 result store 加载它，再决定最终 workflow result 怎么 materialize。

第三个问题是存储策略解耦。对象本体可以按对象存储策略管理：生命周期转冷、过期删除、版本化、对象锁、加密、跨区域复制、库存扫描。事件日志按 replay 和审计策略保留。两个生命周期可以不同，只要系统明确：某些老 workflow 的 `result_ref` 可能已经过期，过期后只能看到 metadata，不能再取回完整结果。

第四个问题是幂等和去重。LogServe 本地 `LocalStore.Put` 和 S3 store 都用 `sha256(data)` 生成对象名。这意味着相同内容可以映射到相同 key，重复写入不会在逻辑上产生多个不同引用。真实生产系统还可以加上 `If-None-Match: *` 做条件写，避免多个 writer 覆盖同一 key。

第五个问题是跨存储后端。一个引用可以有 scheme：

```text
local://workflows/wf-1/steps/a/sha256.json
s3://logserve-results/workflows/wf-1/steps/a/sha256.json
minio://logserve-results/workflows/wf-1/steps/a/sha256.json
```

控制面不需要知道底层是本地文件、MinIO、S3 还是以后换成别的对象存储。它只依赖 `Store.Put` 和 `Store.Get` 接口。这个边界对面试很有价值：它说明你没有把对象存储细节泄漏到 workflow 状态机里。

但 result reference 不是魔法，它引入了新的故障边界。

最重要的是写入顺序。通常应该先写对象，再写事件：

```text
worker/control receives result
  -> Put object to result store
  -> get result_ref
  -> append StepSucceeded(result_ref) to log
  -> update metadata view
```

如果先写事件再写对象，控制面或下游任务可能看到一个还不存在的 `result_ref`。如果先写对象再写事件，事件写入失败时会留下 orphan object。两者都不完美。工程上通常选择“先对象后事件”，因为 orphan object 可以通过生命周期、mark-and-sweep 或按 namespace 扫描清理；但一个指向不存在对象的成功事件会破坏 correctness。

第二个边界是引用必须足够稳定。只保存 `s3://bucket/key` 有时不够。更稳的引用还会包含：

```text
bucket/key:
  找到对象。

version_id:
  bucket 开启 versioning 时，固定读取某个版本。

etag/checksum:
  验证读回内容没有变。

size:
  快速做 sanity check。

content_type / encoding:
  下游知道如何解析。

created_at / expires_at:
  帮助判断生命周期。

producer attempt:
  排查重复执行和重试。
```

第三个边界是权限。下游 task 有 `result_ref`，不代表它应该能读对象。多租户系统要把租户、workflow、namespace、IAM policy、KMS key 放在一起设计。否则一个日志可见的人可能通过引用读到不该读的结果。

第四个边界是引用过期。对象生命周期规则可能会删除结果；Glacier 类存储可能要求 restore 后才能读；跨区域复制可能有延迟。系统要决定：过期后的 workflow status 是报 `result expired`，还是保留一个 compact summary，或者要求用户重新执行。

第五个边界是小结果是否也要走对象存储。不是所有结果都要 object store。小结果 inline 更方便：状态页能直接展示，replay 不用额外 GET，下游参数绑定更简单。LogServe 用 inline threshold 做折中，这个选择合理。面试里可以说：不要为了“架构统一”把 50 字节结果也强制写 S3，否则会把 S3 请求延迟和成本引进每个小任务。

一个比较完整的 result reference 可以这样定义：

```json
{
  "uri": "s3://logserve-results/workflows/wf-1/steps/extract/7f...a9.json",
  "version_id": "3HL4kqtJlcpXroDTDmJ+rmSpXd3dIbrHY+MTRCxf3vjVBH40Nrjfkd",
  "size_bytes": 8388608,
  "sha256": "7f...a9",
  "content_type": "application/json",
  "encoding": "gzip",
  "created_at_ms": 1760000000000,
  "expires_at_ms": 1762592000000
}
```

LogServe 当前实现比这个更简化：`Store.Put` 返回字符串形式的 `local://...` 或 `s3://bucket/key`，没有把 version_id、checksum、content_type 都放进 protobuf。这对机制验证足够，但面试要能说出生产化还要补哪些元数据。

面试里可以这样答：

```text
result reference 模式把大结果本体放到对象存储，事件日志和 metadata 只保存引用。它解决了日志膨胀、metadata 大字段、replay 慢、重复写入昂贵、生命周期混乱和存储后端耦合的问题。正确实现时要先写对象再写成功事件，引用最好包含 uri、size、checksum、version、content type 和过期信息。它也有新风险：对象写成功但事件失败会产生 orphan object；事件引用的对象被删会产生 broken ref；多租户下还要控制谁能 dereference。LogServe 当前实现的是这个模式的简化版。
```

## Q003. 对象存储和本地文件系统的语义差异是什么？

**回答：**

对象存储和本地文件系统最容易被误用的地方，是它们看起来都能“按路径存文件”。S3 key 可以写成 `workflows/wf-1/result.json`，本地路径也可以写成 `workflows/wf-1/result.json`。但这两个东西的语义完全不一样。

本地文件系统通常是层级结构。目录是真的目录，文件有 inode、权限、mtime、硬链接、符号链接、打开的文件句柄、rename、truncate、append、fsync、文件锁。应用可以打开一个文件，写一段，seek 到中间改一段，再继续 append。只要在同一个文件系统内，`rename` 往往可以作为原子发布手段。

对象存储的基本模型是 bucket + object key + object data + metadata。S3 文档明确说对象由 key 唯一标识；prefix 和 delimiter 可以让控制台或 SDK 推断出“文件夹”的样子，但底层没有真正的 POSIX 目录层级。`a/b/c.json` 只是一个 key，不代表一定存在 `a` 和 `a/b` 这两个目录。

差异可以这样拆：

```text
命名:
  文件系统有目录树和路径。
  S3 是 bucket 内的 flat key space，斜杠只是 key 字符。

写入:
  文件系统支持 open/write/append/truncate，写入期间有文件句柄。
  普通对象存储更接近 PUT whole object 或 multipart complete 后生成对象。

修改:
  文件系统可以原地修改一段字节。
  S3 general purpose bucket 的常见模式是重写整个对象，或者写新 key 再切引用。

rename:
  本地同文件系统 rename 通常是原子的。
  S3 general purpose bucket 没有通用原子 rename；常见做法是 copy + delete，不是单个原子操作。

锁:
  本地文件系统可以有 advisory lock 或强一些的文件锁语义。
  S3 不提供通用 POSIX file locking；并发 writer 要靠应用层条件写、lease 或数据库协调。

目录:
  文件系统目录可以为空存在。
  S3 “目录”通常来自 prefix；控制台创建文件夹时可能是一个 zero-byte object。

权限:
  文件系统常见权限是 uid/gid/mode/ACL。
  S3 主要用 IAM、bucket policy、access point、object ownership、KMS、tag 条件。

一致性:
  本地文件系统一致性受内核、page cache、fsync、挂载方式影响。
  S3 对对象 PUT/DELETE/GET/LIST 提供强 read-after-write，但单 key 原子性不等于跨 key 事务。
```

S3 的一致性现在不能再按旧知识说成“新对象读后写强一致、覆盖和 LIST 最终一致”。AWS 当前文档说，S3 在所有 Region 对对象 PUT 和 DELETE 提供 strong read-after-write consistency；成功 PUT 后，随后的 GET 或 LIST 能看到写入。对单个 key 的更新是原子的：并发 GET 要么读到旧对象，要么读到新对象，不会读到半个旧对象加半个新对象。

但这个强一致不是数据库事务。AWS 文档也写得很清楚：没有跨 key 原子更新；两个 writer 同时 PUT 同一个 key 时，最终谁赢不能靠业务猜，通常是 last-writer-wins，应用如果在意要自己实现锁或条件写。也就是说：

```text
S3 可以保证:
  单个 key 的 PUT 成功后，GET/LIST 能看到结果。
  覆盖单个 key 时，读者不会看到损坏的半对象。

S3 不保证:
  key A 和 key B 同时原子更新。
  多 writer 对同一个 key 自动串行化成业务期望顺序。
  rename 目录树。
  像本地文件那样边写边被另一个进程读到完整一致的文件句柄语义。
```

AWS 的 Mountpoint for S3 文档也把这个边界讲得很直接。Mountpoint 可以让应用用文件接口读写 S3 对象，但它不是完整 POSIX 文件系统。官方文档说明它可以列出和读取已有文件、创建新文件，但不能修改已有文件或删除目录，也不支持符号链接或 file locking。Mountpoint 的语义说明还强调：对 general purpose bucket，它不会去模拟低效的 POSIX rename；写新对象通常要从头顺序写。

这对工程实现有几个直接影响。

第一，不要把 S3 当本地临时目录用。很多本地写法是：

```text
write temp file
fsync temp file
rename temp -> final
```

在本地文件系统里这是常见原子发布模式。放到 S3 general purpose bucket 上就不能照搬。更合理的是写一个唯一 key，例如带 attempt_id 或 content hash；写成功后再用元数据、manifest 或事件日志发布这个 key。

第二，不要依赖空目录。`workflows/wf-1/` 这个 prefix 下没有对象时，它在 S3 里就像不存在。生命周期规则、权限规则、扫描任务都应该按 prefix 和 tag 设计，而不是按目录 inode 设计。

第三，不要用 LIST 当强事务索引。S3 LIST 现在对对象写入是强一致的，但它仍然是对象存储的列举接口，不是数据库索引。海量对象、分页、prefix 过滤、权限过滤、成本和延迟都要考虑。控制面最好保存 result_ref 和必要索引，不要每次靠 LIST 找 workflow 的最终结果。

第四，不要假设本地缓存视图永远最新。Mountpoint 或应用层 cache 可能为了性能缓存 metadata 或对象内容。AWS Mountpoint 语义说明提到，启用缓存后 strong read-after-write 视图会放松，可能看到缓存数据。对象存储本身强一致，不代表你所有客户端缓存都强一致。

第五，路径清洗很重要。对象 key 可以包含很多 UTF-8 字符，但一旦你把它映射到本地路径、URL、日志或安全策略里，`..`、`.`、反斜杠、空格、尾随点、URL 编码都可能出问题。LogServe 的 local store 里有 `cleanNamespace`，就是为了避免 `../` 这类路径逃逸。

面试里可以这样答：

```text
本地文件系统是 POSIX 风格的目录树，有 open/write/append/rename/fsync/lock/inode 等语义；对象存储是 bucket + key 到 object 的映射，prefix 只是命名约定。S3 对单 key PUT/DELETE/GET/LIST 提供强一致，单 key 覆盖是原子的，但没有跨 key 事务，也没有 general purpose bucket 上的通用原子 rename、file lock 和原地修改。工程上不要把 S3 当本地目录用，应该写不可变对象，用 result_ref、manifest、条件写和生命周期规则管理。
```

## Q004. S3 的对象 key 设计需要考虑哪些因素？

**回答：**

S3 key 不是随便拼一个路径字符串。它同时承担寻址、分组、权限、生命周期、性能、排障和成本治理的作用。key 设计得好，后续清理、扫描、审计、排障都顺；设计得差，系统上线后会到处补索引、补迁移脚本。

先说官方限制。S3 object key 是 bucket 内对象的唯一标识。AWS 文档说 key 可以使用 UTF-8 字符，最大长度是 1,024 bytes。注意是 bytes，不是字符数。中文、emoji、某些符号会占多个字节。key 是大小写敏感的，`Result.json` 和 `result.json` 是两个对象。

key 设计首先要考虑可读性和稳定性。一个 LogServe result object 可以长这样：

```text
workflows/<workflow_id>/steps/<step_id>/attempts/<attempt>/results/<sha256>.json
```

或者更偏内容寻址：

```text
results/sha256/7f/7f3a...c9.json
```

两种设计侧重点不同。

按业务层级组织，排障友好：

```text
workflows/wf-123/steps/extract/attempts/2/result.json
```

优点是人容易看，prefix 可以直接按 workflow 清理。缺点是如果同一个 step 重试覆盖同一个 key，就要处理并发和幂等；如果 workflow_id 含敏感信息，也会泄漏到 key。

按内容哈希组织，幂等友好：

```text
objects/sha256/7f/7f3a...c9.json
```

优点是相同内容天然去重，不容易覆盖。缺点是只看 key 不知道属于哪个 workflow，需要 metadata 或事件日志反查。

生产里常见的是两者结合：

```text
tenant=<tenant_id>/date=2026-06-19/workflows/<workflow_id>/steps/<step_id>/<sha256>.json
```

但要小心，不要把 PII、用户 prompt、文件名原文、邮箱、手机号、身份证号直接塞进 key。S3 key 会出现在访问日志、CloudTrail、错误消息、监控、账单分析、预签名 URL 和下游系统里。key 应该是标识符，不应该是敏感内容本体。

第二，要考虑生命周期。S3 Lifecycle 通常按 bucket、prefix、tag 过滤。你如果希望 workflow 中间结果 7 天删除、最终结果 30 天删除、审计快照 1 年转 Glacier，那 key 或 tag 必须能表达这个分类。例如：

```text
tmp/workflows/...       7 天删除
results/workflows/...   30 天删除
snapshots/actors/...     90 天后转 IA，1 年后转 Glacier
audit/...                Object Lock 或更长保留
```

如果所有对象都平铺在 `objects/<uuid>`，生命周期规则就只能靠 object tag 或外部索引。tag 也可以，但写入时必须保证 tag 完整，否则后面清理会漏。

第三，要考虑访问控制。S3 bucket policy 可以按 prefix 授权。例如只允许某个 worker role 写 `workflows/*/steps/*`，只允许某个 tenant role 读 `tenant/<tenant_id>/...`。如果 key 里没有租户或业务边界，策略会变得很难写。反过来，key 里放太多业务细节也会泄漏信息。

第四，要考虑性能和热点。AWS 现在会自动扩展 S3 request rate，官方文档给的基线是每个 partitioned prefix 至少 3,500 PUT/COPY/POST/DELETE 或 5,500 GET/HEAD 每秒，并且 bucket 中 prefix 数量没有固定上限。早年那种必须把 key 前缀随机打散的经验，不能机械套用到今天。

但这不等于 key 设计完全不管热点。如果所有 writer 都往同一个小 prefix 下写超高 QPS 小对象，S3 扩展是逐步发生的，可能遇到 503 Slow Down。高并发系统可以按 tenant、日期、workflow_id、hash 前缀分散写入：

```text
results/date=2026-06-19/shard=7f/workflows/...
```

第五，要考虑列表和分页。S3 LIST 是按 prefix 列举，不是 SQL 查询。你要经常查“某个 workflow 的所有 step 输出”，就把 workflow_id 放在靠前 prefix。你要经常按日期清理，就把日期放在靠前 prefix。你要按 tenant 做账单和清理，就把 tenant 放在靠前 prefix 或 tag。不要设计一个只能靠全 bucket scan 才能回答日常问题的 key 空间。

第六，要考虑字符兼容性。AWS 文档列了一些相对安全的字符：字母、数字、`!`、`-`、`_`、`.`、`*`、`'`、`(`、`)`。斜杠可以用来表达 prefix，但它仍然是 key 的一部分。需要特别避开或谨慎处理：

```text
相对路径段:
  . 或 .. 可能被工具、代理、文件系统适配层规范化。

反斜杠:
  Windows 路径和 URL 处理容易混淆。

尾随点:
  S3 console 下载时可能移除 key 末尾的句点。

空格和特殊字符:
  签名、URL 编码、shell 脚本、日志解析容易出错。

过长 key:
  1,024 bytes 限制；多字节字符更容易超。
```

第七，要考虑版本和幂等。如果开启 S3 Versioning，`bucket + key + version_id` 才能唯一定位某个版本。只保存 key，后续覆盖后可能读到新版本。对 result reference 来说，如果对象应该不可变，最好使用不会覆盖的 key；如果确实要覆盖，引用里要保存 `version_id` 或 ETag。

第八，要考虑对象格式。key 后缀不是必须，但有实际用处：

```text
.json:
  方便人工判断和 Content-Type 设置。

.json.gz:
  表明压缩格式，减少误读。

.bin:
  不承诺文本语义。

.manifest.json:
  表示这是清单，不是数据本体。
```

第九，要考虑临时对象和提交协议。多步骤写入时，不要把未完成对象放在最终 key 上。常见做法是：

```text
tmp/<request_id>/<uuid>.part
  -> 校验完成
  -> publish manifest or final result_ref
```

对于 S3 general purpose bucket，发布 final result 最好是写一个不可变 final object，或者在 metadata/log 里发布 final key。不要依赖 copy + delete 伪装成原子 rename。

对 LogServe 当前实现，可以这样评价：本地和 S3 store 都用 `namespace + sha256(data).json`。这在机制验证里很好，因为 key 可预测、幂等、不会把大结果直接塞日志。生产化可以继续补几个字段：tenant prefix、content type、version_id、object tag、checksum 字段、TTL 或 lifecycle class。

面试里可以这样答：

```text
S3 key 设计要同时考虑唯一性、可读性、生命周期、权限、性能、列表方式、字符兼容和幂等。key 最大 1,024 bytes，prefix 不是真目录但会影响 LIST、策略和生命周期。业务上可以按 tenant/date/workflow/step 组织，也可以按 sha256 内容寻址；不要把 PII 或 prompt 原文放进 key。高 QPS 时要关注 prefix 热点和 503 Slow Down，清理和归档时要让 prefix 或 tag 能表达生命周期。对 result_ref，最好用不可变 key，并记录 size、checksum、content type，开启 versioning 时还要保存 version_id。
```

## Q005. 对象存储是否支持原子 append？

**回答：**

答案要分层说。传统对象存储、S3 general purpose bucket、绝大多数 S3-compatible 存储，都不应该被当成支持 POSIX 原子 append 的文件系统。对象存储的基本写入模型是 PUT 一个对象，或者用 multipart upload 上传多个 part，最后 complete 成一个对象。它擅长写入完整对象和读取完整对象或 range，不擅长多个 writer 同时对同一个对象尾部追加。

S3 对单 key 的 PUT 是原子的。一个 writer 覆盖对象时，读者要么看到旧对象，要么看到新对象，不会看到半个对象。AWS 文档也明确说，更新是 key-based，没有跨 key 原子更新；并发写同一个 key 时，如果没有应用层协调，通常是最后写入者获胜。这个“单 key 覆盖原子性”和“append 原子性”不是一回事。

multipart upload 也不是 append。它的语义是：

```text
CreateMultipartUpload
  -> UploadPart(1)
  -> UploadPart(2)
  -> ...
  -> CompleteMultipartUpload
  -> S3 组装出一个新对象
```

在 complete 之前，part 不是最终可见对象。complete 之后，对象作为整体出现。你可以用 multipart 解决“大对象上传”和“失败后重传某些 part”的问题，但它不是对一个已经存在的对象执行 append。要给已有对象追加内容，传统做法通常是读旧对象、生成新对象、再用新引用发布；或者写新 segment，而不是改旧对象。

不过，2026 年回答这个问题不能忽略 S3 的新特例。AWS 当前 `PutObject` API 里有 `x-amz-write-offset-bytes` 请求头，用于向已有对象追加数据。官方文档写明：offset 必须等于现有对象大小；如果对象不存在，offset 为 0 可以创建新对象。同时文档也写明，这个能力只支持 Amazon S3 Express One Zone storage class 中 directory bucket 的对象。

所以准确表述是：

```text
S3 general purpose bucket:
  不提供通用原子 append。用 PUT 覆盖对象或 multipart 创建对象。

S3 Express One Zone directory bucket:
  支持基于 x-amz-write-offset-bytes 的受限 append。

S3-compatible MinIO / 其他对象存储:
  不能默认假设支持这个 AWS 特性，要查具体实现。
```

即使使用 S3 Express One Zone 的 append，也不能把它简单等同于多 writer WAL。原因是 append 需要调用方知道正确的 offset。多个 writer 同时追加同一个对象时，必须有人协调当前长度、写入顺序、失败重试和幂等。否则两个 writer 可能基于同一个旧长度发请求；一个成功后，另一个 offset 就不匹配，需要重读长度、重新计算和重试。这更像 compare-and-append，而不是数据库日志服务里的自动多 writer append。

真正要实现日志，常见设计不是“所有 writer append 到一个 S3 object”，而是：

```text
segment 模式:
  每个 writer 或 shard 写独立 segment object。
  segment 完成后写 manifest 或 index。
  读取时按 manifest 顺序拼接。

事件日志服务:
  用 Kafka、Pulsar、NATS JetStream、数据库 WAL 或专门 log service 处理 append。
  对象存储只保存封存后的 segment、snapshot 或 backup。

manifest 发布:
  新数据写到不可变对象。
  用条件写发布 manifest，或者在数据库/log 里记录新对象引用。

compaction 模式:
  多个小对象后续 compact 成较大对象。
  老对象由生命周期规则清理。
```

这里可以联系 LogServe。LogServe 的 shared log 是单独实现的 append-only log 服务，负责事件顺序、幂等 append、replay 和 logical trim。对象存储边界用于 result store、actor snapshot、checkpoint artifact 这类大对象。这个划分是正确的：不要让 S3 result store 承担 workflow event log 的 append 语义。S3 可以保存 sealed log segment，但不应该替代需要严格顺序和低延迟确认的控制面 append。

如果面试官追问“那 S3 的条件写能不能实现 append”，可以这样回答：条件写能防止覆盖或做 ETag 检查。`If-None-Match: *` 可以保证某个 key 不存在时才创建；`If-Match` 可以保证对象还是某个 ETag 时才覆盖。这些可以构建乐观并发控制，但仍然不是通用 append API。你要自己处理读当前版本、生成新对象、条件发布、冲突重试。对于高并发日志，这通常不划算。

如果追问“追加日志能不能一个对象一个 record”，答案也要谨慎。一个 record 一个对象确实避免 append，但会产生大量小对象，LIST、生命周期、请求成本和 compaction 都会变成问题。可行做法是按 shard 和时间窗口写 segment，比如：

```text
logs/workflow-events/date=2026-06-19/shard=07/segment-000001.jsonl
logs/workflow-events/date=2026-06-19/shard=07/segment-000002.jsonl
```

segment 写满后封存，再更新 manifest。这样对象存储做它擅长的事情：存大块不可变对象。

面试里可以这样答：

```text
一般对象存储不支持 POSIX 意义上的原子 append。S3 general purpose bucket 的 PUT 是单 key 覆盖原子，multipart upload 是组装新对象，不是追加已有对象。AWS 当前在 S3 Express One Zone 的 directory bucket 上提供 `x-amz-write-offset-bytes`，可以按对象当前大小做受限 append，但这个能力不等于通用多 writer WAL，且不能假设 MinIO 或所有 S3-compatible 存储都支持。工程上更稳的做法是用专门日志系统处理 append，把对象存储用于 sealed segment、snapshot、result object 和 manifest。
```

## Q006. multipart upload 解决什么问题？

**回答：**

multipart upload 解决的是“大对象上传不能像小对象一样一次 PUT 搞定”的问题。它把一个对象拆成多个 part，每个 part 可以独立上传、并行上传、失败后单独重传，最后用 `CompleteMultipartUpload` 把这些 part 组装成一个对象。

AWS 官方文档把流程拆成三步：

```text
CreateMultipartUpload:
  初始化上传，拿到 upload_id。

UploadPart:
  按 part_number 上传每个 part。
  每个 part 上传成功后，S3 返回该 part 的 ETag/checksum 信息。

CompleteMultipartUpload:
  提交 part_number + ETag 列表。
  S3 按 part_number 升序拼接，生成最终对象。
```

这个机制主要解决四类问题。

第一，提升吞吐。单个大对象如果只用一个 HTTP 连接上传，吞吐受单连接、单线程、TCP 窗口、客户端磁盘读取和网络波动影响。multipart upload 可以让客户端并行上传多个 part，把可用带宽吃满。比如一个 100 GB 文件，可以拆成 1000 个 100 MB part，并行上传，再 complete。官方文档也给过类似 100 GB、1000 个 part 的例子。

第二，降低失败重传成本。普通单次 PUT 上传到 99% 时断网，通常要从头来。multipart upload 中，如果第 713 个 part 失败，只需要重传这个 part。对跨地域网络、移动网络、长时间上传、大模型 checkpoint、训练数据集、备份文件，这个差别很大。

第三，支持暂停和恢复。multipart upload 初始化后会有 `upload_id`。只要 upload 没有 complete 或 abort，就可以继续上传剩余 part。你可以记录已经成功的 part，进程重启后通过自己的状态或 `ListParts` 做校验，然后继续。注意：官方文档说 multipart upload 本身没有自动过期；如果不 complete 或 abort，已上传 part 会继续占存储并计费。

第四，支持边生成边上传。有些结果不是一开始就知道总大小，比如压缩流、导出任务、离线 batch result、模型训练 checkpoint 打包。multipart upload 允许你在生成数据的同时上传 part，最后再 complete。它不是 append API，但对“最终对象很大、生成过程很长”的场景很实用。

对象存储里，multipart upload 还有一个重要语义：在 complete 之前，最终对象不可见。part 已经存在并计费，但不是普通 `GET bucket/key` 能读到的对象。只有 complete 成功后，S3 才把它作为完整对象发布。这个语义对 result reference 很重要：不要在 multipart 还没 complete 时把 `result_ref` 写进 workflow 成功事件。

可以用一条时序说明：

```text
worker 生成大结果
  -> CreateMultipartUpload(key)
  -> UploadPart(1)
  -> UploadPart(2)
  -> UploadPart(3)
  -> CompleteMultipartUpload(parts)
  -> 得到最终对象 ETag/checksum/version
  -> append StepSucceeded(result_ref=...)
```

这个顺序不能倒。`result_ref` 是对最终对象的承诺，不是对一堆临时 part 的承诺。否则下游 step 可能看到一个还没完成的对象。

multipart upload 也有边界。

首先，它不是事务。多个客户端可以对同一个 key 发起多个 multipart upload。开启 versioning 时，每个 complete 可能产生一个新版本；未开启 versioning 时，其他 PUT、DELETE 或 complete 可能影响最终可见结果。官方文档也提醒：并发 multipart upload 到同一 key 时，最终 current version 的判断和发起时间、版本化状态有关。业务上应该避免多个 writer 写同一个 result key，或者使用条件写、唯一 key、内容哈希 key。

其次，它不是数据校验的全部。每个 part 有自己的 ETag/checksum，最终对象也可以有 checksum。你应该记录 part_number、part ETag、算法、最终 checksum。不要只凭“complete 成功”就认为下游一定拿到业务上正确的数据。S3 会做传输和存储完整性校验，但业务层仍然要知道对象是不是对应当前 workflow attempt。

第三，它不是小对象优化。对 10 KB、100 KB 的结果强行 multipart，只会增加 API 调用、状态机和失败面。AWS 文档建议对象达到 100 MB 左右时考虑 multipart upload。实际阈值要看网络、延迟、对象大小分布、SDK transfer manager 配置。LogServe 当前 result store 只是简化的 `Put(ctx, namespace, data)`，还没有 multipart 逻辑；这对机制验证足够。生产化时，大结果 result store 可以在超过阈值后切换到 multipart。

第四，part 设计要谨慎。S3 multipart 有一些硬限制：part number 是 1 到 10,000；除最后一个 part 外，part size 要满足最小限制；part 太小会导致 API 调用多、complete 清单大，part 太大又降低失败重传收益和并行度。工程上常见做法是 64 MB、128 MB、256 MB 一类的 part size，再按对象大小和网络带宽调。

第五，complete 请求要用自己记录的 part 列表。AWS 文档明确提醒，不要把 `ListParts` 的返回结果直接当作 complete 请求输入；complete 时应该使用客户端在上传每个 part 后记录的 part_number 和 ETag。`ListParts` 更适合恢复时做核对。原因很现实：分布式上传、重传同一个 part number、并发上传和分页都可能让“我以为的 part 列表”和“最终应该提交的 part 列表”不一致。

LogServe 语境下，可以这样设计大 result 上传：

```text
result size <= inline_threshold:
  直接写进 event/metadata。

inline_threshold < result size < multipart_threshold:
  普通 PutObject，返回 result_ref。

result size >= multipart_threshold:
  multipart upload。
  complete 成功后才写 StepSucceeded(result_ref)。

complete 失败:
  不写成功事件。
  abort upload 或交给 lifecycle 清理。
```

面试里可以这样答：

```text
multipart upload 解决大对象上传的吞吐、失败恢复和长时间上传问题。它把对象拆成多个 part，每个 part 独立上传、可并行、失败后只重传该 part，最后 CompleteMultipartUpload 按 part_number 组装成一个对象。complete 前最终对象不可见，part 会占存储并计费。它适合大 result、checkpoint、备份和导出文件，不适合小结果；也不是事务或 append API。对 result_ref 来说，必须 complete 成功后才能把引用写进事件日志。
```

## Q007. multipart upload 失败后如何清理残留 part？

**回答：**

multipart upload 失败后的核心问题是：part 不是最终对象，但会继续占用存储并计费。只要 upload 已经 initiated，并且上传过一个或多个 part，如果没有 complete 或 abort，S3 会保留这些 part。它们不会出现在普通对象列表里，却会形成账单和清理负担。

清理方式有两种：主动 abort 和生命周期兜底。

主动 abort 是请求路径上的清理。上传失败、业务取消、checksum 不匹配、complete 失败、worker 发现 attempt 已被 fencing 掉时，客户端应该调用 `AbortMultipartUpload`。这会停止该 upload，并删除已经上传的 part。AWS 文档也说，如果不想继续完成 multipart upload，就应该 abort；只有 complete 或 abort 后，S3 才释放 part storage。

典型流程是：

```text
upload_id = CreateMultipartUpload(key)
try:
  UploadPart(...)
  UploadPart(...)
  CompleteMultipartUpload(...)
except:
  AbortMultipartUpload(bucket, key, upload_id)
  raise
```

但真实系统里，`finally abort` 不是万能的。进程可能崩溃，机器可能断电，Kubernetes pod 可能被 kill，网络可能在 abort 请求发出前断掉。还有一种情况：你发了 abort，但某些 part upload 请求还在路上。AWS 文档提醒，part uploads in progress 在 abort 后仍可能成功或失败；为了释放所有 part，最好在所有 part upload 都结束后再 abort，或者之后再检查。

所以需要第二层：bucket lifecycle 规则。S3 支持 `AbortIncompleteMultipartUpload` lifecycle action，可以让没有在指定天数内完成的 multipart upload 自动变成可 abort 状态，S3 会停止 upload 并删除相关 part。官方示例里常见是 `DaysAfterInitiation: 7`。这条规则只影响未完成的 multipart upload，不会删除已经 complete 的对象。

一个生产系统通常这样做：

```text
同步路径:
  业务失败时尽量 AbortMultipartUpload。

后台清理:
  周期性 ListMultipartUploads，找出超过阈值的 upload_id。
  对确认不再需要的 upload 调 AbortMultipartUpload。

S3 lifecycle 兜底:
  配置 AbortIncompleteMultipartUpload，例如 1 天、3 天或 7 天。
  防止进程崩溃后长期遗留 part。
```

阈值怎么定，要看业务。如果是用户上传文件，保留 7 天让客户端恢复可能合理。如果是 worker 内部生成 result object，失败后一般不需要保留很久，1 天甚至几小时就够。对于高吞吐任务系统，残留 part 很容易被忽略，因为它们不在普通 `ListObjectsV2` 结果里。要看 `ListMultipartUploads`、S3 Storage Lens、账单、CloudWatch 或自己的 upload 状态表。

还要处理权限。Abort 需要 `s3:AbortMultipartUpload`。List upload 需要 `s3:ListBucketMultipartUploads`，List parts 需要 `s3:ListMultipartUploadParts`。如果 worker 只有 `s3:PutObject`，它可能能上传 part，却不能清理失败 upload。这个权限设计很危险。生产上至少要让发起 multipart 的 role 有 abort 权限；如果由清理服务统一回收，还要给它 list 和 abort 权限。

在 result reference 模式里，失败清理和事件顺序要配套。一个安全策略是：

```text
CreateMultipartUpload:
  记录 upload_id、key、workflow_id、step_id、attempt、created_at。

UploadPart:
  记录 part_number、ETag、size、checksum。

CompleteMultipartUpload 成功:
  写 StepSucceeded(result_ref)。
  删除 upload session 状态。

Complete 失败或业务取消:
  AbortMultipartUpload。
  不写 StepSucceeded。

进程崩溃:
  后台扫描 upload session 或 S3 in-progress uploads。
  超时后 abort。
```

这里最好不要只依赖 S3 lifecycle。Lifecycle 是兜底，不是业务状态机。它通常按天级别处理，不适合给用户一个确定的“已取消就立刻释放空间”的承诺。对成本敏感的大对象系统，业务层要主动 abort。

还有一个细节：如果使用 multipart upload 配合条件写，complete 阶段可能因为 `If-None-Match` 或 `If-Match` 失败。AWS 条件写文档提到，`CompleteMultipartUpload` 遇到某些并发冲突时，可能需要重新 initiate multipart upload。此时旧 upload 的 part 也要 abort 或等 lifecycle 清理。不要只处理 `UploadPart` 失败，complete 失败同样会留下 part。

对 LogServe 当前实现，可以这样讲边界：它的 S3 store 现在是简化版单次 PUT，没有 multipart upload，因此暂时没有残留 part 清理问题。生产化增加 multipart 后，就要给 result store 加 upload session、abort、生命周期配置和清理指标。否则大 result 上传失败会悄悄变成 S3 账单问题。

面试里可以这样答：

```text
multipart upload 失败后，要用 AbortMultipartUpload 主动清理 upload_id 下已经上传的 part，因为未完成 part 会继续占存储并计费。进程崩溃或 abort 失败时，还要配置 S3 Lifecycle 的 AbortIncompleteMultipartUpload 作为兜底，比如超过 1 天或 7 天自动删除 incomplete upload。业务层最好记录 upload_id、key、attempt、part ETag 和 created_at，后台扫描超时 upload 并 abort。注意 abort 需要 s3:AbortMultipartUpload 权限，complete 失败也可能留下 part，不能只处理 UploadPart 失败。
```

## Q008. S3 ETag 是否一定等于 MD5？

**回答：**

不一定。这个问题现在必须回答得很明确：S3 ETag 可能是 MD5，也可能不是。把 ETag 当成 MD5 是老经验，只有在特定条件下才成立。

AWS API 文档对 ETag 的说法是：ETag 是对象内容变化的标识，可能是也可能不是对象数据的 MD5 digest，取决于对象如何创建、如何加密。几个常见情况可以这样记：

```text
单 part PutObject / POST / Copy:
  如果是 plaintext 或 SSE-S3 加密，ETag 通常是对象数据的 MD5。

SSE-KMS 或 SSE-C:
  ETag 不是对象数据的 MD5。

Multipart Upload 或 Part Copy:
  ETag 不是完整对象的 MD5，不管加密方式是什么。

S3 Console 上传较大对象:
  可能自动使用 multipart upload，因此 ETag 也不再是完整对象 MD5。

Directory bucket:
  AWS 文档明确提到 MD5 不受支持。
```

multipart upload 的 ETag 最容易误导人。很多人看到 ETag 长这样：

```text
"d41d8cd98f00b204e9800998ecf8427e-8"
```

就以为前半段是完整对象 MD5，后面的 `-8` 是 part 数。实际工程里不能拿这个 ETag 直接和本地文件 MD5 比较。它通常是某种 part checksum 的组合标识，能帮助 S3 和客户端在 multipart complete 流程中识别 part，但不是稳定的“完整对象 MD5 语义”。

为什么这个边界重要？

第一，数据完整性校验不能依赖 ETag。你如果把本地文件 MD5 和 S3 ETag 比，单 part、SSE-S3 场景可能刚好一致；一旦对象变大、SDK 自动 multipart、开启 SSE-KMS、换成 directory bucket，就会误报不一致，或者更糟糕：你以为验证了完整性，实际没有。

第二，去重不能只靠 ETag。result store 如果要内容寻址，应该自己算 SHA-256 或明确的 checksum，并把它写进 key 或 metadata。LogServe 当前 local/S3 store 用 `sha256(data)` 生成文件名，这比把 ETag 当业务 fingerprint 更稳。

第三，条件写中的 ETag 是版本条件，不是业务 checksum。`If-Match` 可以表达“只有当前对象还是这个 ETag 时才覆盖”，适合乐观并发控制。但这不等于“对象内容等于某个 MD5”。并发控制和数据完整性是两件事。

第四，下载校验应该用 S3 checksum 功能或自带 checksum。AWS 现在支持多种 checksum 算法，包括 CRC64NVME、CRC32、CRC32C、SHA1、SHA256、MD5、XXHash 系列等。官方文档还提到 CRC64NVME 是默认 checksum 算法之一。客户端可以在上传时提供 checksum，让 S3 服务端校验；后续也可以通过 HeadObject/GetObject 相关能力读取 checksum 信息，或者在业务 metadata 中保存自己的 SHA-256。

一个更稳的 result reference 可以这样做：

```json
{
  "uri": "s3://bucket/results/sha256/7f...a9.json",
  "size_bytes": 104857600,
  "sha256": "7f...a9",
  "s3_etag": "\"abc...-8\"",
  "checksum_algorithm": "SHA256",
  "checksum_value": "7f...a9",
  "version_id": "..."
}
```

这里 `s3_etag` 可以保留，用于排障、条件请求、缓存判断；但业务完整性看 `sha256` 或 S3 明确 checksum 字段。不要把两者混在一起。

面试里可以举一个典型事故：开发环境小文件用单 part PUT，ETag 正好等于 MD5；上线后文件变成 500 MB，SDK 自动 multipart，ETag 变成 `hash-partcount` 形式。校验逻辑开始报错，团队以为上传损坏。真正的问题是校验逻辑依赖了不成立的假设。

面试里可以这样答：

```text
S3 ETag 不一定等于 MD5。只有部分单 part、plaintext 或 SSE-S3 对象的 ETag 才通常是对象 MD5；multipart upload、SSE-KMS、SSE-C、Part Copy、directory bucket 等场景下都不能这么认为。ETag 更适合作为对象版本/条件请求标识，不应作为业务完整性 checksum。大结果 result_ref 应该自己记录 SHA-256、size、checksum algorithm，或者使用 S3 的 checksum 字段；ETag 可以保留用于调试和 If-Match，但不要把它当 MD5。
```

## Q009. 对象存储的一致性语义如何影响读后写？

**回答：**

读后写语义决定了一个系统在写完对象后，能不能立刻把引用交给下游读取。对 result reference 模式来说，这个问题很具体：`PutObject` 成功返回后，控制面能不能马上写 `StepSucceeded(result_ref)`？下游 step 拿到 `result_ref` 后，能不能马上 `GetObject`？

对 Amazon S3 当前语义，答案是：对对象 PUT/DELETE、GET、LIST、HEAD 这类对象操作，S3 提供 strong read-after-write consistency。AWS 文档说，成功 PUT 后，随后的 GET 或 LIST 能看到写入；覆盖已有对象后立刻读取会返回新数据；删除后立刻读取不会返回已删除对象。对象 metadata、tag、ACL 等读取也在强一致范围内。

这意味着在 S3 本身这一层，下面的顺序是成立的：

```text
PutObject(key=result)
  -> 200 OK
  -> append StepSucceeded(result_ref=s3://bucket/key)
  -> downstream GetObject(s3://bucket/key)
  -> 读到刚写入的对象
```

这个语义比早年 S3 的旧经验简单很多。以前很多系统会在 S3 外面加一致性索引，担心 PUT 后 LIST 看不到。现在如果只谈 AWS S3 的对象读后写，不应该再说“新对象最终一致”。但要注意，强一致不等于所有问题都消失。

第一，强一致是单对象和对象列表层面的，不是跨系统事务。`PutObject` 成功后，写事件日志可能失败。此时对象已经存在，但 workflow 没有成功事件，形成 orphan object。反过来，如果你先写事件再写对象，事件可能指向不存在的对象。工程上通常先写对象再写事件，因为 orphan object 可以清理，而 broken result_ref 会破坏下游 correctness。

第二，强一致不等于跨 key 原子。假设一个 result 由 `data.json` 和 `manifest.json` 两个对象组成，你不能要求读者在某一瞬间同时看到两个 key 的同一事务版本。S3 文档也写明，没有跨 key 原子更新。要做多对象发布，最好写不可变 data object，最后用单个 manifest key 或事件日志发布一致视图。

第三，并发 writer 仍然危险。S3 对单 key 更新是原子的，读者不会看到半对象；但两个 writer 同时 PUT 同一个 key，业务顺序不一定是你想要的。AWS 文档明确说，S3 不支持 concurrent writer 的对象锁；如果两个 PUT 同时写同一 key，最终值按服务端接收/时间语义决定，应用要自己做锁。对 workflow result 来说，最好让 key 包含 attempt 或内容哈希，避免多 writer 覆盖。

第四，客户端缓存会改变体感一致性。S3 本身强一致，不代表你的 SDK wrapper、CDN、HTTP cache、Mountpoint、本地 result cache 都强一致。AWS Mountpoint 语义文档也提到，启用缓存后可能看到缓存数据。预签名 URL 如果走 CloudFront 或浏览器缓存，还要考虑 HTTP cache header。排障时要问：读到旧数据的是 S3，还是中间缓存？

第五，LIST 强一致不代表 LIST 是数据库查询。写后马上 LIST prefix 能看到新对象，但大规模 LIST 仍然有分页、权限过滤、成本、延迟。控制面不要靠频繁 LIST 来决定 workflow 状态。事件日志和 metadata 仍然是状态真相，S3 是 result body 的存储。

第六，versioning 会改变“当前对象”的含义。开启 S3 Versioning 后，同一个 key 可以有多个版本。写后读当前版本通常能读到新版本，但如果 result_ref 保存了 version_id，下游应该读指定版本；如果没保存 version_id，后续覆盖可能让同一个 key 指向新内容。对不可变 result，最好 key 不覆盖，或者 ref 包含 version_id。

第七，删除和生命周期会影响旧 result_ref。S3 删除后强一致，所以对象过期或 lifecycle 删除后，下游马上可能读不到。一个 workflow 的 metadata 仍然有 `result_ref`，不代表对象永远存在。系统要把 `result_ref expired` 当成正常状态处理，而不是当作日志损坏。

在 LogServe 里可以这样落地：

```text
大结果写入:
  Put result object 成功后再写 StepSucceeded。

下游读取:
  如果 step args 需要上游结果，按 result_ref 从 objectstore 加载。

恢复:
  replay 日志只恢复 result_ref，不把大对象读入控制面。

清理:
  result object lifecycle 可以独立于 workflow event retention。

边界:
  当前项目验证 result_ref 机制，不声称实现完整对象版本治理和 CDN 缓存一致性。
```

面试里可以这样答：

```text
对象存储的一致性决定了写完对象后能不能马上发布 result_ref。当前 Amazon S3 对 PUT/DELETE 和 GET/LIST/HEAD 提供强读后写一致性，所以 PutObject 成功后，下游通常可以马上 GetObject 或 List 看到它。这个保证让“先写对象，再写 StepSucceeded(result_ref)”成为合理协议。但它不是跨系统事务，也不是跨 key 原子更新；对象写成功但事件写失败会留下 orphan object。并发写同一 key 仍要用唯一 key、version_id、条件写或应用层锁处理，客户端缓存和生命周期删除也会影响实际读到什么。
```

## Q010. 预签名 URL 的安全边界是什么？

**回答：**

预签名 URL 的本质是把一次 S3 操作临时授权给拿到 URL 的人。它不是登录态，不是对象 ACL，也不是长期权限。更直接地说，它是 bearer token：谁拿到这个 URL，谁就能在有效期内按签名里的方法、bucket、key、headers 条件去访问对象。

AWS 文档也明确说，预签名 URL 的能力受创建它的 IAM principal 权限限制；URL 在过期前可以多次使用；如果底层临时凭证先过期，URL 也会提前失效。这个边界很关键。

一个预签名 URL 通常绑定这些内容：

```text
HTTP method:
  GET、PUT、HEAD 等。GET URL 不能自动变成 PUT 权限。

bucket/key:
  只能访问签名时指定的对象。

expiration:
  到期后新请求失败。

signed headers/query:
  如果签了 Content-Type、checksum、x-amz-meta-*，请求必须匹配。

creator permissions:
  创建 URL 的 role/user 必须本来就有对应 S3 权限。
```

安全边界第一条：URL 泄漏就等于权限泄漏。它通常会出现在浏览器地址栏、代理日志、服务端 access log、referer、监控、错误上报、聊天记录、截图、工单里。不要把预签名 URL 当普通链接到处打印。服务端日志里最好只记录 object key 的 hash、request id、过期时间，不记录完整 query string。

第二条：权限要最小化。生成 GET URL 的 role 不应该有 `s3:PutObject`，生成 PUT URL 的 role 不应该能读所有对象。bucket policy 和 IAM policy 要限制 prefix、tenant、object tag、source VPC/IP、KMS key。预签名 URL 继承创建者权限，所以创建者权限过大，URL 的潜在影响面也大。

第三条：有效期要短。AWS SigV4 下，IAM user 凭证创建的预签名 URL 最长可到 7 天；临时凭证创建的 URL 不会超过临时凭证寿命。用户下载 result object 通常不需要几天有效期，几分钟到几十分钟更合理。对于内部 worker 拉取结果，甚至可以更短。AWS 还支持用 bucket policy 的 `s3:signatureAge` 限制签名年龄，比如拒绝签名超过 10 分钟的请求。

第四条：预签名 PUT URL 会覆盖同 key 对象，除非你限制它。AWS 文档提醒，用预签名 URL 上传指定 key 时，如果同 key 已存在，S3 会替换对象。对用户上传或 workflow result 上传，这很危险。应该使用唯一 key、attempt_id、content hash key，必要时配合条件写。如果你需要限制上传内容，还要签入 `Content-Type`、checksum、`Content-Length` 边界和 metadata 约束，或者让上传先到隔离的 staging prefix。

第五条：预签名 URL 不等于业务授权。业务系统应该先检查用户是否能访问这个 workflow/result，再生成 URL。不能因为用户猜到 workflow_id 或 result key 就发 URL。对象 key 最好不要包含可猜测的敏感信息；URL 生成接口要做鉴权、审计、速率限制。

第六条：撤销能力有限。预签名 URL 发出去后，没有一个“删除这条 URL”的独立开关。可以通过让底层凭证失效、改 bucket policy、删对象、改 key、禁用 KMS key、改变网络限制等方式让它不能再用，但这些都是间接手段，可能影响其他请求。因为撤销不方便，所以有效期要短，权限要窄。

第七条：下载和上传的边界不同。

```text
GET presigned URL:
  泄漏后别人能读对象。
  风险是数据泄露、绕过业务审计、长期缓存。

PUT presigned URL:
  泄漏后别人能写对象。
  风险是覆盖结果、上传恶意内容、制造存储账单、污染后续 workflow。

HEAD presigned URL:
  看似无害，但可能泄漏对象是否存在、大小、ETag、metadata。
```

第八条：大文件下载的过期语义要理解。AWS 文档说，S3 在 HTTP 请求开始时检查预签名 URL 是否过期；如果下载在过期前开始，连接不断时可以继续；连接断了，过期后重试会失败。对大 result 下载，客户端要支持重新申请 URL，而不是把一个即将过期的 URL 反复 retry。

第九条：不要把预签名 URL 长期写进事件日志。事件日志是 replay 和审计用的，可能保留很久。预签名 URL 是临时授权材料，不应该作为稳定 result reference。正确做法是日志保存稳定的 `s3://bucket/key` 或内部 `result_ref`；当用户或下游需要访问时，再按权限即时生成短期 presigned URL。

第十条：KMS 和加密权限也要考虑。对象如果用 SSE-KMS，加解密仍受 KMS key policy 和 IAM 约束。生成 URL 的主体、S3 服务和访问路径都要满足相关权限。不要只看 S3 bucket policy。

LogServe 现在还没有把 `result_ref` 暴露成预签名 URL 的完整外部下载服务。如果以后加这个能力，我会按这个边界做：

```text
metadata:
  只保存 stable result_ref，不保存 presigned URL。

API:
  GetResultDownloadURL(workflow_id, step_id)
  先做业务鉴权，再生成短期 URL。

scope:
  URL 只允许 GET 指定 key。

expiry:
  默认几分钟，最多按业务配置。

audit:
  记录谁为哪个 result 生成了 URL，不记录完整 URL。

upload:
  PUT URL 只给 staging key，签 checksum 和 content type，完成后服务端校验再发布 result_ref。
```

面试里可以这样答：

```text
预签名 URL 是临时 bearer token。谁拿到 URL，谁就能在有效期内执行签名指定的 S3 操作；它的能力受创建者 IAM 权限、HTTP method、bucket/key、签名 header、过期时间和 bucket policy 限制。安全上要短有效期、最小权限、不要记录完整 URL、限制 source IP/VPC 或 signatureAge，PUT URL 要防覆盖和恶意上传。预签名 URL 不是稳定 result_ref，不能长期写进事件日志；日志应保存 s3://bucket/key，真正下载时再按业务权限生成短期 URL。
```

## Q011. 大对象上传成功但元数据写失败时如何清理？

**回答：**

这是 result reference 模式里最典型的半成功状态：对象已经进了对象存储，但日志或 metadata 里还没有稳定记录它。对象存储和数据库、事件日志之间没有一个天然的跨系统事务，所以不能假设“PutObject 成功”和“写 StepSucceeded 事件成功”会一起提交。

这类故障通常发生在下面这个顺序里：

```text
worker/control 拿到大结果
  -> PutObject 或 CompleteMultipartUpload 成功
  -> 拿到 s3://bucket/key 或 version_id
  -> append StepSucceeded(result_ref) 失败
  -> metadata UpdateWorkflow 失败
```

这里留下的是 orphan object，也就是对象存在，但没有任何业务状态引用它。这个状态不好看，但比另一种状态更容易治理。工程上通常宁愿先写对象、再写成功事件，因为 orphan object 可以靠 GC 或 lifecycle 清掉；如果反过来先写 metadata，再写对象，就可能让下游看到一个指向不存在对象的成功结果，这会破坏 correctness。

第一步不是急着删对象，而是先判断元数据写入是不是可以重试。写 metadata 失败可能只是控制面重启、数据库连接断开、append log 超时。对象已经上传成功时，最好的补救是用同一个 `result_ref`、同一个 step 幂等键，继续重试写成功事件：

```text
PutObject succeeded:
  ref = s3://bucket/workflows/wf-1/steps/a/sha256.json

Append StepSucceeded failed:
  retry append with idempotency_key = wf-1:a:input_hash:succeeded
  payload still uses the same ref
```

只要事件日志有幂等 append，重复提交同一个成功事件就不会产生多个逻辑结果。LogServe 现在的 workflow step 成功事件就使用了类似的 idempotency key：`workflow_id + step_id + input_hash + succeeded`。所以面试时可以说：上传成功但写元数据失败时，优先把它当成“提交阶段失败”，先重试提交，不是马上删除对象。

第二步才是清理。清理要有一个前提：确认这个对象没有被任何 durable metadata 引用。不能因为这次写 metadata 失败就立刻删除 key，原因有三个。

第一，写 metadata 的请求可能已经成功，只是客户端没收到响应。比如 `appendLog` 已经落盘，网络在响应阶段断了。此时如果清理线程立刻删对象，日志里就会出现一个有效的 `result_ref`，但对象没了。这个事故比 orphan object 更严重。

第二，key 如果是内容寻址的，可能被其他 workflow 或其他 attempt 复用。LogServe 当前的 local/S3 store 用 `sha256(data)` 生成对象名，同一 namespace 下相同内容会得到同一个 key。生产系统如果做跨 namespace 或跨 workflow 去重，删除前更要查反向引用或 mark set。

第三，S3 开启 versioning 后，删除“key”和删除“某个 version”不是一回事。普通 DeleteObject 可能只是写入 delete marker；真正释放某个版本，要删除指定 version ID。result reference 如果保存了 version_id，清理也应该针对 version_id，不要误删当前 key 下的其他版本。

比较稳的做法是把上传过程显式建模成 upload session：

```text
ObjectUploadSession:
  upload_id / attempt_id
  workflow_id
  step_id
  object_key
  version_id
  checksum
  size_bytes
  status = UPLOADING | OBJECT_WRITTEN | COMMITTED | ABORTED
  created_at
  updated_at
```

流程可以是：

```text
1. 创建 upload session，状态为 UPLOADING。
2. 上传对象；multipart 场景记录 S3 upload_id 和 part ETag。
3. PutObject 或 CompleteMultipartUpload 成功后，把 session 标成 OBJECT_WRITTEN。
4. 写 StepSucceeded(result_ref) 和 metadata。
5. 写成功后，把 session 标成 COMMITTED。
6. 后台扫描长时间停留在 OBJECT_WRITTEN 的 session。
7. 对每个候选对象，先查日志和 metadata 是否引用它；没有引用再删除或打 expired tag。
```

如果是 multipart upload，还要区分“对象已经 complete”和“upload 还没 complete”。AWS 文档说 multipart upload initiate 后不会自动过期，必须 complete 或 stop；未完成的 part 会继续占存储并计费。清理策略要这样拆：

```text
UploadPart 阶段失败:
  AbortMultipartUpload(upload_id)
  清掉 upload session。

CompleteMultipartUpload 成功，但 metadata 失败:
  这时已经是完整对象，不是 part。
  进入 orphan object 清理流程。

进程崩溃，不知道 complete 是否成功:
  先 HEAD key 或按 version_id 查询。
  如果对象存在，按 orphan object 处理。
  如果对象不存在且 upload_id 还 in-progress，abort。
```

S3 Lifecycle 的 `AbortIncompleteMultipartUpload` 很适合作为兜底。它可以在 multipart upload 发起后超过指定天数仍未完成时自动 abort，并删除相关 part。但它只处理 incomplete multipart upload，不会删除已经 complete 的对象。已经 complete 的 orphan result object 仍然需要应用层 GC 或另外的对象过期策略。

对象 key 设计也会影响清理难度。推荐把未提交对象放到 staging prefix，提交后再发布稳定引用：

```text
staging/workflows/wf-1/steps/a/attempt-3/<uuid>.json
live/workflows/wf-1/steps/a/sha256/<hash>.json
```

但在 S3 general purpose bucket 里，rename 不是普通 POSIX 原子 rename。常见做法是上传到 final immutable key，最后由事件日志发布；或者上传到 staging key，服务端校验后 copy 到 final key，再写事件。无论哪种，发布动作都应该是“写日志/metadata 中的 ref”，不是依赖对象 key 的目录移动来表达事务提交。

对 LogServe 当前实现，边界要说清楚：`materializeResult` 是先 `resultStore.Put`，再 append `StepSucceeded`。如果 `Put` 成功但 append log 或 metadata 更新失败，当前代码没有同步删除对象，也没有 upload session 表，所以可能留下 orphan object。因为本项目是机制验证系统，这个边界可以接受；如果生产化，要补三件事：

```text
1. result object metadata:
   workflow_id、step_id、attempt_id、checksum、created_at、status。

2. orphan sweeper:
   扫 object prefix 或 S3 Inventory，与 workflow log/metadata 的 live refs 做差集。

3. deletion guard:
   只删除早于某个 watermark、未被引用、未被 pin、未处于 Object Lock/replication pending 的对象。
```

面试里可以这样答：

```text
大对象上传成功但元数据写失败，本质是对象存储提交了、控制面提交失败，留下 orphan object。协议上我会先重试元数据提交，用同一个 result_ref 和幂等键写 StepSucceeded；不能马上删，因为写日志可能已经成功只是响应丢了。确认无法提交后，后台 GC 再根据 upload session、created_at、checksum、workflow_id 和日志/metadata reachability 判断是否删除。multipart 阶段没 complete 的用 AbortMultipartUpload 和 lifecycle 兜底；已经 complete 的对象要走应用层 orphan object 清理。LogServe 当前先 Put 再写事件，因此 correctness 上避免了 broken ref，但还没有完整 orphan sweeper，这是生产化要补的点。
```

## Q012. 元数据写成功但对象上传失败时如何恢复？

**回答：**

这个故障比上一题更危险。元数据已经说“结果在这里”，但对象并不存在，或者对象没上传完整。下游拿到 `result_ref` 后会读失败，workflow replay 也会恢复出一个看似成功、实际不可取回的状态。这就是 broken reference。

先说结论：正常协议应该避免这种状态。对 result reference，成功事件只能在对象成功写入之后发布。也就是说，下面这个顺序是不推荐的：

```text
append StepSucceeded(result_ref=s3://bucket/key)
  -> update workflow metadata
  -> PutObject(key) 失败
```

如果系统先写 metadata，再上传对象，就必须把 metadata 状态设计成非终态，例如 `RESULT_PREPARING` 或 `OBJECT_PENDING`，不能直接写 `SUCCEEDED`。只有对象 `PutObject` 或 `CompleteMultipartUpload` 成功，并且可通过 `HEAD/GET` 校验后，才可以把 step 切到 `SUCCEEDED`。

更合理的状态机是：

```text
StepStarted
  -> ResultUploading
  -> ObjectWritten(ref, checksum, size, version_id)
  -> StepSucceeded(result_ref)
```

如果中间对象上传失败，状态最多停在 `ResultUploading` 或 `StepFailed`，不会把一个坏引用暴露给下游。

如果历史上已经出现“metadata 成功、对象失败”的坏状态，恢复策略要看结果能不能重建。

第一种情况：worker 或控制面还有本地临时结果。比如大结果先写在 worker spool，再上传对象存储。此时可以重新上传同一份数据，最好上传到同一个内容哈希 key：

```text
metadata:
  result_ref = s3://bucket/workflows/wf-1/steps/a/sha256-X.json
  sha256 = X
  size = 300MB

repair:
  read worker spool / retry payload
  verify sha256 == X
  PutObject same key with If-None-Match: *
  HEAD key
  mark repair_completed
```

如果同 key 已经存在，还要校验 size 和 checksum，不能盲目认为是同一个对象。使用 `If-None-Match: *` 可以防止覆盖已有对象；AWS S3 条件写文档明确支持在 `PutObject` 和 `CompleteMultipartUpload` 上用这个条件阻止同名覆盖。对内容寻址 key 来说，这个条件尤其有用。

第二种情况：结果可重新计算。workflow step 如果是确定性的，并且输入、代码版本、环境、随机种子或外部依赖都可控，可以把这个 step 重新调度。这里要谨慎使用“重试”。如果上游任务有副作用，例如已经调用外部 API、扣款、发邮件、写数据库，就不能简单重跑。面试里最好说成：

```text
纯计算 step:
  可以重新执行，生成同一 result_ref 或新 result_ref。

有副作用 step:
  不能盲目重跑。
  需要业务幂等键、外部事务记录或人工修复。
```

第三种情况：结果无法重建。那就不能继续把 workflow 标成成功。应该把 step 或 workflow 标记为 `RESULT_UNAVAILABLE`、`REPAIR_REQUIRED` 或失败状态，并保留错误原因：

```json
{
  "status": "FAILED",
  "error_code": "RESULT_OBJECT_MISSING",
  "result_ref": "s3://bucket/key",
  "repairable": false
}
```

不要用空 JSON、空文件或默认值伪装结果。这样下游会在错误数据上继续计算，故障会扩散。

恢复时还要区分对象上传失败的具体阶段。普通 `PutObject` 失败通常不会产生可见对象；multipart upload 在 `CompleteMultipartUpload` 成功前也不会产生最终对象，但已经上传的 part 会留在 in-progress upload 里。此时要 `AbortMultipartUpload`，然后决定是否重传。只有 complete 成功后，才有最终对象和可发布引用。

还有一种隐蔽情况：对象存在，但内容不对。比如上传时使用了同一个 key，另一个 attempt 覆盖了对象；或者 metadata 保存的是 A 的 checksum，key 里实际是 B。恢复流程不能只做 `HEAD 200`，至少要比较：

```text
size_bytes
checksum / sha256
version_id
content_type
producer attempt
```

如果 bucket 开了 versioning，result reference 最好保存 `version_id`。这样即使同 key 后来被覆盖，旧 workflow 也能读到当时发布的版本。没有 version_id 时，禁止覆盖 key 更重要：每次结果用内容哈希、attempt id 或 UUID 生成不可变 key。

对 LogServe 当前代码来说，正常成功路径已经避开了这个坏状态。`completeWorkflowStep` 在写 `StepSucceeded` 前先调用 `materializeResult`；`materializeResult` 只有在 `resultStore.Put` 成功后才返回 ref。如果对象上传失败，函数直接返回错误，成功事件不会写入。也就是说，当前实现不会主动制造“metadata 成功但对象失败”的 `StepSucceeded`。不过还要承认两个边界：

```text
1. 当前 S3 store 是单次 PUT 简化实现，没有 multipart repair session。
2. ref 里没有 version_id、size、checksum 字段，完整生产校验还不够。
```

如果以后加 metadata-first 的异步上传，就必须增加非终态，不然 correctness 会倒退。正确的异步版本应该是：

```text
StepResultPending(ref_candidate)
  -> uploader writes object
  -> verifier HEAD/GET checksum
  -> StepSucceeded(result_ref)
```

面试里可以这样答：

```text
元数据写成功但对象上传失败，说明系统发布了 broken result_ref。最好的恢复是从协议上避免：对象必须先成功写入并校验，再写 StepSucceeded；如果要先写 metadata，只能写 ResultUploading 这类非终态，不能写成功。已经出现坏状态时，先 HEAD/GET 校验对象；如果 worker spool 还在，就按 checksum 重新上传；如果 step 可确定性重算，就重新调度；如果无法恢复，就把 step 标成 RESULT_UNAVAILABLE 或失败，不能返回空结果伪装成功。LogServe 当前是先 Put 再写事件，所以成功路径不会产生这个状态，但生产版还应补 version_id、checksum 和 repair workflow。
```

## Q013. 对象引用如何防止悬挂引用？

**回答：**

悬挂引用指的是 metadata、事件日志或 workflow state 里保存了一个 `result_ref`，但这个引用已经不能正确读出对象。它不只包括对象被删。下面这些都算：

```text
对象不存在:
  GET/HEAD 返回 404。

对象被 versioning delete marker 遮住:
  不带 version_id 读取时返回 404，header 里可能有 x-amz-delete-marker。

对象被覆盖:
  key 还在，但内容不是当时发布的结果。

权限失效:
  对象存在，但当前 worker 或用户没有 GetObject/KMS Decrypt 权限。

对象转入归档层:
  metadata 还在，但读取前要 restore。

checksum 不匹配:
  读出来的数据不是 ref 描述的那份数据。

生命周期过期:
  系统按策略删除了对象，但业务 metadata 没有记录 result 已过期。
```

防止悬挂引用，第一条是引用本身要足够完整。只保存一个字符串 `s3://bucket/key` 太弱。更稳的 result reference 至少应该有：

```json
{
  "uri": "s3://logserve-results/workflows/wf-1/steps/a/sha256.json",
  "version_id": "optional-version-id",
  "size_bytes": 104857600,
  "sha256": "7f...",
  "content_type": "application/json",
  "encoding": "gzip",
  "created_at_ms": 1760000000000,
  "expires_at_ms": 1762592000000
}
```

`uri` 用来定位对象，`version_id` 用来固定版本，`sha256` 和 `size` 用来确认读到的是不是同一份结果，`expires_at` 告诉上层这个引用不是永久承诺。预签名 URL 不适合作为这种稳定引用，因为它是临时授权材料，到期后自然失效。

第二条是对象 key 要尽量不可变。workflow result、actor snapshot 这类对象一旦被事件日志引用，就不应该被覆盖。可以用这些 key 设计：

```text
按内容寻址:
  workflows/<wf>/steps/<step>/sha256/<hash>.json

按 attempt 寻址:
  workflows/<wf>/steps/<step>/attempt-3/<uuid>.json

按版本寻址:
  workflows/<wf>/steps/<step>/<logical-name>.json + version_id
```

不要把所有 attempt 都写到 `workflows/wf-1/steps/a/result.json`。如果覆盖同一个 key，下游和 replay 看到的内容会随时间变化，事件日志就失去了“当时发生了什么”的含义。AWS S3 条件写里的 `If-None-Match: *` 可以用来避免同名覆盖；如果 key 已经存在，写入失败，应用再决定复用还是换 key。

第三条是发布协议要正确。对象引用只能在对象成功写入之后发布：

```text
PutObject / CompleteMultipartUpload
  -> optional HEAD/Get checksum verification
  -> append StepSucceeded(result_ref)
  -> update metadata view
```

S3 当前对 PUT、DELETE、HEAD/GET/LIST 提供强一致，这让“写对象成功后马上发布 ref”成为可行协议。但强一致不是跨系统事务，所以仍然要处理 orphan object。不要因为怕 orphan，就反过来先写成功事件。

第四条是删除必须经过 reachability 判断。对象生命周期规则如果直接按 prefix 删除所有 `workflows/` 下 30 天前的对象，而 metadata 里还保存这些 ref，就会制造大量悬挂引用。正确做法是：

```text
metadata / log:
  记录 result_ref 的业务 TTL、pin 状态、workflow retention。

application GC:
  判断 ref 已不可达或已过期。

object deletion:
  只删除 GC 判定安全的对象。

S3 lifecycle:
  作为 staging、tmp、incomplete multipart、expired prefix 的兜底。
```

第五条是读的时候要验证，不要默认 ref 一定健康。下游 dereference 可以分两层：

```text
fast path:
  GetObject(ref)
  如果成功且 size/checksum 匹配，返回数据。

repair path:
  404 -> 查 metadata 是否已过期、是否 delete marker、是否 version_id 缺失。
  403 -> 查 IAM/KMS/tenant policy。
  Glacier restore required -> 发起 restore 或返回 result archived。
  checksum mismatch -> 标记 corruption，禁止继续计算。
```

这里要把 404、403、归档、checksum mismatch 分开。它们对用户和系统的含义不同。404 可能是 lifecycle 删除；403 可能是权限配置；归档可能是成本策略；checksum mismatch 可能是严重数据损坏或覆盖事故。

第六条是多对象结果要用 manifest。一个大 result 可能拆成多个对象：

```text
part-00000.parquet
part-00001.parquet
manifest.json
```

不要让 workflow metadata 保存一堆 part key，然后希望下游自己拼。更好的方式是只发布一个 manifest ref。manifest 里列出所有 part、size、checksum、schema 和版本。GC 判断 reachability 时，从 manifest 继续 mark 所有 part。这样发布视图是单点的，删除也更容易做对。

第七条是利用 S3 Versioning 和 Object Lock，但不要误解它们。Versioning 可以保留多个版本，避免简单覆盖或误删后无法恢复；Object Lock 可以在保留期内阻止对象版本被删除或覆盖，适合审计或合规场景。它们能降低悬挂引用风险，但也会增加成本和 GC 复杂度。启用 versioning 后，GC 必须考虑 current version、noncurrent version、delete marker；Object Lock 保护的版本在保留期内不能删，应用层要把这种对象当作“暂不可回收”。

对 LogServe 当前实现，`result_ref` 只是字符串，没有 version_id、checksum、size 这些字段。key 是 SHA-256 生成的，这一点有利于不可变和去重；但 protobuf 里没有把 checksum 元数据显式暴露出来。如果把这个系统继续做生产化，我会把 `result_ref` 从 string 升级成结构体：

```protobuf
message ResultRef {
  string uri = 1;
  string version_id = 2;
  int64 size_bytes = 3;
  string sha256 = 4;
  string content_type = 5;
  int64 created_at_ms = 6;
  int64 expires_at_ms = 7;
}
```

面试里可以这样答：

```text
防悬挂引用要从引用格式、写入协议、删除协议和读取校验四层做。引用不能只有 s3://bucket/key，最好包含 version_id、size、checksum、content type 和 expires_at；对象 key 要不可变，使用内容哈希、attempt id 或 S3 version_id，写入时用 If-None-Match 防覆盖；只有对象成功写入并校验后才发布 StepSucceeded；删除前必须通过应用层 reachability GC，不能只靠按 prefix 的 lifecycle。读取时还要区分 404、403、归档和 checksum mismatch。LogServe 当前 string ref 足够做机制验证，但生产版应把 result_ref 结构化。
```

## Q014. 对象 GC 如何判断哪些对象仍然可达？

**回答：**

对象 GC 的核心问题不是“哪些对象很久没访问”，而是“哪些对象仍然被系统状态需要”。访问时间只能帮你做冷热分层，不能决定对象是否可删。一个一年没人下载的 actor snapshot 可能仍然是 replay 的根；一个昨天上传失败留下的 staging object 可能已经没人需要。

我会把对象 GC 设计成 mark-and-sweep。

mark 阶段从 durable roots 出发。所谓 root，必须是重启后还存在的状态，不能是进程内 map。对 LogServe 这类系统，root 大概包括：

```text
未过 retention 的 workflow:
  WorkflowCompleted.result_ref
  StepSucceeded.result_ref
  失败 workflow 的诊断 result_ref

actor:
  当前可用于 replay 的 ActorSnapshotCreated.snapshot_ref
  retention 窗口内的历史 snapshot

日志:
  仍在 replay/audit retention 内的事件 payload 中的 refs

用户 pin:
  用户显式保存、导出、下载链接背后的结果

系统任务:
  正在运行的 step、重试中的 upload session、pending multipart upload

合规和审计:
  legal hold、Object Lock、审计保留期内的对象

派生 manifest:
  manifest ref 指向的 part objects
```

mark 的输出不是一个模糊集合，最好是精确到对象版本：

```text
LiveObjectRef:
  bucket
  key
  version_id optional
  reason = workflow_result | step_result | actor_snapshot | pinned | upload_pending
  owner_id = workflow_id / actor_id / tenant_id
  keep_until
```

如果没有 version_id，也要记录 key、checksum 和 size。这样 sweep 时如果 key 被覆盖，GC 至少能发现“candidate key 的当前对象和 live ref 描述不一致”。

sweep 阶段扫描对象存储里的候选对象。小规模系统可以按 prefix `ListObjectsV2`；大规模 S3 bucket 更适合用 S3 Inventory。AWS 官方说明 S3 Inventory 可以按天或周输出 CSV、ORC 或 Parquet，列出对象及其 metadata，而且不走同步 List API，不影响 bucket 的请求速率。生产系统常用 Inventory + Athena/Spark 做差集：

```text
candidates = inventory(prefix = result prefixes)
live = refs_from_metadata_and_log

delete_candidates =
  candidates
  - live
  - recently_created_objects
  - active_upload_sessions
  - locked_or_legal_hold_objects
  - replication_pending_objects
```

这里 `recently_created_objects` 很重要。对象先写、metadata 后写，中间有一个提交窗口。如果 GC 正好扫到刚上传但还没写 metadata 的对象，就可能误删。一般要加 grace period 和 watermark：

```text
只处理 created_at < gc_started_at - grace_period 的对象。
比如 grace_period = 24h 或至少大于最大任务运行时间 + 最大提交重试时间。
```

对正在上传的 multipart upload，不能只看对象列表，因为未 complete 的 part 不会作为普通对象出现。要另外扫描 upload session 或使用 lifecycle 的 `AbortIncompleteMultipartUpload`。应用层知道 upload_id 的情况下，应主动 abort；S3 lifecycle 是兜底。

GC 还要处理并发。常见保护手段有四个：

```text
1. 双次 mark:
   sweep 删除前重新查一次 metadata/log，确认 ref 仍不可达。

2. tombstone:
   先把对象或 metadata 标记为 pending_delete，过一个周期再删。

3. generation / epoch:
   GC 记录本轮 watermark，只删除早于 watermark 的对象。

4. object tag:
   staging、live、pinned、expired、gc-candidate 用 tag 或 prefix 区分。
```

为什么不能只做 reference count？引用计数看起来简单：metadata 加一，删除 metadata 减一，计数为 0 就删。但在分布式系统里，refcount 自己也有一致性问题。对象写成功但 refcount 增加失败，会误删；metadata 删除成功但 refcount 减少失败，会泄漏；重复重试可能把计数加错。refcount 可以作为加速索引，但不能作为唯一真相。最终判断仍然要能从日志和 metadata 重建 live set。

对于 LogServe，最自然的 root 是 shared log 和 materialized metadata view。系统本来就强调 replay：workflow、actor、LLM 状态可以从日志重建。对象 GC 可以利用这个性质：

```text
1. 从 workflow streams 扫 StepSucceeded 和 WorkflowCompleted，收集 result_ref。
2. 从 actor streams 扫 ActorSnapshotCreated，保留当前 snapshot 和 retention 窗口内 snapshot。
3. 从 metadata store 取当前 workflow/actor 状态，交叉校验。
4. 对 objectstore prefix 做 inventory/list。
5. 删除不在 live set、早于 grace period、未被 pin 的对象。
```

这里还要注意 actor snapshot。`docs/report.md` 里写到 actor replay 会从 snapshot object 和 tail log 开始，snapshot 前的 actor stream 会 logical trim。只要 tail log 依赖某个 snapshot，那个 snapshot ref 就是 root。不能因为旧日志被 trim 了，就把 snapshot 删掉；恰好相反，trim 后 snapshot 更重要。

如果对象是多租户的，GC 还要按 tenant 隔离。不能让一个租户的 metadata 扫描结果影响另一个租户的对象删除。对象 key、bucket、prefix、KMS key 和 IAM role 最好都能反映 tenant 边界。

删除执行本身也要分批。S3 单次删除、Batch Operations、Inventory manifest 都可以用来做大规模清理。AWS Batch Operations 能基于对象列表对大量对象执行操作，并提供进度和完成报告。生产环境里，我不会让控制面主进程同步删除百万对象，而是生成 delete manifest，交给后台 job，限速执行，记录 metrics：

```text
gc_mark_live_refs
gc_candidate_objects
gc_deleted_objects
gc_skipped_locked
gc_skipped_recent
gc_delete_errors
orphan_bytes_reclaimed
```

面试里可以这样答：

```text
对象 GC 应该按 reachability 做，而不是按最后访问时间做。mark 阶段从 durable roots 出发，包括未过 retention 的 workflow result、step result、actor snapshot、仍需 replay/audit 的日志、用户 pin、active upload session 和 legal hold；多对象结果要从 manifest 继续 mark part。sweep 阶段用 ListObjects 或 S3 Inventory 找候选对象，减去 live set、近期创建对象、pending upload、Object Lock/replication pending 对象，再分批删除。为了避免竞态，要有 grace period、watermark、删除前二次检查和 tombstone。LogServe 当前没有物理 object GC，但 shared log + metadata view 很适合生成 live ref set。
```

## Q015. 对象生命周期策略和应用层 GC 如何配合？

**回答：**

对象生命周期策略和应用层 GC 解决的是两个不同问题。S3 Lifecycle 知道对象的 prefix、tag、创建时间、版本状态和 storage class，但不知道某个对象是不是还被 workflow replay 需要。应用层 GC 知道业务引用关系，但不适合承担所有低层存储治理工作，比如 incomplete multipart upload、冷存储转移、noncurrent version 清理和大规模批量删除。

所以二者的关系应该是：应用层 GC 决定“能不能删”，生命周期策略负责“按存储规则自动转移或兜底清理”。不要把生命周期规则当成业务正确性的唯一来源。

可以按对象类型分层配置。

第一类是 staging 或 tmp 对象。这些对象理论上还没有被成功事件引用，生命周期可以很激进：

```text
prefix:
  staging/
  tmp/
  uploads/

策略:
  1 天或 7 天后删除。
  incomplete multipart upload 超过 1 天或 7 天自动 abort。

应用层:
  正常失败时主动删除或 abort。
  lifecycle 只做兜底。
```

AWS 官方建议用 `AbortIncompleteMultipartUpload` 生命周期动作清理未完成的 multipart upload。这个策略非常适合大对象上传失败后的成本控制，因为未完成的 part 会计费。注意它不会删除已经 complete 的对象。

第二类是 live result 对象。它们已经被 `StepSucceeded`、`WorkflowCompleted` 或 `ActorSnapshotCreated` 引用。对这类对象，不应该简单用“创建 30 天后删除”作为唯一规则，除非业务明确承诺结果只保留 30 天，并且 metadata 会同步显示过期状态。

更稳的方式是 metadata 里保存业务 retention：

```text
ResultRef:
  uri
  created_at
  expires_at
  retention_class = debug | normal | audit | pinned
  pinned_by_user
  legal_hold
```

应用层 GC 到期后先把对象从 live set 移出，或者给对象打 `gc=eligible`、`expires_at=...` tag，再由生命周期规则删除这些已标记对象。这样 lifecycle 删除的是应用层已经判定可删的对象，而不是盲删所有老对象。

第三类是冷数据。很多 workflow 结果在一段时间后很少读，但仍要保留。生命周期适合做 storage class transition：

```text
0-30 天:
  S3 Standard，支持快速读取和调试。

30-90 天:
  Standard-IA 或 Intelligent-Tiering，降低成本。

90 天后:
  Glacier Instant Retrieval / Flexible Retrieval / Deep Archive，视恢复要求决定。

业务 metadata:
  标记 result_archived，下载时提示 restore 或异步取回。
```

这里要把“可达”和“热”分开。可达对象可以转冷；不可达对象才可以删。应用层 GC 不应该因为对象进入 Glacier 就认为它不可达；下游读到 archived result 时，也不应该报“对象丢失”，而应该返回“对象已归档，需要 restore”。

第四类是 versioned 对象。启用 S3 Versioning 后，生命周期更复杂。AWS 文档明确提到，普通 expiration 在 versioning-enabled bucket 上会创建 delete marker，非当前版本要通过 `NoncurrentVersionExpiration` 才能永久删除。也就是说，如果只配置 current version expiration，旧版本可能继续占空间；如果 result_ref 保存了 version_id，那个非当前版本可能仍然是 live object，不能随便删。

协调方式可以是：

```text
current version:
  live result 不覆盖，尽量不可变 key。

noncurrent version:
  只有不被任何 result_ref(version_id) 引用时，才允许 NoncurrentVersionExpiration。

delete marker:
  对 expired object delete marker 单独清理，避免列表和成本噪声。
```

第五类是受保护对象。Object Lock、legal hold、合规 retention、replication pending 都可能阻止删除。Lifecycle 文档也提到，对某些 Object Lock 或 replication 状态对象不会执行删除动作。应用层 GC 要把这种结果记为 `delete_deferred`，不要反复报错误，更不能把 metadata 删除掉却留下一个无法删除的受保护对象没人管。

一个可落地的配合模型是：

```text
写入:
  对象写到 live prefix，带 workflow_id、step_id、created_at、retention_class tag。

业务过期:
  metadata 标记 result expired。
  用户再访问时返回 result expired 或 archived，不假装还能读。

GC mark:
  从日志和 metadata 计算 live refs。

GC sweep:
  对不可达对象打 gc=eligible tag，或写入 expired prefix/manifest。

Lifecycle:
  删除 gc=eligible 且超过 grace period 的对象。
  abort incomplete multipart upload。
  transition 冷数据。
  清理 noncurrent versions 和 expired delete markers。

审计:
  GC job 记录删除 manifest、对象数量、字节数和失败原因。
```

为什么不直接让 lifecycle 全权删除？因为 lifecycle 是存储层规则，它不知道 workflow 是否还在 retention 内，也不知道 actor replay 是否还依赖某个 snapshot。举个例子：actor stream logical trim 后，snapshot object 可能是恢复 actor 的唯一入口。如果 lifecycle 因为 snapshot object 过了 30 天就删掉，actor replay 就断了。应用层必须先判断这个 snapshot 是否已被新 snapshot 替代、是否仍在 tail log 起点之前、是否还有审计需求。

为什么也不能只靠应用层 GC？因为应用层 GC 不擅长所有对象存储细节。进程崩溃后残留的 multipart part、海量 noncurrent version、按 storage class 转冷、过期 delete marker、批量对象操作，这些交给 S3 Lifecycle、Inventory、Batch Operations 更合适。应用层负责语义，S3 负责规模化执行和存储成本优化。

对 LogServe 当前项目，可以这样讲：

```text
当前 LogServe 是单机机制验证系统，result store 支持 local 和 S3-compatible MinIO，日志保存 result_ref。现在还没有完整的 lifecycle + GC 闭环，也没有物理删除 segment。合理的生产化路线是：先把 result_ref 结构化，metadata 中加入 expires_at、retention_class、pin；再从 shared log/materialized view 生成 live refs；最后把不可达对象交给对象存储 lifecycle 或批处理删除。这样既保持 replay 正确性，也控制大对象成本。
```

面试里可以这样答：

```text
生命周期策略和应用层 GC 要分工：应用层 GC 判断对象是否仍被 workflow、actor snapshot、审计或用户 pin 引用；S3 Lifecycle 负责低层自动化，比如 incomplete multipart abort、冷存储 transition、过期对象删除、noncurrent version 清理和 delete marker 清理。不能只按 prefix+天数删除 live result，否则会制造悬挂引用；也不能让控制面自己同步删海量对象。比较稳的做法是 metadata 记录 expires_at、retention_class、pin 和 legal hold，GC 算 live set，把不可达对象标记为 gc=eligible，再由 lifecycle 或 Batch Operations 批量删除。LogServe 目前验证了 result_ref 边界，生产化要补这套生命周期闭环。
```

## Q016. 对象压缩和加密的顺序应该如何设计？

**回答：**

通常顺序是先压缩，再加密。原因很直接：压缩算法靠发现重复模式来减少字节数；现代加密算法的输出应该接近随机，密文再压缩基本没有收益。把顺序反过来，通常只会浪费 CPU，还可能让系统误以为自己做了压缩。

对一个大 result object，合理的数据路径可以写成：

```text
业务结果 JSON / protobuf / parquet
  -> 序列化成 canonical bytes
  -> 可选压缩：zstd / gzip / snappy
  -> 计算 plaintext 或 compressed plaintext 的业务 checksum
  -> 客户端加密，或交给 S3 做服务端加密
  -> PutObject
  -> metadata 记录 compression、encryption、checksum、size
```

如果使用 S3 服务端加密，应用上传的是压缩后的对象内容，S3 在服务端写盘前加密。也就是说，应用层看到的顺序仍然是“压缩后上传”，S3 内部再加密。AWS 文档里说 S3 默认会对新对象使用 SSE-S3 加密，也可以按请求或 bucket 默认配置使用 SSE-KMS、DSSE-KMS、SSE-C。这里的服务端加密不会帮你压缩对象，也不会理解你的业务格式。

如果使用客户端加密，顺序更明确：客户端必须先压缩，再加密，然后把密文上传给 S3。S3 只看到密文对象。AWS S3 客户端加密文档也把边界说得很清楚：对象在发送到 S3 之前已经加密，S3 不参与加解密。此时 `Content-Encoding: gzip` 这种 HTTP 语义要慎用，因为对象在 S3 里实际是密文，不是可被普通 HTTP 客户端透明解压的 gzip 内容。更稳的做法是在 result reference 或对象 metadata 里记录：

```json
{
  "content_type": "application/json",
  "compression": "zstd",
  "encryption": "client-side/aws-encryption-sdk",
  "plaintext_sha256": "...",
  "compressed_sha256": "...",
  "ciphertext_sha256": "...",
  "plaintext_size": 104857600,
  "compressed_size": 8388608,
  "ciphertext_size": 8389120
}
```

要不要同时记录三个 checksum？不一定。生产系统通常至少要有一个业务可验证的内容哈希和一个传输/存储层 checksum。我的偏好是：

```text
plaintext_sha256:
  证明解密、解压后拿到的是业务原文。

compressed_sha256:
  适合内容寻址和避免重复压缩。

ciphertext checksum / S3 checksum:
  证明上传下载过程中的对象字节没有坏。
```

如果 result_ref 只保存 `s3://bucket/key`，后面排查会很痛苦。你不知道对象是不是压缩过、用什么算法压缩、解密后要不要再解压，也不知道 key 是按明文 hash 还是密文 hash 生成的。尤其是数据生命周期和迁移场景，缺少这些字段会让老对象很难读。

压缩和加密还有一个安全边界：不要在攻击者可控数据和秘密数据混在一起时盲目压缩。Web 安全里有一类压缩侧信道问题，攻击者通过观察压缩后长度推断秘密。对象存储里的离线 result 不一定有这个风险，但如果 result 里混合了用户可控前缀和敏感 token，并且攻击者能多次选择输入、观察对象大小或响应长度，就要小心。最简单的处理是：秘密字段单独加密或单独存储，不和攻击者可控内容一起压缩。

压缩也会影响读取方式。大对象如果要 range read，压缩格式很关键：

```text
gzip:
  通常需要从流开头解压，随机读取不友好。

zstd seekable / splittable 格式:
  可以更好地支持分块读取，但要保存索引。

parquet / ORC:
  格式内部已经按列和块组织，并带压缩，通常不要再外面套一层 gzip。

已经压缩的媒体文件:
  jpg、png、mp4、zip、模型权重通常再压缩收益很小。
```

这会直接影响 workflow 下游。一个下游 step 只想读结果里的某一段，如果你把整个 2 GB 对象 gzip 后再客户端加密，基本只能整对象下载、解密、解压。对大 result，最好按块或按格式本身的 row group/chunk 来组织对象，而不是只想着“压缩率最高”。

加密还会影响去重。按明文 hash 命名对象，天然能识别相同结果；但如果你做客户端加密，并且每次使用不同随机 nonce 或不同 data key，相同明文会得到不同密文。用密文 hash 做 key 就无法跨次去重。用明文 hash 做 key 又会泄漏“两个对象内容相同”这个事实。这里没有免费答案，要按安全要求选：

```text
追求去重:
  可以用明文 hash 或压缩明文 hash 做内部 key，但要把 key 访问权限收紧。

追求不可关联:
  用随机密文 key 或 attempt/uuid key，不做跨对象内容去重。
```

对 LogServe 当前实现，`S3Store.Put` 会对原始 `data` 算 SHA-256，用这个 hash 生成 `.json` key，然后把原始 bytes 直接 PUT 到 S3-compatible store。它没有压缩层，也没有显式 SSE-KMS header 或客户端加密层。在真实 AWS S3 上，新对象至少会走默认 SSE-S3；在 MinIO 上取决于部署配置。这个实现对机制验证足够，因为它验证了“大结果外置 + result_ref”的边界。生产化可以补：

```text
1. result codec:
   json / protobuf / parquet。

2. compression codec:
   none / zstd / gzip，并记录算法和原始大小。

3. checksum:
   plaintext_sha256、object checksum。

4. encryption:
   SSE-KMS 配置或客户端 envelope encryption。

5. read path:
   GetObject -> verify ciphertext/object checksum -> decrypt -> decompress -> verify plaintext hash。
```

面试里可以这样答：

```text
压缩和加密一般是先压缩、再加密。加密后的字节接近随机，再压缩基本没有收益。S3 服务端加密场景下，应用先把对象压缩好再上传，S3 负责落盘加密；客户端加密场景下，客户端先压缩再加密，S3 只保存密文。metadata 里必须记录 compression、encryption、size、checksum，否则下游不知道怎么读，也无法校验。还要注意压缩侧信道、range read 退化、已压缩格式重复压缩、以及明文 hash 去重可能泄漏相同内容。LogServe 当前还没有压缩和客户端加密层，生产化时应该把这些字段结构化进 result_ref。
```

## Q017. 服务端加密和客户端加密的区别是什么？

**回答：**

服务端加密和客户端加密的分界线是：明文在哪里出现，谁管理加密过程，谁能解密。

服务端加密是应用把明文通过 TLS 发给 S3，S3 在写入磁盘前加密，读取时 S3 校验权限后解密再返回明文。AWS 文档里把 server-side encryption 定义为接收数据的应用或服务在目的地加密；S3 会在写盘时加密对象，在访问时解密对象。对调用方来说，`PutObject` 和 `GetObject` 的数据还是普通明文 bytes。

S3 服务端加密常见有几种：

```text
SSE-S3:
  S3 管理密钥。现在是 S3 新对象的默认加密方式。

SSE-KMS:
  S3 使用 AWS KMS key 做 envelope encryption。
  可以用 AWS managed key，也可以用 customer managed key。
  上传需要 kms:GenerateDataKey，下载需要 kms:Decrypt。

DSSE-KMS:
  双层服务端加密，用于需要多层加密控制的场景。

SSE-C:
  客户提供密钥，S3 用它加解密，但不保存该密钥。
  新 general purpose bucket 默认禁用 SSE-C 写入，确实需要时要显式启用。
```

客户端加密是应用在本地先加密，再把密文上传到 S3。S3 不知道业务明文，也不参与解密。AWS S3 客户端加密文档说，S3 收到的是已经加密的对象，S3 不负责加密或解密。解密必须由客户端拿到对应 key、encryption context、encrypted data key 等材料后完成。

两者差异可以这样说：

```text
明文暴露位置:
  SSE: 客户端和 S3 服务端处理明文，磁盘上是密文。
  CSE: S3 只看到密文，明文只在客户端出现。

密钥控制:
  SSE-S3: S3 管理。
  SSE-KMS: KMS 管理，客户可以控制 key policy、审计、禁用、轮换。
  CSE: 客户端或客户的 key management 系统负责 envelope encryption。

权限模型:
  SSE-KMS: S3 权限 + KMS 权限都要满足。
  CSE: S3 GetObject 只拿到密文，真正可读还要有客户端解密权限。

服务端能力:
  SSE: S3 仍可按普通对象做很多存储层操作。
  CSE: S3 无法理解对象内容；S3 Select、服务端转码、内容扫描、部分代理处理会受限。

排障:
  SSE-KMS: 常见错误是 AccessDenied 或 KMS Decrypt/GenerateDataKey 权限缺失。
  CSE: 常见错误是 key 丢失、encryption context 不匹配、SDK 版本或算法套件不兼容。

密钥丢失后果:
  SSE-S3: 由 S3 管理，用户不处理底层 key。
  SSE-KMS customer managed key: key 禁用或计划删除会影响访问。
  CSE: 客户端 wrapping key 或 encrypted data key 丢了，数据基本不可恢复。
```

SSE-KMS 是很多业务系统的默认折中。它不要求你自己实现加密格式，也能通过 KMS key policy、IAM、CloudTrail、key rotation、禁用 key 等方式获得控制。代价是请求路径多了 KMS 权限和 KMS 额度问题。AWS S3 性能文档也提醒，如果使用 SSE-KMS，要注意 KMS 请求配额。对象存储本身可能没到瓶颈，KMS 先成了 p99 尾部延迟来源。

客户端加密适合更强隔离的场景。比如对象存储管理员不应该看到明文，跨云或跨对象存储迁移时希望密文格式可移植，或者合规要求明文永远不离开特定 trust boundary。AWS Encryption SDK 使用 envelope encryption：每条消息用唯一 data key 加密，再用 wrapping key 加密 data key，并把 encrypted data key 和密文放在加密消息里。它还建议使用 encryption context 作为附加认证数据。这个上下文不是秘密，但可以把 `tenant_id`、`workflow_id`、`step_id`、`purpose` 绑定到密文，防止密文被搬到错误上下文里解密。

客户端加密的麻烦也要讲清楚。你要保存加密材料和算法版本，要做 key rotation，要保证所有读者 SDK 兼容；range read、压缩、checksum、内容类型识别都会更复杂。对象 metadata 也可能泄漏信息，比如 key 名、大小、压缩率、创建时间、tenant prefix。客户端加密保护的是对象内容，不是所有元数据。

对 result reference 来说，可以按敏感度分层：

```text
普通中间结果:
  SSE-S3 或 SSE-KMS 足够，result_ref 记录 kms_key_id / encryption_type。

租户隔离强、审计要求高:
  SSE-KMS + customer managed key，按租户或环境隔离 key。

对象存储不在信任边界内:
  客户端 envelope encryption，result_ref 记录 encryption suite、encrypted data key location、context。

公开下载或短期缓存:
  通常不要客户端加密，否则下载端必须集成解密逻辑。
```

LogServe 当前的 `S3Store` 自己实现了 SigV4 PUT/GET，没有设置 `x-amz-server-side-encryption`。在 AWS S3 上，这意味着会使用 bucket 默认加密，通常是 SSE-S3；如果 bucket 配了默认 SSE-KMS，就走 bucket 配置。它也没有客户端加密，所以 result store 或 S3 服务端在请求处理时能看到对象明文字节。面试里不要把它说成端到端加密实现。

面试里可以这样答：

```text
服务端加密是客户端把明文交给 S3，S3 写盘前加密、读出时解密；SSE-S3 默认开启，SSE-KMS 给客户更多 key policy 和审计控制，但读写还要满足 KMS 权限。客户端加密是应用在本地先加密，S3 只保存密文，S3 管理员和存储层功能都看不到明文；代价是客户端要管理 envelope encryption、key rotation、encryption context、SDK 兼容和恢复流程。LogServe 当前只验证 result_ref 和 S3-compatible 存储，没有实现客户端加密；生产版可以按数据敏感度选择 SSE-KMS 或客户端加密。
```

## Q018. 如何校验从对象存储读回的数据未损坏？

**回答：**

读回校验要分层看。S3 返回 200 不等于业务数据一定正确；TLS 没报错也不等于对象内容就是当初那个 workflow result。更稳的做法是同时校验“对象字节没坏”和“业务内容没被换”。

最底层是传输和服务端校验。上传时可以让 S3 校验 checksum。AWS 文档列了多种 checksum 算法，包括 CRC64NVME、CRC32、CRC32C、SHA1、SHA256、MD5、XXHash 系列。上传时客户端可以提供 checksum，S3 会独立计算并比较，不匹配就拒绝保存。`PutObject` API 也支持 `Content-MD5` 和 `x-amz-checksum-*` 相关 header；不过前面已经说过，ETag 不能被当作通用 MD5。

读取时可以做三件事：

```text
1. HeadObject / GetObject:
   拿到 size、ETag、version_id、checksum header、encryption header。

2. 下载对象 bytes:
   让 SDK 或应用校验 S3 checksum。

3. 应用层再算自己的 hash:
   与 result_ref 里的 sha256、size、content_type、version_id 比较。
```

为什么还需要应用层 hash？因为 S3 checksum 证明的是对象存储里这份对象的完整性，业务 hash 证明的是“这份对象是不是我要的那个结果”。如果同一个 key 被覆盖，S3 可以诚实地告诉你当前对象 checksum 正确，但它不是当初那个 workflow step 输出。result_ref 里保存 `version_id` 和 `sha256`，才能把读回内容和业务事件绑定起来。

一个生产化的 `ResultRef` 可以这样设计：

```json
{
  "uri": "s3://bucket/workflows/wf-1/steps/a/sha256.json",
  "version_id": "3HL4...",
  "size_bytes": 8388608,
  "sha256": "7f...",
  "compression": "zstd",
  "encryption": "SSE-KMS",
  "s3_checksum_algorithm": "CRC64NVME",
  "s3_checksum_value": "..."
}
```

读取流程：

```text
GetObject(uri, version_id)
  -> 如果 404，判断是否过期、delete marker 或悬挂引用。
  -> 如果 403，区分 S3 权限和 KMS Decrypt 权限。
  -> 校验 Content-Length == size_bytes。
  -> 校验 S3 checksum 或 SDK 校验结果。
  -> 如果客户端加密，先验证认证标签，再解密。
  -> 如果压缩，解压。
  -> 计算 plaintext_sha256，与 result_ref 比较。
  -> 解析 JSON / protobuf / parquet schema。
```

顺序不要乱。客户端加密通常使用带认证的加密格式，例如 AES-GCM 或 AWS Encryption SDK 的消息格式。要先让解密过程验证认证标签和 encryption context，确认密文没有被改，再把明文交给解压器。不要对未经认证的密文解压。对“先压缩再加密”的对象，读路径自然是“先验证/解密，再解压，再校验明文 hash”。

如果是 SSE-KMS，S3 checksum 可能以加密形式存储在对象 metadata 里。AWS 文档提到 SSE-KMS 对象的 checksum 作为对象 metadata 会被加密保存。调用方不需要自己处理这个细节，但权限上要注意：读对象除了 `s3:GetObject`，还需要 KMS `Decrypt`。缺 KMS 权限时，你看到的是访问失败，不是 checksum mismatch。

multipart 对象要特别小心。multipart ETag 不等于完整对象 MD5。你要么使用 S3 支持的 full object checksum，要么自己在业务层记录完整明文 hash。不能把 `etag == md5` 写成通用校验逻辑。前面 Q008 已经讲过这个坑，面试里可以直接点出来。

大对象读回还有一个性能问题：每次都整对象下载再 SHA-256，成本可能很高。可以分层优化：

```text
小对象:
  下载后直接算 SHA-256。

大对象:
  使用 streaming hash，一边读一边算，不把整对象放内存。

分块对象:
  每个 chunk 有 checksum，manifest 有 Merkle root 或全量 hash。

冷数据巡检:
  用 S3 Batch Operations 的 Compute checksum，对大量对象在存储侧计算并生成报告。
```

真正上线时，校验失败要有明确处理：

```text
checksum mismatch:
  不能把数据交给下游 step。
  标记 result_corrupt。
  记录 bucket/key/version_id/request_id/checksum。
  如果有副本或 replication，尝试读副本。
  没有副本就让 workflow 进入 repair 或 failed 状态。

size mismatch:
  一般直接失败，不继续解析。

schema mismatch:
  说明对象字节可能没坏，但业务版本不兼容。
```

对 LogServe 当前实现，local store 和 S3 store 都用 `sha256(data)` 生成对象名，这是一个好的起点；但 `result_ref` 只是字符串，读回时 `LoadResult` 直接 `Get`，没有把 ref 中的 hash 解析出来重新校验，也没有记录 version_id、S3 checksum 或 compression。生产化时我会至少补 `sha256` 和 `size_bytes` 字段，并在 `LoadResult` 后做 streaming 校验。

面试里可以这样答：

```text
读回校验要同时看存储层和业务层。上传时用 S3 checksum header 让 S3 校验对象字节；读取时拿 HeadObject/GetObject 的 size、version_id、checksum，再由 SDK 或应用校验对象 checksum。业务层还要把读回数据算 SHA-256，与 result_ref 里的 hash、size、version_id 对上，不能依赖 ETag，因为 multipart、SSE-KMS 等场景下 ETag 不等于完整 MD5。客户端加密时先验证认证标签和 encryption context，再解密、解压、校验明文 hash。LogServe 当前 key 用 SHA-256 命名，但读路径还没有显式校验，生产化应该补结构化 checksum。
```

## Q019. 对象存储延迟抖动如何影响上层 p99？

**回答：**

对象存储的延迟抖动会被上层放大，尤其是 workflow、actor snapshot、LLM checkpoint 这类路径。平均延迟看起来还行，不代表 p99 安全。一个 step 如果在关键路径上等待 `GetObject`，那次对象读取的尾部延迟就会进入 workflow 的尾部延迟。

先看最简单的串行链路：

```text
step A output result_ref
  -> step B GetObject(A.result_ref)
  -> step B compute
  -> step B PutObject(B.result)
  -> append StepSucceeded
```

如果 `GetObject` 平时 30 ms，偶尔 2 s，`PutObject` 平时 50 ms，偶尔 3 s，那么 step B 的 p99 不会接近平均值。它会被慢 GET、慢 PUT、KMS、DNS、连接池、重试、对象大小、解压、校验共同影响。

并行 fan-in 会更明显。假设一个 workflow step 要读取 100 个上游 shard，只要最慢的那个没回来，整个 step 就不能继续：

```text
latency = max(GetObject shard_1 ... shard_100)
```

单个 GET 的 p99 可能是 500 ms，100 个并发 GET 的“这一批里至少一个很慢”的概率会明显变高。面试里不一定要推公式，但可以说清楚：fan-out/fan-in 会把单次对象访问尾部延迟变成批处理尾部延迟。

S3 官方性能文档建议对延迟敏感应用跟踪并重试慢操作。它还提到，遇到高请求率或热点对象时可能收到 HTTP 503 Slow Down，AWS SDK 会用指数退避重试；对小请求，一个经验是 2 秒后重试，后续再 backoff；对大请求，可以跟踪吞吐并重试最慢的一部分请求。这里要注意：重试是改善尾部延迟的工具，也可能增加系统负载和成本。

对象存储抖动的来源很多：

```text
对象大小:
  大对象受网络吞吐、分块下载、校验、解压影响。

连接管理:
  没有连接池时，每次请求都要 TCP/TLS 握手。

DNS 和 endpoint:
  解析缓存、单 IP 绑定、跨区访问会影响延迟。

KMS:
  SSE-KMS 读写路径需要 KMS 权限和请求额度，KMS 抖动会进入 S3 请求尾部。

请求热点:
  某些 prefix 或 key 请求率突然上升时，可能遇到 503 Slow Down。

客户端资源:
  线程池、HTTP 连接池、内存拷贝、解压 CPU、checksum CPU 都会叠加。

冷数据:
  Glacier restore、跨区域访问、缓存未命中会让延迟模型完全不同。
```

上层系统要做的是隔离和削峰，而不是把对象存储当本地磁盘。

第一，区分控制路径和数据路径。LogServe 的设计已经把大对象放到 result store，事件日志只保存 `result_ref`。这能保护 replay 和 metadata path：控制面不应该为了刷新 dashboard 去下载 300 MB result。只有下游真正需要结果时才 dereference。

第二，给对象访问单独的 timeout 和 budget。不要让一个慢 `GetObject` 卡死 worker 线程。可以这样设：

```text
workflow step deadline:
  30s

object get timeout:
  2s 首次尝试 + 退避重试，总预算 8s

compute budget:
  剩余 22s
```

重试要吃总 deadline，不能每层都各自重试三次。否则上层重试、SDK 重试、HTTP 重试叠在一起，p99 会变成无法解释的长尾。

第三，对大对象用并行和分块。S3 文档建议高吞吐下载使用多个连接，可以用 Range GET 或按 multipart 原 part 边界并行下载。对 result store 来说，这意味着大 result 最好有 manifest 和 chunk，或者使用 Parquet/ORC 这类天然分块格式。整对象单连接下载最简单，但 p99 很容易被一条慢连接拖住。

第四，缓存热对象。S3 文档建议重复访问的 working set 可以用 CloudFront、ElastiCache 或应用缓存。LogServe 的 LLM checkpoint cache 就是类似思想：冷启动从源目录或对象存储取 checkpoint，热启动命中 worker-local cache。对 workflow result，也可以做 worker-local cache，但要受 checksum、version_id 和生命周期约束，不能把旧对象当新结果。

第五，限制并发和做 backpressure。对象存储慢时，worker 如果继续拉取大量任务，每个任务都堵在 `GetObject`，会耗尽 goroutine、连接、内存和磁盘临时空间。应该把 object store 访问当成一种资源：

```text
max_concurrent_gets
max_concurrent_puts
max_bytes_in_flight
per-tenant object io quota
object_io_queue_depth
```

第六，观测指标要按层拆开。只看 workflow p99 不够，要把这些字段打出来：

```text
object_get_ms
object_put_ms
object_bytes
object_retry_count
object_status_code
object_checksum_ms
object_decompress_ms
kms_ms 或 kms_error
cache_hit
range_count
```

这样你才能判断 p99 是 S3 慢、KMS 慢、解压慢、checksum CPU 慢，还是下游 compute 慢。LogServe 当前 LLM 部分已经有 `checkpoint_fetch_ms`、`cache_hit`、`model_load_ms` 这类字段；对象 result store 也应该用类似思路拆指标。

对上层语义还有一个细节：对象读取慢不等于 step 失败。它可能只是临时抖动。重试几次后仍失败，才把 step 标成可重试失败；如果读到 404 或 checksum mismatch，那是不同错误，不能按普通慢请求处理。

面试里可以这样答：

```text
对象存储抖动会直接进入 workflow step、actor snapshot replay、LLM checkpoint fetch 的 p99。串行链路里慢 GET/PUT 会加到 step latency 上；fan-in 场景里，一批对象读取的耗时等于最慢那个对象，尾部会被放大。处理上要把控制面和数据面分开，metadata replay 不下载大对象；对象访问要有独立 timeout、总 deadline、指数退避、连接池、并发限制、bytes-in-flight 限制和缓存。大对象用 range/chunk 并行读，热对象放本地 cache。观测上要拆 object_get_ms、retry_count、checksum_ms、decompress_ms、KMS 错误和 cache_hit，否则只看 workflow p99 找不到原因。
```

## Q020. 如何为对象存储访问做重试而不造成重复上传？

**回答：**

对象存储重试的难点不在 GET，而在 PUT。GET 一般是无副作用的，失败后重试只会多花请求成本。PUT、CompleteMultipartUpload、CopyObject 这类写操作要小心：客户端看到超时，不代表服务端没写成功。请求可能已经成功落到 S3，只是响应在网络上丢了。

第一条原则是对象 key 要幂等。不要每次重试都生成一个新 key：

```text
错误做法:
  retry-1 -> workflows/wf-1/steps/a/random-1.json
  retry-2 -> workflows/wf-1/steps/a/random-2.json
  retry-3 -> workflows/wf-1/steps/a/random-3.json

结果:
  一次业务结果留下多个对象，metadata 可能只引用其中一个。
```

更稳的 key 是由业务身份和内容决定：

```text
workflows/<workflow_id>/steps/<step_id>/attempt-<n>/<sha256>.json
```

或者直接用内容 hash：

```text
workflows/<workflow_id>/steps/<step_id>/sha256/<hash>.json
```

LogServe 当前 `S3Store.Put` 就是用 `sha256(data)` 生成对象名。同一 namespace 下相同 data 重试，会 PUT 到同一个 key。这个方向是对的。

第二条原则是用条件写防覆盖。AWS S3 支持 `If-None-Match: *`，意思是只有 key 不存在时才写入；如果已存在，返回 412；如果并发冲突，可能返回 409，官方 API 文档建议 409 时重试上传。这个机制适合防止两个 worker 或两个 attempt 把不同内容写到同一个 key。

处理逻辑可以是：

```text
PutObject(key, body, If-None-Match: *)
  -> 200 OK:
       写入成功，返回 ref。

  -> timeout / connection reset:
       不知道服务端是否成功。
       HEAD key。
       如果存在且 checksum/size 匹配，视为成功。
       如果不存在，重试同一个 key。

  -> 412 Precondition Failed:
       key 已存在。
       HEAD/GET checksum。
       如果 checksum 匹配，视为成功。
       如果不匹配，说明 key 冲突，换 attempt key 或失败。

  -> 409 ConditionalRequestConflict:
       按官方建议重试同一个条件写流程。
```

第三条原则是 metadata 发布也要幂等。对象上传成功后，写 `StepSucceeded(result_ref)` 可能失败。此时不能重新执行 step、重新生成新对象、再写另一个 ref。应该用同一个 `result_ref` 和同一个 idempotency key 重试事件提交。LogServe 当前 step 成功事件的 idempotency key 包含 `workflow_id + step_id + input_hash + succeeded`，能表达“这个输入的这个 step 只有一个成功结果”。

第四条原则是 multipart upload 要把 upload session 当成状态。multipart 不是一次 PUT，而是：

```text
CreateMultipartUpload
UploadPart(part_number)
CompleteMultipartUpload
```

每个 part 可以单独重试。重传同一个 part number 会覆盖旧 part，这本身是可接受的，但应用要记录 part number 和 ETag，完成时用自己记录的 part 列表，不要临时依赖 ListParts 结果拼 complete 请求。AWS multipart 文档也提醒，complete 请求应该使用自己维护的 part number 和 ETag 列表。

multipart 的幂等处理：

```text
UploadPart 超时:
  重传同一个 part_number。
  或 ListParts 检查该 part 是否存在，再决定是否重传。

CompleteMultipartUpload 超时:
  HEAD final key。
  如果对象存在且 checksum/size 匹配，视为成功。
  如果不存在，检查 upload_id 是否还可继续。
  如果 upload session 已无效，重新 initiate，重新上传。

业务取消或最终失败:
  AbortMultipartUpload。
```

第五条原则是重试要分错误类型。AWS SDK 标准重试模式会对 transient error 和 throttling error 做指数退避和 jitter，默认 max attempts 通常是 3。它会把 500、502、503、504、连接 reset、socket timeout 这类当成可重试，把 SlowDown 当成 throttling 类处理。不要自己在外层再无脑套三层重试，否则一次 PUT 可能被放大成很多请求。

比较稳的策略是：

```text
GET:
  可重试 5xx、RequestTimeout、SlowDown、连接错误。
  404 是否重试要看对象是否刚发布；老 ref 的 404 通常不是 transient。

PUT:
  可重试网络错误、5xx、SlowDown。
  超时后先 HEAD 同 key。
  412 不直接失败，先校验已有对象是否就是目标内容。

CompleteMultipartUpload:
  可重试 transient/SlowDown。
  响应未知时先 HEAD final object，再决定。

403:
  通常不是重试问题。检查 IAM/KMS/bucket policy。

400/校验失败:
  通常不是重试问题。修请求或数据。
```

第六条原则是把重试和业务重试分开。对象 PUT 重试是在同一个业务 attempt 内把同一份 bytes 写到同一个 key；workflow retry 是重新执行 step，可能产生新 bytes、新 attempt、新 result_ref。不要让对象层重试偷偷变成业务层重试。

可以这样分层：

```text
object retry:
  same bytes
  same key
  same checksum
  same attempt_id

workflow retry:
  new task attempt
  may produce new bytes
  new attempt_id
  success event still guarded by step idempotency
```

第七条原则是限制重试风暴。对象存储返回 503 SlowDown 时，所有 worker 同时重试会让情况更糟。需要 jitter、retry quota、per-prefix 限速、全局 bytes-in-flight、熔断和 backpressure。AWS SDK 标准模式有 retry quota；adaptive 模式有客户端限速，但官方文档不建议把 adaptive 当通用默认，尤其是一个 client 服务多个资源或多租户时，某个资源被限速会拖慢其他请求。

对 LogServe 当前实现，S3 store 使用一个 30 秒 HTTP client timeout，自己签名发 PUT/GET，没有条件写、没有 HEAD-after-timeout、没有 SDK retry policy，也没有 multipart upload。这对 MinIO 兼容测试和机制验证够用；生产化要补：

```text
1. PutObject 使用 If-None-Match: *。
2. 超时后 HEAD key + checksum 判断是否已成功。
3. result_ref 结构化保存 size、sha256、version_id。
4. SDK 或自研 retry policy 使用指数退避 + jitter + max attempts。
5. multipart upload session 持久化，失败时 abort。
6. metadata append 使用幂等键重试，不重新上传新对象。
```

面试里可以这样答：

```text
对象存储重试要保证同一次业务结果始终写同一个 key。PUT 超时后不能直接换 key 再传一份，而是 HEAD 原 key，校验 size 和 checksum；如果对象已存在且匹配，就把这次重试视为成功。写入时最好用 If-None-Match:* 防覆盖，412 时校验已有对象是否相同，409 按条件写流程重试。multipart upload 要记录 upload_id、part number、part ETag，part 超时重传同一个 part，complete 超时先 HEAD final key，失败或取消时 abort。metadata 发布也要幂等，用同一个 result_ref 和 StepSucceeded idempotency key 重试。LogServe 当前 hash key 有利于幂等，但还缺条件写、HEAD-after-timeout 和正式 retry policy。
```

## Q021. 对象 key 中是否应该包含用户输入？

**回答：**

一般不要把原始用户输入直接放进对象 key。可以把用户输入映射成内部 ID、短 slug、hash 或 metadata，但不要让用户提交的文件名、邮箱、标题、prompt、租户名原样进入 key。对象 key 看起来只是存储路径，实际会出现在日志、监控、预签名 URL、错误信息、访问策略、账单分析、Inventory、复制规则、生命周期规则和排障截图里。把用户输入放进去，风险会比一开始想的大。

S3 官方文档里有几个基础事实要先记住：

```text
key 是 bucket 内对象的唯一名字。
key 是 UTF-8 字节序列，最大 1,024 bytes。
key 大小写敏感。
S3 没有真正目录树，/ 只是 key 里的字符。
控制台和 SDK 会用 prefix/delimiter 模拟文件夹。
某些字符需要额外转义，有些字符最好避免。
包含 . 或 .. 这种 period-only path segment 的 key 会让工具和应用行为不一致。
```

这几个事实放在一起，就能看出为什么“用户输入进 key”容易出事。

第一，隐私泄漏。用户上传文件名可能叫：

```text
张三_身份证扫描件.pdf
company-layoff-plan-2026.xlsx
alice@example.com-medical-report.json
```

如果它进入 key，后续任何有对象列表、日志、错误报告或 URL 的人，都能看到这些信息。即使对象内容加密了，key 本身通常仍是明文元数据。客户端加密也保护不了 key 名。

第二，路径和工具语义混乱。用户输入可能包含 `/`、`\`、`../`、`./`、连续空格、换行、`?`、`#`、`%2F`、Unicode 同形字符。S3 可以接受很多 UTF-8 字符，但你的 SDK、代理、浏览器、Nginx、日志系统、本地缓存、下载工具未必都按同一种方式处理。S3 文档专门提醒 `.` 和 `..` 这样的 path segment 会让路径归一化行为变得不可预测。

第三，访问控制会变复杂。很多 bucket policy、IAM policy、lifecycle rule、replication rule 都按 prefix 管理。如果用户能影响 prefix，就可能把对象放进错误租户、错误生命周期、错误权限范围里。比如你本来想所有用户对象都在：

```text
tenants/<tenant_id>/workflows/<workflow_id>/...
```

结果用户文件名是：

```text
../../admin/audit-log.json
```

S3 自己不会把它当真正父目录，但你的应用、控制台展示、本地缓存同步工具可能会归一化。更麻烦的是，有些工程师看到这个 key 会误判对象属于哪个逻辑目录。

第四，key 会影响成本和运维。对象 key 不是免费语义。prefix 会影响 LIST、Inventory 分析、生命周期规则、冷热分层、批量删除和排障。如果用户输入导致 key 分布混乱，后面做 GC、按租户计费、按 workflow 删除、按日期归档都会变难。用户输入很长时还会逼近 1,024 字节限制。

更稳的设计是：key 只放系统生成的稳定标识，用户可见名字放 metadata 或数据库字段。

```text
推荐:
  tenants/t_9f31/workflows/wf_20260619_001/steps/extract/attempt_1/sha256_7f...json

不推荐:
  张三/实验结果/../../admin/final report (copy)!!.json
```

用户输入如果确实需要进 key，只能进一个经过严格规范化的 display slug，而且它不应该参与权限判断。比如：

```text
tenants/t_9f31/uploads/u_1234/original_filename_slug/report-2026.pdf
```

这里真正的 owner 是 `t_9f31`，真正的对象 ID 是 `u_1234`；`report-2026.pdf` 只是显示辅助字段。就算 slug 丢了或冲突了，也不影响权限和查找。

比较稳的 key 生成规则可以这样写：

```text
tenant_id:
  由系统生成，固定格式，比如 t_<base32>。

workflow_id / step_id:
  由系统生成或从 DSL 中白名单化，不能直接用用户标题。

attempt:
  数字或 UUID。

content hash:
  SHA-256，适合去重和幂等。

extension:
  从实际 content type 映射，不直接信用户文件名后缀。
```

用户原始文件名可以放在 metadata，但 metadata 同样可能出现在日志和 API 响应里。敏感业务里，文件名也应该加密或只保存在权限更严的数据库里。

对 LogServe 当前实现，`S3Store.Put` 的 key 是：

```text
cleanNamespace(namespace) + "/" + sha256(data) + ".json"
```

workflow 路径传入的 namespace 类似：

```text
workflows/<workflow_id>/steps/<step_id>
```

这比直接使用用户 result 名字安全很多。`cleanNamespace` 会把反斜杠转成斜杠，并跳过空段、`.`、`..`。不过生产化时我会更严格：遇到非法 segment 直接报错，而不是静默跳过。静默跳过可能让两个不同输入归一成同一个 namespace，排障时很难发现。

面试里可以这样答：

```text
对象 key 里不要放原始用户输入。key 会出现在 URL、日志、Inventory、生命周期规则、访问策略和排障工具里，用户输入会带来隐私泄漏、路径混乱、prefix 注入和长度问题。更好的做法是 key 使用系统生成的 tenant_id、workflow_id、step_id、attempt_id 和内容 hash；用户文件名或标题只作为显示 metadata，必要时还要脱敏或加密。如果必须放用户可见名字，也只能放经过白名单化的 slug，而且不能参与权限判断。LogServe 当前使用 workflow/step namespace 加 sha256 文件名，这个方向是对的，但生产版应对非法 segment 直接拒绝。
```

## Q022. 如何防止路径穿越和 key 注入？

**回答：**

对象存储不是 POSIX 文件系统，但路径穿越问题仍然存在。原因不是 S3 会把 `../` 当父目录，而是上层系统经常把 key 映射到本地缓存、HTTP 路径、Nginx 代理、预签名 URL、IAM prefix、生命周期规则或审计日志。只要某一层把 key 当路径处理，`../`、`%2e%2e`、反斜杠、重复编码、换行、`?`、`#` 都可能变成问题。

我会把防护拆成四层。

第一层：不要从 raw string 拼 key。所有 key 都由 segment builder 生成：

```text
BuildObjectKey(
  tenantID,
  workflowID,
  stepID,
  attemptID,
  contentHash,
)
```

每个 segment 都按自己的规则校验，而不是最后对整条字符串做一次 replace。

```text
tenant_id:
  ^t_[a-z0-9]{16,32}$

workflow_id:
  ^wf_[a-z0-9_-]{8,64}$

step_id:
  来自 DSL 定义，但限制为 [a-zA-Z0-9_-]，长度有限。

hash:
  ^[a-f0-9]{64}$
```

如果某个 segment 不符合规则，直接拒绝。不要“帮用户修正”。安全场景里，静默修正常常制造碰撞。

第二层：拒绝危险 path segment。即使 S3 文档说某些相对路径元素在特定规则下是合法的，应用也不应该接收它们。对象 key 一旦穿过多个系统，就会有工具做路径归一化。建议直接拒绝：

```text
""
"."
".."
包含 "/"
包含 "\\"
包含 NUL、控制字符、CR、LF
包含 URL path 分隔语义的编码结果，比如 %2F、%5C、%2E%2E
超出长度限制
```

还有一个容易漏的点：先解码，再校验，而且只允许一种规范表示。比如输入是 `%252e%252e%252fadmin`，解码一次是 `%2e%2e%2fadmin`，再被代理或下游解码一次就变成 `../admin`。处理 HTTP 输入时要明确：在哪里 URL decode、decode 几次、decode 后再做 allowlist。不要让不同层各自猜。

第三层：本地文件映射必须做目录边界检查。对象 key 经常会被下载到 worker-local cache：

```text
cacheRoot + "/" + objectKey
```

这一步如果没保护，`../../` 就真的变成本地文件路径穿越。正确做法是：

```text
1. 把 key segment 化。
2. 每个 segment 通过 allowlist。
3. Join 到 cacheRoot。
4. Clean/Abs。
5. 检查最终路径仍在 cacheRoot 内。
6. 写文件时用安全权限，不跟随不可信 symlink。
```

LogServe 的 `LocalStore.Get` 已经做了一个重要保护：`filepath.Join` 后 `Clean`，再检查 `cleanPath` 是否仍在 store 根目录下。这个检查能挡住很多从 ref 到本地文件的逃逸。`cleanNamespace` 也会跳过 `.` 和 `..`。不过，生产版我还是建议从“跳过”改成“拒绝”，并且把 key builder 和 ref parser 分开，不让外部传任意 `local://` 或 `s3://` 字符串进来。

第四层：权限判断不能基于用户可控 prefix。不要用下面这种逻辑：

```text
if strings.HasPrefix(key, "tenants/"+userInputTenant+"/") {
  allow
}
```

更稳的是先从认证上下文拿 `tenant_id`，再由服务端生成 prefix：

```text
tenantID = authContext.TenantID
key = BuildObjectKey(tenantID, workflowID, stepID, attemptID, hash)
```

读取时也一样。用户不能直接提交完整 `s3://bucket/key` 让系统读取。API 应该接收业务 ID：

```text
GetWorkflowResult(workflow_id, step_id)
```

服务端从 metadata 找到 result_ref，确认这个 workflow 属于当前 tenant，再读取对象。这样就算用户知道别人的 key，也不能绕过业务授权。

还要防日志注入。key 里如果允许换行、制表符、控制字符，日志会被污染，审计工具也可能被欺骗。所以 key 和用户显示名进入日志前都要转义或结构化输出。不要把完整预签名 URL 或含敏感用户输入的 key 直接打进日志。

对 S3 请求本身，要使用 SDK 或可靠的 URL escaping。LogServe 当前 `S3Store.objectURL` 会对 path segment 做 `url.PathEscape`，这比手写拼 URL 安全。但它仍然依赖上游 namespace 是可信业务结构。生产化时应补单元测试：

```text
../secret
..%2Fsecret
%252e%252e%252fsecret
a/./b
a//b
a\\b
a\nb
非常长的 Unicode 文件名
```

面试里可以这样答：

```text
防路径穿越和 key 注入的核心是不要拼 raw key。key 应该由服务端用受控 segment 生成，每个 segment 做 allowlist 校验，拒绝空段、.、..、斜杠、反斜杠、控制字符和重复编码后的危险字符。用户不能直接传 s3://bucket/key 让系统读取，只能传 workflow_id、step_id 这类业务 ID，服务端再从 metadata 找 ref 并做 tenant 鉴权。本地缓存落盘时要 Join、Clean、Abs，并确认最终路径仍在 cacheRoot 内。LogServe 当前 LocalStore 有目录逃逸检查，S3 URL 也做 PathEscape，但生产版应该把非法 segment 从“跳过”改成“拒绝”。
```

## Q023. MinIO 与 AWS S3 的兼容边界可能在哪里？

**回答：**

MinIO 的价值在于它支持 S3 API，适合本地开发、私有化部署、CI、边缘环境和一部分生产对象存储场景。但“S3-compatible”不等于“所有 AWS S3 行为都一模一样”。面试里要把 API 兼容、语义兼容、运维能力、性能模型和云厂商周边能力分开讲。

MinIO 官方兼容页列出了支持的 S3 API，并说明具体 API 的参考文档看 Amazon S3。它支持常见对象 API，例如 `PutObject`、`GetObject`、`HeadObject`、`ListObjectsV2`、`DeleteObject`、`RestoreObject`、`SelectObjectContent`，也支持 multipart upload 相关 API。它还列出支持的条件头，例如 `If-Match`、`If-None-Match`、`If-Modified-Since`。这些能力足够覆盖 LogServe 目前的 result store：创建 bucket、PUT 大结果、GET result_ref。

边界通常出现在下面几类地方。

第一，AWS 专有服务集成。AWS S3 和 IAM、KMS、CloudTrail、CloudWatch、S3 Inventory、Batch Operations、Macie、Storage Lens、S3 Access Points、VPC Endpoint、Multi-Region Access Point、Transfer Acceleration、EventBridge、Lambda 等深度集成。MinIO 可以有自己的身份认证、KMS、审计、Prometheus、bucket notification、tiering，但不是同一套 AWS 控制面。你把代码从 MinIO 切到 AWS 时，权限、审计、KMS、网络和监控都要重验。

第二，API 覆盖不是 100%。MinIO 官方兼容页明确列出一些不支持项，例如 `GetObjectAcl`、`PutObjectAcl`。它还写到 multipart upload 有差异：`ListMultipartUploads` 需要 exact object name 作为 prefix，`PutBucketLifecycle` 不支持 `AbortIncompleteMultipartUpload` lifecycle action。这个点很适合面试讲：如果你的业务依赖 AWS lifecycle 自动清理 incomplete multipart upload，在 MinIO 上就不能只靠同一条 lifecycle 配置，应用层要主动记录 upload_id 并 abort。

第三，一致性和故障模型不同。AWS S3 是托管的大规模分布式服务，强 read-after-write 一致性由 AWS 承担。MinIO 是你部署和运维的对象存储，数据可靠性取决于 erasure coding、磁盘、节点、网络、负载均衡、时钟、证书、KMS、升级和容量规划。测试 MinIO 通过，说明 S3 API 调用路径能跑通，不等于 AWS 上的延迟、错误码、限流和跨区语义完全一样。

第四，endpoint 和签名细节。AWS S3 常见 virtual-hosted-style endpoint，bucket 在 host 里；本地 MinIO 常见 path-style：

```text
AWS:
  https://bucket.s3.us-east-1.amazonaws.com/key

MinIO:
  http://minio:9000/bucket/key
```

SigV4 region、TLS、证书、bucket DNS 名称、path escaping、预签名 URL host 都可能不同。LogServe 当前 `S3Store` 自己拼的是 path-style URL：

```text
endpoint + "/" + bucket + "/" + escapedKey
```

这对 docker-compose 里的 MinIO 很方便；切 AWS 时也可能工作，但生产 AWS S3 推荐形态、证书和兼容细节要单独验证。更稳的是用官方 AWS SDK 或成熟 S3 client，让它处理 endpoint、region、签名和重试。

第五，ETag、checksum、加密、versioning 和 lifecycle 细节要用测试锁住。比如：

```text
single PUT ETag 是否等于 MD5。
multipart ETag 格式。
SSE-S3 / SSE-KMS / SSE-C 行为。
bucket versioning 和 delete marker。
object lock 和 legal hold。
lifecycle transition/expiration。
RestoreObject 语义。
conditional write 的 412/409 行为。
```

不要靠“兼容”两个字推断这些细节。把你依赖的语义写成兼容性测试。

第六，冷热分层和归档。MinIO AIStor 文档里有 object tiering：对象数据可以从 primary tier 移到 secondary tier，metadata 留在 primary tier；需要时可以透明取回，也可以用 `mc ilm restore` 创建临时恢复副本。AWS S3 Glacier Flexible Retrieval / Deep Archive 则有明确的 restore request、retrieval tier、临时副本和恢复时间模型。两者都能表达冷热分层，但恢复时间、计费、API、监控和失败模式不一样。

对 LogServe 来说，MinIO 是很好的本地 result store 后端。当前 `deployments/docker-compose.yml` 里也配置了：

```text
LOGSERVE_RESULT_STORE=minio
LOGSERVE_S3_ENDPOINT=http://minio:9000
LOGSERVE_S3_BUCKET=logserve-results
```

这说明项目验证的是 S3-compatible result_ref 路径，不是 AWS S3 全托管生产能力。面试时要这样划清楚：我用 MinIO 验证对象存储边界、result_ref、PUT/GET 和本地部署便利性；如果上 AWS，要补 AWS IAM/KMS/lifecycle/cost/latency/retry/observability 的端到端验证。

面试里可以这样答：

```text
MinIO 与 AWS S3 的兼容主要在 S3 对象 API 层，常见 PutObject、GetObject、HeadObject、ListObjects、multipart、条件头都能覆盖很多业务。但兼容不等于 AWS S3 全部语义和生态都一样。边界包括不支持的 ACL API、multipart lifecycle 差异、endpoint/path-style/签名细节、IAM/KMS/CloudTrail/Inventory/Batch Operations 等 AWS 专有集成、冷热分层和归档恢复模型、错误码和限流行为。LogServe 用 MinIO 验证 S3-compatible result store 是合理的，但上线 AWS 或私有化生产时，要把依赖的 API 和语义写成兼容性测试，而不是只看“兼容 S3”。
```

## Q024. 对象存储成本如何由容量、请求次数、跨区流量组成？

**回答：**

对象存储成本不能只看“每 GB 每月多少钱”。AWS S3 价格页把成本拆成几类：storage、request and data retrieval、data transfer and transfer acceleration、management and insights、replication、transform/query 等。面试里如果只说容量成本，会漏掉很多真实账单来源。

可以按这个公式理解：

```text
月成本 =
  存储容量成本
  + 请求成本
  + 数据取回成本
  + 数据传输成本
  + 生命周期/监控/Inventory/Batch/复制等管理成本
  + KMS、日志、计算侧间接成本
```

第一块是容量。容量按对象大小、存储时间、Region、storage class 算。S3 Standard、Standard-IA、One Zone-IA、Intelligent-Tiering、Glacier Instant Retrieval、Glacier Flexible Retrieval、Deep Archive 价格不同。低频和归档类通常更便宜，但会带来最小存储时长、最小计费对象大小、取回费用或 restore 等成本。比如 Standard-IA 有 30 天最小存储时长和 128 KB 最小计费对象大小；Glacier Flexible Retrieval 通常有 90 天最小存储时长；Deep Archive 是 180 天。小对象很多时，最小计费对象大小和对象元数据成本会把账单抬高。

第二块是请求。`PUT`、`COPY`、`POST`、`LIST`、`GET`、lifecycle transition、restore 都可能计费，费率还按请求类型和 storage class 不同。AWS 定价页明确说，用控制台浏览对象也会产生 GET、LIST 等请求。对 LogServe 这类系统，很多人会低估这块：

```text
每个 step 写一个 result:
  至少一个 PUT。

下游读取上游 result:
  至少一个 GET，有时还会先 HEAD。

GC:
  LIST / Inventory / DeleteObjects / tagging。

multipart:
  CreateMultipartUpload + 多个 UploadPart + CompleteMultipartUpload。

重试:
  每次失败重试也可能增加请求数。
```

如果 result 被拆成很多小对象，请求成本和 LIST 成本会变得明显。反过来，如果所有东西都打成一个超大对象，下游 range read、partial retry、GC 和生命周期管理又会变差。对象大小要按访问模式设计，不是越大越好，也不是越碎越好。

第三块是取回成本。低频和归档类 storage class 便宜，但读出来可能收费。Standard 通常没有 retrieval charge；Standard-IA、One Zone-IA、Glacier Instant Retrieval、Glacier Flexible Retrieval、Deep Archive 都有不同的取回或 restore 成本。结果对象如果经常被下游 workflow 读，就不适合太早放进归档层。否则你会把容量省下来的钱又花在 retrieval 上，还把延迟变长。

第四块是数据传输。对象在同一个 Region 内由某些 AWS 服务访问，成本模型和跨 Region、跨 AZ、出公网不一样。跨 Region replication、跨 Region 读取、从 S3 出公网下载、Transfer Acceleration 都可能带来额外费用。对多区域 workflow 来说，一个常见错误是把 result bucket 放在 A Region，worker 跑在 B Region；每次读取 result 都在付跨区流量和延迟。

第五块是管理功能。S3 Inventory、Storage Lens、Batch Operations、Replication、Object Lambda、事件通知、CloudTrail data events、KMS 请求都可能单独计费或带来间接成本。SSE-KMS 尤其要注意：每次 PUT/GET 可能触发 KMS 相关操作，成本和配额都要算。

第六块是生命周期的副作用。lifecycle transition 本身可能有请求费用；过早 transition 到 IA 或 Glacier 类，可能触发最小存储时长费用；删除刚写入不久的对象，也可能仍按最小天数收费。GC 策略如果把对象写入后几小时就频繁转层或删除，账单可能比留在 Standard 更难看。

一个面向 result store 的成本模型可以这样做：

```text
按对象记录:
  tenant_id
  workflow_id
  size_bytes
  storage_class
  created_at
  expires_at
  get_count
  put_count
  bytes_egress
  restore_count

按月聚合:
  GB-month
  PUT/LIST/GET 请求数
  retrieval GB
  cross-region/outbound GB
  lifecycle transition count
  KMS request count
```

这样你才能回答“为什么这个租户贵”。只看 bucket 总容量是不够的。

对 LogServe 当前项目，`result_ref` 只记录字符串，没有存 `size_bytes`、`storage_class`、`get_count`、`tenant_id` 这些成本字段。作为机制验证没问题；生产化要补对象级计量。否则你只能从对象存储账单倒推，很难按 workflow 或 tenant 解释成本。

面试里可以这样答：

```text
对象存储成本由容量、请求、取回、数据传输和管理功能共同组成。容量按 GB-month、Region 和 storage class 计费；PUT/GET/LIST、multipart、restore、lifecycle transition 都可能产生请求成本；低频和归档类读出来还有 retrieval 或 restore 成本；跨 Region、出公网、复制和 Transfer Acceleration 会产生流量成本。还有 KMS、Inventory、Batch Operations、Storage Lens、CloudTrail data events 这些间接费用。对 LogServe 这种 result store，生产版要按对象记录 size、storage class、get/put 次数、egress、restore 和 tenant，否则无法解释成本。
```

## Q025. 冷热分层和归档策略如何影响恢复时间？

**回答：**

冷热分层最容易被误解成“便宜一点但读得慢一点”。实际设计时要问得更具体：对象是不是马上可读？读之前是否要 restore？restore 要多久？恢复出来的是永久对象还是临时副本？恢复期间 workflow 怎么表现？过期后 metadata 还在不在？

S3 storage class 可以粗略分成几类：

```text
S3 Standard:
  热数据，毫秒级访问，适合近期 workflow result、正在调试的对象、频繁下游读取。

S3 Standard-IA / One Zone-IA:
  低频访问，但仍要求毫秒级读取。容量便宜一些，取回有成本，适合保留但偶尔读的结果。

S3 Intelligent-Tiering:
  自动按访问模式移动对象。Frequent 和 Infrequent tier 仍是低延迟访问；Archive Access 和 Deep Archive Access 需要异步恢复。

S3 Glacier Instant Retrieval:
  归档但仍支持毫秒级访问，适合很少读但一旦读又要快的数据。

S3 Glacier Flexible Retrieval:
  需要 restore。Expedited 通常 1-5 分钟，Standard 通常 3-5 小时，Bulk 通常 5-12 小时。

S3 Glacier Deep Archive:
  更便宜，恢复更慢。Standard 通常 12 小时内，Bulk 通常 48 小时内。
```

这些恢复时间会直接改变上层语义。热对象的 `GetObject` 可以在 workflow step 内完成；Deep Archive 对象不可能在普通 step timeout 内恢复。你不能让一个 30 秒 deadline 的 worker 去同步等 12 小时 restore。

比较稳的状态机是：

```text
ResultAvailable:
  对象可直接 GET。

ResultArchived:
  metadata 还在，但对象需要 restore。

RestoreRequested:
  已发起 restore，等待对象临时可读。

RestoreAvailable:
  临时恢复副本可读，expires_at 明确。

ResultExpired:
  对象已按生命周期删除，不能再恢复，只保留 metadata 或摘要。
```

用户或下游任务读到归档对象时，不应该得到普通 500。它应该得到一个可解释的状态：

```text
result is archived, restore requested, estimated restore tier = Standard
```

如果业务允许异步，可以先返回 restore job id，等恢复完成后再继续 workflow。对严格同步的 workflow，要在调度前检查 result storage class，发现需要 restore 就不要把 step 分配给普通 worker。

生命周期策略要和业务 SLA 对齐。比如：

```text
0-7 天:
  Standard，方便用户调试和下游重跑。

7-30 天:
  Standard-IA 或 Intelligent-Tiering，仍可快速读取。

30-180 天:
  Glacier Instant Retrieval 或 Flexible Retrieval，按恢复 SLA 选。

180 天后:
  Deep Archive 或删除，只保留摘要和审计 metadata。
```

这只是示例，不是通用答案。真正策略要看 result 的访问曲线、合规保留期、用户是否会回看、恢复 SLA、成本预算和对象大小。小对象很多时，直接转 IA 或归档未必省钱；频繁 restore 的归档对象也不便宜。

归档还会影响 GC。一个对象进入 Glacier 不代表不可达，它仍然可能是 workflow 审计或 actor replay 的 root。应用层 GC 要区分：

```text
archived but live:
  不可删除，只是恢复慢。

expired and unreachable:
  可以删除。

restore temporary copy:
  到期后临时副本会消失，原归档对象仍按策略存在。
```

还要注意恢复后的读取窗口。S3 Glacier Flexible Retrieval 和 Deep Archive restore 通常是创建一个临时可用副本，restore 请求里要指定保留天数。这个窗口过了，对象会回到需要再次 restore 的状态。metadata 里应该记录：

```text
storage_class
archive_status
restore_requested_at
restore_available_until
restore_tier
last_restore_error
```

MinIO 的 tiering 模型也要单独看。MinIO 可以把对象数据转移到 secondary tier，primary tier 保留对象 metadata；访问时可以透明取回，或者用 `mc ilm restore` 创建临时恢复副本。这个语义和 AWS Glacier 的计费、恢复层级、状态头不完全一样。跨后端抽象时，不要把 AWS 的 `RestoreObject` 语义硬套到所有 S3-compatible store。

对 LogServe 当前项目，actor snapshot 和 workflow result 都可能走 result store。这里要特别小心 actor snapshot：如果 actor stream 已经 logical trim，snapshot object 就是 replay 的入口。把它归档到需要 12 小时恢复的层，会让 actor recovery 从秒级变成小时级；如果 lifecycle 删除它，actor replay 直接断。生产策略应该把“replay-critical snapshot”和“普通历史 result”分开：

```text
replay-critical actor snapshot:
  保持热或低频即时访问层，直到被新 snapshot 安全替代。

workflow debug result:
  短期热存，过期后可归档或删除。

审计 result:
  可归档，但 metadata 要明确恢复 SLA。
```

面试里可以这样答：

```text
冷热分层影响的不只是成本，还会改变恢复时间和上层状态机。Standard、Standard-IA、Glacier Instant Retrieval 可以做到低延迟读取，但 IA 和归档即时层有取回成本；Glacier Flexible Retrieval 需要 restore，Expedited 通常分钟级，Standard 是小时级，Bulk 更慢；Deep Archive 恢复通常按 12 小时或 48 小时级别设计。上层不能让普通 workflow step 同步等待归档恢复，而要把 result 标成 archived、restore_requested、restore_available 或 expired。LogServe 里 actor snapshot 如果是 replay 入口，就不能随便转到慢归档层；普通历史 result 可以按 SLA 和成本转冷。
```

## Q026. result reference 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

result reference 的核心目标是把“控制面状态”和“数据面大对象”分开。控制面只记录稳定、可 replay、可索引、可审计的引用；大对象本体放到 result store、S3、MinIO、本地对象目录或其他 blob store。它不是单纯的“为了省数据库空间”，也不是一个安全功能。更准确地说，它是一种边界设计。

最小形态是：

```text
workflow event / metadata:
  status = StepSucceeded
  result_ref = s3://bucket/workflows/wf-1/steps/a/sha256.json
  result_size = 300MB
  result_sha256 = ...

object store:
  s3://bucket/workflows/wf-1/steps/a/sha256.json
  body = 大结果本体
```

如果只能选一个主目标，我会说 result reference 首先服务正确性，其次服务性能和可维护性，安全性是它带来的边界条件之一，但不是自动成立的安全保证。

为什么说首先是正确性？因为 workflow、actor、LLM runtime 的状态真相应该能被稳定 replay。事件日志里应该记录“哪个 step 成功了、成功结果在哪里、结果的 hash 和版本是什么”，而不是塞进几百 MB 的 payload。恢复时，控制面只要重建状态图和 result_ref，不需要把所有历史大对象读一遍。这样系统才能回答：

```text
这个 step 是否已经成功？
这个结果是否已经发布？
下游应该读哪个对象？
重启后能不能继续调度？
GC 能不能判断对象是否仍然可达？
```

这些都是 correctness 问题。没有 result_ref，很多系统会把大结果直接塞进 metadata 或事件 payload；日志 replay 会变慢，甚至因为单条记录过大而失败。更糟糕的是，大结果生命周期和控制面日志生命周期绑在一起：想保留日志就被迫保留结果，想删结果又破坏 replay 语义。

性能是第二层收益。控制面小了，append log、metadata update、dashboard 查询、workflow replay 都更快。大对象只在真正需要的时候读取。一个 dashboard 页面想看 workflow 状态，不应该下载 300 MB 结果；一个 replay 过程想确认 step 成功，不应该解压模型输出。result reference 把热路径从“搬大对象”变成“搬短引用”。

可维护性也很重要。对象存储有自己的能力：multipart upload、checksum、lifecycle、storage class、versioning、Object Lock、Inventory、Batch Operations、跨区域复制。控制面数据库和事件日志有自己的能力：事务、幂等、replay、索引、审计。result reference 让这两套系统各做自己擅长的事。以后从本地文件切 MinIO，从 MinIO 切 AWS S3，理论上不应该改 workflow 状态机，只要 `Store.Put/Get` 的语义不变。

安全性不是自动获得的。result reference 可以降低日志泄漏面：日志里不存大结果本体，只存一个引用。但如果引用是可直接访问的预签名 URL，或者任何拿到 `s3://bucket/key` 的人都能读对象，那安全性反而更差。正确的做法是：

```text
日志:
  保存稳定内部 ref，不保存长期可用的 presigned URL。

读取:
  先做业务鉴权，再 dereference。

对象:
  使用 IAM/KMS/bucket policy/tenant prefix 控制访问。

引用:
  记录 checksum、version、size，防止读错对象。
```

所以 result reference 不是“安全机制”，而是“给安全机制留出清晰边界”。

LogServe 当前实现体现了这个思路。`materializeResult` 会根据 inline threshold 决定小结果 inline，大结果调用 `resultStore.Put`，然后在 `StepSucceeded` 或 `WorkflowCompleted` 事件里写 `ResultRef`。这说明项目已经把控制面状态和大结果本体分开了。边界也要承认：当前 `result_ref` 还是 string，没有结构化记录 `version_id`、`size_bytes`、`checksum`、`storage_class`；S3 store 也还没有条件写和 HEAD-after-timeout。这些是生产化要补的内容。

面试里可以这样答：

```text
result reference 的核心目标是把控制面和数据面分开：日志和 metadata 只记录结果引用，大对象本体放到对象存储。它首先解决正确性问题，因为 replay、去重、恢复、GC 都需要一个稳定的结果指针；其次解决性能问题，避免控制面搬大对象；再带来可维护性，让对象生命周期、分层、校验和复制交给对象存储。安全性不是自动成立的，它只是让日志不直接保存敏感大结果，真正的访问控制还要靠 IAM、KMS、业务鉴权和短期授权。LogServe 当前实现了这个机制的简化版。
```

## Q027. result reference 的典型适用场景和不适用场景分别是什么？

**回答：**

result reference 适合“结果本体大、控制面只需要稳定引用”的场景。不适合“结果很小、必须立即内联参与状态判断、或者对象和 metadata 必须强事务提交”的场景。这个边界要说清楚，否则很容易把所有结果都扔进对象存储，最后反而把系统弄慢。

典型适用场景有几类。

第一类是大结果。比如模型输出文件、批处理结果、Parquet 分片、图片、压缩包、向量索引、训练 checkpoint、actor snapshot。它们可能从几 MB 到几十 GB，放进 metadata 或事件日志会拖垮 replay、备份和查询。对象存储适合这种大 payload。

```text
workflow step:
  生成 800 MB parquet。

event:
  StepSucceeded(result_ref=..., sha256=..., size=...)

下游:
  需要时按 ref 读取。
```

第二类是跨进程、跨 worker、跨语言传递结果。LogServe 有 Go 控制面、worker、本地 executor pool、Python SDK。大结果如果放在进程内内存或本地临时文件里，worker 重启后就丢了；放到 result store 后，下游可以通过 ref 找到它。这个 ref 也适合跨语言，因为它只是协议字段，不是 Go 指针或 Python 对象。

第三类是需要异步消费的结果。上游完成后，下游可能几秒后、几分钟后甚至几天后才读。事件日志保存 ref，比把对象塞在消息队列里更稳。消息队列和控制面不需要承载大 payload。

第四类是需要独立生命周期的结果。workflow 事件可能保留一年用于审计，但结果本体只保留 30 天；或者 result 保留 7 天，actor snapshot 保留到被新 snapshot 替代。result reference 让“状态保留”和“对象保留”可以分开治理。

第五类是需要校验、去重或不可变发布的结果。如果对象 key 按 content hash 或 attempt id 生成，事件里保存 checksum、version_id 和 size，下游可以确认读到的是哪一份结果。对象存储的 versioning、Object Lock、checksum、Inventory 也能帮忙。

不适用场景也很常见。

第一，小结果不一定要 object store。几十字节、几 KB 的 JSON 结果，直接 inline 往往更好。inline 结果能直接展示、replay 不需要额外 GET、调试简单。LogServe 也用了 inline threshold，这是合理折中。不要为了“架构统一”把 `"ok"` 这种结果也写 S3。一次 PUT + GET 的延迟和成本可能比结果本身还大。

第二，强同步低延迟路径要小心。如果一个 RPC 要在 20 ms 内返回结果，强行先写对象存储再写 metadata，p99 可能被对象 PUT、KMS、网络和重试拉长。此时可以 inline、写本地临时缓存，或者把对象上传放到后台异步路径，但不能把成功状态提前发布成可读 ref。

第三，必须跨 metadata 和对象本体强事务的场景不适合直接用裸 result_ref。S3 和数据库之间没有原生分布式事务。你可以设计 outbox、upload session、pending 状态、补偿 GC，但不能假装 `PutObject + INSERT metadata` 是一个原子事务。如果业务要求“两个系统必须一起提交或一起回滚”，要么换存储模型，要么承认并处理半成功状态。

第四，高频随机读写的小块数据不适合普通对象存储。S3 不是 POSIX 文件系统，不支持普通意义上的原子 append、原地修改、文件锁和低延迟小块写。把 mutable state、队列、锁、热索引塞进 result object，会踩到对象存储语义边界。actor 的当前状态可以通过日志和 snapshot 管理，但 actor 的每条 command 不应该靠改同一个 object 完成。

第五，不能把 result reference 当成长期授权链接。预签名 URL 是临时 bearer token，不是稳定 result_ref。日志里应该保存内部 ref，用户下载时再按权限生成短期 URL。

第六，下游必须全文扫描每个小结果时，ref 可能增加复杂度。如果每个 workflow 有 10000 个 1 KB 分片，下游要逐个 GET，性能和请求成本会很差。此时应合并成 manifest、batch object、Parquet row group，或者直接 inline 一部分结果。

面试里可以这样答：

```text
result reference 适合大结果、异步消费、跨 worker 传递、需要独立生命周期和可校验发布的场景，比如 workflow 大输出、actor snapshot、LLM checkpoint、批处理文件。不适合很小的 JSON 结果、强同步低延迟返回、需要跨数据库和对象存储强事务的写入、频繁原地修改的小块状态，也不能当长期授权 URL 使用。LogServe 的 inline threshold 就是这个边界：小结果 inline，大结果 materialize 到 result store，再在日志里保存 ref。
```

## Q028. result reference 和相近概念最容易混淆的边界在哪里？

**回答：**

result reference 最容易和几个概念混在一起：对象 key、预签名 URL、cache key、content hash、数据库外键、manifest、artifact ID。它们看起来都像“一个字符串指向另一个东西”，但语义不一样。面试时把这些边界说清楚，比背一堆 S3 API 更有价值。

第一，result reference 不是预签名 URL。预签名 URL 是临时授权材料，谁拿到谁能在有效期内访问。它会过期，泄漏后等于授权泄漏，不适合写进长期事件日志。result_ref 应该是稳定内部引用：

```text
稳定 ref:
  s3://bucket/workflows/wf-1/steps/a/sha256.json

临时下载 URL:
  https://bucket.s3...?...X-Amz-Signature=...
```

日志保存前者。用户下载时，服务端先做业务鉴权，再临时生成后者。

第二，result reference 不只是 object key。`bucket/key` 只能定位当前对象；完整 result reference 还应该包含对象版本、大小、checksum、content type、压缩算法、加密信息、创建时间、过期时间。S3 官方文档说启用 versioning 后，bucket + key + version 才能唯一定位一个对象版本。没有 version_id 时，同一个 key 被覆盖后，旧 workflow 可能读到新内容。

第三，result reference 不是 content hash。content hash 可以作为 key 的一部分，也可以用于校验和去重，但 hash 本身不告诉你去哪里读、用什么权限读、对象是否过期、是否压缩、是否加密。一个 `sha256:7f...` 不是完整 result_ref，除非系统有明确的 content-addressable store 解析规则。

第四，result reference 不是数据库外键。数据库外键通常有事务、约束和级联删除语义；对象存储没有参与数据库事务。metadata 中的 `result_ref` 指向对象存储，系统必须自己处理 orphan object、broken ref、GC、重试、权限和 lifecycle。把它当外键，会低估很多边界。

第五，result reference 不是 cache key。cache key 指向的是可丢弃副本；result_ref 指向的是业务结果的 durable location。worker-local cache 可以用 result_ref 作为 key，但 cache miss 后必须能从权威对象存储重新加载。不能把 cache path 写进事件日志当稳定结果。

第六，result reference 和 manifest 也不同。manifest 是描述多对象结果的一个对象，里面可能列出许多 part、schema、checksum。result_ref 可以指向 manifest：

```text
result_ref = s3://bucket/results/wf-1/manifest.json
manifest:
  part-00000.parquet
  part-00001.parquet
  schema
  checksums
```

此时 result_ref 指向“结果入口”，真正的数据在 manifest 里。GC 时要从 manifest 继续 mark 所有 part。

第七，result reference 不是 artifact ID。artifact ID 是业务层名字，比如 `artifact_123`；result_ref 是存储层定位和校验信息。业务 API 可以暴露 artifact ID，内部 metadata 再映射到 result_ref。这样可以避免把 bucket/key 暴露给用户。

LogServe 当前实现里，`ResultRef` 是 protobuf 中的 string，内容可能是 `local://...` 或 `s3://bucket/key`。这对机制验证足够，但它把“引用定位”和“引用元数据”压缩成了一个字符串。生产化后我会拆成结构体：

```protobuf
message ResultRef {
  string uri = 1;
  string version_id = 2;
  int64 size_bytes = 3;
  string sha256 = 4;
  string content_type = 5;
  string compression = 6;
  string encryption = 7;
  int64 created_at_ms = 8;
  int64 expires_at_ms = 9;
}
```

面试里可以这样答：

```text
result reference 不是预签名 URL，预签名 URL 是临时授权，不能长期写日志；它也不只是 S3 key，完整 ref 还应包含 version、size、checksum、content type、压缩和过期信息。它不是数据库外键，因为对象存储不参与数据库事务；不是 cache key，因为 cache 可丢，result_ref 指向 durable 结果；也不是单纯 content hash，hash 只能校验或寻址一部分语义。多对象结果里，result_ref 还可能指向 manifest，再由 manifest 引出多个 part。LogServe 当前 string ref 是简化实现，生产版应结构化。
```

## Q029. result reference 在高并发场景下可能出现哪些隐藏问题？

**回答：**

高并发下，result reference 的问题通常不在“能不能生成一个字符串”，而在发布、覆盖、读取、缓存、GC、权限和成本被同时放大。单线程 demo 很容易通过；几十个 worker 同时跑时，边界会暴露出来。

第一个隐藏问题是重复上传。同一个 workflow step 可能因为 worker 超时、控制面重投递、网络抖动而被执行多次。如果每次执行都生成随机 key，就会留下多个等价对象。metadata 只引用其中一个，其他对象变成 orphan。更稳的做法是用内容 hash、attempt id 和 step id 生成幂等 key，并用条件写避免覆盖。AWS S3 支持 `If-None-Match: *` 这类条件写，可以防止同 key 被意外覆盖。

第二个问题是同 key 覆盖。两个 worker 同时写：

```text
workflows/wf-1/steps/a/result.json
```

最后谁赢取决于服务端接收顺序。S3 对单对象读写有强一致性，但这不等于帮你做业务层 compare-and-swap。正确做法是 immutable key：

```text
workflows/wf-1/steps/a/attempt-2/sha256-...
```

或者写同 key 时必须使用条件写和 checksum 校验。

第三个问题是成功事件竞争。多个 attempt 都可能拿着自己的 `result_ref` 去写 `StepSucceeded`。如果事件日志没有幂等键或 fencing，旧 attempt 可能覆盖新 attempt，或者两个成功事件都被接受。LogServe 当前 `StepSucceeded` 的 idempotency key 包含 `workflow_id + step_id + input_hash + succeeded`，这能限制同一输入的成功事件重复提交。但生产系统还要考虑 lease epoch、attempt number 和 worker fencing，避免旧 worker 在超时后继续发布结果。

第四个问题是读放大和热点。一个上游大结果被 1000 个下游任务同时 dereference，会形成 thundering herd。对象存储可能撑得住，但 worker 侧会看到连接池耗尽、KMS 请求变慢、解压 CPU 飙升、本地磁盘临时空间被打满。应该做：

```text
worker-local cache
singleflight 防止同一 ref 被并发重复下载
max bytes in flight
per-ref / per-tenant 限速
分块 manifest 和 range read
```

第五个问题是 cache stampede 和旧缓存。worker 用 result_ref 做 cache key 是合理的，但 ref 必须包含 version 或 checksum。否则同一个 `s3://bucket/key` 被覆盖后，本地 cache 还以为自己有正确结果。immutable key 可以缓解这个问题；version_id 和 checksum 能进一步确认。

第六个问题是 GC 竞态。对象先写、metadata 后写。高并发下，GC 如果按 prefix 扫到刚写完但还没发布的对象，可能误删；如果 metadata 刚删除但下游仍在读，也可能造成读失败。GC 要有 watermark、grace period、二次 mark 和 active upload session 排除。不能只靠“对象不在当前 metadata 表里”就删除。

第七个问题是 LIST 和 Inventory 被滥用。控制面不应该通过高频 LIST prefix 来判断 workflow 状态。S3 LIST 虽然强一致，但它不是低成本状态数据库。高并发下频繁 LIST 会增加延迟和请求成本。状态真相应该在 log/metadata，S3 只是 result body 存储。

第八个问题是权限和 KMS 被放大。很多并发 GET/PUT 如果都走 SSE-KMS，KMS 权限、配额和延迟会进入 p99。多租户下，如果 result_ref 没绑定 tenant，某个 worker 可能拿错 ref 或用错 KMS key。生产系统要把 tenant_id、workflow_id、KMS key、bucket/prefix policy 放在一起设计。

第九个问题是 multipart session 泄漏。大对象并发上传时，每个失败 attempt 可能留下 in-progress multipart upload。未完成 part 会计费。应用层要记录 upload_id，失败或取消时 abort；生命周期规则只能兜底。

第十个问题是 metadata 结构太弱。一个 string ref 在低并发下够用；高并发下排障需要知道：

```text
哪次 attempt 写的？
哪个 worker 写的？
对象 size 和 checksum 是什么？
是否有 version_id？
是否已被 GC 标记？
是否有 active reader？
是否正在 restore？
```

这些字段不在 ref 或 metadata 里，线上排障就只能猜。

面试里可以这样答：

```text
高并发下 result reference 的隐藏问题包括重复上传、同 key 覆盖、多个 attempt 竞争写成功事件、旧 worker 超时后继续发布、下游并发 dereference 造成热点、worker cache stampede、GC 误删刚上传未发布对象、multipart session 泄漏、KMS 和对象存储请求放大。解决思路是 immutable key、内容 hash、If-None-Match 条件写、attempt/epoch fencing、成功事件幂等键、singleflight 本地缓存、bytes-in-flight 限制、GC grace period 和结构化 result_ref。LogServe 当前 hash key 和 StepSucceeded 幂等键是好的起点，但还缺条件写、version_id、upload session 和读放大控制。
```

## Q030. result reference 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

result reference 的真正难点都在异常路径。正常路径很简单：写对象、写事件、下游读取。系统质量取决于崩溃、重启、超时、重试时有没有把半成功状态说清楚。

第一种边界：对象写成功，事件或 metadata 写失败。LogServe 当前就是先 `resultStore.Put`，再 append `StepSucceeded`。如果 `Put` 成功但 append log 失败，会留下 orphan object。这个状态可以接受，但要有清理策略。正确处理是先用同一个 `result_ref` 和同一个 idempotency key 重试事件提交；确认无法提交后，再由 orphan sweeper 根据日志和 metadata 可达性清理对象。不能马上删，因为 append log 可能已经成功，只是响应丢了。

第二种边界：事件写成功，对象写失败。这个状态更危险，会产生 broken ref。正常协议应该避免它：只有对象写成功并完成必要校验后，才能发布 `StepSucceeded(result_ref)`。如果业务为了异步上传必须先写 metadata，那 metadata 只能是 `ResultUploading` 或 `ResultPending`，不能是成功状态。

第三种边界：PUT 超时，客户端不知道对象是否写成功。网络 timeout 不等于服务端失败。处理方式是对同一个 key 做 `HEAD`，校验 size、checksum、version。对象存在且匹配，就把 PUT 视为成功；对象不存在，再重试同一个 key。不要换随机 key 再传一份。S3 条件写和 checksum 字段在这里很有用。

第四种边界：`CompleteMultipartUpload` 超时。multipart complete 可能已经成功，也可能失败。客户端不能简单重新 complete，更不能直接开始新 upload 并发布新 ref。要先 `HEAD` final key；如果对象存在且 checksum/size 匹配，视为成功；如果不存在，检查 upload session 是否还可继续，必要时 abort 后重传。

第五种边界：控制面重启后只恢复 metadata，但对象不可读。可能原因包括对象被 lifecycle 删除、KMS 权限变了、bucket policy 变了、对象归档未 restore、MinIO/AWS endpoint 切换、ref 的 bucket 与当前配置不匹配。重启流程不能假设所有 ref 都健康。最好有后台 verifier 或按需校验，把错误分成：

```text
RESULT_OBJECT_MISSING
RESULT_OBJECT_FORBIDDEN
RESULT_OBJECT_ARCHIVED
RESULT_CHECKSUM_MISMATCH
RESULT_STORE_UNAVAILABLE
```

第六种边界：worker 崩溃时本地临时结果丢失。如果对象还没上传，metadata 也没写成功，step 应该按 task retry 重新执行。如果对象已经上传但 worker 在通知控制面前崩溃，控制面可能不知道这个对象存在。没有 upload session 时，只能靠对象 GC 清理；有 upload session 时，控制面可以在重启后继续提交或清理。

第七种边界：下游读取超时。下游 `GetObject` 超时不一定说明上游结果坏了。它可能是对象存储抖动、KMS 慢、归档恢复中、网络问题。下游重试要受 step deadline 控制；超过预算后可以把 step 标为可重试失败，但不要把上游 result_ref 判为损坏。只有 404、checksum mismatch、version mismatch 这类才是 ref 健康问题。

第八种边界：重试导致重复发布。workflow retry 和 object retry 要分开。object retry 是同一份 bytes、同一个 key、同一个 checksum；workflow retry 是重新执行 step，可能生成新 bytes、新 attempt。两个层次混在一起，就会出现“对象层为了重试生成新结果”的错误。

第九种边界：GC 和 retry 竞态。一个慢 worker 上传对象后，事件提交一直失败；GC 扫描时看不到 metadata 引用，可能想删。解决办法是 upload session、created_at grace period、active attempt 标记和删除前二次检查。对象刚创建几分钟内，不应该被 orphan GC 删除。

第十种边界：ref schema 升级。旧事件里可能只有 string ref，新事件里有结构化 ref。重启 replay 时要兼容两种格式。否则系统升级后老 workflow 不能读历史结果。生产系统要把 ref version 写进 payload：

```text
ref_schema_version = 1: string uri
ref_schema_version = 2: uri + checksum + size + version_id
```

对 LogServe 当前项目，异常路径可以这样总结：

```text
已做:
  大结果 Put 成功后才写 StepSucceeded。
  StepSucceeded 有 idempotency key。
  S3/local key 使用 sha256(data)，重试同内容时 key 稳定。

还没做:
  Put timeout 后 HEAD 判断。
  If-None-Match 条件写。
  multipart upload session。
  structured result_ref。
  orphan sweeper。
  result verifier。
  archive/expired 状态。
```

这不是缺陷清单，而是机制验证和生产实现之间的边界。面试里这样讲会更稳：我知道当前实现验证的是核心路径，不把它包装成完整生产对象事务系统。

面试里可以这样答：

```text
result reference 的异常边界主要是半成功状态。对象写成功但事件失败会留下 orphan object，要用同一个 ref 和幂等键重试提交，最后由 GC 清理；事件成功但对象失败会形成 broken ref，正常协议必须避免，成功事件只能在对象写入并校验后发布。PUT 或 CompleteMultipartUpload 超时时，客户端不知道服务端是否成功，要先 HEAD 同 key 校验 size/checksum，再决定是否重试。重启后要处理对象缺失、权限变化、归档未恢复、checksum mismatch 和 store 不可用。LogServe 当前先 Put 再写 StepSucceeded，方向正确，但生产化还要补条件写、HEAD-after-timeout、upload session、orphan sweeper 和结构化 ref。
```

## Q031. result reference 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

不能只答“网络”。result reference 的性能瓶颈取决于它卡在写入、读取、replay、GC 还是下游 fan-out。大多数线上系统里，远端对象存储访问的网络和服务端 I/O 是主瓶颈；但一旦把压缩、加密、checksum、全量缓冲、锁内上传和并发读取放进来，CPU、内存和锁竞争也会进入 p99。面试里应该把路径拆开讲。

写入路径通常是这样：

```text
result bytes
  -> serialize
  -> compress
  -> encrypt / checksum
  -> upload object
  -> write metadata / event
  -> publish result_ref
```

如果结果很大，上传阶段通常受网络带宽、对象存储服务端吞吐、TLS、KMS、bucket 所在区域和客户端连接池影响。AWS S3 官方性能文档强调，应用要通过并发连接、水平扩展请求、合理重试和按前缀扩展吞吐来获得高性能；这说明对象存储不是本地函数调用，它的尾延迟会进入业务路径。对于 result reference，写成功事件之前必须先确认对象已经发布，所以 PUT 的 p99 会直接影响 step completion 的 p99。

CPU 瓶颈主要来自四块：序列化、压缩、加密、checksum。压缩如果使用高压缩等级，CPU 会明显上升；客户端加密会让上传前多一段加密计算；强校验需要计算 SHA-256、CRC32C、CRC64NVME 等校验和。对大对象来说，这些 CPU 时间可能比一次内存 copy 还贵。不要把 checksum 当免费字段。它换来的是可验证性，但要被计入延迟预算。

内存瓶颈经常被低估。很多简化实现会把 result 放成 `[]byte`，再计算 hash，再传给 HTTP body，再在读取时 `ReadAll`。这对 1 MB 结果没问题；对 500 MB 结果就会带来内存峰值、GC pause 和 OOM 风险。更稳的实现会使用流式 upload、multipart upload、临时文件或分块管道，避免“为了生成一个 ref，把大对象在内存里复制三四份”。

锁竞争是 result reference 在控制面里最隐蔽的瓶颈。理论上对象上传属于数据面，应该尽量放在控制面锁外；实际代码如果在 workflow 全局锁、数据库事务或 actor lock 里调用 `PutObject`，一个慢 PUT 就会堵住其他 step 的调度和完成。LogServe 当前 `completeWorkflowStep` 在 `workflowMu` 内调用 `materializeResult`，而 `materializeResult` 对大结果会调用 `resultStore.Put`。这对机制验证可以接受，但如果并发 step 很多，大对象上传会拉长锁持有时间。线上版本更合理的做法是缩小锁范围：先在锁内确认 attempt 仍然有效，锁外上传对象，再回到锁内用 attempt/epoch 校验并追加成功事件。

本地 I/O 也会出现。比如 local object store 写文件、临时文件落盘、压缩前后 spill、worker cache、读回校验、GC 扫目录。使用 MinIO 时，虽然 API 看起来像 S3，但底层可能是本机磁盘、局域网磁盘或分布式 erasure set，I/O 特征和 AWS S3 不一样。单机测试里看起来是磁盘瓶颈，上云后可能变成网络、KMS 或区域距离瓶颈。

读取路径的瓶颈还有 fan-out：

```text
一个上游 result_ref
  -> 1000 个下游任务同时读
  -> 对象存储 GET 放大
  -> KMS / 网络 / 解压 / worker memory 同时放大
```

这时瓶颈不只是对象存储，而是系统里最先被打满的那一层：连接池、带宽、KMS、解压 CPU、本地缓存锁、磁盘临时空间都可能成为 p99 来源。工程上要加 `singleflight`、本地缓存、bytes-in-flight 限制、并发 GET 限速和按 ref 的热点指标。

replay 路径的性能目标和读取路径不同。replay 不应该为了恢复 workflow 状态而下载所有大结果。它应该只读事件日志和 metadata，最多保留 `result_ref`、size、checksum、状态。如果 replay 被迫 dereference 每个 result，说明 ref 边界设计错了。

GC 路径的瓶颈通常是 LIST、Inventory、metadata 扫描和删除请求。小规模可以 prefix scan；大规模要靠对象清单、批处理、mark-sweep 和生命周期规则。GC 不应抢占线上 PUT/GET 资源，也不能用高频 LIST 当状态查询。

LogServe 当前实现可以这样拆：

```text
CPU:
  sha256(data)、JSON 序列化、SigV4 签名。

内存:
  Put/Get 都以 []byte 为接口，S3 Get 使用 io.ReadAll。

网络:
  S3Store 通过 HTTP PUT/GET 访问 MinIO/S3，client timeout 为 30s。

锁:
  completeWorkflowStep 持有 workflowMu 时 materialize 大结果。

I/O:
  LocalStore 使用 os.WriteFile/os.ReadFile；S3/MinIO 背后还会有对象服务端磁盘 I/O。
```

所以如果让我优化，我不会先盲目换对象存储。第一步是打点：`object_put_ms`、`object_get_ms`、`bytes`、`checksum_ms`、`compress_ms`、`encrypt_ms`、`lock_wait_ms`、`lock_hold_ms`、`retry_count`、`alloc_bytes`、`gc_pause_ms`。没有这些指标，只看一个 workflow latency，很难知道 p99 被哪一层拖住。

面试里可以这样答：

```text
result reference 的瓶颈通常先来自对象存储 PUT/GET 的网络和服务端 I/O，但不能只看网络。大对象会放大序列化、压缩、加密、checksum 的 CPU；全量 []byte 缓冲会造成内存峰值和 GC；如果在 workflow lock 或数据库事务里上传对象，锁竞争会把对象存储抖动传给控制面 p99。LogServe 当前 S3 store 是单次 HTTP PUT/GET，Get 会 ReadAll，completeWorkflowStep 还在 workflowMu 内 materialize 大结果，所以机制验证足够，但生产优化要做流式/multipart、锁外上传、singleflight 读缓存、bytes-in-flight 限制和分层打点。
```

## Q032. result reference 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

这三类测试不要混在一起。correctness test 问的是“语义是否正确”；stress test 问的是“在并发、故障和抖动下是否还能保持不变量”；benchmark 问的是“吞吐、延迟、内存和成本曲线在哪里拐弯”。同一个 result reference 功能，如果只写 happy path 单元测试，很容易在上线后遇到 orphan、broken ref、重复上传和 p99 抖动。

correctness test 先测不变量。至少包括这些场景：

```text
1. 小结果低于 threshold 时 inline，不写对象存储。
2. 大结果超过 threshold 时写对象存储，并在事件里保存 result_ref。
3. 对象 Put 失败时，不允许写 StepSucceeded / WorkflowCompleted。
4. StepSucceeded 里有 result_ref 时，LoadResult 能读回原始 bytes。
5. 读回结果的 checksum、size、version 与 metadata 一致。
6. 同一 result bytes 重试 Put，得到稳定 key 或被条件写正确处理。
7. ref scheme 不支持、bucket 不匹配、key 非法时要拒绝。
8. local:// ref 不能逃逸对象目录，../ 和反斜杠要被拦住。
9. metadata 里不能同时出现冲突的 inline result 和 result_ref。
10. replay 时不需要下载大对象，也能重建 workflow 状态。
11. GC 不删除仍被 workflow、actor snapshot、manifest 引用的对象。
12. manifest result 能继续 mark 到所有 part。
```

LogServe 现有 `LocalStore.Get` 会检查路径是否逃逸 store 目录，`cleanNamespace` 会过滤空段、`.` 和 `..`，这类逻辑就应该有明确测试。S3Store 只接受配置 bucket 的 `s3://bucket/key`，也应该测“别的 bucket 的 ref 被拒绝”。这些不是边角料；它们防止的是路径穿越、跨租户读错对象和悬挂引用。

correctness test 还要测失败顺序：

```text
Put success + append log fail:
  不应该产生 broken ref，可以留下 orphan，后续由 sweeper 清。

Put fail + append log attempted:
  应该失败，不能发布成功事件。

Put timeout + HEAD shows object exists:
  应视为可恢复成功。

Put timeout + HEAD missing:
  可以用同 key 重试。

CompleteMultipartUpload timeout:
  先 HEAD final object，再决定 retry/abort。
```

如果实现还没有 HEAD-after-timeout 和 multipart upload，测试可以先写成 TODO 或 future test，但不变量要先写清楚。这样后面扩展不会把协议改歪。

stress test 测的是系统在压力下会不会破坏上述不变量。它应该制造并发、重试和故障，而不是只把 benchmark 跑久一点。典型压力场景包括：

```text
多 workflow 并发完成大结果。
同一个 step 被多个 worker 重复完成。
worker 上传对象后崩溃。
控制面在 Put 成功、append log 前崩溃。
对象存储随机 500、503、timeout、慢响应。
下游同时 dereference 同一个大 result_ref。
GC 与上传、读取、workflow retry 并发执行。
MinIO 重启或网络短暂断开。
对象被 lifecycle 转冷或删除。
KMS 或加密服务变慢。
```

stress test 的通过条件不是“没有 panic”这么低。它要检查最终状态：没有 broken ref；orphan object 数量在预期范围内；重复 attempt 不会发布多个冲突成功事件；GC 不会删 live object；重启后 workflow 能 replay；错误能被分类成 missing、forbidden、checksum mismatch、store unavailable，而不是全变成 `internal error`。

LogServe 已经有一些验证入口，比如 `go test -count=1 ./...`、`go test -race -count=1 ./internal/control ./internal/worker`、故障注入脚本和 benchmark 脚本。针对 result reference，我会补专门的 fault injection：让 `Store.Put` 在“写入后返回错误”“写入前返回错误”“写入后阻塞超时”三种模式下运行，再看事件日志和对象目录是否符合协议。

benchmark 测性能曲线。它不应该只报一个吞吐值。至少要按 payload size 分层：

```text
1 KB
64 KB
1 MB
16 MB
128 MB
1 GB
```

每个 size 都测 inline 和 ref 的分界点。指标包括：

```text
Put p50/p95/p99
Get p50/p95/p99
workflow step completion latency
replay latency
throughput: objects/s, MB/s, workflows/s
allocs/op, B/op, peak RSS
GC pause
lock wait / lock hold
retry count
S3 request count
egress bytes
cache hit ratio
```

如果是 Go benchmark，还要看 `B/op` 和 `allocs/op`。result reference 很容易在接口层引入全量复制，平均延迟看不出来，内存指标会先报警。若做对象存储 benchmark，要区分 local、MinIO、AWS S3；MinIO 单机结果不能直接外推到 AWS S3，AWS S3 的跨区、KMS、storage class 和 request pricing 都会改变曲线。

benchmark 还要测几个设计选择：

```text
inline threshold 从 32 KB 到 16 MB 时，控制面 replay 和对象请求成本如何变化？
单次 PUT 与 multipart upload 在大对象上差多少？
压缩等级改变后，CPU 与网络节省是否值得？
客户端加密打开后，p99 和内存峰值变化多少？
singleflight cache 能否压低并发 Get 的请求数？
锁外上传能否降低 workflowMu hold time？
```

面试里可以这样答：

```text
correctness test 测不变量：大结果必须先对象发布再写成功事件，ref 能读回同一份 bytes，checksum/size/version 匹配，非法 ref 被拒绝，GC 不删 live object，replay 不需要下载大结果。stress test 测并发和故障：重复 attempt、Put 超时、控制面崩溃、对象存储 5xx、下游 fan-out、GC 与上传并发，最后检查没有 broken ref 和错误状态可恢复。benchmark 测曲线：按对象大小、并发、inline threshold、压缩加密、local/MinIO/S3 分组，记录 p50/p95/p99、MB/s、objects/s、allocs/op、B/op、锁等待、重试次数和请求成本。LogServe 现有测试入口可以复用，但要补 objectstore mock fault 和 result_ref 专项 benchmark。
```

## Q033. 如果要求从零实现一个简化版 result reference，你会先定义哪些不变量？

**回答：**

我会先定义不变量，而不是先写 `Put` 和 `Get`。result reference 的代码看起来很小，真正决定质量的是协议。只要不变量清楚，后面换本地文件、MinIO、AWS S3，或者从单机扩到分布式，边界都不会乱。

第一个不变量：成功状态不能指向不存在的对象。也就是：

```text
如果 event.status = StepSucceeded 且 event.result_ref != "":
  dereference(event.result_ref) 必须能读到对象，
  或者返回一个明确的可恢复状态，例如 archived/restoring，
  不能是无解释的 404。
```

这要求发布顺序必须是“先写对象，后写成功事件”。如果对象写失败，不能把 step 标成成功。如果业务必须异步上传，那成功事件要拆成 `ResultPending` 和 `ResultAvailable`，不能把 pending 假装成 succeeded。

第二个不变量：已发布对象不可变。一个 result_ref 一旦出现在事件日志里，它指向的 bytes 就不能变化。最简单办法是用 content hash 或 attempt-scoped key：

```text
workflows/{workflow_id}/steps/{step_id}/attempt-{n}/{sha256}.json
```

不要用 `result.json` 这种可覆盖名字做长期 ref。对象存储支持覆盖，但 result reference 语义应该尽量是 append-only / immutable。这样 replay、cache、checksum、审计和 GC 都简单得多。

第三个不变量：同一个幂等操作不能发布两个不同结果。比如同一个 workflow、step、input_hash、attempt_epoch 对应一个 logical completion。重复请求可以返回同一个 ref，但不能第一次写 A，第二次写 B。可以用：

```text
idempotency_key = workflow_id + step_id + input_hash + attempt_epoch
fingerprint = sha256(result bytes + metadata)
```

如果幂等键相同但 fingerprint 不同，应该报冲突，而不是静默覆盖。

第四个不变量：ref 必须能被验证。最小结构不要只有 URI，至少要有：

```text
uri
size_bytes
sha256 或对象存储 checksum
content_type
created_at_ms
schema_version
```

如果存储支持 versioning，再加 `version_id`。有了这些字段，读回对象后可以校验“我读到的是不是当初发布的那份 bytes”。没有校验字段，错读、覆盖和静默损坏都很难发现。

第五个不变量：ref 不是权限。拿到 ref 不等于有权读对象。服务端 dereference 前必须做业务鉴权，检查 tenant、workflow、actor 或 artifact 权限。日志里不保存长期预签名 URL。下载时才生成短期授权，或者由服务端代理读取。

第六个不变量：对象生命周期不能早于引用生命周期。只要 workflow、actor snapshot、manifest 或审计记录还引用某个对象，它就不能被应用层 GC 删除，也不能被生命周期规则过早 expire。可以允许对象进入冷层或归档，但 metadata 要知道它处于 `archived/restoring/available/expired` 哪个状态。

第七个不变量：orphan 可以存在，broken ref 不应该进入稳定状态。对象写成功但 metadata 写失败，会产生 orphan；这是可清理的。metadata 成功但对象不存在，会产生 broken ref；这是协议错误。设计上宁可接受 orphan，也不要接受 broken ref。

第八个不变量：GC 必须保守。删除前必须经过 mark-sweep、watermark、grace period 和二次检查。对于 multipart upload、pending upload session、刚创建对象、正在 restore 的对象，要么跳过，要么单独处理。GC 的错误应该倾向于“多留一会儿”，不能为了省存储费破坏可恢复性。

第九个不变量：replay 不依赖对象存储可用。控制面 replay 应该能在对象存储短暂不可用时恢复 workflow 状态，只是把需要读取结果的操作标为 blocked 或 pending。否则对象存储一抖，整个控制面都起不来。

第十个不变量：ref schema 要可演进。第一版可能是：

```json
{
  "uri": "s3://bucket/key",
  "sha256": "...",
  "size_bytes": 123
}
```

第二版可能加入 `version_id`、`compression`、`encryption`、`storage_class`。事件日志是长期数据，不能要求旧 ref 一夜之间全部迁移。payload 里要有 `schema_version`，读取逻辑要兼容旧格式。

如果从零实现简化版，我会先写这几个接口和状态：

```go
type ResultRef struct {
    URI           string
    SizeBytes     int64
    SHA256        string
    ContentType   string
    SchemaVersion int
    CreatedAtMs   int64
}

type Store interface {
    Put(ctx context.Context, namespace string, r io.Reader, opts PutOptions) (ResultRef, error)
    Get(ctx context.Context, ref ResultRef) (io.ReadCloser, ObjectInfo, error)
    Head(ctx context.Context, ref ResultRef) (ObjectInfo, error)
}
```

最小状态机是：

```text
NoResult
  -> Uploading(upload_session)
  -> ObjectWritten(result_ref)
  -> Committed(result_ref in event log)
  -> Deleted / Expired
```

简化版可以不把 `Uploading` 写进持久 metadata，但脑子里必须有这个状态。否则一遇到 timeout、crash、retry，就不知道该补交事件还是清对象。

LogServe 当前实现的最小不变量已经有一部分：大结果 `Put` 成功才写 `StepSucceeded`，本地/S3 key 基于 SHA-256，`LoadResult` 只通过配置 store 读取，local ref 防路径逃逸。缺的主要是结构化 `ResultRef`、`Head`、checksum 字段、version_id、条件写和 GC 状态机。

面试里可以这样答：

```text
我会先定义这些不变量：成功事件里的 result_ref 必须可读；已发布 ref 指向的对象不可变；同一个幂等完成不能发布两个不同结果；ref 必须带 size、checksum、schema_version，最好带 version_id；ref 不是权限，读之前要做业务鉴权；对象生命周期不能早于引用生命周期；orphan 可以被 GC，broken ref 不应进入稳定状态；GC 必须保守；控制面 replay 不依赖对象存储可用；ref schema 必须可演进。有了这些不变量，再写 Put/Get/Head 和状态机，代码会自然很多。
```

## Q034. result reference 的常见误用是什么，误用后通常会产生什么线上症状？

**回答：**

result reference 的误用有个特点：上线初期不一定报错，数据量、并发、重试和生命周期规则上来以后才暴露。症状也不总是“对象读不到”，有时表现为账单上涨、p99 抖动、replay 变慢、权限串租户，或者偶发 checksum mismatch。

第一种误用：把预签名 URL 当 result_ref 写进日志。短期看下载方便，长期看会出两个问题：URL 过期后历史结果无法读取；URL 泄漏后，任何拿到它的人都能在有效期内访问对象。线上症状是旧 workflow 页面点击结果时报 403，或者安全审计发现日志里有可访问的签名参数。正确做法是日志保存内部 ref，访问时再按权限生成短期 URL。

第二种误用：把用户输入直接拼进 object key。比如：

```text
results/{tenant}/{filename}
```

如果 `filename` 来自用户，可能包含 `../`、控制字符、超长路径、URL 编码混淆、敏感信息或高基数前缀。线上症状包括对象分布混乱、清理脚本误删、日志泄漏 PII、某些客户端打不开 key、MinIO 和 AWS S3 表现不一致。正确做法是用户输入只做 metadata，不直接控制 key；key 使用系统生成的 ID、hash 和白名单化片段。

第三种误用：使用可变 key 保存不可变结果。比如所有 attempt 都写：

```text
workflows/wf-1/steps/a/result.json
```

线上症状是 replay 读到的结果和当初 step 成功时不一致，本地 cache 命中旧对象，下游偶尔看到前后矛盾的数据。正确做法是 immutable key，加 attempt、input_hash、content_hash 或 version_id。

第四种误用：先写 metadata，再异步上传对象，却把状态标成成功。线上症状是用户看到 workflow 成功，但点击结果 404；下游任务启动后读不到上游结果；重试又可能生成另一份对象。正确协议是对象写入并校验成功后才发布成功事件；如果必须异步，状态要叫 `uploading/pending`。

第五种误用：没有 checksum、size、version。线上症状是对象被覆盖、截断、读错 bucket、跨环境 ref 混用时，系统只能在业务层报奇怪错误，无法判断是对象损坏还是数据本来如此。正确做法是 ref 或 metadata 里记录校验字段，读回后校验。

第六种误用：把所有结果都写对象存储，不设 inline threshold。线上症状是小结果也要 PUT/GET，workflow completion p99 上升，请求费增加，调试变慢。几十字节的 JSON 不值得走对象存储。LogServe 使用 inline threshold 就是在避免这个误用。

第七种误用：把大结果全部 inline。线上症状是 metadata 表膨胀、事件日志变大、replay 很慢、备份和复制成本上升，控制面接口返回时间变长。严重时单条记录超过数据库、消息队列或 gRPC 限制。

第八种误用：全量缓冲大对象。接口都是 `[]byte`，读写都 `ReadAll`。线上症状是内存峰值高、Go GC pause 增加、容器 OOM、并发一上来吞吐下降。生产版应该支持 streaming、multipart、临时文件和 backpressure。

第九种误用：在全局锁或数据库事务里 PUT 对象。线上症状是对象存储一抖，workflow 调度、状态更新、其他 step 完成都变慢；p99 看起来像控制面锁竞争，根因却是对象 PUT。LogServe 当前 `workflowMu` 内 materialize 大结果就是一个需要在生产化时拆开的点。

第十种误用：重试时换随机 key。线上症状是对象数量持续增长，metadata 只引用最后一个，老对象无人引用；账单涨，GC 扫出大量 orphan。正确做法是同一 logical upload 使用稳定 key，timeout 后先 HEAD 校验，再决定是否重试。

第十一种误用：没有 orphan GC。线上症状非常直接：业务量没变，存储容量持续增长；对象数量远大于 workflow 成功结果数；按 prefix 查到大量没有 metadata 引用的对象。

第十二种误用：GC 太激进。线上症状更危险：历史 workflow 偶发 404；重跑或 replay 时读不到结果；某些慢下游任务刚开始读，对象已经被删。正确做法是 mark-sweep、grace period、watermark、二次检查和 restore/active reader 状态。

第十三种误用：把 cache path 当 result_ref。比如写入事件的是：

```text
/tmp/worker-3/result-abc.json
```

线上症状是 worker 重启、容器迁移或换机器后结果消失。cache 是优化，不是权威存储。事件日志里必须写 durable store 的 ref。

第十四种误用：把 MinIO 测试结果当成 AWS S3 语义全覆盖。MinIO 对 S3 API 兼容度很高，适合本地验证，但生产 AWS S3 还有 IAM、KMS、storage class、lifecycle、regional endpoint、request pricing、Object Lock、versioning、eventual operational limits 等细节。线上症状是本地测试全绿，上云后在权限、KMS、checksum、条件写或归档恢复上失败。

第十五种误用：把 ref 当成跨租户授权凭据。线上症状是 A 租户拿到 B 租户 ref 后能读结果，或者运维脚本跨 prefix 误扫。正确做法是 tenant scope 必须进入 metadata、bucket policy、KMS key 和 dereference 鉴权逻辑，不能只靠“key 很难猜”。

面试里可以这样答：

```text
常见误用包括：把预签名 URL 写进日志、把用户输入直接拼 key、用可覆盖 key 保存结果、先写成功 metadata 再异步上传、没有 checksum/version、所有小结果都写对象存储、大结果全部 inline、全量 []byte 缓冲、锁内上传、重试生成随机 key、没有 orphan GC、GC 太激进、把本地 cache path 当 durable ref、把 MinIO 当成 AWS S3 完全等价、把 ref 当权限。线上症状通常是 404 broken ref、历史结果过期、账单持续上涨、p99 抖动、replay 变慢、OOM、cache 读旧数据、跨租户泄漏和偶发 checksum mismatch。
```

## Q035. result reference 在单机和分布式环境中的语义有什么差异？

**回答：**

单机和分布式环境里，result reference 的外形可能一样，都是一个 `local://...` 或 `s3://...` 字符串；语义差很多。单机环境更像“把大文件放到旁边，再在日志里记路径”。分布式环境里，ref 是跨进程、跨机器、跨故障域的 durable contract。后者要处理未知提交结果、并发 attempt、权限、版本、GC 竞态和跨区域成本。

单机环境里，控制面、worker 和对象目录可能在同一台机器上。`local://workflows/wf-1/...` 只要路径不逃逸 store 目录，写文件成功后，当前进程就能读。延迟低，调试简单，失败模式也少一些：进程崩溃、磁盘满、文件权限错误、临时目录被清理。可以用 `os.WriteFile` 和 `os.ReadFile` 做机制验证。LogServe 当前 local store 就是这种模式。

但单机语义有明显边界。local path 对别的机器没有意义；容器重建后本地目录可能丢；磁盘没有对象存储那样的 bucket policy、versioning、lifecycle、跨 AZ durability；文件写入是否原子也要自己处理。单机可以验证 result reference 模式，却不能证明它已经适合多机生产。

分布式环境里，ref 必须让任何授权 worker 都能读取同一份结果。对象存储成为共享 durable store。这里第一个差异是“可见性”。AWS S3 对对象 PUT/DELETE、HEAD、metadata 等提供强 read-after-write 一致性，这降低了读后写复杂度；但这不等于对象和数据库之间有事务。`PutObject` 成功、`AppendLog` 失败，仍然会产生 orphan。`AppendLog` 成功、对象不存在，仍然是 broken ref。系统要靠协议避免后者、清理前者。

第二个差异是“未知结果”。单机写文件失败通常比较明确；分布式 PUT 超时时，客户端不知道服务端是否已经写入。正确做法是同 key `HEAD`，校验 size/checksum/version，而不是直接换 key 重传。multipart complete timeout 也是同理。

第三个差异是“并发发布”。单机 demo 可能只有一个 worker；分布式调度里，同一个 step 可能因为 lease 过期、worker 假死、网络分区而被多个 worker 同时执行。result_ref 必须和 attempt、epoch、input_hash、idempotency key 绑定。旧 worker 在超时后返回结果，不能覆盖新 attempt 的成功事件。

第四个差异是“权限模型”。单机文件权限通常比较粗；分布式对象存储要处理 IAM、bucket policy、KMS key、tenant prefix、VPC endpoint、临时凭证和审计。ref 里不能包含长期 secret。拿到 ref 的 worker 也不一定有权限读，必须由服务端或控制面决定谁能 dereference。

第五个差异是“生命周期和 GC”。单机可以直接扫目录；分布式对象存储里，LIST、Inventory、Batch Operations、lifecycle transition、restore、versioned delete marker 都会进入设计。GC 要考虑并发 upload、跨区域复制延迟、归档恢复、active readers 和审计保留。应用层 GC 和 S3 lifecycle 必须分工：应用层判断可达性，生命周期处理未完成 multipart、转冷、过期兜底。

第六个差异是“成本”。单机磁盘主要看容量；对象存储要同时看容量、PUT/GET/LIST 请求、跨区流量、KMS 请求、归档恢复、最低存储时长和早删费用。一个设计在单机上只是“多一次读文件”，在 S3 上可能是“每个小 result 都多一次 PUT 和一次 GET，并且下游 fan-out 放大请求费”。

第七个差异是“观测和排障”。单机上可以 SSH 看文件；分布式系统要靠 structured ref、request id、trace id、object version、checksum、worker id、attempt id、upload id。没有这些字段，线上只能看到“结果加载失败”，无法判断是权限、对象缺失、归档、损坏、网络还是配置错 bucket。

第八个差异是“兼容性”。MinIO 与 AWS S3 在常用 API 上兼容，但并不等于所有边界完全相同。签名、错误码、checksum、versioning、Object Lock、lifecycle、KMS、storage class、region endpoint、directory bucket 这些特性都可能有差异。分布式生产代码不要只以本地 MinIO 结果作为唯一依据。

对 LogServe 来说，准确表述应该是：当前 result reference 实现是单机/多进程机制验证，支持 local 和 MinIO/S3 风格 store，能验证“大结果外置、控制面写 ref、下游按 ref 加载”的核心路径。它不是完整分布式对象事务层。若要走分布式生产，需要补：

```text
结构化 ResultRef
Head/verify API
条件写
multipart upload
attempt epoch / fencing
锁外上传
orphan sweeper
应用层 mark-sweep GC
对象 lifecycle 策略
checksum/version 校验
跨租户鉴权
对象访问 trace
```

面试里可以这样答：

```text
单机里 result reference 更像“日志里记一个本地对象路径”，主要问题是路径安全、磁盘可靠性和进程重启。分布式里它变成跨 worker 的 durable contract：任何授权节点都要能读同一份不可变结果，还要处理 PUT 超时后的未知状态、多个 attempt 竞争发布、对象和 metadata 没有跨系统事务、IAM/KMS 权限、version/checksum 校验、GC 与 lifecycle 竞态、跨区延迟和请求成本。LogServe 当前实现适合说明机制：大结果外置、小结果 inline、StepSucceeded 写 ref；但生产分布式语义还要补条件写、HEAD-after-timeout、结构化 ref、fencing、multipart、GC 和鉴权。
```

## Q036. S3 object key 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

S3 object key 的核心目标是给 bucket 内的对象一个稳定、唯一、可寻址的名字。S3 的数据模型很简单：bucket 里放 object，object 由 key 标识。AWS 官方文档说 object key 是 bucket 内对象的唯一标识，key 是 UTF-8 字符序列，长度最多 1024 字节，并且大小写敏感。这个定义听起来像“文件名”，但它比文件名更接近一个对象存储命名协议。

如果只能选一个主目标，我会说 object key 首先解决正确性和可维护性，其次影响性能和安全性。

正确性来自“同一个逻辑结果到底对应哪个对象”。比如一个 workflow step 生成了大结果，控制面要把 `result_ref` 写进事件日志。如果 key 不稳定，重试、恢复和下游读取就会变得很难判断：

```text
第一次上传:
  workflows/wf-1/steps/a/random-001.json

PUT 超时后重试:
  workflows/wf-1/steps/a/random-002.json

metadata 里到底该写哪一个？
第一个对象是否已经成功写入？
第二个对象是不是同一份 bytes？
```

这就是正确性问题。一个好的 key 设计会让同一个 logical result 有明确的地址，最好还能通过 attempt、input_hash、content_hash 或 version_id 判断它是不是同一份结果。

可维护性来自 prefix 组织。S3 本身是平坦命名空间，没有真正的目录；`/` 只是 key 字符串的一部分。AWS 控制台和 ListObjects 可以用 prefix 和 delimiter 推导出类似目录的浏览方式，但那不是 POSIX 目录。工程上仍然会把 key 设计成有层次：

```text
v1/tenants/{tenant_id}/workflows/{workflow_id}/steps/{step_id}/attempt-{n}/{sha256}.json
```

这样做不是为了让 S3 变成文件系统，而是为了让人、脚本、生命周期策略、审计、Inventory、Batch Operations、prefix 级监控都能理解对象属于谁、什么时候产生、能不能删。

性能也和 key 有关系，但不是“key 越短越快”这么简单。S3 官方性能文档给出的粒度是 prefix：每个 partitioned prefix 至少可以达到 3500 次 PUT/COPY/POST/DELETE 或 5500 次 GET/HEAD 每秒，并且 bucket 中 prefix 数量没有固定上限。高并发写入如果全部挤在少数热 prefix 下，可能在扩展过程中看到 503 Slow Down；把负载合理分散到多个 prefix 可以提高吞吐。反过来，如果 key 设计只为了分散而完全不可读，后续 GC、排障和成本归因会很痛苦。

安全性也会被 key 影响，但 key 本身不是安全机制。不要把 key 当权限，也不要在 key 里放邮箱、手机号、原始文件名、提示词、用户可控路径或业务密文。key 会出现在日志、错误信息、S3 Inventory、访问审计、监控指标、预签名 URL 中。即使 bucket 是私有的，key 泄漏也会给攻击者提供对象结构和租户信息。安全边界应该靠 IAM、bucket policy、KMS、业务鉴权和短期授权，不是靠“key 很难猜”。

LogServe 当前实现就是一个简化但方向正确的例子。`S3Store.Put` 会把 namespace 清洗后和 `sha256(data)+".json"` 拼成 key：

```text
workflows/{workflow_id}/steps/{step_id}/{sha256}.json
```

这个 key 有几个好处：workflow/step prefix 方便定位，SHA-256 后缀让同内容有稳定对象名，重试同一份 bytes 时不会因为随机名产生一堆对象。它的边界也很清楚：目前 key 里没有 attempt、input_hash、schema_version、tenant_id，也没有 version_id 和条件写。作为单机/多进程机制验证足够；要做生产分布式，需要把 key schema 和 result_ref metadata 结构化。

面试里可以这样答：

```text
S3 object key 的核心目标是在 bucket 内稳定、唯一地定位对象。它首先是正确性问题：重试、恢复、下游读取、GC 和 replay 都依赖 key 指向同一份预期对象；也是可维护性问题：prefix 设计决定对象能不能被审计、分层、清理和排障。性能受 prefix 分布影响，AWS S3 会按 partitioned prefix 扩展请求吞吐；安全上 key 不能当权限，也不应该包含敏感用户输入。LogServe 现在用 workflow/step namespace 加 sha256.json 生成 key，验证了稳定命名和 result_ref 的核心机制，但生产版还要补 tenant、attempt、version、checksum 和条件写。
```

## Q037. S3 object key 的典型适用场景和不适用场景分别是什么？

**回答：**

S3 object key 适合给“已经决定放到对象存储里的对象”做稳定命名。它不适合承担数据库索引、权限系统、搜索系统、锁服务、消息队列或 POSIX 路径的职责。很多设计问题都来自把 key 用过头了。

典型适用场景有几类。

第一类是不可变业务结果。比如 workflow step 输出、LLM 结果、大 JSON、Parquet、图片、checkpoint、actor snapshot。key 里可以放业务上下文和不可变后缀：

```text
v1/workflows/wf-20260619/steps/extract/attempt-2/sha256-7f....json
```

这种 key 的优点是清楚：它属于哪个 workflow、哪个 step、哪个 attempt、对象内容 hash 是什么。事件日志只要保存 `s3://bucket/key` 或结构化 result_ref，就能在 replay 时恢复引用。

第二类是多对象结果的 manifest。一个结果可能由很多 part 组成，不适合把每个 part 都塞进 metadata。可以让主 key 指向 manifest：

```text
v1/results/wf-1/manifest.json
v1/results/wf-1/parts/part-00000.parquet
v1/results/wf-1/parts/part-00001.parquet
```

上层只引用 manifest key。GC、校验和下游读取再沿着 manifest 找到所有 part。

第三类是生命周期和成本分层。key prefix 可以表达保留策略：

```text
hot/workflows/...
cold/audit/...
snapshots/actors/...
tmp/uploads/...
```

应用层仍然要判断对象是否可达，但 prefix 能让 lifecycle rule、Inventory、Batch Operations 和人工排障更容易操作。不要把所有对象都扔到 bucket 根目录下。

第四类是多租户隔离的组织维度。常见做法是：

```text
tenants/{tenant_id}/workflows/{workflow_id}/...
```

这样 bucket policy、KMS key、审计日志和成本归因都能沿 tenant prefix 做限制或聚合。不过这不是完整鉴权。服务端 dereference 时仍然要检查调用者是否有权访问该 tenant 和 workflow。

第五类是高吞吐对象写入。key 可以把负载分散到多个 prefix，例如按 tenant、日期、workflow 分桶。现代 S3 不再要求像早年那样为了性能强行在 key 前面加随机 hash，但高并发场景仍然要避免所有 PUT/GET 压到一个热 prefix，尤其是同一个 key 或同一个小范围 prefix 被大量并发访问。

不适用场景也要明确。

第一，object key 不适合作为数据库查询条件的替代品。你可以按 prefix list，但 S3 List 不是低延迟二级索引。想查“某个用户最近 100 个成功 workflow”“所有状态为 failed 的 step”“所有大于 1GB 的结果”，应该在 metadata 表、日志索引或清单分析里做，不要靠扫描 key 字符串。

第二，object key 不适合作为权限凭据。`s3://bucket/key` 只是地址。谁能读，取决于 IAM、bucket policy、KMS、业务授权和临时凭证。把 key 设计得随机一些可以降低枚举风险，但不能替代鉴权。

第三，object key 不适合保存原始用户文件名。用户文件名可以放 metadata，展示时再使用；key 应该使用系统生成的安全片段。原始文件名里可能有空格、`..`、反斜杠、控制字符、Unicode 归一化差异、URL 特殊字符和敏感内容。AWS 文档也提醒，某些字符、相对路径片段、period-only segment 会让工具和 SDK 行为不一致。

第四，object key 不适合表达可变状态。比如 `actors/a/current.json` 每次都覆盖，短期写起来简单，长期会破坏 replay 和缓存。更稳的做法是不可变 snapshot key 加上 metadata 中的 current pointer，或者用日志系统表达状态演进。

第五，object key 不适合作为分布式锁。S3 条件写可以帮你做“如果 key 不存在才创建”的乐观并发控制，但这不是通用 lock service。锁有租约、续租、fencing token、超时恢复、持有者身份。用对象 key 硬做锁，边界会很快变复杂。

第六，object key 不适合承载过多业务语义。一个 key 里塞入 tenant、user、workflow、step、model、prompt hash、时间、状态、版本、压缩算法、加密策略、存储层级，最后会超过 1024 字节限制，也会让 schema 演进困难。关键索引字段放 key，其他放 metadata、tag 或控制面表。

第七，object key 不适合做临时 cache path。worker 本地缓存可以用 result_ref 的 hash 做本地路径，但事件日志不能写 `/tmp/...`。cache miss 后必须能从权威对象存储读取。

面试里可以这样答：

```text
S3 object key 适合给不可变对象命名，比如 workflow 大结果、actor snapshot、manifest、分片数据、审计归档和临时上传区。好的 key 会包含少量稳定上下文，如 tenant、workflow、step、attempt、content hash 或日期 prefix，便于读取、GC、生命周期、审计和成本归因。它不适合替代数据库索引、权限系统、搜索系统、分布式锁、消息队列，也不应该直接使用原始用户文件名或可变 current.json。LogServe 当前的 workflow/step/sha256.json 适合机制验证；生产版要补 tenant、attempt、schema version，并把复杂查询留给 metadata。
```

## Q038. S3 object key 和相近概念最容易混淆的边界在哪里？

**回答：**

S3 object key 最容易被误认为文件路径。这个误解会带出一串问题：把 prefix 当目录、把 key 当 URL、把 key 当权限、把 ETag 当 checksum、把版本化对象当单一对象。面试里把这些边界说清楚，基本就能判断候选人有没有真正用过对象存储。

第一，key 不是文件系统路径。S3 是平坦对象命名空间，`a/b/c.json` 只是一个 key 字符串，不表示真的有 `a` 目录和 `b` 子目录。AWS 控制台会用 `/` 展示“文件夹”，ListObjects 也可以用 prefix 和 delimiter 做分组，但这是浏览视图，不是 POSIX 目录语义。没有目录 inode、rename、fsync、文件锁、相对路径解析和原子 append。

第二，prefix 不是目录。prefix 是 key 的前缀字符串。`workflows/wf-1/` 可以帮助 List 和 lifecycle，但 S3 不会因为这个 prefix 存在就自动创建目录。AWS 文档还提到，控制台创建 folder 时，本质上会创建一个 key 以 `/` 结尾的 0 字节对象。通过 REST API、CLI 或 SDK 看，它仍然是普通对象。删除这个 0 字节对象，并不等于删除 prefix 下所有对象。

第三，key 不是完整对象地址。完整地址至少要有 bucket + key。启用 versioning 后，还要有 version_id 才能唯一指向某个对象版本。`result.json` 只是 key；`s3://bucket/result.json` 才是一个内部 URI；`https://bucket.s3.../result.json?...` 是 HTTP URL；预签名 URL 又额外带临时授权。不要把这些混成一个字段。

第四，key 不是 result_ref 的全部。result_ref 是上层语义，可能包含：

```text
bucket
key
version_id
size_bytes
checksum
content_type
compression
encryption
expires_at
```

key 只解决“在哪里”。它不解决“这是不是我当初写的那份 bytes”“能不能读”“是否过期”“是否需要解压”“是否需要 KMS 解密”。

第五，key 不是 ETag，也不是 checksum。ETag 可以用于条件写，也能在某些情况下反映对象内容变化，但它不等于通用 MD5，尤其是 multipart upload、加密或不同服务端实现下。真正的数据完整性应该用 S3 checksum 字段、Content-MD5、应用层 SHA-256 或对象 metadata 记录。LogServe 当前把 SHA-256 放在 key 后缀里，这有利于内容寻址，但还没有在 `ResultRef` 里结构化记录 checksum。

第六，key 不是 metadata 或 tag。key 适合放少量稳定、高频使用的组织维度。对象大小、业务状态、模型版本、过期时间、压缩算法、trace id、用户展示名，很多时候更适合放 metadata、tag、manifest 或控制面数据库。把所有字段塞进 key，会导致 key 很长、schema 难升级、隐私暴露。

第七，key 不是用户文件名。用户看到的文件名可以是 `report final (2).xlsx`，对象 key 可以是：

```text
v1/tenants/t-1/uploads/2026/06/19/uuid.xlsx
```

用户文件名放 metadata：`original_filename=report final (2).xlsx`。这样既能展示原名，也不会让空格、括号、Unicode、`../`、反斜杠和控制字符污染 key。

第八，key 不是授权边界。`tenants/t1/...` 看起来像租户隔离，但真正能不能读取要靠 IAM、bucket policy、KMS 和业务鉴权。key prefix 可以作为 policy 条件的一部分，但不能单独承担安全语义。更不能因为 key 随机就允许未鉴权下载。

第九，key 不是分片路由的唯一工具。S3 性能按 partitioned prefix 扩展，但对象大小、请求并发、连接池、KMS、客户端重试、网络距离、下游 cache 都会影响性能。不要把“加 hash prefix”当万能优化。现代 S3 会自动扩展，官方也强调扩展到更高请求率是逐步发生的，过程中可能看到 503 Slow Down。

第十，key 不是生命周期真相。`tmp/` prefix 不代表对象一定可以删；`archive/` prefix 也不代表对象已经归档。应用层要有 metadata 判断可达性、pin、retention、restore 状态。生命周期规则只能按 prefix/tag/date 等条件执行，不知道业务引用图。

LogServe 当前代码里，`cleanNamespace` 会去掉空段、`.`、`..`，并把反斜杠换成 slash；`LocalStore.Get` 还会检查解析后的路径是否逃逸对象目录。这说明项目没有把 key 当普通本地路径直接信任。S3Store 里再通过 `escapePath` 对 URL path 片段做转义，避免 key 中特殊字符影响 HTTP 请求。这是对象 key 和文件路径边界的基本防线。

面试里可以这样答：

```text
S3 object key 最容易和文件路径、prefix/folder、URL、result_ref、ETag、metadata、用户文件名、权限边界混淆。key 是 bucket 内对象名，S3 没有真实目录；prefix 只是字符串前缀，控制台 folder 可能只是 0 字节对象；完整对象身份是 bucket + key，启用 versioning 后还要 version_id；key 不是 checksum，也不是授权凭据。LogServe 里 cleanNamespace 和 local path escape check 就是在防止把 key 当本地路径信任。
```

## Q039. S3 object key 在高并发场景下可能出现哪些隐藏问题？

**回答：**

高并发下，object key 的问题通常不是“能不能拼出字符串”，而是同名写入、热 prefix、LIST 放大、缓存一致性、生命周期误伤和排障信息不足。低并发 demo 用什么 key 都能跑；并发上来后，key schema 的缺陷会被放大。

第一个问题是同 key 写入竞争。两个 worker 同时写：

```text
workflows/wf-1/steps/extract/result.json
```

如果不用条件写，后完成的写入可能覆盖先完成的对象。S3 对单个 PUT/DELETE/HEAD 有强一致性，但它不会替你判断哪个业务 attempt 才合法。正确做法是用不可变 key，或者用 `If-None-Match: *` 防止覆盖。AWS 条件写文档说明，如果同 key 已存在，`If-None-Match` 会让写入失败；多个条件写同时发生时，先完成的成功，后续请求失败。

第二个问题是重复 attempt 产生重复对象。任务超时后重试，如果每次生成随机 key，就会产生多个内容相同或相近的对象。metadata 只引用一个，剩下的都变成 orphan。高并发时这个问题会直接变成存储账单和 GC 压力。内容 hash key、attempt-scoped key 和 upload session 能降低这个风险。

第三个问题是热 prefix。AWS 当前的 S3 性能模型允许每个 partitioned prefix 达到很高请求率，但扩展不是瞬间完成的。所有对象都写到 `results/`，并发突增时可能看到 503 Slow Down；所有下游都读同一个 `manifest.json`，也可能造成热点。key 设计应把天然分区维度放进 prefix，比如 tenant、workflow、日期、结果类型，不要把全部流量压在一个短 prefix 上。

第四个问题是 LIST 变成隐形瓶颈。S3 List 是分页 API，不是数据库索引。高并发系统如果每次调度都 `ListObjects(prefix=workflows/wf-1/)` 来判断状态，会引入请求费、分页延迟和控制面抖动。状态真相应该在 metadata 或事件日志里；List/Inventory 更适合审计、GC、批处理和离线校验。

第五个问题是 key schema 导致 GC 误判。比如 key 里只有日期：

```text
results/2026-06-19/{uuid}
```

GC 想按 workflow 清理时找不到 workflow 维度，只能扫很多对象；想按 tenant 清理时也没有 tenant prefix。反过来，如果 key 里只有 workflow，没有时间和 retention class，生命周期策略很难按保留期工作。高并发下，GC 扫描面一大，就会和线上 PUT/GET 抢资源。

第六个问题是本地缓存 key 失效。worker 可能用 `bucket/key` 做本地 cache key。如果同一个 key 被覆盖，本地 cache 会继续返回旧对象。不可变 key、version_id 和 checksum 可以解决这个问题。不要让可变 key 进入 worker-local cache。

第七个问题是编码和规范化差异。高并发系统通常会有多语言客户端。Go、Python、JavaScript 对 URL 编码、Unicode 归一化、反斜杠、空格、`+`、`%2F` 的处理可能不同。如果 key 里允许任意用户输入，线上会出现“Python 能写，Go 读不到”“控制台显示一个名字，SDK 看到另一个 key”的问题。

第八个问题是 key 生成器自身成为锁热点。很多团队会用全局递增序号生成 key：

```text
results/{global_counter}.json
```

这会把对象命名依赖集中到一个数据库行、Redis counter 或锁。高并发下，瓶颈不在 S3，而在 key 分配器。UUID、ULID、Snowflake、内容 hash、workflow-scoped attempt number 都可以避免全局锁，但要结合幂等语义设计。

第九个问题是租户隔离被 prefix 设计拖累。如果 key 没有 tenant 维度，后续想按租户限流、授权、审计、计费、删除都困难。高并发多租户系统里，一个大租户还可能把公共 prefix 打热，影响小租户。

第十个问题是错误排查缺字段。线上只看到：

```text
s3://bucket/7f9a....json
```

你很难判断它属于哪个 tenant、workflow、attempt、生成时间和结果类型。纯 hash key 很适合内容寻址，但可运维性差。常见折中是 prefix 可读、suffix 用 hash：

```text
v1/tenants/t1/workflows/wf1/steps/s1/attempt-2/sha256-7f9a.json
```

LogServe 当前的 `workflows/{workflow_id}/steps/{step_id}/{sha256}.json` 已经避免了纯 hash 根目录和完全随机 key，但还没有 tenant、attempt、schema version。高并发生产版要补这些字段，并配合 `If-None-Match` 或版本化对象处理同名写入。

面试里可以这样答：

```text
高并发下 object key 的隐藏问题包括同 key 覆盖、重复 attempt 留下 orphan、热 prefix 导致 503 Slow Down、把 LIST 当状态查询、GC 因 key 维度不足而扫全桶、可变 key 让本地 cache 读旧数据、多语言 URL 编码和 Unicode 处理不一致、全局 key 分配器成为锁热点、多租户 prefix 设计不清导致限流和授权困难。解决思路是不可变 key、attempt/content hash、条件写、合理 prefix 分布、metadata 作为状态真相、结构化 result_ref 和按租户/工作流的可观测性。
```

## Q040. S3 object key 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

异常路径里，object key 的核心问题是：重试时还能不能找到同一个逻辑对象，恢复时能不能判断哪个对象是有效结果。key 如果每次随机生成，崩溃后系统很难把半成功状态拼回去；key 如果可预测但不带 attempt 和 checksum，又容易覆盖或误认。

第一种边界：PUT 超时。客户端不知道对象是否已经写到 S3。正确做法是对同一个 key 做 HEAD，检查 size、checksum、ETag、version_id 或应用层 SHA-256。对象存在且匹配，就把这次 PUT 当成功；对象不存在，再用同一个 key 重试。不要直接换一个新 key 上传，否则原对象如果其实成功了，就变成 orphan。

第二种边界：随机 key 在崩溃后丢失。假设 worker 上传对象前生成随机 key，只保存在内存里：

```text
key = workflows/wf-1/steps/a/random-77.json
```

如果上传成功后 worker 崩溃，控制面不知道这个 key，重试会生成 `random-88.json`。旧对象只能靠 orphan GC 找到。稳定 key 或 upload session 能降低这个损失。内容 hash key 更适合“同一份 bytes 重试”的场景；attempt key 更适合“每次执行都要保留独立尝试”的场景。

第三种边界：条件写失败。使用 `If-None-Match: *` 时，412 不一定是坏事，它可能说明对象已经由前一次成功写入。此时要 HEAD 已有 key，校验它是不是同一份内容。如果相同，可以复用；如果不同，说明 key schema 冲突或并发 attempt 错误，不能静默覆盖。

第四种边界：409 或 503 这类并发/扩展错误。对象存储可能要求客户端重试。重试必须保持同一个 logical operation 的 key、checksum 和 metadata 不变。换 key 会破坏幂等；换 metadata 会让后续校验困难。

第五种边界：multipart upload。multipart upload 的 upload_id 绑定 bucket/key。进程崩溃后，如果没有持久化 upload_id 和 part ETag，系统通常只能 abort 旧 upload 或等生命周期清理，再重新上传同一个 final key。CompleteMultipartUpload 超时后，也要先 HEAD final key，不能直接再生成一个 key。

第六种边界：重启后本地 cache 与远端 key 不一致。worker 可能缓存了 `bucket/key -> local_path`。如果 key 可变或对象被覆盖，重启前后的 cache 可能读到旧内容。不可变 key 加 checksum 可以让 cache 安全；可变 key 必须带 version_id 或禁用缓存。

第七种边界：Delete + Put 交错。S3 强一致性能让 HEAD/GET 看到最新结果，但如果 bucket 开了 versioning，删除会产生 delete marker，后续同 key PUT 又产生新版本。恢复逻辑只拿 `bucket/key`，不拿 version_id，就可能读到当前版本而不是当时事件引用的版本。长期 result_ref 最好记录 version_id 或保证 key 不覆盖。

第八种边界：key schema 升级。旧对象可能是：

```text
workflows/{wf}/steps/{step}/{sha}.json
```

新对象可能是：

```text
v2/tenants/{tenant}/workflows/{wf}/steps/{step}/attempt-{n}/sha256-{sha}.json
```

重启 replay 时必须能读旧 ref。不要在 key 解析函数里只支持新格式。更稳的是把 schema version 放在 key prefix 或 result_ref metadata 里。

第九种边界：本地路径和 S3 key 的语义不一致。LogServe 同时支持 local 和 S3-compatible store。local store 要防路径穿越，S3 key 要防 URL 编码和特殊字符问题。一个 namespace 在 Windows 下有反斜杠，在 S3 里是普通字符；LogServe 用 `cleanNamespace` 把反斜杠转成 slash，并跳过 `.`、`..`，这就是跨后端一致性的处理。

第十种边界：重试时内容变了。对于 workflow step，重试可能重新执行，结果 bytes 可能不同。如果 key 只由 workflow/step 生成，第二次执行会覆盖第一次。应该把 attempt、input_hash、content_hash 或 version_id 放进 key 或 metadata，明确这到底是同一次对象重试，还是一次新的业务 attempt。

面试里可以这样答：

```text
异常路径下，object key 要支撑幂等恢复。PUT 超时后不能换 key，要 HEAD 同 key 校验 size/checksum/version；随机 key 如果只在内存里，上传成功后崩溃会留下 orphan；If-None-Match 返回 412 时要判断已有对象是否就是前一次成功写入；multipart 重启要处理 upload_id、part ETag 和 complete 超时；开启 versioning 后，bucket+key 不一定足以定位历史对象，还要 version_id。LogServe 当前用 workflow/step/sha256.json，重试同内容比较稳定，但还缺 HEAD-after-timeout、条件写、upload session 和 key schema version。
```

## Q041. S3 object key 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

object key 本身只是字符串，真正的性能瓶颈不在“拼接字符串要多久”，而在 key 设计如何影响请求分布、LIST 模式、幂等校验、URL 编码、签名、缓存和对象存储扩展。大多数场景里，网络和对象存储 I/O 仍然是主瓶颈；但 key schema 选得不好，会把 CPU、内存和锁竞争也拉进来。

网络和 I/O 是第一层。S3 请求是远程 HTTP 调用，key 决定访问哪个对象、哪个 prefix、是否命中热点。AWS 官方性能文档给出的经验值是：每个 partitioned prefix 至少 3500 次写请求或 5500 次 GET/HEAD 请求每秒，bucket 内 prefix 数量没有固定上限。高并发读写时，如果 key 都集中在一个热 prefix，S3 扩展过程中可能返回 503 Slow Down；如果 key 分散合理，可以通过并行请求提高吞吐。

LIST 是第二个常见瓶颈。key 设计决定 ListObjects 的分页范围。下面两个 key schema 的运维成本差很多：

```text
只有日期:
  results/2026-06-19/{uuid}.json

有租户和 workflow:
  tenants/t1/workflows/wf1/results/{uuid}.json
```

如果经常要按 workflow 清理或排障，第二种可以小范围 list；第一种可能扫当天所有对象。对象数量上来后，LIST 的网络往返、分页、XML/JSON 解析和内存占用都会变成瓶颈。

CPU 主要来自几块。第一是 key 里的 content hash。LogServe 当前用 SHA-256 作为对象名后缀，这有利于幂等和去重，但计算 hash 是 O(bytes)，大结果越大，CPU 越明显。第二是 URL 编码和 SigV4 签名。S3Store 会对 path 做转义，并为请求构造 canonical request；key 越复杂，越容易触发编码和签名边界。第三是应用层解析 key。GC、审计或导出脚本如果大量解析 key 字符串，也会吃 CPU。

内存瓶颈多出现在 listing 和索引构建。一次性把几百万个 key 拉到内存里做过滤，是很常见的事故。正确做法是分页流式处理，或者用 S3 Inventory、控制面 metadata、Batch Operations。key schema 决定你能不能缩小扫描范围。

锁竞争通常不在 S3，而在应用的 key 生成器和 metadata 更新路径。比如使用一个全局自增计数器生成 key，所有上传都要抢同一把锁；或者在 workflow 全局锁里计算 hash、上传对象、写 metadata，就会把对象存储抖动变成控制面锁等待。LogServe 当前 `completeWorkflowStep` 在 `workflowMu` 内 materialize 大结果，这个问题在 Q031 已经讲过；对 key 来说，相关点是 SHA-256 和 PUT 都发生在这个路径里。

key 长度和字符集通常不是主要性能瓶颈，但会增加错误率。AWS object key 最多 1024 字节，特殊字符可能需要 URL 编码或 XML entity；period-only path segment 会让工具发生路径归一化差异。复杂 key 不一定慢，但更容易让 SDK、控制台、CLI、日志系统、代理和签名代码出错。出错后的重试和排障成本会变成实际性能问题。

KMS 和权限检查也会被 key schema 间接影响。多租户系统常用 prefix 绑定 policy 或 KMS key。如果所有租户混在一个 prefix 下，限流、审计和密钥隔离会很粗；如果 prefix 过细，policy 数量和管理复杂度会上升。性能设计不能只看 S3 request rate，也要看 IAM/KMS/网络路径。

对 LogServe 来说，当前 key 性能的真实来源是：

```text
CPU:
  对 result bytes 计算 SHA-256；SigV4 请求签名。

内存:
  Put/Get 接口都是 []byte；key 生成前结果已经在内存里。

网络/I/O:
  MinIO/S3 HTTP PUT/GET；local store 写文件。

锁:
  大结果 materialize 发生在 workflowMu 路径内。

可运维性:
  key 有 workflow/step prefix，便于定位；缺 tenant/attempt/schema version，生产排障维度还不够。
```

面试里可以这样答：

```text
S3 object key 的性能瓶颈通常不是字符串拼接，而是 key 影响了请求分布、LIST 范围和幂等恢复。网络和对象存储 I/O 仍然是主瓶颈；热 prefix 会带来 503 Slow Down；LIST 大范围扫描会吃网络、内存和解析 CPU；content-hash key 会增加 SHA-256 CPU；全局 key 分配器会形成锁热点；特殊字符会放大编码、签名和工具兼容问题。LogServe 当前 key 使用 workflow/step/sha256，幂等性较好，但 SHA-256、[]byte 缓冲和锁内 materialize 是后续优化点。
```

## Q042. S3 object key 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

object key 的测试要覆盖三层：key 生成是否满足协议，极端并发和异常下是否仍然可恢复，性能曲线是否符合预期。只测“生成的字符串长得像路径”不够。

correctness test 先测 key 不变量：

```text
1. 同一个 logical result 在同一输入下生成稳定 key。
2. 不同 workflow、step、attempt 或 content hash 不会意外碰撞。
3. key 长度不超过 1024 字节。
4. key 大小写敏感，测试不能把 A 和 a 当同一个对象。
5. raw user input 不直接进入 key；如果必须进入，先白名单化。
6. key 中没有空段、`.`、`..`、反斜杠、控制字符和不可接受字符。
7. local ref 不能路径逃逸；S3 key 不能被 URL 编码绕过。
8. `s3://bucket/key` 解析时 bucket 必须匹配配置。
9. 启用 versioning 时，result_ref 能记录并读取 version_id。
10. key schema version 升级后，旧 key 仍能读取。
```

LogServe 现在可以直接测 `cleanNamespace`：输入 `a/../b`、`a\\.\\b`、空字符串、只有 `..`、前后空格，输出应该稳定且不逃逸；`LocalStore.Get` 应该拒绝 `local://../../x` 这类 ref；`S3Store.Get` 应该拒绝非配置 bucket。S3Store 当前 key 后缀由 SHA-256 生成，也应该测相同 bytes 得到相同 key，不同 bytes 得到不同 key。

correctness test 还要测 key 和写入协议配合：

```text
Put 成功后，result_ref 中的 key 能 Get。
Put 失败时，不产生成功 metadata。
If-None-Match 失败时，不覆盖旧对象。
HEAD 已有对象时，size/checksum 匹配才允许复用。
Delete marker 或 version mismatch 时，读取失败要分类清楚。
```

如果当前实现还没有条件写和 HEAD，可以先用 mock store 写 future test，把期望协议固定下来。

stress test 要制造并发。典型场景有：

```text
1000 个 workflow 同时生成不同 key。
同一个 workflow step 被多个 worker 重复完成。
所有请求集中到一个热 prefix。
请求分散到多个 prefix。
并发 Put、Get、List、Delete 同一 prefix。
Put 超时后重试同 key。
进程在 key 生成后、Put 前、Put 后、metadata 前分别崩溃。
大量包含特殊字符的用户文件名进入 metadata，但不能污染 key。
GC 同时扫描 prefix 并删除不可达对象。
```

stress test 的结果不能只看吞吐。还要检查：没有同 key 不同内容覆盖；orphan 数量在预期范围内；没有 broken ref；GC 不删 live key；所有错误能按 conflict、missing、forbidden、checksum mismatch、timeout 分类；高并发下 key 生成器没有全局锁瓶颈。

benchmark 要回答 key schema 的代价。至少比较这些维度：

```text
key 生成:
  UUID vs ULID vs Snowflake vs SHA-256(content)

prefix 分布:
  单 prefix vs tenant prefix vs workflow prefix vs hash shard prefix

操作类型:
  PUT p50/p95/p99
  GET/HEAD p50/p95/p99
  LIST 单页和全量扫描耗时
  batch delete / GC 扫描耗时

资源:
  CPU time
  B/op
  allocs/op
  peak memory
  request count
  503 Slow Down 次数
  retry count
```

如果用 AWS S3 跑 benchmark，要把对象大小、并发数、prefix 数、KMS 开关、区域距离、客户端连接池和重试策略都固定住。用 MinIO 跑 benchmark 也有价值，但它测的是本地/私有对象存储路径，不能直接外推到 AWS S3。

对 LogServe 的最小测试矩阵可以这样设计：

```text
correctness:
  cleanNamespace、LocalStore path escape、S3 bucket mismatch、same bytes same key。

stress:
  mock Store 注入 timeout/after-write-error；重复 completeWorkflowStep；并发大结果。

benchmark:
  sha256 key generation cost；local Put/Get；MinIO Put/Get；不同 inline threshold；锁外上传前后 workflowMu hold time。
```

面试里可以这样答：

```text
correctness test 测 key 协议：稳定生成、无碰撞、长度限制、大小写敏感、用户输入不直入 key、路径穿越被拒绝、bucket 匹配、version_id/schema version 可读。stress test 测并发和故障：重复 worker 写同一逻辑结果、热 prefix、并发 Put/Get/List/Delete、Put 超时同 key 重试、GC 并发扫描、进程在不同阶段崩溃。benchmark 测 key schema 的成本：UUID/ULID/hash key、prefix 分布、PUT/GET/HEAD/LIST p99、CPU、B/op、allocs/op、内存、503 和重试次数。LogServe 当前至少应测 cleanNamespace、same bytes same key、S3 bucket mismatch 和并发 result materialize。
```

## Q043. 如果要求从零实现一个简化版 S3 object key，你会先定义哪些不变量？

**回答：**

我会先定义 key schema 和不变量，再写拼接函数。object key 一旦写进事件日志或外部系统，就会变成长生命周期数据。今天随手拼出来的字符串，明天可能挡住 GC、迁移、审计和恢复。

第一个不变量：key 是 opaque name，不是文件路径。系统内部可以用 `/` 做 delimiter，但不能允许调用方传入任意路径片段。所有 segment 都要经过白名单校验：

```text
允许:
  a-z A-Z 0-9 - _ .

拒绝:
  空段
  .
  ..
  反斜杠
  控制字符
  未转义空格
  / 出现在 segment 内部
```

如果业务确实要保存原始文件名，把它放 metadata，不放 key。

第二个不变量：key 长度必须按 UTF-8 字节数检查，不能按字符数。S3 object key 最多 1024 字节。中文、emoji、某些组合字符会占多个字节。简化实现可以直接限制得更保守，比如 512 字节，并在生成时失败。

第三个不变量：key 必须有 schema version。比如：

```text
v1/tenants/{tenant}/workflows/{workflow}/steps/{step}/attempt-{attempt}/sha256-{hash}.json
```

以后从 `v1` 升到 `v2`，旧对象仍然能解析和读取。不要把 key schema 藏在代码注释里。

第四个不变量：key 中必须有足够的归属维度。多租户系统至少要有 tenant；workflow 系统要有 workflow_id；step 结果要有 step_id；重试系统要有 attempt 或 input_hash；内容寻址要有 digest。缺哪个维度，后面就会在哪个方向上排障困难。

第五个不变量：已发布 key 指向不可变对象。成功事件一旦保存 `key`，这个 key 的内容不能再变。要么通过 content hash 保证同 key 同内容，要么通过条件写和 version_id 保证不会静默覆盖。不要把 `current.json` 当长期 result key。

第六个不变量：同一 logical operation 的 key 生成要幂等。PUT 超时后，进程重试应生成同一个 key。生成函数不能依赖当前时间、随机数或内存状态，除非这些值已经持久化为 attempt_id 或 upload_id。

第七个不变量：key 不能承载权限。`tenants/t1/...` 可以帮助 policy 和审计，但 dereference 前仍然要检查业务权限。不要因为 key 里有 tenant_id 就跳过鉴权。

第八个不变量：key 不能泄漏敏感信息。不要把用户邮箱、手机号、原始 prompt、SQL、文件原名、访问 token 放进 key。key 会出现在日志、错误、URL、监控、清单和审计里。

第九个不变量：key schema 要服务 GC 和 lifecycle。至少要能回答：

```text
这个对象属于哪个 tenant？
属于哪个 workflow / result type？
是否临时对象？
是否可归档？
是否可按 prefix 扫描？
```

如果 key 完全随机，GC 必须依赖外部索引；这不是不行，但要提前承认并建设索引。

第十个不变量：key parser 必须保守。只解析自己生成的 schema；未知版本、非法 segment、bucket 不匹配、超长 key、大小写不符合约定，都应该拒绝。不要把任意 `s3://...` 都当内部对象读取。

一个简化实现可以这样设计：

```go
type ObjectKeyInput struct {
    TenantID   string
    WorkflowID string
    StepID     string
    Attempt    int
    SHA256Hex  string
    Ext        string
}

func BuildResultKey(in ObjectKeyInput) (string, error) {
    // validate each segment with a strict allowlist
    // validate sha256 is 64 hex chars
    // validate ext is one of json, bin, parquet
    // check UTF-8 byte length <= 1024
    return fmt.Sprintf(
        "v1/tenants/%s/workflows/%s/steps/%s/attempt-%d/sha256-%s.%s",
        in.TenantID, in.WorkflowID, in.StepID, in.Attempt, in.SHA256Hex, in.Ext,
    ), nil
}
```

对应的 result_ref 不应该只存 key：

```text
bucket
key
version_id
size_bytes
sha256
content_type
schema_version
created_at_ms
```

LogServe 当前简化版已经有一部分：namespace 由 workflow/step 组成，文件名是 SHA-256，`cleanNamespace` 会跳过 `.` 和 `..`，local store 会检查路径逃逸。若从零实现生产版，我会把 tenant、attempt、schema version、extension 白名单、UTF-8 byte length、version_id 和 checksum 元数据补上。

面试里可以这样答：

```text
我会先定义这些不变量：key 是 opaque name 不是文件路径；每个 segment 必须白名单化；长度按 UTF-8 字节数检查并小于 1024；key 带 schema version；包含 tenant/workflow/step/attempt/content hash 等必要归属维度；发布后不可变；同一 logical operation 重试生成同一个 key；key 不承载权限，也不泄漏敏感信息；schema 要支持 GC 和 lifecycle；parser 只接受自己生成的格式。LogServe 当前 workflow/step/sha256.json 是简化实现，生产版要补 tenant、attempt、v1 prefix、version_id 和结构化 checksum。
```

## Q044. S3 object key 的常见误用是什么，误用后通常会产生什么线上症状？

**回答：**

S3 object key 的误用通常不是马上把系统打挂，而是慢慢把恢复、GC、审计、权限和成本搞乱。等对象数量上来以后，问题会表现成一堆看似无关的症状：偶发 404、账单上涨、LIST 很慢、缓存读旧数据、某些对象控制台能看到但 SDK 读不到。

第一种误用是把 S3 key 当文件系统路径。比如允许用户传入 `../../x`、`\tmp\a`、`./file`，然后直接拼到 key 里。S3 本身不按 POSIX 路径解析 key，但 SDK、控制台、代理、下载工具、本地缓存层可能会做路径归一化。线上症状是：同一个对象在不同工具里表现不一致，清理脚本误删，local backend 出现路径穿越风险。LogServe 的 `cleanNamespace` 会跳过 `.`、`..` 并把反斜杠换成 slash，`LocalStore.Get` 还检查最终路径不能逃逸对象目录，这就是防这个误用。

第二种误用是把原始用户输入放进 key。用户文件名、邮箱、手机号、prompt、SQL、业务标题都不适合直接进入 key。key 会出现在日志、监控、错误、S3 Inventory、CloudTrail、预签名 URL 和人工排障截图里。线上症状是 PII 泄漏、URL 编码问题、对象名不可读、跨平台下载异常。正确做法是系统生成 key，用户原始文件名放 metadata。

第三种误用是用可变 key 保存不可变业务结果：

```text
workflows/wf-1/steps/a/result.json
```

这个 key 每次重试都覆盖，短期省事，长期会破坏 replay。下游今天读到的是新结果，事件日志里记录的却是旧 step 的成功。线上症状是 workflow 回放不一致、worker cache 命中旧数据、审计无法解释“当时到底输出了什么”。正确做法是 key 里带 attempt、input hash、content hash 或 version_id。

第四种误用是所有对象都扔到一个大 prefix：

```text
results/{uuid}
```

早期没问题，后面 LIST、GC、成本归因和批量删除都会变慢。AWS S3 会按 partitioned prefix 扩展请求吞吐，但扩展需要时间；请求突增时可能出现 503 Slow Down。线上症状是某个 prefix 下 PUT/GET p99 抖动，GC 扫描范围太大，按 tenant 或 workflow 查对象很费劲。

第五种误用是纯 hash key：

```text
sha256/7f9a...json
```

内容寻址很干净，但可运维性差。看到一个 key，很难知道它属于哪个租户、哪个 workflow、哪个 step、哪个保留策略。线上症状是排障全靠查数据库，Inventory 和对象列表对人没帮助。常见折中是可读 prefix 加 hash 后缀。

第六种误用是 key 里塞太多业务字段。tenant、user、workflow、step、model、日期、状态、压缩、加密、版本、原始文件名全塞进去，最后 key 变长，schema 难升级，还可能超过 1024 字节限制。线上症状是某些对象上传失败，旧工具解析不了新 key，改一个字段要迁移一批对象。key 应该只放稳定、必要、低敏的组织维度。

第七种误用是把 key 当权限。`tenants/t1/...` 有助于 IAM policy 和审计，但不是完整鉴权。服务端读取对象前仍然要检查调用者是否能访问这个 tenant、workflow 或 artifact。线上症状是 IDOR 类漏洞：用户拿到别人的 key 或 result_ref 后，可以绕过业务层读对象。

第八种误用是把预签名 URL 的 path 当内部 key 保存。预签名 URL 包含临时签名、过期时间和查询参数，不是稳定对象身份。线上症状是历史日志里的 URL 过期后无法访问，或者日志里泄漏短期访问凭据。内部存 `bucket/key/version_id`，对外下载时再生成短期 URL。

第九种误用是忽略 key 的大小写敏感。S3 key 是大小写敏感的，`Result.json` 和 `result.json` 是不同对象。线上症状是本地 Windows 测试通过，上 S3 后出现重复对象或读取不到。跨平台系统最好规定 key 全部小写或对 segment 做规范化。

第十种误用是用 key 承担查询索引。比如想查“最近一小时失败任务”，就按 prefix 扫对象。S3 List 不是业务索引。线上症状是控制面查询慢、请求费高、分页处理复杂，还会把对象存储抖动传给业务 API。状态查询应该走 metadata、日志索引或分析表。

第十一种误用是 key schema 没有版本。上线半年后想从 `workflows/{id}/...` 改成 `v2/tenants/{tenant}/workflows/{id}/...`，旧对象还在，读取代码却只认新格式。线上症状是升级后历史结果读不了，GC 把旧对象当非法对象。key 里加 `v1/` 或在 result_ref 里记录 schema_version 能减少这种风险。

第十二种误用是把 key 当本地 cache 路径。worker-local cache 可以用 key 的 hash 生成本地文件名，但不能把本地路径写回 metadata。线上症状是 worker 重启、容器迁移、磁盘清理后历史结果消失。

面试里可以这样答：

```text
S3 object key 的常见误用包括：把 key 当文件路径、直接拼用户输入、用可变 key 保存不可变结果、所有对象放在一个热 prefix、纯 hash key 导致排障困难、key 里塞太多业务字段、把 key 当权限、保存预签名 URL、忽略大小写敏感、用 LIST 替代数据库索引、没有 key schema version、把本地 cache path 写成 ref。线上症状通常是偶发 404、结果被覆盖、replay 不一致、缓存读旧数据、PII 泄漏、跨平台编码问题、GC 扫描慢、账单上涨、503 Slow Down 和历史对象升级后读不了。
```

## Q045. S3 object key 在单机和分布式环境中的语义有什么差异？

**回答：**

单机环境里，object key 经常被当成“文件相对路径”。分布式环境里，它是多个进程、多个 worker、多个区域、多个权限域共同依赖的对象身份。外形可能一样，语义负担差很多。

单机 local store 的 key 通常会映射到本地路径：

```text
local://workflows/wf-1/steps/a/sha256.json
-> /var/lib/logserve-objectstore/workflows/wf-1/steps/a/sha256.json
```

这时最重要的是路径安全、目录创建、文件写入原子性、磁盘空间、进程重启后文件还在不在。LogServe 的 LocalStore 做了路径逃逸检查，这说明它没有把 ref 当普通字符串直接信任。

分布式 S3 语义不同。S3 key 是 bucket 内对象名，不是本地路径。任何授权 worker 都可能在不同机器上通过 `bucket/key` 读取同一对象。这里 key 的稳定性、不可变性、版本、权限和生命周期会直接影响正确性。一个 key 如果被覆盖，所有 worker 都可能读到新内容；一个 key 如果进入 lifecycle 过期，所有历史 workflow 都可能断引用。

第二个差异是并发写。单机里你可能靠进程内锁保护一个文件路径；分布式里多个 worker 可以同时向同一个 key 发 PUT 或 multipart complete。S3 保证单对象读写的强一致性，但不理解你的业务 attempt。要靠不可变 key、条件写、versioning、attempt/epoch fencing 来保证“哪个结果可以发布”。

第三个差异是故障恢复。单机写文件失败通常比较直接；S3 PUT 或 CompleteMultipartUpload 超时时，客户端不知道服务端是否已经成功。恢复流程必须能根据同一个 key 做 HEAD/GET 校验。key 如果每次随机生成，超时恢复会变成 orphan 制造机。

第四个差异是权限。单机文件权限通常比较粗；分布式对象存储会有 IAM、bucket policy、KMS、VPC endpoint、Access Point、临时凭证。key prefix 可以参与 policy，但不能替代业务鉴权。多租户系统里，key schema 如果没有 tenant 维度，后面做权限隔离和审计会非常别扭。

第五个差异是可观测性。单机排障可以直接看目录；分布式排障依赖日志、trace、request id、bucket/key/version_id、checksum、worker id、attempt id。纯随机 key 在单机还凑合，在分布式里会让排障成本很高。

第六个差异是生命周期。单机删除文件通常就是删除文件；S3 里有 versioning、delete marker、noncurrent version、Object Lock、Glacier restore、lifecycle transition。`bucket/key` 可能有多个版本，当前版本和事件当时引用的版本不是一回事。长期 result_ref 最好保存 version_id 或保证 key 永不覆盖。

第七个差异是性能模型。单机主要看磁盘 I/O；S3 要看 prefix 请求率、网络 RTT、跨区流量、KMS、连接池、503 Slow Down、重试策略和请求成本。key schema 在分布式里会影响 prefix 热点、LIST 范围和多租户隔离。

第八个差异是兼容性。MinIO、本地文件和 AWS S3 都能用类似 key 字符串，但行为边界不同。MinIO 适合本地和私有化验证；AWS S3 还要考虑托管服务的 IAM/KMS/lifecycle/Inventory/Batch Operations 和区域语义。不能因为本地 key 能读，就断言生产分布式语义已经完整。

对 LogServe 的表述要收住：当前 key 设计验证了单机/多进程机制，支持 local 和 S3-compatible store。它不是完整生产分布式 key 管理层。生产版至少要补：

```text
v1 key schema
tenant prefix
attempt 或 input_hash
version_id
checksum metadata
条件写
HEAD-after-timeout
key 兼容性测试
按 prefix 的指标
GC 和 lifecycle 策略
```

面试里可以这样答：

```text
单机里 S3 object key 往往只是本地对象目录下的相对路径，主要问题是路径安全、磁盘和进程重启。分布式里 key 是跨 worker 的对象身份，必须稳定、不可变、可校验、可授权、可 GC。它要处理并发写同一 key、PUT 超时未知状态、versioning、delete marker、IAM/KMS、prefix 热点和生命周期。LogServe 当前 workflow/step/sha256 key 适合机制验证；如果扩到生产分布式，要补 tenant、attempt、schema version、version_id、条件写和按 prefix 的观测。
```

## Q046. multipart upload 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

multipart upload 的核心目标是让一个大对象可以分块上传。它把“上传一个巨大对象”拆成三个阶段：初始化 upload session、上传多个 part、最后 complete 成一个对象。AWS 官方文档写得很直接：每个 part 是对象数据的连续片段，可以独立上传，也可以乱序上传；所有 part 上传完成后，S3 按 part number 组装成最终对象。

如果只能选一个主目标，它首先解决性能和可恢复性问题；正确性是它必须保证的发布语义；安全性不是核心目标；可维护性来自 upload session、part 列表和清理协议。

性能收益很好理解。一个 100 GB 对象如果用单连接 PUT，吞吐受单连接、TCP 窗口、客户端磁盘读取、TLS 和网络波动限制。multipart upload 可以并行上传多个 part，让客户端更充分地使用带宽。AWS 文档也建议 100 MB 以上的对象考虑 multipart upload；稳定高带宽网络上可以用并行 part 提高吞吐。

可恢复性来自“失败只重传失败 part”。普通 PUT 上传到 99% 断网，通常要重新上传整个对象。multipart upload 中，如果第 713 个 part 失败，只需要重传这个 part。对于跨地域上传、移动网络、超大 checkpoint、离线导出、备份归档，这个差异很大。

正确性核心是 complete 边界。part 上传成功，不代表最终对象可见。只有 `CompleteMultipartUpload` 成功后，S3 才把 part 组装成普通对象。complete 时必须带 upload_id，以及 part number 和对应 ETag；S3 会按 part number 升序拼接。换句话说：

```text
UploadPart 成功:
  只说明某个 upload_id 下的 part 存在。

CompleteMultipartUpload 成功:
  才说明 bucket/key 下的最终对象发布。
```

这对 result reference 很关键。workflow 不能在 part 上传完一半时写 `StepSucceeded(result_ref)`。只有 complete 成功，并且 size/checksum/version 校验通过后，才能发布 ref。

安全性不是 multipart upload 自动解决的。它仍然要靠 IAM、bucket policy、KMS、SSE、客户端鉴权。甚至 multipart 还会增加权限面：Create/UploadPart/Complete 需要 `s3:PutObject`，Abort 需要 `s3:AbortMultipartUpload`，ListParts 需要 `s3:ListMultipartUploadParts`。如果只给上传权限不给 abort 权限，失败 upload 会留下 part 并继续计费。

可维护性体现在状态机。multipart upload 让大对象上传过程可观测：你可以记录 upload_id、part number、part size、ETag、checksum、created_at、attempt_id。进程崩溃后可以恢复；业务取消后可以 abort；后台 sweeper 可以清理超时 upload。没有这些状态，multipart 只是把一次 PUT 变成一堆更难清的 part。

LogServe 当前 S3Store 还没有 multipart upload。它使用单次 HTTP PUT，把完整 `[]byte` 传给 S3-compatible store。这对单机/MinIO 机制验证足够：验证的是大结果外置和 result_ref。生产化时，如果 result 可能达到几百 MB 或 GB，就应该把 `Store.Put` 扩展成流式或 multipart：超过阈值走 multipart，complete 成功后再写事件日志。

面试里可以这样答：

```text
multipart upload 的核心目标是把一个大对象拆成多个 part 独立上传，最后 complete 成一个对象。它主要解决性能和失败恢复：并行 part 提高吞吐，单个 part 失败只重传该 part；正确性边界在 CompleteMultipartUpload，complete 成功前最终对象不可见，不能发布 result_ref。它不是安全机制，权限、KMS 和鉴权仍然要单独做。LogServe 当前是单次 PUT，没有 multipart；如果生产化支持大 result，就要引入 upload session、part ETag、checksum、abort 和生命周期兜底。
```

## Q047. multipart upload 的典型适用场景和不适用场景分别是什么？

**回答：**

multipart upload 适合大对象、长时间上传、网络不稳定、需要并行吞吐或边生成边上传的场景。不适合小对象，也不适合拿来模拟 append、事务、队列或多 writer 协调。

典型适用场景先看对象大小。AWS 文档建议对象达到 100 MB 左右时考虑 multipart upload；S3 的 multipart 限制也给了明确边界：单对象最大 48.8 TiB，最多 10,000 个 part，part number 是 1 到 10,000，单个 part 5 MiB 到 5 GiB，最后一个 part 没有最小大小限制。对象越大，multipart 的收益越明显。

第一类场景是大 result。比如 workflow step 生成 5 GB 的压缩结果、LLM 批处理输出、Parquet 数据集、模型 checkpoint、日志归档包。这些对象用单次 PUT 风险高，失败重传成本大。multipart 可以把对象按 64 MB、128 MB 或 256 MB 分片上传。

第二类是网络不稳定或跨地域上传。网络抖动时，multipart 不需要重传整个对象，只重传失败 part。客户端还可以调节并发度和 part size，避免单连接拖垮总进度。

第三类是高带宽环境。比如 EC2 到 S3、机房到云、批处理 worker 到对象存储。并行上传多个 part 可以把带宽吃满。这里 multipart 不是为了“对象存储更快”，而是为了让客户端并发利用网络。

第四类是边生成边上传。导出任务、压缩任务、模型训练 checkpoint 打包时，最终大小可能一开始不知道。multipart 允许你先 initiate，然后随着数据生成不断上传 part，最后 complete。

第五类是大对象复制。S3 也有 UploadPartCopy，可以把已有大对象分段复制到新对象。这个适合跨 bucket、跨前缀、变更 metadata 或重写 storage class 的大对象操作，但仍然要处理 complete 和 abort。

不适用场景也很明确。

第一，小对象不要 multipart。一个 20 KB JSON 结果强行 multipart，会多出 CreateMultipartUpload、UploadPart、CompleteMultipartUpload、状态持久化、清理和权限面。请求次数和失败面比收益大。LogServe 的 inline threshold 和单次 PUT 对小/中等结果更合理。

第二，不要用 multipart 做 append。multipart 是为“创建一个最终对象”服务，complete 后对象成为普通对象。它不是 POSIX append，也不是多 writer WAL。你可以边生成边上传 part，但不能把已经 complete 的对象继续追加。

第三，不要用 multipart 做事务。Create、UploadPart、Complete、Abort 不是一个跨系统事务；metadata 写入、事件日志、对象存储之间仍然有半成功状态。complete 成功但 metadata 失败，会留下 orphan object；metadata 先成功但 complete 失败，会形成 broken ref。

第四，不要把 multipart 当分布式锁。多个客户端可以对同一个 key 发起多个 multipart upload。AWS 文档说明，versioning 开启时每个 complete 会产生新版本；未开启时，其他 PUT/DELETE/complete 可能影响最终可见结果。业务层仍然需要唯一 key、条件写、attempt fencing。

第五，短生命周期临时数据未必适合 multipart。比如几十 MB 的临时中间结果，生成后几秒就删。如果 upload session 清理没做好，未完成 part 的成本会超过收益。

第六，对严格低延迟的小请求，multipart 不合适。Create + N 个 UploadPart + Complete 的往返次数比单次 PUT 多。对象不够大时，延迟会更差。

第七，如果客户端无法持久化 upload_id、part ETag、checksum，最好不要自己手写 multipart。用 AWS SDK Transfer Manager 或成熟 S3 client 更稳。自己实现却不记录状态，崩溃恢复会很脆。

面试里可以这样答：

```text
multipart upload 适合大对象、长时间上传、跨地域或不稳定网络、需要并行吞吐、边生成边上传、以及大对象 copy。AWS 建议 100 MB 以上考虑 multipart，S3 限制是最多 10,000 个 part，part 通常 5 MiB 到 5 GiB，最后一个 part 可小于 5 MiB。不适合小 JSON、小图片、低延迟请求，也不能当 append、事务、分布式锁或队列。LogServe 当前单次 PUT 适合机制验证；大 result 生产化才需要 multipart。
```

## Q048. multipart upload 和相近概念最容易混淆的边界在哪里？

**回答：**

multipart upload 最容易和普通 PUT、append、range upload、分片文件、manifest、resumable upload、multi-part HTTP form 混在一起。名字都像“分块”，语义差别很大。

第一，它不是普通 PUT 的透明实现。SDK 可能在高层 API 里自动选择 multipart，但底层语义仍然是 CreateMultipartUpload、UploadPart、CompleteMultipartUpload。complete 前最终对象不可见；未完成 part 会计费；失败时要 abort。普通 PUT 没有 upload_id 和 part 状态。

第二，它不是 append。UploadPart 可以按 part number 上传，也可以乱序上传，但 complete 时 S3 按 part number 升序拼接，生成一个新对象。对象 complete 之后，不能用同一个 upload_id 继续追加。S3 Express directory bucket 有特定 append 能力，但这不能推广到 general purpose bucket 或所有 S3-compatible 存储。

第三，它不是 range write。HTTP Range 常见于读取对象的一部分；multipart upload 是写入新对象的多个 part。它不能对已有对象中间某个 byte range 原地修改。

第四，它不是 manifest。multipart complete 后，S3 只暴露一个普通对象；part 不再作为独立对象存在。manifest 则是应用层对象，里面列出多个普通对象或 part 文件：

```text
manifest.json
  part-00000.parquet
  part-00001.parquet
```

multipart 是传输机制，manifest 是数据组织机制。GC 逻辑完全不同。

第五，它不是把多个小对象合并成一个“目录”。UploadPart 的输入是同一个最终对象的连续数据片段。你可以用 UploadPartCopy 从多个源对象复制片段，但 complete 后还是一个对象，不保留子对象语义。

第六，它不是业务事务。CompleteMultipartUpload 成功只说明 S3 组装了对象，不说明数据库 metadata、事件日志、索引、权限表都提交了。result reference 仍然要处理对象成功但 metadata 失败的 orphan。

第七，它不是 checksum 的替代品。multipart ETag 很容易被误认为完整对象 MD5，但 AWS 文档明确说 complete 后的 ETag 不一定是对象数据的 MD5。正确做法是使用 S3 checksum、Content-MD5、应用层 SHA-256 或完整对象 checksum，并记录到 result_ref。

第八，它不是自动恢复协议。S3 提供 upload_id、ListParts、Abort、Complete；但“哪些 part 已经上传、哪些 ETag 应该用于 complete、崩溃后是否继续”要客户端自己管理。AWS 文档还提醒，ListParts 返回结果只适合校验，不应作为 complete 请求的唯一来源，客户端要维护自己上传 part 时拿到的 ETag 列表。

第九，它不是浏览器表单里的 `multipart/form-data`。两者名字相似，但完全不同。S3 multipart upload 是对象存储 API；HTTP multipart/form-data 是请求体编码格式。

第十，它不是自动清理机制。Create 后没有自动过期；必须 complete 或 abort。可以用 lifecycle 的 `AbortIncompleteMultipartUpload` 兜底，但它只清理 incomplete upload，不删除已经 complete 的对象。

面试里可以这样答：

```text
multipart upload 是大对象上传协议，不是 append、range write、manifest、事务、checksum、自动恢复机制，也不是 HTTP multipart/form-data。它和普通 PUT 的区别是有 upload_id、part number、part ETag、complete/abort 状态；complete 前最终对象不可见，未完成 part 会计费；complete 后 part 不再作为独立对象存在。对 result_ref 来说，multipart 只是把对象写入 S3 的方式，不能替代 metadata 幂等、checksum 校验和 GC。
```

## Q049. multipart upload 在高并发场景下可能出现哪些隐藏问题？

**回答：**

高并发下，multipart upload 的问题比普通 PUT 多，因为它把一次写入拆成了很多请求和一个 upload session。并发越高，状态越多，失败面也越多。

第一个问题是同 key 多 upload 并发。多个 worker 可以同时对同一个 key initiate multipart upload。AWS 文档说明，versioning 开启时，每个 complete 会创建新版本；多个 upload 用同一个 key 时，当前版本的判断和发起时间有关。未开启 versioning 时，其他 PUT、DELETE 或 complete 可能抢先影响最终可见结果。业务层不能让多个 attempt 写同一个 key，除非有明确条件写和 fencing。

第二个问题是 part number 覆盖。同一个 upload_id 下，如果用相同 part number 上传新 part，会覆盖旧 part。高并发上传器如果 part 分配器有 race，两个 goroutine 可能都写 part 17，最后 complete 得到的对象内容和本地预期不同。part number 分配必须确定、线程安全、可恢复。

第三个问题是 part ETag 列表错乱。CompleteMultipartUpload 需要 part number 和对应 ETag。如果并发上传时没有安全地记录 ETag，或者重试后没有更新 ETag，就会出现 `InvalidPart` 或生成错误对象。客户端不能只靠 ListParts 临时拼 complete 请求，应该维护自己的 part manifest。

第四个问题是过高并发把资源打满。multipart 并行度太高会耗尽客户端文件句柄、内存 buffer、HTTP 连接池、CPU checksum、TLS、磁盘读取、KMS 请求和对象存储请求配额。吞吐不一定继续上升，p99 反而变差。

第五个问题是大量 incomplete upload。高并发 worker 崩溃、取消或被 fencing 后，如果没有 abort，就会留下很多未完成 part。它们不会出现在普通对象列表里，但会计费。线上症状是对象数量看起来不多，S3 账单却上涨。必须记录 upload_id 并配置 `AbortIncompleteMultipartUpload` lifecycle。

第六个问题是 complete 阶段成为尾延迟。所有 part 上传完不等于对象发布。CompleteMultipartUpload 可能需要几秒甚至更久；AWS API 文档还说明 complete 处理时可能先返回 200 OK 头部，错误嵌在响应体里。自己手写 HTTP 客户端时，如果只看状态码，不解析 body，会把失败当成功。

第七个问题是条件写冲突。使用 `If-None-Match: *` 防覆盖时，高并发 complete 同 key 可能得到 412 或 409。AWS API 文档说 409 ConditionalRequestConflict 时要重新 initiate multipart upload 并重传 part。不能拿旧 upload_id 继续硬 complete。

第八个问题是 KMS 放大。SSE-KMS 下，Create、UploadPart、Complete、checksum 获取都可能涉及 KMS 权限和调用。并发 part 太多时，瓶颈可能不在 S3，而在 KMS 限流、权限或延迟。

第九个问题是 GC 和 upload 并发。GC 扫到某个 key 没有 metadata 引用，可能想删；但这个 key 可能有一个正在 complete 的 upload。应用层需要区分 active upload session、committed object 和 orphan object，不能只靠 ListObjects。

第十个问题是跨 worker 恢复互相踩踏。一个 worker 崩溃后，另一个 worker 接管 upload session。如果两个 worker 都继续上传或 abort，同一个 upload_id 会出现状态混乱。需要 attempt/epoch/fencing，确保只有当前 owner 可以 complete。

LogServe 目前没有 multipart，因此这些问题还没进代码路径。生产化引入时，不能只把单次 PUT 换成 SDK Transfer Manager，还要把 upload session 和 workflow attempt 绑定起来。

面试里可以这样答：

```text
高并发 multipart 的隐藏问题包括：多个 upload 同时写同一 key、part number 被并发覆盖、part ETag manifest 记录错、并发过高耗尽连接池/内存/KMS、失败 worker 留下大量 incomplete upload、CompleteMultipartUpload 成为 p99 来源、200 OK 里嵌错误被误判成功、条件写 412/409、GC 和 active upload 竞态、旧 worker 和新 worker 同时操作同一个 upload_id。解决思路是唯一 key、attempt fencing、线程安全 part 分配、持久化 part manifest、并发限流、abort/lifecycle 兜底、complete 后 HEAD/checksum 校验。
```

## Q050. multipart upload 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

multipart upload 的异常路径比普通 PUT 难，原因很简单：状态多。普通 PUT 只有“发请求”和“结果未知”；multipart 有 upload_id、part number、part ETag、checksum、complete、abort、lifecycle，每一步都可能半成功。

第一种边界：CreateMultipartUpload 成功，但客户端崩溃。upload_id 已经产生，但没有上传 part。这个 upload 仍然是 in-progress。恢复时如果本地没有记录 upload_id，就只能靠 ListMultipartUploads 或 lifecycle 清理。更稳的做法是先持久化 upload session，再开始上传 part。

第二种边界：某些 part 上传成功，客户端崩溃。S3 已经保存这些 part 并计费，但最终对象不可见。重启后要么继续同一个 upload_id，要么 abort 后重来。继续时必须知道已上传 part 的 part number、ETag、checksum 和 size。ListParts 可以校验，但不应替代自己的 part manifest。

第三种边界：UploadPart 超时。超时不代表 part 失败。客户端应该用同一个 part number 重试。即使前一次其实成功了，后一次同 part number 上传会覆盖前一次 part，只要 bytes 相同就是幂等的；如果 bytes 不同，说明 part 切分或输入流不稳定，应该失败而不是继续。

第四种边界：CompleteMultipartUpload 超时。complete 可能已经成功，也可能失败。恢复时先 HEAD 最终 key，校验 size、checksum、version_id。如果对象存在且匹配，就把 complete 视为成功；如果不存在或 checksum 不匹配，再判断是否能重试 complete 或必须 abort 重传。

第五种边界：Complete 返回 200 OK 但 body 里是错误。AWS API 文档明确提醒，complete 处理期间可能先发 200 OK 头部并发送空白字符保持连接，但最终错误嵌在响应体里。用官方 SDK 通常会处理这个情况；自己写 HTTP 客户端必须解析响应体。LogServe 当前 S3Store 是手写 HTTP client，如果以后手写 multipart，这个点不能漏。

第六种边界：AbortMultipartUpload 与 UploadPart 并发。AWS 文档提醒，abort 后正在进行的 part upload 仍可能成功或失败。为了释放所有 part storage，最好在所有 part 请求结束后 abort，或者 abort 后再次确认没有残留。应用层要把 upload session 标成 cancelling，不再启动新 part。

第七种边界：条件写失败。`If-None-Match` 返回 412 说明 key 已存在；要 HEAD 已有对象并判断是否是同一份 result。409 ConditionalRequestConflict 更麻烦，AWS API 文档建议重新 initiate multipart upload 并重新上传 part。旧 upload 要 abort 或等 lifecycle。

第八种边界：进程重启后 attempt 已失效。worker 重启时发现自己有一个 upload_id，但控制面已经调度了新 attempt。旧 attempt 不能继续 complete，否则可能发布过期结果。要用 attempt epoch 或 lease fencing 决定是否继续、abort 或交给 sweeper。

第九种边界：metadata 写失败。complete 成功后，最终对象已经可见；如果随后写事件日志失败，会留下 orphan object。这和普通 PUT 一样，但 multipart 的成本更高。恢复时应该用同一个 result_ref 和幂等键重试 metadata 写入；如果最终确定不可达，再由 GC 删除完整对象。

第十种边界：生命周期兜底不是实时清理。`AbortIncompleteMultipartUpload` 可以在指定天数后让 incomplete upload 进入可 abort 并删除 part，但它不会马上执行，也不会删除已经 complete 的对象。不能把它当请求路径补偿。

面试里可以这样答：

```text
multipart 的异常边界包括：Create 成功但 upload_id 没持久化、部分 part 成功后崩溃、UploadPart 超时、Complete 超时、Complete 返回 200 OK 但响应体里是错误、Abort 与正在进行的 UploadPart 并发、条件写 412/409、旧 attempt 重启后继续 complete、complete 成功但 metadata 写失败、lifecycle 清理不实时。恢复原则是：持久化 upload session 和 part manifest；part 超时用同 part number 重试；complete 超时先 HEAD final key；旧 attempt 要 fencing；失败或取消时 abort；lifecycle 只做兜底。
```

## Q051. multipart upload 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

multipart upload 的主瓶颈通常是网络和 I/O，但性能曲线由客户端并发、part size、checksum、加密、磁盘读取、连接池和 complete 阶段共同决定。它不是“part 越多越快”。

网络是最直观的瓶颈。multipart 的价值在于多个 part 并行上传，把带宽吃满。并发太低，带宽用不满；并发太高，连接池、TLS、S3 请求、KMS 和重试都会变成负担。AWS 文档建议稳定高带宽网络上用 multipart 提高吞吐，不稳定网络上用 multipart 降低失败重传成本。

I/O 包括客户端读源数据、临时文件、压缩输出、对象存储服务端写入。大对象上传时，客户端本地磁盘可能先成为瓶颈：你开 32 个 part 并发，磁盘顺序读变随机读，吞吐反而下降。对于边生成边上传的结果，生产者速度也可能限制上传速度。

CPU 主要来自 checksum、压缩、加密和 TLS。S3 支持多种 checksum，AWS SDK 和 S3 控制台会计算上传 checksum；multipart 下每个 part 有 checksum，完整对象也可以有 full object checksum。客户端如果还做压缩和客户端加密，CPU 会明显进入瓶颈。SSE-KMS 场景还要考虑 KMS 权限和调用延迟。

内存瓶颈来自 part buffer。假设 part size 是 128 MB，并发 16 个 part，理论上光 buffer 就可能占 2 GB；再加上压缩、加密、HTTP client buffer、重试缓存，很容易 OOM。生产实现要限制 `part_size * concurrency`，最好支持流式读取和 backpressure。

锁竞争通常发生在客户端 part 调度器、part manifest、进度回调、重试队列、workflow 控制面锁。上传器要记录每个 part 的 ETag 和 checksum，如果所有 goroutine 都抢同一把大锁，吞吐会下降。更隐蔽的是把 multipart upload 放在 workflow 全局锁里：对象存储一抖，控制面也抖。

CompleteMultipartUpload 是单独的尾延迟来源。所有 part 上传完后，还要发 complete。S3 要校验 part 列表并组装对象；如果提供完整对象 checksum，还要校验。complete 的 p99 会直接影响 result_ref 发布时延。不能只看 part 上传速度。

part size 是核心参数。part 太小，请求数多、ETag manifest 大、complete 列表长、请求费高，还可能碰到 10,000 part 上限。part 太大，失败重传成本高，并行度低，内存占用大。常见工程选择是 64 MB、128 MB、256 MB 起步，再按对象大小和网络调优。

LogServe 当前还没有 multipart；它的 S3Store 是单次 PUT，性能瓶颈是 `[]byte` 全量缓冲、SHA-256、HTTP PUT 和 30 秒 timeout。引入 multipart 后，要新增指标：

```text
part_size
part_concurrency
part_upload_ms
complete_ms
bytes_in_flight
retry_count_by_part
abort_count
incomplete_upload_count
checksum_ms
kms_ms
```

面试里可以这样答：

```text
multipart upload 的瓶颈通常先来自网络和 I/O：并行 part 用来吃满带宽，失败只重传 part。但 CPU 会被 checksum、压缩、加密、TLS 拉高；内存由 part_size * concurrency 决定；锁竞争会出现在 part manifest、调度器和控制面；CompleteMultipartUpload 还有单独尾延迟。part 太小请求数和成本高，太大重传和内存成本高。生产上要用指标调 part size、并发、bytes-in-flight、complete_ms、retry_count、abort_count，而不是盲目提高并发。
```

## Q052. multipart upload 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

multipart upload 的测试要分三类。correctness test 保证协议正确；stress test 验证并发和故障下不破坏状态；benchmark 找到 part size、并发、内存和 p99 的平衡点。

correctness test 先测基本协议：

```text
1. 小于阈值的对象走单次 PUT，不启用 multipart。
2. 大对象按固定 part size 切分，最后一个 part 可以小于 5 MiB。
3. 非最后 part 小于 5 MiB 时，complete 应失败或客户端提前拒绝。
4. part number 在 1 到 10,000 范围内。
5. 同一个 part number 重传时，manifest 更新到最新 ETag。
6. CompleteMultipartUpload 的 part 列表按 part number 升序。
7. complete 前最终对象不可见，不允许发布 result_ref。
8. complete 后 GET/HEAD 的 size/checksum 与原始 bytes 一致。
9. abort 后不能再用同 upload_id 上传 part。
10. 失败 upload 被 abort 或被 lifecycle 兜底清理。
```

还要测异常分类：

```text
UploadPart 失败:
  只重传该 part。

UploadPart 超时:
  同 part number 重试。

Complete 超时:
  HEAD final key 后决定成功/重试/abort。

Complete 返回 embedded error:
  客户端不能只看 200 OK。

条件写 412:
  校验已有对象是否可复用。

条件写 409:
  重新 initiate 并重传 part。
```

stress test 要制造并发和崩溃：

```text
同一对象多个 part 并行上传。
同一 part 重试与成功响应乱序到达。
多个 worker 对同一 key initiate multipart。
worker 在 Create 后、部分 part 后、complete 前、complete 后崩溃。
控制面在 complete 成功后、metadata 写入前崩溃。
GC 与 active multipart upload 并发。
Abort 与正在进行的 UploadPart 并发。
对象存储返回 500、503、SlowDown、timeout。
SSE-KMS 打开后 KMS 限流。
大量 incomplete upload 堆积。
```

stress test 的断言不能只看“最终有对象”。还要检查没有 broken ref、没有错误发布旧 attempt、incomplete upload 数量可控、orphan object 可被 GC、part manifest 没有丢 ETag、checksum mismatch 能被发现。

benchmark 要回答三个问题：part size 取多少，并发开多少，multipart 阈值设在哪。测试矩阵至少包括：

```text
对象大小:
  10 MB, 100 MB, 1 GB, 10 GB

part size:
  8 MB, 16 MB, 64 MB, 128 MB, 256 MB

并发:
  1, 2, 4, 8, 16, 32

后端:
  local/minio/aws-s3

校验/加密:
  no checksum, CRC64NVME, SHA256, SSE-S3, SSE-KMS
```

指标包括：

```text
total_upload_ms
part_upload_p50/p95/p99
complete_ms
throughput_MBps
CPU
B/op
peak RSS
allocs/op
retry_count
abort_count
503_count
KMS latency
request_count
cost estimate
```

对 LogServe 来说，当前还没有 multipart，测试可以先写 mock 版 `MultipartStore`。把协议测清楚，再接 MinIO 或 AWS。直接用真实 S3 做单元测试会慢、贵、不可重复；更适合放在集成测试或 nightly benchmark。

面试里可以这样答：

```text
correctness test 测 multipart 协议：part 切分、5 MiB 限制、part number、ETag manifest、complete 前不可发布 ref、complete 后 size/checksum 一致、abort 后不可继续、异常码分类。stress test 测并发和故障：多 part 并行、同 part 重试乱序、多个 worker 同 key、各阶段崩溃、GC 与 active upload 并发、abort 与 UploadPart 并发、S3 5xx/503/KMS 限流。benchmark 测 part size、并发、阈值、checksum、加密、后端差异，记录吞吐、p99、complete_ms、CPU、内存、B/op、重试、abort、请求数和成本。
```

## Q053. 如果要求从零实现一个简化版 multipart upload，你会先定义哪些不变量？

**回答：**

我会先写不变量，再写上传代码。multipart upload 最容易写成“能传文件”的 demo，但生产问题都在半成功状态里。没有不变量，崩溃恢复、重试、abort、GC 很快会乱。

第一个不变量：一个 upload session 绑定唯一的 bucket、key、upload_id、attempt_id。不能让两个业务 attempt 共享同一个 upload_id，也不能让旧 attempt 在 lease 过期后继续 complete。

第二个不变量：final object 在 complete 成功前不可发布。part 上传成功不等于 result 可读。metadata 里最多记录 `UPLOADING`，不能写 `SUCCEEDED(result_ref)`。

第三个不变量：part 切分确定。给定同一份输入、part size 和对象长度，part number 到 byte range 的映射必须稳定：

```text
part 1: [0, part_size)
part 2: [part_size, 2*part_size)
...
```

这样 UploadPart 超时后，同 part number 重试才是幂等的。

第四个不变量：非最后 part 必须满足最小大小。S3 要求 part size 5 MiB 到 5 GiB，最后一个 part 可以更小。客户端应该在 complete 前就检查，不要等 S3 返回 `EntityTooSmall`。

第五个不变量：part manifest 是权威状态。每个 part 上传成功后，必须记录：

```text
part_number
offset
size
etag
checksum
attempt_id
uploaded_at
```

CompleteMultipartUpload 只能使用这个 manifest。ListParts 可以校验，不能替代 manifest。

第六个不变量：同 part number 重传必须内容一致。如果同一个 part number 的 byte range 或 checksum 变了，说明输入流不稳定或状态损坏，应该失败。不要把不同 bytes 覆盖进同一个 part number 后继续 complete。

第七个不变量：complete 是发布点。Complete 成功并通过 HEAD/checksum 校验后，状态从 `UPLOADING` 变成 `OBJECT_WRITTEN`；再写事件日志或 metadata，变成 `COMMITTED`。如果 metadata 写失败，留下的是完整对象 orphan，不是 broken ref。

第八个不变量：失败、取消、过期必须 abort。只要 upload 不会再 complete，就要调用 AbortMultipartUpload。abort 失败要重试或交给 sweeper；bucket lifecycle 的 `AbortIncompleteMultipartUpload` 只是兜底。

第九个不变量：所有状态可恢复。服务重启后，能从持久化状态知道：

```text
哪些 upload 还在 UPLOADING？
哪些 part 已成功？
哪些 upload 已 complete 但 metadata 未提交？
哪些 upload 已 cancel，需要 abort？
```

如果 upload_id 只在内存里，系统重启后就只能靠扫 bucket 补救。

第十个不变量：错误要分类。`InvalidPart`、`InvalidPartOrder`、`EntityTooSmall`、`BadDigest`、`412 Precondition Failed`、`409 ConditionalRequestConflict`、`SlowDown`、timeout 不能全变成同一种错误。不同错误对应不同恢复动作。

第十一个不变量：并发受控。`part_size * concurrency` 必须有上限；每个 tenant、workflow、host 也要有限速。否则 multipart 很容易把内存、连接池和 KMS 打满。

第十二个不变量：checksum 是协议的一部分。至少记录完整对象 SHA-256 或 S3 checksum；最好记录每个 part checksum。complete 后 HEAD/GET 校验，读回时也能验证。

第十三个不变量：key 不可变。一个成功 complete 的 key 不应被后续 attempt 覆盖。使用 content hash key、attempt key、version_id 或条件写。不要所有 multipart 都写 `result.bin`。

一个简化状态机可以这样设计：

```text
NEW
  -> INITIATED(upload_id)
  -> UPLOADING(parts...)
  -> COMPLETING
  -> OBJECT_WRITTEN(bucket, key, version_id, checksum, size)
  -> COMMITTED(metadata/event written)

CANCEL_REQUESTED
  -> ABORTING
  -> ABORTED

FAILED_RETRYABLE
FAILED_FINAL
```

最小接口也要比 `Put([]byte)` 更明确：

```go
type MultipartSession struct {
    Bucket    string
    Key       string
    UploadID  string
    AttemptID string
    PartSize  int64
    State     string
    Parts     []PartRecord
}

type PartRecord struct {
    Number   int
    Offset   int64
    Size     int64
    ETag     string
    Checksum string
}
```

LogServe 当前没有这套状态；它的 `Store.Put(ctx, namespace, data)` 是单次写入接口。若要扩展，我会先加一个内部 multipart implementation，而不是把 multipart 细节泄漏给 workflow 状态机。workflow 仍然只看到“Put 成功返回 result_ref”；对象层内部负责 session、part、complete、abort 和清理。

面试里可以这样答：

```text
我会先定义这些不变量：upload session 绑定 bucket/key/upload_id/attempt；complete 前不能发布 result_ref；part 切分稳定；非最后 part 满足 5 MiB 限制；part manifest 是 complete 的权威来源；同 part number 重试内容必须一致；complete 后还要 HEAD/checksum 校验；失败或取消必须 abort；状态可持久化恢复；错误分类决定恢复动作；并发和 bytes-in-flight 有上限；checksum 是协议字段；成功 key 不可覆盖。LogServe 当前是单次 PUT，生产版应把 multipart 封装在 objectstore 层，保持 workflow 只依赖 Put/Get/result_ref。
```

## 参考资料

- [Amazon S3 data consistency model](https://docs.aws.amazon.com/AmazonS3/latest/userguide/Welcome.html#ConsistencyModel)
- [Amazon S3 objects, keys, and buckets overview](https://docs.aws.amazon.com/AmazonS3/latest/userguide/Welcome.html)
- [Naming Amazon S3 objects](https://docs.aws.amazon.com/AmazonS3/latest/userguide/object-keys.html)
- [Organizing objects using prefixes](https://docs.aws.amazon.com/AmazonS3/latest/userguide/using-prefixes.html)
- [Amazon S3 pricing](https://aws.amazon.com/s3/pricing/)
- [Using Amazon S3 storage classes](https://docs.aws.amazon.com/AmazonS3/latest/userguide/storage-class-intro.html)
- [Restoring an archived object](https://docs.aws.amazon.com/AmazonS3/latest/userguide/restoring-objects.html)
- [Restoring archived objects in Amazon S3 Intelligent-Tiering](https://docs.aws.amazon.com/AmazonS3/latest/userguide/intelligent-tiering-archive-access.html)
- [Best practices design patterns: optimizing Amazon S3 performance](https://docs.aws.amazon.com/AmazonS3/latest/userguide/optimizing-performance.html)
- [Performance design patterns for Amazon S3](https://docs.aws.amazon.com/AmazonS3/latest/userguide/optimizing-performance-design-patterns.html)
- [Uploading and copying objects using multipart upload in Amazon S3](https://docs.aws.amazon.com/AmazonS3/latest/userguide/mpuoverview.html)
- [Aborting a multipart upload](https://docs.aws.amazon.com/AmazonS3/latest/userguide/abort-mpu.html)
- [Configuring lifecycle to delete incomplete multipart uploads](https://docs.aws.amazon.com/AmazonS3/latest/userguide/mpu-abort-incomplete-mpu-lifecycle-config.html)
- [Amazon S3 multipart upload limits](https://docs.aws.amazon.com/AmazonS3/latest/userguide/qfacts.html)
- [CreateMultipartUpload API reference](https://docs.aws.amazon.com/AmazonS3/latest/API/API_CreateMultipartUpload.html)
- [UploadPart API reference](https://docs.aws.amazon.com/AmazonS3/latest/API/API_UploadPart.html)
- [CompleteMultipartUpload API reference](https://docs.aws.amazon.com/AmazonS3/latest/API/API_CompleteMultipartUpload.html)
- [AbortMultipartUpload API reference](https://docs.aws.amazon.com/AmazonS3/latest/API/API_AbortMultipartUpload.html)
- [ListParts API reference](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListParts.html)
- [PutObject API reference](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutObject.html)
- [Object API reference](https://docs.aws.amazon.com/AmazonS3/latest/API/API_Object.html)
- [Checking object integrity in Amazon S3](https://docs.aws.amazon.com/AmazonS3/latest/userguide/checking-object-integrity.html)
- [How to prevent object overwrites with conditional writes](https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-writes.html)
- [Download and upload objects with presigned URLs](https://docs.aws.amazon.com/AmazonS3/latest/userguide/using-presigned-url.html)
- [Protecting data with encryption](https://docs.aws.amazon.com/AmazonS3/latest/userguide/UsingEncryption.html)
- [Protecting data with server-side encryption](https://docs.aws.amazon.com/AmazonS3/latest/userguide/serv-side-encryption.html)
- [Using server-side encryption with AWS KMS keys (SSE-KMS)](https://docs.aws.amazon.com/AmazonS3/latest/userguide/UsingKMSEncryption.html)
- [Protecting data by using client-side encryption](https://docs.aws.amazon.com/AmazonS3/latest/userguide/UsingClientSideEncryption.html)
- [Concepts in the AWS Encryption SDK](https://docs.aws.amazon.com/encryption-sdk/latest/developer-guide/concepts.html)
- [Best practices for the AWS Encryption SDK](https://docs.aws.amazon.com/encryption-sdk/latest/developer-guide/best-practices.html)
- [Retry behavior - AWS SDKs and Tools](https://docs.aws.amazon.com/sdkref/latest/guide/feature-retry-behavior.html)
- [Managing the lifecycle of objects](https://docs.aws.amazon.com/AmazonS3/latest/userguide/object-lifecycle-mgmt.html)
- [Expiring objects](https://docs.aws.amazon.com/AmazonS3/latest/userguide/lifecycle-expire-general-considerations.html)
- [Retaining multiple versions of objects with S3 Versioning](https://docs.aws.amazon.com/AmazonS3/latest/userguide/Versioning.html)
- [Working with delete markers](https://docs.aws.amazon.com/AmazonS3/latest/userguide/DeleteMarker.html)
- [Locking objects with Object Lock](https://docs.aws.amazon.com/AmazonS3/latest/userguide/object-lock.html)
- [Cataloging and analyzing your data with S3 Inventory](https://docs.aws.amazon.com/AmazonS3/latest/userguide/storage-inventory.html)
- [Performing object operations in bulk with Batch Operations](https://docs.aws.amazon.com/AmazonS3/latest/userguide/batch-ops.html)
- [Mount an Amazon S3 bucket as a local file system](https://docs.aws.amazon.com/AmazonS3/latest/userguide/mountpoint.html)
- [MinIO S3 API compatibility](https://docs.min.io/aistor/developers/s3-api-compatibility/)
- [MinIO object tiering](https://docs.min.io/aistor/administration/object-lifecycle-management/object-tiering/)
