# 七、LLM Serving、Model Cache、Checkpoint Cache 与调度策略（深度）

这一组问题主要围绕 LLM 调度的实现细节。重点不是“能调用模型”本身，而是 LogServe 如何把模型缓存、checkpoint 拉取、worker 负载、历史延迟和事件日志放进同一个调度闭环里。

## Q501. canAssignTaskToWorker 对 LLM task 的决策流程是什么？

`canAssignTaskToWorker` 先做通用判断：worker 是否存在、是否活跃、是否还有 capacity。普通 task 到这里基本就可以决定是否能分配。

如果 task 是 LLM task，流程会继续看当前调度策略。

在 `RESOURCE_ONLY` 下，只要 worker 有资源就可以拿任务。这个策略不看模型缓存，主要作为 baseline。

在 `LOCALITY_AWARE` 下，控制面会先算出当前 task 的首选 worker。只有被选中的 worker 来 poll 时，才会拿到这个 task。选择依据包括目标模型是否已缓存、worker 当前运行任务数、可用 capacity，以及这个 task 已经排队多久。

在 `PREDICTED_LATENCY` 下，控制面会使用 LLM stats 估算每个候选 worker 的预测延迟。预测最低的 worker 才能拿到任务。

所以这条路径的核心是：LLM task 不是谁空闲就给谁，而是先结合缓存和历史指标选一个更合适的 worker，再由 poll 机制完成领取。

## Q502. LOCALITY_AWARE 如何给 worker 打分？

当前打分比较直接。每个活跃且有 capacity 的 worker 都会得到一个分数。

基础上，可用 capacity 越多分越高；正在运行的任务越多分越低。这样可以避免所有请求都堆到同一个 worker。

如果 worker 本地已经缓存了目标模型，会得到很大的加分。这个加分让缓存命中在大多数情况下优先于“看起来更空闲但没有模型”的 worker。

还有一个细节：如果系统里已经存在“有缓存且有 capacity”的 worker，并且这个 task 排队时间还不长，未缓存 worker 会被明显扣分。这样做是为了让请求稍微等一下，尽量落到缓存命中的 worker 上。

如果两个 worker 分数相同，代码按 worker id 做稳定选择。这不算复杂调度，但结果可复现，测试也更容易写。

## Q503. 为什么 cached worker 有容量时要优先选择它？

因为 LLM 请求的慢点经常不在执行函数，而在模型加载。

一个没有缓存的 worker 即使很空闲，也可能要先拉 checkpoint、写本地 cache、加载模型，然后才能开始生成。一个已经缓存目标模型的 worker 即使手上有少量任务，实际完成时间也可能更短。

优先选择 cached worker 有三个直接收益。

第一，减少 cold start。checkpoint fetch 和模型加载都是尾延迟的来源。

第二，减少 cache 抖动。模型被分散加载到很多 worker，会增加本地磁盘占用和淘汰次数。

第三，调度行为更稳定。同一模型连续请求更容易落到已有缓存的 worker 上，实验里的 cache hit rate 也因此更高。

这也是 locality-aware 的基本判断：LLM task 的资源成本不仅是 CPU 或 worker slot，还包括模型是否已经在本地。

## Q504. localityQueueWait 的作用是什么？

`localityQueueWait` 用来限制“为了缓存命中而等待”的时间。

如果一个 task 刚进入队列，系统可以偏向 cached worker。短时间等待通常是值得的，因为命中缓存能省掉一次冷启动。

但如果 task 已经排队一段时间，继续坚持只等 cached worker 可能反而拉高延迟。此时调度器会放松对 cold worker 的惩罚，让没有缓存但有资源的 worker 也有机会执行。

这其实是在做一个权衡：缓存命中很好，但不能为了命中缓存无限排队。

当前实现里这个阈值是一个简单常量，适合实验验证。生产环境里更合理的做法是让它跟 SLO、模型加载成本、worker 队列长度和请求优先级一起配置。

## Q505. 如果 cached worker 繁忙，何时允许 cold worker 执行？

有两种情况会让 cold worker 拿到任务。

