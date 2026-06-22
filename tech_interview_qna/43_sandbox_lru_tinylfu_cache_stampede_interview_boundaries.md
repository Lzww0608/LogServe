# 43. sandbox、LRU、TinyLFU 与 cache stampede 追问链

这一批继续按面试追问组织，不写百科条目。sandbox 关心的是隔离边界和逃逸面；LRU 和 TinyLFU 关心的是容量有限时谁能留下；cache stampede 关心的是缓存失效瞬间如何保护后端。它们有一个共同点：一句话定义都很容易把人带偏。说“容器就是沙箱”“LRU 就是淘汰最久没用的”“TinyLFU 就是 LFU”“加锁就能防击穿”，都只说到了表面。

## Q001. 面试官如果只问一个问题检验你是否理解 sandbox，可能会问什么？

**回答：**

我会预期他问这个问题：

```text
你要在平台里运行用户提交的 Python 代码。你说会放进容器 sandbox。请说明：这段代码能看到哪些文件、能访问哪些网络、能发起哪些 syscall、能拿到哪些 secret、能占用多少 CPU/内存/pid/磁盘、超时后如何清理子进程树；如果它尝试读宿主路径、访问 metadata service、fork 炸弹、加载 native 扩展或写爆 stdout，你的 sandbox 分别怎么拦？
```

这道题比“sandbox 是什么”更能看出理解深度。sandbox 不是一个单独开关，而是一组边界的组合：身份、文件系统、进程、网络、系统调用、资源、设备、环境变量、输出通道、临时目录和清理策略。任何一条边界没说清楚，攻击者或失控代码就会从那条路出去。

先看文件边界。用户代码的根目录应该是受控工作目录，最好是只读 base image 加独立可写层或临时目录。不能挂宿主源码、Docker socket、云凭证目录、SSH key、全局模型 cache 或其他租户的 result path。路径检查不能只看字符串，要看最终解析后的真实路径，防止 `../`、符号链接、zip-slip 和 bind mount 逃逸。

再看网络边界。默认允许所有 egress 是很危险的。用户代码可以访问云 metadata service、内网控制面、数据库、对象存储、日志系统，也可以把数据外带到公网。更稳的策略是按任务类型配置 egress allowlist，或者默认断网，只开放必要的对象存储、包源镜像和结果上报通道。DNS 也要算网络出口，不能只拦 HTTP。

系统调用边界不能只靠“跑在容器里”。seccomp 过滤能减少进程可调用的 kernel surface，但它自己不是完整 sandbox。它要和 capabilities、user namespace、mount namespace、pid namespace、network namespace、AppArmor/SELinux、只读 rootfs、no-new-privileges 一起用。尤其是 `CAP_SYS_ADMIN`、privileged、hostPID、hostNetwork、hostPath、device mount 这些配置，很多时候不是优化开关，而是把边界打开。

资源边界也要具体。CPU quota、memory.max、pids.max、open files、stdout/stderr 最大字节数、临时目录大小、运行时间、GPU 显存、磁盘 I/O 都要有限制。只限制 CPU 和内存不够，fork 炸弹可能先打爆 pid，日志循环可能先打爆磁盘或父进程内存，native 扩展可能通过设备或驱动扩大攻击面。

最后是生命周期。超时后不是杀一个 pid 就结束。用户代码可能 fork 子进程，子进程再 fork 孙进程。sandbox 应该有进程组、cgroup、job object 或容器边界，能在失败、超时、取消后清掉整个执行单元。清理动作本身要有指标：是否仍有残留进程，临时目录是否删除，网络连接是否关闭，输出是否截断，资源计数是否归零。

面试里可以这样回答：sandbox 的核心不是“用了容器”，而是把不可信代码能观察和影响的世界缩小到明确范围。容器、namespace、cgroup、seccomp、capabilities、LSM、只读文件系统、网络策略、资源上限和审计各管一部分。少了任何一层，都要能说出剩下的风险。

## Q002. sandbox 的一句话定义是否容易误导，误导点在哪里？

**回答：**

容易误导。常见定义是：sandbox 是一个隔离环境，用来安全运行不可信代码。这句话方向没错，但太容易让人以为只要把代码“放进去”，安全就自然成立。

第一个误导是把隔离环境当成单一边界。真实 sandbox 通常由多层组成。namespace 隔离看到的进程、网络、挂载点和用户 ID；cgroup 限制和统计资源；seccomp 限制 syscall；capabilities 收窄 root 权限；LSM 做更细的访问控制；容器 runtime 负责组合这些机制。每层都有盲区。比如 cgroup 能限制内存，不限制你读哪个文件；seccomp 能拦 syscall，不知道业务上哪个对象属于哪个租户。

第二个误导是把容器等同于强安全沙箱。容器共享宿主内核。对普通服务部署，它是很好的打包和隔离工具；对强对抗的不可信代码，它只是基础隔离。只要开启 privileged、挂 Docker socket、给 hostPath、暴露设备、保留过多 capabilities，容器就可能变成宿主控制入口。高风险场景通常要考虑 microVM、gVisor、Kata、Firecracker、Wasm runtime 或专用节点池。

第三个误导是只看入口，不看出口。很多人会限制用户代码读文件，却忘了 stdout、stderr、DNS、HTTP、错误日志、trace、对象存储 key、模型 cache 都可能是数据出口。sandbox 不只是防它拿宿主权限，也要防它把不该拿的数据带出去。

第四个误导是忽略资源耗尽。安全不只是逃逸。用户代码死循环、分配大内存、fork 大量进程、生成巨大日志、写满临时目录、占住 GPU，都能造成拒绝服务。一个 sandbox 如果只关心权限，不关心资源，仍然会被正常 API 打垮。

