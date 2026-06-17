# 11. worker pool、本地队列、调度与 backpressure

这一组问题讨论的是服务端容量治理：任务来了以后，要不要接、放到哪里、谁来执行、排不下时怎么处理、压力怎么往上游传。

面试里不要把 worker pool 讲成“开几个 goroutine 干活”这么简单。worker pool 真正解决的是资源边界问题：CPU、内存、连接池、下游 QPS、请求 deadline 和队列等待时间都要被纳入同一个设计。队列不是越大越安全，worker 也不是越多越快。很多线上雪崩不是因为系统没有并发，而是因为并发没有边界。

可以先抓住五条线：

```text
容量线：
  worker 数、队列长度、下游连接数、GOMAXPROCS、内存预算是不是匹配。

等待线：
  任务能等多久，队列里最老任务的年龄是否已经超过请求 deadline。

拒绝线：
  队列满时是阻塞、返回错误、丢弃、降级，还是转入持久化队列。

隔离线：
  慢任务、快任务、重要任务、低价值任务是不是混在同一个池里。

反馈线：
  下游过载后，压力是否能通过错误、deadline、限流、降级传回上游。
```

Go 官方 pipeline 文章是很好的入口：它把 fan-out/fan-in、bounded parallelism、buffer 选择和 cancellation 的边界讲得很清楚。`golang.org/x/sync/semaphore` 文档展示了用 weighted semaphore 限制并发的 worker pool 形态；`golang.org/x/time/rate` 文档则适合解释 rate limiting。Google SRE 的 overload 章节可以支撑一个工程判断：过载时快速拒绝、客户端自我节流、降级和丢弃低价值请求，常常比把请求全塞进队列更可靠。

## Q001. worker pool 解决什么问题？

**回答：**

worker pool 解决的不是“怎么并发”，而是“怎么有边界地并发”。在 Go 里，直接 `go func()` 很容易；难的是当请求量超过处理能力时，系统仍然能保持可预测的资源占用、可解释的延迟和明确的失败方式。

最朴素的 worker pool 长这样：

```go
type Job struct {
    ID int
}

func worker(ctx context.Context, jobs <-chan Job) {
    for {
        select {
        case <-ctx.Done():
            return
        case job, ok := <-jobs:
            if !ok {
                return
            }
            handle(job)
        }
    }
}

func start(ctx context.Context, n int, jobs <-chan Job) {
    for i := 0; i < n; i++ {
        go worker(ctx, jobs)
    }
}
```

这段代码背后的含义比代码本身重要。

第一，worker pool 限制同时执行的任务数。没有 pool 时，每个请求都可能启动一个 goroutine 去做昂贵工作。goroutine 虽然轻量，但任务背后的 CPU、内存、数据库连接、外部 RPC、文件句柄并不轻量。worker pool 把“最多同时处理多少个任务”变成显式参数。

第二，worker pool 把等待显式化。任务不能立刻执行时，会进入队列、被拒绝、被降级，或者阻塞提交方。没有队列设计时，等待往往藏在 goroutine 堆积、连接池等待、下游超时里，线上看起来像“偶发慢”，其实是系统已经过载。

第三，它保护下游。比如本服务能接 10,000 QPS，但数据库只承受 500 个并发查询。worker pool 可以把并发查询限制在数据库能承受的范围内。这个限制不是保守，而是避免把下游打挂后引发级联故障。

第四，它让 shutdown 和取消更可控。worker pool 可以统一接收 `context`，统一关闭输入队列，统一等待 worker 退出。随手启动 goroutine 的代码，后面加 graceful shutdown 往往很痛苦。

第五，它给观测留出口。worker 数、队列长度、入队等待时间、最老任务年龄、拒绝数、处理耗时、worker 忙闲比，这些指标都能直接暴露。没有 pool 时，你只能看 goroutine 数和 pprof，定位会晚很多。

worker pool 不一定要表现为“固定 N 个常驻 goroutine”。`x/sync/semaphore` 的官方例子展示了另一种形式：每次任务启动前先 `Acquire` 一个令牌，最多只允许 `maxWorkers` 个 goroutine in flight。它本质上也是并发上限：

```go
sem := semaphore.NewWeighted(int64(maxWorkers))

for _, job := range jobs {
    if err := sem.Acquire(ctx, 1); err != nil {
        return err
    }
    go func(job Job) {
        defer sem.Release(1)
        handle(job)
    }(job)
}
```

这种写法适合任务来源已经在当前 goroutine 里，且不需要一个显式队列的场景。固定 worker pool 更适合长期服务、队列消费、需要监控队列深度的场景。

worker pool 解决不了什么也要说清楚。

```text
它不能让 CPU-bound 任务无限变快。CPU 核心有限，worker 太多只是排队和切换。
它不能替代 context。任务已经过期时，worker 必须能停止。
它不能替代 rate limiting。外部入口仍然可能流量过大。
它不能替代持久化队列。进程崩溃后，内存队列里的任务会丢。
它不能自动区分任务价值。高优先级和低优先级要你自己隔离。
```

面试里可以这样答：

```text
worker pool 解决的是有界并发问题：限制同时执行的任务数，把等待和拒绝显式化，保护 CPU、内存、连接池和下游服务。它还能让 shutdown、取消和观测更清楚。它不是为了让所有任务更快，也不是无限队列；如果任务量超过处理能力，pool 必须配合队列容量、超时、拒绝、降级和 backpressure。
```

一句话：worker pool 是容量边界，不是简单的 goroutine 复用技巧。

## Q002. 固定大小 worker pool 和动态 worker pool 的 trade-off 是什么？

**回答：**

固定大小 worker pool 和动态 worker pool 的差别，本质上是“可预测性”和“弹性”的取舍。

固定大小 worker pool 的 worker 数在启动时确定，运行中不随负载变化：

```go
jobs := make(chan Job, queueSize)

for i := 0; i < workerN; i++ {
    go worker(ctx, jobs)
}
```

它的优点很直接。

第一，资源上限清楚。最多多少任务同时执行，最多占多少数据库连接，最多有多少并发下游 RPC，都可以算出来。做容量规划和故障演练时，这一点很值钱。

第二，行为稳定。不会因为短暂流量尖峰突然扩出很多 worker，把 CPU、连接池、下游服务打满。系统宁可在队列上体现压力，也不把压力偷偷放大。

第三，调试简单。固定 worker 数下，队列长度和处理耗时之间的关系更容易解释。动态扩缩容时，你看到延迟变化，很难马上判断是负载变了、worker 变了、还是下游变慢了。

第四，适合 CPU-bound 或资源强绑定任务。CPU-bound 任务通常不应该远超 `GOMAXPROCS`；数据库任务通常不应该超过连接池或下游承诺的并发。

它的缺点也明显。负载低时，worker 可能空闲；负载突增时，它只能让队列增长或拒绝，不能临时扩容吸收尖峰。对于 I/O 等待多、任务耗时波动大的场景，固定值太小会浪费下游空闲能力，太大又会在异常时放大压力。

动态 worker pool 会根据队列长度、worker 忙闲、延迟、CPU 或外部信号调整 worker 数：

```text
队列持续增长 -> 增加 worker
worker 长期空闲 -> 减少 worker
错误率或下游延迟升高 -> 不扩容，甚至缩容
```

动态池的优点是弹性。它可以在短时间内吸收突发流量，也可以在低峰减少常驻 goroutine 和资源占用。对于 I/O 密集型、任务耗时波动很大、下游容量也有弹性的系统，它有价值。

代价是控制器复杂。你要回答几个问题：

```text
扩容依据是什么：队列长度、队列年龄、CPU、p95 延迟，还是下游错误率？
多久扩一次：太快会抖动，太慢救不了尖峰。
最大 worker 数是多少：没有上限就是无界并发。
缩容怎么做：正在处理任务的 worker 不能直接杀。
下游变慢时是否扩容：很多系统一扩容就把下游彻底打挂。
```

动态池最危险的误用，是把队列增长直接解释成“worker 不够”。队列增长可能是下游慢、锁竞争、数据库连接池满、请求已经过期。此时扩 worker 会让更多任务同时冲向同一个瓶颈，吞吐不升，延迟和错误率上升。

一个务实的选择是：先用固定池，把容量、队列、拒绝、超时做好；只有在观测证明负载确实有弹性、下游也能承受时，再引入动态调整。动态也要有硬上限、冷却时间、缩容策略和过载保护。

面试里可以这样答：

```text
固定大小 worker pool 的优点是资源上限清楚、行为稳定、容易观测，适合 CPU-bound 或下游容量固定的任务；缺点是弹性差，突发时只能排队或拒绝。动态 worker pool 能吸收波动、减少低峰资源，但控制器复杂，容易把下游慢误判成 worker 不够，扩容后造成级联过载。工程上通常先用固定池和明确队列策略，再在有指标支撑时做有上限的动态扩缩。
```

一句话：固定池买的是可预测性，动态池买的是弹性，但动态池必须有刹车。

## Q003. 队列长度应该如何选择？

**回答：**

队列长度不是拍脑袋选一个“够大”的数。它应该来自等待时间预算、任务服务时间、到达速率、内存占用和失败策略。

先明确一个事实：队列的作用是吸收短暂波动，不是长期吞吐缺口。如果系统平均每秒只能处理 1,000 个任务，但入口长期进来 2,000 个任务，任何有限队列都会被填满；无界队列只会把失败延后。

选队列长度时可以按这个顺序思考。

第一，看请求还能等多久。假设一个在线请求总 deadline 是 200ms，网络和下游调用已经要花 120ms，那队列等待最好不要超过几十毫秒。队列里任务再多，只是制造过期工作。

```text
可排队时间 = 请求 deadline - 已消耗时间 - 预计处理时间 - 安全余量
```

如果可排队时间是 30ms，而 worker 平均每个任务处理 10ms，4 个 worker 理想情况下每 10ms 处理 4 个任务。队列长度设成几百没有意义，因为排到后面的请求大概率已经超时。

第二，看服务时间分布，不只看平均值。平均处理 10ms，但 p99 是 500ms，说明队列会被慢任务拖住。此时应该先分离慢任务、加 deadline、做隔离，而不是简单加队列。

第三，看突发模型。队列可以吸收短 burst，比如平时 100 QPS，偶尔 1 秒冲到 300 QPS。如果 worker 能在随后的低峰把积压处理完，队列有价值。如果 burst 持续 5 分钟，队列只是在积累延迟。

第四，看内存。队列里放的如果只是小结构体或 ID，容量可以相对大些；如果每个任务带着大请求体、图片、压缩包、数据库结果，队列会直接变成内存放大器。实际工程里常常只在队列里放引用或轻量 descriptor，大对象放外部存储或让调用方持有。

第五，看任务是否有新鲜度。搜索联想、监控采样、缓存刷新这类任务，旧任务价值会快速下降。队列太长只会处理过期任务。订单写入、账务处理这类任务不能随便丢，通常要用持久化队列和幂等语义，而不是依赖内存队列。

第六，用队列年龄比队列长度更可靠。长度是当前积压多少，年龄是最老任务等了多久。一个长度 100 的队列，如果每个任务 1ms，问题不大；一个长度 5 的队列，如果最老任务等了 10 秒，已经很危险。

提交任务时应该带 context，不要无限阻塞：

```go
func submit(ctx context.Context, q chan<- Job, job Job) error {
    select {
    case q <- job:
        return nil
    case <-ctx.Done():
        return ctx.Err()
    default:
        return ErrQueueFull
    }
}
```

这段代码选择了“满了就快速返回”。如果业务允许等待，可以去掉 `default`，但必须保留 `ctx.Done()`：

```go
func submitWait(ctx context.Context, q chan<- Job, job Job) error {
    select {
    case q <- job:
        return nil
    case <-ctx.Done():
        return ctx.Err()
    }
}
```

队列长度还要配合观测和压测校准。Google SRE 章节里有一个很实用的建议：要把组件压到失败，观察容量边界和过载模式。队列长度也一样，不能只在低负载下看。

面试里可以这样答：

```text
队列长度应该从等待时间预算反推，而不是越大越好。先看请求 deadline 还能给排队留多少时间，再看 worker 服务时间分布、突发流量、单个任务内存、任务新鲜度和失败策略。队列只适合吸收短期波动，不能弥补长期处理能力不足。线上要监控队列长度、最老任务年龄、入队等待时间、拒绝数和处理耗时。
```

一句话：队列长度的单位表面是“个数”，本质是“还能等多久”。

## Q004. 无界队列为什么危险？

**回答：**

无界队列危险，是因为它把过载伪装成“还接得住”。请求没有被拒绝，也没有被阻塞，只是被悄悄堆进内存。等问题暴露时，通常已经从局部过载变成全进程故障。

常见后果有几类。

第一，内存不可控。无界队列最终受限于进程内存，而不是业务 SLO。任务对象、闭包、请求体、上下文、日志字段都会被保留。队列越长，GC 要扫描和管理的对象越多，CPU 和延迟都会变差。

第二，延迟失真。入口看起来提交成功，但任务真正执行可能已经是几秒、几十秒之后。调用方如果已经超时，worker 处理的就是过期工作。系统吞吐数字还在增长，用户体验已经失败。

第三，backpressure 被切断。队列满本来应该告诉上游“我处理不过来了”。无界队列永远不满，压力不会往上游传，只会在本服务内部积累。

第四，重试会放大灾难。调用方看不到及时失败，等超时后发起重试；旧任务还在队列里，新任务又进队列。一次过载就变成重复工作堆积。

第五，shutdown 变慢。服务要优雅退出时，队列里还有大量任务。你要么等很久，要么丢任务，要么强杀进程。无界队列把这个选择推迟到了最糟糕的时候。

第六，公平性变差。先到的大批低价值任务可能把后来的高价值任务挡住。FIFO 队列下，重要请求排在垃圾请求后面，这就是典型的容量污染。

第七，故障定位变难。无界队列的错误常常不在入队点，而在很久后的 OOM、GC 抖动、下游超时、队列 drain 卡住。你看到的是“内存高”，根因是“系统早就应该拒绝”。

一个无界队列可能只是几行代码：

```go
type Queue struct {
    mu sync.Mutex
    xs []Job
}

func (q *Queue) Push(job Job) {
    q.mu.Lock()
    q.xs = append(q.xs, job)
    q.mu.Unlock()
}
```

它的问题不是 append 本身，而是没有容量、没有 deadline、没有拒绝策略、没有任务年龄指标。这样的队列会把所有上游压力变成 heap 压力。

有些系统确实需要“看起来无界”的任务接收能力，例如订单、支付、审计日志。这时通常要用持久化队列、磁盘日志、消息系统和幂等消费。它仍然有配额、分区、磁盘水位、消费 lag 和拒绝策略。不是内存里无限 append。

面试里可以这样答：

```text
无界队列危险，因为它切断 backpressure，把过载从入口失败变成内存增长、GC 抖动、过期任务、重试放大和 OOM。队列越长，延迟越不可控，调用方越可能超时重试，旧任务又继续占资源。需要高可靠接收时应使用持久化队列和明确配额，而不是进程内无界队列。
```

一句话：无界队列不是容量，是把失败藏起来。

## Q005. 队列满时应该阻塞、丢弃、降级还是返回错误？

**回答：**

队列满时没有一个通用答案。选择取决于任务价值、是否有 deadline、调用方能不能重试、任务是否可丢、下游是否已经过载。

可以先把四种策略分开。

第一，阻塞。适合调用方可以等待、等待本身就是 backpressure 的场景。比如内部批处理 pipeline，下游慢了，上游自然慢下来：

```go
select {
case q <- job:
    return nil
case <-ctx.Done():
    return ctx.Err()
}
```

阻塞必须带 context 或 timeout。无限阻塞会把 goroutine、连接、请求上下文都挂住，最后变成 goroutine leak 或连接池耗尽。

第二，返回错误。适合同步在线请求、调用方有 retry/backoff 逻辑、任务已经超过本服务可承受范围。HTTP 常见是 429 或 503，gRPC 常见是 `ResourceExhausted` 或 `Unavailable`。返回错误要快，最好带可重试语义和 `Retry-After` 这类提示。

第三，丢弃。适合低价值、可采样、可合并、过期就没意义的任务，比如 metrics、日志、实时推荐候选、UI 提示、缓存预热。丢弃也要有指标，不能静默丢。可以丢新任务，也可以丢旧任务：

```text
drop newest：保护队列里已有任务，适合每个任务都还有价值。
drop oldest：保留最新状态，适合状态刷新、监控采样、实时事件。
```

第四，降级。适合可以少做一点但仍然返回有用结果的场景。比如不做个性化、跳过低价值下游、返回缓存、降低采样率、只处理高优先级任务。Google SRE 的 cascading failures 章节也强调，过载时服务 degraded results 或丢弃不重要流量，比让整体成功率崩掉更好。

选择时可以问几个问题：

```text
这个任务过期后还有价值吗？
调用方还在等吗，deadline 还剩多少？
调用方能否安全重试，是否有幂等键？
失败是局部的，还是下游已经过载？
丢弃会影响正确性，还是只影响质量？
任务是否有优先级或租户配额？
```

几个典型选择：

```text
在线用户请求：短时间等待，超过预算返回错误或降级。
支付/订单：不要进内存无界队列，写持久化队列，保证幂等。
日志/metrics：队列满时采样或丢弃，并记录 drop count。
缓存刷新：丢旧任务或合并 key，保留最新刷新。
下游过载：快速失败或降级，避免重试风暴。
```

有一个常见错误：队列满了还继续同步重试入队。

```go
for {
    select {
    case q <- job:
        return nil
    default:
        // 立刻重试，CPU 空转
    }
}
```

这会把队列满变成 CPU 忙等。要么阻塞等待，要么 sleep/backoff，要么返回。

面试里可以这样答：

```text
队列满时的策略要看任务语义。调用方能等，就带 context 阻塞一小段时间；在线请求超出等待预算，应快速返回错误或降级；低价值、可采样、过期即无效的任务可以丢弃；必须处理的任务要进持久化队列而不是内存硬塞。无论选哪种，都要暴露 reject/drop/degrade 指标，不能静默吞。
```

一句话：队列满不是异常分支，而是系统容量设计的一部分。

## Q006. backpressure 和 rate limiting 的区别是什么？

**回答：**

backpressure 和 rate limiting 都是在控制流量，但它们控制的依据不同。

rate limiting 是按规则限制入口速率。比如每个用户每秒 100 次请求，每个 API key 每分钟 1,000 次请求，某个客户端最多 50 QPS。它通常发生在入口层、客户端、网关或调用下游之前。`golang.org/x/time/rate` 的 `Limiter` 就是典型 token bucket：`Allow` 立即判断，`Reserve` 预留未来 token，`Wait` 阻塞到 token 可用或 context 取消。

```go
lim := rate.NewLimiter(rate.Limit(100), 200)

if !lim.Allow() {
    return ErrRateLimited
}
```

rate limiting 的问题是，它不一定知道下游现在是不是真的健康。100 QPS 在平时没问题，但下游数据库正在抖动时，100 QPS 也可能过载。

backpressure 是从下游状态反向传播压力。下游处理不过来时，上游要慢下来、少发、丢弃、降级或返回错误。它不是固定速率，而是基于实际容量和队列状态的反馈。

Go pipeline 文章里有一个很典型的例子：下游只消费一部分结果就返回，上游如果还继续发送，会有 goroutine 卡住。解决方式是通过 `done` channel 告诉 upstream 停止发送。这就是进程内 backpressure/cancellation 的雏形：

```go
select {
case out <- v:
case <-done:
    return
}
```

在服务系统里，backpressure 可以表现为：

```text
队列满了，提交方阻塞或收到 ErrQueueFull。
semaphore Acquire 超时，调用方不再启动新任务。
下游返回 429/503，客户端降低发送量。
连接池等待时间变长，上游开始降级。
context deadline 剩余时间不足，直接拒绝新工作。
```

两者关系可以这样理解：

```text
rate limiting：入口前的限速器，通常按配额、令牌、窗口、租户策略工作。
backpressure：运行时反馈，告诉上游“我现在处理不过来了”。
```

它们通常要一起用。rate limiting 防止普通情况下的超额流量；backpressure 处理实际容量下降、下游抖动、队列变长、worker 饱和。Google SRE 的 client-side throttling 也是这个思路：当客户端看到后端开始拒绝请求时，客户端在本地自我节流，避免所有请求都打到后端再被拒绝。

面试里可以这样答：

```text
rate limiting 是按策略限制进入系统的速率，比如 token bucket、用户配额、API key QPS；backpressure 是下游处理不过来时，把压力反馈给上游，让上游阻塞、降速、降级或失败。rate limit 可以在系统健康时预防过量，backpressure 反映运行时真实拥塞。两者要配合，只有固定限速没有反馈，下游变慢时仍可能雪崩；只有 backpressure 没有入口限速，系统会一直在过载边缘抖动。
```

一句话：rate limiting 是预设红线，backpressure 是实时反馈。

## Q007. backpressure 应该从下游向上游如何传播？

**回答：**

backpressure 的方向应该和请求或数据流的方向相反：哪里处理不过来，哪里就要把压力传回调用方。不要在中间层用无界队列、无限重试、后台 goroutine 把压力吃掉。

在进程内，最直接的传播方式是 bounded channel、semaphore 和 context。

```go
func submit(ctx context.Context, q chan<- Job, job Job) error {
    select {
    case q <- job:
        return nil
    case <-ctx.Done():
        return ctx.Err()
    default:
        return ErrQueueFull
    }
}
```

如果队列满了，提交方立刻知道本地处理不过来。它可以返回错误、降级，或者稍后重试。`x/sync/semaphore` 的 `Acquire(ctx, n)` 也是同样思想：资源不可用时阻塞，但 context 到期就返回错误，不会无限等待。

在 pipeline 里，backpressure 可以通过阻塞 send 自然传播。下游不读，上游 send 会阻塞，上游再阻塞自己的上游。这个模型简单，但必须处理 early return。Go blog pipeline 文章强调，如果下游提前退出，要显式通知 upstream 停止发送，否则会泄漏 goroutine。

在服务之间，backpressure 需要协议化。常见信号包括：

```text
HTTP 429：调用太频繁或超过配额。
HTTP 503：服务当前不可用或过载。
Retry-After：告诉上游不要马上重试。
gRPC ResourceExhausted：资源耗尽、配额不足。
gRPC Unavailable：服务暂时不可用，可按策略重试。
deadline exceeded：请求已经没有足够预算。
```

传播时有几个要点。

第一，deadline 要往下传。上游给 200ms，已经花了 150ms，中间层不应该再给下游发一个默认 1s 的请求。否则用户已经走了，下游还在处理过期工作。

第二，重试要有预算。backpressure 返回错误后，如果所有上游立刻重试，会变成重试风暴。要用指数退避、jitter、最大尝试次数、全链路 retry budget。

第三，降级要在靠近业务语义的地方做。底层队列只知道满了，不知道哪些任务可以丢。上层应该区分核心请求、可选特性、预取、日志、刷新任务。

第四，客户端也要参与。Google SRE 的 client-side throttling 强调，当后端开始拒绝请求时，客户端本地就应减少发出请求的概率。这样比所有请求都打到后端再被拒绝更省资源。

第五，不要把 backpressure 藏在异步后台。比如 handler 收到请求后直接放进无界队列并返回 202，看起来入口没压力，但后台和下游可能已经爆了。除非有持久化队列、状态查询、幂等和容量指标，否则这是隐藏过载。

第六，backpressure 信号要可观测。每一层都应该有队列长度、最老任务年龄、拒绝数、超时数、降级数、重试数、下游错误码。没有指标，上游只会把所有失败当成随机错误。

面试里可以这样答：

```text
backpressure 要沿调用链从下游往上游传：本地用 bounded queue、semaphore Acquire(ctx)、context deadline；服务间用 429、503、ResourceExhausted、Unavailable、Retry-After 和明确的 deadline。中间层不能用无界队列或无限重试吞掉压力。上游收到压力后要减少发送、限速、降级、丢弃低价值任务或快速失败，并配合 retry budget 和 jitter，避免把过载放大。
```

一句话：backpressure 不是某一层的错误码，而是整条链路共同遵守的减压协议。

## Q008. 负载过高时系统应该优先保护吞吐还是延迟？

**回答：**

要先问业务目标。离线批处理通常更看吞吐；在线请求通常更看延迟，特别是尾延迟。服务端面试里，如果没有特殊说明，我会优先保护已接收请求的延迟和系统稳定性，而不是盲目追求入口吞吐。

原因很简单：过载时继续接请求，会让队列变长，队列变长会让更多请求超时，超时又会带来重试，重试再增加负载。最后系统可能还在“处理很多请求”，但成功完成的有用工作下降。这时入口吞吐是虚假的，真正应该看 goodput，也就是按 SLO 成功完成的请求数。

