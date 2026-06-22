# 22. 消息队列、Kafka、NATS、SQS、visibility timeout 与 redelivery

这一章讨论消息系统里最容易被问深的问题：队列到底解决什么、队列和日志有什么不同、消费者是 push 还是 pull、visibility timeout 为什么会引入重复投递、ack 什么时候发、顺序到底按什么范围保证。面试里不要把“消息队列”说成一个万能异步组件。它提供的是一组故障语义和流量调节能力，代价是重复、乱序、延迟、重试和幂等都要被明确设计。

下面的回答主要参考 Apache Kafka 4.3 官方文档、NATS 官方文档和 Amazon SQS Developer Guide。Kafka 更像持久化日志，消费者自己推进 offset；SQS 更像托管队列，消息被 receive 后进入 visibility timeout；NATS Core 偏实时 pub/sub，JetStream 才提供持久流、consumer 状态、ack 和 redelivery。概念名字相似，但边界很不一样。

## Q001. 消息队列解决什么问题？

消息队列解决的第一类问题是时间解耦。调用方不必等下游把所有工作做完，只要把任务、事件或命令写进队列，就可以先返回。下游消费者按自己的速度处理。典型例子是用户上传图片后，接口只保存原图和任务消息，缩略图、转码、OCR、通知这些工作放到后台。

```text
同步调用：
用户请求 -> API -> 图片处理 -> 存储 -> 通知 -> 返回

消息队列：
用户请求 -> API -> 写任务消息 -> 返回
消费者异步处理图片、通知和索引
```

这不是单纯为了“快”。更准确地说，它把用户请求路径和后台处理路径拆开，让前台延迟不被慢任务拖住。

第二类问题是生产者和消费者解耦。生产者只需要知道消息格式和目的地，不需要知道有几个消费者、消费者部署在哪里、消费者什么时候升级。消费者可以独立扩缩容、重启、灰度。Kafka 官方文档把 producer 和 consumer 的解耦视为 Kafka 可扩展性的一个重要设计点；NATS 的 publish-subscribe 也强调发布者按 subject 发消息，订阅者按 subject 接收。

第三类问题是削峰填谷。流量突增时，队列作为缓冲层吸收 backlog，消费者慢慢追。没有队列时，下游数据库、第三方 API、搜索引擎可能被峰值直接打穿。有队列后，系统牺牲的是处理完成时间，不是立即把请求全部失败。

```text
10:00 瞬间进来 100 万条任务
消费者每分钟只能处理 5 万条
队列让任务排队，消费端按能力慢慢消化
```

但这也意味着队列不是消灭压力，而是移动压力。积压最终还是要处理。如果生产速度长期大于消费速度，队列只会越来越长。

第四类问题是失败隔离和重试。消费者崩溃、网络断开、下游暂时不可用时，消息不应直接丢失。SQS 的 visibility timeout 就是这个模型：消费者收到消息后，消息暂时对其他消费者不可见；如果消费者没在超时前删除消息，消息重新可见并可被再次处理。NATS JetStream 也有 `AckWait` 和 redelivery；Kafka 则通过 offset commit 决定消费者失败后从哪里继续。

第五类问题是并行处理。队列可以把任务分发给多个 worker。Kafka 用 partition 和 consumer group 实现并行；SQS standard queue 可以让多个消费者 receive；NATS queue group 让同一组订阅者中每条消息只由一个成员处理。这个能力常用来把 CPU 密集或 I/O 密集任务横向扩展。

第六类问题是 fan-out。一个事件可以被多个下游独立消费：风控、审计、通知、推荐、数仓。Kafka topic 可以有多个 consumer group 各自维护 offset；NATS pub/sub 会把消息发给所有匹配 subject 的活跃订阅者；如果要持久 replay，就要用 Kafka 这类日志或 NATS JetStream 这类持久流。

第七类问题是把处理状态显式化。消息系统通常会暴露 backlog、consumer lag、in-flight messages、redelivery count、DLQ 等指标。这些指标能让运维知道系统卡在哪里。同步 RPC 链路里，很多失败只表现成某个接口超时，排查路径更短但也更脆。

消息队列不解决的东西同样重要。

第一，它不自动提供端到端 exactly-once。Kafka 有事务和幂等 producer，SQS FIFO 有去重和顺序能力，NATS JetStream 有 ack/redelivery，但只要消费者要写数据库、调支付、发邮件，端到端语义仍然要靠幂等、事务、outbox、dedup 表和补偿来完成。

第二，它不自动提供全局顺序。Kafka 保证的是 topic-partition 内顺序，通常同 key 落到同 partition 才能说 key 内有序；SQS FIFO 是 message group 内有序；SQS standard 只做 best-effort ordering；NATS queue group 下多个消费者并行处理时，完成顺序更不能当成发送顺序。

第三，它不自动消除下游瓶颈。消费者处理能力不够，队列只是把失败变成积压。最后还是要扩容、限流、降级、批处理、拆分 topic/queue 或优化下游。

第四，它不适合所有同步业务。用户创建订单时，库存扣减、支付授权、订单号返回可能需要同步确认。把关键一致性步骤都丢进队列，会让用户看到“请求成功但结果未知”，后续补偿成本很高。

面试里可以这样回答：

```text
消息队列主要解决时间解耦、生产者消费者解耦、削峰填谷、失败重试、并行处理和 fan-out。它让前台请求不必等待后台慢任务，让消费者能独立扩缩容，让短时峰值变成 backlog，并在消费者失败时通过 ack、offset、visibility timeout 或 redelivery 重新处理消息。但队列不自动保证端到端 exactly-once，也不自动保证全局顺序，更不会消灭下游容量问题。真正可靠的设计还要处理幂等、重复投递、乱序、DLQ、监控和补偿。
```

## Q002. 队列、日志、pub/sub 的区别是什么？

队列、日志、pub/sub 经常被混着说，但它们的消费语义不同。

队列的核心语义是 work distribution：一条消息通常被某个消费者拿走处理，处理成功后确认，之后不再给同一组消费者。SQS standard queue、RabbitMQ work queue、NATS queue group 都接近这个模型。它适合任务分发，例如发邮件、转码、导入、异步调用第三方 API。

```text
message A -> worker 1
message B -> worker 2
message C -> worker 1
```

如果 worker 1 崩溃，没 ack 的消息会重新进入可消费状态。这里的重点是“工作由谁完成”，不是“所有人都看到完整历史”。

日志的核心语义是 append-only history。消息被追加到某个有序结构中，消费者通过 offset 或 sequence 记录自己读到哪里。消息不会因为某个消费者读过就立刻删除，而是按 retention 或 compaction 策略保留。Kafka topic partition 就是典型日志。Kafka 官方文档说，topic 中的 event 可以按需多次读取，消费后不会像传统消息系统那样删除，而是按保留配置丢弃旧事件。

```text
partition 0:
offset 0 -> offset 1 -> offset 2 -> offset 3

consumer group A offset = 2
consumer group B offset = 0
```

日志适合事件流、CDC、审计、数据管道、流处理和 replay。它的优势是多个 consumer group 可以互不影响地读同一份历史。代价是消费者要理解 offset、lag、重放、保留期、分区和顺序边界。

Pub/sub 的核心语义是 fan-out：发布者发到某个 topic、subject 或 channel，所有匹配的订阅者都收到一份。NATS Core 是典型 subject-based pub/sub。官方文档说，发布者发到 subject，所有活跃订阅者都会收到；如果订阅者不在线，Core NATS 默认不会帮它保存历史。

```text
publisher -> subject orders.created
subscriber A 收到
subscriber B 收到
subscriber C 收到
```

Pub/sub 适合实时通知、服务间事件广播、控制消息、在线订阅。它的关键问题是：是否持久化、离线订阅者能不能补读、消息是否需要 ack、失败是否 redeliver。NATS Core 和 NATS JetStream 在这里差别很大：Core NATS 偏实时，JetStream 提供持久 stream 和 consumer 状态。

这三者会重叠。Kafka topic 同时有日志和 pub/sub 味道：它是持久日志，但多个 consumer group 订阅同一 topic 时又像 pub/sub。NATS JetStream stream 可以像日志一样保存消息，也可以通过 consumer 做队列式消费。SQS FIFO queue 通过 message group 提供组内顺序，但它仍然是队列式 receive/delete 语义。

对比可以这样记：

```text
队列：一组消费者竞争处理，每条消息通常由一个消费者完成
日志：消息追加到有序历史，消费者自己推进位置，可以重放
pub/sub：发布给所有匹配订阅者，重点是广播和 fan-out
```

它们的状态归属也不同。

队列通常由 broker 记录消息是否 in-flight、是否 ack、是否要重投。SQS 里消息被 receive 后进入 visibility timeout，delete 后才算处理完成。NATS JetStream consumer 会记录 delivered 和 acknowledged。

日志通常让消费者位置成为核心状态。Kafka consumer 按 partition 拉取数据，并提交 offset。offset 可以存在 Kafka 的内部 topic，也可以和外部输出一起存到目标系统里。Kafka 设计文档专门提到，把消费位置和输出写在同一个地方，可以避免很多两阶段提交问题。

Pub/sub 的状态要看实现。纯实时 pub/sub 可能没有消费状态，订阅者在线就收到，离线就错过。持久 pub/sub 会引入 subscription/consumer 状态，语义就开始接近日志或队列。

面试里可以这样回答：

```text
队列关注工作分发，一条消息在同一个消费组里通常由一个消费者处理并 ack；日志关注可重放历史，消息追加到有序 log，消费者用 offset/sequence 记录自己的位置；pub/sub 关注广播，发布者发到 topic 或 subject，多个订阅者都能收到。Kafka 更像持久日志，多个 consumer group 可以独立读同一 topic；SQS 更像托管队列，用 receive、visibility timeout 和 delete 表达处理状态；NATS Core 是实时 pub/sub，JetStream 加上 stream 和 consumer 后才有持久、ack 和 redelivery。选型时要先问是任务分发、历史重放，还是事件广播。
```

## Q003. push-based 和 pull-based 消费模型有什么差异？

Push-based 是 broker 主动把消息推给消费者。消费者订阅后，broker 按自己的调度把消息发到消费者连接、回调地址或 subject。NATS Core pub/sub、NATS JetStream push consumer、很多 Webhook 系统都属于这个方向。Push 的好处是低延迟，消息一到就能推；对简单在线订阅、通知、服务请求分发很自然。

问题在于流控。消费者处理能力各不相同，broker 如果推得太快，慢消费者会被打爆。Kafka 官方设计文档也提到，push 系统的难点在于 broker 控制传输速率，当消费者处理能力低于生产速度时容易被压垮。Push 系统通常需要额外的 flow control、pending 限制、ack window、连接断开策略或 backpressure 协议。

Pull-based 是消费者主动向 broker 要消息。消费者自己决定什么时候拉、一次拉多少、处理完再拉多少。Kafka consumer 就是典型 pull：消费者向 partition leader 发 fetch request，并在请求中带上想读的 offset。SQS 也是 pull：消费者调用 `ReceiveMessage`，处理后 `DeleteMessage`。NATS JetStream 也支持 pull consumer，官方还建议新项目优先考虑 pull consumer，尤其在需要水平扩展、细粒度流控和错误处理时。

Pull 的优势是消费者控制节奏。慢消费者可以少拉，快消费者可以多拉；处理端可以按 CPU、内存、下游 QPS、批大小来调节。Kafka 还利用 pull 做批量传输，消费者一次 fetch 一批消息，吞吐更好。

```text
push:
broker -> consumer
broker 决定什么时候发

pull:
consumer -> broker: 给我 N 条
broker -> consumer: 返回一批
consumer 控制批量和节奏
```

Push 适合这些场景：

```text
在线通知
低延迟事件广播
连接稳定、消费者能力相对可控
服务发现和请求分发
NATS Core subject pub/sub
```

Pull 适合这些场景：

```text
后台批处理
任务处理耗时差异大
消费者要按自身负载调节拉取
需要批量、限速、暂停、恢复
需要更清晰地管理 ack 和 redelivery
Kafka consumer、SQS long polling、JetStream pull consumer
```

两者不是绝对对立。NATS JetStream 同时支持 push 和 pull consumer。Kafka 传统 consumer 是 pull，但 Kafka share consumer 又引入了类似队列式单条确认和 acquisition lock 的机制。SQS 是 pull，但 long polling 可以让 receive 请求等待消息出现，看起来有一点“阻塞推送”的感觉，本质仍然是客户端发起 receive。

工程上最容易踩的坑是把 push 当成“更实时所以更好”。如果消费者处理时间不可控，push 会让慢消费者积压在客户端侧，问题不一定能从 broker lag 直接看出来。Pull 至少让积压留在 broker 侧，lag、in-flight、visibility timeout、pending ack 更容易监控。

另一个坑是 pull 配置太激进。一次拉太多、并发处理太多，visibility timeout 或 ack wait 又不够长，就会造成还没处理完就 redeliver。Pull 给了消费者控制权，也把调参责任交给消费者。

面试里可以这样回答：

```text
push-based 是 broker 主动推消息给消费者，延迟低，适合在线通知和实时 pub/sub，但需要仔细做流控，否则慢消费者容易被压垮。pull-based 是消费者主动拉消息，消费者可以控制批量、节奏和并发，更适合后台任务、批处理和处理时间差异大的场景。Kafka 传统 consumer 是 pull，SQS ReceiveMessage 是 pull，NATS JetStream 同时支持 push 和 pull，并建议新项目在需要扩展和错误控制时优先考虑 pull。区别不只是方向，还包括 backpressure、批处理、可观测性和失败重投的控制权在谁手里。
```

## Q004. visibility timeout 的语义是什么？

Visibility timeout 是“消息被某个消费者拿到后，在一段时间内暂时不让其他消费者看到”的语义。它不是删除，也不是最终确认，只是一个有期限的处理租约。

SQS 的定义很清楚：消费者 receive 到消息后，消息仍然留在队列里，但对其他消费者暂时不可见；消费者应在 visibility timeout 内处理并删除消息；如果超时前没有 delete，消息会重新可见，可能被同一个或另一个消费者再次 receive。队列默认 visibility timeout 是 30 秒，可以按队列设置，也可以对单条消息用 `ChangeMessageVisibility` 调整。

```text
t0: consumer A ReceiveMessage 得到 message M
t0-t30: M 对其他消费者不可见
t10: A DeleteMessage，M 被删除，不再投递

如果 A 没删除：
t30: M 重新可见
t31: consumer B 可能拿到 M
```

这个模型的核心是：broker 不知道消费者处理是否真的成功，它只知道消费者有没有在租约期内确认完成。SQS 的确认动作是 `DeleteMessage`；NATS JetStream 的确认动作是 ack；Kafka 传统 consumer 的类似动作是提交 offset，但 Kafka 的消息不会因为 offset commit 被删除，只是该 consumer group 下次从新位置继续读。

Visibility timeout 解决的是消费者崩溃后的消息恢复。如果消费者拿到消息后进程死了、机器宕机、网络断了，它自然发不出 delete。超时后消息重新出现，系统还有机会处理它。没有这个机制，消息要么在交付后立刻删除导致丢失，要么永远锁住导致卡死。

它也引入了重复。SQS 官方文档明确说，由于 at-least-once delivery，visibility timeout 期间也不能绝对保证不会重复投递。标准队列内部会把消息冗余存储到多个服务器，某个副本在删除时不可用时，后面可能再次投递该副本。也就是说，visibility timeout 降低并发重复处理概率，但不提供“处理期间绝不重复”的强保证。

NATS JetStream 里的 `AckWait` 很像 visibility timeout。消息投递给 consumer 后，如果在 `AckWait` 内没有 ack，就会 redeliver。JetStream 还可以配置 `MaxDeliver`、`BackOff`、`MaxAckPending`。Kafka share consumer 也有 time-limited acquisition lock，记录被 acquire 后在 lock duration 内不对同 share group 其他消费者可用，过期后释放。

传统 Kafka consumer 没有 SQS 那种 visibility timeout。Kafka 的 broker 不把消息标记为 in-flight；消费者拉取记录后，处理失败时只要不提交 offset，重启或 rebalance 后会从旧 offset 重新读。超时控制更多来自 `max.poll.interval.ms`、session timeout、rebalance 和应用自己的处理逻辑。这个差异很重要，不要把 Kafka offset commit 直接等同于 SQS delete。

Visibility timeout 的正确心理模型是：

```text
receive/dispatch：消息被租给一个消费者
visibility timeout/AckWait/lock duration：租约时间
ack/delete/commit：消费者声明处理完成
timeout：租约过期，允许重新投递
```

业务代码不能把“收到消息”当作“拥有消息”。它只是暂时拿到处理权。如果处理时间超过租约，要续租；如果决定不处理，可以提前释放；如果处理成功，要及时确认；如果处理失败，要让消息按重试策略进入 redelivery 或 DLQ。

面试里可以这样回答：

```text
visibility timeout 是消息被消费者取到后的临时不可见期，本质是处理租约。以 SQS 为例，ReceiveMessage 后消息仍在队列里，但在 timeout 内不会被其他消费者正常看到；消费者处理成功后 DeleteMessage，消息才算完成；如果没在 timeout 内删除，消息重新可见并可能被再次投递。它用于消费者崩溃恢复，但不保证绝不重复。NATS JetStream 的 AckWait、Kafka share consumer 的 acquisition lock 有类似租约味道；传统 Kafka 则主要靠 offset commit 表示消费位置，没有 SQS 式 visibility timeout。
```

## Q005. visibility timeout 过短或过长分别有什么问题？

Visibility timeout 过短，最大的问题是同一条消息还在处理，租约已经过期。消息重新可见后，另一个消费者可能开始处理同一条消息。于是系统出现并发重复处理。

```text
t0: A 收到消息，预计处理 90 秒
t30: visibility timeout 到期
t31: B 收到同一条消息
t60: A 处理完成并 delete
t80: B 也可能处理了一部分副作用
```

如果消费者处理的是发邮件、扣库存、调用支付、写第三方系统，这种重复会很麻烦。即使业务做了幂等，也会浪费资源，制造锁冲突和外部 API 压力。

过短还会破坏顺序。SQS FIFO queue 的同一 message group 中，一条消息 in-flight 时，后续同组消息不会被交付，直到该消息被 delete 或 visibility timeout 到期。timeout 太短时，前一条消息可能被重复投递，整个 group 的进度变得抖动。Kafka 或 NATS 的类似 ack timeout 场景也一样：消费者慢一点就 redeliver，会让处理顺序和完成顺序更难推理。

过短还会让 redelivery count 虚高。NATS JetStream 的 `MaxDeliver`、SQS redrive policy、DLQ 都可能因为 timeout 设置不合理而误判消息“处理多次失败”。消息本来只是处理慢，却被送进 DLQ。

Visibility timeout 过长，问题反过来。消费者崩溃后，消息要等很久才重新可见。重试被延迟，业务恢复慢。

```text
t0: A 收到消息
t5: A 崩溃
visibility timeout = 30 分钟
t30m: 消息才重新可见
```

这对订单、支付、通知这类链路很难接受。用户看到的不是失败，而是长时间“处理中”。

过长还会占住 in-flight 配额。SQS 文档说，in-flight message 是已经被 receive 但还没 delete 的消息，standard queue 大约有 120,000 的 in-flight 限制。timeout 太长且消费者处理失败时，大量消息卡在不可见状态，新消息 receive 不出来。短轮询可能返回 `OverLimit`，长轮询可能只是拿不到新消息。

FIFO queue 里，过长会造成 message group 头阻塞。同一 group 后续消息要等当前 in-flight 消息 delete 或 timeout。一个长 timeout 配上一个崩溃消费者，就能让某个用户、订单或业务 key 的后续事件卡很久。

过长还会拖慢故障发现。DLQ、报警、重试延迟都被拉长。系统看起来 backlog 不一定大，因为消息不可见；但业务事实上停住了。监控要看 visible、not visible、age of oldest message、receive count、DLQ 进入速度。

比较稳的做法是按处理时间分布设置 timeout，而不是拍脑袋。

```text
短任务：timeout 略大于 p99 处理时间
长任务：较短初始 timeout + heartbeat/续租
不可预测任务：拆小步骤，或把长流程交给 workflow/状态机
失败重试：配置 backoff 和 DLQ，不要无限快速重投
```

SQS 支持 `ChangeMessageVisibility` 动态延长或缩短 timeout。官方文档也建议不确定处理时间时从较短 timeout 开始，然后用 heartbeat 续租。注意 SQS visibility timeout 有最大 12 小时限制，续租不会重置这个总上限。如果任务可能超过这个量级，应该拆任务或用 Step Functions 一类状态机。

面试里可以这样回答：

```text
visibility timeout 过短会让消息还没处理完就重新可见，导致并发重复处理、redelivery count 虚高、误进 DLQ、FIFO message group 抖动和外部副作用重复。过长会让消费者崩溃后的重试被延迟，占住 SQS in-flight 配额，造成 FIFO 组内头阻塞，也会拖慢报警和 DLQ。合理做法是让 timeout 覆盖正常 p99 处理时间，对长任务用 heartbeat/ChangeMessageVisibility 续租，处理不可预测的任务要拆小步骤，并结合 backoff、DLQ 和幂等。
```

## Q006. 消息 ack 在什么时候发送最安全？

最安全的 ack 时机是：业务副作用已经以可恢复、可判重的方式落地之后。换句话说，消费者要先把“这条消息已经处理到什么状态”写到可靠存储，再告诉 broker 这条消息可以不再投递。

对于 SQS，ack 的实际动作是 `DeleteMessage`。安全顺序通常是：

```text
ReceiveMessage
执行业务处理
把处理结果和幂等状态提交到数据库
DeleteMessage
```

如果在数据库提交前 delete，消费者崩溃时消息没了，业务状态也没落地，消息丢失。如果在数据库提交后 delete 之前崩溃，消息会重复投递，但幂等表或业务唯一键可以识别它已经完成。

对于 NATS JetStream，安全顺序类似：处理成功并持久化后再 ack。JetStream consumer 会跟踪 delivered 和 acknowledged；没有 ack 或 nack 后会 redeliver。`AckExplicit` 要求每条消息单独 ack，适合可靠处理。

对于 Kafka，ack 的对应概念通常是 offset commit。安全顺序是先把输出写成功，再提交 offset。

```text
poll records
处理 records
写数据库/下游 topic 成功
commit offset
```

如果先 commit offset 再写数据库，消费者崩溃后 Kafka 会从新 offset 继续，旧消息不会再处理，造成丢失。如果写数据库成功但 offset commit 失败，后面会重复消费。重复消费要靠数据库唯一键、幂等更新或把 offset 和输出写到同一个事务里解决。

Kafka 官方设计文档提到，写外部系统时，难点在于协调 consumer position 和实际输出；常见做法是把 offset 存在和输出同一个地方，而不是试图让 Kafka 和外部系统做通用两阶段提交。Kafka Streams 或 Kafka-to-Kafka 的事务场景可以用 Kafka transaction 把输出 topic 和 offset 原子提交；但写外部数据库、调用第三方 API，仍然需要外部系统配合。

更具体地说，ack 之前至少要满足这些条件：

```text
消息 schema 能解析，业务 key 能确定
幂等记录已经创建或检查
业务状态已经提交
外部副作用有幂等键或 outbox 记录
错误可分类：成功、可重试失败、不可重试失败
日志和指标能追踪 message id / offset / receive count
```

不可重试错误也不一定直接 ack。比如消息格式永久错误、缺少必填字段、业务对象不存在，可能应该写入失败表或 DLQ 后 ack，避免无限重试。关键是“先持久化失败结论，再 ack”，不要静默吞掉。

有些场景可以早 ack，但要承认语义变成 at-most-once。例如指标采样、非关键日志、实时在线通知，丢一条可接受，可以先 ack 或自动 ack 换低延迟。面试里要讲清楚这是业务取舍，不是通用可靠模式。

面试里可以这样回答：

```text
ack 最安全的时机是业务处理结果已经可靠落地之后。SQS 是处理成功并提交数据库后 DeleteMessage；NATS JetStream 是业务状态提交后 ack；Kafka 是输出写成功后再 commit offset。这样崩溃时最多重复，不会静默丢失。重复要靠幂等键、唯一约束、inbox/outbox、状态机或把 offset 和输出写在同一事务里处理。不可重试错误也应先写失败记录或送 DLQ，再确认。只有业务允许丢消息时，才可以先 ack 换低延迟。
```

## Q007. 先 ack 后处理和先处理后 ack 的风险分别是什么？

先 ack 后处理的风险是消息丢失。消费者已经告诉 broker“这条消息处理完了”，但实际业务还没做完。如果随后进程崩溃、机器掉电、下游写失败，broker 不会再投递这条消息。

SQS 里就是先 `DeleteMessage` 再写数据库；Kafka 里就是先 commit offset 再处理；NATS JetStream 里就是先 ack 再做业务。它们都会把语义推向 at-most-once。

```text
t0: 收到消息 M
t1: ack/delete/commit offset
t2: 进程崩溃
t3: M 不会再被正常投递
结果：消息丢失
```

这种模式不是永远错误。对非关键 telemetry、可丢弃的实时状态、日志采样，先 ack 可以降低重复和延迟。但不能用于订单、支付、扣库存、发券、账务这类必须处理的消息。

先处理后 ack 的风险是重复。消费者已经完成业务副作用，但还没来得及 ack 就崩溃。broker 以为消息没完成，于是 redeliver。

```text
t0: 收到消息 M
t1: 写数据库成功 / 调支付成功 / 发邮件成功
t2: ack 之前崩溃
t3: M 重新投递
结果：业务可能执行第二次
```

这就是 at-least-once 的典型代价。消息不会轻易丢，但消费者必须能处理重复。

风险差异可以这样记：

```text
先 ack 后处理：少重复，多丢失
先处理后 ack：少丢失，多重复
```

实际生产系统通常更愿意接受重复，而不是丢失。重复可以通过幂等、去重、唯一约束、版本号、状态机、外部 API 幂等键来控制；丢失往往只能靠审计和补偿发现。

不过先处理后 ack 也不是只加一个幂等表就结束。还要考虑副作用顺序。比如消费者写数据库成功后发邮件，邮件成功后 ack 失败，消息重投后数据库幂等跳过，但邮件可能又发一次。解决方案通常是 outbox：

```text
消费消息
在一个数据库事务里：
  写业务状态
  写 processed_message
  写 outbox email event
提交
ack 消息
另一个 relay 从 outbox 发邮件，发邮件也带幂等键
```

Kafka 里还有一个特殊点：如果消费后又写回 Kafka topic，可以用事务 producer 把输出记录和 offset 一起提交，消费者用 `read_committed`。这能在 Kafka-to-Kafka 管道里获得更强语义。但只要输出是外部数据库或第三方系统，仍然要靠外部系统配合。

