# 十四、简历与答辩风险问题：容易被质疑的问题

这一组问题比上一组更尖锐。回答时不要急着否认质疑，先承认项目边界，再把自己的设计选择讲清楚。对成熟系统的比较也要克制：LogServe 是一个围绕 shared log、replay、workflow、actor 和 LLM scheduling 做的实验型 runtime，不是 Temporal、Kafka、Ray、vLLM 的替代品。

## Q951. 这个项目是不是功能堆太多但每个都不深？

我会先承认这个风险：从模块列表看，LogServe 确实覆盖了 task、workflow、actor、LLM、benchmark、dashboard、Kubernetes manifests，容易给人一种“面太宽”的感觉。所以答辩时不能按功能清单逐个念，而要把主线讲清楚。

这个项目的主线是 shared log。后面的模块都围绕同一件事展开：状态变化先进入 append-only log，再由 control plane materialize 成当前视图；系统故障后，通过 replay 重建 workflow、actor、LLM stats 和 backpressure 配置。task 是执行单元，workflow 是多 step 组合，actor 是有状态对象，LLM scheduling 是 AI runtime 的调度场景。它们看起来不同，但都在用同一套 log-first、replay、idempotency、fencing 的语义。

深度主要体现在几个点上：

1. shared log 承担的角色超过普通日志输出：它是带有 segment/index/recovery/idempotent append/trim 的事件存储。
2. control plane 不把 metadata 当 source of truth，而是从 log bootstrap。
3. workflow 支持 DAG step、retry、timeout、result ref、replay 校验和 duplicate final result 去重。
4. actor 有 `ActorCommandSubmitted`、`command_seq`、mailbox、snapshot replay、logical trim 和 epoch fencing。
5. LLM scheduling 不只调用 mock LLM，还把 model cache、checkpoint cache、event log 和 materialized EWMA stats 接进调度决策。
6. fault injection 和 benchmark 有脚本产物，不只停留在 README。

我也会说清楚没有做深的部分：logd 没有多副本复制，Kubernetes 没有压测，vLLM 没有真实 GPU 实验，Python executor 没有沙箱，多租户和权限还没做。这些是生产化工作，不会在面试里包装成已完成。

所以我的回答是：项目没有按热门词堆模块，核心围绕“shared log 驱动的可恢复 AI runtime”展开。它的深度不在每个方向都做到工业级，重点是几条路径共享同一套恢复和一致性设计。

## Q952. 为什么不直接用 Temporal？

Temporal 是成熟的 workflow 平台，生产系统要做强 durable workflow，我会优先考虑它。LogServe 的目标不是证明 Temporal 不好；我想亲手实现一条从 shared log 到 workflow/actor/LLM runtime 的主链路。

两者关注点不同。Temporal 的核心是 durable execution、workflow history、deterministic replay、activity retry、worker task queue 等。它已经把 workflow 领域做得很深。LogServe 的重点是把 workflow、actor、LLM scheduling 都放到同一个 shared log 语义下：task stream、workflow stream、actor stream、LLM event stream 都能 replay，metadata view 可以重建。

还有一个差异是 AI infra 场景。LogServe 里 LLM task 会考虑 worker model cache、checkpoint cache、cold start、cache hit rate、model load latency 和 predicted-latency stats。Temporal 可以编排 LLM 调用，但它本身不会替我管理 worker-local checkpoint cache，也不会直接做模型缓存感知调度。

如果项目目标是上线一个可靠 workflow 产品，我不应该重新造 Temporal。这个项目的目标是展示我对 event sourcing、log-first、恢复语义、actor fencing 和 AI serving locality 的理解。Temporal 是参照系，LogServe 是为了把底层机制亲手实现出来。

## Q953. 为什么不直接用 Kafka 做 shared log？

生产环境里，Kafka 是很自然的选择。它已经解决了 broker replication、consumer offset、segment retention、吞吐、运维工具等问题。LogServe 自研 logstore 的目标并非取代 Kafka；我需要把 shared log 这层语义做成可控的项目核心。

