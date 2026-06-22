# 41. DAG、topological sort、deterministic replay、gRPC 追问链

这一批问题把 workflow 调度和 RPC 边界放在一起看。DAG 和 topological sort 解决的是依赖顺序，deterministic replay 解决的是恢复时能不能沿着同一条历史走回来，gRPC 解决的是进程间调用的接口、传输和失败表达。它们听起来分属不同层，但线上事故经常连在一起：一个节点是否 ready、一次重试是否会重复副作用、一次 RPC 超时是否已经在服务端执行成功，最后都会影响任务系统能不能解释自己的状态。

下面的问题不重复基础定义，重点回答面试里最容易被追问的部分：一句话定义哪里会误导，生产事故从哪里触发，指标怎么设计才能看到尾部和边界，正确性与性能分别能保证到哪一步。

## Q001. 面试官如果只问一个问题检验你是否理解 DAG，可能会问什么？

**回答：**

我会预期他问这个问题：

```text
一个 workflow 有 A、B、C、D 四个 step，B 和 C 依赖 A，D 依赖 B 和 C。现在 A 成功，B 失败后重试，C 成功但产物被删除，D 一直没有执行。请说明：D 到底什么时候算 ready？D 的输入依赖如何验证？如果 workflow 定义后来改了，正在运行的历史实例应该按旧 DAG 还是新 DAG 走？
```

这道题比“DAG 是什么”更能看出理解深度。DAG 不是一张漂亮的流程图，它是调度器判断任务能不能执行的约束集合。节点、边、输入引用、触发规则、重试策略、版本和资源限制都要进入语义里。

先说 ready。D 的上游 B 和 C 都达到可接受终态，D 才能进入 ready。这里的“可接受终态”不一定都是 success，要看触发规则。普通任务可能要求所有上游成功；清理任务可能允许上游 skipped 或 failed；补偿任务甚至可能只在上游失败时触发。如果定义里没把这些规则说清楚，调度器就会在失败和跳过场景里做出不一致决定。

再说输入依赖。D 依赖 B 和 C，不只是依赖它们的状态，还依赖它们承诺产出的数据是否存在、版本是否匹配、权限是否可用、schema 是否兼容。C 标记为成功但产物被删除，D 不应该因为拓扑条件满足就盲目执行。此时应该把错误归到依赖产物缺失、上游结果损坏，或者触发重建，而不是说 DAG 本身没问题就继续跑。

第三是运行实例和定义版本。一个 DAG 定义发布后，已经启动的 run 应该绑定到某个定义版本。否则你今天改了一条边，昨天还在跑的实例会突然按新依赖判断 ready，恢复、重试、补偿都会乱。生产系统通常要保留 DAG version、step version 或 workflow definition snapshot，让 running instance 有可解释的历史。

第四是边的方向和含义。很多事故来自方向写反：`A -> B` 到底表示 A 依赖 B，还是 B 依赖 A？不同库和内部模型可能不同。面试时我会主动说明：“我会先统一边的语义，例如 edge 从 prerequisite 指向 dependent，然后所有 ready 判断、拓扑排序、可视化都按这个方向。”

所以这个问题的核心答案是：DAG 的价值在于让依赖关系可验证、可调度、可恢复。判断一个节点能不能跑，不只看所有前驱是否跑完，还要看前驱终态是否满足触发规则、输入产物是否仍然有效、运行实例是否绑定到正确的 DAG 版本，以及重试和恢复是否沿着同一份定义解释。

## Q002. DAG 的一句话定义是否容易误导，误导点在哪里？

**回答：**

容易误导。最常见的一句话是：DAG 是有向无环图。数学上没错，工程上太薄。

在 workflow 或任务调度里，DAG 通常不是单纯的图结构。它还包含任务定义、依赖条件、输入输出引用、触发规则、重试策略、超时、资源需求、并发限制、版本、调度时间和可观测字段。只说“有向无环图”，会让人误以为只要没有环，系统就能正确调度。

第一个误导点是把无环当成正确性充分条件。无环只说明依赖顺序没有直接或间接自我等待。它不说明边是否完整，不说明隐藏依赖是否建模，不说明每个 step 的输入是否存在，也不说明失败语义是否合理。很多 DAG 事故不是有环，而是少了一条边。

第二个误导点是忽略运行实例。DAG definition 和 DAG run 不是一回事。definition 是模板，run 是某次执行的状态机。一个 run 可能处在旧版本定义上，可能有部分 step 成功、部分 step 正在重试。把定义和运行态混在一起，会导致改图后历史实例不可解释。

第三个误导点是把 DAG 当成执行顺序。DAG 给的是偏序，不是唯一序列。A 和 B 没有依赖，先跑谁都合法。真正执行顺序还会受资源、优先级、队列、worker 可用性、限流、数据 locality 影响。面试里如果说“拓扑排序后按顺序执行”，很容易被追问并行度怎么利用。

第四个误导点是忽略数据依赖和控制依赖的差别。有些边只是表示执行前后关系，有些边还表示下游要读取上游产物。控制依赖满足不代表数据产物可用。生产系统里最好把状态依赖、数据引用、资源依赖区分清楚。

第五个误导点是把 DAG 当成静态不变。很多系统支持动态 fan-out、条件分支、参数化任务、backfill。动态生成不是错，但必须定义生成时机、生成结果是否持久化、重放时能否得到同一张图。

所以我会把定义改成：DAG 在任务系统里是带版本的依赖约束模型，用有向无环关系描述 step 之间的偏序，并附带运行时需要的触发、输入、失败、重试和资源语义。无环只是入门检查，不是完整正确性。

## Q003. DAG 最常见的生产事故触发条件是什么？

**回答：**

DAG 的事故通常不是“图论算法写错”，而是业务依赖没有被完整建模。调度器按图跑得很认真，但图本身没有表达真实世界的约束。

第一类事故是隐藏依赖。下游 step 读取一个共享文件、全局缓存、数据库表或环境变量，但 DAG 里没有边。平时因为执行够慢，依赖刚好已经准备好；并发度一提高，下游提前执行，读到旧数据或空数据。这个问题在迁移到更快调度器、增加 worker 数、打开并行执行后特别常见。