在线服务里，常见策略是：

```text
保护核心请求，拒绝或降级非核心请求。
限制队列等待时间，避免处理过期任务。
快速失败，给调用方明确错误。
保留少量容量给健康检查、控制面、恢复操作。
让低优先级流量先被 shed。
```

Google SRE 的 cascading failures 章节有一个很实际的观点：组件到达容量边界后，理想情况是对额外负载返回错误或降级结果，而不是显著降低已经成功处理请求的速率。换句话说，不要为了接下所有请求，把所有请求都拖慢到失败。

但这不是说吞吐不重要。对离线任务、日志处理、数据导入、批量转码，业务可能更关心总完成量。此时可以接受更高队列等待，只要任务不过期、内存可控、失败可恢复。即便如此，也要有队列上限和持久化策略，不能无界堆内存。

一个实用判断是：

```text
用户正在等待：保护延迟，超预算就拒绝或降级。
任务有明确截止时间：保护 deadline 前的完成率。
任务可延后且必须完成：保护持久化和最终吞吐。
任务低价值且可重建：过载时丢弃或采样。
```

还有一个常见误区：把 worker 数加大当成保护吞吐。CPU 已满时加 worker 不会提升吞吐；下游慢时加 worker 会增加下游并发；锁竞争高时加 worker 会让等待更多。过载时更应该先控制 admission，再看瓶颈在哪里。

面试里可以这样答：

```text
在线系统负载过高时，我会优先保护延迟和稳定性，准确说是保护 goodput：在 SLO 内成功完成的请求数。继续接收所有请求会让队列变长、请求过期、重试放大，最后吞吐数字还在但用户看到的是失败。批处理可以更偏吞吐，但也要有持久化和容量上限。策略上通常是限流、快速失败、降级、丢低价值任务，保住核心流量。
```

一句话：在线服务过载时先保住能按时完成的请求，别用长队列制造虚假吞吐。

## Q009. head-of-line blocking 是什么？

**回答：**

head-of-line blocking 指的是队首的慢任务挡住后面的任务，导致后面的任务明明可以很快完成，却因为排队顺序或共享资源被迫等待。

在 worker pool 里，一个简单例子是所有任务共用同一个 FIFO 队列：

```go
jobs := make(chan Job, 100)

for i := 0; i < 4; i++ {
    go worker(jobs)
}
```

如果前面进来一批慢任务，每个都要跑 5 秒，后面一个只要 5ms 的健康检查任务也要排队。worker pool 没有坏，它只是按队列顺序执行。问题在于不同服务时间、不同优先级的任务被混在了一条队列里。

head-of-line blocking 有几种常见形态。

第一，队列级 HoL。单 FIFO 队列里，慢任务排在快任务前面。后面的快任务排队时间被慢任务放大。

第二，worker 级 HoL。worker 拿到一个长任务后一直被占用。worker 数少时，少量长任务就能占满所有 worker。

第三，连接级 HoL。一个连接或一个流上的前一个请求卡住，后续请求不能及时处理。网络协议里也常讨论这个问题；在业务系统里，单连接串行消费、单 partition 串行消费也会发生。

第四，锁级 HoL。一个慢任务持有全局锁，后面所有快任务都等锁。看起来是 worker pool 慢，实际是临界区设计问题。

第五，租户级 HoL。一个大客户或热点租户把共享队列塞满，其他租户的轻量请求排在后面。此时公平性和隔离比单纯吞吐更重要。

解决方式要看场景。

```text
按任务类型拆队列：CPU、I/O、慢任务、快任务分开。
按优先级调度：核心请求优先，低价值任务延后。
设置任务 deadline：过期任务不要继续占队列。
限制重任务并发：给大任务单独 semaphore。
分片队列：按 key/tenant/shard 隔离，避免单热点拖全局。
work stealing：空闲 worker 可以从其他队列拿任务，但要避免破坏隔离。
```

也可以在任务层做拆分。一个 10 秒的大任务如果可以拆成多个小块，中间让出执行权，快任务就不容易被完全挡住。但这会增加调度和合并复杂度。

观测上，平均延迟很容易掩盖 HoL。要看：

```text
队列等待时间分布
最老任务年龄
按任务类型拆分的 p95/p99
每个 worker 当前任务耗时
锁等待时间
租户/优先级维度的排队情况
```

面试里可以这样答：

```text
head-of-line blocking 是队首慢任务挡住后面的快任务。在线程池或 worker pool 里，慢任务、快任务、不同优先级任务共用 FIFO 队列时很常见。它会让平均吞吐看起来正常，但快任务 p99 被慢任务拖高。常见解决方式是按任务类型或优先级拆队列，限制重任务并发，设置 deadline，做租户隔离，必要时使用优先级队列或 work stealing。
```

一句话：HoL 的本质是共享队列里缺少隔离，慢任务把快任务的等待时间带坏。

## Q010. 不同类型任务共用一个线程池有什么风险？

**回答：**

不同类型任务共用一个线程池，最大的风险是互相污染容量。CPU-bound、I/O-bound、慢任务、快任务、高优先级、低优先级任务，对 worker 数、队列长度、超时和失败策略的要求都不一样。混在一个池里，最后谁都不舒服。

常见风险有这些。

第一，慢任务拖垮快任务。比如报表任务和登录请求共用一个池。报表任务每个跑 10 秒，登录请求只要 20ms。高峰时几个报表任务占满 worker，登录请求只能排队，用户看到的是核心链路变慢。

第二，I/O-bound 任务占住 worker。一个任务卡在外部 HTTP、数据库、文件系统上，worker 就不能处理其他任务。CPU 可能很空，但线程池满了。此时应该用单独的 I/O 并发限制和 deadline，而不是让它占住核心请求池。

第三，CPU-bound 任务造成调度竞争。CPU-heavy 任务太多时，会和所有请求争 CPU。即使 worker pool 限制了 goroutine 数，也可能把核心请求的 p99 拉高。

第四，优先级反转。低价值任务先把队列占满，高价值任务后到，只能被拒绝或等待。队列是公平的，业务不是。

第五，死锁或自阻塞。任务 A 在池里执行，又提交子任务 B 到同一个池，并等待 B 完成。如果所有 worker 都在等自己的子任务，池就卡住：

```go
func task(pool *Pool) {
    done := make(chan struct{})
    pool.Submit(func() {
        defer close(done)
        subtask()
    })
    <-done
}
```

如果池里所有 worker 都在跑 `task`，没有空 worker 执行 `subtask`。这类 bug 在线上很难看，表现像随机卡死。

第六，队列策略无法统一。日志任务满了可以丢，支付任务不能丢；搜索建议过期就没意义，账务任务必须落盘；缓存刷新可以降级，用户写请求要返回明确错误。一个共享队列无法同时满足这些策略。

第七，观测变模糊。池的队列长度升高，你不知道是报表、日志、下游慢、CPU 计算还是某个租户打爆。没有按任务类型拆指标，排查只能靠采样和猜。

第八，故障会横向扩散。一个低优先级下游变慢，占住共享池，最后把不相关的核心链路也拖慢。这就是缺少 bulkhead。船舱没有隔板，一个洞会让整条船进水。

工程上更稳的做法是按资源和价值隔离：

```text
CPU-heavy 任务单独池，worker 数接近 CPU 预算。
I/O-heavy 任务单独池，带连接池和 deadline。
核心请求单独队列，低价值任务不能挤占。
慢任务和快任务分开，避免 HoL。
每个租户或优先级有配额。
共享下游用 semaphore 统一限并发。
```

不是说永远不能共用。低流量、同质任务、服务时间相近、失败策略一致时，共用一个池很简单，也够用。一旦任务类型差异明显，就要拆。拆池不是为了架构好看，是为了让容量边界和失败模式可控。

面试里可以这样答：

```text
不同类型任务共用一个池，风险是容量互相污染：慢任务拖快任务，I/O 等待占住 worker，CPU-heavy 任务抢 CPU，低价值任务挤占高价值任务，任务在同一池里提交子任务还可能自阻塞。队列满时也很难统一策略，因为有的任务能丢，有的必须持久化。工程上通常按资源类型、优先级、租户和任务耗时做隔离，用独立队列、独立 worker pool、semaphore 和 per-class 指标保护核心链路。
```

一句话：共享池省代码，但会把不同任务的失败模式绑在一起。

## Q011. CPU-bound 和 I/O-bound 任务的线程池大小应该如何估算？

**回答：**

先把两个词说清楚。CPU-bound 任务的瓶颈主要在 CPU 时间，比如压缩、加密、图片处理、排序、规则计算。I/O-bound 任务的大部分时间在等外部资源，比如数据库、RPC、磁盘、对象存储、消息队列。两类任务的 worker 数不能用同一套直觉估。

CPU-bound 的起点通常是可用 CPU 并行度。Go 里要看 `GOMAXPROCS`，不是只看机器标称核数。Go runtime 文档里说，`GOMAXPROCS` 限制同时执行用户级 Go 代码的 CPU 数；现代 Go 默认会结合逻辑 CPU、CPU affinity，以及 Linux cgroup CPU quota 选择默认值。所以容器里写 `runtime.NumCPU()` 只是一个参考，真正要看当前 runtime 的并行度预算。

CPU-bound 估算可以从这里起步：

```text
worker_count ~= GOMAXPROCS
```

如果服务还要处理网络、日志、GC、其他 goroutine，可以留一点余量：

```text
worker_count ~= max(1, GOMAXPROCS - reserve)
```

`reserve` 不是固定公式。一个 2 核容器很难再留 1 核；一个 32 核服务可能给核心 worker 24-28 个并发，把剩余 CPU 留给网络栈、GC、metrics、控制面和临时峰值。真正上线前要压测，因为 CPU-bound 任务还会受缓存命中、分配率、锁竞争、SIMD、cgo、GC assist 影响。

CPU-bound 任务不要轻易把 worker 开到 CPU 数的好几倍。开多了只会让更多 goroutine 在 runnable 队列里竞争时间片。表现通常是：

```text
CPU 接近 100%
worker utilization 接近 100%
runtime /sched/latencies:seconds 变高
p99 latency 升高
吞吐没有继续上涨，甚至下降
```

这时加 worker 没用，应该优化执行逻辑、减少分配、拆隔离池，或者扩机器。

I/O-bound 的估算要从外部资源和等待时间入手。一个任务如果 5ms 在本地 CPU，95ms 等数据库或 RPC，它占 worker 100ms，但真正烧 CPU 只有 5ms。此时 worker 数可以大于 CPU 数，但上限要受下游容量控制。

常用估算有两条线。

第一条，用 Little's Law 估 in-flight：

```text
in_flight ~= throughput * average_latency
```

比如目标是 1000 tasks/s，每个外部 RPC 平均 50ms，稳定态平均 in-flight 约为：

```text
1000 * 0.050 = 50
```

这表示只为维持这个吞吐，平均同时要有 50 个任务在系统里。考虑尾延迟、突发和重试，worker 数可能要更高一点，但不能超过下游的连接池、QPS 配额和服务端并发能力。

第二条，用下游容量反推：

```text
worker_count <= min(
    db_connection_pool_size,
    downstream_max_concurrency,
    outbound_fd_or_socket_budget,
    memory_budget_per_task,
    retry_budget_limit,
)
```

I/O-bound 任务最容易犯的错是“CPU 还很空，所以继续加并发”。CPU 空不代表系统健康。数据库连接池满、RPC p99 上升、socket 堆积、重试变多，都会让本服务看起来还能接请求，但下游已经开始抖。

一个实用流程是：

```text
CPU-bound：
1. worker 从 GOMAXPROCS 或略低开始。
2. 压测吞吐和 p99。
3. 看 CPU profile、heap profile、mutex/block profile。
4. worker 增加后吞吐不涨，说明 CPU 或锁/内存已经到瓶颈。

I/O-bound：
1. 先确认下游并发预算和连接池大小。
2. 用目标吞吐 * 平均服务时间估 in-flight。
3. worker 上限不能超过下游可承受并发。
4. 看排队时间、外部调用 p95/p99、重试数、超时数。
```

Go 里也可以不用常驻 worker，而用 `errgroup.SetLimit` 或 `semaphore.Weighted` 限制 active goroutine。官方 `errgroup` 文档说 `SetLimit` 会限制 group 内 active goroutine 的最大数量，超过限制时 `Go` 会阻塞；`TryGo` 则在超限时直接返回 false。这个语义很适合“没有显式本地队列，只想限制并发”的批处理或扇出调用。

面试里可以这样答：

```text
CPU-bound 任务先按 GOMAXPROCS 估，通常接近可用 CPU 数，最多留一点给网络、GC 和控制面；worker 开太多只会增加 runnable 队列和调度竞争。I/O-bound 任务要按下游容量和 Little's Law 估：目标吞吐乘以平均服务时间可以得到平均 in-flight，再用连接池、下游限额、内存预算和超时预算设上限。两者最后都要靠压测校准，不能只靠公式。
```

一句话：CPU-bound 看 CPU 并行度，I/O-bound 看等待时间和下游容量。

## Q012. 优先级队列会带来 starvation 吗？

**回答：**

会。严格优先级队列最容易让低优先级任务饿死。只要高优先级任务持续到达，低优先级任务就一直排不到执行机会。它不是理论上的小问题，线上会表现为“系统没挂，但某一类任务永远做不完”。

最简单的模型是这样：

```text
worker = 10
high priority arrival = 每秒 10 个，每个 100ms
low priority queue = 已经积压 10000 个
```

高优先级任务刚好吃满全部 worker，低优先级任务的等待时间会无限增长。监控上看，worker utilization 很高，高优先级 p99 也许还不错，但低优先级队列年龄一直上涨。

严格优先级还有几个副作用。

第一，低优先级任务可能过期。比如离线统计、缓存刷新、邮件发送、搜索索引构建。如果一直被挤压，等它执行时结果已经没有价值。

第二，低优先级任务可能占内存。优先级队列不执行它们，但它们仍在队列里。任务对象、payload、上下文、trace 信息、闭包引用都会留下。

第三，会出现业务层面的“隐性不公平”。高优先级不一定等于高价值。某个租户或某类请求如果总被标成高优先级，就可能挤掉其他正常流量。

第四，可能加剧优先级反转。低优先级任务如果持有共享锁、连接或缓存更新权限，高优先级任务反而会等它释放资源。队列层面优先，不代表执行路径里也优先。

常见缓解方法有几种。

```text
aging：
  任务等待越久，有效优先级越高。

quota：
  每个优先级、租户或任务类型有自己的执行份额。

weighted fair queue：
  高优先级拿更多份额，但低优先级仍能前进。

reserved capacity：
  给核心任务保留容量，也给后台任务保留最低容量。

deadline-aware scheduling：
  已经过期的任务直接丢弃，接近 deadline 的任务优先处理。

separate pools：
  不同优先级使用独立队列和 worker，避免互相污染。
```

一个常见做法是“高优先级优先，但低优先级有保底”。比如每 10 个任务里最多取 8 个高优先级，至少留 2 个名额给普通任务。它牺牲了一点高优先级吞吐，换来系统长期可收敛。

优先级队列还要配合观测，至少要按 priority 暴露这些指标：

```text
queue_depth{priority}
oldest_task_age_seconds{priority}
enqueue_total{priority}
dequeue_total{priority}
drop_total{priority, reason}
queue_wait_seconds{priority}
execution_seconds{priority}
```

只看总队列长度会漏掉 starvation。总队列长度稳定，不代表每个优先级都在前进。

面试里可以这样答：

```text
优先级队列会带来 starvation，特别是严格优先级策略下，高优先级任务持续到达时，低优先级任务可能永远没有执行机会。工程上要用 aging、配额、weighted fair queue、保留容量、deadline-aware drop 或独立池来保证低优先级也能前进。还要按优先级看最老任务年龄和排队时间，不能只看总队列深度。
```

一句话：优先级解决“谁先做”，但必须再回答“谁一定能被做”。

## Q013. 公平调度和吞吐优化之间有什么冲突？

**回答：**

公平调度和吞吐优化经常不是同一个目标。公平调度关心每个任务、租户、优先级或连接都能得到服务；吞吐优化关心单位时间内完成更多任务。两者有重叠，但冲突很常见。

先看公平。公平调度可能会让 scheduler 在不同队列、租户或任务类型之间轮转：

```text
tenant A -> tenant B -> tenant C -> tenant A -> ...
```

这样可以避免 A 把 B、C 挤死。问题是，轮转会打断局部性。如果 A 的任务都访问同一批缓存、同一个 shard、同一个连接，连续处理 A 可能更快；强行轮转会增加 cache miss、锁切换、连接切换和上下文管理成本。

再看吞吐。吞吐优化常会偏向这些策略：

```text
批处理，减少系统调用和锁开销。
短任务优先，提高完成数。
同类任务连续执行，提高缓存命中。
固定 worker 处理固定 shard，减少跨队列同步。
让最快完成的路径拿更多资源。
```

这些策略很有效，但容易牺牲公平。短任务优先会让长任务长期等待；热点 shard 连续处理会让冷门租户延迟变高；批处理会让单个请求多等一个 batch window。

FIFO 看起来公平，其实也不总公平。慢任务排在前面会造成 head-of-line blocking，让后面的快任务一起等。严格 FIFO 对到达顺序公平，对延迟目标不公平。

优先级调度也一样。它对业务重要性更公平，但对低优先级任务不公平。吞吐优化倾向于处理“最划算”的任务，公平调度倾向于保护“不能被饿死”的任务。

工程上要先定义“公平对象”。到底对谁公平？

```text
对请求公平：先到先服务。
对用户公平：每个用户有类似等待时间。
对租户公平：每个租户有配额和隔离。
对任务类型公平：慢任务不能拖死快任务，快任务也不能饿死慢任务。
对业务价值公平：核心链路优先，后台任务保底。
```

定义错了，调度策略就会错。比如多租户系统按请求 FIFO，看起来公平，实际可能被大租户打满；按租户 weighted fair queue，吞吐可能略降，但小租户不会被挤出去。

一个成熟回答通常会把公平和吞吐拆开：

```text
吞吐优化：
  批处理、局部性、减少锁竞争、减少跨 worker 迁移。

公平保护：
  配额、限并发、租户隔离、aging、deadline、低优先级保底。

权衡指标：
  总吞吐、p50/p99、各租户 p99、最老任务年龄、drop rate、worker utilization。
```

不要只说“公平会降低吞吐”。更准确的说法是：公平会限制某些局部最优行为，比如连续处理同类任务、让热点流量占满资源；吞吐优化会放大这些局部最优，所以需要边界。

面试里可以这样答：

```text
公平调度要防止某类任务或租户饿死，吞吐优化则倾向于批处理、局部性和优先完成便宜任务。冲突在于：公平会增加轮转和隔离成本，吞吐优化可能让热点任务持续占用资源。FIFO、严格优先级、短任务优先都只对某个维度公平。工程上要先定义公平对象，再用配额、weighted fair queue、aging 和隔离池控制，同时用总吞吐和分组 p99 一起评估。
```

一句话：吞吐问“总共做得多不多”，公平问“有没有人一直等不到”。

## Q014. work stealing 适合什么场景？

**回答：**

work stealing 适合任务量不均匀、任务会动态产生子任务、每个 worker 有本地队列的场景。它的基本思路很朴素：worker 平时优先处理自己的本地队列；当自己没活了，再去别的 worker 那里偷一部分任务。这样既保留局部性，又能在负载不均时把闲置 worker 用起来。

典型场景有几类。

第一，fork-join 或 DAG 任务。比如并行递归、搜索、编译、图遍历、分治计算。任务在执行过程中会生成子任务，提前很难知道每个分支的工作量。

```text
处理一个目录：
  遇到文件 -> 计算 hash
  遇到子目录 -> 生成子任务

某些目录有 10 个文件，某些目录有 100000 个文件。
固定切分很容易不均匀，work stealing 可以让空闲 worker 去偷大目录产生的任务。
```

第二，任务耗时差异大，但不要求严格顺序。比如图片处理、规则匹配、批量数据清洗。某些任务很快，某些任务很慢，用单一全局队列会有锁竞争；用固定分片队列又可能某个分片积压。work stealing 可以在两者之间取平衡。

第三，希望保留缓存局部性。worker 先处理本地队列，相关任务更可能被同一个 worker 连续执行。只有本地没活时才偷，减少不必要迁移。

第四，全局队列锁竞争明显。所有 worker 都抢一个队列时，吞吐可能被队列锁卡住。本地队列加偷取，可以把大部分 push/pop 留在本地，只有偷取时访问别人队列。

work stealing 不适合这些场景。

```text
任务非常均匀：
  简单 round-robin 或固定分片就够了。

任务很小：
  偷取、同步和调度成本可能超过任务本身。

强顺序要求：
  比如严格 FIFO、严格优先级、精确按 offset 提交。

强租户隔离：
  随便偷任务可能打破租户配额和资源边界。

I/O-bound 且主要瓶颈在下游：
  偷更多任务不能让数据库更快，只会放大下游压力。

任务绑定状态或线程：
  比如必须在同一连接、同一 shard、同一 OS thread 上执行。
```

Go runtime 的调度器本身有 work stealing 思路：P 有本地 run queue，空闲 P 会从其他 P 或全局队列找可运行 goroutine。业务层是否还要做 work stealing，要看你有没有自己的任务队列、优先级、租户隔离和下游容量边界。不要因为 runtime 有 work stealing，就以为业务 worker pool 也自动解决了负载均衡。

实现时要注意几个不变量：

```text
任务最多被执行一次。
偷取和本地 pop 的并发访问要有清晰同步。
任务完成、取消、超时都要能回收。
偷取不能绕过优先级、租户配额和 deadline。
偷取失败不能忙等烧 CPU。
```

面试里可以这样答：

```text
work stealing 适合任务动态产生、耗时不均、没有严格顺序要求、又希望减少全局队列竞争的场景，比如 fork-join、分治计算、图搜索、批量处理。worker 优先处理本地队列，空闲时去偷别人的任务，可以兼顾局部性和负载均衡。它不适合强 FIFO、严格优先级、强租户隔离、任务很小或瓶颈在下游 I/O 的场景，因为偷取成本和语义破坏可能大于收益。
```

一句话：work stealing 是给“不均匀的可并行工作”补平负载，不是给所有队列系统通用加速。

## Q015. 任务本地排队时间应该如何度量？

**回答：**

本地排队时间要从任务被系统接纳开始算，到 worker 真正开始执行为止。不要用请求总耗时倒推，也不要只看队列长度。队列长度是某一刻的存量，排队时间才是用户和任务实际感受到的等待。

最小状态机是：

```text
submitted/admitted -> enqueued -> dequeued/started -> finished -> acked
```

对 worker pool 来说，最重要的时间戳是：

```text
enqueue_time：任务进入本地队列的时间。
start_time：worker 从队列取出并准备执行的时间。
finish_time：任务执行函数返回的时间。

queue_wait = start_time - enqueue_time
execution_time = finish_time - start_time
```

Go 里用 `time.Now()` 记录，再用 `time.Since(t)` 或 `end.Sub(start)` 计算即可。Go 的 `time.Time` 在同一进程内通常带单调时钟读数，用来算间隔可以避开墙钟回拨问题。跨进程或跨机器不要直接拿两个机器的 wall clock 相减，分布式链路要么在每个节点本地打阶段耗时，要么依赖 trace 系统处理时钟问题。

一个简单结构可以这样写：

```go
type Job struct {
    ID         string
    EnqueuedAt time.Time
    Deadline   time.Time
    Payload    any
}

func submit(q chan<- Job, payload any) error {
    job := Job{
        ID:         newID(),
        EnqueuedAt: time.Now(),
        Deadline:   time.Now().Add(200 * time.Millisecond),
        Payload:    payload,
    }

    select {
    case q <- job:
        return nil
    default:
        return ErrQueueFull
    }
}

func worker(q <-chan Job) {
    for job := range q {
        start := time.Now()
        observeQueueWait(start.Sub(job.EnqueuedAt))

        handle(job)

        observeExecution(time.Since(start))
    }
}
```

实际系统里还要记录“最老任务年龄”：

```text
oldest_task_age = now - queue_head.enqueue_time
```

队列长度只告诉你有多少任务，最老任务年龄告诉你有没有任务已经等到失去意义。很多过载系统在崩之前，总队列长度看着还没爆，但最老任务年龄已经超过请求 deadline。

本地排队时间应该做直方图，而不是只做平均值：

```text
queue_wait_seconds_bucket{task_type, priority, pool}
queue_wait_seconds_p50
queue_wait_seconds_p95
queue_wait_seconds_p99
oldest_task_age_seconds{queue}
```

平均值会掩盖尾部。一个队列里 99% 的任务等 1ms，1% 的任务等 10s，平均值可能还不刺眼，但 p99 已经说明调度策略有问题。

