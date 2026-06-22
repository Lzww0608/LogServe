# 49. TCP TIME_WAIT、epoll、TLS certificate 与 Kubernetes readiness 追问链

这一批放四个非常工程化的主题：TCP TIME_WAIT、epoll、TLS certificate 和 Kubernetes readiness。它们看起来分散，一个在传输层，一个在 Linux I/O 多路复用，一个在 TLS 身份认证，一个在 Kubernetes 流量摘除。但面试里问法很像：一句话定义都很容易把边界说错，真正的理解要落到事故现场。

这组回答还是按 LogServe 的口径写。LogServe 可以借这些概念解释自己怎样处理连接、事件循环、TLS 部署边界和容器化探针；但项目本身不是一个内核网络栈、证书平台或 Kubernetes 控制面。说清楚哪些是项目可以观测和配置的边界，哪些属于操作系统、Ingress、Service Mesh、证书颁发系统或集群平台，面试时更稳。

## Q001. 面试官如果只问一个问题检验你是否理解 TCP TIME_WAIT，可能会问什么？

**回答：**

我会预期他问一个生产事故题：

```text
服务突然出现大量 TIME_WAIT，短时间内新连接失败，报错里有 cannot assign requested address 或 bind: address already in use。你先判断 TIME_WAIT 在客户端还是服务端？它为什么存在？能不能直接把 TIME_WAIT 调小或强行复用端口？
```

这个问题比问“TIME_WAIT 是什么状态”更能看出理解。TCP TIME_WAIT 不是异常状态，也不是连接泄漏。它主要出现在主动关闭连接的一方，主动关闭方发出 FIN，经历 FIN-WAIT，确认对端 FIN 后进入 TIME_WAIT，等待足够长的时间再真正释放连接控制块。

它有两个核心目的。第一，保证旧连接里的延迟重复报文在网络中自然消失，避免新的同四元组连接被旧报文污染。这里的四元组是源 IP、源端口、目的 IP、目的端口。第二，保证最后一个 ACK 有机会被重传。如果对端没有收到最后 ACK，它可能重发 FIN，TIME_WAIT 这一侧还能再次 ACK，帮助对端正常关闭。

面试里要先定位 TIME_WAIT 在哪边。如果是客户端主动短连接调用下游，TIME_WAIT 多半在客户端，风险是本机 ephemeral port 被耗尽，或者 NAT 网关端口被耗尽。如果是服务端主动关闭，例如 HTTP keep-alive 配置不当、服务端 idle timeout 先到、反向代理先断连接，那么 TIME_WAIT 可能压在服务端或代理上。看到 TIME_WAIT 多，不等于服务端处理慢，先看谁主动 close。

我会继续追问连接模式。是不是没有连接池？是不是 HTTP keep-alive 被禁用？是不是 gRPC 连接频繁重建？是不是 client 每个请求都新建 TCP？是不是健康检查太频繁？是不是负载均衡器、NAT、Service Mesh sidecar 改变了主动关闭方？这些比直接改内核参数更重要。

“能不能调小 TIME_WAIT”这个问题要谨慎。操作系统确实有一些端口复用、TIME_WAIT 复用、端口范围扩展和连接回收相关参数，但它们都有协议和安全边界。粗暴缩短 TIME_WAIT，可能让延迟旧包进入新连接，或者让 NAT/负载均衡场景出现更难排查的问题。优先方案通常是复用长连接、扩大 ephemeral port range、分散源 IP、调整连接池、修正 idle timeout 顺序，而不是先动 TIME_WAIT 的协议保护。

结合 LogServe，我会这样落地：如果 worker、scheduler、metadata store、object store、mock LLM 或外部 LLM 调用都用短连接，TIME_WAIT 和端口耗尽会直接影响 workflow p99 和任务重试。项目里要证明的不是“没有 TIME_WAIT”，而是连接复用、超时、重试和指标能把短连接风暴暴露出来。

## Q002. TCP TIME_WAIT 的一句话定义是否容易误导，误导点在哪里？

**回答：**

容易误导。常见一句话是：TIME_WAIT 是 TCP 关闭后等待 2MSL 的状态。这句话没错，但太短，容易让人以为它只是“关闭慢”或“系统没释放干净”。

第一个误导是没说谁进入 TIME_WAIT。通常是主动关闭的一方进入。服务端看到自己机器上 TIME_WAIT 很多，不一定说明客户端有问题，可能是服务端先关连接；客户端 TIME_WAIT 很多，也可能只是它高频短连接调用下游。排查时先看连接方向和主动 close 方。

第二个误导是把 TIME_WAIT 当成错误。TIME_WAIT 是 TCP 正常关闭路径的一部分。它保护新旧连接边界，也处理对端 FIN 重传。真正的问题不是 TIME_WAIT 存在，而是连接创建和关闭速率太高，或者端口、内存、NAT 表、conntrack 表容量跟不上。

第三个误导是把 2MSL 当成所有系统的固定秒数。规范描述的是协议语义，具体实现会有自己的常量、优化和 sysctl 行为。Linux、BSD、Windows、负载均衡设备和 NAT 网关的等待时间可能不同。面试里可以说 2MSL 是协议含义，但事故排查必须看具体系统。

第四个误导是忽略四元组。TIME_WAIT 阻止的是同一个四元组过早复用。客户端调用同一个目标 IP 和端口时，源端口是主要稀缺资源；如果有多个目标地址、多个源 IP 或连接池复用，压力会不同。只看 TIME_WAIT 总数，不知道目标分布，很容易误判。

第五个误导是以为 `SO_REUSEADDR` 可以解决所有问题。它在不同操作系统和场景里的语义不同，对服务端监听端口重启有帮助，但不能随意让一个还在 TIME_WAIT 保护期内的同四元组新连接安全复用。客户端 ephemeral port 耗尽也不是简单加一个 socket option 就结束。

第六个误导是把它和 CLOSE_WAIT 混淆。TIME_WAIT 多通常说明主动关闭后的等待；CLOSE_WAIT 多常常说明应用收到对端 FIN 后没有 close socket，可能是应用 bug 或资源泄漏。两个状态的排查方向完全不同。

更准确的一句话是：TIME_WAIT 是 TCP 主动关闭方在连接结束后保留四元组一段时间，用来处理延迟重复报文和对端 FIN 重传的协议保护状态；它本身正常，异常的是连接 churn 或端口/NAT/conntrack 容量被打满。

## Q003. TCP TIME_WAIT 最常见的生产事故触发条件是什么？

**回答：**

最常见的触发条件是短连接风暴。应用层以为自己只是多发了一点请求，内核层看到的是每秒大量 TCP 连接建立和关闭，TIME_WAIT 迅速堆起来，最后打满 ephemeral port、NAT 端口或 conntrack 表。

