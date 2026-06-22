# 40. context cancellation、worker pool、backpressure、actor mailbox 追问链

这一批问题放在并发和容量治理的交界处。`context cancellation` 讲的是工作什么时候该停、停的信号怎么传；`worker pool` 讲的是同时做多少工作才有边界；`backpressure` 讲的是系统处理不过来时怎样把压力往上游传；`actor mailbox` 讲的是把同一个 actor 的状态修改排成一条顺序线。

面试里这些词很容易被一句话讲轻。真正要说清楚的是边界：取消信号不会杀 goroutine，worker pool 不是无限吸收任务，backpressure 不是单纯限流，mailbox 也不是天然可靠队列。下面的问题按同一套追问链展开，重点放在定义误区、事故条件、指标设计，以及正确性和性能边界。

## Q001. 面试官如果只问一个问题检验你是否理解 context cancellation，可能会问什么？

**回答：**

我会预期他问这个问题：

```text
一个 HTTP 请求进来后，你用 context.WithTimeout 派生出 ctx，启动几个 goroutine 去查数据库、调用 RPC、向结果 channel 发送数据。客户端中途断开，或者超时先到了。请说明：哪些代码会停，哪些代码不会停？如果某个 goroutine 正卡在 channel send、数据库调用或网络读写上，它会自动退出吗？你要把 ctx 放在哪些 select 和下游调用里？
```

这道题能一下子测出候选人有没有把 context cancellation 当成“线程中断”。Go 的 `context` 传递的是取消信号、deadline 和请求级 value。它不会抢占 goroutine，不会把阻塞中的普通代码强行打断，也不会回滚已经发生的副作用。真正退出的是那些主动观察 `ctx.Done()`、主动检查 `ctx.Err()`、或者调用了支持 context 的下游 API 的代码。

一个比较稳的回答应该拆成几层。

第一层，父 context 被取消后，派生出来的子 context 会一起被取消。`Done()` 对应的 channel 会关闭，`Err()` 会返回 `context.Canceled` 或 `context.DeadlineExceeded`。如果用了带 cause 的取消，还可以从 `context.Cause` 里拿到更具体的原因。这个信号是广播式的，适合让同一条请求链上的多个组件同时知道“这次工作已经没有意义了”。

第二层，goroutine 是否退出取决于它有没有接这个信号。比如 worker 正在做一个支持 context 的数据库查询，且查询函数收到了同一个 ctx，那么取消有机会传到数据库驱动或 RPC 框架；如果 worker 只是执行一段 CPU 循环，循环里从不检查 ctx，它就会继续跑。channel 也一样：

```go
select {
case out <- result:
    return nil
case <-ctx.Done():
    return ctx.Err()
}
```

如果只写 `out <- result`，而调用方已经返回、不再接收结果，这个 goroutine 可能永远卡住。context 不会帮你把这个 send 弄醒。

第三层，调用方要负责释放派生 context 的资源。用 `WithCancel`、`WithTimeout`、`WithDeadline` 得到的 `cancel` 不只是“手动取消”按钮，它还会解除父子引用、停止关联 timer。即使函数正常结束，也应该 `defer cancel()`。不调用可能让子 context、timer 或相关引用活到父 context 取消为止。

第四层，要区分“停止等待结果”和“撤销副作用”。请求超时后，调用方不该继续等结果，但下游可能已经写了数据库、发了消息、扣了库存。context cancellation 不是事务回滚协议。需要幂等键、事务、outbox、补偿逻辑的地方，不能用 `ctx.Done()` 代替。

所以我会这样收束：context cancellation 是一条协作式停止信号。它的正确用法是创建请求级取消树，把同一个 ctx 传给所有下游阻塞点，在 channel send/receive、锁等待、队列提交、RPC/DB 调用处都留退出路径，并且在函数结束时调用 cancel 释放资源。它不负责杀 goroutine，也不保证业务副作用消失。

## Q002. context cancellation 的一句话定义是否容易误导，误导点在哪里？

**回答：**

容易误导。最常见的一句话是：`context` 用来取消 goroutine。这个说法听起来顺口，但问题很大。

更准确的说法是：`context.Context` 在 API 边界上传递 deadline、取消信号和请求级 value，取消信号由被调用方协作观察。这里有两个词很关键：传递、协作。context 只把信号送到那里，不替代码做退出决定。

误导点主要有几类。

第一，把 cancellation 理解成强制中断。很多语言里有线程中断、任务取消、future cancel 的概念，容易让人以为 Go 的 context 也能把 goroutine 杀掉。Go 没有这种语义。goroutine 正在算、正在抢锁、正在向没人接收的 channel 发送、正在调用一个不接受 context 的库函数，只要代码没有设计退出点，它就不会因为 ctx 取消而自动停。

第二，把 context 当成生命周期所有权。context 只是信号和请求元数据的载体，不是 goroutine handle。你不能通过 context 等 goroutine 结束，也不能知道所有派生 goroutine 是否已经退出。真正的生命周期管理还要靠 `WaitGroup`、`errgroup`、channel close、worker shutdown 协议，或者任务状态机。

第三，把 `WithTimeout` 当成精准执行时间限制。deadline 到了，context 会取消，但业务代码实际停下来还有延迟。这个延迟取决于它多久检查一次 ctx、下游 API 是否支持取消、网络栈和驱动是否能中断当前操作。面试里如果只说“设置超时就不会慢”，通常会被继续追问。

第四，把 `Context` 的 value 当成通用参数袋。官方建议很明确：value 只适合跨 API、跨进程传递的请求级数据，比如 trace id、auth token、租户信息。把分页参数、可选配置、日志开关都塞进 context，会让函数签名看起来干净，实际依赖变得隐形。

第五，把 `cancel` 忘成可选项。`cancel` 不只是异常路径才用。正常返回也要调用它，因为它释放 timer 和父子 context 之间的引用。这个点在服务端尤其常见：代码看起来没有 goroutine 泄漏，压测一久 timer、context 树和保留对象开始堆积。

所以一句话定义最好改成：context 是请求链上的取消、deadline 和请求级元数据传递机制；它只发出协作式信号，退出、清理、回滚和等待结束都要由业务代码自己设计。

## Q003. context cancellation 最常见的生产事故触发条件是什么？

**回答：**

