# 七、LLM Serving、Model Cache、Checkpoint Cache 与调度策略（拓展）

这一组问题更偏真实 GPU serving。LogServe 当前用 mock LLM 和 checkpoint cache 把调度链路跑通，已经能验证 cache-aware scheduling、事件日志和 replay。真正接 GPU 后，问题会变得更细：prefill、decode、KV cache、batching、显存、租户隔离、计费和 tracing 都要进入调度模型。

## Q541. LLM serving 中 cold start 的主要组成是什么？

LLM serving 的 cold start 通常不是一个单点耗时，而是一串动作叠在一起。

第一段是模型文件准备。模型权重可能在对象存储、共享文件系统或本地 checkpoint source 里。worker 本地没有缓存时，要先拉取 checkpoint。LogServe 当前用 `checkpoint_fetch_ms` 记录这一段。

第二段是权重加载。checkpoint 到了本地以后，还要反序列化、加载到内存或 GPU 显存，并初始化模型运行时。LogServe 当前用 `model_load_ms` 记录这一段。

第三段是服务端预热。真实 vLLM 场景里，可能还有 CUDA kernel 初始化、CUDA graph 捕获、KV cache block 分配、runtime 编译、LoRA adapter 加载等开销。

第四段是请求自身的首次执行。即使模型已经加载，长 prompt 的 prefill 也会让首 token 慢下来。

所以我面试时会把 cold start 拆成三层：文件层冷启动、模型层冷启动、请求层首 token 冷启动。LogServe 现在主要覆盖前两层，后续接 GPU 后要把第三层拆得更细。

## Q542. 模型权重加载、KV cache、prompt prefill、decode 阶段的瓶颈分别是什么？

模型权重加载的瓶颈通常在 I/O、反序列化和显存写入。模型越大，对象存储带宽、本地磁盘吞吐、PCIe/NVLink 和 GPU 显存容量都会影响加载时间。

KV cache 的瓶颈在显存容量和碎片管理。每个请求生成 token 时都要保存 key/value。并发请求多、上下文长、输出长，KV cache 会很快吃掉显存。显存不够时，请求要排队，严重时直接 OOM。

prompt prefill 的瓶颈主要是计算量。长 prompt 需要一次性处理大量输入 token，attention 计算重，TTFT 会变高。RAG 请求尤其容易出现这个问题，因为检索结果会把 prompt 拉长。

decode 的瓶颈更像 token-by-token 的持续占用。每一步生成一个或少量 token，需要反复执行模型。短请求看 TTFT，长输出请求更看 decode 吞吐和 ITL。

调度时不能只看“模型是否缓存”。一个 worker 可能模型已缓存，但 KV cache 快满、decode 队列很长，实际也不该继续接长请求。

## Q543. vLLM 的 paged attention 解决什么问题？

PagedAttention 主要解决 KV cache 管理浪费的问题。

传统做法容易给每个请求预留较大的连续 KV cache 区域。问题是请求长度变化很大，有的很短，有的很长，预留空间会造成内部碎片；请求完成和新请求进入时，又会出现外部碎片。显存被浪费后，能放进同一个 batch 的请求数就变少。

PagedAttention 的思路接近操作系统里的分页。它把 KV cache 切成固定大小的 block，请求看到的是逻辑上的连续 token 序列，底层物理显存可以是不连续的 block。请求增长时按需分配新 block，结束后释放 block。

这样做的收益是显存利用率更高，batch 能容纳更多请求，长短请求混跑时也不那么容易被碎片拖垮。

放到 LogServe 里看，PagedAttention 属于 vLLM 内部的执行优化。LogServe 不需要重写它，但调度器应该理解 KV cache 压力，因为这会影响一个 worker 是否还能接新请求。

## Q544. continuous batching 对延迟和吞吐有什么影响？

continuous batching 允许服务端在生成过程中动态加入新请求，而不是等一个固定 batch 全部结束后再启动下一批。

它通常能提升吞吐。因为 GPU 不容易空转，新请求可以插入正在运行的服务循环里，短请求也不用等一个大 batch 完全结束。

但它对延迟的影响要分开看。

对系统总体吞吐和平均延迟，它往往是好事。GPU 利用率高了，单位时间能处理更多 token。

