# 十二、高频链式追问脚本：从 LLM scheduling 开始

这一组问题要把 LLM 调度从“缓存命中优先”讲到“延迟预测”。LogServe 当前有三种策略：`RESOURCE_ONLY` 只看 worker 是否有空闲容量；`LOCALITY_AWARE` 把模型缓存纳入调度；`PREDICTED_LATENCY` 再加入历史延迟、冷启动代价、checkpoint fetch 和 eviction 信号。回答时不要把 mock 实验说成真实 GPU 性能结论，它验证的是调度链路和相对趋势。

## Q866. 为什么 LLM task 需要 locality-aware scheduling？

LLM task 和普通 task 的最大差异是本地性。普通 Python task 只要 worker 有 CPU 和执行环境，调到哪台机器差别不大。LLM task 要考虑模型是否已经在 worker 本地缓存里，甚至是否已经在 GPU 显存里热着。

一次 LLM 冷启动通常包含几段成本：从对象存储或本地目录取 checkpoint，读模型文件，加载权重，初始化推理后端，做第一次 token 前的准备。模型越大，这些成本越高。把请求调度到已经缓存同一模型的 worker，可以省掉大部分冷启动时间。

LogServe 的 worker 会通过注册和 heartbeat 上报 `cached_models`。control plane 调度 LLM task 时，根据 task 里的模型名和模型版本，检查 worker 的缓存状态。`LOCALITY_AWARE` 策略会给 cache hit worker 更高分，同时仍然考虑 worker capacity 和 running tasks。

项目实验里这个趋势已经能看到。单机三 worker 场景下，resource-only 的 cache hit rate 是 0.833，locality-aware 是 1.0；p95 latency 从 305ms 降到 205ms。样本规模不大，但方向和系统设计一致：连续提交 model-A 请求时，调度器应该优先选择已有 model-A cache 的 worker。

所以 LLM task 需要 locality-aware scheduling，根本原因是模型冷启动本身就是 LLM serving 的一大块延迟来源。

## Q867. 只看 cache hit 会不会导致负载不均？

会。只看 cache hit 很容易把所有请求都打到同一个 cached worker 上。比如 worker-1 缓存了 model-A，worker-2 和 worker-3 没有缓存。连续 model-A 请求都进 worker-1，短期 cache hit rate 会很好，但 worker-1 的队列会越来越长，最终 p95/p99 latency 反而变差。

LogServe 当前的 `LOCALITY_AWARE` 没有只看 cache hit。它的打分大致是：

- 有剩余 capacity 才能被考虑；
- `available capacity` 加分；
- `running tasks` 扣分；
- cache hit 大幅加分；
- 如果存在可用 cached worker，且任务排队时间还没超过阈值，cold worker 会被扣分。

这相当于给缓存一个强偏好，但没有完全忽略负载。`PREDICTED_LATENCY` 又进一步把 running tasks 转成延迟惩罚，把历史耗时放进预测。

生产环境里还要继续补几类机制：

- 每个 worker 的本地 executor queue 长度；
- vLLM 队列和正在 decode 的 token 数；
- GPU 显存余量；
- per-tenant fair share；
- cache 命中后的最大等待时间；
- 随机化或 tie-break，避免同分 worker 长期被饿死。

所以面试回答要承认风险：cache locality 是强信号，但不能单独当作调度器。

## Q868. cached worker 忙时，是否应该等它还是用 cold worker？

取决于两个时间谁更小：cached worker 的等待时间，还是 cold worker 的冷启动加执行时间。

如果 cached worker 只是短暂忙碌，等它通常更划算。比如 model-A 冷启动要 2 秒，cached worker 队列只要等 100ms，那就应该等。反过来，如果 cached worker 排队已经很久，而 cold worker 加载模型只要 300ms，那用 cold worker 更合理。

当前 `LOCALITY_AWARE` 用一个简单阈值表达这个取舍：`localityQueueWait = 250ms`。当存在有缓存且有容量的 worker，并且任务排队时间还没超过这个阈值时，cold worker 会被明显降分。排队时间超过阈值后，cold worker 不再被强压制。

