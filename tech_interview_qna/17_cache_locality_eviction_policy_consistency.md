# 17. 缓存、局部性、eviction policy 与一致性

这一章讨论的 cache，不只指 Redis 或本地 `map` 加 TTL。更准确地说，cache 是系统里一类用“更近、更快、容量更小的存储”换取访问延迟下降和后端压力下降的机制。它可以出现在很多层：

```text
CPU 层:
  L1/L2/L3 cache、TLB、page cache。

进程内:
  本地对象缓存、函数结果缓存、连接池元数据缓存。

服务层:
  Redis/Memcached、本地 sidecar cache、HTTP reverse proxy cache。

存储层:
  数据库 buffer pool、LSM block cache、对象存储 metadata cache。

分布式层:
  CDN、客户端缓存、边缘节点缓存。
```

面试里讲 cache，最容易犯的错误是只说“加缓存能变快”。这句话太粗。真正要解释清楚的是：缓存命中的收益是什么，miss 的成本是什么，数据是否会变旧，内存不够时淘汰谁，流量变化时策略是否还能适应，以及缓存层本身会不会成为新的瓶颈。

这份回答参考了 Redis 官方 key eviction 文档，Guava 与 Caffeine 的缓存文档，Caffeine 对 Window TinyLFU 的设计说明，ARC 与 TinyLFU 论文，以及常见计算机体系结构里关于 locality、cache line、working set、hit/miss 成本的基本模型。需要注意的是，不同系统里的 cache 语义差异很大：CPU cache 通常由硬件透明管理，数据库 buffer pool 更关心页和事务，Redis/Caffeine 这类应用缓存还要处理 TTL、淘汰、加载、并发和一致性。

## Q001. cache 的本质目标是什么？

**回答：**

cache 的本质目标是利用访问局部性，把一部分未来很可能再次使用的数据放在更快、更近的位置，从而减少平均访问成本。

这里的“成本”不只是一项延迟。它通常包括：

```text
latency:
  请求等待时间，例如从内存读 100ns，从远端数据库读 5ms。

throughput:
  后端能承受的请求量，例如数据库 QPS、对象存储 GET 次数。

CPU:
  重复计算、序列化、反序列化、压缩解压的开销。

network:
  跨进程、跨机器、跨地域的传输成本。

money:
  云存储请求费用、带宽费用、数据库实例规格。

availability:
  后端抖动时，缓存能不能暂时顶住读流量。
```

所以 cache 的目标不是“把所有东西放到内存里”。如果能放下所有数据，那更像是主存储或内存数据库，不再是典型 cache。cache 的难点正是容量有限：只能留下部分数据，必须决定哪些数据值得留、留多久、什么时候删、删了之后如何回源。

可以用一个很简单的模型理解 cache：

```text
平均成本 = hit_rate * hit_cost + miss_rate * miss_cost + cache_overhead
miss_rate = 1 - hit_rate
```

假设本地 cache 命中成本是 0.1ms，回源数据库成本是 20ms，cache 命中率是 90%，忽略其他开销：

```text
avg = 0.9 * 0.1ms + 0.1 * 20ms = 2.09ms
```

如果没有 cache，每次都要 20ms。这个收益很明显。但如果 miss 成本只有 0.2ms，而 cache 自己要加锁、序列化、维护淘汰队列，hit rate 再高也可能不划算。

cache 要解决的工程问题一般有五类。

第一，降低慢路径访问。比如用户资料从数据库读要 5ms，从本地内存读只要几十纳秒到几微秒。读多写少、重复访问明显的数据，很适合缓存。

第二，保护后端。热点 key 可能占据大部分读流量。如果每次都打数据库，会把数据库连接池、CPU、锁和磁盘 I/O 打满。缓存命中能把这部分流量挡在前面。

第三，复用计算结果。某些结果不是从数据库读，而是由 CPU 计算、模型推理、复杂查询、图遍历得来。缓存可以避免重复计算。

第四，吸收短期抖动。后端偶尔慢，cache hit 仍然可以返回结果。这里就会涉及 stale data 的接受程度：宁愿返回稍旧的数据，还是宁愿失败或等待。

第五，改变访问形态。比如把很多小请求合并成一次批量回源，把跨地域读变成本地边缘节点读，把随机远端 I/O 变成本地内存查找。

但是 cache 也会引入新的成本：

```text
内存成本:
  缓存本身占内存，元数据也占内存。

一致性成本:
  源数据更新后，缓存里的旧值怎么办。

复杂度成本:
  TTL、失效、淘汰、回源、预热、击穿、穿透、雪崩。

尾延迟成本:
  cache miss 可能排队回源，多个 miss 可能同时打爆后端。

观测成本:
  需要看 hit rate、miss penalty、eviction、expired、load latency。
```

所以面试里不要把 cache 描述成一个“万能加速层”。更好的说法是：cache 是一种利用局部性降低平均成本的权衡。它适合读多、重复访问、源数据较慢、可接受短暂不一致或有明确失效机制的场景；不适合强一致、低复用、写多读少、数据体积远超内存且访问均匀的场景。

举一个后端场景：订单详情接口从数据库读订单、用户、商品、优惠信息。某些用户会反复刷新订单页，某些热门商品被大量订单引用。缓存商品基础信息和用户展示信息是合理的；缓存订单支付状态就要谨慎，因为状态更新后旧缓存可能导致错误展示。

面试里可以这样答：

```text
cache 的本质目标是利用局部性，把未来大概率还会用的数据放到更快、更近的地方，降低平均访问成本和后端压力。它不是为了保存全部数据，而是在容量有限的情况下选择最值得保留的数据。判断 cache 是否有价值，要看 hit cost、miss cost、hit rate、维护成本、一致性成本和内存成本。命中率高不一定就好，关键是命中节省的成本能不能覆盖缓存本身带来的复杂度。
```

## Q002. temporal locality 和 spatial locality 有什么区别？

**回答：**

temporal locality 是时间局部性，关心“同一个东西最近被访问过，短时间内可能还会再访问”。spatial locality 是空间局部性，关心“访问了某个位置，附近位置短时间内可能也会被访问”。

这两个概念来自计算机体系结构，但在后端缓存里同样适用。

时间局部性的例子：

```text
用户刚查过 profile，几秒内又刷新页面；
热门商品详情被大量用户反复访问；
权限配置被每个请求读取；
某个 feature flag 在一个进程内频繁判断；
数据库查询结果在短时间内被相同参数重复请求；
```

这类场景适合用 LRU、TTL、refresh-after-write、本地内存缓存等机制。LRU 的基本假设就是：最近用过的 key，未来还可能用。

空间局部性的例子：

```text
CPU 读取数组 a[i] 后，很可能继续读 a[i+1]；
数据库读一个 page 后，同一个 page 里的其他行可能也会用到；
对象存储读一个 manifest 后，后续会读同一目录下多个对象；
GraphQL 查询用户信息时，通常还会读用户的组织、角色、头像；
搜索页加载第一页后，用户可能马上点第二页；
```

空间局部性经常通过“块”或“批量”来利用。CPU cache 不是只加载一个字节，而是加载一个 cache line；数据库 buffer pool 以 page 为单位缓存；应用层可以做 batch load、read-ahead、prefetch、`getAll(keys)`。

两者的区别可以这样看：

```text
temporal locality:
  复用同一个 key。
  典型问题是“最近用过的还会不会再用”。
  常见策略是 LRU、LFU、TTL、refresh。

spatial locality:
  复用相邻或相关 key。
  典型问题是“读了 A 后，附近的 B/C/D 要不要一起拿”。
  常见策略是 cache line、page、prefetch、batch、read-ahead。
```

有些系统同时利用两者。比如数据库读一行时，实际会把整页放入 buffer pool。这利用了空间局部性。这个 page 最近被访问过后，后续继续留在 buffer pool，又利用了时间局部性。

后端系统里最常见的误判，是把空间局部性做成过度预取。比如用户请求商品 A，系统顺手把同类目 1000 个商品都加载进缓存。如果用户并不会访问这些商品，就会带来几个问题：

```text
污染缓存:
  把真正热点挤出去。

放大回源:
  一个请求变成一批数据库请求。

增加尾延迟:
  预取阻塞正常返回。

浪费带宽:
  拉取了没人用的数据。
```

时间局部性也可能失效。比如一次性批处理扫描 1 亿条记录，每条记录只读一次。LRU 会认为“最新读到的最值得留”，于是不断把旧热点挤掉。扫描结束后，缓存里全是不会再用的数据。这就是 scan workload 对 LRU 不友好的根本原因。

在面试里可以把两种局部性和 cache 粒度联系起来：

```text
按 key 缓存:
  主要利用时间局部性。

按 page/block 缓存:
  利用空间局部性，也可能利用时间局部性。

按 group/batch 缓存:
  依赖业务相关性，空间不一定是地址连续，也可能是逻辑相邻。

按 query result 缓存:
  主要利用参数重复访问的时间局部性。
```

面试里可以这样答：

```text
temporal locality 是同一个数据最近被访问过，短时间内可能还会被访问；spatial locality 是访问某个数据后，附近或相关的数据可能也会被访问。LRU、TTL、本地对象缓存主要吃时间局部性；cache line、数据库 page、batch load、prefetch 主要吃空间局部性。时间局部性失效时，最近访问不代表未来会访问；空间局部性判断错时，预取会污染缓存并放大后端压力。
```

## Q003. cache hit rate 是否一定能代表性能提升？

**回答：**

不一定。cache hit rate 是重要指标，但它不能单独代表性能提升。命中率只告诉你“请求中有多少比例在缓存里找到了数据”，没有告诉你命中节省了多少成本，也没有告诉你 miss 有多贵、缓存维护有多贵、尾延迟有没有改善。

最简单的反例是：命中率很高，但命中的都是便宜请求。

比如一个接口有两类数据：

```text
A 类:
  90% 请求，回源成本 0.2ms。

B 类:
  10% 请求，回源成本 200ms。
```

如果缓存只命中 A 类，hit rate 可以达到 90%，但总体延迟仍然被 B 类 miss 拖住。反过来，如果缓存只命中 B 类，hit rate 只有 10%，平均延迟可能下降很多。

所以更有解释力的指标是 cost-weighted hit rate 或 saved cost：

```text
saved_cost = sum(hit_count[key] * miss_cost[key]) - cache_overhead
```

对于对象大小差异很大的场景，还要看 byte hit rate。CDN 和对象缓存里，一个 1KB 对象和一个 1GB 视频片段的命中价值完全不同。按请求数看命中率，可能很高；按字节看命中率，可能很低。

第二个问题是 miss penalty。Redis 官方文档会用 `keyspace_hits` 和 `keyspace_misses` 算命中率，但实际性能还要结合 `evicted_keys`、`expired_keys`、内存使用、回源延迟、命令耗时一起看。命中率从 80% 到 90% 是否有价值，取决于剩下 10% miss 的成本。

第三个问题是 cache 本身有开销。

命中也不是免费的：

```text
本地 cache:
  hash 查找、锁竞争、对象引用、GC 压力。

远端 cache:
  网络 RTT、序列化、连接池、Redis CPU、排队。

带压缩 cache:
  解压缩成本。

带加密 cache:
  加解密成本。

带指标 cache:
  统计和采样开销。
```

如果原始计算非常便宜，cache hit 反而可能比直接计算更慢。比如一个简单字符串拼接、一次内存 map 查询、一次本地小函数计算，为它访问 Redis 通常不划算。

第四个问题是 hit rate 可能掩盖尾延迟。

平均命中率 99% 看起来很好，但 1% miss 如果同时发生，可能触发 cache stampede：大量请求一起回源，数据库被打满，p99/p999 变差。用户看到的是尾延迟，不是平均命中率。

第五个问题是命中数据可能是错的。

如果缓存一致性没做好，hit rate 越高，错误数据被返回得越稳定。比如库存、权限、支付状态、风控结果，命中旧值可能比 miss 更危险。此时性能指标不能脱离 correctness 指标：

```text
stale hit rate；
缓存值版本落后程度；
读到旧权限的次数；
缓存回源失败后的降级次数；
```

第六个问题是命中率可能被请求形态扭曲。

例如一个服务把空结果也缓存了，缓存穿透少了，hit rate 提高了；但如果 TTL 太长，真实新数据出现后仍然返回空结果，这不是纯性能问题，而是一致性问题。

面试里分析缓存指标时，可以用这组指标：

```text
hit rate:
  命中请求比例。

byte hit rate:
  命中字节比例。

miss penalty:
  miss 平均/分位回源成本。

load latency:
  cache loader 的耗时。

eviction rate:
  因容量不足被踢掉的速度。

expiration rate:
  因 TTL 到期消失的速度。

stale rate:
  命中但过期或版本落后的比例。

backend offload:
  后端 QPS 实际下降多少。

tail latency:
  p95/p99/p999 是否下降。
```

面试里可以这样答：

```text
hit rate 不能单独代表性能提升。要看命中的请求原本有多贵、miss penalty 有多大、缓存自身是否引入网络和锁开销、对象大小是否不同、尾延迟有没有下降，以及命中的值是否正确。一个 90% hit rate 的缓存，如果只命中便宜请求，价值可能很低；一个 10% hit rate 的缓存，如果命中的是最贵的慢查询，价值可能很高。所以我会同时看 hit rate、byte hit rate、saved cost、miss latency、backend offload、eviction/expiration 和 stale rate。
```

## Q004. cache miss 的成本如何建模？

**回答：**

cache miss 的成本不能只写成“回源一次”。真实系统里，一个 miss 通常包含排队、回源、计算、写入缓存、并发合并、失败重试和对后端的影响。建模时至少要分成直接成本和放大成本。

最基础的模型是：

```text
read_cost = hit_rate * hit_cost + miss_rate * miss_cost
```

其中：

```text
hit_cost:
  从 cache 读取并返回的成本。

miss_cost:
  发现 miss、回源、加载、写 cache、返回结果的成本。
```

但这个模型太粗。更接近工程实际的 miss 成本可以拆成：

```text
miss_cost =
  cache_lookup_cost
  + backend_queue_time
  + backend_fetch_time
  + compute_or_decode_time
  + cache_insert_cost
  + response_serialize_cost
  + coordination_cost
```

如果是 Redis 旁路缓存：

```text
miss path:
  1. 查 Redis，没有 key。
  2. 查数据库。
  3. 反序列化数据库结果。
  4. 序列化成缓存格式。
  5. SET Redis + TTL。
  6. 返回给调用方。
```

这里每一步都可能排队。数据库连接池满了，miss 的成本会从几毫秒变成几百毫秒；Redis 写入慢了，miss 也会变慢；如果多个请求同时 miss 同一个 key，还会出现击穿。

所以还要建模并发放大。

假设一个热点 key 过期，瞬间有 1000 个请求进来。如果没有 singleflight、互斥加载或请求合并，这 1000 个请求都会回源。此时 miss 成本不是“一次数据库查询”，而是：

```text
total_backend_cost = concurrent_miss_count * backend_fetch_cost
```

更糟的是，后端过载后每次 fetch 变慢，慢又导致更多请求堆积，形成正反馈：

```text
miss 变多 -> 回源变多 -> 后端变慢 -> 请求堆积 -> 更多超时和重试 -> 后端更慢
```

这也是缓存雪崩和热点击穿的本质。

对不同数据大小，还要考虑对象成本。

```text
small object:
  主要成本可能是网络 RTT 和序列化固定开销。

large object:
  主要成本可能是带宽、内存拷贝、压缩解压、GC。

variable size object:
  eviction 应该考虑 size，不然一个大对象可能挤掉大量小热点。
```

如果缓存的是计算结果，miss 成本还包括 CPU 和锁：

```text
miss_cost = compute_cpu + dependency_io + lock_wait + allocation + insert
```

例如权限系统缓存用户权限。miss 后要读用户、角色、组织、策略，再做规则合并。这个成本不是单次数据库读，而是一个依赖图。

还要把失败成本放进模型。

miss 时后端可能失败：

```text
backend timeout；
数据库连接池耗尽；
对象存储 5xx；
反序列化失败；
cache set 失败；
```

这时系统要决定：返回错误，返回旧值，降级为空，还是重试。每个选择都影响用户体验和后端压力。

更完整的模型可以写成：

```text
expected_cost =
  P(hit_fresh) * cost_hit
  + P(hit_stale_accepted) * cost_stale_hit
  + P(miss_single) * cost_miss_single
  + P(miss_concurrent) * cost_miss_amplified
  + P(load_fail) * cost_failure
  + maintenance_cost
```

这个模型的意义不在于数学精确，而是提醒你不要只看命中率。要把 miss 的形态分清楚。

实际观测时，建议记录：

```text
cache_lookup_latency；
miss_load_latency；
backend_queue_time；
backend_fetch_latency；
cache_set_latency；
load_success/load_error；
singleflight_shared_count；
concurrent_load_count；
miss_by_reason: absent / expired / evicted / invalidated；
```

`absent`、`expired`、`evicted`、`invalidated` 都是 miss，但含义不一样。因为容量不足被 evict，说明内存或淘汰策略有问题；因为 TTL 到期，说明 freshness 策略在起作用；因为主动 invalidation，说明一致性流程触发了。

面试里可以这样答：

```text
cache miss 的成本应该拆成查缓存失败、后端排队、回源读取或计算、结果序列化、写回缓存和并发协调成本。简单公式是 avg = hit_rate * hit_cost + miss_rate * miss_cost，但真实系统还要考虑并发 miss 放大、热点 key 过期、后端过载、对象大小、失败重试和旧值降级。建模时我会区分 absent、expired、evicted、invalidated 这些 miss 原因，并观测 miss_load_latency、backend_queue_time、singleflight 合并次数和后端 QPS。
```

## Q005. cold cache、warm cache、hot cache 分别是什么意思？

**回答：**

cold cache、warm cache、hot cache 描述的是缓存中有多少“对当前 workload 有用的数据”。它们不是严格标准术语，但在系统设计和故障排查里很常用。

cold cache 是冷缓存。意思是缓存刚启动、刚清空、刚扩容、刚迁移，里面几乎没有当前请求需要的数据。此时大部分请求都会 miss，后端压力会突然上升。

典型场景：

```text
服务重启，本地内存缓存为空；
Redis flush 或 key 被批量删除；
新节点加入集群，还没有加载热点；
部署后缓存版本 key 变化，旧缓存全部失效；
数据库 buffer pool 刚启动；
CDN 新边缘节点还没有内容；
```

cold cache 的风险是回源洪峰。正常情况下，缓存挡住 90% 读流量；一旦缓存冷启动，后端瞬间承受 10 倍流量。很多线上事故不是数据库突然变慢，而是缓存集群重启后数据库被冷启动流量打满。

warm cache 是暖缓存。意思是缓存已经装入了一部分常用数据，命中率开始接近稳定状态，但还没有完全达到最佳。它可能来自自然流量逐渐填充，也可能来自预热任务。

典型场景：

```text
服务启动后预加载配置、权限、热门商品；
新 Redis 分片上线后导入热点 key；
数据库重启后执行 warmup query；
CDN 提前 push 热门内容；
```

warm cache 的目标是避免冷启动尖峰。预热不是简单把所有数据都塞进去，而是根据最近访问日志、业务热点、排行榜、租户分布选择一批高价值 key。预热太多也会污染缓存，把真正在线流量需要的数据挤掉。

hot cache 是热缓存。意思是缓存里已经保留了当前 workload 的热点工作集，命中率和延迟都比较稳定。它不是“缓存里数据很多”，而是“缓存里有用的数据多”。

判断 hot cache 要看：

```text
hit rate 是否稳定；
miss latency 是否稳定；
eviction rate 是否正常；
热点 key 是否留得住；
后端 QPS 是否被有效削峰；
p95/p99 是否下降；
```

有一个容易混淆的点：hot cache 不等于 hot key。hot cache 是整体缓存处于热状态；hot key 是某个 key 被极高频访问。hot key 可能导致单分片 Redis、单本地锁、singleflight、网络连接成为瓶颈。也就是说，缓存热了不代表系统一定健康。

可以用一个时间线理解：

```text
T0:
  cache empty，cold。

T1:
  流量开始进入，miss 多，后端压力高。

T2:
  热点逐渐进入 cache，warm。

T3:
  工作集稳定，hit rate 稳定，hot。

T4:
  流量模式变化或大批 key 过期，重新变冷或局部变冷。
```

工程上要特别关注“从 cold 到 hot 的过程”。常见手段包括：

```text
预热:
  根据访问日志加载 top N key。

分批上线:
  新节点逐步接流量。

TTL jitter:
  避免同一时间大批 key 过期。

singleflight:
  同一个 key 同时 miss 时只让一个请求回源。

stale-while-revalidate:
  允许短时间返回旧值，后台刷新。

限流和熔断:
  缓存冷启动时保护后端。
```

面试里可以这样答：

```text
cold cache 是缓存里几乎没有当前 workload 需要的数据，常见于重启、清空、扩容或版本切换后，风险是大量 miss 同时回源；warm cache 是已经通过自然流量或预热装入了一部分热点，命中率在上升；hot cache 是缓存里保留了稳定工作集，命中率和延迟都比较稳定。这里的 hot 指缓存状态，不等于某个 hot key。设计上要关注冷启动到热状态的过渡，用预热、分批接流、TTL jitter、singleflight 和 stale-while-revalidate 保护后端。
```

## Q006. LRU 的基本思想是什么？

**回答：**

LRU，Least Recently Used，基本思想是：如果缓存满了，就淘汰最久没有被访问的条目。它背后的假设是时间局部性：最近访问过的数据，未来更可能再次访问；很久没访问的数据，未来再次访问概率较低。

一个典型 LRU cache 有两个核心结构：

```text
hash map:
  key -> node，用于 O(1) 查找。

doubly linked list:
  按最近访问顺序排列，用于 O(1) 移动和淘汰。
```

常见操作是：

```text
get(key):
  如果 key 不存在，miss。
  如果 key 存在，返回 value，并把节点移动到链表头部。

put(key, value):
  如果 key 已存在，更新 value，并移动到头部。
  如果 key 不存在，插入头部。
  如果容量超限，删除链表尾部节点。
```

链表头部表示最近使用，尾部表示最久未使用。

举个例子，容量为 3：

```text
访问 A:
  [A]

访问 B:
  [B, A]

访问 C:
  [C, B, A]

访问 A:
  [A, C, B]

访问 D:
  淘汰 B，变成 [D, A, C]
```

LRU 的优势很直接。

第一，容易理解。最近访问的留下，最久没访问的删，符合很多业务场景的直觉。

第二，可以做到 O(1) 操作。hash map 加双向链表是经典实现，很多面试题也会围绕这个展开。

第三，对时间局部性强的 workload 效果不错。比如用户反复打开同一个页面、配置项频繁读取、热门商品反复访问。

第四，不需要提前知道访问频率。LFU 要维护频率，ARC/TinyLFU 要更多元数据，LRU 只看访问顺序。

但 LRU 也有明显边界。

第一，它只看 recency，不看 frequency。一个 key 过去一小时被访问了 1 万次，只要最近一段时间没被访问，就可能被淘汰；另一个 key 只访问一次，但刚刚访问过，会被留在前面。

第二，它容易被 scan 污染。一次顺序扫描大量冷数据，会把热点挤出去。

第三，它对周期访问不一定好。假设缓存容量是 3，访问序列是 A B C D A B C D，每个 key 都会在再次访问前被淘汰，命中率可能是 0。

第四，精确 LRU 在高并发下有维护成本。每次 hit 都要更新访问顺序，意味着写链表、加锁或做异步维护。Redis 官方文档就说明 Redis 不是实现精确 LRU，而是随机采样若干 key，选择其中最久未使用的淘汰，以减少内存和 CPU 成本。

工程实现中，LRU 常常会做近似：

```text
sampled LRU:
  随机采样若干候选，淘汰其中最旧的。

segmented LRU:
  分 probation/protected 区，二次访问才进入保护区。

clock:
  用 reference bit 近似最近访问，降低链表维护成本。

async LRU:
  访问记录异步写入队列，后台维护顺序。
```

面试里如果问 LRU，可以先讲“hash map + double linked list”，再主动补一句工程边界：真实系统未必用精确 LRU，因为每次命中都维护全局顺序会带来锁竞争和元数据成本。

面试里可以这样答：

```text
LRU 的思想是利用时间局部性：最近访问过的数据未来更可能再访问，所以缓存满时淘汰最久没访问的条目。经典实现是 hash map 加双向链表，get 命中后把节点移到表头，put 插入表头，容量超限时淘汰表尾。它简单、O(1)、对时间局部性强的场景效果好，但只看最近性不看频率，容易被 scan workload 污染；高并发系统里精确 LRU 还会有锁和元数据维护成本，所以 Redis 等系统会使用近似 LRU。
```

## Q007. LRU 在 scan workload 下为什么可能表现差？

**回答：**

LRU 在 scan workload 下表现差，是因为它把“刚刚访问过”误判成“未来还会访问”。顺序扫描大量只访问一次的数据时，这些数据会不断进入 LRU 的最近端，把真正的热点挤出去。扫描结束后，缓存里留下的可能是一堆不会再访问的冷数据。

举个例子，缓存容量是 3，原本有热点：

```text
cache = [A, B, C]
```

A、B、C 是频繁访问的热 key。此时来了一次批处理扫描：

```text
D, E, F, G, H
```

按 LRU：

```text
访问 D:
  淘汰 C，cache = [D, A, B]

访问 E:
  淘汰 B，cache = [E, D, A]

访问 F:
  淘汰 A，cache = [F, E, D]

访问 G:
  淘汰 D，cache = [G, F, E]

访问 H:
  淘汰 E，cache = [H, G, F]
```

扫描结束后，A、B、C 这些热点没了，D/E/F/G/H 又不会再用。接下来正常在线请求访问 A、B、C，会全部 miss。这个过程叫 cache pollution，缓存污染。

scan workload 在很多系统里都很常见：

```text
后台任务全表扫描；
数据迁移逐条读取；
报表任务扫历史数据；
预热脚本不加筛选地扫全量 key；
分页接口被爬虫顺序拉取；
对象存储按目录批量遍历；
```

LRU 的问题在于它只记录最近一次访问，无法区分：

```text
刚刚访问一次，但以后不会再访问；
刚刚访问一次，而且很快会再访问；
过去频繁访问，只是最近短暂没访问；
```

对 LRU 来说，这三类访问在“最近性”上可能没有足够区别。

scan workload 还有一个变体：循环扫描。如果工作集大小略大于缓存容量，LRU 可能出现持续抖动。

比如容量 3，访问序列：

```text
A B C D A B C D A B C D
```

每个 key 再次访问前，刚好被其他 key 挤出去。结果可能每次都是 miss。这叫 thrashing 或 cache churn。

解决思路通常有几类。

第一，绕过缓存。明确知道某类请求是一次性扫描，就不要污染主缓存：

```text
后台导出任务不写入在线缓存；
批处理读使用 no-cache 标记；
数据库顺序扫描使用单独 buffer 策略；
```

第二，用 admission policy。不是所有 miss 的数据都立刻放进缓存。TinyLFU 的核心思想就是先看这个新对象在近期历史里是否足够频繁，再决定要不要接纳它。一次性扫描对象频率低，就不容易进入主缓存。

第三，用分段策略。SLRU/2Q 这类策略把缓存分成 probation 和 protected。新对象先进入 probation，只有再次被访问才进入 protected。这样一次性扫描数据很难挤掉真正二次访问过的热数据。

第四，用 ARC 这类自适应策略。ARC 同时跟踪 recent 和 frequent，并用 ghost list 观察被淘汰对象是否又被访问，从而在 workload 变化时调节 recency/frequency 的比例。ARC 论文明确把 scan resistance 作为优势之一。

第五，对后台扫描限速或隔离缓存。在线请求和离线任务不要共享同一个有限缓存空间，或者给离线任务单独 cache pool。

面试里可以这样答：

```text
LRU 在 scan workload 下差，是因为顺序扫描会把大量只访问一次的冷数据放到最近端，逐步挤掉真正的热点。扫描结束后，缓存里留下的是不会再用的数据，正常请求反而 miss 增加。根因是 LRU 只看最近性，不看频率和访问是否可复用。解决方式包括让扫描绕过缓存、给后台任务单独缓存、使用 SLRU/2Q 的 probation 区、使用 TinyLFU admission policy，或者用 ARC 这类同时考虑 recency 和 frequency 的策略。
```

## Q008. LFU 的优势和缺点是什么？

**回答：**

LFU，Least Frequently Used，基本思想是：缓存满时淘汰访问频率最低的条目。它关注的是 frequency，而 LRU 关注的是 recency。

LFU 的优势是能保留长期热点。对于访问分布稳定、热点集中、符合 Zipf 或幂律分布的 workload，LFU 往往比 LRU 更稳。比如：

```text
热门商品；
热门视频；
热门文章；
公共配置；
城市/地区字典；
少量大客户的高频租户配置；
```

如果某个 key 在一天里被访问了 100 万次，LFU 会给它很高的频率计数。短时间的扫描或偶发冷请求，不容易把它挤出去。

这就是 LFU 相对 LRU 的核心优势：抗短期污染。LRU 可能因为一次扫描丢掉热点；LFU 会认为扫描对象频率低，不值得保留。

但 LFU 的缺点也很明显。

第一，历史包袱重。一个对象过去很热，现在已经不热了，如果频率不衰减，它仍然会长期占着缓存。这在热点变化快的业务里很糟：

```text
昨天的热搜；
已经下架的商品；
直播结束后的直播间信息；
活动结束后的优惠券规则；
短期爆红的视频；
```

第二，需要维护频率元数据。每次访问都要更新计数，还要能找到频率最低的条目。朴素实现可能需要堆或多级链表。即使可以做到 O(1)，实现也比 LRU 更复杂。

第三，计数会饱和或膨胀。频率计数不能无限增长。工程实现要考虑 counter 位数、衰减、采样、近似计数。Redis 的 LFU 就不是精确 LFU，而是用概率计数器估计访问频率，并带有 decay 机制，让旧热点的计数随时间下降。

第四，低频新热点启动慢。一个新 key 如果刚开始变热，但还没积累足够频率，可能在进入缓存前就被淘汰。纯 LFU 对 bursty workload 不如 LRU 灵敏。

第五，频率不等于价值。一个小 key 被访问 100 次，一个大 key 被访问 100 次，占用内存差异可能很大。LFU 如果不考虑对象大小和 miss 成本，可能保留了“访问频繁但收益不高”的对象。