还要区分“入队前等待”和“入队后等待”。调用方可能在提交任务时因为队列满而阻塞：

```go
select {
case q <- job:
    // admitted
case <-ctx.Done():
    return ctx.Err()
}
```

这段阻塞时间不属于队列内部等待，但属于 admission wait。面试时说清楚会加分：提交方等待、队列内部等待、worker 执行，是三个不同阶段。

如果使用优先级队列、延迟队列或 work stealing，本地排队时间还要按“任务被哪个队列持有”打标签。否则一个任务先在全局队列等，后来被移到本地队列，本地等待看起来很短，真实等待被吃掉了一段。

面试里可以这样答：

```text
本地排队时间应该在任务进入队列时打 enqueue timestamp，在 worker 真正开始执行时打 start timestamp，queue_wait = start - enqueue。它要用 histogram 和 oldest task age 观测，按任务类型、优先级、队列或 pool 分组。还要把 admission 阻塞、队列等待和执行耗时分开，不能用总 latency 倒推，也不能只看 queue depth。
```

一句话：排队时间要在任务生命周期里打点，不能靠感觉从端到端延迟里猜。

## Q016. 排队时间和执行时间如何区分？

**回答：**

排队时间是任务已经被接纳，但还没开始执行的时间。执行时间是 worker 已经拿到任务，任务处理函数正在运行的时间。两者都算在端到端延迟里，但含义完全不同。

可以用这条时间线：

```text
client_send
  -> admitted
  -> enqueued
  -> dequeued
  -> handler_start
  -> downstream_call_start
  -> downstream_call_end
  -> handler_finish
  -> writeback_done
  -> client_response
```

对 worker pool，最常用的划分是：

```text
admission_wait = enqueued - submit_start
queue_wait     = handler_start - enqueued
execution_time = handler_finish - handler_start
writeback_time = writeback_done - handler_finish
```

不要把 worker 拿到任务后的所有时间都叫 CPU 执行时间。执行阶段里可能还包含：

```text
本地 CPU 计算
等待数据库连接
等待外部 RPC
等待锁
等待磁盘
等待 rate limiter
写结果或 ack
```

所以执行时间高，不一定是 CPU 高。要继续拆。CPU profile 看 CPU 热点；block profile 看 goroutine 阻塞；mutex profile 看锁等待；runtime trace 可以看到 goroutine 创建、阻塞、解除阻塞、系统调用、GC、P start/stop 等事件。`runtime/trace` 官方文档还提供 user annotation，能用 task、region、log 把逻辑操作和多个 goroutine 关联起来。

一个比较清楚的打点方式是：

```go
func worker(q <-chan Job) {
    for job := range q {
        start := time.Now()
        observe("queue_wait", start.Sub(job.EnqueuedAt), job.Labels)

        err := traceRegion(job.Context, "execute", func() error {
            return handle(job)
        })

        finish := time.Now()
        observe("execution", finish.Sub(start), job.Labels)
        observeResult(err, job.Labels)
    }
}
```

如果不想引入 trace，普通 metrics 也能做：

```text
job_admission_wait_seconds
job_queue_wait_seconds
job_execution_seconds
job_downstream_seconds
job_writeback_seconds
job_total_seconds
```

这些指标之间应该能加起来解释总耗时。不能完全相等没关系，真实系统会有采样、并发、异步回调和观测开销，但数量级应该对得上。

常见误判有几个。

第一，只看端到端 p99。端到端 p99 上升，可能是队列等太久，也可能是执行慢了。前者要减小入队、限流或扩 worker；后者要优化 handler 或下游。

第二，把 channel send 阻塞算进 queue wait。提交方阻塞在 `q <- job` 时，任务还没进入队列。那是 admission wait，更接近 backpressure。

第三，把等待连接池算成“业务执行”。从 worker 角度它在执行，从资源角度它在等另一个队列。连接池也是队列，应该单独观测。

第四，任务超时后还执行。这样 execution_time 可能很好看，但用户已经超时。要同时看 deadline miss 和 stale execution。

面试里可以这样答：

```text
排队时间是 enqueued 到 handler_start，执行时间是 handler_start 到 handler_finish。提交方阻塞在入队前要算 admission wait，结果写回要算 writeback。执行时间还要再拆 CPU、锁等待、I/O、下游调用和连接池等待。定位时要用阶段化 metrics、trace region、CPU/block/mutex profile，而不是只看端到端延迟。
```

一句话：排队慢是容量和调度问题，执行慢是处理路径或下游问题，修法不一样。

## Q017. 队列深度、worker utilization、p99 latency 之间有什么关系？

**回答：**

这三个指标是 worker pool 里最容易一起看的指标，但它们不是线性关系。

先定义一下：

```text
queue_depth：
  当前等待执行的任务数。

worker utilization：
  worker 忙碌时间 / worker 总可用时间。

p99 latency：
  99% 的任务在这个耗时以内完成，通常包括排队和执行。
```

低负载时，队列深度接近 0，worker utilization 不高，p99 主要由执行时间决定。此时加队列、调度优化都没什么意义，应该看 handler 本身和下游。

负载接近容量时，worker utilization 上升，队列开始积压。这里的变化会很陡。一个服务从 60% utilization 到 80% utilization 可能还平稳，从 90% 到 98% 时 p99 会突然炸。原因很简单：只要短时间 arrival rate 超过 service rate，任务就会排队；利用率越接近 100%，系统越没有余量消化突发。

可以用一个小例子：

```text
worker = 20
平均执行时间 = 50ms
理论服务能力 = 20 / 0.050 = 400 tasks/s
```

如果稳定进来 200 tasks/s，平均只需要 10 个 worker，队列大多为空。

如果进来 380 tasks/s，平均需要 19 个 worker。看起来还没超过 400，但任何服务时间波动、GC pause、下游慢一点、批量突发，都会让队列堆起来。p99 首先变差，因为尾部任务吃到最多等待。

如果进来 450 tasks/s，已经超过服务能力。队列会持续增长。只要不拒绝或降级，p99 会跟着队列年龄增长，最后变成超时风暴。

Little's Law 可以解释平均关系：

```text
L = lambda * W
```

`L` 是系统里的平均任务数，`lambda` 是有效吞吐，`W` 是平均停留时间。worker pool 里可以粗略理解为：

```text
queue_depth + running_tasks ~= throughput * total_latency
```

注意这是平均关系，不是 p99 公式。p99 受服务时间分布、突发、HoL blocking、优先级、GC、锁竞争影响更大。平均队列深度不高，p99 也可能很差，因为队列里有少量任务被慢任务挡住。

这三个指标组合起来看更有用：

```text
queue_depth 上升，utilization 高，p99 上升：
  worker 或下游容量不足，系统在排队。

queue_depth 上升，utilization 低：
  调度器、队列锁、dispatcher、优先级策略或 worker 唤醒有问题。

queue_depth 低，utilization 高，p99 上升：
  执行路径慢，可能是 CPU、锁、I/O 或下游。

queue_depth 低，utilization 低，p99 上升：
  可能是外部依赖、长尾任务、少量慢请求、GC/STW、网络抖动，或者观测口径不一致。

utilization 接近 100%，吞吐不再增长：
  已到容量边界，加请求只会增加排队。
```

worker utilization 也不能单独当健康指标。后台批处理系统希望 utilization 高；在线服务如果 utilization 长时间接近 100%，通常说明没有突发余量。对延迟敏感服务，长期保持一些 idle capacity 是正常的成本。

面试里可以这样答：

```text
queue depth 是等待存量，worker utilization 是执行资源忙碌程度，p99 latency 是尾部任务的真实体验。利用率接近 100% 时，队列和 p99 往往非线性上升，因为系统没有余量吸收突发。Little's Law 能解释平均关系：系统内任务数约等于吞吐乘以平均停留时间，但不能直接推出 p99。定位时要联合看：队列升且 worker 忙是容量不足；队列升但 worker 不忙是调度或队列问题；队列不深但 p99 高，多半在执行路径或下游。
```

一句话：队列深度告诉你积压，utilization 告诉你资源忙不忙，p99 告诉你尾部有没有失控。

## Q018. Little's Law 在容量规划中如何使用？

**回答：**

Little's Law 是容量规划里最实用的 sanity check：

```text
L = λ * W
```

`L` 是系统内平均并发量或平均任务数，`λ` 是有效到达率或吞吐，`W` 是任务在系统里的平均停留时间。John D. C. Little 1961 年的论文给出的条件更严格：均值有限、过程平稳等。工程上用它时要记住，它适合稳定态平均值，不是突发 p99 的万能公式。

放到 worker pool 里，可以这么映射：

```text
L = running_tasks + queued_tasks
λ = completed_tasks_per_second，也就是有效吞吐
W = queue_wait + execution_time + writeback_time
```

假设一个服务目标是：

```text
目标吞吐：500 tasks/s
平均执行时间：20ms
允许平均排队时间：30ms
写回平均时间：10ms
```

那么平均停留时间是：

```text
W = 20ms + 30ms + 10ms = 60ms = 0.060s
```

系统里平均任务数应该约为：

```text
L = 500 * 0.060 = 30
```

这 30 个任务包括正在执行、正在排队、正在写回的任务。如果平均执行时间是 20ms，维持 500/s 吞吐平均需要的 running 数是：

```text
running ~= 500 * 0.020 = 10
```

如果写回平均 10ms：

```text
writeback_inflight ~= 500 * 0.010 = 5
```

剩下的平均排队任务数约为：

```text
queue ~= 500 * 0.030 = 15
```

这个计算的价值不是给你一个精确队列长度，而是让容量配置能自洽。比如你配置了 10 个 worker、队列长度 10000，还要求 p99 100ms，这通常不自洽。队列太大时，系统允许大量任务在里面等，尾延迟自然会失控。

Little's Law 也能用来检查监控是否可信：

```text
观测吞吐 = 800/s
观测平均 total latency = 50ms
推算平均系统内任务数 = 40
```

如果你监控里的 `running + queued` 长期只有 5，要么指标口径错了，要么 latency 不是同一段系统的停留时间，要么 throughput 统计的是另一个边界。

使用时有几个坑。

第一，用有效吞吐，不用原始请求量。系统拒绝了 30% 请求时，`λ` 应该用进入并完成系统的速率。被拒绝的请求属于 admission 层，不应该混入同一个队列系统。

第二，用稳定窗口。刚启动、刚过载、刚扩容、刚恢复时，系统不在稳定态，公式只能做粗略参考。

第三，不要用平均值掩盖尾部。容量规划要同时保留 p95/p99 预算。Little's Law 告诉你平均 in-flight，不能保证尾延迟。

第四，边界要一致。只看 worker pool，就不要把客户端网络时间算进 `W`；看端到端，就要把 admission、排队、执行、写回都算进去。

第五，不要把公式当扩容理由。`L` 变大可能是吞吐增加，也可能是等待时间变长。要拆 `λ` 和 `W`。

面试里可以这样答：

```text
Little's Law 用 L = λW 把系统内平均任务数、有效吞吐和平均停留时间联系起来。容量规划时，可以用目标吞吐乘以目标平均 latency 估系统允许的 in-flight，再拆成 running、queue 和 writeback。它也能反查监控口径是否一致。限制是它描述稳定态平均值，不直接保证 p99；过载、突发、拒绝和多阶段系统里必须先定义清楚边界。
```

一句话：Little's Law 不是调参玄学，它是检查容量、延迟和队列是否自洽的尺子。

## Q019. 如何识别系统瓶颈在入队、调度、执行还是回写？

**回答：**

要识别瓶颈，先把任务生命周期拆成阶段。没有阶段化打点，只看总延迟，最后只能猜。

一条常见链路是：

```text
submit_start
  -> admitted
  -> enqueued
  -> dequeued
  -> execution_start
  -> execution_finish
  -> writeback_start
  -> writeback_done
  -> ack
```

每一段都有自己的症状。

入队瓶颈通常发生在 admission 层。任务还没进队列，就卡在提交、限流、队列锁、channel send、序列化或校验上。

```text
症状：
  admission_wait 上升。
  queue_depth 不一定高。
  submit goroutine 堵在 channel send、mutex 或 limiter。
  block profile 能看到发送阻塞或锁等待。
  返回 ErrQueueFull、context deadline exceeded 变多。
```

如果调用方阻塞在：

```go
select {
case q <- job:
    return nil
case <-ctx.Done():
    return ctx.Err()
}
```

这不是执行慢，是入队或 backpressure 已经生效。修法可能是缩短等待、快速返回、拆队列、优化 admission lock，或者把可持久化任务写到外部队列。

调度瓶颈发生在任务已经在队列里，但 worker 没能及时拿到或开始执行。

```text
症状：
  queue_depth 上升。
  oldest_task_age 上升。
  worker utilization 却不高。
  dispatcher CPU 高或锁竞争高。
  priority heap、全局队列、work stealing 逻辑消耗明显。
  runtime /sched/latencies:seconds 上升。
```

这种情况要看队列实现、worker 唤醒、条件变量、select 分支、优先级策略、全局锁、跨队列偷取成本。很多人看到队列涨就加 worker，但 worker 本来就空，加 worker 只会制造更多抢锁者。

执行瓶颈发生在 worker 已经开始处理，处理函数本身慢。

```text
症状：
  worker utilization 高。
  execution_seconds 上升。
  queue_depth 跟着上升。
  CPU profile 有明显热点，或 block/mutex profile 指向锁和 I/O 等待。
  下游 RPC、DB、磁盘、对象存储 latency 上升。
```

执行瓶颈要继续分 CPU、锁、内存、I/O。Go 的 `runtime/pprof` 可以采 CPU、heap 等 profile；`runtime.SetBlockProfileRate` 可以采阻塞；`runtime.SetMutexProfileFraction` 可以采锁竞争。`runtime/metrics` 里的 `/sync/mutex/wait/total:seconds` 能提示锁等待是否整体变差，详细栈还要靠 pprof。

回写瓶颈发生在任务执行完了，但结果提交、ack、写数据库、发消息、写响应慢。

```text
症状：
  execution_seconds 正常。
  writeback_seconds 上升。
  completed-but-unacked 数量上升。
  输出队列积压。
  数据库写入、消息 broker ack、HTTP response write 变慢。
  重试和重复提交风险上升。
```

回写慢很容易被误判成执行慢。尤其是 worker 函数把“处理”和“ack”写在同一个 `handle()` 里，指标只有一个 execution。拆开后才能知道真正卡在哪里。

可以用一个表快速定位：

```text
admission_wait 高，queue_wait 低：
  入队或限流层卡住。

queue_wait 高，worker_util 低：
  调度、队列、唤醒或锁竞争问题。

queue_wait 高，worker_util 高：
  worker 容量不足或执行太慢。

execution 高，CPU 高：
  CPU-bound 热点。

execution 高，CPU 不高，block/mutex 高：
  锁、I/O、连接池或下游等待。

writeback 高：
  结果提交、ack、持久化或响应写回慢。
```

如果系统比较复杂，建议给每个任务带 `trace_id` 和阶段事件：

```text
job_stage_event{job_id, stage, pool, worker, priority, tenant}
```

生产环境不一定全量记录每个任务事件，可以采样，但聚合指标要全量。Go 的 `runtime/trace` 适合在压测和疑难场景里抓短时间窗口，它能看到 goroutine 阻塞、系统调用、GC、P 状态变化，再配合业务阶段标签会清楚很多。

面试里可以这样答：

```text
识别瓶颈要先把任务拆成 admission、queue、schedule/start、execute、writeback、ack 几段。入队瓶颈看 admission wait 和 channel/mutex block；调度瓶颈看 queue wait、oldest age、worker idle 和调度锁；执行瓶颈看 worker utilization、execution histogram、CPU/block/mutex profile 和下游 latency；回写瓶颈看 writeback time、completed-but-unacked 和输出队列。没有阶段化指标，只看总 latency 很难定位。
```

一句话：瓶颈定位靠阶段边界，别把所有慢都塞进“worker 不够”。

## Q020. 任务超时后是否应该从队列中移除？

**回答：**

一般来说，任务如果在队列里已经超过它的 deadline，继续执行通常没有意义，应该在开始执行前丢弃或标记过期。但“是否移除”要看任务语义：请求型任务、后台任务、必须完成的持久化任务，处理方式不一样。

先看在线请求。用户请求已经超时，任务还在本地内存队列里，这时继续执行会浪费容量，还可能制造过期写入。

```text
HTTP 请求 200ms 超时。
任务在队列里等了 500ms。
worker 此时才拿到任务。
```

如果还执行它，用户已经拿不到结果。更糟的是，这个 stale task 可能写缓存、发通知、扣库存、调用下游，造成“用户看到失败，系统后来又做了”的语义混乱。在线请求类任务应该带 `context` 或 deadline，worker 取出任务后先检查：

```go
type Job struct {
    Ctx        context.Context
    EnqueuedAt time.Time
    Payload    any
}

func worker(q <-chan Job) {
    for job := range q {
        select {
        case <-job.Ctx.Done():
            observeDrop("expired_in_queue")
            continue
        default:
        }

        if err := handle(job.Ctx, job.Payload); err != nil {
            observeError(err)
        }
    }
}
```

这不是“从队列中删除任意元素”，而是惰性删除：worker 拿到后发现过期，直接跳过。对于 Go channel 这种 FIFO 队列，想从中间移除某个任务并不自然；强行做会把 channel 外面再包一层复杂结构。很多场景用惰性删除就够了。

如果使用 priority heap、延迟队列或自定义队列，可以支持主动删除或跳过过期任务：

```text
入队时记录 deadline。
队首任务过期时直接 pop 并 drop。
任务取消时只打 canceled 标记。
worker pop 到 canceled/expired 任务就跳过。
定期清理过期任务，避免队列里全是尸体。
```

主动删除要小心锁成本。每次取消都去堆里查找并删除，可能让 cancellation path 变成新瓶颈。高并发系统常用 canceled flag + lazy cleanup。

后台任务要分语义。

缓存刷新、搜索建议、预计算、缩略图生成这类任务，过期后通常可以丢。旧的刷新任务执行完反而可能覆盖新结果。

```text
cache_refresh(key=A, version=1) 已过期。
cache_refresh(key=A, version=2) 已经入队或执行。
version=1 再执行会污染结果。
```

这种任务要么按 key 合并，要么用版本号/fencing token 防止旧结果覆盖新结果。

必须完成的任务不能因为请求超时就从系统里消失。比如支付、账务、订单状态变更、消息投递。这里要区分“客户端等待超时”和“业务任务取消”：

```text
客户端请求超时：
  客户端不再等待结果。

业务任务取消：
  系统决定这件事不再做。
```

两者不是一回事。必须完成的任务应该进入 durable queue 或事务日志，客户端超时后返回“处理中”或可查询状态，而不是静默丢掉。worker 执行时要保证幂等、去重和可恢复。

任务已经开始执行时，也不能靠“从队列移除”解决。它已经不在队列里了。只能靠协作式取消：

```go
func handle(ctx context.Context, job Job) error {
    if err := step1(ctx, job); err != nil {
        return err
    }
    select {
    case <-ctx.Done():
        return ctx.Err()
    default:
    }
    return step2(ctx, job)
}
```

如果任务调用下游，要把 `ctx` 传下去，让数据库、HTTP client、RPC client 有机会停止等待。只在本地检查 `ctx.Done()`，下游调用不带超时，取消不会真正释放资源。

过期任务还要打指标：

```text
drop_total{reason="expired_in_queue"}
drop_total{reason="canceled_before_start"}
stale_execution_total
queue_wait_at_drop_seconds
oldest_task_age_seconds
```

这些指标能告诉你队列是不是已经失去意义。大量 expired-in-queue 通常说明上游还在提交系统已经来不及处理的任务，此时该做 backpressure、load shedding、缩短队列或扩容。

面试里可以这样答：

```text
请求型任务如果在队列里已经超过 deadline，通常应该在执行前丢弃或跳过，因为用户已经拿不到结果，继续执行只会浪费容量。Go channel 不适合从中间删除任务，常见做法是任务带 context/deadline，worker 取出后检查，过期就 drop；自定义队列可以做 lazy cancellation 或定期清理。必须完成的业务任务不能因为客户端超时就丢，要进入 durable queue，返回可查询状态，并靠幂等和恢复保证最终处理。已经开始执行的任务只能协作式取消，不能再从队列移除。
```

一句话：过期请求别执行，必须完成的任务别丢；先分清 timeout 是“没人等了”还是“业务取消了”。

## Q021. 任务取消后 worker 如何安全停止执行？

**回答：**

任务取消后，worker 安全停止执行的核心不是“把正在跑的线程杀掉”，而是让任务从提交、排队、执行、回写这几个阶段都能识别同一个取消信号，并且在退出时把状态讲清楚。

在 Go 里，不能也不应该从外部强行杀死某个 goroutine。goroutine 只有自己返回，才算干净退出。官方 `context` 文档把这个模型讲得很直接：取消信号通过 `Done()` channel 广播，调用方负责调用 `CancelFunc` 释放资源，被调用方负责观察这个信号并返回。worker pool 里也是一样，取消不是抢占式中断，而是协作式协议。

先把任务拆成四个阶段看：

```text
还没入队：
  admission 层直接返回 context canceled / deadline exceeded，不要再创建任务。

已经入队但还没执行：
  标记 canceled 或检查 deadline，worker 取出时跳过。

正在执行：
  执行函数、下游 RPC、数据库、外部进程都要接收同一个 ctx。

已经执行完，正在回写：
  不要因为客户端取消就随便丢掉结果，要看任务是不是已经产生了业务副作用。
```

最容易答错的是第三段。任务已经开始跑了，就不在队列里了，删除队列元素没有意义。能做的是让执行逻辑周期性检查取消信号：

```go
func handle(ctx context.Context, job Job) error {
    if err := step1(ctx, job); err != nil {
        return err
    }

    select {
    case <-ctx.Done():
        return ctx.Err()
    default:
    }

    return step2(ctx, job)
}
```

如果任务是 I/O-bound，取消要传到真正阻塞的地方。HTTP 请求要用 `http.NewRequestWithContext`，gRPC 调用要用带 deadline 的 ctx，数据库调用要用 `QueryContext`/`ExecContext`，外部进程要用 `exec.CommandContext` 或自己在取消后 kill 进程。只在 worker 外层检查 `ctx.Done()`，里面的网络调用却不带超时，这种取消只是心理安慰，资源不会释放。

如果任务是 CPU-bound，就要把大循环切成小块，在块边界检查 ctx：

```go
for i := 0; i < len(items); i++ {
    if i%1024 == 0 {
        select {
        case <-ctx.Done():
            return ctx.Err()
        default:
        }
    }
    compute(items[i])
}
```

检查太频繁会影响吞吐，检查太少会导致取消延迟过长。面试里可以说：取消点应该放在循环批次边界、阻塞调用前后、拿锁前后、写外部副作用之前。

LogServe 当前的 worker 路径里，`TaskSpec.TimeoutMs` 会在 `executeTask` 里转成 `context.WithTimeout`，再传给 Python executor、LLM executor 和 actor executor。Python executor 的 `Execute` 在 ctx 取消时会 kill Python 子进程，并在超时后调用 `Restart` 拉起新的 runner。这是一个比较硬的隔离方式：对于不听话的 Python 任务，Go 侧不指望它主动返回，而是把执行进程打掉，避免同一个 runner 被卡死。

但是这里要承认边界。杀掉子进程只能释放本地执行资源，不能自动撤销已经发生的外部副作用。任务如果已经写数据库、发消息、调用第三方接口，取消之后仍然要靠幂等 key、事务、去重表、fencing token 或补偿逻辑处理。worker “安全停止”不等于业务“安全回滚”。

更完整的设计通常会有这些规则：

```text
入队前：
  ctx 已取消就拒绝，不创建 task_id。

排队中：
  任务记录 deadline，worker 开始前检查是否过期。

执行中：
  ctx 传到所有阻塞 API；CPU 循环定期检查；外部进程有 kill/cleanup 路径。

写副作用前：
  再检查一次 ctx，但必须区分“客户端不等了”和“业务不做了”。

退出时：
  defer 释放 semaphore、锁、连接、临时文件和 WaitGroup。

状态上报：
  取消、超时、业务失败要分开计数，不要全写成 failed。
```

面试里可以这样答：

```text
worker 不能靠外部强杀 goroutine 来取消任务，正确做法是协作式取消。任务从 admission、排队、执行到回写都要携带 context/deadline：入队前发现取消就拒绝，队列里过期就跳过，执行中把 ctx 传给 HTTP、gRPC、DB、外部进程和 CPU 循环的检查点。取消后要释放本地资源并上报明确状态，但已经发生的外部副作用不能靠取消自动回滚，只能靠幂等、事务、fencing 或补偿保证业务安全。
```

一句话：取消是贯穿任务生命周期的协议，不是 worker 手里的一把 kill 开关。