面试里可以这样回答：

```text
先 ack 后处理的主要风险是消息丢失，因为 broker 已经认为消息完成，消费者崩溃后不会再正常投递；它适合可丢弃的非关键消息。先处理后 ack 的主要风险是重复，因为业务副作用成功后如果 ack/delete/offset commit 失败，消息会重新投递；这是可靠业务更常用的 at-least-once 模式。工程上通常选择先处理后 ack，然后用幂等表、业务唯一键、状态机、outbox、外部幂等键和事务边界处理重复。
```

## Q008. 消息重复投递为什么是常态？

重复投递是常态，因为消息系统很难在所有故障点上同时知道“消息是否已交付、消费者是否已处理、处理结果是否已持久化、ack 是否已成功”。只要这几个状态分布在不同机器、不同存储、不同网络请求里，就会有不确定窗口。

第一类来源是生产者重试。生产者发送消息后遇到网络超时，不知道 broker 是没收到、收到了但没返回，还是已经复制成功。Kafka 官方文档也用这个例子解释过：网络错误可能发生在消息 committed 之前或之后。生产者为了不丢，只能重试；重试可能造成重复。Kafka 的 idempotent producer 可以在 broker 侧用 producer ID 和 sequence number 去重，但这解决的是特定生产者会话和 Kafka log 内的重复，不等于端到端业务无重复。

第二类来源是消费者 ack/delete/offset commit 丢失。消费者处理成功后，ack 请求在网络中丢了，或者 broker 处理 ack 前消费者崩溃。broker 看不到完成信号，只能重投。

SQS 的 at-least-once 文档给了另一个具体原因：SQS 为高可用把消息副本存到多个服务器；如果 delete 时某个存储副本不可用，后续可能再次投递那份副本。官方文档直接要求应用设计成幂等。

第三类来源是 visibility timeout 或 AckWait 到期。消费者处理时间超过 timeout，消息重新可见；另一个消费者拿到它。原消费者可能稍后也处理成功，于是出现并发重复。

第四类来源是消费者崩溃和 rebalance。Kafka consumer 拉了一批消息，处理到一半还没 commit offset，进程崩溃。新的 consumer 接管 partition 后从上次 committed offset 继续读，会重放一部分已经处理过但没提交 offset 的消息。

第五类来源是 broker failover 和复制边界。leader 切换、ISR 变化、unclean leader election、跨 region 复制、异步复制延迟，都可能让生产者或消费者看到不确定结果。系统为了可用性和持久性做取舍时，重复通常比丢失更容易被接受。

第六类来源是手动重放和恢复。运营人员重置 Kafka consumer group offset，或者把 DLQ 消息重新导回主队列，或者重新跑一批历史事件。对业务来说，这些也是重复输入。系统如果只能处理“第一次”，恢复手段会很危险。

第七类来源是客户端并发和超时重试。HTTP 请求超时后上游重试，消息发送两次；消费者调用外部接口超时后重试，外部接口其实已经执行；消费者自己失败后又被消息系统重投。最终消息层、业务层、外部系统都可能引入重复。

所以很多官方文档都会强调 at-least-once。SQS standard queue 明确说消息可能多次投递、可能偶尔乱序；NATS JetStream consumer 文档说未 ack 或 nack 会自动 redeliver；Kafka 默认从消费者角度也是 at-least-once，除非你接受先 commit offset 的 at-most-once，或在 Kafka-to-Kafka 场景使用事务。

重复投递不是异常日志里的稀有错误，而是系统语义的一部分。它会在低概率故障、超时、重启、发布、扩容、rebalance、DLQ 重放时出现。区别只是平时少，高峰或故障时多。

面试里可以这样回答：

```text
重复投递是常态，因为发送、持久化、处理、ack、offset commit 和外部副作用分布在不同系统里，任何一步超时或崩溃都会留下不确定窗口。生产者重试可能重复写入，消费者处理成功但 ack 丢失会重投，visibility timeout/AckWait 到期会重投，Kafka rebalance 会从上次 committed offset 重放，SQS 多副本删除不完整也可能再次投递，人工重放和 DLQ 回灌也会制造重复。可靠系统默认按 at-least-once 设计，把幂等放在消费者和业务存储里，而不是假设消息只来一次。
```

## Q009. 消费者如何实现幂等处理？

消费者幂等的目标是：同一条业务消息被处理一次或多次，最终业务状态一样，外部副作用也不会重复到不可接受的程度。它不是简单地“忽略重复 message id”，而是要把消息、业务状态和副作用放进同一个恢复模型。

第一步是定义稳定的幂等键。不要用每次投递都会变化的 delivery tag、receipt handle、Kafka offset 当唯一业务幂等键。它们适合定位消息，但不一定代表业务唯一性。更好的 key 通常来自业务：

```text
order_id
payment_id
event_id
source + event_id
tenant_id + operation + idempotency_key
aggregate_id + version
```

Kafka offset 可以用于“某个 topic-partition 的处理进度”，但如果同一业务事件可能从别的 topic、DLQ、补偿脚本再来，只靠 offset 不够。

第二步是用可靠存储记录处理状态。常见做法是 inbox/dedup 表：

```sql
CREATE TABLE processed_messages (
  consumer_name text NOT NULL,
  message_key text NOT NULL,
  payload_hash text NOT NULL,
  status text NOT NULL,
  processed_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (consumer_name, message_key)
);
```

消费者处理时先尝试插入：

```sql
INSERT INTO processed_messages(consumer_name, message_key, payload_hash, status)
VALUES ($1, $2, $3, 'processing')
ON CONFLICT (consumer_name, message_key) DO NOTHING;
```

如果插入成功，当前消费者拥有处理权；如果冲突，就读取旧记录，看它是 complete、processing、failed，还是 payload_hash 不一致。payload_hash 很关键，同一个幂等键配不同请求体不能静默当成重复成功。

第三步是把 dedup 记录和业务状态放进同一个事务。比如处理订单创建事件：

```text
BEGIN
  INSERT processed_messages ...
  INSERT/UPDATE orders ...
  UPDATE processed_messages SET status='complete'
COMMIT
ack/delete/commit offset
```

如果数据库事务提交后 ack 失败，消息重投时能看到 processed_messages 已 complete，于是跳过副作用并 ack。这样重复变成可控分支。

第四步是业务更新本身要幂等。不要写：

```sql
UPDATE accounts SET balance = balance - 100 WHERE id = $id;
```

如果同一扣款消息重复来两次，就扣两次。更稳的做法是按 transaction_id 建唯一约束：

```sql
CREATE TABLE account_entries (
  account_id bigint NOT NULL,
  transaction_id text NOT NULL,
  amount numeric NOT NULL,
  PRIMARY KEY (account_id, transaction_id)
);
```

插入 entry 成功才影响余额，重复 transaction_id 被唯一约束挡住。

第五步是处理顺序和版本。对于事件驱动状态机，幂等通常要配合 version：

```text
当前订单 version = 7
消息 version = 8 -> 应用
消息 version = 8 -> 重复，跳过
消息 version = 6 -> 旧消息，跳过或记录
消息 version = 10 -> 中间缺 9，暂停或补拉
```

这比单纯记录 message_id 更能防止乱序覆盖。

第六步是外部副作用要有自己的幂等机制。发支付、发券、发邮件、调用第三方 API，数据库里的 dedup 只能保护本地状态。外部调用最好带 idempotency key。如果对方不支持幂等，就用 outbox，把“需要发外部请求”先持久化，再由 relay 发送，并记录发送状态、重试次数和对账信息。

第七步是 ack 时机要配合幂等。只有幂等状态和业务状态提交后才 ack。否则幂等记录还没落地，消息却被确认，崩溃后就没法恢复。

第八步是 TTL 和存储膨胀。幂等记录不能无限增长，也不能删得太早。保留时间至少覆盖消息最大重投窗口、DLQ 回灌窗口、上游重试窗口和审计要求。SQS FIFO 有自己的 deduplication interval，但业务幂等通常要比这个窗口更长。

第九步是并发处理。两个消费者同时处理同一业务 key 时，要靠数据库唯一约束、行锁、upsert 或乐观版本控制来抢占。不要用进程内 map 做幂等，那只能挡住单进程内的重复。

第十步是可观测性。重复不是错误，但重复率升高是信号。要记录：

```text
message_id / event_id
consumer group
Kafka topic/partition/offset 或 SQS message id/receive count
dedup 命中次数
payload_hash mismatch
处理状态
ack/delete/commit 结果
DLQ 原因
```

面试里可以这样回答：

```text
消费者幂等要先定义稳定业务幂等键，例如 source+event_id、order_id、payment_id、tenant+operation+idempotency_key，而不是只依赖 delivery tag 或 receipt handle。然后用数据库唯一约束或 inbox 表记录处理状态，把 dedup 记录和业务状态放进同一个事务，提交成功后再 ack/delete/commit offset。业务更新要用唯一流水、upsert、版本号或状态机防止重复扣款和旧事件覆盖；外部副作用要用 outbox 或第三方 idempotency key；幂等记录的 TTL 要覆盖最大重投和回灌窗口。最后要监控重复率、payload mismatch、receive count、DLQ 和 ack 失败。
```

## Q010. 消息顺序通常按队列、partition、key 还是全局定义？

消息顺序通常按某个有限范围定义，很少是全局定义。面试里听到“保证顺序”，第一反应应该是追问：哪个范围内的顺序？单队列？单 partition？单 key？单 message group？单 consumer？还是所有消息的全局顺序？

Kafka 的顺序边界是 topic-partition。Kafka 官方文档说明，topic 会被分成多个 partition；相同 event key 的事件会写到同一个 partition；给定 topic-partition 的消费者会按写入顺序读到事件。所以 Kafka 常见说法是“partition 内有序”。如果希望同一个订单的事件有序，就要让 `order_id` 作为 key，并确保分区器稳定。

```text
topic orders, 3 partitions
order_id=100 -> partition 1
order_id=200 -> partition 2

order 100 内有序
order 200 内有序
不同 order 之间没有全局顺序
```

如果没有 key，或者 key 分布改变、partition 数变化、分区器升级，同一业务实体的事件可能不再落到同一 partition。顺序设计要把 key 策略写进契约，不要只写“Kafka 保证顺序”。

SQS standard queue 不保证严格顺序。AWS 文档说 standard queue 提供 at-least-once delivery，消息可能多次投递，也可能偶尔乱序，只做 best-effort ordering。它适合可以容忍重复和乱序的任务。

SQS FIFO queue 的顺序边界是 message group。相同 `MessageGroupId` 的消息按顺序处理；一条同组消息 in-flight 时，后续同组消息不会交付，直到它 delete 或 visibility timeout 到期。不同 group 可以并行。这个模型的好处是按业务 key 保序，坏处是某个 group 的慢消息会阻塞该 group。

```text
MessageGroupId = order-100:
  event1 -> event2 -> event3 组内有序

MessageGroupId = order-200:
  可以和 order-100 并行
```

NATS Core pub/sub 更偏实时分发，不应拿它当持久全局有序日志。NATS JetStream 有 stream sequence，consumer 可以按 stream 中的序列消费；但如果用 queue group 或多个消费者并行处理，投递顺序、开始处理顺序和完成处理顺序也要分开看。一个慢消费者可能让后完成的消息先完成副作用。

传统队列的“队列顺序”也容易误解。单消费者、单线程、处理时间固定时，看起来是 FIFO；多个消费者并行、visibility timeout、redelivery、失败重试、DLQ 回灌一加入，完成顺序就不等于发送顺序。很多系统保证的是 delivery order，不保证 processing completion order。

全局顺序代价很高。要让所有消息全局有序，通常意味着单分区、单 message group、单 writer 或全局 sequencer。吞吐、可用性和跨地域延迟都会受影响。大多数业务真正需要的是“同一聚合根内有序”，例如同一订单、同一用户、同一账户、同一设备，而不是整个系统所有事件有序。

顺序还要和幂等一起设计。即使同 key 有序，也可能重复；即使顺序正确，消费者崩溃后也可能重放旧消息。消费者要用 version、sequence、状态机保护：

```text
expected_version = current_version + 1
version 小于等于 current_version：重复或旧消息，跳过
version 大于 expected_version：缺消息，暂停、补拉或告警
```

如果业务不能处理乱序，最好在消息中显式携带：

```text
aggregate_id
sequence/version
event_time
producer_id
causation_id / correlation_id
```

只依赖 broker 的到达顺序，很难覆盖重试、恢复和回放。

面试里可以这样回答：

```text
消息顺序通常按有限范围定义，不是全局。Kafka 保证 topic-partition 内顺序，相同 key 通常会落到同一 partition，所以常说按 key 保序其实是通过 partition 实现；SQS standard 只做 best-effort ordering，可能乱序；SQS FIFO 按 MessageGroupId 保序，不同 group 并行；NATS Core 不适合当持久有序日志，JetStream 可以按 stream sequence 消费，但并行消费者下完成顺序仍可能变化。全局顺序通常要单分区或全局 sequencer，吞吐和可用性代价很高。工程上更常见的是按订单、用户、账户这类业务 key 保序，再用 version 和幂等处理重复、旧消息和缺口。
```

## Q011. Kafka partition key 如何影响顺序和负载均衡？

Kafka 的 partition key 同时影响顺序和负载。它不是一个随手填的字段。key 选得好，同一业务实体的事件会稳定落到同一个 partition，消费者能按 partition log 顺序读取；key 选得差，可能出现热点 partition、消费者空闲、某些业务实体乱序。

Kafka 官方介绍里说，topic 会拆成多个 partition，相同 event key 的事件会写到同一个 partition；给定 topic-partition 的 consumer 会按写入顺序读到该 partition 的事件。设计文档里也说，producer 可以随机选择 partition 做负载均衡，也可以用 key 做语义分区，比如按 user id 分区，让某个用户的数据都进入同一个 partition。

顺序边界要说清楚：Kafka 保证的是 partition 内顺序，不是 topic 全局顺序。比如订单事件用 `order_id` 作为 key：

```text
order_id=100 -> partition 3
order_id=200 -> partition 7

order 100 内部事件有序
order 200 内部事件有序
order 100 和 order 200 之间没有全局顺序
```

这正是 Kafka 常见的建模方式。多数业务真正需要的是“同一订单、同一账户、同一设备内有序”，不是整个系统所有事件有一个全局序列。

负载均衡也由 key 决定。高基数、分布均匀的 key 会把流量摊到多个 partition；低基数或倾斜 key 会制造 hot partition。

```text
比较好的 key：order_id、user_id、device_id、account_id
容易出问题的 key：tenant_id、status、country、event_type
```

如果一个大租户占 80% 流量，而 key 只用 `tenant_id`，这个租户对应的 partition 会持续很热。你可能看到 consumer group 里还有空闲实例，但某个 partition lag 一直涨。原因是同一个 partition 在同一个 consumer group 里同一时刻只能由一个 consumer 处理，不能靠无限加 consumer 解决。

partition 数变化也会影响 key 映射。默认 hash 分区通常会用 partition 数参与计算。增加 partition 后，同一个 key 未来可能映射到新 partition；旧事件还在旧 partition，新事件进入新 partition，历史上的“同 key 全部在同一 partition”就不成立了。强依赖同 key 顺序的系统，扩 partition 前要评估是否接受这种切换，是否需要自定义 partitioner，或者按业务窗口迁移。

没有 key 时，producer 通常可以更自由地做分区负载均衡，吞吐更好，但失去业务实体级顺序。这适合指标、日志、无状态任务，不适合订单状态机和账户流水。

producer 配置也会影响顺序。Kafka producer 配置文档提醒过，如果 `enable.idempotence=false` 且 `max.in.flight.requests.per.connection` 大于 1，重试可能让后发 batch 先写入，造成乱序。启用 idempotence 后，在约束配置内能避免这类重试乱序。现在 idempotence 默认开启，但配置冲突时可能被关闭，生产排查时仍要确认。

面试里可以这样回答：

```text
Kafka partition key 决定消息写入哪个 partition。相同 key 通常进入同一个 partition，所以能获得同 key 在 partition 内的顺序；Kafka 保证的是 topic-partition 内顺序，不是全局顺序。key 也决定负载分布，高基数且均匀的 key 更容易负载均衡，低基数或热点 key 会造成 hot partition。没有 key 时负载可能更均衡，但失去业务实体级顺序。扩 partition、改 partitioner、不稳定业务 key、关闭 producer idempotence 后重试，都可能破坏顺序假设。
```

## Q012. consumer group 的再均衡会带来什么影响？

Consumer group rebalance 是 Kafka 重新分配 partition ownership 的过程。consumer 加入、离开、崩溃、订阅 topic 变化、partition 数变化、心跳失败、`max.poll.interval.ms` 超时，都可能触发 rebalance。Kafka consumer 配置文档说明，如果使用 group management 时两次 `poll()` 间隔超过 `max.poll.interval.ms`，consumer 会被认为失败，group 会把它的 partition 分配给其他成员。

第一个影响是消费暂停。旧协议和某些 assignor 下，consumer 需要 revoke partition，再拿到新 assignment 后继续消费。Kafka 4.0 后新 consumer rebalance protocol 已 GA，官方文档说它采用 fully incremental design，降低 rebalance 时间，不再依赖全局同步屏障。但线上仍然要把 rebalance 当成会影响 p99 的事件。

第二个影响是重复处理。consumer 已经处理了一批消息，但还没提交 offset，rebalance 发生了。partition 转给新 consumer 后，新 consumer 从上次 committed offset 开始读，已经处理但未提交的消息会重放。

```text
C1 读取 offset 100-199
C1 处理到 150，还没 commit
rebalance 后 partition 给 C2
C2 从 committed offset 100 开始读
offset 100-150 可能重复处理
```

第三个影响是丢失风险。反过来，如果自动提交或过早提交 offset，rebalance 后新 consumer 会从更靠后的 offset 开始。那些 offset 已提交但业务还没处理成功的消息就被跳过。这就是 offset commit 时机和 rebalance 交织后的典型坑。

第四个影响是本地状态迁移成本。Kafka Streams、Flink、自己写的有状态消费者都会维护本地 RocksDB、窗口、缓存或聚合状态。partition 换 owner 后，新实例要恢复状态或 warm cache。Kafka 设计文档提到，dynamic membership 在重启和发布时会造成任务重分配，大型有状态应用恢复成本很高；`group.instance.id` 这类 static membership 能减少短暂重启带来的 rebalance。

第五个影响是处理顺序和外部副作用。partition 内 log 顺序还在，但如果旧 consumer 把消息丢到线程池里继续异步处理，新 consumer 又开始处理同一 partition 的后续消息，外部副作用完成顺序可能错乱。rebalance 回调里要停止新任务、等待或取消旧任务、提交安全 offset、释放资源。

常见缓解方式包括：

```text
调小 max.poll.records，避免单次处理太久
确保 max.poll.interval.ms 覆盖正常处理时间
使用 group.instance.id 做静态成员
使用 cooperative sticky assignor 或新 rebalance protocol
滚动发布时控制并发
优雅停机，停止 poll -> 处理完当前批次 -> commit -> close
在 onPartitionsRevoked 中提交已完成 offset 并释放资源
```

面试里可以这样回答：

```text
consumer group rebalance 会重新分配 partition，带来消费暂停、partition ownership 变化、本地状态恢复、重复处理或消息跳过风险。未提交 offset 的已处理消息会被新 consumer 重放；过早提交 offset 的未处理消息可能被跳过。有状态应用还会有 cache、RocksDB、窗口状态恢复成本。触发原因包括成员加入离开、崩溃、心跳/session 超时、max.poll.interval.ms 超时、订阅或 partition 数变化。工程上要用幂等、正确 offset commit、rebalance listener、静态成员、协作式/增量 rebalance 和优雅停机降低影响。
```

## Q013. offset commit 的时机如何选择？

Offset commit 的时机决定 Kafka 消费端在故障时更偏向丢消息，还是更偏向重复消息。Kafka 设计文档把它讲得很直接：先保存位置再处理，consumer 崩溃后可能跳过尚未处理的消息，对应 at-most-once；先处理再保存位置，崩溃后可能重复读已经处理过的消息，对应 at-least-once。

可靠业务通常选择处理成功后再 commit：

```text
poll records
处理业务
业务状态提交成功
commit offset
```

这样 consumer 在 commit 前崩溃时，消息会重放。风险是重复，不是静默丢失。订单、支付、库存、账务、任务调度这类业务通常更愿意接受重复，因为重复可以用幂等控制；丢失更难发现。

先 commit 再处理只适合可丢消息：

```text
poll records
commit offset
处理业务
```

如果 commit 后崩溃，这批消息不会再被这个 group 正常读取。它适合非关键日志采样、临时指标、可由其他链路补齐的数据，不适合核心业务。

自动提交要谨慎。`enable.auto.commit=true` 时，consumer offset 会按 `auto.commit.interval.ms` 周期提交。问题是自动提交不理解业务处理是否已经成功。你可能 poll 到一批消息，自动 commit 先发生，业务还没写库；进程崩溃后就跳过了这些消息。可靠消费者通常关闭自动提交，再在业务成功后手动提交。

手动 commit 也要看粒度。Kafka commit 的 offset 是“下一条要读的位置”，处理完 offset 100 后应提交 101。批量 commit 吞吐好，但失败时重放范围大；逐条 commit 重放少，但协调开销更高。很多系统按“批次 + 时间 + partition”折中。

并发处理时，不能简单提交 poll 批次里的最大 offset，因为中间可能有消息还没完成。

```text
offset 10 完成
offset 11 还在处理
offset 12 完成

此时最多只能提交 11，不能提交 13
```

写外部数据库时，最稳的是把消费进度和业务输出写到同一个外部事务里。Kafka 官方设计文档也提到，写外部系统的难点是协调 consumer position 和输出；通用 2PC 很重，更常见的是把 offset 存在输出系统里。

```sql
BEGIN;
INSERT INTO orders ...;
INSERT INTO consumer_offsets(topic, partition, next_offset) ...;
COMMIT;
```

如果消费 Kafka 后再生产 Kafka，可以使用 Kafka transactions：把输出 topic 记录和消费 offset 放进同一个事务。事务 abort 时，输出不可见，offset 也不会推进；`read_committed` consumer 不会读到 aborted records。

面试里可以这样回答：

```text
offset commit 的时机本质是在丢失和重复之间取舍。先 commit 再处理是 at-most-once，崩溃会跳过未处理消息，只适合可丢场景；先处理成功再 commit 是 at-least-once，崩溃会重放，需要幂等，可靠业务通常选这个。生产上一般关闭 enable.auto.commit，按 partition 维护已连续完成的 next offset，批量或定时提交。并发处理时不能提交超过未完成 offset。写外部系统时，最好把业务输出和 offset 存在同一个事务里；Kafka-to-Kafka 流处理可以用 Kafka transactions 把输出记录和 offset 原子提交。
```

## Q014. Kafka exactly-once semantics 具体保证什么？

Kafka 的 exactly-once semantics 要先限定范围。它主要保证 Kafka 内部读、处理、再写 Kafka 的管道在故障和重试下不会让 committed 输出重复可见，并且消费 offset 与输出 topic 的写入可以原子推进。它不等于“任何消费者业务代码只执行一次”，也不等于“写外部数据库、发邮件、调用支付都 exactly once”。

Kafka 官方设计文档把问题拆成两部分：消息发布的持久性保证，以及消费消息时的位置和输出结果如何协调。Producer 遇到网络错误时，不知道消息是否已经 committed；重试可能写出重复。Kafka 的 idempotent producer 用 producer id 和 sequence number 让 broker 去重，保证重试不会在 log 里写出重复 entry。Producer 配置文档说，`enable.idempotence=true` 时 producer 会确保每条消息在 stream 中只写一份，并要求 `acks=all`、`retries>0`、`max.in.flight.requests.per.connection<=5`。

但 idempotent producer 只解决单 producer session 到 Kafka log 的重复写入。Exactly-once processing 还需要事务。

Kafka transactions 能把这些动作放进同一个事务：

```text
消费 input topic 的 records
生产 output topic 的 records
提交这些 input records 对应的 consumer offsets
```

如果事务 commit，`read_committed` consumer 能看到输出记录，offset 也推进；如果事务 abort，输出记录对 `read_committed` consumer 不可见，offset 不推进。Kafka 设计文档明确说，consumer position 存在内部 topic 中，因此可以和输出 topic 一起写入同一个事务。

一个典型 Kafka-to-Kafka EOS 流程是：

```text
consumer poll input records
producer.beginTransaction()
处理 records，producer.send(output)
producer.sendOffsetsToTransaction(offsets, groupMetadata)
producer.commitTransaction()
```

Kafka EOS 保证的边界可以这样列：

```text
producer retry 不会在 Kafka log 中写出重复 batch
一个事务内写多个 topic/partition 要么都可见，要么都不可见
消费 offset 和输出记录可以原子提交
read_committed consumer 不读 aborted transaction 的记录
```

它不保证这些：

```text
不保证外部数据库写入 exactly once
不保证 HTTP、邮件、支付这类外部副作用只发生一次
不保证源系统没有重复事件
不保证业务逻辑本身幂等
不保证 read_uncommitted consumer 看不到 aborted records
```

面试里可以这样回答：

```text
Kafka exactly-once semantics 主要保证 Kafka 内部的 read-process-write 管道：幂等 producer 防止生产者重试在 log 中写出重复，transactional producer 可以把多个 topic/partition 的输出和输入 offset 原子提交，read_committed consumer 不会读到 aborted transaction 的结果。它适合 Kafka Streams 或消费 Kafka 后再写 Kafka 的场景。它不保证外部数据库、HTTP、邮件、支付等副作用 exactly once，也不保证业务事件源本身没有重复。只要离开 Kafka 事务边界，仍然要用幂等、outbox、唯一约束和状态机。
```

## Q015. Kafka transactional producer 解决什么问题？

Kafka transactional producer 解决的是“多个 Kafka 写入以及消费 offset 提交要么一起生效，要么一起回滚”的问题。它不是普通 producer 的重命名，也不是只为了提升吞吐。它把一组 send 和 offset commit 放进事务边界，让下游 `read_committed` consumer 只看到 committed 结果。

没有事务时，一个流处理程序可能这样失败：

```text
读取 input offset 100
写 output topic A 成功
写 output topic B 成功
准备提交 offset 101
进程崩溃
```

重启后从 offset 100 再处理一次，A 和 B 可能出现重复输出。如果先提交 offset 再写输出，又可能丢输出。Transactional producer 让输出记录和 offset 一起 commit，避免这个裂缝。

典型流程是：

```text
initTransactions()
beginTransaction()
send output records
sendOffsetsToTransaction(offsets, consumerGroupMetadata)
commitTransaction()
```

如果中途失败：

```text
abortTransaction()
```

