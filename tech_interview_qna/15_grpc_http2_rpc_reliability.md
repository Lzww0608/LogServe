# 15. gRPC、HTTP/2、RPC 语义与网络调用可靠性

这一组问题的核心不是“gRPC 怎么调接口”，而是网络调用的语义边界：一次远程调用到底有没有到达服务端？服务端有没有执行？响应丢了以后能不能重试？deadline 到了以后下游还在不在跑？这些问题在本地函数调用里很少显式出现，但在 RPC 里会直接变成线上故障。

可以先抓住几条主线：

```text
RPC:
  把远程服务包装成像本地方法一样的接口，但不能真的拥有本地调用的确定性。

HTTP/2:
  给 gRPC 提供 multiplexing、stream、flow control、header compression、binary framing 等传输能力。

deadline:
  表达“调用方还愿意等多久”，应该沿调用链传播，并让服务端及时停止无意义工作。

status code:
  表达调用结果和失败类型，直接影响重试、告警、降级和客户端行为。

retry:
  只能在语义允许时做。网络层看到失败，不等于业务没有执行。
```

这份回答参考了一手规范和官方文档，包括 RFC 9113 HTTP/2、gRPC 官方 core concepts、deadline、status code、error handling、flow control、retry 和 request hedging 文档。

## Q001. RPC 和本地函数调用的本质差异是什么？

**回答：**

RPC 的危险之处就在于它看起来像本地函数调用。客户端代码里可能只是：

```go
resp, err := userClient.GetProfile(ctx, req)
```

形式上像调用一个对象方法，但语义完全不同。本地函数调用发生在同一个进程、同一个地址空间里，参数通常是内存对象，失败模式主要是返回错误、panic、异常或进程崩溃。RPC 跨进程、跨机器、跨网络，调用过程里多了序列化、传输、排队、负载均衡、超时、取消、鉴权、版本兼容和远端执行。

本质差异可以从几个角度说。

第一，本地调用的控制流比较确定，RPC 的控制流不确定。本地函数如果返回了，调用方基本知道函数执行完了；如果 panic，也知道发生在当前进程里。RPC 超时时，客户端只知道自己没等到结果，不知道服务端到底有没有执行完。请求可能没发出去，可能发到了负载均衡器，可能到达服务端但还没进业务逻辑，也可能业务已经提交成功，只是响应在回来的路上丢了。

这就是 RPC 最重要的语义边界：

```text
timeout != server did not execute
connection reset != business rollback
retry != safe by default
```

第二，RPC 有部分失败。本地函数通常和调用方同生共死；RPC 里客户端、服务端、网络、中间代理、服务发现、DNS、连接池都可能单独失败。客户端还活着，服务端可能挂了；服务端成功了，响应可能丢了；代理重启了，后端其实没问题。分布式系统里最难处理的就是这种“我不知道对方做到了哪一步”。

第三，RPC 有显著的延迟和排队。本地函数调用通常是纳秒到微秒级；RPC 要经过序列化、内核、网络、TLS、HTTP/2 stream、服务端队列、线程池或 event loop、业务处理、再走回来。调用方必须显式设置 deadline，否则就可能无限等。gRPC 官方 deadline 文档也提醒，默认不设置 deadline 时，客户端可能一直等待响应。

第四，RPC 必须处理数据契约。本地函数参数和返回值由同一份代码编译器检查；RPC 两边可能是不同语言、不同版本、不同部署节奏。请求和响应要通过 protobuf 或其他 IDL 定义。字段新增、默认值、unknown fields、枚举值、错误模型都要考虑兼容。

第五，RPC 需要显式安全边界。本地函数调用一般在进程内，权限边界比较清楚；RPC 是网络入口，要考虑认证、授权、TLS、mTLS、token、metadata、租户隔离和审计。不能因为接口像函数，就把它当成可信内部调用。

第六，RPC 的副作用要设计幂等。本地函数失败时，调用方通常能通过当前进程状态判断是否需要重试；RPC 失败时，客户端很可能不知道远端是否已经写库、发消息、扣款、创建资源。只要有副作用，就要考虑 request id、idempotency key、业务唯一约束或事务状态机。

面试里可以这样回答：

```text
RPC 是远程调用，不是本地函数调用的透明替代。它多了网络不确定性、部分失败、序列化、版本兼容、超时、取消、重试和安全边界。最关键的一点是：RPC 失败只说明调用方没有拿到一个成功结果，不说明服务端没有执行。所以涉及副作用的 RPC 必须设计 deadline、幂等、状态码和可观测性。
```

## Q002. gRPC 基于 HTTP/2 带来了哪些能力？

**回答：**

gRPC 可以理解为在 HTTP/2 之上定义了一套 RPC 语义：用 `.proto` 定义 service 和 message，用 HTTP/2 stream 承载请求响应，用 metadata 和 trailers 传递头信息、状态码和错误信息。默认数据格式是 protobuf，但 gRPC 的核心不只是 protobuf，它很大一部分能力来自 HTTP/2。

HTTP/2 给 gRPC 带来的能力主要有这些。

```text
multiplexing:
  一个 TCP 连接上可以同时跑多个 stream。多个 RPC 不需要每个都开一条连接。

streaming:
  HTTP/2 stream 是双向的字节流抽象，gRPC 在上面实现 unary、server streaming、client streaming、bidirectional streaming。

flow control:
  HTTP/2 有连接级和 stream 级 flow control，接收方可以限制发送方速度，避免被快发送方压垮。

header compression:
  HPACK 压缩重复 metadata 和伪头字段，减少大量 RPC 里重复 header 的成本。

binary framing:
  HTTP/2 用二进制 frame 表达 HEADERS、DATA、WINDOW_UPDATE、RST_STREAM、PING 等控制信息，比 HTTP/1.1 文本解析更适合机器协议。

cancellation:
  通过 stream 关闭、RST_STREAM、deadline 过期等机制，gRPC 可以表达调用取消。

trailers:
  gRPC 常用 response trailers 传 `grpc-status`、`grpc-message` 和错误细节。这样即使响应体是 stream，也能在末尾给出最终状态。

connection reuse:
  channel 可以复用底层 HTTP/2 连接，减少握手、TLS 和慢启动成本。
```

这些能力让 gRPC 比传统 HTTP/1.1 上的 RPC 更适合高并发内部服务调用。比如同一个客户端到同一个后端服务，可以在一条连接上同时发很多 RPC；服务端可以边生成边返回流式结果；客户端上传大文件时，可以一边发 chunk，一边让底层 flow control 控制发送速度。

但也要注意，HTTP/2 并没有解决所有网络问题。RFC 9113 明确说 HTTP/2 的 multiplexing 解决的是 HTTP 层的队头阻塞和多连接低效，但 TCP 层的 head-of-line blocking 仍然存在。一条 TCP 连接丢包时，这条连接上的所有 stream 都可能受影响。gRPC 也不会自动把业务做成 exactly-once；status code、deadline、retry、幂等仍然要业务设计。

面试里可以这样说：

```text
gRPC 借 HTTP/2 得到了多路复用、双向 stream、连接复用、header compression、binary framing、flow control、cancellation 和 trailers。protobuf 解决消息结构，HTTP/2 解决传输能力，gRPC 再在上面定义 RPC 生命周期、状态码、deadline 和 streaming API。不过 HTTP/2 不能消除 TCP 层丢包带来的阻塞，也不能替业务决定哪些调用可以重试。
```

## Q003. HTTP/2 multiplexing 解决了 HTTP/1.1 的哪些问题？

**回答：**

HTTP/1.1 的核心问题是一个连接上的并发能力很弱。HTTP/1.0 基本是一条连接一次请求；HTTP/1.1 支持持久连接和 pipelining，但 pipelining 没有彻底解决应用层 head-of-line blocking。前面的响应没回来，后面的响应就算已经准备好了，也很难越过它返回。浏览器和客户端最后常用多条 TCP 连接绕过这个问题。

多连接方案能并发，但也带来代价：

```text
每条连接都要 TCP/TLS 握手；
每条连接都有自己的拥塞窗口；
服务端要维护更多 fd、buffer、TLS state；
请求 header 大量重复；
连接之间竞争带宽，整体调度不够精细。
```

HTTP/2 multiplexing 的做法是把一个请求/响应 exchange 放到一个 stream 里，再把不同 stream 的 frame 交错发送到同一个连接上。RFC 9113 里说得很清楚：每个 HTTP 请求/响应关联自己的 stream，stream 大体相互独立，一个 stalled request/response 不应该阻止其他 stream 前进。

这解决了几个问题。

第一，减少连接数量。一个客户端到一个 origin 或后端，可以用更少的 TCP 连接承载更多并发请求。对 gRPC 来说，一个 channel 通常就能跑很多并发 RPC。

第二，减少 HTTP/1.1 pipelining 的应用层队头阻塞。HTTP/2 的 DATA、HEADERS 等 frame 可以来自不同 stream，慢请求不会在 HTTP 层把快请求完全堵在后面。

第三，提高长连接利用率。连接保持得更久，拥塞窗口更稳定，TLS 握手和连接建立成本更少。

第四，配合 HPACK 降低重复 header 成本。内部 RPC metadata 往往重复，比如 authority、method、content-type、authorization、trace header。HTTP/2 header compression 能减少这部分流量。

第五，给流式 RPC 提供基础。server streaming、client streaming、bidirectional streaming 都需要在同一个逻辑调用里持续收发消息。HTTP/2 stream 比 HTTP/1.1 的请求响应模型更适合这个语义。

但 multiplexing 也有边界。它不是“所有请求都完全隔离”。几个点要记住：

```text
TCP head-of-line blocking 仍然存在:
  底层 TCP 丢包时，同一连接上的所有 stream 都要等缺失字节补齐。

connection-level flow control 会影响所有 stream:
  连接窗口耗尽时，不只是某个 stream 会停。

HPACK 是连接级有状态压缩:
  header 解码状态出错可能影响整个连接。

连接过载会形成共享故障域:
  单条连接上承载太多流量，抖动会影响更多 RPC。
```

面试里可以这样回答：

```text
HTTP/2 multiplexing 解决了 HTTP/1.1 一条连接并发弱、pipelining 队头阻塞、多 TCP 连接开销大、重复 header 多的问题。它把每个请求响应放进独立 stream，再把不同 stream 的 frame 交错发到同一连接上。边界是它只解决 HTTP 层 multiplexing，不解决 TCP 层丢包导致的 head-of-line blocking。
```

## Q004. HTTP/2 stream-level flow control 是什么？

**回答：**

HTTP/2 flow control 是一种接收方控制发送方速度的机制。简单说，接收方告诉发送方“我现在还能接收多少 DATA 字节”，发送方只能在窗口允许的范围内发送。窗口耗尽后，必须等对方通过 `WINDOW_UPDATE` 补充窗口。

HTTP/2 有两层 flow control：

```text
connection-level flow control:
  限制整条连接上所有 DATA frame 的总发送量。

stream-level flow control:
  限制单个 stream 上 DATA frame 的发送量。
```

stream-level flow control 的作用，是防止某一个大 stream 把接收方内存打爆，也防止一个慢消费者无限积压数据。比如一个 server streaming RPC 正在不停返回日志，如果客户端读取很慢，客户端的 HTTP/2/gRPC runtime 就不会无限接收。窗口变小后，服务端写 stream 会被阻塞或等待。

这和应用层 backpressure 是连在一起的。gRPC flow control 文档里也说，flow control 是为了避免快发送方压垮接收方；写入 stream 不代表数据已经发到网络上，只是交给 gRPC runtime，runtime 可能因为 flow control 等待。

几个关键点：

```text
flow control 只控制 DATA frame:
  header、metadata、控制 frame 不按同样方式消耗 stream data window。

窗口是信用额度:
  接收方读走数据并释放能力后，通过 WINDOW_UPDATE 增加窗口。

stream 之间相对隔离:
  单个 stream 的窗口耗尽，不应该直接让其他 stream 的 stream window 耗尽。

connection window 是共享边界:
  如果连接级窗口耗尽，所有 stream 都会受影响。

应用必须持续读:
  如果应用不读流，窗口不释放，对端最终会停住。
```

在 gRPC 里，stream-level flow control 最常见的坑是“双方都在写，没人读”。例如双向流里客户端同步写很多消息，服务端也同步写很多响应，但双方都没及时读对方消息，就可能互相等窗口，造成死锁。gRPC 官方 flow control 文档也提醒，手动 flow control 或同步读写如果两边都大量写而不读，可能出现 deadlock。

面试里可以这样说：

```text
HTTP/2 stream-level flow control 是每个 stream 独立的接收窗口。DATA frame 发送会消耗窗口，接收方读取并释放能力后用 WINDOW_UPDATE 补窗口。它把网络层 backpressure 传到 gRPC stream 写入上，防止快发送方压垮慢接收方。要注意连接级窗口仍然是共享的，应用不读流会导致窗口不释放，双向流两边只写不读还可能死锁。
```

## Q005. gRPC unary、server streaming、client streaming、bidirectional streaming 的适用场景是什么？

**回答：**

gRPC 官方 core concepts 把 service method 分成四类：unary、server streaming、client streaming、bidirectional streaming。选哪一种，不是看“哪个高级”，而是看请求和响应的自然形态。

### Unary RPC

unary 是一个请求，一个响应，最像普通函数调用：

```proto
rpc GetUser(GetUserRequest) returns (GetUserResponse);
```

适合查询一个对象、提交一个命令、检查状态、创建资源、更新配置这类边界清楚的操作。大多数 CRUD 或 command API 都应该先从 unary 开始。它简单、容易设置 deadline、容易做重试策略、监控也清楚。

但 unary 不适合一次返回大量结果。比如一次查 10 万条记录，如果用 unary 塞进一个巨大 response，会带来内存峰值、超时、重试成本和尾延迟。更合理的是分页，或者 server streaming。

### Server streaming RPC

server streaming 是一个请求，服务端返回多个响应：

```proto
rpc ListEvents(ListEventsRequest) returns (stream Event);
```

适合这些场景：

```text
分页或大结果集:
  服务端边查边返回，客户端边读边处理。

watch/subscription:
  客户端订阅某个资源变化，服务端持续推送事件。

日志、指标、任务进度:
  服务端不断产生结果，不必等全部完成。

模型推理流式输出:
  逐 token 或逐 chunk 返回。
```

关键边界是客户端要持续读取，否则 flow control 会让服务端写阻塞。还要定义结束条件、心跳、取消、断线重连和从哪个 offset 恢复。

### Client streaming RPC

client streaming 是客户端发送多个请求，服务端最后返回一个响应：

```proto
rpc Upload(stream Chunk) returns (UploadResult);
```

适合上传文件、批量提交、客户端采集一段轨迹后汇总、流式写入日志等场景。服务端可以边收边校验，最后给一个 summary。

它的风险是失败恢复。客户端发到第 80 个 chunk 时连接断了，服务端已经写了多少？能不能续传？重复 chunk 怎么处理？这类接口最好有 upload id、chunk sequence、checksum 和幂等提交语义。

### Bidirectional streaming RPC

bidirectional streaming 是两边都发 stream：

```proto
rpc Chat(stream ClientMessage) returns (stream ServerMessage);
```

适合真正的双向会话：实时协作、聊天、语音识别、游戏同步、控制平面 watch、长连接代理、交互式任务执行。两边的读写相互独立，gRPC 保证单个方向内的消息顺序，但不保证“请求 1 一定对应响应 1”这种业务配对，除非你自己在消息里放 sequence id 或 correlation id。

双向流最容易出工程问题：流生命周期长，deadline 怎么设、心跳怎么做、backpressure 怎么传、服务端如何释放资源、断线如何恢复、消息是否幂等，都要写清楚。

面试里可以这样总结：

```text
unary 适合边界清楚的一问一答；server streaming 适合大结果集、订阅、进度和流式输出；client streaming 适合上传、批量写入和客户端聚合；bidirectional streaming 适合实时双向会话。越靠后的模式越灵活，但状态、flow control、取消、重连和幂等也越难。
```

## Q006. RPC deadline 和 timeout 如何传播？

**回答：**

timeout 是持续时间，比如“最多等 200ms”。deadline 是绝对截止点，比如“到 10:00:00.200 为止”。gRPC 文档里也解释了这个差别：有些语言 API 暴露 deadline，有些暴露 timeout；timeout 可以在调用开始时转换成 deadline。

RPC deadline 的语义是：调用方在这个时间点之后不再关心结果。它不是服务端的“保证完成时间”，也不是网络层的“必达时间”。deadline 到了，客户端会结束等待，RPC 通常以 `DEADLINE_EXCEEDED` 失败；服务端也应该停止无意义工作。

传播 deadline 的原因很简单：一个入口请求经常会调用多个下游。如果入口给了 1 秒，总不能中间服务花 900ms 后，还给下游设置 1 秒。正确做法是把剩余预算传下去。

一个典型链路：

```text
Client -> API Server -> User Service -> DB Service

入口 deadline: 500ms
API Server 排队和鉴权用了 80ms
调用 User Service 时只剩 420ms
User Service 做本地处理用了 120ms
调用 DB Service 时只剩 300ms
```

gRPC 的 deadline propagation 就是处理这个问题。有些语言默认传播，有些语言需要显式开启。官方文档还提到一个细节：deadline 是时间点，机器之间时钟可能不一致，所以 gRPC 在传播时会转换成 timeout，并扣掉已经消耗的时间，用来避免 clock skew 的影响。

服务端收到 deadline 后，应该做几件事：

```text
尽早检查剩余时间:
  如果剩余时间明显不够，不要启动昂贵任务。

把 context 传给下游:
  下游 RPC、数据库查询、消息发送都应该受同一预算控制。

周期性检查取消:
  长循环、流式处理、大查询、外部命令要检查 ctx 是否 done。

区分客户端取消和 deadline:
  CANCELLED 通常表示调用方不再需要；DEADLINE_EXCEEDED 表示时间预算耗尽。

记录剩余预算:
  观测里要看到 deadline 太短、排队太久还是下游太慢。
```

一个常见错误是每一层都重新设置固定 timeout：

```text
API Server 调 User Service: 500ms
User Service 调 DB Service: 500ms
DB Service 调 Storage: 500ms
```

这样入口客户端可能早就超时了，后端还在继续跑，形成无意义负载。流量尖峰时，这会把系统拖垮。

面试里可以这样说：

```text
deadline 表示调用方愿意等到什么时候，timeout 表示还能等多久。RPC 链路里应该传播剩余时间，而不是每一层重新给固定 timeout。gRPC 会把 deadline 转成扣除已耗时的 timeout 传下游，避免时钟偏差。服务端要把 context 继续传给数据库和下游 RPC，并在长任务里检查取消，否则客户端超时后后端还会继续浪费资源。
```

## Q007. gRPC status code 应该如何映射业务错误？

**回答：**

gRPC status code 是 RPC 语义的一部分，不只是错误字符串。客户端会根据 status code 决定是否重试、是否提示用户、是否告警、是否降级。乱用 status code，会让系统可靠性策略失效。

先分清两层：

```text
transport / framework error:
  网络断开、deadline 到、服务不可用、连接关闭、方法不存在、认证失败。

business error:
  参数非法、资源不存在、版本冲突、余额不足、状态不允许、配额不足。
```

gRPC status code 的选择原则是：用最能指导客户端下一步行为的 code，而不是用最像 HTTP 状态码的 code。

常见映射可以这样记：

```text
INVALID_ARGUMENT:
  请求参数本身非法，和系统当前状态无关。比如 email 格式错误、page_size 为负数。

NOT_FOUND:
  请求的资源不存在。注意权限隐藏场景有时也会返回 NOT_FOUND，但要有一致策略。

ALREADY_EXISTS:
  创建资源时资源已经存在，比如重复用户名、重复唯一 key。

PERMISSION_DENIED:
  调用方已认证，但没有权限。

UNAUTHENTICATED:
  调用方身份无法确认，比如 token 缺失、过期、无效。

RESOURCE_EXHAUSTED:
  配额、限流、容量、连接数、磁盘等资源耗尽。

FAILED_PRECONDITION:
  系统状态不满足操作前提，客户端需要先修正状态再重试。

ABORTED:
  并发冲突、事务 abort、CAS/版本检查失败，通常需要在更高层重做 read-modify-write。

UNAVAILABLE:
  服务临时不可用，适合有限重试。

DEADLINE_EXCEEDED:
  超过调用方 deadline。注意服务端可能已经完成副作用，只是响应晚了。

INTERNAL:
  服务端内部不变量破坏或未预期错误。不要拿它包所有业务错误。

UNKNOWN:
  信息不足、跨错误域转换失败。它应该少见。
```

业务错误要不要放进 response body？一般建议：真正失败的 RPC 用非 OK status；业务上可预期的“结果状态”也可以建模在 response 里，但要谨慎。

例如校验失败：

```text
CreateUser(email="bad")
-> INVALID_ARGUMENT
```

订单支付失败：

```text
PayOrder(order_id)
余额不足，如果这是业务流程的正常结果:
  可以返回 OK + payment_status=DECLINED

如果这个 RPC 的语义是“扣款必须成功，否则调用失败”:
  可以返回 FAILED_PRECONDITION 或特定业务错误 detail
```

错误细节可以用 richer error model，比如 `google.rpc.Status`、`BadRequest`、`PreconditionFailure`、`QuotaFailure`，放在 trailers 里。但要注意 gRPC 官方 error handling 文档也提醒，rich error model 的跨语言一致性、代理可见性、header/trailer 大小限制和 HPACK 效率都有成本。不要把大段业务对象塞进错误详情。

面试里可以这样答：

```text
gRPC status code 要表达客户端下一步该怎么做。参数错用 INVALID_ARGUMENT；资源没有用 NOT_FOUND；权限问题分 UNAUTHENTICATED 和 PERMISSION_DENIED；配额用 RESOURCE_EXHAUSTED；状态不满足用 FAILED_PRECONDITION；并发冲突用 ABORTED；临时不可用用 UNAVAILABLE；超时用 DEADLINE_EXCEEDED。不要所有业务错误都塞 INTERNAL，也不要把可重试和不可重试错误混在同一个 code 里。
```

## Q008. UNAVAILABLE、DEADLINE_EXCEEDED、ABORTED、FAILED_PRECONDITION 的语义区别是什么？

**回答：**

这四个 code 经常被混用，但它们对客户端行为的指导完全不同。gRPC 官方 status code 文档对 `FAILED_PRECONDITION`、`ABORTED`、`UNAVAILABLE` 的选择给了很直接的 guideline：如果客户端只需要重试当前失败调用，用 `UNAVAILABLE`；如果客户端应该在更高层重试整个读改写序列，用 `ABORTED`；如果必须先修正系统状态，再重试，用 `FAILED_PRECONDITION`。

### UNAVAILABLE

`UNAVAILABLE` 表示服务当前不可用，通常是临时问题。比如后端实例重启、连接断开、负载均衡没有可用 endpoint、服务正在 graceful shutdown、网络短暂故障。

客户端行为：

```text
可以有限重试；
要有 backoff 和 jitter；
要受 deadline 限制；
不能默认无限重试；
只适合语义允许重试的 RPC。
```

`UNAVAILABLE` 不是业务失败。不要用它表示“库存不足”“订单状态不对”。

### DEADLINE_EXCEEDED

`DEADLINE_EXCEEDED` 表示 deadline 过了，调用没能在调用方愿意等待的时间内完成。它最容易被误解。官方文档也提醒：对会改变系统状态的操作，即使最终返回了 `DEADLINE_EXCEEDED`，操作也可能已经成功完成，只是响应来得太晚。

客户端行为：

```text
不能简单判断服务端没执行；
有副作用时要用幂等 key 或查询操作确认状态；
可以根据业务语义决定是否重试；
要检查 deadline 是否设置过短、排队是否过长。
```

### ABORTED

`ABORTED` 通常是并发冲突或事务中止。比如版本号检查失败、事务冲突、compare-and-swap 失败、乐观锁冲突。它的意思不是“服务挂了”，而是“你这次操作基于的前提被别人改了”。

客户端行为：

```text
不要只重试同一个写请求；
应该重新读取状态，重新计算，再提交；
适合重做 read-modify-write 序列。
```

例如：

```text
GetAccount(version=10)
UpdateAccount(expected_version=10)
-> ABORTED because current version is 11
```

这时直接重发 `expected_version=10` 没意义。

### FAILED_PRECONDITION

`FAILED_PRECONDITION` 表示系统当前状态不满足操作前提，而且这个状态需要被显式修正。比如删除非空目录、启动已经被禁用的 workflow、提交未完成校验的订单、在未关闭任务前删除 worker。

客户端行为：

```text
不要自动重试；
先修正状态；
可能需要用户操作或另一个 API 调用。
```

它和 `INVALID_ARGUMENT` 的区别是：`INVALID_ARGUMENT` 是参数本身永远不对；`FAILED_PRECONDITION` 是参数在某些系统状态下不成立。

面试里可以这样总结：

```text
UNAVAILABLE 是临时不可用，当前调用可有限重试；DEADLINE_EXCEEDED 是时间预算耗尽，不代表服务端没执行；ABORTED 是并发冲突，要重做更高层读改写流程；FAILED_PRECONDITION 是系统状态不满足前提，要先修状态再调用。四者的差别不在错误文案，而在客户端下一步行为。
```

## Q009. 哪些 RPC 可以安全重试，哪些不能？

**回答：**

判断 RPC 能不能重试，不能只看它是 GET 还是 POST，也不能只看 gRPC method name。要看业务语义：重复执行一次，会不会产生额外副作用？客户端能不能识别重复？服务端有没有幂等保护？

通常比较安全重试的 RPC：