第一类是客户端没有连接池。HTTP client 每次请求都新建连接，用完就关；数据库、Redis、对象存储、外部 API、LLM API 都可能出现。低 QPS 时看不出问题，流量一上来，客户端本机源端口耗尽，报 `EADDRNOTAVAIL`，业务却误以为是下游挂了。

第二类是 keep-alive 配置被关掉或被中间层破坏。应用代码设置了 `Connection: close`，代理禁用了 upstream keepalive，Ingress 到 Pod 每次都重连，Service Mesh sidecar 连接池太小，都会把请求量转成连接 churn。很多 p99 问题看起来是业务慢，实际是新建 TCP 和 TLS 握手开销暴涨。

第三类是 idle timeout 顺序错误。客户端、负载均衡器、网关、服务端都有 idle timeout。谁先超时，谁更可能主动关闭并承担 TIME_WAIT。如果服务端 idle timeout 比上游代理短，服务端可能持续主动 close；如果代理比客户端短，客户端可能不断遇到断开的连接并重建。

第四类是健康检查和探针过密。每个 Pod、每个 sidecar、每个节点、每个负载均衡器都在打 TCP 或 HTTP 探针，周期很短又不复用连接。单个探针很小，乘以实例数后就是连接风暴。Kubernetes readiness/liveness 配置不合理时，这个问题很常见。

第五类是 NAT 或 conntrack 表先满。容器、Kubernetes Node、NAT 网关、云负载均衡出口都可能维护连接状态。应用主机看起来还有端口，NAT 层已经没有可用映射。此时业务报错可能分散在多个服务上，因为共享出口被打满。

第六类是故障重试放大。下游变慢后，上游超时重试；重试没有复用连接，又造成更多 SYN、TLS 握手和 TIME_WAIT。连接数上升，排队更严重，失败率继续上升。这个闭环比单纯 QPS 上升更危险。

第七类是本地压测方式不真实。压测工具每次都短连接，源 IP 又只有一台机器。被压测服务没问题，压测机先端口耗尽。报告上看到服务端错误，根因在压测客户端连接模型。

LogServe 里如果 workflow worker 对 metadata store、object store 或 LLM endpoint 使用短连接，任务重试时就可能放大 TIME_WAIT。处理方式不是把 TIME_WAIT 当垃圾清掉，而是把连接池、超时、重试、backoff、探针频率和出口 NAT 容量一起看。

## Q004. TCP TIME_WAIT 的指标应该怎么设计才不会只看平均值？

**回答：**

TIME_WAIT 的指标设计要围绕连接生命周期和端口容量，而不是只看平均连接数。平均值很容易骗人，因为端口耗尽常常发生在某个目标、某个源 IP、某个节点或某个分钟级突刺里。

第一组是 socket state 分布。按主机、进程、容器、namespace、local address、remote address、remote port 统计 ESTABLISHED、TIME_WAIT、CLOSE_WAIT、FIN_WAIT、SYN_SENT、SYN_RECV。只看全机 TIME_WAIT 总数不够，要知道是哪个进程在对哪个目标制造短连接。

第二组是连接创建和关闭速率。每秒 active opens、passive opens、connection closes、active closes、failed connects、resets、retransmits 都要有。TIME_WAIT 数量约等于关闭速率乘以等待时间，所以速率比瞬时数更能解释趋势。

第三组是 ephemeral port 使用率。要看可用端口范围、已占用源端口数、同一目标四元组压力、`EADDRNOTAVAIL`、`EADDRINUSE`、connect timeout。端口使用率要按目标拆，因为调用一个固定下游和调用多个下游的风险完全不同。

第四组是 NAT 和 conntrack。Kubernetes 节点、Service Mesh、NAT 网关、云出口都要看 conntrack count、conntrack max、insert failed、drop、NAT allocation failure、SNAT port utilization。很多事故不是应用主机端口先满，而是共享网关先满。

第五组是连接复用率。HTTP keep-alive reuse、TLS session resumption、HTTP/2 stream reuse、gRPC channel reuse、connection pool hit rate、pool wait time、pool idle count、max lifetime close count 都要有。TIME_WAIT 只是结果，复用率才是控制旋钮。

第六组是延迟分段。新建 TCP 耗时、TLS handshake 耗时、连接池等待、业务请求耗时要拆开看 p95/p99。短连接事故时，业务 handler 可能很快，但 connect 和 handshake p99 很差。

第七组是错误分类。connect refused、connect timeout、no route、TLS handshake timeout、remote reset、本地端口不可用，要分开统计。把它们都合进 request error rate，会让排查慢很多。

第八组是突刺视图。TIME_WAIT 风险不适合只看 5 分钟平均。要看 10 秒或 30 秒窗口、max、p99、按节点 topN、按 remote endpoint topN。一个节点短时间打满源端口，就足够造成业务局部故障。

LogServe 可以把这些映射为 `outbound_connections_opened_total`、`connection_pool_reuse_ratio`、`connect_duration_seconds`、`tls_handshake_duration_seconds`、`time_wait_sockets`、`worker_external_call_errors_total`。如果部署在 Kubernetes，还要加 Node conntrack 和 NAT 出口指标。

## Q005. TCP TIME_WAIT 的正确性边界和性能边界分别是什么？

**回答：**

TIME_WAIT 的正确性边界，是保护同一四元组连接的旧报文不会污染新连接，并让主动关闭方还能处理对端 FIN 重传。它不保证应用层请求成功，不保证对端真的处理完业务，也不保证连接池配置合理。它只是 TCP 关闭语义里的一层安全垫。

它也不是“最终确认所有数据都被业务消费”。TCP 能保证字节流可靠、有序交付到对端 TCP 栈，并通过 FIN 关闭方向；应用是否读完、处理完、提交事务，要看应用协议。把 TIME_WAIT 说成“业务已经结束”是不准确的。

TIME_WAIT 不能替代应用层幂等。连接关闭、超时、RST、重试都可能让客户端不知道请求是否被服务端处理。支付、任务提交、workflow 调度这类操作仍要有 request id、幂等表、事务或日志。TCP 状态解决不了业务重复提交。

性能边界主要是资源保留。每个 TIME_WAIT 都占用一定内核状态和一个四元组复用窗口。单机内存通常不是第一瓶颈，源端口、NAT 映射、conntrack 表和连接创建 CPU 更常先出问题。高短连接 QPS 下，等待时间越长，累计 TIME_WAIT 越多。

端口边界很具体。客户端访问同一个目标 IP:port 时，可用并发新连接速率受 ephemeral port 范围和 TIME_WAIT 时间影响。比如端口范围只有几万，等待时间几十秒，每秒几千短连接就可能接近极限。多个源 IP、多个目标 IP 或连接复用能缓解，但不能改变同四元组保护的基本事实。

延迟边界也明显。短连接意味着每次都要 TCP 三次握手，TLS 场景还要握手和证书验证。连接池、HTTP/2、gRPC、长连接能把这个成本摊掉。TIME_WAIT 多通常是连接复用失败的信号，不是单独的调参对象。

