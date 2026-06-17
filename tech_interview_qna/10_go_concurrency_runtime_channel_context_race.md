# 10. Go 并发、runtime、channel、context 与 race 检测

这一组问题讨论的是 Go 并发的基础层：goroutine 怎么被 runtime 调度，channel 到底提供了什么同步语义，context 为什么经常和 goroutine 生命周期绑在一起，以及排查并发问题时应该看哪些工具。

面试里回答这类题，不要只说“goroutine 很轻量”“channel 用来通信”。这两句话都对，但太浅。更稳的答法是把它拆成四条线：

```text
调度线：
  goroutine 是 G，OS thread 是 M，P 提供执行 Go 代码所需的调度和运行资源。

同步线：
  channel 的 send、receive、close 不只是传值，还会建立 memory model 里的 happens-before 关系。

生命周期线：
  goroutine 必须有退出路径，常见退出信号来自 channel close、context cancellation、deadline 或上游关闭。

排查线：
  goroutine profile 看谁卡住，block/mutex profile 看阻塞点，trace 看调度过程，race detector 看未同步共享内存访问。
```

这里的事实依据主要来自 Go 官方 FAQ、Go 语言规范、Go memory model、runtime 源码注释、`context`/`runtime/pprof`/race detector 文档。官方文档里有一句很重要的话：如果必须靠读完整 memory model 才能解释程序行为，那程序很可能已经写得太聪明了。工程里更好的做法是把所有共享状态访问放在清晰的同步边界内。

## Q001. goroutine 和 OS thread 的关系是什么？

**回答：**

goroutine 不是 OS thread。goroutine 是 Go runtime 管理的轻量级执行单元，OS thread 是操作系统调度的内核线程。Go runtime 会把大量 goroutine 多路复用到一组 OS thread 上执行。

可以先用一句话概括：

```text
goroutine 是 Go 程序看到的并发单位，OS thread 是操作系统真正执行代码的单位，中间由 Go scheduler 负责映射和切换。
```

当你写：

```go
go f()
```

Go 语言规范说，这会启动一个新的 goroutine，调用方不会等待 `f` 执行完；`f` 返回时，这个 goroutine 也结束。注意这里说的是 goroutine，不是新建一个 OS thread。runtime 通常会复用已有线程，把这个 goroutine 放进可运行队列，后续由某个有执行资格的线程把它取出来运行。

goroutine 和 OS thread 的关系可以从几个角度看。

第一，数量不是一一对应。一个 Go 进程里可以有成千上万个 goroutine，但同一时刻真正并行执行 Go 代码的数量通常受 `GOMAXPROCS` 限制。`GOMAXPROCS` 控制的是 P 的数量，也就是最多有多少个执行 Go 代码的“许可证”。OS thread 的数量可以多于 P，因为有些线程可能正在系统调用里阻塞，有些线程可能空闲，有些线程可能用于 cgo、网络轮询或 runtime 自己的工作。

第二，goroutine 阻塞时，runtime 会尽量让线程去执行别的 goroutine。比如一个 goroutine 在 channel receive 上阻塞，runtime 可以把这个 goroutine park 掉，让当前 M/P 去运行别的 runnable goroutine。这样一个阻塞的 goroutine 不会天然占住一个 OS thread。普通线程模型里，如果线程在同步原语上阻塞，这个线程通常就交回内核调度；Go 的好处是 goroutine 阻塞可以先被 runtime 调度器吸收一部分。

第三，遇到阻塞系统调用时，情况稍微不同。M 进入系统调用后可能暂时离开 Go 调度器。如果它持有 P，runtime 会把 P 交给别的 M，让其他 goroutine 继续运行。系统调用回来后，这个 M 要重新拿到 P 才能继续执行 Go 代码。这个机制解释了为什么 Go 可以在有阻塞 syscall 的情况下仍然保持并发度，也解释了为什么 OS thread 数量有时会超过 `GOMAXPROCS`。

第四，goroutine 的栈和线程栈不同。官方 FAQ 里提到 goroutine 初始栈很小，runtime 会按需要增长和收缩。OS thread 的栈通常按操作系统线程模型分配，成本更高。goroutine 便宜，主要便宜在调度由 runtime 管理、栈按需增长、创建和切换成本都比 OS thread 低得多。

但不能把 goroutine 理解成“完全不受线程影响”。有几个边界要记住。

```text
runtime.LockOSThread:
  可以把当前 goroutine 固定到当前 OS thread，常见于 GUI、某些 C 库、线程局部状态或必须跑在主线程的场景。

cgo 或阻塞 syscall:
  可能导致 runtime 创建或保留更多 OS thread。

线程局部变量:
  Go 代码一般不应该依赖 OS thread identity，因为普通 goroutine 可能在不同 M 上继续运行。

并行度:
  goroutine 很多不代表 CPU 并行度无限，真正同时跑 Go 代码仍然受 P、CPU 核数和调度状态限制。
```

面试里可以这样答：

```text
goroutine 是 Go runtime 管理的轻量级并发单元，OS thread 是操作系统执行代码的线程。Go scheduler 会把很多 goroutine 复用到少量 OS thread 上。一个 goroutine 阻塞在 channel、锁、timer 等 runtime 能感知的点时，runtime 可以 park 它并调度其他 goroutine；如果线程进入阻塞 syscall，runtime 会尽量把 P 交给别的线程，避免整体并发度掉下来。所以 goroutine 和 OS thread 不是一一对应关系，goroutine 更像用户态调度的任务，OS thread 是底层执行载体。
```

一句话：goroutine 是 Go 的并发抽象，OS thread 是底层执行资源，runtime scheduler 负责把前者安排到后者上。

## Q002. Go scheduler 的 G、M、P 大致表示什么？

**回答：**

G、M、P 是 Go runtime scheduler 里最常见的三个概念。

```text
G:
  goroutine，表示一段要执行的 Go 代码以及它的栈、状态、调度信息。

M:
  machine，也就是 OS thread。真正执行指令的是 M。

P:
  processor，不是 CPU 硬件本身，而是执行 Go 代码所需的一组 runtime 资源和资格。
```

runtime 的 HACKING 文档说得很直：G 是 goroutine；M 是 OS thread；P 表示执行 Go 用户代码所需的资源，例如调度器和内存分配器状态。调度器的工作就是把一个 G、一条 M 和一个 P 配到一起。M 要执行 Go 代码，必须拿着 P；没有 P 的 M 可以阻塞在 syscall、执行 runtime 代码、空闲，但不能正常跑用户 Go 代码。

可以把一次普通调度想成这样：

```text
1. 程序执行 go f()，runtime 创建一个 G。
2. 这个 G 被放到某个 P 的本地 run queue，或者全局 run queue。
3. 某个 M 拿着一个 P，从队列里取出 runnable G。
4. M 执行这个 G 的代码。
5. G 如果阻塞、让出、被抢占或运行结束，M/P 再去找下一个 G。
```

G 的重点是“任务状态”。一个 G 里面有 goroutine 栈、入口函数、当前执行状态、调度上下文等信息。它可能处于 runnable、running、waiting、syscall、dead 等状态。我们平时说 goroutine leak，本质上就是一些 G 本该结束，却长期停在 waiting、IO wait、select、chan receive 等状态里。

M 的重点是“执行载体”。M 对应 OS thread，最终由操作系统调度到 CPU 上运行。Go runtime 可以创建多个 M，也可以让 M 休眠、唤醒、进入 syscall 或退出。因为系统调用、cgo、锁线程等原因，M 的数量不一定等于 `GOMAXPROCS`。

P 的重点是“Go 代码执行资格和局部资源”。P 的数量等于 `GOMAXPROCS`。P 有本地 run queue，还有一些 per-P 的缓存和 runtime 状态。M 必须绑定一个 P 才能执行 Go 用户代码。这个设计的好处是：调度状态可以分散在多个 P 上，减少全局锁竞争；每个 P 有自己的本地队列，也有利于局部性。

举一个 syscall 场景更容易理解：

```text
M1 持有 P1，正在执行 G1。
G1 进入阻塞 syscall。
M1 可能在内核里等很久。
runtime 把 P1 释放出来。
M2 拿到 P1，继续执行其他 runnable G。
G1 syscall 返回后，M1 要重新拿到某个 P，才能继续跑 G1 的 Go 代码。
```

这个过程解释了为什么 P 才是控制 Go 并行度的核心。M 是线程，G 是任务，P 是执行 Go 代码的许可证和资源包。

常见误区有三个。

第一，把 P 理解成 CPU 核。P 不是硬件 CPU，只是 runtime 里的 processor 抽象。P 的数量默认通常和可用 CPU 数相关，但它仍然是 runtime 的概念，可以通过 `GOMAXPROCS` 调整。

第二，以为 M 数量等于 P 数量。不是。M 可以比 P 多，尤其有阻塞 syscall、cgo、`LockOSThread`、网络服务高峰时。只是不持有 P 的 M 不能执行用户 Go 代码。

第三，以为 goroutine 切换一定是内核线程切换。不是。很多 goroutine 调度发生在 runtime 层，不需要内核把一个 OS thread 换成另一个 OS thread。当然，底层 M 最终还是会被 OS 调度。

面试里可以这样答：

```text
G 是 goroutine，代表待执行的 Go 代码和它的运行状态；M 是 OS thread，是真正执行指令的线程；P 是 processor，代表执行 Go 代码所需的调度资源和资格。M 必须绑定 P 才能跑用户 Go 代码，P 的数量由 GOMAXPROCS 控制。调度器的核心工作就是从本地队列、全局队列、netpoll、timer 等地方找到 runnable G，再把 G 放到持有 P 的 M 上执行。
```

一句话：G 是任务，M 是线程，P 是 Go runtime 给线程发的执行资格和本地资源。

## Q003. work stealing 在 Go scheduler 中解决什么问题？

**回答：**

work stealing 解决的是调度负载不均衡问题：有的 P 的本地队列里有很多 runnable goroutine，有的 P 已经没活干了。如果不用 stealing，空闲的 P/M 可能在旁边闲着，而另一个 P 的队列越来越长，CPU 并行度就浪费了。

Go scheduler 为了减少全局锁竞争，不会把所有 runnable goroutine 都塞进一个全局队列。每个 P 都有本地 run queue。这个设计有好处：本地入队/出队快，缓存局部性好，锁竞争少。但它也带来一个问题：任务分布可能不均匀。

比如：

```text
P0 的本地队列有 200 个 G。
P1 的本地队列为空。
P2 的本地队列为空。
P3 的本地队列为空。
```

如果 P1、P2、P3 只看自己的本地队列，它们就会空转或睡眠。work stealing 的思路是：空闲的 P/M 会去别的 P 的本地队列偷一部分 runnable G 过来执行。Go runtime 源码里 `runqsteal` 的注释就写着，它会从另一个 P 的本地 runnable queue 里偷一半元素放到当前 P 的队列里，并返回其中一个 G 立即执行。

它主要解决几个具体问题。

第一，提升 CPU 利用率。只要系统里还有 runnable goroutine，空闲 P 就应该尽量找到活干，而不是因为自己的本地队列为空就睡掉。stealing 能让 runnable G 分散到多个 P 上，最终把可用 CPU 并行度用起来。

第二，保留本地队列的伸缩性。另一种做法是所有任务都进全局队列，但这会让调度路径集中到一个全局结构上，锁竞争和缓存抖动都更明显。per-P queue 加 stealing 是一个折中：平时走本地队列，只有没活干时才去全局队列、netpoll、其他 P 那里找。

第三，避免过度线程唤醒。runtime 源码注释里专门讨论了 worker thread parking/unparking 的平衡：线程太少会浪费并行度，线程太多会浪费 CPU 和电量，还会造成频繁 park/unpark。work stealing 和 spinning worker 的设计配合起来，目标是在有工作时逐步把并行度拉满，在没工作时让多余线程睡下去。

第四，改善局部性和延迟。新 ready 的 goroutine 通常优先放在当前 P 的本地队列，这样相关任务更可能在同一 P 附近执行。只有当其他 P 没活干时才偷，尽量不把所有任务都打散。

大致流程可以这样理解：

```text
当前 M/P 要找下一个 G：
  先看当前 P 的本地 run queue；
  再看全局 run queue；
  再看网络轮询、timer、GC work 等来源；
  如果还没有，就尝试从其他 P 的本地 run queue 偷一部分；
  仍然没有，就进入 spinning 或 park。
```

这里不用背每个细节，面试重点是讲清楚“为什么需要偷”。因为调度状态是分布式的，分布式队列带来伸缩性，也带来不均衡；work stealing 正是这个设计的补偿机制。

常见误区是把 work stealing 理解成“抢占正在运行的 goroutine”。不是。stealing 主要偷的是别的 P 队列里的 runnable G，不是把另一个 M 正在执行的 G 抢过来。正在运行的 G 是否被抢占，是另一个调度问题。

另一个误区是把 stealing 当成公平性保证。它不是严格公平调度。它的目标是吞吐、局部性和 CPU 利用率之间的平衡。Go scheduler 会努力避免长期饿死，但它不是实时调度器，也不承诺 goroutine 按创建顺序执行。

面试里可以这样答：

```text
Go scheduler 有 per-P 本地 run queue，这样可以减少全局队列竞争，也能保留局部性。但本地队列会导致负载不均：某些 P 很忙，某些 P 没活干。work stealing 让空闲的 P/M 去其他 P 的队列里偷一批 runnable goroutine，从而把工作摊开，避免 CPU 空闲。它解决的是分布式调度队列带来的负载均衡问题，不是严格公平性，也不是抢正在运行的 goroutine。
```

一句话：work stealing 是 per-P 本地队列的配套机制，用来在保持调度伸缩性的同时把 runnable goroutine 分摊到空闲 P 上。

## Q004. goroutine 泄漏通常有哪些原因？

**回答：**

goroutine 泄漏指的是：一个 goroutine 按业务逻辑本该退出，但因为等待某个永远不会发生的事件、没有收到取消信号、或者被某个阻塞点卡住，长期留在进程里。它不一定占 CPU，但会占栈、引用对象、文件连接、timer、锁等待关系，时间长了会拖垮服务。

最常见的原因可以按“卡在哪里”来分。

第一类是 channel send 卡住。无缓冲 channel 没有 receiver，或者 buffered channel 已满而没有消费者，发送方就会阻塞。

```go
func leak() {
    ch := make(chan int)
    go func() {
        ch <- 1 // 没有人接收，永远卡住
    }()
}
```

这类问题在 pipeline 里很常见。下游提前返回，上游还在发送；如果上游没有监听 `ctx.Done()` 或 done channel，就会卡在 send 上。

第二类是 channel receive 卡住。消费者在等数据，但生产者已经退出，又没有 close channel。

```go
func worker(ch <-chan Job) {
    for job := range ch {
        handle(job)
    }
}
```

这段代码本身没问题，前提是拥有发送端的一方最终会 `close(ch)`。如果没有 close，`range ch` 会一直等下一个值，worker 就不会退出。

第三类是 select 没有退出分支。很多循环写成：

```go
for {
    select {
    case msg := <-ch:
        handle(msg)
    }
}
```

如果 `ch` 不再有数据，这个 goroutine 会一直等。如果业务需要可取消，通常要加：

```go
case <-ctx.Done():
    return
```

或者监听关闭信号。

第四类是 context 没有 cancel。`context.WithCancel`、`WithTimeout`、`WithDeadline` 会返回 `CancelFunc`。官方 `context` 文档明确说，调用 `CancelFunc` 会释放相关资源；不调用会让子 context 和它的子节点一直挂在父 context 下面，直到父 context 被取消。很多 goroutine leak 其实不是 goroutine 本身写错了，而是调用方忘了 `defer cancel()`。

```go
ctx, cancel := context.WithTimeout(parent, time.Second)
defer cancel()
```

这句 `defer cancel()` 不是形式主义。即使超时时间会到，也应该在操作结束时主动 cancel，尽早释放 timer 和引用关系。

第五类是 I/O 没有 deadline 或关闭路径。goroutine 卡在网络读、数据库调用、外部 RPC、文件读写上。如果没有 context 传递、没有 deadline、连接也不会被关闭，goroutine 可能长期停在 `[IO wait]`。

第六类是 WaitGroup、锁或条件变量使用错误。比如某个分支忘了 `wg.Done()`，另一个 goroutine 永远 `Wait()`；或者持锁后在异常路径没 unlock；或者 `sync.Cond` 等待条件没有被 signal/broadcast。表现上也是 goroutine 越积越多。

第七类是 ticker、timer 或后台循环没有停止。`time.NewTicker` 需要 `Stop()`；后台循环需要退出信号。否则定时任务所在 goroutine 会一直活着。`time.After` 在高频循环里也要小心，它会创建 timer；虽然不一定是 goroutine leak，但可能造成 timer 堆积和内存压力。

第八类是 nil channel 被误用。nil channel 的 send/receive 永远阻塞。如果本来想“关闭”一个 channel，却把它置 nil 后仍然有 goroutine 直接读写它，就可能卡死。nil channel 在 select 里可以用来禁用 case，但直接读写 nil channel 是危险信号。

第九类是 fan-out 不受控。每个请求、每条消息、每个连接都 `go func()`，但没有并发上限、没有超时、没有回收路径。短时间看不出问题，流量一上来 goroutine 数量就会一路涨。

第十类是 panic/recover 或错误路径导致关闭动作没执行。比如生产者 panic 后没有 close 输出 channel，下游 `range` 永远等；或者上游错误返回时忘记通知多个 worker 退出。

可以用一条检查线来判断是否容易泄漏：

```text
这个 goroutine 的退出条件是什么？
谁负责触发退出？
如果下游提前退出，上游会不会知道？
如果上游不再发送，下游会不会知道？
如果外部 I/O 永远不返回，有没有 deadline 或 cancellation？
```

面试里可以这样答：

```text
goroutine leak 的本质是生命周期没有闭环。常见原因包括 channel send 没有 receiver、receive 等不到 close、pipeline 下游提前退出但上游还在发、select 循环没有监听 ctx.Done、context 的 cancel 没调用、I/O 没有 deadline、WaitGroup/锁/Cond 的唤醒路径缺失、ticker 没 Stop，以及 goroutine per request 没有限流。排查时我会先问每个 goroutine 的退出信号来自哪里，谁拥有关闭权，错误路径和取消路径是否也能走到退出。
```

一句话：goroutine 泄漏通常不是“Go 没回收”，而是程序没有给 goroutine 设计可达的退出路径。

## Q005. 如何定位 goroutine leak？

**回答：**

定位 goroutine leak 的核心不是只看数量，而是看“数量是否持续增长”和“增长的是哪一类 stack”。一个服务启动后 goroutine 数量上升到稳定值很正常；问题是某个请求、任务或错误路径之后，goroutine 数量回不去。

我一般按这个顺序排查。

第一步，先确认趋势。可以用 `runtime.NumGoroutine()` 打点，或者把它放到 metrics 里。重点看：

```text
空闲时是否持续上涨；
压测结束后是否回落；
某类请求、重试、超时、客户端断开后是否增加；
每次执行同一个测试用例后是否多出固定数量的 goroutine。
```

如果每跑一次用例多 1 个或多 N 个，基本就有可复现入口了。

第二步，抓 goroutine profile。生产服务通常会引入：

```go
import _ "net/http/pprof"
```

然后访问：

```text
/debug/pprof/goroutine?debug=2
```

或者用：

```text
go tool pprof http://host:port/debug/pprof/goroutine
```

在代码里也可以直接写：

```go
pprof.Lookup("goroutine").WriteTo(os.Stdout, 2)
```

`debug=2` 的文本栈很适合看 leak，因为它会列出每个 goroutine 当前停在哪。常见状态包括：

```text
[chan send]       卡在发送，通常是没人接或 buffer 满。
[chan receive]    卡在接收，通常是没人发或 channel 没 close。
[select]          卡在 select，重点看有没有 ctx.Done 或退出 case。
[IO wait]         卡在网络 I/O，重点看 deadline/context 是否传下去了。
[semacquire]      卡在锁、WaitGroup、Cond 等同步原语。
[sleep]           后台 ticker、timer 或重试循环。
```

第三步，对比两份 profile。不要只看单次快照。更有效的是：

```text
1. 服务刚启动稳定后抓一次 baseline。
2. 执行可疑请求或压测。
3. 等待正常清理时间。
4. 再抓一次 goroutine profile。
5. 比较新增 stack 的形状和数量。
```

如果新增的 goroutine 都停在同一行，比如某个 `out <- value`，那基本就能定位到下游没有消费或没有取消。若都停在 `<-ctx.Done()`，反而说明它们在等取消信号，可能是 cancel 没被调用。若停在 `database/sql`、`net/http` 或 gRPC 的等待栈，要继续查 timeout、连接关闭和 context 传递。

第四步，看 block profile 和 mutex profile。goroutine profile 告诉你“现在卡在哪里”，block profile 能告诉你“过去一段时间在哪些同步点上花了大量阻塞时间”。可以在程序里设置：

```go
runtime.SetBlockProfileRate(1)
```

然后看 `/debug/pprof/block`。如果怀疑锁竞争，再配合：

```go
runtime.SetMutexProfileFraction(1)
```

看 `/debug/pprof/mutex`。这两个 profile 不一定直接证明 leak，但能帮助区分“真的泄漏”还是“被一个慢同步点压住了”。

第五步，用 trace 看调度过程。`runtime/trace` 或 `go test -trace` 能看到 goroutine 创建、阻塞、唤醒、syscall、GC、网络轮询等事件。trace 比 pprof 更重，但当你想知道“这个 goroutine 为什么没有被唤醒”“是不是所有 worker 都卡在同一个 channel”时很有用。

第六步，把 context、pprof label 和请求 ID 串起来。`runtime/pprof` 支持 label，`pprof.Do` 内启动的 goroutine 会继承 label。对复杂服务来说，给关键路径加上 operation、tenant、request class 等标签，之后在 profile 里更容易把一堆相似 goroutine 分组。

第七步，用 race detector 排除另一类并发 bug。`go test -race` 找的是数据竞争，不是 goroutine leak。它不会告诉你哪个 goroutine 没退出。但如果你用普通 bool 当取消标志：

```go
var stopped bool

go func() {
    for !stopped {
        work()
    }
}()

stopped = true
```

这本身就是 data race。某些情况下 goroutine 可能看不到你以为已经写入的退出标志。这里应该改成 channel close、context、mutex 或 atomic。race detector 能帮你抓出这种未同步共享状态。

第八步，写一个能复现的测试。测试里可以记录前后 goroutine 数量，也可以在操作结束后等待一小段时间，再 dump profile。注意不要把 Go test 自己的后台 goroutine、HTTP server、runtime timer 等正常 goroutine 误判为 leak。更可靠的方式是比较特定 stack 是否增加，而不是只比较总数。

一个排查模板可以这样写：

```text
先看 goroutine 数是否随请求增长；
再抓 goroutine profile，找重复最多的阻塞栈；
然后沿着那行代码问：谁负责发送、谁负责接收、谁负责 close、谁负责 cancel；
如果卡在 I/O，看 timeout/deadline；
如果卡在锁或 WaitGroup，看 Done/Unlock/Signal 是否覆盖所有分支；
最后用 race detector 排除共享退出标志或状态变量的数据竞争。
```

面试里可以这样答：

```text
我会先用 runtime.NumGoroutine 或 metrics 确认它是持续增长，而不是启动后的稳定后台 goroutine。然后抓 /debug/pprof/goroutine?debug=2，对比 baseline 和压测后的 stack，重点看重复的 [chan send]、[chan receive]、[select]、[IO wait]、[semacquire]。如果栈指向 channel，就查发送方、接收方和 close/cancel 的所有权；如果指向 I/O，就查 context 和 deadline；如果指向 WaitGroup 或锁，就查 Done/Unlock 是否覆盖错误路径。block profile、mutex profile 和 trace 可以继续缩小阻塞点。race detector 不直接找 leak，但能找出用普通共享变量做退出标志这类错误。
```

一句话：goroutine leak 要靠“趋势 + 栈对比 + 生命周期所有权”定位，不能只盯一个总数。

## Q006. channel 的 send、receive、close 分别有什么语义？

**回答：**

channel 有两个层面的语义：一层是通信，也就是传值；另一层是同步，也就是在 goroutine 之间建立顺序关系。面试里最好两个都讲。

先看 send。

```go
ch <- v
```

send 的语义是把 `v` 发送到 channel。Go 语言规范规定，channel 表达式和要发送的值会先求值，然后通信开始。通信是否阻塞取决于 channel 类型和状态：

```text
unbuffered channel:
  必须有 receiver ready，send 才能继续。

buffered channel:
  buffer 没满时，send 可以把值放进 buffer 后继续。

nil channel:
  send 永远阻塞。

closed channel:
  send 会 panic。
```

send 还有 memory model 语义：一次 send 会 synchronizes-before 对应 receive 的完成。也就是说，发送方在 send 之前完成的写入，接收方在 receive 之后可以可靠观察到，前提是这次 receive 确实接到了这次 send 的值。

再看 receive。

```go
v := <-ch
v, ok := <-ch
```

receive 的语义是从 channel 取一个值。它的阻塞规则是：

```text
unbuffered channel:
  必须有 sender ready，receive 才能继续。

buffered channel:
  buffer 非空时，receive 可以立即取出一个值。

nil channel:
  receive 永远阻塞。

closed channel:
  如果 buffer 里还有旧值，先取旧值；
  buffer 清空后，立即返回元素类型零值。
```

双返回值形式里的 `ok` 很重要。`ok == true` 表示这个值来自一次成功 send；`ok == false` 表示 channel 已关闭并且已经没有 buffered 值，这次拿到的是零值。

再看 close。

```go
close(ch)
```

close 的语义不是“关闭连接”，也不是“释放内存”，而是记录：这个 channel 不会再有新的值发送。close 之后：

```text
已经在 buffer 里的值仍然可以被接收；
buffer 被读空后，receive 立即返回零值和 ok=false；
send 到 closed channel 会 panic；
再次 close closed channel 会 panic；
close nil channel 会 panic。
```

close 还有广播效果：多个 goroutine 等待同一个 channel receive 时，channel 关闭后它们都可能被唤醒。常见的 done channel 就利用了这个语义：

```go
done := make(chan struct{})

go func() {
    <-done
    cleanup()
}()

close(done) // 广播取消信号
```

但 close 不携带原因。如果需要取消原因、deadline、跨 API 传递取消信号，通常用 `context.Context` 更合适。

channel 还有 FIFO 语义。Go 语言规范说，channel 是 first-in-first-out queue；如果一个 goroutine 往 channel 发送多个值，另一个 goroutine 从同一个 channel 接收，会按发送顺序收到。这里的说法要谨慎：多个 sender 并发发送时，全局顺序由实际同步发生的顺序决定，不是业务上你想象的顺序。

把三者放在一起看，可以得到一张小表：

| 操作 | 正常 channel | nil channel | closed channel |
| --- | --- | --- | --- |
| send | 无缓冲等 receiver；有缓冲等空间 | 永远阻塞 | panic |
| receive | 无缓冲等 sender；有缓冲等数据 | 永远阻塞 | 先读 buffer，之后零值且 `ok=false` |
| close | 标记不再发送，唤醒接收方 | panic | panic |

常见使用边界是：谁发送，谁关闭。更准确地说，应该由“拥有发送端生命周期的一方”关闭 channel。receiver 通常不应该 close 一个仍可能被别人发送的 channel，因为它不知道是否还有并发 send；send 和 close 并发时，send 可能 panic。

面试里可以这样答：

```text
send 是把值交给 channel，可能因为没有 receiver 或 buffer 满而阻塞；对 closed channel send 会 panic，对 nil channel send 会永远阻塞。receive 是从 channel 取值，可能因为没有 sender 或 buffer 空而阻塞；从 closed channel 读会先读完 buffer，再返回元素零值和 ok=false；从 nil channel 读永远阻塞。close 表示不会再有新值发送，它会让接收方在 buffer 读完后立即得到零值和 ok=false；重复 close、close nil channel、send closed channel 都会 panic。channel 操作还建立同步关系，send synchronizes-before 对应 receive 完成，close synchronizes-before 因关闭而返回零值的 receive。
```

一句话：send 负责交付值，receive 负责取值，close 负责声明“不会再有新值”，三者同时也是同步原语。

## Q007. 向已关闭 channel 发送会发生什么？

**回答：**

向已关闭 channel 发送会触发运行时 panic。不是返回 false，也不是静默丢弃。

```go
ch := make(chan int)
close(ch)
ch <- 1 // panic: send on closed channel
```

Go 语言规范对这一点写得很明确：send on a closed channel causes a run-time panic。原因也很合理：close 的语义是“不会再有值发送”。如果关闭后还允许 send，就会破坏 receiver 对关闭信号的判断。

这件事在工程里最容易出现在两个场景。

第一个场景是多个 sender 里有人提前 close。

```go
func bad() {
    ch := make(chan int)

    for i := 0; i < 3; i++ {
        go func(i int) {
            defer close(ch) // 错：多个 sender 都可能 close
            ch <- i
        }(i)
    }
}
```

这里可能第一次 close 后，其他 sender 还在发送，于是 panic；也可能多个 goroutine 重复 close，于是 panic。正确做法通常是让一个协调者在所有 sender 完成后 close：

```go
ch := make(chan int)
var wg sync.WaitGroup

for i := 0; i < 3; i++ {
    wg.Add(1)
    go func(i int) {
        defer wg.Done()
        ch <- i
    }(i)
}

go func() {
    wg.Wait()
    close(ch)
}()
```

第二个场景是 receiver 试图主动 close 输入 channel。receiver 只知道自己不想再接收，不知道 sender 是否还会发送。它 close 掉 channel 后，sender 继续 send 就会 panic。receiver 如果想停止，通常应该通过 context、done channel 或另一个控制 channel 通知 sender，不要随手关闭别人的输出。

```go
func producer(ctx context.Context, out chan<- int) {
    defer close(out)
    for i := 0; ; i++ {
        select {
        case <-ctx.Done():
            return
        case out <- i:
        }
    }
}
```

这个模式里，producer 拥有 `out` 的发送端生命周期，所以由 producer close。消费者不想要数据时，调用 cancel，而不是 close `out`。

