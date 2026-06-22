# 20. metadata store、materialized view、CQRS 与 event sourcing

这一章讨论控制面状态在 metadata store、materialized view、CQRS 和 event sourcing 之间如何分工。它们经常被放在一起讲，是因为真实系统里很少只有一个“数据库表”承担全部责任：写入路径需要保证状态转移可靠，读取路径需要低延迟、可查询、可分页，恢复路径又需要能从持久记录中重新构造当前状态。

下面的回答参考 Martin Fowler 对 CQRS 和 Event Sourcing 的原始说明、Microsoft Azure Architecture Center 对 CQRS、Event Sourcing、Materialized View 模式的定义，也结合 LogServe 的实现边界：LogServe 采用 log-first 的控制面路径，先写共享日志，再更新 metadata store 中的当前状态视图；共享日志是机制验证中的主要事实来源，metadata store 更接近可重建的 materialized view。需要注意，LogServe 当前定位是单节点/多进程的机制验证原型，不应被表述成完整生产级分布式 event store。

## Q001. metadata store 在系统中通常承担什么职责？

**回答：**

metadata store 通常承担的是“控制面当前状态”的保存和查询职责。它不一定是业务数据本体，也不一定是系统的最终事实来源；更常见的角色是让调度器、API、后台任务、恢复逻辑能够快速知道某个对象现在处于什么状态。

以任务系统、工作流系统、对象存储引用系统或者 AI runtime 为例，metadata store 往往会保存这些信息：

1. 对象的当前状态：任务是 queued、running、succeeded 还是 failed；工作流处在哪个 step；actor 是否还活着；模型是否已经注册。
2. 控制面索引：按状态、时间、租约、worker、workflow id 查询对象，避免每次都扫描完整日志或对象存储。
3. 调度所需字段：任务优先级、租约过期时间、worker 心跳、worker 当前负载、模型标签、并发限制。
4. 幂等性和去重信息：某个 request id、idempotency key 或 command id 是否已经处理过。
5. 恢复辅助状态：高水位、投影进度、最近一次 snapshot、待补偿对象、重试次数。
6. 查询 API 的读模型：给 UI、CLI、dashboard、status endpoint 返回当前可读状态。

所以 metadata store 的核心价值通常不是“保存全部历史”，而是让系统能高效回答“现在是什么”。这和 append-only log、event store、对象存储这类事实记录不同。event store 更关注“发生过什么”，metadata store 更关注“现在可以怎么决策”。

在 LogServe 里，`internal/metadata/store.go` 的接口就体现了这种职责。它包含 task、workflow、worker、actor、model 等控制面对象的创建、查询、租约、心跳、状态更新和列表操作。例如：

```text
CreateTask / GetTask / ListTasks / LeaseTask / CompleteTask
CreateWorkflow / GetWorkflow / UpdateWorkflow / ListWorkflows
UpsertWorkerHeartbeat / ListWorkerLoads
CreateActor / GetActor / UpdateActor / ListActors
RegisterModel / GetModel / ListModels
```

这些接口面向的是控制面决策，而不是完整历史审计。调度器需要快速找可运行任务，worker 需要领取任务，API 需要返回任务状态，恢复流程需要把日志里的事实重新投影到当前状态。metadata store 在这里更像一个“可查询的当前状态表”。

更重要的是，metadata store 是否是 source of truth，要看系统如何定义写入顺序。在 LogServe 的设计里，控制面采用 log-first 路径：例如提交任务时先追加 `TaskSubmitted` 日志，再写入 metadata；工作流 step 开始、成功、失败时也是先追加对应事件，再更新 metadata。这样设计的含义是：如果 metadata 更新失败，只要日志已经写入，系统仍然可以通过 replay 恢复 metadata。相反，如果先改 metadata 再写日志，一旦写日志失败，系统就会留下一个无法从事实来源重建的状态。

metadata store 常见职责可以分成四类：

**第一，服务在线路径。**

在线路径最需要低延迟、可索引、可分页。比如 `ListTasks(status=queued)`、`GetWorkflow(id)`、`LeaseTask(workerID)` 这类查询如果每次都扫描 event log，延迟会非常高，代码也会复杂。metadata store 可以使用 B-tree、LSM、内存 map、关系数据库索引或者 Redis-like 结构承接这些访问。

**第二，隔离读写复杂度。**

写入路径通常围绕状态转移和一致性设计，读取路径则围绕查询形状设计。metadata store 可以把复杂事件流投影成更容易查询的数据结构。比如 workflow 的事件流里可能有 `WorkflowSubmitted`、`StepScheduled`、`StepStarted`、`StepSucceeded`、`WorkflowCompleted`，但 API 需要的是一个 workflow 当前状态对象，以及每个 step 的状态摘要。

**第三，支持恢复和补偿。**

metadata store 可能损坏、落后或部分写入失败。只要系统还有更权威的日志或事件存储，就可以重放事件修复 metadata。LogServe 的 `BootstrapFromLog` 就是这种思路：启动时从 `task:`、`workflow:`、`actor:`、`llm:` 等 stream 读取事件，重新构造当前状态，再把 running 中间态转回 queued 这类可恢复状态。

**第四，承接控制面策略。**

很多策略不适合直接塞进 event store。例如 worker 当前负载、模型 EWMA 延迟、lease 过期时间、临时调度候选集合、后台 GC 标记状态，这些都更适合放在 metadata store 或派生视图里。它们可以影响调度，但不一定都是业务事实。

面试里要特别说清边界：metadata store 不是天然可靠，也不天然权威。它只有在系统明确把它定义为 source of truth，并围绕它设计事务、备份、恢复、并发控制时，才是最终事实来源。如果系统采用 event sourcing 或 log-first，则 metadata store 更像 materialized view。它可以很重要，甚至在线路径离不开它，但理论上应该能从更权威的事件记录中重建。

常见错误是把 metadata store 当成“顺手塞状态的地方”。这样做短期开发快，长期会出现三个问题：

1. 状态来源混乱：有的状态来自日志，有的状态只存在 metadata，恢复后不一致。
2. 并发控制混乱：多个 worker 或 control-plane 实例同时修改同一行，缺少版本号、租约或 fencing token。
3. GC 和生命周期混乱：对象存储中是否还有引用、任务是否完成、result reference 是否可删除，都没有统一判断依据。

面试里可以这样答：

```text
metadata store 通常保存控制面当前状态和查询索引，例如任务状态、工作流状态、租约、worker 心跳、模型注册信息和幂等性记录。它的目标是让调度、查询、恢复和运维接口高效工作。

但它不一定是 source of truth。在 log-first 或 event-sourcing 系统里，metadata store 更像从事件日志投影出来的 materialized view：在线路径依赖它，但它应该能从日志重建。设计时要明确写入顺序、幂等键、版本号、租约语义和恢复流程，否则 metadata 很容易变成不可解释的“第二套事实”。
```

## Q002. source of truth 和 materialized view 的区别是什么？

**回答：**

source of truth 是系统承认的权威事实来源；materialized view 是从权威事实来源派生出来、为了查询或决策而预先计算好的视图。二者最大的区别不是存储介质，而是冲突发生时相信谁。

如果 source of truth 和 materialized view 不一致，系统应该以 source of truth 为准，并修复或重建 materialized view。比如事件日志里显示任务已经 `TaskCompleted`，但 metadata 里仍然是 running，那么在 log-first 设计里应该相信事件日志，然后把 metadata 修正为 completed。反过来，如果 metadata 是系统唯一权威表，那日志只能作为审计或异步通知，不能反过来覆盖 metadata。

Microsoft 的 Materialized View 模式强调，materialized view 是为了让查询更高效而生成的预填充视图，适合源数据不直接满足查询需求的场景；视图通常不由应用直接更新，而是由源数据变化驱动更新，并且应当可以在必要时重建。这个定义里有两个关键信号：一是 view 面向查询形状，二是 view 来自其他数据。

source of truth 的典型特征是：

1. 用来裁决不一致。
2. 丢失后系统无法完整恢复，或者恢复成本最高。
3. 写入路径必须优先保护它的正确性。
4. 需要明确并发控制、持久化、备份、保留策略。
5. 其他派生数据可以从它重建。

materialized view 的典型特征是：

1. 面向读性能和查询便利性。
2. 可以延迟更新，因此可能短暂过期。
3. 可以删除后重建，至少设计上应该如此。
4. 结构通常和源数据不同，可能是反规范化、聚合、索引化后的形态。
5. 需要记录投影进度，例如 event offset、sequence、version 或 high watermark。

以 LogServe 为例，`TaskSubmitted`、`TaskStarted`、`TaskCompleted` 这些日志事件更接近 source of truth，因为启动恢复时可以读取 task stream，重放事件并重建 task metadata。`metadata.Task.Status` 则是当前状态视图，方便调度器和 API 查询。这个状态很重要，但它的正确性来自“能否被日志重放解释”，而不是来自它自己。

这一区别也会影响故障处理。

如果是 source of truth 写成功、materialized view 写失败，通常可以异步补投影、重放事件或者由后台 reconciler 修复。比如任务提交事件已经写入日志，但 metadata 创建失败，恢复流程可以从日志重新创建任务视图。

如果是 materialized view 写成功、source of truth 写失败，问题严重得多。因为系统已经暴露了一个没有事实依据的状态。比如 metadata 显示任务已提交，但日志里没有 `TaskSubmitted`，重启后这个任务可能消失；更糟的是 worker 已经基于这个状态执行了任务，后续结果没有对应的 command/event 可以解释。

所以写入顺序一般要围绕 source of truth 设计：

```text
command -> validate -> append authoritative event -> update projection/materialized view -> serve query
```

如果必须先更新某个视图，也要把它设计成 pending 状态，并通过 outbox、transactional log、补偿任务或者二阶段提交等机制保证最终能和 source of truth 对齐。

还要注意，source of truth 不一定永远是 event log。Martin Fowler 在 Event Sourcing 文章里也提到，事件日志可以是官方记录，也可以由应用状态作为官方记录并从中派生日志。真实系统要根据事务边界、团队能力、查询模型和恢复需求选择。关键是必须明确“冲突时谁说了算”，而不是同时让两个存储都自称权威。

面试里可以这样答：

```text
source of truth 是裁决事实的权威记录；materialized view 是为了查询效率从权威记录派生出来的预计算视图。

区别可以用一句话判断：如果二者不一致，应该相信谁？相信的那个是 source of truth，另一个要么重建，要么补偿。LogServe 这类 log-first 设计里，共享日志更接近 source of truth，metadata store 是当前状态的 materialized view；metadata 能加速调度和查询，但不能产生日志里无法解释的状态。
```

## Q003. CQRS 的基本思想是什么？

**回答：**

CQRS 的全称是 Command Query Responsibility Segregation，基本思想是把“修改状态的路径”和“读取状态的路径”分开建模。命令负责表达意图并改变系统状态，查询负责读取数据且不产生副作用。

Martin Fowler 对 CQRS 的经典定义是：使用不同的模型来更新信息和读取信息。Microsoft 的 CQRS 模式也强调，将读操作和写操作分离到不同的数据模型中；commands 更新数据，queries 返回数据，二者可以独立优化。这里的“模型”不一定是两个数据库，也可以是同一个数据库上的不同对象、不同接口、不同表结构，甚至只是同一服务里的不同代码路径。

最小版本的 CQRS 可以很简单：

```text
Command side:
  SubmitTask(command)
  CompleteTask(command)
  RegisterModel(command)
  CreateWorkflow(command)

Query side:
  GetTaskStatus(query)
  ListQueuedTasks(query)
  GetWorkflow(query)
  ListWorkerLoads(query)
```

command side 关注的是状态转移是否合法。例如任务是否已经完成，worker 是否持有有效 lease，workflow step 是否可以从 scheduled 变成 running，idempotency key 是否重复。query side 关注的是读取是否快、返回结构是否适合调用方、分页和过滤是否高效。

CQRS 解决的主要矛盾是：写模型和读模型经常需要完全不同的结构。

写模型通常要表达业务不变量：

1. 一个任务不能从 succeeded 回到 running。
2. 只有持有有效 lease 的 worker 才能 complete task。
3. workflow step 的依赖全部成功后才能调度。
4. 同一个 idempotency key 的 command 只能生效一次。
5. 事件追加必须保持 stream 内顺序。

读模型通常要满足查询形状：

1. 按状态分页列出任务。
2. 展示 workflow 当前 DAG 执行进度。
3. 展示 worker 心跳和负载。
4. 统计模型最近延迟和失败率。
5. 给 dashboard 返回反规范化后的摘要。

如果强行用一个模型同时满足两边，写路径会被查询索引污染，读路径又要理解复杂业务规则。CQRS 把二者拆开后，写侧可以保持严格状态机和事件追加，读侧可以使用 materialized view、缓存、搜索索引、宽表、聚合表。

在 LogServe 里，CQRS 的影子很明显：

1. 写路径：`SubmitTask`、`CompleteTask`、workflow step 状态推进、actor command 处理等操作，本质上是 command。它们会检查状态、追加事件，再更新 metadata。
2. 读路径：`GetTask`、`ListTasks`、`GetWorkflow`、`ListWorkerLoads`、dashboard 查询等操作，本质上是 query。它们主要读取 metadata store 或由日志投影出的统计视图。
3. 控制面事件：`TaskSubmitted`、`TaskStarted`、`StepSucceeded`、`LLMCompleted` 等事件把写侧事实投影到读侧视图。

CQRS 并不等于 event sourcing。两者经常一起出现，但边界不同：

```text
CQRS:
  关注命令模型和查询模型分离。

Event sourcing:
  关注状态变化是否以事件序列作为主要持久化形式。
```

一个系统可以有 CQRS 但没有 event sourcing。例如写侧更新关系数据库规范化表，读侧异步同步到 Elasticsearch 或 Redis。这个系统读写分离了，但状态来源仍然是关系数据库当前表。

一个系统也可以有 event sourcing 但没有明显的 CQRS。例如所有读取都通过 replay 当前 aggregate 直接返回，没有单独读模型。这样仍是 event sourcing，只是查询性能通常会受限。

CQRS 的收益主要有四点：

1. 读写可以独立扩展。读多写少的系统可以横向扩展查询视图。
2. 查询模型可以贴近 UI/API，不必暴露复杂领域模型。
3. 写模型可以聚焦不变量，减少“为了报表而污染写路径”的问题。
4. 安全权限可以分离，例如允许更多服务读视图，但只有少数服务能发 command。

代价也很明确：

1. 系统从强一致读变成可能的最终一致读。
2. 需要维护投影、补偿、重放、版本迁移。
3. 调试复杂度上升，因为一次写入可能跨 command handler、event store、projection、read API。
4. 测试范围扩大，需要分别测试 command correctness、projection correctness 和 query behavior。

Martin Fowler 对 CQRS 的态度很谨慎：它适合复杂领域中的特定部分，不应盲目套在整个系统上。面试里这点很重要。CQRS 不是架构身份标签，而是一种在读写压力、模型复杂度、查询形状明显分化时才值得引入的分离手段。

面试里可以这样答：

```text
CQRS 的基本思想是把 command side 和 query side 分开。command 负责表达意图、校验不变量并改变状态；query 负责读取数据，不产生副作用，并可以使用专门优化过的 read model。

它不要求一定使用 event sourcing，也不要求一定拆成两个数据库。拆分的粒度要由复杂度决定。LogServe 中提交任务、推进 workflow、完成任务属于写侧命令；查询任务状态、workflow 状态、worker 负载属于读侧查询。共享日志和 metadata 投影让读写路径可以采用不同模型，但也带来了投影延迟、补偿和测试复杂度。
```

## Q004. event sourcing 的核心思想是什么？

**回答：**

event sourcing 的核心思想是：系统不只保存对象的当前状态，而是把每一次状态变化都作为不可变事件追加到事件存储中；当前状态由这些事件按顺序重放得到。

Martin Fowler 对 Event Sourcing 的经典表述是：把应用状态的所有变化捕获为事件序列。Microsoft 的 Event Sourcing 模式也强调，用 append-only store 记录完整动作序列，这个事件存储作为 system of record，再通过重放事件 materialize 出领域对象或查询视图。

普通状态存储关心的是：

```text
task(id=1).status = "succeeded"
```

event sourcing 关心的是：

```text
TaskSubmitted(task=1)
TaskStarted(task=1, worker=A)
TaskCompleted(task=1, worker=A, resultRef=...)
```

前者只告诉你“现在是什么”，后者还告诉你“为什么变成现在这样”。如果事件足够完整、顺序足够可靠、replay 逻辑足够确定，那么系统可以通过事件流重新构造当前状态。

event sourcing 一般需要定义这些基础要素：

1. stream identity：事件属于哪个对象或聚合，例如 `task:{id}`、`workflow:{id}`、`actor:{id}`。
2. sequence/order：同一 stream 内事件必须有明确顺序，通常是递增 sequence number。
3. event schema：事件类型和 payload 要稳定，并支持版本演进。
4. append-only 语义：历史事件不应被随意改写，修正错误通常追加补偿事件。
5. idempotency：重试 command 时不能产生重复事实。
6. concurrency control：并发命令写同一 stream 时要检测 expected version 或使用等价 fencing。
7. replay handler：从空状态应用事件，得到当前状态。
8. projection：把事件流投影成 query model 或 materialized view。
9. snapshot：在事件流很长时，用快照减少 replay 成本，但快照不能替代事件语义。

event sourcing 解决的主要问题是正确性、可恢复性和可追溯性。它让系统在崩溃后能从事件重新恢复状态，也让开发者可以回答“某个状态是如何一步步形成的”。它还支持时间旅行查询、重新计算读模型、修复投影 bug、构造新 materialized view。

但 event sourcing 并不免费。它把复杂度从“当前表更新”转移到了“事件建模、版本演进、重放、投影一致性、幂等和并发控制”。事件一旦成为事实来源，就不能像普通日志那样随便改字段、删记录或只保留摘要。

在 LogServe 中，event sourcing 的思路体现在共享日志和 bootstrap 流程上：

1. `internal/logstore` 提供 append/read 语义，record 中包含 stream id、sequence、event type、idempotency key、payload、timestamp 和 CRC。
2. 控制面写路径先追加事件，再更新 metadata store。
3. `BootstrapFromLog` 启动时读取 task、workflow、actor、worker、model、LLM stats 等 stream，把事件重放成当前 metadata。
4. 对于任务，`TaskSubmitted`、`TaskStarted`、`TaskRedelivered`、`TaskCompleted`、`TaskFailed` 等事件决定 task 当前状态。
5. 对于 workflow，`StepScheduled`、`StepStarted`、`StepSucceeded`、`WorkflowCompleted` 等事件决定 DAG 执行视图。

这说明 LogServe 的控制面状态不是只靠 metadata 当前表存在。metadata 可以看作事件日志的投影。只要日志还在，系统就可以重新构造任务和 workflow 的当前视图。

同时要如实说明边界：LogServe 是单节点/多进程机制验证，不是完整生产级分布式 event sourcing 平台。比如它不应被夸大成跨区域全局有序、严格 exactly-once、支持任意复杂 schema migration 的系统。面试里这样说反而更可靠：它实现了 event-sourcing 的关键机制切片，即 append-only 事件日志、幂等写入、按 stream replay、metadata 投影和启动恢复；生产化还需要更强的复制、保留、迁移、监控和灾备能力。

event sourcing 的典型写入流程可以概括为：

```text
1. 接收 command
2. 读取当前 aggregate 状态或版本
3. 校验 command 是否合法
4. 生成一个或多个 domain event
5. 以 expected version 追加事件
6. 异步或同步更新 projection/materialized view
7. query side 从 projection 读取当前状态
```

如果 projection 更新失败，事件仍然存在，系统可以重放修复。如果事件追加失败，则 command 不应被视为成功，也不应该提前暴露状态。

面试里可以这样答：

```text
event sourcing 的核心是把状态变化本身作为一等数据保存下来。系统持久化的是按顺序追加的事件序列，当前状态由事件 replay 得到，查询视图由事件 projection 得到。

它的关键不变量包括事件不可变、stream 内有序、事件 schema 可演进、写入幂等、并发有版本控制、replay handler 确定。LogServe 的共享日志和 BootstrapFromLog 就体现了这种思路：控制面先写 TaskSubmitted、StepSucceeded 等事件，再更新 metadata；重启时可以从日志恢复 metadata。边界是它验证的是单节点共享日志机制，不能夸大成完整分布式 event store。
```

## Q005. event sourcing 和 audit log 的区别是什么？

**回答：**

event sourcing 和 audit log 都会记录“发生过什么”，但它们的系统地位完全不同。

event sourcing 中，事件是状态变化的事实来源。系统当前状态由事件重放得到，materialized view 可以从事件重建。audit log 更像事后审计记录，用来回答谁在什么时候做了什么，便于安全审计、合规、排障或用户行为追踪；它通常不承担恢复业务状态的责任。

一个简单判断方法是：把当前数据库删掉，只保留日志，系统能否恢复业务状态？

如果答案是“可以通过日志 replay 恢复”，这些日志更接近 event-sourced events。

如果答案是“不行，日志只够人工排查，无法重建完整状态”，它更接近 audit log。

比如订单系统里有两种记录方式：

```text
Audit log:
  2026-06-19 10:01:03 user=42 action="updated order" order=1001 ip=...

Event sourcing:
  OrderCreated(order=1001, buyer=42)
  ItemAdded(order=1001, sku=A, quantity=2)
  ShippingAddressChanged(order=1001, address=...)
  OrderSubmitted(order=1001)
```

audit log 的记录对安全和排障有价值，但“updated order”通常不足以重建订单状态。event sourcing 的事件必须表达状态转移需要的全部信息，并且具备顺序、版本、幂等和 replay 语义。

二者可以从以下角度区分：

| 维度 | Event Sourcing | Audit Log |
| --- | --- | --- |
| 系统地位 | 状态事实来源 | 审计、合规、排障记录 |
| 是否驱动业务状态 | 是，当前状态由事件 replay/projection 得到 | 通常否，当前状态在数据库或其他系统中 |
| 事件设计 | 面向领域状态转移，必须可重放 | 面向可读性、追踪性、合规上下文 |
| 顺序要求 | 强，至少 aggregate/stream 内有序 | 可能只要求时间戳大致可排序 |
| 完整性要求 | 高，缺事件会破坏状态恢复 | 可按合规要求保留，未必能恢复状态 |
| schema 演进 | 必须严肃设计版本兼容 | 可以更灵活，但要满足审计查询 |
| 修改历史 | 通常追加补偿事件，不改写旧事件 | 视合规要求可能也不可改，但语义不同 |
| 查询方式 | 常通过 projection/read model 查询 | 常通过日志搜索、SIEM、审计报表查询 |

event sourcing 也能带来审计能力，因为完整事件流天然提供了“状态如何变化”的记录。但这不代表 audit log 就等于 event sourcing。很多系统有非常详细的 audit log，仍然不是 event-sourced，因为它们的 source of truth 是关系数据库当前表，日志只是旁路记录。

在 LogServe 里，这个区别也很清楚。`TaskSubmitted`、`TaskStarted`、`TaskCompleted`、`StepSucceeded`、`ActorCommandApplied`、`LLMCompleted` 这类事件被用于重建 metadata、workflow 状态、actor 状态或统计视图，因此它们是控制面事实事件。相对地，如果系统另有 HTTP access log、debug log、operator action log，这些日志可以帮助审计和排障，但通常不能单独恢复任务或 workflow 状态。

这个边界在故障恢复时会暴露得非常明显：

1. 如果 event store 丢了一个 `TaskCompleted` 事件，metadata replay 后任务可能仍然 running 或 queued，系统状态会错误。
2. 如果 audit log 丢了一条“用户点击完成按钮”的记录，业务数据库可能仍然正确，只是审计链路不完整。
3. 如果 metadata 被手工改了，但没有对应 event，event sourcing 系统重启后会把这个改动丢掉，因为它无法从事实来源解释。
4. 如果 audit log 记录了操作，但业务事务回滚了，审计里可能出现“尝试过”的痕迹，这不一定表示业务状态已经改变。

设计事件时要避免把 event-sourced events 写成日志消息。下面这种事件对人可读，但对 replay 不够稳定：

```text
Task updated by worker A
```

更适合作为状态事件的是：

```text
TaskStarted(taskID, workerID, leaseID, startedAt)
TaskCompleted(taskID, workerID, leaseID, resultRef, completedAt)
```

后者可以驱动状态机，也可以校验 lease，还能在 replay 时决定最终状态。audit log 可以额外记录操作者、IP、User-Agent、request id、审批链等上下文；这些字段未必参与领域状态计算。

面试里可以这样答：

```text
event sourcing 的事件是业务状态的事实来源，系统当前状态要能从事件序列 replay 出来；audit log 是旁路审计记录，用来追踪谁在什么时候做了什么，通常不承担状态恢复职责。

二者可能都 append-only，也都能帮助排障，但判断标准是：删除当前状态后，只靠这些记录能不能重建业务状态。能重建的是 event-sourced event；只能帮助人工解释的是 audit log。LogServe 中 TaskSubmitted、StepSucceeded、LLMCompleted 等事件会参与 metadata 和统计视图重建，所以属于控制面事实事件；HTTP access log 或 debug log 则只是审计/观测日志。
```

## Q006. event sourcing 的 replay reducer 需要满足哪些性质？

**回答：**

replay reducer 是 event sourcing 里最容易被低估的一块。它的形式看起来很简单：

```text
newState = reduce(oldState, event)
```

真正困难的是，这个函数会在很多场景下被反复调用：服务启动恢复、读模型重建、投影 bug 修复、历史事件回放、snapshot 之后追 tail、灰度新 reducer、甚至线上排查时临时重算状态。只要 reducer 不是稳定的，event sourcing 的“可以重建状态”就只剩口号。

一个合格的 replay reducer 至少要满足下面这些性质。

**第一，确定性。**

同一份初始状态、同一串事件、同一个 reducer 版本，必须得到同一个结果。reducer 里不应该读当前时间、生成随机数、访问外部服务、查询正在变化的数据库，也不应该依赖 map 遍历顺序这类不稳定行为。

错误写法大概是这样：

```text
on TaskStarted:
  state.startedAt = now()
```

正确做法是让事件本身携带时间：

```text
TaskStarted(taskID, workerID, leaseID, startedAt)
on TaskStarted:
  state.startedAt = event.startedAt
```

否则同一条事件今天 replay 一次、明天 replay 一次，会得到不同状态。系统很难解释哪个才是真相。

**第二，无副作用。**

replay reducer 不能发邮件、扣款、调用 webhook、创建对象、删除文件、更新远程缓存。重放事件的目标是恢复状态，不是重新执行当年的业务动作。

这点在面试里很关键。很多人说“事件来了就处理”，但没有区分两类 handler：

```text
projection/replay handler:
  只更新本地状态或 read model，可以重复运行。

side-effect handler:
  发送通知、调用外部系统、触发财务动作，必须有幂等和去重边界。
```

event sourcing 的 replay reducer 应该属于第一类。第二类可以由事件驱动，但不能在任意 replay 时自动触发。

**第三，按 stream 顺序应用。**

event sourcing 不是默认乱序可交换模型。大多数业务事件对顺序敏感：`TaskCompleted` 在 `TaskStarted` 之前出现，含义就不一样；`OrderCanceled` 和 `OrderPaid` 的顺序也会影响最终状态。

因此 reducer 通常要求同一个 aggregate 或 stream 内按 sequence 递增应用事件。跨 stream 是否需要全局顺序，要看业务模型。LogServe 的 log record 有 per-stream `Seq`，`readAllLog` 也是按 `FromSeq` 分页读某个 stream，这说明它的基本恢复单位是 stream，而不是一个全局总序。

不要把“reducer 必须可重放”和“reducer 必须对事件顺序不敏感”混在一起。除非事件专门按 CRDT、计数器增量或集合并集这类可交换语义设计，否则顺序就是状态的一部分。

**第四，能处理重复投递。**

官方资料里也提醒，event delivery 经常是 at-least-once，projection handler 必须能处理重复事件，否则 materialized view 会 drift。即使 event store 自己用 idempotency key 防止重复 append，projection 仍然可能因为崩溃重启，在“已经写 view、还没写 checkpoint”之后再次处理同一条事件。

常见做法是：

1. 在 projection 进度表里记录 `last_processed_seq`。
2. 每次只接受 `seq == last_processed_seq + 1` 的事件。
3. 如果收到 `seq <= last_processed_seq`，直接跳过。
4. 如果收到 `seq > last_processed_seq + 1`，说明中间缺事件，不能继续悄悄处理。

如果 reducer 本身的更新是幂等的，也可以作为第二层保护。例如 `TaskCompleted` 把 task status 设成 completed，比“完成计数加一”更容易做成幂等。

**第五，能识别非法转移。**

reducer 不是简单地把 payload 复制到 state。它应该表达状态机约束，至少在重放时发现明显错误。比如：

```text
TaskCompleted 不能在没有 TaskSubmitted 的情况下出现。
TaskStarted 的 lease epoch 不能比当前 epoch 更旧。
WorkflowCompleted 不应出现在仍有 running step 的情况下。
ActorCommandApplied 的 commandSeq 应该递增。
```

遇到非法事件时有两种处理策略。恢复路径可以 fail fast，让系统停在可排查状态；在线投影可以把事件送入 dead-letter 或 quarantine，再报警。最危险的做法是静默跳过，因为 view 会继续向前，看起来“健康”，实际上已经和 event log 分叉。

**第六，对旧 schema 宽容，但不能吞掉语义。**

事件一旦写入 event store，后续代码要长期读它。Microsoft 的 Event Sourcing 文档提到 tolerant deserialization、event versioning、upcasting 等策略，本质上都是在解决 reducer 如何面对历史事件。

比较稳妥的做法是：

```text
event envelope:
  event_type
  event_version
  stream_id
  seq
  payload

replay path:
  deserialize old event
  upcast to current in-memory event shape
  apply reducer
```

新增可选字段通常可以用默认值兼容。改变字段含义就危险得多，最好引入新 event type 或新 version，而不是让同名字段在不同年代表示不同意思。

**第七，snapshot 兼容。**

reducer 要能从空状态开始 replay，也要能从 snapshot 状态开始 replay tail。也就是说：

```text
reduce(emptyState, events[1..N]) == reduce(snapshotAtK, events[K+1..N])
```

如果 snapshot 只存了部分状态，或者 reducer 版本变了但 snapshot 没有版本号，就可能出现“全量 replay 正确，从 snapshot 恢复错误”的问题。snapshot 是优化，不是新的事实来源；这一点 Microsoft 文档也明确强调过。

**第八，性能可控。**

reducer 会被大量调用。它不应该每处理一个事件都做 O(N) 扫描，导致全量 replay 变成 O(N²)。例如 workflow 事件很多时，如果每个 step 事件都重新扫描整个 DAG，启动恢复时间会很快失控。

更好的方式是维护增量状态：

```text
on StepSucceeded:
  mark step succeeded
  decrement downstream dependency counters
  enqueue newly ready steps
```

当然，增量状态也要能从事件流解释，不能偷偷依赖外部缓存。

放到 LogServe 里看，`replayTaskSpec` 的思路就是一个小型 reducer：它读取 `TaskSubmitted`、`TaskStarted`、`TaskRedelivered`、`TaskCompleted`、`TaskFailed` 等事件，重建 task spec、status 和 lease epoch。`BootstrapFromLog` 再把重建结果写入 metadata store。这里的关键边界是：reducer 只恢复控制面状态，不应该重新执行任务，也不应该重新调用 LLM。

面试里可以这样答：

```text
event sourcing 的 replay reducer 要确定、无副作用、按 stream 顺序应用事件，并且能处理重复投递、旧 schema、非法状态转移和 snapshot 恢复。它不能读当前时间、随机数或外部服务，否则同一串事件 replay 会得到不同结果。

我会把 reducer 当成状态机函数来设计：输入是旧状态和事件，输出是新状态或明确错误。副作用 handler 要和 replay handler 分开。LogServe 里 task、workflow、actor 的 bootstrap 就依赖这个性质：日志可以重放成 metadata，但 replay 不能重新执行任务或重新发起外部调用。
```

## Q007. materialized view 落后于日志时会出现什么现象？

**回答：**

materialized view 落后于 event log，本质上就是 source of truth 已经前进了，但 read model 还停在旧位置。用户看到的是“系统明明已经写成功，查询却像没发生过”。在 CQRS 和 event sourcing 系统里，这不是罕见异常，而是 eventual consistency 的正常代价，只是系统必须把这个代价控制在可解释、可观测、可恢复的范围内。

常见现象有几类。

**第一，读后不一致。**

用户提交 command 成功，马上查 read API，却看到旧状态：

```text
SubmitTask -> 返回成功
GetTask -> 404 或 queued
稍后再查 -> running / succeeded
```

这就是典型的 read-your-writes 问题。写入 event store 已经成功，但 materialized view 还没处理到这条事件。Microsoft 的 Event Sourcing 文档也明确说，创建 materialized view 或 projection 时系统会表现为最终一致；从应用处理请求、写入事件、发布事件，到 consumer 更新视图之间存在延迟。

**第二，状态显示过期。**

任务已经完成，metadata 仍显示 running；workflow 已经有 step 成功，页面还显示执行中；模型延迟统计已经被新事件改变，但调度器读到的 EWMA 还是旧值。

这类问题通常不会破坏事实来源，但会影响用户判断和控制面策略。比如 LogServe 的 LLM 统计如果落后，调度器可能继续认为某个模型很快，实际最近已经变慢；这不会让日志错，但会让调度选择短时间内不够准确。

**第三，调度做出旧决策。**

如果调度器完全依赖 materialized view，view 落后会产生更实际的后果：

1. 已经完成的任务仍被看作 running，导致资源迟迟不释放。
2. 已经 ready 的 workflow step 没被调度，导致 workflow 卡住一段时间。
3. worker 心跳或负载 view 落后，导致任务分配给已经不可用或过载的 worker。
4. lease 状态落后，可能触发不必要的 redelivery。

这时要靠 lease、epoch、fencing token、idempotency key 和完成时校验兜底。读模型可以落后，但真正改变事实的 command 不能只信旧 view。

**第四，GC 和生命周期判断偏差。**

如果对象存储的 GC 依赖 materialized view 判断对象是否可达，view 落后会很危险。比如 result reference 已经写入 event log，但引用视图还没更新，GC 可能误以为对象无人引用。反过来，引用已经删除但 view 还没追上，GC 会延迟释放对象。

所以生命周期类操作通常要保守：

```text
对象删除条件 = event log 确认不可达
           + materialized view 已追到足够新的 seq
           + grace period 已过
```

仅凭一个可能落后的 view 做硬删除，风险太大。

**第五，监控指标出现“负延迟”或跳变。**

view 落后时，dashboard 里的计数、状态分布、p99、失败率可能突然跳变。比如 projection 卡了 5 分钟，恢复后一次性处理大量事件，图表上会像某个时间点突然完成了很多任务。真实情况是事件早就发生了，只是 view 才刚追上。

因此指标最好同时区分两种时间：

```text
event_time: 事件发生时间
materialized_time: 视图处理时间
```

只看 materialized_time，排障时很容易误判。