调参边界要谨慎。扩大 ephemeral port range、增加 conntrack 容量、调整 keepalive、优化 NAT 出口可以做；缩短 TIME_WAIT、强行复用四元组、依赖不清楚的内核参数，要先证明协议和网络环境允许。尤其在 NAT、负载均衡、时间戳和跨主机路径复杂的环境里，激进复用会把偶发旧包问题变成低频脏数据。

对 LogServe 来说，正确的边界是：连接层要稳定承载 workflow 的外部调用，但 workflow 语义靠 shared log、checkpoint、幂等和 replay 保证。TIME_WAIT 只告诉你连接生命周期压力，不能替 LogServe 证明任务执行正确。
## Q006. 面试官如果只问一个问题检验你是否理解 epoll，可能会问什么？

**回答：**

我会预期他问一个边缘触发卡死题：

```text
一个 socket 注册了 EPOLLIN | EPOLLET。epoll_wait 返回可读，业务只 read 了一部分数据就返回事件循环。之后这个连接明明还有数据，但服务不再处理，像是卡住了。为什么？怎么修？
```

这个问题能检查你是否知道 epoll 的核心：它告诉应用“文件描述符可能可以做 I/O”，不是替应用完成 I/O。特别是 edge-triggered 模式下，事件只在状态发生变化时通知。你收到一次可读事件后，如果没有把 socket 读到 `EAGAIN` 或 `EWOULDBLOCK`，内核可能不会再因为“还有旧数据未读”重复通知你。应用就把剩余数据留在缓冲区里，连接看起来死了。

正确做法是把被 epoll 管理的 socket 设成 nonblocking。收到可读事件后循环 read，直到返回 `EAGAIN`；收到可写事件后循环 write，直到写完应用缓冲或返回 `EAGAIN`。如果写不完，再关注 EPOLLOUT；写完后不要长期监听 EPOLLOUT，否则很多 socket 一直可写，会让事件循环空转。

我会顺手区分 level-triggered 和 edge-triggered。LT 是默认模式，只要条件仍成立，epoll_wait 还会继续返回这个 fd。ET 只在状态变化时触发，性能上可以减少重复通知，但应用责任更重。ET 用错时最典型的症状就是“连接还有数据却不再被唤醒”。

然后说 epoll 的两个集合。epoll instance 里有 interest list，表示应用注册想监听哪些 fd 和哪些事件；内核维护 ready list，表示哪些 fd 已经就绪。`epoll_ctl` 负责增删改 interest list，`epoll_wait` 从 ready list 取事件。理解这两个集合，比背“epoll 是 O(1)”更有用。

面试官可能继续问多线程。多个 worker 同时处理同一个 fd，可能重复读写、乱序、并发关闭。`EPOLLONESHOT` 可以让 fd 触发一次后自动 disabled，应用处理完再 `EPOLL_CTL_MOD` rearm。多个 epoll 实例监听同一个 listen fd，还要考虑惊群，某些场景可以用 `EPOLLEXCLUSIVE` 降低无意义唤醒。

结合 LogServe，我会说：如果项目里有 HTTP/gRPC 服务、worker 长连接、流式 LLM 响应或日志 tailing，epoll 问题会体现在 event loop lag、连接饥饿、写缓冲积压和 p99 抖动上。不能只说用了 Go runtime 或 netpoll 就结束，底层仍然是 readiness notification 和非阻塞 I/O 的语义。

## Q007. epoll 的一句话定义是否容易误导，误导点在哪里？

**回答：**

容易误导。常见一句话是：epoll 是 Linux 上高性能的 I/O 多路复用机制。这句话太宽，容易把几个重要边界说没了。

第一个误导是把 epoll 当成异步 I/O。epoll 不是 IOCP，也不是说 read/write 已经在后台完成。它只是通知 fd 当前可能可读、可写、出错或挂起。真正的数据拷贝、协议解析、业务处理仍在应用线程里完成。你的回调慢，整个事件循环就慢。

第二个误导是以为 epoll 自动比 poll/select 快。epoll 避免每次把完整 fd 集合来回拷贝和扫描，适合大量连接、少量活跃的场景。但如果活跃 fd 本来就很多，或者每个事件的业务回调很重，瓶颈不在 epoll。把性能问题全归因于 I/O 多路复用模型，往往会走偏。

第三个误导是只背 O(1)。实际成本包括 `epoll_ctl` 注册修改、ready list 事件返回、用户态处理、fd 生命周期管理、锁竞争、cache miss、网络栈处理。`epoll_wait` 返回 N 个事件，应用就要处理 N 个事件。N 很大时，循环本身和回调才是成本。

第四个误导是忽略触发模式。默认 LT 更宽容，ET 更容易写出高性能但也更容易写错。ET 必须配合 nonblocking I/O，并且读写到 `EAGAIN`。只说“用 ET 更快”，却没有 drain loop，基本就是事故预告。

第五个误导是忽略 fd 生命周期。fd 被 close 后，数字可能很快复用；dup、fork、close、epoll_ctl del 的时机都可能制造难排查问题。事件里拿到的 fd 数字，不等于业务对象还活着。生产代码通常需要连接对象、generation、引用计数或明确的 ownership。

第六个误导是忽略可写事件。EPOLLOUT 很容易触发，因为大多数 TCP socket 大部分时间都可写。如果一直监听所有连接的 EPOLLOUT，事件循环会被可写事件刷屏。正确做法通常是只有应用输出缓冲区非空且上次写到 EAGAIN 时，才关注写事件。

更准确的一句话是：epoll 是 Linux 提供的 readiness notification 机制，通过 interest list 和 ready list 让应用高效等待大量 fd 的可 I/O 状态；它不替应用完成 I/O，也不替应用处理非阻塞循环、触发模式、fd 生命周期和事件公平性。

## Q008. epoll 最常见的生产事故触发条件是什么？

**回答：**

epoll 事故最常见的触发条件，是事件循环里某个边界没处理好：没有 drain 到 EAGAIN、一直监听可写、回调太慢、fd 复用、关闭顺序错、多线程 rearm 错。它们表面上都像“网络偶发卡住”。

第一类是 ET 模式没有读干净。收到 EPOLLIN 后只读一次，或者只读固定大小 buffer，剩余数据留在内核缓冲区。之后没有新边沿，就不再通知。用户看到连接没断、CPU 不高、日志也没错，但请求不继续推进。

第二类是阻塞 I/O 混进事件循环。fd 没设 nonblocking，或者某个库函数内部做了阻塞 DNS、磁盘读、同步日志、TLS 握手、压缩。一个连接卡住，整个线程上的其他连接都被拖住。epoll 只能告诉你 fd 就绪，不能防止你在回调里阻塞。

