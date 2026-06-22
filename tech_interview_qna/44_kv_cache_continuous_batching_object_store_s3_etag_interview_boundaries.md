# 44. KV cache、continuous batching、object store 与 S3 ETag 追问链

这一批继续按“面试官追问链”来写，不做百科式定义。KV cache 和 continuous batching 属于 LLM serving 的运行时边界；object store 和 S3 ETag 属于结果持久化、数据完整性和存储语义边界。它们放在一起，是因为线上事故常常发生在两类误解里：把运行时中间态当成普通缓存，把对象存储语义当成本地文件系统语义。

面试时不要只说“KV cache 加速推理”“continuous batching 提高吞吐”“对象存储适合大文件”“ETag 可以校验文件”。这些说法都太薄。更好的回答要能说明：状态属于谁，什么时候创建，什么时候可见，什么时候释放，哪些条件下能复用，哪些条件下只能丢弃或重算。

## Q001. 面试官如果只问一个问题检验你是否理解 KV cache，可能会问什么？

**回答：**

我会预期他问这个问题：

```text
一个 LLM 在线服务上线长上下文和流式输出后，吞吐没有明显提升，TTFT p99 变高，decode 阶段偶发 OOM。有同事说“我们已经有 KV cache，为什么还慢？”你怎么解释 KV cache 在 prefill、decode、prefix 复用、取消请求和多租户隔离里的生命周期？你会检查哪些不变量？
```

这个问题比“KV cache 是什么”更有区分度。它会逼你把 cache、显存、调度器和正确性边界放在一起讲。

KV cache 缓存的不是用户看到的文本，也不是模型权重，而是 Transformer attention 里每层、每个 token 的 key/value 中间状态。自回归生成时，模型每生成下一个 token，都要关注历史 token。如果每一步都重新计算历史 token 的 K/V，代价很高。KV cache 的价值在于：历史 token 的 K/V 已经算过，下一步 decode 直接读取它们，只为新 token 追加新的 K/V。

这里有两个阶段要分清。

prefill 阶段处理 prompt。长 prompt、RAG 拼接、多轮对话历史都会在这一阶段生成一大段初始 KV。prefill 主要影响 TTFT，因为第一个 token 必须等 prompt 的状态建好后才能生成。decode 阶段逐 token 生成输出，每一步读取已有 KV，再追加新 token 的 KV。decode 更影响 TPOT 或 inter-token latency。

KV cache 的生命周期通常绑定请求、会话、prefix 或调度器里的 block ownership。一个请求结束、失败、取消或被抢占后，它持有的 KV block 必须释放或转入明确的复用结构。不能只删除请求对象，却让显存 block 继续挂在 allocator 里。更不能让另一个租户的请求“碰巧”拿到旧 block。

prefix cache 是另一个容易被追问的点。如果多个请求有完全相同的前缀，系统可以复用这个前缀的 KV。听起来简单，实际条件很苛刻：模型权重版本、tokenizer、chat template、LoRA adapter、special tokens、position/RoPE 语义、attention mask、权限边界都要一致。相同字符串不一定是相同 token 序列；相同 token 序列也不一定允许跨租户复用。

我会这样回答：KV cache 是 LLM decode 阶段的历史 attention 状态，不是普通业务缓存。理解 KV cache 要讲清楚 prefill 如何生成初始 KV、decode 如何追加 KV、allocator 如何分配和回收 block、prefix cache 在什么条件下能复用，以及失败、取消、抢占时如何保证半写状态不会被后续请求读到。

## Q002. KV cache 的一句话定义是否容易误导，误导点在哪里？

**回答：**

容易误导。最常见的一句话是：KV cache 会缓存历史 token，避免重复计算。这句话方向没错，但容易带出几个错误判断。

第一个误导是把 KV cache 理解成 token 文本缓存。它不是把 prompt 字符串或 token id 存一份。它存的是模型每层 attention 里的 key/value tensor。不同模型结构、head 形状、KV head 数、position encoding、量化方式都会影响它。业务层看到的“同一段话”，在模型运行时未必是同一份 KV。

第二个误导是把它当成模型权重缓存。模型权重是相对静态的参数，跟模型版本和实例生命周期绑定。KV cache 是动态中间状态，跟请求上下文和生成进度绑定。权重缓存命中通常解决冷启动和加载成本；KV cache 解决的是自回归 decode 重算历史 token 的成本。两个都叫 cache，但安全边界完全不同。

第三个误导是认为 KV cache 一定只会让系统更快。长上下文下 KV 会占大量显存，读写 KV 还会消耗显存带宽。固定大小 cache 可能浪费空间；动态 cache 可能带来 allocator 开销；paged cache 可以降低碎片，但要付出 block table 和调度管理成本；offload 到 CPU 能救 OOM，却会增加搬运延迟。它不是免费优化。

第四个误导是忽略正确性。KV 绑定的不只是 token，还绑定模型版本、tokenizer、adapter、position、attention mask 和权限上下文。错误复用不会像空指针那样立刻崩溃，它可能表现为输出跑题、幻觉增加、同一请求偶发不同结果，甚至把 A 用户上下文影响到 B 用户输出。这个风险比慢更难排查。

第五个误导是把 KV cache 当成可持久化 checkpoint。通常情况下，GPU 上的 KV 是运行时状态。worker 崩溃后，除非你实现了外部 KV store、完整校验、版本绑定和恢复协议，否则更稳的做法是重新 prefill，而不是假装能从一段残留 KV 继续 decode。

所以我会把定义改成：KV cache 是自回归推理过程中，每层 attention 对历史 token 计算出的 K/V 中间状态；它减少重复计算，但会消耗显存和带宽，并且只能在模型、tokenizer、位置、权限和生命周期都明确的条件下复用。

## Q003. KV cache 最常见的生产事故触发条件是什么？

**回答：**

最常见触发条件是把上下文长度、并发数和输出长度同时放大，却仍按“请求数”估算容量。KV cache 的成本不是一个请求一个固定单位，而是跟 prompt token 数、generated token 数、层数、KV head 数、head dim、dtype、batch 形态和复用策略相关。