第五个误导是忽略可观测性。sandbox 拒绝了什么 syscall，哪个任务触发了 OOM，哪个租户的 egress 被拦，哪个镜像还在用宽松 profile，这些都要能看到。没有观测，sandbox 出问题时只能猜。

更准确的一句话是：sandbox 是一组用最小权限、隔离命名空间、资源配额、系统调用过滤、文件和网络策略，把不可信代码的可见世界和影响范围限制住的机制；它降低风险，不自动消除风险。

## Q003. sandbox 最常见的生产事故触发条件是什么？

**回答：**

sandbox 最常见的生产事故不是内核 0day，而是为了方便把边界开大了。开发调试时加的 privileged、hostPath、hostNetwork、Docker socket、`CAP_SYS_ADMIN`、宽松 seccomp profile、全量环境变量，进入生产后就变成逃逸和横向移动通道。

第一类事故是挂载错。宿主目录、源码目录、模型 cache、结果目录、服务账号 token、云凭证目录被挂进容器。用户代码本来只应该读自己的输入，结果能读到其他租户文件或控制面凭证。符号链接和归档解压也容易绕过目录限制，特别是 zip-slip 和 `../` 路径。

第二类是网络没收紧。很多平台默认给 sandbox 全量 egress。用户代码能访问 metadata service 拿云临时凭证，能扫内网端口，能访问 control plane，能把数据发到外部域名。后来加防火墙时，又忘了 DNS、IPv6、代理环境变量、包管理器镜像这些路径。

第三类是资源限制缺项。只配 memory limit，没配 pids limit，fork 炸弹照样打垮节点；只限制 CPU，没限制 stdout，日志能撑爆父进程或磁盘；只限制容器内存，没看 page cache、tmpfs、GPU 显存和文件描述符。资源限制要按攻击面列，不是只配一个 `--memory`。

第四类是 seccomp 或 capabilities 过宽。某些程序为了兼容，直接用 unconfined seccomp 或保留大量 capabilities。短期跑通了，长期等于给所有任务更大的 kernel surface。特别是允许 ptrace、mount、bpf、perf_event、keyctl、clone3、unshare 这类能力时，要非常谨慎。

第五类是镜像和依赖供应链。sandbox 运行的是“不可信代码”，但基础镜像、包管理器、native extension、模型 loader、postinstall script 也在边界内。用户能安装依赖时，安装阶段本身就可能执行代码。只沙箱运行阶段，不沙箱构建和安装阶段，边界仍然漏。

第六类是清理不彻底。任务超时后直接标记失败，但进程树、挂载、临时目录、cgroup、网络连接、GPU context 没清干净。下一次任务复用 runner 时，就可能继承旧状态，或者继续被后台进程消耗资源。

第七类是策略漂移。集群里有的节点启用了 seccomp，有的没有；有的 runtime 支持某个 profile，有的不支持；Kubernetes admission policy 没拦住旧 workload；手工紧急修复留下例外。生产事故常常来自少数例外，而不是主路径。

所以我会把触发条件总结成一句：sandbox 事故多半来自边界被调试便利、兼容性和例外配置一点点打开。真正要管理的是配置漂移、资源缺口和数据出口，而不是只盯着“容器是否启动”。

## Q004. sandbox 的指标应该怎么设计才不会只看平均值？

**回答：**

sandbox 指标不能只看平均执行时间。平均值看不出逃逸尝试、资源尖峰、策略漂移和清理失败。一个任务越界一次，就可能比一万次正常执行更重要。

第一组是策略覆盖率。多少任务启用了 seccomp、AppArmor/SELinux、non-root、no-new-privileges、read-only rootfs、drop capabilities、network policy、egress allowlist、pids limit、memory limit、CPU quota。要按节点、runtime、镜像、任务类型、租户拆。覆盖率不是全局 99% 就够，剩下 1% 可能正是高风险任务。

第二组是拒绝事件。seccomp denied syscall、LSM denied path、capability denied、network denied、DNS denied、filesystem denied、device denied。每个事件要带 task id、tenant、image digest、profile version、syscall/path/domain/reason。拒绝事件过少不一定是好事，可能是策略没生效。

第三组是资源高水位。CPU throttling、memory peak、OOM kill、pids.current/pids.max、open file count、tmpdir bytes、stdout/stderr bytes、disk write bytes、network bytes、GPU memory、execution wall time。看 p95、p99、max 和超限次数。资源攻击经常只发生在尾部。

第四组是清理结果。任务结束后 cgroup 是否为空、残留进程数、残留文件大小、挂载是否卸载、临时目录删除是否成功、网络 namespace 是否释放、GPU context 是否释放。清理失败要单独告警，不要埋在任务失败日志里。

第五组是配置漂移。profile hash、trust image digest、runtime version、kernel version、seccomp actions_avail、capability set、mount set、environment allowlist hash。sandbox 是否安全，取决于真实运行配置，不取决于 YAML 里写过什么。

第六组是数据出口。公网 egress 次数、未知域名访问、metadata IP 访问尝试、日志输出中疑似 secret、对象存储写入路径、跨租户 object prefix 拒绝。很多泄露不是通过文件读取暴露的，而是通过出口通道带走的。

第七组是用户体验指标。sandbox 初始化耗时、镜像拉取耗时、依赖安装耗时、任务启动 p99、冷启动比例、因策略误杀导致的失败率。安全策略如果误杀太多，团队会绕过它。指标要能区分真实攻击、用户代码 bug 和平台策略过紧。

面试里可以这样说：sandbox 看板要同时看“边界有没有启用、越界有没有被拦、资源有没有被限、清理有没有完成、配置有没有漂移”。平均执行时间只是性能背景，不证明 sandbox 安全。

## Q005. sandbox 的正确性边界和性能边界分别是什么？

**回答：**