被 abort 的记录仍可能存在于 log 文件中，但对 `isolation.level=read_committed` 的消费者不可见。`read_uncommitted` 消费者仍可能看到更多底层记录，所以读端配置也属于语义的一部分。

Transactional producer 还解决 zombie producer 问题。生产者用稳定的 `transactional.id` 注册事务身份。新的 producer 实例用同一个 `transactional.id` 初始化时，broker 会通过 epoch fencing 让旧实例不能继续提交旧事务。没有 fencing，旧实例网络恢复后可能继续写入，和新实例交错。

它适合这些场景：

```text
消费 Kafka topic，处理后写回 Kafka topic
同一次处理要写多个 output topic/partition
需要把 input offset 和 output records 原子提交
Kafka Streams 的 exactly-once processing
```

它不适合这些误用：

```text
把 Kafka transaction 当成数据库事务
认为它能保护对 Redis/PostgreSQL/S3/HTTP 的写入
认为所有 consumer 都会自动看不到 aborted records
认为配置 transactional.id 后业务就不需要幂等
```

Transactional producer 也有成本。它要和 transaction coordinator 通信，维护 transaction state，commit/abort 会增加延迟。事务不能无限大；长事务会拖累下游 `read_committed` 可见性，也会增加 coordinator 和 broker 压力。生产里通常要控制事务批量大小和处理时间。

面试里可以这样回答：

```text
Kafka transactional producer 解决的是 Kafka 内多个输出记录和消费 offset 的原子提交问题。它可以 beginTransaction、send 多个 topic/partition、sendOffsetsToTransaction，然后 commit 或 abort；read_committed consumer 只看到 committed 结果。transactional.id 还用于跨实例 fencing，防止旧 producer 复活后提交旧事务。它适合 Kafka-to-Kafka 流处理和 Kafka Streams EOS，不会自动把 PostgreSQL、Redis、HTTP、支付这类外部副作用纳入事务。事务有 coordinator 通信和延迟成本，也要控制事务大小和时长。
```

## Q016. NATS JetStream 和 Kafka 的设计侧重点有什么区别？

NATS JetStream 和 Kafka 都能做持久消息与重放，但设计侧重点不同。Kafka 的中心抽象是持久化 log：topic 被分成 partition，记录追加到 partition，consumer group 按 offset 消费。JetStream 是在 NATS subject-based messaging 上增加持久 stream、consumer、ack、redelivery 和 retention 策略。它更贴近 NATS 的轻量连接、subject 路由和服务通信模型。

Kafka 更强调高吞吐顺序日志和流处理生态。它适合大规模事件流、CDC、数据管道、审计日志、Kafka Streams/Flink 这类基于 partition log 的处理。Kafka 的顺序边界是 partition，消费者位置是 offset，多个 consumer group 可以独立读取同一份 topic 历史。

JetStream 更强调 subject 到 stream 的捕获、灵活 consumer 和消息确认。NATS stream 可以绑定 subject，例如 `orders.*`；consumer 可以是 push 或 pull，可以配置 durable name、ack policy、AckWait、MaxDeliver、BackOff、MaxAckPending 等。它很适合把 NATS 原本的实时 pub/sub 扩展成可持久、可 redeliver、可 replay 的消息系统。

两者在消费模型上也不同。Kafka 传统 consumer 是 pull partition log，consumer group 内一个 partition 由一个成员消费。JetStream consumer 可以 pull，也可以 push；pull consumer 对水平扩展和流控更友好。Kafka 的 redelivery 主要来自 offset 未推进后的重放；JetStream 的 redelivery 则直接围绕 ack、AckWait 和 deliver count。

顺序模型也不同。Kafka partition 是强约束，partition 数直接决定并行度和顺序范围。JetStream stream 有 stream sequence 和 consumer sequence，但如果你用多个消费者并行处理或 queue/pull 分发，开始处理顺序和完成顺序仍要分开看。JetStream 更像“subject + stream + consumer state”的组合，Kafka 更像“partitioned log + consumer offset”的组合。

运维侧重点也不一样。Kafka 通常围绕 broker、topic、partition、副本、ISR、controller、consumer lag 运维；JetStream 则围绕 account、stream、consumer、ack pending、redelivery、storage、replicas、limits 运维。Kafka 的数据工程生态更成熟，JetStream 在 NATS 服务通信和轻量部署里更顺手。

面试里可以这样回答：

```text
Kafka 的设计中心是 partitioned append-only log，强调高吞吐持久事件流、partition 内顺序、consumer offset、consumer group 和流处理生态。NATS JetStream 是在 NATS subject pub/sub 上增加持久 stream 和 consumer 状态，强调 subject 路由、push/pull consumer、ack、AckWait、MaxDeliver 和灵活 redelivery。Kafka 更适合作为大规模事件日志和数据管道，JetStream 更适合 NATS 体系内的持久消息、任务分发和服务事件。两者都能持久化和重放，但顺序、消费状态、流控和运维模型不同。
```

## Q017. SQS standard queue 和 FIFO queue 的语义差异是什么？

SQS standard queue 的核心语义是高吞吐、at-least-once delivery、best-effort ordering。AWS 文档说 standard queue 支持几乎无限吞吐，消息至少投递一次，但可能重复，也可能偶尔乱序。它适合任务之间没有严格顺序要求，消费者能幂等处理重复的场景。

SQS FIFO queue 的核心语义是 first-in-first-out、exactly-once processing 语义和 message group 内有序。FIFO queue 要求发送 `MessageGroupId`；相同 group 内的消息按顺序交付，一条同组消息 in-flight 时，后续同组消息不会交付，直到前一条被 delete 或 visibility timeout 到期。不同 message group 可以并行。

对比可以这样看：

```text
Standard queue:
  高吞吐
  at-least-once
  best-effort ordering
  可能重复

FIFO queue:
  MessageGroupId 内有序
  支持去重窗口
  同组消息会被前一条 in-flight 阻塞
  吞吐和并行度受 group 设计影响
```

FIFO 的 exactly-once processing 也要小心理解。SQS FIFO 可以在去重窗口内用 `MessageDeduplicationId` 或 content-based deduplication 去重，避免 producer 重试造成重复入队。但消费者处理成功后 delete 失败、visibility timeout 到期、业务外部副作用重复等问题，仍然需要消费者幂等。它不是端到端 exactly-once。

Standard queue 常用于转码、邮件、索引、异步任务、可以乱序并行处理的事件。FIFO queue 常用于订单状态、账户变更、同一聚合根事件必须有序的流程。FIFO 的关键设计不是“开 FIFO 就完事”，而是 `MessageGroupId` 怎么选。group 太粗，吞吐低；group 太细，业务顺序可能不够。

面试里可以这样回答：

```text
SQS standard queue 提供高吞吐、at-least-once delivery 和 best-effort ordering，消息可能重复、可能乱序，消费者必须幂等。SQS FIFO queue 按 MessageGroupId 保证组内 FIFO，并提供去重机制；同一 group 的后一条消息会等前一条 delete 或 visibility timeout 到期，不同 group 可以并行。FIFO 更适合同一订单、账户、设备内必须有序的场景，但吞吐和可用并行度取决于 group 设计。FIFO 的去重不等于端到端 exactly-once，外部副作用仍要幂等。
```

## Q018. dead-letter queue 解决什么问题？

Dead-letter queue，简称 DLQ，解决的是“某些消息反复处理失败，不能无限堵在主队列里”的问题。它把超过最大接收次数或重试次数的消息隔离出来，让主消费链路继续推进，同时保留失败样本供排查、修复和回灌。

没有 DLQ 时，毒丸消息会反复出现：消费者拿到它，解析失败或业务失败；visibility timeout 到期后它又回来；消费者再失败。结果是主队列吞吐下降、日志刷屏、正常消息也被拖慢。Kafka 中如果消费者坚持不提交 offset，某个 partition 会卡在毒丸消息处，后续消息都无法推进。

SQS 的 redrive policy 可以把消息在 `maxReceiveCount` 后移动到 DLQ。AWS 文档建议 source queue 和 DLQ 使用同类型队列，并提醒 FIFO 队列使用 DLQ 会打破严格顺序，因为失败消息被移走后，后续同组消息可能继续处理。这个边界面试里要讲清楚。

DLQ 适合处理这些失败：

```text
消息格式永久错误
schema 不兼容
业务对象不存在且不能自动补偿
外部依赖长期失败后超过重试上限
消费者 bug 导致特定 payload 总失败
权限、配置、数据污染导致无法处理
```

DLQ 不应该变成垃圾桶。把消息丢进 DLQ 后没人看，等于延迟丢失。正确的 DLQ 需要这些配套：

```text
记录失败原因、异常栈、consumer 版本、receive count
给 DLQ age、size、进入速率做报警
有人工或自动修复流程
修复后能按条件 redrive 回主队列或单独补偿
回灌要限速，避免再次打爆主队列
```

Kafka 没有内置 SQS 那种队列级 DLQ，但应用常用 DLQ topic。消费者遇到永久错误时，把原消息、headers、topic/partition/offset、错误原因写入 `xxx.DLQ`，写成功后提交原 offset。临时错误则不要立刻进 DLQ，可以先重试、退避或写 retry topic。

面试里可以这样回答：

```text
DLQ 解决的是毒丸消息或长期失败消息反复重试、阻塞主队列的问题。消息超过最大接收次数或重试上限后被移到 DLQ，主链路继续处理正常消息，同时保留失败消息用于排查和回灌。SQS 用 redrive policy 和 maxReceiveCount；Kafka 通常用应用层 DLQ topic；NATS JetStream 可结合 MaxDeliver 和 advisories/转发逻辑处理。DLQ 不是垃圾桶，必须有报警、失败原因、修复流程和限速回灌。FIFO 场景还要注意，移走失败消息可能破坏严格顺序。
```

## Q019. 毒丸消息 poison message 如何处理？

Poison message 是一条会让消费者稳定失败的消息。它可能是 schema 不兼容、字段缺失、非法枚举、超大 payload、业务状态不可能、反序列化 bug，也可能触发消费者代码里的 panic。它和普通临时失败不同：临时失败重试可能成功，毒丸消息重试一万次还是失败。

处理毒丸消息的第一步是分类。不要所有异常都立刻 DLQ，也不要所有异常都无限重试。

```text
可重试：下游超时、限流、临时网络失败、数据库死锁
不可重试：JSON 解析失败、schema 版本不支持、必填字段缺失
需要人工判断：业务对象不存在、状态机不允许、权限缺失
```

第二步是限制重试。SQS 用 `maxReceiveCount` 配合 DLQ；JetStream 用 `MaxDeliver`、`AckWait`、`BackOff`；Kafka 应用通常用 retry topic、延迟队列、错误计数或 DLQ topic。核心是不要让同一条消息在主消费路径里无限占用资源。

第三步是保留足够上下文。只保存 payload 不够。至少要记录：

```text
原始 topic/queue/subject
partition/offset 或 message id
message key / group id
headers
schema version
异常类型和错误栈
consumer 版本
首次失败时间和最后失败时间
重试次数
```

第四步是决定是否推进 offset 或 ack。Kafka 中如果确认是不可重试毒丸消息，通常写 DLQ 成功后提交原 offset，否则 partition 会永远卡住。SQS 中进入 DLQ 后主队列消息会移走；如果应用自己判断不可重试，也可以先写失败表，再 delete 原消息。关键是“隔离成功后再推进”，避免既丢上下文又卡主链路。

第五步是修复和回灌。修复可能是发布消费者补丁、补 schema、修数据、转换 payload、给缺失对象补偿。回灌时要限速，并且带上原始幂等键。否则 DLQ 一次性回灌会制造二次故障。

第六步是防止毒丸消息扩散。消费者不要在处理失败时不断产生新的失败消息；日志要采样；告警要聚合；对超大或恶意 payload 要有大小限制和 schema 校验。安全边界上，毒丸消息也可能是攻击输入，不能把完整敏感 payload 随便打进日志。

面试里可以这样回答：

```text
poison message 是会稳定让消费者失败的消息，不能按普通临时错误无限重试。处理流程是先分类错误：临时错误退避重试，不可重试错误写 DLQ 或失败表，需要人工判断的保留上下文；然后限制最大投递次数，记录原 topic/partition/offset、key、headers、schema、错误栈、consumer 版本和重试次数。Kafka 中通常写 DLQ topic 成功后提交原 offset，避免 partition 卡死；SQS 用 redrive policy；JetStream 用 MaxDeliver、BackOff 和 ack 策略。修复后回灌要限速并保持幂等。
```

## Q020. 消息积压如何定位原因？

消息积压要先分清是生产太快、消费太慢、消费停了，还是消息系统本身慢。不要一看到 lag 就加 consumer。加 consumer 对 hot partition、下游瓶颈、毒丸消息、FIFO 单 group 阻塞都可能没用。

第一步看积压范围。Kafka 要看每个 topic-partition 的 lag，而不是只看 group 总 lag。某一个 partition lag 很高，其他 partition 很低，通常是 key 倾斜、hot partition 或该 partition 被毒丸消息卡住。所有 partition lag 都涨，可能是整体消费能力不足、下游慢、消费者发布出问题或 broker I/O 压力。

SQS 要看 visible messages、not visible messages、oldest message age、receive count、DLQ 进入速度。visible 很高说明待消费很多；not visible 很高说明大量消息 in-flight，可能消费者慢、visibility timeout 太长、delete 失败或处理卡住；oldest age 上升说明业务延迟已经变坏。

NATS JetStream 要看 consumer lag、num ack pending、num redelivered、num waiting、stream storage、consumer delivered/ack floor。ack pending 高通常说明消费者拿到了但没确认；redelivery 高说明处理超时或失败；waiting 高可能说明 pull consumer 供给和请求节奏不匹配。

第二步看消费者状态：

```text
consumer 实例数是否正常
是否频繁 rebalance
是否超过 max.poll.interval.ms
处理耗时 p95/p99 是否上升
线程池、连接池是否耗尽
GC、CPU、内存是否异常
是否大量报错后重试
是否卡在某个外部 API 或数据库锁
```

第三步看下游。很多积压根因不在消息系统，而在数据库慢查询、第三方限流、对象存储慢、支付接口超时、Redis 热 key、锁等待。消费者只是把下游慢转成了队列 lag。

第四步看消息内容。最近是否上线了新 schema？是否出现超大消息？是否某个 key 流量暴涨？是否某类消息处理复杂度高？是否有毒丸消息一直失败？按 key、event_type、tenant、payload size 做 top N 很有用。

第五步看 broker 或队列层。Kafka 要看 broker 磁盘 I/O、网络、under-replicated partitions、ISR 抖动、controller 事件、请求延迟。SQS 是托管服务，更多看 API 错误、限流、in-flight 限制、visibility timeout 和 DLQ。JetStream 要看 stream 存储、replica、server 资源和 consumer 配置。

定位路径可以这样走：

```text
1. lag 是全局涨还是某几个 partition/group 涨？
2. 消费者是否还在 poll/receive/fetch？
3. 消费者拿到消息后是处理慢、ack 慢，还是失败重试？
4. 下游依赖是否变慢或限流？
5. 是否有 hot key、毒丸消息、schema 变更或超大 payload？
6. broker/队列层是否有 I/O、复制、限流、in-flight 问题？
7. 扩容 consumer 是否真的能增加并行度？
```

处理积压也要谨慎。可以临时扩 consumer、增加 partition、拆 topic、限流 producer、暂停低优先级消息、加快批处理、修下游慢点、把毒丸消息隔离到 DLQ。不要一边故障一边盲目回灌 DLQ，也不要在下游已经过载时继续扩大消费者并发。

面试里可以这样回答：

```text
消息积压要先定位范围和瓶颈。Kafka 看 group lag 的 partition 分布、rebalance、consumer 处理耗时、broker I/O 和 hot key；SQS 看 visible、not visible、oldest message age、receive count、DLQ 和 in-flight 限制；JetStream 看 ack pending、redelivery、consumer lag 和 stream 存储。根因可能是生产速度超过消费能力、消费者挂了或频繁 rebalance、下游数据库/API 慢、毒丸消息卡住、key 倾斜、visibility timeout 配错、broker I/O 或复制问题。处理时要对症：扩容、限流、拆分、隔离毒丸、修下游、调整 timeout 或重放策略，而不是看到 lag 就直接加 consumer。
```

## Q021. 队列长度和消费延迟哪个更能反映用户体验？

更接近用户体验的是消费延迟，或者说消息从进入队列到真正完成业务处理的时间。队列长度只说明还有多少工作没做，不能直接说明用户等了多久。10 万条消息如果消费者每秒能处理 5 万条，排队时间可能只有几秒；2000 条消息如果消费者卡在下游数据库锁上，每条处理 3 秒，用户看到的就是分钟级延迟。

所以队列长度是容量指标，消费延迟是体验指标。容量指标回答“系统欠了多少账”，体验指标回答“这笔账会让用户等多久”。粗略可以这样算：

```text
backlog_seconds = 当前待处理消息数 / 当前有效消费速率
```

Kafka 的 `consumer lag` 常常是 record 数量，不是时间。它很重要，但要结合生产速率、消费速率、消息时间戳和业务 SLA。更好的指标组合是 `record lag`、`time lag` 和端到端处理延迟。SQS 里也类似：`ApproximateNumberOfMessagesVisible` 更像队列长度，`ApproximateAgeOfOldestMessage` 更接近最坏等待时间。AWS fair queues 文档里用 dwell time 描述消息从进入队列到被处理之间的停留时间，这个指标比单纯 backlog 更贴近多租户用户的感受。

NATS JetStream 要看 consumer lag、`num_ack_pending`、redelivery 和 ack floor。`ack pending` 高说明消息已经交给消费者但没有确认，问题可能在消费者处理、下游依赖或 ack 路径。只看 stream 里还有多少消息并不够。

生产监控里两类指标都要有：

```text
体验层：oldest message age、p95/p99 end-to-end latency、per-tenant dwell time
容量层：queue length/lag、in-flight/ack pending、producer rate、consumer rate
定位层：per-partition lag、retry rate、DLQ rate、payload size top N、error type top N
```

还要把正常延迟和故障延迟分开。retry topic、delay queue、定时消息里的等待可能是设计出来的，不一定代表用户正在等；但如果它属于用户可见流程，比如支付后通知、导出任务、工作流 step，那么消息年龄就会直接变成体验指标。

面试里可以这样回答：

```text
队列长度更像容量和风险指标，消费延迟更能反映用户体验。队列有多少消息不能说明用户要等多久，必须结合消费速率、消息年龄、业务优先级和租户维度。Kafka 的 lag 要换算成 time lag，SQS 要看 oldest message age 和 dwell time，JetStream 要看 ack pending 和 redelivery。看到 lag 只能说明有积压，看到消息年龄和端到端延迟升高，才说明用户体验正在变坏。
```

## Q022. 如何设计 retry topic 或延迟队列？

retry topic 和延迟队列的目标不是“失败了再扔回主队列”。它要把临时失败从主消费链路里隔离出去，等合适的时间再试，同时保留上下文，最后有上限地进入 DLQ。设计不好时，retry 会制造新的故障：消费者 sleep 卡住 partition、重试风暴打爆下游、同一条消息在几个 topic 之间无限循环。

Kafka 常见做法是分层 retry topic：

```text
orders.main
orders.retry.10s
orders.retry.1m
orders.retry.5m
orders.retry.30m
orders.dlq
```

主消费者失败后先分类错误。临时错误，比如 429、网络超时、数据库短暂不可用，写入 retry topic；不可重试错误，比如 schema 不兼容、必填字段缺失、业务状态非法，直接写 DLQ 或失败表。retry 消息要带上 `original_topic`、`original_partition`、`original_offset`、`message_key`、`attempt`、`first_seen_at`、`next_attempt_at`、`last_error`、`traceparent`、`schema_version` 和 `tenant_id`。

关键顺序是：先把 retry 消息持久化成功，再提交原 offset。否则会出现原 offset 已提交但 retry 没写成功导致消息丢失，或者 retry 写成功但原 offset 没提交导致重复处理。严格场景可以用 Kafka transaction；做不到事务时，至少要有幂等键和去重表。

Kafka 本身不是按时间自动弹出消息的延迟队列。retry topic 里的消息如果还没到 `next_attempt_at`，消费者不能简单 sleep。sleep 会占住 partition，后面已经到期的消息也读不到。常见做法是用延迟调度器按 `next_attempt_at` 管理小顶堆，到期后再转发；或者按延迟级别拆 topic；更长的延迟放进调度表、Redis 时间轮或专门调度服务。

SQS 的 delay queue 和 message timer 更直接，但最长是 15 分钟。超过这个范围，AWS 文档建议用 EventBridge Scheduler 这类调度服务。SQS 失败重试还可以利用 visibility timeout：消费者处理失败不 delete，超时后消息重新可见；处理时间不确定时，用 `ChangeMessageVisibility` 延长当前尝试。

NATS JetStream 可以用 `AckWait`、`MaxDeliver` 和 `BackOff`。`BackOff` 是 redelivery 延迟序列，设置后会覆盖 ack timeout 的节奏；普通 `nak` 不自动应用 BackOff，需要显式使用带延迟的 `nakWithDelay`。

面试里可以这样回答：

```text
retry topic 或延迟队列要把临时失败从主链路隔离出来，并有明确的重试节奏和终止条件。Kafka 通常用 main、retry.10s、retry.1m、retry.5m、DLQ 这类 topic 分层，失败后写 retry topic，带上 attempt、next_attempt_at、原 topic/partition/offset、错误原因和 trace，上游 offset 要在 retry 消息持久化后再提交。SQS 可以用 visibility timeout、delay queue 和 message timer，但 delay/message timer 最长 15 分钟；JetStream 可以用 AckWait、BackOff、MaxDeliver 和 nakWithDelay。无论哪种实现，都不要在消费者线程里 sleep，不要无限重试，要考虑顺序、幂等、DLQ 和限速回灌。
```

## Q023. 指数退避重试如何与队列组合？

指数退避解决的是“失败后不要立刻一起回来”的问题。下游已经超时或限流时，如果大量消费者同时失败又同时重试，队列会把故障放大成重试风暴。基本公式是：

```text
delay = min(base_delay * multiplier^(attempt - 1), max_delay)
delay = delay + jitter
```

比如 `base_delay=1s`、`multiplier=2`、`max_delay=5m`，前几次就是 1s、2s、4s、8s，之后逐步封顶。jitter 很重要，否则所有失败消息会在同一批时间点醒来。

和队列组合时，真正要回答的是三件事：状态放在哪里，原消息什么时候 ack，消费者是否会被阻塞。Kafka 里常见方式是失败后算出下一次尝试时间，写入 retry topic，然后提交原 offset。不要在 consumer 里 `sleep(5m)`，这会卡住 partition。SQS 可以选择不 delete 原消息，等待 visibility timeout 后重新可见；也可以 delete 原消息并发送一条带 `DelaySeconds` 的新消息。前者简单，后者更容易表达自定义 retry metadata，但要处理 delete/send 之间的失败窗口。JetStream 可以直接配置 `BackOff` 和 `MaxDeliver`，或者用 `nakWithDelay` 控制单次延迟。

退避必须先做错误分类。429、503、网络抖动、数据库死锁适合退避；JSON 解析失败、schema 版本不支持、必填字段缺失、权限永久拒绝不适合退避。不可重试错误应该尽快进入 DLQ 或 parking lot，否则只是把 DLQ 推迟了几个小时。

还要有 retry budget。常用字段是 `attempt`、`max_attempts`、`first_seen_at`、`next_attempt_at`、`deadline_at`、`last_error`。一个通知任务可以重试 24 小时，支付扣款不能无上限重试，用户会话内的 LLM 任务超过会话 TTL 后也可能没有意义。

面试里可以这样回答：

```text
指数退避和队列组合时，不要让消费者 sleep，也不要无限不提交 offset。失败后先判断是否可重试；可重试错误计算 next_attempt_at，带上 attempt、deadline、错误原因和 trace，写入 retry topic 或延迟队列，写成功后再 ack/commit 原消息。Kafka 常用多级 retry topic 或调度器，SQS 可以用 visibility timeout、ChangeMessageVisibility 或 DelaySeconds，JetStream 可以配置 BackOff、MaxDeliver 或 nakWithDelay。退避要有 cap、jitter、最大尝试次数和业务 deadline；不可重试错误直接进 DLQ。
```

## Q024. 消息体过大时应该如何处理？

消息体过大时，第一反应不应该是把 broker 限制调大。队列适合传递处理意图和少量上下文，不适合当大对象存储。大 payload 会拖慢 broker 网络和磁盘，放大 consumer 内存压力，影响 batch，增加重试成本，也让 DLQ、trace、日志和审计变得困难。

更稳的模式是 claim check：大对象放对象存储，消息里只放引用。

```json
{
  "job_id": "job-123",
  "payload_ref": "s3://bucket/path/object",
  "payload_size": 73400320,
  "payload_sha256": "...",
  "content_type": "application/jsonl",
  "schema_version": "v3",
  "expires_at": "2026-06-20T10:00:00Z",
  "idempotency_key": "tenantA:job-123"
}
```

SQS 单条消息最大 1 MiB；AWS 的大消息方案是 Extended Client Library，把 payload 放到 S3，SQS 消息里放对象引用，扩展 payload 可到 2 GB。Kafka 也能调 `max.request.size`、broker `message.max.bytes`、topic `max.message.bytes`、consumer `max.partition.fetch.bytes`，但 producer、broker、replica fetch、consumer fetch 都要配套，否则会出现能发不能收、能写不能拉的问题。NATS 在连接 `INFO` 里公布 `max_payload`，超过 server 接受上限的消息会被拒绝。

claim check 的关键是原子性和生命周期。通常先写对象存储，再发消息；消费者下载对象，校验 size 和 checksum，再处理并 ack。如果对象不存在，要区分写入未完成、权限错误、生命周期过早删除、引用错误。DLQ 和日志里不要放完整 payload，只放引用、hash、大小、schema version 和错误原因。

LogServe 里的 result ref、actor snapshot 和 checkpoint cache 也是同一类思路：shared log 或任务消息保存引用，真正的大结果和 checkpoint artifact 放在本地或 S3-compatible 存储边界。这样 replay 不需要把大对象塞进日志流。

面试里可以这样回答：

```text
消息体过大时优先用 claim check 模式：大 payload 放对象存储，队列消息只放 payload_ref、size、checksum、schema version、过期时间和幂等键。SQS 单消息最大 1 MiB，大消息可以用 Extended Client Library 放 S3；Kafka 虽然能调 max.request.size、message.max.bytes、max.partition.fetch.bytes，但会增加内存、网络、复制和重试成本；NATS 也有 max_payload 上限。消费者处理时要下载对象、校验 checksum，再 ack。不要把大 payload 写日志或 DLQ，生命周期、权限、加密和清理要单独设计。
```