还有一个常见问题：“能不能先判断 channel 是否关闭，再决定是否发送？”Go 没有安全的内置 `isClosed(ch)` 用于这种用途。即使你自己用非阻塞 receive 试探，也会有竞态：你检查时没关闭，不代表下一瞬间不会被别的 goroutine close。真正可靠的办法是设计所有权：

```text
一个 channel 只有一个关闭责任方；
close 发生在所有 send 完成之后；
多方都可能请求关闭时，用 sync.Once 或单独的 coordinator；
sender 发送时同时监听 ctx.Done，避免下游退出后卡住或 panic。
```

`recover` 可以捕获 `send on closed channel` 的 panic，但这不是常规控制流。把 panic 当成“发送失败”来处理，通常说明 channel 所有权设计已经乱了。

面试里可以这样答：

```text
向 closed channel send 会 panic。原因是 close 已经声明这个 channel 不会再有新值。工程里要避免这个问题，关键不是发送前检查是否关闭，而是明确 channel 所有权：通常由发送方，或者拥有所有发送方生命周期的 coordinator，在确认不会再有 send 之后 close。receiver 不应该随便 close 输入 channel；它想停止时应该发取消信号，比如 context cancellation。
```

一句话：closed channel 只能继续 receive，不能再 send；避免 panic 的办法是设计关闭所有权，而不是临时探测状态。

## Q008. 从已关闭 channel 读取会发生什么？

**回答：**

从已关闭 channel 读取不会 panic。它会先把 channel buffer 里已经发送的值读完；等 buffer 空了之后，每次 receive 都会立即返回元素类型的零值。如果用双返回值形式，第二个返回值 `ok` 会是 `false`。

例子：

```go
ch := make(chan int, 2)
ch <- 10
ch <- 20
close(ch)

v, ok := <-ch // 10, true
v, ok = <-ch  // 20, true
v, ok = <-ch  // 0, false
v, ok = <-ch  // 0, false
```

这说明 close 不会清空 buffer。它只是禁止后续 send，并让 receiver 在已有值读完后知道“不会再有值了”。

这也是为什么 `for range ch` 能工作：

```go
for v := range ch {
    handle(v)
}
```

`range ch` 会持续 receive，直到 channel 被关闭并且 buffer 被读空。读到 `ok=false` 后，循环退出。

这里有几个细节要讲清楚。

第一，零值可能是合法业务值。比如 `chan int` 的零值是 0，`chan string` 的零值是空字符串，`chan *T` 的零值是 nil。如果只写：

```go
v := <-ch
```

你分不清 `v` 是发送方真的发了一个零值，还是 channel 已关闭后自动返回的零值。所以只要零值有业务含义，就应该用：

```go
v, ok := <-ch
if !ok {
    return
}
```

第二，从 closed channel 读取可以作为广播信号。`chan struct{}` 经常用于 done channel：

```go
select {
case <-done:
    return
case item := <-work:
    handle(item)
}
```

当 `close(done)` 后，所有等待 `<-done` 的 goroutine 都可以继续执行。它们拿到的是 `struct{}{}` 的零值，但通常不关心值，只关心“已经关闭”。

第三，close 和 memory model 有同步语义。Go memory model 规定，closing a channel synchronizes-before 一个 receive，因为关闭而返回零值。也就是说，关闭前完成的写入，在接收方观察到关闭后可以可靠看见。这个语义让 done channel 可以作为同步信号使用。

第四，读 closed channel 和读 nil channel 完全不同。closed channel 永远 ready；nil channel 永远不 ready。把 channel 置 nil 是禁用通信，不是广播关闭。

第五，不要把 closed channel 当成通用队列状态查询。`len(ch)` 可以看到当前 buffer 里还有多少值，但并不能可靠表示未来是否还会有值。并发环境里，`len` 只适合调试或指标，不适合作为正确性判断。

面试里可以这样答：

```text
从 closed channel receive 不会 panic。它会先返回 close 前已经进入 buffer 的值，并且 ok=true；buffer 读空之后，receive 会立即返回元素类型零值，并且 ok=false。单返回值形式分不清真实零值和关闭零值，所以如果零值有业务意义，要用 v, ok := <-ch。for range channel 正是依赖这个语义：channel 关闭且 buffer 读空后退出循环。close 还可以作为广播信号，因为等待 receive 的 goroutine 会被唤醒。
```

一句话：closed channel 对 receiver 是“读完剩余值后立即结束”，对 sender 是“禁止再发送”。

## Q009. nil channel 在 select 中有什么作用？

**回答：**

nil channel 的 send 和 receive 永远不能继续。在普通代码里，直接读写 nil channel 会永久阻塞；在 `select` 里，nil channel 对应的 case 永远不会被选中。因此 nil channel 常被用来动态禁用某个 select 分支。

比如一个 merge 函数要同时读两个输入 channel，其中一个关闭后，就不应该继续选它：

```go
for ch1 != nil || ch2 != nil {
    select {
    case v, ok := <-ch1:
        if !ok {
            ch1 = nil // 禁用这个 case
            continue
        }
        out <- v

    case v, ok := <-ch2:
        if !ok {
            ch2 = nil // 禁用这个 case
            continue
        }
        out <- v
    }
}
```

这里如果不把 closed channel 置 nil，会出问题。因为 closed channel 的 receive 会立即返回零值和 `ok=false`，它在 select 里永远 ready。循环可能一直选中这个已经关闭的 channel，造成空转。把它设成 nil 后，这个 case 就被禁用了，select 只会关注还没关闭的 channel。

nil channel 在 select 里常见的用途有几个。

第一，动态打开或关闭某个输入。

```go
var in <-chan Item
if enabled {
    in = realInput
}

select {
case item := <-in:
    handle(item)
case <-ctx.Done():
    return
}
```

当 `enabled == false` 时，`in` 是 nil，这个 receive case 不会被选中。

第二，控制发送节奏。只有当有待发送数据时，才启用 send case：

```go
var out chan<- Item
var next Item

if len(queue) > 0 {
    out = realOut
    next = queue[0]
}

select {
case out <- next:
    queue = queue[1:]
case item := <-in:
    queue = append(queue, item)
}
```

当队列为空时，`out` 是 nil，send case 被禁用，select 不会错误地发送零值。

第三，实现阶段切换。比如一个 goroutine 启动时先等待配置加载，加载完成后才监听数据；或者 shutdown 后禁用新的输入，只继续 drain 已有队列。nil channel 可以让状态机写得比较清楚。

但 nil channel 也容易造成隐蔽死锁。要记住三个边界。

第一，`select` 里所有 case 都是 nil channel，且没有 default，会永久阻塞：

```go
select {
case <-nilCh:
}
```

`select {}` 也是永久阻塞，常用于让 main goroutine 不退出，但业务代码里要慎用。

第二，nil channel 和 closed channel 是相反的。nil channel 永远不 ready，closed channel receive 永远 ready。很多 bug 就出在把这两个状态混为一谈。

第三，nil channel 只适合禁用 case，不适合当作关闭通知。关闭通知应该用 close(done) 或 context cancellation。nil channel 没有广播能力，直接等它只会等到进程结束。

面试里可以这样答：

```text
nil channel 的通信永远不能 proceed，所以在 select 里，涉及 nil channel 的 send/receive case 等价于被禁用。这个特性常用于动态控制 select：某个输入 channel 已经关闭后，把变量设成 nil，避免 closed channel 一直 ready 导致空转；或者只有队列非空时才把 out 设置成真实 channel，启用发送分支。但 nil channel 不是关闭信号，直接读写会永远阻塞；select 里如果只剩 nil case 且没有 default，也会永远阻塞。
```

一句话：nil channel 在 select 里最常用的价值是“动态禁用 case”。

## Q010. buffered channel 和 unbuffered channel 的同步语义有什么区别？

**回答：**

unbuffered channel 是 rendezvous，同步性更强；buffered channel 是有容量的队列，可以把 sender 和 receiver 解耦一段距离。两者都能传值，也都能建立同步关系，但“send 返回”代表的含义不同。

先看 unbuffered channel：

```go
ch := make(chan int)
```

无缓冲 channel 没有存放元素的位置。一次 send 必须等到某个 receiver ready；一次 receive 也必须等到某个 sender ready。双方在通信点相遇，值直接从 sender 交给 receiver。send 和 receive 配对完成后，双方才继续执行。

所以 unbuffered channel 常用于：

```text
强背压：
  消费者没准备好，生产者就不能继续。

交接语义：
  发送方知道值已经被某个接收方接走。

同步点：
  双方在这个通信点建立清晰的 happens-before。
```

例子：

```go
done := make(chan struct{})

go func() {
    prepare()
    done <- struct{}{}
}()

<-done
usePreparedState()
```

这里 send 和 receive 配对。`prepare()` 在 send 之前，send synchronizes-before receive 完成，所以主 goroutine 在 `<-done` 之后可以观察到 `prepare()` 建立的状态。

再看 buffered channel：

```go
ch := make(chan int, 10)
```

buffered channel 有容量。send 在 buffer 未满时可以直接把值放进去并返回，不需要 receiver 此刻 ready；receive 在 buffer 非空时可以直接取值，也不需要 sender 此刻 ready。这让 sender 和 receiver 在时间上解耦。

它适合：

```text
削峰：
  生产速度短时间高于消费速度时，buffer 吸收一部分波动。

队列：
  worker pool 的任务分发。

信号量：
  用容量限制并发数。

减少切换：
  不要求每次发送都和接收方同步相遇。
```

但 buffered channel 的 send 返回，只表示值已经进入 buffer，不表示 receiver 已经处理完。这个边界非常重要。

```go
ch := make(chan Job, 1)
ch <- job // 只说明 job 进了队列，不说明 worker 已经处理
```

如果需要知道 worker 处理完成，应该额外用 ack channel、WaitGroup、result channel 或 context，而不是把“写入 buffered channel 成功”当成完成信号。

从 memory model 看，两者也有差异。

第一，对任意 channel，send synchronizes-before 对应 receive 的完成。也就是说，receiver 拿到某个值后，可以看到 sender 在发送这个值之前完成的写入。

第二，对 unbuffered channel，还有一个反向同步规则：receive synchronizes-before 对应 send 的完成。因为无缓冲通信必须双方 rendezvous，发送方从 send 返回时，接收方已经参与了这次通信。

第三，对 buffered channel，Go memory model 给了一个更一般的规则：容量为 C 的 channel 上，第 k 次 receive synchronizes-before 第 k+C 次 send 完成。这个规则解释了为什么 buffered channel 可以实现计数信号量。

典型信号量写法：

```go
limit := make(chan struct{}, 3)

for _, task := range tasks {
    task := task
    limit <- struct{}{} // acquire
    go func() {
        defer func() { <-limit }() // release
        do(task)
    }()
}
```

这里 channel 容量是 3，最多只有 3 个 goroutine 能成功 acquire 后进入工作区。后续 send 需要等前面有人 receive 释放一个槽位。

可以把两者对比成表：

| 维度 | unbuffered channel | buffered channel |
| --- | --- | --- |
| 容量 | 0 | 大于 0 |
| send 何时继续 | receiver ready 并完成交接 | buffer 有空位 |
| receive 何时继续 | sender ready 并完成交接 | buffer 有数据 |
| 背压 | 每个值都强背压 | buffer 满后才背压 |
| send 返回代表 | 值已被接收方接走 | 值已进入 buffer |
| 常见用途 | 同步点、交接、done 信号 | 队列、削峰、限流、信号量 |

常见误区有几个。

第一，以为 buffered channel 不同步。不是。它仍然通过“某次 send 对应某次 receive”建立同步，只是 send 可以早于 receive 返回。

第二，以为 buffer 越大越好。buffer 变大后，生产者更不容易被背压，错误可能更晚暴露，内存占用也会上升。worker 消费慢时，大 buffer 只是把排队藏起来。

第三，把 buffered channel 当成无锁队列后忘记关闭和取消。channel 本身并不解决生命周期。生产者什么时候停止、消费者什么时候退出、错误时怎么 drain，仍然要设计。

第四，用 buffered channel 当 completion ack。`ch <- task` 成功不是 task 完成，只是排队成功。完成信号要单独建模。

面试里可以这样答：

```text
unbuffered channel 没有容量，send 和 receive 必须同时就绪，双方在通信点 rendezvous；它天然提供强背压和明确的同步点。buffered channel 有队列容量，send 在 buffer 未满时就能返回，receive 在 buffer 非空时就能返回，所以它把生产者和消费者解耦了一段距离。两者都有 happens-before：send synchronizes-before 对应 receive 完成；无缓冲 channel 还可以认为 receive 也同步到对应 send 完成。buffered channel 的 send 返回只表示值进了 buffer，不表示被处理完，所以它适合队列、削峰和信号量，不适合直接当处理完成确认。
```

一句话：unbuffered channel 强调同步交接，buffered channel 强调有限排队和背压延后。

## Q011. channel 适合传递数据还是传递 ownership？

**回答：**

channel 可以传递数据，也可以传递 ownership。更准确地说，channel 传递的是值；如果这个值代表某个资源的唯一使用权，那么它就在传递 ownership。Go 官方博客那句常被引用的话是“不要通过共享内存来通信，而要通过通信来共享内存”。这句话不是说 Go 禁止共享内存，而是说：如果可以把某个对象的访问权通过 channel 交给另一个 goroutine，就尽量不要让多个 goroutine 同时碰同一份可变状态。

先分清“值”和“对象”。

```go
ch := make(chan int)
ch <- 10
```

这里传的是 `int` 值，接收方拿到的是一个拷贝。发送之后，发送方继续使用自己的 `10` 没有问题，因为它不是共享可变对象。

再看指针、slice、map 这类值：

```go
type Task struct {
    ID   string
    Data []byte
}

tasks := make(chan *Task)
tasks <- task
```

channel 里传递的是 `*Task` 这个指针值的拷贝，但它指向的 `Task` 对象还是同一个。发送方如果在发送后继续修改 `task.Data`，接收方也在读写同一块内存，那就不是“通过 channel 传 ownership”，而是“通过 channel 传了一个共享指针”。这个时候仍然可能 data race。

所以真正的 ownership 语义来自约定：

```text
发送前：
  发送方拥有对象，可以修改它。

send 成功后：
  发送方不再访问这个对象，除非接收方之后明确归还。

receive 成功后：
  接收方获得对象的独占访问权，可以修改它。
```

这类模式在 worker pool、buffer 复用、对象池、pipeline 里很常见。

```go
type Buffer struct {
    B []byte
}

free := make(chan *Buffer, 128)
work := make(chan *Buffer, 128)

// producer 拿到空 buffer，填充后把 ownership 交给 worker。
buf := <-free
buf.B = append(buf.B[:0], payload...)
work <- buf

// worker 处理完，再把 ownership 还回 free。
buf = <-work
process(buf.B)
free <- buf
```

这个设计的好处是：同一时刻只有一个 goroutine 持有 `buf` 的修改权。channel 在这里既是队列，也是 ownership 交接点。

但并不是所有 channel 都在传 ownership。有几种情况只是传数据或信号。

第一，传不可变值。

```go
events <- Event{ID: id, CreatedAt: now}
```

如果 `Event` 里没有共享可变引用，或者发送后不会再改，那它就是普通数据传递。

第二，传只读引用。

```go
updates <- snapshot
```

如果 `snapshot` 在发送后被视为 immutable，多个 goroutine 只读访问可以接受。这里传的是共享读权限，不是独占 ownership。

第三，传信号。

```go
done := make(chan struct{})
close(done)
```

`chan struct{}` 常常不关心数据，只关心事件发生。close 还能广播给多个接收方。

第四，传请求和响应句柄。

```go
type request struct {
    key   string
    reply chan result
}
```

这里 channel 承载的是一次交互协议。request 的 ownership 可能只在服务 goroutine 内部，但 `reply` 是回传结果的通道。

面试里最容易加分的点，是把 ownership 和 data race 联系起来。channel 的 send/receive 建立 happens-before，只能保证“发送前的写入对接收后可见”。它不保证发送后双方不再并发访问同一个对象。如果发送方 send 之后继续写同一个 slice，而接收方也读这个 slice，happens-before 并不能救你。

比如这段有问题：

```go
b := []byte("hello")
ch <- b
b[0] = 'H' // 接收方可能正在读 b
```

如果想传 ownership，就要把规则写清楚：

```go
b := []byte("hello")
ch <- b
// 之后发送方不再碰 b
```

或者干脆复制：

```go
copyB := append([]byte(nil), b...)
ch <- copyB
```

常见判断方法是问这几个问题：

```text
发送后，发送方还会不会读写这个对象？
接收方是否可以修改它？
如果接收方处理完，要不要归还给某个池？
有没有多个接收方共享同一个指针？
对象里是否包含 map、slice、指针、interface 等间接引用？
```

面试里可以这样答：

```text
channel 语法上发送的是值，但工程上经常用它传 ownership。传 int、struct 这类值时，接收方拿到的是值拷贝；传指针、slice、map 时，channel 只复制引用，底层对象仍然共享。要把 channel 当 ownership 交接点，就必须约定 send 成功后发送方不再访问这个对象，receive 成功后接收方获得独占访问权，必要时再通过另一个 channel 归还。否则它只是传了一个共享引用，仍然要用 mutex、atomic 或复制来避免 data race。
```

一句话：channel 传的是值，ownership 是建立在“发送后不再碰、接收后独占用”这个约定上的并发设计。

## Q012. 什么时候 channel 比 mutex 更合适？

**回答：**

当问题的核心是“事件、队列、交接、取消、背压、扇入扇出”时，channel 往往比 mutex 更合适。mutex 适合保护共享状态，channel 更适合表达 goroutine 之间的通信协议。

可以先用一个粗略判断：

```text
如果你想表达“谁通知谁”“谁把任务交给谁”“谁等待谁完成”“什么时候停止”，优先考虑 channel。

如果你只是想保护一小块共享内存，优先考虑 mutex。
```

channel 更合适的第一类场景，是 ownership handoff。一个对象从生产者转交给消费者，生产者交出后不再访问它。这样可以避免共享可变状态。

```go
jobs := make(chan *Job)

go func() {
    for job := range jobs {
        handle(job)
    }
}()

jobs <- job // 把 job 的处理权交出去
```

这里如果用 mutex，就需要一个共享队列、一个条件变量或者轮询逻辑；channel 已经把队列和唤醒机制封装好了。

第二类是 pipeline。每个阶段只关心输入和输出：

```go
func parse(in <-chan []byte) <-chan Event {
    out := make(chan Event)
    go func() {
        defer close(out)
        for b := range in {
            out <- decode(b)
        }
    }()
    return out
}
```

这种写法让阶段边界很清楚：上游关闭输入，下游读完退出。mutex 在这里反而会把阶段间的状态揉到一起。

第三类是 fan-in/fan-out。多个 worker 从同一个任务 channel 拉活，或者多个输入汇入一个输出。channel 天然能表达“任务队列”和“结果流”。

```go
jobs := make(chan Job)
results := make(chan Result)

for i := 0; i < n; i++ {
    go worker(jobs, results)
}
```

第四类是取消和广播。`close(done)` 可以唤醒所有 `<-done` 的等待者。`context.Context` 也是类似思路：取消时 `Done()` channel 被关闭，所有监听者都能退出。

```go
select {
case <-ctx.Done():
    return ctx.Err()
case item := <-in:
    return handle(item)
}
```

第五类是背压。无缓冲 channel 强制发送方和接收方 rendezvous；有缓冲 channel 在 buffer 满时给生产者施加背压。这个语义比“队列长度加锁检查”更直接。

```go
sem := make(chan struct{}, 10)

sem <- struct{}{}        // acquire
defer func() { <-sem }() // release
```

这个 buffered channel 就是一个简单信号量。

第六类是一次性结果或完成通知。

```go
done := make(chan error, 1)

go func() {
    done <- doWork()
}()

select {
case err := <-done:
    return err
case <-ctx.Done():
    return ctx.Err()
}
```

如果只用 mutex，你还要自己维护“结果是否 ready”、等待者如何睡眠、超时如何处理。channel 让等待和通信合在一起。

第七类是替代简单 Cond。`sync` 文档也提到，对很多简单场景，channel 比 `sync.Cond` 更合适；close 对应广播，send 对应唤醒一个等待者。这里的重点不是“channel 比所有同步原语高级”，而是它直接表达事件。

但 channel 更合适不等于 channel 总是更好。它也有成本。

```text
状态散落在多个 goroutine 时，调试可能更难；
channel 关闭所有权设计错了，会 panic 或 leak；
过度使用 channel owner goroutine，可能把简单内存访问变成串行消息瓶颈；
需要同步读取多个字段的一致快照时，channel 不一定比 mutex 清楚。
```

面试里可以这样答：

```text
channel 更适合表达通信协议，而不是单纯保护内存。比如任务分发、pipeline、fan-in/fan-out、ownership handoff、done 信号、context cancellation、限流信号量、一次性结果返回，这些场景里 channel 把排队、等待、唤醒、背压和同步关系放在同一个抽象里。它的优势是让 goroutine 之间的关系变成显式消息流。但如果问题只是保护一小块共享状态，用 channel 可能会把简单问题写复杂。
```

一句话：需要表达“交给谁、等谁、何时停”的时候，channel 通常比 mutex 更自然。

## Q013. 什么时候 mutex 比 channel 更合适？

**回答：**

当问题的核心是“多个 goroutine 需要安全访问同一份内存状态”时，mutex 往往比 channel 更合适。mutex 的语义很直接：进入临界区，维护共享状态的不变量，离开临界区。只要共享状态和锁的对应关系清楚，它比 channel owner goroutine 更短、更快，也更容易读。

典型场景是 map、计数器、缓存、连接表、状态机的小块更新。

```go
type Cache struct {
    mu sync.RWMutex
    m  map[string]Value
}

func (c *Cache) Get(k string) (Value, bool) {
    c.mu.RLock()
    defer c.mu.RUnlock()
    v, ok := c.m[k]
    return v, ok
}

func (c *Cache) Set(k string, v Value) {
    c.mu.Lock()
    defer c.mu.Unlock()
    c.m[k] = v
}
```

这段代码用 channel 也能写：起一个 cache goroutine，所有请求通过 channel 发进去，再通过 reply channel 回来。但这样每次读都要一次消息往返，还要定义 request 类型、reply channel、退出协议、错误处理。如果只是保护一个 map，mutex 更合适。

mutex 更合适的第一类场景，是临界区很短。

```go
mu.Lock()
counter++
mu.Unlock()
```

这类操作用 channel 交给单独 goroutine 处理，通常只会增加调度和通信成本。

第二类是需要维护多个字段的不变量。

```go
type Queue struct {
    mu    sync.Mutex
    items []Item
    size  int
}
```

如果 `items` 和 `size` 必须一起更新，mutex 可以把不变量保护在一个临界区里。channel 也能用 owner goroutine 维护，但如果调用方经常要同步查询多个字段，mutex 更直观。

第三类是读多写少。`sync.RWMutex` 可以让多个 reader 并发读取，只在写入时排他。

```go
mu.RLock()
v := config.Current
mu.RUnlock()
```

如果用单个 channel owner goroutine，所有读请求也会被串行化。对于高频读的配置、路由表、缓存索引，这可能成为瓶颈。

第四类是调用路径需要普通函数语义。很多库 API 只是一次同步调用，不适合强行变成异步消息。

```go
func (s *Store) Stats() Stats
func (s *Store) Put(k string, v Value)
```

这些方法内部用 mutex 保护状态，调用方不用知道内部并发实现。用 channel 反而可能把内部协议泄漏到 API 设计里。

第五类是错误处理需要留在当前调用栈。channel 把工作交给另一个 goroutine 后，错误、panic、context、日志字段都要额外传播。mutex 保护的同步函数里，错误可以直接返回，defer 可以直接释放资源。

第六类是性能敏感的共享结构。mutex 在无竞争或低竞争时非常便宜；channel send/receive 也有同步成本，还可能涉及排队、select、调度。不能因为 channel 是 Go 的特色，就把所有共享状态都改成 channel。

当然，mutex 也有边界。

```text
临界区不能太长；
不能在持锁时做慢 I/O；
要避免锁顺序反转和死锁；
要明确哪把锁保护哪些字段；
不要把锁复制出去；
不要在没有必要时用 TryLock 做复杂控制流。
```

一个实用判断是：

```text
如果我能一句话说清楚“这把锁保护这些字段”，mutex 往往是好选择。

如果我必须解释一套跨 goroutine 的任务协议、退出协议、背压协议，channel 可能更合适。
```

面试里可以这样答：

```text
mutex 更适合保护共享内存状态，尤其是短临界区、map/cache/计数器、多个字段不变量、读多写少的数据结构和同步函数 API。比如一个 map 加 RWMutex，读写逻辑直接、性能也好；如果改成 channel owner goroutine，每次读写都变成消息往返，反而复杂。channel 解决通信和生命周期问题，mutex 解决共享状态访问问题。能清楚说出“这把锁保护哪些字段”时，mutex 通常更合适。
```

一句话：mutex 适合保护状态，channel 适合表达通信；把状态访问硬改成消息协议，不一定更 Go。

## Q014. select 的公平性如何理解？

**回答：**

Go 的 `select` 公平性要按语言规范理解：当多个 case 的通信都可以继续时，运行时会从这些 ready case 里选一个，选择方式是 uniform pseudo-random selection。它不是按源码顺序选第一个，也不是严格轮询，也不保证业务层面的绝对公平。

先看最小例子：

```go
select {
case v := <-ch1:
    use(v)
case v := <-ch2:
    use(v)
}
```

如果 `ch1` 和 `ch2` 同时 ready，Go 不保证先选 `ch1`，也不保证这次选 `ch1`、下次就选 `ch2`。规范只说会从可继续的通信里伪随机选一个。这样做的目的，是避免固定源码顺序导致前面的 case 长期压住后面的 case。

但这个“公平”不是调度器意义上的强公平。它至少不保证下面这些事：

```text
不保证每个 ready case 在有限步内一定被选中；
不保证两个 case 长期精确 50/50；
不保证不同 goroutine 之间公平；
不保证 channel 里的多个 sender/receiver 严格按业务期望排序；
不保证带 default 的循环不会饿死其他工作。
```

`select` 的公平性只发生在“进入这一次 select 时，哪些 case 已经 ready”这个局部范围内。每次执行 select，都会重新评估 case 的 channel 表达式和发送值表达式，然后从 ready 集合里挑一个。

这里还有几个容易踩的细节。

第一，case 表达式会先求值。规范说，进入 select 时，receive 的 channel operand、send 的 channel 和右侧表达式都会按源码顺序求值一次；这些副作用不管最后选哪个 case 都会发生。

```go
select {
case ch <- f():
    // 即使这个 send 最后没被选中，f() 也已经执行了
case <-done:
}
```

所以不要把昂贵操作或有副作用的逻辑直接塞进 send case 的右侧。更稳的写法是先准备好值，或者用 nil channel 控制是否启用 send。

第二，closed channel 的 receive 永远 ready。如果某个 input 关闭后不把它设成 nil，它会在 select 里一直参与竞争，可能造成空转：

```go
case v, ok := <-in:
    if !ok {
        in = nil
        continue
    }
```

第三，default 会让 select 非阻塞。带 default 的 select 如果放在 for 循环里，很容易变成 busy loop：

```go
for {
    select {
    case v := <-ch:
        handle(v)
    default:
        // 如果这里不 sleep、不阻塞、不退出，可能烧 CPU
    }
}
```

第四，公平性不等于优先级。如果你想让取消优先，可以显式写两层 select：

```go
select {
case <-ctx.Done():
    return ctx.Err()
default:
}

select {
case <-ctx.Done():
    return ctx.Err()
case item := <-in:
    return handle(item)
}
```

这不是因为 Go 的 select 不好，而是因为“取消优先”是业务策略，语言本身不会替你决定。

第五，nil channel 可以动态禁用 case。这样比依赖概率更清楚：

```go
var out chan<- Item
if len(queue) > 0 {
    out = realOut
}

select {
case out <- queue[0]:
case item := <-in:
    queue = append(queue, item)
}
```

当 `out == nil` 时，send case 不会 ready，也就不会被选中。

面试里可以这样答：

```text
select 的公平性是局部的、伪随机的。规范规定，如果多个通信 case 都能继续，会从 ready case 中做 uniform pseudo-random selection；所以它不会固定偏向源码前面的 case。但这不是严格轮询，也不是实时公平，不保证某个 case 在有限时间内一定被选到。工程上如果需要优先级、关闭后禁用、取消优先或避免 busy loop，要自己用 nil channel、两层 select、队列或调度策略表达出来。
```

一句话：`select` 避免固定顺序偏置，但不提供业务级公平或优先级保证。

## Q015. context cancellation 应该如何在线程或 goroutine 间传播？

**回答：**

Go 里更准确的说法是：context cancellation 在 goroutine 和 API 调用链之间传播。`context.Context` 是并发安全的，同一个 context 可以传给多个 goroutine；取消时，它的 `Done()` channel 会关闭，所有监听这个 `Done()` 的 goroutine 都能收到信号。

官方 `context` 文档给了几个基本规则：

```text
请求入口创建 Context；
对外调用接受 Context；
调用链显式传递 Context；
需要取消、超时、deadline 时，从 parent 派生 child；
操作结束后调用 cancel；
goroutine 里监听 ctx.Done()。
```

常见函数签名是：

```go
func DoSomething(ctx context.Context, arg Arg) error
```

`ctx` 放第一个参数，不放进 struct，不用全局变量。这样调用方能清楚看到这个操作受哪个生命周期控制。

传播的基本形状是树。

```go
func Handle(ctx context.Context, req Request) error {
    ctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
    defer cancel()

    g, ctx := errgroup.WithContext(ctx)

    g.Go(func() error {
        return callA(ctx)
    })

    g.Go(func() error {
        return callB(ctx)
    })

    return g.Wait()
}
```

这里 parent context 被派生出一个带 timeout 的 child。`callA`、`callB` 都拿到同一个 child。只要 timeout 到、handler 返回并调用 cancel、上游请求取消，`ctx.Done()` 都会关闭。子调用应该尽快停止。

如果不用 `errgroup`，也可以手写：

```go
func worker(ctx context.Context, in <-chan Job) error {
    for {
        select {
        case <-ctx.Done():
            return ctx.Err()
        case job, ok := <-in:
            if !ok {
                return nil
            }
            if err := handle(ctx, job); err != nil {
                return err
            }
        }
    }
}
```