## Q022. 线程池中任务 panic 或异常应该如何隔离？

**回答：**

任务 panic 或异常的隔离目标很朴素：一个坏任务不能打死整个 worker pool，不能泄漏 worker slot，不能污染后续任务，也不能让控制面以为任务还在正常执行。

在 Go 里要先记住一个边界：`recover` 只能在同一个 goroutine 的 deferred function 里捕获本 goroutine 的 panic。你在主 goroutine 外层写一个 `defer recover()`，捕不到 worker goroutine 里的 panic。所以每个 worker goroutine 的任务执行边界都要自己兜底：

```go
func runOne(ctx context.Context, job Job) (err error) {
    defer func() {
        if r := recover(); r != nil {
            err = fmt.Errorf("task panic: %v\n%s", r, debug.Stack())
        }
    }()

    return job.Handle(ctx)
}
```

这个 `defer` 一般还要和资源释放放在一起：

```go
func worker(ctx context.Context, q <-chan Job, sem *semaphore.Weighted) {
    for job := range q {
        func() {
            defer sem.Release(1)
            defer observeDone(job.ID)
            err := runOne(ctx, job)
            complete(job, err)
        }()
    }
}
```

为什么要包一层匿名函数？因为 `defer` 的作用域要跟单个任务绑定，而不是跟整个 worker 生命周期绑定。否则一个 worker 循环里连续跑很多任务，资源释放、panic 恢复和结果上报容易混在一起。

异常隔离可以分成三层。

第一层是语言级隔离。Go 任务 panic 后，worker 捕获 panic，把任务标成失败，记录 stack，不让进程退出。Python、JavaScript 这类脚本任务也要把异常转成结构化错误返回，而不是把 stderr 当成唯一结果。LogServe 的 Python executor 就是在 `handle_request` 里捕获 `Exception`，返回 `{"ok": false, "error": traceback}`，Go 侧再把它转成任务失败。

第二层是状态级隔离。任务失败后必须释放 worker capacity，写清楚 terminal event，不要让 running 计数一直挂着。LogServe 里 `CompleteTask` 会把任务写成 `SUCCEEDED` 或 `FAILED`，并在原状态是 running 时 `DecrementWorkerLoad`。这类计数释放最好放在明确的完成路径里，不能散落在业务函数里。

第三层是进程级隔离。有些异常不是普通 panic，而是内存破坏、native 扩展崩溃、解释器卡死、GPU runtime 挂死。此时 recover 没用，应该把不可信任务放进独立进程、容器、sandbox 或至少独立 runner。LogServe 对 Python 任务用了子进程 loop；如果任务超时，Go 侧会 kill 进程并重启 runner，避免一个卡死的 Python 执行器继续承接后续任务。

还要避免两个反向错误。

一个错误是把所有 panic 都吞掉。panic 被恢复后，如果只写一行日志然后继续，调用方会以为任务成功了。正确做法是把 panic 转为任务失败，并带上足够排查的信息：panic value、stack、task_id、worker_id、attempt、lease_epoch、输入摘要。敏感输入不要全量打日志。

另一个错误是盲目复用可能已经污染的 worker。Go 层纯业务 panic 通常可以继续复用 goroutine；但如果任务修改了进程级全局状态、改了当前目录、污染了环境变量、打坏了解释器状态，最好重建 runner。进程池里经常会有“任务失败后是否 recycle process”的策略，不能把它当成普通错误。

隔离后的指标也要分开：

```text
task_failed_total{reason="panic"}
task_failed_total{reason="exception"}
task_failed_total{reason="timeout"}
worker_recovered_panic_total
executor_restart_total
runner_poisoned_total
```

这些指标能帮你判断是业务代码质量问题，还是执行环境不稳定。

面试里可以这样答：

```text
线程池里的任务 panic 要在每个 worker goroutine 的单任务边界 recover，不能指望外层 goroutine 捕获。recover 后要把 panic 转成任务失败，记录 stack 和 task/worker/attempt 信息，并用 defer 保证 semaphore、锁、WaitGroup、running 计数都会释放。脚本任务要把异常结构化返回；更不可信的任务要放进独立进程或容器，超时或崩溃后重启 runner。隔离的目标不是吞掉错误，而是让一个坏任务只影响自己，不污染后续任务，也不破坏任务状态机。
```

一句话：panic 可以恢复，但必须在正确的 goroutine、正确的任务边界恢复，并且恢复后要把任务明确地变成失败。

## Q023. worker crash 后任务状态如何恢复？

**回答：**

worker crash 后能不能恢复，取决于任务状态是不是只存在 worker 本地内存里。如果任务只在本地队列、goroutine 栈或进程变量里，那么进程一死就没了。可靠的设计一定要把“任务已经被接收、租给谁、租约版本是多少、最终是否完成”放到 worker 之外。

可以按 crash 发生的位置分析：

```text
提交前 crash：
  控制面还没记录任务，调用方重试即可。

提交后、还没分配给 worker：
  任务仍在全局队列或持久化日志里，重新调度即可。

worker poll 到任务后、还没开始执行就 crash：
  控制面已经把任务租给这个 worker，需要租约超时后 redelivery。

执行中 crash：
  本次 attempt 丢失，任务租约过期后重新入队；外部副作用可能已经发生。

执行完成后、回写前 crash：
  控制面看不到完成，会重试；这要求任务结果提交和业务副作用具备幂等性。

回写完成后 crash：
  控制面已经 terminal，后续重复完成要被识别为旧消息或幂等重复。
```

LogServe 当前就是按这个思路做的。控制面 `PollTask` 不是简单把任务从队列里弹给 worker，而是调用 `LeaseTask`，把任务状态改成 `RUNNING`，写入 `worker_id`，并递增 `task_lease_epoch`。之后 worker 调用 `StartTask` 和 `CompleteTask` 都要带这个 epoch。任务如果长时间停在 running，`redeliverExpiredTasks` 会写 `TaskRedelivered` 事件，再通过 `RequeueExpiredRunningTasks` 把它改回 `QUEUED` 并清掉 worker。

这个设计解决了两个很常见的 crash 场景。

第一，worker poll 到任务后立刻死掉，甚至还没调用 `StartTask`。从控制面看，这个任务已经被 lease 给旧 worker 了，所以不会马上给别人。等 `redeliveryTimeout` 到了，下一次 poll 会触发 redelivery，任务重新进入队列。集成测试里也覆盖了 “polled task dies before start” 这条路径。

第二，旧 worker 卡住很久后又“复活”，试图提交旧结果。由于 redelivery 后新的 poll 会拿到更大的 `task_lease_epoch`，旧 worker 的 `CompleteTask` 会因为 epoch 不匹配被拒绝。这个 fencing 很重要，否则你会看到新 worker 已经成功，旧 worker 又把结果覆盖回去。

但这不等于 exactly-once 执行。更准确地说，这是 exactly-once-ish 的状态提交：控制面尽量只接受一个有效租约下的最终结果，但 worker 可能至少执行一次。执行中 crash 后重试，业务函数可能跑第二遍；旧 worker 如果在网络分区里继续调用外部系统，也可能产生副作用。要处理这个问题，任务函数本身要有幂等 key，外部写入要有唯一约束，消息投递要能去重，actor 这类有状态对象要用 owner + epoch fencing。

本地队列在恢复里只能当缓存。LogServe worker 本地有 `taskQueue`、`llmQueue`、`actorQueue`，这些队列是为了把已经 poll 到的任务分给本地 executor pool，不是 source of truth。进程崩溃后，本地队列里的任务会丢；恢复依赖的是控制面的 lease 超时和 shared log，而不是把本地队列恢复出来。

工程上还要补三类观测：

```text
redelivery_total
redelivery_age_seconds
stale_completion_rejected_total
lease_epoch_gap
task_attempt_total
worker_last_heartbeat_age_seconds
```

如果 redelivery 很多，可能是 worker crash，也可能是任务超时设置太短、执行时间太长、心跳不稳定或控制面扫描太慢。不要一看到 redelivery 就直接加 worker。

面试里可以这样答：

```text
worker crash 后，不能依赖 worker 本地内存恢复任务，任务状态必须在控制面或持久化队列里。常见做法是 poll 时给任务加租约，把 worker_id 和 lease_epoch 写入状态；worker 完成时必须带回同一个 epoch。租约超时后任务重新入队，新的 worker 拿到更高 epoch；旧 worker 后续提交结果会被 fencing 拒绝。这样能恢复 poll-before-start 和 execution crash，但不能保证业务 exactly-once，外部副作用仍然要靠幂等、去重、事务或补偿处理。
```

一句话：worker crash 恢复靠外部化的任务状态、租约超时和 fencing，不靠 worker 自己记得什么。

## Q024. 本地队列和全局队列的调度差异是什么？

**回答：**

本地队列和全局队列最大的差异是控制权不同。全局队列负责“任务应该给哪个 worker”，本地队列负责“已经给到这个 worker 的任务，应该什么时候被本地 executor 执行”。这两个队列不要混为一谈。

全局队列通常在控制面或 broker 里。它看得到所有任务、所有 worker、租约、优先级、租户、公平性、重试和持久化状态。它适合做这些决策：

```text
任务是否可以接收。
任务应该分配给哪个 worker。
某个 worker 是否还有 capacity。
某个 actor command 是否轮到执行。
LLM 请求是否应该发给有模型缓存的 worker。
running 任务是否租约过期，需要 redelivery。
```

本地队列在 worker 进程内。它更靠近执行器，成本低，延迟小，能利用本地状态，比如模型缓存、Python runner、GPU、连接池、actor lock。它适合做这些事情：

```text
把普通 task、LLM task、actor task 分到不同 executor pool。
吸收短暂的本地执行波动。
记录 local_queue_wait，判断本地 pool 是否饱和。
让 worker 可以提前 poll 少量任务，减少控制面往返。
```

LogServe 当前两层都有。控制面维护一个全局 `queue []string`，`PollTask` 扫描队列并结合 actor mailbox、LLM 调度策略、worker capacity 做分配。worker 拿到任务后，再按类型放进本地 `taskQueue`、`llmQueue`、`actorQueue`，分别由 `TaskPoolSize`、`LLMPoolSize`、`ActorPoolSize` 控制并发。`TaskStarted` 事件里的 `local_queue_wait_ms` 只描述 worker 本地等待时间，不等于任务在全局队列里的总等待时间。

两者的优缺点也不一样。

全局队列的优点是可恢复、可观测、可做全局公平。缺点是调度路径容易变重，尤其是单队列线性扫描、全局锁、复杂 predicate 混在一起时，poll p99 会随着队列深度和 worker 数增长。LogServe 的 `docs/plan.md` 里也指出，当前 `PollTask` 扫描 `queue []string`，并在扫描过程中做 spec lookup、metadata lookup、actor mailbox 判断和 LLM worker preference，队列大时会成为热点。

本地队列的优点是快，能减少控制面频繁调度，也能保留 worker-local locality。缺点是不可恢复，worker crash 后本地队列里的任务就没了；它还可能隐藏真实 backlog。控制面以为任务已经分出去，实际它在 worker 本地排了很久。如果本地队列过长，redelivery 和超时语义也会变复杂。

一个常见原则是：全局队列不要把太多任务一次性灌进某个 worker，本地队列也不要无界。worker 可以有 prefetch，但 prefetch 数量要和本地 pool capacity、任务 deadline、租约超时绑定。否则某个 worker 拿了一堆任务后变慢，其他 worker 反而没活干，全局调度失去调节能力。

面试里可以这样答：

```text
全局队列负责跨 worker 的调度和恢复，能看到所有任务、worker、租约、优先级和公平性；本地队列负责 worker 内部的执行排队，成本低，适合利用本地缓存、连接池和 executor pool。全局队列更适合做 admission、lease、redelivery 和公平调度，本地队列更适合做短期缓冲和本地并发控制。风险是本地队列不可恢复且会隐藏 backlog，所以必须有界，并暴露 local_queue_wait；全局队列如果是单锁线性扫描，也会成为控制面瓶颈。
```

一句话：全局队列决定任务归属，本地队列决定本机开工时间；前者管公平和恢复，后者管局部效率。

## Q025. 多队列调度如何避免热点队列饿死？

**回答：**

多队列调度最怕两种极端：一种是热点队列把所有 worker 吃满，冷门但重要的队列长期拿不到执行机会；另一种是为了公平把所有队列平均分，结果高吞吐队列被人为压死。好的调度要在“最低保障”和“剩余容量利用”之间做平衡。

先不要直接上严格优先级。严格优先级的规则很简单：

```text
只要 high queue 有任务，就永远不看 low queue。
```

这在告警、控制命令、短任务抢占里有价值，但如果 high queue 持续有流量，low queue 就会饿死。很多线上事故不是低优先级任务慢一点，而是清理、补偿、过期删除、异步索引这类“看起来不紧急”的任务长期不跑，最后把系统拖垮。

常用做法有几类。

第一，weighted round-robin。给每个队列一个权重，比如 A:B:C = 5:3:1，调度器按比例轮询。这样热点队列可以拿更多份额，但其他队列仍有机会。缺点是任务成本差异大时不够准，一个 10 秒任务和一个 10 毫秒任务都算一次。

第二，deficit round-robin。每轮给队列增加 quantum，任务按估算成本消耗 credit。队列 credit 不够就跳过，等下轮累积。这比简单轮询更适合任务成本不一致的情况：

```text
queue.credit += queue.quantum
while queue.credit >= next_job.cost:
    dispatch(next_job)
    queue.credit -= next_job.cost
```

第三，保底加借用。每个队列有最小份额，也有最大 burst。低优先级队列不忙时，高优先级队列可以借走容量；低优先级队列开始堆积后，调度器要把保底份额还回来。这种方式比“永远固定比例”更适合真实业务。

第四，aging。任务等得越久，有效优先级越高。它能防止长时间排队的任务永久沉底。aging 要有上限，不然所有老任务最终都变成最高优先级，系统又退回一锅粥。

第五，按资源隔离。CPU-bound、I/O-bound、LLM、actor、后台清理不要全挤进一个队列一个池。LogServe worker 现在就把普通任务、LLM、actor 分到不同本地队列和 pool；actor 还要受 per-actor lock 和控制面 `command_seq` 约束，保证同一个 actor 的方法顺序。这样的隔离能减少热点类型拖垮所有任务。

还要避免调度器自己被热点队列拖死。如果每次 poll 都从头扫描一个巨大队列，热点队列不仅占用 worker，还会占用调度 CPU。更好的结构是按类型、tenant、target worker、actor_id、model key 建索引，让调度器只看候选集合，而不是每次全表扫。

观测上要看每个队列的：

```text
queue_depth{queue}
oldest_task_age_seconds{queue}
dispatch_total{queue}
starvation_seconds{queue}
share_actual{queue}
share_target{queue}
```

只看总 queue_depth 没用。总深度下降，可能只是热点队列被快速消费；某个冷门队列的 oldest age 还在涨，说明它已经饿死。

面试里可以这样答：

```text
多队列避免饿死，不能只靠严格优先级。常见做法是 weighted round-robin、deficit round-robin、保底份额加空闲借用、任务 aging，以及按资源类型隔离 executor pool。调度器要限制每个队列的最大占用，同时给低优先级或冷门队列最低服务保障。指标上要按队列看 oldest age、实际份额、目标份额和 starvation time，而不是只看总吞吐。
```

一句话：热点队列可以多吃容量，但不能把别人的最低服务时间吃没。

## Q026. 如何设计 drain 和 graceful shutdown？

**回答：**

drain 和 graceful shutdown 不是一个 `close(channel)` 就结束了。它是一段顺序明确的协议：先停止接新任务，再处理已经接下来的任务，最后关闭资源。顺序错了，要么丢任务，要么 panic，要么 shutdown 卡死。

一个 worker 的关闭流程可以这样设计：

```text
1. 进入 draining 状态。
2. readiness 变 false，负载均衡和控制面不再把新任务发过来。
3. 停止 poll 或停止 admission，不再接收新任务。
4. 关闭本地输入队列，或者让 dispatcher 退出后由 owner 关闭队列。
5. 等待正在执行的任务完成，设置 grace deadline。
6. deadline 到了以后取消剩余任务，必要时 kill 外部进程。
7. 把已完成任务的结果提交到控制面。
8. 关闭 runner、连接、日志、临时文件和指标 exporter。
```

这里最关键的是“谁拥有 channel，谁关闭 channel”。多个 worker goroutine 不能各自 close 输入队列。通常是 dispatcher 停止投递后，由 pool 的 owner 关闭队列，worker 用 `for job := range queue` 退出。

Go 服务一般会用 `signal.NotifyContext` 接 SIGINT/SIGTERM，然后把 ctx 传给 worker pool。Kubernetes 下还要考虑 `terminationGracePeriodSeconds`：Pod 收到 SIGTERM 后有一段宽限时间，超过后会被 SIGKILL。你的 graceful shutdown 必须在这段时间里完成，不能假设无限等待。

LogServe 当前的 worker 已经有一部分这个结构。`cmd/logserve-dev` 使用 `signal.NotifyContext`；`worker.Run` 在 ctx done 时返回；`startLocalExecutorPool` 返回的 pool 在 defer 中 `Close`，`Close` 会关闭 `taskQueue`、`llmQueue`、`actorQueue` 并等待 worker goroutine。执行中的任务拿到同一个 ctx，Python runner 在 ctx 取消时会 kill 子进程。不过当前还没有一个显式的“draining worker”控制面状态，worker 停止 poll 和控制面不再分配主要依赖进程退出、心跳停止和租约超时。

设计时要把 drain 分成两类。

第一类是“排空后退出”。适合短任务、可等待任务。停止接新任务后，等待本地队列和 running 任务清空，再退出。这样重复执行少，但 shutdown 时间可能长。

第二类是“快速释放租约”。适合任务很长、平台给的 shutdown grace 很短的场景。worker 停止接新任务后，对本地还没开始的任务发 nack 或释放租约；正在执行的任务给短 grace，超时后取消，让控制面后续 redelivery。这样恢复快，但要求任务幂等。

不要在 shutdown 开始时立刻 close 所有东西。比如先 close 结果 channel，执行中的 worker 后续写结果就会 panic；先关数据库连接，任务回写会失败；先关日志客户端，terminal event 写不出去。正确顺序通常是先停入口，再等执行，再关出口。

面试里可以这样答：

```text
graceful shutdown 要按顺序做：先把 worker 标成 draining，让控制面或负载均衡不再派新任务；再停止 poll/admission；然后关闭由 dispatcher 拥有的本地输入队列，让 worker 自然退出；接着等待 running 任务在 grace deadline 内完成，超时后通过 context 取消并清理外部进程；最后提交已完成结果并关闭连接、runner、日志和指标。长任务系统还要支持释放租约或等待 redelivery，不能把退出建立在无限等待上。
```

一句话：drain 的重点是先切断入口，再收尾正在执行的工作，最后才关资源。

## Q027. 如何避免 shutdown 过程中丢任务或重复执行？

**回答：**

shutdown 过程中既想不丢任务，又想完全不重复执行，通常做不到。更现实的目标是：不丢已接收的任务；重复执行即使发生，也不会重复提交结果或重复产生不可接受的业务副作用。

先看丢任务。最危险的位置是任务已经从全局队列取走，但还没进入可恢复状态。如果控制面只是把任务 pop 出队列，然后通过网络发给 worker，中间 worker crash，这个任务就消失了。正确做法是先把任务状态改成“租给某 worker”，再返回给 worker；如果 worker 不完成，租约超时后 redelivery。LogServe 的 `PollTask` 就是先 `LeaseTask`，再把任务从全局 queue 移除并返回；因此 worker 在 poll 后 crash，任务不会永久消失。

再看重复执行。重复通常来自这些位置：

```text
worker 执行中 crash，控制面 redelivery，另一个 worker 重跑。
worker 被判定超时后其实还在跑，旧结果晚到。
shutdown 期间 CompleteTask 请求发出去了，但 worker 没收到响应，于是重试。
控制面重启，从日志恢复后再次调度未 terminal 的任务。
```

防重复结果提交靠 fencing。任务完成时必须带 worker id、attempt 或 lease epoch；控制面只接受当前租约的完成。LogServe 的 `ValidateTaskLease` 会检查 `worker_id` 和 `task_lease_epoch`，旧 worker 或旧 epoch 的完成会返回 stale task lease rejected。这样旧结果不会覆盖新结果。

防重复业务副作用要靠业务层。比如一个任务负责扣款，worker 在扣款后 crash，控制面没收到完成，于是 redelivery。第二次执行如果再次扣款，控制面 fencing 也救不了，因为副作用已经发生在外部系统。这里必须使用幂等 key、唯一约束、事务 outbox、外部接口 idempotency key 或“先记录 intent，再执行副作用，再提交完成”的协议。

shutdown 时可以按这个策略降低风险：

```text
停止 poll：
  不再拿新任务，避免扩大本地未完成集合。

本地未开始任务：
  如果已经有 lease，要么显式 nack/release，要么让租约尽快过期 redelivery。

正在执行任务：
  给 grace period；完成的正常 CompleteTask，未完成的取消或交给 redelivery。

结果回写：
  先持久化结果或 terminal event，再 ack/CompleteTask。

重复提交：
  CompleteTask 设计成幂等，terminal 任务再次完成直接返回 accepted。
```

当前 LogServe 还没有显式 nack API，所以 shutdown 中已经 leased 但没执行的任务，主要靠 redelivery timeout 回到队列。这个方式简单，但会增加恢复等待时间。生产系统可以补一个 `ReleaseTask` 或 `NackTask`，让 draining worker 把未开始的本地任务立即还给控制面。

还要注意 local queue 的 prefetch 数量。prefetch 越多，shutdown 时“已经被控制面租给某 worker、但还在本地排队”的任务越多。这些任务不会丢，但恢复要等租约过期；如果租约超时设置很长，用户看到的就是长时间卡住。所以本地队列大小、prefetch、lease timeout 和 shutdown grace 要一起设计。

面试里可以这样答：

```text
避免 shutdown 丢任务，关键是任务从全局队列取走前必须先进入可恢复的 leased/running 状态，worker 不完成就能超时 redelivery。避免重复结果，关键是 CompleteTask 带 worker id 和 lease epoch，控制面只接受当前租约，旧 worker 的完成要被 fencing 拒绝。业务副作用仍要靠幂等 key、唯一约束、事务 outbox 或外部 idempotency key。shutdown 时先停止 poll，再处理本地未开始任务和 running 任务：能完成的完成，不能完成的释放租约或等待 redelivery，不要静默丢掉。
```

一句话：任务不丢靠租约和 redelivery，结果不乱靠 fencing，副作用不重复靠业务幂等。

## Q028. 如何为 worker pool 做压测？

**回答：**

worker pool 压测不能只测“最多每秒跑多少任务”。真正要测的是容量边界：到什么负载开始排队，排队从哪里开始，拒绝是否及时，取消是否释放资源，worker crash 后能否恢复，指标能不能解释现象。

第一步先定义任务模型。至少要分三类：

```text
CPU-bound：
  纯计算，观察 GOMAXPROCS、CPU saturation、调度延迟和抢占。

I/O-bound：
  sleep、HTTP、数据库或对象存储，观察连接池、下游 latency 和 ctx 取消。

mixed/long-tail：
  大多数任务很快，少量任务很慢，观察 head-of-line blocking 和 p99。
```

如果只用 `time.Sleep`，测不到 CPU、GC、锁竞争。如果只用 CPU 循环，测不到下游阻塞。真实 worker pool 往往是混合负载，压测要把这些类型分开，再组合。

第二步设计负载方式。闭环压测是“一个请求完成后再发下一个”，它容易掩盖过载，因为系统慢了，压测工具也跟着发得慢。开环压测按固定到达率发任务，更容易看出系统在 λ 大于 μ 时如何失败。worker pool 的 backpressure、queue high watermark、drop 策略，最好用开环或阶梯式负载验证。

```text
step load：
  每 2 分钟提高一次 arrival rate，看排队从哪里开始。

spike load：
  短时间冲高 5-10 倍，看队列能否吸收 burst。

soak test：
  中高负载跑 30-120 分钟，看 goroutine、heap、FD 是否泄漏。

failure test：
  压测中 kill worker、放慢下游、制造 panic 和 timeout。
```

第三步扫参数。不要只测一个 pool size。至少扫这些维度：

```text
worker 数：1、2、4、8、16
本地队列长度：0、capacity、2*capacity、10*capacity
任务类型比例：CPU/I/O/慢任务/失败任务
timeout：短 deadline、正常 deadline、无 deadline
下游容量：连接池大小、mock latency、错误率
```