## Q025. 消息 schema 变化如何保证消费者兼容？

schema 兼容的核心是把消息当成公开契约管理。消息进入 Kafka、SQS、NATS JetStream 后，可能被旧消费者、新消费者、重放任务、DLQ 回灌、审计程序在不同时间读取。生产者今天发出的消息，几天后才被某个消费者处理很常见。

第一步是每条消息都能识别 schema。可以把 schema id、schema version 或 content type 放在 header、attribute 或 envelope 里。Kafka 生态常用 Schema Registry。Kafka 官方多租户文档也提醒，Kafka 本身不内置 schema registry，但数据契约场景通常需要 registry 管理 topic 接受哪些事件类型、消费者如何解析，以及 schema evolution。

Confluent Schema Registry 对兼容性有明确分类：`BACKWARD` 表示新 schema 的消费者能读旧 schema 写的数据；`FORWARD` 表示旧 schema 的消费者能读新 schema 写的数据；`FULL` 表示两个方向都兼容。滚动发布里旧 producer、新 producer、旧 consumer、新 consumer 会同时存在，所以很多团队会要求 `BACKWARD_TRANSITIVE` 或 `FULL_TRANSITIVE`，而不是只检查上一个版本。

安全变更通常是新增可选字段、提供默认值、消费者忽略未知字段。危险变更包括删除字段、重命名字段、修改类型、修改单位、把 optional 改 required、复用字段编号、改变枚举值语义、改变分区 key 或幂等键含义。比如 `amount` 从“分”改成“元”，类型没变但业务已经坏了，应该新建 `amount_cents` 或 `amount_decimal`，不能静默改语义。

发布顺序一般是：先让消费者支持新旧 schema，再让生产者发送新 schema，观察 retention 和 DLQ 回灌窗口，最后清理旧字段。如果必须先升级生产者，那就要求旧消费者也能读新数据，也就是 forward compatibility。CI 里要做 schema compatibility check 和消费者契约测试，不要只靠代码 review 看 JSON 示例。

面试里可以这样回答：

```text
消息 schema 兼容要靠数据契约和发布流程。每条消息带 schema id/version，schema 进入 registry 或契约仓库，CI 做 backward/forward/full compatibility check。安全变更通常是新增可选字段和默认值，危险变更是删除、重命名、改类型、改单位、把 optional 改 required、复用字段编号、改变 key 语义。消费者要做 tolerant reader，忽略未知字段、处理默认值、未知 enum 降级，不支持的版本进 DLQ。滚动升级一般先让消费者兼容新旧 schema，再升级 producer，等历史消息和 DLQ 回灌窗口过去后再清理旧字段。
```

## Q026. 消息队列是否适合做任务调度器？

消息队列适合做“到期后执行”的工作分发，不适合单独承担完整任务调度器。队列擅长可靠投递、ack、重试、削峰、并发消费；调度器还要处理时间索引、取消、改期、周期任务、时区、错过执行时间后的补偿、任务去重、租户配额和可观测性。

短延迟可以直接用队列能力。SQS delay queue 和 message timer 可以把消息隐藏一段时间，最长 15 分钟；visibility timeout 可以让失败消息稍后重投；JetStream `BackOff` 可以表达 redelivery 间隔；Kafka retry topic 可以做短期退避。但如果需求是“明天 10 点提醒用户”“每月最后一个工作日生成账单”“用户可以取消尚未执行的任务”，单靠队列就很别扭。

SQS 文档对长周期调度给了清楚边界：delay queue 和 message timer 只适合有限延迟，更灵活的调度应该用 EventBridge Scheduler。Kafka 也不是天然调度器。topic 按 offset 读，不按 due time 弹出。你可以把 `execute_at` 写进消息，但消费者读到未到期消息后如果 sleep，会卡住 partition；如果 seek 回去，会增加复杂度；如果放内存，进程重启又丢调度状态。

比较稳的架构是“调度器 + 队列”：

```text
schedule store 记录 task_id、execute_at、tenant_id、payload_ref、status、lease、version
scheduler 扫描到期任务，获取 lease，投递到 queue/topic，标记 dispatched
worker 从 queue/topic 消费，执行业务，ack/commit
```

调度器负责“什么时候该执行”，队列负责“到期任务如何被 worker 可靠处理”。LogServe 也可以这样解释：workflow retry、timeout、ready step 判断属于控制面语义；队列只负责把已经 ready 的 task 分发给 worker。workflow DAG 状态、actor mailbox 顺序、LLM 调度信息仍然要在 shared log 和 materialized view 里有记录。

面试里可以这样回答：

```text
消息队列适合短延迟重试和到期后的任务分发，不适合单独做完整任务调度器。SQS delay queue/message timer 最长 15 分钟，JetStream BackOff 适合 redelivery，Kafka retry topic 可以做短期退避；但取消、改期、周期任务、时区、查询未来任务、错过窗口补偿、租户限速这些能力需要调度状态和时间索引。更稳的是调度器维护 schedule store，到期后投递消息队列，worker 从队列执行。调度器回答什么时候执行，队列回答怎么可靠分发和重试。
```

## Q027. 任务队列和事件总线的职责边界是什么？

任务队列传的是“请做这件事”，事件总线传的是“这件事已经发生”。任务队列是 command/work distribution：同一组 worker 里，一条任务通常只希望一个 worker 成功处理。它关心 ack、visibility timeout、retry、DLQ、并发度、租约、超时和执行结果。典型消息是 `GenerateThumbnail`、`SendEmail`、`RunWorkflowStep`、`CallExternalPaymentProvider`。

事件总线是 event broadcast/history。事件代表事实，生产者不应该知道有几个消费者，也不应该等消费者完成才算事实成立。多个下游可以各自订阅：审计、指标、通知、风控、数仓。典型事件是 `OrderCreated`、`PaymentAuthorized`、`WorkflowStepSucceeded`、`ActorSnapshotCreated`、`ModelLoaded`。

边界混乱会出问题。用事件总线发命令时，服务 A 发布 `SendEmailRequested`，实际上期待邮件服务必须执行；如果没有消费者，事件仍然写入成功，A 可能误以为业务完成。更清楚的设计是任务队列里放 `SendEmail` command，事件总线里放 `EmailSent` 或 `EmailFailed` fact。反过来，用任务队列承载事实历史也危险：任务 ack/delete 后，审计、重放、数仓补数就没有可靠来源。

字段也不同：

```text
任务队列消息：task_id、command_type、target、payload_ref、deadline、retry_policy、idempotency_key
事件总线消息：event_id、event_type、aggregate_id、sequence、occurred_at、producer、schema_version
```

LogServe 的例子很直观。`TaskSubmitted`、`TaskStarted`、`TaskCompleted` 是 shared log 里的事实事件，用来 replay 和重建状态；worker poll 到的 ready task 是任务队列语义，用来分发工作。workflow step 状态变化是事件，某个 worker 执行 step 的动作是任务。actor mailbox 里的 command 更接近任务/命令，`ActorCommandApplied` 是事实事件。

面试里可以这样回答：

```text
任务队列传 command，事件总线传 fact。任务队列关心某个工作由一个 worker 执行成功，所以需要 ack、visibility timeout、retry、DLQ、租约、执行结果和幂等；事件总线关心事实已经发生，多个下游可以独立订阅、重放和维护 offset。不要用事件总线伪装必须执行的命令，也不要用会被 ack/delete 的任务队列当事实历史。LogServe 里 shared log 保存 TaskSubmitted/Started/Completed 这类事实，worker 队列负责把 ready task 分发出去；一个是状态真相，一个是执行通道。
```

## Q028. 消息丢失、重复、乱序分别如何检测？

这三个问题不能只靠 broker 指标。broker 能告诉你 topic/queue 里发生了什么，但端到端问题跨越 producer、本地事务、broker、consumer、下游数据库和外部 API。检测要在业务层放标识、序号和对账机制。

消息丢失的检测靠对账。真正的丢失是“业务上应该出现的消息，没有出现在应该到达的地方”。如果 producer 在写数据库后、发消息前崩溃，Kafka 或 SQS 都不知道有消息该发。要用 outbox 或源表对账：业务表有事实、outbox 没有事件，是生产侧丢；outbox 有事件、broker 没有，是发布链路丢；broker 有事件、consumer sink 没有，是消费侧失败、积压或写下游失败。

重复检测靠全局唯一 id 和幂等存储。每条消息都要有 `event_id` 或 `idempotency_key`。消费者处理前写 `processed_message(message_id, consumer_name)`，插入成功说明第一次处理，冲突说明重复投递或重复生产。重复不一定是故障，SQS standard 是 at-least-once，Kafka 在提交 offset 前崩溃也会重复，JetStream ack 超时也会 redeliver。真正要告警的是重复率突然升高，或者重复导致外部副作用没有幂等。

乱序检测要先定义顺序范围。没有全局顺序就不要检测全局乱序。Kafka 是 partition 内有序；SQS FIFO 是 MessageGroupId 内有序；业务上常见的是 aggregate 内有序，比如同一 `order_id` 的事件序列。消息里放 `aggregate_id` 和单调 `sequence`，消费者保存每个 aggregate 的 last sequence。收到 44 但本地 last 是 42，说明 43 缺失或延迟；收到 41，说明重复或迟到；收到 43，正常推进。

可以监控这些信号：

```text
丢失：outbox unpublished count、produced vs consumed、source rows without sink rows、DLQ age
重复：dedup conflict count、duplicate event_id rate、receive count/redelivery count
乱序：sequence gap count、late event count、per-key reorder buffer size、event_time 与 processing_time 差值
```

面试里可以这样回答：

```text
消息丢失靠对账检测，重复靠唯一消息 id 和 dedup 表检测，乱序靠 per-key sequence 检测。丢失要从业务事实、outbox、broker、consumer sink 四层对账；重复要给每条消息 event_id/idempotency_key，消费者写 processed_message 表或幂等 sink；乱序要先定义顺序范围，比如 Kafka partition、SQS MessageGroupId 或业务 aggregate，然后用 sequence、last_seen、gap detector 和 watermark 判断缺口、迟到和重复。broker 指标只能辅助，端到端检测必须有业务 id、序号和对账。
```

## Q029. 如何设计端到端 trace 跨越消息队列？

跨消息队列的 trace 要解决两个问题：上下文怎么传过去，以及异步边界怎么建模。同步 RPC 可以把下游 span 当成上游 span 的 child；消息队列里生产和消费之间隔着 broker、排队时间、重试和多个消费者，不能简单照搬同步调用。

第一步是在消息里传 trace context。推荐用 W3C `traceparent` 和 `tracestate`，放到 Kafka headers、NATS headers、SQS message attributes，或者放进统一 envelope。还要保留 `correlation_id` 和 `idempotency_key`，因为 trace 用来看路径，业务纠错仍然要靠业务 id。

第二步是 span 建模。OpenTelemetry messaging semantic conventions 把 messaging operation 分成 create、send、receive、process 等类型。生产者发送消息时建 producer/send span；消费者 poll/receive 时可以建 receive span；真正执行业务逻辑时建 process span。batch receive 和 fan-out 场景更适合用 span links，把多个 producer span 链接到一个 receive/process span，而不是强行选一个 parent。

需要记录的消息字段包括：

```text
messaging.system = kafka / aws_sqs / nats
destination = topic / queue / subject
consumer group / durable consumer
partition / offset
message id
message key / MessageGroupId
receive count / delivery count / retry attempt
visibility timeout / ack wait
payload size
tenant id
```

这些字段可以进 span attribute 或 structured log，但不要全部放成高基数 metric label。`event_id`、`order_id`、`tenant_id` 的使用要受控。

redelivery 要保留原 trace context，同时给每次尝试创建新的 process span，并标记 attempt、delivery count、last_error。这样能看到同一个业务请求经历了几次处理。如果每次重投都生成完全无关的新 trace，排查会断。

面试里可以这样回答：

```text
端到端 trace 跨队列时，要把 traceparent/tracestate 放进 Kafka headers、NATS headers、SQS message attributes 或统一 envelope。生产者建 send span，消费者 receive/poll 建 receive span，业务处理建 process span；batch receive 和 fan-out 场景用 span links 连接原始 producer span，不要强行伪造成同步父子调用。span/log 里要记录 messaging.system、topic/queue/subject、consumer group、partition/offset、message id、key、retry attempt、delivery count、payload size 和 tenant id。redelivery 保留原 trace context，同时为每次尝试建新的 process span。trace 用来排查路径，端到端正确性仍然要靠 event_id、幂等键和 sequence。
```

## Q030. 多租户队列如何防止 noisy neighbor？

noisy neighbor 指一个租户的流量、慢任务或失败重试占住共享队列和消费者资源，导致其他租户的消息也变慢。多租户队列最怕只看全局 backlog：总积压可能不大，但某个安静租户的消息一直被热租户挤在后面；也可能总积压很大，但真正受影响的是一个租户。

第一层是租户标识必须进入消息。没有 tenant id，就谈不上公平、限速、隔离和按租户告警。tenant id 可以放在 message key、header、SQS standard queue 的 `MessageGroupId`、Kafka topic 命名空间，或者消息 envelope 里。它还要进入日志、trace 和 metrics。

第二层是选择隔离强度。强隔离是每个大租户独立 queue/topic/consumer pool，独立 DLQ、限速、告警和配额；弱隔离是共享队列里做公平调度、并发上限和令牌桶。强隔离成本高，但故障边界清楚；共享模式成本低，但实现和监控复杂。

Kafka 官方多租户文档强调 topic 命名空间、ACL、retention、quota、rate limit、monitoring/metering。Kafka client quota 可以按用户主体限制请求速率，避免某个租户占满 broker CPU、网络或请求处理路径。topic 命名可以用 `tenantA.orders.events` 或 `team.product.event`，再配 prefixed ACL。

SQS standard queue 可以用 fair queues。AWS 文档说明，在 standard queue 中给消息设置 `MessageGroupId` 作为租户标识后，SQS 可以缓解一个租户造成 backlog 时对其他租户 dwell time 的影响。这里的 `MessageGroupId` 不是 FIFO 顺序语义，只是 fair queue 的租户标识；这一点要和 FIFO queue 的 `MessageGroupId` 区分开。

消费者侧也要限流：per-tenant concurrency limit、per-tenant in-flight cap、token bucket、weighted fair queue、按租户拆 worker pool。重试和 DLQ 也要按租户隔离，一个租户的 poison message 不应该把全局 retry topic 打满。回灌 DLQ 时要按租户限速。

监控必须按租户拆：tenant backlog、tenant oldest age、dwell time、in-flight、retry rate、DLQ rate、consumer latency、throttled count、quiet tenant dwell time。只看全局 p95 会掩盖小租户被饿死的问题。

面试里可以这样回答：

```text
多租户队列防 noisy neighbor，先要让每条消息带 tenant id，然后按 SLA 选择强隔离或共享公平调度。强隔离是大租户独立 topic/queue、consumer pool、DLQ、配额和告警；共享模式要做 per-tenant concurrency、in-flight cap、token bucket、weighted fair queue 和按租户 retry budget。Kafka 可以用 topic 命名空间、prefixed ACL、client quota、retention 和监控隔离租户；SQS standard queue 可以用 MessageGroupId 启用 fair queues，缓解 noisy tenant 对其他租户 dwell time 的影响，但这不是 FIFO 顺序语义。监控必须看 tenant backlog、oldest age、retry/DLQ、in-flight 和 quiet tenant dwell time，不能只看全局队列长度。
```

## Q031. queue backpressure 如何反馈给生产者？

queue backpressure 的本质是把“下游处理不过来”这件事传回生产侧。不要只让队列无限变长。队列变长只是把压力藏起来，用户延迟、存储成本、重试风暴和恢复时间都会变差。一个好的队列系统至少要让生产者知道三件事：现在还能不能写、写入要不要变慢、哪些消息应该被拒绝或降级。

最直接的反馈是同步写入失败或阻塞。Kafka producer 有本地 buffer，官方 `buffer.memory` 配置说明里写得很清楚：如果 records 产生得比发送到 broker 更快，producer 会阻塞到 `max.block.ms`，之后抛异常。这个行为就是客户端侧 backpressure。`delivery.timeout.ms` 又给一条消息从 `send()` 返回后到成功或失败的总时间上限。如果应用不处理这些异常，只是无限重试，backpressure 就被绕过去了。

第二种反馈是 broker 侧限速。Kafka multi-tenancy 文档提到 client quota、request rate quota、broker-side quota，用来限制用户或租户对 broker CPU、网络、连接数和请求处理路径的占用。被 quota throttle 后，producer 的请求延迟会上升，producer metrics 里能看到 throttle time。这个信号应该进入应用的限流器，而不是只在监控图上看。

第三种反馈是业务层拒绝。比如队列里某个租户 backlog 已经超过阈值，API 可以返回 `429`、`503` 或业务错误码，告诉调用方稍后再试。对用户可见的写请求，宁可在入口处明确拒绝，也不要写进一个已经需要 30 分钟才能处理的队列。这里的阈值不要只看总 queue length，要看 per-tenant oldest age、消费者吞吐、DLQ rate 和下游错误率。

第四种反馈是降级和采样。不是所有消息都同等重要。审计、支付、订单状态这类消息不能随便丢；指标、推荐刷新、缓存预热、搜索索引补充可以降采样、合并或延迟。backpressure 反馈给生产者后，生产侧可以做这些动作：

```text
高优先级：继续写，但限制并发和超时时间
普通任务：降低发送速率，合并重复任务
低价值事件：采样、丢弃、只保留最后一条
大 payload：拒绝或改为对象存储引用
故障租户：单独限速或切到隔离队列
```

NATS Core 的思路更硬。官方 slow consumer 文档说，Core NATS 更偏向保护系统整体：消费者跟不上时，客户端可能丢消息，server 也可能断开慢消费者连接。它不是把所有压力都转成持久 backlog。JetStream 则可以用持久 stream、consumer ack、`MaxAckPending`、`MaxBytes`、`DiscardPolicy` 等机制控制堆积，但生产者仍然要观察 publish ack、错误和 stream limit。

SQS 是托管队列，生产者通常不会因为消费者慢而自动阻塞。它更像“你可以继续 SendMessage，但后果体现在 visible messages、oldest age、in-flight、DLQ 和成本上”。所以 SQS 场景更需要应用自己把 CloudWatch 指标或内部消费延迟反馈到入口限流。特别是 in-flight 接近上限时，消费者可能收不到更多消息；短轮询可能拿到 `OverLimit`，长轮询则可能只是拿不到新消息。生产侧如果继续灌，只会把恢复时间拉长。

面试里可以这样回答：

```text
queue backpressure 要从队列层、消费者层和业务层一起反馈给生产者。Kafka 可以通过 producer buffer 阻塞、max.block.ms 超时、delivery.timeout.ms、broker quota 和 throttle time 反馈；SQS 不会自动因为消费者慢而阻塞 SendMessage，通常要把 queue age、visible/not visible、DLQ rate、消费吞吐反馈到 API 限流；NATS Core 会用 slow consumer 机制保护 server，JetStream 则靠 stream limit、ack pending 和 publish ack 暴露压力。生产侧收到压力后要限速、拒绝、降级、采样、按租户隔离，而不是让队列无限变长。
```

## Q032. 如何平衡消息保留时间和存储成本？

消息保留时间不是越长越安全。保留时间越长，重放、审计和故障恢复越方便；但存储、复制、索引、备份、合规删除和恢复扫描成本都会上升。真正要平衡的是四个问题：消费者最晚多久会来读，业务允许重放多久，故障排查需要多久，法律或隐私要求数据保存或删除多久。

Kafka 里最直接的是 `retention.ms` 和 `retention.bytes`。官方 topic config 说明里，`retention.ms` 表示使用 delete retention policy 时日志最多保留多久，也代表消费者必须在多久内读走数据；`retention.bytes` 则按每个 partition 的大小限制丢弃旧 segment。注意它是 per-partition 配置，算 topic 总空间时要乘以 partition 数量。一个 50 分区 topic 配 `retention.bytes=10GiB`，理论上就是 500GiB 级别，不是 10GiB。

Kafka 还有一个容易被忽略的点：retention 是按 segment 清理，不是逐条消息精确删除。`segment.bytes` 越大，文件少、顺序 I/O 好，但 retention 粒度更粗，旧数据可能比你以为的多留一段时间。`segment.bytes` 太小，又会增加文件和索引管理成本。

NATS JetStream 的保留策略更显式。`LimitsPolicy` 按 `MaxMsgs`、`MaxBytes`、`MaxAge` 等限制保留，先碰到哪个限制就按策略删除；`WorkQueuePolicy` 更像任务队列，消息 ack 后删除；`InterestPolicy` 则根据消费者兴趣和 ack 情况删除。也就是说，如果只是任务分发，不一定要保留完整历史；如果要 replay 和审计，就不要用会在 ack 后删除的语义。

SQS 的消息保留范围更窄。官方 quota 文档给出的普通消息 retention 默认 4 天，最短 60 秒，最长 14 天。DLQ 还要单独配置 retention。AWS DLQ 文档建议 DLQ retention 要长于源队列，否则消息可能还没来得及排查就过期了。标准队列里消息移动到 DLQ 后原始 enqueue timestamp 不变，这个细节会影响你对 DLQ 剩余可排查时间的估计。

一个可落地的保留策略通常分层：

```text
热队列 / 主 topic：保留 1-7 天，覆盖正常消费延迟和短期 replay
retry topic：按最大 retry deadline 保留，多数比主 topic 短
DLQ：保留更久，例如 7-14 天，保证有人排查和回灌
审计日志 / 对象存储：按合规要求保留，和消息队列分开
低价值指标：短保留或聚合后保留
```

还要按消息类型定策略。订单、支付、权限变更这类事实事件可以保留更久；缓存刷新、搜索索引、推荐预热这类派生任务可以短保留。大 payload 不应该直接留在队列里，应放对象存储并通过生命周期策略清理。多租户系统还要按租户预算和 SLA 控制 retention，避免一个租户的数据把共享 broker 磁盘吃满。

面试里可以这样回答：

```text
保留时间要按恢复窗口、重放需求、排查窗口、合规要求和存储预算来定。Kafka 用 retention.ms 和 retention.bytes 控制时间和大小，retention.bytes 是 per-partition，segment 大小会影响清理粒度；NATS JetStream 要区分 LimitsPolicy、WorkQueuePolicy、InterestPolicy，任务队列 ack 后删除和事件日志长期保留是两种语义；SQS 默认保留 4 天，最长 14 天，DLQ retention 应该长于源队列。实践上主队列覆盖正常消费和短期 replay，retry topic 按重试 deadline，DLQ 给排查和回灌留更长窗口，审计数据放到更便宜的存储层。
```

## Q033. visibility timeout 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

visibility timeout 的核心目标是租约式排他处理：一个消费者收到消息后，在一段时间内让其他消费者看不到它，给当前消费者一个处理窗口。如果消费者成功处理并 delete/ack，消息结束；如果消费者崩溃、超时、断网或没有 delete，窗口到期后消息重新可见，再交给别的消费者处理。

所以它首先解决的是正确性和故障恢复问题，不是性能优化。它避免的是“同一条消息在正常情况下被多个消费者同时处理”，同时又避免消费者崩溃后消息永久丢失。SQS 文档把这个机制说得很直接：消息被 receive 后仍在队列里，但临时对其他消费者不可见；如果超时前没有 delete，就会重新可见。

它也有性能影响，但不是目标本身。timeout 太短，会让正在处理的消息过早重投，造成重复工作和下游压力；timeout 太长，消费者崩溃后消息要等很久才重试，用户延迟变差，in-flight 也会被占住。性能调优是在正确性语义上选一个合适窗口。

它和安全性关系不大。visibility timeout 不是访问控制，不防止未授权消费者，不保证消息内容保密，也不隔离租户。安全问题仍然要靠 IAM、ACL、加密、网络隔离、租户授权和审计。把 visibility timeout 说成安全机制，是概念错位。

它对可维护性有帮助，但属于间接效果。因为它把“消费者失败后怎么恢复”变成了队列协议的一部分，应用不用自己写一套复杂的抢占锁和超时扫描。不过可维护性也有代价：你必须监控 in-flight、oldest age、receive count、DLQ，必须给长任务做 heartbeat，必须让消费者幂等。

可以用一个简单判断来回答它的性质：

```text
主要目标：正确性 / 故障恢复
次要影响：性能和资源利用
间接收益：可维护性
不解决：安全、严格 exactly-once、业务幂等、永久锁、长周期调度
```

在 LogServe 这类系统里也可以类比。worker 拿到 task 后，需要一个 lease 或 ownership 语义防止多个 worker 正常情况下同时执行同一任务；worker 死掉后，task 又要能 redeliver。这个机制和 visibility timeout 很像。但最终结果仍然要靠 `TaskCompleted` 事件、幂等键、step 状态机来兜底，不能只靠 timeout。

面试里可以这样回答：

```text
visibility timeout 的核心目标是给消费者一个租约式处理窗口：消息被 receive 后暂时对其他消费者不可见，成功后 delete，失败或超时后重新可见。它主要解决正确性和故障恢复问题，避免正常情况下多个消费者同时处理同一条消息，又避免消费者崩溃导致消息永久丢失。性能只是受它影响：太短会重复处理，太长会延迟重试并占住 in-flight。它不是安全机制，也不提供端到端 exactly-once，消费者仍然必须幂等。
```

## Q034. visibility timeout 的典型适用场景和不适用场景分别是什么？

visibility timeout 适合“处理时间有限、失败后可以重试、重复执行可接受或可幂等”的任务。典型场景是图片转码、邮件发送、索引构建、导入任务、异步通知、工作流 step、短到中等时长的外部 API 调用。这些任务都有一个共同点：消费者拿到消息后需要一段时间处理，处理期间不希望其他消费者抢同一条消息；消费者死了以后，又希望消息能回来。

它也适合处理时间有波动但可续租的任务。SQS 支持 `ChangeMessageVisibility`，长任务可以通过 heartbeat 延长当前消息的不可见时间。比如视频转码预计 2 分钟，但有些文件要 10 分钟，就可以每 30 秒续一次租约。续租要谨慎，最好有总 deadline，不能让一个坏 worker 永远续下去。

它不适合严格不可重复的外部副作用。比如“扣款”这种动作，如果下游没有幂等键，visibility timeout 到期后的重投可能造成重复扣款。正确做法是先让外部系统支持幂等请求，或者把扣款状态写进本地事务和状态机，再允许消息重试。visibility timeout 本身只保证重新投递，不保证副作用只发生一次。

它也不适合超长任务调度。SQS visibility timeout 最大 12 小时，而且从第一次 receive 开始算，续租不重置这个上限。超过这个范围的任务，应该拆分成多个 step，或者用工作流引擎、调度表、Step Functions、Temporal 这类状态机系统。把一个 3 天任务塞进一条消息，然后靠续 visibility timeout 活着，维护成本会很高。

