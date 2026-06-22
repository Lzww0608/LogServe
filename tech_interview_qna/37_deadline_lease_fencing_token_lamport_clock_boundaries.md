# 37. deadline、lease、fencing token、Lamport clock 与 vector clock 追问链

这一组问题的共同点是“资格和时间不能靠感觉判断”。deadline 管调用方愿意等到什么时候，lease 管某个持有者在一段时间内有没有资格，fencing token 管旧持有者回来以后还能不能写，Lamport clock 管分布式事件之间能表达到什么程度的先后关系，vector clock 管系统能不能识别两个版本是因果先后还是并发分叉。它们经常一起出现，也经常被混用。面试时要把这几条线分开：等待预算不是回滚保证，租约不是互斥锁的全部语义，token 不被下游检查就没有保护力，逻辑时钟也不是物理时间。

## Q001. 面试官如果只问一个问题检验你是否理解 deadline，可能会问什么？

我觉得最有效的问题是：客户端给服务 A 的 RPC 设置了 2 秒 deadline，A 自己排队和处理已经花了 1.5 秒，现在 A 要调用服务 B。A 应该给 B 传 2 秒、500 毫秒，还是不传 deadline？如果 2 秒到了，B 还在执行，会发生什么？

这个问题能直接考出 deadline 的本质。deadline 不是某个下游服务的本地超时配置，而是一次用户请求的剩余等待预算。A 已经消耗了 1.5 秒，就不能假装预算还完整存在。继续调用 B 时，应该传递剩余预算，或者传一个更短的本地上限，但不能重新给 B 一个完整的 2 秒。否则每一层都重置超时，一次入口请求串过几层以后，用户早就放弃了，后端还在做一串已经没有价值的工作。

第二个关键点是，deadline 到了以后，调用方停止等待，不等于服务端自动回滚。B 可能还没收到请求，可能正在队列里，可能已经执行业务写入，可能正在写响应。RPC 框架可以把取消信号传到服务端，服务端也可能把调用标记为 cancelled，但业务代码必须主动检查这个取消信号，及时停止可以停止的工作。已经提交到数据库、已经发出去的消息、已经调用外部支付接口，不能因为客户端 deadline 过了就自动消失。

第三个关键点是 deadline 和 retry 绑定。第一次调用到 deadline，客户端拿到的是“不再等了”或“超过截止时间”，不是“服务端肯定没执行”。如果下一步要重试，必须先看操作是否幂等，是否有 idempotency key，是否可以查询第一次提交状态。创建订单、扣款、发消息这种请求，不能把 deadline exceeded 当成安全重发的证据。

这个问题还会看你是否理解传播方式。跨机器直接传播一个绝对时间点可能遇到时钟差，所以很多 RPC 实现会把 deadline 转换成剩余 timeout，再扣掉已经花掉的时间传下去。这样做的目标不是让所有机器时钟完全一致，而是让预算沿调用链逐步减少。

所以我会这样回答：deadline 表示调用方愿意等到什么时候。它应该沿调用链传播剩余预算；过期后客户端不再等待，框架发出取消信号，服务端业务代码要协作停止；但 deadline 不保证服务端没执行、不保证副作用回滚，也不能替代幂等和状态查询。

## Q002. deadline 的一句话定义是否容易误导，误导点在哪里？

容易误导。最常见的定义是：deadline 是请求必须完成的截止时间。这个说法看起来对，但“必须完成”四个字会带出很多误解。

第一个误解是把 deadline 当成强制终止。实际上 deadline 到了，调用方可以停止等待，框架可以取消 RPC，服务端可以收到取消信号，但业务计算、数据库事务、外部 API 调用、后台 goroutine 不一定马上停。没有主动检查 context、没有把取消信号传到子任务、没有给外部调用设置超时，deadline 就只能停在调用边界，挡不住内部继续耗资源。

第二个误解是把 deadline 当成 rollback。一个请求超过 deadline 时，业务状态可能已经改变。比如服务端已经写入任务日志，只是响应没发回来；或者 workflow step 已经完成，控制面还没来得及把结果返回给 SDK。客户端看到 deadline exceeded，只能说明它没在预算内拿到结果，不能说明操作没有生效。

第三个误解是把 deadline 和 timeout 混成一个词。timeout 通常是持续时间，比如 500 毫秒；deadline 是某个截止点。单层调用里二者可以互相换算，但多层调用里差异很大。每一层都设置“再等 500 毫秒”，预算会被放大；传递同一个截止点或剩余预算，才能让整条链路受入口请求约束。

第四个误解是把 deadline 当成 SLO。SLO 是系统对一类请求长期表现的承诺，比如 p99 在某个范围内；deadline 是单次请求的等待上限。deadline 设得比真实 p99 短，系统会大量超时和重试；设得太长，又会让无效工作堆积。它需要基于负载测试、尾延迟和业务价值来定，不是随手写一个数字。

第五个误解是忽略作用域。用户请求、内部 RPC、数据库查询、队列任务、后台 cleanup 都可以有 deadline，但它们的语义不完全一样。用户请求的 deadline 到了，可能应该停止；后台修复任务到期后，可能应该让出资源，稍后续跑；持久任务如果只因为本次 worker 的 deadline 到了就丢失，那就是正确性问题。

我更愿意把 deadline 说成：一次操作在当前调用方看来还值得等待和继续消耗资源的边界。它控制等待和取消传播，不直接证明远端状态，也不替业务设计幂等、补偿和持久状态机。

## Q003. deadline 最常见的生产事故触发条件是什么？

最常见的事故是没有 deadline。客户端、网关、worker、数据库查询、外部 API 调用都可能一直等下去。上游已经断开，下游还占着连接、线程、goroutine、内存和锁。故障初期只是少量请求慢，后来慢请求堆积，连接池耗尽，队列变长，健康请求也被拖死。

第二类是 deadline 设得过短。服务端正常 p99 需要 800 毫秒，客户端却设置 300 毫秒。结果是大量请求其实能完成，但客户端提前放弃并重试。重试又增加下游压力，压力让延迟更高，延迟更高又触发更多 deadline exceeded。这种事故看起来像服务端不可用，根因可能是客户端预算和真实尾延迟不匹配。

第三类是不传播 deadline。入口请求有 2 秒预算，服务 A 调 B、B 调 C 时每一层都重新给 2 秒。用户 2 秒后已经拿到失败，C 还在跑，甚至继续写库和发消息。排障时会看到一堆“请求已取消后仍在执行”的后台负载。对 workflow 或任务系统来说，这还会让一次逻辑操作在多个 attempt 之间重叠。

第四类是传播了取消信号，但业务代码不检查。Go 里拿到了 context，却只在函数入口检查一次；循环、批量扫描、外部命令、Python subprocess、模型加载、对象存储上传都没有中途检查。deadline 到了以后，表面上 RPC 已经结束，实际工作还在后台继续跑。

