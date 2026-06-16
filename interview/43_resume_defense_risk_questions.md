# 十四、简历与答辩风险问题：简历措辞可能引发的追问

这一组问题的重点在于把话说准。面试官看到 shared log、log-first、exactly-once-ish、vLLM、Kubernetes、benchmark 这些词，通常会顺着问实现深度和边界。我的回答原则是：先讲已经落地的代码路径，再讲实验数据，最后主动交代还没有覆盖的部分。

## Q936. 你简历中写 shared-log-based AI runtime，shared log 的实现深度在哪里？

我会先把 shared log 讲成一个真正参与恢复语义的组件，而不是一个普通日志文件。

LogServe 里的 logd 提供的是按 stream 组织的 append-only log。每条记录都有 `stream_id`、stream 内单调递增的 `seq`、`event_type`、`idempotency_key`、payload、timestamp 和校验信息。上层的 task、workflow、actor、LLM 都会把状态变化写成事件，例如 `task:<task_id>`、`wf:<workflow_id>`、`actor:<actor_id>`、`llm:<task_id>`。metadata view 可以丢，可以重建；这些事件流才是恢复时最先读取的东西。

实现深度主要在四块。

第一，存储层不是把所有内容塞进内存。logstore 使用 segment file 滚动写入，index file 保存 stream 到 segment offset 的索引，读路径通过 index 回到 segment 读取 payload。启动恢复时会扫描 segment，重建内存索引，遇到 partial tail 会 truncate 到最后一条完整记录。这样做以后，进程重启不依赖旧内存状态。

第二，写入有幂等语义。`AppendLog(stream_id, idempotency_key, payload)` 如果收到重复 key，会返回已有 seq，并标记 duplicate。上层重试 AppendLog 时，不会因为 RPC 超时多写一条语义相同的事件。

第三，有耐久性策略。logd 支持 `always`、`batch`、`interval` 三种 fsync policy，实验里也对比了吞吐差异。单机 benchmark 下，always 大约 1.7k records/s，batch/interval 在这个 workload 下达到 20 万 records/s 量级。这个数字只说明 logstore microbenchmark，不代表端到端 task throughput。

第四，已经加了 snapshot-aware retention 的基础。logstore 支持 `TrimStream(stream_id, before_seq)` 这样的 logical trim，并提供 compactable records/bytes 统计。actor snapshot 创建后会记录 trim point，replay 从 snapshot 加 tail log 开始。当前还没有做 physical compaction，所以它是 retention metadata，不是磁盘空间立即释放。

所以我会说：shared log 的实现深度在“可恢复事件存储 + 幂等 append + segment/index/recovery + fsync policy + logical trim”，它已经承载了 runtime 的恢复语义，但还不是多副本 Kafka 或 Raft log。

## Q937. 你写 log-first control plane，哪些代码路径体现了 log-first？

我会用几条路径回答，避免只说口号。

`SubmitTask` 的路径是最直接的。客户端提交 task 时，control 先把 `TaskSubmitted` 写入 `task:<task_id>` stream，payload 里包含 `TaskSpec` 和幂等 fingerprint。log append 成功后，才把 task 放进 materialized metadata view 和调度队列。这样即使 metadata 更新失败，control 重启后也能从 `TaskSubmitted` 重建 task。

workflow 也是类似思路。workflow 提交后会写 workflow 事件；step 被调度时，会写 `StepScheduled`，并为底层 task 写 `TaskSubmitted`。step 成功时写 `StepSucceeded`，workflow 完成时写 `WorkflowCompleted`。workflow 的当前状态不是只存在 map 里，`ReplayWorkflow` 可以从 `wf:<workflow_id>` 事件流还原 step 状态、attempt、result ref 和最终状态。

actor 路径更能体现 log-first。创建 actor 时写 `ActorCreated`；分配 owner 时写 `ActorOwnershipGranted`；提交命令时先写 `ActorCommandSubmitted` 并分配 `command_seq`；命令执行完成后，只有通过 owner 和 epoch 检查，才写 `ActorCommandApplied`。snapshot 也是先把对象写到 result store，再写 `ActorSnapshotCreated`，之后 metadata view 才更新 snapshot 信息。

