# 47. p99、coordinated omission、flame graph 与 Jepsen 追问链

这一批放四个主题：p99、coordinated omission、flame graph 和 Jepsen。它们看起来分属观测、压测、profiling 和分布式正确性测试，但面试时经常连在一起问：你看到的指标是真的吗？慢请求为什么被平均值盖住？压测报告为什么比真实用户体验好看？火焰图上的宽块能不能直接等于事故根因？Jepsen 发现了异常，能不能说明系统一定不正确，或者没发现异常就一定正确？

LogServe 的口径还是要稳。项目可以借这些概念解释自己的验证边界：workflow p99、worker 执行耗时、shared log append/fsync、metadata replay、任务重投递、故障注入和一致性检查。但它不是完整的观测平台、profiling 产品或 Jepsen 测试套件。面试里说清楚边界，比把词堆满更有说服力。

## Q001. 面试官如果只问一个问题检验你是否理解 p99，可能会问什么？

**回答：**

我会预期他问一个很具体的排障题：

```text
一个接口平均延迟 80ms，p50 40ms，但 p99 从 300ms 升到 4s。错误率没有明显上升，CPU 平均利用率也不高。你会先怀疑什么？怎么证明是某个 route、tenant、下游、锁、连接池、GC、队列等待，还是指标本身算错了？
```

这比问“p99 是什么”更能看出理解。p99 的核心不是数学定义，而是尾部用户体验和系统饱和的早期信号。平均值好看，p99 很差，通常说明大多数请求没问题，但有一小部分请求卡在某个共享资源、慢下游或排队点上。

我会先解释 p99 的含义：在给定时间窗口和筛选条件下，99% 的样本小于等于这个值，剩下 1% 更慢。它不是最慢请求，也不是单个用户一定会每 100 次碰到 1 次。它是一个分布位置，必须和样本量、窗口、维度一起解释。每分钟只有 50 个请求时，p99 会非常不稳定；每分钟 100 万个请求时，1% 就是 1 万个请求，p99 已经是很大的影响面。

然后我会说排查路径。先拆维度：route、method、status、region、version、tenant tier、dependency、worker、queue、partition。p99 升高如果集中在一个 route，可能是业务逻辑、SQL、缓存或下游；如果集中在一个 region，可能是网络或区域资源；如果集中在少数 tenant，可能是大客户数据量或热 key；如果所有 route 一起变慢，才更像全局资源、GC、连接池、日志后端或部署变更。

第三步看端到端和分段。入口 p99 高，不代表 handler 代码慢。请求可能在客户端排队、负载均衡器排队、连接池等待、队列等待、线程池等待、数据库锁等待、下游重试、日志同步写入里耗掉时间。指标上要把 request duration、queue wait、handler duration、downstream call duration、retry backoff、connection acquisition、lock wait 分开。trace 可以帮忙找慢请求的路径，但 p99 本身最好来自 metrics histogram，而不是只靠采样 trace。

第四步验证指标本身。不能把各实例 p99 平均后当全局 p99。Prometheus 文档里对 histogram 和 summary 的区别说得很直接：summary 暴露的是客户端预计算的 quantile，通常不能跨实例聚合；histogram 把样本放进 bucket，服务端可以聚合 bucket 后估算整体 quantile。bucket 太粗，p99 也会被估错。面试里说“我们看 p99”不够，还要说明 p99 是怎么采集、怎么聚合、bucket 怎么设的。

结合 LogServe，我会把 p99 落到几个具体点：workflow_duration、task_queue_wait、worker_execute_duration、shared_log_append_duration、metadata_replay_duration、LLM_call_duration。比如 workflow p99 升高，可能不是 DAG 调度慢，而是某类 task 等 worker、shared log fsync 抖动、对象存储 result reference 慢，或者结构化日志阻塞了热路径。只看平均 workflow 耗时会把这些尾部问题盖掉。

面试里可以这样答：p99 是尾部延迟位置，不是平均性能的装饰。真正理解 p99，要能说明样本量、窗口、维度、聚合方式和 bucket 误差，并能沿着端到端路径拆出排队、锁、GC、连接池、下游和重试。p99 升高时，不要先猜代码慢，先证明慢在哪里。

## Q002. p99 的一句话定义是否容易误导，误导点在哪里？

**回答：**

容易误导。常见定义是：p99 表示 99% 的请求延迟低于某个值。这句话没错，但它太干净，干净到容易让人忘掉统计条件。

第一个误导是忘记时间窗口。过去 1 分钟的 p99、过去 5 分钟的 p99、过去 30 天的 p99，不是同一个问题。短窗口适合值班排障，但抖动大；长窗口适合 SLO，但会把短时间事故摊平。接口低流量时，短窗口 p99 可能根本没有统计意义。这个时候看“好事件比例”或固定阈值下的达标率，往往比追一个 p99 点更稳。

第二个误导是忘记筛选条件。所有 route 混在一起的 p99，可能掩盖某个核心接口已经崩了；所有租户混在一起，可能掩盖高级租户的慢查询；成功和失败混在一起，可能让快速失败的 500 把延迟拉低。p99 一定要带维度解释：哪个服务、哪个接口、哪个状态码、哪个区域、哪个版本、哪个用户群。

第三个误导是以为 p99 可以平均。每台机器各自算出一个 p99，再取平均，不是整体 p99。分位数不是可加的。请求量少的实例和请求量多的实例权重不同，局部分布也可能完全不同。跨实例要么聚合原始样本，要么聚合 histogram bucket 后用 `histogram_quantile()` 这类方法估算。

第四个误导是忽略 bucket 误差。histogram 的 p99 是估算值，误差受 bucket 边界影响。如果 SLO 是 800ms，bucket 却只有 500ms 和 2s，p99 落在中间时就很粗。指标看起来有三位小数，实际信息量可能只有“在 500ms 到 2s 之间”。bucket 要围绕 SLO、超时和排障阈值设计。

第五个误导是把 p99 当成最坏情况。p99 之外还有 1% 更慢。对高 QPS 服务，这 1% 可能是大量用户。对支付、交易、实时推荐、调度系统，p999、超时数、最大值、慢请求样本也有价值。p99 不是终点，它只是比平均值更靠近尾部的一把尺子。

第六个误导是忘记用户路径会 fan-out。一个页面调用 20 个下游，每个下游单看 p99 都还可以，入口请求碰到至少一个慢分支的概率会明显上升。用户体验的 p99 不是每个服务 p99 的简单组合。复杂链路里要看端到端 p99，也要看每个依赖的贡献。

更准确的一句话是：p99 是在明确窗口、样本集合和聚合方法下，分布中 99% 样本不超过的延迟估计值；它适合观察尾部体验和饱和信号，但必须和样本量、维度、bucket、错误率和超时一起解释。

## Q003. p99 最常见的生产事故触发条件是什么？

**回答：**