不适合的场景还包括：

```text
需要严格全局顺序的流水线：timeout 到期和 DLQ 会制造空洞和阻塞
需要多人同时看同一事件的 fan-out：应该用 pub/sub 或多个 consumer group
需要长期保存事实历史：应该用日志或事件存储，不是 ack/delete 队列
需要精确定时执行：用调度器，不要靠 visibility timeout
消费者不可幂等：先补幂等，再谈 redelivery
```

FIFO 队列里还要特别小心。SQS 文档说明，同一 MessageGroupId 的消息如果有一条 in-flight，后续同组消息不会可见，直到前一条 delete 或 timeout 到期。这适合同一订单内有序处理，但一个慢消息会阻塞整个 group。

面试里可以这样回答：

```text
visibility timeout 适合有限时长、可重试、可幂等的任务，比如转码、索引、异步通知、工作流 step、短中等时长 API 调用。它让消费者处理期间独占消息，崩溃后消息又能重新可见。处理时间波动时可以 heartbeat 续租，但要有总 deadline。不适合严格不可重复副作用、超长任务调度、精确定时、长期事实存储、fan-out 广播，以及消费者没有幂等的场景。FIFO queue 里还要注意同一 MessageGroupId 的 in-flight 消息会阻塞后续同组消息。
```

## Q035. visibility timeout 和相近概念最容易混淆的边界在哪里？

最容易混淆的是 visibility timeout、message retention、delay queue、lease、ack timeout、consumer lag 和业务 timeout。名字都带 timeout，但它们管的不是同一件事。

visibility timeout 是“消息被消费者拿到之后，暂时对其他消费者不可见多久”。它从 receive 开始计时。message retention 是“消息最多能在队列里保存多久”。SQS 的 message retention 默认 4 天，最长 14 天；visibility timeout 默认 30 秒，最长 12 小时。一个决定处理窗口，一个决定生命周期。

delay queue 或 message timer 是“消息刚进入队列后，先隐藏多久再允许第一次消费”。visibility timeout 是第一次消费之后隐藏。前者用于延迟开始，后者用于处理期间排他和失败重投。把两者混起来会设计出奇怪的 retry：比如本来想 5 分钟后第一次执行，却错误地用 visibility timeout，这时消息必须先被某个消费者 receive 才会进入不可见状态。

ack timeout 和 visibility timeout 很像，但语义依系统而变。NATS JetStream 的 `AckWait` 是消费者收到消息后等待 ack 的时长，超时会 redeliver；配置 `BackOff` 后又会覆盖 `AckWait` 的节奏。SQS 的 visibility timeout 是同类租约模型，但表现为消息不可见。Kafka 传统 consumer group 没有 visibility timeout，只有 offset commit、session timeout、`max.poll.interval.ms`。你不提交 offset，重启后会从旧 offset 重新消费；但 broker 不会因为单条消息超时就自动把它交给另一个消费者，除非 partition ownership 发生变化。

它还容易和业务 timeout 混。业务 timeout 是“调用下游最多等多久”或“任务多久算失败”；visibility timeout 是“队列多久后允许别人再拿”。业务 timeout 应该小于 visibility timeout，给消费者留出记录失败、写 retry/DLQ、delete/ack 的时间。反过来，如果业务 timeout 10 秒，visibility timeout 5 秒，另一个消费者可能在第一个消费者还没停止时就拿到同一条消息。

和锁也有区别。visibility timeout 像 lease，不像永久锁。它会自动过期，过期后消息可以被别人处理。你不能假设自己拿到消息后就永久拥有它。长任务必须续租；续租失败要停止副作用或用 fencing token 防止旧 worker 写入结果。

面试里可以这样回答：

```text
visibility timeout 是 receive 之后的临时不可见窗口，message retention 是消息生命周期，delay queue/message timer 是第一次消费前的延迟，ack timeout 是某些系统等待 ack 的重投触发条件，业务 timeout 是任务或下游调用的失败边界。SQS visibility timeout 从 receive 开始，默认 30 秒、最长 12 小时；SQS retention 默认 4 天、最长 14 天；delay timer 最长 15 分钟。Kafka 传统 consumer group 没有单消息 visibility timeout，它靠 offset commit 和分区 ownership。visibility timeout 更像会过期的 lease，不是永久锁，也不是 exactly-once。
```

## Q036. visibility timeout 在高并发场景下可能出现哪些隐藏问题？

高并发下第一个隐藏问题是 in-flight 上限。SQS 文档说 standard queue 大约有 120,000 条 in-flight 消息限制，取决于队列流量和 backlog。短轮询碰到上限会返回 `OverLimit`；长轮询可能不报错，只是不返回新消息。这个现象很容易被误判成“队列空了”或“消费者没问题”，实际是消息都被拿走但没 delete。

第二个问题是 timeout 太长造成并发被吃满。假设 1000 个消费者每秒 receive 很多消息，但下游卡住，所有消息都进入 not visible。此时主队列 visible 可能下降，in-flight 飙升，oldest age 继续变差。生产者还在写，消费者却拿不到更多消息。队列看起来“没有多少 visible backlog”，用户体验却在恶化。

第三个问题是 timeout 太短造成重复风暴。消费者正常 p99 处理时间是 20 秒，你把 visibility timeout 设成 10 秒，高峰期 GC、网络抖动、下游慢一点，就会出现同一条消息被多个消费者同时处理。重复处理又加重下游压力，下游更慢，更多消息 timeout，形成反馈环。

第四个问题是 heartbeat 放大 API 压力。长任务用 `ChangeMessageVisibility` 续租是合理的，但如果每条消息每秒续一次，10 万条 in-flight 就会制造巨量 API 调用。SQS message quota 文档也提醒要减少不必要的 visibility change 和重复 delete。高并发下续租间隔要按任务时长和风险设计，比如 timeout 2 分钟，处理线程每 30-45 秒续一次，并带 jitter。

第五个问题是 FIFO message group 阻塞。同一 MessageGroupId 下，一条消息 in-flight 时，后续同组消息不会可见。高并发时如果 group 设计太粗，比如所有订单都用同一个 group，吞吐会退化成串行。group 太细又可能破坏业务顺序。这个问题不能靠加消费者解决。

第六个问题是旧 worker 和新 worker 并发写结果。visibility timeout 过期后，新消费者开始处理；旧消费者其实没死，只是卡顿或 GC，随后也完成并写数据库。如果没有幂等键、版本号、fencing token 或 compare-and-set，最后状态可能被旧结果覆盖。

面试里可以这样回答：

```text
高并发下 visibility timeout 的隐藏问题主要是 in-flight 上限、重复风暴、续租 API 压力、FIFO group 阻塞和旧 worker 迟到写入。SQS standard queue 约有 120k in-flight 限制，长轮询达到上限时可能只是拿不到新消息；timeout 太长会占住 in-flight 并延迟重试，timeout 太短会让正常慢请求被重复投递；大量 ChangeMessageVisibility 会变成网络/API 瓶颈；FIFO 同组消息会被一条 in-flight 消息阻塞。最终还要靠幂等、fencing、per-group 并发设计和指标监控兜底。
```

## Q037. visibility timeout 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

第一个边界是“处理成功但 delete 失败”。消费者已经写了数据库、发了邮件或调用了外部 API，但在 delete/ack 前进程崩溃，或者网络断了。visibility timeout 到期后消息会重新可见。这个场景必然要求消费者幂等，否则你会重复写库、重复发邮件、重复扣款。

第二个边界是“delete 成功但业务结果没落稳”。如果消费者先 delete，再写数据库，进程在中间崩溃，消息不会再回来，业务结果却丢了。这就是为什么前面的问题一直强调先处理并持久化结果，再 ack/delete。若外部副作用不可回滚，就要先写本地状态或 outbox，再执行外部调用，并用幂等键关联。

第三个边界是“timeout 到期时旧消费者还活着”。这比崩溃更危险。旧消费者只是卡顿、GC、线程池阻塞或下游调用超时没返回；新消费者拿到同一条消息开始处理；随后旧消费者恢复并写入结果。解决办法是写结果时检查 attempt、lease token、version 或状态机条件。LogServe 的 epoch fencing 也是这类思路：旧 worker 不能用旧 epoch 写入 actor 或 task 状态。

第四个边界是重启后的批量重投。消费者集群重启时，大量 in-flight 消息没有 delete，等 timeout 到期后同时可见。系统恢复的一瞬间可能出现 redelivery storm。需要给消费者启动加限速，retry 加 jitter，DLQ 回灌也要限速。否则重启之后不是恢复，而是二次冲击。

第五个边界是续租失败。长任务通常会 heartbeat 续 visibility timeout，但续租请求可能失败。续租失败后，当前 worker 不应该继续无条件执行外部副作用。比较稳的写法是：续租失败就停止可重复副作用，或者在最终提交结果时用 fencing token 验证自己仍然拥有租约。

第六个边界是 DLQ 与 receive count。SQS redrive policy 用 `maxReceiveCount` 决定消息被接收多少次后进 DLQ。这个计数和 visibility timeout 配合时，要小心短 timeout 导致 receive count 快速上升，把本来只是慢处理的消息过早打进 DLQ。AWS DLQ 文档还提醒 FIFO 队列使用 DLQ 会破坏严格顺序，这在状态机类业务里要提前说明。

面试里可以这样回答：

```text
visibility timeout 暴露的核心边界是 ack/delete 与业务副作用之间没有原子性。处理成功但 delete 失败会重复投递，所以消费者必须幂等；先 delete 再处理会丢消息；timeout 到期时旧消费者可能还活着，所以写结果要有 lease token、attempt、version 或 fencing；集群重启会让大量 in-flight 消息同时 redeliver，需要限速和 jitter；长任务续租失败后不能继续盲写；maxReceiveCount 配太低会把慢消息过早送进 DLQ。visibility timeout 只能提供租约窗口，不能替代事务、幂等和状态机。
```

## Q038. visibility timeout 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

visibility timeout 本身不是一个重 CPU 算法。它的性能瓶颈通常来自状态管理、网络/API 调用、in-flight 规模、消费者处理时间和下游 I/O，而不是 CPU 算不动。不同系统表现不一样，但可以按路径拆开看。

在 SQS 这类托管队列里，应用侧最常见瓶颈是网络和 API 调用。`ReceiveMessage`、`DeleteMessage`、`ChangeMessageVisibility` 都是远程 API。短轮询太频繁会制造空响应和额外请求；长轮询可以减少空轮询。大量 heartbeat 续租会把 `ChangeMessageVisibility` 变成瓶颈；大量重复 delete 或频繁 visibility change 也会消耗 TPS。SQS quota 文档明确建议高吞吐场景用 batching、减少不必要 API 调用、使用 long poll。

第二个瓶颈是 in-flight 状态。每条被 receive 但未 delete 的消息都占一个 in-flight slot。in-flight 接近上限时，消费者不是 CPU 忙，而是拿不到更多消息。此时扩 consumer 没用，应该缩短处理时间、及时 delete、拆队列、降低 visibility timeout、修下游或提高配额。

第三个瓶颈在消费者和下游。visibility timeout 的所有指标最终都会被消费者处理耗时放大。数据库慢查询、对象存储下载慢、第三方 API 限流、线程池耗尽、GC 停顿，都会表现成消息 not visible 时间变长、timeout、redelivery、DLQ。你看到的是 visibility timeout 问题，根因可能完全在下游。

第四个瓶颈是锁竞争和调度器实现。自研队列或本地任务系统如果用一个全局锁维护“不可见消息小顶堆”，高并发 receive、ack、timeout scan 会抢同一把锁。这个问题在托管 SQS 里你看不到实现细节，但在自己写 broker 或 LogServe 这类控制面时会碰到。解决方向是按 shard/stream 分片、用时间轮、减少全局扫描、把状态写入 append-only log 后异步 materialize。

第五个瓶颈是存储 I/O。Kafka 传统 consumer group 没有单消息 visibility timeout，但 retry topic、DLQ、offset commit、事务都要落盘；JetStream 的 ack floor、consumer state、stream storage 也有 I/O。保留时间太长、segment 太多、消费者落后太多，会让 replay 和扫描成本上升。

面试里可以这样回答：

```text
visibility timeout 的瓶颈通常不是 CPU，而是网络/API、in-flight 状态、消费者处理耗时和下游 I/O。SQS 场景下 Receive/Delete/ChangeMessageVisibility 都是远程 API，短轮询、频繁续租、重复 delete 会消耗 TPS；in-flight 接近上限时消费者拿不到新消息；timeout 过期和 redelivery 往往是数据库、对象存储、第三方 API、GC 或线程池慢造成的。自研队列还要注意不可见消息索引、超时扫描和 ack 路径的锁竞争。优化方向是 batching、long polling、合理续租、及时 delete、拆分队列、修下游、分片状态，而不是只加 CPU。
```

## Q039. visibility timeout 的 correctness test、stress test 和 benchmark 应该分别测什么？

这三类测试目的不同。correctness test 验证语义对不对，stress test 找极端并发和故障下会不会崩，benchmark 测正常负载下的性能曲线。把它们混在一起会漏问题：一个系统 benchmark 很快，不代表崩溃后不会丢消息；correctness test 通过，也不代表 10 万 in-flight 下不会抖。

correctness test 要测租约语义和边界条件：

```text
receive 后消息在 visibility timeout 内不会被其他消费者正常拿到
超时未 delete 后消息会重新可见
delete 后消息不会再出现
ChangeMessageVisibility 延长后不会提前可见
ChangeMessageVisibility 设置为 0 后会尽快重新可见
处理成功但 delete 失败会重复投递，消费者幂等能挡住
先 delete 后处理崩溃会被测试识别为丢消息风险
FIFO 同一 MessageGroupId 的后续消息被 in-flight 前序消息阻塞
maxReceiveCount 到达后进入 DLQ
```

这里要故意制造崩溃点。比如在“写业务结果后、delete 前”杀进程；在“receive 后、处理前”杀进程；在“续租失败后”继续尝试提交结果；在 timeout 到期前后制造两个消费者竞争。correctness test 的断言应该看业务表、dedup 表、DLQ、receive count，而不只是看日志里有没有报错。

stress test 要测高并发和异常组合：

```text
大量消费者同时 receive/delete/change visibility
in-flight 接近上限时系统表现
短 timeout + 慢下游导致的重复风暴
长 timeout + 消费者崩溃导致的恢复延迟
批量消费者重启后的 redelivery storm
FIFO hot group 阻塞
DLQ 大量回灌
租户流量倾斜
网络抖动和 API 限流
```

stress test 的指标包括 duplicate rate、lost count、oldest age、in-flight、receive count 分布、DLQ rate、消费者错误率、下游错误率、恢复时间。它不追求漂亮吞吐，追求把系统推到边界后还能解释行为。

benchmark 要测稳定负载下的效率。比如固定消息大小、固定处理耗时、不同 consumer 数、不同 visibility timeout、不同 batch size、不同 long polling 设置，测吞吐、p50/p95/p99 端到端延迟、API 调用数、ChangeMessageVisibility 次数、DeleteMessage 批量效率、成本估算。不要只报 messages/s，还要报“每成功处理一条消息需要多少 API 调用”和“重复率”。

如果是 LogServe 这类自研 runtime，还要加几类内部 benchmark：lease table 或 materialized view 更新成本、超时扫描成本、worker kill 后 redelivery 时间、log replay 后是否恢复 in-flight 状态、fencing 是否挡住旧 worker 写入。correctness test 对应状态机，stress test 对应故障和并发，benchmark 对应热路径成本。

面试里可以这样回答：

```text
correctness test 测语义：receive 后不可见、超时后重投、delete 后不再出现、续租生效、设置 0 可见、崩溃点不丢消息、重复投递被幂等挡住、FIFO group 阻塞和 DLQ maxReceiveCount。stress test 测边界：大量 in-flight、短 timeout 重复风暴、长 timeout 恢复慢、消费者批量重启、API 限流、hot group、DLQ 回灌、租户倾斜。benchmark 测稳定性能：吞吐、端到端延迟、API 调用数、续租频率、delete batch 效率、重复率和成本。三者不能互相替代，尤其不能用 benchmark 结果证明 correctness。
```

## Q040. 如果要求从零实现一个简化版 visibility timeout，你会先定义哪些不变量？

我会先定义状态机和不变量，而不是先写定时器。visibility timeout 的本质不是“睡一段时间再重试”，而是给一次投递发放一个有期限的租约。租约、ack、redelivery 的边界如果没有定义清楚，后面无论用 Redis、MySQL、内存堆、Raft log 还是 Kafka compacted topic 存状态，都会在崩溃和并发下出错。

一个简化模型可以把消息分成几个状态：

```text
Ready: 可以被 receive 拿到。
InFlight: 已经交给某个消费者，当前租约未结束。
Acked/Deleted: 已经完成，不再投递。
DeadLettered: 超过最大投递次数，进入死信队列或失败表。
Expired: InFlight 的派生判断，表示 now >= visible_at，可以重新变成 Ready。
```

我会先写下这些不变量：

```text
1. 每条消息有稳定的 message_id，但每次投递有新的 delivery_id 或 receipt_handle。
2. receive 必须原子地把消息从 Ready 变为 InFlight，并设置 owner、delivery_id、visible_at、attempt。
3. 同一条消息在同一时刻最多只有一个有效租约，即 current_delivery_id 只有一个。
4. ack/delete 只能作用于当前有效租约，不能只凭 message_id 删除消息。
5. 过期租约不能阻止 redelivery，旧消费者后来的 ack 要么被拒绝，要么必须被 fencing 住。
6. ack 成功后消息进入终态，后续 receive 不应再返回它。
7. 处理失败但未 ack 的消息不能丢，timeout 后必须重新可见或进入 DLQ。
8. attempt 只能单调增加，maxDeliver 到达后不能无限循环打爆下游。
9. visible_at 的判断必须使用服务端时间或单调时间，不能信任消费者本地时钟。
10. 如果需要 FIFO/group 语义，同一 group 前序消息 InFlight 时，后序消息不能被正常投递。
11. receive、ack、timeout、redrive 这些状态变更要可观测，至少能查到 attempt、last_error、last_delivery_time。
```

这里最容易被忽略的是 `delivery_id` 或 `receipt_handle`。SQS 的 `DeleteMessage` 要求用最近一次 receive 返回的 receipt handle，而不是 message id；这不是 API 小细节，而是在解决旧租约误删新投递的问题。假设消费者 A 拿到消息后卡住，timeout 到期后消费者 B 拿到同一消息并开始处理，A 又恢复并发送 ack。如果系统只认 message_id，A 的旧 ack 可能会删除 B 正在处理的当前投递。

实现上可以先用一张状态表表达租约：

```text
message_id
payload_ref
state
visible_at
current_delivery_id
owner_id
attempt
max_attempts
created_at
updated_at
last_error
```

receive 的语义是：在一个事务或 CAS 中找 `state=Ready` 或 `state=InFlight and visible_at <= now` 的消息，设置为 `InFlight`，生成新的 `current_delivery_id`，`attempt += 1`，并返回 payload 和 delivery_id。ack 的语义是：只有当 `state=InFlight and current_delivery_id = request.delivery_id` 时，才能把状态改成 `Deleted`。timeout 可以不靠后台线程主动扫描，也可以在 receive 查询时惰性判断；但无论主动还是惰性，都必须满足“过期后可重新投递，未过期时正常不可投递”。

崩溃点也要提前定义。比较安全的顺序是先持久化 InFlight 状态，再把消息返回给消费者；如果先返回、后持久化，broker 崩溃后会忘记这次投递，可能立刻重复投递。ack 也一样，服务端要先持久化 Deleted/Acked，再向消费者返回 ack 成功。消费者侧则要接受“业务提交成功但 ack 响应丢失”这个现实，所以业务处理必须幂等。

如果面试官追问“能不能做到 exactly once”，边界要说清楚：visibility timeout 自己只能提供租约式的 at-least-once 工作分配，不能证明业务副作用 exactly once。要接近端到端 exactly once，需要幂等键、去重表、事务性 outbox/inbox，或者像 Kafka 事务那样把消费进度和生产结果放进同一个事务边界。

面试里可以这样回答：

```text
我会先定义状态机和租约不变量：消息有 Ready、InFlight、Acked、DeadLettered 等状态；receive 必须原子地生成新的 delivery_id/receipt_handle，并设置 visible_at；同一消息同一时刻最多只有一个有效租约；ack/delete 必须带当前 delivery_id，旧租约 ack 不能删除新投递；timeout 到期后消息必须重新可见；attempt 单调增加，超过 maxDeliver 进 DLQ；时间以服务端为准；如果有 FIFO group，前序 InFlight 会阻塞后序。这个机制保证的是 at-least-once 和故障恢复，不是端到端 exactly once。
```

## Q041. visibility timeout 的常见误用是什么，误用后通常会产生什么线上症状？

visibility timeout 最常见的误用，是把它当成“不会重复处理”的保证。它真正保证的是一段时间内尽量避免其他消费者同时处理同一条消息，但在 SQS 这样的 at-least-once 系统里，即使还在 visibility timeout 窗口内，也不能把重复投递当成绝对不可能。NATS JetStream 的 AckWait、MaxDeliver、Backoff 也是在定义 redelivery 策略，不是在替消费者完成幂等。

常见误用可以分几类：

```text
timeout 设置过短：任务还没处理完就重新可见。
timeout 设置过长：消费者崩溃后要等很久才重试。
把 visibility timeout 当分布式锁：没有 fencing token，也没有业务资源保护。
先 ack/delete 再执行业务：进程崩溃会造成应用层丢任务。
业务成功后不及时 ack/delete：消息会重复投递，in-flight 长时间占用。
没有幂等键：重复投递直接变成重复扣款、重复发券、重复发邮件。
没有 DLQ 或 maxReceiveCount：毒丸消息无限重试，拖垮消费者。
盲目续租：任务卡死后一直不可见，恢复时间被人为拉长。
每几秒 ChangeMessageVisibility：API 成本和限流风险上升。
用消息队列当精确定时器：timeout 和 delay 都不是强实时调度语义。
只看 queue depth：大量消息可能已经 InFlight，表面队列不长，用户仍然很慢。
```

timeout 太短时，会看到 duplicate rate 上升、同一个 message_id 的 receive count 很快增加、下游出现重复写入或唯一键冲突、日志里同一任务被多个 worker 同时处理。timeout 太长时，队列长度可能不高，但 oldest age、端到端延迟和用户等待时间持续变大；消费者崩溃后，任务像“卡住”一样迟迟不恢复。

把 visibility timeout 当锁用时，症状会更隐蔽。比如某个消费者拿到消息后开始更新订单，timeout 到期后另一个消费者也开始更新同一订单，第一个消费者最后恢复并写回旧结果。如果业务表没有版本号、fencing token 或幂等状态机，就会出现状态回退、重复状态流转、支付和发货状态不一致。这个问题不是把 timeout 调长就能解决的，因为只要任务可能超过 timeout 或消费者可能暂停，旧 worker 就可能回来。

还有一种误用是把 DLQ 当垃圾桶。消息进了 DLQ 以后不分析原因，只是定时整批 redrive 回主队列。结果毒丸消息反复进入主队列，触发重试风暴，正常消息排队时间也变长。DLQ 应该用于隔离和诊断无法成功处理的消息，而不是掩盖主逻辑错误。

正确做法一般是：timeout 取任务 p99 或最大合理处理时间，长尾任务用心跳续租；业务写入使用 idempotency key 或唯一约束；ack/delete 放在业务提交之后；maxReceiveCount 和 DLQ 必须配置；监控不能只看 visible messages，还要看 in-flight、age、receive count、DLQ rate、duplicate rate、ChangeVisibility 调用量。对于预计超过 12 小时或需要人工等待的流程，不要硬撑 visibility timeout，应该拆任务或交给工作流/调度系统。

面试里可以这样回答：

```text
常见误用包括把 visibility timeout 当 exactly-once 或分布式锁、timeout 过短、timeout 过长、先 ack 后处理、处理成功后不 delete、没有幂等、没有 DLQ、盲目续租、把队列当精确定时器。线上症状是重复处理、下游唯一键冲突、重复扣款或发通知、in-flight 打满、队列看起来不长但用户等待很久、DLQ 暴涨、毒丸消息反复 redrive、FIFO group 被一个慢任务卡死。修复方向不是单纯调大 timeout，而是补幂等、fencing、合理续租、及时 ack、maxReceiveCount、DLQ 和端到端延迟监控。
```

## Q042. visibility timeout 在单机和分布式环境中的语义有什么差异？

单机环境里，visibility timeout 更像一个本地内存状态机。一个进程持有队列、定时器和消费者线程，消息从 ready list 移到 in-flight map，过期后再移回 ready list。只要进程不崩溃，本地锁和单调时钟就能比较容易地保证“未过期不再投递”。很多简化实现甚至可以用一个 mutex、一个最小堆、一个 map 完成。

但单机语义有一个大前提：故障模型很弱。如果整个进程崩溃，内存里的 in-flight 状态、attempt、deadline 都会丢，除非把这些状态写入 WAL 或数据库。单机队列可以在进程内避免并发投递，但很难在机器宕机后继续证明消息没有丢、没有重复、没有旧 ack。也就是说，单机实现容易把“正常运行时正确”误认为“崩溃恢复后正确”。

分布式环境里，visibility timeout 不只是定时器，而是一个复制的租约协议。差异主要在这里：

```text
时间来源：不能信任消费者本地时间，应该以 broker/server 时间为准。
租约持久化：receive 返回前，租约状态要写入可靠存储或复制日志。
并发控制：多个 broker/分片/leader 可能同时竞争同一消息，需要 CAS、事务或 leader 所有权。
旧 ack：网络延迟会让旧消费者在 redelivery 之后才 ack，必须用 receipt_handle 或 generation fencing。
故障恢复：leader 切换后，新 leader 要能恢复 in-flight 和 visible_at。
重复投递：网络分区、复制延迟、存储副本不可用都会让重复更现实。
指标一致性：in-flight、visible、oldest age 往往是近似值或延迟值。
```

SQS 的语义体现了这种分布式现实。消息 receive 后会暂时不可见，但标准队列仍然是 at-least-once，应用必须能处理重复消息；DeleteMessage 使用 receipt handle，并且每次 receive 同一消息都会得到新的 handle。NATS JetStream 的 consumer 是 stream 的有状态视图，负责跟踪 delivery 和 acknowledgment；durable consumer 的状态可以恢复，但 AckWait 到期仍会触发 redelivery。Kafka 经典 consumer 不是 visibility timeout 模型，它用 partition ownership 和 offset commit 表达进度；分区同一时刻通常归一个 group member，但 rebalance、崩溃和 offset commit 时机仍会带来重复或跳过风险。