第一种情况是没有任何 cached worker 还有 capacity。此时继续等缓存 worker 没意义，调度器会在有资源的 worker 中选择。

第二种情况是虽然存在 cached worker，但 task 已经排队超过 locality 等待阈值。排队时间过长后，调度器会降低对 cold worker 的惩罚。如果 cold worker 更空闲，它就可能胜出。

这点很重要。locality-aware 不是“只要有缓存就永远等缓存”，而是“短时间优先缓存，等待过久就优先完成请求”。

面试里可以这样说：LogServe 当前做的是缓存亲和调度，不是缓存强绑定调度。

## Q506. PREDICTED_LATENCY 为什么不在热路径 replay 所有 `llm:*` stream？

因为那会把调度路径做成 O(历史事件数)。

LLM task 每次调度都去扫描所有 `llm:*` stream，早期 demo 可能能跑，但请求一多，调度器会被 replay 拖慢。更糟的是，调度本身发生在高频路径上，慢一次就会影响所有排队任务。

当前实现把历史事件提前物化成 LLM stats。worker 完成一次 LLM 请求后，`LLMCompleted` 事件会更新对应的统计项。调度时只需要按候选 worker 查这份 materialized stats，复杂度接近 O(worker 数)。

控制面重启时再从 log 重建这份 stats。这样既保留了 log-first 和可恢复能力，也避免了每次调度都全量 replay。

## Q507. materialized LLM stats 在什么时候更新？

主要有两个时机。

第一个时机是 LLM 请求完成后。worker 写入 `LLMCompleted` 事件，控制面读取到该事件后，会按模型名、模型版本和 worker id 这三个维度更新统计项。

第二个时机是控制面启动或恢复时。`bootstrapLLMStats` 会列出所有 `llm:` stream，读取其中的 `LLMCompleted` 事件，然后重新构建 stats。

这份 stats 记录请求数、cache hit 数、总延迟 EWMA、模型加载 EWMA、checkpoint fetch EWMA、最近一次 eviction 数和更新时间。

它不是 source of truth。source of truth 仍然是 log。stats 只是为了让调度器在热路径里快速查询。

## Q508. control restart 后 LLM stats 如何重建？

控制面重启后会执行 bootstrap 流程，其中包含 LLM stats 的重建。

具体做法是：先清空内存里的 LLM stats，然后通过 log service 列出 `llm:` 前缀的 stream。对每个 LLM stream，按顺序读取事件，遇到 `LLMCompleted` 就把 payload 物化进 stats。

这样可以恢复每个 worker 对每个模型的历史表现，比如请求数、cache hit 数和延迟 EWMA。

需要注意的是，worker 本地实际有哪些 checkpoint 仍然要靠 worker heartbeat 上报。控制面从 LLM event log 可以恢复历史统计，但不能凭空确认某个 worker 当前磁盘上一定还有某个文件。当前缓存状态还是运行时状态，需要 worker 启动后扫描本地 cache 并上报。

## Q509. EWMA 的权重为什么是 previous*7 + sample*3？这个选择有什么问题？

这个公式等价于旧值占 70%，新样本占 30%。它的好处是简单，能让预测值跟着最近样本变化，同时不至于被单个异常请求完全带偏。

问题也很明显。

第一，这个权重是经验值，不是根据实验自动学习出来的。

第二，它没有时间衰减。如果一个 worker 很久没处理某个模型，旧统计仍然可能影响调度。

第三，它把 cold、warm、不同 prompt 长度的请求混在一起。真实 LLM 延迟跟 prompt token、输出 token、batch 状态都有关系，单个 EWMA 只能给粗略估计。

第四，样本很少时 EWMA 不稳定。只有一两次请求的 worker，看起来可能很快，也可能只是碰巧。

所以这个公式适合项目阶段的可解释调度，不适合直接当生产级预测模型。

## Q510. 如果某 worker 的历史数据很少，predicted latency 如何避免误判？

当前实现用默认值兜底：没有历史总延迟时，用一个默认 base latency；没有冷启动历史时，对未缓存 worker 加默认 cold start penalty。这样不会因为没有数据就把预测延迟算成 0。

