# 50. cgroup、HPA、SLO 与 error budget 追问链

这一批放四个运维和可靠性面试里经常被追问的主题：cgroup、HPA、SLO 和 error budget。它们表面上分别属于 Linux 资源控制、Kubernetes 自动扩缩容、服务可靠性目标和发布风险管理，但共同点很强：都不是一句“自动限制”“自动扩容”“可用性目标”“允许错误”就能说清楚的东西，真正的理解要落到控制对象、观测口径、反馈延迟和边界条件。

这组回答仍然按 LogServe 的口径写。LogServe 可以借这些问题解释容器资源边界、任务调度压力、延迟 SLO、失败预算和发布风险控制，但不能把自己包装成完整的 Kubernetes 平台或成熟 SRE 体系。面试时更稳的说法是：项目里验证的是机制和工程取舍，例如任务执行、日志持久化、恢复、限流、观测与重试；cgroup、HPA、SLO 和 error budget 是部署与运维层面的约束和目标，需要和集群、运行时、监控系统、告警策略一起闭环。

## Q001. 面试官如果只问一个问题检验你是否理解 cgroup，可能会问什么？

**回答：**

我会预期他问一个容器事故题：

```text
一个 Pod 配了 1 核 CPU limit、512Mi memory limit。监控上 CPU 平均使用率只有 50%，内存 RSS 也没有长期超过 512Mi，但业务 p99 抖动严重，偶尔还被 OOMKilled。你怎么判断这是应用问题、容器运行时问题，还是 cgroup 资源边界触发了？
```

这个问题比问“cgroup 是什么”更能检验理解。cgroup 的核心不是“Docker 的资源限制”，而是 Linux 内核把进程组织成层级，并通过不同 controller 对 CPU、内存、I/O、pids 等资源做 accounting、限制和分配。容器只是把 cgroup、namespace、capability、seccomp、文件系统隔离等机制组合起来的一种运行形态。

我会先问它是 cgroup v1 还是 v2。v2 是统一层级，controller 的开启有 top-down 约束，父 cgroup 没开某个 controller，子 cgroup 不能凭空使用；普通资源域还有 no internal process 约束，也就是说父节点要把资源分配给子节点时，内部进程和子 cgroup 的混用有明确限制。这个层级语义会影响“我明明给容器配了 limit，为什么还受上层限制”的排查。

CPU 侧要区分 request、share/weight、quota 和实际调度。Kubernetes request 主要影响调度和相对权重，limit 通常会转成 CFS quota。CPU 平均 50% 并不代表没有 throttling，因为 quota 是按周期扣的，一个短时间突刺可能在周期内把额度用光，后半个周期被限流，结果就是 p99 抖动。看平均 CPU 会漏掉这个问题，应该看 throttled periods、throttled time、run queue、PSI 和业务 tail latency 的时间对齐。

内存侧要看 cgroup 口径，不只看进程 RSS。cgroup memory 统计可能包含匿名内存、page cache、slab、socket buffer、tmpfs、sidecar 开销等；Go、Java、Python 的运行时堆、native memory、mmap、线程栈也可能让应用自以为“堆没满”，但 cgroup 已经到达 memory.max。v2 里 memory.high 可以触发 reclaim 和限速，memory.max 是硬上限，memory.events 里的 high、max、oom、oom_kill 能直接告诉你是否触过边界。

然后我会把容器视角和节点视角分开。Pod 被 OOMKilled 可能是自己打到 cgroup memory.max，也可能是节点内存压力下 kubelet 驱逐；CPU 抖动可能是容器 limit 导致，也可能是节点上其他 workload 竞争、NUMA、I/O wait 或 GC。cgroup 是资源边界的第一层证据，但不是唯一证据。

结合 LogServe，我会这么回答：如果 scheduler、worker 或日志 replay 任务跑在容器里，cgroup 触发会表现成任务执行延迟、重试增多、心跳超时和 p99 波动。项目层面应该暴露 worker 处理耗时、队列长度、重试次数、持久化延迟；部署层面还要同时看 cgroup CPU throttling、memory.events、OOMKilled、node pressure。两边对齐，才能判断是业务代码慢，还是资源边界先把它卡住。

## Q002. cgroup 的一句话定义是否容易误导，误导点在哪里？

**回答：**

容易误导。常见一句话是：cgroup 是 Linux 用来限制容器资源的机制。这句话方便记忆，但至少有六个误导点。

第一个误导是把 cgroup 等同于容器。cgroup 可以管理任意进程组，systemd service、批处理任务、数据库进程、容器 runtime 都可以用它。容器需要 cgroup，但 cgroup 本身不提供完整容器语义。文件系统视图、网络命名空间、进程命名空间、用户命名空间、权限边界和 syscall 限制都不是 cgroup 一件事完成的。

第二个误导是只说“限制”，忽略 accounting 和优先级。很多 controller 既能记录使用量，也能设置硬上限、软保护、相对权重或回收压力。CPU weight、CPU quota、memory.low、memory.high、memory.max、io.weight、pids.max 的含义不一样。把它们都说成 limit，会在事故里把“被限流”“被回收”“被 OOM kill”“只是优先级低”混在一起。

第三个误导是忽略层级。cgroup 是树，不是单个容器上的平面标签。子 cgroup 不能突破父 cgroup 的约束。一个 Pod 看起来有自己的 limit，但如果上层 slice、QoS class、node-level policy 或 systemd 层级已经收紧，子进程仍然会受上层影响。排查时只看容器目录是不够的。

第四个误导是把 request 和 limit 混为一谈。Kubernetes 里 request 影响调度和资源保证口径，limit 影响运行时上限。CPU request 低、limit 高，可能调度到拥挤节点上；CPU limit 太低，可能被 CFS throttling；内存 request 低可能在节点压力下更容易被驱逐；内存 limit 太紧则可能直接 OOMKilled。它们不是一个旋钮。

第五个误导是认为 cgroup 能保证性能。cgroup 能设置边界和相对分配，但它不能保证应用 p99、不能自动解决锁竞争、不能让慢磁盘变快，也不能替代容量规划。CPU quota 可以防止一个容器吃满机器，但也可能让 latency-sensitive workload 在短突刺时被周期性卡住。

第六个误导是忽略 v1/v2 差异。v1 多层级、多 controller 独立挂载，历史上容易出现视角不一致；v2 统一层级，语义更清晰，但接口文件名、事件指标和约束方式变了。线上迁移或混合集群里，指标采集器如果按旧路径读，就可能读错或读不到。

所以更准确的一句话是：cgroup 是 Linux 内核把进程放进层级化资源域，并通过 controller 进行资源统计、限制、保护和分配的机制；容器资源限制只是它最常见的工程使用场景之一。