第五类是 deadline 和副作用没有配套。客户端在 deadline 后重试创建资源，服务端没有 idempotency key；worker 在 step deadline 后又上报结果，控制面没有 epoch 或 attempt 校验；外部 API 调用超时后直接再次提交，后面发现扣款、发券、消息都重复了。deadline 本身不懂业务副作用，必须和幂等、状态查询、fencing 一起设计。

第六类是嵌套 retry 吃光预算。一次请求有 1 秒 deadline，中间几层各自做重试和 backoff，没有检查剩余时间。最后一个 attempt 发出去时只剩几十毫秒，必然失败，却还占用了下游资源。更糟的是，每层日志都显示“我只重试了两次”，没人看到整条链路的放大倍数。

所以 deadline 的生产事故通常不是“超时错误太多”这么简单，而是等待预算、取消传播、业务副作用和重试策略没有一起建模。

## Q004. deadline 的指标应该怎么设计才不会只看平均值？

deadline 的指标要围绕“预算从哪里花掉了”和“过期后还有多少无效工作”来设计。平均延迟没有太大价值，因为 deadline 问题通常发生在尾部和跨层传播上。

第一组是 deadline 配置分布。要看每个 endpoint、RPC method、任务类型、客户端版本设置的 deadline：是否缺失、是否过短、是否过长，p50、p95、p99 分别是多少。很多事故不是服务端突然变慢，而是某个 SDK 版本把默认 timeout 改短，或者某条内部链路没有设置 deadline。

第二组是剩余预算。服务端收到请求时，要记录 incoming remaining budget；发起下游调用时，要记录 outgoing remaining budget；请求结束时，要记录 unused budget 或 overrun。这样才能看出时间花在入口排队、业务执行、下游依赖、重试 backoff，还是客户端一开始就给了不现实的预算。

第三组是过期原因。deadline exceeded 要按 caller、callee、method、依赖、队列阶段拆分。要区分还没开始处理就过期、处理中被取消、下游调用过期、等待锁过期、等待连接池过期、重试耗尽后过期。一个总的 deadline_exceeded_count 只能告诉你系统在流血，不能告诉你伤口在哪里。

第四组是取消协作效果。要看 cancellation observed latency，也就是框架发出取消到业务停止之间隔了多久；还要看 canceled-but-still-running 数量、cancel 后继续消耗的 CPU/IO、cancel 后仍提交成功的副作用、goroutine 或 subprocess 泄漏。deadline 真正保护资源，靠的是这组指标。

第五组是尾延迟和预算命中率。要看 end-to-end latency 的 p95、p99、max，也要看 first-attempt latency、retry-added latency、queue wait、processing time。deadline exceeded 的比例还要和真实成功率一起看：如果大量请求在 deadline 后其实成功提交，说明客户端和服务端的结果收敛设计有问题。

第六组是链路放大。要看一次 logical request 产生了多少下游 attempts、多少 attempts 在发出时剩余预算已经很低、多少 retries 因 deadline 被跳过、多少下游请求在上游放弃后仍完成。这个指标能抓住“用户只发了一次请求，系统内部跑了很多无效工作”的问题。

我会把 deadline 面板设计成四条线：入口预算是否合理，预算在哪一层被消耗，取消是否真的停住工作，过期后有没有重复副作用。只看平均响应时间，基本看不到这些问题。

## Q005. deadline 的正确性边界和性能边界分别是什么？

deadline 的正确性边界是：它只定义调用方等待和继续执行的边界，不定义远端操作是否发生。这个边界必须说清楚，否则系统会把超时当成失败确认，把重试当成无害动作。

对无副作用读请求来说，deadline 到了通常可以直接放弃，最多损失一次查询结果。对有副作用写请求来说，deadline 到了以后结果可能是未知的：请求可能没到服务端，也可能已经提交成功。正确做法通常是使用 idempotency key、业务唯一键、状态查询、事务日志或任务状态机，让后续重试或查询收敛到同一个结果。

服务端取消也是协作式边界。RPC 框架可以把 context 标记为 done，但业务代码要把这个信号传给数据库、对象存储、外部命令、子 goroutine 和下游 RPC。对于不可中断的临界区，deadline 不能强行打断，只能阻止后续阶段继续扩散。已经进入提交点的操作，要么完成并记录结果，要么进入可恢复的 unknown 状态，不能悄悄丢。

性能边界是：deadline 可以减少无效等待和资源占用，但设得太紧会降低成功率、制造重试风暴；设得太松会让故障请求占住资源。一个好的 deadline 应该来自真实延迟分布和业务价值：用户交互请求通常预算短，后台恢复任务可以长一些；扇出调用要给每个子调用分配预算，而不是让所有子调用各自拿完整时间。

在 LogServe 这类系统里，workflow step、worker poll、LLM 调用、result upload 都可以有 deadline，但它们不能替代持久状态机。worker 本次执行超时了，控制面可以重新调度；如果旧 worker 后来上报结果，还要靠 attempt、epoch、idempotency key 或 fencing 来决定是否接受。deadline 负责节制资源，正确性要靠日志和版本边界兜住。

所以我会总结：deadline 的正确性边界是“不等了，不等于没发生”；性能边界是“用有限等待减少无效工作，但不能把正常尾延迟切成失败和重试”。

## Q006. 面试官如果只问一个问题检验你是否理解 lease，可能会问什么？

我会期待这个问题：worker A 拿到了某个任务或资源的 lease，TTL 是 30 秒。A 执行过程中发生长时间 GC pause 或机器卡住，超过 30 秒没有续约。控制面把 lease 发给 worker B。几秒后 A 恢复，继续把任务完成结果写回来。控制面和下游存储应该接受 A 的结果吗？

这个问题能直接区分“知道 lease 是什么”和“知道 lease 的坑在哪里”。lease 是带时间边界的资格，不是永久所有权。A 在 30 秒内有资格工作，是因为租约授予方认为它还活着、还在续约、还没超过租约窗口。A 超过 TTL 后没有成功续约，系统为了活性可以把资格交给 B。但这不代表 A 这个进程已经真的停止，也不代表它发出的旧请求不会晚到。

正确答案是：不能只因为 A 曾经拿到过 lease 就接受它后来的写入。写完成结果时，A 必须带上本次 lease 对应的 epoch、generation 或 fencing token。控制面或下游存储要检查这个 token 是否仍然是当前有效的 token。如果 B 已经拿到了更高 epoch，A 的旧 epoch 写入就应该被拒绝，或者至少不能覆盖 B 的结果。

这里最容易犯错的是把 lease 当成本地互斥锁。单进程里的 mutex 解锁以后，旧线程不会在另一个宇宙里继续拿着锁写共享状态；分布式系统里，旧 holder 可能只是被暂停、网络包被延迟、响应丢了、时钟慢了。lease 解决的是“持有者崩溃后不要永久卡住”，不自动解决“旧持有者复活后还在写”。

这个问题还会考 TTL 的选择。TTL 太短，正常抖动、GC、调度延迟、网络波动都会造成误失效和频繁抢占；TTL 太长，真实故障后的 failover 很慢。续约间隔、心跳抖动、租约授予方的时钟、网络延迟、存储提交延迟都要一起考虑。

