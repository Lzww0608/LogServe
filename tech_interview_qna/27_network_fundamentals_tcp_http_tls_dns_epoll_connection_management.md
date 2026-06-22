# 27. 网络基础：TCP、HTTP、TLS、DNS、epoll 与连接管理

这一章先从 TCP 的连接生命周期讲起。面试里 TCP 很容易被背成固定答案：三次握手是为了建立连接，四次挥手是为了断开连接，TIME_WAIT 多了就调内核参数，`TCP_NODELAY` 开了就更快。这样的回答太薄。真正要说清楚的是：TCP 只提供可靠有序字节流，不理解 HTTP 请求、不理解业务心跳，也不知道一个服务实例是不是健康；连接管理里的很多线上问题，恰恰来自把这些层次混在一起。

下面的回答主要参考 RFC 9293 的 TCP 规范、RFC 1337 对 TIME_WAIT 风险的讨论、RFC 1122 对 TCP keepalive 的要求、RFC 896 对 small-packet problem 和 Nagle 算法的说明、Linux `tcp(7)` 手册、Go `net.TCPConn` 文档，以及 gRPC keepalive 官方文档。结合 LogServe 时，我会把边界说清楚：如果系统用 gRPC/HTTP/2、worker 长连接、logd 本地或远端连接池，连接复用、deadline、heartbeat、keepalive 和关闭顺序都要按不同层次设计，不能只靠一个 TCP 参数兜底。

## Q001. TCP 三次握手解决什么问题？

**回答：**

TCP 三次握手解决的不是“互相打个招呼”这么简单。它至少做了四件事：确认双方收发路径可用，交换并确认初始序列号，避免旧的重复 SYN 把连接误打开，顺便协商 TCP 选项。把这四件事拆开讲，面试官通常会觉得你不是在背图。

最标准的过程是：

```text
client -> server: SYN, seq = x
server -> client: SYN + ACK, seq = y, ack = x + 1
client -> server: ACK, ack = y + 1
```

第一步，客户端发 SYN，告诉服务端：“我想建立连接，我的初始序列号是 x。”这里的 SYN 会占用一个序列号，所以服务端后续确认的是 `x + 1`。RFC 9293 里 TCP 序列号的定义也是这么处理的：SYN 被设置时，segment 的序列号就是 initial sequence number，后续第一个数据字节从 ISN+1 开始。

第二步，服务端回 SYN+ACK。这个包同时做两件事：确认收到了客户端的 SYN，也告诉客户端服务端自己的初始序列号 y。TCP 是全双工字节流，两个方向各有一套序列号空间。客户端到服务端的数据从 x 之后开始编号，服务端到客户端的数据从 y 之后开始编号。不能只确认一个方向。

第三步，客户端回 ACK。这个 ACK 的关键意义是：客户端确认自己收到了服务端的 SYN，也就是确认服务端到客户端这条路径至少可用过一次。服务端收到这个 ACK 后，才可以更放心地把连接推进到 ESTABLISHED。

为什么不能两次握手？最直观的问题是服务端无法确认客户端真的收到了自己的 SYN+ACK。假设只有 SYN 和 SYN+ACK 两个包，服务端发出 SYN+ACK 后就认为连接建立了；如果这个 SYN+ACK 丢了，客户端并不知道连接已经建立，服务端却已经分配了连接状态。再极端一点，网络里有一个旧的重复 SYN 延迟了很久才到达服务端，服务端回 SYN+ACK 后如果直接建立连接，就会得到一个“没人真正要用”的半开连接。三次握手用最后一个 ACK 把这个风险压下去。

三次握手还和旧报文段有关。TCP 连接由四元组标识：

```text
source IP, source port, destination IP, destination port
```

同一个四元组如果被很快复用，旧连接里的报文可能还在网络里游荡。序列号、窗口检查、TIME_WAIT 和三次握手一起工作，降低旧报文被新连接接收的概率。RFC 1337 讨论 TIME_WAIT 风险时也提到，三次握手可以拒绝旧的重复初始 SYN，避免把旧连接重放成新连接。

握手阶段还会协商很多后续传输要用的能力。常见选项包括 MSS、window scale、SACK permitted、timestamp。它们一般放在 SYN 或 SYN+ACK 里。比如 MSS 决定一端愿意接收的最大 TCP segment payload；window scale 决定大带宽延迟积链路上窗口能不能扩大；SACK 影响丢包后的恢复效率；timestamp 会参与 RTT 估计，也可以用于 PAWS 这类防旧包机制。握手不是只建立“有连接”这个事实，它也在定后续传输的参数。

还要注意，三次握手解决的是 TCP 层问题，不解决应用层问题。握手成功只说明两端内核之间的 TCP 连接进入可通信状态，不代表 HTTP 服务已经完成初始化，不代表 TLS 已经认证通过，不代表对端业务线程没有卡死，也不代表后续写一定不会失败。面试里把这句话说出来很重要，因为线上经常有人看到 `connect()` 成功，就误以为服务可用。

拿一个 Go 服务举例：

```go
conn, err := net.DialTimeout("tcp", "logd.internal:9000", 200*time.Millisecond)
```

这个调用成功，最多说明 TCP 连接建立了。后面还要看 TLS 握手、协议握手、认证、版本协商、第一条请求是否按 deadline 得到响应。LogServe 这类 runtime 如果 worker 和 logd 之间要走长连接，不能把 TCP connect 成功当作 logd 可写；最好在应用层有一个轻量的 hello 或 health/readiness 语义，确认对端协议版本、epoch 或当前 log offset。

三次握手也不是没有成本。它至少需要一个 RTT 才能让客户端发送常规应用数据。TCP Fast Open 这类机制尝试让数据更早到达服务端，但会引入重放风险和部署条件，不能把它当普通 TCP 的默认行为。对于大多数面试回答，说清楚普通三次握手的语义，比讨论加速机制更重要。

最后可以这样收束：

```text
三次握手的核心是双向确认初始序列号和收发能力，避免旧 SYN 或丢失 SYN+ACK 造成假连接，并协商后续传输选项。它建立的是 TCP 字节流，不是应用层健康状态。
```

## Q002. TCP 四次挥手为什么需要 TIME_WAIT？

**回答：**

先把“四次挥手”和 `TIME_WAIT` 分开看。四次挥手来自 TCP 的全双工关闭语义：一端不想再发送数据，不代表它不能继续接收数据。`FIN` 关闭的是一个方向的数据流。两端各自发送一个 FIN，各自确认对方的 FIN，所以常见图里会看到四个报文：

```text
A -> B: FIN
B -> A: ACK
B -> A: FIN
A -> B: ACK
```

这个过程也可能被压成三个包，比如 B 在 ACK A 的 FIN 时顺便也发自己的 FIN。但语义还是两边分别关闭发送方向。

`TIME_WAIT` 出现在主动关闭的一方。假设 A 先发 FIN，B 后发 FIN，A 最后给 B 的 FIN 回 ACK。A 发送这个最后 ACK 后，不会马上删除连接状态，而是进入 `TIME_WAIT`。在很多资料里这个等待时间写成 2MSL，也就是两倍 Maximum Segment Lifetime。不同系统实现的具体时间可以不同，但基本思想没变：让旧报文段有时间从网络中消失，同时保留足够状态处理对端重传的 FIN。

第一个原因是保证最后一个 ACK 有机会补发。最后 ACK 是不可靠的普通 TCP segment，它可能丢。如果 A 发完最后 ACK 立刻 CLOSED，而 B 没收到 ACK，B 会停在 LAST-ACK，然后重传 FIN。A 如果没有任何状态，可能回 RST，B 看到的就是一次不干净的关闭。A 保留 `TIME_WAIT` 状态后，收到 B 重传的 FIN，可以再回一次 ACK。这是很朴素的原因，也是线上最容易理解的原因。

第二个原因是防旧报文污染新连接。TCP 连接靠四元组区分，如果 A 和 B 很快复用同一个四元组，新连接可能撞上旧连接延迟到达的数据、ACK 或 FIN。`TIME_WAIT` 的等待窗口让旧报文段自然过期，降低它们进入新连接的概率。RFC 1337 讨论过 TIME-WAIT assassination hazards，其中一个核心点就是旧重复报文可能让新连接接收错误数据、状态失步或者连接失败。`TIME_WAIT` 看起来烦，其实是在用一点时间换协议安全边界。

第三个原因是序列号空间需要时间隔离。TCP 序列号是 32 位，会随着发送数据推进。连接非常快、非常长，或者四元组复用非常快时，新旧连接的序列号范围可能重叠。窗口检查能挡掉很多旧包，但不是万能。`TIME_WAIT` 和初始序列号选择一起减少这种重叠风险。

面试里还可以补一句：`TIME_WAIT` 在谁身上出现，取决于谁主动关闭。HTTP 短连接里，如果客户端每次请求后主动 close，客户端会积累 `TIME_WAIT`；如果服务端通过 idle timeout 主动断开大量空闲连接，服务端会积累 `TIME_WAIT`。所以看到服务端 `TIME_WAIT` 多，不能直接说“客户端有问题”，要先看关闭方向。

为什么不能把 `TIME_WAIT` 调得很短？因为你是在降低对旧包和最后 ACK 丢失的容忍度。很多环境里确实会调内核参数、打开端口复用或扩大 ephemeral port 范围，但这些是容量和风险之间的选择，不是免费的优化。尤其在 NAT、负载均衡、四层代理、大量短连接并存的场景里，过早复用四元组可能把偶发协议问题放大成难排查的业务错误。

可以用一个例子说明最后 ACK 丢失的问题：

```text
1. A 主动关闭，发 FIN。
2. B ACK 后处理完剩余数据，也发 FIN。
3. A 回 ACK，但这个 ACK 在网络里丢了。
4. B 没等到 ACK，重传 FIN。
5. A 如果还在 TIME_WAIT，就再回 ACK；如果已经彻底忘了连接，B 可能收到 RST。
```

从应用视角看，第 5 步决定了关闭是否像一个正常的 TCP 关闭，还是像一次连接异常。`TIME_WAIT` 的存在就是为了让主动关闭方还能处理这种尾巴。

结合 LogServe，如果 worker、gateway、metadata service、logd 之间用了大量短 TCP 连接，`TIME_WAIT` 会很快变成噪声。更稳的做法是复用连接：gRPC channel、HTTP/2 multiplexing、连接池、合理的 idle timeout。连接关闭时，也要区分优雅关闭和强制关闭。比如 worker 停止前可以先停止接收新任务，等待 in-flight 请求完成，再关闭连接；不要在还有未确认写的时候直接 `SetLinger(0)` 或粗暴 reset。

一句话回答：

```text
TIME_WAIT 主要保护两个边界：最后 ACK 丢了还能重传 ACK；同一四元组被复用前，旧连接的报文有时间消失。它是 TCP 为可靠关闭和旧包隔离付出的时间成本。
```

## Q003. TIME_WAIT 过多通常说明什么？

**回答：**

`TIME_WAIT` 多，本身不一定是故障。它首先说明这台机器上有大量连接被主动关闭，并且这些连接还处在 TCP 的正常等待窗口里。真正要问的是：为什么有这么多连接被创建和关闭？谁在主动关？连接是否应该复用？有没有端口、NAT 或内存压力？

先不要把 `TIME_WAIT` 和 `CLOSE_WAIT` 混在一起。`TIME_WAIT` 通常是主动关闭方的正常状态；`CLOSE_WAIT` 往往说明对端已经发 FIN，本端应用还没有 close socket，可能是应用代码泄漏连接或读循环退出后忘了关闭。很多线上排查一开始就走错方向，是因为只看到了“连接状态很多”，没有区分状态含义。

常见原因有几类。

第一类是短连接太多。HTTP/1.0、禁用 keep-alive、客户端连接池没生效、每次 RPC 都新建 TCP、健康检查频率太高、脚本或爬虫不断建立连接，都会制造大量 `TIME_WAIT`。如果服务之间本来应该使用长连接，却看到每秒创建几千个短连接，那不是内核问题，是连接复用策略有问题。

第二类是本机主动关闭太多。比如服务端设置了很短的 idle timeout，主动踢掉空闲连接；客户端请求完成后马上 close；反向代理和后端的 keepalive 时间不匹配，代理不断重连；部署滚动重启时旧进程主动关掉大量连接。谁先发 FIN，谁更可能进入 `TIME_WAIT`。所以要按本地端口和远端端口分组看，而不是只看总数。

第三类是重试和错误放大。下游变慢后，客户端超时重试，每次重试都新建连接；连接还没充分复用，就被 deadline 或连接错误打断；负载均衡把请求打到不稳定实例，造成连接反复失败。这个时候 `TIME_WAIT` 多只是表面，根因可能是下游 p99 变高、DNS 轮询不稳、连接池被频繁清空或熔断策略太激进。

第四类是 NAT、代理或四层负载均衡参与了连接生命周期。客户端机器本身 `TIME_WAIT` 不多，但 NAT 网关或 sidecar 上很多；应用以为自己只连了一个服务，实际经过代理后产生了更多 upstream 连接。Kubernetes、service mesh、四层 LB 场景下，这一点很常见。

排查时我会先看这几组数据：

```text
ss -tan state time-wait | wc -l
ss -tan state time-wait | awk '{print $4, $5}' | sort | uniq -c | sort -nr | head
ss -s
cat /proc/sys/net/ipv4/ip_local_port_range
cat /proc/sys/net/ipv4/tcp_fin_timeout
```

还要看应用层指标：每秒新建连接数、连接池命中率、HTTP/gRPC channel 数、每个 upstream 的 active/idle 连接、请求超时率、重试次数、服务端主动关闭次数。只看 `TIME_WAIT` 数量没有意义。一个高 QPS gateway 上有几万 `TIME_WAIT` 可能很正常；一个内部 worker 平时只有几十个连接，突然涨到几万，就该查连接复用和重试。

`TIME_WAIT` 过多的直接症状通常是 ephemeral port 压力。客户端向同一个远端四元组空间不断建短连接，源端口被占住，可能出现 `connect: cannot assign requested address`、连接建立变慢、NAT 表打满、四层代理连接跟踪压力上升。服务端也可能有内存和统计噪声，但现代内核处理 `TIME_WAIT` 的成本已经比早期低很多，不该一看到数量大就慌。

解决思路按优先级来：

```text
优先复用连接：HTTP keep-alive、HTTP/2、gRPC channel、数据库连接池。
对齐 idle timeout：客户端、代理、服务端不要互相抢着关。
控制重试：deadline、backoff、jitter、熔断，避免超时后新建连接风暴。
扩大容量：调大 ephemeral port range，增加源 IP，扩容 NAT/LB。
最后才看内核参数：复用 TIME_WAIT、缩短等待等做法要理解风险。
```

有些参数看起来很诱人，比如 `tcp_tw_reuse`、更短的 TIME_WAIT、强行 reset 关闭。它们可能缓解端口压力，也可能让旧包风险、跨 NAT 行为和排障复杂度上升。面试里比较稳的态度是：先改连接生命周期和连接池，再考虑内核调参。内核参数应该服务于已经想清楚的流量模型，而不是替应用层设计背锅。

结合 LogServe，如果 worker 执行器、gateway、metadata API、logd 之间未来走远程 TCP/gRPC，我会重点看三个指标：每秒新建连接数、channel reuse 比例、主动关闭方向。LogServe 的控制面请求应该尽量复用连接，尤其是 log append、lease/epoch 检查、任务拉取这类高频路径。否则系统还没遇到真正的业务瓶颈，就会先被连接抖动和 TIME_WAIT 噪声拖住。

所以这个问题可以这样答：

```text
TIME_WAIT 多通常说明本机正在主动关闭大量短连接。它可能是正常高流量，也可能暴露连接未复用、idle timeout 不匹配、重试风暴或 NAT/代理压力。排查时先看关闭方向、四元组分布、连接新建速率和端口耗尽，再决定是改应用连接池还是调系统参数。
```

## Q004. TCP keepalive 和应用层 heartbeat 有什么区别？

**回答：**

TCP keepalive 和应用层 heartbeat 最大的区别是层次不同。TCP keepalive 是内核在连接空闲很久后发探测包，问的是“这个 TCP peer 还会不会对这个连接作出 ACK”；应用层 heartbeat 是协议或业务自己发消息，问的可以是“这个服务实例是否还活着、是否还能处理当前会话、当前 epoch 是否还有效、对端处理进度到哪里了”。这两个问题差很多。

RFC 9293 说 TCP keepalive 是可选机制，应用必须能按连接打开或关闭，而且默认应关闭。RFC 1122 也提醒，纯 ACK 报文在 TCP 里并不是可靠传输的，不能因为某一次 keepalive probe 没响应就立刻认定连接死了。Linux 默认参数更能说明它的定位：`tcp_keepalive_time` 默认 7200 秒，也就是连接空闲 2 小时后才开始探测；之后默认 75 秒间隔、9 次探测。这个默认值显然不是给业务级秒级故障检测用的。

TCP keepalive 的优点是应用不用定义额外协议，内核可以在连接完全空闲时做底层探活。它适合处理几类问题：客户端崩溃但没有发 FIN，连接长期占着服务端资源；中间 NAT 或防火墙会清理长时间无流量连接，需要偶尔有包经过；某些长连接确实很久没有业务数据，但希望尽早发现底层连接不可达。Go 里 `TCPConn.SetKeepAlive` 和 `SetKeepAliveConfig` 就是把这类能力暴露给应用，底层是否支持、参数如何生效还要看操作系统。

它的局限也明显。TCP keepalive 不知道应用是否健康。对端进程可能 event loop 卡住、worker pool 打满、业务状态损坏，但内核仍然能回 ACK；这时 TCP keepalive 会认为连接还活着。反过来，网络短暂抖动也可能让 keepalive 失败，但业务重试后其实能恢复。它只能告诉你传输层连接状态的大概情况，不能替代 readiness、health check 或业务心跳。

应用层 heartbeat 的能力更强，因为它可以携带业务语义。比如：

```text
ping: worker_id, current_epoch, last_applied_log_offset
pong: server_epoch, min_required_version, overload_hint, lease_expire_at
```

这样的 heartbeat 不只是探活。它还能检查协议版本，发现旧 worker，推进 session 超时，刷新租约，报告背压，让服务端知道客户端还在消费。对于 LogServe 这类系统，worker 和控制面之间的 heartbeat 如果只停留在 TCP ACK 层，就太弱了；它应该能表达 worker 当前 attempt、actor epoch、最后处理到的 log offset，必要时让控制面撤销旧 worker 的执行资格。

gRPC keepalive 是一个典型的应用/协议层例子。它用 HTTP/2 PING frame 来保持连接和检测断链。gRPC 官方文档还特别区分 keepalive 和 health checking：keepalive 关心连接，health checking 关心服务是否健康。文档也提醒不要把 keepalive 配得太激进，否则可能被服务端用 GOAWAY 拒绝。这个提醒很现实：heartbeat 本身也是流量，过多会变成干扰。

可以用表格区分：

| 维度 | TCP keepalive | 应用层 heartbeat |
|---|---|---|
| 所在层 | TCP 内核层 | 协议层或业务层 |
| 默认时间尺度 | 通常很长，Linux 默认 2 小时开始 | 应用自定，常见为秒级或几十秒 |
| 能检查什么 | 连接对端是否响应 TCP probe | 服务、会话、租约、版本、进度、负载 |
| 是否有业务 payload | 基本没有 | 可以有 |
| 能否发现应用卡死 | 不可靠，内核可能仍 ACK | 可以，只要应用线程必须参与响应 |
| 是否能穿过代理表达端到端语义 | 不一定，尤其 TCP 代理会终止连接 | 取决于协议设计，通常更接近端到端 |

还有一个容易忽略的点：应用层 heartbeat 要和 deadline、重试、连接池一起设计。比如 heartbeat 每 10 秒发一次，3 次失败后认为断开；但一次业务 RPC 的 deadline 是 2 秒。那业务失败不应该等 heartbeat 判死，它应该按自己的 deadline 返回。Heartbeat 负责维护长连接和会话状态，不负责替每个请求决定成败。

TCP keepalive 也不能替代写超时。连接长时间没有业务数据时，keepalive 可能还没触发；你一写数据才发现对端不可达。Linux 的 `TCP_USER_TIMEOUT` 可以控制已发送数据未确认多久后关闭连接，gRPC 文档也提到一些实现会结合 TCP_USER_TIMEOUT 和 keepalive。这个语义和“空闲探测”又是两回事。

面试里可以这样回答：

```text
TCP keepalive 是内核层的空闲连接探测，默认很慢，只能说明 TCP peer 大致还可达；应用层 heartbeat 是协议/业务消息，可以检查服务健康、会话状态、租约和进度。前者适合清理死连接和穿透 idle timeout，后者适合做业务级存活判断。两者可以同时用，但不能互相替代。
```

## Q005. Nagle 算法和 TCP_NODELAY 的 trade-off 是什么？

**回答：**

Nagle 算法解决的是 small-packet problem。应用如果频繁写很小的数据，比如每次写 1 个字节或十几个字节，TCP/IP 头部、ACK、路由处理和拥塞控制成本会被放大。RFC 896 当年讨论的就是这个问题：大量小包会造成低效率，甚至加剧拥塞。Nagle 的基本想法很简单：如果连接上已经有未确认的小数据，就先攒一攒，等 ACK 回来或攒到足够大再发。

可以把它简化成这个规则：

```text
如果没有未确认数据，先把小数据发出去。
如果已经有未确认数据，新的小写入先缓冲。
等收到 ACK，或者缓冲数据达到 MSS，再发送。
```

这样做的好处是减少 tinygram。吞吐型场景、批量传输、小消息可以稍微等一等的协议，通常能从 Nagle 里得到更好的网络利用率。它让发送端不要把每次 `write(2)` 都变成一个独立 TCP segment，尤其是在慢链路或高 RTT 链路上，效果会更明显。

代价是延迟。应用写了一个小请求，本来希望马上发出去，但连接上还有未确认数据，Nagle 会把它压住。更麻烦的是 Nagle 和 delayed ACK 可能互相等待：发送端等 ACK 再发小包，接收端等一会儿看能不能把 ACK 和响应合并。结果就是一个本来很小的交互，多等几十毫秒甚至更久。对 RPC、游戏、终端交互、数据库协议、控制面消息来说，这种抖动很讨厌。

`TCP_NODELAY` 的作用就是禁用 Nagle。Linux `tcp(7)` 里说得很直接：设置 `TCP_NODELAY` 后，segment 会尽快发送，即使只有很少的数据。Go 的 `net.TCPConn.SetNoDelay` 文档也写明，Go 默认是 no delay，也就是 `Write` 后尽可能快地发送。这解释了为什么很多 Go 网络服务默认更偏向低延迟，而不是让内核替你攒小包。

但 `TCP_NODELAY` 不是“开启以后性能一定更好”。它只是把“是否合并小写入”的责任更多交给应用。你如果每个字段、每个 header、每个 protobuf fragment 都单独 `Write`，同时又打开 `TCP_NODELAY`，内核就可能真的发出很多小包。低延迟是低了，包数、系统调用、网卡中断、拥塞窗口消耗都可能上去。高 QPS 下，这会变成 CPU 和网络成本。

更稳的做法是：延迟敏感的连接可以关闭 Nagle，但应用自己要合并写。比如用 `bufio.Writer`、一次序列化完整 frame、一次 `Write` 写出 length-prefix + payload，或者在 HTTP/2/gRPC 里交给框架的 framing 和 flow control。不要一边开 `TCP_NODELAY`，一边把一个逻辑消息拆成十几次小写。

可以按场景选：

| 场景 | 倾向 | 原因 |
|---|---|---|
| 交互式请求/响应、RPC 控制面、低延迟交易 | `TCP_NODELAY` | 小请求不希望被未确认数据挡住 |
| 大文件传输、日志批量上传、备份同步 | 保持 Nagle 或应用批量写 | 吞吐和包效率更重要 |
| 自定义二进制协议，有明确 frame | 常开 `TCP_NODELAY`，但应用合并 frame 写入 | 降低尾延迟，同时避免碎片写 |
| 很多 tiny write 的旧代码 | 先修写入方式，再谈 `TCP_NODELAY` | 否则只是把坏写法暴露到网络上 |

还有一个相关选项是 `TCP_CORK`。Linux 上它更像“先别发，我还在拼一个完整包”，常用于把 header 和 body 拼到一起发送。`TCP_NODELAY` 偏向尽快发，`TCP_CORK` 偏向攒完整。它们不是同一个方向的优化。做服务端响应时，如果你能明确知道什么时候拼完一个完整响应，应用层批量写通常比单纯依赖 Nagle 更可控。

面试里也可以说说排查方法。怀疑 Nagle/delayed ACK 造成延迟时，不要只看代码里的 `SetNoDelay`。要抓包看小包间隔、ACK 延迟、payload 大小、PSH/ACK 时序；再看应用是否多次小写、是否用了 flush、是否有 TLS record 分片、是否经过代理。很多“TCP_NODELAY 没开”的判断其实是猜的，抓包以后才发现是应用层 flush 或 TLS record 切得太碎。

结合 LogServe，如果 worker 到 logd 的协议是高频小控制消息，比如 claim task、append result metadata、ack offset，我会倾向于使用长连接并关闭 Nagle，避免控制面请求被小包合并策略拖住。但同时要保证协议 framing 一次写完整消息，不要把长度、header、body 分成多次 `Write`。如果是批量上传执行日志、trace 或大结果引用，则应该批量发送，追求吞吐和压缩效率。

一句话总结：

```text
Nagle 用延迟换更少的小包，TCP_NODELAY 用更多小包风险换更低发送延迟。低延迟 RPC 通常会关 Nagle，但前提是应用自己做好消息聚合；吞吐型批量传输则不该盲目追求 no delay。
```

## Q006. TCP 拥塞控制和流量控制有什么区别？

**回答：**

这两个词很容易混在一起，因为它们最后都会限制发送端能发多少数据。但它们管的对象不同。流量控制保护接收端，问的是“对端应用和接收缓冲区还能不能吃下这些字节”；拥塞控制保护网络路径，问的是“中间链路、路由器队列和整个路径还能不能承受这些包”。

在 TCP 里，发送端实际能在网络中保持的未确认数据量，受两个窗口共同限制：

```text
allowed_in_flight = min(rwnd, cwnd)
```

`rwnd` 是 receiver window，也就是接收端通过 ACK 报文通告出来的接收窗口。接收端应用读得慢、socket receive buffer 堆积，`rwnd` 就会变小，极端情况下会变成 0。这时不是网络拥塞，而是接收端没有空间。发送端要停下来，等对端窗口更新。RFC 9293 和 RFC 5681 都把 `rwnd` 看作接收端给出的限制，它的语义很清楚：不要把对端内核和应用压爆。

`cwnd` 是 congestion window，它是发送端自己维护的拥塞窗口。它不来自对端应用，而是来自发送端对路径状态的估计。慢启动、拥塞避免、快速重传、快速恢复这些算法都在调整 `cwnd`。RFC 5681 的核心思路是：刚开始不知道路径容量，就逐步探测；出现丢包、重复 ACK、RTO 或 ECN 这类信号时，认为路径可能拥塞，降低发送速率。它保护的是共享网络，不是某个接收进程。

可以用表格拆开：

| 维度 | 流量控制 | 拥塞控制 |
|---|---|---|
| 保护对象 | 接收端 buffer 和应用读取能力 | 网络路径、队列和共享带宽 |
| 主要变量 | `rwnd` | `cwnd`、`ssthresh` |
| 信号来源 | 接收端 ACK 里的 advertised window | ACK 到达节奏、丢包、重复 ACK、RTO、ECN |
| 典型症状 | 对端读得慢，本端发送被窗口挡住 | RTT 上升、重传增多、吞吐下降、尾延迟抖动 |
| 应用侧动作 | 加快消费、增大接收缓冲、降低上游写入 | 限流、退避、减少并发、避免重试风暴 |

一个常见误判是：看到发送慢，就说网络拥塞。其实如果 `rwnd` 很小，说明对端应用没及时读，或者接收端 buffer 配得太小；这时加带宽没有用。反过来，如果 `rwnd` 很大，但 `cwnd` 因为丢包不断缩小，问题在路径拥塞、无线链路质量、跨区网络、队列过深或突发流量上。

RPC 场景里，这个区别会直接影响排查方向。假设 LogServe 的 worker 通过长连接向 logd 追加结果：如果 logd 的 append 线程被 fsync 或锁竞争拖住，接收端应用读 socket 变慢，可能先表现为流量控制；如果跨节点网络出现丢包，发送端的 `cwnd` 会下降，所有共享这条连接的请求都变慢。前者应该看 logd 的消费能力、buffer、goroutine 和磁盘路径；后者应该看 retransmit、RTT、网卡队列、LB/NAT 和路径质量。

还要注意，应用层 backpressure 和 TCP flow control 不是一回事。TCP 的 `rwnd` 只能说 socket 接收缓冲区有没有空间，它不知道业务队列是不是已经满了。一个服务可以持续从 socket 读数据，让 `rwnd` 看起来正常，但业务队列已经堆积。成熟的 RPC 系统通常还要有应用层限流、队列长度、deadline 和 overload 信号。不要把 TCP 窗口当作完整的服务健康信号。

面试里可以这样收束：

```text
流量控制用 rwnd 保护接收端，拥塞控制用 cwnd 保护网络路径。发送端能发多少取 min(rwnd, cwnd)。如果对端读不动，是流量控制问题；如果路径丢包、RTT 变差、cwnd 被压低，是拥塞控制问题。RPC 排查时必须分清这两类，否则容易把应用消费慢误判成网络慢。
```

## Q007. 丢包、重传、乱序会如何影响 RPC 延迟？

**回答：**

它们对 RPC 的影响主要体现在尾延迟，而不是平均延迟。一次 RPC 看起来只是一个 request 和一个 response，但底下要经过 DNS、连接池、TCP/TLS、HTTP/2 或 HTTP/1.1 framing、序列化、服务端排队和业务处理。只要其中一个 TCP segment 丢了，整个调用的完成时间就可能从几十毫秒跳到几百毫秒甚至秒级。

先说丢包。TCP 把丢包当作可靠传输和拥塞控制的输入。发送端可能通过重复 ACK 触发 fast retransmit，也可能等 RTO 超时后重传。前者通常还算快，后者代价很高。RFC 6298 描述 RTO 计算时会维护 SRTT 和 RTTVAR；一旦进入 RTO 路径，等待时间按重传定时器走，尾延迟会很难看。对 RPC 来说，这不是“某个包慢了一点”，而是整个响应完成不了。

重传还会带来第二层影响：拥塞窗口会收缩。RFC 5681 要求 TCP 在重传超时、重复 ACK 等信号出现时降低发送速率。于是一个丢包不只拖慢当前缺失的字节，还会让后续发送变保守。高 QPS RPC 如果很多调用共享同一条连接，丢包后的 `cwnd` 下降会把后面一批请求也带慢。