但这只能避免最明显的误判。更稳妥的做法还需要几层保护。

可以设置最小样本数。样本不足时，不完全相信这个 worker 的历史均值。

可以给低样本 worker 加不确定性惩罚。样本越少，预测越保守。

也可以保留少量探索流量。否则某个 worker 早期表现好，可能长期吃到更多请求；其他 worker 没机会积累样本，调度器就越来越偏。

面试里可以坦诚说：当前实现解决了“不能每次 replay”的问题，但历史样本置信度还可以继续加强。

## Q511. 冷启动 penalty 如何估算？

当前 cold start penalty 主要来自两项历史 EWMA：模型加载时间和 checkpoint fetch 时间。

如果 worker 没有缓存目标模型，调度器会把这两项加到预测延迟里。这样一个经常拉 checkpoint 很慢的 worker，在预测里就不会显得太便宜。

如果没有历史数据，代码会给一个默认冷启动惩罚。这样冷 worker 不会因为缺少统计而被错误地当成最快 worker。

这个估算还比较粗。真实系统里，冷启动还和模型大小、对象存储带宽、磁盘读写、GPU 显存碎片、并发加载数量有关。后续可以把这些信号加进去，尤其是模型大小和 cache 剩余容量。

## Q512. eviction penalty 是否足以预测未来延迟？

不完全足够。

`eviction_count` 能说明本次加载触发了 cache 淘汰。它是一个有用信号，因为频繁淘汰通常意味着这个 worker 的 cache 压力比较大。

但它不能完整预测未来延迟。

比如淘汰了哪个模型很重要。如果淘汰的是很少再用的模型，影响不大；如果淘汰的是热点模型，后面很可能马上出现新的 cold miss。

还要看模型大小、剩余容量、请求分布、是否有多个模型互相挤占。只用 eviction_count 做线性惩罚，容易低估 cache churn。

所以 eviction penalty 是一个简化信号。它能让调度器意识到“这个 worker 的 cache 不太稳”，但还不能替代完整的 cache pressure 模型。

## Q513. queue penalty 当前是否准确？还应该考虑哪些排队信号？

当前 queue penalty 主要看 worker 正在运行的任务数，并给每个 running task 加一个固定惩罚。它还会根据 task 自身排队时间，对非缓存 worker 的选择做一点调整。

这个估算能反映大致负载，但不够准确。

真实排队延迟还应该看 worker 本地 executor queue 的长度、不同 pool 的占用情况、LLM pool 的并发度、vLLM 内部队列、GPU 上的 batch 状态、请求 token 数、网络延迟和历史 local_queue_wait_ms。

对 LLM 来说，只看 running task 尤其粗糙。一个 16 token 的短请求和一个 2048 token 的长请求占用差别很大。后续如果要做更准的调度，应把 prompt token、max_tokens、当前 batch 队列和 GPU 显存状态纳入预测。

## Q514. 如果 worker heartbeat 上报缓存状态滞后，调度会出现什么问题？

调度器看到的是 metadata view 里的 cached_models。如果 heartbeat 滞后，这份视图就可能和 worker 本地磁盘不一致。

一种问题是误判为有缓存。控制面把请求发过去，worker 实际发现 checkpoint 不在本地，最后变成 cold miss，延迟比预测高。

另一种问题是误判为没缓存。worker 明明已经有模型，但控制面没看到，于是把请求发给别的 worker，错过一次 cache hit。

还有一种情况是 worker 本地刚发生 eviction，但心跳还没来得及上报。调度器继续把请求发给它，会造成意外冷启动。

当前系统通过 worker heartbeat 和 LLMCompleted 事件不断修正视图，但它不是强同步。面试中可以把这里归入最终一致的边界。

## Q515. checkpoint cache manifest 的作用是什么？

manifest 是 worker 本地 checkpoint cache 的索引文件。