sandbox 的正确性边界是“按策略限制进程能做什么”。它可以限制可见文件、可访问网络、可调用 syscall、可用资源、可见进程、可用设备和运行身份。它不能证明用户代码无害，也不能证明内核没有漏洞，更不能保证业务数据不会通过允许的通道被误用。

正确性上要先承认策略是白纸黑字的。你允许读某个目录，就要接受它能读目录里的所有可见内容；你允许访问某个域名，就要接受它可以通过这个域名发送数据；你允许某个 syscall，就要理解它可能被怎么组合。sandbox 没有读心能力，只执行策略。

第二，sandbox 不替代业务授权。即使文件系统隔离正确，应用层仍要校验 tenant、user、object id、result path。用户代码如果拿到一个合法 token，sandbox 不会自动知道这个 token 权限过大。凭证最小化和业务授权仍然要做。

第三，sandbox 不替代供应链安全。基础镜像、依赖包、模型文件、native extension、构建脚本都可能在 sandbox 内执行。sandbox 可以降低影响面，但不能证明 artifact 可信。digest、签名、来源、格式限制和构建隔离仍然需要。

第四，sandbox 不保证强对抗完全隔离。容器共享内核，seccomp 不是完整 sandbox，namespace 不是硬件虚拟化。高风险多租户和恶意代码执行场景，要考虑 microVM、专用节点、Wasm 或更强隔离。这个边界要主动说明，不能把容器宣传成安全结论。

性能边界主要来自启动和隔离开销。创建容器、mount namespace、拉镜像、加载 profile、建立网络 namespace、设置 cgroup、冷启动解释器，这些都会增加任务启动延迟。强隔离，比如 microVM，启动和内存开销可能更高，但换来更强边界。

运行时也有开销。seccomp 过滤每个 syscall，通常可接受，但高 syscall workload 会感知到；cgroup CPU quota 会引入 throttling，影响尾延迟；网络策略和代理会增加连接成本；只读文件系统和临时层会改变 I/O 行为；频繁创建销毁 sandbox 会给节点带来调度和清理压力。

所以我会这样总结：sandbox 正确性保证的是“策略定义的边界被执行”，不保证代码可信、内核无漏洞、业务权限正确或数据不会从允许出口流出；性能边界在启动、资源隔离、syscall 过滤、网络策略和清理成本上。安全越强，成本越需要被测量和容量规划。

## Q006. 面试官如果只问一个问题检验你是否理解 LRU，可能会问什么？

**回答：**

我会预期他问这个问题：

```text
缓存容量是 3，正常热点是 A、B、C，后台任务顺序扫描 D、E、F、G、H。用 LRU 会发生什么？如果每次命中都要把节点移动到链表头，高并发下又会发生什么？
```

这道题能同时问出 LRU 的假设和实现成本。LRU 的核心假设是时间局部性：最近访问的数据，未来更可能再次访问。缓存满时淘汰最久未访问的条目。这个假设在很多在线读场景里很好用，但它遇到顺序扫描会很脆。

按容量 3 来看，开始缓存是 `[A, B, C]`，它们都是热点。后台扫描 D 时，D 被放入缓存，最久未使用的一个热点被踢掉；继续扫描 E、F、G、H，缓存里逐步变成扫描数据。扫描结束后，缓存可能留下 `[F, G, H]` 这类不会再访问的冷对象，真正的 A、B、C 已经被挤出。正常请求回来时全部 miss。这就是 scan pollution。

LRU 的问题不是它“错”，而是它只看最近性，不看频率和对象价值。一次访问的 D 比长期高频但刚刚没访问的 A 更“新”，于是 D 被留下。对有后台扫描、批处理、导出、全量同步、分页遍历的系统，单纯 LRU 容易让离线流量污染在线缓存。

实现上还有一层问题。经典 LRU 是 hash map 加双向链表，get 命中后把节点移到表头，put 插入表头，容量超限淘汰表尾。理论上 O(1)，但高并发下每次 hit 都变成写操作。链表移动需要锁或复杂的并发结构，热点 key 会造成锁竞争和缓存行抖动。很多系统因此使用近似 LRU、分段 LRU、异步维护访问顺序或随机采样。Redis 的 LRU 就是近似采样，不是每次精确维护全局链表。

面试里可以这样回答：LRU 简单、直观、适合时间局部性强的 workload；它的边界是 scan、循环访问、频率热点被短期新对象冲刷，以及高并发下维护访问顺序的写放大。能说出这些，才算真正理解 LRU。

## Q007. LRU 的一句话定义是否容易误导，误导点在哪里？

**回答：**

容易误导。常见定义是：LRU 淘汰最久没有被访问的数据。这句话没错，但会让人忽略几个工程细节。

第一个误导是把“最久没访问”当成精确事实。很多生产系统并不实现精确 LRU。精确 LRU 需要每次访问都更新全局顺序，内存和并发成本都不低。Redis 这类系统使用随机采样近似 LRU，从少量候选里选最久未访问的淘汰。采样数越大越接近真实 LRU，成本也越高。

第二个误导是把最近性当成价值。LRU 默认最近访问过的数据更可能再次访问，但这只是经验假设。某个对象刚刚被访问一次，不代表它值得留；某个对象过去一小时被访问一万次，只要最近短暂没访问，也可能被淘汰。频率、对象大小、回源成本、租户权重、数据新鲜度都不在纯 LRU 的判断里。

第三个误导是忽略 scan 污染。顺序扫描、分页导出、批处理预热、冷数据巡检都可能把只访问一次的数据推到最近端，挤掉真正热点。LRU 对这种 workload 没有天然免疫力。

第四个误导是忽略循环访问。缓存容量是 N，工作集是 N+1，并按固定顺序访问时，LRU 可能接近 0 命中。每次要访问的对象都刚刚被上一轮淘汰。这类例子能很好地说明 LRU 不是“总比随机好”。