乱序不一定等于丢包。网络可能因为多路径、ECMP、队列调度或链路抖动让后发的 segment 先到。接收端看到序列号中间有洞，会对后续到达的数据发重复 ACK，推动发送端更快发现缺口。RFC 5681 也提到 out-of-order data 应该尽快 ACK，以加速 loss recovery。但应用层看不到这些已经到达的后续字节，因为 TCP 对应用暴露的是有序字节流。

这就是对 RPC 最麻烦的地方：TCP 会帮你恢复顺序，但恢复之前应用层被挡住。假设 response 分成 10 个 segment，第 3 个丢了，第 4 到第 10 个都到了，内核仍然不能把第 4 个之后的数据交给应用，因为字节流中间缺了一段。等第 3 个重传回来，应用才继续读。对 HTTP/2 更明显：多个 stream 复用在同一条 TCP 连接上，一个底层 TCP segment 丢失，可能挡住后续所有 stream 的 frame 交付。

可以把一次 RPC 的延迟拆成这样：

```text
rpc_latency =
  queue_wait
  + serialization
  + send_wait
  + network_rtt
  + loss_recovery_or_retransmit_wait
  + server_processing
  + response_delivery
```

丢包、重传、乱序主要拉长 `send_wait`、`network_rtt` 和 `loss_recovery_or_retransmit_wait`。它们还会让重试更危险。客户端 deadline 到了以后重试，如果原请求其实只是卡在重传恢复中，下游可能同时处理原请求和重试请求。没有幂等键、去重或取消语义时，RPC 可靠性问题会变成业务语义问题。

指标上不要只看应用 p99。要同时看 TCP retransmits、RTO、duplicate ACK、RTT、连接池等待、HTTP/2 stream 队列、gRPC deadline exceeded、retry count。抓包时可以看是否出现大量 duplicate ACK、SACK block、重传包、RST，或者 response 的后半段早已到达但应用迟迟读不到。

结合 LogServe，worker 向 control 或 logd 发送高频小 RPC 时，丢包会直接影响 task ack、result append、heartbeat 和 lease refresh。最危险的不是某一次 RPC 慢，而是 heartbeat 或 lease 续约被网络恢复时间拖住，控制面误以为 worker 掉线，又把任务重新分配出去。这里要靠 deadline、幂等 append、epoch fencing、重试退避和应用层状态判断一起兜住，不能只相信 TCP 最终会重传成功。

面试里可以这样回答：

```text
丢包会触发重传和拥塞窗口下降；RTO 路径会显著拉高尾延迟。乱序会产生重复 ACK 和接收端缓存，但 TCP 是有序字节流，缺口补齐前应用层读不到后续字节。RPC 共享连接时，一个底层丢包可能拖慢多个调用，HTTP/2/gRPC 尤其明显。排查时要把应用 p99、deadline、重试和 TCP retransmit/RTO 放在一起看。
```

## Q008. head-of-line blocking 在 TCP 中如何产生？

**回答：**

TCP 里的 head-of-line blocking 来自它的基本抽象：可靠、有序、面向字节流。这个抽象对应用很友好，应用读到的字节不会乱、不缺、不重复；代价是只要前面的字节缺了，后面的字节即使已经到达，也不能越过缺口交给应用。

一个简单例子：

```text
发送端发送：segment 1, segment 2, segment 3, segment 4
网络中丢失：segment 2
接收端收到：segment 1, segment 3, segment 4
应用可读：segment 1
应用暂时不可读：segment 3, segment 4
```

接收端内核可以缓存 segment 3 和 segment 4，也可以通过 duplicate ACK 或 SACK 告诉发送端中间缺了 segment 2。但在 segment 2 重传并补齐之前，应用层只能读到连续字节流的前缀。所谓 head-of-line，就是队头那个缺失的字节挡住了后面已经到达的数据。

HTTP/1.1 里有两层 HOL。第一层是应用层：如果在一条连接上 pipeline 多个请求，服务端必须按请求顺序发送响应，前一个慢响应会挡住后一个快响应。RFC 9112 对 pipelining 的约束就要求响应按请求接收顺序返回。第二层是 TCP 层：即使没有 pipelining，只要响应字节流中间有缺口，后面的字节也读不到。

HTTP/2 解决了 HTTP/1.1 的应用层 pipeline HOL，因为它把请求和响应拆成 frame，并用 stream id 在同一条连接上交错传输。RFC 9113 也明确说 HTTP/2 允许在同一连接上 interleaving messages。但它仍然跑在一条 TCP 连接上。底层 TCP 字节流一旦有缺口，HTTP/2 解析器拿不到后续 frame，其他 stream 也会跟着卡。也就是说，HTTP/2 解决的是应用层队头阻塞，不解决 TCP 层队头阻塞。

TLS 会让现象更难排查。TLS record 也要按字节流读取和解密。一个 TCP 缺口可能让 TLS 层无法组装完整 record，HTTP 层看起来只是 read 卡住。你在应用日志里看到的是某个 RPC deadline exceeded，但根因可能是底层一个 segment 丢了，或者网卡队列/中间代理造成重传。

QUIC 的改进点就在这里。RFC 9000 说 QUIC 的 stream 是连接内的有序字节流，但不同 stream 之间不保证字节顺序；它还说明 QUIC 的一个收益是避免多 stream 之间的 head-of-line blocking。丢包只会阻塞包含在丢失 packet 里的那些 stream，其他 stream 可以继续前进。当然，单个 stream 内部仍然有顺序要求，丢了这个 stream 前面的数据，它自己还是会被挡住。

工程上，TCP HOL 的缓解方法通常有几类：

```text
减少单连接上互相影响的请求：按目标、优先级或负载拆连接。
控制大响应：不要让大对象和小控制 RPC 混在同一条连接上互相拖。
设置 deadline：让调用方别无限等底层恢复。
观测底层网络：看 retransmit、RTO、RTT 和 HTTP/2 stream 队列。
需要时使用 QUIC/HTTP/3：让不同 stream 的丢包影响更隔离。
```

结合 LogServe，如果 control-plane 心跳、task claim 和大结果上传都共享同一条 HTTP/2/gRPC 连接，大结果传输的丢包可能拖住心跳 frame。更稳的设计是把控制面小 RPC 和大数据路径分开，或者至少用不同 channel、不同 deadline、不同流控参数。否则你会看到一个很怪的现象：业务处理没慢，logd 也没慢，但 worker 的控制面 RPC p99 被大响应和 TCP HOL 拉高。

一句话回答：

```text
TCP HOL 来自有序字节流：前面的字节缺失时，后面已经到达的字节不能交给应用。HTTP/2 多路复用能消除 HTTP/1.1 pipeline 的应用层 HOL，但因为仍跑在单条 TCP 字节流上，底层丢包仍会挡住所有 stream 的 frame 交付。QUIC 把顺序约束缩小到单个 stream，减少了跨 stream 的 HOL。
```

## Q009. HTTP keep-alive 为什么重要？

**回答：**

HTTP keep-alive 重要，是因为连接本身很贵。一次看起来普通的 HTTP 请求，如果每次都新建连接，背后至少要做 TCP 三次握手；HTTPS 还要做 TLS 握手、证书校验、密钥协商和 ALPN；连接建立后还要经历 TCP 慢启动，拥塞窗口从较小的值逐步探测路径容量。请求量一上来，握手、端口、fd、TLS CPU 和慢启动都会变成成本。

HTTP/1.1 默认倾向于持久连接。RFC 9112 对 persistent connection 的规则说得很细：客户端可以在持久连接上继续发送请求，直到发送或收到 `Connection: close`；如果想复用连接，客户端必须读完整个 response body，服务端也必须读完整个 request body，否则连接上的剩余字节会被误解成下一条消息。这说明 keep-alive 不是一个简单开关，它依赖正确的消息边界和连接状态管理。

keep-alive 的收益有几类。

第一，少握手。TCP/TLS 握手少了，首包延迟和 CPU 都会下降。对内部 RPC 来说，这一点更明显：如果 worker 每次 append log 都新建 TCP/TLS，控制面延迟会被握手成本污染；复用连接后，绝大多数请求走热连接。

第二，保留拥塞窗口和路径状态。短连接每次从较小的拥塞窗口开始，刚要跑顺就关掉。长连接可以保留已经探测出的路径能力，对连续请求更友好。尤其是跨 AZ、跨地域或移动网络，慢启动成本不小。

第三，减少资源抖动。大量短连接会制造 `TIME_WAIT`、临时端口消耗、NAT 表压力、负载均衡器连接跟踪压力和服务端 accept 队列压力。keep-alive 把“每请求一个连接”改成“多个请求复用连接”，系统更稳定。

第四，连接池才能生效。客户端连接池、HTTP transport、gRPC channel 都依赖连接复用。连接不复用，连接池只是摆设。很多线上问题不是服务端处理慢，而是客户端 transport 没有复用，导致每个请求都在 dial、TLS handshake、等待连接槽位。

但 keep-alive 也有边界。连接保持太久会占 fd、内存、TLS state 和连接表；空闲连接可能被 NAT、LB、防火墙、服务端 idle timeout 关闭；客户端拿到一条看似可用的 idle connection，发请求时才发现对端刚关，这会产生一次额外重试。RFC 9112 也提醒连接可以在任何时候被关闭，实现必须准备恢复异步关闭事件。

所以工程上要把几个参数配在一起：

```text
MaxIdleConns / MaxIdleConnsPerHost
IdleConnTimeout
TLSHandshakeTimeout
ResponseHeaderTimeout
ExpectContinueTimeout
HTTP/2 max concurrent streams
服务端 idle timeout
LB / NAT idle timeout
```

如果客户端 idle timeout 比 LB 长很多，客户端可能复用已被 LB 清掉的连接，首个请求失败；如果服务端 idle timeout 太短，连接频繁被服务端主动关，`TIME_WAIT` 和重连会增多；如果连接数过少，HTTP/1.1 下会排队，HTTP/2 下可能被单连接流控和 HOL 放大。

结合 LogServe，我会把 keep-alive 看作控制面稳定性的基础配置。worker 到 control、worker 到 logd、SDK 到 gateway 这类高频小请求，应该复用连接，并为控制面 RPC 设置短 deadline。大结果上传、trace 批量写入或对象存储路径则可以用单独连接池，避免大流量把控制面连接拖慢。

可以这样回答：

```text
HTTP keep-alive 通过复用 TCP/TLS 连接，省掉握手、减少慢启动、降低 fd/端口/NAT/LB 压力，并让连接池真正生效。它不是越久越好，必须和 idle timeout、连接池大小、LB 超时、请求 deadline 配套。否则会出现 stale connection、连接泄漏或单连接排队。
```

## Q010. HTTP/1.1、HTTP/2、HTTP/3 的核心差异是什么？

**回答：**

最短的说法是：HTTP/1.1 是文本消息加连接复用，HTTP/2 是在 TCP 上的二进制分帧和多路复用，HTTP/3 是把 HTTP 语义映射到 QUIC 上。三者的 HTTP 语义很接近，差异主要在传输、连接复用、并发、头部压缩和丢包影响。

HTTP/1.1 的基本单位还是一条请求和一条响应。它支持持久连接，能在同一条 TCP 连接上发多个请求；也支持 pipelining，但响应必须按请求顺序返回，所以前一个慢响应会挡住后一个。实际工程里，浏览器和客户端通常靠多条连接并发来绕开这个限制。它简单、可调试、兼容性强，但连接数多、头部重复、应用层 HOL 明显。

HTTP/2 把 HTTP 消息拆成二进制 frame，用 stream id 区分同一连接上的多个请求和响应。RFC 9113 说明 HTTP/2 允许在同一连接上 interleaving messages，并使用更高效的 HTTP fields 编码。它解决了 HTTP/1.1 多连接和 pipelining 的很多问题：一个 TCP 连接上可以同时跑多个 stream，头部用 HPACK 压缩，服务端可以用 GOAWAY、RST_STREAM、WINDOW_UPDATE 管理连接和流。

HTTP/2 的问题是底层仍然是一条 TCP 连接。TCP 层丢包会挡住整条字节流，所以多个 stream 共享了底层 HOL 风险。HTTP/2 还有连接级和 stream 级 flow control，配不好会出现一个大 stream 占住窗口、小 stream 排队的情况。gRPC 大量使用 HTTP/2，所以 gRPC 性能排查经常要看 max concurrent streams、flow control、单连接拥塞和连接复用。

HTTP/3 使用 QUIC。RFC 9114 定义 HTTP/3，RFC 9000 定义 QUIC 传输。QUIC 跑在 UDP 上，把加密、可靠传输、流、多路复用、拥塞控制和连接迁移放到用户态协议里。HTTP/3 的 stream 由 QUIC 提供，不再共享 TCP 的单一有序字节流。因此一个 QUIC packet 丢失时，通常只阻塞相关 stream，其他 stream 可以继续前进。它还通过 TLS 1.3 集成握手，支持连接 ID 和连接迁移。

可以用表格记：

| 维度 | HTTP/1.1 | HTTP/2 | HTTP/3 |
|---|---|---|---|
| 传输 | TCP | TCP | QUIC over UDP |
| 消息格式 | 文本起始行和 header | 二进制 frame | HTTP/3 frame over QUIC stream |
| 并发方式 | 多连接；pipelining 很少用 | 单连接多 stream | QUIC 多 stream |
| 头部压缩 | 基本没有 | HPACK | QPACK |
| HOL 问题 | 应用层 pipelining HOL + TCP HOL | 消除应用层 pipelining HOL，仍有 TCP HOL | 减少跨 stream HOL，单 stream 内仍有顺序阻塞 |
| TLS/握手 | HTTPS 时 TCP + TLS 分层 | HTTPS 时 TCP + TLS + ALPN | QUIC 内集成 TLS 1.3 |
| 连接迁移 | 四元组变了通常断 | 四元组变了通常断 | Connection ID 支持路径变化 |

不要把 HTTP/3 简化成“HTTP/2 的升级版，必然更快”。在稳定低丢包的数据中心内网里，HTTP/2/gRPC 已经很好；HTTP/3 的 UDP、用户态协议栈、加密处理、负载均衡和可观测性成本也要考虑。HTTP/3 更明显的优势通常出现在高丢包、移动网络切换、弱网、多 stream 并发和跨网络路径变化的场景。

结合 LogServe，如果内部 RPC 主要是数据中心内的 gRPC，HTTP/2 是现实选择：生态成熟、工具完善、Go 支持好。HTTP/3/QUIC 值得关注，但不应该为了“新”而改传输层。真正影响当前系统的是连接复用、deadline、flow control、message size、重试和控制面/数据面隔离。

面试里可以这样说：

```text
HTTP/1.1 以请求响应和持久连接为核心，并发主要靠多连接；HTTP/2 在 TCP 上做二进制分帧、多路复用和 HPACK，适合 gRPC，但仍受 TCP 层 HOL 影响；HTTP/3 把 HTTP 语义放到 QUIC 上，用 UDP、TLS 1.3、QUIC stream 和 connection ID 减少跨 stream HOL 并支持连接迁移。语义相近，传输模型差异很大。
```

## Q011. QUIC 解决了 TCP+TLS+HTTP/2 的哪些问题？

**回答：**

QUIC 不是把 TCP 改个名字搬到 UDP 上。它把传输、加密、流、多路复用和连接管理重新组合了一次，主要针对 TCP+TLS+HTTP/2 在现代网络里的几个痛点。

第一个痛点是建连延迟。传统 HTTPS over TCP 至少要先 TCP 三次握手，再 TLS 握手，然后才能发 HTTP 请求。TLS 1.3 已经把握手做快了，但 TCP 和 TLS 仍然是分层的。QUIC 把 TLS 1.3 集成进传输握手里。RFC 9001 说明，在没有丢包时，新连接通常可以在一个 RTT 内建立并加密；会话恢复时还可以用 0-RTT 发送应用数据。这个收益对短连接、移动网络和高 RTT 路径很明显。

第二个痛点是 HTTP/2 over TCP 的跨 stream HOL。HTTP/2 在应用层可以多路复用，但底层仍是一条有序 TCP 字节流。一个 TCP segment 丢了，后续 frame 无法交给 HTTP/2 层，多个 stream 会一起卡。QUIC 在连接内提供多个 stream，stream 之间没有 TCP 那种全连接字节顺序。RFC 9000 明确说 QUIC 的好处之一是避免多个 stream 之间的 head-of-line blocking；丢包通常只挡住包含在丢失包里的 stream。

第三个痛点是连接迁移。TCP 连接由四元组识别：源 IP、源端口、目的 IP、目的端口。手机从 Wi-Fi 切到蜂窝网络，或者 NAT 重新映射端口，四元组变了，TCP 连接通常就断。QUIC 用 connection ID 标识连接，路径变化后仍可以把包关联到原连接，再通过 path validation 确认新路径。这对移动端和 NAT 复杂环境有价值。

第四个痛点是传输协议演进慢。TCP 在内核和中间设备里，部署新能力很慢，还可能被中间盒干扰。QUIC 在 UDP 上实现大部分传输逻辑，很多行为在用户态库里升级，部署速度更快。当然，这不是免费午餐：UDP 被某些网络限速或阻断、负载均衡要理解 QUIC connection ID、观测工具也要更新。

第五个痛点是安全边界更统一。QUIC 默认加密，包头也尽量保护。RFC 9000 说 QUIC packet 会认证整体并尽可能加密；RFC 9001 则定义 QUIC 如何使用 TLS。相比“明文 TCP + 可选 TLS”的历史包袱，QUIC 的安全假设更现代。HTTP/3 基本就是在加密 QUIC 连接上运行。

但要把边界说清楚。QUIC 不会让丢包消失，不会绕过拥塞控制，也不会让所有 RPC 自动更快。单个 stream 内还是有顺序阻塞；如果一个大响应所在 stream 丢包，它自己仍要等重传。0-RTT 有重放风险，只适合幂等或能承受重放的请求。QUIC 还可能带来更高 CPU、UDP 运维复杂度、LB 支持问题和排障门槛。

对 gRPC/HTTP/2 用户来说，QUIC 解决的是底层传输的一部分问题，不是 RPC 语义问题。deadline、幂等、重试、流控、鉴权、健康检查仍然要做。把 HTTP/2 换成 HTTP/3，不会自动修好无限重试、没有幂等键、连接池滥用或业务队列堆积。

结合 LogServe，如果未来 worker 和 control/logd 跨弱网、跨地域、移动边缘节点通信，QUIC 的连接迁移和跨 stream HOL 隔离会有吸引力。但当前如果是单机或数据中心内机制验证，HTTP/2/gRPC 的确定性和工具链更重要。先把连接复用、deadline、幂等 append、epoch fencing 和观测做好，再考虑传输层替换，顺序更稳。

一句话回答：

```text
QUIC 主要解决 TCP+TLS+HTTP/2 的三类问题：建连延迟高、HTTP/2 仍受 TCP 跨 stream HOL 影响、四元组变化导致连接断开。它通过 UDP 上的加密传输、TLS 1.3 集成握手、多 stream、connection ID 和用户态演进来改善这些问题。但它不消除丢包、不替代 RPC deadline/幂等/鉴权，也不保证所有场景都更快。
```

## Q012. TLS 握手的主要步骤是什么？

**回答：**

现在面试里通常按 TLS 1.3 回答。TLS 1.2 还有 RSA key exchange、额外往返和不同的消息顺序，但现代 HTTPS、gRPC、QUIC 讨论时，TLS 1.3 更有代表性。核心目标是三件事：协商参数、认证身份、派生会话密钥。握手完成后，应用数据才在加密通道里传输。

一次典型 TLS 1.3 握手大概是：

```text
Client -> Server: ClientHello
Server -> Client: ServerHello
Server -> Client: EncryptedExtensions
Server -> Client: Certificate
Server -> Client: CertificateVerify
Server -> Client: Finished
Client -> Server: Finished
```

`ClientHello` 是客户端开场。它会带支持的 TLS 版本、随机数、cipher suites、supported groups、key_share、SNI、ALPN 等扩展。SNI 让服务端知道客户端想访问哪个域名，便于同一 IP 上选择证书；ALPN 用来协商应用层协议，比如 `h2` 或 `http/1.1`。

`ServerHello` 是服务端选择参数。服务端选 TLS 版本、cipher suite、key share 等。到这里，双方已经可以基于 ECDHE 之类的密钥交换材料计算握手密钥，后续很多握手消息会被加密保护。TLS 1.3 相比 TLS 1.2 的一个变化是，握手更早进入加密状态，消息顺序也更简洁。

`EncryptedExtensions` 承载那些不需要放在 ServerHello 里的扩展结果，比如 ALPN 选择。然后服务端发 `Certificate`，把证书链交给客户端；发 `CertificateVerify`，用证书私钥对握手 transcript 做签名，证明自己确实持有证书对应私钥；最后发 `Finished`，用握手密钥保护并校验整个 transcript，防止握手过程被篡改。

客户端收到后要做几件事：验证证书链是否能连到受信任 CA，检查域名是否匹配，检查有效期，检查 key usage / extended key usage，必要时检查吊销状态或策略；再验证 `CertificateVerify` 和 `Finished`。这些都通过后，客户端发自己的 `Finished`。握手完成后，双方切到 application traffic keys，开始收发 HTTP/gRPC 数据。

如果启用 mTLS，服务端还会发送 `CertificateRequest`。RFC 8446 说，如果服务端希望基于证书做客户端认证，就发送这个消息；客户端随后发送自己的 `Certificate` 和 `CertificateVerify`。这时服务端也要验证客户端证书链和身份映射。

会话恢复是另一个常见点。TLS 1.3 可以用 PSK/resumption 减少后续连接的成本；某些情况下还能 0-RTT 发送早期数据。但 0-RTT 有重放风险，不能随便用于非幂等操作。比如提交订单、扣款、写日志 append 这类请求，如果没有业务级去重，不应盲目用 0-RTT。

把它放到 RPC 里看，TLS 握手不只是“加密一下”。它会影响首请求延迟、连接池预热、证书轮换、ALPN 协商、mTLS 身份、负载均衡终止位置和错误排查。很多 `deadline exceeded` 其实发生在请求发出前：DNS、dial、TCP handshake、TLS handshake、连接池等待都吃掉了 deadline。

面试里可以这样说：

```text
TLS 1.3 握手先由 ClientHello/ServerHello 协商版本、套件、key share、SNI 和 ALPN；然后服务端通过 Certificate 和 CertificateVerify 证明身份，双方用 Finished 校验握手 transcript，并派生应用数据密钥。mTLS 会多出 CertificateRequest 和客户端证书验证。握手完成后，HTTP/gRPC 数据才作为加密 application data 发送。
```

## Q013. mTLS 解决什么问题？

**回答：**

mTLS 解决的是连接两端的强身份认证，尤其是服务端识别客户端身份的问题。普通 TLS 里，客户端通常验证服务端证书：我是不是连到了 `api.example.com`，证书链和域名是否可信。mTLS 是 mutual TLS，服务端也要求客户端出示证书，并验证这个证书来自受信 CA、未过期、用途正确，然后把证书里的身份映射成调用方 principal。

它最适合服务间调用。内部网络不能再假设“进了 VPC 就可信”。服务 A 调服务 B 时，B 需要知道对面到底是 `worker-service`、`scheduler`、`gateway`，还是某个伪装进来的进程。mTLS 通过握手阶段的客户端证书把身份绑定到连接上，比普通 header 可靠得多。Header 可以被应用伪造，客户端证书私钥不能靠随便写个 metadata 冒充。

TLS 1.3 的机制也很直接。服务端发送 `CertificateRequest` 表示需要客户端证书；客户端发送 `Certificate` 和 `CertificateVerify`；服务端验证证书链、签名、有效期和策略。RFC 8446 还说明，如果客户端没有合适证书，服务端可以选择继续无客户端认证，也可以用 `certificate_required` 这类 alert 中止握手。生产里通常会选择中止，否则 mTLS 形同虚设。

mTLS 常解决这些问题：

```text
服务间身份：确认调用方是哪一个 workload。
防止旁路调用：没有证书的进程不能直接打内部管理接口。
通道机密性和完整性：继承 TLS 的加密和防篡改。
证书身份绑定：把 SAN、URI SAN、SPIFFE ID 或组织内身份映射到 principal。
零信任基础：网络位置不再等于权限，连接身份成为授权输入。
```

但它不是完整鉴权。mTLS 告诉你“对面是谁”，不自动告诉你“它能做什么”。服务端仍然要按 method、tenant、resource、role、scope、operation 判断权限。比如 `worker-service` 可以上报 heartbeat，不代表它可以删除所有 actor snapshot；`gateway` 可以提交 workflow，不代表它可以伪造 control-plane epoch。

它也不是用户身份的万能替代。一个内部服务用 mTLS 调另一个服务，只能说明服务身份。终端用户是谁、token 是否有效、租户是否匹配，仍然要靠 JWT/OAuth/session、业务 ACL 和审计。很多系统会同时使用 mTLS 做 workload 身份，用 bearer token 或 signed metadata 做用户/租户身份。

mTLS 的真实成本在证书生命周期。证书要签发、分发、轮换、吊销、过期告警；私钥要保护；CA 根和中间证书要滚动；客户端和服务端 trust bundle 要一致；时钟漂移会影响有效期判断。mTLS 把“网络信任”变成了“证书体系信任”，安全性更强，但运维复杂度也上来了。

结合 LogServe，如果以后 control、worker、logd、gateway 分布到多进程甚至多机器，mTLS 可以保护控制面入口：只有持有 worker 身份证书的进程能注册和心跳，只有 control 身份能下发 actor ownership 或 epoch fencing 相关操作。但授权仍然要在应用层做，不能只因为 mTLS 握手通过，就允许任意 append 或任意 lease 操作。

面试里可以这样答：

```text
mTLS 在 TLS 的服务端认证基础上增加客户端证书认证，让服务端也能确认调用方身份。它适合内部服务间调用、service mesh 和管理面 API，可以防止伪造客户端、减少旁路访问，并为授权提供强 principal。但 mTLS 只解决“是谁”和通道安全，不自动解决“能做什么”；证书签发、轮换、吊销和过期也要完整治理。
```

## Q014. 证书过期会造成什么线上风险？

**回答：**

证书过期最直接的风险是 TLS 握手失败。客户端在验证证书链时会检查当前时间是否落在证书 `notBefore` 到 `notAfter` 之间。RFC 5280 对 validity period 的定义就是这两个时间点；证书路径验证也要求当前时间包含在证书有效期内。过了 `notAfter`，正常客户端应该拒绝这张证书。

线上表现通常不是“安全性降低一点”，而是调用直接断。浏览器会报证书错误，HTTP client 会返回 certificate has expired 或类似错误，gRPC 会在建连阶段失败，服务发现健康检查可能判定实例不可用。对外入口证书过期，会造成用户无法访问；内部 mTLS 客户端证书过期，会造成服务间调用失败；中间证书或根链配置错误，则可能是一批服务同时失败。

风险有几类。

第一，入口流量中断。HTTPS 证书过期后，严格客户端不会继续请求。即使服务端业务进程完全健康，TLS 层也过不去。监控如果只看进程存活和端口监听，可能误判服务正常。

第二，内部依赖雪崩。某个内部 API 的服务端证书过期，调用方开始大量失败和重试；重试又打满连接池、线程池和日志系统。mTLS 下客户端证书过期同样危险：只有那一批实例无法被服务端认证，看起来像“部分 worker 掉线”。

第三，长连接掩盖问题。已有 TLS 连接可能在证书过期后继续使用，因为证书验证发生在握手时；新连接失败，老连接暂时正常。于是故障会表现为滚动发布、连接重建、实例重启后才爆发。这个特征很坑：你以为发布导致故障，其实发布只是触发了新握手。

第四，证书链和时钟问题。服务端叶子证书没过期，但中间证书过期、链顺序配置错、客户端 trust store 太旧，也会失败。机器时间漂移也会让客户端认为证书尚未生效或已经过期。这个问题在容器、边缘节点、离线环境里更常见。

第五，自动续期没有真正生效。证书文件更新了，但进程没有 reload；LB 终止 TLS，应用证书更新了但 LB 没更新；多个 region 只更新了一部分；sidecar 和应用使用不同 trust bundle。证书轮换必须验证“线上握手拿到的新证书”，不能只验证文件存在。

应对方式不是等到过期当天报警。比较稳的做法是：

```text
监控证书 notAfter，提前 30/14/7/3/1 天告警。
从客户端视角主动探测 TLS 握手和证书链。
把入口证书、内部 mTLS 证书、中间 CA、trust bundle 都纳入盘点。
轮换后验证真实端口上的证书，不只看磁盘文件。
让服务支持热加载或滚动重启，并避免所有证书同一时间过期。
```

结合 LogServe，如果未来 worker/control/logd 使用 mTLS，证书过期会被误读成 worker crash、网络分区或 control 不可达。控制面应该把 TLS handshake error、认证失败、证书过期和业务 heartbeat timeout 分开打日志和指标。否则排查时只看到“worker lost”，很难马上定位到证书生命周期。

面试里可以这样说：

```text
证书过期会让正常客户端在 TLS 握手或证书路径验证阶段拒绝连接，外部表现是 HTTPS/RPC 直接不可用。风险包括入口中断、内部 mTLS 调用失败、重试雪崩、长连接掩盖新连接失败、中间证书或 trust bundle 漏更新。生产上要监控 notAfter、做真实握手探测、支持热加载和分批轮换。
```

## Q015. DNS 缓存和 TTL 如何影响故障切换？

**回答：**

DNS TTL 决定的是记录最多可以在缓存里用多久。RFC 1035 对 TTL 的定义很直白：资源记录可以被缓存一段时间，过了这个时间应该重新咨询信息来源；TTL 为 0 表示只用于当前事务，不应缓存。RFC 2181 又补了一句很关键的话：TTL 是 maximum time to live，不是 mandatory time to live。也就是说，解析器可以更早丢弃，也可能因为实现、策略或上限/下限而表现得和你想的不完全一样。

故障切换时，TTL 的影响很直接。假设 `api.example.com` 的 A 记录 TTL 是 300 秒，旧 IP 故障后，权威 DNS 立刻改成新 IP。已经缓存旧记录的递归解析器、操作系统、应用进程，在 TTL 到期前仍可能继续返回旧 IP。于是你会看到一部分客户端已经切到新实例，另一部分还在打旧实例。DNS 不是推送系统，改记录不会立刻冲掉全世界缓存。

低 TTL 能缩短这个窗口，但不等于秒级切换保证。原因有几个：

```text
递归解析器可能设置最小 TTL 或最大 TTL。
操作系统和语言运行时可能有自己的 DNS cache。
应用连接池可能已经连上旧 IP，不会因为 DNS 更新立刻断开。
客户端可能缓存解析结果，甚至只在进程启动时解析一次。
负载均衡器、代理、sidecar 也可能有独立 resolver 行为。
```

这里最容易漏掉的是连接复用。DNS 只影响新解析和新连接。已经建立的 HTTP keep-alive、HTTP/2/gRPC channel、数据库连接、TLS session，不会因为 DNS A 记录变了自动迁移。故障切换如果要让旧连接尽快退出，需要配合连接 draining、GOAWAY、idle timeout、健康检查失败后的连接关闭，或者客户端连接池刷新。