它记录模型名、模型版本、checkpoint 文件名、文件大小和最近访问时间。worker 重启后，不需要重新扫描和猜测每个文件属于哪个模型，只要读取 manifest 并确认 checkpoint 文件存在，就能恢复本地 cache 索引。

manifest 还有一个作用是支持 LRU 淘汰。最近访问时间写在 manifest 中，worker 可以根据它选择最久没用的 checkpoint。

如果只有二进制 checkpoint 文件，没有 manifest，重启后要么无法准确恢复模型映射，要么只能靠文件名约定推断，边界情况会很多。

## Q516. worker 重启后如何扫描并恢复本地 checkpoint cache？

worker 创建 model cache 时，会读取本地 cache 目录下的 manifest 文件。

对每个 manifest，worker 会解析模型名、模型版本、checkpoint 文件名、大小和最近访问时间。然后它会检查对应 checkpoint 文件是否真的存在。

如果文件存在，就把这条记录放回内存里的 checkpoint 索引，并把模型加入 cached models。`cache_used_bytes` 也会重新累计。

如果 manifest 指向的文件不存在，这条记录会被忽略或清理。这样可以避免把已经丢失的 checkpoint 继续上报给控制面。

恢复后，worker 的 heartbeat 会把 cached_models 上报给 control plane，调度器就能继续利用本地缓存。

## Q517. LRU eviction 如何实现？

当一个新 checkpoint 要进入本地 cache，而 `used + incoming > capacity` 时，worker 会触发淘汰。

当前实现会在 checkpoint 索引中选择 `lastAccess` 最早的记录，也就是最久没被访问的模型 checkpoint。然后删除 checkpoint 文件和 manifest 文件，更新内存索引，减少 used bytes，并增加 eviction count。

这个过程会循环执行，直到有足够空间放下新 checkpoint。

实现上，淘汰、复制、写 manifest、更新索引都在同一把 checkpoint 锁保护下完成。这样可以避免两个 LLM task 同时修改 cache 目录，把索引或文件状态搞乱。

## Q518. 如果 checkpoint 文件大于 cache capacity，应如何处理？

当前应该直接失败，而不是尝试淘汰所有文件后继续写。

原因很简单：如果单个 checkpoint 已经超过容量上限，淘汰多少旧文件都放不下。继续执行只会浪费时间，还可能把原有 cache 清空。

当前 `ensureCheckpoint` 会检查源 checkpoint 文件大小。如果大于本地 capacity，就返回错误。worker 会把这次 LLM task 标记为失败。

更好的用户体验是提前在 Model Registry 或 worker 启动时做校验：模型大小超过某类 worker 的 cache capacity 时，不允许调度过去，错误信息也应该直接说明“模型 checkpoint 大于本地 cache 容量”。

## Q519. 如果两个 LLM task 同时 cold miss 同一模型，如何避免重复下载？

当前依靠 `checkpointMu` 做串行化。

两个 task 同时发现本地没有同一个模型时，只有一个 task 能先进入 `ensureCheckpoint` 的关键区。它会完成检查、复制 checkpoint、写 manifest、更新内存索引。

第二个 task 进入时，会再次检查本地索引和文件状态。此时模型已经存在，它就走 cache hit 路径，不会重复复制同一个 checkpoint。

这个做法简单可靠。代价是锁粒度偏粗，同一个 worker 上不同模型的 checkpoint 拉取也会互相等待。后续可以改成 per-model lock，让 model-A 和 model-B 的 cold miss 并行，但同一个模型仍然只拉一次。

## Q520. checkpointMu 保护了什么？

`checkpointMu` 保护的是 worker 本地 checkpoint cache 的一致性。

它覆盖几个关键操作：检查 checkpoint 是否存在、更新 lastAccess、从 source 复制文件、写 manifest、执行 LRU 淘汰、删除旧文件、更新内存里的 checkpoint map 和 cached model map。

这些操作如果不加锁，很容易出现竞态。

比如一个 goroutine 正在淘汰 model-A，另一个 goroutine 同时认为 model-A 还存在并上报 cache hit。或者两个 goroutine 同时拉 model-D，最后 manifest 被覆盖、used bytes 计算错误。

