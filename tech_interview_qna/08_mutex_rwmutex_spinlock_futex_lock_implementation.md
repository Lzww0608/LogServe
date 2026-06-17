# 8. Mutex、RWMutex、spinlock、futex 与锁实现原理

这一组问题讨论的是锁的语义和实现边界。面试里不要只说“加锁保证线程安全”，这句话太粗。锁真正保护的是共享状态和状态不变量；`Lock/Unlock` 只是让一段访问这些状态的代码在互斥条件下执行。

后面谈 `Mutex`、`RWMutex`、`spinlock`、`futex` 时，可以先抓住四条线：

```text
语义线：锁保护什么状态，建立什么 happens-before 关系。
性能线：锁竞争、临界区长度、cache line 抖动、阻塞/唤醒成本。
调度线：公平、饥饿、抢占、park/unpark、进入内核的时机。
工程线：锁粒度、锁顺序、能否重入、能否升级/降级、是否允许跨 goroutine unlock。
```

Go 的 `sync.Mutex` 和 `sync.RWMutex` 是很好的切入点。官方文档明确说 `Mutex` 是 mutual exclusion lock，`Lock` 在锁被占用时会阻塞，`Unlock` 和后续 `Lock` 建立同步关系；`RWMutex` 允许多个 reader 或一个 writer，并且在 writer 等待时会阻止新的 reader 继续进入，以免 writer 永远等不到。Linux 的 futex 则解释了很多用户态锁为什么“无竞争时不进内核，有竞争时才睡眠”。

## Q001. mutex 的基本语义是什么？

**回答：**

`mutex` 是 mutual exclusion 的缩写，基本语义是互斥访问：同一时刻最多只有一个执行单元持有这把锁。拿到锁的线程或 goroutine 可以访问受保护的共享状态；没拿到锁的执行单元要等待，或者在 `tryLock` 这类接口里立刻失败。

最小语义可以写成：

```text
Lock:
  尝试获得锁。
  如果锁空闲，调用方获得锁并继续执行。
  如果锁已被占用，调用方阻塞、等待或返回失败，取决于接口设计。

Unlock:
  释放锁。
  之后某个等待者可以继续获得锁。
```

以 Go 为例，`sync.Mutex` 的官方文档说得很直：`Mutex` 是互斥锁，零值是未加锁状态；`Lock` 在锁已被使用时会阻塞，直到锁可用；`Unlock` 在锁没有被锁住时调用会触发运行时错误。还有一个容易被忽略的点：Go 文档明确说，锁住的 `Mutex` 不绑定某个特定 goroutine，一个 goroutine 加锁后可以安排另一个 goroutine 解锁。

但这只是行为层面的描述。mutex 更重要的语义是同步关系。

在 Go memory model 里，对同一个 `sync.Mutex` 或 `sync.RWMutex`，前一次 `Unlock` 会 synchronizes-before 后一次成功返回的 `Lock`。这句话的含义是：前一个临界区里对共享状态的写入，在后一个临界区里可以被正确观察到。否则锁就只是在排队执行代码，不能可靠地保护内存可见性。

可以用一个简单例子说明：

```go
var (
    mu sync.Mutex
    x  int
)

func writer() {
    mu.Lock()
    x = 42
    mu.Unlock()
}

func reader() int {
    mu.Lock()
    defer mu.Unlock()
    return x
}
```

这里 `mu` 做了两件事：

```text
互斥：reader 和 writer 不会同时访问 x。
同步：writer 在 Unlock 前写入的 x，对之后成功 Lock 的 reader 可见。
```

mutex 的基本语义不包括这些东西：

```text
不保证业务操作一定快；
不保证没有死锁；
不保证公平；
不保证调用方拿锁顺序就是排队顺序；
不自动知道你想保护哪个变量；
不自动让多个锁之间有一致顺序。
```

所以面试里如果问“mutex 是什么”，不要只答“互斥锁”。可以这样说：

```text
mutex 的语义是给一组共享状态建立排他访问和内存同步边界。Lock 成功之后，调用方进入受保护区域；Unlock 释放这个边界，并让后续 Lock 能看到之前临界区内已经完成的写入。它解决的是共享状态并发访问的正确性问题，但公平性、死锁、粒度和性能要靠具体实现和使用方式处理。
```

一句话：mutex 的本质不是“挡住代码”，而是让共享状态在明确的同步边界里被排他地读写。

## Q002. mutex 保护的是代码块还是共享状态？

**回答：**

严格说，mutex 保护的是共享状态和状态不变量，不是代码块。代码块只是我们使用锁的方式。

很多人会说：

```text
这段代码加了锁，所以线程安全。
```

这个说法容易误导。真正应该问的是：

```text
这把锁保护哪些字段？
这些字段之间有什么不变量？
所有读写这些字段的路径是否都拿同一把锁？
```

比如一个队列：

```go
type Queue struct {
    mu    sync.Mutex
    items []Task
    size  int
}
```

这里 `mu` 保护的不是 `Push` 这个函数，也不是 `Pop` 这个函数，而是：

```text
items；
size；
items 和 size 之间的一致性。
```

如果 `Push` 拿锁但 `Len` 不拿锁，仍然可能出错：

```go
func (q *Queue) Push(t Task) {
    q.mu.Lock()
    defer q.mu.Unlock()
    q.items = append(q.items, t)
    q.size = len(q.items)
}

func (q *Queue) Len() int {
    return q.size // 没拿锁，仍然 race
}
```

正确思路是：任何访问受保护状态的路径都要遵守同一套锁协议。

```go
func (q *Queue) Len() int {
    q.mu.Lock()
    defer q.mu.Unlock()
    return q.size
}
```

代码块和状态之间的关系可以这样理解：

```text
锁保护的是状态；
临界区是访问这些状态的代码范围；
锁粒度决定一次保护多少状态和多少操作；
不变量决定锁必须覆盖到哪里。
```

一个更典型的例子是转账：

```go
func Transfer(from, to *Account, amount int) {
    // ...
}
```

真正要保护的不只是某个账户的 `balance` 字段，而是跨两个账户的业务不变量：

```text
from.balance 减少 amount；
to.balance 增加 amount；
总金额不变；
中间状态不能被别人观察到。
```

如果只给单个字段分别加锁，可能会暴露半完成状态。此时锁的边界要覆盖整个不变量，而不是机械地围住几行代码。

这也是为什么“把一段代码放进锁里”不一定正确：

```go
mu.Lock()
read A
mu.Unlock()

mu.Lock()
write B
mu.Unlock()
```

如果业务不变量要求 A 和 B 一起变化，这样拆锁就破坏了原子性。反过来，如果临界区里做了网络 I/O、磁盘 I/O、复杂计算，把锁持有得太久，也会拖慢所有访问同一状态的 goroutine。

面试里可以这样回答：

```text
mutex 保护的是共享状态以及这些状态之间的不变量。代码块只是临界区，是访问这些状态时必须经过的执行范围。判断一把锁用得对不对，要看所有读写同一组状态的路径是否使用同一把锁，以及锁的范围是否覆盖完整的不变量，而不是看某个函数里有没有 Lock/Unlock。
```

一句话：锁不是给代码贴标签，它是给状态和不变量划边界。

## Q003. 为什么锁粒度会影响性能和正确性？

**回答：**

锁粒度指的是一把锁覆盖多大范围的状态和操作。它会同时影响性能和正确性，因为锁既是同步边界，也是串行化边界。

粒度太粗，正确性通常更容易保证，但并发度会下降。比如：

```go
type Store struct {
    mu   sync.Mutex
    data map[string]string
}
```

所有 key 都用同一把锁：

```text
读 key=a 要拿 mu；
写 key=b 也要拿 mu；
遍历 key=c 也要拿 mu。
```

这样很简单，不容易漏保护。但如果 `data` 很热，不同 key 之间本来可以并行，现在全被串行化了。表现是：

```text
goroutine 大量阻塞在 Lock；
CPU 没打满但延迟很高；
p99 延迟被某个长临界区拖住；
读多写少场景也被单把锁卡住。
```

粒度太细，性能可能变好，也可能更糟；正确性反而更难。

例如按 key 分片：

```go
type ShardedStore struct {
    shards [64]struct {
        mu   sync.Mutex
        data map[string]string
    }
}
```

不同 shard 可以并行，锁竞争下降。但一旦操作跨多个 shard，就要处理锁顺序：

```text
move keyA from shard 3 to shard 9
```

如果一个 goroutine 先锁 3 再锁 9，另一个先锁 9 再锁 3，就可能死锁。

锁粒度影响正确性的地方主要有三类。

第一，粒度太小可能保护不完整的不变量。

```text
字段 A 和字段 B 必须一起变化；
你给 A 和 B 分别加锁；
别人可能看到 A 已更新、B 未更新的中间状态。
```

第二，细粒度锁会引入锁顺序问题。

```text
函数 f: lock A -> lock B
函数 g: lock B -> lock A
```

这就是死锁的典型形状。

第三，锁拆得太细后，调用方容易不知道该拿哪把锁。

尤其在大型代码里，状态所有权不清晰时，可能出现：

```text
有的路径拿 account.mu；
有的路径拿 user.mu；
有的路径两个都拿；
还有一条路径直接读字段。
```

这种 bug 比“单把大锁慢”更难排查。

锁粒度影响性能的地方也很直接。

```text
锁竞争：
  同一把锁等待者越多，阻塞越多。

临界区长度：
  持锁时间越长，别人等待越久。

cache line bouncing：
  多核频繁修改同一把锁的状态，会让 cache line 在 CPU 核之间来回迁移。

上下文切换：
  等锁失败后 park/unpark 或进入内核，会产生调度成本。

局部性：
  非公平锁有时吞吐高，是因为刚运行的线程更容易继续拿到锁，cache 还热。
```

所以调锁粒度不是简单地“越细越好”。正确做法是先按状态不变量分组，再用 benchmark 和 profile 看热点。

面试里可以这样答：

```text
锁粒度影响正确性，是因为锁要覆盖完整的不变量；拆得太细可能让别人看到半更新状态，还会引入多锁顺序和死锁问题。锁粒度影响性能，是因为锁会把受保护范围串行化；太粗会制造不必要的竞争，太细会增加锁操作、cache 抖动和复杂度。工程上先按状态所有权和不变量定边界，再根据 profile 拆热点，而不是先追求细锁。
```

一句话：锁粒度本质上是在“简单正确”和“可并行”之间划线，线画错了，要么慢，要么错。

## Q004. 粗粒度锁和细粒度锁的 trade-off 是什么？

**回答：**

粗粒度锁和细粒度锁的区别，不只是“锁多锁少”。它们代表两种设计取舍：粗锁把更多状态放进同一个串行化区域；细锁把状态拆开，让更多操作可以并行。

粗粒度锁的优点很实在。

```text
代码简单；
状态边界清楚；
不容易漏锁；
不容易产生多锁死锁；
调试和推理成本低；
锁操作次数少。
```

例如：

```go
type Cache struct {
    mu sync.Mutex
    m  map[string]*Entry
}
```

所有 cache 状态都由 `mu` 保护。面试、课程项目、低并发工具、控制面服务里，这种设计经常够用。它的问题是并发度低：

```text
读不同 key 也互相阻塞；
一个慢操作拖住所有操作；
热点锁让 goroutine 大量排队；
CPU 核数增加后吞吐不怎么涨。
```

细粒度锁的优点是减少不必要的串行化。

比如按 shard 加锁：

```go
type Cache struct {
    shards [64]struct {
        mu sync.Mutex
        m  map[string]*Entry
    }
}
```

不同 shard 的请求可以并行。读写分布均匀时，吞吐会更好。代价也明显：

```text
代码更复杂；
锁顺序必须固定；
跨 shard 操作难写；
不变量容易被拆坏；
锁数量多，占用更多内存；
测试覆盖要更细；
bug 通常更隐蔽。
```

粗锁和细锁的权衡可以放在一张表里：

| 维度 | 粗粒度锁 | 细粒度锁 |
| --- | --- | --- |
| 正确性推理 | 简单，状态边界集中 | 难，容易拆坏不变量 |
| 并发度 | 低，不相关操作也排队 | 高，不同分片可并行 |
| 锁开销 | Lock/Unlock 次数少 | 锁对象和操作次数多 |
| 死锁风险 | 低，常常只有一把锁 | 高，多锁顺序要严格 |
| 调试难度 | 相对低 | 高，race 和死锁更难复现 |
| 适用场景 | 控制面、低竞争、复杂不变量 | 数据面、热点明确、分片自然 |

选择时可以按这个顺序想：

```text
1. 状态不变量能不能自然拆开？
2. 访问是否真的有竞争？
3. 临界区是否长？
4. 是否有跨分片/跨对象操作？
5. profile 是否证明锁是瓶颈？
```

如果没有 profile，先用粗锁往往更稳。很多系统的锁问题不是“锁太粗”，而是临界区里做了不该做的事：

```text
持锁做网络 I/O；
持锁做磁盘 fsync；
持锁调用外部回调；
持锁执行复杂用户逻辑；
持锁等待另一个 subsystem。
```

这些问题先修掉，可能比拆成 64 把锁更有效。

细锁适合那些状态天然可分区的场景：

```text
per-key cache；
per-shard queue；
per-connection state；
per-partition offset；
每个对象有独立生命周期。
```

不适合拆的场景：

```text
多个字段共同构成一个强不变量；
操作经常跨多个对象；
状态变化必须全局原子；
团队很难维护锁顺序约定。
```

面试里可以这样回答：

```text
粗粒度锁的 trade-off 是简单、稳、容易维护，但会牺牲并发度，热点下吞吐和尾延迟会变差。细粒度锁能减少无关操作之间的竞争，适合状态天然分片、热点明确的路径，但会带来多锁顺序、死锁、漏保护和不变量拆分问题。工程上一般先用能证明正确性的粗粒度边界，再根据 profile 拆真正的热点。
```

一句话：粗锁买的是简单正确，细锁买的是并发度，账单是复杂度和死锁风险。

## Q005. RWMutex 适合什么读写比例？

**回答：**

`RWMutex` 适合读远多于写、读临界区有一定长度、并且读操作之间真的可以并行的场景。它不是“有读有写就应该用”的锁。

Go 官方文档对 `RWMutex` 的语义是：它可以被任意数量的 reader 持有，或者被一个 writer 持有。也就是说：

```text
多个 RLock 可以同时成功；
Lock 和任何 RLock/Lock 都互斥；
writer 等待时，新的 reader 会被挡住，避免 writer 饿死。
```

所以 `RWMutex` 的收益来自这一点：

```text
读操作之间不互相阻塞。
```

如果读操作非常多，写操作很少，读临界区还比较长，`RWMutex` 往往有用。例如：

```text
配置快照：大量读，偶尔热更新；
路由表：大量查询，偶尔替换；
只读缓存：命中时读多，失效时写少；
元数据索引：读路径频繁，写路径低频；
订阅者列表：广播读多，增删订阅少。
```

但没有一个固定比例可以放之四海皆准。有人会说“读写 9:1 就用 RWMutex”，这只是粗略经验，不是规则。真正要看：

```text
读临界区有多长；
写临界区有多长；
读写是否集中在同一把锁；
CPU 核数有多少；
读路径是否只是读一个整数；
写一来是否会频繁阻塞后续读；
是否有 reader counter 的 cache line 竞争。
```

一个很短的读临界区，比如只读一个字段：

```go
rw.RLock()
v := s.value
rw.RUnlock()
```

这时 `RWMutex` 可能不如 `Mutex`，甚至不如 `atomic.Value` 或 `atomic.Pointer`。原因是 `RWMutex` 的读锁不是免费午餐，它通常要维护 reader 计数、检查是否有 writer 等待，还会带来原子操作和 cache line 抖动。

`RWMutex` 适合满足这几个条件：

```text
读明显多于写；
读临界区足够长，能抵消 RLock/RUnlock 的额外成本；
读之间没有修改共享状态；
写不需要非常低的尾延迟；
没有读锁升级为写锁的需求；
可以接受 writer 到来后新 reader 被挡住。
```

不适合的情况：

```text
写很多；
读临界区极短；
读操作其实会更新统计、缓存、lazy init；
经常需要先读再升级写；
写延迟非常敏感；
锁保护的是一个单值，atomic 更合适；
每次读都要复制大对象，瓶颈不在锁。
```

Go 的 `RWMutex` 还有两个边界要记住：

```text
RLock 不能递归依赖：writer 等待时新 reader 会阻塞。
RLock 不能升级成 Lock，Lock 也不能降级成 RLock。
```

这意味着下面这种写法危险：

```go
rw.RLock()
if needWrite {
    rw.Lock() // 错：读锁升级写锁，会把自己卡住
}
rw.RUnlock()
```

正确做法通常是先释放读锁，再重新竞争写锁，并重新检查条件：

```go
rw.RLock()
need := check()
rw.RUnlock()

if need {
    rw.Lock()
    if checkAgain() {
        update()
    }
    rw.Unlock()
}
```

面试里可以这样回答：

```text
RWMutex 适合读远多于写的场景，但比例不是唯一判断标准。读临界区要足够长，读之间要真正能并行，写要比较少，且没有升级锁需求。短读、小对象、写频繁或写延迟敏感时，RWMutex 的 reader 计数、writer 阻塞和调度成本可能抵消收益，普通 Mutex、atomic.Value 或 copy-on-write 反而更好。
```

一句话：RWMutex 的收益来自并行读；如果读很短、写不少或不变量复杂，它可能只是更贵的 Mutex。

## Q006. RWMutex 在写多场景下为什么可能比 Mutex 更慢？

**回答：**

写多场景下，`RWMutex` 经常比 `Mutex` 更慢，因为它为了支持“多 reader 单 writer”维护了更多状态。写多时，这些额外机制没有换来并行读收益，反而变成成本。

普通 `Mutex` 的语义很直接：

```text
一个锁状态；
一个等待队列或信号量；
Lock 失败就等待；
Unlock 唤醒等待者。
```

`RWMutex` 要处理更多情况：

```text
当前有多少 reader；
是否有 writer 正在等待；
writer 等待哪些 reader 离开；
新的 reader 是否应该阻塞；
写锁释放时要唤醒多少 reader；
多个 writer 之间如何排队。
```

Go 的 `RWMutex` 源码结构里就能看到这些字段：

```go
w           Mutex
writerSem   uint32
readerSem   uint32
readerCount atomic.Int32
readerWait  atomic.Int32
```

这说明它不是一个简单的“读锁开关”。读路径和写路径都要围绕 reader count、semaphore 和 writer pending 状态工作。

写多时慢，常见原因有几类。

第一，写锁路径比 `Mutex` 更重。

writer 不只是抢一把锁，还要：

```text
和其他 writer 竞争；
宣布有 writer pending；
阻止新的 reader；
等待已有 reader 离开；
更新 readerCount/readerWait；
必要时 park/unpark。
```

如果写操作很多，系统大部分时间都在走这条复杂路径。读锁并行的优势很少出现。

第二，writer pending 会阻止新的 reader。

Go 文档明确说：如果有 goroutine 调用 `Lock`，而锁已经被 reader 持有，那么新的 `RLock` 会阻塞，直到 writer 获得并释放锁。这个设计是为了防止 writer starvation。

写多场景下，writer 经常 pending，于是读也不再自由并行：

```text
writer1 等待 reader 离开；
新 reader 被挡住；
writer1 完成；
writer2 又来了；
新 reader 继续被挡。
```

这时 `RWMutex` 退化成一种更复杂的互斥锁。

第三，reader counter 会产生 cache line 竞争。

即使只是读，每个 `RLock/RUnlock` 通常也要修改共享计数。多个 CPU 核频繁修改同一个 `readerCount`，会让 cache line 来回迁移。读很多但读临界区极短时，这个成本会很明显。

第四，唤醒成本更高。

写锁释放时，可能要唤醒一批 reader；reader 离开时，最后一个 reader 要唤醒 writer。频繁读写交替时，调度器在 reader 和 writer 之间来回切换，尾延迟会变差。

第五，写多时临界区互斥语义没有变。

无论 `RWMutex` 还是 `Mutex`，写操作都必须独占：

```text
writer vs writer: 互斥
writer vs reader: 互斥
```

如果 workload 主要是写，`RWMutex` 没法让写并行。它只增加了管理 reader 的成本。

可以用一个简单判断：

```text
读多写少：
  RWMutex 可能让读并行，收益覆盖成本。

写多或读写交替频繁：
  RWMutex 经常退化成更复杂的 Mutex。
```

面试里可以这样回答：

```text
RWMutex 在写多场景下可能比 Mutex 慢，是因为写锁仍然必须独占，但实现还要维护 reader 计数、writer pending、reader/writer semaphore，并在 writer 等待时阻止新 reader。写多时读并行收益很少，额外的原子操作、cache line 竞争和 park/unpark 成本就暴露出来。普通 Mutex 反而路径更短、更可预测。
```

一句话：RWMutex 只有读并行能还本；写多时它只是带着读写管理开销的互斥锁。

## Q007. 读写锁如何处理 writer starvation？

**回答：**

`writer starvation` 指的是 writer 一直等不到写锁。典型场景是 reader 源源不断进入：

```text
reader1 持有读锁
writer 开始等待
reader2 进来
reader3 进来
reader4 进来
...
writer 永远等不到 reader count 变成 0
```

如果读写锁允许新 reader 在 writer 等待时继续进入，读吞吐可能很好，但 writer 可能饿死。很多实现会在 writer 到达后改变策略：已有 reader 可以完成，但新 reader 要排队。

Go 的 `sync.RWMutex` 就是这种策略。官方文档说明：如果有 goroutine 调用 `Lock`，而锁已经被一个或多个 reader 持有，那么并发的 `RLock` 会阻塞，直到 writer 获得并释放锁。目的就是让锁最终对 writer 可用。

流程可以写成：

```text
1. reader1、reader2 已经持有 RLock。
2. writer 调用 Lock，进入等待。
3. 新来的 reader3 调用 RLock，会被挡住。
4. reader1、reader2 退出后，writer 获得 Lock。
5. writer Unlock 后，reader3 等待者再继续。
```

这是一种 writer-preference 或 writer-pending-blocks-new-readers 策略。它不让 writer 被无限新 reader 淹没。

常见处理策略有几种。

```text
1. Reader preference
   新 reader 只要没有 active writer 就能进。
   读吞吐好，但 writer 可能饿死。

2. Writer preference
   writer 等待后，新 reader 阻塞。
   writer 不容易饿死，但读延迟可能升高。

3. FIFO fairness
   reader 和 writer 按队列顺序来。
   饥饿少，但吞吐可能下降。

4. Phase-fair RWLock
   一批 reader 作为一个 phase，writer 和 reader phase 交替。
   尝试在吞吐和公平之间折中。

5. Reader quota / time slice
   允许一批 reader 通过，到阈值后让 writer 先走。
```

防 writer starvation 不是免费的。writer 等待时阻止新 reader，会让读请求出现排队：

```text
读多写少系统里，偶尔一个写会让后续读短暂停住；
写操作如果很慢，会放大读尾延迟；
如果 writer 频繁到来，RWMutex 可能接近 Mutex。
```

这也是为什么写临界区要短。一个 writer 进来后，新 reader 被挡在外面；如果 writer 持锁做网络请求或磁盘 I/O，读路径也会被拖住。

工程上还要注意，避免“读锁升级写锁”制造自我饥饿或死锁：

```go
rw.RLock()
// ...
rw.Lock() // 不要这样写
```

Go 文档明确说 `RLock` 不能升级成 `Lock`。如果 writer 等待会阻止新 reader，那么递归读锁、升级锁这类写法更容易把自己卡住。

面试里可以这样回答：

```text
读写锁处理 writer starvation 的核心办法是：writer 开始等待后，不再允许新的 reader 无限制进入。已有 reader 退出后，让 writer 先获得锁。Go 的 RWMutex 文档就是这个语义，这会禁止递归读锁，也不支持读锁升级写锁。代价是读请求可能被等待中的 writer 阻塞，尤其写临界区长或写频繁时，读延迟会明显上升。
```

一句话：防 writer 饿死的办法通常是挡住新 reader；它救了 writer，但会把一部分读延迟推高。

## Q008. 公平锁和非公平锁有什么区别？

**回答：**

公平锁和非公平锁的区别在于：锁释放后，谁有资格先拿到锁。

公平锁通常按等待顺序授予锁：

```text
先等待的人先获得锁；
后来的线程不能随便插队；
饥饿风险低；
等待时间更可预测。
```

非公平锁允许 barging，也就是新来的执行单元可能在已经有等待者的情况下抢到锁：

```text
锁刚释放；
等待者还没被调度起来；
一个正在 CPU 上运行的新线程直接抢到锁。
```

这听起来“不公平”，但性能上经常更好。原因很简单：新来的线程已经在 CPU 上跑，cache 可能还是热的；如果严格唤醒最早等待者，可能要调度切换，等待者醒来后还要重新进入运行队列。

Java `ReentrantLock` 的官方文档把这个 trade-off 说得很直：公平模式在竞争下倾向于把锁给等待最久的线程；使用公平锁的程序整体吞吐可能更低，但获得锁时间的方差更小，也能避免饥饿。它还提醒，锁公平不等于线程调度公平。

Go 的 `sync.Mutex` 实现更像混合策略。Go 源码注释里写到，`Mutex` 有 normal 和 starvation 两种模式：

```text
normal mode:
  waiters 大体按 FIFO 排队，但被唤醒的 waiter 并不直接拥有锁；
  新来的 goroutine 会和被唤醒的 waiter 竞争；
  新来的 goroutine 因为已经在 CPU 上运行，可能有优势；
  这种模式吞吐更好。

starvation mode:
  如果 waiter 等待超过约 1ms，mutex 切到饥饿模式；
  unlock 时把所有权直接交给队首 waiter；
  新来的 goroutine 不抢锁，也不自旋，而是排到队尾；
  这样压低极端尾延迟。
```

这说明现实里的锁很少是单纯“公平”或“不公平”。很多实现会在吞吐和饥饿之间折中。

公平锁的优点：

```text
等待时间更可预测；
不容易饿死；
适合任务耗时差异大、需要控制尾延迟的场景；
便于推理排队行为。
```

公平锁的缺点：

```text
吞吐可能下降；
上下文切换更多；
cache locality 变差；
释放锁后必须唤醒指定等待者，调度成本更高。
```

非公平锁的优点：

```text
吞吐高；
实现相对简单；
CPU/cache locality 更好；
短临界区下表现常常更好。
```

非公平锁的缺点：

```text
等待时间方差大；
某些等待者可能长期抢不到；
尾延迟可能难看；
线上排障时表现更不稳定。
```

面试里可以这样回答：

```text
公平锁强调排队顺序，通常能减少饥饿和等待时间方差，但吞吐会因为调度切换和 cache locality 变差而下降。非公平锁允许新来的线程插队，短临界区下吞吐往往更好，但尾延迟和饥饿风险更高。Go 的 Mutex 不是简单 FIFO，它在 normal mode 里允许竞争以换吞吐，在等待超过阈值后进入 starvation mode，把锁直接交给队首 waiter 来控制尾延迟。
```

一句话：公平锁买的是可预测等待，非公平锁买的是吞吐；成熟实现常常两边都借一点。

## Q009. 可重入锁和不可重入锁有什么区别？

**回答：**

可重入锁允许同一个 owner 在已经持有锁的情况下再次加锁；不可重入锁不允许。这里的 owner 通常是线程，不一定是 goroutine。关键点是：可重入锁必须记录“谁持有锁”和“重入了几次”。

可重入锁的典型语义：

```text
thread T Lock -> 成功，hold count = 1
thread T Lock -> 仍然成功，hold count = 2
thread T Unlock -> hold count = 1，锁还没释放
thread T Unlock -> hold count = 0，锁真正释放
```

Java 的 `ReentrantLock` 官方文档就是这个语义：锁由最后成功加锁且尚未解锁的线程拥有；如果当前线程已经拥有锁，再次调用 `lock` 会立刻返回，并增加 hold count；`unlock` 会减少 hold count，降到 0 才释放。

不可重入锁的语义更简单：

```text
Lock:
  如果锁已被持有，就等待。
```

它不管“是不是同一个调用方”。如果同一个线程或 goroutine 在没有释放的情况下再次 `Lock`，就会把自己堵住。

可重入锁的好处是方便组合。

比如：

```java
void outer() {
    lock.lock();
    try {
        inner();
    } finally {
        lock.unlock();
    }
}

void inner() {
    lock.lock();
    try {
        // ...
    } finally {
        lock.unlock();
    }
}
```

如果 `lock` 可重入，`outer` 调 `inner` 不会自锁。很多面向对象代码喜欢这种语义，因为方法之间可以互相调用，不必暴露“调用时是否已持锁”的细节。

但可重入锁也有代价。

```text
实现上要记录 owner 和 hold count；
Unlock 必须校验当前 owner；
递归进入可能让临界区变长；
容易掩盖锁边界设计不清的问题；
可能让不变量在半更新状态下被内部方法再次观察。
```

不可重入锁的优点是边界更硬。它会逼你把“哪些函数要求调用方已持锁”说清楚。Go 的标准库里经常能看到这种风格：

```text
exported method:
  自己 Lock/Unlock。

unexported helper:
  假设调用方已经持锁，名字或注释说明 locked。
```

例如：

```go
func (s *Store) Put(k, v string) {
    s.mu.Lock()
    defer s.mu.Unlock()
    s.putLocked(k, v)
}

func (s *Store) putLocked(k, v string) {
    s.m[k] = v
}
```

这样比依赖重入更清晰。

不可重入锁的风险是自死锁：

```go
func (s *Store) A() {
    s.mu.Lock()
    defer s.mu.Unlock()
    s.B()
}

func (s *Store) B() {
    s.mu.Lock() // 如果 A 已经持锁，这里会卡住
    defer s.mu.Unlock()
}
```

面试里可以这样回答：

```text
可重入锁记录 owner 和 hold count，同一个 owner 已经持锁时可以再次加锁，只有 unlock 次数抵消后才真正释放。不可重入锁不区分是不是同一个 owner，只要锁被持有，再次 Lock 就等待。可重入锁便于方法组合，但容易隐藏临界区过大和锁边界不清的问题；不可重入锁实现简单、语义硬，但递归调用或已持锁调用公开方法时会自死锁。
```

一句话：可重入锁宽容重复进入，不可重入锁逼你把锁边界设计清楚。

## Q010. Go 的 sync.Mutex 是否可重入？

**回答：**

Go 的 `sync.Mutex` 不可重入。

更准确地说，`sync.Mutex` 没有记录“哪个 goroutine 持有锁”，也没有 hold count。官方文档还特别说明：锁住的 `Mutex` 不关联某个特定 goroutine，一个 goroutine 可以加锁，然后安排另一个 goroutine 解锁。这个设计直接说明它不是 Java `ReentrantLock` 那种 owner-based reentrant lock。

所以这段代码会把自己卡住：

```go
var mu sync.Mutex

func f() {
    mu.Lock()
    defer mu.Unlock()

    mu.Lock()   // 阻塞：同一个 goroutine 也不能再次进入
    defer mu.Unlock()
}
```

如果程序里没有别的 goroutine 能释放这把锁，运行时最终可能报：

```text
fatal error: all goroutines are asleep - deadlock!
```

但不要误解成“Go 检测到了重入”。Go 不是在第二次 `Lock` 时说“你重入了，所以报错”。它只是正常等待一把已经被持有的锁；如果所有 goroutine 都睡了，运行时才发现整个程序 deadlock。

再看一个更隐蔽的例子：

```go
type Store struct {
    mu sync.Mutex
    m  map[string]string
}

func (s *Store) Put(k, v string) {
    s.mu.Lock()
    defer s.mu.Unlock()
    s.put(k, v)
}

func (s *Store) put(k, v string) {
    s.mu.Lock()
    defer s.mu.Unlock()
    s.m[k] = v
}
```

`Put` 已经持有 `s.mu`，再调用 `put`，`put` 又尝试加同一把锁，于是自死锁。Go 代码里通常这样改：

```go
func (s *Store) Put(k, v string) {
    s.mu.Lock()
    defer s.mu.Unlock()
    s.putLocked(k, v)
}

func (s *Store) putLocked(k, v string) {
    s.m[k] = v
}
```

`putLocked` 的名字直接说明：调用它之前必须已经持锁。这样比假装锁可重入更安全。

Go 的这个设计还带来另一个面试点：`Unlock` 不要求由加锁的同一个 goroutine 调用。

例如：

```go
mu.Lock()
go func() {
    // 做完某个异步步骤后释放
    mu.Unlock()
}()
```

这是允许的，但不代表推荐。跨 goroutine 解锁会让锁所有权很难推理，除非你在实现某种明确的同步协议。普通业务代码里，最好还是谁加锁谁解锁，且用 `defer` 保证异常路径释放。

为什么 Go 不做可重入？一个实用解释是：Go 的 goroutine 没有被设计成暴露稳定的 goroutine id 给用户，`sync.Mutex` 也不维护 owner。维护 owner 和递归计数会增加成本，还会鼓励更模糊的锁边界。Go 更常见的写法是：

```text
公开方法自己加锁；
内部 helper 假设已持锁；
不要在持锁状态下调用可能反过来拿同一把锁的公开方法；
用文档或命名标清 locked 前置条件。
```

面试里可以这样回答：

