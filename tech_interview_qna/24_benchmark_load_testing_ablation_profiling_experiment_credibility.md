# 24. Benchmark、压测、ablation、profiling 与实验可信度

这一章关注性能实验里最容易被问细的地方：microbenchmark 和 end-to-end benchmark 分别能证明什么，为什么 warm-up 不能省，为什么一次 benchmark 不能直接当结论，怎么控制变量，以及报告硬件、内核、运行时和配置版本时应该写到什么程度。

下面的回答主要参考 Go `testing`/`benchstat` 官方文档、Google Benchmark 用户手册、OpenJDK JMH 示例、pyperf 文档、Linux kernel CPU frequency 文档、Brendan Gregg 的性能分析资料，以及 SPEC 的 benchmark 运行规则。面试时不要把 benchmark 说成“跑一下看快不快”。更准确地说，它是一套有假设、有边界、有控制变量、有重复测量、有统计解释的实验。

## Q001. microbenchmark 和 end-to-end benchmark 的区别是什么？

microbenchmark 和 end-to-end benchmark 的区别，先看测量对象。microbenchmark 测的是一个很小的代码单元，可能是一个函数、一个数据结构操作、一段序列化逻辑、一个锁实现、一次 map 查找、一条压缩路径。它的问题通常是：

```text
这个函数每次调用多少 ns？
每次调用分配多少 B？
每次调用有多少 allocs？
这个局部实现 A 是否比实现 B 快？
某个锁、缓存、编码、hash、batch 逻辑有没有明显开销？
```

end-to-end benchmark 测的是完整用户路径或完整系统路径。它可能从客户端请求开始，穿过网关、服务、数据库、缓存、消息队列、对象存储、worker，再到结果返回。它的问题更接近线上：

```text
用户一次请求 p50/p95/p99 是多少？
系统最大稳定 QPS 是多少？
错误率在什么负载下开始上升？
队列 backlog 和 tail latency 是否同时增长？
一次 workflow 从提交到完成要多久？
```

microbenchmark 的优点是隔离。它可以把一个局部因素放大到足够清楚，便于做实现选择。比如 Go 的 `testing` 包会反复执行 benchmark body，报告 `ns/op`；加上 `-benchmem` 后还能看 `B/op` 和 `allocs/op`。如果我要比较两种 record framing、两种 checksum、两种对象池策略，microbenchmark 很合适。它的反馈快，噪声相对可控，也适合放进性能回归检查。

但隔离也是它的缺点。microbenchmark 很容易测到一个“离开真实系统就不存在”的结果。CPU cache、branch predictor、GC、JIT profile、锁竞争、真实输入分布、网络、磁盘、下游延迟、调度器干扰，都可能在小测试里被简化掉。OpenJDK JMH 示例里专门有 dead code、constant folding、blackhole、forking、false sharing 这些样例，就是提醒 JVM 上的 microbenchmark 很容易被编译器和运行时优化骗到。Go 里也类似：如果结果没有被使用，编译器可能消掉代码；如果输入太理想，分支预测和 cache 行为会比线上好很多。

end-to-end benchmark 的优点是真实。它能覆盖多个组件之间的交互成本：RPC 序列化、连接池、调度、锁、队列、数据库、缓存、磁盘 flush、对象存储、worker poll、重试、超时、限流。很多线上问题只在端到端路径里出现。比如单独测 `appendLog()` 很快，但端到端 workflow p99 很慢，原因可能是 worker queue wait、结果对象写入、Python executor 冷启动或下游 mock LLM 排队。

它的缺点是归因困难。端到端结果变慢了 20%，你不知道是代码变慢、GC 变多、数据库缓存没热、网络抖动、机器频率变化、压测客户端不够强，还是某个配置变了。end-to-end benchmark 很适合回答“用户会不会变慢”，不适合直接回答“哪一行代码导致慢”。所以它通常要配合 profiling、trace、日志、资源指标和 ablation。

两者不是替代关系。比较合理的实验链路是：

```text
microbenchmark：确认局部改动确实降低了 ns/op、B/op、allocs/op。
component benchmark：确认组件级行为没有被真实输入、并发和资源竞争抵消。
end-to-end benchmark：确认用户路径、吞吐、p99、错误率和资源消耗真的改善。
profiling/trace：解释改善或回退来自哪里。
```

在 LogServe 里，microbenchmark 可以测 shared log append 编码、CRC、索引查找、actor command dispatch、result reference 构造、checkpoint cache 命中路径。end-to-end benchmark 则应该测 SDK submit -> control 调度 -> worker 执行 -> actor/LLM -> result store -> 状态查询这一整条路径。前者说明局部机制有没有开销，后者说明 workflow 用户体验和系统容量。

面试里我会这样回答：

```text
microbenchmark 测很小的代码单元，重点是隔离局部成本，例如 ns/op、B/op、allocs/op，适合比较算法、数据结构、序列化、锁和缓存策略。end-to-end benchmark 测完整用户路径或系统路径，重点是吞吐、p95/p99、错误率、资源饱和和真实交互成本。microbenchmark 反馈快、归因清楚，但容易脱离真实负载；end-to-end 更接近线上，但噪声大、归因难。我的做法是两者都跑：先用 microbenchmark 证明局部改动有收益，再用端到端 benchmark 证明收益没有被网络、队列、GC、IO、重试和调度成本吃掉。
```

## Q002. benchmark 中为什么需要 warm-up？

warm-up 的目的，是让被测系统进入接近稳定的运行状态，再开始采集用于报告的数据。很多系统刚启动时和运行一段时间后不是同一个状态。缓存是冷的，连接池还没建好，JIT 还没编译热点代码，GC heap 还没达到稳定形状，CPU 频率还没升上去，磁盘 page cache 还没命中，服务端线程池还没扩张，数据库 buffer pool 还没热。如果把这些启动阶段混进结果，benchmark 测到的就可能是冷启动成本，而不是稳态性能。

warm-up 最常见的原因是缓存。CPU cache、TLB、page cache、应用缓存、对象池、连接池、数据库 buffer pool 都会影响结果。第一次请求可能要加载配置、建立 TLS、打开文件、初始化正则、生成 JIT profile、创建 goroutine 或线程。后面的请求只走热路径。你要先问清楚实验目标：如果你测的是冷启动，冷态数据就应该保留；如果你测的是线上稳态吞吐，就应该把 warm-up 阶段单独处理。

托管运行时更需要 warm-up。JVM、PyPy、JavaScript 引擎、.NET 这类运行时有解释执行、采样 profile、分层编译、内联、去优化、GC 调整等过程。JMH 存在的一个重要原因，就是 JVM microbenchmark 非常容易被这些机制影响。pyperf 文档也明确提到 worker 会先 warm up，warmup 的结果不进入最终结果；同时它也提醒，有些 benchmark 并不会稳定，随便跳过几个 warm-up 值不一定就可靠。

Go 没有 JVM 那种 JIT，但仍然有 warm-up 问题。Go benchmark 本身会自动调整 `b.N` 或使用 `b.Loop()` 让测试体运行足够长；如果有昂贵 setup，要用 `b.ResetTimer()` 或让 setup 放在 `b.Loop()` 之外。可是系统层面的 warm-up 仍然存在：page cache、连接池、GC heap、CPU frequency、runtime scheduler、数据库缓存、对象池。端到端压测里，Go 服务同样需要预热。

Google Benchmark 也把 warm-up 做成了显式控制项。它的 `--benchmark_min_warmup_time` 会让 benchmark 在指定时间内先跑一段，期间结果被丢弃，不进入报告。这个设计背后的意思很直接：有些代码需要先填充缓存、准备状态或进入稳定路径，再看 steady-state。

warm-up 不能机械化。不能一句“预热 30 秒”就结束。要看指标是否稳定：吞吐是否稳定，p99 是否稳定，错误率是否为零，CPU 频率是否稳定，GC 周期是否进入正常节奏，连接数是否达到目标，队列是否没有积压。对于端到端 benchmark，还要确认压测客户端本身也热了。客户端 DNS、TLS、连接池、线程池如果没预热，测出来的是客户端冷态。

也不能把 warm-up 当成美化结果的工具。有些系统线上就是频繁冷启动，比如 serverless、短生命周期 worker、批处理 job、模型加载、容器自动扩缩容。此时冷启动是业务体验的一部分，不能全部丢掉。更好的报告方式是分开写：

```text
cold start latency：第一次请求、首次模型加载、首次连接建立。
warm steady-state latency：缓存和连接池稳定后的请求。
re-warm latency：重启、failover、cache flush 后恢复到稳定性能需要多久。
```

在 LogServe 里，warm-up 要特别注意几类东西：Python executor 进程池是否已经启动，workflow worker 是否已经 poll，checkpoint cache 是否有数据，LLM mock 或模型服务是否已加载，shared log 文件和 page cache 是否热，dashboard/metrics exporter 是否已经开始采集。测冷启动时要保留这些成本，测稳态时要先预热并说明预热条件。

面试里可以这样回答：

```text
benchmark 需要 warm-up，是因为系统刚启动时和稳态时不是同一个状态。缓存、page cache、连接池、线程池、GC heap、CPU 频率、JIT 编译、数据库 buffer pool、对象池都会变化。warm-up 的结果通常不进入稳态报告，否则会把冷启动成本混进 steady-state 性能。Go 虽然没有 JIT，也仍然会受 page cache、连接池、GC 和 CPU frequency 影响；JVM、PyPy 这类运行时更明显。但 warm-up 不能用来掩盖真实冷启动。如果业务关心冷启动，就要单独报告 cold start、warm steady-state 和恢复到稳态的时间。
```

## Q003. 为什么单次 benchmark 结果不可信？

单次 benchmark 不可信，原因很简单：你不知道它是系统真实差异，还是这一次环境噪声。性能测量不是纯函数。即使代码完全不变，结果也会被 CPU 频率、调度器、后台进程、GC、ASLR、内存布局、cache 状态、page fault、磁盘 flush、网络抖动、容器邻居、温度、风扇策略、电源模式影响。一次结果只给你一个点，没有方差，也没有置信区间。

Google Benchmark 的用户手册说得很直接：默认每个 benchmark 只跑一次，但 benchmark 经常有噪声，单次结果可能不代表整体行为；可以用 `--benchmark_repetitions` 重复运行，并报告 mean、median、standard deviation 和 coefficient of variation。Go 的 `benchstat` 也要求每个 benchmark 通常至少跑 10 次，才能做更稳健的 A/B 比较，并给出中位数、置信区间和显著性判断。

单次结果最容易制造几类误判。

第一，把偶然快当成优化。比如某次运行刚好 CPU 频率高、机器空闲、cache 热、GC 没发生，你看到 `ns/op` 降了 5%，就以为优化成功。重复 10 次后可能发现差异在噪声里。

第二，把偶然慢当成回退。比如某次运行遇到后台杀毒、Windows 更新、容器邻居抢 CPU、磁盘 flush、thermal throttling，你看到 p99 高了很多，就以为代码坏了。单次结果没有办法区分这种情况。

第三，看不到分布。性能数据通常不是正态分布。尾部可能很长，偶发 outlier 可能来自 GC、page fault、锁竞争或调度。只报一次平均值或一次吞吐，无法解释稳定性。压测里更要看 p50、p95、p99、max、错误率、超时、吞吐曲线和资源曲线。

第四，无法判断变化是否有工程意义。统计显著和工程显著也不是一回事。样本很多时，1% 的差异可能统计显著，但如果这个路径不在热路径上，工程上不值得改。反过来，样本少时，15% 的差异可能看起来很大，但噪声也很大，需要更多重复和更好的控制变量。

第五，单次 benchmark 很难发现漂移。比如第一次运行快，第二次慢，第三次更慢，可能是内存泄漏、缓存污染、JIT deopt、GC heap 增长、队列积压、数据库 buffer 被挤掉。只跑一次根本看不到趋势。

可靠做法是重复、随机化、记录环境、用统计工具比较。Go 项目里常见流程是：

```text
go test -run='^$' -bench=. -benchmem -count=10 > old.txt
go test -run='^$' -bench=. -benchmem -count=10 > new.txt
benchstat old.txt new.txt
```

如果是端到端压测，我会跑多轮独立实验，而不是只跑一个 5 分钟窗口。每轮保留原始结果，报告中位数、p95/p99、错误率、吞吐、资源使用、置信区间或至少标准差。还要说明是否丢弃 warm-up，是否有 outlier，outlier 是保留还是解释后剔除。不能只挑最好的一次。

在 LogServe 里，如果我说“新 shared log append 优化让 workflow p99 下降 20%”，至少要有旧版和新版多轮结果，最好还要有 `benchstat` 的 microbenchmark 对比、端到端压测对比、profile 或 trace 解释。只有一张图、一行 QPS、一次运行，不足以支撑结论。

面试里可以这样回答：

```text
单次 benchmark 不可信，因为性能结果会受 CPU 频率、调度器、GC、ASLR、cache、page fault、后台进程、磁盘和网络抖动影响。一次结果没有方差，也没有置信区间，分不清真实优化和偶然噪声。Go benchstat 通常建议每个 benchmark 至少跑多次，例如 old/new 各 `-count=10` 后比较中位数和置信区间；Google Benchmark 也支持 repetitions 并报告均值、标准差、变异系数。我的原则是保留原始多轮数据，报告中位数、波动、样本数和统计判断，不拿最好的一次当结论。
```

## Q004. 如何控制 benchmark 的变量？

控制变量的目标，是让结果差异尽量只来自你要研究的因素。你要比较一个优化，就只能让“优化开关”变化，其他尽量不变。否则 benchmark 变快了，你不知道是代码变快，还是输入变了、机器变了、配置变了、缓存状态变了、压测客户端变了。

第一类变量是代码变量。比较 old/new 时，要固定分支、commit、编译参数、依赖版本、生成代码、feature flag。不能 old 版本开了日志，new 版本关了日志；不能 old 用 race build，new 用 release build；不能 old/new 的依赖版本顺便升级。Go 里要记录 `go version`、`GOOS`、`GOARCH`、`GOAMD64`、`GOMAXPROCS`、build tags、`-gcflags`、`-ldflags`。Java 要记录 JDK 版本、JVM flags、GC、JIT 相关参数。C/C++ 要记录 compiler、optimization flags、libc、linked libraries。

第二类变量是输入变量。microbenchmark 要固定输入规模、数据分布、随机种子、命中率、冷热比例、对象大小、错误比例。只测全命中缓存，会高估性能；只测小 payload，会漏掉内存分配和序列化成本；只测顺序 key，会隐藏 hash 冲突、分支和 cache 行为。端到端压测要固定请求 mix、payload、用户分布、think time、到达率、并发数、超时、重试策略。

第三类变量是环境变量。硬件型号、CPU 核数、频率策略、NUMA、内存、磁盘、文件系统、内核版本、容器限制、cgroup、网络拓扑、数据库部署，都可能影响结果。Linux CPU frequency scaling 会根据 governor 和负载调整频率；如果一次跑在 powersave，一次跑在 performance，结果没法比。云机器还要注意实例类型、宿主机噪声、同租户干扰、突发积分。

第四类变量是运行状态。缓存热不热，连接池是否预热，数据库 buffer pool 是否热，page cache 是否清理或保留，GC heap 是否稳定，队列是否为空，后台 compaction 是否进行，日志文件是否已经增长到某个大小。这些都要定义。不是所有实验都要清缓存，有些实验要测冷态，有些要测热态。关键是写清楚。

第五类变量是观测开销。profiling、trace、debug log、metrics、race detector、sanitizer、pprof、eBPF、perf events 都会影响被测系统。测真实性能时，要说明是否开启；定位原因时可以开启 profile，但不要把 profile 模式下的吞吐当作正常模式结论。Google Benchmark 也提供 profiler manager 和 perf counters 相关机制，但这些都属于额外观测，不是免费的。

第六类变量是压测工具本身。压测客户端要足够强，网络不要先瓶颈，连接数和端口范围要够，客户端 CPU 不能满，生成 payload 不应成为瓶颈。否则你测到的是压测机的上限。还要避免 coordinated omission：如果客户端等响应后再发下一个请求，就会低估系统卡顿期间的真实等待。

控制变量不是把世界变成真空。线上系统本来就有噪声。合理做法是分层：

```text
microbenchmark：强控制变量，尽量隔离单个因素。
component benchmark：保留真实并发和输入分布，但控制外部依赖。
end-to-end benchmark：保留完整链路，记录所有不可控因素。
production shadow/canary：接受真实噪声，用更长窗口和更多样本判断。
```

如果要做 ablation，变量控制更重要。ablation 是逐个关闭或替换组件，看整体结果怎么变。一次只动一个因素：关 cache、关 batching、关 compression、关 prefetch、关 tracing、换调度策略。如果一次关了三个功能，结果变快也不知道是谁的贡献。

在 LogServe 里，我会这样控制变量：固定 workflow DAG、payload 大小、任务数量、worker 数、Python executor pool、mock LLM 延迟分布、checkpoint cache 初始状态、shared log 目录、fsync 策略、GOMAXPROCS、Go 版本、机器电源模式。比较某个优化时，只改那一个 commit 或 feature flag。每轮开始前确认队列为空、日志目录状态一致、数据库或对象存储状态一致。

面试里可以这样回答：

```text
控制 benchmark 变量，就是让结果差异尽量只来自被研究因素。代码侧固定 commit、依赖、编译参数、运行时参数和 feature flag；输入侧固定数据规模、分布、随机种子、请求 mix、payload、并发、到达率、超时和重试；环境侧固定硬件、CPU governor、内核、容器资源、磁盘、网络和数据库状态；运行状态侧说明冷态还是热态、缓存和连接池是否预热；观测侧说明是否开启 profile、trace、debug log。做 ablation 时一次只关一个组件。在 LogServe 里，我会固定 workflow、worker 数、mock LLM 延迟、shared log 目录和 GOMAXPROCS，再比较单个改动。
```

## Q005. 如何报告硬件、内核、运行时和配置版本？

报告环境的原则是：别人要能判断你的结果适不适合迁移，也要能尽量复现实验。性能数据离开环境就很容易失真。一个 `100 ns/op`、`p99 200ms`、`5 万 QPS` 的数字，如果没有硬件、内核、运行时、配置、负载和统计方法，基本只能当参考，不能当证据。

硬件信息至少要写 CPU、内存、磁盘、网络。CPU 不只是“8 核”，要写型号、物理核/逻辑核、基础频率和 turbo、cache、NUMA、是否独占机器。内存要写容量、频率或规格、NUMA 布局。磁盘要写 NVMe/SATA/HDD、本地盘还是云盘、文件系统、挂载参数、是否开启 write cache。网络要写本机、同机房、跨 AZ，网卡或云网络带宽。端到端压测还要写压测机配置，避免服务端没到瓶颈，客户端先满了。

内核和系统配置要写操作系统、内核版本、CPU governor、cgroup/container 限制、ulimit、透明大页、swap、文件系统、I/O scheduler、防火墙或代理。Linux 下 `uname -a` 能给内核和架构，CPU frequency 相关信息要看 cpufreq governor 或系统电源模式。容器里还要写 CPU quota、cpuset、memory limit、是否共享宿主机，以及容器运行时版本。

运行时和工具链要写到能解释行为。Go 项目至少写：

```text
go version
GOOS / GOARCH / GOAMD64
GOMAXPROCS
build tags
gcflags / ldflags
是否开启 race、asan、msan、checkptr
关键依赖版本
```

Java 要写 JDK/JRE 版本、JVM flags、GC、heap、JIT 编译相关参数。Python 要写 CPython/PyPy 版本、虚拟环境、依赖 lockfile、是否启用特殊统计或 JIT。C/C++ 要写 compiler、标准库、优化级别、LTO/PGO、链接库版本。数据库、Kafka、Redis、对象存储、Prometheus、OTel Collector 这类外部组件也要写版本和关键配置。

benchmark 配置同样要报告。microbenchmark 要写命令、重复次数、warm-up、运行时长、是否 `-benchmem`、是否固定 CPU、是否使用 benchstat、样本数。Google Benchmark 里要写 `--benchmark_repetitions`、`--benchmark_min_time`、`--benchmark_min_warmup_time`、输出格式和额外 context。pyperf 要写 processes、values、warmups、loops、是否校准。端到端压测要写并发数、到达率、请求 mix、payload、数据集、测试时长、warm-up 时长、统计窗口、超时、重试、连接复用策略。

结果报告要把环境和数字放在一起。只在 README 某处说“机器是 8 核”不够。每张表或每组实验至少要能追溯到环境块。Go benchmark 输出本身会包含 `goos`、`goarch`、pkg 和 `BenchmarkName-N`，其中 `-N` 常常对应并行度或 CPU 数；`benchstat` 会按文件级配置分组。如果你自己写报告，也应该保留类似的元数据。

一个比较完整的报告片段可以这样写：

```text
Hardware:
  CPU: AMD Ryzen 9 7950X, 16C/32T, SMT on, governor=performance
  Memory: 64 GiB DDR5
  Disk: local NVMe SSD, ext4
  Network: client and server on same host

System:
  OS: Ubuntu 24.04
  Kernel: Linux 6.8.x
  Container: none
  ulimit -n: 1048576

Runtime:
  Go: go1.22.x linux/amd64
  GOMAXPROCS: 16
  Build: release, race=false, gcflags=default
  Commit: abc1234

Benchmark:
  Command: go test -run='^$' -bench=. -benchmem -count=10
  Warm-up: first run discarded, caches preloaded
  Tool: benchstat old.txt new.txt
  Dataset: 1 KiB records, 1M operations, fixed seed 42
```

如果是 LogServe 的端到端实验，我会再加业务配置：

```text
Workflow: 10-step DAG, fixed seed, 1 KiB input, 4 KiB result ref
Workers: 8 Go workers, Python executor pool=4
LLM: mock latency p50=50ms, p99=300ms
Shared log: fsync=on, segment size=64 MiB, local NVMe
Storage: local MinIO or filesystem path, version/config
Observability: metrics on, tracing sampling=off/on with ratio
```

还要报告负面信息：没有做的控制也要写。比如“未固定 CPU governor”“运行在共享云主机”“没有清理 page cache”“压测客户端和服务端同机”“只跑了一轮”。这些说明会降低结论强度，但比假装没有问题更可信。

面试里可以这样回答：

```text
报告 benchmark 环境时，我会写硬件、系统、运行时、依赖、服务配置、负载配置和统计方法。硬件包括 CPU 型号、核数、频率策略、内存、磁盘、网络；系统包括 OS、kernel、CPU governor、cgroup、ulimit、文件系统；运行时包括 Go/JDK/Python 版本、GOMAXPROCS/JVM flags、编译参数、依赖版本；benchmark 配置包括命令、warm-up、重复次数、样本数、输入数据、并发、到达率、超时、重试和统计工具。更重要的是把未控制的限制也写出来。这样别人才能判断结果是否可复现、是否能迁移到生产环境。
```

## Q006. 吞吐和延迟为什么需要同时报告？

吞吐和延迟必须一起报告，因为它们回答的是两个不同问题。吞吐回答“系统单位时间完成了多少工作”，比如 requests/s、workflows/s、tasks/s、bytes/s。延迟回答“单个请求或单个任务等了多久”，通常要看分布，至少看 p50、p95、p99、max，而不能只看平均值。

只报告吞吐，容易把排队和用户体验藏起来。一个系统在压到极限时，完成吞吐可能还在上升，但队列已经开始堆积，p99 从几十毫秒涨到几秒。此时说“吞吐提高了 20%”并不完整，因为提高可能是靠牺牲尾延迟换来的。对在线服务来说，用户看到的是请求完成时间；对 workflow 系统来说，用户关心的是一次提交多久能被调度、执行、落盘并返回结果，而不只是后台每秒处理了多少条记录。

只报告延迟也不够，因为低延迟可能只是因为负载很低。同样是 p99=100ms，在 100 RPS 下和在 50,000 RPS 下含义完全不同。没有吞吐、并发、到达率、请求 mix 和错误率，延迟数字无法说明系统容量。更严重的是，某些压测工具在系统变慢后会自然降低请求发起速度，导致“看起来延迟还可以”，但其实它没有持续施加目标负载。

正确报告时，至少要把四组数放在一起：

- offered load：压测器计划施加的负载，比如目标到达率、并发数、payload、请求 mix。
- achieved throughput：系统实际完成的吞吐，比如成功 RPS、失败 RPS、超时数、被拒绝数。
- latency distribution：p50、p90/p95、p99、p999、max，最好按成功、失败、超时分别看。
- saturation signals：CPU、内存、GC、磁盘 I/O、网络、队列长度、in-flight、连接池等待、线程池/worker pool 使用率。

这里要特别区分“发起吞吐”和“完成吞吐”。open-loop 压测里，压测器可能按 10,000 RPS 发起请求，但系统只完成 7,000 RPS，剩下的变成排队、超时或丢弃；如果只看完成吞吐，就会漏掉 backlog。closed-loop 压测里，请求完成得慢会反过来降低新的请求发起速度；如果不报告 offered load，就会误以为系统在目标压力下表现稳定。

在 LogServe 这类 workflow/log 系统里，我会这样报告：

```text
Load:
  offered_workflows_per_second: 2000
  achieved_completed_workflows_per_second: 1850
  timeout_workflows_per_second: 80
  rejected_workflows_per_second: 70

Latency:
  submit_to_append_p50/p99
  append_to_schedule_p50/p99
  schedule_to_worker_start_p50/p99
  end_to_end_workflow_p50/p95/p99/max

Saturation:
  log append queue length
  worker queue length
  fsync latency
  goroutine count
  GC pause
  CPU and disk utilization
```

面试里可以这样回答：

```text
吞吐和延迟要同时报告，因为吞吐描述容量，延迟描述单个请求的体验和排队成本。只看吞吐，可能看不到 p99 已经爆炸；只看延迟，可能不知道这个延迟是在很低负载下测出来的。一个可信的压测结论应该同时给出目标到达率、实际完成吞吐、错误和超时、延迟分布以及资源饱和信号。尤其在 closed-loop 压测中，系统变慢会降低新的请求发起速度，所以更要报告 offered load 和 achieved throughput，否则很容易把排队问题误判成系统稳定。
```

## Q007. load test、stress test、soak test、spike test 的区别是什么？

这几个词都属于负载测试，但目标、负载形状、持续时间和结论强度不同。Grafana k6 的文档也强调，不同 traffic pattern 会暴露不同风险，没有一个测试类型可以覆盖所有失败模式。

load test 通常指在预期正常负载或预期峰值负载下验证系统表现。它的核心问题是：“在我们计划支持的业务压力下，系统是否满足 SLO？”负载一般接近生产平均值或计划峰值，持续时间中等，重点看吞吐、p95/p99、错误率和资源是否有足够余量。比如 LogServe 预期每秒 1000 个 workflow 提交，就用接近真实请求 mix 和 payload 的 1000 workflows/s 跑一段稳定窗口，验证端到端 p99 是否达标。

stress test 是把负载推到预期容量以上，找系统的极限和退化方式。它的核心问题是：“系统从哪里开始不满足 SLO，过载后是排队、超时、拒绝、崩溃，还是优雅降级？”stress test 不一定要求系统始终达标，重点是画出负载和延迟、错误率、资源饱和之间的曲线。好的 stress test 会回答容量拐点在哪里、瓶颈是什么、保护机制是否生效。

soak test 也叫 endurance test，关注长时间运行。它的核心问题是：“系统在持续压力下会不会慢慢坏掉？”负载通常不一定很高，可能就是平均生产负载，但持续数小时甚至更久。它用来发现内存泄漏、goroutine/thread 泄漏、连接泄漏、文件句柄泄漏、缓存无限增长、日志段清理失效、GC 趋势变差、磁盘空间增长、队列慢性堆积等问题。短 benchmark 很难暴露这些问题。

spike test 关注突然的负载尖峰。它的核心问题是：“流量突然从低位跳到高位，系统能不能吸收、限流、扩容并恢复？”负载形状是短时间内急剧上升，然后可能快速下降。它常用于秒杀、批量任务同时提交、上游重试风暴、发布后缓存击穿、定时任务整点触发等场景。spike test 重点看队列吸收能力、连接池、线程池、限流器、自动扩缩容、冷启动和恢复时间。

可以用这个表来区分：

| 类型 | 主要目标 | 负载形状 | 持续时间 | 典型结论 |
| --- | --- | --- | --- | --- |
| load test | 验证预期负载下是否达标 | 接近正常或计划峰值 | 中等 | SLO 是否满足、资源余量多少 |
| stress test | 找极限和退化方式 | 超过预期并逐步升高 | 中等 | 容量拐点、瓶颈、过载行为 |
| soak test | 找长时间运行问题 | 稳定负载 | 长 | 泄漏、漂移、慢性堆积 |
| spike test | 验证突发流量承受能力 | 短时间急升急降 | 短 | 尖峰下是否限流、排队、恢复 |

不要把这些测试名字当成固定绝对值。一个系统的 stress load，可能只是另一个系统的 average load。真正重要的是写清楚测试目标、负载模型、持续时间和判断标准。

面试里可以这样回答：

```text
load test 是在预期负载下验证系统是否满足 SLO；stress test 是超过预期负载去找容量上限和退化方式；soak test 是在较长时间内观察泄漏、资源漂移和慢性队列堆积；spike test 是制造突然的流量尖峰，看系统能否限流、扩容、排队并恢复。它们不是四个随便替换的词，而是不同的负载形状和风险假设。报告时必须写清目标负载、持续时间、请求 mix、成功率、延迟分布和资源指标。
```

## Q008. closed-loop 压测和 open-loop 压测有什么区别？

closed-loop 和 open-loop 的本质区别是：新的请求到达是否依赖前一个请求完成。

closed-loop 模型里，一个虚拟用户或客户端通常是“发请求，等待响应，可能 sleep 或 think time，然后再发下一个请求”。因此并发数相对固定，到达率会被系统响应时间影响。系统越慢，每个虚拟用户越久才能发起下一次请求，整体请求发起速率就会下降。k6 文档把这类模型描述为下一次 iteration 要等上一次 iteration 完成后才开始。

open-loop 模型里，请求到达按外部计划发生，不等待前一个请求完成。比如每秒固定发起 1000 次请求，或者按某个随机到达过程发起请求。系统变慢时，请求仍然按计划到达，in-flight 数量和队列长度会增加。k6 的 arrival-rate executor 就属于 open model，`constant-arrival-rate` 会在指定时间内按固定 rate 启动 iteration，并且这个启动过程独立于系统响应时间。

两者适合的场景不同。closed-loop 适合模拟“固定数量活跃用户”的交互式行为，比如 500 个用户各自等页面返回后再点下一步。它能回答在固定并发用户下系统体验如何，也比较容易控制客户端资源。open-loop 更适合模拟外部到达率固定的服务入口，比如 API 网关每秒收到多少请求、消息队列每秒进入多少消息、定时任务平台每秒提交多少 workflow。它能更直接暴露排队、过载和尾延迟。

一个简化对比如下：

| 维度 | closed-loop | open-loop |
| --- | --- | --- |
| 请求发起 | 等前一次完成后再发 | 按计划到达，和完成时间解耦 |
| 主要控制量 | 并发用户数、think time | 到达率、到达分布 |
| 系统变慢后的效果 | 到达率自然下降 | in-flight 和队列增加 |
| 适合问题 | 固定用户并发下体验如何 | 固定外部流量下容量是否够 |
| 主要风险 | 掩盖排队和 coordinated omission | 压测器资源不足会产生 dropped iterations |

在 LogServe 里，closed-loop 可以是 100 个客户端，每个客户端等 workflow 完成后才提交下一个 workflow。open-loop 可以是无论前一个 workflow 是否完成，都按 2000 workflows/s 的计划持续提交。前者回答“100 个同步调用者的体验”，后者回答“上游真的以 2000 workflows/s 打进来时系统是否扛得住”。这两个问题都合理，但不能混着解释。

面试里可以这样回答：

```text
closed-loop 压测中，请求发起和响应完成是耦合的，虚拟用户通常等上一个请求结束后才发下一个，所以系统变慢会自动降低到达率。open-loop 压测中，请求按外部计划到达，不等前一个请求完成，所以系统变慢会表现为 in-flight 增加、队列增长、超时或拒绝。closed-loop 适合模拟固定并发用户，open-loop 适合验证固定到达率下的服务容量和排队行为。报告时要明确自己控制的是并发数还是到达率。
```

## Q009. 为什么 closed-loop 压测可能掩盖排队延迟？

closed-loop 会掩盖排队延迟，是因为它存在天然的反馈限速。系统响应越慢，压测客户端等待越久，新的请求越晚发起。这样一来，系统最需要承受高到达率的时候，压测器反而把到达率降下来了。结果是队列没有按照真实外部流量那样增长，p99 也可能比真实情况低很多。

一个例子更直观。假设服务正常能处理 1000 RPS，正常延迟是 100ms。现在用 100 个 closed-loop 客户端压测，每个客户端等请求返回后立刻发下一次请求。在系统健康时，理论上大约能形成 1000 RPS。但如果某个瓶颈让响应时间变成 2s，这 100 个客户端最多只能形成约 50 RPS。也就是说，系统一慢，压测压力就自动从 1000 RPS 掉到 50 RPS。你看到的结果可能是“延迟升高了一点但系统恢复了”，但真实世界里如果上游仍然以 1000 RPS 到达，队列会继续堆积，后来的请求会等得更久。

k6 的 open/closed model 文档也指出，closed model 中 iteration duration 会影响新 iteration 的开始速率；当目标系统变慢时，arrival rate 会下降，这类问题在性能测试文献里常被称为 coordinated omission。也就是说，压测器和被测系统“同步变慢”，漏掉了本来应该发生在慢周期里的请求。