放到 LogServe 里，可以用 task lease 或 actor ownership 来回答。worker poll 到任务后获得一个 lease 或 epoch，执行完成时必须带着这个版本回来。控制面如果已经把任务 redeliver 给新 worker，旧 worker 的完成事件不能直接覆盖新状态。否则 fault injection 里 worker kill recovery 看起来能恢复，实际会在长 pause 场景下接受 stale completion。

所以我会说：lease 负责给活着的持有者一段时间的资格，但它不是旧持有者停止工作的证明。只要结果会影响正确性，就必须用 epoch 或 fencing token 把旧 lease 的写入挡在状态机外。

## Q007. lease 的一句话定义是否容易误导，误导点在哪里？

容易误导。常见定义是：lease 是有过期时间的锁。这个说法方便记忆，但会让人把两个概念混得太近。

第一个误导点是把 lease 当成互斥锁。lease 可以用于实现某种互斥资格，但它本身只是授予方在一段时间内承认某个 holder 的资格。这个资格是否能保护资源，还取决于资源写入方是否检查 holder 的 epoch 或 token。没有下游检查，lease 过期前后都可能被旧 holder 写穿。

第二个误导点是把过期当成事实停止。lease 过期只能说明租约服务没有在 TTL 内收到有效续约，不能说明 holder 已经停止执行。holder 可能被暂停，可能和租约服务断网但和存储还通，可能已经把写请求发出只是响应延迟。分布式系统里，“我认为你过期了”和“你不会再产生影响”不是同一句话。

第三个误导点是忽略时间来源。lease 依赖时间窗口，但不同实现的时间语义不同。有的 TTL 由租约服务端判断，有的客户端也会本地判断剩余时间；有的系统用单调时钟，有的会被系统时间跳变影响；有的 lease 建在共识存储上，有的只是缓存里的过期 key。时间边界越弱，越不能把 lease 当成正确性屏障。

第四个误导点是忽略续约协议。lease 不是拿到一次就完事，它要 keepalive、renew、revoke、expire，还要处理续约请求和业务写入之间的竞态。客户端看到续约成功响应之前，是否能继续做不可逆操作？续约请求超时但服务端其实成功了，应该怎么判断？这些才是工程里的难点。

第五个误导点是忽略用途差异。Kubernetes 用 Lease 对象做 node heartbeat 和 leader election；etcd 的 lease 可以挂到 key 上，过期或撤销时删除这些 key；任务队列里的 lease 常用于 visibility timeout 和 redelivery。这些都叫 lease，但正确性要求不一样。节点心跳偏向故障检测，任务 lease 偏向重投递，leader lease 如果要保护写入就要配 fencing。

我会把 lease 定义成：由一个可信授予方发放的、需要续约的临时资格。它可以帮助系统在持有者失联后继续前进，但不能单独证明旧持有者不会再写，也不能替代下游的版本检查。

## Q008. lease 最常见的生产事故触发条件是什么？

最常见的是旧 holder 复活后继续写。进程长时间暂停、机器被抢占 CPU、磁盘卡顿、GC stop-the-world、网络包延迟，都可能让 holder 错过续约。系统把 lease 转给新 holder 后，旧 holder 又恢复执行。如果写路径没有 fencing，两个 holder 就会同时认为自己有资格，或者旧结果覆盖新结果。

第二类是 TTL 配置不合理。TTL 太短，正常抖动也会触发失效，leader election 抖动、任务重复执行、队列 redelivery 增多；TTL 太长，真实故障后切换慢，资源长时间没人处理。很多事故发生在部署变更后：延迟分布、GC 行为、网络路径变了，但 lease TTL 仍然按旧环境设置。

第三类是续约和业务写入没有绑定。客户端续约成功后开始写，但写请求到达时 lease 可能已经被别人抢走；或者续约请求超时，客户端以为失败，实际服务端已经续上；再或者客户端本地认为 lease 还没过期，但授予方已经撤销。只有把写入和当前 epoch/token 校验放在同一个状态更新里，才能降低这些竞态。

第四类是网络分区。holder 可能连不上租约服务，却还能连上下游存储；也可能能连租约服务，连不上其他依赖。租约服务根据自己的观察发出新 lease，下游存储如果不认 token，就会接受来自两个世界的写入。单靠“租约过期”无法覆盖所有网络拓扑。

第五类是时间跳变和时钟假设。客户端用墙钟判断 lease 是否还有效，NTP step、虚拟机暂停恢复、容器迁移都可能让时间突然跳。即使租约服务端用自己的时间判断，客户端也不能把本地时间剩余量当成强正确性依据。时间可以用于 failure detection，不能单独承担 safety。

第六类是把 lease key 的删除语义理解错。像 etcd 这类系统里，lease 过期会删除挂在 lease 上的 key，并产生 delete event。如果业务把这些 key 当作长期状态，或者没有处理过期删除事件，下游视图就会突然丢数据。lease key 更适合表达临时存在、成员身份、session 或候选资格，不适合承载没有恢复来源的唯一状态。

所以 lease 事故的根因通常不是“租约服务坏了”，而是系统把一个临时资格机制当成了完整互斥、完整正确性或完整生命周期管理。

## Q009. lease 的指标应该怎么设计才不会只看平均值？

lease 的指标要看租约生命周期和边界窗口。平均续约延迟只能说明平时还行，真正危险的是 TTL 剩余量的尾部、过期前后的竞态和旧 holder 写入。

第一组是授予和续约指标。要看 lease grant latency、renew latency、keepalive success/error、renew interval jitter、revoke latency、expire count。所有这些都要按资源类型、holder、租户、leader election group、任务队列分开。一个全局平均 renew latency 没有意义，某个热点资源的续约抖动才会引发抢占。

第二组是 TTL 水位。要看 observed TTL remaining 的 p50、p95、p99、min，尤其是续约完成时剩余 TTL 还有多少。只要 min 经常贴近 0，说明系统在失效边缘运行。还要统计续约发起时间与 TTL 的比例，比如总是到最后 90% 才续约，就很容易被一次抖动击穿。

第三组是 ownership transition。要看 leader transition count、task lease transfer count、同一资源连续抢占次数、旧 holder 到新 holder 的交接延迟、过期后多久发出新 lease、主动释放和 TTL 过期的比例。频繁 TTL 过期通常比主动释放更危险，因为它说明系统靠失败检测在推进。

第四组是 stale action 指标。要看旧 epoch 完成被拒绝次数、旧 holder 写入尝试、lease expired but work still running、fencing reject count、same-resource concurrent holder observation。没有这组指标，lease 看起来都续得很好，实际下游可能已经被旧请求污染。

第五组是时间和环境指标。进程 pause time、GC pause、调度延迟、网络 RTT、租约服务端 apply latency、存储写延迟都要和 lease TTL 放在同一张图里看。TTL 如果是 10 秒，而 p99.9 pause 偶尔到 8 秒，系统已经很危险。只看 lease 服务自己的指标会漏掉客户端暂停。