`PREDICTED_LATENCY` 的做法更细一点。它会估计每个 worker 的总延迟：

```text
predicted =
  historical_total_latency
  + running_task_penalty
  + cold_start_penalty
  + eviction_penalty
```

这个公式本质上就是在回答“等热 worker 还是用冷 worker”。如果冷启动代价很高，热 worker 更可能胜出；如果热 worker 太忙，冷 worker 也会被选中。

生产里这个判断还应该加入真实队列信号，例如 vLLM waiting requests、prefill/decode backlog、GPU utilization 和模型加载带宽。当前项目用简单规则把主链路跑通，后续可以逐步替换成更真实的 latency model。

## Q869. PREDICTED_LATENCY 相比 LOCALITY_AWARE 的改进是什么？

`LOCALITY_AWARE` 是规则型策略。它知道哪个 worker 有缓存，也知道 worker 大概还有多少 capacity，但它并不知道某个 worker 历史上服务这个模型到底快不快。它对冷启动的估计也比较粗。

`PREDICTED_LATENCY` 把调度从“命中缓存优先”推进到“预测完成时间最短”。它使用 `(model_name, model_version, worker_id)` 维度的 materialized stats：

- request_count；
- cache_hit_count；
- ewma_total_latency_ms；
- ewma_model_load_ms；
- ewma_checkpoint_fetch_ms；
- last_eviction_count；
- last_updated_ms。

调度时，如果 worker 有该模型缓存，cold start penalty 接近 0；如果没有缓存，就把历史 model load 和 checkpoint fetch 作为惩罚。worker 当前 running tasks 也会变成排队惩罚。发生 eviction 的 worker，还会加 eviction penalty。

这样有两个好处。

第一，不同 worker 的硬件差异能被历史数据吸收。即使两台机器都 cache hit，快的那台 EWMA latency 会更低。

第二，冷启动不是固定常数。某些模型 checkpoint 大，某些模型小；某些 worker 本地磁盘快，某些慢。历史 `model_load_ms` 和 `checkpoint_fetch_ms` 可以让调度器更贴近实际代价。

当前实现仍然比较轻量。EWMA 的权重是 `previous*7 + sample*3`，没有按 prompt length、max tokens、batch size 建模。它已经比纯 cache hit 更可扩展，但还不是完整的 GPU serving 调度器。

## Q870. EWMA stats 从哪里来？重启后怎么恢复？

EWMA stats 来自 LLM event log。worker 执行 LLM task 时，会写三个事件到 `llm:<task_id>` stream：

- `ModelLoadStarted`
- `ModelLoaded`
- `LLMCompleted`

`LLMCompleted` 里包含 model、worker、cache_hit、model_load_ms、checkpoint_fetch_ms、first_token_ms、total_latency_ms、cache_used_bytes、cache_capacity_bytes、eviction_count 等字段。

control plane 在 LLM task 完成后，会读取对应 `llm:<task_id>` stream，找到 `LLMCompleted`，调用 `materializeLLMCompleted` 更新内存里的 `llmStats`。这张 stats map 的 key 是：

```text
(model_name, model_version, worker_id)
```

更新 EWMA 的逻辑很简单：

```text
第一次样本：ewma = sample
后续样本：ewma = previous * 0.7 + sample * 0.3
```

重启恢复靠 replay。control plane bootstrap 时会 `ListStreams("llm:")`，逐个读 LLM stream，重新 materialize 所有 `LLMCompleted` 事件。这样 predicted-latency 的输入不会只停留在内存里，可以从 shared log 重建。

这个设计还有一个好处：调度 stats 和审计历史来自同一份事件。dashboard、ReplayLLM、scheduler 看到的模型加载历史可以互相对照。

## Q871. 冷启动 penalty 如何估计？

当前 `PREDICTED_LATENCY` 的冷启动 penalty 主要来自两项：

```text
ewma_model_load_ms + ewma_checkpoint_fetch_ms
```

如果 worker 没有缓存目标模型，调度器会查询这个 worker 过去服务该模型时的统计。历史上加载慢，penalty 就高；历史上 checkpoint fetch 慢，penalty 也高。没有历史样本时，代码里使用一个默认值，大致让 cold worker 不会轻易超过 cached worker。

