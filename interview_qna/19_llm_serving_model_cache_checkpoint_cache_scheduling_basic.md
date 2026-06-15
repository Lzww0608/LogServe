# 七、LLM Serving、Model Cache、Checkpoint Cache 与调度策略（简单）

这一组问题讲 LogServe 的 LLM serving 链路。它不是只把 LLM 当普通 task 跑，而是把模型注册、worker 本地模型缓存、checkpoint cache、冷启动指标和调度策略放进同一条 runtime 路径里。

## Q481. LogServe 的 LLM serving 支持哪些 adapter？

当前主要支持两类 adapter。

第一类是 `mock`。这是默认 adapter，用来在没有 GPU、没有真实模型服务的机器上模拟 LLM 请求。worker 会模拟模型加载时间、首 token 延迟，然后返回一个 mock 文本。

第二类是 `vllm`。这个 adapter 会调用 vLLM 提供的 OpenAI-compatible HTTP 接口，也就是 `/v1/chat/completions`。

如果提交请求时没有指定 adapter，控制面会先看 Model Registry 里注册的 adapter；如果 registry 里也没填，就默认用 `mock`。

这个设计让项目在单机实验环境里也能跑完整链路，同时保留接真实 vLLM 服务的入口。

## Q482. mock LLM 的作用是什么？

mock LLM 是为了让 LLM serving 链路在没有 GPU 的环境下也能验证。

它主要模拟几件事：

- 模型第一次加载的冷启动时间。
- 首 token 延迟。
- cache miss 和 cache hit 的差异。
- LLM event log 的产生。
- LLM task 在 worker pool 中的执行路径。

没有 mock，就必须准备 GPU 和模型服务才能测试调度器、checkpoint cache、ReplayLLM、workflow 中调用 LLM 这些能力。对你的实验机器来说，mock 很重要，因为它可以稳定复现实验，不依赖外部模型服务状态。

mock 不追求生成质量。它的价值是让系统行为可测：冷启动有没有记录，cache hit 有没有变快，locality-aware 是否调到缓存命中的 worker。

## Q483. vLLM adapter 调用什么接口？

vLLM adapter 调用的是 OpenAI-compatible 的 chat completions 接口。

代码里会把 `LOGSERVE_VLLM_BASE_URL` 和路径拼成：

```text
/v1/chat/completions
```

请求体包含模型名、messages 和 max_tokens。messages 里目前就是一个 user prompt。

如果模型版本不是默认值，代码会把模型名和版本组合成一个模型标识传给 vLLM。返回时优先读取 `choices[0].message.content`，如果没有，再读取 `choices[0].text`。

所以接入真实 vLLM 时，关键配置是 vLLM base URL、模型注册信息，以及 worker 能访问这个 HTTP 服务。

## Q484. Model Registry 保存哪些字段？

Model Registry 保存一份模型元数据。当前主要字段是：

- 模型名称
- 模型版本
- 模型大小
- 模型路径
- adapter

注册模型时，控制面会先写 `system:models` stream 的 `ModelRegistered` 事件，然后再更新 metadata。也就是说，模型注册同样遵守 log-first。

提交 LLM 请求时，控制面会先检查模型是否已经注册。没注册的模型会直接拒绝，不会把未知模型任务投给 worker。

## Q485. worker 为什么要上报 cached_models？

因为 LLM 调度要知道哪个 worker 本地已经有目标模型。

worker 注册和 heartbeat 时会上报 cached_models。控制面把它保存到 worker metadata 里。调度器看到一个 LLM task 时，就能判断：

- worker-1 是否已经缓存 model-A。
- worker-2 是否已经缓存 model-B。
- 哪个 worker 执行这个请求更可能命中缓存。

如果不上传 cached_models，调度器只能看 worker 是否空闲。这样很容易把 model-A 请求发到没有 model-A 的 worker 上，造成不必要的 cold start。

## Q486. checkpoint cache 的 cold miss 会做什么？

checkpoint cache miss 指 worker 本地没有目标模型 checkpoint。

当前 worker 会做这几步：

1. 在 checkpoint source 目录里查找目标模型文件。
2. 检查文件大小是否超过本地 cache capacity。
3. 如果空间不够，按最近使用时间淘汰旧 checkpoint。
4. 把 checkpoint 从 source 复制到 worker 本地 cache 目录。
5. 读取 checkpoint，模拟或计算模型加载时间。
6. 写 manifest，记录 checkpoint 文件、模型名、大小和最近访问时间。
7. 更新本地 cache 索引。
8. 在 LLM event 里记录 checkpoint_fetch_ms、model_load_ms、cache_used_bytes、cache_capacity_bytes 和 eviction_count。

这个过程就是冷启动的主要来源。实验里第一次请求通常 cache_hit=false，checkpoint_fetch_ms 和 model_load_ms 会大于 0。

## Q487. warm cache 命中后会省掉什么开销？

warm cache 命中后，worker 不需要再从 checkpoint source 复制模型文件到本地 cache。

省掉的主要是：