对 LogServe 来说，当前控制面多数写路径是 append log 后同步更新 metadata，在线情况下 view lag 不一定明显。但启动恢复、bootstrap、LLM stats replay、未来异步 projection 或多进程部署都会遇到这个问题。`BootstrapFromLog` 从 `FromSeq=1` 读取各 stream 重建 metadata；在它完成之前，metadata 就不是完整视图。系统要么等 bootstrap 完成再对外服务，要么明确进入 degraded mode，只允许有限查询。

工程上通常会暴露几个观测指标：

```text
projection_lag_events = event_store_last_seq - projection_last_seq
projection_lag_seconds = now - last_projected_event_time
projection_apply_errors_total
projection_oldest_unapplied_event_age
read_model_rebuild_duration
```

有了这些指标，view lag 才能从“用户说状态不对”变成可定位的问题。

面试里可以这样答：

```text
materialized view 落后于日志时，source of truth 已经更新，但查询模型还没追上。现象包括写后马上读到旧状态、任务或 workflow 状态显示过期、调度基于旧负载做决策、dashboard 指标跳变，以及 GC 误判对象可达性。

这不是简单 bug，而是 CQRS/event sourcing 的 eventual consistency 成本。解决思路是暴露 projection lag，给用户接口明确 pending/accepted 语义；真正改变事实的 command 要用 lease、epoch、idempotency 和日志校验兜底，不能只依赖可能落后的 read model。
```

## Q008. 如何检测 materialized view 和 event log 不一致？

**回答：**

检测 materialized view 和 event log 不一致，不能只靠“线上没人报错”。view drift 经常是悄悄发生的：某个事件被跳过、重复应用、旧 reducer 处理错字段、projection 崩溃后 checkpoint 提前推进，查询结果看起来仍然有数据，但已经不是 event log 能解释出来的状态。

比较可靠的检测方法有几层。

**第一，记录投影进度。**

每个 materialized view 都应该有自己的 projection progress：

```text
projection_name
stream_id 或 partition_id
last_processed_seq
last_processed_event_time
reducer_version
updated_at
checksum/hash
```

这样至少能回答三个问题：

1. view 处理到 event log 的哪个位置？
2. 有没有 stream 卡住？
3. 当前 view 是哪个 reducer 版本生成的？

如果 event log 的最新 sequence 是 10000，而 projection 只处理到 9300，这不是“不一致”，而是“落后”。如果 projection 声称已经处理到 10000，但重算结果不同，那才是 view drift。

**第二，检查 sequence 连续性。**

event log 应保证同一 stream 内 sequence 递增。projection 处理时也应要求连续：

```text
next_event.seq == last_processed_seq + 1
```

如果发现 seq 跳跃，要停下来报警。继续处理会让错误扩大。LogServe 的 log record 有 per-stream `Seq`，`ReadLog` 也支持从指定 `FromSeq` 读取，这给连续性检测和增量追赶提供了基础。

**第三，做重放对账。**

最直接的办法是从 event log 重新 replay 出一份临时视图，然后和线上 materialized view 比较：

```text
rebuilt = replay(event_log[1..N])
current = read_materialized_view()
diff(rebuilt, current)
```

对账不一定每次全量做。可以按 stream、按时间窗口、按对象采样，也可以只比较摘要：

```text
task_count_by_status
workflow_count_by_state
last_seq_per_stream
hash(canonical_json(projected_state))
sum(result_ref_count)
```

对热点对象或生命周期敏感对象，例如 result reference、任务租约、workflow step 状态，可以提高对账频率。

**第四，给 projection 写幂等约束。**

很多不一致来自重复应用事件。比如 `TaskCompleted` 被处理两次，如果 read model 里只是把 status 设置成 completed，问题不大；如果它把 completed_count 加一，就会多算。

可以在 view 表里记录每条 event 的应用痕迹，或者用唯一约束限制重复消费：

```text
projection_applied_events(projection_name, stream_id, seq)
unique(projection_name, stream_id, seq)
```

这会增加存储开销，但对金融、库存、对象引用计数这类场景很值。

**第五，区分数据损坏和投影错误。**

event log 本身也可能损坏，所以检测链路要分层：

1. record CRC 或 checksum 验证事件内容是否损坏。
2. sequence 检查验证事件顺序是否连续。
3. schema/version 检查验证事件是否可解析。
4. reducer replay 检查验证事件能否生成合法状态。
5. view diff 检查验证当前 materialized view 是否等于 replay 结果。

LogServe 的 log record 包含 CRC32，logstore 的恢复和读取路径也围绕 segment/index 做了校验，这能帮助发现底层记录损坏。但 CRC 只能说明“读回来的 record 没坏”，不能说明 projection 正确。projection 正确性还需要 replay 对账。

**第六，用业务不变量发现异常。**

有些 drift 不需要全量 replay 也能被不变量抓住：

```text
completed task 不应再有有效 lease
workflow completed 后不应还有 running step
actor command count 不应小于 submitted/applied command seq
result object 引用计数不能为负
projection last_seq 不能超过 event log next_seq - 1
```

这些检查可以放在后台 reconciler、管理接口或者定期 CI 测试里。它们不能证明 view 一定正确，但能快速发现明显错位。

**第七，新旧 reducer 双跑。**

当 reducer 逻辑要改时，可以让旧 projection 和新 projection 并行消费同一段 event log，比较输出差异。这个方法对 schema migration 很有用。不要直接拿新 reducer 覆盖线上 view，然后才发现老事件无法正确 replay。

如果放到 LogServe 的现状里，我会这样补强检测：

1. 为 metadata projection 增加 `projection_progress`，记录每个 `task:`、`workflow:`、`actor:`、`llm:` stream 处理到的 seq。
2. bootstrap 后可选跑一次 sample replay，对比 metadata 中的 task status、workflow step 状态、actor command count。
3. 对 result reference 这类生命周期对象，记录 event log 中引用创建/释放事件的 canonical hash，和 metadata view 中的引用状态对账。
4. 对 `LLMCompleted` 统计视图，记录最后处理的 event seq 和 event count，避免重复事件让 EWMA 或计数漂移。

面试里可以这样答：

```text
检测 view 和 event log 不一致，先要区分“落后”和“漂移”。落后可以看 projection_last_seq 和 event_log_last_seq；漂移要靠 replay 对账，把日志重放成临时状态，再和 materialized view 比较。

工程上我会做四件事：每个 projection 记录 last_processed_seq 和 reducer_version；处理事件时检查 seq 连续性；定期按 stream 或采样对象做 replay diff；再用业务不变量兜底，比如 completed task 不应有有效 lease、workflow completed 不应有 running step。CRC 能发现日志损坏，但不能证明投影正确，投影还要单独验证。
```

## Q009. 读模型重建会对系统启动时间造成什么影响？

**回答：**

读模型重建会把系统启动从“加载配置、打开端口”变成“先扫日志、重放事件、重建索引、再对外服务”。事件越多、payload 越大、reducer 越复杂，启动时间越长。这个成本通常是线性的，但实现不好时会被放大成平方级。

最简单的成本模型是：

```text
startup_time =
  read_event_log_time
  + deserialize_time
  + reducer_apply_time
  + materialized_view_write_time
  + index/cache_warmup_time
```

如果每条事件都要读磁盘、反序列化 protobuf/JSON、更新 metadata store、刷新索引，那么 1 万条事件和 100 万条事件的启动体验会完全不同。读模型越多，重建成本还会乘上 projection 数量。

Microsoft 的 Event Sourcing 文档把这个问题叫 entity state re-creation：eventstream 很长时，每次从头 replay 会消耗时间和计算资源，因此通常需要 snapshot。Martin Fowler 也提到，从空状态应用全部事件概念上简单，但事件很多时会很慢；当前 application state 可以作为可派生缓存保存起来，崩溃后只 replay snapshot 之后的事件。

在 LogServe 当前实现里，这个影响比较直接。`BootstrapFromLog` 会读取模型、worker、scheduler、backpressure、task、workflow、actor、LLM stats 等 stream。`readAllLog` 从 `FromSeq=1` 开始，以 `bootstrapReadLimit=1000` 分页读到尾部。也就是说，某个 stream 有 30 万条事件，就要分页读完整个 stream，再交给 reducer/materializer。

这会带来几种启动现象。

**第一，ready 时间变长。**

服务进程可能已经起来了，但控制面还不能安全接请求。因为 metadata view 没重建完，调度器不知道哪些任务 queued，API 也不能准确返回 workflow 状态。比较稳妥的做法是 bootstrap 完成前不对外报告 ready。

**第二，冷启动 I/O 峰值变高。**

启动时集中扫描 segment、读取 payload、重建 metadata，会和正常请求抢磁盘和 CPU。如果多个实例同时启动，还会形成启动风暴。分布式系统里这点更明显：所有实例一起从 event store 拉历史，event store 会先被恢复流量打满。

**第三，内存压力上升。**

如果重建过程先把 records 全部读到内存，再一次性处理，内存会随事件数增长。LogServe 的 `readAllLog` 返回一个 slice，这对机制验证和小规模数据可以接受；生产化时更好的方式是边读边 apply，或者分批 apply 后释放。

**第四，锁和写放大。**

重建读模型不是只读操作。它会写 metadata store、建索引、更新缓存。如果 metadata store 是关系数据库或带锁的内存 map，启动期间会有大量写入。在线恢复时还可能和新写入竞争锁。

**第五，用户看到旧状态或不可用状态。**

如果系统允许 bootstrap 未完成就服务查询，用户会看到不完整 view：任务少了、workflow step 没恢复、LLM 统计为空。这个问题比启动慢更难排查，因为它表现为“部分正确”。

缓解办法有几类。

**第一，用 snapshot 减少 replay 范围。**

对长 stream 保存 snapshot：

```text
snapshot(state_at_seq=500000)
replay events[500001..latest]
```

actor、workflow、长生命周期 aggregate 很适合这么做。LogServe 的 actor snapshot 和 trim 方向已经体现了这种优化思路：snapshot 记录某个命令序列后的 actor state，后续恢复不必永远从第一条 actor 事件开始。

**第二，保存 projection checkpoint。**

如果 materialized view 本身是持久化的，重启时不必全量重建，只需要从 `last_processed_seq + 1` 追 tail。全量 replay 只在 view 丢失、reducer 版本变化或对账失败时触发。

**第三，分层启动。**

系统可以先恢复最小必要视图，再后台恢复低优先级视图。例如调度必须依赖 task/workflow view，就先恢复它；dashboard 聚合、历史报表、LLM 统计可以晚一点追。

**第四，按 partition 并行。**

不同 stream 或不同 shard 之间如果没有强顺序依赖，可以并行 replay。注意同一 stream 内仍要保持 sequence 顺序。

**第五，让 read model 可降级。**

一些查询可以明确返回：

```text
503 projection rebuilding
202 accepted but not yet visible
partial result with projection_lag metadata
```

这比返回看似完整但其实缺数据的结果更好。

面试里可以这样答：

```text
读模型重建会把启动时间变成和事件数量、payload 大小、reducer 成本、metadata 写入成本相关的过程。全量 replay 时，服务可能需要先扫描 event log、反序列化事件、重建索引和缓存，然后才能安全对外 ready。

LogServe 当前 bootstrap 基本是从每个 stream 的 FromSeq=1 分页读到尾部，再重建 metadata；数据量大时启动时间会线性增长。缓解方式是 snapshot、projection checkpoint、按 stream 并行 replay、分层启动，以及在 view 未追平前明确进入 rebuilding/degraded 状态。
```

## Q010. 如何做增量物化而不是全量 replay？

**回答：**

增量物化的核心思路很直接：materialized view 不要每次从头扫描 event log，而是记住自己已经处理到哪里，下次从那个位置之后继续读。

全量 replay 是：

```text
for event in log[1..latest]:
  view = reduce(view, event)
```

增量物化是：

```text
last = projection_progress.last_processed_seq
for event in log[last+1..latest]:
  view = reduce(view, event)
  projection_progress.last_processed_seq = event.seq
```

难点不在这几行伪代码，而在崩溃、重复投递、多 stream、schema 变更和事务边界。

一个比较稳的设计会有三张逻辑表或等价结构：

```text
event_log(stream_id, seq, event_type, payload, crc, timestamp)

materialized_view(...)

projection_progress(
  projection_name,
  stream_id_or_partition,
  last_processed_seq,
  reducer_version,
  updated_at,
  checksum
)
```

处理流程通常是：

```text
1. 读取 projection_progress，拿到 last_processed_seq。
2. 从 event log 读取 seq > last_processed_seq 的事件。
3. 按 seq 顺序逐条 apply reducer。
4. 在同一个事务里更新 materialized_view 和 projection_progress。
5. 如果事务失败，不推进 checkpoint。
6. 重启后从旧 checkpoint 继续，重复事件由幂等逻辑跳过。
```

最理想的情况是 view 更新和 checkpoint 更新在同一个数据库事务里完成。这样不会出现“view 已更新但 checkpoint 没推进”或“checkpoint 推进但 view 没更新”的半成品。

如果做不到同事务，就要接受 at-least-once，并把 reducer 做成幂等：

```text
crash after view update before checkpoint:
  重启后会再次处理同一事件。
  reducer 必须保证重复处理结果不变。

crash after checkpoint before view update:
  这是更危险的情况。
  系统会以为事件已处理，但 view 实际没变。
  应避免这种写入顺序，或者用 applied_events 表、outbox、WAL、reconcile 修复。
```

所以一般选择：

```text
apply view update -> write applied event marker/checkpoint -> commit
```

并且让这两步共享原子提交边界。

多 stream 场景要更小心。LogServe 的基本 log 读取是按 stream 的：`task:{id}`、`workflow:{id}`、`actor:{id}`、`llm:{id}`。如果 projection 只关心单个 stream，例如重建某个 task 状态，记录 per-stream seq 就够了。如果 projection 聚合多个 stream，例如 dashboard 统计或全局调度视图，就要记录每个 stream/partition 的进度，或者引入全局 event sequence。

常见做法是：

```text
projection_progress:
  projection=task_status, stream=task:123, last_seq=8
  projection=workflow_status, stream=workflow:abc, last_seq=31
  projection=llm_stats, partition=llm-events-0, last_seq=12039
```

如果没有全局顺序，就不要假装有一个“全系统已处理到 N”的 checkpoint。那会掩盖某些 stream 已追平、某些 stream 还落后的事实。

增量物化还要处理 reducer 版本。假设你改了 workflow reducer 的规则，旧 materialized view 是 reducer v1 算出来的，新代码是 v2。此时从 `last_seq+1` 继续处理不一定正确，因为历史事件的解释方式变了。

通常有几种选择：

1. reducer 变更向后兼容，可以继续增量。
2. 新建 projection v2，从头或从 snapshot 后重建，和旧 projection 双跑对账。
3. 对旧事件做 upcast，让 v2 reducer 看到统一事件形状。
4. 标记 projection stale，后台全量 rebuild。

对 LogServe 来说，从当前实现演进到增量物化，可以沿着已有能力做：

1. 利用 log record 的 `Seq` 和 `ReadLog(FromSeq)`，让 bootstrap 从 checkpoint 后读取，而不是总从 1 开始。
2. 给 metadata store 增加 projection progress，例如 `taskProjectionLastSeq(streamID)`、`workflowProjectionLastSeq(streamID)`。
3. 每处理一批事件后，把 metadata 更新和 progress 更新放在同一个事务或同一个锁保护的临界区里。
4. 对 `LLMCompleted` 统计视图记录最后处理的 seq 和事件 fingerprint，避免重复事件改变 EWMA。
5. 对 actor 使用 snapshot 的 `BeforeSeq` 思路：从最近 snapshot 恢复 actor state，再 replay snapshot 之后的事件。

需要强调的是，增量物化并不取消全量 replay。全量 replay 仍然是校验和修复手段。系统应该同时支持：

```text
normal path:
  从 checkpoint 增量追日志。

repair path:
  丢弃 view，从 event log 或 snapshot+tail 重建。

migration path:
  新 reducer 版本重建新 view，与旧 view 对账后切换。
```

面试里可以这样答：

```text
增量物化就是给每个 projection 记录处理进度，例如每个 stream 或 partition 的 last_processed_seq。重启后从 last_processed_seq + 1 继续读 event log，而不是从 1 全量 replay。

关键点是事务边界和幂等性：view 更新和 checkpoint 推进最好同事务提交；如果只能 at-least-once，就要用 seq、applied_events 或幂等 reducer 防止重复应用。LogServe 已经有 per-stream Seq 和 ReadLog(FromSeq)，所以可以自然演进为 checkpoint-based bootstrap；全量 replay 仍然保留，用于修复、对账和 reducer 版本迁移。
```

## Q011. event schema 变化如何影响历史 replay？

**回答：**

event schema 一变，历史 replay 最先受影响。event sourcing 把事件当成长期事实来源，旧事件不会因为代码升级而自动变成新格式。新代码启动后要读的是几个月、几年以前写下来的 payload；如果 reducer 只认识今天的结构，历史事件就可能反序列化失败，或者更糟，能反序列化但语义已经错了。

Microsoft 的 Event Sourcing 文档把这个问题列在核心注意事项里：event store 是永久信息来源，不应该直接更新事件数据；如果 persisted events 的 schema 要变，新旧事件合并 replay 会变得困难。它给出的策略包括 tolerant deserialization、event versioning、upcasting，以及作为最后手段的 in-place migration。这个判断很实用，因为 event schema 演进的问题不在“怎么改字段名”，而在“十年前的事件还能不能被今天的代码解释”。

schema 变化常见有几种。

**第一，新增字段。**

这是最容易处理的变化。比如 `TaskCompleted` 以前只有 `taskID`、`workerID`、`resultRef`，后来增加 `durationMs`。只要新增字段是可选的，reducer 给旧事件默认值，历史 replay 通常还能跑。

```text
TaskCompleted v1:
  taskID
  workerID
  resultRef

TaskCompleted v2:
  taskID
  workerID
  resultRef
  durationMs?
```

这类变化要求反序列化器能忽略未知字段，也能容忍缺失字段。JSON 和 protobuf 在这方面都能做，但业务 reducer 仍要明确默认语义。`durationMs = 0` 到底表示“耗时为 0”，还是“旧事件没有记录耗时”，不能含糊。

**第二，删除字段。**

删除字段比新增字段危险。新代码可能不再使用这个字段，但旧 reducer、旧 projection、历史审计、合规导出仍可能需要它。如果字段真的不再需要，通常不要改写旧事件，而是在新版本事件里不再写它；旧事件保持原样。

```text
旧事件仍然包含字段。
新事件不再包含字段。
replay reducer 同时支持两种形态。
```

**第三，字段改名。**

字段改名看起来是小重构，放进 event store 里就是 schema breaking change。`userID` 改成 `actorID`，如果没有版本号或 upcaster，新 reducer 可能读不到旧字段。

更稳的做法是保留旧字段解析逻辑，或者写 upcaster：

```text
upcast TaskStarted v1:
  actorID = userID
```

upcaster 的责任是在反序列化阶段把旧事件转换成当前内存结构，event store 中的旧事件不动。

**第四，字段语义改变。**

这是最危险的变化。比如 `amount` 以前单位是美元，后来改成美分；`status=completed` 以前表示 worker 返回成功，后来表示 result 已经落入对象存储。这种变化即使字段名没变，replay 结果也会错。

面对语义变化，最好引入新字段、新 event type 或新 version。不要让同一个字段在不同年代表达不同含义。

```text
PaymentCaptured v1:
  amountUsdDecimal

PaymentCaptured v2:
  amountCents
  currency
```

**第五，event type 改名或拆分。**

比如以前只有 `TaskFinished`，后来拆成 `TaskCompleted` 和 `TaskFailed`。历史 replay 时要知道旧的 `TaskFinished` 如何映射到新语义。如果旧事件 payload 里没有成功/失败信息，就无法无损转换，只能把旧事件作为单独分支处理，或者补一个迁移事件。

**第六，reducer 逻辑改变。**

schema 没变，reducer 也可能变。比如以前 `TaskRedelivered` 会把 running 改回 queued，后来引入 lease epoch 后，只有旧 epoch 才允许 redelivery。用新 reducer replay 旧事件，结果可能和旧系统运行时不同。

这类变化要靠历史 fixture 测试。把真实或脱敏后的旧事件样本放进测试，升级 reducer 时跑：

```text
given historical events v1/v2/v3
when replay with current reducer
then projected state equals expected state
```

如果不能证明旧事件仍然可解释，就不能贸然升级。

生产系统里通常会给事件加 envelope：

```text
{
  "event_id": "...",
  "stream_id": "task:123",
  "seq": 8,
  "event_type": "TaskCompleted",
  "event_version": 2,
  "schema_id": "task-completed.v2",
  "occurred_at": 1760000000000,
  "producer": "control-v1.7.3",
  "payload": { ... }
}
```

`event_version` 决定 reducer 或 upcaster 怎么处理；`schema_id` 方便连接 schema registry 或兼容性测试；`producer` 对排查很有用。不是所有系统都需要这么完整，但版本号和事件类型稳定性最好一开始就定下来。

LogServe 当前的 log record 里有 `StreamID`、`Seq`、`EventType`、`IdempotencyKey`、`Payload` 和 `CRC32`，payload 多数用 JSON。这个结构足够支撑机制验证，也便于调试；生产化时还缺一个显式 `EventVersion` 或 schema id。否则以后 `TaskSubmitted`、`WorkflowStarted`、`ActorCommandApplied` 的 payload 一旦变化，`BootstrapFromLog` 和 actor replay 就要靠手写兼容逻辑猜旧格式。

面试里可以这样答：

```text
event schema 变化会直接影响历史 replay，因为 event store 里的旧事件不会随着代码升级自动变成新格式。常见风险包括反序列化失败、字段语义漂移、旧 reducer 和新 reducer replay 结果不一致、snapshot 与事件版本不兼容。

我会给 event envelope 加 event_version 或 schema_id，新增字段走 tolerant deserialization，字段改名或结构调整走 upcaster，语义变化尽量引入新 event type。in-place rewrite 历史事件会破坏不可变审计语义，只能作为最后手段。LogServe 现在有 EventType 和 JSON payload，机制验证够用；生产化要补显式版本和历史事件 replay 测试。
```

## Q012. 事件中应该保存 command 还是保存 fact？

**回答：**

event log 里应该保存已经被系统接受并发生的 fact，而不是原始 command。command 可以保存，但它应该属于 command log、inbox、请求审计或幂等记录；它不应该替代领域事件。

command 表达的是“请求系统做什么”：

```text
SubmitTask
ReserveSeats
ApproveInvoice
DeleteUser
```

fact 表达的是“系统已经确认发生了什么”：

```text
TaskSubmitted
SeatsReserved
InvoiceApproved
UserErasureRequested
```

差别很关键。command 可能被拒绝，可能因为权限不足失败，可能因为业务规则变化而产生不同结果。fact 已经通过当时的校验，成为系统历史的一部分。event sourcing 的 replay 是重建历史事实，不是重新审判历史请求。

举一个座位预订的例子：

```text
Command:
  ReserveSeats(conferenceID=1, userID=42, seats=2)

Fact:
  SeatsReserved(conferenceID=1, userID=42, seats=2, reservationID=abc)
```

如果 event log 保存 command，十天后 replay 时可能会遇到完全不同的环境：座位数量变了，用户权限变了，限购规则变了，风控规则也变了。重新执行 command 可能失败。这样 replay 的状态就不再等于当时真实发生过的状态。

Microsoft 的 Event Sourcing 文档强调事件应该描述对象上的 action 或 logical change，也提醒事件设计要捕获业务意图，而不是只记录最终状态。它举的例子是“预订了两个座位”比“剩余座位变成 42”更有价值，因为前者说明发生了什么，后者只说明结果。这个例子也说明了 command 和 fact 的边界：`ReserveSeats` 是请求，`SeatsReserved` 是已发生的业务事实。

这并不意味着 command 没有价值。command 往往要保存这些信息：

1. 幂等 key：同一个请求重试时能返回相同结果。
2. 请求者身份：谁发起的操作。
3. 原始输入摘要：用于判断同一个 idempotency key 是否绑定同一 payload。
4. 拒绝原因：command 没产生 fact 时也要能解释。
5. 审计信息：IP、User-Agent、trace id、审批链。

但这些信息可以放在 command inbox、审计日志或事件 metadata 中，不一定放进领域事件 payload。领域事件要服务 replay、projection 和业务语义，不能变成完整 HTTP request dump。

LogServe 里已经有这种分工的影子。`SubmitTaskRequest` 是 command；写进日志的是 `TaskSubmitted` 事件。`CompleteTaskRequest` 是 command；日志里会出现 `StepSucceeded`、`StepFailed`、`TaskCompleted` 或 actor 相关事件。metadata store 里保存 idempotency fingerprint，用来判断同一个 idempotency key 是否重复提交了不同 payload。这说明 command 的幂等控制和 event 的事实记录是两个层次。

面试里可以这样答：

```text
event log 应该保存 fact，而不是直接保存 command。command 是请求，可能成功也可能被拒绝；fact 是系统已经接受并发生的状态变化。replay 需要重建历史事实，不能重新执行当年的请求，因为今天的业务规则、外部状态、时间和权限都可能变了。

command 可以保存在 inbox、审计日志或幂等表里，用来去重、排查和解释失败；事件本身最好用过去式命名，例如 TaskSubmitted、SeatsReserved、InvoiceApproved。LogServe 中 SubmitTaskRequest 是 command，TaskSubmitted 才是 replay 用的事实事件。
```

## Q013. 为什么事件应该尽量表示已经发生的事实？

**回答：**

事件尽量表示已经发生的事实，是为了让 event log 能成为稳定、可重放、可审计的历史记录。事件一旦写入日志，后续 projection、replay、补偿、统计、外部订阅者都会把它当作“系统承认发生过”。如果事件写成命令、意图或模糊状态变更，读者和机器都要猜：它到底发生了吗？成功了吗？为什么发生？

事实事件通常用过去式命名：

```text
TaskSubmitted
TaskStarted
TaskCompleted
WorkflowStarted
StepSucceeded
ActorCommandApplied
InvoiceApproved
UserErasureRequested
```

这些名字的语义很直接：事件写入时，系统已经接受了这个事实。replay reducer 不需要重新问“能不能提交任务”，只需要把 `TaskSubmitted` 应用到状态机。

事实事件有几个工程收益。

**第一，replay 更确定。**

replay 处理事实，不处理意图。`TaskCompleted` 表示任务已经完成，reducer 把状态改成 completed。`CompleteTask` 则表示有人想完成任务；它是否成功，还要看 lease、epoch、当前状态、结果引用是否存在。把 command 当事件保存，replay 就会变成重新跑业务逻辑。

**第二，投影更容易维护。**

一个 read model 只关心自己订阅的事实。例如 dashboard 看到 `StepSucceeded` 就更新成功计数，对象引用视图看到 `ResultReferenceCreated` 就增加可达引用。它不需要理解完整 command handler。

**第三，外部消费者更安全。**

事件可能被多个消费者处理。支付系统、邮件系统、搜索索引、缓存刷新、审计系统都可能订阅同一条事件。事实事件告诉它们“可以基于这个事实行动”；命令事件则会让下游误以为自己也要参与决策。

**第四，审计更有意义。**

审计想知道的是系统实际发生了什么，而不是有人尝试过什么。尝试也可以记录，但那是 command audit。event-sourced log 里的核心事件应该能解释状态如何形成。

**第五，补偿更清楚。**

event sourcing 一般不改历史事件。修正错误靠补偿事件，例如 `ReservationCanceled` 补偿之前的 `SeatsReserved`。如果原事件是 `ReserveSeatsCommandAccepted` 这种半事实半命令，后续补偿语义会变得拧巴。

也要避免另一种极端：只记录“字段变了”。比如：

```text
StatusChanged(from=pending, to=approved)
BalanceChanged(from=100, to=70)
```

这种事件能 replay 当前状态，但业务含义太弱。更好的事件是：

```text
InvoiceApproved(invoiceID, approverID)
PaymentCaptured(paymentID, amount, currency)
```

前者告诉你为什么状态变化，后者只告诉你变化结果。Microsoft 文档里“两个座位被预订”优于“剩余座位变成 42”的例子，说的就是这个边界。

LogServe 的事件命名基本遵循这个方向：`TaskSubmitted`、`TaskStarted`、`StepSucceeded`、`WorkflowCompleted`、`ActorCommandApplied` 都是事实。需要警惕的是，某些事件 payload 如果直接包含完整 request 或脚本源码，就会把命令输入、执行事实和敏感数据混在一起。对于机制验证可以接受；生产化时应把事实、命令元数据和敏感 payload 分层。

面试里可以这样答：

```text
事件表示已经发生的事实，是为了让 event log 成为稳定的历史记录。事实事件可以直接 replay、projection 和审计；命令只是请求，今天重新执行可能因为规则、时间、权限或外部状态变化得到不同结果。

我会用过去式、领域化的事件名，例如 TaskSubmitted、StepSucceeded、SeatsReserved。事件要说明发生了什么以及必要原因，不要只写 StatusChanged，也不要把完整 command 当成事件。LogServe 的控制面事件大体就是事实事件，后续要注意敏感 payload 和 command 元数据的分层。
```

## Q014. event sourcing 如何处理 GDPR 删除请求？

**回答：**

先把边界说清楚：这是工程设计角度的回答，不是法律意见。GDPR 删除请求要结合数据类别、处理目的、合同义务、法定义务、审计保留和当地监管解释判断。工程师能做的是把系统设计成“可以执行合规决策”，而不是事后发现 event log 天生删不动。

GDPR 第 17 条规定了数据主体在特定条件下请求删除个人数据的权利，例如数据不再为原处理目的所必需、数据主体撤回同意且没有其他法律依据、数据被非法处理等。第 5 条还要求个人数据只在处理目的必要范围内保存，并以适当安全方式处理。event sourcing 的难点在于：event log 通常 append-only、不可变、长期保留，这和“删除个人数据”的要求天然有张力。

处理思路不是简单地说“event log 不能删”。真实系统通常会组合几种办法。

**第一，从设计上避免把个人数据写入事件。**

这是最好的办法。事件里只保存稳定标识符和业务事实，个人数据放在可删除的 profile store、PII vault 或 customer data service。

```text
不推荐：
  UserRegistered(userID, email, phone, address, name)

更稳：
  UserRegistered(userID)
  PII store:
    userID -> email, phone, address, name
```

删除请求到来时，删除或匿名化 PII store 中的数据；event log 仍保留 `UserRegistered(userID)` 这类业务事实。只要 `userID` 本身不能再合理关联到自然人，事件流就不再暴露原始个人数据。这里还要注意匿名化和伪匿名化的差别：伪匿名化仍可能通过额外信息重新识别，不能当作彻底删除。

**第二，用删除/擦除事实事件驱动投影清理。**

删除请求本身也可以是事实事件：

```text
UserErasureRequested(userID, requestedAt)
UserPersonalDataErased(userID, erasedAt, scope)
```

这类事件不包含被删除的个人数据，只记录系统做过合规处理。projection 收到后清理 read model、搜索索引、缓存、对象引用、导出副本。这样系统既能解释“为什么这个用户数据不见了”，又不会把被删除的数据重新写回 view。

**第三，对必须进入事件的敏感字段做 envelope encryption。**

有些业务很难完全避免个人数据进入事件，例如签署合同、支付账单、客服工单。可以把敏感字段单独加密，并使用 per-subject 或 per-tenant data encryption key。

```text
Event:
  userID
  eventType
  encryptedPIIBlob
  keyID

Erasure:
  delete key for userID/keyID
```

删除密钥后，事件结构还在，但敏感内容无法恢复，这通常叫 crypto-shredding。Microsoft 的 Event Sourcing 文档也把这种做法列为处理个人数据和不可变 event store 冲突的一种方案。它的代价是密钥管理复杂，读写都要加解密，备份系统也要跟着处理密钥删除。

**第四，保留业务事实，删除可识别属性。**

有些事实出于财务、风控、审计或安全目的必须保留，但不一定要保留可识别个人信息。比如订单金额、库存变动、账务分录可以保留；姓名、电话、地址、邮箱应尽量移出事件或加密。

```text
OrderPlaced(orderID, customerRef, amount, currency)
ShippingAddressCaptured(encryptedAddressBlob, keyID)
```

收到删除请求后，业务事实仍能解释账务和库存，地址 blob 因密钥删除而不可恢复。

**第五，定义 retention 和 legal hold。**

不是所有删除请求都意味着所有记录立即删除。GDPR 第 17 条本身也包含例外，例如为遵守法律义务、公共利益、法律请求等可能需要保留。工程上要有 legal hold、retention policy、删除范围和审计记录。不要让删除流程绕过合规判断直接物理删日志。

**第六，清理派生视图和副本。**

即使 event log 用 crypto-shredding 处理了，materialized view、搜索引擎、缓存、数据仓库、对象存储、备份导出里仍可能有个人数据。删除流程必须覆盖这些派生存储，否则 event store 做得再漂亮也没用。

event sourcing 下的删除流程可以设计成这样：

```text
1. 接收 erasure request，验证主体身份和范围。
2. 写入 UserErasureRequested，不包含敏感 payload。
3. 合规策略判断是否可删除、部分删除或 legal hold。
4. 删除 PII store 中的个人数据。
5. 删除或轮换 per-subject encryption key。
6. 发布 UserPersonalDataErased。
7. 投影清理 read model、搜索索引、缓存、对象存储引用。
8. 记录不可含敏感数据的处理审计。
```

LogServe 当前主要是机制验证，不是用户身份数据平台。但它的 actor 创建和 task payload 可能包含代码、prompt、输入参数、result reference。如果这些 payload 未来可能含个人数据，最好不要直接把明文 PII 写进 logstore。更稳的方式是：事件里保存 result reference 或 PII reference，敏感内容放在可加密、可按 subject 删除的外部 store；event log 只记录引用和事实。

面试里可以这样答：

```text
event sourcing 处理 GDPR 删除请求，不能等到上线后再补。我的原则是：个人数据尽量不进 event log；事件只保存业务事实和稳定引用；PII 放在可删除的 profile/PII store。必须进入事件的敏感字段用 per-subject key 加密，删除请求通过删除 PII 记录或销毁密钥实现不可恢复。

删除本身也要事件化，例如 UserErasureRequested 和 UserPersonalDataErased，但事件里不能再包含被删除的数据。派生 read model、搜索索引、缓存、对象存储和备份导出都要纳入删除流程。对于 LogServe 这种 log-first 原型，生产化时要特别避免把 prompt、actor args、result payload 中的个人数据明文写入不可变日志。
```

## Q015. event log 中包含敏感数据时如何做脱敏或加密？

**回答：**

event log 里一旦包含敏感数据，就要按长期留存数据来设计保护措施。不要只把它当成普通日志。event store 可能被 replay、备份、复制、导出、投影到搜索引擎，也可能被开发者拿去排查问题；敏感字段进入事件后，扩散面很快变大。

GDPR 第 32 条把伪匿名化和加密列为可采用的安全措施之一，第 5 条要求个人数据以适当安全方式处理，防止未授权处理和意外丢失、破坏或损害。工程上落地时，可以分几层处理。

**第一，数据最小化。**

最强的保护是不要写。事件只保存 replay 必需的字段，排查用上下文放到短期日志或受控审计系统里。