## Q003. cgroup 最常见的生产事故触发条件是什么？

**回答：**

最常见的触发条件是“平均资源看起来没满，但局部边界已经被打穿”。cgroup 事故很少表现成简单的 CPU 长期 100% 或内存长期满格，更多是限额、周期、层级、运行时和指标口径之间的错位。

第一类是 CPU limit 引发的尾延迟。服务平均 CPU 只有 40% 到 60%，但请求在短时间批量进入，某几个线程或 goroutine 把 CFS quota 用完，容器在当前周期剩余时间被 throttled。监控只看一分钟平均 CPU，会觉得资源很充足；用户看到的是 p99/p999 抖动、超时、重试和排队。

第二类是内存 limit 与真实内存口径不一致。应用只看语言运行时堆，例如 Go heap 或 JVM heap，觉得离上限还有距离；cgroup 看到的是匿名内存、page cache、mmap、线程栈、native allocation、sidecar、tmpfs、socket buffer 等总和。日志压缩、批量序列化、对象存储下载、TLS buffer、profiler、sidecar 都可能把容器推到 memory.max。

第三类是 page cache 被误判。写日志或读大文件时，page cache 上升，业务监控说进程 RSS 不高，但 cgroup memory.current 增长明显。内核可以回收 page cache，但在内存压力、脏页回写、I/O 慢或 memory.high 触发时，应用会被 reclaim 拖慢。最后表现成写入延迟上升，不一定先表现成 OOM。

第四类是 sidecar 抢资源。Service Mesh sidecar、日志采集 agent、指标 exporter、安全 agent 和应用在同一个 Pod 或同一个节点上竞争 CPU、内存和 I/O。业务团队只看主容器，忽略 sidecar 的 cgroup 使用量，结果主容器背锅。尤其是 TLS 终止、访问日志、流量镜像和压缩场景，sidecar 不是零成本。

第五类是 pids 限制或线程数失控。应用没有 OOM，也没有 CPU 满，但新线程、新进程或 fork 失败。原因可能是 pids.max 打满、线程池泄漏、子进程执行器没有回收、压测时每个请求启动外部命令。错误信息常常被包装成“无法创建线程”或“resource temporarily unavailable”。

第六类是 I/O controller 或底层存储竞争。看 CPU 和内存都正常，日志 append、WAL fsync、checkpoint 或对象读取突然慢。cgroup I/O 权重、blkio 限速、云盘 burst credit、节点上其他容器的 I/O 都可能影响。只看应用层平均耗时，看不出是哪个 cgroup 或设备在排队。

第七类是运行时没有按 cgroup 配置自适应。老版本 JVM、Go runtime 或一些线程池默认按宿主机 CPU 核数和内存大小估算并发度，容器 limit 只有 1 核，它却开出几十个线程或把 GC 目标设得太激进。现在主流运行时对容器感知已经好很多，但仍要确认版本、参数和镜像基础环境。

LogServe 的场景里，最危险的是日志写入、任务 replay、workflow 调度、外部调用重试叠在一起：CPU 被 throttled 后任务变慢，任务变慢触发重试，重试增加日志和网络调用，内存与 I/O 压力继续上升。面试时要把这个反馈环说出来，而不是只说“容器资源不够”。

## Q004. cgroup 的指标应该怎么设计才不会只看平均值？

**回答：**

cgroup 指标要围绕“边界是否被触发”设计，而不是围绕“平均用量是多少”设计。平均 CPU、平均内存、平均 I/O 带宽都只能说明总体消耗，不能说明容器有没有被 throttled、被 reclaim、被 OOM、被 pids 限制或被 I/O 队列拖慢。

CPU 要至少看四类。第一是使用量，按容器、Pod、进程和核心维度看 CPU seconds。第二是限流，重点看 throttled periods、total periods、throttled time，算 throttling ratio 和 throttled time p95/p99。第三是压力，结合 PSI 的 some/full 指标看任务等待 CPU 的时间占比。第四是业务尾延迟，把 CPU throttling 与请求 p99、队列长度、重试、超时在同一时间轴上对齐。

内存要看 current、peak、events 和组成。memory.current 只能告诉你当前值，memory.peak 或历史峰值更接近事故；memory.events 里的 high、max、oom、oom_kill 要作为边界触发计数；匿名内存、file cache、slab、sock、tmpfs、swap 的拆分可以帮助判断是堆膨胀、page cache、网络 buffer 还是系统对象。只看 RSS 会漏掉很多容器内存压力。

I/O 要看吞吐、延迟和排队，而不是只看 MB/s。按 cgroup 和设备拆分 read/write bytes、read/write ios、queued time、await、fsync latency、log append latency、checkpoint latency。日志型系统尤其要把应用层 append p99 与底层 I/O 延迟放在一起，否则会把存储抖动误判成调度器慢。

pids 要看当前值和失败事件。pids.current 接近 pids.max 时，线程池扩张、子进程执行、shell 调用、runtime helper 都可能失败。这个指标平时很安静，一旦触发会表现成零散的 resource unavailable，必须有事件计数和接近上限告警。

还要看层级和邻居。容器自己的 cgroup 指标、Pod 聚合、node-level pressure、QoS class、system slice、sidecar 指标都要能串起来。一个容器没到上限，不代表父 cgroup 没有压力；一个主容器没问题，不代表同 Pod 的 sidecar 没有抢资源。

指标粒度上要避免只看一分钟平均。CPU quota 按很短周期生效，内存 high/max 也可能是瞬时触发，I/O 队列可以在几秒内堆满。建议保留短窗口 max、p95、p99、事件计数、速率和 exemplars，事故时能从业务请求 trace 跳到当时的 cgroup 状态。

对 LogServe，我会把这些指标映射到业务指标：worker runnable 队列长度、任务执行 p99、append-only log 写入 p99、replay 耗时、checkpoint 耗时、外部调用重试、actor mailbox 积压。cgroup 指标告诉我资源边界是否触发，业务指标告诉我边界触发有没有伤害用户可见语义。

## Q005. cgroup 的正确性边界和性能边界分别是什么？

**回答：**

cgroup 的正确性边界要先说清楚：它控制资源域，不证明程序逻辑正确。它可以限制一个进程组能用多少 CPU、内存、I/O 或进程数，可以统计和触发事件，但它不能保证并发状态机不竞态，不能保证日志 fsync 语义正确，不能保证分布式一致性，也不能保证请求一定在某个延迟内完成。