单机和分布式还有一个重要差别：单机可以比较容易地做强互斥，分布式系统通常更愿意暴露 at-least-once，再要求消费者幂等。强互斥在分布式里成本高，而且一旦遇到 pause、网络分区、leader failover、复制延迟，就必须引入租约 token、任期号、quorum 写入和恢复协议。对于消息队列来说，让消息可重复、业务可幂等，往往比承诺“绝不重复”更可控。

面试里可以这样回答：

```text
单机里 visibility timeout 主要是本地状态机和定时器问题，ready、in-flight、deadline 可以放在内存里，用 mutex 和单调时钟控制，难点在进程崩溃后如何恢复。分布式里它是复制租约问题，必须考虑服务端时间、持久化、leader 切换、CAS 或事务、旧 ack fencing、网络延迟、重复投递和近似指标。SQS、NATS JetStream 都把它设计成 at-least-once 语义，要求消费者幂等；Kafka 经典 consumer 不是 visibility timeout，而是 partition ownership 加 offset commit。
```

## Q043. ack 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

ack 的核心目标是让消费者告诉消息系统：“这次投递对应的工作已经完成，可以推进队列状态了。”在 SQS 里，这个动作通常是 `DeleteMessage`，表示这条消息可以从队列中删除；在 NATS JetStream 里，是对某条 delivery 的 acknowledgment，consumer 用它跟踪哪些消息已经完成，哪些需要 redelivery；在 Kafka 经典 consumer 里，更接近的是 offset commit，表示这个 consumer group 已经安全推进到某个 offset 之后。

所以 ack 首先解决的是正确性问题。没有 ack，broker 不知道消息是否真的处理完成，只能在超时后重投，或者在投递后直接丢弃。前者会无限重复，后者会丢消息。ack 给系统建立了一个完成边界：收到消息不等于完成，业务处理成功也不等于 broker 已经知道完成，只有完成后的确认被 broker 接受，队列状态才可以前进。

ack 也影响性能和成本：

```text
正确性：决定消息是否会 redeliver，决定 offset 是否推进，决定是否会跳过未处理消息。
性能：ack 太频繁会增加网络/API/协调器压力；ack 太慢会增加 in-flight、lag 和重复风险。
成本：SQS DeleteMessage、ChangeMessageVisibility、ReceiveMessage 都是 API 调用，批量确认会影响成本。
可维护性：明确 ack 边界后，排查重复、丢失、DLQ 和 lag 会容易很多。
安全性：通常不是 ack 的主要目标，但 ack 请求本身需要鉴权，避免非授权消费者删除或推进消息。
```

ack 解决不了业务副作用的原子性。比如消费者写数据库成功后，发送 ack 时网络超时；broker 可能没收到 ack，于是消息会重投。反过来，如果消费者先 ack 再写数据库，ack 成功后进程崩溃，broker 不会再投递，业务结果就丢了。这个窗口说明 ack 只是消息系统的完成信号，不是业务事务本身。要把业务写入和消费进度放到同一正确性边界，需要幂等表、事务 outbox/inbox、Kafka transaction，或者业务侧状态机。

Kafka 这里尤其容易说错。Kafka 经典 consumer 的 commit 不是“删除消息”，因为 Kafka topic 是日志，消息按 retention 保留；commit 只是记录 consumer group 的已处理位置。commit offset 10 通常表示 10 之前的记录都被认为处理完了，后续重启会从这个位置恢复。如果并发处理同一 partition 的多条消息，先完成 offset 10，却还有 offset 8 没完成，直接提交到 11 就可能跳过 8。这和 SQS/NATS 的逐条 ack 很不一样。

面试里可以这样回答：

```text
ack 的核心目标是建立“处理完成”的系统边界，让 broker 可以删除消息、推进 consumer 状态或提交 offset。它主要解决正确性问题：没有 ack 就无法区分处理中、成功、失败和需要重投。它也影响性能，因为 ack 频率、批量大小和提交策略会影响网络、I/O、协调器和 API 成本。安全性不是主要目标，但 ack/delete/commit 必须鉴权。需要强调的是，ack 不是业务事务，ack 成功不代表业务副作用 exactly once；业务成功但 ack 失败会重复，ack 成功但业务未提交会丢结果，所以消费者必须设计幂等和提交顺序。
```

## Q044. ack 的典型适用场景和不适用场景分别是什么？

ack 适合用在消费者能明确判断“这条消息已经处理完成”的场景。比如任务队列、订单异步处理、日志落库、搜索索引更新、邮件发送、视频转码、数据同步、Webhook 投递、事件驱动的投影构建。共同点是：消息系统需要知道哪些任务完成了，未完成的要重试，失败多次的要进 DLQ 或告警。

典型适用方式是业务提交成功后 ack：

```text
1. receive/poll/fetch 消息。
2. 用 message_id、event_id、业务 idempotency key 做幂等检查。
3. 执行业务逻辑，并把结果提交到数据库、对象存储或下游服务。
4. 只有业务结果确定成功后，才 ack/delete/commit offset。
5. 如果业务失败，不 ack，或显式 nack，交给 redelivery/DLQ 策略处理。
```

SQS worker 通常在处理成功后调用 DeleteMessage。NATS JetStream 使用 AckExplicit 时，每条消息处理完显式 ack，AckWait 内未 ack 会 redeliver。Kafka consumer 如果关闭自动提交，通常在处理完成后手动 commit offset；如果还会向 Kafka 生产结果，可以考虑事务性消费-生产流程，避免“结果发出但 offset 没提交”或“offset 提交但结果没发出”的不一致。

ack 不适合的场景也要明确：

```text
纯 telemetry/fire-and-forget：允许少量丢失时，强 ack 可能成本太高。
实时广播：每个订阅者都可靠 ack 会让系统变成持久队列，语义不同。
不可幂等副作用：重复投递无法承受，又没有去重和补偿机制。
长时间人工流程：靠一个 visibility timeout hold 几小时或几天并不合适。
精确定时任务：ack 只能表达完成，不提供精确调度。
分布式锁：ack 不是资源 fencing，也不能保护数据库里的共享资源。
安全授权：ack 不能代替权限检查，只能确认消息处理状态。
```

还有一个边界是自动 ack 或自动提交。它适合低风险、处理很快、重复或丢失影响可接受的场景，但不适合支付、库存、订单状态流转这类强业务语义。Kafka 的 `enable.auto.commit` 如果在消息真正处理完成前提交 offset，消费者崩溃后就可能跳过未处理记录。NATS 的 `AckNone` 表示服务端把投递当成已确认，适合观测或顺序 replay 这类不需要逐条可靠处理的场景，不适合任务队列。

如果任务处理时间变化很大，ack 还要和续租或 backoff 配合。处理中的长任务需要 heartbeat/ChangeMessageVisibility 或合适的 AckWait；失败任务需要 maxReceiveCount/MaxDeliver 和 DLQ；下游限流时不要疯狂 nack 立即重投，而是延迟重试或暂停消费。ack 只是完成信号，重试节奏仍然要单独设计。

面试里可以这样回答：

```text
ack 适合任务队列和可靠事件处理：消费者处理成功后确认，失败则不确认或 nack，让系统重投或进 DLQ。典型顺序是先做幂等检查，再提交业务结果，最后 ack/delete/commit。它不适合被当成分布式锁、精确定时器、安全授权或业务事务；也不适合不可幂等且没有去重的副作用。自动 ack/自动提交只适合低风险场景，支付、库存、订单这类流程应使用显式 ack、幂等键、合理 visibility/AckWait 和 DLQ。
```

## Q045. ack 和相近概念最容易混淆的边界在哪里？

ack 最容易和七类概念混淆：receive、visibility timeout、offset commit、producer ack、nack、业务提交、幂等去重。把这些边界讲清楚，基本就能看出一个人是否真的理解消息队列。

第一，ack 不是 receive。receive 只是消费者拿到了消息，消息进入 in-flight 或者消费者本地 buffer；ack 才是消费者告诉系统“这次投递完成”。SQS receive 后消息仍然留在队列里，只是暂时不可见；如果不 DeleteMessage，timeout 后还会出现。NATS JetStream consumer 也会跟踪 delivery 和 ack，未 ack 会触发 redelivery。

第二，ack 不是 visibility timeout。visibility timeout 是租约期限，ack 是完成确认。timeout 解决“消费者挂了以后什么时候别人可以重试”，ack 解决“消费者成功以后消息是否还需要重试”。timeout 到期不表示业务失败，只表示这次租约没有按时完成；ack 成功才表示队列状态可以前进。

第三，Kafka 的 offset commit 和 SQS/NATS 的逐条 ack 不完全相同。Kafka topic 的消息不会因为某个 consumer commit 而删除；commit 只是 consumer group 的恢复位置。commit 一个较大的 offset 通常隐含前面的 offset 都已完成，所以 Kafka partition 内并发处理时要特别小心，不能因为高 offset 先完成就提交到高 offset，从而跳过低 offset 的失败记录。

第四，consumer ack 不是 producer ack。producer ack 说明 broker 已经接受或复制了生产者发送的消息；consumer ack 说明消费者已经处理完某次投递。一个保证“消息进来了”，一个保证“消息处理完了”。

第五，ack 不是 nack。ack 是成功确认；nack/reject 是失败或拒绝处理的信号，可能触发立即重投、延迟重投或 DLQ。NATS 的 Backoff 对 ack timeout 生效，但普通 nak 默认可能立即 redeliver，除非使用带延迟的 nak。SQS 没有传统 AMQP 意义上的 nack，常见做法是让 visibility timeout 到期，或者把 visibility timeout 改成 0 让消息尽快可见。

第六，ack 不是业务提交。数据库 commit、HTTP 200、文件写入成功和 broker ack 是不同系统里的状态。两个系统之间没有事务时，永远存在“业务成功但 ack 失败”和“ack 成功但业务没成功”的窗口。正确设计不是假装窗口不存在，而是通过幂等、去重、状态机、outbox/inbox 或事务性消息把窗口变得可恢复。

第七，ack 不是幂等。ack 可以减少重复，但不能消除所有重复；幂等是消费者面对重复时保证结果不出错的能力。SQS 标准队列要求应用能处理同一消息多次出现；Kafka consumer 重启后也可能从已处理但未提交的 offset 重新消费；NATS AckWait 到期也会重投。ack 和幂等是互补关系，不是替代关系。

面试里可以这样回答：

```text
ack 的边界最容易和 receive、visibility timeout、offset commit、producer ack、nack、业务事务和幂等混淆。receive 是拿到消息，ack 是确认完成；visibility timeout 是租约，ack 是完成；Kafka commit 是 consumer group 进度，不是删除消息；producer ack 保证写入 broker，consumer ack 保证处理完成；nack 表示失败路径；业务 commit 和 ack 分属两个系统，必须处理不一致窗口；幂等是重复发生后的保护，不能被 ack 取代。
```

## Q046. ack 在高并发场景下可能出现哪些隐藏问题？

高并发下，ack 的问题通常不是“能不能发一个确认包”，而是确认顺序、批量边界、旧租约、协调器压力和状态竞争。低并发测试里很少暴露这些问题，因为一个 worker 顺序处理时，ack 的先后和业务完成的先后大体一致；一旦并发处理，完成顺序和消息顺序就会分离。

第一类问题是乱序完成导致进度错误。Kafka partition 是有序日志，如果同一 partition 内并发处理 offset 10、11、12，12 先完成并直接 commit 到 13，而 10 失败了，那么消费者重启后可能从 13 开始，10 和 11 被逻辑跳过。正确做法是按 partition 维护连续完成水位线，只有低 offset 都完成后才能提交更高 offset，或者避免同一 partition 内无序并发。

第二类问题是 ack storm。大量 worker 同时完成任务，会集中调用 DeleteMessage、ack 或 commit。SQS 这里会表现为 API TPS、网络往返和成本上升；Kafka 会压到 group coordinator 和 offset commit 路径；NATS JetStream 会更新 consumer ack state，持久 durable consumer 还涉及状态存储和复制。批量确认能缓解，但批量边界一旦设计不好，又会放大失败窗口。

第三类问题是旧 ack 和新投递竞争。消费者 A 超时后消息被投递给 B，A 恢复后发 ack。如果系统没有 receipt handle、delivery sequence、generation 或 owner fencing，A 的 ack 可能影响 B 的当前处理。SQS 用每次 receive 的 receipt handle 缩小这个风险；自研系统必须做类似的 current_delivery_id 校验。NATS 在超时和重新投递场景下也不能把 ack 当作业务互斥，应用侧仍要保持幂等和 fencing。

第四类问题是批量 ack 的部分失败。SQS 批量 DeleteMessage 最多一批 10 条，单批里可能部分成功、部分失败；Kafka commitAsync 也可能因为重试和回调顺序导致旧 commit 干扰新判断；NATS 批量拉取后如果应用只记录“这一批成功”，中间某条失败就会很难恢复。高并发系统必须记录每条消息或每个 offset 的完成状态，而不是只记录 batch 成功。

第五类问题是 ack 与 backpressure 互相影响。NATS 的 MaxAckPending 达到上限会暂停投递；SQS in-flight 接近上限时 receive 会拿不到新消息；Kafka poll 后处理太慢会触发 `max.poll.interval.ms` 相关的 rebalance 风险。ack 慢会让系统误判消费者还在处理大量消息，最终形成吞吐下降、重复上升、延迟变长的组合问题。

第六类问题是共享数据结构锁竞争。自研队列里，ack 往往要更新 message state、attempt、group cursor、DLQ 计数、metrics 和持久化日志。如果所有 ack 都争一个全局锁，吞吐会在高并发下突然塌。更合理的设计是按 queue/partition/group 分片，使用追加日志或细粒度锁，把 metrics 异步化，并让 ack path 尽量短。

面试里可以这样回答：

```text
高并发下 ack 的隐藏问题包括：Kafka partition 内乱序完成导致提交高 offset 跳过低 offset；大量 worker 同时 ack 造成 API、网络、coordinator 或 consumer state 写入压力；旧租约 ack 和新投递竞争，需要 receipt_handle/generation fencing；批量 ack 部分失败导致状态不清；ack 慢触发 MaxAckPending、in-flight 上限或 rebalance；自研队列还会遇到 ack path 全局锁和状态表热点。解决方向是按 partition 维护连续提交水位线、批量但可追踪、ack token 校验、幂等处理、限流和分片化状态。
```

## Q047. ack 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

ack 的边界条件集中在几个窗口：处理前崩溃、处理后 ack 前崩溃、ack 后业务提交前崩溃、ack 请求或响应丢失、timeout 后旧消费者恢复、消费者重启后本地状态丢失。这些窗口不能靠“代码里 try/catch 一下”解决，必须从语义上承认 at-least-once 和不确定结果。

典型窗口如下：

```text
receive 后、处理前崩溃：消息应在 timeout 后 redeliver，不能丢。
业务处理成功后、ack 前崩溃：消息会重投，业务必须幂等。
ack 成功后、业务提交前崩溃：消息系统认为完成，但业务结果丢，这是最危险的顺序。
ack 请求发出后网络超时：消费者不知道 broker 是否收到，重试 ack 要幂等或可识别。
ack 响应丢失：broker 已完成，消费者以为失败，可能重复发送确认。
timeout 到期后旧消费者继续运行：旧 ack 或旧业务写入必须被 fencing 或幂等挡住。
重启后本地 dedup cache 丢失：重复消息可能绕过只存在内存里的去重。
```

SQS 的 DeleteMessage 有两个实际边界：第一，每次 receive 都会得到新的 receipt handle，删除时应该使用最近一次的 handle；使用旧 handle 时，请求可能成功返回，但消息不一定被删除。第二，标准队列在少数情况下，即使 DeleteMessage 成功，之后仍可能再次收到同一消息，因为分布式副本状态可能不完全一致。这个设计直接说明，消费者必须幂等，不能把 DeleteMessage 成功当作业务层“永不再见”。

Kafka 的边界主要是 commit 时机。自动提交可能在业务处理完成前推进 offset；手动提交如果放在业务提交前，也会导致跳过；放在业务提交后，则崩溃时会重复。commitSync 能让调用方知道提交结果，但会阻塞；commitAsync 吞吐更好，但要处理回调顺序和失败重试，避免旧提交干扰新进度。rebalance 时还要在 partition revoked 前提交已完成进度，否则新 consumer 可能重复处理。

NATS JetStream 的边界主要是 AckWait、MaxDeliver、Backoff 和 durable consumer state。AckWait 到期后未确认消息会 redeliver；MaxDeliver 达到后不会无限投递，而是进入失败处理路径或发 advisory；Backoff 会影响 timeout redelivery 节奏。消费者重启后，如果 durable consumer 状态还在，可以继续从未 ack 的位置恢复；如果是 ephemeral 且状态清理了，语义就不同。

超时和重试还会暴露“业务执行时间不稳定”的问题。任务 p50 只要 1 秒，但 p99 偶尔 2 分钟，如果 visibility/AckWait 只按平均值设置，就会出现少量长尾任务被重复处理。反过来，如果按最大值设置成 30 分钟，消费者崩溃后恢复又太慢。比较稳妥的做法是短初始 timeout 加 heartbeat 续租，并设置最大租约、最大投递次数和 DLQ。

面试里可以这样回答：

```text
ack 在故障场景下会暴露几个经典窗口：receive 后崩溃要靠 timeout 重投；业务成功后 ack 前崩溃会重复；ack 后业务提交前崩溃会造成应用层丢结果；ack 请求或响应丢失会让结果未知；timeout 后旧消费者恢复会产生旧 ack 和旧写入；重启后内存去重丢失会让重复穿透。SQS 还要求使用最近一次 receipt handle，DeleteMessage 成功后标准队列仍要求应用能处理重复；Kafka 要小心 commit 时机和 rebalance；NATS 要关注 AckWait、MaxDeliver 和 Backoff。核心解法是幂等、正确提交顺序、fencing、持久去重和 DLQ。
```

## Q048. ack 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

ack 的瓶颈要按系统拆开看，但大多数线上场景里，第一瓶颈不是 CPU，而是网络往返、持久化 I/O、协调器或状态更新。ack 本身序列化的数据很小，CPU 通常不是主要问题；真正贵的是“确认之后系统要更新哪些共享状态，以及这个更新要不要复制、落盘、走远程 API”。

SQS 场景下，ack 等价于 DeleteMessage 或 DeleteMessageBatch。瓶颈主要是网络/API TPS、区域服务延迟、批量大小和 in-flight 管理。单条 delete 每条消息一次 API 调用，吞吐上去后成本和 TPS 压力都明显；批量 delete 可以降低调用数，但要处理部分失败。频繁 ChangeMessageVisibility 也会增加 API 调用。SQS FIFO 还要考虑每个 MessageGroupId 的串行约束，单个 hot group 会限制并行度。

Kafka 场景下，瓶颈常在 offset commit 的协调路径。consumer commit 需要和 group coordinator 交互，过于频繁的 commit 会增加 coordinator、网络和 broker I/O 压力。`commitSync` 还会阻塞消费线程，增加端到端延迟；`commitAsync` 能降低阻塞，但要处理失败和顺序。很多 Kafka consumer 的性能优化不是“每条都 commit”，而是按时间、条数或 partition 水位线批量 commit，在重复窗口和吞吐之间取平衡。

NATS JetStream 场景下，ack 会更新 consumer 的 delivery/ack 状态。durable consumer 的状态需要保存，replicated stream/consumer 还会涉及复制成本；MaxAckPending 达到上限后会暂停投递，表现出来像吞吐突然下降。NATS 的 consumer 配置也提供内存存储选项，用于减少 consumer state 的文件存储开销，这说明 ack path 的存储介质会影响性能。

自研队列里，瓶颈通常按这个顺序排查：

```text
网络：ack 是否每条远程调用，是否可以 batch，p99 RTT 是否变差。
I/O：ack 是否同步写 WAL/数据库，fsync 或 quorum 写是否过频。
锁竞争：是否所有 ack 共用一个全局 mutex，是否有 group cursor 热点。
下游状态：ack 是否同步更新多个索引、metrics、DLQ 计数和审计表。
内存：in-flight map、timer heap、dedup cache 是否过大，GC 是否影响超时。
CPU：序列化、压缩、校验通常不是首要瓶颈，但高 QPS 下仍要看 profiler。
```

优化时不要只降低 ack 频率。ack 太少会让重复窗口变大，崩溃后回放更多消息；ack 太多会让吞吐下降。比较好的策略是按业务风险设置提交粒度：低风险日志可以批量大一点，支付和库存要更保守；Kafka 按 partition 连续水位线 commit；SQS 用 DeleteMessageBatch 但记录部分失败；NATS 调整 AckWait、MaxAckPending、pull batch 和 consumer storage。

面试里可以这样回答：

```text
ack 的瓶颈通常不是 CPU，而是网络、I/O、协调器和锁竞争。SQS 的 DeleteMessage 是远程 API，单条 delete 会带来 TPS、成本和 p99 延迟压力，批量能缓解但要处理部分失败。Kafka offset commit 会打到 group coordinator，过频 commit 会增加网络和 broker 压力，commitSync 还会阻塞。NATS JetStream ack 会更新 consumer state，durable 和复制会带来存储成本，MaxAckPending 会形成流控。自研队列要重点看 WAL/DB 写、全局锁、in-flight 索引和 timer heap。优化要在重复窗口、吞吐、成本和恢复时间之间平衡。
```

## Q049. ack 的 correctness test、stress test 和 benchmark 应该分别测什么？

ack 的测试也要分层。correctness test 证明语义没有错，stress test 暴露并发和故障组合，benchmark 才衡量性能。只跑 benchmark 不能证明 ack 正确，因为一个系统可能每秒确认十万条，但在“业务成功后 ack 前崩溃”这个窗口里仍然会重复；也可能在并发 commit 时跳过未完成 offset。

correctness test 要围绕状态转移和故障窗口写：

```text
处理成功后 ack，消息不再正常投递。
处理失败不 ack，timeout/AckWait 后会 redeliver。
ack 使用错误 receipt_handle 或旧 delivery_id 时不能删除当前投递。
ack 重复发送应幂等，不能产生负计数或错误状态。
ack 成功但响应丢失时，消费者重试不会破坏状态。
业务成功后 ack 前崩溃，重启后消息会重投，幂等表挡住重复副作用。
ack 后业务前崩溃应被测试标记为危险顺序。
Kafka 同一 partition 并发处理时，只能提交连续完成水位线。
rebalance/restart 后从最后安全提交点恢复。
DLQ 或 MaxDeliver 到达后不再无限重投。
```

这些测试最好带故障注入，而不是只 mock 成功路径。比如在写业务表后、ack 前 kill worker；在 ack 发出后丢弃响应；在 timeout 到期前后让两个消费者竞争同一消息；让旧 delivery_id 的 ack 在新投递之后到达；让 Kafka consumer 在 partition revoked 前后崩溃。断言不能只看日志，要看业务表、dedup 表、消息状态、committed offset、receive count、DLQ 记录。

stress test 要把并发和异常叠起来：

```text
上千 worker 同时 receive 和 ack。
批量 ack 部分失败。
大量消息同时 timeout 后 redelivery。
下游慢导致 ack 延迟和 MaxAckPending/in-flight 增长。
Kafka consumer group 频繁 rebalance。
网络抖动、API 限流、broker failover。
hot partition、hot MessageGroupId、hot queue group。
DLQ 大量写入和 redrive 回灌。
消费者批量重启后的重复风暴。
```

stress test 的指标要包括 lost count、duplicate count、stale ack count、ack error rate、redelivery rate、DLQ rate、in-flight/MaxAckPending、consumer lag、oldest age、rebalance 次数、业务幂等冲突数、恢复时间。这里的目标不是拿漂亮吞吐，而是确认系统被打到边界时，失败模式可解释、可限流、可恢复。

benchmark 则要固定变量，测 ack path 的成本曲线：

```text
单条 ack vs batch ack 的吞吐和 p99。
不同 commit interval、commit batch size 下的 Kafka lag 和重复窗口。
不同 AckWait、MaxAckPending、pull batch 下的 NATS 吞吐和 redelivery。
不同 DeleteMessageBatch 大小、long polling、visibility 续租频率下的 SQS API 调用数和成本。
ack path CPU、内存、锁等待、WAL/DB 写延迟、网络 RTT。
每成功处理一条消息的平均 API 调用数和平均确认延迟。
```

如果是 LogServe 这类自研日志驱动系统，我还会加几类专项测试：ack 状态是否能通过 log replay 恢复；worker kill 后 in-flight 是否按期重新可见；旧 worker 的 ack 是否被 generation 拦住；materialized view 更新是否和 ack 顺序一致；高并发 ack 是否会把同一个 key 的锁打爆。因为这类系统的关键不是“能不能调用一个 ack API”，而是 ack 事件进入日志后，所有派生状态能不能一致恢复。

面试里可以这样回答：

```text
correctness test 测语义和故障窗口：成功 ack 后不再投递，失败未 ack 会重投，旧 receipt_handle 不能删除新投递，重复 ack 幂等，业务成功后 ack 前崩溃会重投且幂等生效，Kafka 只提交连续完成 offset，rebalance 后从安全点恢复，MaxDeliver 后进入 DLQ。stress test 测大量 worker、ack storm、批量部分失败、timeout 风暴、下游慢、broker failover、API 限流、hot group、DLQ 回灌和批量重启。benchmark 测 ack path 成本：吞吐、p99、batch 效率、commit interval、API 调用数、网络 RTT、WAL/DB 延迟、锁等待、CPU/内存和每条成功消息的成本。三类测试目的不同，不能互相替代。
```

## Q050. 如果要求从零实现一个简化版 ack，你会先定义哪些不变量？

我会先把 ack 定义成一次投递的完成确认，而不是简单理解成“客户端回一个 OK”。这个区别很重要。消息系统真正关心的不是消费者有没有收到消息，而是这次投递对应的业务处理是否已经完成，完成以后队列状态、消费进度、in-flight 计数、顺序约束和重试策略应该怎么变化。

先定义最小状态机：

```text
Ready: 消息可以被投递。
InFlight: 消息已经投递给某个消费者，等待 ack/nack/timeout。
Acked/Deleted: 当前消息或当前投递已经完成，不再正常投递。
Failed/DeadLettered: 达到最大失败次数，进入失败处理路径。
```

然后定义 ack 的不变量：