对单个请求尾延迟，它可能带来干扰。prefill 请求插入 decode 流程时，可能拉高已有请求的 inter-token latency。长 prompt、大 batch、混合长短请求时，这个问题更明显。

所以 LogServe 接 vLLM 后，不应该只记录 total latency。还要拆 TTFT、ITL、prefill queue、decode queue 和 batch size。否则调度器只看到一个模糊的总耗时，很难判断是 batching 带来的收益还是干扰。

## Q545. prefill/decode disaggregation 会如何影响 LogServe 调度模型？

prefill/decode disaggregation 会把一个 LLM 请求拆成两个资源阶段。

prefill 阶段处理 prompt，通常计算密集，决定 TTFT。decode 阶段逐 token 生成，持续占用 GPU，影响 ITL 和总延迟。

如果两类阶段放在不同 vLLM 实例或不同 GPU pool 上，LogServe 的调度单位就不能再只是“worker”。它要知道哪些 worker 是 prefill pool，哪些 worker 是 decode pool，还要记录 KV cache 从 prefill 到 decode 的传输成本。

事件日志也要细化。除了 `ModelLoadStarted`、`ModelLoaded`、`LLMCompleted`，还应该有 `PrefillStarted`、`PrefillCompleted`、`KVTransferStarted`、`KVTransferCompleted`、`DecodeStarted`、`DecodeCompleted`。

这样 predicted latency 才能分阶段估算。长 prompt 请求优先看 prefill pool，长输出请求优先看 decode pool。RAG 场景里，这个拆分很有价值，因为 RAG prompt 往往比普通 chat prompt 更长。

## Q546. 如果 worker 有多张 GPU，调度单位应该是 worker、GPU 还是 model replica？

单机多 GPU 下，只以 worker 为单位太粗。

worker 是进程或服务实例，适合作为心跳、日志、任务执行的管理边界。但真实调度应该看到 worker 内部的 GPU 资源。

GPU 是硬件资源边界。显存、算力、温度、当前队列、已加载模型都可能每张卡不同。一个 worker 有 4 张 GPU，其中一张满了，不代表整个 worker 都不能接任务。

model replica 是服务能力边界。一个模型副本可能占一张 GPU，也可能跨多张 GPU 做 tensor parallel。调度请求时，真正要找的是“能服务这个模型的 replica”，而不是抽象的 worker。

我会把三者都建模：worker 负责生命周期和心跳，GPU 负责资源上报，model replica 负责请求路由。LogServe 当前先做 worker 级调度，后续 GPU 化时应该把 `WorkerInfo` 扩展出 GPU 和 replica 两层。

## Q547. 如果模型需要 tensor parallel，worker capacity 如何建模？

tensor parallel 会让一个模型副本同时占用多张 GPU。此时 capacity 不能再写成简单的 `MaxTasks`。

应该把 capacity 改成资源向量。

比如一个模型需要 `tp=2`，就要同时拿到两张兼容 GPU。还要看这两张 GPU 是否在同一节点、NVLink 拓扑是否足够、显存是否都能放下 shard、当前是否已有其他 replica 占用。

调度时也要保证原子分配。不能先占到 GPU-0，再发现 GPU-1 不可用。否则会出现半分配状态。

对 LogServe 来说，比较自然的做法是在 Model Registry 里记录模型的并行需求，在 worker heartbeat 里上报 GPU topology 和当前 replica，然后调度器按资源向量做匹配。事件日志里记录 `ModelReplicaPlaced`，这样重启后能解释这个 replica 为什么占用了哪些 GPU。

## Q548. 如果模型可量化，调度器是否应考虑精度/显存/延迟 tradeoff？

应该考虑，但不能由调度器私自决定。

量化会改变显存占用和推理速度，也可能影响输出质量。比如 INT4、INT8、FP8、BF16 在显存和速度上差异明显，但质量、兼容性和 kernel 支持也不同。

更合理的设计是把量化形式作为模型变体注册到 Model Registry。用户或上层 workflow 明确请求某个服务等级，比如高质量、低延迟、低成本。调度器只在满足这个约束的候选模型里选择。

如果用户请求的是“低成本优先”，调度器可以选择显存占用更小、吞吐更高的量化模型。如果请求的是“质量优先”，就不应该偷偷降到更低精度。

这也影响 cache key。模型名相同但量化格式不同，不能共用同一个 checkpoint cache key。