资源隔离也不等于安全隔离。cgroup 能限制资源消耗，不能单独提供文件系统隔离、网络隔离、用户权限隔离或 syscall 安全边界。容器逃逸、特权容器、hostPath、capability、seccomp、AppArmor、SELinux、namespace 配置都属于另一组问题。把“cgroup 限制了资源”说成“容器就是安全的”，在面试里很容易被追问穿。

CPU 正确性边界是 quota 和 weight 只影响调度与分配，不保证公平到每个请求，也不保证低延迟。CPU limit 可以避免单个容器长期吃满宿主机，但对突刺型服务可能制造周期性 throttling。CPU request 或 weight 能表达相对重要性，但节点过载时仍然会有排队和抖动。

内存正确性边界是 hard limit 会杀进程或触发 OOM，soft/high 类机制更多是回收和压力调节。memory.max 保护节点不被单个 cgroup 吃光，但对应用而言可能是突然终止；memory.high 可以提前施压，但也可能让延迟变差。正确的系统设计必须能处理进程被杀、重启、replay 和幂等恢复。

性能边界主要来自四个方面。第一是观测和控制有成本，controller 统计、I/O 调度、内存回收都不是免费的。第二是限制会改变应用运行形态，例如 CPU throttling 改变 goroutine 调度和 GC 节奏。第三是层级和邻居会让资源争用复杂化。第四是运行时和应用参数如果没按 cgroup 调整，线程池、GC、批大小、连接池可能都用错宿主机级别的假设。

一个成熟回答应该落到取舍：cgroup 给了部署层的资源边界和故障半径控制，但低延迟服务要谨慎设置 CPU limit，内存服务要明确 OOM 恢复路径，I/O 密集服务要有日志写入和 checkpoint 的独立指标。对 LogServe 来说，正确性靠 append-only log、幂等任务、状态 replay 和持久化边界；cgroup 只能控制运行环境的资源压力，不能替代这些机制。
## Q006. 面试官如果只问一个问题检验你是否理解 HPA，可能会问什么？

**回答：**

我会预期他问一个扩容失效题：

```text
一个服务配置了 HPA，CPU 目标利用率 60%，minReplicas 是 4，maxReplicas 是 20。流量突刺后 HPA 的确从 4 扩到 20，但用户已经大量超时；另一次故障里业务延迟很高，HPA 却几乎不扩。你怎么解释这两种情况？
```

这个问题能同时检查 HPA 的算法、指标口径、反馈延迟和边界。HPA 的核心动作是根据观测到的指标计算期望副本数，然后更新 workload 的 replicas。它不是请求级调度器，也不是容量魔法。它看到的是 metrics pipeline 里的过去数据，做的是离散的副本数调整。

CPU HPA 的基本公式可以口述成：期望副本数约等于当前副本数乘以“当前指标值 / 目标指标值”。如果当前平均 CPU utilization 是目标的两倍，期望副本数大致翻倍。这里的 utilization 很关键，它通常是 usage 除以 request，不是除以 limit，也不是除以宿主机 CPU 总量。request 配错，HPA 的判断就会错。

第一种“扩了但用户已经超时”，通常是反馈链太长。请求先积压，CPU 或自定义指标上升；metrics-server 或 adapter 采集、聚合、暴露；HPA controller 轮询计算；Deployment/ReplicaSet 创建新 Pod；scheduler 找节点；镜像拉取；容器启动；readiness 通过；Service endpoint 更新；流量才真正进新 Pod。每一步都有延迟。HPA 是反应式控制器，不是预知流量的系统。

第二种“延迟高但不扩”，常见原因是指标没有反映瓶颈。I/O 慢、数据库慢、锁竞争、外部依赖慢、队列阻塞、单分片热点、event loop 卡住，都可能让延迟高但 CPU 不高。如果 HPA 只看 CPU，它会觉得没有扩容必要。对队列消费、任务执行、网关或 workflow worker，队列深度、in-flight、处理耗时、lag、并发槽位利用率往往比 CPU 更贴近负载。

还要检查 HPA 对缺失指标和未 ready Pod 的处理。新 Pod 刚启动时，CPU 指标可能还没有，Pod 也可能未 ready。HPA 为了避免错误扩缩，会对缺失指标和 readiness 做保守处理。结果是你以为“新 Pod 已经创建”，HPA 实际还没有把它当成稳定容量。启动慢的服务会把这个问题放大。

最后看上限和下游。maxReplicas 太低，HPA 会进入 ScalingLimited；Cluster Autoscaler 没有及时补节点，新 Pod pending；数据库、缓存、对象存储或外部 API 已经是瓶颈，扩应用副本只会增加下游压力；Service 的负载均衡不均匀，热点分片仍然打到少数 Pod。HPA 只能调 replicas，不能自动修复这些结构问题。

结合 LogServe，我会说：如果 worker 用 HPA 扩副本，CPU 目标只能覆盖一部分场景。更合理的是把队列 backlog、ready task 数、任务执行耗时、worker 并发槽位、失败重试率和外部依赖延迟纳入判断。扩容前还要确认任务幂等、租约或 fencing、日志 replay 和去重语义，否则副本变多可能先放大并发问题。

## Q007. HPA 的一句话定义是否容易误导，误导点在哪里？

**回答：**

容易误导。常见一句话是：HPA 会根据 CPU 自动扩缩容 Pod。这句话对入门有用，但在面试和生产里不够精确。

第一个误导是把 HPA 限定成 CPU。HPA 可以使用 resource metrics、container resource metrics、custom metrics 和 external metrics。CPU 是最常见的入口，但不是唯一入口。对队列消费者、定时任务、网关、消息系统、workflow engine 来说，CPU 往往不是最好的负载信号。

第二个误导是把 CPU utilization 说成“CPU 使用率”。在 Kubernetes HPA 里，CPU utilization 通常是当前 CPU usage 相对于 Pod request 的百分比。一个容器 request 配得太低，稍微用一点 CPU 就显得 utilization 很高；request 配得太高，真实压力已经上来，utilization 仍然不高。HPA 的数学没有错，错的是口径。

第三个误导是“自动”两个字。HPA 自动执行控制循环，但它依赖 metrics-server、custom metrics adapter、external metrics adapter、API Server、controller manager、scheduler、kubelet、镜像仓库和 workload controller。任何一环延迟或失败，扩缩容都会慢或失效。

第四个误导是把扩容等同于可用容量增加。HPA 把 replicas 调大后，新 Pod 还要经过调度、拉镜像、启动、warmup、readiness、连接池预热、缓存加载、JIT 或 runtime 初始化。对于冷启动慢、缓存依赖重、连接池预热慢的服务，副本数增加和有效吞吐增加之间有明显间隔。