关键点是：阻塞点要监听 `ctx.Done()`。只在循环开头检查一次不够，因为 goroutine 可能卡在 channel send、receive、网络 I/O、数据库调用里。

发送方也要监听取消：

```go
select {
case out <- v:
    return nil
case <-ctx.Done():
    return ctx.Err()
}
```

否则下游已经退出，上游还卡在 send 上，就会变成 goroutine leak。

对外部 I/O，要确认库真的接受并尊重 context。比如 HTTP request、database/sql、gRPC 通常都有 context 版本 API。传了 context 不等于一定会立刻停，底层操作要能观察到它。

取消传播还要注意原因。普通 `WithCancel` 只能通过 `ctx.Err()` 得到 `context.Canceled` 或 `context.DeadlineExceeded`。如果你需要知道具体业务原因，可以用 `context.WithCancelCause`，下游用 `context.Cause(ctx)` 读取原因。

```go
ctx, cancel := context.WithCancelCause(parent)
cancel(errors.New("quota exceeded"))

<-ctx.Done()
err := context.Cause(ctx)
```

但不要把 cancellation 当成强制杀线程。Go 没有安全的“从外部杀掉 goroutine”的通用机制。context 是协作式取消：你发信号，goroutine 在合适的点观察信号，然后自己清理并返回。

还要避免几个错误。

第一，不调用 cancel。

```go
ctx, cancel := context.WithTimeout(parent, time.Second)
defer cancel()
```

即使 timeout 最终会发生，也应该在操作提前结束时 cancel，释放 timer 和父子引用。

第二，把 context 存进长期 struct。这样会让每次调用的生命周期混在对象生命周期里，导致旧请求的取消影响新请求，或者新请求无法独立设置 deadline。

第三，用 `context.Background()` 截断上游取消。库函数里随手创建 background，会让调用方取消不了它。只有程序根部、初始化、测试这类地方才适合用 `Background()`。

第四，把 cancel 只放在成功路径。错误路径、超时路径也要能释放资源。

面试里可以这样答：

```text
context cancellation 是协作式传播。入口拿到 ctx 后，沿调用链作为第一个参数传下去；需要局部超时或取消时，用 WithCancel、WithTimeout、WithDeadline 派生子 ctx，并且 defer cancel。启动 goroutine 时把 ctx 传进去，goroutine 在 channel send/receive、I/O、循环等待等阻塞点 select ctx.Done，收到后返回 ctx.Err 或清理退出。同一个 Context 可以安全传给多个 goroutine，取消 parent 会取消所有 derived child。它不是强杀 goroutine，而是一套约定好的停止信号。
```

一句话：context cancellation 要靠显式传递和每个阻塞点主动监听，谁派生谁负责 cancel。

## Q016. context 中为什么不应该存大对象或必需参数？

**回答：**

`context.Context` 的 value 只适合放“跨 API 边界传递的请求级元数据”，不适合放大对象，也不适合放函数必需参数。官方文档说得很明确：context values 只能用于 request-scoped data that transits processes and APIs，不要拿来传 optional parameters。必需参数更不应该放进去。

先说必需参数。假设一个函数真正需要 user ID：

```go
func LoadProfile(ctx context.Context) (*Profile, error) {
    userID := ctx.Value(userIDKey{}).(string)
    return load(userID)
}
```

这段代码的问题是，函数签名没有告诉调用方 `userID` 是必需的。调用方看不出来要往 context 里塞什么；塞错 key、塞错类型、没塞值，问题要到运行时才暴露。更好的写法是：

```go
func LoadProfile(ctx context.Context, userID string) (*Profile, error) {
    return load(ctx, userID)
}
```

这样依赖关系写在类型系统里，测试也更直接。

再说大对象。context 会形成一条 parent-child 链。只要某个 child context 还活着，它可能间接持有 value 引用。如果把大 buffer、大 struct、数据库连接、缓存对象放进去，就可能延长这些对象的生命周期，造成内存占用难以下降。

```go
ctx = context.WithValue(ctx, payloadKey{}, hugePayload)
```

如果 `hugePayload` 是几 MB 的请求体，后面又派生了多个 child context，这个对象可能被整个调用链持有。它不是泄漏的唯一原因，但很容易让内存生命周期变模糊。

还有性能问题。`Context.Value` 是按 key 往 parent 链上查找的。通常这不是瓶颈，但如果你把 context 当参数包，频繁查大量值，既慢又难读。

context value 合适的例子是这些：

```text
trace id；
request id；
auth token 或 caller identity 的轻量引用；
pprof label；
跨进程 API 边界需要透传的元数据；
日志字段中少量请求级信息。
```

这些值有共同特征：

```text
和请求生命周期一致；
不是函数完成业务所必需的显式输入，或者它属于横切关注点；
体积小；
类型和 key 受包内控制；
缺失时通常可以降级，而不是 panic。
```

不合适的例子是：

```text
必需业务参数；
可选配置项；
大对象和大切片；
数据库连接、事务对象、锁、logger 的可变大状态；
用于控制函数行为的一堆开关。
```

这里有个容易争议的点：logger 能不能放 context？工程里有些团队会放 request-scoped logger，有些团队只放 trace id，再由日志库从 context 提取字段。无论哪种，原则都是别把 context 变成“万能依赖注入容器”。如果 logger 是函数正常工作必需的依赖，构造时注入 struct 更清楚；如果只是请求级日志字段，context 可以承载轻量元数据。

key 也要注意。官方文档建议不要用 string 或其他内置类型做 key，避免不同包之间冲突。常见写法是定义未导出的空 struct 类型：

```go
type requestIDKey struct{}

func WithRequestID(ctx context.Context, id string) context.Context {
    return context.WithValue(ctx, requestIDKey{}, id)
}

func RequestID(ctx context.Context) (string, bool) {
    v, ok := ctx.Value(requestIDKey{}).(string)
    return v, ok
}
```

面试里可以这样答：

```text
context value 不是参数传递机制，也不是依赖注入容器。它适合放 trace id、request id、认证元数据这类小的、请求级、跨 API 边界透传的信息。必需参数应该写在函数签名里，否则依赖关系被藏起来，编译器帮不上忙，只能运行时发现缺失或类型错误。大对象也不应该放 context，因为 context 会沿调用链和派生链被持有，容易延长对象生命周期，增加内存压力。判断标准是：这个值是不是请求级元数据，体积小不小，缺失时是否还能合理处理。
```

一句话：context 负责取消、deadline 和少量请求级元数据，不负责替代函数参数。

## Q017. context deadline 和 timeout 有什么区别？

**回答：**

deadline 是绝对时间点，timeout 是相对时长。`context.WithDeadline(parent, d)` 表示最晚到时间点 `d` 取消；`context.WithTimeout(parent, timeout)` 本质上就是 `WithDeadline(parent, time.Now().Add(timeout))`。

可以用两行代码理解：

```go
ctx1, cancel1 := context.WithDeadline(parent, time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC))
ctx2, cancel2 := context.WithTimeout(parent, 200*time.Millisecond)
```

`ctx1` 关心的是“到 12:00 截止”。`ctx2` 关心的是“从现在开始最多 200ms”。官方文档也直接说明，`WithTimeout` 返回的是 `WithDeadline(parent, time.Now().Add(timeout))`。

两者取消后的错误通常一样：如果是时间到了，`ctx.Err()` 返回 `context.DeadlineExceeded`。如果是手动调用 cancel 或 parent 被取消，返回 `context.Canceled`。

deadline 更适合跨层传递总预算。

比如一次请求从入口到数据库最多 500ms：

```go
deadline := time.Now().Add(500 * time.Millisecond)
ctx, cancel := context.WithDeadline(parent, deadline)
defer cancel()

callA(ctx)
callB(ctx)
callDB(ctx)
```

每一层都能通过 `ctx.Deadline()` 看到同一个绝对截止时间。中间已经花掉 300ms，下游自然只剩 200ms。这样不会每层都重新拿到一个完整 timeout。

timeout 更适合局部操作。

```go
ctx, cancel := context.WithTimeout(parent, 100*time.Millisecond)
defer cancel()
return query(ctx)
```

这表示“这次 query 最多 100ms”。如果 parent 本身已经有更早 deadline，那么 child 不会超过 parent。官方文档说 `WithDeadline` 会把 deadline 调整为不晚于给定时间；如果 parent deadline 更早，语义上等价于 parent。换句话说，child context 不能延长 parent 的生命周期。

常见错误是每一层都写固定 timeout：

```go
func handler(ctx context.Context) error {
    return service(ctx)
}

func service(ctx context.Context) error {
    ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
    defer cancel()
    return repo(ctx)
}

func repo(ctx context.Context) error {
    ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
    defer cancel()
    return db.QueryContext(ctx, query)
}
```

这看起来每层都很谨慎，但如果入口本来就只有 500ms，总预算可能被表达得很乱。更好的方式是入口设总 deadline，下游只在确实需要更小局部预算时收紧。

还有一个实践点：无论 deadline 还是 timeout，都要调用 cancel。

```go
ctx, cancel := context.WithTimeout(parent, time.Second)
defer cancel()
```

即使 1 秒后会自动取消，也应该在操作提前结束时释放 timer 和父 context 对 child 的引用。

再看 `WithDeadlineCause` 和 `WithTimeoutCause`。它们用于设置超时或 deadline 触发时的 cause。普通 `CancelFunc` 不会设置 cause；如果你需要区分“DB budget exceeded”和“上游取消”，可以考虑用 cause，但不要把错误语义搞得太复杂。

面试里可以这样答：

```text
deadline 是绝对截止时间，timeout 是从当前时刻开始计算的相对时长。WithTimeout(parent, d) 基本等价于 WithDeadline(parent, time.Now().Add(d))。跨服务、跨层调用时，deadline 更适合表达总预算，因为下游能看到同一个截止时间；局部单次操作可以用 timeout 表达最多执行多久。无论哪种，child context 都不能晚于 parent 的 deadline，操作结束后都要调用 cancel 来释放资源。
```

一句话：deadline 表达“最晚什么时候结束”，timeout 表达“最多还能跑多久”。

## Q018. defer 的执行时机是什么？

**回答：**

`defer` 的执行时机是：当前函数即将返回时执行，包括正常 `return`、执行到函数末尾返回、或者因为 panic 开始展开栈。多个 defer 按后进先出顺序执行。

语言规范里有几个关键点。

第一，`defer` 语句执行时，会立即求值函数值和参数，但不立即调用函数。

```go
func f() {
    x := 1
    defer fmt.Println(x)
    x = 2
}
```

这段会打印 `1`，因为 `fmt.Println(x)` 的参数在执行 defer 这一行时就已经求值保存了。

如果想让 defer 执行时再读取变量，要用闭包：

```go
func f() {
    x := 1
    defer func() {
        fmt.Println(x)
    }()
    x = 2
}
```

这段会打印 `2`，因为闭包捕获的是变量，真正读取发生在 deferred function 执行时。

第二，多个 defer 逆序执行。

```go
func f() {
    defer fmt.Println(1)
    defer fmt.Println(2)
    defer fmt.Println(3)
}
```

输出是：

```text
3
2
1
```

这个特性很适合资源清理：

```go
mu.Lock()
defer mu.Unlock()

f, err := os.Open(name)
if err != nil {
    return err
}
defer f.Close()
```

第三，defer 在 return 设置返回值之后、函数真正返回给调用方之前执行。命名返回值可以在 defer 中被修改。

```go
func f() (n int) {
    defer func() {
        n++
    }()
    return 1
}
```

这会返回 2。流程是：`return 1` 先把 `n` 设为 1，然后执行 defer，defer 把 `n` 加到 2，最后函数返回。

第四，panic 时也会执行当前 goroutine 调用栈上已经注册的 defer。

```go
func f() {
    defer cleanup()
    panic("bad")
}
```

`cleanup()` 会在 panic 向上传播时执行。这也是 recover 必须写在 defer 里的原因。

第五，defer 属于当前函数，不属于代码块。

```go
for _, name := range names {
    f, _ := os.Open(name)
    defer f.Close()
}
```

这些 `Close` 不会在每轮循环结束时执行，而是在整个函数返回时执行。如果 `names` 很大，就会长时间持有很多文件句柄。更好的写法是把每次循环体提成函数：

```go
for _, name := range names {
    if err := processFile(name); err != nil {
        return err
    }
}

func processFile(name string) error {
    f, err := os.Open(name)
    if err != nil {
        return err
    }
    defer f.Close()
    return process(f)
}
```

第六，`os.Exit` 不会运行 defer。`runtime.Goexit` 会终止当前 goroutine，并运行它的 defer，但不会让整个进程退出。普通 `return`、panic、函数末尾返回都会跑 defer。

第七，defer 的性能已经比早期 Go 好很多，但在极高频热路径里仍然要看情况。普通业务代码里，优先写清楚；在确认 defer 成为瓶颈之前，不要为了省一点成本把资源释放写得容易漏。

面试里可以这样答：

```text
defer 在所在函数即将返回时执行，包括正常 return、走到函数末尾和 panic 展开栈。执行 defer 语句时，函数值和参数会立刻求值并保存；真正调用发生在返回前。多个 defer 按 LIFO 逆序执行。显式 return 时，返回值先被设置，再执行 defer，所以 defer 可以修改命名返回值。defer 不是块级作用域，循环里 defer 会推迟到整个函数返回，这点容易造成资源长时间不释放。
```

一句话：defer 是函数级的延迟调用，注册时求值，返回前逆序执行。

## Q019. panic、recover 和 goroutine 边界有什么关系？

**回答：**

panic 只沿当前 goroutine 的调用栈传播，recover 也只能恢复同一个 goroutine 里的 panic。一个 goroutine 不能用 recover 捕获另一个 goroutine 里的 panic。

先看 panic 的传播。当前函数调用 `panic` 后，普通控制流停止，当前函数已经注册的 defer 会执行；然后 panic 继续向调用者传播，调用者的 defer 继续执行。这个过程一直到当前 goroutine 的最顶层。如果没有 recover，程序会崩溃并打印 panic 信息。

```go
func a() {
    defer fmt.Println("a defer")
    b()
}

func b() {
    defer fmt.Println("b defer")
    panic("boom")
}
```

执行顺序大致是：

```text
b defer
a defer
panic 报告
```

recover 的限制很严格：它只有在 deferred function 中直接调用，且当前 goroutine 正在 panicking 时，才会拿到 panic 值并停止 panic 传播。

正确写法：

```go
func safeCall(fn func()) (err error) {
    defer func() {
        if v := recover(); v != nil {
            err = fmt.Errorf("panic: %v", v)
        }
    }()
    fn()
    return nil
}
```

错误写法：

```go
func bad() {
    if v := recover(); v != nil {
        fmt.Println(v)
    }
}
```

正常执行时直接调用 recover，只会得到 nil。

goroutine 边界是重点。下面这段外层 recover 捕不到子 goroutine 的 panic：

```go
func main() {
    defer func() {
        if v := recover(); v != nil {
            fmt.Println("recovered:", v)
        }
    }()

    go func() {
        panic("worker failed")
    }()

    time.Sleep(time.Second)
}
```

原因是 `go func()` 启动了新的 goroutine。它的调用栈和 main goroutine 的调用栈不同。main 里的 defer 只在 main goroutine 栈展开时运行，不会包住 worker 的栈。

如果要保护 worker，recover 必须写在 worker goroutine 内部：

```go
go func() {
    defer func() {
        if v := recover(); v != nil {
            log.Printf("worker panic: %v", v)
        }
    }()

    doWork()
}()
```

但这也要小心。recover 不是通用错误处理机制。很多 panic 表示程序不变量被破坏，比如数组越界、nil 指针、并发 map 写、类型断言错误。简单 recover 后继续运行，可能把服务留在坏状态。服务框架通常会在请求边界 recover，把 panic 转成 500、记录堆栈，然后让当前请求结束；后台 worker 则要根据情况重启 worker、上报错误或退出进程。

panic 和 error 的边界也要清楚。

```text
可预期失败：
  文件不存在、网络超时、参数校验失败、业务拒绝，用 error。

不可继续的不变量破坏：
  编程错误、状态不可能、严重初始化失败，可以 panic。

goroutine 顶层：
  如果不希望一个 worker panic 直接带崩进程，要在 goroutine 入口 defer recover。
```

还有一个细节：recover 后，panic 点之后的代码不会继续执行。它恢复的是 deferred function 所在函数的返回过程，而不是跳回 panic 那一行后面继续跑。

```go
func f() {
    defer func() {
        recover()
    }()
    panic("x")
    fmt.Println("never")
}
```

`fmt.Println("never")` 不会执行。

面试里可以这样答：

```text
panic 沿当前 goroutine 的调用栈向上传播，传播过程中会执行这个 goroutine 栈上已经注册的 defer。recover 只有在同一个 goroutine 的 deferred function 里直接调用才有效。外层 goroutine 的 recover 捕不到子 goroutine 的 panic，所以如果要保护后台 worker，必须在 worker goroutine 的入口处 defer recover。recover 后也不是从 panic 点继续执行，而是停止 panic 传播，让 deferred function 所在函数返回。工程上通常只在请求边界或 goroutine 顶层 recover，并记录堆栈；普通业务失败应该返回 error。
```

一句话：panic/recover 不跨 goroutine，recover 必须放在发生 panic 的那条 goroutine 的 defer 里。

## Q020. Go map 为什么不是并发安全的？

**回答：**

Go 的普通 map 不是并发安全的，因为 map 的读写和扩容会修改内部结构，多个 goroutine 同时读写会破坏这些结构。官方博客说得很直接：同时读写 map 的行为没有定义；如果多个 goroutine 并发读写 map，必须用同步机制保护，比如 `sync.RWMutex`。

先分清两种情况。

```text
多个 goroutine 只读：
  可以并发读，只要没有任何 goroutine 写入或删除。

有任何 goroutine 写：
  所有读写都需要同步。
```

Go FAQ 也提到，map access 只有在有更新时才 unsafe；所有 goroutine 都只是查找、遍历，不做 assignment 或 delete 时，可以并发访问。问题出现在读写并发、写写并发、遍历时并发写这些场景。

为什么 map 不像 channel 那样自带同步？核心原因是性能和设计取舍。大多数 map 只在单 goroutine 或外部已经同步的上下文里使用。如果每次 map 读写都内置锁，所有普通 map 操作都要付同步成本，但很多场景根本不需要。Go 选择让 map 保持轻量，把并发控制交给调用者。

map 内部不是一个简单数组。它有 bucket、hash、overflow bucket、装载因子、扩容、搬迁等机制。写入可能触发：

```text
更新 bucket 中的 key/value；
分配 overflow bucket；
改变计数；
触发 grow；
在 grow 期间逐步搬迁旧 bucket；
更新 map header 里的状态字段。
```

如果另一个 goroutine 同时读，可能看到中间状态。如果另一个 goroutine 同时写，内部元数据可能互相踩坏。Go runtime 对一些并发 map 写会报：

```text
fatal error: concurrent map writes
fatal error: concurrent map read and map write
```

但不能把这个 fatal error 当成完整保护。它是运行时尽力检测部分错误，不是语言层面保证所有并发错误都能被及时发现。正确性仍然要靠同步。

典型错误：

```go
var m = map[string]int{}

go func() {
    for {
        m["x"]++
    }
}()

go func() {
    for {
        _ = m["x"]
    }
}()
```

这就是 data race，也可能触发 concurrent map read/write fatal error。

常见修复有几种。

第一，用 `sync.Mutex` 或 `sync.RWMutex`。

```go
type SafeMap struct {
    mu sync.RWMutex
    m  map[string]int
}

func (s *SafeMap) Get(k string) (int, bool) {
    s.mu.RLock()
    defer s.mu.RUnlock()
    v, ok := s.m[k]
    return v, ok
}

func (s *SafeMap) Inc(k string) {
    s.mu.Lock()
    defer s.mu.Unlock()
    s.m[k]++
}
```

这是最常见、最清楚的做法。读多写少可以用 `RWMutex`，写多或临界区复杂时普通 `Mutex` 往往更简单。

第二，用单 owner goroutine 加 channel。

```go
type req struct {
    key   string
    delta int
    reply chan int
}

func owner(in <-chan req) {
    m := map[string]int{}
    for r := range in {
        m[r.key] += r.delta
        r.reply <- m[r.key]
    }
}
```

这个模式适合 map 更新本来就是一条事件流，或者需要严格串行化状态机。但如果只是高频读写缓存，owner goroutine 可能成为瓶颈。

第三，用 `sync.Map`。它适合特定场景，不是普通 map 的通用替代。官方 `sync.Map` 文档描述的适用场景包括：entry 写一次读很多次，或者多个 goroutine 操作不相交的 key。它还明确说明 `Range` 不一定对应一致快照。也就是说，如果你需要复杂不变量、强一致遍历、多个字段事务式更新，`sync.Map` 不一定合适。

第四，用 copy-on-write 或 atomic pointer 发布不可变快照。配置表、路由表这类读多写少结构，可以构造新 map 后整体替换：

```go
type Snapshot map[string]Route

var current atomic.Value // stores Snapshot

func Load(k string) (Route, bool) {
    m := current.Load().(Snapshot)
    v, ok := m[k]
    return v, ok
}
```

前提是发布后的 map 不再修改。否则还是并发读写。

还有几个细节要注意。

第一，`len(m)` 也不是同步。写入并发时，读 `len` 同样没有安全保证。

第二，遍历更敏感。`for range m` 期间如果另一个 goroutine 写 map，会出问题。即使单 goroutine 内遍历时删除当前 key 是允许的，并发删除也不是安全的。

第三，map 的 value 如果是指针，保护 map 结构不等于保护指针指向的对象。你用锁保护 `m[k]` 的读写，但拿到 `*Obj` 后多个 goroutine 同时改 `Obj`，仍然需要同步。

第四，只读安全的前提是“没有写”。初始化完后不再修改的 map，可以在多个 goroutine 里直接读。这个场景很常见，比如静态路由表、只读字典。

面试里可以这样答：

```text
普通 Go map 不是并发安全的，因为写入、删除、扩容和 bucket 迁移会修改内部结构；如果另一个 goroutine 同时读或写，可能看到中间状态，甚至破坏 map 元数据。Go 没有给普通 map 每次访问都内置锁，是为了避免所有单线程或外部同步场景都付锁成本。并发访问时要自己加 sync.Mutex/RWMutex，或者用 owner goroutine 串行化，或者在适合的场景用 sync.Map。多个 goroutine 只读同一个初始化完的 map 是可以的；只要有写，就必须同步。
```

一句话：Go map 轻量但不自带同步，有写并发时要把同步边界放在 map 外面。

## Q021. sync.Map 适合什么负载？

**回答：**`sync.Map` 适合“并发访问很多，但共享状态关系比较简单”的负载。官方 `sync` 文档说得很克制：`sync.Map` 是 specialized，不是普通 `map` 加锁的通用替代；它主要为两类场景优化。

第一类是某个 key 只写一次，之后读很多次。典型例子是只增长缓存、按类型缓存反射结果、按连接 ID 保存一次初始化后的元数据：

```go
var cache sync.Map // map[string]*Entry

func loadEntry(key string) (*Entry, error) {
    if v, ok := cache.Load(key); ok {
        return v.(*Entry), nil
    }

    e, err := buildEntry(key)
    if err != nil {
        return nil, err
    }
    actual, loaded := cache.LoadOrStore(key, e)
    if loaded {
        return actual.(*Entry), nil
    }
    return e, nil
}
```

这里的关键不是“读多写少”四个字，而是“同一个 key 的生命周期简单”：要么没有，要么初始化一次，之后读。`LoadOrStore` 可以避免多个 goroutine 同时初始化后相互覆盖。即使有重复构造，也只会发布其中一个值。

第二类是多个 goroutine 读写的是不相交的 key 集合。比如每个 worker 只维护自己分片下的 key，或者每个连接只更新自己的状态：

```go
var sessions sync.Map // map[sessionID]*SessionState

func updateSession(id string, fn func(*SessionState)) {
    v, _ := sessions.LoadOrStore(id, &SessionState{})
    st := v.(*SessionState)
    fn(st)
}
```

这个例子还缺一层保护：`sync.Map` 只保护 map 结构本身，不自动保护 `*SessionState` 指向的对象。如果 `fn` 会修改 `SessionState` 的字段，而同一个 session 可能被多个 goroutine 同时更新，那么 `SessionState` 内部还要有锁、atomic，或者把每个 session 的更新串行化。

不适合 `sync.Map` 的场景也很重要。

第一，value 的强一致不变量很多。比如 map 里多个 key 必须一起更新，或者要维护“总数”“索引”“LRU 链表”等额外结构。`sync.Map` 的单个操作是并发安全的，但它不帮你把多个操作变成事务：

```go
// 这不是原子事务。
m.Store("user:1", u)
m.Store("email:"+u.Email, u.ID)
```

如果中间被别的 goroutine 观察到，可能只看到一半状态。这类场景用 `Mutex` 包住整个不变量更清楚。

第二，需要类型安全。`sync.Map` 的 key 和 value 都是 `any`，每次 `Load` 都要类型断言。对业务代码来说，普通 `map[K]V` 加 `Mutex` 往往更容易维护：

```go
type Store struct {
    mu sync.RWMutex
    m  map[string]*User
}
```

第三，需要一致快照。官方文档明确说 `Range` 不一定对应某个一致时刻的快照。遍历期间某个 key 被并发更新或删除，`Range` 可能看到任意时刻的映射。它保证的是不会重复访问同一个 key，不保证“遍历看到的是同一个版本的全量 map”。

第四，写热点集中在同一批 key 上。如果很多 goroutine 都在反复更新同一个 key，`sync.Map` 不能消除这个热点。此时更常见的做法是分片锁、单 owner goroutine、atomic value，或者重新设计数据流。

面试里可以这样答：

```text
sync.Map 适合两类负载：一个 key 写一次读很多次，比如只增长缓存；或者多个 goroutine 操作互不相交的 key 集合。它是专用结构，不是 map+mutex 的无脑替代。需要类型安全、跨 key 不变量、一致快照、强事务式更新时，我更倾向普通 map 加 Mutex/RWMutex。还要注意 sync.Map 保护的是 map entry 的发布和读取，不保护 value 指向对象的内部字段。
```

一句话：`sync.Map` 适合 key 维度上冲突低、生命周期简单的共享表，不适合复杂状态机和强不变量。

## Q022. sync.Once 的 happens-before 语义是什么？

**回答：**`sync.Once` 的核心不只是“函数只执行一次”，还包括可见性保证。官方 `sync.Once` 文档和 Go 内存模型都说明：`once.Do(f)` 中 `f` 的返回，synchronizes before 任意一次 `once.Do(f)` 的返回。

这句话拆开看有三层意思。

第一，只有一个 goroutine 会真正执行 `f`。其他同时调用 `once.Do(f)` 的 goroutine 会等待，直到执行 `f` 的 goroutine 返回。

第二，`f` 里写入的普通内存，在所有 `once.Do(f)` 返回之后都可见。也就是说，下面这种懒加载是安全发布：

```go
var (
    once sync.Once
    cfg  *Config
    err  error
)

func ConfigOnce() (*Config, error) {
    once.Do(func() {
        cfg, err = loadConfig()
    })
    return cfg, err
}
```

调用方拿到的 `cfg` 不会是“指针已经赋值，但对象字段还没初始化完”的中间状态。`once.Do` 建立了 happens-before 边，保证 `loadConfig` 内部以及 `cfg, err = ...` 的写入对之后返回的调用方可见。

第三，这个保证是由 `Once` 自己提供的，不需要再额外加锁保护初始化结果。常见错误是用一个普通 bool 做双重检查：

```go
var initialized bool
var cfg *Config

func Bad() *Config {
    if !initialized {
        cfg = load()
        initialized = true
    }
    return cfg
}
```

这段代码有 data race，也没有可见性保证。某个 goroutine 看到 `initialized == true`，不代表它一定能看到 `cfg` 指向对象的完整初始化结果。Go 内存模型里也专门提醒过类似的 double-checked locking 写法不可靠。

`sync.Once` 还有几个面试里容易追问的点。

第一，如果 `f` panic 了，`Once` 会认为这次 `Do` 已经完成。后续再调用 `Do` 不会重试原函数。也就是说，`sync.Once` 适合“初始化失败也要固定结果”的场景；如果你想失败后可重试，要自己设计状态机。

第二，不要在 `f` 里递归调用同一个 `Once` 的 `Do`。外层 `Do` 等 `f` 返回，内层 `Do` 又等外层初始化完成，会死锁。

第三，`sync.Once` 不能复制。和 `Mutex`、`WaitGroup` 一样，一旦用过就不能按值拷贝，否则状态会被拆成两份。

面试里可以这样答：

```text
sync.Once 的 happens-before 语义是：被执行的函数 f 返回，先行发生于任何 once.Do(f) 调用返回。所以 f 里的初始化写入，对所有从 Do 返回的 goroutine 都可见。它解决的是一次性初始化和安全发布两个问题，不只是防止函数重复执行。需要注意 f panic 后 Once 也会认为已经执行过，后续不会自动重试。
```

一句话：`sync.Once` 是“一次执行 + 安全发布”的组合，happens-before 保证让懒初始化结果能被其他 goroutine 正确观察。

## Q023. WaitGroup 常见误用有哪些？

**回答：**`sync.WaitGroup` 是计数信号量，用来等一组任务结束。它的 API 很小，但误用很多，原因是 `Add`、启动 goroutine、`Done`、`Wait` 之间的顺序必须清楚。

第一类误用是 `Add` 放在 goroutine 里面。

```go
var wg sync.WaitGroup

for _, job := range jobs {
    go func(job Job) {
        wg.Add(1) // 错：可能比 Wait 更晚执行
        defer wg.Done()
        handle(job)
    }(job)
}

wg.Wait()
```

如果主 goroutine 先执行到 `Wait`，此时计数还是 0，`Wait` 会直接返回，任务还没开始就被认为完成了。正确写法是先 `Add`，再启动 goroutine：

```go
var wg sync.WaitGroup

for _, job := range jobs {
    wg.Add(1)
    go func(job Job) {
        defer wg.Done()
        handle(job)
    }(job)
}

wg.Wait()
```

如果使用的 Go 版本有 `WaitGroup.Go`，更推荐把“增加计数、启动 goroutine、返回时 Done”合在一起：

```go
var wg sync.WaitGroup

for _, job := range jobs {
    job := job
    wg.Go(func() {
        handle(job)
    })
}

wg.Wait()
```