所以实际系统里的 LFU 往往不是纯 LFU，而是带 aging、window、近似计数和 admission 的变体：

```text
aging/decay:
  旧频率逐渐衰减，适应热点变化。

window LFU:
  只统计最近一段时间访问频率。

TinyLFU:
  用紧凑 sketch 估计近期频率，用作 admission policy。

W-TinyLFU:
  小窗口吸收 recency burst，大区使用 SLRU，并由 TinyLFU 控制准入。
```

Redis 的 LFU 文档也体现了这个思想：它用近似计数器和 decay time，不追求全量精确频率。原因很现实：精确频率很贵，而且旧频率不衰减会让缓存无法适应变化。

面试里可以这样答：

```text
LFU 的优势是保留长期高频热点，对访问分布稳定、热点集中、scan 干扰较多的 workload 通常比 LRU 稳。缺点是历史包袱重，旧热点如果不衰减会长期占缓存；实现也更复杂，需要维护频率元数据；新热点启动慢，短期 burst 不一定能快速进入缓存；如果不考虑对象大小和 miss 成本，频率也不等于价值。工程上常用近似 LFU、decay、window 或 TinyLFU，而不是纯精确 LFU。
```

## Q009. ARC、TinyLFU、SLRU 试图解决什么问题？

**回答：**

ARC、TinyLFU、SLRU 都是在回答同一个问题：单纯 LRU 或单纯 LFU 太粗，真实 workload 里既有最近性，也有频率，还有扫描、突发热点、热点迁移和容量限制。它们试图在低开销下更准确地判断“什么数据值得留在缓存里”。

先说 SLRU，Segmented LRU。

SLRU 把缓存分成两个区域：

```text
probation segment:
  新进入的对象先放这里，表示还在观察。

protected segment:
  被再次访问的对象晋升到这里，表示更可能是真热点。
```

一个对象第一次访问时进入 probation。如果它很快再次被访问，说明不是纯一次性扫描数据，就进入 protected。protected 满了时，会把较旧对象降回 probation 或淘汰。

SLRU 解决的是“不要让一次性访问直接污染主缓存”。它比纯 LRU 更抗 scan，因为扫描对象通常只访问一次，很难进入 protected 区。

再说 ARC，Adaptive Replacement Cache。

ARC 同时维护 recency 和 frequency，并且用 ghost list 记录最近被淘汰的 key。它大致关注两类对象：

```text
recent:
  最近访问过一次的对象。

frequent:
  最近访问过多次的对象。
```

ghost list 不保存 value，只保存被淘汰 key 的历史。它的作用是观察策略是否错了：如果一个刚从 recent 区淘汰的 key 又被访问，说明 recent 区可能太小；如果一个刚从 frequent 区淘汰的 key 又被访问，说明 frequent 区可能太小。ARC 根据这些信号自适应调整两个区域的大小。

ARC 解决的问题是：不同 workload 对 recency 和 frequency 的需求不同，而且会变化。固定比例的 SLRU 或 2Q 不一定总合适。ARC 的论文强调它会在线、自调节地平衡最近性和频率，并且对 scan workload 有抵抗能力。

TinyLFU 更像 admission policy，而不只是 eviction policy。

传统缓存通常是：miss 后把新对象放进缓存，如果满了就淘汰一个旧对象。TinyLFU 会多问一步：这个新对象真的值得进入缓存吗？

它会维护一个紧凑的近似频率结构，估计近期访问历史中的 key 频率。当新对象要进入缓存时，把它的估计频率和待淘汰候选的频率比较：

```text
如果新对象更可能带来未来命中:
  接纳新对象，淘汰候选。

如果新对象只是偶发访问:
  拒绝接纳，保留旧对象。
```

W-TinyLFU 在 TinyLFU 外面加了一个小的 LRU window，用来吸收短期突发和新热点；主缓存区域通常用 SLRU。Caffeine 文档说明它使用 Window TinyLFU，是因为它在多种 workload 上命中率高、内存开销低，并且能同时估计频率和最近性。

可以把三者这样对比：

```text
SLRU:
  用 probation/protected 分层，二次访问才进入保护区。
  重点是防止一次性访问污染缓存。

ARC:
  recent/frequent + ghost list，自适应调整 recency 和 frequency 的比例。
  重点是适应 workload 变化，减少手工调参。

TinyLFU:
  用近似频率 sketch 做 admission，决定新对象是否值得进入缓存。
  重点是把低价值 miss 拦在缓存外，减少污染。
```

它们不是银弹。代价包括：

```text
更多元数据；
实现复杂度更高；
并发维护更难；
调试更困难；
指标解释更复杂；
```

但是相比纯 LRU，它们更接近真实 workload。真实系统很少只有一种局部性：既有热点 key，又有突发 key，还有扫描和周期任务。一个通用缓存策略通常要同时处理这些情况。

面试里可以这样答：

```text
SLRU、ARC、TinyLFU 都是在修补纯 LRU/LFU 的缺点。SLRU 把缓存分成 probation 和 protected，要求对象至少被再次访问才进入保护区，减少 scan 污染。ARC 同时跟踪最近访问和频繁访问，并用 ghost list 观察被淘汰 key 是否又被访问，从而自适应调整 recency/frequency 比例。TinyLFU 更偏 admission policy，用紧凑频率 sketch 判断新对象是否值得进入缓存，避免一次性 miss 把旧热点挤掉。它们共同目标是用有限元数据提高命中质量和 workload 适应性。
```

## Q010. cache eviction 和 cache expiration 有什么区别？

**回答：**

cache eviction 和 cache expiration 都会让缓存条目消失，但触发原因不同，语义也不同。

eviction 是容量或资源压力触发的淘汰。缓存空间不够了，需要腾位置，于是根据某个策略删掉一部分条目。常见策略有：

```text
LRU:
  淘汰最久未访问。

LFU:
  淘汰访问频率最低。

Random:
  随机淘汰。

TTL-based eviction:
  在候选里优先淘汰 TTL 最短。

Size-aware:
  考虑对象大小、成本和收益。
```

Redis 的 `maxmemory-policy` 就是典型 eviction 配置。当内存超过 `maxmemory`，Redis 会按策略选择 key 淘汰。Caffeine/Guava 的 `maximumSize`、`maximumWeight` 也属于 size-based eviction。

expiration 是时间或有效期触发的过期。条目到了 TTL、idle timeout 或业务定义的有效期，就被认为不该继续作为新鲜数据使用。常见形式有：

```text
expireAfterWrite:
  写入或更新后一段时间过期。

expireAfterAccess:
  最后一次读/写后一段时间过期。

absolute expiration:
  到某个固定时间点过期。

business TTL:
  根据业务字段决定有效期，例如 token expires_at。
```

两者的关键区别：

```text
eviction:
  因为缓存资源不够，所以删。
  被删的条目未必过期，可能仍然是新鲜的。
  重点是容量管理。

expiration:
  因为时间或业务有效期到了，所以失效。
  过期条目即使缓存还有空间，也不应该当新鲜值返回。
  重点是 freshness 管理。
```

举个例子，一个商品详情缓存 TTL 是 10 分钟，Redis 内存满了，第 1 分钟就把它淘汰了。这是 eviction，不是 expiration。它的数据可能仍然新鲜，只是缓存没地方放。

反过来，缓存里还有很多空间，但这个商品详情已经超过 10 分钟 TTL。它是 expired。此时继续返回它可能不符合新鲜度要求。

这两个概念在指标上也要分开看：

```text
evicted_keys 高:
  可能说明内存不足、对象太大、策略不合适、热点留不住。

expired_keys 高:
  可能说明 TTL 太短、写入量大、业务自然过期多。

hit stale 高:
  可能说明 expiration 检查不严格，或允许 stale-while-revalidate。

miss after eviction:
  更偏容量和策略问题。

miss after expiration:
  更偏 freshness 和 TTL 设计问题。
```

工程上还要注意：过期不一定立刻清理。Guava 文档明确提到缓存不会在条目过期的瞬间立刻后台清理所有值，而是在写操作和偶尔读操作中做维护，也可以手动调用 cleanup。很多缓存系统都有类似惰性清理或后台清理机制。也就是说：

```text
expired:
  逻辑上不应再作为新鲜值使用。

physically removed:
  物理上从缓存结构里删除。
```

这两个时刻可能不同。

面试里还可以补一句 refresh。refresh 和 expiration 也不同。Guava 文档里提到 refresh 会重新加载新值，但旧值在刷新期间仍可返回；expiration 通常会让下一次读等待重新加载或直接 miss。生产系统里常用 `refreshAfterWrite + expireAfterWrite` 组合：先允许后台刷新，刷新长时间没发生时再彻底过期。

面试里可以这样答：

```text
eviction 是容量压力触发的淘汰，缓存满了或超过权重限制时按 LRU/LFU/TinyLFU 等策略删条目；被淘汰的数据不一定旧，只是缓存放不下。expiration 是时间或业务有效期触发的失效，TTL 到了即使缓存还有空间也不应该当新鲜值返回。指标上 evicted 多通常看内存和淘汰策略，expired 多通常看 TTL 和新鲜度设计。还要区分逻辑过期和物理删除，很多缓存会惰性清理过期项。
```

## Q011. TTL 过短和过长分别有什么问题？

**回答：**

TTL 本质上是在“数据新鲜度”和“后端压力”之间做取舍。TTL 不是越短越安全，也不是越长越省资源。它决定的是：一个缓存值在多长时间内可以被当作足够新鲜的近似答案使用。

TTL 过短，最直接的问题是命中率下降。数据还没真正变旧，缓存就被判定过期，下一次请求只能重新访问数据库、RPC 服务或对象存储。这样会带来几类连锁反应：

```text
命中率下降:
  cache miss 变多，缓存层的价值下降。

后端 QPS 上升:
  数据库、搜索引擎、远程服务承受更多读流量。

尾延迟变差:
  原本可以在内存里返回的请求，变成跨网络、跨进程或磁盘访问。

更容易出现 stampede:
  热点 key 到期后，大量并发请求同时穿透到后端。

缓存维护成本上升:
  频繁写入、过期、删除，带来额外 CPU、内存分配和复制开销。
```

比如商品详情 TTL 设置为 1 秒，而商品实际几分钟才改一次。高峰期每秒都有大量请求把同一个 key 打穿到数据库，缓存看起来存在，实际效果接近没有缓存。AWS 的缓存有效性文档也强调，TTL 需要根据数据变化频率和返回陈旧数据的风险来选；频繁变化的数据可以用短 TTL，但这不是无条件越短越好。

TTL 过长的问题正好相反：缓存命中率看起来很好，但返回的是旧数据。这个风险在业务上往往比 miss 更隐蔽，因为监控里 hit rate 很漂亮，用户拿到的答案却可能已经错了。

典型问题包括：

```text
业务状态陈旧:
  订单状态、库存、余额、权限、价格已经变了，缓存还返回旧值。

错误被放大:
  一次错误写入如果带着长 TTL，会在很长时间内持续影响用户。

修复延迟:
  数据库修正后，缓存没有失效，用户侧仍然看不到修复结果。

负缓存误伤:
  用户刚创建、商品刚上架，旧的 not found 缓存仍然生效。

内存占用更久:
  冷 key 长时间留在缓存里，挤占真正有价值的热数据。
```

所以 TTL 的设计不能只看“能不能提高 hit rate”。更合理的做法是先问三个问题：

```text
这个数据多久变化一次？
  变化越频繁，TTL 通常越短，或者需要主动失效。

用户能接受多旧的数据？
  推荐列表可以旧几十秒，余额和权限可能不能旧。

miss 成本有多高？
  miss 成本越高，越需要 singleflight、预热、异步刷新或 stale-while-revalidate。
```

工程上常用几种补偿手段：

```text
TTL jitter:
  给 TTL 加随机抖动，避免大量 key 在同一时刻过期。

主动失效:
  写数据库后删除缓存，或者通过 CDC/binlog 事件失效缓存。

逻辑过期:
  缓存值里带 expire_at。读请求可以先返回旧值，再触发后台刷新。

分级 TTL:
  热点数据、普通数据、负缓存、异常结果使用不同 TTL。

版本号:
  缓存值携带 version，避免旧加载结果覆盖新值。
```

Redis 的 `EXPIRE` 文档还提醒了一个容易忽略的点：TTL 通常是基于绝对时间戳保存的，所以机器时间跳变会影响过期行为。分布式缓存里，如果节点时钟被大幅调整，可能出现 key 提前过期或延迟过期。这类边界在面试里不一定要展开，但做生产系统时要知道它不是纯逻辑计数器。

面试里可以这样答：

```text
TTL 过短会让缓存频繁过期，命中率下降，后端 QPS 和尾延迟上升，热点 key 还容易触发 cache stampede。TTL 过长会让 hit rate 看起来很好，但返回旧数据，尤其会影响库存、价格、权限、余额和负缓存这类敏感场景。TTL 应该按数据变化频率、业务可接受陈旧时间和 miss 成本来定，常见做法是加随机抖动、写后主动失效、热点 key 后台刷新、负缓存短 TTL，并用版本号防止旧值覆盖新值。
```

## Q012. cache stampede 是什么？

**回答：**

cache stampede 指的是某个缓存 key 失效、被淘汰或尚未加载时，大量并发请求同时发现 miss，于是一起访问后端数据源。缓存本来是为了保护后端，结果在同一时间点失效后，反而把流量集中打到数据库或远程服务上。

一个典型过程是：

```text
1. 热点 key 在缓存中存在，所有请求都命中。
2. 这个 key 到期、被淘汰、被删除，或者缓存集群重启后丢失。
3. 同一时间有很多请求读取这个 key。
4. 每个请求都发现 miss。
5. 如果没有并发合并，所有请求都会去查数据库。
6. 数据库变慢后，请求堆积、重试增加，进一步放大压力。
```

stampede 的关键不是“缓存 miss”，而是“同一个 miss 被大量并发重复计算”。如果只有一个请求 miss，它只是普通缓存未命中；如果一万个请求同时 miss 同一个热点 key，它就可能变成后端事故。

常见触发条件有：

```text
热点 key 到期:
  比如首页配置、热门商品、活动库存、排行榜。

批量过期:
  部署时统一预热，所有 key 使用相同 TTL，之后一起过期。

冷启动:
  缓存刚启动或 namespace 刚切换，所有 key 都是空的。

缓存淘汰:
  内存不足时热点 key 被挤出，下一波请求同时回源。

后端变慢:
  单次加载时间越长，并发请求堆积窗口越大。

重试放大:
  调用方超时重试，使同一个 miss 被更多请求重复触发。
```

它和几个相近概念的边界也要分清：

```text
cache stampede:
  多个请求同时回源加载同一个或一批缓存项，重点是并发重复加载。

cache breakdown:
  通常特指热点 key 过期后，大量请求打到数据库。

cache avalanche:
  大量 key 同时失效，或整个缓存层不可用，导致大面积流量回源。

cache penetration:
  请求的 key 根本不存在，缓存和数据库都没有，导致每次都穿透。
```

stampede 的后果不只是数据库 QPS 变高。更严重的是它会让系统进入正反馈：

```text
回源变慢 -> 请求堆积 -> 连接池耗尽 -> 超时重试 -> 更多回源 -> 后端更慢
```

所以治理 stampede 的核心是“同一个 key 同一时刻只让少量请求回源”，其他请求等待、返回旧值、降级，或者被限流。

常见手段包括：

```text
singleflight / request coalescing:
  同一进程内同一个 key 只执行一次加载。

分布式锁:
  多个实例之间同一个 key 只允许一个实例回源。

stale-while-revalidate:
  允许短时间返回旧值，同时后台刷新。

TTL jitter:
  避免大量 key 在同一时刻到期。

预热:
  活动、发布、冷启动前提前加载热点 key。

逻辑过期:
  物理缓存不过期或较晚过期，由业务字段判断是否需要刷新。

限流和熔断:
  保护数据库和下游服务，避免重试风暴。
```

面试里可以这样答：

```text
cache stampede 是缓存失效或冷启动时，大量并发请求同时 miss，并且重复回源加载同一个热点数据，导致数据库或下游服务被瞬间打爆。它的重点是并发重复加载，而不只是普通 miss。常见触发点是热点 key 到期、批量过期、缓存重启、后端变慢和重试放大。治理思路是合并同 key 的回源请求，比如 singleflight、分布式锁、stale-while-revalidate、TTL 抖动、预热、限流和熔断。
```

## Q013. singleflight 如何缓解 cache stampede？

**回答：**

singleflight 的思路很朴素：对同一个 key，在同一时间只允许一个执行者真正去加载数据，其他并发请求不要重复做同样的工作，而是等待第一个执行者的结果。

Go 的 `golang.org/x/sync/singleflight` 官方文档把它描述为 duplicate function call suppression，也就是“重复函数调用抑制”。`Group.Do` 的语义是：同一个 key 上如果已经有一个调用正在执行，后来的调用会等待前一个调用完成，并收到同一份结果。返回值里的 `shared` 可以告诉调用方这个结果是否被多个调用共享。

放到缓存读取链路里，伪代码一般是：

```text
read(key):
  value = cache.get(key)
  if value exists:
    return value

  value = singleflight.do(key, function:
    value2 = cache.get(key)
    if value2 exists:
      return value2

    fresh = db.load(key)
    cache.set(key, fresh, ttl)
    return fresh
  )

  return value
```

这里有两个细节很重要。

第一，进入 singleflight 后最好再读一次缓存。因为当前请求在等待之前，可能已经有别的请求把缓存填好了。如果不做二次检查，就可能多一次不必要的回源。

第二，singleflight 不是缓存。它只合并并发执行，不保存长期结果。第一次请求结束后，下一次请求如果又 miss，仍然会再执行加载函数。

它缓解 stampede 的方式可以用一个例子说明：

```text
没有 singleflight:
  1000 个请求同时读 product:123。
  缓存 miss。
  1000 个请求同时查数据库。

有 singleflight:
  1000 个请求同时读 product:123。
  缓存 miss。
  第 1 个请求查数据库。
  其余 999 个请求等待。
  第 1 个请求查完并写缓存。
  999 个请求共享结果返回。
```

这能把“同进程、同 key、同时间窗口”的重复回源从 N 次降到 1 次。对热点 key 过期特别有效。

但 singleflight 的边界也必须说清楚：

```text
它通常是进程内的:
  一个 Go 进程内可以合并请求，但多个服务实例之间不会自动合并。
  如果有 100 个实例，每个实例仍可能各自回源一次。

它不解决错误本身:
  如果加载函数失败，等待者通常会一起拿到同一个错误。
  错误是否缓存、是否重试，需要业务自己决定。

它可能放大等待:
  如果加载函数卡住，所有等待同 key 的请求都会被拖住。
  所以必须配 context timeout。

key 粒度要正确:
  key 太粗会把无关请求串行化，key 太细又合并不了重复加载。

结果不能盲目共享:
  如果结果和用户身份、租户、权限有关，singleflight key 必须包含这些维度。
```

在分布式环境里，singleflight 常和其他手段组合：

```text
单机 singleflight:
  降低每个实例内部重复回源。

分布式锁:
  降低跨实例重复回源，但要处理锁超时、续租和误释放。

stale value:
  加载失败或超时时，允许短时间返回旧值。

TTL jitter:
  从源头降低同时过期概率。

限流:
  防止等待队列和后端被拖垮。
```

另外，Go singleflight 提供 `Forget`，可以让后续同 key 调用不再等待当前 in-flight 调用，而是重新执行。这在某些“当前加载已经明显超时或不可信”的场景有用，但不能乱用，否则会重新制造 stampede。

面试里可以这样答：

```text
singleflight 通过按 key 合并并发加载来缓解 cache stampede。缓存 miss 后，第一个请求真正回源，后续同 key 请求等待并共享它的结果，这样同进程内同一时间窗口的 N 次数据库查询可以降到 1 次。但它不是缓存，也不是分布式锁，通常只能解决单进程内重复回源。实际使用时要做二次缓存检查、设置 context timeout，key 要包含租户和权限维度，并且通常还要配合 TTL 抖动、分布式锁、旧值降级和限流。
```

## Q014. cache penetration、cache breakdown、cache avalanche 分别是什么？

**回答：**

这三个词都描述“缓存没有挡住流量”，但原因和治理手段不一样。面试时最容易混在一起，需要按“请求对象是否存在、影响范围是单个热点还是大量 key、触发原因是什么”来区分。

cache penetration 是缓存穿透。请求查询的对象本来就不存在，缓存里没有，数据库里也没有。因为没有结果可缓存，下一次同样请求又会打到数据库。

例子：

```text
用户请求 user:-1。
缓存没有。
数据库也没有。
接口返回 not found。
下一次又请求 user:-1。
缓存仍然没有。
数据库又被查一次。
```

如果攻击者构造大量随机不存在的 ID，就会绕开缓存，把压力直接打到数据库。常见治理方式是：

```text
负缓存:
  把 not found 结果短时间缓存起来。

Bloom filter:
  在访问缓存和数据库前先判断 key 是否可能存在。

参数校验:
  明显非法的 ID、租户、枚举值直接拒绝。

限流:
  对异常 miss 模式、随机 key、单用户高 miss 率做限制。
```

cache breakdown 常被翻译成缓存击穿，通常指一个热点 key 失效后，大量请求同时打到后端。对象是存在的，平时也能缓存住，只是在某个时刻失效了。

例子：

```text
product:hot-123 是活动页热点商品。
平时所有请求都命中缓存。
TTL 到期的一瞬间，大量请求同时 miss。
大家一起查数据库。
数据库被单个热点 key 打穿。
```

治理方式偏向热点保护：

```text
singleflight:
  同一实例内合并热点 key 回源。

分布式互斥:
  多实例之间限制同 key 同时回源。

热点 key 逻辑过期:
  物理缓存保留，过期后后台刷新。

预热:
  活动开始前加载热点 key。

不过期或长 TTL + 主动更新:
  对极热点配置和首页数据常见，但要处理一致性。
```

cache avalanche 是缓存雪崩。它不是一个 key 的问题，而是大量 key 同时失效，或者缓存层整体不可用，导致大面积流量回源。

例子：

```text
所有商品详情都设置 30 分钟 TTL。
发布时同时预热。
30 分钟后大量 key 一起过期。
数据库 QPS 突然暴涨。
```

或者：

```text
Redis 集群故障。
应用读缓存全部失败。
所有读请求直接打数据库。
```

治理方式偏向系统级保护：

```text
TTL jitter:
  不让大量 key 同时过期。

分批预热:
  冷启动时按批次加载，不一次性制造同周期 TTL。

多级缓存:
  本地缓存 + 分布式缓存，降低单层失败影响。

熔断降级:
  缓存不可用时保护数据库，必要时返回降级数据。

容量规划:
  数据库要能承受一定比例的 miss，而不是完全依赖缓存。

故障演练:
  验证缓存集群不可用时系统如何退化。
```

三者可以这样对比：

```text
penetration:
  key 不存在，缓存和数据库都没有，每次请求都穿透。

breakdown:
  key 存在，而且通常是热点；热点 key 过期后瞬间打穿后端。

avalanche:
  大量 key 或整个缓存层同时失效，影响范围更大。
```

面试里可以这样答：

```text
cache penetration 是查不存在的数据，缓存没有、数据库也没有，导致请求反复穿透，通常用负缓存、Bloom filter、参数校验和限流处理。cache breakdown 是热点 key 过期或被淘汰后，大量请求同时回源，通常用 singleflight、分布式锁、逻辑过期、预热和热点 key 主动更新处理。cache avalanche 是大量 key 同时过期或缓存层故障，导致大面积流量打到后端，通常用 TTL 抖动、分批预热、多级缓存、熔断降级和容量规划处理。
```

## Q015. write-through、write-back、write-around 的区别是什么？

**回答：**

这三个模式讨论的是“写请求发生时，数据库和缓存分别怎么更新”。它们不只是缓存策略，也是写路径的一致性、延迟和可靠性取舍。

write-through 是写穿透。应用写数据时，同步写入后端存储，并同步更新缓存。一次写请求只有在数据库和缓存都处理完后才算完成。

```text
write-through:
  client -> service
  service -> database write
  service -> cache write
  service -> return success
```

优点是读路径简单。写完后，缓存里通常就是新值，后续读可以直接命中。它适合读多写少、写后马上会读、对读新鲜度要求比较高的场景。

缺点也明显：

```text
写延迟更高:
  一次写要等数据库和缓存两个系统。

部分失败难处理:
  数据库成功但缓存失败，或者缓存成功但数据库失败，都要补偿。

缓存污染:
  很多写入的数据可能之后根本不会被读，但仍然进入缓存。

写放大:
  所有写都多了一次缓存写。
```

AWS 的 Redis 缓存策略文档也提到，write-through 会在数据库更新后立即更新缓存，但它可能把不会再访问的数据也写入缓存，从而增加缓存成本。

write-back 也叫 write-behind。它的核心是先写缓存，缓存再异步把变更刷到数据库。

```text
write-back:
  client -> service
  service -> cache write
  service -> return success
  async worker -> database write
```

它的优点是写延迟低，可以把多次写合并、批量落库，对高频更新场景很有吸引力，比如计数器、日志缓冲、某些可重放事件。

但它的风险最大：

```text
持久性风险:
  缓存还没刷到数据库就崩溃，数据可能丢失。

顺序风险:
  异步刷盘乱序，会把旧值写到新值后面。

重放和幂等:
  worker 重试时必须保证不会重复计数或覆盖新值。

恢复复杂:
  缓存、队列、数据库之间要有明确的 WAL 或事件日志。
```

严格来说，如果缓存只是 Redis 这类内存系统，而没有可靠日志，那么把它当成 write-back 的唯一承载点通常很危险。生产上更常见的是“写入可靠队列/日志，异步更新数据库和缓存”，而不是单纯依赖缓存本身承担持久化职责。

write-around 是绕写缓存。写请求只写数据库，不立即更新缓存。缓存等下一次读 miss 时再加载。

```text
write-around:
  client -> service
  service -> database write
  service -> cache delete or no cache write
  service -> return success

next read:
  cache miss -> database read -> cache set
```

它的优点是避免缓存污染。很多数据写完后不会马上被读，没必要把所有写都塞进缓存。它适合写多读少、写入数据冷热差异大、缓存容量比较宝贵的场景。

缺点是写后第一次读会 miss。如果旧缓存没有被删除，还可能读到旧值。因此 write-around 通常要配合缓存失效：

```text
更新数据库后删除缓存:
  避免旧值继续被读。

读 miss 后再加载:
  只有真正被读的数据才进入缓存。
```

三者放在一起看：

```text
write-through:
  同步写数据库和缓存。
  读新鲜度较好，写延迟和双写复杂度较高。

write-back:
  先写缓存，异步写数据库。
  写延迟低，但持久性、顺序和恢复复杂度高。

write-around:
  写数据库，不主动写缓存，读 miss 时再加载。
  避免缓存污染，但写后第一次读可能 miss，需要删除旧缓存。
```

面试里可以这样答：

```text
write-through 是写请求同步更新数据库和缓存，读新鲜度好，但写延迟更高，且有双写失败问题。write-back 是先写缓存再异步落库，写延迟低、可批量合并，但要承担数据丢失、乱序刷盘、重试幂等和恢复复杂度，通常需要可靠日志。write-around 是写数据库时不写缓存，只删除旧缓存或等读 miss 再加载，能减少缓存污染，但写后第一次读可能 miss，如果旧缓存没删掉还会读到旧值。
```

## Q016. cache-aside 模式有什么一致性风险？

**回答：**

cache-aside 是最常见的缓存模式之一。读路径通常是：

```text
1. 先读缓存。
2. 缓存命中就返回。
3. 缓存 miss 就读数据库。
4. 把数据库结果写入缓存。
5. 返回结果。
```

写路径常见做法是：

```text
1. 更新数据库。
2. 删除缓存。
```

Microsoft Azure 的 Cache-Aside pattern 文档也明确说，应用负责在缓存和数据源之间搬运数据；当数据被修改时，可以修改数据源并使缓存失效。它同时提醒，缓存数据不能总是保证和数据源一致，尤其是在多个应用实例都有本地缓存，或者数据源被外部进程修改时。

cache-aside 的一致性风险主要来自“缓存和数据库不是一个原子系统”。

第一类风险是写数据库和删缓存之间的部分失败：

```text
1. 服务更新数据库成功。
2. 服务删除缓存失败，或者删除请求超时。
3. 缓存里继续保留旧值。
4. 后续读请求命中旧缓存。
```

这时数据库已经是新值，但缓存还在返回旧值。

第二类风险是读写并发导致旧值回填。一个典型时序是：

```text
1. 请求 A 读缓存 miss。
2. 请求 A 读数据库，拿到旧值 v1。
3. 请求 B 更新数据库为 v2。
4. 请求 B 删除缓存。
5. 请求 A 把刚才读到的 v1 写入缓存。
6. 后续请求从缓存读到旧值 v1。
```

这就是“旧读结果覆盖新状态”。简单的“更新数据库后删缓存”能降低风险，但不能完全消除并发窗口。

第三类风险是外部写入绕过应用缓存逻辑：

```text
后台任务直接改数据库。
运维脚本修数据。
另一个服务写数据库但没有发缓存失效事件。
CDC 延迟或消息丢失。
```

这些都会让缓存不知道数据库已经变了。

第四类风险来自读副本和复制延迟：

```text
1. 主库已经写入 v2。
2. 缓存被删除。
3. 读请求 miss 后去读从库。
4. 从库还没追上，只返回 v1。
5. 请求把 v1 写回缓存。
```

这类问题在线上很常见，因为很多系统为了读性能会让 cache loader 读只读副本。

第五类风险是本地缓存不一致。Azure 文档里提到，如果每个应用实例都有自己的本地缓存，一个实例更新数据后，其他实例的本地缓存未必立刻失效。多级缓存里尤其要注意：

```text
本地内存缓存:
  延迟最低，但跨实例一致性最弱。

分布式缓存:
  一致性比本地缓存好，但仍然不是数据库事务的一部分。

数据库:
  通常是事实源，但读副本可能有延迟。
```

常见缓解手段包括：

```text
短 TTL:
  限制旧值最长存活时间，但不能完全避免旧读。

更新数据库后删除缓存:
  比直接更新缓存更常见，因为删除是让下一次读重新加载。

版本号 / CAS:
  缓存写入时比较 version，旧版本不能覆盖新版本。

CDC / binlog invalidation:
  数据库变更后统一发失效事件。

outbox:
  把业务写和失效事件放在同一个数据库事务里，再异步投递。

double delete:
  写库前后或写库后延迟再删一次，用来缩小并发旧值回填窗口，但不是强一致保证。

按业务选择一致性等级:
  权限、余额、库存不要只依赖弱一致缓存。
```