## Q549. 如果多个模型热度不同，checkpoint cache 应该用 LRU、LFU 还是 cost-aware eviction？

LRU 简单，适合当前实现。它按最近访问时间淘汰最久没用的 checkpoint，代码容易写，行为也好解释。

LFU 更重视访问频率。热点模型即使短时间没访问，也不容易被淘汰。问题是它对流量变化反应慢，老热点可能长期占住空间。

cost-aware eviction 更适合 LLM。因为不同模型大小、加载时间、下载成本、热度都不同。淘汰一个 500MB 冷门模型和淘汰一个 30GB 热门模型，后果完全不一样。

我会先保留 LRU 作为 baseline，然后加 cost-aware 分数：

```text

eviction_score = reload_cost * future_access_probability / size

```

分数高的模型尽量保留，分数低的优先淘汰。这里的 reload_cost 可以来自 checkpoint_fetch_ms 和 model_load_ms，future_access_probability 可以用近期请求频率估算。

## Q550. 如果 checkpoint 大小差异很大，简单 LRU 有什么问题？

简单 LRU 只看最近访问，不看大小。

这会导致两个问题。

第一，大模型可能反复挤掉很多小模型。加载一个很大的 checkpoint 时，LRU 可能连续淘汰多个小模型。后面这些小模型又被访问，就会出现一串 cold miss。

第二，小而热的模型可能被误淘汰。它刚好短时间没被访问，就被一个大模型挤走。虽然它重新加载成本低一些，但请求频率高，累积影响不小。

对 LLM cache 来说，淘汰策略最好同时看 size、reload cost 和热度。一个很大、很冷、加载成本也不高的模型，应该比一个小而热的模型更容易被淘汰。

当前 LogServe 的 LRU 足够解释 cache 行为和实验结果，但要跑多模型生产负载，需要升级成 size-aware 或 cost-aware。

## Q551. 如何设计 admission control 防止 LLM 请求压垮 GPU？

admission control 应该发生在请求进入队列之前，至少要早于 worker 执行。

可以先做硬限制：每个模型最大并发、每个 worker 最大 LLM queue、每个租户最大 in-flight token、每张 GPU 最大显存水位、全局最大排队请求数。

再做 SLO 判断。根据当前队列、历史 TTFT、预计 tokens 和模型冷启动状态，估算请求是否还能在目标延迟内完成。如果明显做不到，可以拒绝、降级或返回排队过载。

还需要按请求成本限流。一个超长 prompt 或超大 max_tokens 请求，不能和短请求等价处理。否则少量长请求就能把 decode 资源拖满。

在 LogServe 里，admission decision 也应该写事件，比如 `LLMAdmissionRejected` 或 `LLMAdmissionDelayed`。这样 dashboard 才能解释“为什么请求没进队列”，而不是只看到吞吐下降。

## Q552. 如何根据 SLO 做 latency-aware scheduling？

先要把 SLO 明确成可计算的目标。比如 TTFT p95 小于 1s，total latency p95 小于 5s，或者某租户的排队时间不能超过 200ms。

调度器拿到请求后，估算每个候选 worker 的完成时间：

```text

estimated_latency = queue_wait + cold_start + prefill_time + decode_time + network_overhead

```

如果某个 worker 能满足 SLO，就优先选满足 SLO 且资源成本低的 worker。如果没有 worker 满足，就要触发 admission control：拒绝、降级模型、减少 max_tokens、排入低优先级队列，或者提示客户端重试。

这里要注意，SLO-aware 不等于永远选最快 worker。多租户系统还要考虑公平性。某个高优先级请求可以抢更快资源，但普通请求不能长期饿死。

LogServe 当前的 predicted latency 是基础。下一步可以把预测值和请求 deadline 结合，让调度器从“选最快”升级成“选能按时完成且不会破坏整体公平性”的 worker。

## Q553. 如何将 predicted latency 与 queueing theory 结合？

现在的 predicted latency 更像经验模型：历史 EWMA 加上一些惩罚项。它好解释，也容易实现。

排队论可以补上“等待时间”这一部分。比如每个 worker 或 model replica 都可以看成一个服务台，请求到达率是 λ，服务率是 μ。利用率接近 1 时，排队时间会急剧上升。

调度器可以把历史服务时间估成 μ，把最近请求到达率估成 λ，再用排队模型估算 queue wait。然后把它加到 predicted latency 里。