所以这把锁不是为了业务串行，而是为了保护本地 cache 文件系统和内存索引的一致性。

## Q521. cache hit 在 ModelLoadStarted 时判断和 ensureCheckpoint 后判断可能不同，为什么？

`ModelLoadStarted` 是加载开始时的早期观察，`ensureCheckpoint` 后的结果才更接近最终事实。

两者可能不同，原因有几类。

第一，内存索引可能还没完全反映磁盘状态。`cache.has` 初步检查时可能没有命中，但 `ensureCheckpoint` 进入后发现本地文件和 manifest 是可用的，于是把它重新索引为 hit。

第二，文件系统状态可能变化。比如 checkpoint 文件被外部删除，早期判断为 hit，后续真正读取时发现文件不在了。

第三，并发请求会改变结果。一个 task 先进入 `ensureCheckpoint` 完成复制，另一个 task 稍后进入时就从 cold miss 变成 warm hit。

所以日志里同时保留 `ModelLoadStarted` 和 `ModelLoaded` 是有价值的。前者记录调度和加载开始时的判断，后者记录加载完成后的实际结果。

## Q522. mock LLM latency 与真实 vLLM latency 差异在哪里？

mock LLM 的延迟是可控的，主要由配置里的模型加载睡眠和首 token 睡眠组成。它适合验证调度逻辑，但不等于真实模型服务。

真实 vLLM 延迟要复杂得多。它包括 HTTP 网络开销、vLLM 内部排队、prefill、decode、batch 合并、KV cache 状态、GPU 显存压力、模型权重是否已加载、LoRA adapter 是否需要切换，以及输出 token 数。

mock 场景里，请求之间的干扰比较小。真实 vLLM 场景中，一个长输出请求可能影响同批或后续请求的尾延迟。

所以实验报告里要讲清楚：mock 证明的是 LogServe 的调度和缓存机制，不证明某个真实模型的绝对生成性能。

## Q523. first_token_ms 和 total_latency_ms 分别衡量什么？

`first_token_ms` 衡量从请求开始到模型产出第一个 token 的时间。它更接近用户感受到的“开始响应速度”。

`total_latency_ms` 衡量从请求开始到整个 LLM 请求完成的总时间。它包含模型加载、checkpoint fetch、排队、首 token 和后续生成时间。

两者适合看不同问题。

如果 first_token_ms 很高，可能是 prefill、排队或模型加载慢。

如果 first_token_ms 正常但 total_latency_ms 很高，可能是输出太长、decode 慢、batch 被长请求拖住，或者下游网络传输慢。

当前 mock 实现里两者比较简单；接真实 vLLM 后，这两个指标会更有诊断价值。

## Q524. 真实 vLLM 场景中还需要记录哪些指标？

至少需要记录 token 维度和 GPU 维度的指标。

请求侧可以记录 prompt tokens、completion tokens、max_tokens、是否流式输出、请求排队时间、prefill 时间、decode 时间、tokens per second、batch size 和错误类型。

模型侧可以记录模型是否已加载、KV cache 使用率、GPU 显存占用、GPU 利用率、并发请求数、vLLM 内部队列长度、adapter 切换时间。

缓存侧可以继续记录 checkpoint_fetch_ms、model_load_ms、cache hit、cache used/capacity 和 eviction。

这些指标结合起来，才能区分“调度错了”“模型冷启动慢”“vLLM 内部排队”“输出太长”这几类问题。

## Q525. adapter=vllm 时错误处理和超时如何设计？

当前 vLLM adapter 会通过 HTTP 调用 `/v1/chat/completions`。它需要处理几类错误：base URL 未配置、请求构造失败、网络超时、HTTP 非 2xx、响应 JSON 解析失败、choices 为空。

超时应该沿用 task 的 context deadline。也就是说，控制面和 worker 看到的是同一个任务超时语义，而不是 vLLM 调用自己无限等待。

更完整的设计可以把错误分成可重试和不可重试。网络抖动、短暂 503、连接重置可以有限重试；模型不存在、请求参数错误、prompt 超限一般不应该重试。