最常见的触发条件不是“没有用 context”，而是只在入口用了 context，下游和阻塞点没有贯彻到底。线上事故通常长这样：入口请求已经超时或客户端已经断开，调用方返回了，后台 goroutine 还在跑；它继续占连接、占锁、占 channel、占内存，甚至继续写下游。短时间看只是延迟抖动，流量一大就变成 goroutine 堆积、连接池耗尽、队列越积越长。

几个高发场景要特别熟。

一是忘记 `defer cancel()`。比如每次请求都 `WithTimeout`，但正常路径没有调用 cancel。timer 和子 context 会保留到 timeout 或父 context 取消。单次成本不大，QPS 高时就是稳定泄漏。

二是没有把 ctx 传给下游。HTTP handler 收到了 `r.Context()`，但内部调用数据库时用了 `context.Background()`；调用 RPC 时重新起了一个不带 deadline 的 context；提交 worker pool 时只把任务对象放进队列，没有把剩余时间预算一起传过去。结果入口超时已经发生，下游还在慢慢做无效工作。

三是 channel send/receive 没有取消分支。典型例子是 fan-out 查询：多个 goroutine 竞争返回第一个结果，主流程拿到一个结果后返回，其他 goroutine 还试图向无缓冲 channel 写结果。没有 `select { case ch <- v: case <-ctx.Done(): }`，这些 goroutine 会卡住。

四是 worker 只在取任务时看 ctx，执行任务时不看。这样 shutdown 时队列入口停了，但正在执行的任务可以继续跑很久。更麻烦的是任务里还有重试循环，每次重试都用新的 background context，原来的取消信号彻底断掉。

五是把取消误认为失败回滚。请求方超时后重试，第一次请求其实已经写入成功，第二次又写一次。如果没有幂等键，context cancellation 会和重试一起放大成重复扣款、重复创建任务、重复发消息。

六是把已取消的 context 复用。某些代码把 context 存到 struct 里，后续请求复用同一个字段。只要这个 context 被取消过，后面所有调用都会立刻失败。这个问题不一定马上暴露，通常出现在连接复用、后台 manager、SDK wrapper 里。

七是错误地忽略 `Err()` 或 cause。日志里只有“context canceled”，但不知道是客户端断开、上游主动取消、deadline 到期，还是系统过载时自己取消。排障时所有取消混成一种错误，根因会被掩盖。

我会把生产事故总结成一句更实在的话：context cancellation 的风险在断链。入口有 ctx 不够，所有可能等待的点、所有下游调用、所有后台 goroutine 都要能收到同一条停止信号，并且在退出时把结果通道、资源、状态机和幂等语义处理干净。

## Q004. context cancellation 的指标应该怎么设计才不会只看平均值？

**回答：**

context cancellation 不能只看“平均取消耗时”或“平均请求耗时”。平均值会把最危险的尾部藏掉。真正要看的是取消发生后，系统有没有及时停工、有没有继续消耗资源、有没有产生错误副作用。

我会把指标拆成几组。

第一组是取消原因。至少区分 `deadline_exceeded`、`client_canceled`、`server_shutdown`、`overload_rejected`、`manual_cancel`、`parent_cancel`。如果用了 cancellation cause，还应该把 cause 归类进低基数 label。不能把所有 `context.Canceled` 都算成失败，也不能把所有 deadline 都归因于下游慢。

第二组是取消传播延迟。记录从 ctx 被取消到 goroutine 退出、下游调用返回、任务状态终止的时间。这里要看 p50、p95、p99 和最大值。一个系统平均 2ms 响应取消，但 p99 有 30s，就说明某些阻塞点没接 ctx。

第三组是取消后的残留工作。比如请求取消后仍在运行的 goroutine 数、仍持有的数据库连接数、仍在队列中的任务数、仍在执行的 RPC 数、仍未释放的 timer 数。这些指标比“取消次数”更接近事故本身。

第四组是队列和 channel 相关指标。取消时任务在队列里等了多久，取消后是否还被 worker 取走，结果 channel 是否发生 send 阻塞，是否有 abandoned result，是否有 dropped response。很多泄漏不在 RPC 调用里，而在结果回传路径上。

第五组是 deadline 预算。记录进入每一层时剩余 deadline，比如入口剩 800ms，排队后剩 50ms，调用数据库前已经剩 5ms。这样可以发现“请求不是在数据库慢了，而是在队列里已经快死了”。平均请求耗时看不出这个问题。

第六组是副作用和重试。取消后仍成功提交的写操作数、重试次数、幂等命中率、重复请求被拒绝或合并的次数。这类指标能回答一个关键问题：取消以后系统有没有继续改变外部状态。

第七组是泄漏观测。goroutine profile 中卡在 `select`、channel send、DB driver、netpoll、锁等待的数量；timer heap 或 runtime metrics 的变化；每个请求结束后仍活着的派生 worker 数。这里不一定全做成业务指标，但至少要有压测和诊断入口。

看板上我会避免只放一条均值曲线。更有用的组合是：按取消原因分组的取消率、取消传播 p95/p99、取消后残留 goroutine、队列 oldest age、下游 inflight、deadline 剩余预算分布、取消后副作用数量。这样面试官会知道你关注的是尾部和资源闭环，而不是给 `ctx.Err()` 加了一个计数器。

## Q005. context cancellation 的正确性边界和性能边界分别是什么？

**回答：**

正确性边界先讲清楚：context cancellation 保证的是信号传播，不保证工作已经停完。父 context 取消后，子 context 会被取消，`Done` 会关闭，支持 context 的代码可以观察到这个事实。它不保证 goroutine 被杀，不保证阻塞系统调用都能中断，不保证业务副作用回滚，不保证外部服务真的取消了请求。

从工程上看，正确性至少有几条边界。

一是传播边界。ctx 必须沿调用链显式传递。中途改用 `Background()`，取消树就断了。需要脱离请求生命周期的后台任务可以用新的 root context，但这应该是明确设计，不是随手写。

二是退出边界。每个可能长时间等待的地方都要有取消路径：channel send/receive、队列入队、锁等待、RPC、DB、外部进程、定时器等待、重试 sleep。无法取消的操作要承认边界，比如某个第三方库不支持 context，那就要用连接 deadline、进程隔离、短任务切片，或者把它放到可控 worker 里。