第五个误导是忽略缩容风险。HPA 不只扩容，也会缩容。缩容如果太激进，可能导致抖动、连接中断、队列重分配、缓存命中下降、leader 迁移或任务抢占。Kubernetes 默认会有 downscale stabilization 窗口，目的就是避免刚扩上去又马上缩下来。

第六个误导是以为 HPA 能处理所有瓶颈。HPA 不会自动扩数据库，不会自动增加 Kafka partition，不会自动消除锁竞争，不会修复单租户热点，不会让外部 API 变快，也不会保证节点有容量。它只是在 workload 副本数这个维度上做控制。

更准确的一句话是：HPA 是 Kubernetes 里根据资源、容器、自定义或外部指标，把 workload 副本数朝目标值调整的反应式控制器；它的效果取决于指标是否代表负载、Pod 是否能快速变成有效容量，以及下游和节点是否能承接新增副本。

## Q008. HPA 最常见的生产事故触发条件是什么？

**回答：**

HPA 最常见的事故不是“没开自动扩容”，而是“自动扩容基于错误信号做了正确动作”。控制器按指标工作，但指标口径、工作负载形态或系统瓶颈不匹配，最后扩容方向看起来合理，结果对用户没有帮助，甚至放大故障。

第一类是用 CPU 扩 I/O 型或队列型服务。请求都堵在数据库、缓存、磁盘、外部 API 或消息队列上，应用线程在等待，CPU 并不高。用户延迟很差，HPA 不扩；或者 HPA 扩了，更多 Pod 同时打下游，下游更慢。队列消费者常见的扩容信号应该是 backlog、lag、oldest message age、processing time 和可用并发槽位，而不是单纯 CPU。

第二类是 request 配错。CPU request 太小，HPA 很容易看到高 utilization，频繁扩容；request 太大，HPA 看不到压力。内存或 CPU request 还会影响调度，导致新 Pod 被放到不合适的节点。很多团队以为 HPA 目标 60% 是绝对 CPU，其实它是相对 request 的控制目标。

第三类是 metrics pipeline 不稳定。metrics-server 抓不到 kubelet 指标，自定义指标 adapter 延迟过大，Prometheus 查询太重，外部指标偶尔返回空值，HPA 会保守处理或不扩。业务侧看到的是“已经配置了 HPA”，控制面看到的是 ScalingActive false、FailedGetResourceMetric 或缺失指标。

第四类是启动慢和 readiness 配置不合理。新 Pod 需要几十秒到几分钟才能 ready，HPA 扩容速度追不上突刺。readiness 太宽松，Pod 还没预热好就进流量，用户继续超时；readiness 太严格，Pod 长期不 ready，HPA 以为容量补不上。启动 CPU spike 还可能误导 CPU HPA，让刚启动的 Pod 被当成高负载。

第五类是 maxReplicas、PDB、quota 和节点容量卡住。HPA 算出应该 100 个副本，但 maxReplicas 是 20；Namespace ResourceQuota 不允许再创建 Pod；PodDisruptionBudget 或 rollout 策略限制并发变更；节点资源不足导致 Pending；Cluster Autoscaler 扩节点又需要几分钟。HPA 期望值和真实容量之间有差距，必须看 HPA conditions 和事件。

第六类是多指标取最大值导致意外扩容。HPA 使用多个指标时，通常会按每个指标计算期望副本数，然后取最大的建议值。一个 noisy external metric、错误的 Prometheus 查询或某个低流量分片的尖峰，可能让 replicas 被拉得很高。反过来，如果某个指标缺失，缩容会被更保守地处理。

第七类是缩容抖动。流量波动、指标延迟和 Pod 预热叠加，HPA 先扩后缩，缩完又扩。结果连接池、缓存、任务分片、队列 ownership 不断变化，用户看到周期性延迟尖峰。downscale stabilization、行为策略、冷却窗口和最小副本数都要按 workload 调整。

LogServe 面试里可以把这个说成：worker 扩容前必须先确认任务是否可并行、是否有幂等 key、租约是否能防止双执行、日志 replay 是否可承受、metadata store 是否会成为瓶颈。HPA 只让 worker 数量变化，不能替代执行语义设计。

## Q009. HPA 的指标应该怎么设计才不会只看平均值？

**回答：**

HPA 指标设计要分成两层：控制器用来扩缩容的指标，以及验证扩缩容是否真正改善用户体验的指标。只看平均 CPU 或平均副本数，会把大部分事故都藏起来。

控制器层面，要看 currentReplicas、desiredReplicas、minReplicas、maxReplicas、lastScaleTime、HPA conditions、ScalingLimited、AbleToScale、ScalingActive。desiredReplicas 长期高于 currentReplicas，说明 workload controller、调度或 quota 可能卡住；desiredReplicas 长期等于 maxReplicas，说明 HPA 已经顶到上限；ScalingActive false 则说明指标可能不可用。

指标源层面，要把每个 metric 的 current value、target value、计算出的 desired replicas 单独暴露。CPU、memory、custom metric、external metric 不要只看 HPA 最终结果。多个指标取最大值时，必须知道是哪一个指标在驱动扩容，否则很难判断是业务压力还是指标噪声。

Pod 层面，要看 ready replicas、available replicas、pending pods、startup duration、readiness duration、image pull duration、container restart、OOMKilled、CPU throttling。HPA 把 replicas 调大不等于可用副本变多。扩容链路里任何一步慢，用户都要继续等。

负载层面，要按副本和分片看分布，不只看全局平均。每个 Pod 的 QPS、in-flight、队列长度、处理耗时、错误率、CPU、内存、连接数都要有分位数。一个服务平均每个 Pod 100 QPS，不代表没有某个 Pod 500 QPS、另一个 Pod 20 QPS。热点分片和负载均衡偏斜会让平均值非常好看。

用户体验层面，要看 p50、p95、p99、p999 延迟、超时率、错误率、重试率、排队时间、SLO burn rate。HPA 的目的不是让 CPU 回到目标值，而是让用户可见延迟和错误率回到可接受范围。CPU 降了但 p99 没降，说明瓶颈可能不在应用副本数。

队列和异步任务场景要看 backlog、oldest item age、consumer lag、drain rate、enqueue rate、success rate、retry rate、dead-letter rate。平均队列长度不够，要看最老任务年龄和分片级 lag。队列长度稳定但最老任务越来越老，说明有局部阻塞。

缩容层面，要看 scale-down recommendation、stabilization window、被终止 Pod 的 in-flight 请求、任务迁移耗时、连接 drain 是否生效。缩容事故常常不在 HPA 指标里显眼，但会在请求中断、任务重复执行和缓存 miss 里出现。