```text
1. ack 不是按 message_id 随便确认，而是确认一次具体 delivery。
2. 每次 delivery 都要有 delivery_id、receipt_handle、consumer generation 或类似 token。
3. ack 只能作用于当前有效 delivery；旧 delivery 的 ack 不能删除或推进新 delivery。
4. ack 的状态变化必须原子：减少 in-flight、标记完成、释放 group 顺序、更新指标不能只做一半。
5. ack 成功返回前，服务端必须先把完成状态持久化或复制到足够安全的位置。
6. 同一个 delivery 的重复 ack 应该幂等，不能把计数减成负数，也不能重复释放后续消息。
7. ack 失败或结果未知时，客户端必须能安全重试，系统不能因为重试 ack 破坏状态。
8. 未 ack 的 delivery 到期后可以 redeliver；已经 ack 的 delivery 不应再因为普通 timeout 进入重试。
9. 如果有最大投递次数，ack 成功会结束重试计数；未 ack 或 nack 才会继续推进 attempt。
10. 如果有 FIFO/group 语义，ack 或 timeout 是释放后续消息的边界。
11. 如果是日志型消费，commit 高 offset 隐含低 offset 已完成，提交水位线必须单调。
12. ack/delete/commit 必须鉴权，不能让非当前租户或非授权消费者推进别人的队列状态。
```

SQS 的实现能很好说明为什么要区分 message_id 和 delivery token。`DeleteMessage` 要求传 `ReceiptHandle`，而且同一条消息每次 receive 都会得到不同 handle；用旧 handle 时，请求可能成功返回，但消息未必真的被删除。这个语义提醒我们：ack 必须绑定具体投递，而不是只绑定消息本身。否则一个超时后恢复的旧 worker 可能误删后来 worker 正在处理的消息。

NATS JetStream 的抽象稍微不同。consumer 是 stream 的有状态视图，它跟踪消息是否被 delivery、是否被 ack。`AckExplicit` 要求逐条确认，`AckNone` 相当于投递即认为确认，`AckAll` 会确认一段 pending 消息。这里的不变量要把 ack policy 写进状态机：如果选择 `AckAll`，确认最后一条消息会隐式确认前面的消息；如果业务不能接受这个隐含批量边界，就不应该使用它。`AckWait` 和 `MaxDeliver` 也要进入模型，未按时 ack 会触发 redelivery，达到最大投递次数后不能无限循环。

Kafka 经典 consumer 又是另一类模型。它没有 SQS 那种“删除单条消息”的 ack；topic 仍按 retention 保留，consumer group 提交的是 offset。实现简化版 Kafka ack 时，我会把不变量写成“每个 group/topic/partition 有一个 committed offset，表示小于该 offset 的记录都被认为处理完成”。这意味着 commit offset 101 不是只确认 offset 100，而是确认 100 及之前的连续前缀已经完成。如果同一 partition 内并发处理，必须维护连续完成水位线，不能因为 offset 100 先完成就跳过 offset 98、99。

如果从零实现，我会先落一张状态表或一段追加日志：

```text
message_id
stream_position 或 queue_position
delivery_id / receipt_handle
consumer_id / generation
state
attempt
visible_at / ack_deadline
acked_at
committed_offset
last_error
```

receive/poll 生成 delivery，ack/delete/commit 消费 delivery。服务端处理 ack 时做条件更新：`state=InFlight and current_delivery_id=request.delivery_id` 才能成功。Kafka 风格则做 `committed_offset = max(committed_offset, next_contiguous_completed_offset)`，但这个 max 不能绕过未完成的洞。状态写入成功后再回包；如果回包丢了，客户端重试 ack 应该得到“已经完成”或“旧 delivery 已无效”的可解释结果。

还要提前定义 ack 与业务提交的边界。消息系统只能知道 ack 是否成功，不知道数据库写入是否成功。消费者最常见的正确顺序是先提交业务结果，再 ack。这样崩溃时最多重复，靠幂等挡住。反过来，先 ack 再写业务，进程一崩就会把任务从消息系统视角彻底完成，应用层结果却丢了。实现 ack 不变量时不能承诺端到端 exactly once，只能提供一个可靠、可重试、可 fencing 的完成信号。

面试里可以这样回答：

```text
我会先把 ack 定义成“某一次投递的完成确认”。核心不变量是：每次 delivery 有独立的 delivery_id/receipt_handle/generation；ack 只能确认当前有效 delivery；旧 ack 不能删除新投递；重复 ack 要幂等；ack 成功前服务端要先持久化完成状态；ack 会原子地减少 in-flight、释放顺序约束、停止普通 redelivery；未 ack 到期才重投；FIFO/group 要以 ack 或 timeout 释放后续消息。Kafka 这种日志模型要改成 per group/topic/partition 的 committed offset，而且提交高 offset 隐含低 offset 已处理，所以必须维护连续完成水位线。ack 本身不是业务事务，业务通常先提交结果再 ack，重复靠幂等处理。
```

## Q051. ack 的常见误用是什么，误用后通常会产生什么线上症状？

ack 最危险的误用，是把“收到消息”当成“处理完成”。很多线上丢数据问题不是队列把消息弄丢了，而是消费者太早 ack、自动提交太早、批量确认边界太粗，导致消息系统认为任务完成了，业务结果却还没落地。

常见误用可以按系统分开看：

```text
SQS: receive 后立刻 DeleteMessage，或者用 message_id/旧 receipt handle 理解删除语义。
SQS: DeleteMessageBatch 返回 HTTP 200 就当全部成功，不检查 Failed 项。
NATS JetStream: 业务还没提交就 ack，或者误用 AckNone/AckAll。
NATS JetStream: AckWait 配得太短，消费者还在处理，消息已经 redeliver。
Kafka: 开启 auto commit，但处理逻辑慢，offset 在业务完成前推进。
Kafka: 同一 partition 并发处理，却按最高完成 offset commit，跳过低 offset。
通用: 把 ack 当 exactly-once 保证，不做幂等。
通用: 失败也 ack，导致毒丸消息被吞掉，后续无法分析。
通用: 永远不 ack 失败消息，也不设置 DLQ，导致无限重试。
通用: ack 太频繁或太慢，分别造成性能压力或大量 in-flight/lag。
```

太早 ack 的症状通常是“队列看起来很健康，业务却缺数据”。SQS 里 visible messages 和 in-flight 都不高，DeleteMessage 成功率也正常，但订单没更新、索引缺文档、邮件没发出。Kafka 里 lag 看起来下降很快，重启后也不会补消费，但业务表里少记录。NATS 里如果用了 `AckNone` 或过早 ack，服务端不会再 redeliver，问题会落到业务系统里才显现。

太晚 ack 或忘记 ack 的症状是另一组：消息重复投递、receive count 增长、NATS redelivery 增多、SQS in-flight 接近上限、Kafka lag 不下降、消费者日志里反复处理同一任务。用户侧看到的是请求迟迟完成不了、异步任务卡住、通知重复、库存重复扣减或状态来回跳。这个时候单纯扩容 consumer 有时没用，因为真正的问题是 ack 边界或幂等设计错了。

忽略批量 ack 的部分失败也很常见。SQS `DeleteMessageBatch` 最多删除 10 条，但官方 API 明确说即使 HTTP 200，批次内也可能有成功和失败的混合结果。应用如果只看 HTTP 状态，不看每条 entry 的结果，就会以为一批都删了，失败的那几条后面又重新出现，最后变成“偶发重复”。这种重复很难排查，因为请求日志里会显示批量 delete 返回了成功。

Kafka 的误用更容易表现成“偶发跳过”。比如一个 partition 内并发处理 offset 100、101、102，102 先完成后提交到 103，100 失败了。重启后 consumer 从 103 开始，100 不会再被这个 group 正常消费。这个问题和 SQS 重复不同，它更像静默数据缺口。正确做法是按 partition 维护连续完成前缀，或者不在同一 partition 内乱序处理。

还有一种误用是把 ack 当业务错误处理。比如反序列化失败、依赖返回 400、数据违反约束时，代码为了“不阻塞队列”直接 ack 丢掉。短期看队列清了，长期看 DLQ 没有证据，用户数据丢了，排查时只能从业务日志里拼。更稳妥的做法是区分可重试错误和不可重试错误：可重试的不 ack 或延迟重试，不可重试的写失败表或 DLQ 后再确认，保证问题可见。

面试里可以这样回答：

```text
ack 的常见误用是太早确认、确认错对象、忽略批量部分失败、把 ack 当 exactly-once、失败也直接 ack、或者长期不 ack 又没有 DLQ。SQS 里典型错误是 receive 后立刻 DeleteMessage、用旧 receipt handle、DeleteMessageBatch 只看 HTTP 200；NATS 里是误用 AckNone/AckAll 或 AckWait 配得太短；Kafka 里是 auto commit 提前推进 offset，或者并发处理时提交高 offset 跳过低 offset。线上症状要么是队列很干净但业务缺数据，要么是重复处理、in-flight 打满、lag 不降、DLQ 暴涨、用户看到重复通知或任务卡住。
```

## Q052. ack 在单机和分布式环境中的语义有什么差异？

单机环境里，ack 通常是一个本地状态转移：消费者线程处理完消息，拿锁把 `InFlight` 改成 `Acked`，从 in-flight map 删除，必要时推进本地 offset 或释放 group 队首。只要进程不崩溃，这个语义比较直观；ack 调用返回了，就说明内存状态已经更新。甚至可以做到很强的局部互斥，因为所有状态都在一个进程里。

但单机语义有一个明显短板：崩溃恢复。如果 ack 状态只在内存里，进程在返回 ack 后、刷盘前崩溃，重启后可能重新投递已经确认的消息；如果进程在业务成功后、ack 状态写入前崩溃，也会重复。要让单机 ack 有崩溃语义，至少需要 WAL、事务数据库或 checkpoint。否则它只能说“进程活着的时候正确”，不能说“机器重启后正确”。

分布式环境里，ack 是远程请求，也是复制状态更新。调用方看到的结果不再只有成功和失败，还有“请求到没到服务端不知道”“服务端成功了但响应丢了”“旧 leader 接收了请求但已经失去所有权”“ack 到达时消息已经超时重投给别人”。这些情况会让 ack 从一个简单函数调用变成租约、任期、复制和幂等问题。

差异可以这样拆：

```text
单机: ack 是本地内存或本地磁盘状态变化。
分布式: ack 是 RPC，可能丢、重试、乱序、延迟到达。
单机: 用 mutex 就能保护大部分并发状态。
分布式: 需要 receipt_handle、generation、leader epoch、CAS 或事务。
单机: ack 返回值通常能直接代表本地状态。
分布式: ack 结果未知时，客户端必须能安全重试。
单机: 只有本地崩溃恢复问题。
分布式: 还要处理 broker failover、复制延迟、网络分区、跨副本一致性。
单机: 指标可以较精确。
分布式: visible、in-flight、lag、ack rate 可能是延迟或近似统计。
```

SQS 是一个很典型的分布式语义。`DeleteMessage` 成功返回 HTTP 200，应用仍然要能处理后续再次收到同一消息的情况；官方文档解释过，标准队列在少数情况下可能因为某个存储副本不可用而再次返回已删除消息。它还要求删除使用最新 receipt handle。也就是说，分布式 ack 成功并不等于“业务层绝对不会再见到这条消息”。它只说明服务端接受了这次删除请求，应用仍要幂等。

NATS JetStream 的差异在 consumer state。consumer 会跟踪 delivery 和 ack，durable consumer 的状态可以恢复；但 ack policy、AckWait、MaxDeliver、Backoff、MaxAckPending 都会影响分布式行为。文档还提醒，在某些队列场景里，超出 ack 窗口的旧 ack 仍可能被考虑。这个边界说明：ack 不能单独作为业务互斥，应用如果担心旧 worker 写入，仍要在业务存储里做 fencing。

Kafka 的分布式语义落在 group coordinator 和 partition ownership 上。consumer 的 position 会在 poll 时前进，但 committed offset 是另一个概念；commit 写入的是 consumer group 的恢复位置。rebalance 时 partition 会转移给别的 consumer，如果旧 consumer 还在处理或提交，就要靠 generation、coordinator 和 rebalance listener 控制边界。单机队列里“我处理完就 ack”很直接，Kafka 里则要问：哪个 group、哪个 partition、提交到哪个 offset、这个 offset 之前是否真的都完成了。

所以单机和分布式最大的差异，不是 API 名字，而是 ack 能不能被当作强事实。单机里，在锁和本地持久化正确的前提下，ack 可以比较接近确定事实；分布式里，ack 更像一个可重试、可重复、可能迟到的状态更新请求。系统要靠幂等 ack、delivery token、持久化、复制、generation fencing 和业务幂等来收敛到正确结果。

面试里可以这样回答：

```text
单机里 ack 主要是本地状态转移，用锁把 InFlight 改成 Acked，释放 in-flight 和顺序约束；难点是崩溃恢复，所以需要 WAL 或事务存储。分布式里 ack 是 RPC 和复制状态更新，可能丢失、重试、乱序、迟到，也可能遇到 leader 切换和网络分区，因此必须有 receipt_handle/generation/leader epoch 这类 token，ack 还要幂等。SQS 即使 DeleteMessage 成功也要求应用能处理重复；NATS JetStream 要关注 durable consumer state、AckWait 和旧 ack；Kafka commit 是 group/partition 的恢复位置，不是删除消息。分布式环境下 ack 不能替代业务幂等和 fencing。
```

## Q053. redelivery 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

redelivery 的核心目标是：当一次投递没有被确认完成时，让消息重新获得被处理的机会，避免因为消费者崩溃、超时、网络中断或临时错误而静默丢任务。它首先解决的是正确性问题，具体说是 at-least-once 语义下的“未确认不丢”。性能、可维护性都会被它影响，但不是它的第一目标。安全性通常也不是 redelivery 的核心目标，除非你把租户隔离、权限校验、DLQ 访问策略也放进讨论。

SQS 的语义很直接：消费者 receive 后，消息仍然在队列里，只是在 visibility timeout 内暂时不可见；如果消费者没有在 timeout 之前 DeleteMessage，消息会重新可见，之后可以被同一个或另一个消费者拿到。NATS JetStream 也是类似方向：consumer 跟踪 delivery 和 acknowledgment，如果消息没有 ack，或者被 nak，系统会尝试重新投递；AckWait、MaxDeliver、Backoff 共同决定什么时候重投、最多投几次。Kafka 经典 consumer 不叫 redelivery，但故障后的效果相似：进程重启或分区转移后，新的 consumer 从 committed offset 恢复，之前处理过但没有提交 offset 的记录会被再次读取。

所以 redelivery 解决的不是“让系统更快”，而是“失败后还有机会完成”。它的正确性边界一般包括：

```text
消息已经投递但未 ack，不能永久消失。
消费者崩溃后，消息能在合理时间内重新进入可处理状态。
临时错误可以重试，不需要生产者重新发送。
重复投递是允许的，消费者必须幂等。
达到最大投递次数后，消息要进入 DLQ 或失败路径，而不是无限打爆下游。
```

性能层面，redelivery 是一把双刃剑。它能缩短故障恢复时间，也可能制造重复风暴。timeout 太短，慢任务会被提前重投，两个消费者同时处理同一条消息；timeout 太长，消费者崩溃后恢复慢，用户等待时间拉长。Backoff 太激进，会把临时下游故障放大成队列风暴；Backoff 太保守，恢复又慢。redelivery 的性能问题通常来自策略参数，而不是来自概念本身。

可维护性方面，redelivery 的价值在于让失败路径显式化。没有 redelivery，消费者挂了以后任务可能就没了；有 redelivery 但没有 attempt、last_error、DLQ、trace，系统会变成“任务一直重复，但没人知道为什么”。比较好的设计会把 receive count、redelivery reason、next visible time、last consumer、last error、DLQ reason 都记录下来。这样线上排查时能分清是业务错误、timeout 配置错误、下游慢，还是消费者部署出了问题。

安全性不是 redelivery 的主要目标，但 redelivery 不能绕过安全边界。多租户队列里，一个租户的失败消息不应该被另一个租户消费；DLQ redrive 也要有权限控制；包含敏感数据的消息在重复投递、DLQ、日志和 trace 中都要遵守同样的数据保护规则。也就是说，redelivery 本身不提供安全性，但它会扩大失败消息的传播路径，所以安全边界要跟着覆盖。

面试里可以这样回答：

```text
redelivery 的核心目标是保证未确认的消息不会因为消费者崩溃、超时、网络中断或临时错误而静默丢失。它主要解决正确性问题，也就是 at-least-once 下的故障恢复；性能和可维护性是派生影响。SQS 中未 DeleteMessage 的消息在 visibility timeout 后重新可见；NATS JetStream 中未 ack 或 nak 的消息会按 AckWait、MaxDeliver、Backoff 重投；Kafka 经典 consumer 则通过 committed offset 恢复，未提交的记录可能被再次读取。redelivery 不保证 exactly once，反而要求消费者幂等，并且需要 DLQ、attempt、last_error 和 backoff 防止重复风暴。
```

## Q054. redelivery 的典型适用场景和不适用场景分别是什么？

redelivery 适合处理“失败可能是暂时的，而且重复处理可以被接受或被幂等挡住”的异步任务。典型例子包括订单异步状态推进、搜索索引更新、邮件或短信发送、Webhook 投递、图片转码、数据同步、缓存刷新、物化视图更新、报表生成。共同点是：消费者可能崩溃，下游可能短暂不可用，任务失败后应该再试一次，而不是让生产者重新构造消息。

一个比较健康的 redelivery 使用方式通常长这样：

```text
消息带稳定 idempotency key。
消费者先做幂等检查，再执行业务。
可重试错误不 ack，让系统按 timeout/backoff 重投。
不可重试错误写失败表或 DLQ，再停止普通重投。
每次投递记录 attempt、last_error、next_retry_at。
超过 maxReceiveCount/MaxDeliver 后进入 DLQ 或人工处理。
```

SQS 里，这通常对应 visibility timeout、ChangeMessageVisibility、maxReceiveCount 和 DLQ。NATS JetStream 里，对应 AckWait、nak/nakWithDelay、MaxDeliver、Backoff 和 advisory。Kafka 里，则更像“处理失败时不提交 offset，重启或重新分配后从上次 committed offset 继续读”。Kafka 如果要做延迟重试，通常会引入 retry topic、delay topic 或应用层调度，而不是指望 broker 对单条记录做 SQS 式 visibility timeout。

不适用场景也很重要。第一类是不可幂等且没有补偿机制的副作用，比如重复扣款、重复发货、重复创建外部不可撤销资源。不是说这些业务不能用队列，而是不能裸用 redelivery。必须先有幂等键、业务状态机、唯一约束、外部请求幂等参数或补偿流程。

第二类是永久性错误。比如消息 schema 已经不兼容、必填字段缺失、业务规则明确拒绝、目标用户不存在。继续 redelivery 只会浪费资源。正确做法是把错误分类：临时错误重试，永久错误进入 DLQ 或失败表，并保留原始消息和错误原因。

第三类是长时间等待和人工流程。visibility timeout 不是工作流引擎，redelivery 也不是日程系统。任务要等人工审批三天，或者要等外部系统回调，应该拆成状态机、工作流或调度任务，而不是让消息一直 in-flight 或反复 redeliver。SQS visibility timeout 有最大期限，NATS AckWait/Backoff 也不适合承担复杂业务日程。

第四类是纯广播和实时流。很多 telemetry、指标、行情或在线广播场景更关心新鲜度，而不是每条都必须最终处理。强行给每个订阅者做 redelivery，会把系统从 pub/sub 推向持久任务队列，成本、延迟和状态复杂度都会上升。NATS Core 默认更偏 at-most-once，JetStream 才提供持久化和 ack/redelivery；选错模型会让系统很别扭。

面试里可以这样回答：

```text
redelivery 适合暂时性失败和可幂等处理的异步任务，比如索引更新、Webhook、转码、数据同步、邮件发送和物化视图更新。它要求消息有 idempotency key，消费者能区分可重试和不可重试错误，并配置 backoff、maxDeliver 和 DLQ。不适合裸处理不可幂等副作用，不适合永久性坏消息无限重试，不适合长时间人工流程，也不适合只关心新鲜度的实时广播。Kafka 里 redelivery 通常体现为从 committed offset 重新读取；如果要延迟重试，一般用 retry topic 或应用层调度来补。
```

## Q055. redelivery 和相近概念最容易混淆的边界在哪里？

redelivery 最容易和 retry、visibility timeout、ack、nack、replay、DLQ、scheduler 混在一起。它们确实常常一起出现，但边界不一样。把这些概念混成一个词，线上设计就会很难讨论清楚。

第一，redelivery 不是 retry 的全部。retry 是业务或客户端再次尝试某个操作，可能发生在同一个进程里，也可能不经过消息系统；redelivery 是消息系统把未完成的消息再次交给消费者。消费者调用第三方 API 失败后，在本地重试三次，这是 retry；三次都失败后不 ack，让消息过一会儿重新投给消费者，这是 redelivery。两层都可以有，但要避免叠加后次数失控。

第二，redelivery 不是 visibility timeout。visibility timeout 是控制“什么时候重新可见”的租约机制，redelivery 是重新投递这个结果。SQS 里 timeout 到期后消息变得可见，之后 receive 才会形成下一次投递；NATS 里 AckWait 到期会触发 redelivery；Kafka 经典 consumer 没有这种 per-message visibility timeout，它靠 committed offset 恢复消费位置。

第三，redelivery 不是 ack。ack 是告诉系统“这次投递完成了，不需要普通重投”；redelivery 是在没有完成确认时发生。ack 的正确性边界是完成信号，redelivery 的正确性边界是未完成恢复。两者合起来构成 at-least-once：成功就 ack，失败或崩溃就重投。

第四，redelivery 也不等于 nack。nack 是消费者明确表示“这次不处理或处理失败”；redelivery 是系统后续重新投递。NATS 中普通 nak 可能导致立即重投，`nakWithDelay` 才能指定延迟；Backoff 对 ack timeout 生效，但不直接应用到普通 nak。SQS 没有传统 AMQP 里的 nack，常见做法是让 visibility timeout 到期，或者把 visibility timeout 改成 0 让消息更快可见。

第五，redelivery 不是 replay。replay 通常是从日志或 stream 的历史位置重新读一段数据，可能是为了修复 bug、重建索引、跑新消费者或做审计。redelivery 更偏“这条消息之前没确认完成，所以再次交付”。Kafka 的 offset reset、NATS DeliverAll、从历史 stream 重放，都更接近 replay；SQS 的 timeout 后再次 receive，更接近 redelivery。replay 可能处理已成功处理过的历史数据，redelivery 通常围绕未确认或失败的 delivery。

第六，redelivery 不是 DLQ。DLQ 是 redelivery 多次仍失败后的隔离区，不是普通重试通道。消息进入 DLQ 后，系统应该保留诊断信息，等人工或修复程序处理。把 DLQ 当成自动回灌队列，会把毒丸消息重新送回主队列，造成周期性故障。SQS 文档也提醒 FIFO 队列使用 DLQ 可能破坏严格顺序，这说明 DLQ 不是无成本的兜底。

第七，redelivery 不是 scheduler。延迟重投看起来像调度，但它的目标是失败恢复，不是精确定时。需要“明天 9 点执行”的任务，应该用调度系统或工作流引擎；需要“失败后 30 秒再试”的消息，才是 redelivery/backoff 更自然的场景。

面试里可以这样回答：

```text
redelivery 是消息系统把未确认或失败的消息再次交给消费者，不等于所有 retry。本地 retry 是消费者自己重试操作；visibility timeout 或 AckWait 决定什么时候有资格重投；ack 表示完成，redelivery 处理未完成；nack 是失败信号，redelivery 是后续投递结果；replay 是从历史日志重新读，可能包含已成功处理的数据；DLQ 是多次重投失败后的隔离区；scheduler 负责精确定时，redelivery 只适合失败恢复。Kafka 里更常见的是从 committed offset 恢复，而不是 SQS 式单条消息重投。
```

## Q056. redelivery 在高并发场景下可能出现哪些隐藏问题？

高并发下，redelivery 最麻烦的地方不是“消息会再来一次”，而是大量消息会在同一时间再来一次。少量重复可以靠幂等处理，大量重复会变成风暴：消费者、数据库、第三方 API、DLQ、日志和监控一起被打满。很多队列事故表面上是消费者慢，实际是 timeout、backoff、ack 延迟和下游故障叠加后，把 redelivery 放大成了第二波流量。

第一类问题是 redelivery storm。假设下游数据库短暂抖动 30 秒，几万条消息都没有及时 ack；visibility timeout 或 AckWait 一到，它们同时重新可见。新一轮消费者又打向同一个数据库，数据库还没恢复就被再次压住。这个循环会让系统进入“失败产生重试，重试制造更多失败”的状态。解决方向是指数退避、jitter、全局限流、暂停消费、按错误类型决定是否重试，而不是让所有消息按同一个固定时间回流。

第二类问题是重复并发执行。timeout 配短或消费者发生长 GC、线程池阻塞、网络暂停时，原消费者还在处理，消息已经被重新投递给另一个消费者。两个 worker 同时更新同一订单、同一库存、同一索引文档。如果业务没有幂等键、状态版本号或 fencing token，就会出现重复扣减、状态回退、最后写赢覆盖正确结果。redelivery 不是锁，它只能保证失败后还有机会，不保证同一时刻只有一个业务执行体。

第三类问题是 hot key、hot group 和 hot partition。SQS FIFO 的同一 MessageGroupId 前序消息 in-flight 时，后续消息不会正常投递；如果这条消息反复 redeliver，整个 group 会被卡住。Kafka 单个 partition 出现毒丸记录时，如果处理逻辑一直失败且 offset 不推进，后面的记录也会被挡住。NATS JetStream 如果 MaxAckPending 达到上限，投递会被暂停。高并发系统不是总量平均就安全，局部热点足以拖垮用户体验。

第四类问题是 DLQ 和 redrive 反向冲击主队列。大量消息同时超过 maxReceiveCount/MaxDeliver，会集中进入 DLQ；修复代码后，如果一次性把 DLQ 全量回灌，主队列会突然承受远大于正常峰值的流量。更糟的是，修复不完整时，毒丸消息又会回到 DLQ，形成周期性事故。DLQ redrive 应该限速、分批、带采样验证，并保留原始 attempt 和错误信息。

第五类问题是指标误导。只看 queue depth 可能看不出问题，因为很多消息在 in-flight；只看 lag 可能不知道是新消息堆积还是同一批消息反复失败；只看消费者 QPS 可能把重复处理误认为吞吐提升。redelivery 场景下要看 duplicate rate、receive count 分布、redelivery rate、oldest age、in-flight、DLQ rate、业务幂等冲突、下游错误率。没有这些指标，系统会在“看起来很忙”里慢慢坏掉。

第六类问题是公平性。多租户队列里，一个租户的失败消息如果不断 redeliver，会占用消费者线程、in-flight 配额、API 调用和 DLQ 写入能力。其他租户的正常消息排在后面，用户体验被拖慢。这时需要按租户隔离队列、限流、配额、fair queue 或者在消费者侧做调度，不能只用一个全局 FIFO 或一个全局 worker 池。

面试里可以这样回答：