三是资源边界。派生 context 后要调用 cancel。取消后还要等 goroutine 退出，关闭或 drain 必要的 channel，释放连接和文件句柄。只取消不等待，系统可能只是把泄漏换成后台清理延迟。

四是副作用边界。context 不能表达“这个写操作是否已经提交”。写操作要用事务、幂等键、状态机、outbox 或补偿逻辑保护。取消只说明调用方不再需要结果，不说明世界没有被改变。

性能边界也很现实。

第一，`WithTimeout` 和 `WithDeadline` 会创建 timer，热路径上过度派生会带来分配和 timer 管理成本。不要在每个很小的内部函数里重复套 timeout。更好的做法是从入口 deadline 里切预算，必要时派生，完成后及时 cancel。

第二，取消 fan-out 的成本和派生树规模有关。一个请求派生出成千上万个子 context，取消时要传播信号，随后大量 goroutine 同时醒来清理。瓶颈可能出现在调度、锁、日志、指标、连接池归还，而不是 context 本身。

第三，过短 deadline 会制造重试风暴。请求在本地排队 90ms，剩 10ms 去调下游，几乎必超时；上游马上重试，又制造更多排队。deadline 是资源预算，不是越短越安全。

第四，频繁在 select 里检查 ctx 通常不是主要性能问题，但如果把取消路径做得很重，比如每次取消都同步打大日志、写审计、上报高基数指标，就会让故障时的失败路径拖垮系统。

所以我的边界回答是：context cancellation 解决的是协作式取消传播和 deadline 预算，不解决强制抢占、业务回滚和执行完成确认。性能上要控制派生规模、timer 数量、取消风暴和失败路径成本，不能把 context 当成零成本的全局刹车。

## Q006. 面试官如果只问一个问题检验你是否理解 worker pool，可能会问什么？

**回答：**

我觉得最能检验理解的问题是：

```text
你有一个 worker pool，32 个 worker，队列长度 1000。现在入口每秒提交 5000 个任务，下游数据库最多稳定处理 300 个并发查询。任务可能超时，调用方也可能取消。请说明任务会在哪里排队，队列满了怎么办，worker 阻塞时系统会发生什么，shutdown 时如何保证不泄漏、不重复、不默默丢任务？
```

这个问题比“worker pool 怎么写”更有价值。会写 `for job := range jobs` 不难，难的是说清楚容量边界、接收语义、取消语义、失败语义和观测方式。

我会先说明：worker pool 的核心不是复用 goroutine，而是限制同时执行的任务数量。32 个 worker 表示最多有 32 个任务处于执行阶段，队列长度 1000 表示最多有 1000 个任务处于等待阶段。入口流量超过执行能力时，多出来的任务必须有明确去处：等待、拒绝、降级、转入持久化队列，或者阻塞提交方。没有这个策略，pool 只会把过载藏起来。

然后要追问队列满了怎么办。在线请求通常不适合无限等待。如果请求 deadline 只剩几十毫秒，继续排队没有意义，应该快速返回容量错误或降级结果。离线任务如果不能丢，就不要依赖内存 channel，要进入持久化队列，并且定义重试和幂等。

worker 阻塞时也要讲清楚。worker 可能卡在数据库、RPC、锁、文件 I/O 或外部进程。pool 限制的是并发数，不保证任务能完成。32 个 worker 全部卡住后，队列会开始增长；队列满后入口策略生效。如果这时入口仍然无界创建 goroutine 等待入队，worker pool 的边界就被绕开了。

取消要贯穿提交和执行两个阶段。提交阶段可以这样写：

```go
select {
case jobs <- job:
    return nil
case <-ctx.Done():
    return ctx.Err()
default:
    return ErrQueueFull
}
```

如果业务允许等待入队，可以去掉 `default`，但仍然要保留 `ctx.Done()`。执行阶段 worker 也要把 ctx 传给下游，并且任务自己的重试 sleep、结果回传、状态更新都要能停止。

shutdown 时要明确顺序：先停止接收新任务，再取消或等待已接收任务，再关闭队列，再等待 worker 退出，最后处理未完成任务。这里有一个语义问题必须说清楚：任务进入队列算不算 accepted？如果已经返回 accepted，后面 shutdown 不能静默丢弃；要么执行完成，要么标记失败可查，要么持久化后恢复。

最后我会补一句：worker pool 不提供公平性、顺序性、持久性和幂等性，除非你额外设计。它只是一个有界执行器。面试官问到这一步，真正想听的是“有界”两个字背后的工程约束。

## Q007. worker pool 的一句话定义是否容易误导，误导点在哪里？

**回答：**

常见定义是：worker pool 是一组固定 goroutine 从任务队列里取任务执行。这个定义没错，但非常容易让人只盯着代码形态，忽略它真正要保护的东西。

第一个误导点是把 pool 当成性能优化。很多人说“用 worker pool 减少 goroutine 创建成本”。在 Go 里 goroutine 创建成本通常不是第一矛盾。真正昂贵的是任务背后的资源：CPU、内存、数据库连接、远端 QPS、文件句柄、GPU、外部 API 配额。worker pool 的价值在于把这些资源的并发上限显式化。

第二个误导点是默认有队列就安全。队列只是把执行不了的任务推迟。队列有界，满了就要拒绝或阻塞；队列无界，过载会变成内存增长和延迟失真。一个很长的队列会让入口看起来成功，用户实际已经等不到结果。

第三个误导点是忽略提交语义。任务放进队列以后，调用方认为它成功了吗？如果入队成功后进程崩溃，任务丢了算谁的？如果队列满返回错误，调用方能不能重试？如果相同任务重复提交，如何去重？这些都不是 worker pool 自动解决的。

第四个误导点是以为固定 worker 数等于稳定吞吐。worker 太少会浪费下游能力，太多会放大下游拥塞。CPU-bound 任务和 I/O-bound 任务的 pool size 不是同一种算法。下游数据库只有 100 个连接，你开 1000 个 worker，结果很可能只是把等待从本地队列挪到数据库连接池。

第五个误导点是把 shutdown 想简单了。`close(jobs)` 只能让 worker 在队列 drain 后退出。它不会取消正在执行的任务，不会通知提交方，不会处理半完成结果，也不会解决 worker panic 后任务丢失的问题。

所以我会把一句话改成：worker pool 是一个带接收策略、队列策略、执行上限、取消机制、失败语义和观测指标的有界执行器。固定 goroutine 加 channel 只是最小实现，不是完整设计。