LLM 路径也有事件流。worker 执行 LLM task 时写 `ModelLoadStarted`、`ModelLoaded`、`LLMCompleted` 到 `llm:<task_id>`。control 里的 predicted-latency stats 可以从这些完成事件重建。

还有 backpressure。`SetBackpressure` 会先写 `BackpressureConfigured` 到 `system:backpressure` stream，再更新内存配置。control 重启时可以恢复最后一次 backpressure 配置。

一句话概括就是：只要这个状态会影响恢复、调度或审计，代码都尽量先把事件落到 shared log，再更新 metadata view。metadata 是读模型，log 是状态来源。

## Q938. 你写 replay 重建状态，能否举一个 control 重启恢复的完整例子？

可以用普通 task 加 workflow 的例子讲。

假设用户提交一个 `simple_rag` workflow，DAG 是 `embed -> search -> generate_mock`。第一次 control 运行时，已经完成了 `embed`，然后 control 或 worker 进程重启。重启后，control 不应该把 workflow 当成全新请求，也不应该重新执行 `embed`。

恢复过程大致是这样：

1. control 启动后连接 logd。
2. `BootstrapFromLog` 读取系统相关 stream，例如 model、worker、scheduler、backpressure，再读 task、workflow、actor、LLM stats 相关 stream。
3. 对 workflow，它读取 `wf:<workflow_id>` 里的事件。`WorkflowSubmitted` 还原定义和输入，`StepScheduled` 还原 step 和 attempt，`StepStarted` 还原开始时间，`StepSucceeded` 还原已经成功的 `embed` 结果。
4. 对 task，它读取 `task:<task_id>` 里的 `TaskSubmitted/TaskStarted/TaskCompleted/TaskFailed`，重建 task spec 和当前状态。
5. 如果发现某个 task 处在 running 但对应 worker 已经不活跃，或者 control 重启后无法确认它还被有效 lease 持有，就会把它恢复成可重新投递的状态。
6. workflow engine 再根据 DAG 依赖计算 ready steps。因为 `embed` 已经 succeeded，所以不会重新调度 `embed`，而是继续调度 `search` 或后续 step。

项目里有一个对应测试：第一个 worker 完成 `embed` 后停止，第二个 worker 接管后 workflow 从 `search`/`generate_mock` 继续，最后完成，并且 `embed` attempts 仍然是 1。

这个例子能说明 replay 的作用：control 的内存队列和 metadata view 可以丢，但只要 shared log 还在，系统可以重建“哪些事情已经发生过”，再从未完成的地方继续。

## Q939. 你写 exactly-once-ish，这个 ish 具体是什么意思？

这个词是故意写得保守。

LogServe 不宣称严格 distributed exactly-once。worker 执行用户函数这件事本身是 at-least-once 的：任务可能执行成功后 CompleteTask 超时，worker 可能重试；worker 挂掉后 control 可能 redeliver；旧 attempt 也可能在新 attempt 后面返回。所以“函数执行绝对只发生一次”这个保证没有成立。

`ish` 指的是结果提交层尽量做到 effectively-once。系统用幂等 key、lease epoch、actor epoch、command_seq 和 terminal state 保护，把重复执行带来的影响挡在状态提交层。

workflow 里，同一个 step 的最终结果按：

```text
workflow_id + step_id + input_hash
```

去重。重复 successful completion 到达时，不会再写第二个 `WorkflowCompleted`，也不会覆盖 final result。

actor 里，命令应用按：

```text
actor_id + actor_call_id + applied
```

去重，同时 completion 必须满足当前 owner、当前 epoch，以及 `command_seq == command_count + 1`。旧 worker 的完成请求会被拒绝。

普通 task 里，`task_lease_epoch` 防止 redelivery 后旧 completion 污染状态。metadata store 也会保护 terminal status，不让一个已经 succeeded/failed 的 task 被后到事件随便覆盖。