```text
纯查询:
  GetUser、ListOrders、CheckStatus。前提是查询不会改变状态，也不会触发昂贵副作用。

幂等写:
  PutConfig、SetDesiredState、CreateWithRequestId、UpsertByUniqueKey。
  重复执行的最终状态相同。

带幂等 key 的命令:
  CreateOrder(request_id)、Charge(idempotency_key)。
  服务端用 key 去重，返回同一结果。

可交换或可去重的事件上报:
  带 event_id、sequence、dedup key 的 telemetry。

明确没有到达服务端业务逻辑的透明重试:
  gRPC 自己在低层 race 场景下可能做 transparent retry。
```

不应该默认重试的 RPC：

```text
非幂等创建:
  CreateOrder 没有 request_id，重试可能创建两笔订单。

扣款、发券、发短信、发邮件:
  重试可能重复扣、重复发。

append-only 写入:
  AppendLog、PublishMessage 如果没有 message id，重试可能产生重复记录。

状态推进:
  Submit、Approve、Cancel、Complete 如果没有状态机幂等，重试可能越过边界。

长事务或流式上传:
  连接断了以后服务端已收到多少不清楚，必须设计续传和 chunk 去重。

依赖时间或随机性的操作:
  重试可能得到不同结果，比如重新分配资源、重新抽样。
```

状态码也不是唯一依据。`UNAVAILABLE` 看起来适合重试，但如果 RPC 是“扣款”且没有幂等 key，自动重试仍然危险。`DEADLINE_EXCEEDED` 更危险，因为服务端可能已经完成副作用，只是响应晚了。遇到这类情况，客户端更应该先查询业务状态，而不是盲目重发。

比较稳的设计是让写操作显式具备幂等语义：

```text
client_request_id:
  客户端生成，服务端持久化去重。

business unique key:
  比如 order_id、payment_id、upload_id。

state machine:
  重复提交同一状态转换返回已有结果。

dedup window:
  对事件上报保留一段时间的 event_id。

result replay:
  重复请求返回第一次执行的结果，而不是重新执行副作用。
```

面试里可以这样答：

```text
安全重试的 RPC 要么是纯读，要么是幂等写，要么有服务端去重。非幂等创建、扣款、发消息、append、状态推进和流式半完成操作不能默认重试。判断标准不是 gRPC status code 本身，而是重复执行是否改变业务结果。对 DEADLINE_EXCEEDED 尤其要小心，因为服务端可能已经成功执行。
```

## Q010. gRPC retry policy 为什么需要谨慎配置？

**回答：**

retry policy 的目标是处理瞬时故障，但配置不好会把小故障放大成大故障。gRPC 官方 retry 文档也说，没有 retry policy 时，gRPC 不能在大多数情况下安全重试；默认只有低层 transparent retry，而且前提是 gRPC 确信请求没有被服务端应用逻辑处理。显式 retry policy 会让客户端在更多场景、更积极地重试，所以必须谨慎。

主要风险有几个。

第一，重复副作用。retry policy 只能看到 status code，看不到业务是否幂等。你把 `UNAVAILABLE` 配成可重试，如果某个写请求实际已经到达服务端并提交，只是响应路径失败，下一次 attempt 可能重复创建、重复扣款、重复发布消息。

第二，放大流量。一次请求失败，如果配置 `maxAttempts=4`，理论上会变成最多 4 次后端请求。故障时所有客户端同时重试，会把已经不稳定的服务压得更死。这就是 retry storm。

第三，尾延迟变长。重试需要 backoff。单次 attempt 失败后，第二次可能成功，但整体延迟已经接近 deadline。用户看到的 p99 可能变差。特别是同步调用链里，每一层都重试，会造成组合爆炸。

第四，和 deadline 冲突。重试必须受整体 call deadline 限制，而不是每次 attempt 重新拿一份完整 timeout。否则入口已经超时，下游还在执行重试。

第五，错误码配置过宽。有些团队把 `UNKNOWN`、`INTERNAL`、`DEADLINE_EXCEEDED` 都配成可重试。`UNKNOWN` 和 `INTERNAL` 可能是代码 bug，重试只会增加压力；`DEADLINE_EXCEEDED` 可能已经执行成功；`RESOURCE_EXHAUSTED` 如果是配额问题，重试会更糟。

第六，流式 RPC 更复杂。unary 请求历史比较容易 replay；client streaming 或 bidi streaming 里，前面已经发送的一串消息是否能重放、服务端是否已经处理一部分，都很难判断。自动 retry 对流式 RPC 要更保守。

第七，观测会变复杂。应用看到一次 RPC，底层可能有多个 attempts。指标要区分：

```text
logical call count；
attempt count；
retry delay；
per-attempt status；
final status；
server pushback；
throttling state；
deadline remaining。
```

否则你只看业务 QPS，以为流量没变；后端看到的 attempt QPS 已经翻倍。

比较稳的配置原则：

```text
只给明确幂等的方法开 retry；
只把 UNAVAILABLE 这类瞬时错误列为 retryable；
设置小的 maxAttempts；
使用指数 backoff 和 jitter；
尊重 server pushback 和 retry throttling；
全链路 deadline 控制总时间；
监控 attempt 级指标；
对写操作要求 idempotency key；
流式 RPC 默认不要自动重试，除非协议显式支持恢复。
```

gRPC retry 文档里的配置项也体现了这些约束：`maxAttempts`、`initialBackoff`、`maxBackoff`、`backoffMultiplier`、`retryableStatusCodes`，以及 retry throttling。配置的重点不是“多试几次”，而是“只在语义允许、系统承受得住、观测能看清的情况下重试”。

面试里可以这样说：

```text
gRPC retry policy 要谨慎，因为网络失败不等于业务没执行。重试可能重复副作用，也可能在故障时放大流量，形成 retry storm。安全配置必须按方法粒度开，只给幂等或有 idempotency key 的 RPC 配 retry，限制 maxAttempts，用 backoff+jitter，受 deadline 约束，并监控 attempt 级指标。流式和非幂等写默认不要自动重试。
```

## Q011. client-side load balancing 和 server-side load balancing 有什么差异？

**回答：**

这两个名字容易让人误解。这里的 server-side load balancing 通常不是“业务服务端自己均衡”，而是客户端先连到一个代理或负载均衡器，再由它把请求转给后端实例。gRPC 官方博客里也把这种模式叫 proxy load balancing；client-side load balancing 则是客户端知道后端实例列表，自己为每次 RPC 选择一个后端。

server-side load balancing 的链路大概是：

```text
client -> LB/proxy -> backend
```

客户端只知道 LB 的地址，不知道真实 backend。LB 可以是 L4，也可以是 L7。L4 负载均衡通常只看 TCP 连接，把一个客户端连接转到某个后端连接上；它不理解 gRPC method，也不理解 HTTP/2 stream。L7 负载均衡会终止并解析 HTTP/2/gRPC，可以按 method、metadata、tenant、header、健康状态等做选择，也可以把同一条客户端 HTTP/2 连接上的不同 stream 分发给不同后端。代价是 LB 在数据路径上，多了一跳，多了 TLS/HTTP/2 终止、转发、缓冲和故障点。

client-side load balancing 的链路更像：

```text
name resolver / xDS / service discovery -> client
client -> backend A/B/C
```

客户端通过 DNS、服务发现、xDS 或其他 resolver 拿到 endpoint 列表，在本地维护到各后端的 subchannel，然后用 `pick_first`、`round_robin`、加权、locality-aware、outlier ejection 等策略选择后端。gRPC 官方 custom load balancing 文档里说得很直接：name resolver 给 policy 一组 server IP，policy 维护连接并在 RPC 发送时选择连接。这样少了一跳，延迟和吞吐通常更好，尤其是东西向服务调用。但客户端复杂了：每种语言的 gRPC 客户端都要支持相同策略，客户端还要处理健康状态、连接抖动、endpoint 更新、fallback、熔断和观测。

两种模式的取舍可以这样看：

```text
server-side / proxy LB:
  优点是客户端简单，适合不可信客户端和公网入口；
  服务发现、证书、灰度、限流、WAF、审计可以集中做；
  缺点是多一跳，LB 可能成为吞吐瓶颈或单独的故障域；
  L4 对 HTTP/2 长连接不够细，可能按连接而不是按 RPC 均衡；
  L7 能按 RPC 均衡，但成本更高，也更容易引入代理兼容性问题。

client-side LB:
  优点是少一跳，延迟低，客户端可以按每次 RPC 选择后端；
  对 gRPC 的长连接和 HTTP/2 multiplexing 更自然；
  可以结合 locality、负载报告、健康状态做更细的选择；
  缺点是客户端必须可信或受控制，策略要跨语言一致；
  服务发现、证书、灰度和安全边界不能全靠客户端自觉。
```

gRPC 场景里，HTTP/2 长连接会放大这个差异。传统 HTTP/1.1 短连接或少量请求下，L4 LB 的连接级均衡还算够用；gRPC 一个 channel 可以长期复用同一条或少数几条 HTTP/2 连接，连接一旦被 L4 LB 分到某个后端，后面的很多 RPC 都可能落在同一个后端。结果是“连接均衡了，但 RPC 没均衡”。L7 proxy 或 client-side picker 才能更接近按 RPC 粒度均衡。

流式 RPC 还要单独看。一个 streaming RPC 一旦开始，就不能在中途被重新均衡到另一个后端。client-side 和 L7 server-side 都只能在 stream 建立前选择后端，不能把一个已经打开的 stream 拆开迁移。长连接、大 stream、多租户混跑时，要避免把所有重流量都压到一个后端；有时会按业务类型拆 channel、拆 endpoint，或者对长流单独做调度。

面试里可以这样答：

```text
server-side load balancing 是客户端打到一个代理或 LB，由它选择后端；client-side load balancing 是客户端通过服务发现拿到后端列表，自己为每个 RPC 选择 subchannel。前者客户端简单，适合公网和不可信客户端，但多一跳，L4 模式容易在 HTTP/2 长连接下变成连接级均衡；后者延迟低、按 RPC 选择更自然，但客户端复杂，要求服务发现、健康检查和策略跨语言一致。gRPC 内部服务更常见 client-side 或 xDS/service mesh 控制，公网入口更常见 proxy/L7 LB。
```

## Q012. gRPC keepalive 解决什么问题，又可能造成什么问题？

**回答：**

gRPC keepalive 解决的是“连接看起来还在，其实已经不可用”以及“连接太久没流量，被中间设备当成 idle 关掉”的问题。它不是业务健康检查。keepalive 只说明这条 HTTP/2 连接的对端还能回应 PING，不说明服务方法还能正常处理请求，也不说明数据库、依赖服务和业务线程池健康。

gRPC 的 keepalive 基于 HTTP/2 PING。连接空闲或长时间没有数据时，一端定期发 PING，对端要回 ACK。如果在 `KEEPALIVE_TIMEOUT` 内收不到 ACK，发送方认为连接不可用并关闭连接。它常见的价值有几个：

```text
长连接穿过 NAT、代理、LB:
  中间设备可能有 idle timeout。keepalive 可以让连接不被误认为空闲。

长时间 streaming RPC:
  流可能很久没有业务消息，但调用仍然有效。PING 可以尽早发现断链。

移动网络或不稳定网络:
  TCP 连接可能半开，应用层如果不探测，会等到下一次写才发现。

降低冷启动延迟:
  空闲一段时间后再发第一个 RPC，如果连接已经死了，keepalive 能提前暴露问题，避免请求路径上才重连。
```

问题也很明显。第一个是配置太激进会打爆服务端。官方 keepalive 文档专门提醒，不要为了“保险”把客户端 PING 间隔设得很短；如果大量客户端无业务流量也不断 PING，服务端看到的是持续控制面流量。服务端可以拒绝这种行为，甚至发 `GOAWAY`，debug data 可能是 `too_many_pings`。

第二个是它会制造误判。网络拥塞、短暂抖动、GC stop-the-world、代理排队，都可能让 PING ACK 晚到。keepalive timeout 太短时，会把本来可以恢复的连接主动关掉，导致正在进行的 streaming RPC 失败。对 unary RPC，这种失败可能只是下一次调用重连；对 client streaming 或 bidi streaming，可能意味着已经发送但未确认的一段消息要靠业务协议恢复。

第三个是资源成本。PING 不大，但连接数大时很可观。每个客户端、每个 channel、每个后端连接都发 PING，服务端要处理 frame、定时器、连接状态和日志。移动端还会影响电量和无线网络唤醒。代理层也可能把这些 PING 当成异常流量或触发连接管理策略。

第四个是安全边界。keepalive 不是授权，不是健康检查，也不是服务发现。一个连接能回应 PING，只说明 HTTP/2 transport 还活着；后端可能已经进入 draining，业务方法可能全部返回 `UNAVAILABLE`，也可能某个租户被限流。健康检查、负载均衡和熔断不能只看 keepalive。

配置上要记住几条原则：

```text
客户端是否允许 without calls，要和服务端约定；
不要把 PING 间隔设到秒级，除非两端明确接受；
长流可以比短 unary 更需要 keepalive；
keepalive timeout 要覆盖正常网络抖动；
服务端要设置 permit keepalive 和 max connection age/idle；
观测里要区分 keepalive 失败、业务失败和健康检查失败。
```

面试里可以这样说：

```text
gRPC keepalive 用 HTTP/2 PING 维护和探测连接，主要解决长连接空闲被代理/LB 回收、半开连接发现太晚、长时间 stream 无业务数据时无法确认连接存活的问题。它的问题是配置太频繁会给服务端和代理造成额外负载，甚至被服务端用 GOAWAY/too_many_pings 拒绝；timeout 太短会在抖动时误杀连接，让正在进行的 stream 失败。keepalive 只证明 transport 还活着，不等于业务健康。
```

## Q013. 连接池在 HTTP/2 下是否仍然重要？

**回答：**

仍然重要，但关注点变了。HTTP/1.1 时代连接池主要是为了并发和复用：一条连接同一时刻很难承载很多请求，客户端需要多条 TCP 连接绕过应用层队头阻塞。HTTP/2 有 multiplexing，一条连接上可以同时跑多个 stream，所以不再需要“每个并发请求一条连接”的池化方式。可是这不代表连接池消失了。

在 gRPC 里，更准确的对象通常是 channel、subchannel 和 HTTP/2 connection，而不是手写一个裸 TCP 连接池。官方 performance best practices 也建议尽量复用 stubs 和 channels。channel 复用可以省掉 TCP/TLS/HTTP/2 握手，保留 HPACK 状态、连接窗口、keepalive 状态和负载均衡状态。

连接池仍然重要的场景主要有这些。

第一，HTTP/2 单连接有并发 stream 上限。服务端会通过 `SETTINGS_MAX_CONCURRENT_STREAMS` 或实现内部限制控制一条连接上的并发 stream 数。达到上限后，新的 RPC 可能在客户端排队，等已有 stream 结束。高 QPS unary、慢请求混入、长时间 streaming RPC 都可能触发这个问题。官方性能文档也提到，活跃 RPC 数达到连接并发 stream 限制后，额外 RPC 会在客户端等待；临时方案可以是按高负载区域拆 channel，或者建 channel pool 分散到多条连接。

第二，连接级 flow control 是共享的。HTTP/2 有 stream-level flow control，也有 connection-level flow control。某个大响应、慢消费者或大上传如果把连接级窗口打满，其他 stream 也会受到影响。多个连接可以隔离不同业务类型，避免一个大流量调用拖慢所有小请求。

第三，TCP 层队头阻塞仍然存在。HTTP/2 解决的是 HTTP 层 multiplexing，但底层还是 TCP。一条连接丢包时，这条连接上的所有 stream 都要等缺失字节补齐。多条连接不能消除网络问题，但可以减少单条连接故障影响的 RPC 范围。

第四，负载均衡需要多条后端连接。client-side LB 会对多个后端维护 subchannel。即使每个后端只需要一条 HTTP/2 连接，整体上客户端仍然维护一个连接集合。对不同 region、zone、优先级、灰度版本、租户隔离，也可能需要不同 channel。

第五，连接身份和配置不一定相同。不同 channel 可能使用不同证书、authority、SNI、call option、keepalive、压缩、限流、deadline 策略。把所有流量塞进一个全局 channel，短期省事，长期会让隔离、观测和故障处理变难。

但连接也不是越多越好。连接过多会带来 TLS 握手、fd、内存、HPACK 状态、keepalive PING、服务端连接管理和负载均衡噪声。很多性能问题不是“连接太少”，而是 channel 没复用，导致每次 RPC 都新建连接；这会比单连接排队更糟。

一个比较稳的实践是：

```text
默认复用 channel/stub；
按后端、流量类型、隔离级别拆 channel；
只有在并发 stream 上限、长流阻塞、连接级 flow control 或 p99 排队证据明确时才引入 channel pool；
channel pool 要有上限、预热、健康状态和指标；
指标要看 active streams、queued RPC、subchannel state、connection age、flow-control stall、per-channel p99。
```

面试里可以这样答：

```text
HTTP/2 让一条连接可以 multiplex 多个 RPC，所以连接池不再是 HTTP/1.1 那种“靠多连接实现并发”的默认手段。但连接管理仍然重要：单连接有并发 stream 上限、连接级 flow control、TCP 队头阻塞和故障域问题；gRPC 还需要为多个后端维护 subchannel 来做 client-side LB。我的原则是先复用 channel/stub，只有在高并发、长流、大消息或排队指标证明单连接成为瓶颈时，才按业务隔离或 channel pool 扩多条连接。
```

## Q014. TLS、mTLS 和 ALPN 在 gRPC 中分别有什么作用？

**回答：**

这三个东西在同一条连接建立过程中经常一起出现，但作用不同。

TLS 解决两件事：加密传输和认证服务端。客户端连接 `api.example.com` 时，会校验证书链、域名、有效期和用途，确认自己连到的是目标服务，而不是中间人。然后双方协商会话密钥，后续 HTTP/2 frame、gRPC metadata、protobuf payload 都在加密通道里传输。gRPC 官方 authentication 文档也把 SSL/TLS 作为内建认证机制之一：用来认证 server，并加密 client/server 之间的数据。

mTLS 是 mutual TLS，也就是服务端也校验客户端证书。普通 TLS 里，通常只有客户端验证服务端；mTLS 里，客户端也要提供证书，服务端根据证书链、SAN、SPIFFE ID、组织字段或其他证书属性识别调用方。它常用于内部服务间调用、service mesh、零信任网络和高权限管理面 API。mTLS 给的是“这个连接对端是谁”的强身份信号，但它不是完整鉴权。鉴权还要根据 method、tenant、resource、scope、角色和策略判断这个身份能不能做某个操作。

ALPN 是 Application-Layer Protocol Negotiation。它让客户端在 TLS ClientHello 里带上自己支持的应用层协议，比如 `h2`，服务端在 ServerHello 里选择一个协议。RFC 7301 的设计点就是在 TLS 握手里完成应用协议选择，不额外增加 round trip。对 gRPC 来说，ALPN 的作用是让同一个 443 端口上的连接明确跑 HTTP/2，而不是 HTTP/1.1。没有正确的 ALPN，很多 TLS 终止层、代理或服务端会按 HTTP/1.1 处理，gRPC 就无法正常建立 HTTP/2 语义。

可以把它们分层理解：

```text
TLS:
  保护通道，认证服务端，加密 HTTP/2 frame 和 gRPC payload。

mTLS:
  在 TLS 基础上增加客户端证书认证，让服务端知道调用方身份。

ALPN:
  在 TLS 握手中协商应用层协议，让双方确认这条连接使用 h2。
```

还有几个细节经常被追问。

第一，TLS 和 call credentials 不是一回事。TLS/mTLS 是 channel 层安全；JWT、OAuth token、API key、tenant id 这类通常通过 call credentials 或 metadata 传递，是每次调用的身份或授权材料。官方文档也提醒，token-based credentials 通常要和 SSL/TLS 一起用；不要在明文通道里发送 bearer token。

第二，mTLS 身份不要盲目信 metadata。客户端可以伪造普通 metadata，但不能伪造通过受信 CA 签发并完成握手的客户端证书。服务端可以把证书身份映射成 principal，再结合 metadata 里的 tenant、scope、trace id 做进一步校验。

第三，ALPN 不是加密算法，也不是认证机制。它只是在 TLS 握手里决定“这条连接说哪种应用层协议”。它本身不说明调用方是谁，也不授权任何 RPC。

第四，TLS 终止位置会影响安全边界。如果 TLS 在边缘 LB 终止，LB 到后端可能是明文、重新 TLS 或 mTLS。内部链路如果承载敏感 metadata 和 payload，就不能只说“入口有 TLS”，还要说明后端链路怎么保护。

面试里可以这样答：

```text
TLS 在 gRPC 里负责加密通道和认证服务端；mTLS 在此基础上让服务端也校验客户端证书，常用于服务间身份；ALPN 在 TLS 握手里协商应用层协议，通常选择 h2，让双方确认这条 TLS 连接承载 HTTP/2/gRPC。TLS/mTLS 是连接层安全，JWT/OAuth 等 call credentials 是调用层身份或授权材料；ALPN 不是认证机制，只是协议协商。
```

## Q015. protobuf service evolution 如何保证兼容？

**回答：**

protobuf 兼容性的核心不是“字段名别乱改”，而是“wire identity 别乱动”。对 binary protobuf 来说，字段号才是 wire format 里的身份。字段名主要影响生成代码、JSON/TextFormat 和人的理解；字段号一旦上线，就不能改、不能复用。官方 proto3 guide 明确说，field number 在消息类型投入使用后不能改变，因为它在 wire format 中标识字段；官方 best practices 也强调不要复用 tag number，删除字段后要 reserved。

消息演进时，最基本的规则是：

```text
可以新增字段:
  老 reader 不认识就跳过或保留 unknown field；
  新 reader 读老数据时看到默认值或 unset。

不要改字段号:
  改字段号等价于删除旧字段并添加新字段，老数据会被误读或丢失。

不要复用删除字段的号:
  历史日志、缓存、队列、老服务里可能仍有旧字段数据。

删除字段要 reserved:
  reserved field number 防止未来误用；
  如果使用 JSON/TextFormat，也要考虑 reserved field name。

不要随意改字段类型:
  有些数值类型之间在二进制层面条件兼容，但工程上仍要谨慎；
  message type、string/bytes、repeated/scalar、map/repeated、oneof 变化都容易丢数据。

避免 required:
  proto3 没有 required；proto2 required 会让滚动升级和长期演进很痛苦。

enum 要有 0 值 UNSPECIFIED:
  老客户端看到新 enum 值时，行为要可控；
  不要让 0 表示一个有业务含义的真实状态。
```

service evolution 还要再往上一层看。`.proto` 里不仅有 message，还有 service method。新增 method 通常是安全的，老客户端不会调用它；删除 method、改 method 名、改 request/response 类型、改变错误语义或幂等语义，都会破坏客户端。很多时候，更稳的做法是新增字段或新增 method，而不是原地改变旧 method 的语义。

举个例子：

```proto
service TaskService {
  rpc CreateTask(CreateTaskRequest) returns (CreateTaskResponse);
}

message CreateTaskRequest {
  string name = 1;
  optional int32 priority = 2;
  reserved 3;
  reserved "old_owner";
}
```

如果后来要支持 `idempotency_key`，可以新增字段：

```proto
message CreateTaskRequest {
  string name = 1;
  optional int32 priority = 2;
  reserved 3;
  reserved "old_owner";
  string idempotency_key = 4;
}
```

但不能把 `name = 1` 改成 `id = 1`，也不能把旧的 `owner = 3` 删除后又拿 `3` 给 `idempotency_key`。历史消息里 tag 3 的字节还可能存在，新代码会按新含义读它，轻则 parse 失败，重则数据污染。

ProtoJSON 要更保守。官方 ProtoJSON 文档提醒，JSON 格式不支持 unknown fields，而且会把 field name 和 enum name 编进消息里，所以重命名字段、删除字段、改 enum 名比 binary wire format 更容易变成 breaking change。如果一个 API 对外暴露 ProtoJSON，就不能只按 binary protobuf 的兼容规则设计。

工程上还要有发布策略：

```text
先让 reader 支持新旧格式，再让 writer 写新字段；
新字段在所有关键 reader 支持前，不要让业务必须依赖它；
删除字段要经过弃用、停止写入、确认历史数据和消费者、reserved；
CI 做 breaking-change 检查，比如 buf breaking；
保留跨版本 golden data，覆盖老 writer + 新 reader、新 writer + 老 reader；
文档写清字段 presence、默认值、枚举未知值和幂等语义。
```

面试里可以这样答：

```text
protobuf 演进的底线是字段号稳定。可以新增字段，但不能改字段号、不能复用删除字段号，删除后要 reserved；不要轻易改字段类型、repeated/scalar、oneof 和 enum 语义；不要加 required。service 层面新增 method 通常安全，删除或改变旧 method 语义不安全。binary protobuf 对 unknown fields 更友好，ProtoJSON 不支持 unknown fields，所以对外 JSON 映射要更保守。真正落地要配合滚动升级顺序、breaking-change CI 和跨版本 golden test。
```

## Q016. 拦截器 interceptor 可以用于哪些横切逻辑？

**回答：**

interceptor 适合处理“和具体业务方法无关，但几乎每个 RPC 都要做”的逻辑。它类似 HTTP middleware 或 filter，但要记住它是在 RPC 调用路径上，不是连接管理工具。gRPC 官方 interceptor 文档也说，interceptor 适合实现不属于单个 RPC method 的通用行为；同时也提醒它是 per-call 的，不能用来管理 TCP 连接、端口或 TLS。

常见用途可以按方向分。

客户端 interceptor：