第三类是 EPOLLOUT 风暴。应用长期监听可写事件，导致 epoll_wait 每次都返回大量“可写”fd，真正需要读的连接被延后，CPU 空转上升。写事件应该是按需开启，写完或缓冲清空后关闭关注。

第四类是 accept 没有循环到 EAGAIN。listen fd 收到连接事件后只 accept 一个连接，高并发时 backlog 里还有很多连接，但 ET 模式可能不会重复通知。结果新连接排队、超时、SYN/accept backlog 异常。正确做法同样是循环 accept 到 EAGAIN。

第五类是连接对象生命周期错误。一个 fd close 后被系统复用，旧事件迟到，应用拿 fd 数字找到已经释放或错误复用的连接对象，出现串线、panic、重复关闭。这个问题低频但很致命。工程上要把 fd 和连接 generation 绑定，关闭时从 epoll 删除，事件处理前确认对象仍然有效。

第六类是多线程重复处理。同一个 fd 被多个线程唤醒，两个线程同时 read/write，状态机乱掉。用 `EPOLLONESHOT` 后忘记 rearm，也会让连接处理一次后消失。用多 epoll 实例监听同一个 listen fd，不处理惊群，就会让很多线程被无效唤醒。

第七类是事件循环公平性差。一次 epoll_wait 返回很多事件，应用按顺序处理，每个回调都可能继续产生更多工作。某些热连接一直占据循环，冷连接或新连接被饿死。表现是平均延迟还行，少数连接 p99/p999 很差。

LogServe 如果出现这类问题，可能表现为 worker 心跳延迟、流式响应卡住、日志 tail 延迟、任务调度 RPC p99 抖动。排查时要看事件循环延迟、fd 状态、连接对象生命周期和回调耗时，而不是只看 CPU 平均值。

## Q009. epoll 的指标应该怎么设计才不会只看平均值？

**回答：**

epoll 的指标要围绕事件循环健康度、fd 分布、事件类型和处理公平性设计。只看平均 QPS 或平均请求延迟，通常看不到事件循环已经在局部饥饿。

第一组是 event loop lag。记录每轮循环从预期唤醒到实际处理的延迟、单轮处理耗时、最长连续占用时间、回调 p95/p99、队列排队时间。事件循环问题最怕平均值，平均 1ms 但 max 2s，用户已经感知卡顿。

第二组是 epoll_wait 行为。每秒 epoll_wait 调用次数、返回事件数分布、timeout 次数、空唤醒次数、每次返回的 maxevents 是否经常打满。如果 maxevents 经常打满，说明 ready list 可能长期积压，应该看是否需要增加 batch、拆 event loop 或优化回调。

第三组是事件类型分布。EPOLLIN、EPOLLOUT、EPOLLERR、EPOLLHUP、EPOLLRDHUP 要分开统计。EPOLLOUT 占比异常高，常常说明可写事件关注策略错；HUP/ERR 上升，要结合连接关闭和 reset 看。

第四组是非阻塞 I/O 结果。read/write/accept 返回 `EAGAIN` 的次数、每次事件读写的字节数、每次事件 drain 的循环次数、partial write 次数、write buffer size。ET 模式下，这些指标能证明应用有没有读写到边界。

第五组是 fd 和连接对象。open fd count、epoll interest count、active connection count、closed-but-pending events、epoll_ctl add/mod/del 错误、fd reuse generation mismatch。生命周期问题必须有直接证据，不能只靠日志猜。

第六组是 accept 和 backlog。listen socket 的 accept rate、accept loop drain count、SYN backlog、accept queue、connection refused、new connection latency。新连接慢不一定是业务慢，可能是 accept 没跟上。

第七组是线程和 CPU。event loop thread CPU、run queue latency、context switch、lock contention、GC pause、scheduler delay 都要跟 event loop lag 对齐。Go runtime、Netty、Nginx、Redis 这类系统虽然抽象了 epoll，但最终仍会被调度和回调阻塞影响。

第八组是公平性。按连接、tenant、route、remote peer 统计等待时间和处理次数 topN。一个热 fd 反复就绪，可能压住其他连接。看全局平均事件处理时间，不会发现某个租户连接被饿死。

LogServe 可以把这些指标放在服务入口、worker RPC、日志流和外部调用适配层。比如 `event_loop_lag_seconds`、`fd_ready_events_total`、`socket_read_eagain_total`、`write_buffer_bytes`、`stream_stall_seconds`。即使用 Go，也可以通过 runtime/netpoll 间接指标、pprof、trace 和连接层埋点接近这些问题。

## Q010. epoll 的正确性边界和性能边界分别是什么？

**回答：**

epoll 的正确性边界，是它只承诺把 fd 的就绪状态通知给应用。它不保证一次通知对应一条完整消息，不保证 read 一次能读完，不保证 write 一次能写完，不保证回调不会阻塞，也不保证多线程处理同一个 fd 是安全的。

应用协议正确性要自己做。TCP 是字节流，epoll 告诉你可读，只说明内核缓冲区里有字节或状态变化。消息边界、length prefix、protobuf 解码、半包、粘包、TLS record、HTTP chunk，都要应用层解析。把 EPOLLIN 当成“收到一个完整请求”，肯定会出错。

ET 模式下，正确性责任更重。所有被监听 fd 应该是 nonblocking，read/write/accept 要循环到 EAGAIN。用 `EPOLLONESHOT` 时，处理完要 rearm。关闭连接时要清理 interest list，并防止迟到事件访问旧对象。epoll 给了机制，状态机要应用自己写稳。

错误事件也要处理。EPOLLERR、EPOLLHUP 通常即使没有显式关注也会返回。应用不能只处理 EPOLLIN/EPOLLOUT。连接半关闭、对端 reset、写端关闭、读端 EOF，都要映射到协议状态，否则容易出现泄漏或死连接。

性能边界首先是事件处理成本。epoll 能减少等待大量 fd 的扫描成本，但每个 ready event 仍要被用户态处理。连接数量很大但活跃连接少，epoll 很适合；如果每个连接都活跃，或者每个事件回调都很重，瓶颈会转移到业务逻辑、内存拷贝、锁、GC、协议解析。

第二个边界是 `epoll_ctl` 成本。频繁 add/mod/del 大量 fd 也有成本。错误地每次循环都修改 interest，或者大量短连接频繁注册删除，会增加内核开销。很多高性能网络库会尽量减少不必要的 mod。

第三个边界是单线程 event loop。一个 loop 能管理很多连接，但同一个 loop 上的所有回调共享执行时间。任何慢操作都会影响同 loop 上的其他连接。要么把慢操作移出 event loop，要么多 loop 分片，要么通过 backpressure 限制单连接占用。

第四个边界是可移植性。epoll 是 Linux API，BSD 有 kqueue，Windows 有 IOCP。跨平台 runtime 会抽象这些差异，但抽象不等于语义完全一样。面试里如果讨论 Go/Netty/libuv，要知道上层库替你处理了哪些边界，哪些仍然会暴露成连接卡顿或尾延迟。

