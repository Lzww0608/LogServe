# 18. LLM serving、模型缓存、KV cache、batching 与 GPU 调度

这一章讨论的是 LLM 在线推理系统里最容易被混在一起的几个概念：模型加载、模型权重缓存、KV cache、prefill、decode、batching、GPU 调度和端到端延迟。它们都和“让模型更快、更稳、更便宜地服务请求”有关，但边界并不一样。

可以先记住一个主线：LLM serving 不是一次普通 RPC 调用，而是一条跨越网络、网关、tokenizer、调度器、GPU 计算、KV 内存管理、流式输出和日志观测的流水线。优化时不能只盯着一个平均延迟数字，要把 first-token latency、per-token latency、total latency、吞吐、GPU 利用率和成本放在一起看。

下面的回答参考了 vLLM、TensorRT-LLM、Hugging Face Transformers/TGI 文档以及 PagedAttention 论文。面试时不需要背文档原文，但要能把系统边界讲清楚：哪些延迟来自排队，哪些来自 prefill，哪些来自 decode，哪些来自 checkpoint 加载，哪些来自 KV cache 容量和 GPU 调度。

## Q001. LLM serving 的主要延迟组成有哪些？

**回答：**

LLM serving 的延迟不能只理解成“模型 forward 一次的时间”。对一个真实在线请求来说，端到端延迟通常由这些部分组成：

```text
客户端/网络延迟
  -> API 网关、鉴权、限流、路由
  -> 排队和准入控制
  -> prompt 预处理、chat template、tokenization
  -> 调度器等待 GPU slot / batch slot / KV cache block
  -> prefill 阶段
  -> 第一个 token 的采样和返回
  -> decode 阶段逐 token 生成
  -> detokenization、streaming flush、后处理
  -> 日志、计费、trace、指标上报
```

其中最重要的几类延迟是：

1. **排队延迟**

   请求到达服务端后，不一定马上进入 GPU。调度器可能要等 batch 窗口、GPU 空闲、KV cache 空间、并发额度或者优先级队列。高并发时，排队延迟往往比单次模型计算更影响 p95/p99。

2. **tokenization 和 prompt 处理延迟**

   包括 JSON 解析、chat template 拼接、工具调用 schema 处理、tokenizer 编码、输入长度检查等。对短 prompt 来说，这部分可能看起来不起眼；对高 QPS 小请求，它会变成 CPU 热点。

3. **prefill 延迟**

   prefill 是把 prompt tokens 一次性送入模型，计算每一层的中间状态，并建立初始 KV cache。它通常和 prompt 长度强相关。长上下文、RAG 拼接大段文档、多轮对话历史很长时，first-token latency 主要被 prefill 拉高。

4. **decode 延迟**

   decode 是自回归生成阶段。模型每次生成一个或少量 token，都要读取已有 KV cache，再计算下一个 token。它的关键指标不是一次完整 forward 的延迟，而是每个输出 token 的时间，也就是 TPOT、ITL 这类指标。

5. **采样和后处理延迟**

   包括 logits processor、temperature、top-p、top-k、重复惩罚、结构化输出约束、stop word 检查、logprobs、detokenization、SSE/WebSocket flush 等。复杂采样策略和 JSON schema 约束会增加 CPU 或 GPU 外的额外开销。

6. **冷启动和缓存未命中延迟**

   如果模型进程刚启动，可能要加载 checkpoint、初始化 CUDA context、分配 KV cache pool、做 warmup。即使进程已经热起来，如果 prefix cache、模型权重缓存、本地文件缓存未命中，也可能出现明显抖动。

7. **通信和分布式执行延迟**

   多 GPU tensor parallel、pipeline parallel、跨节点推理会引入 NCCL 通信、跨卡同步、跨节点网络延迟。模型越大、并行切分越复杂，通信越可能影响尾延迟。

面试里要注意：用户体感最明显的通常不是 total latency，而是 **first-token latency**。流式接口下，只要第一个 token 很快返回，用户会觉得系统“开始响应了”；但如果输出很长，total latency 和每 token 速度仍然很重要。

LLM serving 常见延迟指标可以这样拆：

```text
TTFT / first-token latency:
  从请求进入服务端到第一个 token 返回。
  主要受排队、tokenization、prefill、prefix cache 命中、冷启动影响。

TPOT / ITL:
  生成阶段每个 token 的平均或分位延迟。
  主要受 decode、KV cache 读取、batching、GPU 调度影响。

E2E latency / total latency:
  从请求开始到完整输出结束。
  主要受 TTFT、输出 token 数、TPOT、流式/非流式后处理影响。
```

如果只能给一个工程化回答，我会说：LLM serving 延迟由“排队 + 输入处理 + prefill + decode + 输出处理 + 冷启动/缓存/通信抖动”组成。优化时先按 TTFT、TPOT、total latency 分开观测，再决定是优化调度、KV cache、checkpoint、batching 还是业务侧 prompt。

面试里可以这样答：

```text
LLM serving 的延迟不是单次模型计算时间，而是一条流水线。主要包括网络和网关、鉴权限流、排队调度、tokenization、prefill、first token 采样、decode 逐 token 生成、detokenization 和流式返回。长 prompt 通常拉高 prefill 和 TTFT，长输出通常拉高 total latency，decode 阶段更关注每 token 延迟。高并发下还要看 batch 排队、KV cache 空间、GPU 调度和多卡通信带来的尾延迟。
```

## Q002. 模型冷启动通常包括哪些阶段？

**回答：**

模型冷启动可以分成几层：进程冷、模型冷、GPU 冷、缓存冷。它不只是“把模型文件读进来”。

比较完整的冷启动链路如下：

```text
容器/进程启动
  -> Python runtime、依赖库、CUDA/NCCL/Triton/torch 初始化
  -> 读取模型配置、tokenizer、generation config
  -> 定位或下载模型文件
  -> 加载 checkpoint shards
  -> 反序列化或 safetensors 映射
  -> dtype 转换、量化权重加载或反量化准备
  -> 张量并行/流水并行切分和 weight mapping
  -> CPU 内存到 GPU 显存搬运
  -> 分配 KV cache pool、workspace、CUDA graph buffer
  -> 初始化 CUDA context、通信组、kernel autotune
  -> warmup 请求
  -> 健康检查通过，对外接流量
```

这里有几个阶段特别容易成为瓶颈。

第一，**模型文件获取**。如果模型不在本地 NVMe，而是从对象存储、网络文件系统或模型仓库拉取，冷启动会被网络带宽、并发下载、认证、重试拖慢。

第二，**checkpoint 读取和解析**。大模型通常由多个 shard 组成。服务端要读取配置、解析权重文件、把不同 shard 映射到不同 GPU 或不同并行 rank。TensorRT-LLM 的 checkpoint loading 文档里也把 checkpoint loader、config loader、weight loader、weight mapper 作为独立组件来描述，这说明真实加载过程本身就是一套子系统。

第三，**CPU 到 GPU 的显存搬运**。权重可能先进入 CPU 内存，再通过 PCIe 或 NVLink 搬到 GPU。模型越大，搬运越慢；如果多个副本同时启动，还会争抢磁盘、内存带宽和 GPU 总线。

第四，**运行时初始化**。CUDA context、NCCL 通信组、Triton kernel、CUDA graph、engine runtime、workspace buffer 都可能在第一次请求或 warmup 时才真正初始化。很多“第一次请求特别慢”的问题，本质不是模型数学计算慢，而是 lazy initialization。

第五，**KV cache 和调度资源初始化**。推理服务通常要预留 KV cache block、workspace buffer、batch metadata buffer。KV cache 不是模型权重，但它会消耗大量显存。预留得太少，吞吐上不去；预留得太多，模型权重或其他 buffer 可能放不下。

还要区分几种“冷”：

```text
进程冷:
  服务进程刚启动，runtime 和依赖都没初始化。

模型冷:
  模型权重还没加载到本地内存或 GPU 显存。

GPU 冷:
  CUDA context、kernel、通信组、workspace 还没热起来。

缓存冷:
  本地文件缓存、prefix cache、KV cache 复用、tokenizer cache 都没有命中。
```

优化冷启动通常不是一个点，而是一组组合拳：提前把模型放到本地 NVMe、使用 safetensors 或预转换格式、预分片权重、错峰拉起副本、保留 warm pool、启动后先做 warmup、把健康检查放在真正 warmup 完成之后。

面试里可以这样答：

```text
模型冷启动一般包括进程和依赖初始化、模型文件定位或下载、checkpoint shard 读取、配置和 tokenizer 加载、权重反序列化或映射、并行切分和 weight mapping、权重搬到 GPU、KV cache 和 workspace 分配、CUDA/NCCL/kernel 初始化以及 warmup。线上要区分进程冷、模型冷、GPU 冷和缓存冷，因为它们对应的优化手段完全不同。
```

## Q003. checkpoint 加载为什么可能成为瓶颈？

**回答：**

checkpoint 加载容易成为瓶颈，核心原因是它同时吃 **存储带宽、网络带宽、CPU、内存、GPU 显存和跨卡通信初始化**，而且大模型权重体积非常大。

具体来说有几个常见原因。

1. **权重体积太大**

   一个 7B、13B、70B 甚至更大的模型，权重文件可能从十几 GB 到上百 GB。即使磁盘是 NVMe，完整读取也需要时间；如果从对象存储或网络文件系统读取，冷启动时间会更不稳定。

2. **checkpoint shard 数量多**

   大模型通常不是一个单文件，而是多个 shard。加载器要读 index、解析 metadata、打开多个文件、把不同 tensor 映射到不同 device 或并行 rank。大量小 shard 还会放大文件系统 metadata 开销。

3. **格式转换或反序列化成本**

   如果 checkpoint 是 PyTorch pickle 风格的 `.bin`，反序列化有安全和性能负担。Hugging Face TGI 文档也强调 safetensors 相比 pickle-like 格式更安全、更快；如果服务启动时还要把 PyTorch 权重转换成 safetensors，冷启动会进一步变慢。

4. **CPU 内存中转和 dtype 转换**

   很多加载流程会先把权重放到 CPU 内存，再搬到 GPU。中间可能还要做 float32 到 float16/bfloat16、量化格式解析、tensor layout 转换。这些步骤既消耗 CPU，也消耗内存带宽。

5. **多 GPU 切分和映射**

   Tensor parallel 或 pipeline parallel 下，每个 rank 只需要一部分权重，但加载器必须知道每个 tensor 如何切分、重命名、转置或合并。TensorRT-LLM 文档中提到 weight loader 和 weight mapper，就是为了解决不同 checkpoint 格式到内部运行格式的映射问题。

6. **GPU 搬运和显存分配**

   权重最终要进入 GPU 显存。PCIe/NVLink 带宽、GPU 显存碎片、并发副本启动都会影响加载速度。多个副本同时冷启动时，容易出现“所有实例都在读同一批权重”的启动风暴。

7. **校验、安全扫描和远端重试**

   企业环境里可能还会对模型文件做 checksum、签名校验、权限校验、供应链扫描。如果模型来自远端仓库，认证、限流、断点续传、失败重试都会影响启动时间。

实际优化时，我会优先看几个问题：

```text
模型是否已经在本地 NVMe？
是否每次启动都从远端下载？
checkpoint 是否是 safetensors？
是否在启动路径里做临时格式转换？
是否已经按 tensor parallel rank 预分片？
多个副本是否同时拉取同一批大文件？
加载时 CPU 内存是否发生 swap 或 OOM？
GPU 上是否等到第一次请求才初始化 kernel / CUDA graph？
```

常见优化包括：使用本地权重缓存、预热节点、预分片 checkpoint、避免启动时转换格式、使用 safetensors、并行读取但限制并发、把大模型副本滚动启动、保留热池、把健康检查延后到真实 warmup 完成之后。

面试里可以这样答：

```text
checkpoint 加载慢不是单纯文件 I/O 慢。大模型权重很大，通常还有多个 shard，需要解析格式、映射 tensor、做 dtype 或量化处理、经过 CPU 内存中转，再搬到 GPU。多卡推理还要做权重切分和通信初始化。如果权重来自远端存储，网络和对象存储也会进入关键路径。线上一般用本地 NVMe 缓存、safetensors、预分片、warm pool 和错峰启动来降低冷启动影响。
```

## Q004. 模型权重缓存和 KV cache 是同一种缓存吗？

**回答：**

不是。模型权重缓存和 KV cache 都叫 cache，但它们缓存的东西、生命周期、隔离边界和优化目标完全不同。

可以用一张表区分：

```text
模型权重缓存:
  缓存内容: 模型参数、checkpoint、engine、compiled graph
  生命周期: 跟模型版本和服务实例绑定，通常很长
  共享范围: 同一个模型副本上的所有请求共享
  主要位置: 磁盘、本地 NVMe、CPU 内存、GPU 显存
  失效条件: 模型版本变更、权重文件变更、engine 变更
  主要目标: 降低冷启动、避免重复下载和重复加载

KV cache:
  缓存内容: attention 中每层每个 token 的 key/value 激活
  生命周期: 跟请求、会话、prefix 或 batch 调度相关，通常较短
  共享范围: 默认属于某个请求；prefix 相同时可按策略复用
  主要位置: GPU 显存，也可能 offload 到 CPU
  失效条件: 请求结束、会话结束、prefix cache eviction、显存不足
  主要目标: 避免自回归生成时重复计算历史 token 的 K/V
```

模型权重是静态的。一个模型部署起来后，权重对所有请求都是一样的。把权重缓存到本地磁盘或 GPU 显存，是为了让服务不用每次请求都重新下载或重新加载模型。

KV cache 是动态的。它来自某个 prompt 或某段上下文的中间计算结果。自回归模型生成第 `t+1` 个 token 时，需要关注前面所有 token。如果每一步都重新计算历史 token 的 key/value，成本会非常高。KV cache 就是把这些历史 key/value 留下来，下一步直接读。

这两个缓存的安全边界也不一样。模型权重缓存通常不包含用户数据；KV cache 可能包含用户 prompt、会话上下文、RAG 文档片段在模型内部的表示。因此 KV cache 的复用、跨请求共享、落盘、offload、调试 dump 都要更谨慎。

还有一个容易混淆的概念是 **prefix cache**。prefix cache 通常是 KV cache 的一种复用策略：如果两个请求有相同前缀，就复用这个前缀对应的 KV cache。vLLM 的 Automatic Prefix Caching 文档就明确说，它缓存已有查询的 KV cache；新查询如果共享相同前缀，可以跳过共享部分的计算。但这仍然不是模型权重缓存。

面试里可以这样答：

```text
模型权重缓存和 KV cache 不是一种缓存。权重缓存缓存的是模型参数或 engine，生命周期跟模型版本和实例绑定，主要解决冷启动和重复加载。KV cache 缓存的是每个请求或 prefix 在 attention 里的 key/value 中间状态，生命周期跟上下文和生成过程绑定，主要解决自回归生成重复计算历史 token 的问题。权重缓存偏静态，KV cache 偏动态，而且 KV cache 更可能包含用户上下文，需要更严格的隔离。
```

## Q005. KV cache 解决什么问题？

**回答：**

KV cache 解决的是自回归生成中的重复计算问题。

Transformer decoder 在生成文本时是一个 token 一个 token 往后生成的。生成当前 token 时，模型要对之前所有 token 做 attention。对每一层 attention 来说，历史 token 的 key 和 value 一旦算出来，在后续步骤里并不会因为新 token 到来而改变。

如果没有 KV cache，生成过程大致是：

```text
生成第 1 个 token:
  计算 prompt 所有 token 的 K/V

生成第 2 个 token:
  再次计算 prompt + token1 的 K/V

生成第 3 个 token:
  再次计算 prompt + token1 + token2 的 K/V

...
```

这会导致大量重复计算。KV cache 的思路是：

```text
prefill:
  计算 prompt tokens 的 K/V，并写入 KV cache

decode step 1:
  只计算新 token 的 K/V
  attention 读取历史 KV cache
  把新 token 的 K/V 追加到 KV cache

decode step 2:
  继续只计算最新 token 的 K/V
  继续读取历史 KV cache
```

Hugging Face Transformers 的 KV cache 文档也把这个点说得很直接：KV cache 存储可复用的 key/value 计算结果，从而减少计算时间、提高生成速度。

KV cache 的收益主要体现在 decode 阶段。没有它，长输出会因为不断重复计算历史 token 而非常慢；有了它，每一步只需要为新 token 计算新的 K/V，同时读取历史 K/V。

但 KV cache 不是免费的。它带来几个工程问题：

1. **显存占用大**

   KV cache 大小大致和层数、token 数、KV head 数、head dimension、数据类型字节数成正比。上下文越长、batch 越大、并发请求越多，KV cache 占用越高。

   可以粗略理解为：

   ```text
   KV cache memory
     ~= layers * tokens * kv_heads * head_dim * 2(K和V) * bytes_per_element
   ```

2. **内存带宽压力大**

   decode 每一步都要读历史 KV cache。生成越往后，历史上下文越长，读取的数据越多。很多 decode 场景不是纯计算瓶颈，而是显存带宽和 cache 管理瓶颈。

3. **碎片和调度复杂**

   请求长度不同、结束时间不同，KV cache 空间会不断分配和释放。PagedAttention 论文和 vLLM 的设计重点之一，就是用类似操作系统分页的方式管理 KV cache，降低碎片和浪费。

4. **隔离和复用风险**

   KV cache 默认是请求私有状态。prefix cache 可以让相同前缀复用 KV cache，但必须确保前缀完全一致、模型配置一致、采样上下文一致，并且不能跨权限边界错误复用。

面试里可以这样答：

```text
KV cache 解决自回归生成里的历史 token 重复计算问题。prefill 阶段把 prompt 的 key/value 存起来，decode 每生成一个新 token 时只计算这个新 token 的 K/V，然后读取历史 KV cache 做 attention。它能显著降低生成阶段计算量，但会消耗大量显存，并引入内存带宽、碎片管理、prefix 复用和安全隔离问题。
```

## Q006. prefill 和 decode 阶段的计算特征有什么区别？

**回答：**

prefill 和 decode 是 LLM serving 里两个很不一样的阶段。很多调度和性能优化，都是围绕这两个阶段的差异展开的。

**prefill 阶段**处理的是输入 prompt。它一次性把一段上下文 token 喂给模型，计算每一层的 hidden states 和初始 KV cache。

它的特点是：

```text
输入 token 多
可以并行处理整个 prompt
大矩阵乘法占比高
GPU 利用率通常较好
更容易 compute-bound
延迟和 prompt length 强相关
直接影响 first-token latency
```

例如一个 RAG 请求拼接了几千个 token 的文档，第一步要把这些 token 都处理完，才能开始稳定 decode。因此长 prompt 的 TTFT 往往高。

**decode 阶段**处理的是输出生成。自回归模型每一步只生成下一个 token，然后把这个 token 再放回上下文继续下一步。

它的特点是：

```text
每个请求每步通常只处理 1 个新 token
步骤之间有严格依赖，不能随便并行
每步都要读取已有 KV cache
小 batch 时 GPU 利用率容易低
更容易受显存带宽、调度开销、同步开销影响
延迟和 output length 强相关
直接影响 TPOT、ITL 和 total latency
```

这就是为什么 LLM serving 需要特殊 batching。传统模型服务里，一个 batch 进来，模型 forward 一次，结果就结束了；LLM decode 是一个长循环，不同请求输出长度不同，有的请求很快结束，有的请求还要继续生成几百个 token。如果按固定 batch 跑到底，就会浪费大量 GPU 时间。

prefill 和 decode 的冲突也很典型：

```text
prefill:
  大块计算，容易占满 GPU，适合提高吞吐。

decode:
  小步循环，关注每 token 延迟，怕被大 prefill 阻塞。
```

如果服务端一次塞入很多长 prompt，prefill 会把 GPU 占住，正在流式输出的 decode 请求可能出现 token 间隔变大。TensorRT-LLM 的 chunked context / chunked prefill 思路，就是把长 prefill 切成块，让 prefill chunk 可以和 decode token 混合调度，从而降低 TTFT 和队头阻塞。

面试里可以这样答：

```text
prefill 是处理输入上下文，通常一次处理很多 token，大矩阵乘法多，GPU 利用率高，延迟主要跟 prompt 长度相关，并且决定 TTFT。decode 是逐 token 自回归生成，每步只处理新 token，但要读取历史 KV cache，步骤之间有依赖，容易受内存带宽和调度开销影响，主要决定 TPOT 和 total latency。服务端调度要平衡大块 prefill 和小步 decode，否则长 prompt 会阻塞正在生成的请求。
```

## Q007. first-token latency 和 total latency 分别适合衡量什么？

**回答：**

first-token latency 通常也叫 TTFT，表示从请求发出到第一个 token 返回的时间。total latency 表示从请求开始到完整输出结束的总时间。它们衡量的是两个不同体验。

**first-token latency / TTFT** 更适合衡量交互式体验：

```text
用户点发送后，系统多久开始说话？
长 prompt 的 prefill 是否太慢？
排队是否太长？
prefix cache 是否命中？
冷启动是否影响首 token？
流式接口是否及时 flush？
```

聊天机器人、Copilot、客服助手、Agent 控制台这类场景，TTFT 非常重要。即使完整回答要生成十几秒，只要第一个 token 很快出来，用户会感觉系统没有卡死。

但 TTFT 不能代表全部体验。一个系统可能第一个 token 很快，但后续每个 token 都很慢，用户看到的是断断续续输出。

**total latency** 更适合衡量完整任务完成时间：

```text
完整回答多久生成完？
批处理总结多久完成？
代码生成多久返回完整文件？
非流式 API 的等待时间是多少？
一次 agent step 的整体耗时是多少？
```

total latency 受输出长度影响很大。输出 20 个 token 和输出 2000 个 token，不应该只用同一个平均延迟比较。工程上通常还要同时看：

```text
TTFT:
  首 token 速度，体现开始响应的快慢。

TPOT:
  每输出 token 平均耗时，体现 decode 速度。

ITL:
  token 间隔，体现流式输出是否平滑。

Total latency:
  完整请求结束时间，体现整体任务完成速度。

Throughput:
  每秒处理多少 tokens 或 requests，体现系统容量和成本效率。
```

Hugging Face TGI 的 streaming 文档也强调，流式输出可以让用户更早看到结果，降低感知延迟；但它不一定改变完整端到端生成时间。也就是说，TTFT 和 total latency 是两个视角，不能互相替代。

面试里可以这样答：

```text
first-token latency 衡量系统多久开始响应，适合看聊天、Copilot 这类交互式体验，主要受排队、tokenization、prefill、prefix cache 和冷启动影响。total latency 衡量完整输出多久结束，适合看非流式接口、批处理和任务完成时间，主要受输出长度和每 token decode 速度影响。线上通常要同时看 TTFT、TPOT、ITL、total latency 和 throughput，单看一个平均延迟很容易误判。
```

## Q008. throughput、latency、cost 在 LLM serving 中如何取舍？

**回答：**

LLM serving 的 throughput、latency、cost 基本是一个三角关系。想把 GPU 用满，通常要提高 batching 和并发；想要低延迟，通常要减少排队和过大的 batch；想降低成本，通常要提高 tokens/s 和 GPU 利用率，但这又可能伤害尾延迟。

可以先把三个目标定义清楚：

```text
throughput:
  单位时间完成多少 requests 或 tokens。
  LLM serving 里更常用 tokens/s，因为请求输出长度差异很大。

latency:
  用户等待多久。
  至少要拆成 TTFT、TPOT、total latency 和 p95/p99。

cost:
  单位请求或单位 token 的成本。
  主要来自 GPU 小时、显存规格、节点空闲率、网络和存储。
```

**提高 throughput 的手段** 常常包括更大的 batch、continuous batching、更高并发、更长的 batch 等待窗口、更激进的 prefix cache、更高的 GPU 利用率。这样可以降低 cost/token，但代价是请求可能排队更久，TTFT 和 p99 latency 变差。

**降低 latency 的手段** 常常包括限制并发、缩短 batch 等待窗口、给 decode 更高优先级、拆分长 prefill、做 prefix cache、保留热副本。这样用户体验更好，但 GPU 可能吃不满，单位 token 成本上升。

**降低 cost 的手段** 还包括量化、选择更小模型、speculative decoding、KV cache 优化、多租户混部、自动扩缩容。但这些手段都可能有副作用：量化可能影响质量，speculative decoding 增加系统复杂度，小模型可能不满足业务效果，混部可能加剧尾延迟。

一个比较工程化的取舍顺序是：

```text
先定义 SLO:
  例如 TTFT p95 < 1s，TPOT p95 < 50ms，错误率 < 0.1%。

再定义质量边界:
  模型大小、量化格式、最大上下文、最大输出长度不能随意牺牲。

然后在 SLO 内最大化吞吐:
  调 batch、max_num_tokens、max_batch_size、KV cache block、并发数。

最后折算成本:
  看 GPU 利用率、tokens/s/GPU、cost/token、扩缩容效率。
```

TensorRT-LLM 的调度文档里提到 `max_batch_size`、`max_seq_len`、`max_num_tokens` 这些参数会影响调度、workspace 和延迟。`max_num_tokens` 设得太低，吞吐上不去；设得太高，TTFT 和 total latency 可能变差。这就是 throughput-latency trade-off 的典型例子。

还有一个常见误区：只看 GPU utilization。GPU 利用率高不一定代表服务好。可能只是长 prompt 把 GPU 打满了，但用户的 first-token latency 已经爆炸；也可能 decode token 间隔很差，流式体验不稳定。

面试里可以这样答：

```text
LLM serving 里吞吐、延迟和成本要按 SLO 取舍。更大的 batch 和更高并发能提高 tokens/s、降低 cost/token，但会增加排队和尾延迟；更低延迟通常需要缩短 batch 等待、限制并发、优先调度 decode 或拆分长 prefill，但 GPU 利用率和成本会变差。工程上应先定 TTFT、TPOT、p95/p99、质量和错误率边界，再在这些边界内调 max_batch_size、max_num_tokens、KV cache 和并发，最大化 tokens/s/GPU。
```

## Q009. continuous batching 解决什么问题？

**回答：**

continuous batching 解决的是 LLM 请求 **到达时间不同、prompt 长度不同、输出长度不同** 时，传统固定 batch 容易浪费 GPU 和造成队头阻塞的问题。

传统 static batching 大致是：

```text
收集一批请求
  -> 组成 batch
  -> 这批请求一起 prefill / decode
  -> 等整批请求都完成
  -> 再处理下一批请求
```

这对普通分类模型可能还可以，因为一次 forward 后请求就结束。但 LLM decode 是一个逐 token 循环，而且每个请求输出长度不一样：

```text
请求 A 生成 20 个 token 就结束
请求 B 生成 200 个 token 才结束
请求 C 是长 prompt，prefill 很慢
请求 D 是短 prompt，但刚好排在长 prompt 后面
```

如果 batch 固定到最后，请求 A 结束后留下的计算 slot 不能马上给新请求用；请求 D 可能被请求 C 的长 prefill 阻塞；GPU 上有些 step 只有少量活跃序列，利用率很差。

continuous batching，也叫 in-flight batching 或 iteration-level batching，思路是在每个 decode iteration 或调度周期动态调整 batch：

```text
每一轮调度:
  移除已经完成的请求
  接收新到达的请求
  给新请求安排 prefill 或 prefill chunk
  给已有请求安排下一步 decode
  根据 max_batch_size、max_num_tokens、KV cache 空间决定本轮 batch
```

TensorRT-LLM 文档中也把 in-flight batching 描述为 continuous batching / iteration-level batching：context 阶段的序列可以和 generation 阶段一起处理，从而提高吞吐、降低延迟、更好利用 GPU。

continuous batching 带来的收益主要有：

1. **提高 GPU 利用率**

   已完成请求可以马上退出 batch，新请求可以进入，不需要等整批请求全部结束。

2. **降低队头阻塞**

   新短请求不一定要等一个固定大 batch 跑完才能开始。配合 chunked prefill，可以避免超长 prompt 长时间占住 GPU。

3. **适配变长输出**

   LLM 输出长度天然不确定，continuous batching 比固定 batch 更适合这种动态工作负载。

4. **提高吞吐并改善尾延迟**

   在高并发下，continuous batching 能让 decode step 更饱满，同时减少空转。

代价是系统复杂度明显上升。调度器要处理公平性、优先级、KV cache block 分配、请求取消、超时、preemption、max token budget、不同阶段混排等问题。如果调度策略不好，也可能出现长请求饿死、短请求插队过度、prefix cache 命中变差、p99 抖动等问题。

面试里可以这样答：

```text
continuous batching 解决固定 batch 在 LLM 变长生成场景下的浪费和队头阻塞。LLM 请求的 prompt 和输出长度差异很大，decode 又是逐 token 循环。continuous batching 在每个调度迭代里移除完成请求、加入新请求，并把 prefill 和 decode 按 token budget、batch size、KV cache 空间动态混排。这样能提高 GPU 利用率、降低排队、改善吞吐和尾延迟，但需要更复杂的调度和 KV cache 管理。
```

## Q010. static batching 和 dynamic batching 有什么区别？

**回答：**

static batching 和 dynamic batching 的核心区别是：batch 的组成是否在运行时根据请求到达和执行状态变化。

**static batching** 是固定批处理：

```text
先收集一批请求
batch 一旦形成，组成基本固定
通常按同一批一起执行
很多实现会 padding 到相同长度
简单、可预测、调度成本低
```

它适合离线推理、请求长度相近、任务耗时接近、对尾延迟不敏感的场景。比如批量 embedding、批量分类、离线评测，static batching 很容易把 GPU 打满。

但在 LLM 在线生成里，static batching 有明显缺点：

```text
请求输出长度不同，短请求会等长请求。
长 prompt 可能阻塞短 prompt。
padding 会浪费计算。
batch 内请求完成时间不同，GPU slot 会空出来。
新请求不能及时进入正在运行的 batch。
```

**dynamic batching** 是运行时组 batch：

```text
请求到达后进入队列
调度器在一个短时间窗口内聚合请求
根据 batch size、token budget、KV cache、优先级组成 batch
batch 可能随时间变化
可以减少 padding 和空转
```

在很多推理服务系统里，dynamic batching 只是指“请求到达后等一个很短窗口，把多个请求合成一次模型调用”。而在 LLM serving 里，continuous batching 可以看成更细粒度的 dynamic batching：它不是只在请求开始时动态组批，而是在每个 token iteration 都重新调度。

可以这样区分：

```text
static batching:
  固定一批请求一起跑，简单但不适合变长在线生成。

classic dynamic batching:
  在请求进入模型前动态凑批，适合提升一次 forward 的利用率。

continuous batching:
  在 LLM decode 的每个迭代动态加入和移除请求，适合变长生成。
```

dynamic batching 的关键参数通常包括：

```text
max_batch_size:
  一批最多多少个请求或序列。

max_num_tokens:
  一轮调度最多处理多少 token，直接影响吞吐和 TTFT。

batch_wait_timeout:
  最多等多久来凑 batch，越长吞吐可能越好，延迟越差。

KV cache capacity:
  还能容纳多少上下文和生成 token。

priority / fairness:
  是否允许短请求、交互式请求、付费请求优先。
```

所以 static batching 胜在简单、稳定、容易 benchmark；dynamic batching 胜在适配真实在线流量，尤其是 prompt 和输出长度高度不均匀的 LLM workload。代价是 dynamic batching 的调度器更复杂，问题也更隐蔽，比如 p99 抖动、请求饥饿、KV cache 碎片、取消请求清理不彻底。

面试里可以这样答：

```text
static batching 是先固定一批请求再一起执行，简单、可预测，适合离线或长度相近的任务，但在 LLM 变长生成里容易 padding 浪费、短请求等长请求、GPU slot 空转。dynamic batching 是运行时根据请求到达、token budget、batch size、KV cache 和优先级动态组批。LLM serving 里的 continuous batching 是更细粒度的 dynamic batching，每个 decode 迭代都可以加入新请求、移除完成请求，因此更适合在线生成，但调度和 KV cache 管理复杂得多。
```

## Q011. batch size 过大为什么可能提高吞吐但恶化延迟？

**回答：**

batch size 变大后，吞吐提高是很自然的。GPU 不喜欢零碎的小矩阵乘法，也不喜欢大量很短的 kernel launch。把多个请求合在一起，矩阵维度更大，GPU SM 更容易被喂饱，调度、kernel launch、采样、后处理这些固定开销也能摊薄到更多 token 上。

但在线服务看的不是只有 tokens/s。batch size 过大以后，延迟会从几个地方变差。

第一，**凑 batch 本身会带来等待**。

动态 batcher 常见做法是等一小段时间，看有没有更多请求可以加入 batch。等待窗口越长，平均 batch 越大，吞吐可能越好。但先到的请求要多等一段时间，TTFT 会增加。Triton 的 dynamic batcher 文档也把这个取舍说得很清楚：可以提高 `max_batch_size` 或设置非零 batch delay 来换吞吐，但要观察是否超过 latency budget。

第二，**单轮执行时间会变长**。

batch 大以后，一次 GPU iteration 要处理更多 tokens。对 prefill 来说，大 batch 可能让某些请求更晚拿到第一个 token；对 decode 来说，一轮 decode 变慢会直接拉大 token 间隔，也就是用户在流式输出里看到的卡顿。

第三，**长 prompt 会拖住短 prompt**。

如果 batch 里混入长 prompt，请求之间的工作量不均匀。即使移除了 padding，调度器也可能因为 `max_num_tokens` 被长 prompt 占满，导致短请求进不来。TensorRT-LLM 文档里提到，`max_num_tokens` 过高有助于 GPU 利用率，但超过饱和点后可能伤害 TTFT 和 total latency，这就是典型例子。

第四，**KV cache 压力会上升**。

大 batch 意味着同时在服务更多序列，每个序列都要占 KV cache。显存被 KV cache、workspace、activation buffer 挤满后，调度器可能降低并发、拒绝新请求、触发 offload，甚至 OOM。吞吐看起来高，但 p95/p99 会开始抖。

第五，**尾延迟会比平均延迟更敏感**。

平均 throughput 经常在 batch 变大时变好，但线上用户感知的是某一次请求等了多久。batch 太大时，请求排队时间、长请求阻塞、GPU 内存压力、跨卡同步都会叠加到尾延迟上。最后的结果可能是 tokens/s 漂亮，SLO 反而被打爆。

可以把它理解成一条曲线：

```text
batch size 从小到中:
  GPU 利用率上升，吞吐明显变好，延迟可能还能接受。

batch size 继续变大:
  GPU 利用率接近饱和，吞吐提升变慢。

batch size 过大:
  排队、单轮执行时间、KV cache 压力、尾延迟开始恶化。
```

所以生产环境里不会只问“最大 batch 能设多大”，而是先定 SLO，再找吞吐和延迟的拐点。常见调参指标包括 TTFT p95/p99、TPOT p95/p99、tokens/s/GPU、KV cache 使用率、请求拒绝率和 OOM 次数。

面试里可以这样答：

```text
batch size 变大能提高吞吐，因为 GPU 上的大矩阵乘法更容易跑满，调度和 kernel launch 这类固定开销也被摊薄了。但 batch 过大会增加凑批等待，让单次 iteration 更长，还会放大长 prompt 对短请求的阻塞，并消耗更多 KV cache。结果就是 tokens/s 可能变好，但 TTFT、TPOT 和 p99 latency 变差。线上一般先定延迟 SLO，再找 batch size、max_num_tokens 和并发数的拐点。
```