p99 事故最常见的触发条件是共享资源进入排队区间。系统不一定整体满载，平均 CPU 也可能很好看，但某个小资源已经被打满：连接池、线程池、锁、队列、下游限流、磁盘 fsync、日志 sink、热点分片。尾部先变差，平均值后知后觉。

第一类是连接池耗尽。数据库、Redis、HTTP client、对象存储 SDK 都可能有池。平时每次拿连接 1ms，事故时排队 2s。服务端自报处理很快，客户端 p99 却很差。排查时要看 connection_acquire_duration、in_flight、waiter count、pool timeout，而不是只看下游 server latency。

第二类是锁和热点 key。某个全局 map、logger、metrics registry、tenant 配额行、任务调度游标、数据库热点行，被大量请求争用。p50 还行，因为很多请求没碰到热点；p99 很差，因为碰到热点的请求排队。Go 服务里还要看 mutex profile、block profile、goroutine dump；数据库里看 lock wait、row contention、deadlock 和慢事务。

第三类是 GC、STW 或运行时暂停。大多数请求正常，暂停发生时一批请求一起变慢。p99 会突然抬高，p50 可能没怎么动。要把 runtime pause、heap、allocation rate、goroutine、thread、CPU throttling 和请求延迟对齐看。容器 CPU quota 也会制造类似长尾。

第四类是下游长尾和重试放大。下游偶发慢，客户端重试，重试又占连接和线程，最后把入口 p99 拉高。特别是 fan-out 场景，一个入口请求依赖多个下游，任何一个分支慢都可能拖住整体。指标要区分 single attempt latency 和 logical call latency，后者包含重试和 backoff。

第五类是队列和 worker 饱和。队列长度平均看着还行，但某类任务或某个 partition 积压。用户感知的 workflow p99 包括 queue wait，不只是 worker 执行时间。LogServe 这类系统尤其要看 task_queue_wait、lease 重投递、worker pool backlog、按 task type 拆的 oldest age。

第六类是观测系统反过来拖慢业务。同步写日志、trace exporter 阻塞、metrics label 高基数爆炸、profile 开销过大，都可能只在高并发时拉高 p99。事故期间错误日志增加，日志后端变慢，业务等待日志写入，这种反馈很常见。

第七类是发布或配置变更。新版本多了一个远程调用，bucket 改坏了，超时变长了，连接池变小了，日志级别调成 debug，某个 feature flag 只影响 5% 流量。平均值可能看不出来，p99 会先叫。

我会总结成一句：p99 事故多半不是“所有请求都慢”，而是少数路径被排队、争用、暂停、重试或下游长尾击中。排查要从维度拆分和端到端分段开始，别盯着全局平均 CPU 发呆。

## Q004. p99 的指标应该怎么设计才不会只看平均值？

**回答：**

p99 的指标设计要从用户路径和分布开始，不是给每个接口打一个 `avg_latency_ms` 就完事。平均值可以保留，用来看容量和总体成本，但它不能代表用户体验。

第一组是端到端延迟。入口请求、workflow、任务执行、RPC 调用，都要有 histogram。指标名要带单位，比如 `_seconds`。维度要控制住：service、route、method、status、region、version、tenant_tier 可以有；user_id、workflow_id、trace_id 这种高基数不能进 metrics label。

第二组是分段延迟。端到端 p99 只是症状。要同时记录 queue_wait、connection_acquire、handler、db_query、cache_get、rpc_attempt、rpc_call、retry_backoff、fsync、log_append、object_store_put。这样 p99 升高时可以看到慢在排队、执行、下游还是提交。

第三组是 histogram bucket。bucket 要围绕 SLO 和超时边界设计。比如接口目标 300ms、超时 2s，就应该有 50ms、100ms、200ms、300ms、500ms、1s、2s、5s 这类边界。没有接近 SLO 的 bucket，就很难回答“99% 是否低于目标”。如果使用 native histogram，也要理解 resolution 和查询成本。

第四组是流量和样本量。每条 p99 曲线旁边都应该有 request count 或 rate。没有样本量的 p99 是危险图表。低流量接口更适合看长窗口、慢请求列表、超时数、最大值和达标率。高流量接口可以看短窗口 p99/p999。

第五组是错误和超时。成功请求 p99、失败请求 p99、timeout count、cancellation count 要拆开。快速失败会拉低平均延迟，也可能拉低混合 p99，但用户看到的是错误。SLO 通常应该同时看 latency 和 availability，不能只看成功请求的延迟。

第六组是聚合规则。跨实例用 histogram bucket 聚合后再算 quantile，或者用原始样本离线算。不要平均 p99。跨 route、跨 region、跨 tenant 的聚合也要谨慎：大盘用于概览，排障必须能 drill down 到具体维度。

第七组是相关资源指标。p99 面板旁边要有 saturation：CPU throttling、GC pause、heap、goroutine/thread、connection pool wait、lock wait、queue depth、oldest age、downstream p99、retry count、日志队列深度。没有原因指标，p99 只是报警灯。

第八组是 exemplars 和 trace。metrics histogram 用来发现 p99，trace 用来解释慢请求。可以在慢 bucket 上挂 trace exemplar，或者用 tail sampling 保留慢 trace。不要反过来只用采样 trace 计算 p99，采样策略会骗你。

面试里可以这样答：p99 指标要包括端到端 histogram、分段 histogram、样本量、错误率、超时、bucket 设计、维度拆分和资源饱和度。平均值只能做配角。一个可用的 p99 dashboard 应该能回答三件事：谁慢、慢在哪、影响多少请求。

## Q005. p99 的正确性边界和性能边界分别是什么？

**回答：**

p99 的正确性边界是统计解释。它描述的是某个样本集合的分布位置，不描述业务是否正确，也不证明系统满足强一致、幂等或可靠性。p99 很好看，仍然可能有数据丢失；p99 很差，也不一定是业务逻辑错了，可能只是排队或观测开销。

第一条边界是样本集合。p99 只对被记录的样本成立。没有记录超时、取消、队列等待、客户端等待，p99 就会偏乐观。只统计成功请求，也会让事故看起来很轻。压测里如果有 coordinated omission，系统停顿期间本该到达的请求没被记录，p99 会被严重低估。

第二条边界是时间和聚合。窗口太短会抖，窗口太长会掩盖事故；summary quantile 不能随便跨实例聚合；histogram 受 bucket 误差影响。p99 的数值不是客观真理，它来自采集方式和查询方式。面试里要敢说“这个 p99 是估算值”。

第三条边界是用户体验。p99 接近用户体验，但不等于完整体验。用户可能一次操作触发多个请求，可能受前端渲染、网络、重试、缓存、排队影响。后端接口 p99 低，不代表页面 p99 低。真正的体验指标应该包含端到端路径和关键业务动作。