负缓存也会影响恢复。RFC 2308 定义了 DNS negative caching。NXDOMAIN 或 NODATA 这类负响应也可以缓存，并且有自己的 TTL 规则。发布过程中如果某个名字短暂返回 NXDOMAIN，客户端可能在一段时间里持续认为这个名字不存在。很多“服务已经创建，客户端还是解析不到”的问题，根因就是负缓存。

DNS 故障切换还有一个现实边界：它适合粗粒度流量转移，不适合当作强实时故障检测。健康检查 DNS 可以在权威侧停止返回坏 IP，但已经拿到坏 IP 的客户端仍要靠自己的连接失败、重试、退避、重新解析来恢复。客户端如果没有短 deadline 和重试策略，低 TTL 也救不了正在等待的请求。

上线或迁移前通常会先降 TTL：

```text
T-24h: 把 TTL 从 3600 降到 60。
等待旧 3600 TTL 自然过期。
切换 A/AAAA/CNAME 到新目标。
观察新旧 IP 流量和错误率。
稳定后再把 TTL 调回较高值，减少 DNS 查询量。
```

如果没提前降 TTL，切换时才把 TTL 改成 60，已经缓存了 3600 的解析器不会立刻听你的。TTL 是缓存记录被拿到时的属性，不是你后来在权威 DNS 上改了就能追溯生效。

结合 LogServe，如果 SDK、worker 或 gateway 通过 DNS 找 control/logd，故障切换不能只靠改 DNS。客户端要有连接失败后的重新解析，连接池要能丢弃坏连接，RPC 要有 deadline 和指数退避，服务端要有健康检查和 draining。否则 DNS 已经指向新实例，应用仍然卡在旧连接上。

面试里可以这样回答：

```text
DNS TTL 决定正向记录在缓存中的最长使用时间，负响应也可能被缓存。故障切换时，权威 DNS 改记录只影响新的解析；递归解析器、OS、应用缓存和已有长连接会让旧 IP 继续存在一段时间。低 TTL 可以缩短窗口，但不能替代客户端 deadline、重试、重新解析、连接池刷新和服务端 draining。迁移前要提前降 TTL，而不是切换当天才改。
```

## Q016. 连接池为什么需要最大连接数、最大空闲数和连接寿命？

**回答：**

连接池不是为了“缓存越多连接越好”。它真正要控制的是三个东西：并发上限、复用效率和连接新鲜度。最大连接数、最大空闲数、连接寿命分别对应这三个问题。少了其中任何一个，系统都会在压力、故障切换或长期运行后暴露出很难看的问题。

先说最大连接数。一个连接不是免费的。它至少占用客户端 fd、内核 socket buffer、TLS 状态、服务端连接对象、数据库 backend 或 RPC server goroutine。对数据库来说，连接数还可能对应真正的 server process 或 worker slot。对 HTTP/gRPC 来说，连接数太多会带来握手、内存、调度和负载均衡压力。最大连接数的意义是给下游一个硬边界：

```text
upstream concurrency <= pool max connections * per-connection concurrency
```

如果没有这个边界，流量一抖，客户端可能同时打开成百上千个连接。短期看，请求没有排队；实际是把排队从客户端搬到了数据库、RPC server 或内核 accept queue 里。最坏的情况是所有调用方都这么做，下游先被连接数打满，然后超时和重试再把故障放大。

最大连接数也提供一种很直接的 backpressure。池子满了，请求要么等待连接，要么按 deadline 失败。这个失败至少发生在调用方这里，可观测、可限流、可降级。相比让下游被无限连接打穿，这是更可控的失败方式。Go 的 `database/sql.DB.SetMaxOpenConns` 就是这个含义：限制打开到数据库的最大连接数。Go `net/http.Transport.MaxConnsPerHost` 也类似，它限制同一个 host 下 dialing、active、idle 状态的总连接数。

但最大连接数不能单独使用。只设最大连接数，不设最大空闲数，连接池可能在低峰期保留太多闲置连接。闲置连接也占 fd、内存、TLS session、服务端连接槽位，还可能卡住服务端滚动发布。比如服务端已经下线 draining，一个客户端还握着一批老连接不放，新请求继续打到即将退出的实例上。最大空闲数用来控制“愿意为未来复用付出多少资源”。

最大空闲数太低也有问题。每次请求都新建连接，TCP 三次握手、TLS 握手、HTTP/2 preface、gRPC channel warmup 都会重复发生。轻负载下看不出，一到高并发，延迟和 CPU 会被握手成本抬高。数据库连接更明显，新建连接可能包含认证、权限初始化、prepared statement cache 重建。一个合理的空闲池能把热路径从：

```text
connect -> TLS/auth -> request -> close
```

变成：

```text
borrow idle connection -> request -> return to pool
```

这就是最大空闲数存在的价值。它不是容量上限，而是复用预算。Go `database/sql.DB.SetMaxIdleConns` 控制 idle pool 最大连接数；Go `http.Transport.MaxIdleConns` 和 `MaxIdleConnsPerHost` 控制 HTTP keep-alive idle 连接的保留量。这个参数一般要和流量形态一起看：突发型服务需要留一点 idle 吸收下一波请求，低频后台任务可以更保守。

连接寿命解决的是另一个问题：连接不能因为还能用就永远用。长连接会穿过很多外部状态：DNS 解析、负载均衡后端集合、NAT 表、服务端版本、证书、数据库主从切换、防火墙 idle 策略。连接创建时绑定的是当时的世界。这个世界变了，连接池如果不主动换血，就会继续把请求送到旧路径上。

常见例子是 DNS 和负载均衡切换。`api.internal` 的解析已经指向新实例，但旧 HTTP/2 连接还连着旧 IP；连接池只要一直复用它，DNS 更新就不会影响这条连接。再比如数据库 failover 后，旧连接可能还指向旧主库，或者服务端已经准备断开但客户端还没感知。连接寿命就是强制连接在使用一段时间后退休，让池子逐步重新解析、重新握手、重新进入新的后端集合。

连接寿命和空闲超时也要分清：

```text
max idle time: 一个连接空闲多久后可以关掉。
max lifetime: 一个连接从创建开始最多活多久，哪怕中间一直有人用。
```

Go `database/sql` 同时提供 `SetConnMaxIdleTime` 和 `SetConnMaxLifetime`，这两个参数就是这个区别。HTTP `Transport.IdleConnTimeout` 则控制 idle keep-alive 连接最长空闲多久。很多连接池还会加 jitter，避免所有连接在同一秒同时到期，造成连接重建尖峰。

这三个参数要一起调。一个典型思路是：

```text
最大连接数：按下游容量、请求平均耗时和调用方数量倒推。
最大空闲数：按平峰并发和突发程度保留，不让每次请求都握手。
连接寿命：短于负载均衡/NAT/服务端连接生命周期，长于正常请求耗时，最好带 jitter。
```

如果系统是 HTTP/2 或 gRPC，还要注意“连接数”和“并发请求数”不是一回事。一个 HTTP/2 连接可以承载多个 stream。最大连接数太小，所有 stream 会挤在少数连接上，某个连接的 flow control、丢包或后端卡顿会影响更多请求；最大连接数太大，又会损失复用和增加服务端压力。连接池参数必须理解协议。

结合 LogServe，如果 control 到 logd、worker 到 control、SDK 到 gateway 都走长连接，连接池不能只写一个默认值。control 写 shared log 的连接要有明确 deadline 和连接上限，避免 logd 故障时 goroutine 全部卡在借连接或写连接上；worker 的 gRPC channel 要控制 idle 和 keepalive，避免服务重启后一直复用旧连接；如果未来 metadata store 接 PostgreSQL，`SetMaxOpenConns` 要按单机实验和数据库连接预算设，而不是让每个组件无限开连接。

面试里可以这样回答：

```text
最大连接数限制下游并发和资源占用，也把过载变成调用方可观测的等待或超时；最大空闲数控制复用预算，避免每次请求都重新握手，又不让 idle 连接吃光 fd 和服务端槽位；连接寿命让池子定期换血，处理 DNS、LB、NAT、证书、服务端滚动发布和数据库 failover。连接池调参不是只为了性能，也是为了故障边界。
```

## Q017. 连接泄漏如何定位？

**回答：**

连接泄漏的第一步不是马上翻代码，而是先判断“泄漏”到底是什么。真正的泄漏是连接已经不再承担业务工作，却一直没有归还或关闭。它和正常高并发、慢请求、下游变慢造成的连接占用很像。如果不先分清，容易把容量问题误判成代码忘记 `Close()`。

我通常先看四类信号：

```text
进程 fd 数是否持续上升，且流量回落后不下降。
连接池 InUse 是否持续接近上限，Idle 很少，WaitCount/WaitDuration 增长。
TCP 状态是否异常，比如 ESTABLISHED、CLOSE_WAIT、SYN_SENT 长时间堆积。
goroutine 或线程栈是否大量卡在 borrow connection、read、write、TLS handshake 或 response body 读取上。
```

fd 数持续上涨是最粗的入口。Linux 上可以看 `/proc/<pid>/fd` 数量、`lsof -p <pid>`、`ss -tanp`。如果都是 socket，再按 remote address、TCP state、连接创建时间分组。`ESTABLISHED` 很多，不一定泄漏，可能是业务并发真的高；`CLOSE_WAIT` 很多更可疑，它说明对端已经发 FIN，本进程也读到了关闭，但本进程还没有 close 本地 socket。这个通常指向应用层忘记关闭连接、response body、stream 或 wrapper。

连接池指标更关键。以 Go `database/sql` 为例，`DB.Stats()` 里有 `OpenConnections`、`InUse`、`Idle`、`WaitCount`、`WaitDuration`、`MaxIdleClosed`、`MaxLifetimeClosed` 等信息。`InUse` 长期不降，`WaitCount` 不断增加，说明连接被借出去后回不来。HTTP 客户端也类似，要关注 per-host active/idle 连接、连接创建速率、idle close、请求超时和响应 body 关闭情况。

Go 里最常见的泄漏点有几个：

```go
resp, err := client.Do(req)
if err != nil {
    return err
}
defer resp.Body.Close()

// 如果需要复用 HTTP/1.1 keep-alive，通常还要读完或丢弃 body。
_, _ = io.Copy(io.Discard, resp.Body)
```

`resp.Body` 不关，连接就不能回到池里。只关不读完，在某些场景下连接也不能复用，只能被关闭，表现为连接 churn 而不是稳定复用。数据库里也一样：`rows.Close()`、`tx.Commit()` / `tx.Rollback()`、`stmt.Close()`、专用 `Conn.Close()` 都要有清晰所有权。gRPC streaming 则要确认 stream 的 `CloseSend`、context cancel、接收循环退出路径都能走到。

第二个常见问题是错误路径。正常路径写了 `defer Close()`，错误路径早返回时没有执行；或者函数把 body/rows/conn 传给下游，所有权不清楚，调用方和被调用方都以为对方会关。定位这类问题时，单靠代码审查很慢，最好在连接借出位置加轻量追踪：

```text
borrow 时记录连接 id、调用点、goroutine id 或采样 stack。
return/close 时清掉记录。
定期打印超过阈值还没归还的连接及其 borrow stack。
```

这个办法很土，但在线上排查特别有效。池子里每条“借出未归还”的连接都能指向一段代码，比盯着 fd 数猜要快得多。生产上要采样或只在 debug 开关打开时启用，避免每次借连接都抓完整栈。

goroutine profile 也很有用。如果大量 goroutine 卡在 `database/sql.(*DB).conn`，说明它们在等连接；如果卡在 `net.(*conn).Read`，要看是不是读超时没设；如果卡在 `io.Copy` 或业务 handler，要看响应体或流式读取是否没有退出条件。注意这里要把“等连接的人”和“持有连接的人”分开。等待者不是泄漏源，持有者才是。

TCP 状态能帮助判断方向：

```text
CLOSE_WAIT: 对端关闭了，本进程没 close，优先查应用 close 路径。
FIN_WAIT2: 本端关闭写方向，对端迟迟不发 FIN，可能是对端或协议关闭顺序问题。
SYN_SENT: connect 卡住，优先查连接超时、网络、SYN backlog、路由。
ESTABLISHED 长期空闲: 可能是连接池 idle 设置、长连接正常保留，也可能是忘记回收。
TIME_WAIT 多: 通常说明短连接或主动关闭多，不等于泄漏。
```

还要排除“看起来像泄漏”的几种情况。下游慢了，请求持有连接的时间变长，`InUse` 会升高；这不是泄漏，是服务时间变长导致并发占用增加。连接池太小，等待连接的 goroutine 会堆积；这也不是泄漏。服务端没有读请求体或客户端没有读响应体，连接不能复用，可能表现为不断新建连接；这是资源使用不当，不是单纯 fd 漏关。

结合 LogServe，可以按组件拆：SDK 到 control 的 RPC 有没有未取消的 context；worker long-poll 有没有在 worker 退出时关闭；control 到 logd 的 append/read 连接有没有 deadline；dashboard snapshot 的 HTTP client 有没有关 body；result store 或 metadata store 访问有没有把 rows/stream 关闭。LogServe 这种机制验证系统更要把日志打细：连接泄漏会被误认为 worker 丢失、queue redelivery 或 logd 变慢。

面试里可以这样说：

```text
我会先用 fd 数、连接池 Stats、TCP state 和 goroutine profile 判断是不是泄漏。CLOSE_WAIT 指向本进程没 close，InUse 长期不降指向借出未归还，WaitCount 增长说明调用方已经被池子反压。然后在 borrow/return 处加连接 id 和采样 stack，找出超过阈值未归还的调用点。Go 里重点查 resp.Body.Close、rows.Close、tx rollback/commit、stream close 和 context cancel。不要把慢请求、池子太小、TIME_WAIT 多误判成泄漏。
```
## Q018. epoll、select、poll 的区别是什么？

**回答：**

`select`、`poll`、`epoll` 都是在问同一个问题：一批 fd 里，哪些现在可以读、可以写，或者已经出错。区别在于它们怎么表达这批 fd、内核怎么保存关注集合、每次等待要付出多少成本。

`select` 最老，也最受限制。调用方传入读集合、写集合、异常集合，内核检查后把就绪结果写回这些集合。它的问题有三个。第一，fd 编号受 `FD_SETSIZE` 限制，在很多系统上默认就是 1024 这个量级。第二，调用后集合会被修改，下一轮要重新构造。第三，每次调用都要把整套 fd set 在用户态和内核态之间传来传去，然后线性扫描。fd 数量一大，它就很笨重。

`poll` 把 fd set 换成了 `pollfd` 数组：

```c
struct pollfd {
    int   fd;
    short events;
    short revents;
};
```

它没有 `select` 那个固定 fd 编号上限，表达能力也更直接，事件结果放在 `revents`。但它仍然是“每次把数组交给内核，内核扫描一遍，再返回有结果的数组”。如果你监听 10 万个连接，只有 10 个活跃，`poll` 仍然要处理那 10 万个条目。它解决了 `select` 的一部分接口问题，没有解决大规模稀疏活跃连接的根本成本。

`epoll` 的模型不一样。它先创建一个 epoll instance，然后用 `epoll_ctl` 把关注的 fd 加进去、改掉或删掉。内核维护 interest list 和 ready list。等待时调用 `epoll_wait`，拿回的是已经就绪的一批事件。也就是说，关注集合不需要每次完整传入，活跃 fd 也不需要用户态再从头扫一遍。

可以粗略对比成这样：

```text
select: 每次传 fd_set，每次线性检查，有 FD_SETSIZE 限制。
poll: 每次传 pollfd 数组，每次线性检查，没有 select 的固定 fd 编号限制。
epoll: 先注册关注集合，事件发生后进入 ready list，epoll_wait 返回就绪事件。
```

这也是为什么高并发网络服务通常不用 `select`。如果大量连接大部分时间是 idle 的，比如 HTTP keep-alive、gRPC 长连接、WebSocket、消息推送，`epoll` 的事件驱动模型更合适。它不是“每次问所有连接有没有动静”，而是“有动静时再把事件交给我”。

不过 `epoll` 不是免费午餐。它是 Linux 特有接口，跨平台要换成 kqueue、IOCP、event ports 或由运行时封装。它还有几个容易踩的细节：fd 关闭和 open file description 的关系、同一个 fd dup 后是否仍在 interest list、边缘触发必须读写到 `EAGAIN`、`EPOLLONESHOT` 要 rearm、事件缓存里 fd 已关闭时要标记。面试里如果只说 `epoll O(1)`，反而显得很浅。更准确的说法是：`epoll` 避免了每次完整提交和扫描关注集合，在大量 fd、少量活跃 fd 的场景下扩展性更好。

Go 程序一般不会直接写 `epoll`。Go runtime 的 netpoller 会在 Linux 下使用 epoll，在 BSD/macOS 下使用 kqueue，在 Windows 下使用 IOCP。开发者通常只写阻塞风格的 `net.Conn.Read` / `Write`，runtime 负责把 goroutine 挂起和唤醒。理解 epoll 仍然有价值，因为你排查大量连接、goroutine 卡读写、fd 泄漏、CLOSE_WAIT、accept 延迟时，底层还是这些机制。

结合 LogServe，如果 worker 和 control 之间有很多长连接，Go runtime 会帮忙处理网络轮询，不需要自己写 epoll loop。但如果未来引入自研 gateway 或高性能 TCP proxy，就要知道：`select` 不适合大量 fd；`poll` 可以但扫描成本高；`epoll` 更适合高并发 idle 连接，但要把 LT/ET、非阻塞 fd、accept/read/write drain、关闭路径设计清楚。

面试里可以这样回答：

```text
select、poll、epoll 都是 I/O 多路复用。select 用 fd_set，有 FD_SETSIZE 限制，每次调用集合会被改写；poll 用 pollfd 数组，去掉了固定 fd 编号限制，但仍然每次线性扫描；epoll 把关注集合留在内核里，用 ready list 返回就绪事件，适合大量连接少量活跃的场景。epoll 还支持 LT/ET/oneshot，但边缘触发和 fd 生命周期处理更容易出错。
```

## Q019. 水平触发和边缘触发有什么区别？

**回答：**

水平触发和边缘触发的区别，可以用一句话先压住：水平触发关心“现在是否仍然可读可写”，边缘触发关心“状态刚刚发生变化”。这句话很短，但工程后果很大。

水平触发，也就是 LT，是默认模式。只要 fd 还满足条件，`epoll_wait` 就会反复告诉你它就绪。比如 socket receive buffer 里还有 1 KB 数据，你这次只读了 100 字节，下一次 `epoll_wait` 还会继续返回这个 fd。它比较宽容，写起来像 `poll`：

```text
事件来了 -> 读一点 -> 没读完也没关系 -> 下次还会提醒你
```

边缘触发，也就是 ET，只在状态变化时提醒。典型例子是原来不可读，后来新数据到达，内核发一次事件。如果你收到事件后只读了一半，剩下的数据还在 socket buffer 里，但状态没有从“不可读”再次变成“可读”，下一次可能就没有事件了。Linux `epoll(7)` 里也用 pipe 举过类似例子：写入 2 KB，只读 1 KB，ET 模式下后续等待可能卡住。

所以 ET 模式的核心规则是：

```text
fd 必须是 nonblocking。
收到读事件后循环 read，直到返回 EAGAIN/EWOULDBLOCK。
收到写事件后尽量 flush，直到写完或返回 EAGAIN/EWOULDBLOCK。
处理完还没完成的业务状态要自己保存。
```

如果不这么做，程序不会马上报错，而是“安静地卡住”。这类 bug 很难看：连接还在，fd 也在 epoll 里，进程 CPU 不高，就是某个请求再也没有进展。很多新手写 ET 的问题不在 epoll 调用本身，而在没有把协议状态机和非阻塞 I/O 写完整。

LT 的好处是简单、稳。少读一点没关系，没写完也没关系，下一轮还会提醒。代价是事件可能重复，活跃 fd 会不断被返回，用户态要处理更多 wakeup。大多数业务服务器，除非真的在写自己的事件循环，LT 已经够用。

ET 的好处是减少重复通知，适合高性能事件循环。但它把复杂度转移给应用：你要维护 output buffer、input parser、半包状态、写不完的剩余数据、关闭状态、错误状态，还要防止一个连接数据太多导致其他连接饥饿。Linux 手册也提醒，ET 下大量 I/O 可能让一个 fd 长时间占用处理时间，应用通常需要自己的 ready list 做轮转。

accept 也有同样的问题。监听 socket 在 ET 模式下收到可 accept 事件后，不能只 `accept()` 一次就结束，而应该循环 accept，直到返回 `EAGAIN`。否则 accept queue 里剩下的连接可能不会再触发新事件，客户端看起来就像已经连上但服务端迟迟不处理。

还有一个相关参数是 `EPOLLONESHOT`。它表示 fd 返回一次事件后会被禁用，应用处理完后要用 `epoll_ctl(EPOLL_CTL_MOD)` 重新 arm。多线程事件循环里它很常见，用来避免多个线程同时处理同一个 fd。但忘了 rearm，效果和漏读一样：连接静默卡死。

结合 LogServe，如果只写 Go 的 `net/http` 或 gRPC，不需要直接选择 LT/ET，Go runtime 和库已经处理了底层轮询。但理解 LT/ET 能帮助解释一些现象：为什么非阻塞网络库要一直读到 EAGAIN，为什么写 buffer 要自己维护，为什么高性能 server 里一个连接不能无限 drain，为什么 accept loop 要一直 accept 到没有新连接。

面试里可以这样回答：

```text
水平触发是只要条件还成立就持续通知，写法简单，不容易漏事件；边缘触发是状态变化时通知一次，要求 fd 非阻塞，并且每次读写都要 drain 到 EAGAIN。ET 可以减少重复 wakeup，但必须自己维护协议状态、读写缓冲和 rearm 逻辑。少读、少写、accept 一次就停，都会造成连接卡住。
```
## Q020. 惊群问题是什么？

**回答：**

惊群问题指的是：一个事件实际上只需要一个执行者处理，但系统唤醒了一群等待者。大家一起醒来、抢锁、抢 fd、抢任务，最后只有一个成功，其余人发现没活干又睡回去。业务上没有多做事，CPU、调度、锁竞争、缓存失效却都发生了。

最经典的例子是多个进程或线程等待同一个监听 socket：

```text
100 个 worker 阻塞在 accept。
来了 1 个新连接。
内核唤醒很多 worker。
只有 1 个 accept 成功，其余 worker 返回 EAGAIN 或继续睡眠。
```

如果连接持续到来，这种无效唤醒会变成可观的系统开销。高并发服务里它不只是“多醒几次”，还会带来 run queue 抖动、锁竞争、CPU cache 污染、延迟尾部变差。线程越多、事件越细、每次处理越短，惊群越明显。

`epoll` 里也能看到这个问题。多个 epoll instance 或多个线程等同一个目标 fd，如果一个事件让所有等待者都醒，就会浪费。Linux 后来提供了 `EPOLLEXCLUSIVE`，用于一些场景下避免多个 epoll 等待者被同一个事件全部唤醒。`epoll(7)` 还提到，如果多个线程等同一个 epoll fd，且目标 fd 使用边缘触发通知，事件就绪时只会唤醒其中一个等待者，这对避免惊群有帮助。

但惊群不只存在于 `accept`。分布式系统里也有同类问题：

```text
缓存 key 过期，所有请求同时打数据库。
服务恢复，所有客户端立刻重连。
定时任务整点启动，所有实例同时拉取队列。
下游超时，所有调用方按固定间隔同时重试。
```

这些都不是内核 accept 惊群，但模式一样：一个资源或事件，引发大量参与者同时行动，最后真正需要的行动很少，系统被无效竞争拖垮。

解决思路也分层。内核和网络层可以用这些办法：

```text
使用 SO_REUSEPORT，让内核按连接分发到不同 listener。
使用 EPOLLEXCLUSIVE 或合适的事件循环模型，减少同一事件唤醒多个等待者。
accept 后尽快回到事件循环，不在 accept 锁附近做重活。
控制 worker 数，不是线程越多越好。
```

应用层的办法更常见：

```text
重试加指数退避和 jitter。
缓存失效加 singleflight 或 stale-while-revalidate。
服务恢复后限速重连，避免所有客户端同一秒握手。
队列消费者用 lease、visibility timeout 或分片，别让所有消费者抢同一个任务。
```

面试时要注意不要把惊群说成“线程被唤醒就是坏事”。唤醒多个等待者有时是合理的，比如多个任务真的可以并行处理。惊群的关键在于“唤醒数远大于可处理资源数”。如果来了 100 个连接，唤醒 100 个 worker 不一定是惊群；来了 1 个连接，唤醒 100 个 worker 才是。

结合 LogServe，worker 从 control 拉任务、control 重启后 worker 重连、checkpoint cache miss 后多个任务同时拉同一个模型文件，都可能出现应用层惊群。这里不一定要改内核参数，更实用的是 jitter、singleflight、限速、租约和明确的 backpressure。

面试里可以这样回答：

```text
惊群是一个事件只需要少数执行者处理，却唤醒了大量等待者，导致无效调度和竞争。经典场景是多个进程同时等 accept，来了一个连接却唤醒很多进程。内核层可以用 EPOLLEXCLUSIVE、SO_REUSEPORT、合理的事件循环减轻；应用层要用 jitter、singleflight、限速重连、lease 等办法。判断是不是惊群，看唤醒者数量是否远大于实际可处理资源。
```

## Q021. socket backlog 满了会发生什么？

**回答：**

这个问题不能只答“连接会被拒绝”。在 Linux 上，TCP listener 至少涉及两个队列：半连接队列和已完成连接队列。不同队列满了，现象不一样。

半连接队列保存的是已经收到 SYN、回了 SYN+ACK、还没收到最终 ACK 的连接，也就是常说的 SYN queue。它的上限和 `tcp_max_syn_backlog`、syncookies 等配置有关。如果 SYN queue 满，内核可能开始丢 SYN 或启用 syncookie 机制。客户端看到的通常是 connect 变慢，因为 SYN 或 SYN+ACK 要重传；严重时就是连接超时。

已完成连接队列保存的是三次握手已经完成、等待应用 `accept()` 的连接。`listen(fd, backlog)` 里的 backlog，在 Linux 2.2 之后主要指这个队列的长度。这个值还会被 `/proc/sys/net/core/somaxconn` 静默截断。也就是说，应用写 `listen(fd, 100000)` 不代表真的有 100000 个 accept backlog，内核上限可能更低。

当 accept queue 满了，服务端内核已经没地方放新完成的连接。Linux 的行为和配置有关。默认情况下，它可能忽略新连接握手的最后 ACK，让连接在服务端看来还没完成，之后重传 SYN+ACK。客户端这边可能已经认为 `connect()` 成功了，发第一批数据却迟迟没有响应。这个现象很迷惑：客户端不是立刻 `ECONNREFUSED`，而是表现为连接建立慢、首包延迟高、请求超时。

如果开启 `tcp_abort_on_overflow`，内核在 listener 来不及 accept 时可能 reset 连接。这个参数听起来很直接，但 Linux 手册也提醒要慎用，因为它会伤害客户端体验。更好的优先级通常是：让应用更快 accept、增加合理 backlog、扩容、限流、优化 accept 后的处理路径，而不是先让内核主动 RST。

backlog 满时，线上现象可能有这些：

```text
客户端 connect 超时、connect 延迟上升，或偶发 connection reset。
服务端 CPU 不一定高，但 accept 速度跟不上连接到达速度。
ss -ltn 看到监听 socket 的 Recv-Q 接近 Send-Q 或 backlog 上限。
/proc/net/netstat 里的 ListenOverflows、ListenDrops 增长。
SYN_RECV 状态很多，说明半连接队列压力大。
请求 p99/p999 上升，但应用日志里未必有对应 handler 记录，因为请求还没被 accept。
```

要区分 backlog 满和“没有进程监听”。没有 listener 时，客户端通常很快拿到 connection refused。backlog 满时，listener 是存在的，只是队列装不下或应用 accept 太慢，所以更常见的是超时、重传、首包卡住、偶发 reset。

为什么应用会 accept 太慢？常见原因有几个：accept loop 和业务处理绑在同一个线程，accept 到连接后立刻做 TLS 握手或重活；进程被 GC、锁、CPU 抢占拖住；worker 数太少；文件描述符耗尽；上游重试风暴带来连接尖峰；TLS 握手成本过高；短连接太多，没有 keep-alive。

处理时不要只调大 backlog。调大 backlog 可以吸收短突发，但如果长期到达速率大于 accept 和处理速率，队列迟早还会满。更稳的处理顺序是：

```text
确认 ListenOverflows/ListenDrops、SYN_RECV、accept 延迟和 fd 限制。
把 accept 和业务处理解耦，accept 后尽快交给 worker 或事件循环。
启用 keep-alive 或连接复用，降低连接创建速率。
给入口加限流和连接数上限，避免重试风暴直接打 listener。
合理调整 somaxconn、tcp_max_syn_backlog、应用 listen backlog。
观察是否有 SYN flood，需要时再考虑 syncookies、防火墙或边缘防护。
```

结合 LogServe，如果 SDK 或 worker 在 control 重启后同时重连，control listener 的 backlog 可能先被打满。此时业务日志可能什么都没有，因为请求还没进入 gRPC handler。比较好的设计是 worker 重连加 jitter，control 入口有连接上限和 readiness，客户端 deadline 短一点并退避重试。否则一个短暂重启会被重连风暴放大。

面试里可以这样回答：

```text
backlog 满要分 SYN queue 和 accept queue。SYN queue 满会导致 SYN/SYN-ACK 重传、connect 变慢或超时；accept queue 满说明三次握手完成但应用来不及 accept，Linux 上通常表现为忽略最后 ACK、重传 SYN-ACK，或在 tcp_abort_on_overflow 下 reset。排查看 ListenOverflows、ListenDrops、SYN_RECV、ss 的监听队列和 accept 延迟。调大 backlog 只能吸收突发，根本还是 accept 速度、连接复用、限流和重试退避。
```
## Q022. 半开连接如何检测？

**回答：**

半开连接这个词要先定义清楚。很多人把两种情况都叫半开：一种是 TCP half-open，一端以为连接还在，另一端已经崩溃、断电、重启或状态丢失；另一种是 half-close，一端发了 FIN，只关闭自己的发送方向，另一端还可以继续发送。前者是故障检测问题，后者是 TCP 正常关闭语义。面试里最好先把这两层分开。

如果对端正常关闭，它会发 FIN。本端读到 FIN 后，`read()` 返回 0，Go 里通常表现为 `io.EOF`。如果本端用 epoll，可以看到可读事件，读到 0 才知道对端关闭；也可以关注 `EPOLLRDHUP`，它专门用于 stream socket 对端关闭或 shutdown 写方向的情况。注意 `EPOLLHUP` 只是提示 hang up，仍然要把 buffer 里已有数据读完，不能一看到 HUP 就直接丢数据。

如果对端异常关闭并发送 RST，本端读写会得到 connection reset 之类错误。比如你向已经 reset 的连接写数据，可能得到 `ECONNRESET` 或 `EPIPE`。这类情况比较容易发现，因为内核有明确错误。