## Q012. vLLM 的 PagedAttention 试图解决什么问题？

**回答：**

PagedAttention 主要解决 LLM serving 里的 KV cache 内存管理问题，尤其是 **KV cache 很大、生命周期动态、长度不均匀时造成的显存浪费和碎片**。

LLM decode 阶段每个请求都要保留 KV cache。问题在于，每个请求的 prompt 长度不同，输出长度也不同。一个请求可能只生成几十个 token，另一个请求可能生成上千个 token。传统做法如果按最大长度预留一大块连续 KV cache，会浪费很多显存。

可以想象一个朴素实现：

```text
为每个请求预留 max_seq_len 的 KV cache
请求实际只用了 200 tokens
但显存里可能按 4096 或 8192 tokens 预留
剩下部分暂时没人能用
```

这样会出现几个问题：

1. **内部碎片**

   给请求分配了一整块最大长度空间，但请求实际没用完。空着的部分属于这个请求，其他请求不能直接用。

2. **外部碎片**

   请求不断进入和退出，显存里留下许多大小不一的空洞。总空闲显存看起来够，但可能没有合适的连续空间给新请求。

3. **batch size 被显存浪费限制**

   KV cache 占了太多显存后，调度器能同时放进来的请求变少。吞吐下降，排队延迟上升。

4. **共享前缀和 beam search 复制成本高**

   多个请求共享相同 prefix，或者 beam search 分支共享历史上下文时，如果每条序列都复制完整 KV cache，会浪费显存。

PagedAttention 借鉴操作系统分页的思路，把每个序列的 KV cache 切成固定大小的 block。逻辑上，一个请求的上下文还是连续 token；物理上，这些 KV block 不要求在 GPU 显存里连续。调度器或 cache manager 维护一张 block table，把逻辑 block 映射到物理 block。

它的基本思路可以这样看：

```text
请求的逻辑 KV cache:
  block0 -> block1 -> block2 -> block3

GPU 显存里的物理 block:
  block0 可能在位置 A
  block1 可能在位置 F
  block2 可能在位置 C
  block3 可能在位置 M

attention kernel 根据 block table 去读这些 block
```

vLLM 的设计文档提到，它的 attention kernel 兼容 paged KV cache，key cache 和 value cache 会存储在分离的 block 中。PagedAttention 论文则把目标讲得更系统：减少 KV cache 内存浪费，并支持更灵活的 KV cache 共享，从而提升同等延迟下的吞吐。

注意，PagedAttention 不是简单地“让 attention 数学公式更快”。它真正解决的是服务系统里的显存管理瓶颈：少浪费显存，就能放下更多并发序列；能共享 KV block，就能减少重复存储；能按 block 分配和回收，就能降低变长请求造成的碎片。

面试里可以这样答：

```text
vLLM 的 PagedAttention 主要解决 KV cache 显存管理问题。LLM 请求长度和输出长度都不固定，如果按最大序列长度给每个请求预留连续 KV cache，会产生大量内部碎片、外部碎片和重复复制，限制 batch size。PagedAttention 把 KV cache 切成固定大小的 block，用 block table 把逻辑连续的上下文映射到物理上不连续的显存块。这样可以按需分配、回收和共享 KV block，提高显存利用率，并在相似延迟下支持更高吞吐。
```

## Q013. GPU memory fragmentation 对 LLM serving 有什么影响？

**回答：**

GPU memory fragmentation 对 LLM serving 的影响很直接：明明总显存看起来还没用完，系统却放不下新请求，或者不得不降低 batch size、触发 offload、重新加载模型，最后把吞吐和尾延迟都拖坏。

LLM serving 里有几类显存长期在竞争：

```text
模型权重
KV cache
prefill / decode workspace
CUDA graph buffer
activation 临时内存
通信 buffer
LoRA adapter 或多模型权重
```

其中 KV cache 最容易制造碎片。原因是请求是动态的：有的请求 prompt 长，有的请求短；有的很快结束，有的会生成很久。KV cache 随着 decode 增长，随着请求完成释放。如果用大块连续内存管理，分配和释放交错后就会出现碎片。

碎片的影响主要有几个。

1. **有效可用显存下降**

   物理上空闲显存很多，但被切成零散小块，无法满足某个大块分配。对需要连续 tensor 的实现来说，这会表现为 OOM 或调度失败。

2. **batch size 被迫变小**

   KV cache 放不下更多序列，调度器只能减少并发。吞吐下降，请求排队变长，TTFT 被拉高。

3. **prefix cache 和模型缓存更容易被驱逐**

   当显存紧张时，系统可能驱逐 prefix KV cache、卸载低频模型、减少缓存池。短期看释放了空间，长期看会增加后续请求的 prefill 和冷启动成本。

4. **尾延迟抖动**

   有些请求刚好命中碎片导致额外等待，有些请求触发显存整理、offload 或重试。平均延迟可能还正常，p99 会先坏。

5. **重启变成“修复内存”的手段**

   如果 allocator 或框架释放后不能很好地把内存还给系统，长时间运行的 serving 进程可能出现显存水位越来越高。Triton 的 model management 文档也提到，模型 load/unload 后看到内存增长时，可能和内存分配器释放策略有关，可以考虑 tcmalloc、jemalloc 这类分配器差异。GPU 侧也有类似的工程问题，只是表现更昂贵。

PagedAttention、paged KV cache、固定大小 block、KV cache pool，本质都是在缓解这个问题。TensorRT-LLM 文档也对比了 contiguous KV cache 和 paged KV cache：连续 KV cache 是一个按最大长度预留的 monolithic tensor，短序列会浪费内存；paged KV cache 则把 cache 分成 block，由 cache manager 分配和回收。

实际工程里，我会用这些办法降低碎片影响：

```text
提前规划模型权重、KV cache、workspace 的显存预算。
用固定大小 block 管理 KV cache。
按 token budget 做准入控制，而不是只按 request count。
限制最大上下文和最大输出。
对长 prompt 做 chunked prefill。
监控 reserved memory、allocated memory、KV block 使用率和 OOM。
必要时做滚动重启，但不要把重启当作唯一方案。
```

面试里可以这样答：

```text
GPU memory fragmentation 会降低有效显存。LLM serving 里 KV cache 会随着变长请求动态增长和释放，如果按大块连续内存管理，很容易出现总空闲显存够但放不下新请求的情况。结果是 batch size 下降、请求排队增加、prefix cache 或模型缓存被驱逐、p99 latency 抖动，严重时 OOM 或需要重启进程。Paged KV cache、固定 block、KV cache pool 和 token budget 准入控制都是为了减少这类碎片。
```

## Q014. 模型并行、张量并行、流水线并行分别解决什么问题？

**回答：**

这三个词容易混在一起。先讲边界：**模型并行** 是一个大类，意思是把一个模型拆到多个设备上执行；**张量并行** 和 **流水线并行** 是模型并行里常见的两种拆法。

TensorRT-LLM 的 parallelism 文档给了一个很实用的判断：多 GPU 并行通常在两种情况下需要，要么模型放不进单张 GPU，要么单张 GPU 性能不够。

可以这样区分。

**模型并行**

模型并行是总称，解决的是“单个模型无法或不适合由一个设备独立承载”的问题。

```text
目标:
  把一个模型拆到多个 GPU 上。

解决:
  单卡显存放不下模型。
  单卡吞吐或延迟达不到要求。
  某些模型结构需要跨设备拆分。

代价:
  引入跨 GPU 通信、同步和调度复杂度。
```

面试里可以把模型并行理解成 umbrella term，不要把它和张量并行并列成完全独立的第三种机制。

**张量并行**

张量并行通常在 layer 内部切分权重矩阵或 attention heads。每张 GPU 保存某个 tensor 的一部分，处理同一批 token 的一部分计算，最后通过 all-reduce、all-gather 或 concat 合并结果。

```text
解决:
  单层权重太大。
  单卡算力不够。
  希望多个 GPU 同时处理同一层。

特点:
  layer 内并行。
  每个 GPU 都看到同一批输入 token。
  通信发生在层内或层间的张量合并处。

风险:
  通信频繁，依赖 NVLink / PCIe / 网络。
  batch 太小或互联太慢时，通信成本可能吃掉收益。
```

Hugging Face TGI 的 tensor parallelism 文档用矩阵乘法举例：把权重按列切开，各 GPU 分别计算后再把结果拼起来。这个例子很好理解：张量并行切的是 tensor，不是请求。

**流水线并行**

流水线并行通常按层切分模型。比如前 20 层放 GPU0，中间 20 层放 GPU1，后 20 层放 GPU2。一个请求的 activation 会从第一段流到第二段，再流到第三段。

```text
解决:
  模型整体太深或太大，按层拆开后才能放下。
  权重跨多 GPU 分摊。

特点:
  layer 间并行。
  不同 GPU 负责不同层段。
  activation 在 stage 之间传递。

风险:
  pipeline bubble。
  小 batch 或交互式请求里，某些 stage 可能空等。
  decode 阶段逐 token 依赖强，流水线效率不一定理想。
```

还要顺手区分 **数据并行**。数据并行不是把一个模型拆开，而是每张 GPU 放一份完整模型，不同 GPU 处理不同请求。它解决的是高吞吐和横向扩展，不解决单模型放不进单卡的问题。

总结一下：

```text
模型并行:
  总称，把一个模型拆到多个设备上。

张量并行:
  切 layer 内部的 tensor / weight / head，适合单层很大或需要多卡共同算一层。

流水线并行:
  切不同层，适合模型整体太深太大，需要按层分段放到多卡。

数据并行:
  不拆模型，而是复制模型，用不同副本处理不同请求。
```

面试里可以这样答：

```text
模型并行是总称，解决单个模型放不进单卡或单卡性能不够的问题。张量并行是在层内部切分权重矩阵、attention head 这类 tensor，多张 GPU 处理同一批 token 的不同计算片段，再通过通信合并结果。流水线并行是按层切分模型，不同 GPU 负责不同 layer stage，activation 在 stage 间传递。张量并行更像切一层里的矩阵，流水线并行更像切模型深度；两者都要付跨卡通信和调度成本。
```

## Q015. 多模型共用 GPU 时如何做调度？

**回答：**

多模型共用 GPU 的调度不能只做简单 round-robin。因为每个模型的权重大小、KV cache 占用、加载时间、batch 形态、SLO、租户优先级都不一样。一个小 embedding 模型、一个 7B chat 模型、一个 70B chat 模型放在同一组 GPU 上，调度策略肯定不能一样。

可以把问题拆成三层。

第一层是 **放置**：哪些模型常驻哪些 GPU。

```text
大模型:
  通常独占一组 GPU，或者用 tensor/pipeline parallel 跨多卡。

中小模型:
  可以多个模型共享一张 GPU，但要限制显存和并发。

热点模型:
  多副本，靠数据并行扩吞吐。

冷门模型:
  不常驻，按需加载，配合模型缓存和 eviction。
```

第二层是 **请求调度**：一个请求来了，送到哪个模型副本。

调度器至少要看：

```text
目标模型是否已经加载。
该副本队列长度和当前 batch 状态。
该 GPU 剩余 KV cache / workspace。
请求的 prompt length 和 max_new_tokens。
请求是否有 prefix cache 或会话 locality。
请求的租户、优先级、超时时间和 SLO。
```

第三层是 **资源仲裁**：多个模型同时想跑时，谁先用 GPU。

NVIDIA Triton 提供了两个相关概念。一个是 instance group，可以指定模型实例数量以及放在哪些 GPU 上；另一个是 rate limiter，可以跨已加载模型做优先级和资源约束，避免所有模型实例同时执行把服务压到 OOM。这些机制虽然不等于完整的 LLM 调度器，但它们体现了生产系统里必须考虑的两个点：模型实例放置和跨模型资源限制。

多模型共用 GPU 常见策略有几种。

```text
静态隔离:
  每个模型或模型组分配固定 GPU / MIG / 显存份额。
  好处是稳定，坏处是空闲资源不好复用。

多副本路由:
  热点模型有多个副本，请求路由到队列短、KV 空间足、prefix cache 命中的副本。

按需加载:
  冷门模型不常驻，收到请求后加载。
  要配合模型缓存、超时、排队和 eviction。

优先级调度:
  交互式请求优先于批处理，高等级租户优先于低等级租户。
  但要防止低优先级请求长期饥饿。

SLO-aware 调度:
  如果某个请求已经接近超时，就不再把它送到需要冷加载的副本。
```

LLM 场景还要特别注意 KV cache。多模型共享 GPU 时，不只是权重占显存，正在生成的请求也占 KV cache。一个模型刚开始跑时看起来能放下，但随着 decode 变长，KV cache 会继续增长。调度器只按模型权重估算显存是不够的。

比较稳的设计是：

```text
把模型权重常驻计划和请求级 KV cache 预算分开。
每个模型维护独立队列和 SLO。
按 model_id、tenant、priority、prompt length、max_new_tokens 做 admission control。
优先路由到已经加载模型且有缓存 locality 的副本。
超过显存或 SLO 风险时，拒绝、降级、排队或转移到别的副本。
```

面试里可以这样答：

```text
多模型共用 GPU 时，调度器要同时管模型放置、请求路由和资源仲裁。不能只看队列长度，还要看模型是否已加载、权重大小、KV cache 空间、batch 状态、prompt/output 长度、优先级和 SLO。热点模型通常多副本常驻，冷门模型按需加载并受 eviction 策略管理；跨模型执行要有限流和资源配额，避免多个模型同时分配 workspace 或 KV cache 导致 OOM。生产上还会优先把请求路由到已有模型权重、prefix cache 或会话状态的副本。
```

## Q016. 模型缓存 eviction 时应该考虑 size、热度、加载成本还是 SLO？

**回答：**

都要考虑。模型缓存 eviction 如果只用 LRU，很容易做出错误选择。模型缓存和普通 key-value cache 不一样，一个模型可能几十 GB，加载一次可能几十秒，还可能影响某个高优先级业务的 SLO。

可以先明确这里说的“模型缓存”通常包括：

```text
本地磁盘上的 checkpoint 缓存
CPU 内存里的权重缓存
GPU 显存里的模型权重或 engine
LoRA adapter 缓存
编译后的 TensorRT engine / CUDA graph / runtime artifact
```

eviction 需要看的因素至少有四类。

1. **size**

   大模型释放一次能拿回很多空间，但如果它很热或者加载很慢，驱逐它可能得不偿失。小模型释放空间少，但如果完全没人用，驱逐它的风险低。

2. **热度**

   热度不是只看最近一次访问。更合理的是看一段时间内的请求频率、租户分布、周期性流量、会话连续性。一个模型刚刚没被访问，不代表它不重要；比如每小时整点有一波批任务。

3. **加载成本**

   加载成本包括对象存储下载、磁盘读取、反序列化、权重搬到 GPU、并行 rank 初始化、engine 构建、warmup。两个同样大小的模型，加载成本可能完全不同。

4. **SLO 影响**

   如果某个模型服务的是强交互业务，冷加载会直接让请求超时，那它的保留优先级应该高。反过来，一个离线批处理模型即使重新加载慢一点，也可能可以接受。

一个简单的直觉评分可以写成：

```text
保留价值 ~= 未来命中概率 * 冷加载成本 * SLO 惩罚系数 / 占用空间
```

这个公式不需要照搬到生产，但它表达了一个关键点：eviction 不是只看“最近用没用”，而是看“留着它能避免多少未来成本”。

真实系统还会加一些约束：

```text
正在服务请求的模型不能直接驱逐。
刚加载完成的模型不要马上驱逐，要有 hysteresis。
高优先级模型、基座模型、系统模型可以 pin 住。
同一模型如果已经有多个副本，可以先驱逐冗余副本。
不同租户之间要有公平性，不能让一个租户挤掉所有缓存。
模型版本变更时，旧版本和新版本要分开计数。
```

还要区分磁盘缓存和 GPU 缓存。磁盘缓存 eviction 的目标是减少远端下载；GPU 缓存 eviction 的目标是释放显存给权重、KV cache 和 workspace。GPU 缓存更贵，因为一旦驱逐，后续请求可能要经历完整 H2D 搬运和 warmup。

面试里可以这样答：

```text
模型缓存 eviction 应该同时考虑 size、热度、加载成本和 SLO。只用 LRU 不够，因为大模型释放空间多但加载成本也高，冷门小模型释放空间少但风险低；高 SLO 模型即使访问间隔稍长也不一定能驱逐。比较合理的思路是估计保留价值：未来命中概率乘以冷加载成本和 SLO 惩罚，再除以占用空间。同时要保护正在执行的模型、设置 hysteresis、支持 pinning，并区分磁盘缓存和 GPU 显存缓存。
```

## Q017. checkpoint 从对象存储拉取时如何优化冷启动？

**回答：**

checkpoint 从对象存储拉取时，冷启动慢通常不是单点问题，而是对象存储带宽、并发请求、文件 shard、反序列化、CPU buffer、GPU 搬运和并行加载共同造成的。

优化可以按链路拆。

第一，**尽量不要在请求路径上第一次拉模型**。

生产上应该让模型在接流量前完成预取和校验：

```text
节点启动时预取热点模型。
滚动发布前提前把新版本拉到本地。
扩容时先 warm pool，再切流量。
健康检查要等模型真的可用，而不是进程刚启动就 ready。
```

第二，**使用本地缓存**。

对象存储适合做权威来源，不适合每次冷启动都从零读取。常见做法是把 checkpoint 缓存在本地 NVMe，按模型版本和 checksum 做内容寻址。节点重启后，如果本地缓存还在，就跳过远端下载。

第三，**选对权重格式**。

Hugging Face TGI 文档提到 safetensors 相比 pickle-like 格式更快也更安全，并且 TGI 会优先查找 safetensors 权重；如果没有，可能在 serving 时把 PyTorch 权重转换成 safetensors。这个转换不应该出现在关键启动路径里。能提前转就提前转，能提前分片就提前分片。

第四，**提高读取并发，但要有限制**。

对象存储通常支持并发 range read。可以按 shard 并行下载，也可以按 tensor 并行读取。但并发不是越大越好，过大会打爆对象存储、网卡、CPU buffer 或本地磁盘。vLLM 的 Run:ai Model Streamer 文档里就提供了 `concurrency` 这类参数，用来控制从文件或 S3 server 读取 tensor 的并发。

第五，**使用 streaming loader 或直接加载到 GPU 的路径**。

vLLM 文档里提到 Run:ai Model Streamer 可以并发读取 tensor 并 stream 到 GPU memory，也支持从 S3、GCS、Azure Blob 这类对象存储加载。fastsafetensors 则利用 GPU Direct Storage 加载权重到 GPU memory。不同硬件和存储环境收益不同，但方向是一致的：减少不必要的中间拷贝和串行等待。

第六，**为并行部署预分片**。

如果模型用 tensor parallel 或 pipeline parallel，每个 worker 只需要自己那部分权重。vLLM 的 Run:ai Model Streamer 文档也提到 sharded loader 对 tensor 或 pipeline parallel 模型特别有用，因为每个 worker 只需要读自己的 shard，而不是读完整 checkpoint 再丢弃一部分。

第七，**避免扩容风暴**。

最容易被忽略的是同时冷启动。一次扩容 100 个副本，全部从对象存储拉同一个 70B checkpoint，可能把对象存储、NAT、节点磁盘和 GPU 初始化全部打满。应该做错峰启动、分批预取、本地镜像缓存、P2P 分发或区域级缓存。

可以把优化清单写成：

```text
本地 NVMe 内容寻址缓存。
提前预取和 checksum。
safetensors 或预转换格式。
按并行 rank 预分片。
并发 range read，但限制并发。
streaming loader，减少中间拷贝。
对象存储和 GPU 节点同 region / zone。
扩容错峰，避免 thundering herd。
warm pool 接流量前完成真实 warmup。
```

面试里可以这样答：

```text
从对象存储拉 checkpoint 的冷启动优化，核心是不要让远端下载和格式转换落到首个用户请求上。常见做法是本地 NVMe 缓存、提前预取和校验、使用 safetensors、预分片 checkpoint、限制并发的 range read、使用 streaming loader 或 GPU Direct Storage、让每个 tensor/pipeline rank 只读自己的 shard，并且扩容时错峰启动，避免所有副本同时打对象存储。健康检查要等模型加载和 warmup 完成后再放流量。
```

## Q018. 模型预热会带来什么资源浪费？

**回答：**

模型预热的价值是减少第一次请求的慢启动，但它不是免费的。预热越激进，资源浪费越明显。

Triton 的 model warmup 文档说明了一个典型原因：某些 backend 会把一部分初始化推迟到第一次或前几次推理请求，warmup 可以让模型在真正接收请求前完成初始化。但文档也提醒，warmup 会让模型更新时服务响应变慢，需要按场景实验选择配置。

预热的资源浪费主要有这些。

1. **GPU compute 浪费**

   warmup 请求会真的跑模型。大模型 prefill 和 decode 都要占 GPU。如果同时 warmup 很多模型，会挤掉真实流量。

2. **显存占用浪费**

   预热通常要求模型权重、workspace、CUDA graph buffer、可能还有 KV cache pool 都准备好。模型热着但没有请求时，这些显存就是机会成本，别的模型或请求用不了。

3. **对象存储和磁盘带宽浪费**

   如果预热触发模型加载，会读取 checkpoint。预热了很多最后没被访问的模型，本质上就是提前消耗了下载和磁盘 I/O。

4. **缓存污染**

   预热低频模型可能挤掉真正热点模型的本地缓存、GPU 缓存或 prefix cache。缓存容量固定时，“多热一点”不一定更好。

5. **扩容和发布变慢**

   如果健康检查必须等待所有 warmup 完成，发布、扩容和故障恢复都会变慢。对大模型集群来说，这个时间可能是分钟级。

6. **过期和无效预热**

   warmup 只对某些初始化有效。模型版本切换、CUDA graph shape 变化、batch size 变化、max context 变化后，之前的 warmup 可能不再覆盖真实请求。用一个很短的假 prompt 预热，也不一定能代表长上下文请求。

比较合理的做法是有选择地预热：

```text
只预热热点模型和强 SLO 模型。
按历史流量预测预热时间。
用代表性 prompt length 和 batch shape。
限制 warmup 并发，避免启动风暴。
把 warmup 和 readiness gate 绑定，但不要无限扩大 warmup 范围。
低频模型允许冷启动或进入低优先级队列。
```

面试里可以这样答：

```text
模型预热能减少首次请求的慢启动，但会浪费 GPU compute、显存、对象存储带宽和本地缓存容量。预热的模型如果没被访问，权重和 workspace 常驻就是机会成本；同时预热很多模型还会拖慢扩容和发布，并可能污染缓存。生产上通常只预热热点模型、强 SLO 模型和即将切流的版本，用代表性输入做 warmup，并限制预热并发。
```

## Q019. 如何预测某个请求的模型加载成本？

**回答：**

预测模型加载成本，要先区分“这个请求要不要加载模型”。如果目标模型已经在某个副本的 GPU 显存里，加载成本基本为零；如果只在本地磁盘缓存里，需要读盘和搬到 GPU；如果还在对象存储里，就要把远端下载也算进去。

一个实用的成本模型可以拆成：

```text
T_load
  ~= T_queue_for_loader
   + T_remote_fetch
   + T_local_read
   + T_deserialize_or_map
   + T_transform
   + T_host_to_device
   + T_parallel_init
   + T_runtime_init
   + T_warmup
```

每一项都可以用特征估计。

**T_remote_fetch**

看模型是否命中本地缓存。如果没有命中，就按对象存储读取字节数、有效带宽、并发限制、重试概率估算。不能只用文件大小除以标称带宽，真实环境还要看对象数量、range read 并发、同节点是否有其他下载。

**T_local_read**

本地 NVMe、网络文件系统、容器镜像层的读性能差异很大。很多时候对象存储已经不是瓶颈，本地解压、overlayfs、metadata 操作反而慢。

**T_deserialize_or_map**

safetensors、PyTorch `.bin`、量化格式、TensorRT engine 的加载成本不同。如果需要启动时转换格式，成本要单独算。

**T_transform**

包括 dtype 转换、量化/反量化准备、tensor reshape、weight mapping、按 tensor parallel rank 切分。TensorRT-LLM checkpoint loading 文档把 checkpoint loader、config loader、weight loader、weight mapper 分开描述，说明这些步骤在系统里不是一个简单文件读取。

**T_host_to_device**

权重进入 GPU 显存要经过 PCIe、NVLink 或类似通道。模型越大、并发加载越多，搬运成本越明显。

**T_parallel_init**

多卡模型要初始化 NCCL 通信组、rank 拓扑、tensor/pipeline parallel 结构。跨节点时还要考虑网络和 rendezvous。

**T_runtime_init 和 T_warmup**

第一次请求可能触发 CUDA context、kernel autotune、CUDA graph capture、engine runtime 初始化。即使权重已经加载，第一次真实推理仍然可能慢。

预测时不要只做静态公式。更稳的是把历史指标纳入模型：

```text
features:
  model_id / model_version
  weight_bytes
  shard_count
  format
  quantization
  parallelism config
  cache_state: gpu / cpu / disk / remote
  node_type
  storage backend
  current download concurrency
  current GPU memory pressure
  recent p50 / p95 load time

outputs:
  expected_load_time_p50
  expected_load_time_p95
  timeout_risk
  是否值得把请求排到该节点
```

调度器拿到预测结果后，可以做更好的选择：如果某个副本已经加载模型但队列略长，另一个副本需要冷加载，交互式请求通常应该去前者；如果是离线任务，可以接受后者排队加载。

面试里可以这样答：

```text
预测模型加载成本时，先看模型是否已经在 GPU、CPU、本地磁盘或对象存储。加载时间可以拆成 loader 排队、远端拉取、本地读盘、反序列化或 mmap、格式转换、权重搬到 GPU、多卡通信初始化、runtime 初始化和 warmup。生产上会按 model version、权重大小、shard 数、格式、并行配置、缓存状态、节点类型和历史 p50/p95 load time 建模。调度器再根据这个预测决定是路由到已加载副本，还是接受冷加载。
```

## Q020. locality-aware scheduling 的基本思想是什么？

**回答：**

locality-aware scheduling 的基本思想是：不要只把请求发给“当前最空”的节点，而要把请求发给 **已经拥有相关本地状态、能少搬数据、少重复计算、少冷启动** 的节点。

在 LLM serving 里，locality 可以有很多种。

```text
模型权重 locality:
  目标模型已经加载在某个 GPU 上。

prefix / KV cache locality:
  某个副本已经有相同系统 prompt、文档 prefix、多轮会话历史的 KV cache。

LoRA adapter locality:
  某个租户或任务的 adapter 已经加载。

会话 locality:
  多轮对话连续落在同一个或同一组副本上。

数据 locality:
  RAG 文档、embedding cache、工具结果在某个节点附近。

拓扑 locality:
  tensor parallel 所需 GPU 在同一 NVLink island，跨节点通信更少。
```

如果忽略 locality，调度器可能做出看似合理但很慢的决策。例如节点 A 队列稍长，但已经加载目标模型并且命中 prefix cache；节点 B 当前空闲，但需要从对象存储加载模型，还要重新 prefill 几千个 token。只看队列长度会选 B，真实延迟反而更差。

locality-aware 调度通常会给每个候选副本打分：

```text
score(worker)
  = queue_cost
  + expected_compute_cost
  + expected_load_cost
  + expected_kv_transfer_cost
  - cache_hit_benefit
  + SLO_risk_penalty
```

这个分数表达的是：排队短不一定最好，综合成本低才好。

在 LLM 场景里，prefix cache locality 特别重要。vLLM 的 Automatic Prefix Caching 文档说明，如果新请求和已有请求共享相同前缀，可以复用已有 KV cache，跳过共享部分的 prefill。那调度器就应该尽量把相同系统 prompt、相同长文档、同一会话的请求路由到有对应 prefix cache 的副本。

不过 locality-aware scheduling 也不能走极端。只追求 locality 会带来热点：

```text
某个热门模型或热门 prefix 的副本被打爆。
少数长会话一直粘在同一节点，导致负载不均。
冷节点永远接不到流量，缓存越来越冷。
强行 sticky session 会降低故障切换能力。
```

所以真实系统会在 locality 和 load balancing 之间取舍。研究里也有 locality-aware fair scheduling 这类工作，目标就是在 prefix locality、吞吐、公平性之间找平衡，而不是单纯最大化 cache hit rate。

常见实现手段包括：

```text
consistent hashing 或 rendezvous hashing，保持会话粘性。
最长 prefix match，把请求路由到 KV prefix 最匹配的副本。
bounded-load routing，命中 locality 但不允许节点超过负载上限。
缓存命中收益估算，只有收益足够大才牺牲一点负载均衡。
fallback 机制，目标副本慢或失败时允许迁移。
```

面试里可以这样答：

```text
locality-aware scheduling 是把请求路由到已经拥有相关本地状态的副本，而不是只选当前队列最短的副本。LLM serving 里的 locality 包括模型权重已加载、prefix/KV cache 命中、LoRA adapter 已加载、同一会话状态、数据位置和 GPU 拓扑。它的收益是减少冷加载、减少重复 prefill、减少 KV 或权重搬运；风险是热点和负载不均。所以调度器一般会在 cache hit benefit、queue cost、load cost 和 SLO risk 之间打分，做有边界的 locality 优先。
```

## Q021. 为什么 cache locality 和 load balancing 可能冲突？

**回答：**

cache locality 和 load balancing 冲突，本质上是两个目标在拉扯。

locality 想把请求送到“已经有相关状态”的地方。这个状态可能是模型权重已经在 GPU 上、LoRA adapter 已经加载、prefix/KV cache 命中、同一会话历史还在本地、RAG 文档 embedding 或工具结果还在缓存里。命中 locality 的好处很直接：少加载模型，少做 prefill，少搬 KV，TTFT 更低，GPU 做的无效工作更少。

load balancing 想把请求分散到“当前更空”的地方。它关心的是队列长度、正在运行的请求数、剩余 KV cache block、GPU 利用率、并发上限、租户配额和 SLO 风险。命中负载均衡的好处也很直接：避免热点副本被打满，降低尾延迟，故障时也更容易切走。

问题在于，最有 locality 的副本经常不是最空的副本。举个 LLM serving 的例子：

```text
worker A:
  已经加载 model-X
  有同一个长文档的 prefix cache
  队列里还有 20 个请求

worker B:
  当前很空
  没有 model-X
  没有相关 prefix cache
  需要冷加载或重新 prefill 8000 tokens
```

如果只看 locality，会把新请求继续送到 A。短期内看起来省了 prefill，但 A 的队列越来越长，后来的短请求也跟着排队，p99 TTFT 会被拉高。

如果只看 load balancing，会把请求送到 B。B 很空没错，但它要加载模型或者重新算长 prompt，可能反而比在 A 排一小会儿还慢。更糟的是，B 算完之后也会产生一份新的 KV cache，缓存被复制到更多副本上，显存压力更大。

所以真实调度器不会把两者写成二选一，而是算综合成本：

```text
estimated_cost(worker)
  = queue_wait
  + prefill_cost_after_prefix_hit
  + decode_cost
  + model_load_cost
  + kv_transfer_or_recompute_cost
  + tenant_slo_penalty
  - locality_benefit
```

这里的核心不是“命中缓存就一定路由过去”，而是问：缓存命中节省的时间，能不能抵消这个副本当前排队和过载带来的时间。

Locality-aware Fair Scheduling 这类研究也在讨论同一个问题：传统公平调度如果不看 prefix locality，会损失缓存收益；只追求 prefix 命中率，又可能牺牲租户公平和负载均衡。因此调度器通常需要一个有边界的 locality 优先级，而不是无限 sticky。

常见做法有几类。

第一，给 locality 设置负载上限。比如某个副本命中 prefix cache，但它的 queue wait 已经超过阈值，或者 KV cache 使用率接近 100%，就不再继续粘过去。

第二，只在收益足够大时牺牲负载均衡。相同系统 prompt 几十个 token，不值得为了命中 prefix cache 把请求送到热点副本；相同法律文档 100k tokens，收益就很大，可以接受一点排队。

第三，把 locality 分层。模型权重 locality 通常比普通 prefix cache locality 更重要，因为冷加载模型的成本很大；同一会话的 KV cache locality 又可能比普通静态 prompt locality 更重要，因为它直接影响多轮对话 TTFT。

第四，做 bounded-load routing。先找 locality 最好的若干候选，再过滤掉明显过载的节点；或者先找负载健康的节点，再在里面选 prefix match 最长的。

第五，给多租户加公平约束。不能让一个租户的长会话因为缓存命中一直占住热点副本，挤掉其他租户的短请求。可以按租户维护 token budget、并发上限和 deficit counter。

面试里可以这样答：

```text
cache locality 想把请求送到已有模型权重、prefix/KV cache 或会话状态的副本，load balancing 想把请求送到当前更空的副本。两者冲突是因为最有缓存收益的副本经常也是热点副本。只追求 locality 会造成队列堆积和租户不公平；只追求负载均衡又会丢掉 prefix cache、触发冷加载或重复 prefill。生产调度一般会估算 queue wait、model load cost、prefill cost、KV cache benefit 和 SLO 风险，在缓存收益足够大且目标副本不过载时才优先 locality。
```

## Q022. 历史 EWMA latency 适合预测哪些场景，不适合哪些场景？

**回答：**

EWMA 是 exponentially weighted moving average，指数加权移动平均。它的特点是最近样本权重大，旧样本权重逐渐衰减。写成形式大概是：

```text
ewma_new = alpha * latency_now + (1 - alpha) * ewma_old
```

`alpha` 越大，越相信最近一次观测；`alpha` 越小，曲线越平滑，但反应更慢。

EWMA 适合预测“同质、稳定、短期相关”的延迟。比如同一个模型、同一种硬件、相近 prompt length、相近 batch 策略、相同缓存状态下，最近一段时间的 prefill latency、decode TPOT、模型加载时间、网络 RPC 延迟，往往有短期惯性。此时 EWMA 很有用，因为它便宜、在线更新简单，不需要复杂特征，也不会被很久以前的旧状态拖住。

它适合这些场景：

```text
同一模型副本的短期 queue wait 趋势
同一 GPU 上 decode 每 token 延迟的短期抖动
某个模型从本地 NVMe 加载到 GPU 的近期耗时
同一种请求形态的 tokenizer / prefill 延迟
某个后端最近是否进入慢状态
```

举例说，一个 7B 模型在同一张 A100 上，最近几分钟的 prompt 大多在 1k 到 2k tokens，batch 大小也差不多。此时用 EWMA 估算下一批请求的 prefill 时间，比用全局平均值靠谱。

但 EWMA 不适合预测“结构性差异很大”的请求。LLM serving 里最容易出错的地方就是把所有请求的延迟揉成一个平均数。一个 20 token 的短问答和一个 100k token 的长文档分析，请求路径完全不同。EWMA 如果不带特征，只会给出一个看似平滑、实际没意义的中间值。

它不适合这些场景：