第一类事故是长上下文上线后显存水位突然上升。以前每个请求几百 token，后来接入 RAG、工具调用历史、多轮对话，prompt 变成几千或几万 token。prefill 变慢，KV block 占用变大，短请求排队等长请求，TTFT p99 先坏掉。平均延迟可能还看得过去，因为短请求数量多，把长请求问题冲淡了。

第二类事故是取消和超时不释放 KV。用户断开流式连接、网关超时、业务取消、采样失败后，调度器如果只停止输出，不释放请求持有的 block，显存会慢慢漏。这个问题通常不是立刻 OOM，而是运行几小时后 free block 下降，preemption 增多，最后 decode 阶段 OOM。

第三类事故是 prefix 复用条件不严。系统 prompt 相同，开发者就认为可以复用 KV，但实际 chat template 版本改过、tokenizer revision 变了、LoRA adapter 不同、租户权限不同，或者多模态输入里有额外 token。结果是 cache hit 看起来很高，输出质量却变得不稳定。更严重的是跨租户信息泄漏。

第四类事故是把 KV offload 当成无痛扩容。CPU offload 或远端 KV store 可以缓解显存压力，但会引入 PCIe/NVLink/网络传输、序列化、调度等待和一致性检查。尾延迟会变宽，decode 速度可能被 KV 取回拖慢。它适合有明确冷热分层的场景，不适合拿来掩盖容量规划错误。

第五类事故是 allocator 碎片和 block 形态问题。请求长短差异很大、block size 不合适、batch 中 prefill/decode 混杂、抢占频繁，都会让可用显存看起来还有，连续可用或可调度的 KV block 却不够。paged attention 能缓解碎片，但不能让容量约束消失。

第六类事故是监控只看 GPU utilization。GPU 利用率高并不等于服务健康。GPU 可能忙于长 prefill，decode 的小请求却在排队；也可能忙于搬 KV 或等待调度器，实际 token 吞吐不好。KV 相关事故要看 token 级和 block 级指标，不能只看一张 GPU 总览图。

我会把触发条件总结成一句：KV cache 事故通常来自“token 级容量”被“请求级容量”掩盖。只要长 prompt、长输出、取消泄漏、错误复用和显存碎片叠在一起，平均值会骗人，p99 和 OOM 会先说实话。

## Q004. KV cache 的指标应该怎么设计才不会只看平均值？

**回答：**

KV cache 的指标要按请求阶段、token 规模、显存状态和复用语义分开。只看平均延迟或 GPU 利用率，基本看不到问题。

第一组是阶段延迟。至少要拆 TTFT、prefill_ms、queue_ms、decode_tpot、inter_token_latency、total_latency。TTFT 要按 prompt token 数分桶；TPOT 要按 output token 数、batch size、KV 长度分桶。长 prompt 和短 prompt 混在一起算平均值，等于主动丢掉诊断信息。

第二组是 KV 容量指标。包括 allocated_blocks、free_blocks、reserved_blocks、block_size、used_bytes、fragmentation、watermark、allocation_failures、preemption_count、eviction_count、offload_bytes、offload_latency。显存使用率只能说明大概水位，不能说明调度器为什么拒绝新请求。

第三组是生命周期指标。要记录 request_finished 后释放了多少 block，cancel 后释放延迟，超时任务残留 block，prefix block refcount，orphan block 数，worker 重启后清理结果。KV 泄漏最怕没有生命周期指标，因为它会慢慢出现，然后突然把系统推到 OOM。

第四组是复用指标。prefix cache hit ratio 不够，还要看命中的 token 数、节省的 prefill tokens、命中前缀长度分布、命中后 TTFT 降幅、miss 原因、复用条件字段，比如 model revision、tokenizer revision、adapter、tenant、chat template。命中很多但每次只命中几十 token，对长上下文帮助有限。

第五组是正确性和隔离信号。比如跨租户复用次数应该为零，adapter mismatch 拒绝次数，tokenizer mismatch 拒绝次数，prefix hash collision 检查，KV checksum 或调试抽样校验，半写 chunk 被丢弃次数。正确性指标不一定每天报警，但没有它，线上只能靠用户反馈发现错答案。

第六组是资源与调度指标。包括 pending_requests、prefilling_requests、decoding_requests、max_num_batched_tokens、batch_token_utilization、scheduler_iteration_gap、CPU scheduler time、kernel time、GPU memory bandwidth、CUDA graph 命中情况。KV cache 性能问题常常不是 attention 算子本身，而是调度器和 allocator 之间的缝隙。

面试里可以这样收束：KV cache 指标要从“请求平均延迟”下钻到“token、block、阶段和生命周期”。我会按 prompt/output 长度分桶看 TTFT 和 TPOT，按 block 看显存水位和碎片，按状态机看取消/失败是否释放，按复用条件看 prefix hit 是否真的安全和有效。

## Q005. KV cache 的正确性边界和性能边界分别是什么？

**回答：**

KV cache 的正确性边界是：任何被 decode 读取的 KV，都必须对应当前请求允许使用的、完整提交的、版本一致的历史上下文。这里的“版本一致”不是一句话，至少包括模型权重、tokenizer、chat template、adapter、position/RoPE 语义、attention mask、special tokens、dtype/量化策略和权限上下文。

正确性上有几个底线。

第一，prefill 没完成的 KV 不能对 decode 可见。chunked prefill 中间失败，就应该丢弃未提交状态，不能让 decode 读一半。

第二，请求取消、失败、超时后，KV ownership 必须结束。共享 prefix 可以保留，但要有 refcount、租户边界和版本绑定。私有 decode KV 必须释放。

第三，prefix 复用不能只靠字符串相同。必须基于 token 序列和运行时配置相同。多租户场景还要验证权限边界，即使技术上可复用，也未必业务上允许复用。

第四，KV 不应默认成为持久化恢复状态。除非系统设计了完整的外部 KV 协议，否则 worker 崩溃后重新 prefill 更可靠。把显存里的中间态当 checkpoint，会把故障恢复做成隐性赌博。