closed-loop 不是错误模型，它只是回答另一个问题。如果真实业务就是“用户看见页面返回后才会进行下一步”，closed-loop 很自然。但如果你想验证 API 每秒固定收到多少请求、队列每秒固定进入多少消息、上游批处理每秒固定提交多少任务，就不能只用 closed-loop。此时至少要补一个 open-loop 或 constant-arrival-rate 测试，并报告 backlog、in-flight、dropped iterations、timeout、rejection。

在 LogServe 里，如果测试脚本写成“提交 workflow，等 workflow 完成，再提交下一个”，那么 worker 卡住 10 秒时，脚本本身也停住了。这会导致 10 秒内本来应该进入系统的 workflow 没有被提交，log append queue 和 worker queue 的压力都被低估。真实线上如果上游仍持续提交，队列会增长，等待时间会扩散到后续大量 workflow。

面试里可以这样回答：

```text
closed-loop 会掩盖排队延迟，因为它把请求到达率和响应时间耦合在一起。系统变慢后，虚拟用户等待更久，新请求自然减少，压测压力被自动降下来了。真实 open-loop 流量下，请求不会因为系统慢就消失，而是会排队、超时或被拒绝。所以 closed-loop 的 p99 可能低估真实尾延迟。要避免这个问题，需要报告目标到达率和实际完成吞吐，补充 open-loop 或 constant-arrival-rate 压测，并观察 backlog、in-flight、超时和拒绝。
```

## Q010. coordinated omission 如何影响压测结论？

coordinated omission 指的是测量过程和被测系统的慢周期发生了“协调”，导致慢周期里本该被观察到的大量延迟样本被漏掉。它最常见于 closed-loop 压测和同步监控采样：系统卡住时，压测器也在等待；等待期间没有继续按计划发起请求，于是那些本来会排队的请求没有进入样本集。

它对结论的影响非常大，尤其会低估尾延迟。假设压测计划是每 10ms 发一个请求，系统突然暂停 10s。如果压测器在暂停期间只等一个请求返回，那么原始样本里可能只有一个 10s 的慢请求；但真实固定到达率下，这 10s 里应该有大约 1000 个请求受到影响，其中很多请求会经历从几十毫秒到数秒不等的排队等待。HdrHistogram 的 README 用类似场景说明，如果只记录暂停期间的一个长样本，原始分布会显示绝大多数结果仍然很快；用 expected interval 修正后，分布才会更接近随机请求实际会经历的响应时间。

wrk2 的 README 也把这个问题说得很直接：传统 wrk 这类“收到响应后再在同一连接上发下一个请求”的模型，会在高延迟期间避免继续测量，从而忽略大量高延迟现象。wrk2 的做法是生成 constant throughput，并按请求“本应发送的时间”而不是“实际发送的时间”来记录延迟，这样后续请求受到暂停影响的等待时间也会进入延迟统计。

coordinated omission 会带来几类错误结论：

- p99、p999 被显著低估，看起来尾延迟比真实情况好。
- max 可能只看到一个孤立慢点，却看不到慢点扩散成一批排队请求。
- 系统容量被高估，因为压测器在系统慢的时候自动少发了请求。
- A/B benchmark 被污染。如果 A 版本偶发长暂停，B 版本没有，但压测器漏掉 A 的慢周期，结果可能错误地认为 A 的尾延迟可接受。
- SLO 结论失真。你以为 99% 请求满足 200ms，其实只是慢周期里本该到达的请求没有被统计进去。

缓解方法不是简单地“换一个百分位”。p99、p999 都来自样本集，样本集漏了，百分位也会漏。更可靠的做法是：

- 使用 open-loop 或 constant-arrival-rate 负载模型，让请求到达独立于响应完成。
- 记录 scheduled start time 到 completion time 的延迟，而不是只记录 actual send time 到 response time。
- 使用能修正 coordinated omission 的 histogram 或压测工具，比如 HdrHistogram 的 expected interval 记录方式，或者 wrk2 这类 constant throughput 工具。
- 同时报告 offered load、achieved throughput、dropped iterations、timeout、rejection 和 backlog。
- 不要把超时、失败、被限流请求从延迟和可用性结论里静默删掉。

在 LogServe 的语境里，coordinated omission 可能发生在一个很普通的测试脚本里：脚本提交一个 workflow，然后阻塞等待结果。如果 worker pool 因为 Python executor 卡住 10 秒，脚本这 10 秒没有继续提交 workflow。最后报告里可能只有一个 10 秒慢请求，但真实线上上游会继续提交，shared log、scheduler queue、worker queue 都会累积等待。这个实验会低估系统在暂停或过载期间的真实尾延迟。

面试里可以这样回答：

```text
coordinated omission 会让压测结果系统性偏乐观。系统慢的时候，压测器也在等待，于是慢周期里本应到达并排队的大量请求没有被采样。结果是 p99、p999、容量上限和 SLO 结论都可能被高估或低估到错误方向：看起来尾延迟很好，其实只是漏测了。处理办法是用 open-loop 或 constant-arrival-rate 模型，按计划发送时间到完成时间记录延迟，保留超时和失败，并结合 backlog、in-flight、dropped iterations 一起解释。
```

## Q011. p99 latency 的置信度如何受样本量影响？

p99 latency 是样本分位数。把一次压测得到的延迟从小到大排序，p99 大致落在 `ceil(0.99 * n)` 这个位置。这里的 `n` 是有效样本数，不是压测持续了多久，也不是压测器打印了多少行日志。样本量越小，p99 越接近极端值，置信度越差。

最容易忽略的是尾部样本数量。p99 只看最慢的 1% 附近，所以尾部样本数约等于 `0.01 * n`：

| 有效请求数 n | p99 附近尾部样本数 | 直觉解释 |
| --- | --- | --- |
| 100 | 约 1 个 | p99 基本就是最大值附近，极不稳定 |
| 1,000 | 约 10 个 | 能看到尾部，但一个异常点就能明显影响结果 |
| 10,000 | 约 100 个 | 可以粗略看 p99，但分组后仍可能不足 |
| 100,000 | 约 1,000 个 | 更适合比较 p99，仍要看流量分布是否真实 |

还有一个简单概率可以帮助判断样本是否太少：如果真实最慢的 1% 请求存在，那么 `n` 个独立样本完全没碰到这 1% 的概率是 `0.99^n`。`n=100` 时，漏掉真实最慢 1% 的概率还有约 36.6%；`n=1000` 时，这个概率已经很低。但这只能说明“有没有碰到尾部”，不能说明 p99 数值估计得很准。要稳定估计 p99，需要尾部附近有足够多样本，而不是只碰到一两个慢请求。

从统计上看，样本 p99 的秩本身也有波动。真实 p99 以下的样本数量服从近似二项分布，均值是 `0.99n`，标准差约为 `sqrt(n * 0.99 * 0.01)`。`n=1000` 时秩标准差约 3.1 个样本；`n=10000` 时约 9.95 个样本。秩波动转换成延迟值后会不会很大，取决于尾部曲线有多陡。如果 p98 到 p99.5 之间延迟从 100ms 跳到 2s，同样的秩波动会变成很大的数值波动。

所以 p99 的置信度至少受四件事影响：

- 样本总数。没有足够请求数，p99 只是一个很脆的排序结果。
- 样本是否独立同分布。请求如果来自同一个连接、同一个 key、同一个 worker，统计上会相关，不能当成同样多的独立样本。
- 流量分布是否覆盖真实尾部。全是小 payload、小 DAG、热缓存请求，样本再多也测不到真实 p99。
- 是否保留失败、超时和被拒绝请求。把最慢请求删掉后算 p99，本质上是在换问题。

面试里我会把结论说得很直白：

```text
p99 对样本量非常敏感，因为它只使用最慢 1% 附近的信息。100 个请求的 p99 基本不可信，1000 个请求也只有大约 10 个尾部样本；如果要比较两个版本的 p99，通常需要上万到十万级请求，并且要按 endpoint、payload、tenant、缓存状态分层看。样本量只解决统计波动，不能修复流量模型错误。请求分布不真实，p99 会稳定地错。
```

## Q012. 如何选择压测请求分布？

压测请求分布要从测试目标倒推。你要验证平均生产负载、峰值容量、长时间稳定性、突发流量，还是某个优化的回归风险？目标不同，请求分布也不同。Grafana k6 文档把不同负载类型分开，就是因为不同 traffic pattern 会暴露不同风险。

最好的起点通常是生产观测数据，而不是手写一个“看起来平均”的脚本。可以从访问日志、trace、metrics、队列消息、业务审计表里抽取这些维度：

- endpoint / RPC method / workflow type 的比例。
- payload size、result size、DAG 节点数、任务运行时间的分布。
- tenant、用户、key、topic、partition 的热度分布。
- read/write 比例，缓存命中/未命中比例。
- 到达过程：平稳到达、突发、批处理、周期性尖峰、重试风暴。
- dependency 行为：数据库、对象存储、外部模型服务的延迟和错误率。
- 请求之间的相关性：一次 workflow 可能触发一串任务，不是独立随机点。

选择分布时可以分成三层。

第一层是业务 mix。比如 LogServe 里不同 workflow 类型占比不同，10 步 DAG 和 100 步 DAG 不能混成一个“平均 DAG”。如果生产里 5% 的大 workflow 消耗了 50% 的资源，就必须保留这个重尾。

第二层是到达模型。用户交互类请求可以用 closed-loop 或带 think time 的模型；队列、API 网关、批处理提交更适合 open-loop 或 arrival-rate 模型。k6 的 `constant-arrival-rate` 文档明确把 iteration 启动速率和响应完成时间解耦，这类模型更适合测固定外部到达率下的排队行为。

第三层是数据和状态。缓存、索引、page cache、连接池、worker 本地状态都会影响结果。请求 key 的选择不能只用均匀随机。真实系统里常见 Zipf-like 热点、少数大租户、热门对象、冷热数据混合。分布选错，压测会把缓存、锁、数据库索引和队列行为全部测偏。

我会优先用这几种方式：

- 生产 trace 抽样回放：适合验证真实 mix，但要脱敏，避免不可重复副作用。
- 经验分布采样：从生产统计里抽出 endpoint、payload、key 热度、DAG 大小的直方图，再按固定 seed 采样。
- 分层 synthetic workload：专门构造 small/medium/large workflow、热 key/冷 key、成功/失败路径，用来解释瓶颈。
- 压力型分布：在真实 mix 基础上放大大 payload、慢 dependency、重试比例，用来测风险边界。

面试里可以这样回答：

```text
压测请求分布应该从生产观测和测试目标里来。先确定要测正常负载、峰值、突发还是某个优化，再决定 endpoint 比例、payload 分布、tenant/key 热度、到达过程、缓存状态和依赖延迟。能用生产 trace 或统计直方图就不要拍脑袋写均匀随机；如果要做 synthetic workload，也要分层写清楚 small、medium、large、hot key、cold key 的比例。最后固定随机种子并报告分布，否则结果很难复现。
```

## Q013. 均匀分布和真实流量分布差异会导致什么误判？

均匀分布最大的风险是把真实系统里的偏斜抹平。线上流量很少是均匀的：少数租户可能贡献大部分请求，少数 key 可能被反复访问，少数 endpoint 可能走最贵的路径，请求到达也常常有批量、周期和突发。压测脚本如果把 endpoint、key、payload、DAG 大小都均匀随机，得到的是一个干净但很可能不存在的世界。

它会造成几类误判。

缓存会被测错。真实流量有热点时，缓存命中率可能很高；均匀 key 会把热点打散，导致缓存看起来没用。反过来，如果真实流量里有大量冷数据扫描，压测只打固定热 key，又会把缓存命中率测得过高。两种错法都会影响 p99，因为缓存 miss 往往落在尾部。

锁和热点会被测错。真实系统里某个 tenant、partition、workflow namespace 很热，可能触发单点锁竞争、队列倾斜、数据库行锁或 shard 热点。均匀分布把请求摊开后，这些问题不出现，于是调度策略看起来很公平，实际生产会在热点上排队。

资源消耗会被测错。payload size 和 workflow DAG 大小通常是重尾的。均匀分布如果只覆盖平均大小，会低估内存、序列化 CPU、对象存储 I/O 和 GC 压力。少数超大请求可能决定 p99，但在均匀小请求压测里完全看不到。

到达过程也会被测错。真实上游可能整点批量提交、失败后集中重试、发布后缓存击穿。均匀到达会让队列一直很平滑，看不到 spike 下的 backlog 和超时扩散。

对 LogServe 来说，均匀分布可能导致这些具体误判：

- 每个 workflow 都差不多大，于是看不出大 DAG 对 scheduler queue 的影响。
- 每个 result ref 都差不多小，于是低估对象存储和本地 cache 压力。
- 每个 actor 或 namespace 都平均访问，于是看不出单 namespace 热点下的锁竞争。
- mock LLM 延迟太平滑，于是看不出慢 dependency 如何把 worker pool 堵住。

均匀分布不是完全不能用。它适合做 baseline、单元级压测、算法复杂度扫描。问题出在把它当成生产结论。报告里必须说明“这是 uniform synthetic workload”，并补充真实分布或压力分布。

面试里可以这样回答：

```text
均匀分布会把真实流量里的热点、重尾、突发和相关性抹掉。结果可能高估系统容量，低估 p99、锁竞争、队列倾斜、缓存 miss 和大请求的影响；也可能因为把真实热点打散而低估缓存收益。均匀 workload 可以用来做 baseline，但不能直接代表生产。可信压测要至少补充生产 trace 抽样、经验分布采样，或者明确构造 hot key、large payload、burst、retry 这些场景。
```

## Q014. ablation study 的目的是什么？

ablation study 的目的，是拆出某个设计或优化的独立贡献。它不只是“多跑几组配置”，而是回答一个因果问题：这个性能提升到底来自哪一部分？如果把某个组件关掉、替换成基线实现，效果是否消失？

一个完整系统里，性能改善常常来自多个因素叠加。比如吞吐提高可能来自 batching，p99 改善可能来自调度策略，也可能只是缓存更热、GC 更少、压测顺序更有利。ablation 的价值就是把这些因素拆开，避免把所有收益都归功于最想宣传的那个优化。

常见 ablation 方式有几种：

- remove：关掉某个优化，比如禁用 batch append，观察吞吐和 p99 是否退化。
- replace：把新策略替换成简单基线，比如把自适应调度换成 FIFO 或 round-robin。
- isolate：只开启一个优化，其他优化保持基线，看单独贡献。
- interaction：同时开启两个优化，再和单独开启对比，看是否有相互增强或相互抵消。

设计 ablation 时要保持其他变量不变。相同硬件、相同版本、相同负载、相同数据集、相同随机种子、相同 warm-up、相同统计窗口。否则你测到的是一堆变量混合后的差异。

在 LogServe 里可以这样做：

| 目标 | Baseline | Ablation variant | 观察指标 |
| --- | --- | --- | --- |
| 验证 batch append | 单条 append | 开启 batch append | append 吞吐、fsync 次数、p99 append latency |
| 验证调度策略 | FIFO | 新调度策略 | queue wait p99、workflow makespan、公平性 |
| 验证本地 checkpoint cache | cache off | cache on | replay 时间、heap、cache hit ratio |
| 验证 backpressure | backpressure off | backpressure on | timeout、rejection、queue length、恢复时间 |

面试里可以这样回答：

```text
ablation study 是为了证明某个组件或优化到底贡献了多少。做法是保持负载、硬件、数据集和统计方法不变，只移除或替换一个因素，看结果如何变化。它比单纯报告最终系统快多少更可信，因为最终收益可能来自缓存、batching、调度、GC 或压测顺序。好的 ablation 还要看交互效应：两个优化单独有效，放在一起不一定继续叠加。
```

## Q015. 如何证明某个优化确实有效而不是噪声？

要证明优化有效，不能只看一次 benchmark 变快。一次结果可能来自 CPU 频率、GC 时机、cache 状态、后台进程、网络抖动、压测顺序，甚至只是随机慢请求少了一些。证明过程要把“效果大小”和“随机波动”分开。

我通常按这个顺序做。

先定义假设和最小有意义收益。比如“把 append 路径分配从 2 allocs/op 降到 0 allocs/op，并让 p50 sec/op 至少下降 5%”。如果只有 0.5% 的变化，而系统噪声本来就有 2%，就算统计显著也未必值得合并。

然后做成对比较。old 和 new 要在同一台机器、同一组参数、同一数据集下跑。顺序最好交替，比如 old/new/old/new，而不是先跑完全部 old 再跑全部 new。这样可以降低温度、后台负载、缓存状态随时间漂移带来的偏差。

Go microbenchmark 可以用官方 benchstat 工作流：`go test -run='^$' -bench=. -benchmem -count=10` 分别收集 old/new，再用 `benchstat old.txt new.txt` 看 median、置信区间和 p-value。benchstat 文档也提醒，统计显著不等于效果大；很小的变化在低噪声和大样本下也可能显著。

端到端压测要多跑几个独立窗口，而不是只截一个最好看的窗口。每轮都报告 offered load、achieved throughput、错误率、超时、p50/p95/p99、资源利用率、队列长度。优化如果只让成功请求变快，但超时和拒绝增加了，那不是简单的性能提升。

最后要用 profile 或系统指标解释机制。比如优化声称减少锁竞争，mutex profile 或 block profile 应该看到对应等待下降；声称减少 CPU，CPU profile 的热点应该变窄；声称减少 GC，heap allocation、GC pause、allocs/op 应该下降。数字变好但机制对不上，要怀疑实验。

面试里可以这样回答：

```text
我会先定义最小有意义收益，再做重复、成对、可复现的 A/B benchmark。microbenchmark 用 go test -bench -count 多次采样，再用 benchstat 看中位数、置信区间和 p-value，同时看 ns/op、B/op、allocs/op。端到端压测要固定 offered load、请求分布、数据集和统计窗口，多轮运行并报告错误率、超时和资源指标。最后用 profile 解释机制：CPU 优化要看到 CPU 热点下降，锁优化要看到 mutex/block 等待下降。只有统计结果和机制证据都对得上，我才会说优化有效。
```

## Q016. 如何设计 A/B 对照实验比较两个调度策略？

比较两个调度策略，先要明确比较对象。A 和 B 只能在调度策略上不同，其他因素要尽量相同：worker 数量、队列容量、workflow DAG、任务耗时分布、依赖延迟、重试策略、超时策略、缓存状态、日志落盘策略都要固定。否则结果差异很难归因到 scheduler。

实验指标不能只看平均完成时间。调度策略常常在吞吐、公平性和尾延迟之间取舍。我会至少看这些指标：

- end-to-end workflow latency：p50、p95、p99、max。
- queue wait time：从 ready 到被 worker 开始执行的等待时间。
- makespan：一批 workflow 全部完成的总时间。
- throughput：完成 workflows/s 和 tasks/s。
- fairness：不同 tenant、不同 DAG 大小、不同优先级的等待时间差异。
- starvation：是否有任务长期拿不到 worker。
- retry/timeout/rejection：调度策略是否把压力转移成失败。
- resource profile：CPU、heap、lock contention、worker utilization。

设计上有三种常用方式。

第一种是离线 replay。把同一批 workflow 到达事件、DAG、任务耗时、依赖延迟记录下来，分别喂给 A 和 B。优点是可重复、便宜、适合调试因果；缺点是模拟器可能漏掉真实系统开销。

第二种是线上或端到端交替实验。按固定窗口交替运行 A/B，比如 A 10 分钟、清空队列、B 10 分钟、清空队列，再重复多轮。这样可以减少两个策略互相干扰。注意要有 warm-up 和 washout，队列残留会污染下一轮。

第三种是随机分流。按 tenant 或 workflow id 把请求分到 A/B。它接近真实线上 A/B，但调度器共享 worker pool 时会互相影响。要么资源隔离，要么把它当成 canary，而不是纯净实验。

对 LogServe 更稳的做法是先离线 replay，再端到端交替。离线 replay 用相同 trace 比较策略决策和排队时间；端到端交替用真实 worker、WAL、Python executor、对象存储 mock 验证开销。最后按 workflow size、tenant、priority 分层看结果，避免一个策略只优化小任务，却饿死大任务。

面试里可以这样回答：

```text
我会把调度策略作为唯一变量，固定 workload、worker 数、队列容量、任务耗时、重试和超时策略。指标不只看平均完成时间，还要看 queue wait p99、workflow p99、makespan、throughput、公平性、starvation、timeout 和资源利用率。实验可以先用同一条 trace 做离线 replay，再做端到端交替 A/B，并在每轮之间清空队列或设置 washout。调度策略很容易把收益从一类任务转移到另一类任务，所以必须按 tenant、DAG 大小和优先级分层报告。
```

## Q017. 如何避免 benchmark 被缓存命中率偶然影响？

缓存命中率会强烈影响 benchmark。CPU cache、page cache、应用 cache、数据库 buffer cache、对象存储 client cache、连接池、DNS cache 都可能参与进来。如果不控制缓存状态，old/new 的差异可能只是一个版本更早跑、刚好把数据预热了。

第一步是明确要测哪种缓存状态。冷缓存、热缓存和混合缓存是三个不同问题：

- cold-cache：系统刚启动或数据第一次访问，适合测恢复、扩容、首次访问。
- warm-cache：工作集已预热，适合测稳定状态吞吐。
- mixed-cache：按真实命中率混合，适合生产容量估计。

第二步是让缓存状态可重复。热缓存测试要显式 warm-up，直到 hit ratio 和延迟稳定，再开始统计。冷缓存测试要重启进程、清理应用 cache，必要时更换数据集；如果涉及 OS page cache，是否清理要写清楚。生产里不能随便清 page cache，但实验报告必须说明有没有做。

第三步是固定 key 序列和数据集。随机 key 每次不同会导致命中率漂移。应该固定 seed，或者直接用生产 trace 的 key 序列。数据集大小要和目标一致：如果要测 cache miss，就让工作集大于 cache；如果要测 hot set，就让热点比例接近生产。

第四步是避免运行顺序污染。不要 old 跑完把 cache 热好后马上跑 new，然后说 new 更快。可以交替运行 old/new，或者每轮都重置到同样状态。共享 Redis、数据库、对象存储也要隔离，不然另一个测试可能把你的缓存状态改掉。

第五步是把 hit ratio 当成结果一起报告。只报告延迟不够。要写 LRU hit ratio、page cache hit/miss、DB buffer hit、对象存储本地 cache hit、LogServe segment index 命中、本地 checkpoint cache 命中。优化如果只是让 hit ratio 变高，结论应该写成“在这个缓存状态下更快”，而不是泛化成算法一定更快。

面试里可以这样回答：

```text
避免缓存偶然影响，先要定义测的是冷缓存、热缓存还是生产混合缓存。热缓存要显式 warm-up 并等 hit ratio 稳定后再统计；冷缓存要重启或清理相关缓存；混合缓存要用真实 key 分布或固定 seed 的经验分布。old/new 要交替运行或每轮重置状态，不能让后跑的版本吃到前一个版本预热好的 cache。报告里必须带缓存命中率和工作集大小，否则延迟差异很难解释。
```

## Q018. 如何区分 CPU-bound、I/O-bound、lock-bound、network-bound？

区分瓶颈不能靠单个指标，要看吞吐、延迟、资源饱和、profile 和扩容实验是否互相吻合。

CPU-bound 的典型表现是 CPU 利用率接近饱和，run queue 变长，CPU profile 里业务函数、序列化、压缩、哈希、GC 等栈很宽。增加 CPU 核数或降低每次请求的计算量后，吞吐会改善；换更快磁盘通常没用。如果 Go 程序里 `GOMAXPROCS` 太低，也可能表现成 CPU 没吃满但调度受限，所以要同时看进程 CPU、系统 CPU、goroutine 状态和 `GOMAXPROCS`。

I/O-bound 的表现是请求在等磁盘、文件系统、数据库或对象存储。CPU 可能不高，但 iowait、磁盘 util、队列深度、fsync latency、read/write latency 上升。profile 里纯 CPU 热点不明显，trace 或系统指标会显示大量时间花在 syscall、fsync、读写等待或外部存储响应上。换更快磁盘、减少 fsync、batch 写入、降低读放大后会改善。

lock-bound 的表现是增加并发后吞吐不升反降，p99 拉长，CPU 可能没有满，但 goroutine 大量卡在 mutex、channel、WaitGroup 或 Cond 上。Go 里要看 block profile 和 mutex profile。runtime/pprof 文档说明，block profile 记录在同步原语上阻塞的栈，mutex profile 记录竞争 mutex 的持有者栈和别人等待的累计时间。锁问题常常不是“锁次数多”，而是临界区太长或热点太集中。

network-bound 的表现是时间花在远端调用、RTT、带宽、连接池、TLS、丢包或重传上。客户端和服务端同机时问题消失，跨机或跨 AZ 时 p99 变差。指标上看 socket send/recv、连接池等待、TCP retransmit、带宽利用率、远端服务延迟。Go profile 可能只看到 goroutine 在 netpoll 或 RPC client 上等待，这时要结合 trace、连接池 metrics 和网络层指标。

可以用一个快速判断表：

| 类型 | 常见信号 | 验证动作 |
| --- | --- | --- |
| CPU-bound | CPU 高、CPU profile 热点宽、吞吐随核数变化 | 优化算法/分配，增加核数，对比 CPU profile |
| I/O-bound | iowait/磁盘队列/fsync 高，CPU 不一定高 | batch I/O、换存储、减少同步写 |
| lock-bound | block/mutex profile 高，并发越大 p99 越差 | 缩短临界区、分片锁、减少共享状态 |
| network-bound | RTT/重传/连接池等待/远端延迟高 | 同机对比、连接池调参、减少调用次数 |

面试里可以这样回答：

```text
我会用资源指标和 profile 交叉判断。CPU-bound 看 CPU 饱和、run queue 和 CPU profile 热点；I/O-bound 看 iowait、磁盘队列、fsync/read/write latency；lock-bound 看 Go block profile、mutex profile、goroutine 阻塞和并发扩展性；network-bound 看 RTT、带宽、重传、连接池等待和远端调用延迟。最后做验证实验：加核、换磁盘、分片锁、同机部署或减少 RPC。如果指标和验证动作都指向同一类瓶颈，结论才稳。
```

## Q019. CPU profile、heap profile、block profile、mutex profile 分别看什么？

这四类 profile 回答的问题不同，不能混用。

CPU profile 看程序在采样时正在 CPU 上运行的栈。它适合找计算热点：序列化、压缩、哈希、调度算法、GC、runtime 开销、循环里的昂贵函数。CPU profile 的宽度代表采样占比，不代表请求端到端等待时间。如果程序大部分时间在等磁盘或网络，CPU profile 可能很“干净”，但延迟仍然很差。

heap profile 看内存分配。Go 的 runtime/pprof 文档说明，heap profile 默认偏向 live objects，也可以通过 pprof 的 `-alloc_space`、`-alloc_objects` 看累计分配。它适合找内存泄漏、对象保留、缓存无限增长、每请求分配过多、GC 压力。分析 heap 时要区分 in-use 和 alloc：in-use 高说明对象还活着，alloc 高说明分配 churn 大，但对象可能很快被回收。

block profile 看 goroutine 在同步原语上阻塞的时间。包括 mutex、RWMutex、WaitGroup、Cond、channel send/receive/select 等。它适合找 goroutine 为什么没在跑：是被 channel 背压卡住，还是在等锁，还是等某个 WaitGroup。Go 里要通过 `runtime.SetBlockProfileRate` 开启或调整采样率，否则数据可能为空或不足。

mutex profile 专门看 mutex 竞争。runtime/pprof 文档里有一个容易误解的点：mutex profile 的栈对应导致竞争的临界区结束位置，常见是 `Unlock` 的调用栈；样本值是其他 goroutine 等这个锁的累计时间。它回答“谁持有锁导致别人等”，不是简单回答“谁在 Lock 这里等”。Go 里通过 `runtime.SetMutexProfileFraction` 控制采样比例。

在 LogServe 里可以这样对应：

- CPU profile：看 WAL 编码、CRC、调度器排序、JSON/protobuf、mock LLM wrapper 是否吃 CPU。
- heap profile：看 workflow state、result ref、trace/log buffer、checkpoint cache 是否保留太多对象。
- block profile：看 append queue、worker queue、channel backpressure、WaitGroup 等待。
- mutex profile：看 shared log metadata、scheduler state、actor registry、cache map 的锁竞争。

面试里可以这样回答：

```text
CPU profile 看 on-CPU 热点，适合找算法和计算开销；heap profile 看 live objects 和累计分配，适合找泄漏、对象保留和 GC 压力；block profile 看 goroutine 在锁、channel、WaitGroup 等同步原语上的阻塞时间；mutex profile 看竞争锁的持有者以及别人等待这个锁的累计时间。CPU profile 不能解释所有延迟，I/O、锁和网络等待通常要靠 block、mutex、trace 和系统指标一起看。
```

## Q020. 火焰图适合发现哪些问题？

火焰图适合把大量采样栈压缩成一张可读的图，用来快速发现“时间或资源主要花在哪些调用路径上”。Brendan Gregg 对 flame graph 的说明里有两个关键点：x 轴不是时间线，而是样本集合的横向排列；一个框越宽，说明这个栈帧出现在样本里的比例越高。y 轴是调用栈深度，底部是祖先调用，顶部通常是当前正在消耗资源的位置。

它最适合发现这些问题：

- CPU 热点：某个函数或调用链占了大量 CPU，比如序列化、压缩、哈希、排序、正则、日志格式化。
- 意外调用路径：本来以为是轻量请求，结果走了反射、JSON、锁包装、debug logging 或 expensive validation。
- 分配热点：用 allocation flame graph 看哪些路径创建了最多对象或字节。
- 锁等待热点：用 block/mutex profile 生成火焰图，看等待时间集中在哪些调用链。
- GC 或 runtime 开销：看 `runtime.mallocgc`、scan、sweep、map growth 等是否变宽。
- 回归对比：用 before/after 或 differential flame graph 看某次改动让哪些栈变宽或变窄。

火焰图的优点是适合回答“从哪里开始看”。纯文本 `top` 只能列函数名，容易丢掉上下文；火焰图能看到这个函数是从哪条业务路径进来的。比如同一个 `json.Marshal`，如果来自 hot path 的日志字段，就该优化日志；如果来自 control-plane debug endpoint，就不一定是问题。

它也有边界。普通 flame graph 不是时间序列，不能告诉你第 10 秒发生了什么，也不能直接展示请求之间的排队因果。要看时间线、goroutine 生命周期、网络事件、GC 时间点，更适合用 Go execution trace、flame chart 或 tracing。火焰图也依赖采样来源：CPU profile 生成的火焰图看 CPU，heap profile 生成的看内存，mutex profile 生成的看锁等待。采样源错了，图再漂亮也会误导。

在 LogServe 里，火焰图可以用来快速定位这些问题：append 路径 CPU 是否花在 CRC/编码，scheduler 是否花在排序或锁内遍历，workflow replay 是否花在反序列化，Python executor wrapper 是否因为 JSON 转换产生大量分配，worker queue 是否在某个锁上集中等待。

面试里可以这样回答：

```text
火焰图适合从采样栈里找主要热点和调用路径。框越宽，说明该栈帧在样本中出现越多；y 轴表示调用深度，x 轴不是时间。CPU 火焰图适合找计算热点，allocation 火焰图适合找分配热点，mutex/block 火焰图适合找等待集中在哪些调用链。它不适合单独解释时间顺序和排队因果；如果要看某一秒发生了什么，要结合 trace、指标和日志。
```

## Q021. 为什么 benchmark 需要记录 raw data 而不只记录 summary？

benchmark 只记录 summary，很容易把关键问题藏掉。summary 通常是平均值、中位数、p95、p99、吞吐、错误率这几行数字，它适合快速阅读，但不适合审计实验。raw data 才能回答这些问题：慢请求集中在哪个时间段？是否有 warm-up 漂移？是否有两个模式混在一起？p99 是稳定尾部，还是一个异常点撑起来的？失败和超时有没有被统计口径吞掉？

Go 的 benchstat 会基于多次 benchmark 原始输出做统计比较，报告中位数、置信区间和 p-value。它需要的是多轮原始结果，而不是你手工整理后的一行“快了 12%”。端到端压测也一样。如果只保存最后的 p99=300ms，后面就没法重新计算 p99.9、按 endpoint 分组、排除 warm-up、检查 coordinated omission，也不能用新的统计方法复核旧结论。

raw data 至少要保留几类东西：

- 每轮 benchmark 的原始输出，包括 `ns/op`、`B/op`、`allocs/op`、样本数、命令行参数。
- 每个统计窗口的吞吐、错误、超时、重试、dropped iterations、in-flight、队列长度。
- 延迟原始样本或高精度 histogram，最好能区分成功、失败、超时、被拒绝。
- 资源时间序列：CPU、内存、GC、磁盘 I/O、网络、连接数、文件描述符。
- workload 元数据：请求分布、随机种子、数据集、payload、并发、到达率、warm-up、测试时长。
- 环境元数据：commit、配置、依赖版本、内核、硬件、容器限制、运行时参数。

raw data 也能保护你自己。很多性能回归不是一开始就能解释清楚的。今天只关心平均延迟，明天可能发现 p99 在某个时间段抖动；今天只比较调度策略，明天可能怀疑 GC 或 page cache 影响结果。没有原始数据，就只能重跑。重跑可能遇到不同硬件状态、不同依赖版本、不同背景负载，结论很难对齐。

当然，raw data 不是让报告变成垃圾场。报告里可以放 summary，仓库或 artifact 里保存原始文件。敏感字段要脱敏，大量 per-request 样本可以压缩，超大日志可以采样或保留 histogram。但不能只保留一句“结果见下表”。

面试里可以这样回答：