LogServe 已经有适合压测的旋钮：`--capacity`、`--task-pool-size`、`--llm-pool-size`、`--actor-pool-size` 可以分别调本地执行并发；README 里也说明了 worker local executor pool。现有 `tests/integration/worker_pool_test.go` 验证了 4 个 Python sleep 任务可以并发启动，但这只是 correctness 级别。真正压测要把脚本里的 benchmark、fault injection 和 dashboard snapshot 串起来，看吞吐、p99、queue、redelivery、backpressure。

第四步采集分阶段指标。至少要有：

```text
submit/admission latency
global queue wait
local_queue_wait_ms
execution_seconds
writeback_seconds
throughput
rejected_total
timeout_total
redelivery_total
worker utilization
goroutine count
heap/GC
CPU/block/mutex profile
```

没有这些指标，压测只会得到一个“p99 变高了”的结论，没法判断是队列拥塞、执行慢、回写慢还是调度器锁竞争。

第五步明确失败标准。压测不是为了把系统压到漂亮数字，而是为了知道什么时候必须拒绝：

```text
oldest_task_age 超过 deadline 的 50%。
queue_wait p99 超过预算。
worker utilization 已经接近 100%，queue_depth 仍持续上升。
timeout/retry 开始放大入口流量。
GC 或 goroutine 数持续上升不回落。
redelivery/stale completion 开始出现。
```

这些信号出现时，就该触发 admission control、扩容、降级或拆池，而不是继续把任务塞进队列。

面试里可以这样答：

```text
worker pool 压测要先定义任务模型，分别覆盖 CPU-bound、I/O-bound、长尾和失败任务；再用阶梯负载、突刺负载、长稳负载和故障注入测容量边界。参数上要扫 worker 数、本地队列长度、timeout、下游容量和任务类型比例。指标必须拆成 admission、global queue wait、local queue wait、execution、writeback、rejection、timeout、redelivery、CPU/heap/block/mutex profile。压测的目标不是只拿吞吐峰值，而是找出排队开始、延迟失控、应该拒绝和恢复是否正确的边界。
```

一句话：worker pool 压测要把系统压到失败，并且知道它为什么失败。

## Q029. 如何设计指标区分排队拥塞和执行耗时？

**回答：**

要区分排队拥塞和执行耗时，唯一可靠的办法是给任务生命周期打阶段时间戳。只看总 latency，最后一定会猜错。

一条任务可以拆成这样：

```text
submit_start
  -> admitted
  -> global_enqueued
  -> leased_to_worker
  -> local_enqueued
  -> execution_start
  -> execution_finish
  -> writeback_start
  -> writeback_done
```

对应的指标可以这样定义：

```text
admission_wait = admitted - submit_start
global_queue_wait = leased_to_worker - global_enqueued
local_queue_wait = execution_start - local_enqueued
execution_time = execution_finish - execution_start
writeback_time = writeback_done - writeback_start
total_latency = writeback_done - submit_start
```

LogServe 当前已经有一个很有用的点：`TaskStarted` 事件包含 `local_queue_wait_ms`，表示任务进入 worker 本地队列后，到 executor goroutine 真正开始执行之间的时间。这个指标能判断本地 pool 是否饱和。如果 `local_queue_wait_ms` 高，但执行时间正常，说明问题在 worker 本地排队；如果 local wait 很低但 execution 高，说明任务函数或下游慢。

还需要补全全局侧指标。控制面 Dashboard 现在有 `queue_depth`、`queue_high_watermark`、worker `running_tasks`、`last_log_append_ms`，这些能看 backlog 和控制面压力。但如果要更精确地区分全局调度拥塞，最好记录任务从 `TaskSubmitted` 到 `PollTask` lease 的时间，也就是 global queue wait。否则一个任务总共等了 5 秒，你只能知道其中本地等了多少，不知道全局队列等了多少。

诊断时可以看组合，而不是看单个指标：

```text
queue_depth 上升，oldest_task_age 上升，worker utilization 高：
  执行容量不够，或者执行太慢。

queue_depth 上升，worker utilization 低：
  调度器、队列锁、worker poll、匹配条件或唤醒机制有问题。

local_queue_wait 高，global_queue_wait 低：
  控制面派得出去，但某个 worker 本地 pool 饱和或 prefetch 太多。

execution_time 高，queue_wait 后续跟着升：
  执行路径慢，排队是结果。

writeback_time 高，completed-but-unacked 增长：
  结果提交、日志、DB 或 broker ack 慢。

rejected_total 高，queue_wait 不高：
  admission control 生效，系统在入口保护自己。
```

直方图比平均值重要。平均 queue wait 20ms，p99 2s，说明有长尾或者队列不公平；平均 execution 50ms，p99 5s，说明任务成本分布有问题，可能要拆池或做慢任务隔离。

标签要克制。推荐按这些维度打：

```text
pool = task / llm / actor
task_type
tenant
priority
worker_id
result = success / failed / timeout / canceled
```

不要把 `task_id` 放进 Prometheus label。task_id 适合日志和 trace，不适合指标 label，会把时序库打爆。

面试里可以这样答：

```text
区分排队拥塞和执行耗时，要把任务生命周期拆成 admission、global queue、local queue、execution、writeback 几段，并分别打 histogram。queue_depth 和 oldest_task_age 说明积压，worker utilization 说明执行资源是否忙，local_queue_wait 说明 worker 本地池是否饱和，execution_seconds 说明任务函数或下游是否慢，writeback_seconds 说明结果提交是否慢。定位时看组合：队列涨且 worker 忙，多半容量或执行慢；队列涨但 worker 闲，多半调度或队列实现有问题；执行高但队列不高，瓶颈在任务本身。
```

一句话：总耗时只能告诉你用户等了多久，阶段耗时才告诉你系统卡在哪里。

## Q030. 什么时候应该做 admission control？

**回答：**

admission control 的判断标准很直接：继续接任务只会让已经接下来的任务更慢、更容易超时，甚至拖垮进程时，就应该在入口拒绝或降级。它不是最后一招，而是保护系统稳定性的正常路径。

最典型的触发场景有几类。

第一，队列等待已经吃掉 deadline。比如请求总超时 200ms，排队 p99 已经 150ms，后面执行还要 100ms，这时继续接收只是在制造必然失败的任务。应该在入口快速返回，而不是让请求在队列里慢慢过期。

第二，到达率长期高于服务率。短 burst 可以靠队列吸收；长期 λ > μ，任何有限队列都会满，无界队列只会把失败推迟到 OOM 或全局超时。Little's Law 在这里能做 sanity check：如果目标吞吐和目标停留时间推出来的 in-flight 远小于你实际允许的队列容量，说明你在用队列掩盖过载。

第三，下游已经过载。worker pool 不是只保护自己，还要保护数据库、对象存储、LLM server、第三方 API。下游 p95/p99 升高、错误率升高、连接池等待升高时，继续放量会造成级联故障。Google SRE 的 overload 建议也强调：过载时及时拒绝、限流、降级，通常比把请求全部排队更可靠。

第四，本机资源接近硬边界。goroutine 数、heap、FD、线程数、GPU memory、连接池、磁盘队列、GC pause 已经接近预算时，要在更靠前的位置拒绝。等到 OOM killer 或进程 panic，已经太晚。

第五，需要公平性。没有 admission control 的多租户系统里，一个大租户或一个热点 key 可以把全局队列填满，其他租户跟着超时。 admission control 应该能按 tenant、priority、task type 设置 quota 或 token，而不是只有全局阈值。

LogServe 当前已经有两个 admission control 雏形。`queue_high_watermark` 会在全局队列 backlog 超过阈值时拒绝新任务；`log_append_slow_ms` 会在 shared log append 变慢时拒绝新提交，避免控制面继续制造需要写日志的任务。集成测试也覆盖了队列水位触发 backpressure、幂等重复绕过 backpressure、log append slow 触发拒绝这些路径。

admission control 应该放在昂贵工作之前。也就是认证、轻量解析、幂等检查之后，真正分配大内存、写大 payload、调用下游、排入长队之前。幂等重复请求是一个例外：如果相同 idempotency key 的任务已经存在，直接返回已有 task_id 通常比拒绝更好。LogServe 也是先查 idempotency，再检查 queue high watermark。

返回方式要明确。HTTP 系统一般返回 429 或 503，并带 retry-after 或错误码；gRPC 可以用 `RESOURCE_EXHAUSTED` 或 `UNAVAILABLE`。后台任务可以返回“稍后重试”或进入更低优先级的 durable queue。不要让调用方误以为任务已经接收成功。

面试里可以这样答：

```text
当继续接收任务会让排队时间超过 deadline、到达率长期高于服务率、下游已经过载、本机资源接近硬边界，或者热点租户会挤掉其他租户时，就应该做 admission control。它应该放在昂贵工作之前，基于队列水位、oldest age、worker utilization、下游 latency/error、连接池等待和资源预算做判断。拒绝要快速、明确、可观测；幂等重复请求可以直接返回已有结果或 task_id，而不是被队列水位误伤。
```

一句话：admission control 是在系统还有能力清醒拒绝时拒绝，而不是等系统已经失控后被动失败。

## Q031. 令牌桶和漏桶的差异是什么？

**回答：**

令牌桶和漏桶都用来限制速率，但它们的手感不一样。令牌桶更像“允许攒一小段余量，然后一次性花掉”；漏桶更像“无论入口多抖，出口都尽量匀速”。面试里不要只背概念，最好讲清楚它们对 burst、排队和延迟的影响。

令牌桶的规则是：桶里按固定速率产生 token，桶有容量上限；请求来了以后，拿到 token 就可以通过，拿不到就等待或拒绝。Go 官方 `golang.org/x/time/rate` 的 `Limiter` 就是这种模型：`NewLimiter(r, b)` 里的 `r` 是长期速率，`b` 是最大 burst。它还把行为拆成三类：`Allow` 适合超额就丢，`Reserve` 适合预约未来 token，`Wait` 适合阻塞直到 token 可用或 context 取消。

可以这样想：

```text
rate = 100/s
burst = 50

系统空闲 1 秒后，桶里最多攒 50 个 token。
下一瞬间来了 50 个请求，可以一起通过。
之后继续按 100/s 的速度放行。
```

所以令牌桶允许短 burst。它适合用户请求、API rate limit、日志采样、任务提交这类场景：平时不满载时，可以把短时间突发吃掉；长期流量还是被限制在 `r` 附近。

漏桶有两种常见说法，容易混。网络教材里常说的漏桶是“水按固定速率漏出”，入口请求先进入桶，桶满了就丢，出口尽量恒定。Go 生态里 `go.uber.org/ratelimit` 文档也把自己描述成 leaky-bucket rate limit：调用 `Take()` 前进，必要时 sleep，效果是把操作节奏压成比较稳定的速率。

可以这样想：

```text
出口速率 = 100/s
桶容量 = 50

突然来了 50 个请求。
它们不会一起出去，而是排队后按 100/s 慢慢出去。
如果入口继续暴涨，桶满后新请求被丢或被拒绝。
```

所以漏桶更强调平滑输出。它适合保护下游，尤其是下游不喜欢突发流量的场景，比如写磁盘、调用第三方 API、刷数据库、发送通知。代价是 burst 会变成排队延迟。

两者的差异可以按几个维度答：

```text
是否允许 burst：
  令牌桶允许，最多 burst 个 token。
  漏桶通常把 burst 平滑成固定出口速率。

等待在哪里发生：
  令牌桶可以立即通过、等待 token，或者直接拒绝。
  漏桶更像排队等待出口漏出。

对延迟的影响：
  令牌桶在 token 足够时延迟低，token 不足时才等。
  漏桶会把突发摊平，排队延迟更明显。

对下游的保护：
  令牌桶保护长期平均速率，但允许瞬时冲击。
  漏桶更适合保护不耐 burst 的下游。
```

在 worker pool 里，这两个东西不要替代 pool。worker pool 限制的是同时执行多少任务；令牌桶/漏桶限制的是任务进入某个阶段的速率。一个系统可以同时用：

```text
入口令牌桶：
  限制每个租户提交速率，允许小 burst。

全局队列 high watermark：
  队列太深时直接 backpressure。

worker pool：
  限制同时执行任务数。

下游漏桶：
  把写数据库或调用外部 API 的节奏压平。
```

LogServe 当前已经有 backpressure 和 worker pool，但还没有单独把 token bucket/leaky bucket 做成通用 admission 组件。后续如果要做多租户限流，可以用 token bucket 放在 submit 入口；如果要保护 shared log、对象存储或 vLLM 后端，可以考虑在下游调用前加更平滑的 limiter。

面试里可以这样答：

```text
令牌桶按固定速率补 token，桶里可以攒 token，所以允许短 burst；桶空时请求要么等待，要么拒绝。漏桶更强调固定速率输出，入口突发会被排队摊平，桶满后再丢或拒绝。令牌桶适合 API 入口限流和允许短时间突发的场景，漏桶适合保护不耐突发的下游。它们限制的是速率，worker pool 限制的是并发数，不能互相替代。
```

一句话：令牌桶管“最多能攒多少突发额度”，漏桶管“出口节奏有多平”。

## Q032. 熔断器和 backpressure 的职责边界是什么？

**回答：**

熔断器和 backpressure 都是在系统扛不住时减少压力，但它们看的信号不同，作用方向也不同。

熔断器主要保护调用方不要继续打一个已经不健康的依赖。它观察的是下游调用结果：失败率、慢调用比例、超时、拒绝数。典型状态是 CLOSED、OPEN、HALF_OPEN。Resilience4j 的官方文档就是这种模型：关闭状态正常放行；失败率或慢调用比例达到阈值后打开；打开后拒绝调用；过一段时间进入半开，放少量探测请求判断是否恢复。

backpressure 主要表达“我现在处理不过来，请上游慢一点或别再发了”。它观察的是本系统的容量信号：队列深度、oldest task age、worker utilization、内存、连接池、log append latency、下游 pending。它不一定说明下游坏了，只说明继续接活会把当前系统拖进更糟的状态。

职责边界可以这样拆：

```text
熔断器：
  判断某个依赖是否健康。
  防止继续调用明显失败或明显变慢的下游。
  通常按 dependency / endpoint / cluster 维度配置。
  失败时返回快速错误或走 fallback。

backpressure：
  判断当前系统是否还有接收能力。
  把本地拥塞传回上游。
  通常按 queue / pool / tenant / priority / resource 维度配置。
  失败时阻塞、拒绝、降级、shed load 或缩短队列。
```

Envoy 的 circuit breaking 文档把网络层的连接数、pending 请求、active 请求、active retry 等都做成上限。这个例子很适合说明：有些“熔断”实现其实也是资源上限控制。工程里名字会重叠，面试时要讲清楚自己系统里的语义。如果是按下游健康状态开闭，叫 circuit breaker 更合适；如果是按本地容量和队列水位让上游慢下来，叫 backpressure 更清楚。

一个典型组合是：

```text
下游错误率高：
  circuit breaker 打开，调用快速失败。

本地队列过深：
  backpressure 拒绝新任务或让调用方等待。

下游慢但还没大量失败：
  circuit breaker 可能因为 slow-call rate 打开；
  backpressure 也可能因为本地 worker 被占满而触发。

重试放大流量：
  circuit breaker 限制失败依赖；
  retry budget / backpressure 限制重试总量。
```

LogServe 当前有 backpressure：`queue_high_watermark` 按全局队列长度拒绝新任务，`log_append_slow_ms` 按 shared log append 延迟拒绝新提交。它还没有对 vLLM、对象存储、PostgreSQL 这类依赖实现独立熔断器。如果后续接真实 vLLM 服务，比较合理的做法是在 LLM adapter 外面加 per-model/per-endpoint circuit breaker：连续超时或 slow call 过多时短时间打开，同时控制面不要继续把大量任务调度到这个后端。

常见错误是用熔断器代替容量控制。Resilience4j 文档里也特别提醒：circuit breaker 的滑动窗口不是并发限制；如果要限制同时执行线程数，要用 bulkhead。换到 worker pool 语境，就是熔断器不能替代 pool、semaphore、队列水位和 admission control。

面试里可以这样答：

```text
熔断器看的是依赖健康度，主要根据失败率、慢调用比例和超时在 CLOSED/OPEN/HALF_OPEN 之间切换，用来避免继续打坏的下游。backpressure 看的是本系统容量，主要根据队列深度、oldest age、worker utilization、连接池和资源水位，把拥塞信号传回上游。两者可以同时存在：下游坏了用熔断快速失败，本地接不动了用 backpressure 拒绝或降速。熔断器不是并发控制，不能替代 worker pool 或 bulkhead。
```

一句话：熔断器问“下游还能不能打”，backpressure 问“我还能不能接”。

## Q033. worker pool 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

worker pool 的核心目标是有边界地执行任务。它表面上是性能工具，实际上先是容量边界和故障隔离工具。只说“提高性能”会把重点讲偏。

最直接的目标有四个：

```text
限制并发：
  同时跑多少任务必须有上限。

显式排队：
  跑不动时任务在哪里等、能等多久，要看得见。

传播压力：
  队列满、等待过久、资源不足时，要让上游知道。

统一生命周期：
  任务开始、取消、失败、panic、完成、shutdown 都有固定路径。
```

从 correctness 看，worker pool 本身不能保证业务正确。它不会自动让任务幂等，不会自动保证 exactly-once，也不会自动解决锁顺序和数据竞争。但它能提供一些正确性边界：任务不会无限制启动；取消和 shutdown 有统一入口；每个任务都有完成回调；panic 可以在任务边界被转成失败；worker slot 能用 defer 释放。这些边界能减少混乱。

从性能看，worker pool 的作用不是让所有任务更快。CPU-bound 任务的并行度超过 CPU 核数后，更多 worker 只会增加调度和缓存抖动。I/O-bound 任务可以用更多 worker 隐藏等待，但下游连接池和限流会成为新瓶颈。它真正改善的是吞吐稳定性和尾延迟可控性，而不是魔法加速。

从安全性看，worker pool 保护的是资源安全。没有 pool 时，每个请求都可能启动 goroutine、申请内存、打开文件、占连接、打下游。流量一上来，系统会以不可控方式扩张。pool 把“最多同时消耗多少资源”变成配置。对于不可信任务，还要加进程隔离、超时、权限和 sandbox；普通 worker pool 只解决一部分。

从可维护性看，pool 让任务生命周期集中。日志、指标、trace、panic recovery、重试、超时、结果写回都可以挂在同一个执行边界上。没有这个边界，系统里到处都是 `go func()`，后面排查谁还在跑、谁该退出、谁占着连接，会很痛苦。

所以如果必须选一个主目标，我会说：worker pool 主要解决的是资源安全和可控性问题，性能是它带来的结果之一，正确性和可维护性是它提供的工程边界。它不是单纯的性能优化。

放到 LogServe 里也一样。worker local executor pool 的价值不只是并发执行 Python 任务，而是把普通 task、LLM task、actor task 的本地并发分开，用 `TaskPoolSize`、`LLMPoolSize`、`ActorPoolSize` 控制资源；同时通过 `local_queue_wait_ms` 观察本地排队。控制面还用 queue high watermark 和 lease epoch 把全局容量与恢复语义接起来。

面试里可以这样答：

```text
worker pool 的核心目标是有边界地执行任务：限制同时执行数，显式化排队，提供取消、失败、panic、完成和 shutdown 的统一边界，并在过载时把压力传回上游。它不是单纯为了提高性能；CPU-bound 任务不会因为 worker 多就无限变快。更准确地说，worker pool 主要解决资源安全和可控性问题，同时改善吞吐稳定性、尾延迟和可维护性。业务正确性仍要靠幂等、事务、fencing 和状态机。
```

一句话：worker pool 的第一价值是让并发有边界，不是让 goroutine 看起来更整齐。

## Q034. worker pool 的典型适用场景和不适用场景分别是什么？

**回答：**

worker pool 适合“任务很多、每个任务成本明确、系统必须限制并发”的场景。不适合“任务需要长期占用、强实时、严格顺序、或者等待语义比执行语义更复杂”的场景。

典型适用场景有这些：

```text
批处理：
  图片缩放、日志解析、离线导入、报表生成。

I/O 扇出：
  调多个外部 API、对象存储读写、DB 查询，但要保护连接池。

后台任务：
  邮件发送、通知推送、缓存刷新、索引构建。

服务端限并发：
  每个请求里有昂贵步骤，需要限制同时运行数量。

不可信或慢任务：
  脚本执行、LLM 调用、用户自定义函数，需要超时和隔离。
```

LogServe 的普通 Python task、LLM 请求、actor method 就属于这种类型。worker 把从控制面 poll 到的任务放进本地队列，再由固定数量 executor goroutine 或 Python runner 执行。这样可以避免一个 worker 在瞬间启动无限 Python 执行。

不适用场景也要说清楚。

第一，极短小且不阻塞的任务。任务本身只做几个 CPU 指令，送进队列、唤醒 worker、切换 goroutine 的成本可能比任务更贵。直接同步执行或批量处理更合适。

第二，必须严格低延迟的实时路径。worker pool 只要有队列，就可能排队；只要排队，就有尾延迟。交易撮合、音视频实时处理、硬实时控制这类场景不能随便塞进普通 pool。

第三，强顺序任务。比如同一个 key 的状态机更新、actor mailbox、日志 append。可以用 pool 执行不同 key，但同一个 key 内部要串行化。LogServe actor 任务虽然进 actor pool，但同一个 actor 还受 per-actor lock 和控制面 `command_seq` 保护。

第四，长生命周期任务。WebSocket 连接、watch stream、长期订阅、常驻 actor loop 如果直接占一个 worker slot，会让 pool 很快被耗尽。它们更适合独立的连接管理模型，而不是普通短任务 pool。

第五，任务必须可持久恢复。内存 worker pool 不是 durable queue。进程一死，本地队列就没了。必须完成的任务要先写 WAL、消息队列、数据库或控制面的任务日志，worker pool 只能做执行层。

第六，下游才是真瓶颈。数据库只允许 20 个连接，你开 200 个 worker 只是让 180 个 worker 卡在连接池。此时应该限制下游并发、做 admission control、拆池或优化下游，而不是盲目加 pool size。

面试里可以这样答：

```text
worker pool 适合大量相似、可独立执行、成本相对明确、需要限并发的任务，比如后台批处理、I/O 扇出、脚本执行、通知发送、LLM 调用和昂贵请求步骤。不适合极短任务、硬实时路径、同一 key 必须严格顺序的状态更新、长生命周期连接，以及必须靠持久化保证不丢的任务队列。它也不能解决下游容量不足；如果瓶颈是 DB 或外部 API，加 worker 只会把等待位置换到连接池。
```

一句话：能排队、能重试、能限并发的短任务适合 worker pool；不能等、不能乱序、不能丢的核心语义要放在 pool 外面设计。

## Q035. worker pool 和相近概念最容易混淆的边界在哪里？

**回答：**

worker pool 很容易和 goroutine、队列、线程池、信号量、rate limiter、bulkhead、actor runtime 混在一起。它们都在“控制并发”附近，但控制的对象不一样。

先看 goroutine。goroutine 是 Go 的并发执行单元，worker pool 是一种使用 goroutine 的模式。写 `go f()` 只是启动并发，不代表有容量治理。worker pool 要回答：最多多少任务同时执行？排不下怎么办？取消怎么传？panic 怎么隔离？shutdown 怎么等？

再看队列。队列负责保存等待中的任务，worker pool 负责消费队列并执行任务。只有队列没有 worker，只是堆积；只有 worker 没有队列，突发时要么阻塞提交方，要么直接拒绝。很多系统把“有一个 channel”叫 worker pool，其实只是本地队列，还缺少生命周期、观测和失败处理。

信号量更轻。`semaphore` 控制同时进入某段代码的数量，不一定有常驻 worker，也不一定有队列。适合“调用前先拿许可证”的场景。固定 worker pool 更适合长期服务、任务来源持续、需要观测 queue depth 的场景。

rate limiter 控制速率，worker pool 控制并发数。一个系统每秒最多接 100 个任务，不代表同时最多跑 100 个任务；同时最多跑 10 个任务，也不代表每秒最多接 10 个任务。速率和并发的关系还要看执行时间。

bulkhead 更强调隔离。它通常把不同依赖、租户或任务类型分到不同池里，避免一个区域的故障拖垮全部。worker pool 是实现 bulkhead 的常见手段之一，但 bulkhead 是架构边界，pool 是执行机制。LogServe 把 task、LLM、actor 分成不同本地 pool，就带了一点 bulkhead 的味道。

actor runtime 不是普通 worker pool。actor 的重点是每个 actor 的状态归属、mailbox 顺序、快照和 ownership。它可以用 worker pool 执行 actor method，但同一个 actor 的消息不能被普通 pool 随机并发执行。LogServe 里 actor 有 `owner_worker_id`、epoch、`command_seq` 和 per-actor lock，这些都不是普通 worker pool 自动提供的。