第二类误用是 `Done` 漏掉。函数里有早返回、错误分支、panic 风险时，`Done` 很容易不执行。通常写成 `defer wg.Done()`，放在 goroutine 函数的第一行：

```go
wg.Add(1)
go func() {
    defer wg.Done()
    if err := step1(); err != nil {
        return
    }
    step2()
}()
```

第三类误用是 `Done` 多调用，导致计数变成负数。`Done` 等价于 `Add(-1)`，计数为负会 panic。常见原因是多个分支都调用 `Done`，外层又 `defer Done`：

```go
go func() {
    defer wg.Done()
    if bad {
        wg.Done() // 错：重复 Done
        return
    }
}()
```

第四类误用是复制 `WaitGroup`。`WaitGroup` 一旦使用就不能复制。把它作为值传参、嵌在结构体里按值复制、放进 slice 后扩容拷贝，都可能把内部计数和等待队列复制出不一致状态。传参时用指针：

```go
func run(wg *sync.WaitGroup) {
    defer wg.Done()
}
```

第五类误用是复用时顺序不清。官方文档说，如果一个 `WaitGroup` 被复用来等待多批独立事件，新一批 `Add` 必须发生在上一批所有 `Wait` 返回之后。否则某个 `Wait` 正在等上一批任务，另一边又给同一个 `WaitGroup` 加新任务，语义会混在一起。

第六类误用是拿 `WaitGroup` 当错误收集和取消机制。`WaitGroup` 只会等，不会传播错误，不会取消其他 goroutine，也不会限制并发数。下面这种写法有共享变量 race 的风险：

```go
var err error

for _, job := range jobs {
    wg.Add(1)
    go func(job Job) {
        defer wg.Done()
        if e := handle(job); e != nil {
            err = e // 错：多个 goroutine 并发写 err
        }
    }(job)
}
wg.Wait()
```

这种场景更适合 `errgroup`，或者自己加锁、channel 收集错误。

面试里可以这样答：

```text
WaitGroup 常见坑是 Add 顺序错误、Done 漏掉或多调、WaitGroup 被复制、复用时上一轮 Wait 没结束就开始下一轮 Add，以及把 WaitGroup 当成错误传播或取消机制。最基本的规则是：手写 Add/Done 时，正数 Add 要在启动 goroutine 之前；goroutine 入口处 defer Done；WaitGroup 用过后不要复制；要错误和取消就考虑 errgroup。
```

一句话：`WaitGroup` 只负责“等计数归零”，其他顺序、错误、取消和共享变量同步都要你自己处理。

## Q024. errgroup 相比 WaitGroup 解决了什么问题？

**回答：**`errgroup` 可以理解为“带错误传播和可选取消的 WaitGroup”。它在 `golang.org/x/sync/errgroup` 里，不是标准库 `sync` 包的一部分，但在 Go 服务端代码里非常常见。

`WaitGroup` 只解决一件事：等一组任务结束。它不关心任务成功还是失败。`errgroup.Group` 在等待之外补了几个工程上最常见的需求。

第一，收集第一个非 nil 错误。`Group.Go` 接收的是 `func() error`，`Wait` 会等所有 goroutine 返回，然后返回第一个非 nil error：

```go
var g errgroup.Group

for _, url := range urls {
    url := url
    g.Go(func() error {
        return fetch(url)
    })
}

if err := g.Wait(); err != nil {
    return err
}
```

这避免了多个 goroutine 并发写同一个 `err` 变量，也避免了手写错误 channel 后忘记关闭、阻塞、只读一部分等问题。

第二，配合 `WithContext` 做失败取消。`errgroup.WithContext(parent)` 返回 `(*Group, context.Context)`。任意一个任务返回非 nil error 后，派生 context 会被取消；其他任务如果尊重这个 context，就可以尽快停止：

```go
g, ctx := errgroup.WithContext(parent)

for _, shard := range shards {
    shard := shard
    g.Go(func() error {
        return queryShard(ctx, shard)
    })
}

return g.Wait()
```

这对 fan-out RPC、并行查询、pipeline 很有用。一个分片失败后，继续跑其他分片可能只是浪费资源。`WaitGroup` 本身没有这个能力，你需要自己维护 `context.WithCancel` 和错误通道。

第三，限制并发。`errgroup.Group` 有 `SetLimit` 和 `TryGo`，可以避免一次性启动过多 goroutine：

```go
g, ctx := errgroup.WithContext(parent)
g.SetLimit(16)

for _, file := range files {
    file := file
    g.Go(func() error {
        return processFile(ctx, file)
    })
}

return g.Wait()
```

这比“手写 buffered channel 当 semaphore + WaitGroup + error channel”更集中，边界更少。

但 `errgroup` 也不是万能的。

第一，它默认只返回第一个错误。如果你要收集所有错误，需要自己聚合，或者用 `errors.Join` 之类的结构。

第二，它不会自动杀掉 goroutine。取消只是关闭 context 的 `Done`，任务函数必须自己检查 `ctx.Done()`，或者调用支持 context 的 I/O、RPC、数据库 API。

第三，它不自动 recover panic。任务里 panic 仍然会让程序按 panic 规则走，除非你在任务内部处理。

面试里可以这样答：

```text
WaitGroup 只管等待，不管错误和取消。errgroup 在等待的基础上，让每个任务返回 error，Wait 返回第一个非 nil error；WithContext 可以在第一个错误出现时取消派生 context；SetLimit 可以限制并发数。所以 fan-out RPC、并行文件处理、pipeline 这类任务，用 errgroup 通常比 WaitGroup+error channel+cancel 手写组合更稳。
```

一句话：`errgroup` 解决的是“等一组 goroutine，并把失败和取消作为同一个生命周期来管理”。

## Q025. race detector 能发现哪些类型的问题？

**回答：**Go 的 race detector 能发现运行时实际发生的 data race。Go 内存模型里 data race 的定义很明确：同一个内存位置上，一个写操作和另一个读或写操作并发发生，并且这些访问没有被 happens-before 排序，且不是 `sync/atomic` 提供的原子访问。

使用方式通常是：

```bash
go test -race ./...
go run -race ./cmd/server
go build -race ./cmd/server
```

它发现的不是“所有并发 bug”，而是具体的非同步共享内存访问冲突。常见类型包括下面几类。

第一，普通变量并发读写：

```go
var closed bool

go func() {
    closed = true
}()

if closed {
    cleanup()
}
```

`bool`、`int`、指针这些看起来很小的变量也会 race。Go race detector 文档里也专门提醒，原始类型变量的 data race 一样会因为非原子访问、编译器优化、重排序等原因产生难排查问题。

第二，多个 goroutine 并发写同一个错误变量或结果变量：

```go
var err error

go func() { err = doA() }()
go func() { err = doB() }()
```

这类代码经常出现在手写 `WaitGroup` 时。看起来只是“记录一个错误”，但实际上是并发写同一变量。

第三，闭包捕获循环变量导致的共享变量 race。Go 官方 race detector 文档里有经典例子：goroutine 读取循环变量 `i`，主 goroutine 同时递增 `i`。Go 1.22 改了 loop variable 的默认语义，但旧模块、显式复用变量、循环外变量仍可能出现类似问题。

第四，map 并发读写：

```go
var m = map[string]int{}

go func() {
    m["x"] = 1
}()

_ = m["x"]
```

普通 map 不并发安全。race detector 能报告共享内存访问冲突；运行时也可能直接报 `fatal error: concurrent map read and map write`，但不能依赖运行时报错覆盖所有情况。

第五，slice、struct、interface 等复合对象的内部字段 race。比如一个 goroutine 修改 `s[0]`，另一个 goroutine 读 `s[0]`；或者一个 goroutine 写结构体字段，另一个 goroutine 读同一字段。

race detector 的报告通常会给出两边冲突访问的栈，以及 goroutine 创建栈。定位时不要只看报错行，要看“哪个对象被两个 goroutine 共享了”。修复方式一般是三类：用 mutex/channel 建立顺序，用 atomic 保护单个数值状态，或者改变 ownership，让同一份可变数据只被一个 goroutine 写。

面试里可以这样答：

```text
Go race detector 能发现实际执行路径上的 data race，也就是同一内存位置被并发读写或写写，并且没有 mutex、channel、atomic 等同步建立 happens-before。常见报告包括共享变量、错误变量、loop variable、map、slice、struct 字段的并发访问。它给的是运行时检测结果，所以覆盖率取决于测试或运行流量是否走到了那条路径。
```

一句话：race detector 是动态 data race 检测器，擅长抓“没同步的共享内存访问”，不等于并发正确性证明。

## Q026. race detector 发现不了哪些逻辑并发 bug？

**回答：**race detector 的边界要说清楚：它检测 data race，不检测所有 race condition。一个程序没有 data race，也可能有严重并发 bug。

第一，检测不到没有执行到的路径。`-race` 是运行时动态检测，只有测试或实际运行覆盖到冲突访问，它才有机会报告。没有覆盖的分支、错误路径、超时路径、低概率 interleaving，都可能漏掉。

第二，检测不到所有死锁和 goroutine leak。比如两个 goroutine 用 channel 互相等待，或者生产者退出后消费者永远等不到关闭，这可能没有共享内存 data race：

```go
func leak(ch <-chan int) {
    go func() {
        for v := range ch {
            _ = v
        }
    }()
}
```

如果没人关闭 `ch`，这个 goroutine 会一直阻塞。race detector 不会把它当 data race。

第三，检测不到使用 atomic 后的逻辑错误。atomic 可以消除 data race，但不能自动保证业务状态机正确：

```go
if atomic.LoadInt64(&balance) >= cost {
    atomic.AddInt64(&balance, -cost)
}
```

每次访问都是 atomic，不一定有 data race，但两个 goroutine 都可能通过检查，然后把余额扣成负数。问题在于“检查和扣减”不是一个原子事务。

第四，检测不到 channel 协议错误。比如谁负责 close、是否可能重复 close、是否可能向已关闭 channel send、是否可能所有 sender 都退出但 receiver 还在等。这些是协议和 ownership 问题，race detector 不保证能发现。

第五，检测不到顺序依赖的业务 race condition。比如两个请求都合法、也都加了锁，但业务要求“先冻结账户再扣款”，实际顺序变成“先扣款再冻结”。这不是 data race，而是流程编排错误。

第六，检测不到性能型并发问题。锁竞争太高、调度延迟、GC assist 导致尾延迟变差、goroutine 数量暴涨、channel buffer 造成排队，这些要靠 pprof、trace、指标和压测看。

第七，不能证明“没有 race”。一次 `go test -race ./...` 通过，只能说明这次运行覆盖到的路径没有报告 data race。它不是形式化证明。

面试里可以这样答：

```text
race detector 发现的是实际运行路径上的 data race。它发现不了没跑到的路径，也发现不了很多没有 data race 的并发逻辑 bug，比如死锁、goroutine leak、错误的 channel close 协议、atomic 组合操作不原子、业务顺序错误、锁竞争和尾延迟问题。所以 -race 是必要工具，但还要配合测试设计、代码审查、pprof、trace 和运行时指标。
```

一句话：`-race` 能抓共享内存同步错误，抓不了所有并发协议和业务时序错误。

## Q027. data race 和 race condition 的区别是什么？

**回答：**data race 是内存模型里的严格概念，race condition 是更宽的工程概念。

data race 指的是同一内存位置上，并发发生的读写或写写访问之间没有 happens-before 顺序，并且至少一个访问不是同步操作。Go 内存模型说，data-race-free 的程序可以按顺序一致的方式理解，也就是 DRF-SC。换句话说，避免 data race 是 Go 并发程序的底线。

race condition 是指程序结果依赖不可控的执行顺序，而这个顺序没有被设计成可靠协议。它可能包含 data race，也可能完全没有 data race。

有 data race 的例子：

```go
var n int

go func() { n++ }()
go func() { n++ }()
```

`n++` 不是原子操作，两个 goroutine 并发读写同一变量，没有同步。这既是 data race，也是 race condition。

没有 data race 但仍有 race condition 的例子：

```go
var balance int64 = 100

func buy(cost int64) bool {
    if atomic.LoadInt64(&balance) < cost {
        return false
    }
    atomic.AddInt64(&balance, -cost)
    return true
}
```

这里每次访问 `balance` 都用 atomic，race detector 通常不会报 data race。但“检查余额”和“扣款”之间没有形成一个不可分割的业务动作。两个并发购买都可能看到余额足够，然后都扣款成功。问题是竞态条件，不是 data race。

再看一个 channel 例子：

```go
select {
case <-ctx.Done():
    return ctx.Err()
case result := <-resultCh:
    return handle(result)
}
```

如果 `ctx.Done()` 和 `resultCh` 同时 ready，`select` 会选一个 ready case。没有共享内存 data race，但业务上你要明确这种顺序是否可接受。比如“结果到了就必须优先返回成功”，那就不能把这个 select 当成强优先级逻辑。

面试里可以这样答：

```text
data race 是 Go 内存模型定义的低层错误：同一内存位置有并发读写或写写，且没有 happens-before 和 atomic 保护。race condition 更宽，指结果依赖不可控时序。很多 data race 都是 race condition，但 race condition 不一定有 data race，比如全部用 atomic 或 channel 也可能把业务状态机写错。
```

一句话：data race 是内存访问层面的同步错误，race condition 是程序语义层面的时序错误。

## Q028. slice append 在并发下有什么风险？

**回答：**`append` 的风险来自 slice 的两个事实：slice header 里有指向底层数组的指针、长度和容量；`append` 可能复用原底层数组，也可能扩容分配新数组。

第一，如果多个 goroutine 对同一个 slice 变量 append，肯定不安全：

```go
var xs []int

go func() {
    xs = append(xs, 1)
}()
go func() {
    xs = append(xs, 2)
}()
```

这会同时读写 slice header 的 len、cap、ptr，也可能同时写底层数组。race detector 会报共享变量访问冲突；即使没报，结果也可能丢元素、覆盖元素，或者 header 变成不一致状态。

第二，即使不是同一个 slice 变量，只要共享底层数组，也可能 race：

```go
base := make([]int, 0, 10)
a := base
b := base

go func() {
    a = append(a, 1)
}()
go func() {
    b = append(b, 2)
}()
```

`a` 和 `b` 是两个 slice header，但都指向同一个底层数组，且容量足够时 `append` 会直接写 `base` 的数组位置。两个 goroutine 都可能写 index 0，产生 data race 和覆盖。

第三，是否扩容会让 bug 更隐蔽。如果容量不够，某次 append 会分配新数组，两个 goroutine 可能暂时互不影响；如果容量够，又会共享同一数组。代码表现会随着初始容量、元素大小、运行时版本、并发时机变化。

第四，并发写不同下标不一定错，但前提很严格：不能并发修改 slice header，且每个 goroutine 只写自己独占的元素位置。例如：

```go
out := make([]Result, len(jobs))

var wg sync.WaitGroup
for i, job := range jobs {
    i, job := i, job
    wg.Add(1)
    go func() {
        defer wg.Done()
        out[i] = handle(job) // 每个 i 唯一时，这个元素位置是独占的
    }()
}
wg.Wait()
```

这里没有并发 `append`，slice 长度提前固定，每个 goroutine 写不同 index。注意 `handle` 返回的对象本身如果共享可变内部状态，还要另算。

常见修复方式有几种。

第一，用锁保护 append：

```go
var (
    mu sync.Mutex
    xs []int
)

func add(v int) {
    mu.Lock()
    xs = append(xs, v)
    mu.Unlock()
}
```

第二，每个 goroutine 写自己的局部 slice，最后单线程合并：

```go
parts := make([][]int, workers)

// 每个 worker 只写 parts[id]
// Wait 后由一个 goroutine append 合并
```

第三，用 channel 把结果交给一个 collector goroutine，由它独占 append：

```go
results := make(chan int)

go func() {
    for v := range results {
        xs = append(xs, v)
    }
}()
```

面试里可以这样答：

```text
slice append 并发风险在于 append 会读写 slice header，也可能复用底层数组写元素。多个 goroutine 对同一个 slice append 是 data race；多个不同 slice header 如果共享底层数组，也可能写到同一块数组。安全做法是加锁、预分配固定长度后按独占 index 写、每个 goroutine 收集局部结果再合并，或者用一个 collector 独占 append。
```

一句话：并发 append 的问题不是 append 这个函数特殊，而是 slice header 和底层数组 ownership 没有被同步管理。

## Q029. for range 中闭包捕获变量的坑是什么？

**回答：**这个坑的经典版本发生在 Go 1.22 之前，或者发生在仍使用旧语义的模块中：`for range` 的循环变量是每轮复用同一个变量，闭包捕获的是变量本身，不是当轮的值。

经典错误：

```go
for _, v := range []string{"a", "b", "c"} {
    go func() {
        fmt.Println(v)
    }()
}
```

在旧语义下，三个 goroutine 捕获的是同一个 `v`。等 goroutine 真正运行时，循环可能已经结束，`v` 已经变成最后一轮的值，所以常见输出是三次 `c`。Go 官方 blog 和 race detector 文档都用过类似例子。

修复方式是在循环体里创建每轮自己的变量，或者把值作为参数传进去：

```go
for _, v := range values {
    v := v
    go func() {
        fmt.Println(v)
    }()
}
```

或者：

```go
for _, v := range values {
    go func(v string) {
        fmt.Println(v)
    }(v)
}
```

这个问题不只出现在 goroutine 里。只要闭包比当前迭代活得更久，就可能出错：

```go
var fns []func()

for i := 0; i < 3; i++ {
    fns = append(fns, func() {
        fmt.Println(i)
    })
}

for _, fn := range fns {
    fn()
}
```

map range 里还要特别注意同时捕获 key 和 value：

```go
for k, v := range m {
    k, v := k, v
    go func() {
        use(k, v)
    }()
}
```

历史上还有一个更隐蔽的变体：取循环变量地址。

```go
var ptrs []*Item
for _, item := range items {
    ptrs = append(ptrs, &item) // 旧语义下多个地址指向同一个循环变量
}
```

很多人只复制 key，不复制 value，结果 value 的字段地址被后续函数保存，仍然指向复用变量。这类 bug 比简单的 goroutine 打印更难看出来。

Go 1.22 后这个问题大幅缓解，但面试里不能只说“已经没了”。原因有三点：旧模块可能还在旧语义下；循环外变量仍会被复用；显式把变量声明在循环外再赋值，仍然是同一个变量。

```go
var v string
for _, v = range values {
    go func() {
        fmt.Println(v) // v 是循环外变量，仍然共享
    }()
}
```

面试里可以这样答：

```text
老版本 Go 里 for range 的循环变量按循环复用，闭包捕获的是变量，不是当轮值，所以 goroutine 或延迟执行的函数可能都看到最后一轮值。修复方式是 v := v 创建每轮副本，或把 v 作为闭包参数传入。Go 1.22 改成每次迭代新变量，但旧模块、循环外变量、显式复用变量仍要小心。
```

一句话：闭包捕获的是变量生命周期，不是你脑子里那一刻的值；Go 1.22 改了常见循环变量语义，但不要把所有捕获问题都当成自动消失。

## Q030. Go 1.22 后 loop variable 语义有什么变化？

**回答：**Go 1.22 改了 `for` 循环变量的作用域：以前循环变量通常是整个循环复用一个变量；Go 1.22 起，每次迭代会创建新的变量，用来避免闭包意外共享同一个 loop variable。

Go 1.22 release notes 的表述是：过去由 `for` 循环声明的变量创建一次，每次迭代更新；Go 1.22 中每次迭代都会创建新变量，避免 accidental sharing bugs。

看这个例子：

```go
values := []string{"a", "b", "c"}

for _, v := range values {
    go func() {
        fmt.Println(v)
    }()
}
```

旧语义下，闭包捕获同一个 `v`，经常打印 `c c c`。新语义下，每轮都有自己的 `v`，三个 goroutine 会分别捕获各自那轮的变量，所以会打印 `a`、`b`、`c`，顺序仍由调度决定。

这项变化有几个边界要讲清楚。

第一，新语义按模块逐步启用。Go 官方 loopvar 文章说明，为了兼容旧代码，新语义只应用在 `go.mod` 里声明 `go 1.22` 或更高版本的包中；也可以通过 build tag 做更细粒度控制。也就是说，Go 编译器版本升级了，不代表所有旧模块立刻改变语义，关键还要看模块的 `go` 版本。

第二，它覆盖的不只是 `range`。Go 1.22 release notes 说的是 `for` loops，两类常见写法都受影响：

```go
for _, v := range values {
    // 每轮新的 v
}

for i := 0; i < n; i++ {
    // 每轮新的 i
}
```

第三，改的是循环声明的变量，不是所有变量。下面这种仍然共享，因为 `v` 声明在循环外：

```go
var v string
for _, v = range values {
    go func() {
        fmt.Println(v)
    }()
}
```

第四，新语义可能暴露旧测试里的假通过。官方文章举过 `t.Parallel` 的例子：旧语义下所有子测试都捕获最后一个 case，所以测试可能“看起来通过”；新语义后每个子测试拿到自己的 case，错误才会显现。

第五，迁移时不能机械删除所有 `v := v`。在 Go 1.22 新语义下，很多 `v := v` 已经不需要；但如果你的代码要兼容旧模块或旧 Go 版本，保留它通常没问题。真正要关注的是语义，而不是追求形式统一。

面试里可以这样答：

```text
Go 1.22 把 for 循环变量从 per-loop 语义改成 per-iteration 语义，每次迭代都会有新的变量，所以闭包捕获 range 变量或 for 的 i 时，不再默认共享同一个变量。这主要修复了 goroutine、defer、t.Parallel、闭包列表里的经典坑。边界是：新语义按 go.mod 的 go 1.22 及以上逐步启用；循环外声明再赋值的变量仍然会共享。
```

一句话：Go 1.22 让循环变量更接近直觉，但捕获循环外变量和旧模块兼容性仍要自己判断。

## Q031. interface nil 和 concrete nil 的区别是什么？

**回答：**Go 里 interface value 不是单纯一个指针。官方 FAQ 解释过，接口值在实现上可以理解为一对东西：动态类型 `T` 和动态值 `V`。一个接口值只有在动态类型和动态值都为空时，才等于 `nil`。

典型例子：

```go
type MyError struct {
    msg string
}

func (e *MyError) Error() string {
    return e.msg
}

func bad() error {
    var e *MyError = nil
    return e
}

func main() {
    err := bad()
    fmt.Println(err == nil) // false
}
```

`e` 这个 concrete value 是一个 nil `*MyError`。但当它被赋给 `error` 接口时，接口里装的是：

```text
(dynamic type = *MyError, dynamic value = nil)
```

动态类型不为空，所以 `err != nil`。

这会导致几个常见坑。

第一，返回 error 时不要返回 typed nil。错误写法：

```go
func load() error {
    var err *MyError
    if ok {
        return nil
    }
    return err // err 可能是 nil 指针，但作为 error 返回后不是 nil interface
}
```

正确做法是没有错误时显式 `return nil`，有错误时才构造非 nil 错误：

```go
func load() error {
    if ok {
        return nil
    }
    return &MyError{msg: "failed"}
}
```

第二，接口里装了 nil 指针，调用方法可能 panic，也可能不 panic，取决于方法实现：

```go
func (e *MyError) Error() string {
    if e == nil {
        return "<nil MyError>"
    }
    return e.msg
}
```

方法可以接收 nil receiver，但如果直接访问字段，就会 panic。不要把“接口不等于 nil”和“里面的对象可安全使用”混为一谈。

第三，`fmt.Println(err)` 打印 `<nil>` 不代表 `err == nil`。格式化时可能调用 `Error` 或处理 nil 指针，显示结果会迷惑人。判断错误一定看 `err == nil`。

第四，slice、map、chan、func 这类 nil 值放进 `any` 后也类似：

```go
var s []int = nil
var x any = s
fmt.Println(x == nil) // false
```

接口里有动态类型 `[]int`，动态值是 nil slice，所以接口本身不是 nil。

面试里可以这样答：

```text
interface nil 是指接口的动态类型和动态值都为空；concrete nil 是具体类型的 nil 值，比如 nil *MyError、nil []int。把 concrete nil 放进接口后，接口有了动态类型，所以接口值不等于 nil。error 返回里最常见的坑是返回 typed nil，导致调用方判断 err != nil。
```

一句话：接口是否为 nil 看的是 `(type, value)` 这一对，只有两者都空才是 nil interface。

## Q032. error wrapping 的设计目的是什么？

**回答：**error wrapping 的目的，是在给错误增加上下文的同时，保留底层错误的结构化身份，让程序可以可靠判断原因，而不是解析字符串。

Go 1.13 之前，常见写法是：

```go
if err != nil {
    return fmt.Errorf("read config %s: %v", path, err)
}
```

这能给人看，但丢掉了底层错误的可检查身份。调用方如果想判断是不是 `os.ErrNotExist`，只能看字符串，或者要求中间层返回自定义类型。

Go 1.13 引入 `Unwrap` 约定、`errors.Is`、`errors.As` 和 `fmt.Errorf` 的 `%w`。使用 `%w` 后，外层错误带着上下文，底层错误还在链上：

```go
if err != nil {
    return fmt.Errorf("read config %s: %w", path, err)
}
```

调用方可以这样判断：

```go
if errors.Is(err, os.ErrNotExist) {
    useDefaultConfig()
}
```

这比字符串匹配稳得多，因为错误文本可以改，语言可以本地化，路径和参数也会变；错误身份则可以作为 API 契约。

error wrapping 解决了几件事。

第一，保留调用链上下文。日志里看到 `open /etc/app/config.yaml: permission denied` 比只看到 `permission denied` 有用得多。

第二，保留机器可判断的 cause。外层可以写“读取配置失败”，底层仍然是 `fs.ErrNotExist`、`context.Canceled`、`context.DeadlineExceeded` 或自定义错误类型。

第三，支持分层抽象。底层包返回具体错误，上层包决定是否暴露它。Go 官方 errors blog 特别提醒：用 `%w` 包装一个底层错误，就等于把它变成你 API 的一部分。以后调用方可能依赖 `errors.Is(err, sql.ErrNoRows)`。如果你不想承诺底层实现，就用 `%v` 或转换成自己的错误类型。

第四，支持错误链和错误树。现代 `errors` 包支持 `Unwrap() error`，也支持 `Unwrap() []error`，`errors.Is` 和 `errors.As` 会遍历错误树。这让 `errors.Join` 这类组合错误也能被统一检查。

面试里可以这样答：

```text
error wrapping 的设计目的，是让错误既有人可读的上下文，又保留程序可判断的底层原因。fmt.Errorf("%w") 会让外层错误实现 Unwrap，errors.Is/As 可以沿着错误链检查 sentinel 或具体类型。是否 wrap 是 API 设计决定：wrap 代表你愿意把底层错误暴露给调用方；不想暴露实现细节时不要用 %w。
```

一句话：wrapping 不是为了把字符串拼长，而是为了在分层错误里保留可检查的因果关系。

## Q033. errors.Is 和 errors.As 分别解决什么问题？

**回答：**`errors.Is` 解决“这个错误是不是某个错误值或错误类别”的问题；`errors.As` 解决“这个错误链里有没有某种具体错误类型”的问题。

`errors.Is(err, target)` 更像升级版的 `err == target`。它会检查 `err` 本身以及它包裹的错误链或错误树：

```go
if errors.Is(err, fs.ErrNotExist) {
    return createDefault()
}

if errors.Is(err, context.Canceled) {
    return nil
}
```

`Is` 适合 sentinel error、标准错误类别、业务错误类别。例如 `fs.ErrNotExist`、`context.Canceled`、`context.DeadlineExceeded`、`sql.ErrNoRows`。如果错误类型自己实现了 `Is(error) bool`，还可以自定义匹配逻辑。

`errors.As(err, &target)` 更像升级版类型断言。它会沿着错误链找第一个能赋给目标类型的错误，并把它放进 target：

```go
var pathErr *fs.PathError
if errors.As(err, &pathErr) {
    log.Printf("op=%s path=%s", pathErr.Op, pathErr.Path)
}
```

`As` 适合需要读取结构化字段的场景。比如你不只是想知道“是不是路径错误”，还要拿到 `Op`、`Path`、底层 `Err`。

两者的选择可以这样判断。

第一，如果你只关心“是否属于某类原因”，用 `Is`：

```go
switch {
case errors.Is(err, context.Canceled):
    return nil
case errors.Is(err, context.DeadlineExceeded):
    return retryLater()
}
```

第二，如果你要拿出错误里的字段，用 `As`：

```go
var netErr net.Error
if errors.As(err, &netErr) && netErr.Timeout() {
    return retry()
}
```

第三，不要用字符串判断错误：

```go
// 不稳
if strings.Contains(err.Error(), "not found") {
    ...
}
```

第四，`As` 的第二个参数要传“目标变量的指针”。如果目标类型本身是指针类型，比如 `*fs.PathError`，就要写 `&pathErr`，看起来像指针的指针，这是正常的。

第五，`Is` 和 `As` 会穿透 `%w`、`Unwrap` 和多错误树；但它们只能看到被包装时暴露出来的错误。如果中间层用 `%v` 重新格式化，链就断了。

面试里可以这样答：

```text
errors.Is 用来判断错误链里是否有某个目标错误值或类别，替代直接 ==；errors.As 用来判断错误链里是否有某个类型，并把具体错误取出来，替代单层类型断言。Is 关心身份和类别，As 关心类型和字段。它们依赖 wrapping 链，如果中间层不用 %w 或 Unwrap，调用方就检查不到底层错误。
```

一句话：`Is` 问“是不是这个原因”，`As` 问“有没有这种类型并让我拿到它”。

## Q034. defer 在高频路径中是否有成本？

**回答：**有成本，但现代 Go 里大多数普通 `defer` 的成本已经很低。面试里不要简单说“defer 很慢，别用”，这个说法过时且容易误导。

Go 1.14 release notes 明确提到，大多数 `defer` 的性能被优化到相比直接调用几乎没有额外开销，使得 `defer` 可以用于性能关键代码而不用过分担心。编译器会对很多简单 defer 做 open-coded defer 优化。

所以在普通业务路径、错误处理路径、锁释放、资源关闭里，优先写清楚：

```go
mu.Lock()
defer mu.Unlock()

f, err := os.Open(name)
if err != nil {
    return err
}
defer f.Close()
```