```text
prompt length 差异很大的混合流量
max_tokens 差异很大的请求
prefix cache 命中和未命中混在一起
模型冷启动和热路径混在一起
不同模型大小混在一起
batch 策略刚调整过
GPU 刚发生 OOM、preemption、降级或迁移
突发流量、租户切换、批处理任务突然进入
```

原因有几个。

第一，LLM 延迟不是单峰分布。短 prompt、长 prompt、cache hit、cache miss、长输出、短输出可能是几组不同分布。一个 EWMA 只能描述一条线，描述不了多类请求。

第二，EWMA 对非平稳变化反应有限。比如新模型版本刚上线，KV cache block size、quantization、CUDA graph shape 或 max_num_tokens 都改了，旧 EWMA 还会影响一段时间。

第三，它容易被尾部样本污染。GPU OOM 前后的重试、冷加载、对象存储抖动，会把 EWMA 拉歪。如果调度器马上拿它做路由，可能形成错误反馈：某个节点因为一次慢请求被认为很慢，流量被切走，后续又缺少新样本来纠正。

第四，它不能解释原因。EWMA 只能说“最近慢”，不能告诉你慢在 queue wait、prefill、decode、KV cache transfer、model loading 还是网络。

更合理的做法是把 EWMA 拆细，而不是不用：

```text
按 model_id / model_version 分开
按 prompt token bucket 分开
按 max_tokens bucket 分开
按 cache_hit / cache_miss 分开
按 prefill / decode / queue / load 分开
按 GPU type 和 parallelism config 分开
同时保留 p95/p99 或 histogram，而不是只保留均值
```

面试里可以这样答：

```text
EWMA 适合预测短期稳定、同质请求的延迟，例如同一模型副本最近的 queue wait、decode TPOT、热模型 prefill latency 或本地模型加载时间。它不适合把长 prompt、短 prompt、cache hit、cache miss、冷启动、热路径和不同模型混在一起预测。LLM serving 的请求成本强依赖 prompt length、max_tokens、batch 状态和 KV cache 状态，单一 EWMA 很容易误导调度器。生产上可以用分桶 EWMA：按模型、长度、缓存命中、阶段和硬件拆开，再配合 histogram 和尾延迟指标。
```

## Q023. LLM 请求的 prompt length 如何影响调度决策？

**回答：**

prompt length 是 LLM serving 调度里最重要的输入特征之一。它影响的不只是“请求会不会慢”，还会影响 TTFT、batch 组成、KV cache 占用、max_num_tokens 预算、prefix cache 收益，以及长短请求之间的干扰。

先看计算路径。一个请求进入模型后，通常先做 prefill，再做 decode：

```text
prompt tokens -> prefill -> first token -> decode output tokens
```

prompt length 主要影响 prefill。prompt 越长，prefill 要处理的 token 越多，要生成的初始 KV cache 也越多。长 prompt 一般会拉高 TTFT，因为第一个 token 必须等 prefill 做完之后才能出来。

它对调度的影响可以分成几层。

第一，影响准入控制。调度器要先判断请求能不能放进当前 iteration 或 batch。TensorRT-LLM 的调度文档里提到，`max_num_tokens` 会限制一次调度能处理的 token 数；如果前几个请求的 prompt 已经占掉大部分 token budget，后面的长 prompt 就进不来。TGI 的 launcher 参数也把 `max_total_tokens` 称为运行请求的 memory budget，因为 prompt tokens 加上生成 tokens 会决定每个请求占用的资源。

第二，影响 TTFT 预测。两个请求都只生成 100 tokens，但一个 prompt 是 50 tokens，另一个 prompt 是 50k tokens，用户等待第一个 token 的时间完全不同。因此调度器不能只看输出长度，也不能只看历史平均 latency。

第三，影响 KV cache 预算。prefill 结束后，prompt 的 K/V 会留在 KV cache 里。长 prompt 会占更多 block，减少同一时刻可容纳的并发请求数。即使它后续只生成很少 tokens，也可能占住大量显存。

第四，影响 batching。prefill 是大块计算，长 prompt 可能让一个 iteration 变得很重。如果把它和一批正在 decode 的短请求放在一起，decode 的 token 间隔可能变大。TensorRT-LLM 文档里介绍的 chunked context / chunked prefill，就是把长上下文拆成多个 chunk，避免长 prompt 一次性占满调度预算。

第五，影响 prefix cache 的价值。长 prompt 如果能命中 prefix cache，收益非常大。比如同一份法律文档、同一份代码仓库摘要、同一个长系统 prompt，复用 KV cache 可以跳过共享部分的 prefill。vLLM 的 Automatic Prefix Caching 文档也强调，它主要减少共享前缀的 prefill 计算；如果主要时间花在生成长答案上，收益就没那么明显。

第六，影响路由。对于长 prompt，请求可能应该被送到有相同 prefix cache 的副本，即使那个副本稍微忙一点；对于很短 prompt，负载均衡可能更重要，因为 locality 的收益不大。

可以把调度器看到的请求特征写成：

```text
request_features:
  prompt_tokens
  cached_prompt_tokens
  uncached_prompt_tokens
  max_new_tokens
  tenant_priority
  deadline_or_slo
  required_model
  required_lora
```

真正影响 prefill 的不是原始 prompt length，而是 `uncached_prompt_tokens`。如果 100k tokens 里 95k tokens 命中 prefix cache，那么它对当前调度轮次的压力可能比一个完全未命中的 20k prompt 还小。

面试里可以这样答：

```text
prompt length 主要影响 prefill、TTFT 和 KV cache 占用。长 prompt 要先处理更多输入 token，才能生成第一个 token，因此会拉高 TTFT；同时它会写入更多 KV cache block，减少可并发服务的请求数。调度器要用 prompt_tokens、cached_prompt_tokens、max_tokens 和当前 token budget 判断请求能不能进入 batch。长 prompt 还可能阻塞短请求，所以生产系统常用 chunked prefill、token budget、prefix-cache-aware routing 和长度分桶队列来控制影响。
```

## Q024. max tokens 对资源预估有什么影响？

**回答：**

这里的 `max tokens` 通常指请求允许生成的最大新 token 数，也可能在某些系统里和 `max_total_tokens = input_tokens + max_new_tokens` 一起出现。它不是一个纯业务参数，而是资源预留参数。

LLM serving 不能只看“当前已经生成了多少 token”。调度器还要问：这个请求最坏情况下可能继续生成多少 token，会占多久 GPU，会继续增长多少 KV cache，会不会把后面的请求挤出去。

`max_tokens` 主要影响四件事。

第一，影响 KV cache 上限预估。decode 每生成一个 token，就会给每层追加新的 K/V。一个请求的最大上下文长度大致是：

```text
max_sequence_tokens = prompt_tokens + max_new_tokens
```

所以 KV cache 预算不能只按 prompt 算。短 prompt 但 `max_tokens` 很大，也可能是资源重请求。TGI 文档对 `max_total_tokens` 的说明很直接：它定义运行中客户端请求的 memory budget；值越大，每个请求占用的 RAM 越多，batching 越不有效。

第二，影响调度器的风险判断。请求不一定真的生成到 `max_tokens`，可能遇到 EOS、stop sequence、tool call 或长度限制提前结束。但服务端必须为最坏情况留余量，否则一批请求同时生成很长，就会把 KV cache 打满。

第三，影响 admission control。系统会设置类似 `max_batch_total_tokens`、`max_num_tokens`、`max_total_tokens` 的上限。一个请求的 `prompt_tokens + max_tokens` 太大，可能被拒绝、排到低优先级队列，或者要求用户降低 `max_tokens`。

第四，影响成本和计费预估。即使实际输出很短，`max_tokens` 很大也意味着系统承诺了更大的服务空间。对交互式请求，过大的 `max_tokens` 会让调度器保守预留资源，降低整体吞吐；对离线任务，则可以接受更大的上限，但要进入不同队列。

注意一个细节：资源预估里有“保守”和“乐观”的差别。

保守预估按 `prompt_tokens + max_tokens` 预留 KV cache。优点是 OOM 风险低，缺点是资源利用率低。很多请求并不会真的生成到上限。

乐观预估按历史输出分布预留，比如同类请求 p90 只生成 200 tokens，即使用户给了 2000 的 `max_tokens`，调度器也先按 200 到 400 估计。优点是吞吐高，缺点是尾部请求可能中途扩容失败，需要 preemption、swap、降级或停止生成。

生产里通常两者结合：

```text
hard_limit:
  prompt_tokens + max_tokens 不能超过模型 context window 和服务端上限

reservation:
  按租户、接口和历史输出分布预留一个较现实的预算

growth_control:
  decode 过程中持续检查 KV cache 水位，超过阈值时降低并发、暂停低优先级请求或提前拒绝新请求

billing / fairness:
  对设置很大 max_tokens 的请求按预留或实际 token 做不同策略
```

面试里可以这样答：

```text
max_tokens 会影响最坏情况下的输出长度、KV cache 增长、GPU 占用时间和 batch token budget。prompt 很短但 max_tokens 很大，请求仍然可能长期占住 decode 资源；prompt 很长但 max_tokens 很小，则主要压力在 prefill 和初始 KV cache。调度器通常用 prompt_tokens + max_tokens 做硬上限和准入控制，再结合历史输出长度做更现实的资源预留。max_tokens 设得过大，会让系统保守预留显存、降低 batching 效率，并增加 OOM 和尾延迟风险。
```

## Q025. 如何避免长请求拖慢短请求？

**回答：**

长请求拖慢短请求，在 LLM serving 里主要有两种形态。

一种是长 prompt 拖慢短请求。长 prompt 的 prefill 很重，如果一次性放进 GPU，会让同一轮或者后续几轮 decode 变慢，短请求即使只问一句话，也要等它处理完。

另一种是长输出拖慢短请求。某些请求 `max_tokens` 很大，decode 持续很多步，占着 KV cache 和 batch slot。短请求来了以后，如果调度器是 run-to-completion 或过度偏向已有 batch，就会排队。

避免这个问题不能靠一句“加优先级”，要从调度、内存和队列三层处理。

第一，prefill chunking。把长 prompt 切成多个 chunk，而不是一次性处理完。TensorRT-LLM 文档提到 chunked context 可以把 context phase 拆成多个 execution iteration，从而让长上下文和 generation phase 更稳定地混合调度。这样长 prompt 仍然会慢，但不会长时间独占 GPU。

第二，decode 优先但不能绝对优先。decode 每步小，但用户正在看流式输出，TPOT 抖动会直接影响体验。因此很多系统会优先服务已在 decode 的请求，保证输出不断流。问题是如果永远优先 decode，新来的短请求 prefill 会饿死。比较好的做法是给 prefill 留预算，比如每轮最多多少 decode tokens，同时给等待队列设置 deadline。

第三，长度分桶队列。短 prompt、长 prompt、超长上下文、离线批处理分开排队。短交互请求走低 TTFT 队列，长文档分析走长上下文队列，批处理走吞吐队列。调度器再按权重分配 GPU 时间，而不是让它们在同一个 FIFO 里互相伤害。

第四，SRPT 或近似剩余时间调度，但要加公平边界。短任务优先可以降低平均延迟，但会伤害长任务。LLM 场景里可以用估算的 remaining tokens、uncached prompt tokens、max_tokens、当前已生成 tokens 来近似剩余成本，同时给长任务年龄提升，避免饥饿。

第五，准入控制。长请求不是不能进系统，而是要在进入前就算清楚资源。超长 prompt、过大的 `max_tokens`、高并发同租户请求，如果会把 KV cache 水位推到危险区，就排队、拒绝、降级或转离线。

第六，KV cache 管理。长请求占的 KV cache 多。系统需要 paged KV cache、block-level 回收、preemption/offload，必要时把低优先级长请求的 KV cache offload 到 CPU，给短交互请求腾出 GPU 显存。FastServe 这类系统就把 token 级抢占和 GPU/host 之间的中间状态换入换出作为降低阻塞的方向。

第七，分池部署。把交互式短请求和离线长任务部署到不同副本组，是最简单也最稳的办法。共享 GPU 时调度很复杂；如果业务上能分池，短请求池只接受小上下文和较小 `max_tokens`，长任务池追求吞吐，故障边界也更清楚。

面试里可以这样答：

```text
避免长请求拖慢短请求，要先区分长 prompt 和长输出。长 prompt 用 chunked prefill、token budget 和长度分桶，避免一次性占满 GPU；长输出用 decode 预算、剩余时间估计、KV cache 配额和必要的 preemption/offload 控制。生产上还会把交互式短请求、长上下文请求和离线批处理分池，给短请求更严格的 TTFT/TPOT SLO，给长请求吞吐优先但限制并发，防止队头阻塞和 KV cache 挤占。
```

## Q026. SJF、SRPT、公平队列在推理调度中各有什么问题？

**回答：**

SJF、SRPT、公平队列都能用来解释 LLM 推理调度，但直接照搬都会出问题。

SJF 是 shortest job first，短任务优先。它的问题是：LLM 请求的“任务长度”很难准确知道。prompt length 可以知道，`max_tokens` 可以知道，但真实输出长度不知道；prefix cache 是否命中、batch 状态、KV cache 水位、采样策略、工具调用、stop sequence 都会改变实际耗时。

如果用 `prompt_tokens + max_tokens` 当 job size，会高估很多请求。用户设置 `max_tokens=4096`，实际可能 100 tokens 就停了。如果因此把它排到后面，延迟会被不必要地放大。

如果只用 prompt length 当 job size，又会低估长输出请求。一个 20 token prompt 让模型写 3000 token 报告，prefill 很短，decode 很长。它不是短任务。

SJF 的另一个问题是长任务饥饿。短请求源源不断进入时，长文档、批处理、代码库分析可能一直被推迟。平均延迟好看，但用户层面的公平性很差。

SRPT 是 shortest remaining processing time，最短剩余处理时间优先。它比 SJF 更细，因为任务执行过程中会更新剩余时间。对 LLM 来说，它看起来很诱人：每生成一个 token 后，都可以重新估算剩余输出 tokens。

但 SRPT 的问题也明显。

第一，剩余输出长度仍然不可知。`max_tokens - generated_tokens` 是上界，不是期望值。模型可能下一步 EOS，也可能继续生成很久。

第二，SRPT 往往需要抢占。LLM 抢占不是普通 CPU 线程切换。你要保留或搬走 KV cache。GPU 上 KV cache 很大，抢占频繁会带来 copy、offload、reload 和显存碎片，TPOT 可能被打坏。

第三，它会偏向短请求，长请求仍然可能饥饿。尤其是交互式系统中短请求不断到达时，长请求的剩余时间一直比新短请求大。

第四，它可能破坏 streaming 稳定性。一个已经开始输出的请求，如果经常被抢占，用户看到的 token 间隔会变得很不稳定。TTFT 可能变好，TPOT 和用户体感变差。

公平队列，比如按租户、用户或请求组做 weighted fair queueing，看起来更适合 multi-tenant。它的问题是：公平的单位很难定义。

如果按请求数公平，一个租户发 10 个短请求，另一个租户发 10 个 100k prompt 请求，资源消耗完全不一样。

如果按 token 数公平，也不够。prefill token 和 decode token 的成本不同；cached prompt token 和 uncached prompt token 的成本不同；同样 1000 tokens，在不同模型和不同 batch 状态下成本也不同。

如果按 GPU 时间公平，观测和归因很难。一个 batch 里混了多个租户，GPU kernel 是一起跑的，怎么把一次 iteration 的 GPU 时间准确分摊到每个租户，并不简单。

因此公平队列通常需要近似资源单位，比如：

```text
uncached_prefill_tokens * prefill_weight
+ decode_tokens * decode_weight
+ reserved_kv_blocks * memory_weight
+ model_load_cost_share
```

再配合优先级、SLO、租户额度和欠账补偿。

LLM serving 里更常见的是混合策略：

```text
短交互请求:
  低 TTFT 优先，限制 max_tokens 和 prompt length

正在 decode 的请求:
  保证一定 TPOT，避免输出断流

长上下文请求:
  chunked prefill，分轮推进

多租户公平:
  按 token、KV cache、并发和 GPU 时间的近似成本做配额

低优先级批处理:
  利用空闲 GPU，必要时可抢占
```

面试里可以这样答：

```text
SJF 的问题是 LLM 请求真实长度不可知，prompt length 和 max_tokens 都只是部分信号，而且会让长请求饥饿。SRPT 能动态看剩余工作量，但剩余输出长度仍然只能估计，抢占还会带来 KV cache 搬移、显存碎片和 TPOT 抖动。公平队列能做多租户隔离，但公平单位难定义：按请求数不公平，按 token 也不够，因为 prefill、decode、cached token、uncached token 成本不同。生产调度通常是混合策略：长度分桶、chunked prefill、decode SLO、租户 token/KV 配额和年龄提升一起用。
```

## Q027. multi-tenant LLM serving 如何做配额和隔离？

**回答：**

multi-tenant LLM serving 的难点在于，租户之间争的不只是 QPS。它们争的是 GPU compute、KV cache 显存、模型权重常驻空间、LoRA adapter slot、batch 位置、queue time、对象存储下载带宽和错误预算。

如果只做“每个租户每秒多少请求”的限流，很容易被绕过。一个租户每秒 1 个请求，但每个都是 100k prompt 加 8k max_tokens，比另一个租户每秒 100 个短请求还重。

比较完整的配额要分几类。

第一，入口配额：

```text
requests per second
concurrent requests
tokens per minute
input tokens per minute
output tokens per minute
max prompt tokens per request
max total tokens per request
```

入口配额解决的是“不要让请求无限进入系统”。其中 token 维度比 QPS 更重要。

第二，运行时资源配额：

```text
max running sequences
max reserved KV cache blocks
max GPU memory share
max batch token share
max LoRA adapters loaded
max model replicas or warm slots
```

这类配额解决的是“进入以后不能把 GPU 和 KV cache 全占满”。尤其是 KV cache，如果不按租户限制，一个长上下文租户可以把整张卡的 block 池吃完。

第三，调度公平：

```text
weighted fair queueing
deficit round robin
priority queue with quota
deadline-aware scheduling
per-tenant prefill/decode budget
```

调度公平要避免两个极端：高优租户饿死低优租户，或者低价值批处理请求影响交互式请求的 SLO。

第四，缓存隔离。KV cache、prefix cache 和 LoRA adapter cache 都可能带来跨租户问题。默认应该把 cache key 纳入 tenant_id、model_version、adapter_id、prompt template version、权限域等信息，不能只按 token prefix 做全局复用。即使两个租户的文本前缀相同，也不一定允许共享内部 KV 表示。

第五，模型和副本隔离。强隔离租户可以独占模型副本或 GPU slice；普通租户共享池。MIG、独立 GPU、独立进程、独立 Kubernetes namespace、独立队列，都可以作为隔离手段。隔离越强，利用率通常越低，但故障和数据边界更清楚。

第六，故障隔离。一个租户触发 OOM、长时间 decode、异常 stop sequence、恶意超长 prompt，不应该让整个模型服务崩掉。服务端要按租户做熔断、降级和隔离重试：

```text
单租户达到错误率阈值 -> 降低并发或拒绝
单租户达到 KV cache 水位 -> 新请求排队
单租户触发连续 OOM -> 降低 max_total_tokens 或转低优先级池
单租户批处理过大 -> 转离线队列
```

第七，观测和计费隔离。每个租户都要能看到自己的 queue wait、TTFT、TPOT、成功率、拒绝率、input/output tokens、KV cache 使用、prefix cache hit rate。没有这些指标，配额很难解释，也很难调。

面试里可以这样答：

```text
multi-tenant LLM serving 不能只按 QPS 配额，要按请求数、并发、input tokens、output tokens、max_total_tokens、KV cache block、batch token budget 和模型常驻资源一起限制。隔离上要分入口限流、运行时资源配额、调度公平、缓存隔离、模型副本隔离和故障隔离。KV/prefix cache 默认应带 tenant_id、model_version、adapter_id 和权限域，避免跨租户复用。调度上可以用 weighted fair queueing 或 deficit round robin，但资源单位要接近真实成本，而不是简单按请求数。
```

## Q028. GPU OOM 时服务应该如何降级？

**回答：**

GPU OOM 不应该只靠重启进程解决。重启是最后手段，因为它会丢掉模型权重热状态、KV cache、prefix cache、CUDA graph 和正在运行的请求。服务应该把 OOM 当成资源管理失败信号，分层降级。

先区分几种 OOM：

```text
模型加载 OOM:
  权重、engine、workspace 放不下

KV cache OOM:
  运行中请求太多、上下文太长、max_tokens 太大，KV block 不够

临时 workspace / activation OOM:
  batch 太大、max_num_tokens 太大、prefill 太重

碎片化 OOM:
  总空闲显存看似够，但没有可用连续空间或 block 池分配失败

泄漏或异常 OOM:
  某条路径没有释放 tensor、请求取消未清理、异常重试堆积
```

不同 OOM 降级方式不一样。

第一，入口降级。发现 KV cache 水位、显存水位或 queue wait 接近危险区时，不要等到 OOM。可以提前拒绝或排队：

```text
返回 429 / resource_exhausted
要求降低 max_tokens 或 prompt length
把长上下文请求转入离线队列
临时降低每租户并发
暂停低优先级租户
```

第二，调度降级。减少单轮 token budget、降低 max_batch_total_tokens、降低 prefill 并发、减少同时运行的 sequences。这样吞吐会下降，但能保住服务。

第三，功能降级。关闭或限制高成本功能：

```text
限制 n-best / parallel sampling
关闭 logprobs 或 top_n_tokens
限制 beam search
限制长 JSON schema / guided decoding
限制超长 RAG 文档
降低 speculative decoding 的额外 draft 开销
```

第四，内存降级。可以驱逐 prefix cache、释放空闲 KV block、卸载低优先级 LoRA adapter、卸载冷门模型副本，把低优先级请求的 KV cache offload 到 CPU。要注意，offload 会引入 PCIe/NVLink 传输成本，不应该无脑启用。

第五，请求级降级。对已经在运行的请求，可以按优先级处理：

```text
高优交互请求:
  尽量保留，保证 TTFT/TPOT

低优批处理:
  可抢占、可重试、可转离线

长输出请求:
  可以提前 stop，并返回 partial result 或 length_limit 说明

超过租户预算请求:
  取消或延后
```

第六，模型级降级。如果某个模型太大或热度突然上升，可以临时切到量化版本、小模型、低并发副本，或者把部分流量转到 CPU/其他 GPU 池。这个策略要由业务确认，因为它可能改变输出质量。

第七，恢复策略。OOM 之后要清理状态，而不是立刻接新流量：

```text
标记 GPU worker unhealthy
停止接新请求
等待正在运行请求完成或取消
释放 KV cache 和 workspace
必要时重建 CUDA context 或重启 worker
warmup 通过后再接流量
保留 OOM 前后的请求特征和显存快照
```

最重要的是，OOM 要可观测。要记录触发 OOM 时的 model_id、prompt_tokens、max_tokens、running requests、waiting requests、KV cache usage、batch token budget、tenant_id、GPU memory used/free、最近是否有模型加载或 cache eviction。

面试里可以这样答：

```text
GPU OOM 时不要只重启。先判断是模型加载 OOM、KV cache OOM、workspace OOM 还是碎片化 OOM。降级可以从入口限流开始：拒绝超长 prompt、降低 max_tokens、减少租户并发；调度上降低 max_batch_total_tokens、prefill 并发和 running sequences；内存上驱逐 prefix cache、释放 KV block、卸载冷门模型或 LoRA，必要时 offload 低优先级请求；请求上优先保交互式高优流量，抢占或延后批处理。重启 worker 是最后手段，恢复前要 warmup 并确认显存水位正常。
```

## Q029. 如何观测 GPU 利用率、显存使用、queue wait、TTFT 和 TPOT？

**回答：**

这些指标要分两层看：GPU 层和 serving 层。

GPU 层回答“硬件忙不忙、显存够不够”。Serving 层回答“请求在哪里等、用户感受到什么延迟、调度器是不是健康”。只看 GPU 层会误判，因为 GPU 利用率高不代表用户体验好；只看 TTFT/TPOT 又不知道瓶颈是在显存、排队还是 batch 策略。

GPU 利用率和显存使用通常从 NVIDIA DCGM、dcgm-exporter、NVIDIA Management Library 或 `nvidia-smi` 来看。在 Kubernetes 环境里，dcgm-exporter 可以把 GPU 指标暴露成 Prometheus metrics，NVIDIA 文档里的示例也是通过 `/metrics` 拉取 DCGM 指标。常见指标包括：

```text
GPU utilization / SM utilization:
  GPU 计算单元忙碌程度

memory used / memory free:
  显存已用和剩余

memory bandwidth utilization:
  decode 阶段可能更接近显存带宽瓶颈

power / temperature / throttling:
  判断是否降频、散热或功耗受限

ECC / XID errors:
  判断硬件错误或驱动异常
```

但 LLM serving 还要看 KV cache，而不是只看总显存。vLLM 的 production metrics 里就有 `vllm:kv_cache_usage_perc`，还有 running/waiting requests、queue time、prefill time、decode time、inter-token latency、request prompt tokens、generation tokens、`request_params_max_tokens` 等指标。这些比裸 GPU 显存更接近调度器真实状态。

queue wait 可以在服务入口和调度器之间打点：

```text
t_arrive:
  请求进入 serving 系统

t_enqueued:
  请求进入调度队列

t_scheduled:
  请求第一次被调度到 GPU

queue_wait = t_scheduled - t_enqueued
```

如果系统有多级队列，还要拆：

```text
gateway queue
router queue
model queue
prefill queue
decode waiting / preempted time
```

vLLM 指标里的 `request_queue_time_seconds` 就是 WAITING 阶段时间；`num_requests_waiting_by_reason` 还会区分 capacity、deferred 等等待原因，这对定位“为什么在等”很有价值。

TTFT 的打点要从用户视角定义清楚：

```text
TTFT = first_token_sent_time - request_received_time
```

如果在网关、router 和 model worker 之间都有排队，TTFT 应该覆盖完整路径，否则你只能看到模型内部 TTFT，无法解释用户为什么等很久。

流式接口里，first token 的时间点通常是服务端写出第一个非空 token、SSE event 或 websocket message 的时间。非流式接口也可以内部记录 first token generated time，只是用户收不到中间结果。

TPOT 可以有两种口径。

第一种是平均每输出 token 时间：

```text
TPOT = decode_duration / number_of_output_tokens
```

第二种是 inter-token latency，也就是相邻 token 输出之间的间隔：

```text
ITL_i = token_i_sent_time - token_(i-1)_sent_time
```

平均 TPOT 适合看吞吐和成本；ITL 的 p95/p99 更适合看流式体验是否断续。vLLM 同时暴露 inter-token latency、decode time 和 generation token 数，通常可以组合出更完整的图。

一个比较实用的仪表盘会放这些图：

```text
GPU:
  SM utilization
  memory used/free
  memory bandwidth
  power/throttle
  XID/ECC errors

Serving capacity:
  running requests
  waiting requests
  waiting by reason
  KV cache usage
  prefix cache hit/query
  preemptions

Latency:
  queue wait p50/p95/p99
  TTFT p50/p95/p99
  TPOT or ITL p50/p95/p99
  E2E latency p50/p95/p99

Workload shape:
  prompt tokens histogram
  generated tokens histogram
  max_tokens parameter histogram
  prefill time and decode time
  per model / tenant breakdown
```

看图时要注意几种典型组合。

GPU utilization 低、queue wait 高：可能调度器被 KV cache、水位、并发上限、模型加载或上游限流卡住，不一定是算力不够。

GPU utilization 高、TTFT 高：可能 prefill 太重、batch 等待窗口太大、长 prompt 阻塞短请求。

GPU utilization 高、TPOT 差：可能 decode batch 太大、KV cache 读带宽瓶颈、多卡通信或抢占太频繁。

显存接近满、waiting by reason 是 capacity：多半是 KV cache 或 batch token budget 到顶。

面试里可以这样答：

```text
GPU 利用率和显存用 DCGM/dcgm-exporter、NVML 或 nvidia-smi 观测；LLM serving 自身要暴露 running requests、waiting requests、KV cache usage、prefix cache hit、queue time、prefill time、decode time、TTFT、TPOT/ITL 和 token 直方图。queue wait 从入队到首次被调度计算；TTFT 从请求到达到第一个 token 返回；TPOT 可以用 decode time 除以输出 token 数，也可以看 inter-token latency。线上一定要按 model、tenant、prompt length 和 cache hit 拆分，否则平均值会掩盖长尾。
```

## Q030. TTFT 和 TPOT 分别是什么意思？

**回答：**

TTFT 是 time to first token，意思是从请求开始到第一个 token 返回的时间。它回答的是：用户多久能看到模型开始响应。

TPOT 是 time per output token，意思是生成阶段平均每个输出 token 花多长时间。它回答的是：模型开始输出之后，后续输出速度怎么样。

这两个指标分别对应 LLM serving 的两个体验阶段。

```text
request arrives
  -> queue
  -> tokenize / route
  -> prefill
  -> first token
  -> decode token 2
  -> decode token 3
  -> ...
  -> finish
```

TTFT 覆盖从请求到第一个 token 的路径，通常包含：

```text
网络和网关
鉴权和限流
调度排队
tokenization
prefix cache 查找
prefill
第一次采样
第一次流式 flush
```

所以 TTFT 对长 prompt、queue wait、冷启动、prefix cache miss 非常敏感。聊天、客服、Copilot、Agent 控制台这类交互式场景，TTFT 很关键。第一个 token 很久不出来，用户会觉得系统卡住了。

TPOT 主要看 first token 之后的 decode 阶段，通常受这些因素影响：

```text
decode batch size
KV cache 读取成本
GPU memory bandwidth
采样和 logits processor
多卡通信
preemption / offload
正在混合调度的 prefill chunk
```

TPOT 对长输出体验很关键。TTFT 很低但 TPOT 很差，用户会看到模型一个字一个字慢慢蹦。非流式接口里，TPOT 会直接影响 total latency；流式接口里，TPOT 或 ITL 会影响输出是否顺滑。

可以用一个简单公式理解：

```text
total_latency
  ~= TTFT + output_tokens * TPOT + postprocess_time
```

这个公式是近似的，因为真实系统里 TPOT 每一步可能不同，batch 会动态变化，网络 flush 也有开销。但它足够说明两者关系：TTFT 决定“多久开始”，TPOT 决定“开始之后跑得快不快”。

还要区分 TPOT 和 ITL。很多系统会把 inter-token latency 作为流式体验指标：

```text
ITL_i = token_i_time - token_(i-1)_time
```

TPOT 更像平均速度，ITL 更像每一步的间隔。平均 TPOT 好看，不代表 p99 ITL 好。如果某些 token 间隔突然拉到几秒，用户会明显感到输出卡顿。

面试里可以这样答：

```text
TTFT 是 time to first token，从请求到第一个 token 返回的时间，主要衡量系统开始响应的速度，受排队、tokenization、prefill、prefix cache、冷启动影响。TPOT 是 time per output token，表示 decode 阶段平均每个输出 token 的耗时，主要衡量开始输出后的生成速度，受 KV cache、decode batch、显存带宽、采样和多卡通信影响。流式应用要同时看 TTFT 和 TPOT/ITL；TTFT 低但 TPOT 高，用户会觉得开始很快但输出很慢。
```

## Q031. 吞吐测 benchmark 时为什么需要控制 prompt/output 分布？

**回答：**

LLM serving 的吞吐 benchmark 不能只写“并发 100、跑 5 分钟、测 tokens/s”。如果不控制 prompt 和 output 分布，测出来的吞吐数很容易没有可比性。

原因是 LLM 请求的成本高度依赖输入长度和输出长度。输入 prompt 主要影响 prefill、TTFT 和初始 KV cache 写入；输出长度主要影响 decode、TPOT、KV cache 增长和请求占用 GPU 的时间。两个 benchmark 的 QPS 一样，但一个是 128-token prompt + 32-token output，另一个是 8k-token prompt + 512-token output，系统压力完全不是一回事。

可以先把一次请求拆开看：

```text
request_cost
  ~= tokenize_cost(prompt)
   + prefill_cost(prompt_tokens)
   + decode_cost(output_tokens, current_kv_length)
   + kv_cache_memory(prompt_tokens + output_tokens)
   + scheduler_overhead
```

其中 `prompt_tokens` 和 `output_tokens` 的分布决定了大部分成本。

如果只控制平均值，也不够。比如两个 workload 的平均 prompt 都是 2k tokens：

```text
workload A:
  所有请求都是 2k prompt

workload B:
  90% 请求是 100 tokens
  10% 请求是 19k tokens
```

平均值相同，但调度难度完全不同。B 里面的长请求会制造队头阻塞、KV cache 突刺和 p99 抖动。吞吐 benchmark 如果只报告平均 tokens/s，会把这些尾部问题藏起来。

output 分布也一样。很多 benchmark 会用固定 `max_tokens`，但真实请求会因为 EOS、stop sequence、工具调用、用户取消而提前结束。如果 benchmark 强行让所有请求都生成固定长度，它能测 decode 吞吐上限，却不一定代表真实业务。反过来，如果不记录实际输出 token 数，只看请求数，就会把“短输出 workload”误判成系统吞吐高。

因此 benchmark 至少要控制和报告这些维度：

```text
input / prompt:
  prompt token p50 / p90 / p95 / p99
  prompt length distribution
  是否有共享 prefix
  是否使用真实 chat template
  tokenizer 版本

output:
  requested max_tokens 分布
  actual generated tokens 分布
  stop / EOS / cancel 比例
  streaming 或 non-streaming

arrival:
  closed-loop 还是 open-loop
  request rate
  burst pattern
  warmup 时间

cache:
  prefix cache hit rate
  model cache hit rate
  KV cache 水位
```

NVIDIA GenAI-Perf 的参数设计也能看出这个问题：它有 synthetic input tokens mean/stddev、output tokens mean/stddev、prefix prompt 数量、prefix prompt length、streaming、request count 等参数。也就是说，专业工具不会只问“多少请求”，而是要求你描述输入输出长度和负载形态。

vLLM 的 benchmark 文档也把 dataset、custom dataset、prefix caching benchmark、long document QA benchmark、request prioritization benchmark 分开，原因也是一样：不同请求形态测的是不同瓶颈。

对 LogServe 这种有 mock LLM 和 checkpoint cache 的项目，控制分布尤其重要。当前 mock LLM 可以稳定模拟 model load、first-token latency、cache hit/miss、scheduler 选择和 LLM event replay，但它不会真实产生 GPU prefill/decode 成本。因此 benchmark 应该明确写清楚：

```text
这是 mock LLM 调度 benchmark，不是 GPU token throughput benchmark。
prompt/output 分布用于控制任务形态和调度压力，不代表真实模型算子成本。
```

面试里可以这样答：

```text
LLM 吞吐 benchmark 必须控制 prompt/output 分布，因为 prompt 决定 prefill、TTFT 和初始 KV cache，output 决定 decode 时间、TPOT 和 KV cache 增长。只报平均 QPS 或 tokens/s 没意义，长尾 prompt 和长输出会显著影响 p99、OOM 风险和调度公平。可靠 benchmark 要报告 input/output token 的 p50/p95/p99、max_tokens、实际生成 token、arrival pattern、cache hit rate、streaming 模式和 tokenizer 版本；否则两次结果可能测的根本不是同一个系统瓶颈。
```