性能边界则来自显存容量、显存带宽、block 管理、batch 形态和调度开销。KV cache 能减少重复计算，但 decode 阶段会越来越依赖读取历史 KV。上下文越长，KV 内存越大，读写压力越明显。paged cache 降低浪费，不能消除容量；量化 cache 降低内存，可能增加解码开销或影响质量；offload 降低显存压力，通常会牺牲尾延迟。

所以我会把边界说清楚：正确性上，KV 是绑定上下文的中间状态，不能跨版本、跨权限、跨生命周期乱用；性能上，KV 是用显存和带宽换计算量，收益取决于上下文长度、复用率、调度策略和内存管理。它能让 LLM serving 可用，但不是无限上下文和无限并发的许可证。

## Q006. 面试官如果只问一个问题检验你是否理解 continuous batching，可能会问什么？

**回答：**

我会预期他问这个问题：

```text
一个 LLM 服务同时处理短问答、长 RAG prompt 和流式长输出。静态 batching 下 GPU 利用率不低，但短请求 TTFT 很差。你准备改成 continuous batching。请说明：每个 generation step 如何选择 pending、prefill、decode 请求？token budget、KV cache 空间、取消请求、长 prompt 和不同采样参数怎么处理？
```

这道题能看出你是否理解 continuous batching 的核心：它不是简单“把更多请求凑一批”，而是在每个生成步动态维护 batch 里的成员。

传统静态 batching 会把一组请求凑在一起，然后等这一组都处理完再进入下一组。LLM 生成有一个麻烦点：每个请求的 prompt 长度、输出长度、停止条件都不同。短请求早就结束了，长请求还在继续。如果 batch 必须等最慢请求，GPU 会浪费 slot，后面的请求也会排队。

continuous batching 的做法是：每个 generation step 都重新看当前 batch。完成的请求立刻离开；等待队列里的新请求只要有 token budget、KV cache 空间和调度资格，就可以加入。decode 请求通常每步只需要一个 query token，但要读取已有 KV；prefill 请求可能一次消耗很多 prompt tokens；长 prompt 还可能被拆成 chunk，穿插在 decode 中间。

真正难的是策略。调度器要在吞吐、TTFT、TPOT、公平性和显存之间取平衡。一直优先 prefill，长 prompt 可能挤压正在流式输出的请求，用户会感觉输出卡顿；一直优先 decode，新请求 TTFT 会变差。只按请求数限制 batch，也会误判成本，因为一个 20 token 请求和一个 20k token 请求不是同一类任务。

还要处理每个请求自己的状态。一个请求从 pending 到 prefilling，再到 decoding，最后 finished、cancelled 或 failed。取消后要停止输出并释放 KV；流式请求要按 token 返回；不同请求可能有不同 max_new_tokens、stop sequence、temperature、top_p、logits processor。把它们放进同一个 forward pass，不代表它们的业务语义可以混在一起。

面试里可以这样答：continuous batching 是 LLM serving 的动态调度机制。它在每个生成步移除完成请求，加入可调度的新请求，并按 token budget 和 KV cache 空间管理 prefill、decode、chunked prefill 和 cancellation。它提升吞吐，但真正工程难点是保护短请求 TTFT、流式输出 TPOT、公平性和 KV 生命周期。

## Q007. continuous batching 的一句话定义是否容易误导，误导点在哪里？

**回答：**

容易误导。常见定义是：continuous batching 会动态合并请求，提高 GPU 利用率。这句话太像性能宣传，容易让人忽略它背后的状态机和资源边界。

第一个误导是把它当成普通动态 batching。普通动态 batching 往往是在请求入口等一个小窗口，把同时到达的请求拼成一批，然后整批执行。continuous batching 的变化发生在生成循环内部。每个 decode step 都可能有请求完成、请求加入、请求取消、prompt chunk 被调度。它不是入口处凑 batch，而是运行时持续重排 batch。

第二个误导是只讲 GPU utilization。GPU 忙不代表用户体验好。一个长 prefill 可以让 GPU 很忙，同时让一堆短请求等第一个 token；decode batch 很满，也可能因为 CPU 调度器、采样后处理、流式 flush 或 KV block 分配拖住尾延迟。continuous batching 的目标不是让图表好看，而是让吞吐和尾延迟都可控。

第三个误导是忽略 prefill 和 decode 的差异。prefill 一次处理很多 prompt tokens，主要影响 TTFT；decode 每步生成 token，主要影响 TPOT。把它们混在一个 batch 里，要考虑 token budget 和 kernel 形态。长 prompt 如果不切 chunk，会堵住 decode；chunk 太小，又会增加调度和 kernel launch 开销。

第四个误导是认为所有请求可以无条件混批。不同请求有不同采样参数、stop 条件、结构化输出约束、LoRA adapter、模型版本、租户优先级和超时。continuous batching 可以让它们共享一次 forward 的部分计算路径，但不能让业务语义丢掉。请求级状态必须保留。

第五个误导是忽略 KV cache。continuous batching 能动态换人，前提是 KV cache 能动态分配、索引、增长和释放。没有可靠的 paged KV、block table、allocator 和 release 机制，调度器很快会被显存碎片、OOM 和取消泄漏拖垮。

更准确的一句话是：continuous batching 是在 LLM 自回归生成循环中，按 token budget、KV cache 和请求状态动态重组 batch 的调度方式；它能提高吞吐，但要靠正确的状态机、内存管理和公平策略避免尾延迟失控。

## Q008. continuous batching 最常见的生产事故触发条件是什么？

**回答：**

最常见触发条件是把 continuous batching 当成“开关型优化”，上线后没有按流量形态重新调 token budget、prefill 策略和队列策略。它确实能提高吞吐，但也会把调度错误放大得更快。

第一类事故是长 prefill 队头阻塞。RAG 请求、长聊天历史、超长系统 prompt 进入服务后，如果 prefill 不切 chunk，某些 step 会被大 prompt 占满。decode 请求虽然已经在流式输出，但下一 token 等不到 GPU，TPOT 抖动。用户看到的不是“整体慢”，而是输出一阵一阵卡。