第五个误导是把 entry 数量当容量。真实缓存往往按 bytes、weight、显存、对象构建成本计量。一个 100MB 对象和一个 1KB 对象都算一个 entry，LRU 决策就会非常粗。对象大小差异大时，要看 byte hit rate 和 cost-weighted hit rate。

更准确的一句话是：LRU 是一种基于最近访问时间的淘汰启发式，适合时间局部性明显的 workload；生产实现常用近似或分段策略，并且要警惕 scan、循环访问、高并发维护成本和对象大小差异。

## Q008. LRU 最常见的生产事故触发条件是什么？

**回答：**

LRU 最常见的生产事故触发条件是流量形态突然从“热点重复访问”变成“高基数一次性访问”。这时 LRU 会把新来的冷对象当成最近对象保留，逐步挤掉真正热点。

第一类触发是后台扫描。报表导出、全量同步、数据校验、搜索爬取、离线任务、分页遍历，一次性访问大量 key。它们通常不是在线热点，但因为访问时间最新，会污染缓存。事故表现是扫描期间后端 QPS 上升，扫描结束后在线请求 hit rate 下降，p99 延迟变差。

第二类触发是工作集略大于缓存容量。比如缓存能放 100 万个 key，某个业务活动需要循环访问 110 万个 key。LRU 每次都淘汰马上要用的对象，命中率会比预期低很多。团队只看“缓存很大”，没有看 miss ratio curve，就容易误判容量。

第三类触发是热点迁移和突发流量。新活动开始，老热点还在，短时间内大量新 key 被访问一次。LRU 会给这些新 key 机会，但其中很多不会复用。没有准入策略时，主缓存会被冲刷。

第四类触发是对象大小差异。一个刚访问的大对象被留住，可能挤掉很多小热点。entry hit rate 还可能看起来不错，byte hit rate、saved latency 和后端成本却变差。

第五类触发是实现锁竞争。进程内精确 LRU 在高 QPS 下，每次 get 都要更新链表。热点越热，锁越热。最后缓存命中本来应该很快，反而在锁上排队。为了修命中率引入的 LRU，变成了新的 p99 来源。

第六类触发是 TTL 和 LRU 混用时没有分清原因。大量 key 不是被 LRU 淘汰，而是同一时间过期；团队误以为是容量不足，于是加内存。真正原因可能是 TTL 同步、缺少 jitter 或写入批次过于集中。

排查时我会先分清 miss 原因：absent、expired、evicted、invalidated、load failed。再按接口、租户、对象大小、扫描任务、时间窗口看 eviction。只看全局 hit rate，很难定位 LRU 事故。

## Q009. LRU 的指标应该怎么设计才不会只看平均值？

**回答：**

LRU 指标不能只看全局 hit rate。LRU 的风险在尾部、切片和污染。一个全局 95% 命中率的缓存，可能正在把某个租户、某类对象或某个关键接口打到 50%。

第一组是命中率切片。按 key namespace、接口、租户、对象大小、请求类型、在线/离线流量、读写类型看 request hit rate、byte hit rate、cost-weighted hit rate。命中便宜对象没有太大价值，命中昂贵查询才是真的节省成本。

第二组是淘汰指标。evictions per second、evicted key age、evicted object size、eviction reason、eviction victim 是否很快又被访问。被淘汰后短时间又 miss 的比例很重要，它说明策略可能踢错了对象。

第三组是扫描污染指标。单次访问 key 占比、高基数 key 增速、scan/batch 请求占用缓存比例、后台任务导致的 evictions、扫描后热点 key miss。可以给后台任务打标，单独看它们造成的缓存写入和淘汰。

第四组是容量和工作集。used bytes、entry count、average object size、p95 object size、working set estimate、miss ratio curve。只知道内存用了 80% 没用，要知道再加 10% 容量能减少多少 miss。

第五组是维护成本。LRU 锁等待、链表更新耗时、访问队列长度、异步 drain backlog、metadata bytes、CPU cycles per operation。对进程内缓存，维护 LRU 的成本可能比 hash lookup 本身更高。

第六组是 TTL 和一致性。expired_keys、evicted_keys、invalidated_keys 要分开。Redis 的 `keyspace_hits` 和 `keyspace_misses` 能算基本命中率，但还要结合 `evicted_keys`、`expired_keys`、内存水位和命令耗时。否则你看不出是容量淘汰、过期策略，还是主动失效造成 miss。

第七组是后端影响。cache miss 后的 load latency p95/p99、backend QPS、backend error rate、loader concurrency、singleflight collapse ratio。缓存策略好不好，最终要看有没有保护后端和降低尾延迟。

面试里可以这样答：LRU 指标要证明它留住了该留的数据，而不是只证明“有命中”。全局 hit rate 是入口指标，淘汰后再访问、扫描污染、byte/cost hit rate、维护成本和后端 offload 才能说明策略是否健康。

## Q010. LRU 的正确性边界和性能边界分别是什么？

**回答：**

LRU 的正确性边界很窄：当缓存容量不足时，它按最近访问顺序选择淘汰对象。它不保证缓存值是新的，不保证命中值有权限被当前用户读取，不保证对象值得保留，也不保证命中率最优。它只是一个淘汰启发式。

正确性上要把几件事分开。第一，LRU 不处理 freshness。值是否过期，要由 TTL、版本号、invalidation、write-through/write-behind 或源数据校验决定。第二，LRU 不处理多租户权限。缓存 key 如果没带 tenant、user、scope、schema version，就可能串租户。第三，LRU 不处理回源幂等和 stampede。miss 后怎么加载、是否 singleflight、是否限流，是另一层问题。

LRU 也不保证最优命中率。理论最优需要知道未来访问序列，LRU 只是用过去最近性猜未来。对时间局部性强的 workload，它很接近直觉；对 scan、循环访问、频率热点、对象大小差异，它可能很差。