## Q032. mock LLM benchmark 能说明什么，不能说明什么？

**回答：**

mock LLM benchmark 的价值很明确：它能验证 serving 系统的控制面和调度机制，但不能证明真实 GPU 推理性能。

以 LogServe 当前实现为例，mock LLM adapter 会模拟 model load 和 first-token latency。worker 在执行 LLM task 时会写入：

```text
ModelLoadStarted -> ModelLoaded -> LLMCompleted
```

`ReplayLLM` 可以重建 model name/version、worker、cache hit、checkpoint fetch、cache used/capacity、eviction count、model load、first-token latency 和 total latency。控制面还会用 `LLMCompleted` 事件维护 `(model_name, model_version, worker_id)` 维度的 materialized EWMA stats，用于 predicted-latency scheduling。

所以 mock benchmark 能说明这些事情：

```text
模型注册是否生效
worker heartbeat 是否上报 cached models
locality-aware scheduler 是否更偏好已有模型缓存的 worker
predicted-latency scheduler 是否使用历史 EWMA
checkpoint cache 是否 cold fetch、warm hit、LRU eviction
LLM event stream 是否可 replay
idempotency / retry / recovery 是否和普通 task 路径一致
dashboard 和 benchmark 是否能展示 cache hit、cold start、latency
```

这些都是系统机制问题。没有 GPU 也可以测，而且应该先测。因为如果控制面连 cache hit、event replay、调度策略、重启恢复都不稳定，上真实 GPU 只会让问题更难定位。

但 mock benchmark 不能说明这些事情：

```text
真实 prefill 吞吐
真实 decode TPOT
真实 KV cache 显存占用
continuous batching 的 GPU 利用率
PagedAttention / paged KV cache 的收益
CUDA kernel、NCCL、多卡通信开销
真实 OOM、显存碎片、memory bandwidth 瓶颈
tokenizer CPU 热点
真实模型质量、采样行为、EOS 分布
```

原因很简单：mock LLM 没有跑 transformer。它不会做矩阵乘法，不会分配真实 KV cache，不会经历 GPU memory bandwidth，不会被 batch shape 影响，也不会因为长 prompt 真实拉高 prefill 时间。它模拟的是“一个 LLM 请求在系统里的生命周期”，不是“一个真实模型在 GPU 上的执行成本”。

mock benchmark 还有一个容易误解的地方：mock latency 可以调得很像，但仍然不等于真实延迟。比如你把 mock first-token latency 设成 200ms，最多只能验证系统在 200ms 外部服务延迟下的调度和排队行为；它不能证明某个模型在某张 GPU 上 TTFT 就是 200ms。

比较严谨的写法是：

```text
mock benchmark:
  用于验证调度、cache、event、replay、fault injection、backpressure。

real GPU benchmark:
  用于验证 prefill/decode、KV cache、batching、TTFT/TPOT、显存和真实吞吐。
```

LogServe 文档里的边界也应该保持这个说法：当前结果来自单机 mock LLM 和 file-backed checkpoint cache，能说明 locality-aware scheduling 在模拟冷启动和缓存命中下改善了 p95；但不能把它写成“真实 vLLM/GPU 推理性能提升”。vLLM adapter 已经存在，下一步要用真实 GPU、真实模型、真实 prompt/output 分布单独压测。

面试里可以这样答：

```text
mock LLM benchmark 能说明控制面机制是否正确，比如模型注册、worker cache 上报、locality-aware scheduling、predicted latency、checkpoint cache、event replay 和 fault recovery。它不能说明真实 GPU 性能，因为没有真实 prefill、decode、KV cache、CUDA kernel、NCCL、多卡通信和显存压力。LogServe 里 mock LLM 的结果应该表述为机制验证和单机调度实验，不应外推成真实 vLLM/GPU serving 的吞吐或 p99 结论。
```

## Q033. 真实 GPU benchmark 需要记录哪些硬件和软件版本？

**回答：**

真实 GPU benchmark 如果不记录环境，结果基本不可复现。LLM serving 对硬件、驱动、CUDA、推理框架、模型格式、tokenizer、并行策略都很敏感。少一个版本号，别人很可能跑不出同样结果。

硬件层至少要记录：

```text
GPU:
  GPU 型号，例如 A100 80GB、H100 SXM、L40S、4090
  GPU 数量
  每张 GPU 显存容量
  是否 MIG
  GPU interconnect: PCIe / NVLink / NVSwitch
  GPU clock / power limit / persistence mode
  ECC 是否开启

CPU:
  CPU 型号、socket 数、core/thread 数
  NUMA 拓扑
  CPU governor

Memory:
  系统内存容量
  内存通道和 NUMA 绑定

Storage:
  模型权重来源：本地 NVMe、网络盘、对象存储
  磁盘型号或有效读带宽

Network:
  网卡速率
  RDMA / InfiniBand / RoCE 是否参与
  跨节点 benchmark 的拓扑
```

软件层至少要记录：

```text
OS:
  发行版和 kernel 版本
  container runtime
  Docker image digest

NVIDIA stack:
  driver 版本
  CUDA runtime / toolkit 版本
  cuBLAS / cuDNN / NCCL 版本
  TensorRT / TensorRT-LLM 版本

Framework:
  vLLM / TGI / SGLang / TensorRT-LLM 版本
  PyTorch 版本
  Transformers 版本
  tokenizers 版本
  Python 版本

Model:
  model id
  exact revision / commit hash
  checkpoint format
  dtype: fp16 / bf16 / fp8 / int8 / int4
  quantization method
  max model length
  tokenizer revision
  chat template revision
```

推理配置也要记录，否则同一套硬件上仍然不可比：

```text
tensor parallel / pipeline parallel / data parallel
max_num_batched_tokens
max_model_len
gpu_memory_utilization
KV cache dtype
prefix cache 是否开启
chunked prefill 是否开启
speculative decoding 是否开启
CUDA graph 是否开启
batching 策略
streaming 或 non-streaming
sampling 参数: temperature/top_p/top_k/beam/best_of/logprobs
```

workload 本身也要记录：

```text
请求数
warmup 请求数
benchmark 持续时间
open-loop 还是 closed-loop
request rate / arrival pattern
prompt token 分布
output token 分布
实际生成 token 分布
并发数
租户/模型混合比例
cache 预热状态
```

NVIDIA GenAI-Perf 和 vLLM benchmark CLI 都把这些参数显式暴露出来：输入 token 均值/方差、输出 token 均值/方差、streaming、request count、prefix prompts、dataset、request rate 等。它们不是形式主义，而是为了让“吞吐”和“延迟”有上下文。

建议在报告里把环境写成一块固定模板：

```text
Hardware:
  8x H100 SXM 80GB, NVSwitch, dual EPYC ...

Software:
  Ubuntu ..., NVIDIA driver ..., CUDA ..., vLLM ..., PyTorch ...

Model:
  meta-llama/..., revision ..., bf16, tensor_parallel=...

Serving config:
  max_model_len=..., max_num_batched_tokens=..., prefix_cache=...

Workload:
  prompt p50/p95/p99=...
  output p50/p95/p99=...
  request_rate=...
  streaming=true
```

面试里可以这样答：

```text
真实 GPU benchmark 要记录硬件、软件、模型、推理配置和 workload。硬件包括 GPU 型号/数量/显存、NVLink/NVSwitch、CPU、内存、磁盘和网络；软件包括 OS、driver、CUDA、NCCL、TensorRT、vLLM/TGI、PyTorch、Transformers、tokenizers 和容器镜像；模型包括 checkpoint revision、dtype、quantization、tokenizer 和 chat template；推理配置包括并行方式、max_num_batched_tokens、max_model_len、KV cache dtype、prefix cache、chunked prefill、streaming 和采样参数。没有这些信息，TTFT、TPOT、tokens/s 和 p99 都不可复现。
```

## Q034. 模型缓存命中率提高是否一定降低 p99？

**回答：**

不一定。模型缓存命中率提高通常会降低冷启动成本，但 p99 可能不降，甚至可能变差。

先说命中率为什么有帮助。模型权重或 checkpoint 已经在本地磁盘、CPU 内存或 GPU 显存里，请求就不用重新下载、反序列化、搬到 GPU、warmup。对冷启动占比高的 workload，cache hit rate 提高一般会降低平均延迟和部分尾延迟。

但 p99 看的是最慢的 1%。那 1% 不一定是模型加载导致的。

常见反例有几个。

第一，缓存命中把流量集中到热点 worker。locality-aware 调度会偏向已有模型的副本。如果某个模型很热，所有请求都粘到少数缓存副本，queue wait 变长。模型加载省下来了，但排队变成新的 p99 来源。

第二，命中的是模型权重，不是 KV cache。模型已经加载不代表请求的长 prompt 已经 prefill。一个 100k prompt 请求即使命中模型权重，仍然可能造成很高 TTFT。

第三，命中率提高可能伴随 KV cache 压力上升。更多请求被同一批 GPU 接住后，KV cache block 被吃满，调度器开始拒绝、preempt、offload 或降低 batch 并发，尾延迟反而变差。

第四，p99 可能被少量 cache miss 主导。命中率从 90% 提高到 98% 很好看，但剩下 2% 如果是 70B 模型从对象存储冷加载，p99 仍然可能落在 miss 上。尤其请求量不大时，p99 采样很容易被一两个极慢请求决定。

第五，缓存 eviction 可能制造抖动。为了提高热门模型命中率，系统驱逐了冷门模型。冷门模型请求一来，就触发完整冷加载。整体 hit rate 高，低频租户的 p99 很差。

第六，命中率指标本身可能太粗。你要问命中的是什么：

```text
本地磁盘 checkpoint hit
CPU memory weight hit
GPU resident model hit
LoRA adapter hit
prefix/KV cache hit
tokenizer/chat template cache hit
```

这些 hit 对 p99 的影响不一样。磁盘 hit 只能省远端下载；GPU resident hit 才能省 H2D 搬运和部分 warmup；prefix cache hit 才能减少长 prompt prefill。

所以应该把 cache hit rate 和延迟拆开看：

```text
p99 by cache_hit / cache_miss
p99 by model_id
p99 by tenant
p99 by prompt length bucket
queue wait p99
model load p99
prefill p99
decode TPOT p99
KV cache usage p99
eviction count
```

LogServe 当前 locality-aware 实验里 cache hit rate 从 resource-only 的 0.833 提升到 1.000，同时 p95 从 305ms 降到 205ms，这是在单机 mock LLM、3 workers、模拟 checkpoint cache 的特定 workload 下成立。这个结果能说明调度机制有效，但不能推广成“命中率提高一定降低所有线上 p99”。真实 GPU 上还要看负载集中、KV cache、长 prompt 和 batching。

面试里可以这样答：

```text
模型缓存命中率提高不一定降低 p99。它能减少冷加载，但 p99 可能被 queue wait、长 prompt prefill、KV cache 水位、热点 worker、eviction storm 或少量巨大 miss 主导。只看总 hit rate 太粗，还要区分磁盘 checkpoint hit、GPU resident weight hit、LoRA hit、prefix/KV hit。生产上应按 cache hit/miss、model、tenant、prompt length 分桶看 p99，并同时看 queue wait、model load、prefill、TPOT、KV usage 和 eviction。
```

## Q035. 请求级重试在 LLM serving 中可能造成什么副作用？

**回答：**

请求级重试在普通 RPC 里已经要小心，在 LLM serving 里更要小心。因为一次 LLM 请求很贵，而且可能已经产生了部分输出、工具调用、计费和缓存状态。

副作用主要有这些。

第一，放大负载。原请求可能还在 GPU 上跑，客户端或网关又发了一次重试。系统从一个慢请求变成两个慢请求，queue wait 更高，OOM 风险更大。高并发下会变成 retry storm。

第二，重复消耗 token 和 GPU 时间。LLM retry 不是轻量操作。即使 prompt 相同，也可能重新 prefill、重新 decode、重新占 KV cache。对于长 prompt 请求，重试代价很高。

第三，结果可能不一致。采样有随机性，temperature、top_p、并发 batch、浮点非确定性都会让两次输出不同。用户可能看到两个版本，系统也不知道哪个该作为最终结果。

第四，流式输出会出现部分结果问题。用户已经收到前 200 个 token，连接断了。服务端如果从头重试，前缀可能不同；如果继续输出，客户端又不一定知道从哪里接。除非协议支持可验证的 resume，否则不能把重试伪装成无缝继续。

第五，工具调用和外部副作用可能重复。模型生成 tool call 后，下游可能已经创建订单、发送邮件、写数据库。重试如果没有 idempotency key，会重复执行。

第六，缓存会被污染。大量失败重试可能把冷门模型、LoRA adapter、prefix cache、KV cache 塞进缓存，挤掉真实热点。重试本来是容错，最后变成缓存抖动来源。

第七，计费和审计会变复杂。用户发了一次请求，系统内部跑了三次。到底按一次还是三次计费，日志里哪个 completion 是最终输出，都要有明确规则。

第八，重试可能掩盖真实故障。如果 OOM、上下文超限、模型不存在、adapter 加载失败，这些错误不该盲目重试。重试只会浪费资源。

比较稳的策略是：

```text
只对明确 transient 的错误重试:
  网络断开、上游 503、短暂队列超时、worker crash before start

不对确定性错误重试:
  context too long、bad request、model not found、tenant quota exceeded

使用 idempotency key:
  同一个业务请求的重复提交必须可识别

设置 retry budget:
  每个租户、每个请求、每个时间窗口都有上限

使用 backoff + jitter:
  避免所有客户端同时重试

取消原请求:
  重试前尽量确认原请求没有继续占 GPU

区分 queued / running / streamed:
  已经开始流式输出的请求不能当普通 RPC 重试
```

LogServe 已经有 LLM idempotency key 和 log-first event 流，这是做安全重试的基础。更进一步的生产实现要记录请求是否已经 `ModelLoaded`、是否已经 first token、是否已经产生外部 tool call，然后决定能不能重试。

面试里可以这样答：

```text
LLM 请求级重试会放大 GPU 负载、重复 prefill/decode、占用 KV cache，并可能产生不同输出。流式请求已经发出部分 token 后，重试还会带来续接语义问题；如果请求里有 tool call，重试可能重复执行外部副作用。生产上只能对 transient 错误做有限重试，必须有 idempotency key、retry budget、backoff+jitter，并区分 queued、running、已经开始 streaming 的请求。context too long、quota exceeded、model not found 这类确定性错误不该重试。
```

## Q036. 流式输出失败后能否重试？

**回答：**

能重试，但不能默认“无感续上”。这句话要分清楚。

流式输出的特点是：服务端生成一点，客户端就看到一点。OpenAI 的 streaming 文档里也能看到，Chat Completions 的流式响应是增量 chunk，delta 里可能是 role、content token 或空内容；Responses API 也会持续产生 typed event。也就是说，一旦流已经开始，输出就不是一个原子结果了。

失败可能发生在几种位置：

```text
第一个 token 前失败:
  客户端还没看到内容，比较像普通请求失败。

输出中途网络断开:
  客户端已经看到部分 token，服务端可能还在生成。

服务端 worker crash:
  KV cache 和 sampler state 可能已经丢失。

下游代理超时:
  model worker 可能仍在跑，网关已经断了。

客户端主动断开:
  这更像取消，不一定应该重试。
```

如果第一个 token 前失败，可以按普通 transient 错误重试，前提是原请求没有继续运行，或者有 idempotency 机制能识别重复。

如果已经输出了一部分，情况就复杂了。不能简单从头重试然后把新输出接到旧输出后面，因为第二次生成的前缀可能不同。即使 temperature=0，也可能因为并发 batch、浮点细节、不同后端版本、工具调用状态不同而不完全一致。

真正的“续上”需要很强的条件：

```text
服务端保存了请求状态
KV cache 没丢
sampler RNG state 没丢
已经发送的 token 序列可校验
客户端能告诉服务端已收到到哪个 offset
协议支持 resume token / stream id
后端保证同一请求继续生成，而不是重新采样
```

大多数 OpenAI-compatible LLM serving 系统并不提供这种严格 resume。它们通常只能做三种处理：

第一，告诉用户流失败，保留 partial output。客户端可以显示“回答中断”，让用户选择重新生成。

第二，从头重试。重试时要把 partial output 当成 UI 上的旧内容，不要假装它和新内容是同一次 completion 的连续结果。

第三，应用层续写。把已收到内容放回 prompt，要求模型“继续”。这不是协议级 retry，而是一个新的请求。它可能重复、偏题或改变风格，只能作为产品层 fallback。

对工具调用更要谨慎。如果 partial stream 已经包含 tool call 的一部分，不能在解析不完整 JSON 后盲目执行；如果 tool call 已经执行，重试必须带 idempotency key，避免重复副作用。

服务端实现上还要处理取消。如果客户端连接断了，worker 应该尽快感知 `context canceled`，停止 decode，释放 KV cache。否则客户端以为失败并重试，原请求还在 GPU 上跑，系统会双倍消耗资源。

面试里可以这样答：

```text
流式输出失败后可以重试，但不能默认无缝续传。第一个 token 前失败，可以按普通 transient 错误重试；已经输出部分 token 后，用户已经看到 partial result，重新请求可能生成不同文本，不能直接拼接。真正 resume 需要保存 KV cache、sampler state、已发送 token offset 和 stream id，大多数 OpenAI-compatible 服务不提供。生产上通常返回 partial output、允许用户重新生成，或用应用层“继续写”作为新请求；同时要取消原请求，避免旧请求继续占 GPU。
```

## Q037. LLM serving 中如何处理取消请求？

**回答：**

取消请求不是简单把 HTTP 连接关掉。LLM serving 里取消要从入口一路传到调度器、worker、模型后端和 KV cache 管理，否则资源不会真正释放。

取消来源通常有这些：

```text
用户关闭页面
客户端主动 cancel
HTTP/SSE/WebSocket 连接断开
网关超时
租户超出预算
服务端主动降级或抢占
上游 workflow 被取消
```

处理取消时要先看请求状态。

如果请求还在队列里，最简单：从调度队列删除，记录 canceled 状态，不要再分配给 worker。这个路径要快，否则大量取消请求会堆在队列里，污染 queue wait。

如果请求已经进入 prefill，不能马上在任意位置打断 GPU kernel，但可以设置 cancellation flag，让当前 iteration 结束后不再继续调度它。prefill 阶段取消后，要释放已经分配的 KV block。

如果请求已经在 decode，应该在 token 边界或 iteration 边界停止。decode 是逐 token 推进的，比较适合检查取消信号。停止后要释放 KV cache、batch slot、stream writer 和统计对象。

如果请求正在模型加载或 adapter 加载，取消更麻烦。模型加载可能是共享动作：一个请求触发模型加载，后面很多请求都会用。此时不能因为单个请求取消就把整个模型加载杀掉，除非没有其他等待者。LoRA adapter 加载也类似。

取消还要写清楚语义：

```text
用户取消:
  不算系统失败，通常不触发重试。

超时取消:
  可能算 deadline exceeded，需要看是否可重试。

服务端抢占:
  低优先级请求可能被恢复或重新排队。

worker crash:
  这是失败，不是正常取消。
```

观测也要区分。把 cancel 全部算成 error rate，会误导 SLO；完全不记录又会看不到资源浪费。建议指标拆成：

```text
canceled_before_schedule
canceled_during_prefill
canceled_during_decode
canceled_after_stream_started
tokens_generated_before_cancel
kv_blocks_released_after_cancel
cancel_to_resource_free_ms
```

OpenAI Responses API 有显式的 cancel response endpoint，这说明长运行生成任务在 API 层也需要取消语义。OpenAI streaming 文档则说明流式响应是增量事件；客户端断开后，服务端如果不停止生成，就会造成后台浪费。

结合 LogServe 当前实现，worker 侧 mock sleep 和 vLLM HTTP 调用都走 Go `context.Context`，这是取消传播的基础。后续如果要更完整，可以在 LLM event stream 里增加 `LLMCanceled` 或 `LLMAborted`，并在 ReplayLLM 中重建取消阶段和已生成 token。

面试里可以这样答：

```text
LLM 取消要按请求阶段处理。排队阶段直接从队列移除；prefill 阶段在 iteration 边界停止并释放已分配 KV；decode 阶段在 token 边界停止，释放 KV cache、batch slot 和 stream；模型或 LoRA 加载阶段要判断加载动作是否被其他请求共享。取消要通过 context 从 HTTP/gRPC 传到 scheduler、worker 和后端，并记录 canceled_before_schedule、during_prefill、during_decode、tokens_generated_before_cancel、resource_free_ms。用户取消不应简单算作系统失败，也不应触发盲目重试。
```

## Q038. 如何设计 LLM adapter 抽象以支持 OpenAI-compatible API？

**回答：**

LLM adapter 抽象的目标不是把 OpenAI API 原样散落在系统各处，而是把外部协议和内部调度执行解耦。

比较稳的做法是中间放一个内部 IR，也就是统一请求对象：

```text
LLMRequest:
  request_id
  tenant_id
  model_name
  model_version
  adapter_name
  messages / prompt
  tools
  tool_choice
  response_format
  max_tokens
  temperature
  top_p
  stop
  stream
  metadata
  idempotency_key
```

OpenAI-compatible `/v1/chat/completions`、Responses API、内部 SDK 的 `llm_generate()`，都先转换成这个 IR。调度器只关心 model/version/adapter、prompt token 估算、max_tokens、stream、priority、tenant，而不直接关心 HTTP 字段长什么样。

adapter 接口可以分两层。

第一层是执行接口：

```text
type LLMAdapter interface {
  Generate(ctx, request) (response, usage, error)
  Stream(ctx, request) (<-chan event, error)
  CountTokens(ctx, request) (tokenStats, error)
  Health(ctx) error
}
```

第二层是能力描述：

```text
AdapterCapabilities:
  supports_streaming
  supports_tools
  supports_json_schema
  supports_logprobs
  supports_lora
  supports_embeddings
  supports_cancel
  tokenizer_id
  max_context_tokens
```

这样做的好处是，系统能在请求进入队列前就判断某个 adapter 能不能处理这个请求。比如 mock adapter 不支持 tool calling，vLLM adapter 可能支持 OpenAI-compatible chat completions，但不一定支持所有 Responses API 事件；某些后端不支持 logprobs 或 structured output。

OpenAI-compatible 需要特别注意几个点。

第一，消息格式和 chat template。OpenAI chat messages 是 role/content 结构，底层模型往往需要特定 chat template。adapter 要负责把 messages 变成后端能理解的 prompt，或者交给后端的 tokenizer/template 处理。不能让业务层自己拼字符串。

第二，流式事件语义。OpenAI streaming 是增量 chunk/event。内部 adapter 最好输出统一事件：

```text
started
delta_text
delta_tool_call
usage_update
completed
error
canceled
```

再由 HTTP 层转换成 OpenAI-compatible SSE chunk。不要让 worker 直接拼 SSE 文本，否则后续支持 WebSocket、gRPC、Responses events 会很痛苦。

第三，错误映射。内部错误要映射成稳定的外部错误：

```text
model not found -> 404 或 invalid_request
context too long -> 400
quota exceeded -> 429
queue timeout -> 503 / 504
worker canceled -> canceled
backend unavailable -> 503
```

第四，usage 和 token accounting。OpenAI-compatible 响应通常要有 prompt_tokens、completion_tokens、total_tokens。真实后端可以返回；mock adapter 或不返回 usage 的后端则要用对应 tokenizer 估算，并标记口径。

第五，取消和重试。adapter 必须接受 context cancellation，并在底层 HTTP、gRPC 或 engine request 里停止请求。否则 OpenAI-compatible 层返回取消，GPU 还在继续跑。

第六，兼容不等于全量支持。vLLM 文档有 OpenAI-compatible server，TGI 也提供 OpenAI-compatible 路径，但不同后端支持的字段不完全一样。adapter 设计要允许部分支持和显式拒绝，而不是默默忽略字段。

LogServe 当前的 vLLM adapter 已经调用 `/v1/chat/completions`，这是第一步。更完整的抽象可以把 `mock` 和 `vllm` 都放在同一个 adapter interface 下：mock 用于机制测试，vLLM 用于真实后端；控制面只保存 adapter 名称、model version 和 max tokens，不和具体 HTTP 协议耦合。

面试里可以这样答：

```text
LLM adapter 应该把 OpenAI-compatible 协议转换成内部统一 IR，而不是让调度器直接依赖 HTTP 字段。IR 包含 model、version、messages、tools、response_format、max_tokens、sampling、stream、tenant 和 idempotency_key。adapter 暴露 Generate、Stream、CountTokens、Health、Cancel 等能力，同时声明是否支持 streaming、tools、logprobs、LoRA、JSON schema 和最大上下文。HTTP 层负责把内部事件映射成 OpenAI-compatible chunks，adapter 负责调用 mock、vLLM、TGI 或其他后端，并做错误、usage、取消和 token 口径映射。
```

## Q039. 不同 tokenizer 对请求成本估算有什么影响？

**回答：**

tokenizer 会直接影响成本估算，因为 LLM serving 里的很多资源都按 token 计。

同一段文本，用不同 tokenizer 得到的 token 数可能不同。英文、中文、代码、JSON、emoji、空格、换行、特殊符号，在 BPE、SentencePiece、tiktoken 或某个模型自带 tokenizer 下切分方式都可能不同。Hugging Face Transformers 文档也把 tokenizer 定义为准备模型输入、把字符串切成 subword token、转换成 id、管理 special tokens 的组件；这说明 tokenizer 不是一个可替换的小工具，它和模型输入格式绑定。

token 数估错会带来很多问题。

第一，准入控制错误。系统以为请求只有 4k tokens，实际 tokenizer 后是 6k，就可能超过 context window，进入模型后才失败。反过来，估太保守会拒绝本来可以服务的请求。

第二，KV cache 预算错误。KV cache 大小和 token 数近似线性相关。低估 prompt tokens 会导致显存预留不足，运行中 OOM；高估会降低并发和 batch 利用率。

第三，调度成本错误。调度器用 prompt length 预测 prefill，用 max_tokens 预测 decode。如果 tokenizer 不匹配，长短请求分类会错，SJF/SRPT、长度分桶、token budget 都会错。

第四，计费错误。很多服务按 input/output tokens 计费。tokenizer 不一致会导致账单和后端实际消耗不一致。

第五，prefix cache 判断错误。prefix cache 的共享边界通常在 token 级别。两个字符串看起来前缀相同，但 chat template、special tokens 或 tokenizer version 不同，token 前缀可能不同，不能复用同一份 KV cache。

第六，OpenAI-compatible 适配会出错。客户端以为自己发的是 chat messages，服务端实际要先套 chat template。template 增加的 system/user/assistant special tokens 也会消耗上下文。只按用户原始文本估算，会漏掉模板 token。

所以生产上应该遵守几条原则：

```text
使用模型绑定的 tokenizer，而不是全局 tokenizer。
tokenizer revision 要和 model revision 一起记录。
成本估算要包含 chat template 和 special tokens。
工具调用、JSON schema、system prompt、RAG 文档都要计入。
对不确定 tokenizer 的请求保守预留 margin。
最好由 serving 后端或同版本 tokenizer 做 server-side counting。
benchmark 报告必须记录 tokenizer 版本。
```

OpenAI cookbook 里专门有 tiktoken 计数示例，原因也是一样：对 OpenAI 模型，应该使用对应 encoding 估算 token。Hugging Face 模型则应使用该模型自己的 tokenizer 和 chat template。

对 LogServe 当前实现来说，如果后续要把 mock/vLLM adapter 扩展成更真实的成本模型，就不能只用字符串长度。应该把 `prompt_tokens`、`max_tokens`、`tokenizer_id`、`chat_template_version` 写入 LLM event 或 stats。否则 predicted latency 只能看历史总延迟，无法区分真实长短请求。

面试里可以这样答：

```text
不同 tokenizer 会让同一段文本得到不同 token 数，从而影响 context limit、KV cache 预算、prefill 成本、batch token budget、调度分桶和计费。chat template、special tokens、tool schema、JSON 和多语言文本都会改变 token 数。服务端应该使用模型绑定的 tokenizer，并把 tokenizer revision 和 model revision 一起记录；成本估算要包含模板和特殊 token。不能用字符数或一个全局 tokenizer 估算所有模型，否则会低估 OOM 风险或过度拒绝请求。
```

## Q040. LoRA adapter 缓存和基础模型缓存如何协同？

**回答：**

LoRA adapter 缓存和基础模型缓存不是同一层，但必须协同调度。

基础模型缓存保存的是 base model 权重，体积大、加载慢、生命周期长。LoRA adapter 保存的是在 base model 上叠加的低秩增量权重，通常小很多，但数量可能非常多，常见于多租户、个性化模型、任务专用微调。

可以这样理解：

```text
base model:
  meta-llama/Llama-...
  大，加载慢，适合常驻

LoRA adapter:
  tenant-A/sql-lora
  小，加载快，但数量多，需要按需加载和驱逐

实际请求:
  base model + selected LoRA adapter
```

vLLM 的 LoRA 文档里也体现了这个关系：先启用 base model 的 `enable_lora`，再通过 `LoRARequest` 或 OpenAI-compatible server 的 `--lora-modules` 指定 adapter；文档还提到 server entrypoint 支持 `max_loras`、`max_lora_rank`、`max_cpu_loras` 等配置。也就是说，LoRA 是挂在 base model serving 上的可切换增量，而不是独立完整模型。

协同的第一件事是 cache key 设计。LoRA cache key 不能只有 adapter 名字，至少要包含：

```text
base_model_id
base_model_version / revision
adapter_id
adapter_revision
tenant_id 或权限域
lora_rank
dtype
quantization / target modules
```

同名 adapter 挂在不同 base model 上不一定兼容；同一个 adapter 更新后 revision 不同，也不能混用。

第二件事是路由优先级。一个请求最好去同时命中 base model 和 LoRA adapter 的 worker。如果没有同时命中，可以按成本排序：

```text
base + adapter 都命中:
  最好，直接服务

base 命中，adapter 未命中:
  通常可接受，只需加载较小 adapter

base 未命中，adapter 命中:
  实际上很少有意义，因为 adapter 不能脱离 base 使用

base 和 adapter 都未命中:
  最贵，可能需要排队或路由到别处
```

第三件事是 eviction 策略。base model 大但复用高，通常更适合 pin 热点模型；LoRA adapter 小但数量多，更适合按租户、热度、最近使用、加载成本做缓存。不能为了保留大量低频 LoRA 把 base model 挤掉，因为 base miss 的代价通常更大。

第四件事是多租户隔离。LoRA 往往是租户私有资产，adapter cache 不能跨权限域乱用。即使两个租户 adapter 文件内容相同，也要考虑访问权限、审计和数据边界。

第五件事是显存和 CPU 内存分层。常见做法是：

```text
GPU:
  当前活跃 LoRA adapter

CPU memory:
  最近使用或即将使用的 adapter

disk / object store:
  冷 adapter
```

加载路径可以从对象存储到本地磁盘，再到 CPU，再到 GPU。`max_cpu_loras` 这类参数就是为了在 CPU 层多留一些 adapter，减少反复远端拉取。

第六件事是 batching。不同 LoRA adapter 的请求能不能放在同一个 batch，取决于推理框架实现。支持 multi-LoRA batching 的系统可以在一个 batch 里处理不同 adapter；不支持时，adapter 维度会降低 batching 效率。因此调度器要知道后端能力，而不是假设所有 adapter 都像普通请求一样可混批。

第七件事是 OpenAI-compatible 暴露方式。很多系统会把 LoRA adapter 暴露成一个“model name”，例如用户请求 `sql-lora`，服务端内部映射到 `base_model + adapter`。对外简单，对内必须保存映射关系，否则计费、权限、缓存和观测都会混乱。

面试里可以这样答：

```text
base model cache 和 LoRA adapter cache 是两层。base model 大、加载慢、复用高，通常常驻或优先保留；LoRA adapter 小、数量多、租户相关，适合按需加载和分层缓存。调度时最好路由到同时命中 base 和 adapter 的 worker；如果只能命中 base，也比完全冷加载好。LoRA cache key 必须包含 base model/version、adapter revision、tenant、rank、dtype 等信息，不能只按 adapter 名称。eviction 上一般优先保护热点 base model，再在 LoRA 层按热度、租户配额和加载成本驱逐。
```

## Q041. prefix cache 适合什么 workload？

**回答：**

prefix cache 适合“很多请求共享同一段较长前缀”的 workload。它省的是 prefill 阶段的重复计算：同一模型、同一 tokenizer、同一 chat template 下，如果请求开头的一大段 token 完全相同，服务端可以复用这段 prefix 已经算出的 KV cache，后续只处理新增 token。

最典型的场景是：

```text
固定系统提示词 + 固定工具 schema + 固定 few-shot 示例 + 不同用户问题
固定长文档 / 代码仓库上下文 + 不同查询
固定任务说明 + 大批量分类样本
固定 RAG 检索结果 + 多个后续追问
同一 agent workflow 的长工具说明和格式约束
```

OpenAI 的 prompt caching 文档把命中条件说得很直接：缓存命中需要 exact prefix match，静态内容应该放在 prompt 开头，变量内容放在末尾；对于较长 prompt，系统会按前缀 hash 做 cache routing。vLLM 的 Automatic Prefix Caching 也是同一个思想，只是更多暴露在自托管推理系统的 KV cache 管理和路由层。

所以它特别适合这些 workload：

1. 长 prompt，短或中等 output。prefix cache 主要减少 prefill，prompt 越长、共享部分越多，收益越明显。
2. 前缀稳定。系统 prompt、工具定义、few-shot、文档块不要每次都插入随机 ID、时间戳、trace 字段，否则 token 级前缀被破坏。
3. 有 locality-aware routing。缓存通常在某台机器、某块 GPU 或某组 KV cache block 上，调度器要尽量把相同 prefix 的请求送到同一处。
4. 租户边界清楚。即使两个租户的 prompt 字面相同，也要确认缓存 key 是否允许跨租户复用；企业场景一般要把 tenant、model、adapter、权限域放进 cache namespace。

它不适合这些场景：

```text
每个请求都很短:
  prefill 本身不贵，cache lookup 和路由复杂度可能不划算

每个请求前缀都不同:
  例如自由聊天、个性化上下文、每次检索结果都变

变量字段放在开头:
  user_id、timestamp、request_id 一开头就变化，后面再长也很难命中

主要瓶颈在 decode:
  output 很长时，prefix cache 只减少开头成本，TPOT 仍然由 decode 决定

强隔离或高敏数据场景:
  缓存复用必须先过数据边界审查，不能为了命中率牺牲隔离
```

一个容易踩的坑是“看起来相同”和“token prefix 相同”不是一回事。空格、换行、chat template、tool schema 排序、JSON 字段顺序、tokenizer 版本都会影响 token 序列。调度器如果只按原始字符串或业务 ID 判断，可能会高估命中率。

在 LogServe 这类有 worker-local model cache 语义的系统里，prefix cache 可以理解成更细的一层 locality：模型权重命中只能说明 worker 已经有模型；prefix cache 命中还要求同一模型下的 prompt 前缀也在那个执行位置附近。前者减少 model load，后者减少 prefill。

面试里可以这样答：