第二类事故是只按 max batch size，不按 max batched tokens。一个 batch 里 32 个短请求和 32 个超长 prompt 的成本完全不同。调度器如果只看请求数，就会把 token 预算打爆，造成 TTFT p99、OOM、preemption 或大量抢占。

第三类事故是新请求加入过于激进。为了提高吞吐，调度器不断塞 pending 请求，导致已经在 decode 的流式请求被挤压。长输出用户会感到 token 间隔变大。服务端看平均吞吐上升，前端看打字机效果变差。

第四类事故是取消路径不完整。continuous batching 中请求可能在 pending、prefilling、decoding 任意阶段取消。每个阶段都要有释放 KV、移出队列、停止流式输出、通知调用方、清理采样状态的路径。只处理 decoding 取消，不处理 prefilling 取消，显存泄漏会很隐蔽。

第五类事故是不同采样和约束混批后 CPU 成为瓶颈。每个请求有自己的 temperature、top_p、stop words、JSON schema、logprobs、工具调用约束。GPU forward 结束后，CPU 后处理如果串行或锁竞争严重，scheduler iteration gap 会变大。GPU 利用率可能还行，但 token 出不来。

第六类事故是公平性缺失。大租户持续提交长请求，小租户短请求被排在后面；或者低优先级批任务把在线请求的 KV 空间占满。continuous batching 不自带公平性。没有租户配额、优先级、deadline、最大等待时间和 admission control，它会优先服务“更容易把资源填满”的流量。

第七类事故是多副本下 locality 被打散。prefix cache、KV cache 和 warmed model 都有实例局部性。负载均衡器随机路由后，单机压测里的 cache hit 上线消失，TTFT 回退。continuous batching 本身不会解决跨副本 routing。

我会总结成一句：continuous batching 事故通常不是算法概念错，而是 token 预算、KV 空间、长 prompt、取消释放和公平性没有一起设计。它让 GPU 更忙，也会让错误调度更快进入 p99。

## Q009. continuous batching 的指标应该怎么设计才不会只看平均值？

**回答：**

continuous batching 的指标必须围绕 generation loop 设计。入口 QPS、平均延迟和 GPU utilization 只能看大概，不能解释哪个 step 坏了。

第一组是请求状态指标。要分别看 pending、prefilling、decoding、streaming、finished、cancelled、failed 的数量和停留时间。pending 时间主要影响排队；prefilling 时间影响 TTFT；decoding 时间和每步间隔影响流式体验。所有状态揉成一个 latency，排障会很慢。

第二组是 token budget 指标。包括每个 step 的 batched_prompt_tokens、batched_decode_tokens、max_num_batched_tokens、budget_utilization、chunked_prefill_tokens、uncached_prompt_tokens。continuous batching 的资源单位是 token，不是请求。没有 token 视角，就看不到长 prompt 的真实成本。

第三组是尾延迟指标。TTFT、TPOT、inter-token latency、total latency 都要看 p50/p90/p95/p99，并按 prompt length、output length、是否 streaming、是否 prefix hit、是否 chunked prefill、租户、模型版本分桶。平均 TTFT 降了，但短请求 p99 升了，这就是一次不合格的优化。

第四组是 GPU 和调度器间隙。要看 GPU kernel time、scheduler iteration time、CPU sampling time、logits processor time、stream flush time、iteration gap、CUDA graph hit、batch shape 分布。很多系统不是 GPU 算不动，而是每步之间的 CPU 和同步开销太大。

第五组是 KV cache 指标。free blocks、allocated blocks、allocation failures、preemption、eviction、offload、refcount、cancel release latency 都要跟 batch 指标关联。continuous batching 把请求不断换进换出，KV 生命周期是主线。

第六组是公平性指标。按租户、优先级、请求类型统计等待时间、token served、drop/reject、deadline miss、max queue age。吞吐优化不能靠饿死某类请求换来。线上服务尤其要看小请求有没有被大请求拖住。

第七组是错误和回退指标。OOM、max length reject、request cancelled、schema processor error、stop sequence error、rank timeout、worker restart、batch rebuild、fallback kernel 都要记录。continuous batching 一出问题，通常会回退到更慢路径，回退次数本身就是信号。

面试里可以这样答：continuous batching 的监控要围绕“每个生成步”看。我要知道本 step 放了多少 prefill token、多少 decode token、用了多少 KV block、谁被 admitted、谁完成、谁取消、GPU 算了多久、CPU 调度花了多久，以及这些变化如何影响 TTFT 和 TPOT 的尾部。

## Q010. continuous batching 的正确性边界和性能边界分别是什么？

**回答：**

continuous batching 的正确性边界是：虽然多个请求共享同一个 batch 执行，但每个请求的语义状态必须独立。batch 是执行组织，不是业务状态合并。

正确性上有几个底线。

第一，每个请求的 prompt tokens、position、attention mask、KV ownership、sampling params、stop condition、max_new_tokens 都要独立保存。混批不能让一个请求的 stop sequence 影响另一个请求。

第二，prefill 到 decode 的转换必须有明确状态。prompt 没处理完，不能开始 decode；chunked prefill 没提交完成，不能读半成品 KV；请求取消后，不能再被 scheduler 放回 batch。

第三，输出归属必须稳定。流式输出时，每个 token 必须发回对应 request_id。batch 内顺序变化、请求完成、请求加入，都不能打乱客户端看到的流。

第四，失败隔离要清楚。某个请求采样失败、schema 约束失败、长度超限，不应该让整个 batch 失败。反过来，模型 runner、GPU kernel、rank 通信这类 batch 级失败，也要能把受影响请求标成可重试或失败，不能静默丢状态。

性能边界则是吞吐、TTFT、TPOT、显存和调度开销之间的平衡。continuous batching 通常提高吞吐，因为完成的请求马上离开，新请求马上补上。但它也可能增加 per-step 调度、KV bookkeeping、CPU 采样和 batch shape 管理成本。短请求延迟、长请求连续输出、公平性和 GPU 利用率之间会互相拉扯。