所以我的说法是：执行层至少一次，状态提交层幂等和 fencing，结果语义接近 exactly-once。外部副作用如果不幂等，平台不能替用户兜底，用户需要把副作用设计成幂等 API 或放到可补偿事务里。

## Q940. 你写 actor snapshot replay，snapshot 存在哪里？如何保证可用？

actor snapshot 通过 result store 存，不直接塞进 actor log。默认本地开发环境使用 filesystem-backed `local://` result store；Compose 里有 MinIO，项目也保留了 S3-compatible adapter 的边界。snapshot log 事件里保存的是 `snapshot_ref`、snapshot 对应的 command count 等 metadata。

写入顺序是：先把 snapshot 对象写到 result store，拿到 `snapshot_ref`；再向 `actor:<actor_id>` stream 写 `ActorSnapshotCreated`；最后更新 metadata view。这样 replay 看到 `ActorSnapshotCreated` 时，理论上已经有对象可读。

恢复时，`ReplayActor` 会读取 actor stream。如果发现 snapshot 事件，就通过 result store 加载 snapshot state，再回放 snapshot 之后的 tail command。这样就不需要从 actor 创建开始读完整命令历史。实验里出现过 full replay 21 条 command，snapshot replay 1 条 command 的对比。

可用性边界要说清楚。当前能保证的是：在本地/MinIO adapter 正常工作的前提下，snapshot ref 可以被 replay 读取。还没有完整实现对象 checksum、对象引用计数、mark-and-sweep 清理、跨副本对象存储容灾。如果 snapshot 对象丢失，而 actor stream 又做了 logical trim，replay 会失败；所以真正生产化前，必须给 snapshot 对象加 checksum、生命周期保护和一致性检查，physical compaction 前也要确认 snapshot 持久可靠。

## Q941. 你写 epoch fencing，和普通锁有什么区别？

普通锁解决的是“现在谁能进入临界区”。epoch fencing 解决的是“旧 owner 即使后来恢复，也不能用过期身份提交状态”。

actor ownership 里，每个 actor 有 `owner_worker_id` 和单调递增的 epoch。worker 失联超过 owner lease 后，control 会把 actor owner 转给另一个活跃 worker，并把 epoch 加一。旧 worker 如果只是网络卡住或 GC pause，后面可能还会把执行结果发回来。这个时候不能只看“它以前拿过锁”，必须检查它带回来的 epoch 是否仍然是当前 epoch。

所以 actor completion 需要同时满足：

```text
worker_id == current owner_worker_id
actor_epoch == current epoch
```

不满足就拒绝，错误是 stale actor completion。task 里也有类似的 `task_lease_epoch`，防止 redelivery 后旧 worker 完成任务。

锁更像本地并发控制，epoch fencing 更像分布式 lease token。它能处理“旧请求晚到”的问题。即使两个 worker 都以为自己还在执行，最终只有持有最新 epoch 的 completion 能写入 actor state。

当前边界也要承认：如果有两个 control 实例同时 grant ownership，而没有 leader election 或强一致 metadata，那么仍可能出现 split-brain。现在的实验假设单 control。生产化要引入 control leader、Raft/etcd lease，或者把 ownership grant 放到一个有线性一致性的存储里。

## Q942. 你写 locality-aware scheduling，调度分数怎么计算？

LogServe 有三种 LLM 调度策略。

`RESOURCE_ONLY` 只看 worker 是否有空闲 capacity。它不会关心某个 worker 是否已经有模型缓存，所以在模型冷启动场景下可能把请求派给没有缓存的 worker。

`LOCALITY_AWARE` 会把模型缓存放进调度决策。直观上，它优先选已经缓存目标模型的 worker；如果 cached worker 有可用 capacity，就尽量派给它；如果 cached worker 都很忙，才考虑 cold worker。打分会考虑 cache hit、资源空闲情况和队列等待。目标是减少 cold start，同时避免因为盲目追缓存把所有请求堆在一个 worker 上。