我需要的能力有几类：按 stream 写事件、stream 内单调 seq、idempotent append、从 seq 读取、启动恢复、partial tail truncation、logical trim、compactable bytes 统计。自己实现以后，workflow、actor、LLM 的 replay 路径可以直接围绕这些 API 设计。

用 Kafka 也能做。可以把 `stream_id` 映射成 topic 或 key，把 `seq` 映射成 partition offset，把 metadata view 做成消费者 materialized view。但这样项目的工程重心会变成 Kafka 集成：topic 管理、partition 选择、consumer group、offset commit、exactly-once producer 配置、broker 运维。对这个项目来说，我更想展示的是日志存储与 runtime 恢复的闭环。

我会承认当前 logstore 的边界：单机，没有复制，没有 leader election，durability 取决于 fsync policy 和本地磁盘。生产化可以有两条路：一条是把 logd 改成 Raft 多副本；另一条是直接换 Kafka/Pulsar 这样的成熟日志系统，把 LogServe 的 control/workflow/actor/LLM 语义保留在上层。

## Q954. 为什么不直接用 Ray 做分布式执行？

Ray 很适合分布式 Python 执行、actor 和并行计算。它的优势是执行层和资源调度，尤其适合 ML workload。LogServe 的关注点不完全一样。

LogServe 要解决的是“状态怎么恢复”和“事件如何解释系统历史”。它把 task、workflow、actor、LLM 的状态变化写进 shared log，然后从 log 重建 metadata view。worker 执行可以失败，completion 可以重复到达，但最终状态提交要靠 idempotency、lease epoch、actor epoch 和 command_seq 来收敛。

Ray 的 actor 是内存中有状态对象，也有 fault tolerance 机制，但我的项目重点是把 actor command log、snapshot replay、logical trim、epoch fencing 这条链路显式写出来。workflow 也是同理：LogServe 关注 step event log、result ref、replay consistency，而不是单纯把函数分发到远端进程。

如果要生产化，Ray 可以成为 LogServe 的 executor backend。LogServe control plane 负责 log-first、workflow/actor 语义和调度策略，Ray 负责底层分布式执行和资源管理。两者不是非要二选一。

## Q955. 为什么 LLM serving 不直接用 vLLM/Ray Serve？

真实 LLM serving 我会用 vLLM、Ray Serve、Triton 或 KServe 这类成熟组件。LogServe 里实现 LLM serving，目标不是自己写一个更强的模型服务器；重点是把 LLM 请求纳入 runtime 的调度和恢复框架。

vLLM 解决的是模型推理效率，比如 continuous batching、paged attention、OpenAI-compatible API、显存管理。Ray Serve 解决的是模型服务部署、replica、路由和 autoscaling。LogServe 关注的是另一层：一个 workflow 里产生 LLM step 后，应该派给哪个 worker；这个 worker 是否已有模型 checkpoint；cold miss 是否要拉 checkpoint；模型加载和 first token latency 是否写入 event log；control 重启后 predicted-latency stats 能不能从 `LLMCompleted` 重建。

所以 LogServe 的 vLLM adapter 只是调用边界。真正的推理可以由 vLLM 完成，LogServe 不抢 vLLM 的工作。LogServe 更像在 vLLM 外面加了一层 workflow/actor/log-first runtime 和 locality-aware scheduling。

当前实验没有真实 GPU，只有 mock LLM 和 checkpoint cache probe。这个边界要直接讲。mock 实验验证的是调度和事件路径，真实 serving 性能还需要 vLLM/GPU 实验补上。

## Q956. 你实现的 shared log 有没有复制和高可用？

没有。当前 logd 是单机 shared log，没有多副本复制，也没有 leader election。

这意味着 logd 是可用性瓶颈。logd 挂掉时，control 的 log append 应该失败，新的状态变化不能继续提交；如果使用 `fsync=batch` 或 `interval`，进程崩溃时还存在丢失最近已返回写入的风险，这取决于具体成功语义和 flush 时机。项目里已经通过 fsync policy 和 recovery 测试展示了单机 crash recovery 的一部分，但没有解决副本容灾。