它的性能还受 kernel 支持影响。decode-only batch 可以走更快路径；prefill 和 decode 混合时可能走变长路径；CUDA graph 需要较稳定的 shape；paged attention 需要 block table；tensor parallel 下还要跨 rank 同步请求状态。一个看似简单的调度策略，到了多 GPU 和多副本环境就会变成分布式运行时问题。

所以我会说：correctness 上，continuous batching 只改变“怎么一起算”，不能改变“每个请求是什么”；performance 上，它用更复杂的调度和 KV 管理换更高 GPU 利用率，收益取决于流量形态、token 预算、prefill/decode 配比和实现开销。

## Q011. 面试官如果只问一个问题检验你是否理解 object store，可能会问什么？

**回答：**

我会预期他问这个问题：

```text
你的 workflow 系统把大结果写到 S3 或 MinIO，事件日志里只保存 result_ref。请说明：什么时候认为对象已经成功发布？引用里要不要保存 version、size、checksum、content type？如果对象写成功但事件写失败，或者事件写成功但对象读不到，系统如何恢复？你为什么不能把对象存储当成本地文件系统用？
```

这道题很适合检验 object store 的理解。它不是问“对象存储是什么”，而是问你是否知道对象存储的语义边界会影响控制面正确性。

对象存储的基本模型是 bucket、object key、object data、metadata，有版本化时还会有 version id。key 看起来像路径，比如 `workflows/wf-1/result.json`，但这不等于 POSIX 文件系统路径。斜杠通常只是 key 里的字符或前缀组织方式，不代表有真实目录、inode、rename、append、seek、file lock 和 fsync 语义。

对 workflow runtime 来说，大结果放 object store 的合理做法是：先把对象本体写入 result store，确认 size、checksum、version 或 ETag 等元数据，再把短引用写进事件日志或 metadata。这样日志保持小、可 replay；大对象有自己的生命周期、权限和存储成本模型。

这里的发布顺序很关键。先写事件再写对象，会产生 broken ref：控制面已经宣布 step 成功，下游却读不到对象。这是 correctness 错误。先写对象再写事件，事件写入失败会留下 orphan object。orphan object 可以用生命周期或扫描清理；broken ref 会让状态机撒谎。所以工程上通常宁愿接受 orphan，也不要发布一个不存在的成功引用。

引用也不能太薄。只保存 `s3://bucket/key` 在简单 demo 里够用，生产里往往还要保存 size、checksum、content type、version id、created_at、expires_at、encryption/key 信息和写入 attempt。否则对象被覆盖、生命周期过期、KMS 权限变化、跨区域复制滞后、S3-compatible 后端行为不同，控制面都很难判断问题在哪。

面试里我会这样答：object store 适合保存不可变或少变的大对象，控制面保存引用。理解它的关键不是会调用 PutObject，而是能解释对象发布点、引用完整性、orphan/broken ref 恢复、版本和 checksum，以及它为什么不是一个可以随便 append、rename、fsync 的远程文件系统。

## Q012. object store 的一句话定义是否容易误导，误导点在哪里？

**回答：**

容易误导。最常见的一句话是：对象存储就是用 key 存文件的远程存储。这句话会把人带向“远程文件系统”的错觉。

第一个误导是路径语义。对象 key 可以包含 `/`，控制台也可能把 prefix 显示成文件夹，但对象存储的核心是 flat key space 加 metadata。`a/b/c.json` 是一个 key，不意味着可以对 `a/b` 做真实目录 rename，也不意味着父目录存在。

第二个误导是写入语义。文件系统可以 open、write、append、truncate、seek，最后 fsync。对象存储通常是 PUT 一个对象，或者 multipart 上传多个 part 后 complete 成一个对象。你不能假设可以像本地文件那样原地改中间几个字节，也不能假设 append log 有相同语义。

第三个误导是 rename。很多本地系统用 `write temp -> fsync -> rename` 做原子发布。S3 general purpose bucket 没有通用 POSIX rename；常见做法是 copy + delete，语义和成本都不同。把 rename 当成原子操作，会直接影响 checkpoint、manifest 和日志发布设计。

第四个误导是一致性和事务边界。Amazon S3 对单对象 PUT/DELETE 和相关读操作提供强一致性，但这不是跨对象事务。你同时写对象、写 metadata、写数据库、写事件日志，中间任何一步失败，都要自己处理补偿。对象存储不会替你保证 workflow 状态机原子提交。

第五个误导是性能模型。对象存储可以有很高吞吐，但单个小对象 GET/PUT 不是内存缓存，也不是本地 SSD。它有网络延迟、TLS、DNS、连接池、请求计费、KMS、重试、503 Slow Down、跨 Region 成本。把它放到每个小状态转移路径里，p99 和成本都会难看。

第六个误导是认为 S3-compatible 就等于 S3 完全语义。MinIO、Ceph、云厂商兼容接口、网关型对象存储可能在一致性、ETag、multipart、versioning、lifecycle、SSE、LIST 行为上有差异。代码应该依赖自己验证过的最小语义，而不是只看 API 名字相同。

更准确的一句话是：object store 是以 bucket/key 标识对象、以整对象或 multipart 发布为核心的存储系统；它适合大对象、不可变结果、分层生命周期和高吞吐访问，但不提供普通文件系统的目录、rename、append 和跨对象事务语义。

## Q013. object store 最常见的生产事故触发条件是什么？

**回答：**

最常见触发条件是把对象存储放进了不适合它的控制面路径，或者把对象引用的生命周期管理得太薄。对象存储本身很成熟，但它不替应用设计状态机。

第一类事故是 broken ref。事件日志或数据库已经写了 `result_ref`，下游读取时对象不存在。原因可能是发布顺序反了、PutObject 超时后结果未知、multipart complete 没成功、对象被生命周期删掉、版本覆盖、权限变了，或者写到了另一个 bucket/region。对 workflow 来说，这比普通读取失败更严重，因为控制面已经记录“结果成功”。