```text
不推荐：
  TaskSubmitted(taskID, userEmail, fullPrompt, apiKey, inputFileContent)

更稳：
  TaskSubmitted(taskID, ownerUserID, promptRef, inputObjectRef)
```

如果某个字段不参与 replay，不参与业务事实判断，也不参与合规审计，就不要放进 event log。

**第二，引用替代明文。**

敏感内容放在专门的数据存储里，事件只保存引用：

```text
LLMRequested(taskID, promptRef, model, policy)
ResultProduced(taskID, resultRef, checksum)
```

引用指向的对象可以有独立生命周期、访问控制、加密策略和删除策略。上一章对象存储 result reference 的问题，和这里正好接上：大对象和敏感 payload 不适合直接塞进 event log。

**第三，字段级脱敏。**

有些字段为了排查需要保留部分信息，可以做不可逆脱敏或部分掩码：

```text
email_hash = HMAC(secret, lower(email))
phone_masked = "+1******1234"
ip_prefix = "203.0.113.0/24"
```

注意普通 hash 不够安全。邮箱、手机号这种低熵数据很容易被字典撞出来。需要关联统计时，用带密钥的 HMAC；不需要关联时，直接删除或置空更好。

**第四，字段级加密。**

对 replay 必需但敏感的字段，用 envelope encryption：

```text
payload:
  public:
    taskID
    eventType
  encrypted:
    ciphertext
    keyID
    algorithm
    nonce
```

密钥可以按 subject、tenant、stream 或数据类别分层管理。不要所有事件共用一个长期 master key，否则密钥泄漏或删除需求都会变得不可控。

**第五，crypto-shredding。**

如果删除请求要求让某个主体的数据不可恢复，可以删除该主体的数据密钥。事件还在，replay 仍能看到事件类型和非敏感字段，但敏感 blob 无法解密。

这会影响 replay。reducer 必须能处理“敏感字段已不可用”的情况：

```text
on ShippingAddressCaptured:
  if key missing:
    state.address = erased
    state.personalDataStatus = erased
```

不能让 projection 因为解不开旧敏感字段而整条 stream 无法 replay。

**第六，密钥和权限隔离。**

加密本身不够，谁能解密更重要。至少要分离：

1. event store 读取权限。
2. KMS decrypt 权限。
3. projection 服务身份。
4. 人工排障权限。
5. 数据导出权限。

开发环境和测试环境不应默认拿到生产解密权限。脱敏事件样本要单独生成，不能把生产 event log 直接复制出来。

**第七，投影时继续保护。**

加密 event log 没有自动保护 read model。projection 解密事件后，如果把明文写到 SQL、KV、Elasticsearch 或缓存里，敏感数据仍然泄漏。每个 materialized view 都要重新判断：是否需要这个字段，是否要脱敏，是否有独立 retention。

LogServe 现在的 logstore record 只保存 bytes payload，底层有 CRC32 做完整性校验，但没有字段级加密或敏感字段分类。对于机制验证没问题；如果用于真实用户 prompt、代码、文件内容或业务结果，就应该把明文 payload 改成 reference 或 encrypted blob。尤其是 LLM 场景，prompt 和输出常常混有个人数据、密钥片段或业务机密，不能把它们当普通调度事件处理。

面试里可以这样答：

```text
event log 里有敏感数据时，我会先做数据最小化：能不写就不写，能写引用就不写明文。必须保留的敏感字段用字段级 envelope encryption，并用 per-subject、per-tenant 或 per-stream key 管理；删除需求可以通过删除数据密钥做 crypto-shredding。

脱敏要区分用途。排查需要关联时用 HMAC，不要用普通 hash；展示需要部分信息时用 mask；replay 必需字段要让 reducer 能处理 key 已删除的情况。还要保护 materialized view，因为 projection 解密后写入 SQL、KV 或搜索引擎，可能重新制造一份明文副本。
```

## Q016. materialized view 可以使用内存、SQL、KV 还是搜索引擎？

**回答：**

materialized view 可以用内存、SQL、KV、文档数据库、搜索引擎、OLAP 表，甚至本地文件。它不是一种固定存储产品，而是一种派生读模型。选什么存储，要看查询形状、延迟、数据量、一致性要求、重建成本和运维能力。

Microsoft 的 Materialized View 模式说得很清楚：视图可以桥接不同数据存储，利用各自能力。例如写入侧使用高效写入的云存储作为参考数据源，读取侧用关系数据库保存 materialized views，以获得更好的查询性能。它还提到 materialized view 可以为安全或隐私只暴露源数据的子集，也可以在离线场景下做本地缓存。

几种选择的边界大概是这样。

**内存 view。**

适合低延迟、数据量较小、可快速重建的控制面状态。例如 worker 心跳、调度候选集、最近统计、短期缓存。

优点是快，代码简单。缺点是进程重启会丢，需要从 event log 或 snapshot 恢复；多实例之间还要考虑一致性。

LogServe 的 `MemoryStore` 就属于这种风格。它适合机制验证和单节点控制面：task、workflow、actor、worker、model 状态都能快速查。代价是它不是 durable source of truth，恢复要靠 shared log replay。

**SQL view。**

适合需要事务、复杂过滤、分页、JOIN、唯一约束和人工排查的读模型。例如任务列表、workflow 查询、账务摘要、管理后台。

SQL 的优势是查询能力强，约束和事务成熟，projection checkpoint 可以和 view 更新放在同一个事务里。缺点是高写入吞吐和复杂 JSON 搜索可能需要额外设计。

**KV view。**

适合按 key 查当前状态，或者做轻量索引。例如：

```text
task:{id} -> task status
workflow:{id} -> workflow state
lease:{taskID} -> lease info
model:{name}:{version} -> model metadata
```

KV 的读写延迟低，扩展性好。缺点是 ad-hoc 查询弱，二级索引要自己维护；如果要按状态分页列任务，就要额外维护 `tasks_by_status:{status}` 这样的集合或索引。

**搜索引擎 view。**

适合全文搜索、多字段过滤、相关性排序、日志检索、运维查询。例如按错误消息搜索 task，按 prompt 摘要查 LLM 请求，按 operator action 做审计检索。

搜索引擎的优势是检索强。缺点是最终一致性更明显，更新幂等、删除传播、mapping 变化和敏感数据保护要认真做。不要把搜索索引当 source of truth。

**OLAP/报表 view。**

适合按时间窗口聚合、p99、吞吐、失败率、成本分析。这类 view 可以接受分钟级延迟，但要求批处理、列式存储和聚合性能。

选择时可以按问题倒推：

```text
点查当前状态 -> KV / SQL / 内存
分页管理后台 -> SQL
全文检索 -> 搜索引擎
高频调度状态 -> 内存 + checkpoint
离线报表 -> OLAP
可重建但要跨重启保留 -> SQL/KV
强事务更新 projection progress -> SQL
```

同一个系统可以有多个 materialized view。比如 event log 是 source of truth，同时维护：

1. SQL task table 给 API 分页查询。
2. Redis/KV 当前 lease table 给调度器快速判断。
3. Elasticsearch task index 给运维搜索。
4. ClickHouse/BigQuery 聚合表给报表。
5. 内存 view 给 hot path。

重点是每个 view 都要声明来源、更新方式、滞后指标、重建方式和删除策略。没有这些，view 越多，一致性问题越难排。

面试里可以这样答：

```text
materialized view 可以用内存、SQL、KV、搜索引擎或 OLAP，取决于查询形状。内存适合低延迟控制面状态，SQL 适合分页、过滤和事务，KV 适合按 key 查当前状态，搜索引擎适合全文检索，OLAP 适合报表聚合。

它们都只是派生读模型，不应该自称 source of truth。每个 view 都要有 projection progress、lag 指标、重建流程、幂等更新和删除策略。LogServe 当前 MemoryStore 适合单节点机制验证；生产化可以把 task/workflow view 放 SQL，把调度热状态放内存或 KV，把运维检索放搜索引擎。
```

## Q017. 读模型更新失败时如何恢复？

**回答：**

读模型更新失败，不应该让已经写入 event log 的事实丢失。event sourcing 的基本恢复思路是：event log 是事实来源，read model 是投影；投影失败后，要能从失败位置继续处理，或者丢弃 view 后重建。

失败大致分几类。

**第一，事件已写入，read model 没更新。**

这是最常见、也最好处理的情况。只要 event log 写成功，projection 可以重试。系统需要记录 projection progress，重启后从 `last_processed_seq + 1` 继续。

```text
event log:
  TaskCompleted seq=10 已写入

projection progress:
  last_processed_seq = 9

恢复:
  从 seq=10 继续处理
```

**第二，read model 更新了，checkpoint 没推进。**

这种情况重启后会重复处理同一事件。解决办法是让 reducer 幂等，或者用 `applied_events` 表记录已处理事件。

```text
unique(projection_name, stream_id, seq)
```

如果同一事件再次到来，跳过即可。对于计数器类投影，幂等尤其重要，否则 completed_count 会被加两次。

**第三，checkpoint 推进了，read model 没更新。**

这是最危险的顺序。系统会以为事件已经处理，但 view 实际落后。应通过事务边界避免：view 更新和 checkpoint 推进必须同事务提交。如果做不到，就要靠 replay diff 或业务不变量检测出来，然后把 checkpoint 回退或重建 view。

**第四，event payload 无法解析。**

schema 变化、坏数据、旧版本 bug 都会导致 projection 卡住。不要静默跳过。可以把事件送到 quarantine/dead-letter，并暴露报警：

```text
projection_apply_errors_total
projection_blocked_stream
projection_blocked_seq
projection_error_reason
```

对于核心 read model，遇到无法解析事件通常应该停住而不是继续。继续处理会让 view 和 log 分叉。

**第五，read model 存储不可用。**

SQL、KV、搜索引擎都可能短暂不可用。projection worker 应该使用带 jitter 的重试，并避免无限制堆积内存。事件还在 log 里，worker 可以稍后追赶。对外 API 要暴露 view lag 或 degraded 状态。

恢复策略通常是三档。

```text
快速恢复:
  从 checkpoint 继续增量处理。

局部修复:
  重放某个 stream / partition / 时间窗口。

全量重建:
  丢弃 view，从 event log 或 snapshot+tail 重建。
```

要让这三档可操作，系统需要：

1. projection progress。
2. 幂等更新。
3. 可重复 replay 的 reducer。
4. dead-letter/quarantine。
5. lag 和错误指标。
6. 管理命令：暂停、恢复、回退 checkpoint、重建某个 projection。

LogServe 当前控制面多数是 log-first 后同步更新 metadata。如果 `appendLog` 成功但 `meta.CreateTask` 或 `meta.UpdateWorkflow` 失败，恢复路径应依靠 `BootstrapFromLog` 从日志重建 metadata。当前 `BootstrapFromLog` 是全量读取 stream 后 replay，这对小规模机制验证直接有效；增量化后可以记录每个 projection 的 last seq，失败后从断点追。

面试里可以这样答：

```text
读模型更新失败时，先看 event log 是否已经写成功。如果日志成功，read model 可以从 checkpoint 重试；如果 view 已更新但 checkpoint 没推进，幂等 reducer 或 applied_events 表要能跳过重复事件；最危险的是 checkpoint 先推进但 view 没更新，所以 view 更新和 checkpoint 更新最好同事务提交。

恢复手段包括从 last_processed_seq 增量追、按 stream 局部 replay、丢弃 view 全量 rebuild。LogServe 的 log-first 设计让 metadata 写失败后还能靠 BootstrapFromLog 恢复，这是 event log 作为事实来源的直接收益。
```

## Q018. 双写 event log 和 read model 的一致性如何保证？

**回答：**

双写 event log 和 read model 的一致性，核心不是“两个地方都写一下”，而是要定义哪个写入是权威事实，以及另一个写入失败时怎么修复。没有这个定义，双写很容易产生四种半失败状态。

```text
1. event log 成功，read model 成功：正常。
2. event log 成功，read model 失败：可以重放修复。
3. event log 失败，read model 成功：危险，出现没有事实来源的状态。
4. 两者都失败：请求失败，通常可重试。
```

在 log-first event sourcing 设计里，正确方向通常是：

```text
append event log -> commit authoritative fact -> update read model/projection
```

只要 event log 写成功，read model 可以同步更新，也可以异步更新。同步更新让读后可见性更好，但请求延迟更高，失败处理也复杂。异步 projection 更符合 CQRS，但会带来 view lag。无论哪种方式，read model 都应能从 event log 重建。

不要反过来：

```text
update read model -> append event log
```

如果 read model 成功而 event log 失败，系统会暴露一个无法 replay 的状态。重启后这个状态消失，或者更糟，下游已经基于它执行了动作。

一致性手段可以按强度排列。

**第一，同库事务。**

如果 event log 和 read model 在同一个数据库里，可以把 append event、更新 view、推进 checkpoint 放在同一个事务里。这最简单，但会牺牲 event store 和 read model 的独立扩展能力。

**第二，event log 为准，read model 可重建。**

这是 event sourcing 最常见的模式。command handler 只承诺事件落盘；read model 由投影处理。读接口接受 eventual consistency，或者在响应里返回 event seq，让客户端可以等 view 追到该 seq。

```text
SubmitTask -> returns {taskID, streamID, seq}
GetTaskStatus(minSeq=seq) -> 如果 view 未追到 seq，返回 rebuilding/pending
```

**第三，outbox。**

如果本地业务数据库是事实来源，还要向 broker 发事件，就用 outbox。业务变更和 outbox message 同事务写入本地数据库，再由 relay 发布。它解决的是“数据库提交了但消息没发”或“消息发了但数据库回滚”的一致性问题。

**第四，inbox/idempotent consumer。**

消费者更新 read model 时，要记录已处理 message id 或 stream seq。因为 outbox relay、broker、网络重试都可能让消息重复到达。消费者幂等才能保证 read model 不因重复事件漂移。

**第五，reconciler。**

即使前面都做了，仍要有后台对账：按 stream 重放 event log，比较 read model；发现差异后修复或报警。分布式系统不要只靠“理论上不会发生”。

LogServe 当前的控制面是 log-first：`enqueueTaskWithMetadata` 先 append `TaskSubmitted`，再 `meta.CreateTask`；workflow step 状态推进也是先 append `StepScheduled`、`StepStarted`、`StepSucceeded` 等事件，再更新 metadata。这保证了 metadata 失败时还有日志可恢复。它的边界是：metadata 更新和 append log 不在一个分布式事务里，所以在线读可能看到短暂落后；生产化时要补 projection progress、重试队列或 reconciler。

面试里可以这样答：

```text
双写一致性的关键是先确定 source of truth。log-first 系统里 event log 是权威事实，流程应是 append event 成功后再更新 read model；read model 失败可以通过 replay 修复。最危险的是 read model 成功但 event log 失败，因为那会产生无法重建的状态。

如果 event log 和 read model 在同库，可以用同事务；如果跨存储，就接受 eventual consistency，并用 checkpoint、幂等 consumer、outbox/inbox 和 reconciler 保证最终追平。LogServe 现在先写共享日志再更新 metadata，方向是对的；生产化要补更完整的投影进度和对账机制。
```

## Q019. outbox pattern 解决什么问题？

**回答：**

outbox pattern 解决的是“本地状态更新”和“对外发送消息/事件”之间的原子性问题。它常见于微服务：服务更新自己的数据库后，还要发事件给 broker，让其他服务更新 read model、触发 saga 或执行外部动作。问题是数据库和消息 broker 通常不在同一个事务里。

没有 outbox 时，代码经常长这样：

```text
BEGIN
  update orders set status='paid'
COMMIT

publish OrderPaid to broker
```

这里有两个经典故障。

```text
数据库提交成功，publish 失败：
  订单已经 paid，但下游不知道。

publish 成功，数据库回滚：
  下游看到 OrderPaid，但订单其实没 paid。
```

如果强行用两阶段提交 2PC，需要数据库和 broker 都支持，还会把服务和基础设施耦合得很重。Microservices.io 的 Transactional Outbox 模式给出的方案是：发送方先把 message 作为同一个数据库事务的一部分写入 outbox 表；单独的 relay 进程再从 outbox 读取并发布到 broker。这样数据库提交和“待发送消息”提交绑定在一起。

典型结构是：

```text
business table:
  orders(id, status, ...)

outbox table:
  id
  aggregate_id
  aggregate_type
  event_type
  payload
  created_at
  published_at
  ordering_key
```

写入流程：

```text
BEGIN
  update orders set status='paid'
  insert into outbox(event_type='OrderPaid', payload=...)
COMMIT

relay:
  select unpublished messages
  publish to broker
  mark published
```

outbox 的收益是：

1. 数据库事务提交，消息一定会被 relay 看到。
2. 数据库事务回滚，outbox message 也不会存在。
3. 不需要数据库和 broker 之间做 2PC。
4. 可以按 aggregate/order key 保持发送顺序。
5. relay 失败后可以继续重试。

它的代价也要讲清楚。relay 可能重复发布消息。Microservices.io 也明确提醒：relay 可能在发布后、记录已发布前崩溃，重启后再次发布。因此消费者必须幂等。outbox 解决了“至少会发出去”，不直接提供全链路 exactly-once。

outbox 和 event sourcing 的关系有两种。

一种是传统 CRUD 系统使用 outbox。业务数据库是 source of truth，outbox 只是把数据库变更可靠地发布出去。

另一种是 event-sourced 系统使用 outbox 发布 integration event。event store 是 source of truth，projection 或 relay 把内部领域事件转换成外部消息，写入 outbox 后发送给 broker。这样可以避免把内部 event store 直接暴露给所有服务。

LogServe 当前更接近 event log 作为本地事实来源。它自己写共享日志，再更新 metadata；如果未来要把 `TaskCompleted`、`WorkflowCompleted`、`LLMCompleted` 发布到 Kafka、NATS 或外部系统，就需要 outbox 或等价 relay。否则就会出现“LogServe 日志里任务完成了，但外部 broker 没收到事件”的问题。

面试里可以这样答：

```text
outbox pattern 解决本地数据库更新和消息发布之间的原子性问题。服务在同一个本地事务里更新业务表并写 outbox 表；事务提交后，relay 异步把 outbox 消息发到 broker。这样数据库提交了，消息不会丢；数据库回滚，消息也不会发。

它避免了 2PC，但只保证至少一次发布。relay 可能重复发送，所以消费者仍要幂等。对 LogServe 来说，如果未来要把共享日志里的任务/工作流事件发布到外部 broker，就需要 outbox 或等价的 log relay，避免日志已提交但外部事件丢失。
```

## Q020. inbox pattern 解决什么问题？

**回答：**

inbox pattern 解决的是消费者侧的“消息重复、处理一半崩溃、外部副作用重复执行”问题。它和 outbox 是一对：outbox 让生产者可靠发送，inbox 让消费者可靠接收和幂等处理。

消息系统通常给的是 at-least-once delivery。生产者、broker、relay、网络、消费者 ack 都可能失败，导致同一条消息重复投递。消费者如果直接处理，就可能重复扣款、重复发邮件、重复创建任务、重复增加计数。

inbox 的基本做法是：消费者收到消息后，先把 message id 或 event id 记录到自己的 inbox 表，再执行业务处理。处理成功后标记完成。下次同一消息再来，消费者查到已经处理过，就跳过或返回相同结果。

典型结构是：

```text
inbox table:
  message_id
  consumer_name
  received_at
  processed_at
  status
  payload_hash
  error
```

处理流程：

```text
receive message M

BEGIN
  if inbox(message_id=M.id, consumer=me) status=processed:
    COMMIT
    ack
    return

  insert inbox row if absent
  apply business update / update read model
  mark inbox row processed
COMMIT

ack broker message
```

关键是 inbox 记录、业务更新、read model 更新最好在同一个本地事务里完成。如果消费者先 ack broker，再写数据库，崩溃后消息丢了；如果先写数据库但没记录 inbox，重试时又会重复处理。

MassTransit 的 transactional outbox 文档把 Consumer Outbox 描述成 inbox + outbox 的组合：inbox 用来跟踪收到的消息以保证消费者行为，outbox 用来暂存消费者处理过程中要发布/发送的消息，消费者成功完成后再把这些消息发到 broker。这个组合很常见，因为消费者处理一条消息时，往往也会更新本地状态并发布新消息。

inbox 主要解决这几类问题。

**第一，重复投递。**

同一 `message_id` 多次到达，只处理一次。对 projection 来说，可以用 `(projection_name, stream_id, seq)` 作为 inbox key。

**第二，消费者崩溃恢复。**

如果消费者处理到一半崩溃，inbox 里会留下 status。重启后可以判断是重试、跳过还是人工修复。

**第三，幂等副作用。**

发邮件、调用支付、创建外部工单这类副作用不能简单依赖“我觉得 broker 不会重复”。inbox 至少能防止同一消息触发多次本地处理；外部调用还要配合对方的 idempotency key。

**第四，read model 防漂移。**

projection consumer 可以把每条事件的处理记录放进 inbox/applied_events。重复事件不会让计数器多加一次，也不会让搜索索引重复写脏数据。

inbox 和 outbox 的边界可以这样记：

```text
outbox:
  我已经更新了本地状态，如何保证该发出去的消息最终发出去？

inbox:
  我收到了可能重复的消息，如何保证本地只产生一次效果？
```

两者一起用时，消费者侧流程是：

```text
BEGIN
  insert inbox received marker
  update local state / materialized view
  insert outbox messages produced by this handling
  mark inbox processed
COMMIT

relay publishes outbox messages
ack original broker message
```

LogServe 的 metadata projection 如果未来异步化，也可以用 inbox 思路：每个 projection 记录已经处理过的 `(streamID, seq)`。`TaskCompleted` 事件重复到达时，metadata 不会重复更新统计；`LLMCompleted` 重复到达时，EWMA 或计数也不会被重复计算。当前 logstore 的 idempotency key 防的是重复 append，同步 metadata 更新防的是一部分重复 command；异步 consumer 场景还需要 inbox/applied_events 来防重复消费。

面试里可以这样答：

```text
inbox pattern 解决消费者侧的重复消息和半失败问题。消费者先把 message id/event seq 记录到 inbox，再在同一个事务里更新本地状态或 read model，并标记已处理。重复投递时查到 inbox 记录就跳过。

outbox 管生产者可靠发送，inbox 管消费者幂等接收。两者组合后，可以在不使用 2PC 的情况下做到至少一次发送、幂等消费和可恢复处理。对 LogServe 的异步 projection 来说，inbox 可以表现为 applied_events 表，用 `(projection, streamID, seq)` 防止重复事件让 metadata 或统计视图漂移。
```

## Q021. saga 和 event sourcing 的关系是什么？

**回答：**

saga 和 event sourcing 经常一起出现，但它们解决的是两类问题。saga 关心一个跨服务、跨数据源、跨步骤的业务流程如何在没有分布式事务的情况下走到可接受的最终状态；event sourcing 关心一个对象或聚合的状态如何通过事件序列持久化、重放和投影。

Microservices.io 对 saga 的定义很直接：把一个跨多个服务的业务事务实现为一串 local transactions；每个 local transaction 更新自己的数据库，并发布消息或事件触发下一个 local transaction；如果某一步因为业务规则失败，saga 执行补偿事务，撤销前面已经完成的步骤。AWS Prescriptive Guidance 也把 saga 描述成一段业务工作流，在平台级失败时可以 forward recovery，在应用级失败时通过 compensating transaction 做 backward recovery。

这说明 saga 的重点是“流程一致性”，不是“状态怎么存”。它可以用事件，也可以不用 event sourcing。

常见组合有四种。

**第一，saga choreography + 普通数据库。**

每个服务更新自己的数据库，然后发布 domain event。其他服务订阅事件后执行下一步。

```text
OrderCreated -> InventoryReserved -> PaymentCaptured -> OrderApproved
```

这里用了事件协作，但不一定是 event sourcing。每个服务的 source of truth 可能仍然是普通 SQL 当前表。事件只是服务间协作消息。

**第二，saga orchestration + 普通数据库。**

有一个 orchestrator 保存 saga 状态，给各个 participant 发 command，并等待 reply。

```text
CreateOrderSaga:
  send ReserveInventory
  wait InventoryReserved
  send CapturePayment
  wait PaymentCaptured
  send ApproveOrder
```

这种模式更容易看清流程，适合参与者多、依赖复杂的场景。它同样不要求 event sourcing。

**第三，event-sourced aggregate 参与 saga。**

每个服务内部用 event sourcing 维护自己的聚合状态。例如 Order Service 用 `OrderCreated`、`OrderApproved` 事件维护订单，Payment Service 用 `PaymentAuthorized`、`PaymentCaptured` 事件维护支付。saga 只是协调这些服务。

这时 event sourcing 给 saga 提供可靠的本地状态记录和事件发布基础。服务崩溃后可以 replay 本地事件恢复订单或支付状态，saga 不必依赖易丢的内存状态。

**第四，saga 本身也 event-sourced。**

saga orchestrator 的状态也可以用事件记录：

```text
SagaStarted
InventoryReservationRequested
InventoryReserved
PaymentCaptureRequested
PaymentFailed
InventoryReleaseRequested
SagaCompensated
```

这样做的好处是 saga 崩溃后可以从事件重建自己执行到哪一步、哪些补偿已经发出、哪些 reply 已收到。复杂长事务里这很有价值。

但要注意，event sourcing 不是 saga 的替代品。event sourcing 可以让状态可重放，不能自动解决跨服务业务一致性。比如你已经记录了 `InventoryReserved` 和 `PaymentCaptureFailed`，系统仍然需要业务逻辑决定：释放库存、换支付方式、人工处理，还是继续重试。这个决策属于 saga。

反过来，saga 也不是 event sourcing。saga 可以只用普通表保存当前状态：

```text
saga_instance(id, state, current_step, retry_count)
```

只要它能可靠记录进度、重试和补偿，照样是 saga。

在 LogServe 里，workflow engine 和 saga 有相似之处：一个 workflow 由多个 step 组成，控制面调度 step，记录 `StepScheduled`、`StepStarted`、`StepSucceeded`、`StepFailed`、`WorkflowCompleted` 等事件。如果某个 step 失败，系统可以决定重试、失败整个 workflow，或者未来扩展成补偿步骤。它更像单系统内部的 orchestrated workflow，不是严格意义上跨多个业务服务的分布式 saga。这个边界要讲清楚。

面试里可以这样答：

```text
saga 解决跨服务业务流程的一致性问题，event sourcing 解决状态持久化和重放问题。saga 可以用 event sourcing 实现，也可以只用普通数据库；event sourcing 可以记录 saga 的每一步，但不会自动给出补偿策略。

两者组合时，event store 记录订单、支付、库存或 saga orchestrator 的状态变化；saga 根据这些事实决定下一步 command 或补偿 command。LogServe 的 workflow 和 saga 有相似的编排结构，但当前更多是单系统内部的可恢复 workflow，不应夸大成完整跨服务 saga 平台。
```

## Q022. eventual consistency 会给 API 用户带来什么体验问题？

**回答：**

eventual consistency 对后端来说是“写入事实已经成功，read model 稍后追上”；对 API 用户来说，体验通常更直接：我刚刚做的事，系统好像没承认。

在 CQRS、event sourcing、materialized view 架构里，写路径和读路径分开。写 API 可能已经把事件 append 到 event log，但读 API 读的是落后一小段的 materialized view。Microsoft 的 Event Sourcing 文档提醒，创建 materialized view 或 projection 时系统只有最终一致性，应用处理请求、事件进入 event store、事件发布、消费者处理之间存在延迟。这个延迟如果不暴露给用户，就会变成 API 体验问题。

常见现象有这些。

**第一，创建后马上查询 404。**

用户调用：

```text
POST /tasks -> 201 Created
GET /tasks/{id} -> 404 Not Found
```

后端并没有丢数据，只是 read model 还没投影到这条 task。对用户来说，这就是最糟糕的“刚创建就不存在”。

**第二，状态读回旧值。**

用户调用完成任务：

```text
POST /tasks/{id}/complete -> 200 OK
GET /tasks/{id} -> status=running
```

几秒后再查才变成 completed。这会让客户端误以为 complete 请求失败，可能触发重复提交。

**第三，列表和详情不一致。**

详情页显示 completed，列表页仍把任务放在 running 分类；dashboard 的总数和过滤列表对不上；分页时第一页有新对象，第二页又没有。这通常是多个 read model 或索引投影进度不同导致的。

**第四，状态倒退。**

如果用户请求被负载均衡到不同副本或不同区域，第一次读到 completed，第二次读到 running。Azure Cosmos DB 对 eventual consistency 的说明里也提到，eventual consistency 下客户端可能读到比过去读过的值更旧的值。这个体验比“只是慢一点”更难接受，因为用户看到了时间倒流。

**第五，异步流程结果不明确。**

saga 或 workflow 接口经常是：

```text
POST /orders -> 202 Accepted
```

这只表示流程已开始，不表示订单最终 approved。用户如果不知道要轮询哪里、等多久、什么状态算失败，就会把 pending 当成故障。

**第六，错误处理变复杂。**

客户端收到超时，不知道 command 是否已经写入 event log；收到 409，不知道是并发冲突还是 view 旧；收到 404，不知道是真不存在还是 projection 未追上。API 语义不清时，客户端会用重试和轮询硬顶，最后制造更多负载。

**第七，权限和可见性延迟。**

用户刚被授权，read model 还没更新，查询仍然 403；用户刚被撤权，某些 view 还允许访问。这类问题涉及安全，不能只当“最终一致就好”。

所以 eventual consistency 要在 API 设计里显式出现，而不是藏在实现里。常见做法包括：

1. 写 API 返回 operation id、stream id、event seq 或 projection token。
2. 读 API 返回 `projection_seq`、`projection_lag_ms`、`stale` 标志。
3. 对异步流程使用 `202 Accepted + Location`，让客户端查 operation 状态。
4. 对创建后未投影的对象返回 `202/409 pending`，避免误导成永久 404。
5. 对需要 read-your-writes 的场景提供 `min_seq` 或 session token。
6. 在 UI 中把 pending、accepted、processing、completed 区分开。

LogServe 当前控制面多数写路径 append log 后同步更新 metadata，所以单节点场景下用户不一定明显感到 lag。但一旦 metadata 更新异步化，或者后续引入 SQL/KV/search 多个 read model，就会遇到上面的问题。比如 `SubmitTask` 成功后，`GetTask` 如果读的是未追平的 metadata，就可能看不到任务；`ListWorkflow` 和 `GetWorkflow` 也可能短暂不一致。

面试里可以这样答：

```text
eventual consistency 给 API 用户的主要问题是写后马上读不到、读到旧状态、列表和详情不一致、不同副本之间状态倒退，以及异步流程结果不明确。后端知道事件已经落盘，用户看到的是系统像没处理请求。

API 设计要把这种语义暴露出来。写接口返回 operation id、stream seq 或 projection token；读接口返回 view 的 checkpoint 和 lag；异步流程用 202 Accepted + Location；需要 read-your-writes 的接口支持 min_seq/session token 或等待 projection 追平。
```

## Q023. 如何向用户暴露 read-your-writes 语义？

**回答：**

read-your-writes 的含义是：同一个用户或同一个会话刚写入的数据，后续读取至少能看到这次写入，不会读到更旧的状态。它不是全局强一致；它是面向会话的最低可见性保证。

Azure Cosmos DB 的 session consistency 是一个很好的参考。它在单个 client session 内保证 read-your-writes 和 write-follows-reads。写操作后，服务端返回 session token；客户端缓存 token，并在后续读请求里带上。服务端只有在副本至少包含 token 对应版本时才返回数据，否则会换副本或重试。这个思路可以迁移到 event-sourced read model：写 API 返回一个“最低可见版本”，读 API 用它作为屏障。

在 event sourcing/CQRS 系统里，最常见的做法是把写入位置返回给客户端。

```http
POST /tasks
HTTP/1.1 202 Accepted
Location: /operations/op-123

{
  "task_id": "task-1",
  "stream_id": "task:task-1",
  "event_seq": 42,
  "read_token": "task:task-1:42"
}
```

客户端后续读取时带上这个 token：

```http
GET /tasks/task-1?min_seq=42
X-Read-Your-Writes: task:task-1:42
```

服务端有几种处理方式。

**第一，等待 read model 追平。**

如果当前 materialized view 的 checkpoint 已经到 42，就直接返回。如果还在 39，可以短暂等待：

```text
if projection_seq >= min_seq:
  return current view
else wait up to 200ms
if still behind:
  return 202 or 503 with Retry-After
```

这种方式体验好，但要设置超时，避免读请求被大量阻塞。

**第二，读 source of truth。**

如果 read model 未追平，可以直接从 event log 读该 stream，临时 replay 到 `min_seq`，返回最新状态。这适合单对象详情页，不适合复杂列表查询。

```text
GET /tasks/{id}:
  view behind -> replay task:{id} from event log -> return reconstructed state
```

**第三，读主副本或写入区域。**

如果系统是多副本/多区域，客户端后续读被路由到写入所在区域或主 read model，减少状态倒退。这个做法不能代替 token，但可以降低 lag。

**第四，返回明确的 pending 语义。**

如果读模型追不上，不要返回假 404。更好的响应是：

```http
HTTP/1.1 202 Accepted
Retry-After: 1

{
  "status": "projection_pending",
  "required_seq": 42,
  "current_seq": 39
}
```

这样客户端知道不是对象不存在，而是 view 还没追上。

**第五，把 token 放进 operation resource。**

异步 saga/workflow 可以返回 operation id。客户端轮询 operation：

```http
GET /operations/op-123
{
  "state": "accepted",
  "write_seq": 42,
  "projection_seq": 39,
  "result_location": "/tasks/task-1"
}
```

当 `projection_seq >= write_seq` 后，再提示客户端读取 result。

read-your-writes 设计要避免几个坑。

1. token 不能是客户端自己编的，必须由服务端签发或可验证。
2. token 要带 scope，例如 stream、partition、tenant，避免把一个 partition 的版本误用于另一个 partition。
3. token 是最低版本屏障，不是“读取历史版本”的请求。Cosmos DB 文档也强调 session token 是 minimum version barrier。
4. 列表查询比详情查询难。列表要保证包含新对象，必须知道列表 projection 的 checkpoint，而不是对象 stream 的 checkpoint。
5. 超时后要返回明确状态，不要无限等待。

LogServe 可以自然暴露这类语义。logstore `AppendLogResponse` 已经有 `Seq`，控制面写入 `TaskSubmitted`、`WorkflowStarted`、`StepSucceeded` 时可以把 `stream_id` 和 `seq` 包装成 read token。metadata store 如果记录每个 projection 的 last seq，`GetTask` 或 `GetWorkflow` 就能判断自己是否追到用户刚写入的事件。

面试里可以这样答：

```text
我会让写 API 返回 read token，例如 stream_id + event_seq。后续读请求带 min_seq，服务端检查 materialized view 的 checkpoint 是否已经达到这个 seq；达到就返回，没达到就短暂等待、从 event log 临时 replay 单对象，或者返回 202 projection_pending 和 Retry-After。

这个 token 类似 Cosmos DB session token，是最低可见版本屏障，不是全局强一致。LogServe 可以利用 AppendLogResponse 的 Seq 和 stream_id 做这件事，但还需要 metadata projection 记录 checkpoint，读 API 才知道 view 是否追平。
```