真实环境里冷启动 penalty 应该拆得更细：

- checkpoint 从对象存储下载的时间；
- 本地磁盘读取时间；
- 权重加载到 GPU 显存的时间；
- tokenizer、runtime、CUDA kernel 初始化；
- vLLM engine 建立 model executor 的时间；
- 第一次 prefill 的额外抖动；
- LoRA adapter 或量化权重加载时间。

估计方式也可以分层。短期用 EWMA，因为它简单、稳定、对噪声不太敏感。长期可以按模型大小、checkpoint source、worker 磁盘、GPU 类型建立更明确的模型。比如：

```text
cold_start =
  checkpoint_size / effective_bandwidth
  + historical_model_load
  + gpu_memory_pressure_penalty
```

当前项目已经记录 `model_load_ms` 和 `checkpoint_fetch_ms`，这两项足够支撑单机实验和调度策略对比。真实 GPU 上要把这些字段扩展到更细的 latency breakdown。

## Q872. checkpoint cache 的 LRU eviction 会不会导致 cache thrashing？

会，尤其在工作集大于 cache 容量时。

当前 worker 的 checkpoint cache 是本地磁盘缓存。缓存 key 是模型名加模型版本；cache miss 时从 checkpoint source 拷贝到 worker 本地；cache 空间不足时按 LRU 删除最久未访问的 checkpoint。`checkpointMu` 能避免同一 worker 上多个并发 miss 重复下载同一个模型，这点很重要。

LRU 的问题也很清楚。假设缓存只能放两个模型，请求序列是 A、B、C、A、B、C。LRU 会一直把刚要用的模型删掉，形成 thrashing。模型大小差异大时问题更明显：一个大模型可能挤掉多个小模型，但它未必比小模型更常用。

LogServe 当前已经记录 `eviction_count`，调度器也会把 last eviction count 转成 penalty。这能让 predicted-latency 看到“这个 worker 最近因为缓存压力付出了代价”。但这还不够，eviction policy 本身仍然只是 LRU。

后续可以改成几种策略：

- cost-aware eviction：综合模型大小、加载成本、访问频率；
- LFU/LRU 混合：避免短期扫描流量污染缓存；
- model pinning：把常用模型固定在指定 worker；
- admission control：一次性低频模型不进入缓存；
- per-tenant cache quota：防止一个 tenant 把缓存打爆；
- prewarm：根据 workload 提前加载热门模型；
- shard-aware placement：让不同 worker 负责不同热模型。

所以回答可以直接说：LRU 是可工作的 baseline，不是生产级最优策略。

## Q873. mock LLM 结果如何证明真实 vLLM 场景有效？

mock LLM 不能证明真实 vLLM 的绝对性能。它能证明的是调度逻辑和事件链路。

当前 mock 实验覆盖了这些点：

- worker 会模拟模型加载和 first token 延迟；
- cache miss 和 cache hit 会产生不同 latency；
- `ModelLoadStarted / ModelLoaded / LLMCompleted` 会写入日志；
- ReplayLLM 能从日志恢复模型加载和执行历史；
- checkpoint cache probe 能看到 cold miss 后生成本地 checkpoint artifact，warm hit 后跳过 fetch；
- locality-aware 的 cache hit rate 和 p95 latency 优于 resource-only；
- predicted-latency 可以用 materialized EWMA stats 做调度。

这说明 LogServe 的调度框架是通的：请求能携带模型信息，worker 能上报缓存，control 能根据策略选择 worker，event log 能 replay，实验能对比策略。

真实 vLLM 场景还需要单独验证。mock 不包含 GPU 显存压力、prefill/decode 分离、continuous batching、KV cache、CUDA kernel、tensor parallel、不同 prompt length 等因素。真实实验要补这些指标：

- time to first token；
- tokens/s；
- prefill latency；
- decode latency；
- batch size；
- GPU memory usage；
- queue wait inside vLLM；
- OOM 和 eviction；
- prompt length / output tokens 分布。