```text
benchmark 要记录 raw data，因为 summary 会丢掉分布、异常点、时间漂移、失败样本和分组信息。只有 raw data 才能重新计算不同百分位、检查 warm-up、验证 p99 是否稳定、做 A/B 统计比较、追踪错误和超时。我的做法是报告里放 summary，artifact 里保留原始 benchmark 输出、延迟样本或 histogram、资源时间序列、workload 配置和环境元数据。没有 raw data，实验结论基本不可审计。
```

## Q022. 如何识别压测客户端成为瓶颈？

压测客户端成为瓶颈时，看到的延迟和吞吐就不再只反映被测系统。Grafana k6 的大规模测试文档给了一个很实用的判断：压测机要留出 CPU idle，k6 如果把 CPU 吃到 100%，测试会被客户端限速，结果中的响应时间可能比真实情况更差。这个原则不限于 k6，JMeter、wrk、自研压测器也一样。

常见信号有这些：

- 压测机 CPU 接近 100%，run queue 很长，压测脚本自己在排队。
- 压测机内存接近上限，开始 swap；这会让不同时间段的结果不可比。
- 网卡带宽、包处理能力或连接跟踪表打满。
- 文件描述符、ephemeral port、socket buffer 不够，出现 `too many open files`、`i/o timeout`、connect timeout。
- 压测器的 scheduled 请求数大于实际 sent 请求数，或者 k6 出现大量 `dropped_iterations`。
- 增加目标系统容量没有提升吞吐，但把压测客户端拆成多台后吞吐上升。
- 被测系统 CPU、队列、连接池都不高，但客户端侧 latency 很高。
- 客户端 profile 里 JSON 构造、日志、TLS、随机数据生成、结果写盘占了大量时间。

识别时我会做几组对照。先把目标系统换成极轻量的 no-op endpoint 或本地回环服务，验证压测器最大能打多少。再把同样负载拆到两台或多台压测机，看总吞吐是否线性上升。如果线性上升，原来的单机客户端大概率是瓶颈。还要监控压测机自身的 CPU、内存、网络、文件描述符、GC、dropped iterations，而不是只监控服务端。

压测脚本本身也要被审查。复杂脚本会在客户端做大量工作：每次请求生成大 JSON、读取文件、计算签名、写详细日志、同步发送 metrics、保存每个响应体。这些都可能让压测器比服务端更忙。真正要测服务端时，客户端热路径应该尽量简单；如果业务确实要求签名或 payload 生成，也要把这部分成本单独报告。

面试里可以这样回答：

```text
判断压测客户端是不是瓶颈，要看客户端资源和目标负载是否对得上。客户端 CPU 接近 100%、内存 swap、网卡打满、文件描述符不足、dropped iterations 增加、scheduled 请求发不出去，都说明压测器可能在限速。服务端很空但客户端 latency 高，也要怀疑客户端。验证办法是用 no-op 服务测压测器上限，把负载拆到多台压测机，看吞吐是否上升，并 profile 压测脚本本身。压测报告里要同时写 offered load、sent load、completed load 和客户端资源。
```

## Q023. 如何识别日志输出影响了 benchmark？

日志很容易污染 benchmark，因为它看起来只是“多打印一点信息”，实际会引入格式化、分配、锁、系统调用、磁盘或管道 I/O。Go testing 文档里有一个细节：benchmark 中的 `Log` / `Logf` 会被打印，避免性能依赖 `-test.v`。这意味着把 `b.Logf` 放在 benchmark 热路径里，几乎一定会改变结果。

日志影响 benchmark 的信号通常很明显：

- CPU profile 里出现 `fmt.Sprintf`、JSON encoder、logger formatter、time formatting、caller lookup。
- heap profile 里字符串、byte buffer、日志字段对象分配很高。
- mutex profile 里 logger 的全局锁、stdout/stderr 锁、文件 writer 锁很宽。
- 系统调用里 `write`、`fsync`、管道阻塞增多。
- p99 和日志 flush、rotate、stdout 收集器抖动对齐。
- 关闭日志或把输出改成 `io.Discard` 后，吞吐和 p99 明显变化。

识别方式要做对照，而不是凭感觉删日志。可以跑四组：日志关闭、日志采样、日志写 `io.Discard`、日志写真实文件或 stdout。如果 `io.Discard` 仍然慢，说明格式化和字段构造已经很贵；如果真实文件才慢，说明 I/O 或锁是主要问题。还可以把日志 level 固定，避免一个版本走 debug，一个版本走 info。

端到端压测里还要看日志收集链路。容器 stdout 会被 runtime、日志 agent、宿主机文件系统处理。服务本身写日志很快，不代表整条链路不阻塞。日志量太大时，stdout pipe、sidecar、agent、磁盘都可能反过来影响服务线程。

在 LogServe 里，日志污染可能发生在 WAL append、scheduler decision、worker 状态变更这些高频路径。调试时可以打开详细日志；正式 benchmark 应该固定日志级别，记录日志 bytes/s，把关键事件改成计数器或采样日志。否则最后测到的可能是日志系统，不是共享日志或调度器。

面试里可以这样回答：

```text
识别日志影响 benchmark，我会看 profile 和做日志级别对照。CPU profile 如果出现大量格式化、JSON 编码、caller lookup，heap profile 如果出现日志字段分配，mutex profile 如果看到 logger 锁，或者系统调用里 write/fsync 很高，就要怀疑日志。然后跑 logging off、sampled logging、io.Discard、真实文件/stdout 四组对照。正式 benchmark 要固定日志级别和输出路径，并报告日志量；不要把 debug 日志留在 hot path 里。
```

## Q024. 如何识别 GC 对 benchmark 的影响？

GC 对 benchmark 的影响通常有两种：一种是平均成本，程序不断分配，GC 消耗 CPU；另一种是尾延迟，某些 GC 阶段、辅助标记或堆增长让请求在特定时间段变慢。只看平均延迟容易漏掉第二种。

Go 里可以从几条线索判断。第一条是 benchmark 输出的 `B/op` 和 `allocs/op`。`testing.B.ReportAllocs` 或 `-benchmem` 会打开分配统计。如果某个优化让 `ns/op` 变好但 `B/op` 翻倍，后续在高并发或长时间运行下可能被 GC 吃回来。

第二条是 runtime 指标。Go 的 `GODEBUG=gctrace=1` 会在每次 GC 打印一行摘要，包括回收量和暂停时间；runtime 文档也说明相关字段可以对应到 `runtime/metrics`。`runtime/metrics` 里有 GC cycle、heap、pause 等指标，可以在压测窗口里记录成时间序列。p99 尖峰如果和 GC cycle、heap goal、pause 或 assist 时间对齐，GC 就是重要嫌疑。

第三条是 profile。CPU profile 里 `runtime.mallocgc`、scan、sweep、write barrier 相关栈变宽，说明 GC 或分配路径在吃 CPU。heap profile 的 `alloc_space` 能找到累计分配来源，`inuse_space` 能找活对象和缓存保留。两者要分开看：累计分配高不一定泄漏，in-use 高才更像对象保留或 cache 无界增长。

第四条是参数对照。可以固定 workload 后改 `GOGC` 或临时用 `debug.SetGCPercent` 做诊断：如果提高 GOGC 后吞吐上升、GC CPU 降低但内存上升，说明之前 GC 压力很强；如果降低 GOGC 后 p99 变差，也能说明 GC 参与了尾延迟。但这只是诊断，不等于生产应该关 GC 或随便调大 GOGC。

在 LogServe 里，我会重点看 workflow state、trace/log buffer、result ref、checkpoint cache 和序列化临时对象。GC 影响不一定来自业务对象本身，很多时候来自每次调度都创建临时 slice/map，或者高频日志构造字符串。

面试里可以这样回答：

```text
识别 GC 影响，我会同时看 -benchmem、runtime/metrics、gctrace、CPU profile 和 heap profile。B/op、allocs/op 上升说明有分配压力；p99 尖峰和 GC cycle 或 pause 对齐说明 GC 影响尾延迟；CPU profile 里 runtime.mallocgc 等栈变宽说明分配和 GC 在吃 CPU；heap profile 要区分 alloc_space 和 inuse_space。GOGC 对照可以帮助判断敏感性，但不能把关 GC 当作正式结论。
```

## Q025. 如何识别文件系统 cache 对 benchmark 的影响？

文件系统 cache 会让 I/O benchmark 出现很大的错觉。第一次读 segment 走磁盘，第二次读可能直接走 page cache；第一次打开目录要查 inode/dentry，后面这些元数据也可能已经在 cache 里。你以为 replay 速度变快了，可能只是数据已经被操作系统缓存。

最直观的信号是重复运行越来越快。第一次冷启动慢，第二次、第三次明显变快，磁盘读吞吐下降但应用吞吐上升，说明 page cache 参与了。也可以看系统指标：page fault、major fault、磁盘 read IOPS、读延迟、iowait、block device queue depth。如果应用报告读了 10GB，但磁盘层几乎没读，数据大概率来自缓存。

Linux 官方文档提供了 `drop_caches` 用于测试和调试：写 `1` 可以丢 page cache，写 `2` 丢 dentries/inodes，写 `3` 两者都丢。文档也明确说这不是控制 cache 增长的机制，而且会带来性能问题，不建议在测试或调试之外使用。真正做 benchmark 时，要把这件事写清楚，不能偷偷清 cache 后把结果当线上表现。

设计上要把缓存状态分开：

- cold-cache：重启进程、清应用 cache、必要时用测试环境清 page cache，测首次启动或灾难恢复。
- warm-cache：显式 warm-up 到读延迟和 hit ratio 稳定，测稳定状态。
- mixed-cache：按生产命中率构造热数据和冷数据，测容量规划。

还要控制数据集大小。如果数据集远小于内存，几轮之后几乎都是内存读；如果目标是测磁盘恢复，就应该让数据集大于可用 page cache，或者轮换多个数据集。使用 `O_DIRECT` 可以绕过 page cache，但会改变 I/O 语义和对齐要求，不适合拿来直接代表普通文件 I/O。

在 LogServe 里，文件系统 cache 会影响 WAL replay、segment scan、index lookup、checkpoint load。报告 replay speed 时必须写清楚 segment 是否已经被 page cache 预热，否则 “1GB/s replay” 可能只是内存扫描速度。

面试里可以这样回答：

```text
识别文件系统 cache 影响，我会做冷缓存、热缓存和混合缓存三组对照，并观察磁盘层指标。如果应用读很多数据但磁盘 I/O 很低，或者第二轮比第一轮快很多，就要怀疑 page cache。Linux 的 drop_caches 可以在测试环境里辅助构造冷缓存，但不能在生产随便用，也不能把清 cache 后的结果泛化。报告里要写数据集大小、内存大小、是否 warm-up、是否清 page cache、磁盘读吞吐和 major page fault。
```

## Q026. 如何设计 crash recovery benchmark？

crash recovery benchmark 先看正确性，再看速度。系统崩溃后如果恢复出错、丢记录、重复执行、破坏索引，恢复再快也没有意义。正确的顺序是：先定义崩溃点和恢复不变量，再测恢复时间、扫描量和资源消耗。

崩溃点要覆盖状态转换边界。以 LogServe 这类 log-first 系统为例，至少要考虑这些位置：

- record 已写入用户态 buffer，但还没进入内核。
- write 完成，但 fsync 还没完成。
- fsync 完成，但内存 index 还没更新。
- index 更新了，但 checkpoint 还没落盘。
- checkpoint 文件写了一半，rename 还没完成。
- result ref 已生成，但对象内容还没写完。
- worker 执行完成，但 completion event 还没 append。

每个 crash point 都要有明确预期：哪些记录允许丢，哪些必须存在；哪些任务允许重试，哪些不能重复对外生效；恢复后 sequence、checksum、segment boundary、index、workflow state 是否一致。这里最好用 kill -9、进程崩溃注入、磁盘错误注入、截断文件、损坏尾部 record 等方式构造，而不是只测 clean shutdown。

指标可以分几层：

| 指标 | 含义 |
| --- | --- |
| recovery_time | 从进程启动到可以接收请求的时间 |
| replay_time | 扫描 log 并重建状态的时间 |
| bytes_scanned / records_replayed | 恢复工作量 |
| invalid_records_detected | checksum 或尾部损坏识别数量 |
| duplicate_or_lost_events | 正确性风险，必须为 0 或符合设计说明 |
| time_to_first_workflow_ready | 控制面恢复可用性 |
| CPU / IO / heap / GC | 恢复过程资源成本 |

benchmark 还要分数据规模。只测 1000 条日志没有意义。应该覆盖 1 segment、很多 segment、带 checkpoint、无 checkpoint、尾部损坏、大量未完成 workflow、大量 result ref、冷 page cache、热 page cache。每一组都要保存恢复前后状态校验结果。

面试里可以这样回答：

```text
crash recovery benchmark 要先定义恢复不变量，再测速度。我会在 append、fsync、index update、checkpoint rename、result write、completion event 这些边界注入 kill 或文件损坏，然后验证恢复后没有非法丢失、重复执行、checksum 漏检或状态错乱。指标包括 recovery_time、replay_time、扫描字节数、重放记录数、检测到的损坏记录、time_to_first_ready、CPU/IO/heap。clean shutdown 不等于 crash recovery，必须测真实崩溃路径。
```

## Q027. 如何设计 replay speed benchmark？

replay speed benchmark 测的是从持久化事件重建内存状态的速度。它和 crash recovery benchmark 有重叠，但重点不同：crash recovery 更关心崩溃边界和正确性；replay speed 更关心不同数据规模、checkpoint 间隔、编码格式、索引策略和缓存状态下的重建吞吐。

输入数据要可控。至少要配置这些维度：

- log 总大小：例如 1GB、10GB、100GB。
- record 数量和平均大小：小记录多、大记录少，性能完全不同。
- segment 数量和 segment size。
- workflow 状态复杂度：未完成 workflow、已完成 workflow、长 DAG、短 DAG。
- checkpoint 间隔：无 checkpoint、每 1 万条、每 100 万条。
- 编码和压缩：JSON、protobuf、自定义二进制、是否压缩。
- index 状态：有 index、index 丢失需要重建、index 部分损坏。
- cache 状态：page cache 冷/热，应用 cache 冷/热。

指标不要只写“总耗时”。我会同时记录 records/s、MB/s、segments/s、time_to_first_ready、full_replay_time、state_objects_rebuilt、allocs/op 或总分配、peak heap、GC time、磁盘读吞吐、校验失败数。对于线上系统，time_to_first_ready 有时比 full replay 更重要，因为可以先恢复可接收请求的最小状态，再后台补齐冷数据。

replay benchmark 还要避免把 page cache 当成算法收益。测 WAL replay 时，如果第二轮直接从内存读，MB/s 会漂亮很多，但这个结果不能代表冷启动恢复。报告里要明确 cold-cache 和 warm-cache 两组。还要固定随机种子和数据生成器，避免一次全是小 workflow，另一次混入大量大 workflow。

在 LogServe 里，我会设计这些场景：只有 shared log、shared log + checkpoint、checkpoint 损坏回退到 log、尾部 partial record、100 万个小 workflow、少量超大 workflow、冷热 result ref 混合。每组都校验 replay 后 materialized state 与基准状态一致。

面试里可以这样回答：

```text
replay speed benchmark 要固定日志规模、record 分布、segment 数、checkpoint 间隔、编码格式、index 状态和 cache 状态。指标包括 full_replay_time、time_to_first_ready、records/s、MB/s、扫描字节数、重建对象数、峰值 heap、GC、磁盘读吞吐和校验错误数。要分冷 page cache 和热 page cache 报告，不能把第二轮内存命中当成恢复算法变快。正确性校验也要保留，否则 replay 快但状态错没有价值。
```

## Q028. 如何设计 cache eviction benchmark？

cache eviction benchmark 要测策略在压力下的取舍，而不是只测一个命中率。LRU、LFU、TTL、size-aware、cost-aware、分层 cache 面对不同 workload 会有完全不同的表现。均匀随机请求通常测不出差异，因为它把热点、扫描和阶段变化都抹平了。

workload 至少要覆盖几类：

- 稳定热点：少数 key 被高频访问，测基本 hit ratio。
- Zipf-like 分布：更接近很多线上 key 热度。
- scan workload：大量一次性访问，测 cache pollution。
- phase shift：热点从 A 集合切到 B 集合，测适应速度。
- mixed object size：小对象和大对象混合，测 byte hit ratio 和容量浪费。
- mixed cost：有些 miss 很便宜，有些 miss 要读磁盘或远端对象存储。
- write/update/delete：测 stale entry、失效传播和并发一致性。

指标也不能只看 request hit ratio。还要看 byte hit ratio、weighted hit ratio、miss penalty、p99 latency、eviction rate、admission rate、stale hit、cache memory、metadata memory、锁竞争、GC、后台清理成本。一个策略 request hit ratio 高，但总是保留大对象导致内存膨胀，未必更好。另一个策略 byte hit ratio 高，但让小的高频控制面数据频繁 miss，也可能伤害 p99。

测试流程要分 warm-up 和 measurement。warm-up 用来把 cache 带到目标状态，measurement 才统计正式指标。容量要分多档，比如 cache 能容纳 1%、10%、50% 工作集，才能看策略曲线。并发场景要单独测，因为 eviction 常常涉及全局锁、分片锁、引用计数或后台 goroutine。

在 LogServe 里，cache eviction 可能对应 checkpoint cache、segment index cache、workflow state cache、result metadata cache。benchmark 要把“命中后省了什么”写清楚：省磁盘读、省反序列化、省对象存储请求，还是省调度状态重建。不同成本下，最好策略可能不同。

面试里可以这样回答：

```text
cache eviction benchmark 要用有热点、扫描、阶段变化、对象大小差异和 miss 成本差异的 workload。指标要看 request hit ratio、byte hit ratio、miss penalty、p99、eviction rate、stale hit、内存、GC 和锁竞争。流程上先 warm-up，再进入正式统计；容量要扫多档；并发下还要看 eviction 元数据是否成为锁瓶颈。只用均匀随机 key 测 LRU/LFU，基本测不出生产风险。
```

## Q029. 如何设计 fault injection benchmark？

fault injection benchmark 要把故障变成可重复的实验变量。它不是“随便把系统搞坏看看”，而是定义故障类型、注入时间、持续时间、概率、影响范围和预期恢复行为，然后测系统在故障期间和故障后的表现。

故障可以从小到大设计：

- latency：依赖服务延迟增加、fsync 变慢、对象存储慢响应。
- error：HTTP 5xx、RPC error、磁盘写失败、ENOSPC、EIO。
- drop/timeout：网络包丢失、连接超时、依赖无响应。
- crash：worker kill、scheduler kill、主进程 kill -9。
- corruption：尾部 log partial write、checksum 错、index 文件损坏。
- resource pressure：CPU 限制、内存压力、文件描述符耗尽、线程池耗尽。

工具要选可控的。Linux kernel 有 fault injection 框架，可以注入 slab/page allocation、block request 等失败；Grafana 的 xk6-disruptor 文档也说明它给 k6 增加故障注入能力，用于延迟和响应错误等场景。自研系统也可以在 storage、network client、executor、scheduler 边界加 fault hook。关键是故障要被记录：什么时候注入、命中了多少请求、实际错误类型是什么。

benchmark 指标要覆盖故障前、故障中、故障后：

| 阶段 | 关注点 |
| --- | --- |
| fault-free baseline | 正常吞吐、p99、错误率 |
| fault active | 降级吞吐、timeout、retry、queue growth、backpressure、rejection |
| recovery | backlog drain time、time_to_healthy、重复执行、数据一致性 |

每次只改一个主要故障。比如先测对象存储 500ms 延迟，再测 5% 5xx，不要一开始同时加延迟、错误、丢包、kill。组合故障当然要测，但应该在单故障行为清楚之后再做。还要确认故障真的打到了目标路径，不然 benchmark 可能只是跑了一次正常压测。

在 LogServe 里，fault injection benchmark 可以测：WAL fsync 延迟、segment 尾部损坏、Python executor 卡住、mock LLM 5xx、对象存储写失败、worker kill、scheduler restart。判断标准不是“没有报错”，而是是否限流、是否重试、是否产生重复 completion、是否能 replay 恢复、p99 和 backlog 是否在故障解除后回到基线。

面试里可以这样回答：

```text
fault injection benchmark 要把故障定义成可控变量：类型、概率、持续时间、影响范围、注入点和预期行为都要写清楚。先跑 baseline，再注入单一故障，记录故障期间的吞吐、p99、timeout、retry、backlog、rejection，故障解除后看恢复时间、重复执行和数据一致性。工具可以用 Linux fault injection、xk6-disruptor，或者在系统边界加 fault hook。最重要的是确认故障真的命中目标路径，并且不要一开始混合太多故障。
```

## Q030. benchmark 中 mock 组件能说明什么，不能说明什么？

mock 组件能说明机制，不能直接说明真实生产性能。这个边界必须说清楚。mock 的价值是可控、便宜、稳定，适合把一个变量固定住，专门观察系统里的另一个变量。比如把 LLM 服务 mock 成固定延迟，就能更清楚地看 workflow scheduler、shared log、重试、backpressure 的行为。

mock 能说明这些事情：

- 控制面逻辑是否正确：调度、重试、超时、幂等、状态机转换。
- 某个机制在理想依赖下的开销：append、replay、cache、scheduler decision。
- 对特定故障的反应：固定 5xx、固定 timeout、固定慢响应。
- 算法相对比较：同一 mock workload 下 A 策略是否比 B 策略少排队。
- 回归检测：代码变更是否让基础路径变慢或分配增加。

mock 不能说明这些事情：

- 真实依赖的尾延迟。真实 LLM、数据库、对象存储、网络都可能有重尾和突发。
- 协议和序列化成本。如果 mock 绕过 HTTP/gRPC/TLS，就测不到连接池、TLS、header、body copy。
- 资源竞争。真实依赖会和系统抢 CPU、内存、网络、磁盘，mock 可能不会。
- 错误分布。真实错误不是固定 1%，常常和流量、时间、租户、请求大小相关。
- 反馈效应。真实依赖慢了会引发重试、排队、限流、熔断，简单 mock 不一定能复现。
- 线上容量结论。mock 下能跑 5000 workflows/s，不代表接真实模型服务也能跑。

mock 也分层次。noop mock 只能测框架开销；fixed-latency mock 能测调度等待；distribution mock 能模拟 p50/p99；trace-driven mock 更接近生产；faultable mock 可以测失败恢复。越接近真实，实验越有解释力，但也越复杂。报告里应该写清 mock 类型、延迟分布、错误分布、是否走真实协议、是否消耗真实资源。

在 LogServe 里，如果使用 mock LLM，我会把结论写成“验证 workflow runtime、shared log、replay、调度和 backpressure 机制”，不会写成“证明真实 LLM 服务性能达标”。真实 LLM 接入后还要重新测 streaming、batching、GPU queue、token 长度分布、网络、超时和模型服务错误。

面试里可以这样回答：

```text
mock benchmark 能证明机制在受控依赖下是否工作，也能做回归和 A/B 对比。它不能直接证明真实依赖下的生产性能，因为真实系统有尾延迟、协议成本、资源竞争、错误相关性和反馈效应。使用 mock 时要写清类型：noop、固定延迟、分布延迟、trace-driven、faultable mock。比如 mock LLM 可以说明 LogServe 的调度、日志、replay、重试机制，不足以说明接真实 LLM 后的端到端容量和 p99。
```

## Q031. 如何把单机 benchmark 结论外推到多机？

单机 benchmark 只能给多机系统提供一部分信息。它能说明单节点上某条代码路径、某个存储介质、某个调度器实现的局部成本，但不能直接推出集群吞吐和 p99。多机之后会多出网络、分片、复制、协调、负载均衡、数据倾斜、故障恢复、跨节点排队这些变量。把单机吞吐乘以机器数，通常只是一个乐观上限。

外推时先问一个问题：单机实验测到的瓶颈在多机里是否仍然是瓶颈。比如单机测到 WAL append 能到 200k records/s，这只能说明本地 append 不是明显瓶颈；多机后如果每条 workflow 要跨节点复制、等待 quorum、更新全局索引，瓶颈可能变成网络 RTT、leader CPU、replication log、对象存储或协调服务。局部优化没有消失，但它不再决定整体上限。

比较稳的做法是分层外推：

- 先把单机结果当成 per-node upper bound，而不是集群结论。
- 列出多机新增成本：网络 RTT、序列化、复制、分区路由、跨节点调度、故障检测、重试。
- 做 1、2、4、8 节点 scaling curve，分别看吞吐、p99、错误率和资源利用率。
- 区分 strong scaling 和 weak scaling。前者是总 workload 固定、机器变多；后者是每台机器 workload 固定、总 workload 随机器数增加。
- 检查负载是否真的可分片。hot tenant、hot key、单 leader、全局锁都会破坏线性扩展。
- 报告 per-node 指标和 cluster-level 指标。只看集群吞吐会藏掉某个节点打满。

尾延迟尤其不能直接外推。一个请求如果要经过多个节点，总延迟更像多段延迟的组合；只要其中一个节点慢，整个请求就慢。节点数增加后，遇到慢节点、网络抖动、重试、队列倾斜的概率也会上升。单机 p99=50ms，不代表多机 p99 仍然接近 50ms。

对 LogServe 这类当前以单机/多进程机制验证为边界的系统，我会这样说：单机 benchmark 可以支撑“本地 append、replay、调度、cache 的局部成本”这个结论；它不能证明“分布式部署下的复制协议、跨节点恢复、leader failover、网络分区下的 p99”。如果要外推，下一步必须补多节点实验或模拟网络成本，并把外推写成假设，不写成事实。

面试里可以这样回答：

```text
单机 benchmark 外推到多机时，我会把它当成 per-node upper bound，然后逐项加上多机成本：网络、复制、分片路由、协调、负载均衡、故障检测和重试。不能把单机吞吐直接乘机器数。正确做法是跑 1、2、4、8 节点 scaling curve，区分 strong scaling 和 weak scaling，同时看 per-node 资源、cluster 吞吐、p99、错误率和数据倾斜。单机结论能说明局部机制成本，不能证明分布式系统的整体容量和尾延迟。
```

## Q032. 为什么不能轻易把本地 SSD 结果推广到云盘？

本地 SSD 和云盘的 I/O 路径不同，性能模型也不同。本地 NVMe/SSD 通常是机器本地设备，路径短，延迟和吞吐主要受设备、文件系统、内核、队列深度影响。AWS EC2 instance store 文档也明确说，instance store 是物理连接到宿主机的临时块存储，适合 buffer、cache、scratch data 这类临时数据。EBS 则是另一类抽象：EBS 文档反复强调 volume type、provisioned IOPS、throughput limit、实例带宽、queue length、I/O size 都会影响性能。

所以本地 SSD 上的结果不能直接推广到云盘，至少有几个原因。

第一，云盘有明确的 IOPS 和吞吐上限。AWS EBS 文档里不同 volume type 的最大 IOPS、最大吞吐、延迟目标都不同。`gp3`、`gp2`、`io2`、HDD-backed volume 的行为差别很大。你在本地 SSD 上测到 2GB/s 顺序写，不代表某个 EBS volume 也有这个吞吐。

第二，I/O size 会改变结果。EBS 文档说明 SSD volume 的 I/O size 统计上限是 256 KiB，HDD volume 是 1024 KiB；大 I/O 可能被拆分，小的顺序 I/O 可能被合并。benchmark 如果只写“每次 write 1MB”，但不写真实下发到块设备的 I/O size，就很难解释云盘上的 IOPS 和吞吐。

第三，queue length 和 latency 的关系不同。EBS 文档把 volume queue length 定义为 pending I/O 请求数，并指出队列深度需要和 I/O size、latency 校准。为了打满吞吐，你可能需要更高并发；为了控制 p99，你可能需要更低队列深度。本地 SSD 上“队列深一点吞吐更高”的经验，在云盘上可能变成 p99 爆炸。

第四，云盘还受实例侧带宽影响。EBS 文档建议检查 EC2 实例带宽是否成为限制，并使用 EBS-optimized 或当前代实例。本地 SSD benchmark 没有这条网络/虚拟化路径。

第五，耐久性和故障语义不同。instance store 是临时存储，宿主机停止或故障时数据语义和 EBS 不一样。EBS 有持久块存储语义、卷类型、监控指标、故障检查指标。crash recovery 或 fsync benchmark 不能只看速度，还要看持久化语义是否一致。

面试里可以这样回答：

```text
不能把本地 SSD 结果直接推广到云盘，因为 I/O 路径、上限和故障语义都变了。云盘通常有 volume type、provisioned IOPS、吞吐上限、实例带宽、I/O size、queue length 和微突发限制。本地 SSD 上 fsync 1ms、顺序写 2GB/s，不代表 EBS gp3 或其他云盘也能做到。要推广，必须在目标云盘类型和目标实例规格上重测，并报告 IOPS、throughput、queue depth、p99 latency、CloudWatch 指标和文件系统配置。
```

## Q033. 如何用 confidence interval 表达 benchmark 波动？

confidence interval 用来表达估计值的不确定性。benchmark 里常见写法是“中位数 1.23ms，95% CI 为 ±4%”，意思不是下一次一定落在这个区间，而是在当前采样和统计模型下，我们对真实中心位置的不确定性大致有多大。它比只写一个平均值更诚实。

Go 的 benchstat 是一个很好的参考。官方文档说明 benchstat 会基于多次 benchmark 结果计算统计摘要和 A/B 比较，报告中位数、置信区间、p-value 和样本数。它还提醒一个重要边界：统计显著不等于效果大。样本足够多、噪声足够低时，极小差异也可能显著；工程上仍要看变化幅度是否值得。

表达时我会至少写这些字段：

```text
BenchmarkAppendRecord
  old:  820 ns/op ± 3%  (95% CI, n=20)
  new:  690 ns/op ± 2%  (95% CI, n=20)
  diff: -15.9%, p=0.001
  B/op: old 128, new 64
  allocs/op: old 2, new 1
```

端到端压测也可以用 CI，但要注意粒度。不能把一次压测里的 100 万个请求当成 100 万个完全独立的实验样本，因为它们共享同一个机器状态、同一个 GC 周期、同一个负载阶段。更稳的做法是用多个独立 run 或多个稳定窗口，先得到每轮的 p50/p95/p99/吞吐，再对这些 run-level 指标算 CI。p99 本身可以用 bootstrap 或重复窗口估计波动，但要保留 raw data。

CI 还要和图一起看。两个版本的 CI 不重叠，不代表一定有工程价值；CI 重叠，也不代表绝对没有差异。报告里最好同时给效果大小、CI、样本数、p-value、原始数据链接和实验环境。性能结论不是统计数字自动生成的，还要看 profile 和机制解释。

面试里可以这样回答：

```text
我会用 confidence interval 表达 benchmark 的不确定性，比如 “690 ns/op ±2%，95% CI，n=20”。A/B 比较时同时报告 old/new 的中位数、CI、diff、p-value 和样本数。端到端压测不要把单次请求都当成独立实验，最好按多轮 run 或稳定窗口计算 run-level CI。CI 能说明波动范围，但不能替代工程判断；还要看效果大小、profile 证据和复杂度成本。
```

## Q034. 如何判断优化是否值得引入额外复杂度？

判断优化是否值得，核心是把收益和成本都量化。性能优化不是越多越好。一个优化如果让 p99 降低 1%，却引入复杂状态机、更多锁、更多故障模式和更难调试的代码，通常不值得。反过来，如果一个改动让核心路径 CPU 降低 30%、分配清零、代码还更简单，那它很可能值得。

我会看六个方面。

- 效果大小：吞吐、p99、CPU、内存、I/O、成本是否有明显改善。只看平均值不够。
- 覆盖范围：优化是否命中高频路径，还是只优化了一个很少出现的 synthetic case。
- 稳定性：多轮 benchmark、不同 workload、不同数据规模下是否都有效。
- 复杂度：代码分支、状态、锁、缓存失效、调试难度、测试成本是否明显增加。
- 风险：是否影响正确性、恢复、幂等、超时、重试、可观测性。
- 可回滚性：是否能 feature flag、配置开关、快速降级。

一个实用标准是先设门槛。比如 hot path 优化要至少降低 5% CPU 或 10% p99，或者显著降低机器成本；非 hot path 优化门槛更高。小收益不是不能做，但必须低风险、低复杂度。复杂优化要有 profile 证据和 regression benchmark 保护。

在 LogServe 里，假设把 scheduler 从简单队列换成复杂的多级反馈队列。如果它只让平均 latency 降 3%，但代码复杂很多，还可能饿死大 workflow，那我不会急着合入。除非它能证明 queue wait p99 明显下降，公平性没有变坏，timeout 没增加，并且有可解释的 profile 或 trace 证据。

面试里可以这样回答：

```text
我会把性能收益和工程成本放在一起判断。收益看吞吐、p99、CPU、内存、I/O 和机器成本，成本看代码复杂度、状态数量、锁、故障模式、测试成本和可回滚性。优化必须命中真实 hot path，并在多轮、多 workload 下稳定有效。小收益只有在低复杂度时才值得；复杂优化要有 profile 证据、明确的效果门槛和 regression benchmark 保护。
```

## Q035. 如何设计 regression benchmark 防止性能回退？

regression benchmark 的目标不是追求一次极限成绩，而是持续发现性能回退。它要稳定、可重复、覆盖真实 hot path，并且能在 CI 或 nightly 环境里产生可比较的结果。

先选 benchmark 集合。不要什么都放进回归门禁。适合放进去的是：历史上回退过的路径、profile 里的热点、用户敏感路径、核心数据结构、序列化/压缩/append/replay/scheduler 这类基础能力。对 LogServe 来说，候选包括 WAL append、record encode/decode、CRC、segment index lookup、workflow replay、scheduler pick、checkpoint cache hit/miss。

然后分层运行：

- PR 级 microbenchmark：少量、快速、低噪声，主要看 `ns/op`、`B/op`、`allocs/op`。
- nightly component benchmark：更长时间，覆盖 cache、GC、并发、数据规模。
- nightly 或 weekly end-to-end benchmark：覆盖真实请求 mix、p99、错误率、恢复速度。