性能边界则是尾延迟优化的成本。把 p99 从 2s 降到 500ms，可能只需要修一个热点锁；把 p99 从 80ms 降到 20ms，可能要重写架构、增加缓存、减少 fan-out、拆分热点、优化 GC、改协议。越往尾部和越低延迟目标走，成本越高。

第二个性能边界是观测开销。为了看 p99，需要 histogram、trace、日志和 profile，但这些东西也会消耗 CPU、内存、网络和存储。高基数 label、过细 bucket、同步 exporter、全量 trace，都可能把业务 p99 拉高。观测系统必须 fail open，慢了可以降采样或丢观测，不能拖死业务。

第三个边界是优化目标冲突。降低 p99 可能会牺牲吞吐、成本或平均延迟。比如限制并发可以降低排队尾部，但可能降低峰值吞吐；加副本可以降低等待，但增加成本；hedged requests 可以降低尾延迟，但放大下游负载。面试里不要把 p99 优化说成单向收益。

所以我会这样说：p99 的 correctness 是“在明确样本、窗口和算法下的分位估计”，不是业务正确性证明；performance 是“用分布视角发现尾部问题”，但优化尾部要付出容量、复杂度和观测成本。p99 很有用，前提是你知道它没看到什么。
## Q006. 面试官如果只问一个问题检验你是否理解 coordinated omission，可能会问什么？

**回答：**

我会预期他问一个压测结果很好看、线上却很慢的题：

```text
压测工具用 100 个线程循环发请求：每个线程发一个请求，等响应回来，再发下一个。报告显示 p99 只有 200ms。但线上用户说偶尔会卡 10 秒。后来发现服务每分钟会停顿 10 秒。为什么压测没把这 10 秒体现到 p99 里？你会怎么改压测和指标？
```

这就是 coordinated omission 的典型场景。系统慢的时候，压测客户端也被迫慢下来。它等待响应期间没有继续按原本到达率发请求，于是少记了一批本该排队的请求。最后报告里可能只有一个 10 秒样本，而不是 10 秒停顿期间所有计划到达请求的等待时间。

我会先把两个时间说清楚：发送计划时间和完成时间。用户流量通常不是“上一个请求完成后才允许下一个用户来”。如果系统在 10 秒里无法处理请求，这 10 秒内本该到达的请求会排队、超时或失败。压测工具如果按完成节奏发请求，服务一停，它也停；服务恢复，它再继续。这样测到的是“闭环客户端在被系统反压后的体验”，不是“固定到达率下用户会经历的延迟”。

第二步说为什么 p99 会偏低。假设目标到达率是每秒 1000 个请求，服务暂停 10 秒，真实世界里有大约 1 万个请求会受到影响。闭环压测可能只记录 100 个线程手上的 100 个慢请求，剩下 9900 个没有生成，也就没有进入 histogram。p99 看起来好很多，不是系统真的好，是样本缺了。

第三步说修正方法。压测应该按固定到达率或独立到达过程发请求，记录从“本该发送或到达的时间”到完成的端到端延迟。HdrHistogram 这类工具提供 coordinated omission 修正思路：当记录值大于期望采样间隔时，可以补入一系列递减的虚拟样本，或者在记录时用 expected interval 修正。当然，最好的方式是压测工具本身能维持独立到达率，并把排队、超时、失败都记录下来。

第四步说不是所有场景都一样。如果你要模拟的是固定数量 worker 的闭环系统，比如 100 个后台 worker 每个完成后才拿下一个任务，闭环模型本身有意义。但如果你要模拟用户到达、消息入队、外部请求流量，闭环压测会低估拥塞时的体验。面试里要先问“我们模拟的到达模型是什么”，而不是直接说某个工具对或错。

结合 LogServe，coordinated omission 可能出现在 worker 压测里：压测脚本提交一个 workflow，等它完成再提交下一个，于是 shared log fsync 卡住、worker pool 停顿、LLM 调用排队时，脚本也停了。报告会说 p99 还可以，但真实系统里用户会继续提交任务，队列等待会暴涨。更好的测试要按固定提交率产生 workflow，并把 submit_time、scheduled_time、started_time、completed_time 都记录下来。

面试里可以这样答：coordinated omission 是测量端被被测系统拖慢后漏记了本该到达的请求，导致尾延迟被低估。解决它要明确到达模型，按计划到达时间计算延迟，记录排队和超时，并使用支持修正的 histogram 或压测工具。闭环压测不是没用，但不能拿它代表开放用户流量。

## Q007. coordinated omission 的一句话定义是否容易误导，误导点在哪里？

**回答：**

容易误导。常见定义是：coordinated omission 是压测工具漏记慢请求。这个说法太粗，容易让人以为只是工具 bug。

第一个误导是把它当成“少记录了几个异常点”。真实问题更严重。系统停顿期间，所有本该到达的请求都受到影响。如果到达率很高，缺失的不是几个样本，而是一整段时间的用户体验。尾部指标会被系统性压低。

第二个误导是只看服务端处理时间。服务端可能只统计从请求进入 handler 到返回的 duration。请求在客户端、负载均衡、连接池、线程池、队列里等了很久，handler 看到的仍然很快。coordinated omission 经常和“只测处理时间，不测等待时间”一起出现。

第三个误导是以为固定并发等于固定吞吐。100 个线程循环请求，吞吐会随着系统变慢而下降。系统停顿时，客户端没有继续制造压力。固定并发适合测饱和后的闭环吞吐，但不适合模拟外部用户按时间到达。固定到达率和固定并发是两种模型。

第四个误导是以为补样本就能修复一切。HdrHistogram 的修正方法能补偿一类规律采样遗漏，但它依赖 expected interval，也不能替你记录业务失败、超时、丢弃、排队位置。压测工具真正应该做的是从到达计划开始记录请求生命周期。

第五个误导是把它只放在压测里。生产监控也会有类似问题。比如只在服务端收到请求后打点，入口队列爆了但请求还没进入服务，指标就看不到；只统计成功完成的任务，超时取消的不进 histogram；客户端超时后断开，服务端后面完成了一个“成功请求”，用户体验却是失败。

第六个误导是忘记错误率和完成率。系统变慢时，如果大量请求根本没发出、没接收、没完成，只看已完成请求 p99 会很乐观。报告必须同时给出计划发送数、实际发送数、完成数、超时数、错误数、丢弃数和有效到达率。

更准确的一句话是：coordinated omission 是测量过程的采样节奏被被测系统的响应节奏影响，导致拥塞、暂停或排队期间本应观测到的高延迟样本缺失。它不是单个慢请求漏报，而是尾部延迟分布被改写。

## Q008. coordinated omission 最常见的生产事故触发条件是什么？

**回答：**

最常见触发条件是用闭环压测结论指导开放流量系统。测试环境里“发一个、等一个、再发一个”，线上却是用户、消息或上游服务按时间持续到达。系统一暂停，测试流量跟着停；线上流量不会停，于是排队、超时和重试一起爆。