性能边界主要来自两部分：查找和维护顺序。hash map 查找可以 O(1)，双向链表移动也可以 O(1)，但 O(1) 不等于便宜。每次 hit 更新链表是写操作，涉及锁、指针修改、缓存行失效、内存分配和异步队列。高并发热点 key 会把这些成本放大。

近似 LRU 是常见折中。随机采样减少全局顺序维护成本；分段 LRU 降低 scan 污染；异步记录访问减少读路径锁竞争；按权重淘汰处理对象大小。每种折中都会牺牲一部分精确性或增加复杂度。

如果缓存是远程 Redis，性能边界还包括网络 RTT、命令吞吐、内存碎片、淘汰周期、key 过期扫描和主线程阻塞。LRU 策略本身不是全部，缓存系统的执行模型同样重要。

我会总结为：LRU 正确性只覆盖“按最近性淘汰”，不覆盖新鲜度、权限、一致性和最优性；性能看似 O(1)，真实瓶颈在并发更新访问顺序、元数据内存、对象大小和远程缓存的执行成本。

## Q011. 面试官如果只问一个问题检验你是否理解 TinyLFU，可能会问什么？

**回答：**

我会预期他问这个问题：

```text
为什么 TinyLFU 不是简单把 LRU 换成 LFU？一个 miss 的新对象和一个缓存里的 victim 竞争时，TinyLFU 到底比较什么？为什么 W-TinyLFU 还要留一个小的 LRU window？
```

这道题能把 TinyLFU 的核心问出来：它主要是 admission policy，不是传统意义上“满了淘汰谁”的单一 eviction policy。传统缓存经常是 miss 后加载新对象，默认把它放进缓存，满了再踢一个旧对象。TinyLFU 多问一步：这个新对象真的值得进入缓存吗？

TinyLFU 会维护一个紧凑的近似频率结构，记录近期访问历史。新对象作为 candidate，要进入缓存时，会和即将被淘汰的 victim 比较估计频率。如果 candidate 的近期频率不高，就算刚刚被访问，也可能不准入。这样一次性扫描数据很难把长期热点挤出去。

这里的“频率”通常不是精确计数。TinyLFU 用 sketch 近似估计，常见实现会使用 Count-Min Sketch 一类结构，并做 aging 或 reset，让旧历史逐渐失效。它关心的是近期频率，不是从系统启动以来的永久频率。否则旧热点会留下历史包袱，新热点进不来。

W-TinyLFU 里的 window 很关键。纯 TinyLFU 对新对象可能太严格，因为新热点刚出现时频率还低。如果一开始就拿它和老热点比，可能会被拒绝。小的 LRU window 给新对象一个短期试用区，让 bursty 或刚变热的对象有机会被再次访问，证明自己。window 淘汰出的对象再进入主缓存准入竞争，主缓存通常用 SLRU 之类结构保护稳定热点。

所以完整回答是：TinyLFU 用近期频率估计做准入，保护缓存不被低复用对象污染；W-TinyLFU 加小窗口，是为了兼顾新热点的最近性和老热点的频率。它不是“把访问次数最少的踢掉”这么简单。

## Q012. TinyLFU 的一句话定义是否容易误导，误导点在哪里？

**回答：**

容易误导。常见定义是：TinyLFU 是一种轻量 LFU 缓存策略。这句话容易让人以为 TinyLFU 就是“用更省内存的方式统计频率，然后淘汰低频 key”。实际重点不在这里。

第一个误导是把 TinyLFU 当 eviction policy。TinyLFU 更准确地说是 admission policy。它决定新对象是否值得进入缓存。缓存满时具体 victim 怎么选，仍然可以由 LRU、SLRU 或其他策略完成。W-TinyLFU 就是 window LRU、main SLRU 和 TinyLFU admission 的组合。

第二个误导是把它当精确 LFU。TinyLFU 的频率是近似的，而且通常有时间衰减。它不是维护每个 key 的精确全历史计数。近似带来误判风险，也带来低内存和低 CPU 开销。工程上要接受这是一种概率和启发式策略。

第三个误导是忽略近期性。传统 LFU 容易被旧热点占住。TinyLFU 通过 aging/reset 关注近期历史，W-TinyLFU 又通过 window 接收短期 burst。它不是单纯偏向长期频率，而是在 recency 和 frequency 之间做折中。

第四个误导是把 Count-Min Sketch 等同于 TinyLFU。Sketch 只是估计频率的数据结构。TinyLFU 还包括 doorkeeper、aging、准入比较、和底层缓存结构的组合方式。只放一个 sketch，不等于实现了 TinyLFU。

第五个误导是以为 TinyLFU 能解决所有缓存问题。它不解决 TTL 新鲜度，不解决权限隔离，不解决缓存击穿，不解决大对象权重，不自动避免 loader 风暴。它只帮助决定哪些对象更值得占有限容量。

更准确的一句话是：TinyLFU 是一种基于近似近期频率的缓存准入策略，用很小的元数据判断 miss 加载出的 candidate 是否比缓存中的 victim 更值得保留，常和 LRU window、SLRU 主缓存组成 W-TinyLFU。

## Q013. TinyLFU 最常见的生产事故触发条件是什么？

**回答：**

TinyLFU 的事故通常来自两类误用：把它当万能策略，或者没有理解它的近似和衰减参数。它很强，但不是不需要调。

第一类触发是新热点启动被挡住。准入策略太严格，window 太小，aging 太慢，老热点频率还很高，新活动刚开始的对象频率低，进不来主缓存。表现是活动启动初期 miss 很高，后端被打，过一段时间才恢复。这个问题不是 TinyLFU 失效，而是 recency/frequency 平衡没调好。