`PREDICTED_LATENCY` 更进一步。它不只看有没有 cache，而是使用 materialized LLM stats 估算每个 worker 对某个模型的总延迟：

```text
predicted_latency =
  ewma_total_latency_ms
  + queue_penalty
  + cold_start_penalty
  + eviction_penalty
```

stats 的 key 是 `(model_name, model_version, worker_id)`，字段包括 request count、cache hit count、EWMA total latency、EWMA model load latency、EWMA checkpoint fetch latency 和 last update time。它由 `LLMCompleted` 事件增量维护，调度时只需要对当前活跃 worker 做 O(worker 数) 查询，不需要每次扫描所有 `llm:*` stream。

实验上，在 3 worker、模型缓存不均匀的设置里，resource-only 的 cache hit rate 是 0.833，locality-aware 和 predicted-latency 是 1.0；resource-only p95 是 305ms，locality-aware/predicted-latency 是 205ms。这个实验样本小，只能说明机制方向，不能当成生产性能结论。

## Q943. 你写 predicted-latency，预测公式是否可靠？

它是一个工程启发式，不是严格的排队模型。

公式里用 EWMA 表示历史延迟，用 queue penalty 表示当前 worker 排队压力，用 cold start penalty 表示没有缓存时的额外开销，用 eviction penalty 表示 cache 紧张带来的风险。好处是实现简单，热路径成本低，control 重启后可以从 LLM event log 重建 stats。

可靠性边界有几个。

第一，历史样本少时容易误判。某个 worker 只跑过一两个很短请求，EWMA 可能看起来很好，但不代表它长期更快。后续应该给样本数小的 worker 加默认保守估计，或者引入探索策略。

第二，当前公式没有充分考虑 prompt length、max tokens、batch size、GPU memory、prefill/decode 分离、vLLM continuous batching 等真实 serving 因素。mock LLM 里这些变量都被简化了。

第三，queue penalty 现在主要来自 worker 和本地队列信号，还不是完整的排队论模型。真实场景要拆分 queue wait、model load、checkpoint fetch、prefill、decode、network latency。

所以我会这样说：predicted-latency 的价值在于把调度从“只看缓存”推进到“基于历史完成事件的在线估计”，并且避免热路径 replay-all。它目前适合项目实验和 mock serving，接入真实 GPU 后需要用更完整的指标校准公式。

## Q944. 你写 vLLM adapter，是否跑过真实 GPU？没有的话如何说明？

我会直接说明：当前实验没有跑真实 GPU，主要跑的是 mock LLM 和 file-backed checkpoint cache。

vLLM adapter 的代码边界已经实现，它走 OpenAI-compatible `/v1/chat/completions` 接口，可以通过 `LOGSERVE_VLLM_BASE_URL` 或 worker 参数连接 vLLM 服务。也就是说，系统已经预留了调用真实 vLLM 的执行路径，但实验报告里的 LLM 数字来自 mock serving，不应该被解释成 GPU 性能。

mock LLM 的作用是让没有 GPU 的单机实验也能验证 runtime 机制，包括：

1. LLM task 能进入 worker 的 LLM pool。
2. worker 能按模型名和模型版本检查 cache。
3. cold miss 会触发 checkpoint fetch。
4. warm request 能命中本地 checkpoint。
5. `ModelLoadStarted/ModelLoaded/LLMCompleted` 能写入 event log。
6. scheduler 能比较 resource-only、locality-aware、predicted-latency。
7. RAG workflow 能把 `llm_generate()` 当作真实 workflow step。

如果面试官问真实 GPU，我会说下一步要补三组实验：真实 vLLM 单模型和多模型 cold start；prompt length/max tokens 分布下的 p50/p95/p99；LogServe scheduler 与 vLLM continuous batching 同时存在时的调度边界。现在不能说“GPU 性能已经验证”，只能说“vLLM 接口已接入，GPU 负载实验未覆盖”。