## Q024. 幂等事件应用如何设计？

**回答：**

幂等事件应用的目标是：同一条事件处理一次和处理多次，read model 的最终状态一样。这个性质非常重要，因为事件投递通常是 at-least-once。Microsoft 的 Event Sourcing 文档也明确提醒，消费者可能多次收到同一事件；如果没有幂等，projection 会 drift，支付、通知这类副作用也可能执行多次。

设计幂等应用，第一步是给每条事件一个稳定身份。

```text
event_id: 全局唯一
stream_id + seq: 每个 stream 内唯一
message_id: broker 层唯一
idempotency_key: command 或业务操作唯一
```

对 event-sourced projection 来说，`stream_id + seq` 通常最可靠。LogServe 的 log record 已经有 per-stream `Seq`，这可以作为 projection 去重和 checkpoint 的基础。

第二步是记录已处理位置或已处理事件。

最简单的是 checkpoint：

```text
projection_progress(
  projection_name,
  stream_id,
  last_processed_seq
)
```

如果事件必须按 stream 顺序处理，那么 `seq <= last_processed_seq` 就是重复事件，直接跳过；`seq == last_processed_seq + 1` 才能处理；`seq > last_processed_seq + 1` 说明中间有缺口，不能继续。

更严格的是 applied events 表：

```text
projection_applied_events(
  projection_name,
  stream_id,
  seq,
  event_id,
  applied_at
)
unique(projection_name, stream_id, seq)
```

这适合多 partition、多 worker、可能乱序到达的投影。

第三步是把 view 更新和去重记录放在同一个事务里。

```text
BEGIN
  insert applied_events(projection, stream, seq)
  update read_model
  update projection_progress
COMMIT
```

如果没有同一事务，就可能出现 view 更新了但 applied_events 没写，或者 applied_events 写了但 view 没更新。前者会导致重复处理，后者会导致事件被跳过。后者更危险。

第四步是让状态更新本身尽量可重复。

这类写法比较安全：

```text
on TaskCompleted:
  set task.status = "completed"
  set task.completed_at = event.completed_at
```

这类写法要小心：

```text
on TaskCompleted:
  completed_count += 1
```

如果必须更新计数器，可以先用唯一事件表防重复，或者按事实重算：

```text
completed_count = count(distinct task_id where terminal_status=completed)
```

第五步是检查 payload 是否一致。

同一个 event id 或 idempotency key 再次出现时，payload 应该一样。如果同一个 key 绑定了不同 payload，不能悄悄当成重复事件跳过。LogServe 的 `ensureIdempotencyFingerprint` 就是在 command/idempotency 层做这件事：同一个 idempotency key 只能对应同一份请求指纹。

第六步是外部副作用单独幂等。

发送邮件、扣款、调用 webhook 不能只靠 projection 幂等。要给外部系统传 idempotency key，或者在本地 outbox/inbox 里记录 `notification_sent(event_id)`。

面试里可以这样答：

```text
幂等事件应用的核心是给事件稳定身份，并把“是否处理过”和“read model 更新”放在同一个原子边界里。常用 key 是 event_id 或 stream_id + seq。处理时检查 checkpoint/applied_events，重复事件直接跳过，缺 seq 停住报警。

状态更新也要尽量写成 set/upsert，而不是盲目 +=。如果必须做计数器，要用唯一事件记录防重复。LogServe 已经有 per-stream Seq 和 idempotency key；如果 projection 异步化，还需要 projection_applied_events 或 projection_progress 来保证事件重复到达时 metadata 不漂移。
```

## Q025. 重复事件和乱序事件如何处理？

**回答：**

重复事件和乱序事件要分开处理。重复事件是“已经处理过的事件又来了”；乱序事件是“未来事件先来了，中间事件还没到”。重复可以跳过，乱序通常不能直接应用。

处理重复事件，靠事件身份和幂等记录：

```text
if event.seq <= last_processed_seq:
  skip

if applied_events contains (stream_id, seq):
  skip
```

如果 payload hash 不一致，要报警。相同 `(stream_id, seq)` 不应该对应两份不同 payload。

处理乱序事件，先看系统承诺的顺序范围。大多数 event-sourced 系统只要求同一个 stream/aggregate 内有序，不保证全局有序。Microsoft 文档也强调影响实体当前状态的事件顺序很关键，常见做法是给事件标注递增标识符。

对单 stream projection，可以严格要求：

```text
expected = last_processed_seq + 1

if event.seq == expected:
  apply
elif event.seq <= last_processed_seq:
  duplicate, skip
else:
  gap detected, buffer or stop
```

乱序时有几种策略。

**第一，等待缺失事件。**

如果 `seq=12` 先到，而 checkpoint 是 10，就等待 `seq=11`。可以短暂 buffer `seq=12`，但要有内存上限和超时。

**第二，从 event store 补读。**

如果 broker 投递乱序，但 event store 支持按 stream 读取，可以从 `last_seq + 1` 直接读 authoritative stream。这样 projection 不依赖 broker 顺序。

**第三，停住该 stream。**

对强状态机，例如任务、订单、actor command，乱序直接应用会破坏状态。宁愿停住该 stream 并报警，也不要猜。

**第四，业务上可交换时允许乱序。**

少数场景事件本身可交换，例如集合并集、最大值更新、某些 CRDT 语义。此时可以乱序应用，但这是特殊设计，不能默认套用到任务状态机。

终态事件还要处理 stale 和 fencing。比如一个旧 lease 的 `TaskCompleted` 在新 lease `TaskRedelivered` 之后到达，不能覆盖新状态。LogServe 的 task replay 里有 lease epoch 判断，actor completion 里也有 epoch 和 command seq 检查，目的就是拒绝 stale completion 和 out-of-order actor command。

如果事件来自多个 stream，不能用一个全局 seq 简化问题，除非 event store 真的提供全局顺序。跨 stream 投影要记录 vector checkpoint：

```text
task:1 -> seq 8
task:2 -> seq 3
workflow:a -> seq 12
```

需要跨 stream 因果关系时，要把因果依赖写进事件，例如 `causation_id`、`correlation_id`、`depends_on_seq`。

面试里可以这样答：

```text
重复事件靠 event_id 或 stream_id + seq 去重，已经处理过就跳过；同 key 不同 payload 要报警。乱序事件不能随便应用，单 stream projection 应要求 seq 连续，未来事件先到时 buffer、补读 event store，或者停住该 stream。

只有事件语义本身可交换时才允许乱序应用。任务、订单、actor command 这类状态机通常必须有序。LogServe 里 per-stream Seq、lease epoch 和 actor command_seq 都是在处理这个问题：重复可以幂等，乱序和 stale completion 要被拒绝或等待。
```

## Q026. 事件 replay 是否应该产生外部副作用？

**回答：**

一般不应该。replay 的目标是重建状态，不是重新执行历史上的外部动作。Martin Fowler 在 Event Sourcing 文章里专门提醒：如果 replay 事件时又向外部系统发送 modifier message，事情会出错，因为外部系统不知道这是正常处理还是重放。比如历史上发过一次邮件，replay 时又发一次；历史上扣过一次款，replay 时又扣一次。这类问题线上很难收拾。

外部副作用包括：

```text
发送邮件、短信、Webhook
扣款、退款、调用支付网关
创建第三方工单
删除对象存储文件
调用 LLM 或外部 API
发布用户可见通知
```

这些动作的共同点是：它们改变了 event-sourced 系统之外的世界。replay 可以重复很多次，外部世界不能跟着重复很多次。

正确分工是：

```text
replay reducer:
  从事件重建内部状态或 materialized view。

live side-effect handler:
  只在实时处理新事件时触发外部动作。

outbox/notification dispatcher:
  用幂等 key 可靠发送外部消息。
```

如果业务确实需要根据旧事件补发外部动作，也应该是一个显式 backfill job，而不是普通 replay 顺手做。backfill 要有范围、幂等 key、审批、dry-run 和审计。

有些外部查询也要小心。replay 时如果 reducer 调用外部汇率服务、权限服务、LLM 服务，得到的是今天的结果，不一定是事件发生时的结果。Fowler 对 external queries 的处理建议也是：要么外部系统能按历史时间查询，要么 gateway 记住当时响应，replay 时使用记录下来的响应。

LogServe 里这个边界很重要。`BootstrapFromLog` 重建 metadata 时，不能重新执行任务、不能重新调用 worker、不能重新调用 LLM、不能重新写 result store。它只能恢复控制面状态。actor replay 也只能重建 actor state，不能因为看到 `ActorCommandApplied` 就再次执行 Python 方法。

面试里可以这样答：

```text
事件 replay 不应该产生外部副作用。replay 是内部状态重建，可以重复执行；外部副作用会改变外部世界，重复执行会导致重复通知、重复扣款、重复调用 API。

我会把 reducer/projector 和 side-effect handler 分开。replay 模式只更新状态和 read model；实时模式通过 outbox、notification dispatcher 或 gateway 触发外部动作，并使用幂等 key。LogServe 的 BootstrapFromLog 只能重建 metadata，不能重新执行任务或重新调用 LLM。
```

## Q027. 如何让 replay 只重建状态而不重发通知？

**回答：**

要让 replay 只重建状态，核心是把“状态投影”和“通知发送”拆成两条管线。不要在 reducer 里直接调用邮件、短信、Webhook 或消息 broker。只要 side effect 混进 reducer，replay 就一定有风险。

一个比较稳的结构是：

```text
event store
  -> state projector / replay reducer
       只更新 materialized view
  -> live effect projector
       只处理新事件，写 outbox
  -> outbox dispatcher
       发送邮件、webhook、外部消息
```

state projector 可以从头跑无数次。live effect projector 只从“上线后的新 checkpoint”消费，或者在 replay 模式下禁用。

具体做法有几种。

**第一，显式 replay mode。**

事件处理上下文里有模式：

```text
context.mode = live | replay | backfill
```

gateway 检查模式。replay 模式下，调用 `NotificationGateway.Send` 不会真的发送，只会记录 debug 或直接 no-op。Fowler 也建议通过 gateway 隔离外部系统，让 gateway 知道当前是否处于 replay processing。

**第二，副作用只写 outbox，不直接发送。**

实时处理新事件时，effect handler 写 outbox：

```text
on TaskCompleted live:
  insert outbox(NotificationRequested(event_id, task_id))
```

replay 时不运行这个 handler，或者运行到 shadow outbox。真正发送由 dispatcher 完成，并用 `event_id + notification_type` 做幂等。

**第三，给 effect handler 单独 checkpoint。**

状态 projection 和通知 projection 不共用 checkpoint。

```text
state_projection checkpoint = seq 100000
notification_projection checkpoint = seq 95000
```

重建 state view 时，只重置 state checkpoint；不要重置 notification checkpoint。否则通知 handler 会把历史事件重新扫一遍。

**第四，记录 notification sent。**

即使误扫历史事件，也要能去重：

```text
sent_notifications(
  notification_type,
  source_event_id,
  recipient,
  sent_at
)
unique(notification_type, source_event_id, recipient)
```

这不是替代 replay mode，而是第二道防线。

**第五，把“要通知”建模成事实事件。**

业务事件和通知事件分开：

```text
TaskCompleted
NotificationScheduled
NotificationSent
```

replay `TaskCompleted` 只重建 task 状态。是否要发送通知，由实时流程在当时生成 `NotificationScheduled`。历史 replay 不再重新推导“当时应该通知谁”，除非你显式运行补发任务。

**第六，测试副作用隔离。**

这类问题必须测：

```text
given historical events
when replay projection
then notification gateway calls = 0
and outbox rows created = 0
and materialized view matches expected
```

LogServe 如果未来有任务完成通知、workflow webhook、LLM 完成回调，就应该按这个方式分层。当前 `BootstrapFromLog` 只调用 metadata 恢复逻辑，不应接入任何 worker dispatch 或外部 notification。actor replay 读取 actor stream 后也只重建 state，不应重新触发 actor command。

面试里可以这样答：

```text
让 replay 不重发通知，靠结构隔离而不是靠人记得别调用。reducer/projector 只负责状态；通知、webhook、支付这类副作用通过单独的 live effect handler 写 outbox，再由 dispatcher 发送。replay 模式禁用 effect handler 或写 shadow outbox。

同时要给通知发送加幂等表，例如 unique(notification_type, source_event_id, recipient)。状态 projection 和通知 projection 使用独立 checkpoint，重建 read model 时不能重置通知 checkpoint。测试里要明确断言 replay 后 gateway 调用次数为 0。
```

## Q028. 物化视图的 checkpoint 如何设计？

**回答：**

checkpoint 记录的是某个 materialized view 已经可靠处理到 event log 的哪个位置。它是 read model 可恢复、可观测、可增量更新的基础。没有 checkpoint，系统只能全量 replay；checkpoint 设计错了，view 会悄悄漏事件或重复应用事件。

一个最小 checkpoint 至少要有：

```text
projection_name
stream_id 或 partition_id
last_processed_seq
updated_at
```

生产系统通常还会加：

```text
reducer_version
event_schema_version 或 schema_set_version
last_event_id
last_event_time
checkpoint_time
status: running / blocked / rebuilding
error_message
checksum
owner_instance
lease_until
```

如果 projection 消费多个 stream，就不能只存一个整数。要么 event store 提供全局顺序，要么 checkpoint 是一个 vector：

```text
task:1 -> seq 10
task:2 -> seq 7
workflow:a -> seq 31
llm:task-x -> seq 3
```

如果事件来自 Kafka 这类 partitioned log，也可以记录 topic/partition/offset。

checkpoint 的核心规则是：只有事件对应的 view 更新已经提交，checkpoint 才能推进。推荐事务顺序是：

```text
BEGIN
  apply event to materialized view
  insert applied_events / update checkpoint
COMMIT
```

不要先推进 checkpoint 再更新 view。这样一旦崩溃，系统会以为事件处理过了，实际 view 没变。

批量 checkpoint 可以提高吞吐，但要接受重复处理窗口：

```text
每处理 100 条事件更新一次 checkpoint。
崩溃后最多重复 100 条。
```

只要 reducer 幂等，这个 trade-off 可以接受。对不可幂等聚合，不要大批量延迟 checkpoint。

checkpoint 还要和 reducer version 绑定。假设 `task_status_projection` 的 reducer 从 v1 升到 v2，旧 checkpoint 不能无条件复用。需要判断：

1. v2 是否兼容 v1 的 view state。
2. 是否需要从头 rebuild。
3. 是否可以从 snapshot + tail 继续。
4. 是否要新建 `task_status_projection_v2` 双跑。

checkpoint 也要支持错误状态。遇到无法解析事件、seq 缺口或业务不变量失败时，projection 应该停在出错事件之前，并记录：

```text
blocked_stream_id
blocked_seq
error_type
error_message
first_seen_at
```

继续推进 checkpoint 会掩盖错误。

LogServe 当前 `readAllLog` 可以从 `FromSeq` 读 stream，logstore stats 有 `NextSeq` 和 `TrimmedBeforeSeq`，这些都是实现 checkpoint 的基础。但现有 `BootstrapFromLog` 仍是从 `FromSeq=1` 全量读。要做增量 materialized view，可以在 metadata store 增加 projection progress：

```text
projection: task_metadata
stream_id: task:{id}
last_processed_seq: N

projection: workflow_metadata
stream_id: workflow:{id}
last_processed_seq: M

projection: llm_stats
stream_id: llm:{taskID}
last_processed_seq: K
```

actor snapshot 里已有类似思路：snapshot 记录某个 command count 和 snapshot seq，后面 replay tail；logstore 的 `TrimmedBeforeSeq` 表示早于某个 seq 的记录被逻辑裁剪。checkpoint 设计要和 snapshot/trim 对齐，否则会出现 checkpoint 指向已经被 trim 的事件。

面试里可以这样答：

```text
物化视图 checkpoint 要记录 projection 处理到哪个 stream/partition 的哪个 seq，并且最好带 reducer_version、last_event_time、status、错误信息和 checksum。view 更新和 checkpoint 推进必须在同一个原子边界里提交；只能在事件成功应用后推进 checkpoint。

多 stream projection 要用 vector checkpoint，不能假装一个全局整数。LogServe 已有 ReadLog(FromSeq)、per-stream Seq 和 TrimmedBeforeSeq，适合演进出 projection_progress；现有全量 BootstrapFromLog 可以作为 rebuild path，增量路径则从 checkpoint+1 继续。
```

## Q029. 如何压测 materialized view 重建速度？

**回答：**

压测 materialized view 重建速度，不能只测“能不能 replay 完”。要测吞吐、延迟、资源、正确性和退化场景。真正上线后，重建通常发生在最不舒服的时候：服务重启、投影损坏、schema 迁移、节点扩容、事故恢复。那时候你需要知道它多久能追平、会不会把 event store 打爆、会不会把 metadata DB 写满。

一个完整压测应该先定义目标：

```text
全量 rebuild 1000 万事件 <= 10 分钟
snapshot + tail rebuild 100 万事件 <= 30 秒
replay 过程中内存峰值 < 2GB
projection apply error = 0
rebuild 后 checksum 与基准一致
```

然后构造事件数据。数据不能只是一种小 payload，要覆盖真实分布：

1. stream 数量：少量热点 stream、大量冷 stream。
2. 每个 stream 事件长度：短任务、长 workflow、长生命周期 actor。
3. payload 大小：小 JSON、较大 result reference、含多 step workflow payload。
4. 事件类型分布：submitted、started、completed、failed、redelivered、snapshot。
5. 异常数据：重复事件、缺口、旧 schema、无法解析 payload。
6. snapshot 场景：无 snapshot、每 25 条 snapshot、每 100 条 snapshot。

压测维度至少包括：

```text
events_per_second
bytes_per_second
rebuild_duration_seconds
projection_apply_latency_ms
checkpoint_write_latency_ms
read_model_write_latency_ms
CPU utilization
memory RSS / heap / GC pause
disk read throughput
DB write IOPS
lock contention
error count
checksum mismatch count
```

要分冷缓存和热缓存。第一次从磁盘扫 segment 和第二次从 OS page cache 读，速度会差很多。只报热缓存结果很容易误导。

压测还要覆盖并行度：

```text
1 projector worker
2 projector workers
4 projector workers
8 projector workers
```

并行 replay 只能跨 stream/partition 做，同一个 stream 内仍要保持顺序。并行度上去后，瓶颈可能从 CPU 变成 metadata store 写锁、DB 索引写入、磁盘读、网络或 GC。

正确性验证不能省。每轮压测后都要输出 canonical checksum：

```text
hash(sorted(task_id,status,lease_epoch,result_ref))
hash(sorted(workflow_id,step_id,status))
hash(sorted(actor_id,command_count,state_hash))
```

如果速度提升了但 checksum 变了，那不是优化，是投影错了。

LogServe 可以做几类针对性 benchmark：

1. 生成 N 个 `task:` stream，每个 stream 含 `TaskSubmitted`、`TaskStarted`、`TaskCompleted`。
2. 生成 workflow stream，包含不同 DAG 宽度和 step 数。
3. 生成 actor stream，比较 full replay 和 snapshot+tail replay。
4. 用真实 `BootstrapFromLog` 跑全量恢复，统计耗时和内存。
5. 对 `MemoryStore`、未来 SQL metadata store 分别测 apply throughput。

如果用 Go，可以写 benchmark 或压测命令：

```text
BenchmarkBootstrapTasks_100K
BenchmarkBootstrapWorkflows_10Kx50Steps
BenchmarkActorReplay_WithSnapshot
BenchmarkProjectionApply_DuplicateEvents
```

压测报告里不要只写平均值。重建速度要看 p95/p99 apply latency、最长 stream、最慢 partition、checkpoint flush 耗时。最长尾部通常决定恢复时间。

面试里可以这样答：

```text
压测 materialized view 重建速度，我会生成接近真实分布的 event log，分别测全量 replay、snapshot+tail、增量追赶和并行 replay。指标包括 events/s、bytes/s、总重建时间、CPU、内存、GC、磁盘读、metadata 写 IOPS、checkpoint 延迟和错误数。

压测后要做 correctness checksum，确认重建出的 task/workflow/actor 状态和基准一致。LogServe 可以直接围绕 BootstrapFromLog、task stream、workflow stream、actor snapshot replay 写 benchmark；冷缓存和热缓存要分开报，否则恢复时间会被高估或低估。
```

## Q030. 如何观测 replay lag？

**回答：**

replay lag 是 event log 已经前进到某个位置，但 materialized view 或 replay worker 还没处理到那里。它要同时从“事件数量”和“时间”两个角度观测。只看一种都不够。

最基本的指标是：

```text
event_store_latest_seq{stream_or_partition}
projection_checkpoint_seq{projection, stream_or_partition}
projection_lag_events = latest_seq - checkpoint_seq
```

这告诉你还差多少条事件。但 1000 条事件可能只代表 1 秒，也可能代表 30 分钟，所以还要有时间 lag：

```text
event_store_latest_event_time
projection_last_event_time
projection_lag_seconds = now - projection_last_event_time
```

如果 event 写入暂停，`lag_events` 可能是 0，但 projection 已经 2 小时没处理事件。这时要看 heartbeat：

```text
projection_last_success_time
projection_last_poll_time
projection_worker_heartbeat
```

观测 replay lag 通常要分层。

**第一，按 projection。**

不同 view 的 lag 不一样：

```text
task_metadata_projection
workflow_status_projection
llm_stats_projection
search_index_projection
notification_projection
```

task view 追平不代表 search index 追平。

**第二，按 stream/partition。**

平均 lag 没意义。一个 partition 卡住，整体平均值可能还很好。要看最大 lag、p95 lag、最老未处理事件年龄。

```text
projection_lag_events_max
projection_lag_seconds_max
oldest_unprocessed_event_age_seconds
```

**第三，按错误状态。**

lag 增长通常有原因：

```text
projection_apply_errors_total
projection_blocked_stream
projection_blocked_seq
dead_letter_events_total
schema_decode_errors_total
checkpoint_commit_failures_total
```

如果只看 lag，不看 error，会知道“慢了”，但不知道“卡在哪”。

**第四，按吞吐。**

要知道能不能追上：

```text
event_ingest_rate_events_per_sec
projection_apply_rate_events_per_sec
catchup_eta_seconds =
  lag_events / max(projection_apply_rate - ingest_rate, small_value)
```

如果事件写入速度一直高于投影速度，lag 不会自然消失。

**第五，在 API 响应中暴露视图新鲜度。**

内部监控之外，读 API 可以返回：

```json
{
  "task_id": "task-1",
  "status": "running",
  "projection": {
    "name": "task_metadata",
    "stream_id": "task:task-1",
    "checkpoint_seq": 9,
    "latest_seq": 10,
    "stale": true
  }
}
```

对用户可见接口不一定每次都暴露这么细，但管理 API、debug header 或 dashboard 应该能看到。

告警可以这样设：

```text
critical: projection blocked for > 5m
warning: projection_lag_seconds_p95 > 30s
warning: oldest_unprocessed_event_age > 60s
critical: checkpoint not advancing while event ingesting
critical: apply_errors_total increases
```

LogServe 已有 logstore stats 的 `NextSeq` 和 `TrimmedBeforeSeq`，如果 metadata projection 记录 `last_processed_seq`，就能计算 `NextSeq - 1 - last_processed_seq`。`readAllLog` 当前从 `FromSeq=1` 读到尾部，bootstrap 完成后可以记录 replay duration；增量化后可以持续暴露 per-projection lag。对 actor stream，还要把 snapshot seq 和 tail lag 分开：snapshot 很新时 tail lag 小，恢复快；snapshot 很旧时 tail replay 长。

面试里可以这样答：

```text
replay lag 要看两类指标：事件数量 lag 和时间 lag。数量 lag 是 event_store_latest_seq - projection_checkpoint_seq；时间 lag 是 now - last_projected_event_time。还要按 projection、stream/partition 维度看最大值和 p95，不能只看平均。

我还会监控 apply rate、ingest rate、checkpoint commit latency、apply errors、blocked stream/seq、dead-letter 数量和 oldest unprocessed event age。LogServe 可以用 logstore 的 NextSeq 减 metadata projection 的 last_processed_seq 计算 lag；读 API 或 dashboard 可以暴露 projection checkpoint，方便解释 read model 为什么还没追上。
```

## Q031. materialized view 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

materialized view 的核心目标，是把“权威数据源中的事实”预先转换成“读路径真正需要的形状”，让查询不必每次都从原始模型、事件日志、多个表、多种存储或远程系统里重新计算。

它最直接解决的是**性能问题**，但不能只把它理解成一个性能技巧。更准确地说，它在几个维度上都有作用：

```text
source of truth / event log / normalized tables
        |
        | projection / refresh / replay
        v
materialized view / read model / query model
        |
        v
low-latency query / UI DTO / report / search / dashboard
```

**第一，它首先解决读性能和读扩展问题。**

很多系统的写模型并不适合直接查询：

- 写模型可能是规范化表，需要多表 join。
- 写模型可能是 event log，需要 replay 才能得到当前状态。
- 写模型可能是对象存储、KV、NoSQL 文档、远程数据源，查询能力有限。
- 写模型可能为了写入吞吐、幂等、审计、恢复而优化，而不是为了 UI 查询优化。

materialized view 把昂贵查询提前做掉。查询时直接读一个已经整理好的结果：

```text
GET /tasks?status=running
```

不需要每次扫描所有 TaskSubmitted、TaskStarted、TaskCompleted 事件，也不需要每次 join task、worker、lease、workflow 表。对于 p99 来说，这一点很关键。一次复杂查询平均很快没有意义，真正的问题是高峰期某些请求触发全量扫描、跨库 join、远程读取或大对象反序列化，导致尾延迟被拉高。

**第二，它解决读写模型不一致带来的可维护性问题。**

CQRS 里常见的做法是：写模型按业务一致性建模，读模型按查询和展示建模。两者的目标不同：

```text
write model: 保证业务规则、幂等、并发控制、事件含义
read model: 保证查询简单、字段齐全、响应稳定、便于分页和过滤
```

如果把所有查询都压到写模型上，写模型会慢慢变成一个“既要保证领域规则，又要照顾所有页面展示”的大杂烩。materialized view 的价值是承认读写需求不同，把复杂性放到投影更新逻辑里，而不是散落到每个查询接口里。

**第三，它可以间接服务正确性，但它本身不是 correctness 的 source of truth。**

这一点面试里很容易答错。materialized view 可以帮助正确性，因为它让派生状态的生成路径更明确：

- 从哪个 source 生成。
- 处理到了哪个 sequence / offset / checkpoint。
- 是否允许 lag。
- 是否可重建。
- 是否有 drift 检测。
- 是否有幂等事件应用。

但它不能替代权威数据。view 是派生数据，原则上应该可以丢弃后重建。如果业务把 view 当成唯一事实来源，比如只在 view 里扣库存、只在 view 里记录任务完成状态，就会把系统带到一个危险状态：一旦 view 更新失败、丢失、重建、回滚，业务事实也跟着丢。

因此更准确的表述是：

```text
materialized view 不是正确性的根；
它是正确性可验证的派生状态。
```

**第四，它有时服务安全和隐私，但不能单独承担安全边界。**

Materialized view 可以只暴露源数据的一个子集，例如：

- UI 只需要任务状态，不暴露完整 prompt / result。
- 报表只需要聚合指标，不暴露用户级明细。
- 搜索索引只保留可检索字段，不保留敏感字段。

这能降低误用风险，也能让接口契约更清楚。但安全不能只靠“view 里没有这个字段”。仍然需要权限校验、审计、删除策略、加密策略和源数据访问控制。否则一旦有人绕过 view 访问源表、event log 或对象存储，安全边界就失效了。

**第五，它在 LogServe 里的定位更接近“控制面当前状态视图”。**

LogServe 的 logstore 保存 append-only record，metadata store 保存 task、workflow、worker、actor、model、LLM stats 等当前状态。这里 metadata store 可以理解成一组 materialized views：

```text
logstore records:
  TaskSubmitted
  TaskStarted
  TaskCompleted
  WorkflowCreated
  StepScheduled
  ActorSnapshotCreated

metadata store:
  task_id -> current task status / lease / attempt
  workflow_id -> workflow current state
  worker_id -> heartbeat / load
  actor_id -> snapshot reference / generation
```

这样 control API 和 scheduler 不需要每次读 event log 计算当前状态。系统启动时可以 bootstrap/replay，把 log 重新投影到 metadata store。这个设计主要服务性能、恢复和代码边界；但 LogServe 目前仍是单机/多进程机制验证项目，不应该把它描述成已经具备完整分布式 projection ownership、全局 exactly-once 或跨节点强一致读模型。

面试里可以这样回答：

```text
materialized view 的核心目标是把权威数据源预先投影成查询友好的派生状态。它首先解决性能问题，降低查询延迟、复杂 join、event replay 和跨存储读取成本；其次提升可维护性，让写模型按业务规则建模，读模型按查询建模。它可以帮助正确性可观测，比如 checkpoint、lag、drift detection，但它本身不是 source of truth。真正的事实仍然在 event log、数据库主表或对象存储元数据里，view 应该可以丢弃并重建。
```

## Q032. materialized view 的典型适用场景和不适用场景分别是什么？

判断 materialized view 是否适用，不能只问“查询慢不慢”。更好的判断方式是看三个条件是否同时成立：

```text
1. 源数据形状不适合直接读。
2. 读路径对延迟、吞吐、分页、过滤或展示稳定性有要求。
3. 业务能够接受 view 与 source 之间存在可解释、可观测、可恢复的延迟。
```

**典型适用场景一：读多写少或读路径明显重于写路径。**

例如：

- Dashboard 展示任务数量、失败率、排队时间。
- 管理后台按状态分页查询任务。
- 报表按天、模型、worker、租户聚合。
- UI 需要一次返回 workflow、steps、latest task、result reference。

如果每个读请求都去扫日志、join 多张表或读多个存储，查询成本会随着用户数放大。把结果预先物化，写入时多做一点投影工作，读取时节省大量计算，通常是合理的。

**典型适用场景二：源数据是 event log，需要频繁读当前状态。**

Event sourcing 中，event log 适合记录事实，不适合直接服务所有查询。比如一个 task 的当前状态可能来自：

```text
TaskSubmitted(seq=10)
TaskStarted(seq=13)
TaskHeartbeat(seq=14)
TaskCompleted(seq=18)
```

如果每次查询 task status 都 replay 事件，吞吐和尾延迟会很差。此时维护一个 `task_metadata` view：

```text
task_id | status    | attempt | worker_id | updated_seq
--------------------------------------------------------
t1      | completed | 1       | w3        | 18
```

这就是典型适用场景。

**典型适用场景三：源数据规范化、半结构化或跨存储，直接查询复杂。**

例如：

- 源表高度规范化，查询需要多表 join。
- 订单明细在 NoSQL 文档中，报表需要按地区、品类聚合。
- 任务正文在对象存储，元数据在 SQL，搜索索引在 Elasticsearch。
- 写入系统使用 KV，读取系统需要按多个维度过滤。

materialized view 可以把读路径真正需要的字段整理到一个地方，甚至放到不同类型的存储里。Microsoft 的 Materialized View pattern 也明确把这种“源数据不适合查询、查询复杂、查询性能差”的场景作为主要动机。

**典型适用场景四：需要 query-specific read model。**

有些 view 只服务一个查询，也很正常：

```text
running_tasks_by_worker
workflow_summary_by_user
daily_model_usage
actor_snapshot_index
failed_steps_for_retry_dashboard
```

它们看起来不像“通用数据模型”，但这正是 materialized view 的价值：用冗余换查询简单。

**典型适用场景五：需要隔离源数据结构或敏感字段。**

比如源数据里有完整请求、输出、token 明细、内部错误堆栈，普通查询只需要：

```text
task_id
status
created_at
completed_at
result_ref
```

物化一个较窄的 view，可以减少接口误暴露和过度耦合。不过这只是安全辅助，不是完整权限模型。

**不适用场景一：源数据很简单，直接查就足够。**

如果数据量小、查询简单、索引充分、读写都在同一个数据库内，增加 materialized view 只会带来额外复杂度。比如一个小型配置表：

```text
model_name | enabled | timeout_ms
```

直接查主表就可以，不需要再维护一个 projection。

**不适用场景二：业务要求强一致、实时可见，而且不能解释延迟。**

如果用户提交订单后必须立刻在读接口中看到严格一致的库存、余额、风控状态，那么异步物化视图就很危险。可以考虑：

- 同事务更新主表和读表。
- 读写同源。
- 使用 session consistency / read-your-writes token。
- 在关键路径上读 source of truth。

materialized view 可以做到“足够新”，也可以通过同步更新降低延迟，但它天然要面对 source 与 view 的一致性窗口。

**不适用场景三：源数据变化极快，而 view 维护成本超过收益。**

比如每秒上百万次状态变化，每次变化都要更新多个聚合 view。此时 view 可能反而成为写入瓶颈：

```text
event ingest: 100k/s
projection update: 40k/s
lag: 持续增长
```

这种情况下要考虑窗口聚合、采样、批处理、近似统计、按需计算，或者只物化真正高价值的查询。

**不适用场景四：view 的语义还没有稳定。**

如果团队还没有定义清楚：

- 哪些事件会影响 view。
- 重复事件怎么处理。
- 乱序事件怎么处理。
- 删除和补偿事件怎么处理。
- checkpoint 代表什么。
- view 是否可以重建。

那贸然加 view 只会把不确定性固化成线上状态。物化视图不是“先存一份再说”，它必须有明确的 projection 语义。

**不适用场景五：系统没有运维 view 的能力。**

只要引入 materialized view，就要承担：

- replay。
- rebuild。
- drift detection。
- lag monitoring。
- checkpoint。
- schema evolution。
- backfill。
- 失败恢复。

如果团队没有这些能力，小系统里直接查询源数据往往更稳。

面试里可以这样回答：

```text
materialized view 适合读多写少、查询复杂、event log 当前状态读取、跨存储聚合、UI DTO、报表、搜索索引和安全子集暴露。不适合源数据简单、强一致优先、源数据变化极快、view 语义不稳定、或者团队没有 replay/checkpoint/lag/drift 运维能力的场景。它的本质是用冗余和异步维护成本，换读路径的低延迟和简单性。
```

## Q033. materialized view 和相近概念最容易混淆的边界在哪里？

materialized view 最容易和 cache、index、read replica、snapshot、普通 view、event log、source of truth 混在一起。面试时如果只说“提前算好的一份数据”，通常还不够，需要把边界说清楚。

**第一，materialized view 和 cache 的边界。**

cache 通常缓存“某个查询或对象的结果”，重点是减少重复读取。materialized view 是“从 source 通过明确规则派生出来的查询模型”，重点是改变数据形状。

```text
cache:
  key = GET /tasks/t1
  value = task response
  invalidation = TTL / delete / write-through

materialized view:
  source = event log / base tables
  projection = apply(event) -> task_current_state
  checkpoint = stream seq / offset
```

cache 可以没有完整的重建语义，失效后回源即可。materialized view 一般应该能从 source 全量重建，并且知道自己处理到哪个位置。

当然，Microsoft 文档里也会把 materialized view 说成一种 specialized cache，因为它确实是可丢弃、可重建的派生数据。但工程实现上要比普通 cache 多出 projection、checkpoint、幂等和一致性语义。