这些地方 `defer` 带来的正确性价值很高：防止早返回漏释放，防止复杂分支下遗漏 unlock/close。

但高频路径里仍要理解边界。

第一，`defer` 是在当前函数返回时执行，不是在当前代码块结束时执行。循环里 defer 会累积到函数返回：

```go
func bad(files []string) error {
    for _, name := range files {
        f, err := os.Open(name)
        if err != nil {
            return err
        }
        defer f.Close() // 直到 bad 返回才关闭，循环很大时会持有很多 fd
    }
    return nil
}
```

应该把循环体抽成函数，或者显式关闭：

```go
func processOne(name string) error {
    f, err := os.Open(name)
    if err != nil {
        return err
    }
    defer f.Close()
    return process(f)
}
```

第二，不是所有 defer 都能被编译器优化到同样低成本。复杂控制流、循环中的 defer、动态数量 defer、涉及 recover 的场景，开销和语义都更复杂。

第三，如果函数在每个请求里执行百万次，或者在编解码、压缩、哈希、网络包处理这种超热路径上，还是要用 benchmark 和 pprof 说话。可以比较：

```go
func withDefer(mu *sync.Mutex) {
    mu.Lock()
    defer mu.Unlock()
    work()
}

func withoutDefer(mu *sync.Mutex) {
    mu.Lock()
    work()
    mu.Unlock()
}
```

如果测出来 defer 真是热点，再考虑手动释放。不要凭经验把所有 `defer` 删掉，删错一次锁释放或文件关闭，代价通常比那点性能更高。

面试里可以这样答：

```text
defer 有语义成本，但 Go 1.14 之后大多数普通 defer 已经被优化得很低，正常代码应优先用 defer 保证 unlock、close、recover 等清理逻辑正确。需要小心的是循环里的 defer 会推迟到函数返回才执行，可能积累资源；超高频热路径也要用 benchmark 和 pprof 验证后再决定是否手动释放。
```

一句话：`defer` 默认是正确性工具，只有在证据显示它处于热路径时才值得为性能改写。

## Q035. Go GC 对 latency-sensitive 服务有什么影响？

**回答：**Go 的 GC 是并发标记清扫为主，目标是降低 stop-the-world 时间，但它仍然会影响 latency-sensitive 服务，尤其是尾延迟。影响不只来自 STW pause，还来自 GC CPU、写屏障、标记辅助、堆大小和内存带宽。

第一，GC 仍有 stop-the-world 阶段。现代 Go 的 STW 通常很短，但 latency-sensitive 服务关心的是 p99、p999，而不是平均值。如果服务本身目标延迟很低，比如几毫秒甚至亚毫秒，几十到几百微秒的暂停也可能进入尾延迟预算。

第二，高分配率会增加 GC 压力。每秒分配越多，堆增长越快，GC 周期越频繁。GC 期间 mutator 也就是业务 goroutine 可能要做 mark assist，等于在请求路径上帮 GC 干活。你看到的延迟尖刺不一定表现为长 STW pause，也可能表现为请求 goroutine 被 assist 拖慢。

第三，live heap 越大，标记成本越高。GC 主要关心“仍然活着、可达”的对象。临时对象分配很多但很快死亡，会增加分配速率；长期存活对象很多，会增加每轮扫描和标记成本。缓存过大、全局 map 保留大量对象、slice 小切片引用大底层数组，都可能扩大 live heap。

第四，`GOGC` 是 CPU 和内存之间的权衡。默认 `GOGC=100` 大致表示新分配堆增长到 live heap 的某个比例后触发下一轮 GC。调高 `GOGC` 通常减少 GC 频率和 CPU，但占更多内存；调低 `GOGC` 通常减少堆峰值，但增加 GC 频率和 CPU。

第五，`GOMEMLIMIT` 或 `debug.SetMemoryLimit` 可以给运行时软内存限制。它对容器服务很有用，但限制设得太紧会导致 GC 频繁运行，严重时接近 thrashing。Go GC guide 也提醒，内存限制是强大的工具，但误配会把 OOM 风险换成严重变慢。

第六，GC 和调度、容器 CPU 配额也有关。GC 需要 CPU；如果容器 CPU limit 很低、`GOMAXPROCS` 配置不合理、请求高峰时 CPU 已经接近满载，GC 竞争 CPU 会更容易变成尾延迟。

优化方向通常不是“关闭 GC”，而是减少它对请求路径的干扰：

```text
降低分配率：复用 buffer、减少临时对象、避免不必要的 []byte/string 转换。
降低 live heap：控制缓存大小，及时释放大对象引用，避免小切片保留大数组。
调参数：基于指标调整 GOGC/GOMEMLIMIT，不凭感觉。
看证据：同时看 GC pause、GC CPU fraction、alloc rate、heap live、p99/p999。
```

面试里可以这样答：

```text
Go GC 对低延迟服务的影响不只是 STW pause。现代 Go pause 通常较短，但高分配率会让 GC 更频繁，live heap 变大会增加标记成本，mark assist 和写屏障也可能把成本摊到请求路径上，最终影响 p99/p999。调优时要同时看分配率、live heap、GC CPU、pause 分布和服务尾延迟，再决定是否减少分配、控制缓存、调整 GOGC 或设置 GOMEMLIMIT。
```

一句话：低延迟服务看 GC，要看尾延迟和总成本，不要只盯一次 pause 的均值。

## Q036. 如何观测 Go 程序的 GC pause？

**回答：**观测 GC pause 可以从轻到重分几层：运行时统计、运行时指标、日志、pprof/trace、业务监控。

第一，用 `runtime.ReadMemStats`。`MemStats` 里有几个直接相关字段：

```go
var ms runtime.MemStats
runtime.ReadMemStats(&ms)

fmt.Println("NumGC:", ms.NumGC)
fmt.Println("LastGC:", time.Unix(0, int64(ms.LastGC)))
fmt.Println("PauseTotal:", time.Duration(ms.PauseTotalNs))
fmt.Println("LastPause:", time.Duration(ms.PauseNs[(ms.NumGC+255)%256]))
fmt.Println("GCCPUFraction:", ms.GCCPUFraction)
```

`PauseTotalNs` 是程序启动以来 GC stop-the-world pause 的累计纳秒数；`PauseNs` 是最近 256 次 GC cycle 的 pause 环形缓冲。这个方式简单，但更适合临时诊断或暴露指标，不适合做高维度分析。

第二，用 `runtime/metrics`。新代码更推荐从 `runtime/metrics` 读直方图，例如：

```text
/sched/pauses/total/gc:seconds
/sched/pauses/stopping/gc:seconds
/gc/heap/live:bytes
/gc/cycles/total:gc-cycles
```

`/sched/pauses/total/gc:seconds` 统计 GC 相关 STW 总暂停延迟分布，适合接入 metrics 系统后看分位数。`/gc/pauses:seconds` 在较新的文档里被标记为 deprecated，推荐用 `/sched/pauses/total/gc:seconds`。

第三，用 `GODEBUG=gctrace=1` 看 GC 日志：

```bash
GODEBUG=gctrace=1 ./server
```

它会打印每轮 GC 的时间、堆大小、CPU 等信息。适合本地、压测和临时线上诊断，但不要无控制地长期打开到高流量日志里。

第四，用 execution trace：

```bash
curl -o trace.out 'http://localhost:6060/debug/pprof/trace?seconds=5'
go tool trace trace.out
```

trace 能看到 GC、STW、调度、网络阻塞等时间线。它比单独 pause 数字更适合回答“这段尾延迟是不是 GC 造成的，还是锁、系统调用、调度排队造成的”。

第五，用 pprof 辅助看原因。heap profile 看内存从哪里来，allocs profile 看分配热点。GC pause 是结果，分配率和 live heap 往往是原因。

第六，把 GC 指标和业务延迟放在同一张时间线上。比如请求 p99 抖动时，同时看：

```text
alloc rate 是否上涨
heap live 是否上涨
GC cycle 是否变密
GC pause 分布是否变大
GCCPUFraction 是否升高
goroutine runnable/latency 是否异常
```

面试里可以这样答：

```text
最直接可以用 runtime.ReadMemStats 看 NumGC、PauseTotalNs、PauseNs、GCCPUFraction；生产里更推荐 runtime/metrics，比如 /sched/pauses/total/gc:seconds 的直方图。临时诊断可以打开 GODEBUG=gctrace=1；要分析时间线和尾延迟原因，用 net/http/pprof 暴露 trace 后 go tool trace。排查时要把 GC pause 和业务 p99、分配率、live heap、GC CPU 一起看。
```

一句话：GC pause 既要能读到数值，也要能把它和分配热点、调度时间线、业务延迟关联起来。

## Q037. pprof CPU、heap、mutex、block profile 分别看什么？

**回答：**这四类 profile 看的不是同一个问题。用错 profile，很容易得出错误结论。

CPU profile 看“CPU 时间花在哪里”。它按采样记录正在 CPU 上执行的栈，适合回答：

```text
哪个函数消耗 CPU 最多？
热路径在哪里？
是不是 JSON、压缩、加密、正则、序列化、日志格式化太贵？
```

常见命令：

```bash
go test -cpuprofile cpu.out ./...
go tool pprof cpu.out

go tool pprof 'http://localhost:6060/debug/pprof/profile?seconds=30'
```

注意 CPU profile 不擅长看“等待”。如果请求慢是因为锁等待、channel 阻塞、I/O 等待，CPU profile 可能看起来很空。

heap profile 看“堆内存由哪些分配点贡献”。它常用两个视角：

```text
inuse_space / inuse_objects：当前还活着的对象，适合看内存占用和泄漏。
alloc_space / alloc_objects：历史累计分配，适合看分配压力和 GC 压力。
```

比如服务 RSS 上涨，要先看 inuse；GC 频繁但内存不高，要看 allocs。

```bash
go tool pprof 'http://localhost:6060/debug/pprof/heap'
go tool pprof -alloc_space 'http://localhost:6060/debug/pprof/heap'
```

mutex profile 看“锁竞争在哪里”。它记录 contended mutex 的持有/等待相关栈，适合回答：

```text
哪个锁让 goroutine 等得最多？
是全局锁、日志锁、连接池锁、缓存锁，还是 runtime 内部锁？
临界区是不是太大？
```

使用前通常要开启采样：

```go
runtime.SetMutexProfileFraction(5) // 平均记录约 1/5 的竞争事件
```

然后看：

```bash
go tool pprof 'http://localhost:6060/debug/pprof/mutex'
```

block profile 看“goroutine 阻塞在同步原语上的时间”。官方 `runtime/pprof` 文档说，block profile 跟踪在同步原语上阻塞的时间，比如 `sync.Mutex`、`sync.RWMutex`、`sync.WaitGroup`、`sync.Cond`、channel send/receive/select。

使用前要开启：

```go
runtime.SetBlockProfileRate(1) // 记录每个阻塞事件，开销较高，压测或短时间诊断用
```

然后看：

```bash
go tool pprof 'http://localhost:6060/debug/pprof/block'
```

block profile 适合看 channel 堵塞、等待 WaitGroup、Cond、锁等待等“不是 CPU 忙，但请求就是慢”的问题。mutex profile 更聚焦锁竞争，block profile 范围更广。

实际排查时可以按症状选：

```text
CPU 打满：先看 CPU profile。
内存高或 GC 压力大：看 heap/allocs profile。
怀疑锁竞争：看 mutex profile。
goroutine 很多、请求卡住、channel 堵：看 block profile 和 goroutine profile。
怀疑调度、网络、GC 时间线：看 trace。
```

面试里可以这样答：

```text
CPU profile 看 CPU 热点；heap profile 看堆上当前存活对象和累计分配热点；mutex profile 看锁竞争；block profile 看 goroutine 在 channel、Mutex、WaitGroup、Cond 等同步原语上阻塞的时间。CPU profile 不等于延迟分析，等待型问题要看 block/mutex/trace。
```

一句话：CPU 看“忙在哪”，heap 看“内存从哪来”，mutex 看“锁争在哪”，block 看“等在哪”。

## Q038. Go 中逃逸分析如何影响性能？

**回答：**逃逸分析是编译器判断一个变量能不能放在栈上，还是必须放到堆上的过程。它影响性能的主要路径是：堆分配更贵，堆对象会增加 GC 扫描和回收压力；栈分配通常只是移动栈指针，函数返回后自然失效。

最简单的例子：

```go
func local() int {
    x := 10
    return x
}
```

`x` 只在函数内部用，通常可以放在栈上，甚至直接放寄存器。

下面这个就可能逃逸：

```go
func ptr() *int {
    x := 10
    return &x
}
```

`x` 的地址被返回，函数返回后还要活着，所以不能放在当前栈帧里，必须让它逃逸到堆上。

常见导致逃逸的场景包括：

第一，返回局部变量地址：

```go
func NewUser(name string) *User {
    u := User{Name: name}
    return &u
}
```

这不一定坏。构造对象并返回指针是正常设计，只是它通常会产生堆对象。

第二，闭包捕获变量，且闭包生命周期超出当前函数：

```go
func counter() func() int {
    n := 0
    return func() int {
        n++
        return n
    }
}
```

`n` 要跟着闭包活下去，通常会逃逸。

第三，把值装进 interface，尤其是传给 `fmt`、`any`、反射、日志库：

```go
fmt.Sprintf("%v", user)
```

是否逃逸取决于具体调用和编译器优化，但 interface 装箱是常见逃逸来源。

第四，传给编译器无法看透的函数。跨包调用、接口调用、反射、cgo、函数变量调用，都可能让编译器保守判断。

第五，goroutine 捕获变量：

```go
func run(req *Request) {
    go func() {
        use(req)
    }()
}
```

新 goroutine 的生命周期可能超过当前函数，捕获的变量通常要延长生命周期。

查看逃逸分析可以用：

```bash
go build -gcflags='-m=2' ./...
```

或者针对测试：

```bash
go test -run '^$' -bench . -benchmem -gcflags='-m=2' ./...
```

你会看到类似：

```text
moved to heap: x
... escapes to heap
... does not escape
```

但要注意，逃逸不是绝对坏事。为了减少一次堆分配而把 API 改得很别扭，可能不值。更应该关注热路径上的逃逸和分配：在 benchmark 的 `allocs/op`、heap profile 的 `alloc_space`、真实服务的 GC 指标里能看到成本，再去改。

面试里可以这样答：

```text
逃逸分析决定变量能放栈上还是必须放堆上。逃逸到堆会增加分配成本和 GC 压力，进而影响吞吐和尾延迟。常见逃逸来源有返回局部地址、闭包捕获、goroutine 捕获、interface 装箱、反射或编译器看不透的调用。可以用 -gcflags=-m=2 查看，但优化时要结合 benchmem 和 pprof，不要看到 escapes to heap 就机械改代码。
```

一句话：逃逸分析影响的是对象生命周期和分配位置，真正要优化的是热路径上的堆分配。

## Q039. 如何减少临时对象分配？

**回答：**减少临时对象分配，核心是减少热路径上的“短命堆对象”。但要先通过 benchmark、pprof、metrics 找到分配热点，不能靠猜。

第一，预分配 slice 和 map 容量：

```go
out := make([]Item, 0, len(in))
for _, x := range in {
    out = append(out, convert(x))
}

m := make(map[string]Value, expectedSize)
```

这能减少扩容和 rehash，也能减少底层数组多次分配。

第二，固定长度时直接按下标写，避免 append 竞争和增长：

```go
out := make([]Result, len(jobs))
for i, job := range jobs {
    out[i] = handle(job)
}
```

第三，复用 buffer。字符串拼接用 `strings.Builder`，字节构造用 `bytes.Buffer` 或预分配 `[]byte`：

```go
var b strings.Builder
b.Grow(128)
b.WriteString(prefix)
b.WriteString(name)
s := b.String()
```

高频编码路径可以让调用方传入 buffer：

```go
func AppendUser(dst []byte, u User) []byte {
    dst = append(dst, u.Name...)
    dst = append(dst, ':')
    dst = strconv.AppendInt(dst, int64(u.Age), 10)
    return dst
}
```

第四，避免不必要的 `[]byte` 和 `string` 来回转换。每次转换都可能分配，尤其是在日志、协议解析、HTTP header、JSON 编解码路径上。能用 `strconv.AppendInt`、`bytes.Equal`、`strings.Cut` 等工具时，就少做中间对象。

第五，少在热路径使用 `fmt.Sprintf`。`fmt` 很通用，也更容易分配。简单数字和字符串拼接可以用 `strconv` 和 builder：

```go
buf = strconv.AppendInt(buf, id, 10)
```

第六，减少闭包和 interface 装箱。高频回调、日志字段、`any` 参数、反射调用都可能制造临时对象。不是说不能用，而是热路径里要看 `allocs/op`。

第七，控制对象生命周期，避免小对象保留大对象。比如从大文件 buffer 里切一个小 slice 返回，会让整个大 buffer 不能释放：

```go
func token(buf []byte) []byte {
    return buf[:10] // 可能保留整个 buf
}
```

需要长期保存时复制小片段：

```go
t := append([]byte(nil), buf[:10]...)
```

第八，谨慎使用 `sync.Pool`。它适合复用临时大对象，比如压缩 buffer、编码 buffer、临时 `bytes.Buffer`。但它不是缓存，GC 可以清理 pool；对象过小、竞争过高、生命周期复杂时，`sync.Pool` 可能让代码更难维护，还可能因为复用对象未清理干净引入 bug。

第九，按 ownership 设计数据结构。能在一个 goroutine 内局部构造并一次性发布，就不要到处共享可变对象。共享越多，逃逸和同步越多。

第十，用工具确认收益：

```bash
go test -bench . -benchmem ./...
go test -run '^$' -bench BenchmarkX -memprofile mem.out ./pkg
go tool pprof -alloc_space mem.out
go build -gcflags='-m=2' ./pkg
```

面试里可以这样答：

```text
减少临时分配一般从热路径入手：预分配 slice/map，复用 buffer，用 strings.Builder、bytes.Buffer、strconv.Append 系列减少中间对象，避免不必要的 []byte/string 转换，少在热路径用 fmt 和反射，固定长度结果按下标写，必要时用 sync.Pool 复用大临时对象。所有优化都要用 benchmem、heap/allocs profile 和逃逸分析验证。
```

一句话：减少分配不是到处抠对象，而是在证据明确的热路径上缩短生命周期、复用缓冲、避免无意义中间值。

## Q040. 为什么不要过早优化 allocation？

**回答：**不要过早优化 allocation，不是说分配不重要，而是说 allocation 优化很容易牺牲正确性、可读性和 API 质量，而收益未必在真实瓶颈上。

第一，很多分配不在热路径。一次请求里偶尔分配几个小对象，对延迟和吞吐可能没有可见影响。你花一天把它改成对象池，可能完全不如少一次数据库调用、少一次网络 round trip、减少一次锁竞争有效。

第二，编译器和运行时已经会做很多优化。逃逸分析、内联、栈分配、open-coded defer、栈增长、GC pacing 都在变化。你今天为了绕开某个逃逸写了复杂代码，下一版编译器可能已经能优化；复杂代码留下来的维护成本却一直在。

第三，手动复用对象容易引入 bug。对象池最常见的问题是状态没清干净：

```go
buf := pool.Get().(*bytes.Buffer)
defer pool.Put(buf)

// 忘记 Reset，下一次拿到的人看到旧数据
```

或者把已经放回 pool 的对象继续使用：

```go
pool.Put(buf)
return buf.Bytes() // 错：返回的数据可能被下一次复用覆盖
```

这类 bug 往往比多一次分配更严重。

第四，减少 allocation 可能增加内存保留。比如为了少分配复用一个很大的 buffer，但高峰后 buffer 长期留在池里，RSS 不降；或者为了避免复制返回小切片，却把大数组一直挂住。看起来少了分配，实际内存占用和 GC 扫描压力更差。

第五，过早优化会破坏接口。为了避免一个对象分配，把函数签名改成到处传 scratch buffer、返回复用对象、要求调用方遵守隐含生命周期，API 会变得难用。除非这是底层库或已证实的热路径，否则不值得。

第六，allocation 不是唯一性能来源。CPU profile、block profile、mutex profile、trace 可能告诉你真正的问题是锁竞争、channel 堵塞、系统调用、调度延迟、GC assist、慢 I/O。只看 `allocs/op` 容易把方向带偏。

合理流程应该是：

```text
1. 先写清楚、写对，明确 ownership 和同步边界。
2. 用 benchmark 和生产指标确认瓶颈。
3. 用 pprof/benchmem/escape analysis 找到具体分配点。
4. 只优化热路径上可证明有收益的分配。
5. 用测试、基准和压测确认没有牺牲正确性和尾延迟。
```

面试里可以这样答：

```text
不要过早优化 allocation，是因为分配优化经常让代码变复杂、破坏 API、引入对象复用 bug，收益也可能不在真实瓶颈上。Go 编译器和运行时已经会把很多短命对象放到栈上，真正需要处理的是 profile 证明的热路径堆分配。优化前先看 benchmem、pprof、GC 指标和业务延迟，优化后再验证。
```

一句话：allocation 优化要基于证据，先保证语义清楚，再处理热路径上的真实堆分配。

## Q041. goroutine leak 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**严格说，goroutine leak 不是一个要实现的功能，而是一个要避免的故障。它的核心目标可以反过来说：让每个启动的 goroutine 都有明确 owner、明确退出条件、明确取消路径，并且能在调用方放弃、超时、出错、服务关闭时退出。

Go blog 的 pipeline/cancellation 文章里有一句很实在的话：goroutine 不会被垃圾回收，它必须自己退出。泄漏的 goroutine 会消耗内存和 runtime 资源，goroutine 栈里持有的堆引用还会让对象不能被 GC。这个定义比“goroutine 数量变多”更准确：问题不是数量本身，而是生命周期失控。

从问题类型看，goroutine leak 同时影响正确性、性能、安全性和可维护性，但主次要分清。

第一，它首先是正确性问题。一个 goroutine 被启动后永远阻塞在 send、receive、锁、网络读、timer 或 select 上，说明程序的生命周期协议不完整。比如下游只读一个值就返回，上游还在无缓冲 channel 上发送：

```go
func gen() <-chan int {
    out := make(chan int)
    go func() {
        for i := 0; ; i++ {
            out <- i
        }
    }()
    return out
}

func first() int {
    ch := gen()
    return <-ch // gen 的 goroutine 后续会永远卡在 out <- i
}
```

这里的问题不是“慢”，而是 `gen` 的 owner 没有告诉内部 goroutine：调用方已经不再消费了。

第二，它是性能和资源问题。泄漏 goroutine 的栈初始很小，但会按需增长；更麻烦的是它会保留栈上的引用、channel、timer、连接、锁等待、请求对象、日志上下文。泄漏多了以后，heap、GC roots、调度开销、pprof 输出、关闭耗时都会变差。

第三，它可能变成安全问题。外部请求如果能触发一个不会退出的 goroutine，就等于提供了资源耗尽入口。单次泄漏也许不明显，高并发或恶意请求下会变成内存、FD、连接池、goroutine 数量的 DoS。

第四，它是可维护性问题。泄漏通常说明代码没有清楚表达“谁创建，谁取消，谁等待，谁关闭 channel”。这类代码后续很难改：加一个 timeout、重试或早返回，就可能把原来的隐性泄漏放大。

修复思路通常围绕四个动作。

```text
1. 创建 goroutine 时定义 owner。
2. owner 结束时发出取消信号，常见是 context 或 done channel。
3. goroutine 在所有阻塞点都能观察取消。
4. owner 能等待 goroutine 退出，常见是 WaitGroup 或 errgroup。
```

改写上面的例子：

```go
func gen(ctx context.Context) <-chan int {
    out := make(chan int)
    go func() {
        defer close(out)
        for i := 0; ; i++ {
            select {
            case <-ctx.Done():
                return
            case out <- i:
            }
        }
    }()
    return out
}

func first(ctx context.Context) int {
    ctx, cancel := context.WithCancel(ctx)
    defer cancel()

    ch := gen(ctx)
    return <-ch
}
```

面试里可以这样答：

```text
goroutine leak 的治理目标是让 goroutine 生命周期有边界：谁创建、谁取消、谁等待、阻塞点如何退出。它首先是正确性问题，因为泄漏说明并发协议不完整；随后会带来性能问题，比如内存、GC、调度、FD、连接池压力；在外部输入能触发泄漏时，也会变成资源耗尽安全问题。可维护性上的表现是生命周期边界混乱，后续加 timeout、重试、早返回时容易出事。
```

一句话：goroutine leak 的核心不是“少开 goroutine”，而是每个 goroutine 都必须能被它的 owner 收回来。

## Q042. goroutine leak 的典型适用场景和不适用场景分别是什么？

**回答：**这里要先纠正题目里的措辞：goroutine leak 本身没有“适用场景”，它是 bug。更准确的问法是：哪些场景必须重点防 goroutine leak，哪些场景不应该被误判成 leak。

必须重点防的场景有几类。

第一，pipeline、fan-out/fan-in、生成器。Go blog 的 pipeline 文章专门讲了这个问题：下游可能只消费一部分数据就返回，上游如果还在发送，就会卡住。任何“一个 goroutine 生产，一个 goroutine 消费”的代码，都要设计提前退出路径：

```go
select {
case out <- v:
case <-ctx.Done():
    return ctx.Err()
}
```

第二，请求级并发。HTTP handler、RPC handler、数据库查询、外部 API fan-out 都和请求生命周期绑定。请求取消、客户端断开、deadline 到期时，内部 goroutine 应该停止。如果 handler 返回了，内部 goroutine 还拿着请求对象继续跑，通常就是泄漏或业务越界。

第三，worker pool 和后台队列。worker 数量可能固定，但它们也要有 shutdown 协议。队列关闭、服务退出、上游失败时，worker 要退出，不能永远等一个不会再来的任务。

第四，watch、subscribe、stream、long polling。它们天然是长连接或长生命周期，容易把“正常长期运行”和“泄漏”混在一起。判断标准是：订阅取消或连接断开后，服务端 goroutine 是否退出。

第五，带 timeout 和 retry 的代码。每次重试都可能启动新 goroutine。如果旧尝试没有取消，重试越多泄漏越多。这个问题在线上比单元测试里明显，因为失败和超时路径更频繁。

第六，测试代码。测试结束后还有 goroutine 在跑，可能污染后续测试、抢日志、持有端口、让 race detector 报出看似无关的问题。`testing` 文档也提醒，`FailNow`、`SkipNow` 只停止测试 goroutine，不会停止测试里自己启动的其他 goroutine。

不应该误判成 leak 的场景也要分清。

第一，进程级后台 goroutine 不一定是 leak。HTTP server 的 accept loop、metrics exporter、signal handler、runtime 内部 goroutine，只要它们的 owner 是整个进程，生命周期到进程退出为止，就不是泄漏。它们是有意设计的常驻任务。

第二，短时间 goroutine 数量升高不一定是 leak。高峰流量、批处理、突发 fan-out 会让 goroutine 数量上升；只要压力下降后数量回落，栈里没有大量相同阻塞点，就更像正常弹性并发。

第三，阻塞不一定是 leak。一个 worker 阻塞在任务队列上，且队列在服务运行期一直有效，这是正常等待。泄漏的边界是：等待条件已经不可能发生，或者 owner 已经结束但 goroutine 还在等。

第四，有些任务不能靠 goroutine 自己取消。比如某些不支持 context 的阻塞系统调用、第三方库、驱动调用。此时要用 deadline、关闭连接、隔离 worker、进程级超时等方式治理，不能只在外层加一个 context 就假装解决。

面试里可以这样答：

```text
goroutine leak 本身不是适用技术，而是要避免的故障。最容易出现在 pipeline、fan-out、请求级并发、worker pool、watch/stream、timeout/retry 和测试里。判断是不是 leak，要看 owner 生命周期结束后 goroutine 是否还能退出。常驻 server loop、metrics、signal handler 这类进程级 goroutine 不是 leak；短时间 goroutine 数量上升也不是 leak，只要压力结束后能回落。
```

一句话：看 goroutine 是否泄漏，不看它活了多久，而看它是否还归某个有效 owner 管。

## Q043. goroutine leak 和相近概念最容易混淆的边界在哪里？

**回答：**goroutine leak 最容易和高 goroutine 数、普通阻塞、死锁、内存泄漏、backpressure、data race 混在一起。面试时把边界说清楚，会比背几个例子更有价值。

第一，goroutine leak 不等于 goroutine 数量多。高并发服务在高峰时有几万 goroutine 可能是正常的，只要它们是请求、连接、任务的一部分，结束后会回落。泄漏的特征是数量随时间或操作次数单调上升，压力结束后仍不下降，并且 pprof 里出现大量相同的阻塞栈。

第二，goroutine leak 不等于阻塞。阻塞是状态，泄漏是生命周期错误。比如 worker 等待任务：

```go
for job := range jobs {
    handle(job)
}
```

如果 `jobs` 会在 shutdown 时关闭，这个阻塞是正常的。如果没有任何地方会关闭 `jobs`，服务 Stop 后 worker 仍然在等，这才是泄漏。

第三，goroutine leak 和 deadlock 不一样。deadlock 通常是整个或一组 goroutine 互相等待，系统没有进展，严重时 runtime 报 `all goroutines are asleep - deadlock!`。goroutine leak 可以发生在系统仍然对外服务时，甚至吞吐看起来还行，只是后台不断累积垃圾 goroutine。

第四，goroutine leak 和 memory leak 有交集，但不是同一个概念。goroutine leak 会带来内存泄漏，因为 goroutine 栈、栈上引用、channel、timer、请求对象可能一直可达。反过来，纯内存泄漏可能没有多余 goroutine，比如全局 map 持续保存对象。

第五，goroutine leak 和 backpressure 的边界在于“是否有恢复路径”。有界队列满了，生产者阻塞，这是 backpressure；如果消费者已经退出但生产者还在发，阻塞就变成泄漏。buffered channel 只能延后问题，不能替代取消协议。

第六，goroutine leak 和 data race 是两类问题。race detector 抓的是并发访问共享内存没有同步；goroutine leak 很多时候没有 data race。一个 goroutine 安静地卡在 `<-ch` 上，`-race` 通常不会报。

第七，goroutine leak 和“慢 I/O”也容易混淆。goroutine 卡在网络读可能只是对端慢；如果没有 deadline，且请求已经取消后它仍然无限等，就变成 leak。边界是 deadline、连接关闭和取消是否能传到底层 I/O。