还可以加 circuit breaker。某个 vLLM endpoint 连续失败时，短时间内不要继续把请求调过去。

这里要注意幂等语义。vLLM 调用如果已经产生了外部副作用，重试可能造成重复输出或重复计费。LogServe 当前更适合把 vLLM 请求看成可重试但结果提交幂等的任务，而不是严格 exactly-once。

## Q526. LLMCompleted 事件写失败会影响 predicted-latency stats 吗？

会。

predicted-latency stats 是从 `LLMCompleted` 事件物化出来的。如果这个事件没有成功写入 log，控制面就没有可靠样本可用，重启后也无法从 log 恢复这次请求的 LLM 指标。

当前 worker 在 LLM 执行完成后会尝试写 `LLMCompleted`。如果写失败，任务不应该被当成一个完整的成功样本来更新 stats。否则 metadata 和 log 会分叉：内存里看起来有统计，重启后又消失。

所以正确原则是：调度统计也要从 log 派生。写 log 失败时，宁可丢掉这个统计样本，也不要绕过 log 直接更新 materialized view。

## Q527. 如果 LLM task 失败，是否应该更新 stats？

当前成功延迟 stats 主要由 `LLMCompleted` 更新，失败请求不混进成功延迟 EWMA。

这是合理的第一步，因为失败延迟和成功延迟含义不同。比如模型不存在导致立刻失败，如果把它算进 total latency，调度器可能误以为这个 worker 很快。

但失败也应该被记录，只是不应该直接混进成功延迟。更好的做法是单独维护 failure_count、timeout_count、last_error_type、recent_failure_rate。

调度时可以对近期失败率高的 worker 或模型服务加惩罚。这样既不会污染成功延迟，又能避免继续把请求打到不健康的 worker。

## Q528. 模型版本为空时为什么默认 `v1`？

默认 `v1` 是为了让模型注册和请求提交有稳定的 key。

如果不做默认值，同一个模型可能出现空版本、`v1`、缺省字段三种表示。缓存 key、Model Registry key、LLM stats key 都会被拆散，调度器很难判断它们是不是同一个模型。

默认 `v1` 后，用户只注册一个模型名也能跑通，简单 demo 不需要写太多字段。

这里有一个边界：生产环境里的模型版本应该显式管理，尤其是模型权重、量化方式、LoRA adapter 或 prompt 模板发生变化时，不能都混在 `v1` 下面。

## Q529. Model Registry 中 size_bytes 当前如何参与调度？未来如何参与？

当前 `size_bytes` 主要是模型元数据，调度器没有充分使用它。

checkpoint cache 在 worker 侧会根据真实 checkpoint 文件大小判断是否放得下，并更新 cache used/capacity。也就是说，容量判断主要发生在 worker 本地，而不是 control plane 调度前。

未来可以把 `size_bytes` 更早地放进调度决策。

比如，调度器可以预判某个 worker 的剩余 cache 容量是否足够，避免把大模型发给明显放不下的 worker。也可以用模型大小估算 checkpoint fetch 时间和模型加载时间。对于 GPU 场景，size_bytes 还可以参与显存需求估算。

这样 cold start penalty 就不只是历史平均值，而能结合当前模型大小做更准确的预测。

## Q530. 如果多个模型共享底层权重或 LoRA adapter，cache key 如何设计？

不能只用“对外暴露的模型名”做 key。

更稳妥的 key 应该拆成几个部分：base model 的名称和权重指纹、量化格式、tensor parallel 配置、LoRA adapter id、adapter 权重指纹，以及 runtime 相关配置。

如果两个 served model 共享同一个 base checkpoint，只是 LoRA adapter 不同，cache 应该能复用 base weights，同时单独管理 adapter cache。

否则会有两个问题。

第一，重复占用磁盘和显存。同一个 base model 被当成两个完全不同模型加载。

第二，错误复用。名字相同但权重实际不同，可能命中错误缓存，生成结果也会错。

所以真实系统里 cache key 最好用内容指纹或 manifest digest，而不是只靠字符串名字。