熔断器也不是 worker pool。熔断器判断下游健康，决定某类调用是否快速失败；worker pool 判断本地执行资源，决定同时跑多少任务。一个下游已经熔断时，worker pool 可能仍然空闲；一个 pool 已经满时，下游可能完全健康。

面试里可以这样答：

```text
worker pool 和相近概念的边界在控制对象。goroutine 是执行单元，worker pool 是有上限地组织 goroutine；队列保存等待任务，pool 消费任务；semaphore 限制进入某段代码的并发，不一定有常驻 worker；rate limiter 限制速率，pool 限制同时执行数；bulkhead 是隔离策略，pool 可以作为实现手段；actor runtime 还要保证状态归属和 mailbox 顺序；circuit breaker 判断下游健康，不负责本地并发容量。
```

一句话：worker pool 管“同时执行多少任务”，别把所有并发治理工具都叫 pool。

## Q036. worker pool 在高并发场景下可能出现哪些隐藏问题？

**回答：**

worker pool 在低压下通常很漂亮：几个 worker、一个 channel、任务都能跑完。高并发下问题会从边界里冒出来，很多还不是业务函数本身的问题。

第一类是队列问题。队列太长会把过载藏起来，任务等到开始执行时已经过期；队列太短又可能让 burst 全部被拒。无界队列更危险，它把失败从“明确拒绝”变成“内存慢慢涨、GC 变重、最后 OOM”。队列里如果放大对象，内存放大会更明显。

第二类是 head-of-line blocking。慢任务和快任务混在同一个 FIFO 队列里，慢任务在前面会拖住后面的快任务。普通任务、LLM 冷启动、actor 调用、后台清理如果共用一个池，长尾会互相传染。

第三类是锁竞争。提交任务、取任务、更新指标、写结果、调度器扫描如果共用一把锁，高并发下 worker 越多，锁竞争越重。LogServe 当前控制面的 `PollTask` 会扫描全局 `queue []string`，并在扫描时查 metadata、actor mailbox 和 LLM preference；这个结构在小规模下清楚，但队列大时会成为调度热点。

第四类是下游放大。看到队列增长就加 worker，如果瓶颈其实在 DB、对象存储、vLLM、日志系统，更多 worker 只会把更多并发压到下游。吞吐不升，p99 和错误率反而上升。

第五类是取消不彻底。任务超时了，worker slot 释放了，但底层 HTTP 请求、Python 子进程、数据库查询还在跑。表面看 pool 没满，实际上资源被旧任务拖住。

第六类是结果通道反压。worker 执行完后要把结果写到 results channel、日志、DB 或控制面。如果回写路径慢，worker 会卡在完成阶段，执行容量也被占住。很多指标只算 execution，不算 writeback，就会误判。

第七类是调度不公平。某个 tenant、priority、key 或任务类型持续提交，把全局队列填满。没有按队列或租户的 oldest age、share、quota 指标，很难发现冷门队列已经饿死。

第八类是 goroutine 和 timer 泄漏。每个任务都开子 goroutine、timer、context，却没有在完成后 cancel 或 wait；压测一段时间后 goroutine 数和 heap 不回落。

第九类是 panic 和异常污染。某个任务 panic 后，如果没有在任务边界 recover，整个 worker 进程可能退出；如果 recover 后没有重置状态，后续任务可能在污染的 runner 里继续执行。

第十类是观测维度不够。只看吞吐和总 p99，很难知道问题在 admission、global queue、local queue、execution、writeback 还是下游。高并发问题通常要靠阶段指标、profile 和 trace 一起看。

面试里可以这样答：

```text
worker pool 高并发下的隐藏问题包括：无界或过长队列掩盖过载，慢任务造成 head-of-line blocking，全局队列锁和调度扫描成为热点，下游容量被更多 worker 放大，取消不彻底导致资源被旧任务占住，结果回写变慢反压 worker，租户或队列不公平，goroutine/timer 泄漏，panic 污染执行环境，以及指标没有拆阶段导致误判。排查时不能只看 worker 数，要看 queue age、local wait、execution、writeback、下游 latency、block/mutex profile 和 goroutine profile。
```

一句话：worker pool 能把并发收住，但也会把过载、锁竞争和长尾问题集中暴露出来。

## Q037. worker pool 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

worker pool 的普通路径很简单：取任务、执行、完成。真正难的是异常路径。崩溃、重启、超时和重试会逼你回答一个问题：任务到底处在哪个状态，谁有权继续推进它。

崩溃场景先看任务在哪：

```text
任务还在全局队列：
  worker 崩溃不影响它。

任务已经被 worker poll 走，但没开始：
  本地队列丢失，必须靠控制面租约超时重新投递。

任务正在执行：
  本次 attempt 丢失，可能已经产生部分副作用。

任务执行完但没回写：
  控制面不知道完成，后续会重试。

任务回写成功但 worker 没收到响应：
  worker 可能重试 CompleteTask，控制面要幂等。
```

重启场景会暴露本地内存边界。worker 本地队列、runner 状态、进程内缓存都不是可靠状态。LogServe worker 的本地 `taskQueue`、`llmQueue`、`actorQueue` 只是执行缓冲；worker 重启后不会恢复这些 channel。恢复依赖控制面的任务状态、shared log bootstrap、redelivery timeout 和 `task_lease_epoch` fencing。

超时场景会暴露“客户端不等了”和“业务不做了”的区别。请求超时不代表业务任务应该消失。必须完成的任务应该可查询、可重试、可幂等；可丢弃任务才适合在队列或执行前 drop。已经开始执行的任务还要看底层是否支持取消。LogServe 对 Python runner 的超时会 kill 子进程并 restart runner，这是执行层清理，不是业务回滚。

重试场景会暴露幂等性。worker pool 最多保证“某个 attempt 在某个 worker slot 里执行”，不能保证任务只执行一次。网络超时、worker crash、控制面重启都可能导致同一个任务再次执行。控制面可以拒绝旧 lease 的完成，但不能自动撤销外部副作用。

还有几个边界很容易漏：

```text
重复完成：
  terminal 任务再次 CompleteTask 应该返回幂等成功或明确拒绝。

旧 worker 复活：
  旧 lease 的结果不能覆盖新 lease 的结果。

本地队列里的任务过期：
  worker 开始前要检查 deadline 或租约是否仍有效。

重试风暴：
  下游短暂失败时，所有任务一起重试会把系统打得更坏。

shutdown 与 redelivery 交错：
  draining worker 未开始的任务要释放租约或等待超时，不应静默丢失。
```

LogServe 的集成测试已经覆盖了几个关键点：running task 租约过期后 redelivery，poll 后未 start 的任务也能 redelivery，stale completion 会被 `task_lease_epoch` 拒绝，普通 task 能从 `TaskSubmitted` 日志里在控制面重启后恢复。这些测试说明当前设计的边界是“控制面状态提交可恢复”，不是“任务执行 exactly-once”。

面试里可以这样答：

```text
崩溃、重启、超时和重试会暴露 worker pool 的状态边界：本地队列不是可靠存储，poll 后未开始的任务必须靠租约超时 redelivery；执行中 crash 可能导致任务至少执行一次；完成后回写前 crash 会导致重试；旧 worker 复活提交结果要用 lease epoch fencing 拒绝；客户端超时和业务取消要分开；重试要有幂等 key、backoff、retry budget，避免重试风暴。worker pool 负责执行边界，持久状态和业务幂等必须在 pool 外设计。
```

一句话：异常路径会证明 worker pool 只是执行器，不是任务语义本身。

## Q038. worker pool 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

worker pool 的瓶颈可能来自 CPU、内存、锁、I/O、网络，但不要一开始就猜。正确做法是先把任务生命周期拆开，再用指标和 profile 判断瓶颈在哪一段。

CPU 瓶颈的表现比较直接：

```text
CPU utilization 接近上限。
worker utilization 高。
execution_seconds 上升。
queue_depth 跟着上升。
CPU profile 里业务函数或序列化/压缩/加密是热点。
```

CPU-bound 任务里，worker 数超过 `GOMAXPROCS` 太多通常没用。更多 worker 会增加调度成本、缓存 miss 和上下文切换。此时要优化算法、批处理、减少拷贝，或者按 CPU 核数限制 pool。

内存瓶颈常见于队列和结果。任务对象太大、队列太长、每个任务都复制 payload、结果全放内存、goroutine 栈长期保留，都会让 heap 和 GC 压力上升。表现是 heap 增长、GC pause 或 GC CPU 上升、p99 抖动。队列里最好放 descriptor、ID 或轻量引用，大对象放对象存储或由调用方持有。

锁竞争通常出现在调度路径和指标路径：

```text
全局 queue lock。
metadata store 全局锁。
worker load 计数锁。
日志 writer 锁。
指标 map 锁。
actor lock。
```

如果 queue_depth 高但 worker utilization 不高，就要怀疑调度器锁、条件判断、poll 机制或唤醒路径。Go 的 block profile、mutex profile、runtime trace 很适合查这类问题。LogServe 的 `docs/plan.md` 里已经指出，`PollTask` 的全局队列扫描和 `MemoryStore` 的全局 `RWMutex` 在规模变大后会是潜在热点。

I/O 瓶颈通常来自磁盘、对象存储、数据库、日志系统。LogServe 里 shared log append latency 已经被纳入 backpressure：`log_append_slow_ms` 触发时，新任务会被拒绝。这说明写日志不是背景噪声，它可以成为控制面接收任务的瓶颈。

网络瓶颈主要来自下游 RPC、vLLM、第三方 API、跨节点调度和结果回传。它的表现不一定是本机 CPU 高，而是 execution 或 writeback 高、下游 timeout 高、连接池等待高。此时加 worker 往往会让网络和下游更糟。

还有一种瓶颈叫“排队策略瓶颈”。系统资源看起来都没满，但 p99 很差，原因是慢任务排在快任务前面，或者所有任务共享一个 FIFO。这不是 CPU、内存、I/O 任一单点，而是调度策略错了。

面试里可以这样答：

```text
worker pool 的瓶颈要按阶段判断。CPU 瓶颈表现为 CPU 和 worker utilization 高、execution_seconds 高，CPU profile 有业务热点；内存瓶颈表现为队列和 payload 放大、heap/GC 上升；锁竞争表现为 worker 不忙但 queue wait 高，block/mutex profile 指向调度锁或 metadata 锁；I/O 瓶颈在日志、DB、对象存储和磁盘；网络瓶颈在下游 RPC、vLLM 或第三方 API。加 worker 只对执行容量不足有帮助，如果瓶颈在锁、下游或队列策略，加 worker 可能更糟。
```

一句话：先看阶段指标和 profile，再决定是加 worker、拆锁、缩队列、限下游还是改调度。

## Q039. worker pool 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

这三类测试不能混着写。correctness test 测语义对不对，stress test 测并发交错下会不会坏，benchmark 测成本和容量边界。一个 worker pool 如果只有 benchmark，没有 correctness 和 stress，数字再好也不可信。

correctness test 要测确定性行为：

```text
提交任务后最终会执行。
worker 数限制真的生效。
队列满时按设计阻塞、拒绝或丢弃。
context 取消后任务不会无限卡住。
panic 被转成任务失败，不会打死整个 pool。
Close/Stop 可以重复调用或按 API 明确拒绝。
关闭队列后 worker 能退出。
任务完成后 slot、WaitGroup、semaphore 都释放。
不同任务类型进入正确的 pool。
同一 actor/key 的任务保持顺序。
```

LogServe 现有 `worker_pool_test.go` 就属于 correctness test：它提交 4 个 sleep Python task，配置 `TaskPoolSize=4`，然后检查 `TaskStarted` 时间分布，确认本地 task pool 没有把任务串行化。它验证的是“pool 并发确实打开了”，不是峰值吞吐。

stress test 要制造并发交错和故障：

```text
Submit、Dispatch、Cancel、Close 同时发生。
很多 worker 同时 poll。
任务执行中 panic、timeout、ctx cancel。
worker crash 后 redelivery。
旧 worker 完成和新 worker 完成交错。
队列满和 shutdown 同时发生。
不同 GOMAXPROCS 下重复跑。
配合 -race、-count、-cpu。
```

这类测试不追求每次都测出性能数字，而是让竞态、死锁、send on closed channel、WaitGroup 泄漏、旧结果覆盖这类问题暴露出来。命令通常像这样：

```bash
go test -race -run TestWorkerPool -count=100 ./...
go test -run TestWorkerPool -count=1000 -cpu=1,2,8 ./...
```

benchmark 要测成本和曲线：

```text
不同 worker 数下的吞吐和 p50/p95/p99。
不同队列长度下的 queue wait。
不同任务类型比例下的 head-of-line blocking。
Dispatch/Complete 的 ns/op 和 allocs/op。
队列深度 1k、10k、100k 时 PollTask p99。
pool shutdown 在 N 个 running task 下耗时。
panic/timeout 比例上升时吞吐是否崩。
```

Go 的 `testing` 包里 `Benchmark`、`RunParallel`、`ReportAllocs` 都适合做微基准；系统级 benchmark 则要跑真实控制面、worker、logd 和下游 mock。微基准告诉你某个锁或队列实现贵不贵，系统压测告诉你真实瓶颈在哪里。两者不能互相替代。

测试还要区分“行为指标”和“性能指标”。比如 correctness 里可以断言所有任务最终完成；benchmark 里不要断言 p99 一定小于某个很紧的值，否则 CI 会抖。性能回归更适合用固定机器、固定脚本和历史趋势看。

面试里可以这样答：

```text
correctness test 测语义：并发上限、队列满策略、取消、panic 隔离、shutdown、slot 释放、任务类型分池和顺序约束是否正确。stress test 测并发交错：Submit/Cancel/Close 并发、worker crash、timeout、redelivery、stale completion、队列满和 shutdown 交错，并配合 -race、-count、-cpu 重复跑。benchmark 测成本和容量曲线：不同 worker 数、队列长度、任务类型比例下的吞吐、queue wait、p99、allocs、锁竞争和 shutdown 时间。
```

一句话：correctness 问“对不对”，stress 问“乱不乱”，benchmark 问“贵不贵、扛到哪儿”。

## Q040. 如果要求从零实现一个简化版 worker pool，你会先定义哪些不变量？

**回答：**

从零实现 worker pool，不要先写 channel 和 goroutine。先定义不变量。没有不变量，代码看起来能跑，但取消、关闭、panic、重复提交一来就会散。

我会先定这些不变量。

第一，任务所有权不变量。一个任务在任意时刻只能处于一个明确位置：

```text
not_submitted
queued
running(worker_id)
completed
failed
canceled
```

不能既在队列里又在 running 集合里；不能已经 terminal 还被重新执行。状态转换要单向，terminal 状态不能回到 queued。

第二，并发上限不变量。任意时刻 running 任务数不能超过 pool size。无论任务成功、失败、panic、取消还是超时，worker slot 都必须释放。这个不变量通常靠固定 worker 数、semaphore 或 running 计数保护，并用 defer 兜底。

第三，队列有界不变量。队列容量必须有上限。超过上限时行为要明确：阻塞、返回 `ErrQueueFull`、按策略丢弃、或者进入持久化队列。不能偷偷变成无界 slice。

第四，关闭不变量。关闭后不再接受新任务；已经接受的任务要么 drain 完成，要么被取消并给出明确结果。队列只能由 owner 关闭，不能由多个 worker 关闭。`Stop` 要么幂等，要么文档明确只能调用一次；工程上我会做成幂等。

第五，取消不变量。每个任务都有 context 或 deadline。任务开始前检查一次，执行中把 ctx 传给阻塞调用，完成回写前按语义再检查。取消不能导致 worker slot 泄漏，也不能让任务永远停在 running。

第六，结果唯一不变量。每个任务最多产生一个最终结果。panic、timeout、业务错误都要转成 terminal result；不能既发送 success 又发送 failed。结果 channel 关闭要发生在所有 worker 退出之后。

第七，panic 隔离不变量。单个任务 panic 只能影响这个任务，不能打死 pool，也不能跳过 slot 释放。recover 必须在单任务执行边界，而不是只放在外层。

第八，观测不变量。每个任务至少能记录 enqueue time、start time、finish time、status。没有这些时间点，就无法区分排队拥塞和执行慢。pool 级别要暴露 queue depth、running、completed、failed、dropped、oldest age。

第九，公平性不变量。如果有多个队列或优先级，要定义最低服务保障或调度规则。否则高优先级或热点队列可能让低优先级永久饿死。

第十，外部状态不变量。如果任务必须可恢复，本地 worker pool 不能是 source of truth。要先有持久化任务记录、lease、attempt 或 epoch。简化版如果不支持 crash recovery，也要在接口文档里明确：进程崩溃会丢失 queued/running 的本地任务。

一个简化接口可以长这样：

```go
type Pool struct {
    queue   chan Job
    results chan Result
    stop    context.CancelFunc
    wg      sync.WaitGroup
    once    sync.Once
}

func (p *Pool) Submit(ctx context.Context, job Job) error
func (p *Pool) Stop(ctx context.Context) error
```

`Submit` 的不变量是：如果返回 nil，任务已经被 pool 接收；如果返回错误，pool 不负责执行它。`Stop` 的不变量是：返回后没有 worker goroutine 还在执行任务，也不会再往 results 写。

面试里可以这样答：

```text
从零实现 worker pool，我会先定义不变量：任务状态单向流转，任意时刻只能在 queued/running/terminal 之一；running 数不能超过 pool size；队列必须有界，满了行为明确；关闭后不接新任务，Stop 幂等，队列只由 owner 关闭；取消和 panic 不会泄漏 worker slot；每个任务最多产生一个最终结果；结果 channel 在所有 worker 退出后关闭；指标必须记录 enqueue/start/finish/status；多队列要有公平规则；如果需要 crash recovery，本地 pool 不能当 source of truth，必须配 lease 和持久化状态。
```

一句话：先把状态、容量、关闭、取消和结果唯一性定死，再写 worker goroutine。

## Q041. worker pool 的常见误用是什么，误用后通常会产生什么线上症状？

**回答：**

worker pool 最常见的误用，是把它当成“加速器”。很多人看到任务慢，就加 worker；看到队列涨，就加队列；看到 goroutine 多，就包一层 pool。这样改完以后，系统看起来更有结构了，但线上症状往往更隐蔽。

第一种误用是无界队列。入口永远接收，任务全塞进内存。短时间看，错误率下降了，因为请求没有被拒绝；过一会儿，p99 开始飙，heap 涨，GC 变重，最后 OOM 或整进程卡住。无界队列最坏的地方不是会失败，而是失败来得太晚。

第二种误用是 worker 数随手开大。CPU-bound 任务里，worker 数远超 `GOMAXPROCS` 只会增加调度和缓存抖动；I/O-bound 任务里，worker 太多会把数据库、对象存储、HTTP 下游或连接池打满。线上症状通常是吞吐没怎么涨，CPU、连接数、下游错误率、超时和重试一起涨。

第三种误用是把不同任务混在一个池里。快任务、慢任务、LLM 冷启动、后台清理、用户请求都走同一个 FIFO。结果是一个慢任务把后面的快任务拖住，head-of-line blocking 很严重。看监控时会发现平均耗时还行，但 p99 很难看，队列里最老任务年龄一直升。

第四种误用是提交任务时不带 context。调用方已经超时了，提交方还卡在 `q <- job`；任务进入队列后也没有 deadline，worker 过了很久才执行一件已经没人等的事。线上症状是“用户已经失败了，后台还在忙”，容量被 stale task 吃掉。

第五种误用是把本地 worker pool 当 durable queue。进程一重启，本地 channel 里的任务全没了。必须完成的任务，比如订单、支付、账务、消息投递，不能只放在内存 pool 里。线上症状是偶发丢任务，而且日志里很难还原，因为任务从来没进入持久化状态。

第六种误用是没有 panic 和异常隔离。某个任务 panic 后直接打死 worker 进程，或者 recover 后没有把任务标失败。线上症状是 worker 突然退出、running 计数不下降、任务长期卡在 running、控制面反复 redelivery。

第七种误用是结果通道没人消费。worker 执行完卡在发送 result 上，所有 worker slot 慢慢耗尽。看起来像执行慢，其实是完成路径堵了。这个问题在“执行很快、回写很慢”的系统里特别常见。

第八种误用是没有指标。只有总吞吐和总延迟，没有 queue wait、execution time、writeback time、oldest age、reject/drop 计数。线上症状是所有人都在猜：有人说 worker 不够，有人说下游慢，有人说 GC 问题，但没有阶段证据。

LogServe 当前避免了一部分误用：worker 本地队列有容量，按 task/LLM/actor 分池，`TaskStarted` 记录 `local_queue_wait_ms`，控制面有 `queue_high_watermark` 和 lease redelivery。但它也有明确边界：本地队列不是持久化队列，控制面单队列扫描在大 backlog 下会成为调度热点。

面试里可以这样答：

```text
worker pool 常见误用包括：无界队列、盲目加 worker、不同任务混池、提交不带 context、把本地 pool 当 durable queue、没有 panic 隔离、结果通道不消费、没有阶段指标。线上症状通常是 p99 飙升、oldest task age 增长、heap/GC 上升、goroutine 堆积、下游连接池耗尽、任务卡在 running、shutdown 卡死、重试风暴，以及吞吐没涨但错误率变高。
```

一句话：worker pool 用错以后，不一定马上报错，更常见的是把过载拖成慢性病。

## Q042. worker pool 在单机和分布式环境中的语义有什么差异？

**回答：**

单机 worker pool 和分布式 worker pool 看起来都在“取任务执行”，但语义差很多。单机主要处理内存里的并发和生命周期；分布式还要处理网络、租约、故障恢复、重复执行和所有权。

单机环境里，worker pool 通常由 channel、mutex、cond、semaphore 和 goroutine 组成。任务在同一个进程里排队，worker 从内存队列取任务，完成后写本地结果。它的主要问题是并发上限、取消、panic、shutdown、队列长度和指标。进程活着时，很多事情比较简单：内存可见性由锁或 channel 保证，队列状态不需要跨机器同步。

但单机 pool 的边界也很硬：进程崩溃，本地队列就没了；机器重启，running 任务也没了；没有外部状态时，任务无法恢复。单机 pool 可以很适合缓存刷新、图片处理、短后台任务，但不适合作为必须完成任务的唯一存储。

分布式环境里，任务不能只存在 worker 内存中。你需要全局任务状态、worker 注册和心跳、租约、attempt、epoch、幂等 key、redelivery、stale completion 拒绝。原因很简单：worker 可能 poll 到任务后崩溃；网络可能让控制面误以为 worker 死了；旧 worker 可能在新 worker 接手后又提交结果。

所以分布式语义通常是 at-least-once execution，加上 exactly-once-ish result commit。也就是说，任务函数可能执行多次，但控制面尽量只接受一个有效租约下的最终结果。业务副作用还要靠幂等、事务或 fencing 处理。

LogServe 就是这种分界。worker 本地有 executor pool，但全局任务状态在控制面和 shared log。`PollTask` 会 lease 任务并递增 `task_lease_epoch`；任务超时 running 后会 redelivery；旧 worker 带旧 epoch 提交结果会被拒绝。本地 pool 只负责执行，不负责分布式语义本身。

单机和分布式还有几个差异：

```text
队列顺序：
  单机 FIFO 比较容易定义。
  分布式里有多个 worker、多个 poll、重试和 redelivery，严格全局顺序很贵。

时间语义：
  单机 timeout 主要看本地 clock。
  分布式 lease timeout 会受时钟、心跳、网络延迟影响。

关闭语义：
  单机 close channel + wait group 可以收尾。
  分布式要让调度器停止分配、释放租约或等待 redelivery。

公平性：
  单机看一个队列就够。
  分布式要按 worker、tenant、region、资源类型做全局公平。
```

面试里可以这样答：

```text
单机 worker pool 主要是进程内并发控制：有界队列、固定 worker、context 取消、panic 隔离和 graceful shutdown。它的队列和 running 状态通常在内存里，进程崩溃就丢。分布式 worker pool 必须把任务状态外部化，用控制面或 broker 记录 queued/running/terminal、worker heartbeat、lease、attempt/epoch 和 redelivery；完成时还要用 worker id 和 lease epoch 拒绝旧结果。分布式里通常只能承诺 at-least-once 执行，业务 exactly-once 要靠幂等和 fencing。
```

一句话：单机 pool 管进程内并发，分布式 pool 还要管任务所有权和故障恢复。

## Q043. bounded queue 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

bounded queue 的核心目标是把等待变成有上限的等待。它不是为了让任务更快，而是为了让系统在处理不过来时用可预测的方式失败。

一个队列只要有界，就会逼你回答三个问题：

```text
最多允许多少任务等待？
排满以后谁承担压力？
等待多久以后任务还值不值得执行？
```

这三个问题比“队列用 slice 还是 channel”重要得多。