第二类是动态 DAG 不稳定。动态 fan-out 依赖当前时间、随机数、外部 API、数据库查询结果，恢复或重试时生成出不同节点集合。原来有 100 个 shard，重放时变成 99 个，调度器不知道哪个才是真正的历史。动态图必须把展开结果持久化，不能每次运行重新“想一遍”。

第三类是定义版本漂移。运行中的 workflow 还没结束，代码发布改了依赖边、step 名、输入 schema 或 trigger rule。恢复时用新定义解释旧历史，出现下游永远不 ready、已完成 step 被当成缺失、旧产物无法解析。

第四类是失败和 skipped 语义不清。某个上游失败后，下游到底是失败、跳过、等待重试、走补偿，还是允许继续？如果每个 step 自己随便判断，DAG 的状态机会变成一堆特殊分支。最后线上会看到任务“卡住”，但没人知道它是在等什么。

第五类是 fan-out 爆炸。图没有环，但宽度太大。一个用户请求展开出几十万个节点，调度器、metadata store、队列和可视化全被打爆。DAG 正确不代表可执行，图的规模和资源上限也要验证。

第六类是节点身份不稳定。step id 由列表下标、临时文件名、map 迭代顺序拼出来，代码改动后同一个逻辑 step 的 id 变了。重试和恢复找不到旧状态，幂等键也失效。

第七类是把资源约束当成依赖边。比如“最多同时跑 10 个 GPU step”不是 DAG 边，而是调度资源约束。如果硬塞成依赖链，会牺牲并行度，也容易在重试时造成假顺序。

第八类是产物生命周期和 DAG 生命周期脱节。上游成功后产物被清理，metadata 还显示 success；下游 ready 后读取失败。DAG 调度必须和 result reference、retention、checksum、schema 一起看。

我会把事故触发条件总结成一句：DAG 最怕图和真实依赖不一致。无环、可视化好看、拓扑排序通过，都不能替代版本、输入产物、失败规则、动态展开和资源边界的验证。

## Q004. DAG 的指标应该怎么设计才不会只看平均值？

**回答：**

DAG 的指标不能只看“平均 workflow 耗时”。平均值会把关键路径、热点节点、宽度爆炸、调度延迟和失败重试全部压平。一个 DAG 系统真正要看的是：图哪里阻塞、哪里排队、哪里重复、哪里超过历史定义能承受的范围。

我会从图结构开始看。每个 DAG run 的节点数、边数、最大深度、最大宽度、critical path length、fan-out 分布、fan-in 分布都要有。尤其要看 p95/p99 和最大值。平均 100 个节点没意义，一个 run 展开成 50 万个节点就能打爆系统。

第二组是 ready 和 blocked。ready node 数量、blocked node 数量、每个 blocked node 等待的前驱、最老 blocked age、ready 后到真正调度的等待时间。很多问题不是执行慢，而是节点已经 ready 却在 scheduler 或队列里等太久。

第三组是阶段耗时。把 DAG run 拆成 dependency resolution、ready queue wait、worker queue wait、step execution、result materialization、downstream unblocking。只看总耗时会误判瓶颈。比如 critical path 上某个 step 很慢，和 scheduler lag 很高，是两类问题。

第四组是关键路径指标。critical path 上每个节点的 p95/p99、重试次数、等待时间、下游 fan-in 等待时间。DAG 的整体延迟由关键路径决定，不由所有节点平均决定。大量并行小任务再快，也抵不过关键路径上一条慢链。

第五组是失败语义指标。失败节点数、skipped 节点数、retrying 节点数、blocked_by_failed_dependency、input_missing、cycle_validation_failed、definition_version_mismatch。错误要带 reason，不能只有 failed。

第六组是重试和重复。每个 step 的 attempt count、重复调度次数、幂等命中、同一 step 被两个 worker 抢到的次数、stale completion 拒绝次数。DAG 系统的正确性事故常常藏在重试路径里。

第七组是产物和输入。result reference 缺失率、checksum mismatch、schema mismatch、读取 p99、产物大小分布、被清理但仍被引用的次数。DAG ready 不等于输入可用，这组指标能补上盲区。

第八组是资源和公平性。按 DAG type、租户、优先级看队列年龄、worker 占用、拒绝率、并发上限触发次数。否则一个大 backfill 会把普通在线 workflow 挤掉。

我会在面试里说：DAG 指标的主视角应该是结构分布、关键路径、ready/blocked age、调度延迟、失败原因、重试和产物有效性。平均 workflow duration 只能说明大概健康，不能指导排障。

## Q005. DAG 的正确性边界和性能边界分别是什么？

**回答：**

DAG 的正确性边界是偏序约束。它能说明哪些节点必须在另一些节点之前完成，哪些节点可以并行，哪些依赖组合会形成环。它不能自动证明每个 step 的业务逻辑正确，也不能保证外部副作用幂等，更不能保证下游读取到的数据一定还在。

正确性上至少要守住几条线。

第一，图快照要稳定。一次 run 应该绑定到固定的 DAG definition version，动态展开结果也应该可恢复。恢复时不能用今天的代码随便解释昨天的历史。

第二，边语义要固定。edge 方向、依赖类型、触发规则、失败传播、skipped 传播都要有明确约定。否则同一张图在不同模块里会被解释成不同含义。

第三，ready 判断要完整。前驱状态、触发规则、输入产物、资源许可、取消状态都可能影响能不能执行。只看 indegree 归零是不够的。

第四，节点身份要稳定。step id、attempt id、result ref、幂等键要能跨重试、恢复和版本演进保持可解释。节点身份不稳定，DAG 的历史状态就会散掉。

第五，外部副作用不由 DAG 保证。一个 step 发邮件、扣款、写数据库，DAG 只能决定它什么时候被调度。是否能安全重试，要靠幂等、事务、outbox 或补偿。

性能边界也要说清楚。