真正麻烦的是静默故障。比如对端机器断电、中间 NAT 表被清掉、防火墙丢包、网络单向中断。此时本端内核可能还认为连接是 ESTABLISHED。只要你不读不写，TCP 没有理由立刻发现对端已经没了。所谓“检测半开连接”，核心就是要主动制造可观测事件：

```text
应用层 heartbeat 或 ping/pong。
请求级 deadline 和读写超时。
TCP keepalive。
协议层 keepalive，例如 HTTP/2 PING、gRPC keepalive。
业务层 lease 或 session timeout。
```

TCP keepalive 是兜底，不适合当快速故障检测。Linux 默认 `tcp_keepalive_time` 是 7200 秒，也就是连接空闲两小时后才开始探测；之后还要按间隔发多次 probe。对线上 RPC 来说，这太慢了。可以调 socket 级别的 keepalive 参数，但也不能无限激进，因为中间设备、移动网络、云 LB 可能对频繁探测不友好。

应用层 heartbeat 更可控。比如每 10 秒发一次 ping，连续 3 次没有 pong 就认为连接不可用。它能检测 TCP 还能收发但业务线程卡死的情况，这是 TCP keepalive 看不到的。TCP keepalive 只证明内核 TCP 栈还会 ACK，不证明对端应用还能处理请求。

读写 deadline 也很重要。不要写这种逻辑：

```text
读不到就一直等。
写不出去就一直等。
没有请求就永远相信连接还活着。
```

Go 的 `net.Conn.SetReadDeadline`、`SetWriteDeadline`、`SetDeadline` 都是绝对时间点，超过后当前和后续 I/O 会失败，直到你设置新的 deadline。长连接如果要持续使用，通常每次读写前或每轮心跳后刷新 deadline。gRPC 则应该给 RPC 设置 deadline，stream 也要有心跳或上层取消机制。

还要避免一个误区：没有通用、无副作用的 `isConnected()`。你问内核 socket 当前是不是 ESTABLISHED，只能得到本端最后知道的状态。远端是否已经死掉，必须通过读、写、keepalive 或应用协议交互来发现。很多 Java/Go/C++ 代码里所谓连接健康检查，如果不发送任何数据，只是在看本地状态，价值很有限。

结合 LogServe，worker 和 control 的长连接如果半开，最坏的情况是 control 以为 worker 还在，worker 其实已经失联；或者 worker 以为任务还在执行，control 已经重启并 redeliver。这里需要多层检测：RPC deadline 防止一次调用无限挂住，worker heartbeat 或 lease 防止 worker 静默失联，epoch/fencing 防止旧 worker 在恢复后继续提交过期结果。TCP keepalive 只能作为底层兜底。

面试里可以这样回答：

```text
半开连接不能靠本地 isConnected 判断。正常 FIN 可以通过 read 返回 EOF 或 EPOLLRDHUP 发现，RST 会在读写时报错；静默故障只能靠主动探测和超时发现，包括应用 heartbeat、协议 keepalive、TCP keepalive、读写 deadline 和请求 deadline。TCP keepalive 默认很慢，只能兜底；业务系统要用应用层心跳、lease、deadline 和重试退避来缩短检测窗口。
```

## Q023. 负载均衡四层和七层有什么区别？

**回答：**

四层负载均衡和七层负载均衡的区别，不是“一个高级一个低级”，而是它们看见的信息不同、能做的决策不同、承担的协议责任也不同。

四层负载均衡工作在传输层附近，主要看 IP、端口、TCP/UDP 连接、SNI 这类有限信息。它通常不解析完整 HTTP 请求，不理解 URL、Header、Cookie、gRPC method。它做的是把一个连接转发到某个后端：

```text
client TCP connection -> LB -> backend TCP connection
```

七层负载均衡会解析应用层协议，比如 HTTP/1.1、HTTP/2、gRPC。它可以按 host、path、method、header、cookie、JWT、content-type、gRPC service/method 等信息路由，也可以做 TLS 终止、认证、限流、压缩、header 改写、重试、熔断、灰度发布和可观测性采集。

四层的优点是轻。它不需要理解业务协议，适合 TCP、UDP、数据库、自定义二进制协议，也更容易保持端到端 TLS。缺点是决策粒度粗。它一般按连接分发，不按请求分发。如果一个客户端建了一条 HTTP/2 长连接，里面跑了几千个 stream，四层 LB 看到的仍然主要是一条连接，这些 stream 往往都会落到同一个后端。

七层的优点是细。它可以按请求或 stream 做路由。比如：

```text
/api/v1/tasks       -> task-service
/api/v1/workflows   -> workflow-service
Header x-tenant=a   -> tenant-a backend
gRPC /LogService/Append -> logd shard group
```

代价是它必须终止或至少理解应用协议。终止 TLS 后，代理要负责证书、mTLS、真实客户端 IP、header 信任边界、请求体大小、流控、超时、重试语义。七层代理如果错误重试非幂等请求，可能制造重复写；如果超时设置不合理，可能在后端仍在执行时提前返回，再由客户端重试，造成二次提交。

健康检查也不同。四层健康检查通常是 TCP connect 成功、端口可达，最多再做一点协议探测。七层健康检查可以访问 `/readyz`、检查 HTTP status、验证依赖是否可用。TCP connect 成功只能说明进程在监听，不代表业务 ready。反过来，七层健康检查更准确，但也更可能因为应用依赖抖动把实例摘掉。

真实客户端 IP 也是差异之一。四层如果做直接转发或保留源地址，后端可能看到真实 client IP；如果是 TCP proxy，后端看到的也可能是代理 IP，需要 PROXY protocol。七层反向代理通常会把自己的连接作为下游连接，后端看到的是代理地址，真实 IP 要靠 `Forwarded`、`X-Forwarded-For` 或受信任的代理协议传递。

TLS 场景要讲清楚：

```text
L4 passthrough: LB 不解密，后端负责 TLS，LB 看不到 path/header。
L4 with SNI routing: LB 可根据 TLS ClientHello 里的 SNI 做有限路由，但仍看不到 HTTP 内容。
L7 TLS termination: LB 终止 TLS，能看 HTTP/gRPC，再向后端明文或重新 TLS。
```

结合 LogServe，如果只是把 SDK 到 control 的 gRPC 连接分发到多个 control 实例，四层可能就够了。但如果要按租户、workflow 类型、gRPC method、模型名称做灰度或限流，就需要七层代理或应用网关。七层更强，但它会进入 LogServe 的语义边界：deadline 传播、幂等 key、streaming RPC、重试和真实客户端身份都要设计清楚。

面试里可以这样回答：

```text
四层负载均衡主要按连接和 IP/端口信息转发，协议无关、开销低，但决策粒度通常是连接；七层负载均衡解析 HTTP/gRPC 等应用协议，可以按 host/path/header/method 路由，也能做 TLS 终止、认证、限流、重试和观测。代价是七层代理要承担协议语义，尤其是重试、超时、真实客户端 IP 和 TLS 信任边界。HTTP/2/gRPC 下还要注意四层按连接分发可能导致很多 stream 黏在同一个后端。
```
## Q024. 反向代理会如何影响真实客户端 IP？

**回答：**

反向代理一上来，后端应用看到的 `remote_addr` 往往就不再是客户端 IP，而是代理的 IP。原因很简单：后端收到的是代理重新发起的连接。TCP peer 是代理，不是原始客户端。很多线上日志里突然所有请求都来自 `10.0.0.12`，就是因为这个地址是 Nginx、Envoy、Ingress 或 LB。

为了把原始客户端地址传下去，代理通常会写这些信息：

```text
Forwarded: for=203.0.113.10;proto=https;host=api.example.com
X-Forwarded-For: 203.0.113.10, 10.0.0.5, 10.0.0.12
X-Real-IP: 203.0.113.10
PROXY protocol: 在 TCP 连接开头传递源/目标地址信息
```

`Forwarded` 是 RFC 7239 定义的标准头，`X-Forwarded-For` 是事实标准，历史更长，用得更广。`X-Forwarded-For` 通常是一个列表，每经过一个代理追加一个地址。最左边经常是原始客户端，越往右越接近当前服务。但这只是约定，不是安全保证。

关键问题是：这些 header 可以被客户端伪造。如果边缘代理不清洗，恶意客户端可以直接发：

```http
X-Forwarded-For: 1.2.3.4
```

后端如果无条件相信第一个 IP，就会把攻击者当成 `1.2.3.4`。这会影响审计、限流、风控、地域策略，甚至访问控制。真实 IP 处理的核心不是“取哪个 header”，而是“信任哪些代理写入的哪一段 header”。

比较稳的策略是：

```text
边缘入口删除客户端传入的 Forwarded/X-Forwarded-*，重新生成可信 header。
内部代理只追加，不随意覆盖已有可信链路。
应用只信任来自已知代理 CIDR 的请求里的转发头。
按 trusted hops 或 trusted CIDRs 从右向左解析 XFF。
日志同时记录 socket remote_addr 和解析后的 trusted client IP。
```

Envoy 文档里对这个问题讲得很直白：直接下游连接的源地址一定是已知的，XFF 只有在由可信代理写入时才可信。它提供 `use_remote_address`、`xff_num_trusted_hops`、trusted CIDR 这类配置，就是为了避免应用自己乱猜。

多层代理时，例子更清楚：

```text
Client 203.0.113.10
  -> CDN 198.51.100.1
  -> Edge Envoy 10.0.0.5
  -> Service Proxy 10.0.0.12
  -> App

X-Forwarded-For: 203.0.113.10, 198.51.100.1, 10.0.0.5
App remote_addr: 10.0.0.12
```

如果 App 信任 `Service Proxy` 和 `Edge Envoy`，但不直接信任客户端输入，它应该从可信链路中推出 `203.0.113.10` 才是原始客户端。如果前面还有不受控代理，或者 CDN 没有被纳入可信 CIDR，就不能盲目取最左边。

四层代理还有另一套办法，叫 PROXY protocol。它不靠 HTTP header，而是在 TCP payload 最前面加一段代理协议头，告诉后端原始源地址和目标地址。它适合 TLS passthrough、非 HTTP 协议、数据库代理等场景。代价是后端 listener 必须显式支持 PROXY protocol，否则会把这段前缀当成业务协议数据，直接解析失败。

还有一种方式是透明代理或直接服务器返回，让后端保留真实源 IP。但这通常要求网络路由、conntrack、回包路径都配合，部署复杂度比普通反向代理高。应用层大多数时候还是通过可信代理头或 PROXY protocol 解决。

结合 LogServe，如果以后有 gateway 或 Ingress，日志里不能只记 `RemoteAddr`。SDK 用户、worker 节点、控制台请求最好区分“直连 peer IP”和“可信客户端 IP”。鉴权也不要直接信任 `X-Forwarded-For`，只能信任入口代理清洗和追加后的结果。否则本地实验没问题，上云后日志、限流和审计都会偏。

面试里可以这样回答：

```text
反向代理会重新发起到后端的连接，所以后端 socket 看到的 peer IP 通常是代理 IP。真实客户端 IP 需要通过 Forwarded、X-Forwarded-For、X-Real-IP 或 PROXY protocol 传递。问题在于这些 header 可以伪造，应用只能信任来自受控代理链的字段，通常要在边缘清洗、内部追加，并按 trusted hops 或 trusted CIDRs 解析。日志最好同时保留 remote_addr 和 trusted client IP。
```

## Q025. 超时应该分为连接超时、读超时、写超时、整体 deadline 吗？

**回答：**

应该分，而且必须分。一个网络调用可能卡在很多地方：DNS、TCP connect、TLS 握手、写请求、等响应头、读响应体、应用处理、下游调用。只设一个“timeout”看起来简单，排障时却会把所有问题混成一句“请求超时”。只设连接超时更糟，连接成功后读写可以无限挂住。

先看连接超时。它控制的是建立连接阶段，包括拨号路径上的等待。它解决不了连接建立之后的问题。服务端接收了 TCP 连接，但 TLS 握手卡住、HTTP 响应头不回来、响应体慢慢滴，这些都不归 connect timeout 管。很多线上事故就是 `DialTimeout` 写了 200ms，于是大家以为有超时保护；实际请求已经连上，然后在读响应上挂了几分钟。

读超时控制的是从连接读取数据的等待。它可以拆得更细：读响应头超时、读 body 超时、两次 message 之间的 idle 超时。HTTP 服务端也常分 `ReadHeaderTimeout` 和 `ReadTimeout`，因为慢速发送 header 和大 body 上传是两类风险。长连接或 streaming 场景不能简单设一个固定 read timeout 后不刷新，否则正常长 stream 也会被切断；更常见的做法是每次收到消息或 heartbeat 后刷新读 deadline。

写超时控制的是把数据写到连接的等待。写也会卡住：对端不读、内核 send buffer 满、网络拥塞、TLS 层阻塞。Go 的 `SetWriteDeadline` 文档还提醒，即使 write 超时，也可能已经写出了一部分数据。这对协议设计很重要：写超时后你不能简单假设请求一定没发出去。对非幂等请求，后续重试必须靠 idempotency key、request id 或业务去重。

整体 deadline 是最容易被忽略的。它限制的是一次业务调用从开始到结束的总预算。没有整体 deadline，阶段超时和重试可能叠加出很长的尾延迟：

```text
connect 200ms
TLS 300ms
response header 500ms
body read 2s
retry 3 次
```

每个阶段都“没超太久”，最后用户等了好几秒。整体 deadline 能把这件事压住。gRPC 官方文档也强调客户端应该显式设置 deadline；默认没有 deadline 时，客户端可能一直等下去。Go 里通常用 `context.WithTimeout` 或 `context.WithDeadline` 表达整体预算，再让 HTTP/gRPC/数据库调用沿着 context 传播。

一个比较完整的 HTTP 客户端超时模型会像这样：

```text
DNS/Connect timeout
TLS handshake timeout
request write timeout
response header timeout
body read timeout 或 idle read timeout
idle connection timeout
whole request deadline
```

服务端也要分：

```text
ReadHeaderTimeout: 防慢请求头。
ReadTimeout: 限制读完整请求的时间。
WriteTimeout: 限制写响应的时间。
IdleTimeout: keep-alive 空闲连接最多保留多久。
handler context deadline: 业务处理最多花多久。
```

这些超时不是越短越好。超时太短会制造误杀，尤其是跨 AZ、冷启动、GC、TLS 握手、慢客户端、大响应体。超时太长又会拖住 goroutine、连接池和下游资源。更靠谱的做法是按 SLO、p99/p999、重试次数、下游容量来定，并在指标里把不同阶段的超时分开打点。`connect_timeout`、`tls_timeout`、`read_header_timeout`、`deadline_exceeded` 的处理方向完全不同。

还有一个细节：deadline 是绝对时间点，不是每次 I/O 自动重新计时。Go `net.Conn.SetDeadline` 设置后，会影响未来和已经阻塞的 I/O；超时后如果要继续使用连接，需要设置新的 deadline。很多自定义协议 bug 就出在这里：设置一次 deadline 后复用连接，后面的请求莫名其妙立刻超时。

结合 LogServe，timeout 分层尤其重要。workflow step 有业务 deadline，RPC 有调用 deadline，worker heartbeat 有检测窗口，log append 有写 deadline，long-poll 有单次 poll timeout。不能把它们揉成一个全局 timeout。比如任务执行可能允许 30 秒，但 control 写 logd 不应该卡 30 秒；worker long-poll 可以 20 秒正常返回空，但 heartbeat 失联可能要按连续几次失败判断。超时后是否重试，还要看操作是否幂等，LogServe 里就是 `workflow_id + step_id + input_hash`、idempotency key、epoch/fencing 这些语义。

面试里可以这样回答：

```text
超时应该分层。连接超时只管建连，读超时管等数据，写超时管发送阻塞，TLS/响应头/idle timeout 也常单独设置；整体 deadline 限制一次业务调用的总预算，并向下游传播。分层的价值是既防止无限等待，又能定位到底卡在 connect、TLS、写、读 header、读 body 还是业务处理。重试时必须尊重整体 deadline，写超时后也不能假设请求一定没到达服务端，非幂等操作要靠幂等键保护。
```

## Q026. 网络抖动如何影响分布式 lease？

**回答：**

网络抖动影响 lease 的核心不是“平均延迟变大”，而是尾部延迟变得不可预测。lease 通常靠 TTL 和 keepalive 判断持有者是否还活着。以 etcd 为例，lease 有服务端授予的 TTL；如果集群在 TTL 内没有收到 keepalive，lease 就会过期，挂在这个 lease 上的 key 会被删除。etcd 的 lock 和 election 也把所有权或 leadership 绑定到 lease 上，lease 过期后锁会释放，leader 会让位给后面的候选者。

这套机制在网络稳定时很清楚：持有者定期续约，服务端按 TTL 兜底。抖动一来，问题会变细。某一次 keepalive 可能只是晚到了 300ms，业务节点还活着，CPU 也没问题，但 lease 服务端已经看不到它；另一边可能已经拿到新 lease。老持有者如果只看自己的本地状态，还以为自己仍然有权写共享资源，新的持有者也开始工作，结果就是双主或旧主写入。

所以 lease 的危险点有两个：

```text
误判死亡：节点没死，只是 keepalive 因网络、GC、调度、磁盘抖动迟到。
旧主残留：lease 已经过期或换主，但旧节点没有及时知道，继续执行副作用。
```

第一个问题影响可用性，第二个问题影响正确性。面试里要把这两件事分开讲。只把 TTL 调大可以减少误判，但会拉长真实故障的恢复时间；只把 TTL 调小可以更快发现故障，但会在抖动时制造大量误切。真正能保护正确性的不是“把 lease 时间设准”，而是 fencing token，也就是每次获得 lease 或 leadership 时拿到一个单调递增的版本号、epoch、revision。所有写共享状态的操作都必须携带这个 token，下游只接受最新 token。

一个可靠的 lease 设计通常会这样做：

```text
heartbeat_interval < keepalive_deadline < lease_ttl
lease_ttl 覆盖 p99/p999 RTT、重试、进程调度、GC、磁盘 fsync、leader 切换时间
续约失败后持有者主动降级，不等本地 TTL “自然到期”
所有外部副作用携带 epoch / fencing token
服务端或资源端按 epoch 拒绝旧 owner
```

这里不能只看平均 RTT。平均 RTT 20ms、偶发 2s 抖动的网络，比稳定 80ms 的网络更难配 lease。因为 lease 误判通常发生在尾部。生产系统更关心这些指标：

```text
keepalive RTT p99/p999
连续 keepalive 失败次数
lease renew 剩余 TTL
GC pause / scheduler delay
服务端 apply 延迟
leader election 次数
fencing reject 次数
```

还要避免把“故障检测”和“所有权证明”混成一件事。heartbeat 可以告诉控制面“这个 worker 可能不健康”，但 worker 能不能继续写 actor 状态，应该由 epoch/fencing 决定。否则网络抖动时很容易出现这样的事故：控制面认为 worker A 失联，把 actor 迁给 worker B；A 过了几秒恢复网络，继续把旧 actor 命令写回去。如果 log 或状态存储不校验 epoch，数据就乱了。

结合 LogServe，当前项目是单机多进程的机制验证，不应该把它描述成已经解决了跨机 lease 的所有边界。但已有的 actor ownership 和 epoch fencing 是正确方向：worker 心跳失联后可以触发重分配，真正写入 actor 完成事件时还要检查 `owner_worker_id + epoch`。如果以后扩展到多机，network jitter 会直接影响 worker lease、long-poll、control 到 logd 的 append 超时，以及任务 redelivery。应对方式不是盲目缩短心跳，而是把心跳窗口、任务 deadline、log append timeout、lease TTL 分开，再让每个会产生副作用的写入带 fencing 信息。

面试里可以这样回答：

```text
网络抖动会让 lease 续约延迟变大，导致活节点被误判死亡；更严重的是旧 owner 可能在 lease 已失效后继续写共享资源。TTL 只能在误判率和故障恢复速度之间取舍，不能单独保证正确性。工程上要用足够覆盖 p99/p999 抖动的 TTL、提前续约、连续失败再判定，并用单调递增的 epoch 或 fencing token 保护所有副作用。旧 owner 即使恢复，也会因为 token 过期被资源端拒绝。
```

## Q027. 跨地域 RTT 对用户体验和一致性协议有什么影响？

**回答：**

跨地域 RTT 首先影响用户体验，因为它给每一次串行网络交互加了物理下限。用户在新加坡，请求打到美国东部；一次 HTTP 请求、一次数据库写、一次鉴权 RPC、一次缓存 miss，每一段都要付 RTT。一个页面如果串行调用 5 个后端服务，单次跨洋 RTT 就会被放大成肉眼可见的延迟。前端看到的是“页面慢”，后端看到的往往是很多小 RPC 都不算慢，但链路加起来很慢。

它对一致性协议的影响更硬。Raft 论文里说，常见情况下一个命令在多数派响应后就可以完成；这意味着写入延迟至少要受多数派里较慢副本的网络距离影响。Raft 的 leader election 还依赖 `broadcastTime << electionTimeout << MTBF` 这种时间关系。跨地域 RTT 增大后，heartbeat 和 election timeout 都要放大；否则只是网络慢一点，就会频繁选主。etcd 官方调优文档也给了很直接的经验：election timeout 至少要是 RTT 的 10 倍，用来覆盖网络方差；全球部署时 election timeout 可能需要到几十秒量级。

这带来一个很实际的结论：强一致写入跨地域复制时，低延迟和大范围容灾不能同时免费得到。你可以把多数派副本放在多个地域，提高区域故障容忍；代价是写 quorum 通信要跨地域。Google Cloud Spanner 的多地域配置文档也明确提到，多地域读写副本分布在多个 region 后，写 quorum 通信会引入额外网络延迟；读可以在更多地方更快，但写延迟会增加。Spanner 还用 TrueTime 提供外部一致性，这类设计说明了另一件事：全局一致性要么靠通信，要么靠有界时钟不确定性和等待，最后都要付延迟预算。

对用户体验，可以按这几类处理：

```text
读多写少：把只读副本、缓存、CDN、边缘计算放近用户。
写路径：把用户写请求路由到 leader 或主写地域，减少无意义绕路。
弱一致可接受：用本地写入、异步复制、会话一致性或最终一致性。
强一致必须要：明确告诉业务方跨地域写的 p95/p99 会更高。
交互链路：减少串行 RPC，批量化、并行化、预取，避免一次页面跨地域来回多次。
```

对一致性协议，要看 quorum 怎么放。如果 5 个副本横跨 3 个大洲，多数派可能每次都要跨洲；如果把 3 个 voting 副本放在同一大陆、远端只放 read-only replica，写延迟会低一些，但远端灾备语义不同。没有一种拓扑同时满足“任意大洲故障仍然强一致可写”和“所有用户写入都像本地写一样快”。面试时直接承认这个取舍，比泛泛说“优化网络”更可信。

还有一个容易漏掉的点：跨地域 RTT 会改变 timeout、lease 和重试策略。单地域 100ms 的 RPC deadline，跨地域可能还没完成 TLS 或首包；单地域 1s 的 lease TTL，跨地域抖动时可能频繁误判。重试如果不带 jitter，会把跨地域链路打出同步洪峰。跨地域系统里，deadline 要按端到端预算传递，重试次数要少，退避要有随机化，还要区分用户请求和后台复制流量。

结合 LogServe，如果以后从单机机制验证扩展到多节点，shared log 的写路径会成为核心延迟点。只要 log append 要等跨地域 quorum，workflow step 状态推进、actor command commit、LLM 任务完成事件都会被 RTT 影响。更现实的演进方式是先做单地域多节点，再考虑异地只读副本、异步灾备或按租户/任务做地域归属。把所有 worker 混在一个全球 lease 池里，TTL 和调度都会很难调。

面试里可以这样回答：

```text
跨地域 RTT 会给用户请求增加物理延迟，尤其是串行 RPC 会把一次 RTT 放大成多次等待。对一致性协议，强一致写通常要等 leader 或多数派副本响应，所以 quorum 跨地域会抬高写延迟；选主和 lease timeout 也要按更大的 RTT 和抖动重新设置。工程上通常把读放近用户，把写路由到 leader 地域，能接受陈旧数据的路径用缓存或只读副本，必须强一致的路径则明确承担跨地域 quorum 的延迟。
```

## Q028. 如何排查偶发 connection reset？

**回答：**

`connection reset` 的意思是连接收到了 TCP RST，或者本机在读写时发现对端已经用 reset 方式终止了连接。它不是一个单一故障。应用主动 abort、进程重启、代理 idle timeout、负载均衡踢连接、协议错误、backlog 溢出、NAT 状态消失、防火墙注入 RST，都可能表现成同一句 `read: connection reset by peer`。

排查第一步是确认 reset 发生在调用的哪个阶段：

```text
connect 阶段：SYN 后收到 RST，常见于端口未监听、负载均衡拒绝、后端快速失败。
TLS 握手阶段：可能是证书/SNI/协议版本不匹配，也可能是代理直接断开。
请求写入阶段：对端已经关了连接，本端继续写，内核返回 reset 或 broken pipe。
响应头阶段：服务端接收请求后还没回 header 就 abort。
响应体阶段：大响应、长连接、streaming 中途被代理或服务端断开。
连接复用阶段：客户端复用了一个对端或代理早已清理的 idle connection。
```

阶段不同，方向完全不同。偶发 reset 如果集中在复用连接的第一笔请求上，优先查 idle timeout 不匹配：客户端连接池保留 90s，负载均衡 60s 清 idle，客户端第 70s 拿出来复用，就可能第一读写失败。修法通常是把客户端 idle timeout 设得比代理更短，或者启用更可靠的 keepalive/健康探测。不要一上来就怀疑业务处理。

第二步是抓包确认谁发了 RST。服务端日志说“我没报错”没有决定性；RST 可能来自服务端内核、sidecar、四层负载均衡、防火墙或 NAT。抓包时至少看这些字段：

```text
RST 包的源 IP、源端口、TTL
RST 前最后一个正常包是谁发的
是否伴随重传、零窗口、FIN、TLS alert
连接是否刚好超过某个 idle timeout
同一时间服务端是否重启、发布、OOM、FD 用尽、accept 队列溢出
```

常用命令可以是：

```text
tcpdump -nn -i any 'tcp[tcpflags] & tcp-rst != 0'
ss -tanpi
ss -s
netstat -s | grep -i reset
dmesg / journalctl / 容器重启事件
```

如果有代理，代理日志要一起看。Nginx、Envoy、HAProxy、云 LB 往往能告诉你 upstream reset、downstream reset、idle timeout、max connection、request body too large、TLS handshake failure。很多“偶发”其实不是随机，而是只发生在某一类路径：某个 upstream 实例、某个 AZ、某个大请求、某个客户端版本、某个连接空闲时长。

第三步是看负载和内核队列。服务端压力大时，reset 可能来自 accept 处理太慢、backlog 满、应用主动关闭连接、进程崩溃重启。Linux `connect(2)` 文档也提醒，连接超时在服务器太忙或 syncookies 场景下可能很长；而 backlog 满时，如果配置了 `tcp_abort_on_overflow`，可能直接 reset。此时要看 `ListenOverflows`、`ListenDrops`、`SYN_RECV`、`accept` 速率、线程池/协程池、FD 数、CPU steal、GC pause。

第四步是把应用错误和请求上下文打全。只记录 `connection reset by peer` 没什么价值。更有用的是：

```text
remote_addr / local_addr
是否复用连接
连接空闲多久
请求方法、路径、body 大小
错误发生阶段
重试次数
上游实例 ID
trace_id / request_id
```

在 Go 里，很多 reset 会包在 `net.OpError` 里，底层可能是 `ECONNRESET`。HTTP 客户端侧可以用 `httptrace` 拆 DNS、connect、TLS、got connection、wrote request、got first response byte。服务端侧要记录 handler 是否已经开始处理、是否写了部分响应、是否 panic、是否主动 `Close`。

结合 LogServe，容易出现 reset 的地方主要是 SDK 到 control、worker long-poll、control 到 logd、dashboard API。如果 reset 集中在 worker long-poll，先查服务端是否把长轮询当普通短请求设置了过短 write timeout；如果集中在 control 到 logd，查 logd 是否重启、append backlog 是否堆积、连接池是否复用 stale connection。任务语义上还要注意：reset 后不能断言请求一定没有到达服务端。对 `TaskSubmitted`、`ActorCommandSubmitted` 这类写操作，仍然要靠 idempotency key 和 log-first 去重。

面试里可以这样回答：

```text
排查 connection reset 先按阶段切开：connect、TLS、写请求、等响应头、读 body、复用 idle connection。然后抓包看 RST 是谁发的，再对齐服务端、代理、LB、内核队列和发布重启日志。偶发 reset 常见原因是 idle timeout 不匹配、服务端重启、backlog/accept 压力、代理超时、协议错误或 NAT 状态丢失。对写请求，reset 不代表服务端一定没处理，重试必须靠幂等键或 request id 保护。
```

## Q029. 如何排查端口耗尽？

**回答：**

端口耗尽通常指客户端出站连接没有可用的临时端口了。Linux 在未显式 bind 本地端口时，会从 `ip_local_port_range` 里自动挑一个本地端口。`connect(2)` 文档明确写到，如果所有 ephemeral port 都在用，可能返回 `EADDRNOTAVAIL`。应用层看到的可能是 `cannot assign requested address`、`connect: address not available`，也可能被封装成拨号失败、连接超时、上游不可达。

先算清楚端口是不是理论上会不够。TCP 连接由五元组区分：

```text
source IP, source port, destination IP, destination port, protocol
```

如果一个进程从同一个源 IP 高速打同一个 `dstIP:dstPort`，可用并发连接数大致受源端口范围限制。Linux 默认端口范围常见是几万级，不是无限。连接关闭后还会进入 TIME_WAIT，一段时间内不能随便复用。高 QPS 短连接、没有 keep-alive、连接池最大连接数过大、重试风暴，都会把端口烧完。

排查时我会先看错误和端口范围：

```text
cat /proc/sys/net/ipv4/ip_local_port_range
grep -i 'EADDRNOTAVAIL\|cannot assign requested address' app.log
ss -s
ss -tan state time-wait | wc -l
ss -tan state syn-sent | wc -l
ss -tan state established | wc -l
```

然后按目的地址聚合。端口耗尽通常不是“全网都不够”，而是某个源 IP 到某个上游地址的连接太多：

```text
大量连接集中到同一个数据库 / Redis / API 网关
大量 TIME_WAIT 指向同一个 dstIP:dstPort
大量 SYN_SENT 指向同一个不可达上游
大量短连接来自同一个客户端进程或同一个 NAT 网关
```

容器和云上还要看 SNAT。很多 Pod 看起来有不同 Pod IP，出集群或出 VPC 时可能被同一个节点 IP、NAT 网关 IP 或 egress gateway 汇聚。此时端口限制发生在 NAT 设备上，不一定发生在应用容器里。现象是应用只看到 connect timeout/reset，节点或 NAT 指标显示 SNAT port allocation failed、conntrack table full、active connections 飙升。Kubernetes 环境还要看 node-local 连接、Service 转发、conntrack 项数。