这样做的收益是：当某个 worker 看起来还有 capacity，但利用率已经很高时，调度器会提前感知排队风险，而不是等 running tasks 爆满才反应。

但排队论只是近似。LLM 请求服务时间分布很重尾，batching 也会改变服务模型。实践里可以用排队论给一个保守信号，再用真实 metrics 校正。

## Q554. M/M/1 或 G/G/k 排队模型能否帮助调度？

能帮助，但不能直接套公式就完事。

M/M/1 假设到达间隔和服务时间都是指数分布，且只有一个服务台。LLM serving 很难满足这些假设。请求长度差异大，服务时间重尾，vLLM 还有 continuous batching。

G/G/k 更宽松，允许一般到达分布和一般服务时间分布，也能表示多个服务台。它更接近多 worker 或多 replica 的情况，但估算也更复杂。

实际做法可以简单一点：用 M/M/1 或 G/G/k 的思想，而不是死套它们的前提。

比如用最近窗口估计到达率、服务时间均值和方差，给高利用率 worker 加非线性 queue penalty。利用率从 50% 到 70% 可能还能接受，但从 90% 到 98% 时，调度器应该非常保守。

所以排队模型适合做调度特征，不适合当唯一决策来源。

## Q555. 如果请求 prompt 长度差异很大，只用历史 total latency 是否足够？

不够。

历史 total latency 混在一起后，会把短 prompt 和长 prompt 的差异抹掉。一个 worker 过去处理的大多是短请求，它的 EWMA 会很好看；下一次来了一个超长 RAG prompt，预测就会偏低。

LLM 延迟至少要按 token 维度拆开。

prefill 主要跟 prompt tokens 有关。decode 主要跟输出 tokens 有关。batching、KV cache 和显存压力又会影响它们的斜率。

如果只用 total latency，调度器无法判断一个请求是“长输入短输出”还是“短输入长输出”。这两种请求适合的资源可能不同。

LogServe 当前的 total latency EWMA 是可用的起点，但接真实 GPU 后应该记录 prompt_tokens、completion_tokens、max_tokens，并建立 token-aware 的预测模型。

## Q556. 如何把 prompt length、max_tokens、batch size 纳入预测？

可以把延迟拆成几段估算。

```text

predicted_latency =
  queue_wait
  + cold_start
  + prefill_ms_per_token * prompt_tokens
  + decode_ms_per_token * expected_output_tokens
  + batching_adjustment

```

`prompt_tokens` 可以由 tokenizer 估算。`expected_output_tokens` 可以先用 max_tokens 的一部分估计，再用历史完成长度校正。

`prefill_ms_per_token` 和 `decode_ms_per_token` 应该按模型、worker、GPU 类型、batch size 分组统计。一个小模型和一个大模型的 token 成本不一样，同一个模型在不同 GPU 上也不一样。

batch size 的影响不是线性的。适度 batching 会提升吞吐，但 batch 太大可能拉高单请求延迟。可以记录每次 `LLMCompleted` 时的 batch size 和 queue wait，用回归或分桶统计做预测。

面试里不需要把模型讲得很玄。核心是：把总延迟拆成 token 成本和排队成本，别让一个 EWMA 承担所有信息。

## Q557. 如何避免调度器产生 cache thrashing？

cache thrashing 指模型反复被加载和淘汰，系统大部分时间花在搬模型，而不是生成 token。

要避免它，调度器需要看到 cache 压力。

第一，调度前检查模型大小和 worker cache 剩余容量。明显放不下的 worker 不应该被选中。

第二，对近期 eviction 多的 worker 加惩罚。LogServe 已经有 `eviction_count`，可以继续扩展成窗口化的 eviction rate。

第三，做模型放置约束。热点模型尽量固定在一组 worker 上，不要让每个 worker 都偶尔冷加载它。

第四，加入 admission control。如果一个低优先级模型会挤掉高优先级热点模型，可以拒绝、延后或调到专门的 cold pool。

第五，淘汰策略要 cost-aware。简单 LRU 在模型大小差异大时容易制造抖动。

简单说，调度器不能只问“现在谁能跑”，还要问“跑完会不会把 cache 搞乱”。

## Q558. 如果模型缓存状态由心跳上报，如何处理 stale cache view？

心跳上报一定会有滞后。处理办法不是假装它强一致，而是把 stale 当成正常情况。