第一类事故是 stop-the-world 暂停。GC、容器 CPU throttle、虚拟机 pause、进程调度停顿、磁盘卡顿，都会让服务短时间不处理请求。闭环客户端只记录手头那批请求慢了，停顿期间本该到达的请求不存在。线上这些请求会排队，p99/p999 远差于压测报告。

第二类事故是线程池或连接池满。压测线程被阻塞后不再发新请求，报告吞吐下降、延迟尚可；真实入口还在接请求，等待队列越来越长。服务端 handler duration 可能没变，因为排队发生在 handler 之前。用户看到的是端到端卡住。

第三类事故是下游周期性慢。比如数据库 checkpoint、对象存储偶发长尾、第三方 API 每隔一段时间抖动。闭环压测里，请求被慢下游挡住后，客户端也停止发新请求；真实流量里，上游请求继续堆积，还会触发重试。结果线上 p99 比压测 p99 高一个数量级。

第四类事故是只统计成功请求。超时、取消、被限流、被队列丢弃的请求没有进入延迟 histogram。系统越差，样本越少，p99 反而看起来稳定。这比普通 coordinated omission 更危险，因为它把失败也从分布里删掉了。

第五类事故是按 worker 完成节奏生成任务。很多任务系统的压测脚本会让 worker 完成一个任务后再创建下一个任务。这样测到的是 worker 最大吞吐，不是 scheduler 面对突发提交时的排队延迟。LogServe 如果只这样测，就看不到 workflow 提交高峰下 ready queue 和 shared log 的等待。

第六类事故是客户端超时比服务端指标短。用户 1 秒超时，服务端 5 秒后完成并记录一个 5 秒成功；或者客户端超时后根本没记录端到端失败。监控如果只在服务端看已完成请求，会同时错过用户失败和未完成队列。

第七类事故是负载发生器能力不足。压测机 CPU、网络、连接数先满，发不出计划流量，但报告仍按“实际发出的请求”计算 p99。看起来系统承受住了，实际上测试没有打到目标到达率。报告必须展示 load generator 自身的饱和度。

我会总结成一句：coordinated omission 事故来自“系统慢时，测量也跟着少测”。只要目标是用户到达或消息到达，就要按到达计划记录，而不是按完成节奏讲故事。

## Q009. coordinated omission 的指标应该怎么设计才不会只看平均值？

**回答：**

coordinated omission 的指标设计要把“计划到达、实际开始、完成结果”拆开。只看平均 latency 或已完成请求 p99，都可能被漏采样骗过。

第一组是到达率指标。包括 planned_arrival_rate、actual_send_rate、accepted_rate、completed_rate。压测报告必须证明自己真的按目标速率发出了请求。如果 planned 是 10k RPS，actual 只有 6k RPS，后面的 p99 不能代表 10k RPS 下的系统。

第二组是生命周期时间戳。每个请求或任务最好有 scheduled_at、sent_at、accepted_at、started_at、completed_at、failed_at。用户感知延迟应该从 scheduled_at 或 sent_at 算到最终结果，而不只是从 handler start 算。队列系统还要有 enqueue_time、dequeue_time、ack_time。

第三组是端到端 histogram。记录 planned_to_complete、sent_to_complete、accepted_to_complete、handler_duration、queue_wait。coordinated omission 主要污染 planned_to_complete 和 sent_to_complete。如果只有 handler_duration，就看不到入口排队。

第四组是失败和缺口。统计 planned_count、sent_count、accepted_count、completed_count、timeout_count、cancel_count、drop_count、rejected_count。还要报告 incomplete requests。压测结束时还有 2 万个未完成请求，不能只说已完成请求 p99 很好。

第五组是修正后的分布。使用 HdrHistogram 这类工具时，可以同时报告 raw histogram 和 corrected histogram，并写清 expected interval。raw 说明闭环观察，corrected 估计固定到达节奏下的用户体验。两者差距很大时，本身就是重要信号。

第六组是负载发生器健康。load generator 的 CPU、GC、网络、socket、open file、event loop lag、send queue、clock drift 都要监控。否则你不知道漏发请求是系统反压，还是压测机自己先趴了。

第七组是服务端饱和。队列深度、oldest age、in-flight、线程池等待、连接池等待、锁等待、GC pause、downstream p99、retry count，要和到达率放在同一张图上。系统慢的时候，如果到达率同时下降，就要怀疑测量被协调了。

第八组是窗口和尾部。看 p50/p95/p99/p999、最大值、超时率、错误率、SLO 达标率。平均值仍然可以保留，但要放在旁边。coordinated omission 最喜欢骗尾部，不是骗平均。

面试里可以这样答：指标要证明请求本该什么时候到、实际什么时候到、什么时候开始处理、什么时候结束、失败多少、漏了多少。没有计划到达率和未完成请求数的压测报告，我不会相信它的 p99。

## Q010. coordinated omission 的正确性边界和性能边界分别是什么？

**回答：**

coordinated omission 的正确性边界在测量，不在系统业务逻辑。它告诉你延迟分布可能被漏采样污染，但它不证明系统功能正确，也不直接说明哪段代码慢。它解决的是“我们有没有把用户等待测进去”。

第一条边界是到达模型。固定并发闭环模型并不总是错。后台 worker 完成一个任务再取下一个任务，这种系统本来就有闭环特征。但如果你用它模拟外部用户流量、API 请求、消息入队、cron 批量触发，就会低估排队。正确性取决于测试模型是否匹配真实流量。

第二条边界是修正只是估计。按 expected interval 补样本，可以让停顿期间缺失的延迟进入分布，但它不能恢复真实请求内容、真实超时、真实重试、真实用户取消。它也不能替代完整的请求生命周期打点。修正后的 histogram 是更接近真实体验的估计，不是万能真相。

第三条边界是服务端和客户端视角。服务端 handler 延迟可以用于优化代码路径，但不能代表用户体验。客户端端到端延迟能代表体验，但不一定告诉你根因。两个都要测，不能互相替代。

性能边界则在负载生成和观测成本。固定到达率压测会继续向系统施压，系统越慢，排队越多，未完成请求越多。你需要足够强的 load generator、连接数、内存和结果存储，否则测试机先成为瓶颈。开放模型更真实，也更容易把被测系统打到雪崩，所以必须有停止条件和安全阈值。

第二个性能边界是数据量。按计划到达记录所有请求、超时和未完成项，会产生大量样本。高 QPS 下需要高效 histogram、采样策略、批量写入和结果压缩。观测不能把压测和被测系统都拖死。

第三个边界是解释成本。修正 coordinated omission 后，p99 可能比以前难看很多。团队要接受这是测量变准了，不是系统突然变差了。接下来还要靠 trace、profile、资源指标定位原因，不能停在“p99 变差”这一步。