对 LogServe，我会把 HPA 指标和内部执行指标一起设计：ready task backlog、worker lease 获取失败、任务执行 p99、日志 append p99、replay recovery time、外部调用超时、worker CPU throttling、Pod startup-to-ready。这样能回答“扩容有没有把机制层面的瓶颈真正缓解”。

## Q010. HPA 的正确性边界和性能边界分别是什么？

**回答：**

HPA 的正确性边界很清楚：它只负责把 workload 的副本数调到某个建议值附近，不保证业务语义正确。副本数变多以后，请求会不会重复处理、任务会不会双执行、锁和租约会不会失效、日志顺序会不会乱、下游会不会被打爆，都不是 HPA 自动解决的。

它也不保证请求一定可用。HPA 依赖指标，指标是滞后的；依赖 Pod 启动，启动有延迟；依赖 scheduler，scheduler 可能没有节点；依赖 readiness，readiness 可能配置错误；依赖 Service 负载均衡，负载均衡可能不均匀。HPA 的控制回路正常，不代表用户请求已经恢复。

在多指标场景里，HPA 的正确性边界还包括指标语义。一个 external metric 代表队列总长度，另一个代表每个 Pod 平均 CPU，它们对应的期望副本数不一定线性可比。控制器只能按规则计算，不能判断这个指标是否真的代表用户痛点。指标设计错，HPA 会稳定地做错事。

性能边界首先是反应式延迟。HPA 通常不是毫秒级控制系统，从负载上升到指标采集、控制器计算、Pod ready、流量生效，可能是几十秒到几分钟。对突然流量尖峰，要靠预留容量、限流、排队、降级、预热、定时扩容或预测扩容配合，而不是指望 HPA 独立兜底。

第二个性能边界是水平扩展不一定线性。应用可能有单分片热点、全局锁、数据库瓶颈、对象存储限流、Kafka partition 数不足、metadata store 连接池不足、外部 API 配额限制。副本数翻倍，吞吐可能只提升一点，也可能让下游更差。

第三个性能边界是成本和稳定性。minReplicas 太低省钱但抗突刺差；maxReplicas 太高可能把下游压垮；缩容太快会抖动；扩容太慢会超时；指标窗口太短会追噪声，窗口太长会反应慢。这些都要根据 workload 调参数。

LogServe 里如果用 HPA，我会把正确性问题交给任务幂等、租约、fencing、append-only log、状态 replay 和去重；把性能问题交给队列指标、worker 并发、资源限制、冷启动和下游容量。HPA 只是其中一个反馈控制器，不能被描述成“开了自动扩容就可靠”。
## Q011. 面试官如果只问一个问题检验你是否理解 SLO，可能会问什么？

**回答：**

我会预期他问一个“指标通过但用户不满意”的题：

```text
一个服务对外宣称 99.9% 可用，月度报表也显示 SLO 达标，但大客户仍然投诉接口慢、偶发失败、批处理延迟很大。你怎么判断这个 SLO 是否设计错了？
```

这个问题比问“SLO 是什么”更能看出理解。SLO 不是在墙上写一个 99.9%，而是先定义 SLI，也就是用户关心的服务行为怎么被测量，再为这个 SLI 设置目标和时间窗口。没有好的 SLI，SLO 就只是一个漂亮数字。

我会先问“可用”的分子和分母是什么。分母是所有请求、付费用户请求、关键 API 请求，还是只统计成功进入应用的请求？分子是 HTTP 2xx、业务成功、在延迟阈值内成功，还是没有 5xx 就算成功？如果超时请求、连接失败、客户端取消、网关拒绝、队列任务过期没有进入分母，99.9% 就会虚高。

第二个问题是用户旅程是否被覆盖。一个系统可能 API health check 99.99% 正常，但用户真正关心的是提交任务、任务被调度、执行完成、结果可读、失败可恢复。只看单个接口的可用性，可能掩盖端到端工作流的失败。LogServe 这类系统尤其要把 workflow 成功率、日志持久化、任务恢复和结果可见性纳入讨论。

第三个问题是延迟阈值。平均延迟达标没有太大说服力。用户通常被 p95、p99、p999 和超时伤害。一个请求如果 10 秒后成功，HTTP 状态码是 200，但对用户而言可能已经失败。所以很多 SLO 应该定义成“在某个延迟阈值内成功的请求占比”，而不是简单的成功率。

第四个问题是分段。全局 99.9% 可以掩盖某个区域、某个租户、某个 API、某个分片、某个客户端版本的严重问题。请求量大的低价值流量会稀释小流量高价值用户的痛点。面试里如果能主动说出按 region、tenant、route、operation、priority 分段，就说明不只会背定义。

第五个问题是窗口。日 SLO、周 SLO、月 SLO、滚动 28 天窗口、自然月窗口，决策含义不同。一个服务月度达标，但连续两个小时不可用，仍然可能造成重大事故。SLO 需要搭配短窗口和长窗口 burn rate 告警，而不是月底看一次平均报表。

所以我会回答：判断 SLO 是否设计错，要回到用户可见行为、SLI 分母、成功定义、延迟阈值、分段维度、时间窗口和决策用途。一个达标但用户不满意的 SLO，大概率测的是系统方便测的东西，不是用户真正依赖的东西。

## Q012. SLO 的一句话定义是否容易误导，误导点在哪里？

**回答：**

容易误导。常见一句话是：SLO 是服务可用性的目标。这句话方向没错，但会把 SLO 讲窄，也容易把 SLO、SLI、SLA 混在一起。

第一个误导是把 SLO 等同于 uptime。很多服务最重要的不是进程活着，而是请求是否成功、响应是否够快、数据是否新鲜、任务是否按时完成、结果是否正确、恢复是否可预测。批处理系统、消息系统、工作流系统、存储系统的 SLO 可能围绕 freshness、durability、completion latency、availability、correctness proxy 设计，而不只是 uptime。

第二个误导是省略 SLI。SLO 是目标，SLI 是测量指标。没有明确 SLI 的 SLO不可执行。比如“99.9% 成功率”听起来清楚，但必须说明哪些事件算总数，哪些算好事件，哪些流量排除，采集点在哪里，重复请求怎么算，客户端超时怎么算，采样丢失怎么算。

第三个误导是把 SLO 和 SLA 混用。SLO 是内部或对外的服务目标，可以用来指导工程决策；SLA 是带有合同、赔付或正式承诺含义的协议。SLO 可以比 SLA 更严格，用来提前保护用户体验。把二者混用，会导致团队要么过度保守，要么把内部目标当成法律承诺。