常见根因有这些：

```text
没有复用连接：HTTP keep-alive 被禁用，gRPC channel 每次新建。
连接池配置错误：MaxConns 过大，或者请求结束没有关闭 body，连接不能回池。
短连接风暴：重试没有退避，失败时所有实例一起重连。
上游变慢：连接占用时间变长，Little's Law 下并发连接数上升。
DNS/LB 变化：大量连接集中到少数后端 IP。
NAT 汇聚：多个实例共享少量源 IP。
TIME_WAIT 堆积：主动关闭方短时间创建大量连接。
```

修复方向要先从减少连接创建开始，而不是立刻调内核参数。最有效的通常是：

```text
开启 HTTP keep-alive / HTTP/2 / gRPC 长连接。
复用客户端和连接池，不要每个请求新建 client。
限制每个上游的最大并发连接数，让排队发生在应用层。
设置合理 idle timeout，避免频繁建连又避免 stale connection。
失败重试加指数退避和 jitter。
增加源 IP 或 NAT IP，分散 SNAT 端口压力。
必要时扩大 ip_local_port_range。
```

`tcp_tw_reuse` 这类参数要谨慎。Linux 内核文档说它是在协议安全时复用 TIME_WAIT socket，并明确提示不要在没有专家建议时随便改。它可能缓解某些场景，但不是“端口耗尽万能开关”。如果根因是没有连接复用或 NAT 端口太少，乱调 TIME_WAIT 参数只是把问题推迟。

结合 LogServe，SDK、control、worker、logd 之间应尽量复用长连接。worker poll 不应该在空队列时高速重连；control 写 logd 也不应该每条 log append 都新建 TCP。出现端口耗尽时，还要看任务失败后的重试是否造成同步重连。如果所有 worker 在 control 重启后一秒内同时重连，端口、accept queue 和连接池都会被打满。

面试里可以这样回答：

```text
端口耗尽先看应用是否出现 EADDRNOTAVAIL 或 cannot assign requested address，再查 ip_local_port_range、TIME_WAIT、SYN_SENT、ESTABLISHED，并按目的地址聚合连接数。根因通常是短连接过多、连接没有回池、上游变慢导致连接占用时间变长、重试风暴，或者 NAT/SNAT 把很多实例汇聚到少数源 IP。优先修连接复用、连接池上限、退避和 NAT 容量，最后才考虑扩大端口范围或调整 TIME_WAIT 相关内核参数。
```

## Q030. 如何排查 DNS 解析慢？

**回答：**

DNS 慢要按解析链路拆。应用并不是直接问权威 DNS，通常是：

```text
应用 DNS API
  -> libc / Go resolver / JVM resolver
  -> /etc/resolv.conf 里的 nameserver、search、ndots、timeout、attempts
  -> 节点本地 DNS cache 或 systemd-resolved
  -> CoreDNS / kube-dns / VPC resolver
  -> 上游递归 DNS
  -> 权威 DNS
```

任何一层慢，应用看到的都是“DNS lookup 慢”。所以第一步是在出问题的同一台机器、同一个容器、同一个网络命名空间里测。不要只在自己电脑上 `dig`。

```text
time getent hosts example.com
dig example.com A +stats
dig example.com AAAA +stats
dig @<nameserver> example.com A +stats
dig +trace example.com
```

如果在 Kubernetes 里，就进 Pod 看 `/etc/resolv.conf`。Kubernetes 官方 DNS 调试文档也是从 dnsutils Pod、`nslookup`、`/etc/resolv.conf`、CoreDNS Pod 状态和日志开始查。`resolv.conf(5)` 里有几个配置特别容易制造慢查询：`search` 会让短名字按搜索域逐个尝试；`ndots` 会决定少于多少个点的名字先走 search；`timeout` 和 `attempts` 会决定一个 nameserver 不响应时要等多久、重试几次。一个外部域名如果被当成短名字反复拼接多个 search suffix，慢几百毫秒甚至几秒并不奇怪。

典型例子：

```text
options ndots:5
search ns.svc.cluster.local svc.cluster.local cluster.local corp.example.com

访问 api.github.com
```

如果名字没有写成 FQDN，解析器可能先尝试：

```text
api.github.com.ns.svc.cluster.local
api.github.com.svc.cluster.local
api.github.com.cluster.local
api.github.com.corp.example.com
api.github.com
```

中间任何一个 search 域超时，都会拉长总耗时。解决不一定是全局改 `ndots`，也可以让外部域名使用完整 FQDN，必要时在名字后加点，或者给特定客户端配置更合适的 resolver 行为。

第二步看是缓存问题还是上游问题。CoreDNS cache 插件会缓存成功和否定响应，默认会按 TTL 缓存，缓存命中能明显减少后端查询。forward 插件会把 DNS 请求转发到上游，支持 UDP、TCP、DNS-over-TLS、健康检查、连接复用、`max_concurrent` 等。CoreDNS 官方文档里还写了 forward 的默认 dial/read timeout，以及 `coredns_proxy_request_duration_seconds`、healthcheck failures、connection cache hits/misses、max concurrent rejects 等指标。排查时这些指标比“感觉 DNS 慢”有用得多。

我会重点看：

```text
coredns_dns_request_duration_seconds 或 coredns_proxy_request_duration_seconds
coredns_cache_hits_total / misses
coredns_cache_evictions_total
coredns_forward 或 proxy healthcheck failures
max_concurrent rejects
SERVFAIL / NXDOMAIN 比例
上游 resolver 的 p95/p99
```

第三步分 A 和 AAAA。很多应用会同时查 IPv4 和 IPv6；如果 IPv6 路径配置半残，AAAA 查询或后续连接会拖慢。Go 的 `net.DNSError.Timeout()` 文档还提醒，DNS lookup 因超时失败时不一定总能准确标成 timeout。Go 服务可以临时用 `GODEBUG=netdns=go+1` 或 `GODEBUG=netdns=cgo+1` 确认走的是 Go resolver 还是 cgo resolver，再用 `httptrace` 的 DNSStart/DNSDone 把 DNS 阶段单独打点。

第四步抓包。DNS 是 UDP 为主，但响应过大、TC bit、DNS-over-TLS、某些策略会走 TCP。抓包能看到是否真的发了查询、发给哪个 nameserver、是否重试、是超时还是 SERVFAIL：

```text
tcpdump -nn -i any 'port 53'
tcpdump -nn -i any 'tcp port 853'
```

如果抓包显示第一个 nameserver 没响应，glibc 会尝试下一个并按 attempts 重复；如果所有请求都很快返回 NXDOMAIN，慢可能在应用层 search 次数太多；如果 CoreDNS 到上游慢，则要查上游健康、网络 ACL、DNS 防火墙、DoT 握手、`max_concurrent` 和 cache 命中率。

修复方向通常是这些：

```text
对外部域名使用 FQDN，减少 search/ndots 带来的额外查询。
启用或调好 NodeLocal DNSCache / CoreDNS cache。
给 CoreDNS 配置多个健康上游，观察 forward 插件指标。
合理设置 timeout、attempts、rotate，不要让坏 nameserver 排第一且长期超时。
避免极低 TTL 造成缓存失效风暴。
对热点域名做预热或本地缓存，但要尊重 TTL 和故障切换需求。
把 DNS lookup latency 从 connect/request latency 中单独打点。
```

结合 LogServe，DNS 慢会表现为 SDK 连接 control 慢、worker 找 control/logd 慢、未来接入对象存储或模型服务时冷启动慢。排查时不要只看 RPC latency，要把 DNS、connect、TLS、request 分开。对于内部组件，实验环境可以先用稳定的本地地址或服务发现；如果上 Kubernetes，再专门看 Pod 的 `resolv.conf`、CoreDNS cache 命中和搜索域配置。

面试里可以这样回答：

```text
DNS 慢要从应用所在机器或 Pod 里查，先看 /etc/resolv.conf 的 nameserver、search、ndots、timeout、attempts，再用 dig/getent/nslookup 对比短名、FQDN、A/AAAA 和指定 nameserver。Kubernetes 里还要看 CoreDNS Pod、日志、cache/forward 指标和上游 resolver 延迟。常见根因是 search 域和 ndots 导致额外查询、首个 nameserver 超时、CoreDNS cache miss 或过载、上游 DNS 慢、AAAA/IPv6 路径异常。修复时要减少无谓查询、启用缓存、配置健康上游，并把 DNS latency 单独打点。
```

## Q031. TCP handshake 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

TCP handshake 的核心目标是建立一个双方都能确认的连接上下文。这个上下文至少包括四元组、双方初始序列号、初始接收窗口、MSS、窗口扩大、SACK、时间戳等 TCP option 能力，以及双方对“这个连接确实是新连接”的共同认识。RFC 9293 里说得很直接：三次握手的主要原因是防止旧的重复连接请求造成混乱。

所以它首先解决的是正确性问题。TCP 是字节流协议，后面的每个字节都靠序列号定位。如果双方没有先交换并确认初始序列号，接收端就无法区分这些数据到底属于当前连接，还是网络里滞留的旧连接残片。SYN 会消耗一个序列号，ACK 确认的是对端下一个期望收到的序列号；这一步不是仪式，而是后续可靠传输的起点。

它也有一点性能含义，但性能不是主目标。握手会在传输数据前增加一次 RTT，这是性能成本；握手期间顺便协商 MSS、窗口扩大、SACK、时间戳，这些选项能影响后续吞吐、重传和大带宽延迟积链路的表现。也就是说，handshake 不是为了性能而生，但它把很多性能能力的协商放在了连接建立阶段。

安全性方面，普通 TCP handshake 只提供很弱的保护。它能通过随机 ISN 降低 off-path 攻击者猜中序列号的概率，RFC 6528 专门讨论过 ISN 随机化；它不能认证对端身份，不能加密数据，也不能防止 on-path 攻击者篡改报文。身份认证和机密性要靠 TLS、mTLS、IPsec 或应用层鉴权。把 TCP 三次握手说成“保证安全连接”是面试里的常见失分点。

可维护性更不是它的首要目标。三次握手的状态机反而让实现变复杂：`SYN-SENT`、`SYN-RECEIVED`、simultaneous open、half-open、RST、重传、backlog、SYN cookie 都是维护成本。但这种复杂度换来的是跨网络、跨实现的互操作正确性。

可以这样拆：

```text
正确性：主要目标。同步双方 ISN，确认连接不是旧重复报文造成的假连接。
性能：副作用和成本并存。增加 1 RTT，但协商 MSS、window scale、SACK、timestamp。
安全性：有限保护。随机 ISN 抗盲猜，不提供认证和加密。
可维护性：不是目标。实现状态机更复杂，但接口抽象更稳定。
```

结合 LogServe，SDK 到 control、worker 到 control、control 到 logd 如果走 TCP/gRPC，handshake 只是 transport 层的连接建立。它不代表 worker 已注册，不代表任务已经提交，也不代表 actor ownership 已经拿到。LogServe 的正确性仍然靠 log-first 事件、idempotency key、epoch fencing、workflow step 状态机这些应用层语义。

面试里可以这样回答：

```text
TCP handshake 的核心目标是正确建立连接上下文，尤其是同步双方初始序列号，避免旧重复 SYN 或旧连接残片被误认为新连接的一部分。它主要解决正确性问题；性能上会增加 1 RTT，但也协商 MSS、SACK、window scale 等能力；安全性只到随机 ISN 抗盲猜这一层，不提供认证和加密；可维护性不是它的主要目标。
```

## Q032. TCP handshake 的典型适用场景和不适用场景分别是什么？

**回答：**

TCP handshake 适用于需要可靠、有序、双向字节流的场景。Web、数据库连接、消息队列、RPC、SSH、文件传输、gRPC over HTTP/2，这些都适合 TCP。应用不想自己处理丢包、乱序、重传、流量控制时，TCP 是合理选择。握手成本通常可以通过连接复用摊薄，比如 HTTP keep-alive、HTTP/2 多路复用、gRPC channel、数据库连接池。

更具体一点，适用场景通常有这些特征：

```text
请求或会话持续时间足够长，可以摊薄 1 RTT 建连成本。
业务需要可靠有序交付，不愿接受应用层自己处理乱序和重传。
协议是字节流模型，不依赖消息边界由传输层保留。
网络路径中代理、防火墙、负载均衡对 TCP 支持成熟。
客户端和服务端需要 backpressure，不能只靠无连接发包。
```

不适用场景也很清楚。第一类是极短报文、极高频、对首包延迟非常敏感的场景。DNS 传统上常用 UDP，就是因为多数查询短小，一来一回就结束。监控打点、实时游戏位置更新、某些音视频实时流，如果能接受少量丢包，通常不希望每个交互都付出 TCP 建连和队头阻塞成本。

第二类是消息边界很重要且应用愿意自己处理可靠性的场景。TCP 是字节流，不保留消息边界。你发送两次 `write()`，对端可能一次 `read()` 读到合并结果，也可能分多次读到。很多初学者把 TCP 当“可靠消息包”，这是错的。需要消息边界时，要在应用层加 length-prefix、delimiter、frame header，或者选择 QUIC、SCTP、应用层消息协议。

第三类是需要更快连接迁移或多路流隔离的场景。移动网络里 IP/端口经常变化，TCP 连接会断；QUIC 把连接身份放在传输层以上，可以更好地处理迁移。HTTP/2 over TCP 虽然能多路复用，但底层 TCP 丢一个包会挡住所有 stream 的后续字节；HTTP/3/QUIC 把队头阻塞范围缩小到单个 stream。

还有一种“看起来适用，实际要小心”的场景：海量短连接服务。比如一个内部 RPC 每次请求都新建 TCP，理论上可以用，线上会遇到握手 RTT、SYN backlog、accept queue、TIME_WAIT、临时端口耗尽、LB idle timeout 等一堆问题。这类场景不一定要放弃 TCP，但必须做连接池和复用。

结合 LogServe，control、worker、logd、SDK 都更适合复用 TCP/gRPC 连接，而不是每个 task 或每次 poll 新建连接。workflow step、actor command、LLM request 都是应用层事件，不应该把 TCP 连接生命周期当成任务生命周期。连接断了可以重连，任务是否重复提交要看 idempotency key 和 shared log。

面试里可以这样回答：

```text
TCP handshake 适合可靠、有序、双向字节流场景，例如 HTTP、数据库、RPC、SSH、消息队列连接。它不适合每次都只有一个极短报文且首包延迟敏感的交互，也不适合误以为传输层会保留消息边界的协议。高并发短连接不是 TCP 不可用，而是必须用 keep-alive、HTTP/2/gRPC channel 或连接池摊薄握手成本。
```

## Q033. TCP handshake 和相近概念最容易混淆的边界在哪里？

**回答：**

最容易混的是“TCP 连接建立”和“应用请求成功”。三次握手成功，只说明内核层四元组对应的 TCP 状态进入 `ESTABLISHED`，双方可以传字节。它不说明 HTTP 请求已经发出，不说明 TLS 已经完成，不说明数据库认证成功，也不说明 RPC 服务端 handler 已经接收请求。很多排障把 `connect ok` 当成“服务可用”，这会漏掉 TLS、协议解析、鉴权、线程池、应用队列这些后续失败点。

第二个边界是 TCP handshake 和 TLS handshake。TCP handshake 建的是传输层连接；TLS handshake 在这个字节流上协商加密参数、验证证书、生成密钥。HTTPS 的“连接建立慢”通常包含 DNS、TCP connect、TLS handshake，甚至代理 CONNECT。只说“握手慢”不够，必须问是哪一层握手。

第三个边界是 TCP handshake 和 HTTP keep-alive。keep-alive 是复用已经建立的 TCP 连接，让多个 HTTP 请求不用重复建连。它不是 TCP 三次握手的一部分。HTTP/2 进一步在一个 TCP 连接上跑多个 stream，但底层仍然只有一个 TCP handshake。HTTP/3/QUIC 则换了传输层，连接建立、加密和流复用语义都不同。

第四个边界是 TCP handshake 和 `accept()`。服务端收到 SYN 并完成三次握手后，连接会进入已完成队列，应用再通过 `accept()` 取走。Linux `listen(2)` 文档明确说，现代 Linux 的 backlog 对 TCP 来说是已完成、等待 accept 的 socket 队列长度；半连接队列另由 `tcp_max_syn_backlog` 等控制。所以客户端 `connect()` 成功，并不代表服务端应用已经 `accept()`，更不代表 handler 已经开始处理。

第五个边界是 handshake 和 liveness。RFC 9293 说 TCP 是 connection oriented，但不天然包含 liveness detection。一个 TCP 连接处于 `ESTABLISHED`，只能说明本地内核还保留连接状态；对端进程可能卡死，网络中间设备可能已经丢了状态，另一端机器也可能重启过。检测活性要靠读写错误、TCP keepalive、应用 heartbeat、deadline。

第六个边界是 handshake 和安全认证。随机 ISN 能降低盲注入难度，但对端是谁、数据是否被窃听或篡改，TCP 本身不解决。SYN cookie 也不是认证机制，它主要是在 SYN flood 压力下减少服务端半连接状态占用。把 SYN cookie 当成安全认证，是概念混淆。

可以用一张边界表：

```text
TCP handshake: 同步 ISN，建立传输层字节流。
TLS handshake: 在字节流上协商密钥和证书认证。
HTTP request: 应用层请求语义，可能复用已有 TCP 连接。
accept(): 服务端应用取走已建立连接。
keepalive/heartbeat: 连接或应用活性探测。
SYN cookie: SYN flood 缓解，不是身份认证。
```

结合 LogServe，`connect()` 到 control 成功不代表 worker 注册成功；gRPC channel ready 不代表 workflow step 已经写入 shared log；TCP 连接断开也不代表 task 失败。LogServe 要在应用层区分 transport connectivity、RPC success、log append committed、worker lease/heartbeat、actor ownership epoch。

面试里可以这样回答：

```text
TCP handshake 只建立传输层连接，容易和 TLS handshake、HTTP 请求、accept、keep-alive、应用 heartbeat 混在一起。connect 成功不代表服务端应用已经 accept 或处理请求；ESTABLISHED 不代表对端应用还活着；TCP 也不提供身份认证和加密。排查问题时要分 DNS、TCP connect、TLS、应用协议、服务端队列和业务处理阶段。
```

## Q034. TCP handshake 在高并发场景下可能出现哪些隐藏问题？

**回答：**

高并发下，handshake 的问题通常不在“三个包发不发得出去”，而在状态和队列。服务端收到 SYN 后需要维护半连接状态，完成握手后要把连接放进 accept 队列。SYN 速率高、accept 慢、backlog 太小、CPU 中断打满、连接跟踪表爆掉，都会让客户端看到 connect 慢、超时、reset 或偶发成功率下降。

最典型的是 SYN backlog 和 accept queue。半连接队列满时，SYN 可能被丢弃，客户端重传 SYN，表现为 connect 延迟拉长。已完成队列满时，Linux 可能忽略最终 ACK，让客户端以为连上但服务端还没把连接交给应用；如果打开 `tcp_abort_on_overflow`，满队列时还可能 reset 客户端。`listen(2)` 和 Linux ip-sysctl 都提醒过这些行为，尤其是 `tcp_abort_on_overflow` 不应该随便打开。

第二个隐藏点是 SYN flood 与合法突发流量很像。RFC 4987 讨论 SYN flood 时说，攻击会消耗 end-host 保存半连接所需的资源。线上更麻烦的是，合法流量突增也会产生类似症状。Linux 的 `tcp_syncookies` 是 fallback 设施，内核文档明确说不应该用它支撑合法高负载。如果日志里出现 SYN flood warning，但实际是业务流量突增，应该调 backlog、accept 能力、连接复用、负载均衡，而不是把 SYN cookie 当扩容手段。

第三个是客户端侧资源。高并发短连接会耗尽临时端口，堆积 TIME_WAIT，或者让 NAT/SNAT 端口表打满。客户端看到的是 `EADDRNOTAVAIL`、`connect timeout`、`connection reset`，服务端可能完全没感觉。多实例经过同一个 NAT 网关时，这个问题更明显。

第四个是连接建立路径上的共享组件：L4 LB、反向代理、sidecar、iptables/nftables、conntrack、云安全组、eBPF 程序。它们都可能在握手路径上分配状态。应用服务器还没到瓶颈，LB 的 SYN proxy 或 conntrack 先满了，也会表现为握手失败。

第五个是 TLS 叠加。纯 TCP handshake 主要吃网络 RTT 和内核路径；TLS handshake 会增加 CPU、证书链、密钥交换、session ticket、OCSP、证书加载等成本。线上经常说“连接建立慢”，实际上慢的是 TCP+TLS+应用初始化一起慢。指标必须拆开。

排查高并发握手问题，我会看这些：

```text
服务端：ListenOverflows、ListenDrops、SYN_RECV、ESTABLISHED、accept Q、accept rate。
内核：tcp_max_syn_backlog、somaxconn、tcp_synack_retries、tcp_syncookies、tcp_abort_on_overflow。
客户端：SYN_SENT、TIME_WAIT、EADDRNOTAVAIL、connect latency p99、端口范围。
中间层：LB active connections、SYN rate、conntrack count、SNAT port usage。
应用：accept loop 是否阻塞，worker/thread 是否耗尽，是否每请求新建连接。
```

结合 LogServe，如果 worker 很多并且 control 重启，所有 worker 同时重连，就会制造握手风暴。即使单个连接很轻，瞬时 SYN、TLS、gRPC channel 建立、注册 RPC、heartbeat 都会叠在一起。更稳的做法是连接复用、重连退避加 jitter、限制每个进程的并发 dial、服务端 accept/backlog 指标可观测。

面试里可以这样回答：

```text
高并发下 TCP handshake 的隐藏问题主要是队列和状态耗尽：SYN backlog 满、accept queue 满、SYN cookie 触发、conntrack/NAT 表满、客户端临时端口耗尽、TIME_WAIT 堆积、LB 或 sidecar 先到瓶颈。症状包括 connect p99 变高、SYN_SENT/SYN_RECV 增多、偶发 reset、ECONNREFUSED、EADDRNOTAVAIL。处理方向是连接复用、backlog/accept 能力调优、重连退避、扩容中间层，而不是只调一个 timeout。
```

## Q035. TCP handshake 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

崩溃和重启会暴露 TCP 最难的一类边界：一端忘了连接状态，另一端还以为连接存在。RFC 9293 把这类问题放在 half-open connections and anomalies 里讨论。比如服务端重启后丢失所有 TCB，客户端本地连接还处于 `ESTABLISHED`；客户端下一次发数据，服务端可能因为找不到对应连接而回 RST。应用看到的是“之前好好的连接，下一次写突然 reset”。

重启还有 ISN 和旧报文问题。TCP 用 32 位序列号，网络里可能有旧连接的延迟报文。如果重启后立刻复用相同四元组和相近序列号，旧报文可能落到新连接窗口里。RFC 9293 的 quiet time 和 MSL 讨论就是为了避免“丢失序列号记忆后马上发新序列号”带来的风险。现代实现通常用随机化和时间相关 ISN 降低风险，但测试简化版握手时不能忽略这个边界。

超时场景主要分主动打开和被动打开。客户端发 SYN 后收不到 SYN-ACK，会重传 SYN，最后 connect timeout。服务端发 SYN-ACK 后收不到最终 ACK，会重传 SYN-ACK；Linux 的 `tcp_synack_retries` 控制被动连接尝试里 SYN-ACK 的重传次数。这里有一个常见误判：客户端 connect timeout 不一定是服务端没收到 SYN，可能是 SYN-ACK 回不来，也可能是最终 ACK 或中间设备状态出问题。

重试场景最危险的是应用把 TCP connect 失败等同于“服务端一定没处理”。对 TCP handshake 本身，connect 没成功通常意味着应用数据还没进入正常字节流；但一旦用了 TCP Fast Open，客户端可能在 SYN 里带数据，重试就有应用层重复风险。RFC 7413 为 Fast Open 引入 cookie，就是为了在允许握手期间带数据时降低新的安全风险。即使不用 TFO，TLS 0-RTT、HTTP 重试、RPC retry 也会把类似问题带回应用层。

还有 simultaneous open。虽然普通客户端/服务端模型少见，但 RFC 9293 要求 TCP 实现支持同时打开：两端都发 SYN，状态会从 `SYN-SENT` 到 `SYN-RECEIVED` 再到 `ESTABLISHED`。如果从零实现简化版 handshake，很容易只写“客户端主动、服务端被动”的路径，漏掉同时打开、重复 SYN、RST、旧 SYN 这些边界。

崩溃重启后还要看端口和 TIME_WAIT。服务端进程重启时 bind/listen 可能遇到地址占用；客户端重试太快可能堆积 SYN_SENT 和 TIME_WAIT；负载均衡把新连接打到刚启动但应用还没 ready 的实例，会造成 connect 成功但应用协议失败。这些都不是三次握手规范本身的问题，却会在握手阶段暴露。

结合 LogServe，control/logd/worker 崩溃重启后，TCP 连接重建只是恢复通道。真正的恢复要靠 shared log replay、worker 重新注册、heartbeat、task redelivery、actor epoch fencing。一次 TCP reset 不能直接推导 task 成败；SDK 需要带 idempotency key，control 需要根据 log 里的事件判断是否已提交。

面试里可以这样回答：

```text
崩溃和重启会让一端丢失 TCP 状态，另一端仍以为连接存在，下一次读写常见 RST，这就是 half-open 的典型来源。超时会触发 SYN 或 SYN-ACK 重传，connect timeout 不等于服务端完全没收到包。重试时还要注意 TFO、TLS 0-RTT 或应用层 retry 可能带来重复副作用。实现上还要覆盖旧重复 SYN、RST、simultaneous open、TIME_WAIT 和重启后 ISN/quiet time 这些边界。
```

## Q036. TCP handshake 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

普通场景下，TCP handshake 的主要瓶颈来自网络 RTT。三次握手至少需要客户端发 SYN、服务端回 SYN-ACK、客户端回 ACK，客户端通常要等一个 RTT 才认为 connect 完成。跨地域、移动网络、丢包、SYN 或 SYN-ACK 重传，都会直接把握手耗时拉高。你看到 connect p50 接近某条链路 RTT，基本就是这个原因。

高并发场景下，瓶颈会从“网络 RTT”变成“网络 + 内核状态 + 队列 + 中间设备”。服务端要处理 SYN、分配或编码半连接状态、发 SYN-ACK、把完成连接放到 accept queue。accept queue 满、SYN backlog 满、软中断 CPU 打满、网卡队列拥塞、conntrack 表满、LB SYN proxy 压力，都能让 handshake 变慢或失败。

CPU 通常不是单个 TCP handshake 的第一瓶颈，但在以下场景会变成瓶颈：

```text
每秒新建连接数非常高，软中断和内核协议栈处理打满 CPU。
启用大量 iptables/nftables/eBPF/conntrack 规则，握手包路径变长。
叠加 TLS handshake，密钥交换和证书处理占 CPU。
SYN flood 或合法突发导致 SYN cookie、SYN-ACK 重传、RST 处理激增。
```

内存瓶颈主要体现在半连接状态、已完成连接、socket buffer、conntrack/NAT 表、accept 队列。RFC 4987 讨论 SYN flood 时指出，攻击目标正是端主机保存 SYN 状态的资源。SYN cookie 的思路就是在压力下减少服务端保存半连接状态，但它不是合法高负载的扩容方案。

锁竞争通常是特定实现里的瓶颈，不是协议层必然瓶颈。早期或某些配置下，listen socket、accept queue、全局计数器、conntrack hash bucket、日志锁都有可能造成竞争。现代内核、RSS/RPS、SO_REUSEPORT、多队列网卡会缓解，但如果所有连接都打到一个 listener、一个 CPU、一个进程 accept loop，锁和调度仍然可能成为热点。

磁盘 I/O 一般不在 TCP handshake 的关键路径上。除非应用在 accept 后立刻同步写日志、TLS 读证书、审计系统同步落盘，或者系统内存压力导致异常。说“TCP 三次握手慢主要是磁盘 I/O”通常不成立。真正的 I/O 更可能是网络 I/O。

判断瓶颈时不要猜，拆指标：

```text
connect latency 接近 RTT：网络路径主导。
SYN/SYN-ACK 重传高：丢包、回程路由、防火墙、队列丢包。
ListenDrops/Overflows 高：accept 或 backlog 问题。
softirq CPU 高：包处理压力。
conntrack 接近上限：中间状态表问题。
TLS handshake CPU 高：不是 TCP handshake 本身，而是 TLS 层。
```

结合 LogServe，单机实验里 TCP handshake 通常不会是瓶颈，更多瓶颈在 log append fsync、worker 执行、LLM mock/vLLM、scheduler 和 result store。只有在大量 worker/SDK 短连接、control 重启后的重连风暴、dashboard 频繁新建连接时，handshake 才会变成可见问题。对应的工程手段是复用 gRPC channel、限制并发 dial、连接池和重连 jitter。

面试里可以这样回答：

```text
单个 TCP handshake 的主要成本通常是网络 RTT；高并发时瓶颈会转移到 SYN backlog、accept queue、softirq CPU、conntrack/NAT 状态、临时端口和中间负载均衡。CPU 在每秒新建连接数很高或叠加 TLS 时会明显，内存体现在半连接和 socket/conntrack 状态，磁盘 I/O 通常不在 TCP handshake 关键路径。判断时要看 connect latency、重传、ListenDrops、softirq、conntrack 和 TLS 指标。
```

## Q037. TCP handshake 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

这三类测试目的完全不同。correctness test 测状态机是否符合协议；stress test 测极端和异常条件下是否还能保持正确或优雅失败；benchmark 测在给定环境下的成本和容量。把 benchmark 当 correctness test 是不够的，因为跑得快不代表旧 SYN、RST、重传、simultaneous open 都处理对了。

Correctness test 应该围绕 RFC 9293 的状态机和不变量来写：

```text
正常三次握手：CLOSED -> SYN-SENT -> ESTABLISHED，LISTEN -> SYN-RECEIVED -> ESTABLISHED。
双方 ISN 正确消耗：SYN 占用一个序列号，ACK 确认对端 ISN+1。
option 协商：MSS、SACK permitted、window scale、timestamp 只在合法阶段生效。
重复 SYN：不会创建两个等价连接或污染当前连接。
丢 SYN / 丢 SYN-ACK / 丢最终 ACK：按重传和超时处理。
RST：非同步状态下可回到 LISTEN，同步状态下 abort 并通知上层。
simultaneous open：两端都主动打开也能进入 ESTABLISHED。
half-open：一端状态丢失后，另一端后续数据触发合理 RST/恢复路径。
```

如果是简化实现，还要明确不测哪些高级特性，比如 TCP Fast Open、MPTCP、SYN cookie、ECN、timestamp PAWS。但“不支持”也要有行为：收到不认识的 option 要忽略还是拒绝，不能随机崩。

Stress test 要制造压力和坏网络：