拓扑验证通常是 O(V+E)，但真实系统的瓶颈经常不在算法，而在 metadata 读写、状态更新、ready queue、worker 调度、结果存储和可视化。小图上算法差异不明显，大规模图上对象分配、边存储、数据库索引和锁竞争会更重要。

DAG 的并行度受最大宽度、资源池、下游容量和关键路径限制。图很宽不代表能全跑，资源不够会排队；图很深不代表慢，如果每个节点很短也能接受。关键路径才是端到端延迟的硬约束。

动态 DAG 和大 fan-out 会带来控制面压力。节点数和边数越大，状态转移、事件日志、调度扫描、UI 查询都会变贵。生产系统通常要限制图规模、分批展开、分层调度，或者把大量同构任务合并成 map stage。

所以我的答案是：DAG 的正确性边界在依赖偏序和运行定义的可解释性；性能边界在图规模、关键路径、ready queue、状态存储和资源调度。DAG 解决“谁依赖谁”，不解决所有执行问题。

## Q006. 面试官如果只问一个问题检验你是否理解 topological sort，可能会问什么？

**回答：**

我会预期这个问题：

```text
给你一张 DAG，其中 A 和 B 都没有前驱，C 依赖 A 和 B，D 依赖 A。请输出一个可并行调度的拓扑结果。如果图里有环，怎么报错？如果 A 和 B 同时 ready，顺序是否必须固定？如果线上恢复时要求结果可重复，你会怎么处理 tie-break？
```

这个问题能把“背过算法”和“理解调度语义”分开。拓扑排序不是为了得到唯一队列，而是为了得到满足依赖偏序的执行顺序或 ready 批次。A 和 B 没有依赖，`A,B,C,D` 和 `B,A,D,C` 都可能合法，只要每条边的前驱在后继之前。

如果用于调度，我更愿意输出 ready batch，而不是只输出一条线性序列：

```text
batch 1: A, B
batch 2: D, C   // D 只依赖 A，C 依赖 A 和 B
```

当然，第二批里 C 和 D 的相对顺序也不一定唯一。真正运行时还要看资源、优先级、队列和 worker availability。

Kahn 算法的思路很适合解释 ready 语义：先找 indegree 为 0 的节点，把它们放进 ready set；节点完成后，减少下游 indegree；下游 indegree 变成 0，就进入 ready set。如果最终还有节点没处理，说明被环挡住了，或者图定义不完整。

环怎么报错也很重要。不能只说“拓扑排序失败”。生产系统要告诉用户至少一条 cycle 路径，比如 `A -> B -> C -> A`。如果只返回 false，用户很难修 DAG。对于大型图，还要区分真正的 cycle、缺失节点、重复边、方向错误。

然后是确定性。普通拓扑排序可能受 map 迭代顺序、插入顺序、数据库返回顺序影响。数学上合法，工程上可能很危险。比如调度器重启后用不同顺序选择 ready node，导致日志、任务 id、优先级和资源分配都不同。需要可重复时，ready set 必须有稳定 tie-break，例如按 topological rank、step id、用户声明顺序、优先级再加 step id 排序。

所以我的回答会落在这几个点：拓扑排序给的是偏序的一个合法线性化，或者一组 ready 批次；多解是正常现象；环必须显式检测并报告路径；工程系统里如果恢复、审计或 replay 需要稳定结果，就要定义 deterministic tie-break，而不是依赖容器迭代顺序。

## Q007. topological sort 的一句话定义是否容易误导，误导点在哪里？

**回答：**

常见定义是：拓扑排序把 DAG 的节点排成一个线性顺序，使每条边的起点都在终点之前。这个定义准确，但面试里只说到这里会显得很浅。

第一个误导点是让人以为排序结果唯一。拓扑序通常有很多个。只要不违反依赖，多个顺序都合法。工程系统如果需要稳定输出，必须自己定义 tie-break。算法不会天然给你业务上“最正确”的那一个。

第二个误导点是忽略 ready set。调度器不一定真的需要一个完整线性列表，它更关心“现在有哪些节点可以运行”。一次性 `static_order` 适合构建、安装、编译依赖；workflow 调度更常用增量 ready：上游完成一个，下游 indegree 变化，新的节点变 ready。

第三个误导点是没有说环。拓扑排序只对 DAG 成立。图里有环时，应该明确失败，并尽量报告环路径。把失败节点留在 blocked 状态不解释，线上就会表现成“任务一直等”。

第四个误导点是把图方向说模糊。某些 API 里 `add(node, predecessors)`，某些内部模型里存的是 successor list。方向一混，输出顺序就反了。面试里最好先说明边的语义。

第五个误导点是把拓扑排序当成调度策略。拓扑排序只回答依赖顺序，不回答哪个 ready node 优先、资源怎么分配、失败怎么传播、重试怎么处理、不同租户怎么公平。调度器还要在拓扑约束之外做决策。

第六个误导点是忽略图快照。排序必须针对某个固定图。图一边被修改一边排序，除非算法专门支持增量更新，否则结果不可解释。很多库在开始排序后会禁止再加节点，就是为了守住这个边界。

所以我会把定义补成：topological sort 是在固定的无环依赖图上生成一个满足偏序的合法顺序或 ready 批次；它可能有多个合法结果，遇到环必须失败，工程上还要定义稳定 tie-break 和调度策略。

## Q008. topological sort 最常见的生产事故触发条件是什么？

**回答：**

拓扑排序的事故通常发生在“算法合法，但系统假设它唯一或稳定”的地方。线上不会报“拓扑排序错了”，而是表现为任务顺序偶发变化、恢复后状态不一致、某些节点永远不 ready。

第一类是非确定性 tie-break。ready set 来自 map、hash set 或数据库无序查询，A 和 B 谁先出来不固定。单次执行都合法，但任务 id、日志顺序、资源分配、缓存 key 或 replay 决策依赖这个顺序时，系统就会出现偶发差异。

第二类是环检测太晚。用户提交 DAG 时没有校验，直到调度到一半才发现剩余节点永远不 ready。此时已经跑了一部分 step，产物和副作用都产生了，回滚和补偿很麻烦。DAG definition 最好在接收时就做完整 cycle check。