```text
注入 metadata:
  request id、traceparent、tenant id、locale、feature flag、幂等 key。

日志和指标:
  method、deadline、status code、attempt、payload size、latency、peer。

trace:
  创建 span，把 trace context 写入 outgoing metadata。

重试和超时策略:
  对明确幂等的方法做有限 retry；
  统一补 deadline，但不要覆盖调用方更短的 deadline。

熔断和限流:
  在本地按 method/tenant 保护下游，避免无意义请求继续发出。

故障注入:
  测试环境里按比例注入延迟、错误、取消。
```

服务端 interceptor：

```text
认证:
  读取 authorization metadata，校验 token；
  或读取 mTLS peer identity，映射 principal。

鉴权:
  根据 principal、method、resource、tenant、scope 做策略判断。

限流和配额:
  按租户、用户、method、调用来源计数，超出返回 RESOURCE_EXHAUSTED。

审计:
  记录谁在什么时候调用了哪个高风险 method，结果是什么。

日志、metrics、trace:
  统一打点，记录 status、duration、request size、response size。

panic/recover 和错误规范化:
  把未处理异常转成 INTERNAL，避免进程崩溃或泄露栈信息。

输入保护:
  检查 message size、metadata size、deadline 是否缺失或过长。
```

流式 RPC 要更小心。unary interceptor 通常包住一次 request/response；stream interceptor 可能要包装 `RecvMsg` 和 `SendMsg`，才能统计每条消息、限制流速、处理取消、注入 trace event。只在 stream 建立时检查一次权限不一定够，比如一个长时间 bidi stream 里后续消息带不同 resource id，就还要在业务层或包装后的 recv/send 路径继续校验。

interceptor 的顺序也很重要。比如服务端一般希望先做 panic recovery，再做 trace/logging，再做 authn/authz，再进入业务；客户端可能希望 trace 最外层看到完整耗时，retry 内层看到每次 attempt。顺序不同，指标含义会不同：日志记录的是逻辑调用一次，还是每次 retry attempt；缓存命中是否记为一次真实下游 RPC；限流是在 retry 前还是 retry 后执行。

不适合放 interceptor 的东西也要说清楚：

```text
不要把核心业务状态机塞进 interceptor；
不要在 interceptor 里做长时间阻塞操作；
不要用 interceptor 配 TLS、端口、resolver、连接池；
客户端认证 token 更适合 call credentials 时，不要强行用 interceptor 重造；
不要在日志 interceptor 里打印完整 payload，尤其是 PII、大对象和 token。
```

面试里可以这样答：

```text
interceptor 适合做跨 RPC 的横切逻辑，比如 metadata 传播、日志、metrics、trace、认证、鉴权、限流、配额、审计、panic recovery、错误规范化、故障注入和有限 retry。它分 client/server，也分 unary/stream；stream 场景可能要包装 send/recv。要注意 interceptor 是 per-call 机制，不负责 TCP/TLS/端口/连接池；顺序会影响日志、trace、retry 和限流的语义，核心业务逻辑不要藏在里面。
```

## Q017. 认证、鉴权、限流、trace context 如何在 RPC metadata 中传播？

**回答：**

gRPC metadata 是和一次 RPC 关联的旁路信息，底层通过 HTTP/2 headers 和 trailers 传输。官方 metadata 文档把它定义为 key-value 数据，用来携带认证凭据、trace 信息或自定义 header。key 是 ASCII，大小写不敏感；二进制值通常用 `-bin` 后缀；`grpc-` 前缀保留给 gRPC 自己使用。还要记住 header 大小有限，官方协议文档和 metadata 文档都提到 request headers 默认建议限制大约 8 KiB 量级。metadata 不是放大对象、用户档案或复杂策略的地方。

认证信息一般这样放：

```text
authorization: Bearer <token>
x-api-key: <key>              # 如果系统使用 API key
cookie / custom token header  # 特定网关场景
```

但 mTLS 身份通常不靠 metadata 传。客户端证书是在 TLS 握手里完成认证，服务端从连接的 peer information 里拿到证书身份，再映射成 principal。metadata 里的 `x-user-id`、`x-tenant-id` 只能作为声明，必须由 token、证书、网关签名或内部可信边界来证明。外部客户端能直接传的身份字段，默认都不可信。

鉴权信息不要简单等同于认证信息。认证回答“你是谁”，鉴权回答“你能不能做这件事”。常见做法是：

```text
token 里带 subject、issuer、audience、scope、tenant；
服务端 interceptor 校验 token 后得到 principal；
再根据 RPC method、resource id、tenant、scope、RBAC/ABAC policy 做授权；
授权结论不建议由上游随便塞一个 x-allowed=true 传下来。
```

内部多跳调用时，可以传播调用者身份，也可以传播服务身份。两者要分清：

```text
end-user identity:
  终端用户是谁，通常来自入口网关校验后的 token 或签名 header。

service identity:
  当前调用服务是谁，通常来自 mTLS、SPIFFE、workload identity。

delegation:
  A 代表用户 U 调 B，要让 B 知道 A 是服务调用方，U 是被代理的用户；
  不要把二者混成一个 user-id。
```

限流 metadata 常见字段包括 tenant、user、client id、method、region、priority、quota group。注意限流依据必须来自可信来源。比如 tenant id 可以来自已校验 token 的 claim，而不是直接信任客户端传来的 `x-tenant-id`。限流失败通常返回 `RESOURCE_EXHAUSTED`，可以在 trailers 或错误详情里带 retry-after、quota name、当前限制等信息，但不要把内部限流规则全暴露出去。

trace context 通常走标准 header。现在更通用的是 W3C Trace Context：

```text
traceparent
tracestate
baggage
```

有些系统还会用 B3、`grpc-trace-bin` 或厂商自定义 header。关键不是名字，而是要全链路一致：入口创建或接受 trace id，客户端 interceptor 注入 outgoing metadata，服务端 interceptor 提取并创建 server span，再把 context 传给下游 RPC、数据库、消息队列。baggage 要慎用，因为它会跨服务传播，容易泄露用户信息，也会撑大 header。

安全上要注意：

```text
metadata 会被很多日志、代理、trace 系统看见，不要放明文密码、长期 token、大对象或 PII；
authorization 要在日志里脱敏；
跨信任边界时要清理客户端传入的内部 header；
只允许网关或认证服务写入的 header，要签名或在 mTLS 内部链路中重建；
metadata size 超限会导致请求被拒绝，不能把业务 payload 塞进去。
```

面试里可以这样答：

```text
gRPC metadata 底层是 HTTP/2 headers/trailers，适合传认证凭据、trace context、tenant、request id、限流维度等小型上下文。认证常用 authorization bearer token 或 call credentials；mTLS 身份来自连接证书，不是普通 metadata。鉴权要在服务端根据已验证身份、method、resource、tenant 和 scope 决策，不能信任客户端自报。限流维度也要来自可信 claim。trace context 用 traceparent/tracestate/baggage 或团队统一格式传播，但 baggage 和 token 都要控制大小和日志脱敏。
```

## Q018. 大消息通过 RPC 传输有什么风险？

**回答：**

大消息不是“慢一点的小消息”。它会改变 RPC 的资源模型。gRPC over HTTP/2 协议里，每条 gRPC message 是 length-prefixed message：1 字节压缩标记、4 字节 message length，再跟 message bytes。虽然协议字段能表达很大的长度，但具体实现、代理、服务端和客户端通常都有更小的 message size、header size、flow-control、内存和超时限制。工程上不能把“协议能表示”理解成“系统适合这么传”。

风险可以分几层。

第一，内存峰值。很多 unary RPC 会在发送前把 request 完整序列化成一段 bytes，接收后再完整反序列化成对象。一个 200 MB 的对象，在客户端可能同时存在原始对象、序列化 buffer、压缩 buffer、HTTP/2 发送 buffer；服务端也类似。GC 语言里还会引入大对象分配、堆膨胀和 STW 压力。即使网络带宽够，进程内存也可能先出问题。

第二，延迟和 deadline。大消息序列化、压缩、传输、解压、反序列化都耗时。deadline 如果按普通小 RPC 设置，容易中途超时；deadline 放得过长，又会占住连接、线程、窗口和配额。更麻烦的是失败后重试：小请求重试一次成本不高，大请求重试一次可能重新上传或下载几百 MB。

第三，HTTP/2 flow control 和队头影响。大 DATA 流会消耗 stream window 和 connection window。单个 stream 的窗口耗尽时自己会停；连接级窗口耗尽时，其他 stream 也会受影响。底层 TCP 丢包时，这条连接上的所有 stream 都可能等缺失字节补齐。大消息和小延迟 RPC 混在同一条连接上，很容易把 p99 拉高。

第四，代理和网关限制。很多 L7 LB、ingress、service mesh、API gateway 对 request body、response body、header、stream duration 都有限制。某些代理虽然支持 HTTP/2，但对 gRPC trailers、RST_STREAM、flow control 或大 DATA frame 的行为不一定完全符合预期。大消息更容易踩到这些边界。

第五，压缩风险。压缩可以省带宽，但会增加 CPU 和内存。大对象如果可压缩，解压后体积可能远大于线上传输体积，形成 decompression bomb 风险。压缩还可能让错误恢复更难，因为必须等足够的压缩块才能解出数据。

第六，可靠性语义变差。大上传断在 80% 时，服务端到底处理了多少？大下载断在 80% 时，客户端能否断点续传？如果没有 chunk id、offset、checksum、commit 协议，只能从头再来。对非幂等写，这还会变成半成功问题。

更稳的设计通常是：

```text
小控制信息走 RPC；
大对象走 object store、文件服务或专门的数据通道；
RPC response 返回 object reference、size、checksum、version、expires_at；
上传用 chunk、offset、upload_id、checksum、commit；
下载支持 range、分页、streaming 或 resume；
服务端设置 max message size、max stream duration、rate limit 和 backpressure；
观测 request/response bytes、serialization time、compression ratio、flow-control stall。
```

当然，不是所有大一点的 payload 都禁止走 RPC。如果是几十 KB 或少量 MB 的配置、模型 metadata、批量查询结果，且有明确上限、deadline、压缩和监控，unary 或 streaming RPC 可以接受。问题出在没有上限，把“对象、文件、模型权重、图片、日志批次、查询全集”直接塞进一个 response。

面试里可以这样答：

```text
大消息走 RPC 的风险不只是带宽，而是内存峰值、序列化/压缩 CPU、deadline、重试成本、HTTP/2 flow control、连接级阻塞、代理限制和半成功语义。unary 大对象往往会在两端完整 materialize，多份 buffer 叠加后很容易把 GC 和内存打爆。更稳的做法是控制面走 RPC，大对象走对象存储或分块 streaming，带 upload id、offset、checksum、commit 和可恢复机制。
```

## Q019. 为什么大对象通常不直接放在 RPC response 中？

**回答：**

因为 RPC response 更适合表达“调用结果”和“小型结构化数据”，不适合承担大对象数据平面的职责。把大对象直接放进 response，看起来接口简单，实际会把存储、传输、重试、缓存、权限、观测和故障恢复都绑死在一次 RPC 生命周期里。

一个典型反例是：

```proto
message GetReportResponse {
  bytes pdf = 1;
}
```

如果 PDF 只有几十 KB，这没什么问题；如果它可能是 500 MB，就会很麻烦。服务端要在生成或读取后把它塞进 response，客户端要等完整 response 回来才能处理，连接要一直被占用。中途失败时，客户端不知道已经拿到的部分能不能复用，通常只能重试整个 RPC。重试又可能让服务端重新生成、重新读取、重新传输。

更常见的设计是：

```proto
message GetReportResponse {
  string object_uri = 1;
  string version = 2;
  int64 size_bytes = 3;
  string sha256 = 4;
  string content_type = 5;
  int64 expires_at_unix_ms = 6;
}
```

也就是 RPC 返回对象引用，而不是对象本体。真正的数据从对象存储、文件服务、CDN、range download 或专门的 streaming endpoint 取。这样做有几个好处。

第一，失败恢复更清楚。对象存储通常支持 range、etag、generation、checksum 和断点续传。下载到一半断了，可以按 offset 恢复，而不是重跑整个业务 RPC。

第二，资源隔离更好。生成报告的控制面服务不需要长期占着 gRPC stream 传大文件。对象传输可以交给更适合高吞吐、大带宽、缓存和限速的系统。控制面服务的 p99 不会被少数大下载拖坏。

第三，缓存和分发更自然。大对象可以被 CDN、对象存储 cache、边缘节点或本地缓存处理。RPC response 通常不适合被通用缓存层识别，也不适合跨客户端复用。

第四，权限和审计更细。返回 object reference 时，可以绑定短期签名 URL、租户、对象版本、访问范围和过期时间。下载服务可以单独记录谁下载了哪个对象、下载了多少、是否校验通过。直接塞在 RPC response 里，这些信息容易和业务调用日志混在一起。

第五，协议演进更稳。对象 metadata 可以新增字段，下载协议可以独立优化，比如从单次下载改成 range、多 part、压缩、加密、冷热分层，不需要改变核心 RPC 的响应语义。

也有例外。如果对象很小、强一致地依附于一次查询、客户端必须原子拿到它，而且有明确大小上限，可以直接放 response。比如头像缩略图、几十 KB 的配置快照、小型证书链。但接口要写清楚上限，服务端要 enforce。不要让一个本来是小对象的字段无限增长。

面试里可以这样答：

```text
大对象不直接放 RPC response，是因为它会把一次控制面调用变成长时间大数据传输：内存峰值高，失败后难以断点恢复，重试成本大，还会拖慢同一连接上的其他 RPC。更稳的做法是 response 返回对象引用、版本、大小、checksum、content type 和过期时间，大对象走对象存储、range download 或专门 streaming 通道。只有对象小且有明确上限时，才适合直接放 response。
```

## Q020. 如何处理 RPC 半成功：服务端已执行但客户端没收到响应？

**回答：**

这是 RPC 可靠性里最容易出事故的问题。客户端看到 timeout、connection reset、`UNAVAILABLE` 或 `DEADLINE_EXCEEDED`，只能说明“我没有拿到成功响应”，不能推出“服务端没有执行”。服务端可能已经写库、扣款、发消息、创建资源，只是响应在返回路上丢了，或者服务端在发送 response 前崩了。

先把几种状态分清：

```text
请求没离开客户端:
  可以安全重试。

请求到达代理但没到业务服务:
  可能可以重试，但客户端通常无法证明。

请求到达服务端但业务没开始:
  框架可能知道一部分，比如 REFUSED_STREAM 表示未处理。

业务已开始但未提交:
  需要服务端自己的事务和取消处理。

业务已提交但响应丢失:
  这就是典型半成功。
```

客户端通常无法靠网络错误区分这些状态，所以要从 API 设计上处理。核心做法是 idempotency key 加 result replay。

写请求由客户端生成一个稳定的 `operation_id` 或 `idempotency_key`：

```proto
message CreateOrderRequest {
  string idempotency_key = 1;
  OrderSpec spec = 2;
}
```

服务端收到后，先在持久化存储里记录这个 key 的状态，常见状态是：

```text
IN_PROGRESS:
  请求已接受，正在执行。

SUCCEEDED:
  已提交副作用，并保存了可重放的结果。

FAILED_RETRYABLE:
  未提交或可安全重试。

FAILED_FINAL:
  业务失败，重复请求应返回同一个失败结果。
```

同一个 key 再来时，服务端不能重新执行副作用，而是返回第一次执行的结果，或者返回“仍在处理中”。这就是 result replay。它比“客户端重试时服务端查重”更进一步：不仅去重，还要能把第一次成功的 response 再给客户端。

关键是提交顺序。对创建资源这类操作，服务端应该在同一个事务或同一个可靠状态机里完成：

```text
写入 idempotency record；
执行业务写入；
保存 response 摘要或 resource id；
把 idempotency record 标记为 SUCCEEDED；
再发送 RPC response。
```

如果业务写入和 idempotency record 分离，中间崩溃就会留下“资源创建了，但 key 状态不知道”的裂缝。支付、发消息、调用外部系统时，还要把外部系统也纳入幂等语义：传外部 payment id、message id、dedup key，或者用 outbox/inbox 模式确保“本地提交”和“对外发布”可恢复。

客户端处理逻辑也要变：

```text
第一次调用带 operation_id；
如果收到 OK，结束；
如果收到 DEADLINE_EXCEEDED/UNAVAILABLE/连接断开，不要换 key 盲目重试；
用同一个 key 重试同一个 RPC，或调用 GetOperation(operation_id) 查询状态；
如果服务端返回 IN_PROGRESS，按 backoff 轮询或等待通知；
如果返回 SUCCEEDED，使用保存的结果；
如果返回 FAILED_FINAL，按业务失败处理；
如果长时间 UNKNOWN，进入人工对账或补偿流程。
```

对于无法做到幂等的操作，要提供确认接口或状态资源。比如扣款后客户端没收到响应，正确做法不是“再扣一次试试”，而是查 `GetPayment(payment_id)` 或 `GetOperation(operation_id)`。如果查到成功，直接展示成功；查到不存在，再决定是否重新发起。对于发邮件、发短信、发 webhook，也要有 message id 和发送记录，避免重复发送。

取消也要讲清楚。客户端 deadline 到了或 context cancelled，不等于服务端自动回滚。服务端收到取消信号后可以停止尚未提交的工作，但如果副作用已经提交，就只能返回已有状态或让客户端之后查询。API 文档要明确 cancellation 的语义：best-effort cancel、cancel before commit、还是提交后不可取消。

这不是 exactly-once。更准确的说法是：

```text
网络层仍然是 at-least-once / maybe-once；
业务层通过 idempotency key、dedup、状态查询、result replay 和补偿，
把重复请求收敛成用户可理解的一次业务结果。
```

面试里可以这样答：

```text
RPC 半成功不能靠客户端猜。timeout 或 UNAVAILABLE 只说明客户端没拿到响应，不说明服务端没执行。写操作要设计 idempotency key 或 operation_id，服务端持久化请求状态和第一次执行结果；同一个 key 重试时返回 result replay，而不是重新执行副作用。客户端失败后用同一个 key 重试或查询 GetOperation，不要换 key 盲目再发。支付、消息、外部调用还要把外部 idempotency key、outbox/inbox、对账和补偿设计进去。这不是网络层 exactly-once，而是业务层把不确定结果收敛成可恢复状态机。
```

## Q021. RPC 的幂等性应该如何暴露给调用方？

**回答：**

幂等性不能只藏在服务端实现里。调用方要不要自动重试、遇到 `DEADLINE_EXCEEDED` 后能不能继续发、网关能不能切下一个 upstream、SDK 能不能打开 retry policy，都取决于它是否知道这个 RPC 重复执行以后会发生什么。gRPC over HTTP/2 协议文档里有一句很关键的话：除非显式定义，否则 gRPC call 不被假定为幂等；被标记为幂等的调用可能被发送多次。也就是说，默认心智应该是“不知道，不重试”，而不是“网络失败就再来一次”。

暴露幂等性有几层。

第一层是 API 语义和命名。查询类接口要让调用方一眼看出它没有副作用，例如 `GetTask`、`ListEvents`、`DescribeWorker`。写接口要避免靠名字制造错觉。`CreateOrder` 如果没有 idempotency key，它不是幂等的；`CreateOrder` 如果要求 `request_id` 并且服务端会 result replay，它才可以对同一个 key 幂等。更好的写法是把这个约束放进 request message：

```proto
message CreateTaskRequest {
  string idempotency_key = 1;
  TaskSpec spec = 2;
}

message CreateTaskResponse {
  string task_id = 1;
  bool replayed = 2;
}
```

`replayed` 不是必须字段，但面试里可以说这个字段有时很有用。它能让调用方和排障人员知道，这次返回的是第一次执行结果，而不是重新执行了一次副作用。

第二层是 `.proto` 注释和 API 文档。不要只写“支持重试”，要写清楚粒度：

```text
同一个 idempotency_key + 同一份业务参数:
  重复调用返回同一个业务结果。

同一个 idempotency_key + 不同业务参数:
  返回 ALREADY_EXISTS、FAILED_PRECONDITION 或 INVALID_ARGUMENT，不能悄悄按新参数执行。

key 的保存时间:
  例如 24 小时、7 天，或跟资源生命周期一致。

结果重放范围:
  重放 task_id、operation status、错误详情，还是只保证不会重复执行副作用。

失败状态:
  超时后应该重试同一个 key，还是查询 GetOperation。
```

第三层是机器可读配置。gRPC service config 可以按 service/method 配置 timeout、retry policy、hedging policy、wait-for-ready 和 load balancing。服务拥有方如果给某个 method 下发 retry policy，本质上就是在向客户端声明“这个方法在这些 status code 下可以被自动重试”。这必须和 API 语义一致。不要因为 `UNAVAILABLE` 看起来像瞬时错误，就给所有写接口开 retry。

第四层是 SDK 行为。手写客户端或生成 SDK 里，可以把幂等性变成类型和选项：

```text
纯读方法:
  默认允许有限 retry。

需要 idempotency key 的写方法:
  SDK 自动生成 key，或强制调用方传 key。

非幂等写方法:
  SDK 默认不开 retry，只允许调用方显式确认。

长操作:
  返回 operation_id，SDK 提供 WaitOperation / GetOperation。
```

第五层是错误码约定。幂等请求重复到达时，不要每个服务随便返回。常见约定是：第一次成功后重复请求返回同一个 OK response；同 key 但参数冲突返回 `ALREADY_EXISTS` 或 `FAILED_PRECONDITION`；还在执行返回 `ABORTED`、`UNAVAILABLE`、`RESOURCE_EXHAUSTED` 或一个明确的 operation 状态，取决于 API 设计。关键是调用方要知道下一步是“继续等同一个 operation”，不是“换个 key 再创一次”。

面试里可以这样答：

```text
RPC 的幂等性要在接口契约里显式暴露：proto 注释、API 文档、idempotency_key 字段、operation_id、错误码语义、SDK 默认 retry 行为和 service config retry policy 要一致。纯读可以声明 no side effects；幂等写要说明 key 的作用域、保存时间、参数冲突行为和 result replay；非幂等写默认不允许自动重试。调用方真正需要的不是一句“可重试”，而是知道重复发送同一个请求时服务端会返回什么、是否会重复副作用。
```

## Q022. 负载均衡和长连接粘性会如何影响 worker 分布？

**回答：**

gRPC 的连接复用会让负载均衡问题比普通短请求更微妙。HTTP/2 multiplexing 让一个 channel 上可以同时跑很多 RPC；这减少了握手和连接数，但也意味着“连接分布均匀”不等于“RPC 分布均匀”，更不等于“worker 负载均匀”。

先看 L4 负载均衡。L4 LB 通常按 TCP 连接选择后端。如果一个客户端进程只建一条 gRPC 长连接，那么这条连接一旦落到某个后端 worker，后续大量 unary RPC 和 streaming RPC 都可能继续压在同一个 worker 上。LB 看起来做了均衡：100 个客户端连接分布到 10 个后端，每个后端 10 条连接。但如果其中 3 个客户端是大流量客户端，它们所在的 worker 就会被打满，其他 worker 很闲。

这个现象可以叫连接粘性，也可以叫长连接导致的负载倾斜。它常见于：

```text
少量高 QPS 客户端；
每个客户端复用单 channel；
L4 LB 只在连接建立时选后端；
streaming RPC 持续时间很长；
worker 处理能力差异较大；
客户端没有 round_robin 或 xDS 策略。
```

L7 代理能缓解一部分。因为 L7 能理解 HTTP/2 stream 和 gRPC method，理论上可以把同一条客户端连接上的不同 RPC stream 转发给不同后端。这样按 RPC 粒度更均匀。但它也不是万能的。已经开始的 streaming RPC 不能中途迁移；代理自己也会有 upstream 连接池、流控、超时和 buffering。L7 多一跳，还可能引入新的 p99。

client-side LB 更适合内部 gRPC。客户端通过 resolver 拿到多个 endpoint，维护多个 subchannel，每次 RPC 由 picker 选择后端。`round_robin` 会比 `pick_first` 更容易分散请求；加权策略、locality-aware、backend metrics 可以继续改善分布。gRPC custom load balancing 文档里也说，LB policy 负责维护到 server 的连接，并在 RPC 发送时选择连接。这是 gRPC 长连接模型里更自然的均衡点。

worker 分布还要看“worker”的含义。如果一个后端实例内部有多个 worker 线程或 goroutine，连接被均衡到实例后，实例内部还要把 stream 分配给执行单元。常见问题是 accept loop、completion queue、event loop 或应用队列有粘性：某些连接被固定在某个线程上，大 stream 持续占用该线程，导致局部排队。这个时候外部 LB 再均匀也没用，实例内部已经倾斜。

排查时不要只看总 QPS，要看分布：

```text
每个 backend 的 active streams、QPS、p99、inflight；
每个 client channel 的 subchannel 状态和 picked backend；
每个 worker 的队列长度、处理耗时、CPU、发送/接收字节；
长流数量和持续时间；
连接数与 RPC 数的比例；
pick_first、round_robin、xDS、L4/L7 的实际策略。
```

解决办法也分层：

```text
内部服务优先使用 client-side LB 或支持 gRPC 的 L7；
避免所有流量通过单 channel + L4 连接级均衡；
对高负载业务拆 channel 或拆 target；
长流和短 unary 分离，避免长流占住同一批连接；
后端做 graceful draining，让老连接逐步退出；
必要时用 max connection age 打散长期粘住的连接；
实例内部避免把连接永久绑定到单个执行 worker。
```

面试里可以这样说：

```text
gRPC 长连接会让 L4 负载均衡退化成连接级均衡。一个高流量客户端如果只建一条 HTTP/2 连接，后续很多 RPC 都会落到同一个后端 worker，造成 worker 分布倾斜。L7 proxy 可以按 stream/RPC 分发，client-side LB 可以按每次 RPC 选择 subchannel，通常更适合内部 gRPC。排查时要看 backend/worker 级 QPS、active streams、队列、长流数量和 picker 结果，而不是只看整体 QPS。
```