**第二，materialized view 和 index 的边界。**

index 加速访问原始数据，通常不改变数据语义：

```text
tasks(status, created_at) index
```

materialized view 可以改变形状、做聚合、做 join、做过滤、做转换：

```text
worker_id | running_task_count | oldest_running_task_age
```

index 回答的是“如何更快找到原表中的行”；materialized view 回答的是“读路径真正想看的结果长什么样”。

PostgreSQL 的数据库原生 materialized view 还可以再建 index，这说明两者可以组合：

```text
base table -> materialized view -> index on materialized view
```

**第三，materialized view 和 read replica 的边界。**

read replica 复制的是同一个数据库的同一种数据模型，主要解决读扩展和容灾：

```text
primary table schema == replica table schema
```

materialized view 可以只保留部分字段，甚至跨多个源聚合：

```text
event log + metadata + object refs -> workflow_summary_view
```

read replica 的问题通常是复制延迟；materialized view 的问题除了延迟，还包括 projection 逻辑正确性。

**第四，materialized view 和普通 view 的边界。**

普通 view 通常是一个保存下来的查询定义，查询时仍然访问底层表。materialized view 保存的是查询结果，读的时候直接读结果。PostgreSQL 文档里就强调，materialized view 会以类似表的形式持久化结果，刷新时重新执行定义查询。

所以差异是：

```text
normal view:
  stored query definition
  compute on read

materialized view:
  stored query definition / projection definition
  stored query result
  compute on refresh / update / replay
```

这也是为什么 materialized view 会有“数据不一定最新”的问题。

**第五，materialized view 和 source of truth 的边界。**

source of truth 是权威事实来源。materialized view 是派生结果。二者不能混淆。

```text
source of truth:
  event log
  canonical metadata row
  object store metadata record

materialized view:
  searchable index
  dashboard aggregate
  current-state read model
```

如果 view 丢了，理论上应该能从 source 重建。如果 source 丢了，view 不能证明完整历史，也不能安全恢复全部事实。

**第六，materialized view 和 snapshot 的边界。**

snapshot 通常优化的是单个 aggregate 或 stream 的 rehydration：

```text
load snapshot at seq=1000
replay events 1001..latest
```

materialized view 优化的是查询：

```text
find all running tasks by worker
show workflow list page
compute daily model usage
```

snapshot 面向写模型或恢复路径，materialized view 面向读模型。两者可以一起用：先用 snapshot 加快 replay，再把 replay 结果写入 view。

**第七，materialized view 和 event log / audit log 的边界。**

event log 保存发生过的事实，materialized view 保存这些事实投影出的当前状态或查询状态。

```text
event log:
  TaskSubmitted
  TaskStarted
  TaskCompleted

materialized view:
  task_id=t1, status=completed
```

audit log 主要服务审计和追责，不一定能完整恢复业务状态。event-sourced log 通常要求可以 replay 出状态。materialized view 则是 replay 的结果之一。

**第八，数据库原生 materialized view 和应用层 projection 的边界。**

这是非常常见的混淆点。

PostgreSQL materialized view 是数据库对象：

```sql
CREATE MATERIALIZED VIEW sales_summary AS
SELECT seller_no, invoice_date, sum(invoice_amt)
FROM invoice
GROUP BY seller_no, invoice_date;
```

刷新方式是数据库命令：

```sql
REFRESH MATERIALIZED VIEW sales_summary;
```

应用层 projection 则可能是代码处理事件：

```text
on TaskCompleted(event):
  update task_current_state
  update workflow_summary
  update model_usage_daily
  commit checkpoint
```

两者都可以叫 materialized view，但运维和一致性模型不同：

```text
DB native materialized view:
  SQL-defined
  refreshed by DB command
  often full refresh or database-supported concurrent refresh

application materialized view:
  code-defined
  updated by event handlers / replay
  checkpoint, idempotency, partition ownership handled by application
```

面试里可以这样回答：

```text
materialized view 和 cache 的区别在于它有明确的派生规则和重建语义；和 index 的区别在于它可以改变数据形状；和 read replica 的区别在于它不是复制同一个 schema；和普通 view 的区别在于它持久化结果；和 snapshot 的区别在于 snapshot 优化 rehydration，view 优化查询；和 source of truth 的区别在于 view 是可丢弃、可重建的派生数据。数据库原生 materialized view 是 SQL 对象，应用层 projection 是代码维护的读模型，二者名字相近但一致性和运维责任不同。
```

## Q034. materialized view 在高并发场景下可能出现哪些隐藏问题？

materialized view 在低并发下看起来很简单：source 变了，更新 view。高并发下真正困难的是：同一个 source 变化可能被多个 worker、多个 partition、多个 retry、多个版本的投影逻辑同时处理。隐藏问题通常不在“能不能查到数据”，而在“查到的数据是否可解释、是否单调、是否会漂移”。

**第一，重复事件导致非幂等更新。**

如果事件投递是 at-least-once，projector 可能收到重复事件：

```text
event(seq=18, type=TaskCompleted)
event(seq=18, type=TaskCompleted)  # retry / redelivery
```

如果 reducer 是：

```text
completed_count += 1
```

重复应用会让计数膨胀。正确做法通常是记录已经处理的 event id / stream seq / global offset，或者把更新设计成幂等：

```text
if task.last_completed_seq < event.seq:
  task.status = completed
  completed_count changes once
```

Microsoft Event Sourcing 文档也强调，事件消费者通常需要按 at-least-once 处理，handler 必须幂等，否则 projection 会从 eventstream 漂移。

**第二，乱序事件导致状态倒退。**

高并发下，不同 partition、队列、consumer group、网络重试可能让事件乱序到达：

```text
TaskCompleted(seq=20) arrives first
TaskStarted(seq=19) arrives later
```

如果 view 盲目应用后来的 `TaskStarted`，状态可能从 completed 倒退到 running。解决方式不是简单按 timestamp 排序，因为时钟也可能漂移。更可靠的是：

- 使用 per-stream sequence。
- 检查 expected next seq。
- 对可交换事件定义 commutative reducer。
- 对乱序事件暂存或拒绝。
- 对跨 stream 查询明确只保证 eventual consistency。

**第三，并发 projector 争抢同一 view row。**

比如多个 projector 同时更新 `workflow_summary`：

```text
StepSucceeded(step=1)
StepSucceeded(step=2)
StepFailed(step=3)
```

如果它们都读旧值、再写回新值，就会出现 lost update：

```text
read completed_steps = 0
write completed_steps = 1
```

三个事件处理完，计数可能还是 1。需要数据库原子更新、乐观并发版本、单 partition ownership，或者让同一个 aggregate 的事件按同一 key 串行处理。

**第四，view 更新和 checkpoint 提交不是原子操作。**

这是 projection 最危险的问题之一：

```text
1. apply event to view
2. commit checkpoint
```

如果 1 成功、2 失败，重启后会重复应用事件。要求 reducer 幂等。

反过来：

```text
1. commit checkpoint
2. apply event to view
```

如果 1 成功、2 失败，重启后会跳过事件，view 永久少一笔。通常要避免这种顺序，或者把 view 更新和 checkpoint 放在同一个事务里。

**第五，hot key 导致尾延迟和 lag 被少数实体拖垮。**

平均吞吐看起来足够，但一个热门 workflow、热门 tenant、热门 model 的事件过多：

```text
tenant=A: 90% events
tenant=B-Z: 10% events
```

如果按 tenant 分区，A 分区 lag 持续增长。读 API 查 A 的数据时 stale，查其他 tenant 正常。只看全局平均 lag 会掩盖问题，所以要看 max lag、p95 lag、oldest unprocessed event age 和 per-partition 指标。

**第六，锁竞争把 view 变成全局串行瓶颈。**

单机内存 view 经常用一把大锁保护：

```text
metadataStore.mu.Lock()
apply event
metadataStore.mu.Unlock()
```

并发低时没问题，并发高时所有 projection、API 读取、scheduler 查询都抢同一把锁。LogServe 的 MemoryStore 使用 map 和锁，这在机制验证阶段很直接，但如果任务量和 worker 数上来，热点查询和投影更新可能互相影响。生产实现通常会考虑分片锁、按 key 锁、MVCC、SQL 行级锁或外部存储。

**第七，读请求观察到中间态。**

一个事件可能更新多个 view：

```text
TaskCompleted ->
  task_current.status = completed
  workflow_summary.completed_steps += 1
  worker_running_count -= 1
```

如果读请求在三个更新之间到达，可能看到：

```text
task is completed
workflow still says running
worker still has one running task
```

这不一定是 bug，但必须定义语义：读模型是否原子更新？是否允许跨 view 短暂不一致？是否向用户暴露 freshness watermark？

**第八，backpressure 没做好时，view lag 会反过来拖垮写路径。**

如果每次写入都同步等待所有 view 更新：

```text
append event -> update 8 views -> return
```

某个慢 view 会拖慢写请求。如果完全异步：

```text
append event -> return
projector later updates views
```

读路径会看到 stale 数据。系统要明确选择：哪些 view 必须同步，哪些可以异步，哪些失败后进入 dead-letter，哪些允许降级。

**第九，安全和删除相关 view 最容易被漏更新。**

例如用户删除、权限变化、租户迁移后，源数据已经更新，但搜索 view 或报表 view 还保留旧字段。线上症状通常不是报错，而是“某些旧数据仍然能被查到”。因此敏感字段 view 需要更严格的删除传播、重建校验和访问控制。

面试里可以这样回答：

```text
高并发下 materialized view 的隐藏问题主要是重复事件、乱序事件、并发 projector 的 lost update、view 更新和 checkpoint 非原子、hot key、锁竞争、跨 view 中间态、lag/backpressure 和安全删除传播。解决思路是 per-stream sequence、幂等 reducer、原子 view+checkpoint、partition ownership、热点指标、读模型 freshness 暴露，以及把关键 view 和非关键 view 的同步级别分开。
```

## Q035. materialized view 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

materialized view 的实现质量，主要看故障路径。正常路径只需要“事件来了就更新 view”，但崩溃、重启、超时、重试会把很多隐含假设暴露出来。

**第一，事件已写入 source，但 view 还没更新。**

典型流程：

```text
1. append TaskSubmitted to event log
2. update task_metadata view
3. return success
```

如果系统在 1 之后、2 之前崩溃，event log 里有事实，但 metadata view 里没有。恢复时必须通过 replay 补上：

```text
read event log from checkpoint
apply TaskSubmitted
task_metadata[t1] = queued
```

这也是为什么 view 不能是唯一事实来源。LogServe 的写路径通常先 append record，再更新 metadata；启动时通过 bootstrap 从 log 重建 metadata，这个方向是合理的。

**第二，view 已更新，但 checkpoint 还没提交。**

流程：

```text
1. apply event(seq=100) to view
2. commit projection checkpoint = 100
```

如果 1 成功、2 之前崩溃，重启后 checkpoint 还停在 99，系统会再次处理 seq=100。此时 reducer 必须幂等，否则：

- 计数加两次。
- 状态被重复推进。
- 通知重复发送。
- 统计指标膨胀。

对于 current-state view，可以用 `last_applied_seq` 防重复；对于聚合计数，要更小心，因为 `count += 1` 天然不幂等。

**第三，checkpoint 已提交，但 view 更新没真正落盘。**

这是更严重的顺序错误：

```text
1. commit checkpoint = 100
2. apply event(seq=100) to view
```

如果 1 成功、2 失败，重启后系统以为 seq=100 已处理，不会重放，view 永久缺失这一条事件。除非有 drift detection，否则可能很久才发现。工程上应当：

- 避免先提交 checkpoint。
- 使用同一个事务提交 view mutation 和 checkpoint。
- 或者让 checkpoint 表示“下一条待处理事件”，并且只在 view mutation durable 后推进。

**第四，超时造成“成功还是失败”不确定。**

远程数据库、对象存储、消息系统经常出现：

```text
client sends update
server applies update
response times out
client retries
```

客户端看到的是超时，不知道服务端是否成功。如果 retry 没有 idempotency key / event id / compare-and-set，就可能重复写 view。面试中要强调：timeout 不是 failure，timeout 是 unknown。unknown 场景要靠幂等键、条件写、read-after-timeout 校验来处理。

**第五，重启时 reducer 版本变化。**

如果旧版本 projector 处理到 seq=500，新版本部署后从 checkpoint=500 继续处理，但 reducer 逻辑变了：

```text
v1: status = event.status
v2: status = deriveStatus(event.status, retry_policy)
```

那 view 的前 500 条是旧语义，后续是新语义。解决办法包括：

- projection version。
- view schema version。
- 全量 rebuild。
- backfill job。
- 双写新旧 view，验证一致后切流。

**第六，部分 rebuild 被误当成完整 view 对外服务。**

重建 view 时，如果直接清空旧 view 并逐步写入新 view，读请求可能看到半成品：

```text
delete all rows
replay first 10%
API reads view
```

更稳的做法：

- shadow build 新 view。
- build 完成后原子切换 alias / version。
- 对外暴露 rebuild 状态。
- 禁止未完成 view 服务强一致查询。

**第七，日志裁剪早于 view checkpoint。**

如果 event log 支持 trim：

```text
trim before seq=1000
projection checkpoint=800
```

那么 projector 重启后需要 801..999，但日志已经没了。结果是 view 无法完整恢复。LogServe 的 actor snapshot 和 trim 就有这种边界：只有在 snapshot 足够新、tail events 可读、checkpoint 明确的情况下，trim 才安全。通用原则是：

```text
trim point <= min(all required projection checkpoints, all required snapshot seq)
```

如果某个 view 未来还要重建，trim 策略也要考虑它是否还有完整 source。

**第八，replay 时不应重新触发外部副作用。**

view 重建需要 replay 事件，但 replay 的目标是恢复状态，不是重新发邮件、重新扣款、重新调用 webhook。Martin Fowler 在 event sourcing 讨论里也特别强调，重放处理时要隔离外部系统调用。实践上通常把逻辑分为：

```text
pure projection:
  event -> view state

side-effect handler:
  event -> notification/payment/webhook
```

replay 只运行 pure projection。

**第九，死信事件会让 view 永远卡住或跳洞。**

如果某条事件 schema 解析失败：

```text
seq=120 decode failed
```

projector 有三种选择：

- 卡住，保证不跳过。
- 跳过并 dead-letter，暴露 view 不完整。
- 用兼容逻辑/upcaster 修复后继续。

不能悄悄跳过，否则 checkpoint 前进了，view 却少了事实。

面试里可以这样回答：

```text
materialized view 在故障场景下主要暴露四类边界：event 写入和 view 更新之间的窗口、view 更新和 checkpoint 提交之间的原子性、timeout 后成功状态未知、以及 replay/rebuild 的副作用隔离。安全实现通常要求 source of truth 可重放、reducer 幂等、view mutation 和 checkpoint 原子或顺序安全、checkpoint 不早于 durable view、rebuild 使用 shadow view，日志裁剪不能早于所有需要的 projection checkpoint。
```

## Q036. materialized view 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

materialized view 的瓶颈不能固定说是 CPU 或 I/O，要看它处在生命周期的哪个阶段：

```text
initial rebuild / bootstrap
incremental projection
query serving
checkpoint / durability
backfill / schema migration
```

不同阶段瓶颈不同。

**第一，CPU 瓶颈通常来自反序列化、reducer 和聚合计算。**

典型 CPU 消耗包括：

- event payload JSON/Protobuf 解码。
- schema version upcast。
- 校验 event id、sequence、signature。
- reducer 执行业务逻辑。
- 聚合、排序、分组、hash。
- 压缩/解压缩。
- 加密/解密。

如果 view 是简单 current-state map：

```text
task_id -> status
```

CPU 通常不是主瓶颈。若 view 是复杂报表：

```text
group by tenant, model, day, status
percentile latency
deduplicate by idempotency key
```

CPU 可能很快成为瓶颈。

**第二，内存瓶颈来自全量 replay、批处理缓冲和高基数 key。**

内存 view 很快、简单，但会遇到：

- 全量 replay 时把所有状态放进 map。
- 聚合维度过多，key 数量爆炸。
- 暂存乱序事件。
- 批量更新缓冲太大。
- rebuild 时新旧 view 双份存在。
- GC pause 影响 p99。

例如按以下维度聚合：

```text
tenant_id x model_id x user_id x day x status
```

如果 user_id 是高基数维度，view 的大小可能远超预期。内存瓶颈往往不是平均值，而是某个租户或某天突然产生大量 key。

**第三，锁竞争来自共享 view、共享 checkpoint 和热点聚合。**

单机实现经常用全局锁：

```text
lock
  apply event
  update checkpoint
unlock
```

这样 correctness 容易保证，但并发上不去。读请求也可能被写入阻塞。隐藏瓶颈包括：

- 一个大 RWMutex 保护所有 metadata。
- 热点 workflow 的状态更新串行化。
- checkpoint 表单行更新。
- 数据库唯一索引冲突。
- projection worker 抢同一个 partition lease。

LogServe 的 MemoryStore 对机制验证很合适，因为 map + lock 简单清楚；如果要扩展到更高并发，就要把锁粒度、热点 key、读写隔离和持久化 checkpoint 拿出来单独设计。

**第四，I/O 瓶颈来自 event log 扫描、view 写入和 checkpoint fsync。**

initial rebuild 时，瓶颈通常是顺序读 event log：

```text
read events 1..N
deserialize
apply
```

增量投影时，瓶颈可能变成写 view：

- SQL upsert。
- 二级索引维护。
- LSM compaction。
- WAL/fsync。
- checkpoint 持久化。
- 大批量 backfill。

PostgreSQL 原生 materialized view 的 `REFRESH MATERIALIZED VIEW` 会替换 view 内容；如果不用 concurrent refresh，可能阻塞读；即使用 concurrent refresh，也需要唯一索引且同一 view 一次只能跑一个 refresh。这个边界说明，数据库内建能力也不能消除刷新成本，只是把成本交给数据库调度。

**第五，网络瓶颈来自跨进程、跨机房、远程存储和搜索引擎。**

如果 event store、projector、read DB、search index 不在同一节点：

```text
event store -> projector -> read database -> API
```

每一段都有网络延迟、重试、限流和批处理权衡。跨区域场景还会引入复制延迟，projection lag 不再只是本机处理速度，而是端到端传输和提交速度。

**第六，读路径性能和写路径性能要分开看。**

materialized view 常常让读请求变快，但让写入路径变重：

```text
without view:
  write event only
  read computes expensive query

with view:
  write event + update projections
  read simple view
```

所以要看系统目标。如果系统主要瓶颈是读，view 很有价值。如果写流量很高、读很少，view 可能不划算。

**第七，瓶颈定位要看指标，而不是凭经验猜。**

可以把指标拆成：

```text
projection_decode_ms
projection_apply_ms
projection_checkpoint_ms
projection_view_write_ms
projection_batch_size
projection_lag_events
projection_lag_seconds
projection_lock_wait_ms
projection_memory_bytes
projection_gc_pause_ms
view_query_p50/p95/p99
```

如果 `apply_ms` 高，可能是 CPU/reducer；如果 `checkpoint_ms` 高，可能是 I/O；如果 `lock_wait_ms` 高，是锁竞争；如果 `lag_seconds` 高但 CPU 空闲，可能是网络或下游限流；如果 GC pause 高，是内存和对象分配。

面试里可以这样回答：

```text
materialized view 的瓶颈取决于阶段。全量重建常见瓶颈是 event log 顺序 I/O、反序列化和内存；增量投影常见瓶颈是 view 写入、checkpoint fsync、锁竞争和热点 key；复杂聚合会变成 CPU；远程 event store、SQL、搜索引擎或跨区域复制会变成网络。不能只说某一个瓶颈，应该用 decode/apply/view-write/checkpoint/lock-wait/lag/query-p99 指标拆开看。
```

## Q037. materialized view 的 correctness test、stress test 和 benchmark 应该分别测什么？

这三类测试目标不同，不能混在一起：

```text
correctness test: 结果对不对
stress test: 极端并发和故障下会不会坏
benchmark: 成本和性能是多少
```

**第一，correctness test 要证明 view 是 source 的正确派生。**

最基本的测试是给定一组事件，检查最终 view：

```text
Given:
  TaskSubmitted(t1)
  TaskStarted(t1, worker=w1)
  TaskCompleted(t1)

Expect:
  task_view[t1].status == completed
  worker_view[w1].running_count == 0
```

但这只是 happy path。真正需要覆盖的是边界：

**重复事件：**

```text
TaskCompleted(seq=3)
TaskCompleted(seq=3)
```

期望 view 不漂移，计数不重复增加。

**乱序事件：**

```text
TaskCompleted(seq=3)
TaskStarted(seq=2)
```

期望系统要么暂存、要么拒绝、要么用 sequence 防止状态倒退。

**缺口事件：**

```text
seen seq=10
next event seq=12
```

期望 projector 不应悄悄跳过 seq=11。

**幂等重放：**

```text
apply events 1..100
apply events 1..100 again
```

期望 view 不变，或者重复部分被跳过。

**checkpoint 原子性：**

模拟崩溃点：

```text
after view write, before checkpoint
after checkpoint, before view write
during batch update
```

检查重启后能否恢复正确。

**schema evolution：**

老事件和新事件混合：

```text
TaskSubmittedV1
TaskSubmittedV2
```

期望 upcaster / tolerant reader 能处理历史事件。

**删除和补偿：**

```text
UserDeleted
TaskCanceled
WorkflowCompensated
```

期望 view 中敏感字段、索引、聚合都更新。

**源与 view 对账：**

可以写一个慢但权威的 oracle：

```text
recompute_from_source(events) == materialized_view
```

即使线上不能每次全量算，测试里也应该有这种 oracle。

**第二，stress test 要证明高并发和故障下不会破坏不变量。**

Stress test 不关心单次结果是否快，重点是把系统推到危险状态：

- 多个 writer 并发写同一个 aggregate。
- 多个 projector 争抢同一个 partition。
- 高重复率事件投递。
- 高乱序率事件投递。
- 热点 tenant / workflow / task。
- 下游 read DB 随机超时。
- checkpoint 写入随机失败。
- projector 随机 crash/restart。
- rebuild 和线上查询同时发生。
- schema version 滚动升级。
- event log trim 与 projection checkpoint 交错。

测试时持续检查不变量：

```text
checkpoint never advances past durable view
no negative running_task_count
completed task never returns to running
projection lag eventually decreases after load stops
view rebuild equals oracle
no duplicate side effects during replay
```

对 LogServe 这种任务系统，可以压：

```text
N workflows
M steps per workflow
K workers
random lease timeout / retry / completion
random bootstrap during load
```

然后检查 task view、workflow view、worker load、logstore records 是否能对上。

**第三，benchmark 要测成本、吞吐、延迟和资源，不判断正确性。**

Benchmark 要先定义 workload：

```text
events: 1M TaskSubmitted/Started/Completed
key distribution: uniform / zipf hot key
payload size: 1KB / 16KB / 256KB
projection count: 1 / 5 / 20
storage: memory / SQL / KV / search
```

然后分别测：

**重建速度：**

```text
events_per_second
bytes_per_second
total_rebuild_time
time_to_first_scannable_view
```

**增量投影延迟：**

```text
event_append_to_view_visible_ms
projection_apply_p50/p95/p99
checkpoint_commit_p50/p95/p99
```

**读路径收益：**

```text
source_query_p99
view_query_p99
view_staleness_ms
```

**资源：**

```text
CPU %
allocs/op
heap bytes
GC pause
disk write bytes/event
network bytes/event
read DB IOPS
```

**扩展性：**

```text
1 projector
2 projectors
4 projectors
8 projectors
```

看吞吐是否接近线性，还是被锁、hot key、checkpoint 或下游 DB 卡住。

**第四，三类测试的常见误区。**

误区一：用 benchmark 代替 correctness。跑得快但重复事件会多加计数，没有意义。

误区二：只测 happy path replay。线上更多问题来自 crash point、timeout unknown、重复投递和 schema 演进。

误区三：只测平均延迟。materialized view 服务的是读路径，p99、max lag、oldest unprocessed age 更重要。

误区四：只测单 projection。真实系统往往一个事件更新多个 view，慢 view 会产生 backpressure。

面试里可以这样回答：

```text
correctness test 要测从 source 到 view 的派生是否正确，包括重复、乱序、缺口、checkpoint 崩溃点、schema 演进、删除和重建对账。stress test 要测并发 writer/projector、hot key、随机超时、随机 crash、rebuild 与查询并行时不变量是否仍成立。benchmark 要测 rebuild 吞吐、增量投影延迟、view query p99、lag、CPU/内存/GC/I/O/网络和扩展性。三者不能互相替代。
```

## Q038. 如果要求从零实现一个简化版 materialized view，你会先定义哪些不变量？

从零实现 materialized view，先写代码反而危险。应该先定义不变量，因为这些不变量决定了 checkpoint、幂等、replay、并发和 trim 的设计。

**不变量一：source of truth 唯一且可枚举。**

必须先说明 view 从哪里来：

```text
source = event log
or
source = base tables
or
source = object metadata table
```

如果 source 是 event log，需要能按稳定顺序读取：

```text
read(stream_id, from_seq, limit)
```

如果 source 都不能完整枚举，就不能承诺 view 可重建。

**不变量二：view 中任意一行都能由 source 推导。**

view 不能含有“手工修过但 source 没有”的状态。形式化一点：

```text
view_at_checkpoint = project(source_events[<= checkpoint])
```

这条不变量非常重要。它意味着：

- view 可以删除后重建。
- 测试可以用 oracle 对账。
- 线上可以做 drift detection。
- bug 修复后可以 backfill。

**不变量三：checkpoint 只在 view mutation durable 后推进。**

简化版可以定义：

```text
checkpoint = last event seq that has been durably reflected in the view
```

那么更新顺序必须满足：

```text
apply event to view
persist view mutation
persist checkpoint
```

更好的是把 view mutation 和 checkpoint 放在同一事务里：

```sql
BEGIN;
  UPSERT view_rows ...
  UPDATE projection_checkpoint SET seq = 100;
COMMIT;
```

如果做不到同事务，就必须让重复应用安全。

**不变量四：reducer 必须 deterministic。**

同一批事件，无论在哪台机器、什么时候 replay，结果都一样：

```text
project(events) == project(events)
```

reducer 里不能直接用：

- 当前时间 `now()` 作为业务结果。
- 随机数。
- 当前外部服务返回值。
- 非稳定 map iteration 顺序。
- 本地配置里的隐式版本差异。

如果确实需要时间，应当由事件携带发生时间，或者在 envelope 里记录 ingestion time。

**不变量五：reducer 必须 idempotent 或由 projector 保证 exactly-once apply。**

现实里 exactly-once 很难，所以简化版也应该按 at-least-once 设计：

```text
apply(event)
apply(event)
```

结果应等价于应用一次。实现方式可以是：

- view row 保存 `last_applied_seq`。
- projection checkpoint 按 stream/partition 保存。
- processed_events 表去重。
- 用 set-state 替代 increment。

对于计数类聚合，如果不能天然幂等，就必须记录贡献明细或事件去重表。

**不变量六：同一 stream 内事件按 seq 单调应用。**

简化版可以不支持全局顺序，但至少要定义 per-stream 顺序：

```text
task:t1 seq=1,2,3...
workflow:w1 seq=1,2,3...
```

projector 看到 seq 缺口时不能直接跳过：

```text
expected=10, got=12 -> gap
```

要么等待，要么报错，要么进入 repair 流程。

**不变量七：view schema 和 projection code 有版本。**

因为 view 语义会变：

```text
projection_name = task_current_state
projection_version = 3
schema_version = 2
```

不变量可以定义为：

```text
同一个 view version 内，reducer 语义固定。
reducer 语义变化时，要 rebuild 或迁移。
```

否则线上会出现半旧半新的 view。

**不变量八：replay 只改变 view，不产生外部副作用。**

重建状态时不应发送通知、扣款、调用 webhook。可以规定：

```text
projection handler: pure state update
side-effect handler: separate consumer with own inbox/idempotency
```

这样 replay 才安全。

**不变量九：读者能知道 view 的 freshness。**

简化版也要暴露：

```text
projection_checkpoint_seq
source_latest_seq
lag_events
lag_seconds
projection_version
```

否则 API 用户无法判断读到的是最新状态还是落后状态。

**不变量十：一个 partition 同一时刻只有一个有效 owner。**

如果允许多个 projector 并行，必须定义 ownership：

```text
partition_id -> owner_id, lease_epoch
```

写 view 时带 fencing token：

```text
update view where lease_epoch = current_epoch
```

简化版也可以先规定“单 projector”，但要写进语义里，不能假装分布式并发安全。

**不变量十一：log trim 不得破坏重建能力。**

如果 source 支持裁剪，需要定义：

```text
trim_before <= min(required_checkpoint_or_snapshot_seq)
```

否则 view 需要重建时会发现 source 已经没了。

**不变量十二：view 可以整体丢弃并重建。**

这是 materialized view 的本质。简化版可以提供：

```text
drop view
rebuild from source
verify checksum
switch alias
```

如果 view 丢了不能重建，那它已经不只是 view，而是事实来源的一部分。

面试里可以这样回答：

```text
我会先定义这些不变量：source of truth 唯一且可枚举；view 任意状态都能由 source 和 checkpoint 推导；checkpoint 只在 view mutation durable 后推进；reducer deterministic、idempotent、按 per-stream seq 应用；事件缺口不能静默跳过；projection/schema 有版本；replay 不产生外部副作用；读者能看到 freshness；多 projector 要有 ownership/fencing；log trim 不得早于需要的 checkpoint；view 可以丢弃并从 source 重建。代码实现只是这些不变量的落地。
```

## Q039. materialized view 的常见误用是什么，误用后通常会产生什么线上症状？

materialized view 的误用通常不是立刻崩溃，而是慢慢变成“看起来能跑，但状态越来越解释不清”。面试里可以把误用和线上症状对应起来。

**误用一：把 materialized view 当成 source of truth。**

表现：

```text
只有 view 里有状态，event log / 主表没有完整事实。
```

线上症状：

- 重建 view 后数据消失。
- 回放 event log 得不到当前状态。
- 审计无法解释某个字段为什么变化。
- 修复 projection bug 时不敢 rebuild。
- 灾难恢复只能恢复部分状态。

正确做法是让 view 可丢弃、可重建；真正事实留在 event log、主表或权威 metadata。

**误用二：允许业务代码直接手工修改 view。**

例如为了修数据直接：

```sql
UPDATE task_current_view SET status = 'completed' WHERE task_id = 't1';
```

但 source 中没有对应事件。线上症状：

- view 和 source 对不上。
- 下次 rebuild 后修复消失。
- 不同 view 之间互相矛盾。
- 客服看到 completed，审计看到 running。

如果确实要修正，应该写补偿事件或修 source，再由 projection 更新 view。

**误用三：没有 checkpoint 或 checkpoint 语义不清。**

表现：

```text
projector 只知道自己“跑过”，不知道处理到哪里。
```

线上症状：

- 重启后不知道从哪里 replay。
- 重复消费导致计数膨胀。
- 跳过事件导致状态缺失。
- lag 无法观测。
- log trim 不知道是否安全。

checkpoint 必须清楚表示“哪些 source 已经 durable reflected in view”。

**误用四：非幂等 reducer 处理 at-least-once 事件。**

表现：

```text
on TaskCompleted:
  completed_count += 1
```

但没有 event 去重。线上症状：

- 统计数量偶尔偏大。
- retry 越多，报表越离谱。
- 同一个 task 在 completed list 里出现多次。
- worker running count 变成负数。

这种 bug 很隐蔽，因为低并发和无重试环境下测不出来。

**误用五：忽略乱序和缺口。**

表现：

```text
收到什么事件就应用什么事件。
```

线上症状：

- completed task 又变成 running。
- workflow summary 比 task detail 落后或超前。
- 同一页面刷新两次状态来回跳。
- 排查时发现 event log 顺序正确，但 view 应用顺序错误。

解决方式是按 stream sequence、offset、version 或业务规则处理顺序。

**误用六：用异步 view 做强一致决策。**

例如用落后的库存 view 判断是否还能下单，用落后的权限 view 判断是否能访问敏感数据。线上症状：

- 超卖。
- 重复调度任务。
- 已删除权限仍可访问。
- GC 删除仍被引用的对象。

如果决策必须强一致，要读 source of truth，或者使用同步事务、条件写、read-your-writes token、fencing 等机制。

**误用七：view 太宽，包含过多敏感字段。**

为了方便查询，把完整 payload、prompt、result、错误堆栈、用户信息都塞进 view。线上症状：

- GDPR/删除请求难处理。
- 搜索索引泄漏敏感字段。
- 普通后台接口过度暴露。
- view 存储成本暴涨。
- schema 变化影响面变大。

view 应该按查询需要最小化字段，敏感字段尽量引用化、脱敏或单独加密。

**误用八：过度物化每一个查询。**

表现：

```text
一个新页面 -> 一个新 view
一个新过滤条件 -> 一个新 view
一个新排序 -> 一个新 view
```

线上症状：

- 写放大严重。
- projection lag 变大。
- 存储成本膨胀。
- schema migration 要改很多 view。
- 某个冷门 view 坏了没人发现。

materialized view 应该服务高价值、高频、复杂或隔离性强的查询，不是所有查询都要物化。

**误用九：没有 rebuild 和 drift detection。**

表现：

```text
view 一旦建好就长期运行，从不全量对账。
```

线上症状：

- 小错误累积成大漂移。
- 某次部署 bug 影响历史数据但没人知道。
- 客户报错时只能手工查。
- 修复代码后不知道哪些 view 要重建。

至少要有离线对账：

```text
hash(source-derived-state) == hash(view-state)
```

以及按 projection 暴露 checkpoint、lag、version。

**误用十：replay 时触发外部副作用。**

表现：

```text
replay TaskCompleted -> send notification again
```

线上症状：

- 重建 view 后用户收到历史通知。
- 支付、webhook、邮件重复触发。
- 测试环境 replay 污染外部系统。

replay handler 要和 side-effect handler 分开。

面试里可以这样回答：

```text
常见误用包括把 view 当 source of truth、手工改 view、没有 checkpoint、非幂等 reducer、忽略乱序缺口、用异步 view 做强一致决策、view 过宽包含敏感字段、过度物化、没有 rebuild/drift detection，以及 replay 时触发副作用。线上症状通常是状态漂移、计数膨胀、读到旧权限、任务重复调度、重建后数据消失、报表对不上、p99 上升和 projection lag 持续增长。
```

## Q040. materialized view 在单机和分布式环境中的语义有什么差异？

单机和分布式环境最大的差异，不是“数据量大小”，而是语义假设是否还能成立。单机里很多事情可以靠进程内顺序、锁和本地事务自然成立；分布式里必须显式设计顺序、所有权、幂等、checkpoint 和读一致性。

**第一，单机环境里顺序和所有权更简单。**

单机实现可以这样做：

```text
one process
one log reader
one metadata store
one lock
```

事件按本地 log 顺序读取，projector 顺序应用，metadata map 用锁保护。只要进程不并发乱写，很多问题会被简化：

- 不需要 partition ownership。
- 不需要跨节点 fencing。
- 不需要 consumer group rebalance。
- 不需要处理两个 projector 同时写同一行。
- read-your-writes 可以通过同步更新做到。