第六组是恢复效果。任务系统要看 lease 过期后的 redelivery latency、重复执行率、最终只接受一次结果的比例；leader election 要看无主窗口和双主防护；成员心跳要看 false positive 和 false negative。lease 的价值不在“续约平均多快”，而在故障时能不能既快恢复又不接受旧写。

我会把 lease 面板做成三块：TTL 余量是否健康，ownership 切换是否抖动，旧 holder 行为是否被挡住。只看 keepalive 平均延迟，基本等于没看 lease 风险。

## Q010. lease 的正确性边界和性能边界分别是什么？

lease 的正确性边界是：授予方在自己的观察和时间模型下，暂时承认某个 holder 有资格；它不保证旧 holder 已停止，也不保证所有下游资源都知道这个资格变化。这个边界决定了 lease 不能单独保护关键写入。

如果租约服务建立在强一致存储或共识协议上，它可以较可靠地决定“当前记录里谁是 holder”。但 holder 的业务请求可能绕过租约服务直接到达下游存储。下游如果不检查 epoch 或 fencing token，就无法区分当前 holder 和过期 holder。正确性要求高的场景，lease grant、epoch 增长、状态写入接受条件必须形成一条闭环。

lease 对 liveness 很有用。没有 TTL，持有者崩溃后资源可能永久卡住；有了 lease，系统能在心跳停止后重新授予资格。它牺牲的是某些情况下的重复执行或误抢占风险。真正成熟的设计会承认这一点：lease 用于发现疑似失联，fencing 用于挡旧写，幂等用于收敛重复执行。

性能边界主要是 TTL 和续约负载。TTL 短，故障恢复快，但续约频繁、误失效多、租约服务压力大；TTL 长，续约压力低，误判少，但故障切换慢。leader election、任务 lease、节点 heartbeat、临时 key session 的最佳 TTL 不一样，不能共用一个全局值。

续约路径本身也会成为瓶颈。大量 worker、actor、task、model cache session 都需要 keepalive 时，租约服务的写放大、watch 通知、存储 apply latency 会影响整个系统。需要批量续约、抖动续约时间、分片资源、限制热点 key，避免所有 holder 同时续约或同时过期。

在 LogServe 里，lease 更适合作为“任务可以被重新分配”的活性机制，而不是最终写入资格本身。task lease 过期后可以 redeliver；actor ownership 可以换 epoch；旧 worker 的完成必须通过 epoch/fencing 检查。这样 lease 负责让系统继续跑，日志和版本检查负责不让旧结果污染状态。

一句话：lease 的正确性边界到“临时资格”就停了，关键写入要靠 fencing；它的性能边界在 TTL、续约频率、切换延迟和租约服务负载之间取平衡。

## Q011. 面试官如果只问一个问题检验你是否理解 fencing token，可能会问什么？

最能检验理解的问题是：客户端 A 拿到锁或 lease，token=33，然后暂停很久。锁过期后客户端 B 拿到新 token=34，并写入共享存储。后来 A 恢复，把带着 token=33 的写请求发到共享存储。共享存储应该做什么？

答案很明确：共享存储必须拒绝 token=33 的写入，因为它已经接受过更高 token=34 的写入，或者当前资源的有效 token 已经推进到 34。fencing token 的核心就在这里：不相信旧 holder 自己会停止，而是在资源写入点检查“这个写入是不是来自足够新的授权”。

这个问题比“什么是 fencing token”更好，因为它会暴露两个常见误区。第一，发 token 的锁服务不是最后一道防线。锁服务可以告诉 B 获得了更高 token，但真正能阻止 A 污染数据的是共享存储。第二，token 必须单调递增。随机 UUID、随机 lock value、客户端本地时间戳都不能提供“新授权一定大于旧授权”的性质。

fencing token 的比较要在资源的原子更新边界内完成。比如某个对象、某行记录、某个 actor 状态保存了 last_accepted_token。写入时带 token，存储执行条件更新：只有 token 大于当前 last_accepted_token 时才接受，并把 last_accepted_token 推进。不能先读 token、业务处理、再写数据，中间被其他 holder 插入就会出竞态。

还要说清 token 的作用域。一个全局递增 token 可以用于所有资源，但可能成为瓶颈；per-resource token 扩展性更好，但不同资源之间不能比较。面试中要主动问：token 是按锁、按 actor、按 shard、按任务，还是按整个系统分配？作用域错了，比较结果就没有意义。

放到 LogServe 的 actor 里，epoch fencing 就是这个思路。旧 worker 可能曾经拥有 actor，但新的 ownership epoch 已经授予出去。旧 worker 再提交 ActorCommandApplied 或状态变更时，控制面要检查 owner_worker_id 和 epoch，不能只看“这个 worker 曾经拿到过 actor”。

所以我会回答：fencing token 是随授权单调增长的写入资格号。它必须由可信授予方生成，并由实际资源在写入时原子检查。没有下游检查，token 只是日志字段，不是保护机制。

## Q012. fencing token 的一句话定义是否容易误导，误导点在哪里？

容易误导。常见定义是：fencing token 用来防止旧 leader 或旧锁持有者写入。这个定义方向对，但容易让人以为“发了 token 就安全”。

第一个误导点是忽略下游执行。fencing token 的安全性来自资源服务器拒绝旧 token，而不是来自客户端自觉。旧 holder 恢复后可能不知道自己过期了，也可能知道但代码有 bug，还可能有之前已经发出去的网络请求晚到。只有下游每次写入都检查 token，才能把旧请求挡住。

第二个误导点是把 token 当成普通请求 ID。请求 ID 用来追踪或去重，通常不要求单调；fencing token 必须随着每次授权递增。随机值能证明“释放锁时是我自己的锁”，但不能证明“这个写比之前授权更新”。如果不能比较大小，就无法拒绝旧 holder。

第三个误导点是把 token 当成物理时间。时间戳看起来递增，但机器时钟会跳，跨节点时钟不一致，网络延迟也会让旧请求晚到。fencing token 更像由共识点、事务序列、数据库版本、ZooKeeper sequence/zxid 或控制面 epoch 产生的逻辑顺序，不应该依赖客户端墙钟。

第四个误导点是忽略持久性。授予方重启后如果 token 计数从 1 开始，旧 token 和新 token 就可能重复或倒退。下游存储如果不持久化 last accepted token，重启后也可能忘记已经接受过更高授权。token 的单调性和检查结果都要跨故障保持。

第五个误导点是忽略作用域。一个 token 只在定义的资源范围内有意义。actor A 的 epoch 不能拿来和 actor B 的 epoch 比；task attempt token 不能直接保护某个外部支付订单；全局 token 可以比较，但会引入中心化成本。面试里不说作用域，答案通常不可靠。

我会把 fencing token 定义成：由授权方在某个资源作用域内单调发放，并由资源写入方持久、原子检查的资格版本。它保护的是旧授权写入，不是通用去重，也不是锁服务自己的装饰字段。

## Q013. fencing token 最常见的生产事故触发条件是什么？