第四个误导是认为 SLO 越高越好。100% 可靠性通常不是合理目标，因为成本极高，还会阻塞变更。一个合理 SLO 要在用户体验、工程成本、发布速度、风险承受能力之间取平衡。高于用户可感知需求太多，可能是浪费；低于用户依赖程度，可能是不负责任。

第五个误导是忽略时间窗口。99.9% over 30 days 和 99.9% over 1 hour 完全不同。前者允许短时间烧很多预算，后者更严格。SLO 必须带窗口，否则无法计算 error budget，也无法做 burn rate 告警。

第六个误导是把 SLO 当作监控图表。SLO 的价值不在于多一张仪表盘，而在于给发布、回滚、限流、降级、容量投入、技术债偿还提供决策依据。一个没有对应策略的 SLO，通常只是报表。

更准确的一句话是：SLO 是基于明确定义的 SLI，在特定时间窗口内对用户可见服务行为设定的目标，用来约束可靠性、成本和变更速度之间的取舍。

## Q013. SLO 最常见的生产事故触发条件是什么？

**回答：**

SLO 本身不会制造事故，真正的问题是 SLO 设计错了以后，团队被错误目标牵引。最常见的生产事故触发条件是“监控达标，用户受伤”。

第一类是 SLI 选错。服务只看 HTTP 5xx，但用户遇到的是超时、慢响应、业务校验失败、异步任务迟迟不完成、结果不可读。网关返回 200，业务状态却是 failed；任务提交成功，执行结果丢失；日志写入返回成功，但恢复时 replay 不完整。错误 SLI 会让真正故障从 SLO 体系里消失。

第二类是平均值掩盖尾部。平均延迟 100ms 看起来很好，但 p99 可能 5 秒。平均成功率达标，但某个高价值租户连续失败。SLO 如果只用平均延迟或全局平均成功率，事故就会在局部和尾部积累，直到用户投诉才暴露。

第三类是分母被人为缩小。只统计到达应用的请求，不统计被负载均衡器拒绝的请求；只统计服务端认为完成的请求，不统计客户端超时；只统计健康检查，不统计真实用户路径；只统计重试后的最终成功，不统计用户经历的失败和延迟。这样 SLO 会长期漂亮，但没有保护意义。

第四类是排除规则过宽。维护窗口、依赖故障、客户端错误、低流量租户、某些区域、某些 API 被大量排除，最后 SLO 只剩容易达标的流量。排除规则不是不能有，但必须少、明确、可审计，并且不能把用户真实痛点洗掉。

第五类是告警策略只盯瞬时错误率或月度报表。瞬时错误率容易噪声大，月度报表发现问题太晚。更常见的做法是用多窗口、多燃烧率告警：短窗口抓快烧预算的事故，长窗口抓持续慢性退化。否则团队可能在预算快烧完时才知道。

第六类是 SLO 没有对应行动。预算烧穿后仍然照常发布，重复事故没有拉出技术债，容量不足没有投资，慢接口没有降级策略。SLO 如果不改变发布和运维行为，就不会真正降低风险。

第七类是低流量和分段流量处理不好。低 QPS 服务一个失败就可能让短窗口比例剧烈波动；大客户专线、内部关键任务、跨区域流量又可能被全局流量稀释。SLO 设计要处理低流量噪声，也要保护关键用户分段。

LogServe 里可以举例：只看进程存活和 HTTP 200 不够，要看任务提交到完成的端到端成功率、日志 append 成功率、恢复后任务状态一致性、结果可读性、调度延迟和 replay 延迟。如果这些 SLI 不清楚，SLO 再好看也不能证明系统可靠。

## Q014. SLO 的指标应该怎么设计才不会只看平均值？

**回答：**

SLO 指标设计的第一原则是用“好事件 / 总事件”的比例描述用户体验，而不是用一堆平均值描述系统资源。好事件必须同时满足成功语义和时间阈值，分母必须覆盖用户真正依赖的请求或任务。

请求型服务可以设计 availability SLI：好事件是在规定时间内返回正确成功响应的请求，总事件是所有有效用户请求。这里要明确 4xx 是否计入、客户端取消是否计入、网关超时是否计入、重试如何计入。很多系统会把“成功且延迟小于阈值”作为好事件，避免慢成功污染体验。

延迟型 SLO 不建议只看平均延迟。可以定义“99% 的读请求在 200ms 内完成”“95% 的写请求在 500ms 内完成”，或者直接用阈值型好事件比例。分位数可以辅助诊断，但 SLO 计算本身用好事件比例更容易和 error budget 对接。

异步任务型系统要看 completion SLI。比如“任务在 5 分钟内成功完成的比例”“最老待处理任务年龄低于阈值的时间占比”“workflow 从 submitted 到 terminal 状态的 p99”。只统计提交接口成功，会漏掉执行、重试、恢复和结果写回。

数据型系统要看 freshness、durability 和 correctness proxy。物化视图多久更新一次，日志是否持久化，checkpoint 后是否可恢复，读到的数据是否不超过允许陈旧度，恢复后状态是否和日志一致。这些指标很难只用 HTTP status 表达。

分段维度要提前设计。region、tenant、route、operation、priority、client version、dependency、shard、Pod、node 都可能是切分维度。不是每个维度都要单独承诺 SLO，但至少在事故分析和告警里能看到，否则全局平均会稀释局部故障。

时间窗口要同时有长短两类。长窗口用于评估目标是否达成和 error budget 是否健康；短窗口用于发现快速燃烧预算的事故。比如 5 分钟、1 小时、6 小时、3 天、28 天窗口各有用途。只看 30 天平均，会让短时严重事故在数学上显得很小。

还要监控测量质量。SLI 数据丢失、采样率变化、日志延迟、指标重复、客户端埋点版本不一致、Prometheus 查询超时，都会让 SLO 失真。SLO 体系需要有“指标本身是否可信”的元监控。

对 LogServe，我会设计几类 SLO 候选：任务提交成功率、任务在阈值内进入执行的比例、任务在阈值内完成的比例、append-only log 写入成功且 fsync/flush 在阈值内的比例、恢复后 replay 完整的比例、结果对象可读取比例。每个 SLO 都要说明分母、好事件、窗口、排除条件和对应行动。

## Q015. SLO 的正确性边界和性能边界分别是什么？

**回答：**

SLO 的正确性边界是：它是一个可靠性目标和决策工具，不是系统正确性的数学证明。一个系统 SLO 达标，仍然可能存在数据竞态、重复执行、权限漏洞、偶发一致性问题或某个小租户的严重故障。SLO 只能说明在定义好的 SLI 和窗口内，好事件比例达到了目标。