LogServe 当前更接近这种模式：logstore 是权威记录，metadata store 是内存/元数据当前状态，bootstrap 时从 log replay。这个设计适合解释机制和验证 event log + metadata projection 的关系。

**第二，单机环境仍然要处理崩溃恢复。**

单机不等于没有一致性问题。它仍然会遇到：

```text
append log succeeded, metadata update failed
metadata update succeeded, process crashed before response
bootstrap replay duplicate event
in-memory view lost
```

所以单机也需要：

- log 作为 source of truth。
- replay。
- idempotent reducer。
- 明确启动流程。
- trim 和 snapshot 边界。

只是这些问题发生在一个进程或一个节点内，调试和约束更简单。

**第三，分布式环境里全局顺序通常不存在或代价很高。**

分布式系统常见的是：

```text
partition A: seq 1,2,3
partition B: seq 1,2,3
```

同一 partition 内有序，不同 partition 之间没有全局有序。跨实体 view 会看到组合状态：

```text
workflow view 已处理 partition A 到 offset 100
task view 已处理 partition B 到 offset 80
```

因此要明确：

- view 是按 partition checkpoint 定义 freshness。
- 跨 partition 查询只保证 eventual consistency。
- 如果要全局一致快照，需要额外协议或统一事务边界。

**第四，分布式环境需要 projection ownership 和 fencing。**

多个 projector worker 可能同时运行：

```text
projector-1 handles partition-3
projector-2 handles partition-3 after rebalance
```

如果旧 worker 卡顿后恢复，又继续写 view，就会出现 split-brain projection。解决方式通常是 lease + epoch：

```text
partition_id = 3
owner = projector-2
epoch = 42
```

每次写 view 和 checkpoint 都带 epoch。旧 epoch 的写入被拒绝：

```text
UPDATE view
WHERE partition_id = 3 AND epoch = 42
```

没有 fencing，只靠“我觉得旧 worker 已经停了”，分布式语义不可靠。

**第五，分布式环境要接受 at-least-once，并把 exactly-once 当成端到端性质设计。**

很多消息系统可以提供某些范围内的 exactly-once，但 view 的端到端语义还取决于：

- event source。
- message delivery。
- projector code。
- view storage。
- checkpoint storage。
- retry behavior。

如果 view 更新和 checkpoint 不在同一个事务中，重复和丢失仍然可能发生。所以更稳妥的思路是：

```text
at-least-once delivery
+ idempotent reducer
+ atomic view/checkpoint
+ event id dedup
= effectively-once projection
```

**第六，分布式读语义要显式暴露。**

单机同步更新后，API 很容易做到：

```text
write returns -> subsequent read sees write
```

分布式异步 projection 下不一定成立：

```text
POST /tasks -> event appended at offset 100
GET /tasks/t1 -> read view checkpoint 97
```

用户会觉得“刚提交的任务不见了”。要提供 read-your-writes，可以：

- 写 API 返回 event offset / version。
- 读 API 支持 `min_checkpoint >= offset`。
- gateway 等待 projection catch up。
- session token 路由到足够新的副本。
- 对未追上的 view 返回 pending/stale 状态。

如果不设计这个语义，最终一致性会变成用户体验问题。

**第七，分布式环境里 rebuild 和 backfill 影响更大。**

单机 rebuild 可以停机或本地重放。分布式 rebuild 要考虑：

- 是否影响线上读。
- shadow view 怎么切换。
- 每个 partition checkpoint 如何记录。
- backfill 是否和实时增量并行。
- 旧 projection 和新 projection 如何双跑对账。
- 下游存储是否被 backfill 打爆。

常见做法是：

```text
build view_v2 from source snapshot
tail incremental events
compare view_v1 and view_v2
switch read alias to view_v2
retire view_v1
```

**第八，分布式环境里成本和故障面更大。**

引入分布式 materialized view 后，系统多了：

- projector worker。
- queue / broker。
- checkpoint table。
- read model storage。
- dead-letter queue。
- lag monitor。
- replay/backfill tools。
- schema registry/upcaster。
- ownership lease。

这些组件本身都会失败。面试里要敢于说边界：materialized view 不是免费午餐，它用额外组件和最终一致性换读路径性能和扩展性。

**第九，LogServe 的回答边界。**

如果结合 LogServe，可以这样说：

```text
LogServe 当前可以把 metadata store 看成单机 materialized view：logstore 是事实记录，bootstrap 从 log 重建 metadata，control plane 读 metadata 提供当前状态。这个语义在单机/多进程机制验证中足够清楚。若扩展成分布式系统，需要补上 projection partition ownership、持久化 checkpoint、fencing token、per-view lag 指标、read-your-writes token、shadow rebuild 和跨节点重复事件处理，否则不能声称具备完整分布式 materialized view 语义。
```

面试里可以这样回答：

```text
单机 materialized view 可以依赖本地 log 顺序、进程内锁和同步更新，语义更接近“append log 后更新本地 metadata，再通过 replay 恢复”。分布式环境里必须显式定义 partition 顺序、projection ownership、fencing、idempotency、atomic view/checkpoint、lag、read-your-writes 和 rebuild/backfill 语义。单机问题主要是崩溃恢复；分布式问题还包括重复投递、乱序、split-brain projector、跨 partition 不一致和用户读到旧 view。
```

## Q041. CQRS 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

CQRS 的核心目标，是把“改变系统状态的操作”和“读取系统状态的操作”分开建模。它不是先从数据库拆分开始，也不是先从消息队列开始，而是先从职责拆分开始：

```text
command side:
  接收意图
  校验业务规则
  修改权威状态
  产生事件或更新 source of truth

query side:
  读取状态
  返回 DTO / read model / materialized view
  不执行领域修改
```

Microsoft 的 CQRS pattern 文档把它定义为把 read operations 和 write operations 分离到不同的数据模型中，从而分别优化模型。Martin Fowler 也强调，CQRS 的核心是“更新信息的模型”和“读取信息的模型”可以不同。这个点比“读写库分离”更底层。

**第一，CQRS 首先解决的是复杂性和读写目标冲突。**

传统 CRUD 模型里，同一个模型既要服务写入，又要服务查询：

```text
Task {
  TaskID
  Status
  WorkerID
  LeaseEpoch
  Payload
  ResultRef
  RetryPolicy
  CreatedAt
  UpdatedAt
}
```

写入侧关心的是：

- 任务能不能提交。
- idempotency key 是否重复。
- lease epoch 是否匹配。
- 状态机是否允许从 running 到 completed。
- 重试是否超出上限。

读取侧关心的是：

- 按状态分页。
- 展示最近 50 个失败任务。
- 展示 workflow summary。
- 展示 worker 当前负载。
- 展示 result reference。

这两组需求会把同一个模型拉向不同方向。CQRS 的价值，是承认“写模型”和“读模型”可以不是同一个东西。写模型可以围绕业务不变量组织；读模型可以围绕查询和 UI 组织。

**第二，它通常能解决性能和扩展问题。**

读写负载经常不对称：

```text
reads: 10000/s
writes: 200/s
```

如果读写共用同一张表、同一组索引、同一个 ORM 模型，复杂查询和写事务会互相影响。CQRS 允许：

- 写侧用事务数据库、append-only log 或聚合存储。
- 读侧用 denormalized table、KV、搜索引擎、缓存或内存 view。
- 读侧横向扩展。
- 写侧按 aggregate 串行化，减少冲突。

Microsoft 文档也明确把独立扩展、针对读写分别优化 schema、降低 lock contention 作为 CQRS 的主要收益。

**第三，它能改善可维护性，但前提是边界真实存在。**

可维护性来自两个方向：

```text
write model: 业务规则集中在 command handler / aggregate
read model: 查询代码只负责读取和组装 DTO
```

如果系统真的有复杂业务规则和复杂查询，这个拆分会让代码更清楚。比如 `SubmitTask`、`CompleteTask`、`CallActor` 是命令；`GetTask`、`ListWorkflows`、`DescribeActor` 是查询。命令侧处理幂等、状态机、租约、日志追加；查询侧只读 metadata store 或 read model。

但如果系统只是简单增删改查，CQRS 会让代码量变多，部署链路变长，数据一致性更难解释。Fowler 的提醒很实在：大多数系统并不需要 CQRS，滥用会增加风险。

**第四，它可以辅助正确性，但不是自动提供正确性。**

CQRS 对正确性的帮助在于：命令模型可以更好地表达业务意图。

```text
bad command:
  SetTaskStatus(status=completed)

better command:
  CompleteTask(task_id, lease_epoch, result_ref)
```

后者可以校验：

- task 是否存在。
- 当前是否 running。
- lease epoch 是否匹配。
- result_ref 是否可用。
- 是否重复完成。

这种命令比裸更新字段更接近业务语义，能减少非法状态。

不过 CQRS 也会引入新的正确性问题：

- read model 落后于 write model。
- 用户基于 stale read 发出错误 command。
- command 成功但 query 暂时读不到。
- 同一个命令重试造成重复写。
- read model 和 write model 漂移。

所以 CQRS 不是“正确性银弹”。它把写侧正确性做得更集中，同时要求系统处理读侧滞后和同步失败。

**第五，它能改善安全边界，但不能替代权限系统。**

CQRS 让读权限和写权限更容易分开：

```text
query:
  ListTasks
  GetWorkflow

command:
  SubmitTask
  CancelWorkflow
  RegisterWorker
```

写命令可以经过更严格的授权、审计、幂等和业务校验。读模型可以只暴露必要字段，避免把内部状态、敏感 payload 或错误堆栈泄露给普通用户。

但安全仍然需要：

- 认证。
- 授权。
- 字段级过滤。
- 审计。
- 防重放。
- 租户隔离。

只把接口拆成 command/query，不等于安全。

**第六，结合 LogServe 可以这样理解。**

LogServe 里，控制面命令包括：

```text
SubmitTask
StartWorkflow
CompleteTask
CallActor
RegisterModel
WorkerHeartbeat
```

这些操作会追加 logstore record，并更新 metadata store。查询侧则读 metadata：

```text
GetTask
ListTasks
GetWorkflow
ListWorkers
GetActor
```

这里已经有 CQRS 的影子：命令侧关注日志追加、幂等、lease epoch、actor command sequence；查询侧关注当前状态视图。它不是一个完整分布式 CQRS 平台，但作为单机/多进程机制验证，已经能解释“写事实”和“读状态”为什么要分开。

面试里可以这样回答：

```text
CQRS 的核心目标是把 command 和 query 分开建模。command 表示业务意图，负责校验和修改权威状态；query 只读取状态，返回面向展示的 DTO 或 read model。它首先解决读写模型目标冲突、性能扩展和可维护性问题，也能辅助正确性和安全边界，但不会自动保证正确性。真正的代价是读写模型同步、eventual consistency、重复命令、stale read 和运维复杂度。
```

## Q042. CQRS 的典型适用场景和不适用场景分别是什么？

CQRS 适不适合，不看系统是否“高级”，而看读写模型是否真的有不同目标。如果只是为了显得架构复杂而拆 CQRS，通常会把简单问题做难。

**典型适用场景一：复杂领域模型，写入侧需要强业务规则。**

例如：

- 订单预订。
- 账户扣款。
- 工作流调度。
- actor command 顺序执行。
- 库存占用。
- 多步骤审批。

这些系统的写操作不是简单 `UPDATE status = ?`。命令需要表达业务意图：

```text
BookRoom
ReserveInventory
CompleteTask
CancelWorkflow
ApplyActorCommand
```

命令处理器要校验不变量，比如余额不能为负、同一个 actor command sequence 不能跳号、任务完成必须带正确 lease epoch。CQRS 可以让写模型专注这些规则，不被查询字段污染。

**典型适用场景二：读写负载差异很大。**

如果读远多于写：

```text
read: dashboard/list/search/detail
write: submit/complete/cancel
```

读模型可以单独扩展。比如读侧用：

- denormalized SQL table。
- Redis/KV。
- Elasticsearch/OpenSearch。
- 内存 metadata view。
- OLAP/reporting table。

写侧仍然用事务表或 event log 保证业务规则。

**典型适用场景三：查询形状和写入形状差异很大。**

写模型可能是：

```text
Workflow aggregate
Step aggregate
Task aggregate
Worker lease
Actor state
```

页面需要的是：

```text
workflow_id
status
completed_steps
failed_steps
latest_error
running_worker
last_result_ref
```

如果每次查询都临时 join 或 replay，查询会很重。CQRS 允许读侧维护 `workflow_summary`、`task_list_view`、`actor_current_state` 之类的模型。

**典型适用场景四：task-based UI。**

Microsoft 文档特别提到 task-based user interface。意思是 UI 不是直接暴露数据库字段，而是引导用户完成一个业务任务：

```text
不是：
  SetReservationStatus(reserved)

而是：
  BookHotelRoom(customer, room_type, date_range)
```

这种命令可以携带业务上下文，并让后端给出更清楚的校验和冲突处理。

**典型适用场景五：多人并发协作，冲突需要业务化处理。**

比如多个用户同时修改同一资源。CRUD 式更新很容易变成最后写入者覆盖。CQRS 可以把冲突处理放到命令逻辑里：

```text
ApproveDocument
RequestChange
AssignTask
ClaimJob
```

命令处理器可以按业务规则决定接受、拒绝、排队、合并或补偿。

**典型适用场景六：和 event sourcing 配合。**

Event sourcing 里，写模型保存事件：

```text
TaskSubmitted
TaskStarted
TaskCompleted
```

读模型从事件投影：

```text
task_current_state
workflow_summary
worker_load
```

Microsoft 文档把 event store 作为 write model 的 source of truth，把 read model 作为由事件生成的 materialized views。这个组合很常见，但要注意：CQRS 不要求 event sourcing，event sourcing 也不必然要求把所有 API 都做成复杂 CQRS。

**典型适用场景七：读权限和写权限明显不同。**

读模型可以只暴露用户能看的字段，命令接口可以单独做授权。比如普通用户能看任务状态，但不能修改 worker lease；管理员能 cancel workflow，但不能直接改 event log。

**不适用场景一：简单 CRUD 已经足够。**

如果系统只是：

```text
create user
update profile
list products
delete tag
```

业务规则简单，查询也简单，那 CQRS 只会增加 handler、DTO、projection、同步、测试和部署成本。Microsoft 文档也把“领域或业务规则简单、简单 CRUD UI 足够”列为不适合的情况。

**不适用场景二：团队还没有处理 eventual consistency 的能力。**

CQRS 一旦拆开读写模型，就会出现：

```text
command succeeded
query stale
```

如果团队没有设计：

- read-your-writes。
- projection lag。
- retry。
- idempotency。
- outbox/inbox。
- drift detection。
- rebuild。

那线上用户会看到“提交成功但列表没有”“状态跳来跳去”“刷新后才出现”等问题。

**不适用场景三：强一致读是主需求。**

比如交易余额、权限判断、库存扣减、对象 GC 决策。如果读到旧数据会直接造成资金、安全或数据丢失问题，就不能随便用异步 read model 做决策。可以仍然使用 command/query 接口分离，但关键决策要读 source of truth 或同事务维护的状态。

**不适用场景四：只有少数复杂报表。**

如果主系统大多数查询都很简单，只有几个报表慢，不一定需要全系统 CQRS。Fowler 提醒过，可以用 reporting database 解决少数重查询，而不是把所有查询都切到单独模型。

**不适用场景五：组织边界不支持。**

CQRS 带来更多代码和运维责任：

```text
command handler
query handler
read model
projection worker
message retry
checkpoint
lag metrics
schema migration
```

如果团队规模小、发布频率低、业务变化少，直接 CRUD 反而更稳。

面试里可以这样回答：

```text
CQRS 适合复杂领域、task-based UI、读写负载差异大、查询模型和写模型差异大、多人并发冲突需要业务化处理、以及 event sourcing/read model 场景。不适合简单 CRUD、强一致读优先、团队没有处理 eventual consistency 的能力、只有少数复杂报表、或者业务规则很薄的系统。CQRS 要用在读写职责真的分裂的地方，而不是把普通 CRUD 包装成一套复杂架构。
```

## Q043. CQRS 和相近概念最容易混淆的边界在哪里？

CQRS 这个词容易被泛化。很多系统只是做了读写接口分离、主从复制、缓存、事件投递，就说自己是 CQRS。面试里要把边界讲清楚。

**第一，CQRS 和 CQS 的边界。**

CQS 是 Command Query Separation，常见于对象或函数设计：

```text
command: 改变状态，不返回业务查询结果
query: 不改变状态，返回结果
```

CQRS 是架构模式，强调读写可以使用不同模型：

```text
command model != query model
```

CQS 可以发生在一个类的方法级别；CQRS 通常发生在应用服务、领域模型、存储模型甚至部署架构级别。二者思想相关，但层级不同。

**第二，CQRS 和 CRUD 的边界。**

CRUD 把数据看成资源记录：

```text
CreateTask
ReadTask
UpdateTask
DeleteTask
```

CQRS 更关注业务意图：

```text
SubmitTask
LeaseTask
CompleteTask
CancelTask
RequeueExpiredTask
```

CRUD 不一定错。简单系统用 CRUD 很好。CQRS 的价值在于写操作有明确业务语义，读模型又和写模型不一样。

**第三，CQRS 和读写库分离的边界。**

读写库分离通常是数据库复制架构：

```text
primary handles writes
replica handles reads
schema mostly same
```

CQRS 是模型分离：

```text
write model optimized for commands
read model optimized for queries
schema may be completely different
```

一个系统可以用 CQRS 但仍然只有一个数据库；也可以有主从复制但不是 CQRS。Microsoft 文档也说明，CQRS 可以先在同一个 underlying database 里分离 read/write model，再进一步发展成不同 data stores。

**第四，CQRS 和 event sourcing 的边界。**

CQRS 不要求 event sourcing。写侧可以是：

```text
SQL current-state table
document store
append-only event log
```

Event sourcing 也不必然意味着外部 API 都是 CQRS。它只是把状态保存为事件序列。两者经常组合，是因为 event log 很适合作为 write model，read model 可以从事件投影出来。

边界可以这样记：

```text
CQRS: command model 和 query model 分开
Event Sourcing: state 由事件历史表示
```

**第五，CQRS 和 materialized view 的边界。**

CQRS 的 query side 可以使用 materialized view，但 materialized view 不是 CQRS 本身。

```text
CQRS:
  architectural separation of command/query models

materialized view:
  concrete derived read structure
```

没有 CQRS 也可以建物化视图，比如给单体 CRUD 系统做报表加速。CQRS 也可以先不建异步物化视图，比如 command/query 分离但读侧直接查同一个数据库。

**第六，CQRS 和 microservices 的边界。**

CQRS 不等于微服务。单体可以 CQRS：

```text
same process
same database
separate command handlers and query handlers
```

微服务也可以不用 CQRS。把每个操作拆成一个服务，不会自动产生清晰的 command/query 模型。反过来，过早把 CQRS 和微服务绑定，通常会把一致性、事务和调试成本放大。

**第七，CQRS 和异步消息的边界。**

很多 CQRS 系统会用消息队列处理命令或更新 read model，但消息队列不是 CQRS 的必要条件。

```text
synchronous CQRS:
  command handler updates DB
  query handler reads read model in same DB

asynchronous CQRS:
  command handler persists event
  projector updates read model later
```

异步消息引入的是 retry、duplicate、ordering、dead-letter、lag。它能扩展系统，也会增加故障模式。

**第八，CQRS 和 API 分层的边界。**

有些系统只是把接口命名成：

```text
CommandController
QueryController
```

但内部仍然是同一个模型、同一套 service、同一堆 CRUD update。这不算真正有价值的 CQRS，只是命名变化。CQRS 的实质要看：

- 写侧是否表达业务意图。
- 写侧是否拥有自己的不变量。
- 读侧是否按查询优化。
- 读写同步语义是否明确。

**第九，CQRS 和权限拆分的边界。**

CQRS 可以帮助权限拆分：

```text
read permission != write permission
```

但这不是完整的 access control。查询接口仍然要做字段过滤和租户隔离，命令接口仍然要做认证、授权和审计。

面试里可以这样回答：

```text
CQRS 容易和 CQS、CRUD、读写库分离、event sourcing、materialized view、microservices、异步消息混淆。CQRS 的边界是 command model 和 query model 分开，不是必须分库，不是必须上消息队列，不是必须 event sourcing，也不是简单把 API 改名。event sourcing 解决状态如何保存，materialized view 是 read model 的一种实现，读写库分离是复制拓扑，CQRS 是模型和职责分离。
```

## Q044. CQRS 在高并发场景下可能出现哪些隐藏问题？

CQRS 在高并发下的风险，来自两边：write side 的命令冲突和 query side 的滞后。系统拆开以后，每一边可以优化，但两边之间的缝也会变成 bug 来源。

**第一，重复命令导致重复写入。**

客户端超时后重试：

```text
SubmitTask(idempotency_key=k1)
timeout
SubmitTask(idempotency_key=k1)
```

如果 command handler 没有幂等处理，可能创建两个任务、追加两条事件、调度两次 workflow。高并发下，重复通常不是用户手抖，而是网络超时、gateway retry、队列 redelivery、worker 重启造成的。

LogServe 在 task、workflow、actor 上使用 idempotency key 和 fingerprint，是这个问题的具体处理方式：同一个 key、同一个 payload 可以复用结果；同一个 key、不同 payload 要拒绝。

**第二，基于 stale read 发出的 command 会违反用户预期。**

典型流程：

```text
GET /tasks/t1 -> status=running  (read model 落后)
POST /tasks/t1/cancel
```

但 write model 里任务可能已经 completed。命令处理器不能相信 read model 的状态，必须重新检查 source of truth 或 aggregate 当前版本。否则 CQRS 会把 UI 的旧状态变成错误写入。

**第三，read-your-writes 体验问题在高并发下会放大。**

用户提交成功后立刻查询：

```text
POST /workflows -> 200 OK, workflow_id=w1
GET /workflows/w1 -> 404
```

这不一定是数据丢失，可能只是 projection lag。但用户看起来就是失败。并发越高，lag 越容易抖动。解决方式包括：

- 命令返回 version / offset。
- 查询支持 `min_version`。
- gateway 等待 read model 追到某个 checkpoint。
- 对未追上的查询返回 pending。
- 对关键查询直接读 write model。

**第四，热点 aggregate 会限制写侧吞吐。**

CQRS 常把写侧按 aggregate 串行化：

```text
actor:actor-1 command seq 1,2,3...
workflow:w1 step updates
```

这能保证不变量，但热点实体会成为瓶颈。比如大量请求都打到同一个 actor，不能靠简单加 projector 解决，因为同一 actor 的 command 顺序必须保留。LogServe 的 actor command sequence 就是这种边界：乱序 completion 被拒绝是正确的，但热点 actor 的吞吐也会受限。

**第五，命令队列重排会破坏业务顺序。**

如果命令经过队列：

```text
CancelTask
CompleteTask
```

实际处理顺序可能反过来。系统要定义：

- 同一 aggregate 的命令是否必须有序。
- 命令是否带 expected version。
- 乱序命令是等待、拒绝还是补偿。

不能只依赖消息系统“通常有序”。一旦分区、重试、死信重放，顺序假设就会暴露。

**第六，read model 多投影之间短暂不一致。**

一个命令可能影响多个读模型：

```text
CompleteTask ->
  task_current_view
  workflow_summary_view
  worker_load_view
  daily_usage_view
```

高并发下，有的 view 更新快，有的慢。用户可能看到：

```text
task detail: completed
workflow page: still running
worker page: still busy
```

这类不一致要么被业务接受，要么通过事务、同批 checkpoint、UI freshness 标记来处理。

**第七，锁竞争从数据库转移到 command handler 或 projection。**

拆 CQRS 后，原来一个 CRUD 表上的锁竞争可能变成：

- command handler 抢 aggregate lock。
- idempotency 表唯一索引冲突。
- outbox 表高频写入。
- projection checkpoint 单行热点。
- read model upsert 热点。

如果只看主业务表锁下降，会误以为系统变好了；实际 p99 可能被 idempotency、outbox 或 projector 卡住。

**第八，异步 read model lag 触发级联重试。**

服务 A 写入成功，服务 B 立刻查询 read model 没查到，于是重试。高并发下这会造成：

```text
projection lag -> query miss -> retry storm -> read DB load increases -> projection slower -> lag larger
```

这种反馈环很常见。解决方式是让调用方理解 command result 的语义，不要用“立刻查询不到”判断写失败。

**第九，权限和审计视图滞后可能造成安全窗口。**

如果用户权限变更先写 write model，read model 过几秒才更新，那么这几秒里旧权限可能仍然可见。普通业务列表的 stale read 可以接受，权限判断的 stale read 不能随便接受。

**第十，高并发下观测更难。**

CQRS 出问题时，不能只看 API error rate。还要看：

```text
command_accept_rate
command_reject_rate
idempotency_conflict_total
write_model_latency_p99
outbox_lag
projection_lag
read_model_staleness
read_your_writes_wait_ms
dead_letter_events
```

否则只会看到“用户说状态不对”，却不知道问题在 command、event、projection 还是 query。

面试里可以这样回答：

```text
CQRS 高并发下的隐藏问题包括重复命令、stale read 驱动错误 command、read-your-writes 失败、热点 aggregate、命令队列乱序、多个 read model 短暂不一致、idempotency/outbox/checkpoint 锁竞争、projection lag 引发重试风暴，以及权限视图滞后的安全窗口。写侧要靠 idempotency、expected version、aggregate 顺序和业务校验；读侧要靠 lag 指标、freshness token、必要时回读 source of truth。
```

## Q045. CQRS 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

CQRS 的正常路径很清楚：command 写入，query 读取。真正考验设计的是 command 执行到一半崩溃、客户端超时、消息重复投递、服务重启后 read model 落后这些情况。

**第一，command 已接收但还没持久化。**

流程：

```text
client -> command handler
handler validates command
process crashes before write
```

客户端可能看到连接断开或超时。此时系统没有事实记录，重试应该可以重新执行。问题是：客户端不知道命令是否已经成功，所以仍然需要 idempotency key。没有 idempotency key，无法区分“第一次没写成”和“第一次写成了但响应丢了”。

**第二，write model 已更新，但响应还没返回。**

流程：

```text
append event / update DB succeeded
process crashes before response
client retries
```

这就是 timeout unknown。重试必须返回同一个结果，或者明确拒绝不同 payload。LogServe 的 `GetTaskByIdempotencyKey`、`GetWorkflowByIdempotencyKey`、`GetActorByIdempotencyKey` 就服务这个边界。

**第三，write model 已更新，但 read model 还没更新。**

流程：

```text
command writes event seq=100
projector is still at seq=95
query reads old view
```

这不是崩溃才会发生，正常异步也会发生。崩溃重启后更明显，因为 projector 要从 checkpoint 追赶。系统要定义命令返回后查询的语义：

- 是否保证 read-your-writes。
- 是否返回 accepted 而不是 completed。
- 是否提供 operation id / event offset。
- 查询是否能等待 projection catch up。

**第四，write model 和 outbox/event publish 之间的双写边界。**

如果 CQRS 读模型靠事件更新，写侧常见流程是：

```text
1. update write DB
2. publish event
```

如果 1 成功、2 失败，read model 永远不知道更新。反过来如果先 publish event、再 update DB，event 可能描述了未提交的事实。常见解决方案是 transactional outbox：

```text
same transaction:
  update aggregate
  insert outbox event

separate relay:
  publish outbox event
  mark published
```

然后消费者用 inbox/idempotency 处理重复事件。

**第五，command handler 产生外部副作用的边界。**

如果 command handler 一边写库，一边发邮件、扣款、调用第三方：

```text
charge card succeeded
DB commit failed
```

或：

```text
DB commit succeeded
send email timeout
retry command
send email twice
```

CQRS 本身不解决这个问题。要把外部副作用设计成可幂等、可补偿，或者通过可靠事件驱动的 side-effect handler 处理。

**第六，重启后 command queue redelivery。**

如果命令通过队列异步执行，worker 崩溃后消息可能重新投递：

```text
message delivered
handler writes event
handler crashes before ack
message redelivered
```

这要求 command handler 幂等。ack 之前崩溃和 ack 之后崩溃，对系统含义完全不同。不要假设队列只投递一次。

**第七，projection checkpoint 和 read model 更新的边界。**

CQRS 常见读侧恢复逻辑：

```text
read events from checkpoint+1
apply to read model
advance checkpoint
```

如果 checkpoint 先推进、read model 后更新，崩溃会丢事件。如果 read model 先更新、checkpoint 后推进，崩溃会重复事件。后者可以靠幂等处理，前者更危险。最好把 read model mutation 和 checkpoint 放在同一事务里。

**第八，schema/version 变化后的重启边界。**

部署新版本后：

```text
old command format
new command handler
old read model schema
new projector
```

如果没有版本兼容，重启可能导致历史命令无法重放、旧事件无法投影、新查询读不到字段。CQRS 里模型多，schema migration 的面也更大：

- command contract。
- event contract。
- write model schema。
- read model schema。
- projection code。
- API response DTO。

**第九，超时重试可能破坏用户意图。**

有些命令不是天然可重复的：

```text
IncreaseQuota(+10)
TransferMoney(100)
SubmitActorCommand(next)
```

同一个命令重复执行会改变结果。要么命令带唯一 command id，要么用业务 idempotency key，要么改成 set-style 命令：

```text
SetQuota(to=100)
```

但不是所有业务都能改成 set-style，所以 idempotency 是基本要求。

**第十，LogServe 的具体边界。**

LogServe 的写路径通常是 log-first：先 append record，再更新 metadata。这个顺序的含义是：

```text
append succeeded, metadata failed:
  bootstrap/replay 可以恢复 metadata

metadata succeeded, response lost:
  idempotency key 可以让 retry 找回已有结果
```

Actor command 还多了 sequence 边界：completion 必须匹配下一个 command seq，否则会拒绝 stale 或 out-of-order completion。这是 CQRS write side 不变量的一个具体例子。

面试里可以这样回答：

```text
CQRS 在故障场景下主要暴露 command 持久化前后、响应返回前后、write model 和 read model 同步、outbox 发布、queue ack、projection checkpoint、外部副作用和 schema version 的边界。timeout 不能当成失败，只能当成 unknown，所以 command 要有 idempotency key。读模型恢复要保证 view update 和 checkpoint 的顺序安全。关键副作用要可幂等或可补偿，不能因为 command retry 重复发生。
```

## Q046. CQRS 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

CQRS 的性能瓶颈要分 command side 和 query side 看。它不是天然更快，而是允许把不同瓶颈隔离开来。

```text
command side:
  validation -> aggregate load -> business logic -> persist -> publish

query side:
  read model lookup -> filter/sort/page -> serialize response

projection:
  consume events -> update read model -> checkpoint
```

**第一，command side 的瓶颈常来自锁竞争和 I/O。**

写侧要保证业务不变量，常常需要：

- 读取 aggregate 当前状态。
- 检查 expected version。
- 写事务。
- 写 outbox。
- 写 event log。
- 更新 idempotency record。

高并发下，瓶颈可能是：

```text
aggregate row lock
unique index on idempotency key
event log append fsync
outbox table insert
transaction commit latency
```

如果同一个 workflow、actor、账户、库存项被大量命令访问，锁竞争会比 CPU 更早出现。

**第二，query side 的瓶颈常来自 read model 设计和索引。**

CQRS 让 query side 有机会变快，但前提是 read model 真按查询设计。否则只是把慢查询从主表搬到了读库。

常见瓶颈：

- 分页没有稳定排序键。
- 过滤字段没有索引。
- read model 太宽，序列化成本高。
- 搜索索引更新滞后。
- dashboard 聚合每次现算。
- N+1 查询。
- 大对象字段被读接口顺手返回。

读模型越接近 UI 需要的形状，查询越简单；但写放大也越明显。

**第三，projection 的瓶颈可能是 CPU、I/O、锁和网络混合。**

投影链路包括：

```text
read event
decode payload
apply reducer
write read model
commit checkpoint
```

CPU 消耗来自反序列化、schema upcast、聚合计算。I/O 消耗来自读 event log、写 read DB、checkpoint fsync。锁竞争来自同一 view row、同一 checkpoint row、同一 aggregate key。网络消耗来自跨进程 event broker、远程数据库、搜索引擎或跨区域复制。

如果 projection 慢，CQRS 的读侧会 stale。它不一定让 API error rate 上升，但会让用户看到旧状态。

**第四，内存瓶颈常出现在 read model cache、批处理和重建。**

CQRS 系统经常为了提升读性能，引入：

- 内存 read model。
- projection batch buffer。
- idempotency cache。
- query result cache。
- rebuild shadow view。

内存问题的线上表现可能是 GC pause、p99 抖动、projection stop-the-world、重建时 OOM。特别是高基数字段，比如 tenant、user、workflow、model、day 的组合，容易让聚合 view 膨胀。

**第五，网络瓶颈来自拆分后的 hop 增加。**

简单 CRUD 可能只有：

```text
API -> DB
```

CQRS 可能变成：

```text
API -> command service -> write DB -> outbox -> broker -> projector -> read DB -> query service
```

每个 hop 都有延迟、重试和限流。读写分离后，局部组件更容易扩展，但端到端链路更长。跨区域时，network latency 会直接变成 projection lag 和 read staleness。

**第六，CQRS 的性能收益常常来自避免错误的共享。**

比如传统模型里，复杂报表和写事务共用一个数据库：

```text
long-running report query
blocks or slows write transaction
```

CQRS 可以把报表放到 read model 或 OLAP 存储，写侧不被报表拖慢。这个收益不是“CQRS 代码更快”，而是资源隔离和模型优化。

**第七，LogServe 里的可能瓶颈。**

LogServe 当前更接近单机机制验证：

- command side：append logstore record、更新 metadata store、检查 idempotency。
- query side：读 metadata store。
- bootstrap：从 logstore replay 到 metadata。

小规模下，瓶颈可能是 map 锁、序列化和 log I/O；大规模下，可能是：

- metadata store 锁竞争。
- logstore append/Read 扫描。
- bootstrap replay 时间。
- idempotency map/table 增长。
- actor 热点 command sequence。

如果切到 PostgreSQL metadata，瓶颈还会转向 SQL upsert、索引、事务提交和连接池。

**第八，性能评估不能只看平均值。**

CQRS 系统要看：

```text
command_latency_p99
command_conflict_rate
write_db_commit_ms
outbox_publish_lag
projection_apply_lag
read_model_query_p99
read_model_staleness_ms
read_your_writes_wait_ms
```

平均 command latency 低，但 projection lag 高，用户仍然会觉得系统慢。读 p99 低，但写侧冲突率高，命令仍然失败。

面试里可以这样回答：

```text
CQRS 的瓶颈要分写侧、读侧和投影链路。写侧常见瓶颈是 aggregate 锁、事务提交、event log/outbox I/O、idempotency 唯一索引；读侧瓶颈是 read model schema、索引、分页、序列化和缓存；投影瓶颈可能是事件解码 CPU、read model 写入 I/O、checkpoint 锁、远程 broker/DB 网络。CQRS 的性能收益来自读写隔离和独立优化，不是拆开以后天然更快。
```