所以我会这样说：coordinated omission 的 correctness 是测量模型正确，尤其是到达时间和等待时间不能漏；performance 是开放负载和完整记录带来的压力。它提醒我们，压测不是为了产出漂亮数字，而是为了暴露系统在拥塞时对用户做了什么。
## Q011. 面试官如果只问一个问题检验你是否理解 flame graph，可能会问什么？

**回答：**

我会预期他问一个很容易误判的题：

```text
线上 p99 升高，你抓了一分钟 CPU profile，生成 flame graph。图上某个 JSON 编码函数很宽。你能不能直接说它就是 p99 升高的根因？如果不能，你还要问哪些问题？
```

我的回答会是：不能直接下结论。flame graph 能告诉你采样期间哪些调用栈在某类资源上占比高，比如 CPU on-CPU 样本、off-CPU 阻塞样本、内存分配样本。它很擅长找“时间或资源花在哪里”，但它不是因果证明，更不是 p99 专用工具。

我会先解释图怎么看。Brendan Gregg 的说明里有几个点很关键：x 轴不是时间流逝，而是采样栈的总体占比排列；y 轴是调用栈深度；每个矩形是一个 stack frame；矩形越宽，说明它在采样栈里出现得越频繁。顶部通常表示当时正在消耗 CPU 的函数，下面是调用 ancestry。颜色一般不是热度，很多火焰图颜色只是为了区分相邻块。

第二步要问 profile 类型。CPU flame graph 只能解释 CPU 样本。p99 升高如果来自连接池等待、锁等待、磁盘 I/O、网络、下游 RPC、GC pause、队列等待，CPU 图可能看不出来。这个时候要抓 mutex profile、block profile、off-CPU profile、alloc profile、heap profile，或者结合 trace 和 metrics。一个很宽的 JSON 编码块可能只是系统还有 CPU 活干，真正慢的是其他请求在等数据库。

第三步要问采样窗口。你抓的是事故期间，还是事故后？抓的是所有流量，还是某个实例？这一分钟有没有覆盖 p99 请求？如果只是平均一分钟 CPU，图上的宽块更接近“总 CPU 消耗”，不一定对应尾延迟。p99 问题常常集中在少数 route、tenant、payload size、下游分支。最好按慢请求窗口、特定实例、特定 route 或压测场景抓 profile。

第四步要问负载和基线。没有对比图，很难判断宽块是否异常。JSON 编码一直占 20% CPU，可能是正常业务成本；发布后从 5% 变成 30%，才说明变化。差分 flame graph、前后版本对比、同流量回放，通常比单张图更有说服力。

第五步要问优化收益。宽不等于该优化。一个函数占 40% CPU，但已经是必要工作且很难优化；另一个函数占 5%，却在热路径里造成锁等待或分配尖刺。火焰图告诉你优先看哪里，不替你做工程判断。

结合 LogServe，flame graph 可以用在 workflow 调度、shared log append、metadata replay、Python executor IPC、LLM result reference 序列化这些路径上。如果 workflow p99 升高，CPU flame graph 上看到 JSON 日志编码很宽，只能说明日志编码吃 CPU；还要看日志 sink 是否阻塞、worker queue 是否堆积、fsync 是否慢、trace 里慢请求是否真的卡在日志上。

面试里可以这样答：flame graph 是采样 profile 的调用栈可视化，宽度表示样本占比，不是时间轴，也不是根因判决书。理解它，要能说明采样类型、窗口、流量维度、基线对比和 p99 之间的关系。看到宽块，下一步是验证，不是马上改代码。

## Q012. flame graph 的一句话定义是否容易误导，误导点在哪里？

**回答：**

容易误导。常见定义是：flame graph 用来展示程序性能热点。这个定义太短，容易让人把“热点”理解成“事故根因”。

第一个误导是把 x 轴当时间轴。火焰图的横向位置通常没有时间含义，函数按折叠栈聚合后排列。左边先出现不代表先执行，右边不代表后执行。宽度才是重点，表示在采样集合中的占比。想看随时间变化，要用时间序列、trace、FlameScope 这类按时间切片的视图，或者分窗口抓 profile。

第二个误导是把颜色当热度。经典 flame graph 的颜色多半是随机或按类型映射，用来帮助区分块。红色不一定更慢，蓝色不一定更冷。除非你明确使用了 differential 或 hot/cold 变体，否则不要靠颜色判断问题。

第三个误导是把 CPU 图当所有性能问题。CPU flame graph 只能显示 CPU 栈样本。服务慢经常是 off-CPU：锁、I/O、网络、sleep、channel、futex、GC stop、连接池等待。CPU 图很干净，p99 仍然可能很差。要根据症状选择 profile 类型。

第四个误导是忽略采样偏差。采样频率太低会漏掉短函数；栈展开失败会把样本归到错误位置；JIT、内联、符号缺失、容器权限、eBPF/perf 配置都会影响结果。语言运行时的 profile 也可能只看到用户态，看不到内核或 native 库。

第五个误导是把累计宽度和单次请求延迟混在一起。一个函数很宽，说明它在总样本里占比高，可能是因为很多请求都调用它一点点；p99 慢可能来自少数请求调用另一个函数很久。总 CPU 热点和尾延迟根因有重叠，但不是同一个概念。

第六个误导是只看最上层。顶部宽块表示 leaf 消耗，下面的父栈能告诉你调用路径。同一个函数可能被多个路径调用。优化前要知道是哪条业务路径把它叫热了，否则可能改错地方。

更准确的一句话是：flame graph 是把采样 profile 的调用栈折叠并按层级展开的可视化，宽度表示某个函数或调用路径在样本集合中的占比；它帮助定位资源消耗集中处，但需要结合采样类型、时间窗口和业务维度解释。

## Q013. flame graph 最常见的生产事故触发条件是什么？

**回答：**

flame graph 本身不会触发生产事故，真正常见的是误用它导致排障方向错，或者 profiling 开销在事故中放大问题。面试官问这个题，多半想看你会不会把工具当真相。

第一类事故是抓错 profile。p99 是锁等待导致的，却只抓 CPU flame graph。图上看到 JSON 编码、日志字段构造、protobuf marshal 很宽，于是团队花两天优化 CPU。最后发现真正问题是数据库连接池耗尽，请求都在等连接。CPU 图没有撒谎，是你问错问题。

第二类事故是抓错时间窗口。事故发生在 10:03 到 10:05，profile 抓在 10:10。图上看到的是恢复后的正常流量。或者 p99 只发生在某个实例，profile 抓了另一台。火焰图必须和指标时间、实例、版本、route 对齐。

第三类事故是把宽块当根因。某个函数占 30% CPU，可能只是因为它是核心业务的正常成本。真正导致事故的是发布后多了一次下游调用、连接池排队、重试风暴。没有基线或差分图，单张火焰图只能说“这里花得多”，不能说“这里导致了变化”。