第一，给 cached_models 加时间戳。调度器看到过旧的缓存视图时，要降低它的可信度。

第二，worker 在执行前做本地确认。控制面认为它有缓存，不代表本地文件一定还在。worker 应以本地 `ensureCheckpoint` 结果为准。

第三，执行结果反向修正视图。`ModelLoaded` 和 `LLMCompleted` 里的 cache_hit、checkpoint_fetch_ms、eviction_count 可以帮助控制面更新统计。

第四，在 eviction 后尽快 heartbeat。这样减少“控制面以为还有缓存，worker 实际已经淘汰”的窗口。

第五，如果 cache miss 与调度预测不一致，可以记录 `CacheViewStaleDetected` 之类事件，便于 dashboard 展示。

LogServe 当前是最终一致模型。这个说法比“调度器总是知道真实 cache 状态”更准确。

## Q559. 如果 GPU OOM，worker 应如何上报并触发重调度？

GPU OOM 不能只当普通 task failed 处理。它说明资源模型错了，调度器需要学习。

worker 发现 OOM 后，应该上报结构化错误：模型名、请求 tokens、max_tokens、当前显存使用、KV cache 使用、batch size、GPU id、是否发生在 prefill 还是 decode。

控制面收到后，应写事件，比如 `LLMResourceExhausted` 或 `LLMTaskFailed`，错误类型标成 GPU_OOM。然后更新 worker 健康状态或短时间降低该 worker 对这个模型的可用 capacity。

是否重调度要看错误性质。如果是某张 GPU 瞬时显存不足，可以换到更空的 worker 重试。如果是请求本身超过模型上下文或显存上限，重试没有意义，应该直接失败并告诉客户端。

还要防止重试风暴。OOM 后立即把同一批请求打到另一个 worker，可能把另一个 worker 也打爆。重调度要带 retry budget 和 backoff。

## Q560. 如果使用 KServe、Ray Serve、Triton，LogServe 应该集成在哪一层？

LogServe 更适合做上层 runtime 和控制面，而不是替代所有 serving framework。

KServe 更偏 Kubernetes 上的模型部署、流量入口和推理服务生命周期。LogServe 可以把 KServe endpoint 当成 model replica，负责 workflow、actor、shared log、调度决策和审计。

Ray Serve 自带 actor 和 replica 管理，适合 Python 分布式 serving。LogServe 如果接 Ray Serve，应该避免重复做底层 replica 扩缩容，而是把 Ray Serve 部署视为执行后端。

Triton 更偏高性能模型服务，适合多模型、多后端、GPU 推理。LogServe 可以在 Triton 前面做任务编排、租户限流、事件日志和结果引用。

一句话：LogServe 不需要取代它们。它可以作为 log-first 的 AI runtime，负责任务语义、恢复、调度和观测，把具体推理由 KServe、Ray Serve、Triton 或 vLLM 执行。

## Q561. 如果要做 multi-tenant LLM serving，模型缓存如何隔离？

缓存隔离要分两层：安全隔离和资源隔离。

安全上，租户之间不能因为共享 cache 泄漏模型权重、LoRA adapter 或 prompt 相关 KV cache。共享 base model 可以，但前提是 base model 是平台级公共资源；租户私有 adapter、私有 checkpoint、私有 prefix cache 不能混用。

资源上，每个租户要有 cache quota。否则大租户频繁加载模型，会把小租户的模型挤掉。checkpoint cache 的 eviction 也要考虑 tenant id，不能只做全局 LRU。

cache key 应该包含 tenant scope。公共模型可以是 `global/model-A`，私有模型应该是 `tenant-X/model-A`。如果支持共享 base weights 和私有 LoRA，还要把 base cache 和 adapter cache 分开。

LogServe 的 shared log 也要有 tenant namespace。模型加载、淘汰、请求完成、计费事件都要能按租户查询和审计。

## Q562. 如何实现 per-tenant quota 和 fair sharing？

先定义可计量资源：请求数、in-flight requests、input tokens、output tokens、GPU seconds、cache bytes、checkpoint fetch 带宽、队列长度。

每个租户有硬 quota 和软 quota。硬 quota 超了直接拒绝或排队；软 quota 超了可以降低优先级，但不一定立刻拒绝。