面试里可以这样答：

```text
cache-aside 的风险来自缓存和数据库不是原子更新。读 miss 后回填缓存、写数据库后删除缓存，这两个动作之间会出现失败和并发窗口。典型问题包括数据库更新成功但删缓存失败、读请求先拿到旧值后又把旧值写回缓存、外部进程绕过缓存直接改库、读副本延迟导致旧值被回填、本地缓存跨实例不一致。缓解手段是短 TTL、写后删缓存、CDC 或 outbox 失效事件、版本号 CAS、防止旧版本覆盖新版本，并对强一致业务减少对缓存的依赖。
```

## Q017. 双写数据库和缓存为什么容易不一致？

**回答：**

双写数据库和缓存容易不一致，根本原因是它们是两个独立系统，通常没有一个共同的原子事务。数据库提交成功，不代表缓存更新成功；缓存更新成功，也不代表数据库提交成功。即使两边都成功，也可能因为并发和重试导致顺序错乱。

最简单的部分失败场景：

```text
场景一:
  1. 写数据库成功。
  2. 写缓存失败。
  3. 数据库是新值，缓存是旧值或空值。

场景二:
  1. 写缓存成功。
  2. 写数据库失败。
  3. 缓存里出现数据库并不存在的新值。
```

如果业务返回成功的判断不严谨，用户可能马上读到一个数据库里没有正式提交的状态。

更麻烦的是并发乱序。比如两个请求同时更新同一个用户资料：

```text
初始值:
  database = v1
  cache = v1

请求 A:
  想写 v2。

请求 B:
  想写 v3。

时序:
  A 写数据库 v2 成功。
  B 写数据库 v3 成功。
  B 写缓存 v3 成功。
  A 因为网络抖动，晚一点写缓存 v2 成功。

结果:
  database = v3
  cache = v2
```

数据库里是新值 v3，缓存却被旧请求覆盖成 v2。这个问题不是“先写缓存还是先写数据库”就能彻底解决的，因为跨系统操作没有全局顺序保证。

重试也会制造旧写覆盖新写：

```text
1. 请求 A 写 v2，缓存写超时。
2. 调用方不知道是否成功，于是重试。
3. 请求 B 写 v3 并成功。
4. 请求 A 的重试晚到，把缓存写回 v2。
```

消息驱动的缓存失效也有类似问题：

```text
1. 数据库连续发生 version=10 和 version=11 两次更新。
2. 两条缓存更新消息进入队列。
3. 消息消费顺序变成 version=11 先到，version=10 后到。
4. 如果没有版本检查，旧消息会覆盖新缓存。
```

还有一种常见误区是“更新数据库后更新缓存”看起来比“更新数据库后删除缓存”更直接。但更新缓存需要构造完整新值，可能依赖复杂 join、权限过滤、派生字段和读模型。如果这个构造过程和读路径不完全一致，缓存值也会偏离真实查询结果。因此很多系统更倾向于：

```text
写路径:
  更新数据库，然后删除缓存。

读路径:
  下次 miss 时按统一读逻辑从数据库加载并回填。
```

但“写数据库后删缓存”也不是强一致。删除失败、读写并发、从库延迟都可能让旧值回到缓存。所以它只是工程上常用的弱一致方案，不是分布式事务。

要降低双写不一致，通常会组合这些手段：

```text
以数据库为事实源:
  缓存只是派生状态，必要时可以重建。

更新数据库后删除缓存:
  避免直接写入复杂缓存值。

版本号:
  缓存写入和消息消费时拒绝旧版本覆盖新版本。

outbox:
  在数据库事务内写业务数据和待发送事件，再由 worker 投递缓存失效。

CDC:
  从数据库变更日志生成缓存失效或更新事件。

幂等重试:
  所有补偿动作可以重复执行，不产生错误副作用。

监控和修复:
  对关键数据抽样比对缓存和数据库，发现漂移后重建。
```

面试里可以这样答：

```text
数据库和缓存是两个独立系统，双写没有天然原子性，所以容易出现部分失败、并发乱序、重试晚到和消息乱序。数据库成功但缓存失败会留下旧缓存；缓存成功但数据库失败会产生脏缓存；两个并发写都成功时，旧请求可能最后写缓存，把新值覆盖掉。更新数据库后删除缓存能降低复杂缓存值写错的概率，但仍然不能保证强一致。生产上通常以数据库为事实源，配合写后删缓存、outbox 或 CDC、版本号 CAS、幂等补偿和一致性校验。
```

## Q018. 如何使用版本号避免缓存旧值覆盖新值？

**回答：**

版本号的核心作用是给每个缓存值一个可比较的新旧顺序。只要缓存写入时能判断“这个值是不是比当前缓存更新”，旧加载结果、旧消息、旧重试就不能覆盖新值。

最常见设计是在数据库行里维护一个单调递增字段：

```text
id: 123
value: ...
version: 42
updated_at: ...
```

缓存里不要只存业务值，而要连同版本一起存：

```json
{
  "value": {
    "name": "Alice",
    "level": "vip"
  },
  "version": 42
}
```

当写请求更新数据库时，数据库负责产生新版本：

```text
UPDATE user
SET name = ?, version = version + 1
WHERE id = ?;
```

然后缓存更新、缓存回填、消息消费都遵守同一个规则：

```text
只有 incoming.version >= cached.version 时，才允许写入缓存。
如果 incoming.version < cached.version，说明这是旧结果，必须丢弃。
```

这样可以处理前面提到的旧值回填问题：

```text
1. 请求 A 读数据库，拿到 v1(version=10)。
2. 请求 B 更新数据库，写入 v2(version=11)，并写入缓存。
3. 请求 A 晚一点尝试把 v1 写入缓存。
4. 缓存发现 incoming version=10 < current version=11。
5. 拒绝写入，旧值不会覆盖新值。
```

这里最关键的是“比较和写入必须原子”。如果在 Redis 里先 `GET`，应用本地比较，再 `SET`，中间仍然可能被并发写打断。更可靠的方式是用 Lua 脚本在 Redis 单线程执行里完成：

```text
1. 读取当前缓存版本。
2. 比较 incoming version 和 current version。
3. 如果 incoming 更新，则写入新值和 TTL。
4. 否则保持原值。
```

伪代码：

```text
cache_set_if_newer(key, value, version, ttl):
  current = redis.get(key)
  if current does not exist:
    redis.set(key, {value, version}, ttl)
    return written

  if version >= current.version:
    redis.set(key, {value, version}, ttl)
    return written

  return ignored
```

版本号也可以用于缓存失效消息。比如数据库更新后发送：

```json
{
  "key": "user:123",
  "version": 43,
  "event": "user_updated"
}
```

消费者处理时，如果发现缓存里已经是 version=44，就不能再根据 version=43 的旧消息做覆盖或删除。是否要删除旧版本，需要看协议设计：

```text
update message:
  只有消息版本更新时才写缓存。

invalidate message:
  可以删除小于等于该版本的缓存，不能删除更新版本。

tombstone:
  删除对象时写入带版本的墓碑，防止旧读结果把已删除对象复活。
```

版本来源也要谨慎。最好由事实源产生，例如数据库自增版本、事务序号、binlog position、全局递增序列，或者每个实体自己的 fencing token。单纯用应用服务器本地时间当版本有风险：

```text
机器时钟可能漂移。
同一毫秒内可能多次更新。
不同实例的时间不一定严格单调。
NTP 调整可能造成时间回拨。
```

如果只能用 `updated_at`，至少要保证数据库侧生成，并明确同一时间戳冲突时的处理规则。

版本号不是强一致魔法。它解决的是“旧值不能覆盖新值”，不保证读请求永远读到最新数据库状态。缓存里可能暂时没有值，也可能旧值还没被删除。但版本号能显著降低双写、异步消息、回源回填、重试乱序带来的破坏。

面试里可以这样答：

```text
做法是让数据库为每个实体产生单调递增 version，缓存值也带上 version。任何缓存写入、回填或消息更新都必须比较版本，只有 incoming version 不小于当前缓存版本时才允许写入；旧版本结果直接丢弃。比较和写入要在缓存侧原子完成，比如 Redis 用 Lua 脚本，否则 GET 后再 SET 仍有竞态。删除也最好使用带版本的 tombstone 或 invalidate 消息，避免旧读结果把已删除或已更新的数据重新写回缓存。版本号不能让缓存强一致，但能防止旧值覆盖新值。
```

## Q019. 负缓存 negative cache 适合什么场景？

**回答：**

负缓存是把“没有结果”也缓存起来。它缓存的不是正常业务对象，而是某种否定结论，例如：

```text
这个用户不存在。
这个商品不存在。
这个 ID 非法或已删除。
这个 DNS 名称不存在。
这个用户对资源没有权限。
这个列表查询结果为空。
```

RFC 2308 讨论 DNS negative caching 时给了一个很经典的定义：负缓存保存的是“某个东西不存在”的知识。它的价值是减少响应时间和网络流量。这个思想放到业务系统里也成立：如果一个 miss 的结论在短时间内稳定，就没有必要每次都查数据库。

负缓存最适合几类场景。

第一类是防 cache penetration。大量请求查询不存在的 key，如果不缓存 not found，每个请求都会打到数据库。比如：

```text
GET /users/999999999
GET /products/not-exist
GET /orders/random-id
```

如果这些 ID 大概率不存在，就可以缓存一个短 TTL 的 not found 标记：

```json
{
  "exists": false,
  "reason": "not_found",
  "version": 0
}
```

第二类是不存在结果相对稳定。比如历史订单 ID、已经删除的老资源、DNS NXDOMAIN、配置项不存在。这类结果短时间内变化概率低，缓存起来收益明显。

第三类是空集合查询。比如某个用户当前没有通知、某个标签下暂时没有商品、某个租户还没有配置。如果不缓存空结果，列表查询会频繁访问数据库。

第四类是权限或关系判断，但要非常谨慎。比如“用户 A 不是项目 P 的成员”可以短时间缓存，用来减少权限表查询。但 key 必须包含完整安全维度：

```text
tenant_id
user_id
resource_id
permission
auth_context_version
```

否则可能把一个用户的无权限结论错误复用给另一个用户。

负缓存的 TTL 通常应该比正缓存短。原因很简单：不存在的东西可能马上被创建，空列表可能马上变成非空，权限可能刚刚被授予。常见设计是：

```text
正缓存:
  几分钟到几十分钟，取决于业务。

负缓存 not found:
  几秒到几十秒，通常较短。

负缓存非法参数:
  可以更长，因为非法格式不会变合法。

下游 5xx:
  一般不作为普通负缓存；最多做很短暂的错误抑制或熔断状态。
```

还要区分“真实不存在”和“暂时查不到”：

```text
404 / not found:
  可以短 TTL 负缓存。

403 / no permission:
  可以按用户和权限维度短 TTL 缓存。

empty list:
  可以短 TTL 缓存，但创建新元素时要失效。

timeout / 5xx:
  不应随便缓存成不存在，否则会把临时故障伪装成业务不存在。
```

负缓存常和 Bloom filter 一起使用。Bloom filter 可以在更前面挡掉明显不存在的 key，负缓存则处理 Bloom filter 误判、业务删除、短期重复 miss 等情况。

面试里可以这样答：

```text
负缓存适合缓存短时间内稳定的否定结果，比如对象不存在、列表为空、DNS NXDOMAIN、已删除资源、非法 ID 或某些按用户和租户维度隔离的无权限判断。它主要用来降低 cache penetration，让不存在的 key 不要每次都查数据库。设计时要把负缓存和正常值区分开，保存 reason、version 或 tombstone，TTL 通常比正缓存短；404 可以短 TTL 缓存，5xx 和 timeout 不能随便缓存成不存在。
```

## Q020. 负缓存会带来什么风险？

**回答：**

负缓存的最大风险是把“曾经不存在”变成“现在仍然不存在”。它提高了 miss 场景的性能，但如果 TTL、key 维度和失效机制设计不好，会直接影响正确性。

第一类风险是创建后仍然读到 not found：

```text
1. 用户查询 product:123。
2. 当时数据库没有，系统写入负缓存 not found，TTL 5 分钟。
3. 1 分钟后商品 product:123 被创建。
4. 后续请求仍然命中负缓存，返回 not found。
5. 商品明明已经存在，用户却看不到。
```

所以对象创建、恢复、权限授予、列表新增时，最好主动删除相关负缓存。否则只能等 TTL 自然过期。

第二类风险是把临时故障误缓存成业务不存在：

```text
数据库超时。
下游服务 503。
RPC 返回 unknown。
```

这些不是“对象不存在”，只是系统暂时没能确认对象存在。如果把它们写成负缓存，故障恢复后用户仍然会被错误挡住。更合理的处理是返回错误、短暂熔断、使用旧值，或者缓存一个很短 TTL 的错误状态，但不要和 not found 混在一起。

第三类风险是缓存污染和内存攻击。攻击者可以构造大量随机不存在的 key：

```text
user:random-1
user:random-2
user:random-3
...
```

如果每个都写入负缓存，缓存容量会被这些无价值的负条目占满，挤掉真正的热数据。治理方式包括：

```text
限制负缓存最大数量。
对异常 miss 来源限流。
对 key 做合法性校验。
使用 Bloom filter 先挡一层。
负缓存使用更短 TTL。
```

第四类风险是权限和租户维度错误。比如把这个结果缓存成：

```text
project:123 -> no_permission
```

这就是错误的，因为权限是和用户、租户、角色、组织关系有关的。正确 key 至少要包含：

```text
tenant_id:user_id:project_id:permission
```

否则 A 用户无权限的结论可能影响 B 用户，或者一个租户的不存在结论影响另一个租户。

第五类风险是删除和重建的 ABA 问题：

```text
1. resource:123 存在，version=10。
2. resource:123 被删除，写入 tombstone version=11。
3. resource:123 被重新创建，version=12。
4. 一个旧的 not found 或旧 tombstone 晚到，覆盖 version=12。
```

这类问题要靠版本号解决。负缓存不是简单写一个 `null`，最好也带版本或 tombstone 信息，让旧的否定结论不能覆盖更新的正值。

第六类风险是掩盖数据同步延迟。比如写入主库后，读从库还没同步到，于是 loader 认为对象不存在并写入负缓存。之后从库同步完成，但负缓存还在。这类问题在读写分离系统里很常见。

治理负缓存通常要注意：

```text
短 TTL:
  控制错误否定结论的最长影响时间。

主动失效:
  create、restore、grant permission、insert list item 时删除负缓存。

区分原因:
  not_found、no_permission、empty、invalid、backend_error 不要混为一谈。

版本号:
  正值、负值、tombstone 都带 version，旧版本不能覆盖新版本。

容量控制:
  限制负缓存占比，避免随机 key 污染。

观测指标:
  监控 negative hit rate、not_found rate、random key miss、负缓存大小。
```

面试里可以这样答：

```text
负缓存的风险是把过去的不存在延续到现在。对象刚创建、权限刚授予、列表刚新增时，旧的 not found 或 empty 缓存可能继续生效；如果把 timeout、5xx 这类临时故障缓存成不存在，还会掩盖系统恢复。攻击者也可以制造大量随机不存在 key，污染缓存容量。权限类负缓存如果 key 没有包含租户、用户和权限维度，还可能产生越权或误拒绝。解决方式是短 TTL、创建和授权时主动失效、区分 not_found 和 backend_error、限制负缓存占比，并让负缓存 tombstone 也带版本号。
```

## Q021. 多级缓存如何设计失效顺序？

**回答：**

多级缓存的失效顺序不能只背一句“先删本地缓存还是先删 Redis”。真正要先定清楚的是层级、事实源和并发窗口。

一个常见结构是：

```text
L0: 进程内本地缓存
  例如 Caffeine、Guava、Go map + TTL。
  延迟最低，但只对当前实例可见。

L1: 分布式缓存
  例如 Redis、Memcached。
  多个实例共享，延迟比本地缓存高，但一致性和复用更好。

L2: 数据源
  例如数据库、对象存储、搜索索引、配置中心。
  通常是事实源。
```

读路径通常是从近到远：

```text
read:
  local cache -> distributed cache -> database/source
```

失效路径一般要反过来围绕事实源设计。更稳的写路径是：

```text
1. 先更新事实源，例如数据库。
2. 数据库提交后，删除或更新分布式缓存。
3. 发布带 key 和 version 的失效事件。
4. 各应用实例收到事件后删除本地缓存。
5. 必要时做延迟二次删除或版本校验，处理并发旧值回填。
```

为什么不建议先删缓存再写数据库？Azure Cache-Aside 文档里的示例就明确提醒过这个问题：如果先删缓存，另一个客户端可能在数据库更新前读到旧值，并把旧值重新放回缓存。这个窗口很短，但高并发下足够制造线上旧值。

所以更常见的顺序是：

```text
update database -> invalidate distributed cache -> invalidate local caches
```

但这只是基本顺序，不是强一致保证。因为本地缓存分散在每个进程里，服务端通常没有办法同步、可靠、立即地把所有本地缓存删干净。工程上要靠事件、TTL 和版本号兜底。

一个更完整的协议可以这样写：

```text
write(entity_id, new_value):
  tx:
    update table set value = new_value, version = version + 1
    insert outbox_event(key, new_version, type = invalidate)
  commit

  worker:
    delete distributed_cache[key]
    publish invalidate(key, new_version)

  each app instance:
    if local_cache[key].version <= new_version:
      local_cache.delete(key)
```

这里有几个关键点。

第一，失效事件要带版本。否则消息乱序时，旧失效事件可能删掉新缓存，或者旧更新事件覆盖新值。

第二，本地缓存 TTL 应该短于或等于分布式缓存 TTL。因为本地缓存最难统一失效，不能让它比共享缓存活得更久。

第三，读取时也要带版本意识。比如本地缓存 miss 后从 Redis 读到 version=10，而当前进程刚收到过 version=11 的失效事件，就不能把 version=10 放回本地缓存。

第四，批量失效不要用全量扫描 key。缓存 key 设计时就要支持按实体、租户、业务域定位，或者使用 namespace version：

```text
user_profile:v12:tenant_1:user_123
```

如果一次大版本发布需要废弃旧结构，可以把 namespace 从 `v12` 切到 `v13`，让旧 key 自然过期。这样比扫描 Redis 删除一堆 key 更稳。

多级缓存还有一种常见问题：本地缓存失效了，但分布式缓存里仍然是旧值。于是本地缓存下一次 miss 又从 Redis 把旧值读回来。要避免这个问题，分布式缓存失效应该先于本地缓存事件生效，或者本地缓存事件里带版本，让进程知道“低于这个版本的值都不能再接受”。

失效顺序可以总结成三句话：

```text
事实源先提交。
共享缓存先失效。
本地缓存靠事件、短 TTL 和版本号收敛。
```

如果业务要求强一致，比如余额、权限、库存扣减，就不要只靠多级缓存失效顺序。可以直接读事实源，或者把缓存只用于非关键展示字段。

面试里可以这样答：

```text
多级缓存读路径通常是本地缓存、分布式缓存、数据库；失效路径要围绕事实源设计。常见做法是先更新数据库并提交，再删除或更新分布式缓存，然后发布带 key 和 version 的失效事件，让各实例删除本地缓存。本地缓存最难统一失效，所以 TTL 要更短，失效事件要带版本，读取和回填也要拒绝旧版本。不要简单先删缓存再写数据库，因为并发读可能把旧值重新回填。强一致数据不要只依赖这套弱一致失效链路。
```

## Q022. 本地缓存和分布式缓存的 trade-off 是什么？

**回答：**

本地缓存和分布式缓存解决的是同一个问题的不同侧面。本地缓存追求极低延迟和少一次网络跳转；分布式缓存追求跨实例共享和集中管理。

本地缓存的优势很直接：

```text
延迟低:
  进程内内存访问通常比访问 Redis、数据库这类网络服务快很多。

不占网络:
  高频小对象不用每次走 TCP、序列化、反序列化。

隔离性好:
  Redis 抖动时，本地已有的热数据仍然可以撑一段时间。

实现简单:
  在单进程内用 Caffeine、Guava、sync.Map、LRU map 就能做。
```

Redis client-side caching 文档也说明了这个价值：访问本机内存比访问网络服务快很多，如果一小部分数据被频繁访问，本地缓存能降低应用延迟和数据库负载。

但本地缓存的问题同样明显：

```text
跨实例不共享:
  A 实例热起来，不代表 B 实例也热。

一致性差:
  每个实例都有自己的副本，删除和更新很难同时到达。

内存重复:
  100 个实例都缓存同一批热点对象，总内存会被放大 100 倍。

发布和扩缩容影响大:
  新实例冷启动，老实例下线，本地命中率会波动。

难做全局容量控制:
  每个实例只知道自己的内存，不知道全局缓存状态。
```

分布式缓存的优势是共享：

```text
跨实例复用:
  一个实例写入 Redis，其他实例也能命中。

集中淘汰:
  容量、TTL、淘汰策略、指标可以统一管理。

更适合会话和共享读模型:
  多个服务实例看到的是同一个缓存层。

更容易做热 key 观测:
  Redis 这类系统能提供 keyspace hits/misses、evicted_keys、hotkeys 等线索。
```

它的成本是网络和系统复杂度：

```text
多一次网络访问:
  p50 可能还能接受，p99 会受网络、连接池、Redis 排队影响。

需要序列化:
  对象越大，CPU 和字节开销越明显。

会成为共享依赖:
  Redis 故障、慢查询、集群迁移会影响所有实例。

一致性仍然不是强一致:
  Azure 缓存指导也指出，分布式缓存通常要在一致性和可用性之间取舍。
```

所以真实系统常用两级结构：

```text
L0 local cache:
  放极热、小、变化慢、能容忍短暂陈旧的数据。
  TTL 短，容量小，通常只做几十秒到几分钟。

L1 distributed cache:
  放跨实例共享、加载成本高、需要统一失效的数据。
  TTL 可以更长，容量更大。

database/source:
  事实源。
```

选型时可以按问题判断：

```text
数据是否每个实例都会反复读？
  是，本地缓存收益高。

数据是否需要跨实例共享？
  是，分布式缓存更合适。

能接受多旧的数据？
  不能接受，就少用本地缓存，或者只缓存可校验的版本。

对象是否很大？
  很大时，本地和分布式缓存都会吃内存，要考虑拆分和压缩。

失效事件是否可靠？
  不可靠时，本地缓存 TTL 要短，不能承担关键一致性。
```

Redis 的 client-side caching 提供了一种折中：客户端本地缓存数据，Redis 负责跟踪某些 key，并在 key 被修改、过期或淘汰时向客户端发 invalidation message。文档也提醒，这会引入服务端跟踪内存、广播 CPU、连接断开后陈旧数据等问题。所以它不是免费的一致性协议，只是把“本地缓存失效”做得更有边界。

面试里可以这样答：

```text
本地缓存的优势是延迟最低、没有网络开销、Redis 故障时还能短暂兜底；缺点是每个实例一份副本，内存重复，冷启动明显，跨实例一致性弱。分布式缓存的优势是多实例共享、容量和淘汰统一、指标集中；缺点是多一次网络和序列化，也会成为共享依赖，故障会影响所有实例。常见设计是本地缓存只放小而热、变化慢、可容忍短暂陈旧的数据，TTL 更短；Redis 放跨实例共享的读模型，并用版本号、失效事件或 Redis tracking 控制本地缓存陈旧。
```

## Q023. 缓存 key 设计不合理会导致什么问题？

**回答：**

缓存 key 是缓存系统的地址协议。key 设计错了，后面 TTL、淘汰、失效、观测和扩展都会变得很难。

第一类问题是 key 冲突。不同业务对象映射到了同一个 key，轻则返回错数据，重则造成越权。

```text
错误:
  cache key = "123"

可能代表:
  user:123
  order:123
  product:123
```

Azure 缓存指导建议使用有结构的 key，例如 `customer:100`，而不是只有 `100`。这个建议看似简单，但非常实用。缓存 key 至少要表达业务域和实体类型。

第二类问题是缺少隔离维度。多租户、用户权限、语言、地区、AB 实验、接口版本都可能影响结果。如果 key 没有包含这些维度，就会把一个上下文里的结果复用到另一个上下文。

```text
错误:
  product_detail:123

可能缺少:
  tenant_id
  region
  currency
  language
  user_segment
  schema_version
```

比如价格缓存没有包含币种，美元价格可能被人民币页面读到；权限缓存没有包含 user_id，A 用户的权限结论可能影响 B 用户。

第三类问题是 key 太长或太随机。key 本身也占内存，也要参与网络传输和哈希计算。把完整 URL、完整 JSON 查询条件、长 token、用户输入原文直接塞进 key，会带来几种后果：

```text
内存浪费:
  key 比 value 还大。

高基数:
  每次请求都是新 key，命中率很低。

观测困难:
  指标系统被高基数标签打爆。

敏感信息泄露:
  key 里可能出现手机号、邮箱、token、身份证号。
```

常见做法是对复杂参数做规范化，再哈希：

```text
search:v3:tenant_7:sha256(canonical_query)
```

这里的 canonical_query 要保证参数顺序、默认值、大小写处理一致。否则同一个语义请求会生成多个 key：

```text
?a=1&b=2
?b=2&a=1
```

如果没有规范化，这两个请求会变成两个缓存项，命中率下降，失效也会变难。

第四类问题是 key 不利于失效。比如缓存了很多商品详情，但 key 没有统一前缀，也没有实体 ID，就很难在商品更新时定位要删除哪些缓存。

```text
较差:
  cache:93a8f91b

较好:
  product_detail:v4:tenant_1:product_123
```

如果一个实体影响多个缓存项，可以设计反向索引或版本命名空间：

```text
product_version:123 = 42
product_detail:v42:123
product_recommend:v42:123
```

第五类问题是集群分片不均。Redis Cluster 根据 key 的哈希槽分布数据。单个热点 key 会落在一个 shard 上，怎么扩容都不能把这个 key 的请求自然分散出去。Redis 的反模式文档也明确把 hot keys 列为高风险问题，并建议把频繁访问的数据分散到多个 shard。

第六类问题是 key schema 没有版本。缓存 value 的结构变了，但 key 还沿用旧版本，新代码读到旧结构就可能反序列化失败，或者更糟，按旧字段解释成错误结果。

比较稳的 key 设计通常长这样：

```text
{domain}:{schema_version}:{tenant}:{entity_id}:{view}:{params_hash}

例子:
product:v3:tenant_7:id_123:detail:lang_zh
search:v2:tenant_7:q_9af2c1
permission:v5:tenant_7:user_8:repo_10:read
```

面试里可以这样答：

```text
缓存 key 设计不好会导致错读、越权、命中率低、失效困难、内存浪费和热点分片问题。key 太粗会冲突，比如 user:123 和 order:123 混在一起；key 缺少租户、用户、地区、语言、版本等维度，会把不同上下文的数据串起来；key 太长或直接包含用户输入，会增加内存和网络成本，还可能泄露敏感信息；key 太随机会导致高基数和低命中。好的 key 应该有业务前缀、schema version、隔离维度、规范化参数和必要的 hash，并且能支持定位失效和热点拆分。
```

## Q024. 缓存对象过大有什么风险？

**回答：**

缓存对象过大，问题不只是“占内存”。它会同时影响网络、CPU、GC、淘汰、公平性和可用性。

最直观的是内存风险：

```text
单个对象很大:
  一个 key 就占掉大量缓存空间。

对象大小差异大:
  按条目数淘汰不公平，一个大对象可能挤掉很多小热点。

元数据和碎片:
  大对象更容易放大内存碎片和 allocator 压力。
```

如果缓存策略只看 entry count，而不看 weight，一个 5 MB 对象和一个 500 B 对象都算一条，就会严重误判容量。Caffeine 支持 maximumWeight 这类按权重淘汰的思路，Redis 侧也要通过 value size、MEMORY USAGE、bigkey 扫描等方式观察。

第二是网络和序列化成本。缓存命中本来应该很快，但大对象会让命中路径也变慢：

```text
Redis -> application:
  大量字节传输，TCP 分包，客户端缓冲区变大。

application:
  JSON/Protobuf/MessagePack 反序列化耗 CPU。

runtime:
  大对象分配增加 GC 压力，尤其是 Java、Go、.NET 这类托管运行时。
```

命中率很高但 p99 仍然很差，常见原因之一就是 value 太大。缓存省掉了数据库查询，但没省掉大对象搬运和解析。

第三是阻塞和复制风险。Redis 主线程执行命令，大 value 的读写、删除、复制、AOF 追加、迁移都更重。大 key 在集群迁移、主从复制、备份恢复时也会放大尾延迟。

第四是更新粒度太粗。很多系统把完整 JSON 文档作为 string 存进 Redis：

```text
user_profile:123 -> 200 KB JSON
```

如果只想更新头像字段，也要读出整段 JSON、反序列化、修改、序列化、写回。Redis 反模式文档也提到，把 JSON blob 存在字符串里会导致每次读写都要处理完整文档，原子字段更新也会变难。更合适的做法可能是：

```text
拆字段:
  用 HASH 或 RedisJSON 存储需要独立读写的字段。

拆视图:
  列表页缓存摘要，详情页缓存完整对象。

拆分页:
  大列表按 page 或 cursor 缓存，不把几万条放一个 key。

只缓存派生结果:
  不缓存完整对象，缓存渲染需要的最小字段集合。
```

第五是热点风险。一个很大的热点 key 被频繁读取，会让某个 Redis shard 同时承受高 QPS 和高带宽。即使 CPU 不满，网卡、客户端缓冲区和复制链路也可能先到瓶颈。

治理大对象一般从几个方向做：

```text
设置 value size 上限:
  超过阈值不缓存，或者只缓存摘要。

按权重淘汰:
  不让大对象和小对象在淘汰策略里完全等价。

拆分对象:
  按字段、分页、业务视图拆 key。

压缩:
  只在 CPU 余量足、对象可压缩、网络是瓶颈时使用。
  压缩不能掩盖对象模型错误。

观测:
  记录 p50/p95/p99 value bytes、序列化耗时、get/set 耗时、big key 数量。
```

面试里可以这样答：