```text
高 SYN rate，观察 SYN_RECV、ListenDrops、ListenOverflows、SYN cookie 触发。
大量合法短连接，观察 accept queue、TIME_WAIT、临时端口、CPU softirq。
丢包、延迟、抖动、乱序、重复包，验证重传和超时不会泄漏状态。
服务端 accept 故意变慢，验证 backlog 满时行为符合预期。
客户端半开后不发最终 ACK，验证半连接能超时回收。
重启服务端或客户端，验证 half-open、RST、重连语义。
```

Linux 上可以用 `tc netem` 模拟 delay、jitter、loss、duplicate、reorder。它不能证明协议正确，但能把很多“实验室里稳定网络才会过”的实现打出来。SYN flood 类测试要在隔离环境做，避免伤到共享网络。

Benchmark 则要量化这些指标：

```text
connect latency p50/p95/p99/p999
每秒新建连接数 connections/s
accept rate
CPU cycles 或 softirq CPU
每连接内存开销
SYN/SYN-ACK retransmission rate
backlog overflow rate
客户端端口使用量和 TIME_WAIT 数
连接复用前后的请求延迟差异
```

benchmark 要控制变量。只测 localhost 会把网络 RTT 消掉，适合看内核和应用 accept 成本；跨机同 AZ 能看真实网卡和交换机；跨地域能看 RTT 主导的成本。TLS 要单独测，否则你不知道慢的是 TCP connect 还是 TLS handshake。连接池场景也要分 cold connect 和 warm reused connection，否则数据没有解释力。

结合 LogServe，correctness test 不应该只测 SDK 能连上 control，还要测“连接失败后任务提交不会重复破坏语义”。stress test 可以模拟 control 重启后 worker 同时重连、logd 短暂不可达、worker long-poll 断开重试。benchmark 则分别记录 TCP/gRPC cold connect、warm channel RPC、log append latency、workflow end-to-end latency。

面试里可以这样回答：

```text
correctness test 测 TCP 状态机和序列号不变量，例如正常握手、重复 SYN、丢包重传、RST、simultaneous open、half-open。stress test 用高 SYN rate、慢 accept、丢包抖动、重启、半开连接检查状态是否泄漏和是否优雅失败。benchmark 才测 connect p99、connections/s、accept rate、CPU、内存、重传率、backlog overflow 和连接复用收益。三者不能互相替代。
```

## Q038. 如果要求从零实现一个简化版 TCP handshake，你会先定义哪些不变量？

**回答：**

我会先定义不变量，而不是先写状态转移代码。TCP handshake 的难点不是画出 SYN、SYN-ACK、ACK 三个箭头，而是在重复包、乱序、重传、RST、旧连接残片出现时，状态不能被污染。简化实现可以少做很多 option，但几个核心不变量不能省。

第一组是不变量关于连接身份：

```text
一个连接由 local_ip、local_port、remote_ip、remote_port、protocol 唯一标识。
LISTEN socket 不是已建立连接；accept 后产生新的 connected socket。
同一时刻不能存在两个完全相同四元组且状态冲突的活动连接。
收到不匹配四元组的包不能修改当前连接状态。
```

第二组是不变量关于状态机：

```text
客户端主动打开：CLOSED -> SYN-SENT -> ESTABLISHED。
服务端被动打开：LISTEN -> SYN-RECEIVED -> ESTABLISHED。
未收到对端 SYN 不能发送合法 SYN-ACK。
未确认对端 SYN-ACK 不能把主动打开视为 ESTABLISHED。
重复包最多触发重发或 ACK，不应创建第二个连接。
超时必须回收 SYN-SENT / SYN-RECEIVED 状态。
```

第三组是不变量关于序列号：

```text
每端选择自己的 ISN。
SYN 消耗一个序列号。
SYN-ACK 的 ack 必须等于 client_isn + 1。
最终 ACK 的 ack 必须等于 server_isn + 1。
进入 ESTABLISHED 后，本端 snd_nxt 和对端 rcv_nxt 必须与已确认的 ISN 对齐。
任何不在接收窗口或不符合当前状态的段不能推进状态。
```

第四组是不变量关于旧包和安全边界：

```text
ISN 不能简单从 0 开始递增到所有连接都可预测。
重启后不能立即复用可能与旧报文冲突的序列空间。
RST 只能在可接受序列条件下影响连接，避免被随便打掉。
半连接状态必须有上限和超时，避免 SYN flood 或误用拖垮服务端。
```

第五组是不变量关于资源：

```text
SYN-SENT、SYN-RECEIVED、ESTABLISHED 都要有明确生命周期。
backlog 满时行为要确定：丢弃、延迟、reset，不能随机失败。
每条失败路径都释放 TCB、timer、队列节点和引用计数。
重传 timer 不能无限创建；同一连接同类 timer 至多一个有效实例。
```

第六组是不变量关于接口语义：

```text
connect 成功只代表 TCP ESTABLISHED，不代表应用层请求成功。
accept 返回的是已建立连接，不返回半连接。
connect 失败后 socket 状态不可继续假设可用，应关闭重建。
写入 API 在未 ESTABLISHED 前要么阻塞/排队，要么返回明确错误。
```

如果实现范围允许，我还会把“不支持项”写成显式规则：不支持 simultaneous open 就要明确收到交叉 SYN 时如何处理；不支持 TCP Fast Open 就不能接受 SYN data；不支持 option 就要按 TCP 规则忽略未知 option 或拒绝非法 option。简化版最怕“正常路径能跑，异常包随缘”。

结合 LogServe，这个思路和项目里的 actor/runtime 设计很像。actor command 不是先写代码再猜行为，而是先定义 `command_seq`、owner epoch、mailbox 串行化、log-first commit 这些不变量。TCP handshake 的不变量在传输层，LogServe 的不变量在应用层；层次不同，但方法一致。

面试里可以这样回答：

```text
我会先定义连接身份、状态机、序列号、资源回收和接口语义的不变量。核心包括四元组唯一标识连接，SYN 消耗一个序列号，SYN-ACK 和最终 ACK 必须确认对端 ISN+1，重复或旧包不能创建第二个连接，半连接必须有超时和上限，connect 成功只表示 TCP ESTABLISHED。简化实现可以少做 option，但这些不变量不能省。
```

## Q039. TCP handshake 的常见误用是什么，误用后通常会产生什么线上症状？

**回答：**

最常见的误用是每个请求都新建 TCP 连接。功能上能跑，线上会很快暴露：connect p99 高、TIME_WAIT 暴涨、临时端口耗尽、SYN backlog 压力、LB 连接数抖动、CPU softirq 上升。HTTP、gRPC、数据库、对象存储客户端都应该复用连接或 client 实例。把 client 放在请求函数里每次 new，是非常典型的隐患。

第二个误用是把 connect timeout 当成完整请求 timeout。connect timeout 只管建连阶段，连接建立后 TLS、写请求、读响应头、读 body 都可能卡住。症状是监控里 connect 很快，但请求线程/goroutine 挂住，连接池被占满，用户看到整体超时。正确做法是连接超时、TLS handshake timeout、response header timeout、read/write timeout 和整体 deadline 分开。

第三个误用是把 TCP 连接成功当成服务健康。健康检查只做 TCP connect，会把“端口在 listen”误判成“服务可用”。应用可能还没加载完、线程池满、数据库断了、logd append 卡住，TCP 仍然能握手成功。症状是 LB 继续把流量打给半死实例，用户请求 5xx 或超时。更可靠的是应用层健康检查，但也不能太重，避免健康检查本身打垮服务。

第四个误用是忽略 backlog 和 accept 能力。只调大应用并发，不看 `somaxconn`、`tcp_max_syn_backlog`、accept loop、worker 线程。症状是流量突增时 connect timeout、ECONNREFUSED、reset、ListenDrops/Overflows 增加。把 `tcp_abort_on_overflow=1` 当优化也危险，它可能把本来可恢复的突发直接变成客户端 reset。

第五个误用是乱用 TCP Fast Open 或 TLS 0-RTT，把带副作用的请求放到早期数据里。症状是重试、重放、网络切换时出现重复下单、重复提交、重复写日志。这不是说 TFO 不能用，而是早期数据必须只承载幂等或可重放安全的操作。

第六个误用是认为 TCP 保留消息边界。握手成功后只是字节流，应用如果没有 framing，就会出现粘包/拆包 bug。症状是偶发协议解析失败、请求串包、读取半个消息、压测下比低并发更容易复现。这个问题发生在握手之后，但根源常常是把“连接”误解成“消息通道”。

第七个误用是重连没有退避和 jitter。服务端重启或网络抖动后，所有客户端同时发起 handshake，形成重连风暴。症状是服务刚恢复又被 SYN、TLS、认证、注册请求打满，监控呈现周期性雪崩。连接层 retry 必须尊重整体 deadline，并用指数退避和随机抖动。

结合 LogServe，典型误用是 SDK 每次 submit task 都新建 gRPC 连接，worker long-poll 断开后 tight loop 重连，或者只用 TCP connect 检查 control 健康。更稳的做法是复用 channel、设置分层 timeout、worker 注册和 heartbeat 做应用层健康、任务提交用 idempotency key，actor 完成用 epoch fencing。

面试里可以这样回答：

```text
常见误用包括每请求新建连接、只设 connect timeout、把 TCP connect 成功当服务健康、忽略 backlog/accept 能力、早期数据承载非幂等操作、误以为 TCP 保留消息边界、重连没有退避。线上症状通常是 connect p99 高、TIME_WAIT 和端口耗尽、ListenDrops/Overflows、偶发 reset、健康检查假阳性、协议解析粘包拆包、服务恢复时重连风暴。
```

## Q040. TCP handshake 在单机和分布式环境中的语义有什么差异？

**回答：**

TCP handshake 在协议语义上没有因为单机或分布式而改变。无论两端在同一台机器、同一个机房，还是跨地域，三次握手仍然是 SYN、SYN-ACK、ACK，仍然同步 ISN，仍然建立一个四元组标识的连接。差异在工程语义：单机环境里你更容易把 connect 成功理解成“对端服务可用”；分布式环境里，中间层和失败模式会让这个理解变得危险。

单机或同机回环里，握手路径短，丢包少，RTT 极小，没有真实 LB、NAT、跨 AZ、防火墙、conntrack 压力。connect 成功和应用进程可达之间的距离很近。测试也更稳定，所以很多问题不会出现：SYN-ACK 回程丢失、NAT 状态过期、LB idle timeout、跨地域 RTT、SYN flood 防护、SNAT 端口耗尽。

分布式环境里，握手经过的组件多得多：

```text
client -> sidecar -> node iptables/eBPF -> NAT/SNAT -> L4 LB -> firewall -> server node -> sidecar -> app
```

每一层都可能接受、丢弃、重置、代理或重写连接。后端应用看到的 peer 可能是代理，不是真客户端；客户端 connect 成功可能只是连到了 LB，不代表后端实例已经准备好；L4 LB 可能完成前半段握手后再连接后端；SYN proxy 甚至会改变服务端看到 SYN 的时机。于是 TCP handshake 的“端到端”直觉被中间层削弱了。

单机场景里，连接失败通常比较直接：端口没 listen、进程崩了、backlog 小。分布式场景里，同一个错误码有更多解释：`ECONNREFUSED` 可能来自目标主机，也可能来自 LB；`ETIMEDOUT` 可能是安全组、路由、SYN-ACK 回程、conntrack 丢状态；`ECONNRESET` 可能是后端、代理、防火墙或协议不匹配。排查必须带上路径视角。

还有调度和身份语义差异。单机里，一个进程重启后连接断了，客户端重连大概率还是同一个服务实例。分布式里，重连可能落到另一个实例、另一个 AZ、甚至另一个 region。TCP 连接身份不能等同于业务会话身份。需要 sticky session、token、request id、lease、epoch、幂等键来维持应用层语义。

性能语义也不同。单机 loopback 上 handshake benchmark 主要测内核和应用 accept；跨机测网卡、交换机、队列；跨地域测 RTT；经过 TLS/LB/sidecar 则测整条连接建立路径。把 localhost benchmark 结果拿去推断公网 connect p99，基本没有意义。

结合 LogServe，当前单机多进程实验里，TCP handshake 的语义比较接近“本机进程间通道建立”。如果扩展到分布式，worker 重连到 control 成功只代表某条网络通道恢复，不代表旧任务没有执行、不代表 actor owner 没变、不代表 log append 已提交。系统必须继续依赖 shared log、worker heartbeat、idempotency key、epoch fencing，而不是依赖 TCP 连接是否持续存在。

面试里可以这样回答：

```text
协议层语义不变，都是通过三次握手同步 ISN 并建立四元组连接。差异在工程语义：单机路径短，connect 成功接近进程可达；分布式环境里有 LB、NAT、sidecar、防火墙、conntrack 和跨地域 RTT，connect 成功可能只说明连到中间层，不代表后端应用健康。重连也可能落到不同实例，所以业务会话、幂等、lease、epoch 不能靠 TCP 连接本身表达。
```

## Q041. TIME_WAIT 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

TIME_WAIT 的核心目标是关闭后的正确性保护。它不是一个“连接没有关干净”的异常状态，而是 TCP 主动关闭路径里很正常的一段等待期。按 RFC 9293 的状态机，连接一方完成主动关闭后，在收到对端 FIN 并发出最后一个 ACK 之后进入 TIME-WAIT。这个状态至少有两个作用：第一，如果最后一个 ACK 丢了，对端会重传 FIN，TIME_WAIT 端还能再回 ACK；第二，让旧连接里的延迟报文在网络中自然过期，避免同一个四元组被快速复用后，旧报文误伤新连接。

所以它首先解决的是正确性问题。TCP 靠四元组和序列号识别一条连接。如果本地端口马上复用，而网络里还有旧 FIN、旧 ACK、旧数据段、旧 RST，它们可能撞上新连接。正常的序列号检查已经能挡掉很多旧包，但 TCP 不能假设所有历史报文都一定无害。TIME_WAIT 用时间隔离旧连接，是 TCP 可靠字节流语义的一部分。

它对性能的影响更像代价。TIME_WAIT 会占用 socket 状态、定时器和本地端口，短连接很多时会造成端口耗尽、连接失败或内核表项压力。这个代价不是 bug，而是协议为了避免旧报文污染新连接付出的成本。面试里如果只说“TIME_WAIT 占资源，所以应该调小”，回答就会偏。正确顺序通常是先减少短连接和主动关闭风暴，再考虑内核参数。

安全性方面，TIME_WAIT 有一点防旧报文注入的意义，但它不是认证机制。RFC 1337 讨论的 TIME-WAIT assassination hazards 说明，错误处理 RST 或旧报文可能提前杀掉 TIME_WAIT，从而暴露旧报文污染新连接的风险。真正的身份认证、加密、防中间人攻击，还是 TLS、mTLS、IPsec 或应用层鉴权负责。

可维护性不是它的主目标。TIME_WAIT 反而会给排障带来额外心智负担：`ss` 里看到一堆 `TIME-WAIT`，应用开发者容易误判成泄漏；调参又容易引入更隐蔽的问题。但这个状态让 TCP 在连接关闭、重传和四元组复用之间有一个明确边界，长期看是为了协议行为稳定。

可以这样拆：

```text
正确性：主要目标。确认最后 ACK 可重传，并让旧报文过期。
性能：主要是成本。占用端口、状态和定时器，短连接多时会放大。
安全性：有限相关。降低旧报文污染风险，不提供认证和加密。
可维护性：不是目标。排障更复杂，但关闭语义更清楚。
```

结合 LogServe，如果 SDK、worker、control、logd 之间频繁新建 TCP/gRPC 连接，TIME_WAIT 很快会从协议细节变成可见故障：客户端端口被占满，worker 重连失败，或者控制面看到一批短连接抖动。系统的正确性不能靠“连接还在不在”判断，任务提交、workflow step、actor command 仍然要靠 shared log、idempotency key、epoch fencing 和 heartbeat 来定边界。

面试里可以这样回答：

```text
TIME_WAIT 的核心目标是正确性。它保证主动关闭方在最后 ACK 丢失时还能响应对端重传的 FIN，同时等待旧连接中的延迟报文过期，避免同一四元组快速复用后旧报文污染新连接。它会带来端口和内核状态成本，但这不是它的设计目标；安全性和可维护性也只是间接受影响。
```

## Q042. TIME_WAIT 的典型适用场景和不适用场景分别是什么？

**回答：**

TIME_WAIT 适用于正常 TCP 连接关闭，尤其是主动关闭方需要保护同一四元组复用的场景。常见例子是客户端主动关闭到服务端的短连接、反向代理主动关闭上游或下游连接、服务端因为 idle timeout 主动关闭空闲连接。只要连接经历了正常 FIN 关闭流程，最后发送 ACK 的一方通常就要承担 TIME_WAIT。

它特别适合这些场景：

```text
连接用同一四元组标识，且四元组可能很快被复用。
网络里可能存在延迟、重复、乱序报文。
关闭流程走 FIN/ACK，而不是直接 RST 中断。
应用需要确认对端 FIN 被最终 ACK 覆盖。
系统愿意用一段等待时间换旧报文隔离。
```

这里有个容易忽略的点：TIME_WAIT 不是只有客户端才会有。谁主动关闭，谁更可能进入 TIME_WAIT。HTTP 服务端如果设置很短的 keep-alive idle timeout，并主动关闭大量空闲连接，服务端也会堆很多 TIME_WAIT。反向代理如果频繁主动断开上游连接，也可能在代理节点上看到 TIME_WAIT，而不是在业务进程所在机器上看到。

不适用场景也要说清楚。UDP 没有 TCP TIME_WAIT，因为 UDP 没有 TCP 这种连接状态机；QUIC 虽然也要处理 closing/draining 之类的关闭阶段，但它不是 TCP 的 TIME_WAIT，连接身份、加密和重传语义都不同。把 QUIC 的关闭状态直接叫 TIME_WAIT，会混淆协议层次。

异常关闭也不等同于 TIME_WAIT。比如 `SO_LINGER` 设置为 0 后 close 往往会发 RST，连接会被中止，未发送或未确认的数据可能丢失。这样做可能减少本端 TIME_WAIT，但代价是对端看到 connection reset，应用语义可能被破坏。用 RST 逃避 TIME_WAIT，通常是把一个可解释的资源问题换成更难排查的数据截断或重置问题。

长连接场景不是“不适用”，而是 TIME_WAIT 不应该频繁出现。gRPC channel、HTTP/2 连接、数据库连接池、worker 长轮询连接如果设计合理，连接生命周期应该明显长于单个请求生命周期。TIME_WAIT 只在连接退出时出现，而不是每个 task、每个 RPC、每个 heartbeat 都出现。

结合 LogServe，worker 到 control 的注册、poll、heartbeat 更适合复用长连接；SDK 到 control 的任务提交也应该通过 gRPC channel 或 HTTP keep-alive 摊薄连接成本。TIME_WAIT 可以出现在进程退出、rolling restart、idle close、故障重连时，但不应该成为正常请求路径上的高频状态。

面试里可以这样回答：

```text
TIME_WAIT 适用于 TCP 正常关闭后需要保护四元组复用的场景，通常由主动关闭方承担。它不适用于 UDP，也不能直接套到 QUIC；RST 异常关闭也不是 TIME_WAIT 的正常语义。长连接不是没有 TIME_WAIT，而是应该把 TIME_WAIT 降低到连接生命周期结束时，而不是每个请求结束时。
```

## Q043. TIME_WAIT 和相近概念最容易混淆的边界在哪里？

**回答：**

TIME_WAIT 最容易和 CLOSE_WAIT 混。TIME_WAIT 通常是正常关闭后的等待状态，多数时候说明本端主动关闭了连接，内核正在等旧报文过期。CLOSE_WAIT 则完全不同：对端已经发 FIN，本端内核已经把 EOF 告诉应用，但应用还没 close socket。如果 CLOSE_WAIT 很多，通常是应用忘记关闭连接、读循环退出后没有释放资源，或者某个连接对象泄漏。一个是协议保护，一个是应用收尾问题。

第二个容易混的是 FIN_WAIT2。FIN_WAIT2 表示本端已经发 FIN，且对端已经 ACK 了本端 FIN，但对端还没发 FIN。它不是 TIME_WAIT。FIN_WAIT2 很多时，要看对端是否迟迟不关闭、应用协议是否半关闭、超时设置是否缺失。TIME_WAIT 是最后 ACK 发出后的等待；FIN_WAIT2 是还在等对端结束发送方向。

第三个边界是 TIME_WAIT 和端口耗尽。TIME_WAIT 过多可以导致本地临时端口紧张，但 TIME_WAIT 本身不是“端口耗尽”的全部原因。端口耗尽还可能来自连接泄漏、长时间 ESTABLISHED、NAT 端口池不足、连接目标过于集中、`ip_local_port_range` 太小、客户端没有连接复用。排查时要看状态分布，不要看到 TIME_WAIT 就直接调内核参数。

第四个边界是 TIME_WAIT 和 keepalive。TCP keepalive 用来探测长时间空闲的 ESTABLISHED 连接是否还活着；TIME_WAIT 是连接已经关闭后的等待。keepalive 不能清理 TIME_WAIT，也不能替代 TIME_WAIT。应用层 heartbeat 同理，它能判断业务会话是否健康，但不能改变 TCP 四元组复用的旧包隔离规则。

第五个边界是 TIME_WAIT 和 `SO_REUSEADDR`、`SO_REUSEPORT`。这些选项主要影响 bind/listen 或端口复用规则，不是“强行绕过 TIME_WAIT 的万能按钮”。对客户端主动连接来说，同一个本地 IP、本地端口、远端 IP、远端端口组成的四元组不能在不安全的情况下随便复用。否则旧报文风险仍然存在。

第六个边界是 TIME_WAIT 和 backlog。SYN backlog、accept queue 处理的是连接建立阶段；TIME_WAIT 是连接关闭后阶段。一个服务连接失败，可能是握手队列满，也可能是端口耗尽，也可能是 TIME_WAIT 表项压力。它们都影响连接，但位置完全不同。

可以用这张表记：

```text
TIME_WAIT  ：正常关闭后的旧包隔离，常见于主动关闭方。
CLOSE_WAIT ：对端已关闭，本端应用未 close，常见于应用泄漏。
FIN_WAIT2  ：本端 FIN 已被 ACK，还在等对端 FIN。
ESTABLISHED：连接仍建立，不代表应用健康。
SYN_RECV   ：握手阶段半连接，不是关闭阶段问题。
```

结合 LogServe，如果 worker 进程里看到大量 CLOSE_WAIT，应该查 gRPC stream、HTTP response body、socket close 路径；如果看到大量 TIME_WAIT，要查谁在主动关闭、是否每次 poll 都新建连接、连接池 idle 策略是否太激进。两者的修法不一样。

面试里可以这样回答：

```text
TIME_WAIT 是正常 TCP 关闭后的保护状态，CLOSE_WAIT 多数是应用没有 close，FIN_WAIT2 是还在等对端 FIN。TIME_WAIT 可能导致端口压力，但不能和端口耗尽、keepalive、backlog、SO_REUSEADDR 混为一谈。排障时先按 TCP 状态机定位阶段，再看是应用泄漏、短连接过多，还是内核队列和端口范围问题。
```

## Q044. TIME_WAIT 在高并发场景下可能出现哪些隐藏问题？

**回答：**

高并发下，TIME_WAIT 最大的隐藏问题是它把“每次连接关闭的成本”累积成系统级限制。单个 TIME_WAIT 很轻，几十个也没什么感觉；当请求模型变成每秒几千、几万次短连接时，本地端口、内核表项、NAT 映射和负载均衡器连接状态都会被放大。

第一类问题是客户端临时端口耗尽。客户端连接同一个服务端 IP:port 时，能并发或快速轮转使用的四元组数量受本地临时端口范围限制。假设可用临时端口约 28,000 个，TIME_WAIT 保留 60 秒，粗略上同一个目标的可持续新连接速率只有几百到一千量级。超过这个速率，可能出现 `EADDRNOTAVAIL`、`cannot assign requested address`、connect timeout 或大量 SYN 重传。扩大端口范围能缓解，但不能替代连接复用。

第二类问题是服务端或代理节点主动关闭导致 TIME_WAIT 堆在“意想不到的一侧”。很多人默认 TIME_WAIT 在客户端，其实不是。服务端 keep-alive timeout 太短、反向代理主动断上游、sidecar 主动关闭连接，都可能让服务端、代理或 sidecar 成为 TIME_WAIT 大户。排障时只看业务进程所在机器，可能看不到真正的瓶颈。

第三类问题是 NAT、conntrack 和四层负载均衡的状态表压力。分布式环境里，客户端到服务端之间可能有节点级 SNAT、Kubernetes Service、云 NAT Gateway、L4 LB、service mesh sidecar。每一层都可能维护自己的连接状态和超时。应用机器上的 TIME_WAIT 不高，不代表 NAT 网关没有端口池压力；反过来，应用看到 connect 抖动，根因可能在中间设备的状态回收。

第四类问题是调参带来的隐性正确性风险。Linux 有 `tcp_tw_reuse` 这类参数，内核文档的语气也很谨慎：只在协议安全时复用 TIME-WAIT socket，并提示不要在没有技术专家建议时随便改。RFC 6191 讨论过利用 TCP timestamps 降低 TIME_WAIT 状态带来的复用成本，但前提是时间戳、PAWS 和序列号安全检查成立。把这类开关当作“高并发优化默认项”，容易在 NAT、时间戳异常、跨路径流量里埋问题。

第五类问题是观测误导。`TIME-WAIT` 数量高不一定说明系统坏了。如果服务每秒处理大量短连接，TIME_WAIT 数量本来就会高。真正要看的是失败率、端口使用率、连接复用率、每秒新建连接数、`retransmits`、`listen` overflow、conntrack drop、NAT 分配失败、p99 connect latency。只把 TIME_WAIT count 作为告警，很容易误报。

结合 LogServe，高并发下最应该避免的是每个 task、每次 worker poll、每次 LLM request 都新建连接。正确做法是复用 gRPC channel、控制最大连接数、给重连加 jitter，并把任务幂等放在 shared log 层。否则 TIME_WAIT 会和 worker 重启、控制面 overload、logd 短连接写入叠在一起，症状看起来像“系统随机不稳定”。

面试里可以这样回答：

```text
TIME_WAIT 在高并发下会暴露短连接模型的成本：客户端临时端口耗尽、代理或服务端主动关闭导致 TIME_WAIT 转移、NAT/conntrack 状态表压力、连接抖动和 connect p99 上升。TIME_WAIT 数量高不一定是错，关键要结合每秒新建连接数、失败码、端口范围、连接复用率和中间设备状态一起看。
```

## Q045. TIME_WAIT 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

TIME_WAIT 最容易在故障场景里暴露“TCP 连接状态”和“应用语义状态”不是一回事。进程崩溃、机器重启、代理超时、客户端重试，都会让连接状态机和业务状态机错开。答这类问题时，不能只背四次挥手，要把旧报文、最后 ACK、端口复用和应用幂等一起说。

第一种边界是最后 ACK 丢失。主动关闭方进入 TIME_WAIT 后，如果最后一个 ACK 在路上丢了，对端会重传 FIN。TIME_WAIT 端收到后再发 ACK，并继续等待。这正是 TIME_WAIT 的核心价值之一。如果没有 TIME_WAIT，本端可能已经完全忘记连接，对端重传 FIN 时只能收到 RST 或得不到正确响应，关闭过程会变得不确定。

第二种边界是进程崩溃但内核还在。普通进程退出时，内核会关闭它持有的 socket，可能走 FIN，也可能因为未读数据或 linger 设置触发 RST。应用已经没了，不代表 TCP 状态立刻全消失；内核仍可能保留 TIME_WAIT。重启后的新进程如果马上绑定同一端口，服务端监听端口通常可以通过合理的 reuse 选项恢复，但主动连接的四元组复用仍要受 TIME_WAIT 和安全检查约束。

第三种边界是整机重启。机器重启会丢失内存中的 TIME_WAIT 状态。RFC 9293 里有 quiet time 的思想：如果一台主机丢失了它的序列号历史，理论上应避免马上用可能重叠的序列号重建连接，以免旧报文混入新连接。现代系统通常依赖随机 ISN、时间戳和 PAWS 等机制降低这个风险，但边界仍然存在。面试里说“重启后 TIME_WAIT 就无所谓了”太粗糙。

第四种边界是超时关闭。很多中间层不是等应用协议优雅结束，而是 idle timeout 到了就关连接。有的发 FIN，有的发 RST，有的只是静默丢弃状态。客户端看到的可能是 EOF、connection reset、broken pipe、read timeout 或下一次写才失败。TIME_WAIT 只覆盖正常关闭路径，不保证应用已经处理完请求，也不保证对端知道业务结果。

第五种边界是重试。连接关闭后客户端重试，如果上一条请求已经到达服务端但响应丢了，新连接上的重试可能造成重复提交。TIME_WAIT 不能解决这个问题，它只管 TCP 旧报文隔离，不管业务幂等。RPC、任务提交、支付、actor command 这类操作必须有 request id、idempotency key、sequence number 或事务语义。

第六种边界是用 RST 逃避等待。`SO_LINGER=0` 或异常退出可能让本端不进入常规 TIME_WAIT，但对端可能看到 reset，未读数据会被丢弃，HTTP/gRPC 层可能报告 `connection reset by peer`。这类做法只适合明确要中止连接的场景，不能作为高并发服务的常规优化。

结合 LogServe，control 崩溃后重启、worker 重连、SDK retry 都不能依赖 TCP 连接关闭结果判断任务是否成功。任务是否已经提交，要看 `TaskSubmitted` 是否写进 shared log；workflow step 是否重复，要看 `workflow_id + step_id + input_hash`；actor command 是否过期，要看 `command_seq` 和 epoch。TIME_WAIT 解决的是传输层旧包，不解决业务层重复执行。

面试里可以这样回答：

```text
TIME_WAIT 在故障场景里主要暴露四个边界：最后 ACK 丢失时要能回应重传 FIN；进程崩溃后内核可能仍保留关闭状态；整机重启会丢失 TIME_WAIT，需要依赖 ISN、timestamp、PAWS 等机制降低旧包风险；应用重试可能重复提交，TIME_WAIT 管不了业务幂等。它保护 TCP 四元组，不保护业务操作。
```

## Q046. TIME_WAIT 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

单个 TIME_WAIT 的性能瓶颈通常不在 CPU，也不在磁盘 I/O。TIME_WAIT 本身只是内核里一段轻量状态和一个定时器，保留四元组、序列号窗口、时间戳等必要信息，等到超时后回收。真正的问题出现在连接 churn 很高的时候：状态数量、端口占用、定时器管理、哈希表查找、conntrack/NAT 状态和锁竞争一起放大。

如果按资源类型拆，最先被打满的往往是端口和连接状态，而不是 CPU：

```text
端口：客户端到同一目标高频短连接时，临时端口被 TIME_WAIT 占住。
内存：大量 TIME_WAIT socket、conntrack entry、代理连接状态占用内存。
CPU：高频创建/销毁连接、定时器回收、包处理和状态查找会消耗 CPU。
锁竞争：极端短连接压测下，内核连接表、accept 路径、conntrack 可能竞争。
网络：不是 TIME_WAIT 本身耗带宽，而是短连接反复握手/挥手增加包量和 RTT 成本。
磁盘 I/O：通常无关，除非应用把连接事件同步写日志写到瓶颈。
```