如果继续做生产化，我会优先给 logd 加复制。比较稳妥的做法是 Raft：一个 leader 接收 AppendLog，写入本地 log 后复制到多数 follower，达到 quorum 后返回成功。control 的 metadata view 更新相当于 apply log 后的 materialized view 更新。ReadLog 如果需要 linearizable read，就读 leader 或走 read index；如果读 follower，就要接受可能读到旧数据。

所以简历里应该写“shared log service / single-node logstore”，不要写“高可用日志系统”。它现在的价值在于实现了 log-first runtime 的语义基础，不在于完成了生产级复制。

## Q957. 没有多机实验，为什么说是 distributed runtime？

我会把这个词限定清楚。当前实验是单机多进程，不是多物理节点集群。logd、control、worker 是独立进程，通过 gRPC 通信；实验里启动 3 个 worker，验证 worker polling、lease、redelivery、actor ownership、LLM locality scheduling。这些机制属于分布式 runtime 的控制语义，但实验环境还没有覆盖跨机器网络。

所以比较准确的说法是：LogServe 的架构是 distributed runtime 架构，当前验证是 single-node multi-process experiment。它有 control plane、worker、log service、SDK、gRPC 边界、worker identity、heartbeat、lease、epoch fencing、redelivery，这些设计都是为了多 worker 环境准备的。

我不会说它已经完成多机生产验证。多机实验还需要补：独立机器上的 logd/control/worker，真实网络延迟，worker 节点掉线，logd 数据盘恢复，control leader 选举，worker cache 丢失，跨节点 model checkpoint fetch。当前单机实验只能说明代码路径和机制闭环跑通。

## Q958. mock LLM 的实验有什么意义？

mock LLM 的意义是隔离变量。

如果一开始就接真实 GPU，很多因素会混在一起：模型权重大小、显存、batching、prompt length、decode tokens、GPU OOM、vLLM 内部调度、网络延迟。这样很难判断 LogServe 自己的调度和事件路径有没有问题。

mock LLM 先验证几件事：

1. LLM request 能作为 task 进入 control queue。
2. worker 能根据 model name/version 判断本地 cache。
3. cold miss 会触发 checkpoint fetch。
4. warm request 会命中 checkpoint cache。
5. `ModelLoadStarted/ModelLoaded/LLMCompleted` 会写入 `llm:<task_id>`。
6. `ReplayLLM` 能重建 cache hit、model load、first token、total latency。
7. locality-aware scheduler 比 resource-only 更容易选中 cached worker。
8. RAG workflow 里 `llm_generate()` 可以作为 DAG step。

mock 实验不说明真实 GPU 性能，也不说明 vLLM 的吞吐。它说明 LogServe 的 runtime plumbing 是通的。后续接真实 vLLM 时，才去回答 prefill/decode、continuous batching、显存和 token throughput 这些问题。

## Q959. task throughput 只有 5.17 tasks/s，是否说明系统性能很差？

不能直接这么判断。

这个 5.17 tasks/s 来自单机实验里的端到端 benchmark，workload 很小，目的是把 workflow、task、actor、LLM、checkpoint、dashboard、fault injection 等路径跑通。它不是为极限吞吐调优的压测，也没有固定大规模并发、批量提交、长时间压测和多轮置信区间。

端到端 task throughput 会被很多东西影响：benchmark 脚本的请求数、worker 数、Python runner 执行开销、gRPC 往返、control 队列锁、log append、result store、等待轮询间隔、测试里人为的 mock latency。5.17 tasks/s 更像“这组实验参数下的端到端观测值”，不能推成系统上限。

如果面试官追问性能，我会说：当前项目的性能结论主要看相对指标和机制指标，例如 snapshot replay 从 21 command 降到 1 command、locality-aware cache hit rate 从 0.833 到 1.0、resource-only p95 305ms 对比 locality-aware 205ms、logstore fsync policy 对 append throughput 的影响。task throughput 需要单独设计压测才能回答。