第二类事故是 orphan object。对象写成功后，metadata 或事件写失败，留下无人引用的对象。它不一定影响 correctness，但会带来成本、合规和清理问题。没有 attempt id、created_at、workflow id、object tags 和 sweeper，清理时很容易误删仍在用的对象。

第三类事故是把 object store 当日志。每个状态事件都写对象、频繁 append、用 LIST 扫描当索引、用 copy+delete 模拟事务，都会让系统变慢且难以恢复。对象存储适合数据面，不适合承载高频细粒度控制面状态。

第四类事故是热 key 或热 prefix。大量 worker 同时读写少数 key、少数 prefix，或者把所有结果都放到一个顺序命名空间里，可能遇到请求拥塞、503 Slow Down、尾延迟上升。S3 会自动扩展，但扩展不是瞬时完成；高请求率场景要分散前缀、并行连接、重试和限流。

第五类事故是小对象过多。每个 step 都产生几十字节小对象，GET/PUT 请求数、LIST 成本、metadata 管理、压缩归档、生命周期扫描都会变重。小结果 inline，大结果 object store，这个折中很重要。

第六类事故是 multipart 协议没收口。part 上传成功不等于对象发布成功；CompleteMultipartUpload 才是发布点。complete 前就写 result_ref，会产生不可读对象；失败后不 abort，会留下 incomplete upload；part manifest 丢 ETag 或 checksum，complete 和校验都会出问题。

第七类事故是权限和加密边界。对象写入者、读取者、KMS key、bucket policy、预签名 URL、跨账号复制、对象所有权设置如果没有对齐，表现可能是“偶发 403”。尤其是控制面能写、worker 能不能读，下游租户能不能 dereference，要提前定义。

所以我会总结：object store 事故多半不是“存储坏了”，而是应用把对象发布、引用完整性、生命周期、权限和请求模型当成了透明细节。对象存储越可靠，越容易让人忘记这些边界需要自己设计。

## Q014. object store 的指标应该怎么设计才不会只看平均值？

**回答：**

object store 指标要把操作类型、对象大小、key 分布、错误类型和引用状态分开。只看平均 GET 延迟或总请求量，会把最关键的问题盖住。

第一组是按操作拆的延迟和错误：PUT、GET、HEAD、LIST、COPY、DELETE、CreateMultipartUpload、UploadPart、CompleteMultipartUpload、AbortMultipartUpload。每类都要看 p50/p90/p95/p99、超时、重试次数、4xx、5xx、503 Slow Down。PUT 慢和 LIST 慢不是一个问题。

第二组是对象大小分布。小对象、中等对象、大对象、multipart 对象要分桶。小对象关注请求开销和 p99；大对象关注吞吐、首字节时间、range GET、part 并发和重试成本。平均对象大小没有诊断价值。

第三组是吞吐和并发。包括 upload_bytes_per_sec、download_bytes_per_sec、inflight_requests、connection_pool_usage、DNS latency、TLS handshake、range request 数、multipart 并发、bytes in flight。对象存储性能通常靠横向并行，不是单连接硬扛。

第四组是 key/prefix 热点。按 bucket、prefix、tenant、workflow、object type 看请求速率、错误率和尾延迟。一个全局平均可能正常，某个 prefix 已经在 503 或 KMS throttling。

第五组是引用健康。要统计 result_ref 写入数、dereference 成功率、broken ref、orphan object、expired ref、missing checksum、checksum mismatch、version mismatch、permission denied、lifecycle 删除命中。workflow 系统最关心的是“引用是否可信”，不是 S3 控制台里对象总数。

第六组是 multipart 生命周期。包括 active uploads、incomplete uploads、abort success/failure、part retry、part overwrite、complete latency、complete failure、part manifest 缺字段。multipart 问题如果只看最终 PUT 成功率，很容易漏掉成本和清理问题。

第七组是成本和治理。请求费用、存储量、版本数量、删除标记、归档恢复请求、跨区域流量、KMS 调用量、预签名 URL 使用量都要看。对象存储事故不一定只表现为延迟，也可能先表现为账单异常。

面试里可以这样答：object store 的指标不能只看“平均延迟”。我要按操作、大小、prefix、租户和对象类型看 p99、错误、重试、吞吐、并发和成本；对 workflow 还要单独看 result_ref 健康，包括 broken ref、orphan object、checksum mismatch、version mismatch 和 lifecycle 误删。

## Q015. object store 的正确性边界和性能边界分别是什么？

**回答：**

object store 的正确性边界首先是单对象边界。一个对象通过 key 和可选 version id 标识；PUT 或 multipart complete 后对象可读；metadata 描述对象；checksum 或其他完整性字段用于验证内容。Amazon S3 对单对象写后读提供强一致性，但这不等于你的整个业务事务强一致。

对 workflow 系统来说，正确性边界要再往上收一层：result_ref 只有在对象已经写入并通过必要校验后才能发布。事件日志里写了成功引用，就意味着下游可以按这个引用读取结果。如果对象还没 complete、checksum 不匹配、权限不可读、version 不确定，就不能发布成功事件。

还有几个底线。

第一，对象 key 最好不可变。成功的结果对象不要被后续 attempt 覆盖。可以用内容哈希、attempt id、version id 或条件写保护。

第二，引用要足够完整。至少要有 uri、size、checksum 或可替代校验、content type、created_at；需要强恢复时还要 version id、encryption、expires_at。

第三，对象和元数据不是同一个事务。对象写成功但事件失败，留下 orphan；事件成功但对象失败，产生 broken ref。系统必须明确选择顺序和补偿策略。

第四，LIST 不是主索引。即使 S3 的一致性比过去强，业务也不应该把 LIST 当成 workflow 状态机的唯一来源。控制面索引应该在数据库、事件日志或 metadata view 里。

性能边界则是高吞吐、网络型、请求计费型存储的边界。对象存储可以通过多连接、range GET、multipart、多个 prefix、同 Region 部署获得很高吞吐；但它不适合低延迟高频小状态更新，不适合每个调度决策都同步 GET，也不适合拿 LIST 做实时查询。