最常见的是 token 只在控制面里存在，下游不检查。leader election 成功后拿到了 epoch，日志也打印了 epoch，但写数据库、写对象存储、调用外部 API 时没有把 epoch 带过去，或者带过去以后对方只是记录一下。旧 leader 恢复后照样能写，事故发生时团队才发现 fencing 没有闭环。

第二类是 token 不单调。某些实现用随机 UUID、Redis lock value、客户端时间戳、本地自增计数当 fencing token。随机值不能比较新旧；本地计数在多节点之间会重复；时间戳会受时钟跳变影响；进程重启后计数可能回退。这样的 token 可以做身份校验，但不能做 fencing。

第三类是 token 发放不持久。锁服务或控制面重启后忘记最大 token，重新发出较小 token。下游如果已经接受过较大 token，就会拒绝所有新写；如果下游也忘了最大 token，就可能接受旧写。两边任一边丢状态，都会把 fencing 语义打破。

第四类是检查不是原子的。写入流程先读取 last_token，发现当前 token 更大，然后执行业务处理，最后写入状态。中间另一个更高 token 的 holder 已经提交成功，当前旧请求最后又覆盖了它。正确做法是把 token 比较和状态更新放到同一个事务、条件写或 compare-and-swap 里。

第五类是比较条件写错。有的系统接受 token >= last_token，结果同一个 token 的重复请求可以覆盖不同 payload；有的系统只检查 actor epoch，不检查 owner id 或 command sequence；有的系统把 per-resource token 当全局 token 比。比较规则看起来很小，实际会直接决定旧写能不能进来。

第六类是外部副作用无法 fencing。你可以让自己的数据库拒绝旧 token，但外部邮件、短信、支付、第三方 API 不一定支持带 token 条件写。对这些副作用，只靠 fencing 不够，还要用 idempotency key、业务唯一键、状态查询和补偿流程。

所以 fencing token 的事故往往不是理论没懂，而是闭环断在某一段：授予不单调、持久性丢失、下游不检查、检查不原子，或者作用域对不上。

## Q014. fencing token 的指标应该怎么设计才不会只看平均值？

fencing token 的指标要证明两件事：token 是否严格向前推进，旧 token 是否真的被挡住。平均写延迟和平均锁获取时间都不是核心。

第一组是发放指标。要按资源作用域统计 token grant count、current token、grant latency、token regression、duplicate token、grant failure。token regression 必须是高优先级告警，因为它直接说明授予方的单调性出问题。对于 per-resource token，要能看到热点资源的发放频率，而不是只看全局总数。

第二组是下游检查指标。每个受保护资源都要统计 writes with token、writes without token、accepted token、rejected stale token、rejected duplicate token、token comparison errors。写请求没有 token 不应该悄悄走旁路；旁路写入越多，fencing 保护越像摆设。

第三组是 last accepted token 水位。要能按 resource 查看 last_accepted_token、最近接受时间、最近拒绝时间、拒绝 token 与当前 token 的差距。差距很大说明有很旧的 holder 或延迟请求还在回来；差距接近 1 且频繁出现，可能是正常切换边界，也可能是 lease TTL 太短导致抢占抖动。

第四组是竞态窗口指标。要看 lease expired to stale write delay、new token grant to first accepted write、old holder resume count、stale completion rejected after redelivery、leader transition 后旧 leader 写入尝试。fencing 主要防的是这些窗口，窗口不被观测，就很难证明机制真的生效。

第五组是原子检查失败指标。数据库条件更新失败、CAS conflict、transaction retry、compare version mismatch、external sink reject，都要单独记录。它们不是普通失败，很多时候正是 fencing 在阻止旧写。告警策略不能把这类拒绝全当异常，也不能完全忽略它们。

第六组是覆盖率指标。哪些写路径已强制 token，哪些路径仍是 best effort，哪些外部副作用无法检查 token，需要有清单和运行时计数。生产事故经常从一个“临时管理接口”“修复脚本”“旧版本 worker”绕过 token 开始。

我会把 fencing 面板做成三块：token 有没有倒退，旧写有没有被拒绝，所有关键写路径有没有强制检查。只看平均锁获取时间，根本看不出 fencing 是否有效。

## Q015. fencing token 的正确性边界和性能边界分别是什么？

fencing token 的正确性边界是：在一个明确资源作用域内，所有会改变该资源状态的写入，都必须携带由可信授权方单调发放的 token；资源方必须用持久状态原子拒绝低于当前水位的 token。少任何一段，语义都不完整。

它能解决的是旧 holder 写入问题。比如旧 leader、旧 worker、旧 lease holder 恢复后继续发请求，只要 token 较小，就会被资源方拒绝。它不能自动解决所有并发问题：同一个 token 下的重复请求要靠幂等和 command sequence；不同资源之间的事务一致性要靠事务或日志；外部系统不支持条件写时，token 只能停在你自己的边界内。

token 的顺序也要解释清楚。它表达的是授权顺序，不一定表达真实时间顺序，也不一定表达业务事件的因果顺序。token=34 的 holder 比 token=33 的 holder 更新，是因为授权方这么排序；不是因为它在物理世界里一定更早或更晚开始执行。

性能边界主要来自发放和检查。全局 token 简单，但会让所有授权经过一个中心点；per-resource token 扩展性更好，但需要管理更多水位和作用域。每次写入都做条件更新，会增加存储延迟和冲突重试。热点资源频繁换 owner 时，token 发放、watch 通知、条件写失败都会变成可见成本。

还有一个性能边界是故障恢复速度。更积极的 lease 过期和重授予会产生更多 fencing reject，这是安全机制在工作，但也说明系统正在重复执行或频繁抢占。更保守的 TTL 减少 reject，却会让真实故障恢复更慢。fencing 让你能安全地恢复，不代表恢复没有成本。

在 LogServe 里，epoch fencing 的边界应该放在 actor ownership、task completion、workflow step result 这类状态提交点。它可以拒绝旧 worker 的结果，但不能证明 worker 没有消耗 CPU、没调用外部 API、没产生日志。资源消耗要靠 deadline、lease、取消和幂等共同控制。

一句话：fencing token 保护的是状态提交边界，前提是单调、持久、作用域清楚、下游强制检查；它的成本是额外协调、额外条件写和热点资源上的冲突。

## Q016. 面试官如果只问一个问题检验你是否理解 Lamport clock，可能会问什么？

我觉得面试官最可能问：事件 A 的 Lamport timestamp 是 10，事件 B 的 Lamport timestamp 是 12。你能不能断定 A happens-before B？如果不能，Lamport clock 到底保证了什么？

正确回答是：不能只因为 10 小于 12 就断定 A happens-before B。Lamport clock 保证的是单向性质：如果 A happens-before B，那么 C(A) 一定小于 C(B)。反过来不成立。两个并发事件也可以被分配成 10 和 12，只是因为不同进程本地计数、消息接收和 tie-break 规则让数字有大小。