```text
Go 的 sync.Mutex 不可重入。它没有 goroutine owner 和 hold count；同一个 goroutine 在持有锁时再次 Lock 会像其他 goroutine 一样阻塞，如果没有人释放就会死锁。官方文档还说明 locked Mutex 不绑定特定 goroutine，允许一个 goroutine Lock、另一个 goroutine Unlock，这也说明它不是 owner-based reentrant lock。Go 里通常用 locked helper、清晰的锁边界和避免持锁调用外部回调来处理这个问题。
```

一句话：Go 的 `sync.Mutex` 是不可重入的；同一个 goroutine 重复 `Lock` 不会成功递归进入，只会等待自己释放。

## Q011. spinlock 和 blocking mutex 的核心差异是什么？

**回答：**

核心差异不在于“谁更快”，而在于等待方式。

`spinlock` 拿不到锁时，线程不会马上睡眠，而是在用户态或内核态反复检查锁状态，通常是围绕一个原子变量做 CAS、test-and-set 或 exchange：

```text
while !CAS(&lock, 0, 1) {
    cpu_relax()
}
```

这段等待期间，线程仍然占着 CPU 时间片。它没有把自己交给调度器挂起，也没有等待别人唤醒。只要持锁者很快释放锁，等待者可能马上在本核或另一个核上抢到锁，避免一次阻塞、调度、唤醒、上下文切换的成本。

`blocking mutex` 的思路不同。它通常也有一个很短的 fast path：先尝试用原子操作把锁从 unlocked 改成 locked。如果成功，直接进入临界区；如果失败，说明锁已经被别人持有，它会进入慢路径，把当前线程或 goroutine 放进等待队列，然后睡眠，等 unlock 时被唤醒。

可以粗略对比成：

```text
spinlock:
  拿不到锁 -> 继续运行 -> 反复看锁有没有释放 -> 消耗 CPU，少调度切换

blocking mutex:
  拿不到锁 -> 进入等待队列 -> park/sleep -> 不消耗 CPU，但之后需要 wakeup/schedule
```

所以 spinlock 的成本主要是 CPU 空转和 cache line 抖动。多个 CPU 同时盯着同一个锁变量，不断读写同一条 cache line，这条 cache line 会在核之间来回迁移。锁持有时间越长，空转越浪费。

blocking mutex 的成本主要是调度和唤醒。线程从 running 变成 sleeping，再从 sleeping 变回 runnable，需要内核或运行时参与。这个成本比一次原子 CAS 高很多，但好处是 CPU 可以去跑别的工作，不会被一个拿不到锁的线程白白占住。

面试里最容易说错的是把 spinlock 说成“忙等，所以一定差”。这不准确。spinlock 在很短临界区、不会被长时间抢占、CPU 核数足够、等待时间小于一次睡眠唤醒成本时，反而可能更合适。内核里很多自旋锁就是为这种场景服务的：临界区短，不能睡眠，持锁期间要保护调度器、runqueue 或中断相关结构。

但在普通业务代码里，blocking mutex 更常见。原因也很现实：

```text
业务临界区很难保证极短；
持锁期间可能发生 I/O、GC、page fault、日志打印、RPC、channel 等待；
线程可能被操作系统抢占；
goroutine 数量通常远大于 CPU 核数；
线上更怕 CPU 被空转打满。
```

Go 的 `sync.Mutex` 不是一个纯 spinlock。源码里能看到它有 fast path，也有慢路径；慢路径里会在适当条件下 active spinning，但最终会通过运行时信号量把 goroutine 挂起。也就是说，它是混合策略：先赌锁很快释放，赌错了就睡眠，不让 goroutine 无限烧 CPU。

一个很小的例子：

```go
mu.Lock()
counter++
mu.Unlock()
```

如果只有几个 goroutine 在多核机器上抢这把锁，`counter++` 又非常短，短暂自旋可能比 park/unpark 更便宜。

但如果变成这样：

```go
mu.Lock()
rows, err := db.QueryContext(ctx, query)
// 处理网络和数据库返回
mu.Unlock()
```

这里如果用 spinlock，就很糟。等待者可能在数据库请求期间一直占 CPU，系统吞吐会被空转吃掉。正确做法通常是缩小锁范围，或者用 blocking mutex 让等待者睡眠。

面试里可以这样回答：

```text
spinlock 和 blocking mutex 的核心差异是等待策略。spinlock 拿不到锁时不睡眠，而是反复检查锁状态，适合锁很快释放、不能睡眠或睡眠唤醒成本比等待时间还高的场景。blocking mutex 拿不到锁时把执行单元挂起，等 unlock 唤醒，适合临界区可能较长、线程可能被抢占、等待期间不想浪费 CPU 的场景。现代 mutex 往往不是二选一，而是先做短暂自旋，再进入阻塞慢路径。
```

一句话：spinlock 用 CPU 时间换低延迟，blocking mutex 用调度等待换 CPU 利用率。

## Q012. spinlock 适合临界区很短还是很长的场景？

**回答：**

spinlock 适合临界区很短的场景，而且这里的“短”不是代码行数短，而是持锁时间在实际运行中稳定地短。

判断一个临界区适不适合 spinlock，要看等待者自旋的时间是否小于阻塞和唤醒的成本。一次进入内核睡眠、放入等待队列、调度其他线程、再被唤醒并重新调度回来，成本并不低。如果持锁者马上就会释放锁，自旋几十到几百个 CPU cycle 可能比睡眠划算。

典型适合自旋的形状是：

```text
只改几个内存字段；
只做一次队列入队/出队；
只更新一个状态位；
只移动一个链表节点；
持锁期间不会 I/O；
持锁期间不会等待条件变量、channel、RPC、磁盘、网络；
持锁期间不会主动让出 CPU。
```

反过来，下面这些场景不适合 spinlock：

```text
持锁做网络 I/O 或磁盘 I/O；
持锁等待数据库、RPC、文件系统；
持锁执行复杂循环或大对象拷贝；
持锁调用外部回调；
持锁期间可能被长时间抢占；
单核机器上等待同一个 CPU 上的持锁者运行；
goroutine 或线程数量远大于 CPU 核数。
```

为什么长临界区特别不适合 spinlock？因为等待线程不能推进业务，也不让出 CPU。假设线程 A 持锁 10ms，线程 B 自旋等锁 10ms。B 这 10ms 里什么业务都没做，只是在不断读同一个锁变量。线程数量一多，CPU 看上去很忙，但真正完成的请求变少，尾延迟还会上升。

还有一个容易忽略的点：即使临界区本身很短，只要持锁线程可能被抢占，spinlock 也会变危险。比如持锁线程 A 刚拿到锁就被 OS 调度走，线程 B 在另一个 CPU 上疯狂自旋。A 不运行，锁就不会释放；B 越自旋，浪费越多。如果中优先级线程又持续抢占 A，就会和 priority inversion 叠在一起，延迟会很难看。

所以在用户态通用程序里，纯 spinlock 通常要非常谨慎。很多运行时或 libc 的锁会采用“有限自旋”：

```text
先原子 CAS；
失败后自旋少量次数；
如果期间锁释放，直接拿到锁；
如果仍然拿不到，进入 futex/park/semacquire 之类的阻塞路径。
```

这种策略承认两件事：

```text
短等待时，睡眠太贵；
长等待时，自旋太浪费。
```

Go 的 `sync.Mutex` 源码也体现了这个思路。慢路径里会判断是否适合 active spinning；满足条件时自旋，不满足或自旋多次后走运行时信号量等待。它不是让 goroutine 永远原地打转。

面试里可以这样回答：

```text
spinlock 适合临界区非常短、持锁期间不会睡眠或阻塞、持锁线程很可能正在别的 CPU 上继续运行的场景。它不适合长临界区，也不适合持锁做 I/O、等待外部系统、调用不受控代码或可能被长时间抢占的路径。实际工程里常见的是有限自旋加阻塞的混合锁：先赌锁很快释放，赌错就睡眠。
```

一句话：spinlock 只适合“等一小会儿就能拿到”的锁；等久了，它烧掉的是 CPU 和尾延迟。

## Q013. futex 的 fast path 和 slow path 大致是什么？

**回答：**

`futex` 是 Fast Userspace Mutex 的缩写。它本身不是完整的 mutex，而是 Linux 提供给用户态锁实现的一个底层机制。理解 futex 要抓住一句话：无竞争时完全在用户态完成，有竞争时才进入内核等待或唤醒。

一个用户态 mutex 通常会在内存里放一个 futex word，Linux man page 里强调它是一个 32-bit 的值。用户态通过原子操作修改这个值：

```text
0: unlocked
1: locked, no known waiter
2: locked, maybe has waiter
```

不同实现的状态编码不一定一样，但思路类似。

fast path 是无竞争路径。加锁时，线程只需要做一次原子 CAS：

```c
if (atomic_compare_exchange(&state, 0, 1)) {
    // got lock, no syscall
    return;
}
```

如果锁原来是 0，CAS 改成 1 成功，线程拿到锁。整个过程不需要系统调用，内核也不知道这把锁存在。这就是 futex 性能好的关键：普通 mutex 的绝大多数加锁释放，如果没有竞争，都只是用户态原子指令。

解锁的 fast path 也类似。如果锁状态显示没有等待者，释放锁只需要把状态改回 0：

```c
if (atomic_exchange(&state, 0) == 1) {
    // no known waiter, no syscall
    return;
}
```

slow path 是竞争路径。线程 CAS 失败，发现锁已经被别人持有，这时不能盲目睡眠，否则可能错过唤醒。futex 的核心语义是 compare-and-block：调用 `FUTEX_WAIT` 时，内核会检查 futex word 是否仍然等于调用方传入的 expected value；只有仍然相等，才把线程真正挂起。

大致流程是：

```text
lock slow path:
  CAS 失败，锁已被持有
  把状态标记为有等待者
  futex(FUTEX_WAIT, addr, expected_locked_value)
  被唤醒后回到用户态
  重新尝试 CAS

unlock slow path:
  释放用户态状态
  如果发现可能有等待者
  futex(FUTEX_WAKE, addr, 1)
```

为什么 `FUTEX_WAIT` 要带 expected value？这是为了避免 lost wakeup。

考虑这个竞态：

```text
T1 持锁
T2 发现锁被持有，准备睡眠
T1 释放锁并唤醒等待者
T2 如果这时才真正睡下，就可能再也没人唤醒它
```

futex 的 compare-and-block 把“检查值”和“睡眠”做成一个原子意义上的动作。如果 T1 已经把锁值改掉，T2 调用 `FUTEX_WAIT` 时内核会发现值不等于 expected，不会让 T2 睡死，而是返回，让 T2 回到用户态重新尝试。

所以 futex 的 fast path 和 slow path 可以这样说：

```text
fast path:
  用户态原子操作成功，不进内核。
  lock 是 CAS 0->locked，unlock 是 locked->0。

slow path:
  出现竞争后进入内核。
  wait 方用 FUTEX_WAIT 做 compare-and-block；
  unlock 方在释放状态后用 FUTEX_WAKE 唤醒等待者。
```

PI futex 的 slow path 会更复杂。Linux PI-futex 文档里描述过，用户态用 TID 表示锁 owner；如果从 0 到 TID 的原子转换失败，就调用 `FUTEX_LOCK_PI`，内核会把它接到带 priority inheritance 的 rt-mutex 上。解锁 fast path 如果能把 TID 改回 0，就不进内核；如果有 waiters bit，就走 `FUTEX_UNLOCK_PI`，由内核处理唤醒和优先级继承状态。

面试里可以这样回答：

```text
futex 的 fast path 是用户态原子操作：无竞争加锁用 CAS 修改 futex word，解锁时直接把状态改回 unlocked，不做系统调用。slow path 是竞争路径：拿不到锁的线程调用 FUTEX_WAIT，内核只有在 futex word 仍等于 expected value 时才阻塞它；释放锁的一方在必要时调用 FUTEX_WAKE。这个设计把高频无竞争路径留在用户态，把真正需要排队睡眠的路径交给内核。
```

一句话：futex 不是“锁都进内核”，而是“只有睡眠和唤醒这件事需要内核帮忙”。

## Q014. 为什么用户态自旋后再进入内核阻塞可以提高性能？

**回答：**

因为锁等待时间有两种完全不同的形状：很多等待非常短，少数等待很长。用户态先自旋一小段，再进入内核阻塞，正是在同时处理这两种情况。

如果每次 CAS 失败都立刻进入内核睡眠，会发生什么？

```text
线程 A 持锁；
线程 B CAS 失败；
B 立刻 futex wait / park；
A 几百纳秒后释放锁；
内核或运行时再把 B 唤醒；
B 被调度回来后重新运行。
```

如果 A 很快释放锁，那么 B 的睡眠和唤醒成本可能比实际等待时间还高。一次上下文切换、调度器队列操作、内核态用户态切换、cache/TLB 局部性损失，都可能比“再等几十个 cycle”贵。

所以很多锁会先做有限自旋：

```text
CAS failed
for i := 0; i < spinLimit; i++ {
    cpu_relax()
    if CAS succeeds {
        return
    }
}
park / futex wait
```

这可以提高性能的原因主要有四个。

第一，避免不必要的系统调用。futex 的价值就是无竞争和短竞争不进内核。短暂自旋如果成功，整个等待过程仍然停留在用户态。

第二，减少上下文切换。线程睡下去再醒过来，调度器要参与。短临界区下，调度成本经常比临界区本身更大。

第三，保持 cache locality。刚刚在同一个 CPU 上运行的线程，相关数据、栈、代码路径可能还在 cache 里。如果马上睡眠再被调度到别的 CPU，局部性会变差。短暂自旋有机会在热 cache 状态下继续执行。

第四，降低唤醒风暴。某些锁实现如果过早把大量线程放进等待队列，unlock 时唤醒和重新竞争会更重。先让少量线程在用户态观察锁，有时能减少进入内核排队的人数。

但这里有一个边界：自旋必须是有限的。自旋太久，优势会反过来变成浪费：

```text
锁持有者被抢占了，自旋者等不到；
持锁者在做 I/O，自旋者只能烧 CPU；
等待者数量很多，大家一起打同一条 cache line；
系统 CPU 已经很紧张，自旋会挤占真正能推进工作的线程。
```

所以“先自旋再阻塞”不是简单地说 spin 一定好，而是一个分层策略：

```text
第一层：无竞争，CAS 直接成功。
第二层：短竞争，有限自旋等释放。
第三层：长竞争，进入内核或运行时阻塞。
```

Go 的 `sync.Mutex` 就是这个味道。它会在慢路径里判断能否 active spinning，满足条件才自旋；不满足时通过运行时信号量等待。Linux futex 的 mutex 实现也通常是用户态原子操作加内核 wait/wake。两者背后的工程判断一致：不要为短等待付出完整睡眠成本，也不要为长等待无限空转。

面试里可以这样回答：

```text
用户态先自旋再阻塞，是为了覆盖短等待和长等待两类情况。短等待时，锁可能马上释放，自旋成功就省掉系统调用、上下文切换和唤醒成本；长等待时，自旋到上限后进入 futex 或运行时 park，避免继续浪费 CPU。它提高性能的前提是自旋有限，而且锁持有时间通常较短。如果持锁路径可能 I/O、被长时间抢占或竞争非常激烈，自旋反而会放大 CPU 消耗和 cache line 抖动。
```

一句话：先自旋是为了不把几百纳秒的等待变成一次完整调度；后阻塞是为了不把几毫秒的等待变成 CPU 空烧。

## Q015. lock convoy 是什么？

**回答：**

`lock convoy` 可以理解成“锁后面排出车队”。一把热锁被很多线程反复争用时，某个慢持锁者或被抢占的持锁者会让后面一串线程都排队。等锁释放后，这些线程不是平滑通过，而是一个接一个被唤醒、抢锁、阻塞、再唤醒，系统吞吐下降，尾延迟变高。

一个典型过程是：

```text
T1 拿到锁；
T1 持锁期间被抢占，或临界区变慢；
T2、T3、T4 ... 都卡在这把锁上；
T1 释放锁；
等待队列里的某个线程被唤醒；
它运行、拿锁、释放；
下一个线程再被唤醒；
队列持续存在。
```

为什么这叫 convoy？因为锁成了单车道收费站。前面一辆车慢，后面车队全慢；即使后面每辆车本来都很快，也要按这个瓶颈过。

lock convoy 经常出现在这些场景：

```text
全局大锁保护大量无关状态；
短请求和长请求共用同一把锁；
持锁期间做 I/O、日志、内存分配、复杂计算；
锁释放后总是唤醒一个等待线程，等待队列长期不为空；
公平锁严格按队列交接，吞吐被上下文切换拖慢；
系统过载，持锁线程被频繁抢占。
```

它的症状一般不是“程序完全卡死”，而是性能退化：

```text
CPU 利用率可能不低，但吞吐上不去；
p99/p999 延迟突然变高；
大量线程处于 mutex wait、futex wait、semacquire；
火焰图里业务计算不多，等待和调度很多；
mutex profile 显示少数 unlock 栈贡献了大量等待时间。
```

注意 lock convoy 和 deadlock 不一样。deadlock 是大家互相等，没人能前进；lock convoy 是还能前进，但队列像堵车一样一直存在，系统变慢。它也和普通 lock contention 不完全一样。普通竞争可能只是偶尔排队；convoy 强调等待队列被持续维持住，释放锁后马上又被下一批等待者占满。

解决思路通常不是“换一个更高级的锁名”，而是减少这把锁形成车队的机会：

```text
缩小临界区，不在锁内做 I/O；
拆分全局锁，按 key、shard、partition 分片；
把慢路径移出锁外；
把读多写少的数据改成 copy-on-write 或 atomic pointer；
把短任务和长任务拆到不同队列或不同锁域；
降低严格公平交接带来的调度成本，允许适度 barging；
对热点 key 做限流、批处理或单线程 owner 模型。
```

举个 Go 服务里的例子。假设一个 `Store` 用一把全局 `sync.Mutex` 保护所有 stream：

```go
type Store struct {
    mu      sync.Mutex
    streams map[string]*Stream
}
```

如果 `Append` 在持锁期间做编码、写文件、fsync、更新索引，所有 stream 都会被这把锁串行化。某一次 fsync 慢了，后面所有 append 都排队。这就是容易形成 convoy 的结构。更好的方向是把锁只用于内存状态切换，I/O 批处理或拆锁，至少不要让无关 stream 的慢 I/O 互相拖住。

面试里可以这样回答：

```text
lock convoy 是很多线程在同一把热锁后面持续排队，某个慢持锁者或调度延迟会把后续线程串成队列。它不是死锁，系统还能前进，但吞吐下降、尾延迟上升、上下文切换和 futex/mutex wait 变多。处理时要看持锁时间、锁粒度和热点资源，常见办法是缩小临界区、拆分锁、移出慢 I/O、分片热点状态，必要时用批处理或单 owner 模型减少共享锁竞争。
```

一句话：lock convoy 不是锁坏了，而是这把锁变成了系统里持续排队的收费站。

## Q016. priority inversion 是什么？

**回答：**

`priority inversion` 是优先级反转。它说的是：高优先级任务因为等待低优先级任务持有的资源，实际执行顺序被反过来了；更糟的是，中优先级任务还可能抢占低优先级任务，导致高优先级任务等得更久。

最经典的三个任务例子是：

```text
H: high priority
M: medium priority
L: low priority

L 先拿到锁 lockA；
H 后来要 lockA，发现 L 持有，于是 H 阻塞；
M 这时变成 runnable；
调度器看到 M 的优先级高于 L，于是让 M 跑；
L 没法运行，就没法释放 lockA；
H 虽然优先级最高，却被间接挡住。
```

表面上 H 在等 L；实际运行时，M 也在拖 H。因为 M 不需要 H 等的那把锁，但它抢占了能释放锁的 L。

Linux RT-mutex 文档把这个问题讲得很直：高优先级任务想使用低优先级任务持有的资源时，高优先级任务必须等低优先级任务完成资源使用，这就是 priority inversion。真正危险的是 unbounded priority inversion，也就是等待时间没有上界。只要中优先级任务一直可运行，低优先级持锁者就可能一直没机会释放锁。

这个问题在实时系统里尤其严重，因为实时系统关心的是最坏情况延迟，而不是平均吞吐。高优先级任务如果是音频线程、控制线程、交易路径、心跳线程，它“理论优先级很高”没有用；只要它等的资源被低优先级任务拿着，它就跑不起来。

在普通服务端程序里，priority inversion 不一定以 OS priority 的形式出现，也可能以调度资源、队列优先级、协程池优先级的形式出现。比如：

```text
高优先级请求需要某个全局锁；
低优先级后台任务持有这把锁做较长清理；
中间一堆普通请求持续占用 CPU；
后台任务迟迟跑不到，导致高优先级请求一直等。
```

这仍然是同一个结构：高优先级工作被低优先级持锁者挡住，而中间优先级工作让持锁者无法尽快释放资源。

priority inversion 和 deadlock 也不一样。deadlock 是循环等待，通常没人能继续；priority inversion 中，低优先级任务只要运行并释放锁，高优先级任务就能继续。问题是调度器如果不知道这个依赖关系，就可能不给低优先级任务足够运行机会。

面试里可以这样回答：

```text
priority inversion 是高优先级任务等待低优先级任务持有的锁或资源，导致实际运行顺序被反过来。典型三任务场景是：低优先级 L 持锁，高优先级 H 等锁，中优先级 M 抢占 L；结果 H 虽然优先级最高，却要等 M 跑完或等 L 重新获得 CPU 后释放锁。它不一定是死锁，但会破坏实时性，严重时会变成无上界等待。
```

一句话：priority inversion 的本质是调度优先级和资源依赖关系不一致。

## Q017. priority inheritance 如何缓解 priority inversion？

**回答：**

`priority inheritance`，也就是优先级继承，思路很直接：如果高优先级任务 H 阻塞在低优先级任务 L 持有的锁上，就临时把 L 的有效优先级提高到 H 的级别，让 L 尽快运行并释放锁。

还是三个任务的例子：

```text
L 拿着 lockA；
H 要 lockA，阻塞；
内核发现 H 在等 L 持有的锁；
L 临时继承 H 的高优先级；
M 虽然是中优先级，但不能抢占继承后的 L；
L 继续运行，尽快释放 lockA；
L 释放后恢复原始优先级；
H 获得锁继续执行。
```

这样做解决的不是“高优先级任务不需要等待”这个问题。只要资源被别人拿着，等待不可避免。它缓解的是无关的中优先级任务把等待时间无限拉长。换句话说，priority inheritance 不能消除临界区本身的长度，但能让持锁者尽快完成临界区。

Linux RT-mutex 文档里 PI 的核心就是这个：如果一个进程阻塞在当前进程持有的锁上，当前进程继承阻塞者的优先级。释放锁后，继承优先级撤销。复杂情况下还会形成 PI chain：

```text
H 等 L1，L1 的 owner 是 A；
A 又等 L2，L2 的 owner 是 B；
B 又等 L3，L3 的 owner 是 C；
```

这时 boost 可能沿着链传递。最终要让链末端真正能释放资源的 owner 得到足够高的有效优先级。Linux RT-mutex 里会维护 waiter tree、pi_waiters tree，并根据最高优先级等待者调整 owner 的有效优先级。

PI futex 则把这个能力暴露给用户态锁。普通 futex 只帮用户态做 wait/wake，不知道“锁 owner 的调度优先级应该被 boost”。PI futex 的慢路径会把用户态 futex 接到内核的 rt-mutex 上，由内核管理 owner、waiters 和优先级继承。

但 priority inheritance 不是万能的。它主要缓解 priority inversion，不解决这些问题：

```text
临界区本身太长；
持锁期间做不可中断 I/O；
锁顺序错误导致 deadlock；
资源不是通过支持 PI 的锁表达；
应用层队列、线程池或 RPC 排队没有把优先级传下去；
低优先级任务持有多个资源导致复杂依赖链。
```

所以工程上还要配合：

```text
缩短高优先级路径需要的锁；
高优先级线程避免等待低优先级后台锁；
实时路径不要持锁做 I/O；
使用支持 PI 的 pthread mutex / PI futex / RT-mutex；
把优先级从入口传到队列、worker 和下游请求。
```

面试里可以这样回答：

```text
priority inheritance 的做法是：当高优先级任务阻塞在低优先级任务持有的锁上时，临时提升锁 owner 的有效优先级，让它不要被中优先级任务抢占，从而尽快释放锁。释放锁后，owner 恢复原来的优先级。它不能消除锁等待，也不能修复长临界区或死锁，但能把无上界的优先级反转压回到“等待持锁者完成临界区”的范围内。
```

一句话：PI 不是让高优先级任务插队拿锁，而是让挡住它的人先把锁还出来。

## Q018. deadlock 产生的四个必要条件是什么？

**回答：**

死锁的四个必要条件通常是：

```text
1. mutual exclusion，互斥条件；
2. hold and wait，占有并等待；
3. no preemption，不可抢占；
4. circular wait，循环等待。
```

这四个条件是“必要条件”，意思是死锁发生时它们都成立；只要破坏其中任意一个，就能避免这一类死锁。

第一个是互斥条件。某个资源同一时刻只能被一个执行单元持有。锁本身就是互斥资源，数据库行锁、文件锁、连接独占权、actor mailbox owner 也都可能是互斥资源。如果资源天然可共享，比如只读不可变对象，就不满足这个条件。

第二个是占有并等待。一个线程已经持有资源 A，同时还在等待资源 B。单独等待一把锁通常不会形成死锁；危险的是“我手里不放，还想再拿别人的”。

第三个是不可抢占。别人持有的资源不能被强行拿走，只能等持有者主动释放。普通 mutex 就是这样：系统不会因为你等急了就把锁从 owner 手里抢出来。某些资源可以通过超时、取消、事务回滚、lease 过期来模拟“可抢占”，这就是破坏 no preemption 的办法。

第四个是循环等待。存在一个等待环：

```text
T1 持有 A，等待 B；
T2 持有 B，等待 C；
T3 持有 C，等待 A。
```

最小的环是两个线程两把锁：

```text
T1: lock A -> lock B
T2: lock B -> lock A
```

这四个条件放到代码里更好理解：

```go
var a sync.Mutex
var b sync.Mutex

func f() {
    a.Lock()
    defer a.Unlock()

    b.Lock()
    defer b.Unlock()
}

func g() {
    b.Lock()
    defer b.Unlock()

    a.Lock()
    defer a.Unlock()
}
```

如果 `f` 先拿到 `a`，`g` 先拿到 `b`，然后两边都继续拿第二把锁：

```text
f 持有 a，等待 b；
g 持有 b，等待 a。
```

四个条件全部成立：

```text
a/b 是互斥锁；
f/g 都占有一把再等另一把；
mutex 不能被外部抢占；
等待关系形成环。
```

死锁不一定只发生在 mutex 上。channel、WaitGroup、条件变量、线程池、连接池、事务锁也会形成类似结构：

```text
goroutine A 等 goroutine B 发消息；
goroutine B 等 A 释放锁；
事务 1 锁 row x 等 row y；
事务 2 锁 row y 等 row x；
worker 持有队列锁等待任务完成；
任务完成回调又需要同一把队列锁。
```

面试里可以这样回答：

```text
死锁的四个必要条件是互斥、占有并等待、不可抢占和循环等待。互斥表示资源不能同时被多个执行单元使用；占有并等待表示线程拿着已有资源再等新资源；不可抢占表示资源只能由持有者释放；循环等待表示等待图里形成环。工程上避免死锁通常就是破坏其中一个条件，最常见的是破坏循环等待，也就是规定全局锁顺序。
```

一句话：死锁不是“锁多了”就一定发生，而是持有关系和等待关系闭环了。

## Q019. 如何通过锁顺序避免死锁？

**回答：**

通过锁顺序避免死锁，本质上是破坏 circular wait。做法是给所有可能同时持有的锁定义一个全局顺序，任何代码只允许按这个顺序加锁，释放时通常反向释放。

最简单的规则是：

```text
如果 A < B，那么所有地方都只能先 lock A，再 lock B；
禁止先 lock B，再 lock A。
```

这样等待图里就不可能形成环。因为每次等待都只能从低序号资源等高序号资源，边的方向是单调上升的；单调上升不可能绕回起点。

举个账户转账的例子。两个账户各有一把锁：

```go
type Account struct {
    ID      int64
    mu      sync.Mutex
    Balance int64
}
```

错误写法可能是按调用参数顺序加锁：

```go
func Transfer(from, to *Account, amount int64) {
    from.mu.Lock()
    defer from.mu.Unlock()

    to.mu.Lock()
    defer to.mu.Unlock()

    from.Balance -= amount
    to.Balance += amount
}
```

如果同时发生：

```text
Transfer(A, B)
Transfer(B, A)
```

就可能一个拿 A 等 B，另一个拿 B 等 A。

正确方向是按稳定 ID 排序：

```go
func Transfer(from, to *Account, amount int64) {
    first, second := from, to
    if first.ID > second.ID {
        first, second = second, first
    }

    first.mu.Lock()
    defer first.mu.Unlock()

    second.mu.Lock()
    defer second.mu.Unlock()

    from.Balance -= amount
    to.Balance += amount
}
```

这里业务方向仍然是从 `from` 到 `to`，但锁方向只看 ID。这样 `Transfer(A, B)` 和 `Transfer(B, A)` 都会先锁 ID 小的账户，再锁 ID 大的账户。

锁顺序设计里有几个细节很重要。

第一，排序 key 必须稳定。不能用会变化的字段，比如余额、状态、当前队列长度。常见的是 ID、地址、shard index、资源层级。

第二，顺序要跨模块一致。只在一个函数里排序还不够。只要另一个模块用相反顺序，就仍然可能死锁。大型系统通常要把锁层级写进设计文档或代码注释里：

```text
configMu -> shardMu -> entryMu
globalMu -> streamMu -> segmentMu
metadataMu -> workerMu
```

第三，不要在持锁时调用未知代码。外部 callback、interface 方法、用户传入函数、RPC handler 都可能反过来拿别的锁，破坏你以为的顺序。一个常见规则是：

```text
持锁时只改内部状态；
解锁后再调用外部回调或发送通知。
```

第四，动态锁集合也要排序。比如一次要锁多个 shard，不要按请求输入顺序锁，而是先收集 shard id，去重，排序，再逐个加锁：

```go
sort.Ints(shards)
for _, id := range shards {
    locks[id].Lock()
}
defer func() {
    for i := len(shards) - 1; i >= 0; i-- {
        locks[shards[i]].Unlock()
    }
}()
```

第五，能不同时持有多把锁就不要同时持有。锁顺序是必要防线，但更好的设计是减少嵌套锁：

```text
先复制需要的数据，释放锁后计算；
用消息传递交给 owner goroutine；
用事务或单线程 executor 处理跨资源操作；
把跨资源操作拆成可重试状态机。
```

Linux lockdep 文档里的 multi-lock dependency rules 也是这个思想：同一个 lock class 不能递归获取；两把锁不能以相反顺序获取。lockdep 会在内核里跟踪锁依赖，如果发现过去出现过 `L1 -> L2`，后来又出现 `L2 -> L1`，就能报告潜在死锁。

面试里可以这样回答：

```text
锁顺序避免死锁的原理是破坏循环等待。给所有可能嵌套获取的锁定义全局顺序，所有代码都只能按这个顺序加锁，通常反向解锁。比如账户转账不能按 from/to 参数顺序加锁，而要按稳定 account id 排序后加锁。动态锁集合也要先去重排序。这个规则必须跨模块一致，并且持锁期间不要调用可能拿锁的外部代码，否则很容易绕过全局顺序。
```

一句话：锁顺序不是为了好看，而是让等待关系只能单向流动，没机会绕成环。

## Q020. 如何定位线上死锁？

**回答：**

线上定位死锁要先区分两类问题：

```text
真死锁：等待关系成环，相关执行单元永远无法继续；
长时间阻塞/锁竞争：没有环，但某把锁、某个 I/O 或某个下游把请求拖住。
```

这两类现象都可能表现为“请求卡住”“goroutine 很多”“CPU 不高但服务不动”。定位时不要一上来就认定是死锁，要先抓现场。

Go 服务里最有用的第一份证据通常是 goroutine dump。线上如果接了 `net/http/pprof`，可以抓：

```text
curl http://host:port/debug/pprof/goroutine?debug=2
```

或者用：

```text
go tool pprof http://host:port/debug/pprof/goroutine
```

如果没有 HTTP pprof，也可以在进程内用 `runtime.Stack(buf, true)` 打印所有 goroutine 栈，或者通过运维侧触发 SIGQUIT 获取崩溃式栈信息。关键是拿到“所有 goroutine 正在等什么”。

看 goroutine dump 时，先找这些状态：

```text
sync.Mutex.Lock
sync.RWMutex.Lock / RLock
runtime_SemacquireMutex
runtime_SemacquireRWMutex
chan send / chan receive
select
WaitGroup.Wait
Cond.Wait
netpoll / syscall
database/sql 等待连接
```

如果大量 goroutine 都卡在同一个 `Lock` 调用点，要看谁持有这把锁。Go 标准 `sync.Mutex` 不直接暴露 owner，所以要靠栈、日志、profile 和代码推理。常见办法是：

```text
看阻塞栈集中在哪个锁；
回到代码里查这把锁保护什么状态；
找所有 Lock/Unlock 路径；
检查是否有持锁调用外部函数、I/O、channel、等待另一个锁；
检查是否存在 A->B 和 B->A 的相反锁顺序；
检查 defer Unlock 是否一定执行；
检查 panic、return、context cancel 路径是否漏解锁。
```

第二份证据是 block profile 和 mutex profile。Go 官方 pprof 文档里区分得很清楚：