```text
缓存对象过大会让命中路径也变慢。它会占用大量内存，挤掉小而热的数据；会增加网络传输、序列化、反序列化和 GC 成本；在 Redis 里还会放大主线程处理、复制、AOF、迁移和删除的尾延迟。大 JSON blob 还会让字段级更新变困难，每次都要处理完整文档。治理方式是设置 value size 上限，按 weight 淘汰，拆字段、拆视图、拆分页，只缓存必要字段，并监控 value bytes、big key、序列化耗时和 p99 缓存延迟。
```

## Q025. 如何观测缓存命中率、驱逐率、加载延迟和热点 key？

**回答：**

缓存观测要按层拆开看。一个接口可能先查本地缓存，再查 Redis，最后查数据库。如果只看总命中率，很容易误判。

最基础的指标是：

```text
request_total:
  缓存读请求总数。

hit_total / miss_total:
  命中和未命中次数。

hit_rate:
  hit / (hit + miss)。

byte_hit_rate:
  命中字节数 / 请求字节数。
  大对象场景里比对象命中率更有意义。

stale_hit_total:
  返回旧值的次数，例如 stale-while-revalidate。
```

这些指标最好按层拆：

```text
local_cache_hit_rate
redis_cache_hit_rate
overall_cache_hit_rate
database_load_total
```

否则会出现这种误判：

```text
overall hit rate = 95%
local hit rate = 5%
redis hit rate = 90%
```

这说明本地缓存基本没发挥作用，只是 Redis 在扛。如果本地缓存占了很多内存，就要重新评估。

驱逐率要区分 eviction 和 expiration：

```text
expired:
  TTL 到期，属于新鲜度策略。

evicted:
  容量不足，被淘汰。

explicit invalidation:
  业务主动删除。
```

Redis 的 `INFO` 命令会暴露 `keyspace_hits`、`keyspace_misses`、`expired_keys`、`evicted_keys`、`total_eviction_exceeded_time` 等字段。Caffeine 也可以通过 `recordStats()` 打开统计，`Cache.stats()` 会返回 `hitRate()`、`evictionCount()`、`averageLoadPenalty()` 等指标。这些都适合接入 Prometheus、Micrometer、OpenTelemetry 或内部监控系统。

加载延迟要单独看。miss 后加载数据通常才是缓存系统真正的危险路径：

```text
cache_get_latency:
  查缓存本身花了多久。

loader_latency:
  从数据库或下游服务加载花了多久。

serialization_latency:
  编码和解码耗时。

singleflight_wait_latency:
  等待同 key 加载结果花了多久。

loader_inflight:
  当前正在回源的加载数。

loader_error_total:
  加载失败次数。
```

命中率下降不可怕，命中率下降同时 loader p99 上升、inflight 上升、数据库连接池打满才危险。

热点 key 观测要小心指标基数。不能把完整 key 都打成 Prometheus label，否则监控系统会被高基数拖垮。更常见的做法是：

```text
按 key prefix 聚合:
  product_detail、user_profile、permission_check。

采样记录 top-N:
  在应用侧用近似计数器、Count-Min Sketch、滑动窗口 top-K。

Redis 侧工具:
  使用 redis-cli --hotkeys、--bigkeys、--memkeys，或查看 Redis Enterprise/Cloud 的热点分析。

链路追踪:
  span 上记录 cache.layer、cache.hit、cache.key_prefix、value_size，不记录完整敏感 key。

访问日志离线分析:
  从请求日志、缓存访问日志中计算 top key、reuse distance、对象大小分布。
```

热点不一定都是坏事。缓存本来就是利用热点。真正要报警的是：

```text
单 key QPS 接近单 shard 上限。
热点 key miss 或过期导致回源尖峰。
热点 key value 过大，带宽先打满。
热点 key 写入频繁，失效风暴。
```

一个比较实用的仪表盘会同时放这些图：

```text
hit rate by layer and prefix
miss rate by prefix
eviction / expiration rate
cache get/set p50/p95/p99
loader p50/p95/p99
loader inflight and errors
top key approximate QPS
value size distribution
Redis memory, fragmentation, rejected connections, command latency
database QPS caused by cache miss
```

面试里可以这样答：

```text
缓存观测要分层、分前缀看，不能只看一个总 hit rate。本地缓存、Redis、整体命中率要分开；驱逐要区分 TTL 过期、容量淘汰和业务主动删除；加载延迟要看 loader p95/p99、inflight、错误率和 singleflight 等待时间。Redis 可以用 INFO 看 keyspace_hits、keyspace_misses、expired_keys、evicted_keys 等字段；Caffeine 可以用 recordStats 采集 hitRate、evictionCount、averageLoadPenalty。热点 key 不要直接作为高基数 label 打到监控里，通常用 prefix 聚合、采样 top-N、redis-cli --hotkeys、访问日志和 trace tag 来定位。
```

## Q026. 热点 key 如何拆分或保护？

**回答：**

热点 key 先要分类。读热点、写热点、大对象热点，处理方式不一样。

读热点是最常见的：

```text
product:123
home_config
activity:current
ranking:top10
```

如果它是小对象、变化慢，保护方式比较直接：

```text
本地缓存:
  每个应用实例本地保留一份，减少 Redis QPS。

更长 TTL + 主动失效:
  不让热点 key 频繁自然过期。

stale-while-revalidate:
  过期后先返回旧值，后台刷新。

singleflight:
  同实例内同 key 只允许一个请求回源。

预热:
  活动开始前把热点 key 放进缓存。
```

如果它是读热点但更新也比较频繁，就要小心本地缓存和长 TTL。可以用逻辑过期和版本号：

```text
value = {
  data: ...,
  version: 42,
  expire_at: 2026-06-17T10:00:00Z
}
```

请求读到逻辑过期值时，如果没有刷新任务在跑，就触发后台刷新；用户可以短时间拿旧值。写路径通过版本号和失效事件让旧值尽快收敛。

如果热点来自单个 Redis Cluster key，扩容 shard 本身不一定有用。Redis 反模式文档解释过这个问题：一个 key 由哈希决定落到一个 shard 上，单 key 的大量访问会集中到同一个节点。拆分方式通常有几种。

第一种是副本拆分，适合读多写少、值较小的对象：

```text
product:123:copy:0
product:123:copy:1
product:123:copy:2
...
```

读请求按随机、轮询或一致性哈希选择一个 copy。写入或失效时要处理所有 copy。它能分散读压力，但写放大和一致性复杂度会上升。

第二种是字段拆分，适合大对象：

```text
product:123:base
product:123:price
product:123:stock
product:123:comments_summary
```

这样不会每次读取都搬完整对象，也能让更新频率不同的字段使用不同 TTL。

第三种是分页拆分，适合大列表：

```text
ranking:daily:page:1
ranking:daily:page:2
ranking:daily:page:3
```

不要把几万条榜单数据塞进一个 key。大 key 热起来之后，带宽和序列化会先出问题。

第四种是分片计数，适合写热点：

```text
counter:article_123:shard:0
counter:article_123:shard:1
...
counter:article_123:shard:63
```

写请求随机写某个 shard，读请求求和。这样能把写压力分散出去，但读路径变重，并且只能用于可聚合的数据，例如计数器、点赞数、限流桶。

除了拆 key，还可以保护回源：

```text
限流:
  热点 miss 时限制回源并发。

熔断:
  后端慢时返回旧值或降级值。

请求合并:
  singleflight 或分布式锁。

TTL jitter:
  避免热点集合同时过期。

专门缓存池:
  热点 key 使用单独容量，避免被普通 key 挤掉。
```

拆分不是越多越好。拆得越细，失效越复杂，读路径聚合成本越高，数据一致性越难解释。面试里要说清楚：先确认瓶颈在单 key、单 shard、网络带宽、回源还是序列化，再选择拆分方式。

面试里可以这样答：

```text
热点 key 先分读热点、写热点和大对象热点。读多写少的小对象可以用本地缓存、长 TTL 加主动失效、预热、singleflight 和 stale-while-revalidate 保护；单 Redis key 压到一个 shard 时，可以做多副本 key，把读请求分散到 product:123:copy:n，但写入和失效会放大。大对象热点要按字段或分页拆，避免每次搬完整 value。写热点如计数器可以做分片 counter，写随机 shard，读时聚合。拆分会带来一致性和失效复杂度，所以要先确认瓶颈，再决定是拆 key、加本地缓存、限流，还是保护回源。
```

## Q027. cache warming 是否总是有必要？

**回答：**

cache warming，也就是缓存预热，不是总是有必要。它适合“我们明确知道哪些数据会被访问，而且冷 miss 成本很高”的场景。

适合预热的场景：

```text
热点集合已知:
  首页配置、活动商品、排行榜、热门文章、模型配置。

启动冷 miss 成本高:
  服务刚发布、缓存刚重启，如果不预热会让第一波用户打到数据库。

活动时间明确:
  秒杀、直播、考试、抢票开始前，热点 key 很确定。

数据集小且静态:
  Azure Cache-Aside 文档也提到，如果静态数据集能放进缓存，可以启动时 prime cache，并使用不让它过期的策略。

加载过程很慢:
  比如需要复杂 join、远程 RPC、对象存储读取、模型反序列化。
```

不适合预热的场景同样多：

```text
热点不可预测:
  用户请求高度长尾，预热命中不了多少。

数据变化太快:
  刚预热完就过期或被更新，浪费后端资源。

数据集太大:
  预热接近全量加载，会污染缓存并挤掉真正热点。

发布链路敏感:
  预热时间过长，拖慢部署和扩容。

后端脆弱:
  多个实例同时预热，可能比自然 miss 更容易把数据库打爆。
```

预热最常见的误用是“把全库扫进缓存”。这通常不是预热，而是把缓存当数据库副本。结果是启动慢、缓存污染、内存压力大、真正热点反而被挤掉。

更稳的预热应该有范围和节奏：

```text
top-N:
  根据最近访问日志、活动配置或运营列表预热，不全量扫库。

rate limit:
  控制预热 QPS，不能影响正常业务读写。

batch:
  使用批量接口，减少小请求风暴。

jitter:
  预热写入时给 TTL 加抖动，避免之后一起过期。

version:
  key 带 schema version，避免新版本读旧结构。

observe:
  预热后看 warm hit rate、数据库 QPS、缓存污染和对象大小。
```

还有一个细节：预热不一定要同步阻塞启动。很多服务可以先启动，对外提供降级能力，然后后台按优先级预热：

```text
phase 1:
  加载必须存在的小配置。

phase 2:
  预热核心热点 key。

phase 3:
  低优先级数据后台慢慢加载。
```

这样比“服务不预热完就不启动”更稳。除非这些缓存是服务正确性的必要条件，否则不要让预热成为发布单点。

面试里可以这样答：

```text
cache warming 不总是必要。它适合热点集合已知、冷 miss 成本高、活动时间明确、数据集小而静态的场景，比如首页配置、活动商品、排行榜。它不适合热点不可预测、数据变化快、数据集太大或后端承受不了批量加载的场景。错误的预热会污染缓存、拖慢发布，甚至把数据库打爆。生产上更好的做法是按 top-N 和业务优先级预热，限速批量加载，TTL 加 jitter，key 带版本，并通过预热后的 hit rate 和后端 QPS 判断是否真的有效。
```

## Q028. prefetch 什么时候有效，什么时候会污染缓存？

**回答：**

prefetch 是在请求真正需要某个对象之前，提前把它加载进缓存。它和 cache warming 的区别是：warming 通常发生在启动、发布或活动前；prefetch 更常发生在运行时，根据当前访问预测下一步访问。

prefetch 有效的前提是“预测准，而且资源还有余量”。

典型有效场景：

```text
顺序访问:
  用户打开第 1 页时，提前加载第 2 页。

强关联对象:
  读商品详情时，提前加载价格、库存、卖家摘要。

批量读取更便宜:
  Caffeine 的 loadAll 文档也提到，如果批量获取比逐个获取更高效，可以实现 loadAll。

分组计算:
  计算某个 group 中任意一个 key 时，顺手能得到同组其他 key。

流式处理:
  消费者按 offset 递增读取，提前拉后续 block 能减少等待。

CDN/静态资源:
  页面即将需要的 JS、CSS、图片可以适度预取。
```

这些场景有一个共同点：当前访问和未来访问之间有稳定关系。预取的数据大概率会在短时间内被用到。

prefetch 会污染缓存的场景也很常见：

```text
低预测命中:
  预取 100 个对象，最后只用到 1 个。

扫描 workload:
  后台任务顺序扫全表，预取把正常热点挤掉。

对象很大:
  预取对象占用带宽和内存，收益不确定。

个性化强:
  每个用户下一步都不同，预取难复用。

下游已经紧张:
  预取抢占了正常请求的数据库、网络和连接池资源。

TTL 很短:
  预取完还没用就过期。
```

所以 prefetch 不能只看“是否减少了某些请求延迟”，还要看它有没有抢资源。一个好的 prefetch 机制通常会有几条限制：

```text
预算:
  每个请求最多预取多少个 key。

优先级:
  prefetch 的优先级低于真实用户请求。

准入:
  只有预测概率足够高、对象足够小、近期会复用时才放入主缓存。

隔离:
  扫描任务和预取任务使用单独缓存区或低优先级队列。

可取消:
  用户离开页面、请求取消后，后续预取也要停止。

指标:
  统计 prefetched_hit、prefetched_evicted_before_hit、prefetch_bytes、prefetch_backend_qps。
```

一个非常有用的指标是：

```text
prefetch useful rate =
  预取后被真正命中的对象数 / 预取对象数
```

还要看：

```text
prefetch pollution =
  预取对象在首次命中前就被淘汰的比例
```

如果 useful rate 低、pollution 高，prefetch 就是在用后端资源和缓存空间制造噪声。

面试里可以这样答：

```text
prefetch 在未来访问可预测、对象较小、批量加载更便宜、后端有余量时有效，比如分页下一页、同组对象、顺序 block、商品详情的关联字段。它会在预测不准、扫描 workload、对象很大、个性化很强或后端已经紧张时污染缓存，因为预取数据可能还没被用到就挤掉真正热点。工程上要给 prefetch 设置预算、低优先级、准入条件和可取消逻辑，并监控 prefetched_hit、预取后未命中就被淘汰的比例、预取字节数和后端 QPS。
```

## Q029. 缓存容量如何通过工作集大小估算？

**回答：**

工作集大小可以理解为：在某个时间窗口内，系统真正会反复访问、值得留在缓存里的那批数据有多大。缓存容量估算不是问“数据库有多大”，而是问“近期会被复用的数据有多大，miss 成本有多高”。

第一步是采集访问轨迹：

```text
timestamp
key
key_prefix
object_size_bytes
hit_or_miss
load_latency
tenant/user/route 等低基数维度
```

不要只采样接口 QPS。容量问题要看 key 级别的复用和对象大小。

第二步是选时间窗口。窗口可以按业务来定：

```text
5 分钟:
  高频接口、热点商品、首页配置。

1 小时:
  推荐、搜索、用户资料。

1 天:
  日榜、课程、知识库、相对静态内容。
```

然后计算每个窗口内被访问过的 distinct key 和 distinct bytes：

```text
working_set_bytes(window) =
  sum(size(key)) for keys accessed in window
```

如果 95% 的 10 分钟窗口里，活跃 key 的总大小在 20 GB 以下，那么 20 GB 是一个容量估算起点，不是最终答案。

第三步是看复用距离和 miss ratio curve。LRU 类缓存更关心“一个 key 两次访问之间，插入了多少其他数据”。如果一个 key 每隔 1 分钟访问一次，但这一分钟内会访问 100 GB 其他对象，而缓存只有 10 GB，它仍然可能留不住。

可以用访问日志模拟不同容量下的命中率：

```text
capacity=1GB   -> hit rate 60%
capacity=2GB   -> hit rate 72%
capacity=4GB   -> hit rate 82%
capacity=8GB   -> hit rate 87%
capacity=16GB  -> hit rate 89%
```

这里 8 GB 到 16 GB 只提升 2 个点，就可能已经过了收益拐点。容量应该选在“边际收益还值得”的位置，而不是盲目追求 99% 命中。

第四步要按字节和成本加权。对象命中率可能骗人：

```text
1000 个小对象命中，省了 1 MB。
1 个大对象 miss，回源 50 MB。
```

所以要同时看：

```text
object hit rate:
  请求数量维度。

byte hit rate:
  网络和内存维度。

cost-weighted hit rate:
  miss 延迟或数据库成本维度。
```

第五步加安全余量：

```text
metadata overhead:
  key、value 包装、TTL、LRU/LFU 元数据。

fragmentation:
  allocator 碎片和对象大小分布。

replication:
  主从、副本、本地多实例都会放大内存。

burst:
  活动、发布、热点迁移、租户突增。

negative cache:
  负缓存也会占容量。

reserved headroom:
  不要把 Redis 内存打到 100%，否则容易 eviction storm。
```

一个实用公式可以写成：

```text
target_capacity =
  p95(working_set_bytes at chosen window)
  * metadata_factor
  * replication_factor
  * burst_factor
```

其中 `metadata_factor` 和 `burst_factor` 不能拍脑袋，最好通过压测和线上采样校准。

最后要用真实流量验证。上线后看：

```text
evicted_keys 是否持续增长
hit rate 是否达到预期
loader latency 是否下降
数据库 QPS 是否下降
热点 key 是否还会被淘汰
value size 分布是否偏离预估
```

如果增加容量后 hit rate 基本不变，说明瓶颈不在容量，可能是 TTL 太短、key 设计太细、访问本身无复用、缓存对象太大，或者请求参数没有规范化。

面试里可以这样答：

```text
缓存容量不要按数据库总大小估，要按工作集大小估。先采集访问日志，包括 key、时间、对象大小、加载成本；再按业务窗口计算活跃 distinct bytes，并用 LRU 模拟或 miss ratio curve 看不同容量下的命中率。容量通常选在命中率收益曲线的拐点附近，同时按对象大小和 miss 成本加权，不只看请求命中率。最后还要加 key 元数据、内存碎片、复制、本地多实例、突发流量和负缓存的余量。上线后用 evicted_keys、hit rate、loader latency、数据库 QPS 和 value size 分布反向校准。
```

## Q030. 缓存和 backpressure 有什么关系？

**回答：**

缓存和 backpressure 是两种不同的保护手段。缓存减少后端请求量，backpressure 控制请求进入系统的速度。两者经常一起出现，因为缓存 miss、过期、回源加载都会把压力重新传给后端。

Reactive Streams 对 backpressure 的核心描述是：异步系统里要控制资源消耗，不能让快的生产者迫使慢的消费者无限缓冲。放到缓存系统里，生产者可以是用户请求，消费者可以是缓存 loader、数据库、Redis、下游 RPC。

缓存命中时：

```text
request -> cache hit -> return
```

后端没有压力，backpressure 基本不用介入。

缓存 miss 时：

```text
request -> cache miss -> loader -> database/RPC -> cache set -> return
```

这条路径就需要 backpressure。否则大量 miss 会同时创建大量 loader 任务，打满数据库连接池、线程池、Redis 连接池和服务内存。

cache stampede 本质上就是缓存系统缺少回源侧 backpressure：

```text
热点 key 过期。
10000 个请求同时 miss。
10000 个请求同时回源。
数据库变慢。
请求超时重试。
压力继续放大。
```

更合理的设计是：

```text
同 key:
  singleflight 合并，只让一个加载任务执行。

全局:
  loader 并发有上限，超过上限排队或快速失败。

队列:
  队列长度有上限，不能无限堆积。

超时:
  loader 必须带 context deadline。

降级:
  后端慢时返回旧值、默认值或明确的降级响应。

限流:
  对异常 miss、高基数 key、恶意请求限流。
```

缓存还可能掩盖后端容量问题。平时 hit rate 99%，数据库看起来很轻松；一旦缓存重启、大量 key 过期、版本切换或 Redis 故障，剩下 1% 的真实后端容量可能根本撑不住。这时缓存不是保护层，而是把问题推迟到了失效时刻。

所以容量设计里要问：

```text
如果 Redis 完全不可用，数据库能承受多少流量？
如果热点 key 过期，最多允许多少并发回源？
如果 loader p99 从 20ms 变成 2s，等待队列会不会爆？
如果 miss 请求被重试，是否会放大压力？
```

缓存和 backpressure 的组合策略通常是：

```text
stale-while-revalidate:
  回源慢时先返回旧值，后台刷新。

adaptive TTL:
  后端压力高时适当延长非关键数据 TTL。

negative cache:
  对不存在对象短时间缓存，减少无效回源。

singleflight:
  合并同 key 并发加载。

bulkhead:
  缓存 loader 使用独立线程池和连接池，避免拖垮主请求池。

circuit breaker:
  数据库或下游异常时停止继续制造回源请求。

request shedding:
  超过系统承载能力时丢弃低优先级请求。
```

不要把缓存当成无界队列。比如“Redis 写不进去就先放内存队列，慢慢写”听起来像降级，实际上可能把进程内存撑爆。backpressure 的核心就是让压力在入口处被看见，而不是藏到某个无界 buffer 里。

观测上要重点看：

```text
miss rate
loader inflight
loader queue length
singleflight waiters per key
loader timeout/error
stale served count
rejected/shed requests
database QPS caused by miss
Redis latency and rejected connections
```

面试里可以这样答：

```text
缓存负责减少后端请求，backpressure 负责在 miss、回源和刷新路径上限制进入系统的压力。命中时压力被缓存吸收；miss 时如果没有 backpressure，就会出现 stampede，大量请求同时回源，把数据库、线程池和连接池打满。工程上要给 loader 设置并发上限、队列上限和超时，用 singleflight 合并同 key 请求，用 stale-while-revalidate 返回旧值，用熔断、限流和降级保护后端。缓存不能当成无界 buffer，否则只是把压力藏起来；真正的 backpressure 要让过载在入口处被限制或拒绝。
```

## Q031. 如何为缓存策略做 trace-driven simulation？

**回答：**

trace-driven simulation 的目标是用真实访问轨迹复现缓存决策，而不是靠几个手写样例猜 LRU、LFU、TinyLFU 谁更好。它回答的问题通常是：

```text
同一批请求，在不同缓存容量下，命中率会怎么变？
同一批请求，用 LRU、LFU、ARC、TinyLFU，miss 成本差多少？
热点迁移、scan、burst、对象大小差异，对策略有什么影响？
```

Caffeine 的 Simulator wiki 就是一个典型例子。它提供一组 eviction policy 和分布生成器，可以用来判断某个策略是否适合特定 usage scenario，并支持多种公开 trace 格式。这个思路比单纯跑 synthetic benchmark 更接近线上，因为真实流量里的局部性、长尾、突发和扫描混在一起。

第一步是定义 trace 格式。最小字段可以是：

```text
timestamp
key
operation: get / put / delete
object_size_bytes
load_cost_ms
status: hit / miss / error，可选
tenant_or_prefix，低基数字段
```

如果要模拟一致性和 TTL，还要加：

```text
version
ttl
write_event
invalidate_event
source_version
```

key 必须脱敏。不能把用户 ID、手机号、token、完整 URL 查询参数直接放进 trace。一般做法是保留稳定哈希、key prefix、对象大小和时间关系：

```text
raw key:
  user_profile:tenant_7:user_123456

trace key:
  user_profile:tenant_hash_09:key_hash_a83f
```

第二步是清洗 trace。常见脏数据包括：

```text
重复采样:
  同一请求被网关和应用层记录两次。

时间乱序:
  多实例日志合并后 timestamp 不严格递增。

缺少 size:
  需要从缓存 value length、序列化长度或离线样本补齐。

错误请求:
  5xx、timeout、限流请求要不要进入模拟，要先定规则。

异常大 key:
  需要单独标注，否则会扭曲容量模拟。
```

第三步是实现 replay。最简单的 replay 是单线程按时间顺序处理：

```text
for event in trace:
  if event.operation == get:
    policy.recordRead(event.key, event.size, event.cost)
  if event.operation == put:
    policy.recordWrite(event.key, event.size)
  if event.operation == delete:
    policy.invalidate(event.key)
```

对 LRU 来说，读命中就把 key 移到最近端；miss 后是否 admit，要按策略定。对 LFU 来说，读命中和 miss 后都可能增加频率计数。对 TinyLFU 这类策略，还要模拟 admission policy，而不是只模拟 eviction。

第四步是做容量 sweep，而不是只测一个容量：

```text
capacity:
  1GB, 2GB, 4GB, 8GB, 16GB

metrics:
  object_hit_rate
  byte_hit_rate
  miss_count
  miss_cost_ms
  eviction_count
  admission_reject_count
```

这样可以画 miss-ratio curve。容量估算和策略选择都要看曲线的拐点。一个策略在 1GB 下领先，不代表在 16GB 下还领先。

第五步是分段分析。不能只看全局平均值：

```text
按 key prefix:
  user_profile、product_detail、permission、search。

按时间:
  高峰、低峰、发布后、活动期间。

按对象大小:
  小对象、中对象、大对象。

按 miss 成本:
  低成本 DB 查、复杂 join、远程 RPC、对象存储。
```

比如 LRU 全局 hit rate 低于 LFU，但在权限缓存上更稳；LFU 全局 hit rate 高，但在热点快速迁移时旧热点残留。只看一个数字会错过这些差异。

第六步是做验证。trace-driven simulation 很容易犯一个错误：用过去的完整 trace 调参数，然后宣称策略很好。这是过拟合。更合理的做法是：

```text
train window:
  用前一天或前一周流量选择参数。

test window:
  用后续流量评估，不再调参数。

stress trace:
  单独加入 scan、冷启动、热点切换、批量过期。
```

第七步要分清 simulation 和 benchmark。simulation 主要比较策略命中质量，不适合直接比较实现吞吐。Caffeine Simulator 文档也提醒，由于 batching 和 broadcasting，timing 只有在每个 policy 独立运行时才更可比。真正的吞吐、锁竞争、GC、序列化开销，还要跑实现级 benchmark。

面试里可以这样答：

```text
trace-driven simulation 是把真实访问日志清洗成 timestamp、key、operation、size、load cost 等事件，然后按时间顺序 replay 到不同缓存策略里。要做容量 sweep，输出 object hit rate、byte hit rate、miss cost、eviction count，并按业务前缀、时间窗口、对象大小和 miss 成本分段看。trace 要脱敏，清洗重复和乱序，训练窗口和测试窗口要分开，避免用同一批流量调参又评估。它适合比较策略命中质量，不能替代实现级 benchmark；吞吐、锁竞争和 GC 还要单独压测。
```

## Q032. 如何用基准测试比较 LRU 和 LFU？

**回答：**

比较 LRU 和 LFU 要拆成两件事：策略效果和实现成本。策略效果看命中率、字节命中率、miss 成本；实现成本看 CPU、内存、锁、GC 和尾延迟。把这两类混在一个数字里，很容易得出错误结论。

第一类测试是 trace simulation。用同一份 trace、同一容量、同一对象大小，分别跑 LRU 和 LFU：

```text
input:
  production trace
  capacity list
  object sizes
  optional miss costs

output:
  hit rate
  byte hit rate
  cost-weighted miss rate
  eviction count
  final resident set
```

这能回答“哪个策略更会留下有价值的数据”。它不回答“哪个实现更快”。

第二类测试是 microbenchmark 或 workload benchmark。Caffeine 的 Benchmark wiki 使用 JMH，并分别测 100% read、75% read / 25% write、100% write、多线程同 key、多线程分散 key 等场景。这个思路很适合拿来设计 LRU/LFU 对比：

```text
read-only:
  所有 key 预加载，测 get 命中开销。

read-miss:
  控制 miss 比例，测插入和淘汰成本。

mixed:
  读写混合，测锁竞争和元数据更新。

same hot key:
  多线程打同一个 key，测热点锁竞争。

Zipf:
  模拟长尾热点。

scan:
  加入一次性扫描，观察缓存污染。

phase shift:
  旧热点变冷，新热点出现，观察 LFU 是否历史包袱重。
```

第三类测试是端到端压测。比如通过真实服务接口压：

```text
client -> service -> local cache -> Redis -> database
```

这能看到序列化、网络、线程池、数据库连接池、singleflight、超时和重试的影响。LRU/LFU 的差异在这里可能被其他瓶颈掩盖，所以它适合验证线上收益，不适合单独归因策略。

比较时要固定变量：

```text
容量一致:
  按 entry count 比，还是按 bytes/weight 比，要说清楚。

TTL 一致:
  如果 TTL 不同，测到的是过期策略，不是 LRU/LFU。

loader 一致:
  miss 后加载成本要一样。

对象大小一致:
  如果对象大小差异大，最好按 weight 淘汰或同时报告 byte hit rate。

线程数一致:
  并发度、连接池、CPU 绑定要固定。

预热一致:
  冷启动和 warm cache 要分开测。
```

指标也要分层：

```text
策略指标:
  hit rate、byte hit rate、miss cost、eviction count。

实现指标:
  ns/op、ops/s、p50/p95/p99、B/op、allocs/op。

并发指标:
  lock wait、CAS retry、queue length、blocked goroutines/threads。

系统指标:
  CPU、GC、memory、network、database QPS。
```

LRU 通常实现简单，命中时更新一个 recency 链表或队列。LFU 要维护频率，精确 LFU 还要维护频率桶、minFreq、节点列表；近似 LFU 要维护计数器和衰减。Redis 的 LFU 是近似 LFU，使用概率计数和 decay。这个差异必须在 benchmark 里体现。否则只比命中率，会忽略维护频率的成本。

一个常见结论是：

```text
稳定长尾热点:
  LFU 往往更容易保留高频 key。

热点快速迁移:
  LRU 可能更快适应。

scan workload:
  纯 LRU 容易被污染，LFU 或 admission policy 通常更稳。

高并发小对象:
  LRU/LFU 的元数据更新成本可能比 value 读取还贵。
```

面试里可以这样答：

```text
比较 LRU 和 LFU 要分开测策略效果和实现成本。先用同一份 trace、同一容量、同一对象大小做 simulation，比较 hit rate、byte hit rate、miss cost 和 eviction count；再用 JMH、Go benchmark 或服务压测测 ns/op、ops/s、allocs、GC、锁竞争和 p99。workload 要覆盖 read-only、mixed read/write、Zipf、scan、phase shift、same hot key，冷启动和 warm cache 分开。LRU 维护 recency，成本低但容易被 scan 污染；LFU 维护频率，对稳定热点好，但有历史包袱和计数维护成本。
```

## Q033. 缓存命中但数据损坏如何检测？

**回答：**