第四类事故是 profiling 开销过大。线上高峰期临时打开高频采样、全量 alloc profile、阻塞 profile、详细符号化，可能增加 CPU、内存、锁竞争或 I/O。尤其是已经 p99 升高时，再加重观测开销，会让事故更严重。profile 要有采样频率、持续时间和安全开关。

第五类事故是符号和栈不完整。容器里缺 perf 权限，二进制 stripped，JIT 符号没加载，Go 内联影响栈，Cgo/native 栈断裂。图看起来很干净，实际上把大量样本归成 unknown 或 runtime。没有检查 unknown 占比和采样质量，结论会很虚。

第六类事故是忽略业务维度。把所有 route 混在一起抓 profile，最热的可能是高 QPS 的健康接口；真正 p99 差的低 QPS 接口在图上很窄。要么按场景压测，要么对慢 route 单独抓，要么用 trace/exemplar 定位慢请求再 profile 相近负载。

第七类事故是优化后没验证端到端。团队把火焰图上的宽块优化了 20%，CPU 降了，用户 p99 没变，因为瓶颈转移到了下游或队列。profile 优化要用同一 workload 复测 p99、吞吐、错误率、资源饱和，而不是只看火焰图变窄。

我会总结成一句：flame graph 事故不是图错了，而是人把它用到了不匹配的问题上。先定症状和假设，再选 profile 类型、窗口和维度，最后用端到端指标验证收益。

## Q014. flame graph 的指标应该怎么设计才不会只看平均值？

**回答：**

flame graph 不是指标，但它应该嵌在一套性能诊断指标里。否则你只会有一张漂亮图，不知道它代表什么流量、什么窗口、什么用户影响。

第一组是触发指标。抓 profile 前，先用 metrics 判断症状：request p99、error rate、QPS、CPU、GC pause、queue wait、lock wait、connection pool wait、downstream p99。profile 应该由明确症状触发，比如某实例 CPU 超 80% 且 p99 升高，或 mutex wait p99 升高。

第二组是 profile 元数据。每次 profile 要记录 service、instance、version、region、route 或 workload、start_time、duration、profile_type、sampling_rate、CPU quota、QPS、p99、error rate。没有这些元数据，过几天回看图，很难知道它说明什么。

第三组是采样质量。记录 samples count、dropped samples、unknown frames ratio、kernel/user split、runtime frames ratio、symbolization failure、profile overhead。unknown 太多的图不适合下结论。样本太少也不要硬解释细枝末节。

第四组是多类型 profile。CPU、alloc、heap、mutex、block、goroutine、off-CPU、I/O，各自回答不同问题。指标面板要能从 p99 症状跳到对应 profile，而不是所有问题都打开 CPU 图。

第五组是差分和趋势。保存发布前后、优化前后、事故前后的 profile，并能做差分 flame graph 或 top function delta。绝对宽度有用，但变化更有价值。排障常问的是“什么变了”，不是“什么一直很贵”。

第六组是端到端验证。profile 发现优化点后，必须用 p50/p95/p99/p999、吞吐、错误率、资源、成本复测。函数变窄只是局部收益，用户不一定感知。最好在 benchmark、压测和线上灰度里都验证。

第七组是业务维度。按 route、tenant tier、task type、workflow type、payload size 抽样 profile。LogServe 里可以分别抓 shared log append 压测、metadata replay、worker execution、LLM 调用、Python IPC。混在一起抓全局 profile，只能看到最大流量路径。

第八组是开销预算。profiling 本身要有 CPU、内存、磁盘和安全预算。持续 profiling 可以低频运行；临时高频 profile 要有超时和采样限制。指标里要能看到 profiler 是否导致 dropped data 或业务 p99 抖动。

面试里可以这样答：flame graph 要和 p99、吞吐、错误率、资源饱和、profile 元数据、采样质量和差分对比配套。它不是平均值替代品，也不是独立证据。它告诉你样本里的资源花在哪里，指标告诉你这件事对用户有没有意义。

## Q015. flame graph 的正确性边界和性能边界分别是什么？

**回答：**

flame graph 的正确性边界是采样 profile 的忠实可视化。它可以正确地展示“在这个采样窗口、这个 profile 类型、这些栈能被正确展开的前提下，哪些调用栈出现得更频繁”。它不能证明没有别的问题，也不能证明某个宽块就是事故根因。

第一条边界是 profile 类型。CPU flame graph 的正确性只覆盖 on-CPU 样本；alloc flame graph 覆盖分配样本；off-CPU 覆盖阻塞等待。拿 CPU 图解释 I/O 等待，就是超出边界。

第二条边界是采样质量。采样频率、符号解析、栈展开、JIT、内联、容器权限、内核栈、语言运行时，都会影响图。unknown frames 很多时，火焰图仍然能生成，但结论不可靠。正确使用前要先检查图的可信度。

第三条边界是聚合范围。全局 profile 只能解释全局资源占比。p99 请求、某个 route、某个 tenant 的问题，需要针对那个范围采样或用 trace 关联。否则高流量健康路径会把低流量慢路径淹没。

第四条边界是因果。火焰图是观察，不是实验。宽块可能是原因，也可能是结果，也可能是正常成本。要通过基线对比、差分、定向 benchmark、代码改动和端到端指标验证因果。

性能边界则是采样和符号化开销。低频 CPU 采样通常可接受，高频采样、全量内存 profile、阻塞 profile、持续符号化都可能有成本。生产环境要控制采样率、持续时间、数据量和权限。观测工具不能为了找 p99 把 p99 再拉高。

第二个性能边界是优化收益递减。火焰图能找到大块，但不是每个大块都值得改。优化一个占 50% CPU 的函数可能收益巨大，也可能因为下游瓶颈不变而没有用户收益。优化后还可能改变资源分布，把瓶颈推到锁、I/O 或网络。

第三个边界是可读性。调用栈太深、函数名太长、模板/JIT 生成代码太多、goroutine 或协程栈太碎，图会很难读。需要折叠、过滤、按包聚合、按业务路径拆分。否则火焰图只是墙纸。

所以我会这样说：flame graph 的 correctness 是“在采样范围内展示调用栈占比”，performance 是“用低成本采样换定位线索”。它适合找热点和对比变化，不适合单独证明 p99 根因。真正的闭环是指标发现问题，profile 提供假设，实验验证收益。
## Q016. 面试官如果只问一个问题检验你是否理解 Jepsen，可能会问什么？

**回答：**

我会预期他问一个关于“测试结论能证明什么”的题：

```text
你写了一个 Jepsen 风格测试：5 个节点，随机读写，期间注入网络分区、进程 kill、时钟偏移。跑了 30 分钟没有发现 linearizability violation。你能不能宣布系统是线性一致的？如果不能，还缺哪些东西？如果发现一个异常 history，你怎么判断是系统 bug、测试模型 bug，还是 client 记录错了？
```