面试里可以这样答：

```text
goroutine leak 的边界是生命周期失控，不是数量多或阻塞本身。高 goroutine 数在高峰期可能正常，阻塞在有效队列上也可能正常；如果 owner 已经结束、等待条件不可能再发生、压力结束后数量不回落，才是泄漏。它和 deadlock、memory leak、backpressure、data race 有交集，但诊断入口不同：leak 主要看 goroutine profile、生命周期协议和取消路径。
```

一句话：goroutine leak 的判定标准是“还该不该活、还能不能退出”，不是“现在是不是 blocked”。

## Q044. goroutine leak 在高并发场景下可能出现哪些隐藏问题？

**回答：**高并发会把小泄漏放大。单次请求泄漏一个 goroutine，在本地压几次看不出来；到了线上每秒几千请求，几分钟就会变成明显资源问题。

第一，内存增长。每个 goroutine 有栈，栈会按需增长。泄漏 goroutine 还会把栈上的引用保住，比如 request body、response buffer、日志字段、数据库结果、用户上下文。GC 看到这些引用仍然可达，就不能回收相关对象。

第二，GC 成本上升。泄漏 goroutine 越多，runtime 要扫描和管理的栈越多。即使这些 goroutine 都是 parked，不消耗 CPU，它们仍然扩大 GC root 集合，增加内存压力。尾延迟可能因此变差。

第三，调度和诊断成本上升。大量 parked goroutine 不会像 busy loop 那样打满 CPU，但 runtime 仍要维护它们的状态。goroutine profile 也会变得很大，真正有用的栈被淹没，线上定位变慢。

第四，连接、FD、timer、ticker 泄漏。很多 goroutine leak 不是只多一个 goroutine，而是连带保存外部资源：

```text
网络读 goroutine -> 持有 TCP 连接或 HTTP response body
重试 goroutine -> 持有 timer
watch goroutine -> 持有订阅和服务端 stream
worker goroutine -> 持有队列引用和缓存对象
```

第五，连接池被耗尽。请求取消后，内部 goroutine 继续占着数据库连接、HTTP 连接或 Redis 连接，会让新请求拿不到连接。表面上看是“数据库慢”或“连接池不够”，根因可能是旧请求没有退出。

第六，背压变成级联阻塞。一个泄漏的 send 卡住上游，上游 worker 不释放，下游队列不消费，最后整个链路开始排队。这个过程在 pprof 上可能表现为大量 goroutine 卡在 channel send、receive、select 或 `sync.(*Cond).Wait`。

第七，错误路径被放大。高并发下 timeout、client disconnect、partial read、retry 更常见。很多 leak 不在成功路径出现，而在“下游提前返回但上游还在工作”的路径出现。

第八，安全风险变高。如果外部请求能制造不会退出的 goroutine，攻击者不需要很高 QPS，也能慢慢耗尽内存、连接或 goroutine。限制并发和设置 deadline 是防线，根本上还要保证取消能传播。

定位时通常先看这些指标：

```text
runtime.NumGoroutine 是否随时间单调上升
goroutine profile 中相同栈是否持续增加
heap inuse 和 alloc rate 是否异常
block profile 是否集中在 channel/锁等待
FD、连接池、timer、队列长度是否一起上升
p99/p999 是否在错误率之前先变差
```

面试里可以这样答：

```text
高并发下 goroutine leak 的隐藏问题是资源被成倍放大：goroutine 栈和栈上引用导致内存和 GC 压力上升；连接、FD、timer、ticker、请求对象被保留；连接池和队列被旧请求占住；大量相同阻塞栈让诊断困难。它不一定先表现为 CPU 打满，更多时候先表现为 goroutine 数单调上升、heap 增长、连接池耗尽和尾延迟变差。
```

一句话：goroutine leak 在线上常常不是一个 goroutine 的问题，而是一条资源链没有被释放。

## Q045. goroutine leak 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**这些场景会把 goroutine 生命周期边界打穿。成功路径里 goroutine 能自然结束，不代表失败路径也能结束。

第一，崩溃会清掉本进程 goroutine，但不会自动修复外部语义。进程崩溃后，本地泄漏当然不存在了；但它可能留下未确认消息、半写文件、未关闭的服务端 stream、正在远端执行的请求。重启后如果没有幂等和恢复协议，旧操作和新操作可能重叠。

第二，优雅重启要求 root context 取消并等待退出。服务收到 SIGTERM 后，常见做法是关闭 listener，取消 root context，等待 in-flight 请求和后台 worker 退出：

```go
ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
defer stop()

g, ctx := errgroup.WithContext(ctx)
g.Go(func() error { return serveHTTP(ctx) })
g.Go(func() error { return runWorkers(ctx) })

<-ctx.Done()
shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()
_ = server.Shutdown(shutdownCtx)
return g.Wait()
```

如果后台 goroutine 不听 root context，重启时就会卡在 shutdown，或者被进程强杀，留下未完成工作。

第三，timeout 只有在被观察时才有意义。`context.WithTimeout` 会在到期后关闭 `Done`，但不会强制杀掉 goroutine。`context` 文档也说 `CancelFunc` 通知操作放弃工作，但不等待工作停止。goroutine 必须在阻塞点检查：

```go
select {
case <-ctx.Done():
    return ctx.Err()
case out <- v:
}
```

如果底层 I/O 不支持 context，也要设置 deadline 或关闭连接，否则 goroutine 可能一直卡在 read/write。

第四，忘记调用 cancel 会泄漏 context 资源。`context` 文档明确说，取消派生 context 会释放相关资源；操作完成后应该调用 cancel。`WithDeadline` 的例子也提醒，即使 deadline 会过期，仍然应该调用 cancel，否则 context 和 parent 可能保留得比需要更久。

```go
ctx, cancel := context.WithTimeout(parent, 100*time.Millisecond)
defer cancel()
```

第五，重试会制造旧尝试泄漏。常见错误是超时后直接发起新 goroutine，但旧 goroutine 没被取消：

```go
for i := 0; i < 3; i++ {
    go callBackend(req) // 旧尝试没人管
    time.Sleep(backoff(i))
}
```

正确方式是每次尝试有自己的 context，失败或下一次尝试前取消，并且最终等待或让 goroutine 能自行退出。

第六，panic/recover 只处理当前 goroutine。某个 worker recover 了自己的 panic，不代表它启动的子 goroutine 会退出。反过来，子 goroutine panic 如果没有 recover，可能直接打崩进程。生命周期管理不能靠 recover 兜底。

第七，channel close 在错误路径上最容易错。早返回时忘记 close 输出 channel，下游 range 永远等；错误路径上 close 了仍有 sender 在发，又会 panic。正确模式通常是“发送方负责 close，且 close 发生在所有 send 结束后”，取消用独立的 context 或 done channel。

面试里可以这样答：

```text
崩溃会清掉本进程 goroutine，但不会清掉外部副作用；重启要取消 root context 并等待后台任务退出；timeout 只是关闭 Done，不会杀 goroutine，所有阻塞点都要观察取消；重试要取消旧尝试，不能只启动新 goroutine；错误路径上还要处理 channel close 和 WaitGroup/errgroup 等待。goroutine leak 多数不是成功路径暴露，而是在超时、早返回、重试、shutdown 这些路径暴露。
```

一句话：goroutine leak 的边界条件通常出现在“调用方已经走了，但被调用方还活着”的时刻。

## Q046. goroutine leak 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**要看泄漏的 goroutine 卡在哪里。goroutine leak 没有单一瓶颈，常见瓶颈来自内存和外部资源；CPU 反而不是最常见的第一症状。

第一，卡在 channel send/receive 的泄漏，主要消耗内存、GC 和调度管理成本。

```go
out <- v // 没有 receiver，goroutine parked
```

这种 goroutine parked 后不持续消耗 CPU，但它的栈、栈上引用、channel 引用都还活着。goroutine 数持续增长时，heap 和 GC 可能先出问题。

第二，卡在 lock 上的泄漏，瓶颈是锁竞争和等待。比如 goroutine 拿不到某个全局锁，或者拿了锁后等待 channel：

```go
mu.Lock()
defer mu.Unlock()
<-ch // 持锁等待，可能拖住其他请求
```

这类问题可以用 mutex profile、block profile 和 goroutine profile 一起看。锁等待本身不一定是 leak，但如果等待条件失效，它会把锁竞争放大。

第三，卡在网络 I/O 上，瓶颈是连接、FD、端口和连接池。比如没有 deadline 的 `Read`、没有使用 request context 的 HTTP 调用、没有关闭 response body：

```go
req, _ := http.NewRequest("GET", url, nil)
resp, err := http.DefaultClient.Do(req) // 没有带 ctx，也没有超时
```

这类泄漏可能表现为 `too many open files`、连接池耗尽、上游服务连接数异常、请求 hang 住。

第四，卡在 timer/ticker 上，瓶颈是 heap 和定时器管理。长生命周期 ticker 如果没有 Stop，或者 context timeout 没有 cancel，都会让定时器和相关对象保留更久。

```go
ticker := time.NewTicker(time.Second)
defer ticker.Stop()
```

第五，busy loop 型泄漏才会明显消耗 CPU。比如 select 里有 default，取消没处理好，goroutine 开始空转：

```go
for {
    select {
    case v := <-ch:
        use(v)
    default:
        // 空转
    }
}
```

这种问题 CPU profile 会很明显，和普通 parked goroutine leak 不一样。

第六，泄漏还可能制造二次瓶颈。比如旧请求 goroutine 占着数据库连接，新请求拿不到连接后排队；排队又增加 goroutine；goroutine 变多后 GC 压力上升；尾延迟继续变差。线上看起来像数据库慢，实际是 goroutine 生命周期没有收敛。

定位时可以按瓶颈选择工具：

```text
goroutine profile：看大量 goroutine 卡在哪个栈。
block profile：看 channel、WaitGroup、Cond、锁等同步等待。
mutex profile：看锁竞争。
heap/allocs profile：看泄漏是否保留大量对象。
CPU profile：看是否 busy loop 或高频重试。
runtime/metrics 和连接池指标：看 goroutine、heap、GC、FD、连接数趋势。
```

面试里可以这样答：

```text
goroutine leak 最常见的第一瓶颈是内存、GC 和外部资源，不一定是 CPU。卡在 channel 上通常是 parked goroutine，主要保留栈和引用；卡在网络 I/O 会消耗连接和 FD；卡在锁上会放大锁竞争；timer/ticker 泄漏会增加 heap 和定时器成本；只有 busy loop 或高频重试才会明显打 CPU。要先看 goroutine profile，再按栈选择 heap、block、mutex、CPU 或网络指标。
```

一句话：goroutine leak 的瓶颈取决于它卡住的资源，CPU 不是默认答案。

## Q047. goroutine leak 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**这三类测试的目标不同。correctness test 测生命周期协议是否正确；stress test 测低概率 interleaving 和错误路径；benchmark 测治理方式的成本和容量边界。

correctness test 要验证“取消后能退出”。常见断言包括：

```text
调用方提前返回时，上游 goroutine 能退出。
context cancel 后，所有 worker 能退出。
输出 channel 在所有 send 完成后关闭。
没有 send on closed channel。
Stop/Close 可以重复调用或至少行为明确。
Wait 能在超时时间内返回。
错误路径和成功路径都释放资源。
```

可以写一个小的 leak check，但要知道它受 runtime 和其他测试影响，不能把 `runtime.NumGoroutine` 当成绝对精确值：

```go
func TestNoLeakAfterCancel(t *testing.T) {
    before := runtime.NumGoroutine()

    ctx, cancel := context.WithCancel(context.Background())
    done := make(chan struct{})

    go func() {
        defer close(done)
        runPipeline(ctx)
    }()

    cancel()

    select {
    case <-done:
    case <-time.After(time.Second):
        t.Fatal("pipeline did not exit after cancel")
    }

    deadline := time.Now().Add(time.Second)
    for time.Now().Before(deadline) {
        if runtime.NumGoroutine() <= before+2 {
            return
        }
        time.Sleep(10 * time.Millisecond)
    }
    t.Fatalf("goroutine count did not return near baseline: before=%d after=%d",
        before, runtime.NumGoroutine())
}
```

更稳的做法是让组件暴露 `Close/Stop/Wait`，测试直接等 `Wait` 返回；goroutine 数只做辅助信号。测试中还要跑 `go test -race`，因为修 leak 时常会引入共享状态 race。

stress test 要扩大时序组合。重点不是跑一次 happy path，而是反复打断：

```text
并发启动和取消。
随机早返回，只消费部分 channel。
随机 timeout 和 retry。
慢 producer、慢 consumer、buffer 满。
上游报错、下游报错、中间 stage 报错。
Stop 和 Start/Submit 并发。
服务 shutdown 时仍有 in-flight 请求。
```

可以把压力循环和 `-race`、`-count`、不同 `GOMAXPROCS` 组合起来：

```bash
go test -race -run TestPipeline -count=100 ./...
go test -run TestPipeline -count=1000 -cpu=1,2,8 ./...
```

benchmark 要测两个层面。

第一，正常路径成本。比如加了 context select 后，吞吐、ns/op、allocs/op 变化多少：

```go
func BenchmarkPipeline(b *testing.B) {
    b.ReportAllocs()
    for i := 0; i < b.N; i++ {
        ctx, cancel := context.WithCancel(context.Background())
        _ = runOnce(ctx)
        cancel()
    }
}
```

第二，高并发和取消路径成本。`testing.B.RunParallel` 适合测多个 goroutine 并发调用时的吞吐和资源稳定性：

```go
func BenchmarkCancelParallel(b *testing.B) {
    b.ReportAllocs()
    b.RunParallel(func(pb *testing.PB) {
        for pb.Next() {
            ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
            _ = runOnce(ctx)
            cancel()
        }
    })
}
```

benchmark 里不要只看 `ns/op`，还要看 `allocs/op`、goroutine steady state、heap、block/mutex profile。对于 leak 治理，最重要的问题不是“取消分支有多快”，而是“高并发取消后资源是否回到稳定水平”。

面试里可以这样答：

```text
correctness test 测生命周期协议：cancel、early return、error、Close 后 goroutine 是否退出，Wait 是否返回，channel 是否正确关闭。stress test 测随机取消、超时、重试、慢消费者、buffer 满、并发 Stop 等低概率时序，最好配合 -race、-count、-cpu。benchmark 测治理成本和容量边界：正常吞吐、allocs/op、取消路径开销、goroutine 数是否稳定，以及 heap/block/mutex profile 是否健康。
```

一句话：leak 测试不能只测功能返回值，必须把“退出”本身当成可验证结果。

## Q048. 如果要求从零实现一个简化版 goroutine leak，你会先定义哪些不变量？

**回答：**这道题的表述要改一下：不能“实现 goroutine leak”，应该是实现一个 leak-safe 的并发组件，或者实现一个简化的 goroutine leak 检测/治理框架。先定义不变量，比先写代码更重要。

如果我要实现一个简化版 leak-safe worker group，我会先定这些不变量。

第一，每个 goroutine 必须有 owner。owner 可以是请求、组件、服务进程，不能是“随手 go 一下”。owner 负责取消和等待。

第二，每个 goroutine 必须有退出条件。退出条件至少包含：输入关闭、context 取消、组件 Stop、内部错误、处理完成。

第三，每个可能阻塞的点都必须能退出。channel send、receive、锁等待、timer、I/O 调用，都要么有 context，要么有 deadline，要么由关闭资源来打断。

```go
select {
case jobs <- job:
case <-ctx.Done():
    return ctx.Err()
}
```

第四，只有发送方关闭 channel。多个 sender 时，要等所有 sender 退出后由 owner 关闭，或者不关闭共享输入 channel，改用 context 取消。

第五，Stop 必须是幂等或明确禁止重复调用。线上 shutdown、错误回滚、defer 清理经常会重复触发 Stop。如果重复 Stop 会 panic，要在 API 上写清楚；更常见是用 `sync.Once` 做幂等。

第六，Stop 后不能再接收新任务。否则一边关闭 worker，一边 Submit 新任务，会制造 send on closed channel 或卡住。

第七，Wait 返回时，组件内部启动的 goroutine 都已经退出。不能只表示“主循环退出”，子 goroutine 还在跑。

第八，取消不等于等待。`cancel()` 只是发信号，`Wait()` 才证明 goroutine 退出。这个边界在 `context` 文档里也能看到：`CancelFunc` 通知操作放弃工作，但不等待工作停止。

第九，资源释放和 goroutine 退出绑定。连接、文件、ticker、timer、临时 buffer、订阅句柄，都应该在 goroutine 退出路径上释放。

第十，错误要能传播到 owner。否则 worker 内部失败后静默退出，owner 继续等待结果，可能又造成等待方泄漏。

一个很小的骨架可以长这样：

```go
type Group struct {
    ctx    context.Context
    cancel context.CancelFunc
    wg     sync.WaitGroup
    once   sync.Once
}

func NewGroup(parent context.Context) *Group {
    ctx, cancel := context.WithCancel(parent)
    return &Group{ctx: ctx, cancel: cancel}
}

func (g *Group) Go(fn func(context.Context)) {
    g.wg.Add(1)
    go func() {
        defer g.wg.Done()
        fn(g.ctx)
    }()
}

func (g *Group) Stop() {
    g.once.Do(g.cancel)
}

func (g *Group) Wait() {
    g.wg.Wait()
}
```

这个骨架还不够完整，比如没有错误传播、没有禁止 Stop 后 Go、没有并发安全地记录状态。真正实现时要补状态机。但它表达了最小不变量：统一 context、统一取消、统一等待。

如果是实现 leak detector，不变量会换成：

```text
采样前后有稳定窗口。
允许 runtime 和测试框架自身 goroutine 白名单。
关注新增且持续存在的栈，而不是一次性数量波动。
能输出 goroutine profile，给出相同栈聚合。
不能把进程级常驻 goroutine 判成 leak。
```

面试里可以这样答：

```text
我不会说实现一个 goroutine leak，而是实现 leak-safe 的并发组件。先定不变量：每个 goroutine 有 owner；所有阻塞点能响应取消；发送方负责关闭 channel；Stop 后不接收新任务；cancel 发信号，Wait 证明退出；Wait 返回时内部 goroutine 全部结束；资源释放和退出路径绑定；错误能回传 owner。代码可以从 context+WaitGroup 的 Group 骨架开始，再补状态机和错误传播。
```

一句话：防 leak 的不变量就是生命周期 ownership，不是某个单独的 API。

## Q049. goroutine leak 的常见误用是什么，误用后通常会产生什么线上症状？

**回答：**常见误用基本都围绕同一个问题：启动 goroutine 的地方很随意，结束 goroutine 的协议却没有写出来。

第一，生产者 send，但消费者可能提前退出：

```go
func produce(out chan<- Item) {
    go func() {
        for _, item := range loadAll() {
            out <- item
        }
    }()
}
```

如果消费者只读一部分就返回，生产者卡在 `out <- item`。线上症状是 goroutine profile 里大量相同的 channel send 栈。

第二，消费者 range，但生产者从不 close：

```go
for v := range ch {
    handle(v)
}
```

如果没有任何 sender 负责 `close(ch)`，消费者会永远等。线上症状是 shutdown 卡住，worker 不退出，测试偶发超时。

第三，创建 context 后不调用 cancel：

```go
ctx, _ := context.WithTimeout(parent, time.Second)
return call(ctx)
```

`context` 文档明确要求操作完成后调用 cancel 释放资源。漏掉后，timer、child context 和 parent 的关联会保留到超时或 parent 取消。线上症状是 timer/heap 压力变高，短请求多时更明显。

第四，外层有 context，内部 goroutine 不用：

```go
func handler(w http.ResponseWriter, r *http.Request) {
    go func() {
        doSlowWork(context.Background()) // 错：脱离请求生命周期
    }()
}
```

请求取消了，内部任务还在跑。线上症状是客户端断开后服务端 CPU、数据库、外部 RPC 仍然持续消耗。

第五，重试不取消旧尝试：

```go
for attempt := 0; attempt < 3; attempt++ {
    go tryOnce()
    if waitResult(timeout) {
        return nil
    }
}
```

超时只是调用方不等了，旧 goroutine 还可能继续占资源。线上症状是失败率越高，goroutine 和连接数涨得越快。

第六，ticker 没有 Stop：

```go
t := time.NewTicker(time.Second)
go func() {
    for range t.C {
        flush()
    }
}()
```

如果没有退出条件和 `t.Stop()`，这个 goroutine 会跟着 ticker 常驻。若它本来应该属于某个组件实例，组件销毁后仍然 flush，就是泄漏。

第七，在库函数里悄悄启动后台 goroutine，但没有返回 Close/Stop。调用方没有办法管理生命周期。线上症状是模块被重复创建后 goroutine 数线性增长。

第八，只依赖 buffered channel 避免阻塞。buffer 能吸收短暂峰值，但如果下游永久退出，buffer 填满后生产者还是会卡住。线上症状通常延迟出现，看起来像“跑了一段时间才挂”。

第九，测试里调用 `t.Fatal` 后以为所有 goroutine 都停了。`testing` 文档说 `FailNow` 只能从测试 goroutine 调用，也不会停止测试里启动的其他 goroutine。线上对应的问题是错误路径提前返回但后台任务没收掉。

常见线上症状可以按观测面归类：

```text
goroutine 数：runtime.NumGoroutine 单调上升。
pprof：大量 goroutine 卡在同一处 channel send/receive、select、I/O、锁。
内存：heap inuse 上升，GC 更频繁，栈持有大对象。
连接：FD、数据库连接、HTTP idle/active connection 异常。
延迟：p99/p999 先变差，随后错误率上升。
shutdown：优雅关闭超时，测试进程不退出。
日志：请求结束后仍然打印旧 request id 的日志。
```

面试里可以这样答：

```text
常见误用包括：send 方不知道 receiver 会提前退出，range 一个永远不 close 的 channel，创建 context 不 cancel，内部 goroutine 用 Background 脱离请求生命周期，重试时不取消旧尝试，ticker 不 Stop，库函数启动后台 goroutine 却不暴露 Close，错误路径早返回但没有等待清理。线上通常表现为 goroutine 数单调上升、pprof 大量相同阻塞栈、heap 和 GC 压力上升、连接池耗尽、尾延迟变差、优雅关闭卡住。
```

一句话：goroutine leak 的误用不是“用了 goroutine”，而是没有给 goroutine 留退路。

## Q050. goroutine leak 在单机和分布式环境中的语义有什么差异？

**回答：**单机里的 goroutine leak 是进程内部生命周期问题；分布式环境里，本地 goroutine 只是更大请求链路的一段。取消、超时、重试、重启都要跨进程传播，语义会弱很多。

单机里，owner 和 goroutine 在同一个进程。`context.CancelFunc` 关闭 `Done`，本地 goroutine 在 select 里收到信号后退出；`WaitGroup` 或 `errgroup` 可以证明它已经结束。进程退出时，本地 goroutine 全部消失。

分布式环境里，取消不是共享内存事件，而是协议事件。服务 A 的 context cancel 只能影响 A 本地 goroutine；如果 A 已经发出 RPC 到服务 B，B 是否停止取决于 RPC 框架是否把 deadline/cancel 传过去，以及 B 的 handler 是否真的检查 context。

```text
本地：close(ctx.Done) -> goroutine select 收到 -> return。
远端：client cancel -> 连接/协议传递取消 -> server runtime 感知 -> handler 检查 ctx -> 下游也继续传。
```

中间任何一层不支持，取消就断了。

第二，重启语义不同。单机进程重启会清掉本地泄漏；但远端服务可能还在执行旧请求，消息队列里可能还有未 ack 消息，数据库里可能已有半完成写入。重启不能当成分布式取消。

第三，重试会制造重复工作。单机里旧 goroutine 没退出是 leak；分布式里旧请求可能已经到达远端并继续执行，新重试又发出第二个请求。即使本地 goroutine 没泄漏，也可能有远端 orphan work。这里需要 request id、幂等键、去重、lease、fencing token 或任务状态机。

第四，deadline 是端到端预算，不只是本地 timeout。单机里 `WithTimeout(100ms)` 控制本地等待；分布式里要把剩余预算传给下游。否则 A 超时返回了，B 和 C 还各自跑默认超时，资源继续消耗。

第五，stream/watch 更容易出现双端泄漏。客户端忘记取消，本地 goroutine 泄漏；服务端没有感知断开，服务端 stream goroutine 也泄漏。要依赖 context、连接关闭、heartbeat、read deadline、server-side cleanup 一起工作。

第六，观测也不同。单机看 `runtime.NumGoroutine` 和 goroutine profile；分布式要串起 trace、request id、连接数、队列积压、远端 in-flight、重试率。一个服务的 goroutine 数正常，不代表整个请求链路没有 orphan work。

第七，安全边界更复杂。单机 goroutine leak 可能导致本进程 DoS；分布式泄漏可能变成跨服务放大：一个入口请求触发多个下游长任务，下游取消不及时，资源消耗在多个服务里扩散。

面试里可以这样答：

```text
单机 goroutine leak 是进程内 goroutine 生命周期失控，可以用 context、done channel、WaitGroup/errgroup 和 pprof 直接治理。分布式环境里，取消和超时要靠协议传播；本地 cancel 不等于远端停止，重启也不等于远端工作取消。重试还可能制造 orphan work 和重复副作用，所以要传递 deadline、使用幂等键/请求 ID、让下游尊重 context，并用 trace 和跨服务指标确认资源是否真的收敛。
```

一句话：单机 leak 关注 goroutine 是否退出，分布式 leak 还要关注远端工作是否停止、重复请求是否可控。

## Q051. GMP scheduler 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**GMP scheduler 的核心目标，是把大量 goroutine 高效地映射到有限的 OS thread 上执行，同时让阻塞、系统调用、网络 I/O、GC、抢占和 work stealing 都能协同工作。Go runtime 的 `HACKING` 文档把它说得很直白：scheduler 管理 G、M、P 三类资源；G 是 goroutine，M 是 OS thread，P 是执行 Go 用户代码所需的资源；scheduler 的工作就是把 G、M、P 匹配起来。

这不是应用层可以直接调用的“任务调度库”。它是 runtime 内部机制。应用代码通过 `go` 语句、channel、mutex、syscall、netpoll、`GOMAXPROCS`、`runtime.Gosched`、`runtime.LockOSThread` 等方式影响它，但不能直接指定某个 goroutine 跑在哪个 P 或哪个线程上。

从目标分类看，GMP 首先解决性能和资源利用问题，其次保证 runtime 自身正确性。

第一，性能目标是低成本并发。Go 希望你能创建很多 goroutine，而不是手写线程池。goroutine 比 OS thread 轻量，但最终还是要靠 OS thread 执行。GMP 把 goroutine 的调度控制放在 runtime 里，减少频繁创建 OS thread、线程上下文切换和全局锁竞争的成本。

第二，资源目标是把并行度控制在合理范围。P 的数量等于 `GOMAXPROCS`，表示同一时刻最多有多少个 M 可以执行 Go 用户代码。一个 M 进入阻塞系统调用时，会把 P 释放出来，让别的 M 接手继续执行 Go 代码。这样一个阻塞 syscall 不会把整个进程的 Go 执行资源卡死。

第三，延迟目标是减少 runnable goroutine 等太久。local run queue、global run queue、work stealing、netpoll、timer、sysmon、preemption 都在这里发挥作用。比如某个 P 的队列空了，可以从别的 P 偷一部分 runnable G；某个 goroutine 长时间占 CPU，sysmon 可以触发抢占，降低其他 goroutine 饥饿风险。

第四，正确性目标是 runtime 层面的状态机不乱。一个 G 同一时刻不能被两个 M 执行；一个 P 同一时刻只能被一个 M 拥有；G 的状态要在 runnable、running、waiting、syscall、dead 等状态之间按规则转换；GC 要能在安全点扫描栈；netpoll 返回的 goroutine 不能丢。

安全性不是 GMP 的直接目标，但它会影响资源耗尽风险。比如 goroutine 爆炸、线程爆炸、忙等、忘记取消、无限 fan-out，最终会拖垮进程。GMP 能降低并发成本，不能替你做业务限流。

可维护性也不是直接目标，但 GMP 把复杂调度藏在 runtime 里，让应用层只需要写 goroutine、channel、context。代价是：一旦出问题，定位要看 pprof、trace、schedtrace 和 runtime 指标，不能只靠业务日志。

面试里可以这样答：

```text
GMP scheduler 的核心目标是把很多 goroutine 映射到有限 OS thread 上，并用 P 表示执行 Go 代码所需的调度和分配器资源。它主要解决性能、并行度控制和 runtime 资源利用问题，同时要求 runtime 状态机正确，比如 G 不能被重复执行，P 不能被多个 M 同时拥有，系统调用和网络 I/O 不能长期占住执行资源。它不替代业务层限流、取消和分布式调度。
```

一句话：GMP 是 Go runtime 的本地执行调度器，目标是用有限线程高效、可控地跑大量 goroutine。

## Q052. GMP scheduler 的典型适用场景和不适用场景分别是什么？

**回答：**GMP scheduler 对所有 Go 程序都自动生效，所以这里的“适用场景”不是“什么时候启用 GMP”，而是“什么样的并发模型适合交给 Go runtime 调度”。

适合的场景有几类。

第一，大量独立或弱耦合任务。比如 HTTP/RPC 请求处理、后台 worker、pipeline stage、fan-out 查询。每个任务用一个或少量 goroutine 表达，runtime 负责把 runnable goroutine 分配到 P/M 上。

```go
for {
    conn, err := ln.Accept()
    if err != nil {
        return err
    }
    go handleConn(conn)
}
```

这类代码的优势是直接：业务生命周期天然对应 goroutine 生命周期，调度细节交给 runtime。

第二，I/O 密集型服务。网络 I/O 会和 runtime netpoll 配合，goroutine 在等网络时可以 park，M/P 去跑别的 G。对常见 TCP、HTTP、RPC 服务来说，这比每连接一个 OS thread 更节省资源。

第三，中等粒度 CPU 并行。CPU-bound 任务可以用 goroutine 并发执行，但并行度要由 `GOMAXPROCS` 和应用层 worker 数共同控制。比如按 shard 并行处理、并行压缩、并行哈希。这里不能无限 `go`，否则只是把排队从业务队列挪到 runtime run queue。