第二类是高基数随机 key 污染 sketch。攻击流量、爬虫、参数没规范化、把 request id 放进 key，都会产生大量只访问一次的 key。TinyLFU 可以拒绝它们进入缓存，但 sketch 更新本身仍有成本，计数结构也可能被噪声污染。准入拒绝高不一定是坏事，但如果 CPU 被 sketch 更新打满，就变成新瓶颈。

第三类是 aging/reset 造成抖动。TinyLFU 需要让旧频率衰减。全局 reset 太粗，可能某个时刻 CPU 尖峰，或者频率突然整体下降导致准入行为抖动；衰减太慢又会保留旧热点；衰减太快则接近 LRU，保护不了长期热点。

第四类是对象大小和成本没纳入。TinyLFU 默认比较的是访问频率。如果一个大对象频率略高，却挤掉很多小而昂贵的热点，request hit rate 可能上升，byte hit rate 或 saved cost 下降。大对象缓存要考虑 size-aware 或 cost-aware 策略。

第五类是并发实现出问题。W-TinyLFU 要维护 window、probation、protected、frequency sketch 和准入移动。高并发下如果锁粒度、异步 drain、容量计数做不好，可能出现短暂超容量、重复节点、访问记录丢失、频率滞后。命中率异常只是表象，根因可能是维护路径跟不上 QPS。

第六类是指标误读。admission reject 很高可能是好事，说明扫描对象被挡住；也可能是坏事，说明新热点进不来。要结合被拒绝对象后续是否再次访问、后端 load、命中率和尾延迟判断。

我会总结：TinyLFU 最怕 workload 变化和参数失衡被平均命中率掩盖。上线时要做 trace-driven simulation，观察 window 大小、aging、sketch 误判、对象大小和 per-tenant 效果，不能只凭默认配置。

## Q014. TinyLFU 的指标应该怎么设计才不会只看平均值？

**回答：**

TinyLFU 指标除了看 hit rate，还必须看准入决策质量。因为 TinyLFU 的价值在“哪些 miss 加载结果不该进缓存”。只看命中率，看不出它拒绝得对不对。

第一组是准入指标。admission attempts、admitted、rejected、candidate estimated frequency、victim estimated frequency、reject 后 candidate 再次访问比例、admit 后对象命中次数。拒绝后很快又被访问，说明可能误拒；准入后再也不访问，说明可能误收。

第二组是分区指标。W-TinyLFU 要看 window、probation、protected 的 entry count、bytes、hit rate、eviction、promotion、demotion。window 命中高说明近期 burst 明显；protected 命中高说明稳定热点留住了。只看整体缓存看不出哪个区域失衡。

第三组是 sketch 指标。sketch update QPS、reset/aging 次数和耗时、counter saturation、估计频率分布、doorkeeper 命中、buffer drain backlog。热点 key 可能造成更新争用，随机 key 可能造成噪声。sketch 本身是热路径元数据，要当成系统组件看。

第四组是 workload 切片。按租户、接口、对象大小、key namespace、在线/离线流量、时间窗口看 request hit rate、byte hit rate、cost-weighted hit rate。TinyLFU 全局提升 2%，但把某个小租户打掉 30%，不能算健康。

第五组是污染和抗扫描。扫描任务写入尝试、被拒绝比例、扫描期间热点 key eviction、扫描后 hit rate 恢复时间。TinyLFU 的优势之一是抗污染，这要用指标证明。

第六组是后端保护。load latency p99、backend QPS、loader concurrency、singleflight collapse、error rate。TinyLFU 拒绝对象不代表用户请求失败，它只是避免缓存污染；真正目标还是降低 miss cost 和后端压力。

第七组是实现成本。缓存操作 CPU、锁等待、CAS retry、访问日志队列长度、维护线程延迟、metadata bytes per entry。TinyLFU 元数据小，但不等于没有成本。

面试里可以这样说：TinyLFU 的指标要回答“拒绝是否正确、窗口是否够用、旧频率是否及时衰减、扫描是否被挡住、后端是否减压”。全局平均命中率只是结果之一，不足以解释策略质量。

## Q015. TinyLFU 的正确性边界和性能边界分别是什么？

**回答：**

TinyLFU 的正确性边界是缓存准入决策。它用近似近期频率判断 candidate 是否值得进入缓存，减少低复用对象污染。它不保证最优命中率，不保证频率估计准确，不保证缓存值新鲜，也不保证业务一致性。

正确性上要先说近似。Sketch 会有误差，多个 key 可能碰撞，频率可能被高基数噪声抬高。TinyLFU 做的是概率意义上的更好选择，不是数学上永远正确的选择。误收和误拒都可能发生，所以需要 trace 和线上指标验证。

第二，TinyLFU 不处理 value 语义。对象过期、版本变更、权限变化、租户隔离、主动失效，都要由缓存 key、TTL、version、invalidation 和授权逻辑处理。TinyLFU 只决定容量竞争，不决定数据是否该被返回。

第三，TinyLFU 不等于防 stampede。它可能拒绝低频对象进入缓存，但 miss 回源时是否 singleflight、是否 stale-while-revalidate、是否限流，是 loader 层的职责。一个热点 key 过期时，TinyLFU 不能自动阻止几千个请求同时回源。

性能边界主要是元数据更新和维护路径。每次访问都可能更新 sketch 或访问日志。为了并发性能，很多实现会异步记录访问，再批量 drain 到策略结构里。这降低热路径成本，但会让频率估计滞后。滞后通常可接受，但在突发新热点场景里要观察。

内存边界也要算。TinyLFU 元数据比保留 ghost entries 的策略小，但仍然有 sketch、队列、分区、节点指针和权重信息。对象很小、QPS 很高时，元数据占比可能明显。

对象大小是另一个性能边界。按 entry 准入不一定按 byte 或 cost 最优。大对象缓存、模型 checkpoint、图片、视频片段这类场景，要把 size 和 miss cost 纳入策略，否则命中率指标会误导。