```text
prefix cache 最适合长而稳定的共享前缀，例如固定 system prompt、工具 schema、few-shot、RAG 长文档或代码上下文，然后尾部是不同用户问题。它主要降低 prefill 和 TTFT，不直接解决长 output 的 decode 成本。设计时要把静态内容放前面、变量内容放后面，并让路由器保持 prefix locality。反过来，短 prompt、每次前缀都不同、变量字段插在开头、强租户隔离不清楚的 workload，prefix cache 收益很小，甚至会带来路由和安全复杂度。
```

## Q042. speculative decoding 如何影响延迟和吞吐？

**回答：**

speculative decoding 的核心做法是先让一个更便宜的 proposer 生成候选 token，再由目标模型批量验证。proposer 可以是小 draft model、EAGLE、MTP、n-gram、suffix decoding 或自定义 proposer。目标模型如果一次验证多个 token，其中一部分被接受，就等于少走了多次逐 token decode。

它最直接影响的是 TPOT 和 inter-token latency，而不是完整的 prefill 成本。vLLM 文档也把 speculative decoding 定位为在中低 QPS、memory-bound workload 下减少 inter-token latency。原因是 decode 阶段经常受显存带宽和模型权重读取限制，一次只生成一个 token 时 GPU 算力未必吃满；如果能让目标模型一次验证多个候选 token，就可能把多步 decode 合并成更少的目标模型 forward。

对延迟的影响可以分成几段看：

```text
TTFT:
  如果 TTFT 主要由 queue wait 和 prefill 决定，speculative decoding 改善有限。
  如果首 token 后还要很快吐出后续 token，用户体感会明显变好。

TPOT:
  候选 token 接受率高时，平均每个输出 token 的目标模型调用次数减少，TPOT 下降。

尾延迟:
  如果 draft 开销稳定、接受率稳定，p95/p99 可能下降。
  如果接受率波动大、batch 里请求差异大，尾部反而可能更乱。
```

对吞吐的影响没有一句话答案。它可能提高吞吐，也可能降低吞吐。

会提高吞吐的情况：

1. 目标模型 decode 是瓶颈，proposer 很便宜。
2. 候选 token 接受率高，一次验证能推进多个 token。
3. batch size 不大，系统处在 memory-bound、低到中等 QPS 区间。
4. 采样参数比较稳定，温度不太高，任务输出模式可预测。
5. 框架对 speculative decoding 的 batching、KV cache 和 CUDA graph 支持成熟。

会降低吞吐的情况：

```text
draft model 太重:
  多跑一个模型的成本抵消了被接受 token 的收益

接受率低:
  目标模型验证后大量候选被拒绝，白白增加 proposer 开销

高 QPS 已经把 GPU 填满:
  再加 draft 工作可能挤掉正常 batch

prompt 很长但 output 很短:
  主要成本在 prefill，decode 优化空间小

请求异质性强:
  不同 speculative depth、不同 proposer、不同采样参数让 batch 组织更难
```

TensorRT-LLM 的 speculative decoding 文档列了 n-gram、Medusa、EAGLE、Lookahead、PARD 等多种方法。vLLM 的文档也把方法分成模型型和轻量型：EAGLE、MTP、draft model 通常延迟收益更强，但资源开销也更明显；n-gram 和 suffix decoding 简单，收益较温和，胜在不一定需要额外模型。

工程上要重点观测这些指标：

```text
acceptance rate:
  候选 token 被目标模型接受的比例

accepted tokens / rejected tokens:
  OpenAI usage 中也有类似 accepted_prediction_tokens / rejected_prediction_tokens 字段

draft latency:
  proposer 生成候选的时间

verify latency:
  目标模型验证候选的时间

TPOT / tokens per second:
  最终用户看到的真实输出速度

GPU memory and batch occupancy:
  draft 模型或额外 KV cache 是否挤占主模型容量
```

面试里可以这样答：

```text
speculative decoding 用便宜 proposer 先猜多个 token，再让目标模型批量验证。接受率高、draft 足够便宜、decode 是瓶颈时，它能降低 TPOT，提高用户看到的输出速度，也可能提高吞吐。它不一定显著改善 prefill 主导的 TTFT。接受率低、draft 太重、高 QPS 已经满载、batch 很异质时，额外 proposer 工作会吃掉 GPU 和显存，吞吐可能下降。所以不能只看是否启用了 speculative decoding，要看 acceptance rate、draft/verify latency、TPOT、GPU 利用率和端到端吞吐。
```

## Q043. 量化会如何影响内存、吞吐和精度？

**回答：**

量化是用更低精度表示权重、激活或 KV cache。它通常用内存换精度：模型占用更小，显存带宽压力降低，但数值误差变大。vLLM 的量化文档说得很简洁：quantization trades off model precision for smaller memory footprint。真正面试时要把“内存、吞吐、精度”拆开讲，因为三者不总是同向变化。

对内存的影响最稳定：

```text
FP16 / BF16 weights:
  每个参数约 2 bytes

INT8 weights:
  每个参数约 1 byte，再加 scale/zero-point 等元数据

INT4 weights:
  每个参数约 0.5 byte，再加元数据

FP8 KV cache:
  KV cache 从 FP16/BF16 降到 FP8，可显著减少长上下文和大 batch 的 KV 占用
```

内存下降带来的直接好处是：同一张 GPU 可以放更大的模型、更长上下文、更多并发序列，或者给 KV cache 留更多空间。对于 LLM serving，KV cache 经常是 decode 阶段的关键资源，所以只量化权重和量化 KV cache 的效果不同。权重量化解决“模型能不能放下”和权重带宽；KV cache 量化解决“同时服务多少 token”和长上下文容量。

对吞吐的影响要看瓶颈在哪里：

1. 如果瓶颈是显存容量，量化后 batch 可以变大，吞吐通常提高。
2. 如果瓶颈是显存带宽，低精度权重和 KV cache 会减少读取量，吞吐可能提高。
3. 如果瓶颈是 dequant kernel、调度、CPU、网络或 tokenizer，量化不一定提高吞吐。
4. 如果硬件没有合适的低精度 kernel，INT4/INT8 可能需要频繁反量化，速度不升反降。
5. 小 batch、短 prompt、短 output 下，量化的收益可能被框架 overhead 吃掉。

对精度或质量的影响最难一句话判断。权重量化可能影响困惑度、事实性、代码生成、数学推理、工具调用 JSON 格式、长尾 token 选择。KV cache 量化则可能影响长上下文注意力质量，尤其在很长 context、精确引用、代码补全、结构化输出这类任务里要单独测。vLLM 的 FP8 KV cache 文档也提到 scale calibration：没有校准、随机 token 校准、用数据集校准，精度风险是不一样的。

量化选型时可以按风险从低到高理解：

```text
FP16/BF16:
  稳定，显存大

FP8:
  对 Hopper/Ada 等硬件更友好，吞吐和显存有较好折中

INT8:
  通常还能保持较好质量，但要看方法和校准

INT4:
  显存省得多，质量和 kernel 支持风险也更高

KV cache FP8:
  长上下文和高并发收益明显，但要测长上下文质量
```

线上系统不要只看 benchmark 里的 tokens/s。至少要做三类验证：

```text
性能:
  TTFT、TPOT、吞吐、显存峰值、OOM 率

质量:
  业务 eval、结构化输出通过率、工具调用准确率、长上下文召回

稳定性:
  不同 prompt/output 分布、不同 batch size、不同 GPU 型号上的退化
```

在 LogServe 这类抽象里，model metadata 最好记录 dtype、quantization method、KV cache dtype、adapter dtype。否则调度器只知道“模型名”，不知道这个 worker 上的模型能承载多大 batch，也不知道一次预测的资源估算该按 FP16 还是 INT4 算。

面试里可以这样答：

```text
量化通常减少显存和带宽压力，让更大的模型、更长上下文或更大的 batch 可以放进 GPU，所以吞吐可能提高，OOM 风险也会下降。但吞吐不是必然提高，因为 dequant 开销、kernel 支持、batch size 和实际瓶颈都会影响结果。精度上，低比特权重或 FP8 KV cache 可能影响事实性、推理、代码、JSON 工具调用和长上下文质量。工程上要把 weight dtype、activation dtype、KV cache dtype、量化方法、校准数据和硬件 kernel 一起记录，并用业务 eval 验证，而不是只看显存省了多少。
```

## Q044. 如何设计多区域 LLM serving 的路由策略？

**回答：**

多区域 LLM serving 的路由策略不能照搬普通 Web 服务的“就近加权轮询”。LLM 请求的执行时间长、显存占用高、KV cache 有 locality，跨区域多几十到两百毫秒网络延迟，在普通 Web 请求里可能很致命，但在一个持续数秒的 LLM 请求里有时是可接受的。SkyLB 这类多区域 LLM 负载均衡研究也指出，多区域路由要同时考虑 KV cache locality 和负载不确定性。

一个可落地的路由分层是：

```text
Global routing:
  选择候选区域，考虑用户地理位置、合规、区域健康、容量、成本

Regional routing:
  在区域内选择模型池，考虑模型版本、adapter、GPU 类型、队列长度

Worker routing:
  在具体 worker/GPU 上考虑 model cache、prefix cache、KV cache、batch slot
```

路由决策至少要看这些维度：

1. 用户延迟。优先选离用户近、网络质量稳定的区域，尤其关注 TTFT。
2. 数据合规。某些租户数据不能出区域，合规条件比性能优先级更高。
3. 模型可用性。目标 base model、LoRA adapter、tokenizer、量化版本必须一致。
4. GPU 容量。看活跃序列数、KV cache 剩余、显存水位、queue wait，而不是只看机器是否存活。
5. cache locality。相同 session、相同 prompt prefix 或相同 adapter 的请求，尽量保持粘性。
6. 负载均衡。某区域排队过长时，可以把部分请求推到远端空闲区域。
7. 故障语义。区域故障、模型池降级、流式连接断开时，要定义是否可以重试或接管。

多区域路由最常见的矛盾是 locality 与 load balancing。把同一类请求都送到缓存热的区域，TTFT 和成本可能更好；但热点区域一旦拥塞，排队时间会吃掉缓存收益。可行做法是“locality first with bounded overflow”：

```text
1. 先找满足合规和模型版本的区域
2. 优先本地区域或 session 粘性区域
3. 如果 queue wait / KV pressure 超过阈值，允许溢出到远端区域
4. 溢出时优先选也有相同 model / adapter / prefix 热度的区域
5. 记录溢出原因，避免盲目跨区导致 cache 全部变冷
```

对会话型 workload，还要小心“粘性”的边界。聊天上下文很长时，把同一个 session 留在一个区域可以提高 prefix cache 命中率；但如果这个区域 GPU 满了，继续粘住会让用户排队。比较稳的做法是给粘性设置软约束：命中缓存时优先，排队超过阈值时迁移。迁移后不要假装 KV cache 也迁移了，除非系统真的支持跨区域 KV transfer，并且能接受隐私和网络成本。

故障策略也要提前定清楚：

```text
非流式请求:
  可以用 request id 做幂等重试，失败后换区域重新执行

流式请求:
  已经吐出的 token 很难无缝接上，通常只能报错、让客户端重新发起，或者提供 best-effort continuation

工具调用请求:
  如果模型可能触发外部副作用，跨区域重试前必须检查幂等键和权限

长请求:
  远端重试可能造成重复 GPU 消耗，要有预算和 retry limit
```

在 LogServe 这类系统里，现有 `LOCALITY_AWARE` 和 `PREDICTED_LATENCY` 思路可以扩展到多区域：先把 worker 的 region、模型缓存、历史 EWMA latency、queue wait 作为调度特征，再加合规和租户配额过滤。真正线上时，不建议只用 region-level QPS 做路由，因为 LLM 的资源消耗跟 prompt length、max tokens、adapter、cache hit 都相关。

面试里可以这样答：

```text
多区域 LLM serving 路由要先过滤合规、模型版本和租户权限，再在低延迟区域、缓存热区域和空闲区域之间做权衡。普通就近路由不够，因为 LLM 有 model cache、prefix/KV cache、长 decode 和不可预测输出长度。实用策略是 locality first with bounded overflow：默认就近并保持 session/prefix 粘性；当 queue wait、显存水位或 batch slot 超阈值时，把请求溢出到有容量的远端区域。流式请求、工具调用和长请求还要单独定义重试、幂等和失败语义。
```

## Q045. LLM 服务的安全风险包括 prompt injection、数据泄露、越权调用还是资源滥用？

**回答：**

都包括，而且这四类风险在 LLM serving 里经常互相放大。OWASP Top 10 for LLM Applications 把 prompt injection、sensitive information disclosure、excessive agency 等列为核心风险；资源滥用虽然不总是被当成单独应用漏洞，但在推理服务里会直接造成成本、可用性和多租户隔离问题。

prompt injection 是最典型的 LLM 应用层风险。攻击者把恶意指令藏在用户输入、网页、文档、邮件、RAG 检索结果或工具返回值里，让模型忽略系统指令、泄露上下文、调用工具或输出错误内容。传统系统里“代码”和“数据”可以强隔离，LLM prompt 里这条边界更模糊，所以不能只靠一句“不要听用户恶意指令”解决。

数据泄露有几条路径：

```text
prompt 泄露:
  系统 prompt、开发者指令、RAG 文档、其他用户上下文被模型吐出

日志泄露:
  prompt、completion、tool result、embedding payload 被完整写入日志

缓存泄露:
  prefix cache、KV cache、LoRA cache、RAG cache 没有按 tenant 和权限域隔离

工具泄露:
  模型调用搜索、数据库、工单系统时拿到了不该拿的数据

训练 / 反馈数据泄露:
  线上请求被错误进入训练或 eval 管道
```

越权调用主要出现在 tool use / agent 场景。模型本身不应该拥有真实权限，工具调用层必须重新鉴权。常见问题是：把高权限 API key 放给模型、让模型自由拼 URL 或 SQL、没有 human approval、没有按用户身份做授权、没有幂等键，最后模型在 prompt injection 下执行了删除、转账、发邮件、改配置等操作。

资源滥用在 LLM serving 中非常现实：

```text
超长 prompt:
  占满 prefill 和 KV cache

超大 max tokens:
  拖慢 decode，制造长尾

并发刷请求:
  挤占 batch slot 和 GPU 显存

恶意 cache pollution:
  用大量唯一 prefix 让 prefix cache 失效或驱逐热点

重试风暴:
  客户端超时后反复重试，同一请求被多个 worker 同时执行

高成本参数:
  best_of、parallel tool calls、多候选生成、reasoning effort 等参数被滥用
```

控制手段要分层做：

1. 输入层：长度限制、文件类型限制、RAG 文档来源控制、prompt injection 检测。
2. 权限层：工具按用户身份授权，最小权限 token，敏感动作二次确认。
3. 缓存层：cache key 包含 tenant、model、adapter、权限域，禁止跨安全域复用。
4. 调度层：按 token 而不是按 request 做 quota，限制 max prompt tokens、max output tokens、并发序列和 GPU seconds。
5. 日志层：脱敏、采样、加密，避免完整 prompt 和工具结果长期裸存。
6. 输出层：结构化输出校验、URL/SQL/命令 allowlist，工具调用前做 schema 和 policy check。
7. 运营层：审计、异常成本告警、租户隔离报表、滥用封禁。

面试里可以这样答：

```text
这些都是 LLM 服务安全风险。prompt injection 会影响模型指令遵循；数据泄露可能发生在 prompt、日志、RAG、工具结果、prefix/KV cache 和训练反馈链路；越权调用主要来自工具和 agent 权限设计不当；资源滥用则通过长 prompt、大 max tokens、高并发、cache pollution 和重试风暴拖垮 GPU。防护不能只靠 prompt，要在鉴权、工具网关、缓存命名空间、token quota、日志脱敏、输出校验和审计上一起做。
```

## Q046. model cold start 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

model cold start 的核心目标是把“还不能服务请求的模型”变成“可以稳定服务请求的模型”，并把这个过程的延迟、资源占用和失败边界控制住。它主要解决性能和可用性问题，不是主要解决正确性、安全性或可维护性问题。

一个完整 cold start 可能包括：

```text
解析 model id / version / adapter
从对象存储或本地缓存找到 checkpoint
校验 manifest、checksum、权限和兼容性
把权重加载到 CPU/GPU
初始化 CUDA context、NCCL、TensorRT engine 或 vLLM engine
分配工作区和 KV cache block
加载 tokenizer、chat template、LoRA adapter
做 warmup 或 CUDA graph capture
标记 worker/model ready
```

这些步骤的直接目标是降低首个请求的等待时间和失败概率。没有 cold start 管理时，第一个命中冷模型的用户可能承担完整 checkpoint 下载、权重加载、GPU 初始化、kernel 编译和 warmup 成本，TTFT 可能从几百毫秒变成几十秒甚至几分钟。

它和正确性的关系是“必要但不是核心”。模型版本必须加载对，checksum 必须校验，tokenizer 必须匹配，否则当然会出错。但这些属于加载校验，不是 cold start 这个概念本身要解决的主要矛盾。冷启动做得好，不代表模型输出更正确；它只是让正确版本更快进入可服务状态。

它和安全性的关系也类似。加载前要验证权限、签名、来源、租户边界，防止加载未授权模型或恶意权重。但 cold start 的主要指标仍然是 load latency、ready latency、失败率、缓存命中、显存峰值和服务可用性。

可维护性会受到影响，但不是主目标。把 cold start 流程做成状态机、manifest、事件日志和重试机制，确实让系统更好维护；不过面试里如果只能选一个主类，答案应是性能/可用性。

在 LogServe 的实现语义里，`ModelLoadStarted -> ModelLoaded -> LLMCompleted` 这条事件链正好体现了 cold start 的边界：从发现模型未命中、开始加载，到模型可用于执行，再到请求完成。mock adapter 会模拟 model load 和 first-token latency，vLLM adapter 则把生成请求转给 OpenAI-compatible 后端。这说明 cold start 首先影响的是调度和延迟，不是模型语义本身。

面试里可以这样答：

```text
model cold start 的核心目标是性能和可用性：把冷模型从未加载状态变成 ready 状态，并控制首次请求的等待、显存峰值和失败率。它会涉及正确性校验和安全校验，比如版本、checksum、权限、签名，但这些是加载前置条件，不是 cold start 的主目标。冷启动做得好，用户少等，系统少抖，容量更可控；它不会让模型本身更聪明或更正确。
```

## Q047. model cold start 的典型适用场景和不适用场景分别是什么？

**回答：**

model cold start 机制适合“模型很多、访问稀疏、GPU 昂贵、不能全部常驻”的场景。它不适合“请求必须极低延迟、模型集合固定且持续高流量”的场景。

适用场景：

```text
多模型平台:
  上百或上千个模型，只有少数热点模型持续活跃

多租户 LoRA:
  base model 常驻，租户 adapter 按需加载

低频任务模型:
  某些分类、抽取、审查模型每天只被调用几次

按需扩容:
  峰值时新 worker 拉起模型，平峰时释放 GPU

多区域容灾:
  备用区域不常驻全量模型，故障时再加载或预热

灰度发布:
  新版本先冷加载到少量 worker，验证后扩大流量

成本敏感平台:
  通过 scale-to-zero 或模型缓存减少空闲 GPU 成本
```

不适用或要谨慎的场景：

```text
严格低延迟在线服务:
  用户要求稳定几十到几百毫秒 TTFT，不能让请求承担加载成本

固定热点模型:
  模型一直高 QPS，常驻比频繁冷启动更简单

超大模型:
  加载需要数分钟，冷启动不适合放在请求路径上

硬实时或安全关键系统:
  请求超时风险不可接受，应预加载并做容量冗余

显存碎片严重的平台:
  频繁加载卸载可能带来碎片、OOM 和抖动

有状态会话:
  session 依赖已有 KV cache 或长上下文，冷迁移代价很高
```

更准确地说，cold start 不是一个“好或不好”的功能，而是一种成本策略。它用首请求延迟和复杂度换 GPU 利用率。如果用户流量稳定，模型数量少，GPU 够用，常驻模型通常更可靠。如果模型长尾很重，冷启动和分层缓存才有明显价值。

实践中常见折中是 warm pool：

```text
热点模型:
  常驻 GPU

温模型:
  权重在本地磁盘或 CPU memory，GPU 按需加载

冷模型:
  只在对象存储，有请求时拉取

即将变热的模型:
  根据发布计划、租户预约或历史模式提前 warmup
```

面试里可以这样答：

```text
model cold start 适合模型数量多、访问长尾明显、GPU 成本高、不能全量常驻的系统，比如多租户 LoRA、低频任务模型、灰度发布和多区域备用容量。不适合固定高 QPS、强低延迟、超大模型请求路径加载、硬实时或有状态长会话场景。它本质上是用可控的首次加载延迟换资源利用率，所以通常要和 warm pool、热点常驻、分层缓存一起设计。
```

## Q048. model cold start 和相近概念最容易混淆的边界在哪里？

**回答：**

model cold start 容易和 cache miss、container cold start、TTFT、warmup、autoscaling 混在一起。面试时最好先定义边界：model cold start 指模型服务实例在当前执行位置还不能直接提供该模型推理，需要完成权重、运行时和必要缓存初始化的过程。

容易混淆的边界如下：

```text
model cold start vs container cold start:
  container cold start 是进程或容器起来。
  model cold start 是模型权重和推理运行时 ready。
  容器已启动不代表模型已加载。

model cold start vs checkpoint fetch:
  checkpoint fetch 只是把文件从对象存储拉到本地。
  cold start 还包括加载权重、初始化 runtime、分配显存和 warmup。

model cold start vs model cache miss:
  cache miss 是状态判断。
  cold start 是从 miss 到 ready 的过程。

model cold start vs prefix cache miss:
  model cold start 关注权重和运行时。
  prefix cache miss 关注 prompt 前缀的 KV 是否可复用。
  模型可以是热的，但 prefix 是冷的。

model cold start vs TTFT:
  TTFT 是用户看到第一个 token 的端到端时间。
  cold start 只是 TTFT 的一个组成部分，还要加排队、prefill、网络和首步 decode。

model cold start vs warmup:
  warmup 是 cold start 后段的优化动作，比如跑 dummy request、CUDA graph capture。
  不是所有 cold start 都一定做完整 warmup。

model cold start vs autoscaling:
  autoscaling 拉起 worker 或 GPU 容量。
  cold start 让某个模型在这份容量上 ready。
```

一个具体例子：

```text
worker 进程已启动:
  container warm

模型 checkpoint 已在本地磁盘:
  checkpoint cache hit

权重还没进 GPU:
  model cold

权重已进 GPU，但没有该 prompt prefix:
  model warm, prefix cold

该 session 的 KV cache 也在:
  model warm, prefix/KV warm
```

这个区分很重要，因为不同问题用不同手段解决。container cold start 用镜像预拉取、进程池、节点预热；checkpoint fetch 慢用本地 SSD、并行下载、模型格式优化；model cold start 慢用常驻、warm pool、分层缓存、加载锁；prefix miss 用 prompt 结构和 locality-aware routing；TTFT 高还可能是队列和 prefill。

面试里可以这样答：

```text
model cold start 的边界是模型从当前执行位置不可服务到可服务。它不是 container cold start，不只是 checkpoint 下载，也不是 prefix cache miss 或 TTFT 本身。容器可以是热的但模型冷；模型可以是热的但 prefix 冷；TTFT 高也可能来自 queue wait 或长 prompt。把这些边界说清楚，才能给出正确优化手段。
```

## Q049. model cold start 在高并发场景下可能出现哪些隐藏问题？

**回答：**

高并发下，model cold start 最大的风险不是“单个请求慢”，而是多个请求同时把系统推向同一个冷模型，造成放大效应。平时看起来只是一次加载，峰值时会变成下载风暴、加载风暴、OOM 风暴和重试风暴。

典型隐藏问题：

1. thundering herd。大量请求同时发现模型未加载，每个 worker 都去下载同一个 checkpoint，打满对象存储、网络和磁盘。
2. duplicate load。同一台机器上多个线程或进程同时加载同一模型，CPU 内存、GPU 显存和文件句柄被重复占用。
3. GPU OOM。调度器只看模型大小，不看 KV cache、workspace、CUDA graph、LoRA adapter、并发 batch，加载到一半才爆显存。
4. queue head-of-line blocking。冷模型请求占住队列或 worker slot，短热请求被迫等待。
5. cache eviction storm。为了加载冷模型驱逐热点模型，随后热点流量又把冷模型挤掉，系统反复抖动。
6. retry amplification。客户端超时后重试，服务端还在加载旧请求，新请求又触发更多加载。
7. readiness 假阳性。模型文件在本地或进程启动了，健康检查就标记 ready，但 GPU 权重还没完成加载。
8. 多租户互相影响。一个租户触发大量冷模型，挤占其他租户的 GPU、磁盘带宽和加载队列。
9. NCCL/CUDA 初始化竞争。多卡模型或 tensor parallel 初始化同时发生，通信和显存分配更容易失败。
10. 观测误判。平均延迟只看到“请求慢”，没有把 checkpoint fetch、model load、queue wait 分开，排障很慢。

控制手段要直接针对“并发放大”：

```text
singleflight / load lock:
  同一 model/version/adapter 在同一资源域内只允许一个加载任务，其他请求等待结果

admission control:
  冷模型加载也要占预算，不能无限排队

warm pool:
  热点和即将变热的模型提前加载

staggered warmup:
  多 worker 分批预热，避免同时打满存储和 GPU

load-aware scheduling:
  调度器知道某模型正在加载，把后续请求合并到同一加载结果

tenant quota:
  限制每个租户可同时触发的冷模型数和 GPU seconds

atomic cache metadata:
  文件完整后再发布 manifest，避免读到半成品
```

在 LogServe 的事件模型里，这类问题可以通过观察 `ModelLoadStarted` 和 `ModelLoaded` 的比例发现：如果同一 model/version 在短时间内出现大量 `ModelLoadStarted`，但完成事件慢或失败多，就说明加载并发没有被合并，或者缓存/容量策略有问题。

面试里可以这样答：

```text
高并发下 model cold start 会放大成 herd：很多请求同时下载、同时加载、同时占显存，可能引发对象存储瓶颈、重复加载、GPU OOM、队头阻塞、热点模型被驱逐、重试风暴和租户互相影响。解决思路是 singleflight 加载锁、admission control、warm pool、分批预热、加载状态复用、租户级冷启动配额和细粒度观测。不能让每个请求独立决定“模型不在，我自己加载一个”。
```

## Q050. model cold start 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

cold start 的边界条件集中在“状态是否已经发布”和“资源是否已经释放”。加载模型不是一个原子动作：文件可能下载到一半，manifest 可能还没写，GPU 显存可能已经分配，worker 可能还没 heartbeat，客户端可能已经超时。只要这些状态没有严格定义，崩溃和重试就会制造脏缓存、重复加载和错误路由。

崩溃场景要关注：

```text
下载中崩溃:
  本地可能留下半个 checkpoint 文件
  需要临时文件 + checksum + atomic rename

写 manifest 时崩溃:
  manifest 可能缺字段或指向不存在文件
  启动扫描时必须校验完整性

GPU 加载后崩溃:
  进程退出会释放显存，但控制面可能还以为 worker 有热模型
  heartbeat / lease 过期前不要继续路由

ModelLoaded 事件后崩溃:
  模型加载成功不代表请求完成
  请求执行状态和模型缓存状态要分开
```

重启场景要关注“磁盘缓存”和“GPU resident 状态”的区别。重启后本地磁盘可能仍有 checkpoint，但 GPU 显存里的权重已经没了。worker 启动时可以扫描本地模型缓存 manifest，但不能直接宣称 GPU 已经 ready，除非它真的重新加载并通过 readiness check。

超时场景要看超时发生在哪一层：

```text
checkpoint fetch 超时:
  取消下载，释放文件锁，清理临时文件

model load 超时:
  释放加载锁和 CPU/GPU 中间资源，标记失败原因

warmup 超时:
  决定是否降级为 loaded-but-not-warmed，还是整体失败

request 超时:
  客户端不等了，不代表服务端加载任务还应该继续
  如果加载结果能服务后续请求，可以转后台；否则要取消
```

重试场景最容易出副作用。客户端重试、网关重试、调度器重试和 worker 内部重试如果互相不知道，就可能同一模型加载多次、同一请求执行多次。解决办法是给 model load 和 request execution 分别设计幂等 key：

```text
model load key:
  model_id + version + quantization + adapter + worker/resource domain

request idempotency key:
  tenant + request_id + input hash + model version
```

流式请求还要额外小心。模型刚加载完，已经开始输出 token，连接断了。这时不能简单“换 worker 重试并继续输出”，因为另一个 worker 生成的 token 不一定接得上，采样状态也不一定相同。服务端可以提供重新执行、从已输出文本继续、或返回失败让客户端决定，但要明确语义。

在 LogServe 这类有事件回放的系统里，边界可以通过事件序列表达：`ModelLoadStarted` 没有对应 `ModelLoaded`，说明加载中断或失败；有 `ModelLoaded` 但没有 `LLMCompleted`，说明模型可用和请求完成之间发生了故障。统计 EWMA latency 时也要避免把未完成请求当成成功样本。

面试里可以这样答：

```text
cold start 在崩溃、重启、超时和重试下会暴露状态边界。下载中崩溃要防半文件，manifest 要 atomic publish；重启后磁盘有 checkpoint 不代表 GPU 已加载；超时要释放加载锁、临时文件和显存；重试要用 model load key 合并加载，用 request idempotency key 防重复执行。模型加载成功和请求完成是两件事，流式输出失败后也不能假装可以无缝接续。
```

## Q051. model cold start 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

都可能是瓶颈，但它们出现在不同阶段。model cold start 不是一个单点操作，而是一条流水线：找模型、拉 checkpoint、校验文件、反序列化权重、搬到 CPU/GPU 内存、初始化 runtime、分配 KV cache/workspace、做 warmup。只说“瓶颈来自 I/O”或者“瓶颈来自 CPU”都太粗。

可以按阶段拆：

```text
远端拉取阶段:
  主要瓶颈是网络、对象存储吞吐、TLS/认证、跨区域延迟、并发下载限制。

本地读取阶段:
  主要瓶颈是磁盘 I/O、文件数量、随机读、mmap 行为、文件系统元数据。

解析和转换阶段:
  主要瓶颈是 CPU、内存带宽、反序列化、dtype 转换、weight mapping、checksum。

加载到 GPU 阶段:
  主要瓶颈是 PCIe/NVLink、GPU 显存分配、pinned memory、CUDA context 初始化。

多卡初始化阶段:
  主要瓶颈是 NCCL、rank 同步、tensor parallel shard 对齐、网络或 NVLink 拓扑。

调度保护阶段:
  主要瓶颈可能是锁竞争、singleflight 等待、全局加载队列和 admission control。
```

TensorRT-LLM 的 checkpoint loading 文档把加载拆成 config loader、weight loader、weight mapper、checkpoint loader 几个组件，这正好说明模型加载不仅是“读文件”。weight mapper 和格式转换可能消耗 CPU；weight loader 可能卡在磁盘或对象存储；分布式模型还要考虑每个 rank 读自己 shard 的方式。

vLLM 的 fastsafetensors 和 Run:ai Model Streamer 文档也说明了同一件事：如果目标是加快模型加载，优化点可能在 GPU direct storage、并发读 tensor、从对象存储 streaming 到 GPU、限制 CPU buffer memory、按 shard 加载，而不是只改一个缓存目录。

在实际排障时，我会把 cold start latency 拆成这些指标：

```text
resolve_ms:
  找 model metadata、revision、tokenizer、adapter 的时间

fetch_ms:
  从远端或源目录把 checkpoint 拉到本地的时间

local_read_ms:
  从本地磁盘读 checkpoint 的时间

deserialize_ms:
  safetensors/bin/pth 解析和 tensor 构造时间

map_convert_ms:
  权重命名映射、dtype 转换、量化反量化准备时间

gpu_transfer_ms:
  CPU 到 GPU 或 GDS 到 GPU 的搬运时间

runtime_init_ms:
  CUDA context、NCCL、engine、kernel、CUDA graph 初始化

warmup_ms:
  dummy request、graph capture、memory pool 预热时间

lock_wait_ms:
  等待加载锁、singleflight、全局队列的时间
```

LogServe 的简化实现里，`ensureCheckpoint` 把 checkpoint fetch 和 model load 分成两个指标：`CheckpointFetchMs` 和 `ModelLoadMs`。这很有用，因为第一次 miss 慢可能是 copy 慢，也可能是 read/load 慢；第二次 hit 时 fetch 应该是 0，但 load 仍然存在。它还用了一个全局 `checkpointMu` 串行化 checkpoint 加载，正确性简单，但如果不同模型也要排同一把锁，高并发下就可能出现 lock_wait，而不是 I/O 本身慢。

面试里可以这样答：

```text
model cold start 的瓶颈要按阶段看。远端 checkpoint miss 通常卡网络和对象存储；本地缓存命中后可能卡磁盘 I/O、CPU 反序列化、内存带宽、dtype 转换和 GPU 传输；多卡模型还会卡 NCCL 和 rank 同步；高并发时加载锁和 singleflight 等待也会变成瓶颈。工程上要把 fetch、local read、deserialize、GPU transfer、runtime init、warmup、lock wait 分开打点，否则只看一个 cold_start_ms 很难定位。
```

## Q052. model cold start 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

这三类测试不要混在一起。correctness test 验证语义对不对；stress test 验证并发和故障下会不会乱；benchmark 验证在指定硬件和 workload 下到底快不快。一个 cold start 系统如果只做 benchmark，很容易把脏缓存、重复加载、错误版本这些问题藏起来。

correctness test 主要测不变量：

```text
第一次请求 cold miss:
  应触发 checkpoint fetch / model load，返回 cache_hit=false

第二次同模型请求:
  应命中本地缓存，fetch_ms 应为 0 或接近 0

model key:
  model name、version、revision、quantization、adapter 不同就不能误命中

manifest:
  重启后能从完整 manifest 恢复缓存索引

capacity:
  checkpoint 超过容量时应拒绝或降级，不能写一半

eviction:
  LRU 或成本策略驱逐正确，不删除正在加载或正在服务的模型

cancellation:
  context 取消后临时文件、锁、显存和内存资源要释放

event order:
  ModelLoadStarted、ModelLoaded、LLMCompleted 的状态转换不能乱序
```

LogServe 现在的 `model_cache_test.go` 就覆盖了几个关键语义：第一次 copy 后命中、容量超限触发 LRU 驱逐、重启后扫描 manifest、并发 cold load 被串行化成一个 miss 和多个 hit。这些属于 correctness test，不需要追求真实 GPU 性能。

stress test 主要测系统在压力下是否还能守住边界：

```text
同一模型并发 cold miss:
  100 个请求同时打同一个冷模型，应只有一次真实下载/加载

不同模型并发 cold miss:
  不同模型之间是否被一把粗锁串行化，队列是否爆炸

容量接近满:
  多个大模型同时进入时，eviction 是否抖动，是否出现负 used_bytes

慢对象存储:
  fetch 慢、超时、断连时是否释放锁和临时文件

重启风暴:
  多 worker 同时启动扫描 cache，是否把存储或控制面打满

租户竞争:
  一个租户大量冷模型是否挤掉其他租户热点模型

故障注入:
  在 copy、rename、manifest write、read、load、heartbeat 各阶段杀进程
```