第三类是边方向写反。比如 API 叫 `dependencies`，内部存成 `children`，转换时反了一次。结果下游先跑，上游后跑。小图人工看不出来，大图里会表现为输入缺失和奇怪的重试。

第四类是重复边、缺失节点和孤儿节点处理不一致。重复边如果 indegree 计算两次，但完成时只减一次，下游永远不 ready；依赖一个不存在的节点，如果被当成 0 indegree，下游会提前跑。

第五类是排序过程中图被修改。动态 DAG 如果一边调度一边加边，已经 ready 的节点可能后来又多了前驱。此时要么禁止修改已准备的图，要么把动态展开作为新的持久化事件，并重新定义 ready 规则。

第六类是并发完成更新 indegree 时竞态。两个上游同时完成，两个 worker 同时减少同一个下游 indegree，如果没有原子状态转移，可能重复入 ready queue，也可能漏入。拓扑排序本身是单线程概念，分布式调度要额外保证状态更新幂等。

第七类是失败、跳过和取消没有纳入拓扑状态。上游 failed 后，下游 indegree 是否减少？如果不减少，下游永远 blocked；如果直接减少，下游可能在缺输入时运行。正确做法是把前驱终态和 trigger rule 一起算。

第八类是恢复时只重算拓扑，不读取历史完成状态。调度器重启后从定义重新排序，忘了哪些节点已经成功、哪些正在重试，导致重复调度或漏调度。

我会总结：topological sort 最常见的线上问题不是 O(V+E) 写错，而是稳定性、图快照、环报告、方向语义、并发 indegree 更新和失败状态传播没有定义清楚。

## Q009. topological sort 的指标应该怎么设计才不会只看平均值？

**回答：**

拓扑排序的指标如果只看“排序耗时平均值”，基本看不到生产问题。大多数图很小，平均耗时很好看；真正打爆系统的是少数大图、宽图、深图、环图和动态展开图。

我会先看图规模分布：节点数、边数、最大 indegree、最大 outdegree、深度、宽度。必须看 p95、p99、max。平均 200 条边没意义，一个异常 DAG 有 200 万条边就够把 scheduler 内存打穿。

第二看 ready set。每轮 ready set size、ready batch 数量、ready 到 dispatch 的等待时间、ready 后未调度的最老年龄。拓扑排序用于调度时，ready set 的年龄比排序时间更接近用户感受。

第三看 blocked 节点。blocked count、blocked reason、最老 blocked age、blocked_by_failed_dependency、blocked_by_missing_input、blocked_by_cycle、blocked_by_resource。拓扑上未 ready 和资源上未调度要分开，否则排障会误判。

第四看 cycle 和校验。cycle detection count、cycle path length、definition rejected count、缺失节点、重复边、方向校验失败、动态修改被拒绝次数。这些指标能发现用户定义和生成器是否在制造坏图。

第五看确定性。相同 DAG definition 的 order hash、ready batch hash、tie-break 变更次数、重启前后 ready set 差异。这个指标不是每个系统都需要，但需要 deterministic replay 或审计的系统很有用。

第六看状态转移。indegree decrement latency、下游入队次数、重复 ready 入队、stale completion ignored、done 事件处理 p99。并发调度的瓶颈通常在状态更新，不在拓扑排序函数本身。

第七看内存和存储。每个图的内存占用、边索引大小、加载图耗时、metadata 查询 p99、持久化 ready queue 写入 p99。大 DAG 系统通常先被存储和对象分配拖慢。

第八看关键路径。topological rank、critical path length、关键路径节点等待时间。即使 ready set 很大，端到端延迟也可能被一条深链限制。

我会给面试官一个判断标准：拓扑排序指标要围绕“图规模、ready/blocked、cycle、确定性、状态更新、存储开销和关键路径”设计。平均排序耗时只能证明普通图不慢，证明不了系统能处理尾部图。

## Q010. topological sort 的正确性边界和性能边界分别是什么？

**回答：**

topological sort 的正确性边界很窄：在一张固定的无环有向图上，输出满足依赖偏序的顺序。它不保证顺序唯一，不保证执行成功，不保证资源最优，也不保证失败传播正确。

正确性上要明确几件事。

第一，输入图必须固定。排序开始后如果还能加边、删边、改节点，除非算法专门支持增量更新，否则输出没有稳定语义。

第二，图必须无环。有环时不能假装输出部分结果就是成功。可以返回已知 ready 部分用于诊断，但整体排序必须报告 cycle。生产系统最好告诉用户哪条环挡住了进度。

第三，边语义必须统一。`u -> v` 表示 u 在 v 前，还是 v 依赖 u？这个约定要贯穿存储、API、可视化和调度。方向不清，算法再对也没用。

第四，多解是正常的。如果上层依赖稳定顺序，就要加稳定 tie-break。否则同一个图在不同运行中得到不同合法拓扑序，可能破坏 replay、审计、快照或测试。

第五，拓扑顺序不是执行计划。执行计划还要考虑资源、优先级、公平性、超时、重试、失败规则和产物可用性。topological sort 只是调度器的一个输入。

性能边界也要放在真实系统里看。理论复杂度通常是 O(V+E)，这对单机内存图很好解释。但线上瓶颈可能来自图加载、数据库扫描、边索引构建、对象分配、锁竞争、ready queue 持久化、并发状态更新。

对于超大图，内存布局很重要。用字符串节点 id、map 嵌套 slice、重复边不去重，都会让常数项很大。对于动态 DAG，反复全量排序可能太贵，需要增量维护 indegree、分层展开或限制图规模。

对于分布式调度，性能边界在一致性和吞吐之间。每个上游完成都要更新下游 indegree；如果所有更新打到同一行或同一把锁，宽 fan-in 会成为热点。为了吞吐做异步更新，又要处理重复、乱序和崩溃恢复。

我会总结：topological sort 保证偏序，不保证调度最优；性能上算法复杂度只是底线，真正边界在图规模、存储模型、ready 更新和分布式状态一致性。

## Q011. 面试官如果只问一个问题检验你是否理解 deterministic replay，可能会问什么？