门禁要基于统计，不要用单次结果。Go 项目可以用 `go test -bench ... -benchmem -count=10` 收集样本，再用 benchstat 比较基线和当前分支。阈值要区分严重程度：比如核心 hot path `ns/op` 退化超过 5% 且统计显著就报警；`allocs/op` 从 0 变成 1 可以直接失败；端到端 p99 退化超过 10% 进入人工审查。

基线也要管理。可以用主分支最近一段时间的滚动基线，也可以用固定 release baseline。滚动基线适合发现短期回退，固定基线适合防止长期慢慢变差。每次 benchmark 都要保存 raw data、环境、commit、配置和图表，否则回归发生时很难复盘。

最后要接受一个现实：性能门禁会有噪声。不要把所有 benchmark 都做成硬失败。可以把低置信度变化标成 warning，把高置信度大退化标成 failure。对云上 CI，硬件噪声更大，适合用专用机器或 nightly 专用环境。

面试里可以这样回答：

```text
regression benchmark 要覆盖真实 hot path，并按成本分层：PR 跑快速 microbenchmark，nightly 跑 component 和 end-to-end。Go 里可以用 go test -bench -benchmem -count 多次采样，再用 benchstat 和基线比较。门禁要看统计显著性和工程阈值，比如 ns/op 退化 5%、allocs/op 从 0 变 1、p99 退化 10%。每次都保存 raw data、环境和 commit。低置信度变化报警，高置信度大退化才阻塞。
```

## Q036. microbenchmark 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

microbenchmark 的核心目标是测一个很小、边界清楚的代码片段在受控输入下的成本。它主要解决性能问题，尤其是每次操作的耗时、分配、吞吐、可扩展性曲线。它可以间接帮助可维护性决策，比如证明一个复杂优化不值得，但它本身不是正确性测试，也不是安全测试。

典型 microbenchmark 会把目标代码放进重复循环，让框架自动调整迭代次数。Go 的 testing 包就是这种模型：benchmark 函数围绕 `b.N` 或新的 benchmark loop 运行目标代码，并可以用 `-benchmem` 或 `ReportAllocs` 报告分配。它适合回答这类问题：一次 record encode 要多少 ns？一次 map lookup 是否分配？一个 lock-free 队列在单线程下是否比 mutex queue 快？

microbenchmark 的价值在于隔离变量。端到端实验里，延迟可能来自网络、磁盘、GC、调度、日志、缓存和业务逻辑；microbenchmark 把范围缩小到一个函数或一个数据结构，方便定位成本来源。它也适合做 regression guard，因为小 benchmark 更快、更稳定。

边界也要说清楚。microbenchmark 跑得快，不代表系统快；microbenchmark 正常，不代表系统正确；microbenchmark 没问题，也不代表没有安全漏洞。比如一个 parser microbenchmark 很快，但没有覆盖恶意输入；一个 append microbenchmark 很快，但 crash 后可能丢数据。这些都要用其他测试补上。

面试里可以这样回答：

```text
microbenchmark 主要解决性能问题，目标是测很小代码路径的单位成本，比如 ns/op、B/op、allocs/op、单线程或并发下的吞吐。它能帮助定位热点、比较实现、做性能回归门禁。它不主要解决正确性或安全性，最多作为辅助证据。一个函数 microbenchmark 很快，只说明这个受控场景下成本低，不能推出端到端 p99、崩溃恢复或安全边界都没问题。
```

## Q037. microbenchmark 的典型适用场景和不适用场景分别是什么？

microbenchmark 适合测边界小、输入可控、结果可重复的代码路径。越接近纯函数或单个数据结构操作，越适合。越依赖网络、磁盘、调度、外部服务和长时间状态演化，越不适合单靠 microbenchmark 下结论。

适用场景包括：

- 编码/解码：JSON、protobuf、自定义二进制格式。
- 校验和、哈希、压缩、加密这类 CPU-heavy 操作。
- 数据结构：heap、queue、ring buffer、map、index lookup。
- 内存分配优化：对象复用、buffer pool、slice 预分配。
- 锁和原子操作的局部成本。
- parser、matcher、路由、调度器 pick 的小步骤。
- regression guard：防止 `allocs/op` 或 `ns/op` 意外上升。

不适用场景包括：

- 需要真实网络 RTT、连接池、TLS、拥塞和丢包的场景。
- 需要真实磁盘、云盘、fsync、page cache 的场景。
- 需要多个服务协作、复制、分片和 failover 的场景。
- 需要长时间 soak 才会暴露的内存泄漏、cache drift、队列慢性堆积。
- 需要真实用户流量 mix、突发和重试风暴的 p99 结论。
- crash recovery、timeout/retry、数据一致性这类跨边界问题。

在 LogServe 里，record encode/decode、CRC、segment index lookup、scheduler pick、checkpoint cache lookup 很适合 microbenchmark。workflow 从提交到完成、WAL fsync、Python executor、对象存储、replay recovery、mock LLM 端到端延迟，就不能只靠 microbenchmark。

面试里可以这样回答：

```text
microbenchmark 适合测小而稳定的代码路径，比如编码、哈希、数据结构操作、分配优化、局部锁成本和调度器某个 pick 函数。不适合直接评价网络、磁盘、云盘、分布式复制、端到端 p99、长时间泄漏、crash recovery 和真实重试风暴。它的定位是局部成本测量和回归保护，不是生产容量证明。
```

## Q038. microbenchmark 和相近概念最容易混淆的边界在哪里？

microbenchmark 最容易和 unit test、profiling、component benchmark、end-to-end benchmark、load test、ablation study 混在一起。它们都可能跑同一段代码，但回答的问题不同。

| 概念 | 主要回答的问题 | 和 microbenchmark 的边界 |
| --- | --- | --- |
| unit test | 结果是否正确 | microbenchmark 看成本，正确性断言不是重点 |
| profiling | 真实程序时间花在哪里 | profile 是观察现有运行，microbenchmark 是构造小实验 |
| component benchmark | 一个模块整体表现如何 | component 范围更大，可能包含 I/O、队列和并发 |
| end-to-end benchmark | 用户视角或业务流程表现如何 | E2E 包含网络、存储、依赖、调度和错误路径 |
| load test | 系统在负载下是否达标 | load test 控制并发/到达率，microbenchmark 控制局部操作 |
| ablation study | 某个设计贡献多少 | ablation 是实验设计方法，可以用 micro 或 E2E 执行 |

一个常见误区是把 microbenchmark 当成 profile。profile 会告诉你真实系统里哪个函数热；microbenchmark 只告诉你某个函数在你构造的输入下有多快。一个函数 microbenchmark 很慢，但真实系统很少调用，未必值得优化。反过来，一个函数 microbenchmark 很快，但在真实系统里被调用十亿次，仍可能是瓶颈。

另一个误区是把 microbenchmark 当成 unit test。benchmark 里可以有轻量校验，防止编译器消掉结果或结果明显错误，但完整正确性应该放在测试里。把复杂断言、网络 mock、随机故障都塞进 microbenchmark，会让 benchmark 变慢、噪声大、语义不清。

面试里可以这样回答：

```text
microbenchmark 的边界在于它回答的是小代码路径的成本，不是系统整体表现，也不是正确性证明。unit test 证明结果对不对，profile 观察真实程序热点，component benchmark 测模块，end-to-end benchmark 测完整链路，load test 测负载下是否达标，ablation study 测某个设计贡献。microbenchmark 可以参与这些分析，但不能替代它们。
```

## Q039. microbenchmark 在高并发场景下可能出现哪些隐藏问题？

高并发会让 microbenchmark 的问题更明显。单线程 benchmark 里很快的代码，放到多 goroutine 或多线程下可能被锁、原子、cache line、调度器、内存分配器拖慢。最危险的是 benchmark 看起来很稳定，但它没有模拟真实并发结构。

常见隐藏问题有这些：

- 锁竞争：单线程没有竞争，`RunParallel` 或真实服务里所有 goroutine 抢同一把锁。
- false sharing：不同线程写不同字段，但字段落在同一 cache line 上，导致 cache line 来回失效。
- atomic 热点：单个计数器、全局序号、统计指标在高并发下变成瓶颈。
- allocator 竞争：单次操作有小分配，单线程不明显，高并发下触发 GC 和 allocator 压力。
- scheduler 影响：goroutine 抢占、P 数量、`GOMAXPROCS`、线程绑定会影响结果。
- 数据倾斜：benchmark 每个 worker 用独立 key，生产里却有 hot key。
- coordinated omission：并发 benchmark 如果每个 worker 等操作完成再发下一个，慢周期可能降低到达率。
- 共享资源缺失：真实系统会共享连接池、队列、日志 writer、metrics registry，microbenchmark 可能全都绕过了。

Go 的 `RunParallel` 可以帮助测并发路径，但它不是万能的。它能让多个 goroutine 并行调用目标代码，仍然需要你设置正确的输入分布、hot key 比例、`GOMAXPROCS`、`-cpu` 参数、每 worker 状态和共享状态。否则你可能测到的是一个过于理想化的并发场景。

在 LogServe 里，如果只 microbenchmark `scheduler.pick()` 的单线程耗时，可能看不到多 worker 同时更新 ready queue 的锁竞争，也看不到 metrics/logging 在热路径上的共享锁。应该补并发 benchmark：固定 hot namespace、多个 worker、真实队列长度、混合任务大小，观察 throughput、p99、mutex profile、block profile 和 allocs/op。

面试里可以这样回答：

```text
高并发下 microbenchmark 容易漏掉锁竞争、false sharing、atomic 热点、allocator 压力、GC、调度器影响、hot key 倾斜和共享连接池/日志/metrics 的争用。Go 的 RunParallel 能测并发调用，但还要设置 GOMAXPROCS、-cpu、key 分布、每 worker 状态和共享状态。单线程 ns/op 很好看，不代表高并发 p99 和吞吐也好。
```

## Q040. microbenchmark 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

microbenchmark 可以暴露一部分边界条件，但不要指望它完整验证崩溃恢复。它更适合测小边界的成本和局部行为：一次 append + checksum、多一次 retry 的额外分配、一次 context timeout 的开销、一次 replay record parse 的成本、一次 cleanup 是否泄漏对象。完整的 crash recovery 仍然要靠 fault injection、集成测试和端到端恢复 benchmark。

在崩溃场景下，microbenchmark 能测这些局部点：

- record encode 后是否包含足够 checksum 和 length 信息。
- partial record 检测成本有多高。
- replay parser 遇到尾部截断是否快速停止。
- checkpoint 元数据校验是否产生大量分配。
- fsync wrapper、rename wrapper、segment rotation 的单位成本。

在重启场景下，它能测初始化和重建的小路径：加载一个 index entry、解析一个 checkpoint block、重建一个 workflow state、恢复一个 timer heap。它不能证明进程 kill 后所有状态都一致，因为那需要真实持久化文件和恢复流程。

在超时场景下，microbenchmark 可以暴露 timer 和 context 使用成本。比如每个请求都创建 `context.WithTimeout`、timer、goroutine 或 channel，单次看起来很小，高并发下会变成分配和 GC 压力。还可以测取消路径是否释放资源，避免 timeout 后 buffer、result ref、task state 被保留。

在重试场景下，microbenchmark 可以测 retry policy 的局部成本：backoff 计算、幂等 key 生成、dedup map lookup、错误包装、指标上报。它也能暴露一个常见问题：benchmark 只测成功路径，失败路径里日志、错误构造和 metrics 反而更贵。真正的重试风暴还需要 open-loop 压测或 fault injection。

面试里可以这样回答：

```text
microbenchmark 在崩溃、重启、超时、重试场景下主要暴露局部边界成本：partial record 检测、checksum、replay parser、checkpoint block 解析、context timeout、timer、retry backoff、dedup lookup、错误构造和 cleanup。它能告诉我这些小路径是否分配过多或成本过高，但不能单独证明 crash recovery 正确。完整结论还要靠真实崩溃注入、恢复校验和端到端 benchmark。
```

## Q041. microbenchmark 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

microbenchmark 的瓶颈最常见来自 CPU、内存分配、缓存行为和锁竞争。它通常不应该主要来自网络或真实 I/O；如果一个所谓 microbenchmark 的主要时间花在磁盘、网络、数据库或远端服务上，那它已经更接近 component benchmark 或 integration benchmark 了。这个边界很重要，否则你以为在测函数成本，实际测的是环境噪声。

CPU 瓶颈最直观。编码、解析、哈希、压缩、加密、排序、正则、调度决策这类逻辑，microbenchmark 很适合测。CPU-bound 的典型信号是 `ns/op` 随算法复杂度、输入大小、分支预测、SIMD、编译器优化变化；CPU profile 里目标函数或其子函数很宽。比如 LogServe 的 record checksum、二进制编码、scheduler pick，都可能是 CPU 主导。

内存瓶颈也很常见。Go 里要看 `B/op` 和 `allocs/op`，`testing.B.ReportAllocs` 或 `-benchmem` 可以打开分配统计。很多 microbenchmark 的差异不是算得更快，而是少分配、少触发 GC、少逃逸到 heap。比如把每次 append 都创建 `[]byte` 改成复用 buffer，`ns/op` 下降可能来自 GC 压力减少，而不是 CPU 算法本身变快。

锁竞争在并发 microbenchmark 中会变成主因。单线程 benchmark 看不出来，一上 `RunParallel` 或 `-cpu` 多档就暴露。典型信号是并发度增加后吞吐不升反降，mutex/block profile 出现热点，p99 比平均值恶化很多。全局 map、全局 metrics counter、共享 logger、单队列 scheduler 都可能在 microbenchmark 里变成锁瓶颈。

I/O 和网络不是不能出现在 microbenchmark 里，但要非常谨慎。如果目标就是测 `fsync` wrapper、系统调用封装、socket serialization 的最小成本，可以写小 benchmark；但结论必须写成“这个局部调用的成本”，不能外推成文件系统、云盘或网络服务性能。真实 I/O 和网络需要更长时间窗口、真实设备指标、并发、队列深度、错误和 p99，已经超出普通 microbenchmark 的边界。

面试里可以这样回答：

```text
microbenchmark 的瓶颈通常来自 CPU、内存分配、cache 行为和锁竞争。CPU-bound 看 ns/op 和 CPU profile；内存问题看 B/op、allocs/op、heap/GC；锁问题要用 RunParallel、-cpu、mutex/block profile 看并发扩展性。I/O 和网络如果成为主因，通常说明这个测试已经不是纯 microbenchmark，而是 component 或 integration benchmark。它可以测局部调用成本，但不能代表真实磁盘、云盘或网络服务的端到端表现。
```

## Q042. microbenchmark 的 correctness test、stress test 和 benchmark 应该分别测什么？

这三类测试不能混在一起。它们可以复用同一批输入，但目标不同，失败信号也不同。

correctness test 测结果是否对。它应该覆盖边界输入、非法输入、随机输入、并发安全、错误路径、幂等性和不变量。对一个 encode/decode 函数，correctness test 要验证 round-trip、checksum、截断数据、版本兼容、空输入、大输入。它不关心 `ns/op`，只关心对不对。Go 的 `TestXxx`、table-driven test、fuzz test 都更适合放 correctness。

stress test 测在压力、极端输入或长时间运行下是否还能保持正确和可用。它关心资源上限、并发冲突、长时间漂移、失败路径和退化方式。比如对一个并发队列，stress test 可以开很多 goroutine、随机 push/pop/cancel、长时间运行，并用 race detector 或不变量检查是否丢元素、重复元素、死锁。它不一定稳定到适合做性能门禁。

benchmark 测成本。它应该尽量控制变量，固定输入分布和环境，排除不必要的断言、日志、随机生成和 I/O。Go 的 `BenchmarkXxx` 或 JMH/Google Benchmark 的 benchmark 更适合回答 `ns/op`、吞吐、`B/op`、`allocs/op`、并发扩展性。benchmark 里可以保留轻量 correctness guard，比如把结果写入全局 sink，或少量校验防止编译器消除，但完整正确性不该塞进热路径。

以 LogServe 的 record codec 为例：

| 类型 | 应该测什么 | 不应该混入什么 |
| --- | --- | --- |
| correctness test | encode/decode round-trip、checksum、partial record、版本兼容 | 性能阈值判断 |
| stress test | 大量随机 record、并发 encode/decode、损坏输入、长时间运行 | 稳定性能结论 |
| benchmark | 固定 record size 下的 ns/op、B/op、allocs/op、吞吐 | 大量日志、随机生成、磁盘 I/O |

面试里可以这样回答：

```text
correctness test 测结果对不对，覆盖边界、错误、并发和不变量；stress test 测压力下是否还正确、是否死锁、泄漏或资源耗尽；benchmark 测单位成本和吞吐，比如 ns/op、B/op、allocs/op。三者可以用同一模块，但不要混成一个测试。benchmark 里只放轻量防优化校验，完整正确性和故障路径要放在 test 或 stress test 里。
```

## Q043. 如果要求从零实现一个简化版 microbenchmark，你会先定义哪些不变量？

从零实现 microbenchmark，第一件事不是写计时器，而是定义不变量。没有不变量，测出来的数字很容易被编译器、运行时、计时器和测试框架污染。

我会先定义这些不变量：

- 目标代码必须被执行指定次数。不能被编译器消除，不能因为结果未使用而变成空循环。
- 计时范围只覆盖目标代码。setup、数据生成、校验、清理不能混进热路径，除非它们就是被测对象。
- 输入在每轮之间可控。固定 seed、固定数据集、明确是否复用对象，避免每轮测到不同 workload。
- 输出要被消费。可以写入 sink、累加 checksum、返回给框架，防止 dead code elimination。
- 每次操作语义一致。不能前几次是冷缓存，后几次变成热缓存，却把它们当成同一种样本。
- 运行时间足够长。单次太短时，计时器精度、调度器抖动、CPU 频率变化会主导结果。
- 多轮结果要保留。不能只记录最好一次；要输出 raw samples，方便算中位数、方差和 confidence interval。
- 并发 benchmark 要固定并发模型。线程数、goroutine 数、共享状态、per-worker 状态、key 分布都要写清楚。
- 资源指标口径固定。是否统计分配、是否开启 GC、是否开启 profile、是否开日志，都要一致。

一个极简框架大概会这么做：先运行 warm-up；然后选择迭代次数，让总时间超过最小测量窗口；正式测多轮；每轮记录 elapsed、iterations、ns/op、可选 bytes/op、allocs/op；最后输出 raw data 和 summary。并发版本还要支持固定 worker 数和 barrier start，避免线程一个个启动带来的偏差。

还要定义失败条件。比如目标函数返回错误、sink 校验不匹配、每轮迭代次数不一致、计时器分辨率不足、样本方差过大、环境元数据缺失，都应该让结果标成不可用，而不是硬输出一个数字。

面试里可以这样回答：

```text
从零实现 microbenchmark，我会先定义不变量：目标代码必须真的执行，结果必须被消费，计时范围只包含被测代码，输入分布固定，setup 不进热路径，运行时间足够长，多轮 raw data 必须保留，并发模型必须明确。还要固定资源口径，比如是否统计分配、是否开启 GC、是否开 profile。没有这些不变量，benchmark 很容易测到空循环、数据生成、日志、调度器抖动或编译器优化。
```

## Q044. microbenchmark 的常见误用是什么，误用后通常会产生什么线上症状？

microbenchmark 最大的误用，是把局部、受控、理想化的结果当成生产结论。它能告诉你一小段代码在特定输入下的成本，但线上系统还有并发、缓存、GC、网络、磁盘、故障、流量分布和长时间运行。误用后，症状往往出现在 p99、资源成本和故障路径上。

常见误用有这些：

- 输入太理想。只测小 payload、热 key、空队列、无错误路径。线上症状是大请求、冷数据、错误请求一来，p99 飙升。
- 忘记消费结果。编译器把目标代码消掉，benchmark 极快。线上症状是“本地测过很快”，上线后 CPU 完全对不上。
- 把 setup 放进循环。测到数据生成、随机数、日志或分配，而不是目标函数。线上症状是优化错方向，真正 hot path 没变。
- 只测单线程。忽略锁竞争、atomic 热点、false sharing。线上症状是并发上来后吞吐不涨，mutex 等待和 p99 上升。
- 忽略分配。只看 ns/op，不看 B/op 和 allocs/op。线上症状是 GC CPU 高、tail latency 抖动、内存成本变大。
- 用 microbenchmark 证明端到端。局部函数快了，但网络、磁盘、队列或下游才是瓶颈。线上症状是发布后 SLO 没改善。
- 只跑一次或拿最好一次。噪声被当成优化。线上症状是回归门禁不稳定，性能结论反复横跳。
- 在 benchmark 热路径里打日志、做断言、读写文件。线上症状是测试结果依赖环境，换机器就不复现。

对 LogServe 来说，一个典型误用是只 microbenchmark scheduler 的 pick 函数，然后宣称 workflow latency 会下降。真实线上可能 queue wait、worker pool、WAL fsync、Python executor、result storage 才是主因。结果就是局部优化合入后代码更复杂，但端到端 p99 没改善，甚至因为锁或缓存状态增加而变差。

面试里可以这样回答：

```text
microbenchmark 常见误用是用理想输入、单线程、热缓存、无错误路径去证明生产性能。还包括忘记消费结果导致代码被优化掉、把 setup 放进循环、只看 ns/op 不看分配、只跑一次、用局部结果证明端到端。线上症状通常是 p99 没改善、并发下锁竞争爆发、GC 增加、真实大请求变慢、SLO 没变化但代码复杂度上升。microbenchmark 应该服务于定位和回归，不能替代 E2E 和压测。
```

## Q045. microbenchmark 在单机和分布式环境中的语义有什么差异？

在单机环境里，microbenchmark 的语义相对清楚：测某个进程、某个 runtime、某个 CPU/内存/锁路径的局部成本。它关注的是 `ns/op`、`B/op`、`allocs/op`、CPU cache、GC、锁竞争、调度器影响。变量虽然不少，但大多还在一台机器的控制范围内。

分布式环境里，microbenchmark 的语义会变窄。它仍然只能说明局部节点上的代码成本，不能说明系统级行为。一个 RPC handler 的 microbenchmark 可以测 handler 内部解析和路由成本，但不能代表跨节点 RTT、负载均衡、连接池、队列、重试、复制、分区、leader failover、时钟漂移和一致性协议成本。

更具体地说，单机 microbenchmark 的操作边界通常是函数、数据结构、内存对象；分布式系统的性能边界通常是一次跨节点请求、一次复制提交、一次任务调度、一次恢复。两者粒度不同。如果把后者强行压成 microbenchmark，就会把最重要的排队和协调成本拿掉。

分布式环境里还有一个统计差异。单机 benchmark 的噪声主要来自本机调度、GC、CPU 频率、cache、后台进程；分布式 benchmark 的噪声还包括网络 RTT、丢包、节点异构、时间窗口不一致、热点分片、远端 GC、云盘抖动、负载均衡策略。局部 `ns/op` 再稳定，也可能被系统级 p99 淹没。

这不代表 microbenchmark 在分布式系统里没用。它很适合给每个节点的局部成本建模：序列化、压缩、checksum、路由表 lookup、一致性协议里的状态机 step、log append 编码、dedup key 生成。然后再用 component benchmark、cluster benchmark 和 fault injection 验证这些局部成本在真实网络和复制路径里是否仍然重要。

面试里可以这样回答：

```text
单机 microbenchmark 测的是本机局部代码路径的单位成本，比如 CPU、分配、锁和 cache。分布式环境里，它仍然只能说明某个节点上的局部成本，不能说明跨节点 RTT、复制、重试、负载均衡、分区、leader failover 和一致性协议的端到端表现。它适合给局部成本建模，但分布式结论必须再用 component benchmark、cluster benchmark、压测和故障注入验证。
```

## Q046. end-to-end benchmark 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

end-to-end benchmark 的核心目标，是在尽量接近真实使用路径的条件下，测量一个完整业务链路从入口请求到最终可观察结果之间的性能、容量和稳定性。这里的“end-to-end”重点不是“代码覆盖了所有函数”，而是“用户或上游系统真正关心的那条路径被完整走通”：入口协议、认证、网关、服务编排、队列、数据库、缓存、外部依赖、异步 worker、结果查询、日志和指标采集，都尽量按真实部署方式参与。

所以它主要解决的是**性能和可靠性边界问题**，尤其是用户可见延迟、吞吐、错误率、超时率、排队时间、资源利用率、恢复后的尾延迟和 SLO 是否满足。它不主要解决纯正确性、安全性或可维护性问题，但会和这些问题有交叉：

- 对正确性：end-to-end benchmark 必须带最基本的正确性断言，否则测出来的只是“系统很快地返回了东西”，不代表业务真的完成。例如要检查 HTTP 状态码、响应 schema、最终状态、持久化记录、事件数量、去重语义和结果一致性。但这些断言是性能实验的护栏，不等价于完整 correctness test。完整正确性仍然应该由单元测试、集成测试、属性测试、故障恢复测试等覆盖。
- 对性能：这是它的主战场。它回答的是“在某个真实流量模型、数据规模、部署拓扑和依赖条件下，完整链路能承受多少负载，p50/p95/p99 怎么变化，错误和超时从哪里开始上升”。
- 对安全性：它通常不会证明系统安全，只是在真实链路必须经过认证、授权、TLS、限流或审计时，把这些成本纳入性能测量。安全漏洞、权限绕过、注入、密钥泄露等仍然需要专门的安全测试。
- 对可维护性：它不能直接证明代码可维护，但能作为性能回归保护。一次重构如果通过了 end-to-end benchmark，至少说明关键链路的用户可见性能没有明显退化；但它不能告诉你设计是否清晰、模块边界是否合理。

官方和一手资料里也能看到这个边界。k6 的 API load testing 文档把测试范围分成 isolated API、integrated APIs 和 end-to-end API flows，其中 end-to-end API flows 是模拟真实交互、把系统作为整体来测，重点放在高频和关键用户场景上；同时它也要求用 checks 验证响应、用 thresholds 表达延迟和错误率目标。这说明 end-to-end benchmark 不是只看吞吐数字，而是把“完整路径 + 正确性护栏 + 性能阈值”放在一起。

放到 LogServe 这类系统里，一个 end-to-end benchmark 不应该只测“append 一条日志有多快”或“某个调度函数调用耗时多少”。更合理的链路是：客户端提交任务或工作流请求，系统写入共享日志，调度模块消费事件，worker 执行任务或调用 Python/LLM mock，结果写回对象存储或状态表，客户端再查询最终结果。这个 benchmark 要回答的是：在给定并发、任务大小、失败率和 worker 数量下，用户从提交到看到结果需要多久，p99 是否可接受，中间是否出现队列堆积、重试放大、结果丢失或重复完成。

面试时可以这样回答：end-to-end benchmark 的目标不是证明每一行代码正确，而是从用户视角验证完整链路在真实约束下的性能和可靠性。它主解决性能问题，同时用轻量正确性断言防止“测到错误路径”；它可以暴露安全和可维护性问题的症状，但不能替代安全审计、单元测试、集成测试和代码评审。

## Q047. end-to-end benchmark 的典型适用场景和不适用场景分别是什么？

end-to-end benchmark 适合用在“单个局部指标已经不够说明问题”的场景。只要瓶颈可能来自多个组件之间的组合效应，就应该考虑从完整链路测一次，而不是只盯一个函数或一个服务。

典型适用场景包括：

1. **关键用户路径的 SLO 验证**。例如登录、下单、支付、任务提交、结果查询、日志写入后可见、工作流完成通知等。这类路径对用户有直接影响，单个组件快不代表整条链路快。
2. **发布前性能回归检查**。当改动涉及协议层、数据库 schema、缓存策略、调度策略、重试逻辑、序列化格式、日志采集或依赖版本时，end-to-end benchmark 可以发现“每个模块单测都过，但整体慢了”的问题。
3. **容量规划和扩容决策**。例如要回答 4 个 worker、8 个 worker、16 个 worker 下系统吞吐如何变化，p99 是否线性变差，数据库或队列是否先成为瓶颈。
4. **跨组件瓶颈定位的第一步**。它不能直接告诉你哪一行代码慢，但能告诉你用户可见延迟是否异常、异常出现在什么负载区间、错误率和排队长度是否一起上升。之后再用 tracing、profiling、数据库慢查询、队列指标继续下钻。
5. **异步系统的端到端时延评估**。很多系统不是同步返回最终结果，而是“先接收、后调度、再执行、再回写”。这种场景下只测入口 QPS 没意义，必须测从提交到最终状态可见的完整耗时。
6. **故障恢复后的用户体验验证**。例如服务重启、worker crash、依赖短暂不可用之后，系统是否能恢复，恢复期间 p99、失败率和 backlog drain time 是否可接受。
7. **真实依赖交互成本评估**。认证、TLS、连接池、DNS、限流、数据库事务、消息队列、对象存储、日志采集、指标上报都会影响完整链路。组件 benchmark 容易漏掉这些成本。

不适合的场景也很明确：

1. **微小算法或数据结构优化的判断**。如果问题是“这个 map 查找和数组二分哪个更快”，应该先做 microbenchmark，而不是搭一套完整系统。
2. **早期功能正确性验证**。功能还没稳定时，end-to-end benchmark 的结果经常被功能 bug、测试数据问题和环境抖动污染。此时更适合单元测试和集成测试。
3. **精确定位 CPU 热点**。end-to-end benchmark 能告诉你慢，但通常不能告诉你具体慢在哪个函数。定位热点要靠 CPU profile、trace、火焰图、数据库执行计划等。
4. **安全性证明**。它可以包含登录和授权路径，但不能替代权限测试、模糊测试、依赖扫描、渗透测试和威胁建模。
5. **每个 commit 都跑的大规模门禁**。完整链路 benchmark 通常慢、贵、波动大，不适合作为所有提交的唯一 CI gate。更常见的做法是：小 benchmark 高频跑，end-to-end benchmark 在夜间、发布前或关键改动后跑。
6. **环境不可控的随手对比**。如果数据库、缓存、云盘、网络、实例规格、数据集和负载模型都不固定，end-to-end benchmark 很容易变成“测环境波动”，而不是测系统差异。
7. **只需要验证某个 mock 逻辑的场景**。过多 mock 的 end-to-end benchmark 只能说明编排框架开销，不能说明真实依赖下的容量。

在 LogServe 中，适合做 end-to-end benchmark 的例子是：批量提交任务后，从写共享日志、调度、worker 执行到结果可查询的完整耗时；或者在 worker 数量变化时观察吞吐、p99、重试次数和 backlog。相反，如果只是想比较日志条目序列化方式、内存队列 push/pop 成本、某个锁实现开销，就不应该用 end-to-end benchmark 起步，因为完整链路会把太多无关因素混进来。

面试回答可以收束成一句话：end-to-end benchmark 适合回答“真实业务链路在真实约束下是否达标”，不适合回答“某个小函数是否最快”或“系统为什么慢到某一行”。它是系统级性能证据，不是万能测试。

## Q048. end-to-end benchmark 和相近概念最容易混淆的边界在哪里？

最容易混淆的地方，是把“测试范围”“负载模型”“验证目的”和“诊断手段”混成一类词。end-to-end benchmark 描述的是**测量范围**：完整链路。load test、stress test、soak test 描述的是**施压方式和目标**。integration test、system test 更偏**正确性验证**。profiling 描述的是**定位手段**。ablation study 描述的是**因果比较方法**。这些概念可以组合，但不能互相替代。

常见边界可以这样区分：

| 概念 | 核心问题 | 和 end-to-end benchmark 的边界 |
|---|---|---|
| microbenchmark | 某个函数、算法、数据结构或局部操作有多快 | 范围很小，环境更可控，适合局部优化；不能代表完整链路用户体验 |
| component benchmark | 单个服务、单个模块或单个依赖的性能如何 | 比 microbenchmark 更接近系统，但仍然没有覆盖完整业务路径 |
| integration test | 多个组件能否按契约协作 | 主要看正确性和接口契约，一般不追求统计稳定的性能结论 |
| system test | 整个系统功能是否符合预期 | 可以是端到端，但不一定是 benchmark；重点通常是功能行为，不是容量和尾延迟 |
| end-to-end benchmark | 完整业务链路在给定负载下的性能和可靠性如何 | 范围是完整链路，目标是性能/容量/SLO，必须带基本正确性检查 |
| load test | 目标负载下系统表现如何 | 是施压类型；可以压单接口，也可以压完整链路 |
| stress test | 超过预期负载后系统如何退化 | 是探索极限和失效模式；可以基于 end-to-end 场景执行 |
| soak test | 长时间运行是否泄漏、漂移或退化 | 是时间维度；可以用 end-to-end 工作流跑几个小时或几天 |
| spike test | 流量突增时系统是否扛得住 | 是流量形态；可作用于端到端链路 |
| synthetic monitoring | 线上持续模拟少量用户请求 | 更像持续探针，频率低、目标是发现可用性问题；不是容量实验 |
| profiling | CPU、内存、锁、I/O 时间花在哪里 | 是诊断工具；通常在 benchmark 发现问题后使用 |
| ablation study | 移除或替换某个因素后差异是多少 | 是实验方法；可以用端到端指标衡量某个组件或优化的贡献 |
| chaos/fault injection | 故障注入后系统是否韧性足够 | 是故障方法；end-to-end benchmark 可以观察故障对用户路径的影响 |

一个典型误区是说“我们做了 end-to-end benchmark，所以就知道瓶颈在哪里”。这不严谨。end-to-end benchmark 给出的是整体表现，比如 p99 从 300ms 变成 2s、错误率从 0.1% 变成 3%、队列长度开始增长；至于根因是数据库锁、连接池耗尽、GC、DNS、日志 I/O、重试放大还是下游限流，需要结合 trace、profile、指标和日志继续查。