这个问题能把 Lamport clock 的核心讲出来。它不是物理时钟，而是一个逻辑计数器。进程内部每发生一个事件，计数器递增；发送消息时把当前计数带上；接收消息时，本地计数更新为 max(local, received) + 1。这样如果一条消息把因果关系从一个进程传到另一个进程，接收事件的时间戳就会大于发送事件。

但 Lamport clock 不能识别并发。A 和 B 如果没有进程内顺序，也没有消息链把二者连起来，它们就是并发事件。Lamport timestamp 仍然会给它们一个大小，有时还会用进程 ID 打破平局，形成全序。这个全序对日志排序、请求排队、冲突裁决很有用，但它是人为扩展出来的顺序，不代表真实因果。

这也是它和 vector clock 的差异。vector clock 可以在很多场景下判断两个事件是否并发，因为它保留了每个进程的进度向量；Lamport clock 只有一个标量，开销小，但信息少。面试时如果把 Lamport clock 说成“能判断事件先后”，就要补一句：它只能保证因果先后会反映为时间戳大小，不能从时间戳大小推出因果先后。

放到系统设计里，Lamport clock 适合做轻量逻辑顺序，比如给事件一个可重复排序，或让消息接收后推进本地逻辑时间。它不适合直接当真实时间、租约过期依据、跨节点 freshness 证明，也不适合单独解决并发写冲突。

所以我会回答：Lamport clock 的核心保证是 happens-before implies smaller timestamp。它把因果关系编码进单调计数，但它给出的大小关系比真实因果更强，里面包含人为排序，不能反向推理。

## Q017. Lamport clock 的一句话定义是否容易误导，误导点在哪里？

容易误导。常见定义是：Lamport clock 是分布式系统里的逻辑时钟，用来给事件排序。问题出在“排序”这个词太宽。

第一个误导点是把逻辑时间当成物理时间。Lamport timestamp 不是 10 点、12 点这种真实时间，也不表示事件之间隔了多久。timestamp 从 10 到 12，只说明逻辑计数推进了两步或经过了某些合并规则，不说明物理时间过去了 2 秒、2 毫秒，甚至不说明 A 和 B 在真实世界里谁先被用户触发。

第二个误导点是把全序当成因果序。Lamport clock 可以配合进程 ID 把所有事件排成一个确定顺序，但这个顺序只是为了让系统有一致裁决。两个并发写入被排成 A before B，并不代表 A 因果上影响了 B。把这个顺序当成真实因果，就会在冲突处理、审计解释和用户语义上犯错。

第三个误导点是忽略反向不成立。Lamport 规则保证 A 发生在 B 之前时，C(A) 小于 C(B)。但 C(A) 小于 C(B) 时，A 可能 happens-before B，也可能和 B 并发。这个细节是面试里最容易被追问的点。

第四个误导点是把它当成冲突检测工具。Lamport clock 可以帮助决定一个 deterministic winner，比如 timestamp 大的赢；但它不能告诉你两个更新是不是并发冲突。要识别并发，通常需要 vector clock、版本向量、因果元数据，或者业务层条件更新。

第五个误导点是忽略作用域和持久性。每个进程或节点的逻辑计数要随事件推进，接收消息时要合并远端值。进程重启后如果计数回退，或者消息里没有带 clock，逻辑时间就会断。Lamport clock 不是随便在内存里放一个 int 就完了，关键路径上要清楚它在哪些事件上递增，哪些消息携带它，是否需要持久化。

我会把它定义得更窄：Lamport clock 是一种标量逻辑计数规则，用来让因果相关事件满足时间戳递增；它可以导出确定全序，但不能把时间戳大小等同于真实因果、真实时间或并发检测。

## Q018. Lamport clock 最常见的生产事故触发条件是什么？

最常见的事故是拿 Lamport timestamp 做 last-write-wins，然后把并发更新误当成有先后关系。两个节点在没有通信的情况下分别修改同一份配置，timestamp 一个是 100，一个是 101。系统让 101 覆盖 100，看起来有确定结果，但这只是人为裁决，不说明 101 的作者看到了 100 的修改。用户可能丢失更新，审计时还会误以为后者基于前者。

第二类是把 Lamport clock 当成 freshness。比如缓存项、leader 心跳、租约判断、任务超时，如果只看逻辑 timestamp 大小，就可能误判新旧。Lamport clock 只表达因果传播，不表达真实时间经过多久。一个节点长时间停顿后恢复，逻辑 clock 仍可能很小；另一个节点高频内部事件会把 clock 推得很大，但它不一定拥有更新鲜的业务数据。

第三类是实现规则不完整。发送消息没带 clock，接收消息没有执行 max(local, remote) + 1，只在本地事件递增，跨进程因果关系就断了。表面上所有事件都有 timestamp，实际不能满足 happens-before 的基本条件。

第四类是重启后计数回退。进程内存里的 logical clock 如果不持久化，也不从本地日志、快照、已提交事件或远端消息恢复，重启后可能从 0 开始。旧事件 timestamp 比新事件还大，排序、去重、冲突裁决都会乱。是否需要持久化取决于作用域，但不能默认无所谓。

第五类是 tie-break 规则不稳定。两个事件 Lamport timestamp 相同，需要用 process id、node id、stream id 之类的稳定字段打破平局。如果进程 ID 重用、节点 ID 不稳定、排序规则在版本升级中变化，同一批事件在不同节点重放时可能得到不同顺序。

第六类是把不同作用域的 clock 混着比较。per-actor Lamport clock、per-stream logical clock、全局 sequencer 产生的序号，语义不同。actor A 的 clock=100 和 actor B 的 clock=90 未必可比；拿它们做全局 freshness 或全局审计顺序，会制造假结论。

在 LogServe 里，Lamport clock 或 command_seq 可以帮助表达 actor mailbox 内部顺序，但不能替代 actor ownership epoch，也不能判断 worker 是否仍持有 lease。一个解决顺序，一个解决资格，混用就会出事。

所以 Lamport clock 的生产事故常来自两个方向：一是把它的保证用反了，从 timestamp 大小推因果；二是实现上没有把发送、接收、重启和 tie-break 的规则保持一致。

## Q019. Lamport clock 的指标应该怎么设计才不会只看平均值？

Lamport clock 的指标不像延迟那样看平均值，它更像协议不变量检查。重点是有没有回退、有没有漏合并、有没有把不可比较的作用域拿来比较。

第一组是单调性指标。每个 clock scope 都要能检查 local clock regression、event timestamp <= previous timestamp、restart after max timestamp、duplicate timestamp without stable tie-break。任何回退都应该被记录，因为它直接破坏逻辑顺序。

第二组是消息合并指标。接收消息时要统计 remote clock、local before、local after，验证 local after 是否大于 remote 和 local before。可以抽样记录 merge violations，也可以在测试和 debug 模式下强校验。很多实现 bug 就藏在“收消息时忘了 max+1”。

第三组是作用域指标。要记录 clock_scope，比如 global、stream、actor、node、client session。跨 scope 比较的次数、缺少 scope 的事件、scope 为空的消息，都应该暴露出来。Lamport clock 一旦脱离作用域，数字再漂亮也没意义。