缓存命中不等于数据正确。命中只说明 key 找到了，不能证明 value 没坏、没串租户、没过版本、没被错误序列化。

先区分几类问题：

```text
stale:
  数据是完整的，但版本旧。

corrupted:
  数据字节损坏、截断、反序列化失败、字段不合法。

wrong-key:
  key 设计或路由错误，读到了另一个对象。

wrong-context:
  缓存值属于另一个用户、租户、地区或权限上下文。

poisoned:
  攻击者或错误输入把恶意/错误内容写进缓存。
```

检测手段要分层。

第一层是反序列化和 schema 校验。缓存读出来后，不要直接信任：

```text
JSON/Protobuf decode 是否成功。
必填字段是否存在。
字段类型和范围是否合理。
schema_version 是否兼容。
tenant_id、entity_id 是否和 key 一致。
```

如果 key 是 `user:tenant_7:user_123`，value 里最好也带：

```json
{
  "tenant_id": "tenant_7",
  "user_id": "user_123",
  "version": 42,
  "schema_version": 3
}
```

读出后做交叉检查。这样能抓到 key/value 串写、反序列化错误和部分上下文泄露。

第二层是版本校验。value 带 version、updated_at、source_revision 或 ETag。RFC 9110 把 ETag 定义为区分同一资源多个 representation 的 opaque validator，并说明强 validator 应在可观察内容变化时变化。业务缓存也可以借鉴这个思想：缓存值携带一个能代表源数据版本的 validator。

```text
cache value:
  data
  version
  checksum
  source_snapshot_id
  generated_at
```

如果读路径能拿到当前版本，例如请求上下文里有权限版本、租户配置版本，就可以拒绝旧版本缓存。

第三层是 checksum 或 digest。写缓存时计算：

```text
checksum = crc32c(serialized_value)
或者
digest = sha256(canonical_payload)
```

读缓存时重新计算并比较。CRC32C 适合发现随机传输或存储损坏，成本低；SHA-256 更适合防篡改，但 CPU 更高。要注意：checksum 只能证明“字节和写入时一致”，不能证明写入时的数据就是业务正确的。

第四层是抽样回源校验。对关键缓存，可以按低比例抽样：

```text
1. 缓存命中，返回 value。
2. 异步或旁路读取事实源。
3. 比较 version、关键字段、hash。
4. 不一致时记录 drift，并删除缓存。
```

抽样比例要小，不能把数据库打爆。更适合核心权限、价格、库存、配置这类高风险数据。

第五层是异常指标：

```text
deserialize_error_total
schema_mismatch_total
checksum_mismatch_total
tenant_mismatch_total
version_regression_total
sample_drift_total
cache_hit_but_source_miss_total
```

这些指标比“缓存命中率”更能暴露正确性问题。

如果检测到损坏，处理顺序通常是：

```text
1. 不返回损坏值。
2. 删除或隔离该 key。
3. 回源重新加载。
4. 如果回源失败，按业务决定降级或报错。
5. 记录原始 key prefix、schema version、checksum、写入来源。
```

不要把损坏缓存当普通 miss 静默吞掉。否则线上会变成“偶发慢请求”，根因很难找。

面试里可以这样答：

```text
缓存命中只说明 key 存在，不说明 value 正确。检测要从反序列化、schema、key/value 交叉校验、版本和 checksum 几层做。value 里应带 tenant_id、entity_id、schema_version、version，读出后确认它和 key、请求上下文一致；关键数据可以带 CRC32C 或 SHA-256，读时重算比对；还可以低比例抽样回源比较 version 或关键字段。发现损坏后不要返回缓存值，要删除 key、回源重载，并记录 deserialize_error、checksum_mismatch、tenant_mismatch、version_regression 等指标。
```

## Q034. 缓存中是否需要 checksum？

**回答：**

是否需要 checksum，要看缓存里放的是什么、错误成本多高、底层系统已经提供多少保护。不是所有缓存都需要业务层 checksum，但关键缓存不应该只靠“Redis 没报错”来证明 value 正确。

checksum 主要能发现几类问题：

```text
字节截断:
  value 写入或读取过程中被截断。

存储或传输损坏:
  网络、磁盘、内存、客户端缓冲区发生错误。

序列化边界错误:
  多段拼接、压缩、加密、解密流程出错。

错误覆盖:
  某些情况下可以发现 payload 和 metadata 不匹配。
```

但 checksum 也有边界：

```text
不能发现业务旧值:
  旧值的 checksum 也可能完全正确。

不能发现权限串用:
  A 用户的数据被放到 B 用户 key 下，checksum 仍然匹配。

不能防恶意篡改:
  攻击者如果能同时改 value 和 checksum，普通 checksum 没用。

不能替代 schema validation:
  字节没坏，不代表字段合法。
```

所以 checksum 通常要和这些字段一起存：

```json
{
  "schema_version": 3,
  "entity_id": "123",
  "tenant_id": "7",
  "version": 42,
  "payload": "...",
  "checksum": "crc32c:..."
}
```

选择算法时，可以按风险分：

```text
CRC32C:
  成本低，适合发现随机损坏。

xxHash / HighwayHash:
  很快，适合非安全校验和分片指纹。

SHA-256:
  成本高一些，适合需要更强碰撞抗性的内容指纹。

HMAC-SHA256:
  带密钥，适合需要防篡改的缓存令牌或边界不可信场景。
```

如果缓存运行在同一可信内网、value 很小、数据可以随时回源、错了也只是展示问题，业务层 checksum 可能不划算。因为每次 get/set 都要多算一次，CPU 成本和延迟会增加。

更适合加 checksum 的场景：

```text
大对象:
  压缩、分片、拼接、跨服务传输链路长。

高价值数据:
  权限、价格、配置、风控规则、模型特征。

多语言写入:
  Java、Go、Python 多个服务都写同一种缓存，序列化风险更高。

离线生成:
  缓存由批处理或异步任务写入，在线服务读取。

跨机房或落盘:
  缓存内容经过复制、持久化、恢复。
```

HTTP 世界里 ETag 更像“版本验证器”而不是普通 checksum。RFC 9110 提到 ETag 可以用内部 revision、内容 hash、文件属性或修改时间生成，强 validator 适合检测 representation 是否变化。业务缓存可以借鉴这个思想：如果目标是验证“内容版本是否一致”，version/ETag 比 checksum 更重要；如果目标是发现“字节是否损坏”，checksum 更直接。

面试里可以这样答：

```text
缓存不一定都要 checksum。普通展示缓存、value 小、可回源、错误成本低时，业务层 checksum 可能不划算。大对象、关键配置、权限、价格、风控规则、多语言写入或跨机房复制的缓存，更适合加 checksum。checksum 能发现字节截断、传输或存储损坏、压缩解压错误，但不能发现业务旧值、权限串用，也不能防止攻击者同时修改 value 和 checksum。设计上通常同时存 schema_version、entity_id、tenant_id、version 和 checksum；随机损坏用 CRC32C，防篡改用 HMAC-SHA256。
```

## Q035. 安全场景下缓存是否可能造成权限泄露？

**回答：**

会，而且这是缓存系统里最容易被低估的问题之一。缓存泄露权限的根因通常不是加密算法错了，而是缓存 key、缓存层级和缓存控制头没有把安全上下文表达完整。

第一类是 key 缺少身份或租户维度：

```text
错误:
  permission:repo_123

应该至少包含:
  tenant_id
  user_id
  resource_id
  action
  auth_context_version
```

如果 key 只有资源 ID，A 用户对 repo_123 的读取结果可能被 B 用户复用。对权限缓存来说，`allow` 和 `deny` 都不能随便跨用户复用。

第二类是缓存个性化响应到共享缓存。MDN 的 Cache-Control 文档区分 private cache 和 shared cache，并提醒 shared cache 会把单份响应复用给多个用户；个性化内容应该使用 `private`，否则可能被共享缓存复用而泄露个人信息。RFC 9111 也规定，`private` 响应指令表示 shared cache 不能存储该响应。

危险响应包括：

```text
用户首页
订单详情
账户余额
权限菜单
带 cookie 登录后的 HTML
带 Authorization 请求得到的 JSON
```

这些响应如果被 CDN、反向代理、网关共享缓存存下来，后果很直接：另一个用户可能拿到前一个用户的数据。

第三类是 HTTP cache key 没有包含 `Vary` 相关维度。比如响应内容按 `Accept-Language`、`Authorization`、cookie、地域、AB 实验变化，但缓存 key 只按 URL：

```text
GET /api/me
```

如果共享缓存忽略了 Authorization 或 Cookie，就会把用户态接口当公共接口缓存。

第四类是 web cache poisoning。OWASP 和 PortSwigger 都有相关说明：攻击者可以利用缓存 key 未覆盖的输入，把恶意响应写进共享缓存，后续正常用户命中这个被污染的响应。对后端业务缓存来说，类似问题也存在：如果用户可控字段参与 value 生成，但没有参与 key 或校验，就可能污染其他用户结果。

第五类是负缓存和权限缓存混用。比如某个用户没有权限：

```text
resource:123 -> no_permission
```

这个 key 少了 user_id。结果可能是所有人都被拒绝；反过来，如果缓存的是 allow，就会产生越权。

治理方式要具体：

```text
key 设计:
  所有影响权限和结果的维度必须进入 key。

缓存分层:
  公共数据和用户私有数据分开缓存池、分开 prefix。

Cache-Control:
  私有响应使用 private 或 no-store，不让共享缓存存。

Vary:
  响应依赖的请求头必须进入缓存变化维度。

权限版本:
  权限变更后递增 auth_context_version，让旧缓存自然失效。

二次校验:
  高风险接口命中缓存后仍做轻量权限校验。

日志:
  记录 cache.hit、cache.key_prefix、tenant_id hash、user_id hash，避免记录明文敏感 key。
```

最重要的一点：安全场景不要把缓存命中当授权结果。缓存可以加速授权判断，但不能降低授权判断的维度。

面试里可以这样答：

```text
缓存完全可能造成权限泄露。常见原因是 key 缺少 tenant、user、resource、action、权限版本等维度，导致一个用户的结果被另一个用户复用；或者把登录后的个性化响应放进 CDN、代理这类 shared cache。HTTP 场景要正确使用 Cache-Control: private/no-store 和 Vary；业务缓存要把安全上下文放进 key，公共缓存和私有缓存分池，权限变更时递增 auth_context_version。高风险数据即使命中缓存，也最好做轻量权限校验。缓存可以加速授权，不能减少授权维度。
```

## Q036. LRU 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

LRU 的核心目标是：在缓存容量不够时，优先淘汰最长时间没有被访问的对象。它假设最近被访问过的数据，短期内更可能再次被访问。这就是 temporal locality。

LRU 主要解决的是性能问题，不是正确性问题，也不是安全性问题。

它提升性能的方式是：

```text
保留最近访问过的对象。
减少后续请求的 miss。
降低数据库、磁盘、远程服务访问次数。
让有限内存服务更多有复用价值的数据。
```

LRU 不保证数据新鲜，也不保证权限正确。一个缓存项即使排在最近端，也可能已经过期、权限变了、数据库更新了。正确性要靠 TTL、版本号、失效事件、权限校验、checksum 这些机制。

LRU 也不直接解决安全性。相反，如果 key 设计错了，LRU 只会更努力地保留热点错误数据。比如某个错误的权限 allow 被大量访问，LRU 会把它留在缓存里。

它对可维护性有一点帮助，因为语义简单：

```text
get 命中:
  移到最近端。

put 新 key:
  插入最近端。

容量超限:
  从最久未访问端淘汰。
```

但可维护性不是 LRU 的主要目标。高并发 LRU 要维护链表、哈希表、锁或分段队列，实现并不总是简单。

面试里可以这样答：

```text
LRU 的核心目标是在容量不足时淘汰最长时间未访问的对象，利用 temporal locality 提高命中率。它主要解决性能问题，通过保留近期访问对象减少 miss 和回源成本。LRU 不解决正确性，最近访问的数据也可能是旧数据；不解决安全性，错误的权限缓存如果是热点反而会被保留；可维护性只是附带收益，因为模型简单。生产上还要配 TTL、版本号、失效事件和权限校验。
```

## Q037. LRU 的典型适用场景和不适用场景分别是什么？

**回答：**

LRU 适合最近性强的 workload。也就是一个对象刚被访问过，短时间内再次访问概率高。

典型适用场景：

```text
用户会话和资料:
  最近活跃用户很可能继续访问。

商品详情:
  热门商品在一段时间内反复被点。

配置和字典:
  最近用过的配置可能被同一服务继续使用。

数据库 page cache:
  查询局部性明显时，最近页容易再次被访问。

编译、模板、正则缓存:
  最近编译过的对象短期内复用概率高。
```

Redis 文档也把 `allkeys-lru` 当作一个常见默认选择，适合预期一部分元素被访问得远多于其他元素的场景。这其实就是长尾热点和 temporal locality 的组合。

不适用场景主要有几类。

第一，scan workload：

```text
后台任务扫全表。
批处理顺序读取大量 key。
导出任务逐个访问冷数据。
```

这些对象可能只访问一次，却会进入最近端，把真正热点挤出去。

第二，稳定长期热点但近期没访问。LRU 只看最近，不看总频率。一个对象过去一小时访问一百万次，如果最近几分钟没访问，也可能被淘汰。

第三，循环访问集合大于缓存容量：

```text
访问顺序:
  A B C D A B C D ...

缓存容量:
  3
```

LRU 会持续淘汰下一次马上要用的对象，命中率可能很差。

第四，对象大小和 miss 成本差异大。LRU 默认把所有 key 当等价对象，一个 10MB value 和一个 100B value 都是一个条目。这样可能保留低价值大对象，淘汰高价值小对象。

第五，热点快速迁移但数据新鲜度强约束。LRU 能适应最近性，但不能处理“最近访问但已经不允许返回”的数据，仍然需要 TTL 和失效。

面试里可以这样答：

```text
LRU 适合有明显 temporal locality 的场景，比如最近活跃用户、商品详情、数据库页、模板和配置缓存。Redis 也把 allkeys-lru 作为常见默认选择，适合一小部分 key 被频繁访问的长尾访问模式。它不适合一次性 scan、循环工作集大于缓存容量、长期高频但短期冷却的对象、对象大小和 miss 成本差异很大的场景。LRU 只看最近性，不看频率、大小、成本和业务新鲜度。
```

## Q038. LRU 和相近概念最容易混淆的边界在哪里？

**回答：**

LRU 最容易和 TTL、LFU、MRU、LRM、cache-aside 里的“最近更新”、以及操作系统里的 page replacement 混在一起。

第一，LRU 不是 TTL。

```text
LRU:
  因为容量不够，淘汰最长时间没访问的对象。

TTL:
  因为时间到了，让对象失效。
```

一个对象可能很新鲜，但因为很久没被访问而被 LRU 淘汰；也可能刚被访问过，但 TTL 到了，不应该继续返回。

第二，LRU 不是 LFU。

```text
LRU:
  看最后一次访问时间。

LFU:
  看访问频率。
```

LRU 对热点迁移敏感，能快速接纳最近热点；LFU 对稳定热点好，但容易有历史包袱，所以需要 decay。

第三，LRU 不是 MRU。

```text
LRU:
  淘汰最久没访问的对象。

MRU:
  淘汰最近访问的对象。
```

MRU 在某些循环扫描场景反而可能有用，但它不是通用默认策略。

第四，LRU 不是 LRM。Redis 新文档里区分了 LRU 和 LRM：LRU 在读写时更新访问时间，LRM 只在写操作时更新最近修改时间。LRM 更关心“多久没被改”，不是“多久没被读”。

第五，LRU 不是 cache-aside。cache-aside 是读写模式：

```text
read miss -> load from DB -> set cache
write -> update DB -> delete cache
```

LRU 是容量超限时的淘汰策略。一个系统可以同时使用 cache-aside 和 LRU。

第六，工程里的 LRU 可能不是精确 LRU。Redis 文档明确说 Redis LRU 使用近似算法，随机采样一小部分 key，再淘汰其中最久未访问的 key。这样节省内存和 CPU，但语义上不是理论上的全局精确 LRU。

面试里可以这样答：

```text
LRU 的边界在于它只处理容量淘汰，并且只看最近访问时间。它不是 TTL，TTL 处理时间失效；不是 LFU，LFU 看访问频率；不是 LRM，LRM 看最近修改；也不是 cache-aside，cache-aside 是读写模式。生产实现还要区分精确 LRU 和近似 LRU，Redis 为了节省内存和 CPU 就用采样近似 LRU。LRU 决定放不下时删谁，不决定数据是否新鲜、是否有权限、是否该回源。
```

## Q039. LRU 在高并发场景下可能出现哪些隐藏问题？

**回答：**

LRU 在单线程里很简单：哈希表查 key，双向链表移动节点。高并发后，最麻烦的不是算法大 O，而是每次命中都要更新元数据。

第一个问题是锁竞争。命中读也不是纯读，因为要把节点移到最近端：

```text
get(key):
  map lookup
  move node to head
```

如果所有线程都要抢同一把锁，热点 key 越热，锁竞争越严重。读多写少的缓存反而会出现大量写元数据。

第二个问题是热点 key 放大锁竞争。多个线程反复访问同一个 key，每次都尝试更新 recency。真正的 value 读取很快，时间花在锁、CAS retry、队列维护上。

第三个问题是分段 LRU 的语义偏差。为了降低锁竞争，很多实现会把缓存分成多个 shard：

```text
shard = hash(key) % N
每个 shard 一个局部 LRU
```

这样吞吐提高，但淘汰不再是全局最久未访问。某个 shard 可能满了并淘汰热点，而另一个 shard 还有空间。

第四个问题是异步维护导致顺序不精确。高性能缓存可能把读事件先写入 buffer，再由后台线程批量更新 LRU 队列。这样 recency 是近似的，不是每次 get 都立刻反映到队列里。

第五个问题是内存和 GC。精确 LRU 通常需要：

```text
hash entry
linked node
prev/next pointer
key reference
value reference
metadata
```

大量小对象时，元数据可能比 value 还大。Java/Go 这类运行时还会有 GC 压力。

第六个问题是 eviction callback 或 loader 在锁内执行。这个很危险：

```text
持锁 -> 淘汰 -> 调用回调 -> 回调访问缓存或下游
```

可能造成死锁、长时间阻塞或重入问题。淘汰回调最好在锁外执行。

第七个问题是高并发 miss 下的 stampede。LRU 只决定淘汰，不合并加载。同一个 key 被淘汰后，大量请求同时 miss，如果没有 singleflight，会一起回源。

面试里可以这样答：

```text
LRU 高并发的隐藏问题主要来自命中也要更新 recency 元数据。读请求会变成链表移动，导致锁竞争、CAS retry 和热点 key 争用。分段 LRU 能提高吞吐，但语义变成局部 LRU，不再是全局最久未访问；异步维护能降成本，但 recency 会近似。精确 LRU 的节点、指针和元数据也会带来内存和 GC 压力。实现时还要避免在锁内执行 eviction callback 或 loader，并用 singleflight 处理被淘汰后的热点 miss。
```

## Q040. LRU 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

LRU 的状态主要是缓存内容和访问顺序。崩溃、重启、超时和重试会打乱这两个东西。

崩溃和重启时，内存 LRU 通常全部丢失：

```text
before restart:
  热点 key 已经 warm。

after restart:
  缓存为空，访问顺序也为空。
```

结果是冷启动 miss 暴涨。多个实例同时重启时，会形成缓存雪崩。需要预热、限流、singleflight、stale cache 或分批发布来保护后端。

如果 LRU 有持久化，问题会不同。缓存内容可能恢复了，但访问顺序是否恢复、TTL 是否过期、版本是否仍然有效，都要重新判断。不能因为 key 从磁盘加载出来，就认为它仍然新鲜。

超时场景里，最常见的问题是 loader 晚到：

```text
1. 请求 A miss，开始加载 v1。
2. A 超时，调用方放弃。
3. 请求 B 更新数据为 v2，并写入缓存。
4. A 的 loader 晚到，把 v1 写入缓存。
```

这不是 LRU 独有，但 LRU 会把晚到的旧值放到最近端，后续更容易命中它。解决方式是版本号和 compare-and-set，旧版本不能覆盖新版本。

重试会带来重复写和访问顺序污染：

```text
同一个请求超时后重试 3 次。
每次都访问同一批 key。
LRU 认为这些 key 很新。
```

如果重试来自异常流量，LRU 可能把异常请求的 key 推到最近端，挤掉正常热点。

删除和淘汰也有边界。比如一个 key 被淘汰后，in-flight 请求仍然持有旧 value 引用；另一个请求重新加载了新 value。此时要确保：

```text
旧引用不会被重新 put 回缓存。
eviction callback 不会删除新版本资源。
重试不会复活已删除对象。
```

分布式环境还有节点重启后的热点迁移问题。一个 Redis shard 重启、一个应用实例重启，局部 LRU 状态都丢了。流量如果立刻打满新节点，会造成局部冷启动。

面试里可以这样答：

```text
LRU 在崩溃和重启时会丢失缓存内容和访问顺序，导致冷启动 miss 暴涨；多个实例同时重启会放大成雪崩。超时时要防止 loader 晚到把旧值写回缓存，重试会重复访问并污染 recency。持久化缓存恢复后也不能直接信任，需要检查 TTL、版本和 schema。被淘汰的旧 value 如果还有 in-flight 引用，也不能让回调或重试删掉新版本。常见保护是分批重启、预热、singleflight、限流、版本 CAS 和 stale 降级。
```

## Q041. LRU 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

单机内存 LRU 的瓶颈通常来自锁竞争、CPU 和内存，不是 I/O 或网络。分布式缓存里的 LRU 则会叠加网络和服务端排队。

在进程内 LRU 里，每次 get 命中都可能做：

```text
hash lookup
metadata update
linked list remove
linked list insert at head
stats update
```

如果实现是全局锁，瓶颈首先是锁竞争。并发越高、热点越集中，越明显。Caffeine 的 Benchmark wiki 里也把同 key 和 Zipf 分布作为场景，用来展示锁和并发结构的影响。

CPU 瓶颈主要来自：

```text
hash 计算
指针操作
CAS retry
队列维护
频繁统计计数
对象序列化
```

内存瓶颈来自：

```text
每个 entry 的元数据。
双向链表节点。
对象头和指针。
allocator 碎片。
GC 扫描和回收。
```

小 value 场景下，元数据和 GC 可能比业务数据更贵。比如缓存 100 字节 value，却为每个 entry 付出几十字节甚至更多元数据。

I/O 通常不是 LRU 算法本身的瓶颈，但 miss 后回源会产生 I/O。这个时候瓶颈在 loader，不在 LRU：

```text
cache hit:
  LRU 元数据维护成本。

cache miss:
  数据库、磁盘、RPC、对象存储成本。
```

网络也是类似。进程内 LRU 没有网络；Redis 这类远程缓存会有网络，但那是缓存系统部署方式带来的，不是 LRU 策略本身带来的。Redis 的近似 LRU 还会有采样成本，`maxmemory-samples` 增大时更接近真实 LRU，但会增加 CPU。

面试里可以这样答：

```text
进程内 LRU 的瓶颈通常来自锁竞争、CPU 和内存。命中也要更新 recency，可能移动链表节点、更新队列和统计，所以高并发热点 key 会造成锁竞争或 CAS retry；大量小对象时，节点、指针、对象头和 GC 成本会很明显。I/O 主要出现在 miss 后回源，不是 LRU 算法本身；网络主要出现在 Redis 这类远程缓存部署里。Redis 近似 LRU 通过采样降低内存和 CPU，样本数调高会更接近真实 LRU，但 CPU 成本也会上升。
```

## Q042. LRU 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

这三类测试目的不同。correctness test 验证语义对不对；stress test 找并发和边界问题；benchmark 衡量性能和成本。

correctness test 应该测确定性行为：

```text
put/get:
  put 后能 get 到。

capacity:
  超过容量时只淘汰一个或必要数量的对象。

recency update:
  get 命中后，该 key 变成最近使用。

overwrite:
  put 已存在 key 时更新 value，并更新最近性。

delete:
  删除后不能再命中，链表和 map 都清理。

eviction order:
  构造 A B C，访问 A，再插入 D，应淘汰 B。

zero/one capacity:
  容量 0、容量 1 的行为明确。
```

还要测不变量：

```text
map.size == list.size
链表无环
每个节点 prev/next 一致
每个 key 只出现一次
tail 一定是最久未访问对象
```

stress test 关注并发和异常：

```text
多线程 get/put/delete 同一批 key。
热点 key 反复访问。
容量很小，持续淘汰。
eviction callback 慢或抛异常。
loader 超时、失败、重试。
并发 clear 和 get/put。
长时间运行，检查内存泄漏。
```

如果语言支持 race detector 或线程 sanitizer，要打开。LRU 链表指针错一次，可能不是立刻崩，而是几小时后出现循环、丢节点或 size 不一致。

benchmark 关注可量化成本：

```text
read hit ns/op
read miss + insert ns/op
write/update ns/op
eviction ns/op
allocs/op
B/op
ops/s under threads
p50/p95/p99 latency
lock wait / contention
GC pause
```

workload 要覆盖：

```text
100% read hit
read/write mixed
Zipf 长尾
same hot key
uniform random
scan pollution
phase shift
```

correctness test 不需要追求吞吐；benchmark 不应该用小样例证明语义正确；stress test 不应该只报告 ops/s。三者要分开。

面试里可以这样答：

```text
LRU 的 correctness test 要验证 put/get、容量淘汰、get 后更新最近性、覆盖、删除、容量 0/1、确定的淘汰顺序，并检查 map size 等于链表 size、链表无环、key 不重复、tail 是最久未访问。stress test 要多线程反复 get/put/delete、热点 key、持续淘汰、慢回调、loader 超时和 clear 并发，配合 race detector 找数据竞争。benchmark 则测 read hit、miss insert、eviction 的 ns/op、allocs/op、p99、锁竞争和 GC，并覆盖 Zipf、scan、phase shift、mixed workload。
```

## Q043. 如果要求从零实现一个简化版 LRU，你会先定义哪些不变量？

**回答：**

从零实现 LRU，先不要急着写代码。先定义不变量。LRU 的 bug 多数不是哈希表查不到，而是链表、map、容量和回调之间状态不一致。

核心数据结构通常是：

```text
map[key] -> node

node:
  key
  value
  prev
  next

list:
  head = 最近使用
  tail = 最久未使用
```

先定义这些不变量：

```text
唯一性:
  每个 key 在 map 中最多出现一次，在链表中也最多出现一次。

一致性:
  key 在 map 中存在，当且仅当对应 node 在链表中存在。

数量一致:
  map.size == list.size。

容量约束:
  size <= capacity。

顺序语义:
  head 是最近访问或写入的节点。
  tail 是最久未访问的节点。

指针正确:
  对任意 node，node.next.prev == node，node.prev.next == node。
  head.prev == nil，tail.next == nil。

空表正确:
  size == 0 时 head == nil 且 tail == nil。

单元素正确:
  size == 1 时 head == tail。
```

操作语义也要写清楚：

```text
get existing:
  返回 value，并把 node 移到 head。

get missing:
  返回 miss，不改变缓存。

put new:
  插入 head；如果超容量，淘汰 tail。

put existing:
  更新 value，并把 node 移到 head。

delete:
  从 map 和链表同时删除。

evict:
  只淘汰 tail，且回调拿到被淘汰的 key/value。
```

如果支持权重容量，不变量要改成：

```text
total_weight <= max_weight
node.weight >= 0
total_weight == sum(node.weight)
```

如果支持并发，还要定义锁不变量：

```text
所有 map 和 list 修改必须在同一把锁或同一 shard 锁下完成。
不能在持锁时调用用户回调。
loader 不能在持锁时执行。
```

如果支持 TTL，LRU 和过期要分开：

```text
expired item 不能作为命中返回。
过期删除也必须同时删 map 和 list。
LRU 淘汰和 TTL 删除都要维护相同结构不变量。
```

面试里可以这样答：

```text
我会先定义 map 和双向链表的一致性不变量：每个 key 只出现一次；map 中存在等价于链表中存在；map.size 等于 list.size；size 不超过 capacity；head 是最近使用，tail 是最久未使用；链表 prev/next 双向一致，空表和单元素状态明确。操作上，get 命中要移到 head，miss 不改状态；put 新 key 插入 head 并在超容量时淘汰 tail；put 旧 key 更新 value 并移到 head；delete 同时删 map 和链表。并发实现还要规定所有结构修改在锁内完成，回调和 loader 不能在锁内执行。
```

## Q044. LRU 的常见误用是什么，误用后通常会产生什么线上症状？

**回答：**

LRU 常见误用可以分成策略误用和实现误用。

第一，把 LRU 当成 freshness 机制。很多人以为“最近访问过”就代表“数据还新”。这是错的。LRU 只说明访问时间，不说明数据库有没有更新、权限有没有变化。

线上症状：

```text
hit rate 很高，但用户看到旧价格、旧权限、旧配置。
更新数据库后，缓存长时间不变。
投诉集中在修改后马上读取的场景。
```

第二，在 scan workload 上使用纯 LRU。后台导出、全量同步、批处理扫描会把大量只访问一次的冷 key 放到最近端，挤掉真正热点。

线上症状：

```text
每天某个批任务一跑，线上接口 miss 暴涨。
数据库 QPS 在批任务期间升高。
批任务结束后缓存需要一段时间恢复。
```

第三，缓存 key 缺少维度。LRU 会保留热点 key，如果这个 key 本身错误，就会让错误结果更稳定。

线上症状：

```text
用户串数据。
权限偶发越权或误拒。
不同语言、地区、租户看到同一份结果。
```

第四，用 entry count 控制容量，但对象大小差异大。

线上症状：

```text
缓存条目数没满，内存先爆。
大对象频繁进出，GC 和网络抖动。
小热点被大冷对象挤掉。
```

第五，没有处理并发 miss。LRU 淘汰了热点 key 后，所有请求一起回源。

线上症状：

```text
热点 key 过期或被淘汰瞬间，数据库 QPS 尖峰。
接口 p99 飙升。
重试风暴。
```

第六，自己实现 LRU 但在锁内执行回调或加载。

线上症状：

```text
偶发全局卡顿。
线程堆栈停在 eviction callback。
死锁或缓存操作长时间阻塞。
```

第七，在多实例里误以为每个本地 LRU 加起来就是全局 LRU。实际上每个实例只知道自己的访问顺序。

线上症状：