## Q008. worker pool 最常见的生产事故触发条件是什么？

**回答：**

worker pool 的生产事故通常从一个误判开始：以为加了 pool，系统就不会过载。实际上 pool 只能限制执行并发；如果入口、队列、下游、取消、重试没有一起设计，过载会换个地方爆。

最常见的是无界队列。入口请求一直提交成功，队列长度持续增长，任务在队列里等到过期。最后内存上升、GC 变重、p99 飙升，调用方超时重试，队列更长。这个事故的根因不是 worker 太少，而是 admission 没有拒绝边界。

第二是 pool size 和下游容量不匹配。比如 200 个 worker 同时查数据库，但数据库连接池只有 50 个连接，另外 150 个 worker 都在等连接。入口看见 worker 忙，队列变长，于是又扩 worker。吞吐没升，连接池等待和超时一起升。

第三是任务不响应取消。请求已经超时，任务仍然在 worker 里跑；worker 被无效任务占住，新请求进来只能排队。这个问题在线上很隐蔽，因为 CPU 可能不高，worker 利用率却满了。

第四是 worker panic 后没有恢复。一个任务触发 panic，把 worker goroutine 打死，pool 的实际并发数悄悄下降。系统还能跑，但吞吐变低，队列慢慢涨。没有 `panic` 计数和 worker 存活指标时，这类问题很晚才会被发现。

第五是任务之间发生池内死锁。任务 A 在 worker 里提交任务 B 到同一个 pool，然后同步等待 B 完成；如果所有 worker 都在等自己提交的子任务，队列里的子任务永远拿不到 worker。这个问题在批处理、递归任务、DAG 调度里很常见。

第六是慢任务和快任务混在一个队列。几个慢任务占住 worker，快任务排在后面，形成 head-of-line blocking。看平均任务耗时还行，某一类请求的 p99 已经不可接受。

第七是 shutdown 顺序错误。先 close 队列但还有提交方在 send，直接 panic；先取消 context 但不等 worker 退出，后台还在写状态；认为 close 后队列里的任务都处理了，实际 worker 中途失败。正确做法要明确“停止接收、取消、drain、等待、标记未完成”的顺序。

第八是重试放大。pool 满返回错误，上游立即重试；worker 内部调用下游失败也立即重试；任务 redelivery 再叠一层。最后拒绝本来是保护机制，却制造了更多请求。

我会把事故触发条件概括为：worker pool 出事，多半是入口没有 admission control，队列没有等待预算，任务没有取消路径，下游没有容量对齐，失败没有幂等和重试预算。只看 worker 数量解决不了这些问题。

## Q009. worker pool 的指标应该怎么设计才不会只看平均值？

**回答：**

worker pool 的指标必须把等待和执行拆开。只看平均任务耗时，会把排队时间、执行时间、下游等待、取消、拒绝全部混在一起。面试里我会说：pool 的核心看板不是“平均耗时”，而是“任务从提交到完成的每个阶段花了多久，以及哪里开始超过预算”。

关键指标有这些。

队列深度要看当前值、最大值和分位数，但更重要的是队列年龄。`queue_depth=10` 不一定危险，10 个任务每个 1ms 就能处理完；`queue_depth=2` 但最老任务等了 30s，已经很严重。`oldest_job_age` 和 `enqueue_wait_ms` 的 p95/p99 比平均深度更有用。

提交结果要分开计数：accepted、rejected、timeout_on_enqueue、canceled_before_enqueue、duplicate、shed、persisted。只看 accepted QPS 会误以为吞吐稳定，实际可能大量请求已经被拒绝。

worker 状态要拆成 busy、idle、blocked_on_downstream、blocked_on_queue、blocked_on_lock、panicked、exiting。至少要知道 worker 满载时是在做有效计算，还是都卡在连接池或下游 RPC 上。

任务耗时要拆成 wait time 和 service time。总耗时 p99 高，如果 wait time 高，问题在容量和 admission；如果 service time 高，问题在任务本身或下游；如果 cancel 后仍长时间 service，说明取消没有传进去。

按任务类型、租户、优先级、下游依赖分组。一个平均 pool 看起来健康，不代表所有任务都健康。热点租户可能占满队列，低延迟任务可能被慢任务拖死，某个下游慢可能把共享 worker 全部占住。

还要看饱和度和恢复能力。包括 worker utilization、queue occupancy、reject rate、backlog drain time、从过载恢复到正常 p99 的时间。过载发生时，系统是否快速拒绝、是否保持内存稳定，比平时的平均耗时更能说明设计水平。

错误和重试也必须纳入。任务失败率、panic 数、重试次数、retry budget 消耗、幂等命中、重复执行、取消后执行完成的数量。这些指标能看出 pool 是否在异常路径上放大工作量。

如果要给面试官一个完整答案，我会这样说：worker pool 看 p99 队列等待、最老任务年龄、队列深度、accepted/rejected/canceled、worker busy/idle/blocked、任务 service time、下游等待、panic、重试、按租户和任务类型拆分的公平性。平均耗时只能放在角落里，不能作为主判断。

## Q010. worker pool 的正确性边界和性能边界分别是什么？

**回答：**

worker pool 的正确性边界是：它最多保证“被接受的任务会按照定义好的执行语义进入有限并发处理”。这个定义必须你自己补完整。pool 本身不保证任务不丢、不重复、不乱序、不超时、不公平、不产生副作用。

正确性上要先回答几个问题。

任务什么时候算 accepted？如果 send 到内存 channel 就算 accepted，那进程崩溃时任务可能丢。要可靠执行，就需要持久化队列、任务状态机或日志。内存 worker pool 适合请求内短任务，不适合承诺长期可靠任务。

accepted 后失败怎么表达？执行失败、取消、超时、worker panic、shutdown、进程崩溃分别如何记录？如果调用方只拿到一个“提交成功”，后面任务静默消失，就是语义漏洞。

任务是否可以重复执行？worker panic 后重试、超时后 redelivery、调用方重试都可能造成重复。没有幂等键和去重，worker pool 不会帮你实现 exactly-once。

顺序是否有保证？普通 pool 通常不保证全局 FIFO，更不保证同一个 key 的顺序。多个 worker 并发执行时，任务完成顺序和入队顺序不同。需要按 key 串行，就要分区队列、actor mailbox 或 per-key lock。