benchmark 才关心性能数字。它应该明确 workload 和硬件，而不是只说“cold start 优化了”。至少要记录：

```text
冷启动:
  p50/p95/p99 cold_start_ms、fetch_ms、load_ms、ready_ms

热命中:
  warm_hit_ms、cache lookup ms、本地 read ms

端到端:
  TTFT、TPOT、total latency、queue wait

吞吐:
  每秒加载模型数、tokens/s、并发请求下成功率

资源:
  CPU、RSS、page cache、磁盘读写带宽、网络带宽、GPU 显存峰值

副作用:
  eviction_count、cache_hit_rate、OOM、timeout、retry_count
```

如果是真实 GPU benchmark，还要记录 GPU 型号、驱动、CUDA、NCCL、推理框架、模型格式、dtype、量化、存储类型、对象存储区域、网络带宽、模型大小和 shard 数。ServerlessLLM 这类论文之所以强调 local checkpoint storage、multi-tier checkpoint loading 和 startup-time-aware scheduling，就是因为加载路径和存储层级对冷启动延迟影响很大。

面试里可以这样答：

```text
correctness test 测状态语义：第一次 miss、第二次 hit、版本隔离、manifest 恢复、容量和驱逐、取消清理、事件顺序。stress test 测高并发和故障：同模型并发只加载一次、不同模型是否被粗锁阻塞、慢存储、重启风暴、容量抖动、租户竞争。benchmark 测性能：cold/warm latency、fetch/load 分解、TTFT、queue wait、p95/p99、带宽、显存、eviction 和 hit rate。三者不能互相替代。
```

## Q053. 如果要求从零实现一个简化版 model cold start，你会先定义哪些不变量？

**回答：**

我会先写不变量，再写代码。model cold start 的坑不在“怎么 copy 文件”本身，而在状态什么时候可以对外可见、失败后怎么收回、并发下谁负责加载。

第一组不变量是身份不变量：

```text
cache key 必须唯一标识可执行模型:
  model_name
  model_version / revision
  checkpoint digest
  dtype / quantization
  tensor parallel 配置
  adapter_id / adapter_revision
  tokenizer / chat template revision
```

不能只用 model name。`llama-8b` 这个名字背后可能是不同 revision、不同量化、不同 tokenizer，误命中会比冷启动慢更糟。

第二组是不发布半成品：

```text
只有完整 checkpoint 可以进入 cache index
只有完整 manifest 可以被重启扫描恢复
只有模型真实加载完成后才能上报 ready
只有 ready 的 worker 才能被调度器认为可执行该模型
```

实现上通常需要临时文件、checksum、atomic rename 和 manifest 原子发布。LogServe 的 `copyCheckpoint` 会先写 `.tmp`，完成后 rename 到目标路径；manifest 也先写 `.tmp` 再 rename。这个方向是对的，因为崩溃后最多留下临时文件，不应该留下一个看起来可用的半 checkpoint。

第三组是并发不变量：

```text
同一 resource domain 内，同一 model key 只能有一个 active load
等待同一模型的请求复用同一个加载结果
取消一个等待者不应该取消所有等待者，除非加载任务本身被取消
不同模型是否能并行，要由容量和带宽预算决定
加载中的模型不能被 eviction 删除
```

简化实现可以先用 per-model singleflight，比全局锁更容易扩展。全局锁能避免重复加载，但会让不同模型也互相阻塞；做 demo 可以接受，线上要小心。

第四组是容量不变量：

```text
used_bytes 不能超过 capacity_bytes，或者必须明确允许临时超额
checkpoint 大于容量时不能进入缓存
eviction 之后 index、manifest、文件必须一致
正在服务的模型不能被直接删除
GPU resident memory 和磁盘 checkpoint cache 要分开统计
```

第五组是状态机不变量：

```text
Missing -> Fetching -> CachedOnDisk -> LoadingToRuntime -> Ready -> Serving
失败路径必须回到 Missing 或 CachedOnDisk，不能卡在中间
每个状态都有 timeout、cancel、retry 和 cleanup 规则
事件日志能重建最后一次可信状态
```

第六组是观测不变量：

```text
每次 cold start 至少记录:
  model key
  worker id
  cache hit/miss
  fetch_ms
  load_ms
  eviction_count
  used/capacity
  error phase
```

没有这些指标，线上问题会变成一句“模型第一次慢”，根本没法判断是网络、磁盘、CPU、GPU 还是锁等待。

面试里可以这样答：

```text
我会先定义不变量：cache key 必须包含模型版本、revision、dtype、量化、adapter 和 tokenizer；半文件、半 manifest、半加载状态不能对外发布；同一模型同一资源域只能有一个 active load；容量和驱逐后文件、manifest、内存 index 必须一致；重启后磁盘缓存不等于 GPU ready；每个状态都有取消、超时和失败清理规则。先守住这些，再写 copy、load 和 warmup 代码。
```

## Q054. model cold start 的常见误用是什么，误用后通常会产生什么线上症状？

**回答：**

model cold start 最常见的误用是把它当成“懒加载就行”。懒加载只是触发时机，真正要设计的是缓存状态、容量、并发、路由和失败边界。误用后线上症状通常不是立刻崩，而是 p99 抖、偶发 OOM、热模型被挤掉、重试变多、某些租户突然变慢。

常见误用如下：

```text
把本地有 checkpoint 当成模型 ready:
  症状是调度器把请求送到 worker，但 worker 还要加载 GPU 权重，TTFT 突然很高。

只按 model name 做 cache key:
  症状是版本串线、LoRA 串线、量化配置不一致，输出质量或工具调用异常。

不做 singleflight:
  症状是同一冷模型被同时下载和加载多次，网络、磁盘、显存一起飙升。

全局加载锁过粗:
  症状是 model-A 冷启动阻塞 model-B，队列等待时间变成长尾。

不设容量和驱逐策略:
  症状是磁盘写满、节点 NotReady、checkpoint copy 失败。

驱逐正在使用的模型:
  症状是请求执行中失败，或者下一步 decode/adapter 加载找不到文件。

健康检查过早 ready:
  症状是发布或扩容后大量请求打到还没热好的实例。

重试没有幂等:
  症状是一次用户请求触发多个 worker 加载同一模型，成本和延迟同时放大。
```

还有一种误用更隐蔽：把 cold start 当成性能优化，而不是资源策略。比如为了省 GPU，把热点模型也频繁卸载，结果平均 GPU 利用率看起来提高了，p99 和用户体验却变差。LLM serving 里用户往往对 TTFT 很敏感，省下的空闲显存不一定抵得过冷启动长尾。

在 LogServe 这样的系统里，如果 `LOCALITY_AWARE` 或 `PREDICTED_LATENCY` 只看到“worker 有缓存”，但不知道缓存是磁盘 checkpoint、CPU memory、还是 GPU resident，调度就会过于乐观。README 里区分了 checkpoint fetch、model load、first-token、total latency，这个分解能避免把所有慢都归咎于调度器。

面试里可以这样答：

```text
常见误用包括：把 checkpoint 在磁盘上当成模型已 ready，只用 model name 做 cache key，不做 singleflight，全局锁过粗，不限制容量，驱逐活跃模型，健康检查过早通过，重试没有幂等。线上症状通常是 p99 TTFT 抖动、重复下载、磁盘写满、GPU OOM、缓存命中率看起来高但首 token 仍然慢、热模型被冷模型挤掉、某个租户拖慢其他租户。
```

## Q055. model cold start 在单机和分布式环境中的语义有什么差异？

**回答：**

单机里的 model cold start 是本地状态问题；分布式里的 model cold start 是全局调度问题。这个区别很大。单机只要保证本进程、本磁盘、本 GPU 的状态正确；分布式还要保证控制面、多个 worker、多区域、租户配额和事件日志对“ready”的理解一致。

单机语义通常是：

```text
本机 cache 里是否有 checkpoint
本进程是否加载了模型权重
本 GPU 是否有足够显存
本地锁是否防止重复加载
本地 manifest 是否可在重启后恢复
```

在单机里，`cache.has(model-A:v1)` 的含义比较直接。它可能表示磁盘上有 checkpoint，也可能表示进程内标记了模型可用。虽然仍要区分磁盘和 GPU，但至少状态边界在一台机器内。

分布式语义会多出几层：

```text
worker-local:
  这个 worker 是否有 checkpoint / GPU resident model

node-local:
  同一节点上不同进程是否共享本地 SSD / page cache

cluster-level:
  控制面认为哪些 worker 有模型，信息是否过期

region-level:
  哪个区域有模型、adapter、prefix locality 和 GPU 容量

tenant-level:
  哪个租户有权使用这个模型和缓存
```

分布式环境里，ready 状态必须有 lease 或 heartbeat。否则 worker 崩了，控制面还以为它有热模型；worker 重启后磁盘缓存还在，但 GPU resident 状态没了；网络分区时，两个调度器可能都以为自己应该触发加载。LogServe 通过 worker 注册和 heartbeat 上报 cached models，这就是把本地缓存状态发布给控制面的最小机制。

多卡模型还会增加分布式冷启动语义。tensor parallel 下，模型不是“某台机器 loaded”就行，而是所有 rank 都加载了正确 shard，NCCL 初始化完成，rank 拓扑一致，才能对外 ready。某一个 rank 慢或失败，整个模型实例都不能服务。

调度策略也不同：

```text
单机:
  重点是加载锁、容量、磁盘缓存、GPU 显存。

分布式:
  重点是把请求路由到已有 locality 的 worker，避免重复冷启动，控制跨 worker 的加载预算。
```

ServerlessLLM 把 checkpoint locality 纳入 startup-time-aware scheduling，原因就在这里：分布式系统里，某台机器已经有本地 checkpoint，另一台没有；如果调度器不知道这个差异，就会把请求送到看似空闲但冷的机器上，TTFT 反而更差。

面试里可以这样答：

```text
单机 cold start 关注本地 checkpoint、进程、GPU 显存和加载锁；分布式 cold start 还要关注控制面看到的 worker 缓存状态是否新鲜、heartbeat/lease 是否过期、多 worker 是否重复加载、租户配额、多区域 locality、多卡 rank 是否全部 ready。单机的 ready 是本地事实，分布式的 ready 是带时效的声明，必须有事件、heartbeat 和失败恢复机制。
```

## Q056. checkpoint cache 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

checkpoint cache 的核心目标是减少重复下载和重复读取远端模型 artifact 的成本。它主要解决性能和可用性问题：让模型冷启动更快，让对象存储和网络压力更小，让 worker 重启后能复用本地已有 checkpoint。

它不是 GPU model cache。checkpoint cache 通常缓存的是磁盘上的模型文件、shard、safetensors、bin、manifest、tokenizer 文件或 adapter 文件。GPU model cache 缓存的是已经加载到 GPU 的权重和 runtime 状态。前者命中后仍可能要读文件、解析、搬运到 GPU；后者命中后才能接近直接执行。

Hugging Face Hub 的 cache 文档也体现了这个目标：本地磁盘 cache 避免重复下载同一个文件，按 blobs、snapshots、refs 组织不同 revision。ServerlessLLM 论文里更进一步，把 near-GPU storage 和 multi-tier checkpoint loading 当成降低 LLM serverless 冷启动的核心手段。

checkpoint cache 与正确性的关系是前置条件。它必须保证版本、revision、checksum、manifest 正确；如果缓存错版本，性能越好，事故越快。但它的主目标不是提高模型输出正确率，而是减少 artifact 获取时间。

checkpoint cache 与安全性的关系也不是主目标，但不能忽略。缓存里可能有私有模型、租户 adapter、授权文件，必须有权限域、文件权限、加密、审计和清理策略。尤其是多租户平台，不能让租户 A 的私有 checkpoint 被租户 B 的请求命中。

可维护性会因为 manifest、索引、清理策略变好，但这仍然是附带收益。面试里如果问“主要解决哪类问题”，我会回答性能/可用性；如果系统做得严谨，还会补充正确性和安全性是不变量，不是可选项。

在 LogServe 里，checkpoint cache 的定位很清楚：mock checkpoint cache 会把 `--model-source-dir` 的 checkpoint copy 到 `--model-cache-dir`，写 manifest，worker 重启时扫描 manifest，并在容量超过 `--model-cache-capacity-bytes` 时 LRU 驱逐。它解决的是冷 miss 时的文件获取和重启后的本地复用。

面试里可以这样答：

```text
checkpoint cache 的核心目标是性能和可用性：把模型 artifact 保存在本地或近 GPU 存储里，减少重复远端下载、跨区域网络和对象存储压力，从而降低 model cold start。它不是 GPU model cache，命中后仍可能要加载权重到 GPU。正确性、安全性和可维护性是约束：版本、checksum、manifest、权限和清理必须正确，但主目标仍是减少 artifact 获取成本。
```

## Q057. checkpoint cache 的典型适用场景和不适用场景分别是什么？

**回答：**

checkpoint cache 适合 checkpoint 大、远端读取慢、模型会重复使用、worker 本地存储足够的场景。不适合模型只用一次、更新极频繁、强加密不可落盘、或本地磁盘比远端还差的场景。

典型适用场景：

```text
多模型长尾平台:
  模型不一定常驻 GPU，但可能一段时间内反复被请求。

serverless LLM:
  实例会 scale-to-zero，checkpoint cache 可以减少下一次冷启动下载。

多区域 serving:
  每个区域保留热点模型 checkpoint，避免跨区域拉取。

多租户 LoRA:
  base model checkpoint 大，adapter 较多，可以分层缓存。

CI / benchmark / eval:
  同一批模型反复启动，cache 可以减少实验等待时间。

边缘或私有云:
  外网带宽不稳定，本地缓存能减少对中心仓库依赖。
```

不适用或要谨慎的场景：

```text
一次性模型:
  模型只启动一次，缓存维护成本可能大于收益。

模型频繁更新且 revision 不固定:
  latest 指针频繁变化，容易误用旧 checkpoint。

本地磁盘小或慢:
  缓存会挤爆磁盘，或者本地读比远端并行 streaming 慢。

强安全限制:
  私有模型不允许落盘，或必须使用硬件加密、短生命周期临时卷。

超大模型但无容量规划:
  一个 checkpoint 就超过 cache capacity，反复失败。

已有高效共享存储:
  如果集群有高吞吐低延迟文件系统，worker-local cache 的收益要重新测。
```

还有一个细节：checkpoint cache 适合缓存“可复用 artifact”，不适合缓存“运行时状态”。KV cache、CUDA graph、GPU memory pool、session state 不应该混进 checkpoint cache。混在一起会让重启恢复语义变得很危险。

工程上常见分层是：

```text
object store:
  权威来源，可靠但可能慢

node-local SSD:
  checkpoint cache，跨进程或跨重启复用

CPU memory:
  热权重或 adapter 暂存

GPU memory:
  已加载模型和活跃 KV cache
```

面试里可以这样答：

```text
checkpoint cache 适合大模型 artifact、重复访问、远端下载慢、本地 SSD 充足、serverless 或多区域冷启动场景。不适合一次性模型、revision 频繁变化、本地磁盘很小或很慢、模型不允许落盘、没有容量规划的超大模型，或者已经有高性能共享存储且本地缓存收益不明显的场景。它缓存的是 artifact，不是 GPU runtime state。
```

## Q058. checkpoint cache 和相近概念最容易混淆的边界在哪里？

**回答：**

checkpoint cache 最容易和 model cache、KV cache、prefix cache、Docker image cache、Hugging Face Hub cache 混淆。它们都叫 cache，但缓存对象、生命周期和命中后的效果完全不同。

边界可以这样划：

```text
checkpoint cache:
  缓存模型文件或 shard，例如 safetensors、bin、tokenizer、adapter artifact。
  命中后减少下载，但仍要加载到 runtime。

model cache / GPU resident cache:
  缓存已加载到 GPU 或推理 runtime 的模型。
  命中后可以直接或接近直接服务请求。

KV cache:
  缓存某个请求或 session 在 attention 中的 key/value。
  它是运行时 token 状态，不是模型文件。

prefix cache:
  缓存共享 prompt prefix 对应的 KV block。
  它依赖具体 prompt、tokenizer、模型和安全域。

Docker image cache:
  缓存容器镜像层。
  容器启动快不代表 checkpoint 或模型已 ready。

Hugging Face Hub cache:
  是通用 artifact download cache。
  可以作为 checkpoint cache 的底层来源，但不等于 serving 层可用状态。
```

一个常见误判是：看到 `~/.cache/huggingface/hub` 里有模型文件，就认为服务可以直接跑。实际上这只说明下载层可能命中。推理服务还要确认 revision、文件完整性、模型格式、dtype、分片、tokenizer、runtime 配置，最后还要把权重加载到 GPU。

另一个误判是把 checkpoint cache 命中等同于 cold start 消失。checkpoint cache 命中只消除了远端 fetch，不消除本地 read、反序列化、权重映射、GPU transfer、CUDA/NCCL 初始化和 warmup。LogServe 的指标拆分也反映了这一点：cache hit 时 `CheckpointFetchMs` 可以是 0，但 `ModelLoadMs` 仍然会存在。

版本边界也很重要。Hugging Face Hub cache 用 refs、snapshots、blobs 组织 revision，这能避免同名文件在不同 revision 下混淆。自研 checkpoint cache 也需要类似的 revision 或 digest 概念，不能只按 `model-A-v1.checkpoint` 这种弱名字判断。

面试里可以这样答：

```text
checkpoint cache 缓存的是模型 artifact，命中后减少下载；model cache 缓存的是已加载 runtime 或 GPU 权重；KV cache 和 prefix cache 缓存的是请求 token 状态；Docker image cache 只说明容器层热；HF Hub cache 是通用下载缓存，不等于 serving ready。checkpoint cache 命中不代表 cold start 为零，它只省掉 fetch，仍可能有本地读、解析、GPU transfer 和 warmup。
```

## Q059. checkpoint cache 在高并发场景下可能出现哪些隐藏问题？

**回答：**

checkpoint cache 高并发下最容易出问题的地方是“大家都以为自己只是在读文件”。实际上它会竞争对象存储、磁盘、page cache、锁、manifest、驱逐队列和容量预算。模型文件又很大，一个小的并发失控就会变成很明显的线上事故。

典型隐藏问题：

```text
同 checkpoint 重复下载:
  没有 per-key singleflight 时，多个请求同时 miss，同一个文件被下载多次。

粗粒度锁:
  一把全局锁保护所有 checkpoint，不同模型也互相阻塞。

磁盘带宽打满:
  多个大 checkpoint 同时 copy/read，热请求也被 I/O 抖动影响。

page cache 污染:
  冷模型大文件读入 OS page cache，把热点文件挤出去。

容量竞态:
  多个加载任务都认为空间够，写完后超过 capacity。

eviction storm:
  冷模型挤掉热模型，热模型请求又触发重新下载，反复抖动。

manifest 竞争:
  多进程同时写同一个 manifest，可能出现旧 last_access 覆盖新状态。

对象存储限流:
  S3/GCS/Azure Blob 或内部仓库返回 throttling，冷启动尾延迟暴涨。

跨租户影响:
  一个租户用大量唯一模型污染 cache，其他租户 hit rate 下降。
```

LogServe 的简化实现用了 `checkpointMu` 把 checkpoint 加载串行化，所以它可以保证同一进程内并发 cold load 只有一个 miss。`TestModelCheckpointCacheSerializesConcurrentColdLoads` 验证了 12 个并发 caller 下只有 1 个 miss 和 11 个 hit。这个设计正确性很清楚，但代价是不同模型也被串行化。线上系统通常会改成 per-model lock，再加全局带宽和容量 semaphore。

比较稳的控制手段：

```text
per-key singleflight:
  同一 model/revision 只下载一次

global bandwidth limiter:
  不同模型可以并行，但总下载/读盘并发受控

capacity reservation:
  写入前先预留空间，失败就不开始下载

active reference:
  正在加载或正在服务的 checkpoint 不能被驱逐

tenant-aware eviction:
  热度、大小、加载成本和租户配额一起算

atomic manifest:
  文件和 manifest 都用临时路径 + rename 发布

negative cache:
  不存在的 checkpoint 短时间缓存错误结果，避免反复打远端
```

观测上要多看几个“缓存层指标”：

```text
checkpoint_cache_hit_rate
checkpoint_fetch_ms p50/p95/p99
checkpoint_bytes_read / written
cache_lock_wait_ms
eviction_count
cache_used_bytes / capacity_bytes
object_store_throttle_count
duplicate_download_prevented_count
```

面试里可以这样答：

```text
checkpoint cache 在高并发下会遇到重复下载、粗锁阻塞、磁盘带宽打满、page cache 污染、容量竞态、eviction storm、manifest 写竞争、对象存储限流和跨租户 cache pollution。简单实现可以用全局锁保证正确，但会牺牲不同模型并发；线上更常见的是 per-key singleflight 加全局带宽/容量限制，再配合 atomic manifest、active reference、tenant-aware eviction 和完整指标。
```

## Q060. checkpoint cache 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

checkpoint cache 的边界条件集中在“文件完整性”和“索引可信度”。checkpoint 往往很大，下载、写盘、manifest 更新、索引更新不是原子动作。崩溃或超时发生在中间时，系统必须知道哪些东西可以保留，哪些必须丢弃。

崩溃时要考虑：

```text
copy 中崩溃:
  只允许留下 .tmp 或 staging 文件，不能把半文件当成命中。

rename 后、manifest 前崩溃:
  文件可能完整但没有索引，重启扫描要决定是否忽略、补 manifest，还是重新校验。

manifest 写一半崩溃:
  manifest 必须原子发布，坏 JSON 不能进入 cache index。

eviction 中崩溃:
  可能文件删了 manifest 还在，或者 manifest 删了文件还在，扫描时要修复。

读 checkpoint 时崩溃:
  不能因为读过一次就提前更新成 ready，ready 应该属于 model runtime，不属于 checkpoint cache。
```

重启时要区分“磁盘缓存可恢复”和“模型运行时可恢复”。checkpoint cache 可以从 manifest 扫描恢复；GPU memory、CUDA context、NCCL communicator、KV cache 都不能靠磁盘 manifest 恢复。LogServe 的 README 写到 worker 重启会扫描 manifest 并报告 persisted cache entries，这对 checkpoint cache 是合理的；但如果是 GPU resident model cache，就不能只靠 manifest 声明 ready。

超时和取消要处理两个层面：

```text
下载超时:
  关闭文件句柄，删除 tmp 文件，释放 per-key lock。

校验超时:
  不发布 manifest，不更新 cache index。

读盘超时:
  返回错误，但不要删除一个已经完整的 checkpoint，除非校验失败。

等待加载锁超时:
  当前请求可以取消等待，但后台加载是否继续要有策略。
```

LogServe 的 `copyCheckpoint` 在 `ctx.Done()` 时会 close target 并删除 `.tmp`，这是一个很好的简化边界。`writeCheckpointManifest` 也先写 `.tmp` 再 rename。需要注意的是，真实系统还应加入 checksum 或 content digest，否则只能知道文件大小和路径，不能证明内容就是预期 revision。

重试时最容易重复写。必须让 retry 复用同一个 key：

```text
checkpoint key:
  model + version + revision/digest + format + quantization + adapter

download attempt id:
  用于临时文件名和日志，不用于最终 cache key

request retry id:
  用于避免同一用户请求触发多个独立冷启动
```

如果第一次下载成功但响应在返回前超时，第二次重试应该命中已经发布的 checkpoint；如果第一次只写了临时文件，第二次必须重新下载或从可验证的断点继续，不能直接把临时文件改名。

面试里可以这样答：

```text
checkpoint cache 的故障边界是文件完整性和索引可信度。copy 中崩溃只能留下 tmp，不能命中；manifest 必须 atomic publish；重启扫描要处理文件在但 manifest 缺、manifest 在但文件缺、坏 JSON、eviction 半完成。超时要删除临时文件并释放锁，重试要按 model revision/digest 复用同一个 cache key。磁盘 checkpoint 恢复不等于 GPU 模型 ready，这是最容易出错的边界。
```

## Q061. checkpoint cache 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

checkpoint cache 的瓶颈通常先来自网络和 I/O，然后才轮到 CPU、内存和锁竞争。它缓存的是模型 artifact，所以它的关键路径是“从远端或源目录拿到文件，再把文件可靠地放到本地缓存”。这和 model cold start 不一样：model cold start 还包括反序列化、GPU 搬运、runtime init 和 warmup；checkpoint cache 本身主要管 artifact 获取和本地复用。

分阶段看会更清楚：

```text
远端 miss:
  网络、对象存储吞吐、跨区域延迟、认证、TLS、仓库限流。

本地 miss 但源目录在同机或同机房:
  磁盘读写、共享文件系统吞吐、文件系统元数据、rename/fsync 行为。

cache hit:
  通常不再走网络，瓶颈变成本地 stat、manifest 读取、锁等待、磁盘读。

高并发:
  singleflight 锁、容量锁、eviction 锁、manifest 写锁会变成排队点。

大文件:
  OS page cache、内存带宽、copy buffer、checksum、压缩/解压会变明显。
```

CPU 不是完全不重要。做 checksum、hash、签名验证、manifest JSON 解析、路径规范化、压缩格式处理时都会用 CPU。只是对几十 GB 的 checkpoint 来说，远端下载、磁盘读写和内存拷贝通常更容易成为主瓶颈。

内存瓶颈主要体现在两处。一是 page cache 被大 checkpoint 污染，热文件被挤出去；二是实现不小心把大文件一次性读进内存，而不是流式 copy。LogServe 的 `copyCheckpoint` 用 64KB buffer 流式读写，这种简化实现至少避免了把 checkpoint 全量读入内存。

锁竞争要看锁粒度。LogServe 当前用 `checkpointMu` 串行化 checkpoint 加载，这能保证同一进程内并发 cold load 不会重复 copy，但不同模型也会排同一把锁。对教学型和机制验证系统，这个设计简单可靠；生产系统通常会改成 per-key singleflight，再配一个全局 I/O 带宽和容量 semaphore。

一个排障时很好用的分解是：

```text
source_resolve_ms:
  找源文件或远端 URL 的时间

lock_wait_ms:
  等 per-model / global cache lock 的时间

fetch_ms:
  网络下载或从源目录 copy 到 cache dir 的时间

manifest_ms:
  写 manifest、rename、更新 index 的时间

eviction_ms:
  腾空间和删除旧文件的时间

hit_lookup_ms:
  cache hit 时 stat、manifest、index 查询的时间
```

面试里可以这样答：

```text
checkpoint cache 的瓶颈大多先出在网络和 I/O：远端对象存储、跨区域下载、本地磁盘读写、共享文件系统和大文件 copy。CPU 主要消耗在 checksum、manifest、压缩解压和路径处理；内存问题通常是 page cache 污染或错误地全量读文件；高并发时锁竞争、容量预留和 eviction 会放大尾延迟。所以要把 fetch_ms、lock_wait_ms、eviction_ms、manifest_ms、hit_lookup_ms 分开观测，不能只看一个 cache_miss_ms。
```

## Q062. checkpoint cache 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

checkpoint cache 的三类测试要分开。correctness test 保证缓存语义对；stress test 逼出并发和故障问题；benchmark 才回答“快不快”。如果只跑 benchmark，很容易在一个干净目录里测出漂亮数字，但线上一重启、一并发、一驱逐就出错。

correctness test 应该测这些：

```text
cold miss:
  第一次请求不存在本地 checkpoint，应 copy/fetch 并返回 cache_hit=false。

warm hit:
  第二次请求同一个 model/version，应命中本地文件，不再 fetch。

cache key:
  model、version、revision、digest、dtype、quantization、adapter 不同不能误命中。

manifest:
  manifest 写入后，重启扫描能恢复索引；坏 manifest 不应进入 index。

atomic publish:
  .tmp 文件不能被当成可用 checkpoint。

capacity:
  checkpoint 大于容量时拒绝；容量不足时按策略 eviction。

eviction:
  被驱逐文件、manifest、内存 index 三者一致。

cancellation:
  fetch/copy 被取消时删除临时文件并释放锁。
```

LogServe 的 `model_cache_test.go` 覆盖了几个核心点：第一次 copy、第二次 hit、容量触发 LRU、重启扫描 manifest、并发 cold load 被串行化成 1 个 miss 和多个 hit。这就是 checkpoint cache correctness test 的好例子。

stress test 主要测“并发时还守不守得住语义”：

```text
同一模型 100 个并发 miss:
  真实 fetch 次数应为 1，其余等待或命中。

不同模型并发 miss:
  检查是否被粗锁全部串行化，I/O 和队列是否可控。

容量接近满:
  大量模型进入时 eviction 是否抖动，used_bytes 是否准确。

慢存储 / 断网 / 限流:
  timeout 后临时文件、manifest、锁是否清理。

多进程共享 cache dir:
  manifest 写入、rename、eviction 是否会互相踩。

崩溃注入:
  在 copy、rename、manifest write、eviction 各阶段杀进程。
```

benchmark 需要明确分布和硬件：

```text
cold fetch latency:
  p50/p95/p99 fetch_ms，按 checkpoint size 分桶。

warm hit latency:
  hit_lookup_ms、本地 read_ms、manifest_ms。

throughput:
  每秒下载 GB、每秒恢复多少模型、并发 fetch 成功率。

resource:
  network throughput、disk read/write、CPU、RSS、page cache、lock wait。

cache behavior:
  hit rate、eviction count、duplicate fetch prevented、capacity utilization。

end-to-end impact:
  checkpoint cache hit 对 model cold start、TTFT、p99 的真实影响。
```

benchmark 时必须控制模型大小、文件数量、shard 数、源存储位置、本地磁盘类型、并发度和缓存冷热状态。否则两个 benchmark 的数字不能比较。

面试里可以这样答：

```text
correctness test 测冷 miss、热 hit、cache key、atomic publish、manifest 恢复、容量、eviction 和取消清理。stress test 测同模型并发只 fetch 一次、不同模型并发是否被粗锁拖慢、容量抖动、慢存储、共享目录和崩溃注入。benchmark 测 cold/warm latency、fetch 吞吐、本地读写、lock wait、page cache、hit rate、eviction 和对 TTFT/p99 的影响。三类测试关注点不同，不能互相替代。
```

## Q063. 如果要求从零实现一个简化版 checkpoint cache，你会先定义哪些不变量？

**回答：**

我会先定义文件和索引不变量。checkpoint cache 的核心风险是“把不完整或不属于这个模型的文件当成命中”。只要这个风险没守住，后面再快也没有意义。

第一组是身份不变量：

```text
cache key 至少包含:
  model_name
  model_version / revision
  content digest 或 checksum
  file format
  dtype / quantization
  adapter_id / adapter_revision
  tenant / security domain
```

不能只按文件名或 model name 命中。`model-A:v1` 如果背后 checkpoint 换了 revision，旧缓存必须失效。Hugging Face Hub cache 用 snapshots、refs、blobs 区分 revision 和真实文件内容，自研实现也应该学这个边界。

第二组是发布不变量：

```text
下载只写 tmp 文件
校验通过后才能 rename 到最终路径
manifest 也必须 tmp + rename 原子发布
只有最终路径和完整 manifest 同时存在，才能进入 index
坏 JSON、缺字段、指向目录、指向不存在文件都不能恢复
```

第三组是并发不变量：

```text
同一 cache key 同一时刻只有一个 fetch
等待者复用 fetch 结果
取消一个请求不应破坏其他等待者
不同 cache key 能否并行由带宽和容量预算决定
eviction 不能删除正在 fetch、正在校验或正在使用的 checkpoint
```

第四组是容量不变量：

```text
写入前必须知道 size 或预留容量
单个 checkpoint 超过 capacity 时直接失败或走 no-cache 模式
eviction 后 used_bytes、index、manifest、文件必须一致
容量统计不能因为失败或重复删除变成负数
```

第五组是安全和租户不变量：

```text
不同 tenant / 权限域不能共享私有 checkpoint
缓存目录权限不能放开给非授权用户
manifest 不能允许路径穿越
清理逻辑不能删除 cache dir 之外的文件
```

第六组是观测不变量：

```text
每次 fetch/hit/evict 都要记录:
  model key
  cache hit/miss
  bytes
  fetch_ms
  lock_wait_ms
  source
  eviction_count
  error_phase
```

LogServe 的简化实现已经有几个关键点：safeModelFileName 做基本路径规整，copy 用 `.tmp`，manifest 用 `.tmp`，重启扫描会检查 manifest 和 checkpoint 文件存在。真实系统还应该加 digest 校验和 per-tenant namespace。

面试里可以这样答：

```text
我会先定义不变量：cache key 必须绑定模型 revision/digest、格式、dtype、adapter 和租户；半文件和坏 manifest 不能进入 index；同一 key 只能有一个 fetch；容量必须先预留或严格回滚；eviction 不能删活跃文件；重启扫描只能恢复完整、校验过的文件；路径和权限不能越界。实现可以简单，但这些边界不能省。
```

## Q064. checkpoint cache 的常见误用是什么，误用后通常会产生什么线上症状？

**回答：**

checkpoint cache 最常见的误用是把它当成“只要本地有文件就算成功”。这会把下载缓存、模型 ready、GPU resident、权限边界混在一起，线上症状往往很难看。

常见误用包括：

```text
把 checkpoint cache 当成 model cache:
  症状是缓存命中率很高，但 TTFT 仍然高，因为还要加载 GPU 权重。

按 latest 或弱版本命中:
  症状是发布后部分 worker 继续用旧 checkpoint，结果不一致。

没有 checksum / digest:
  症状是坏文件、半文件、旧文件被误命中，加载阶段才报错。

不做 atomic rename:
  症状是并发请求读到正在写的文件。

没有容量限制:
  症状是本地盘写满，worker 健康检查失败，日志和临时文件也写不进去。

跨租户共享私有 checkpoint:
  症状是数据和模型资产泄露，审计无法解释。

所有模型共用一把锁:
  症状是一个大模型下载阻塞所有小模型 hit/miss。

把缓存放在慢盘或网络盘:
  症状是 warm hit 仍然慢，甚至比远端 streaming 更差。
```

还有一种误用是只看 hit rate。checkpoint cache hit rate 高，只说明少下载了文件；它不说明模型已经加载进 GPU，也不说明 prefix/KV cache 命中，更不说明 p99 一定下降。如果模型加载和 decode 才是瓶颈，checkpoint cache hit rate 提升不会明显改善用户体验。

LogServe 的 README 把 checkpoint fetch、model load、first-token、total latency 分开记录，这是避免误用的关键。面试时可以强调：checkpoint cache 是 artifact 层优化，不能拿它替代 model cache、KV cache 或 serving scheduler。

面试里可以这样答：

```text
常见误用是把本地 checkpoint 当成模型已 ready，按弱版本或 latest 命中，不做 checksum，不做 atomic publish，不设容量和租户隔离，把缓存放在慢盘上，或者只看 hit rate。线上症状包括 TTFT 仍然高、版本串线、坏文件加载失败、磁盘写满、worker 抖动、租户泄露、warm hit 也慢、p99 不降反升。
```

## Q065. checkpoint cache 在单机和分布式环境中的语义有什么差异？

**回答：**