## Q023. DNS 解析和连接生命周期会如何影响故障切换？

**回答：**

DNS 解析不是每次 RPC 都发生。很多人以为后端 IP 从 DNS 里摘掉后，客户端下一次调用自然会去新 IP；gRPC 长连接下经常不是这样。客户端通常在 channel 创建、连接建立、resolver 刷新或连接失败时解析目标，然后维护已有连接。只要连接还处于 READY，很多实现不会因为 DNS TTL 到了就立刻断开旧连接。

gRPC custom name resolution 文档里提到一个现实差异：标准 DNS 往往是在连接开始时查地址，并在连接生命周期内维持连接；watch-based resolver 可以随时间收到更新，更聪明地响应后端故障、扩容和缩容。这句话对故障切换很重要。DNS 适合简单发现，但它不是实时控制面。

故障切换会被几个时间叠加影响：

```text
DNS TTL:
  客户端或本地 DNS cache 多久刷新一次。

resolver refresh:
  gRPC resolver 多久重新解析，是否支持 watch。

connection state:
  channel 当前是 READY、IDLE、CONNECTING 还是 TRANSIENT_FAILURE。

TCP failure detection:
  对端断电、丢包、防火墙丢弃时，TCP 可能很久才发现。

keepalive:
  HTTP/2 PING 可以更早发现半开连接，但配置太激进会有副作用。

LB policy:
  pick_first 可能一直使用第一个可连地址；
  round_robin 会维护多个 subchannel；
  xDS 可以更快下发 endpoint 和健康状态变化。

health checking:
  gRPC client health checking 可以让客户端不再向 NOT_SERVING 的后端发请求。
```

一个常见故障场景是：后端 A 已经从 DNS 里删除，但客户端有一条到 A 的 HTTP/2 连接仍然存活。DNS 已经“正确”，请求却还在发给 A。另一个场景是：A 已经宕机，但网络中间设备没有返回 RST，只是丢包；客户端不写数据就不知道，写了也可能等到 TCP 超时。没有 deadline 和 keepalive 时，故障切换会很慢。

`wait-for-ready` 也会影响体验。官方 wait-for-ready 文档说，开启后，如果 channel 暂时无法连接，RPC 会等待连接 ready，而不是立即失败；deadline 仍然生效。这适合批处理、启动期间依赖服务还没起来的场景。但在线请求如果盲目打开 wait-for-ready，可能把失败变成长时间排队，p99 看起来像“服务慢”，实际是客户端等连接。

更稳的做法是：

```text
所有 RPC 有 deadline；
连接层用合理 keepalive 探测半开连接；
内部服务用 xDS/watch-based resolver 或服务发现，而不只依赖 DNS TTL；
LB policy 根据场景选择 round_robin、weighted、locality-aware，而不是默认 pick_first 到底；
服务端 shutdown 前先 health -> NOT_SERVING，再 drain，再关闭；
客户端观测 resolver update、subchannel state、connection failure、picked endpoint；
对跨公网或移动网络，明确 DNS cache、连接重建和失败重试策略。
```

面试里可以这样答：

```text
DNS 更新不等于 gRPC 立刻切流。gRPC channel 会复用已有 HTTP/2 连接，标准 DNS resolver 往往只在连接开始或刷新时解析；旧连接处于 READY 时，请求可能继续发往已从 DNS 摘掉的后端。故障切换速度取决于 DNS TTL、resolver 刷新、连接状态、TCP 失败检测、keepalive、LB policy、health checking 和 deadline。生产上不能只靠 DNS，要配合 watch-based resolver/xDS、健康检查、graceful draining、合理 keepalive 和每次调用 deadline。
```

## Q024. 如何排查 gRPC p99 延迟升高？

**回答：**

排查 p99 不能先盯平均值。平均值正常、p99 飙高，说明只有一部分请求卡住。gRPC 的路径比较长：客户端排队、picker 选连接、DNS/resolver、连接建立、TLS、HTTP/2 stream、flow control、序列化、网络、服务端排队、业务处理、响应序列化、返回路径、重试和 hedging。要先把一次 logical call 拆成阶段，否则很容易把所有问题都归到“网络慢”。

我会按这个顺序查。

第一，看范围。是所有 method 都慢，还是某几个 method 慢；是所有 client 慢，还是某个 region、某个版本、某个 LB target 慢；是 p50/p90/p99 一起升，还是只有 p99 升。所有 method 一起慢，更像连接、网络、代理、客户端线程池、证书、DNS、LB 或服务端资源；单 method 慢，更像业务、序列化、大消息或下游依赖。

第二，看 status code 和 deadline。`DEADLINE_EXCEEDED` 上升，可能是服务端处理慢，也可能是客户端排队、连接不可用、retry delay 消耗了预算。`UNAVAILABLE` 上升，要看连接失败、GOAWAY、RST_STREAM、健康检查、LB 没有可用 endpoint。`RESOURCE_EXHAUSTED` 上升，可能是限流、连接并发 stream 上限、队列满或 flow control。

第三，把 client call 和 attempt 分开。gRPC OpenTelemetry metrics 里区分 client per-call、client per-attempt、retry/hedging 指标。一次逻辑调用如果发生多次 attempt，call duration 会变长，但单次 attempt 未必慢。要看 attempt count、retry delay、per-attempt status、picked backend、sent/received compressed message size。否则会把 retry storm 误判成服务端慢。

第四，看连接和负载均衡。检查 resolver 结果有没有变、subchannel 是否频繁从 READY 变成 TRANSIENT_FAILURE、pick_first 是否粘到某个慢后端、round_robin 是否有 endpoint 健康状态异常。grpcdebug/Channelz 可以查看 channel、subchannel、socket 和地址解析状态；官方 debugging 文档也把 grpcdebug 定位为查看 Channelz、Health、CSDS 等内部状态的工具。

第五，看 payload。大 request/response、压缩开启、ProtoJSON 转换、反射/动态 message、日志打印完整 payload，都可能让 p99 上升。OpenTelemetry 的 sent/received compressed message size 能帮助判断是不是大消息集中在尾部。

第六，看服务端。服务端要分清 transport 收到请求到应用 handler 开始之间的排队、handler 执行时间、下游依赖时间、发送响应时间。Go 里还要看 goroutine 堆积、锁、GC、CPU throttling；Java 看 GC、executor queue；C++ 看 completion queue、线程数。服务端 p99 正常但客户端 p99 高，问题更可能在客户端排队、网络、代理或重试。

第七，看代理和入口。Ingress、Envoy、NGINX、service mesh 都可能有自己的 timeout、buffer、stream limit、连接池、重试和 circuit breaking。代理的 p99 和 upstream p99 要分开。特别是 gRPC streaming，很多代理的 idle timeout、read timeout、max stream duration、max concurrent streams 配置会直接影响尾延迟。

一个实用的排查表：

```text
client:
  call duration、attempt duration、retry delay、queued before pick、serialization time、blocked write。

network / transport:
  connect time、TLS handshake、RST_STREAM、GOAWAY、flow-control stall、TCP retransmit。

LB / resolver:
  picked backend、subchannel state、resolver update、health status、endpoint weight。

server:
  queue time、handler time、downstream time、response send time、CPU/GC/lock/worker queue。

proxy:
  downstream duration、upstream duration、timeout、reset reason、buffer/flow-control counters。
```

面试里可以这样答：

```text
排查 gRPC p99 要先拆阶段，不要直接说网络慢。我会先按 method、client、region、status code 缩小范围，再把 logical call 和 attempt 分开，看 retry/hedging、deadline、payload size、picked backend、subchannel 状态。然后对比客户端 duration、服务端 handler duration、代理 upstream duration 和网络/连接指标。如果服务端快但客户端慢，看排队、连接、LB、flow control、代理和重试；如果服务端也慢，再查业务队列、下游依赖、GC、锁和大消息序列化。
```

## Q025. 如何区分网络延迟、服务端排队、序列化成本和客户端阻塞？

**回答：**

要区分这些成本，靠一个 `grpc.client.call.duration` 不够。它是端到端结果，只告诉你“客户端等了多久”。要把时间戳和指标埋在不同边界上：应用调用前、序列化前后、gRPC attempt 开始、subchannel pick、header 发出、服务端收到、handler 开始、handler 结束、响应序列化、客户端收到、反序列化完成。并不是每种语言都能直接拿到所有内部时间点，但思路是一样的。

网络延迟通常表现为：客户端 attempt duration 高，服务端 handler duration 不高，服务端排队也不高；同 region 正常，跨 region 或跨公网慢；TCP retransmit、丢包、RTT、TLS handshake、代理 upstream connect time 异常。对 streaming，可能还会看到 write 阻塞和 flow-control stall。网络问题还常伴随 `UNAVAILABLE`、`DEADLINE_EXCEEDED`、RST_STREAM、GOAWAY 或连接重建。

服务端排队表现为：请求已经到达服务端 transport，但业务 handler 迟迟没开始，或者 server interceptor 记录的入口时间和 handler 开始时间之间有明显间隔。原因可能是 worker pool 满、线程池队列长、CPU 饱和、锁竞争、限流等待、数据库连接池等待。服务端排队时，网络 RTT 未必高；同一个 backend 上所有 method 或某一类 method 的 p99 会一起升。

序列化成本表现为：payload size 变大、CPU 升高、handler 业务时间不长但 encode/decode 时间长。客户端在调用前构造 request 慢，或者 gRPC write 前 marshal 慢；服务端 handler 已经结束，但 response 发出前卡住。protobuf binary 一般较快，但大 repeated 字段、map、深层 message、压缩、ProtoJSON、动态反射、日志打印完整对象都会让尾部变慢。OpenTelemetry 的 sent/received compressed message size 可以辅助判断，但最好在应用层单独记录 marshal/unmarshal 耗时。

客户端阻塞表现为：服务端根本没收到请求，或者收到时间很晚；客户端本地线程池、事件循环、连接并发 stream 上限、channel pick 队列、DNS 解析、连接建立、wait-for-ready、retry backoff 都可能让 RPC 在客户端侧等待。官方 performance 文档提到，当单个 HTTP/2 连接达到并发 stream 限制，额外 RPC 会在客户端排队。这个现象很容易被误判成服务端慢。

一套比较清楚的埋点可以这样做：

```text
client_app_start:
  业务代码准备发起调用。

client_marshal_done:
  request 序列化完成。

client_attempt_start:
  gRPC attempt 开始，记录 target、method、deadline。

client_headers_sent:
  请求头或首个 message 交给 transport。

server_transport_in:
  服务端 transport 收到 headers。

server_handler_start:
  业务 handler 开始。

server_handler_done:
  业务 handler 完成。

server_response_ready:
  response 序列化完成。

client_response_done:
  客户端收到并反序列化完成。
```

然后用差值判断：

```text
client_marshal_done - client_app_start:
  客户端构造和序列化。

server_transport_in - client_headers_sent:
  网络、代理、TLS、排队在传输层的成本。

server_handler_start - server_transport_in:
  服务端接入层和 worker 排队。

server_handler_done - server_handler_start:
  业务处理和下游依赖。

server_response_ready - server_handler_done:
  服务端序列化。

client_response_done - server_response_ready:
  返回网络、客户端接收、反序列化。
```

现实里时钟不同步会影响跨机器差值，所以分布式 trace 要配合 NTP/PTP，并尽量用同一侧可测的指标交叉验证。比如客户端看到 800ms，服务端只记录 40ms，代理 upstream 也只有 50ms，那剩下的时间大概率在客户端排队、连接建立、DNS、retry delay 或代理 downstream。不要只靠一条 trace 下结论，要看同类请求的分布。

面试里可以这样答：

```text
区分这些成本要把 RPC 拆成阶段埋点。网络慢通常是 client attempt 高、server handler 不高，并伴随 RTT、重传、连接或代理指标异常；服务端排队是 server transport 收到后到 handler 开始之间变长；序列化成本看 marshal/unmarshal 时间和 payload size；客户端阻塞看请求是否还没发出，常见原因是 channel pick、连接建立、并发 stream 上限、wait-for-ready、retry backoff 或本地线程池。只有端到端 duration 不够，必须结合 trace、client/server metrics、Channelz 和代理日志。
```

## Q026. 流式 RPC 中 backpressure 如何传递？

**回答：**

流式 RPC 的 backpressure 是从接收端一路传回发送端的。gRPC 官方 flow control 文档说，flow control 的目标是避免快发送方压垮慢接收方；当接收侧读取数据后，会把可用容量反馈给发送方；发送太快时，gRPC framework 可能在 write 调用上等待。这里有一个容易忽略的点：应用写入 stream，只代表把消息交给 gRPC runtime，不代表字节已经发到网络上，更不代表对端应用已经处理。

传递链路大致是：

```text
receiver application 慢读
-> receiver gRPC runtime 不释放 stream window
-> HTTP/2 WINDOW_UPDATE 变慢
-> sender gRPC runtime 可发送窗口变小
-> sender write 阻塞、future 不完成、onReady 变 false，或内部 buffer 增长
-> sender application 感知到不能继续无限写
```

HTTP/2 有 stream-level 和 connection-level 两层 flow control。stream-level 保护单个 stream，避免某个接收方被该 stream 压垮；connection-level 保护整条连接。如果一个大 stream 把连接级窗口耗尽，同一条连接上的其他 stream 也会被影响。这就是为什么大 streaming 和低延迟 unary 混在同一条 channel 上时，尾延迟可能变差。

不同语言暴露 backpressure 的方式不同。同步 API 可能是在 `Send` 或 `Write` 上阻塞；异步 API 可能是 `Write` future 不完成；Java 有 `isReady/onReadyHandler`；有些语言允许手动 flow control，应用要主动 request 下一条消息。无论接口形式如何，原则都一样：不要在应用层无限读入、无限排队、无限写给 gRPC runtime。

常见坑有几个。

第一，只写不读导致死锁。官方 flow control 文档也警告，如果客户端和服务端都用同步读写或手动 flow control，并且双方都大量写但不读，就可能死锁。双向流里尤其常见：客户端等自己写完所有请求再读响应，服务端也等自己写完所有响应再读下一批请求，双方窗口都被对方卡住。

第二，把 runtime buffer 当成业务队列。调用 `Send` 返回快，不代表对端处理快。如果应用在上游继续无限生产，最终内存会堆在业务队列、gRPC buffer 或 OS socket buffer 里。正确做法是把 backpressure 接回业务生产者，比如暂停读取文件、暂停消费 Kafka、降低扫描速度。

第三，忽略消息粒度。gRPC flow control 控制的是字节，不理解业务消息成本。一条小消息可能触发很重的业务处理；一条大消息可能只是存储转发。业务层还要有自己的 credit、ack、batch size、inflight limit。

第四，代理会参与流控。Ingress、Envoy、NGINX 或 service mesh 可能终止 HTTP/2，再建立 upstream HTTP/2。它们有自己的窗口、buffer 和超时。客户端以为服务端慢，实际可能是代理 downstream 或 upstream 的 flow control 卡住。

设计上可以这样做：

```text
限制每个 stream 的 inflight 消息数和字节数；
按 onReady / write completion 再生产下一批；
读写协程分离，避免双方只写不读；
大对象用 chunk + ack + checksum；
业务层定义 credit 或 window；
监控 send queue、recv queue、write blocked time、message size、stream duration；
长流和短调用分 channel，降低连接级 flow control 影响。
```

面试里可以这样答：

```text
流式 RPC 的 backpressure 从接收方读速率传到 HTTP/2 flow-control window，再传到发送方的 gRPC write。应用写入 stream 不代表已经发到网络，更不代表对端处理完；发送过快时，write 会阻塞、future 不完成或 onReady 变 false。双向流要避免双方只写不读导致死锁。工程上要限制 inflight、按写完成或 credit 继续生产，把 backpressure 接回业务源头，而不是让 gRPC buffer 无限吸收。
```

## Q027. HTTP/2 head-of-line blocking 和 TCP 层 head-of-line blocking 有什么区别？

**回答：**

HTTP/2 解决的是 HTTP/1.1 应用层的队头阻塞，但没有解决 TCP 层的队头阻塞。这个区别面试里经常被追问。

HTTP/1.1 里，如果一个连接上按顺序发多个请求，响应也很难乱序返回。前面的慢响应会挡住后面的快响应，这叫应用层 head-of-line blocking。浏览器和客户端过去常用多条 TCP 连接绕开它，但连接多又带来握手、拥塞窗口、服务端 fd 和公平性问题。RFC 9113 的 HTTP/2 介绍也解释了这个动机：HTTP/2 允许在同一连接上 interleave messages，并用更高效的 header 编码，减少 HTTP/1.x 的并发问题。

HTTP/2 的做法是把每个请求/响应放进 stream，再把不同 stream 的 frame 交错发送：

```text
stream 1: HEADERS, DATA, DATA
stream 3: HEADERS, DATA
stream 5: HEADERS, DATA, DATA

on the wire:
  s1 HEADERS, s3 HEADERS, s1 DATA, s5 HEADERS, s3 DATA, s5 DATA ...
```

这样一个慢响应不会在 HTTP 层完全挡住另一个快响应。gRPC 的 unary 和 streaming 都利用了这个能力。

但底层还是 TCP。TCP 提供有序字节流。如果某个 TCP segment 丢了，接收端即使收到了后面的字节，也不能把缺口后的数据交给上层 HTTP/2。结果是这条 TCP 连接上的所有 HTTP/2 stream 都要等缺失字节重传。RFC 9113 也明确说，HTTP/2 没有解决 TCP head-of-line blocking。

所以区别是：

```text
HTTP/1.1 / HTTP 层 HOL:
  慢请求或慢响应挡住同连接后续请求/响应；
  HTTP/2 multiplexing 通过 stream frame 交错发送缓解它。

TCP 层 HOL:
  同一 TCP 字节流中一个 segment 丢失，后续字节不能交给上层；
  HTTP/2 无法绕过，因为所有 stream 共享这条 TCP 连接。
```

工程影响很实际。HTTP/2 单连接很省，但在丢包网络、跨公网、移动网络或长肥链路上，一次丢包可能让同连接所有 gRPC stream 抖一下。如果同一连接上混着大下载和低延迟控制 RPC，大流量更容易触发拥塞和重传，小 RPC 的 p99 也会被牵连。

缓解手段有几个：

```text
长流/大消息和低延迟 unary 分 channel；
在高并发或长流场景建立多个 channel/connection；
控制单连接 active streams 和大消息大小；
跨公网设置更保守的 deadline、retry 和 hedging；
关注 TCP retransmit、RTT、拥塞窗口、flow-control stall；
可以评估 HTTP/3/QUIC，但那是另一套部署和兼容性问题。
```

面试里可以这样答：

```text
HTTP/2 multiplexing 解决的是 HTTP 层队头阻塞：不同请求响应用 stream 分开，frame 可以交错发送，慢 stream 不应该在应用层挡住快 stream。但 HTTP/2 仍跑在 TCP 上，TCP 是有序字节流，一个 segment 丢失时，缺口后面的字节不能交给 HTTP/2，导致同一 TCP 连接上的所有 stream 都受影响。这就是 TCP 层 HOL。生产上大流和低延迟 RPC 混用一条连接时，p99 可能被 TCP 丢包和连接级流控拖高。
```

## Q028. gRPC over ingress/proxy 时可能遇到哪些超时和流控问题？

**回答：**

gRPC 经过 ingress、API gateway、Envoy、NGINX 或 service mesh 后，链路不再是简单的 client -> server。中间代理可能终止 TLS、终止 HTTP/2、重新建立 upstream 连接、改写 metadata、执行重试、做限流、做 buffer、做健康检查。每一层都有自己的 timeout 和 flow-control。很多线上问题不是 gRPC 本身慢，而是某个代理配置和 RPC 语义不匹配。

先看超时。至少有这些时间：

```text
client deadline:
  调用方愿意等多久，应该沿 RPC 链路传播。

ingress downstream idle timeout:
  客户端到 ingress 长时间无数据，连接或 stream 被关。

ingress upstream connect timeout:
  ingress 连接后端花太久。

upstream read timeout:
  后端太久没有返回 headers 或 DATA。

upstream send timeout:
  ingress 向后端发送 request 太久没有进展。

max stream duration / route timeout:
  代理对单个 HTTP/2 stream 的总时长限制。

backend deadline:
  服务端自己或下游依赖的 deadline。
```

NGINX 的 gRPC 模块文档里就有 `grpc_connect_timeout`、`grpc_read_timeout`、`grpc_send_timeout`、`grpc_next_upstream_timeout`、`grpc_next_upstream_tries` 等配置。注意 `grpc_read_timeout` 和 `grpc_send_timeout` 这类通常是两次读写之间的 idle 时间，不一定是整个 RPC 的总时长。长时间 server streaming 如果中间没有任何 message 或 heartbeat，就可能被 read timeout 关掉。

再看重试。代理可能在 `grpc_next_upstream` 或 Envoy route retry policy 下重试 upstream。问题是代理未必知道业务幂等性。NGINX 文档里对 `non_idempotent` 有明确提醒：通常非幂等方法在请求已发送到 upstream 后不会再切下一个 server，显式打开才会这么做。gRPC 大多数 method 在 HTTP 层都是 POST，如果代理按 HTTP POST 保守处理，可能少重试；如果强行打开，可能重复副作用。

流控问题也常见。代理两边各有一套 HTTP/2 连接：

```text
client <-> ingress:
  downstream HTTP/2 window、max concurrent streams、buffer。

ingress <-> backend:
  upstream HTTP/2 window、连接池、max concurrent streams、buffer。
```

如果客户端读响应很慢，ingress downstream 窗口释放慢，ingress 可能暂停从 backend 读，backend 的 upstream window 又被卡住。反过来，大上传时 backend 慢读，ingress upstream 写不动，client 到 ingress 也会被 backpressure 影响。代理如果做过多 buffering，会把 backpressure 暂时藏起来，最后变成代理内存暴涨或突然 reset。

还有协议细节。gRPC status 在 trailers 里。代理如果不正确处理 trailers、把非 200 HTTP status 直接映射、丢掉 `grpc-status-details-bin`、修改 `content-type`，客户端看到的错误就会失真。L7 代理还可能限制 header list size，metadata 太大时请求被拒绝。

排查时要拿到三段日志：

```text
client:
  deadline、status、attempt、sent/received bytes、peer。

proxy:
  downstream duration、upstream duration、route timeout、reset reason、retry count、upstream host。

server:
  receive time、handler time、status、cancel reason、bytes。
```

生产配置上，我会明确几条：

```text
代理 timeout 要大于合理业务 deadline，但不能无限；
长流要有 heartbeat 或应用层进度消息；
代理重试只给幂等方法或明确 idempotency key 的方法；
metadata/header size 有上限；
max concurrent streams、buffer、flow-control window 要和业务负载匹配；
保留 trailers 和 grpc-status，不要把 gRPC 错误改成普通 HTTP 错误页；
灰度时同时看代理和客户端的 reset/timeout 指标。
```

面试里可以这样答：

```text
gRPC 过 ingress/proxy 时，最常见问题是多层 timeout 不一致、代理重试不了解业务幂等性、HTTP/2 flow control 被代理拆成 downstream/upstream 两段、trailers 或 grpc-status 被处理错。长流如果很久没消息，可能被 read/idle timeout 断开；大上传/大下载可能被代理 buffer、窗口和 max concurrent streams 卡住。排查要同时看 client、proxy、server 三段时延和 reset reason，不能只看服务端日志。
```

## Q029. 为什么跨公网 RPC 需要更严格的重试和超时策略？

**回答：**

跨公网比机房内调用不稳定得多。你要面对更高 RTT、丢包、抖动、NAT、运营商链路、TLS 终止、企业代理、移动网络切换、DNS 污染或缓存、客户端版本不可控。内网里 20ms deadline 可能合理，跨公网就可能把正常抖动当故障；内网里一次 retry 成本可控，跨公网大量客户端同时 retry 可能直接把入口打成雪崩。

更严格不是“timeout 更短、retry 更多”。恰恰相反，策略要更克制、更有边界。

第一，deadline 要分层。客户端要有总 deadline；每次 attempt 要有 per-attempt timeout；重试 backoff 要消耗总预算，不能每次重试重新拿满时间。gRPC service config 支持 method 级 timeout、retry policy、hedging policy，这些应该按 API 语义和公网 RTT 分布配置，而不是全局一个默认值。

第二，retry 要按幂等性打开。跨公网更容易出现 response 丢失、连接断开、代理 502/504、移动网络切换。对于非幂等写，如果没有 idempotency key，自动重试比失败更危险。对于纯读或幂等写，可以有限 retry，但要有指数 backoff、jitter、max attempts、retry throttling。故障时所有客户端同时重试，jitter 是基本要求。

第三，hedging 要更谨慎。hedging 是同时或延迟发起多个 attempt，取先返回的结果。它能降低尾延迟，但会真实放大流量。跨公网入口如果对高 QPS 方法开 hedging，很容易在下游慢的时候制造双倍压力。只适合纯读、无副作用、容量充足且有全链路限流的场景。

第四，错误分类要更细。公网常见错误包括 DNS 失败、connect timeout、TLS handshake timeout、HTTP/2 GOAWAY、RST_STREAM、proxy 502/503/504、client cancel、deadline exceeded。不同错误的处理不同。比如 connect timeout 可以重试另一个地址；`RESOURCE_EXHAUSTED` 说明配额或限流，不应该马上重试；`UNAUTHENTICATED` 重试网络没有意义；`DEADLINE_EXCEEDED` 对写操作不能盲目再发。

第五，保护服务端。跨公网客户端不可控，必须在入口做限流、并发限制、header/body size 限制、最大 stream 时长、认证失败快速返回、连接数限制和慢客户端保护。否则重试策略再漂亮，也会被恶意或异常客户端绕过。