```text
扩容后命中率下降。
不同实例缓存内容差异很大。
发布重启后局部冷启动明显。
```

面试里可以这样答：

```text
LRU 常见误用包括把最近访问当成数据新鲜、在 scan workload 上直接用纯 LRU、key 缺少租户和权限维度、用条目数控制大小差异很大的对象、没有 singleflight 处理热点 miss、锁内执行回调或 loader，以及把多实例本地 LRU 当成全局 LRU。线上症状通常是 hit rate 高但数据旧、批任务期间 miss 和 DB QPS 暴涨、用户串数据、GC 和内存抖动、热点 key 被淘汰后 stampede、缓存操作偶发卡死、扩容后命中率反而下降。
```

## Q045. LRU 在单机和分布式环境中的语义有什么差异？

**回答：**

单机 LRU 的语义可以比较清楚：一个进程或一个缓存实例维护一套访问顺序，容量超限时淘汰这套顺序里的 tail。

```text
single cache:
  所有访问都进入同一个 LRU 队列。
  tail 更接近全局最久未访问对象。
```

分布式环境里，语义会变弱。

第一，多实例本地缓存不是全局 LRU：

```text
instance A:
  最近访问 user:1。

instance B:
  从没访问 user:1。
```

A 的本地 LRU 和 B 的本地 LRU 完全不同。负载均衡、扩容、缩容都会改变每个实例看到的访问序列。

第二，Redis Cluster 这类分片缓存通常是每个 shard 局部淘汰。key 根据哈希槽落到某个 shard，LRU 只在该 shard 的候选集合里做。

```text
shard 1:
  内存满，淘汰自己的 tail。

shard 2:
  可能还有空闲内存。
```

所以分布式 LRU 不是“所有 key 的全局最久未使用”。它更像多个局部 LRU 的组合。

第三，近似实现会进一步削弱语义。Redis LRU 不是精确维护全局链表，而是随机采样若干 key，淘汰样本中最久未访问的 key。这样节省内存和 CPU，但结果是概率性的。

第四，访问路径影响 recency。如果某些请求命中本地缓存，就不会访问 Redis，那么 Redis 不知道这些 key 在应用层仍然很热。

```text
local cache hit:
  Redis recency 不更新。

local cache miss -> Redis hit:
  Redis recency 才更新。
```

这会让下层缓存的 LRU 看到的是上层 miss 流量，而不是原始用户流量。

第五，复制和故障转移会影响状态。主从切换后，LRU 元数据是否完整、访问时间是否一致、近似计数是否保留，取决于具体实现。

设计时要接受这个现实：分布式缓存里的 LRU 往往是局部、近似、受路由影响的策略。不要把它当强语义协议。

面试里可以这样答：

```text
单机 LRU 通常维护一套访问顺序，容量超限时淘汰这套队列里最久未访问的 key。分布式环境里语义会变成局部和近似：每个应用实例的本地 LRU 只看到自己的流量；Redis Cluster 每个 shard 只在本 shard 内淘汰；Redis LRU 还是采样近似，不是全局精确链表。多级缓存还会让下层只看到上层 miss 流量，本地命中不会更新 Redis recency。所以分布式 LRU 不能理解为全局最久未使用，只能理解为按节点、分片和实现近似收敛的淘汰策略。
```

## Q046. LFU 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

LFU 的核心目标是：在容量有限时，尽量保留访问频率高的对象，淘汰访问频率低的对象。它利用的是 frequency locality，也就是一段时间内有些对象明显比其他对象更常被访问。

LFU 主要解决性能问题。它想减少 miss，降低后端压力，提高缓存容量利用率。它不直接解决正确性、安全性或可维护性。

和 LRU 相比，LFU 的判断标准不同：

```text
LRU:
  最近有没有访问？

LFU:
  访问了多少次？
```

这让 LFU 更适合稳定热点。例如某些热门商品、热门用户、热门配置在很长一段时间内频繁访问，即使短时间没被访问，也不该轻易被一次性 scan 挤掉。

Redis 文档对 LFU 的描述也符合这个目标：LFU 模式会尝试跟踪 item 的访问频率，让很少使用的 key 被淘汰，经常使用的 key 更可能留在内存中。

但 LFU 不保证正确性。高频对象也可能过期，也可能权限变了，也可能 value 损坏。正确性仍然靠 TTL、版本、失效、校验。

LFU 也不保证安全性。一个错误的 allow 权限如果被大量访问，LFU 会把它当高频热点保护起来。

可维护性方面，LFU 通常比 LRU 更复杂。精确 LFU 需要维护频率桶、每个频率下的节点集合、min frequency；近似 LFU 需要计数器、衰减和采样。Redis 的 LFU 就使用概率计数器和 decay，而不是无限增长的精确计数。

面试里可以这样答：

```text
LFU 的核心目标是在容量不足时保留访问频率高的对象，淘汰访问频率低的对象。它主要解决性能问题，适合稳定热点，能减少一次性 scan 对热点的污染。它不解决正确性，高频数据也可能过期或变旧；不解决安全性，错误权限如果高频访问反而会被保留；可维护性通常比 LRU 更差，因为要维护频率、衰减和淘汰候选。生产实现常用近似 LFU，而不是无限精确计数。
```

## Q047. LFU 的典型适用场景和不适用场景分别是什么？

**回答：**

LFU 适合热点稳定、访问频率差异明显的场景。也就是少数 key 长时间占据大部分访问量。

典型适用场景：

```text
热门商品详情:
  一段时间内头部商品反复访问。

热门内容:
  新闻、视频、帖子、榜单中的头部内容。

配置和字典:
  少量配置被大量服务反复读。

读多写少的公共数据:
  地区表、分类树、静态规则。

scan 干扰明显的系统:
  LFU 不容易让一次性访问对象挤掉高频热点。
```

Redis 的 `allkeys-lfu` 就适合希望按访问频率保留 key 的场景。它不是精确 LFU，而是近似 LFU，加了 decay，让历史热点能逐渐降级。

不适用场景也很重要。

第一，热点快速迁移：

```text
上午热点是 A。
下午热点是 B。
晚上热点是 C。
```

如果 LFU 衰减太慢，旧热点频率很高，新热点很难进来。用户会看到缓存对新热点反应慢。

第二，强 recency workload。比如用户刚打开某个工作空间，接下来几分钟会密集访问这个工作空间的数据。LRU 可能比 LFU 更快适应。

第三，循环扫描和全量遍历。如果扫描对象也会增加频率，长时间扫描可能仍然污染 LFU，尤其是精确计数不衰减时。

第四，对象大小和成本差异大。纯 LFU 只看访问次数，一个被访问 100 次的 10MB 对象，未必比被访问 80 次的 1KB 对象更值得缓存。

第五，写频繁或删除频繁的场景。频率元数据维护成本高，且旧频率在对象重建后是否继承，需要明确定义。

面试里可以这样答：

```text
LFU 适合热点稳定、频率差异明显、读多写少的场景，比如热门商品、热门内容、公共配置、字典表，以及有 scan 干扰但头部热点长期稳定的系统。它不适合热点快速迁移、强最近性场景、对象大小和 miss 成本差异很大的缓存，以及写删频繁的场景。LFU 如果没有 decay，会有历史包袱；decay 太快又会退化得接近 LRU 或随机。选 LFU 前最好用 trace 看热点稳定性和频率分布。
```

## Q048. LFU 和相近概念最容易混淆的边界在哪里？

**回答：**

LFU 最容易和 LRU、TinyLFU、精确计数、热门排行榜、TTL 混淆。

第一，LFU 不是 LRU：

```text
LFU:
  按访问频率判断价值。

LRU:
  按最近访问时间判断价值。
```

一个 key 最近刚访问 1 次，LRU 会认为它很新；LFU 可能认为它频率低，不值得保留。一个 key 过去访问很多次但最近没访问，LFU 可能保留，LRU 可能淘汰。

第二，LFU 不等于“访问次数越多越永远保留”。生产 LFU 通常要有 decay。Redis 文档也说明 Redis LFU 用概率计数器加 decay，避免过去很热但现在不热的 key 永久占据缓存。

第三，LFU 不等于 TinyLFU。TinyLFU 更偏 admission policy。它用近似频率判断“新对象是否值得进入缓存”，可以和 LRU、SLRU 等淘汰结构组合。LFU 则通常指缓存内对象按频率淘汰。

第四，LFU 不等于业务排行榜。排行榜统计的是业务事件，例如播放量、点赞数、订单量；LFU 统计的是缓存访问频率。某个视频业务热度高，不代表这个缓存 key 被频繁访问；反过来也一样。

第五，LFU 不解决 TTL。访问频率高的 key 也可能过期。TTL 是新鲜度控制，LFU 是容量控制。

第六，精确 LFU 和近似 LFU不同。精确 LFU 可以维护严格频率顺序，但成本高；近似 LFU 使用有限计数器、采样、衰减，结果是概率性判断。Redis 的 LFU 就是近似。

面试里可以这样答：

```text
LFU 的边界是它按访问频率做容量淘汰，不按最近访问、不按业务热度、不按 TTL。它和 LRU 的区别是 frequency vs recency；和 TinyLFU 的区别是 LFU 通常是淘汰策略，TinyLFU 更常作为准入策略；和排行榜的区别是 LFU 统计缓存访问，不统计业务事件；和 TTL 的区别是 LFU 解决容量，TTL 解决新鲜度。生产里的 LFU 还要区分精确和近似，Redis LFU 用概率计数器和 decay，不是无限精确计数。
```

## Q049. LFU 在高并发场景下可能出现哪些隐藏问题？

**回答：**

LFU 在高并发下的主要问题是：每次访问都可能更新频率元数据。频率更新比 LRU 的 recency 更新还容易变成热点，因为高频 key 的计数器会被大量线程同时修改。

第一个问题是计数器争用：

```text
hot key:
  1000 个线程同时 get。
  每个线程都想增加 frequency。
```

如果是精确计数，原子加、锁或 CAS retry 都会很重。近似 LFU 可以通过概率增加降低更新频率，但会牺牲精度。

第二个问题是频率桶维护成本。精确 O(1) LFU 通常需要：

```text
key -> node
freq -> linked list of nodes
minFreq
```

每次 hit 都要把 node 从旧 freq bucket 移到新 freq bucket。高并发下，这比简单 map lookup 复杂得多。

第三个问题是全局 minFreq 竞争。淘汰时要知道当前最低频率。并发 put、get、evict 同时发生时，minFreq 很容易维护错。

第四个问题是频率饱和。计数器如果无限增长，会溢出或让历史热点永久留存；如果有限计数器饱和，很多 key 都到最大值，就分不出谁更热。Redis LFU 用 0 到 255 的概率计数器，并通过 `lfu-log-factor` 控制增长速度，就是为了处理这个问题。

第五个问题是 decay 的并发成本。频率要衰减，才能适应热点迁移。但衰减是全局定时扫、访问时懒衰减，还是采样衰减？不同做法都会影响并发：

```text
全局扫描:
  CPU 尖峰。

访问时衰减:
  get 路径变重。

采样衰减:
  语义近似，调参复杂。
```

第六个问题是高频攻击。攻击者可以反复访问一批 key，把它们频率刷高，挤掉正常数据。LRU 也会受热点攻击影响，但 LFU 的历史频率可能让污染持续更久。

第七个问题是统计和淘汰不一致。为了性能，很多实现会异步记录访问，再批量更新频率。这样频率不再严格实时，淘汰可能基于滞后的统计。

面试里可以这样答：

```text
LFU 高并发的隐藏问题主要是频率元数据更新成本。热点 key 会让计数器、频率桶和 minFreq 产生争用；精确 LFU 每次 hit 可能要移动频率桶，锁和 CAS 成本高。计数器还会饱和或溢出，需要 decay；decay 如果全局扫描会有 CPU 尖峰，访问时懒衰减会加重 get 路径。近似 LFU 能降低成本，但语义变成概率性的。LFU 还容易被高频访问刷榜污染，旧热点如果衰减慢，会长期占据缓存。
```

## Q050. LFU 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

LFU 的状态比 LRU 更复杂。它不只保存缓存内容，还保存频率、衰减时间、minFreq、桶结构或近似计数器。崩溃、重启、超时、重试会影响这些元数据。

崩溃和重启后，如果 LFU 是纯内存的，频率全部丢失：

```text
before restart:
  hot_key frequency = 100000

after restart:
  hot_key frequency = initial value
```

这会让缓存短时间内分不清长期热点和普通 key。冷启动期间，LFU 的优势发挥不出来，需要预热或从历史访问日志恢复频率。

如果频率被持久化，也有问题：

```text
旧频率是否仍然代表当前热点？
重启花了很久，频率是否应该衰减？
恢复的 value 是否过期？
版本是否仍然有效？
```

不能只恢复 frequency，不检查 TTL、version、schema。

超时会带来旧值写回问题。和 LRU 一样，loader 晚到可能把旧值写进缓存；LFU 额外的问题是旧值可能继承或获得较高频率：

```text
1. 请求 A 加载旧值 v1，超时。
2. 请求 B 写入新值 v2。
3. A 晚到，把 v1 写入缓存。
4. v1 因为重试多次被访问，frequency 变高。
5. v1 更难被淘汰。
```

重试会污染频率。一个失败请求重试多次，LFU 会把它当作多次真实访问：

```text
真实用户意图:
  访问 1 次。

系统观察:
  timeout + retry = 4 次访问。

LFU 结果:
  frequency 增加 4 次。
```

如果某个下游慢，重试集中在一批 key 上，这批 key 的频率会被异常放大。

删除和重建也有边界。一个 key 被删除后又创建，是否继承旧频率？

```text
product:123 version=10 很热。
product:123 被删除。
product:123 version=11 被重新创建。
```

如果直接按 key 继承频率，新对象会带着旧对象热度；如果完全不继承，真实热点恢复慢。更稳的是把 version 纳入缓存 value 或 key，在业务上明确是否继承。

近似 LFU 还有 decay 时钟问题。Redis LFU 有 decay time；如果进程暂停、系统时间变化、节点长期不可用，恢复后计数衰减可能和预期不一致。分布式节点之间频率也不会天然一致。

面试里可以这样答：

```text
LFU 在崩溃重启时会丢失或恢复频率元数据。频率丢失会让长期热点短期内和普通 key 一样；频率持久化又可能把旧热点带回来，所以恢复时必须同时检查 TTL、version、schema 和 decay。超时和 loader 晚到会把旧值写回缓存，重试会把一次真实访问放大成多次频率增加，让错误或异常 key 更难淘汰。删除后重建还要决定是否继承旧频率，通常需要把 version 纳入 key 或 value。近似 LFU 还要注意 decay 时钟和节点间频率不一致。
```

## Q051. LFU 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

LFU 的主要瓶颈通常来自 CPU、内存和锁竞争。I/O 和网络也可能出现，但那更多是缓存 miss 后回源、或者把 LFU 放在 Redis 这类远程缓存里带来的成本，不是 LFU 策略本身最核心的成本。

LFU 比 LRU 更重的地方在于：每次访问都要处理频率。

```text
LRU 命中:
  更新最近性。

LFU 命中:
  更新频率。
  可能移动频率桶。
  可能维护 minFreq。
  可能触发衰减或概率计数。
```

如果是精确 LFU，常见结构是：

```text
key -> node
freq -> linked list / ordered set
minFreq
```

命中一个 key 后，节点要从旧频率桶移动到新频率桶。这个动作在单线程里不难，但高并发下会造成锁竞争。热点 key 越热，计数器和频率桶越容易成为争用点。

CPU 开销主要来自：

```text
计数器更新:
  原子加、CAS retry、概率增长、衰减计算。

频率桶维护:
  从旧 bucket 删除，插入新 bucket。

淘汰候选选择:
  找最低频率，并在同频率内做 tie-break。

统计和采样:
  近似 LFU 常用采样、概率计数、sketch。

序列化:
  如果 LFU 在远程缓存前后还要处理 value 编解码，CPU 会更高。
```

Redis 的 LFU 就没有使用无限增长的精确计数。Redis 文档说明它使用一个概率计数器，并通过 `lfu-log-factor` 控制增长速度，通过 `lfu-decay-time` 控制计数衰减。这说明生产系统往往愿意牺牲一点精确性，换取更低的内存和 CPU 成本。

内存开销来自元数据：

```text
每个 key 的频率计数。
频率桶或链表节点。
时间戳或 decay 信息。
近似算法里的 sketch / doorkeeper。
淘汰统计、访问统计。
```

如果 value 很小，比如几十字节，LFU 元数据可能占总内存的很大比例。这个时候 LFU 命中率高一点，也未必抵得过元数据成本。

锁竞争在高并发里很常见：

```text
同一个 hot key:
  多个线程同时更新同一个频率计数。

同一个 freq bucket:
  大量节点从 freq=1 移到 freq=2。

全局 minFreq:
  put、get、evict 都要维护最低频率。

decay:
  全局衰减或批量重置会带来同步压力。
```

I/O 和网络一般出现在两种情况：

```text
cache miss:
  LFU 没命中，回源数据库、磁盘、RPC。

remote cache:
  访问 Redis/Memcached，本身有网络、连接池、服务端排队。
```

所以判断瓶颈时要分层看：

```text
进程内 LFU:
  重点看 CPU、内存、锁、GC。

Redis LFU:
  还要看网络、Redis command latency、主线程 CPU、内存碎片。

miss path:
  看数据库、对象存储、RPC、loader 并发。
```

面试里可以这样答：

```text
LFU 的策略瓶颈主要来自 CPU、内存和锁竞争。每次命中都要更新频率，精确 LFU 还要移动频率桶、维护 minFreq，同一个 hot key 或低频桶会产生争用。内存上要保存计数器、桶、节点、衰减信息或 sketch，小对象场景里元数据成本很明显。Redis 这类生产实现用概率计数器和 decay，就是为了降低精确 LFU 的 CPU 和内存成本。I/O 和网络通常来自 miss 回源或远程缓存部署，不是 LFU 算法本身的第一瓶颈。
```

## Q052. LFU 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

LFU 的测试也要分三类。correctness test 测语义，stress test 测并发和边界，benchmark 测性能和收益。不要用 benchmark 的高命中率替代正确性测试。

correctness test 先测基本语义：

```text
put/get:
  写入后能读到。

frequency increment:
  get 命中后频率增加。

miss:
  miss 是否增加历史频率，要按策略定义。

eviction:
  容量满时淘汰最低频率 key。

tie-break:
  多个 key 频率相同，按 LRU、插入顺序或其他规则淘汰。

overwrite:
  更新已有 key 时，频率是否保留、重置或增加，要定义清楚。

delete:
  删除后 map、freq bucket、minFreq 都要更新。

capacity 0/1:
  极小容量行为必须确定。
```

如果实现有 decay，还要测：

```text
计数会随时间衰减。
衰减后 minFreq 正确。
旧热点不会永久占据缓存。
衰减不会让频率变成负数或非法值。
```

如果是近似 LFU，要把 correctness 目标写现实一点。近似 LFU 不能要求“每次都淘汰全局最低真实频率 key”，只能测试：

```text
计数器范围合法。
计数随访问概率性增加。
decay 后计数下降。
高频 key 在统计意义上比低频 key 更容易留下。
```

stress test 重点找并发错误：

```text
多线程同时访问同一个 hot key。
大量 key 从 freq=1 升到 freq=2。
get、put、delete、evict 并发。
decay 和访问并发。
eviction callback 慢或抛异常。
loader timeout 后重试。
长时间运行检查内存泄漏。
```

LFU 很容易出现这些 bug：

```text
minFreq 变成不存在的频率。
节点同时存在于两个频率桶。
map 里有 key，但 bucket 里没有节点。
频率桶为空但没被删除。
并发 delete 后 get 又把旧节点移回来了。
```

benchmark 要同时看命中质量和实现成本：

```text
命中质量:
  hit rate、byte hit rate、cost-weighted miss rate。

实现成本:
  ns/op、ops/s、allocs/op、B/op、p50/p95/p99。

并发成本:
  lock wait、CAS retry、hot key contention。

内存成本:
  metadata bytes per entry、GC、fragmentation。
```

workload 至少覆盖：

```text
Zipf:
  稳定长尾热点。

scan:
  一次性扫描污染。

phase shift:
  热点从 A 集合切到 B 集合。

uniform:
  没有局部性，观察 LFU 是否浪费元数据。

same hot key:
  测计数器争用。

mixed read/write:
  测更新、删除、频率继承。
```

面试里可以这样答：

```text
LFU 的 correctness test 要测频率增加、最低频率淘汰、同频 tie-break、覆盖、删除、minFreq、容量 0/1，以及 decay 后频率和淘汰顺序是否正确。近似 LFU 不能要求严格全局最低频率，只能验证计数范围、概率增长、衰减和统计倾向。stress test 要并发 get/put/delete/evict、hot key、频率桶迁移、decay 并发和慢回调，重点抓 minFreq 错、节点重复、bucket 泄漏。benchmark 要同时看 hit rate、byte hit rate、miss cost、ns/op、allocs、p99、锁竞争和元数据内存。
```

## Q053. 如果要求从零实现一个简化版 LFU，你会先定义哪些不变量？

**回答：**

从零实现 LFU，先定义频率语义。很多 LFU bug 的根源不是代码写错一行，而是没有说明“频率什么时候增加、同频怎么淘汰、更新是否继承频率、衰减怎么做”。

一个简化版精确 LFU 通常有这些结构：

```text
map[key] -> node
freq_map[freq] -> ordered list of nodes
min_freq
capacity
size

node:
  key
  value
  freq
  prev / next
```

核心不变量：

```text
唯一性:
  每个 key 在 map 中最多出现一次。
  每个 node 只属于一个 freq bucket。

一致性:
  key 在 map 中存在，当且仅当 node 在某个 freq bucket 中存在。

数量一致:
  size == map.size == 所有 freq bucket 节点数之和。

容量约束:
  size <= capacity。

频率合法:
  node.freq >= 1。
  node 位于 freq_map[node.freq]。

min_freq 正确:
  min_freq 是当前所有 node.freq 的最小值。
  如果 size == 0，min_freq 为空或 0。

同频顺序:
  同一个 freq bucket 内按 LRU 或插入顺序排列。
  淘汰最低频率里的最旧节点。
```

操作语义也要定清楚：

```text
get hit:
  返回 value。
  node 从 freq=f 移到 freq=f+1。
  在新 bucket 中成为最近节点。
  如果旧 bucket 为空，删除旧 bucket，并更新 min_freq。

get miss:
  返回 miss。
  不改变缓存。

put new:
  如果容量为 0，直接忽略。
  如果满了，淘汰 freq=min_freq bucket 中最旧节点。
  新节点 freq=1。
  min_freq=1。

put existing:
  更新 value。
  是否增加频率要定义。常见做法是按一次访问处理，freq+1。

delete:
  同时从 map 和 freq bucket 删除。
  必要时更新 min_freq。
```

如果要支持 decay，不变量会更复杂。简化版可以先不做全局 decay，或者用 epoch：

```text
node.raw_freq
global_epoch
node.last_decay_epoch
effective_freq(node)
```

这时必须定义：

```text
effective_freq 不能为负。
衰减后 node 必须移动到正确 bucket。
min_freq 基于 effective_freq 计算。
```

如果要支持并发，还要加锁不变量：

```text
map、freq_map、min_freq 的修改必须在同一锁或 shard 锁内。
不能在持锁时执行 loader。
不能在持锁时执行用户 eviction callback。
```

面试里可以这样答：

```text
我会先定义 key、node、freq bucket 和 minFreq 的不变量：每个 key 只对应一个 node；每个 node 只在一个频率桶里；map.size 等于所有 bucket 节点数；size 不超过 capacity；node.freq 必须等于所在 bucket；minFreq 必须是当前最小频率；同频桶内再按 LRU 或插入顺序淘汰。get 命中要把节点从 f 桶移到 f+1 桶；put 新 key 满容量时淘汰 minFreq 桶里的最旧节点；put 旧 key 是否增加频率要事先定义。并发实现还要保证 map、bucket、minFreq 的修改原子完成，回调和 loader 不在锁内执行。
```

## Q054. LFU 的常见误用是什么，误用后通常会产生什么线上症状？

**回答：**

LFU 的常见误用，核心都是把“频率”理解得太简单。

第一，不做 decay。精确 LFU 如果只增不减，旧热点会长期留在缓存里。

线上症状：

```text
活动结束后，活动商品仍然占着缓存。
新热点出现，但命中率爬升很慢。
缓存里很多 key 最近根本没人访问。
```

第二，把 LFU 当成 LRU 用。某些业务其实强依赖最近性，比如用户刚打开的工作空间、刚进入的会话、刚切换的项目。LFU 可能因为新 key 频率低，把它们挡在缓存外。

线上症状：

```text
用户刚切换场景时连续 miss。
新热点响应慢。
短时间突发流量无法被缓存吸收。
```

第三，把业务热度当缓存频率。播放量、销量、点赞数不等于这个缓存 key 的访问频率。用业务排行榜直接灌 LFU，可能缓存了业务上热门但当前接口不常读的数据。

线上症状：

```text
预热了很多热门业务对象，但接口 hit rate 没上去。
缓存里有大批不会被当前服务访问的数据。
```

第四，忽略重试和异常流量。失败请求重试多次，会把一次用户意图变成多次缓存访问。LFU 会把异常 key 频率刷高。

线上症状：

```text
下游故障后，某些错误 key 变成高频。
故障恢复后，缓存仍然保留异常期间的热点。
```

第五，对大小差异大的对象使用纯 LFU。访问次数多不等于值得缓存。一个大对象访问 100 次，可能不如一个小对象访问 80 次划算。

线上症状：

```text
内存占用高。
byte hit rate 不好。
大对象挤掉大量小热点。
```

第六，为了精确 LFU 付出过高并发成本。频率桶、minFreq、锁、计数器都要维护，结果命中率提升不大，CPU 却上去了。

线上症状：

```text
CPU 高。
锁等待高。
p99 变差。
吞吐低于简单 LRU。
```

第七，安全场景里缓存错误 allow。LFU 会把高频 allow 保留下来，如果 key 缺少用户或租户维度，问题会持续更久。

面试里可以这样答：

```text
LFU 常见误用包括不做 decay、把它用于强 recency 场景、把业务排行榜当缓存访问频率、忽略重试造成的频率污染、用纯访问次数处理大小差异很大的对象、追求精确 LFU 导致锁和 CPU 成本过高，以及在权限缓存里缺少用户或租户维度。线上症状通常是旧热点长期不退、新热点进不来、故障重试后的异常 key 被保留、hit rate 没明显提升但 CPU 和 p99 上升、byte hit rate 差，甚至权限错误被高频访问保护起来。
```

## Q055. LFU 在单机和分布式环境中的语义有什么差异？

**回答：**

单机 LFU 看到的是当前进程或当前缓存实例的访问频率。只要所有请求都经过这一个实例，频率统计相对清楚：

```text
key A 被访问 1000 次。
key B 被访问 10 次。
容量不足时更倾向保留 A。
```

分布式环境里，LFU 的频率语义会被拆碎。

第一，本地缓存的 LFU 是每个实例各算各的：

```text
instance 1:
  user:1 frequency = 100

instance 2:
  user:1 frequency = 0
```

负载均衡策略一变，某个实例看到的频率也会变。扩容后新实例频率为空，缩容后热点重新分布。

第二，Redis Cluster 这类分片缓存是每个 shard 局部统计。一个 key 只落在一个 shard 上，该 key 的 LFU 计数只在这个 shard 内有意义。不同 shard 之间不会比较全局频率。

第三，多级缓存会过滤访问。应用本地缓存命中后，请求不会到 Redis；Redis 看到的是本地缓存 miss 流量，而不是用户原始访问流量。

```text
真实用户访问:
  key A 每秒 1000 次。

本地缓存命中:
  Redis 可能每分钟才看到几次。
```

这会让下层 LFU 低估上层热点。

第四，近似计数在不同节点上不可比较。Redis LFU 的概率计数器和 decay 与节点本地时间、访问路径、采样有关。主从切换、迁移、重启后，频率元数据是否保留、是否准确，要看具体实现。

第五，分布式系统里重试会放大某些节点上的频率。比如某个 shard 慢，客户端重试都打到同一组 key，这些 key 的频率可能被异常抬高。

所以分布式 LFU 不能理解成“全局访问频率最低的 key 被淘汰”。它更像：

```text
在某个实例、某个 shard、某个时间窗口内，根据该节点看到的近似频率做淘汰。
```

如果业务真的需要全局热门对象，通常要从访问日志、流式聚合或离线统计里算 top-K，再用于预热或缓存保护，而不是指望分布式 LFU 自动给出全局热度。

面试里可以这样答：

```text
单机 LFU 的频率来自单个缓存实例看到的全部访问；分布式环境里，频率会变成局部语义。本地缓存各实例各算各的，Redis Cluster 各 shard 只统计本 shard 的 key，多级缓存还会让下层只看到上层 miss 流量。近似 LFU 的计数器和 decay 也依赖节点本地状态，主从切换、迁移、重启后不一定可比较。所以分布式 LFU 不是全局最低频率淘汰，而是按实例、分片和访问路径看到的局部近似频率淘汰。全局热点通常要靠日志聚合或 top-K 流式统计单独计算。
```

## Q056. ARC 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

ARC，全称 Adaptive Replacement Cache，核心目标是自动在 recency 和 frequency 之间调节。它想解决的问题是：单纯 LRU 只看最近，单纯 LFU 只看频率，真实 workload 往往两者都有，而且比例会变。

ARC 论文把它设计成一种自调节缓存替换算法。它维护两类居民缓存：

```text
T1:
  最近只访问过一次的对象，代表 recency。

T2:
  至少被再次访问过的对象，代表 frequency。
```

同时还维护两个 ghost list：

```text
B1:
  从 T1 被淘汰出去的 key，只保留 key，不保留 value。

B2:
  从 T2 被淘汰出去的 key，只保留 key，不保留 value。
```

ghost list 的作用很关键。它们告诉 ARC：刚被淘汰的对象后来是否又被访问。如果 B1 命中，说明最近性区域可能太小，要给 T1 更多空间；如果 B2 命中，说明频率区域可能太小，要给 T2 更多空间。ARC 用一个目标参数 `p` 在 T1 和 T2 之间动态调节。

ARC 主要解决性能问题。它希望在不同 workload 下提高命中率，尤其是同时处理：

```text
最近访问热点
长期高频热点
一次性扫描
热点迁移
```