**回答：**

我会预期这个问题：

```text
一个 workflow 第一次运行时先 sleep，再调 Activity。运行到 sleep 期间 worker 重启了；你部署了新代码，把顺序改成先调 Activity 再 sleep。系统从 event history replay 时会发生什么？如果 workflow 代码里直接读当前时间、随机数或外部 API，又会发生什么？
```

这道题能测出候选人有没有理解 replay 不是“重新执行一遍业务”。deterministic replay 的核心是：用已有 event history 重新驱动 workflow 代码，让它发出和历史相匹配的命令序列，从而恢复到之前的逻辑状态。它重跑代码，但不能重做外部副作用。

第一次运行时，workflow 代码发出了 `StartTimer` 之类的命令，服务端记录了对应事件。worker 重启后，SDK 会从头执行 workflow 函数，但当代码走到 sleep 时，不是真的重新创建一个全新历史，而是把当前发出的命令和 event history 里对应位置的事件对齐。如果匹配，replay 继续推进。

如果你把代码改成先调 Activity，replay 时第一个命令变成 schedule activity，而历史里第一个事件是 timer started。命令和历史对不上，系统会报 nondeterminism。这里不是业务失败，而是代码已经无法解释已有历史。

直接读当前时间、随机数或外部 API 也会出问题。第一次运行时本地时间是上午，replay 时可能是下午；第一次随机数走 A 分支，replay 时走 B 分支；第一次外部 API 返回可用库存，replay 时返回不可用。只要这些值影响了 workflow API 命令序列，就可能让 replay 发出不同命令。

正确做法是把不确定操作放到可记录的边界里。外部 I/O 放到 Activity，Activity 的结果写入 history，replay 时读取历史结果而不是再调用外部系统。时间、随机数、side effect 也要用 SDK 提供的 replay-safe API，让结果成为历史的一部分。

还要强调版本。长时间运行的 workflow 可能跨多个部署版本。改 workflow 代码时不能随便增删或重排会产生命令的 API 调用，要用版本机制、patch、worker versioning 或兼容分支，让旧历史仍能被旧逻辑解释。

所以我会回答：deterministic replay 要求同一份历史驱动 workflow 代码时，代码产生同一串可匹配的命令。worker 重启没关系，外部服务重试也不是重点；真正危险的是本地非确定性和未版本化的代码变更。

## Q012. deterministic replay 的一句话定义是否容易误导，误导点在哪里？

**回答：**

常见定义是：deterministic replay 就是重放历史得到同样状态。这个说法方向对，但容易让人误会成普通 event sourcing reducer，或者误会成重新执行所有动作。

第一个误导点是把 replay 当成重做业务。workflow replay 不是重新发邮件、重新扣款、重新调用下游。它应该用历史记录恢复本地决策状态。已经完成的 Activity 结果来自 history，不应该再执行一次外部 I/O。

第二个误导点是只看状态，不看命令序列。很多 workflow 引擎关注的不只是最终 state 一样，还要求 replay 时发出的命令和历史事件按位置匹配。你最后算出的变量值可能一样，但中间先 schedule activity 再 start timer，和历史里先 timer 后 activity 仍然不匹配。

第三个误导点是忽略代码版本。历史不变，代码变了，也会 nondeterministic。长期运行 workflow 在新 worker 上 replay 旧 history 时，旧历史必须仍能被当前代码解释。否则部署本身就会打断正在运行的实例。

第四个误导点是把所有确定性都交给语言。语言层面的 map 迭代、goroutine 调度、浮点边界、时间、随机数、环境变量、外部查询都可能影响分支。只要这些分支控制了 workflow API 调用顺序，就会影响 replay。

第五个误导点是把 replay reducer 和 projection 混在一起。projection 重建查询视图，错了可以重跑；workflow replay 是正在运行实例恢复的一部分，nondeterminism 可能直接让实例停住。它对兼容性和副作用边界要求更高。

所以我会把定义改成：deterministic replay 是让 workflow 代码在同一份 event history 下重新执行时，产生同样的决策和命令序列；外部副作用必须通过记录在 history 里的 API 边界进入系统，代码演进也要保持历史可解释。

## Q013. deterministic replay 最常见的生产事故触发条件是什么？

**回答：**

deterministic replay 的事故最常见于部署和非确定性逻辑。系统平时跑得好，一重启、扩容、故障恢复或升级，就突然报 nondeterminism。

第一类是未版本化代码变更。旧 workflow 已经记录了 `TimerStarted -> ActivityScheduled`，新代码改成 `ActivityScheduled -> TimerStarted`。或者删了一个 Activity，改了 Activity type，改了 child workflow id，改了 signal 顺序。只要命令序列对不上，replay 就会停。

第二类是 workflow 里直接调用时间和随机数。当前时间影响分支，随机数影响 fan-out 数量，UUID 影响 child workflow id，这些都会让 replay 和第一次执行不同。应该使用 SDK 提供的 deterministic time、random、side effect API，或把不确定结果记录进 history。

第三类是把外部 I/O 写在 workflow 代码里。直接查数据库、调用 HTTP、访问 LLM、读文件、读缓存。第一次和 replay 的返回值不同，分支就变了。外部 I/O 应该放到 Activity 或等价的可记录边界里。

第四类是容器迭代顺序不稳定。遍历 map 生成子任务、拼接 command id、决定 Activity 顺序。单次运行看不出问题，换进程、换版本、换语言 runtime 后顺序变了。需要排序或稳定 id。

第五类是 schema 和序列化漂移。历史里的 payload 用旧 schema，新代码按新字段解释；默认值改变；枚举新增后旧值落到错误分支。replay 不是只要求代码顺序不变，数据解释也要兼容。

第六类是日志、指标或 side effect 没区分 replay。重放历史时如果又打业务审计、又发 webhook、又上报“执行成功”指标，会制造重复外部影响。很多 SDK 有 replay detection，用来避免重复非业务日志或指标。

第七类是 history 膨胀。replay 本身没错，但历史太长，每次恢复都要跑很久，worker cache miss 后延迟飙升。最后被误判为业务慢，其实是 replay 成本失控。