另一个误区是把“端到端功能测试”当成“端到端性能 benchmark”。功能测试只要验证一个订单能成功创建，可能只跑一次或少量样本；benchmark 必须定义负载模型、样本量、warm-up、测量窗口、统计口径、硬件和版本信息，并报告吞吐、延迟分位数、错误率和资源使用。两者都可以走完整链路，但可信度要求不一样。

在 LogServe 中，假设有一个测试会提交一次任务、等待结果完成并检查状态为 done，这更接近 end-to-end integration/system test。只有当它进一步定义并发用户数、提交速率、任务大小分布、worker 数量、运行时长、p99、错误率、backlog、CPU/内存/磁盘 I/O 等指标时，才变成 end-to-end benchmark。

面试时可以说：end-to-end benchmark 的边界在“完整链路 + 性能统计”。完整链路但不做统计，是端到端测试；做负载但只压一个接口，是 load test 但不一定 end-to-end；发现整体变慢但不下钻，是 benchmark 结论，不是 profiling 结论。

## Q049. end-to-end benchmark 在高并发场景下可能出现哪些隐藏问题？

高并发下，end-to-end benchmark 最容易出问题的地方不是“能不能发很多请求”，而是“发出来的请求、测到的延迟和真实用户经历是不是一回事”。完整链路越长，隐藏变量越多，结论也越容易被测试工具、数据模型、依赖限额和观测系统污染。

常见隐藏问题包括：

1. **压测客户端先成为瓶颈**。负载生成器的 CPU、内存、网络、端口、TLS 握手、DNS、连接池、序列化、日志输出或指标聚合先满了，服务端还没到极限，测试结果却显示吞吐上不去。k6 的大规模测试文档也强调要关注负载生成器资源，并在需要时采用分布式执行。
2. **closed-loop 模型掩盖排队延迟**。如果每个虚拟用户必须等上一个请求完成才发下一个请求，服务变慢时请求到达率会自然下降，队列不会像真实流量那样继续堆积。这会低估高负载下的 p99 和超时率。open-loop 或 constant arrival rate 更适合模拟外部固定到达率，但也要处理 dropped iterations 等信号。
3. **coordinated omission 污染分位数**。系统暂停或严重变慢时，客户端如果也停止发请求，那么最慢的那段时间没有被充分采样，p99 会显得比真实情况好。端到端链路尤其容易出现这个问题，因为任一组件卡住都会让客户端节奏被动放慢。
4. **测试数据热点不真实**。所有请求都用同一个用户、同一个租户、同一个 key、同一个队列、同一批对象名或同一条工作流模板，会制造锁竞争、缓存热点或唯一键冲突；反过来，如果数据完全均匀，也可能掩盖真实线上头部租户和热点对象的问题。
5. **唯一 ID、任务名、幂等键或测试资源耗尽**。高并发生成数据时，如果 ID 空间、临时目录、对象存储 key、数据库连接、端口或文件句柄没有正确隔离，会出现重复写、覆盖、清理失败和偶发冲突。
6. **连接池和下游 quota 成为隐性限流器**。HTTP client pool、数据库 pool、Redis pool、gRPC channel、对象存储 SDK、外部 API quota 都可能限制吞吐。表面上看是服务端慢，实际是客户端或依赖层排队。
7. **重试放大流量**。高并发下少量超时会触发重试，重试又增加负载，进一步导致更多超时。如果 benchmark 只报告入口 QPS，不报告实际下游请求数、retry count 和 duplicate effects，就会误判容量。
8. **异步队列把延迟藏起来**。入口请求很快返回 accepted，但队列长度持续增长，最终完成时间越来越长。如果只测提交接口 latency，就会误以为系统很快。end-to-end benchmark 必须测最终状态可见时间。
9. **观测系统本身改变性能**。高并发下 trace 全采样、debug 日志、同步日志刷盘、高基数指标标签、过细 histogram bucket 都会显著增加 CPU、I/O 和内存压力。测 benchmark 时要记录观测配置，否则结果不可复现。
10. **分布式压测的时钟和聚合问题**。多个 load generator 的时钟偏移、网络位置不同、分片不均、指标聚合口径不同，会影响延迟和吞吐解释。分布式测试要明确每个 generator 的负载份额、地理位置、版本和资源余量。
11. **尾延迟被链路长度放大**。完整链路里每多一个依赖，就多一个尾部风险。即使每个组件 p99 都看起来还可以，串联之后用户可见 p99 也可能明显恶化。
12. **清理和隔离成本被忽略**。高并发 end-to-end benchmark 会写大量数据库记录、日志、对象和队列消息。如果清理不彻底，下一轮实验会被历史数据、cache 状态、索引膨胀和后台 compaction 影响。

LogServe 里的例子会更具体：所有任务都写同一个 shared log 分区，可能让 append 或 replay 成为热点；worker pool 被少数慢任务占满，导致短任务排队；Python executor 或 mock LLM 的并发限制让调度层看起来慢；结果对象写入失败触发重试，造成重复完成事件；客户端只测 submit latency，却没有测 result-ready latency，于是完全漏掉 backlog。

面试回答要强调：高并发 end-to-end benchmark 的难点不是制造并发，而是避免测到“压测器瓶颈、测试数据假象、排队被隐藏、重试被漏算、观测开销被混入”。可信的报告至少要同时给出吞吐、到达率、完成率、p99、错误率、超时率、重试数、队列长度、资源使用和压测客户端余量。

## Q050. end-to-end benchmark 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

崩溃、重启、超时和重试会把 end-to-end benchmark 从“性能测试”推进到“语义边界测试”。系统在正常路径下跑得快，不代表在请求已经部分生效、客户端已经超时、worker 正在执行、服务突然重启时仍然能给出清晰语义。端到端视角的价值就在这里：它能观察用户最终看到的是成功、失败、重复、丢失、卡住，还是状态不一致。

常见边界条件包括：

1. **请求已生效但响应丢失**。服务端已经写入数据库、共享日志或队列，但在返回响应前崩溃。客户端看到超时后可能重试。如果没有幂等键，就可能产生重复任务；如果有幂等键，还要验证重试能拿到同一个结果或明确状态。
2. **入口 accepted 但后台任务丢失**。同步接口返回成功，只表示请求进入系统；如果队列写入、日志 append、调度状态或 worker 领取之间存在非原子窗口，重启后可能出现“用户以为已提交，但系统不再处理”的情况。
3. **worker 执行成功但完成事件未写入**。外部副作用已经发生，例如文件已写、对象已上传、第三方 API 已调用，但 worker 在记录完成状态前 crash。重试后可能重复执行副作用，或者状态永远停在 running。
4. **完成事件已写入但结果不可读**。系统记录任务 done，但对象存储、结果表或索引还没写成功，用户查询得到空结果。这是典型的状态和数据可见性顺序问题。
5. **超时语义不清**。客户端 timeout 不等于服务端 cancel。请求可能还在后台执行。如果系统没有明确的 cancellation、deadline propagation 和查询接口，用户不知道应该重试、等待还是认为失败。
6. **重试风暴和重复副作用**。短暂故障后，大量客户端、网关、服务端和 worker 同时重试，会把恢复中的系统再次打满。end-to-end benchmark 要观察 retry count、实际下游请求数、重复完成数和恢复时间。
7. **重启后的 replay 和恢复顺序**。依赖日志或事件恢复状态的系统，要验证重启后 replay 是否完整、顺序是否正确、重复事件是否幂等、checkpoint 是否丢数据、未完成任务是否能重新调度。
8. **孤儿任务和悬挂状态**。服务重启时，某些任务可能已经被 worker 领取但租约没有释放，或者状态长期停在 running。需要 lease、heartbeat、visibility timeout 或补偿扫描来处理。
9. **backlog drain time 被忽视**。故障期间入口可能继续接收请求，恢复后队列堆积。只看服务“重新启动成功”不够，还要看积压多久清完，恢复期间 p99 是否持续超 SLO。
10. **部分依赖恢复导致状态分裂**。数据库恢复了但缓存没恢复，队列恢复了但对象存储慢，worker 恢复了但指标系统丢点。端到端 benchmark 会看到用户路径不稳定，但根因需要进一步下钻。
11. **错误分类影响 SLO 解释**。超时、取消、限流、依赖 5xx、业务校验失败和重复请求应该分开统计。混在一个 error rate 里，会让恢复策略和 SLO burn rate 判断失真。
12. **测试框架误把失败隐藏掉**。如果 benchmark 脚本在请求失败后直接退出，或只统计成功请求延迟，不统计失败请求、超时请求和最终未完成任务，就会严重美化故障场景。

在 LogServe 中，比较有代表性的场景是：任务提交时 shared log 已经 append，但客户端没收到响应；scheduler 重启后需要从日志恢复队列；worker 在 Python executor 已产生结果后 crash，但完成事件未写入；对象结果写入成功而状态更新失败；客户端超时后带同一个幂等键重试；mock LLM 依赖短暂超时导致 worker 批量重试。end-to-end benchmark 应该观察最终任务数、重复任务数、lost task 数、result-ready latency、recovery time、backlog drain time、p99 after recovery 和用户可见错误率。

这类 benchmark 的设计通常要先定义故障注入时间点：例如在 30 秒稳定负载后 kill scheduler，在 60 秒重启；或者在 worker 完成外部副作用后、写完成事件前注入 crash。然后定义不变量：已确认提交的任务最终只能完成一次或明确失败；同一幂等键不能产生多个业务任务；完成状态必须能读到结果；重启后 backlog 应该收敛；故障窗口内的错误和超时要被计入，而不是从统计里删掉。

面试时可以这样回答：end-to-end benchmark 在这些场景下暴露的不是单纯“慢”，而是分布式系统最麻烦的边界：请求是否已经生效、重试是否幂等、部分副作用能否补偿、恢复后状态是否一致、积压能否清空、失败是否被正确统计。它能告诉你用户最终经历了什么，但定位原因还要结合日志、trace、profile、队列指标和存储层指标。
## Q051. end-to-end benchmark 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

end-to-end benchmark 的瓶颈不应该先假设成某一种资源。它测的是完整链路，瓶颈可能在服务端，也可能在压测客户端、数据库、队列、对象存储、日志系统、指标系统或网络路径上。比较稳妥的判断方式，是先把它当成一个排队系统来观察：请求在哪里等待，等待时间是否随负载增加而非线性上升，哪个资源先接近饱和，错误和超时是否跟某个资源指标同步出现。

CPU 瓶颈通常表现为服务端或压测端 CPU 长时间接近满载，吞吐不再随并发增加而增加，p99 上升但 I/O 等待不明显。常见原因包括序列化/反序列化、压缩、加密、JSON 处理、规则匹配、调度算法、过重的日志格式化、指标聚合、trace 采样和 GC 辅助工作。Go 的 pprof 文档强调 CPU profile 是采样式的，会显示函数在采样中出现的比例；所以 CPU-bound 的判断不能只看平均 CPU，还要看 profile 里热路径是否集中在业务代码、runtime、序列化库、日志库还是测试脚本本身。

内存瓶颈通常表现为 RSS 持续增长、GC 时间增加、heap profile 里对象保留异常、容器 OOM、swap、缓存淘汰频繁，或者压测工具自己先耗尽内存。k6 的大规模测试文档特别提醒，虚拟用户、响应体、文件上传、JS 模块、custom metrics 和 checks 都会增加负载生成器的内存开销；如果 load generator 开始 swap，测试结果会被污染。服务端也一样：端到端链路里如果为了追踪一次工作流保留过多状态、把大响应体放进内存、在队列里堆积大量任务，p99 会先变差，之后才表现成明显错误。

锁竞争瓶颈通常更隐蔽。CPU 可能没有满，数据库和网络也不满，但吞吐上不去，goroutine/thread 大量阻塞在 mutex、channel、连接池、全局 map、单分区队列、单租户锁、日志 append 锁或指标注册锁上。它在线上常见的症状是低并发正常，高并发突然出现台阶式延迟；增加机器或 worker 后收益很小；少数慢请求会拖住一批请求。识别这类瓶颈要看 block profile、mutex profile、线程状态、队列长度和临界区耗时，而不是只看 CPU。

I/O 瓶颈包括磁盘、数据库、队列、对象存储、日志刷盘和文件系统 cache。症状一般是 I/O wait 上升、fsync 或写放大明显、数据库慢查询增加、队列 ack 变慢、对象上传延迟上升、磁盘队列长度变长。端到端 benchmark 很容易把 I/O 瓶颈误判成业务慢，因为用户看到的只是“结果迟迟不可见”。对于 LogServe 这类以 shared log 和 replay 为核心的系统，append、flush、checkpoint、对象结果写入、恢复扫描都可能把链路拖慢。

网络瓶颈既可能出现在 SUT，也可能出现在负载生成器。k6 的文档把网络吞吐、连接数、临时端口、文件描述符和 TCP 连接错误都列为大规模测试要关注的内容。典型症状是带宽打满、连接 reset、dial timeout、context deadline exceeded、TLS 握手成本过高、跨可用区或跨地域延迟上升。分布式压测里还要区分“真实系统慢”和“压测机到目标系统的网络路径慢”。

更真实的回答是：end-to-end benchmark 的瓶颈通常不是单一资源，而是一个资源先饱和后触发连锁反应。比如数据库连接池排队会造成请求超时，超时触发重试，重试增加 CPU 和网络，日志错误量上升又增加 I/O，最后 p99 和错误率一起爆掉。面试里我会说，判断瓶颈要按 USE Method 看每个关键资源的 utilization、saturation、errors，再结合 tracing 和 profiling 看等待点。单看 p99 或单看 CPU 都不够。

放到 LogServe，可以按这条路径排查：压测端是否还有 CPU/内存/网络余量；入口服务 CPU、GC、连接池是否正常；shared log append 和 flush 是否排队；scheduler 是否卡在全局锁或单队列；worker pool 是否被慢任务占满；Python executor/mock LLM 是否限流；结果写入和查询是否 I/O-bound；日志和指标是否因为高基数标签或同步输出拖慢链路。只有这些证据拼起来，才能说瓶颈主要来自哪里。

## Q052. end-to-end benchmark 的 correctness test、stress test 和 benchmark 应该分别测什么？

这三个词经常被混用，但它们的目标不一样。正确的做法不是拿一个脚本同时宣称“验证正确性、压垮系统、证明性能提升”，而是把同一条端到端业务链路拆成不同实验目标。

correctness test 测的是语义是否正确。它关心的不是快不快，而是完整链路有没有按契约完成。对 end-to-end 场景来说，至少要覆盖这些断言：已提交请求必须产生可追踪的业务 ID；最终状态只能在合法状态机里流转；成功状态必须能读到结果；失败状态必须有原因；同一个幂等键不能创建多个业务任务；事件数量、顺序、去重和最终结果一致；权限、租户隔离和输入校验没有绕过。它可以只用小负载，甚至单请求，因为目标是证明语义。k6 文档里的 checks 就属于这种护栏：它们验证响应状态、字段和业务条件，防止测试脚本把错误响应也当成成功样本。

stress test 测的是系统超过预期负载后如何退化。它不只问“最大 QPS 是多少”，还要问“超过容量后是否有边界”。好的 stress test 会逐步提高到达率或并发，观察 saturation 点、排队增长、错误类型、限流行为、重试放大、依赖故障、资源耗尽和恢复能力。它应该验证系统能不能优雅失败：返回 429 还是全部 timeout，是否保护下游，是否有 backpressure，是否产生重复副作用，负载降低后 backlog 能否收敛。stress test 的输出重点是失效模式和极限容量，而不是漂亮的平均延迟。

benchmark 测的是在明确条件下的可比较性能。它应该固定版本、硬件、内核、运行时、配置、数据集、负载模型、warm-up、测量窗口和统计口径，然后报告吞吐、完成率、p50/p95/p99、错误率、超时率、资源使用、队列长度和客户端余量。benchmark 的目标是回答“这个版本比那个版本是否更快”“这个配置是否达标”“这个容量是否够用”。它必须可复现、可比较，不能只跑一次，也不能只挑最好的一轮。

三者的关系可以这样理解：

| 类型 | 首要问题 | 典型负载 | 主要输出 | 失败意味着什么 |
|---|---|---|---|---|
| correctness test | 语义是否正确 | 小流量、确定性输入 | 状态、结果、事件、不变量 | 功能或契约错误 |
| stress test | 超过容量后怎样退化 | 逐步升压、超过目标容量 | 极限点、错误类型、恢复情况 | 容量边界或保护策略有问题 |
| benchmark | 给定条件下性能如何 | 代表性负载、稳定窗口 | 吞吐、延迟分位数、错误率、资源 | 性能不达标或优化不成立 |

在 LogServe 中，correctness test 可以提交 10 个任务，验证每个任务都经历 submit、append、scheduled、running、done/failed，结果可查询且没有重复完成。stress test 可以把提交速率从 100/s 提到 1000/s，观察 shared log、scheduler、worker pool、结果写入和重试是否失控。benchmark 则应固定任务大小、worker 数、mock LLM 延迟分布和数据集，比较不同调度策略或不同 checkpoint 间隔下的 result-ready p99 和吞吐。

面试回答可以更短：correctness test 证明链路语义对；stress test 找容量边界和失效方式；benchmark 在受控条件下给出可复现、可比较的性能数字。三者可以共用部分脚本，但不能共用结论。

## Q053. 如果要求从零实现一个简化版 end-to-end benchmark，你会先定义哪些不变量？

从零实现 end-to-end benchmark 时，我会先定义不变量，再写脚本。原因很直接：没有不变量，压测脚本很容易变成“发请求、打印延迟、看起来很忙”。端到端 benchmark 的可信度来自两类约束：业务语义约束和测量约束。

业务语义不变量先定这些：

1. **请求身份唯一且可追踪**。每次提交都有 request_id、workflow_id 或 task_id，日志、指标、trace 和结果表能串起来。没有这个不变量，后面无法区分丢失、重复和延迟。
2. **已确认提交的请求必须有终态**。终态可以是 succeeded、failed、canceled 或 timed_out，但不能永久停在 unknown、accepted、running。异步系统尤其要定义最大等待时间。
3. **幂等键语义清楚**。同一个幂等键重试后只能对应同一个业务任务，或者返回明确冲突。不能因为客户端超时重试就创建两份任务。
4. **成功必须对应可读结果**。不能只看状态 done，还要能读取结果，结果内容要符合最小业务断言。
5. **失败必须可解释**。失败要有错误类型，例如 validation、timeout、dependency_error、rate_limited、canceled。全都归为 failed 会让 SLO 和恢复分析失真。
6. **事件顺序和状态机合法**。不能先 done 后 running，不能 failed 后又 succeeded，不能同一任务出现两个互斥终态。
7. **重复、丢失和孤儿任务可计数**。benchmark 结束后要能统计 submitted、accepted、completed、failed、timed_out、duplicate、lost、orphaned。
8. **测试数据隔离**。每轮实验使用独立 namespace、租户、前缀或时间窗口，避免上一轮数据污染这一轮。

测量不变量也要提前定：

1. **时间起点和终点固定**。端到端 latency 是从客户端计划发送时间、实际发送时间，还是服务端接收时间开始算？终点是入口响应、状态 done、结果可读，还是通知送达？这些必须写清楚。对异步系统，我更倾向于报告 submit latency 和 result-ready latency 两个口径。
2. **负载模型固定**。closed-loop、open-loop、constant arrival rate、并发用户数、请求分布、任务大小分布、热点比例都要固定。否则不同实验不能比较。
3. **warm-up 和测量窗口分开**。预热期用于让连接池、JIT、cache、worker、队列进入稳定状态，统计时要单独标记或排除。
4. **错误和超时不能从分位数里消失**。超时请求、被拒请求、最终未完成任务要单独计数，并参与结论解释。只统计成功请求 latency 会把系统说得过好。
5. **客户端不能成为未知瓶颈**。必须记录 load generator 的 CPU、内存、网络、文件描述符、dropped iterations 或未发出的计划请求。
6. **观测开销固定**。日志级别、trace 采样率、指标标签、histogram bucket、profile 开关要记录。改了观测配置，性能数字就不能直接比较。
7. **版本和环境可复现**。代码版本、配置、依赖版本、硬件、内核、容器限制、数据库参数、云盘类型和网络位置都要写进报告。
8. **统计输出保留 raw data**。至少保存每次请求的开始时间、结束时间、状态、错误类型、任务大小、worker、重试次数和最终结果。summary 只能做展示，不能替代原始数据。

如果实现一个极简版本，我会把脚本分成四层：workload generator 负责按计划产生请求；checker 负责验证最终状态和结果；metrics recorder 记录每个阶段的时间和状态；reporter 计算吞吐、分位数、错误率、重复率、未完成率和资源指标。这样做虽然比“for 循环发 HTTP 请求”麻烦，但后续能解释结果。

在 LogServe 的语境里，核心不变量可以写得更具体：每个任务的 append event 必须存在；scheduler 只能调度已 append 的任务；worker 完成后必须写 completion event；done 状态必须能读取对象结果；同一 workflow_id 不能产生两个成功终态；重启后 replay 得到的状态必须和运行时状态一致；benchmark 结束时 backlog 应该回到可接受范围。先有这些不变量，再谈吞吐和 p99，结论才站得住。

## Q054. end-to-end benchmark 的常见误用是什么，误用后通常会产生什么线上症状？

end-to-end benchmark 最常见的误用，是把它当成一种“看起来很完整”的证明。因为它走了真实链路，报告里又有 QPS 和 p99，很容易让人误以为结论比 microbenchmark、profile 或单元测试都更有权威。但端到端实验一旦设计错，误导性也更强。

第一类误用是只测入口响应，不测业务完成。很多异步系统的提交接口很快返回 accepted，但真正的工作还在队列、worker、外部依赖和结果写入后面。如果 benchmark 只测 submit latency，线上症状就是入口看起来很健康，用户却一直等不到结果；队列越堆越长，result-ready p99 飙升，客服或上游开始报“任务卡住”。

第二类误用是只统计成功请求。失败、超时、取消、429、依赖错误和最终未完成任务都被过滤掉，p99 自然好看。线上症状是 dashboard 显示延迟达标，但真实用户错误率上升；SLO burn rate 被低估；事故复盘时才发现最慢的一批请求根本没有进入 latency 分布。

第三类误用是用 closed-loop 结果代表 open-loop 场景。服务变慢时虚拟用户也变慢，到达率下降，排队被掩盖。线上症状是压测报告说“峰值没问题”，真实流量一来却突然排队、超时、重试风暴，因为真实上游不会等系统恢复后再发下一批请求。

第四类误用是测试数据过于干净。所有请求命中 cache，任务大小固定，租户均匀，依赖都用 mock，错误率为 0，数据集很小。线上症状是冷启动慢、热点租户慢、大对象慢、cache miss 慢、数据库索引膨胀后慢，但 benchmark 从未覆盖这些情况。

第五类误用是忽略压测客户端瓶颈。负载生成器 CPU、内存、网络、端口或脚本效率先满了，SUT 实际没有到极限。线上症状是容量被低估，团队以为系统只能撑某个 QPS；或者相反，客户端丢请求、漏统计，让系统看起来更稳。k6 大规模测试文档专门提醒要监控 load generator 的 CPU、内存、网络，并减少脚本自身的资源开销。

第六类误用是把 end-to-end benchmark 当成根因定位工具。端到端实验只能说明用户路径变慢，不能直接证明是 CPU、锁、I/O、网络还是数据库。线上症状是团队围绕一个 p99 曲线猜原因，改了很多配置却没有命中根因。正确做法是 benchmark 发现问题后，用 trace、profile、慢查询、队列指标和资源指标继续定位。

第七类误用是环境和版本不固定。今天在本地 SSD 上测，明天在云盘上测；今天开 debug trace，明天关；今天数据集 1 万条，明天 1 亿条；还把结果放在同一张图里比较。线上症状是性能回归判断反复横跳，优化有没有效果说不清，发布前争论变成“到底测的是代码还是环境”。

第八类误用是把 mock-heavy 的端到端测试当成真实容量。mock LLM、mock 对象存储、内存数据库、无网络延迟、无 TLS、无认证，确实能跑得很好。线上症状是接入真实依赖后吞吐掉一个数量级，p99 变差，限流和超时开始出现。

第九类误用是没有清理和隔离。上一轮实验留下大量数据、缓存、队列消息、对象和索引碎片，下一轮继续测。线上症状是 benchmark 波动大、越跑越慢，偶发唯一键冲突、旧任务被误算、历史 backlog 污染新结果。

在 LogServe 中，这些误用会表现得很具体：只测 append QPS 会漏掉 scheduler 和 worker backlog；只测 mock LLM 会低估真实调用延迟；只看成功任务会漏掉 executor timeout；不记录 workflow_id 会分不清重复完成和正常重试；不测 replay 会误以为重启恢复没问题。面试时我会明确说：端到端 benchmark 的误用通常不会直接表现成“benchmark 报错”，而是线上出现 p99 好看但用户慢、入口健康但后台堆积、压测通过但真实依赖超时、优化有效性无法复现。

## Q055. end-to-end benchmark 在单机和分布式环境中的语义有什么差异？

单机和分布式环境下，end-to-end benchmark 的名字一样，但语义差很多。单机更像“完整流程在一个受控盒子里跑得怎样”，分布式更像“跨进程、跨机器、跨网络和跨依赖的用户路径是否仍然达标”。这不是规模大小的区别，而是测量对象变了。

单机环境的优点是可控。时钟一致，网络路径短，部署拓扑简单，依赖数量少，日志和 profile 容易收集，实验重复性相对好。它适合验证基本语义、建立性能基线、比较局部实现、做早期回归检查。比如 LogServe 在单机多进程模式下，可以测 shared log append、scheduler、worker、Python executor mock 和结果查询的完整路径，确认机制设计和基础性能没有明显问题。

但单机语义有明显边界。它不能充分代表跨机网络、负载均衡、DNS、TLS、连接池放大、跨可用区延迟、分布式存储、真实对象存储、真实队列、节点故障、时钟偏移和多实例竞争。单机测得的 p99 往往更像“机制路径的下界”，不是生产分布式环境的承诺。

分布式环境的语义更接近用户真实路径，但变量也更多。负载可能从多个 generator 发出，请求会经过负载均衡器、网关、多个服务实例、数据库主从、缓存集群、队列、对象存储和观测管道。k6 文档提到，分布式执行常用于模拟多地点流量，或者把负载扩展到单机无法承受的规模；它也要求把负载切分到多个执行段。这里的关键不只是“机器更多”，而是每个 generator 的位置、时钟、资源余量、网络路径和分片比例都会影响结果解释。

语义差异可以分几层看：

| 维度 | 单机 end-to-end benchmark | 分布式 end-to-end benchmark |
|---|---|---|
| 时间语义 | 通常一个时钟源，阶段耗时容易对齐 | 多机器时钟可能有偏移，需要明确用客户端时间、服务端时间还是 trace 时间 |
| 负载语义 | 一个 load generator 或本机脚本，负载可控但规模有限 | 多 generator 分片，必须确认实际 offered load、sent load 和 completed load |
| 网络语义 | 本地回环或局域网成本较低 | DNS、TLS、跨 AZ/Region、LB、NAT、带宽和丢包都会进入测量 |
| 状态语义 | 状态常在本地进程、本地磁盘或单数据库里 | 状态分布在多个节点和依赖，读写可见性、复制延迟、故障恢复会影响结果 |
| 瓶颈语义 | 更容易看到本机 CPU、内存、锁、磁盘瓶颈 | 更容易出现连接池、网络、下游 quota、热点分区、跨节点协调瓶颈 |
| 观测语义 | 日志、profile、trace 容易收齐 | 需要跨节点 trace、统一标签、时钟同步和聚合口径 |
| 故障语义 | 常见是进程 crash、磁盘慢、端口耗尽 | 节点宕机、网络分区、部分依赖恢复、重复调度、跨节点 replay 更重要 |

还有一个容易忽视的差异：单机 benchmark 的“端到端”通常是功能链路端到端；分布式 benchmark 的“端到端”还包括部署链路端到端。比如单机里调用一个本地 mock 对象存储，和分布式环境里跨网络调用真实对象存储，虽然业务步骤名字一样，性能语义完全不同。

因此，单机结论外推到分布式时要很保守。可以说“这个机制在单机没有明显 CPU/锁/内存问题”，不能直接说“生产集群也能达到同样 p99”。要外推，至少要补充网络延迟模型、真实依赖、数据规模、节点数、负载分布、失败注入和观测开销。更稳妥的报告方式是把单机 benchmark 当作 baseline，把分布式 benchmark 当作 deployment validation。

面试里我会这样回答：单机 end-to-end benchmark 更适合验证机制和建立下界，分布式 end-to-end benchmark 才能验证真实部署路径。两者都重要，但语义不能混。单机通过说明设计没有在最小闭环里失败；分布式通过才说明跨节点、跨网络、跨依赖后仍然满足用户可见 SLO。
## Q056. warm-up 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

warm-up 的核心目标，是把“系统刚启动或刚进入负载时的过渡状态”和“我们真正想比较的稳定测量窗口”分开。它主要解决的是性能实验可信度问题：让 benchmark 不被一次性初始化、JIT 编译、缓存填充、连接建立、CPU 频率爬升、数据库页缓存、对象池填充、运行时调度适应、GC 初始行为等因素主导。更准确地说，warm-up 不是让系统变快，而是让测量窗口更接近实验想描述的运行状态。

它主要服务于**性能和实验可重复性**。正确性、安全性、可维护性不是 warm-up 的主目标。warm-up 期间仍然可以做轻量 correctness check，防止系统在预热阶段就跑错路径，但这只是护栏；它不能替代单元测试、集成测试、安全测试，也不能证明设计更可维护。

不同工具对这个问题的处理方式不一样，但思想一致。Google Benchmark 文档明确提到，出于 cache effects 等原因，benchmark 可能需要先运行一段 warm-up，warm-up 结果不计入最终统计，并可以用 `MinWarmUpTime` 或 `--benchmark_min_warmup_time` 控制。JMH 示例里有 `@Warmup` 和 `@Measurement` 两个阶段，说明 JVM benchmark 通常需要把预热迭代和测量迭代分开。pyperf 也有 warmups 和自动校准，默认不会把 warm-up 值当成最终结果。Go 的 `testing` 包不叫 warm-up，但 `b.Loop()`、自动调整 `b.N`、`b.ResetTimer()` 体现了同类思想：setup 或准备阶段不应该混进被测操作的计时。

warm-up 要处理的典型过渡状态包括：

1. **代码执行路径变热**：JIT、分层编译、解释器 profile、分支预测、指令 cache、动态链接、函数内联都可能让前几轮比后面慢，或者偶尔比后面快。
2. **数据路径变热**：page cache、数据库 buffer pool、索引页、应用缓存、DNS cache、TLS session、连接池、对象池、线程池、goroutine pool 会在前几轮逐步建立。
3. **系统资源进入稳定区间**：CPU frequency scaling、容器 CPU quota、GC heap size、内存分配器 arena、runtime scheduler、日志和指标管道都可能需要一段时间才表现稳定。
4. **端到端链路填满队列**：异步系统里，入口服务、队列、scheduler、worker、下游依赖和结果写入都有自己的缓冲。刚开始的低延迟可能只是队列还空，不能代表稳定负载下的 result-ready latency。

需要特别说清楚：warm-up 不是“把不好的数据删掉”。如果线上真实用户会经历冷启动、扩容后首批请求、容器重启、cache miss、连接池重建，那么这些冷态性能也应该单独测。warm-up 解决的是 steady-state benchmark 的测量污染，不是把冷启动问题藏起来。

在 LogServe 中，warm-up 可以先提交一批任务，让 shared log 文件、scheduler、worker pool、Python executor/mock LLM、对象结果写入、查询路径和指标管道进入稳定状态。正式测量窗口从 warm-up 之后开始，报告 submit latency、result-ready latency、吞吐、错误率和 backlog。与此同时，冷启动恢复能力也要单独测，不能用 warm-up 后的漂亮数据代表“重启后用户第一批请求”的体验。

面试时可以这样回答：warm-up 的目标是排除或单独标记启动阶段的瞬态成本，让性能数字描述清楚的运行状态。它主要解决性能实验可信度，不解决业务正确性，也不能拿来掩盖真实冷启动和恢复成本。

## Q057. warm-up 的典型适用场景和不适用场景分别是什么？

warm-up 适合用在系统存在明显瞬态行为，而实验目标又是 steady-state 性能的场景。只要前几轮执行和后续执行的成本不一样，就要考虑预热、单独记录冷态数据，或者把冷态与稳态分开报告。

典型适用场景包括：

1. **托管运行时和 JIT 场景**。JVM、JavaScript、.NET、PyPy 等运行时会根据执行 profile 做优化。JMH 提供 `@Warmup`，就是因为直接测第一轮经常会得到不稳定结果。
2. **缓存敏感的 benchmark**。文件系统 page cache、数据库 buffer pool、Redis/local cache、应用内 LRU、对象池、查询计划缓存都会影响结果。如果目标是测热缓存性能，必须预热；如果目标是测冷缓存性能，就要显式清冷并单独报告。
3. **连接和协议成本明显的系统**。HTTP/gRPC 连接池、TLS 握手、DNS、认证 token、服务发现、负载均衡器连接复用都可能让第一批请求慢。端到端压测通常需要先把连接路径跑起来。
4. **异步队列和 worker 系统**。刚开始队列为空，worker 没被填满，后台线程可能还没启动。正式测量前要让提交、调度、执行、回写路径都进入稳定节奏。
5. **数据库和存储系统**。索引页、数据页、WAL、compaction、checkpoint、对象存储 SDK、连接池和 prepared statement 都会产生前几轮偏差。
6. **硬件和操作系统状态变化明显的场景**。CPU 频率、NUMA locality、页表、磁盘 cache、容器调度、cgroup 限制、内核网络缓冲都会影响前期数据。
7. **要比较两个版本或两个配置时**。A/B benchmark 如果一个版本刚启动、另一个版本已经热起来，比较没有意义。两个版本必须用同样的 warm-up 策略。