## Q945. 你写 benchmark，样本量和实验环境是什么？

实验环境是单机 Ubuntu，多进程模拟分布式 runtime：

```text
Linux lab2439 6.8.0-111-generic x86_64 GNU/Linux
Ubuntu 22.04 single-node environment
3 workers
mock LLM
file-backed checkpoint cache
```

一次完整实验会跑 Go tests、go vet、race tests、Python unittest、compileall、gRPC dependency check、logstore benchmark、fault injection、runtime 启动、benchmark、checkpoint cache probe 和 dashboard snapshot。结果写到 `reports/experiment-<timestamp>/`，包括 `command_status.jsonl`、`summary.json`、`benchmark.json`、`checkpoint_cache_probe.json`、runtime logs 和 dashboard snapshot。

样本量要分开说。

logstore benchmark 的样本较大：20,000 records、16 streams、256-byte payload，对比 always/batch/interval fsync。

端到端 runtime benchmark 偏小。一次实验里 workflow latency 只有少量请求，LLM locality ablation 也只有 6 个请求级别。它能说明机制能跑通，能看出 cache hit/cold start 的方向，但不能给出统计显著的生产性能结论。

我会主动把实验结论限定为：单机机制验证和小规模 ablation，不代表多机生产性能。要提高可信度，需要扩大请求数、固定 workload 分布、多轮重复、给出置信区间，并加入真实 GPU、真实对象存储和多节点网络。

## Q946. 你写 fault injection，具体注入了哪些故障？

当前 fault injection 覆盖的是几类常见恢复路径。

第一，worker kill recovery。worker 在执行或持有任务期间退出后，control 通过 lease/redelivery 让任务重新进入可执行状态。workflow 里也测试了 `embed` 成功后第一个 worker 停止，重启 worker 后从 `search` 继续，不重跑 `embed`。

第二，queue redelivery。任务被 poll 后如果没有正常 start 或 complete，超过 redelivery timeout 后可以重新投递。配套测试还覆盖 stale task lease：旧 epoch 的 completion 会被拒绝。

第三，control restart probe。control 重启后，从 shared log bootstrap metadata view，包括 task spec、workflow state、actor state、model/backpressure 等可恢复状态。

第四，actor owner failover。actor owner 停止心跳后，另一个 worker 接管，调用 `get()` 能得到之前 100 次 `inc()` 的状态。stale actor completion 会被 worker id 和 epoch fencing 拒绝。

第五，logstore recovery。unit test 覆盖 partial tail truncation、segment rollover、index rebuild 和 fsync policy 的基本恢复。

没覆盖的地方也要讲清楚：没有做真实断电、磁盘满、文件系统写入错误、logd 多副本 leader 切换、跨机网络分区、真实 S3 超时、真实 GPU OOM、Kubernetes Pod eviction 和长时间 soak test。这些属于后续生产化 fault injection。

## Q947. 你写 Kubernetes manifests，是否真的做了 K8s 压测？

没有做 K8s 压测。这个要直接说。

项目里有 Kubernetes manifests，包含 namespace、logd、control、worker 和 kustomization，目的是给出 cloud-native 部署形态的起点。它说明组件可以被拆成独立进程，用容器方式部署，也方便后续在 kind 或 minikube 上启动。

但当前实验结论来自单机 Ubuntu 脚本，不来自 Kubernetes 集群。也没有给出 Pod eviction、PVC 性能、Service 网络延迟、rolling upgrade、HPA/autoscaling 或多节点调度的压测数据。

如果简历里写 K8s，我会把措辞控制成“提供 Kubernetes manifests / deployment examples”，不写“完成 Kubernetes 压测”或“生产级云原生部署”。如果面试官追问，我会说下一步会在 kind/minikube 先做功能验证，再在多节点集群测 worker Pod 重启、logd PVC 恢复、control rolling restart 和 model cache 在 emptyDir/hostPath/PVC 下的差异。

## Q948. 你写 PostgreSQL/MinIO，核心路径是否依赖它们？