对 LogServe 来说，epoll 的合理边界是支撑高并发网络 I/O 和事件通知；workflow 是否可恢复、任务是否幂等、日志是否一致，仍靠 LogServe 的调度、shared log 和 replay。不要把“网络层能高并发”说成“系统语义可靠”。
## Q011. 面试官如果只问一个问题检验你是否理解 TLS certificate，可能会问什么？

**回答：**

我会预期他问一个 HTTPS 故障题：

```text
浏览器通过 HTTPS 访问 api.example.com 报证书错误。服务端说证书没有过期，而且是权威 CA 签发的。你会检查什么？为什么“证书没过期”仍然可能不可信？
```

这个问题能检查你是否知道 TLS certificate 不是一张“加密许可证”。它要解决的是身份绑定：客户端要确认自己连到的服务身份，确实绑定到服务端在握手中证明持有的公钥对应私钥。证书只是链条的一部分，TLS 握手还要证明私钥、校验证书链、匹配服务名，并把身份绑定到本次密钥交换和握手 transcript。

我会先检查域名匹配。客户端访问的是 `api.example.com`，证书里必须有合适的 subjectAltName，比如 dNSName `api.example.com` 或合规的 wildcard。现代规则不应再依赖 Common Name。很多证书“没过期”，但 SAN 不包含当前访问域名，或者 wildcard 只覆盖一层子域名，照样失败。

第二步看证书链。服务端通常要发送 leaf certificate 和中间 CA 证书，让客户端能构造到本地 trust anchor 的有效路径。证书链少了 intermediate、链顺序错、用了过期或交叉签名不兼容的中间证书，都会导致某些客户端失败。服务器上 `openssl x509 -enddate` 看 leaf 没过期，不代表整条链可用。

第三步看用途和约束。证书要有合适的 key usage、extended key usage、basic constraints。CA 证书不能当普通服务端 leaf 随便用，客户端证书和服务端证书用途也不同。内部 mTLS 场景里，还要确认 client auth 和 server auth 的 EKU、信任根和命名规则。

第四步看私钥匹配。证书里有公钥，服务端必须拥有对应私钥，并在 TLS 握手里通过签名证明。部署时把证书和私钥配错，证书文件看起来完全正常，握手仍会失败。负载均衡器、Ingress、sidecar、应用容器各有一份证书配置时，这类错很常见。

第五步看 SNI 和路由。一个 IP 上托管多个域名，客户端通过 SNI 告诉服务端要访问哪个名称。SNI 没传、代理没转发、Ingress 规则匹配错、默认 backend 返回了默认证书，都会出现“拿到的证书不是这个域名”的错误。

第六步看时间和吊销。客户端时间错会让证书看起来未生效或已过期。OCSP、CRL、短证书、自动轮换策略也可能影响验证。很多线上事故不是证书本身错，而是续期成功但服务没有 reload，或者新 Secret 没同步到所有 Pod。

结合 LogServe，我会说：如果项目支持 TLS 或 mTLS，面试里不能只说“配置 HTTPS”。要能说明证书链、SAN、SNI、私钥、trust store、轮换、reload、握手失败分类这些边界。LogServe 的业务恢复和 shared log 语义，不会因为上了 TLS certificate 自动成立；TLS 只解决连接身份和传输保护的一部分。

## Q012. TLS certificate 的一句话定义是否容易误导，误导点在哪里？

**回答：**

容易误导。常见一句话是：TLS certificate 是用来加密通信的证书。这个说法很常见，但它把认证、密钥交换和加密混成了一团。

第一个误导是把证书当成加密本身。证书主要承载身份、公钥、签发者、有效期、用途和扩展。TLS 里的对称加密密钥来自握手中的密钥交换。证书帮助客户端认证对端，并把对端身份和握手绑定起来；真正加密应用数据的是 TLS record layer 里的会话密钥。

第二个误导是以为有 CA 签名就够了。客户端还要构造有效证书链，链要回到受信任的 trust anchor；证书要在有效期内；用途要符合；关键扩展要能处理；服务名要和客户端要访问的 reference identity 匹配。任何一项失败，都应该视为认证失败。

第三个误导是把“域名属于你”和“服务安全”混起来。公有 CA 证书通常证明你控制某个域名或满足某类验证流程，不证明你的应用没有漏洞，不证明后端授权正确，不证明数据库安全。证书能帮助客户端知道自己连到的是谁，但不替应用做业务鉴权。

第四个误导是忽略 SAN。现代服务身份匹配应看 subjectAltName 中的 dNSName、iPAddress、URI 或 SRV 这类明确类型。Common Name 是自由文本，歧义太多，不该继续作为服务身份匹配依据。老客户端兼容行为不能当新系统设计依据。

第五个误导是忽略私钥证明。别人拿到你的证书文件，只有公钥和签名，没有私钥不能冒充你完成正常的证书认证握手。反过来，如果私钥泄漏，即使证书还没过期，也已经不可信，需要吊销、轮换和排查。

第六个误导是忽略部署链路。证书可能在 CDN、负载均衡器、Ingress、Service Mesh、应用进程、内部 RPC 客户端上各有一份。用户看到 TLS 成功，不代表内部每一跳也有相同认证语义。外层 HTTPS 和内层 mTLS 是两个问题。

更准确的一句话是：TLS certificate 是 TLS/PKIX 体系里把服务身份和公钥绑定起来的凭据，客户端通过证书链、服务名匹配、用途约束和握手中的私钥证明来认证对端，然后 TLS 再建立会话密钥保护传输。

## Q013. TLS certificate 最常见的生产事故触发条件是什么？

**回答：**

最常见的触发条件仍然是证书过期或轮换失败，但真正的事故链往往比“忘记续期”复杂。续期、分发、reload、SNI 路由、trust store 更新，任何一段断了，都会表现成 TLS 握手失败。

第一类是证书过期。域名多、环境多、Ingress 多、内部服务多时，证书库存不完整。某个旧域名、灰度环境、后台 callback、移动端 API、内部 mTLS CA 被漏掉，到期当天才发现。短有效期证书能降低长期密钥风险，但也把自动化要求抬高。

第二类是续期成功但没有生效。ACME 或内部 CA 已经签出新证书，Secret 也更新了，但 Nginx、Envoy、Ingress Controller、应用进程没有 reload；某些 Pod 挂载的是旧 Secret；某个节点缓存了旧证书；CDN 边缘没有全部分发完成。监控如果只查证书仓库，不查线上握手拿到的证书，就会误报正常。

第三类是缺 intermediate 或链不兼容。服务端只部署 leaf，某些客户端能从缓存补链，某些不能。交叉签名链、老系统 trust store、企业代理、嵌入式设备、Java truststore，都可能和主流浏览器表现不同。事故经常只影响一部分客户端。