我会总结：TinyLFU 正确性覆盖“基于近似近期频率做准入”，不覆盖新鲜度、一致性、权限和回源并发；性能成本在 sketch 更新、aging、分区维护、并发同步和元数据内存上。它适合抗扫描和混合 workload，但要靠 trace 和指标调参。

## Q016. 面试官如果只问一个问题检验你是否理解 cache stampede，可能会问什么？

**回答：**

我会预期他问这个问题：

```text
一个热点 key 平时每秒 5 万次请求，TTL 是 60 秒。第 60 秒它过期了，数据库查询要 200ms。没有任何保护时会发生什么？你会用 singleflight、互斥锁、stale-while-revalidate、TTL jitter、预热和限流分别解决哪一段问题？
```

这道题能把 cache stampede 的本质问出来。stampede 不是单个 miss，而是很多请求在同一时间看到同一个 key miss 或 expired，然后一起回源。平时缓存挡住了 5 万 QPS，过期瞬间这些请求穿透到数据库。数据库变慢后，应用线程排队，客户端超时重试，更多请求堆积，最后从一个 key 的过期扩散成服务雪崩。

singleflight 解决的是“同一进程内同一个 key 只让一个请求加载”。第一个请求负责回源，其他请求等待它的结果。它能把并发加载从 N 降到 1，但只在同一进程或同一协调范围内有效。多实例部署时，还要考虑分布式锁、集中协调或让每个实例最多一个加载。

互斥锁或分布式锁解决跨进程协调，但要很谨慎。锁要有过期时间，持有者崩溃后不能永久卡住；锁超时不能太短，否则加载还没完成，第二批请求又进来；锁失败后等待、返回旧值、快速失败还是排队，都要明确。锁服务自己也可能成为瓶颈。

stale-while-revalidate 解决的是用户请求是否必须等回源。过期后，在允许的 stale 窗口内，可以先返回旧值，由后台刷新。这样用户 p99 更稳，后端压力也更低。代价是业务要接受短时间旧数据。权限、价格、库存、风控结果这类数据不一定能随便 stale。

TTL jitter 解决的是大量 key 同时过期。批量写入时如果统一 TTL，整批 key 会在同一秒过期。给 TTL 加随机抖动，可以把过期时间摊开。它不能解决单个超级热点 key 的击穿，但能减少大面积雪崩。

预热解决的是冷启动或发布后的缓存空窗。服务上线、缓存迁移、节点重启、活动开始前，可以提前加载关键热点。但预热要限速、分批、按版本校验，不能把后端预热打爆。

限流和熔断是最后的保护。即使缓存失效，也不能让无限请求打到数据库。可以对 loader concurrency、每 key 回源并发、每租户回源 QPS、全局后端 QPS 做上限。超过上限时返回旧值、降级值、排队或错误。

面试里可以这样答：cache stampede 的核心是“缓存失效把聚合流量瞬间释放到后端”。解决它不是一个锁就够，而是同 key 合并、跨实例协调、允许 stale、TTL 打散、预热和回源限流一起设计。

## Q017. cache stampede 的一句话定义是否容易误导，误导点在哪里？

**回答：**

容易误导。常见定义是：cache stampede 是缓存过期时大量请求同时打到后端。这句话对，但会让人漏掉几个关键边界。

第一个误导是把它等同于普通 miss。普通 miss 是单个请求没命中；stampede 是并发 miss 同时发生，问题在放大倍数。一个 key 的 miss 本来只需要一次回源，结果变成几千次回源。后端看到的是尖峰，不是平滑流量。

第二个误导是只怪 TTL 过期。stampede 也可能来自缓存节点重启、分片迁移、部署清空本地缓存、主动 invalidation、热点 key 被 LRU 淘汰、权限维度改 key、版本发布导致 key namespace 改变。只盯 TTL，会漏掉很多冷启动和失效路径。

第三个误导是把它和 cache avalanche 混在一起。stampede 更强调同一个热点 key 或少量热点 key 被并发回源；avalanche 更强调大量 key 同时失效或缓存层整体不可用。两者会互相触发，但治理重点不同。热点 key 需要 singleflight 和 per-key 限流；大量 key 同时过期需要 TTL jitter、分批刷新和容量保护。

第四个误导是以为加分布式锁就结束。锁只解决“谁加载”，不解决“等待者怎么办”。等待者可以阻塞、返回旧值、快速失败、降级或排队。不同选择影响用户 p99、后端压力和数据新鲜度。锁也会有超时、续约、持有者崩溃、时钟漂移和锁服务可用性问题。

第五个误导是忽略错误路径。如果第一个 loader 失败，等待的请求怎么办？全部重试会造成第二轮 stampede；全部返回错误会造成用户尖峰失败；返回 stale 要看业务能不能接受。没有负缓存和错误缓存时，不存在的 key 也会造成穿透式 stampede。

更准确的一句话是：cache stampede 是缓存失效、冷启动或淘汰后，多个请求对同一份昂贵数据同时回源，导致后端负载被放大；治理要处理合并加载、等待策略、旧值策略、错误策略和回源限流。

## Q018. cache stampede 最常见的生产事故触发条件是什么？

**回答：**

最常见的触发条件是热点 key 同步过期。活动页、配置、权限规则、排行榜、模型元数据、商品详情、用户画像这些 key 平时命中率很高，后端几乎看不到流量。一旦 TTL 到点，所有请求同时发现过期，后端瞬间承接原本被缓存吸收的流量。

第二类是批量写入导致过期时间对齐。部署后预热一批 key，统一 TTL 3600 秒；一小时后整批同时过期。或者定时任务整点刷新缓存，下一小时整点再次失效。监控上会看到非常整齐的周期性 p99 和后端 QPS 尖峰。