## Q531. 如果 checkpoint cache 位于本地磁盘，Pod 迁移会有什么影响？

如果 cache 在 Pod 本地临时磁盘上，Pod 被迁移或重建后，本地 checkpoint cache 通常会丢失。

结果是控制面之前看到的 cached_models 可能过期。新 Pod 启动后扫描不到旧 checkpoint，heartbeat 会重新上报较少的缓存状态。调度器在这段时间里可能误判，也可能出现更多 cold start。

解决办法有几种。

可以使用 node-local cache，把缓存绑定到节点而不是 Pod。也可以用 PVC 保留缓存目录。还可以在 worker 启动时做 cache warmup，提前拉取常用模型。

如果调度在 Kubernetes 上运行，还要把“worker 在哪个节点”和“节点上有哪些模型 cache”纳入调度，否则 Pod 迁移会削弱 locality-aware 的效果。

## Q532. 如果 checkpoint source 是对象存储，网络带宽如何影响 cold start？

对象存储会把 cold start 变成网络敏感路径。

checkpoint 文件越大，首次 fetch 越依赖带宽、延迟、对象存储限流和跨可用区流量。多个 worker 同时 cold miss 同一个大模型时，还可能把对象存储或网络打满。

这会直接体现在 `checkpoint_fetch_ms` 上，也会间接影响 `total_latency_ms` 和 p95/p99。

可以做几类优化：预取热点模型、限制并发 checkpoint fetch、做分层缓存、本地节点缓存、按模型大小控制调度，以及在对象存储附近部署 worker。

实验里的本地 source 目录只能模拟一部分开销。真实对象存储场景要单独测网络带宽和并发拉取。

## Q533. 如果 model load 需要 GPU 显存，cached_models 是否足以描述可服务状态？

不够。

`cached_models` 只能说明 worker 本地有模型文件或 checkpoint cache。它不能说明模型已经加载到 GPU，也不能说明 GPU 还有足够显存服务新请求。

真实 GPU 场景还需要上报更多状态：GPU 总显存、可用显存、当前已加载模型、vLLM engine 是否健康、KV cache 使用率、当前 batch 队列长度、并发请求数和模型上下文长度限制。

一个 worker 可能磁盘上有 checkpoint，但 GPU 已经满了。此时调度过去仍然会失败或排很久。

所以 cached_models 是 locality 的起点，不是完整的 GPU serving readiness。

## Q534. LLM task 的 max_tokens 如何影响调度？

`max_tokens` 会影响 decode 时间，也会影响 GPU 占用时间。

两个请求模型相同、prompt 相同，但 max_tokens 一个是 64，一个是 2048，完成时间可能差很多。长输出请求会占用 decode 资源，也可能拖高同一 batch 中其他请求的延迟。

当前 LogServe 把 max_tokens 放进 TaskSpec，worker 调 vLLM 时会传给后端。但调度器还没有把它充分用于预测。

未来可以把 max_tokens 和 prompt token 估算一起放进 predicted latency。短请求可以调给低等待 worker，长请求可以考虑隔离队列或单独限流，避免把尾延迟传染给所有请求。

## Q535. 如何将 batching 引入 LLM serving？

batching 可以放在两个层次。

第一层是 LogServe worker 本地。worker 可以按模型把短时间内到达的 LLM task 聚合起来，形成 batch 后再调用模型服务。这样 LogServe 能在事件日志里记录 batch id、batch 大小和每个请求的结果。

第二层是 vLLM 自身。vLLM 已经有连续 batching 能力，LogServe 可以把请求转发给 vLLM，由 vLLM 决定怎么合批。

更现实的边界是：LogServe 负责跨 worker 的 placement、cache-aware 调度、workflow/actor 集成和事件记录；vLLM 负责单个模型服务内部的 token-level batching。

如果 LogServe 自己也做 batching，需要解决请求超时、部分失败、单个请求取消、batch 内结果拆分和 per-request event log。这个复杂度不低，最好先让 vLLM 做底层 batching。