高请求率也不是瞬时无限。S3 会扩展到很高请求速率，但扩展需要时间；在新负载模式下可能暂时返回 503 Slow Down。KMS、客户端连接池、DNS、带宽、CPU 校验、SDK retry 都可能成为瓶颈。

所以我会这样说：object store 的正确性边界是对象发布和引用可信，性能边界是大对象吞吐和并行访问。它可以很好地做 result store，但不能替代事件日志、数据库索引或本地低延迟文件系统。

## Q016. 面试官如果只问一个问题检验你是否理解 S3 ETag，可能会问什么？

**回答：**

我会预期他问这个问题：

```text
你在系统里用 S3 ETag 做结果去重和完整性校验。后来大文件改成 multipart upload，bucket 开了 SSE-KMS，一部分对象的 ETag 变成带短横线的形式，本地 MD5 对不上。这个设计哪里错了？你会怎样重新设计 checksum、条件写和 result reference？
```

这道题非常典型。很多人把 ETag 当成“对象内容的 MD5”，在小文件单次 PUT、未使用某些加密方式时可能刚好成立；一到 multipart、SSE-KMS、copy、directory bucket 或 S3-compatible 后端，这个假设就会破。

S3 API 对 ETag 的说法更谨慎：它是 object 的 entity tag，反映对象内容变化，不反映 metadata 变化；它可能是对象数据的 MD5，也可能不是。是否是 MD5 取决于对象如何创建、是否 multipart、是否使用某些加密方式。multipart 上传生成的对象，ETag 不是完整对象的 MD5。

所以如果你的 result reference 里只有 ETag，并且把它解释成 sha/md5 checksum，就会出问题。比如本地算 MD5 校验大对象，发现跟 S3 ETag 不一致，你可能误判对象损坏；或者你用 ETag 做内容寻址，两个相同内容因为上传方式不同得到不同 ETag，去重失效；反过来，把 ETag 当强完整性校验，也可能漏掉你真正需要的端到端 checksum。

正确做法是分清用途。

ETag 可以用于条件请求和对象变化识别，比如 `If-Match`、`If-None-Match`、缓存验证、避免覆盖。但如果你要证明“读回来的 bytes 等于我写入的 bytes”，应该使用明确的 checksum 字段，比如 SHA-256、CRC32C、CRC64NVME，或者在业务 metadata/result reference 里保存自己的 sha256。multipart 场景还要保存 part manifest 和完整对象 checksum。

面试里我会这样答：ETag 不是稳定的业务 checksum 抽象。它在某些简单 PUT 场景可能等于 MD5，但系统设计不能依赖这个偶然性质。我要把 ETag 用在 HTTP/S3 条件语义里，把数据完整性放到明确的 checksum 字段里，把 result reference 设计成 `uri + version + size + checksum + content_type`，而不是 `uri + etag` 就结束。

## Q017. S3 ETag 的一句话定义是否容易误导，误导点在哪里？

**回答：**

容易误导。最常见的一句话是：S3 ETag 是文件的 MD5。这句话在很多老经验里流传很广，但作为工程定义是危险的。

第一个误导是把“有时等于 MD5”说成“总是 MD5”。普通单 part PUT、未使用会改变 ETag 语义的加密方式时，ETag 可能就是对象数据的 MD5。但 multipart 上传、part copy、SSE-KMS、SSE-C 等场景下，ETag 不是完整对象 MD5。大于某个大小的对象通过控制台上传时，也可能自动走 multipart。

第二个误导是忽略 metadata。ETag 反映对象内容变化，不反映 metadata 变化。你改 Content-Type、custom metadata、tagging、lifecycle 相关字段，ETag 未必按你业务期待变化。如果你用 ETag 表示“整个 result reference 的语义版本”，它不够。

第三个误导是把 ETag 当跨后端一致语义。S3-compatible 系统可能模拟 S3 API，但 ETag 生成规则、加密、multipart、gateway 后端都可能不同。用 ETag 做跨 AWS S3、MinIO、Ceph 的内容哈希，会把兼容接口误当成兼容实现。

第四个误导是把 ETag 当安全哈希。就算它在简单场景是 MD5，MD5 也不适合安全完整性或防篡改语义。对象完整性应该使用明确的 checksum 算法和端到端校验，安全边界还要看签名、KMS、权限、审计和不可变策略。

第五个误导是忽略 multipart 的 part ETag。CompleteMultipartUpload 需要每个 part 的 ETag 来组装对象。这个 ETag 是协议字段，不等于完整对象 checksum。把 part ETag、最终 object ETag、业务 checksum 混在一个字段里，迟早会出事故。

更准确的一句话是：S3 ETag 是对象的 entity tag，可用于条件请求和变化识别；它在有限场景下可能等于对象 MD5，但 multipart、加密和不同后端会破坏这个假设，业务完整性应使用明确 checksum。

## Q018. S3 ETag 最常见的生产事故触发条件是什么？

**回答：**

最常见触发条件是系统从小对象单次 PUT 发展到大对象 multipart 或 SSE-KMS，却没有改掉“ETag 等于 MD5”的旧逻辑。代码没变，上传方式变了，语义已经变了。

第一类事故是校验误报。客户端上传大文件后本地计算 MD5，拿它和 S3 ETag 比，发现不一致，就认为对象损坏。实际原因只是 multipart ETag 不是完整对象 MD5。误报会触发重复上传、任务失败、人工排障，浪费很大。

第二类事故是漏校验。团队以为保存了 ETag 就等于保存了 checksum，读回对象时没有计算 SHA-256 或检查 S3 checksum 字段。后来传输、客户端拼接、解压、加密层或兼容后端出现问题，系统没有端到端证据证明 bytes 是否一致。

第三类事故是去重失效。相同内容如果一次单 part 上传，一次 multipart 上传，ETag 可能不同；同样内容在不同 part size 下 multipart，ETag 也可能不同。用 ETag 做内容地址或去重 key，会让重复对象保留下来。对象越大，成本越明显。

第四类事故是条件写误用。`If-Match` 和 `If-None-Match` 可以保护对象覆盖，但它们保护的是 S3 的 ETag 条件语义，不是业务 checksum。有人把 `If-Match` 当成“内容一定等于我的 SHA-256”，这是两层语义混淆。