我会这样对面试官说：

> mock 实验证明调度和恢复机制有效，不证明 GPU 性能数值。它是控制变量实验；真实 vLLM 实验要验证同一套策略在 GPU 队列和 token 级执行下是否仍然成立。

## Q874. 如果真实 GPU 有 continuous batching，LogServe scheduler 的作用是什么？

continuous batching 是 vLLM 内部的请求执行策略。它解决的是一个模型 replica 内部如何把多个请求拼成批次、如何在 prefill 和 decode 阶段共享 GPU 资源。

LogServe scheduler 位于更上层。它解决的是请求应该送到哪个 worker、哪个模型 replica、哪个 GPU，什么时候应该冷启动，什么时候应该等热缓存，是否要受 tenant quota 和 workflow 优先级约束。

两者不冲突。可以这样分层：

- LogServe 负责跨 worker / 跨 replica 调度。
- vLLM 负责单 replica 内 token 级 batching。
- worker heartbeat 把 vLLM 内部队列、GPU 显存、running requests、tokens/s 上报给 control。
- control 根据这些信号决定后续请求落点。

举个例子，worker-1 已经加载 model-A，但 vLLM 队列里有很多长 prompt；worker-2 没有 model-A，但 GPU 空闲。如果只看 cache，worker-1 看起来更好。加入 vLLM queue metrics 后，调度器可能选择 worker-2 冷启动，或者把请求放入 worker-1 等待，取决于预测延迟。

所以 LogServe scheduler 的作用是给 vLLM replica 做上游流量选择和资源治理。vLLM 管单个推理引擎内部，LogServe 管整个 runtime 的 workflow、actor、模型缓存和多 worker 资源。

## Q875. 如果要支持多 GPU、多模型、多租户，调度器怎么扩展？

第一步是把调度单位从 worker 扩展成更细的 resource target。现在调度基本以 worker 为单位；多 GPU 场景下，target 应该变成：

```text
(worker_id, gpu_id, model_replica_id)
```

worker heartbeat 也要扩展。现在上报 capacity、running tasks、cached models。后续要上报：

- 每张 GPU 的显存总量、已用显存、空闲显存；
- 每个 GPU 上加载了哪些模型；
- 每个模型 replica 的队列长度；
- prefill/decode backlog；
- tokens/s；
- 最近 OOM 或 eviction；
- 本地 checkpoint cache 使用量；
- NUMA / PCIe 拓扑，至少要能表达 GPU 亲和性。

第二步是扩展 Model Registry。模型不只需要 name、size、path，还要记录显存需求、tensor parallel 度、quantization、LoRA adapter、最大 context、是否允许 batching、预热策略。模型和 worker 的匹配要从“有没有缓存”变成“这个 GPU 是否能稳定服务这个模型”。

第三步是加入多租户策略。调度器需要 tenant_id、project_id、quota、priority、SLO、budget。一个 tenant 不能因为大量冷启动请求把所有 GPU cache 冲掉。常见策略包括：

- per-tenant concurrency limit；
- per-tenant GPU time quota；
- fair queue；
- priority queue；
- cache quota；
- noisy neighbor 隔离；
- 超限请求进入 backpressure 或排队。

第四步是把 `PREDICTED_LATENCY` 做成可插拔的 cost model。不同任务的预测变量不一样：短 prompt 更受排队影响，长 output 更受 decode 影响，大模型更受显存和 tensor parallel 影响。调度器需要把 prompt length、max tokens、batch size、cache state、GPU memory、queue wait 都放进预测。

最后还要把事件日志扩展起来。`LLMCompleted` 现在记录 model load、first token、total latency、cache hit。多 GPU 场景要继续记录 gpu_id、replica_id、tenant_id、prompt_tokens、output_tokens、batch_size、gpu_memory、queue_wait、prefill_ms、decode_ms。这样 replay 和 benchmark 才能解释调度决策。

我会用一句话收束：

> 多 GPU、多模型、多租户下，调度器要从“选 worker”升级为“选模型副本和资源配额”。cache locality 仍然有用，但它只是 cost model 里的一个特征。