第八类是多版本 worker 混跑没有边界。老 worker 和新 worker 都能拿到同一类 workflow task，但代码兼容性不同。旧实例被新 worker 拿去 replay，直接报 nondeterminism。

我会把触发条件总结成：deterministic replay 出事，通常是 workflow 代码依赖了 history 之外的世界，或者新代码不再能解释旧 history。重启和恢复只是把这个问题暴露出来。

## Q014. deterministic replay 的指标应该怎么设计才不会只看平均值？

**回答：**

deterministic replay 的指标要同时看正确性和成本。只看平均 replay 时间不够。大多数实例 history 很短，平均很好看；真正危险的是少数长历史、版本漂移、cache miss、nondeterminism 错误和恢复风暴。

第一组是 replay 正确性指标。nondeterminism error count、command mismatch 类型、发生在哪个 workflow type、哪个 code version、哪个 history event position。只报一个“replay failed”没有用，必须知道是命令顺序、Activity type、timer、child workflow、signal，还是 payload 反序列化出了问题。

第二组是 history 规模。每个 workflow execution 的 event count、history bytes、activity count、timer count、signal count、child workflow count。看 p95、p99、max。历史长度通常直接决定冷启动 replay 成本。

第三组是 replay latency。full replay p50/p95/p99、从 snapshot 或 continue-as-new 后的 replay p99、cache hit replay 和 cache miss replay 分开。平均值会被 cache hit 掩盖。

第四组是 worker cache。workflow cache hit rate、eviction count、cache memory、sticky task 命中率、cache miss 后恢复时间。很多 replay 成本只有 worker 重启或扩容时才爆出来。

第五组是版本维度。按 workflow type、workflow definition version、worker build id、patch marker 统计 replay 错误和耗时。部署后 nondeterminism 往往集中在某个版本组合上。

第六组是 replay 安全边界。replay 阶段是否触发了 Activity schedule 以外的外部 I/O、是否重复记录业务审计、是否重复上报业务指标、是否执行了非 replay-safe API。可以通过测试、hook 或运行时计数捕捉。

第七组是恢复风暴。worker 重启后 pending workflow task 数、每秒 replay 数、replay CPU、metadata/history store 读取 p99、恢复完成时间。系统平时没问题，故障恢复时可能被 replay 打爆。

第八组是 reducer 或 workflow code 的复杂度。每处理一个历史事件的平均耗时、p99、分配、是否有 O(N²) 扫描。历史 100 个事件时看不出来，10 万个事件时会很痛。

我会在面试里说：deterministic replay 的看板要有 nondeterminism 错误、history size、replay p99、cache miss、版本维度、外部副作用防护和恢复风暴。平均 replay latency 只能说明普通实例没事，不能证明系统可恢复。

## Q015. deterministic replay 的正确性边界和性能边界分别是什么？

**回答：**

deterministic replay 的正确性边界是：同一份输入和同一份 event history 下，workflow 代码必须产生同样的决策和命令序列。它保证 workflow 能从历史恢复逻辑状态，但不自动保证外部世界也被恢复到同样状态。

正确性上要守住几条线。

第一，workflow 代码里的控制流不能依赖未记录的不确定值。时间、随机数、外部 I/O、环境变量、无序集合迭代，只要影响命令序列，就必须被记录或稳定化。

第二，副作用必须放在可记录边界外。Activity 可以执行外部 I/O，结果写入 history；replay 时使用历史结果。workflow 本体不要直接调用外部系统。否则 replay 会重复动作或得到不同结果。

第三，代码演进必须兼容旧历史。会产生命令的 API 调用不能随便增删、重排、改类型或改 id。长运行实例需要 patch、version marker、worker versioning 或继续让旧 worker 处理旧历史。

第四，payload schema 要可演进。replay 时要能读懂旧事件。字段默认值、枚举、序列化格式、压缩方式、加密 key 都可能成为恢复边界。

第五，replay 不等于 exactly-once 外部执行。Activity 可能被重试，worker 可能在副作用完成后崩溃，客户端可能看到超时。外部副作用仍然需要幂等键、事务、outbox 或补偿。

性能边界主要来自 history 和代码复杂度。

history 越长，冷 replay 越慢，读取 history 的 I/O 越大，反序列化和命令匹配成本越高。长期运行 workflow 要考虑 continue-as-new、snapshot、history compaction 或把高频状态挪出 workflow 主历史。

replay 代码必须便宜。每个事件都全量扫描所有 step，或者每次都反序列化巨大状态，会让 replay 变成 O(N²)。workflow 正常运行时可能不明显，恢复时会集中爆发。

cache 能缓解 replay 成本，但不能作为正确性依赖。worker cache hit 时很快，重启或扩容后 cache miss 仍然要能承受。面试里如果只说“我们有缓存，所以 replay 不慢”，不够稳。

日志和指标也要控制。replay 时重复打大量日志、重复上报业务指标，会让恢复路径变慢，还会污染观测数据。

所以我会总结：deterministic replay 的正确性边界在“同历史同命令”，性能边界在 history 大小、冷启动 replay、workflow code 复杂度和版本兼容。它是恢复机制，不是副作用魔法。

## Q016. 面试官如果只问一个问题检验你是否理解 gRPC，可能会问什么？

**回答：**

我会预期这个问题：

```text
客户端通过 gRPC 调用 CreateOrder，设置了 200ms deadline，并开启了对 UNAVAILABLE 的重试。客户端最后收到 DEADLINE_EXCEEDED。请说明：服务端可能有没有创建订单？这个错误能不能重试？deadline 会不会传给下游？如果这是 streaming RPC，flow control 和 backpressure 又会影响什么？
```

这道题能一下子区分“会写 proto”和“理解 RPC 语义”。gRPC 不是本地函数调用。网络可能丢包、连接可能断、服务器可能已经执行但响应没回来、代理可能超时、客户端可能重试、服务端可能继续做后台工作。客户端看到的 status 只是一次 RPC 的可观察结果，不等于业务事务状态。