不适用或需要谨慎使用的场景也不少：

1. **目标就是冷启动或首请求性能**。例如 serverless cold start、容器扩容后的第一批请求、数据库重启后恢复、移动 App 首次打开。此时 warm-up 会把最重要的用户体验删掉。
2. **崩溃恢复和重启恢复 benchmark**。如果问题是“服务重启后多久恢复可用”，warm-up 只能作为恢复后的稳态测量，不能替代恢复窗口本身。
3. **缓存命中率本身是被测对象**。如果要研究 cache eviction、冷/热混合流量、真实线上 hit ratio，强行预热到 100% 命中会制造假象。
4. **短生命周期任务**。很多 CLI、batch job、一次性函数、短连接请求本来就主要生活在冷态。只报告 warm-up 后数据会偏离实际使用。
5. **系统永远达不到稳定态**。有些负载会不断泄漏、compaction、GC、队列堆积或热 key 漂移。此时不能无限拉长 warm-up 直到数据好看，而要承认系统没有 steady state。
6. **安全、正确性、兼容性测试**。这些测试可以有 setup，但不应该用 warm-up 概念解释失败或跳过异常路径。

LogServe 里适合 warm-up 的例子是：在正式比较两种调度策略前，先让 worker pool、shared log、mock LLM、对象写入和查询路径跑一段固定负载，然后只统计稳定窗口。LogServe 里不适合用 warm-up 抹掉的例子是：进程重启后 replay 需要多久、checkpoint 缺失时恢复是否变慢、第一次提交任务是否因为初始化慢到超时。这些冷态行为本来就应该作为单独实验写进报告。

面试回答可以压缩成一句话：当目标是稳态性能，warm-up 很有用；当目标是冷启动、恢复、cache miss、首请求或短任务体验，warm-up 不能删掉问题，只能把冷态和稳态分开报告。

## Q058. warm-up 和相近概念最容易混淆的边界在哪里？

warm-up 最容易和 setup、ramp-up、calibration、measurement、cool-down、cache priming、cold-start benchmark 混在一起。它们都发生在“正式统计前后”，但语义不一样。

| 概念 | 核心含义 | 和 warm-up 的边界 |
|---|---|---|
| setup | 准备实验环境和输入数据 | setup 是构造条件，例如建表、生成数据、启动服务；warm-up 是让运行状态进入目标区间 |
| warm-up | 运行被测路径但不计入正式结果 | 它可以包含真实请求，也可以只跑预热流量；重点是排除瞬态性能 |
| ramp-up | 逐步增加负载 | ramp-up 控制负载形状，避免瞬间打爆系统；它不一定排除统计，也不一定让系统稳定 |
| calibration | 自动选择循环次数、进程数、样本数量或测量时长 | calibration 是测量工具为了让结果有足够分辨率而调参；warm-up 是稳定运行状态 |
| measurement window | 正式统计窗口 | 这里的结果会进入报告；warm-up 通常不进入最终分位数和均值 |
| cool-down | 测试后让系统排空和恢复 | 用于清理 backlog、释放资源、观察恢复；不是正式性能测量 |
| cache priming | 特意填充缓存 | 它是 warm-up 的一种可能手段，但 warm-up 不只针对 cache，也包括 JIT、连接、GC、线程池等 |
| cold-start benchmark | 专门测冷态性能 | 它和 warm-up 是互补关系；冷态不能被 warm-up 删除 |
| canary/shakedown | 小流量验证部署是否可用 | 主要看部署健康和基本正确性，不等同于性能 warm-up |

Go benchmark 里的 `b.ResetTimer()` 也容易被误解。它的作用是清零已经经过的 benchmark 时间、内存分配计数和用户上报指标，不是让系统自动进入稳态。你可以在 `ResetTimer()` 前做 setup 或预热，但必须清楚：这只是把准备阶段从计时里排除，不能证明后面的状态就是线上稳态。Go 文档也提醒，在 `RunParallel` 里不要调用 `ResetTimer`、`StartTimer`、`StopTimer`，因为这些 timer 有全局影响。这是并发 benchmark 很容易踩的坑。

JMH 的 warm-up、measurement 和 fork 也要分清。`@Warmup` 控制预热迭代，`@Measurement` 控制正式测量迭代；fork 是另起 JVM 进程，避免不同 benchmark 的 profile 混在一起。JMH 示例专门展示过不 fork 时，不同测试的 JVM profile 可能互相污染，导致结果失真。也就是说，warm-up 处理的是“同一实验内部的瞬态”，fork/隔离处理的是“不同实验之间的污染”。

端到端压测里，warm-up 和 ramp-up 的混淆更常见。比如先用 5 分钟把 QPS 从 0 拉到 1000，这叫 ramp-up；如果这 5 分钟不计入正式统计，并且目的是让 cache、连接池、worker 和队列进入稳定状态，它同时也是 warm-up。反过来，如果 ramp-up 本身就是用户峰值到来的场景，那它应该进入统计，不能简单删掉。

LogServe 中的边界可以这样判断：启动服务、创建临时目录、清空旧日志，这是 setup；先提交一批任务让 scheduler 和 worker 活跃起来，这是 warm-up；把提交速率从 100/s 拉到 1000/s，这是 ramp-up；正式记录 result-ready p99 的 10 分钟，是 measurement window；测试结束后等 backlog 清空，是 cool-down；重启后第一批任务的耗时，是 cold-start/recovery benchmark，不应该被 warm-up 淹没。

面试时我会说：warm-up 的边界在“运行真实路径但不把瞬态数据放进稳态结论”。它不是 setup，不是 ramp-up 的同义词，也不是删除坏数据的理由。只要删除某段数据会改变用户体验解释，就必须单独报告那段数据。

## Q059. warm-up 在高并发场景下可能出现哪些隐藏问题？

高并发下，warm-up 本身也会制造问题。很多人以为 warm-up 只是“先跑几分钟”，实际它会改变 cache、队列、连接、GC、线程池、下游限额和负载生成器状态。如果不记录这些变化，正式测量窗口看起来稳定，结论却偏离真实线上流量。

常见隐藏问题包括：

1. **warm-up 把系统推到过热状态**。预热流量过大或持续太久，可能提前触发 GC、compaction、限流、队列堆积、数据库锁等待、cache eviction。正式窗口开始时系统已经带着历史包袱，p99 反而被 warm-up 污染。
2. **warm-up 只预热了好走的路径**。如果预热请求全是小 payload、固定用户、固定 key、cache-friendly 查询，正式混合流量里的大对象、冷 key、慢租户和异常路径仍然是冷的。报告会低估真实 p99。
3. **warm-up 造成不真实的缓存命中率**。线上流量通常有热点分布、周期变化和淘汰过程。预热脚本如果把所有数据扫一遍，可能制造线上永远没有的高命中率。
4. **连接池和下游 quota 被提前占满**。高并发预热可能建立大量连接、TLS session、数据库 session 或 gRPC stream。正式窗口开始时，连接池看起来很热，但也可能已经触发服务端限流或下游连接上限。
5. **压测客户端被预热拖垮**。load generator 的 CPU、内存、JS runtime、指标缓冲、文件描述符、网络端口可能在 warm-up 阶段就接近极限。后续测到的是客户端疲劳，不是 SUT 性能。
6. **warm-up 阶段的错误被忽略**。很多脚本只在 measurement window 里统计错误。预热期间如果已经出现 5xx、timeout、dropped iterations、队列增长，这不是无关信息，而是系统无法顺利进入稳态的证据。
7. **异步 backlog 被带入正式窗口**。入口请求预热结束了，但后台任务还没处理完。正式测量开始后，新请求和旧 backlog 混在一起，result-ready latency 无法解释。
8. **自适应系统被训练到错误状态**。负载均衡、autoscaling、JIT profile、adaptive concurrency、缓存淘汰、调度器优先级可能根据 warm-up 流量学习。如果 warm-up 分布不真实，系统会被调到一个不适合正式流量的状态。
9. **多压测机 warm-up 不同步**。分布式压测中，有的 generator 已经完成预热，有的还在启动，正式窗口的 offered load 不一致。p99 聚合后看不出这一层偏差。
10. **预热数据污染业务状态**。高并发 warm-up 生成大量测试用户、任务、日志、对象和队列消息。如果没有独立 namespace 和清理策略，正式窗口会混入预热数据，后续实验也会被污染。
11. **把 warm-up 当作自动扩容准备时间**。如果系统靠 warm-up 先触发 autoscaler 扩容，再开始统计，报告其实测的是“已经扩容后的性能”。线上突发流量能不能扛住，仍然没被回答。

在 LogServe 中，隐藏问题会更明显。预热可能先把 shared log 写到某个大小，让 page cache 和 replay 路径变热；也可能制造大量未完成任务，让 scheduler 正式测量时已经背着 backlog；mock LLM 的连接和对象结果缓存可能被预热到不真实状态；worker pool 可能被慢任务占满，导致正式窗口一开始就不干净。解决办法不是取消 warm-up，而是给 warm-up 单独设指标：warm-up 期间的错误率、队列长度、GC、连接数、CPU、内存、dropped iterations 和最后 backlog 必须达标，正式测量才能开始。

面试回答可以这样说：高并发 warm-up 最大的风险，是它改变了被测系统本身。预热流量必须和正式流量分布一致，必须确认预热结束时系统没有 backlog、没有错误积累、没有客户端瓶颈，并且要把 warm-up 的资源曲线保留下来。否则所谓稳态，很可能只是被人为训练出来的状态。

## Q060. warm-up 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

崩溃、重启、超时和重试会让 warm-up 的语义变得复杂。正常情况下，warm-up 是为了让系统进入稳定状态；故障场景下，warm-up 可能暴露出系统能不能从冷态恢复、能不能重新建立连接和缓存、能不能正确处理预热期间已经部分生效的请求。

首先是崩溃后的状态污染。warm-up 期间如果服务 crash，已经写入的任务、日志、对象、临时文件和连接状态可能留下半成品。下一次启动后继续 warm-up，系统可能把上一轮残留当成真实任务处理，或者因为唯一键、幂等键、对象 key 冲突而失败。一个严谨的 benchmark 要定义：warm-up 失败后是清空环境重来，还是把它当作恢复测试继续跑。

其次是重启后的冷态和稳态边界。服务重启会清掉进程内 cache、连接池、JIT profile、对象池、线程池、scheduler 内存状态；但文件系统 page cache、数据库 buffer pool、外部服务连接状态、对象存储数据可能还在。于是“重启后冷不冷”并不是二元问题。报告要说明哪些状态被清了，哪些状态保留了。否则同样叫 restart benchmark，结果可能完全不可比。

超时场景里，warm-up 会暴露请求是否已经生效的问题。客户端在 warm-up 期间 timeout，不代表服务端没有继续执行。正式窗口开始前，如果这些超时请求还在后台跑，它们会污染队列和资源。更糟的是，脚本可能把 warm-up 超时忽略掉，只统计后面的成功请求。这样会掩盖系统在进入稳态前已经失败的事实。

重试场景里，warm-up 会暴露幂等和重复副作用。预热请求如果因为超时重试，系统必须保证同一个幂等键不会生成多个任务，外部副作用不会重复执行，结果不会被后一次写覆盖。否则正式测量看起来顺利，事后检查却发现多了任务、多了对象、多了 completion event。

还要关注这些边界条件：

1. **warm-up 是否可中断**。收到 cancel、SIGTERM、进程退出时，预热中的请求是否能停止，还是会继续占用 worker 和连接。
2. **warm-up 失败是否进入报告**。如果预热阶段已经出现 5xx、timeout、OOM、连接耗尽，不能因为“不计入正式窗口”就删除。
3. **重启后是否重新预热**。重启后的系统是否需要重新 warm-up？如果需要，预热时长是否计入恢复时间？这要按实验目标决定。
4. **预热和恢复是否互相混淆**。测 steady-state 时，重启后的重新预热可以排除；测 recovery SLO 时，重新预热本身就是用户等待时间的一部分。
5. **队列和 backlog 是否清空**。warm-up 结束不等于后台完成。必须检查 in-flight、队列长度、未完成任务、重试队列和死信队列。
6. **连接和 session 是否失效**。重启后旧连接、TLS session、HTTP/2 stream、数据库 session 可能变成半开状态，第一批请求会遇到 reset 或重连。
7. **观测系统是否丢失 warm-up 故障**。如果 trace 或指标在 warm-up 后才打开，启动阶段最关键的失败证据会消失。

LogServe 里可以这样设计边界测试：warm-up 期间提交一批任务，然后 kill scheduler；重启后检查 replay 是否把 warm-up 任务恢复到正确状态；再确认正式窗口开始前 backlog 是否归零或达到预设阈值。另一个测试是让 worker 在 warm-up 期间执行到一半超时，客户端重试同一 workflow_id，检查 shared log 里是否只有一个业务任务、是否只有一个成功终态、结果对象是否可读。

面试回答要把边界说清楚：warm-up 在故障场景下不能只当“预备阶段”。它会产生真实副作用，可能留下半完成状态，也可能影响恢复时间。一个可信的实验要规定 warm-up 失败如何处理、哪些状态会被清理、重启后是否重新预热、预热期间的错误是否计入结论。否则 warm-up 会把最重要的恢复问题藏起来。
## Q061. warm-up 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

warm-up 本身不是业务功能，但它会真实消耗资源，所以它也会有瓶颈。判断 warm-up 的瓶颈时，不能只看正式测量窗口的指标，还要单独看预热阶段的 CPU、内存、锁、I/O、网络、队列长度和错误。很多 benchmark 结果不稳定，不是被测系统稳态有问题，而是 warm-up 阶段已经把系统带到一个不干净的状态。

CPU 瓶颈常见于 JIT 编译、代码路径首次执行、序列化、压缩、TLS 握手、规则加载、日志格式化、指标初始化和压测脚本本身。托管运行时里，warm-up 期间 CPU 占用高不一定是坏事，因为 JIT、profile 收集和内联优化本来会消耗 CPU；但如果预热结束后 CPU 仍然接近满载，或者压测客户端 CPU 已经打满，正式 benchmark 就会被污染。Google Benchmark 文档提到 warm-up 可以用于缓解 cache effects 等带来的不稳定，但它不会自动告诉你预热阶段是不是 CPU-bound。

内存瓶颈通常来自缓存填充、对象池扩大、连接池建立、JIT 代码缓存、GC heap 增长、响应体保留、指标标签膨胀和压测工具的 VU 状态。warm-up 的一个常见副作用是：为了让系统“热起来”，脚本生成了大量对象、请求记录、trace span 和测试数据，正式窗口开始时 heap 已经很大，GC 压力也被提前带入。pyperf、JMH、Google Benchmark 都把 warm-up 和测量阶段分开，但分开统计不等于内存状态没有残留。

锁竞争在 warm-up 里也会出现，尤其是全局 cache 初始化、单例加载、连接池填充、指标注册、日志 sink 初始化、schema migration、对象池扩容、调度器首次建队列这些路径。症状是 CPU 没满，但预热时间拉长，多个 worker/goroutine/thread 等同一把锁。正式窗口开始后，如果这些锁只在初始化时出现，问题可能消失；如果锁保护的是长期共享结构，warm-up 反而能提前暴露后续高并发瓶颈。

I/O 瓶颈常见于 page cache 填充、数据库 buffer pool 预热、索引扫描、WAL/redo log、checkpoint、文件加载、模型加载、对象存储读写、日志刷盘。很多系统 warm-up 一开始慢，是因为磁盘和数据库页还冷；这不一定说明稳态慢。但如果 warm-up 需要大量写入并触发 compaction、flush、checkpoint 或索引膨胀，正式窗口会被历史 I/O 影响。

网络瓶颈主要来自 DNS、TLS、连接池建立、服务发现、负载均衡器健康检查、跨区域请求、下游限流和压测机带宽。高并发 warm-up 会快速建立大量连接，如果端口、文件描述符、NAT、LB 或下游 quota 先满了，正式窗口看到的错误可能其实是 warm-up 留下的半开连接或限流状态。

在 LogServe 中，warm-up 的瓶颈可能来自 shared log 文件预热、checkpoint 读取、scheduler 首次恢复队列、worker pool 启动、Python executor/mock LLM 初始化、对象结果写入、指标注册和 debug 日志。面试时我会说：warm-up 的瓶颈既可能来自 CPU/内存/锁/I/O/网络，也可能来自“预热把状态带脏”。所以要单独记录 warm-up 阶段指标，并在正式窗口开始前确认 backlog、错误、GC、连接数和资源使用已经回到预期范围。

## Q062. warm-up 的 correctness test、stress test 和 benchmark 应该分别测什么？

warm-up 也需要分清 correctness test、stress test 和 benchmark。否则很容易把“预热跑过了”误解成“系统正确”“系统抗压”“性能数字可信”。这三个目标不同，测试设计也不同。

warm-up 的 correctness test 主要测预热不会破坏语义。它应该检查：预热请求是否使用独立 namespace；预热数据是否能被标记和清理；预热阶段产生的任务是否有终态；预热失败是否被记录；幂等键是否生效；预热请求不会污染正式测量数据；预热结束时队列、in-flight、重试队列、死信队列是否符合预设条件。对异步系统，还要确认预热流量不是只打入口接口，而是完整走到结果可见。

warm-up 的 stress test 测的是预热机制在高负载或异常条件下会不会伤害系统。比如预热流量突然很大时，会不会把连接池打满、触发下游限流、造成 cache eviction、让后台 compaction 长时间运行、使 scheduler 带着 backlog 进入正式窗口。它的目标不是得到稳态性能数字，而是验证 warm-up 策略本身是否有边界：预热失败时能否停止，资源能否回收，是否会留下半完成任务。

warm-up 的 benchmark 则测预热策略对实验可信度的影响。它要回答的是：预热多久后指标进入稳定区间；预热流量分布不同会怎样影响 p99；预热 30 秒、2 分钟、10 分钟的结果是否一致；冷态、预热态、稳态三组数据差多少；warm-up 是否降低了测量方差，还是只是把异常阶段删掉。Google Benchmark 的 `MinWarmUpTime`、JMH 的 `@Warmup`、pyperf 的 warmups 都是在给测量阶段提供更清晰的边界，但真正可信的报告还要说明为什么这个 warm-up 设置合理。

可以这样划分：

| 类型 | 主要问题 | 应测指标 | 不应得出的结论 |
|---|---|---|---|
| correctness test | 预热是否保持语义和数据隔离 | 预热任务终态、重复数、污染数、清理结果、backlog | 系统性能达标 |
| stress test | 预热在高压下会不会伤害系统 | warm-up 错误率、资源峰值、限流、队列残留、恢复时间 | 稳态性能一定可信 |
| benchmark | 预热策略是否让测量更稳定 | 方差、p50/p99 变化、稳定时间、冷/热差异 | 冷启动体验可以忽略 |

LogServe 中，correctness test 可以检查 warm-up 任务是否都写入 shared log、是否最终 done/failed、是否不混入正式 measurement namespace。stress test 可以故意用过高预热速率，观察 scheduler、worker、mock LLM 和对象存储是否留下 backlog。benchmark 可以比较无 warm-up、固定 30 秒 warm-up、直到 backlog 清零再开始测量三种方案，看看 result-ready p99 和方差如何变化。

面试回答可以概括为：warm-up 的 correctness test 测“预热是否干净”，stress test 测“预热是否会把系统打坏”，benchmark 测“预热是否真的提高测量可信度”。三者都重要，但不能互相替代。

## Q063. 如果要求从零实现一个简化版 warm-up，你会先定义哪些不变量？

从零实现 warm-up，我会先定义不变量，而不是先写一个 `sleep(60s)` 或“先发一批请求”。预热不是等待时间，它是一段有目标、有停止条件、有清理策略的实验阶段。

我会先定义这些不变量：

1. **阶段边界清楚**。每条样本必须标记为 warm-up、measurement 或 cool-down，warm-up 样本不进入正式 latency 分位数，但必须保留在 raw data 里。
2. **预热流量分布明确**。预热请求的接口、payload、租户、key 分布、任务大小、成功/失败比例要和实验目标匹配。不能用全小请求预热，再拿它解释混合负载。
3. **数据隔离可验证**。warm-up 使用独立 namespace、前缀、tenant、workflow_id 或标签。正式窗口统计时能排除预热数据，测试结束后能清理。
4. **预热结束条件明确**。可以是固定时长，也可以是资源和队列达到阈值，例如 CPU 不再爬升、连接池达到目标、backlog 清零、错误率低于阈值、p99 连续几个窗口稳定。不能靠“感觉差不多”。
5. **预热失败必须可见**。warm-up 期间的 5xx、timeout、dropped iterations、OOM、连接耗尽、任务丢失不能被删掉。它们不进正式分位数，但要进入报告。
6. **预热不允许留下不可解释 backlog**。正式测量开始前，要记录 in-flight、队列长度、重试队列、死信队列、未完成任务数。如果有残留，要么等待清空，要么在报告里说明。
7. **幂等和重复语义固定**。warm-up 请求如果超时重试，不应该创建多个业务任务。预热阶段也是真实副作用，不能放松语义。
8. **观测配置在预热和正式窗口一致**。如果 warm-up 开 debug 日志、正式窗口关日志，或者 warm-up 不采 trace、正式窗口采 trace，资源状态会不可比。
9. **冷态数据单独保存**。第一批请求、第一次连接、第一次 cache miss、第一次 replay 的数据要保留。warm-up 只是把它们从稳态结论中分离，不是删除。
10. **可重复运行**。多轮 benchmark 之间，warm-up 不能被上一轮 cache、文件、对象、数据库记录污染。需要清理策略或明确保留状态。

一个简化实现可以分成四步：启动系统和依赖，执行 warm-up 负载，检查结束条件，切换到 measurement。切换点要写入日志和指标，例如 `phase=warmup_end`。如果 warm-up 没有达到条件，正式 benchmark 应该失败或标记为 invalid，而不是继续跑出一份看似完整的报告。

在 LogServe 中，我会定义：warm-up 的 workflow_id 前缀固定；所有预热任务必须达到终态；shared log 中预热事件与正式事件可区分；scheduler backlog 在正式窗口前小于阈值；worker pool 已经启动到目标数量；mock LLM 连接和对象结果路径已走通；预热阶段错误和重试单独计数；正式窗口 result-ready latency 不包含 warm-up 任务。这样实现出来的 warm-up 才是实验阶段，不是随便跑几轮。

## Q064. warm-up 的常见误用是什么，误用后通常会产生什么线上症状？

warm-up 的常见误用，本质上都是把“排除瞬态”变成“删除不想看的数据”。这样做短期会让 benchmark 报告更好看，长期会让线上问题更难解释。

第一种误用是把冷启动删掉。报告只展示 warm-up 后的 p99，却不展示容器重启、扩容、发布、故障恢复后的第一批请求。线上症状是扩容后用户仍然慢，发布后前几分钟错误率高，serverless 或短任务场景体验很差，但 benchmark 一直显示稳态达标。

第二种误用是 warm-up 流量不真实。预热只打小请求、单租户、固定 key、cache-friendly 查询，正式或线上流量却有大对象、热点租户、冷 key、失败路径。线上症状是 benchmark p99 很稳，真实 p99 在热点和大请求上明显恶化。

第三种误用是把 warm-up 当成 ramp-up 或 sleep。脚本先 `sleep` 一分钟，或者慢慢升压，但没有检查连接池、cache、队列、错误率、backlog 是否进入预期状态。线上症状是同样的 warm-up 时间，在不同机器、不同数据量、不同依赖状态下结果差很多。

第四种误用是忽略 warm-up 期间的错误。预热阶段已经出现 timeout、dropped iterations、OOM、连接 reset，但报告说“正式窗口没有错误”。线上症状是系统在峰值到来前已经开始丢请求，监控里却找不到对应的 benchmark 证据。

第五种误用是 warm-up 留下脏状态。预热产生大量任务、日志、对象、缓存和索引项，正式窗口继续使用同一环境。线上症状是 benchmark 越跑越慢、越跑越不稳定；偶发重复任务、唯一键冲突、旧数据被统计到新实验里。

第六种误用是只报告热 cache 性能。预热把所有数据扫入 cache，正式窗口完全命中。线上症状是 cache eviction、节点重启、滚动发布或数据增长后性能突然退化，团队发现 benchmark 从未覆盖真实 miss ratio。

第七种误用是用 warm-up 提前触发 autoscaling。先用预热流量让系统扩到足够容量，再开始统计。线上症状是突发流量到来时仍然超时，因为 autoscaler 还没来得及扩容；报告却显示“同样 QPS 下系统没问题”。

第八种误用是不同版本 warm-up 不一致。A 版本预热 10 分钟，B 版本预热 1 分钟；或者 A 版本先跑过一轮，B 版本冷启动。线上症状是优化效果无法复现，benchstat 或报告看起来有差异，换个顺序就反过来。

LogServe 中的典型误用包括：用 warm-up 把 replay 首次恢复成本删掉；预热只提交短任务，线上却有长任务阻塞 worker；预热任务不清理，导致正式窗口 shared log 更大；只看 submit latency，warm-up 后后台 backlog 仍然没清。面试时我会说，warm-up 误用后的线上症状通常不是“预热失败”，而是冷启动慢、扩容慢、重启后慢、cache miss 慢、benchmark 复现不了、正式窗口看起来干净但用户路径不干净。

## Q065. warm-up 在单机和分布式环境中的语义有什么差异？

单机 warm-up 和分布式 warm-up 的语义差别很大。单机里，warm-up 多半是让一个进程、一台机器和一组本地依赖进入目标状态；分布式里，warm-up 是让多个节点、多个负载生成器、多个缓存层、多个连接池和多个下游依赖同时进入可解释状态。

单机 warm-up 相对容易控制。时钟只有一个，cache、连接池、线程池、GC、文件系统 page cache、CPU 频率基本都在本机。你可以比较容易地判断 warm-up 前后状态差异，也容易清理。它适合建立 baseline，比如看某个服务进程在预热后是否稳定，某个本地 shared log 或本地数据库路径是否还有明显冷态成本。

单机的局限是，它会高估预热的可控性。生产系统里，cache 可能分布在多个节点，连接池每个实例都有一份，负载均衡器会把请求打到不同后端，数据库和对象存储有自己的 buffer 和限流。单机 warm-up 只说明最小闭环被预热，不说明整个部署拓扑都热起来。

分布式 warm-up 要处理更多语义：

| 维度 | 单机 warm-up | 分布式 warm-up |
|---|---|---|
| 预热对象 | 一个进程或少量本地依赖 | 多服务实例、LB、缓存集群、数据库、队列、对象存储、观测管道 |
| 负载来源 | 一个压测进程或脚本 | 多个 load generator，可能跨机房或跨地域 |
| 结束条件 | 本机资源和本地队列稳定 | 每个节点、每个分片、每个依赖都要达到阈值，不能只看 aggregate |
| 数据分布 | 容易控制 key 和数据集 | 需要预热分片、租户、热点和真实路由路径 |
| 失败语义 | 进程 crash 或本地资源耗尽 | 节点局部失败、分片没热、连接不均、LB 偏斜、时钟偏移 |
| 观测难度 | 日志/profile 容易对齐 | 需要统一 phase 标记、trace、时钟同步和跨节点聚合 |

分布式 warm-up 最大的问题是“平均值看起来热，局部其实冷”。例如总 cache hit ratio 已经 95%，但某个分片仍然 50%；总连接数已经足够，但某个服务实例刚被加入负载均衡，还没有连接；总 p99 看起来稳定，但某个 region 的首批请求仍然很慢。如果只看聚合指标，正式窗口会把这些局部冷态混进去。

在 LogServe 中，单机 warm-up 可以预热 shared log、scheduler、worker 和 mock LLM；分布式化后，则要考虑多个 scheduler/worker 实例、日志分片、对象存储、跨节点 replay、不同 worker 的连接池和负载均衡路径。单机 warm-up 可以作为机制验证；分布式 warm-up 才能说明部署路径是否进入稳定状态。

面试时我会这样回答：单机 warm-up 的语义是让一个受控环境热起来，分布式 warm-up 的语义是让整个拓扑的关键路径都达到可解释状态。分布式场景不能只说“预热 5 分钟”，还要证明每个节点、每个分片、每个下游和每个负载生成器都达到预热条件。

## Q066. open-loop load 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

open-loop load 的核心目标，是按外部世界给定的到达率向系统施加负载，而不是让系统响应速度反过来决定下一批请求什么时候到来。它主要解决**性能、容量和排队行为的测量问题**，尤其适合回答“当上游每秒固定送来 N 个请求时，系统会不会排队、超时、拒绝、重试或崩掉”。

k6 官方文档对 open model 的定义很直接：closed model 里，下一次 VU iteration 要等上一次完成；open model 里，新的 VU iteration 到达和前一次是否完成解耦。constant-arrival-rate executor 也是这个语义：在指定时间内按固定 rate 启动迭代，迭代启动独立于系统响应时间。这个差异非常关键，因为真实流量通常不会因为你的服务变慢就自动停下来。消息队列、网关、批处理、上游服务、用户群体、定时任务都可能继续按自己的节奏送流量。

open-loop load 主要解决的问题包括：

1. **测真实到达率下的排队**。系统变慢时，请求仍然到来，队列和 in-flight 会增长。closed-loop 很容易把这个过程掩盖。
2. **避免 coordinated omission**。如果压测器等系统响应后再发下一次请求，最慢时段会被低采样。open-loop 按计划发请求，能更接近用户实际等待。
3. **验证容量上限和降级策略**。当 offered load 超过服务能力时，系统应该排队、限流、拒绝还是超时？open-loop 能把这个边界打出来。
4. **模拟外部固定速率来源**。例如 Kafka topic 每秒进来多少消息、API 网关每秒转发多少请求、定时任务每分钟触发多少工作流。
5. **区分 offered load 和 achieved throughput**。open-loop 下，计划到达率、实际发送率、完成率、失败率和 dropped iterations 都要分开看。

它不主要解决正确性、安全性或可维护性。正确性仍然要靠 checks、不变量、状态验证；安全性要靠权限、认证、攻击面测试；可维护性要靠设计和代码质量。不过 open-loop load 会暴露这些领域的症状：例如幂等做错会在重试下产生重复任务，限流策略不清楚会造成雪崩，观测标签过高会把指标系统打爆。

在 LogServe 中，open-loop load 适合模拟“上游每秒固定提交任务”或“日志入口持续接收事件”的场景。即使 worker 变慢，提交仍然按计划发生，于是 shared log、scheduler backlog、worker in-flight、result-ready p99 和 retry count 都会真实上升。面试时可以说：open-loop load 的目标是把系统放到外部到达率面前，观察它如何排队和退化。它是性能和容量实验工具，不是正确性证明。

## Q067. open-loop load 的典型适用场景和不适用场景分别是什么？

open-loop load 适合真实到达率由外部决定的场景。只要上游不会因为被测系统变慢而自动减速，就应该考虑 open-loop，至少要用它补充 closed-loop 结果。

典型适用场景包括：

1. **API 网关和公共接口容量测试**。用户请求、移动端流量、第三方调用不会等你的服务恢复后再发。用固定 arrival rate 可以观察排队和拒绝策略。
2. **消息队列和流处理系统**。topic 每秒进入多少消息通常由上游决定。consumer 慢了，lag 会增长，而不是消息自动减少。
3. **批处理和定时任务**。定时调度、cron、批量导入、数据同步会在固定时间涌入，open-loop 更能模拟这种到达节奏。
4. **SLO 和错误预算验证**。如果 SLO 写的是“每秒 500 请求下 p99 < 300ms，错误率 < 0.1%”，open-loop 更适合固定 offered load。
5. **排队系统和背压策略测试**。要看 queue length、in-flight、rejection、timeout、retry amplification，open-loop 比 closed-loop 更直接。
6. **突发和峰值实验**。spike、flash sale、上游重放、事故恢复后的 backlog replay，都需要把到达率作为独立变量。

不适合的场景也要说清楚：

1. **用户行为强依赖响应完成**。例如一个真实用户必须等页面返回后才点下一步，closed-loop 更贴近单用户会话体验。
2. **目标是测单个会话的交互链路**。登录、浏览、下单这种 session workflow，如果每一步都依赖上一步结果，只用 open-loop 会打破业务语义。
3. **系统没有足够的流量隔离或保护**。open-loop 可以持续加压，如果没有限流、熔断、测试环境隔离，容易把共享依赖打坏。
4. **压测客户端资源不足**。如果 load generator 无法按计划发请求，open-loop 结果会变成测试工具瓶颈。k6 的 dropped iterations 就是必须关注的信号。
5. **只想做 smoke test 或基本正确性测试**。这时固定到达率不是重点，小规模 closed-loop 或简单端到端测试更合适。
6. **真实业务有强反馈控制**。例如上游会根据 429、队列长度或消费者 lag 主动减速，那就要把反馈控制建进负载模型，而不是纯 open-loop。

LogServe 中，适合 open-loop 的场景是固定速率提交 workflow、固定速率追加日志事件、模拟外部队列持续灌入任务。不适合的场景是模拟一个用户在任务完成后再查询结果、再提交下一步，这种链路本身是 closed-loop 的用户旅程。实践里常常两者都要跑：open-loop 测容量和排队，closed-loop 测会话体验。

面试回答可以这样收束：open-loop 适合“到达率是外部事实”的系统，不适合“下一步必须等上一步完成”的会话语义。用错模型，p99、吞吐和容量结论都会偏。

## Q068. open-loop load 和相近概念最容易混淆的边界在哪里？