第四类是 SAN 或 SNI 错。新域名接入后证书没加 SAN；通配符覆盖不了多级子域名；Ingress 默认返回了另一个域名的证书；客户端不发 SNI；HTTP Host 和 SNI 不一致。服务端日志里可能只看到握手失败，看不到完整 HTTP 请求，因为请求还没进入应用层。

第五类是信任根变化。内部 CA 轮换、根证书下发不完整、容器镜像 trust store 太旧、Java 自带 truststore 和系统 trust store 不一致。mTLS 场景更常见：服务端换了 client CA，部分客户端还拿旧证书；客户端 trust bundle 没更新，开始拒绝服务端。

第六类是私钥和证书不匹配。部署脚本把 cert 和 key 混了，或者不同环境 Secret 互相覆盖。这个错误很直接，但在多层代理里排查很烦，因为你要确认出错的是哪一跳。

第七类是时间问题。节点时间漂移，证书还未生效或看起来已经过期。容器、虚拟机、边缘节点、测试环境尤其容易。TLS 错误表面是证书问题，根因可能是 NTP 或宿主机时间。

第八类是证书体积和算法变化。证书链过长、OCSP stapling 问题、弱算法被客户端禁用、后量子或混合证书试验导致握手体积变大，都可能先在少数客户端或高延迟网络上暴露。

LogServe 如果部署在 Kubernetes，证书事故通常不在业务代码里，而在 Secret、Ingress、cert-manager、Envoy/Nginx reload、trust bundle 和客户端配置里。面试里说“我们用 TLS”不够，要能说怎么发现证书即将过期、怎么验证线上实际证书、怎么回滚和轮换。

## Q014. TLS certificate 的指标应该怎么设计才不会只看平均值？

**回答：**

TLS certificate 的指标不能只看平均握手耗时。证书事故往往是某个域名、某条证书链、某类客户端或某个边缘节点先坏掉。指标要从库存、有效期、握手失败原因、线上实际证书和客户端差异几个角度设计。

第一组是证书库存。按域名、SAN、issuer、serial number、not_before、not_after、key algorithm、chain fingerprint、部署位置、owner 统计。所有入口域名、内部服务名、mTLS client cert、webhook cert、Ingress default cert 都要在库存里。没有库存，就没有可靠续期。

第二组是到期时间。`days_until_expiry` 要按证书和部署点上报，看最小值，不看平均值。100 张证书平均还有 60 天没意义，其中一张明天过期就会事故。告警要分 30 天、14 天、7 天、3 天、1 天，内部 CA 和公有 CA 可以分策略。

第三组是线上握手采样。监控应该像真实客户端一样连到每个域名、每个 region、每个 Ingress、每个 CDN 边缘，记录实际收到的 leaf、chain、SAN、issuer、expiry、SNI 返回值。只检查 Kubernetes Secret 或证书仓库，无法证明线上流量用的是新证书。

第四组是握手失败分类。certificate expired、unknown authority、hostname mismatch、missing intermediate、bad certificate、unsupported protocol、cipher mismatch、client cert required、private key mismatch、OCSP failure，要尽量分开。统一的 `tls_handshake_failed_total` 太粗。

第五组是客户端维度。浏览器、移动端、Java、Go、OpenSSL、curl、IoT 设备、企业代理的 trust store 和验证行为可能不同。监控要覆盖关键客户端类型。某些证书链在 Chrome 正常，不代表老 Java 正常。

第六组是轮换链路。ACME order status、challenge failure、cert-manager certificate ready、Secret resourceVersion、Ingress reload timestamp、Envoy SDS push success、Nginx reload failure、应用进程证书 reload success 都要能追。证书签发只是第一步，生效才算完成。

第七组是性能分布。TLS handshake duration、full handshake rate、session resumption rate、OCSP stapling latency、certificate chain bytes、CPU cost、handshake timeout 都要看 p95/p99。证书链大或续期后算法改变，可能让 p99 先抬高。

第八组是安全事件。私钥权限错误、密钥导出、证书吊销、异常 issuer、unexpected SAN、证书透明度日志发现未知证书，都应该有审计和告警。证书不是只看可用性，也关系到身份冒用风险。

LogServe 可以在部署层维护这些指标，而业务层至少要把外部调用的 TLS 错误分类打出来。比如调用对象存储、LLM API、metadata store 时，`x509_unknown_authority` 和 `hostname_mismatch` 要和普通网络 timeout 分开，否则排查会很慢。

## Q015. TLS certificate 的正确性边界和性能边界分别是什么？

**回答：**

TLS certificate 的正确性边界，是证明某个服务身份和某个公钥之间存在受信任的绑定，并在 TLS 握手中确认对端持有对应私钥。它不证明应用代码安全，不证明用户有权限，不证明数据库数据正确，也不证明所有后端跳转都同样受保护。

证书验证依赖本地 trust store。客户端信任哪些根 CA，是本地策略。公有互联网、企业内网、移动 App pinning、Kubernetes 内部 CA、Service Mesh trust domain 都可能不同。证书在一个客户端上有效，不代表在所有客户端上有效。

服务名匹配是硬边界。客户端访问什么 reference identity，就要和证书里的 presented identity 匹配。DNS 解析到哪个 IP、负载均衡怎么转发、HTTP Host 是什么，都不能绕过这个匹配。把证书签给 `example.com`，不能自动用于 `api.internal.example.com`。

证书链有效也不等于请求被授权。TLS server certificate 通常解决服务器认证；mTLS client certificate 可以解决客户端身份的一部分，但业务权限仍要由应用映射：哪个证书 subject/SAN 对应哪个 service account、tenant、role，证书吊销和轮换后权限怎么同步。

吊销是现实边界。证书泄漏后应吊销和轮换，但不同客户端对 CRL、OCSP、OCSP stapling、短证书的处理不一致。很多系统实际更依赖短有效期和自动轮换来降低风险。面试里不能把“吊销了就万事大吉”说得太满。

性能边界首先是握手成本。证书链传输、签名验证、CertificateVerify、密钥交换、OCSP stapling 都会增加握手延迟和 CPU。连接复用、TLS session resumption、HTTP/2、连接池能显著降低成本。短连接风暴会同时放大 TCP TIME_WAIT 和 TLS 握手压力。

第二个性能边界是证书链大小。中间证书多、算法签名大、未来后量子签名更大，都可能增加首包大小、握手分片和移动网络延迟。证书链不是越长越安全，链条越长，部署和兼容性越复杂。

第三个边界是轮换复杂度。短有效期提高安全性，但要求自动签发、分发、reload、回滚全部可靠。证书生命周期越短，监控和自动化越要成熟。人工复制证书文件的流程，迟早会出事故。

对 LogServe 来说，TLS certificate 的合理表述是：它能保护服务入口和内部 RPC 的身份认证与传输安全，但 workflow 正确性、日志恢复、任务幂等和多租户授权仍是应用层设计。TLS 证书是边界的一部分，不是系统可靠性的全部。
## Q016. 面试官如果只问一个问题检验你是否理解 Kubernetes readiness，可能会问什么？