单机里的 checkpoint cache 是一个本地文件系统问题；分布式里的 checkpoint cache 是一个 locality、租户、控制面一致性和资源预算问题。单机可以说“这个目录里有没有文件”；分布式要问“哪个 worker、哪个 node、哪个 region、哪个 tenant 可以安全地复用这个文件”。

单机语义：

```text
cache dir 是本地目录
manifest 和文件由一个进程或少量进程维护
锁通常是进程内锁或文件锁
eviction 只影响本机
重启扫描本地 manifest 即可恢复 artifact index
```

分布式语义会复杂很多：

```text
worker-local:
  每个 worker 自己有 checkpoint cache，命中只对本 worker 有意义。

node-local:
  同一节点多个 worker 共享 NVMe，需要跨进程文件锁和引用计数。

cluster-level:
  控制面要知道哪些 worker/node 有某 checkpoint，但这个信息会过期。

region-level:
  跨区域复制 checkpoint 要考虑网络成本、合规和预热策略。

tenant-level:
  私有 checkpoint 只能在对应权限域内复用。
```

分布式环境里，缓存状态不能当成强一致事实。worker heartbeat 上报“我有 model-A checkpoint”以后，worker 可能崩溃、磁盘可能被清理、eviction 可能已经发生。控制面拿到的是带时效的声明，调度时要允许 miss 后重新 fetch，或者用 lease/TTL 刷新缓存状态。

共享文件系统也不是免费午餐。它能减少重复下载，但可能把所有 worker 的 cold miss 压到同一个元数据服务或存储后端上。worker-local cache 会重复占用磁盘，但 locality 更好，故障边界也清楚。多区域系统常见做法是：权威 checkpoint 在对象存储，区域内做热点预拉取，node-local SSD 做实际 serving cache。

在 LogServe 当前边界里，它是单节点/多进程机制验证，不是生产级分布式 checkpoint cache。它用 worker heartbeat 报告 cached models，用本地 manifest 做重启恢复，这足够表达 locality-aware scheduling 的核心机制；如果扩展到多节点，需要补分布式租约、跨进程锁、cache 状态 TTL、按租户配额和清理协议。

面试里可以这样答：

```text
单机 checkpoint cache 的语义是本地目录、manifest、锁和 eviction；分布式语义要加 worker/node/region/tenant 维度。控制面看到的 cache state 是带时效的声明，不是强一致事实。共享 FS 可以减少重复下载，但可能成为元数据和 I/O 瓶颈；worker-local cache locality 好但会复制文件。生产分布式设计要有 lease、TTL、跨进程锁、引用计数、租户隔离和容量预算。
```

## Q066. KV cache 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

KV cache 的核心目标是避免在自回归生成中反复计算历史 token 的 key/value。它主要解决性能问题，顺带影响可服务的并发量和成本。Hugging Face Transformers 文档说得很直接：自回归模型一次预测一个 token，每次预测都依赖前面的 token；KV cache 保存这些计算结果，后续生成时可以复用，不用从头重算整个上下文。

一个 transformer layer 里，attention 会用到历史 token 的 K 和 V。没有 KV cache 时，每生成一个新 token，都要重新处理历史上下文：

```text
step 1:
  处理 prompt tokens

step 2:
  prompt + token_1 重新算一遍

step 3:
  prompt + token_1 + token_2 重新算一遍
```

有 KV cache 后，历史 K/V 留在缓存里，下一步只需要为新 token 计算 K/V，再和历史缓存做 attention：

```text
prefill:
  计算 prompt 的 K/V，写入 KV cache

decode step:
  只计算新 token 的 K/V
  读取历史 KV cache 做 attention
  把新 token 的 K/V 追加进 cache
```

所以它主要影响 decode 阶段的 TPOT 和吞吐。它也会影响 TTFT，因为 prefill 要建立初始 KV cache；prompt 很长时，prefill 建 cache 的时间和显存占用都很明显。

KV cache 不主要解决正确性。启用或禁用 KV cache，在理想实现下应该得到同样的模型结果；差异主要来自数值精度、量化、实现 bug 或 batch 不一致。它也不是安全机制。相反，KV cache 里保存了用户上下文的运行时表示，必须按请求、会话和租户隔离，否则可能产生隐私风险。

KV cache 的代价是显存。它的大小大致随这些因素增长：

```text
layers
batch / active sequences
sequence length
KV heads
head dimension
dtype bytes
beam width / parallel samples
```

这也是 vLLM PagedAttention 和 TensorRT-LLM paged KV cache 要解决的问题：KV cache 会动态增长和释放，如果管理不好，显存碎片和预留浪费会限制 batch size。

面试里可以这样答：

```text
KV cache 的核心目标是性能：缓存历史 token 在每层 attention 的 K/V，decode 时只为新 token 计算增量，避免每步重算整个上下文。它能降低 TPOT、提高吞吐，也让长上下文生成可承受。代价是显存随 batch、序列长度、层数和 dtype 增长。它不是正确性或安全功能；正确实现下结果应一致，但 KV cache 本身包含用户上下文状态，必须隔离和及时释放。
```

## Q067. KV cache 的典型适用场景和不适用场景分别是什么？

**回答：**

KV cache 适合几乎所有自回归解码场景，尤其是长 prompt、长 output、多轮聊天、流式输出和高并发 serving。没有它，LLM 每生成一个 token 都要反复处理历史上下文，成本会非常高。

适用场景：

```text
自回归文本生成:
  chat completion、completion、代码生成、摘要、翻译。

长上下文:
  RAG 文档、代码仓库上下文、长对话历史。

流式输出:
  每个 decode step 都要尽快产生下一个 token，KV cache 能降低 TPOT。

多轮对话:
  如果系统支持保留会话状态，可以复用已有上下文的 KV。

beam search / parallel sampling:
  多个分支共享前缀时，block sharing 可以减少重复 KV。

prefix caching:
  多请求共享 system prompt、tool schema、few-shot 或文档 prefix。
```

不适用或收益有限的场景：

```text
非自回归模型:
  embedding、rerank、classification、encoder-only 模型通常不需要 decode KV cache。

一次性短输出:
  prompt 和 output 都很短时，KV cache 仍会用，但收益不明显。

显存极紧:
  KV cache 可能让系统 OOM，需要 offload、quantized cache、缩小 batch 或限制 max tokens。

强无状态服务:
  如果每次请求都必须完全独立，不能保留跨请求 KV，只能在请求内部使用。

多租户强隔离:
  可以用 KV cache，但不能跨租户复用，也不能把 cache 状态泄露给另一个请求。

不支持 attention 的架构:
  例如某些 state space 模型不使用传统 K/V cache，缓存语义不同。
```

Hugging Face 文档里列了 DynamicCache、StaticCache、offloaded cache、QuantizedCache 等实现。这说明“用 KV cache”不是一个固定答案。显存充足时，on-device dynamic/paged cache 速度好；显存紧张时，offload 或 quantized cache 可以避免 OOM，但会牺牲一部分吞吐或延迟。

在 serving 系统里，KV cache 的适用性还要看调度策略。长请求如果占据大量 KV blocks，会减少短请求可用 batch slot；prefix cache 如果命中率高，可以提升长 prompt workload 的效率；如果 workload 全是短、唯一、低并发请求，复杂的跨请求 KV 复用收益会低。

面试里可以这样答：

```text
KV cache 适合自回归生成，尤其是长上下文、长输出、流式输出、多轮聊天、beam search、parallel sampling 和 prefix caching。它不太适合 embedding/rerank/classification 这类非自回归任务；短 prompt 短 output 收益有限；显存很紧时要考虑 offload 或量化；强租户隔离下不能跨安全域复用。关键是区分请求内部 KV cache 和跨请求 prefix/session KV reuse。
```

## Q068. KV cache 和相近概念最容易混淆的边界在哪里？

**回答：**

KV cache 最容易和 prefix cache、checkpoint cache、model cache、activation cache、application cache 混淆。名字里都有 cache，但缓存对象完全不同。

可以这样划边界：

```text
KV cache:
  缓存某个请求或会话在每层 attention 的 K/V 张量。
  主要在 GPU 或 CPU/GPU 分层内存中，生命周期通常跟请求或会话绑定。

prefix cache:
  复用共享 prompt prefix 对应的 KV blocks。
  本质上是 KV cache 的跨请求复用方式之一。

checkpoint cache:
  缓存模型权重文件、safetensors、adapter artifact。
  在磁盘或对象存储层，命中后仍要加载模型。

model cache:
  缓存已加载到 runtime/GPU 的模型权重和执行环境。
  它解决 model load，不解决每个请求的 token 状态。

activation cache:
  更常见于训练或模型内部优化，不等同于 serving decode 的 past K/V。

application cache:
  缓存业务响应、RAG 检索结果、工具结果，不是 attention 状态。
```

一个常见误解是“prefix cache 就是 KV cache”。更准确地说，prefix cache 使用 KV cache 作为底层缓存对象，但它多了跨请求匹配、权限边界、hash、block sharing 和 eviction 策略。普通 KV cache 可以只服务当前请求；prefix cache 要判断另一个请求能不能安全复用这段 prefix。

另一个误解是“KV cache 能重启恢复”。一般不能。KV cache 是运行时 tensor 状态，和具体模型实例、GPU memory、dtype、position、rope scaling、attention backend、batch layout 绑定。进程崩溃后，磁盘 checkpoint 还在，但 KV cache 通常没了。除非系统显式做 KV offload、KV transfer 或外部 KV store，否则不能假装它可恢复。

还有一个边界是 correctness。KV cache 命中不等于“可以跳过所有计算”。decode 仍然要为新 token 算 Q/K/V、读历史 K/V、做 attention、更新 cache。缓存的是历史，不是未来。

面试里可以这样答：

```text
KV cache 缓存的是请求或会话的 attention K/V 张量；prefix cache 是跨请求复用共享 prefix 的 KV blocks；checkpoint cache 是磁盘上的模型文件；model cache 是已加载到 GPU/runtime 的权重；业务 cache 缓存检索或响应。KV cache 通常不能靠重启恢复，也不能跨租户随便复用。它减少历史 token 重算，但每个新 token 仍要执行 decode 并追加新的 K/V。
```

## Q069. KV cache 在高并发场景下可能出现哪些隐藏问题？

**回答：**

高并发下，KV cache 最大的问题是显存被动态、碎片化、不可预测地占用。每个请求的 KV cache 会随着 prompt 和 output 增长，调度器如果只看请求数，不看 token 数和剩余 KV blocks，很容易把 GPU 推到 OOM 或长尾。

典型隐藏问题：

```text
KV memory explosion:
  长 prompt、长 max tokens、多 beam、多候选生成同时进入，KV 占用快速增长。

fragmentation:
  请求长短不一，连续内存预留浪费严重，batch size 被限制。

head-of-line blocking:
  长请求占着大量 KV blocks，短请求排队。

wrong admission:
  只按 request count 放行，没有按 prompt tokens + max output tokens 预留 KV。

eviction / preemption 代价:
  把请求踢出后，如果没有可恢复 KV，后续可能要重新 prefill。

prefix cache pollution:
  大量唯一 prefix 占用 block，挤掉热点 prefix。

multi-tenant interference:
  一个租户的长上下文请求占满 KV blocks，其他租户 TTFT 飙升。

offload 反压:
  KV offload 到 CPU 后，PCIe/内存带宽成为 TPOT 瓶颈。
```

PagedAttention 论文指出，传统连续分配会有预留浪费和碎片问题，KV cache 管理不好会限制 batch size。TensorRT-LLM 文档也把 contiguous KV cache 和 paged KV cache 分开：contiguous cache 是大 tensor，短序列会浪费；paged cache 由 cache manager 按 block 分配和回收。

高并发下还要小心估算错误。`max_tokens` 是上界，不是一定会生成这么多；但调度器完全不预留又会 OOM。常见做法是：

```text
按 prompt tokens 立即分配 prefill KV
按 max output tokens 或动态预算做 admission
每步 decode 增量分配 block
超过水位时暂停接收新请求或 preempt 长请求
按 tenant 维护 KV block quota
对 prefix cache 单独设置容量和 TTL
```

观测指标要比 GPU memory 更细：

```text
active_sequences
allocated_kv_blocks
free_kv_blocks
kv_cache_used_bytes
prefix_cache_hit_rate
preemption_count
kv_oom_count
evicted_blocks
queue_wait_by_prompt_length
TPOT by active_kv_tokens
```

面试里可以这样答：

```text
高并发下 KV cache 的隐藏问题是显存动态增长和碎片化。长 prompt、长 output、beam search、多租户和 prefix cache pollution 会迅速吃掉 KV blocks；只按请求数调度会误判容量；长请求会拖慢短请求；offload 会把瓶颈转移到 CPU/PCIe。解决思路是 paged/block KV 管理、按 token admission、free block 观测、tenant quota、preemption 策略和 prefix cache 容量控制。
```

## Q070. KV cache 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

KV cache 的故障边界比 checkpoint cache 更硬：它通常是进程和 GPU 内存里的运行时状态，崩溃后基本消失。checkpoint cache 重启后还能扫描 manifest，KV cache 一般不能这样恢复。除非系统专门做 KV offload、KV transfer、外部 KV store 或 disaggregated serving，否则重启后应该假设 KV cache 全部失效。

崩溃场景：

```text
worker 崩溃:
  GPU memory 释放，所有 request/session KV cache 丢失。

prefill 后崩溃:
  prompt 的 KV 已经算完但未输出，重试只能重新 prefill。

streaming 中崩溃:
  已输出 token 无法无缝接上，重试会生成新序列，采样可能不同。

prefix cache 状态丢失:
  cache hit rate 暂时下降，TTFT 上升，但正确性不应受影响。
```

重启场景要避免“恢复错觉”。服务重启后，模型权重可能重新加载，checkpoint cache 可能命中，但 KV cache 不应该被控制面认为还在。调度器如果继续把 session 粘到这个 worker，并假设历史 KV 可用，会出现两种问题：轻则重新 prefill 导致延迟突然升高，重则实现错误导致位置编码、attention mask 或 token 对齐出错。

超时和取消场景要及时释放 KV blocks：

```text
客户端取消:
  停止 decode，释放该 request 的 KV blocks。

prefill 超时:
  已分配的 prompt KV 要回收，不能留在 block table。

decode 超时:
  释放增量 blocks，更新 active sequence 状态。

等待队列超时:
  如果还没分配 KV，不能占用预算；如果已分配，要回滚。
```

vLLM 的 KVCacheManager API 里有 `free(request)` 这种接口，说明请求结束或取消后释放 blocks 是核心路径。释放顺序、引用计数和共享 prefix 都要小心：如果多个请求共享 prefix block，不能因为一个请求取消就把共享 block 直接删掉；如果引用计数没有减干净，又会造成显存泄漏。

重试场景最容易误以为“可以复用上一次 KV”。一般请求级重试不能复用旧 KV，除非 retry 被路由到同一 worker、同一模型实例、同一 tokenizer、同一 prompt token 序列、同一 position 状态，并且旧 KV 没有释放。这个条件太强，普通系统不应该依赖。更稳的语义是：重试重新执行，必要时用已经输出的文本做 continuation，但不要承诺 bit-level continuation。

面试里可以这样答：

```text
KV cache 在崩溃和重启后通常全部失效，因为它是 GPU/runtime 里的请求状态，不像 checkpoint cache 可以扫描 manifest 恢复。超时和取消必须释放 KV blocks；共享 prefix 要靠引用计数，不能误删也不能泄漏。流式输出失败后一般不能无缝重试，重新执行可能生成不同 token。除非系统明确实现 KV offload/transfer/lease，否则控制面不要把 KV cache 当成可持久状态。
```

## Q071. KV cache 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

KV cache 的主要瓶颈通常来自 GPU 显存容量和显存带宽。CPU、锁、I/O、网络也会参与，但它们一般不是最先出现的瓶颈，除非你做了 KV offload、跨机 KV transfer、外部 KV store，或者 KV cache manager 实现得太粗。

先看普通单机 GPU serving。KV cache 是每层 attention 的 K/V 张量，decode 每一步都要读历史 K/V，再把新 token 的 K/V 追加进去。请求越多、上下文越长、输出越长，KV cache 占用越大。GPU 的 HBM 容量决定能同时容纳多少 active tokens，HBM 带宽决定 decode 每步读历史 K/V 的速度。

可以按瓶颈来源拆：

```text
GPU memory capacity:
  KV blocks 不够，新的请求进不来，或者长请求触发 OOM。

GPU memory bandwidth:
  decode 阶段大量读取历史 K/V，TPOT 变慢。

fragmentation:
  连续分配或粗粒度 block 导致可用显存看起来够，但无法有效分配。

allocator / block manager:
  allocate/free、refcount、prefix sharing、eviction 过慢，调度迭代被拖慢。

CPU memory:
  KV offload 到 CPU 后，CPU 内存容量和内存带宽变成瓶颈。

PCIe / NVLink:
  offload、prefill-decode disaggregation、跨 GPU KV transfer 时会出现。

network:
  跨节点 KV transfer、远程 KV store、prefill/decode 分离部署时才明显。

I/O:
  一般不该在常规 KV cache 路径上出现；如果落盘，说明已经是特殊 offload 或恢复机制。
```

PagedAttention 论文和 TensorRT-LLM 文档都强调了同一个问题：KV cache 动态增长，连续分配会浪费大量空间，paged/block-based 管理可以降低碎片和预留浪费。vAttention 这类后续工作又说明，分页管理虽然减少碎片，也会带来 kernel 和内存管理复杂度。也就是说，KV cache 的瓶颈不是“有没有缓存”这么简单，而是缓存布局、分配粒度和 attention kernel 是否配合。

锁竞争通常来自 KV cache manager，而不是模型计算本身。比如每个 decode step 都要拿全局锁分配 block、释放 block 或更新 prefix refcount，那么在高并发下锁会进 p99。更好的做法是减少全局锁，把 hot path 做成 per-worker、per-GPU 或分片结构，并把昂贵的回收和统计放到异步路径。

面试里可以这样答：

```text
KV cache 的首要瓶颈是 GPU 显存容量和显存带宽：容量决定能放多少活跃 token，带宽影响 decode 读历史 K/V 的 TPOT。其次是碎片和 block manager 开销，特别是 allocate/free、refcount、prefix sharing 和 eviction。CPU、PCIe、网络、I/O 只有在 KV offload、跨节点 KV transfer、disaggregated serving 或外部 KV store 场景下才会变成主瓶颈。排障时要看 allocated/free KV blocks、KV usage、block allocation latency、TPOT 和 preemption，而不是只看总 GPU 利用率。
```

## Q072. KV cache 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

KV cache 的测试要特别小心，因为它既影响性能，也可能悄悄影响输出正确性。正确实现下，用不用 KV cache 应该得到等价结果；一旦 position、mask、block 映射或 refcount 出错，模型可能不报错，但输出会变坏。

correctness test 先测语义：

```text
with-cache vs no-cache:
  固定随机种子、greedy decode 下，启用 KV cache 和禁用 KV cache 输出应一致。

position ids / RoPE:
  追加 token 时位置必须连续，不能因为 chunk、page、resume 发生偏移。

attention mask:
  新 token 只能看合法历史，不能看未来 token，也不能漏掉历史 token。

block table:
  logical block 到 physical block 的映射正确，非连续物理块不影响 attention 结果。

append:
  每步 decode 后，新 K/V 只追加到当前 sequence 的正确位置。

free:
  请求结束、取消、失败后，KV blocks 被释放。

sharing:
  prefix cache、beam search、parallel sampling 的共享 block 引用计数正确。

copy-on-write:
  共享前缀后分支生成不同 token，不能互相污染。

dtype / quantized cache:
  低精度 KV cache 的质量退化在可接受范围内，不能出现结构性错误。
```

stress test 测并发和极端状态：

```text
长短混合:
  长 prompt + 短 prompt、长 output + 短 output 混在一起。

高并发:
  大量请求同时 prefill 和 decode，观察 block 分配和释放是否稳定。

接近容量:
  KV 使用率接近上限时，admission、preemption、offload 是否正确。

取消风暴:
  大量客户端断开，KV blocks 是否及时回收。

prefix sharing:
  多请求共享长前缀，同时有请求完成、取消、分叉。

多租户:
  一个租户长上下文刷满 KV，其他租户是否被保护。

故障注入:
  在 prefill 后、decode 中、streaming 中、释放前杀 worker 或取消 context。
```

benchmark 测真实性能，不只测 tokens/s：

```text
TTFT:
  包含 prefill 建 KV 的时间。

TPOT / ITL:
  decode 阶段读取和更新 KV 的速度。

KV memory:
  used bytes、allocated blocks、free blocks、fragmentation。

throughput:
  output tokens/s、request/s、active sequences。

overhead:
  block allocation latency、free latency、prefix lookup latency、refcount update latency。

offload:
  CPU/GPU transfer bandwidth、offload hit rate、TPOT 退化。

quality:
  quantized/offloaded cache 下的业务 eval、长上下文召回、结构化输出通过率。
```

面试里可以这样答：

```text
correctness test 要证明 KV cache 不改变语义：with-cache 和 no-cache 输出一致，position、mask、block table、append、free、sharing、copy-on-write 都正确。stress test 要打长短混合、高并发、接近容量、取消风暴、prefix sharing、多租户和故障注入。benchmark 要看 TTFT、TPOT、tokens/s、KV used/free blocks、fragmentation、allocation/free overhead、offload 开销和质量退化。只测吞吐不够，KV cache bug 很容易表现成尾延迟和偶发输出异常。
```

## Q073. 如果要求从零实现一个简化版 KV cache，你会先定义哪些不变量？

**回答：**

我会先定义“这块 KV 到底属于谁、代表哪些 token、什么时候可以被读写和释放”。KV cache 不像 checkpoint cache 那样可以靠文件名判断，它是运行时内存状态；一旦身份、位置或生命周期错了，输出可能直接被污染。

第一组是不变量是身份和形状：

```text
KV cache entry 必须绑定:
  model instance
  layer id
  request / sequence id
  logical token range
  physical block id
  dtype
  kv heads
  head dimension
  device / rank
```

不同模型、不同 LoRA、不同 tokenizer、不同 RoPE scaling、不同 tensor parallel rank 的 KV 不能混用。即使 token 文本一样，只要底层模型实例不同，K/V 也不是同一个东西。

第二组是位置不变量：

```text
token position 单调递增
logical block 顺序必须完整
attention mask 与 position 对齐
prefill 写入的 token 数和 scheduler 记录一致
decode 每步只追加新 token 的 K/V
```

第三组是所有权不变量：

```text
一个 physical block 要么空闲，要么被一个 sequence 独占，要么被多个 sequence 共享并有 refcount
共享 block 不能被任意写
分支写入时必须 copy-on-write
refcount 为 0 后才能回收
请求结束、取消、失败都必须走释放路径
```

第四组是容量不变量：

```text
已分配 blocks <= 总 blocks
调度器 admission 必须先检查可用 KV 预算
接近水位时要拒绝、排队、preempt 或 offload
不能在 kernel 执行中释放仍被读取的 block
```

第五组是隔离不变量：

```text
tenant A 的 KV 不能被 tenant B 直接复用
request id 不能重复导致 block 串线
prefix cache key 必须包含 model、tokenizer、template、tenant/security scope
释放后的 block 再分配前不能保留可被读出的旧上下文
```

第六组是故障不变量：

```text
worker 崩溃后 KV cache 默认失效
重启后不能声明旧 KV 还存在
超时/取消后必须最终释放
跨节点 KV transfer 必须有 lease、校验和失败回滚
```

面试里可以这样答：

```text
我会先定义 KV cache 的身份、位置、所有权、容量、隔离和故障不变量。每个 block 必须知道属于哪个 model/layer/request/token range/device；token position 和 attention mask 必须对齐；共享前缀要 refcount 和 copy-on-write；已分配 blocks 不能超过预算；请求结束、取消、失败必须释放；跨租户不能复用；worker 重启后旧 KV 默认失效。实现可以先简单连续分配，但这些不变量不能省。
```

## Q074. KV cache 的常见误用是什么，误用后通常会产生什么线上症状？

**回答：**

KV cache 的常见误用可以分两类：一类导致性能问题，另一类导致输出或隐私问题。后者更危险，因为系统可能不报错，只是回答质量变差、上下文串线，甚至泄露别人的信息。

常见误用：

```text
把 KV cache 当成持久缓存:
  症状是重启后还试图复用旧 session，结果重新 prefill 或状态错误。

跨模型/版本复用 KV:
  症状是输出异常、事实性下降、格式错乱，甚至直接 shape mismatch。

跨 tokenizer / chat template 复用:
  症状是 token position 对不上，模型像“读错上下文”。

prefix cache key 太粗:
  症状是不同租户或不同权限 prompt 误命中，造成数据泄露。

不释放 KV blocks:
  症状是显存慢性泄漏，KV usage 只涨不降，最后 OOM。

只按 request 数做容量控制:
  症状是少量长 prompt 或长 output 把 KV blocks 吃光。

盲目 offload:
  症状是 OOM 少了，但 TPOT 变差，流式输出一卡一卡。

过度预分配 max context:
  症状是实际 token 很少，但显存被静态 cache 浪费。

忽略共享 block refcount:
  症状是一个请求取消后，另一个共享 prefix 的请求突然出错。
```

还有一个很容易被低估的误用：把 prefix cache hit rate 当成 KV cache 健康指标。prefix cache hit rate 高，只说明共享前缀复用得好；它不说明 KV block 是否碎片化，不说明 offload 是否拖慢 TPOT，也不说明租户之间是否公平。

线上症状通常是：

```text
p99 TTFT 升高:
  长 prefill 或 prefix cache 失效导致。

TPOT 抖动:
  KV 读带宽、offload 或 decode batch 被拖慢。

OOM / preemption 增多:
  KV blocks 不够或释放不及时。

短请求被拖慢:
  长请求占用大量 KV，admission 不公平。

输出异常:
  position、mask、block mapping、copy-on-write 出错。

隐私事故:
  prefix/KV 误跨租户复用。
```

面试里可以这样答：

```text
KV cache 常见误用包括把它当持久缓存、跨模型或 tokenizer 复用、prefix key 太粗、不释放 blocks、只按请求数限流、盲目 offload、过度静态预分配、忽略共享 block refcount。线上症状是 TTFT/p99 升高、TPOT 抖动、KV usage 持续上涨、OOM 和 preemption 增多、短请求被长请求拖慢、输出质量异常，严重时会出现跨租户上下文泄露。
```

## Q075. KV cache 在单机和分布式环境中的语义有什么差异？

**回答：**

单机 KV cache 是“这个进程、这块 GPU 上的运行时状态”；分布式 KV cache 是“多个 rank、多个 worker、甚至多个阶段之间的可转移状态”。这两者的复杂度差很多。

单机语义：

```text
KV blocks 属于本进程的某个模型实例
通常在本 GPU 或 CPU/GPU offload 内存里
scheduler、block manager、attention kernel 在同一 worker 内协作
请求结束后本地释放
worker 崩溃后 KV 全部失效
```

分布式语义要多几层：

```text
tensor parallel:
  每个 rank 只持有自己负责的 KV shard，不能单独代表完整请求状态。

pipeline parallel:
  不同 layer 的 KV 在不同 stage 上，释放和重试要跨 stage 协调。

data parallel replicas:
  某个 replica 有 session KV，不代表另一个 replica 也有。

prefill/decode disaggregation:
  prefill GPU 生成 KV，decode GPU 使用 KV，需要 KV transfer 和 ownership handoff。

multi-node serving:
  KV 传输会受网络、RDMA/NVLink、序列化格式、lease 和失败恢复影响。
```

分布式环境里，sticky routing 变得重要。同一个会话如果每轮都打到不同 replica，就无法复用本地 KV 或 prefix cache。Hugging Face TGI 文档也提到，多副本后同一用户不一定命中同一 replica，缓存收益可能消失；sticky session 可以提升命中，但不能盲用，因为它会带来负载不均。

在 prefill/decode 分离架构中，KV cache 还会变成一种传输对象。prefill 端算出 prompt KV，decode 端接管后继续生成。这里需要明确：

```text
KV 属于哪个 request
传输是否完整
decode 端是否确认接收
prefill 端什么时候释放
失败后是否重新 prefill
lease 过期怎么处理
```

LogServe 当前不是生产级分布式 KV cache 系统。它通过 mock/vLLM adapter 表达 LLM 请求生命周期，通过 worker cache 上报表达模型 locality，但不实现真实 KV block 管理。面试里应把这点讲清楚：可以解释 KV cache 的系统设计，但不要把 LogServe 当前实现说成已经支持分布式 KV transfer。

面试里可以这样答：

```text
单机 KV cache 是本进程本 GPU 的请求状态，生命周期跟 worker 和 request 绑定。分布式下要考虑 tensor/pipeline parallel 的 KV shard、data parallel replica locality、sticky routing、prefill/decode disaggregation、跨节点 KV transfer、lease、确认和失败回滚。一个 replica 有 KV 不代表其他 replica 有。LogServe 当前可以验证 LLM 请求和模型 locality 机制，但没有实现真实分布式 KV cache。
```

## Q076. prefill 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

prefill 的核心目标是处理输入 prompt，并为后续 decode 建立初始状态。它会把 prompt tokens 一次性送入模型，计算每一层的 attention K/V，填充初始 KV cache，然后得到可以生成第一个 token 的状态。它主要是推理计算阶段，不是一个“解决某类工程问题”的模块；如果硬要归类，它最直接影响性能，尤其是 TTFT。

prefill 做的事情可以理解为：

```text
输入:
  prompt tokens

计算:
  对 prompt 全量做 transformer forward

产物:
  初始 KV cache
  最后位置的 hidden state / logits
  第一个输出 token 的采样基础

后续:
  decode 从这个 KV cache 继续逐 token 生成
```

Sarathi-Serve 论文把 LLM inference 拆成 prefill 和 decode 两个阶段：prefill 处理整个输入 prompt 并产生第一个输出 token；decode 逐 token 生成后续输出。这个拆分是理解 LLM serving 调度的基础。

prefill 和正确性有关，但不是“正确性机制”。如果 tokenization、chat template、position ids、attention mask 或 RoPE 处理错，prefill 当然会算错；但 prefill 的存在不是为了保证正确性，而是模型生成前必须执行的计算。安全性也类似，prefill 会处理敏感 prompt，因此需要隔离和日志控制，但它不是安全功能本身。

性能上，prefill 有几个明显特点：

```text
并行度高:
  prompt tokens 可以一次处理，大矩阵乘法多，GPU 利用率容易上来。

延迟大:
  prompt 越长，prefill 越慢，TTFT 越高。

显存压力大:
  prefill 会写入初始 KV cache，长 prompt 直接占大量 KV blocks。

调度影响大:
  大 prefill 会阻塞正在 decode 的请求，影响 TPOT。
```

面试里可以这样答：

```text
prefill 的核心目标是把输入 prompt 处理成可继续 decode 的模型状态：计算 prompt 的 hidden states 和每层 K/V，建立初始 KV cache，并产生第一个 token 的 logits。它主要影响性能，尤其是 TTFT 和初始 KV 占用。它不是安全或可维护性机制；正确性上要保证 tokenizer、position、mask、template 和模型版本一致，否则 prefill 结果会错。
```

## Q077. prefill 的典型适用场景和不适用场景分别是什么？

**回答：**

对自回归 LLM 生成来说，只要有新的 prompt，通常就有 prefill。问题不是“要不要 prefill”，而是“prefill 多大、能不能复用、要不要切块、要不要和 decode 分离”。

典型适用场景：

```text
普通 chat/completion:
  每个新请求都要先处理 prompt。

长上下文 RAG:
  文档、代码、知识库片段拼进 prompt，prefill 成本很高。

多轮对话:
  对话历史越长，重新 prefill 越贵；如果能复用 prefix/KV，收益很大。

流式输出:
  用户等第一个 token，prefill 直接决定 TTFT。

批量离线生成:
  可以把多个 prompt 的 prefill 合批，提高吞吐。

prefill/decode disaggregation:
  长 prompt 由 prefill 专用资源处理，decode 专注稳定 TPOT。
```

不适用或收益有限的场景：

```text
embedding / rerank / classification:
  它们可能也做一次 forward，但没有自回归 decode 意义上的 prefill/decode 拆分。

prompt 为空或极短:
  prefill 仍存在，但成本很小，优化它收益有限。

prefix cache 完全命中:
  共享前缀已经有 KV，当前请求只需要处理未命中的尾部。

纯 decode continuation:
  如果系统真的保留了 session KV，下一步可以直接 decode；但普通无状态 API 很少这样承诺。

强低延迟短请求:
  chunked prefill 的调度复杂度可能不值得。
```

chunked prefill 适合长 prompt 和混合负载。TensorRT-LLM 文档里的 chunked context 会把 context phase 拆成多个 iteration，让 context chunk 和 generation phase 更好混合；SARATHI 和 Sarathi-Serve 论文也围绕 chunked-prefill 讨论如何降低 prefill 对 decode 的阻塞。短 prompt 场景下，切块反而可能增加调度开销和 kernel overhead。

面试里可以这样答：

```text
自回归生成里，新 prompt 基本都要 prefill。它特别重要于长上下文、RAG、多轮聊天、流式输出和离线批量生成；长 prompt 场景常用 chunked prefill 或 prefill/decode 分离。它不太适用于 embedding、rerank、classification 这类没有 decode 阶段的任务；极短 prompt 或 prefix cache 完全命中时，prefill 优化收益有限。重点不是是否 prefill，而是如何控制它的大小、复用和调度影响。
```

## Q078. prefill 和相近概念最容易混淆的边界在哪里？

**回答：**

prefill 最容易和 tokenization、model loading、KV cache、prefix cache、decode、TTFT 混在一起。它们都发生在第一个 token 出来之前或附近，但不是一回事。

边界可以这样划：

```text
tokenization:
  把文本变成 token id。它发生在模型 forward 前，不是 prefill。

model loading:
  把权重加载到 CPU/GPU/runtime。模型热了以后，请求仍然需要 prefill。

prefill:
  对 prompt tokens 做模型 forward，建立初始 KV cache，得到 first-token logits。

KV cache:
  prefill 的产物之一。它是状态，不是计算阶段。

prefix cache:
  复用已有 prefix 的 KV，减少当前请求需要做的 prefill。

decode:
  在 prefill 之后逐 token 生成输出，每步追加新的 KV。

TTFT:
  用户看到第一个 token 的时间，包含排队、tokenization、prefill、采样、网络等。
```

一个典型误解是“模型已经 cache hit，所以没有 prefill”。模型权重命中只说明不用加载模型，不说明 prompt 已经处理过。一个 100k token prompt，即使命中 GPU resident model，也可能有很高 prefill latency。

另一个误解是“prefix cache hit 就完全没有 prefill”。更准确地说，命中的前缀可以复用 KV，未命中的后缀仍要 prefill。调度器应该看 `uncached_prompt_tokens`，而不是只看原始 prompt length。

还要区分 prefill 和 first token sampling。prefill 产生 logits，采样或贪心选择第一个 token 是另一步。大多数系统把它们合在 TTFT 里观测，但排障时最好分开。

面试里可以这样答：

```text
prefill 是模型对 prompt tokens 做 forward 并建立初始 KV cache 的阶段。tokenization 在它之前，model loading 是权重准备，KV cache 是它的产物，prefix cache 是减少它的复用机制，decode 是它之后逐 token 生成，TTFT 是包含排队、tokenization、prefill、采样和网络的端到端指标。模型 cache hit 不等于 prefill hit；prefix cache 命中也只省共享前缀的 prefill。
```