```text
block profile:
  记录阻塞在同步原语上的时间，例如 Mutex、RWMutex、WaitGroup、Cond、channel。

mutex profile:
  记录 contended mutex 的持有者栈，也就是哪些 unlock 栈让别人等了很久。
```

使用前通常要打开采样：

```go
runtime.SetBlockProfileRate(1)
runtime.SetMutexProfileFraction(10)
```

线上不要无脑开满很久，采样比例要结合负载控制。抓取方式可以是：

```text
go tool pprof http://host:port/debug/pprof/block
go tool pprof http://host:port/debug/pprof/mutex
```

goroutine dump 告诉你“现在卡在哪里”；profile 告诉你“一段时间内谁造成了大量等待”。死锁更依赖 dump，锁竞争和 convoy 更依赖 profile。

第三步是构造等待图。对每个卡住的 goroutine 记录：

```text
G1 持有什么锁？
G1 正在等什么锁/chan/WaitGroup/资源？
G2 持有什么？
G2 正在等什么？
```

如果能画出环，比如：

```text
G1 holds A, waits B
G2 holds B, waits A
```

基本就能确认死锁。复杂一点可能是：

```text
G1 holds workflowMu, waits actorLock(id=7)
G2 holds actorLock(id=7), waits queueMu
G3 holds queueMu, waits workflowMu
```

这种时候不要只盯着 mutex。channel 和 WaitGroup 也要放进等待图。很多 Go 线上“死锁”实际是：

```text
持锁发送 channel，但接收方也需要同一把锁；
持锁等待 WaitGroup，worker 完成时回调要拿这把锁；
持锁调用外部接口，外部接口同步回调回来拿同一把锁；
RLock 内尝试 Lock，或者 writer 等待时新 reader 被阻塞，代码误以为读锁可递归。
```

第四步是看最近变更和触发条件。死锁通常不是随机的，它需要特定交错：

```text
新加了一把锁；
把原本锁外的调用移到了锁内；
新增了回调；
新增了跨 shard / 跨 actor / 跨 workflow 操作；
错误处理路径里少了 Unlock；
超时取消后某个 goroutine 不再消费 channel；
读写锁升级或降级逻辑被引入。
```

定位后要尽量写一个最小复现。可以用 `GOMAXPROCS`、barrier channel、sleep 或 hook 强制交错：

```go
step1 := make(chan struct{})
step2 := make(chan struct{})

go func() {
    a.Lock()
    close(step1)
    <-step2
    b.Lock()
}()

go func() {
    <-step1
    b.Lock()
    close(step2)
    a.Lock()
}()
```

这类测试不一定优雅，但能把“线上偶发”变成确定性等待环。修复时再把临时 hook 去掉。

Linux/C/C++ 服务还可以用系统工具补证据：

```text
pstack/gdb 查看线程栈；
strace 看大量线程是否卡在 futex wait；
perf lock record/report/contention 看内核锁事件和等待统计；
lockdep 用于内核锁依赖验证；
/proc/<pid>/stack、/proc/<pid>/task/*/stack 辅助看内核态等待。
```

如果是数据库死锁，也要看数据库自己的等待图和 deadlock 日志。应用层只看到请求卡住，真正的环可能在事务锁里。

修复方向通常有几类：

```text
建立全局锁顺序，消除 A->B / B->A；
减少嵌套锁；
不要持锁做 I/O、channel send、WaitGroup.Wait 或外部回调；
把同步调用改成锁外异步通知；
用 tryLock + 释放重试打破 hold-and-wait；
给等待加超时，至少让现场可恢复和可观测；
为关键锁加等待时间、持锁时间、owner hint 日志。
```

面试里可以这样回答：

```text
线上定位死锁先抓现场，不要只看猜测。Go 服务里先拿 goroutine dump，看哪些 goroutine 卡在 Mutex/RWMutex、channel、WaitGroup 或 Cond 上；再开 block profile 和 mutex profile 区分“真死锁”和“严重锁竞争”。然后回到代码构造等待图，找谁持有什么、又在等什么，重点查相反锁顺序、持锁 I/O、持锁调用回调、漏 Unlock、读写锁升级和 channel/WaitGroup 与锁混用。确认等待环后写最小复现，再通过锁顺序、缩小临界区、锁外回调、超时和观测指标修复。
```

一句话：定位死锁不是看哪把锁“可疑”，而是把等待关系画出来，看它有没有闭环。

## Q021. 如何定位锁竞争导致的 tail latency？

**回答：**

锁竞争导致的 tail latency，通常不是平均延迟先变坏，而是 p95、p99、p999 先冒出来。平均值还挺正常，少数请求却被某把热锁拖住很久。定位时要把“请求变慢”和“锁等待”连起来看，不能只看 CPU 或 QPS。

我一般按这个顺序查。

第一步，看延迟分布，而不是只看平均值。锁竞争的一个典型特征是：

```text
p50 变化不大；
p95 开始变高；
p99/p999 抖得很厉害；
吞吐上去后延迟不是线性增加，而是突然拐弯；
CPU 可能没打满，但 goroutine/thread 等待很多。
```

这说明系统不是所有请求都慢，而是有一部分请求排在某个共享资源后面。锁、连接池、队列、数据库行锁都会长成这个样子。

第二步，把请求生命周期拆成几个阶段。比如一个写请求可以拆成：

```text
排队等待；
解析请求；
获取业务锁；
内存状态更新；
编码；
磁盘写入；
fsync；
返回响应。
```

如果只记录总耗时，你只能知道慢了。要定位锁竞争，至少要在关键锁周围打点：

```go
startWait := time.Now()
mu.Lock()
wait := time.Since(startWait)

holdStart := time.Now()
defer func() {
    hold := time.Since(holdStart)
    mu.Unlock()
    observeLock("store.mu", wait, hold)
}()
```

这里要分清两个指标：

```text
wait time:
  从尝试 Lock 到拿到锁的时间。

hold time:
  从拿到锁到 Unlock 的时间。
```

tail latency 经常是 wait time 被少数长 hold time 放大出来的。一个请求持锁 50ms，后面 100 个请求就可能都等它。这个效果在 p99 上很明显。

第三步，用 Go 的 profile 看证据。Go 的 `runtime/pprof` 文档区分了 block profile 和 mutex profile：

```text
block profile:
  看 goroutine 阻塞在同步原语上的位置，比如 Mutex、RWMutex、WaitGroup、Cond、channel。

mutex profile:
  看 contended mutex 的持有者栈，也就是哪些 Unlock 栈造成了别人等待。
```

这两个视角不一样。block profile 更像“我在哪里等”；mutex profile 更像“谁让我等”。定位 tail latency 时，mutex profile 往往更接近根因，因为它能把等待时间归到持锁临界区的结束位置。

线上可以临时打开采样：

```go
runtime.SetBlockProfileRate(1)
runtime.SetMutexProfileFraction(10)
```

然后抓：

```text
go tool pprof http://host:port/debug/pprof/block
go tool pprof http://host:port/debug/pprof/mutex
```

不要长期无脑开到最大。block profile 和 mutex profile 都有采样成本，生产环境通常要控制采样时间、采样比例和访问权限。

第四步，把 profile 和业务标签连起来。纯 pprof 栈能告诉你哪个函数持锁久，但不一定告诉你哪个 stream、tenant、actor、partition、key 导致热点。锁竞争经常不是“全局都慢”，而是某个 key 热：

```text
tenant=A 的写入集中到一个 shard；
某个 actor 的任务特别多；
某个 stream 的 append 触发 fsync 慢；
某个 metadata key 被所有 worker 刷新；
某个全局 map 被读写混用。
```

所以关键锁最好配合指标：

```text
lock_wait_seconds{lock="store.mu", stream="x"}
lock_hold_seconds{lock="store.mu"}
lock_contention_total{lock="store.mu"}
queue_depth{resource="append"}
inflight{resource="metadata_update"}
```

如果标签基数太高，不要直接把 user_id、request_id 全打进指标。可以用采样日志或 top-K 热点统计。

第五步，看锁内到底做了什么。导致 tail latency 的持锁路径往往有这些东西：

```text
持锁 I/O；
持锁 fsync；
持锁调用下游 RPC；
持锁 JSON 编码大对象；
持锁分配大量内存，触发 GC 压力；
持锁调用外部回调；
持锁等待 channel 或 WaitGroup；
一把全局锁保护多个无关资源。
```

一个常见误判是“锁等待高，所以换 RWMutex”。这不一定有用。如果问题是写锁持有时间长，或者读路径也很短但非常频繁，`RWMutex` 可能只会增加 reader 计数和 writer 等待成本。真正要看的是：谁持锁久，为什么久，有没有必要在锁内做这件事。

第六步，用压测复现拐点。锁竞争导致的 tail latency 通常有一个临界点：

```text
并发 10：p99 正常；
并发 50：p99 开始抖；
并发 100：p99 突然爆炸。
```

压测时不要只看总吞吐，要同时采：

```text
请求 p50/p95/p99；
lock wait p50/p95/p99；
lock hold p50/p95/p99；
goroutine 数；
scheduler latency；
GC pause；
磁盘 I/O 延迟；
下游 RPC 延迟。
```

这样才能判断锁是不是根因。比如 lock hold 变长是因为 fsync 变慢，那根因可能是磁盘；锁只是把磁盘慢放大成了所有请求慢。

面试里可以这样回答：

```text
定位锁竞争导致的 tail latency，要先看 p95/p99，而不是平均值。然后把关键路径拆开，分别记录 lock wait 和 lock hold。Go 里可以用 block profile 看 goroutine 卡在哪里，用 mutex profile 看哪些 Unlock 栈让别人等了多久。再结合业务标签找热点 key、shard、tenant 或 stream。最后回到代码里查锁内是否有 I/O、RPC、channel、WaitGroup、外部回调或大对象处理。修复方向通常是缩小临界区、拆锁、分片、把慢操作移出锁外，或者把热点资源改成单 owner / 队列模型。
```

一句话：tail latency 不是看“有没有锁”，而是看哪把锁在高分位上把请求排成了队。

## Q022. mutex profiling 通常能看到哪些信息？

**回答：**

mutex profiling 看的是锁竞争，不是普通函数耗时。以 Go 为例，`mutex` profile 记录的是 contended mutex 的信息，栈对应的是造成竞争的临界区结束位置，也就是 `Unlock` 附近，而不是等待者调用 `Lock` 的位置。

这个点很重要。很多人第一次看 mutex profile 会奇怪：为什么热点栈在 `Unlock`？原因是等待时间是由持锁者造成的。Go 文档里也用了类似例子：如果一个 goroutine 持锁 1 秒，期间 5 个 goroutine 都在等，那么这次 unlock 栈会贡献大约 5 秒的等待量。

mutex profile 通常能看到这些东西：

```text
1. 哪些锁发生了竞争；
2. 哪些代码路径持锁时让别人等待；
3. 累计等待时间；
4. 采样次数或事件数量；
5. 平均每次竞争造成的等待；
6. 对应的函数调用栈；
7. runtime 内部锁和 sync.Mutex / sync.RWMutex 的竞争。
```

在 `go tool pprof` 里常见的列包括：

```text
flat:
  当前栈顶直接归因的等待成本。

cum:
  当前函数及其子调用累计归因的等待成本。

flat% / cum%:
  占总 mutex wait 成本的比例。
```

具体显示会随着 pprof 命令和视图变化，但判断方法差不多：先看 `top`，再看 `list` 或 `web`，找到高占比的 unlock 路径。

mutex profile 能回答的问题是：

```text
谁持锁让别人等？
哪段临界区最长或最热？
锁等待是集中在一两个栈，还是分散在很多锁？
问题是某个业务锁，还是 runtime 内部锁、日志锁、metrics 锁？
RWMutex 的写锁是不是让大量 reader 等？
```

它不能直接回答的问题也要记住：

```text
它不直接告诉你锁对象的业务名字；
它不一定告诉你哪个 goroutine 是 owner；
它不直接给出请求 ID、tenant、key；
它受采样率影响，不是完整审计日志；
它看到的是竞争后的等待成本，不等于 CPU profile 的执行成本；
短时间偶发死锁更适合看 goroutine dump，而不是只看 mutex profile。
```

所以最好给关键锁加名字和指标。标准库的 `sync.Mutex` 没有名字，你可以在业务层封一层轻量观测：

```go
type NamedMutex struct {
    name string
    mu   sync.Mutex
}

func (m *NamedMutex) Lock() time.Time {
    start := time.Now()
    m.mu.Lock()
    wait := time.Since(start)
    observeWait(m.name, wait)
    return time.Now()
}

func (m *NamedMutex) Unlock(holdStart time.Time) {
    observeHold(m.name, time.Since(holdStart))
    m.mu.Unlock()
}
```

线上排查时，mutex profile 和这些业务指标配合起来更有用。profile 负责告诉你栈，业务指标负责告诉你资源名字和影响范围。

还要和 block profile 分开看。block profile 记录阻塞在同步原语上的位置，栈通常在 `Lock`、channel send/receive、`WaitGroup.Wait` 这一侧。mutex profile 更偏向持锁者。一个完整判断通常是：

```text
block profile:
  大量请求卡在 Store.Append 的 mu.Lock。

mutex profile:
  Store.Append 的 Unlock 栈贡献大量等待时间。

代码检查:
  Append 持锁期间做了 record encode、file write、fsync、index update。
```

这三者能拼成因果链。

面试里可以这样回答：

```text
mutex profiling 通常能看到 contended mutex 的等待成本、采样次数和调用栈。在 Go 里，mutex profile 的栈归因到临界区结束处，也就是 Unlock 位置，因为它衡量的是持锁者让其他 goroutine 等了多久。它适合找“谁持锁太久”或“哪条路径造成大量等待”。但它不直接给锁命名，也不一定告诉你业务 key 或 owner，所以线上最好配合 lock wait/hold 指标、业务标签、goroutine dump 和代码审查一起看。
```

一句话：mutex profile 不是告诉你谁在等，而是告诉你谁让别人等。

## Q023. 临界区中做 I/O 为什么危险？

**回答：**

临界区里做 I/O 危险，是因为 I/O 的耗时不可控，而锁会把这个不可控耗时传播给所有等待同一把锁的请求。

内存操作通常是纳秒级或微秒级。磁盘、网络、数据库、RPC、日志刷盘这些 I/O，可能从几十微秒跳到几毫秒、几十毫秒，极端情况下更久。只要它们发生在锁内，这把锁保护的所有路径都会被拖住。

一个最小例子：

```go
mu.Lock()
defer mu.Unlock()

state[k] = v
_, err := file.Write(buf)
if err == nil {
    err = file.Sync()
}
```

这里锁本来可能只需要保护 `state[k] = v`。但因为写文件和 `Sync` 在锁内，等待者看到的不是“内存状态更新耗时”，而是“内存更新 + 文件系统 + 磁盘 flush + 调度”的总耗时。

危险点主要有几个。

第一，尾延迟会被放大。一次慢 I/O 不只影响当前请求，还影响所有排在锁后的请求。一个 30ms 的 fsync 可能让几十个 goroutine 的 lock wait 都变成 30ms 级别。

第二，吞吐会下降。锁把并行请求串行化，I/O 又让每个串行段变长。系统看上去有很多 goroutine，但真正能推进的很少。

第三，容易形成 lock convoy。一个持锁 I/O 慢了，等待队列变长。等锁释放后，队列还没消化完，下一次慢 I/O 又来了，队列一直存在。

第四，I/O 可能反过来依赖当前进程里的其他锁或 goroutine。比如持锁写日志，日志库内部也有锁；持锁调用 RPC，下游回调或拦截器又访问本地状态；持锁等待数据库连接，连接池回收逻辑又需要别的同步。这些间接依赖会让死锁和长阻塞更难排。

第五，错误和超时路径会变复杂。I/O 可能返回慢、返回错、被 context cancel、半成功。你如果在锁内处理重试、回滚、日志和通知，临界区会继续膨胀。

更稳的写法通常是拆成两段：

```go
mu.Lock()
snapshot := buildSnapshotLocked()
mu.Unlock()

err := writeAndSync(snapshot)

mu.Lock()
applyResultLocked(err)
mu.Unlock()
```

这不是说所有 I/O 都绝对不能在锁内。有些系统必须用锁保护“状态和落盘顺序”的一致性，比如 WAL append 需要保证序列号、buffer、文件偏移和索引的原子关系。这时不是简单把 I/O 移出去就完事，而是要承认它会成为性能瓶颈，然后用更明确的结构处理：

```text
单 writer goroutine；
append queue；
group commit；
批量 fsync；
按 stream/shard 拆锁；
先写 WAL 再异步构建索引；
把锁内工作压缩成“分配序号和交换 buffer”。
```

面试里可以这样回答：

```text
临界区中做 I/O 危险，是因为 I/O 延迟不可控，锁会把一次慢磁盘、网络、数据库或日志操作放大成所有等待者的 lock wait。它会拉高 p99，降低吞吐，形成 lock convoy，还可能通过日志库、连接池、RPC 回调引入隐藏的锁依赖。一般要把 I/O 移出锁外，只在锁内复制状态或提交结果；如果一致性要求必须锁内落盘，就要用单 writer、队列、批处理、group commit 或分片来控制影响范围。
```

一句话：锁内 I/O 的问题不是“当前请求慢”，而是它让所有等这把锁的人一起慢。

## Q024. 临界区中调用外部回调为什么危险？

**回答：**

临界区中调用外部回调危险，是因为你把锁的控制权交给了不受当前模块约束的代码。锁本来是保护内部状态的，外部回调却可能做任何事：再次调用你、拿别的锁、阻塞、panic、发 RPC、写日志、等待 channel。你很难保证它不会破坏锁顺序或拉长临界区。

看一个常见写法：

```go
func (s *Store) Put(k, v string) {
    s.mu.Lock()
    defer s.mu.Unlock()

    s.m[k] = v
    for _, cb := range s.callbacks {
        cb(k, v)
    }
}
```

这个代码的问题不是 `callbacks` 本身，而是它在 `s.mu` 里面执行。回调如果再调用 `Store.Get`：

```go
func callback(k, v string) {
    _ = store.Get(k) // Get 也要 s.mu
}
```

如果 `s.mu` 是不可重入锁，就会自死锁。

回调也可能拿另一把锁：

```text
Put 持有 store.mu -> callback 拿 observer.mu
另一路代码持有 observer.mu -> 调用 store.Get -> 等 store.mu
```

这就是经典的锁顺序反转。当前模块以为自己只拿了一把锁，实际上通过回调把外部锁也卷了进来。

还有一种更隐蔽的情况：回调本身不拿锁，但它很慢。

```text
回调写日志；
回调发 metrics；
回调访问网络；
回调做复杂 JSON 编码；
回调阻塞在 channel send。
```

这些都会把锁持有时间拉长。更麻烦的是，慢在外部代码里，mutex profile 可能显示你的 `Unlock` 栈很热，但真正慢的逻辑藏在回调实现里。

更稳的写法是锁内只生成事件，锁外执行回调：

```go
func (s *Store) Put(k, v string) {
    s.mu.Lock()
    s.m[k] = v
    callbacks := append([]func(string, string){}, s.callbacks...)
    s.mu.Unlock()

    for _, cb := range callbacks {
        cb(k, v)
    }
}
```

如果回调需要看到一致快照，就在锁内复制快照，锁外调用：

```go
s.mu.Lock()
event := Event{
    Key:   k,
    Value: v,
    Seq:   s.seq,
}
s.mu.Unlock()

notify(event)
```

如果必须同步保证“状态更新和通知”之间的顺序，也可以用队列：

```text
锁内追加 event 到 internal queue；
锁外由单独 notifier goroutine 顺序发送；
失败重试和慢订阅者隔离在 notifier 里。
```

面试里可以这样回答：

```text
临界区中调用外部回调危险，是因为外部代码不受当前锁协议约束。它可能重入当前对象导致自死锁，也可能拿其他锁造成锁顺序反转，还可能做 I/O、阻塞或 panic，把锁持有时间拉长。通常的做法是锁内只更新状态并复制需要的事件或回调列表，解锁后再调用外部代码。如果必须保证顺序，可以用内部事件队列或单独 notifier，而不是在持锁状态下直接回调。
```

一句话：持锁调用回调，相当于把你的锁边界交给别人扩写。

## Q025. 持锁期间 panic 或异常会带来什么风险？

**回答：**

持锁期间 panic 或异常最大的风险是锁没有释放，后续所有需要这把锁的执行单元都会卡住。另一个风险是共享状态只更新了一半，锁虽然释放了，但不变量已经坏了。

先看最直接的风险：

```go
mu.Lock()
doSomething()
panic("boom")
mu.Unlock()
```

`panic` 之后，普通控制流不会走到 `Unlock`。如果没有 `defer mu.Unlock()`，这把锁就永久保持 locked 状态。其他 goroutine 再调用 `mu.Lock()` 会一直等。小程序里可能最后报：

```text
fatal error: all goroutines are asleep - deadlock!
```

线上服务里更常见的是部分请求挂住，goroutine 堆积，p99 升高，最后健康检查失败。

第二个风险是状态不一致。即使用了 `defer Unlock`，panic 也可能发生在状态更新中间：

```go
mu.Lock()
defer mu.Unlock()

accounts[from] -= amount
panic("boom")
accounts[to] += amount
```

锁会释放，但总金额不变这个不变量已经被破坏。后面的请求能拿到锁，却会读到坏状态。

所以持锁期间的 panic 处理要分两层：

```text
资源层：
  必须释放锁，避免所有等待者永久卡死。

状态层：
  要么保证临界区内操作不会 panic；
  要么把更新做成可回滚；
  要么先计算再一次性提交；
  要么在 recover 后把对象标记为不可用并触发重建。
```

Go 里常见写法是：

```go
mu.Lock()
defer mu.Unlock()

// 只做简单、可控、不应 panic 的状态更新
state[k] = v
```

如果有可能 panic 的复杂逻辑，最好放到锁外：

```go
newValue, err := buildValue(input) // 可能 panic 或返回 error 的复杂部分尽量别在锁内
if err != nil {
    return err
}

mu.Lock()
state[k] = newValue
mu.Unlock()
```

如果必须在锁内处理多个字段，可以先准备新状态，再短临界区交换：

```go
next := old.Clone()
next.Apply(change)

mu.Lock()
state = next
mu.Unlock()
```

有些语言用异常而不是 panic，道理一样。C++ 里 RAII 的 `lock_guard` / `unique_lock` 可以在异常展开时释放锁；Java 里通常用 `try/finally`；Go 里用 `defer`。这些机制解决的是“锁释放”，不是“业务不变量自动恢复”。

还要注意 recover 的位置。不要随便在持锁代码里 recover 后继续运行：

```go
mu.Lock()
defer mu.Unlock()
defer func() {
    if r := recover(); r != nil {
        // 如果状态已经半更新，只记录日志然后继续，可能更危险
    }
}()
```

如果 recover 之后对象状态已经不可信，应该明确回滚、丢弃局部变更，或者让上层重建这部分状态。

面试里可以这样回答：

```text
持锁期间 panic 或异常有两个风险。第一，锁没有释放，后续所有等待这把锁的线程或 goroutine 都会卡住，所以 Go 里通常用 defer Unlock，Java 用 finally，C++ 用 RAII。第二，即使锁释放了，共享状态也可能只更新一半，不变量被破坏。解决时不能只保证 unlock，还要让临界区足够短、尽量不包含会 panic 的复杂逻辑，必要时先构造新状态再原子替换，或者在 recover 后做明确回滚和故障标记。
```

一句话：`defer Unlock` 能防锁泄漏，但不能自动修好半更新的业务状态。

## Q026. defer unlock 的优缺点是什么？

**回答：**

`defer unlock` 的优点是可靠，缺点是它可能让锁持有时间比你以为的更长。要不要用，关键看函数形状。

最常见写法是：

```go
mu.Lock()
defer mu.Unlock()

// 访问共享状态
```

优点很明显。

第一，它能覆盖多 return 路径：

```go
mu.Lock()
defer mu.Unlock()

if invalid {
    return err
}
if done {
    return nil
}
return update()
```

不需要在每个分支手动 `Unlock`，少很多漏解锁风险。

第二，它能覆盖 panic 路径。函数发生 panic 时，defer 仍会在栈展开过程中执行，所以锁能释放。前面说过，这只保证锁释放，不保证状态一定一致，但至少不会把锁永久卡死。

第三，它让锁的生命周期更清楚。读代码时看到 `Lock` 后马上 `defer Unlock`，基本知道这个函数结束会释放锁。

缺点也很实在。

第一，`defer` 释放锁的时机是函数返回，不是逻辑上“共享状态访问结束”。如果函数后半段还有 I/O、编码、日志、RPC，锁会一直被拿着：

```go
func Handle() error {
    mu.Lock()
    defer mu.Unlock()

    item := state[id]

    // 这里其实已经不需要锁了，但 defer 会让锁继续持有
    payload, _ := json.Marshal(item)
    return send(payload)
}
```

这时应该手动缩小临界区：

```go
mu.Lock()
item := state[id]
mu.Unlock()

payload, _ := json.Marshal(item)
return send(payload)
```

第二，循环里用 defer 很危险：

```go
for _, id := range ids {
    mu.Lock()
    defer mu.Unlock()
    process(id)
}
```

这些 `Unlock` 会等到外层函数返回才执行，不会在每轮循环结束执行。结果是第一轮拿锁后，第二轮可能直接卡住，或者把锁持有到非常晚。循环里要么手动解锁，要么把循环体拆成小函数：

```go
for _, id := range ids {
    func() {
        mu.Lock()
        defer mu.Unlock()
        process(id)
    }()
}
```

第三，defer 有一点运行时成本。现代 Go 对 defer 做过很多优化，普通业务代码通常不该为了这点成本放弃安全性。但在极热路径、极短临界区、每秒百万级调用的地方，可以评估手动 unlock。前提是代码仍然简单，不会引入漏解锁。

第四，`defer` 可能掩盖临界区过大的问题。很多人写：

```go
mu.Lock()
defer mu.Unlock()
```

然后不断往函数后面加逻辑。几个月后，这个函数里已经有日志、metrics、RPC、复杂计算，锁还从头持到尾。代码看起来安全，性能却开始变差。

一个实用规则：

```text
小函数、纯状态读写、多 return：
  用 defer unlock。

长函数、锁后还有慢操作：
  手动 unlock 或拆函数。

循环内部：
  不要直接在外层函数里 defer unlock。

极热路径：
  先 profile，再考虑手动 unlock。
```

面试里可以这样回答：

```text
defer unlock 的优点是防漏解锁，能覆盖多 return 和 panic 路径，代码也更容易审查。缺点是释放时机在函数返回，容易不小心把锁持有到 I/O、RPC、日志、编码等慢操作之后；循环里使用还可能把 unlock 延迟到整个函数结束。我的习惯是：短函数、状态访问清晰时用 defer；临界区只占函数前半段或在循环热路径里，就手动 unlock 或拆小函数，让锁范围和业务范围一致。
```

一句话：`defer unlock` 是安全默认值，但不是扩大临界区的借口。

## Q027. 锁和条件变量如何组合？

**回答：**

条件变量不是单独保护状态的工具。它必须和锁一起用：锁保护共享状态和条件谓词，条件变量负责让等待者睡眠和被唤醒。

Go 的 `sync.Cond` 文档说得很明确：`Cond` 有一个关联的 `Locker`，通常是 `*sync.Mutex` 或 `*sync.RWMutex`；观察或修改条件时要持有这把锁，调用 `Wait` 时也要持有这把锁。POSIX condition variable 也是同一个模型。

一个典型结构是：

```go
type Queue struct {
    mu       sync.Mutex
    notEmpty *sync.Cond
    items    []Item
}

func NewQueue() *Queue {
    q := &Queue{}
    q.notEmpty = sync.NewCond(&q.mu)
    return q
}
```

消费者等待队列非空：

```go
func (q *Queue) Pop() Item {
    q.mu.Lock()
    defer q.mu.Unlock()

    for len(q.items) == 0 {
        q.notEmpty.Wait()
    }

    item := q.items[0]
    q.items = q.items[1:]
    return item
}
```

生产者放入元素并唤醒等待者：

```go
func (q *Queue) Push(item Item) {
    q.mu.Lock()
    q.items = append(q.items, item)
    q.notEmpty.Signal()
    q.mu.Unlock()
}
```

这里锁和条件变量各自做一件事：

```text
mu:
  保护 items 和 len(q.items) > 0 这个条件。

cond:
  在条件不满足时让 goroutine 睡眠；
  在条件可能满足时通知等待者重新检查。
```

`Wait` 的行为也要说清楚。它会原子地释放关联锁并让当前 goroutine 等待。被 `Signal` 或 `Broadcast` 唤醒后，`Wait` 返回前会重新拿回这把锁。所以调用 `Wait` 的代码看起来像这样：

```text
持锁；
检查条件不满足；
Wait 原子释放锁并睡眠；
被唤醒；
Wait 返回前重新持锁；
再次检查条件；
条件满足后在锁内使用共享状态。
```

为什么必须把条件变量和锁配在一起？为了避免 lost wakeup。假设没有锁保护：

```text
消费者检查 len(items)==0，准备睡眠；
生产者 push 一个 item 并 signal；
消费者这时才真正 wait；
signal 已经丢了，消费者可能睡死。
```

锁把“检查条件”和“进入等待”连接起来。`Wait` 原子释放锁并睡眠，生产者修改条件和 signal 也按同一把锁的协议进行，才能避免这个窗口。

`Signal` 和 `Broadcast` 的选择也有讲究：

```text
Signal:
  条件变化最多只需要一个等待者继续，比如队列只新增一个元素。

Broadcast:
  条件变化可能让多个等待者继续，或者等待者的条件不完全相同。
```

如果不确定，`Broadcast` 更保守但更贵，可能唤醒一批 goroutine 重新抢锁，最后只有一个能继续。高并发下这会造成 thundering herd。

Go 里很多简单场景可以用 channel 代替 `sync.Cond`。Go 文档也提醒，简单用例下 channel 往往更合适。但 `Cond` 在这些场景仍然有价值：

```text
多个 goroutine 等待同一个复杂谓词；
需要 Broadcast 唤醒一批等待者；
条件和一组共享状态强绑定；
不想为每个等待者创建 channel；
需要精确控制 Signal/Broadcast。
```

面试里可以这样回答：

```text
锁和条件变量的组合方式是：锁保护共享状态和条件谓词，条件变量负责等待和通知。等待方持锁检查条件，如果条件不满足，就在 while 循环里调用 Wait；Wait 会原子释放锁并睡眠，被唤醒后重新拿锁再返回。通知方持同一把锁修改状态，然后 Signal 或 Broadcast。这样可以避免检查条件和进入睡眠之间丢通知。
```

一句话：条件变量通知的是“条件可能变了”，真正的条件仍然要在锁保护下检查。

## Q028. condition variable 为什么需要 while 循环检查条件？

**回答：**

condition variable 必须用 `while` 循环检查条件，因为被唤醒不等于条件一定成立。`Signal` 或 `Broadcast` 只表示“条件可能变了”，不是把资源直接交给你。

正确写法是：

```go
mu.Lock()
for !condition() {
    cond.Wait()
}
// condition is true, use shared state
mu.Unlock()
```

错误写法是：

```go
mu.Lock()
if !condition() {
    cond.Wait()
}
// 这里不能保证 condition 仍然为 true
mu.Unlock()
```

为什么 `if` 不够？原因有几个。

第一，多个等待者会竞争同一个条件。比如队列里新增一个元素，生产者 `Signal` 唤醒一个消费者。但调度不是你能完全控制的。被唤醒的消费者重新拿锁之前，另一个消费者可能已经拿到锁并取走了元素。等它真正从 `Wait` 返回时，队列又空了。

第二，`Broadcast` 会唤醒所有等待者。假设队列里只有 1 个元素，却 `Broadcast` 唤醒 100 个消费者。第一个消费者拿走元素后，剩下 99 个必须重新检查条件，发现不满足再睡回去。如果用 `if`，它们会继续执行并读空队列。

第三，有些系统允许 spurious wakeup。POSIX 条件变量的通用使用习惯就是循环检查谓词。Go 的 `sync.Cond` 文档说 `Wait` 不会无缘无故返回，必须由 `Broadcast` 或 `Signal` 唤醒；但它仍然明确要求调用方通常不能假设返回时条件为真，而应该在循环里等待。原因不是 Go 会随便唤醒你，而是条件可能在你被唤醒到重新拿锁之间已经被别人改变。

第四，等待条件可能不是所有等待者都一样。比如多个 goroutine 都等在同一个 `Cond` 上：

```text
G1 等 queue size >= 1；
G2 等 queue size >= 10；
G3 等 closed == true；
```

一次 `Broadcast` 只是让大家重新检查。不是每个人的条件都会满足。

一个队列例子最直观：

```go
func (q *Queue) Pop() Item {
    q.mu.Lock()
    defer q.mu.Unlock()

    for len(q.items) == 0 {
        q.notEmpty.Wait()
    }

    item := q.items[0]
    q.items = q.items[1:]
    return item
}
```

这里 `for` 做了两件事：

```text
等待前检查，避免条件已经满足时还睡眠；
醒来后再检查，避免资源已经被别人消耗。
```

面试里可以这样回答：