**回答：**

我会预期他问一个滚动发布事故题：

```text
一个 Pod 容器已经 Running，端口也监听了，但刚启动时要加载模型、连接数据库、replay metadata。Deployment 滚动发布后，Service 把流量打到新 Pod，新 Pod 返回大量 503。你怎么用 readiness probe 解决？readiness 和 liveness 有什么区别？
```

这个问题能检查你是否知道 readiness 的核心不是“进程活着”，而是“这个 Pod 当前能不能接服务流量”。Kubernetes readiness probe 失败时，不应该杀容器；它会让 Pod 从 Service 的可用后端里摘掉。应用可以继续启动、加载缓存、等待依赖、恢复状态，等 readiness 成功后再接流量。

liveness 是另一件事。liveness probe 失败表示容器可能卡死或无法自愈，kubelet 会重启容器。把 readiness 和 liveness 混用很危险。比如启动慢时 liveness 过早失败，容器会被反复杀掉，永远起不来；依赖数据库短暂抖动时 liveness 失败，所有 Pod 被重启，事故会扩大。readiness 更适合表达“暂时别给我流量”。

我会把 readiness 的判断写得具体。一个 Web API 的 readiness 可能要检查 HTTP server 已经监听、路由已经加载、关键配置可用、数据库连接池能创建、缓存或索引达到可服务下限、后台 replay 至少完成到安全 offset。对于 LogServe，worker readiness 不能只看进程 alive；它还要看是否完成 shared log replay、是否拿到必要配置、是否能访问 metadata store、是否能接受任务并正确续租。

但 readiness 也不能太重。每几秒跑一次完整数据库查询、对象存储读写、LLM 调用，会把探针变成生产流量，依赖一抖就让全体 Pod 同时 NotReady。比较稳的做法是把深检查放到后台更新本地 ready 状态，readiness endpoint 快速返回当前状态和原因。探针本身要轻、快、可解释。

还要说传播延迟。Pod readiness 变成 False 后，EndpointSlice、kube-proxy、Ingress、负载均衡器更新都需要时间。已经建立的连接也不一定立刻断开。优雅下线不能只依赖 readiness 失败，还要配合 preStop、terminationGracePeriod、应用停止接新请求、等待 in-flight 请求完成。

面试里可以这样答：readiness 是 Kubernetes 的流量准入信号。它告诉 Service 这个 Pod 是否应该收到新流量；它不负责重启容器，也不保证外部负载均衡立即停止转发，更不替应用完成依赖恢复。好的 readiness 要轻量、稳定、表达可服务边界，并和滚动发布、优雅下线、启动探针一起设计。

## Q017. Kubernetes readiness 的一句话定义是否容易误导，误导点在哪里？

**回答：**

容易误导。常见一句话是：readiness probe 用来判断 Pod 是否准备好接流量。这句话方向对，但很容易让人忽略实现边界。

第一个误导是把 Running 当 Ready。容器 Running 只说明进程启动了，端口监听也只说明 socket 打开了。应用可能还在加载配置、迁移 schema、恢复 checkpoint、预热缓存、等待 sidecar、同步证书或建立连接池。Running 到 Ready 之间可能差很多。

第二个误导是把 readiness 当 liveness。readiness 失败是摘流量，liveness 失败是重启。依赖短暂不可用时，readiness 可以让 Pod 暂停接新请求；liveness 如果也跟着失败，就会触发重启风暴。面试里这两个概念必须分开。

第三个误导是以为 readiness 只在启动时跑。readiness probe 会在容器生命周期内持续运行。服务启动后也可能变 NotReady，比如连接池耗尽、配置 reload 失败、worker backlog 过深、磁盘只读、关键下游不可用。它是动态流量信号，不是一次性启动检查。

第四个误导是以为 NotReady 后立刻零流量。Kubernetes 会更新 Pod condition 和 endpoint，但 Service、EndpointSlice、kube-proxy、Ingress Controller、云负载均衡器、客户端连接池都可能有传播和缓存延迟。已有长连接、HTTP/2 stream、gRPC stream 也不会因为 readiness 失败自动消失。

第五个误导是把 readiness 写成全依赖健康检查。一个服务依赖很多下游，readiness 如果检查所有下游，每个下游小抖动都会把服务摘掉，甚至造成级联。readiness 应该表达“我现在能不能正确处理我承诺接收的请求”，而不是替整个依赖图做全局健康判断。

第六个误导是忽略多容器和 readiness gates。一个 Pod 里多个容器都要考虑 ready 状态；还可以通过 readinessGates 引入自定义 Pod condition。sidecar 没准备好、代理还没拿到配置、证书还没下发时，应用容器 ready 也不等于整个 Pod ready。

更准确的一句话是：Kubernetes readiness 是 Pod 是否应被纳入 Service 后端的动态信号，通常由 kubelet 持续执行容器探针并结合 Pod 条件决定；它负责流量准入，不负责重启，也不保证外部流量瞬间停止。

## Q018. Kubernetes readiness 最常见的生产事故触发条件是什么？

**回答：**

readiness 最常见的事故，是探针表达的状态和真实可服务状态不一致。要么太宽，Pod 还没准备好就接流量；要么太严，应用本来能服务却被摘掉；要么太重，探针本身制造故障。

第一类是启动过早 Ready。容器启动后 `/healthz` 直接返回 200，但应用还在加载模型、初始化连接池、replay 日志、预热缓存、注册路由。Deployment 认为新 Pod 可用，开始缩老 Pod，流量打到新 Pod，出现 503 或高 p99。正确做法是 readiness endpoint 明确等待可服务条件。

第二类是把外部依赖写得过死。readiness 每次都查数据库、Redis、对象存储、外部 API，只要任何一个慢，就把 Pod 摘掉。多个 Pod 同时摘掉后，剩余 Pod 压力更大，依赖更慢，形成雪崩。依赖检查要区分关键依赖、可降级依赖和后台依赖。

第三类是探针超时太短。应用在 CPU throttling、GC、磁盘抖动或节点压力下偶尔慢一点，readiness 就失败。Pod 在 Ready/NotReady 之间抖动，Endpoint 不断变化，负载均衡和连接池跟着抖。`timeoutSeconds`、`periodSeconds`、`failureThreshold`、`successThreshold` 要根据真实 p99 设置，不能随便照抄模板。

第四类是 liveness 和 readiness 用同一个重接口。接口慢时 readiness 摘流量已经够了，liveness 又把容器杀掉。重启后冷启动更慢，探针继续失败，变成 CrashLoopBackOff。启动慢的服务还应该用 startupProbe 给初始化留时间。