## Q047. CQRS 的 correctness test、stress test 和 benchmark 应该分别测什么？

测试 CQRS 时，最容易犯的错是只测 command API 成功返回。CQRS 有两条路径：写路径和读路径；还有一条把两者连起来的同步路径。三者都要测。

```text
command correctness
query correctness
projection/synchronization correctness
```

**第一，correctness test 要测命令不变量。**

命令侧测试重点不是字段有没有更新，而是业务规则有没有守住：

```text
CompleteTask requires:
  task exists
  status is running
  lease_epoch matches
  result reference valid
  command idempotent
```

测试用例应覆盖：

- 合法命令成功。
- 非法状态转换被拒绝。
- stale version / lease 被拒绝。
- 同一 idempotency key + 同一 payload 返回同一结果。
- 同一 idempotency key + 不同 payload 被拒绝。
- 并发命令只有一个成功。
- 命令失败不产生半写状态。

LogServe 已有 idempotency 相关测试，这类测试就属于 CQRS command side correctness。

**第二，correctness test 要测查询不修改状态。**

query side 的基本不变量：

```text
query does not mutate write model
query does not advance workflow
query does not acquire task lease
query does not publish event
```

读接口可以更新缓存或访问统计，但不能改变业务状态。测试可以记录 event log seq、metadata version、DB transaction count，确认查询前后业务事实不变。

**第三，correctness test 要测 read model 与 write model 的关系。**

如果 command 成功后异步更新 read model，要测试：

```text
given command result at version V
eventually query model reflects version V
```

测试不一定要求立即一致，但要定义最终一致窗口：

- projector 处理到 offset 后，view 是否正确。
- 重复事件是否不改变结果。
- 乱序事件是否被拒绝或暂存。
- projection crash 后是否能恢复。
- view rebuild 是否等于从 write model 重新计算的结果。

**第四，correctness test 要测 stale read 驱动 command 的处理。**

场景：

```text
read model says task=running
write model already completed
client sends CancelTask based on stale read
```

正确行为不是盲目 cancel，而是在 command side 重新校验并拒绝或返回 already completed。这个测试很重要，因为 CQRS 的很多线上问题来自 stale read。

**第五，stress test 要测高并发冲突和恢复。**

Stress test 可以生成：

- 多客户端同时提交同一个 idempotency key。
- 多客户端不同 key 提交同一 aggregate 的命令。
- 多 worker 同时完成同一个 task。
- 同一个 actor 大量 command。
- command timeout 后快速 retry。
- projector 随机 crash/restart。
- outbox relay 重复 publish。
- read model DB 随机变慢。

持续检查：

```text
no duplicate task
no negative running count
no out-of-order actor command applied
no completed task goes back to running
projection lag eventually returns to zero
read model can be rebuilt to same checksum
```

**第六，stress test 要测 backpressure。**

CQRS 系统常在 projection 慢时出问题。测试应该让 read model 写入变慢：

```text
projection apply rate < command event rate
```

观察：

- command 是否继续无限接收。
- outbox 是否无限增长。
- read model lag 是否暴露。
- query 是否返回 stale 标记。
- 系统是否进入降级或限流。

如果没有 backpressure，最终会把队列、磁盘或 read DB 打满。

**第七，benchmark 要分别测 command、query、projection。**

Command benchmark：

```text
commands/sec
command p50/p95/p99
conflict rate
idempotency lookup latency
write transaction latency
event append latency
```

Query benchmark：

```text
queries/sec
query p50/p95/p99
page size impact
filter/sort impact
serialization bytes
cache hit rate
```

Projection benchmark：

```text
events/sec
event-to-view-visible latency
checkpoint latency
lag under load
catch-up time after outage
rebuild time from zero
```

**第八，benchmark 要对比非 CQRS 基线。**

如果没有基线，很难证明 CQRS 值得。至少要对比：

```text
CRUD direct query p99
vs
CQRS read model query p99

CRUD write p99
vs
CQRS command + outbox p99
```

有时 CQRS 让查询快了 10 倍，但写入慢了 2 倍，这可能值得；有时查询只快了 10%，却多了大量同步复杂度，那就不值得。

**第九，测试要覆盖用户体验语义。**

技术上 command succeeded、read eventually consistent 是对的，但用户体验可能不接受。可以测：

- command 返回后立即查询。
- query 带 `min_version` 等待。
- 等待超时返回什么。
- 刷新页面是否看到状态倒退。
- UI 是否能显示 pending。

面试里可以这样回答：

```text
CQRS 的 correctness test 要测 command 不变量、idempotency、状态转换、query 不修改状态、read model 最终与 write model 一致、stale read 发出的 command 会被写侧重新校验。stress test 要测并发命令冲突、重复投递、超时重试、projector crash、outbox 重放、read model 变慢和 backpressure。benchmark 要分 command、query、projection 三条链路，测吞吐、p99、冲突率、lag、rebuild/catch-up time，并和非 CQRS 基线对比。
```

## Q048. 如果要求从零实现一个简化版 CQRS，你会先定义哪些不变量？

从零实现 CQRS，不应该先画“命令服务、查询服务、消息队列、读库”这张图。先定义不变量。否则系统看起来分层很漂亮，实际一遇到重试和并发就乱。

**不变量一：command 表示业务意图，不是裸字段更新。**

命令应该像这样：

```text
SubmitTask
CompleteTask
CancelWorkflow
ApplyActorCommand
RegisterWorkerHeartbeat
```

而不是：

```text
UpdateTaskStatus
SetWorkflowField
PatchActorState
```

不是说永远不能 patch，而是核心写操作要表达业务语义。这样 command handler 才知道要校验哪些不变量。

**不变量二：query 不改变业务状态。**

所有查询都必须满足：

```text
before_query_business_state == after_query_business_state
```

查询可以读缓存，可以记录访问日志，但不能：

- 分配任务。
- 推进 workflow。
- 更新 lease。
- 触发补偿。
- 追加业务事件。

如果一个接口会改变状态，它就是 command，不要藏在 query 里。

**不变量三：write model 是权威状态。**

简化版要先定义 source of truth：

```text
write DB current state
or
event log
or
metadata table + append-only records
```

read model 不能成为唯一事实来源。读模型丢失后应该能恢复，或者至少能从 write model 重新生成。

**不变量四：每个 command 有唯一 identity。**

命令必须能处理 retry：

```text
command_id / idempotency_key
payload_fingerprint
```

同一个 key、同一个 payload：

```text
return previous result
```

同一个 key、不同 payload：

```text
reject conflict
```

这比“客户端不要重复提交”可靠得多。

**不变量五：command handler 在一个明确的一致性边界内提交。**

要定义什么东西必须一起成功：

```text
aggregate update
idempotency record
outbox event
```

如果使用 event sourcing：

```text
append event
record idempotency
```

如果使用状态表：

```text
update row with expected_version
insert outbox event
```

不能让 command 写了一半就返回成功。

**不变量六：command side 重新校验所有业务前置条件。**

即使客户端刚从 read model 读到状态，command handler 也不能信任它。所有关键条件要在 write model 上再校验：

```text
expected_version matches
lease_epoch matches
actor_command_seq == current_seq + 1
workflow step is schedulable
```

read model 只是提示，不是授权写入的证据。

**不变量七：read model 有 freshness 语义。**

如果 query side 使用异步 projection，要暴露或内部维护：

```text
source_latest_version
read_model_checkpoint
lag_events
lag_seconds
projection_version
```

这样才能实现 read-your-writes、调试 stale read、判断 projection 是否健康。

**不变量八：read model 更新是幂等的。**

事件或 outbox message 可能重复投递：

```text
apply(event)
apply(event)
```

结果应该等同一次。可以用：

- event id 去重。
- per-stream seq。
- projection checkpoint。
- natural key upsert。
- last_applied_version。

**不变量九：read model 更新和 checkpoint 顺序安全。**

定义：

```text
checkpoint = read model 已经 durable 反映到的位置
```

那么 checkpoint 不能早于 view update。更好的设计是：

```text
same transaction:
  update read model
  update projection checkpoint
```

如果无法同事务，宁愿重复应用，也不能跳过事件。

**不变量十：同一 aggregate 的并发写入要有冲突控制。**

简化版可以选择：

```text
single writer per aggregate
or
optimistic concurrency with expected_version
or
database row lock
```

但必须有一个明确选择。否则两个并发 command 都读到 version=1，各自写 version=2，就会 lost update。

**不变量十一：外部副作用不和 command retry 混在一起。**

如果 command 成功后要发通知或调用外部系统，简化版也应该用 outbox/inbox 或 side-effect idempotency：

```text
command commits fact
event handler performs side effect with idempotency key
```

不要在 command transaction 中间直接调用不可控外部系统。

**不变量十二：模型版本可演进。**

CQRS 至少有 command contract、event contract、read DTO、projection schema。要定义：

```text
command_version
event_version
read_model_version
projection_version
```

老命令、老事件、新读模型如何兼容，需要早一点规定。

**不变量十三：失败语义对 API 用户清楚。**

命令返回要区分：

```text
accepted: 已接收，异步处理中
committed: 已写入权威状态
rejected: 业务条件不满足
duplicate: 幂等命中
unknown: 客户端超时，不应由服务端主动返回
```

查询返回要说明是否可能 stale，或者提供版本信息。

**不变量十四：可以从 write model 重建 read model。**

如果 read model 是派生的，就要能：

```text
drop read model
rebuild from source
compare checksum
switch traffic
```

否则 read model 一旦坏了，只能手工修数据，CQRS 的边界就失控了。

面试里可以这样回答：

```text
我会先定义这些不变量：command 表示业务意图；query 不改变业务状态；write model 是 source of truth；每个 command 有 idempotency identity 和 payload fingerprint；command handler 在明确事务边界内提交；写侧必须重新校验业务条件；read model 有 checkpoint/freshness；projection 幂等；read model update 和 checkpoint 顺序安全；同一 aggregate 有并发控制；外部副作用通过 outbox/inbox 或幂等处理；contract/schema 有版本；API 区分 accepted、committed、rejected、duplicate；read model 可以重建。
```

## Q049. CQRS 的常见误用是什么，误用后通常会产生什么线上症状？

CQRS 的误用有一个共同点：只学到了“读写分开”的形状，没有定义读写之间的语义。线上症状通常不是系统直接崩溃，而是状态越来越难解释。

**误用一：简单 CRUD 系统强行 CQRS。**

表现：

```text
CreateUserCommand
UpdateUserNameCommand
DeleteTagCommand
UserQueryService
TagReadModel
```

但业务规则很薄，查询也不复杂。线上症状：

- 代码量翻倍。
- 新字段要改 command、event、projection、DTO 多处。
- 开发速度变慢。
- 测试成本上升。
- 团队绕过架构直接改库。

这是 Fowler 警告的典型情况：不合适的领域套 CQRS，会增加风险。

**误用二：把 CQRS 等同于分库、消息队列和微服务。**

表现：

```text
为了 CQRS，上 broker、读库、写库、多个服务。
```

但系统没有处理重复消息、outbox、inbox、lag、dead-letter。线上症状：

- command 成功但 read model 永远不更新。
- 消息重复导致计数膨胀。
- 服务间排查困难。
- 本地开发和测试变复杂。
- 网络抖动造成状态不一致。

CQRS 可以先在单体内做模型分离，不必一开始就分布式化。

**误用三：query 里偷偷修改状态。**

例如：

```text
GetNextTask()
```

看起来是查询，实际分配任务、更新 lease。线上症状：

- 读接口无法缓存。
- 重试查询产生副作用。
- 监控或爬虫触发状态变化。
- 权限模型混乱。

如果接口会改变业务状态，就应该按 command 设计。

**误用四：command 只是 CRUD update 的薄包装。**

表现：

```text
SetStatus("completed")
SetOwner("worker-1")
PatchWorkflowField(...)
```

线上症状：

- 非法状态进入系统。
- 状态机被多个入口绕过。
- 审计日志看不懂用户意图。
- 补偿和重放无法判断业务含义。

更好的命令是 `CompleteTask`、`LeaseTask`、`StartWorkflowStep`，让业务语义进入 command handler。

**误用五：读模型被当成强一致事实使用。**

表现：

```text
read model says inventory=1 -> accept order
read model says permission=admin -> allow delete
read model says object unreferenced -> GC delete
```

线上症状：

- 超卖。
- 权限撤销后仍能访问。
- 对象被误删。
- workflow 被重复调度。

如果决策需要强一致，就要读 write model，或者让 read model 与 write model 在同一事务中更新，并明确一致性级别。

**误用六：没有 idempotency。**

表现：

```text
client timeout -> retry command -> duplicate side effect
```

线上症状：

- 重复任务。
- 重复扣款。
- 重复邮件。
- event log 出现语义重复。
- 用户刷新页面导致重复提交。

CQRS 的 command side 必须把重试当正常情况。

**误用七：只实现异步，不实现可观测性。**

表现：

```text
command -> event -> projector -> read model
```

但没有 lag、checkpoint、dead-letter、apply error、read staleness 指标。线上症状：

- 用户说“列表没更新”，后端不知道卡在哪里。
- 某个 projector 停了几小时才发现。
- read model 和 source 漂移。
- 重建不知道从哪里开始。

异步没有观测，就等于把问题藏起来。

**误用八：read model 无限膨胀。**

表现：

```text
每个页面一个 view
每个筛选条件一个 projection
每个报表一套存储
```

线上症状：

- 写放大。
- projection lag 上升。
- 存储成本失控。
- schema migration 很痛苦。
- 冷门 view 坏了没人知道。

读模型要按价值维护，不是越多越好。

**误用九：把 event sourcing 和 CQRS 绑定成一套不可分割的东西。**

表现：

```text
只要 CQRS 就必须 event sourcing。
只要 event sourcing 就必须全系统 CQRS。
```

线上症状：

- 为简单业务引入历史事件、upcaster、snapshot、projection。
- 事件设计不成熟导致 replay 失败。
- 开发者不知道事实、命令、DTO 的边界。

两者能组合，但要分别判断是否需要。

**误用十：忽略 UI 体验。**

技术上 command accepted，read model eventually consistent，但 UI 没有 pending、刷新、stale 标记。线上症状：

- 用户重复点击提交。
- 页面显示提交成功又消失。
- 客服无法解释“为什么刚创建的 workflow 查不到”。
- 用户把 eventual consistency 理解成数据丢失。

CQRS 的用户体验要设计，不是后端说 eventual consistency 就完了。

面试里可以这样回答：

```text
CQRS 常见误用包括简单 CRUD 强行 CQRS、把 CQRS 等同于分库微服务消息队列、query 偷偷改状态、command 只是 CRUD patch、把 read model 当强一致事实、没有 idempotency、异步链路没有 lag/checkpoint/dead-letter 观测、read model 过度膨胀、强行绑定 event sourcing，以及忽略 read-your-writes 用户体验。线上症状是代码复杂、状态漂移、重复提交、读到旧数据、报表对不上、投影 lag 不可见、排查链路拉长。
```

## Q050. CQRS 在单机和分布式环境中的语义有什么差异？

CQRS 在单机里更多是代码和模型边界；到了分布式环境，它会变成一致性、消息语义、所有权和运维问题。两者不能混着讲。

**第一，单机 CQRS 可以先只是进程内模型分离。**

单机实现可以是：

```text
same process
same database
CommandService
QueryService
separate command model and query DTO
```

这时 CQRS 的价值主要是：

- command handler 集中业务规则。
- query handler 不带领域修改。
- 读 DTO 和写 aggregate 分开。
- 测试边界清楚。

不一定需要消息队列，也不一定需要读写分库。

**第二，单机可以更容易做到同步读写一致。**

如果 command handler 在一个事务内更新 write table 和 read table：

```text
BEGIN
  update write_model
  update read_model
COMMIT
```

那么 command 返回后 query 可以立刻读到。代价是写事务更重，read model 更新失败会影响 command。小系统或强一致场景可以接受这种设计。

**第三，单机异步 CQRS 也会有 eventual consistency。**

只要 read model 是异步更新，即使在同一台机器，也会出现：

```text
command committed
projector not caught up
query stale
```

所以“单机”不等于“强一致”。区别只是故障面更小，排查更容易。

**第四，分布式 CQRS 必须定义消息语义。**

分布式环境里，write side 和 read side 可能通过 broker 连接：

```text
command service -> write DB/outbox -> broker -> projector -> read DB
```

要明确：

- 消息至少一次还是至多一次。
- 是否可能重复。
- 是否可能乱序。
- 同一 aggregate 是否保序。
- 消费成功的 ack 点在哪里。
- dead-letter 后 read model 是否允许跳过。

多数工程系统应按 at-least-once 设计，然后用 idempotency 和 checkpoint 做 effectively-once projection。

**第五，分布式 CQRS 必须定义所有权和并发写入。**

多个 command service 实例同时处理命令：

```text
service-1 CompleteTask(t1)
service-2 CancelTask(t1)
```

需要：

- expected version。
- aggregate lock。
- compare-and-set。
- single writer partition。
- fencing token。

多个 projector 同时更新 read model，也需要 partition ownership。否则旧 owner 恢复后继续写，会造成 split-brain projection。

**第六，分布式 CQRS 的 read-your-writes 不是默认语义。**

用户命令发到 region A，查询打到 region B：

```text
POST /tasks -> region A offset 100
GET /tasks -> region B read model offset 93
```

这时查询看不到刚写入的数据。要提供 read-your-writes，需要：

- command response 返回 version/offset。
- query 带 `min_version`。
- session stickiness。
- 等待 projection catch up。
- 对 stale read 返回 409/202/pending。
- 关键读回源到 write model。

不设计这个语义，分布式 CQRS 会让用户看到“提交成功但查不到”。

**第七，分布式 CQRS 的一致性通常是分区级的。**

单机里可以用本地全序 log。分布式里更常见的是 per-partition order：

```text
partition task-1: seq 1..N
partition workflow-1: seq 1..M
```

跨 partition 查询很难保证同一时间点的一致快照。比如 workflow summary 处理到了 offset 100，task list 只处理到 offset 96。系统要接受这种短暂不一致，或者为关键查询设计强一致读。

**第八，分布式 CQRS 的重建和迁移要在线化。**

单机 read model 坏了，可以停机重建。分布式系统通常不能停：

```text
build read_model_v2
tail events
compare v1/v2
switch read traffic
retire v1
```

还要处理 backfill 对 broker、write DB、read DB 的压力。重建速度慢会影响新版本上线。

**第九，分布式 CQRS 的安全面更大。**

单机里 command/query 权限可能在一个进程内。分布式里多个服务、多个 read store、多个缓存都可能暴露数据：

- query service 是否做租户隔离。
- read model 是否包含敏感字段。
- search index 是否同步删除。
- 命令服务之间是否认证。
- event broker 里是否有敏感 payload。

安全策略要覆盖整个链路，而不是只看 command API。

**第十，LogServe 的语义边界。**

LogServe 当前可以描述为单机/多进程的 CQRS-like 设计：

```text
command side:
  submit/complete/workflow/actor operations
  append logstore record
  update metadata
  enforce idempotency and sequence rules

query side:
  read metadata current state
  list tasks/workflows/workers/actors

recovery:
  bootstrap metadata from logstore
```

如果要把它扩展成分布式 CQRS，需要补充：

- command partitioning。
- aggregate expected version。
- durable outbox/inbox。
- projection ownership。
- checkpoint table。
- fencing token。
- per-view lag。
- read-your-writes token。
- dead-letter repair。
- shadow rebuild。

这些不是术语升级，而是语义升级。

面试里可以这样回答：

```text
单机 CQRS 可以只是同进程内的 command/query 模型分离，甚至共享一个数据库；如果同步更新 read model，可以更容易提供 command 返回后的立即可读。分布式 CQRS 必须显式定义消息投递、重复、乱序、partition order、command ownership、projector ownership、checkpoint、read-your-writes、lag 和在线 rebuild。单机主要是代码边界和崩溃恢复；分布式还要处理网络、重试、split-brain、跨区域复制和多 read model 不一致。
```

## Q051. event sourcing 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

Event sourcing 的核心目标，是把“系统状态”保存为一串已经发生的事件，而不是只保存最新状态。当前状态可以从事件流重放出来：

```text
events:
  TaskSubmitted
  TaskStarted
  TaskCompleted

current state:
  task.status = completed
```

Microsoft 的 Event Sourcing pattern 文档把它描述为：不只在关系数据库里存当前状态，而是在 append-only store 中存储对象上发生过的完整动作序列。事件存储成为 system of record，可以用来 materialize domain objects。Fowler 的原始文章也强调，核心思想是捕获 application state 的所有变化，并能通过重放事件重建状态。

**第一，它首先解决的是历史和可恢复性问题。**

传统 current-state 存储只知道现在：

```text
task_id=t1, status=completed
```

但它不知道：

- 谁提交了任务。
- 任务什么时候开始。
- 哪个 worker 拿到了 lease。
- 中间是否失败过。
- 是否发生过 redelivery。
- result reference 是哪一步产生的。

Event sourcing 保存的是变化过程：

```text
TaskSubmitted(task_id=t1, idem=k1)
TaskStarted(task_id=t1, worker=w1, lease_epoch=1)
TaskRedelivered(task_id=t1, attempt=2)
TaskCompleted(task_id=t1, result_ref=...)
```

这让系统可以做审计、调试、重放、恢复和生成新的 read model。对复杂系统来说，这不是附加日志，而是状态本身的记录方式。

**第二，它服务正确性，但方式不是“自动正确”。**

Event sourcing 对正确性的帮助在于：

- 每次状态变化都有事实记录。
- 当前状态可以从事件流重建。
- 错误修改不能悄悄覆盖历史。
- 并发写入可以通过 stream version / expected revision 控制。
- 补偿操作可以以新事件表达，而不是改掉旧记录。

例如两个 worker 同时完成同一个 task：

```text
CompleteTask(task=t1, lease_epoch=1)
CompleteTask(task=t1, lease_epoch=1)
```

写侧应该基于当前事件流重建状态，校验 task 是否还处于 running、lease epoch 是否匹配，然后用乐观并发追加事件。如果 stream 已经被另一个完成事件推进，后一个 append 应该失败或转为幂等命中。

但 event sourcing 不会替你设计这些规则。事件流只保证“发生过什么被记录下来”；业务正确性仍然要靠 command handler、aggregate reducer、expected version、idempotency key 和 invariant tests。

**第三，它可以改善写入性能，但不能简单说它总是更快。**

Event sourcing 的写入通常是 append-only：

```text
append event to stream
```

相比反复 update 同一行，append-only 可以减少原地更新带来的行锁竞争。Microsoft 文档也提到，append-only 写入可以改善复杂系统的写性能，尤其是避免 update-in-place 的锁竞争。

不过读取当前状态可能变慢：

```text
load events 1..N
replay
derive state
```

如果 stream 很长，就要用 snapshot、materialized view、缓存、分区和归档来控制成本。写入变简单，读取和运维会变复杂。

**第四，它提升可维护性，但只在事件设计清楚时成立。**

好的事件表达业务事实：

```text
TaskSubmitted
TaskStarted
TaskCompleted
ReservationCanceled
ActorCommandApplied
```

差的事件只是字段变化：

```text
StatusChanged(status=completed)
FieldUpdated(name=status, value=completed)
```

前者能让人理解业务发生了什么，也能支持新的 projection。后者只是换了一种方式写 change log，历史语义很薄。Microsoft 文档也强调，事件应该捕获 business intent，不只是记录结果状态。

**第五，它可以帮助安全审计，但会带来隐私删除问题。**

Event sourcing 天然适合审计：

- 谁做了什么。
- 什么时候做的。
- 之前是什么状态。
- 后来发生了什么补偿。

但 append-only 和隐私删除存在冲突。比如 GDPR 删除请求要求删除个人数据，而事件流希望不可变。常见做法是：

- 事件中少放个人数据，只放引用。
- PII 放到可删除的外部 profile store。
- 对事件中的敏感字段做加密。
- 通过删除密钥做 crypto-shredding。

所以它能增强审计安全，但不能自动满足隐私合规。

**第六，结合 LogServe 的定位。**

LogServe 的 logstore 已经体现了 event sourcing 的核心机制：

```text
Record {
  StreamID
  Seq
  EventType
  IdempotencyKey
  Payload
  TimestampMs
  CRC32
}
```

任务、workflow、actor 通过 `TaskSubmitted`、`TaskStarted`、`TaskCompleted`、`StepScheduled`、`ActorCommandApplied`、`ActorSnapshotCreated` 等事件记录变化。metadata store 是当前状态 view，启动时可以 `BootstrapFromLog`，从 logstore 重新投影 metadata。

但要注意边界：LogServe 是单机/多进程机制验证系统，不是完整分布式 event sourcing 平台。它展示了 log-first control plane、replay、idempotency、actor snapshot 和 metadata projection 的核心思路，但还不能宣称具备跨节点 partition ownership、全局顺序、分布式 exactly-once projection 等生产语义。

面试里可以这样回答：

```text
event sourcing 的核心目标是把状态变化记录成 append-only 事件流，并把事件流作为 source of truth。当前状态、读模型、审计视图和调试视图都可以由事件重放生成。它主要解决历史可追溯、恢复、审计和复杂写入正确性建模问题，也可能提升 append-only 写入性能；代价是读取、投影、schema 演进、隐私删除和运维复杂度更高。它不是简单加一张操作日志，而是改变状态存储模型。
```

## Q052. event sourcing 的典型适用场景和不适用场景分别是什么？

Event sourcing 适不适合，要看业务是否真的需要历史、重放和事实语义。它不适合为了“架构高级”而全系统使用。

**典型适用场景一：必须保留完整业务历史。**

例如：

- 支付流水。
- 账户余额变化。
- 库存预占和释放。
- 订单状态流转。
- 工作流执行历史。
- 审批过程。
- 任务调度和重试历史。

这类系统只存当前状态是不够的。你需要回答：

```text
为什么余额变成 42？
任务为什么被重试？
workflow 哪一步失败？
库存是不是先预占后释放？
谁撤销了审批？
```

事件流能给出过程，而不是只给出结果。

**典型适用场景二：状态由一系列业务事实自然组成。**

如果领域专家本来就用事件说话：

```text
订单已创建
商品已加入
支付已确认
订单已发货
订单已取消
```

event sourcing 会比较自然。事件本身就是业务语言，command handler 产生事实，reducer 用事实推导状态。

如果领域只关心当前配置：

```text
feature_flag.enabled = true
model.timeout_ms = 30000
```

事件流价值就小得多。

**典型适用场景三：需要重建多个 read model。**

一个事件流可以投影出不同视图：

```text
task_current_state
workflow_summary
worker_load
daily_model_usage
audit_timeline
failure_analysis
```

需求变化时，可以从历史事件构建新 view，而不必从 current-state 表里猜历史。Microsoft 文档也把 materialized views 和 CQRS 作为 event sourcing 常见组合。

**典型适用场景四：并发写入同一实体时，需要乐观并发而不是大锁。**

Event sourcing 通常按 stream 追加事件：

```text
append to stream task:t1 expected_seq=5
```

如果另一个写入已经把 stream 推到 6，append 会失败，command handler 重新加载状态后再判断。这种方式适合把冲突显式暴露到业务层，而不是靠数据库 update 覆盖。

**典型适用场景五：需要可靠恢复和调试。**

如果线上出现错误，event sourcing 可以做：

```text
load events before bug
replay with old code
replay with fixed code
compare state
```

LogServe 里的 actor replay、workflow replay、bootstrap from log 都是这个思路：metadata 当前状态可以坏，可以重建；真正重要的是事件记录和 result reference。

**典型适用场景六：需要补偿而不是物理回滚。**

很多业务不能删除过去，只能追加修正事实：

```text
SeatsReserved
ReservationCanceled
PaymentCaptured
PaymentRefunded
```

事件流能保留“发生过、后来被抵消”的历史。Microsoft 文档也指出，撤销或修正通常通过 compensating event，而不是改掉历史事件。

**不适用场景一：简单 CRUD。**

比如：

- 用户昵称。
- 页面配置。
- 标签字典。
- 静态目录。
- 小型后台管理表。

如果当前状态足够，历史没有业务价值，event sourcing 会增加不必要的事件设计、replay、projection、schema evolution、运维和测试成本。

**不适用场景二：强实时查询当前状态，不能接受 projection lag。**

Event sourcing 常和 read model 搭配。只要 read model 异步更新，就会有 eventual consistency。如果业务不能接受“写入成功后读模型短暂落后”，就要特别谨慎。

可以仍然用 event sourcing 写侧，但关键读可能要：

- 直接 rehydrate stream。
- 同步更新 read model。
- 等待 projection catch up。
- 使用 session token。

如果这些成本不值得，就不要上 event sourcing。

**不适用场景三：团队没有事件建模和运维经验。**

Event sourcing 改变的不只是存储方式，还包括：

- command 测试方式。
- 事件版本管理。
- projection 失败恢复。
- snapshot 策略。
- 隐私删除策略。
- 重建和 backfill。
- 线上观测。

Microsoft 文档明确提醒，缺少 event-driven architecture 经验的团队采用 event sourcing，会增加反模式风险，而且迁入迁出成本都很高。

**不适用场景四：短生命周期原型或 MVP。**

事件一旦成为事实源，后面就要长期维护。一个快速试验项目如果很可能被重写，用 current-state 表更实际。事件流的 schema、upcaster、projection、replay 工具，短期内很难回本。

**不适用场景五：数据主要是静态参考数据。**

例如国家码、模型配置、枚举字典、产品目录。如果变化很少，直接版本化配置或普通表更简单。

**不适用场景六：隐私删除和合规压力极高，但没有隔离设计。**

如果事件里大量写入 PII、prompt、个人行为细节，而系统又无法删除或加密隔离，event sourcing 会让合规变难。先设计数据最小化、外部引用、密钥销毁，再考虑事件溯源。

面试里可以这样回答：

```text
event sourcing 适合需要完整历史、审计、恢复、补偿、多 read model 重建、复杂业务事实和并发冲突控制的领域，比如支付、库存、订单、工作流、任务调度。它不适合简单 CRUD、静态参考数据、短生命周期原型、强实时读优先、缺少事件建模经验的团队，或者没有隐私删除策略却要保存大量敏感数据的系统。它应该局部使用，用在历史价值高的边界上。
```

## Q053. event sourcing 和相近概念最容易混淆的边界在哪里？

Event sourcing 最容易和 audit log、message broker、CDC、event-driven architecture、CQRS、WAL、普通业务日志混淆。边界不清，系统会很快变成“看起来有事件，实际上无法重放”。

**第一，event sourcing 和 audit log 的边界。**

Audit log 记录操作痕迹，主要服务审计：

```text
user u1 called CompleteTask at 10:00
```

Event sourcing 的事件是状态来源：

```text
TaskCompleted(task_id=t1, result_ref=r1, lease_epoch=3)
```

它必须足够完整，能参与状态重建。很多 audit log 不能 replay，因为缺少必要字段、顺序、不变量或版本信息。反过来，event sourcing 的事件也能服务审计，但它的目标不止审计。

**第二，event sourcing 和 message broker 的边界。**

Kafka、RabbitMQ、NATS 这类 broker 适合分发消息，但不一定是 event store。Microsoft 文档特别提醒，不要把 event store 和 eventstream message broker 混淆；broker 通常缺少 per-entity stream query 和 optimistic concurrency。

Event store 需要支持：

```text
read stream by entity id
append with expected revision
preserve per-stream order
retain history as source of truth
support replay
```

Broker 更像分发层：

```text
fan out events to projections and consumers
```

有些系统可以把 Kafka 设计成事实源，但那需要明确 retention、compaction、keying、schema、replay、权限和恢复语义，不能默认等价。

**第三，event sourcing 和 CDC 的边界。**

CDC 捕获数据库变化：

```text
UPDATE tasks SET status='completed'
```

它记录的是存储层变化。Event sourcing 事件记录业务事实：

```text
TaskCompleted
```

CDC 可以用于同步 read model、搜索索引、缓存或数据仓库，但它通常不知道业务意图。`status` 从 running 变成 completed，是 worker 完成、管理员修复、超时补偿，还是数据修正？CDC 不一定知道。

**第四，event sourcing 和 event-driven architecture 的边界。**

Event-driven architecture 指组件通过事件通信：

```text
service A publishes event
service B reacts
```

这不等于 event sourcing。很多 event-driven 系统仍然把当前状态存主表，事件只是通知。Event sourcing 则把事件流当成 source of truth。

可以这样区分：

```text
event-driven:
  communication style

event sourcing:
  persistence model
```

**第五，event sourcing 和 CQRS 的边界。**

CQRS 是 command/query model 分离。Event sourcing 是状态存储为事件流。两者常组合：

```text
command side appends events
query side reads materialized views
```

但不是互相必需。你可以有 event sourcing 但查询直接 rehydrate stream；也可以有 CQRS 但写侧是普通 SQL current-state 表。

**第六，event sourcing 和 WAL 的边界。**

数据库 WAL 记录物理或逻辑变更，用于崩溃恢复和复制。Event sourcing 事件是业务语义：

```text
WAL:
  page/row changes

domain event:
  ReservationCanceled
  ActorCommandApplied
```

WAL 通常不是给业务代码直接重放的领域模型。它能恢复数据库，不等于能解释业务历史。

**第七，event sourcing 和普通业务日志的边界。**

普通日志可能是：

```text
logger.info("task completed")
```

它可能丢、可能采样、可能格式变化、可能不参与事务。Event sourcing 事件必须是持久化事实，写入成功与业务提交绑定。不能把日志系统当事实源。

**第八，event sourcing 和 snapshot 的边界。**

Snapshot 是优化，不是 source of truth。Microsoft 文档也明确说 snapshot 不是 eventstream 的替代。正确关系是：

```text
eventstream: source of truth
snapshot: cached state at seq=N
rehydrate: load snapshot + replay events after N
```

如果 snapshot 丢了，可以从 eventstream 重建；如果 eventstream 丢了，snapshot 不能证明完整历史。

**第九，event sourcing 和 materialized view 的边界。**

Materialized view 是事件流的派生读模型：

```text
eventstream -> projection -> read model
```

它可以丢弃并重建。把 read model 当 event store，会导致历史丢失；把 event store 当查询库，又会导致读取成本和耦合过高。

**第十，结合 LogServe 的边界。**

LogServe 的 logstore 更接近 event store：有 `StreamID`、`Seq`、`EventType`、`Payload`、`IdempotencyKey` 和 `CRC32`，并支持按 stream 读取。metadata store 更像 read model。worker 或 control plane 的普通日志不等价于 event store；它们可以用于排查，但不能作为 replay source。

面试里可以这样回答：

```text
event sourcing 和 audit log 的区别在于事件能重建状态，audit log 主要用于审计；和 message broker 的区别在于 event store 要支持 per-entity stream、顺序、历史保留和乐观并发，broker 更多是分发层；和 CDC 的区别在于 CDC 捕获存储变化，event sourcing 捕获业务事实；和 event-driven architecture 的区别在于后者是通信风格，前者是持久化模型；和 CQRS、snapshot、materialized view 都能组合，但边界分别是读写模型、重放优化和派生读模型。
```

## Q054. event sourcing 在高并发场景下可能出现哪些隐藏问题？