有一个粗略公式很有用：如果客户端连接固定的远端 IP:port，临时端口可用数量是 `N`，TIME_WAIT 持续时间近似是 `T` 秒，那么只靠不断新建连接的稳定速率上限大约是 `N / T`。这不是严格协议公式，但能帮助理解为什么短连接模型很快撞墙。比如端口范围只有几万个，TIME_WAIT 又是几十秒，单目标新连接速率不可能无限增长。

CPU 瓶颈通常出现在更高层次。比如 TLS 握手、证书验证、HTTP/2 stream 管理、gRPC 编解码、应用线程池，这些往往比 TIME_WAIT 状态本身更重。TIME_WAIT 更多是把短连接设计推向端口和状态表上限，然后间接让 connect latency、重传、错误率升高。

网络瓶颈也要区分。TIME_WAIT 等待期间没有持续传输数据，不会吃掉业务带宽；但如果应用每次请求都新建连接，三次握手、四次挥手、TLS 握手、慢启动都会增加网络交互，p95/p99 延迟会受 RTT 放大。跨地域更明显，一个本来可以复用连接的 RPC，如果每次都重新握手，会把 RTT 成本直接放进请求路径。

结合 LogServe，shared log append、workflow 调度、actor replay、LLM mock/vLLM 调用才是主要业务路径。TIME_WAIT 通常不是单次请求的 CPU 瓶颈，但如果 SDK 或 worker 连接复用做坏了，它会让端口、NAT 和 control 的连接管理先出问题，表现为吞吐上不去、重连失败和 tail latency 抖动。

面试里可以这样回答：

```text
TIME_WAIT 的瓶颈通常不是磁盘 I/O，也不是单个状态的 CPU 成本，而是高连接 churn 下的端口占用、内核状态数量、定时器和连接表管理、conntrack/NAT 状态，以及极端情况下的锁竞争。网络成本主要来自短连接反复握手和关闭，不是 TIME_WAIT 等待本身在传数据。
```

## Q047. TIME_WAIT 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

测 TIME_WAIT 不能只跑一个“并发连接数很高”的压测。correctness test、stress test 和 benchmark 关注点不同。correctness test 看状态机和旧报文隔离是否对；stress test 看异常和资源压力下会不会崩；benchmark 看成本和容量边界在哪里。

Correctness test 应该先覆盖 TCP 关闭语义。简化实现也至少要测这些：

```text
主动关闭方在收到对端 FIN 并发送最终 ACK 后进入 TIME_WAIT。
TIME_WAIT 期间收到对端重传 FIN，会重新发送 ACK，而不是直接丢掉状态。
TIME_WAIT 期间旧数据段不能交给新连接或应用层。
TIME_WAIT 到期后状态被回收，四元组可以重新用于安全的新连接。
同时关闭时，双方状态转换符合预期。
RST、重复 ACK、乱序 FIN 不会错误破坏状态机。
开启 timestamp/PAWS 时，旧 timestamp 的段被拒绝。
```

如果做应用级测试，可以用网络模拟工具制造丢包、延迟和乱序。Linux `tc netem` 能模拟 delay、loss、duplicate、reorder。测试目标不是证明“TIME_WAIT 数量少”，而是证明最后 ACK 丢失、FIN 重传、旧报文延迟到达时，新连接不会收到旧连接的数据。

Stress test 关注资源压力。可以限制客户端临时端口范围，对同一个服务端 IP:port 发大量短连接，观察何时出现 `EADDRNOTAVAIL`、connect timeout、SYN retransmit、TIME_WAIT 表项增长和 NAT/conntrack drop。也可以让服务端主动关闭空闲连接，确认 TIME_WAIT 是否转移到服务端。再加上进程重启、服务滚动发布、代理 idle timeout、突发重连，模拟真实线上故障。

Benchmark 则要测可比较的指标。不要只报“QPS”。至少要拆出这些指标：

```text
每秒新建连接数和成功率。
connect p50/p95/p99 latency。
TIME_WAIT 数量、峰值和回收速度。
本地临时端口使用率。
CPU、内存、软中断、上下文切换。
conntrack/NAT/LB 连接表占用。
EADDRNOTAVAIL、EADDRINUSE、ECONNRESET、timeout 数量。
连接复用前后吞吐和尾延迟差异。
```

还要有对照组。比如每请求新建 TCP、HTTP keep-alive、HTTP/2/gRPC 复用、不同 idle timeout、不同端口范围、不同主动关闭方。这样才能判断瓶颈来自 TIME_WAIT、TLS 握手、应用处理还是中间层状态表。

结合 LogServe，可以设计三组测试：SDK 每次提交任务都新建连接，SDK 复用 gRPC channel，worker heartbeat/poll 使用长连接。然后比较 control 端连接数、TIME_WAIT 分布、任务提交 p99、worker reconnect 成功率和 shared log append throughput。这样得到的结论才和系统设计有关。

面试里可以这样回答：

```text
Correctness test 测状态机和旧包隔离，尤其是最后 ACK 丢失、FIN 重传、四元组复用和旧数据不进入新连接。Stress test 测短连接风暴、端口范围受限、NAT/conntrack、重启和代理超时下是否稳定。Benchmark 测每秒新建连接数、connect p99、TIME_WAIT 峰值、端口使用率、CPU/内存和错误码，并和连接复用方案做对照。
```

## Q048. 如果要求从零实现一个简化版 TIME_WAIT，你会先定义哪些不变量？

**回答：**

从零实现简化版 TIME_WAIT，第一步不是写定时器，而是定义不变量。TIME_WAIT 的价值就在于状态边界清楚：什么时候进入，期间接受什么，拒绝什么，什么时候释放。如果这些不变量不清楚，代码很容易在 happy path 下能跑，一遇到丢包、重传、RST 或四元组复用就出错。

我会先定义这些不变量：

```text
连接身份不变量：TIME_WAIT 记录必须绑定本地地址、本地端口、远端地址、远端端口和协议。
进入状态不变量：只有完成关闭握手、发出最终 ACK 的连接才进入 TIME_WAIT。
旧包隔离不变量：TIME_WAIT 期间，属于旧连接的数据段不能交给任何新连接或应用层。
FIN 重传不变量：TIME_WAIT 期间如果收到对端重传 FIN，必须能重发最终 ACK。
生命周期不变量：TIME_WAIT 必须至少保持一个明确的安全等待窗口，到期后才能释放资源。
四元组复用不变量：同一四元组只有在旧包风险被时间、序列号或 timestamp 检查消除后才能复用。
资源上限不变量：TIME_WAIT 表不能无限增长，超过上限时要有可解释的丢弃或保护策略。
崩溃边界不变量：进程崩溃不等于协议状态可随意忽略；整机重启要依赖 ISN/timestamp 策略降低风险。
```

简化实现里，TIME_WAIT 表项至少要保存四元组、最后 ACK 所需的序列号/确认号、最近看到的 timestamp、过期时间、连接方向和状态版本。收到报文时先按四元组查 TIME_WAIT 表。如果是对端重传 FIN，就重发 ACK；如果是旧数据或旧 ACK，丢弃；如果是新 SYN，要判断是否允许复用，不能简单地“有 SYN 就建新连接”。

状态机也要定义清楚。比如：

```text
ESTABLISHED
  -> FIN_WAIT_1
  -> FIN_WAIT_2
  -> TIME_WAIT
  -> CLOSED
```

如果同时关闭，路径可能经过 `CLOSING`。如果收到 RST，要按规范决定是否丢弃、关闭或保持 TIME_WAIT，不能让任意旧 RST 轻易杀掉 TIME_WAIT。RFC 1337 讨论的风险就在这里：过早销毁 TIME_WAIT 可能让旧重复段进入后续连接。

定时器不变量也不能随便写成“睡一会”。真实 TCP 用 MSL 和 2MSL 这类概念表达旧报文最大存活时间。简化系统可以把等待窗口配置化，但必须保证所有测试都基于这个窗口。到期回收要是幂等的，重复回收、并发查表和新连接创建不能互相踩。

结合 LogServe，如果只是为了课程或面试实现一个用户态简化 TCP，不需要实现完整拥塞控制、SACK、窗口扩大、所有异常 RST 规则，但必须保证业务层不会把“连接重建成功”当成“任务没有重复”。TIME_WAIT 是传输层状态，LogServe 的 task idempotency 和 actor epoch 仍然独立存在。

面试里可以这样回答：

```text
我会先定义四元组身份、进入 TIME_WAIT 的条件、最终 ACK 可重发、旧数据不交付、新连接不能不安全复用同一四元组、等待窗口到期才能回收、表项有资源上限这些不变量。实现上先做 TIME_WAIT 表和定时器，再处理 FIN 重传、旧包丢弃、RST 边界和安全复用，而不是一开始就追求减少 TIME_WAIT 数量。
```

## Q049. TIME_WAIT 的常见误用是什么，误用后通常会产生什么线上症状？

**回答：**

最常见的误用是把 TIME_WAIT 当成异常，然后第一反应就是“调小、清掉、绕过”。TIME_WAIT 多不一定错。高 QPS 短连接服务、主动关闭空闲连接的代理、滚动重启时的连接回收，都可能产生很多 TIME_WAIT。真正的问题是为什么连接这么短、谁在主动关闭、有没有连接复用、端口和中间设备状态是否撑得住。

第一种误用是每个请求都新建 TCP 连接，然后抱怨 TIME_WAIT 多。HTTP、gRPC、数据库、消息队列这类协议本来就应该复用连接。每请求建连会把 DNS、TCP handshake、TLS handshake、拥塞窗口预热、TIME_WAIT 全放进请求路径。线上症状通常是 p99 延迟高、connect timeout、临时端口耗尽、服务端 accept 抖动。

第二种误用是随手打开或修改 TIME_WAIT 相关内核参数。`tcp_tw_reuse` 不是万能优化开关。Linux 内核文档明确提示不要在没有技术专家建议时修改；它依赖协议安全条件，不能解决所有 NAT、代理和跨网络路径问题。历史上的 `tcp_tw_recycle` 更典型，它因为会破坏 NAT 后多客户端场景，后来已经从 Linux 移除。误用这类参数的症状往往不是稳定复现，而是某些客户端偶发连接失败、跨 NAT 访问异常、灰度环境正常但公网环境抖。

第三种误用是用 RST 关闭来逃避 TIME_WAIT。比如设置 `SO_LINGER=0`，或者应用在还有未读/未写数据时粗暴退出。短期看 TIME_WAIT 少了，长期看对端会出现 `connection reset by peer`、响应截断、gRPC `UNAVAILABLE`、HTTP 客户端读到 EOF 或 reset。更糟的是业务层可能已经执行了一半，客户端却以为失败并重试。

第四种误用是把 TIME_WAIT 和 CLOSE_WAIT 混着处理。CLOSE_WAIT 多要修应用 close 路径；TIME_WAIT 多要看短连接和主动关闭策略。用调 TIME_WAIT 的方法修 CLOSE_WAIT，连接泄漏不会消失；用查泄漏的方法修 TIME_WAIT，又会浪费时间。

第五种误用是健康检查和监控探针过于激进。每秒大量 TCP connect check 或 HTTP 短连接探测，会自己制造 TIME_WAIT 和端口压力。服务还没被真实流量打垮，先被探针和重试流量拖慢。线上表现是发布、扩缩容或网络抖动时，监控流量、业务重试和连接回收叠加，故障被放大。

第六种误用是把 TCP 连接生命周期当业务生命周期。连接断了不等于任务失败，连接还在不等于请求成功。TIME_WAIT 只是关闭后的传输层状态，不能替代 idempotency key、request id、事务、lease 或 epoch。

结合 LogServe，最危险的误用是 SDK 每次 task submit 都新建连接，worker 每次 poll 都新建连接，失败后所有 worker 无 jitter 地立刻重连。症状会是 control 端连接风暴、客户端端口耗尽、worker 心跳抖动、任务重复提交风险上升。修法不是先调 TIME_WAIT，而是复用 gRPC channel、限制并发重连、引入 backoff/jitter，并用 shared log 幂等控制重复。

面试里可以这样回答：

```text
TIME_WAIT 的常见误用包括把它当异常直接调小、每请求新建连接、滥用 tcp_tw_reuse、用 RST close 逃避等待、把它和 CLOSE_WAIT 混淆，以及用高频短连接健康检查制造连接风暴。线上症状通常是临时端口耗尽、connect timeout、connection reset、跨 NAT 偶发失败、p99 延迟抖动和重试风暴。
```

## Q050. TIME_WAIT 在单机和分布式环境中的语义有什么差异？

**回答：**

从 TCP 协议看，TIME_WAIT 的语义在单机和分布式环境里没有变：它仍然保护某个 TCP 端点上的四元组，等待旧报文过期，并在必要时回复对端重传的 FIN。差异在工程环境。单机里你看到的 TIME_WAIT 基本就是本机内核的连接状态；分布式环境里，中间的每一层都可能拆连接、改四元组、维护自己的状态表。

单机环境下，问题比较直接。客户端和服务端可能就在同一台机器、同一个 network namespace，端口范围、TIME_WAIT 数量、进程 close 行为都能用 `ss`、`lsof`、`netstat -s`、`/proc/sys/net/ipv4/*` 直接观察。你可以比较容易判断谁主动关闭、哪个进程产生短连接、端口是否耗尽。loopback 场景下 Linux 还有一些默认更激进的安全复用策略，但这不代表公网或跨 NAT 场景也安全。

分布式环境复杂得多。客户端连接的可能不是后端进程，而是本机 sidecar、节点 SNAT、Kubernetes Service、四层 LB、七层代理、云 NAT Gateway。每一段 TCP 都有自己的 TIME_WAIT：客户端到代理一段，代理到上游一段，LB 到后端一段。客户端看到 `connect()` 成功，只说明连上了某个中间层；后端实例可能还没收到连接。后端看到 TIME_WAIT 少，也不代表入口代理没有端口压力。

NAT 会改变问题形状。多个客户端共享一个出口 IP 时，外部服务端看到的源地址可能一样，只靠源端口区分不同连接。NAT 设备必须分配和回收端口映射，它自己的 TIME_WAIT 或连接保持策略会限制整体并发。应用机器上端口还够，NAT 网关端口池可能已经紧张。云环境里这类问题常表现为某一批节点到某个远端服务偶发连接失败。

负载均衡也会改变语义。L4 LB 可能保留 TCP 连接并把后端选择绑定在连接生命周期上；L7 代理可能复用客户端连接，再用自己的上游连接池访问后端。于是应用层的一次重试可能落到不同后端，TCP TIME_WAIT 只保护某一段连接，不保护“同一个业务会话仍在同一实例上”。如果应用把连接当作身份或 lease，就会在重连、迁移、代理复用时出问题。

跨地域还会放大关闭和重连成本。RTT 高，握手、挥手、TLS、重试都更贵；网络里延迟和乱序概率也更高。TIME_WAIT 的设计目标仍然是旧报文隔离，但用户感知到的是新连接慢、重试慢、连接池预热慢。一致性协议、lease、worker ownership 不能靠 TCP 连接存活来推断，必须用明确的租约时钟、epoch、fencing token 和日志提交位置。

结合 LogServe，单机实验里 TIME_WAIT 主要是本地资源和连接复用问题；未来如果走多节点部署，control、worker、logd、SDK 之间可能跨 LB、sidecar 和 NAT。那时 TCP 连接只表示某一段传输通了，不表示 worker 身份可信、actor ownership 仍有效、任务没有重复提交。LogServe 要继续把身份、幂等和恢复放在 shared log、heartbeat、epoch fencing 和 metadata replay 里。

面试里可以这样回答：

```text
TIME_WAIT 的协议语义在单机和分布式环境中一样，都是保护某个 TCP 四元组的关闭后隔离。差异是分布式环境有 LB、NAT、sidecar、conntrack 和代理连接池，每一段连接都有自己的 TIME_WAIT 和端口压力。TCP TIME_WAIT 不能表达业务身份、session、lease 或一致性状态；这些必须由应用层的 token、idempotency key、epoch 和日志来保证。
```

## Q051. TCP keepalive 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

TCP keepalive 的核心目标是发现长时间空闲连接是否已经失效。它关注的是“连接活性”，不是请求是否成功，也不是服务是否健康。RFC 9293 明确说 TCP 是面向连接的，但它本身不天然包含 liveness detection；RFC 1122 也把 keepalive 描述成可选机制，而且默认不能开得太激进，默认空闲间隔不得小于 2 小时。这个保守设计很有道理：一个空闲连接不一定需要被探测，探测报文也可能因为临时网络故障丢失，不能因为某一个 probe 没有回应就草率判死。

从目标分类看，它主要解决可用性和资源回收问题，间接服务正确性。比如客户端突然断电、移动网络切换、NAT 状态被回收、对端内核重启，本端 socket 可能还停在 `ESTABLISHED`。如果应用永远不读不写，就可能长期占着文件描述符、连接表、会话对象和应用层状态。keepalive 给内核一个机会，在空闲时发探测报文，如果多次没有 ACK 或收到 RST，就把连接标记为失败，让应用下一次 I/O 或轮询时能看到错误。

它不是性能优化。keepalive 会额外发包，会占用定时器和连接状态；在连接很多时，这些包和定时器会变成实际成本。它可能减少“下次业务请求才发现连接已死”的尾延迟，但这是故障检测延迟，不是正常路径吞吐提升。

安全性也不是它的目标。keepalive 不认证对端身份，不加密，不证明应用还在正常处理请求。收到 ACK 只能说明某个 TCP peer 或中间路径仍能回应这个 TCP 段。TLS、mTLS、应用 token、worker epoch、lease 这些仍要由上层处理。

可维护性方面，keepalive 让一些空闲连接问题更可见，但也容易制造误判。参数如果太短，临时网络抖动会被误判成断连；参数太长，又不能及时发现半开连接。更麻烦的是，不同层还有 HTTP keep-alive、HTTP/2 PING、gRPC keepalive、应用 heartbeat，名字相近但语义不同。

可以这样拆：

```text
核心目标：发现空闲 TCP 连接是否已经失效，避免半开连接无限占资源。
主要属性：可用性和资源回收，间接影响正确性。
不是目标：提升吞吐、认证身份、证明服务健康、替代应用 heartbeat。
关键边界：ACK 只证明 TCP 层还可达，不证明请求处理成功。
```

结合 LogServe，TCP keepalive 可以帮助 worker 到 control 的长连接、SDK 到 control 的 gRPC channel、control 到 logd 的连接尽早发现“底层连接已经没了”。但 worker 是否活着，不能只靠 TCP keepalive。LogServe 仍然需要 worker heartbeat、任务租约、log-first 事件、actor epoch fencing。TCP keepalive 只能告诉你这条传输通道可能坏了，不能告诉你某个 task 有没有执行完。

面试里可以这样回答：

```text
TCP keepalive 的核心目标是对长时间空闲的 TCP 连接做活性探测，避免对端崩溃、网络分区或中间设备丢状态后，本端还无限期保留 ESTABLISHED 连接。它主要服务可用性和资源回收，间接影响正确性；它不是性能优化，也不是安全认证，更不能替代应用层 heartbeat 或健康检查。
```

## Q052. TCP keepalive 的典型适用场景和不适用场景分别是什么？

**回答：**

TCP keepalive 适合长连接、低频交互、空闲时间可能很长的场景。典型例子是数据库连接池、消息队列消费者、RPC channel、SSH、控制面连接、内部服务的长连接池。这些连接平时可能没有业务数据，但一旦要用，希望尽量不要等到第一次真实请求写出去后才发现对端早就没了。

它也适合穿过 NAT、防火墙、四层负载均衡的连接。很多中间设备会清理长时间没有流量的连接状态。TCP keepalive 的探测包可以让连接不至于被过早当成 idle 状态清掉，或者至少让应用在中间设备已经丢状态后尽早发现问题。这里要注意，keepalive 不是“永远保活”的保证。中间设备可能过滤空 ACK，可能只认可应用层数据，也可能有自己的 idle timeout。

适用场景通常有这些特征：

```text
连接生命周期明显长于单次请求。
连接可能长时间空闲，但再次使用时希望提前知道是否已断。
连接两端都能承受周期性探测包。
对误判比较敏感，所以 probe 间隔和失败阈值需要保守配置。
应用层另有请求 deadline、heartbeat 或重试语义。
```

不适用场景也很明确。短连接不需要 TCP keepalive，因为连接还没空闲到 keepalive 触发就结束了。高频请求连接也通常不需要，业务数据本身就是活性信号。对延迟要求很高的请求，keepalive 也不能替代 per-request deadline。一个 RPC 卡住了，靠 2 小时后的 TCP keepalive 来发现，显然太慢。

它不适合当服务健康检查。TCP keepalive 只证明连接上的 TCP peer 有反应，不证明应用线程池有空，不证明数据库事务能提交，不证明 LogServe control 能调度任务。健康检查需要应用层语义，比如 HTTP `/healthz`、gRPC health checking、worker heartbeat、读取 log append 能力等。

它也不适合用来维持巨大规模的空闲连接而不做容量设计。百万长连接如果每条都设置很短的 keepalive，探测包、定时器和唤醒会变成系统负载。移动端场景还要考虑电量和无线网络唤醒，过于频繁的 keepalive 会把问题从服务端转移到客户端。

结合 LogServe，worker 到 control 的连接可以有 TCP keepalive 或 gRPC HTTP/2 PING，但 worker 存活应该以应用层 heartbeat 为准。SDK 提交任务的连接可以复用，但每次 task submit 必须有 deadline 和 idempotency key。logd 连接如果长时间 idle，keepalive 可以帮助发现断链，但 append 是否成功仍然以日志返回和持久化语义为准。

面试里可以这样回答：

```text
TCP keepalive 适合长连接、连接池、低频但长期保留的 RPC/数据库/控制面连接，尤其是路径中有 NAT、LB 或防火墙的情况。它不适合短连接，不适合替代请求 deadline，也不适合当应用健康检查。连接很多时还要小心探测间隔，否则 keepalive 本身会变成周期性负载。
```

## Q053. TCP keepalive 和相近概念最容易混淆的边界在哪里？

**回答：**

最容易混的是 TCP keepalive 和 HTTP keep-alive。HTTP keep-alive 指复用一条 TCP 连接承载多个 HTTP 请求，减少反复握手的成本。TCP keepalive 是内核在 TCP 连接空闲时发探测包，判断连接是否还活着。一个是连接复用策略，一个是空闲连接活性探测。名字像，层次完全不同。

第二个边界是 TCP keepalive 和应用层 heartbeat。应用 heartbeat 带业务语义，比如 worker 定期向 control 上报 `worker_id`、负载、模型缓存、当前 epoch；TCP keepalive 只发 TCP 探测包。收到 TCP ACK，不代表 worker 事件循环还能处理任务；应用 heartbeat 超时，也不一定说明 TCP 连接已经断，可能只是应用卡住了。两者都能帮助发现故障，但故障含义不同。

第三个边界是 TCP keepalive 和 gRPC keepalive。gRPC keepalive 基于 HTTP/2 PING，gRPC 官方文档明确说它和 health checking 是不同问题；keepalive 是连接层，health checking 是服务健康。HTTP/2 PING 会穿过 TCP 连接到 HTTP/2 对端，对 gRPC stream 更有意义；TCP keepalive 可能只在某一段 TCP 上生效。穿过 TCP load balancer 时，TCP_USER_TIMEOUT 可能只看到客户端到 LB 这一段，而 gRPC PING 能检测更靠近 HTTP/2 端点的问题。

第四个边界是 TCP keepalive 和 timeout/deadline。keepalive 是空闲连接探测；read timeout、write timeout、connect timeout、RPC deadline 是具体操作的时间预算。一个正在传输但卡住的请求，可能根本不处于“空闲到触发 keepalive”的状态。线上服务必须给请求设置 deadline，不能指望 keepalive 接管所有超时。

第五个边界是 TCP keepalive 和 TCP_USER_TIMEOUT。TCP_USER_TIMEOUT 控制已发送数据在多长时间内未被 ACK 或因零窗口无法发送时，连接应被强制关闭。它关注未确认数据的最长停留时间；keepalive 关注空闲连接探测。它们可以配合，但不是同一个开关。

第六个边界是 keepalive 和 TCP_NODELAY/Nagle。前者是活性探测，后者是小包合并策略。把“延迟高”归因给 keepalive 或把“连接假死”归因给 Nagle，都是层次错位。Nagle 影响写入小数据何时发出，keepalive 影响空闲时是否探测。

结合 LogServe，可以这样划线：TCP keepalive 判断 worker 到 control 的 TCP 连接是否还活；gRPC keepalive 判断 HTTP/2 channel 是否还通；worker heartbeat 判断 worker 进程是否还在参与调度；actor epoch 判断旧 worker 是否还有写 actor 状态的资格；task deadline 判断某次任务是否超时。少了这几个层次，排障时很容易把“连接活着”误读成“系统健康”。

面试里可以这样回答：

```text
TCP keepalive 是 TCP 层空闲连接探测，HTTP keep-alive 是 HTTP 连接复用，gRPC keepalive 是 HTTP/2 PING，应用 heartbeat 才有业务语义。keepalive 也不能替代 connect/read/write timeout 或 RPC deadline。收到 TCP keepalive ACK，只说明 TCP peer 仍能回应，不说明应用服务健康。
```

## Q054. TCP keepalive 在高并发场景下可能出现哪些隐藏问题？

**回答：**

高并发场景里，TCP keepalive 的隐藏问题不是单个探测包有多重，而是“连接数乘以探测频率”之后变成系统负载。每条连接一个定时器，每个周期一批探测包，每个探测包还要经过内核协议栈、网卡队列、NAT、LB、conntrack、对端协议栈。连接数到几十万、上百万时，过短的 keepalive 间隔会变成明显的 CPU、网络和中间设备压力。

第一类问题是探测风暴。如果所有连接在同一时间建立，keepalive 定时器又没有抖动，探测会成批发生。服务端看到的不是平滑的小流量，而是一阵一阵的 ACK/PING 峰值。对于 gRPC/HTTP/2 keepalive，官方文档也提醒不要把客户端 keepalive 配得太短，否则可能被服务端当成 `too_many_pings`，甚至有 DDoS 风险。

第二类问题是误判。高并发系统里，短暂丢包、队列拥塞、GC 停顿、宿主机 steal time、虚拟化网络抖动都可能让 keepalive probe 延迟。参数如果太激进，会把健康连接判死，引发重连。重连又带来握手、TLS、认证、连接池预热和 TIME_WAIT，最后形成“误判 -> 重连 -> 更拥塞 -> 更多误判”的循环。

第三类问题是中间设备行为不一致。NAT、LB、代理可能对 TCP keepalive、HTTP/2 PING、空 ACK、带数据的小包有不同策略。应用端以为自己发了 keepalive，中间层可能没有把它传到真正后端；也可能中间层回应了某些 TCP 层信号，让客户端误以为后端仍然可达。排查时必须知道 keepalive 到底终止在哪一层。

第四类问题是连接生命周期管理被掩盖。一个系统如果有大量永久空闲连接，keepalive 会让它们继续存活，但这不代表这些连接有价值。连接池没有最大空闲数、连接寿命太长、客户端泄漏 channel、服务端没有 idle close，都会让 keepalive 成为“保留无用连接”的工具。

第五类问题是观测口径混乱。TCP keepalive 失败可能表现为 `ETIMEDOUT`、`ECONNRESET`、read EOF、gRPC `UNAVAILABLE`、HTTP/2 GOAWAY，取决于层次和实现。只看应用错误码，不看 socket 选项、tcp info、LB 日志、HTTP/2 PING、worker heartbeat，很难判断是真断链还是服务端拒绝过密探测。

结合 LogServe，高并发时不应该让每个 SDK、每个 worker 都用相同短周期向 control 打 keepalive。更稳的策略是：长连接复用，应用 heartbeat 有 jitter，连接池有 max idle/max lifetime，RPC 有 deadline，重连有 backoff。TCP keepalive 只作为底层兜底，不作为调度系统的主时钟。

面试里可以这样回答：

```text
TCP keepalive 在高并发下会把连接数乘以探测频率变成周期性负载，可能造成探测风暴、误判断连、重连风暴、NAT/LB 状态压力和观测混乱。参数不能只看单连接，要看全局连接数、探测周期、失败阈值、中间设备 idle timeout，以及应用层 heartbeat 和 deadline 的配合。
```

## Q055. TCP keepalive 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

崩溃和重启场景里，TCP keepalive 最容易暴露“对端没有发 FIN/RST”这类半开连接问题。比如客户端机器断电、容器被强杀、移动网络切换，另一端内核可能收不到任何关闭信号，连接仍停在 `ESTABLISHED`。如果应用不再发送业务数据，也没有 keepalive 或 heartbeat，这个连接可以挂很久。

对端进程崩溃但机器没崩，情况又不一样。内核通常会清理进程持有的 socket，可能发 FIN 或 RST。本端下一次读写能更快看到 EOF 或 reset。TCP keepalive主要处理的是“本端没有业务 I/O，所以一直不知道”的情况，不是替代正常错误传播。

整机重启后，如果旧连接的对端还向原四元组发送 keepalive probe，新机器可能已经没有对应 socket 状态，会回 RST。本端收到 RST 后连接失败。这个行为对 TCP 层是合理的，但对应用层不代表请求是否执行成功。请求可能已经在旧进程崩溃前写入日志，也可能完全没到达。

超时场景要区分 idle timeout、keepalive timeout 和 request deadline。LB idle timeout 是中间设备清理空闲连接；TCP keepalive timeout 是 probe 多次失败后的本地判定；RPC deadline 是某次调用的业务时间预算。它们可能同时存在，谁先触发决定了应用看到的错误。只配 keepalive 不配 deadline，会导致请求级卡顿仍然没有清晰边界。

重试场景更需要小心。keepalive 发现连接坏了，通常会导致连接关闭，然后应用或 RPC 框架重连。重连不等于原操作没有执行。对于非幂等操作，不能因为连接断了就盲目重试。需要 request id、idempotency key、事务状态、日志提交位置来判断。

还有一个边界是 transient failure。RFC 1122 早就提醒，ACK-only 段不由 TCP 可靠传输保证，keepalive 机制不能把某一次 probe 没有响应就解释成死连接。现代实现通常用“空闲多久后开始探测、探测几次、每次间隔多久”来降低误判。阈值越短，故障发现越快，误杀概率也越高。

结合 LogServe，control 重启、worker 被 kill、网络短抖、SDK retry 都不能只看 TCP keepalive。任务是否提交成功要查 `TaskSubmitted`；workflow step 是否已经完成要看 log replay；actor 命令是否有效要看 `command_seq` 和 epoch。TCP keepalive 只能把坏连接尽早暴露给上层，不能替上层决定重试是否安全。

面试里可以这样回答：

```text
TCP keepalive 在崩溃和重启时能发现半开连接，但只能说明传输层连接坏了。对端崩溃可能返回 FIN/RST，整机重启可能让旧连接收到 RST，网络分区可能只是 probe 超时。重试是否安全不由 keepalive 决定，要靠应用层幂等键、事务、日志位置或 epoch。
```