open-loop load 最容易和 closed-loop、constant QPS、concurrency、throughput、stress test、spike test 混淆。边界的关键在于：open-loop 描述的是**请求到达过程**，不是并发数，不是吞吐结果，也不是测试强度本身。

| 概念 | 核心含义 | 和 open-loop load 的边界 |
|---|---|---|
| open-loop | 按外部计划启动请求或迭代 | 到达率独立于响应时间，慢了也继续到达 |
| closed-loop | 每个用户/worker 完成后再发下一次 | 响应变慢会自然降低到达率，可能掩盖排队 |
| constant QPS | 常被口语化使用 | 如果 QPS 指计划到达率，接近 open-loop；如果指完成吞吐，就不是同一件事 |
| concurrency | 同时在处理的请求数 | open-loop 固定到达率，并发数会随响应时间变化；并发不是输入固定值 |
| throughput | 单位时间完成了多少 | open-loop 的输入是 offered load，throughput 是系统输出结果 |
| stress test | 超过预期负载找极限 | 可以用 open-loop 执行，也可以用 closed-loop；stress 是目标，不是到达模型 |
| spike test | 短时间突增负载 | 可以是 open-loop 的突增到达率，也可以是并发突增 |
| replay | 按历史流量重放 | 如果按原始时间戳重放，更接近 open-loop；如果按响应驱动重放，就不是 |

最常见的误解是把 open-loop 说成“固定并发”。这不对。open-loop 固定的是计划到达率。系统响应变慢时，在途请求会增加；响应变快时，在途请求会减少。并发数是结果，不是输入。closed-loop 的输入常常是 VU 数或用户数，吞吐随着响应时间变化。

另一个误解是把 achieved throughput 当成 offered load。比如脚本配置每秒启动 1000 次迭代，这是 offered load；系统只完成每秒 700 次，另外 200 次超时、100 次 dropped，这才是 achieved throughput 和损失。报告如果只写“QPS 700”，就看不出系统是被喂了 700，还是被喂了 1000 但只吃下 700。

k6 的文档边界很清楚：open model 让迭代启动与响应时间解耦；constant-arrival-rate 按 `rate` 和 `timeUnit` 启动迭代；如果没有可用 VU 或 SUT 变慢，可能出现 `dropped_iterations`。这些指标应该一起解释，不能只看一个延迟分位数。

在 LogServe 中，如果我说“open-loop 每秒提交 500 个 workflow”，意思是上游计划到达率是 500/s；系统可能只完成 420/s，backlog 每秒增长 80，result-ready p99 上升。这个结论和“并发 500 个用户循环提交”不是一回事。面试时要把输入、输出和派生指标分开说：offered load 是输入，completed throughput 是输出，concurrency/backlog 是系统状态。

## Q069. open-loop load 在高并发场景下可能出现哪些隐藏问题？

open-loop load 在高并发下很有价值，也很容易失真。它会持续按计划制造到达，如果系统跟不上，排队和错误会快速放大。问题在于，你必须确认“计划到达率真的发出去了”“没发出去的请求被记录了”“失败和超时没有从统计里消失”。

常见隐藏问题包括：

1. **压测器没有能力维持计划到达率**。CPU、内存、网络、端口、文件描述符、TLS、DNS、脚本逻辑、指标上报都可能让 load generator 发不出计划请求。此时看到的不是 SUT 容量，而是压测器上限。
2. **dropped iterations 被忽略**。k6 文档明确说，arrival-rate executor 在没有 free VU 时会 drop iteration；这可能是配置不足，也可能是 SUT 变慢导致迭代时间拉长。如果报告只看成功请求 latency，会把最关键的损失删掉。
3. **VU 分配不足或动态分配污染结果**。`preAllocatedVUs` 太低时，测试开始后还要不断分配资源，导致到达率不稳；`maxVUs` 太低时，系统一慢就 drop。constant-arrival-rate 文档也提醒，过低的 preAllocatedVUs 会减少目标速率下的有效测试时间。
4. **队列无界增长**。open-loop 会把系统推到真实排队状态。如果入口无限接收但后台处理不过来，submit latency 可能还好，result-ready latency 和 backlog 已经爆炸。
5. **重试放大被算错**。上游 open-loop 到达率是 1000/s，客户端或服务端重试后下游实际请求可能变成 2000/s。如果只报告原始 offered load，会低估依赖压力。
6. **错误预算和取消语义不清**。被拒绝、超时、取消、dropped、最终未完成请求都应该分开统计。混成一个 error rate，会让容量边界和 SLO burn rate 失真。
7. **协调遗漏没有完全消失**。open-loop 能减少 closed-loop 的 coordinated omission，但如果脚本按实际发送时间而不是计划发送时间算延迟，或者压测器自己卡住没有记录未发请求，仍然会漏掉慢周期。
8. **测试数据生成成为瓶颈**。高并发下生成唯一 ID、签名、payload、租户分布、文件上传、随机数、读取 CSV 都可能成为客户端瓶颈，造成到达率抖动。
9. **下游保护机制被误读**。限流、熔断、排队、降级、backpressure 可能让入口延迟变低但失败率变高。open-loop 报告必须同时解释 latency、throughput、rejection 和 backlog。
10. **分布式压测聚合失真**。多个 generator 的时钟、网络位置、启动时间、分片比例不同，会让全局 arrival rate 看起来正确，但某个 region 或某个分片实际过载。
11. **观测系统被高基数标签打爆**。open-loop 产生大量请求，如果每个 request_id 都进入指标标签，metrics pipeline 可能先崩。此时 SUT 和观测系统互相影响。
12. **清理跟不上写入**。open-loop 会大量生成日志、对象、数据库行和队列消息。测试结束后如果不清理，下一轮实验会被历史数据污染。

LogServe 中的隐藏问题包括：入口每秒固定提交任务，但 scheduler 处理不过来，shared log 持续增长；worker pool 被慢任务占满，短任务也被拖慢；result-ready p99 比 submit p99 高几个数量级；mock LLM 或对象存储触发限流；客户端重试同一 workflow_id 造成重复事件；压测机生成 payload 或记录 raw data 先变慢。

面试时我会说：open-loop 高并发测试必须同时报告 offered load、sent load、completed throughput、dropped iterations、timeout、rejection、retry count、in-flight、backlog、客户端资源和服务端资源。只给 p99 或只给完成 QPS，都不足以支持容量结论。

## Q070. data corruption 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

data corruption 的核心目标不是“让系统跑得更快”，也不是单纯多做一次校验。它要解决的是：数据在写入、传输、落盘、压缩、索引、恢复、复制、缓存和读取过程中，是否仍然保持系统承诺的语义；一旦不保持，系统能不能尽早发现、隔离、恢复，并避免把坏数据继续传播。

所以它首先是正确性和可靠性问题。更准确地说，是防止 silent data corruption 变成 silent state corruption。前者可能只是某个字节错了、某个 record 尾部被截断、某个对象上传后内容与元数据不一致；后者更危险，系统把这个坏数据当成真相继续调度、重试、生成派生索引、更新物化视图，最后用户看到的状态已经错了，而且没有报警。

性能不是它的核心目标，但性能会被它牵连。checksum、hash、fsync、双写、读后校验、周期性 scrub、跨副本 hash 对比都会有成本。PostgreSQL 的 data checksums 文档把 page checksum 放在可靠性和 WAL 章节里：启用后，数据页写入时更新 checksum，读取时验证 checksum。etcd 的 data corruption 检测也很典型：initial check 和 periodic hash check 是为了防止成员状态分叉，不是为了提升吞吐。

安全性也相关，但不能混为一谈。CRC32、CRC32C、页 checksum 这类机制主要发现偶然损坏、截断、乱序写、错页、坏盘、程序 bug，不提供对抗性篡改防护。攻击者如果能同时改数据和 checksum，普通 checksum 就没用了。要解决恶意篡改，需要 MAC、签名、权限边界、审计链路和密钥管理。Amazon S3 的对象完整性文档列出多种 checksum 算法，用来校验上传、下载和静态对象是否完整；这和身份认证、授权、加密不是一回事。

可维护性是间接收益。一个系统如果有清楚的 record 格式、版本号、长度、checksum、segment 边界、snapshot 元数据和恢复流程，出问题时更容易定位。反过来，如果数据格式只靠“读到哪算哪”，恢复工具和排障手册会非常脆弱。

在 LogServe 里，我会把 data corruption 的目标说成三层：第一，shared log 的每条 record 要能发现 partial write、长度错、checksum 错和版本不兼容；第二，materialized metadata view、actor snapshot、result reference 必须能从可信事件重建或校验；第三，实验报告里要能证明这些损坏不是被吞掉了，而是被检测、隔离或恢复了。它主要回答“状态有没有错”，不是回答“p99 有没有低”。

面试时可以这样收束：

```text
data corruption 主要解决正确性和可靠性问题。它的核心目标是防止坏数据被当成真状态继续传播，并在损坏发生时尽早检测、隔离和恢复。checksum、WAL、snapshot、跨副本 hash、对象完整性校验都属于这个方向。它会带来性能成本，也会改善可维护性；如果面对恶意篡改，还要额外引入认证、签名和权限控制，普通 checksum 不能替代安全机制。
```

## Q071. data corruption 的典型适用场景和不适用场景分别是什么？

data corruption 适合放在“数据是系统真相来源”的路径上。只要一个字节错了会改变调度、恢复、计费、权限、任务状态或用户结果，就应该认真设计损坏检测。对 LogServe 来说，shared log、actor snapshot、workflow 状态、result reference、对象存储结果、checkpoint cache manifest 都属于这类路径。

典型适用场景有这些：

1. **append-only log 和 WAL**。日志尾部 partial write、segment 中间 bit flip、record length 错、CRC 错、index 指向错误 offset，都会影响 replay。这里必须有长度、版本、checksum、单调序号、segment 边界和恢复策略。
2. **数据库页和存储页**。PostgreSQL data checksums 的场景就是数据页级别保护：写页时更新 checksum，读页时验证。它不能解决所有问题，但能把一类页损坏从 silent failure 变成可见错误。
3. **对象存储和大结果引用**。大结果不适合全塞进日志，通常只存 result ref。此时对象内容、大小、etag/checksum、content type、生成者、写入完成标记都要能对上。S3 的对象完整性校验就是这一类一手例子。
4. **snapshot 和 checkpoint**。actor snapshot、模型 checkpoint、cache manifest 如果损坏，系统可能恢复到错误状态。snapshot 不能只看“文件存在”，还要验证版本、长度、checksum、对应 log offset 或 command_seq。
5. **跨副本状态一致性**。etcd 的 corruption check 通过成员间 hash 对比发现 persistent state divergence。分布式系统里，单个节点本地看起来正常，不代表各副本还一致。
6. **备份、迁移和恢复演练**。SQLite 官方文档专门提醒，事务活跃时直接复制数据库文件、丢失 hot journal、journal 和数据库文件错配，都可能造成损坏。备份不是 copy 文件这么简单。

不适用场景也要讲清楚。第一，如果数据本身是可丢弃的 telemetry 样本、debug log、临时 cache，并且丢失或单点错误不会影响系统真相，可以降低强校验强度。第二，如果问题是业务规则错误，比如折扣计算公式错、权限模型设计错，checksum 检不出来；那是 correctness test、审计和业务不变量的问题。第三，如果问题是恶意篡改，普通 CRC 不够，需要加密认证或签名。第四，如果系统根本不承诺持久性，比如内存里的 best-effort queue，不能拿 data corruption 机制去证明 durable semantics。

还有一种容易被忽略的不适用：为了 benchmark 好看而临时关闭同步和校验，然后拿结果解释生产可靠性。SQLite 文档提到关闭同步会让系统看起来更快，但 crash 或断电时可能暴露严重一致性风险。实验报告如果没有把这个配置写清楚，结论就不可信。

在 LogServe 里，data corruption 测试适合覆盖 shared log record、segment index、actor snapshot、result object、checkpoint manifest 和 replay materialization；不适合拿来证明“业务 DAG 设计合理”或“LLM 输出质量正确”。这些是不同层次的问题。

## Q072. data corruption 和相近概念最容易混淆的边界在哪里？

data corruption 最容易和 data loss、stale data、inconsistent state、schema incompatibility、security tampering、software bug 混在一起。它们会同时出现，但边界不同。面试时我会先分层，不然很容易把所有“数据不对”都叫 corruption。

| 概念 | 核心含义 | 和 data corruption 的边界 |
|---|---|---|
| data corruption | 数据表示本身被破坏，或内容与校验、结构、版本、不变量不一致 | 重点是“已有数据变坏或无法可信解释” |
| data loss | 数据没有被保存下来，或者应该存在的记录丢了 | 可能没有坏字节，只是缺记录、缺对象、缺事件 |
| stale data | 读到了旧版本 | 数据本身可能完整，但可见性、缓存、复制延迟或版本判断错了 |
| inconsistent state | 多个派生状态互相矛盾 | 可能由 corruption 导致，也可能由事务、并发、重试 bug 导致 |
| schema incompatibility | 新旧版本解析规则不同 | 不一定损坏；如果没有版本门禁，旧数据可能被误读成坏数据 |
| tampering | 恶意篡改 | 需要安全机制；普通 checksum 只能发现非对抗性错误 |
| application bug | 程序按错误逻辑写入了“格式正确但语义错误”的数据 | checksum 会通过，必须靠业务不变量和端到端校验 |

一个例子：shared log 中某条 record 的 CRC 不匹配，这是 data corruption。某条 `TaskCompleted` 根本没写入，这是 data loss。`TaskCompleted` 写了两次并且 result 不同，这是 inconsistent state 或幂等 bug。旧版本 worker 把新版本 record 的字段解释错，这是 schema compatibility 问题。有人绕过权限改了 result object，同时重算了普通 checksum，这是 tampering，不是 checksum 能独立解决的事。

还有一个重要边界是“检测”和“恢复”。检测到 corruption 只说明系统知道自己不可信了，不代表已经恢复。PostgreSQL checksum failure、etcd CORRUPT ALARM、SQLite hot journal 丢失后的失败，都只是把问题暴露出来。恢复还要有可用副本、snapshot、备份、重放日志、替换成员或人工处理流程。

在实验里也要分清：fault injection 可以注入 data corruption，但 fault injection 本身不是 data corruption；crash recovery 可以暴露 corruption，但 crash recovery 不是同一个概念；profiling 可以告诉你 checksum 花了多少 CPU，但 profiling 不能证明 corruption 处理正确。把这些概念混在一起，会导致实验报告看起来覆盖很多，实际没有回答任何一个问题。

LogServe 的边界可以这样说：shared log record 损坏是 data corruption；worker 执行两次同一个 step 是幂等和重试语义问题；dashboard 物化视图落后是 stale/materialization lag；actor snapshot 与 tail log 的 command_seq 接不上，可能是 snapshot corruption，也可能是恢复流程 bug。面试回答要先归类，再说用什么测试证明。

## Q073. data corruption 在高并发场景下可能出现哪些隐藏问题？

高并发下的 data corruption 难点不只是“同时写的人多”。真正麻烦的是，很多损坏来自边界时序：一个 goroutine 正在写，一个 goroutine 正在 flush；一个线程拿着旧 buffer，一个线程把对象放回池；一个进程 rename 新文件，另一个进程还持有旧 fd；一个节点刚写入 snapshot，另一个节点开始基于它做恢复。低并发测试很难撞到这些窗口。

常见隐藏问题包括：

1. **buffer 复用导致写后修改**。为了降低 `B/op`，系统常用 bytes buffer pool。如果 record encode 后还引用可变 slice，后台异步写入时原 slice 已被复用，落盘内容就可能被下一条记录污染。
2. **partial write 和并发 append 交错**。多个 writer 如果没有严格串行化，record A 的 header、record B 的 payload、record A 的 checksum 可能交错。append-only 并不自动等于线程安全。
3. **index 与 log 非原子更新**。log record 已写，index offset 未写；index 已写，log record 还没 fsync；崩溃或并发读刚好卡在中间，就可能读到错误 offset。
4. **checksum 覆盖范围不完整**。只校验 payload，不校验 length、type、version、stream_id、sequence、offset，结果 payload 没坏，但 record 被挂到错误 stream 或错误序号下。
5. **异步 flush 顺序错乱**。为了性能把写入和 fsync 放到后台，如果没有 barrier，后写的元数据可能先持久化，先写的数据反而没落盘。SQLite 文档里反复强调 sync/barrier 语义，就是因为存储层会重排。
6. **对象存储完成标记过早**。result object 还没完整上传，metadata 已经写入 `TaskCompleted`；读者拿到 result ref 后读到半对象、旧对象或校验不匹配对象。
7. **多副本分叉被 aggregate 指标掩盖**。分布式系统里一个节点本地 hash 正常，不代表所有节点一致。etcd 把 leader 与其他成员的 persistent state hash 对比，就是为了解决成员状态分叉。
8. **锁协议不一致**。SQLite 文档列了很多文件锁问题：不同进程使用不同锁协议、绕过 SQLite 直接读写数据库文件、fork 后继承连接，都可能导致损坏。高并发时这类错误更容易出现。
9. **观测系统低估损坏率**。如果一发现 checksum failure 就丢弃记录，但没有计数、没有样本、没有报警，报告里的成功率会显得很好，坏数据已经从统计里消失。
10. **重试把损坏放大**。读到坏 snapshot 后重试恢复，如果每次都基于同一份坏输入写入新事件，可能把一个局部坏对象扩散成多个错误状态。

在 LogServe 里，我会重点盯住 shared log writer 的串行化、record buffer 生命周期、segment index 更新顺序、actor snapshot 写入完成标记、result ref 的对象完整性、worker 并发完成同一个 step 时的幂等提交。高并发 data corruption 测试不能只跑 `go test -race`。race detector 能抓一部分内存竞争，但抓不到磁盘乱序、对象存储半写、跨进程锁协议和错误恢复路径。

比较可靠的做法是：压力下打开 checksum 校验，保留 raw corrupt sample，随机 kill writer，注入短写和截断，强制并发 replay，跑完后做全量 scrub。报告里除了 QPS 和 p99，还要有 corrupt_records_detected、bad_offsets、snapshot_verify_failures、orphan_result_refs、replay_mismatch_count 这些指标。

## Q074. data corruption 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

data corruption 最容易在“操作做到一半”时暴露。正常 shutdown 太干净，很多问题不会出现；真正要测的是 append 后没 fsync、fsync 后没 index、index 后没 rename、对象上传一半、snapshot 写完但 manifest 没写、manifest 写了但 checksum 没算、任务完成事件写了但 result object 不存在。

崩溃场景下，第一类边界是 partial write。日志尾部可能只有 header，没有完整 payload；或者 payload 完整但 checksum 没写；或者 record 完整但 segment footer 没写。系统恢复时应该能识别最后一条有效记录，截断或忽略无效尾部，并记录损坏指标。不能因为尾部坏了就把整个 segment 都读成可信，也不能静默跳过中间损坏继续 replay。

第二类边界是 durable ordering。`TaskCompleted` 事件如果先持久化，而 result object 没有持久化，恢复后控制面会认为任务完成，但用户拿不到结果。相反，result object 已经写完但 completion event 丢了，系统可能重试任务，留下孤儿对象。这不一定都是 corruption，但会和 corruption 检测交织在一起。实验要明确允许哪一种结果、如何清理、如何重试。

第三类边界是 snapshot 与 tail log 的连接点。actor snapshot 如果标记自己覆盖到 `command_seq=100`，tail log 必须从 101 接上。snapshot 文件损坏、manifest 指向错对象、tail log 缺 101、重复 100，都应该 fail closed。更合理的恢复是回退到上一个可信 snapshot 或 full replay，而不是拿不连续状态继续服务。

重启场景会暴露恢复路径是否重复消费坏数据。系统不能只在写入时校验，重启 replay 时也要校验。很多 corruption 是历史残留，只有重启、迁移、compaction、scrub 才会读到。PostgreSQL 的 page checksum 是读页时验证；etcd 的 periodic hash check 也是为了运行中发现成员状态分叉。

超时和重试会暴露幂等边界。写请求超时，不代表写没发生。客户端重试同一个 workflow，如果使用新的 idempotency key，可能生成重复事件；如果使用同一个 key 但 payload 不一致，必须拒绝或报告冲突。data corruption 测试要把“坏数据导致重试”和“重试制造坏状态”都覆盖到。

在 LogServe 里，我会列这些 crash points：

| 注入点 | 预期检查 |
|---|---|
| record header 写完后崩溃 | replay 停在上一条有效 record，报告 partial tail |
| payload 写完但 checksum 未写 | checksum failure，不进入物化视图 |
| log 写完但 index 未写 | 可通过扫描 log 重建 index，不能读错 offset |
| index 写完但 log 未持久化 | index recovery 必须验证目标 record |
| result object 半写 | completion 不应指向不可校验对象 |
| actor snapshot manifest 写错 | replay 回退或失败，不能应用坏 snapshot |
| worker 完成事件超时重试 | step result dedup 保持 exactly-once-ish 结果语义 |

面试时我会强调：崩溃恢复里的 data corruption 结论必须来自真实故障点，而不是 clean shutdown。要同时说明哪些数据允许丢、哪些必须被检测、哪些状态必须回滚、哪些重试必须幂等。

## Q075. data corruption 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

data corruption 机制的瓶颈要看校验发生在哪里。没有一个固定答案。checksum 看起来像 CPU 问题，实际系统里经常被 I/O、锁和扫描范围主导。

CPU 瓶颈主要来自 checksum、hash、压缩、解压、加密认证、序列化解析。CRC32C 如果能走硬件指令，成本可能很低；如果算法选错、payload 大、校验次数多，CPU profile 会很明显。对 LogServe 的 shared log record 来说，每条记录都算 checksum，microbenchmark 可以先看 `ns/op` 和 `B/op`。

内存瓶颈来自大块 buffer、重复 copy、对象池误用、scrub 时加载太多数据、hash 对比时保留过多中间状态。比如对象完整性校验如果每次把大 result 全部读进内存，再算 hash，高并发下很快会把 heap 和 GC 打高。更好的做法通常是 streaming checksum。

锁竞争来自全局 writer lock、segment metadata lock、snapshot registry lock、checksum 统计锁、对象存储 manifest 更新锁。损坏检测常常被放在热路径上，如果所有请求都要抢同一把锁来登记校验状态，性能会从 CPU-bound 变成 lock-bound。mutex profile 和 block profile 比 CPU profile 更能说明这个问题。

I/O 瓶颈最常见。强校验经常意味着多读一次、多写元数据、fsync、rename、读后校验、后台 scrub、全量扫描。etcd 的 latest revision hash check 文档说得很直：它需要扫描整个 etcd 内容，所以性能成本高；compacted revision hash check 复用 compaction 过程，成本低得多。这个差异很适合拿来解释“检测频率”和“检测成本”的权衡。

网络瓶颈出现在对象存储、远程副本、跨节点 hash 对比、备份恢复和分布式 scrub 里。S3 对象完整性校验可以在上传、下载或静态对象上做，但如果每次都下载大对象来校验，网络和对象存储请求成本会很高。分布式系统里跨节点对比 hash 也会受网络延迟和带宽影响。

还有一个容易忽略的瓶颈是观测写入。每次 checksum failure 都打大日志、带 request_id 高基数标签、保存完整坏 payload，会把日志和指标系统拖慢。坏样本要留，但要限速、采样、脱敏和独立存储。

在 LogServe 里，我会这样拆：

| 路径 | 可能瓶颈 | 验证工具 |
|---|---|---|
| record checksum | CPU、内存 copy | Go benchmark、CPU profile、benchmem |
| append + fsync | I/O、batch 策略 | logstore benchmark、iostat、latency histogram |
| segment index verify | I/O、锁 | replay benchmark、block/mutex profile |
| actor snapshot verify | I/O、对象大小、hash | snapshot replay benchmark、heap profile |
| result object integrity | 网络、对象存储、streaming hash | 端到端 benchmark、对象存储指标 |
| cross-node hash check | 网络、全量扫描 | 分布式压测、节点级 CPU/IO 指标 |

面试时不要只说“checksum 会消耗 CPU”。更完整的回答是：小 record 热路径多半看 CPU 和 copy，大对象和 scrub 多半看 I/O/网络，全局元数据容易看锁竞争，频繁错误上报可能看观测系统。

## Q076. data corruption 的 correctness test、stress test 和 benchmark 应该分别测什么？

这三个测试的目标完全不同。correctness test 问“能不能正确发现和处理坏数据”；stress test 问“在压力和混乱时还会不会漏检或扩散”；benchmark 问“这些保护机制的成本是多少”。把它们混在一起，实验结论就会变成“跑过了，所以又对又快”，这通常不可信。

correctness test 应该小而精确。它要构造可解释的坏输入：截断 record、篡改 payload、篡改 length、篡改 type、篡改 stream_id、错 sequence、错 snapshot manifest、缺 result object、对象 checksum 不匹配、index 指向错误 offset。每个 case 都要有明确预期：返回什么错误、是否进入 replay、是否更新物化视图、是否报警、是否允许回退。Go 里可以用 table-driven test 和 fuzz test 补充覆盖，比如随机 bit flip 后验证 parser 不 panic、不越界、不接受坏 record。

stress test 要把坏数据放进高并发和故障环境里。比如多个 writer 同时 append，后台 scrub 正在跑，worker 正在重试，进程被 kill，磁盘注入短写，对象存储偶发返回旧版本。stress test 的重点不是 `ns/op`，而是有没有漏检、死锁、资源泄漏、重复恢复、坏数据扩散。这里要看 corrupt_detected、panic、deadlock、goroutine leak、retry storm、backlog、replay divergence。

benchmark 才看成本。它应该分别测打开校验和关闭校验的差异，测不同 checksum 算法、不同 payload 大小、不同 batch、不同 fsync 策略、不同 scrub 周期、不同对象大小的吞吐和延迟。报告不能只给“启用 checksum 后慢了 5%”。要说明是在 CPU-bound、I/O-bound 还是 network-bound 场景下慢 5%，以及 tail latency 是否扩大。

一个简化表可以这样写：

| 类型 | 应该测 | 不应该拿来证明 |
|---|---|---|
| correctness test | 坏输入是否被拒绝、错误是否明确、状态是否不被污染 | 性能开销 |
| stress test | 高并发、崩溃、重试、短写下是否漏检和扩散 | 稳态容量上限 |
| benchmark | checksum、scrub、恢复扫描、对象校验的成本 | 损坏处理语义正确 |

在 LogServe 里，我会安排三组。第一组 correctness：构造坏 shared log、坏 index、坏 actor snapshot、坏 result ref，验证 replay 和 materialized view。第二组 stress：open-loop 提交 workflow，同时注入 record 截断、worker kill、result object 半写，观察恢复和错误隔离。第三组 benchmark：比较 checksum on/off、segment scan verify、snapshot verify、object integrity verify 对 p95/p99、append throughput、replay time、CPU/heap 的影响。

面试时可以这样说：

```text
correctness test 证明坏数据不会被接受；stress test 证明压力、崩溃和重试下坏数据不会漏检或扩散；benchmark 证明校验和恢复机制的成本可解释。三者要分开报告。一个 corruption benchmark 跑得快，不能说明它处理坏数据正确；一个 correctness test 通过，也不能说明它在生产负载下成本可接受。
```

## Q077. 如果要求从零实现一个简化版 data corruption，你会先定义哪些不变量？

从零实现简化版 data corruption 防护，我不会先写 checksum 函数。checksum 只是工具。真正先要定义的是不变量：什么数据算可信，什么状态必须 fail closed，什么情况下允许恢复，什么情况下必须停止。

我会先定义这些不变量：

1. **record 边界可判定**。每条 record 必须有 magic、version、header length、payload length、type、stream_id、sequence、checksum。解析器必须能判断一条 record 是完整、尾部不完整，还是中间损坏。
2. **checksum 覆盖完整语义字段**。checksum 不能只覆盖 payload。长度、类型、版本、stream、sequence、header 也要被覆盖，否则 payload 没坏但上下文被换了，系统仍然会误读。
3. **单调序号不跳、不倒退、不重复**。同一 stream 内，sequence 或 command_seq 必须连续或有明确缺口语义。actor replay 尤其需要这一点。
4. **index 永远只是缓存**。index 指向 log offset 时，读取者必须验证目标 record。index 损坏不能让系统读出一条伪造的可信 record；必要时可以从 log 重建 index。
5. **snapshot 必须绑定 log position**。snapshot manifest 要包含 actor_id、command_seq、source log offset、schema version、object checksum、object size。snapshot 与 tail log 接不上时不能继续应用。
6. **result ref 必须可验证**。完成事件指向的对象要有 object key、size、checksum、content version。读不到或校验不匹配时，任务不能被当成成功结果返回。
7. **恢复必须 fail closed**。读到中间损坏、未知版本、checksum mismatch、snapshot mismatch 时，不能静默跳过继续服务。可以回退到上一个可信点，也可以标记 degraded，但不能装作正常。
8. **坏数据样本可观测**。每次损坏要记录类型、stream、segment、offset、期望 checksum、实际 checksum、恢复动作。不能只返回一个泛泛的 `io error`。
9. **错误路径幂等**。重复恢复、重复 scrub、重复替换 snapshot 不能制造新的事件或重复完成任务。
10. **版本演进有门禁**。新版本 record 不能被旧版本 parser 当成旧格式误读。未知版本要拒绝或走兼容解析，不要猜。

简化实现可以是一个 append-only log：append 时写 header+payload+crc，fsync 后更新 index；启动时顺序扫描 segment，遇到合法 record 就重建 index，遇到尾部 partial record 就截断到上一条合法边界，遇到中间 checksum mismatch 就停止并报警。这个版本不复杂，但已经把“可恢复尾部”和“不可静默跳过中间损坏”的边界说清楚了。

如果放到 LogServe，我会把 shared log 当成第一层实现目标。每个 `TaskSubmitted/Started/Completed`、actor command、LLM completion 都是 record；actor snapshot 和 result object 是外部对象，但它们的 manifest 要进 log。这样 replay 时可以从 log 验证对象，而不是只相信文件存在。

面试时我会补一句：这些不变量比具体 checksum 算法更重要。CRC32C、SHA-256、xxHash、BLAKE3 都能讨论，但如果 record 边界、版本、序号、snapshot 绑定和 fail-closed 语义没定义，换算法也救不了系统。

## Q078. data corruption 的常见误用是什么，误用后通常会产生什么线上症状？

data corruption 机制最常见的误用，是把“有一个 checksum 字段”当成“系统已经防损坏”。这很危险。checksum 算错范围、校验时机不对、错误处理不对，都会让它变成装饰。

常见误用包括：

1. **checksum 只写不读**。写入时计算 checksum，恢复和读取时却不验证。线上症状是坏数据长期潜伏，直到迁移、重启、compaction、审计时才爆出来。
2. **只校验 payload**。length、type、version、stream、sequence 没被覆盖。线上症状是记录被挂到错误流、错误任务或错误 actor 下，但 checksum 仍然通过。
3. **遇到 checksum failure 自动跳过**。为了让服务继续跑，replay 跳过坏记录。线上症状是少量 corruption 变成状态缺口，workflow 卡住、actor command 少执行、dashboard 与真实日志不一致。
4. **把 corruption 当普通 transient error 重试**。同一份坏 snapshot 被无限重试。线上症状是 retry storm、worker 忙等、错误日志刷屏，但状态没有进展。
5. **关闭 fsync 或同步屏障来跑 benchmark**。报告看起来吞吐很高，崩溃后出现回滚、错序写或日志尾部无法恢复。SQLite 文档里关于 sync 的讨论很适合提醒这一点。
6. **备份时只复制主文件，不复制 journal/WAL 或 manifest**。线上症状是恢复环境才发现备份不可用，或者恢复出来的数据处于事务中间状态。
7. **相信 index，不验证 log**。index 文件损坏后读错 offset。线上症状是偶发 replay panic、解析出奇怪类型、同一个 stream 序号倒退。
8. **对象写入完成标记过早**。metadata 已经显示成功，实际对象还没完整上传或 checksum 不匹配。线上症状是用户查询任务成功，但下载结果失败。
9. **把普通 checksum 当安全签名**。攻击者或越权进程改数据后重算 checksum。线上症状是审计无法证明数据未被篡改。
10. **只在单机测，不测副本分叉**。每个节点本地都能读，但节点间状态不同。线上症状是读请求打到不同节点返回不同结果，leader 切换后状态倒退或跳变。

在 LogServe 中，具体误用可能是：shared log 校验只在 append benchmark 里跑，control replay 没开；actor snapshot 文件存在就信任，不验证 command_seq 和 checksum；result ref 只记录路径，不记录大小和 hash；恢复时遇到坏 record 打日志后继续 materialize；为了提高 logstore benchmark，把 fsync policy 改成 interval，却不在报告里说明 durability 边界。

这些误用后的线上症状通常不叫“data corruption”。报警会表现为 workflow 卡住、任务成功但结果不存在、actor 状态倒退、重启后 dashboard 数量变化、同一任务重复完成、p99 突然变高、恢复时间变长、某个 segment 永远 replay 不过去。面试时要把症状往底层追：是数据坏了、丢了、旧了，还是派生视图错了。

## Q079. data corruption 在单机和分布式环境中的语义有什么差异？

单机 data corruption 的核心问题是：一个进程或一台机器上的持久状态是否还可信。分布式 data corruption 的核心问题更复杂：每个节点本地可信，不代表全局状态一致；某个节点坏了，也不代表整个系统必须停机。语义差异主要来自副本、仲裁、复制延迟、成员替换和跨节点恢复。

单机里，损坏边界相对清楚。一个 log segment、一个 index、一个 snapshot、一个对象文件，坏了就是本地状态坏了。恢复策略通常是截断尾部、重建 index、回退 snapshot、从备份恢复、标记只读或停止服务。测试也相对容易：kill 进程、截断文件、改字节、删 manifest，然后重启验证。