取消边界在哪里？入队前取消、排队中取消、执行中取消、结果回传时取消，语义不同。排队中的任务如果 ctx 已过期，最好不要再执行；执行中的任务只能协作停止；已经产生副作用的任务不能靠取消抹掉。

性能边界也要说得直接。

worker 数不是越多越好。CPU-bound 任务通常受 CPU 核数和调度开销限制；I/O-bound 任务受下游并发、连接池、QPS 配额限制。超过瓶颈容量后，加 worker 只会增加等待、上下文切换、内存和下游拥塞。

队列长度不是越大越好。队列吸收短暂 burst 有用，吸收长期容量缺口没有用。长期输入速率大于服务速率时，任何有限队列都会满，无界队列会把失败推迟到 OOM 或巨大延迟。

pool 会引入排队延迟。低流量时直接执行可能更快，高流量时 pool 保护系统稳定。这个取舍要用 p99、拒绝率、资源水位和恢复时间来看，不能只比较单次任务 ns/op。

共享 pool 会有隔离问题。慢任务、低优先级任务、热点租户可能占用 worker，影响其他任务。性能边界不仅是总吞吐，还有每类任务的尾延迟和公平性。

所以我会总结：worker pool 的正确性来自接收语义、失败语义、取消语义、幂等和持久化，不来自 goroutine 数量；性能来自容量匹配、队列预算、下游保护和隔离，不来自盲目扩 worker。

## Q011. 面试官如果只问一个问题检验你是否理解 backpressure，可能会问什么？

**回答：**

我预期的问题是：

```text
下游稳定只能处理 100 QPS，上游突然打来 1000 QPS。系统对多出来的 900 QPS 做什么？是排队、阻塞、拒绝、降级、丢弃、采样，还是让上游稍后再试？这个信号怎么传回去？如果上游不听会怎样？
```

这个问题能检验一个人是不是把 backpressure 当成“限流开关”。backpressure 的核心是压力反馈：系统发现下游或自身处理不过来时，不能无限缓存，也不能假装已经接收，而要把容量不足这件事转化成上游能理解的信号。

好的回答要先承认容量差距。100 QPS 的下游不可能凭空处理 1000 QPS。多出来的 900 必须被某种策略处理。排队只能吸收短 burst，不能吸收长期过载；阻塞会占住调用方资源；拒绝会牺牲部分请求但保护系统；降级会减少工作量；丢弃只适合可丢的新鲜度任务；持久化队列适合不能丢但可以延迟的任务。

然后要说明反馈信号。HTTP 里可以是 429、503、`Retry-After`；gRPC 可以是 `RESOURCE_EXHAUSTED` 或 `UNAVAILABLE`；流式协议可以是 demand 或 window；内部队列可以是 submit 返回 `ErrQueueFull`；actor mailbox 可以把溢出消息转到 dead letters 或返回拒绝。关键是上游必须能区分“业务失败”和“容量不足”。

还要讲上游不听的情况。如果上游收到拒绝后立即无退避重试，backpressure 会变成重试风暴。真正可用的 backpressure 通常要配合 retry budget、指数退避、jitter、客户端限速、熔断、优先级和幂等。

再往下追，就是承诺边界。backpressure 最好发生在 accepted 之前。还没承诺的请求可以拒绝；已经 accepted 的任务不能因为后来压力升高就静默丢掉。它要么完成，要么以可查询的失败状态结束，要么进入可恢复队列。

我会用一句话收尾：backpressure 不是让系统“扛住更多请求”，而是在处理不过来时及时、清楚、低成本地告诉上游“现在不能再接了”，让失败发生在边界上，而不是在内存、队列和下游里慢慢腐烂。

## Q012. backpressure 的一句话定义是否容易误导，误导点在哪里？

**回答：**

常见定义是：backpressure 就是下游慢时让上游慢下来。这句话方向对，但太粗。它容易让人忽略三个事实：上游未必会慢下来，系统未必选择阻塞，压力信号也不一定来自最终下游。

第一个误导点是把 backpressure 等同于阻塞。阻塞提交方只是其中一种做法。很多在线系统更愿意快速拒绝，因为阻塞会占连接、线程、goroutine、内存和调用方 deadline。流式系统可以用 demand 控制生产速率，队列系统可以拒绝入队，网关可以限速，服务可以降级。不同场景选择不同。

第二个误导点是把 backpressure 和 rate limiting 混在一起。rate limiting 通常按固定配额或令牌桶限制进入速率；backpressure 更强调根据当前容量水位动态反馈，比如队列年龄、worker 饱和、下游错误率、连接池等待、内存压力。二者经常一起用，但不是同一个概念。

第三个误导点是把 backpressure 当成吞吐优化。它更多是保护机制。触发 backpressure 后，成功吞吐可能下降，拒绝率会上升，但系统的内存、p99、下游错误和恢复时间会更可控。它不是让 100 QPS 的下游 magically 处理 1000 QPS。

第四个误导点是只看队列长度。队列长度是信号之一，但队列年龄、剩余 deadline、下游错误率、inflight、内存水位可能更早暴露问题。一个短队列也可能已经全是过期任务，一个长队列也可能只是短 burst。

第五个误导点是忘了协议语义。拒绝以后调用方能不能重试？什么时候重试？重复提交是否幂等？已经 accepted 的任务是否保证可查？如果这些没定义，backpressure 只是把故障从服务端推给客户端。

所以我会定义得更具体：backpressure 是系统在资源或下游接近饱和时，通过阻塞、拒绝、降级、窗口、配额或需求信号，把容量不足传回上游，并把未完成工作限制在有界范围内的机制。它的重点是边界和反馈，不是单纯“慢下来”。

## Q013. backpressure 最常见的生产事故触发条件是什么？

**回答：**

backpressure 事故常见于两种极端：该反馈时没反馈，或者反馈了但上游不理解。前者把过载藏进队列和内存，后者把拒绝放大成重试风暴。

最典型的是无界缓冲。服务为了“不丢请求”，把任务都塞进内存队列。短时间看成功率很高，长时间看队列年龄不断增长，任务过期、内存上涨、GC 加重，最后进程被打死。这里的问题是系统把“处理不了”伪装成“接收成功”。