## Q079. prefill 在高并发场景下可能出现哪些隐藏问题？

**回答：**

高并发下，prefill 最大的问题是它很容易把 GPU 的一个调度迭代变得特别重。prefill 的矩阵乘法大、GPU 利用率高，这本身不是坏事；问题是它会和正在流式 decode 的请求争资源。长 prompt 一次性 prefill，可能让其他用户看到 token 间隔突然变大。

典型隐藏问题：

```text
head-of-line blocking:
  一个超长 prompt 占住 GPU，短请求和 decode 请求都在等。

TTFT p99 飙升:
  长 prompt 排在前面，后面的短 prompt 也拿不到首 token。

TPOT 抖动:
  正在 decode 的流式请求被大 prefill 挤占，输出间隔变长。

KV cache 突增:
  prefill 结束后一次性写入大量 prompt KV，显存水位跳升。

token budget 误判:
  只按请求数 batching，没有按 prompt tokens 控制单轮成本。

prefix cache miss 风暴:
  大量长 prompt 都不命中 prefix cache，prefill 吞吐被打满。

多租户不公平:
  一个租户提交长上下文任务，把交互式短请求拖慢。

pipeline bubbles:
  在 pipeline parallel 下，不同 micro-batch prefill/decode 时间差异大，stage 空转。
```

chunked prefill 的意义就在这里。它把长 prompt 切成多个 chunk，让调度器可以把 prefill chunk 和 decode tokens 混合调度。SARATHI 论文提出用 chunked-prefills 和 decode-maximal batching，让 decode 请求搭在 prefill chunk 上，减少 decode-only batch 的低利用率和长 prefill 的阻塞。Sarathi-Serve 进一步强调在 tail latency 约束下平衡吞吐和延迟。

但 chunked prefill 也不是免费。chunk 太小，会增加调度和 kernel overhead；chunk 太大，又回到长 prefill 阻塞。生产上通常要配 token budget、队列分级、decode 优先级、租户配额和 prefix-cache-aware routing。

面试里可以这样答：

```text
高并发下 prefill 会造成队头阻塞、TTFT p99 升高、TPOT 抖动、KV cache 水位突增、token budget 误判、prefix miss 风暴、多租户不公平和 pipeline bubbles。解决思路是按 token 而不是 request 调度，长 prompt 做 chunked prefill，把 prefill chunk 和 decode 混合，限制单轮 prefill budget，给 decode 留 SLO，同时按租户和长度分桶保护短请求。
```

## Q080. prefill 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

prefill 的故障边界集中在“KV cache 是否已经可用”和“第一个 token 是否已经对外可见”。prefill 不是原子动作。长 prompt 可能被切成多个 chunk，每个 chunk 都会写一部分 KV；崩溃、超时或取消发生在中间时，系统要明确这些部分状态能不能复用。

崩溃场景：

```text
prefill 未开始:
  请求可以重新排队或重试，没有 KV 状态需要清理。

prefill 中崩溃:
  已写入的 KV 随 worker 进程/GPU 状态消失，通常需要重新 prefill。

chunked prefill 部分完成:
  如果没有持久 KV 或外部 KV store，不能只从下一个 chunk 继续。

prefill 完成但未输出:
  重试仍然可能重新 prefill，因为客户端没有拿到可恢复状态。
```

重启场景要避免“半 prefill 恢复”。checkpoint cache 可以从 manifest 恢复，prefill 生成的 KV cache 通常不能恢复。worker 重启后，即使模型权重和 checkpoint 都热，prompt 仍然要重新 prefill，除非系统明确实现了 KV 持久化或 KV transfer。

超时和取消场景：

```text
等待队列超时:
  没有分配 KV，直接从队列移除。

prefill 超时:
  停在 iteration 或 chunk 边界，释放已分配 KV blocks。

客户端取消:
  不再继续后续 prefill chunks，也不要进入 decode。

GPU kernel 已在执行:
  通常不能中途打断 kernel，只能在当前 iteration 完成后检查 cancellation。
```

重试语义要特别小心。非流式请求如果 prefill 期间失败，通常可以重新执行；代价是重新计算 prompt。流式请求如果第一个 token 还没发出去，重试相对干净；如果第一个 token 已经发出，那已经进入 decode 语义，不能把失败简单归为 prefill 问题。

如果 prefill 触发了工具调用前的上下文构造或日志写入，还要注意幂等。一般建议把“请求已开始 prefill”“first token sent”“request completed”这几个状态分开记录，这样超时和重试策略才不会混乱。

面试里可以这样答：

```text
prefill 的故障边界是部分 KV 状态和 first-token 可见性。prefill 中崩溃通常不能恢复，重启后要重新 prefill；chunked prefill 完成一半也不能默认续跑，除非有外部 KV 持久化。超时或取消要在 iteration/chunk 边界停止并释放 KV blocks；GPU kernel 通常不能中途打断。重试时如果第一个 token 没发出，可以重新执行；如果已经发出，就进入流式续接问题，语义要单独定义。
```

## Q081. prefill 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

不能只在 CPU、内存、锁、I/O、网络这几个词里选一个。prefill 是模型对 prompt tokens 做一次大块 forward，它的主瓶颈首先取决于当前是不是 warm serving、prompt 有多长、prefix cache 是否命中、是否跨 GPU 或跨节点执行。

在常见的单机 warm 模型场景里，prefill 的第一瓶颈通常不是磁盘 I/O，也不是外部网络，而是 GPU 侧的计算和显存资源。长 prompt 会带来大量矩阵乘法，prefill iteration 往往能把 GPU 算力吃满；同时它要为 prompt 写入每一层的 KV cache，显存容量、KV block 分配和显存带宽也会影响 TTFT。Sarathi-Serve 论文里也把 prefill 描述成高延迟但容易饱和 GPU compute 的阶段，而 decode 更偏低利用率和逐 token 的内存访问。

可以按场景拆开看：

```text
长 prompt、模型已热、单机 GPU:
  主要看 GPU compute、attention kernel、GEMM、HBM 带宽、KV cache 写入和 token budget。

短 prompt、高 QPS:
  CPU tokenizer、chat template、JSON 解析、请求准入、调度器开销可能冒头。

prefix cache 命中多:
  真正要 prefill 的是 uncached prompt tokens，瓶颈会从模型计算转向 prefix 匹配、KV block 引用计数和调度。

KV cache 接近满:
  显存容量、block allocator、preemption/recompute、eviction/offload 会拉高尾延迟。

chunked prefill:
  单个 chunk 变小后，调度器、队列、锁和 kernel launch overhead 更容易被看见。

disaggregated prefill:
  prefill worker 算完以后要把 KV 交给 decode worker，网络、RDMA/NCCL、KV transfer buffer 和 ACK 语义会变成关键路径。

冷启动或模型未缓存:
  checkpoint 读取、权重搬运、CUDA graph warmup 才会让 I/O 和初始化成本占主导。
```

所以面试里不能说“prefill 的瓶颈就是 CPU”。CPU 的确可能成为瓶颈，但多发生在 prompt 预处理很重、tokenizer 单线程、短请求极高 QPS、Python 调度开销大、日志和 metrics 过密时。比如业务侧把很大的 tool schema、RAG 文档和多轮历史都在请求路径上拼接，再做复杂审计日志，GPU 还没开始算，CPU 已经把 TTFT 拉高了。这种情况下要看 tokenizer latency、input processing latency、scheduler enqueue/dequeue 时间和 CPU flame graph。

锁竞争也不是 prefill 的天然主瓶颈，但在实现里很常见。典型位置有全局请求队列、KV block free list、prefix cache 索引、refcount、租户配额、统计指标聚合。锁竞争的症状不是 GPU kernel 慢，而是 GPU 利用率忽高忽低、batch 形成不稳定、调度线程 CPU 高、p99 TTFT 比 p50 高很多。chunked prefill 如果 chunk 很小，还可能把一次长 prefill 变成大量调度事件，让这些锁更明显。

I/O 要分清是 checkpoint I/O 还是 prefill 本身。prefill 不应该每次都读模型权重。模型、tokenizer、CUDA runtime 已经热起来以后，一次请求的 prefill 主要是计算和 KV 状态构建。只有冷启动、模型换版本、LoRA/adapter 动态加载、外部 prefix/KV store、KV offload 到磁盘这类场景，I/O 才会进入主路径。

网络同理。普通单机推理里，网络只影响请求进入和流式返回，通常不是 prefill 计算瓶颈。多机 tensor parallel、pipeline parallel、disaggregated prefill 或跨节点 KV transfer 下，网络就很重要。尤其是 prefill/decode 分离时，decode 不是重新算 prompt，而是依赖 prefill worker 传来的 KV；这时 KV 的传输、确认、丢失重试和版本匹配都会影响正确性和延迟。

排障时我会先做分段观测，而不是猜：

```text
端到端:
  TTFT p50/p95/p99、queue time、prefill time、first token sampling time。

GPU:
  SM utilization、GEMM/attention kernel 时间、HBM bandwidth、kernel launch gap、CUDA graph 命中。

KV cache:
  allocated/free blocks、block allocation latency、preemption/recompute count、prefix cache hit/miss、uncached prompt tokens。

CPU:
  tokenizer latency、chat template latency、scheduler latency、lock wait、metrics/logging overhead。

分布式:
  NCCL/RDMA 时间、KV transfer bytes、transfer wait、producer/consumer backlog、跨节点错误和重试。
```

LogServe 当前的实验边界也要说清楚：项目里实现的是 LLM 请求生命周期、mock/vLLM adapter、模型 locality-aware scheduling 和 file-backed checkpoint cache，不是真实 GPU prefill profiler。可以用它解释调度和缓存边界，但不能把单机 mock latency 说成 prefill kernel 性能结论。

面试里可以这样答：

```text
prefill 的主瓶颈要按场景判断。warm 单机长 prompt 下，通常是 GPU compute、attention kernel、显存带宽和 KV cache 容量；短 prompt 高 QPS 下，CPU tokenizer、输入处理和调度开销会冒头；KV 紧张时是 block 分配、preemption 和 recompute；chunked prefill 会暴露调度和锁开销；disaggregated prefill 才会把网络和 KV transfer 放到关键路径。I/O 一般属于冷启动或外部 KV/offload 场景，不是每次 prefill 的默认瓶颈。
```

## Q082. prefill 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

这三类测试的目标不同。correctness test 问的是“算得对不对、状态有没有串”；stress test 问的是“高压、取消、OOM、重试时会不会坏”；benchmark 问的是“在可复现配置下，TTFT、吞吐和资源利用率到底是多少”。把三者混在一起，会得到很漂亮但没用的数字。

correctness test 先测语义等价。简化版 prefill 至少要保证同一模型、同一 tokenizer、同一 prompt、同一采样设置下，以下路径产出的 logits 或输出一致：

```text
一次性 prefill:
  prompt 全量 forward，然后 decode。

chunked prefill:
  prompt 分成多个 chunk，按顺序推进，然后 decode。

prefix cache hit:
  前缀复用 KV，只计算未命中后缀，然后 decode。

batch vs single:
  单请求执行和与其他请求合批执行，结果不能因为 batch 形态变化而串状态。
```

具体 correctness case 可以这样列：

```text
token 与 position:
  token ids、position ids、RoPE offset、attention mask、padding/packed sequence 都要对齐。

KV 写入:
  每一层、每个 KV head、每个 token 的 K/V 放在正确 block 和正确 offset。

chunk 边界:
  chunk A 完成后，chunk B 只能接在 A 后面，不能重复写、跳写、乱序写。

prefix cache:
  只有模型版本、tokenizer、chat template、LoRA/adapter、prefix token 序列和 KV dtype 一致时才能复用。

请求隔离:
  A 请求的 KV 不能被 B 请求读到；取消或失败后的 block 不能被错误引用。

batch invariance:
  同一个请求在不同 batch size、不同同批邻居、不同 chunk size 下应保持可解释的一致性。

边界长度:
  空 prompt、1 token prompt、max context、刚好跨 block、刚好跨 chunk 的长度都要覆盖。
```

如果做的是真实 GPU 实现，correctness 还要接受浮点误差。不要要求所有 dtype、所有 kernel、所有 batch shape 完全 bitwise 相等。更合理的是在 greedy decode 或固定 seed 下比较 token 序列，必要时比较 logits 的误差范围。对 FP8/INT8 KV cache、量化 attention 或不同 attention backend，更要把误差阈值写清楚。

stress test 关注压力和失败，而不是平均性能：

```text
长 prompt 压力:
  大量接近 max context 的请求并发进入，观察 OOM、preemption、recompute 和队头阻塞。

混合负载:
  短 chat、长 RAG、长输出、批量离线任务混在一起，观察短请求是否被长 prefill 拖死。

取消和超时:
  prefill 排队时取消、chunk 中间取消、kernel 完成后取消、first token 前超时。

KV 压力:
  KV blocks 快满时继续进请求，验证拒绝、排队、preemption、释放和统计是否正确。

prefix miss 风暴:
  大量看似相似但不共享 token 前缀的请求进来，验证 prefix cache 不会假命中。

锁和队列:
  高并发 enqueue、dequeue、block allocate/free、refcount 更新，观察死锁、饥饿和长尾。

分布式故障:
  prefill worker 失败、decode worker 失败、KV transfer 超时、producer/consumer 版本不一致。
```

benchmark 要可复现，指标也要按阶段拆。只报 total latency 没有意义。至少要报：

```text
用户侧:
  TTFT、TPOT/ITL、E2E latency、成功率、取消率、SLO violation。

吞吐:
  request/s、prompt tokens/s、output tokens/s、total tokens/s。

prefill:
  prefill latency、uncached prompt tokens/s、chunk size、chunks per request。

调度:
  queue time、batch size、max_num_batched_tokens、decode/prefill 混合比例。

KV:
  KV usage、allocated/free blocks、prefix cache hit rate、preemption count、recompute time。

资源:
  GPU utilization、HBM bandwidth、CPU utilization、tokenizer CPU time、网络/RDMA 吞吐。
```

benchmark 的工作负载要分开设计。一个短 prompt chat workload 可以反映交互式系统；一个长 prompt summarization/RAG workload 可以反映 prefill 压力；一个混合 workload 才能看出 chunked prefill 是否真的保护 decode。Hugging Face TGI v3 文档里也把 small scenario 和 long scenario 分开，就是因为短请求和长上下文请求完全不是同一种瓶颈。

面试里可以这样答：

```text
correctness test 测 prefill 状态是否正确：一次性 prefill、chunked prefill、prefix cache、batch/single 执行在同一输入下应等价，并覆盖 position、mask、RoPE、KV block、请求隔离和取消清理。stress test 测系统在长 prompt、高并发、KV 紧张、取消超时、prefix miss 风暴和分布式 KV transfer 失败下会不会 OOM、死锁、串状态或泄漏 block。benchmark 测可复现性能：TTFT、prefill tokens/s、TPOT、吞吐、KV 使用、preemption、GPU/CPU/网络资源，并区分短请求、长请求和混合负载。
```

## Q083. 如果要求从零实现一个简化版 prefill，你会先定义哪些不变量？

**回答：**

我会先定义状态机和不变量，再写 kernel 调用或调度策略。prefill 一旦写错，问题不会只表现为慢，可能直接表现为错答案、跨请求泄漏、流式输出卡死，甚至 GPU memory leak。

一个简化版 prefill 可以先限定范围：

```text
模型:
  单模型、单 tokenizer、单 GPU、greedy decode，先不做 LoRA、多模态和量化 KV。

请求:
  文本 prompt 输入，tokenizer 已固定，prompt token 序列不可变。

KV:
  使用固定大小 block pool，prefill 按 token 顺序写入 KV，decode 只能读取已提交 KV。

调度:
  先支持一次性 prefill，再支持 chunked prefill；先不做跨节点 KV transfer。
```

在这个范围内，我会先写这些不变量：

```text
请求身份不变量:
  request_id 唯一；一个 request 的 KV blocks 只能归属于这个 request 或被显式共享的 prefix。

模型版本不变量:
  KV 必须绑定 model_id、model_revision、tokenizer_revision、chat_template、dtype 和 adapter 配置。

prompt 不变量:
  prefill 开始后，prompt token ids 不可变；任何业务侧 prompt 改写都必须产生新请求。

位置不变量:
  第 i 个 prompt token 的 position、RoPE offset 和 attention mask 必须与模型定义一致。

顺序不变量:
  chunked prefill 只能按 prefix 顺序提交，不能先提交后缀，也不能重复提交同一段。

KV 完整性不变量:
  对每一层、每个 KV head、每个 token，K/V 要么未写入，要么完整写入；不能暴露半写状态给 decode。

可见性不变量:
  只有 prefill 状态从 RUNNING 变为 PREFILL_DONE 后，decode 才能读取这段 KV。

容量不变量:
  分配的 KV blocks 数量必须覆盖 prompt tokens；超过 max context 或剩余 block 不足时，要在计算前拒绝或排队。

隔离不变量:
  一个请求取消、失败或完成后，未共享 block 的引用计数必须归零并回到 free pool。

prefix 共享不变量:
  共享 prefix 的 block 只能读共享，写入新 token 时必须分配新 block 或 copy-on-write，不能改坏其他请求。

错误不变量:
  prefill 失败不能留下可被 decode 读取的状态；重试必须重新构造状态或从已验证的 prefix cache 开始。

观测不变量:
  每个请求必须能解释 queue、tokenize、prefill、first-token、decode、cancel、release 的状态转移。
```

状态机可以很小，但要清楚：

```text
NEW
  -> TOKENIZED
  -> WAITING_FOR_KV
  -> PREFILL_RUNNING
  -> PREFILL_DONE
  -> DECODING
  -> COMPLETED

任意中间态:
  -> CANCELLED / FAILED
```

关键是中间态不能乱跳。`PREFILL_RUNNING -> DECODING` 只能在所有 prompt tokens 的 KV 都提交后发生；`CANCELLED` 进入后不能再排入 decode；`FAILED` 后如果要重试，新的 attempt 必须拿新的 KV 所有权。这样即使后面增加 chunked prefill、prefix cache、preemption，也有基本语义可守。

如果要支持 chunked prefill，我会额外定义 chunk 不变量：

```text
chunk range:
  每个 chunk 是 [start, end) token 区间，区间连续、不重叠、不越界。

commit point:
  request 记录 committed_prompt_tokens，只有 commit point 之前的 KV 可被后续 chunk 使用。

cancellation boundary:
  取消只在 chunk 或 iteration 边界生效，已分配但未提交的 block 必须释放。

budget:
  调度器按 uncached prompt tokens 和 max_num_batched_tokens 形成 batch，不按请求数猜成本。
```

面试里我会强调，简化版也不能省掉模型版本和 tokenizer 版本。很多 prefix/prefill bug 不是 attention 算错，而是“同一段中文文本在不同 chat template 下 token 不一样”“同一个模型名指向了不同 revision”“LoRA adapter 不同却复用了 KV”。这些错误不会 crash，但会让输出变得莫名其妙，排障很难。

LogServe 的 shared log 思路可以借来定义控制面事件，比如 `LLMPrefillStarted`、`LLMPrefillCompleted`、`LLMFirstTokenSent`、`LLMCompleted`。但真实 KV tensor 不适合直接写进 shared log，日志应该记录状态和引用，不要把 GPU 运行时状态伪装成可恢复 checkpoint。

面试里可以这样答：

```text
我会先定义不变量，而不是先写调度。核心是不允许 prompt、模型版本、tokenizer、position、attention mask 和 KV ownership 变得含糊。prefill 开始后 prompt tokens 不可变；KV 绑定模型和 tokenizer 版本；chunk 必须连续按序提交；半写 KV 不能对 decode 可见；取消或失败必须释放未提交 blocks；prefix 共享必须有 refcount 或 copy-on-write；decode 只能在 PREFILL_DONE 后开始。这样简化实现后面加 chunked prefill、prefix cache 或重试时才不会串状态。
```

## Q084. prefill 的常见误用是什么，误用后通常会产生什么线上症状？

**回答：**

prefill 的误用大多来自边界没分清：把模型缓存当成 prompt 缓存，把 prefix cache 当成万能缓存，把 chunked prefill 当成只会变快的开关，把部分 KV 当成可恢复状态。线上症状也不只是慢，有些会直接影响正确性和隔离。

常见误用可以分几类。

第一类是把 model cache hit 当成 prefill hit。模型权重已经在 GPU 上，只说明不用加载 checkpoint，不说明 prompt 已经处理过。一个很长的 RAG prompt 仍然要做大量 prefill。误用后的症状是：监控里 cache hit 很高，但 TTFT 仍然很高；团队以为缓存没生效，实际是缓存层级看错了。

第二类是只按请求数做 batching，不按 token 数做调度。prefill 的成本主要跟 uncached prompt tokens 有关，十个 100 token 请求和十个 100k token 请求不是一个量级。误用后的症状是：短请求被长 prompt 拖住，TTFT p99 突然升高；流式输出 TPOT 抖动；GPU 看起来很忙，但用户觉得系统“卡一下再吐字”。

第三类是滥用 chunked prefill。chunked prefill 的目标是减少长 prefill 对 decode 的阻塞，不是保证所有请求更快。chunk 太小会增加调度、锁、kernel launch 和 KV bookkeeping 开销；chunk 太大又不能保护 decode。误用后的症状是：平均 TTFT 没降，CPU/scheduler 变忙，GPU iteration gap 变多，短请求偶尔更慢。

第四类是错误复用 prefix/KV。只要模型版本、tokenizer、chat template、LoRA adapter、special tokens、position/RoPE 语义不同，就不能复用同一段 KV。多租户环境还要考虑权限和数据隔离。误用后的症状最危险：输出跑题、同样 prompt 偶发不同结果、A 用户内容影响 B 用户、审计上出现数据泄漏风险。它可能不会报错。

第五类是把部分 prefill 状态当成 checkpoint。prefill 中间完成了一些 chunk，不代表系统可以随便从下一个 chunk 恢复。除非实现了外部 KV store、KV transfer 和完整校验，否则 worker 崩溃后应该重新 prefill。误用后的症状是：重启后偶发错误输出，decode 读到缺失 KV，或者系统为了“恢复”而陷入复杂补偿逻辑。

第六类是忽略取消和释放。客户端取消后，如果 prefill 已经排队或正在跑，系统要在安全边界停止，并释放已分配 KV blocks。误用后的症状是：取消率高时显存不断上涨，KV free blocks 下降，后续请求被 preempt 或 OOM；从业务看，用户取消了请求，服务端仍然在算。

第七类是把 TTFT 全部归因于 prefill。TTFT 包含排队、tokenization、prompt 构造、prefill、first-token sampling、网络 flush。误用后的症状是：团队拼命调 chunk size 或 max_num_batched_tokens，但真正瓶颈在 tokenizer、网关、RAG 检索、JSON schema 或日志。

第八类是分布式场景下忘记 locality。多个 replica 后面挂一个负载均衡器，如果请求随机打到不同 replica，prefix cache 或 KV locality 可能完全用不上。Hugging Face TGI 文档也提到，多个 replica 后面没有 sticky routing 时，某个用户的 cache 不一定在当前 replica 上。误用后的症状是：单机压测效果很好，上线后 cache hit 大幅下降，长 prompt TTFT 回退。

可以把症状按监控信号归纳：

```text
TTFT p99 高:
  长 prompt 队头阻塞、prefix miss、token budget 太大、CPU 预处理慢。

TPOT 抖动:
  decode 被大 prefill 挤占，chunk 太大，generation phase 没有被保护。

OOM / preemption 多:
  KV 容量估计错，取消不释放，max_model_len 或 max_num_batched_tokens 太激进。

GPU 忙但吞吐低:
  batch 形态差、pipeline bubbles、scheduler gap、CPU/锁拖住提交。

cache hit 高但不快:
  命中的是模型权重或不相关缓存，不是 prefix/KV；或者只省了少量前缀。

输出偶发错误:
  KV 复用条件不严、position/RoPE 错、chunk 边界错、跨租户状态串。
```

面试里可以这样答：

```text
prefill 常见误用包括：把模型 cache hit 当成 prefill hit，只按请求数而不是 token 数 batching，盲目打开 chunked prefill，跨模型版本或 chat template 复用 KV，把半完成 chunk 当成可恢复 checkpoint，取消后不释放 KV，以及把 TTFT 全算到 prefill 头上。线上症状通常是 TTFT p99 飙升、TPOT 抖动、KV OOM/preemption、cache hit 看似很高但不快、GPU 忙却吞吐低，严重时还会出现错答案或跨请求数据泄漏。
```

## Q085. prefill 在单机和分布式环境中的语义有什么差异？

**回答：**

单机和分布式最大的差异是：单机 prefill 生成的 KV 状态通常就在本进程、本 GPU 或同一台机器的 GPU 组里；分布式下，prefill 的产物可能被切片、转移、复制、丢失或只存在于某个 replica。语义从“本地运行时状态”变成了“带所有权、位置、版本和传输确认的分布式状态”。

单机语义相对直接：

```text
状态位置:
  KV cache 在本进程管理的 GPU memory 或本机 GPU 组里。

调度:
  scheduler、KV allocator、model runner 通常在同一服务实例内协作。

失败:
  进程或 GPU reset 后，prefill KV 基本丢失，请求要重新 prefill。

一致性:
  只要模型、tokenizer、position 和 block mapping 正确，请求隔离主要由本地内存管理保证。

性能:
  主要看 GPU compute、HBM、KV capacity、CPU tokenizer 和本地调度。
```

多 GPU 单机已经比单 GPU 复杂。tensor parallel 下，每个 rank 只持有一部分权重和可能的一部分 KV 视图；pipeline parallel 下，不同层在不同 GPU 上，prefill 的每个 micro-batch 要跨 stage 推进。此时“prefill 完成”不是一个线程返回就行，而是所有相关 rank/stage 都完成了对应层和 token 的状态。任何一个 rank 失败，通常都不能假装其他 rank 的 KV 还能单独用于 decode。

分布式 replica 场景的语义又不一样。假设有多个独立 vLLM/TGI/TensorRT-LLM 实例挂在负载均衡器后面，A replica 对某个 prompt 做过 prefill，不代表 B replica 有这段 KV。prefix cache、KV cache、paged blocks 都有 locality。除非有共享 KV store、显式 KV transfer 或 sticky routing，否则下一次请求打到另一个 replica，就应视为没有可复用 KV。

prefill/decode disaggregation 会把语义推到更复杂的一层：

```text
prefill worker:
  负责处理 prompt，产生 prompt KV 和 first-token 所需状态。

decode worker:
  负责接收或加载 KV，然后继续逐 token decode。

连接层:
  需要传输 KV tensor，确认哪一批 request、哪一段 token、哪一层 KV 已经可用。

所有权:
  KV producer 什么时候可以释放？consumer 什么时候可以读取？失败后谁负责重算？

版本:
  prefill 和 decode 两边必须使用兼容的模型、tokenizer、KV dtype、parallel layout 和 RoPE 配置。
```

vLLM 的 disaggregated prefilling 文档把 connector、lookup buffer、pipe 这类抽象放在核心位置，原因就在这里：分布式 prefill 不只是“把两个进程连起来”，而是要让 KV consumer 能找到并取走 KV，scheduler 能安排 transfer，worker connector 能执行 tensor 传输。这里的正确性问题和单机完全不同。

分布式下还要处理失败语义。prefill worker 算完但 KV transfer 失败，decode worker 不能凭空继续；decode worker 已经拿到一半 KV，也不能默认进入 decode；consumer ACK 前 producer 是否保留 KV，要有明确策略。网络抖动、RDMA/NCCL 错误、worker 重启、版本滚动发布，都会影响 prefill 状态是否可用。

调度目标也会变化。单机调度主要在一个 GPU 或一组 GPU 内平衡 prefill 和 decode；分布式调度还要考虑：

```text
locality:
  哪个 replica 已有 prefix/KV，哪个节点模型已热。

负载:
  prefill worker 和 decode worker 的队列长度、GPU 水位、KV buffer 水位。

传输成本:
  KV bytes 很大，跨节点搬移可能比重新 prefill 更贵。

发布与兼容:
  滚动升级时，新旧模型 revision、tokenizer 或 KV layout 不能混用。

租户隔离:
  不同租户的 KV 不能因为共享 connector 或 buffer 被误取。
```

LogServe 面试里要把项目边界讲准。当前项目可以说明“为什么调度器要知道模型 locality、为什么 shared log 要记录 LLM 请求状态、为什么 checkpoint cache 和 KV cache 是两类东西”。但它没有实现真实 GPU KV block manager，也没有实现分布式 prefill/decode disaggregation。更准确的说法是：LogServe 验证了 LLM 请求控制面和缓存感知调度的机制，不声称解决生产级分布式 prefill。

面试里可以这样答：

```text
单机 prefill 的 KV 通常是本进程、本 GPU 的运行时状态，prefill 完成后本地 decode 直接读；崩溃后一般重新 prefill。分布式下，KV 可能按 tensor/pipeline parallel 切片，也可能只存在于某个 replica，或者由 prefill worker 传给 decode worker。因此要定义 KV 的位置、所有权、版本、传输 ACK、失败重算、sticky routing 和租户隔离。一个节点 prefill 过，不代表另一个节点可复用；没有显式 KV transfer 或共享 KV store，就不能把分布式 prefill 当成本地状态来用。
```

## 参考资料

- [vLLM Optimization and Tuning](https://docs.vllm.ai/en/latest/configuration/optimization/)
- [vLLM Disaggregated Prefilling](https://docs.vllm.ai/en/latest/features/disagg_prefill/)
- [vLLM Automatic Prefix Caching](https://docs.vllm.ai/en/latest/features/automatic_prefix_caching/)
- [OpenAI Prompt Caching](https://developers.openai.com/api/docs/guides/prompt-caching)
- [vLLM Speculative Decoding](https://docs.vllm.ai/en/latest/features/speculative_decoding/)
- [vLLM N-Gram Speculation](https://docs.vllm.ai/en/latest/features/speculative_decoding/n_gram/)
- [TensorRT-LLM Speculative Decoding](https://nvidia.github.io/TensorRT-LLM/features/speculative-decoding.html)
- [vLLM Quantization](https://docs.vllm.ai/en/latest/features/quantization/)
- [vLLM Quantized KV Cache](https://docs.vllm.ai/en/latest/features/quantization/quantized_kvcache/)
- [OWASP Top 10 for Large Language Model Applications](https://owasp.org/www-project-top-10-for-large-language-model-applications/)
- [SkyLB: A Locality-Aware Cross-Region Load Balancer for LLM Inference](https://arxiv.org/abs/2505.24095)
- [vLLM Paged Attention design document](https://docs.vllm.ai/en/latest/design/paged_attention/)
- [vLLM KV Cache Manager API](https://docs.vllm.ai/en/latest/api/vllm/v1/core/kv_cache_manager/)
- [PagedAttention paper](https://arxiv.org/abs/2309.06180)
- [vAttention paper](https://arxiv.org/abs/2405.04437)
- [Hugging Face Transformers KV cache documentation](https://huggingface.co/docs/transformers/en/kv_cache)
- [TensorRT-LLM Paged Attention, IFB, and Request Scheduling](https://nvidia.github.io/TensorRT-LLM/features/paged-attention-ifb-scheduler.html)
- [TensorRT-LLM Checkpoint Loading](https://nvidia.github.io/TensorRT-LLM/features/checkpoint-loading.html)
- [Hugging Face TGI v3 overview: caching and chunking](https://huggingface.co/docs/text-generation-inference/en/conceptual/chunking)
- [Hugging Face TGI Streaming](https://huggingface.co/docs/text-generation-inference/en/conceptual/streaming)
- [Hugging Face TGI Safetensors](https://huggingface.co/docs/text-generation-inference/en/conceptual/safetensors)
- [vLLM Production Metrics](https://docs.vllm.ai/en/latest/usage/metrics/)
- [TensorRT-LLM Parallelism](https://nvidia.github.io/TensorRT-LLM/features/parallel-strategy.html)
- [NVIDIA Triton Inference Server Batchers](https://docs.nvidia.com/deeplearning/triton-inference-server/user-guide/docs/user_guide/batcher.html)
- [NVIDIA Triton Inference Server Model Configuration](https://docs.nvidia.com/deeplearning/triton-inference-server/user-guide/docs/user_guide/model_configuration.html)
- [NVIDIA Triton Inference Server Rate Limiter](https://docs.nvidia.com/deeplearning/triton-inference-server/user-guide/docs/user_guide/rate_limiter.html)
- [NVIDIA Triton Inference Server Model Management](https://docs.nvidia.com/deeplearning/triton-inference-server/user-guide/docs/user_guide/model_management.html)
- [vLLM Loading model weights with fastsafetensors](https://docs.vllm.ai/en/latest/models/extensions/fastsafetensor/)
- [vLLM Loading models with Run:ai Model Streamer](https://docs.vllm.ai/en/latest/models/extensions/runai_model_streamer/)
- [Hugging Face Hub cache management](https://huggingface.co/docs/huggingface_hub/en/guides/manage-cache)
- [Hugging Face Safetensors](https://huggingface.co/docs/safetensors/index)
- [ServerlessLLM paper](https://arxiv.org/abs/2401.14351)
- [Hugging Face TGI Tensor Parallelism](https://huggingface.co/docs/text-generation-inference/en/conceptual/tensor_parallelism)
- [Hugging Face TGI Launcher Arguments](https://huggingface.co/docs/text-generation-inference/en/reference/launcher)
- [NVIDIA DCGM Exporter](https://docs.nvidia.com/datacenter/cloud-native/gpu-telemetry/latest/dcgm-exporter.html)
- [vLLM Benchmark CLI](https://docs.vllm.ai/en/latest/benchmarking/cli/)
- [NVIDIA GenAI-Perf](https://docs.nvidia.com/deeplearning/triton-inference-server/user-guide/docs/perf_analyzer/genai-perf/README.html)
- [vLLM OpenAI-Compatible Server](https://docs.vllm.ai/en/latest/serving/openai_compatible_server/)
- [vLLM LoRA Adapters](https://docs.vllm.ai/en/latest/features/lora/)
- [Hugging Face Transformers Tokenizer](https://huggingface.co/docs/transformers/en/main_classes/tokenizer)
- [Hugging Face Tokenizers](https://huggingface.co/docs/tokenizers/en/index)
- [OpenAI Chat Completions API Reference](https://developers.openai.com/api/reference/resources/chat)
- [OpenAI Streaming Responses Guide](https://developers.openai.com/api/docs/guides/streaming-responses)
- [OpenAI Responses Cancel API](https://developers.openai.com/api/reference/resources/responses/methods/cancel)
- [OpenAI Cookbook: counting tokens with tiktoken](https://developers.openai.com/cookbook/examples/how_to_count_tokens_with_tiktoken)
- [SARATHI paper](https://arxiv.org/abs/2308.16369)
- [Sarathi-Serve paper](https://arxiv.org/abs/2403.02310)
- [FastServe paper](https://arxiv.org/abs/2305.05920)
- [Locality-aware Fair Scheduling in LLM Serving](https://arxiv.org/abs/2501.14312)