它不解决正确性。ARC 只决定容量不够时删谁，不判断 value 是否过期、是否有权限、是否损坏。

它不解决安全性。如果缓存 key 缺少安全上下文，ARC 也会保留错误的热点数据。

可维护性方面，ARC 的使用层面比较省心，因为它自适应，不需要手动固定 recency/frequency 比例。但实现层面比 LRU 复杂，需要维护四个列表和自适应参数。

面试里可以这样答：

```text
ARC 的核心目标是自适应平衡 recency 和 frequency。它用 T1 保存最近只访问一次的对象，用 T2 保存再次访问过的对象，再用 B1/B2 两个 ghost list 观察被淘汰 key 是否又被访问，从而调整 T1/T2 的目标比例。它主要解决性能问题，特别是混合最近性、频率热点、scan 和热点迁移的 workload。ARC 不解决数据新鲜度、权限和安全问题；这些仍然要靠 TTL、版本、失效和授权校验。
```

## Q057. ARC 的典型适用场景和不适用场景分别是什么？

**回答：**

ARC 适合 workload 不稳定、recency 和 frequency 都存在、而且不想手工调参数的场景。

典型适用场景：

```text
数据库 page cache:
  有最近访问页，也有长期热点页，还会遇到扫描查询。

文件系统缓存:
  用户反复访问一批文件，同时后台任务可能顺序读大文件。

应用对象缓存:
  既有短期 burst，也有长期热门对象。

无法提前判断策略:
  有时 LRU 好，有时 LFU 好，希望策略自动调节。

scan 干扰明显:
  ARC 的 ghost list 能观察一次性对象是否真的会回来。
```

ARC 论文的一个重要卖点就是 scan-resistant。因为一次性扫描对象通常只进入 T1，之后被淘汰进 B1；如果没有再次访问，它们不会长期污染 T2。

不适用场景也要讲清楚。

第一，访问完全随机。没有 recency，也没有 frequency，ARC 没有什么可学。

第二，对象大小和 miss 成本差异很大。原始 ARC 更像按条目数管理，如果 value 大小差异明显，需要加 weight-aware 逻辑，否则高命中率不一定代表高收益。

第三，极高并发且实现需要全局锁。ARC 每次访问可能更新四个列表和参数 `p`，实现不当会比 LRU 更容易出现锁竞争。

第四，需要严格 TTL、一致性或权限控制的场景。ARC 只是替换策略，不是 freshness 或 authorization 策略。

第五，工程上还要注意专利和可用实现。ARC 曾经因为专利问题让很多开源项目避免直接实现，转而使用 2Q、CAR、TinyLFU、SLRU 等替代策略。面试里不必展开法律细节，但要知道 ARC 不是所有项目都愿意直接用。

面试里可以这样答：

```text
ARC 适合 recency 和 frequency 混合、workload 会变化、scan 干扰明显、又不想手工调 LRU/LFU 比例的场景，比如数据库页缓存、文件系统缓存和应用对象缓存。它用 ghost list 自适应调整最近性区和频率区，对一次性扫描比纯 LRU 稳。它不适合完全随机访问、对象大小和 miss 成本差异很大但没有 weight 处理、极高并发下只能全局加锁的实现，也不能替代 TTL、一致性和权限控制。工程上还要注意 ARC 的专利历史和可用实现。
```

## Q058. ARC 和相近概念最容易混淆的边界在哪里？

**回答：**

ARC 最容易和 LRU、LFU、2Q、SLRU、TinyLFU 混淆。

第一，ARC 不是 LRU。LRU 只有一条最近性队列，容量不够就淘汰最久未访问对象。ARC 有 T1/T2/B1/B2 四个列表，并且会根据 ghost hit 调整 recency/frequency 的目标比例。

第二，ARC 不是 LFU。LFU 直接维护访问频率，按低频淘汰。ARC 不需要精确计数，它通过“是否再次访问”和 ghost list 的反馈来判断应该偏向最近性还是频率。

第三，ARC 和 2Q/SLRU 有相似之处，但 ARC 会自适应。2Q 和 SLRU 通常也把缓存分成 probation/protected 或多个队列，但队列比例常常是固定的或手动配置的。ARC 的 `p` 会根据 B1/B2 命中调整。

第四，ARC 和 TinyLFU 的边界在 admission。TinyLFU 更像准入策略：一个新对象 miss 后，是否值得进入缓存，要和已有 victim 的估计频率比较。ARC 更像替换策略：对象进入后，在 T1/T2/B1/B2 中迁移，用 ghost list 调整队列目标。

第五，ARC 的 ghost list 不是负缓存。B1/B2 只保存被淘汰 key 的元数据，用来调策略，不代表这个业务对象不存在，也不能直接返回给用户。

第六，ARC 不是 TTL。B1/B2 里的 key 不是“过期 key”，而是“被淘汰 key 的历史痕迹”。TTL 解决新鲜度，ARC 解决容量。

面试里可以这样答：

```text
ARC 的边界在于它是自适应替换策略，不是简单 LRU、LFU 或 TTL。它和 LRU 的区别是有 T1/T2/B1/B2 四个列表；和 LFU 的区别是不用精确频率计数，而是通过 ghost hit 调整 recency/frequency 比例；和 2Q/SLRU 的区别是 ARC 的比例会自动调节；和 TinyLFU 的区别是 TinyLFU 更偏 admission policy，ARC 更偏 replacement policy。ARC 的 ghost list 只保存被淘汰 key 的历史，不是负缓存，也不能当业务结果返回。
```

## Q059. ARC 在高并发场景下可能出现哪些隐藏问题？

**回答：**

ARC 在高并发下比 LRU 更难实现。原因很简单：LRU 通常维护一条主队列，ARC 要维护四个列表和一个自适应参数。

每次访问可能影响：

```text
T1:
  最近一次访问对象。

T2:
  多次访问对象。

B1:
  从 T1 淘汰的 ghost key。

B2:
  从 T2 淘汰的 ghost key。

p:
  T1 的目标大小，影响替换方向。
```

第一个隐藏问题是锁竞争。如果所有操作都用一把全局锁，ARC 的 get 命中也可能修改列表。B1/B2 命中还会调整 `p`，并触发 replacement。高并发热点下，这把锁会很重。

第二个问题是多个列表之间的一致性。一个 key 只能出现在 T1、T2、B1、B2 中的一个位置。并发移动时，如果锁粒度不对，就可能出现：

```text
同一个 key 同时在 T1 和 T2。
key 已经从 T1 删除，但 map 还指向旧节点。
B1/B2 ghost 没清理，目录膨胀。
p 被两个线程按旧状态重复调整。
```

第三个问题是 ghost list 内存。B1/B2 不存 value，但存 key 和元数据。高基数 workload 下，ghost list 仍然会占内存。如果 key 很长，ghost 开销不小。

第四个问题是自适应参数在异常流量下抖动。比如批任务扫描、故障重试、热点攻击同时发生，B1/B2 命中会让 `p` 快速变化。策略可能在短时间内来回摆动。

第五个问题是异步维护会改变语义。为了降低锁竞争，可能把访问事件放到 buffer 后批量处理。这样 ARC 的列表状态和真实访问之间会有延迟，ghost hit 反馈也滞后。

第六个问题是分片 ARC 的语义偏差。按 key hash 分 shard 可以降低锁竞争，但每个 shard 只自适应自己的流量。全局上看，不再是一个 ARC，而是多个局部 ARC。

面试里可以这样答：

```text
ARC 高并发的隐藏问题来自四个列表和自适应参数。每次访问都可能移动 T1/T2/B1/B2，并调整 p；如果用全局锁，get 也会产生明显锁竞争。锁粒度不对会出现 key 同时在多个列表、map 指向旧节点、ghost list 泄漏、p 重复调整等一致性问题。B1/B2 虽然只存 key，也会在高基数和长 key 场景占内存。为了降低竞争做分片或异步维护后，ARC 语义又会变成局部、滞后的近似 ARC。
```

## Q060. ARC 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

ARC 的状态比 LRU 更复杂。它不仅有缓存内容，还有 T1/T2/B1/B2 和参数 `p`。崩溃、重启、超时、重试都会影响这些状态。

崩溃和重启后，如果 ARC 是纯内存的，所有队列和 ghost list 都丢失：

```text
T1/T2:
  当前缓存内容丢失。

B1/B2:
  最近被淘汰 key 的历史丢失。

p:
  自适应目标回到初始值。
```

这意味着重启后 ARC 需要重新学习 workload。短期内它可能退化得像一个冷缓存，无法马上判断应该偏向 recency 还是 frequency。

如果试图持久化 ARC 状态，也有风险：

```text
ghost key 还代表当前 workload 吗？
p 是否应该在长时间停机后保留？
T1/T2 里的 value 是否过期？
schema 和 version 是否仍然有效？
```

不能只恢复队列顺序，而不检查 TTL、版本和数据源状态。

超时场景里，旧 loader 晚到仍然会污染 ARC：

```text
1. 请求 A miss，开始加载旧值。
2. 请求 B 更新数据，并写入新值。
3. 请求 A 超时后 loader 晚到。
4. A 把旧值写入 ARC。
5. 旧值进入 T1，后续若被访问，还可能进入 T2。
```

解决方式仍然是版本 CAS，ARC 本身不会判断新旧。

重试会影响 ghost feedback。一个异常请求如果反复访问同一批刚被淘汰的 key，可能制造大量 B1/B2 命中，导致 `p` 朝错误方向调整。

删除和重建也有边界：

```text
resource:123 被删除后，key 可能还在 B1/B2。
resource:123 重新创建后，ghost hit 是否应该影响 p？
```

如果业务上新旧对象不能继承历史，就要把 version 放进 key，或者在删除时清理相关 ghost 元数据。

面试里可以这样答：

```text
ARC 在崩溃重启时会丢失 T1/T2/B1/B2 和参数 p，重启后需要重新学习 workload；如果持久化这些状态，也必须检查 TTL、version、schema 和停机时间，否则会把旧 ghost 反馈带回来。超时和 loader 晚到仍然可能把旧值写回缓存，ARC 只会把它当普通新对象，需要版本 CAS 防止旧值覆盖。重试和故障流量会制造异常 B1/B2 命中，让 p 朝错误方向调整。删除重建时还要决定 ghost key 是否继承历史，通常要用版本化 key 或清理 ghost 元数据。
```

## Q061. ARC 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

ARC 的策略瓶颈主要来自 CPU、内存和锁竞争。I/O 和网络仍然主要属于 miss 回源或远程缓存部署。

CPU 成本来自列表维护和自适应逻辑：

```text
命中 T1:
  移动到 T2。

命中 T2:
  更新 T2 内部最近性。

命中 B1:
  调整 p，触发 replacement，把 key 从 ghost 恢复为 resident。

命中 B2:
  反向调整 p，触发 replacement。

miss:
  插入 T1，可能淘汰 resident key 到 ghost list，还可能裁剪 ghost list。
```

这比单条 LRU 队列复杂。虽然 ARC 论文强调它可以做到 O(1)，但 O(1) 不等于常数小。四个列表、多个哈希索引、参数调整、容量约束都会增加常数成本。

内存成本来自：

```text
resident entries:
  T1/T2 里的 key、value、节点元数据。

ghost entries:
  B1/B2 只存 key，但仍然要占 map entry、节点、key bytes。

目录:
  为了快速判断 key 在哪个列表，需要索引。
```

ARC 的 ghost list 让总目录规模可以接近缓存容量的两倍。value 不会翻倍，但 key 和元数据会增加。

锁竞争来自全局状态：

```text
T1/T2/B1/B2 的移动。
p 的调整。
replacement 规则。
目录 map 的更新。
```

如果实现分片，锁竞争会降低，但每个 shard 有自己的 `p` 和 ghost 反馈，语义变成局部 ARC。

I/O 和网络不是 ARC 算法的核心瓶颈，但在两种情况下会出现：

```text
miss 回源:
  ARC 决定没命中，数据库或 RPC 成为瓶颈。

远程缓存:
  ARC 在 Redis/缓存服务内部，客户端仍要付网络成本。
```

面试里可以这样答：

```text
ARC 的主要瓶颈通常是 CPU、内存和锁竞争。CPU 来自 T1/T2/B1/B2 四个列表的移动、ghost hit 后调整 p、replacement 和目录维护；内存来自 resident entry 之外还要保留 B1/B2 ghost key，目录规模可能接近缓存容量的两倍；锁竞争来自这些全局列表和 p 的同步。I/O 和网络主要发生在 miss 回源或远程缓存部署里，不是 ARC 替换策略本身的核心成本。分片能降锁竞争，但语义会变成多个局部 ARC。
```

## Q062. ARC 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

ARC 的测试比 LRU/LFU 更复杂，因为它有四个列表和自适应参数。

correctness test 要先覆盖列表迁移：

```text
miss new key:
  进入 T1。

hit in T1:
  从 T1 移到 T2。

hit in T2:
  留在 T2，并更新 T2 内最近性。

evict from T1:
  key 进入 B1，value 被丢弃。

evict from T2:
  key 进入 B2，value 被丢弃。

hit in B1:
  增大 p，说明需要更多 recency 空间。

hit in B2:
  减小 p，说明需要更多 frequency 空间。
```

还要测不变量：

```text
T1、T2、B1、B2 互斥。
T1 + T2 <= capacity。
p 在 [0, capacity] 之间。
resident list 里有 value。
ghost list 里只有 key，没有 value。
目录中每个 key 指向唯一列表。
总目录大小不超过策略定义上限，通常约束在 2 * capacity。
```

stress test 主要测并发和异常：

```text
多线程同时访问 T1/T2/B1/B2 中的 key。
高并发 miss 和 replacement。
scan workload 下 ghost list 快速变化。
p 在并发 ghost hit 下是否越界。
delete/clear 与 get/put 并发。
eviction callback 慢或抛异常。
长时间运行检查 ghost list 是否泄漏。
```

benchmark 要分两部分。策略质量用 trace-driven simulation：

```text
LRU vs LFU vs ARC vs TinyLFU。
不同容量下的 hit rate、byte hit rate、miss cost。
scan、Zipf、phase shift、循环访问。
```

实现成本用 microbenchmark 或服务压测：

```text
get hit in T1/T2。
ghost hit in B1/B2。
miss + replacement。
ns/op、allocs/op、B/op。
p50/p95/p99。
锁等待和 CPU。
ghost metadata 内存。
```

面试里可以这样答：

```text
ARC 的 correctness test 要验证 miss 进入 T1、T1 命中进入 T2、T2 命中更新最近性、T1 淘汰进 B1、T2 淘汰进 B2、B1 命中增大 p、B2 命中减小 p，并检查四个列表互斥、T1+T2 不超过容量、p 在边界内、ghost list 只存 key。stress test 要并发访问四个列表、并发 replacement、scan、p 边界、delete/clear 和慢回调。benchmark 则分策略质量和实现成本，前者用 trace 比 hit rate 和 miss cost，后者测 ns/op、allocs、p99、锁等待和 ghost 元数据内存。
```

## Q063. 如果要求从零实现一个简化版 ARC，你会先定义哪些不变量？

**回答：**

实现 ARC 前先把四个列表和参数 `p` 的边界定义清楚。简化版 ARC 可以先按固定容量、等大小对象、单线程来设计，再扩展并发和权重。

核心结构：

```text
T1:
  resident recency list，存 key + value。

T2:
  resident frequency list，存 key + value。

B1:
  ghost list for T1，存 key。

B2:
  ghost list for T2，存 key。

p:
  T1 的目标大小，范围 [0, capacity]。
```

核心不变量：

```text
互斥:
  一个 key 最多存在于 T1、T2、B1、B2 中的一个列表。

resident 容量:
  |T1| + |T2| <= capacity。

ghost 只存 key:
  B1/B2 不能保留 value 引用，否则内存会被放大。

目录大小:
  |T1| + |T2| + |B1| + |B2| <= 2 * capacity。

p 边界:
  0 <= p <= capacity。

列表顺序:
  每个列表内部维护最近性，头部最近，尾部最旧。

目录一致:
  map[key] 指向 key 当前所在列表和节点。
```

操作语义：

```text
hit T1:
  从 T1 移到 T2 头部。

hit T2:
  移到 T2 头部。

hit B1:
  p 增大。
  触发 replacement。
  从 B1 删除 key。
  加载 value 后放入 T2。

hit B2:
  p 减小。
  触发 replacement。
  从 B2 删除 key。
  加载 value 后放入 T2。

miss new:
  必要时 replacement。
  新 key 放入 T1。
```

replacement 的不变量是：

```text
如果 |T1| > p:
  从 T1 尾部淘汰 resident key 到 B1。

否则:
  从 T2 尾部淘汰 resident key 到 B2。
```

真实 ARC 的 replacement 条件有一些细节，尤其是命中 B2 时如何比较 `|T1|` 和 `p`。简化版也要把这些条件写成测试，不要靠临场理解。

并发实现要加：

```text
四个列表和目录 map 的修改必须原子。
p 的调整和 replacement 必须在同一临界区或有事务化状态机。
不能在持锁时执行 value loader。
不能在持锁时执行用户回调。
```

面试里可以这样答：

```text
我会先定义 T1/T2/B1/B2 和 p 的不变量：T1/T2 是 resident list，存 key 和 value；B1/B2 是 ghost list，只存 key；一个 key 只能在四个列表之一；T1+T2 不超过 capacity；四个列表总大小不超过 2*capacity；p 必须在 0 到 capacity；map 必须准确指向 key 所在列表。操作上，T1 命中进 T2，T2 命中刷新最近性，B1 命中增大 p，B2 命中减小 p，新 miss 进 T1，replacement 根据 p 从 T1 或 T2 淘汰到对应 ghost list。并发实现要保证列表、目录和 p 的更新原子，loader 和回调不能在锁内执行。
```

## Q064. ARC 的常见误用是什么，误用后通常会产生什么线上症状？

**回答：**

ARC 常见误用主要来自两点：把它当成“比 LRU 永远更好”的通用解，或者实现时忽略 ghost list 的成本和复杂度。

第一，把 ARC 当成 freshness 策略。ARC 只决定容量淘汰，不知道数据是否过期。

线上症状：

```text
hit rate 不错，但用户读到旧值。
权限、库存、价格更新后缓存仍然返回旧结果。
```

第二，把 ARC 用在对象大小差异很大的缓存里，却仍按 entry count 管理。ARC 命中率可能提高，但 byte hit rate 不一定好。

线上症状：

```text
内存占用高。
大对象挤掉很多小热点。
hit rate 好看，网络和 p99 仍然差。
```

第三，ghost list 不受控。B1/B2 只存 key，但 key 很长或高基数时，ghost 元数据也会很大。

线上症状：

```text
value 内存没满，元数据先涨。
缓存目录膨胀。
GC 或 allocator 压力高。
```

第四，没考虑高并发实现成本。ARC 维护四个列表和 `p`，如果全局锁保护，吞吐可能低于简单 LRU。

线上症状：

```text
CPU 高。
锁等待高。
cache get p99 变差。
替换策略更聪明，但服务更慢。
```

第五，把分片 ARC 当全局 ARC。每个 shard 的 `p` 独立调整，不能代表全局 workload。

线上症状：

```text
不同分片命中率差异大。
某些分片 ghost list 高速变化。
扩容后策略重新学习，命中率波动。
```

第六，用 ARC 避免做 workload 分析。ARC 自适应，不代表不需要 trace。它也可能在异常重试、扫描、批任务期间被错误反馈带偏。

面试里可以这样答：

```text
ARC 常见误用是把它当成永远优于 LRU 的通用解，或者把它当 freshness、一致性、安全机制。ARC 只做容量替换，不能保证数据新鲜和权限正确。对象大小差异大时按 entry 管理会让 hit rate 好看但 byte hit rate 差；ghost list 如果不限制，会在长 key、高基数场景吃掉大量元数据；高并发全局锁实现可能让 p99 比 LRU 更差；分片 ARC 也不是全局 ARC。线上症状包括旧值命中、元数据膨胀、锁等待高、扩容后命中率波动和异常流量把 p 调偏。
```

## Q065. ARC 在单机和分布式环境中的语义有什么差异？

**回答：**

单机 ARC 的语义相对完整：一个缓存实例维护一组 T1/T2/B1/B2 和一个参数 `p`，所有访问反馈都进入同一个自适应控制环。

```text
single ARC:
  一套 ghost history。
  一个 p。
  一个 replacement 反馈系统。
```

分布式环境里，这个控制环会被拆开。

第一，多实例本地 ARC 各自学习自己的 workload：

```text
instance A:
  p 偏向 recency。

instance B:
  p 偏向 frequency。
```

负载均衡、会话粘性、扩缩容都会影响每个实例看到的访问序列。一个实例的 ghost hit 对另一个实例没有意义。

第二，分片缓存是每个 shard 局部 ARC。每个 shard 只维护自己的 T1/T2/B1/B2。全局上看，不存在一个统一的 `p`。

第三，多级缓存会让下层 ARC 看到过滤后的流量。上层本地缓存命中后，下层分布式缓存不会知道这个 key 仍然热。下层 ARC 看到的是 miss 流，而不是原始用户访问。

第四，迁移和故障转移会打断学习状态。key 从一个 shard 迁到另一个 shard，ghost history 和 `p` 未必一起迁移。即使迁移，也可能不再适合新 shard 的混合流量。

第五，分布式 ARC 很难做全局 ghost list。理论上可以集中统计访问历史，但那会引入通信、同步和一致性成本，通常不划算。

所以生产上 ARC 在分布式环境里的语义更接近：

```text
每个实例或每个 shard 根据自己看到的局部访问历史，自适应调整局部 recency/frequency 比例。
```

不要把它解释成全局最优替换。

面试里可以这样答：

```text
单机 ARC 有一套完整的 T1/T2/B1/B2 和一个 p，所有访问都反馈到同一个自适应控制环。分布式环境里，这个语义会变成局部的：每个应用实例或 shard 都维护自己的 ghost history 和 p；多级缓存还会让下层只看到上层 miss 流量；迁移和故障转移会打断学习状态。全局 ARC 需要共享 ghost list 和访问历史，通信成本很高，通常不做。所以分布式 ARC 不是全局最优替换，而是多个局部自适应策略的组合。
```

## Q066. TinyLFU 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

TinyLFU 的核心目标是用很小的元数据估计近期访问频率，并用这个估计来决定“新对象是否值得进入缓存”。它主要解决的是缓存准入问题，也就是 admission，而不只是 eviction。

传统缓存常见流程是：

```text
miss 后加载对象。
把新对象放进缓存。
如果满了，淘汰一个旧对象。
```

TinyLFU 多问一步：

```text
这个新对象真的值得进入缓存吗？
它的近期频率是否高过要被淘汰的 victim？
```

TinyLFU 论文的重点是：缓存不能只在满了之后决定淘汰谁，也要决定 miss 后的新对象是否应该被接纳。它通过紧凑的频率估计结构记录访问历史，让一次性 scan 对象不容易把旧热点挤掉。

TinyLFU 主要解决性能问题：

```text
减少缓存污染。
提高命中率。
在有限内存下保留更有复用价值的对象。
```

它不解决正确性。被接纳的对象仍然可能过期、权限变化或 value 损坏。

它不解决安全性。错误的 key 维度、错误的权限缓存，TinyLFU 也不会自动发现。

可维护性方面，TinyLFU 的概念比 LRU 复杂，但它的元数据可以做得很小。Caffeine 采用 Window TinyLFU，就是把一个小的 window cache 和主缓存的 SLRU 结合起来，用 TinyLFU 控制准入，同时保留对短期 burst 的适应能力。

面试里可以这样答：

```text
TinyLFU 的核心目标是用紧凑的近期频率估计来做 admission control，判断 miss 后的新对象是否值得进入缓存，而不是默认把它放进去。它主要解决性能问题，减少 scan 和一次性访问对象污染缓存，提高有限容量下的命中质量。它不解决数据新鲜度、权限和安全问题。工程上常见的是 Window TinyLFU，用小窗口吸收短期 burst，主缓存用 SLRU，再由 TinyLFU 决定候选是否能替换 victim。
```

## Q067. TinyLFU 的典型适用场景和不适用场景分别是什么？

**回答：**

TinyLFU 适合“miss 很多，但不是所有 miss 都值得缓存”的场景。它尤其适合有 scan、长尾、一次性访问和稳定热点混在一起的 workload。

典型适用场景：

```text
长尾访问:
  少数热点频繁访问，大量尾部 key 偶尔访问。

scan 干扰:
  后台任务、批处理、导出任务会访问大量一次性 key。

容量紧张:
  每次错误接纳新对象都会挤掉有价值的旧热点。

热点相对稳定:
  近期频率能预测未来复用。

对象 miss 成本较高:
  提高命中质量能明显降低后端压力。
```

Caffeine 的效率文档说明它使用 Window TinyLFU，是因为这种策略能在多种 workload 下提供较高命中率，并用较小空间估计频率。这个选择很有代表性：TinyLFU 不只是理论算法，已经进入高性能本地缓存实现。

不适用场景：

```text
完全随机访问:
  没有可复用热点，频率估计没有价值。

缓存足够大:
  工作集几乎都能放下，admission 的收益不明显。

极强 recency:
  新对象马上会被短期反复访问，但历史频率还没积累。
  这也是 W-TinyLFU 要加 window 的原因。

频繁变化的 key 空间:
  key 生命周期很短，估计刚建立就失效。

安全或一致性强约束:
  TinyLFU 不能替代授权、TTL 和版本校验。

对象大小差异很大:
  如果只按频率比较，不看 bytes 和 miss cost，可能保留高频大对象。
```

TinyLFU 的收益要通过 trace 验证。它通常比纯 LRU 更抗 scan，但如果 workload 本身没有局部性，任何策略都救不了命中率。

面试里可以这样答：

```text
TinyLFU 适合长尾访问、scan 干扰、容量紧张、稳定热点明显的场景，因为它能阻止一次性 miss 对象进入缓存，把位置留给更可能复用的热点。它不适合完全随机访问、工作集本来就能放下、极强短期 recency 但没有历史频率的场景；这也是 W-TinyLFU 加小 window 的原因。key 生命周期很短、对象大小和 miss 成本差异很大、或者需要强一致和授权校验的场景，也不能只靠 TinyLFU。是否适合最好用真实 trace 验证。
```

## Q068. TinyLFU 和相近概念最容易混淆的边界在哪里？

**回答：**

TinyLFU 最容易和 LFU、LRU、Count-Min Sketch、Bloom filter、ARC、W-TinyLFU 混淆。

第一，TinyLFU 不是传统 LFU。LFU 通常说的是缓存内按访问频率淘汰；TinyLFU 更常用于准入，判断新对象能不能进入缓存。它估计的是近期访问频率，而不是维护精确全历史频率。

第二，TinyLFU 不是完整缓存策略本身。它通常要和一个实际存储结构组合，比如 SLRU、LRU window。Caffeine 的 Window TinyLFU 就是：

```text
small window:
  接纳新对象，适应短期 recency。

main cache:
  通常用 SLRU 保存经过筛选的对象。

TinyLFU:
  用频率估计决定候选是否能替换 victim。
```

第三，TinyLFU 用到的 sketch 不是 TinyLFU 的全部。Count-Min Sketch 只是频率估计的数据结构之一；TinyLFU 还包括 admission 规则、aging/reset、可能的 doorkeeper。

第四，Bloom filter 不能替代 TinyLFU。Bloom filter 判断“可能出现过没有”，不告诉你访问频率高不高。TinyLFU 需要比较候选和 victim 的估计频率。

第五，TinyLFU 和 ARC 的反馈来源不同。ARC 用 ghost list 观察被淘汰 key 是否又回来；TinyLFU 用近期频率估计决定新对象是否接纳。两者都想抗污染，但机制不同。

第六，TinyLFU 不是 TTL。它不会判断数据是否新鲜，只判断对象是否值得占缓存容量。

面试里可以这样答：

```text
TinyLFU 的边界在于它主要是 admission policy，不是传统 LFU 的精确频率淘汰。它通常要和 LRU window、SLRU 主缓存组合，W-TinyLFU 就是这种组合。Count-Min Sketch 只是 TinyLFU 估计频率的工具，不等于 TinyLFU；Bloom filter 只能判断可能存在，不能比较频率；ARC 用 ghost list 自适应 recency/frequency，TinyLFU 用近期频率估计筛选候选。TinyLFU 也不解决 TTL、权限和一致性。
```

## Q069. TinyLFU 在高并发场景下可能出现哪些隐藏问题？

**回答：**

TinyLFU 在高并发下的主要问题是频率估计结构的更新成本和准入决策的一致性。

每次访问通常都要更新频率估计：

```text
read hit:
  更新 sketch。

read miss:
  更新 sketch。
  可能进入 window。
  之后和 victim 比较频率。

write/update:
  可能更新缓存值，也可能影响 admission。
```

如果所有线程都更新同一个 Count-Min Sketch，计数器会成为争用点。热点 key 会反复打到同几组 counter，造成缓存行竞争和 CAS retry。高性能实现通常会用分段计数、buffer、批量 drain 或近似更新来降低开销，但语义会变成延迟更新。

第二个问题是 aging/reset。TinyLFU 要关注近期频率，不能让旧历史无限积累。常见做法是周期性衰减或重置计数器。高并发下 reset 很敏感：

```text
全局 reset:
  可能造成停顿或 CPU 尖峰。

增量 reset:
  实现复杂，读到的频率可能不一致。

并发访问中 reset:
  需要保证计数不越界、不变负、不破坏比较规则。
```

第三个问题是 admission 决策和缓存状态之间的竞态：

```text
1. 线程 A 选择 victim。
2. 线程 B 更新了 victim 或候选频率。
3. 线程 A 根据旧估计做替换。
```

这种竞态不一定破坏安全，但会让策略效果变差。为了性能，很多实现接受近似结果。

第四个问题是 window 和 main cache 的并发移动。W-TinyLFU 中，新对象先进入 window，window 淘汰出的候选再和 main cache victim 比较。这个路径涉及多个区域：

```text
window LRU
main probation
main protected
frequency sketch
```

高并发下如果锁粒度不对，会出现对象重复、容量超限、候选丢失或 protected 区比例失控。

第五个问题是高基数攻击。攻击者或异常流量不断访问大量随机 key，会持续污染 sketch。即使这些 key 不进入缓存，也会占用频率估计空间，增加误判。

第六个问题是指标误读。TinyLFU 可能拒绝大量 miss 对象进入缓存，这本来是好事；如果只看 admission reject count，会以为系统异常。要同时看 hit rate、miss cost 和被拒绝对象后续是否复用。