它也不等于 SLA。SLA 有合同、赔付和外部承诺含义；SLO 更常用来指导内部工程取舍。内部 SLO 可以比 SLA 更严格，用来提前发现风险。面试里如果把 SLO 直接说成“对客户赔付的指标”，会被追问 SLA 区别。

SLO 的正确性还受 SLI 质量限制。分母错、采集点错、排除规则过宽、延迟阈值错、时间窗口错，SLO 就会稳定地给出错误结论。SLO 体系的边界不是数字本身，而是“这个数字是否真实代表用户痛点”。

性能边界是严格 SLO 会消耗成本。为了把 99.9% 提到 99.99%，可能需要更多冗余、更快故障切换、更强隔离、更多容量、更少发布、更复杂的数据复制。可靠性不是免费提高的，SLO 必须和用户价值、成本和团队能力匹配。

另一个性能边界是 SLO 不能替代实时保护。SLO 适合管理可靠性目标和预算，但请求进来时仍然需要限流、超时、熔断、降级、排队、backpressure 和容量保护。等 SLO 月度报表变红再处理，已经晚了。

SLO 还可能掩盖分段用户。全局 SLO 达标，不代表每个地区、每个租户、每条 API 都达标。高价值低流量用户尤其容易被总量稀释。成熟系统通常把全局 SLO、关键路径 SLO 和关键客户分段监控结合起来。

对 LogServe，我会谨慎表达：项目可以提出机制验证层面的 SLO，例如日志写入、任务完成、恢复耗时和结果读取；但如果没有真实多租户流量、值班制度、告警路由、错误预算政策和长期生产数据，就不能声称已经建立成熟 SRE SLO 体系。这个边界说清楚，反而更可信。
## Q016. 面试官如果只问一个问题检验你是否理解 error budget，可能会问什么？

**回答：**

我会预期他问一个算账加决策的题：

```text
一个 API 的 SLO 是 30 天内 99.9% 的有效请求成功且延迟低于 300ms。这个月预计有 1000 万次有效请求。某次故障造成 2 万次 bad events。你怎么计算 error budget？故障后团队应该做什么？
```

这个问题能检查你是不是只会背“error budget 是 100% 减 SLO”。先算总预算：99.9% SLO 意味着允许 0.1% 的 bad events。1000 万次请求的 0.1% 是 1 万次 bad events。一次故障造成 2 万次 bad events，已经烧掉 200% 月度预算，也就是预算被透支。

但真正关键不是算术，而是决策。预算烧穿后，团队不能只说“这个月红了”。应该按事先定义的 error budget policy 行动：暂停或限制高风险发布，优先修复导致预算燃烧的根因，补监控和回滚能力，降低变更频率，做容量或隔离投资。预算健康时，可以接受合理的发布和实验风险；预算透支时，可靠性工作优先。

还要看 burn rate。2 万次 bad events 如果在 10 分钟内发生，是快速燃烧，说明需要事故级响应；如果在 20 天内慢慢积累，是慢性退化，可能需要容量治理、性能优化或依赖治理。总预算一样，处理方式不同。

这个题还会追问分母。1000 万次有效请求是按服务端收到的请求算，还是用户发起的请求算？重试后的成功是否掩盖了第一次失败？超时请求是否进入 bad events？被网关拒绝的请求算不算？如果分母和 bad event 定义不清楚，预算计算没有意义。

也要追问窗口。自然月窗口和滚动 30 天窗口的行动不同。自然月窗口容易在月初发生事故后“等下个月刷新”，滚动窗口更能持续表达近期可靠性状态。低流量服务按请求数算预算会很跳，可能需要按时间可用性、任务数或更长窗口处理。

结合 LogServe，我会这么落地：如果定义“任务在 5 分钟内成功完成”的 SLO，error budget 就是允许超时或失败的任务比例。一次调度 bug、日志写入抖动或外部 LLM 依赖超时，会消耗预算。预算烧穿后，应该优先修复任务幂等、重试风暴、backpressure、恢复路径和观测，而不是继续加功能。

## Q017. error budget 的一句话定义是否容易误导，误导点在哪里？

**回答：**

容易误导。常见一句话是：error budget 是 SLO 允许的错误额度。这句话容易让人误以为团队“被允许制造这么多错误”。更准确的理解是：error budget 是用户可接受的不可靠性余量，用来在可靠性、发布速度和工程成本之间做显式取舍。

第一个误导是把它当成道德许可。error budget 不是说“还有预算，所以可以随便上线”。它表达的是当前可靠性是否还有风险空间。如果预算充足，可以进行正常发布和实验；如果预算消耗异常，要收紧风险。它是风险仪表，不是出错许可证。

第二个误导是忽略它依赖 SLO。没有好的 SLO，就没有好的 error budget。SLO 的 SLI 分母错、好事件定义错、窗口错、排除规则错，预算也会错。很多团队争论预算政策，其实根因是前面的 SLO 没定义好。

第三个误导是只看剩余百分比，不看燃烧速度。预算还剩 80%，但过去一小时按 50 倍速度燃烧，服务很快会烧穿；预算只剩 10%，但过去一周稳定且没有继续燃烧，处理优先级可能不同。burn rate 比静态剩余值更适合告警。

第四个误导是把 error budget 等同于 downtime。对请求型服务，预算可以用 bad requests 算；对延迟 SLO，慢请求也是 bad event；对数据 freshness，数据过旧的时间或事件是 bad；对任务系统，超时完成和失败任务是 bad。并不是所有预算都能简单换算成分钟停机。

第五个误导是忽略政策。error budget 的价值来自 policy：预算到什么程度冻结发布，什么发布可以例外，谁有决策权，预算恢复条件是什么，事故复盘如何影响技术债优先级。没有 policy，预算只是图表。

第六个误导是忽略分段。全局预算还有很多，不代表关键租户、关键 API、某个区域还有预算。高价值路径的预算可能已经烧完，全局却看起来健康。预算要能支持业务优先级，而不是只服务平均值。

更准确的一句话是：error budget 是由 SLO 推导出的可承受 bad events 或不可靠性余量，配合 burn rate 和预算政策，用来决定什么时候加速变更、什么时候优先可靠性。

## Q018. error budget 最常见的生产事故触发条件是什么？

**回答：**

error budget 相关事故通常不是预算本身导致的，而是团队没有让预算真正参与决策。最常见触发条件是：预算已经快速燃烧，发布和变更还在照常推进，直到用户投诉或 SLA 风险出现。

第一类是预算烧穿后没有动作。SLO 报表显示连续几周预算不足，团队仍然保持原发布节奏，甚至在不稳定服务上继续做高风险变更。最后一次小故障把剩余预算全部打穿，团队才发现早就没有风险空间。