## Q536. 如果 vLLM 自身已经有 scheduler，LogServe scheduler 的边界在哪里？

LogServe scheduler 和 vLLM scheduler 处理的问题不一样。

LogServe 负责决定请求应该去哪个 worker，重点是模型缓存、本地 checkpoint、worker capacity、workflow DAG、actor mailbox 和系统级 backpressure。

vLLM scheduler 负责单个模型服务内部如何执行请求，重点是 batching、prefill/decode 调度、KV cache、GPU 利用率和 token 生成。

所以 LogServe 不应该重新实现 vLLM 的 token 调度。它更像上层 runtime：把请求放到合适的 worker 和模型服务上，再用事件日志记录完整执行历史。

边界清楚后，两个调度器不会互相抢职责。LogServe 做 placement，vLLM 做 GPU 内部执行。

## Q537. locality-aware ablation 的实验设计是否能证明策略有效？

它能证明一个有限但有用的结论：在固定 worker、固定模型缓存分布、固定请求序列下，locality-aware 比 resource-only 更容易命中缓存，冷启动更少，尾延迟更低。

你之前的实验就是这个逻辑。worker-1 缓存 model-A，worker-2 缓存 model-B，然后连续提交 model-A 请求。resource-only 可能把请求发给没有缓存的 worker，而 locality-aware 会优先选缓存命中的 worker。

实验指标也对得上：cache hit rate 更高，cold start latency 更低，p95/p99 更好。

但它不能证明所有场景都更优。样本量、mock 延迟、请求分布和 worker 数都比较有限。更严格的实验要增加请求数、重复多轮、随机化请求顺序，并报告置信区间。

## Q538. p95 latency 降低是否可能来自样本量太小？如何增强实验可信度？

可能。

如果只有几次请求，p95/p99 很容易被单个样本决定。一次冷 miss 或一次调度顺序变化，就能让尾延迟看起来改善很多。

增强可信度可以从几方面做。

增加样本量，比如每种策略跑几百或几千次请求。重复多轮实验，报告 median、p95、p99、均值和标准差。固定随机种子，或者明确记录请求序列。把 warmup 和正式测量分开。最后，把 raw result 保存下来，而不是只保留 summary。

还可以在不同模型大小、不同 cache capacity、不同 worker 数下重复实验。这样才能说明策略对 cache locality 的改善不是偶然样本造成的。

## Q539. cache hit rate 从 0.833 到 1.0 的业务意义是什么？

在你的实验里，0.833 到 1.0 大致意味着 6 次请求里少了一次 cold miss。样本小，所以不要把这个数字夸大成生产结论。

但方向是有意义的。LLM serving 的冷启动通常很贵，一次 cold miss 可能带来 checkpoint fetch、模型加载、cache 淘汰和尾延迟上升。

如果在更大流量下保持类似趋势，cache hit rate 提升会带来几类收益：用户等待更短，p95/p99 更稳定，对象存储和本地磁盘压力更小，worker 也少做重复模型加载。

所以这个指标的价值不在“0.833 和 1.0 本身”，而在它证明调度器确实能把同一模型请求导向已有缓存的 worker。

## Q540. 如何防止 predicted-latency 对历史快 worker 过度偏置？

需要给历史统计加几个保护。

第一，加时间衰减。很久以前的快样本不能一直影响现在的调度。

第二，加最小样本数和置信度。样本少的 worker，即使 EWMA 很低，也不应该被完全信任。

第三，保留少量探索流量。否则早期表现好的 worker 会拿到更多请求，越拿越有样本，其他 worker 永远没机会证明自己。

第四，对 worker 状态变化做重置或降权。比如 worker 重启、模型被淘汰、cache capacity 改变、vLLM endpoint 切换后，旧统计都应该减弱。

第五，把 queue、cache 和 GPU 状态放进预测，而不是只看历史 total latency。一个历史很快的 worker，如果当前队列很长或 cache 快满了，也不应该继续被选中。

简单说，predicted-latency 要避免“赢家通吃”。历史数据有用，但必须和新鲜状态、不确定性和少量探索一起使用。