```text
高并发下 redelivery 的隐藏问题主要是放大效应。大量消息同一时间 timeout 会形成 redelivery storm；原 worker 还在处理时消息又投给新 worker，会造成并发重复执行；hot MessageGroupId、hot partition 或毒丸记录会阻塞局部顺序；MaxAckPending、in-flight 上限和 commit 卡住会让吞吐突然下降；DLQ 大量写入和一次性 redrive 会反冲主队列；多租户场景还会出现 noisy neighbor。监控不能只看 queue depth 或 lag，要看 redelivery rate、receive count、duplicate rate、in-flight、oldest age、DLQ rate、幂等冲突和下游错误率。
```

## Q057. redelivery 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

redelivery 的边界条件几乎都发生在“系统不知道上一轮到底完成没有”的窗口里。消费者可能业务已经提交但还没 ack，可能 ack 成功但响应丢了，也可能只是拿到消息后崩溃。队列系统看不到业务数据库里的真实状态，只能根据 ack、timeout、commit offset、delivery state 做判断。这就是 redelivery 必然伴随重复的根本原因。

几个典型窗口值得单独测：

```text
receive 后、业务前崩溃：消息应该重投，不能丢。
业务提交后、ack/delete 前崩溃：消息会重投，消费者幂等必须生效。
ack/delete 后、业务提交前崩溃：消息系统认为完成，业务结果可能丢，这是错误顺序。
ack 请求成功但响应丢失：消费者以为失败，后续可能看到重复或重试 ack。
timeout 到期后旧 worker 恢复：旧 worker 可能和新 worker 并发写业务。
重启后内存去重丢失：重复消息绕过本地 cache。
DLQ redrive 后旧 attempt 信息丢失：排查和限流都会变难。
```

SQS 场景里，最常见边界是 visibility timeout。消费者没有及时 DeleteMessage，消息重新可见；如果原消费者只是慢，不是死，就可能和新消费者并发。标准队列还要求应用能处理重复消息，不能把“没有超时”理解成绝对不会重复。FIFO 队列还有 group 边界：同一 MessageGroupId 的前序消息未删除或未重新可见前，后续消息不会正常交付，这会把单条慢消息放大成整个 group 的延迟。

NATS JetStream 的边界落在 AckWait、Backoff、MaxDeliver 和 durable state。AckWait 到期会 redeliver；Backoff 会覆盖 AckWait 的重投节奏；MaxDeliver 到达后不应该无限重投。durable consumer 可以恢复状态，ephemeral consumer 的状态生命周期更弱。如果消费者重启后换了实例，但业务侧没有持久幂等记录，重复仍会穿透。

Kafka 的边界更像 offset 恢复。consumer 的 position 会随着 poll 前进，但 committed position 才是重启恢复点。处理成功但 offset 未提交，重启后会从旧 committed offset 再读一次；offset 提交早于业务成功，就会跳过消息。rebalance 也会暴露边界：partition revoked 前如果没有提交已完成进度，新 owner 会重复；如果提交了未完成进度，就会丢业务结果。

还有一个容易忽略的边界是时钟和长尾。timeout 或 AckWait 是服务端判断，业务执行时间却受下游、线程池、GC、容器调度影响。p99 任务偶尔超过 timeout，会被系统当成失败重投。解决方法不是无限调大 timeout，而是用 heartbeat/续租、合理最大执行时间、任务拆分和幂等提交，把长尾变成可恢复状态。

面试里可以这样回答：

```text
redelivery 暴露的是“不知道上一轮是否完成”的边界。receive 后崩溃要重投；业务成功后 ack 前崩溃会重复；ack 后业务前崩溃会丢业务结果；ack 响应丢失会让结果未知；timeout 后旧 worker 恢复会和新 worker 并发；重启后内存去重丢失会让重复穿透。SQS 要关注 visibility timeout 和 FIFO group 阻塞；NATS 要关注 AckWait、Backoff、MaxDeliver、durable state；Kafka 要关注 committed offset、rebalance 和提交时机。正确做法是业务先幂等提交，再 ack/commit，并用持久去重、fencing、DLQ 和故障注入测试覆盖这些窗口。
```

## Q058. redelivery 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

redelivery 的瓶颈通常不是 CPU。真正贵的是状态管理、远程调用、下游处理和失败放大。一次 redelivery 需要系统找到可重投消息、更新 attempt、生成新的 delivery token、调整 visible_at 或 ack deadline、写指标，最后把消息重新交给消费者。托管队列里这些动作背后是网络 API 和服务端状态；自研队列里则是索引、定时器、锁和持久化。

SQS 场景下，应用侧最明显的成本是网络/API 和下游 I/O。ReceiveMessage、DeleteMessage、ChangeMessageVisibility 都是远程调用；如果 timeout 太短，重复 receive 和重复业务调用会变多。in-flight 接近上限时，短轮询可能返回 OverLimit，长轮询会表现为拿不到新消息。这里的瓶颈经常被误判成消费者不够多，实际上是未删除消息太多、下游慢或 timeout 配错。

NATS JetStream 场景下，要看 consumer state 和 flow control。MaxAckPending 达到上限后会暂停投递；AckWait 太短会制造额外 redelivery；durable consumer 的 ack/redelivery 状态需要保存，复制因子高时还涉及复制成本。push consumer 还要关注客户端是否慢消费，pull consumer 要看 batch、MaxWaiting、MaxRequestBatch 等参数。高吞吐不是只调大 MaxAckPending，外部服务慢时过大的 pending 只会堆更多未完成任务。

Kafka 场景下，redelivery 常表现为重复读取和重复处理。性能瓶颈不在“broker 重新发送一条消息”这个动作，而在 consumer group rebalance、offset commit、下游幂等写入和重复扫描。一个 poison record 卡住 partition 时，后续记录都不能安全推进；如果应用用 retry topic，瓶颈又会转移到额外 topic 的写入、读取、分区设计和延迟调度。

自研队列里，排查顺序一般是：

```text
网络：重投是否造成大量 receive/delete/change visibility 或 consumer RPC。
I/O：attempt、delivery state、DLQ 写入是否同步落盘或写数据库。
锁竞争：ready queue、in-flight map、timer heap、group cursor 是否有全局锁。
内存：pending delivery、dedup cache、延迟队列和 DLQ buffer 是否膨胀。
下游：业务数据库、对象存储、第三方 API 是否才是真正慢点。
CPU：序列化、压缩、过滤、扫描在高 QPS 下也要看，但通常不是第一瓶颈。
```

优化 redelivery 不能只看吞吐。过快重投会把下游打垮，过慢重投会拉长恢复时间。要把指标放在一起看：成功处理吞吐、重复率、每条成功消息的平均投递次数、p99 端到端延迟、DLQ 进入率、重投延迟、API 调用数、下游错误率。一个看起来 messages/s 很高的系统，如果每条成功消息平均被处理 5 次，实际效率很差。

面试里可以这样回答：

```text
redelivery 的瓶颈通常来自网络、I/O、状态管理、锁竞争和下游，不是 CPU。SQS 里 Receive/Delete/ChangeVisibility 都是远程 API，重复投递会增加调用数、成本和 in-flight 压力；NATS JetStream 要看 consumer state、AckWait、MaxAckPending、durable/replicated state；Kafka 里更多是重复读取、rebalance、offset commit 和下游幂等写入成本。自研系统要重点看 timer heap、in-flight map、ready queue、DLQ 写入、WAL/DB 和全局锁。优化目标不是简单提高 redelivery 速度，而是降低重复率、控制重试节奏、保护下游，并缩短真实恢复时间。
```

## Q059. redelivery 的 correctness test、stress test 和 benchmark 应该分别测什么？

redelivery 的测试要把语义、压力和成本分开。correctness test 证明“该重投时会重投，不该重投时不会破坏业务”；stress test 证明大量失败和并发重投时系统不会失控；benchmark 证明在可控参数下，重投路径的吞吐、延迟和成本曲线可以解释。三者不能互相替代。

correctness test 要围绕状态机和故障窗口写：

```text
receive 后未 ack，timeout/AckWait 到期后会 redeliver。
处理成功并 ack/delete/commit 后，不会因普通 timeout 再重投。
业务成功后 ack 前崩溃，消息会重投，幂等表挡住重复副作用。
timeout 前消息不会被正常重复投递，除非系统语义本来允许 at-least-once 重复。
旧 delivery 的 ack 不能删除或推进新 delivery。
attempt/receive count 单调增加。
达到 maxReceiveCount/MaxDeliver 后进入 DLQ 或失败路径。
FIFO group 前序消息未完成时，后序消息不会越过顺序边界。
Kafka partition 内只提交连续完成 offset，失败 offset 后面的记录不能被静默跳过。
DLQ redrive 后仍保留足够诊断信息，且不会绕过幂等。
```

这些测试要带故障注入。只测正常路径没有意义。可以在这些位置杀进程：receive 后、业务写入前、业务写入后 ack 前、ack 发出后响应前、timeout 刚到期时、DLQ 写入前后。还要模拟网络超时、服务端重启、consumer group rebalance、NATS durable consumer 恢复、SQS visibility timeout 到期。断言不能只看日志，要看业务表、去重表、attempt、DLQ、committed offset、in-flight、oldest age。

stress test 要把失败放大：

```text
大量消息同一时间 timeout。
下游服务 5xx 或限流持续一段时间。
消费者批量重启。
大量旧 worker 恢复并和新 worker 并发。
hot key、hot MessageGroupId、hot partition。
MaxAckPending 或 in-flight 接近上限。
DLQ 大量写入，然后分批 redrive。
多租户中单租户大量失败。
```

stress test 的观察指标包括 redelivery rate、duplicate rate、attempt 分布、DLQ rate、in-flight、MaxAckPending、consumer lag、oldest age、业务幂等冲突、下游 p99、消费者错误率、恢复时间。这里不追求最大吞吐，而是看失败能否被限流、隔离、降级和解释。

benchmark 要固定变量，测不同参数对成本的影响：

```text
不同 visibility timeout/AckWait 下的重复率和恢复时间。
不同 backoff/jitter 下的下游压力。
不同 batch size、pull batch、consumer 数下的吞吐和 p99。
不同 maxReceiveCount/MaxDeliver 下的成功率、DLQ 率和总处理成本。
Kafka 不同 commit interval、retry topic 分区数下的 lag 和重复窗口。
每条成功消息平均投递次数、平均 API 调用数、平均业务写入次数。
```

如果是 LogServe 这类自研日志驱动系统，还要加 replay 相关测试：redelivery 状态是否能从日志恢复；worker 崩溃后 in-flight 是否按期重新可见；旧 worker 的完成事件是否被 generation/fencing 拦住；materialized view 是否能从 ack/redelivery 事件重建；高并发 timeout 扫描是否会拖慢正常 append。因为这类系统的重点不是某个 API 名字，而是共享日志里的状态转移能不能恢复一致。

面试里可以这样回答：

```text
correctness test 测语义：未 ack 到期会重投，ack 后不再普通重投，业务成功 ack 前崩溃会重复且幂等生效，旧 delivery ack 不能影响新 delivery，attempt 单调，超过 maxDeliver 进 DLQ，FIFO/group 和 Kafka offset 边界不被破坏。stress test 测故障放大：大量 timeout、下游限流、消费者批量重启、hot key、MaxAckPending/in-flight 上限、DLQ 写入和 redrive、多租户失败倾斜。benchmark 测成本曲线：timeout/AckWait、backoff、batch、consumer 数、commit interval、maxDeliver 对吞吐、p99、重复率、恢复时间、API 调用数和每条成功消息成本的影响。
```

## Q060. 如果要求从零实现一个简化版 redelivery，你会先定义哪些不变量？

从零实现 redelivery，我会先定义不变量，而不是先写定时扫描器。redelivery 是状态机，不是定时任务。定时器只负责发现“哪些 delivery 已经过期”，真正要保证的是消息什么时候可以再次投递、投给谁、attempt 怎么增加、旧 ack 怎么处理、什么时候停止普通重试。

最小不变量可以这样写：

```text
1. 每条消息有稳定 message_id，每次投递有唯一 delivery_id。
2. receive 原子地创建 delivery，设置 owner、visible_at/ack_deadline、attempt。
3. 未 ack 的 delivery 到期后，消息可以重新进入 Ready 或直接生成下一次 delivery。
4. ack 只能确认当前有效 delivery；旧 delivery 的 ack 不能删除新 delivery。
5. attempt 必须单调增加，不能因为并发 timeout 扫描重复加多次。
6. redelivery 不能越过终态，Acked/Deleted/DeadLettered 的消息不能被普通重投。
7. maxDeliver/maxReceiveCount 达到后，消息进入 DLQ 或失败状态。
8. redelivery 策略要区分 immediate、delay、exponential backoff、manual redrive。
9. 如果有 group/FIFO/partition 顺序，重投不能破坏该顺序边界。
10. 状态变更要可恢复：崩溃重启后能从 WAL/数据库/日志恢复 in-flight 和 deadline。
11. redelivery 事件要可观测：attempt、reason、last_error、next_visible_at、previous_owner 都要能查。
```

一个简化实现可以用四个结构：ready queue、in-flight map、deadline heap、message state store。receive 从 ready queue 取消息，写入 in-flight map，并把 deadline 放进 heap。ack 按 delivery_id 删除 in-flight，把消息标为 Acked。timeout scanner 从 heap 顶部取过期 delivery，检查它仍然是当前 delivery，再决定重新放回 ready queue、设置 backoff 后的 next_visible_at，或者转入 DLQ。

伪代码可以这样理解：

```text
receive(now):
  msg = pop_ready(now)
  delivery_id = new_id()
  msg.current_delivery_id = delivery_id
  msg.state = InFlight
  msg.attempt += 1
  msg.deadline = now + visibility_timeout
  persist(msg)
  return msg, delivery_id

ack(message_id, delivery_id):
  msg = load(message_id)
  if msg.state == InFlight and msg.current_delivery_id == delivery_id:
    msg.state = Acked
    persist(msg)
    return ok
  return stale_or_not_found

timeout_scan(now):
  for expired delivery:
    msg = load(message_id)
    if msg.current_delivery_id != delivery_id or msg.state != InFlight:
      continue
    if msg.attempt >= max_attempts:
      msg.state = DeadLettered
    else:
      msg.state = Ready
      msg.next_visible_at = now + backoff(msg.attempt)
    persist(msg)
```

这里有几个细节很容易漏。第一，timeout scanner 必须二次校验 current_delivery_id，因为 heap 里可能有旧 deadline。第二，attempt 是投递次数还是失败次数要说清楚，SQS 的 receive count 更接近投递次数。第三，backoff 要加 jitter，避免所有失败消息同一时间回流。第四，DLQ 不是只存 payload，最好带上最后错误、attempt、first_seen、last_seen、trace id。第五，如果消息体很大，state store 存 payload_ref，不要把大 payload 在 ready/in-flight/DLQ 间反复复制。

分布式版还要补几条：只有当前 leader 或当前 shard owner 能发放 delivery；deadline 判断用服务端时间；状态写入要经过 quorum 或事务；leader failover 后 timeout scanner 不能重复处理同一批过期 delivery；旧 leader 的 ack、timeout 或 redelivery 事件要被 epoch 拦住。没有这些约束，redelivery 会在故障转移时重复放大。

面试里可以这样回答：

```text
我会先定义 redelivery 不变量：消息有稳定 message_id，每次投递有唯一 delivery_id；receive 原子创建 delivery 并设置 deadline；未 ack 到期后才有资格重投；ack 只能确认当前 delivery；旧 ack 和旧 timeout 不能影响新 delivery；attempt 单调；Acked、Deleted、DeadLettered 是终态，不能普通重投；超过 maxDeliver 进 DLQ；backoff 和 jitter 决定 next_visible_at；FIFO/group/partition 顺序不能被重投破坏；所有状态能从持久日志恢复。实现上用 ready queue、in-flight map、deadline heap 和状态表，timeout scanner 只负责发现过期 delivery，真正状态变更用 CAS 或事务完成。
```

## Q061. redelivery 的常见误用是什么，误用后通常会产生什么线上症状？

redelivery 最常见的误用，是把它当成“免费重试”。实际上每一次 redelivery 都会消耗队列资源、消费者资源、下游资源和排查成本。如果错误是永久性的，重投只会把同一个坏消息处理很多遍；如果下游正在故障，重投会把压力继续打回去；如果消费者不幂等，重投会直接变成重复副作用。

常见误用包括：

```text
没有 maxReceiveCount/MaxDeliver，毒丸消息无限循环。
没有 DLQ，失败消息和正常消息混在一起反复处理。
没有错误分类，把永久错误当临时错误重试。
timeout/AckWait 配得太短，慢任务被提前重投。
timeout/AckWait 配得太长，消费者崩溃后恢复很慢。
没有 backoff/jitter，大量消息同时重投。
消费者不幂等，重复投递造成重复扣款、重复发货、重复通知。
DLQ 一键全量 redrive，修复不完整时再次打爆主队列。
Kafka 里用无限本地 retry 卡住 partition，后续消息全部延迟。
把 redelivery 当调度器，依赖它做长周期定时任务。
```

线上症状通常分两种。第一种是重复风暴：消费者 QPS 很高，但成功数不涨；下游 5xx 或限流增加；同一 message_id 的 attempt 快速上升；日志里同一错误刷屏；DLQ 进入率飙升；用户收到重复通知或看到状态反复跳。队列很忙，但业务没有向前走。

第二种是静默延迟：timeout 配得太长，或者 hot partition 被毒丸消息卡住，表面没有大量错误，但 oldest age、consumer lag、用户等待时间持续上升。SQS FIFO 中一个 MessageGroupId 的头部消息反复失败，会让后续消息都等着；Kafka 一个 partition 的某条记录一直失败，后续 offset 也不能安全提交。系统不是崩了，而是局部卡住了。

还有一种误用是用 redelivery 掩盖数据质量问题。比如 schema 不兼容、字段缺失、业务状态非法，消费者一直失败，开发者只调大 maxReceiveCount 或 visibility timeout。这样做只是延迟 DLQ，不能修复数据。更好的做法是把错误分为可重试、不可重试和需要人工判断三类，并把不可重试消息带上下文送到失败表或 DLQ。

Kafka 场景里还有一个特别常见的误用：在消费线程里无限 retry，不提交 offset，也不把失败记录转移到 retry topic 或 DLQ。结果一个 poison record 卡住整个 partition。短期看“没有丢消息”，长期看后面的正常消息全部被拖延。更稳妥的策略是有限本地 retry，然后把失败事件写入 retry/DLQ 主题，主消费路径继续推进，但这要求业务能接受乱序或按 key/partition 做更细的设计。

面试里可以这样回答：

```text
redelivery 常见误用是把它当免费重试：没有 maxDeliver、没有 DLQ、没有错误分类、没有 backoff/jitter、timeout 配错、消费者不幂等、DLQ 全量回灌、Kafka poison record 无限本地 retry。线上症状要么是重复风暴，表现为 attempt 暴涨、消费者很忙但成功数不涨、下游限流、DLQ 飙升、用户收到重复通知；要么是静默延迟，表现为 oldest age 或 lag 上升、FIFO group 或 Kafka partition 被单条坏消息卡住。修复方向是错误分类、幂等、指数退避、最大投递次数、DLQ、限速 redrive 和对 hot key/partition 的隔离。
```

## Q062. redelivery 在单机和分布式环境中的语义有什么差异？

单机环境里，redelivery 可以是一个本地状态机：消息在 ready list、in-flight map 和 deadline heap 之间移动；timeout scanner 发现过期 delivery，把消息放回 ready。所有状态在一个进程里，用一把锁或分片锁就能控制大部分并发。只要进程活着，语义比较容易推理。

单机的难点在崩溃恢复。如果 in-flight 和 deadline 只存在内存里，进程崩溃后系统不知道哪些消息已经投递、哪些应该重投、attempt 到多少了。要让单机 redelivery 有可靠语义，至少要把 receive、ack、timeout、DLQ 写进 WAL、数据库或追加日志。否则重启后可能出现两种问题：已经处理但未记录 ack 的消息重复，已经投递但未记录 in-flight 的消息立刻重新出现。

分布式环境里，redelivery 是复制状态、租约和所有权问题。多个 broker、多个 shard、多个 consumer 同时存在，timeout 判断、ack 到达、leader 切换、网络延迟都可能交错。一个节点认为 delivery 过期，另一个节点可能刚收到 ack；旧 leader 可能还在扫描 timeout，新 leader 已经接管 shard；消费者可能在网络分区后继续处理旧 delivery。没有 generation、leader epoch、CAS 或事务，redelivery 会变成重复和乱序的来源。

差异可以这样看：

```text
单机: 时间和状态都在本地，主要处理线程并发和进程崩溃。
分布式: 时间、状态和所有权分散，必须处理网络延迟、复制、failover。
单机: timeout scanner 可以直接修改内存结构。
分布式: timeout scanner 必须确认自己仍是 owner，状态更新要持久化或达成 quorum。
单机: ack 和 redelivery 的竞争用锁解决。
分布式: 需要 delivery_id、receipt_handle、generation、leader epoch 做 fencing。
单机: 指标通常精确。
分布式: visible、in-flight、oldest age、lag 可能是近似或延迟统计。
```

SQS 的标准队列语义就是分布式现实的体现：visibility timeout 可以减少并发处理，但 at-least-once 模型下仍不能保证绝不重复；DeleteMessage 成功后，应用仍要能处理后续少数重复。NATS JetStream 通过 durable consumer state 跟踪 delivery 和 ack，consumer 可以从失败中恢复，但 AckWait 和 MaxDeliver 仍然表达的是 at-least-once。Kafka 经典 consumer 不做 per-message redelivery，它通过 consumer group 和 committed offset 恢复，partition ownership 变化时会重新读取未提交区间。

分布式环境还有一个顺序差异。单机队列可以比较容易保证全局 FIFO；分布式系统通常只能保证分区、subject、MessageGroupId 或 key 范围内的顺序。redelivery 会放大这个边界：某个 group 的头部消息失败，后续消息就被挡住；如果为了吞吐绕开这个限制，又会牺牲顺序。设计时必须明确“顺序优先还是吞吐优先”，不能只说支持 redelivery。

面试里可以这样回答：

```text
单机里的 redelivery 是本地状态机问题，ready、in-flight、deadline heap 用锁保护即可，真正难点是崩溃后能不能从 WAL 或数据库恢复。分布式里的 redelivery 是复制租约问题，要处理网络延迟、leader 切换、旧 ack、旧 timeout scanner、状态复制和近似指标，需要 delivery_id、receipt_handle、generation、leader epoch、CAS 或事务做 fencing。SQS 标准队列即使有 visibility timeout 也仍是 at-least-once；NATS JetStream 依赖 consumer state、AckWait、MaxDeliver；Kafka 通过 committed offset 恢复未提交区间。分布式 redelivery 不能承诺绝不重复，业务幂等和顺序边界必须单独设计。
```
## 参考资料

- Apache Kafka Documentation, [Introduction](https://kafka.apache.org/43/getting-started/introduction/)
- Apache Kafka Documentation, [Design](https://kafka.apache.org/43/design/design/)
- Apache Kafka Documentation, [Consumer and Share Consumer Configs](https://kafka.apache.org/43/configuration/consumer-configs/)
- Apache Kafka Documentation, [Producer Configs](https://kafka.apache.org/43/configuration/producer-configs/)
- Apache Kafka Documentation, [Consumer Rebalance Protocol](https://kafka.apache.org/43/operations/consumer-rebalance-protocol/)
- Apache Kafka Documentation, [Transaction Protocol](https://kafka.apache.org/43/operations/transaction-protocol/)
- NATS Docs, [Publish-Subscribe](https://docs.nats.io/nats-concepts/core-nats/pubsub)
- NATS Docs, [Queue Groups](https://docs.nats.io/nats-concepts/core-nats/queue)
- NATS Docs, [JetStream Consumers](https://docs.nats.io/nats-concepts/jetstream/consumers)
- NATS Docs, [JetStream Streams](https://docs.nats.io/nats-concepts/jetstream/streams)
- Amazon SQS Developer Guide, [Amazon SQS visibility timeout](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/sqs-visibility-timeout.html)
- Amazon SQS Developer Guide, [Amazon SQS standard queues](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/standard-queues.html)
- Amazon SQS Developer Guide, [Amazon SQS at-least-once delivery](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/standard-queues-at-least-once-delivery.html)
- Amazon SQS Developer Guide, [Amazon SQS FIFO queues](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/sqs-fifo-queues.html)
- Amazon SQS Developer Guide, [Using dead-letter queues in Amazon SQS](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/sqs-dead-letter-queues.html)
- Amazon SQS Developer Guide, [Preventing duplicate processing in a multiple-producer/consumer system](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/avoding-processing-duplicates-in-multiple-producer-consumer-system.html)
- Apache Kafka Documentation, [Broker Configs](https://kafka.apache.org/43/configuration/broker-configs/)
- Apache Kafka Documentation, [Multi-Tenancy](https://kafka.apache.org/43/operations/multi-tenancy/)
- NATS Docs, [Client Protocol](https://docs.nats.io/reference/reference-protocols/nats-protocol)
- Amazon SQS Developer Guide, [Amazon SQS delay queues](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/sqs-delay-queues.html)
- Amazon SQS Developer Guide, [Amazon SQS message timers](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/sqs-message-timers.html)
- Amazon SQS Developer Guide, [Amazon SQS message quotas](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/quotas-messages.html)
- Amazon SQS Developer Guide, [Managing large Amazon SQS messages with Extended Client Library and Amazon S3](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/sqs-managing-large-messages.html)
- Amazon SQS Developer Guide, [Amazon SQS fair queues](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/sqs-fair-queues.html)
- Confluent Documentation, [Schema Evolution and Compatibility Types](https://docs.confluent.io/platform/current/schema-registry/fundamentals/schema-evolution.html)
- OpenTelemetry, [Semantic conventions for messaging spans](https://opentelemetry.io/docs/specs/semconv/messaging/messaging-spans/)
- Apache Kafka Documentation, [Topic Configs](https://kafka.apache.org/43/configuration/topic-configs/)
- Apache Kafka Documentation, [Monitoring](https://kafka.apache.org/43/operations/monitoring/)
- NATS Docs, [Slow Consumers](https://docs.nats.io/running-a-nats-service/nats_admin/slow_consumers)
- Amazon SQS Developer Guide, [Amazon SQS short and long polling](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/sqs-short-and-long-polling.html)
- Apache Kafka Javadocs, [KafkaConsumer](https://kafka.apache.org/43/javadoc/org/apache/kafka/clients/consumer/KafkaConsumer.html)
- Amazon SQS API Reference, [DeleteMessage](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/APIReference/API_DeleteMessage.html)
- Amazon SQS API Reference, [DeleteMessageBatch](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/APIReference/API_DeleteMessageBatch.html)