第四组是 tie-break 指标。相同 logical timestamp 的事件数量、使用的 tie-break 字段、排序规则版本、同一批事件在不同副本上的排序一致性，都可以作为检查项。全序是人为扩展出来的，必须保证所有副本用同一套规则。

第五组是冲突和覆盖指标。如果系统用 Lamport timestamp 做冲突裁决，要统计 concurrent-looking writes、LWW overwrite count、用户可见覆盖、被业务条件写拒绝的更新、需要人工合并的冲突。timestamp 大的赢不代表没有冲突，指标要把这种“被排序掩盖的并发”暴露出来。

第六组是和物理时间的辅助对照。可以看 logical clock jump rate、每秒逻辑事件数、logical clock 与 wall clock 的松散关系，但这只能帮助排障，不能当正确性判断。比如某个节点 clock 增长异常快，可能是消息风暴或内部循环；增长慢，可能是停顿或没有收到消息。

我会把 Lamport clock 的监控重点放在不变量上：每个作用域内不回退，接收远端消息后正确推进，排序 tie-break 稳定，冲突裁决没有被误解释成真实因果。平均 timestamp 没有工程意义。

## Q020. Lamport clock 的正确性边界和性能边界分别是什么？

Lamport clock 的正确性边界很清楚：如果事件 A happens-before 事件 B，那么 A 的 timestamp 必须小于 B 的 timestamp。它能保证因果关系不会被排成反方向，但不能保证 timestamp 小的一定 causally before timestamp 大的。

这条边界决定了它适合做什么。它适合让消息传递后的事件获得更大的逻辑时间，适合给日志和事件一个确定排序，适合在没有物理时钟同步的情况下表达最基本的因果约束。它不适合判断两个事件是否并发，不适合表达真实时间差，不适合做租约过期，不适合替代版本向量或事务隔离。

如果系统需要知道“这两个更新是不是互相没看见”，Lamport clock 不够。需要 vector clock、版本向量、因果上下文，或者直接通过数据库条件更新和业务合并来处理。如果系统只需要“给事件排一个所有副本都同意的顺序”，Lamport clock 加稳定 tie-break 往往足够，而且比 vector clock 轻。

性能边界也来自这个取舍。Lamport clock 是一个标量，维护成本很低；每条消息多带一个数字，接收时做一次 max+1，开销很小。vector clock 要随节点数或参与者数增长，信息更丰富，成本也更高。Lamport clock 用更少的信息换更低成本。

但低成本也会把复杂性推给上层。因为它不能识别并发，业务如果用它做 last-write-wins，就要接受丢失并发更新的风险；如果用它做全局排序，稳定 tie-break 可能制造一个和业务因果无关的顺序；如果在热点路径上把所有事件都送到一个中心 sequencer 获取全局逻辑时间，那性能瓶颈就不再是 Lamport clock 本身，而是中心化分配点。

在 LogServe 里，可以把 Lamport clock 思路用于解释 command_seq、日志位置和 actor mailbox 顺序，但要保持边界：command_seq 说明同一 actor 内的命令顺序，epoch fencing 说明当前 owner 资格，deadline/lease 说明时间预算和临时资格。把这些概念分开，系统语义才不会互相污染。

一句话：Lamport clock 的正确性边界是单向因果保证，性能边界是低开销但信息不足。它适合做轻量排序，不适合回答所有关于真实时间、并发冲突和写入资格的问题。

## Q021. 面试官如果只问一个问题检验你是否理解 vector clock，可能会问什么？

我觉得最能检验理解的问题是：有两个版本，A 的 vector clock 是 `{n1:3, n2:1, n3:0}`，B 的 vector clock 是 `{n1:2, n2:2, n3:0}`。你能不能说 A 比 B 新？如果不能，系统应该把它们当成什么？

答案是不能说 A 比 B 新。vector clock 不是把数字加起来比大小，也不是看某个分量更大就赢。它的比较是逐分量比较：如果 A 的每个分量都小于等于 B，并且至少一个分量严格小于 B，那么 A happens-before B，B 可以覆盖或包含 A 的因果历史。反过来也一样。如果两个向量各有某些分量更大，谁也不支配谁，它们就是并发版本。

上面的例子里，A 在 n1 上更大，B 在 n2 上更大，所以二者不可比较。它们可能来自两个节点在没有看到对方更新的情况下分别写入同一个 key。正确处理通常不是随便选一个丢掉，而是保留 sibling、返回冲突、交给业务合并，或者用明确的业务规则做裁决。

这个问题还会顺带考更新规则。每个参与者维护自己的分量；本地事件或写入时递增自己的分量；发送消息或版本时携带整个向量；接收远端向量时逐分量取 max，再推进本地分量。这样一个版本的向量就不只是“我自己做了几次更新”，还包含“我已经看见过其他参与者推进到哪里”。

它和 Lamport clock 的差异正好在这里。Lamport clock 是一个标量，能保证因果先后的时间戳大小关系，但不能从大小反推因果；vector clock 保留了每个参与者的进度，所以在正常模型下可以判断两个事件或版本是因果先后，还是并发不可比较。代价是 metadata 变大，参与者集合越动态，管理越麻烦。

面试里我会这样说：vector clock 的价值不是给所有版本排一个全局新旧，而是告诉你“这个版本是否包含那个版本的因果历史”。如果两个版本不可比较，系统看到的是并发分叉，不是简单的谁更新。

## Q022. vector clock 的一句话定义是否容易误导，误导点在哪里？

容易误导。常见定义是：vector clock 是用一个向量表示分布式系统中事件因果关系的逻辑时钟。这个定义没有错，但会让人漏掉几个工程里最容易出事故的点。

第一个误导点是把它当成“更大的时间戳”。vector clock 不能像整数时间戳那样直接排序。比较时要看每个分量是否都不大于另一个向量。如果只看总和、最大分量、字符串顺序或最后更新节点，就会把并发版本误判成先后版本，丢掉本该合并的数据。

第二个误导点是忽略作用域。向量的每一维代表一个参与者，但这个参与者到底是节点、replica、client、region、actor，还是 shard，要提前定义。作用域不同，语义完全不同。客户端级 vector clock 可以精确表达客户端分叉，但维度可能爆炸；replica 级 version vector 维度更可控，但不能区分同一 replica 上不同客户端的所有因果细节。

第三个误导点是把 vector clock 和冲突解决混为一谈。它能告诉你两个版本是因果覆盖还是并发冲突，但它不会替你决定购物车怎么合并、配置字段怎么合并、计数器怎么合并。Dynamo 这类系统把并发版本暴露给应用，就是因为存储层只知道版本关系，不知道业务含义。

第四个误导点是忽略传播要求。如果客户端读到一个版本，后续写入却没有带回对应 causal context，服务端就不知道这次写是否基于之前的版本。少传一次上下文，系统就可能制造多余 sibling；乱传或伪造上下文，又可能错误地覆盖别人。