Event sourcing 在高并发下的难点，不是“能不能 append 事件”，而是多个 command 同时基于旧状态做决策、多个消费者重复处理事件、多个 projection 落后或漂移。

**第一，同一 stream 的并发 append 冲突。**

两个 command handler 同时加载同一个 stream：

```text
handler A reads stream task:t1 at seq=5
handler B reads stream task:t1 at seq=5

A appends TaskCompleted expected_seq=5
B appends TaskFailed expected_seq=5
```

如果 event store 不检查 expected revision，两个事件都可能写进去，状态语义混乱。正确做法是 append 时带 expected version；A 成功后，B 的 append 因 stream 已变化而失败，B 重新 rehydrate 后再判断。

Microsoft 文档在座位预订例子里也提到，event store 可以用 optimistic concurrency control，在 stream 已变化时拒绝追加。

**第二，跨 stream 不变量很难保证。**

单个 stream 的顺序容易保证，跨 stream 的规则难：

```text
order:o1
inventory:item-1
payment:p1
```

如果一个命令要同时影响多个 stream，可能出现部分成功。Event sourcing 不能自动提供跨实体事务。常见做法是：

- 把强一致不变量放在同一个 aggregate stream。
- 用 saga/process manager 管跨实体流程。
- 接受 eventual consistency。
- 用补偿事件修正。

**第三，热点 stream 会成为写入瓶颈。**

如果所有 command 都打到一个 stream：

```text
counter:global
tenant:big
actor:hot
```

expected revision 会让写入串行化。正确性没问题，但吞吐上不去。Actor 模型里同一 actor 的 command 必须有序，LogServe 的 actor command sequence 就是这个边界：它防止乱序应用，也限制热点 actor 的并发。

**第四，重复命令导致重复事件。**

客户端超时后重试：

```text
SubmitTask(idempotency_key=k1)
timeout
SubmitTask(idempotency_key=k1)
```

如果 event store 只按 stream seq 去重，不按 command id / idempotency key 去重，可能产生两个语义相同的事件。事件一旦写入，后续 projection、通知、调度都可能重复。

LogServe 的 logstore Append 支持按 `StreamID + IdempotencyKey` 返回 duplicate，这是处理重试的关键。

**第五，消费者 at-least-once 导致 projection 漂移。**

事件消费者可能重复收到同一事件：

```text
SeatsReserved(seq=10)
SeatsReserved(seq=10)
```

如果 projection 做：

```text
available_seats -= 2
```

就会重复扣减。Microsoft 文档明确要求 event handlers 幂等，并跟踪 last processed sequence number 或设计可重复执行的状态变更。

**第六，乱序消费导致状态倒退。**

即使 event store 中 per-stream 有序，分发到消费者时也可能乱序，特别是重试、并发消费、不同分区或 dead-letter 重放后。

```text
TaskCompleted(seq=5)
TaskStarted(seq=4)
```

如果 projection 后处理 seq=4，可能把 completed 改回 running。消费者要检查 stream seq、projection checkpoint 或业务版本。

**第七，projection lag 在高并发下会被误判成数据丢失。**

写入成功后，读模型还没追上：

```text
event store latest seq=10000
task_view checkpoint=9300
```

用户看到“提交成功但列表没有”，很容易重复提交。系统要暴露 lag、read-your-writes token、pending 状态，不能让调用方用 read model 是否可见判断 command 是否成功。

**第八，事件 schema 演进在并发部署时更复杂。**

多个版本的服务同时写事件：

```text
TaskSubmittedV1
TaskSubmittedV2
```

消费者也在滚动升级。若没有 tolerant deserialization、version 字段或 upcaster，projection 可能在高峰期突然卡住。

**第九，side effect 被重复触发。**

一个事件可能被多个 handler 处理：

```text
PaymentCaptured -> send email
PaymentCaptured -> update read model
PaymentCaptured -> call webhook
```

重复投递时，read model 可以幂等，但邮件、支付、webhook 也要有 idempotency。否则高并发重试会放大副作用。

**第十，事件循环。**

事件处理器收到 A 后发 B，另一个处理器收到 B 又发 A：

```text
TaskFailed -> WorkflowRetryScheduled
WorkflowRetryScheduled -> TaskSubmitted
TaskSubmitted -> TaskFailed
```

如果没有终止条件、attempt 上限和 causation/correlation 跟踪，会形成循环。Microsoft 文档也提醒要注意 circular logic。

面试里可以这样回答：

```text
event sourcing 高并发下的隐藏问题包括同一 stream append 冲突、跨 stream 不变量、热点 stream 串行化、重复命令产生重复事件、消费者 at-least-once 导致 projection 漂移、乱序消费、projection lag 被误判成写失败、schema 滚动升级卡住、外部副作用重复触发，以及事件处理循环。核心手段是 expected revision、idempotency key、per-stream sequence、幂等消费者、lag 暴露、saga/补偿和清晰的 aggregate 边界。
```

## Q055. event sourcing 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

Event sourcing 的价值之一是恢复，但只有边界设计对了，恢复才可靠。崩溃和重试会暴露“事件到底有没有写入”“消费者到底处理到哪里”“副作用到底有没有发生”这些问题。

**第一，command 处理时崩溃在 append 之前。**

流程：

```text
load stream
run business logic
crash before append
```

事件没有写入，系统状态没有改变。客户端看到 timeout 或连接断开，可以用同一个 idempotency key 重试。这里的关键是不能在 append 之前触发不可回滚副作用。

**第二，append 成功但响应丢失。**

流程：

```text
append TaskSubmitted succeeded
process crashes before response
client retries
```

这是最常见的 unknown。客户端不知道命令是否成功。event store 必须能按 command id / idempotency key 查重，或者 command handler 能从事件流中识别已经处理过的命令。否则 retry 会追加第二个事件。

LogServe 的 logstore 返回 duplicate record，就是为这个边界服务。

**第三，append 成功但 projection 没更新。**

流程：

```text
append event seq=100
crash before metadata update
```

如果 event store 是 source of truth，恢复时 replay seq=100 即可补上 read model。LogServe 的 log-first 设计也是这个方向：metadata 更新失败不应造成事实丢失，bootstrap 可以从 logstore 恢复。

**第四，projection 更新成功但 checkpoint 没推进。**

流程：

```text
apply event seq=100 to read model
crash before checkpoint=100
```

重启后会重复处理 seq=100。projection 必须幂等。否则计数、统计、通知会重复。

**第五，checkpoint 先推进但 projection 没落盘。**

流程：

```text
checkpoint=100
crash before read model update
```

重启后 projector 以为 seq=100 已处理，read model 永久少一条。这比重复处理更危险。要么把 read model update 和 checkpoint 放在同一事务里，要么确保 checkpoint 永远不早于 durable projection。

**第六，事件写入成功但 broker publish 失败。**

如果 event store 和 message broker 是两个系统：

```text
append event store succeeded
publish to broker failed
```

读模型消费者收不到事件。解决方式通常是 event store tailing、transactional outbox、或者由 event store 自带 subscription 机制。不能把“append 后直接 publish”当成可靠同步。

**第七，消费者处理成功但 ack 失败。**

流程：

```text
consumer applies event
ack to broker times out
broker redelivers event
```

这要求消费者幂等。read model、邮件、webhook、支付都要考虑重复。如果只有 read model 幂等，外部副作用仍然可能重复。

**第八，重启后 snapshot 和 tail events 不一致。**

Snapshot 是优化。重启时常见流程：

```text
load snapshot at seq=1000
read events 1001..latest
replay
```

边界包括：

- snapshot 写入时是否对应明确 seq。
- snapshot state 和 event stream 是否属于同一 stream。
- 1001..latest 是否还能读到。
- log trim 是否早于 snapshot seq。
- snapshot schema 是否兼容当前代码。

如果 snapshot 新、tail 丢，状态仍然无法恢复。

**第九，事件版本升级后历史 replay 失败。**

重启或重建 projection 时会读到多年以前的事件。若消费者只支持最新 schema：

```text
TaskSubmittedV1 -> decode error
```

replay 会卡住。Microsoft 文档建议 tolerant deserialization、event versioning、upcasting；直接改历史事件会破坏 immutability，通常只作为最后手段。

**第十，补偿事件和错误事件的边界。**

如果 bug 写入了错误事件：

```text
SeatsReserved(200)  # 实际应为 2
```

修代码不会修历史。需要：

```text
ReservationCorrected(...)
or
SeatsReleased(198)
```

或者 upcaster 在 replay 时识别旧错误。不能假装旧事件不存在。

**第十一，LogServe 的具体边界。**

LogServe 的 logstore record 有 CRC32，可以检测读回记录损坏；stream seq 支持按序 replay；trim 和 actor snapshot 则引入恢复边界：

```text
snapshot seq must be usable
events after snapshot must still exist
metadata can be rebuilt from log
```

如果 trim 早于某个 projection 或 snapshot 所需 seq，恢复就会失败。这个问题在小系统里不显眼，但一旦引入 log compaction/retention，就必须把每个消费者的 checkpoint 纳入生命周期策略。

面试里可以这样回答：

```text
event sourcing 的故障边界主要是 append 前后、response 前后、projection/checkpoint 顺序、broker publish、consumer ack、snapshot 与 tail events、schema replay、补偿事件和 log trim。append 成功但响应丢失要靠 idempotency；append 成功但 projection 没更新要靠 replay；projection 成功但 checkpoint 没推进要靠幂等；checkpoint 先推进会丢事件，应该避免。重启时 snapshot 只是优化，event stream 仍然是 source of truth。
```

## Q056. event sourcing 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

Event sourcing 的性能瓶颈要按链路拆开看：

```text
command handling:
  load stream -> rehydrate -> validate -> append

projection:
  consume event -> decode -> apply -> write read model -> checkpoint

query:
  read materialized view
  or rehydrate stream on demand
```

**第一，写入侧常见瓶颈是 I/O 和热点 stream 冲突。**

Event store 写入通常是 append-only，但 append 仍然要持久化：

- WAL/fsync。
- 索引更新。
- stream metadata 更新。
- idempotency record。
- expected revision check。

如果每个 stream 分散，吞吐可以很高。如果大量写入集中到一个 stream，expected revision 会导致冲突重试，热点 stream 成为瓶颈。

**第二，rehydration 的瓶颈常来自 CPU 和 I/O。**

每次 command 可能要加载历史事件：

```text
read events 1..N
deserialize
apply reducer
```

stream 短时问题不大。stream 长了以后，I/O 读放大和 CPU 反序列化都会上来。snapshot 可以减少重放事件数量：

```text
load snapshot at seq=10000
replay events 10001..latest
```

但 snapshot 不是免费：它要占存储、要写入、要校验 schema，还要避免和 trim 策略冲突。

**第三，projection 的瓶颈常来自 read model 写入和 checkpoint。**

事件写入快，不代表系统可读状态更新快。Projection 要做：

```text
decode event
apply projection logic
upsert read model
commit checkpoint
```

如果 read model 是 SQL，瓶颈可能是 upsert、索引、事务提交、连接池。如果是搜索引擎，瓶颈可能是批量刷新、segment merge、网络。如果是内存 view，瓶颈可能是锁和 GC。

**第四，CPU 瓶颈来自事件解码、upcast 和 reducer。**

复杂系统里，事件不是简单 JSON：

- 版本兼容。
- upcaster 链。
- payload 校验。
- 加密/解密。
- 压缩/解压缩。
- 聚合计算。

如果 projection 还要做窗口统计、去重、排序、分桶，CPU 会成为主要瓶颈。

**第五，内存瓶颈来自长 stream、批处理和高基数 projection。**

常见问题：

- 一次加载整个 stream。
- projection 缓冲太多未处理事件。
- 去重表无限增长。
- 高基数字段导致 read model 爆炸。
- snapshot/rebuild 时新旧状态双份存在。
- GC pause 拉高 p99。

Event sourcing 系统经常把“历史完整性”放大成“内存里什么都缓存”。这会出问题。要控制 batch size、窗口、缓存生命周期和 key 基数。

**第六，锁竞争来自 expected revision、stream metadata 和 projection 更新。**

Event sourcing 能减少 update-in-place 的行锁，但不会消除锁。锁只是换了地方：

- 同一 stream append 串行化。
- idempotency key 唯一索引。
- projection checkpoint 单行。
- read model 热点 row。
- snapshot 写入锁。

如果系统里有一个全局 stream，比如 `global:tasks`，所有任务事件都写这里，吞吐会很差。更合理的是按 entity/aggregate 分 stream。

**第七，网络瓶颈来自远程 event store、broker、read model 和跨区域复制。**

分布式 event sourcing 的链路可能是：

```text
command service -> event store -> subscription -> projector -> read DB -> query API
```

每一段都有网络延迟和重试。跨区域时，append latency、replication lag、projection lag 都会影响用户体验。读写都很快但网络抖动，p99 仍然会高。

**第八，查询性能取决于是否直接查事件流。**

如果查询每次都 replay：

```text
GET /workflow/w1 -> read all workflow events -> replay
```

stream 长时 p99 会很差。实际系统通常用 materialized view：

```text
workflow_summary_view
task_current_state_view
actor_current_state
```

这把读性能问题转移到 projection lag 和 read model 维护上。

**第九，LogServe 的可能瓶颈。**

LogServe 的 logstore Append、Read、CRC32、index rebuild、bootstrap replay 都是 event sourcing 性能面的缩影：

- 写入：append record + CRC32 + index/idempotency 更新。
- 读取：按 stream 和 seq 读记录。
- 恢复：从 seq=1 分页读全量 log。
- actor：snapshot 降低 replay command 数。
- metadata：memory map 更新受锁影响。

在机制验证规模下这些设计清楚直接；如果扩展到更大规模，就要关注 stream 分区、snapshot 周期、bootstrap 时间、trim 安全、metadata projection lag。

面试里可以这样回答：

```text
event sourcing 的瓶颈不固定。写侧常见瓶颈是 event store append I/O、fsync、expected revision 冲突和热点 stream；rehydration 瓶颈是读历史事件的 I/O、反序列化 CPU 和 reducer；projection 瓶颈是 read model upsert、checkpoint、索引和网络；长 stream 和高基数 projection 会造成内存和 GC 压力。snapshot、materialized view、stream 分区和批处理能缓解，但也引入新的正确性边界。
```

## Q057. event sourcing 的 correctness test、stress test 和 benchmark 应该分别测什么？

Event sourcing 的测试要分三层：事件生成是否正确，事件重放是否正确，事件投影和故障恢复是否正确。只测 API 返回成功不够。

**第一，correctness test 要用 given-when-then 测 command。**

Microsoft 文档也提到，event-sourced 系统适合这种测试风格：

```text
given past events
when command is issued
then new events are produced
```

例子：

```text
given:
  TaskSubmitted
  TaskStarted(lease_epoch=1)

when:
  CompleteTask(lease_epoch=1, result_ref=r1)

then:
  TaskCompleted(result_ref=r1)
```

非法场景也要测：

```text
given:
  TaskSubmitted
  TaskCompleted

when:
  CompleteTask again

then:
  no new event / idempotent result / conflict
```

**第二，correctness test 要测 replay reducer。**

同一事件流重放应该得到确定状态：

```text
replay(events) == expected_state
replay(events) == replay(events)
```

要覆盖：

- 空 stream。
- 事件缺字段。
- 老版本事件。
- 补偿事件。
- 乱序事件是否拒绝。
- duplicate event 是否跳过。
- snapshot + tail replay 是否等价于 full replay。

LogServe 的 actor full replay 与 snapshot replay 一致性，就属于这种测试。

**第三，correctness test 要测 append 并发控制。**

模拟两个 handler 基于同一个 expected seq 追加：

```text
A append expected=5 -> success
B append expected=5 -> conflict
```

然后检查 B 是否 reload 后重新判断，而不是盲目 retry 同一个事件。

**第四，correctness test 要测 idempotency。**

同一个 command id 重试：

```text
SubmitTask(idem=k1, payload=A)
SubmitTask(idem=k1, payload=A)
```

应该返回同一个事件或同一个结果。

同一个 key 不同 payload：

```text
SubmitTask(idem=k1, payload=B)
```

应该拒绝。这能防止 timeout unknown 场景产生语义重复。

**第五，correctness test 要测 projection。**

给定事件流，投影结果必须符合预期：

```text
event stream -> task_current_view
event stream -> workflow_summary
event stream -> worker_load
```

还要测：

- 重复投递。
- 从 checkpoint 恢复。
- checkpoint 失败后重放。
- dead-letter 不应悄悄跳过。
- full rebuild checksum。

**第六，stress test 要模拟高并发写入和故障。**

压力场景包括：

- 同一 stream 大量并发 append。
- 多 stream 均匀写入。
- hot stream + cold stream 混合。
- command timeout + retry。
- event store crash/restart。
- projector crash/restart。
- broker redelivery。
- schema 混合版本。
- snapshot 写入和 trim 并发。

持续检查不变量：

```text
per-stream seq contiguous
no duplicate command effects
no projection negative counts
snapshot replay == full replay
checkpoint never advances past durable projection
lag eventually catches up after load stops
```

**第七，stress test 要测恢复时间。**

Event sourcing 的承诺之一是恢复，所以要测：

- event store 重启后 index rebuild。
- metadata/read model 从零 rebuild。
- snapshot 存在时的恢复。
- snapshot 丢失时的恢复。
- log trim 后是否还能恢复。
- 大 stream 的 rehydration p99。

LogServe 可以压 `BootstrapFromLog` 的耗时、actor snapshot replay 的 command 数、logstore index rebuild 后的 read correctness。

**第八，benchmark 要分写入、重放、投影和查询。**

写入 benchmark：

```text
append_events_per_sec
append_p50/p95/p99
fsync_latency
expected_revision_conflict_rate
idempotency_lookup_latency
```

重放 benchmark：

```text
rehydrate_events_per_sec
rehydrate_p99_by_stream_length
snapshot_load_ms
tail_replay_ms
allocs/op
```

投影 benchmark：

```text
projection_events_per_sec
event_to_view_visible_ms
checkpoint_commit_ms
catchup_time_after_1h_outage
```

查询 benchmark：

```text
read_model_query_p99
direct_replay_query_p99
view_staleness_ms
```

**第九，benchmark 要测不同数据分布。**

均匀分布和热点分布差异很大：

```text
uniform streams:
  many streams, few events each

hot stream:
  one stream, many concurrent commands

long stream:
  one stream, millions of events
```

只测均匀写入会掩盖实际热点。

面试里可以这样回答：

```text
event sourcing 的 correctness test 要测 given-when-then command、replay reducer、append expected revision、idempotency、projection、snapshot 与 full replay 等价、schema 演进和补偿事件。stress test 要测并发 append、hot stream、timeout retry、crash/restart、consumer redelivery、snapshot/trim 并发和 lag catch-up。benchmark 要分 append、rehydration、projection、query 四条链路，测吞吐、p99、冲突率、恢复时间、内存分配和 view staleness。
```

## Q058. 如果要求从零实现一个简化版 event sourcing，你会先定义哪些不变量？

从零实现 event sourcing，最先写的不是 event struct，而是不变量。事件流一旦成为 source of truth，后续很多设计都被它约束。

**不变量一：事件流是权威事实源。**

必须先定义：

```text
stream_id -> ordered events
current_state = replay(stream_id events)
```

如果还有一张 current-state 表，就要说明它是 projection、cache，还是同等权威状态。最怕的是 event stream 和 state table 都能被业务写，最后谁也说不清谁是真的。

**不变量二：事件不可变，只能追加。**

事件写入后不能原地修改：

```text
append ReservationCanceled
```

而不是：

```text
update old SeatsReserved event
```

修正历史用补偿事件或纠错事件。schema 变化用 version/upcaster，直接改历史事件应该是最后手段。

**不变量三：每个 stream 内事件有严格顺序。**

简化版至少要保证 per-stream seq：

```text
task:t1 seq=1
task:t1 seq=2
task:t1 seq=3
```

replay 按 seq 应用。缺口、重复、倒序都要检测。是否需要全局顺序可以另行决定，但 per-stream 顺序不能含糊。

**不变量四：append 支持 expected revision。**

写入时要带上命令基于哪个版本做决策：

```text
append(stream=t1, expected_seq=5, events=[TaskCompleted])
```

如果当前 stream 已经不是 5，append 失败。没有 expected revision，就无法防止并发 command 基于旧状态同时写入。

**不变量五：每个 command 有 idempotency identity。**

需要记录：

```text
command_id / idempotency_key
payload_fingerprint
produced_event_ids
```

同一个 command 重试不能产生新事件；同一个 key 携带不同 payload 要拒绝。

**不变量六：事件表达事实，不表达命令。**

事件应该是过去发生的事实：

```text
TaskSubmitted
TaskCompleted
PaymentCaptured
ReservationCanceled
```

而不是：

```text
SubmitTask
CompleteTask
CapturePayment
CancelReservation
```

命令可能失败，事件表示已经成功发生的事。这个边界影响 replay：replay 事件时不应该重新执行业务决策，只是应用事实。

**不变量七：reducer deterministic。**

同一事件流必须得到同一状态：

```text
replay(events) = state
```

Reducer 不能依赖当前时间、随机数、远程服务、非稳定配置。事件里要包含恢复状态所需的数据，比如 occurred_at、actor_id、result_ref、lease_epoch。

**不变量八：replay 不产生外部副作用。**

重放事件只能重建状态：

```text
event -> state
```

不能发邮件、扣款、调用 webhook。副作用消费者要有自己的 inbox/idempotency，并且能区分 live processing 和 replay。

**不变量九：事件 envelope 必须有足够元数据。**

简化版也应该有：

```text
event_id
stream_id
seq
event_type
event_version
command_id / idempotency_key
correlation_id
causation_id
occurred_at
payload
checksum
```

不一定一开始全用上，但缺少 seq、event type、version、command id、checksum 这类字段，后面排查和恢复会很难。

**不变量十：projection 是派生状态。**

所有 read model 都应满足：

```text
projection_state = project(events up to checkpoint)
```

Projection 可以重建。checkpoint 表示已经 durable 反映到的位置，不能早于实际 view update。

**不变量十一：snapshot 是优化，不是事实源。**

Snapshot 要带：

```text
stream_id
snapshot_seq
state
schema_version
```

恢复时：

```text
load snapshot
replay events after snapshot_seq
```

如果 snapshot 丢失，仍然能从事件流重建。若事件流已 trim，则必须证明 snapshot 和 tail events 足以覆盖恢复。

**不变量十二：保留策略不能破坏恢复能力。**

如果有 trim/retention：

```text
trim_before_seq <= min(required_snapshot_seq, projection_checkpoint)
```

还要定义哪些 projection 未来需要全量重建。不能为了省磁盘删掉唯一事实源。

**不变量十三：事件 schema 有演进规则。**

要定义：

- additive change 是否允许。
- unknown field 怎么处理。
- missing field 默认值是什么。
- event version 放在哪里。
- upcaster 如何注册。
- 旧事件是否永远可读。

否则 event store 保存得越久，迁移越痛。

**不变量十四：隐私数据有隔离策略。**

如果事件不可变，就不能随手把 PII 放进去。简化版也要规定：

```text
events store IDs/references
PII stored in deletable profile store
or encrypted with per-subject key
```

面试里可以这样回答：

```text
我会先定义这些不变量：event stream 是 source of truth；事件不可变且只能追加；每个 stream 内 seq 严格递增；append 必须支持 expected revision；command 有 idempotency identity；事件表达已经发生的事实；reducer deterministic；replay 不产生外部副作用；event envelope 有 event_id、stream_id、seq、type、version、correlation/causation、checksum；projection 是派生状态；snapshot 只是优化；retention 不破坏恢复；schema 有版本和 upcaster；敏感数据不能无脑写进不可变事件。
```

## Q059. event sourcing 的常见误用是什么，误用后通常会产生什么线上症状？

Event sourcing 的误用很容易伪装成“我们也有事件”。真正的问题要等到重放、修 bug、删数据、扩容或追查事故时才暴露。

**误用一：把事件设计成字段变更。**

表现：

```text
StatusChanged("completed")
FieldUpdated("retry_count", 2)
```

线上症状：

- 审计看不懂业务原因。
- 新 projection 无法推导语义。
- 补偿事件难写。
- debug 时只能猜“为什么字段变了”。

更好的事件是：

```text
TaskCompleted
TaskRedelivered
RetryLimitReached
```

**误用二：把命令当事件存。**

表现：

```text
CompleteTaskCommandStored
ReserveSeatsCommandStored
```

命令只是请求，可能失败。事件应该表示已经发生。线上症状：

- replay 时重新执行校验。
- 失败命令也被当成事实。
- 当前状态和历史记录对不上。

**误用三：事件缺少重建状态所需字段。**

比如只存：

```text
TaskCompleted(task_id)
```

但没有 result_ref、lease_epoch、completed_at、worker_id。线上症状：

- 当前状态无法完整重建。
- 新 read model 需要字段时发现历史没有。
- 只能回查旧数据库或日志。

事件要包含未来重放所需的事实，而不是只满足当前 handler。

**误用四：允许修改历史事件。**

表现：

```text
UPDATE events SET payload=...
```

线上症状：

- 审计失真。
- checksum/signature 失效。
- projection 重建结果和旧结果不一致。
- 事故后无法证明发生过什么。

修正应通过补偿事件、纠错事件或受控 upcaster。

**误用五：没有 expected revision。**

表现：

```text
append(stream, event)
```

不检查 command 基于哪个版本。线上症状：

- 并发写覆盖业务不变量。
- 同一 task 同时 completed 和 failed。
- 库存超卖。
- actor command 乱序。

Event sourcing 里的并发控制不是可选项。

**误用六：没有 idempotency。**

表现：

```text
timeout -> retry -> append another event
```

线上症状：

- 任务重复提交。
- 支付重复。
- 通知重复。
- projection 计数膨胀。
- event log 出现语义重复但 seq 不重复。

要用 command id / idempotency key 和 payload fingerprint。

**误用七：projection 不幂等。**

表现：

```text
on SeatsReserved:
  available -= n
```

但没有 last processed seq。线上症状：

- broker redelivery 后计数偏移。
- 重启后重复消费造成报表膨胀。
- read model 和 event stream 漂移。

Microsoft 文档也明确提醒，消费者通常是 at-least-once，handler 必须幂等。

**误用八：把 Kafka topic 当 event store，但没有事实源语义。**

表现：

- retention 会删除历史。
- compaction 会丢中间事件。
- 无法按 entity stream 查询完整历史。
- 没有 expected revision。
- schema 演进不受控。

线上症状：

- 过一段时间后无法重建状态。
- 新 read model 无法 backfill。
- 并发写冲突无法检测。

Broker 可以是分发层，但是否能当 event store 要单独设计。

**误用九：replay 触发外部副作用。**

表现：

```text
replay PaymentCaptured -> call payment gateway
replay TaskCompleted -> send email
```

线上症状：

- 重建 read model 时重复通知。
- 测试 replay 污染外部系统。
- 历史事件触发旧 webhook。

Replay 应该只重建状态。

**误用十：没有 schema evolution 策略。**

表现：

```text
消费者只懂最新事件格式。
```

线上症状：

- 老事件 replay 失败。
- projection 卡在某个历史 seq。
- 新版本部署后旧服务写入的事件无法消费。
- 只能手工改历史。

**误用十一：stream 无限增长但没有 snapshot。**

线上症状：

- command latency 随 stream 长度增长。
- actor 或 workflow 恢复变慢。
- p99 抖动。
- bootstrap 时间越来越长。

Snapshot 不能替代事件流，但长 stream 没有 snapshot 会拖垮 rehydration。

**误用十二：把所有系统都 event sourced。**

简单配置、用户资料、静态目录都做事件流。线上症状：

- 开发慢。
- 查询复杂。
- 数据删除困难。
- schema 迁移成本高。
- 团队绕过事件流改读模型。

Event sourcing 应该选择性使用。

面试里可以这样回答：

```text
event sourcing 常见误用包括把事件设计成字段变更、把命令当事件、事件缺字段、修改历史事件、没有 expected revision、没有 idempotency、projection 不幂等、把 broker 当 event store、replay 触发外部副作用、没有 schema evolution、长 stream 没有 snapshot，以及全系统滥用。线上症状通常是无法重建状态、审计看不懂、并发状态冲突、重复提交、read model 漂移、replay 卡住、通知重复、恢复越来越慢。
```

## Q060. event sourcing 在单机和分布式环境中的语义有什么差异？

Event sourcing 在单机里主要是存储模型和恢复模型；在分布式环境里，它会变成顺序、分区、复制、消费者语义和跨服务一致性问题。两者的复杂度不在一个量级。

**第一，单机环境里 per-stream 顺序更容易保证。**

单机 event store 可以用一把锁或本地事务保证：

```text
stream task:t1:
  seq=1
  seq=2
  seq=3
```

append、index 更新、idempotency record 可以在同一进程内完成。LogServe 的 logstore 就是这种形态：`nextSeq` 按 stream 维护，Append 时生成 seq，Read 按 stream/fromSeq 读取。

**第二，单机环境的恢复路径更短。**

崩溃后可以：

```text
open local log
rebuild index
bootstrap metadata
replay actor/workflow/task state
```

没有跨节点复制延迟，没有 leader 切换，没有多副本一致性。问题主要是本地文件完整性、CRC、index rebuild、trim 和 snapshot 是否安全。

**第三，单机不等于没有并发问题。**

同一个进程里也可能有多个 goroutine 同时提交命令：

```text
CompleteTask
FailTask
```

仍然需要 expected revision、idempotency、lease epoch、actor command sequence。只是这些问题可以用本地锁和本地事务解决，故障面小一些。

**第四，分布式环境里全局顺序通常不存在。**

分布式 event sourcing 更常见的是：

```text
stream task:t1 seq 1..N
stream workflow:w1 seq 1..M
stream actor:a1 seq 1..K
```

每个 stream 内有序，跨 stream 没有天然全局顺序。需要全局排序的业务，要付出额外代价，比如全局 log、单 leader、共识协议或事务协调。

大多数系统应避免依赖全局顺序，把强一致不变量收敛到单个 aggregate stream。

**第五，分布式环境需要 leader、quorum 或复制语义。**

如果 event store 有多个副本，要定义：

- append 写到几个副本才算成功。
- leader 崩溃后是否可能重复接受写入。
- follower 落后时能否服务读。
- read-your-writes 如何保证。
- 跨 region 复制是否同步。

单机 append 成功通常就是本地事实；分布式 append 成功要看复制协议。

**第六，分布式消费者默认要按 at-least-once 设计。**

Projection、integration handler、notification handler 都可能重复收到事件：

```text
event delivered
handler applies
ack lost
event redelivered
```

所以每个消费者都要有自己的 checkpoint/inbox/idempotency。不能因为 event store 里事件只有一条，就假设所有 side effect 也只发生一次。

**第七，分布式投影需要 ownership 和 fencing。**

多个 projector 实例处理同一个 partition 时，要防止旧 owner 继续写：

```text
partition=3 owner=p2 epoch=42
```

写 read model 和 checkpoint 时带 epoch。没有 fencing，就可能出现 split-brain projection：两个 projector 都认为自己负责同一批事件。

**第八，分布式环境里 read-your-writes 要显式设计。**

命令写到 event store 后，查询可能读到落后的 read model：

```text
append event offset=100
read model checkpoint=95
```

跨 region 时更明显。解决方式包括：

- 命令返回 event position。
- 查询携带 `min_position`。
- gateway 等待 projection catch up。
- 会话粘滞到同一 region。
- 关键读直接从 event stream rehydrate。

否则用户会看到“提交成功但查不到”。

**第九，分布式环境里 schema 演进和回放更麻烦。**

多个服务版本同时存在：

```text
service v1 writes EventV1
service v2 writes EventV2
projector v1 consumes both
projector v2 rebuilds history
```

要有 versioning、tolerant reader、upcaster、兼容窗口和回滚策略。单机也需要 schema evolution，但分布式滚动升级会把问题放大。

**第十，分布式环境里数据生命周期更难。**

事件可能被复制到：

- event store。
- broker。
- read model。
- search index。
- object store。
- data warehouse。
- dead-letter queue。
- backup。

删除、加密、retention、crypto-shredding 都要覆盖这些位置。单机事件流只有一个文件或一个数据库，生命周期边界更清楚。

**第十一，LogServe 的边界。**

LogServe 可以描述为单机 event-sourcing-like 系统：

```text
append-only logstore
per-stream seq
idempotency key
metadata projection
bootstrap replay
actor snapshot
CRC32 record check
```

如果要扩展到分布式 event sourcing，需要补齐：

- stream partitioning。
- expected revision 的分布式实现。
- event store replication。
- projector ownership/fencing。
- consumer checkpoint/inbox。
- dead-letter repair。
- cross-region read-your-writes。
- schema registry/upcaster。
- retention 与所有 projection checkpoint 的协调。

这不是把 logstore 换成远程数据库就结束，而是语义层面的扩展。

面试里可以这样回答：

```text
单机 event sourcing 主要处理本地 append-only log、per-stream seq、idempotency、replay、snapshot 和文件恢复；可以用本地锁或事务保证顺序。分布式 event sourcing 还要处理复制一致性、leader/quorum、跨 stream 无全局顺序、at-least-once 消费、projector ownership/fencing、read-your-writes、schema 滚动升级、跨区域 lag 和数据生命周期。单机是恢复模型，分布式是恢复模型加一致性协议和运维体系。
```

## 参考资料

- Martin Fowler, [CQRS](https://martinfowler.com/bliki/CQRS.html)
- Martin Fowler, [Event Sourcing](https://martinfowler.com/eaaDev/EventSourcing.html)
- Microsoft Azure Architecture Center, [CQRS pattern](https://learn.microsoft.com/en-us/azure/architecture/patterns/cqrs)
- Microsoft Azure Architecture Center, [Event Sourcing pattern](https://learn.microsoft.com/en-us/azure/architecture/patterns/event-sourcing)
- Microsoft Azure Architecture Center, [Materialized View pattern](https://learn.microsoft.com/en-us/azure/architecture/patterns/materialized-view)
- Microsoft Azure Architecture Center, [Compensating Transaction pattern](https://learn.microsoft.com/en-us/azure/architecture/patterns/compensating-transaction)
- PostgreSQL Documentation, [Materialized Views](https://www.postgresql.org/docs/current/rules-materializedviews.html)
- PostgreSQL Documentation, [REFRESH MATERIALIZED VIEW](https://www.postgresql.org/docs/current/sql-refreshmaterializedview.html)
- Microsoft Azure Cosmos DB, [Consistency level choices](https://learn.microsoft.com/en-us/azure/cosmos-db/consistency-levels)
- Microservices.io, [Saga pattern](https://microservices.io/patterns/data/saga.html)
- AWS Prescriptive Guidance, [Saga patterns](https://docs.aws.amazon.com/prescriptive-guidance/latest/cloud-design-patterns/saga.html)
- EUR-Lex, [Regulation (EU) 2016/679, General Data Protection Regulation](https://eur-lex.europa.eu/eli/reg/2016/679/oj/eng)
- Microservices.io, [Transactional Outbox pattern](https://microservices.io/patterns/data/transactional-outbox.html)
- MassTransit Documentation, [Transactional Outbox / Consumer Outbox](https://masstransit.io/documentation/patterns/transactional-outbox)