```text
condition variable 要用 while 循环，是因为被唤醒只代表条件可能发生变化，不代表条件现在一定成立。等待者醒来后要重新竞争锁，在它拿回锁之前，资源可能已经被其他线程消耗；Broadcast 也会唤醒很多等待者，但可能只有一部分能继续。POSIX 里还要考虑 spurious wakeup。Go 的 Cond 虽然 Wait 只会被 Signal/Broadcast 唤醒，官方文档仍然要求在循环里检查条件，因为条件本身可能已经变了。
```

一句话：`Wait` 返回只给你一次重新检查条件的机会，不给你条件成立的承诺。

## Q029. semaphore 和 mutex 的区别是什么？

**回答：**

`mutex` 解决的是互斥访问，`semaphore` 解决的是资源额度控制。两者都可能让调用方等待，但语义不同。

mutex 通常只有两个状态：

```text
unlocked
locked
```

同一时刻最多一个执行单元进入临界区。它保护的是一组共享状态和不变量：

```go
mu.Lock()
balance += delta
mu.Unlock()
```

semaphore 维护的是一个计数。计数表示还有多少资源额度可用。获取时减少额度，释放时增加额度。只要额度够，多个执行单元可以同时通过。

比如限制最多 10 个并发请求访问下游：

```go
sem := semaphore.NewWeighted(10)

if err := sem.Acquire(ctx, 1); err != nil {
    return err
}
defer sem.Release(1)

callDownstream()
```

这里不是要保护某个 map 的一致性，而是要限制并发量。最多 10 个 goroutine 可以同时进入下游调用。

可以这样对比：

```text
mutex:
  容量通常是 1；
  强调互斥；
  保护共享状态；
  临界区里通常有不变量；
  错误使用会造成 data race 或状态损坏。

semaphore:
  容量可以是 N；
  强调限流和资源池；
  控制并发进入数量；
  不一定保护具体共享状态；
  错误使用常见为漏 Release，导致额度耗尽。
```

二值 semaphore 看起来像 mutex，但仍然有语义差异。很多 mutex 有 owner 概念，要求谁 lock 谁 unlock；Go 的 `sync.Mutex` 不绑定 goroutine owner，但语义上仍然是互斥锁。semaphore 更像“许可证”，谁 acquire 不一定必须同一个执行单元 release，具体取决于实现和约定。这个灵活性有时有用，也更容易写乱。

semaphore 适合这些场景：

```text
限制同时处理的任务数；
限制同时打开的文件数；
限制数据库连接或下游 RPC 并发；
按权重占用资源，比如一个大任务占 5 个 token，小任务占 1 个 token；
实现 worker pool 的背压。
```

mutex 适合这些场景：

```text
保护 map、slice、计数器、状态机；
保护多个字段之间的不变量；
让读改写序列原子化；
避免并发访问非线程安全对象。
```

不要用 semaphore 代替 mutex 来保护复杂状态，除非你非常清楚自己在做什么。比如把 semaphore 容量设成 1，可以做到一次只放一个 goroutine 进去，但它不会天然表达“这个锁保护哪些字段”“Unlock 和 Lock 的 happens-before 关系如何在语言内存模型里定义”。在 Go 里，共享内存保护优先用 `sync.Mutex` / `sync.RWMutex`，限并发再用 semaphore 或 channel。

也不要用 mutex 做资源池限流。比如限制最多 10 个请求，用 mutex 只能一次一个，太保守；用计数 semaphore 才能表达“最多 N 个”。

面试里可以这样回答：

```text
mutex 是互斥锁，核心语义是同一时刻只能一个执行单元进入临界区，用来保护共享状态和不变量。semaphore 是计数信号量，核心语义是控制资源额度，允许最多 N 个执行单元同时通过。mutex 更适合 map、状态机、多个字段一致性；semaphore 更适合限并发、连接池、下游调用额度和加权资源。二值 semaphore 可以模拟 mutex 的一部分行为，但语义上不如 mutex 清楚，也更容易漏 release 或破坏所有权约定。
```

一句话：mutex 管“这段状态只能一个人改”，semaphore 管“这个资源最多放几个人进来”。

## Q030. barrier、latch、countdown latch 解决什么同步问题？

**回答：**

`barrier`、`latch`、`countdown latch` 都是协调多个执行单元进度的工具。它们不像 mutex 那样保护共享状态，也不像 semaphore 那样控制资源额度。它们解决的是“什么时候可以继续往下走”的问题。

先说 latch。latch 可以理解成一次性门闩。门关着时，等待者都过不去；门打开后，所有等待者都能通过，后来的等待者也直接通过。它通常是 one-shot 的。

一个简单场景：

```text
主线程完成配置加载；
worker 在配置未完成前不能开始；
配置加载完成后打开 latch；
所有 worker 开始运行。
```

Java 的 `CountDownLatch(1)` 就可以当这种开关门用。

`countdown latch` 是带计数的 latch。它初始化时有一个 count，等待者调用 `await` 阻塞；其他线程完成一部分工作后调用 `countDown`。当计数降到 0，所有等待者释放。Java 官方文档也强调它是 one-shot：计数归零后不能 reset；如果需要可重用版本，要考虑 barrier 一类工具。

典型场景是主线程等 N 个 worker 都完成：

```text
done = CountDownLatch(N)

worker 1 完成 -> countDown
worker 2 完成 -> countDown
...
worker N 完成 -> countDown 到 0

driver await 返回，继续汇总结果
```

Go 里的 `sync.WaitGroup` 很像 countdown latch：

```go
var wg sync.WaitGroup
wg.Add(n)

for i := 0; i < n; i++ {
    go func() {
        defer wg.Done()
        work()
    }()
}

wg.Wait()
```

它解决的是“等一组 goroutine 完成”，不是保护共享变量。共享变量仍然要用锁、channel 或其他同步方式处理。

barrier 解决的是另一种问题：一组线程必须都到达某个阶段，才能一起进入下一阶段。POSIX `pthread_barrier_wait` 的语义就是参与线程在 barrier 处阻塞，直到要求数量的线程都调用了 wait；到齐后大家继续。Java `CyclicBarrier` 也类似，而且可以重用，所以叫 cyclic。

典型场景是分阶段并行计算：

```text
阶段 1：每个 worker 处理自己那块数据；
barrier：等待所有 worker 都完成阶段 1；
阶段 2：所有 worker 基于阶段 1 的完整结果继续；
barrier：等待所有 worker 都完成阶段 2；
阶段 3：继续。
```

这和 countdown latch 的差异在于：

```text
countdown latch:
  通常是一方或多方等待 N 个事件完成；
  count 到 0 后释放；
  多数实现是一次性的；
  等待者不一定也是参与完成 count 的那批线程。

barrier:
  一组参与者彼此等待；
  所有人都到齐后一起继续；
  常见实现可以复用多个阶段；
  等待者通常就是参与者。
```

再对比 mutex 和 semaphore：

```text
mutex:
  保护共享状态，一次一个。

semaphore:
  控制资源额度，最多 N 个。

latch/countdown latch:
  等某个开关或 N 个完成事件。

barrier:
  等一组参与者全部到达同一个阶段。
```

这些工具的错误用法也很常见。

第一，countdown latch 漏掉一次 `countDown` / `Done`，等待方会永远卡住。Go 里通常写 `defer wg.Done()`，避免中途 return 或 panic 前漏掉。

第二，barrier 参与者数量必须准确。如果期望 10 个线程到齐，实际只有 9 个能到，剩下 9 个会一直等。线程池里尤其容易出问题：任务数大于 worker 数，worker 又在 barrier 里互等，可能把线程池占满。

第三，不要用 barrier 保护共享状态。barrier 只能保证阶段顺序，不能替代锁。如果阶段内多个 worker 同时写同一个 map，仍然会 race。

第四，latch 是一次性语义。如果你需要反复开关，不要硬拿 one-shot latch 复用，要用 condition variable、channel、CyclicBarrier、Phaser 或自己明确建状态机。

面试里可以这样回答：

```text
barrier、latch、countdown latch 解决的是执行进度协调。latch 像一次性门闩，打开前等待，打开后都能通过；countdown latch 等 N 个事件完成，计数到 0 后释放等待者，Java CountDownLatch 和 Go WaitGroup 都是这个方向；barrier 等一组参与者全部到达同一个阶段后再一起继续，常用于分阶段并行计算，CyclicBarrier 可以反复用于多个阶段。它们不负责保护共享状态，也不是限流工具；共享状态仍然要用锁、channel 或原子操作保护。
```

一句话：mutex 管互斥，semaphore 管额度，latch 和 barrier 管大家什么时候继续。

## Q031. 乐观锁和悲观锁的区别是什么？

**回答：**

乐观锁和悲观锁的区别，核心在于它们对冲突的假设不同。

悲观锁的假设是：冲突很可能发生，所以先把资源锁住，再访问或修改。典型做法是：

```text
先加锁；
读当前状态；
修改状态；
提交；
释放锁。
```

比如 Go 里的 `sync.Mutex`：

```go
mu.Lock()
balance += delta
mu.Unlock()
```

数据库里也有类似思路，比如显式行锁、`SELECT ... FOR UPDATE`、表锁、事务锁。PostgreSQL 文档里把 MVCC 和显式锁都放在并发控制一章里讲：MVCC 让读写少互相阻塞，但表级、行级、advisory lock 仍然存在，用来处理应用想明确管理冲突点的场景。

乐观锁的假设是：冲突不常发生，所以先不阻塞别人，读出数据后正常计算，提交时再检查数据有没有被别人改过。常见做法是 version 字段、CAS、时间戳、etag。

一个典型 SQL 更新：

```sql
UPDATE account
SET balance = balance + 100, version = version + 1
WHERE id = 7 AND version = 12;
```

如果影响行数是 1，说明 version 没变，提交成功。如果影响行数是 0，说明中间有人改过这行，当前事务要重读、重算、重试，或者把冲突返回给上层。

CAS 也是乐观锁的味道：

```go
for {
    old := atomic.LoadInt64(&value)
    next := old + delta
    if atomic.CompareAndSwapInt64(&value, old, next) {
        break
    }
}
```

这里没有先拿互斥锁。它先基于 `old` 算 `next`，最后用 CAS 验证：如果当前值仍然是 `old`，就改成 `next`；如果不是，说明冲突了，重试。

两者的对比可以这样记：

```text
悲观锁:
  冲突前置处理。
  先阻塞别人，再修改。
  等待成本明确。
  冲突多时稳定。
  容易引入死锁、lock wait、lock convoy。

乐观锁:
  冲突后置处理。
  先并发执行，提交时校验。
  无冲突时路径很轻。
  冲突多时大量重试和废弃工作。
  适合读多写少、冲突概率低、计算可重放的场景。
```

乐观锁不是“没有锁”，更准确地说，它把互斥范围压缩到了提交校验的那一瞬间。CAS 要靠硬件原子指令；数据库 version update 也会在数据库内部拿必要的行锁或做并发控制。只是从应用代码的角度看，它没有提前把业务对象长时间锁住。

选型时要看冲突率和重试成本。

如果是商品库存扣减、热点账户余额、同一个 actor 的顺序状态，冲突很高。乐观锁会变成很多请求同时读到旧版本，然后一起提交失败，再一起重试。此时悲观锁、队列化、单 owner goroutine、按 key 串行执行，往往更稳。

如果是用户资料编辑、配置更新、读多写少的缓存刷新，冲突很低。悲观锁会让大量本来不冲突的操作排队，乐观锁通常更合适。

面试里可以这样回答：

```text
悲观锁认为冲突很可能发生，所以先拿锁再读写，典型是 Mutex、行锁、SELECT FOR UPDATE。它冲突多时稳定，但会带来等待、死锁和尾延迟。乐观锁认为冲突较少，所以先并发执行，提交时用 version、CAS、etag 或条件更新检查有没有被别人改过；成功就提交，失败就重试或返回冲突。乐观锁无冲突时很轻，但冲突率高时会出现大量失败重试和废弃工作。
```

一句话：悲观锁先排队再干活，乐观锁先干活再检查有没有白干。

## Q032. 乐观锁冲突率升高时会发生什么？

**回答：**

乐观锁冲突率升高时，系统最先变坏的通常不是正确性，而是吞吐、尾延迟和资源利用率。

乐观锁的正常路径是：

```text
读取版本 v；
基于 v 做计算；
提交时检查版本仍然是 v；
如果没变，提交成功。
```

冲突率低时，这条路径很好。大家很少互相覆盖，几乎不用等待。

冲突率高时，流程会变成：

```text
很多请求同时读到版本 v；
大家都基于 v 做计算；
只有一个请求能把版本从 v 改成 v+1；
其他请求提交失败；
失败请求重新读取 v+1；
重新计算；
再次竞争。
```

问题就出在“重新计算”和“再次竞争”上。

第一，会浪费 CPU 和下游资源。乐观锁失败前做过的校验、计算、序列化、RPC 准备、SQL 构造，都可能白干。如果业务逻辑很重，冲突失败不是一个便宜的错误码，而是一堆浪费。

第二，tail latency 会升高。一个请求如果第一次提交失败，要重读、重算、重试。重试一次还好，连续失败几次，p99 会很难看。

第三，会出现 retry storm。冲突失败后，如果所有请求马上重试，它们会再次撞在同一个热点版本上。越失败越重试，越重试越拥挤，最后把数据库、CPU 或队列打满。

第四，可能出现近似 livelock。系统一直在运行，日志里全是 retry，但真正成功的提交很少。它不是死锁，因为每轮总有人成功；但整体效率很差。

第五，版本粒度太粗时，会产生伪冲突。比如整张用户表只有一个全局 version，两个请求修改用户不同字段也互相冲突。或者一个大 JSON 文档只有一个 version，不同子路径更新也互相打架。这种冲突不是业务上真的不能并发，而是版本设计太粗。

第六，如果重试逻辑没有幂等边界，会带来副作用重复。比如乐观锁失败前已经发送消息、调用外部支付、写了审计日志，再重试就可能重复。乐观锁要求“失败后可安全重放”，这点经常被低估。

应对办法要看冲突原因。

如果冲突是短时间热点，可以加退避和抖动：

```text
失败后不要立刻重试；
指数退避；
加随机 jitter；
限制最大重试次数；
超过次数返回 409 conflict 或进入异步队列。
```

如果冲突是长期热点，退避只是止血。要改结构：

```text
把热点 key 单 owner 化；
按 key 排队；
使用悲观锁或 SELECT FOR UPDATE；
把一个大对象拆成多个版本域；
把计数类更新改成原子增量或 append event；
做分片计数，最后聚合；
把读改写改成数据库原子 UPDATE。
```

比如计数器不要写成：

```text
read count/version -> count+1 -> compare version update
```

高并发下它会冲突很厉害。更好的 SQL 可能是：

```sql
UPDATE counter SET value = value + 1 WHERE id = 1;
```

这让数据库内部处理并发更新，应用层少做无意义重试。

面试里可以这样回答：

```text
乐观锁冲突率升高后，会出现大量提交失败和重试。无冲突时省掉的等待成本，会变成冲突时的废弃计算、重读重算、retry storm 和 p99 上升。严重时系统看起来很忙，但成功提交很少，接近 livelock。处理时要先看冲突是不是热点 key 或版本粒度太粗；短期可以做退避、jitter、重试上限，长期要拆版本域、分片、队列化、单 owner，或者对高冲突资源改用悲观锁和数据库原子更新。
```

一句话：乐观锁怕的不是失败一次，而是很多人一起失败后又一起重试。

## Q033. lock striping 的基本思路是什么？

**回答：**

`lock striping` 的基本思路是：不要让所有数据共用一把锁，而是把数据按 key 映射到多个 stripe，每个 stripe 有自己的锁。这样不同 stripe 上的操作可以并行，同一个 stripe 内仍然串行。

最简单的结构：

```go
type StripedMap struct {
    stripes []struct {
        mu sync.Mutex
        m  map[string]string
    }
}
```

访问时按 hash 选 stripe：

```go
func (s *StripedMap) stripeFor(key string) *stripe {
    h := hash(key)
    return &s.stripes[h%uint64(len(s.stripes))]
}

func (s *StripedMap) Get(key string) (string, bool) {
    st := s.stripeFor(key)
    st.mu.Lock()
    defer st.mu.Unlock()
    v, ok := st.m[key]
    return v, ok
}

func (s *StripedMap) Put(key, value string) {
    st := s.stripeFor(key)
    st.mu.Lock()
    defer st.mu.Unlock()
    st.m[key] = value
}
```

如果原来只有一把全局锁：

```text
所有 key: lock globalMu
```

lock striping 后变成：

```text
key=a -> stripe 3 -> lock stripes[3].mu
key=b -> stripe 9 -> lock stripes[9].mu
key=c -> stripe 3 -> lock stripes[3].mu
```

`a` 和 `b` 可以并行，`a` 和 `c` 仍然串行。它不是无锁，只是把一个大排队队列拆成多个小队列。

这个思路适合这些场景：

```text
按 key 访问 map/cache/session；
多数操作只触碰一个 key；
不同 key 之间没有强一致不变量；
全局锁竞争明显；
数据规模不小，值得用多个锁换并行度。
```

它不适合这些场景：

```text
操作经常跨多个 key；
全局不变量必须一次性维护；
热点集中在少数 key；
key 分布极度倾斜；
stripe 数量设置太少或太多；
遍历、size、clear 这类全局操作很频繁。
```

跨 stripe 操作要格外小心。比如从 keyA 转移到 keyB，要锁两个 stripe：

```go
sa := s.stripeFor(keyA)
sb := s.stripeFor(keyB)
```

如果直接按业务顺序加锁，可能死锁：

```text
G1: lock stripe 1 -> lock stripe 2
G2: lock stripe 2 -> lock stripe 1
```

所以跨 stripe 时要按 stripe index 排序：

```text
先锁 index 小的 stripe；
再锁 index 大的 stripe；
同一个 stripe 只锁一次。
```

Java 的 `ConcurrentHashMap` 文档也体现了类似目标：检索操作通常不阻塞，可以和更新操作重叠；构造参数里还有 `concurrencyLevel` 作为内部 sizing hint。现代实现不一定就是老版本那种固定 Segment 锁，但设计目标仍然是避免整张表被一把锁串行化。

面试里可以这样回答：

```text
lock striping 是把一把全局锁拆成多把 stripe 锁，根据 key 的 hash 选择其中一把。这样不同 stripe 的 key 可以并发访问，同一 stripe 内仍然用锁保证一致性。它适合大多数操作只访问单个 key、key 分布较均匀的 map/cache/session 场景。代价是内存和实现复杂度增加，跨 stripe 操作要按固定顺序加锁，否则会引入新的死锁风险。
```

一句话：lock striping 是把一个大锁队列拆成多个小锁队列。

## Q034. 分段锁如何影响内存占用和热点倾斜？

**回答：**

分段锁能降低锁竞争，但不是免费。它会增加内存占用，也可能被热点倾斜打穿。

先看内存占用。原来一把锁加一张 map：

```go
type Store struct {
    mu sync.Mutex
    m  map[string]Value
}
```

分段后变成很多段：

```go
type Store struct {
    shards []struct {
        mu sync.Mutex
        m  map[string]Value
    }
}
```

额外成本包括：

```text
每个 shard 一把锁；
每个 shard 一个 map header；
每个 shard 可能有独立 buckets 和扩容空洞；
每个 shard 的统计指标、队列、条件变量；
为了避免 false sharing 可能还要 padding；
初始化和 GC 扫描对象变多。
```

如果 shard 数很大，而数据量很小，内存浪费会明显。比如 1024 个 shard，每个 shard 只有几个 key，很多 map 的桶都不满，锁对象和 map 元数据反而占了不少。

再看热点倾斜。分段锁的效果依赖 key 分布。理想情况是 key 均匀落到各个 shard：

```text
64 个 shard；
热点请求大致平均；
每把锁承担约 1/64 的竞争。
```

但真实业务经常不是这样：

```text
一个超级热点用户；
一个热门直播间；
一个全局配置 key；
一个高频 actor；
一个 stream 承载绝大多数 append。
```

如果 80% 请求都打到同一个 key，那么不管你有 16 个 shard 还是 1024 个 shard，这些请求仍然落在同一把锁后面。分段锁只能拆散“不同 key 的竞争”，不能拆散“同一个 key 的竞争”。

还有一种倾斜来自 hash 或取模设计。比如 key 有模式，而 hash 函数很差，很多 key 落到同一个 shard。或者 shard 数是 2 的幂，hash 低位质量不好，用低位取模就会偏。工程上要用稳定、分布好的 hash，并观察每个 shard 的请求量。

分段锁还会影响全局操作。比如 `Len`、`Snapshot`、`Clear`：

```text
Len:
  要么逐段加锁求和；
  要么维护全局原子计数；
  要么接受近似值。

Snapshot:
  可能要锁所有 shard；
  锁顺序必须固定；
  锁持有时间可能很长。

Clear:
  要么逐段清理；
  要么整体替换 shard 指针；
  要考虑并发读写语义。
```

如果全局操作很频繁，分段锁会把问题从“单把锁竞争”变成“多把锁编排复杂”。

选择 shard 数时，不是越多越好。常见考虑：

```text
CPU 核数；
并发 goroutine 数；
key 分布；
每个 shard 的 map 大小；
全局操作频率；
内存预算；
是否需要 padding 避免 false sharing。
```

一个实用经验是：先从 16、32、64、128 这类数量试起，用压测看 lock wait、p99、内存和 shard 分布。不要凭感觉上来就 4096 个锁。

热点倾斜严重时，分段锁不够，需要更针对性的设计：

```text
热点 key 单独拆出来；
对热点 key 做队列化；
按业务维度重新分片；
对计数类热点做分片计数再聚合；
对只读热点用 atomic.Value 或 copy-on-write；
对同一 actor/stream 用单 owner goroutine。
```

面试里可以这样回答：

```text
分段锁用更多锁换并行度，会增加锁对象、map header、bucket、指标和 padding 的内存成本。段数太多而数据少时，元数据和空桶浪费明显。它还依赖 key 分布，如果热点集中在一个 key 或一个 shard，再多分段也解决不了这条热点队列。全局操作也会更复杂，因为 Len、Snapshot、Clear 可能要锁多个 shard。实际要结合 CPU 核数、key 分布、内存预算和压测结果选段数，并对超级热点做单独治理。
```

一句话：分段锁能缓解均匀竞争，救不了单点热点。

## Q035. 如何设计锁层级避免循环依赖？

**回答：**

设计锁层级的目标，是让所有锁获取路径形成一个有向无环图。只要等待关系不能绕回起点，就不会出现锁顺序导致的死锁。

最简单的办法是给锁分层、编号：

```text
Level 1: global/config lock
Level 2: tenant/shard lock
Level 3: stream/actor lock
Level 4: entry/task lock
```

规则是：

```text
只能从低层级编号拿到高层级编号；
不能从高层级反过来拿低层级；
同层多个锁必须按稳定 key 排序；
释放通常按相反顺序。
```

比如：

```text
configMu -> shardMu -> streamMu -> entryMu
```

禁止：

```text
streamMu -> configMu
entryMu -> shardMu
actorMu(id=9) -> actorMu(id=3)
```

Linux lockdep 文档里的 multi-lock dependency rule 也是这个思路：同一个 lock class 不能递归获取，两把锁不能出现相反顺序。过去出现过 `L1 -> L2`，后来又出现 `L2 -> L1`，就说明存在 lock inversion deadlock 风险。

设计时要处理几类细节。

第一，同层动态锁要排序。比如一次要锁多个 actor：

```text
actor: 9, 3, 7
```

必须先排序：

```text
3 -> 7 -> 9
```

不要按请求输入顺序，也不要按业务方向。

第二，跨模块调用要规定方向。比如 control 层、metadata 层、logstore 层之间要明确：

```text
control 可以调用 metadata；
metadata 不能在持锁状态下回调 control；
logstore 不在持锁状态下调用上层 callback。
```

循环依赖经常不是两行代码直接反着拿锁，而是跨模块绕出来：

```text
Service.mu -> Metadata.Update -> callback -> Service.Get -> Service.mu
```

第三，持锁期间不要调用外部回调、接口方法、RPC handler。外部代码可能拿任何锁，它会绕过你设计的层级。更稳的做法是锁内构造事件，锁外通知。

第四，锁层级要和 ownership 一起写清楚。只写“先 A 后 B”还不够，要说明：

```text
这把锁保护哪些字段；
哪些方法要求调用者已持锁；
哪些方法禁止持锁调用；
跨 shard 操作怎么排序；
是否允许 tryLock 失败后释放重试。
```

第五，对历史代码要用工具和测试补强。Go 没有内置 lockdep，但可以用这些办法：

```text
代码审查检查 Lock 调用点；
给 debug build 包一层 ranked mutex；
运行时记录当前 goroutine 持有的 lock rank；
发现低 rank 在高 rank 后被获取就 panic；
用 stress test 和 race test 覆盖跨锁路径。
```

一个简单 ranked lock 思路：

```go
type RankedMutex struct {
    rank int
    mu   sync.Mutex
}
```

debug 模式下，`Lock` 时检查当前 goroutine 已持有的最高 rank，禁止倒序获取。生产环境可以去掉这层成本。

面试里可以这样回答：

```text
避免循环依赖的办法是给锁建立全局层级，让所有代码只能按同一个方向拿锁。比如 global -> shard -> stream -> entry，同层多个锁按稳定 ID 排序。跨模块调用也要遵守方向，持锁期间不要调用外部回调或上层接口。这样等待图只能单向增长，不能形成环。大型系统里最好把每把锁保护的字段、允许的调用方向和跨锁排序规则写进代码注释，并用 review、debug ranked lock 或类似 lockdep 的机制检查反向顺序。
```

一句话：锁层级的价值，是把“大家凭习惯加锁”变成“等待关系不可能成环”。

## Q036. 锁保护的数据结构如何定义 ownership？

**回答：**

锁保护的数据结构要先定义 ownership，否则锁只是散落在代码里的几个 `Lock` 调用。ownership 说的是：谁拥有这份状态，谁可以直接读写它，读写时必须遵守哪把锁或哪条串行化规则。

一个清楚的 ownership 定义通常包括四件事：

```text
1. 哪些字段属于同一组状态；
2. 这些字段由哪把锁保护；
3. 哪些方法可以直接访问这些字段；
4. 状态是否允许把指针或引用暴露给外部。
```

比如：

```go
type Store struct {
    mu sync.Mutex // protects streams, nextSeq, closed

    streams map[string]*Stream
    nextSeq map[string]uint64
    closed  bool
}
```

这个注释看起来简单，但它明确了边界：`streams`、`nextSeq`、`closed` 是同一组受保护状态，访问它们要拿 `mu`。

更好的方法是把字段设成 unexported，通过方法访问：

```go
func (s *Store) Append(stream string, record Record) (uint64, error) {
    s.mu.Lock()
    defer s.mu.Unlock()

    if s.closed {
        return 0, ErrClosed
    }
    seq := s.nextSeq[stream]
    s.nextSeq[stream] = seq + 1
    return seq, nil
}
```

外部拿不到 `nextSeq` 的 map，就不容易绕过锁。

最危险的是把受保护对象的内部指针直接返回：

```go
func (s *Store) GetStream(id string) *Stream {
    s.mu.Lock()
    defer s.mu.Unlock()
    return s.streams[id]
}
```

如果 `Stream` 本身也有可变字段，调用者拿到指针后可以在锁外修改它。这样 `Store.mu` 保护边界就被穿透了。更稳的做法是：

```text
返回只读快照；
返回深拷贝；
让 Stream 自己有独立锁并定义 ownership；
只暴露操作方法，不暴露内部对象指针。
```

ownership 还要处理方法分层。常见模式是：

```go
func (s *Store) Put(k, v string) {
    s.mu.Lock()
    defer s.mu.Unlock()
    s.putLocked(k, v)
}

func (s *Store) putLocked(k, v string) {
    s.m[k] = v
}
```

这里 `putLocked` 的名字说明调用者必须已持锁。不要让一个 helper 有时自己加锁、有时假设已持锁，这会很难审。

还有一种 ownership 不是“锁拥有状态”，而是“goroutine 拥有状态”。也就是 actor / single owner 模型：

```text
某个 goroutine 独占 state；
其他 goroutine 通过 channel 发 command；
owner goroutine 顺序处理 command；
外部不直接读写 state。
```

这也是一种清晰 ownership。它减少了显式锁，但并没有取消同步，只是把同步点变成消息队列。

判断 ownership 是否清楚，可以问几个问题：

```text
看到一个字段，能不能立刻知道谁保护它？
有没有锁外返回内部 map/slice/pointer？
有没有方法名标明 locked 前置条件？
有没有两个锁都声称保护同一个字段？
有没有某条路径不拿锁直接访问？
跨对象操作时，锁顺序是否明确？
```

面试里可以这样回答：

```text
锁保护的数据结构要把 ownership 写清楚：哪些字段是一组共享状态，由哪把锁保护，哪些方法能读写它，是否允许内部指针逃逸。字段最好 unexported，通过方法访问；需要内部 helper 时用 putLocked 这类命名说明调用者已持锁。不要锁内取出指针后在锁外随意改，也不要让两把锁同时模糊地保护同一字段。另一种做法是 single owner goroutine，让状态只在一个 goroutine 内修改，其他人通过消息访问。
```

一句话：ownership 清楚，锁才知道自己到底在保护什么。

## Q037. 为什么复制带锁的结构体通常危险？

**回答：**

复制带锁的结构体危险，是因为你复制的不只是业务字段，还复制了锁的内部状态。复制后，原对象和副本可能共享底层数据，却用不同的锁保护；也可能把一个已经 locked 或有 waiter 的锁状态复制出去，得到一个永远不该存在的锁副本。

看一个简单结构：

```go
type Cache struct {
    mu sync.Mutex
    m  map[string]string
}
```

如果被复制：

```go
c2 := c1
```

`map` 是引用语义，`c1.m` 和 `c2.m` 指向同一张底层 map。但 `c1.mu` 和 `c2.mu` 是两把不同的 mutex 副本。于是：

```text
goroutine A: c1.mu.Lock(); c1.m["x"] = "1"
goroutine B: c2.mu.Lock(); c2.m["x"] = "2"
```

两边都以为自己拿了锁，实际上拿的是不同锁。共享 map 仍然并发写，数据竞争照样发生。

如果复制发生在锁已经用过以后，还会更糟。Go 官方文档明确说 `Mutex`、`RWMutex`、`Cond`、`WaitGroup`、`Pool` 等 sync 类型第一次使用后不能复制。原因是这些类型内部有状态，比如：

```text
locked bit；
waiter count；
semaphore；
reader count；
writer pending 状态；
等待队列关联。
```

这些状态和运行时等待队列、唤醒语义有关。简单字节复制不会把“等待关系”正确迁移过去。

复制带锁结构体的常见来源有：

```text
函数参数按值传递；
方法使用 value receiver；
从函数按值返回；
放进 slice 后扩容复制；
for range 复制元素；
map value 是结构体，取出后修改的是副本；
JSON/protobuf 编解码复制了包含锁的结构；
日志或 metrics 把结构体按值传给接口。
```

比如 value receiver：

```go
func (c Cache) Put(k, v string) {
    c.mu.Lock()
    defer c.mu.Unlock()
    c.m[k] = v
}
```

每次调用 `Put` 都复制一份 `Cache`，锁也被复制。这个方法看起来加了锁，其实锁的是副本。

防御办法：

```text
含锁结构体用 pointer receiver；
不要按值传递含锁结构体；
不要把含锁结构体作为 map value 频繁取出修改；
必要时嵌入 noCopy 标记并跑 go vet；
把锁和可复制数据分开；
提供 Clone 方法时明确不复制锁状态，只复制业务快照。
```

Go 的 `go vet` 有 `copylocks` 检查，专门报告 lock 被按值错误传递的场景。它不能证明所有代码都安全，但能抓住很多常见失误。

面试里可以这样回答：

```text
复制带锁结构体危险，是因为锁的内部状态和它保护的数据边界被一起按字节复制了。副本可能和原对象共享 map、slice、指针等底层数据，却有不同的锁，导致两边都以为加锁了，实际仍然并发访问同一份数据。如果复制的是已经使用过的锁，还可能复制 locked bit、waiter count、semaphore 等运行时状态，造成永久阻塞、错误唤醒或不可预测行为。所以含 sync.Mutex/RWMutex/WaitGroup/Cond 的结构体通常用指针传递，方法用 pointer receiver，并用 go vet copylocks 辅助检查。
```

一句话：锁可以保护对象，但被复制后，保护关系本身就裂开了。

## Q038. Go 中 sync.Mutex 被复制后可能出现什么问题？

**回答：**

Go 中 `sync.Mutex` 被复制后，最常见的问题有两类：锁副本保护不了同一份数据，或者锁内部状态被复制成一个坏状态。

第一类是“假加锁”。这是最常见也最隐蔽的。

```go
type Counter struct {
    mu sync.Mutex
    n  *int
}

func (c Counter) Inc() {
    c.mu.Lock()
    defer c.mu.Unlock()
    *c.n++
}
```

这里 `Inc` 是 value receiver。调用时 `Counter` 被复制，`mu` 也被复制，但 `n` 指针仍然指向同一个整数。多个 goroutine 调用 `Inc` 时，每个副本锁住自己的 `mu`，却修改同一个 `*n`。结果就是数据竞争。

类似问题也会出现在 map：

```go
type Store struct {
    mu sync.Mutex
    m  map[string]int
}

func (s Store) Put(k string, v int) {
    s.mu.Lock()
    defer s.mu.Unlock()
    s.m[k] = v
}
```

`map` 底层共享，`mu` 被复制。锁看起来存在，实际没有串行化所有写 map 的路径。