第六，可观测要面向 attempt。公网问题往往发生在连接和 attempt 层。要看：

```text
DNS latency；
connect latency；
TLS handshake latency；
request bytes / response bytes；
per-attempt status；
retry count；
retry delay；
client region / ASN / network type；
proxy reset reason；
server processing time。
```

面试里可以这样答：

```text
跨公网 RPC 的不确定性更高，客户端和网络也更不可控，所以重试和超时要更严格地按语义设计。每个调用要有总 deadline，每次 attempt 要受总预算约束；retry 只给纯读或有 idempotency key 的写，使用 backoff、jitter、maxAttempts 和 throttling；hedging 只给无副作用读。错误码要区分网络、认证、限流、deadline 和服务端失败。入口还要有并发、大小、stream 时长和慢客户端保护，避免公网重试风暴把服务打垮。
```

## Q030. 如何为 RPC API 做兼容性测试？

**回答：**

RPC API 的兼容性测试要覆盖两层：schema 兼容和行为兼容。只跑 “新 client 调新 server” 没什么意义，线上真正危险的是滚动升级、回滚、灰度、多语言 SDK、老消息重放和代理链路。protobuf 给了演进规则，但规则不会自动阻止人改坏语义，所以测试要把这些规则变成 CI gate。

schema 层先做静态检查：

```text
字段号不能改；
删除字段必须 reserved；
删除 enum value 必须 reserved；
不能复用 tag number；
不能把字段改成不兼容类型；
不能随意 scalar <-> repeated；
不能把旧字段塞进 oneof；
不能删除或重命名对外 method；
ProtoJSON 对外时，字段名和 enum 名重命名要按 breaking change 处理。
```

这类检查适合用 breaking-change 工具做，比如 buf breaking，或者团队自己的 descriptor diff。手工 code review 不够可靠，因为 reviewer 很容易只看字段名，没注意 tag number 和 JSON 名。

第二层是跨版本读写测试。至少要覆盖四个方向：

```text
old client -> new server；
new client -> old server；
old writer data -> new reader；
new writer data -> old reader。
```

对 request/response 都要测。比如新 request 增加了可选字段，老 server 应该忽略还是返回明确错误？新 server 返回新 enum 值，老 client 会不会崩？老 client 没传新字段时，新 server 是否有默认行为？如果对外使用 ProtoJSON，unknown fields 不被支持，删除字段或重命名字段会不会让老 JSON 请求 parse 失败？

第三层是 golden data。保存历史版本序列化出来的二进制 protobuf、ProtoJSON、错误详情、metadata/trailers 样本。每次改 schema 或 runtime 时，用新代码读取这些 golden files；也用老版本客户端读取新服务返回的样本。不要依赖“现构造现解析”的 round-trip，因为同一版本 round-trip 很容易掩盖兼容问题。

第四层是行为契约。RPC 兼容不只是能 parse。还要测：

```text
status code 是否稳定；
错误详情类型是否稳定；
deadline/cancellation 语义是否稳定；
幂等 key 重复请求是否 result replay；
分页 token 是否还能使用；
排序和默认 filter 是否变化；
metadata 中认证、trace、tenant 是否兼容；
streaming message 顺序、半关闭、错误 trailers 是否兼容；
server 是否仍然接受老 SDK 的 authority/content-type/compression。
```

第五层是传输和代理兼容。真实链路上可能有 ingress、Envoy、NGINX、service mesh、TLS/mTLS、ALPN、压缩和最大消息限制。兼容性测试不能只测 in-process server，还要跑一组端到端 smoke：

```text
直连 gRPC；
经过 ingress/proxy；
TLS/mTLS；
压缩开/关；
大 metadata 边界；
大 message 边界；
server streaming / client streaming / bidi streaming；
连接 GOAWAY 和 graceful shutdown；
老客户端遇到新服务端健康检查和 service config。
```

第六层是多语言。gRPC 和 protobuf 的跨语言行为大体一致，但默认值、unknown enum、JSON 映射、时间类型、bytes/base64、int64 JSON 字符串、deadline 表示、metadata 大小写，都会有语言差异。对公开 API，至少要让主要 SDK 跑同一套 conformance 或 contract tests。

面试里可以这样答：

```text
RPC API 兼容性测试要同时测 schema 和行为。schema 层用 descriptor/buf breaking 检查字段号、reserved、类型、enum、method 删除和 ProtoJSON 变化；运行时要做 old client/new server、new client/old server、old data/new reader、new data/old reader 四个方向；再用 golden protobuf/JSON/error/trailer 样本防止 round-trip 掩盖问题。行为上还要测 status code、错误详情、幂等、deadline、分页 token、metadata、stream 顺序和代理链路。公开 API 还要跑多语言 SDK contract tests。
```

## Q031. HTTP/2 multiplexing 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

HTTP/2 multiplexing 的核心目标很明确：在一条连接上同时承载多个独立的请求/响应交换，减少 HTTP/1.x 那种“为了并发不得不开很多连接”的成本，并降低应用层 head-of-line blocking 对延迟的影响。

RFC 9113 对这个目标说得很直接。HTTP/1.0 在一条 TCP 连接上同一时间只能有一个 outstanding request；HTTP/1.1 有 pipelining，但仍然会遇到应用层队头阻塞；HTTP/2 通过把请求/响应切成带 stream id 的 frame，让多个 stream 的 frame 可以交错出现在同一条连接上。换成 gRPC 的话，就是多个 RPC 可以共享同一条 HTTP/2 connection，每个 RPC 对应一个 HTTP/2 stream。

所以它首先解决的是性能和资源利用问题，不是业务正确性问题。

```text
核心目标:
  减少连接数；
  降低建连、TLS 握手和慢启动成本；
  让多个请求在同一连接上并发推进；
  避免一个 HTTP 请求在应用层阻塞整条连接上的后续请求。

不是它的目标:
  保证 RPC exactly-once；
  保证请求之间公平调度；
  消除 TCP 层丢包导致的队头阻塞；
  自动解决服务端排队、业务锁竞争或线程池耗尽。
```

它对正确性有帮助，但只是“传输协议层面的正确性”。比如 stream 有状态机，stream id 不能复用，frame 必须遵守状态转换，connection error 和 stream error 有不同作用范围。这些规则能保证协议不会乱套，但它们不保证业务语义正确。客户端收到 `DEADLINE_EXCEEDED` 时，HTTP/2 multiplexing 不能告诉你服务端有没有执行过业务逻辑；一个带副作用的 RPC 能不能重试，还是要看幂等设计。

它也不是安全机制。TLS、mTLS、ALPN、认证 token、authorization policy 才是安全边界。multiplexing 甚至会放大某些安全或可靠性问题：同一条连接承载很多 stream，如果连接被打爆、被 reset、被代理关闭，影响面可能比一个请求一条连接更大。

可维护性方面，它有间接价值。对上层框架来说，gRPC 可以把连接复用、stream 生命周期、flow control、trailers、deadline 编码等细节封装起来，调用方不用手写这些逻辑。但这不是 multiplexing 的第一目标。你不能因为用了 HTTP/2，就认为系统自然更容易维护；真正影响维护性的还是 API 语义、错误模型、观测指标、超时重试策略和兼容性测试。

面试里可以这样答：

```text
HTTP/2 multiplexing 主要解决性能和资源利用问题。它把多个请求/响应拆成带 stream id 的 frame，在同一条连接上交错传输，从而减少连接数、降低握手和慢启动成本，并缓解 HTTP/1.x 的应用层队头阻塞。它有协议正确性规则，但不解决业务正确性；它也不是安全机制，不能替代 TLS、认证、幂等和重试设计。最容易误解的一点是：HTTP/2 解决的是应用层 multiplexing，不解决 TCP 丢包带来的队头阻塞。
```

## Q032. HTTP/2 multiplexing 的典型适用场景和不适用场景分别是什么？

**回答：**

HTTP/2 multiplexing 最适合“同一个 client 到同一个 authority，有大量相互独立、体量中小、可以并发推进的请求”的场景。gRPC 的 unary RPC、短 server streaming、内部微服务之间的高频小请求，都属于比较自然的使用场景。

典型适用场景有几类。

第一类是内部服务之间的高并发 RPC。比如 API server 同时调用用户、权限、库存、推荐、风控几个服务；或者一个 worker 同时向同一个调度服务发很多状态查询。用 HTTP/1.1 时，为了并发通常要维护一组连接；HTTP/2 可以让多个 stream 共用一条连接，减少连接抖动。

第二类是大量小消息。比如元数据查询、配置读取、心跳、短控制面请求。这些请求单个 payload 不大，主要成本在调度、握手、TLS、header 和往返延迟上。multiplexing 可以让连接长期保持 warm，避免每个请求都经历一遍连接建立。

第三类是请求间没有强顺序依赖。HTTP/2 stream 在逻辑上彼此独立，协议层可以交错发送 frame。你越能把业务拆成独立 stream，它越能发挥作用。如果业务要求严格按全局顺序处理，multiplexing 带来的并发能力反而不一定有用。

第四类是连接数敏感的环境。移动端、边缘节点、service mesh、网关代理、云负载均衡器，都可能受连接数、TLS 握手、文件描述符和 NAT 映射影响。减少连接数经常能降低系统开销。

不适用或要谨慎使用的场景也很常见。

第一，少量超大对象传输。一个超大响应如果长期占用连接带宽、flow-control window、发送队列和 socket buffer，其他小 stream 可能被拖慢。HTTP/2 可以交错 frame，但不能凭空制造带宽。大对象更适合对象存储、分片下载、断点续传或专门的数据通道。

第二，跨公网、高丢包或高抖动链路。HTTP/2 仍然跑在 TCP 上。TCP 要按字节序交付，一旦某个 TCP segment 丢失，后面的字节即使已经到达内核也不能交给上层。结果是同一条 HTTP/2 连接上的所有 stream 都可能一起卡住。这就是 TCP 层 head-of-line blocking。

第三，极长生命周期的 streaming RPC。长流能避免频繁建 RPC，但 gRPC 官方性能建议也提醒，stream 一旦开始就不能再做负载均衡。很多长 bidi stream 黏在少数后端上，会让 worker 分布越来越不均匀。长流还更难调试：失败发生时，你要分清是业务心跳、flow control、代理 idle timeout、server draining，还是客户端读写协程卡住。

第四，服务端业务本身是瓶颈。multiplexing 只解决传输层并发，不会让数据库锁、CPU 计算、序列化、线程池或下游依赖变快。如果 p99 是服务端排队造成的，把更多 stream 塞进同一条连接只会把排队位置从网络层转移到服务端或客户端。

第五，需要故障隔离的场景。单连接 multiplexing 的副作用是失败域变大：连接被 reset，连接上的多个 RPC 同时受影响。对高优先级控制请求、低优先级批量请求、超大传输和普通查询，通常应该分 channel、分连接池、分优先级或分服务，而不是全塞到一条共享连接里。

面试里可以这样答：

```text
HTTP/2 multiplexing 适合大量独立、中小 payload、同目标服务的并发 RPC，尤其是内部微服务和控制面请求。它不适合把超大对象、极长流、强全局顺序请求、高丢包公网链路、不同优先级的流量无脑塞进一条连接。判断标准很简单：如果瓶颈是建连和并发请求调度，它有帮助；如果瓶颈是带宽、TCP 丢包、服务端队列、业务锁或长连接粘性，它可能只是把问题藏起来。
```

## Q033. HTTP/2 multiplexing 和相近概念最容易混淆的边界在哪里？

**回答：**

最容易混淆的地方，是把 multiplexing 当成“所有并发问题的统一答案”。它只是一个传输层能力：多个逻辑 stream 共享一条连接，并通过 frame 的 stream id 区分彼此。它不是线程模型，不是负载均衡，不是批处理，也不是业务流式处理。

几个边界要分清楚。

第一，multiplexing 不等于 parallelism。HTTP/2 允许多个 stream 的 frame 在同一条连接上交错传输，但服务端是不是并行处理这些 RPC，取决于 gRPC runtime、线程池、event loop、业务 handler、下游依赖和 CPU 核数。一个 server 也可以在协议层同时接收很多 stream，业务层却串行排队处理。

```text
multiplexing:
  传输层可以同时承载多个 stream。

parallelism:
  计算和业务处理真正同时执行。
```

第二，multiplexing 不等于 streaming。HTTP/2 stream 是协议里的逻辑通道；gRPC streaming 是 RPC API 形态。一个 unary RPC 也会使用一个 HTTP/2 stream，只是它通常只有一条 request message 和一条 response message。反过来，server streaming、client streaming、bidi streaming 会在同一个 HTTP/2 stream 上传多条 gRPC length-prefixed message。

第三，multiplexing 不等于 connection pooling。连接池是维护多条连接的策略；multiplexing 是一条连接内部承载多个 stream 的能力。HTTP/2 让连接池的重要性下降，但没有让连接池消失。高并发、长流、连接级 flow control、`SETTINGS_MAX_CONCURRENT_STREAMS`、单连接失败域，都可能让你仍然需要多个 channel 或多条连接。

第四，multiplexing 不等于 pipelining。HTTP/1.1 pipelining 是在同一连接上按顺序发送多个请求，但响应仍然必须按顺序回来，所以前一个响应慢会挡住后面的响应。HTTP/2 用 stream id 把请求/响应分开，frame 可以交错，后一个 stream 的 response 不必等前一个 response 完整结束。

第五，multiplexing 不等于 batching。batching 是把多个业务请求合成一个更大的请求，减少调用次数，但会改变业务请求粒度、失败处理和重试语义。multiplexing 不合并业务请求；每个 RPC 仍然有自己的 metadata、deadline、status、trailers 和错误语义。

第六，multiplexing 不等于 load balancing。client 到某个后端的一条 HTTP/2 连接上可以 multiplex 很多 RPC，但这些 RPC 仍然落在同一个连接指向的后端上。如果负载均衡发生在 L4 连接层，长连接会导致请求分布粘在少数后端；如果要按 RPC 粒度均衡，需要 client-side LB、xDS、proxy L7 LB 或多个 subchannel。

第七，multiplexing 不等于 flow control。flow control 是限制发送方不要超过接收方处理能力的机制；multiplexing 是多个 stream 共用连接的机制。两者配合使用，但不是同一件事。没有合理 flow control，multiplexing 会把“一个慢接收方”的问题变成“连接上大量 buffer 和等待”的问题。

面试里可以这样答：

```text
HTTP/2 multiplexing 的边界在于：它只说明多个 stream 可以共享一条连接并交错传输，不说明业务并行、不说明自动负载均衡、不说明请求被 batch，也不等于 gRPC streaming。它缓解 HTTP/1.1 pipelining 的应用层队头阻塞，但仍受 TCP、flow control、服务端线程池和业务队列影响。
```

## Q034. HTTP/2 multiplexing 在高并发场景下可能出现哪些隐藏问题？

**回答：**

HTTP/2 multiplexing 在低中等并发下很舒服：连接少、延迟低、实现细节由框架处理。高并发后，问题会从“连接太多”变成“单连接上共享资源太多”。这些问题不一定在平均延迟里出现，常常先体现在 p99、p999、客户端队列长度、active streams、内存和 reset 数量上。

第一个隐藏问题是 `SETTINGS_MAX_CONCURRENT_STREAMS`。HTTP/2 允许端点声明对端最多能同时打开多少 active stream。gRPC 官方性能文档也提到，每个 gRPC channel 使用零条或多条 HTTP/2 连接，每条连接通常有并发 stream 上限；active RPC 到达上限后，额外 RPC 会在客户端排队，等已有 RPC 结束后才能发出去。线上看到的现象就是：客户端业务线程已经发起调用，但包根本还没上网，p99 里混进了 client-side queue time。

第二个问题是连接级 flow control。HTTP/2 有 stream 级 window，也有 connection 级 window。一个大响应或慢读 stream 如果消耗了太多连接级 window，其他 stream 即使自己很小，也可能要等 WINDOW_UPDATE。你看到的不是某个 RPC handler 慢，而是写调用卡住、send queue 增长、window stall 增多。

第三个问题是公平性。RFC 9113 说 streams largely independent，但这不等于实现一定公平。runtime 的写调度、TLS record 打包、socket buffer、event loop、锁竞争、优先级策略、应用读取速度都会影响哪个 stream 先推进。一个持续吐大消息的 stream 可能让小请求的尾延迟变差。

第四个问题是 TCP 层队头阻塞。HTTP/2 解决了 HTTP/1.x 的应用层队头阻塞，但 RFC 9113 也明确提醒它不解决 TCP head-of-line blocking。同一条 TCP 连接上的某个 segment 丢了，后面的字节不能交付给 HTTP/2 层；结果是连接上所有 stream 都可能一起停顿。并发越高，同一条连接承载的业务越多，单次丢包影响越明显。

第五个问题是 CPU 和锁。高 QPS 小包场景下，瓶颈可能不在网络，而在 frame 编解码、HPACK header 压缩/解压、TLS、protobuf marshal/unmarshal、拦截器、统计埋点、连接写锁、stream map 锁、定时器和上下文取消传播。很多系统刚开始只看网络吞吐，后来发现 CPU profile 里全是 runtime、TLS、压缩和锁等待。

第六个问题是内存和 backpressure。写入 gRPC stream 不代表数据已经发到网络上，gRPC flow control 文档也提醒，write 返回前框架可能等待；write 返回也只是把数据交给框架处理，后面还有 buffering 和 OS 发送。如果应用层没有限制 in-flight message，慢接收方会把内存顶起来。

第七个问题是故障影响面。一条连接上有很多 stream，连接被代理关闭、收到 GOAWAY、TCP reset、TLS 错误或 keepalive 被拒绝时，受影响的不再是一个请求，而是一批 RPC。错误看起来像“某个时间点大量请求同时失败”。

第八个问题是负载分布。少数长连接承载大量 stream，L4 负载均衡只在建连接时选后端，后续 RPC 会粘在已选后端上。扩容新节点后，老连接不迁移，新节点可能很空，老节点还很忙。

面试里可以这样答：

```text
高并发下 HTTP/2 multiplexing 的问题主要来自共享资源：并发 stream 上限导致客户端排队，connection-level flow control 让小请求被大流拖慢，单 TCP 丢包会卡住所有 stream，写调度和锁竞争影响公平性，buffering 会推高内存，长连接还会造成后端负载粘性。排查时不能只看平均延迟，要看 active streams、pending RPC、flow-control stall、连接 reset、CPU profile、内存和 per-connection 请求分布。
```

## Q035. HTTP/2 multiplexing 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

multiplexing 把多个 RPC 放到同一条连接上，所以故障边界会变得更细，也更容易误判。你不能只问“连接断了没有”，还要问“哪些 stream 已经到达服务端、哪些还没到、哪些已经写了响应、哪些只是客户端排队中”。

崩溃时，最典型的边界是连接上所有 active stream 的状态都可能变成未知。服务端进程崩溃前，有些 RPC 可能已经完成业务写入但没来得及发送 trailers；有些可能刚读完 request headers；有些只收了一半 message；还有些可能还在客户端本地队列里，没真正发到连接上。客户端看到的可能都是 `UNAVAILABLE`、`INTERNAL`、`CANCELLED` 或连接 reset，但业务语义完全不同。

重启和优雅关闭时，要看 GOAWAY 和 drain。HTTP/2 的 GOAWAY 用来停止连接继续创建新 stream，并携带 last stream id。大致语义是：last stream id 之后的 stream 没有被处理，客户端可以在新连接上重试；last stream id 之前或等于它的 stream 状态则不一定能从连接层判断清楚。gRPC 的 graceful shutdown 文档也强调，服务端应通知客户端停止发新 RPC，允许 in-flight RPC 在期限内完成，超过期限再强制关闭。

超时时，边界在于 deadline 是每个 RPC 的语义，不是整条连接的语义。同一条 HTTP/2 连接上可以同时有 10ms、100ms、5s 的 RPC。一个 stream deadline 到了，客户端可以取消这个 stream，但不应该影响同连接上的其他 stream。反过来，如果连接级别卡住，比如 TCP 丢包、proxy flow control、TLS 写阻塞，多个不同 deadline 的 RPC 可能一起超时。

重试时，边界更敏感。HTTP/2 有 `REFUSED_STREAM`，RFC 9113 对它的语义是端点在执行任何应用处理之前拒绝 stream，这类错误通常更接近“可安全重试”的传输失败。但很多错误没有这么强的语义。`RST_STREAM CANCEL` 可能是客户端取消，也可能是代理或服务端不再需要这个 stream；连接 reset 更不能说明业务没有执行。所以 gRPC retry policy 只能在 status code、method 幂等性、commit point 和 retry budget 都允许时使用。

还有一个容易忽略的边界是 stream id。HTTP/2 stream id 单调增加，不能复用。长连接跑很久后，stream id 可能耗尽；客户端要新建连接，服务端也可以发 GOAWAY 让客户端迁移。这个问题平时少见，但在极长生命周期连接和极高 QPS 场景下不能完全忽略。

代理会让边界更复杂。客户端到代理是一条 HTTP/2 连接，代理到后端可能是另一条连接。客户端收到的 reset、GOAWAY 或 deadline，不一定来自真正的后端；也可能是 ingress idle timeout、upstream timeout、max stream duration、drain policy 或连接池回收。

面试里可以这样答：

```text
HTTP/2 multiplexing 在故障场景下的核心边界是：连接失败会影响多个 active stream，但每个 stream 的业务执行状态不同。GOAWAY 只能帮助判断哪些新 stream 不应再发到旧连接，不能证明已有 stream 都没执行。timeout 是每个 RPC 的语义，连接级卡顿却可能让一批 RPC 同时超时。重试只能依赖明确的传输语义和业务幂等，比如 REFUSED_STREAM 比连接 reset 更接近可安全重试，但带副作用 RPC 仍然要有 idempotency key。
```

## Q036. HTTP/2 multiplexing 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

没有一个固定答案。HTTP/2 multiplexing 的性能瓶颈会随着请求大小、并发度、链路质量、runtime 实现和业务处理位置变化。比较靠谱的回答是：小消息高 QPS 先看 CPU 和锁，大消息和慢接收先看网络、I/O 和 flow control，长流和超高并发再看内存、客户端队列和负载粘性。

小消息高 QPS 场景，CPU 很容易先到顶。原因不只是 protobuf。每个 RPC 都要经历 metadata 处理、HPACK 编解码、frame parse、TLS record、拦截器、统计指标、deadline timer、context cancellation、status/trailers 处理。单个开销看起来不大，乘上几十万 QPS 后很明显。这个场景下，CPU profile 往往比网卡流量更有价值。

锁竞争也常见。多个 stream 共享一条连接，runtime 内部通常需要维护 active stream map、写队列、flow-control window、transport state、统计计数和定时器。高并发下，连接写锁、stream map 锁、poller/event loop、completion queue、回调执行线程都可能成为热点。表现为 CPU 利用率不低，但 QPS 上不去，p99 抖动明显。

内存问题一般来自 buffering 和 in-flight 过多。HTTP/2 可以让很多 stream 同时存在，但每个 stream 都有状态、header、message buffer、窗口、定时器和回调上下文。应用如果把大量 message 写给框架而不等待下游消费，内存会随着发送队列涨上去。大 metadata、压缩字典、trace baggage 和错误详情也会放大 per-stream 内存。

I/O 和网络瓶颈主要出现在大 payload、跨机房、跨公网、慢接收方和丢包场景。HTTP/2 frame 可以交错，但所有字节还是要经过同一条 TCP 连接。带宽满了就是满了；拥塞窗口小就是发不出去；丢包会让 TCP 按序交付卡住后续字节。此时增加 stream 数只会增加排队，不会提高有效吞吐。

flow control 是网络和内存之间的边界。窗口太小，吞吐上不去；窗口太大，慢接收方会让内存压力变大。连接级窗口还会让一个大 stream 影响其他小 stream。排查时要把“写调用阻塞”“WINDOW_UPDATE 延迟”“send buffer 增长”“接收方读慢”放在一起看。

服务端业务瓶颈也不能忽略。很多时候大家把 p99 归因于 HTTP/2，其实慢在业务 handler、数据库连接池、线程池、GC、下游 RPC 或日志同步写。multiplexing 只是让更多请求更快到达服务端，暴露了后面的队列。

可以按这个顺序判断：

```text
小包高 QPS:
  看 CPU、锁、TLS、HPACK、protobuf、拦截器和指标。

大包或流式:
  看网络带宽、TCP 重传、flow-control window、send/recv buffer。

大量 active streams:
  看客户端 pending RPC、server max concurrent streams、内存和 per-stream 状态。

少数后端很忙:
  看长连接粘性、channel/subchannel 分布和负载均衡策略。

p99 成批抖动:
  看单连接丢包、GOAWAY、proxy drain、GC pause、event loop 卡顿。
```

面试里可以这样答：

```text
HTTP/2 multiplexing 的瓶颈不是固定在网络层。小消息高 QPS 往往是 CPU、TLS/HPACK/protobuf 和锁竞争；大消息或流式传输通常是带宽、I/O 和 flow control；大量并发 stream 会带来内存、客户端排队和 max concurrent streams 限制；跨公网或丢包链路还会暴露 TCP 层 HOL blocking。判断瓶颈要看 profile、active streams、pending RPC、窗口 stall、重传、GC 和后端队列，而不是只看平均延迟。
```

## Q037. HTTP/2 multiplexing 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

这三类测试目标不同，不能混在一起。correctness test 证明协议行为对；stress test 证明极端和混乱条件下不崩、不泄漏、不死锁；benchmark 证明在可复现实验条件下的性能边界。一个 benchmark 跑得快，不代表实现正确；一个 correctness test 全过，也不说明高并发下能扛住。