第二类是只看月度预算，不看短窗口 burn rate。一个 30 天 SLO 允许一定坏事件，但如果 1 小时内烧掉 20% 月度预算，这是明显事故。没有多窗口 burn rate 告警，团队会在数学上觉得“月度还行”，实际正在快速伤害用户。

第三类是重试和降级把 bad events 洗掉。用户第一次请求超时，客户端重试后成功；服务端只记录最终成功，error budget 没有减少。或者系统降级返回空结果、旧数据、默认值，HTTP 仍然 200。预算没有反映用户真实损失，决策就会错误。

第四类是排除规则被滥用。把依赖故障、云厂商故障、某个区域故障、维护窗口、客户端问题都排除，预算自然漂亮。但用户并不关心责任边界，他们只关心服务是否可用。排除规则可以用于复盘归因，但不能轻易把用户痛点从可靠性目标里删除。

第五类是预算没有按关键路径分段。低价值高流量接口很稳定，关键支付、登录、任务提交、结果读取接口不稳定；全局预算仍然健康，业务损失却很大。错误预算如果不分路径、不分租户、不分优先级，就会把风险藏起来。

第六类是把预算当成发布冻结开关，过于机械。预算稍微变红就冻结所有发布，连可靠性修复也被卡住；或者预算健康就允许任何变更。成熟做法通常会区分风险级别：低风险修复、回滚、观测增强可以继续，高风险功能和大规模迁移需要收紧。

第七类是低流量服务预算波动。一个月只有几千次请求的关键内部服务，几个失败就能烧掉大量预算。团队如果机械套用高流量 API 的阈值，会得到很多噪声。低流量场景需要结合事件数、时间窗口和人工判断。

LogServe 面试里可以说：如果系统的错误预算和任务成功 SLO 绑定，最常见事故是任务重试风暴和恢复失败被最终成功掩盖。用户等待很久才拿到结果，预算却没有消耗。要把超时、重复执行、恢复耗时和结果不可读纳入 bad events，预算才有治理意义。

## Q019. error budget 的指标应该怎么设计才不会只看平均值？

**回答：**

error budget 指标不能只显示“本月剩余 80%”。这个数字太静态，不能告诉你预算是稳定消耗、快速燃烧，还是被某个租户、区域、接口集中消耗。指标要围绕预算剩余、燃烧速度、归因和行动设计。

第一组是预算基本盘。对每个 SLO 显示总预算、已消耗预算、剩余预算、剩余天数、当前窗口内 good events、bad events、total events。请求型 SLO 用事件数，时间型 SLO 用时间或探测窗口，任务型 SLO 用任务数。单位要明确，不能把请求、分钟和任务混在一张图里。

第二组是 burn rate。至少要有短窗口和长窗口，例如 5 分钟、1 小时、6 小时、1 天、3 天、28 天。短窗口高 burn rate 用来抓快速事故，长窗口中等 burn rate 用来抓慢性退化。告警不应该只看错误率，而要看它会不会按当前速度快速烧完预算。

第三组是预测耗尽时间。按当前 burn rate 估算预算还可以撑多久。剩余 50% 听起来很多，但如果 forecast 显示 3 小时后耗尽，就应该立刻处理。剩余 5% 但 burn rate 接近 0，则更像历史事故后的恢复状态。

第四组是归因维度。按 service、route、operation、region、tenant、shard、dependency、release version、client version、Pod、node 拆分预算消耗。目标不是做无限维度，而是在预算异常时能快速回答“是谁在烧预算”。

第五组是发布关联。把预算燃烧和 deploy、config change、feature flag、schema migration、依赖升级、流量切换、HPA scale event、节点扩缩容放在同一时间轴。很多事故不是没有指标，而是指标和变更记录分散，复盘时很难证明因果。

第六组是排除和修正审计。被排除的 bad events 数量、原因、审批人、时间范围要能查。否则预算很容易被人为美化。对用户体验有影响但被排除的事件，也应该在复盘里保留。

第七组是政策状态。预算健康、风险观察、发布收紧、冻结高风险变更、只允许可靠性修复等状态应该可见。error budget 的最终目的是改变行动，所以指标面板要能告诉团队现在应该怎么做，而不只是展示百分比。

LogServe 可以把这些指标映射到任务维度：任务完成预算剩余、调度延迟 burn rate、日志写入 bad events、恢复失败预算、外部依赖导致的预算消耗、某个 worker 版本引入的预算燃烧。这样面试官会看到你理解的不只是 SRE 名词，而是如何把它变成工程控制面。

## Q020. error budget 的正确性边界和性能边界分别是什么？

**回答：**

error budget 的正确性边界是：它只能基于已经定义好的 SLO 和 SLI 工作。SLO 错了，预算就错；SLI 数据不可信，预算也不可信。它不能证明系统没有 bug，不能证明数据一定正确，也不能替代一致性测试、安全审计、容量压测和故障演练。

它也不能替代事故响应。预算燃烧是信号，不是自动修复。真正的恢复仍然要靠回滚、限流、降级、扩容、故障切换、依赖隔离、数据修复和人工指挥。error budget 可以告诉你风险是否超标，但不会替你处理正在失败的请求。

它不是免责机制。预算里还有空间，不代表可以接受低质量变更；预算烧穿，也不代表所有变更都应该禁止。可靠性修复、观测增强、回滚工具、容量修复通常应该优先通过。error budget policy 要服务用户体验和工程风险，而不是变成僵硬流程。

性能边界主要体现在成本和速度。严格 SLO 会让 error budget 很小，团队发布速度下降，容量和冗余成本上升；宽松 SLO 会让团队变化更快，但用户可能承受更多失败。预算政策的核心是承认可靠性、成本和迭代速度之间有真实取舍。

第二个性能边界是低流量和长尾分段。低流量服务的预算以事件数计算时波动很大，长尾租户的痛点又容易被全局预算掩盖。成熟的预算设计要结合绝对事件数、用户影响、业务优先级和人工判断，不能只让一个百分比决定一切。

第三个性能边界是依赖归因。下游故障、云平台故障、第三方 API 故障是否消耗本服务预算，要看用户承诺和系统设计。如果用户体验由你负责，那么即使根因在下游，也不能完全从可靠性目标里消失。内部归因可以分摊责任，但用户可见预算要诚实。

对 LogServe，我会把边界说得保守：项目可以展示如何为任务执行、日志持久化、恢复和结果读取设计 error budget，并把预算烧穿与发布策略、可靠性修复挂钩；但没有长期生产流量和值班治理，就不能声称已经有完整 error budget 文化。这样回答既有方法，也没有超出项目实际。