后续要测 task throughput，需要提高 workload 规模，关闭不必要的 debug log，增大 worker pool，固定任务执行时间，分离 Go task 和 Python task，报告 p50/p95/p99、CPU、锁竞争、queue wait 和 executor time。现在这个数字不能当成性能好坏的最终判断。

## Q960. logstore benchmark 高吞吐和 task throughput 低之间怎么解释？

它们测的是两层东西。

logstore benchmark 是 microbenchmark。它只测 append/read/recovery，payload 固定，路径短，batch/interval fsync 下可以把顺序写吞吐跑得很高。实验里 20,000 records、16 streams、256-byte payload，batch/interval append records/s 到了 20 万量级。

task throughput 是端到端 benchmark。一个 task 从 SDK 或脚本提交开始，要经过 control gRPC、TaskSubmitted log、metadata update、queue、worker poll、lease、StartTask、Python runner 或 mock executor、CompleteTask、TaskCompleted log、workflow/actor/LLM 上层状态推进。一个 workflow step 还会有 step event，一个 actor command 还会有 command_seq 和 actor state，LLM task 还会有 model load event。

所以 logstore 高吞吐只能说明日志写入不是当前 microbenchmark 下的瓶颈，不代表整个 runtime 每秒能处理同样数量的 task。端到端路径里，控制面锁、worker poll、Python 执行、gRPC、result store、调度策略都会消耗时间。

答辩时我会用这个解释：logstore benchmark 是底层存储能力测试，task throughput 是系统链路测试。两者不矛盾。前者告诉我 fsync policy 的影响很大，后者告诉我 runtime 还需要针对控制面、worker pool 和执行器做系统级优化。

## Q961. actor owner lease 750ms 是否拍脑袋？

750ms 更准确地说是实验参数，不是生产默认值。

actor owner lease 用来判断 owner worker 是否失联。设置太短，worker 短暂 GC pause、调度抖动或网络延迟就可能被误判死亡，导致 owner 过早转移；设置太长，真实 worker 挂掉后恢复慢，actor mailbox 堆积时间变长。

当前单机实验里，750ms 能让 failover 测试快速完成，也能覆盖 stale owner completion 被 epoch fencing 拒绝的路径。它适合测试，不代表线上应该固定用这个值。

生产里要根据 heartbeat interval、网络 p99、GC pause、worker 执行模型和业务恢复目标来定。比较稳的做法是：

1. heartbeat interval 设为 lease 的 1/3 或 1/4。
2. lease 大于常见网络抖动和 GC pause 的 p99。
3. 连续多次 missed heartbeat 再转移 owner。
4. owner transfer 写入 shared log 或强一致 metadata。
5. 转移后仍用 epoch fencing 拒绝旧 completion。

所以如果被问，我不会说 750ms 是最佳值。它只是单机实验中为了快速暴露恢复路径而选的值。

## Q962. Python 源码远程执行是不是很危险？

是的，这是当前项目最明显的安全边界之一。

LogServe 的 Python SDK 会读取 function source 或 module source，把它放进 TaskSpec，worker 侧 Python runner 负责执行。这个模式适合实验和可信用户环境，但不能直接用于不可信多租户生产环境。风险包括读取宿主机文件、访问内网、发起 SSRF、消耗 CPU/内存、写磁盘、泄露环境变量、依赖包供应链风险，以及 stdout/stderr 干扰协议。

当前项目没有声称已经解决这些安全问题。它的重点是 runtime 语义，不是 sandbox。生产化需要把执行环境换成隔离边界更清楚的形式，例如：

1. 每个 task 在容器或 Firecracker microVM 里运行。
2. 限制文件系统挂载，只给只读代码和临时目录。
3. 限制网络出口，阻断内网元数据服务。
4. 设置 CPU、内存、进程数和运行时间限制。
5. secret 通过受控环境注入，不进入 shared log。
6. 依赖通过镜像或打包 artifact 固定版本。
7. stdout/stderr 单独采集，不和控制协议混在一起。