correctness test 要围绕不变量写。简化版 HTTP/2 multiplexing 至少要测这些：

```text
stream id:
  client-initiated stream 使用递增奇数；
  stream id 不能复用；
  stream 0 只能用于连接控制帧，不能创建普通 stream。

frame routing:
  DATA/HEADERS frame 按 stream id 分发到正确 stream；
  不同 stream 的 frame 可以交错；
  同一个 stream 内部的 DATA 顺序不能乱。

state machine:
  idle -> open -> half-closed -> closed 的转换合法；
  END_STREAM 后不能继续发送 DATA；
  closed stream 收到非法 frame 要产生正确错误。

flow control:
  stream window 和 connection window 不能变成负数；
  WINDOW_UPDATE 后 blocked writer 可以继续；
  慢读 stream 不应导致无限制内存增长。

error boundary:
  RST_STREAM 只关闭对应 stream；
  connection error 才关闭整条连接；
  GOAWAY 后不再创建超过 last stream id 的新 stream。
```

如果放到 gRPC 语义里，还要测 trailers、`grpc-status`、deadline、取消、metadata、压缩和 message framing。gRPC over HTTP/2 里 request/response 都是 HEADERS、length-prefixed message、trailers 的组合，不能只测裸 DATA frame。

stress test 要故意制造混乱。比如同时创建几千或几万个 stream；一半 stream 小包快进快出，另一半大包慢读；随机取消 stream；随机发送 GOAWAY；模拟服务端重启；把 `SETTINGS_MAX_CONCURRENT_STREAMS` 设置得很低；让接收方不读；让写方持续写；注入丢包、延迟、乱序和半关闭；同时跑 deadline、重试和连接重建。目标不是追求漂亮数据，而是找死锁、内存泄漏、goroutine/thread 泄漏、窗口卡死、stream 状态泄漏和错误作用范围搞错。

benchmark 则要控制变量。gRPC 官方 benchmarking 文档把测试分成 contentionless latency、QPS、per-core scalability 等场景，这是很好的思路。对 HTTP/2 multiplexing，可以至少做这些维度：

```text
latency:
  1 connection / 1 stream；
  1 connection / N streams；
  M connections / N streams；
  p50、p95、p99、p999。

throughput:
  小消息 QPS；
  大消息 MB/s；
  streaming ping-pong；
  单核吞吐和多核扩展。

resource:
  CPU cycles/op；
  alloc/op 和 peak RSS；
  goroutine/thread 数；
  active streams；
  pending streams；
  flow-control stall 次数。

fairness:
  大流和小流混跑时，小流 p99 是否被拖垮；
  高优先级和低优先级是否隔离；
  单个慢读 stream 对其他 stream 的影响。
```

测试环境也要讲清楚。直连和经过 proxy 不一样；TLS 和明文不一样；loopback 和跨机房不一样；单客户端和多客户端不一样。benchmark 如果没有说明这些条件，数字很难比较。

面试里可以这样答：

```text
correctness test 测协议不变量：stream id、frame 路由、状态机、flow control、RST_STREAM/GOAWAY 的错误边界。stress test 测极端并发和混乱故障：大量 stream、慢读、大包、小包混跑、随机取消、GOAWAY、重启、窗口耗尽、低 max concurrent streams，重点找死锁、泄漏和状态残留。benchmark 测可复现性能：不同连接数和 stream 数下的 p99、QPS、吞吐、CPU、内存、锁等待、pending RPC 和 fairness。
```

## Q038. 如果要求从零实现一个简化版 HTTP/2 multiplexing，你会先定义哪些不变量？

**回答：**

我会先定义不变量，而不是先写 socket 读写。multiplexing 的难点不在“读到 frame 后放进 map”这么简单，而是在并发、关闭、取消、flow control 和错误处理同时发生时，状态不能乱。

第一组是连接和 stream 的身份不变量。

```text
connection:
  一条连接有一个全局 reader、一个受控 writer、一个 active stream table。

stream:
  每个 stream 有唯一 stream id；
  client 发起的 stream id 递增且为奇数；
  stream id 一旦使用就不能复用；
  stream 0 只用于连接控制，不承载业务数据。
```

第二组是状态机不变量。每个 stream 都必须处在明确状态里：`idle`、`open`、`half-closed-local`、`half-closed-remote`、`closed`。只有合法 frame 能触发合法转换。比如收到 HEADERS 才能从 idle 进入 open；本端发送 END_STREAM 后进入 half-closed-local；两边都结束后进入 closed；closed 后再收到 DATA 就是错误。这个状态机要比业务 handler 更底层，不能让业务代码随便绕过。

第三组是顺序不变量。同一 stream 内，DATA 必须按发送顺序交付给上层；不同 stream 之间不保证全局顺序。也就是说，stream 1 的第二个 DATA 不能早于 stream 1 的第一个 DATA 交给业务，但 stream 3 的响应完全可以先于 stream 1 完成。

第四组是流控不变量。每个 stream 有自己的 send window 和 recv window，连接也有 connection-level window。发送 DATA 前必须同时扣减 stream window 和 connection window；收到 WINDOW_UPDATE 后增加对应 window；任何窗口都不能溢出或变成负数；窗口不足时 writer 必须等待或返回 backpressure，不能无限 buffer。

第五组是错误作用范围不变量。stream error 只能关闭对应 stream，不能误伤其他 stream；connection error 必须关闭整条连接，并通知所有 active stream；RST_STREAM 后不能再向该 stream 交付业务数据；GOAWAY 后不能再创建超过 last stream id 的新 stream，但已有 stream 的处理要按规则收尾。

第六组是资源不变量。active stream 数不能超过对端声明的 `SETTINGS_MAX_CONCURRENT_STREAMS`；每个 stream 的 buffer 有上限；连接发送队列有上限；关闭 stream 后必须释放 buffer、timer、回调、map entry 和等待者。否则正确性测试可能过，高并发跑一晚内存就涨。

第七组是并发不变量。一个 stream 的状态更新要串行化；连接 writer 要保证 frame 不被多个线程交叉写坏；取消、超时、远端 reset、业务完成同时发生时，只能有一个最终结果。这里通常需要明确 owner 模型：比如 reader 负责解析入站 frame，writer loop 负责出站 frame，stream object 通过 channel/queue 接收事件。

第八组是观测不变量。每个 stream 至少能记录 created、headers sent/received、first byte、last byte、reset reason、close reason；连接至少能记录 active streams、pending streams、window、bytes in/out、GOAWAY、last stream id。没有这些指标，调试 multiplexing 基本靠猜。

面试里可以这样答：

```text
从零实现简化版 HTTP/2 multiplexing，我会先定不变量：stream id 唯一且单调、stream 0 只做连接控制；每个 stream 遵守状态机；同一 stream 内有序，不同 stream 间无全局顺序；发送 DATA 必须同时满足 stream 和 connection flow-control window；RST_STREAM 只影响单个 stream，connection error 才影响所有 stream；GOAWAY 后不再创建超过 last stream id 的新 stream；active streams、buffer 和 pending queue 都有上限；关闭 stream 必须释放资源。
```

## Q039. HTTP/2 multiplexing 的常见误用是什么，误用后通常会产生什么线上症状？

**回答：**

最常见的误用，是把 multiplexing 理解成“一个连接就够了”。HTTP/2 确实能在一条连接上跑很多 stream，但这不等于所有流量都应该共享同一条连接。高优先级控制请求、普通查询、长 streaming、大对象传输、批处理任务，如果都挤在同一个 channel 里，最后往往是小请求 p99 被大请求拖垮。

第二个误用是忽略 `SETTINGS_MAX_CONCURRENT_STREAMS`。客户端以为自己发起了 5000 个 RPC，实际上只有前 100 个或几百个 stream 被发送，剩下的在本地排队。线上症状是：服务端看起来不忙，网卡也不满，但客户端 p99 和 deadline error 很高。很多人只抓服务端日志，找不到原因，因为慢在客户端 transport queue。

第三个误用是用长 bidi stream 替代所有 unary RPC。长流适合真正连续的逻辑数据流，比如订阅、实时同步、双向会话。但如果只是为了“减少 RPC 次数”就把很多独立请求塞进一个 bidi stream，错误语义、重试语义、单请求 deadline、鉴权审计、负载均衡都会变复杂。线上症状是：某个 stream 断了以后影响一批逻辑请求；扩容后新节点吃不到流量；问题定位只能看到一个长连接失败，看不到具体哪次业务调用失败。

第四个误用是大消息直接走 RPC response。大响应会占用连接窗口、socket buffer、序列化内存和带宽。小请求和大响应共享连接时，小请求尾延迟会上升。症状是平均延迟还能看，p99/p999 很难看；send queue 增长；客户端或服务端 RSS 上升；有时还会出现 `RESOURCE_EXHAUSTED`、`DEADLINE_EXCEEDED` 或 proxy reset。

第五个误用是不设置 deadline。multiplexing 会让请求并发更多，如果没有 deadline，慢请求会长期占用 stream、窗口、内存和业务资源。症状是 active streams 越积越多，连接看起来还活着，但业务请求不断超时或卡住。

第六个误用是把 multiplexing 当成负载均衡。L4 负载均衡只在建连接时选后端，长 HTTP/2 连接会把大量 RPC 粘在同一个后端上。症状是节点负载不均：新扩容节点很空，老节点 CPU 和 active streams 很高；重启客户端后分布突然变好；过一段时间又倾斜。

第七个误用是忽略 backpressure。应用层一直写，不读或读得很慢，手动 flow control 又处理不好，就可能死锁或内存膨胀。gRPC flow control 文档明确提醒，双方都做同步读或手动 flow control 时，如果都大量写而不读，有死锁风险。症状是两边连接都没断，但 stream 不再前进，goroutine/thread 卡在 write 或 recv。

第八个误用是过度重试。连接上多个 stream 同时遇到 reset 或 timeout，如果客户端对所有请求立即重试，可能打出重试风暴。症状是故障恢复期 QPS 暴涨、下游更慢、限流触发、`UNAVAILABLE` 和 `DEADLINE_EXCEEDED` 同时上升。

面试里可以这样答：

```text
常见误用包括：认为一个 HTTP/2 连接永远够用，忽略 max concurrent streams，把长 bidi stream 当通用请求通道，大消息直接走 RPC，不设 deadline，把 multiplexing 当负载均衡，应用层不处理 backpressure，以及对连接级失败做无节制重试。线上症状通常是客户端 pending RPC 增长、p99 抖动、节点负载不均、active streams 泄漏、内存上涨、写调用阻塞、连接 reset 时一批 RPC 同时失败，或者恢复期出现重试风暴。
```

## Q040. HTTP/2 multiplexing 在单机和分布式环境中的语义有什么差异？

**回答：**

协议语义本身没有差异。无论单机还是分布式，HTTP/2 multiplexing 都是同一套规则：一条连接上有多个 stream，frame 带 stream id，同一 stream 内有序，不同 stream 之间可以交错，flow control 分 stream 和 connection 两层，GOAWAY/RST_STREAM/SETTINGS/PING 都按协议工作。

差异在于环境。单机测试里，很多真实问题不会出现，或者出现得太温和。

单机里，client 和 server 可能在同一台机器，甚至走 loopback。没有真实的跨机网络抖动、丢包、NAT、L4/L7 负载均衡、代理 idle timeout、跨可用区 RTT、TLS 终止、服务发现更新和节点滚动重启。此时 multiplexing 看起来很理想：一条连接上多个 stream 并发推进，延迟稳定，几乎没有 TCP 重传，连接也很少被中间设备关闭。

分布式环境里，第一层差异是故障域。一条 HTTP/2 连接可能跨进程、跨机器、跨机房、跨 proxy。连接 reset 不再只是本地 socket 关闭，而可能来自 ingress、sidecar、负载均衡器、NAT、后端重启、证书轮换或网络中断。连接上多个 stream 会一起受影响，客户端还要判断哪些可以重试。

第二层差异是负载均衡语义。单机测试通常只有一个 server，multiplexing 不涉及请求分布。分布式里，长连接会影响后端 worker 分布。L4 负载均衡按连接选后端，RPC 级别的均衡需要 client-side LB、L7 proxy 或 subchannel 策略。也就是说，multiplexing 在单机只是并发传输，在分布式里还会影响容量利用和扩缩容效果。

第三层差异是公平性。单连接内部的 stream 调度只对这条连接负责，不提供全局公平。一个有大量连接的客户端、一个只开一条连接的客户端、一个不断发大流的客户端，在服务端看到的资源占用可能完全不同。分布式系统要靠限流、配额、优先级队列、连接池隔离和服务端调度来补齐。

第四层差异是观测。单机里你可以很容易抓到两端日志；分布式里一次 RPC 可能经过 client library、sidecar、ingress、service mesh、server transport、业务 handler、下游服务。单看 gRPC status 不够，要串起 trace id、stream/connection 指标、proxy access log、TCP retransmit、server queue time 和 application span。

第五层差异是超时和重试的后果。单机里重试多半只是多打一次本地进程；分布式里重试会放大流量，可能跨 zone、跨 proxy、跨多个下游。deadline 也要沿调用链传播，否则上游已经放弃，下游还在执行无意义工作。

第六层差异是容量规划。单机 benchmark 能告诉你实现的上限，比如一条连接多少 QPS、多少 active streams、CPU/内存如何增长。分布式容量还要看连接分布、节点数、负载均衡策略、proxy 限制、连接回收、滚动发布、故障转移和重试预算。

面试里可以这样答：

```text
HTTP/2 multiplexing 的协议语义在单机和分布式里一样，但工程语义不同。单机里它主要表现为一条连接上的并发 stream；分布式里，它还影响负载均衡、故障域、扩缩容、proxy 超时、重试放大和观测链路。单连接内部没有全局公平，L4 负载均衡也不会按 RPC 重新分配请求。所以单机 benchmark 只能证明实现局部性能，不能证明分布式环境下的 worker 分布、故障切换和尾延迟表现。
```

## Q041. gRPC deadline 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

gRPC deadline 的核心目标是给一次 RPC 定义“调用方还愿意等到什么时候”。这个目标听起来简单，但它是 RPC 可靠性设计里最实用的一条边界：客户端不应该无限等，服务端也不应该在调用方已经放弃后继续消耗资源。

gRPC 官方 deadline 文档的定义很清楚：deadline 是一个时间点，超过这个时间点以后，客户端不再愿意等待服务端响应。默认情况下，gRPC 不会自动设置 deadline；如果调用方不显式设置，客户端可能一直等下去。服务端收到带 deadline 的 RPC 后，如果 deadline 已经过期，gRPC 会取消这个 call；但业务 handler 里启动的长任务、下游 RPC、数据库查询、后台 goroutine，并不会被魔法般打断，应用代码还要主动检查 cancellation。

所以 deadline 主要解决两个问题。

```text
正确性边界:
  调用方明确表达“超过这个时间结果就没有意义了”；
  RPC 结束状态可以区分成功、取消、deadline exceeded；
  下游调用不能随意超过上游剩余时间；
  失败后是否重试，要受剩余 deadline 约束。

资源与性能边界:
  避免客户端无限等待；
  避免服务端继续处理没人要的请求；
  避免队列、连接、stream、线程、goroutine 和下游资源被慢请求拖住；
  让系统在过载时更快释放无意义工作。
```

如果一定要在“正确性、性能、安全性、可维护性”里选主类，我会说它首先解决可靠性语义和资源控制，横跨正确性和性能。它不是纯性能优化，因为 deadline 本身不会让一次数据库查询更快；它也不是业务正确性的充分条件，因为 `DEADLINE_EXCEEDED` 不代表服务端没有执行副作用。gRPC status code 文档明确说，对会改变系统状态的操作，即使操作已经成功完成，也可能因为响应延迟而返回 `DEADLINE_EXCEEDED`。

它也不是安全机制。deadline 不能替代认证、鉴权、限流、配额和熔断。攻击者仍然可以发大量短 deadline 请求，让服务端频繁创建任务又马上取消；也可以发大量长 deadline 请求占用资源。安全和资源隔离要靠 rate limit、quota、auth、server-side admission control 一起做。

可维护性方面，deadline 的价值很大。它让调用链上的时间预算显式化：上游给 500ms，中间服务自己用了 120ms，下游最多只能拿到剩余时间，而不是每层都重新给自己 500ms。没有这个约束，多层 RPC 一叠加，用户请求早就超时了，后端还在忙着做无用功。

面试里可以这样答：

```text
gRPC deadline 的核心目标是表达调用方的时间预算：超过这个时间，结果对调用方就没有意义。它主要解决 RPC 的可靠性语义和资源控制，既有正确性边界，也有性能价值。它防止客户端无限等待，也让服务端和下游及时停止无用工作。但 deadline 不保证业务没有执行，不是安全机制，也不能替代幂等、限流和重试预算。
```

## Q042. gRPC deadline 的典型适用场景和不适用场景分别是什么？

**回答：**

gRPC deadline 适合所有有明确等待上限的 RPC。更直接一点说，生产环境里的大多数 RPC 都应该有 deadline，只是 deadline 的长短要按业务场景定，不能一刀切。

典型适用场景包括几类。

第一类是用户在线请求。比如一次页面加载、一次下单、一次查询、一次权限校验。用户或上游网关通常已经有整体 timeout，后端 RPC deadline 应该落在这个总预算里。一个 300ms 的接口，不能在中间层给下游随手设置 5s；用户早就看见失败了，下游成功也没有意义。

第二类是服务间扇出调用。一个请求同时查 5 个下游服务，deadline 能防止最慢的分支把整体请求拖死。更重要的是，deadline 可以沿调用链传播：中间服务已经花掉 80ms，下游拿到的是剩余预算，而不是一个全新的完整 timeout。

第三类是队列和 worker 调度。任务从队列里取出时可能已经等了一段时间，如果业务要求“超过 2 秒就不用做了”，worker 在真正执行前应该检查 deadline 或任务过期时间。否则系统越拥塞，越会处理大量已经没价值的旧任务。

第四类是批处理里的单次 RPC。批任务本身可以跑很久，但每次 RPC 仍然应该有限定。没有 deadline 的批任务在下游异常时容易卡死整个 worker；有 deadline 后，可以把失败记录下来、退避重试或跳过。

第五类是健康检查、配置拉取、控制面请求。这类请求通常应该短 deadline、快速失败。控制面卡住会影响很多业务流量，不能无限等。

不适用，或者说不应该用“短 deadline”硬套的场景也要分清。

第一，真正长时间的离线作业。比如模型训练、视频转码、大规模数据导出。调用方不应该用一个小时的同步 RPC 等结果。更合理的设计是提交任务返回 job id，然后用查询、回调、事件或流式进度来观察状态。deadline 仍然可以用于每次控制 RPC，但不应该把整个大任务塞进一个超长 unary RPC。

第二，不知道业务耗时分布时，不能拍脑袋给极短 deadline。gRPC 官方文档建议 deadline 应该基于网络延迟、服务端处理时间等估计，并通过 load testing 验证。上线前没有测过，就给所有方法统一 100ms，通常会制造大量假超时。

第三，不能把 deadline 当作服务端限流。deadline 只表达客户端愿意等多久，不表达服务端是否应该接受这个请求。服务端过载要靠 admission control、队列上限、并发限制、熔断、限流和优先级调度。

第四，不能把 deadline 当作业务取消协议。客户端 deadline 到了以后，服务端会收到 cancellation，但如果业务已经提交事务、写消息、发邮件、扣款，deadline 不会自动撤销这些副作用。涉及副作用的场景要有幂等键、状态机和补偿逻辑。

第五，低优先级但必须完成的后台任务，不适合依赖很短的 RPC deadline 作为唯一控制。它们可以有 per-attempt deadline，但任务层还要有整体重试窗口、最大尝试次数和持久化状态。

面试里可以这样答：

```text
deadline 适合在线请求、服务间扇出、worker 执行、控制面请求和批处理里的单次 RPC。它不适合把长离线任务做成一个超长同步 RPC，也不能替代限流、幂等和任务状态机。设置 deadline 要基于业务 SLO、网络延迟、服务端处理时间和压测结果；短 deadline 可以保护资源，但拍脑袋设置会制造大量假超时和重试放大。
```

## Q043. gRPC deadline 和相近概念最容易混淆的边界在哪里？

**回答：**

deadline 最容易和 timeout、cancellation、retry、wait-for-ready、keepalive、HTTP 超时、业务过期时间混在一起。它们都和“时间”有关，但语义不一样。

第一，deadline 和 timeout。deadline 是一个绝对时间点，timeout 是一段持续时间。比如“13:00:02 之前必须完成”是 deadline，“最多等 2 秒”是 timeout。gRPC 文档里也说明，有些语言 API 暴露 deadline，有些暴露 timeout；timeout 可以通过“开始时间 + duration”转换成 deadline。跨服务传播时，gRPC 会把 deadline 转成剩余 timeout，并扣掉已经消耗的时间，这样可以避免机器时钟不一致带来的问题。

第二，deadline 和 cancellation。deadline 过期会触发 cancellation，但 cancellation 不一定来自 deadline。客户端可能主动取消，连接可能出错，服务端也可能因为请求不再需要而结束。gRPC cancellation 文档里说，deadline expiration 和 I/O error 都会触发 cancellation。服务端收到取消信号后，业务 handler 需要配合停止工作；gRPC 库通常不能强行打断你的业务代码。

第三，deadline 和 retry。deadline 是整个 logical call 的时间预算，retry 是失败后的重新尝试。一个错误很常见：每次 retry 都重新给完整 timeout。这样原本 500ms 的用户请求，经过 3 次 retry 变成 1.5s，甚至更久。正确做法是所有 retry attempts 共享同一个 overall deadline；剩余时间不够就不要再发新的 attempt。

第四，deadline 和 wait-for-ready。wait-for-ready 表示 channel 暂时不可用时，RPC 可以先排队等连接 ready，而不是马上失败。但官方文档明确说 deadline 仍然适用；如果等到 deadline 过期，等待会被中断。也就是说，wait-for-ready 不是无限等待开关。

第五，deadline 和 keepalive。keepalive 是连接层活性探测，用来发现半开连接或让空闲连接保持可用；deadline 是 RPC 级时间预算。连接活着不代表某个 RPC 还能等，RPC deadline 到了也不代表连接该断。

第六，deadline 和 HTTP/2 `grpc-timeout`。在 gRPC over HTTP/2 协议里，请求头可以带 `grpc-timeout`，用来编码剩余 timeout，单位可以是小时、分钟、秒、毫秒、微秒、纳秒。它是传输表示，不是业务字段。不要把它和自定义 metadata 里的业务 expire_time 混在一起。

第七，deadline 和业务过期时间。业务过期时间可能代表订单有效期、任务最晚执行时间、token 过期时间。RPC deadline 只代表这次调用愿意等多久。一个订单 15 分钟后过期，不代表这次 RPC 可以阻塞 15 分钟；一次 RPC 100ms deadline 到了，也不代表订单业务对象过期。

面试里可以这样答：

```text
deadline 是 RPC 的总体时间预算，不等于每次 attempt 的 timeout，不等于业务过期时间，也不等于 keepalive。deadline 过期会触发 cancellation，但取消也可能来自客户端主动取消或 I/O 错误。wait-for-ready 可以让 RPC 等 channel ready，但 deadline 仍然会截断等待。重试必须共享同一个 deadline，不能每次重试都刷新预算。
```

## Q044. gRPC deadline 在高并发场景下可能出现哪些隐藏问题？

**回答：**

高并发下，deadline 的问题通常不是“有没有设置”，而是设置得是否一致、是否传播、是否被服务端及时响应。deadline 设计不好，会把一个过载系统推得更糟。

第一个隐藏问题是短 deadline 造成取消风暴。正常流量下，100ms deadline 可能够用；一旦服务端排队上升，很多请求同时过期，客户端集中收到 `DEADLINE_EXCEEDED`，服务端又在处理大量已经取消的任务。如果 handler 不检查 cancellation，CPU 还会继续烧在无用工作上。

第二个问题是 retry 放大。deadline 到了以后调用失败，如果客户端或上游又立刻重试，而每次重试都给新的完整 timeout，就会把排队系统进一步打爆。gRPC retry 文档说，显式 retry 要配置 max attempts、backoff、retryable status codes，并且 response header 一旦收到，RPC 就 committed，不再 retry。deadline 和 retry 要一起设计，否则高并发下很容易变成重试风暴。

第三个问题是 fan-out 预算被稀释。一个上游请求扇出到 20 个下游，每个下游都拿到同样宽松的 deadline，看起来每个调用都“合理”，整体资源却翻了很多倍。更糟的是，如果每个分支还有自己的 retry，系统会在高并发下产生乘法级放大。

第四个问题是客户端本地排队消耗 deadline。HTTP/2 `SETTINGS_MAX_CONCURRENT_STREAMS`、连接池、channel ready、wait-for-ready、线程池、异步队列，都可能让 RPC 在真正发到网络前就等了一段时间。最后服务端看到的剩余 timeout 很短，刚开始处理就被取消。表现是服务端日志里大量“deadline too short”或“context canceled”，但根因在客户端队列或连接管理。

第五个问题是 timer 和 context 开销。每个带 deadline 的 RPC 通常都会创建定时器、上下文、取消回调和一些状态。几十万 QPS 下，timer heap、锁竞争、GC、取消广播、日志和指标都可能成为额外负担。deadline 本身是为了节省资源，但过细、过短、过多的 deadline 也会带来运行时成本。

第六个问题是服务端取消不及时。gRPC 可以通知 handler 这个 RPC 已取消，但不能替你终止业务代码。如果 handler 正在 CPU 密集循环、阻塞系统调用、等待不支持取消的数据库驱动，或者启动了没有绑定 context 的后台 goroutine，deadline 只会改变 RPC 状态，不会释放真实资源。