先说 `DEADLINE_EXCEEDED`。它表示客户端等到 deadline 过期还没有拿到完成结果。对于会改变系统状态的操作，这并不证明服务端没有执行。可能服务端已经创建了订单，只是响应晚了；也可能请求还没到服务端；也可能服务端收到取消后停止了。客户端不能靠 status 猜业务最终状态，必须用幂等键、查询接口或业务状态机确认。

重试也要看幂等性。`CreateOrder` 如果没有 idempotency key，自动重试可能创建两笔订单。gRPC retry policy 只能决定哪些 status 可以发起新 attempt，不能替你证明业务操作可重复。安全做法是把 create 设计成带 request id 的幂等写，或者把重试限制在读操作、无副作用操作、明确幂等的写操作。

deadline 传播是另一个重点。客户端设置 deadline 后，中间服务再调用下游时应该带着剩余预算，而不是重新给一个更长的 timeout。否则入口已经快超时，下游还被允许慢慢跑。服务端收到取消后，应用代码也要主动停止自己启动的后台工作；gRPC 可以取消 call，但不能保证你的业务 goroutine 自动退出。

如果是 streaming RPC，还要谈 flow control。gRPC 基于 HTTP/2，可以在同一连接上多路复用多个 stream，也有流控。写入 stream 成功返回不一定表示数据已经到达对端业务代码，只表示交给了框架和传输层。发送方太快、接收方不读，框架可能等待；双方都同步写又不读，还可能死锁。应用层仍然要设计消息边界、消费速率、取消和半关闭语义。

所以我会回答：gRPC 给你 service contract、status、deadline、metadata、streaming、HTTP/2 flow control 和一些 retry 能力，但它不消除分布式调用的不确定性。真正的正确性来自 deadline 预算、幂等、状态查询、明确 status 映射、取消传播和流控下的应用协议。

## Q017. gRPC 的一句话定义是否容易误导，误导点在哪里？

**回答：**

常见定义是：gRPC 是基于 HTTP/2 和 protobuf 的 RPC 框架。这个定义没错，但很容易让人把 gRPC 当成“更快的 HTTP JSON”或者“远程函数调用”。

第一个误导点是把它当成本地调用。gRPC 的代码生成让调用看起来像函数，但语义完全不同。本地函数要么返回，要么抛异常；RPC 还会遇到 deadline、取消、连接失败、负载均衡、代理、重试、半成功、服务端已执行但客户端没收到响应。

第二个误导点是把 protobuf 当成全部。proto 定义了 service 和 message schema，但生产问题常常在 deadline、status code、metadata、flow control、message size、兼容性、认证、负载均衡和可观测性上。

第三个误导点是以为 HTTP/2 multiplexing 解决所有性能问题。HTTP/2 可以复用连接和多路复用 stream，但底层 TCP 仍可能有 head-of-line blocking；一个连接上的流控、窗口、慢接收方、代理限制也会影响尾延迟。连接少不一定永远好，连接池和负载均衡仍然有意义。

第四个误导点是以为框架自动可靠。gRPC 有 status code、retry、deadline、health checking、keepalive 等机制，但业务可靠性仍要自己设计。重试是否安全、deadline 多长、错误码如何映射、幂等键怎么传，框架不会替你决定。

第五个误导点是忽略 streaming 语义。unary RPC 像一次请求响应，streaming RPC 是一个持续协议。消息顺序、半关闭、取消、flow control、接收端处理速度都会影响正确性和资源占用。

第六个误导点是 metadata 滥用。metadata 适合 trace、auth、tenant、request id 这类横切信息，不适合塞大业务字段。大字段应该进入 message body，或者更常见的是放对象存储引用。

所以我会定义得更完整：gRPC 是一个基于 IDL、HTTP/2、序列化和运行时库的 RPC 系统，提供 unary/streaming 调用、status、deadline、metadata、流控和代码生成；它简化跨进程调用，但不把远程调用变成本地调用，也不自动解决幂等、重试和业务一致性。

## Q018. gRPC 最常见的生产事故触发条件是什么？

**回答：**

gRPC 的生产事故经常来自“看起来像函数调用”的错觉。代码写成 `client.CreateOrder(ctx, req)`，人就容易忘记这是一次跨网络、跨进程、带重试和超时的交互。

第一类是没有 deadline。客户端默认可能等待很久，服务端和中间层也不知道什么时候该放弃。流量一大，慢请求堆积，连接、goroutine、worker、下游资源都被占住。服务间调用应该显式设置现实的 deadline，并向下游传播剩余预算。

第二类是 deadline 设置不合理。太长会拖垮资源，太短会制造无效请求和重试风暴。尤其是链路中每一层都重新给 1 秒 timeout，端到端就会越来越不可控。应该从入口预算往下切，而不是每层重置。

第三类是错误码乱用。把容量不足返回 `INTERNAL`，把业务校验失败返回 `UNAVAILABLE`，把认证失败返回 `UNKNOWN`。上游的重试、告警、降级都会被误导。status code 是 API 语义的一部分，不是日志字符串。

第四类是对非幂等写开启重试。服务端已经执行成功，响应在路上丢了，客户端按 `UNAVAILABLE` 或 deadline 重试，重复创建、重复扣款、重复发消息。gRPC retry policy 不能替代 idempotency key。

第五类是 streaming flow control 被忽略。发送方不断写，接收方处理慢或不读，内存和窗口被拖住；或者双方都同步写大量消息却不读，形成死锁。流式 RPC 要像协议一样设计，不能当成无限 channel。

第六类是连接和负载均衡粘性。HTTP/2 长连接复用很好，但如果客户端只连少数后端，流量会集中。服务扩容后旧连接不重新分布，某些实例热得不行，另一些很闲。

第七类是大消息。把大对象直接塞进 response，序列化、压缩、内存复制、流控窗口、代理限制一起上来。大结果更适合对象存储引用、分页或流式分片。

第八类是代理和 ingress 的超时、最大消息、HTTP/2 设置不一致。客户端、服务端、网关、负载均衡器各有 timeout 和流控配置，任何一层不匹配都可能造成莫名其妙的 reset、deadline、unavailable。