第三类是缓存层重启或扩缩容。本地缓存随进程重启清空，Redis 分片迁移导致大量 key miss，CDN 节点冷启动，sidecar cache 滚动更新。每个实例都觉得自己只是冷启动一点点，合起来就是全局回源风暴。

第四类是主动失效范围太大。数据变更后直接删除某个 namespace 或租户的所有缓存，没有分批重建，也没有旧值兜底。下一波请求全部变成 miss。强一致需求可以理解，但删除动作要配合限流和重建策略。

第五类是热点被淘汰。容量不足、LRU scan 污染、大对象挤压、小租户被大租户冲掉，都可能让热点 key 在 TTL 到期前被 evict。团队以为“TTL 还没到”，实际 key 已经因容量被踢掉。

第六类是回源变慢。stampede 不一定从请求数爆炸开始。有时只是数据库慢了，第一个 loader 卡住，等待队列变长，锁过期后第二个 loader 进来，接着第三个、第四个。回源慢会把一个受控 miss 放大成并发 miss。

第七类是不存在的 key 被反复请求。攻击者或爬虫构造高基数随机 key，每次都 miss，每次都查数据库。没有负缓存、布隆过滤器或参数校验时，这会表现成另一种穿透型 stampede。

排查时我会按时间对齐看：key 过期分布、evicted_keys、cache restart、deploy 时间、invalidation 事件、loader latency、singleflight waiters、backend QPS。如果尖峰和 TTL、发布、迁移或刷新任务对齐，基本就有方向了。

## Q019. cache stampede 的指标应该怎么设计才不会只看平均值？

**回答：**

cache stampede 的指标最怕平均化。全局 hit rate 99% 时，一个超级热点 key 过期仍然能把数据库打满。要围绕“同一个 key 在同一时间有多少请求回源”设计指标。

第一组是 per-key 或 hot-key 指标。top N key 的 QPS、hit/miss、expired、evicted、load count、waiter count、loader duration、stale served。不能给所有 key 做高基数 metrics label，但可以做 top N 聚合、日志采样或专门 hot-key 表。

第二组是回源合并指标。singleflight requests、shared result count、collapse ratio、inflight loads per key、duplicate loads prevented、lock acquisition success/failure、lock wait duration。collapse ratio 低说明大量请求仍在重复回源。

第三组是过期分布。TTL 到期时间直方图、同一秒过期 key 数量、批量写入 key 数量、TTL jitter 覆盖率、主动 invalidation key 数。要能看到过期是否集中，而不是只看当前 key 数。

第四组是 stale 策略指标。stale hit count、stale age p95/p99、background refresh success/failure、refresh duration、stale-while-revalidate window exhausted。允许 stale 时，必须证明旧值年龄在业务可接受范围内。

第五组是后端保护。loader concurrency、backend QPS、backend p99、DB connection pool wait、timeout、retry count、circuit breaker open、rate-limit dropped。stampede 的危害最终体现在后端排队和失败，不只体现在缓存层。

第六组是错误路径。load error rate、negative cache hit、not-found load count、error cached count、retry-after 返回次数。loader 失败后是否造成第二轮、第三轮回源，要有指标看。

第七组是冷启动。进程启动后的 cache warmup duration、本地缓存大小、warm key coverage、首次请求 miss rate、发布期间 miss rate。很多 stampede 来自滚动重启，每个实例都在冷启动。

第八组是用户尾延迟。按 key 热度、接口、租户看 p95/p99/p999，不要只看平均。cache stampede 通常影响最热路径，平均请求耗时可能只轻微变化。

面试里可以这样答：stampede 指标要看 per-key 并发回源、等待者数量、TTL 集中度、stale 年龄、singleflight 合并效果和后端排队。全局 hit rate 是背景，不能证明热点过期时系统安全。

## Q020. cache stampede 的正确性边界和性能边界分别是什么？

**回答：**

cache stampede 治理的正确性边界是“在缓存失效时控制回源并发和返回语义”。它不能自动让旧值变正确，也不能自动保证写后读一致。你选择等待刷新、返回 stale、返回降级值、返回错误，每一种都有业务语义。

如果返回 stale，就要定义最大 stale age、哪些字段允许 stale、哪些操作不能 stale。商品描述可能可以旧几分钟，库存、价格、权限、风控结果可能不能。stale-while-revalidate 是性能工具，不是正确性豁免。

如果用锁或 singleflight，就要定义等待者行为。等待多久，超时后怎么办，loader 失败怎么办，锁持有者崩溃怎么办，锁过期后是否允许第二个 loader，结果是否带版本。没有这些规则，锁只能减少一部分回源，不能保证用户看到合理结果。

如果用负缓存，也要定义 TTL。不存在的 key 缓存太久，会挡住刚创建的新数据；太短，又挡不住穿透。负缓存适合明确不存在或参数非法的场景，不适合所有错误。

如果主动失效，就要定义重建策略。删除缓存后同步回源、异步刷新、版本化 key、双写新旧 key、预热后切流，都会影响一致性和峰值。最危险的是大范围删除后让用户请求自然重建。

性能边界在回源成本和协调成本。singleflight 会让等待者排队，降低后端压力，但第一个 loader 的 p99 会传给所有等待者。分布式锁减少重复加载，但增加锁服务 RTT 和失败模式。stale-while-revalidate 降低用户等待，但可能增加后台刷新任务。TTL jitter 平滑过期，但不能解决单个超级热点。

容量边界也要看。为了防 stampede 保留旧值、负缓存和预热数据，会占用缓存空间。空间不足时，这些保护对象也可能被 LRU 踢掉。保护热点 key 需要容量策略配合，不只是 loader 策略。

我会这样总结：cache stampede 的正确性边界取决于失效时返回什么，以及旧值、错误和锁等待是否符合业务语义；性能边界取决于能否把同 key 回源并发压低、把过期时间摊开、让后端有上限，并接受 stale、锁和后台刷新带来的成本。