第七个问题是同质化 deadline 引发同步尖峰。所有客户端都设置同样的 1s deadline、同样的 retry backoff，故障时会同时超时、同时重试、同时打日志。线上看起来像周期性毛刺。

第八个问题是观测缺失。只看 `DEADLINE_EXCEEDED` 数量不够。你要知道 deadline 花在哪里：客户端排队、连接建立、LB pick、网络、服务端排队、业务处理、下游调用、序列化、写响应。没有分阶段指标，调 deadline 就是在猜。

面试里可以这样答：

```text
高并发下 deadline 的隐藏问题主要是取消风暴、重试放大、fan-out 预算失控、客户端本地排队消耗剩余时间、timer/context 开销、服务端忽略 cancellation，以及同质化 deadline 带来的同步超时尖峰。排查时不能只看 DEADLINE_EXCEEDED，要拆出 client queue、LB pick、network、server queue、handler、downstream 和 retry attempt 的耗时。
```

## Q045. gRPC deadline 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

deadline 在故障场景下最容易暴露一个事实：客户端没等到结果，不等于服务端没执行。这个边界必须反复强调。

崩溃时，服务端可能在任意阶段停止：还没收到请求、收到 headers 但没进 handler、handler 已经开始、数据库写入已经提交、响应已经写出但客户端没收到。客户端最后可能看到 `UNAVAILABLE`、`DEADLINE_EXCEEDED`、`CANCELLED` 或连接错误。deadline 只能说明“调用方等到时间点后放弃了”，不能证明业务副作用不存在。

重启时，要看优雅关闭和 in-flight RPC。gRPC graceful shutdown 的语义是通知客户端停止发新 RPC，让正在执行的 RPC 在一个时间窗口内完成，超过窗口再强制关闭。这里 deadline 会和 server drain timeout 叠加：如果客户端 deadline 更短，客户端会先放弃；如果 server shutdown timeout 更短，服务端可能先终止 in-flight RPC。

超时时，边界在 status 和业务结果之间。`DEADLINE_EXCEEDED` 是 RPC 层状态，不是业务回滚信号。gRPC status code 文档也提醒，状态改变类操作即使已经成功，也可能因为响应延迟而返回 deadline exceeded。比如创建订单成功了，但响应在网络上晚了；客户端超时后重试，如果没有 idempotency key，就可能创建两笔订单。

重试时，要区分 logical call 和 attempt。一次 logical call 可以有多个 retry attempt，但它们应该共享同一个 deadline。gRPC retry 还有 commit point：一旦收到 response header，RPC 就 committed，不再由 gRPC 自动 retry。这个边界很重要，因为 header 到达通常说明服务端已经开始返回应用层响应，之后再 retry 可能破坏语义。

wait-for-ready 场景下，RPC 可能在 channel 不可用时先排队。deadline 过期前 channel 变 ready，请求才会发送；deadline 先过期，请求可能根本没出客户端。这两种情况对“服务端是否执行过”的判断完全不同。

跨服务传播时，还会暴露时钟和剩余时间边界。gRPC 不直接把绝对 deadline 原样传给下游，而是转换成扣掉已耗时的 timeout，避免机器时钟不同步。但这也意味着中间层如果先排队 300ms，再调下游时，下游看到的剩余时间会更短。服务端要能处理“请求刚到就已过期”的情况。

面试里可以这样答：

```text
deadline 在故障场景下暴露的核心边界是：DEADLINE_EXCEEDED 只说明客户端没在预算内拿到结果，不说明服务端没执行。崩溃和重启会让 in-flight RPC 停在不同阶段；超时可能发生在业务提交之后；retry 要共享同一个 logical deadline，并尊重 response header 后的 commit point；wait-for-ready 下请求可能还没出客户端就过期。带副作用 RPC 必须靠幂等键和状态机兜住。
```

## Q046. gRPC deadline 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

deadline 本身通常不是主要性能瓶颈。它更像一个资源止损机制：把不值得继续等待的工作及时切掉。真正的瓶颈通常在服务端排队、下游依赖、网络、序列化、数据库、连接池或线程池。但在高 QPS 下，deadline 的实现和使用方式也会带来开销。

CPU 开销主要来自定时器、上下文取消、状态检查、拦截器、日志和指标。每个 RPC 都创建 deadline timer；超时后要触发取消、关闭 stream、唤醒等待者、记录 status、传播到下游。QPS 很高时，这些操作会进入 runtime profile。尤其是大量请求同时超时，timer callback 和取消传播会形成尖峰。

内存开销来自 per-call 状态。deadline 需要保存截止时间、timer、context、取消回调、metadata、retry attempt 历史和观测数据。单个 RPC 很小，但 active RPC 多、deadline 长、服务端排队深时，这些状态会堆起来。如果 handler 忽略 cancellation，已经超时的请求还继续占用业务对象和 buffer。

锁竞争可能出现在 timer 系统、连接状态、stream map、拦截器统计、日志写入、指标聚合和上下文取消传播上。问题表现是 CPU 不低，但有效吞吐上不去；或者 deadline 过期后，大量 goroutine/thread 同时争同一批锁。

I/O 和网络通常是 deadline 触发的原因，而不是 deadline 自己的开销。比如跨机房 RTT 上升、TCP 重传、proxy 排队、数据库慢查询、下游队列变深，都会让 RPC 超过 deadline。此时单纯调大 deadline 只是让请求等更久，不一定提高系统吞吐。

还有一个容易忽略的瓶颈是观测系统。deadline 超时常常伴随大量错误日志、trace、metrics label 和告警。故障时如果每个 timeout 都打完整堆栈或大 error detail，日志系统可能成为新的瓶颈。

判断时可以按这个顺序看：

```text
deadline exceeded 变多:
  先看是 client queue、network、server queue、handler 还是 downstream 慢。

CPU 上升:
  看 timer、context cancellation、retry、日志、指标、序列化和业务 handler。

内存上升:
  看 active RPC、pending RPC、deadline 很长的等待请求、忽略 cancellation 的后台任务。

锁等待上升:
  看 timer heap、transport lock、metrics/logging lock、connection/stream state。

网络指标异常:
  看 RTT、重传、proxy timeout、跨 zone、连接复用和流控。
```

面试里可以这样答：

```text
deadline 通常不是业务性能瓶颈，它是暴露瓶颈和止损的机制。真正导致 deadline exceeded 的常见原因是服务端排队、下游慢、网络抖动、连接池或线程池耗尽。deadline 自身的开销主要在 timer、context cancellation、per-RPC 状态、日志指标和取消传播；高并发同时超时时，CPU、锁和观测系统会出现尖峰。
```

## Q047. gRPC deadline 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

这三类测试要分开看。correctness test 证明语义对，stress test 证明极端情况下不会泄漏和卡死，benchmark 证明 deadline 带来的开销和节省在哪里。

correctness test 应该先测最基本的语义。

```text
client deadline:
  未完成时到期，客户端得到 DEADLINE_EXCEEDED；
  deadline 未到期且服务端及时返回，客户端得到 OK；
  deadline 已经过期时发起 RPC，应快速失败或不进入业务逻辑。

server cancellation:
  deadline 到期后，服务端 handler 能观察到 cancellation；
  handler 退出后释放资源；
  handler 启动的下游 RPC 能收到取消或更短 deadline。

propagation:
  上游 deadline 传播到下游；
  下游收到的是剩余 timeout，而不是原始完整 timeout；
  子调用 deadline 不能长于父调用剩余时间。

status boundary:
  deadline exceeded 和 caller cancelled 区分清楚；
  服务端业务错误不能错误映射成 DEADLINE_EXCEEDED；
  成功响应晚到时，客户端按 deadline 返回超时。

retry:
  retry attempts 共享同一个 logical deadline；
  剩余时间不足时不再发起新 attempt；
  response header 到达后不再自动 retry。
```

还要测传输编码。gRPC over HTTP/2 用 `grpc-timeout` 表示 timeout，数值最多 8 位，单位有小时、分钟、秒、毫秒、微秒、纳秒。实现里要测单位换算、四舍五入、极小 timeout、极大 timeout、缺省 timeout、metadata 顺序和非法值处理。

stress test 要制造大量并发 deadline。比如 10 万个不同截止时间的 RPC，同时过期的一批 RPC，随机取消，一边 retry 一边超时，服务端慢读，客户端 wait-for-ready 排队，下游 fan-out，再加服务端重启和网络抖动。重点看 timer 泄漏、goroutine/thread 泄漏、active stream 是否归零、下游是否继续跑、日志是否爆炸、取消传播有没有死锁。

benchmark 要分两组：deadline 正常路径的开销，以及过载时的收益。

```text
正常路径:
  无 deadline vs 有 deadline 的 QPS、p50/p99、alloc/op、CPU；
  不同 deadline 数量和 timer 分布下的开销；
  unary、server streaming、bidi streaming 分别测。

超时路径:
  大量 RPC 同时超时时的 CPU 峰值；
  cancellation latency；
  服务端停止无用工作的速度；
  retry attempts 数量；
  active RPC 和内存下降速度。

系统收益:
  有 deadline 时过载恢复时间；
  无 deadline 时队列积压和内存增长；
  deadline 传播后下游节省的 CPU/IO。
```

面试里可以这样答：

```text
correctness test 测 deadline 到期、server cancellation、传播剩余 timeout、status 映射、retry 共享预算和 grpc-timeout 编码。stress test 测大量并发 timer、同时超时、随机取消、wait-for-ready 排队、fan-out、服务重启和网络抖动下是否泄漏、死锁或继续做无用工作。benchmark 则测正常路径的 timer/context 开销，以及过载时 deadline 对 CPU、内存、active RPC、取消延迟和恢复时间的改善。
```

## Q048. 如果要求从零实现一个简化版 gRPC deadline，你会先定义哪些不变量？

**回答：**

我会先定义时间预算和状态转换的不变量。deadline 实现不是“起一个定时器，到点返回错误”这么简单；它要和 RPC 生命周期、取消、retry、下游传播、stream 关闭、业务 handler 协作。

第一组是时间不变量。

```text
deadline:
  一个 logical RPC 最多有一个总体 deadline；
  deadline 表示调用方愿意等待到的最晚时间；
  no deadline 等价于无限等待，但生产默认不推荐；
  child RPC 的 deadline 不能晚于 parent RPC 的剩余 deadline。

remaining timeout:
  传播给下游时使用剩余时间；
  剩余时间 <= 0 时不再发起下游调用；
  剩余时间计算尽量使用单调时钟；
  跨机器传输时不要依赖两端墙上时钟完全同步。
```

第二组是状态不变量。一个 RPC 只能有一个最终状态：`OK`、`DEADLINE_EXCEEDED`、`CANCELLED`、业务错误、transport error 等。deadline 到期、客户端主动取消、服务端返回、连接错误、retry attempt 结束，可能同时发生；实现必须保证只有一个结果赢，其他事件被忽略或变成清理动作。

第三组是取消不变量。deadline 到期必须触发本地 cancellation；服务端收到 cancellation 后，框架要让 handler 能观察到；handler 启动的下游 RPC 要使用同一个 context 或更短 deadline；取消动作必须幂等，重复 cancel 不能重复关闭 stream、重复释放资源或重复调用回调。

第四组是 retry 不变量。多个 retry attempts 属于同一个 logical RPC，它们共享同一个 deadline。每次 attempt 开始前都要检查剩余时间；attempt 的 per-try timeout 不能超过剩余 deadline；一旦 RPC committed，比如收到 response header，就不能再由框架自动 retry。

第五组是资源不变量。RPC 结束后必须释放 timer、context、stream、buffer、回调和观测状态。deadline 已经过期的请求不能无限留在 wait-for-ready 队列、连接池队列或应用队列里。服务端 handler 如果已经退出，下游取消也要收敛，不能留下孤儿 goroutine。

第六组是传输不变量。发送到 HTTP/2 的 timeout 要编码为合法 `grpc-timeout`；timeout header 应该靠近伪头和 call definition；缺省 timeout 要有明确策略；收到非法 timeout 要按协议或实现策略处理，不能让解析错误绕过 deadline。

第七组是观测不变量。每次 RPC 至少能记录 deadline、remaining time、start time、end status、是否本地超时、是否远端取消、retry attempts、排队时间和 handler 时间。没有这些字段，线上 `DEADLINE_EXCEEDED` 只是一串没有方向的错误码。

面试里可以这样答：

```text
从零实现简化版 gRPC deadline，我会先定这些不变量：logical RPC 只有一个总体 deadline；子调用 deadline 不能超过父调用剩余时间；传播时传剩余 timeout 而不是绝对时间；deadline 到期触发幂等 cancellation；RPC 只能有一个最终状态；retry attempts 共享同一预算；RPC 结束必须释放 timer、stream、buffer 和回调；grpc-timeout 编码合法；观测里能拆出排队、执行、重试和取消来源。
```

## Q049. gRPC deadline 的常见误用是什么，误用后通常会产生什么线上症状？

**回答：**

最常见的误用是完全不设置 deadline。gRPC 默认不设置 deadline，客户端可能一直等待。线上症状是请求卡住、连接和 stream 长期占用、线程池耗尽、goroutine 增长、调用方堆积，最后故障从一个慢下游扩散到上游。

第二个误用是所有 RPC 用同一个固定 deadline。比如全系统统一 1s。简单查询可能太宽松，慢慢吞掉资源；复杂写操作可能太紧，正常情况下也大量超时。症状是某些方法 `DEADLINE_EXCEEDED` 长期偏高，而另一些方法明明失败了却拖很久才释放。

第三个误用是每层服务重新设置一个完整 timeout。用户请求总预算 1s，A 调 B 给 1s，B 调 C 又给 1s，C 调 D 还是 1s。最后上游早已超时，下游还在跑。症状是客户端失败后，后端 CPU、数据库和下游 QPS 仍然持续一段时间。

第四个误用是 retry 刷新 deadline。一次调用失败后，每次 retry 都重新开始计时，导致用户感知延迟远超预算，还会放大流量。症状是故障时 retry attempts 飙升，`DEADLINE_EXCEEDED` 和 `UNAVAILABLE` 一起涨，下游恢复更慢。

第五个误用是服务端不处理 cancellation。handler 收到请求后启动后台任务、数据库查询或下游 RPC，却不绑定 context，也不周期性检查是否取消。症状是客户端已经超时，服务端还在写日志、跑 SQL、发消息；active RPC 看似下降了，CPU 和 DB 负载没降。

第六个误用是把 `DEADLINE_EXCEEDED` 当作“服务端没执行”。这是很危险的。带副作用的接口超时后直接重试，可能造成重复创建、重复扣款、重复发消息。症状是业务侧出现重复数据，而 RPC 监控只显示超时和重试。

第七个误用是 deadline 过短，用它掩盖容量问题。短 deadline 可以让客户端快点失败，但如果根因是服务端容量不够、数据库慢、连接池小，短 deadline 只会让错误更快出现。症状是错误率上升、重试上升、p99 下降一点但成功率变差。

第八个误用是缺少分阶段指标。只按 method 统计 `DEADLINE_EXCEEDED`，不知道耗时发生在 client queue、LB pick、connect、server queue、handler 还是 downstream。症状是团队反复调 timeout，没有稳定改善。

面试里可以这样答：

```text
deadline 常见误用包括：不设置 deadline；所有方法统一 timeout；每层重新给完整预算；retry 刷新 deadline；服务端忽略 cancellation；把 DEADLINE_EXCEEDED 当成未执行；用短 deadline 掩盖容量问题；缺少分阶段观测。线上症状通常是请求卡死、active RPC 堆积、超时风暴、重试放大、客户端失败后服务端仍忙、重复副作用，以及 timeout 越调越乱。
```

## Q050. gRPC deadline 在单机和分布式环境中的语义有什么差异？

**回答：**

deadline 的协议语义在单机和分布式里是一致的：调用方设置时间预算，超时后客户端不再等待，服务端收到 cancellation 后应该停止相关工作，向下游传播时使用剩余 timeout。但工程语义差别很大。

单机环境里，client 和 server 可能在同一个进程、同一台机器或 loopback 上。网络抖动小，时钟差异小，连接建立快，服务发现和代理链路也少。deadline 主要表现为“handler 慢不慢”“timer 到没到”。很多测试在单机里都很漂亮，因为没有真实的排队、跨节点 RTT、proxy timeout、负载均衡和下游级联。

分布式环境里，deadline 会穿过多层组件：客户端库、连接池、DNS、负载均衡、sidecar、ingress、服务端队列、业务 handler、数据库、缓存、消息队列、下游 RPC。任何一层都可能消耗预算。一个 500ms deadline 到达真正业务代码时，可能只剩 80ms。

第二个差异是传播。单机里你可能直接传一个 context 就够了；分布式里，deadline 要通过 `grpc-timeout` 或语言 runtime 的 metadata 传给远端。gRPC 会把绝对 deadline 转成剩余 timeout，扣除已经消耗的时间，避免时钟偏移。但如果某一层没有传播，或者手动创建了新的 context，预算就断了。

第三个差异是取消效果。单机里取消通常能很快传到 handler；分布式里，取消信号要经过网络和代理。即使服务端收到取消，正在执行的数据库查询、外部 HTTP 请求、消息投递，也不一定支持取消。结果是 RPC 层看起来结束了，真实资源还没释放。

第四个差异是重试和副作用。单机测试里，超时后重试可能只是多跑一次函数；分布式里，第一次 attempt 可能已经到某个后端并提交副作用，第二次 attempt 可能打到另一个后端。没有幂等键和共享状态，deadline 会把“不知道是否执行”的问题暴露出来。

第五个差异是容量和公平性。单机里 deadline 太短只影响本机测试结果；分布式里，大量客户端同一时间超时、取消、重试，会影响整个集群。某个下游慢一点，上游集群可能一起超时，再把重试流量打回去。deadline 要和限流、熔断、退避、retry budget、优先级一起配置。

第六个差异是观测。单机里看一份日志就能定位；分布式里必须用 trace context 串起每一跳。你需要知道上游给了多少预算，中间层用了多少，下游收到多少，最后在哪里超时。否则同一个 `DEADLINE_EXCEEDED`，可能是客户端队列、proxy、服务端排队、业务慢、下游慢或响应写回慢。

面试里可以这样答：

```text
deadline 的协议语义在单机和分布式里一样，但分布式环境会放大它的工程边界。单机里主要测 handler 是否按时间返回；分布式里，预算会被连接池、LB、proxy、服务端队列和下游调用不断消耗。deadline 要通过剩余 timeout 传播，取消信号也要跨进程传递。超时不代表副作用没发生，重试可能打到不同后端，所以必须配合幂等、trace、retry budget、限流和分阶段延迟指标。
```

## Q051. status code 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

gRPC status code 的核心目标，是给一次 RPC 一个稳定、跨语言、机器可读的最终结果。客户端不能只靠字符串错误消息判断发生了什么，也不能只看 HTTP status。gRPC 的语义结果在 `grpc-status` 里，成功是 `OK`，失败则是明确的错误码，比如 `INVALID_ARGUMENT`、`NOT_FOUND`、`UNAVAILABLE`、`DEADLINE_EXCEEDED`、`ABORTED`、`RESOURCE_EXHAUSTED`。

官方 status code 文档里有两个点很关键。第一，所有 RPC 最终都会向客户端返回一个 status；这个 status 由整数 code 和字符串描述组成。第二，有些 status code 可能由 gRPC library 生成，有些 code 官方明确说不会由 library 生成，只会由用户代码返回，比如 `INVALID_ARGUMENT`、`NOT_FOUND`、`ALREADY_EXISTS`、`FAILED_PRECONDITION`、`ABORTED`、`OUT_OF_RANGE`、`DATA_LOSS`。这对排查非常有用：看到这些 code 时，通常可以先从业务逻辑找，而不是先怀疑网络层。

所以 status code 首先解决正确性和可维护性问题。

```text
正确性:
  调用方能知道 RPC 的最终语义结果；
  retry、fallback、幂等、补偿可以基于稳定 code 决策；
  客户端能区分参数错、权限错、资源耗尽、服务不可用、并发冲突、deadline 超时。

可维护性:
  多语言客户端用同一套错误契约；
  监控、告警、SLO、日志和 trace 可以按 code 聚合；
  API 文档可以说明每个方法可能返回哪些 code；
  客户端 SDK 可以把 code 映射成稳定异常类型。
```

它不主要解决性能问题。一个准确的 status code 不会让慢查询变快，也不会减少网络往返。但它会影响性能策略：`UNAVAILABLE` 可能触发带退避的重试，`RESOURCE_EXHAUSTED` 可能触发限流或降级，`INVALID_ARGUMENT` 则不应该重试。错误码用错，会把性能问题放大成重试风暴。

它也不是安全机制。`UNAUTHENTICATED` 和 `PERMISSION_DENIED` 能表达认证/鉴权结果，但真正的安全来自 token 验证、mTLS、ACL、RBAC、审计和服务端策略。status code 只是把结果告诉调用方。不要因为返回了 `PERMISSION_DENIED`，就以为敏感信息不会泄露；错误消息和 details 里仍然可能泄漏资源名、内部规则或调试数据。

面试里可以这样答：

```text
gRPC status code 的核心目标是把一次 RPC 的最终结果变成稳定、跨语言、机器可读的契约。它主要解决正确性和可维护性：客户端用它决定是否重试、降级、补偿或提示用户，服务端用它做监控和 SLO 聚合。它不是性能优化，也不是安全机制；但 status code 用错会直接影响性能和安全，比如错误重试、告警失真、错误信息泄漏。
```

## Q052. status code 的典型适用场景和不适用场景分别是什么？

**回答：**

status code 适合表达“这次 RPC 调用为什么以这种方式结束”。它是 RPC 边界上的结果语义，不是任意业务状态的容器。

典型适用场景有几类。

第一类是输入错误。请求字段格式不对、必填项缺失、枚举值不合法、分页参数非法，这类通常是 `INVALID_ARGUMENT`。它和系统状态无关，客户端即使重试同样的请求也不会变好。

第二类是资源状态。资源不存在用 `NOT_FOUND`，要创建的资源已经存在用 `ALREADY_EXISTS`，当前系统状态不允许操作用 `FAILED_PRECONDITION`。比如删除非空目录、在未初始化状态下启动任务、对已经关闭的会话继续写入，都更像 precondition 问题，而不是网络失败。

第三类是并发冲突。CAS 失败、版本号不匹配、事务冲突、乐观锁失败，通常更适合 `ABORTED`。官方 status code 文档也把 `ABORTED` 描述为典型的并发问题，并建议调用方在更高层重试整个 read-modify-write 序列，而不是只重试这一次 RPC。

第四类是临时不可用。服务重启、连接断开、后端暂时不可达、server shutting down，常见是 `UNAVAILABLE`。它常常是 transient，但不等于所有操作都能安全重试。非幂等写操作仍然要看是否有幂等键。

第五类是时间预算和取消。客户端不再等待通常是 `DEADLINE_EXCEEDED`；调用方主动取消通常是 `CANCELLED`。这两者都不是业务失败的通用替代品。

第六类是安全和配额。没有身份用 `UNAUTHENTICATED`；身份有效但没有权限用 `PERMISSION_DENIED`；配额、并发、内存、磁盘、流控资源耗尽用 `RESOURCE_EXHAUSTED`。这些 code 对客户端行为差别很大：重新登录、申请权限、退避限流，处理方式完全不同。

不适用场景也要说清。

第一，不要用 status code 表达正常业务分支。比如“用户没有优惠券”“搜索结果为空”“库存为 0 但允许展示”，这些通常应该是成功响应里的字段，而不是错误码。只有当调用语义本身无法完成时，才应该返回非 OK。

第二，不要把 status code 当业务错误码全集。gRPC code 只有一小组通用语义；业务可以在响应体、错误 details 或领域错误码里表达更细的原因。比如 `INVALID_ARGUMENT` 可以配合 `BadRequest` details 指出哪个字段错了，但不要为每个字段错误发明新的 gRPC code。

第三，不要用 `UNKNOWN` 或 `INTERNAL` 包所有错误。`UNKNOWN` 适合信息不足或跨错误空间无法映射的情况；`INTERNAL` 适合系统不变量被破坏。把用户输入错误、权限错误、资源不存在都映射成 `INTERNAL`，会让客户端无从处理，也会污染告警。

第四，不要把 HTTP status 当 gRPC status。gRPC over HTTP/2 通常 HTTP `:status` 是 200，真正的 RPC 结果在 trailers 的 `grpc-status` 里。代理或非 gRPC 服务返回非 200 时，客户端库会合成一个 gRPC status 给应用层，但那是降级处理，不是正常业务契约。

面试里可以这样答：

```text
status code 适合表达 RPC 边界上的最终结果：参数错误、资源不存在、权限失败、配额耗尽、并发冲突、deadline 超时、服务不可用。它不适合表达正常业务分支，也不应该承载所有领域错误细节。业务细节可以放在 response 字段、错误 details 或业务错误码里；gRPC status code 负责稳定的上层决策，比如是否重试、是否提示用户、是否触发告警。
```

## Q053. status code 和相近概念最容易混淆的边界在哪里？

**回答：**

status code 最容易和 HTTP status、异常类型、错误消息、业务错误码、错误 details、重试策略、告警等级混在一起。它们都在“错误处理”附近，但边界不同。

第一，gRPC status code 不等于 HTTP status。gRPC over HTTP/2 的正常响应通常是 HTTP 200，然后在 trailers 里放 `grpc-status`。协议文档也明确说，status 必须通过 trailers 发送，即使 code 是 OK。HTTP status 更多是传输和代理层结果；gRPC status 才是 RPC 语义结果。非 gRPC 网关、代理、负载均衡器返回非 200 时，客户端库可能合成 status，但这不是应用正常返回的业务错误。