第二是只在单个阶段做 backpressure，没有端到端传递。比如 worker pool 队列满了会阻塞，但 HTTP 入口还在接收连接；内部 actor mailbox 已经爆了，但 API 层仍返回 accepted；下游数据库慢了，服务本地还继续排队。压力没有传到真正能减速的地方。

第三是失败路径太重。过载时每个拒绝都同步写审计日志、上报 trace、触发告警、查询远端配置。成功路径已经慢了，拒绝路径又变成新瓶颈。backpressure 的拒绝路径必须比正常路径轻，否则越保护越慢。

第四是重试没有预算。上游收到 429/503 后立刻重试，多个层级都在重试，最终请求量成倍增加。没有 `Retry-After`、退避、jitter、幂等和 retry budget，backpressure 信号会被当成“再试一次”的按钮。

第五是错误分类不清。容量拒绝和业务失败混用同一个错误码，上游不知道该重试、降级还是提示用户。结果要么不该重试的请求被重试，要么可恢复的容量问题被当成业务失败。

第六是阻塞时持有关键资源。比如在拿着全局锁时等待队列空位，在数据库事务里等待下游容量，在 actor 处理消息时同步等待另一个 actor。backpressure 本来是保护系统，却因为阻塞位置错误造成死锁或级联阻塞。

第七是公平性缺失。一个租户、一个热点 key、一个慢 consumer 占满全局队列，其他流量也被拒绝。没有 per-tenant quota、优先级、隔离队列或分区，backpressure 会保护先到者，而不是保护重要工作。

第八是恢复抖动。队列水位刚低一点就全量放开，水位又马上升高，系统在接受和拒绝之间来回跳。需要 hysteresis、冷却时间、滑动窗口或渐进恢复。

我会这样概括：backpressure 出事故，多半不是因为没有一个阈值，而是承诺边界、反馈协议、重试行为、隔离和失败路径成本没有定义清楚。

## Q014. backpressure 的指标应该怎么设计才不会只看平均值？

**回答：**

backpressure 的指标要回答三个问题：压力从哪里来，系统在哪里拒绝或减速，上游有没有按信号改变行为。平均延迟回答不了这些问题，甚至会骗人。过载时如果系统快速拒绝，平均延迟可能很好看，但用户看到的是大量失败；如果系统无限排队，平均成功请求延迟可能还能维持一阵，失败正在队列里积累。

我会先看入口层。offered rate、accepted rate、rejected rate、shed rate、degraded rate 要分开。只看成功 QPS 会漏掉系统为了自保丢掉的请求。拒绝必须带 reason，比如 `queue_full`、`oldest_age_exceeded`、`downstream_slow`、`tenant_quota`、`memory_pressure`、`inflight_limit`。

队列层要看 depth、oldest age、enqueue wait p95/p99、dequeue rate、backlog drain time。oldest age 很重要，因为它直接说明排队任务是否还新鲜。队列深度只是数量，年龄才接近用户等待。

执行层要看 inflight、worker utilization、下游连接池等待、下游 p95/p99、错误率、超时率。backpressure 的信号不应该只来自本地队列，很多时候真正压力在下游。

反馈层要看返回给上游的信号是否有效。比如 429/503 数量、`Retry-After` 分布、客户端重试率、重试间隔、retry budget 消耗、同一 idempotency key 的重复次数。如果拒绝后重试率更高，说明 backpressure 没有被上游遵守。

公平性要单独看。按租户、优先级、任务类型、key 分组看 accepted/rejected、队列年龄、p99。全局指标健康，不代表某个租户没有把其他人挤掉。

资源层要看内存、goroutine、连接、文件句柄、CPU、GC、日志写入、指标上报开销。backpressure 的目标之一是让这些水位稳定。如果拒绝率升高但内存仍继续涨，说明系统还在接收或保留太多工作。

恢复指标也很关键。过载开始到触发保护用了多久，触发后 p99 是否停止恶化，过载解除后队列多久 drain，拒绝率多久恢复，是否出现抖动。一个保护机制平时看不出来，只有压到失败附近才知道好不好。

看板上我会放：offered/accepted/rejected、reject reason、queue oldest age、enqueue wait p99、inflight、downstream saturation、retry rate、per-tenant fairness、resource pressure、backlog drain time、pressure propagation latency。平均延迟只能辅助，不能当主指标。

## Q015. backpressure 的正确性边界和性能边界分别是什么？

**回答：**

backpressure 的正确性边界是：它只负责在容量不足时限制新工作、传递压力和保护已承诺语义。它不创造处理能力，也不能替代幂等、事务、重试协议和持久化。

正确性上第一条边界是 accepted/rejected 必须清楚。返回 rejected 的请求不能偷偷留下半个任务；返回 accepted 的任务不能因为后来压力升高被静默丢弃。这个边界不清楚，客户端重试和状态查询都会乱。

第二条是反馈语义必须可理解。容量拒绝要和业务错误分开。调用方要知道能不能重试、多久重试、是否需要换降级路径、是否应该停止发送。内部系统也一样，`ErrQueueFull` 和 `ErrInvalidRequest` 不能混成一个 error。

第三条是 backpressure 不保证公平，除非你设计公平。全局队列和全局阈值通常会被热点流量占满。需要租户配额、优先级、隔离队列、控制流量保留容量，才能避免关键请求被低价值请求挤掉。

第四条是不能用 backpressure 修复已经发生的副作用。请求被 accepted 后写了一半，后续因为水位高取消，这已经进入任务状态机或补偿逻辑范围，不再是 admission control 能解决的事。

性能边界主要是取舍。

阻塞式 backpressure 可以保住任务，但会占调用方资源。如果阻塞发生在入口连接、线程、goroutine 或锁上，高并发下可能把服务自己拖死。

拒绝式 backpressure 成本低、恢复快，但会牺牲成功率。它适合在线请求和可重试工作，但必须给上游明确退避信号。

缓冲式 backpressure 能吸收短 burst，但队列越长，延迟越不可信，内存越危险。队列容量应该来自等待预算和内存预算，而不是“给大一点保险”。

降级和 shedding 能保护核心路径，但要有业务优先级。随便丢会造成正确性问题；按价值丢、按新鲜度丢、按租户配额丢，才是可解释的策略。

最后，backpressure 自身必须便宜。admission 判断不能每次扫全局状态，拒绝路径不能同步做重 I/O，指标 label 不能爆炸，配置读取不能走远端慢路径。保护机制如果比正常路径还重，过载时它会成为新的热点。