第二类是复制 locked 状态。比如：

```go
var a sync.Mutex
a.Lock()
b := a
a.Unlock()

b.Lock() // 可能永远等
```

`b` 可能复制到的是“已加锁”的状态，但没有任何 goroutine 会对 `b` 做对应的 unlock。它不是 `a` 的别名，而是一个独立副本。`a.Unlock()` 不会释放 `b`。

第三类是等待者和唤醒关系被破坏。`sync.Mutex` 内部不只是一个布尔值。Go 源码里能看到它有 state 和 sema，state 里包括 locked、woken、starving、waiter count 等信息。等待者睡在运行时信号量相关路径上。复制这些字段不会把运行时等待队列也正确复制过去。

所以可能出现：

```text
副本看起来有 waiter count，但没有真实等待者；
真实等待者等在原锁上，副本 unlock 唤醒不到；
副本处于 starving/woken 等中间状态；
Unlock 触发 sync: unlock of unlocked mutex；
Lock 永久阻塞。
```

这些行为不应该依赖。官方文档的边界很清楚：`Mutex must not be copied after first use`。只要第一次使用过，就不要复制。

常见触发点：

```go
func f(c Counter) { ... }        // 参数按值复制
func (c Counter) Method() { ... } // value receiver
return c                         // 返回含锁对象副本
items = append(items, c)          // slice 里复制
for _, item := range items { ... } // range item 是副本
```

防法也直接：

```go
func (c *Counter) Inc() {
    c.mu.Lock()
    defer c.mu.Unlock()
    c.n++
}
```

并且尽量让含锁对象不可复制：

```text
用指针传递；
方法全部用 pointer receiver；
不要导出含锁结构体的可变字段；
go vet -copylocks；
需要快照时写 Snapshot/Clone，只复制业务数据，不复制锁。
```

面试里可以这样回答：

```text
Go 的 sync.Mutex 复制后可能出现假加锁和坏状态。假加锁是指结构体被复制后，map、slice、指针等底层数据仍然共享，但 mutex 变成不同副本，两个 goroutine 拿不同锁改同一份数据。坏状态是指复制了已经 locked、有 waiter、woken 或 starvation 状态的 mutex，原锁的 unlock 不会释放副本，等待者和 sema 关系也不会正确迁移，可能导致永久阻塞或 unlock of unlocked mutex。官方文档明确说 Mutex 第一次使用后不能复制，工程上要用指针接收者、指针传参和 go vet copylocks 防住。
```

一句话：复制 `sync.Mutex` 不是多了一个别名，而是造出一把和原等待关系脱节的新锁。

## Q039. 锁的 starvation mode 通常解决什么问题？

**回答：**

锁的 starvation mode 通常解决的是等待者长期抢不到锁的问题，尤其是非公平锁在高竞争下造成的尾延迟。

很多 mutex 的普通模式不是严格 FIFO。原因很现实：刚来的 goroutine 或线程已经在 CPU 上运行，如果锁刚释放，它直接 CAS 成功，成本很低；而被唤醒的等待者还要从睡眠状态变成 runnable，再被调度上 CPU。为了吞吐，普通模式往往允许这种“新来的插队”，也叫 barging。

问题是，高竞争下等待者可能反复输：

```text
G1 等锁，睡眠；
持锁者 unlock，唤醒 G1；
新来的 G2 正在 CPU 上，先抢到锁；
G1 醒来后发现锁又没了，继续睡；
下一轮又被 G3 抢走；
```

吞吐可能不错，但某些等待者会等很久。p99/p999 就是这样被拉高的。

Go 的 `sync.Mutex` 源码注释把这个讲得很清楚。它有 normal 和 starvation 两种模式。normal 模式下，等待者排 FIFO 队列，但被唤醒的 waiter 并不直接拥有锁，还要和新来的 goroutine 竞争；新来的 goroutine 因为已经在 CPU 上，常有优势。如果某个 waiter 超过 1ms 还没拿到锁，mutex 会切到 starvation mode。

starvation mode 的做法是直接交接：

```text
unlocking goroutine 不让新来的 goroutine 抢；
锁所有权直接 handoff 给等待队列前面的 waiter；
新来的 goroutine 排队；
等待者不再自旋抢锁。
```

这样能保证老等待者往前走，避免长期饥饿。

代价也很明显。直接 handoff 通常比竞争式抢锁吞吐低，因为：

```text
要唤醒指定等待者；
CPU 上正在跑的新 goroutine 不能顺手拿锁；
cache locality 可能变差；
锁交接更接近排队系统；
短临界区下调度成本占比变高。
```

所以 starvation mode 一般不是默认一直开，而是在检测到等待时间过长后才进入。等队列情况缓解，或者被唤醒的等待者发现自己不是长期饥饿状态，就会退出 starvation mode，回到 normal mode。

这背后的 trade-off 是：

```text
normal mode:
  更偏吞吐；
  允许新来的竞争；
  短临界区性能好；
  可能让老 waiter 等很久。

starvation mode:
  更偏公平和尾延迟；
  直接交给老 waiter；
  防止长期饥饿；
  吞吐和 locality 可能下降。
```

面试里可以这样回答：

```text
starvation mode 解决的是非公平锁在高竞争下老等待者长期拿不到锁的问题。普通模式为了吞吐，常允许新来的 goroutine 和被唤醒的 waiter 竞争；新来的已经在 CPU 上，可能连续抢赢，导致 waiter 饥饿。Go 的 Mutex 在 waiter 等待超过约 1ms 后会进入 starvation mode，unlock 时把锁直接交给队首 waiter，新来的排队，避免病态 tail latency。代价是吞吐和 cache locality 可能下降，所以它通常是兜底模式，不是一直开启。
```

一句话：starvation mode 用一点吞吐换等待者不会被无限插队。

## Q040. 多核 CPU 下锁竞争为什么会引发 cache line bounce？

**回答：**

多核 CPU 下，锁竞争会引发 cache line bounce，是因为锁变量本身在内存里属于某一条 cache line，而加锁和解锁都会频繁读写这条 cache line。多个核心同时争同一把锁时，这条 cache line 的所有权会在核心之间来回迁移。

可以先把锁想成一个整数：

```text
0 = unlocked
1 = locked
```

加锁时执行 CAS：

```text
CAS(&lock, 0, 1)
```

CAS 不是普通读。它是 read-modify-write，需要当前 CPU 核心拿到这条 cache line 的独占所有权。否则它不能安全地修改 lock word。

当 CPU0 拿到锁时：

```text
lock 所在 cache line 进入 CPU0 的可写状态；
其他 CPU 上这条 cache line 的副本失效或降级。
```

CPU1、CPU2、CPU3 也在抢锁，它们不断读或 CAS 同一个 lock word。每次有人尝试写，cache coherence 协议都要让这条 cache line 在核心之间转移。这个来回转移就是 cache line bounce。

竞争激烈时，大致会变成：

```text
CPU0 unlock: 写 lock=0，需要独占 cache line；
CPU1 CAS 成功: 写 lock=1，cache line 转到 CPU1；
CPU2 CAS 失败: 也可能请求独占或导致一致性流量；
CPU1 unlock: cache line 又转；
CPU3 抢到: cache line 再转。
```

临界区越短，锁变量本身的 cache coherence 成本占比越高。业务代码可能只做了一个 `counter++`，但锁的 cache line 在多个核之间来回跑，开销比真正业务操作还大。

自旋会放大这个问题。等待者如果不断 CAS，会持续制造写意图，让 cache line 更频繁迁移。好的 spinlock 会用策略减少写流量，比如先读观察、pause/backoff、排队锁、MCS lock 等，避免所有 CPU 同时打同一个变量。

还有 false sharing。即使多个锁保护不同数据，如果这些锁变量刚好落在同一条 cache line，也会互相影响：

```go
type Shard struct {
    mu sync.Mutex
    n  int64
}

var shards [64]Shard
```

相邻 shard 的 `mu` 可能在同一条 cache line 上。CPU0 抢 `shards[0].mu`，CPU1 抢 `shards[1].mu`，逻辑上是两把不同锁，但硬件看到的是同一条 cache line 被不同核心写。于是没有共享业务状态，也会 bounce。

这也是为什么高性能代码有时会 padding：

```go
type PaddedMutex struct {
    mu sync.Mutex
    _  [56]byte // 示意，真实大小要按平台和结构对齐算
}
```

或者把高频写字段分散到 per-CPU/per-shard 结构里，最后聚合。

解决 cache line bounce 的方向：

```text
减少共享锁竞争；
缩小热点锁范围；
按 key/shard/per-CPU 分散；
避免所有线程自旋 CAS 同一个变量；
使用排队锁或有限自旋；
给高频锁或计数器做 cache line padding；
把全局计数改成分片计数；
减少锁内外对同一 cache line 的写。
```

不要把它和普通内存拷贝混为一谈。cache line bounce 的痛点不是“内存带宽不够”这么简单，而是独占写权限在核心之间反复转移，导致流水线等待和互连流量上升。CPU 看起来很忙，真正业务吞吐却上不去。

面试里可以这样回答：

```text
锁竞争会引发 cache line bounce，是因为锁变量所在的 cache line 被多个 CPU 核心反复读写。CAS、Unlock 这类操作需要核心获得 cache line 的独占写权限；当多个核心争同一把锁时，这条 cache line 会在核心之间不断失效和迁移。自旋 CAS 会进一步放大一致性流量。即使是不同锁，如果它们落在同一条 cache line，也会出现 false sharing。优化方向是减少热点共享锁、分片或 per-CPU 化、有限自旋、排队锁，以及对高频锁或计数器做 padding。
```

一句话：多核下锁竞争慢，不只是等待别人释放锁，还在等同一条 cache line 在核心之间来回搬。

## Q041. NUMA 架构下锁竞争会有什么额外成本？

**回答：**

NUMA 下锁竞争的额外成本，来自“CPU 和内存不是等距离”的事实。Linux `numa(7)` 对 NUMA 的定义很直接：内存被分成多个 memory node，CPU 访问某个 node 的耗时取决于 CPU 和这个 node 的相对位置。也就是说，同样是读写一块内存，本地 node 和远端 node 成本不一样。

锁竞争在 UMA 机器上已经会有 cache line bounce。到了 NUMA，问题会再多一层：锁变量所在 cache line、锁保护的数据、持锁线程和等待线程，可能分布在不同 NUMA node 上。

一个常见形状是：

```text
CPU0 在 node0；
CPU17 在 node1；
锁变量和受保护 map 的页面在 node0；
CPU17 上的 goroutine 高频抢这把锁。
```

CPU17 每次抢锁都要跨 node 访问 lock word。CAS 需要独占 cache line，cache coherence 流量要走跨 socket 或跨 NUMA node 的互连。即使临界区很短，锁变量本身也会在 node 间来回跑。

额外成本主要有几类。

第一，远端内存访问。锁变量或受保护数据如果在远端 node，访问延迟更高，带宽也可能更差。锁竞争越频繁，这个成本越明显。

第二，跨 node cache line 迁移。锁的 cache line 不只在 CPU core 间 bounce，还可能跨 socket bounce。跨 socket 一般比同 socket 核间迁移更贵。

第三，持锁线程和等待线程被调度到不同 node。比如持锁者在 node0，等待者在 node1，唤醒后又被调度回 node1。锁交接不只是 goroutine/thread 状态切换，还牵扯 cache locality 丢失。

第四，受保护数据也可能远端化。即使锁本身很快拿到，临界区里访问的 map、队列、索引、buffer 页面如果在另一个 node，临界区持有时间会变长。别人看到的是 lock hold time 上升。

第五，分配策略会影响后续性能。Linux NUMA memory policy 文档里提到 preferred、bind、interleave、本地分配等策略。很多程序默认是 first-touch：哪个 CPU 第一次触碰页面，页面就可能落在哪个 node。初始化线程和业务线程如果不在同一个 node，后面就容易远端访问。

排查时不要只看“这把锁是否热点”，还要看热点是不是跨 NUMA：

```text
线程是否频繁跨 node 迁移；
锁保护的数据页面在哪个 node；
热点 goroutine/线程在哪些 CPU 上跑；
是否有单个全局锁被多个 node 高频抢；
per-node QPS 和延迟是否差异明显；
perf c2c、numastat、/proc/<pid>/numa_maps 是否显示远端访问。
```

工程上常见缓解方式：

```text
按 NUMA node 分片；
每个 node 有本地队列、本地 shard、本地计数；
尽量让 worker、内存和锁在同一个 node；
减少跨 node 全局锁；
把全局统计改成 per-node/per-CPU 聚合；
初始化时按实际 worker 触碰内存；
必要时用 numa policy、CPU affinity 或容器 cpuset/mems 控制布局。
```

但也别一上来就手动绑核。NUMA 优化很容易把系统搞复杂。先用 profile 和 NUMA 观测确认远端访问真的在拖慢，再决定是否做 per-node sharding 或 affinity。

面试里可以这样回答：

```text
NUMA 下锁竞争的额外成本是远端内存和跨 node cache coherence。锁变量所在 cache line、受保护数据、持锁线程和等待线程可能分布在不同 NUMA node；每次 CAS、unlock、唤醒和临界区访问都可能跨 node。这样 lock wait 和 lock hold 都会变长，还会丢 cache locality。优化方向通常是 per-node sharding、per-CPU/per-node 计数、本地队列、减少全局热锁，并让 worker、内存和锁尽量在同一个 node。
```

一句话：NUMA 下抢一把全局锁，可能是在让多个 socket 反复争一条 cache line 和一组远端页面。

## Q042. 如果锁竞争严重，什么时候应该换成 channel、队列、sharding 或无锁结构？

**回答：**

锁竞争严重时，不要先问“换成什么更高级”，先问当前竞争来自哪里。不同根因对应不同替代方案。

如果竞争来自“同一份状态必须按顺序修改”，channel 或队列通常更合适。也就是 single owner 模型：

```text
状态只归一个 goroutine/worker 拥有；
其他 goroutine 不直接拿锁改状态；
它们把命令发到队列；
owner 顺序处理命令并返回结果。
```

这种方式适合 actor、workflow、stream append、任务调度器、需要严格顺序的状态机。它把显式 mutex 换成了消息队列，优点是 ownership 清楚，缺点是队列可能成为新瓶颈，还要处理背压、超时、关闭和请求取消。

如果竞争来自“很多独立 key 被一把大锁串住”，优先考虑 sharding 或 lock striping：

```text
按 streamID、tenantID、actorID、hash(key) 分片；
每个 shard 一把锁或一个 owner goroutine；
跨 shard 操作按固定顺序加锁或走事务/协调流程。
```

它适合 map、cache、session、metadata、per-key 状态。前提是 key 分布比较均匀，大多数操作只碰一个 key。超级热点 key 仍然需要单独治理。

如果竞争来自“热点计数器或简单状态位”，可以考虑 atomic 或分片计数。比如 QPS 计数、最近心跳时间、只读快照指针：

```go
atomic.AddInt64(&counter, 1)
atomic.StorePointer(...)
```

但 Go `sync/atomic` 文档提醒得很明确：这些低层原子原语需要非常小心，除特殊低层应用外，更推荐 channel 或 `sync` 包。也就是说，无锁不是“更简单的 mutex”，而是把复杂性转移到内存序、ABA、生命周期、对象发布、回收和不变量维护上。

如果竞争来自“读多写少，读路径被锁拖住”，可以考虑：

```text
atomic.Value / atomic.Pointer 发布只读快照；
copy-on-write；
RCU 风格读路径；
读写锁；
缓存本地副本，异步刷新。
```

但 `RWMutex` 不是万能答案。如果读临界区很短、写频繁、reader 数很多，reader 计数和 writer pending 也会成为成本。读多写少且读逻辑有一定长度时，`RWMutex` 才更可能划算。

如果竞争来自“下游并发过多”，mutex 可能用错了。应该用 semaphore、限流队列或 worker pool：

```text
最多 100 个并发 RPC；
最多 20 个文件同时打开；
最多 8 个压缩任务并行。
```

这些是额度控制，不是互斥访问。

一个实用判断表：

```text
锁保护复杂不变量，竞争低:
  保留 mutex。

锁保护同一状态机，所有操作必须顺序执行:
  channel / queue / single owner。

锁保护大量独立 key:
  sharding / lock striping。

锁保护简单计数或状态位:
  atomic 或分片计数。

读多写少，读对象可快照:
  atomic.Value / copy-on-write。

临界区里有慢 I/O:
  先移出 I/O，别急着换同步原语。

锁竞争来自单个热点 key:
  单独排队、合并、批处理，或者业务层削峰。
```

面试里可以这样回答：

```text
锁竞争严重时，先看竞争形状。独立 key 被全局锁串住，用 sharding 或 lock striping；同一状态必须顺序修改，用 channel/队列/single owner；简单计数或状态位热点，用 atomic 或分片计数；读多写少且能发布快照，用 atomic.Value 或 copy-on-write；下游资源额度问题，用 semaphore 或 worker pool。不要为了“无锁”而无锁，atomic 的正确性边界更窄，复杂不变量通常还是 mutex 更可维护。
```

一句话：换掉 mutex 的前提，是你知道它现在是在串行化哪一种资源。

## Q043. 如何为锁相关 bug 设计 stress test？

**回答：**

锁相关 bug 的 stress test 目标不是“多跑几次看看”，而是尽量制造容易出错的交错，并在每轮都检查不变量。

第一步，先把并发不变量写清楚。比如：

```text
队列长度不能为负；
任务不能重复完成；
account 总金额不变；
同一个 actor 同一时刻只能执行一个任务；
close 之后不能再 append；
每个 request 最终要么成功，要么明确失败，不能卡住。
```

没有不变量，stress test 只会变成“跑了很多 goroutine，看起来没崩”。

第二步，用 barrier 同时起跑。不要让 goroutine 一个个自然启动，否则交错不够集中：

```go
start := make(chan struct{})
var wg sync.WaitGroup

for i := 0; i < n; i++ {
    wg.Add(1)
    go func(id int) {
        defer wg.Done()
        <-start
        exercise(id)
    }(i)
}

close(start)
wg.Wait()
```

第三步，把容易出错的点做成 hook。比如你怀疑 `lock A -> lock B` 之间有竞态，就在中间插测试 hook：

```go
type Hooks struct {
    AfterLockA func()
}
```

测试里用 channel 卡住它，强制另一条 goroutine 走到指定位置。这样比 `time.Sleep` 稳。sleep 只能碰运气，hook 能构造交错。

第四步，随机化操作序列。比如多个 goroutine 对同一个对象执行：

```text
Put / Get / Delete / Snapshot / Close / Retry / Cancel
```

每轮生成随机操作，但把 seed 打出来。失败后用同一个 seed 复现。Go `go test -shuffle` 会报告 seed，适合打乱测试顺序；业务操作内部也可以自己记录 seed。

第五步，改变调度参数。Go 官方 test flags 支持 `-count` 重复运行，`-cpu` 指定多组 `GOMAXPROCS`，`-shuffle` 随机测试顺序，`-timeout` 防止卡死。常见组合：

```text
go test -run TestConcurrentX -count=1000 -cpu=1,2,4,8 -shuffle=on -timeout=60s
go test -race -run TestConcurrentX -count=100 -cpu=1,4 -timeout=120s
```

`GOMAXPROCS=1` 也有价值。它会暴露一些 cooperative 调度、持锁等待、channel 顺序问题；多核则更容易暴露真正并行读写。

第六步，把死锁检测做进测试。不要让 CI 卡到全局超时才失败。可以给每轮操作一个 deadline：

```go
done := make(chan struct{})
go func() {
    defer close(done)
    runScenario()
}()

select {
case <-done:
case <-time.After(2 * time.Second):
    dumpStacks()
    t.Fatal("scenario timed out")
}
```

超时时打印 goroutine dump，这比一句 “test timed out” 有用得多。

第七步，配合工具，但不要迷信工具：

```text
-race:
  找 data race。

block/mutex profile:
  看阻塞热点。

goroutine dump:
  看死锁和卡住位置。

go test -count/-cpu/-shuffle:
  扩大调度覆盖。
```

第八步，把失败场景缩小成确定性复现。stress test 找到问题后，要尽量提炼成小测试：

```text
G1 到达点 A；
G2 到达点 B；
释放 G1；
检查等待图或状态。
```

否则后面修复回归时会很痛苦。

面试里可以这样回答：

```text
锁相关 bug 的 stress test 要先定义不变量，再制造并发交错。做法包括 barrier 同时起跑、随机操作序列、记录 seed、用 hook 强制关键交错、改变 GOMAXPROCS、用 go test -count/-cpu/-shuffle 重复运行，并给每轮设置 timeout 和 goroutine dump。-race 可以找数据竞争，mutex/block profile 可以看等待热点，但死锁和逻辑错还要靠不变量、超时和等待图来抓。stress test 发现问题后，最好再缩成一个确定性交错测试。
```

一句话：好的并发 stress test 不是跑得久，而是能逼出关键交错并检查不变量。

## Q044. race detector 能发现死锁吗？

**回答：**

不能。Go race detector 主要发现 data race，不是死锁检测器。

Go 官方 race detector 文档定义的 data race 是：两个 goroutine 并发访问同一个变量，其中至少一个是写。race detector 运行时会记录内存访问，发现冲突访问后打印读写栈和 goroutine 创建栈。它解决的是“有没有未同步的共享内存访问”。

死锁是另一类问题。死锁可能完全没有 data race：

```go
var a, b sync.Mutex

func f() {
    a.Lock()
    defer a.Unlock()
    b.Lock()
    defer b.Unlock()
}

func g() {
    b.Lock()
    defer b.Unlock()
    a.Lock()
    defer a.Unlock()
}
```

这里所有共享状态都可以被锁保护得很好，race detector 不一定报告任何 data race。但 `f` 和 `g` 仍然可能一个拿着 `a` 等 `b`，另一个拿着 `b` 等 `a`。

同样，channel 死锁、WaitGroup 永远等不到 Done、Cond lost wakeup、goroutine 泄漏、锁饥饿、livelock，race detector 都不是专门抓这些的。

它能间接帮忙的地方是：有些锁 bug 同时伴随 data race，比如漏加锁、复制 mutex 后假加锁、锁保护范围不一致。这时 `-race` 可能抓到真实问题。但如果你的同步写得“没有数据竞争但逻辑等待错了”，race detector 就不会给答案。

定位死锁要用别的手段：

```text
测试 timeout；
goroutine dump；
block profile；
mutex profile；
等待图分析；
日志记录 lock wait/hold；
debug ranked lock 检查锁顺序；
系统层面看 futex wait / thread stack。
```

面试里可以这样回答：

```text
race detector 不能发现死锁。它检测的是运行时发生的数据竞争，也就是未同步的并发读写同一变量。死锁可能发生在完全没有 data race 的程序里，比如两把锁按相反顺序获取。-race 可以发现漏锁、假加锁等伴随数据竞争的问题，但不能证明没有死锁、没有 goroutine 泄漏、没有 lost wakeup。死锁要靠 timeout、goroutine dump、block/mutex profile 和等待图分析。
```

一句话：`-race` 查的是“有没有乱读写”，不是“大家会不会互相等死”。

## Q045. 没有 data race 是否代表并发逻辑一定正确？

**回答：**

不代表。没有 data race 只说明共享内存访问在同步层面是安全的，不说明业务逻辑一定正确。

Go memory model 里有一个重要保证：data-race-free 的程序表现得像 goroutine 以某种顺序交错执行，也就是 DRF-SC。这个保证很有价值。它让你不用面对“编译器和 CPU 重排导致完全不可理解的结果”。但“某种顺序交错执行”不等于“每一种交错都满足业务不变量”。

一个没有 data race 但逻辑错的例子：

```go
mu.Lock()
if stock > 0 {
    mu.Unlock()

    // 中间做了一堆事

    mu.Lock()
    stock--
}
mu.Unlock()
```

每次访问 `stock` 都拿了锁，可能没有 data race。但检查和扣减不在同一个临界区，中间别的 goroutine 可以把库存扣完。最后可能扣成负数。这是 atomicity bug，不是 data race。

再比如死锁：

```text
G1: lock A -> lock B
G2: lock B -> lock A
```

没有未同步读写，但会卡住。

还有 lost wakeup：

```text
检查条件；
准备 wait；
通知已经发生；
真正开始 wait；
永远睡下去。
```

这也不一定有 data race，尤其所有条件都被锁保护时。错在条件变量使用协议。

常见的“无 data race 但并发错”包括：

```text
死锁；
livelock；
starvation；
lock convoy；
lost wakeup；
goroutine leak；
channel 永远没人收/没人发；
WaitGroup Add/Done 次数不匹配；
check-then-act 不在同一临界区；
读到一致但过期的状态；
超时后结果又回来，覆盖新状态；
重试导致重复副作用；
锁顺序违反设计，但测试没跑到。
```

所以并发正确性至少要看三层：

```text
内存安全:
  有没有 data race。

同步协议:
  会不会死锁、丢通知、泄漏 goroutine、超时后乱写。

业务不变量:
  钱是否守恒，任务是否只完成一次，顺序是否满足要求。
```

面试里可以这样回答：

```text
没有 data race 不代表并发逻辑正确。Go 的 DRF-SC 保证只是说没有数据竞争的程序可以按某种顺序交错理解，但业务仍然可能在某些交错下出错。比如 check-then-act 拆成两个临界区、两把锁反向获取导致死锁、Cond lost wakeup、WaitGroup 次数不匹配、超时重试造成重复副作用，这些都可能没有 data race。并发测试除了跑 -race，还要检查不变量、超时、等待图和失败恢复语义。
```

一句话：data-race-free 是并发正确性的底线，不是完整证明。

## Q046. mutex 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

mutex 的核心目标是保护共享可变状态，让同一组状态在明确的同步边界里被互斥访问，并建立必要的内存可见性关系。它首先解决正确性问题，其次影响可维护性和安全性；性能不是它的首要目标。

Go 的 `sync.Mutex` 文档说它是 mutual exclusion lock，Go memory model 还定义了 `Unlock` 和后续 `Lock` 的 synchronizes-before 关系。这里有两个点：

```text
互斥:
  同一时刻最多一个 goroutine 进入这段受保护状态。

可见性:
  前一个临界区写入的状态，对后一个成功 Lock 的临界区可见。
```

所以 mutex 不是简单“挡住代码”。它真正保护的是状态和不变量：

```go
type Queue struct {
    mu    sync.Mutex
    items []Task
    size  int
}
```

这里 `mu` 保护的是：

```text
items；
size；
items 和 size 的一致性。
```

从问题维度看：

```text
正确性:
  mutex 的主目标。
  防止 data race，维护不变量，让复合操作原子化。

性能:
  不是主目标。
  低竞争时成本可接受，高竞争时可能成为瓶颈。

安全性:
  间接相关。
  data race、半更新状态、重复执行可能变成安全漏洞或权限绕过。

可维护性:
  取决于用法。
  清楚的锁 ownership 提高可维护性；到处散落的锁会降低可维护性。
```

一个常见错误是把 mutex 当性能优化手段。其实大多数时候，加锁是在承认“这里有共享可变状态，需要串行化”。如果性能变差，不能怪 mutex 本身；要看共享状态设计、锁粒度、临界区内容和竞争形状。

面试里可以这样回答：

```text
mutex 的核心目标是正确性：保护共享可变状态和状态不变量，并通过 Lock/Unlock 建立互斥和内存可见性。它主要解决 data race、复合操作原子性、半更新状态这些问题。性能不是 mutex 的首要目标，低竞争时它通常足够便宜，高竞争时会带来等待、尾延迟和 cache coherence 成本。清楚的 mutex ownership 能提升可维护性，但锁边界混乱会让系统更难维护。
```

一句话：mutex 首先是正确性工具，不是并发性能加速器。

## Q047. mutex 的典型适用场景和不适用场景分别是什么？

**回答：**

mutex 适合保护一小组共享可变状态，尤其是这组状态有明确不变量、临界区较短、访问路径可控的时候。

典型适用场景：

```text
保护 map、slice、链表、队列；
保护状态机字段；
保护多个字段之间的不变量；
保护非线程安全对象；
保护短的读改写操作；
实现 per-object / per-shard 的状态串行化；
在对象内部封装并发安全方法。
```

比如：

```go
type SessionStore struct {
    mu       sync.Mutex
    sessions map[string]*Session
}

func (s *SessionStore) Put(id string, sess *Session) {
    s.mu.Lock()
    defer s.mu.Unlock()
    s.sessions[id] = sess
}
```

这里状态边界很清楚：`sessions` 由 `mu` 保护。

mutex 不适合的场景也要明确。

第一，不适合持锁做慢 I/O：

```text
网络；
数据库；
磁盘 fsync；
日志刷盘；
RPC；
压缩大文件。
```

这些会把临界区时间拉长，放大 tail latency。

第二，不适合解决资源额度问题。限制最多 N 个并发访问，下游限流、连接池并发，这些更像 semaphore、channel buffer 或 worker pool。

第三，不适合高频简单计数。一个全局 mutex 保护 QPS counter，在多核下很容易成为热点。分片计数或 atomic 更合适。

第四，不适合跨进程、跨机器一致性。Go 进程内 mutex 只管同一进程内 goroutine。数据库事务、文件锁、分布式锁、租约、fencing token，解决的是另一层问题。

第五，不适合表达复杂异步流程。比如“等所有 worker 完成”“等条件满足”“事件广播”“任务排队”，通常用 WaitGroup、Cond、channel、queue 更自然。

第六，不适合在 ownership 不清楚的公共对象上到处暴露。谁都可以拿锁，谁都可以持锁调用别人，最后会变成死锁和维护灾难。

面试里可以这样回答：

```text
mutex 适合保护一小组共享可变状态和明确不变量，比如 map、slice、队列、状态机、多个字段的一致更新。临界区应该短，访问路径应该可控，最好封装在对象方法里。它不适合持锁做 I/O 或 RPC，不适合限 N 个并发这种额度问题，不适合热点计数器，不适合跨进程/跨机器一致性，也不适合复杂异步协调。遇到这些场景要考虑 semaphore、channel、queue、sharding、atomic、事务或分布式租约。
```

一句话：mutex 适合短临界区里的共享内存一致性，不适合把所有并发问题都塞进一把锁。

## Q048. mutex 和相近概念最容易混淆的边界在哪里？

**回答：**

mutex 最容易和几个概念混在一起：RWMutex、semaphore、condition variable、channel、atomic、spinlock、futex、事务锁、分布式锁。边界不清楚，设计就容易变形。

`Mutex` 和 `RWMutex` 的边界：

```text
Mutex:
  一个执行单元进入临界区。

RWMutex:
  多个 reader 或一个 writer。
```

`RWMutex` 不是“更高级的 Mutex”。只有读多写少、读临界区有一定长度、读之间能真正并行时，它才可能更好。写多或短读场景下，它可能更慢。

`Mutex` 和 `semaphore` 的边界：

```text
Mutex:
  保护共享状态，一次一个。

Semaphore:
  控制资源额度，最多 N 个。
```

如果你想限制 20 个并发 RPC，用 semaphore；如果你想保护 map，用 mutex。

`Mutex` 和 `Cond` 的边界：

```text
Mutex:
  保护条件和状态。

Cond:
  等待条件变化并唤醒等待者。
```

Cond 不能单独用，必须配锁。`Signal` 不是传数据，等待方醒来后还要重新检查条件。

`Mutex` 和 channel 的边界：

```text
Mutex:
  多个 goroutine 共享内存，靠锁串行访问。

Channel:
  通过消息传递 ownership、事件或请求。
```

channel 不自动让业务正确。channel send/close 也会有竞态，队列也会积压。它更适合 ownership 转移、任务排队、生命周期通知，而不是随手替代所有锁。

`Mutex` 和 atomic 的边界：

```text
Mutex:
  适合复合不变量。

Atomic:
  适合单变量或很小的无锁协议。
```

Go `sync/atomic` 文档提醒这些原语需要非常小心。atomic 很容易写出没有 data race 但业务不变量错误的代码。

`Mutex` 和 spinlock / futex 的边界：

```text
spinlock:
  等锁时自旋，不睡眠。

futex:
  Linux 提供的用户态锁底层 wait/wake building block。

mutex:
  面向调用者的互斥语义。
```

很多用户态 mutex 内部会用 futex，也可能先短暂自旋再阻塞。但调用者不应该把这些实现细节和语义混为一谈。

`Mutex` 和数据库事务锁的边界：

```text
进程内 mutex:
  保护当前进程内内存状态。

数据库锁/事务:
  保护数据库里的并发读写和持久化状态。
```

你在 Go 里拿了 mutex，不代表另一个进程不能改数据库。同样，数据库事务锁也不保护你进程内 map。

`Mutex` 和分布式锁的边界：

```text
mutex:
  进程内。

分布式锁/lease/fencing:
  跨进程、跨机器、涉及时钟、会话、租约过期和旧 owner 防护。
```

分布式锁如果没有 fencing token，旧 owner 超时后继续写，仍然可能破坏状态。

面试里可以这样回答：

```text
mutex 的边界是进程内共享内存互斥。RWMutex 是读写分离，不是默认更快；semaphore 管资源额度，不保护状态；Cond 管等待条件变化，但状态仍由锁保护；channel 更适合 ownership 转移和排队；atomic 适合很小的无锁协议，不适合复杂不变量；futex 是实现 building block；数据库锁和分布式锁解决跨事务、跨进程、跨机器问题。把这些边界混了，最容易写出看似同步、实际语义不对的系统。
```

一句话：mutex 管的是本进程里一组共享状态的互斥访问，不是所有“等待”和“协调”的统称。