第五类事故是 multipart manifest 丢失。UploadPart 返回 part ETag，CompleteMultipartUpload 需要按 part number 提交 ETag。系统如果没有持久化 part number、size、ETag、checksum、attempt，worker 崩溃后就不知道哪些 part 可用，可能重传、覆盖、complete 失败，或者错误地发布 result_ref。

第六类事故是引号和格式处理。S3 API 返回的 ETag 往往带双引号；multipart ETag 还可能带 `-N` 后缀。代码如果随手 trim、大小写处理、把它塞进 JSON 再反序列化，容易出现条件请求对不上。它是协议字段，应该按原样保存一个规范化版本，业务 checksum 另存。

第七类事故是跨后端迁移。AWS S3、MinIO、Ceph、网关型对象存储在 ETag、加密、multipart 和 checksum 支持上可能不同。迁移时如果测试只覆盖小文件，生产大对象才会暴露差异。

所以我会说：ETag 事故的根源是把协议层 tag 当成内容校验哈希。小文件时代它“看起来能用”，大文件、加密、multipart 和兼容后端会把这个假设拆掉。

## Q019. S3 ETag 的指标应该怎么设计才不会只看平均值？

**回答：**

S3 ETag 的指标不应该只围绕延迟，而要围绕“它被用来做什么”设计。如果 ETag 被用于条件请求、缓存验证、去重或校验，指标就要能证明这些用法没有混淆。

第一组是对象上传形态。统计 single part、multipart、copy、SSE-S3、SSE-KMS、SSE-C、directory bucket、S3-compatible 后端的对象数量和比例。只要 multipart 或 KMS 比例上升，就不能继续默认 ETag 是 MD5。

第二组是 checksum 覆盖率。看 result reference 里有多少对象保存了 SHA-256、CRC32C、CRC64NVME 或完整对象 checksum；多少对象只有 ETag；多少对象 size 缺失；多少对象 content type 缺失。覆盖率比平均上传延迟更能说明完整性设计是否靠谱。

第三组是校验结果。包括 checksum_mismatch、etag_md5_mismatch、read_verify_failed、missing_checksum、part_checksum_mismatch、post_upload_head_mismatch。要把“ETag 不等于 MD5 的预期不一致”和“真正内容损坏”分开，否则报警会吵死。

第四组是条件请求结果。统计 If-Match、If-None-Match、304 Not Modified、412 Precondition Failed、overwrite_prevented、conditional_put_failed。ETag 在这里是有价值的，但要观察它是否真的在保护覆盖和缓存语义。

第五组是 multipart manifest 健康。包括 active multipart uploads、part count、part ETag missing、part number gap、complete failure、abort failure、incomplete upload age、same part number overwrite、part checksum missing。最终对象 ETag 只是结果之一，part 级状态同样重要。

第六组是后端差异。按 AWS S3、MinIO、本地 mock、测试环境分别看 ETag 格式、checksum 支持、multipart 行为和条件请求错误码。S3-compatible 的坑往往在这里暴露。

第七组是成本和性能副作用。计算 checksum 会消耗 CPU；HEAD/GET 校验会增加请求；multipart 额外 checksum 会影响上传路径。要看 checksum_compute_ms、verify_ms、HEAD p99、upload throughput、CPU usage。完整性不是免费，但也不能因为成本就假装 ETag 是安全哈希。

面试里可以这样答：S3 ETag 指标要回答两个问题：我们有没有把 ETag 用错，以及真正的 checksum 是否覆盖了关键对象。我要按上传形态和加密方式统计对象，区分 ETag 条件请求成功率、checksum 覆盖率、真实 mismatch、multipart manifest 健康和不同 S3 后端差异。

## Q020. S3 ETag 的正确性边界和性能边界分别是什么？

**回答：**

S3 ETag 的正确性边界是：它可以作为 S3 对象的 entity tag，用于条件请求、缓存验证和内容变化识别；但它不是跨所有场景都成立的完整对象 MD5，也不是业务层强 checksum。

正确性上可以这样划线。

第一，在普通单 part、特定加密条件下，ETag 可能等于对象数据 MD5。这个事实可以作为兼容知识，但不能作为系统不变量。

第二，multipart 上传、part copy、SSE-KMS、SSE-C 等场景下，ETag 不能当完整对象 MD5。大文件系统、result store、备份系统只要有 multipart，就应该保存独立 checksum。

第三，ETag 不反映 metadata。业务如果关心 content type、schema version、model version、tenant、expires_at、encryption key，这些要放进 metadata 或 result reference 的版本字段，不能指望 ETag 表达。

第四，条件请求使用 ETag 是合理的。`If-Match` 可以避免在对象已变化时覆盖；`If-None-Match` 可以做“对象不存在才写”或缓存验证。但这保护的是对象版本条件，不是业务数据结构的完整一致性。

第五，跨后端时要以实测语义为准。S3-compatible 不等于所有 ETag 和 checksum 细节都一致。代码应该把 ETag 抽象为 opaque tag，而不是解析出业务含义。

性能边界也要讲清楚。ETag 很便宜，HEAD 或 LIST 里通常能拿到，用它做缓存验证和条件请求成本较低。真正的端到端 checksum 更贵：上传前要计算，multipart 要维护 part checksum 或 full object checksum，读回验证要消耗 CPU 和可能的额外 GET/HEAD。大对象上这笔成本不小。

但成本不能成为误用 ETag 的理由。更稳的设计是按对象重要性分层：小的非关键临时对象可以只做基本校验；workflow result、checkpoint、审计对象、跨租户可见对象必须保存 size 和强 checksum；大对象可以在上传流中边传边算，避免读两遍；multipart 记录 part manifest，complete 后再确认完整对象元数据。

所以我会这样答：ETag 的正确性边界在 S3 协议层，它适合条件请求和变化识别；业务完整性要靠明确 checksum。性能上，ETag 便宜，checksum 更贵，但关键数据不能用一个便宜但语义不稳定的字段冒充端到端校验。