从正确性看，bounded queue 不能直接保证业务正确。任务进队列不代表最终一定执行，队列满也不代表业务失败语义已经处理好。但有界队列能防止一种很常见的错误：系统已经过载了，入口仍然返回“接收成功”。如果任务有 deadline，bounded queue 还可以配合 oldest age 和过期丢弃，避免执行 stale work。

从性能看，bounded queue 本身不是性能优化。队列太小会让短 burst 被拒；队列太大会增加尾延迟。它真正提供的是延迟和内存的边界。性能收益来自更早地拒绝无望任务，避免 heap、GC、goroutine 和下游被无限放大。

从安全性看，bounded queue 很重要。安全性这里不是认证授权，而是资源安全：内存不会无限涨，排队对象不会无限保留，提交方不会把服务拖到 OOM。对高并发服务来说，有界队列和 worker pool 一样，是容量治理的基本防线。

从可维护性看，有界队列让系统更容易解释。queue depth、remaining capacity、oldest task age、reject count 都能直接暴露。没有上限时，队列长度只是一个不断变大的数字，很难定义什么时候系统应该拒绝。

所以如果要归类，我会说 bounded queue 主要解决资源安全和可控失败问题，同时帮助正确性和可观测性。它不是主要为了性能，但它能避免过载把性能拖到不可恢复。

面试里可以这样答：

```text
bounded queue 的核心目标是让等待有上限：最多排多少、排满怎么办、排太久还执不执行，都要明确。它主要解决资源安全和可控失败问题，防止无界排队把内存、GC、goroutine 和下游拖垮。它本身不保证业务正确，也不是单纯性能优化；性能收益来自及时 backpressure、拒绝过期工作和暴露 queue depth/oldest age/reject count。
```

一句话：bounded queue 的价值不是多装任务，而是告诉系统什么时候别再装了。

## Q044. bounded queue 的典型适用场景和不适用场景分别是什么？

**回答：**

bounded queue 适合吸收短时间波动，不适合掩盖长期容量不足。这个边界一定要讲清楚。

适用场景很常见：

```text
短 burst：
  平时 100 QPS，偶尔 1 秒冲到 300 QPS，后面能消化回来。

生产者和消费者速率有轻微抖动：
  生产端偶尔快一点，消费端整体跟得上。

本地 worker pool 前缓冲：
  任务先排队，再由有限 worker 执行。

异步日志或指标批处理：
  短时间积压可以接受，但不能无限占内存。

下游写入前的削峰：
  让短抖动不直接打到 DB、磁盘或外部服务。
```

bounded queue 不适合这些场景：

第一，长期入流大于出流。生产者长期 2000/s，消费者只能 1000/s，任何有限队列都会满。队列满只是时间问题。该做的是扩容、降级、拒绝、限流或削减上游，而不是继续加队列长度。

第二，强 deadline 的在线请求。如果用户 200ms 内要结果，队列允许任务排 5 秒没有意义。队列越大，过期工作越多。

第三，必须完成且不能丢的任务。内存 bounded queue 不是持久化队列。支付、订单、账务、消息投递这类任务要先进入 durable log、broker 或数据库，再由 worker 消费。

第四，任务对象很大。队列里如果放大 payload、图片、压缩包、长 JSON，容量上限即使是 100，也可能吃掉大量内存。此时队列里应放 descriptor 或对象存储引用。

第五，严格优先级或公平调度。单个 bounded FIFO 不知道租户、优先级、deadline，也不知道慢任务和快任务差异。需要多队列、priority queue、deadline queue 或 scheduler。

第六，背压必须跨网络传播的场景。单机 bounded queue 满了，只能影响本进程里的 submitter；分布式系统要把拒绝或 slow-down 信号传给上游服务、网关或客户端。

LogServe 里，worker 本地队列适合做本地 executor pool 的短缓冲；控制面 `queue_high_watermark` 适合防止全局 backlog 无限制增长。但如果要承载长时间离线任务，仍然要依赖 shared log 和 metadata，而不是靠本地 channel。

面试里可以这样答：

```text
bounded queue 适合吸收短 burst、解耦轻微速率抖动、放在 worker pool 前做本地缓冲、给日志/指标/下游写入做削峰。它不适合长期入流大于出流、强 deadline 在线请求、必须持久完成的任务、大对象排队、复杂公平调度，或者需要跨服务传播背压但只在本地排队的场景。队列是短期缓冲，不是容量缺口的补丁。
```

一句话：bounded queue 能买一点时间，买不了长期吞吐。

## Q045. bounded queue 和相近概念最容易混淆的边界在哪里？

**回答：**

bounded queue 容易和 channel、worker pool、semaphore、rate limiter、broker、backpressure 混在一起。它们都可能出现在同一条路径上，但职责不一样。

Go channel 可以用来实现 bounded queue。`make(chan T, n)` 有容量上限，send 在满时阻塞，receive 在空时阻塞。但 channel 只是机制，不等于完整队列策略。它不告诉你排队多久、队列满了返回什么错误、怎么按优先级调度、怎么删除过期任务、怎么持久化。

worker pool 是消费者集合，bounded queue 是等待区。一个 pool 可以没有显式队列，靠 semaphore 限并发；一个队列也可以没有固定 worker，只被某个 dispatcher 消费。把两者混在一起，会导致问题定位不清：到底是排队拥塞，还是执行慢？

semaphore 限制并发，不保存任务。拿不到 semaphore 时，调用方可以等待、超时或失败；bounded queue 则保存已接收但尚未执行的任务。semaphore 更适合保护某段临界资源，queue 更适合异步执行。

rate limiter 限制进入速率，bounded queue 限制等待数量。入口限速是每秒多少，队列容量是多少个。两者不能互相替代：速率低但任务很慢，队列仍会涨；队列有界但入口没有限速，满了以后会频繁拒绝。

broker 或 durable queue 是持久化消息系统，bounded queue 通常是进程内内存结构。Kafka、NATS JetStream、RabbitMQ 这类系统可以提供持久化、ack、redelivery、consumer group；内存 bounded queue 通常做不到。不要因为它也“有容量”就把它当可靠队列。

backpressure 是反馈策略，bounded queue 是产生反馈信号的一个位置。队列满、oldest age 过高、remaining capacity 低，都可以触发 backpressure。但 backpressure 还包括下游慢、连接池满、日志写慢、CPU/内存高等信号。

面试里可以这样答：

```text
bounded queue 是有容量上限的等待区，不等于完整 worker pool。channel 可以实现它，但不自动提供过期、优先级、持久化和错误语义；semaphore 限并发但不保存任务；rate limiter 限速率但不限制等待数量；durable broker 提供持久化和 ack/redelivery，本地 bounded queue 通常不提供；backpressure 是用队列水位等信号反馈给上游的策略。边界清楚后，排队、执行、限速和恢复才能分开设计。
```

一句话：bounded queue 只回答“能等几个”，不回答“谁来跑、跑几次、坏了怎么恢复”。

## Q046. bounded queue 在高并发场景下可能出现哪些隐藏问题？

**回答：**

bounded queue 有上限，但这不代表它在高并发下就安全。它只是把问题从“无限增长”变成“满了以后怎么办”。真正的坑在满之前和满之后。

第一个问题是满队列阻塞提交方。如果 submit 没有 context 或 timeout，大量 goroutine 会卡在 send 或锁上。外面看起来是请求 handler 堆积、goroutine 数上涨、线程栈和内存上涨。队列有界了，但压力没有消失，只是转移到提交方。

第二个问题是惊群和锁竞争。很多生产者同时抢一个队列锁或同时等待一个 cond，消费者一释放空间，提交方一起醒来抢锁。吞吐不一定高，CPU 却很忙。Go channel 内部也要同步，应用层自定义队列也会遇到同样问题。

第三个问题是 head-of-line blocking。bounded FIFO 只按入队顺序出队，不看任务成本、deadline、tenant、priority。慢任务在前面，后面快任务就等着；低价值任务在前面，高价值任务也等着。

第四个问题是过期任务占坑。队列有界时，已经过期的任务如果还留在队列里，会占掉宝贵容量，导致新鲜任务被拒。很多系统应该在入队前、出队前或队首检查 deadline。

第五个问题是错误的容量单位。队列容量按“任务个数”限制，但每个任务占用内存可能差很多。100 个小 descriptor 很安全，100 个大 payload 可能直接把 heap 打爆。

第六个问题是拒绝风暴。队列一满，所有提交都快速失败；上游如果立刻重试，失败流量会被放大。此时需要 retry-after、指数退避、jitter、retry budget，不然 bounded queue 会变成重试风暴的触发器。

第七个问题是指标误导。queue depth 接近容量不一定坏，如果任务很快消费；queue depth 很低也不一定好，如果 oldest age 很高，说明队列头部被慢任务堵住。要同时看 depth、oldest age、enqueue wait、drop/reject、submit block time。

第八个问题是公平性缺失。一个热点 tenant 把 bounded queue 填满，其他 tenant 直接被拒。全局有界不等于按租户有界。多租户场景要做 per-tenant queue、quota 或 weighted scheduling。

面试里可以这样答：

```text
bounded queue 高并发下的隐藏问题包括：队列满后提交方大量阻塞，队列锁或 cond 竞争，FIFO 导致 head-of-line blocking，过期任务占容量，大任务让“按个数限容量”失真，队列满触发上游重试风暴，queue depth 指标误导，以及热点租户填满全局队列。要配合 context submit、oldest age、deadline 清理、拒绝退避、按租户限额和阶段指标一起设计。
```

一句话：bounded queue 防的是无限增长，不自动防阻塞、长尾、公平性和重试放大。

## Q047. bounded queue 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

bounded queue 一遇到崩溃和重启，边界马上暴露：它到底是内存缓冲，还是可靠任务存储？如果只是内存队列，进程没了，队列里的任务也没了。

崩溃时要分位置：

```text
入队前崩溃：
  队列没接收，调用方重试即可。

入队后、执行前崩溃：
  内存队列里的任务丢失，除非外部已经持久化。

出队后、执行前崩溃：
  任务既不在队列里，也没开始执行，最容易丢。

执行中崩溃：
  队列不知道任务执行到哪一步。

执行后、ack 前崩溃：
  可能导致重试和重复执行。
```

所以可靠系统里，bounded queue 通常不能单独存在。要么它只是 durable queue 前后的短缓冲，要么任务先写 WAL/broker/metadata，再进入内存队列。LogServe 的本地 worker 队列就是短缓冲；任务可靠性来自控制面状态和 shared log，不来自本地 channel。

超时会暴露另一个问题：任务在队列里等太久以后还要不要执行？如果 bounded queue 只限制数量，不检查 deadline，它会执行很多 stale work。在线请求类任务应该在入队时记录 deadline，出队前检查，过期就 drop 或标记 canceled。必须完成的后台任务不能因为客户端超时就丢，要转成可查询状态。

重试会暴露幂等性。队列满后返回错误，上游可能重试；worker crash 后任务可能重新入队；ack 丢失后可能重复投递。bounded queue 只负责容量，不负责去重。要靠 idempotency key、attempt、lease、dedup table 或 broker ack 协议处理。

还有一个细节：重启恢复后不要盲目把所有任务重新塞回队列。如果任务已经 terminal，不能回到 queued；如果任务已经过期，可能应该丢；如果任务属于某个 key 的有序流，要恢复顺序和水位。恢复逻辑比入队逻辑更容易写错。

面试里可以这样答：

```text
bounded queue 在崩溃和重启时会暴露它是不是 reliable queue。内存队列在入队后执行前崩溃会丢任务，出队后执行前崩溃尤其危险；如果任务必须恢复，必须先写 WAL、broker 或控制面状态，再进入内存队列。超时场景要检查队列等待是否超过 deadline，避免 stale work。重试场景要靠幂等 key、attempt/lease、ack 或 dedup 处理重复，bounded queue 本身只限制容量，不保证不丢不重。
```

一句话：bounded queue 是容量边界，不是恢复协议。

## Q048. bounded queue 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

bounded queue 的瓶颈最常见来自锁竞争和内存，其次才是 CPU。I/O 和网络通常不是队列本身的直接瓶颈，但会通过消费者变慢把队列顶满。

CPU 瓶颈主要出现在复杂调度上。简单 ring buffer 或 Go channel 的入队出队成本不高；如果每次出队都要扫描优先级、检查 deadline、过滤 tenant、查 metadata，CPU 成本就会上来。LogServe 当前控制面的全局 queue 扫描就是这种风险：队列越深，每次 poll 的判断越多。

内存瓶颈来自两个地方：队列元素和等待者。队列里放大 payload，会直接放大 heap；大量 goroutine 阻塞在 submit 上，也会占栈和引用对象。即使队列本身有界，等待在队列外的 goroutine 也可能无界。

锁竞争是高并发队列最常见的问题。多生产者、多消费者都抢同一把锁，或者每次操作都更新同一个指标 map，都会把队列变成同步热点。Go channel 已经做了很多优化，但它也不是无成本；自定义队列如果用一个全局 mutex，竞争会更明显。

I/O 瓶颈一般来自消费者。消费者写磁盘、写日志、写 DB 变慢，队列就会满。此时优化队列实现没有用，应该看下游 I/O、批处理、group commit、连接池和 backpressure。LogServe 的 `log_append_slow_ms` 就是把 shared log I/O 慢作为 admission 信号。

网络瓶颈也类似。下游 RPC 慢、broker ack 慢、跨节点调度慢，都会让消费者出队后迟迟不释放容量。队列看到的只是 depth 和 age 上升，真正瓶颈在网络或远端服务。

排查时可以按这个顺序：

```text
queue operation CPU 高：
  看调度扫描、序列化、优先级结构。

mutex/block profile 高：
  看队列锁、notEmpty/notFull 条件、指标锁。

heap/GC 高：
  看队列元素大小、阻塞 goroutine、payload 拷贝。

queue depth 高但 consumer busy：
  看执行路径、I/O 和下游。

queue depth 高但 consumer idle：
  看唤醒、调度条件、锁竞争或队列 bug。
```

面试里可以这样答：

```text
bounded queue 自身的性能瓶颈通常先看锁竞争和内存。多生产者多消费者抢同一队列锁、条件变量惊群、指标锁竞争，会让入队出队变慢；队列元素太大、阻塞提交方太多，会把 heap 和 GC 打高。CPU 瓶颈常来自复杂调度扫描，而不是简单入队出队。I/O 和网络通常在消费者或下游，表现为队列 depth/oldest age 上升。优化前要用 block/mutex profile、heap profile、queue wait 和 consumer utilization 分清楚。
```

一句话：队列慢不一定是队列算法慢，很多时候是锁、内存或消费者下游慢。

## Q049. bounded queue 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

bounded queue 的测试要分三层。correctness test 测队列语义，stress test 测并发交错，benchmark 测容量和成本。

correctness test 先测基本不变量：

```text
容量不超过 cap。
空队列 pop 会阻塞、返回 false 或按 API 定义失败。
满队列 push 会阻塞、返回 ErrFull 或按策略丢弃。
FIFO 顺序正确。
close 后不能再 push。
close 后已入队元素可以 drain，或者按 API 明确定义丢弃。
context canceled 后阻塞 push/pop 能返回。
len/cap/remaining 指标正确。
```

如果队列支持 deadline、priority、drop-oldest、drop-newest、tenant quota，还要逐个测。不要只测 happy path。

stress test 要测并发下是否破坏不变量：

```text
多 producer + 多 consumer。
push/pop/close/cancel 同时发生。
满队列和空队列反复切换。
关闭时仍有 goroutine 阻塞在 push/pop。
context timeout 和 close 竞争。
高频 len/metrics 读取和 push/pop 并发。
不同 GOMAXPROCS 下重复跑。
配合 -race、-count、-cpu。
```

这类测试重点不是吞吐数字，而是找 data race、lost wakeup、double close、send on closed channel、goroutine leak、负数长度、超过容量这类错误。

benchmark 要测不同负载形态：

```text
单 producer 单 consumer。
多 producer 单 consumer。
单 producer 多 consumer。
多 producer 多 consumer。
队列容量 0、1、64、1024、65536。
元素大小小对象和大 descriptor。
满队列下的失败成本。
阻塞等待和非阻塞 try-push 的成本。
```

微基准要看 `ns/op`、`allocs/op`、吞吐；系统基准要看 queue wait、oldest age、reject rate、consumer utilization 和 p99。两者都需要。一个 lock-free 队列微基准很快，不代表系统里就更好，因为真实瓶颈可能在消费者和下游。

还要测泄漏。bounded queue 很容易在取消和关闭路径漏 goroutine：

```text
启动 N 个阻塞 push。
cancel 或 close。
等待所有 goroutine 退出。
检查 goroutine 数是否回落。
```

面试里可以这样答：

```text
correctness test 测容量、FIFO、满/空行为、close 语义、drain、context cancel、len/cap 指标和 drop 策略。stress test 测多 producer/consumer 下 push/pop/close/cancel 交错，配合 -race、-count、-cpu 找 data race、lost wakeup、goroutine leak 和超过容量。benchmark 测不同 producer/consumer 数、容量、元素大小、满队列失败成本、阻塞/非阻塞路径的 ns/op、allocs/op、吞吐和 p99。
```

一句话：队列测试不能只看能不能 push/pop，还要看满、空、关、取消和并发抢的时候会不会乱。

## Q050. 如果要求从零实现一个简化版 bounded queue，你会先定义哪些不变量？

**回答：**

从零实现 bounded queue，我会先定义不变量，再决定用 channel、mutex+cond、ring buffer 还是别的数据结构。

第一，容量不变量：

```text
0 <= len <= cap
```

任何并发交错下都不能突破这个范围。`cap` 一旦定义，不能因为扩容 slice 偷偷变成无界。

第二，元素所有权不变量。一个元素要么还没入队，要么在队列里，要么已经被某个 consumer 取走。不能被取两次，不能入队成功后悄悄消失，除非 API 明确是 drop 策略。

第三，顺序不变量。如果是 FIFO，同一优先级内必须按入队顺序出队。如果支持 priority 或 deadline，要定义清楚同优先级如何排序。

第四，满队列语义不变量。队列满时，`Push` 到底阻塞、返回 `ErrFull`、等待 context、drop newest、drop oldest，必须固定。不能有时阻塞、有时静默丢。

第五，空队列语义不变量。队列空时，`Pop` 是阻塞、返回 `ErrEmpty`，还是等待 context，也要固定。

第六，关闭不变量。关闭后是否允许继续 Pop 已有元素？关闭后 Push 返回什么？重复 Close 是幂等还是报错？所有阻塞的 Push/Pop 是否都会被唤醒？这些必须在接口里写清楚。

第七，取消不变量。阻塞 Push/Pop 接收 context 时，ctx 取消后必须返回，并且不能改变队列状态。比如 Push 等空间时 ctx 取消，不能把元素半塞进去。

第八，指标不变量。`Len()`、`Cap()`、`Remaining()` 必须在同步边界内读取，不能返回负数或超过 cap。指标可以是近似值，但要明确。

第九，唤醒不变量。每次 Push 成功后，至少一个等待 Pop 的 goroutine 有机会醒；每次 Pop 成功后，至少一个等待 Push 的 goroutine 有机会醒。用 `sync.Cond` 时尤其要防 lost wakeup，并且等待条件必须放在循环里检查。

第十，内存不变量。元素出队后，ring slot 要清零，避免大对象被队列底层数组长期引用，导致 GC 无法回收。

简化接口可以是：

```go
type Queue[T any] struct {
    mu       sync.Mutex
    notFull  *sync.Cond
    notEmpty *sync.Cond
    buf      []T
    head     int
    size     int
    closed   bool
}

func (q *Queue[T]) Push(ctx context.Context, v T) error
func (q *Queue[T]) Pop(ctx context.Context) (T, bool, error)
func (q *Queue[T]) Close()
```

如果是教学版，也可以直接用 buffered channel。但一旦需要 `Len` 准确语义、drop-oldest、priority、deadline 删除、关闭后 drain 等行为，自定义结构会更清楚。

面试里可以这样答：

```text
我会先定义 bounded queue 的不变量：len 永远在 0 到 cap 之间；元素不能重复出队，成功入队后不能无故消失；FIFO 或 priority 顺序要固定；满队列和空队列行为明确；close 后 Push/Pop 和重复 Close 语义明确；阻塞 Push/Pop 在 context 取消后必须返回且不改变状态；每次 Push/Pop 要正确唤醒等待者；Len/Cap/Remaining 在同步边界内读取；出队后清空 slot，避免大对象被引用。
```

一句话：bounded queue 的实现难点不是数组，而是满、空、关、取消和唤醒的不变量。

## Q051. bounded queue 的常见误用是什么，误用后通常会产生什么线上症状？

**回答：**

bounded queue 常见误用的根源，是以为“有上限”就等于“安全”。实际上，上限只是开始。满了以后怎么处理，才决定线上表现。

第一种误用是容量拍脑袋。随便设 10000，以为越大越安全。结果是排队延迟变长，任务开始时已经过期，内存也被大量 payload 占住。线上症状是 queue depth 经常不满，但 oldest age 很高，用户超时后后台还在执行旧任务。

第二种误用是满了就无期限阻塞。提交方没有 context，handler 卡在入队上。队列没有爆，但 goroutine 爆了。监控里会看到请求耗时增加、goroutine profile 大量卡在 channel send 或 cond wait。

第三种误用是满了静默丢。调用方以为任务接收成功，系统却偷偷 drop。线上症状是偶发丢数据、用户查不到任务、任务状态没有 terminal 记录。这比明确返回错误更难排查。

第四种误用是把 bounded queue 当限流器。队列限制的是等待数量，不是进入速率。上游流量很高时，队列会快速满，然后大量请求被拒或阻塞。正确做法通常还要配 token bucket、admission control 或 per-tenant quota。

第五种误用是队列里放大对象。容量是 1000，但每个任务带几 MB payload。结果是内存、GC、拷贝成本都高。线上症状是 heap 抖动、GC CPU 上升、p99 随队列深度恶化。

第六种误用是没有过期清理。任务带 deadline，但队列不检查。过期任务占住容量，新任务被拒，worker 还在执行无价值任务。线上会看到 expired-in-queue、timeout、reject 一起升。

第七种误用是全局一个队列服务所有租户。热点租户填满队列，其他租户被拒。症状是总 queue depth 解释不了投诉，因为问题只发生在某些 tenant 或 priority。

第八种误用是没有关闭协议。shutdown 时直接 close channel，仍有 producer send，panic；或者 consumer 等不到 close，泄漏。线上表现是发布/重启时偶发 panic、任务卡住、进程退出慢。

面试里可以这样答：

```text
bounded queue 常见误用包括：容量拍脑袋、满了无期限阻塞、满了静默丢、把队列当限流器、队列里放大 payload、不清理过期任务、所有租户共用一个全局队列、没有 close/drain 协议。线上症状通常是 p99 和 oldest age 升高、goroutine 卡在 submit、heap/GC 上升、偶发丢任务、热点租户拖垮全局、过期任务占容量、发布时 send on closed channel 或 shutdown 卡死。
```

一句话：bounded queue 用错后，症状不是“队列无界增长”，而是阻塞、过期、误丢和不公平。

## Q052. bounded queue 在单机和分布式环境中的语义有什么差异？

**回答：**

单机 bounded queue 的语义主要由内存和同步原语决定；分布式 bounded queue 的语义还要处理持久化、ack、redelivery、分区和全局容量。名字一样，含义差很多。

单机里，bounded queue 通常就是 `chan T` 或 mutex 保护的 ring buffer。容量是本进程的内存容量；满了以后阻塞或返回错误；close 由本进程控制；顺序也比较容易理解。它能很好地做本地 worker pool 缓冲。

单机语义的弱点是崩溃不可恢复。进程退出，队列内容没了。即使用了 buffered channel，Go 语言也没有把 channel 内容持久化的语义。单机队列适合作为执行缓冲，不适合作为可靠任务系统的唯一状态。

分布式 bounded queue 的容量要分层：

```text
broker 中的 backlog 上限。
每个 partition 的 backlog 上限。
每个 consumer 的 prefetch 上限。
每个 worker 的本地队列上限。
每个 tenant 的 quota。
```

这些上限不一致时，系统会出现奇怪现象：broker 还有容量，但某个 worker 本地队列满；全局队列不深，但某个 partition 热点；总 backlog 可控，但某个租户被挤死。

分布式队列还要定义 ack 语义。任务什么时候算从队列移除？发送给 worker 时？worker start 时？worker complete 时？如果 worker 收到后崩溃，是否 redelivery？如果 ack 丢了，是否重复？这些都是本地 bounded queue 没有的语义。

顺序也不一样。单机 FIFO 可以比较严格；分布式里有 partition、consumer group、重试、dead letter、redelivery，严格全局 FIFO 很难。多数系统只能保证某个 key 或 partition 内的顺序。