我会总结：backpressure 的正确性边界在承诺语义和反馈协议，性能边界在延迟、吞吐、拒绝率、资源占用和恢复速度之间的取舍。它让系统失败得早、失败得清楚、失败得有边界，而不是让系统永远成功。

## Q016. 面试官如果只问一个问题检验你是否理解 actor mailbox，可能会问什么？

**回答：**

我认为最有区分度的问题是：

```text
两个 sender 同时给同一个 actor 发送命令，其中一个请求已经超时；actor mailbox 里还有旧消息；actor 处理到一半崩溃并重启，恢复时有 snapshot 和 tail log。请说明 mailbox 保证什么顺序，谁能修改 actor state，超时的请求是否会从 mailbox 消失，崩溃后哪些消息可能重放？
```

这道题能把“actor = 对象加队列”的浅理解筛出来。真正的 actor mailbox 不是普通队列那么简单，它是同一个 actor 状态修改的串行入口。

先说保证。对同一个 actor，如果 runtime 确保同一时刻只有一个 handler 在处理这个 actor 的消息，那么 actor 内部状态可以按消息处理顺序推进。外部 sender 不能直接改 actor state，只能发命令。状态修改发生在 actor 自己处理消息时。这是 mailbox 最重要的边界。

再说不保证。mailbox 通常不保证全局顺序。不同 sender 同时发消息，先后取决于到达 mailbox 的顺序、调度、网络和 runtime。不同 actor 之间也没有天然顺序。即使同一个 actor 内部顺序明确，也不代表跨 actor 操作是事务。

超时也要讲清楚。调用方等回复超时，不等于已经入队的命令自动消失。除非协议里设计了取消消息、过期检查、deadline 校验或从队列移除机制，否则 actor 后续仍可能处理这条命令。很多生产事故就来自这里：客户端以为超时就是没执行，实际 actor 稍后执行成功了。

崩溃和恢复要看持久化语义。如果 mailbox 只是内存队列，进程崩溃后未处理消息会丢；如果命令先写入日志，再进入调度，恢复时可以按 command sequence 重放。snapshot 只是恢复状态的加速入口，不是 mailbox 本身。恢复时通常加载 snapshot，再 replay snapshot 之后的 tail log，保证 actor state 回到某个提交边界。

还要区分“执行重复”和“应用一次”。worker 执行 actor method 时崩溃，外部副作用可能已经发生；恢复后 command 可能重试执行。runtime 可以用 command sequence 和 epoch fencing 防止旧结果覆盖新状态，但不能自动让外部副作用 exactly-once。

所以我会回答：actor mailbox 保证的是同一个 actor 的状态修改有一个串行入口；它不保证全局顺序、不自动删除超时消息、不天然持久、不自动处理重复副作用。要生产可用，还得设计 bounded mailbox、deadline、dead letters、持久化日志、snapshot、幂等和恢复协议。

## Q017. actor mailbox 的一句话定义是否容易误导，误导点在哪里？

**回答：**

一句话定义常说：mailbox 是 actor 前面的消息队列。这个定义太轻了。它让人以为 mailbox 只是一个普通 `Queue<Message>`，而忽略 actor 模型最重要的 ownership 和调度语义。

第一个误导点是忽略单消费者语义。mailbox 的关键不是“能排队”，而是同一个 actor 一次只处理一个消息。只要两个线程能同时进入同一个 actor handler，mailbox 就没有保护住 actor state。普通队列加多个 consumer 并不等于 actor mailbox。

第二个误导点是把入队顺序当成业务顺序。很多 runtime 对同一个 sender 到同一个 actor 有较强顺序，但多个 sender 并发发送时，顺序通常只是到达顺序。网络、调度、重试、优先级 mailbox 都会影响顺序。业务如果要求因果顺序，最好显式带 sequence、version 或 command id。

第三个误导点是默认 mailbox 可靠。内存 mailbox 崩溃就丢；bounded mailbox 满了可能拒绝或进 dead letters；priority mailbox 可能改变处理顺序；持久化 actor 也通常持久化事件或命令日志，而不是把内存队列本身当真相。

第四个误导点是忘记容量。unbounded mailbox 很方便，但 actor 处理速度低于消息到达速度时，它会变成内存泄漏和尾延迟放大器。bounded mailbox 才能把压力显式化，但 bounded 后就要处理满了怎么办。

第五个误导点是把 timeout 当成取消。sender 等 reply 的 future 超时，只是 sender 不等了。消息如果已经进 mailbox，actor 仍可能处理。除非消息里带 deadline，actor 处理前检查；或者系统支持取消入队消息，否则超时不会自动撤销命令。

第六个误导点是忽略生命周期。actor restart、passivation、迁移、owner 切换、snapshot replay，都会影响 mailbox 里的消息如何处理。mailbox 不是孤立结构，它和 actor identity、behavior、dispatcher、persistence、supervision 绑在一起。

所以更准确的定义是：actor mailbox 是同一个 actor 的消息接收和调度边界，它把并发 sender 的请求转成 actor 内部的串行处理流。队列只是表现形式，真正重要的是单 owner 状态、顺序语义、容量策略和失败恢复。

## Q018. actor mailbox 最常见的生产事故触发条件是什么？

**回答：**

actor mailbox 的事故大多是“局部串行”带来的副作用：同一个 actor 安全了，但热点 actor、慢消息、无界 mailbox 和超时语义会把系统拖住。

最常见的是 unbounded mailbox。某个 actor 处理速度跟不上发送速度，消息持续堆积。因为只有一个 actor handler 在处理，堆积不会被加 worker 直接解决。最后表现为内存增长、GC 压力、消息年龄变大、调用方超时、重试更多消息。

第二是热点 actor。比如所有请求都打到同一个用户、同一个房间、同一个全局配置 actor。actor 模型把同一 key 串行化，这对正确性很好，但一个 key 就是一条单车道。热点 key 不拆分、不分片、不合并消息，吞吐上限很快就到了。

第三是慢消息造成 head-of-line blocking。一个 actor 处理某条消息时同步调用慢下游，后面的快消息全部排队。即使其他 actor 很健康，这个 actor 的 mailbox 也会老化。actor handler 里做阻塞 I/O，尤其危险。