这道题能直接检验你是不是把 Jepsen 理解成“分布式系统压测工具”。Jepsen 更准确地说，是一套黑盒分布式系统正确性测试方法和 Clojure 工具库：部署真实系统，生成并发操作，注入故障，记录操作 history，再用 checker 对照一致性模型分析这个 history 是否可能合法。它可以发现很强的反例，但一般不能证明系统永远正确。

我会先说 Jepsen 的几个部件。DB 部分负责安装、启动、停止被测系统；client 负责把操作发给系统；generator 生成并发操作；nemesis 注入故障，比如网络分区、进程 kill、时钟扰动；history 记录每个操作的 invoke、ok、fail、info；checker 根据模型分析 history。这个结构比“跑几个脚本打请求”严谨，因为它把操作时序和结果作为一等数据保存下来。

第二步说模型。你要先声明想验证什么：linearizability、serializability、read committed、set、register、queue、counter，还是某个业务不变量。模型不对，测试就没意义。Jepsen consistency guide 里把 consistency model 定义为系统合法 histories 的集合；Elle 这类 checker 也通过事务之间的依赖图和异常类型来找不可解释的历史。也就是说，Jepsen 不是自动知道你的业务应该对，它只检查你给它的模型。

第三步说结果解释。没发现异常，不等于系统正确。可能是运行时间太短，故障不够狠，操作组合不够多，负载太低，checker 模型太弱，client 没记录到关键返回，或者 bug 概率低。Jepsen 分析页面自己也强调，它偏向真实二进制、真实集群、分布式故障和随机生成测试；这种测试能找生产可观察 bug，但不是形式化证明。

第四步说发现异常后怎么做。先保留 history、日志、节点状态、nemesis 时间线、版本和配置。然后检查 client 是否正确区分 ok、fail、timeout、unknown；检查模型是否符合系统文档承诺；缩小 history 找最小反例；用更简单 workload 重现；最后再对照系统日志确认丢写、脏读、乱序、重复提交、读到失败事务结果，还是测试假设错了。

结合 LogServe，如果要做 Jepsen 风格测试，重点不是“跑大流量”，而是构造可检查历史：SubmitWorkflow、LeaseTask、CompleteTask、FailTask、ReplayMetadata、ReadWorkflowState 这些操作要有唯一 ID、开始时间、结束时间和结果；nemesis 可以 kill worker、暂停 log append、删除临时文件、模拟对象存储失败；checker 要检查任务不会凭空完成、同一 attempt 不会被两个 worker 成功提交、replay 后 metadata 能由 shared log 解释。

面试里可以这样答：Jepsen 是用真实系统、并发 history、故障注入和模型 checker 找一致性反例的方法。理解它，不是会说“网络分区测试”，而是能说清楚 workload、nemesis、history、checker、consistency model 和结果边界。它能证明“这条历史违反了模型”，但通常不能证明“系统没有 bug”。

## Q017. Jepsen 的一句话定义是否容易误导，误导点在哪里？

**回答：**

容易误导。常见定义是：Jepsen 是分布式系统故障注入测试工具。这个定义只说到一半，容易把 Jepsen 降级成 chaos engineering 或压测。

第一个误导是只强调故障注入。Jepsen 的故障注入很重要，但真正有杀伤力的是故障下的 history checking。只 kill 节点、断网络、调时钟，然后看服务有没有恢复，这只是可用性或恢复测试。Jepsen 要问的是：在这些故障期间，系统返回给客户端的结果是否仍然满足声明的一致性模型。

第二个误导是把它当性能压测。Jepsen 可以生成性能和可用性图，但它的主线是 safety。高吞吐、低延迟、p99 好看，都不能替代一致性检查。反过来，一个 Jepsen 测试吞吐很低，也可能足够发现严重正确性 bug。性能和正确性要分开解释。

第三个误导是以为它只测数据库。Jepsen 被用于数据库、队列、协调服务、任务调度器、流系统等。只要你能定义操作、故障和模型，就可以写测试。LogServe 这类任务执行系统也可以借这个思路检查“任务是否丢、是否重复完成、恢复后状态是否可解释”。

第四个误导是以为 Jepsen 结论自动权威。Jepsen 测试也会写错。client 可能把 unknown 当 fail，模型可能太强或太弱，checker 可能不适合这个数据类型，nemesis 可能没覆盖真实故障，测试时间太短。Jepsen 发现的反例很有价值，但需要人工复核和重现。

第五个误导是把“没发现异常”当认证。Jepsen 不是认证章。它是反例搜索。跑过一次、一个版本、一个 workload、一个故障组合，只能说明这些条件下没有找到违反模型的 history。版本、配置、负载、部署拓扑一变，结论就要重新评估。

第六个误导是忘记文档承诺。你不能用 linearizability 去要求一个只承诺 eventual consistency 的系统，然后宣布它错了。测试模型必须和系统对外承诺、业务需求对齐。反过来，如果系统文档承诺强一致，Jepsen history 发现非线性化结果，那就是很强的证据。

更准确的一句话是：Jepsen 是一套黑盒分布式正确性测试方法和工具库，通过生成并发操作、注入真实故障、记录 history，并用模型 checker 判断这些观察是否违反系统承诺。故障注入只是其中一环。

## Q018. Jepsen 最常见的生产事故触发条件是什么？

**回答：**

如果说“Jepsen 事故”，本质上通常是分布式系统在异常窗口里暴露了文档承诺和实现之间的差距。Jepsen 只是把这个差距更快找出来。常见触发条件集中在网络分区、leader 切换、时钟、重试、恢复和跨节点状态复制。

第一类是网络分区。多数节点和少数节点都以为自己能服务，或者客户端被路由到旧 leader，导致 split-brain、丢写、旧读、重复提交。系统正常网络下没问题，一分区就暴露 quorum、lease、fencing 或 leader epoch 设计不严。

第二类是进程崩溃和重启。leader ack 了写入但还没复制到足够副本，随后崩溃；follower 恢复后带着旧日志继续服务；本地缓存恢复顺序错，把未提交状态暴露给读者。WAL、snapshot、commit index、apply index、metadata replay 的边界不清，都会在这里出问题。

第三类是时钟异常。依赖本地时钟做 lease、session、TTL、last-write-wins、transaction timestamp、visibility timeout，一旦节点时钟跳变或暂停，就可能提前过期、延迟过期、把旧写当新写。Jepsen 常测 clock skew，是因为真实系统里 NTP、虚拟化和 GC pause 都会制造时间错觉。

第四类是重试和 unknown 结果。客户端超时后不知道操作是否成功，于是重试。系统如果没有幂等 key、request id、事务状态查询或 fencing，就可能重复写入；如果把 unknown 当 fail，又可能读到“失败操作”的结果。很多一致性 bug 不在正常 ok/fail 路径，而在 unknown 结果恢复上。