调度上可以做加权公平队列。每个租户有权重，调度器按权重分配 GPU 时间或 token 预算。高优先级租户可以多拿资源，但不能让低优先级租户完全饿死。

cache 也要做公平。可以给每个租户保底 cache 空间，再留一部分共享空间给热点模型竞争。

事件日志里要记录 quota decision，比如 `TenantQuotaChecked`、`TenantQuotaRejected`、`TenantUsageRecorded`。这样账单、审计和问题排查都有依据。

## Q563. 如何做模型预热？预热事件是否要写入 shared log？

模型预热就是在真实请求到来前，把模型 checkpoint 拉到本地，甚至提前加载到 GPU。

预热可以由三类信号触发：手工配置的常驻模型、历史热度预测、即将到来的 workflow 批量任务。比如每天上午固定有 RAG 流量，就可以提前把对应模型放到一组 worker 上。

预热事件应该写入 shared log。原因是预热会改变调度状态：worker cache 变化了，后续请求是否命中缓存也受它影响。

可以记录这些事件：

```text

ModelWarmupRequested
ModelWarmupStarted
ModelWarmupCompleted
ModelWarmupFailed

```

这样 replay 时能解释某个模型为什么已经在 worker 上，也能把 warmup 成本从用户请求延迟里拆出来。

不过预热不是免费的。它会占磁盘、显存和网络带宽。调度器要避免预热把正在服务的请求挤慢。

## Q564. 如何支持模型灰度发布和版本回滚？

需要把模型发布也纳入事件日志。

Model Registry 不能只保存一个当前模型。它应该保存多个模型版本、权重路径、adapter、量化格式、兼容的 runtime、流量比例和健康状态。

灰度发布时，控制面写入 `ModelRolloutStarted`，然后按比例把请求路由到新版本。每次请求的 LLM event 必须记录实际使用的 model version。否则出了问题，很难知道哪个请求跑了新模型。

如果新版本错误率高、延迟高或质量评估失败，控制面写 `ModelRollbackStarted` 和 `ModelRollbackCompleted`，把流量切回旧版本。

这里还有缓存问题。新版本需要预热，否则灰度初期大量 cold start 会污染延迟指标。回滚也一样，旧版本如果已经被淘汰，回滚时会慢。

所以灰度发布不是简单改一个指针。它要同时处理 registry、调度、cache warmup、指标对比和审计日志。

## Q565. 如何支持 prompt/result 的敏感信息脱敏？

第一原则是：不要默认把完整 prompt 和 result 写进 shared log。

shared log 是恢复和审计基础，保留时间可能很长。如果里面直接放用户 prompt、身份证号、邮箱、手机号、病历或企业秘密，风险很高。

更合理的做法是日志只放 metadata 和引用。比如 request_id、tenant_id、模型、token 数、hash、result_ref、脱敏摘要。原始 prompt 和 result 放到受控对象存储，并按租户权限、加密和生命周期策略管理。

脱敏可以在 SDK 或 control plane 入口做，也可以在写对象存储前做。常见字段可以用规则脱敏，复杂文本可以接 DLP 服务。

还要注意 result_ref 权限。不能让一个租户通过 ref 读取另一个租户的结果。

面试里我会强调：LogServe 的 log-first 不等于“把所有内容都写日志”。log 记录事实和引用，大对象和敏感内容要走受控存储。

## Q566. 如何记录 token usage 用于计费？

token usage 应该作为结构化事件记录。

每次 LLM 请求完成后，worker 或 adapter 需要上报 input_tokens、output_tokens、cached_prompt_tokens、model、model version、tenant_id、worker_id、开始时间、结束时间和是否成功。

控制面写入类似 `LLMUsageRecorded` 的事件，或者把这些字段放进 `LLMCompleted`。如果计费要独立审计，单独事件更清楚。

计费不能只看请求数。不同模型、不同 token 数、不同 GPU 类型、是否命中 prefix cache、是否使用高优先级队列，成本都不同。

失败请求也要定义计费规则。比如请求被 admission control 拒绝不计费；已经完成 prefill 但 decode 失败是否计费，要在产品语义里说清楚。

最后，usage event 要幂等。客户端重试或 worker redelivery 不能导致重复计费。可以用 task_id、attempt、tenant_id、usage_id 做去重键。

## Q567. 如何防止用户 prompt 泄漏到日志？

要从接口设计上限制，而不是靠开发者自觉。