面试里可以这样答：

```text
TinyLFU 高并发的隐藏问题主要在频率 sketch 更新和 admission 决策。每次 hit/miss 都可能更新 Count-Min Sketch，热点 key 会造成 counter、缓存行和 CAS 争用；为了性能做 buffer 或批量 drain 后，频率估计会滞后。aging/reset 也麻烦，全局 reset 可能造成 CPU 尖峰，增量 reset 又会让读到的频率不完全一致。W-TinyLFU 还要并发维护 window、probation、protected 和 sketch，锁粒度不当会容量超限或对象重复。高基数随机 key 还会污染 sketch，增加误判。
```

## Q070. 缓存为什么能提升性能？

**回答：**

缓存能提升性能，靠的不是“内存一定快”这么一句话，而是局部性。一个系统里的访问通常不是均匀随机的。少数用户、商品、配置、权限、模型、页面片段会被反复访问；刚访问过的数据，短时间内也经常再次出现。缓存把这部分数据放在更近、更快的位置，避开慢路径。

慢路径可能是数据库、对象存储、跨地域 RPC、磁盘 I/O、复杂计算、模型加载，也可能只是一次昂贵的序列化。缓存命中后，系统少做了这些事。收益可以拆成几类：请求延迟下降，后端 QPS 下降，数据库连接池压力下降，CPU 重复计算减少，网络带宽减少，尾部故障时还能用旧值或降级值撑住一部分读流量。

一个简单模型是：

```text
平均成本 = hit_rate * hit_cost + miss_rate * miss_cost + cache_overhead
```

如果命中成本是 0.1ms，回源成本是 20ms，命中率 90%，平均读成本会明显下降。这个模型也提醒我们：缓存自己有开销。远端 Redis 有网络 RTT，本地缓存有锁、GC 和内存成本，压缩缓存有 CPU 成本。如果 miss cost 不高，或者数据几乎不复用，缓存可能不划算。

面试里可以补一句边界：缓存优化的是平均访问成本和后端压力，不自动保证正确性。库存、权限、支付状态这类强一致字段，就算缓存能变快，也要先回答“旧值能不能被接受”。

## Q071. 缓存命中率为什么不是唯一指标？

**回答：**

命中率只说明有多少请求在缓存里找到了值，它没有说明这些命中原本有多贵，也没有说明 miss 有多痛。高命中率可能没什么价值，低命中率也可能很值钱。

举个例子：90% 的请求原本查本地内存结构，只要 0.2ms；10% 的请求要打复杂 SQL，耗时 200ms。如果缓存命中的是前 90%，命中率很好看，但总体 p99 仍然被慢查询拖住。反过来，如果只命中那 10% 的慢查询，命中率不高，收益却很大。

所以要看 cost-weighted hit rate。缓存命中的价值应该按 miss cost、对象大小、后端压力和用户路径加权。CDN 或对象缓存还要看 byte hit rate，因为命中一个 1KB JSON 和命中一个 1GB 文件不是一回事。

还要看缓存带来的副作用：miss load latency、eviction rate、expired keys、stale hit rate、backend offload、p95/p99 是否下降。Redis 的 `keyspace_hits` 和 `keyspace_misses` 能算基本命中率，但排查时还要结合 `evicted_keys`、`expired_keys`、内存水位和命令耗时。否则你只知道“命中了”，不知道“省了什么”。

面试里可以这样说：命中率是入口指标，不是结论。真正要证明缓存有效，要证明它降低了目标路径的延迟、尾延迟、后端 QPS 或成本，并且没有引入不可接受的旧值和内存压力。

## Q072. LRU 在什么 workload 下会失效？

**回答：**

LRU 的假设是“最近访问过的数据，未来还可能访问”。只要这个假设不成立，LRU 就会失效，或者至少表现很差。

最典型的是 scan workload。后台任务、数据导出、批处理、全表扫描、日志回放会顺序访问大量 key，每个 key 只访问一次。LRU 会把这些“最近刚访问但以后不会再用”的 key 放进缓存，旧热点反而被挤出去。扫描结束后，缓存里留下的是一批冷数据，线上请求回到热点时会大量 miss。

第二类是循环访问且工作集略大于缓存容量。假设缓存能放 100 个 key，访问序列是 1 到 101 反复循环。每次访问的 key 都刚好被上一轮淘汰，LRU 可以接近 0 命中。这类 workload 用随机淘汰有时都比 LRU 好。

第三类是频率热点和最近性冲突。某个 key 长期高频访问，但短时间内被一批新 key 冲刷；LRU 只看最近性，可能把长期热点踢掉。LFU、TinyLFU、ARC 这些策略就是在不同程度上修正这个问题。

第四类是对象大小差异很大。按 entry 做 LRU 时，一个大对象和一个小对象同权。一个刚被访问过的大对象可能挤掉许多小热点，命中率看起来还可以，byte cost 和内存效率却很差。

所以面试里不要只说“LRU 简单好用”。更准确的说法是：LRU 适合有时间局部性的访问，不适合一次性扫描、循环工作集略大于容量、长期热点被短期噪声冲刷、对象大小和 miss cost 差异很大的场景。

## Q073. LFU 为什么能保留热点但适应变化较慢？

**回答：**

LFU 统计访问频率。一个 key 被访问很多次，计数就高，被淘汰概率就低。这让它很适合稳定热点：比如配置、热门商品、权限规则、排行榜片段。只要热点长期存在，LFU 能比 LRU 更稳地把它留在缓存里。

问题也出在这个“长期”。如果一个 key 过去很热，后来业务活动结束，访问量降下来，纯 LFU 仍然可能因为历史计数很高而保留它。新热点刚出现时，计数还低，可能进不来或很快被淘汰。于是 LFU 对 workload phase shift 反应慢。

真实系统通常会给 LFU 加衰减。Redis 的 LFU 就不是精确全历史计数，而是用近似计数和 decay，让旧访问逐渐失效。Caffeine 这类缓存也不会用朴素 LFU，而会用近期频率估计和准入策略，减少历史包袱。

面试里可以这样答：LFU 能保留热点，是因为它用频率表达“这个 key 反复被需要”；适应变化慢，是因为频率带有历史惯性。要让 LFU 可用，通常需要 aging、decay、滑动窗口或 TinyLFU 这种近期频率估计，否则旧热点会霸占缓存。

## Q074. TinyLFU 为什么引入准入策略？

**回答：**

TinyLFU 引入准入策略，是为了回答一个 LRU/LFU 容易忽略的问题：miss 后加载的新对象，真的值得进缓存吗？传统策略常见流程是 miss、回源、放入缓存、满了再淘汰一个旧对象。这个流程默认新对象应该进缓存，但 scan workload 下这个默认值很危险。

一次性访问的 key 如果都进入缓存，会把已有热点挤出去。TinyLFU 用很小的频率估计结构记录近期访问历史。新对象作为 candidate，要和即将被淘汰的 victim 比较估计频率。候选频率不够高，就不准入，避免污染缓存。

这就是 admission 和 eviction 的区别：eviction 问“满了以后踢谁”，admission 问“这个新对象有没有资格进来”。TinyLFU 的价值主要在后者。Caffeine 的 Window TinyLFU 又加了一个小窗口，让新对象先有机会证明自己；主缓存再用频率准入保护长期热点。这样既不会完全拒绝新热点，也能挡住大量一次性扫描。

面试里可以这样说：TinyLFU 的核心不是精确数频率，而是用近似近期频率做准入控制。它把缓存入口收紧，减少低复用对象挤掉高复用对象的概率。

## Q075. TTL 解决什么问题，又引入什么一致性风险？

**回答：**

TTL 解决的是缓存值不能无限期存在的问题。源数据可能变化，缓存失效通知可能漏掉，业务代码可能忘记删 key。给 key 设置过期时间，至少能保证旧值最终会消失。TTL 还可以限制负缓存、临时结果、会话态、排行榜快照、短期聚合查询的生命周期。

但 TTL 不等于一致性。TTL 到期前，缓存仍然可能返回旧值。比如商品价格已经改了，缓存还有 5 分钟才过期，用户就可能看到旧价。权限、库存、支付状态这类数据，如果只靠 TTL，风险更大。TTL 越长，旧值窗口越大；TTL 越短，回源压力越大。

TTL 还会引入雪崩风险。如果大量 key 在同一时间写入，并使用同一个 TTL，它们可能在同一个时间窗口一起过期，瞬间把请求打回数据库。AWS 的缓存实践里也专门提到给 TTL 加随机扰动，避免同批 key 同时过期。

另一个风险是“过期和更新顺序”。cache-aside 中，更新数据库后删除缓存比较常见；如果先删缓存再更新数据库，就可能有读请求在中间窗口读到旧数据库值并重新写回缓存。TTL 最后会修复它，但旧值窗口仍然存在。

所以 TTL 是兜底机制，不是完整一致性方案。更严格的系统还要配合主动失效、版本号、写入顺序、binlog/CDC、事件通知或读时校验。

## Q076. cache-aside 为什么容易出现数据库与缓存不一致？

**回答：**

cache-aside 的读路径很简单：先查缓存，miss 后查数据库，再把结果写回缓存。写路径通常是更新数据库，然后删除或更新缓存。问题是数据库和缓存是两个独立系统，中间没有天然原子事务。

最常见的不一致来自并发读写。一个读请求 miss 后读数据库旧值，还没来得及写缓存；另一个写请求更新数据库并删除缓存；第一个读请求随后把旧值写进缓存。结果数据库是新值，缓存是旧值。

另一个窗口是失败。数据库更新成功了，删除缓存失败；或者应用在两步之间崩溃；或者 Redis 短暂不可用。此时旧缓存继续存在，直到 TTL 到期或下一次修复任务处理。

还有多副本和延迟问题。读请求可能读到数据库从库旧值，再写入缓存；写请求只删了一个缓存层，本地缓存、Redis、CDN 还有其他层没删；消息通知乱序也可能让旧版本覆盖新版本。

缓解方式通常是：更新数据库后删除缓存，给缓存值带版本号或更新时间，写缓存前检查版本；使用短 TTL 作为兜底；通过 CDC/binlog 异步失效；对强一致路径直接读主库或做读后校验。不能把 cache-aside 描述成强一致模式，它本质上是性能优化加最终收敛。

## Q077. 双写缓存和数据库为什么难以保证原子性？

**回答：**

双写难，是因为缓存和数据库通常不是同一个事务资源。你可以先写数据库再写缓存，也可以先写缓存再写数据库，但这两步之间总有失败窗口。进程崩溃、网络超时、Redis 写失败、数据库提交失败、重试重复执行，都会让两个系统状态分叉。

先写数据库再写缓存：数据库提交成功后，缓存写失败，就会读到旧缓存；如果重试写缓存，又可能把旧业务对象写回去。先写缓存再写数据库：缓存已经暴露新值，数据库提交失败，用户读到一个并不存在的状态。

即使用分布式事务，也不一定值得。缓存是派生数据，追求的是快和可丢弃；把它拉进强事务，会让写路径变慢、可用性下降，而且 Redis/Memcached 这类缓存未必支持你想要的事务语义。更常见的做法是把数据库当 source of truth，缓存可重建，失败后靠删除缓存、版本校验、异步修复和 TTL 收敛。

如果业务必须强一致，通常不要让缓存参与判定。比如支付是否成功、权限是否允许、库存是否扣减，要以数据库或强一致存储为准；缓存只做展示加速或读优化。面试里可以直说：缓存和数据库双写的原子性不是靠“顺序写两次”保证的，必须引入事务、幂等、版本或把缓存降级为可丢弃派生状态。

## Q078. cache stampede 如何导致下游雪崩？

**回答：**

cache stampede 是很多请求同时发现同一个 key miss 或 expired，然后一起回源。平时一个热点 key 的流量被缓存挡住，数据库每分钟可能只被打一次；过期瞬间，几千个请求同时穿透到数据库，慢查询、锁、连接池和 CPU 一起被打满。

它会导致雪崩，是因为后端变慢后，调用方继续排队、超时、重试。重试又增加流量，更多请求堆积，线程池和连接池耗尽。原本只是一个 key 过期，最后可能变成数据库、缓存、应用服务一起抖。

TTL 会放大这个问题。如果大量 key 使用相同 TTL，或者新节点冷启动导致大量 key 同时 miss，就会从单点击穿变成批量雪崩。AWS 的 thundering herd 描述就是这个逻辑：很多进程同时 miss 同一缓存项，然后同时打同一个昂贵查询。

排查时不要只看缓存总体命中率。总体命中率可能仍然很高，但某个热点 key 的过期会把 p99 打爆。需要看 hot key、per-key miss、loader concurrency、singleflight shared count、数据库连接池等待和 retry attempts。

## Q079. singleflight、互斥重建、随机 TTL 分别缓解什么问题？

**回答：**

这三者缓解的是不同阶段的问题。

singleflight 解决同一进程内重复回源。多个 goroutine 同时请求同一个 key，只让第一个执行加载函数，其他等待并共享结果。Go 的 `singleflight.Group` 就是 duplicate function call suppression。它适合本地并发合并，但默认不跨进程、不跨机器。

互斥重建解决跨请求或跨实例的热点重建。缓存过期后，只有拿到锁的请求去回源重建，其他请求等待、返回旧值或快速失败。锁可以在 Redis 里做，也可以在服务端用 per-key mutex 做。它要小心锁超时、持锁进程崩溃、回源失败和锁粒度过大。

随机 TTL 解决同一批 key 同时过期。比如基础 TTL 是 3600 秒，再加减几分钟随机扰动，让过期时间分散。它不能阻止单个热点 key 被击穿，但能减少大批 key 同时失效造成的雪崩。

实际系统常组合使用：随机 TTL 打散批量过期，singleflight 合并单进程内重复加载，分布式锁或 stale-while-revalidate 保护跨实例热点。不要把它们混成一种方案。

## Q080. 热点 key 如何影响单机和分布式缓存？

**回答：**

单机缓存里，热点 key 主要影响 CPU、锁、对象大小和 GC。一个 key 被大量请求反复读取，看起来命中率很好，但每次读取都可能打同一把锁、同一个 map bucket、同一个统计计数器，甚至同一个大对象反序列化路径。热点值很大时，还会造成内存带宽和拷贝压力。

分布式缓存里，热点 key 更麻烦。Redis Cluster 或一致性哈希通常把一个 key 放在一个 shard 上。再多机器也没有用，单个热点 key 仍然打到同一个分片。这个分片的 CPU、网络、连接数、输出缓冲区会先爆，其他分片很空。负载均衡在 key 粒度固定后很难救。

常见保护手段包括：本地缓存挡住一部分读；热点 key 副本化，用多个物理 key 承载同一值；按业务维度拆 key；对大对象做分块；请求侧做 singleflight；服务端限流；对极热数据使用 push 更新或进程内只读快照。

但热点拆分有一致性代价。多个副本 key 更新时可能乱序，删除时可能漏删，本地缓存也会扩大旧值窗口。面试里要说出取舍：热点保护通常是在一致性、内存、更新复杂度和读扩展之间换。

## Q081. 本地缓存和 Redis 缓存如何组合？

**回答：**

常见组合是两级缓存：进程内本地缓存做 L1，Redis 做 L2，数据库做 source of truth。读路径通常是先查本地缓存，本地 miss 查 Redis，Redis miss 再查数据库并回填 Redis 和本地缓存。

这样做的好处是明显的。本地缓存没有网络 RTT，适合配置、权限、热点小对象、模型路由表、租户配额这类高频读。Redis 提供跨进程共享和容量更大的缓存，避免每个实例都直接打数据库。两层配合可以降低 Redis QPS，也能降低数据库 QPS。

难点是一致性和容量。数据库更新后，必须让 Redis 失效，还要让各个进程的本地缓存失效。可以用短 TTL、Redis pub/sub、client-side caching invalidation、版本号、配置中心推送或定期 refresh。失效通知丢了，本地缓存可能比 Redis 更旧。

工程上我会给 L1 更短 TTL 和更小容量，只缓存高价值小对象；L2 承担更长 TTL 和跨实例复用。强一致读直接绕过缓存或做版本校验。还要监控 L1 hit、L2 hit、L2 miss load、失效延迟和本地缓存内存占用。

面试里可以这样答：本地缓存降低单请求延迟，Redis 降低跨实例重复回源；组合后性能更好，但失效链路更长，旧值窗口也更复杂。

## Q082. 缓存预热如何避免污染缓存？

**回答：**

缓存预热的目标是让冷启动节点在接流量前拥有一批真正高价值的数据。污染缓存的原因通常是预热列表选错了：把“可能用到”的数据都塞进去，结果挤掉了在线请求真正需要的工作集。

预热应该从访问 trace、业务热点和成本出发。优先加载最近窗口内高频、高 miss cost、小体积、低一致性风险的数据。比如热门配置、热门商品基础信息、模型权重、租户路由元数据。不要把全量用户、全量订单、全量搜索结果直接塞进缓存。

还要控制节奏。新节点预热时如果并发太高，本来是保护后端，反而变成批量回源压测。预热要限速、分批、可取消，并且在低峰或节点接流量前完成。AWS 对新 cache node 的 prewarm 建议，本质也是先用脚本或真实请求形态填充，再让节点进入服务。

最后要有准入和过期。预热数据也应该经过正常的容量策略、TTL、版本检查和租户配额，不应该拥有永久特权。预热后如果在线流量证明某些 key 没价值，就让它们自然淘汰。预热不是把缓存“填满”，而是缩短从 cold 到 warm 的危险窗口。

## Q083. 多租户缓存如何防止一个租户挤出另一个租户的工作集？

**回答：**

多租户缓存的问题是共享容量下的公平性。一个大租户、异常任务或恶意流量可能制造大量 key，把其他租户的热点挤出去。总体命中率可能还不错，但小租户的命中率和 p99 会明显变差。

第一步是 key 空间隔离。key 必须带 tenant id、schema version、权限维度，避免跨租户串值。第二步是容量隔离。可以按租户设置 maximum weight、entry quota、Redis 分片、独立 namespace，或者至少做 per-tenant admission 和 eviction 统计。

更细一点，可以按租户做预算：每个租户有保底容量和可突增容量。保底部分防止被挤出，突增部分允许大租户利用空闲资源。淘汰时不要只按全局 LRU，要考虑租户当前占用、租户权重、对象大小和 miss cost。

还要防止高基数污染。一个租户如果不断访问随机 key，TinyLFU 准入、负缓存 TTL、请求限流和 per-tenant loader concurrency 都要起作用。监控上要看 per-tenant hit rate、evictions、bytes、hot keys、load errors，而不是只看全局指标。

面试里可以这样总结：多租户缓存不能只做全局 LRU。要同时做 key 隔离、容量配额、准入控制、热点保护和 per-tenant 观测，否则一个租户的工作集会吞掉整个缓存。

## Q084. 模型权重缓存和普通 KV 缓存有什么差异？

**回答：**

模型权重缓存缓存的是模型参数、checkpoint shard、engine、编译产物或本地文件。普通 KV 缓存缓存的是业务 key 到 value 的映射，比如用户资料、配置、查询结果。两者都叫缓存，但生命周期和成本模型完全不同。

模型权重通常很大，单位是 GB 到数百 GB；变化频率低，跟 model id、revision、dtype、量化格式、并行切分和 engine 版本绑定。命中权重缓存，主要省的是下载、磁盘读取、反序列化、CPU 到 GPU 搬运和冷启动时间。淘汰权重缓存时，要考虑模型再次加载成本、SLO、当前副本数和磁盘/显存水位。

普通 KV 缓存的对象通常更小，key 数量更高，变更频率更复杂。它的主要问题是 TTL、淘汰、一致性、热点、stampede、租户隔离。一个业务 key miss 往往只回源一条记录或一个查询；一个模型权重 miss 可能让整个模型实例冷启动。

安全边界也不同。权重缓存一般不含用户请求内容，但涉及供应链、文件完整性、版本签名和模型许可。业务 KV 缓存可能包含用户数据、权限和隐私。LLM 的 KV cache 又是第三类，它缓存的是请求上下文的 attention key/value 激活，跟模型权重缓存不能混为一谈。

## Q085. LLM KV cache 为什么和请求上下文强相关？

**回答：**

LLM KV cache 存的是某个 prompt 或某段上下文在每一层 attention 里计算出来的 key/value。它不是“某个 token 的通用缓存”。同一个词，在不同前文、不同位置、不同 chat template、不同 RoPE offset、不同 LoRA adapter 下，产生的 KV 状态都可能不同。

自回归模型生成下一个 token 时，会读取之前所有 token 的 KV。这个“之前所有 token”就是请求上下文。上下文一变，KV 的语义就变了。哪怕用户问题一样，只要系统提示词、工具 schema、多轮历史、RAG 文档、special tokens 或 tokenizer 版本不同，就不能直接复用。

prefix cache 是一个受限复用。vLLM 的 Automatic Prefix Caching 复用的是相同前缀的 KV cache；它能跳过共享前缀的 prefill，但只对 prefix 完全一致或按实现规则可验证一致的部分成立。它也主要降低 prefill，不会让后续 decode 免费。

因此 KV cache 的 key 不能只用用户 id 或 prompt 字符串。它至少要绑定 model revision、tokenizer、chat template、adapter、dtype、position 规则、prefix tokens 和权限边界。跨租户或跨权限错误复用 KV，是比普通缓存旧值更严重的问题，因为它可能把上下文状态串到另一个请求里。

## Q086. KV cache 的容量、碎片和调度如何影响吞吐？

**回答：**

KV cache 占用通常跟层数、token 数、KV head 数、head dimension、数据类型和并发请求数成正比。上下文越长，生成越长，并发越高，显存里的 KV 就越大。显存不是只放权重，还要放 KV cache、activation、workspace、CUDA graph buffer。KV 空间不足时，请求要排队、抢占、换出到 CPU，或者直接被拒绝。

碎片会让问题更早出现。请求长度不同，结束时间不同，KV block 不断分配释放。如果用连续大块内存，明明总空闲显存够，却找不到连续空间。PagedAttention 把 KV 拆成固定大小 block，类似分页，目标就是降低浪费和碎片，让不同长度请求更容易共存。

调度会决定这些 block 如何被消耗。长 prompt 的 prefill 会一次性申请大量 KV；长输出的 decode 会持续增长 KV；batch 太大可能提高吞吐，但把 KV 占满后会让短请求排队。TensorRT-LLM 的 in-flight batching 和 chunked prefill 这类设计，就是为了把 context phase 和 generation phase 更细地交错起来，减少长 prompt 对队列和 TTFT 的冲击。

吞吐不只是 GPU FLOPS。KV cache 紧张时，系统可能花大量时间在调度、换页、等待 block、显存拷贝和 preemption 上。要看 `free blocks`、KV utilization、preemption count、TTFT、TPOT、batch token 数、decode iteration gap，而不是只看 GPU 利用率。

## Q087. cache locality 与负载均衡的矛盾如何建模？

**回答：**

cache locality 希望同一类请求打到已经有缓存的节点。负载均衡希望请求均匀分散到健康且空闲的节点。这两个目标经常冲突：把请求发到有缓存的节点，可能让它过载；发到空闲节点，可能 miss，触发冷加载或长 prefill。

可以把路由成本写成一个打分模型：

```text
cost(node, request) = queue_cost
                    + compute_cost
                    + miss_probability * miss_cost
                    + transfer_cost
                    + fairness_penalty
                    + risk_penalty
```

普通缓存里，miss_cost 可能是数据库查询；模型服务里，miss_cost 可能是模型权重加载、prefix prefill、KV transfer 或 checkpoint 冷启动。queue_cost 表示目标节点当前排队和 GPU/CPU 水位。公平项表示不能让某个租户或某个节点长期被热点打爆。

如果 miss_cost 远大于 queue_cost，应该偏向 locality，比如模型权重只在少数 GPU 上热着，或者 prefix 很长。 如果目标节点已经排队很深，继续追 locality 会让 p99 更差，这时应该牺牲命中，转发到空闲节点。最好的策略不是固定 sticky，也不是纯 round-robin，而是把 locality 当成路由特征之一。

面试里可以这样说：负载均衡不是只看节点 QPS，缓存调度也不是只看 hit。要比较“命中但排队”和“miss 但空闲”的总成本，并把租户公平、重试、冷启动和尾延迟放进同一个模型。

## Q088. 如何用 trace-driven simulation 比较 eviction 策略？

**回答：**

trace-driven simulation 的核心是用真实访问序列离线重放不同策略，而不是凭直觉说 LRU、LFU、TinyLFU 谁更好。真实 trace 至少要有时间、key、操作类型、对象大小、租户、miss cost 或后端类型。只有 key 序列也能比较 hit rate，但解释力会弱很多。

第一步是清洗 trace。去掉明显坏数据，统一 key 规范，区分 read/write/delete，保留时间顺序，记录对象 size 和版本。写操作、主动失效、TTL 过期要在模拟器里表达出来，否则只模拟读序列会过于乐观。

第二步是定义策略和容量。对每个候选策略，在多个容量点上重放，例如 1GB、2GB、4GB、8GB，画 miss ratio curve。还要比较 request hit rate、byte hit rate、cost-weighted hit rate、eviction count、stale risk、per-tenant hit rate。Caffeine 的 simulator 也采用多种真实 trace 和策略比较，输出 hit/miss/eviction 等指标。

第三步是做 workload 切片。整体结果可能掩盖问题。要按租户、接口、对象大小、时间段、热点 key、scan 任务、白天/夜晚流量分开看。一个策略全局提升 2%，但把小租户命中率打掉 30%，可能不能上线。

第四步是验证实现成本。离线模拟只比较策略质量，不代表线上实现快。真正上线前还要 microbenchmark 和压测，看锁竞争、CPU、内存元数据、GC、p99、更新成本。面试里要强调：trace simulation 回答“该保留哪些对象”，benchmark 回答“实现这个策略贵不贵”。

## Q089. 如何在面试中说明缓存优化的正确性边界？

**回答：**

我会先把缓存定位成派生状态，而不是 source of truth。缓存可以加速读、保护后端、复用计算结果，但它通常不应该决定最终事实。事实在哪里，要看业务：数据库、日志、事务状态机、对象存储、模型版本仓库，才是更权威的来源。

然后按数据类型说明边界。商品描述、用户头像、排行榜快照可以接受短暂旧值；权限、库存、价格、支付状态就要更谨慎。对强一致字段，缓存可以做提示或展示，但提交决策要读权威存储或做版本校验。旧值窗口要被明确接受，而不是被忽略。

再说失败边界。缓存 miss 不应该让系统无限并发回源；缓存 hit 不代表值一定新；删除缓存失败要有 TTL 或修复；双写失败要有幂等和版本；本地缓存失效通知可能丢；负缓存可能挡住新创建的数据；LLM KV cache 不能跨模型版本、tokenizer、adapter、租户和权限边界复用。

最后给工程化答案：我会为缓存定义这些不变量：key 维度正确，value 绑定版本，TTL 是兜底，source of truth 可重建，强一致路径可绕过缓存，回源有 singleflight/限流，淘汰不影响正确性，缓存命中要有 stale 监控。这样说比“加缓存提高性能”更可靠，因为它同时说明了性能收益和不能越过的正确性边界。

## 参考资料

- [Redis key eviction documentation](https://redis.io/docs/latest/develop/reference/eviction/)
- [Guava CachesExplained](https://github.com/google/guava/wiki/cachesexplained)
- [Caffeine Eviction wiki](https://github.com/ben-manes/caffeine/wiki/Eviction)
- [Caffeine Efficiency wiki](https://github.com/ben-manes/caffeine/wiki/Efficiency)
- [ARC: A Self-Tuning, Low Overhead Replacement Cache](https://www.usenix.org/conference/fast-03/arc-self-tuning-low-overhead-replacement-cache)
- [TinyLFU: A Highly Efficient Cache Admission Policy](https://arxiv.org/abs/1512.00727)
- [Go singleflight package documentation](https://pkg.go.dev/golang.org/x/sync/singleflight)
- [Microsoft Azure Cache-Aside pattern](https://learn.microsoft.com/en-us/azure/architecture/patterns/cache-aside)
- [AWS Database Caching Strategies: Cache Validity](https://docs.aws.amazon.com/whitepapers/latest/database-caching-strategies-using-redis/cache-validity.html)
- [AWS Database Caching Strategies: Caching patterns](https://docs.aws.amazon.com/whitepapers/latest/database-caching-strategies-using-redis/caching-patterns.html)
- [Redis EXPIRE command documentation](https://redis.io/docs/latest/commands/expire/)
- [RFC 2308: Negative Caching of DNS Queries](https://www.rfc-editor.org/rfc/rfc2308)
- [Redis client-side caching reference](https://redis.io/docs/latest/develop/reference/client-side-caching/)
- [Redis INFO command documentation](https://redis.io/docs/latest/commands/info/)
- [Redis Anti-Patterns: Common Mistakes Every Developer Should Avoid](https://redis.io/tutorials/redis-anti-patterns-every-developer-should-avoid/)
- [Caffeine Statistics wiki](https://github.com/ben-manes/caffeine/wiki/Statistics)
- [Caffeine Population wiki](https://github.com/ben-manes/caffeine/wiki/Population)
- [Microsoft Azure Caching Guidance](https://learn.microsoft.com/en-us/azure/architecture/best-practices/caching)
- [Reactive Streams](https://www.reactive-streams.org/)
- [A Survey of Miss-Ratio Curve Construction Techniques](https://arxiv.org/abs/1804.01972)
- [Caffeine Simulator wiki](https://github.com/ben-manes/caffeine/wiki/Simulator)
- [Caffeine Benchmarks wiki](https://github.com/ben-manes/caffeine/wiki/Benchmarks)
- [RFC 9110: HTTP Semantics](https://www.rfc-editor.org/rfc/rfc9110.html)
- [RFC 9111: HTTP Caching](https://www.rfc-editor.org/rfc/rfc9111.html)
- [MDN Cache-Control header reference](https://developer.mozilla.org/en-US/docs/Web/HTTP/Reference/Headers/Cache-Control)
- [OWASP Web Cache Poisoning](https://owasp.org/www-community/attacks/Cache_Poisoning)
- [PortSwigger Web Cache Poisoning](https://portswigger.net/web-security/web-cache-poisoning)
- [An O(1) algorithm for implementing the LFU cache eviction scheme](https://dhruvbird.com/lfu.pdf)