第五类是优雅下线只改 readiness。Pod 收到终止信号后 readiness 变 False，但 preStop 太短、terminationGracePeriod 太短、应用没有停止 accept 新请求、Ingress 传播慢，结果仍有请求打进来并被中断。滚动发布时就会出现少量 502/503，平均成功率看起来还行，核心用户路径已经受影响。

第六类是 sidecar 或 Service Mesh 时序问题。应用容器 ready 了，Envoy sidecar 还没拿到配置；或者应用准备退出，sidecar 仍然接流量；mTLS 证书还没下发，readiness 只看应用端口。多容器 Pod 的 readiness 要把代理、证书和网络路径纳入设计。

第七类是探针路径穿过了业务中间件。readiness endpoint 需要鉴权、访问数据库、打日志、走限流、走外部 tracing，事故时它和普通请求一起排队，探针失败加剧摘流量。健康接口应该尽量短路径，避免被普通业务限流或鉴权误伤。

LogServe 里常见风险是 worker 进程启动后立刻 Ready，但实际上 shared log replay、checkpoint 加载、metadata store 连接、任务租约恢复还没完成。另一个风险是 readiness 检查 LLM API，外部 LLM 抖动时把所有 worker 摘掉。更稳的做法是把“能接任务”和“某个外部模型当前可用”分层表达。

## Q019. Kubernetes readiness 的指标应该怎么设计才不会只看平均值？

**回答：**

readiness 的指标要证明两件事：Pod 状态是否稳定，流量是否真的避开了 NotReady Pod。只看 Deployment available replicas 或探针平均成功率不够。

第一组是 Pod ready 状态。按 namespace、workload、pod、container、node 统计 Ready/NotReady 数量、ready transition count、NotReady 持续时间、Ready 抖动次数、ready age。要看最小可用副本数和最久 NotReady，不要只看平均 Ready 比例。

第二组是探针结果。readiness probe success/failure、timeout、HTTP status、exec exit code、TCP connect failure、gRPC probe failure、probe duration p95/p99、连续失败次数都要有。探针失败原因要能区分应用主动返回 NotReady 和 kubelet 探测超时。

第三组是启动到 ready 的分布。container start 到 startupProbe 成功、readiness 首次成功、Pod 进入 EndpointSlice 的耗时都要记录。冷启动、缓存预热、日志 replay、证书下发这些都应该在这个分布里体现。滚动发布时 p99 startup-to-ready 比平均值重要。

第四组是 endpoint 传播。Pod Ready condition 变化时间、EndpointSlice 更新、kube-proxy 生效、Ingress/LoadBalancer 后端更新、Service Mesh xDS 推送时间，能观测到多少就观测多少。NotReady 到真实无新流量之间的延迟，是优雅下线设计的关键。

第五组是流量命中情况。按 Pod ready 状态统计请求数、5xx、连接重置、in-flight 请求、长连接数。理想情况下 NotReady Pod 不应接收新流量；如果仍有请求，说明有直连 Pod IP、客户端缓存、Ingress 传播延迟或连接复用问题。

第六组是滚动发布指标。每次 rollout 的 unavailable replicas、max surge、max unavailable、new pod ready latency、old pod termination latency、发布期间 p99、发布期间 5xx、回滚次数，都要单独看。readiness 很多事故只在发布时出现。

第七组是探针成本。readiness endpoint QPS、CPU、数据库查询数、外部依赖调用数、日志量、trace 量要能看到。健康检查本来应该轻量，如果它占了明显资源，就会在高负载时反过来伤害服务。

第八组是原因暴露。readiness endpoint 最好返回结构化 reason，比如 `replaying_log`、`warming_cache`、`dependency_unavailable`、`backlog_too_high`、`draining`。指标里也应有对应状态。只有 200/503，排查时还要翻日志。

LogServe 可以设计 `pod_ready`、`readiness_transition_total`、`startup_to_ready_seconds`、`shared_log_replay_ready_offset`、`worker_accepting_tasks`、`draining_inflight_tasks`、`readiness_probe_duration_seconds`。这些指标要按 worker、scheduler、API server 角色拆开，不要把所有 Pod 混成一个平均值。

## Q020. Kubernetes readiness 的正确性边界和性能边界分别是什么？

**回答：**

Kubernetes readiness 的正确性边界，是控制 Pod 是否进入 Service 后端，从而影响新流量分发。它不证明业务逻辑正确，不证明所有依赖健康，不证明请求一定成功，也不保证外部负载均衡和已有连接立即停止。

它对 Service 流量有效，但绕过 Service 的路径可能不受它约束。客户端直连 Pod IP、某些自定义 sidecar、外部负载均衡缓存、长连接、消息队列消费者、定时任务、leader election，都可能继续工作。readiness 只能管它所在路径里的流量准入。

它也不等于容量管理。一个 Pod Ready，只说明它当前认为可以接流量，不说明它有足够容量处理无限请求。负载、HPA、队列长度、限流、backpressure、PodDisruptionBudget、资源 request/limit 仍要单独设计。把 overload 时的降级全交给 readiness，可能导致所有 Pod 轮流 NotReady。

readiness 不能替代业务级健康。比如 LogServe 的 API 进程能返回 200，但 workflow replay 卡住、shared log fsync p99 很高、worker 租约续期失败，用户仍会看到任务延迟。readiness 可以把这些信号的一部分纳入准入，但系统正确性仍要靠日志、幂等、重投递和恢复机制。

性能边界首先是探针频率和成本。每个 kubelet 周期性执行探针，实例数一多，探针流量就不小。如果探针访问数据库或外部服务，会给依赖制造固定 QPS；如果 timeout 太短，会在节点压力下误判；如果 period 太长，故障摘流量又太慢。

第二个性能边界是发布速度。readiness 首次成功越慢，滚动发布越慢；failureThreshold 和 successThreshold 越保守，摘流量和恢复流量越慢。慢一点通常更安全，但会影响部署时长和容量余量。这个参数要和 maxSurge、maxUnavailable、启动耗时分布一起看。

第三个边界是下线延迟。readiness False 到真实没有新流量之间有传播时间，已有连接还要 drain。优雅下线要配合应用层 draining、preStop、terminationGracePeriod、连接池和 Ingress/Service Mesh 配置。只改 readiness，不能保证零中断。

第四个边界是状态抖动。readiness 过于敏感会导致 Endpoint 频繁变化，负载均衡不断重算，客户端连接反复失败。系统高负载时，如果 readiness 抖动导致流量集中到剩余 Pod，会放大尾延迟。探针要有阈值和滞后，ready 状态要稳定。

面试里可以这样总结：readiness 是 Kubernetes 的流量开关，不是应用正确性的证明。它应该表达“这个实例现在能不能安全接新请求”，并且要和启动、发布、下线、依赖降级、容量和观测一起设计。对 LogServe 这种机制验证系统，readiness 最好绑定到 replay 完成、关键依赖可用、worker 可接任务和 draining 状态，而不是只返回进程活着。