## Q049. mutex 在高并发场景下可能出现哪些隐藏问题？

**回答：**

mutex 在高并发下的问题，很多不是功能错误，而是延迟、调度和可观测性问题。代码在低并发下完全正确，一上流量就暴露。

常见隐藏问题有这些。

第一，tail latency 放大。一个 goroutine 持锁变慢，后面所有等待者都慢。平均延迟可能还行，p99 先坏。

第二，lock convoy。等待队列长期不为空，锁释放后一个个唤醒、抢锁、再阻塞，系统吞吐下降。

第三，starvation。非公平锁为了吞吐允许新来的 goroutine 抢锁，老等待者可能长时间拿不到。Go `Mutex` 的 starvation mode 就是为了兜住这类病态 tail latency。

第四，cache line bounce。多个 CPU 核心反复 CAS 同一把锁，锁所在 cache line 在核心之间迁移。临界区越短，这个成本越显眼。

第五，NUMA 放大。跨 node 抢全局锁时，远端内存访问、跨 socket cache coherence 和线程迁移都会让等待更贵。

第六，隐藏的长临界区。锁内某个路径平时很快，偶尔做日志、扩容、GC 相关分配、序列化、fsync、RPC、回调，p99 会被这条路径拉起来。

第七，优先级反转。高优先级请求等低优先级后台任务持有的锁，中间普通任务还可能抢占后台任务。

第八，锁保护范围越来越大。维护过程中不断往临界区里加逻辑，原来短锁变成长锁，没人意识到。

第九，观测误导。CPU 不高不代表没问题，可能大量 goroutine 都在等锁。CPU 很高也不一定是业务忙，可能是自旋、CAS、cache coherence 成本。

第十，锁和 GC/内存分配互相影响。锁内分配大对象会让 hold time 不稳定；大量等待 goroutine 增加调度和内存压力。

排查时要看：

```text
lock wait 分布；
lock hold 分布；
mutex profile；
block profile；
goroutine 数；
p99/p999；
热点 key/shard；
临界区内是否有 I/O、分配、回调、channel 等待。
```

修复方向不是固定的：

```text
缩小临界区；
移出慢操作；
分片；
copy-on-write；
single owner queue；
atomic 分片计数；
限流削峰；
调整数据 ownership；
必要时用更细粒度的锁，但要同步设计锁顺序。
```

面试里可以这样回答：

```text
mutex 高并发下的隐藏问题包括 p99 放大、lock convoy、等待者饥饿、优先级反转、cache line bounce、NUMA 远端访问、隐藏长临界区、锁范围维护膨胀，以及 goroutine 堆积带来的调度压力。它们不一定表现为 data race 或功能错误，更多表现为吞吐上不去、CPU 指标反常和尾延迟抖动。定位时要看 wait/hold 分布、mutex/block profile、热点 key 和锁内慢操作。
```

一句话：高并发下 mutex 最怕的不是“不能用”，而是正确但把系统慢慢串成一队。

## Q050. mutex 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

mutex 的边界在故障场景下会变得很明显：它只保护当前进程内存里的临界区，不自动处理崩溃恢复、跨进程一致性、请求超时、重试幂等和持久化状态。

先说崩溃。Go 进程崩了，进程内 `sync.Mutex` 也没了。它不会留下一个可恢复的“锁记录”。这对纯内存状态没问题，因为进程都没了；但如果临界区里改了一半外部状态，比如写了文件、数据库、消息队列，mutex 不会帮你回滚。

比如：

```text
拿 mutex；
写本地内存状态；
写 WAL 一半；
进程崩溃；
重启。
```

重启后你要靠 WAL recovery、事务、幂等 key、校验和、重放/截断逻辑恢复，不能靠 mutex。

跨进程锁还有 owner death 问题。Linux robust futex 文档讨论的就是这种情况：如果进程在持有共享 pthread mutex 时崩溃，等待者需要知道 owner died，并决定受保护数据能不能恢复。robust futex 会在 owner 退出时标记 `FUTEX_OWNER_DIED` 并唤醒等待者；用户态仍然要修数据。普通进程内 mutex 没有这种语义。

再说超时。Go 的 `sync.Mutex.Lock` 没有 context，也没有 timeout。一个 goroutine 等锁时，不能直接用 context 取消这个 `Lock`。如果你需要“等不到就放弃”，要重新设计：

```text
TryLock + 退避；
channel/queue 请求带 context；
semaphore Acquire(ctx)；
把慢操作移到锁外；
在更高层做 deadline 和排队超时。
```

但 `TryLock` 也不是随便加。失败后必须知道如何回滚当前持有的资源，避免引入新的活锁或业务重试风暴。

重试场景也很容易出边界。请求超时后，客户端可能重试；原请求在服务端可能仍然持锁执行。于是会出现：

```text
第一次请求还没结束；
客户端超时重试；
第二次请求排队或并发进入；
第一次稍后成功；
第二次也成功；
副作用重复。
```

mutex 只能保证某段内存状态互斥，不能保证业务幂等。要用 request id、idempotency key、dedup 表、version、fencing token、事务唯一约束来处理重试。

重启后也有边界。内存 mutex 不会记得上次谁正在执行。系统要从持久化日志、数据库状态、任务状态表里判断：

```text
这个任务是未开始、执行中、已完成还是需要补偿？
这个请求是否已经提交过？
这个 actor 的 last applied seq 是多少？
这个锁保护的不变量是否能从持久化状态重建？
```

还有 panic。`defer Unlock` 能释放锁，但不保证临界区内半更新状态恢复。崩溃和 panic 都要求你区分两层：

```text
锁释放:
  防止其他 goroutine 永久等待。

状态恢复:
  保证共享状态、持久化状态、外部副作用仍然一致。
```

面试里可以这样回答：

```text
mutex 在故障场景下的边界是：它只管当前进程内的互斥和内存可见性，不管持久化恢复、跨进程 owner death、超时取消和重试幂等。进程崩溃后 Go 的 sync.Mutex 不会留下可恢复状态；锁内写了一半外部系统，要靠 WAL、事务、幂等 key、重放和补偿恢复。Lock 本身也没有 context timeout，等锁取消通常要用 queue、semaphore、TryLock 或上层 deadline 重新设计。客户端重试可能和原请求并发存在，mutex 不能替代 dedup、version、唯一约束或 fencing token。
```

一句话：mutex 只管“现在这个进程里谁能进临界区”，不管崩溃后世界怎么恢复。

## Q051. mutex 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

mutex 的性能瓶颈通常不是单选题。更准确的说法是：mutex 本身的直接成本来自原子操作、cache coherence、调度和阻塞唤醒；真正把延迟放大的，往往是锁竞争和临界区里的慢操作。CPU、内存、I/O、网络都可能参与，但它们的角色不一样。

可以按三层看。

第一层是无竞争路径。锁没人抢时，`Lock`/`Unlock` 主要是原子指令、内存屏障和少量状态修改。这个成本通常很小，但不是零。在临界区只有几条指令时，原子操作和 cache line 所有权迁移就可能占到很大比例。

第二层是有竞争路径。多个 goroutine 同时抢同一把锁时，成本会变成：

```text
等待时间；
调度切换；
唤醒延迟；
CAS 失败重试；
cache line bounce；
futex/semaphore 之类阻塞路径；
等待队列管理。
```

这时候瓶颈的名字一般叫锁竞争，而不是单纯叫 CPU 或内存。CPU 可能很高，也可能不高。CPU 高时，可能是在自旋、CAS、调度和 cache coherence 上烧掉；CPU 不高时，可能大量 goroutine 都睡在锁上。

第三层是临界区内部的慢操作。mutex 本身只是在门口排队，真正让队伍变长的是持锁时间。如果锁内做了 I/O、网络 RPC、日志落盘、数据库访问、压缩、JSON 编解码、大对象分配、回调，锁等待会被这些操作放大。

一个简单模型是：

```text
lock wait ~= 前面等待者数量 * 平均持锁时间
```

这个模型粗糙，但面试里很好用。锁本身再快，只要临界区里有偶发 100ms 的 RPC，后面的请求就会一起吃到 100ms 级别的排队。

不同指标会给出不同线索：

```text
CPU 高，mutex profile 显示等待高:
  可能是高竞争、短临界区、CAS/cache line bounce、自旋成本。

CPU 不高，goroutine 堆积，mutex/block profile 高:
  可能是锁内 I/O、RPC、sleep、channel 等待或长计算。

内存带宽/cache miss 高:
  可能是锁保护的数据结构很大、跨 NUMA node、共享 cache line 反复迁移。

网络或磁盘 p99 高，并且锁等待同步升高:
  多半是慢 I/O 被放进临界区，锁把外部抖动传播到所有等待者。
```

所以优化 mutex 不能只盯着 `Lock` 这行代码。要把等待时间和持锁时间拆开：

```text
wait time:
  goroutine 等锁多久。

hold time:
  goroutine 拿到锁后多久释放。

critical section content:
  持锁期间到底做了什么。

contention shape:
  是全局锁、热点 key、少数 shard，还是突发流量造成的排队。
```

常见修复也要对症：

```text
锁内有 I/O:
  移到锁外，或者先复制状态再异步写。

热点 key 抢同一把锁:
  sharding、per-key lock、single owner queue。

读多写少:
  可以评估 RWMutex、copy-on-write、atomic snapshot。

短临界区高竞争:
  减少共享写，分片计数，批量合并，避免所有核反复写同一 cache line。

等待者太多:
  限流、队列、背压，而不是让所有请求都冲到同一把锁上。
```

面试里可以这样回答：

```text
mutex 的瓶颈通常直接表现为锁竞争，但根因可能是 CPU、内存、I/O 或网络。无竞争时主要是原子操作和内存同步；高竞争时会出现调度、阻塞唤醒、CAS 失败和 cache line bounce；如果临界区里做 I/O 或 RPC，外部慢操作会通过锁排队放大成 p99 问题。排查时我会分开看 wait time、hold time、锁内操作和热点分布，而不是只看 CPU 利用率。
```

一句话：mutex 的性能问题常常不是锁慢，而是锁把慢路径串行化了。

## Q052. mutex 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

这三类测试的目标不同，不能混在一起。

correctness test 测的是“逻辑是否正确”。它应该围绕不变量设计，而不是围绕吞吐量设计。比如一个带锁 map、队列或状态机，正确性测试要问：

```text
并发插入后元素是否丢失；
计数器是否和实际元素数量一致；
状态转换是否只走允许的边；
错误返回时锁是否释放；
panic 或提前 return 后是否破坏不变量；
读到的数据是否可能是半更新状态；
多个字段之间的关系是否一直成立。
```

如果被测对象是一个简化 mutex，本身的不变量会是：

```text
同一时刻最多一个 goroutine 进入写临界区；
Unlock 后后续 Lock 能看到前一个临界区写入的状态；
Unlock 未上锁对象会失败或 panic，而不是静默破坏状态；
等待者不会因为 lost wakeup 永久睡眠；
重复 Lock/Unlock 的边界行为符合接口定义。
```

stress test 测的是“罕见交错是否会出问题”。它不证明正确，但可以把低概率并发 bug 暴露出来。设计 stress test 时，要故意制造调度扰动：

```text
大量 goroutine；
随机读写比例；
随机 sleep/yield；
不同 GOMAXPROCS；
go test -race；
go test -count=N；
go test -cpu=1,2,4,8；
测试超时；
随机取消 context；
随机 panic/recover；
在临界区前后插入小延迟；
把操作记录成 history，最后检查线性化或不变量。
```

stress test 的重点不是“跑一次通过”。它要能反复跑、并在失败时留下足够信息：

```text
随机种子；
goroutine id 或操作 id；
操作序列；
关键状态快照；
失败前最后几步；
超时时 goroutine dump。
```

benchmark 测的是“成本和扩展性”。它不应该只给一个 ns/op。锁相关 benchmark 至少要拆这些维度：

```text
无竞争 Lock/Unlock 成本；
不同并发度下的吞吐；
不同读写比例下的吞吐；
p50/p95/p99 等待时间；
临界区持有时间；
CPU 使用率；
分配次数；
mutex profile；
block profile；
热点 key 和均匀 key 的差异；
GOMAXPROCS 变化后的曲线。
```

一个常见错误是 benchmark 只测最理想路径，比如每个 goroutine 都访问均匀分布的 key，然后得出“锁没有问题”。线上真正坏掉的往往是热点路径：

```text
1% 的 key 承担 80% 请求；
读多但写会阻塞所有读；
锁内偶发扩容；
锁内日志偶尔阻塞；
GC 或分配让 hold time 抖动。
```

所以 benchmark 最好至少有两组：

```text
理想负载:
  均匀访问，短临界区，看基础扩展性。

病态负载:
  热点 key，长尾临界区，突发写入，看 p99 和退化方式。
```

面试里可以这样回答：

```text
correctness test 主要测不变量，比如互斥、状态一致、错误路径释放锁、没有 lost wakeup。stress test 主要测罕见 interleaving，通过高并发、随机调度、-race、-count、-cpu、超时和 goroutine dump 把死锁、活锁、饥饿暴露出来。benchmark 主要测成本曲线，包括无竞争成本、不同并发度和读写比例下的吞吐、wait/hold 分布、p99、mutex profile 和热点负载下的退化。
```

一句话：correctness 证明你没写错明显逻辑，stress 逼出偶发交错，benchmark 告诉你在压力下会怎么慢。

## Q053. 如果要求从零实现一个简化版 mutex，你会先定义哪些不变量？

**回答：**

从零实现 mutex，不能一上来就写 CAS 和 futex。先定义不变量。没有不变量，代码看起来能跑，也很难判断边界是不是正确。

最核心的不变量是互斥性：

```text
state == locked 时，最多一个执行者被允许进入临界区。
state == unlocked 时，没有执行者持有锁。
```

这条看起来简单，但它决定了 `Lock` 的成功路径必须是原子的。两个 goroutine 同时看到 unlocked，不能都进入临界区。通常要用 CAS 把 `unlocked -> locked` 做成不可分割的状态转换。

第二个是不丢唤醒：

```text
如果有等待者睡眠，并且锁从 locked 变为 unlocked，至少要有一个等待者最终有机会重新竞争或被唤醒。
```

这里最容易写错。典型 bug 是：

```text
goroutine A 准备睡眠；
goroutine B Unlock 并 Wake；
A 还没真正睡下，Wake 丢了；
A 睡下后没人再唤醒。
```

futex 的 compare-and-block 语义就是为了解决这类问题：睡眠前要再次比较用户态的 futex word，只有状态仍然符合预期才阻塞。

第三个是状态和等待队列一致：

```text
waiter count 不能为负；
有等待者时，状态位要能表达 contested；
Unlock 不能在没有等待者时做多余唤醒；
有等待者时不能把锁状态改得让新来者永久绕过老等待者。
```

简化实现可以只有两个状态：

```text
0 = unlocked
1 = locked
```

但只靠两个状态很难高效处理阻塞等待。稍微实用一点的版本通常需要：

```text
locked bit;
waiter count;
maybe starving/fairness bit;
semaphore/futex word;
```

第四个是内存顺序：

```text
Unlock 之前在临界区做的写入，后续成功 Lock 的执行者必须可见。
```

这不是语法层面的互斥，而是 memory model 语义。实现上通常要求：

```text
Lock 成功路径具备 acquire 语义；
Unlock 释放路径具备 release 语义；
阻塞唤醒路径不能绕过这些同步关系。
```

第五个是非法操作的边界：

```text
Unlock 一个未加锁的 mutex 要有定义好的结果；
重复 Unlock 不能静默成功；
复制已经使用过的 mutex 不应被允许；
销毁或重置时不能还有等待者；
如果接口不支持重入，重复 Lock 自己持有的锁可能死锁。
```

Go 的 `sync.Mutex` 不记录 owner，所以它不以“同一个 goroutine 才能 Unlock”作为公开不变量。Java `ReentrantLock` 这类 owner-aware 锁则不同。实现前要先说清楚这一点，否则会把不同语言的语义混在一起。

第六个是进展性：

```text
没有 goroutine 永久持锁时，等待者最终应该有机会拿到锁。
```

这不是说必须严格 FIFO。很多 mutex 会为了吞吐牺牲绝对公平。但至少要避免严重饥饿。Go `Mutex` 的 starvation mode 就是在等待时间过长时改变交接策略，避免老等待者一直被新来的 goroutine 抢走锁。

第七个是性能路径边界：

```text
无竞争 Lock/Unlock 只走用户态原子操作；
有竞争时才进入慢路径；
慢路径阻塞前必须再次确认状态；
唤醒数量要受控，避免 thundering herd。
```

一个简化版 mutex 的状态机可以先写成这样：

```text
Lock:
  CAS unlocked -> locked 成功:
    进入临界区
  否则:
    标记有等待者
    在状态仍为 locked 时阻塞
    被唤醒后重试

Unlock:
  如果当前不是 locked:
    报错或 panic
  如果没有等待者:
    release store unlocked
  如果有等待者:
    release store unlocked 或直接 handoff
    wake one waiter
```

面试里可以这样回答：

```text
我会先定义不变量，而不是先写 CAS。核心不变量包括：同一时刻最多一个执行者进入临界区；Unlock 到后续 Lock 建立内存可见性；等待者不会因为 lost wakeup 永久睡眠；锁状态和 waiter count 一致；非法 Unlock 有明确行为；没有永久持锁时等待者最终有机会前进；无竞争走 fast path，有竞争才进入 blocking slow path。之后再决定是否用 futex、semaphore、spin 或公平队列实现这些不变量。
```

一句话：mutex 实现的难点不是“有一个 bool”，而是让状态转换、睡眠唤醒和内存顺序同时正确。

## Q054. mutex 的常见误用是什么，误用后通常会产生什么线上症状？

**回答：**

mutex 的误用大致分两类：一类直接破坏正确性，一类不一定错，但会把系统拖慢。

第一种是忘记 unlock。常见于多 return 分支、错误路径、panic 路径：

```go
mu.Lock()
if err != nil {
    return err
}
mu.Unlock()
```

症状通常是请求卡住、goroutine 堆积、最终超时。Go 程序里如果所有 goroutine 都睡住，可能出现 `all goroutines are asleep - deadlock!`；线上服务更常见的是部分请求超时，进程还活着。

第二种是 double unlock 或未加锁就 unlock。Go 里会触发运行时错误，比如 `sync: unlock of unlocked mutex`。这种问题通常不是慢，而是直接崩。

第三种是复制带锁结构体。`sync` 文档明确说包含这些同步类型的值使用后不应复制。复制后可能出现两个结构体看起来保护同一份数据，实际锁状态分裂；也可能把等待者、状态位复制出一个无意义的副本。症状可能是偶发 data race、状态错乱、死锁或难以复现的 panic。

第四种是锁保护范围不清楚。比如写的时候加锁，读的时候忘了；或者锁保护了 map，但返回了 map 内部指针让外部无锁修改：

```go
func (c *Cache) Get(k string) *Entry {
    c.mu.Lock()
    defer c.mu.Unlock()
    return c.items[k]
}
```

如果 `Entry` 本身还会被修改，那么返回指针后锁已经失效。症状可能是 race detector 报告、偶发字段不一致、线上出现“不可能”的状态。

第五种是锁顺序不一致：

```text
路径 A: lock userMu -> lock orderMu
路径 B: lock orderMu -> lock userMu
```

症状是死锁，而且通常只在特定流量组合下出现。goroutine dump 会看到两个或多个 goroutine 互相等锁。

第六种是在锁内做 I/O、RPC、日志落盘或外部回调。这个不一定破坏正确性，但会把外部慢路径带进临界区。症状是 p99 抖动、吞吐下降、mutex profile 里等待时间集中在某几个调用栈。

第七种是读写锁升级误用。Go `RWMutex` 的 `RLock` 不能升级成 `Lock`。如果读锁内发现需要写，然后直接 `Lock`，很容易自己等自己：

```go
rw.RLock()
if needWrite {
    rw.Lock() // 错误模式
}
```

症状是偶发死锁，通常在读路径触发写路径时出现。

第八种是把 `defer Unlock` 放进高频循环。它对错误路径安全，但如果函数很大或循环内频繁执行，可能扩大持锁范围或增加不必要开销：

```go
for _, item := range items {
    mu.Lock()
    defer mu.Unlock() // 错误：要等整个函数返回才释放
}
```

症状是锁持有时间异常长，后续迭代或其他 goroutine 全部排队。

第九种是用 `TryLock` 写忙等：

```go
for !mu.TryLock() {
}
```

症状是 CPU 高、吞吐反而下降，还可能让真正持锁者更难得到 CPU。

第十种是把 mutex 当成分布式锁。单机内存锁不能保护数据库、消息队列、另一个进程或另一个节点。症状是单机测试没问题，多实例部署后重复执行、并发写、幂等破坏。

排查线上症状时，可以把现象和误用对应起来：

```text
请求超时、goroutine 堆积:
  忘记 unlock、死锁、锁内慢操作。

CPU 高但业务吞吐低:
  自旋、TryLock 忙等、高竞争短临界区。

进程直接崩:
  double unlock、unlock unlocked mutex、并发 map 写。

p99 抖动:
  锁内 I/O、回调、分配、GC、热点 key。

状态偶发不一致:
  锁保护范围不完整、返回内部可变对象、混用 atomic 和 lock。

多实例重复执行:
  把进程内 mutex 误当成跨进程互斥。
```

面试里可以这样回答：

```text
mutex 常见误用包括忘记 unlock、double unlock、复制带锁结构体、锁顺序不一致、锁内 I/O 或回调、保护范围不完整、RLock 升级成 Lock、循环里 defer unlock、TryLock 忙等，以及把进程内锁当分布式锁。线上症状不一定是 data race，更多是请求超时、goroutine 堆积、p99 抖动、CPU 反常、运行时 panic、偶发状态错乱和多实例重复副作用。
```

一句话：锁的误用往往不会马上报错，它会把错误藏在某个等待队列、慢调用或边界路径里。

## Q055. mutex 在单机和分布式环境中的语义有什么差异？

**回答：**

单机 mutex 和分布式锁看起来都叫“锁”，但语义差很多。

单机进程内 mutex 保护的是共享内存。它的基本假设是：

```text
参与者在同一个进程地址空间；
锁对象本身可靠存在于内存中；
等待和唤醒由运行时或内核调度；
Unlock 和后续 Lock 建立内存可见性；
进程崩溃后锁和内存状态一起消失。
```

Go 的 `sync.Mutex`、`sync.RWMutex` 就是这个层面的工具。它不关心网络分区、节点重启、租约过期，也不关心另一个进程能不能写数据库。

分布式环境里，问题换了。参与者不共享内存，只能通过网络、存储系统或协调服务达成某种互斥。它面对的是：

```text
网络延迟；
网络分区；
进程暂停；
GC stop-the-world；
机器重启；
时钟偏移；
锁服务不可用；
客户端重试；
旧 owner 恢复后继续写；
外部系统已经接受了副作用。
```

所以分布式锁通常不是简单的 `Lock/Unlock`，而是一套 lease/session/fencing 语义。尤其是 fencing token 很关键。没有 fencing token 时，旧 owner 可能在租约过期后继续写外部系统：

```text
A 拿到分布式锁；
A 暂停很久，锁租约过期；
B 拿到新锁并开始写；
A 恢复，也继续写；
外部系统如果不检查 token，就无法区分新旧 owner。
```

单机 mutex 的 correctness 主要是互斥和 memory ordering；分布式锁的 correctness 还要回答：

```text
锁服务是否线性一致；
租约过期后旧 owner 如何被拒绝；
客户端超时后请求是否实际成功；
Unlock 丢包怎么办；
锁 owner 崩溃后如何释放；
重试是否幂等；
外部资源是否检查 fencing token；
时钟是否参与判断，误差如何处理。
```

还有一个很容易混的点：数据库锁也不是进程内 mutex。数据库行锁、表锁、事务隔离保护的是数据库内部对象；它不自动保护应用进程里的 map。反过来，应用里的 mutex 也不阻止另一个服务实例更新同一行数据库。

可以这样划边界：

```text
sync.Mutex:
  单进程共享内存互斥。

process-shared pthread mutex / file lock:
  单机跨进程互斥，语义依赖 OS，可能有 owner death 和 advisory/mandatory 差异。

database lock:
  事务和数据库对象级别互斥。

distributed lock:
  跨节点协调，需要 lease、session、fencing、幂等和一致性假设。
```

面试里可以这样回答：

```text
单机 mutex 的语义是进程内共享内存互斥和内存可见性；分布式锁的语义是跨进程、跨机器的协调，必须处理网络分区、租约过期、owner 崩溃、时钟、重试和旧 owner 写入。单机 mutex 崩溃后锁随进程消失，分布式锁还要解决会话清理和 fencing。把两者混起来，常见后果是多实例部署后仍然出现重复执行或并发写。
```

一句话：单机 mutex 解决共享内存问题，分布式锁解决失败环境里的 ownership 问题。

## Q056. RWMutex 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

RWMutex 的核心目标是：允许多个只读访问并发执行，同时保证写访问独占。它先是正确性工具，然后才可能是性能优化。

Go 文档对 `RWMutex` 的定义很直接：读写互斥锁可以被任意数量的 reader 持有，或者被一个 writer 持有。这个定义里有两个关键点：

```text
多个 RLock 可以同时成立；
Lock 与任何 RLock/Lock 都互斥。
```

所以 RWMutex 解决的第一层问题仍然是正确性。它保护的是“读的时候不能看到写到一半的状态，写的时候不能被其他读写打断”。如果读路径真的只读，写路径维护完整不变量，RWMutex 可以在读多写少时减少不必要的串行化。

它的性能收益有前提：

```text
读远多于写；
读临界区不是极短；
读路径不会修改共享状态；
写频率低，且写持锁时间可控；
数据结构的不变量允许读写分离。
```

如果这些前提不成立，RWMutex 可能比 Mutex 更慢。原因很简单：RWMutex 要维护 reader count、writer waiting 状态、reader/writer semaphore，还要处理 writer 到来后阻止新 reader 的策略。读路径不是“零成本”，尤其在多核下大量 reader 反复更新同一个 reader counter，也会带来 cache line bounce。

安全性方面，RWMutex 不会自动让你的读变安全。读路径只要有 lazy init、统计计数、缓存填充、更新时间戳，就已经不是纯读。如果你在 `RLock` 下偷偷写共享字段，还是会破坏语义。

可维护性方面，RWMutex 有时让意图更清楚：读路径用 `RLock`，写路径用 `Lock`。但它也增加理解成本，特别是 Go `RWMutex` 不支持读锁升级成写锁，也不支持写锁降级成读锁。维护者一旦在读路径里加写逻辑，很容易出死锁或数据竞争。

面试里可以这样回答：

```text
RWMutex 的核心目标是读写分离：多个纯读临界区可以并行，写临界区保持独占。它首先解决正确性，也就是读不能看到半更新状态、写不能和其他读写并发；性能收益只在读多写少、读临界区有一定成本、写不频繁时成立。它不是默认更快，也不是安全魔法。读路径如果会修改共享状态，或者写很频繁，RWMutex 可能比 Mutex 更复杂也更慢。
```

一句话：RWMutex 是“读多写少”场景下的互斥工具，不是 Mutex 的免费升级版。

## Q057. RWMutex 的典型适用场景和不适用场景分别是什么？

**回答：**

RWMutex 适合读远多于写，而且读操作确实可以并发的场景。

典型适用场景有这些。

第一，配置快照。配置更新很少，读取很频繁。读路径只需要拿到当前配置，写路径偶尔替换配置。

```go
rw.RLock()
cfg := current
rw.RUnlock()
```

如果配置对象本身不可变，甚至可以进一步用 atomic pointer 或 copy-on-write，减少读锁成本。

第二，读多写少的缓存或索引。比如路由表、用户权限表、特征开关、服务发现结果。写入频率低，读路径需要查多个字段，使用 RWMutex 可以让多个 reader 同时查。

第三，需要维护多个字段不变量的结构。单个字段可以用 atomic，但多个字段之间有关系时，RWMutex 能让 reader 在同一个一致性视图下读完。

第四，读临界区有一定成本。比如读路径要遍历一个小集合、做多次 map lookup 或组合几个字段。读临界区如果只有一个整数读取，RWMutex 的计数成本可能比串行化省下的还多。

不适用场景也很常见。

第一，写很多。写频繁时，reader 经常被 writer 阻塞，writer 也经常等 reader drain。最后变成更复杂的 Mutex。

第二，读非常短。大量 reader 每次只读一个字段，会反复修改 RWMutex 内部 reader counter，原子操作和 cache line bounce 可能吃掉收益。

第三，读路径会写。比如 lazy loading、缓存 miss 后填充、读时更新 lastAccess、统计命中次数。这种代码用 `RLock` 很危险。要么把写拆出来，要么直接用 `Lock`，要么改成 atomic/分片统计。

第四，需要锁升级。Go `RWMutex` 不支持 `RLock -> Lock` 的升级。读锁内判断“发现没有，升级写入”是典型死锁来源。

第五，临界区内有 I/O、RPC 或回调。读锁允许多个 reader 并发，不代表可以在读锁里做慢操作。一个 writer 到来后，新 reader 会被挡住，已有慢 reader 会拖住 writer。

第六，真正需要的是 ownership 转移或串行处理。比如所有请求都要按顺序修改同一个状态机，channel/actor/single owner queue 往往比 RWMutex 更清楚。

第七，读数据可以接受旧快照。那可以考虑 immutable snapshot、copy-on-write、RCU 风格、atomic pointer。它们让 reader 不碰共享锁，但写路径要承担复制和版本管理成本。

面试里可以这样回答：

```text
RWMutex 适合读多写少、读路径纯读、读临界区有一定长度、写不频繁且状态需要一致视图的场景，比如配置快照、路由表、权限表和读多写少缓存。不适合写频繁、读路径极短、读时会 lazy init 或更新统计、需要读锁升级写锁、锁内有 I/O/RPC、或者更适合用队列和 ownership 转移表达的场景。是否使用 RWMutex 最好用读写比例和 benchmark 验证，而不是凭直觉。
```

一句话：RWMutex 适合让真正的读并发，不适合掩盖写多、慢读和 ownership 不清的问题。

## Q058. RWMutex 和相近概念最容易混淆的边界在哪里？

**回答：**

RWMutex 最容易被误解成“读操作用 RLock 就一定安全、一定更快”。这两个判断都不对。

先说 RWMutex 和 Mutex。Mutex 是完全互斥，任何进入临界区的操作都串行。RWMutex 把临界区分成 reader 和 writer 两类。只有当 reader 真的是只读，而且写操作不频繁时，RWMutex 才可能比 Mutex 有收益。否则 Mutex 更简单，也更不容易误用。

RWMutex 和 atomic 的边界也容易混。atomic 适合很小的同步协议，比如单个计数器、状态位、指针发布。RWMutex 更适合多个字段需要一致视图的情况：

```text
atomic:
  一个字段或很小的不变量。

RWMutex:
  多字段组合不变量，reader 需要读完一组状态。
```

如果你用多个 atomic 字段拼一个复杂状态，很容易读到“字段 A 是新版本、字段 B 是旧版本”的混合快照。RWMutex 可以避免这种问题。

RWMutex 和 `sync.Map` 的边界也不同。`sync.Map` 是特定访问模式下的并发 map，Go 文档提到它主要适合“写一次读很多”或“不同 goroutine 操作不相交 key”的场景。普通 map 加 RWMutex 更适合维护额外不变量，比如 map 长度、索引、过期队列、统计字段要一起更新。

RWMutex 和 copy-on-write 也不同。copy-on-write 让 reader 读不可变快照，writer 复制并发布新版本。它可以让读路径不加锁，但写成本和内存成本更高。RWMutex 则让 reader 共享同一份可变数据，只是读时阻止 writer。

RWMutex 和数据库 MVCC 也不能混。MVCC 通常让事务读到某个版本快照，写入通过事务冲突和提交规则处理。RWMutex 是进程内共享内存锁，没有事务回滚、隔离级别、持久化提交这些语义。

再看 Go 和 Java 的差异。Java 的 `ReentrantReadWriteLock` 是可重入读写锁，文档还讨论公平策略、锁降级和 instrumentation。Go 的 `sync.RWMutex` 不是可重入锁，并且文档明确说 `RLock` 不能升级成 `Lock`，`Lock` 也不能降级成 `RLock`。所以不能把 Java 里的使用习惯直接搬到 Go。

还有 RWMutex 和 semaphore。semaphore 管的是并发额度：

```text
最多 N 个任务同时执行。
```

RWMutex 管的是读写互斥：

```text
多个 reader 或一个 writer。
```

如果目标是限制并发请求数，用 semaphore 更自然；如果目标是保护共享状态的一致性，用 RWMutex 或 Mutex。

面试里可以这样回答：

```text
RWMutex 的边界主要在于：它不是默认更快的 Mutex，不是 atomic 的替代品，不是 sync.Map，不是 MVCC，也不是 semaphore。RWMutex 保护的是进程内共享状态的读写互斥；atomic 适合小状态，sync.Map 适合特定 map 模式，copy-on-write 适合不可变快照，数据库 MVCC 解决事务版本隔离，semaphore 管并发额度。Go 里还要特别注意 RWMutex 不可重入，不支持读锁升级写锁，也不支持写锁降级读锁。
```

一句话：RWMutex 的边界是“共享内存的一读多写互斥”，不是所有读多写少问题的标准答案。

## Q059. RWMutex 在高并发场景下可能出现哪些隐藏问题？