第九类是服务端收到取消后业务还在跑。gRPC call 取消了，但应用代码启动的 goroutine、数据库查询、后台任务没停。客户端以为请求结束，服务端还在消耗资源。

我会总结：gRPC 事故的触发点大多是 deadline、status、retry、idempotency、flow control、连接分布、大消息和代理配置。框架让调用更规范，不代表调用天然可靠。

## Q019. gRPC 的指标应该怎么设计才不会只看平均值？

**回答：**

gRPC 指标要按 method、status、attempt、deadline 和消息大小拆开。只看平均 RPC latency 会把网络、排队、序列化、重试、下游慢和客户端取消全部混在一起。

第一组是基础调用指标。client call duration 和 server call duration 要分开；按 service/method/status code 统计 p50/p95/p99、error rate、inflight。status 不能只分 success/fail，`DEADLINE_EXCEEDED`、`CANCELLED`、`UNAVAILABLE`、`RESOURCE_EXHAUSTED`、`INVALID_ARGUMENT` 的含义完全不同。

第二组是 attempt 指标。一次 logical call 可能有多个 retry attempt。要看 attempts per call、attempt duration、retryable status、retry throttling、server pushback、transparent retry 次数。否则客户端看一次调用，服务端看到三次请求，容量规划会错。

第三组是 deadline 预算。记录进入客户端调用时的 timeout、到服务端时剩余预算、服务端处理结束时是否已经超过 deadline、下游调用继承了多少剩余时间。很多 `DEADLINE_EXCEEDED` 的根因是请求到达服务端时已经没时间了。

第四组是链路阶段。DNS/解析、连接获取、TLS 握手、HTTP/2 stream 创建、请求序列化、客户端发送、服务端队列、handler 执行、下游等待、响应序列化、客户端接收。不是所有语言都天然暴露全部阶段，但排查 p99 时要有办法拆。

第五组是消息大小和压缩。发送/接收 message bytes、compressed bytes、payload p99、最大消息拒绝次数、压缩耗时。大消息会让平均耗时失真，也会影响同连接上的其他 stream。

第六组是 HTTP/2 和流控。active streams、connection count、flow-control wait、write blocked、recv backlog、stream reset、keepalive ping、goaway、connection age。streaming RPC 尤其要看接收端消费速度和发送端阻塞时间。

第七组是负载均衡。每个后端实例的 RPC 数、inflight、p99、错误率、连接数。HTTP/2 长连接可能让 QPS 分布和实例数不成比例，单看全局平均会漏掉热点实例。

第八组是取消和半成功。client canceled、server observed cancel、cancel 后业务继续执行时间、deadline 后成功提交的业务操作、幂等命中率。写接口尤其要看这些。

第九组是维度控制。method、status、cluster、caller、callee、tenant 这些有用；request id、user id、订单号不能放进指标 label。否则指标系统会先炸。

我会给出一套面试回答：gRPC 看板至少包含 method/status 的 p99、client/server duration、attempts per call、deadline remaining、message size、flow-control wait、active streams、LB per endpoint、retry/cancel、半成功和幂等命中。平均延迟只能做背景，不足以证明 RPC 健康。

## Q020. gRPC 的正确性边界和性能边界分别是什么？

**回答：**

gRPC 的正确性边界是 RPC 层边界，不是业务事务边界。它可以告诉你一次调用的 transport、status、deadline、metadata、stream 状态，但不能单独证明业务操作有没有最终提交。

正确性上要先承认几件事。

第一，RPC 可能失败、超时、取消、重复或半成功。`DEADLINE_EXCEEDED` 不代表服务端没执行，`CANCELLED` 不代表业务 goroutine 已经停止，`UNAVAILABLE` 不代表重试一定安全。

第二，status code 是协议语义的一部分。业务校验、并发冲突、容量不足、认证失败、下游不可用，要用不同 status 表达。乱用 status 会让上游重试和告警全错。

第三，重试只对幂等或可去重操作安全。gRPC retry 能重放调用历史，但不能保证你的业务只执行一次。写操作要有 idempotency key、条件写、事务或查询确认。

第四，deadline 是等待预算，不是回滚协议。客户端不等了，服务端可能还在处理。服务端应用要观察取消并停止后台工作，下游调用也要继承 deadline。

第五，streaming RPC 只保证单个 stream 内的消息顺序和流语义，不保证应用层消费一定跟得上，也不保证跨 stream 全局顺序。flow control 控制字节和消息发送节奏，不替你定义业务 backpressure。

第六，metadata 不是安全和业务语义的全部。认证、授权、trace context 可以放 metadata，但大业务数据、隐私字段、可变状态不应该随便塞进去。安全还要 TLS、mTLS、token 校验和服务端授权。

性能边界主要来自几层。

HTTP/2 multiplexing 降低了连接开销，但不是无限并发。单连接的流控、拥塞控制、TCP 层丢包、代理限制和慢接收方都会影响 p99。连接池、负载均衡和最大并发 stream 仍然需要调。

protobuf 序列化比 JSON 常常更省，但大消息仍然贵。序列化、压缩、拷贝、内存峰值、GC、窗口等待都可能成为瓶颈。大对象更适合引用、分页或分块 streaming。

deadline、retry、hedging 会影响负载。合理重试能掩盖瞬时故障，错误重试会放大过载。hedging 能降低尾延迟，也会增加服务端请求量。性能优化必须和容量预算一起算。

拦截器、日志、trace、认证也在热路径上。每次 RPC 都做重 I/O、高基数指标、复杂鉴权或大对象日志，会直接推高 p99。

代理和跨网络部署会增加边界。Ingress、service mesh、LB、NAT、TLS、DNS、keepalive、max message size、idle timeout 都可能改变实际行为。gRPC 应用不能只测直连本地。

所以我会总结：gRPC 的正确性边界是“把远程调用的结果和失败用明确协议表达”，业务一致性还要靠幂等、状态机和事务；性能边界在 HTTP/2 连接、流控、序列化、消息大小、重试负载、代理和观测开销。它是强工具，不是分布式系统免责卡。