第五个误导点是忽略裁剪。vector clock 不能无限增长。节点扩缩容、客户端很多、长时间分叉、旧版本长时间保留，都会让向量变大。系统通常要 prune、truncate 或用 dotted version vector 之类变体。裁剪一旦做得粗糙，就可能丢因果信息，把真实并发误判成覆盖，或者把已覆盖的旧版本当成冲突。

我更愿意把它定义成：vector clock 是一种按参与者记录逻辑进度的 causal context，用逐分量比较判断版本之间是因果先后还是并发分叉。它解决的是因果关系识别，不是全局排序，也不是业务合并器。

## Q023. vector clock 最常见的生产事故触发条件是什么？

最常见的是把并发版本错误地做 last-write-wins。系统明明能看出两个 vector clock 不可比较，但为了省事按时间戳、节点 ID 或响应到达顺序选一个。用户看到的是更新丢失：购物车少了一件商品，配置回滚了一个字段，协作文档丢了一段内容。事故根因不是 vector clock 失效，而是系统没有尊重它发现的并发冲突。

第二类是客户端没有带回 causal context。读请求返回了 value 和 vector clock，客户端写回时只带新 value，不带旧 context。服务端无法判断这次写覆盖了哪个历史，只能生成一个新的分叉版本。短期看只是 sibling 变多，长期会让读放大、合并成本和存储成本一起上升。

第三类是 vector 维度失控。把每个客户端、session 或临时 worker 都当成一个 clock 维度，系统规模一上来，单个对象的版本元数据就会膨胀。请求体变大、存储变大、比较成本变高，p99 延迟被少数“历史很长的 key”拖起来。平均对象大小可能还正常，但热点 key 已经很危险。

第四类是裁剪策略太激进。为了控制大小，系统删掉旧分量或旧 dot。裁剪如果没有清楚的安全条件，就可能丢掉某些参与者的历史，导致错误支配判断。结果要么把并发更新错误覆盖掉，要么永远认为有冲突，业务合并流程被打爆。

第五类是动态成员处理错误。节点下线、扩容、rebalance、region 迁移时，clock 维度和 replica 责任范围会变化。如果旧节点分量还在版本里，新节点又使用新的身份，系统可能同时保留一堆历史身份。反过来，如果过早把旧身份清掉，又会丢掉旧版本因果。

第六类是把 vector clock 当成安全机制。vector clock 假设参与者按协议递增和传播。恶意客户端、buggy SDK、手工修复脚本如果能随便构造一个“很大”的向量，就可能骗过覆盖判断。对外部输入的 causal context 要有限制、校验和作用域隔离，不能把它当成可信授权。

所以 vector clock 的生产事故通常有两种形态：要么系统忽视并发冲突，悄悄丢数据；要么系统保留了太多冲突和元数据，最后被 sibling、读放大和存储膨胀拖慢。

## Q024. vector clock 的指标应该怎么设计才不会只看平均值？

vector clock 的指标要围绕“因果判断是否健康”和“元数据是否失控”来设计。平均版本大小或平均读延迟都不够，因为 vector clock 的问题经常集中在少数高冲突 key 上。

第一组是比较结果分布。每次写入或合并时，要统计 dominates、dominated-by、equal、concurrent、invalid context 的数量。concurrent 比例突然升高，说明网络分区、客户端上下文丢失或热点 key 并发写变多。dominated-by 比例升高，可能是旧客户端重试或乱序写入在增多。

第二组是 sibling 指标。要看每个 key 的 sibling count 分布，尤其是 p95、p99、max；还要看 sibling age、未合并 sibling 数、合并失败数、读请求返回多个版本的比例。平均 sibling count 接近 1 没有意义，一个购物车 key 上挂着几十个 sibling 就足够让用户体验很差。

第三组是元数据大小。要按 key、bucket、tenant、replica 统计 vector length、serialized bytes、最大分量数、被 prune 的分量数、dotted entries 数、context 传输大小。要看尾部，不要只看平均值。vector clock 的开销不是均匀摊开的，热点对象和长生命周期对象最容易膨胀。

第四组是上下文传播质量。要看写请求中携带 causal context 的比例、context 缺失率、context 过期率、非法或不可信 context、SDK 版本分布、跨 region 写入缺失上下文次数。很多“冲突很多”的系统，根因是客户端协议没有把读到的上下文带回来。

第五组是裁剪效果。要统计 prune count、prune reason、prune 后仍产生的并发冲突、prune 后被错误覆盖的检测、vector truncation 命中率。裁剪是必要的，但必须可观测，否则你不知道是在控制成本，还是在丢因果信息。

第六组是业务合并结果。要看 application merge success、manual resolution、LWW fallback、lost-update complaint、合并后写回失败、合并耗时 p99。vector clock 只是把冲突暴露出来，真正的用户结果取决于业务合并能不能稳定完成。

我会把面板设计成三块：因果比较结果、冲突和 sibling 尾部、vector metadata 尾部。只看平均延迟，会把最危险的几个 key 完全盖住。

## Q025. vector clock 的正确性边界和性能边界分别是什么？

vector clock 的正确性边界是：在一个明确参与者集合和可信传播协议内，它可以判断两个版本是否存在因果包含关系，或者是否并发不可比较。这个边界比 Lamport clock 强，但不是无限强。

它能说的是：如果版本 B 的 vector clock 在每个分量上都大于等于 A，且至少一个分量更大，那么 B 的因果历史包含 A，可以认为 B 覆盖了 A。它还能说：如果 A 和 B 各有分量领先，它们是并发分叉，存储层不应该假装其中一个天然更新。这个判断依赖所有相关事件都按规则递增和传播。

它不能说的是：哪个版本业务上正确，哪个版本应该保留，哪个版本在真实物理时间上更晚。它也不能在参与者恶意、上下文伪造、成员身份混乱、裁剪过度的情况下继续给出完整保证。vector clock 暴露并发，不解决并发。

性能边界主要是 metadata 成本。N 个参与者意味着每个版本可能携带 O(N) 的向量，比较和合并也要按分量扫描。动态系统里，N 不只是机器数，还可能是客户端、actor、region 或 replica 历史身份。参与者越多、冲突越久、版本保留越久，成本越明显。

工程上经常要做折中：用 replica 级 version vector 降低维度；用 dotted version vector 表达单次更新；对旧分量做裁剪；对不重要对象改用 LWW；对重要对象暴露 sibling 给业务合并。这些折中都会改变正确性边界，不能只说“我们用了 vector clock”。

在 LogServe 的语境里，vector clock 更适合解释多副本、跨 worker 或未来多节点状态合并时的因果冲突。当前单机机制验证里，actor command_seq 和 log position 已经给了很多顺序边界，不需要把所有状态都做成 vector clock。只有当系统允许多个副本离线并发写同一逻辑对象时，vector clock 的成本才更值得付。

一句话：vector clock 能识别因果覆盖和并发分叉，但它的代价是向量元数据、传播协议和冲突合并。正确性停在“识别关系”，业务合并和成本控制要另外设计。