第四，需要大量阻塞等待但每个等待都不该占线程的场景。channel receive、mutex 等 Go 同步原语会让 goroutine park，通常不会长期占住 OS thread。runtime 的 `HACKING` 文档也区分了 `gopark/goready` 和直接阻塞 M 的 runtime mutex。

不适合或要谨慎的场景也很明确。

第一，硬实时或严格优先级调度。Go scheduler 不承诺硬实时延迟，也不提供用户态优先级队列。你不能要求某个 goroutine 必须在固定微秒内被调度。

第二，依赖 OS thread affinity 的代码。GUI 主线程、某些 OpenGL/DirectX 调用、线程本地状态、某些 C 库需要固定线程。Go 提供 `runtime.LockOSThread`，但这意味着你主动绕开一部分普通调度弹性，必须负责解锁和线程状态恢复。

第三，大量阻塞 cgo 或不可中断 syscall。M 可以因为 syscall/cgo 增长，但这不是免费资源。如果很多 goroutine 卡在不受 Go runtime 管理的阻塞调用里，线程数、栈、FD 和外部资源都会上升。

第四，分布式任务调度。GMP 只调度单进程里的 goroutine，不知道集群节点、队列积压、数据 locality、任务重试、幂等、租约。Kubernetes、Ray、Temporal、消息队列的调度问题，不能靠 GMP 解决。

第五，需要确定性执行顺序的测试。Go scheduler 不保证 goroutine 执行顺序。`proc.go` 里还有针对 race detector 的调度随机化，用来暴露测试里的隐性调度假设。测试应该用 channel、WaitGroup、Cond、context 建立顺序，而不是靠 sleep 或“看起来会先跑”。

第六，极细粒度任务。每个元素一个 goroutine，如果每个任务只做几十纳秒工作，调度、同步、栈和队列成本可能超过业务计算。此时批处理、worker pool、SIMD、普通 for 循环可能更合适。

面试里可以这样答：

```text
GMP 适合 Go 进程内的大量轻量并发，尤其是请求处理、I/O 密集服务、pipeline、worker、适度 CPU 并行。它不适合硬实时调度、严格优先级、依赖固定 OS 线程的代码、大量不可中断 cgo/syscall、分布式任务调度，也不保证测试里的 goroutine 执行顺序。应用层要自己做限流、取消、队列和资源预算。
```

一句话：GMP 擅长进程内 goroutine multiplexing，不负责业务调度和集群调度。

## Q053. GMP scheduler 和相近概念最容易混淆的边界在哪里？

**回答：**GMP scheduler 最容易和 OS scheduler、netpoll/event loop、应用层 worker pool、channel 调度、GC、context cancellation 混在一起。边界说清楚，很多面试追问就不会乱。

第一，GMP scheduler 不是 OS scheduler。Go runtime 调度 G 到 M/P；OS 调度 M 到 CPU core。Go 只能控制 goroutine 在进程内如何排队和切换，不能控制内核把哪个线程放在哪个核心上跑。CPU quota、cgroup、系统负载、抢占线程，还是 OS 的事。

```text
Go 层：G -> M/P
OS 层：thread(M) -> CPU
```

第二，P 不是 CPU core。`HACKING` 文档说 P 可以类比 OS scheduler 里的 CPU，但它不是物理核心。P 表示执行 Go 用户代码所需的 runtime 资源，数量等于 `GOMAXPROCS`。机器有 16 个逻辑 CPU，不代表你的进程一定有 16 个 P；容器里可用 CPU 也要看运行时和 Go 版本对配额的处理。

第三，GMP 不是 netpoll。netpoll 负责把网络 I/O readiness 转成 runnable goroutine；GMP 负责把 runnable goroutine 调度执行。一个 goroutine 卡在网络读上时，通常被 park，等 netpoll 通知后再变成 runnable。

第四，GMP 不是 channel 的语义本身。channel send/receive 会让 goroutine ready 或 park，但 channel 的 FIFO 等待队列、close 语义、select 选择规则属于语言和 runtime 同步原语；GMP 只是在 goroutine 状态变化后接管 runnable G 的执行。

第五，GMP 不是应用层 worker pool。worker pool 管的是业务并发数，比如最多同时处理 32 个文件；GMP 管的是 goroutine 如何在 P/M 上执行。你不做 worker pool，GMP 也会调度，但它不会知道数据库连接数、下游 QPS、内存预算。

第六，GMP 抢占不是 context cancellation。preemption 只是 runtime 为了公平性、GC 和调度让正在运行的 G 停下来，之后它还会继续跑。context cancellation 是业务协议，表示这件工作应该放弃。一个 CPU-bound goroutine 被抢占过很多次，也不代表它会因为请求超时自动退出。

第七，GMP 不等于 race detector 或内存模型。scheduler 改变执行交错，可以暴露 data race，但不会定义 happens-before。真正的 happens-before 来自 mutex、channel、atomic、Once、WaitGroup 等同步关系。

第八，GMP 不保证公平到业务可见的程度。runtime 会努力避免饥饿，比如全局队列检查、work stealing、sysmon 抢占，但这不是服务级 SLO。长临界区、无限 goroutine、无界队列、忙等，还是会让请求排队和尾延迟变差。

面试里可以这样答：

```text
GMP 的边界是进程内 goroutine 执行调度。它不是 OS scheduler，OS 仍然调度 M 到 CPU；P 不是 CPU core，而是执行 Go 代码的 runtime 资源；netpoll 只负责 I/O readiness，GMP 负责 runnable goroutine；worker pool 是业务限流，不是 runtime 调度；preemption 不是 context cancel；scheduler 也不定义 happens-before。写并发程序时不能把这些概念混成一个“调度器”。
```

一句话：GMP 管本进程内 G 怎么跑，不管业务该不该跑、远端跑不跑、同步关系是否正确。

## Q054. GMP scheduler 在高并发场景下可能出现哪些隐藏问题？

**回答：**高并发下，GMP 的成本通常不是“调度器坏了”，而是应用把过多 runnable work、阻塞 work 或不可控 work 交给 runtime。GMP 很强，但它不是无限资源池。

第一，goroutine 数量暴涨。goroutine 栈初始小，但不是零成本。几十万 goroutine 会带来栈内存、GC root、调度状态、pprof 噪声。很多 goroutine 如果都处于 runnable 状态，还会排队争 P。

第二，runnable 队列积压。I/O 密集服务常见的是很多 goroutine parked；CPU 高峰时更危险的是很多 goroutine runnable。P 只有 `GOMAXPROCS` 个，runnable G 太多时，本质上就是在 runtime run queue 排队。表现可能是 CPU 打满、p99 变差，但单个函数看不出特别慢。

第三，work stealing 和全局队列不是免费。local run queue 能减少全局锁竞争，work stealing 能平衡负载；但大量短任务、频繁 ready/park、跨 P 迁移，会增加队列操作、cache miss 和同步成本。

第四，系统调用和 cgo 会放大线程数。普通阻塞 syscall 进入 `_Gsyscall` 后，P 可以被释放给别的 M；但 M 本身仍然卡在 syscall。大量阻塞 syscall/cgo 会让 OS thread 数增长。`runtime` 文档里的 threadcreate profile 就是看线程创建来源的入口。

第五，网络 I/O 不是只靠 GMP。netpoll 能让网络等待不占 M/P，但如果你没有 deadline、没有读完或关闭 body、连接池耗尽，goroutine 仍会在 I/O 链路上堆积。此时看起来像 scheduler 延迟，实际可能是下游慢或连接资源没释放。

第六，长时间不让出的 CPU-bound goroutine 会影响尾延迟。现代 Go 有异步抢占和 sysmon，但抢占不是硬实时。大循环、nosplit、cgo、系统栈、某些不可抢占区域仍可能拖慢 GC 或其他 goroutine 的调度。

第七，忙等会把 GMP 优势抹掉。`select { default: }` 空转、for 循环轮询 atomic、无 sleep 的重试，会让 goroutine 一直 runnable，占住 P：

```go
for {
    select {
    case v := <-ch:
        use(v)
    default:
        // 空转，CPU 会被打满
    }
}
```

第八，`LockOSThread` 使用不当会减少调度弹性。被锁定的 goroutine 必须在固定 OS thread 上跑。如果这类 goroutine 多，或者忘记 `UnlockOSThread`，线程利用率和资源回收会变差。

第九，容器 CPU 配额和 `GOMAXPROCS` 不匹配。`GOMAXPROCS` 太大时，Go 认为自己有很多 P，但容器实际 CPU quota 很小，线程在 OS 层抢 CPU，会导致延迟抖动。`GOMAXPROCS` 太小，又会限制 CPU-bound 任务吞吐。

第十，问题会被误归因。高并发下 p99 变差，可能是 GC、锁、连接池、下游 I/O、业务队列，也可能是 scheduler latency。要用 trace、pprof、schedtrace、runtime metrics 一起看。

可观察信号包括：

```text
runtime.NumGoroutine 持续升高
GODEBUG=schedtrace 输出的 runqueue、threads、idleprocs 异常
go tool trace 中 goroutine runnable latency 变大
threadcreate profile 变多
block/mutex profile 显示大量同步等待
CPU profile 显示 busy loop 或调度相关开销
```

面试里可以这样答：

```text
高并发下 GMP 的隐藏问题多半来自应用层无界并发：goroutine 数暴涨、runnable 队列积压、work stealing/cache miss 成本、阻塞 syscall/cgo 造成线程数增长、忙等占住 P、LockOSThread 降低调度弹性、容器 CPU quota 和 GOMAXPROCS 不匹配。定位时不能只看 CPU，要结合 trace 的 runnable latency、schedtrace、goroutine/threadcreate profile、block/mutex profile 和业务队列指标。
```

一句话：GMP 能调度大量 goroutine，但不能把无界并发变成免费并发。

## Q055. GMP scheduler 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**GMP 是进程内调度器，所以崩溃、重启、超时、重试这些场景里，它能保证的东西很有限。它能调度还活着的 goroutine，不能替你保存业务状态，也不能替你取消远端工作。

第一，崩溃会终止整个进程，所有 G/M/P 都消失。panic 如果没有在对应 goroutine 内 recover，可能打崩进程；runtime 自身的 `throw`、fatal map 并发写这类错误会直接终止。GMP 不做“崩溃后恢复 goroutine”。业务要靠外部 supervisor、进程重启、日志、checkpoint、幂等来恢复。

第二，重启后没有调度状态延续。run queue 里的 G、timer、netpoll wait、channel wait 都是进程内状态，进程重启后全没了。消息是否重放、任务是否重新领取、请求是否重试，是业务协议或外部系统的责任。

第三，超时不是 scheduler 行为。`context.WithTimeout` 到期会关闭 Done，goroutine 只有在检查 context 或调用支持 context 的 API 时才会退出。GMP 的 preemption 只是暂停和恢复 G，不表示这项业务要取消。

```go
for {
    select {
    case <-ctx.Done():
        return ctx.Err()
    case job := <-jobs:
        handle(job)
    }
}
```

如果 `handle` 内部是长时间 CPU 计算，runtime 可能抢占它让别人运行，但业务上它仍会继续算，除非你在计算循环里检查取消。

第四，重试会增加 runnable work。一次请求超时后，如果旧 goroutine 没取消，新重试又启动一批 goroutine，GMP 只能继续调度它们。对 runtime 来说它们都是合法 runnable G；对业务来说可能已经是重复工作。

第五，阻塞 syscall/cgo 不是立即可控。M 进入 syscall 后，P 可以交给别人，但这个 M 仍然在 OS 调用里。超时、重启、shutdown 想让它停下来，通常要靠 deadline、关闭 fd、取消底层请求，或者让 C 库配合。

第六，优雅退出要给 goroutine 留出退出窗口。收到 SIGTERM 后，只是进程准备退出；GMP 不会自动把所有 goroutine 按业务顺序收尾。应用需要 root context、server Shutdown、worker Stop、errgroup/WaitGroup 等组合：

```go
ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
defer stop()

g, ctx := errgroup.WithContext(ctx)
g.Go(func() error { return serve(ctx) })
g.Go(func() error { return workers(ctx) })

err := g.Wait()
```

第七，`LockOSThread` 在崩溃和退出路径上有额外边界。`runtime` 文档说，锁定线程的 goroutine 会一直在当前 OS thread 上执行；如果 goroutine 没解锁就退出，该线程会终止。调用 OS 服务或 C 库后，如果线程状态被永久改变，解锁前要确认这个线程还能安全跑其他 goroutine。

面试里可以这样答：

```text
GMP 只管理进程内还活着的 goroutine。崩溃会清空 G/M/P，重启不会恢复 run queue、timer、netpoll 等调度状态；超时只是业务取消信号，不是 scheduler 自动杀 goroutine；重试会把旧工作和新工作都交给 runtime，GMP 不知道哪个是过期请求；阻塞 syscall/cgo 还需要 deadline 或关闭底层资源。优雅退出必须用 root context、Shutdown、WaitGroup/errgroup 明确收尾。
```

一句话：GMP 负责运行 goroutine，不负责业务恢复、超时语义和重试去重。

## Q056. GMP scheduler 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**GMP 相关瓶颈要按症状拆。它可能表现为 CPU、内存、锁、I/O、网络中的任何一种，但“scheduler 瓶颈”本身通常是 runnable latency、队列积压、线程增长或频繁 park/unpark。

第一，CPU 瓶颈。CPU-bound goroutine 多于 P 时，所有 runnable G 竞争 `GOMAXPROCS` 个执行名额。CPU profile 会显示业务计算、序列化、压缩、加密、正则、JSON 等热点。此时调大 goroutine 数没有用，只会增加排队。

第二，内存瓶颈。goroutine 太多、栈增长、每个 goroutine 持有大对象、调度结构和 channel 堆积，都会增加 heap 和 GC 压力。很多 parked goroutine 不烧 CPU，但会占内存和 GC root。

第三，锁竞争。应用层大锁、runtime 全局锁、channel 热点、连接池锁都会导致 goroutine 频繁 park/unpark。Go diagnostics 文档建议用 block profile 看同步阻塞，用 mutex profile 看锁竞争。锁问题常被误判成 scheduler 慢。

第四，I/O 瓶颈。网络 I/O 可由 netpoll 协作，但磁盘 I/O、某些 syscall、cgo、第三方库可能阻塞 OS thread。P 可以被移交，不代表线程和外部资源没有成本。threadcreate profile 和 trace 可以帮助看线程增长与 syscall 关系。

第五，网络瓶颈。网络不是 GMP 的内部瓶颈，但会让 goroutine 大量等待。下游慢、连接池耗尽、没有 deadline、response body 没关闭，都会让 goroutine 卡住。看起来是 goroutine 多，根因可能是远端服务慢。

第六，调度本身的瓶颈。大量短任务反复创建、ready、park、unpark，任务粒度太细，run queue 操作和 work stealing 成本会变明显。`go tool trace` 里的 goroutine runnable latency、processor utilization、STW/GC、network blocking 时间线，比单独 CPU profile 更适合看这类问题。

可以用这个顺序判断：

```text
CPU 打满：看 CPU profile 和 GOMAXPROCS。
CPU 不满但延迟高：看 block/mutex profile、trace、连接池、下游 I/O。
goroutine 数高：看 goroutine profile 是否大量 parked 或 runnable。
线程数高：看 threadcreate profile、syscall/cgo。
GC 压力大：看 heap/allocs profile、runtime/metrics。
schedtrace runqueue 高：看是否无界 fan-out 或任务过细。
```

面试里可以这样答：

```text
GMP 相关性能瓶颈不固定。CPU-bound 场景受 GOMAXPROCS 和 runnable 队列影响；goroutine 太多会带来内存和 GC 压力；锁和 channel 热点会造成频繁 park/unpark；阻塞 syscall/cgo 会增加线程；网络慢会让大量 goroutine 等待。定位时先区分 CPU 忙、CPU 空但延迟高、goroutine 数高、线程数高，再用 CPU、heap、block、mutex、threadcreate profile 和 trace。
```

一句话：不要把所有并发慢都叫 scheduler 慢，先看 goroutine 是在跑、在等、在抢锁，还是卡在 I/O。

## Q057. GMP scheduler 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**如果是在 runtime 层实现或学习一个简化版 GMP，测试重点和应用层完全不同。应用层测试只能验证“我的代码在 Go scheduler 下行为正确”；runtime 层测试要验证调度状态机本身不丢、不重、不死锁。

correctness test 要测基本不变量。

```text
一个 runnable G 最终会被执行。
一个 G 同一时刻不会被两个 M 执行。
一个 P 同一时刻只属于一个 M。
G 从 running 到 waiting、runnable、syscall、dead 的状态转换合法。
M 进入阻塞 syscall 时会释放 P。
syscall 返回后能重新获取 P 或进入可运行队列。
park 的 G 只有被 ready 后才能重新运行。
timer 到期和 netpoll ready 不会丢唤醒。
stop-the-world 或 GC safepoint 时，运行中 G 能进入可扫描状态。
```

对应用代码来说，correctness test 应该避免依赖调度顺序。比如不要写“goroutine A 一定先跑”，而要用 channel 或 WaitGroup 建立顺序。`proc.go` 里针对 race detector 会随机化部分调度决策，就是为了暴露这类隐性假设。

stress test 要测极端交错和资源边界。

```text
大量 goroutine 同时创建、阻塞、唤醒、退出。
GOMAXPROCS 在 1、2、多核之间切换。
大量 channel send/receive、select、mutex、timer。
大量 syscall/cgo 进入和返回。
netpoll readiness 和 timer 同时到来。
GC、抢占、stack growth 与调度同时发生。
工作队列满、global run queue 有积压、work stealing 高频发生。
```

命令层面可以组合这些维度：

```bash
go test -race -count=100 -cpu=1,2,8 ./...
GODEBUG=schedtrace=1000,scheddetail=1 go test ./...
go test -run TestX -trace trace.out ./...
```

应用层 stress test 则要覆盖高并发请求、超时、取消、下游慢、重试、shutdown，观察 goroutine 数和 p99 是否收敛。

benchmark 要测调度成本和业务可见成本。

runtime 层可以测：

```text
goroutine 创建/退出成本。
park/unpark 成本。
channel ping-pong 成本。
run queue push/pop 成本。
work stealing 成本。
syscall handoff 成本。
netpoll 唤醒到 goroutine 执行的延迟。
```

应用层可以测：

```text
固定 worker pool vs 无界 goroutine 的吞吐和 p99。
不同 GOMAXPROCS 下 CPU-bound 任务吞吐。
高并发 I/O 下 goroutine 数、线程数、heap、GC。
RunParallel 下的 ns/op、allocs/op 和尾延迟。
```

benchmark 不能只看 `ns/op`。调度相关问题经常体现在分位延迟、线程数、run queue、block time、mutex wait、GC pause、heap live。`runtime/trace` 的 task/region 可以把一次逻辑请求跨多个 goroutine 标出来，适合看“业务请求从创建到结束，中间在哪些 goroutine 上等待”。

面试里可以这样答：

```text
correctness test 测调度状态机不变量：G 不丢不重、P 独占、syscall 释放 P、park/ready 合法、timer/netpoll 不丢唤醒、GC safepoint 可达。stress test 测大量创建、阻塞、唤醒、syscall、netpoll、timer、GC、preemption 和不同 GOMAXPROCS 下的极端交错。benchmark 测 goroutine 创建、park/unpark、channel、work stealing、syscall handoff、netpoll 唤醒延迟，以及应用层吞吐、p99、线程数、heap 和 run queue。
```

一句话：GMP 的测试要把“调度状态机正确”和“高压下延迟可控”分开测。

## Q058. 如果要求从零实现一个简化版 GMP scheduler，你会先定义哪些不变量？

**回答：**我会先定义不变量，再写队列。调度器最怕的问题不是少一个优化，而是 G 被丢、被重复执行、状态转换错、P ownership 错、阻塞时不释放资源。

简化版可以先保留三个对象。

```text
G：待执行任务，带状态和函数体。
M：执行载体，可以理解为 worker thread。
P：执行令牌和本地队列，数量固定为 N。
```

先定这些不变量。

第一，G 状态单调按规则转换。一个 G 只能处在 runnable、running、waiting、syscall、dead 等状态之一。只有 runnable 的 G 能进入队列；只有 running 的 G 能执行用户函数；dead 的 G 不能再次入队。

第二，一个 G 同一时刻只能被一个 M 执行。队列 pop 必须转移 ownership，不能两个 P 同时偷到同一个 G。

第三，一个 P 同一时刻只能属于一个 M。M 想执行 Go 代码必须持有 P；M 阻塞在 syscall 时要释放 P；syscall 返回后要重新获取 P 或把 G 放回队列。

第四，所有 runnable G 都必须可达。它要么在某个 P 的 local run queue，要么在 global run queue，要么在 runnext 之类的直接槽位。不能存在“状态是 runnable，但不在任何队列”的 G。

第五，wake 不丢。等待中的 G 被 `ready` 后，必须进入某个队列，并最终被某个 M 执行。timer 到期、I/O ready、channel/mutex 唤醒都要走这条路径。

第六，park 必须释放执行权。G park 后不能继续占用 P；当前 M/P 要去找下一个 runnable G。

第七，work stealing 不能破坏 ownership。偷取时只能偷已经从源 P 队列原子提交出来的 G，不能和源 P 本地 pop 产生重复执行。

第八，公平性要有最低保证。可以不做严格公平，但不能让 global queue、timer、netpoll、某个 P 上的 G 永远没人看。比如每隔固定调度 tick 检查 global queue。

第九，抢占和安全点要可定义。简化版可以先用协作式 yield：G 主动调用 `yield()` 或在函数边界检查 preempt flag。完整 runtime 才需要异步抢占、栈扫描和 GC 安全点。

第十，调度器内部锁不能自相矛盾。全局队列锁、P 队列锁、状态 CAS 的顺序要固定。调度器代码里一旦死锁，整个程序都会卡。

一个简化伪代码可以是：

```go
type G struct {
    state GState
    fn    func()
}

type P struct {
    runq deque[*G]
}

type Scheduler struct {
    global queue[*G]
    ps     []*P
}

func (s *Scheduler) schedule(m *M) {
    p := acquireP()
    for {
        g := p.popLocal()
        if g == nil {
            g = s.global.pop()
        }
        if g == nil {
            g = stealFromOtherP(p)
        }
        if g == nil {
            parkM(m)
            continue
        }
        run(g)
    }
}
```

这个版本还缺很多真实 runtime 细节：netpoll、timer、GC、syscall handoff、stack growth、preemption、M 休眠唤醒、spinning thread 控制。面试里不用把 `proc.go` 重写一遍，重点是说清楚 ownership 和状态机。

面试里可以这样答：

```text
从零实现简化 GMP，我会先定义不变量：G 只能处于一个合法状态；一个 G 同时只能被一个 M 执行；一个 P 同时只能归一个 M；M 执行 Go 代码必须持有 P；阻塞 syscall 要释放 P；runnable G 必须在 local/global/runnext 等可达位置；ready 不能丢唤醒；park 后必须释放执行权；work stealing 不能重复取同一个 G；global queue、timer、netpoll 不能长期饥饿。先保证这些，再谈优化。
```

一句话：GMP 的本质不变量是 ownership、状态转换和 runnable work 不丢不重。

## Q059. GMP scheduler 的常见误用是什么，误用后通常会产生什么线上症状？

**回答：**GMP 是 runtime 内部机制，应用层不能直接误用它，但很容易误解它，写出让调度器背锅的代码。

第一，把 goroutine 当成无限免费。每个请求、每个 item、每次重试都无界 `go`，没有 worker pool、semaphore、context。线上症状是 goroutine 数持续升高、run queue 积压、heap 和 GC 压力上升、p99 先变差。

```go
for _, item := range items {
    go process(item) // item 很多时，无界 fan-out
}
```

第二，用 sleep 假装同步。

```go
go func() {
    initCache()
}()
time.Sleep(10 * time.Millisecond)
useCache()
```

这依赖调度时机。机器慢、CPU 忙、GOMAXPROCS 变化、race detector 调度随机化后都可能失败。症状是测试偶现失败、线上冷启动偶发空指针或读到未初始化状态。

第三，误以为 `GOMAXPROCS` 越大越好。CPU-bound 程序可能需要接近可用 CPU 的 P；I/O-bound 程序盲目调大不一定有收益。容器 CPU quota 小时，过大的 GOMAXPROCS 可能增加 OS 层抢占和延迟抖动。症状是 CPU 上下文切换高，p99 不降反升。

第四，忙等和 `select default` 空转。这样的 goroutine 一直 runnable，占住 P，让真正工作排队：

```go
for {
    select {
    case v := <-ch:
        handle(v)
    default:
    }
}
```

症状是 CPU 打满，CPU profile 里是循环或 atomic 轮询，业务吞吐却没有对应提升。

第五，阻塞 cgo/syscall 没有 deadline。runtime 可以释放 P，但 OS thread 和外部资源仍被占着。症状是线程数上升、FD 或连接数异常、shutdown 卡住。

第六，滥用 `runtime.LockOSThread`。该 API 只适合依赖线程状态的 OS/C 库调用。忘记 unlock、在线程状态被污染后 unlock，都会让调度和线程复用变危险。症状是 threadcreate profile 异常、线程数高、某些请求只在特定环境卡住。

第七，以为 scheduler 会保证业务公平。比如一个租户提交大量 CPU-bound 任务，另一个租户请求变慢。GMP 不知道租户、优先级、配额。症状是同进程内不同业务互相影响，p99 抖动，热点租户拖慢全局。

第八，忽略取消。GMP 能抢占 goroutine，但不会替你停止过期请求。旧请求不检查 context，重试后新旧一起跑。症状是客户端已经超时，服务端仍在烧 CPU 或占数据库连接。

第九，把 pprof 里的 goroutine 多直接归因于 scheduler。大量 goroutine 可能只是都在等下游 I/O、锁、channel 或定时器。症状需要从 goroutine profile 的栈看，而不是只看数量。

面试里可以这样答：

```text
GMP 常见“误用”其实是误解：认为 goroutine 无限免费；用 sleep 依赖调度顺序；盲目调 GOMAXPROCS；写 busy loop 占住 P；阻塞 cgo/syscall 没有 deadline；滥用 LockOSThread；指望 scheduler 做业务公平；请求超时后不检查 context。线上症状包括 goroutine 数和 run queue 上升、线程数异常、CPU 空转或打满、heap/GC 压力变大、p99 抖动、shutdown 卡住。
```

一句话：大多数 GMP 问题不是 runtime 不会调度，而是应用把无界、不可取消、不可观测的工作交给了 runtime。

## Q060. GMP scheduler 在单机和分布式环境中的语义有什么差异？

**回答：**GMP scheduler 只有单机、单进程语义。它调度的是当前 Go 进程里的 goroutine，不知道别的进程、别的节点、消息队列、数据库事务，也不知道一次分布式请求跨了多少服务。

单机里，GMP 的语义边界比较清楚。

```text
go f() 创建 G。
G 进入本进程调度队列。
M 持有 P 后执行 G。
G 阻塞时 park 或进入 syscall。
G ready 后重新入队。
进程退出时所有 G 消失。
```

你可以用 `runtime.NumGoroutine`、goroutine profile、trace、schedtrace、block/mutex profile 观察这个进程的调度情况。`GOMAXPROCS` 控制这个进程同一时刻执行 Go 用户代码的并行度上限。

分布式环境里，每个 Go 进程都有自己的 GMP。服务 A 的 scheduler 不知道服务 B 的 goroutine 队列，也不会把 A 的 goroutine 迁移到 B。跨服务的“调度”靠 RPC、消息队列、负载均衡、服务发现、Kubernetes、任务系统完成。

差异主要有几处。

第一，没有全局公平性。每个服务只在自己的进程内调度 goroutine。一个请求在服务 A 很快被调度，不代表服务 B、C 也有空闲 CPU、连接池、worker。

第二，`GOMAXPROCS` 是每进程配置。容器里每个副本都要考虑 CPU quota。你扩副本数和调 `GOMAXPROCS` 是两层并行度：一个是进程内，一个是集群内。

第三，context 取消要靠协议传播。本地 cancel 只能让本进程 goroutine 看到 `ctx.Done()`；远端是否停止，要看 RPC 框架、server handler、下游调用是否继续传递 context。

第四，重试会跨进程放大。单进程里重试可能只是多几个 goroutine；分布式里一次重试可能多打一次下游、重复写数据库、重复占用队列。GMP 不知道请求幂等，也不会合并重复工作。

第五，观察要跨服务。单机看 trace 就能看到本进程 goroutine 的调度时间线；分布式要把 OpenTelemetry trace、request id、服务端 runtime metrics、队列指标、连接池指标放在一起。一个服务的 scheduler 健康，不代表整条链路健康。

第六，故障隔离不同。单机里 goroutine 爆炸拖垮一个进程；分布式里一个服务的无界 fan-out 可能把下游也拖垮。调度问题会变成级联故障。

第七，数据 locality 不归 GMP 管。GMP 不知道哪个节点有数据、哪个 shard 更近、哪个 GPU 空闲。分布式调度要考虑 locality、租约、fencing、幂等、重试预算、背压，这些都在 runtime 之外。

面试里可以这样答：

```text
GMP 只有单进程语义：它把当前进程里的 G 调度到 M/P 上，不跨进程、不跨机器、不做全局公平。分布式环境中每个服务副本都有自己的 GMP，跨服务调度靠 RPC、队列、负载均衡、Kubernetes 或任务系统。context、deadline、重试、幂等、背压和数据 locality 都要在协议层处理。不能把 GMP 当成分布式 scheduler。
```

一句话：GMP 解决的是单个 Go 进程怎么跑 goroutine，分布式系统解决的是多个进程怎么协同工作。

## Q061. channel close 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**`close(ch)` 的核心目标是表达“这个 channel 不会再有新的发送”，并唤醒正在等待接收的 goroutine。它主要是正确性语义，不是性能优化手段，也不是资源释放函数。

Go specification 对 `close` 的定义很具体：对 channel `ch` 调用内置函数 `close(ch)`，会记录 no more values will be sent on the channel。关闭后，已经发送到缓冲区里的值仍然可以被接收；这些值都收完后，后续 receive 会无阻塞返回元素类型的零值。向已关闭 channel 发送、关闭已关闭 channel、关闭 nil channel，都会 panic。