LogServe 的设计可以作为例子：控制面全局队列是调度入口，本地 worker 队列是执行缓冲；任务可靠性不靠本地 bounded queue，而靠 `TaskSubmitted` 日志、metadata 状态、lease epoch 和 redelivery。这样单机本地队列和分布式任务语义被分开了。

面试里可以这样答：

```text
单机 bounded queue 是进程内容量边界，通常用 channel 或 ring buffer 实现，满了阻塞或拒绝，进程崩溃后内容丢失。分布式 bounded queue 要定义 broker backlog、partition、consumer prefetch、worker local queue 和 tenant quota，还要有 ack、lease、redelivery、幂等和顺序语义。单机队列适合执行缓冲，分布式可靠队列必须把任务状态外部化，不能靠 worker 本地内存。
```

一句话：单机 bounded queue 管本地内存，分布式 bounded queue 还要管任务是否可靠地被某个消费者接手。

## Q053. backpressure 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

backpressure 的核心目标是让过载信号逆着调用链传回去。系统处理不过来时，不要在内部偷偷堆积，而要让上游减速、排队、降级或停止发送。

它解决的第一个问题是资源安全。没有 backpressure，上游继续把请求打进来，下游只能在内存、队列、连接池、goroutine、重试里堆积。最后失败点常常不是业务错误，而是 OOM、FD 耗尽、线程耗尽、GC 抖动或数据库被打挂。

第二个问题是延迟可控。无界排队会把失败变成慢失败。用户看起来一直在等，系统也一直在做无价值工作。backpressure 让系统更早地拒绝或减速，尾延迟反而更容易控制。

第三个问题是故障隔离。一个慢下游、一个热点租户、一个过载 worker，不应该把整个系统拖死。backpressure 可以按队列、租户、优先级、依赖、资源类型局部触发。

从正确性看，backpressure 不直接保证业务正确。拒绝请求以后，调用方怎么重试、是否幂等、是否丢任务，这些还要业务协议定义。但没有 backpressure，系统可能在已经无法完成任务时仍然承诺“已接收”，这会制造正确性风险。

从性能看，backpressure 不是让系统峰值吞吐更高。它常常会降低短期接收量。它的价值是让系统停在可恢复区间，避免吞吐崩塌。

从可维护性看，backpressure 让容量边界显式化。你能看到 reject、drop、queue high watermark、retry-after、slowdown、load shedding，而不是只看到一堆超时和 OOM。

面试里可以这样答：

```text
backpressure 的核心目标是把过载信号传回上游，避免系统在内部无界排队。它主要解决资源安全和可控失败问题，同时帮助尾延迟和故障隔离。它不是单纯性能优化，也不直接保证业务正确；业务层仍要处理拒绝、重试、幂等和状态查询。好的 backpressure 会让系统在还能自我保护时减速或拒绝，而不是等到 OOM、连接池耗尽或下游崩溃。
```

一句话：backpressure 不是让系统多接活，而是让系统知道什么时候别接活。

## Q054. backpressure 的典型适用场景和不适用场景分别是什么？

**回答：**

backpressure 适合处理“下游或本地容量有限，但上游可能继续发送”的场景。只要生产速度可能超过消费速度，就要考虑它。

典型适用场景包括：

```text
HTTP/gRPC 服务入口：
  worker、DB、下游都接近上限时，快速返回 429/503 或 RESOURCE_EXHAUSTED。

本地队列：
  queue depth 或 oldest age 超过阈值时，停止接收或缩短等待。

流式处理：
  下游 subscriber 消费慢时，上游不要无限 buffer。

日志和指标：
  writer 慢时采样、丢弃低价值日志或阻塞低优先级路径。

数据库和外部 API：
  连接池等待、错误率或 p99 升高时，减少调用量。

LLM/GPU 服务：
  显存、batch queue、prefill/decode 队列过载时限制新请求。
```

Reactive Streams 的一手规范就把 back pressure 放在核心位置：异步边界之间不能强迫接收方无限 buffer。这个观点放到 worker pool 里也成立：队列只是边界，不能无限承接。

不适用场景也要说。

第一，必须立即处理的安全关键事件。比如某些控制系统、紧急停止信号、故障隔离命令。它们不能简单排队或拒绝，而要预留专用通道和资源。

第二，调用方不会遵守反馈。你返回 429，但客户端立刻无退避重试；你让上游慢一点，但上游协议没有流控字段。此时 backpressure 信号发出去了，但没有闭环。需要网关限流、连接级限速、配额或直接断开。

第三，任务必须完成且不能拒绝。比如已提交订单的状态推进。这里不是不做 backpressure，而是不能在业务已经承诺后简单拒绝。应该在更早的 admission 层拒绝新请求，已经接收的任务进入 durable queue。

第四，过载来自 bug 或死锁。队列涨是结果，不是原因。backpressure 可以止血，但不能替代修 bug。比如 worker 全卡在同一把锁，拒绝新请求只能避免更坏，不能恢复吞吐。

第五，纯离线、无上游反馈路径的批处理。可以用调度器限速或容量规划，但传统意义的 backpressure 没有接收方可以反馈，只能暂停 source 或减少并发。

面试里可以这样答：

```text
backpressure 适合生产速度可能超过消费速度的路径：服务入口、本地队列、流式处理、日志写入、数据库/外部 API、LLM/GPU 后端。它不适合简单处理安全关键事件、调用方不遵守反馈的协议、已经承诺必须完成的任务、由 bug/死锁导致的过载，或没有反馈路径的离线批处理。必须完成的业务要在 admission 层尽早拒绝新请求，已接收的任务靠 durable queue 和幂等完成。
```

一句话：有反馈闭环的容量边界适合 backpressure，没有闭环时只能限流、隔离或停源。

## Q055. backpressure 和相近概念最容易混淆的边界在哪里？

**回答：**

backpressure 经常和 rate limiting、load shedding、circuit breaker、bulkhead、bounded queue、admission control 混在一起。它们可以组合，但不是一回事。

rate limiting 控制入口速率，通常按用户、租户、IP、API key 或全局 QPS 配置。它可以不看系统当前是否过载。backpressure 是根据实时容量反馈调节，队列深、worker 忙、下游慢时触发。

load shedding 是丢弃或拒绝部分工作。它是 backpressure 的一种执行方式。backpressure 还可以表现为阻塞、减速、返回 retry-after、降低优先级、降级响应。不是所有 backpressure 都是直接丢。

circuit breaker 看依赖健康。下游失败率或慢调用比例高，就打开，快速拒绝对该依赖的调用。backpressure 看本地或链路容量。下游完全健康，但本地队列满了，也应该 backpressure。

bulkhead 是隔离。把不同任务、租户或依赖分到不同资源池，避免互相拖死。backpressure 是反馈。bulkhead 可以让 backpressure 局部化，比如 LLM pool 满了只拒绝 LLM 请求，不影响普通 task。

bounded queue 是测量和承压点。队列深度、剩余容量、oldest age 可以触发 backpressure，但队列本身不是反馈策略。队列满了以后怎么通知上游，才是 backpressure。

admission control 是入口决策。它决定这个请求现在能不能进入系统。backpressure 是 admission control 的重要输入，也可以在系统内部多个阶段发生。比如入口接收了任务，但写对象存储前发现下游慢，也可以对该阶段施加 backpressure。

面试里可以这样答：

```text
rate limiting 是按配置限制速率，backpressure 是按实时容量反馈减速；load shedding 是 backpressure 的一种执行方式；circuit breaker 判断下游健康，backpressure 判断本地或链路容量；bulkhead 做资源隔离，backpressure 做压力反馈；bounded queue 提供队列水位和等待时间信号，但不是策略本身；admission control 是入口是否接收的决策，backpressure 可以作为它的输入。
```

一句话：backpressure 的关键词是“反馈”，不是所有拒绝、限流、熔断都叫 backpressure。

## Q056. backpressure 在高并发场景下可能出现哪些隐藏问题？

**回答：**

backpressure 设计不好，会把过载从一个地方搬到另一个地方，甚至制造新的风暴。高并发下尤其明显。

第一，阻塞式 backpressure 会堆 goroutine。队列满了以后，提交方全部阻塞等待空间；如果这些提交方是请求 handler，用户连接也被占住。系统没有 OOM 在队列里，但可能 OOM 在等待者里。

第二，快速拒绝会触发重试风暴。上游看到 503 或 ErrFull 后立即重试，入口流量变成原来的几倍。没有 retry-after、指数退避、jitter 和 retry budget，backpressure 会变成放大器。

第三，反馈信号太粗。只用全局 queue depth 判断，会误伤健康租户；只用 CPU 判断，会漏掉下游连接池；只用下游错误率判断，会错过慢调用。高并发系统要分资源、分队列、分租户看。

第四，信号延迟。等 queue depth 已经满了才拒绝，可能已经晚了；等 p99 慢调用进入统计窗口，队列已经堆了几秒。backpressure 需要同时看领先指标，比如 oldest age、连接池等待、in-flight、remaining capacity。

第五，振荡。阈值太硬：超过 1000 全拒，低于 1000 全放，系统会在开关之间抖动。要用 hysteresis、冷却时间、渐进降载，而不是单点开关。

第六，优先级反转。低价值批处理任务已经占满队列，关键请求来了反而被拒。没有优先级和保留容量时，backpressure 保护的是“先来的任务”，不是“更重要的任务”。

第七，下游反馈传不回来。某个 worker 或依赖已经慢了，但控制面还按旧状态继续分配任务。分布式系统里心跳、指标采样、调度决策都有延迟。

第八，观测污染。backpressure 生效后吞吐下降，如果只看成功吞吐，可能误判为系统变慢；其实系统在主动保护自己。必须同时看 reject/drop/throttle 指标。

面试里可以这样答：

```text
backpressure 高并发下的隐藏问题包括：阻塞式反馈堆积 goroutine，快速拒绝触发上游重试风暴，反馈信号太粗导致误伤，信号滞后导致拒绝太晚，硬阈值引发振荡，低价值任务占满容量造成优先级反转，分布式指标延迟导致下游压力传不回来，以及只看成功吞吐误判 backpressure 效果。需要退避、jitter、retry budget、分租户/分资源指标、hysteresis 和优先级保留容量。
```

一句话：backpressure 不是简单关门，关门方式不对也会把人群挤到门口。

## Q057. backpressure 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

backpressure 一遇到异常路径，就会暴露它是不是持久状态、是不是幂等、调用方会不会遵守。

崩溃时，内存里的 backpressure 状态可能消失。比如队列水位、近期失败率、下游慢调用窗口、熔断状态、每租户 token 都在内存里，进程重启后全清空。系统可能刚重启就误以为自己很健康，瞬间放开入口，形成冷启动冲击。

如果 backpressure 配置需要在重启后保留，就要持久化。LogServe 的 `SetBackpressure` 会把 `BackpressureConfigured` 写到 `system:backpressure` 日志，控制面 bootstrap 时能恢复 queue high watermark、redelivery timeout 和 log append slow limit。这是配置恢复，不是实时水位恢复；实时水位仍然来自当前队列和指标。

超时场景里，backpressure 要区分“等待入口许可超时”和“任务执行超时”。入口许可超时应该返回没有接收成功；任务执行超时则要进入任务状态机，可能失败、取消或 redelivery。把两者混在一起，会出现调用方以为没提交，系统其实后来执行了的情况。

重试场景最容易出事。backpressure 返回拒绝后，上游如果没有退避，会立刻重试；控制面 redelivery 也可能和客户端重试叠加。此时必须有 idempotency key、retry budget、退避和抖动。否则 backpressure 只是在每次拒绝后制造更多请求。

重启还会影响分布式公平。某个 worker 重启后本地队列空了、running 计数归零；控制面如果没有 lease 和心跳，就可能过早把大量任务分给刚恢复的 worker。更稳妥的做法是 warmup：先小流量探测，再逐步恢复容量。

还有一个边界：backpressure 本身不能丢失已经承诺的任务。如果请求已经返回“accepted”，后续因为系统过载而丢掉任务，就是语义错误。已接收任务要么完成，要么失败并可查询，要么进入可恢复队列。backpressure 应该尽量发生在承诺之前。

面试里可以这样答：

```text
backpressure 在崩溃和重启时会暴露状态是否持久：配置可以写日志恢复，但实时水位通常要重算；刚重启不能立刻全量放开，最好 warmup。超时要区分 admission 等待超时和任务执行超时，前者表示未接收，后者进入任务状态机。重试要有 idempotency key、retry budget、backoff 和 jitter，否则拒绝会变成重试风暴。backpressure 不能让已经 accepted 的任务静默丢失，最好发生在承诺之前。
```

一句话：backpressure 的拒绝语义必须清楚，否则异常路径里最容易出现“到底接没接”的混乱。

## Q058. backpressure 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

backpressure 自己通常不是重 CPU 逻辑。它的性能问题多来自信号采集、全局锁、指标聚合、队列扫描，以及把反馈传给上游的路径。

CPU 瓶颈会出现在复杂决策上。比如每次提交都扫描所有任务、所有 worker、所有租户，计算水位和优先级；或者每次 poll 都重算全局统计。LogServe 当前队列扫描和 metadata 查询如果放到更大规模，就会有这种风险。

内存瓶颈来自指标和等待者。为了做 backpressure，系统可能保留大量滑动窗口、直方图、租户状态、最近错误样本；如果 label 维度失控，比如把 task_id 放进指标，会让时序数据爆炸。阻塞式 backpressure 还会让大量 goroutine 在入口等待。

锁竞争很常见。所有请求进入 admission 时都抢同一个 config lock、queue lock、tenant quota lock 或 metrics lock，就会把 backpressure 判断本身变成瓶颈。正确做法通常是把配置读做成低成本快照，把高频计数用 atomic 或分片结构，避免热路径拿大锁。

I/O 瓶颈常来自配置和日志。每次拒绝都同步写审计日志或配置存储，就会把拒绝路径拖慢。拒绝路径应该轻量，详细日志要采样或异步。配置更新可以持久化，但每个请求不应同步读远端配置。

网络瓶颈出现在分布式反馈。worker 要把本地队列、running、下游错误率上报给控制面；控制面再据此调度。如果心跳太频繁，网络和控制面压力大；太慢，backpressure 信号滞后。这里要在准确性和开销之间取舍。

性能上还有一个容易忽略的问题：backpressure 生效后，错误响应本身也可能成为热点。大量 429/503 如果都走完整日志、trace、告警、重试回调，会把系统压垮。失败路径要比成功路径更轻。

面试里可以这样答：

```text
backpressure 的瓶颈通常不在算法 CPU，而在热路径信号采集和反馈传播。CPU 来自每次提交扫描全局队列或 worker；内存来自高维指标、滑动窗口和阻塞等待者；锁竞争来自 admission、queue、quota、metrics 的全局锁；I/O 来自同步记录拒绝日志或读取配置；网络来自 worker 到控制面的水位上报和控制面到上游的反馈。拒绝路径必须轻量，指标要限维度，配置和计数要避免热锁。
```

一句话：backpressure 是保护机制，但它自己也必须便宜，否则会变成新的入口瓶颈。

## Q059. backpressure 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

backpressure 的测试不能只测“队列满了会报错”。它要测反馈语义、异常交错和过载曲线。

correctness test 先测确定性规则：

```text
queue depth 达到 high watermark 后，新任务被拒绝。
未达到阈值时，任务能正常进入。
幂等重复请求不被 backpressure 误伤。
log append 慢时，新提交被拒绝。
redelivery timeout 配置能生效。
拒绝返回明确错误，不创建 task_id。
已 accepted 的任务不会因为后续 backpressure 静默丢失。
配置更新写入日志，重启后能恢复。
```

LogServe 已经有类似测试：队列水位触发 backpressure、幂等重复绕过 backpressure、log append slow 触发拒绝、backpressure 配置从日志恢复。这些测试比单纯测 queue length 更接近真实语义。

stress test 要测过载和交错：

```text
大量 SubmitTask 并发打到 high watermark。
SetBackpressure 和 SubmitTask 并发。
worker poll、redelivery、submit 同时发生。
log append 变慢时，入口是否快速失败。
拒绝后上游带 backoff 重试，系统是否恢复。
控制面重启时，backpressure 配置和队列状态是否一致。
多租户热点流量是否挤掉普通流量。
```

这类测试要配 `-race`、重复次数和不同 CPU 数。重点看是否有超收、重复创建任务、配置半更新、死锁、goroutine 泄漏。

benchmark 要测曲线，不是测单点：

```text
不同 high watermark 下的吞吐、拒绝率、p99。
不同 submit 并发下 admission path 的 ns/op 和 allocs/op。
不同队列深度下 PollTask 和 SubmitTask 延迟。
拒绝路径和成功路径的成本差异。
指标采集开销。
配置读锁或 atomic 快照的成本。
```

系统压测要把 overload 打出来：先让系统稳定，再逐步提高到超过服务率，看 queue depth、oldest age、reject rate、成功吞吐、错误率、恢复时间。好的 backpressure 曲线应该是：过载后拒绝率上升，成功请求 p99 不无限恶化，内存和 goroutine 不持续增长，下游错误不被重试放大。

面试里可以这样答：

```text
correctness test 测阈值语义：队列到 high watermark 后拒绝，未到阈值能接收，幂等重复不误伤，拒绝不创建任务，已 accepted 任务不被静默丢弃，配置能持久化恢复。stress test 测并发交错：大量 submit、配置更新、worker poll、redelivery、log append slow、重试同时发生，并配合 -race。benchmark 测 admission 成本、拒绝路径成本、不同水位下吞吐/p99/拒绝率、指标开销和过载后的恢复曲线。
```

一句话：backpressure 测试要证明它能拒绝，也要证明它拒绝得及时、便宜、不会乱。

## Q060. 如果要求从零实现一个简化版 backpressure，你会先定义哪些不变量？

**回答：**

从零实现 backpressure，我会先定义“什么时候接收、什么时候拒绝、拒绝后算不算接收”。这几个不变量比具体阈值更重要。

第一，承诺边界不变量。请求只有在通过 admission 并写入必要状态后，才能返回 accepted。返回 rejected 的请求不能留下半创建任务。这个边界必须清楚，否则重试和状态查询都会乱。

第二，容量阈值不变量。每个受保护资源都有明确阈值：队列容量、oldest age、running 数、连接池等待、内存水位、下游错误率。阈值要有单位和测量窗口，不能只写“系统繁忙”。

第三，拒绝语义不变量。被拒绝时返回什么错误、是否可重试、建议多久重试、是否计入指标，要固定。HTTP 可以是 429/503，gRPC 可以是 `RESOURCE_EXHAUSTED` 或 `UNAVAILABLE`。调用方要能区分业务失败和容量拒绝。

第四，幂等不变量。相同 idempotency key 的重复请求，如果原任务已经接收，应该返回已有状态，而不是因为当前高水位被拒绝。LogServe 现在就是先查 idempotency，再检查队列水位。

第五，单调保护不变量。系统越接近容量上限，接收策略只能更保守，不能因为并发竞态出现“大家同时看到还有容量，于是一起超收很多”。可以允许少量误差，但要定义上界。

第六，恢复不变量。水位下降后，系统可以恢复接收，但不要抖动。要有 hysteresis、冷却时间或平滑窗口。比如超过 1000 拒绝，低于 800 才恢复，而不是围着 1000 来回跳。

第七，优先级不变量。高优先级或控制类请求要有保留容量；低优先级请求不能占满全部资源。否则 backpressure 会保护先到者，而不是保护重要工作。

第八，观测不变量。每次拒绝都要计数并带 reason：`queue_full`、`oldest_age`、`log_slow`、`downstream_slow`、`tenant_quota`。同时记录当前水位。没有 reason 的 reject 指标没法指导调参。

第九，反馈闭环不变量。拒绝或减速信号必须能被上游理解。返回错误码、retry-after、gRPC status、stream demand、或者协议层窗口更新都可以；如果上游不会遵守，就要在网关或连接层强制限速。

第十，失败路径轻量不变量。backpressure 触发时，拒绝路径不能做昂贵同步 I/O，不能每次打满日志，不能拿全局大锁。否则过载时保护机制自己会成为热点。

一个简化版 admission 函数可以这样描述：

```go
func Admit(ctx context.Context, req Request) (Decision, error) {
    if existing := findIdempotent(req.Key); existing != nil {
        return AcceptExisting(existing), nil
    }
    snapshot := capacitySnapshot()
    if snapshot.QueueDepth >= highWatermark {
        return Reject("queue_full", retryAfter(snapshot)), nil
    }
    if snapshot.OldestAge > maxQueueAge {
        return Reject("queue_old", retryAfter(snapshot)), nil
    }
    return AcceptNew(), nil
}
```

真正实现时，`capacitySnapshot` 不能太贵；`AcceptNew` 必须和创建任务状态在同一个同步边界内，避免检查时还有容量、创建时已经超收。

面试里可以这样答：

```text
从零实现 backpressure，我会先定义不变量：accepted/rejected 的承诺边界清楚，rejected 不留下半任务；每个容量信号有明确阈值、单位和窗口；拒绝错误可区分、可观测、可重试策略明确；幂等重复请求不被水位误伤；并发 admission 不能无限超收；恢复要有 hysteresis，避免抖动；高优先级有保留容量；每次拒绝带 reason 和水位；反馈信号能被上游理解；拒绝路径必须轻量，不能在过载时同步做重 I/O 或抢全局大锁。
```

一句话：backpressure 的不变量，是先说清楚“没接收就是没接收”，再说清楚为什么拒绝、何时恢复。

## 参考资料

- Go blog: Go Concurrency Patterns: Pipelines and cancellation https://go.dev/blog/pipelines
- Go blog: Defer, Panic, and Recover https://go.dev/blog/defer-panic-and-recover
- Go language specification: channel types、send/receive、close、buffer capacity https://go.dev/ref/spec#Channel_types
- `golang.org/x/sync/semaphore` package documentation: weighted semaphore、bounded concurrent access、worker pool 示例 https://pkg.go.dev/golang.org/x/sync/semaphore
- `golang.org/x/time/rate` package documentation: token bucket、Allow、Reserve、Wait、burst https://pkg.go.dev/golang.org/x/time/rate
- `go.uber.org/ratelimit` package documentation: leaky-bucket rate limit、`Take()` https://pkg.go.dev/go.uber.org/ratelimit
- `context` package documentation: cancellation、deadline、Done channel https://pkg.go.dev/context
- `sync` package documentation: Mutex、Cond、WaitGroup、Pool 等同步原语 https://pkg.go.dev/sync
- `os/signal` package documentation: `NotifyContext` 和信号驱动 shutdown https://pkg.go.dev/os/signal
- `os/exec` package documentation: `CommandContext` 和外部进程取消 https://pkg.go.dev/os/exec
- `errgroup` package documentation: goroutine group、error propagation、SetLimit https://pkg.go.dev/golang.org/x/sync/errgroup
- `testing` package documentation: benchmark、`RunParallel`、allocation reporting https://pkg.go.dev/testing
- `runtime` package documentation: GOMAXPROCS、NumCPU、block/mutex profile controls https://pkg.go.dev/runtime
- `runtime/metrics` package documentation: `/sched/gomaxprocs`、runnable/running/waiting goroutine metrics、scheduler latency、mutex wait https://pkg.go.dev/runtime/metrics
- `runtime/trace` package documentation: execution trace、goroutine blocking/unblocking、task/region/log annotations https://pkg.go.dev/runtime/trace
- Go Diagnostics: profiling、execution trace、pprof 观测入口 https://go.dev/doc/diagnostics
- `runtime/pprof` package documentation: goroutine、block、mutex、heap profiles https://pkg.go.dev/runtime/pprof
- Data Race Detector: `go test -race`、运行时竞态检测 https://go.dev/doc/articles/race_detector
- Resilience4j CircuitBreaker documentation: CLOSED、OPEN、HALF_OPEN、sliding window、slow-call rate https://resilience4j.readme.io/docs/circuitbreaker
- Envoy documentation: circuit breaking limits、connection/request/retry overflow、network-level enforcement https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/upstream/circuit_breaking
- Reactive Streams: asynchronous stream processing with non-blocking back pressure https://www.reactive-streams.org/
- Google SRE: Handling Overload, client-side throttling, quota rejection https://sre.google/sre-book/handling-overload/
- Google SRE: Addressing Cascading Failures, overload testing, load shedding, degraded modes https://sre.google/sre-book/addressing-cascading-failures/
- Kubernetes documentation: Pod termination and termination grace period https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/#pod-termination
- John D. C. Little, "A Proof for the Queuing Formula: L = λW", Operations Research, 1961 https://doi.org/10.1287/opre.9.3.383
- Robert D. Blumofe and Charles E. Leiserson, "Scheduling Multithreaded Computations by Work Stealing", Journal of the ACM, 1999 https://doi.org/10.1145/324133.324234