**回答：**

RWMutex 在高并发下的隐藏问题通常比 Mutex 更隐蔽，因为它表面上提高了并发度，实际可能把写路径或 cache coherence 成本藏起来。

第一，writer 等 reader drain。只要已有 reader 还没释放，writer 就要等。读路径如果偶尔很慢，writer 的 p99 会被慢 reader 拉长。

第二，writer pending 后新 reader 被挡住。Go 文档明确说明：当 writer 在等待时，新的 `RLock` 会阻塞，直到 writer 获取并释放锁。这是为了保证 writer 最终能拿到锁，但副作用是读请求也会突然排队。线上表现可能是“明明读多写少，为什么一次写入让大量读请求 p99 抖动”。

第三，reader counter 成为热点。大量 reader 并发 `RLock/RUnlock`，内部计数器所在 cache line 会在 CPU 核之间来回移动。读操作本身不写业务数据，但读锁实现要写锁内部状态。

第四，读锁持有时间被低估。很多读路径一开始只是 map lookup，后来加了 JSON 编码、权限计算、日志、metrics label 拼接，读锁就变长了。因为 reader 可以并发，开发者更容易忽略单个 reader 的 hold time。

第五，升级死锁。读锁内发现要写，然后尝试拿写锁，在 Go 里是错误模式。高并发下这种路径可能很少触发，一触发就是请求卡死。

第六，写饥饿或读饥饿取决于实现策略。Go `RWMutex` 会在 writer 等待时挡住新 reader，偏向避免 writer 永久饥饿。其他系统可能有不同公平策略。不能只凭“读写锁”三个字推断公平性。

第七，thundering herd。writer 释放后，很多 reader 同时恢复，可能造成 CPU 突刺、cache miss 增加，后续 writer 又开始等待。

第八，读路径偷偷写。比如懒加载缓存、更新 `lastSeen`、命中计数、内部 slice 排序。代码审查时看起来是“读接口”，实际会写共享状态。结果可能是 data race，也可能是逻辑错乱。

第九，profile 解释更难。mutex profile 看到的是等待锁的栈，但 RWMutex 的等待可能来自 writer 等 reader，也可能来自 reader 等 writer。只看一条栈容易误判，要结合读写比例、具体调用栈和业务事件。

第十，全局 RWMutex 掩盖数据建模问题。很多模块共用一把 RWMutex，短期很方便，长期所有读写都绕不开同一个全局协调点。读多时看起来还好，一旦写路径增加，系统退化很明显。

排查时我会问这些问题：

```text
读写比例是多少；
读锁最长持有多久；
writer 等待主要等哪些 reader；
writer pending 时 reader p99 是否同步升高；
RLock/RUnlock 是否比业务读本身还贵；
是否存在读锁内 I/O、分配、回调；
是否有读锁升级写锁；
是否可以用 snapshot、sharding、per-key lock 或 single owner queue。
```

面试里可以这样回答：

```text
RWMutex 高并发下的隐藏问题包括慢 reader 拖住 writer、writer pending 后新 reader 被阻塞、reader counter 形成 cache line 热点、读锁持有时间膨胀、读锁升级导致死锁、释放后大量 reader 同时唤醒，以及读路径偷偷写共享状态。它不一定比 Mutex 更稳，读写比例、hold time 和内部计数器竞争都要通过 profile 和 benchmark 验证。
```

一句话：RWMutex 让读并发了，但没有让协调成本消失，只是把成本换了位置。

## Q060. RWMutex 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

RWMutex 在故障场景下的边界和 Mutex 类似，但多了读锁和写锁两套状态，误用面更宽。

先说 panic。写锁内 panic 后，如果没有 `defer Unlock`，所有后续 reader 和 writer 都可能卡住。读锁内 panic 后，如果没有 `defer RUnlock`，writer 会一直等这个 reader 退出。因为读锁可以有很多个，漏掉一个 `RUnlock` 也足够让写锁永久拿不到。

```go
rw.RLock()
v := data[k]
if bad(v) {
    panic("bad value") // 没有 RUnlock，writer 可能永久等待
}
rw.RUnlock()
```

但 `defer` 只能释放锁，不能恢复半更新状态。写锁内如果已经改了字段 A，还没改字段 B 就 panic，后续 reader 可能在锁释放后读到破坏不变量的状态。真正的修复要么先构造新状态再一次性替换，要么用事务式更新和回滚逻辑。

重启时，Go 的 RWMutex 不会保留任何状态。进程重启后，锁对象重新初始化，内存里的 reader/writer 状态消失。外部系统里的副作用不会消失：

```text
写锁内更新内存索引；
写数据库成功；
更新内存 version 前崩溃；
重启后从数据库或日志恢复。
```

恢复逻辑不能依赖 RWMutex，要依赖持久化日志、事务、版本号、幂等 key、校验和或快照加载。

超时也是边界。Go `RWMutex.Lock` 和 `RLock` 没有 context 参数。一个 goroutine 等写锁或读锁时，不能直接用 context 取消这次等待。尤其 writer pending 时，新 reader 也会被挡住；如果你的请求 deadline 已经过了，它可能还在等 `RLock`。

需要超时语义时，可以考虑：

```text
把读请求提交到带 context 的队列；
用 channel/actor 串行化 ownership；
用 semaphore Acquire(ctx) 限制并发；
用 TryLock/TryRLock 加退避，但要谨慎；
用 atomic snapshot/copy-on-write 避免 reader 等锁；
在进入锁前做 deadline 检查，减少无意义排队。
```

重试场景也要小心。客户端超时后重试，原请求可能仍然持有读锁或写锁。RWMutex 只能保证本进程内状态访问互斥，不保证业务操作幂等：

```text
第一次写请求拿到写锁；
写外部系统很慢；
客户端超时并重试；
第二次请求等待或稍后执行；
两个请求都可能产生外部副作用。
```

解决要靠 request id、幂等表、version compare、唯一约束或 fencing token。RWMutex 不能替代这些机制。

读写锁还有一个特殊边界：读路径拿到的是某个时间点的一致视图，不是业务上的“最新状态承诺”。如果读请求超时后重试，第二次读可能看到更新后的版本。除非你有版本号、快照时间或事务语义，否则不要承诺两次读一定一致。

崩溃恢复时还要考虑 copy-on-write 和 snapshot 的发布顺序。如果写锁内构造新 map，然后替换指针，应该保证新结构完全构造好再发布；发布后旧 reader 是否还持有旧对象，也要在设计中说清楚。RWMutex 保护同一份可变对象，atomic snapshot 则通常依赖不可变对象。两者混用时最怕“发布了新指针，但对象仍继续被写”。

面试里可以这样回答：

```text
RWMutex 在故障场景下的边界是：它只保护进程内读写互斥，不提供持久化恢复、等待取消和业务幂等。panic 时漏 RUnlock 会让 writer 永久等待，漏 Unlock 会让所有读写等待；defer 只能释放锁，不能修复半更新状态。进程重启后 RWMutex 状态消失，外部副作用要靠 WAL、事务、版本号、幂等 key 和恢复流程处理。Go 的 Lock/RLock 没有 context timeout，如果需要取消等待，要用队列、semaphore、TryLock/TryRLock、snapshot 或更高层 deadline 重新设计。
```

一句话：RWMutex 能隔离读写临界区，但不负责故障恢复、超时取消和重试幂等。

## Q061. RWMutex 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

RWMutex 的性能瓶颈通常先表现为锁竞争，但背后的成本可能落在 CPU、内存、I/O 或网络上。它比 Mutex 多一个麻烦点：读路径看起来并发，实际仍然要修改锁内部状态，比如 reader counter；写路径看起来只是偶尔发生，实际要等已有 reader 全部退出。

先看 CPU。`RLock/RUnlock` 不是免费读。Go `RWMutex` 的实现里有 reader count、writer semaphore、reader semaphore 等字段。大量 goroutine 高频读锁时，即使业务数据没有写，锁内部计数器也会被反复原子更新。多核下这会带来 cache line bounce，CPU 时间花在原子操作、内存序和缓存一致性上。

再看内存。RWMutex 保护的数据结构如果很大，reader 进入临界区后可能遍历 map、slice、树或链表。这个成本不一定是锁本身，但会延长读锁持有时间。writer 到来后，要等这些 reader 都释放，写延迟就被读路径的内存访问放大。

I/O 和网络是更危险的慢路径。读锁里做日志、文件读取、RPC、数据库查询、HTTP 回调，会把“读锁”变成长锁。Go `RWMutex` 有一个重要语义：当 writer 已经在等待时，新 reader 会被阻塞，直到 writer 获取并释放锁。于是一个慢 reader 可以让 writer 等，writer pending 又会让后续 reader 等，最后读写都抖。

可以用这个模型理解：

```text
writer wait ~= 已有 reader 的最长剩余持锁时间 + writer 前面的等待
reader wait ~= 前面 pending writer 的等待和执行时间
```

这也是 RWMutex 比普通 Mutex 更容易误判的地方。你看到的是读多写少，实际 p99 可能由“偶发慢读 + 偶发写”共同决定。

诊断时我会拆成几类指标：

```text
读锁获取次数、写锁获取次数；
读锁 hold time 分布；
写锁 hold time 分布；
writer wait time；
writer pending 时 reader wait 是否上升；
RLock/RUnlock 的 CPU 成本；
锁内是否有分配、I/O、RPC、回调；
热点对象是否被全局 RWMutex 保护。
```

优化也要看根因：

```text
reader counter 热:
  分片、per-shard RWMutex、copy-on-write、atomic snapshot。

慢 reader 拖 writer:
  缩短读临界区，先复制必要数据，再在锁外做计算/I/O。

写频繁:
  换 Mutex、sharding、single owner queue，或者重构数据 ownership。

读路径需要旧快照即可:
  用不可变快照、atomic pointer、RCU 风格设计。

锁内网络/I/O:
  移出锁，必要时用版本号校验锁外结果是否仍然有效。
```

面试里可以这样回答：

```text
RWMutex 的直接瓶颈通常是锁竞争和原子计数，CPU 成本来自 RLock/RUnlock 的原子操作、调度和 cache line bounce；内存成本来自 reader 持锁遍历大结构；I/O 和网络会通过读锁或写锁 hold time 放大 p99。最典型的问题是慢 reader 拖住 writer，writer pending 后又挡住新 reader。排查时要分开看 reader hold、writer hold、writer wait、reader wait 和锁内慢操作。
```

一句话：RWMutex 的读并发不等于零成本，慢读和偶发写能一起把尾延迟拉起来。

## Q062. RWMutex 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

RWMutex 的测试要围绕“多读单写”的语义拆开。不能只测并发读不崩，也不能只用 `-race` 跑一遍就结束。

correctness test 先测不变量：

```text
任意时刻最多一个 writer；
writer 执行时没有 reader；
多个 reader 可以同时进入；
reader 看到的是完整状态，不是半更新；
写路径维护的多个字段关系始终成立；
RUnlock 和 Unlock 的调用次数匹配；
错误路径、panic 路径不会漏释放；
不允许的升级路径会被测试覆盖或在设计上禁止。
```

一个实用办法是给临界区加测试用计数器：

```text
activeReaders;
activeWriters;

进入 RLock 后 activeReaders++；
退出前 activeReaders--；
进入 Lock 后检查 activeReaders == 0 && activeWriters == 0；
writer 执行期间 activeWriters == 1。
```

这些计数器本身要用测试锁或 atomic 保护，不能让测试代码引入新的 data race。

stress test 测罕见交错。重点不是跑得快，而是把 writer pending、reader drain、升级误用、漏释放这些低概率问题逼出来：

```text
随机 RLock/Lock 比例；
随机 sleep/yield；
读路径和写路径随机 panic/recover；
写入前后插入延迟；
让 writer 到来时故意挂住一批 reader；
在 GOMAXPROCS=1 和多核下都跑；
go test -race；
go test -count=N；
go test -cpu=1,2,4,8；
超时时输出 goroutine dump。
```

对 RWMutex，stress test 特别要覆盖几个病态场景：

```text
读流量持续不断，writer 是否能前进；
writer pending 后，新 reader 是否被挡住；
慢 reader 是否导致读写 p99 同时变差；
读锁内尝试写锁是否死锁；
读路径返回内部可变对象后是否被锁外修改。
```

benchmark 测成本曲线。RWMutex 不能只测一个读写比例。至少要和 Mutex、分片锁、atomic snapshot、copy-on-write 做对比：

```text
100% read；
99% read / 1% write；
90% read / 10% write；
50% read / 50% write；
热点 key；
均匀 key；
短读临界区；
长读临界区；
短写；
偶发长写。
```

指标也不能只有吞吐：

```text
ops/s；
读 p50/p99；
写 p50/p99；
writer wait；
reader wait；
CPU 使用率；
allocs/op；
mutex profile；
block profile；
不同 GOMAXPROCS 下的扩展曲线。
```

面试里可以这样回答：

```text
RWMutex 的 correctness test 要验证多 reader、单 writer、读写互斥、状态一致、错误路径释放锁。stress test 要制造随机读写比例、调度扰动、慢 reader、writer pending、panic、超时和不同 GOMAXPROCS，重点看死锁、饥饿和漏 RUnlock。benchmark 要测不同读写比例、热点分布和临界区长度下的吞吐、p99、reader/writer wait，并和 Mutex、sharding、atomic snapshot 或 copy-on-write 对比。
```

一句话：RWMutex 测试要同时证明“读能并发、写能独占、写不会饿死、读不会被慢写拖垮”。

## Q063. 如果要求从零实现一个简化版 RWMutex，你会先定义哪些不变量？

**回答：**

简化版 RWMutex 也不能先写代码。要先定义状态和不变量，否则最容易写出 writer 永远等不到、reader 丢唤醒、或者读写同时进入的实现。

最核心的不变量是：

```text
activeWriter 只能是 0 或 1；
activeWriter == 1 时，activeReaders 必须是 0；
activeReaders > 0 时，activeWriter 必须是 0；
reader 可以多个同时存在；
writer 必须独占。
```

第二个是不丢唤醒。RWMutex 通常有两类等待者：等 writer 释放的 reader，等 reader drain 的 writer。任何一个方向丢唤醒都会卡死：

```text
最后一个 reader 退出时，如果有 writer 等待，必须唤醒 writer；
writer 退出时，如果有等待 reader 或等待 writer，必须按策略唤醒；
阻塞前要再次检查状态，避免 wait 和 wake 交错导致 lost wakeup。
```

第三个是公平策略。简化实现也要说清楚偏向谁：

```text
reader-preference:
  新 reader 可以持续进入，吞吐高，但 writer 可能饿死。

writer-preference:
  writer 等待后阻止新 reader，避免 writer 饿死，但读 p99 可能抖。

FIFO:
  更公平，但实现和唤醒成本更高。
```

Go `RWMutex` 的公开语义偏向避免 writer 永久等不到：当 writer 已经在等时，新 reader 会阻塞，直到 writer 获取并释放锁。自己实现时必须把这个策略写进不变量：

```text
waitingWriters > 0 时，新 reader 不能进入，除非设计明确允许 reader 优先。
```

第四个是计数不变量：

```text
activeReaders >= 0；
waitingReaders >= 0；
waitingWriters >= 0；
RUnlock 必须对应一次成功 RLock；
Unlock 必须对应一次成功 Lock；
reader drain 到 0 时只能唤醒一个 writer 或按策略唤醒下一批；
writer 释放后不能同时放行 writer 和 reader 造成读写并发。
```

第五个是内存顺序：

```text
写锁 Unlock 前的写入，后续 Lock/RLock 必须可见；
RUnlock 对后续 writer 的进入要建立正确的同步关系；
Lock/RLock 成功路径要有 acquire 语义；
Unlock/RUnlock 释放路径要有 release 语义。
```

第六个是升级和降级边界。为了简化，最好明确不支持：

```text
RLock 不能升级成 Lock；
Lock 不能降级成 RLock；
不可重入；
不记录 owner，或者记录 owner 后就必须定义跨 goroutine unlock 是否允许。
```

第七个是进展性：

```text
没有执行者永久持锁时，等待者最终应能前进；
reader 流量不断到来时，writer 不应永久饥饿；
writer 流量不断到来时，reader 是否可能饥饿要由策略说明。
```

一个简化状态机可以写成：

```text
RLock:
  如果没有 activeWriter 且没有 waitingWriters:
    activeReaders++
  否则:
    waitingReaders++，睡眠或自旋等待

RUnlock:
  activeReaders--
  如果 activeReaders == 0 且 waitingWriters > 0:
    wake one writer

Lock:
  如果 activeWriter == 0 且 activeReaders == 0:
    activeWriter = 1
  否则:
    waitingWriters++，阻止新 reader，等待 reader drain

Unlock:
  activeWriter = 0
  如果 waitingWriters > 0:
    wake one writer
  否则:
    wake all waiting readers
```

面试里可以这样回答：

```text
我会先定义读写互斥不变量：writer 最多一个，writer 存在时 reader 为零，reader 可以多个；再定义等待不变量：最后一个 reader 要唤醒 writer，writer 释放要按策略唤醒 reader 或 writer，阻塞前要复查状态避免 lost wakeup。还要明确公平策略、计数不能为负、RUnlock/Unlock 必须匹配、内存 acquire/release 关系、是否支持升级/降级/重入，以及没有永久持锁时等待者最终能前进。
```

一句话：RWMutex 的实现难点不是“加一个 reader count”，而是 reader、writer、等待队列和公平策略必须一起成立。

## Q064. RWMutex 的常见误用是什么，误用后通常会产生什么线上症状？

**回答：**

RWMutex 的误用通常比 Mutex 更隐蔽，因为很多代码表面上写了 `RLock`，看起来就是安全的。

第一种误用是在 `RLock` 下写共享状态。比如读缓存时顺手更新 `lastAccess`、命中次数、懒加载字段：

```go
rw.RLock()
entry := m[k]
entry.LastAccess = time.Now() // 错：读锁里写共享状态
rw.RUnlock()
```

症状是 race detector 报告、偶发状态错乱，或者 writer 以为自己独占，实际 reader 正在改内部字段。

第二种是读锁升级写锁。Go `RWMutex` 不支持 `RLock -> Lock`。读锁内发现缓存 miss，然后直接拿写锁，很容易自己等自己：

```go
rw.RLock()
if _, ok := m[k]; !ok {
    rw.Lock() // 错
}
```

症状是特定流量下死锁，goroutine dump 里能看到 goroutine 持有读锁后等写锁。

第三种是忘记 `RUnlock`。一个 reader 漏释放，就足以让后续 writer 永久等待。因为读锁可以很多个，漏一个不容易肉眼看出来。症状是写请求一直卡住，新 reader 也可能在 writer pending 后排队，最后读写都超时。

第四种是把慢操作放进读锁。比如读锁内做 JSON 编码、模板渲染、RPC、数据库查询、日志写入。多个 reader 并发不代表这没问题；writer 到来后，所有慢 reader 都会变成 writer 的等待时间。

第五种是返回内部可变对象。读锁保护了 map 查找，但返回指针、slice、map 后，调用者在锁外继续改：

```go
rw.RLock()
items := cache.items[user]
rw.RUnlock()
return items // 如果 items 可变，锁保护已经结束
```

症状是锁看起来都加了，但数据仍然出现 race 或“不可能”的字段组合。

第六种是复制带锁结构体。`sync` 文档明确说使用后不应复制。复制 RWMutex 会让锁状态和被保护数据的关系断掉。症状可能是偶发死锁、数据竞争或状态分裂。

第七种是读写比例变化后还坚持用 RWMutex。初版读多写少，后来写路径增加，RWMutex 反而成为瓶颈。症状是写 p99 高，writer pending 后读 p99 也升高。

第八种是把 RWMutex 当成分布式读写锁。它只保护当前进程内存。多实例服务里，每个进程都有自己的 RWMutex，互相不知道。症状是多实例并发写数据库、重复执行任务、缓存一致性错。

第九种是滥用 `TryRLock/TryLock`。失败后忙等，或者失败后走一条不完整的 fallback 路径。症状是 CPU 高、活锁、请求偶发读旧状态或跳过必要校验。

第十种是读锁嵌套形成隐式依赖。虽然多个 `RLock` 可以同时成立，但当中间有 writer pending 时，递归读锁可能卡住。Go 文档明确提醒不应把 `RLock` 用作递归读锁。

面试里可以这样回答：

```text
RWMutex 常见误用包括 RLock 下写共享状态、读锁升级写锁、漏 RUnlock、读锁内做 I/O/RPC/回调、返回内部可变对象、复制带锁结构体、读写比例变化后仍使用 RWMutex、把它当分布式锁、TryLock 忙等，以及把 RLock 当递归读锁。线上症状通常是 writer 长时间等待、读写 p99 同时抖动、goroutine 堆积、CPU 异常、race detector 报告、偶发状态错乱或多实例重复副作用。
```

一句话：RWMutex 最常见的坑是“名字叫读路径，实际做了写或慢操作”。

## Q065. RWMutex 在单机和分布式环境中的语义有什么差异？

**回答：**

单机 RWMutex 的语义是进程内共享内存的读写互斥：多个 reader 可以同时读，一个 writer 独占写。它依赖同一个地址空间、同一个锁对象和语言运行时/内核的同步语义。

分布式环境里，RWMutex 这个模型不够用。服务有多个进程、多个节点、网络延迟、节点暂停、重启、租约过期和重试。每个进程里的 RWMutex 只能保护自己那份内存，不能阻止别的实例读写数据库、对象存储、消息队列或共享文件。

单机场景可以这样理解：

```text
RLock:
  保护当前进程内的一致性读视图。

Lock:
  保护当前进程内的独占写。

Unlock/RUnlock:
  建立进程内内存可见性关系。
```

分布式读写锁要回答的问题完全不同：

```text
谁是当前 writer；
reader 是否要读最新版本；
reader lease 和 writer lease 如何互斥；
网络分区时允许谁继续；
旧 writer 恢复后如何被拒绝；
锁服务是否线性一致；
外部系统是否检查 fencing token；
读写请求超时后重试是否幂等；
节点崩溃后锁如何释放。
```

如果是数据库场景，数据库 MVCC 和事务锁也不是 RWMutex。MVCC 通常让读事务看到某个版本快照，写事务按隔离级别和冲突规则提交。RWMutex 没有事务回滚、提交日志、版本快照和持久化隔离级别。反过来，数据库锁也不保护应用进程里的 map。

还有一种容易混的情况是硬件 spinlock。Linux 的 hardware spinlock framework 面向异构处理器或不在同一个 OS 下运行的处理器之间的共享结构同步。它不是普通分布式锁，更不是跨数据中心锁；它通常仍然要求极短临界区、不能睡眠，并且常常和硬件互连相关。

面试里可以这样回答：

```text
单机 RWMutex 只提供当前进程内共享内存的多读单写互斥和内存可见性。分布式环境没有共享地址空间，必须用协调服务、租约、session、fencing token、版本号、事务或幂等机制表达读写 ownership。每个服务实例各自拿 RWMutex，并不能阻止其他实例写数据库。数据库 MVCC、分布式读写锁和硬件 spinlock 都有自己的语义，不能直接等同于 Go 的 sync.RWMutex。
```

一句话：RWMutex 管的是本进程内的读写临界区，分布式系统要管的是跨故障边界的 ownership 和版本。

## Q066. spinlock 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

spinlock 的核心目标是：在不能睡眠或不值得睡眠的场景下，用忙等换取极低的进入/退出开销，并保证同一时刻只有一个执行流进入临界区。

它首先是正确性工具。没有互斥，多个 CPU 同时改共享结构，状态就会坏。Linux locking lessons 里说 spinlock 能保证被保护区域里只有一个 thread-of-control。这是它的第一层价值。

它也强烈带有性能目标。和 blocking mutex 不同，spinlock 抢不到锁时不会主动睡眠，而是在 CPU 上循环等待。这样避免了上下文切换和睡眠/唤醒开销，适合临界区极短、等待时间预计小于调度成本的场景。

但这也是代价。等待者一直占着 CPU。临界区一旦变长，spinlock 会把 CPU 时间烧掉。单核上如果持锁者被抢占，而等待者一直自旋，情况更糟；内核里的 spinlock 通常还要结合禁止抢占或禁止本地中断，避免持锁者被同 CPU 的中断打断后发生自锁。

spinlock 不主要解决安全性。它不会处理权限、输入校验、崩溃恢复、幂等、事务。它也不提升可维护性。相反，spinlock 要求临界区极短、不能睡眠、不能调用可能阻塞的函数，维护成本很高。

可以把目标排序成：

```text
正确性:
  保护共享状态互斥访问。

性能:
  在极短临界区和不可睡眠上下文中避免调度开销。

安全性:
  基本不是它解决的问题。

可维护性:
  通常更难维护，需要明确上下文和锁保护范围。
```

面试里可以这样回答：

```text
spinlock 的核心目标是在不可睡眠或临界区极短的场景下，用忙等实现互斥。它首先保证正确性，也就是共享状态同一时刻只被一个执行流修改；其次可能提升性能，因为避免了阻塞和唤醒开销。代价是等待者持续占用 CPU，所以它只适合很短的临界区、内核中断/原子上下文、硬件寄存器等场景。它不负责崩溃恢复、幂等或安全控制，维护上反而更严格。
```

一句话：spinlock 是“短到不值得睡”的互斥，不是通用高性能锁。

## Q067. spinlock 的典型适用场景和不适用场景分别是什么？

**回答：**

spinlock 适合临界区非常短，而且等待者不能睡眠或睡眠成本太高的场景。典型例子多在内核、驱动、调度器、硬件寄存器访问和中断相关路径。

适用场景包括：

```text
内核中断上下文；
禁止睡眠的原子上下文；
保护很小的 per-CPU 或全局内核结构；
短时间访问硬件寄存器；
持锁只做几个字段更新；
锁竞争预计很低；
持锁者不会被长时间抢占；
上下文切换成本明显大于等待时间。
```

Linux 文档里的 `spin_lock_irqsave` 也体现了内核语境：拿锁时保存并关闭本地中断，释放时恢复中断状态。这样做是为了避免同一 CPU 上中断处理程序再次尝试拿同一把锁，导致持锁者无法继续执行。

不适用场景更重要。

第一，临界区长。不管是循环、排序、遍历大结构，还是复杂计算，都会让等待者白白烧 CPU。

第二，锁内可能睡眠。比如内存分配可能阻塞、I/O、网络、文件系统、数据库、等待 channel、等待条件变量。这些都不应该放在 spinlock 下。

第三，用户态普通业务代码。用户态一般有调度器、线程阻塞、运行时抢占和复杂 I/O。自己写 spinlock 很容易让 CPU 飙高，还不如用 mutex、channel、队列或原子结构。

第四，高竞争热点锁。spinlock 在低竞争短临界区可能好，高竞争下所有 CPU 轮流读写同一 cache line，cache coherence 成本很高。

第五，跨机器或持久化资源。spinlock 只对共享内存或硬件支持的共享锁有效，不保护数据库、分布式任务、消息队列。

第六，实时系统里不受控的自旋。自旋会影响调度延迟；在 PREEMPT_RT 这类内核配置下，很多 `spinlock_t` 语义还会变化，不能凭普通内核经验硬套。

面试里可以这样回答：

```text
spinlock 适合不能睡眠、临界区极短、竞争低、持锁者不会被长时间抢占的场景，比如内核中断上下文、调度器/驱动的小型共享结构、硬件寄存器访问。它不适合长临界区、锁内 I/O/RPC/内存阻塞、用户态普通业务、高竞争热点、跨进程/跨机器协调。只要等待时间可能超过一次阻塞唤醒成本，通常就该考虑 mutex 或重新设计。
```

一句话：spinlock 适合短、低竞争、不可睡眠；一旦长、慢、热，就会变成 CPU 烤炉。

## Q068. spinlock 和相近概念最容易混淆的边界在哪里？

**回答：**

spinlock 最容易和 mutex、RWMutex、semaphore、atomic、local interrupt disable、futex、hardware spinlock 混在一起。

spinlock 和 mutex 的边界是是否忙等。mutex 抢不到通常会阻塞，让出 CPU；spinlock 抢不到会继续占着 CPU 等。临界区短、不能睡眠时，spinlock 合适；临界区可能长或可能阻塞时，mutex 合适。

spinlock 和 RWMutex 的边界是读写并发。普通 spinlock 是独占锁；rwlock 或 reader-writer spinlock 允许多个 reader。但 Linux 文档提醒，reader-writer spinlock 需要更多原子内存操作；如果 reader 临界区不够长，简单 spinlock 反而更好。

spinlock 和 semaphore 的边界是 ownership 与额度。spinlock 保护短临界区，通常 owner 必须释放；semaphore 更像计数资源额度，可能允许多个执行者进入，不一定表达“谁保护哪份共享状态”。

spinlock 和 atomic 的边界是保护范围。atomic 适合单个变量或很小的状态转换；spinlock 适合多字段不变量和需要把一段代码作为临界区。用很多 atomic 拼复杂结构，容易读到混合状态；用 spinlock 则把这段更新整体串起来。

spinlock 和 local interrupt/preemption disable 也不同。关闭本地中断或抢占只影响当前 CPU，本身不保护其他 CPU 同时访问共享数据。Linux locktypes 文档明确区分 CPU local locks 和 spinning locks；跨 CPU 共享状态仍需要真正的锁。

spinlock 和 futex 的边界也很关键。futex 是用户态锁实现的内核辅助机制：无竞争时用户态原子操作，有竞争时进入内核等待/唤醒。spinlock 则是不睡眠的忙等策略。很多用户态 mutex 会先短暂自旋，再用 futex 阻塞；这叫混合策略，不代表 futex 本身就是 spinlock。

hardware spinlock 又是另一回事。Linux 硬件 spinlock 框架面向异构处理器或不在同一 OS 下运行的处理器之间同步共享结构。它依赖硬件锁模块，不是普通内存里的自旋变量，也不是跨数据中心分布式锁。

面试里可以这样回答：

```text
spinlock 的边界是忙等互斥。它和 mutex 的区别是等待时不睡眠；和 RWMutex/rwlock 的区别是普通 spinlock 不允许多 reader；和 semaphore 的区别是它不是资源额度；和 atomic 的区别是它保护一段临界区而不是单个变量；和关闭中断/抢占的区别是后者只管本 CPU；和 futex 的区别是 futex 是阻塞等待的内核辅助；hardware spinlock 则是异构处理器间的硬件同步机制。
```

一句话：spinlock 是一种等待策略明确的互斥锁，不是所有“原子”或“低层同步”的统称。

## Q069. spinlock 在高并发场景下可能出现哪些隐藏问题？

**回答：**

spinlock 在高并发下的问题很直接：抢不到锁的执行者不会睡，它们会继续消耗 CPU。低竞争时这是优势，高竞争时就是灾难。

第一，CPU 空转。大量线程或 CPU 核心同时自旋，业务吞吐没上去，CPU 利用率先满了。线上表现是 CPU 高、系统调用不多、业务延迟升高。

第二，cache line bounce。spinlock 的锁变量通常在一个 cache line 上。多个 CPU 不断读、CAS、写同一 cache line，缓存行在核心之间来回转移。临界区越短，这个成本越容易超过业务本身。

第三，持锁者被抢占。用户态自旋锁尤其容易遇到这个问题：线程 A 拿着锁被 OS 调度走，线程 B 在另一个 CPU 上自旋，B 会一直烧 CPU 等 A 被重新调度。内核 spinlock 往往通过禁止抢占或禁止中断来降低这类风险。

第四，优先级反转。低优先级线程持锁，高优先级线程自旋等待，中间优先级线程占用 CPU，低优先级持锁者迟迟不能运行。普通 spinlock 没有 priority inheritance。

第五，中断/抢占上下文死锁。如果在可能被中断处理程序访问的锁上使用不合适的 spinlock 变体，同一 CPU 上可能出现“持锁者被中断，中断又来拿同一把锁”的自锁。

第六，NUMA 成本放大。跨 socket 自旋会让锁变量和被保护数据在远端访问和一致性流量中来回移动，等待成本比同 socket 高很多。

第七，公平性问题。简单 test-and-set spinlock 可能让某些 CPU 长时间抢不到锁。ticket lock、MCS lock 这类队列锁可以改善公平和总线流量，但实现成本更高。

第八，虚假的低延迟。benchmark 只看平均值时，spinlock 可能很漂亮；一旦看 p99，持锁者被抢占或某个临界区稍微变长，等待者全部在 CPU 上干等。

第九，调试困难。阻塞锁还能在 profile 里看到等待栈；纯自旋可能只表现为 CPU 热点、CAS 热点、缓存一致性成本，业务层看不到“我在等锁”。

面试里可以这样回答：

```text
spinlock 高并发下的隐藏问题包括 CPU 空转、cache line bounce、持锁者被抢占导致等待者白烧 CPU、优先级反转、中断上下文自锁、NUMA 远端一致性成本、公平性差和 p99 抖动。它在低竞争短临界区很快，但竞争一高，等待者不会睡眠，系统可能表现为 CPU 满、吞吐不上升、延迟变差。
```

一句话：spinlock 最怕“大家都很积极地等”，结果 CPU 忙得很，业务没前进。

## Q070. spinlock 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

spinlock 在故障场景下的边界更硬：它通常没有恢复语义，也没有默认超时，持锁者不释放时，等待者就一直转。

先说崩溃。如果进程内自旋锁保护的是进程内内存，进程崩了，锁和内存一起没了。问题在于锁内外部副作用：如果持锁期间写了一半设备寄存器、共享内存、文件或数据库，自旋锁不会帮你恢复。