这个语义解决的是结束信号和广播问题。

第一，它让 receiver 知道数据流结束了。最常见写法是：

```go
for v := range ch {
    handle(v)
}
```

`range ch` 会一直接收，直到 channel 被关闭且缓冲值被读完。没有 close，receiver 不知道“暂时没值”和“永远不会再有值”的区别。

第二，它可以作为广播取消信号。Go blog 的 pipeline/cancellation 文章就用关闭 `done` channel 来通知未知数量的 upstream goroutine 停止发送，因为从 closed channel receive 会立刻返回：

```go
done := make(chan struct{})
defer close(done)

select {
case out <- v:
case <-done:
    return
}
```

这里 channel 里没有真正传数据，`close(done)` 只是广播一个事件。`struct{}` 只是说明不关心值。

第三，它提供 happens-before 边。Go memory model 说，channel close synchronized before 因为 channel 已关闭而返回零值的 receive。也就是说，close 前发生的写入，可以被观察到关闭的 goroutine 正确看到：

```go
var ready bool
done := make(chan struct{})

go func() {
    ready = true
    close(done)
}()

<-done
fmt.Println(ready) // 有同步保证
```

这里不是因为 `ready` 特殊，而是 `close(done)` 与 `<-done` 建立了同步关系。注意这个保证针对“因为关闭而返回的 receive”；如果 buffered channel 里还有旧值，receiver 先读到的是旧发送对应的同步关系。

从分类看：

```text
正确性：最主要。表达生产结束、取消广播、同步可见性。
性能：不是主要目标。close 是 O(等待者数量) 的唤醒，不能拿来当高频事件总线。
安全性：间接相关。避免 goroutine 永久阻塞可以减少资源耗尽风险。
可维护性：很重要。明确谁负责 close，能让并发 ownership 更清楚。
```

面试里可以这样答：

```text
channel close 的目标是表达“不会再发送新值”，并让接收方能观察到数据流结束。它主要解决正确性问题：receiver 可以退出 range，done channel 可以广播取消，close 和观察到关闭的 receive 之间还有 happens-before。它不是释放 channel 内存，也不是让 sender 停止的魔法；向 closed channel send、重复 close、close nil channel 都会 panic。
```

一句话：`close` 是结束信号和同步事件，不是资源释放 API。

## Q062. channel close 的典型适用场景和不适用场景分别是什么？

**回答：**`close` 适合表达“生产方已经结束”或“全局取消信号已经发出”。它不适合表达单个消息，也不适合在多发送方没有协调的情况下随手调用。

典型适用场景有几类。

第一，单 producer 通知多个 receiver：数据流结束。

```go
func produce(nums []int) <-chan int {
    out := make(chan int)
    go func() {
        defer close(out)
        for _, n := range nums {
            out <- n
        }
    }()
    return out
}

for n := range produce([]int{1, 2, 3}) {
    fmt.Println(n)
}
```

发送方负责关闭，因为只有发送方知道什么时候不会再发送。receiver 不应该 close 这个 channel。

第二，fan-in 输出关闭。多个 worker 发送到同一个 output channel 时，不能让任意 worker close output。应该由一个 coordinator 等所有 sender 结束后关闭：

```go
out := make(chan Result)
var wg sync.WaitGroup

for _, w := range workers {
    wg.Add(1)
    go func(w Worker) {
        defer wg.Done()
        for r := range w.Results() {
            out <- r
        }
    }(w)
}

go func() {
    wg.Wait()
    close(out)
}()
```

第三，done channel 广播取消。关闭一个只读事件 channel 可以唤醒任意数量的 waiter：

```go
done := make(chan struct{})

go func() {
    <-done
    cleanup()
}()

close(done)
```

这个模式适合进程内组件。跨 API、跨请求、跨服务时更常用 `context.Context`，因为它能携带 deadline、取消原因和 request-scoped value。

第四，一次性状态发布。比如初始化完成、后台 goroutine 已退出、cache 已构建。close 可以比发送一个值更适合，因为所有等待者都能被唤醒：

```go
ready := make(chan struct{})

go func() {
    buildIndex()
    close(ready)
}()

<-ready
```

不适用场景也很常见。

第一，不要 close 只是为了“释放 channel”。channel 没有必须手动 close 才能被 GC 的要求。只要没有引用，channel 会被回收。close 是语义，不是 free。

第二，不要让 receiver 关闭数据 channel。receiver 通常不知道是否还有 sender。receiver close 后，其他 sender 再 send 会 panic。

第三，多 producer 没有统一 owner 时不要直接 close。要么用 WaitGroup 等全部 sender 结束后由 coordinator close，要么用单独的 done/context 做取消。

第四，不要用 close 表示每个事件。close 只能发生一次。需要多次通知，用发送值、Cond、channel of events、context 或其他同步结构。

第五，不要 close receive-only channel。语言层面不允许对 receive-only channel 调用 close，因为它没有发送权限。

第六，不要靠 recover 兜底重复 close。`safeClose` 把 panic 吞掉只会掩盖 ownership 错误，数据流可能已经坏了。

面试里可以这样答：

```text
channel close 适合生产方告诉接收方“没有新值了”，也适合 done channel 的一次性广播取消。单 producer 可以 defer close(out)；多 producer 要等所有 sender 结束后由 coordinator close；多个等待者等 ready/done 时也适合 close。它不适合释放资源、不适合每个事件、不适合 receiver 随手关闭，也不适合没有 owner 的多 sender 直接 close。
```

一句话：谁能证明“以后不会再 send”，谁才有资格 close。

## Q063. channel close 和相近概念最容易混淆的边界在哪里？

**回答：**`close` 最容易和 send 零值、context cancel、关闭资源、关闭网络连接、nil channel、drain channel、GC 回收混在一起。

第一，close 不等于发送一个零值。

```go
ch <- 0
close(ch)
```

接收方可以用 comma-ok 区分：

```go
v, ok := <-ch
if !ok {
    // channel closed and buffer drained
}
```

如果不用 `ok`，`0` 可能是真实数据，也可能是 closed channel 返回的零值。元素类型是 `*T`、`error`、`bool` 时更容易踩坑。

第二，close 不等于 context cancel。`close(done)` 是进程内广播事件；`context.Context` 还包含 deadline、Err、Cause、跨 API 传递约定。库函数一般应该接收 context，而不是要求调用方传一个自定义 done channel。

第三，close channel 不会关闭 channel 里的资源。比如 channel 里传的是 `*os.File`、`net.Conn`、`io.Closer`，关闭 channel 只表示不会再传新对象，不会自动调用对象的 `Close`：

```go
close(files) // 不会关闭已经发送的 *os.File
```

资源释放要由 owner 明确处理。

第四，close 不会清空缓冲区。buffered channel 关闭后，receiver 会先读完已缓冲的值，再得到零值和 `ok=false`：

```go
ch := make(chan int, 2)
ch <- 1
ch <- 2
close(ch)

fmt.Println(<-ch) // 1
fmt.Println(<-ch) // 2
v, ok := <-ch     // 0, false
```

第五，nil channel 和 closed channel 完全不同。nil channel 的 send/receive 永远阻塞；closed channel 的 receive 立即返回，send panic。select 中把 channel 设为 nil 可以禁用某个 case；close 不是禁用 case，而是让 receive case 永远 ready。

第六，drain 和 close 不是一回事。drain 是把 channel 里的值读完；close 是声明不会再发送。你可以 close 后 drain，也可以在知道 sender 都结束后 drain。但 receiver 不能靠 drain 判断未来不会再 send，除非 channel 已经 close。

第七，close 和 GC 回收无关。一个没 close 的 channel 只要不可达，也会被 GC。一个已经 close 的 channel 只要还被引用，也不会消失。

第八，close 不等于 Wait。close(done) 只是发信号，不代表所有 goroutine 都退出。要证明退出，需要 WaitGroup、errgroup、ack channel 或其他等待机制。

面试里可以这样答：

```text
channel close 的边界是“不会再 send”。它不是发送零值，必须用 v, ok 区分；不是 context cancel，缺少 deadline 和 Err/Cause；不是关闭 channel 里的资源；不会清空 buffer；也不是等待 goroutine 退出。nil channel 会阻塞，closed channel 的 receive 立即返回、send panic。close 是信号，资源释放和等待退出要另写协议。
```

一句话：close 只改变 channel 状态，不替你处理值、资源、取消原因和 goroutine 生命周期。

## Q064. channel close 在高并发场景下可能出现哪些隐藏问题？

**回答：**高并发下，`close` 的主要风险不是它慢，而是 ownership 不清楚。并发 sender、receiver、closer 一多，panic、数据丢失、误唤醒、资源泄漏都会出现。

第一，多 sender 竞争 close。只要有一个 sender 在 close 后继续 send，就会 panic：

```go
func worker(out chan<- int, done <-chan struct{}) {
    for {
        select {
        case out <- next():
        case <-done:
            return
        }
    }
}
```

如果另一个 goroutine 直接 `close(out)`，这里的 `out <- next()` 可能 panic。解决方式通常是：关闭 done 通知 sender 退出，等 sender 全部退出后，再由唯一 owner close out。

第二，重复 close。两个 goroutine 都觉得自己负责结束，就会 `close of closed channel`。`sync.Once` 可以避免重复 close，但它只能解决 panic，不能解决“谁负责 close”的设计问题。

```go
type Broadcaster struct {
    done chan struct{}
    once sync.Once
}

func (b *Broadcaster) Stop() {
    b.once.Do(func() { close(b.done) })
}
```

第三，receiver 被 close 唤醒后误读零值。如果 receiver 没检查 `ok`，高并发下可能把零值当真实任务处理：

```go
job := <-jobs
process(job) // jobs closed 时，job 是零值
```

应该写：

```go
job, ok := <-jobs
if !ok {
    return
}
process(job)
```

第四，select 中 closed channel 会一直 ready。如果循环里不把 closed channel 置 nil，可能造成空转或重复处理：

```go
for ch1 != nil || ch2 != nil {
    select {
    case v, ok := <-ch1:
        if !ok {
            ch1 = nil
            continue
        }
        use(v)
    case v, ok := <-ch2:
        if !ok {
            ch2 = nil
            continue
        }
        use(v)
    }
}
```

第五，close 唤醒大量 goroutine，会制造瞬时调度压力。关闭一个被很多 goroutine 等待的 done channel 是常见做法，但如果有几十万 waiter，它们会同时变成 runnable，随后争 P、争锁、争下游资源。这个问题通常要靠限流和分批收尾解决。

第六，buffered channel 的尾部值容易被忽略。close 后 range 会读完缓冲值；但如果代码在看到 done 后直接 return，可能丢掉已经排队的结果。到底要“尽快退出”还是“drain 已有结果”，必须在协议里写清楚。

第七，close 和 send 的内存同步容易误解。memory model 保证 close 同步到观察到关闭的 receive，但不代表所有 buffered 值之后的处理顺序都按你想的来。每个 send/receive 和 close/closed receive 是不同同步边。

第八，recover 包裹 send/close 会掩盖线上症状。panic 消失了，协议错误还在。你可能看到结果丢失、任务少处理、goroutine 卡住，却没有明显 panic。

面试里可以这样答：

```text
高并发下 channel close 的隐藏问题主要是 ownership 不清：多 sender 直接 close 会和 send 竞争，重复 close 会 panic，receiver 不检查 ok 会把零值当任务，select 里 closed channel 会一直 ready，关闭 done 可能瞬间唤醒大量 goroutine。buffered channel 还要明确 close 后是 drain 已有值，还是收到取消就丢弃。解决思路是唯一 closer、发送方负责关闭、done/context 负责取消、WaitGroup/errgroup 负责等待。
```

一句话：高并发 close 的难点不是语法，而是关闭权和退出协议。

## Q065. channel close 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**这些场景会把 `close` 的一次性语义放大。成功路径里 close 对了，不代表超时、重试、panic、shutdown 路径也对。

第一，panic 路径要保证输出 channel 被关闭。producer 如果 panic 或早返回，而输出 channel 没关闭，下游 `range` 会永远等：

```go
func produce(out chan<- Item) {
    defer close(out)
    for _, item := range items {
        out <- item
    }
}
```

如果 producer 内部还会启动子 goroutine，单纯 `defer close(out)` 不够。必须等所有 sender 结束后再 close，否则子 goroutine 还在 send 时会 panic。

第二，重启不会保留 channel 状态。channel close 是进程内状态，进程崩溃后所有 channel 都没了。分布式任务不能靠“我 close 了 channel”证明远端知道结束。重启恢复要靠持久化状态、消息 ack、幂等键、lease 或外部协调。

第三，timeout 不等于 close 数据 channel。请求超时时，通常应该 cancel context 或 close done channel，让 sender 停止；不应该由 receiver 直接 close 数据 channel：

```go
select {
case v := <-results:
    return v, nil
case <-ctx.Done():
    return zero, ctx.Err()
}
```

这里调用方超时后，如果需要让后台 goroutine 停止，应该把 `ctx` 传给 producer。receiver 关闭 `results` 很危险，因为 producer 可能仍然在发送。

第四，重试会制造旧 sender。第一次尝试超时后，第二次尝试启动新的 sender。如果旧 sender 仍然往同一个 channel 发，而新逻辑已经 close 了 channel，就会 panic。每次尝试最好有自己的 result channel 和 context：

```go
ctx, cancel := context.WithTimeout(parent, timeout)
resultCh := make(chan Result, 1)
go func() {
    defer cancel()
    resultCh <- call(ctx)
}()
```

这里还要处理 `ctx.Done()`，避免 goroutine 卡在发送结果上。buffer 为 1 可以让调用方超时返回后，goroutine 至少能写入结果并退出；但这只适合单结果场景，不能当通用补丁。

第五，shutdown 时 close 顺序很容易错。正确顺序常见是：

```text
1. 停止接收新任务。
2. 发出取消信号或 close input。
3. 等所有 worker/sender 退出。
4. close output。
5. 等下游 drain 或退出。
```

如果先 close output，再等 worker，就可能 send on closed channel。先等 worker，但 worker 还在等 input，又会 deadlock。

第六，重复 Stop 要有语义。线上 shutdown 可能由 signal、健康检查失败、父 context 取消、错误返回同时触发。Stop 内部如果直接 close channel，第二次调用会 panic。`sync.Once` 很常见，但要配合状态机：

```go
func (s *Server) Stop() {
    s.once.Do(func() {
        close(s.done)
    })
}
```

第七，close 不能表达失败原因。超时、取消、上游错误、正常结束都可能让 channel 关闭。receiver 如果需要知道原因，要单独返回 error、使用 `errgroup`、context Cause，或把结果结构里带 error。

面试里可以这样答：

```text
崩溃和重启会清掉进程内 channel 状态，close 不能作为持久化完成信号。超时时不要让 receiver close 数据 channel，而要通过 context/done 通知 producer 停止。重试时每次尝试最好有独立 channel 和取消路径，避免旧 sender 向新关闭的 channel send。shutdown 要按顺序停新任务、取消、等 sender 退出、再 close output。重复 Stop 要用 Once 或状态机，但不能用 Once 掩盖 ownership 错。
```

一句话：close 是进程内一次性事件，遇到超时、重试、重启时必须配合 context、等待和持久化协议。

## Q066. channel close 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**`close` 本身通常不是主要性能瓶颈。真正的成本来自等待队列唤醒、调度抖动、锁竞争和错误协议造成的 goroutine 堆积。

从 runtime 源码看，`closechan` 会拿 channel 的锁，标记 `closed = 1`，释放所有等待接收的 goroutine，再释放所有等待发送的 goroutine。等待发送者会被唤醒后 panic。最后 runtime 在释放 channel lock 后把这些 goroutine ready。也就是说，close 的成本和正在等待这个 channel 的 goroutine 数量有关。

第一，CPU 成本主要来自唤醒后的工作，不是 close 那一行。关闭 done channel 后，几千个 goroutine 同时醒来，随后可能都去抢锁、写日志、释放连接、更新指标。CPU profile 看到的热点可能在 cleanup，而不是 runtime close。

第二，内存成本来自 channel 使用模式。channel 里缓冲的对象、等待 goroutine 的栈、sudog 结构、被栈引用的对象都会占内存。close 不会清空业务对象；receiver 是否 drain 决定缓冲值什么时候被释放。

第三，锁竞争可能出现在两个层面。channel 内部有锁，close、send、receive 都要协调；更常见的是 close 唤醒 goroutine 后，它们争应用层锁，比如全局 map、连接池、日志锁。

第四，I/O 和网络不是 close 的直接瓶颈，但 close 常用来停止 I/O goroutine。如果 goroutine 正卡在不支持取消的 I/O 调用上，close(done) 不会打断它。你还要关闭连接、设置 deadline，或调用支持 context 的 API。

第五，select 空转会制造 CPU 问题。closed channel 在 select 里永远 ready，如果循环里不把它设成 nil，就会反复选择这个 case：

```go
for {
    select {
    case <-done:
        // 如果不 return，也不把 done 置 nil，这个 case 会一直 ready
    default:
    }
}
```

第六，高频 close 不是合理设计。channel 只能 close 一次。如果你的逻辑需要高频通知，应该发送事件、复用 worker、用 Cond、atomic 状态、timer，或者重新设计队列。频繁创建 channel 再 close，可能带来分配和 GC 压力。

排查时可以这样看：

```text
goroutine profile：多少 goroutine 卡在 send/receive/select。
block profile：channel 阻塞时间在哪里。
mutex profile：close 后 cleanup 是否争锁。
heap profile：buffered channel 是否保留大对象。
trace：close 后是否大量 goroutine 同时 runnable。
CPU profile：是否有 closed channel select 空转或 cleanup 风暴。
```

面试里可以这样答：

```text
channel close 通常不是 I/O 或网络瓶颈，它的直接成本主要和等待者数量有关：runtime 要拿 channel lock，标记 closed，唤醒 recvq 和 sendq 上的 goroutine。真正的线上瓶颈常出现在 close 之后：大量 goroutine 同时 runnable、cleanup 抢锁、buffered channel 保留对象、closed channel 在 select 中空转，或者 I/O goroutine 不响应 done。排查要看 goroutine、block、mutex、heap profile 和 trace。
```

一句话：`close` 的成本小，close 唤醒的一整批后续行为才可能贵。

## Q067. channel close 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**`close` 相关测试不能只测“函数返回”。要把 channel 的结束协议当成行为来测：谁关闭、何时关闭、关闭后还能读什么、谁会退出、是否会 panic。

correctness test 主要测语义和 ownership。

```text
producer 正常结束后 close output。
receiver 用 range 能退出。
buffered channel close 后，receiver 能读完已有值，再得到 ok=false。
done close 后，所有等待者能退出。
发送方不会在 close 后 send。
多 sender 由 coordinator close，不由 worker close。
Stop 重复调用不会 panic，或者 API 明确禁止重复调用。
错误路径、早返回、panic 恢复路径也会关闭或取消。
close 前写入的状态，对观察到关闭的 receiver 可见。
```

一个基础测试可以这样写：

```go
func TestCloseDrainsBufferedValues(t *testing.T) {
    ch := make(chan int, 2)
    ch <- 1
    ch <- 2
    close(ch)

    got := []int{}
    for v := range ch {
        got = append(got, v)
    }
    if !reflect.DeepEqual(got, []int{1, 2}) {
        t.Fatalf("got %v", got)
    }
}
```

取消广播测试要等 goroutine 真退出：

```go
func TestDoneCloseStopsWorker(t *testing.T) {
    done := make(chan struct{})
    exited := make(chan struct{})

    go func() {
        defer close(exited)
        <-done
    }()

    close(done)

    select {
    case <-exited:
    case <-time.After(time.Second):
        t.Fatal("worker did not exit")
    }
}
```

stress test 要测并发交错。

```text
很多 receiver 同时等 done，然后 close。
很多 sender 发送，coordinator 等待后 close output。
receiver 提前退出，producer 通过 done 停止。
Stop、Submit、Close 并发调用。
close 与 context timeout、retry、shutdown 交错。
不同 GOMAXPROCS、-race、-count 下重复跑。
```

命令可以这样组合：

```bash
go test -race -run TestChannelClose -count=100 ./...
go test -run TestChannelClose -count=1000 -cpu=1,2,8 ./...
```

如果测试的目标是“不会 send on closed channel”，不要靠 recover 判断成功。更好的设计是让协议保证 close 发生在所有 sender 退出后。recover 只适合测试故意验证 panic 语义：

```go
func TestSendClosedChannelPanics(t *testing.T) {
    ch := make(chan int)
    close(ch)

    defer func() {
        if recover() == nil {
            t.Fatal("expected panic")
        }
    }()
    ch <- 1
}
```

benchmark 要测使用模式，不是单独测 `close`。单次 close 太便宜，孤立数字意义不大。更有用的是：

```text
关闭 done 唤醒 N 个 goroutine 的延迟。
fan-in 完成后 close output 的吞吐。
buffered channel drain 成本。
select 中 done channel 对热路径的影响。
每操作创建 channel+close 的分配成本。
```

可以在 benchmark 里上报等待者规模和分配：

```go
func BenchmarkCloseBroadcast(b *testing.B) {
    for n := 1; n <= 1024; n *= 4 {
        b.Run(fmt.Sprintf("waiters=%d", n), func(b *testing.B) {
            b.ReportAllocs()
            for i := 0; i < b.N; i++ {
                done := make(chan struct{})
                var wg sync.WaitGroup
                wg.Add(n)
                for j := 0; j < n; j++ {
                    go func() {
                        defer wg.Done()
                        <-done
                    }()
                }
                close(done)
                wg.Wait()
            }
        })
    }
}
```

这个 benchmark 测的是广播唤醒模式的成本，不代表所有 close 的成本。结果还会受 goroutine 创建影响；如果要单独看唤醒成本，需要把 setup 和 measurement 分开设计。

面试里可以这样答：

```text
correctness test 要测 close 后的语言语义和 ownership：range 能退出、buffered 值能 drain、ok=false 正确、done 能广播、不会 close 后 send、多 sender 由 coordinator close、错误路径也能收尾。stress test 要测 Stop/Submit/Close 并发、receiver 早退、timeout/retry/shutdown 交错，并配合 -race、-count、-cpu。benchmark 不该只测单次 close，而要测关闭 done 唤醒 N 个 goroutine、fan-in close、drain、select 热路径和 channel 创建分配成本。
```

一句话：channel close 的测试要把“结束协议”当成一等行为，而不是只测有没有返回值。

## Q068. 如果要求从零实现一个简化版 channel close，你会先定义哪些不变量？

**回答：**我会先定义 channel 状态机，而不是直接写队列。`close` 的难点在于一次性状态转换、等待队列唤醒、send/receive/close 并发互斥，以及内存可见性。

简化版 channel 可以先有这些字段：

```go
type Channel[T any] struct {
    mu      sync.Mutex
    closed  bool
    buf     []T
    recvq   queue[*waiter[T]]
    sendq   queue[*waiter[T]]
}
```

先定不变量。

第一，`closed` 只能从 false 变 true，一次性转换，不能回到 false。重复 close 必须 panic 或返回明确定义的错误。Go 语言里的 `close` 选择 panic。

第二，close nil channel panic。简化版如果有 nil 指针 receiver，也要定义清楚；Go spec 明确 close nil channel 会 panic。

第三，close 后不能再 send。send 看到 closed 必须 panic。已经阻塞在 sendq 里的 sender，在 close 时要被唤醒，并在恢复后 panic。runtime `closechan` 源码就是释放所有 writers，并注明 they will panic。

第四，close 不丢 buffered values。已经在 buffer 里的值必须仍然按原顺序被 receiver 读到。只有 buffer 空了，receive 才返回零值和 `ok=false`。

第五，close 要唤醒所有 blocked receivers。它们应该得到零值和 `ok=false`，或者在 buffer 仍有值时按 buffer 规则接收值。简化实现可以规定 close 时先让已有 buffer 继续由后续 receive drain，已经阻塞的 receiver 如果 buffer 为空就返回零值。

第六，receive 的返回值要能区分真实零值和关闭。内部 API 应该返回 `(value, ok)`，而不是只返回 value。

第七，close 和 receive 要建立 happens-before。close 前的写入，对观察到 `ok=false` 的 receiver 可见。简化实现里可以用 mutex unlock/lock 或 condition variable 的同步来保证；真实 Go runtime 还要配合 race detector 和 runtime 原语。

第八，close 时不能持锁执行用户代码。唤醒等待者可以在内部完成，但不能在 channel lock 下调用外部回调，否则容易死锁。Go runtime 的 `closechan` 会先收集 goroutine，释放 channel lock 后再 `goready`。

第九，send、receive、close 的互斥规则要固定。任何检查 `closed` 和修改队列/buffer 的操作都要在同一把锁或同一套原子协议下完成，不能先无锁检查再加锁操作。

第十，select 支持需要可轮询状态。简化版如果支持 select，要能判断 receive 是否 ready：buffer 非空、closed、或有 sender；send 是否 ready：未 closed 且有 receiver 或 buffer 有空间。closed receive 永远 ready；closed send 不是 ready，而是 panic。

伪代码可以这样写：

```go
func (c *Channel[T]) Close() {
    if c == nil {
        panic("close of nil channel")
    }

    c.mu.Lock()
    if c.closed {
        c.mu.Unlock()
        panic("close of closed channel")
    }
    c.closed = true

    receivers := c.recvq.popAll()
    senders := c.sendq.popAll()
    c.mu.Unlock()

    for _, r := range receivers {
        r.wake(zero[T](), false)
    }
    for _, s := range senders {
        s.wakePanic("send on closed channel")
    }
}

func (c *Channel[T]) Recv() (T, bool) {
    c.mu.Lock()
    if len(c.buf) > 0 {
        v := c.buf[0]
        c.buf = c.buf[1:]
        c.mu.Unlock()
        return v, true
    }
    if c.closed {
        c.mu.Unlock()
        var zero T
        return zero, false
    }
    // enqueue receiver and park
    c.mu.Unlock()
    ...
}
```

这只是教学版。真实 runtime 要处理 sudog、select、race detector、typedmemclr、GC write barrier、timer channel、阻塞和唤醒的调度细节。面试里不需要复刻 `runtime/chan.go`，但要把不变量讲清楚。

面试里可以这样答：

```text
从零实现简化版 channel close，我会先定不变量：closed 只能从 false 到 true；nil close、重复 close panic；close 后 send panic；已缓冲值不能丢；buffer drain 完后 receive 返回零值和 ok=false；close 要唤醒所有等待 receiver，也要唤醒等待 sender 并让它们 panic；close 与观察到关闭的 receive 之间要有同步可见性；检查 closed 和修改队列必须在同一同步边界内；唤醒等待者时不要持锁执行外部逻辑。
```

一句话：channel close 的实现核心是一次性状态转换，加上等待队列的正确唤醒和严格的 send/receive 边界。

## 参考资料

- Go FAQ: Why goroutines instead of threads? https://go.dev/doc/faq#goroutines
- Go runtime HACKING: Gs, Ms, Ps 和 scheduler structures https://go.dev/src/runtime/HACKING
- Go runtime `proc.go`: scheduler、worker parking/unparking、`runqsteal` 源码注释 https://go.dev/src/runtime/proc.go
- Go runtime `chan.go`: `closechan`、`chanrecv`、`chansend` 源码语义 https://go.dev/src/runtime/chan.go
- Go language specification: channel types、send statements、receive operator、close、select https://go.dev/ref/spec
- The Go Memory Model: channel communication、goroutine creation、data race 定义、Once 同步语义 https://go.dev/ref/mem
- `context` package documentation: cancellation、deadline、CancelFunc 资源释放和 goroutine leak 示例 https://pkg.go.dev/context
- `runtime/pprof` package documentation: goroutine、heap、allocs、block、mutex profiles https://pkg.go.dev/runtime/pprof
- `net/http/pprof` package documentation: HTTP profiling endpoints、CPU/heap/block/mutex/trace 获取方式 https://pkg.go.dev/net/http/pprof
- Data Race Detector: `go test -race`、`go run -race`、`GORACE` 选项和运行时覆盖边界 https://go.dev/doc/articles/race_detector
- Go blog: Share Memory By Communicating https://go.dev/blog/codelab-share
- Go blog: Go Concurrency Patterns: Pipelines and cancellation https://go.dev/blog/pipelines
- Go blog: Contexts and structs https://go.dev/blog/context-and-structs
- Go blog: Defer, Panic, and Recover https://go.dev/blog/defer-panic-and-recover
- Go 1.14 Release Notes: defer performance improvements https://go.dev/doc/go1.14
- Go 1.22 Release Notes: loop variable per-iteration semantics https://go.dev/doc/go1.22
- Go blog: Fixing For Loops in Go 1.22 https://go.dev/blog/loopvar-preview
- Go blog: Go maps in action https://go.dev/blog/maps
- Go blog: Go Slices: usage and internals https://go.dev/blog/slices-intro
- Go FAQ: nil error、interface value 和 map 并发访问说明 https://go.dev/doc/faq
- Go blog: Working with Errors in Go 1.13 https://go.dev/blog/go1.13-errors
- `errors` package documentation: wrapping、Is、As、Join、Unwrap https://pkg.go.dev/errors
- `sync` package documentation: Mutex、RWMutex、Cond、Map、Once、WaitGroup 的同步语义 https://pkg.go.dev/sync
- `errgroup` package documentation: Group、WithContext、SetLimit、TryGo https://pkg.go.dev/golang.org/x/sync/errgroup
- Go GC guide: GOGC、memory limit、GC cost model https://go.dev/doc/gc-guide
- `runtime` package documentation: MemStats、ReadMemStats、SetBlockProfileRate、SetMutexProfileFraction、GODEBUG https://pkg.go.dev/runtime
- `runtime/metrics` package documentation: GC pause、scheduler pause、goroutine、mutex wait metrics https://pkg.go.dev/runtime/metrics
- Go Diagnostics: profiling、goroutine/block/mutex/heap profile 说明 https://go.dev/doc/diagnostics
- `runtime/trace` package documentation: execution trace、task、region 和跨 goroutine 逻辑操作观测 https://pkg.go.dev/runtime/trace
- `testing` package documentation: `RunParallel`、`FailNow`、`SkipNow`、benchmark allocation reporting https://pkg.go.dev/testing
- `cmd/compile` documentation: `-m` optimization and escape analysis diagnostics https://pkg.go.dev/cmd/compile