第五类是异步复制和读路径绕过。写入走 leader 和 quorum，读取却从落后 follower、本地缓存、materialized view、搜索索引读。正常延迟下看不出，分区或恢复时就读到旧值。系统如果承诺 linearizable read，就必须保证读路径也参与同样的时序约束。

第六类是后台修复和 compaction。节点恢复后 anti-entropy、reconciliation、compaction、snapshot install、log truncation 可能覆盖新值、复活旧值或丢失 tombstone。很多系统不是在故障瞬间错，而是在修复过程中错。

第七类是测试环境没有覆盖真实拓扑。生产跨 AZ、跨地域、异步备份、负载均衡、TLS、磁盘满、慢 I/O、DNS 抖动，测试只在本机 docker 里 kill 进程。Jepsen 风格测试如果 nemesis 太温柔，也会给团队一种错觉。

结合 LogServe，类似触发条件包括：worker 完成任务后 ack 前崩溃，metadata 已更新但 shared log 未提交，replay 时重复触发外部副作用，lease 到期后旧 worker 仍提交结果，对象存储写成功但 result reference 没落日志。它们不一定需要完整 Jepsen 才能测，但要用同样的历史和模型思路定义反例。

我会总结成一句：Jepsen 最容易揭穿的是“正常路径看起来一致，异常窗口里没有全局事实来源”。leader、quorum、lease、日志、重试和恢复只要有一个边界模糊，history 就可能出现无法解释的结果。

## Q019. Jepsen 的指标应该怎么设计才不会只看平均值？

**回答：**

Jepsen 的“指标”不能只理解成性能指标。它首先要输出正确性证据：history 是否 valid，违反了哪个模型，哪个最小反例，哪些故障正在发生。性能图只是辅助。

第一组是 correctness 结果。包括 valid/invalid、consistency model、anomaly type、minimal counterexample、涉及的 operations、key、transaction、process、nemesis interval。invalid 不是一个数字，要能让人读懂为什么这段 history 不可能发生。

第二组是 history 覆盖。记录 operations count、ok/fail/info 分布、read/write/txn 比例、key 数量、并发进程数、每个 key 的操作数、unknown count。一个测试跑了 30 分钟但大部分操作都是 timeout，或者只碰了一个 key，结论范围就很窄。

第三组是 fault 覆盖。记录 network partition 类型、partition 持续时间、kill/restart 次数、clock skew 幅度、pause 时长、disk fault、packet loss、nemesis schedule。没有故障覆盖图，就不知道测试到底有没有打到危险窗口。

第四组是 availability。Jepsen 常常同时展示可用性：成功率、失败率、timeout、unavailable window、recovery time、per-operation latency。正确性和可用性要分开看。系统可以在分区期间拒绝请求来保持安全，这不等于失败；系统也可以高可用但返回错误结果，这更危险。

第五组是 latency 分布。看 p50/p95/p99/p999、最大值、按操作类型拆分、按故障阶段拆分。不要只给平均值。分区期间写入 p99、恢复后读 p99、正常期 p99 是不同问题。Jepsen 性能图要和 nemesis timeline 对齐。

第六组是 model 强度。报告里要写清楚检查的是 linearizable、serializable、snapshot isolation、read committed，还是业务自定义 invariant。弱模型 valid 不能推出强模型 valid。比如 eventual consistency 下合法的 history，在线性一致模型下可能明显非法。

第七组是复现材料。保存版本、配置、拓扑、随机种子、workload 参数、nemesis 参数、client 日志、服务日志、history 文件、checker 输出。Jepsen 的价值不只是红绿结果，而是能让开发者复现和缩小问题。

第八组是 LogServe 这类系统的业务不变量。可以记录 submitted_tasks、leased_tasks、completed_tasks、duplicate_completion、lost_task、orphan_result、replay_mismatch、stale_metadata_read、old_worker_rejected。平均任务耗时不能证明任务没丢。正确性指标要直接对应不变量。

面试里可以这样答：Jepsen 指标要把 correctness、history 覆盖、fault 覆盖、availability、latency、model 强度和复现材料放在一起。平均延迟只是性能侧的一个小数。Jepsen 最重要的输出是“这段观察到的历史是否能被模型解释”。

## Q020. Jepsen 的正确性边界和性能边界分别是什么？

**回答：**

Jepsen 的正确性边界是反例搜索和模型检查。它能很有力地证明“在这次测试记录的 history 里，系统违反了某个模型”。它通常不能证明“系统在所有可能执行中都正确”。这不是缺点，是测试方法的边界。

第一条边界是模型。checker 只能检查你定义的模型。你检查 linearizable register，不能自动证明事务隔离；你检查 append-only set，不能自动证明任务调度幂等；你检查单 key，不代表多 key 事务没问题。模型太弱会漏 bug，模型太强会把系统没承诺的行为报成错。

第二条边界是 workload。生成器没产生某类操作组合，就测不到对应 bug。只测读写，不测 compare-and-set；只测单 key，不测跨 key；只测短事务，不测长事务；只测正常 client，不测 timeout 和 unknown。Jepsen 的结论只覆盖实际历史和相近场景。

第三条边界是 nemesis。没有注入某类故障，就不能声称系统抗那类故障。网络分区、进程 kill、pause、clock skew、disk full、slow fsync、packet loss、one-way partition、leader flapping，触发的 bug 不一样。故障模型要和生产风险匹配。

第四条边界是观察。client 记录的开始时间、结束时间、返回状态、错误类型如果不准，history 就会污染。尤其是 timeout 和 unknown 结果，不能随便当失败。Jepsen 测试代码本身也要被审查。

第五条边界是非确定性。没发现 bug，可能只是没撞到。随机测试要多种 seed、多轮运行、不同负载、不同拓扑。对关键系统，还要结合模型检查、形式化验证、代码审查、故障注入测试和生产监控。

性能边界则是测试成本。Jepsen 测试会部署真实集群、注入故障、记录详细 history、运行 checker。history 很大时，检查可能耗 CPU、内存和时间；故障注入会降低吞吐，甚至让集群长时间不可用。它适合验证关键语义，不适合每次提交都跑超长完整矩阵。

第二个性能边界是结果分析成本。一个 invalid history 往往要人工读、缩小、复现。Elle 这类 checker 能给出依赖图和异常类型，但仍然需要判断模型、client、系统日志是否一致。Jepsen 不是点一下就给修复方案。

第三个边界是环境成本。真实 VM、跨区域、磁盘故障、时钟偏移比本地容器更接近生产，也更贵、更慢、更难稳定。测试策略通常分层：本地小测试快速跑，夜间或发布前跑更重的故障矩阵，重大版本跑接近生产的拓扑。

所以我会这样说：Jepsen 的 correctness 是“对观测 history 的模型反例检查”，不是全域证明；performance 是“用真实故障和详细记录换强证据”，代价是环境、运行和分析成本。它最适合验证那些一旦错了就很难靠监控补救的分布式不变量。