## Q056. TCP keepalive 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

单条连接的 TCP keepalive 成本很低，通常不是性能瓶颈。它只是空闲一段时间后发一个很小的探测段，对端回 ACK。真正的瓶颈来自规模：连接数很多、探测间隔很短、所有连接定时器集中触发、中间设备还要维护状态。

按资源拆，主要是这些地方：

```text
CPU：内核定时器、协议栈收发包、软中断、conntrack/LB 状态查找。
内存：每条长连接本来就占 socket、缓冲区和应用会话对象，keepalive 会延长其存活。
锁竞争：极端连接数下，定时器、连接表、网络队列、conntrack 可能出现竞争。
网络：探测包本身小，但连接数乘以频率后会成为稳定背景流量。
磁盘 I/O：通常无关，除非应用把每次 keepalive 或断连事件同步写日志。
```

更常见的瓶颈其实是参数错配带来的系统性抖动。比如 LB idle timeout 是 60 秒，客户端 TCP keepalive 默认 2 小时，那 keepalive 根本保不住这条连接；下一次请求才发现连接已经被中间层丢了。反过来，如果客户端每 10 秒发 PING，服务端策略不允许这么频繁，就可能触发 GOAWAY 或连接关闭。

性能评估不能只看 keepalive 包量，还要看失败后的重连成本。一次误判断连可能触发 TCP handshake、TLS handshake、HTTP/2 preface、认证、连接池重建、gRPC channel 状态转换。大量连接同时误判时，重连成本会远远超过 keepalive 包本身。

还有一个现实问题是移动端和跨地域链路。移动设备上，频繁 keepalive 会唤醒无线链路，影响电量；跨地域链路上，RTT 和丢包会让短 timeout 更容易误判。服务端看起来只是多收了几个包，客户端可能付出更高代价。

结合 LogServe，TCP keepalive 不太可能成为单次 task 的 CPU 瓶颈。真正要警惕的是 worker 数量扩大后，所有 worker 同周期 heartbeat、gRPC PING、TCP keepalive 和 reconnect 叠加。应把探测和 heartbeat 做 jitter，并用应用层 backpressure 控制重连和任务拉取。

面试里可以这样回答：

```text
TCP keepalive 的单连接成本很低，瓶颈通常来自连接数规模和参数配置。CPU 花在定时器、软中断和连接表处理上；内存来自长期保留连接；网络成本来自连接数乘以探测频率；真正危险的是误判后触发重连风暴，重连成本往往比 keepalive 探测包本身高得多。
```

## Q057. TCP keepalive 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

TCP keepalive 的测试要分层。correctness test 证明状态和判断规则对；stress test 证明在大量连接和故障下不会误伤系统；benchmark 量化参数组合的成本。把这三类混在一起，只能得到“跑了一次没挂”的结论。

Correctness test 先测协议行为和配置边界：

```text
默认关闭或按配置开启 keepalive。
连接空闲达到 keepalive idle 后才开始发送 probe。
收到 ACK 后连接保持 ESTABLISHED，并重置探测失败计数。
连续 probe 未收到响应达到阈值后，连接返回可观测错误。
收到 RST 时连接失败，并把错误传给应用。
有正常业务数据收发时，不应错误触发空闲探测。
单个 probe 丢失不能立即判死，需要符合失败阈值。
```

如果是在 Go 或应用框架里测，还要验证 API 映射是否正确。比如 `SetKeepAlive(true)` 是否真正打开 SO_KEEPALIVE，`SetKeepAlivePeriod` 或 `SetKeepAliveConfig` 是否符合预期，gRPC keepalive 的 PING、timeout、without calls 是否被服务端接受。不能只看代码里调用了设置函数。

Stress test 重点测规模和异常。可以建立大量空闲连接，给不同 keepalive 周期，观察 CPU、软中断、包量、conntrack、LB 连接表和错误率。再注入故障：丢弃 ACK、drop 单方向流量、重启对端、杀进程、清理 NAT 状态、让 LB idle timeout 小于 keepalive idle。目标是看系统能否正确区分短抖、真正断链和中间层清状态。

Benchmark 要给出可比较指标：

```text
连接数规模：1k、10k、100k、更多。
探测配置：idle、interval、probe count、timeout。
背景包量：packets/s、bytes/s、突发峰值。
资源成本：CPU、内存、软中断、上下文切换。
故障发现时间：从断链到应用可见错误的 p50/p95/p99。
误判率：短暂丢包或延迟下健康连接被关闭的比例。
重连成本：重连成功率、TLS/gRPC 建连延迟、连接池恢复时间。
```

还要有对照组：不开 TCP keepalive、只用应用 heartbeat、只用 gRPC HTTP/2 PING、两者都开、不同 LB idle timeout。这样才能判断哪个层次真正解决问题。

结合 LogServe，可以测 worker 到 control 的长连接：模拟 worker 网络断开但进程不退出、control 重启、LB idle timeout、worker heartbeat 卡住。验证 TCP keepalive 发现的是连接断链，应用 heartbeat 发现的是 worker 逻辑失活，任务恢复靠 shared log replay 和 redelivery，而不是混成一个信号。

面试里可以这样回答：

```text
Correctness test 测 idle 后探测、ACK 后保活、连续失败后报错、单个 probe 丢失不判死。Stress test 测大量空闲连接、丢包、单向断链、NAT/LB 清状态和重启。Benchmark 测包量、CPU、内存、故障发现时间、误判率和重连成本，并与应用 heartbeat、gRPC PING 做对照。
```

## Q058. 如果要求从零实现一个简化版 TCP keepalive，你会先定义哪些不变量？

**回答：**

从零实现简化版 TCP keepalive，第一步是定义它只能做什么，不能做什么。keepalive 只能在连接空闲时探测对端 TCP 是否还能回应，不能判断业务健康，不能替代请求超时，也不能因为一次 probe 没回就杀连接。

我会先定义这些不变量：

```text
启用不变量：keepalive 默认关闭，必须由连接或应用显式开启。
空闲不变量：只有在连接超过 idle 时间没有收到数据或 ACK 后才发 probe。
探测不变量：probe 必须构造成能诱发对端 ACK 的 TCP 段，不应携带业务语义。
失败阈值不变量：单个 probe 失败不能判死，必须连续失败达到配置阈值。
恢复不变量：任意合法 ACK 或业务数据都应证明连接仍可用，并清理失败计数。
错误传播不变量：判死后必须让应用后续读写得到明确错误。
资源不变量：定时器和连接状态不能无限增长，关闭连接要取消探测。
分层不变量：keepalive 状态不能直接修改应用 session、lease、任务状态。
```

简化状态机可以这样写：

```text
ESTABLISHED_ACTIVE
  -> ESTABLISHED_IDLE
  -> PROBING
  -> ESTABLISHED_ACTIVE | DEAD
```

`ESTABLISHED_ACTIVE` 表示最近有数据或 ACK；超过 idle 进入 `ESTABLISHED_IDLE`；发出 probe 后进入 `PROBING`；收到 ACK 回 active；连续失败到阈值进入 `DEAD`，关闭连接并通知上层。

实现时要注意时间来源和抖动。所有连接同一时刻探测会制造峰值，所以定时器最好有 jitter 或分桶调度。探测间隔和失败阈值要能配置，而且要有最小值保护，避免调用方把 keepalive 配成高频心跳。gRPC 文档里对过短 keepalive 的提醒，背后就是这个问题。

还要定义和正常 I/O 的关系。正在发送数据、还有未 ACK 数据、正在重传、对端零窗口，这些和 keepalive 的关系要清楚。真实内核里还有 TCP_USER_TIMEOUT、重传定时器、零窗口探测等机制，简化版可以不全做，但不能让 keepalive 和这些机制互相打架。

结合 LogServe，如果在用户态长连接框架里实现类似机制，我会把它命名成 transport ping，而不是 worker heartbeat。transport ping 只负责关闭坏连接；worker heartbeat 单独携带 worker_id、epoch、负载和模型缓存信息；任务重试和 actor fencing 仍然只看 shared log 的状态。

面试里可以这样回答：

```text
我会先定义默认关闭、空闲后才探测、probe 不带业务语义、单次失败不判死、ACK 或业务数据清零失败计数、判死后向应用传播错误、关闭连接取消定时器、不能修改应用状态这些不变量。然后再做定时器、jitter、失败阈值和状态机。
```

## Q059. TCP keepalive 的常见误用是什么，误用后通常会产生什么线上症状？

**回答：**

第一种误用是把 TCP keepalive 当请求超时。一个 RPC 写出去了，服务端 handler 卡住，TCP 连接仍然可以正常 ACK，keepalive 不会告诉你“这个请求已经超时”。线上症状是调用长时间挂起、线程或 goroutine 堆积、连接看起来还活着，但业务没有进展。正确做法是每个请求都设置 deadline。

第二种误用是把 TCP keepalive 当健康检查。收到 ACK 只能说明 TCP 栈回应了探测，不说明应用能处理请求。服务可能死锁，线程池满，队列堆积，数据库不可用，TCP keepalive 仍然成功。线上表现是健康检查显示连接活着，但真实请求失败或超时。

第三种误用是把 keepalive 间隔设得很短。连接数少时看不出问题，连接数上来后会出现周期性包峰值、CPU 抖动、LB 或服务端拒绝过密 PING、gRPC GOAWAY、移动端耗电、误判断连。这个问题通常在压测或灰度扩容后才暴露。

第四种误用是只依赖系统默认值。Linux 默认 keepalive 时间通常是 2 小时，探测 9 次、间隔 75 秒，开始探测后还要约 11 分钟才判死。这个配置对“清理死连接”很保守，对很多 RPC 系统太慢。如果 LB idle timeout 是 60 秒，默认 TCP keepalive根本来不及保活。

第五种误用是多层 keepalive 叠加。TCP keepalive、HTTP/2 PING、应用 heartbeat、连接池 idle check、LB health check 全开，但没有统一预算。结果是网络包很多，故障信号重复，排障时不知道哪个层先断开连接。

第六种误用是连接断开后盲目重试。keepalive 失败只说明连接坏了，不说明上一条操作没成功。非幂等请求如果直接重试，可能导致重复写入、重复提交、重复 actor command。这个错误在任务队列、支付、日志写入、控制面命令里都很危险。

结合 LogServe，常见错误会是：worker 没有应用 heartbeat，只开 TCP keepalive；SDK 没有 task deadline，只等连接失败；control 根据 TCP 连接断开直接判定任务失败并重派。更稳的做法是 worker heartbeat 判断 worker 逻辑活性，task lease 和 shared log 判断任务状态，TCP keepalive 只帮助清理坏连接。

面试里可以这样回答：

```text
常见误用包括把 TCP keepalive 当请求超时、当健康检查、把间隔设得过短、完全依赖 2 小时默认值、多层 keepalive 无预算叠加、断连后盲目重试。线上症状通常是请求挂死、健康误判、周期性 CPU/包量峰值、GOAWAY/too_many_pings、连接误杀和重复提交。
```

## Q060. TCP keepalive 在单机和分布式环境中的语义有什么差异？

**回答：**

TCP keepalive 的协议语义在单机和分布式环境中一样：对某条 TCP 连接做空闲探测。差异在于，分布式环境里“这条 TCP 连接”可能只是整条业务路径的一段。客户端到 sidecar 是一条 TCP，sidecar 到 LB 是另一条，LB 到后端又是一条。每一段都可能有自己的 keepalive、idle timeout 和连接池。

单机环境里，keepalive 比较直观。你能看到本机 socket、进程、文件描述符、TCP 状态，问题多半是进程是否关闭连接、是否有半开连接、keepalive 参数是否生效。loopback 或同机多进程里，中间设备少，TCP ACK 和应用进程之间的关系更近，但仍然不能把 ACK 当应用健康。

分布式环境里，keepalive 的含义会被中间层切开。TCP keepalive 可能只到达本机 sidecar 或四层 LB；HTTP/2 PING 可能到达代理的 HTTP/2 端点；应用 heartbeat 才到达真正的业务服务。云 NAT、Kubernetes Service、service mesh、API gateway 都可能改变连接生命周期。客户端看到连接活着，不代表请求能到后端实例。

跨地域场景更要保守。RTT 高、偶发丢包多，短 keepalive timeout 更容易误判。移动网络和多路径切换下，TCP 连接可能静默失效，keepalive 有价值，但参数太激进会带来耗电和重连抖动。

一致性语义也不同。单机里连接断开可能和进程退出高度相关；分布式里连接断开只是网络事实，不代表 worker lease 到期，不代表 leader 失效，不代表 actor owner 必须释放。连接活着也不代表 lease 没过期。lease、epoch、fencing token、日志提交位置要独立维护。

结合 LogServe，单机实验中 keepalive 主要帮助发现 worker/control/logd 之间的坏连接；未来多节点部署里，control 前可能有 LB，worker 可能通过 NAT 或 sidecar 连接。那时 TCP keepalive 只能作为 transport 信号，不能直接驱动 workflow 状态迁移。系统应该以 shared log、heartbeat、lease/epoch 和 replay 为准。

面试里可以这样回答：

```text
TCP keepalive 的协议语义不变，但分布式环境里它只证明某一段 TCP 连接可达。LB、NAT、sidecar、代理连接池会把端到端路径切成多段。连接活着不等于后端健康，连接断开也不等于业务操作失败；分布式系统仍要用 heartbeat、lease、epoch、idempotency key 和日志状态表达业务语义。
```

## Q061. Nagle 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

Nagle 算法的核心目标是减少小 TCP 段，缓解 tinygram 造成的网络低效和拥塞。RFC 896 讨论的背景很具体：应用一次只发一个字符时，可能形成 1 字节数据加 40 字节 TCP/IP 头的包，网络开销非常高。在慢链路或拥塞网络里，大量小包会占用交换节点和网关资源，增加排队、丢包和重传。

所以它主要解决性能问题，而且是网络效率和拥塞层面的性能。Nagle 的基本规则可以概括成：如果连接上已有未确认数据，就先缓存新的小数据，直到之前的数据被 ACK，或者积累到足够发送一个 MSS 大小的段。RFC 9293 也把它放在 segmentation 相关章节，要求 TCP 实现应支持 Nagle，同时必须允许应用在单个连接上禁用它。

它不解决 TCP 正确性。Nagle 开不开，TCP 都要保证有序、可靠、去重、重传。Nagle 只是改变“小块应用写入”何时变成网络上的 TCP segment。它不改变字节流内容，也不改变 ACK、窗口和序列号的基本语义。

它也不是安全机制。Nagle 不认证、不加密、不防攻击。某些流量形态下，小包更少可能让链路压力小一点，但这不是安全边界。

可维护性方面，Nagle 反而经常制造误解。应用开发者看到一次 `Write` 没有立刻变成一个包，容易以为网络卡了；RPC 开发者看到小请求延迟抖动，可能不知道是 Nagle 和 delayed ACK 组合造成的。Go 的 `TCPConn.SetNoDelay` 默认是 true，说明很多现代应用更偏向低延迟发送，让框架自己做 batching。

可以这样拆：

```text
核心目标：减少小包，提升链路利用率，缓解 tinygram 带来的拥塞。
主要属性：性能，偏网络效率而不是单次低延迟。
不解决：可靠性、消息边界、安全认证、应用层批处理。
关键代价：小写入可能被延迟，尤其和 delayed ACK 组合时更明显。
```

结合 LogServe，Nagle 不是任务正确性的基础。workflow step、actor command、LLM request 是否正确，取决于 shared log、幂等和状态机。Nagle 只会影响 SDK/control/worker/logd 之间小消息的发送时机。如果每个事件都是小包同步请求，Nagle 可能降低包量，也可能增加交互延迟；更合理的是让 gRPC/HTTP/2 和应用层 batching 控制消息粒度。

面试里可以这样回答：

```text
Nagle 的核心目标是性能，具体是减少小 TCP 段，避免大量 tinygram 造成网络低效和拥塞。它不改变 TCP 的可靠性和顺序语义，也不提供安全性。代价是小写入可能等待未确认数据的 ACK，和 delayed ACK 组合时可能让交互式请求出现额外延迟。
```

## Q062. Nagle 的典型适用场景和不适用场景分别是什么？

**回答：**

Nagle 适合小写入很多、对单次交互延迟不极端敏感、但希望减少包量的场景。早期 Telnet 是典型背景：每次键盘输入可能只有一个字节，如果立刻发包，头部开销很高。现在类似的场景包括一些低速链路、嵌入式设备、跨 WAN 的小消息流、批量日志控制消息、自己没有做 batching 的简单协议。

适用场景通常有这些特征：

```text
应用频繁写入很小的数据片段。
允许把多个小写入合并成较少的 TCP 段。
吞吐和链路效率比单次毫秒级响应更重要。
协议不依赖每个 write 立刻被对端看到。
应用层没有更清楚的 batching 或 framing 策略。
```

不适用场景是低延迟交互式协议。比如游戏输入、远程桌面、交互式 shell、实时控制、请求响应式 RPC、小包 ping-pong 协议。这类系统希望写入尽快发出去，而不是等上一个小段 ACK。Linux `TCP_NODELAY` 和 Go `SetNoDelay(true)` 就是为了让应用能关闭 Nagle，Go 的文档还明确说默认是 no delay，数据会尽快发送。

它也不适合已经有应用层 batching 的协议栈。比如 gRPC over HTTP/2、本身有 frame、flush、stream flow control；数据库协议可能有自己的 packet/framing；日志系统可以按 batch append 聚合。让 Nagle 再在底层猜测是否合并，可能只会增加不可控延迟。

要注意，关闭 Nagle 不等于永远每次 `write()` 一个包。内核发送缓冲、GSO/TSO、应用缓冲、TLS record、HTTP/2 frame、调度时机都会影响实际包形态。`TCP_NODELAY` 只是告诉 TCP 不要因为 Nagle 规则等 ACK。

结合 LogServe，如果控制面向 logd 写 append record，可以在应用层做 batch，按记录数、字节数或时间窗口 flush；这样比依赖 Nagle 更可控。worker heartbeat、task poll、actor command 这类低延迟控制消息通常更适合禁用 Nagle 或依赖 Go/gRPC 默认低延迟行为。

面试里可以这样回答：

```text
Nagle 适合小写入多、能接受合并延迟、希望减少包量的连接；不适合交互式、低延迟、请求响应式 RPC 或已经有应用层 batching/framing 的协议。关闭 Nagle 不是反模式，关键看应用是要链路效率，还是要每个小请求尽快发出。
```

## Q063. Nagle 和相近概念最容易混淆的边界在哪里？

**回答：**

Nagle 最容易和 delayed ACK 混在一起。Nagle 是发送端算法，发现有未确认数据时，可能把后续小写入先缓存；delayed ACK 是接收端策略，可能延迟发送 ACK，看看能不能和反向数据一起捎带。两个机制单独看都合理，组合到小请求-小响应协议里，可能互相等待，制造几十到几百毫秒的额外延迟。

第二个边界是 Nagle 和 TCP_NODELAY。`TCP_NODELAY` 不是“发送更快的协议”，只是禁用 Nagle，让小数据尽快进入发送路径。它不会关闭拥塞控制，不会绕过接收窗口，也不会保证对端马上读到。链路拥塞、发送缓冲满、TLS 层缓冲、应用调度仍然可能造成延迟。

第三个边界是 Nagle 和 TCP_CORK。Nagle 是内核基于未 ACK 数据自动合并小段；TCP_CORK 是应用显式告诉内核先别发 partial frame，等应用清除 cork 或超时后再发。Linux tcp(7) 提到 TCP_CORK 适合 header + sendfile 这类吞吐优化。Nagle 是自动的，CORK 更像手动 flush 控制。

第四个边界是 Nagle 和应用层 batching。应用 batching知道业务边界，比如“攒 100 条 log record 或 1 ms flush”；Nagle 只看到字节流和 ACK 状态，不知道任务、日志记录、RPC frame。对工程系统来说，应用层 batching 通常更可解释，也更容易压测和调参。

第五个边界是 Nagle 和 MSS/MTU。MSS 是单个 TCP segment 最大 payload 大小；MTU 是链路层包大小约束。Nagle 会倾向等到能发满 MSS 或前一个小包被 ACK，但它不是路径 MTU 发现，也不会解决分片问题。

第六个边界是 Nagle 和拥塞控制。RFC 896 的动机和拥塞有关，但 Nagle 不是现代意义上的拥塞控制算法。CUBIC、BBR、Reno 这类算法控制拥塞窗口和发送速率；Nagle 主要减少 tinygram 数量。把 Nagle 当成“限流器”不准确。

结合 LogServe，最容易出错的是把一次 task submit 的延迟归因给“网络慢”，却没看 TCP_NODELAY、delayed ACK、TLS record、HTTP/2 frame、gRPC flush 和应用批处理。排查小请求尾延迟时，要抓包或看 tcp_info，确认延迟发生在应用队列、TLS、TCP 发送，还是对端处理。

面试里可以这样回答：

```text
Nagle 是发送端小包合并，delayed ACK 是接收端延迟确认，TCP_NODELAY 是禁用 Nagle，TCP_CORK 是应用显式延迟 partial frame，应用 batching 是业务层聚合。它们都会影响小包和延迟，但层次不同。Nagle 也不是 MSS、MTU 或拥塞控制本身。
```

## Q064. Nagle 在高并发场景下可能出现哪些隐藏问题？

**回答：**

高并发下，Nagle 的问题通常不是“它会不会降低吞吐”，而是它会让很多小请求的延迟分布变得不直观。单连接上一个小写入等待 ACK，看起来只是几十毫秒；成千上万条连接、请求响应协议、delayed ACK、TLS、HTTP/2 和调度队列叠加后，p99 可能明显变差。

第一类隐藏问题是小请求尾延迟。很多 RPC 请求本身很小，比如一个控制命令、一个 heartbeat、一个 poll、一个 metadata update。如果连接上有未确认小段，Nagle 可能暂缓后续小写入。接收端 delayed ACK 又可能等一小段时间再 ACK。结果是请求处理逻辑并不慢，网络也没丢包，但客户端看到额外等待。

第二类问题是多路复用下的放大。HTTP/2/gRPC 在一条 TCP 连接上跑多个 stream。底层 TCP 的发送策略会影响所有 stream 的字节。虽然现代 gRPC/Go 往往默认禁用 Nagle，但如果某些语言、代理、sidecar 或自研协议没有禁用，单连接上的小包等待可能影响多个逻辑请求。

第三类问题是观测困难。应用日志只看到 `Write()` 返回了，不代表包已经发到线上；抓包看到包晚发，又不一定是 Nagle，也可能是 TLS record buffering、HTTP/2 flow control、应用 flush、send buffer、调度延迟。高并发系统里这些因素混在一起，误判很常见。

第四类问题是和应用层 batching 冲突。应用已经按 1 ms 或 64 KB 做 batch，Nagle 再额外延迟，收益很小，延迟却更难解释。反过来，如果应用完全不做 batching，只靠 Nagle 合并，吞吐可能能上去，但业务边界和 flush 时机不可控。

第五类问题是连接数和包量的权衡。关闭 Nagle 会降低小请求延迟，但可能增加包数、CPU、软中断、网卡队列压力和 LB/conntrack 处理量。高并发服务不能简单地说“永远 TCP_NODELAY”。更稳的是按协议特征选择：控制面小请求通常 no delay；批量传输或日志 append 可以应用层批处理；大流量数据传输让 TCP 自己按 MSS 发送。

结合 LogServe，worker heartbeat、poll、actor command 这类小控制消息更怕额外等待；shared log 批量 append 更需要吞吐和 fsync 策略。LogServe 的 Go/gRPC 路径通常不应该依赖 Nagle 做性能优化，而应该在 log append、workflow 调度和模型请求层面明确 batch、deadline 和 backpressure。

面试里可以这样回答：

```text
Nagle 在高并发下的隐藏问题主要是小请求 p99 抖动，尤其和 delayed ACK、HTTP/2 多路复用、TLS buffering、应用 flush 混在一起时很难定位。关闭 Nagle 可以降低控制面小消息延迟，但会增加包量和 CPU/软中断压力。高并发系统应由应用层明确 batching 和 deadline，而不是让 Nagle 猜业务边界。
```

## 参考资料

- IETF RFC 9293, [Transmission Control Protocol (TCP)](https://www.rfc-editor.org/rfc/rfc9293.html)
- IETF RFC 1337, [TIME-WAIT Assassination Hazards in TCP](https://www.rfc-editor.org/rfc/rfc1337.html)
- IETF RFC 6191, [Reducing the TIME-WAIT State Using TCP Timestamps](https://www.rfc-editor.org/rfc/rfc6191.html)
- IETF RFC 7323, [TCP Extensions for High Performance](https://www.rfc-editor.org/rfc/rfc7323.html)
- IETF RFC 1122, [Requirements for Internet Hosts - Communication Layers](https://www.rfc-editor.org/rfc/rfc1122.html)
- IETF RFC 896, [Congestion Control in IP/TCP Internetworks](https://www.rfc-editor.org/rfc/rfc896.html)
- IETF RFC 5681, [TCP Congestion Control](https://www.rfc-editor.org/rfc/rfc5681.html)
- IETF RFC 6298, [Computing TCP's Retransmission Timer](https://www.rfc-editor.org/rfc/rfc6298.html)
- IETF RFC 2018, [TCP Selective Acknowledgment Options](https://www.rfc-editor.org/rfc/rfc2018.html)
- IETF RFC 9110, [HTTP Semantics](https://www.rfc-editor.org/rfc/rfc9110.html)
- IETF RFC 9112, [HTTP/1.1](https://www.rfc-editor.org/rfc/rfc9112.html)
- IETF RFC 9113, [HTTP/2](https://www.rfc-editor.org/rfc/rfc9113.html)
- IETF RFC 9114, [HTTP/3](https://www.rfc-editor.org/rfc/rfc9114.html)
- IETF RFC 9000, [QUIC: A UDP-Based Multiplexed and Secure Transport](https://www.rfc-editor.org/rfc/rfc9000.html)
- IETF RFC 9001, [Using TLS to Secure QUIC](https://www.rfc-editor.org/rfc/rfc9001.html)
- IETF RFC 9002, [QUIC Loss Detection and Congestion Control](https://www.rfc-editor.org/rfc/rfc9002.html)
- IETF RFC 8446, [The Transport Layer Security (TLS) Protocol Version 1.3](https://www.rfc-editor.org/rfc/rfc8446.html)
- IETF RFC 5280, [Internet X.509 Public Key Infrastructure Certificate and CRL Profile](https://www.rfc-editor.org/rfc/rfc5280.html)
- IETF RFC 1035, [Domain Names - Implementation and Specification](https://www.rfc-editor.org/rfc/rfc1035.html)
- IETF RFC 2181, [Clarifications to the DNS Specification](https://www.rfc-editor.org/rfc/rfc2181.html)
- IETF RFC 2308, [Negative Caching of DNS Queries](https://www.rfc-editor.org/rfc/rfc2308.html)
- IETF RFC 7239, [Forwarded HTTP Extension](https://www.rfc-editor.org/rfc/rfc7239.html)
- Linux man-pages, [tcp(7)](https://man7.org/linux/man-pages/man7/tcp.7.html)
- Linux man-pages, [socket(7)](https://man7.org/linux/man-pages/man7/socket.7.html)
- Linux man-pages, [select(2)](https://man7.org/linux/man-pages/man2/select.2.html)
- Linux man-pages, [poll(2)](https://man7.org/linux/man-pages/man2/poll.2.html)
- Linux man-pages, [epoll(7)](https://man7.org/linux/man-pages/man7/epoll.7.html)
- Linux man-pages, [epoll_ctl(2)](https://man7.org/linux/man-pages/man2/epoll_ctl.2.html)
- Linux man-pages, [listen(2)](https://man7.org/linux/man-pages/man2/listen.2.html)
- Go standard library, [net.TCPConn](https://pkg.go.dev/net#TCPConn)
- Go standard library, [net.KeepAliveConfig](https://pkg.go.dev/net#KeepAliveConfig)
- Go standard library, [net.Conn](https://pkg.go.dev/net#Conn)
- Go standard library, [net/http.Transport](https://pkg.go.dev/net/http#Transport)
- Go standard library, [database/sql.DB](https://pkg.go.dev/database/sql#DB)
- Go standard library, [context](https://pkg.go.dev/context)
- gRPC Documentation, [Keepalive](https://grpc.io/docs/guides/keepalive/)
- gRPC Documentation, [Deadlines](https://grpc.io/docs/guides/deadlines/)
- HAProxy Documentation, [Configuration Manual](https://docs.haproxy.org/3.2/configuration.html)
- HAProxy Documentation, [Backends](https://www.haproxy.com/documentation/haproxy-configuration-tutorials/proxying-essentials/configuration-basics/backends/)
- Envoy Documentation, [HTTP header manipulation](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_conn_man/headers)
- etcd Documentation, [Lease API](https://etcd.io/docs/v3.5/learning/api/#lease-api)
- etcd Documentation, [API reference: concurrency](https://etcd.io/docs/v3.5/dev-guide/api_concurrency_reference_v3/)
- etcd Documentation, [Tuning](https://etcd.io/docs/v3.5/tuning/)
- Diego Ongaro and John Ousterhout, [In Search of an Understandable Consensus Algorithm](https://raft.github.io/raft.pdf)
- Google Cloud Spanner Documentation, [Regional, dual-region, and multi-region configurations](https://cloud.google.com/spanner/docs/instance-configurations)
- Google Cloud Spanner Documentation, [TrueTime and external consistency](https://cloud.google.com/spanner/docs/true-time-external-consistency)
- Google Cloud Spanner Documentation, [Latency points in a Spanner request](https://cloud.google.com/spanner/docs/latency-points)
- Linux Kernel Documentation, [IP Sysctl](https://docs.kernel.org/networking/ip-sysctl.html)
- Linux man-pages, [connect(2)](https://man7.org/linux/man-pages/man2/connect.2.html)
- Linux man-pages, [resolv.conf(5)](https://man7.org/linux/man-pages/man5/resolv.conf.5.html)
- Go standard library, [net.Dialer](https://pkg.go.dev/net#Dialer)
- Go standard library, [net.Resolver](https://pkg.go.dev/net#Resolver)
- CoreDNS Documentation, [cache plugin](https://coredns.io/plugins/cache/)
- CoreDNS Documentation, [forward plugin](https://coredns.io/plugins/forward/)
- Kubernetes Documentation, [Debugging DNS Resolution](https://kubernetes.io/docs/tasks/administer-cluster/dns-debugging-resolution/)
- IETF RFC 4987, [TCP SYN Flooding Attacks and Common Mitigations](https://www.rfc-editor.org/rfc/rfc4987.html)
- IETF RFC 6528, [Defending against Sequence Number Attacks](https://www.rfc-editor.org/rfc/rfc6528.html)
- IETF RFC 7413, [TCP Fast Open](https://www.rfc-editor.org/rfc/rfc7413.html)
- Linux man-pages, [tc-netem(8)](https://man7.org/linux/man-pages/man8/tc-netem.8.html)