如果是跨进程共享内存里的自旋锁，风险更大。进程 A 拿锁后崩溃，锁变量可能永久保持 locked。普通 spinlock 没有 robust futex 那种 owner death 标记，也不会自动唤醒等待者。等待进程可能一直烧 CPU。

再说重启。重启后本地内存锁状态清空，但共享外部资源不一定恢复到一致状态。驱动、固件、共享内存协议或数据库状态要靠自己的 reset/recovery 流程，而不是靠 spinlock。

超时方面，普通 spinlock 的接口往往没有超时；硬件 spinlock 框架里倒是有 timeout 版本，它会忙等到超时后返回错误，并且文档强调成功后调用者不能睡，应该尽快释放。这个例子说明了一件事：spinlock 的超时不是取消等待那么简单，超时后还要知道共享状态是否安全、是否需要补偿。

重试场景也危险。拿不到 spinlock 后如果简单重试：

```text
while (!try_lock()) {
    retry++;
}
```

可能造成活锁、CPU 飙高、总线流量上升。更稳的做法通常要加 pause/backoff，或者在超过阈值后改用 blocking mutex、队列、限流，甚至重构共享状态。

还有中断场景。持锁期间如果没有正确屏蔽会访问同一锁的本地中断，系统可能不是“慢”，而是直接卡住。这个问题和崩溃类似：持锁者不再前进，等待者就没有出路。

面试里可以这样回答：

```text
spinlock 在故障场景下没有自动恢复语义。进程内锁随进程崩溃消失，但锁内外部副作用要靠事务、WAL、reset 或补偿恢复；共享内存自旋锁如果 owner 崩溃，锁位可能永久 locked，等待者会一直自旋。普通 spinlock 没有默认 timeout，trylock 忙等重试可能造成活锁和 CPU 飙高。硬件 spinlock 有 timeout 变体，但成功后仍要求不能睡、尽快释放，超时后也要由调用者处理状态恢复。
```

一句话：spinlock 的故障边界很朴素，持锁者不动，等待者就烧 CPU 等。

## Q071. spinlock 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

spinlock 的直接性能瓶颈通常来自 CPU、cache coherence 和锁竞争。I/O 和网络本身不是 spinlock 的正常工作内容；一旦锁内出现 I/O 或网络，基本就是设计问题。

CPU 成本最明显。等待者抢不到锁时不会睡眠，而是循环执行 load、CAS、pause/backoff 之类操作。竞争越激烈，CPU 越忙，但忙不代表有业务进展。

内存成本主要是 cache line。锁变量所在 cache line 会被多个 CPU 反复争抢。如果实现是简单 test-and-set，等待者不断写同一锁变量，会制造大量缓存一致性流量。test-test-and-set、ticket lock、MCS lock 等设计，本质上都是在减少无谓写和改善公平/扩展性。

锁竞争是放大器。低竞争下，spinlock 的成本可能只是几条原子指令；高竞争下，所有等待者一起争同一个 cache line，CPU 和内存子系统都会被拖进去。

I/O 和网络的角色是“禁区”。如果临界区里做磁盘、RPC、数据库或等待其他线程，持锁时间不可控，等待者会一直占 CPU。blocking mutex 至少能让等待者睡眠；spinlock 会让等待者消耗算力和功耗。

NUMA 下成本更重。跨 node 自旋时，锁变量和被保护数据可能在不同 node 间移动。一个全局 spinlock 在多 socket 机器上很容易成为系统级热点。

诊断指标可以这样看：

```text
CPU 使用率高但吞吐不升；
perf 里 CAS、pause、spin 函数占比高；
cache miss / coherence traffic 高；
锁变量所在 cache line 热；
不同 CPU 数增加后性能下降；
临界区 hold time 偶发变长；
同一把锁的等待次数和等待循环次数暴增。
```

面试里可以这样回答：

```text
spinlock 的瓶颈主要来自 CPU 空转、原子操作、cache line bounce 和锁竞争。I/O 和网络不是正常瓶颈，因为它们不应该出现在 spinlock 临界区；如果出现，就会把等待者全部变成忙等。多核和 NUMA 下，全局 spinlock 会产生大量缓存一致性流量，CPU 很忙但吞吐不一定提高。
```

一句话：spinlock 慢起来时，瓶颈通常不是“等不到资源”，而是“所有 CPU 都在用力等同一个 cache line”。

## Q072. spinlock 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

spinlock 的 correctness test 先测基本互斥和内存可见性：

```text
同一时刻最多一个执行者进入临界区；
临界区内多字段更新不会被并发观察到半状态；
unlock 后下一个 lock 能看到前一个临界区的写入；
trylock 成功和失败路径语义正确；
unlock 未加锁对象有明确行为；
不同上下文下不会自己等自己。
```

如果是内核或嵌入式 spinlock，还要测上下文规则：

```text
中断上下文能否使用；
需要 irqsave 的路径是否真的保存/恢复中断状态；
持锁期间是否调用可能睡眠的函数；
是否违反 lock ordering；
是否在 PREEMPT_RT 等配置下语义变化。
```

stress test 要制造竞争和调度扰动。Linux kernel 有 locktorture 这类工具，思路就是创建多个线程反复拿锁、持锁不同时间、调节 reader/writer 数量和竞争强度。自己写用户态或库级测试时，也可以借这个思路：

```text
线程数从 1 到多倍 CPU 数；
随机持锁时间；
随机 trylock 失败路径；
随机 backoff；
固定热点锁；
多个锁顺序组合；
CPU 亲和性变化；
GOMAXPROCS 或线程数变化；
长时间运行；
超时后输出栈和计数器。
```

stress test 还要故意测坏场景：

```text
持锁者被 sleep 或抢占；
临界区偶发变长；
高优先级线程等低优先级持锁者；
同 CPU 中断重入；
多个 CPU 同时释放/抢锁；
trylock 失败后 fallback 是否正确。
```

benchmark 要看无竞争和竞争两条曲线：

```text
无竞争 lock/unlock ns/op；
低竞争吞吐；
高竞争吞吐；
不同 CPU 数扩展性；
平均等待循环次数；
p99 等待时间；
cache miss / coherence 指标；
CPU 利用率；
功耗或忙等比例；
和 mutex、futex mutex、队列锁、MCS/ticket lock 对比。
```

如果 benchmark 只跑一个线程，它只能说明 fast path 成本；如果只跑所有线程抢同一把锁，它只能说明病态热点。两者都要测。

面试里可以这样回答：

```text
spinlock 的 correctness test 测互斥、内存可见性、trylock 语义、非法 unlock 和上下文规则；stress test 用大量线程、随机持锁时间、热点锁、CPU 亲和性、抢占/中断扰动和长时间运行逼出死锁、活锁、公平性问题；benchmark 则分无竞争和高竞争两类，测 ns/op、吞吐、p99、等待循环次数、CPU、cache coherence，并和 mutex 或队列锁对比。
```

一句话：spinlock benchmark 如果不看竞争曲线和 CPU 空转，只看平均吞吐，很容易得出错误结论。

## Q073. 如果要求从零实现一个简化版 spinlock，你会先定义哪些不变量？

**回答：**

从零实现 spinlock，最小版本看起来只是一个原子 bool，但先要把不变量说清楚。

第一，互斥不变量：

```text
state == 0 表示 unlocked；
state == 1 表示 locked；
只有 CAS(0 -> 1) 成功的执行者可以进入临界区；
任意时刻最多一个执行者持有锁。
```

第二，释放不变量：

```text
Unlock 只能由合法持锁者调用；
Unlock 必须把 state 变回 0；
重复 Unlock 或未持锁 Unlock 是 bug；
释放后等待者最终能观察到 state == 0。
```

第三，内存顺序：

```text
Lock 成功要有 acquire 语义；
Unlock 要有 release 语义；
临界区内写入不能被重排到 Unlock 之后；
临界区内读取不能被重排到 Lock 之前。
```

Linux 内存屏障文档里把 lock 操作归入 acquire，把 unlock 操作归入 release。这个语义对 spinlock 同样重要。没有内存序，互斥变量本身可能对了，被保护数据却不可见。

第四，等待策略：

```text
失败后是持续 CAS，还是先读后 CAS；
是否使用 pause/yield；
是否指数退避；
是否有最大自旋次数；
超过阈值后是否降级为阻塞；
是否保证公平。
```

最简单的 test-and-set spinlock 可以工作，但高竞争下会很差。稍微好一点的 test-test-and-set 会先读锁状态，看到可能空闲时再 CAS，减少写失效。ticket lock 可以提供 FIFO 公平，MCS lock 可以让每个等待者在自己的节点上自旋，减少共享 cache line 压力。

第五，上下文不变量：

```text
持锁期间不能睡眠；
持锁期间不能做 I/O；
如果锁会被中断处理程序使用，普通版本必须换成 irqsave 语义；
如果持锁者可能被抢占，要说明是否允许以及后果；
不支持跨进程崩溃恢复。
```

第六，进展性：

```text
如果持锁者最终 Unlock，等待者应该能继续竞争；
实现不能因为编译器优化把循环变成永远读旧值；
等待循环要使用原子读或 volatile/READ_ONCE 等合适机制；
如果声明公平，就要保证排队顺序不被新来者长期插队。
```

一个简化实现的伪代码可以是：

```text
Lock:
  while CAS(state, 0, 1, acquire) fails:
    while load(state, relaxed) == 1:
      cpu_relax()

Unlock:
  store(state, 0, release)
```

这只是教学版本。工程实现还要考虑公平性、NUMA、抢占、中断和调试检测。

面试里可以这样回答：

```text
我会先定义 state 只有 unlocked/locked，两者转换必须原子；只有 CAS 成功者能进入临界区；Unlock 用 release，Lock 成功用 acquire；持锁期间不能睡眠或做慢操作；失败等待策略要明确是 TAS、TTAS、ticket 还是 MCS；如果涉及中断，要定义 irqsave 语义；如果声明公平，要保证等待者不会长期被插队。最后还要定义非法 unlock、重复 unlock 和持锁者被抢占时的行为。
```

一句话：简化 spinlock 的难点不是 CAS，而是内存序、等待策略和上下文限制。

## Q074. spinlock 的常见误用是什么，误用后通常会产生什么线上症状？

**回答：**

spinlock 的误用通常很凶，因为它抢不到锁时不会安静睡眠。

第一种是在 spinlock 临界区里做慢操作：

```text
磁盘 I/O；
网络 RPC；
数据库访问；
日志阻塞写；
等待 channel/condition；
可能睡眠的内存分配；
复杂循环或大对象遍历。
```

症状是 CPU 飙高、吞吐下降、p99 抖动。等待者都在自旋，系统看起来很忙，但业务没推进。

第二种是在可能被中断重入的路径用错锁变体。比如进程上下文拿了普通 spinlock，本地中断进来后也拿同一把锁，同一 CPU 自己等自己。症状可能是内核卡死、软锁死、watchdog 报告。

第三种是用户态手写 spinlock 保护业务结构。用户态线程可能被抢占、阻塞或迁移。持锁线程被调度走后，其他线程自旋烧 CPU。症状是 CPU 高、延迟高、换成 pthread mutex 或 Go mutex 后反而更稳。

第四种是持锁期间调用外部回调。回调里可能拿同一把锁、拿其他锁、做 I/O、panic。症状是死锁、锁顺序反转、不可控长临界区。

第五种是用普通变量实现自旋，没有正确原子操作和内存序。编译器和 CPU 都可能重排或缓存读取。症状是本地测试偶尔过，换架构、换优化级别或压力上来后出现错乱。

第六种是没有 backoff 或 pause。所有等待者疯狂 CAS 同一个变量，造成总线和 cache line 压力。症状是 CPU 满、扩容 CPU 后吞吐不升反降。

第七种是跨 NUMA 全局自旋锁。症状是单 socket 还行，多 socket 性能突然塌。

第八种是以为 spinlock 自带公平。简单实现可能让某些线程长期抢不到。症状是平均延迟看起来还行，个别请求或核心等待时间极长。

第九种是 unlock 未持有的锁或重复 unlock。很多低层 spinlock 不一定有运行时保护，后果可能是多个执行者同时进入临界区，状态直接坏掉。

面试里可以这样回答：

```text
spinlock 常见误用包括锁内 I/O/RPC/睡眠/长循环、在中断路径用错 irqsave 变体、用户态业务手写自旋锁、锁内外部回调、非原子变量实现、没有 pause/backoff、高竞争全局锁、假设公平、重复 unlock 或未持锁 unlock。线上症状通常是 CPU 满、吞吐不升、p99 抖动、watchdog/soft lockup、死锁、状态损坏，或者加 CPU 后性能更差。
```

一句话：spinlock 一旦误用，系统不会“慢慢等”，它会很努力地把 CPU 烧掉。

## Q075. spinlock 在单机和分布式环境中的语义有什么差异？

**回答：**

普通 spinlock 的语义基本限定在共享内存机器内。多个 CPU 核心通过原子指令和缓存一致性协议操作同一个锁变量，从而保护同一份共享内存或硬件状态。

单机场景下，它假设：

```text
参与者能访问同一个锁变量；
原子操作对所有参与者可见；
cache coherence 或硬件互连能传播状态；
持锁者最终会继续执行并释放锁；
等待者可以忙等。
```

分布式环境没有这些前提。跨机器没有共享 cache line，也没有对同一个内存字的原子 CAS。网络请求也不能靠忙等某个本地变量知道远端锁释放。要做分布式互斥，通常要用协调服务、数据库事务、lease、fencing token、共识协议或幂等状态机。

有一个容易混淆的特例：hardware spinlock。Linux 硬件 spinlock 框架用于异构处理器之间的同步，比如一个 Linux CPU 和运行 RTOS 的远端处理器共享某段结构。它确实可能跨不同处理器/OS，但依赖硬件提供的锁模块和共享内存/互连，仍然不是普通分布式锁。

所以边界可以这样分：

```text
普通 spinlock:
  单机共享内存、多核互斥。

hardware spinlock:
  特定硬件支持下的异构处理器共享结构互斥。

distributed lock:
  跨机器、网络、租约、故障检测、fencing、幂等。
```

崩溃语义也不同。单机普通 spinlock 如果在内核里卡住，可能整个系统受影响；分布式锁如果 owner 崩溃，要靠 session 失效、租约过期和 fencing 防止旧 owner 继续写。spinlock 没有这些能力。

面试里可以这样回答：

```text
spinlock 的普通语义是单机共享内存互斥，依赖原子指令和 cache coherence。分布式环境没有共享锁变量和统一原子 CAS，不能用 spinlock 保护跨机器资源。硬件 spinlock 是特例，它依赖硬件模块在异构处理器之间同步共享结构，但也不是通用分布式锁。跨机器互斥要靠 lease、session、fencing token、事务或共识系统。
```

一句话：spinlock 等的是同一个内存字，分布式锁等的是故障环境里的 ownership。

## Q076. futex 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

futex 的核心目标是把用户态同步和内核阻塞连接起来：无竞争时完全在用户态用原子操作完成，有竞争时才进入内核睡眠/唤醒。它主要解决性能和实现机制问题，同时为正确阻塞提供关键原语。

Linux `futex(2)` 文档把它描述成一种 atomic compare-and-block 操作。意思是：线程准备睡眠时，内核会检查 futex word 是否仍然等于预期值；只有还等于预期值，才把线程挂到等待队列里。这一步很关键，因为它避免了 lost wakeup。

用户态锁的大致结构是：

```text
fast path:
  用 CAS 把 unlocked 改成 locked。
  成功则不进内核。

slow path:
  CAS 失败，说明有人持锁。
  把 futex word 和预期值交给 futex WAIT。
  解锁方发现有等待者时调用 futex WAKE。
```

所以 futex 不是 mutex 本身，也不是某种公平策略。它更像一块底层积木：pthread mutex、条件变量、semaphore、barrier、读写锁都可以用 futex 构建。

它解决的性能问题是系统调用开销。普通“每次 lock 都进内核”的锁会很贵；futex 让无竞争路径只用用户态原子指令。只有真正需要睡眠时才让内核介入。

它解决的正确性问题是阻塞等待的原子性。用户态自己很难正确实现“检查条件并睡眠”这个组合，因为 wake 可能刚好发生在检查和睡眠之间。futex 的 compare-and-block 把这两件事在内核边界上合成一个原子动作。

安全性和可维护性不是 futex 的主要目标。直接使用 futex 很难，`futex(7)` 也提醒 bare futex 不是给最终用户的易用抽象。大多数应用应该用 pthread、Go runtime、Java runtime 或语言标准库提供的锁。

面试里可以这样回答：

```text
futex 的核心目标是 fast userspace locking：无竞争时用户态原子操作完成，有竞争时才进入内核等待和唤醒。它主要解决性能问题，避免每次加锁都系统调用；同时通过 atomic compare-and-block 解决 lost wakeup 这类正确性难点。futex 不是 mutex 本身，而是实现 mutex、condvar、semaphore、barrier、RWLock 的底层 building block。
```

一句话：futex 的价值是“平时别进内核，真要睡时睡得正确”。

## Q077. futex 的典型适用场景和不适用场景分别是什么？

**回答：**

futex 适合实现用户态同步原语，而不是让普通业务代码直接调用。

典型适用场景是：

```text
pthread mutex；
pthread condition variable；
semaphore；
barrier；
读写锁；
语言运行时里的阻塞锁；
用户态先自旋、再阻塞的混合锁；
进程共享内存中的同步对象；
需要 PI-futex 的实时优先级继承场景；
需要 robust futex 处理 owner death 的进程共享锁。
```

这些场景有共同特点：无竞争时希望很快，有竞争时又不能让线程一直自旋。futex 正好在用户态原子状态和内核等待队列之间搭桥。

不适用场景也要说清楚。

第一，普通应用层业务锁。直接写 futex 协议很容易错，尤其是状态位、等待者标记、超时、信号、owner death、PI 这些边界。业务代码通常应该用标准库。

第二，纯短临界区低层内核路径。futex 是用户态和内核交互的系统调用接口；内核内部有自己的 spinlock、mutex、rwsem 等。

第三，跨机器分布式锁。futex word 是共享内存中的一个 32-bit 值，内核等待队列也在本机内核里。它不能跨网络等待远端机器。

第四，需要复杂公平、事务、恢复语义的场景。futex 提供 WAIT/WAKE 等基础能力，不自动提供业务幂等、事务回滚、fencing token。

第五，所有竞争都极短且自旋更便宜的场景。进入 futex slow path 意味着系统调用和调度，太短的等待可能不值得睡。这也是很多锁会先短暂自旋，再 futex wait 的原因。

面试里可以这样回答：

```text
futex 适合实现用户态同步原语，比如 pthread mutex、condvar、semaphore、barrier、RWLock、语言运行时锁，以及进程共享内存锁。它适合无竞争多、有竞争需要睡眠的场景。不适合普通业务直接调用，不适合跨机器分布式锁，不替代事务和幂等，也不适合所有等待都极短的路径。应用层通常用标准库，除非你在写 runtime、libc 或高性能同步库。
```

一句话：futex 是给锁实现者用的，不是给业务逻辑当日常同步 API 用的。

## Q078. futex 和相近概念最容易混淆的边界在哪里？

**回答：**

futex 最容易被误解成“内核 mutex”。这不准确。futex 本身只是围绕一个用户态内存字提供等待和唤醒能力，内核通常不维护无竞争锁的状态。锁状态主要在用户态。

futex 和 mutex 的边界：

```text
mutex:
  面向使用者的互斥抽象，有 Lock/Unlock 语义。

futex:
  面向实现者的 wait/wake 系统调用机制。
```

很多 mutex 用 futex 实现，但 futex 不等于 mutex。

futex 和 spinlock 的边界也常被混。spinlock 抢不到就忙等；futex 抢不到时可以让线程睡眠。很多高性能锁会先 spin 一小段时间，再 futex wait，这只是混合策略。

futex 和 condition variable 的边界：condvar 表达“等待某个条件变化”，通常要和 mutex 保护的条件谓词一起使用；futex 只知道某个 futex word 的值和等待队列，不知道业务条件是什么。

futex 和 semaphore 的边界：`futex(7)` 用 semaphore 语义解释 bare futex，但工程里的 semaphore 是更完整的资源计数抽象。futex 可以构建 semaphore，但不替你定义额度、所有权和释放规则。

futex 和 eventfd/pipe 的边界：eventfd/pipe 是文件描述符语义，可用于 poll/epoll 和跨进程事件通知；futex 是基于共享内存地址的等待唤醒，等待者必须对同一 futex key 建立一致理解。

futex 和 robust mutex 的边界：robust futex 只是让内核在线程退出时标记 owner died 并唤醒等待者，用户态仍要修复受保护数据。它不是自动事务恢复。

futex 和 PI mutex 的边界：PI-futex 通过 `FUTEX_LOCK_PI` / `FUTEX_UNLOCK_PI` 关联内核 rt-mutex，缓解 priority inversion。普通 futex wait/wake 没有 priority inheritance。

futex 和分布式锁的边界：futex 依赖本机内核和共享内存 futex word，不能跨网络，也没有租约、会话、fencing。

面试里可以这样回答：

```text
futex 是底层 wait/wake 机制，不是 mutex、spinlock、condvar、semaphore 或分布式锁。mutex 可以用 futex 实现；spinlock 忙等，futex 可睡眠；condvar 等业务条件，futex 只等内存字；robust futex 处理 owner death 标记但不修数据；PI-futex 才有 priority inheritance；分布式锁需要 lease 和 fencing，futex 只在本机共享内存范围内工作。
```

一句话：futex 不定义你的同步语义，它只提供“在这个内存字上睡和醒”的低层能力。

## Q079. futex 在高并发场景下可能出现哪些隐藏问题？

**回答：**

futex 的高并发问题通常出在 slow path：等待队列、唤醒策略、调度抖动和用户态状态协议。

第一，thundering herd。如果解锁时一次唤醒太多等待者，很多线程同时醒来抢同一个锁，只有一个成功，其余又回去睡。结果是系统调用、调度、cache line bounce 都增加。正确做法通常是只唤醒必要数量，比如 mutex 解锁唤醒一个等待者。

第二，wake 丢失或多余 wake。futex 本身提供 compare-and-block，但用户态状态位仍要设计正确。如果等待者标记、锁状态和 wake 条件不一致，就可能出现没人唤醒、唤醒过多或线程睡在错误状态上。

第三，用户态 fast path 竞争过热。无竞争时 futex 很快；高竞争时，大量线程先在用户态 CAS 失败，再进入内核。锁变量仍然会产生 cache line bounce，内核 slow path 也会增加调度成本。

第四，convoy。一个被唤醒的线程拿到锁后很快又阻塞或被抢占，后面等待者排队跟着慢。futex 能睡眠，但不能自动消除锁队列里的慢 holder。

第五，优先级反转。普通 futex wait/wake 不提供 PI。高优先级线程等低优先级 owner，低优先级 owner 又拿不到 CPU，就会反转。需要时要用 PI-futex，但 PI-futex 的状态协议更复杂。

第六，timeout 和 signal 边界。futex wait 可能因为超时、信号或虚假唤醒风格的条件变化返回。用户态必须重新检查条件，而不是把“被唤醒”当成“已经拿到锁”。

第七，robust futex 的恢复误解。owner died 后，等待者能知道锁 owner 死了，但受保护数据可能处于半更新状态。用户态必须做一致性修复，否则只是把坏状态继续传播。

第八，hash bucket 或等待队列热点。很多 futex 地址如果映射到内核内部热点路径，或者大量线程集中等待同一 futex word，会出现内核锁竞争。应用层看到的是 futex syscall 时间升高。

第九，跨进程共享内存生命周期问题。futex word 所在内存被 unmap、复用、初始化顺序错误，等待者可能在错误对象上等。进程共享锁尤其要小心 ABI、对齐、初始化和销毁顺序。

第十，和自旋策略搭配不当。自旋太短，线程频繁进内核，系统调用多；自旋太长，CPU 空转。合适阈值和硬件、临界区长度、竞争形态有关，不能拍脑袋。

面试里可以这样回答：

```text
futex 高并发下的问题包括唤醒过多导致惊群、用户态状态协议错误导致 lost wakeup、fast path CAS/cache line 竞争、lock convoy、普通 futex 的优先级反转、timeout/signal 返回后未重查条件、robust futex owner died 后数据未修复、同一 futex word 或内核等待队列热点、共享内存生命周期错误，以及自旋再阻塞的阈值不合适。
```

一句话：futex 省掉了无竞争系统调用，但高竞争下状态协议、唤醒策略和调度成本还是要自己处理好。

## 参考和校验点

1. Go 官方文档，[package sync](https://pkg.go.dev/sync)：`Mutex`、`RWMutex` 的公开语义、零值、不可复制、`Lock/Unlock`、`RLock/RUnlock`、`TryLock` 和内存同步关系。
2. Go 官方文档，[The Go Memory Model](https://go.dev/ref/mem)：说明共享数据必须通过 channel、`sync` 或 `sync/atomic` 序列化访问，并定义 mutex unlock/lock 的 happens-before 关系。
3. Go 源码，[internal/sync/mutex.go](https://go.dev/src/internal/sync/mutex.go)：展示 `Mutex` 的 fast path、normal/starvation 两种模式、1ms 饥饿阈值、自旋、信号量和直接 handoff 的实现细节。
4. Go 源码，[sync/rwmutex.go](https://go.dev/src/sync/rwmutex.go)：展示 `RWMutex` 的 reader counter、writer semaphore、reader semaphore，以及 writer pending 时阻塞新 reader 的实现。
5. Linux man-pages，[futex(2)](https://man7.org/linux/man-pages/man2/futex.2.html)：解释 futex 的 compare-and-block 语义、32-bit futex word、无竞争用户态原子操作和有竞争时进入内核等待/唤醒。
6. Linux man-pages，[futex(7)](https://man7.org/linux/man-pages/man7/futex.7.html)：说明 futex 是构建 mutex、条件变量、读写锁、barrier、semaphore 的底层 building block，非竞争路径完全在用户态完成。
7. Oracle Java 文档，[ReentrantLock](https://docs.oracle.com/en/java/javase/25/docs/api/java.base/java/util/concurrent/locks/ReentrantLock.html)：用于对比可重入锁、hold count、公平锁和非公平锁的语义与吞吐/饥饿 trade-off。
8. Linux Kernel 文档，[RT-mutex implementation design](https://docs.kernel.org/locking/rt-mutex-design.html)：说明 priority inversion、priority inheritance、PI chain、waiters tree 和优先级调整。
9. Linux Kernel 文档，[Lightweight PI-futexes](https://docs.kernel.org/locking/pi-futex.html)：说明 PI-futex 的用户态 fast path、`FUTEX_LOCK_PI` / `FUTEX_UNLOCK_PI` 慢路径和 rt-mutex 关联。
10. Linux Kernel 文档，[Runtime locking correctness validator](https://docs.kernel.org/locking/lockdep-design.html)：说明 lock class、递归获取限制和多锁反向顺序检测。
11. Linux man-pages，[perf-lock(1)](https://man7.org/linux/man-pages/man1/perf-lock.1.html)：说明 `perf lock record/report/script/info/contention` 可用于分析锁事件和等待统计。
12. Go 官方文档，[runtime/pprof](https://pkg.go.dev/runtime/pprof)、[runtime](https://pkg.go.dev/runtime) 和 [net/http/pprof](https://pkg.go.dev/net/http/pprof)：说明 goroutine/block/mutex profile、`SetMutexProfileFraction`、`runtime.Stack` 和 `/debug/pprof/` 的线上诊断入口。
13. Linux man-pages，[pthread_cond_wait(3)](https://man7.org/linux/man-pages/man3/pthread_cond_wait.3.html)：说明 condition variable 必须关联 mutex，`pthread_cond_wait` 会原子释放 mutex 并等待，返回前重新获取 mutex。
14. Go 官方扩展库文档，[golang.org/x/sync/semaphore](https://pkg.go.dev/golang.org/x/sync/semaphore)：说明 weighted semaphore 用于限制资源并发访问，`Acquire` / `Release` 以权重管理额度。
15. Oracle Java 文档，[CountDownLatch](https://docs.oracle.com/en/java/javase/25/docs/api/java.base/java/util/concurrent/CountDownLatch.html)：说明 countdown latch 等待一组操作完成、计数归零后释放等待者，并且是 one-shot。
16. Oracle Java 文档，[CyclicBarrier](https://docs.oracle.com/en/java/javase/25/docs/api/java.base/java/util/concurrent/CyclicBarrier.html)：说明 barrier 让固定数量线程在共同屏障点互相等待，并可在释放后复用。
17. POSIX man-pages，[pthread_barrier_wait(3p)](https://man7.org/linux/man-pages/man3/pthread_barrier_wait.3p.html)：说明参与线程在 barrier 处阻塞，直到指定数量线程都到达。
18. PostgreSQL 官方文档，[MVCC Introduction](https://www.postgresql.org/docs/current/mvcc-intro.html)：说明 PostgreSQL 通过 MVCC 维护并发一致性，读写通常不互相阻塞，并提供显式锁和 advisory lock 处理特定冲突点。
19. PostgreSQL 官方文档，[Explicit Locking](https://www.postgresql.org/docs/current/explicit-locking.html)：说明表级、行级、页级和 advisory locks，以及行级锁也可能形成死锁。
20. Go 官方文档，[cmd/vet copylocks](https://pkg.go.dev/cmd/vet)：说明 `copylocks` 检查用于发现 lock 被错误按值复制或传递的场景。
21. Oracle Java 文档，[ConcurrentHashMap](https://docs.oracle.com/en/java/javase/25/docs/api/java.base/java/util/concurrent/ConcurrentHashMap.html)：说明并发哈希表支持 retrieval 与 update 重叠、按 key 建立 happens-before，并通过容量和并发度提示影响内部 sizing。
22. Linux Kernel 文档，[Locking lessons](https://www.kernel.org/doc/html/latest/locking/spinlocks.html)：说明 spinlock 在多 CPU 间保护共享数据、读写锁需要额外原子内存操作，以及多锁会增加复杂度。
23. Linux man-pages，[numa(7)](https://man7.org/linux/man-pages/man7/numa.7.html)：说明 NUMA 系统内存被分成多个 node，CPU 访问内存 node 的耗时取决于两者相对位置。
24. Linux Kernel 文档，[NUMA Memory Policy](https://docs.kernel.org/admin-guide/mm/numa_memory_policy.html)：说明 bind、preferred、interleave、本地分配等 NUMA memory policy 及其对页面分配位置的影响。
25. Go 官方文档，[Data Race Detector](https://go.dev/doc/articles/race_detector)：说明 `-race` 检测运行时发生的数据竞争、报告冲突访问栈，并且只能覆盖实际执行到的路径。
26. Go 官方文档，[cmd/go testing flags](https://pkg.go.dev/cmd/go#hdr-Testing_flags)：说明 `go test` 的 `-count`、`-cpu`、`-shuffle`、`-timeout`、`-blockprofile`、`-mutexprofile` 等测试和剖析参数。
27. Go 官方文档，[sync/atomic](https://pkg.go.dev/sync/atomic)：说明 atomic 是低层同步原语，需要谨慎使用；除特殊低层应用外，通常更推荐 channel 或 `sync` 包，并说明 atomic 操作的 memory model 语义。
28. Linux Kernel 文档，[Robust futexes](https://docs.kernel.org/locking/robust-futexes.html)：说明进程持有共享 futex-based mutex 时异常退出的 owner death 问题，以及 robust futex 如何标记 `FUTEX_OWNER_DIED` 并唤醒等待者。
29. Oracle Java 文档，[ReentrantReadWriteLock](https://docs.oracle.com/en/java/javase/25/docs/api/java.base/java/util/concurrent/locks/ReentrantReadWriteLock.html)：用于对比可重入读写锁、公平策略、锁降级和监控接口，提醒不同语言的 RWLock 语义不能混用。
30. POSIX man-pages，[pthread_rwlock_rdlock(3p)](https://man7.org/linux/man-pages/man3/pthread_rwlock_rdlock.3p.html) 和 [pthread_rwlock_trywrlock(3p)](https://man7.org/linux/man-pages/man3/pthread_rwlock_trywrlock.3p.html)：说明 POSIX 读写锁的读锁、写锁、trylock、死锁检测和 priority inversion 边界。
31. Linux Kernel 文档，[Lock types and their rules](https://docs.kernel.org/locking/locktypes.html)：说明内核不同锁类型的规则差异，用于区分 spinlock、rwlock、mutex、semaphore 等同步原语的语义边界。
32. Linux Kernel 文档，[Linux kernel memory barriers](https://docs.kernel.org/core-api/wrappers/memory-barriers.html)：说明 lock acquire、unlock release 的内存屏障含义，以及为什么锁实现不能只关心状态位。
33. Linux Kernel 文档，[Kernel Lock Torture Test Operation](https://docs.kernel.org/locking/locktorture.html)：说明 locktorture 通过多线程、不同持锁时间和读写压力测试核心锁原语。
34. Linux Kernel 文档，[Lock Statistics](https://docs.kernel.org/locking/lockstat.html)：说明 lock contention、wait time、hold time、contention points、cross-CPU bounce 等锁观测指标。
35. Linux Kernel 文档，[Hardware Spinlock Framework](https://docs.kernel.org/locking/hwspinlock.html)：说明硬件 spinlock 面向异构处理器共享结构同步，成功持锁后不能睡眠，并提供 timeout/trylock 等接口边界。