- checkpoint fetch 时间。
- 可能发生的 eviction。
- 冷启动时的远端或源目录读取成本。
- 重新建立本地 checkpoint 文件和 manifest 的成本。

当前实现里，命中后仍会读取本地 checkpoint 来模拟模型加载，所以 `model_load_ms` 可能仍然大于 0。但 `checkpoint_fetch_ms` 应该是 0，cache_hit 是 true。

这也符合实验结果：cold 请求要拉 checkpoint，warm 请求直接使用本地 cache，所以总延迟明显更低。

## Q488. RESOURCE_ONLY 策略是什么？

`RESOURCE_ONLY` 是最朴素的调度策略。

它只看 worker 是否活跃、是否有 capacity，不关心目标模型是否已经在 worker 本地缓存。哪个 worker 来 poll，满足资源条件就可以拿到任务。

这个策略适合作为 baseline。它能说明：如果只看空闲资源，LLM 请求可能被发到没有模型缓存的 worker 上，导致更多 cold start。

在实验中，RESOURCE_ONLY 的 cache hit rate 通常低于 locality-aware，p95/p99 latency 也更容易被 cold start 拉高。

## Q489. LOCALITY_AWARE 策略是什么？

`LOCALITY_AWARE` 会同时看资源和模型缓存。

它会优先选择已经缓存目标模型的 worker。当前打分里，缓存命中会有明显加分；worker running tasks 越多，分数会下降；如果系统里已经有缓存命中的可用 worker，并且任务排队时间还不长，未缓存 worker 会被明显惩罚。

这背后的想法很直接：如果稍微等一下就能让请求跑到已有模型的 worker 上，就不要急着把它发给没有缓存的 worker。

当然，locality-aware 不能无限等。如果一直等缓存 worker，队列延迟也会变高。当前实现里有 queue delay 相关判断，后续可以继续做得更精细。

## Q490. PREDICTED_LATENCY 策略是什么？

`PREDICTED_LATENCY` 是基于历史观测的调度策略。

它不只是问“有没有缓存”，还会看这个 worker 过去处理该模型请求的延迟表现。控制面会从 LLMCompleted 事件里维护一份 materialized stats，包括请求数、cache hit 数、总延迟 EWMA、模型加载 EWMA、checkpoint fetch EWMA 和最近 eviction 数。

调度时，它会估算每个 worker 的 predicted latency，再选择预测延迟最低的 worker。

这个策略能处理一个 locality-aware 覆盖不好的场景：有缓存的 worker 未必总是最快。如果某个缓存 worker 很忙，或者历史上该 worker 延迟很高，另一个 worker即使需要冷启动，也可能更快。

## Q491. locality-aware 为什么能降低 cold start？

LLM cold start 的大头通常是模型加载和 checkpoint fetch。

如果 worker 本地已经有目标模型，就不需要重新复制 checkpoint，也不需要模拟完整冷加载。locality-aware 调度会把同一模型的请求尽量发到已经缓存该模型的 worker 上，所以 cache hit 变多。

cache hit 变多后：

- checkpoint_fetch_ms 降低。
- model_load_ms 降低或更稳定。
- total_latency_ms 降低。
- p95/p99 更不容易被少量 cold miss 拉高。

你之前实验里 locality-aware 的 cache hit rate 高于 resource-only，cold start latency 和 p95 latency 更低，就是这个机制的体现。

## Q492. LLM task 和普通 task 在 TaskSpec 中有什么区别？

普通 task 主要依赖函数名、函数源码和参数 JSON。worker 会把它交给 Python runner 执行。

LLM task 不走用户 Python 函数。它在 TaskSpec 里会带 LLM 专用信息：

- task name 通常是 `llm:<model>`
- function name 是内部保留名
- args 里放 prompt
- 模型名称
- 模型版本
- adapter
- max tokens

worker 看到 TaskSpec 里有 LLM 模型名称，就会进入 `runLLMExecutor`，而不是普通 Python executor。

这也是为什么 LLM task 可以接入 model cache、checkpoint cache 和 LLM event log。它不是普通 task 的黑盒函数调用，而是 runtime 认识的一类任务。

## Q493. LLM event stream 中有哪些事件？

每个 LLM task 有自己的 stream，形如 `llm:<task_id>`。

当前主要事件有三个：

- `ModelLoadStarted`
- `ModelLoaded`
- `LLMCompleted`

`ModelLoadStarted` 记录模型加载开始，以及当时是否已经 cache hit。

`ModelLoaded` 记录模型加载完成后的指标，比如 cache_hit、checkpoint_fetch_ms、model_load_ms、cache_used_bytes、cache_capacity_bytes、eviction_count。

`LLMCompleted` 记录请求完成后的指标，比如 first_token_ms、total_latency_ms，同时也带上 cache 和 checkpoint 指标。

ReplayLLM 就是读取这个 stream，把这些事件重新组装成一次 LLM 请求的执行历史。

## Q494. ReplayLLM 可以重建哪些指标？

ReplayLLM 可以从 `llm:<task_id>` stream 重建：