所以答辩时我会主动说：当前 Python 源码执行适合可信实验环境。它展示 SDK 到 worker 的执行链路，安全沙箱是下一步必须补的生产化内容。

## Q963. 把 function_source 放进 TaskSpec 是否会导致日志膨胀？

会。这是早期实现为了让 demo 自包含做出的取舍。

把 `function_source` 或 `module_source` 放进 TaskSpec 的好处是 worker 不需要提前部署用户代码。SDK 提交后，worker 直接拿到源码和参数就能执行，简单任务、workflow demo、actor demo 都容易跑通。它对本地实验很友好。

代价也很明显。TaskSpec 会变大，`TaskSubmitted` 事件会变大，shared log 和 metadata view 都会膨胀。多个 task 如果来自同一个模块，会重复写相同源码。源码里如果误包含密钥，也会进入 log，后续很难删除。模块很大时，gRPC message size、log append latency、replay 时间都会受影响。

更长期的设计应该是 code artifact。用户代码打包成 zip、wheel、OCI image 或对象存储 artifact，TaskSpec 只放：

```text
code_ref
code_hash
entrypoint
runtime_env
dependency_lock
```

worker 根据 `code_ref` 拉取 artifact，并用 `code_hash` 校验内容。这样 shared log 记录的是可审计引用，而不是每次重复存源码。当前实现保留 `function_source`，是为了简化实验闭环，不是最终生产形态。

## Q964. logical trim 不删除文件，为什么还叫 retention？

这里的 retention 指的是逻辑保留边界，不是物理磁盘回收。

actor snapshot 创建后，系统知道 snapshot 之前的很多 actor command 已经不再是 replay 必需路径。于是它调用 `TrimStream(stream_id, before_seq)`，把这个 stream 在某个 seq 之前的记录标记为 trimmed。默认 `ReadLog` 不再返回 trim point 之前的记录，`ReplayActor` 可以从 `ActorSnapshotCreated`、snapshot object 和 tail log 开始恢复。

这个动作有两个价值。

第一，降低 replay 成本。恢复 actor 时，不再从 `ActorCreated` 开始扫完整历史，而是从 snapshot 后的 tail log 开始。

第二，给 physical compaction 提供依据。logstore 可以统计 compactable records/bytes，dashboard 和 benchmark 能看到哪些数据已经可以被压缩。

但它现在确实不释放磁盘空间，因为 segment file 可能同时包含多个 stream 的记录。真正 physical compaction 要保证某个 segment 里的所有记录都不再被任何 stream 的 replay 或审计读取需要使用，才能重写或删除。还要考虑 snapshot 对象是否可靠、审计读是否要保留旧事件、retention metadata 本身是否持久。

所以我会说：当前是 snapshot-aware logical retention，不是 physical compaction。名字里的 retention 表示系统已经知道哪些事件不再参与默认 replay；磁盘回收是下一步。

## Q965. metadata view 如果从 log 重建，为什么还需要 PostgreSQL？

因为 log 和 metadata view 的职责不同。

shared log 适合做 source of truth。它记录发生过的事件，保证恢复时有历史可重放。它不适合直接承担所有在线查询。比如 dashboard 要查所有 task、按状态筛 workflow、列 actor、看 worker cache、做分页和过滤，如果每次都从 log stream 扫描，会很慢，也不方便建立索引。

PostgreSQL 的作用是 materialized view 后端。control 把 replay 或实时事件应用到 PostgreSQL 表里，用户查询和 dashboard 从表里读当前状态。表可以有索引、分页、条件查询和事务更新。它服务读路径和运维查询，不改变 source of truth 的定义。

如果 PostgreSQL 表被删或损坏，系统可以在 log 还完整的前提下重建 view。这也是 log-first 的价值。反过来，如果 shared log 丢了，只剩 PostgreSQL，很多历史、replay、审计和一致性检查就丢失了。

所以我的回答是：PostgreSQL 的定位不是状态源，它负责查询效率、持久化 view 和 dashboard 体验。内存 metadata store 适合本地测试，PostgreSQL 适合更长时间运行和更复杂查询。两者都不应该替代 shared log。