分布式里，首先要区分 local corruption 和 replica divergence。一个节点的磁盘页坏了，可能只是本地副本损坏；两个节点同一个 revision 的 hash 不一致，则说明系统状态分叉。etcd 文档中的 initial corruption check 和 periodic hash check 就是在处理这个问题：成员启动或运行中把 persistent state 与其他成员对比，不一致时 raise CORRUPT ALARM。

第二个差异是恢复动作要保持集群语义。单机可以直接删本地状态重建；分布式系统里，替换成员、清空本地 snapshot、从 leader 下载、恢复整个集群，都要考虑 quorum、term、revision、membership、客户端可见性。随便把一个旧 snapshot 拿回来，可能让节点带着过期状态重新加入，造成更大的分叉。

第三个差异是“正确副本”不一定容易判断。多数派通常代表 committed state，但如果 corruption 发生在比较旧的 compaction 边界、备份链路或对象存储层，可能需要 snapshot hash、revision、WAL、审计日志一起判断。不能简单说“哪个节点数据多哪个对”。

第四个差异是检测成本。单机 scrub 主要消耗本机 I/O；分布式 scrub 还涉及跨节点 hash、网络、leader 负载、慢 follower、compaction 频率。etcd 把 compacted revision hash check 和 latest revision hash check 分开，就是因为检测及时性、成本和慢 follower 兼容性不同。

第五个差异是损坏传播。单机坏数据如果没有对外输出，影响范围有限；分布式系统会把坏 snapshot、坏对象、坏索引、错误事件复制给其他节点或下游消费者。恢复流程如果没有隔离，corruption 会从一个副本变成全局真相。

LogServe 当前更接近单机、多进程机制验证系统。它可以在单机 Ubuntu、多个 worker、mock LLM 下验证 shared log、replay、actor snapshot、result ref 和 benchmark 工具链。但我不会把这个结论直接说成“已经解决分布式 corruption”。如果未来把 LogServe 扩展成多节点 shared log 或多副本 scheduler，就需要增加跨节点 hash、成员替换、quorum 下的恢复、对象存储一致性、clock/epoch/fencing 和分布式 scrub。

面试里可以这样回答：

```text
单机 data corruption 关注本地持久状态是否可解析、可校验、可恢复；分布式 data corruption 还要关注副本之间是否分叉，以及坏副本如何在不破坏 quorum 和可见性的情况下恢复。单机可以通过截断尾部、重建索引、回退 snapshot 解决一部分问题；分布式系统还需要跨成员 hash、CORRUPT alarm、成员替换、从可信 snapshot 恢复和防止坏状态传播。
```

## Q080. network partition 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

network partition 的核心目标，是把“节点还活着，但彼此无法正常通信”这个故障单独拿出来验证。它不是简单的机器宕机，也不是普通慢请求。机器可能继续处理本地任务、继续持有旧连接、继续写本地缓存，只是和另一部分节点断开了。分布式系统最怕的正是这个状态：每一边都觉得自己还有道理，最后写出两个互相冲突的真相。

所以 network partition 首先解决正确性问题，尤其是共识、租约、幂等、fencing、leader election、写入可见性和恢复后的收敛问题。Raft 论文把网络延迟、分区、丢包、重复和乱序都放进故障假设里，并强调一致性不依赖时钟；极端延迟最多造成可用性问题。etcd 的 failure modes 文档也给了一个很直接的边界：分区把集群切成多数派和少数派，多数派继续服务，少数派不可用；少数派 leader 会退位，分区恢复后跟随多数派恢复状态。

性能是第二层目标。partition 会让请求超时、重试、排队、leader election、连接重建、健康检查抖动、backlog replay 全部发生。它能暴露 p99 和吞吐下降，但这些性能下降背后通常是正确性机制在工作：少数派拒绝写、客户端重连、旧 leader 退位、旧 epoch 被 fence 掉。不能只看“变慢多少”，还要看“慢的时候有没有写错”。

安全性也会被牵涉，但不是 network partition 的主要目标。分区期间如果认证服务、密钥服务、权限缓存、审计管道不可达，系统可能暴露“fail open 还是 fail closed”的设计选择。它和攻击者主动切网不同；如果要测试恶意网络攻击，还需要把身份认证、TLS、重放攻击、防火墙规则和审计一起纳入。

可维护性是结果，不是首要目标。一个系统如果能清楚表达“哪个节点处于哪个 partition、哪个 epoch 有效、哪些请求被拒绝、哪些请求已提交、恢复后如何 reconcile”，排障会容易很多。相反，只说“网络不稳定导致异常”基本没法定位。

在 LogServe 里，network partition 的目标不是证明单机 benchmark 更快，而是验证这些边界：control 与 worker 断开时，worker 是否会被 fencing；旧 worker 恢复连接后能否继续写完成事件；shared log 不可达时 SDK 是失败、排队还是重试；actor ownership 在网络恢复后是否只有一个有效 owner；result ref 已写但 completion event 未提交时如何恢复。这个问题主要属于正确性和可靠性，性能指标只是观察窗口。

面试时我会这样答：

```text
network partition 主要验证分布式正确性：节点还活着但互相不可达时，系统会不会出现 split-brain、双 leader、重复提交、旧 lease 继续生效、少数派写入被当成真状态。它也会影响性能，因为超时、重试、选主和 backlog replay 会抬高 p99，但性能不是主问题。好的 partition 测试要同时看 safety 和 liveness：错的事情不能发生；网络恢复后，系统也要能重新前进。
```

## Q081. network partition 的典型适用场景和不适用场景分别是什么？

network partition 适合用在有多个独立故障域、多个进程、多个副本或外部依赖的系统里。只要系统存在“我本地还能运行，但我联系不到对方”的状态，就该测 partition。它比 kill 一个进程更接近真实故障，因为真实网络常常不是全黑全白，而是单向不通、部分丢包、某条链路慢、某个服务 DNS 可解析但 TCP 连接卡住。

典型适用场景有这些：

1. **共识和复制系统**。Raft、Paxos、etcd、ZooKeeper、数据库主从复制，都要证明多数派规则、leader election、日志提交和恢复收敛。etcd 文档里的多数派可用、少数派不可用，就是这类场景的基本语义。
2. **lease、lock 和 ownership**。如果一个节点拿着 lease 后被隔离，它不能永远认为自己还是 owner。恢复后也不能凭旧 epoch 继续写。actor ownership、分布式锁、leader lease 都在这里。
3. **任务调度和队列系统**。control 联系不到 worker 时，是重新投递、等待心跳超时，还是标记失败？worker 恢复后如果把旧任务结果写回来，系统是否接受？这正是 LogServe 要关心的路径。
4. **对象存储、数据库、消息队列依赖**。应用和依赖之间断开，可能导致 result object 写成功但 completion event 失败，或者消息已发送但 ack 丢失。partition 测试要覆盖这些半成功状态。
5. **跨机房、跨可用区和 Kubernetes 网络故障**。Chaos Mesh 的 NetworkChaos 直接支持 partition、delay、loss、duplicate、corrupt、bandwidth 等故障，适合在 Kubernetes 里验证服务间网络边界。
6. **客户端容错和 retry 语义**。客户端看到 timeout 时不知道服务端是否处理过请求。partition 测试能暴露 idempotency key、retry budget、deadline propagation 是否真的设计清楚。

不适用场景也要说。单进程纯内存算法没有网络边界，强行说 network partition 意义不大。只测函数级性能时，partition 也不是合适工具；应该用 microbenchmark。只想验证业务规则是否正确，也不要把 partition 当万能故障注入。还有一种情况：测试环境没有隔离，partition 会影响共享数据库、共享网络或其他团队服务，这时不应该直接做破坏性实验。

工具选择也有边界。Linux `tc-netem` 适合模拟延迟、丢包、重复、乱序、包损坏和带宽限制；Toxiproxy 适合在 TCP 代理层模拟 latency、down、timeout、reset、bandwidth；Chaos Mesh 适合 Kubernetes Pod 维度的 network chaos。它们能制造网络症状，但不能自动证明系统正确。证明仍然要靠不变量和测试断言。

在 LogServe 里，适合测的是 control-worker、SDK-control、worker-result store、control-shared log 这些链路。不适合的是把本地 executor 函数当成 network partition 测，或者在没有多进程部署时硬说测了分布式容错。当前项目是单机多进程机制验证，可以测“进程间链路断开”的语义，但不能把结果外推成多机生产网络结论。

## Q082. network partition 和相近概念最容易混淆的边界在哪里？

network partition 最容易和 latency、packet loss、node crash、process pause、dependency outage、backpressure、timeout 混在一起。它们会互相诱发，但不是同一个概念。

| 概念 | 核心含义 | 和 network partition 的边界 |
|---|---|---|
| network partition | 节点或节点集合之间互相不可达，通常是部分拓扑断开 | 节点还可能活着，并继续处理本地逻辑 |
| high latency | 通信很慢但仍能到达 | 可能触发 timeout，看起来像 partition，但消息最终可能到达 |
| packet loss | 部分包丢失 | 可能造成连接失败或重传，不一定形成稳定的拓扑分裂 |
| node crash | 进程或机器停止工作 | 节点没有继续执行本地逻辑；partition 中节点可能还活着 |
| process pause | GC、Stop-the-world、CPU 饥饿导致进程不响应 | 网络没断，但其他节点看到的症状像超时 |
| dependency outage | 数据库、对象存储、MQ 不可用 | 可能是依赖故障，不一定是节点间互相隔离 |
| backpressure | 系统主动降速或拒绝请求 | 是负载控制，不是通信拓扑被切开 |
| timeout | 调用方等不到结果 | timeout 是观察结果，不能证明对方没执行 |

最危险的混淆是把 timeout 当成失败事实。客户端超时只说明它没有及时收到响应，不说明服务端没有提交。分区解除后，服务端可能已经写了日志、提交了任务、上传了对象。没有 idempotency 和 request identity，重试会制造重复副作用。

第二个混淆是把高延迟当成 partition。Raft 论文里的 timing 讨论很适合提醒这一点：极端延迟和消息丢失会影响可用性，但一致性不能依赖“消息一定很快”。如果把 election timeout 设得太低，普通抖动也会触发频繁选主。那不是系统更敏捷，而是参数把慢网络误判成 leader 失效。

第三个混淆是把“少数派不可写”当成系统坏了。etcd 在分区时让多数派继续服务、少数派不可用，是为了避免 split-brain。这是正确行为。测试报告如果只写“少数派写失败”，却不说明这是预期保护，会误导读者。

第四个混淆是把 chaos injection 当成 partition 本身。Toxiproxy 的 `down`、`timeout`、`latency`，`tc-netem` 的 loss、delay、reorder，Chaos Mesh 的 partition action 都是制造故障的手段。真正要验证的是系统对这些故障的语义响应：拒绝、排队、重试、降级、选主、fencing、恢复。

在 LogServe 里，我会把边界说得更细：control 进程 crash 是 crash recovery；worker 与 control 双向断开是 partition；worker 只是执行慢是 slow node；shared log 写入慢是 I/O/backpressure；SDK 超时是客户端观察，不等于任务没有被提交。面试时先分类，再谈测试。

## Q083. network partition 在高并发场景下可能出现哪些隐藏问题？

高并发会把 partition 的问题放大。低流量下，一个旧 leader 多处理几个请求可能看不出来；高流量下，几秒钟的错误所有权就能产生大量重复任务、重复对象、冲突事件和重试风暴。

常见隐藏问题包括：

1. **重试风暴**。大量客户端同时遇到 timeout，如果没有 jitter、retry budget、全链路 deadline，恢复瞬间会把多数派或依赖打爆。
2. **少数派积压不可见**。少数派节点继续接收本地请求、写本地队列或缓存。分区恢复后，这批积压如果没有 epoch/fencing，可能冲击主状态。
3. **旧 leader 或旧 owner 继续写**。节点被隔离后还以为自己有 lease，继续完成任务。恢复后如果系统只看 worker_id 不看 epoch，就会接受旧结果。
4. **连接池全部卡死**。高并发时 TCP 连接长时间卡在超时路径，goroutine、线程、fd、ephemeral port 被耗尽。此时瓶颈不在业务逻辑，而在等待和连接管理。
5. **健康检查误判导致抖动**。partition 期间 readiness/liveness、service discovery、负载均衡器同时变化，流量在少数派、多数派和重启节点之间来回打，p99 会很难解释。
6. **backlog replay 造成第二次事故**。网络恢复后，worker、队列、客户端一起补偿重放。系统刚恢复通信，又被历史积压压垮。
7. **幂等键粒度不够**。高并发重试下，如果幂等键只按用户或 workflow 粗粒度生成，会误合并不同请求；如果每次重试都换 key，会重复提交。
8. **观测聚合掩盖分区位置**。全局 error rate 可能只有 5%，但某个 partition 内部是 100% 失败。只看 aggregate 指标会错过拓扑问题。
9. **时钟和 TTL 假设暴露**。分区后 heartbeat 断了，lease 是否过期、谁有权续约、恢复后旧 TTL 是否还算数，这些都会被高并发请求打到。
10. **限流位置不对**。如果限流在少数派本地执行，恢复后多数派仍然会被重试打穿；如果只在中心控制面限流，少数派可能完全不知道该拒绝。

在 LogServe 里，高并发 partition 可能表现为：SDK 大量提交 workflow 时 control 和 worker 断开；control 重新投递任务，旧 worker 恢复后也写回完成；actor mailbox 在两个 owner 上都积压命令；result store 写成功但 control completion 超时，客户端又提交一次；dashboard 只显示总体任务数，看不到某个 worker 的 in-flight 已经不可回收。

报告时不能只给“partition 期间 QPS 降到多少”。要同时给每个分区的请求数、拒绝数、超时数、重试数、leader/owner epoch、queue backlog、in-flight、fd/goroutine、恢复后的 duplicate/completion conflict。高并发 network partition 的重点是防止错误状态被放大。

## Q084. network partition 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

partition 和 crash 叠在一起时，边界最难。节点在少数派里没死，后来又重启；客户端看到 timeout，不知道请求是否提交；leader 在提交一半时隔离；worker 写完外部对象后联系不到 control。这些都不是简单的“失败后再试一次”。

崩溃场景下，要问旧节点重启后带着什么状态回来。它可能有未提交日志、旧 lease、本地队列、半写对象、未上报的 task result。Raft 的规则要求 leader 只能在多数派确认后提交；未提交的旧 leader 记录可能被后续 leader 覆盖。etcd 文档也明确说，旧 leader 发出的未提交写可能丢失，但已提交写不会丢。测试要区分 committed 和 accepted，不能把“旧节点见过”当成“已经提交”。

重启场景会暴露 fencing 是否持久化。一个 worker 被隔离，control 分配了新 owner；旧 worker 重启后如果仍然拿旧 epoch 写 actor state，就会破坏串行化。正确做法是所有完成事件、actor command、lease renewal 都带 epoch 或 term，接收方只接受当前 epoch。

超时场景最容易误判。调用超时后，服务端可能已经完成。客户端必须用同一个 idempotency key 查询或重试，而不是盲目创建新任务。对 LogServe 来说，`workflow_id + step_id + input_hash` 这种 exactly-once-ish 边界就要在 partition 下验证：超时重试后，最终只能有一个可见结果。

重试场景还要看 retry budget 和退避。分区没有恢复时，频繁重试只会消耗连接池和日志；分区恢复时，所有客户端同时重试会制造尖峰。测试要覆盖固定间隔重试、指数退避、带 jitter 的退避、deadline 过期后的停止。

几个具体边界可以列成表：

| 场景 | 需要验证的边界 |
|---|---|
| leader 被隔离后崩溃 | 未提交写不被当成 committed；新 leader 可继续服务 |
| 少数派 leader 恢复通信 | 必须退位，不能继续接受写 |
| worker 断网后继续执行任务 | 完成事件必须带 epoch/fencing，旧结果被拒绝或转人工处理 |
| 客户端提交超时 | 同一 idempotency key 重试不产生第二个任务 |
| result object 写成功但 completion event 超时 | 恢复后能 reconcile，不能返回“成功但无结果” |
| 网络恢复后 backlog 重放 | 按限流和幂等处理，不能把系统再次打垮 |
| 节点重启后本地缓存仍在 | 缓存必须按 term/epoch/version 校验，不能覆盖新状态 |

在 LogServe 面试里，我会说：partition 测试要故意叠加 kill、restart、timeout、retry。只做“断开 30 秒再恢复”太干净，测不到真实边界。真正要证明的是：旧 owner 写不进去，重复任务合不进去，已提交状态不丢，未提交状态可重试或可回滚。

## Q085. network partition 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

network partition 的直接瓶颈当然在网络，但性能症状经常落在别的地方。分区让请求等待、超时和重试，真正把系统压垮的可能是连接池、队列、锁、日志、对象存储或观测系统。

网络瓶颈包括丢包、延迟、乱序、带宽限制、单向断开、DNS 不通、TCP reset、连接半开。Linux `tc-netem` 支持 delay、loss、corrupt、duplicate、reorder、rate 这些故障；Toxiproxy 支持 latency、down、bandwidth、timeout、reset_peer 等 TCP 层故障；Chaos Mesh 在 Kubernetes 里可以按 Pod、方向和目标注入 partition、delay、loss、bandwidth。不同工具影响层次不同，性能解释也不同。

CPU 瓶颈通常来自重试放大和协议处理。TLS 握手、连接重建、gRPC keepalive、序列化、日志解析、leader election、心跳处理都会消耗 CPU。分区恢复时，大量客户端同时重连，CPU 峰值可能比正常峰值高很多。

内存瓶颈来自积压。partition 期间 in-flight 请求、未完成 future、队列消息、retry buffer、result object metadata、trace span 都可能堆起来。Go 服务里会看到 goroutine 增长、heap 增长、timer 增长。内存不是根因，但会变成事故放大器。

锁竞争来自控制面状态。leader election、worker registry、actor ownership、queue redelivery、lease table、retry scheduler、metrics registry 都可能在 partition 恢复时被并发访问。如果所有 worker 同时重新注册，单把 mutex 会让恢复时间变长。

I/O 瓶颈来自日志和恢复。分区期间系统可能写大量 timeout、retry、heartbeat missed、redelivery 事件；恢复后要 replay backlog、重建 index、写 completion、扫 snapshot。shared log 或数据库可能成为瓶颈，而不是网络本身。

在 LogServe 里，我会按阶段看：分区发生时看网络错误、deadline、连接池、in-flight；分区持续时看队列、worker lease、retry buffer、heap；分区恢复时看重新注册、redelivery、shared log append、actor snapshot replay、result reconciliation。对应工具是端到端压测、pprof、mutex/block profile、网络工具、日志事件计数和 dashboard snapshot。

面试时一句话概括：network partition 的根因在通信，但性能瓶颈可能落在 CPU、内存、锁、I/O 任意一层。只看网卡指标不够；要按“断开、持续、恢复”三个阶段分开看。

## Q086. network partition 的 correctness test、stress test 和 benchmark 应该分别测什么？

correctness test、stress test 和 benchmark 在 partition 里必须分开。correctness test 证明系统没写错；stress test 证明系统在混乱下也没写错；benchmark 证明这些保护机制的成本和恢复时间。

correctness test 要有明确拓扑。比如三节点或五节点集群，切成多数派和少数派；leader 在多数派、leader 在少数派、客户端在少数派、worker 在少数派分别测。每个 case 都要断言：少数派写是否被拒绝，多数派写是否可提交，旧 leader 是否退位，旧 epoch 写是否被拒绝，分区恢复后日志和状态是否收敛。etcd 的多数派/少数派语义可以作为这类测试的参照。

stress test 要制造高并发和随机故障。比如 open-loop 提交任务，同时随机断开 worker、control、result store；随机让 Toxiproxy 注入 timeout；用 `tc-netem` 注入丢包和延迟；在 partition 期间 kill 某个进程。重点看有没有重复提交、死锁、retry storm、goroutine leak、backlog 无界增长、旧 owner 写入成功。

benchmark 要测代价，而不是只测“能不能熬过去”。指标应该包括 partition detection time、leader election time、unavailable window、timeout rate、retry count、backlog growth rate、time to drain backlog、recovery p95/p99、恢复阶段 CPU/heap/I/O、客户端成功率。Raft 论文对 election timeout 的讨论很有用：timeout 太低会引发不必要选主，太高会延长不可用窗口。benchmark 应该帮助选择这些参数。

一个简化表：

| 类型 | 应该测 | 不应该拿来证明 |
|---|---|---|
| correctness test | 多数派/少数派、leader 退位、epoch fencing、幂等重试、恢复收敛 | 容量上限 |
| stress test | 高并发、随机断链、丢包、重启、重试风暴下是否仍守住不变量 | 单次稳定延迟 |
| benchmark | 检测时间、不可用窗口、恢复时间、backlog drain、资源成本 | 正确性本身 |

LogServe 可以这样设计：correctness test 切断 control-worker，断言旧 worker 不能写 actor completion；stress test 在 open-loop workflow 提交中随机断开 worker 和 result store；benchmark 测从 partition 开始到 worker 被重新调度、backlog 清空、dashboard 收敛需要多久。报告里要把成功请求、超时请求、被拒绝请求和重复请求分开统计。

## Q087. 如果要求从零实现一个简化版 network partition，你会先定义哪些不变量？

从零实现一个简化版 network partition 测试框架，我会先定义不变量，而不是先写 `iptables DROP` 或 Toxiproxy 配置。网络故障只是注入手段；不变量决定测试到底证明什么。

我会先定义这些不变量：

1. **partition 拓扑明确**。哪些节点在 A 侧，哪些节点在 B 侧，哪些客户端受影响，方向是 `to`、`from` 还是 `both`。Chaos Mesh 的 direction 字段就是这个语义。
2. **每个请求有唯一身份**。请求必须有 request_id、idempotency key、workflow_id 或 command_seq。超时重试后要能判断是不是同一个操作。
3. **所有权带 epoch/term**。leader、worker ownership、actor owner、lease renewal 都必须带 term 或 epoch。旧 epoch 的写入被拒绝。
4. **提交点可判定**。accepted、replicated、committed、applied、visible 要分开。客户端超时不能直接改写提交语义。
5. **少数派行为明确**。少数派是拒绝写、只读、排队，还是完全不可用。不能让它偷偷写本地状态再恢复时合并。
6. **恢复规则明确**。分区解除后，谁是权威状态源，少数派如何 catch up，旧 leader 如何退位，backlog 如何限速清理。
7. **时间参数有边界**。heartbeat interval、election timeout、lease TTL、RPC deadline、retry backoff 必须记录在实验里。否则结果不可复现。
8. **故障注入可撤销**。测试框架必须能确认网络规则已经清理。Chaos Mesh 文档也提醒，如果控制器和 daemon 的连接被打断，网络故障可能无法恢复。
9. **观测按分区维度保留**。不能只保留全局 QPS。要按节点、分区、worker、actor、client 分开统计。
10. **测试结束有收敛检查**。最终日志、物化视图、actor state、result ref、queue backlog 必须收敛到一个解释得通的状态。

简化实现可以这样做：本地启动 control、两个 worker、一个 result store proxy；所有跨进程流量都走 Toxiproxy；测试开始时把 worker-1 到 control 的方向设为 timeout 或 down；持续提交带 idempotency key 的 workflow；partition 期间要求 worker-1 的旧 completion 被拒绝或延迟；恢复后检查只有一个最终结果，actor command_seq 连续，dashboard 状态收敛。

如果用 Linux 层工具，可以用 `tc-netem` 注入 delay/loss/reorder，用 iptables/nftables 做定向 drop。Kubernetes 里可以用 Chaos Mesh NetworkChaos 的 partition action。工具可以换，不变量不能换。

面试时我会说：简化版 network partition 的核心不是“把网断了”，而是把拓扑、请求身份、提交点、epoch、恢复规则和收敛检查定义清楚。否则测试只是制造混乱，不能支持结论。

## Q088. network partition 的常见误用是什么，误用后通常会产生什么线上症状？

network partition 的常见误用，是把“断一下网，看服务还能不能恢复”当成完整测试。这样只能说明脚本跑完了，不能说明系统在分区期间没有写错。

常见误用包括：

1. **只测全断，不测半断和单向断**。真实故障常常是一边能发、一边收不到，或者只有某类流量被挡。线上症状是健康检查正常，但写请求卡死。
2. **只看恢复后服务可用，不看分区期间写入**。恢复后接口能通，不代表期间没有重复提交、旧 owner 写入或少数派脏状态。
3. **把 timeout 当成未执行**。客户端超时后创建新请求。线上症状是重复订单、重复任务、重复对象、重复 workflow。
4. **没有 fencing**。旧 leader、旧 worker、旧 actor owner 恢复后继续写。线上症状是状态倒退、双写、actor mailbox 顺序破坏。
5. **不限制重试**。partition 期间所有客户端高频重试。线上症状是恢复时服务被 retry storm 打垮，明明网络好了，业务仍然不可用。
6. **只测少量并发**。低并发下没有积压，高并发上线后 backlog、连接池、goroutine 才爆。测试结论太乐观。
7. **故障注入工具位置错误**。只在客户端代理层断开，服务端内部复制链路没断；或者只断出站，不断入站。线上症状是测试覆盖和真实故障不一致。
8. **清理网络规则失败**。iptables、tc、Chaos Mesh 实验残留。线上症状是后续测试偶发超时，很久才发现环境没恢复。
9. **只报告平均延迟**。partition 的核心指标是不可用窗口、超时、拒绝、重试、恢复时间和状态收敛。平均延迟会把问题抹平。
10. **把单机多进程结果说成多机结论**。单机 proxy 能测一部分语义，但不能覆盖跨机房路由、负载均衡、DNS、云网络和真实存储依赖。

在 LogServe 中，误用可能是只断 SDK 到 control 的连接，却说验证了 worker 容错；只看 workflow 最后完成，却不检查是否执行了两次；只断 worker 进程而不是网络链路，把 crash recovery 当成 partition；或者分区恢复后 dashboard 正常，就没有检查 shared log 是否有重复 completion。

线上症状往往不是“network partition failed”。它会表现为 p99 飙高、任务重复执行、少数 actor 状态错乱、worker 频繁重新注册、控制面 CPU 突增、连接池耗尽、日志里出现大量 deadline exceeded、恢复后 backlog 很久清不完。面试时要把这些症状和具体边界连起来说。

## Q089. network partition 在单机和分布式环境中的语义有什么差异？

单机里的 network partition 多半是进程间或本机代理层的断链：SDK 到 control 不通、worker 到 control 不通、control 到 result store proxy 不通。它能验证 timeout、retry、幂等、fencing、恢复收敛这些机制，但拓扑很小，时钟、路由、DNS、负载均衡和跨节点复制都被简化了。

分布式环境里的 partition 是拓扑问题。节点集合会被切成多个连通分量；每一边可能还有客户端、缓存、队列和存储。正确性语义通常要依赖多数派、term、epoch、lease、quorum read/write。etcd 的语义很清楚：多数派侧可用，少数派侧不可用；分区恢复后，少数派识别多数派 leader 并恢复状态。这个语义在单机多进程测试里可以模拟，但不能完全等同于真实多机。

单机测试更容易控制和复现。Toxiproxy 可以把某条 TCP 链路设为 down 或 timeout；本地日志可以精确对齐；测试运行快。缺点是它容易低估真实网络复杂度：没有跨可用区抖动，没有云负载均衡器连接迁移，没有 DNS TTL，没有节点时钟差，没有内核队列和真实带宽竞争。

分布式测试更接近生产，但解释更难。Chaos Mesh 可以在 Kubernetes 里按 Pod 和方向做 NetworkChaos；`tc-netem` 可以在节点网络接口上注入 delay/loss/reorder；但你必须记录具体作用范围。否则“发生了 network partition”这句话没有信息量。是谁到谁不通？单向还是双向？TCP 已有连接是否被 reset？DNS 是否可用？健康检查走不走同一条路径？

LogServe 当前的实验边界应该这样说：单机多进程 partition 可以证明机制设计，比如旧 worker 被 fence、幂等重试不重复提交、result ref 半成功可恢复；它不能证明多机生产网络下的容量、跨 AZ quorum、真实对象存储抖动或负载均衡行为。后者需要多节点部署、真实网络故障注入和更完整的观测。

面试回答可以收成一句：单机 partition 是机制验证，分布式 partition 是拓扑和 quorum 验证。前者适合快速暴露语义 bug，后者才适合支撑生产级可用性结论。两者都要做，但结论不能混用。
## 参考资料

- Go Standard Library, [testing package](https://pkg.go.dev/testing)
- Go Performance Tools, [benchstat](https://pkg.go.dev/golang.org/x/perf/cmd/benchstat)
- Go Proposal, [Benchmark Data Format](https://go.dev/design/14313-benchmark-format)
- Google Benchmark, [User Guide](https://github.com/google/benchmark/blob/main/docs/user_guide.md)
- OpenJDK JMH, [JMH samples](https://github.com/openjdk/jmh/tree/master/jmh-samples/src/main/java/org/openjdk/jmh/samples)
- OpenJDK JMH, [JMHSample_20_Annotations](https://github.com/openjdk/jmh/blob/master/jmh-samples/src/main/java/org/openjdk/jmh/samples/JMHSample_20_Annotations.java)
- OpenJDK JMH, [JMHSample_12_Forking](https://github.com/openjdk/jmh/blob/master/jmh-samples/src/main/java/org/openjdk/jmh/samples/JMHSample_12_Forking.java)
- pyperf, [Run a benchmark](https://pyperf.readthedocs.io/en/latest/run_benchmark.html)
- Grafana k6, [API load testing](https://grafana.com/docs/k6/latest/testing-guides/api-load-testing/)
- Grafana k6, [Running distributed tests](https://grafana.com/docs/k6/latest/testing-guides/running-distributed-tests/)
- Grafana k6, [Load test types](https://grafana.com/docs/k6/latest/testing-guides/test-types/)
- Grafana k6, [Open and closed models](https://grafana.com/docs/k6/latest/using-k6/scenarios/concepts/open-vs-closed/)
- Grafana k6, [Constant arrival rate](https://grafana.com/docs/k6/latest/using-k6/scenarios/executors/constant-arrival-rate/)
- HdrHistogram, [HdrHistogram README](https://github.com/HdrHistogram/HdrHistogram)
- Gil Tene, [wrk2 README](https://github.com/giltene/wrk2)
- Go Blog, [Profiling Go Programs](https://go.dev/blog/pprof)
- Go Standard Library, [runtime/pprof](https://pkg.go.dev/runtime/pprof)
- Go Standard Library, [net/http/pprof](https://pkg.go.dev/net/http/pprof)
- Go Standard Library, [runtime.SetBlockProfileRate](https://pkg.go.dev/runtime#SetBlockProfileRate)
- Go Standard Library, [runtime.SetMutexProfileFraction](https://pkg.go.dev/runtime#SetMutexProfileFraction)
- Brendan Gregg, [Flame Graphs](https://www.brendangregg.com/flamegraphs.html)
- Grafana k6, [Dropped iterations](https://grafana.com/docs/k6/latest/using-k6/scenarios/concepts/dropped-iterations/)
- Grafana k6, [Running large tests](https://grafana.com/docs/k6/latest/testing-guides/running-large-tests/)
- Grafana k6, [Injecting faults with xk6-disruptor](https://grafana.com/docs/k6/latest/testing-guides/injecting-faults-with-xk6-disruptor/)
- Go Standard Library, [runtime environment variables](https://pkg.go.dev/runtime#hdr-Environment_Variables)
- Go Standard Library, [runtime/metrics](https://pkg.go.dev/runtime/metrics)
- Linux Kernel Docs, [drop_caches](https://docs.kernel.org/admin-guide/sysctl/vm.html#drop-caches)
- Linux Kernel Docs, [Fault injection capabilities infrastructure](https://docs.kernel.org/fault-injection/fault-injection.html)
- AWS EC2, [Instance store temporary block storage for EC2 instances](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/InstanceStorage.html)
- AWS EBS, [Amazon EBS volume types](https://docs.aws.amazon.com/ebs/latest/userguide/ebs-volume-types.html)
- AWS EBS, [Amazon EBS I/O characteristics and monitoring](https://docs.aws.amazon.com/ebs/latest/userguide/ebs-io-characteristics.html)
- PostgreSQL Documentation, [Data Checksums](https://www.postgresql.org/docs/current/checksums.html)
- SQLite Documentation, [How To Corrupt An SQLite Database File](https://www.sqlite.org/howtocorrupt.html)
- etcd Documentation, [Data Corruption](https://etcd.io/docs/v3.6/op-guide/data_corruption/)
- Amazon S3 Documentation, [Checking object integrity](https://docs.aws.amazon.com/AmazonS3/latest/userguide/checking-object-integrity.html)
- Linux Kernel Docs, [CPU Performance Scaling](https://docs.kernel.org/admin-guide/pm/cpufreq.html)
- Linux man-pages, [uname(1)](https://man7.org/linux/man-pages/man1/uname.1.html)
- Brendan Gregg, [Systems Performance](https://www.brendangregg.com/systems-performance-2nd-edition-book.html)
- Brendan Gregg, [USE Method](https://www.brendangregg.com/usemethod.html)
- SPEC, [CPU 2017 Run and Reporting Rules](https://www.spec.org/cpu2017/Docs/runrules.html)
- etcd Documentation, [Failure modes](https://etcd.io/docs/v3.6/op-guide/failures/)
- Raft, [In Search of an Understandable Consensus Algorithm](https://raft.github.io/raft.pdf)
- Linux man-pages, [tc-netem(8)](https://man7.org/linux/man-pages/man8/tc-netem.8.html)
- Chaos Mesh Documentation, [Simulate Network Faults](https://chaos-mesh.org/docs/simulate-network-chaos-on-kubernetes/)
- Shopify Toxiproxy, [README](https://github.com/Shopify/toxiproxy)