第二，status code 不等于异常类型。Java、Go、Python、C++ 对错误的 API 包装方式不同：有的抛异常，有的返回 error，有的返回 status object。跨语言能稳定依赖的是 status code 和标准错误模型，不是某个语言的异常类名。

第三，status code 不等于错误消息。错误消息是给人看的，可能会本地化、脱敏、改文案，也可能不稳定。客户端逻辑不应该 parse `"user not found"` 这种字符串来决定行为，而应该看 `NOT_FOUND` 和结构化 details。

第四，status code 不等于业务错误码。比如支付失败可能有余额不足、银行卡过期、风控拒绝、通道关闭。它们不一定都要变成不同 gRPC code。可以统一用合适的 gRPC code，再用业务字段或 error details 表达细节。

第五，status code 不等于 retryable。`UNAVAILABLE` 通常表示暂时不可用，官方文档也说它多半可以通过 backoff 重试修正，但同时提醒非幂等操作并不总是安全重试。`ABORTED` 可能要重试更高层事务序列；`RESOURCE_EXHAUSTED` 可能要等配额恢复；`DEADLINE_EXCEEDED` 可能服务端已经成功执行。能不能重试要结合方法幂等性、commit point、deadline、retry budget 和服务端返回的细节。

第六，status code 不等于告警等级。`INVALID_ARGUMENT` 很多时是客户端 bug 或用户输入问题，不一定要报警服务端；少量 `UNAVAILABLE` 可能是滚动发布或瞬时网络；`INTERNAL` 比例上升通常更值得关注。告警应该按 method、caller、target、status、比例和影响面综合判断。

第七，status code 不等于错误 details。gRPC 标准错误模型只有 code 和 message；更丰富的错误模型可以把 protobuf details 放在 trailing metadata 里。官方 error handling 文档也提醒，rich error model 存在跨语言支持差异、代理/日志不可见、HTTP/2 header 压缩效率下降、header size limit 等问题。所以 details 是补充，不是替代 status code。

面试里可以这样答：

```text
gRPC status code 是 RPC 语义结果，不是 HTTP status、异常类名、错误字符串、业务错误码、retryable 标记或告警等级。HTTP status 主要是传输层，gRPC status 在 trailers；错误消息给人看，客户端逻辑看 code；业务细节可以放 response 或 error details；是否重试还要结合幂等、deadline、commit point 和 retry policy。
```

## Q054. status code 在高并发场景下可能出现哪些隐藏问题？

**回答：**

高并发下，status code 的问题通常不在编码本身，而在它驱动了错误处理、重试、监控和日志。code 用错，会把小故障放大；code 太粗，会让排查失焦；details 太大，会让错误路径变成性能热点。

第一个隐藏问题是重试放大。客户端如果把 `UNAVAILABLE`、`RESOURCE_EXHAUSTED`、`DEADLINE_EXCEEDED` 都当成可立即重试，高并发下会形成 retry storm。gRPC retry 文档强调，重试要限制 max attempts、backoff、retryable status codes，并监控 retry metrics。status code 是 retry policy 的输入，不是单独的重试许可。

第二个问题是错误码塌缩。很多系统在压力大时把所有异常都返回 `UNKNOWN` 或 `INTERNAL`。短期看简单，长期看监控完全失真：参数错误、超时、下游不可用、配额不足、认证失败都混在一起，SRE 不知道先扩容、修客户端、查权限还是限流。

第三个问题是高基数指标。status code 本身基数很低，但如果把 `grpc-message`、业务错误文本、资源 id、用户 id、详细原因都作为 metrics label，就会把时序数据库打爆。正确做法是用 `grpc.status`、method、target、caller 等有限维度聚合，细节放日志或 trace。

第四个问题是大错误 details。rich error model 把 protobuf error details 放在 trailers 里。高并发错误路径下，如果每个错误都带大 stack trace、大字段校验列表或大量 debug 信息，会增加 header/trailer 大小，降低 HTTP/2 header compression 效率，甚至触发 max header size 限制，导致原始错误丢失。

第五个问题是错误路径比成功路径更贵。成功响应可能很小；错误时反而序列化复杂 details、打多行日志、记录 trace event、上报 metrics、触发告警、执行补偿。高并发故障时，错误处理本身会吃掉大量 CPU 和 I/O。

第六个问题是客户端分布式行为不一致。不同语言 SDK 对 status、details、异常包装、metadata 访问支持不同。某些客户端能读到 `grpc-status-details-bin`，某些读不到；某些把代理返回的非 gRPC 响应合成为 `UNKNOWN`，某些暴露成不同异常。高并发多语言调用时，错误行为可能分裂。

第七个问题是安全侧信道。压力大时系统常常打开更详细错误消息，结果 `PERMISSION_DENIED`、`NOT_FOUND`、`FAILED_PRECONDITION` 的消息里带出内部资源名、策略名、SQL 错误或部署信息。并发越高，泄漏面越大。

面试里可以这样答：

```text
高并发下 status code 的隐藏问题主要是：错误码驱动重试导致放大，所有错误塌缩成 UNKNOWN/INTERNAL 导致监控失真，错误 details 和日志在故障时变成热点，高基数标签打爆指标系统，多语言客户端对 details 支持不一致，以及错误消息泄漏内部信息。status code 要稳定、低基数、可聚合，详细信息要受大小和脱敏控制。
```

## Q055. status code 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

这些场景下最重要的边界是：客户端看到的 status code 不一定由业务 handler 返回。它可能由 gRPC library、客户端 runtime、服务端 runtime、代理或连接错误合成。换句话说，status code 表达的是调用方最终观察到的 RPC 结果，不总是服务端业务代码的显式决定。

崩溃时，服务端可能还没来得及发送 trailers。gRPC over HTTP/2 的正常 status 在 trailers 里；如果进程崩溃、连接断开、RST_STREAM、代理关闭，客户端可能收不到 `grpc-status`。协议文档要求客户端在遇到非 gRPC 响应、缺失 status/message 等 broken deployment 时合成 status 传播给应用层。这时客户端看到的可能是 `UNAVAILABLE`、`UNKNOWN`、`INTERNAL`，但它不是业务 handler 写出的 code。

重启时，优雅关闭和强制关闭的 code 可能不同。server shutting down 在官方 error handling 文档里对应 `UNAVAILABLE`。如果服务端先 drain，已有 RPC 可能正常完成；如果强制关闭连接，in-flight RPC 可能变成 `UNAVAILABLE` 或其他 transport error。客户端不能只根据 `UNAVAILABLE` 判断业务是否没执行。

超时时，`DEADLINE_EXCEEDED` 是观察结果，不是事务结果。官方 status code 文档明确说，对会改变系统状态的操作，即使操作已经成功完成，也可能因为响应延迟而返回 `DEADLINE_EXCEEDED`。所以带副作用操作超时后重试，必须有幂等键或业务状态查询。

重试时，status code 影响 attempt 行为。gRPC retry 文档说，只有当 RPC 以 retry policy 中配置的 retryable status code 失败、且没超过 attempt 限制时，gRPC 才会创建新的 retry stream；一旦收到 response header，RPC 就 committed，不再自动 retry。这个边界说明：同一个 logical call 最终看到一个 status，但中间 attempts 可能经历了多个 status。

还有一个边界是“请求是否到达业务逻辑”。gRPC transparent retry 只有在库能确定请求没被业务逻辑处理时才会做更安全的重试；如果请求已经进入业务 handler，status code 就不能替你判断副作用。协议里的 idempotency 也说，除非明确声明，否则 gRPC call 不默认幂等。

代理会让边界更复杂。客户端到代理是 gRPC，代理到后端也是 gRPC，或者中间还有 HTTP/JSON 转码。代理可能把 HTTP 503 映射成 `UNAVAILABLE`，把 header size exceeded 映射成 `RESOURCE_EXHAUSTED` 或 `UNKNOWN`，也可能丢失 trailers。排查时要看每一跳的 status，而不是只看最外层。

面试里可以这样答：

```text
崩溃、重启、超时和重试场景下，status code 暴露的核心边界是：客户端看到的 code 可能是 runtime 或代理合成的，不一定是业务 handler 返回的。正常 gRPC status 在 trailers 里，连接断开时可能根本没有 trailers。DEADLINE_EXCEEDED 不代表副作用没发生，UNAVAILABLE 不代表一定可安全重试；重试还受 commit point、retry policy、幂等性和 attempt 历史约束。
```

## Q056. status code 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

单独的 status code 几乎不是性能瓶颈。一个整数 code 和一段短 message 成本很低。真正的瓶颈通常来自 status code 周围的东西：错误 details、trailers、日志、metrics、trace、重试和客户端分支逻辑。

CPU 瓶颈主要出现在错误路径处理。高错误率时，服务端要构造 status、序列化 details、格式化错误消息、写日志、上报指标、结束 stream、触发 retry 或补偿逻辑。客户端也要解析 trailers、构造异常、执行 retry policy、记录 trace。错误路径平时没什么，故障时会突然变热。

内存瓶颈来自大 message 和 rich error details。标准 error model 只有 code 和 string message；rich error model 可以把 protobuf details 放在 trailing metadata 里。details 如果包含大量字段错误、堆栈、debug 信息、下游错误列表，会增加 per-RPC 内存、metadata buffer 和 HTTP/2 header/trailer 大小。

锁竞争通常来自观测系统，而不是 code 本身。比如按 status 聚合 metrics、写共享日志 sink、更新 retry counters、记录 trace span、限流器按 status 反馈 token。高并发错误时，这些共享结构可能出现锁等待。

I/O 瓶颈常见于日志和告警。错误率一高，系统可能为每个非 OK status 打一行甚至多行日志，带上 request、response、stack trace、details。磁盘、日志 agent、网络日志管道和告警系统都可能被打满。

网络瓶颈来自 trailers 和重试。gRPC status 在 trailers 中传递；大 details 会增加 HTTP/2 header/trailer 字节数。官方 error handling 文档也提醒，额外 error detail 会影响 head-of-line blocking，降低 HTTP/2 header compression 效率，并可能碰到 max headers size。另一个网络问题是 status code 触发重试，错误越多，重试流量越多。

实际排查时可以这样拆：

```text
code 本身:
  成本很低。

错误 details:
  看 trailer size、max header size、序列化和跨语言解析。

日志/指标/trace:
  看错误路径 CPU、锁等待、日志 I/O、高基数标签。

retry:
  看 retry attempts、retry delay、transparent retry、下游 QPS 放大。

代理链路:
  看 trailers 是否保留、是否被截断、是否被转成 HTTP 错误。
```

面试里可以这样答：

```text
status code 本身不是性能瓶颈，瓶颈来自围绕它的错误处理。CPU 花在 details 序列化、异常构造、retry policy 和观测埋点；内存花在大错误 details 和 metadata；锁竞争常在 metrics/logging/retry counter；I/O 常在错误日志；网络开销来自 trailers、header compression 下降和重试流量。故障时错误路径会从冷路径变成热路径。
```

## Q057. status code 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

status code 的测试重点不是“能不能返回一个数字”，而是语义映射、传输位置、跨语言一致性、重试行为和观测是否正确。

correctness test 先测映射。

```text
业务错误映射:
  invalid field -> INVALID_ARGUMENT；
  missing resource -> NOT_FOUND；
  duplicate create -> ALREADY_EXISTS；
  permission missing -> PERMISSION_DENIED；
  no credentials -> UNAUTHENTICATED；
  quota exceeded -> RESOURCE_EXHAUSTED；
  optimistic lock conflict -> ABORTED；
  unsupported method -> UNIMPLEMENTED。

系统错误映射:
  deadline -> DEADLINE_EXCEEDED；
  caller cancel -> CANCELLED；
  server shutdown -> UNAVAILABLE；
  invariant broken -> INTERNAL；
  unknown external error -> UNKNOWN。
```

然后测传输和客户端行为。服务端返回非 OK 时，客户端能拿到正确 code、message、metadata 和 details；OK 时不能带矛盾的 error details；`grpc-status-details-bin` 里的 status code 不能和 `grpc-status` 冲突。gRPC over HTTP/2 协议也要求 Status-Details 如果含 status code，不能和 Status header 矛盾，consumer 要检查这一点。

还要测代理和异常路径：服务端只返回 trailers-only；响应 header 后再返回错误 trailers；连接中断导致没有 trailers；非 gRPC HTTP 响应；非法 `grpc-status`；非法 percent-encoded message；过大的 trailers。客户端应该合成或暴露合理 status，而不是 panic 或吞掉错误。

retry correctness 要单独测。配置 retry policy 后，只有指定 retryable status code 触发 retry；非幂等方法不被误重试；收到 response header 后不再自动 retry；最终 status 能反映最后结果，同时 metrics 能看到 attempt 级 status。

stress test 要测高错误率。比如 10 万 QPS 下 30% `INVALID_ARGUMENT`、20% `UNAVAILABLE`、5% 大 error details、随机取消、随机 deadline、代理截断 trailers、多语言客户端混跑。目标是发现日志风暴、metrics 高基数、details 截断、内存增长、retry storm、客户端异常包装不一致。

benchmark 则测错误路径成本：

```text
baseline:
  OK vs 简单非 OK status 的 p50/p99、CPU、alloc。

details:
  无 details、小 details、大 details 的开销；
  trailers size 和 max header limit；
  客户端解析成本。

observability:
  按 status 聚合 metrics 的开销；
  trace/log 开启和关闭的差异；
  高错误率下日志 I/O。

retry:
  不同 retryable status code 配置下的 attempts、QPS 放大、恢复时间。
```

面试里可以这样答：

```text
correctness test 测业务错误到 status code 的映射、trailers 中 grpc-status 的传输、details 与 status 不矛盾、代理/非法响应下的合成 status，以及 retry policy 是否只对指定 code 生效。stress test 测高错误率、大 details、随机取消、deadline、连接中断、代理截断和多语言客户端。benchmark 测 OK 与错误路径的 CPU/alloc/p99、details 大小、日志指标开销和 retry 放大。
```

## Q058. 如果要求从零实现一个简化版 status code，你会先定义哪些不变量？

**回答：**

我会先把 status code 当成 RPC 结果契约，而不是普通整数。实现上最怕的是“状态可以多次写”“OK 和错误信息同时出现”“details 和 code 冲突”“传输错误和业务错误混在一起”。

第一组是不变量是最终状态。

```text
final status:
  每个 RPC 最终只能有一个 status；
  OK 表示成功完成；
  非 OK 表示调用没有按 RPC 语义成功完成；
  status 一旦对客户端可见，就不能再被业务代码改写。
```

第二组是 code 集合不变量。只能使用定义好的 code；未知整数要按兼容策略处理，不能让客户端崩溃；应用层不能随便发明新的 gRPC code。如果需要业务细分，用 details 或业务错误码。

第三组是来源不变量。status 要能区分来源：应用返回、服务端 runtime 生成、客户端 runtime 生成、代理/transport 合成。应用不一定直接看到这个来源字段，但日志和 trace 里最好能保留，否则排查 `UNKNOWN` 和 `UNAVAILABLE` 会很痛苦。

第四组是传输不变量。gRPC status 必须放在 trailers 或 trailers-only 响应里；HTTP status 不是 RPC status；缺失、非法或非 gRPC 响应要合成 status；status message 解码失败不能导致客户端丢掉整个错误。gRPC over HTTP/2 协议明确说，status message 解码遇到非法值时实现不能直接 error 或丢弃 message。

第五组是 details 不变量。details 只能补充错误信息，不能和主 status 冲突；OK 不应该携带 error details；details 大小要有限制；敏感字段要脱敏；客户端不支持 details 时，仍然能靠 code 和 message 做基本处理。

第六组是 retry 不变量。retry policy 只能读稳定 status code；不是所有非 OK 都能重试；`UNAVAILABLE` 也要结合幂等性；收到 response header 后的 committed RPC 不能随便由框架重试；最终 status 和 attempt status 都要可观测。

第七组是观测不变量。metrics label 里只能放低基数字段，比如 method、target、status、caller；不要把 message、resource id、用户 id 放进标签。日志里要有 request id、trace id、status、source、method、peer、deadline、attempt。

面试里可以这样答：

```text
从零实现简化版 status code，我会先定义这些不变量：每个 RPC 只有一个最终 status；OK 和非 OK 语义互斥；只能使用标准 code；status source 要能排查；gRPC status 在 trailers 里，HTTP status 不能替代它；非法或缺失 status 要合成稳定错误；details 不能和主 code 冲突；retry policy 只能基于稳定 code 和幂等性；metrics 标签保持低基数。
```

## Q059. status code 的常见误用是什么，误用后通常会产生什么线上症状？

**回答：**

第一个误用是返回 `OK`，然后在 response body 里塞 `success=false` 或业务错误码。这样 gRPC 层、监控、重试、客户端拦截器都会认为调用成功。线上症状是 SLO 看起来很好，用户却在报错；调用链没有错误 span，告警也不触发。

第二个误用是所有错误都返回 `UNKNOWN` 或 `INTERNAL`。这会让客户端无法区分参数问题、权限问题、资源不存在、配额不足、并发冲突和服务不可用。症状是告警很多但不可行动，客户端只能展示“系统错误”，也不知道能不能重试。

第三个误用是把业务正常分支当错误。比如搜索为空返回 `NOT_FOUND`，用户没有优惠券返回 `FAILED_PRECONDITION`，库存为 0 返回 `RESOURCE_EXHAUSTED`。症状是错误率虚高，SLO 被污染，客户端和 SRE 以为系统有故障。

第四个误用是错误重试语义。把所有下游错误都映射成 `UNAVAILABLE`，客户端按 retry policy 自动重试，结果非幂等写操作被重复执行。症状是重复订单、重复扣款、重复消息，同时下游 QPS 在故障时被放大。

第五个误用是认证和鉴权混淆。没有登录应该是 `UNAUTHENTICATED`，登录了但没有权限是 `PERMISSION_DENIED`。混用以后，客户端可能反复刷新 token，或者该提示申请权限时却让用户重新登录。

第六个误用是 `INVALID_ARGUMENT`、`FAILED_PRECONDITION`、`ABORTED` 混用。参数本身不合法是 `INVALID_ARGUMENT`；系统状态不满足是 `FAILED_PRECONDITION`；并发冲突、事务 abort 更像 `ABORTED`。混用以后，客户端无法知道是修请求、等状态变化，还是重启更高层事务。

第七个误用是 error message 里放敏感信息。比如 SQL、内部 host、用户隐私、权限规则、token 片段。线上症状是日志和客户端错误提示泄漏内部实现。

第八个误用是过大的 error details。把整段 stack trace、下游响应、校验列表全部塞进 trailers。症状是偶发 `RESOURCE_EXHAUSTED`、`UNKNOWN`、header too large，或者代理丢 trailers，客户端反而拿不到原始错误。

面试里可以这样答：

```text
status code 常见误用包括：OK response 里塞业务失败；所有错误都用 UNKNOWN/INTERNAL；把正常业务分支当错误；把所有下游失败映射成 UNAVAILABLE 导致重试放大；混淆 UNAUTHENTICATED 和 PERMISSION_DENIED；混淆 INVALID_ARGUMENT、FAILED_PRECONDITION、ABORTED；错误消息泄漏敏感信息；details 过大。线上症状是 SLO 失真、告警不可行动、客户端乱重试、重复副作用、错误率虚高和 trailers 被截断。
```

## Q060. status code 在单机和分布式环境中的语义有什么差异？

**回答：**

status code 的定义在单机和分布式环境里一样，但它的来源、可信度和排查方式不同。单机里，status 往往就是 handler 直接返回的结果；分布式里，status 可能来自很多层。

单机环境里，客户端和服务端可能在同一进程或同一台机器，网络失败少，代理少，trailers 不容易丢。你看到 `INVALID_ARGUMENT`，基本就是业务代码返回；看到 `INTERNAL`，通常也是本地 handler 或 runtime 出错。测试和调试都比较直接。

分布式环境里，一次 RPC 可能经过 client library、DNS、负载均衡、sidecar、ingress、service mesh、server transport、业务 handler、下游服务。最终 status 可能是：

```text
业务 handler 返回的；
服务端 runtime 生成的；
客户端 runtime 生成的；
代理把 HTTP/2 或 upstream 错误映射出来的；
连接断开后客户端合成的；
deadline/cancellation/retry 过程中的最终观察结果。
```

第二个差异是 status 的局部性。A 调 B 得到 `UNAVAILABLE`，不代表 B 的业务 handler 返回了 `UNAVAILABLE`；可能是 A 到 sidecar 的连接断了，也可能是 sidecar 到 B 的 upstream 出错。B 调 C 得到的 status 也不一定会原样透传给 A。中间服务应该决定是否包装、转换或保留 details，而不是无脑透传所有下游错误。

第三个差异是重试和负载均衡。单机里重试只是再次调用同一个服务；分布式里，下一次 attempt 可能打到另一个后端。`UNAVAILABLE`、`ABORTED`、`RESOURCE_EXHAUSTED` 的处理要考虑幂等性、服务端是否已执行、retry budget、负载均衡和全局容量。

第四个差异是安全。分布式环境里，错误会跨服务边界、跨团队、甚至跨公网。内部服务返回的 `INTERNAL` message 或 details 不能原样暴露给外部客户端。边界服务通常要做错误翻译和脱敏。

第五个差异是观测。单机里看一份日志就能定位；分布式里要按 trace id 串起每一跳的 status。最好同时保留 upstream status、downstream status、最终对外 status、status source、attempt number。否则一个最外层 `UNKNOWN` 可能掩盖内部的 `PERMISSION_DENIED`、`RESOURCE_EXHAUSTED` 或 `DEADLINE_EXCEEDED`。

第六个差异是 SLO 口径。单机服务可以按自己的 status 算错误率；分布式系统要区分 caller 视角和 callee 视角。客户端看到 `DEADLINE_EXCEEDED`，服务端可能已经返回 OK 只是响应晚了；服务端看到 `CANCELLED`，客户端可能是 deadline 到了，也可能是用户主动放弃。SLO 要定义清楚到底按哪一侧计算。

面试里可以这样答：

```text
status code 的定义在单机和分布式里一样，但分布式环境会让 status 的来源变复杂。单机里 status 多半是 handler 返回；分布式里它可能由客户端库、服务端库、代理、连接错误、deadline 或 retry 合成。中间服务不能无脑透传下游错误，要做语义转换和脱敏。排查时要看每一跳的 status、source、attempt 和 trace，而不是只看最外层 code。
```

## 参考资料

- [RFC 9113: HTTP/2](https://www.rfc-editor.org/rfc/rfc9113.html)
- [RFC 7301: TLS Application-Layer Protocol Negotiation Extension](https://datatracker.ietf.org/doc/html/rfc7301)
- [gRPC Introduction](https://grpc.io/docs/what-is-grpc/introduction/)
- [gRPC Core concepts, architecture and lifecycle](https://grpc.io/docs/what-is-grpc/core-concepts/)
- [gRPC over HTTP2 Protocol](https://grpc.github.io/grpc/core/md_doc__p_r_o_t_o_c_o_l-_h_t_t_p2.html)
- [gRPC Deadlines](https://grpc.io/docs/guides/deadlines/)
- [gRPC Cancellation](https://grpc.io/docs/guides/cancellation/)
- [gRPC Status Codes](https://grpc.io/docs/guides/status-codes/)
- [gRPC Error handling](https://grpc.io/docs/guides/error/)
- [gRPC Flow Control](https://grpc.io/docs/guides/flow-control/)
- [gRPC Retry](https://grpc.io/docs/guides/retry/)
- [gRPC Request Hedging](https://grpc.io/docs/guides/request-hedging/)
- [gRPC Service Config](https://grpc.io/docs/guides/service-config/)
- [gRPC Wait-for-Ready](https://grpc.io/docs/guides/wait-for-ready/)
- [gRPC Custom Name Resolution](https://grpc.io/docs/guides/custom-name-resolution/)
- [gRPC Health Checking](https://grpc.io/docs/guides/health-checking/)
- [gRPC Debugging](https://grpc.io/docs/guides/debugging/)
- [gRPC OpenTelemetry Metrics](https://grpc.io/docs/guides/opentelemetry-metrics/)
- [gRPC Load Balancing](https://grpc.io/blog/grpc-load-balancing/)
- [gRPC Custom Load Balancing Policies](https://grpc.io/docs/guides/custom-load-balancing/)
- [gRPC Keepalive](https://grpc.io/docs/guides/keepalive/)
- [gRPC Authentication](https://grpc.io/docs/guides/auth/)
- [gRPC Metadata](https://grpc.io/docs/guides/metadata/)
- [gRPC Interceptors](https://grpc.io/docs/guides/interceptors/)
- [gRPC Performance Best Practices](https://grpc.io/docs/guides/performance/)
- [gRPC Benchmarking](https://grpc.io/docs/guides/benchmarking/)
- [gRPC Graceful Shutdown](https://grpc.io/docs/guides/server-graceful-stop/)
- [Google API Improvement Proposal AIP-193: Errors](https://google.aip.dev/193)
- [google.rpc error details proto](https://github.com/googleapis/googleapis/blob/master/google/rpc/error_details.proto)
- [Protocol Buffers proto3 Language Guide](https://protobuf.dev/programming-guides/proto3/)
- [Protocol Buffers Proto Best Practices](https://protobuf.dev/best-practices/dos-donts/)
- [Protocol Buffers ProtoJSON Format](https://protobuf.dev/programming-guides/json/)
- [NGINX ngx_http_grpc_module](https://nginx.org/en/docs/http/ngx_http_grpc_module.html)
- [Envoy timeout FAQ](https://www.envoyproxy.io/docs/envoy/latest/faq/configuration/timeouts)
- [Ingress-NGINX gRPC example](https://kubernetes.github.io/ingress-nginx/examples/grpc/)