第四是超时消息仍被执行。sender 超时后重试，旧消息还在 mailbox 里，稍后 actor 又处理了一次。没有 command id、deadline、version check 或幂等，状态就会重复推进。

第五是 poison message。某条消息每次处理都 panic，actor 重启后又处理同一条，形成 restart loop。没有 dead-letter、失败计数、跳过策略或人工隔离，整个 actor 会被一条坏消息卡死。

第六是 priority 或 control mailbox 造成饥饿。高优先级消息源源不断，普通消息长期得不到处理。优先级能保护控制面，但必须看低优先级消息年龄，否则会有隐形饥饿。

第七是多 owner 或 stale owner。分布式 actor runtime 如果 owner 切换时 fencing 没做好，两个 worker 可能同时处理同一个 actor 的命令，单 owner 假设被破坏。旧 worker 的完成结果也可能覆盖新 owner 的状态。

第八是恢复语义不清。snapshot 太旧、tail log 不完整、command sequence 错、重复执行外部副作用、内存 mailbox 丢失未处理消息，都会让崩溃恢复后的 actor 状态和调用方预期不一致。

我会总结：actor mailbox 事故主要来自无界积压、热点单车道、慢消息阻塞、超时不等于取消、坏消息重启循环、优先级饥饿和 owner/fencing 错误。mailbox 让状态更容易推理，但也把容量和恢复问题集中到了 actor 粒度上。

## Q019. actor mailbox 的指标应该怎么设计才不会只看平均值？

**回答：**

actor mailbox 的指标尤其不能看平均值。一个系统里大多数 actor 都很空，少数热点 actor 已经爆了；全局平均 mailbox length 可能接近 0。要抓问题，必须按 actor、key、租户、消息类型看尾部。

第一类指标是 mailbox 长度和年龄。长度要看 p95/p99/max，年龄要看 oldest message age。oldest age 往往比长度更重要：长度 5 但最老消息 2 分钟，说明 actor 已经卡住；长度 500 但都在几毫秒内 drain，可能只是短 burst。

第二类是 enqueue 和 dequeue 速率。对每个热点 actor 看消息进入速度、处理速度、两者差值。只看总吞吐会漏掉某个 actor 持续入大于出。

第三类是处理耗时分布。按消息类型记录 handler service time p50/p95/p99，区分 CPU、I/O、锁等待、下游 RPC。慢消息造成的 head-of-line blocking，需要把当前正在处理的消息类型暴露出来。

第四类是超时和过期。消息入队时的 deadline、处理开始时剩余预算、处理完成时是否调用方已超时、过期消息被丢弃或跳过的数量。这样能发现“系统还在认真处理调用方早就不要的消息”。

第五类是失败和恢复。actor restart 次数、poison message 次数、dead letters、stashed messages、replay commands、snapshot load time、tail replay length、stale completion rejected、epoch mismatch。这些指标能看出 mailbox 是否保持单 owner 和可恢复顺序。

第六类是公平性和热点。top N actor mailbox length、top N oldest age、按租户和 actor type 的积压、单 actor 占总消息比例。全局平均没有意义，top N 才接近故障现场。

第七类是容量动作。bounded mailbox reject、dead-letter reason、backpressure trigger、passivation、sharding rebalance、priority starvation。mailbox 满了以后系统怎么处理，必须可见。

第八类是端到端延迟拆分。client send 到入队、入队到开始处理、处理耗时、结果回复耗时。actor 系统很多 p99 问题不在 handler，而在 mailbox 等待。

我会给出一个面试看板：per-actor mailbox length、oldest age、enqueue/dequeue rate、message wait p99、handler p99、expired-before-processing、dead letters、restart count、snapshot/replay lag、stale owner rejection、top hot actors。平均 mailbox length 只能当背景噪声，不能指导排障。

## Q020. actor mailbox 的正确性边界和性能边界分别是什么？

**回答：**

actor mailbox 的正确性边界是：它能把同一个 actor 的状态修改收敛到一个串行处理流，但这个保证只在 actor runtime 真的维护单 owner、单消费者、明确顺序和清晰失败语义时成立。

正确性上要先限定范围。mailbox 的顺序通常只对同一个 actor 有意义，不是全局顺序。不同 actor 并发执行，跨 actor 操作不是事务。两个 actor 之间的转账、库存扣减和订单状态推进，仍然需要 saga、事务日志、补偿或一致性协议。

同一个 actor 内部也要小心。sender 超时不代表消息撤销；priority mailbox 可能改变处理顺序；stash 会让消息暂时不处理；restart 后消息可能重投；持久化系统可能保证 command application 一次，但外部 side effect 仍可能重复。

状态安全来自单 owner，不来自魔法。如果同一个 actor 被两个 worker 同时执行，或者 actor handler 把内部可变状态引用泄漏给外部，mailbox 的正确性就破了。消息最好是不可变值，或者跨边界时序列化复制。

崩溃恢复的边界取决于持久化。内存 mailbox 不能保证 crash 后未处理消息还在。持久化 actor 要说明命令、事件、snapshot、tail replay 的关系。snapshot 只能作为恢复入口，必须绑定 actor id、command count 或 log position，不能随便拿一个状态文件当真相。

性能边界更直观：一个 actor 一次处理一个消息，所以单 actor 的吞吐有硬上限。actor 模型擅长把不同 key 并行起来，不擅长让同一个 key 无限并行。热点 actor 需要 sharding、拆分状态、批处理、合并消息、读写分离或把慢 I/O 移出去。

mailbox 容量也有限。unbounded mailbox 会吃内存，bounded mailbox 会拒绝或丢到 dead letters。容量策略会影响正确性：哪些消息能丢，哪些必须持久化，哪些要优先，哪些要返回失败。

慢 handler 会放大尾延迟。actor 内部如果同步调用慢下游，后面的消息全被挡住。常见做法是把慢 I/O 变成异步请求，收到结果后再发消息回 actor；或者把阻塞工作交给专门 dispatcher，但要处理状态版本和过期结果。

所以我会这样收束：actor mailbox 的正确性优势是 per-actor 串行状态机，边界是只覆盖单 actor、单 owner 和已定义顺序；性能优势是大量 actor 可并行，边界是单 actor 热点、mailbox 积压和慢消息阻塞。它把锁竞争换成了消息顺序和容量治理，问题更清楚，但没有消失。