当前核心路径不强依赖 PostgreSQL 和 MinIO。

LogServe 的主线是 shared log 作为 source of truth，metadata view 可以在内存里，也可以落 PostgreSQL。单机开发和很多测试使用 in-memory metadata store；Compose 模式下可以用 PostgreSQL 保存 materialized view。PostgreSQL 的定位是查询视图和持久化 view，不应该变成状态真相。如果 PostgreSQL 表丢了，只要 shared log 还在，control 重启后可以 replay 重建 view。

MinIO/S3 的定位是 result store 和 snapshot store 的外部化边界。大 workflow result 和 actor snapshot 不适合直接塞进 log，所以日志里保存 `result_ref`。默认本地开发可以用 filesystem-backed `local://`；Compose 提供 MinIO；生产化可以换成 S3-compatible backend。

所以回答要分清：shared log 是恢复主线；PostgreSQL 是 materialized metadata view 的后端；MinIO 是大对象和 snapshot 的对象存储。项目已经写了这些 adapter/部署边界，但核心恢复语义不能建立在“数据库就是 source of truth”上。

边界也要说明：如果 result_ref 指向的对象丢失，shared log 只能告诉你对象应该在哪里，不能凭空恢复对象内容。后续需要 checksum、对象生命周期管理和一致性检查。

## Q949. 你写 dashboard，是否是生产监控还是 snapshot API？

当前更准确地说是 dashboard snapshot API，不是完整生产监控系统。

`DashboardSnapshot` 从 materialized view 读取当前状态，返回 queue depth、backpressure 配置、tasks、workflows、actors、workers、models、compactable log stats 等信息。实验脚本会把它保存成 `dashboard_snapshot.json`，用于检查系统当前状态和写报告。

它能展示 workflow DAG、task 状态、actor 状态、worker/model cache 和部分 log retention 指标。对单机实验和答辩演示够用，也能说明 materialized view 能被统一查询。

但它还没有达到生产监控的程度。生产监控需要 Prometheus metrics、tracing、日志采样、分页查询、权限控制、告警规则、error budget、历史趋势、tail/follow event stream。当前 snapshot 一次性返回较多对象，规模大后也需要分页和过滤。

所以简历里可以写“dashboard snapshot and observability API”，不要写成“完整生产监控平台”。如果被追问，我会说它现在是可观测性的起点，后续会把 append latency、queue depth、lease expired、actor backlog、cache hit、cold start、p95/p99 这些指标接到 Prometheus。

## Q950. 你写 backpressure，策略是否足够完整？

当前 backpressure 是可用的基础策略，还不完整。

已经实现的信号主要有两个。

第一个是 queue high watermark。控制面发现队列积压超过阈值时，会拒绝新的非幂等提交，避免继续把任务塞进系统。这里有一个重要边界：如果请求是同一个 idempotency key 的重复提交，系统会先返回已有任务，不会因为队列满误伤客户端重试。

第二个是 log append slow。control 记录最近一次 AppendLog 的耗时，如果超过配置阈值，新提交会被拒绝。这个信号用于保护 shared log：当 log 写入变慢时，control 不应该继续接收大量新事件。

backpressure 配置本身也走 log-first。`SetBackpressure` 会写 `BackpressureConfigured` 到 `system:backpressure`，control 重启后能恢复配置。

不完整的地方也很明显。当前没有把 worker 本地 executor queue、Python runner 卡住、内存压力、result store 延迟、gRPC latency、MinIO/S3 超时、GPU 显存、LLM batching 队列、actor mailbox backlog 都纳入统一限流。也没有 per-tenant quota、优先级、令牌桶、动态限流和 autoscaling 联动。

所以我会说：现在的 backpressure 解决了两个最直接的入口保护问题，队列过深和 log append 过慢；它不是完整的生产级 admission control。后续要把控制面、worker、本地执行器、对象存储和 LLM serving 的压力信号统一起来，再按 tenant、task type 和 SLO 做分层限流。