TaskSpec 和 LLM event payload 中不要默认保存完整 prompt。日志里可以保存 prompt_hash、prompt_length、token_count、脱敏后的短摘要，以及指向安全对象存储的 ref。

SDK 侧可以提供 `sensitive=True` 或默认敏感模式。敏感模式下，prompt 只通过请求路径传给 worker，不落 shared log。必要时写入加密对象存储，并设置短生命周期。

控制面和 worker 的错误日志也要处理。很多泄漏不是 event log 泄漏，而是错误信息里打印了请求体。HTTP client、vLLM adapter、Python runner 都要避免把完整 prompt 打进 stderr。

还要做测试。比如提交包含邮箱、手机号、API key 形态的 prompt，跑完后 grep log 目录，确认没有明文出现。

这类问题不能只靠文档承诺，最好在 CI 里加防泄漏测试。

## Q568. 如何对 LLM task 做端到端 tracing，包括 vLLM 内部指标？

端到端 tracing 要把 LogServe 的 task 生命周期和 vLLM 的执行指标串起来。

入口处生成 trace_id。SDK submit、control enqueue、worker poll、local queue、model load、checkpoint fetch、vLLM HTTP call、first token、completion 都带同一个 trace_id。

LogServe 侧可以记录 spans：`SubmitLLM`、`ScheduleLLM`、`PollTask`、`LocalQueueWait`、`ModelLoad`、`CheckpointFetch`、`CallVLLM`、`CompleteTask`。

vLLM 侧需要采集 queue time、prefill time、decode time、TTFT、ITL、tokens per second、batch size、KV cache usage、GPU memory、错误类型。vLLM 支持 metrics 和 OpenAI-compatible serving，LogServe adapter 可以把 vLLM request id 与自己的 task_id 绑定。

最后在 dashboard 里按 task_id 查一条链路：请求什么时候提交，为什么调到这个 worker，模型是否 cold miss，vLLM 内部排了多久，首 token 慢在哪里。

这比只看 total_latency_ms 有用得多。

## Q569. 如果未来接入真实 GPU，你最先补哪三组 benchmark？

第一组是 cold start benchmark。

测不同模型大小、不同 checkpoint source、本地 cache hit/miss、不同并发 cold miss 下的 checkpoint_fetch_ms、model_load_ms、TTFT 和 total latency。这个实验能验证 LogServe 的 cache-aware scheduling 是否在真实模型上仍然有效。

第二组是 token-aware serving benchmark。

构造短 prompt、长 prompt、短输出、长输出、RAG prompt 混合流量，测 TTFT、ITL、tokens/s、p95/p99。这个实验能暴露只用 total latency EWMA 的局限。

第三组是 multi-worker locality benchmark。

准备多台 worker、多张 GPU、不同模型缓存分布，对比 resource-only、locality-aware、predicted-latency。指标包括 cache hit rate、cold start rate、GPU utilization、queue wait、SLO violation rate。

如果时间允许，我还会加一个 failure benchmark：vLLM endpoint 挂掉、GPU OOM、worker 重启、cache 文件丢失，看事件日志和重调度是否能解释故障。

## Q570. 如何向面试官解释 mock LLM 实验和真实 GPU 实验的边界？

我会先承认边界，再说清楚 mock 实验已经证明了什么。

mock LLM 不能证明真实模型吞吐，也不能证明 GPU 利用率、KV cache 管理、continuous batching、prefill/decode 延迟一定符合预期。它没有真实权重、真实 token 生成，也没有 vLLM 内部调度干扰。

但 mock 实验能证明 LogServe 自己的 runtime 逻辑：模型注册、LLM task 提交、worker 调度、checkpoint cache、cache hit/miss 指标、LLM event log、ReplayLLM、dashboard 和 benchmark 汇总。这些东西不依赖真实 GPU。

真实 GPU 实验要补的是执行层真实性：模型加载到底多慢，prefill 和 decode 怎样影响尾延迟，vLLM metrics 如何接入，GPU OOM 如何反馈调度器，batching 对吞吐和延迟的影响有多大。

所以我不会把 mock 结果包装成“真实 GPU 性能结论”。更准确的说法是：mock 实验证明控制面和恢复语义已经闭环；真实 GPU 实验用于验证性能模型和调度收益。这个边界讲清楚，反而更可信。