- task_id
- 模型名称
- 模型版本
- worker_id
- cache_hit
- checkpoint_fetch_ms
- cache_used_bytes
- cache_capacity_bytes
- eviction_count
- model_load_ms
- first_token_ms
- total_latency_ms
- 完整事件列表

它不重新调用模型，也不重新执行请求。它只是根据 LLM event log 还原这次请求发生过什么。

这个能力对实验很有用。比如你能证明某次请求是 cold miss，下一次是 warm hit；也能把模型加载、首 token、总延迟拆开看。

## Q495. EWMA stats 用来做什么？

EWMA stats 用来给 predicted-latency 调度提供历史依据。

控制面每看到一个 `LLMCompleted`，就按模型、版本、worker 三元组更新一份统计：

- request_count
- cache_hit_count
- total latency 的 EWMA
- model load 的 EWMA
- checkpoint fetch 的 EWMA
- 最近 eviction count
- last updated time

EWMA 的好处是既能反映历史，又不会被很久以前的数据完全绑住。新的样本会逐步影响预测。

当前代码里更新公式比较简单：新值大约占 30%，旧值大约占 70%。这不是复杂模型，但足够让调度器从“只看静态缓存”升级到“看历史延迟”。

## Q496. predicted_latency 公式里有哪些项？

当前 predicted latency 主要由几部分组成：

- base latency：优先使用该 worker 历史总延迟 EWMA，没有历史时用默认值。
- queue / load penalty：worker running tasks 越多，预测延迟越高。
- cold start penalty：如果 worker 没有目标模型缓存，就加上模型加载 EWMA 和 checkpoint fetch EWMA；没有历史时给默认冷启动惩罚。
- eviction penalty：最近发生过 eviction，会加额外惩罚。
- queue delay 调整：任务排队时间变长时，调度器会减少对非缓存 worker 的惩罚，避免为了 locality 等太久。

所以它不是纯 cache-aware，也不是纯 resource-aware。它把历史延迟、缓存状态、worker 负载和 eviction 都放进一个预测分数里。

## Q497. eviction_count 代表什么？

`eviction_count` 表示这次模型加载为了腾出 checkpoint cache 空间，淘汰了多少个旧 checkpoint。

比如本地 cache 容量有限，要加载 model-D，但空间不够，就会按最近使用时间淘汰旧模型 checkpoint。淘汰 1 个，eviction_count 就是 1。

这个指标有两个用途。

第一，它说明本次请求不只是冷启动，还造成了 cache 抖动。后续请求如果需要被淘汰的模型，可能又要 cold miss。

第二，predicted-latency 策略可以用它做惩罚。一个 worker 频繁 eviction，说明它的 cache 压力大，把更多请求调给它未必划算。

## Q498. cache_capacity_bytes 和 cache_used_bytes 为什么要上报？

这两个指标用来观察 checkpoint cache 的容量压力。

`cache_capacity_bytes` 是 worker 本地 checkpoint cache 的容量上限。`cache_used_bytes` 是当前已经使用的容量。

只看 cache hit 不够。一个 worker 现在命中了模型，但如果 cache 已经快满了，下一次加载新模型可能会触发 eviction。上报容量和使用量后，dashboard 和调度器可以看到：

- 哪个 worker cache 快满了。
- 哪个模型加载导致 used bytes 上升。
- eviction 是否和容量不足有关。
- 是否需要调大 cache 或调整模型放置。

这也是把 LLM serving 做成 infra 项目的关键点之一。只返回文本不够，还要看模型缓存的运行状态。

## Q499. LLM request 如何作为 workflow step 使用？

在 workflow 里，LLM 请求可以被建模成一个 step。上游 step 负责准备 prompt 或检索结果，下游 step 使用 LLM 输出。

比如 RAG workflow：

```python
@workflow
def rag(query):
    vec = embed(query)
    docs = search(vec)
    answer = generate_with_llm(query, docs)
    return answer
```

这里 `generate_with_llm` 可以对应一个 LLM task。控制面调度到这个 step 时，TaskSpec 会带模型名、adapter、prompt 和 max tokens。worker 执行时进入 LLM executor，写 LLM event stream，最终结果再回到 workflow step。

这样 workflow runtime 和 LLM serving 就不是两个孤立 demo。RAG 这样的 AI workflow 能直接复用模型缓存感知调度。

## Q500. 为什么不能只按 worker 空闲程度调度 LLM task？

因为 LLM task 的主要成本不一定是 CPU 空闲度，而是模型是否已经在本地。

一个 worker 很空闲，但没有目标模型，可能要花很多时间加载 checkpoint。另一个 worker 正在跑任务，但已经缓存了目标模型，实际完成得可能更快。

只按空闲程度调度会带来几个问题：

- cache hit rate 低。
- cold start 多。
- p95/p99 latency 被冷启动拉高。
- checkpoint cache 频繁复制和淘汰。
- 模型加载成本被隐藏在“任务执行时间”里，调度器无法优化。

所以 LLM 调度至少要看资源和 locality。更进一步，还要看历史延迟、queue delay、cache capacity 和 eviction。LogServe 的三个策略就是从简单 baseline 往这个方向逐步推进。
