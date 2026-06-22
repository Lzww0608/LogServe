# 23. Observability、日志、指标、Tracing、SLO 与 p99 分析

这一章讨论可观测性里最容易被问混的几个概念：日志、指标、trace 分别回答什么问题，为什么 structured logging 比纯文本日志更适合线上系统，request_id、trace_id、span_id 怎么关联，为什么平均延迟会骗人，以及 p99、tail latency、coordinated omission 这些词背后的工程含义。

下面的回答主要参考 OpenTelemetry 官方文档、Prometheus 官方文档、Google SRE Book / Workbook、OWASP Logging Cheat Sheet 和 HdrHistogram 项目资料。面试里不要把 observability 说成“把日志打全一点”。更准确地说，它是在系统运行时留下足够多、足够可关联、足够低噪声的信号，让我们能回答：系统是否健康，用户是否受影响，问题发生在哪里，为什么发生，以及修复后有没有真的变好。

## Q001. 日志、指标、trace 分别适合回答什么问题？

日志、指标和 trace 都是遥测信号，但它们适合的问题不一样。简单说：

```text
指标回答：现在整体怎么样？趋势是什么？是否超过阈值？
日志回答：某个具体事件发生了什么？上下文是什么？
trace 回答：一次请求经过了哪些服务？时间花在哪里？
```

OpenTelemetry 把 traces、metrics、logs 都归为 signals。Google SRE 的监控章节则强调，面向用户的系统至少要关注 latency、traffic、errors、saturation 这四类 golden signals。指标最适合做这种持续监控，因为它们是低成本、可聚合、适合画趋势和告警的时间序列。

指标适合回答聚合问题：

```text
QPS 是多少？
错误率是否超过 1%？
p99 延迟是否超过 800ms？
队列 backlog 是否持续增长？
CPU、内存、连接池、线程池是不是接近饱和？
某个版本发布后错误率有没有上升？
```

指标的优点是便宜、稳定、可聚合，适合 dashboard、alert、SLO 计算和容量趋势。缺点是丢失细节。你看到 `http_requests_total{status="500"}` 在涨，只能知道 500 多了，不能直接知道是哪一个用户、哪一个请求、哪一条 SQL、哪一个参数导致的。

日志适合回答事件细节：

```text
这次请求为什么失败？
具体异常栈是什么？
用户做了什么操作？
下游返回了什么错误码？
这一条任务的输入、状态转移、失败原因是什么？
```

日志的优点是语义丰富，可以保存业务上下文，适合 debug、审计、安全事件分析、故障复盘。缺点是成本高、噪声大、查询慢，且如果没有稳定字段和关联 ID，很难在海量日志中还原一次请求。OWASP Logging Cheat Sheet 也强调，应用日志既服务安全事件，也服务 operational use cases，但不能“什么都记”，要按用途设计。

trace 适合回答跨服务调用链问题：

```text
一次请求经过了哪些服务？
前端慢是网关慢、业务服务慢、数据库慢，还是第三方 API 慢？
哪个 span 占用了最长时间？
某个错误是在哪个服务第一次出现的？
异步任务和原始请求是否有关联？
```

OpenTelemetry 里，一个 trace 由多个 span 组成。span 代表一次操作或一个工作单元，带有 trace_id、span_id、parent span、开始结束时间、attributes、events、status 等信息。分布式 trace 的关键不是“画一张漂亮链路图”，而是通过上下文传播把跨进程、跨网络边界的 span 关联起来。没有 trace propagation，服务 A 的日志、服务 B 的日志和数据库调用指标就会散在不同地方。

这三类信号最好组合使用。一个常见排查流程是：

```text
1. 指标发现问题：p99 延迟从 300ms 上升到 2s。
2. trace 定位路径：慢请求大多卡在 payment -> risk-check 这个 span。
3. 日志解释原因：risk-check 调用第三方接口超时，重试次数增加。
4. 指标验证修复：降级后错误率下降，p99 恢复，第三方超时计数下降。
```

如果只看日志，可能会陷入单个请求细节，看不出影响范围。如果只看指标，知道系统慢了，却不知道是哪条链路慢。如果只看 trace，知道某次请求怎么走，但不一定知道这个问题是不是普遍存在。三者不是互相替代，而是回答不同层级的问题。

面试里可以这样回答：

```text
指标适合回答聚合和趋势问题，例如 QPS、错误率、p99、资源饱和度、SLO 是否违约；日志适合回答具体事件细节，例如某个请求的异常、参数、状态变化和安全审计；trace 适合回答跨服务因果链和延迟分布，例如一次请求经过哪些服务、哪个 span 慢、错误最早在哪里出现。通常先用指标发现症状，再用 trace 定位链路，最后用日志解释细节，并用指标验证修复是否生效。
```

## Q002. structured logging 相比纯文本日志有什么优势？

structured logging 的核心优势是机器可读。纯文本日志更像写给人看的句子，structured logging 则把一次事件拆成稳定字段，例如 `timestamp`、`level`、`service`、`trace_id`、`user_id`、`operation`、`status`、`duration_ms`、`error_code`。OpenTelemetry 文档也提醒，JSON 只是编码格式；真正的 structured log 要有一致 schema 或定义清楚的 typed fields，下游系统才能可靠解析。

纯文本日志通常长这样：

```text
payment failed for user 123, order 456, timeout after 2000ms
```

它对人很直观，但机器要靠正则猜字段。如果格式稍微变成：

```text
payment timeout: user=123 order=456 elapsed=2s
```

旧查询就可能失效。不同服务、不同语言、不同开发者写出来的句子也不一致，日志平台很难稳定聚合。

structured log 可以写成：

```json
{
  "timestamp": "2026-06-19T10:15:30.123Z",
  "level": "ERROR",
  "service": "payment-service",
  "trace_id": "7bba9f33312b3dbb8b2c2c62bb7abe2d",
  "span_id": "086e83747d0e381e",
  "operation": "charge",
  "user_id": "123",
  "order_id": "456",
  "duration_ms": 2000,
  "error_code": "UPSTREAM_TIMEOUT",
  "retryable": true
}
```

这样一来，查询可以直接按字段过滤：

```text
service = payment-service
level = ERROR
error_code = UPSTREAM_TIMEOUT
duration_ms > 1000
trace_id = ...
```

优势主要有几类。

第一，检索和聚合更可靠。你可以统计每个 `error_code` 的数量，按 `service`、`route`、`tenant_id` 分组，计算某个操作的失败率。纯文本也能做，但要靠 parsing pipeline，规则多了以后很脆。

第二，跨系统关联更容易。日志里稳定带上 `trace_id`、`span_id`、`request_id`，就可以从 trace 跳到相关日志，也可以从日志跳回 trace。OpenTelemetry 支持把 active trace/span context 注入 log record，这一点对排查跨服务问题很关键。

第三，告警和自动化处理更安全。比如 `error_code=PAYMENT_TIMEOUT and retryable=true` 可以触发降级或重试策略分析；`event_type=auth_failure and confidence=high` 可以进入安全监控。纯文本日志很容易因为文案变化让告警失效。

第四，schema 能控制日志质量。字段名、字段类型、枚举值、时间格式、单位都可以规范化。比如统一用 `duration_ms` 或 `duration_seconds`，不要有的服务写 `cost`，有的写 `elapsed`，有的写 `latency`。Prometheus 对 metric 命名和单位也有类似的规范思想，日志虽然更灵活，但不能完全自由发挥。

第五，脱敏更容易。字段化以后，可以对 `password`、`access_token`、`authorization`、`card_number` 做定向 mask 或 drop。纯文本里敏感信息可能藏在 message 字符串里，后处理很难做到稳定。

但 structured logging 不是“所有对象都 JSON dump”。面试时要提醒边界：

```text
字段要稳定，不要每条日志动态产生不同字段名。
高基数字段要谨慎，例如完整 URL、SQL、用户输入、异常 message。
message 仍然有价值，用来给人读；字段用来给机器查。
不要把请求体、响应体、token、密码、银行卡、身份证直接写进结构化字段。
```

如果一个 JSON 日志每条都有不同结构，或者把所有信息塞到 `message` 字段里，它只是 JSON 编码的纯文本日志，不是真正的 structured logging。

面试里可以这样回答：

```text
structured logging 的优势是字段稳定、机器可读、易检索、易聚合、易关联。它能把 timestamp、level、service、trace_id、span_id、operation、status、duration、error_code 等信息作为 typed fields 输出，下游系统可以直接过滤、分组、统计和脱敏。纯文本日志更适合人眼阅读，但格式变化会破坏解析。需要注意，JSON 不等于结构化日志；真正重要的是稳定 schema 和字段语义。同时不要把请求体、token、密码、银行卡、身份证等敏感信息直接写进字段。
```

## Q003. 日志中应该避免记录哪些敏感信息？

日志的基本原则是：能不记录敏感原文就不要记录。日志不是业务数据库，却经常被复制到更多地方：本地文件、集中日志系统、对象存储、搜索引擎、告警平台、工单、截图、故障复盘文档。一个字段一旦进入日志，访问面通常比主库更大，保留时间也可能更长。

OWASP Logging Cheat Sheet 对“Data to exclude”说得很直接：访问令牌、密码、会话标识、PII、数据库连接串、加密密钥、支付卡数据、违法或未经同意采集的信息，都不应该直接进日志，应当移除、mask、sanitize、hash 或 encrypt。

工程里尤其要避免这些内容：

```text
密码、验证码、一次性口令、找回密码 token。
Authorization header、Bearer token、API key、OAuth token、JWT 原文。
Cookie、session_id、refresh_token、CSRF token。
身份证号、护照号、社保号、医保号等政府标识。
银行卡号、支付账户、CVV、银行流水。
手机号、邮箱、详细地址、精确地理位置。
医疗、未成年人、敏感身份、政治宗教等高敏个人信息。
数据库连接串、Redis/MQ/S3 凭证、私钥、加密密钥、签名密钥。
完整请求体和完整响应体，尤其是登录、支付、个人资料、文件上传接口。
内部网络拓扑、私有 IP、主机名、路径等可能被攻击者利用的信息。
```

这里要分清“不能记录”和“可以记录派生信息”。比如为了排查一个用户的问题，通常不需要记录完整手机号，可以记录后四位或 hash 后的稳定标识；为了关联 session，不要记录原始 session_id，可以记录 HMAC 后的 session fingerprint；为了分析支付失败，不要记录卡号，可以记录支付渠道、错误码、金额区间、订单 ID。

常见错误是 debug 日志临时打印对象：

```go
log.Infof("login request: %+v", req)
log.Errorf("payment failed, response=%s", body)
```

这类代码看起来只是开发方便，但上线后会把密码、token、身份证、银行卡、第三方响应里的敏感字段一起带出去。更糟的是，日志采集系统通常不会理解业务对象结构，后处理脱敏很难覆盖所有格式。

比较稳妥的做法是白名单日志字段，而不是黑名单过滤。也就是说，不要把整个 request dump 出来再删敏感字段，而是明确写：

```text
event_type=login_failed
user_hash=...
client_ip_prefix=...
reason=bad_password
trace_id=...
```

另外，trace context 和 baggage 也要小心。OpenTelemetry 的 context propagation 文档提醒，baggage 会跨服务传播，不应放用户凭证、API key 或 PII。trace_id 和 span_id 本身通常不是秘密，但如果你把用户 ID、租户名、权限信息塞进 baggage 或 trace state，再传播到外部服务，就可能泄露内部信息。

面试里可以这样回答：

```text
日志里应避免记录密码、验证码、access token、refresh token、API key、Authorization header、Cookie、session_id、私钥、加密密钥、数据库连接串、银行卡、身份证、手机号、邮箱、详细地址、医疗等高敏个人信息，以及完整请求体和响应体。确实需要关联时，用 hash、HMAC、脱敏后四位、业务 ID 或 trace_id 替代原文。更好的策略是字段白名单，不要 dump 整个 request/response。OpenTelemetry baggage 这类会跨服务传播的上下文也不能放凭证和 PII。
```

## Q004. request_id、trace_id、span_id 的关系是什么？

这三个 ID 都用于关联，但层级不同。

```text
request_id: 应用或网关定义的一次请求标识，范围由系统自己决定。
trace_id: 一条分布式 trace 的全局标识，贯穿一次端到端调用链。
span_id: trace 中某个具体操作或工作单元的标识。
```

request_id 通常是业务系统或网关生成的相关 ID。它可以表示一次外部 HTTP 请求、一次 RPC、一次队列任务、一次用户操作。它不是统一标准，不同公司可能叫 `x-request-id`、`request_id`、`correlation_id`、`operation_id`。它的好处是简单，日志里带上以后，很容易查“这次请求相关的所有日志”。缺点是它不一定表达父子关系，也不一定能跨服务保持一致。

trace_id 是 tracing 系统里的端到端调用链 ID。OpenTelemetry 的 span context 包含 trace ID、span ID、trace flags、trace state。所有属于同一条 trace 的 span 共享同一个 trace_id。比如一次用户下单请求，从 gateway 到 order-service，再到 payment-service、inventory-service、notification-service，只要上下文传播正确，它们产生的 span 都属于同一个 trace_id。

span_id 是单个 span 的 ID。span 表示一个操作，例如：

```text
gateway: POST /orders
order-service: CreateOrder
payment-service: Charge
inventory-service: ReserveStock
database: INSERT orders
```

这些 span 共享同一个 trace_id，但各自有不同 span_id。子 span 会记录 parent span ID，从而形成调用树。OpenTelemetry 的示例中，同一 trace 下的多个 JSON span 共享 `trace_id`，并通过 `parent_id` 表达层级。

W3C Trace Context 标准定义了 `traceparent` header，格式大致是：

```text
version-trace-id-parent-id-trace-flags
```

其中 `trace-id` 对应整条 trace，`parent-id` 是调用方传过来的上游 span id。下游服务收到后，会创建自己的新 span_id，并把上游的 parent-id 作为父 span。OpenTelemetry 文档也说明，服务 A 调服务 B 时，A 会把 trace ID 和 span ID 放进 context，B 用这些值创建属于同一 trace 的新 span，并把 A 的 span 设为父级。

三者的关系可以用一个例子说明：

```text
用户请求 POST /orders
request_id = req-123
trace_id   = 7bba9f33312b3dbb8b2c2c62bb7abe2d

gateway span:
  span_id = aaa
  parent  = none

order-service span:
  span_id = bbb
  parent  = aaa

payment-service span:
  span_id = ccc
  parent  = bbb
```

日志里最好同时带上 `request_id`、`trace_id`、`span_id`。`request_id` 方便和网关、业务日志、客服工单对齐；`trace_id` 方便跳到完整链路；`span_id` 方便定位“这条日志发生在哪个操作里”。如果只带 request_id，跨服务父子关系不清楚；如果只带 trace_id，业务侧查某个用户请求可能不够方便；如果只带 span_id，没有 trace_id 就很难找完整链路。

异步场景还要注意边界。一个消息任务可能不是原请求的子 span，而是后续因果关系。OpenTelemetry 提供 span links 来表达这种关联。比如 HTTP 请求产生一个异步消息，消费者几分钟后处理。消费者 trace 可以通过 link 关联到生产者 span，而不一定强行作为同一个同步调用树的子节点。

面试里可以这样回答：

```text
request_id 是应用或网关定义的一次请求/操作标识，trace_id 是整条分布式 trace 的标识，span_id 是 trace 内某个具体操作的标识。同一条 trace 的所有 span 共享 trace_id，每个 span 有自己的 span_id，并通过 parent span id 形成调用树。W3C traceparent 里包含 trace-id、parent-id 和 trace-flags，下游服务会用上游 parent-id 创建自己的子 span。日志里通常同时记录 request_id、trace_id、span_id：request_id 方便业务查询，trace_id 方便看完整链路，span_id 方便定位日志属于哪个操作。
```

## Q005. 分布式 tracing 如何定位跨服务延迟？

分布式 tracing 定位跨服务延迟的核心方法，是把一次端到端请求拆成一组带时间边界和父子关系的 span。每个 span 记录一个操作的开始时间、结束时间、服务名、操作名、状态、关键属性。上下文传播把这些 span 串成一条 trace。打开 trace 后，不是只看总耗时，而是看每个服务、每个下游调用、每段等待时间各占多少。

一个简化调用链可能是：

```text
gateway POST /checkout                  1200ms
  auth-service VerifyToken                20ms
  cart-service GetCart                    45ms
  product-service GetProducts            180ms
  payment-service Charge                 850ms
    risk-service CheckRisk               700ms
    db INSERT payment                     40ms
  notification-service SendAsync          10ms
```

这条 trace 里，端到端慢主要不是 gateway，也不是 cart，而是 payment 下面的 risk-service。继续展开 risk-service 的 span，可能看到它在等第三方风控 API、连接池、DNS、TLS、数据库锁或重试。

定位时一般按几步走。

第一，看 critical path。不要只把所有 span duration 相加，因为并行调用会重叠。端到端耗时取决于关键路径，也就是决定整体完成时间的那条最长依赖链。两个下游调用各 300ms，如果并行执行，端到端可能还是 300ms 左右；如果串行执行，就是 600ms 以上。

第二，分清 client span 和 server span。服务 A 调服务 B，A 侧 client span 看到的是“从发出请求到收到响应”的时间，包含网络、排队、连接池、B 的处理时间；B 侧 server span 看到的是“B 实际处理请求”的时间。如果 client span 很长，server span 不长，问题可能在网络、负载均衡、连接池、排队或请求还没到 B。Google SRE 也强调，排查时要知道 web server 感知的数据库耗时和数据库自己感知的耗时，否则分不清数据库慢还是中间网络慢。

第三，看 span attributes 和 events。HTTP route、status code、数据库 statement 类型、重试次数、cache hit、message queue topic、tenant、region、version 都能帮助缩小范围。span event 可以记录某个有意义的时间点，例如开始重试、拿到连接、等待锁、收到第一字节。OpenTelemetry 也区分 span attributes 和 span events：如果时间点本身有意义，用 event 更合适。

第四，把 trace 和指标结合起来。单条 trace 只能说明一个样本。定位 p99 延迟时，要找慢 trace 的共同模式：是不是某个 route、tenant、region、版本、下游、partition、数据库 shard 集中变慢。指标能告诉你这个问题影响范围和趋势，trace 告诉你具体路径。

第五，注意采样。生产系统不可能保存所有 trace，尤其是高 QPS 服务。如果只做 head sampling，慢请求可能在入口处就没采样到。tail sampling 可以根据错误、延迟、状态码等结果决定保留哪些 trace，更适合分析异常和尾延迟，但成本更高、实现更复杂。面试里不必展开实现细节，但要知道“为什么我在 trace 系统里找不到这次慢请求”可能是采样策略问题。

第六，异步链路要用 link 或显式关联。HTTP/RPC 这种同步调用容易形成父子 span；队列、定时任务、后台 worker 不一定适合硬塞到同一棵树里。可以在消息 metadata 里传播 trace context，消费者创建新的 trace 或 span，并用 span link 关联生产者。否则一次请求写入消息后，后续异步处理会从 trace 中断开。

面试里可以这样回答：

```text
分布式 tracing 通过 trace_id 把一次端到端请求的多个 span 串起来，每个 span 记录服务、操作、开始结束时间、状态和属性。定位跨服务延迟时，先看端到端 critical path，再展开最慢 span，区分 client span 和 server span，判断是下游处理慢、网络慢、连接池排队、重试、锁等待还是异步队列延迟。trace 适合解释单次请求，指标负责说明影响范围。生产环境还要考虑采样，慢请求分析通常需要保留错误和高延迟 trace；异步链路则要传播 trace context 或使用 span links。
```

## Q006. metrics 中 counter、gauge、histogram、summary 的区别是什么？

Prometheus 官方文档把常见指标类型分成 counter、gauge、histogram、summary。它们不是展示样式的区别，而是数据语义不同。选错类型会直接影响查询、告警和 SLO 计算。

counter 是单调递增计数器，进程重启时可以归零。它适合记录“累计发生了多少次”：

```text
http_requests_total
http_errors_total
jobs_completed_total
bytes_sent_total
cache_misses_total
```

counter 不能用来表示会下降的值。比如当前在线用户数、当前 goroutine 数、当前队列长度都不适合 counter。Prometheus 查询 counter 时通常用 `rate()` 或 `increase()`，因为我们关心一段时间内的速率或增量，而不是进程启动以来的绝对值。

gauge 是可上可下的瞬时值，适合表示“当前是多少”：

```text
memory_usage_bytes
cpu_temperature_celsius
queue_depth
inflight_requests
goroutines
db_connections_in_use
```

gauge 的坑是不要对它乱用 `rate()`。队列长度从 100 到 50，不是“负 QPS”，只是当前值下降了。gauge 适合看趋势、阈值、最大最小值，也适合做容量和饱和度监控。

histogram 用 bucket 记录观测值分布。比如请求延迟，每次请求耗时会落进一组 bucket：

```text
http_request_duration_seconds_bucket{le="0.1"}
http_request_duration_seconds_bucket{le="0.3"}
http_request_duration_seconds_bucket{le="1"}
http_request_duration_seconds_bucket{le="+Inf"}
http_request_duration_seconds_sum
http_request_duration_seconds_count
```

histogram 的优势是可以在服务端聚合。多个实例的 bucket 可以按 `le` 聚合后，用 `histogram_quantile()` 估算 p95、p99。Prometheus 文档也强调，histogram 把观测值放入 bucket，summary 则在客户端计算预设 quantile；histogram 更适合跨实例聚合和 SLO 统计。

summary 也用于观测分布，但它在客户端预计算 quantile，例如 p50、p95、p99。它的优点是客户端可以给出较准确的本地分位数；缺点是这些 quantile 通常不能跨实例正确聚合，也不能事后换一个分位数或时间窗口。Prometheus 文档明确指出，summary 暴露的是预配置 quantile 和时间窗口，不能 later recalculate，也不能 aggregate quantiles。

一个常见选择表：

```text
累计请求数、错误数、任务完成数 -> counter
当前队列长度、连接数、内存、CPU 温度 -> gauge
请求延迟、响应大小、任务耗时，且要聚合 p95/p99 -> histogram
单进程内需要本地 quantile，且不需要跨实例聚合 -> summary
```

延迟指标一般优先选 histogram，尤其是要做 SLO、p95/p99、按服务聚合时。bucket 要围绕 SLO 设计。比如 SLO 是 99% 请求小于 900ms，就应该有接近 0.9s 的 bucket，否则“多少请求满足 SLO”会不准确。Prometheus 新的 native histogram 能减少传统 bucket 的一些限制，但面试时把 classic histogram 的 `_bucket/_sum/_count` 讲清楚就已经很扎实。

面试里可以这样回答：

```text
counter 是单调递增计数器，适合请求数、错误数、完成任务数，查询时常用 rate 或 increase。gauge 是可升可降的当前值，适合队列长度、内存、连接数、in-flight 请求。histogram 把观测值放进 bucket，并提供 count 和 sum，适合请求延迟、响应大小、任务耗时，能跨实例聚合后计算分位数和 SLO。summary 在客户端预计算 quantile，单实例看很方便，但预设窗口和 quantile 后很难变更，也不能把多个实例的 p99 简单平均成整体 p99。线上延迟和 SLO 通常优先用 histogram。
```

## Q007. 为什么平均延迟经常误导人？

平均延迟经常误导人，因为延迟分布通常不是正态分布。它往往是偏斜的：大部分请求很快，少部分请求非常慢。平均值把这些样本压成一个数，会掩盖尾部。Google SRE 在 SLO 章节里也提醒，latency SLI 更应该被看作 distribution，而不是简单 average；简单平均会遮住 tail latency。

举个例子，100 个请求里：

```text
99 个请求耗时 10ms
1 个请求耗时 5000ms
```

平均延迟是：

```text
(99 * 10 + 1 * 5000) / 100 = 59.9ms
```

平均值看起来不到 60ms，很健康。但那 1 个用户等了 5 秒。如果这个 1% 刚好发生在支付、登录、搜索、下单这种关键路径，用户体验很差。更麻烦的是，一个页面可能依赖多个后端服务。Google SRE 的监控章节举过类似思路：一个后端的 99th percentile 很容易变成前端用户体验中的常见慢请求。

平均值还会被快失败污染。比如数据库断开后，服务很快返回 500：

```text
成功请求平均 300ms
失败请求平均 5ms
```

如果把成功和失败混在一起算总平均，系统故障时平均延迟可能反而下降。Google SRE 的 golden signals 里也强调，要区分成功请求和失败请求的 latency。一个快速 500 不是好体验，它只是失败得很快。

平均值也不适合解释饱和。系统接近容量上限时，通常不是所有请求一起慢，而是部分请求开始排队，尾部先变差。p99 可能已经从 300ms 变成 3s，平均值只从 80ms 变成 120ms。等平均值明显上升时，用户可能已经投诉很久了。

另一个问题是平均值无法表达 SLO。假设 SLO 是“99% 请求小于 900ms”，平均延迟 200ms 并不能说明 SLO 达标。可能 95% 请求是 50ms，5% 请求是 3s，平均值仍然不难看，但 99% SLO 已经失败。

这不代表平均值完全没用。平均值适合看成本和容量，比如平均 CPU、平均请求耗时乘以 QPS 可以估算总工作量；Prometheus histogram 的 `_sum/_count` 也可以算 average。但它不应该单独代表用户体验。面试里更稳的说法是：平均值可以作为辅助指标，用户体验和 SLO 要看分布、分位数、错误率和请求量。

面试里可以这样回答：

```text
平均延迟会把分布压成一个数，容易掩盖少量非常慢的请求。延迟通常是长尾分布，不是正态分布；99 个 10ms 和 1 个 5s 的请求平均只有约 60ms，但那个用户等了 5 秒。平均值还会被快速失败污染，数据库挂了以后大量 500 很快返回，平均延迟可能下降。系统饱和时往往 p95/p99 先变差，平均值后知后觉。平均值可以用于容量和成本估算，但不能单独代表用户体验，SLO 应该看分布、分位数、错误率和流量。
```

## Q008. p50、p95、p99、p999 分别适合观察什么？

p50、p95、p99、p999 都是分位数。p99 的意思是：在这段时间、这个筛选条件下，99% 的请求延迟小于等于这个值，剩下 1% 更慢。它不是“最慢请求”，也不是“99 个请求里的第 99 个一定慢到这个值”，而是一个分布位置。

p50 是中位数，适合看典型请求体验：

```text
大部分用户是否感觉正常？
普通路径有没有整体变慢？
发布后常规请求是否退化？
```

p50 很稳，不容易被极少数异常点影响。但它也最容易掩盖尾部。一个服务 p50 30ms、p99 3s，说明大多数请求很快，但尾部很糟。

p95 适合看比较明显的慢请求群体。5% 的请求已经不是极少数。如果 p95 变差，通常说明慢已经不是个别异常，而是某类 route、某个 region、某个租户、某种 payload、某个下游依赖在影响相当多的用户。很多交互式服务会把 p95 用作 dashboard 的主延迟视图之一。

p99 适合看尾部体验和饱和早期信号。Google SRE 的监控章节提到，短窗口的 99th percentile response time 可以作为 saturation 的早期信号。p99 对排队、锁竞争、连接池耗尽、GC、下游长尾、热点分片更敏感。它适合回答：

```text
最慢的 1% 用户是否已经不可接受？
系统是否接近性能悬崖？
某次发布是否只伤害了尾部请求？
```

p999 更适合看极端尾部，常用于高 QPS、低延迟或强用户体验系统。例如广告竞价、实时推荐、交易、游戏、存储系统、RPC 基础设施。p999 的难点是样本量要求很高。每分钟只有 100 个请求时，p999 基本没有统计意义；每分钟 100 万请求时，0.1% 就是 1000 个请求，才值得严肃观察。

不同分位数的使用要结合请求量。一个 p99 点需要足够样本，否则抖动会很大。低流量接口更适合看固定阈值下的好事件比例，例如“过去 30 天 99% 请求小于 900ms”，或者用更长窗口。高流量接口可以看短窗口 p99/p999，低流量后台任务则可能更适合看最大值、超时数和慢任务列表。

还要注意聚合方式。不能把每台机器的 p99 取平均后说这是全局 p99。每个实例的请求量不同，局部分位数不能简单平均。要么用 histogram bucket 聚合后计算全局 quantile，要么在日志/trace 原始样本上计算。Prometheus 文档也说明，summary 的 quantile 不可聚合，histogram 更适合跨实例聚合。

面试里可以这样回答：

```text
p50 看典型请求体验，适合判断普通路径是否正常；p95 看较明显的慢请求群体，5% 变慢通常已经有用户感知；p99 看尾部体验和饱和早期信号，适合发现排队、锁竞争、连接池、GC、下游长尾；p999 看极端尾部，只在请求量足够大、业务对长尾很敏感时才有意义。分位数必须结合样本量、时间窗口和筛选条件解释，不能把各实例 p99 简单平均成全局 p99，跨实例应使用 histogram 聚合或原始样本计算。
```

## Q009. tail latency 为什么重要？

tail latency 重要，是因为用户通常不关心系统的平均表现，而关心自己这一次请求等了多久。一个系统 99% 请求很快，1% 请求很慢，对平台平均值来说可能还不错；对那 1% 用户来说，就是实实在在的卡顿、超时、重复点击、放弃支付或刷新页面。

尾延迟会在组合系统里被放大。假设一个页面要调用 20 个后端接口，每个接口都有 1% 概率超过 1 秒。即使每个服务单独看“只有 1% 慢”，页面碰到至少一个慢接口的概率也会明显上升。服务越多、串行依赖越多、fan-out 越大，尾部越容易变成常态体验。

尾延迟还会暴露饱和。系统接近容量上限时，通常先出现排队：

```text
连接池快满了，少量请求等连接。
线程池快满了，少量任务排队。
数据库锁竞争，少量事务等待。
GC 暂停，某些请求被卡住。
热点 key 或热点分片，让一部分请求变慢。
```

这些问题一开始不一定影响 p50，却会先抬高 p95、p99。等 p50 都变差时，系统通常已经离事故很近了。

尾延迟也影响资源利用。慢请求占着连接、线程、内存、锁、队列槽位，导致后续请求继续排队。一个 5 秒请求不只是让一个用户等 5 秒，它还可能占住一个 worker，使其他请求进入等待。长尾会制造反馈环：请求慢 -> in-flight 增加 -> 排队增加 -> 更多请求慢。

对 SLO 来说，tail latency 是用户承诺的一部分。Google SRE 的 SLO 示例里会定义多个目标，例如 90% 请求小于某阈值、99% 请求小于另一个阈值。这样比“平均延迟小于 100ms”更接近真实体验。尤其是交互式服务，p99 超时可能比平均值更能解释用户投诉。

但也要避免滥用 p999。尾部越极端，越需要更多样本，越容易受到采样、时钟、探针、GC、网络抖动影响。p999 对高 QPS 服务很有价值，对低 QPS 管理接口可能只是噪声。尾延迟分析要配合 trace、日志、资源指标和请求分类，否则只看一条 p99 曲线会很难定位原因。

面试里可以这样回答：

```text
tail latency 重要，因为用户体验由自己那次请求决定，不由平均值决定。少量慢请求会导致超时、重复点击、支付放弃，在 fan-out 或多服务组合场景里还会被放大。尾部变差也是饱和的早期信号，连接池、线程池、锁、GC、热点分片通常先影响 p95/p99，再影响 p50。慢请求还会占住资源，引发排队反馈。SLO 里用 p95/p99 或“多少比例请求低于阈值”比平均延迟更接近用户体验，但 p999 这类极端尾部必须结合样本量和时间窗口解释。
```

## Q010. coordinated omission 是什么？

coordinated omission 是一种延迟测量偏差：系统变慢时，测试工具或客户端也被迫停下来，结果没有继续发出本该发生的请求，于是慢的时间段没有被充分采样，最终报告出来的延迟比真实用户会经历的延迟好看很多。

最典型的错误压测方式是“发一个请求，等响应回来，再发下一个请求”：

```text
send request
wait response
record latency
send next request
```

如果系统一直很快，这种方式看起来没问题。但如果服务暂停 10 秒，客户端也在等待这个请求返回。等待期间它没有继续按照原计划发请求，也就没有记录那些本该排队 9 秒、8 秒、7 秒的请求。最后 histogram 里可能只有一个 10 秒样本，其他缺失样本都被省略了。这就是 omission。因为客户端的发请求节奏被被测系统的响应节奏协调了，所以叫 coordinated omission。

HdrHistogram 的 README 用一个类似例子解释了这个问题：如果每 10ms 采样一次响应时间，系统发生 100 秒暂停，但原始数据只记录了一个 100 秒值，那么 histogram 会显示几乎所有样本都很快，这显然不对。用 expected interval 做修正后，会补上那些本该观察到的延迟样本，分布才更接近随机请求会看到的情况。

可以用一个简化例子看：

```text
目标负载：每 10ms 发 1 个请求。
系统从 t=0 开始暂停 1000ms。

错误测法：
只发出 1 个请求，等 1000ms 返回，记录 1 个 1000ms。

更接近真实的测法：
这 1000ms 内本该有大约 100 个请求到达。
它们会分别等待 1000ms、990ms、980ms ... 10ms。
```

如果不修正，p99、p999 会严重偏低。你以为系统只有一个请求慢，真实情况是暂停期间到达的所有请求都慢。这个问题在 stop-the-world GC、线程池卡死、数据库锁、网络暂停、限流排队、单线程事件循环阻塞时都很常见。

避免 coordinated omission 的方法有几类：

```text
按固定到达率生成负载，不要等上一个响应回来才决定下一个请求何时发。
记录计划发送时间和实际完成时间，用计划时间计算响应延迟。
使用支持 coordinated omission 修正的压测工具或 histogram。
报告吞吐时同时报告实际到达率、完成率、错误率、超时数。
把超时、排队、客户端等待也算进用户感知延迟，不要只算服务端 handler 时间。
```

也要区分服务端延迟和用户感知延迟。服务端 handler 可能只处理了 50ms，但请求在客户端、负载均衡器、连接池、队列里等了 2 秒。对用户来说是 2 秒。coordinated omission 常常让测试只看到 handler 的好看数据，看不到排队和暂停期间的损失。

面试里可以这样回答：

```text
coordinated omission 是延迟测量中的漏采样偏差：系统变慢时，压测客户端因为等待响应而停止按原计划发请求，导致暂停期间本该排队的请求没有被记录，p99/p999 会被严重低估。典型错误是“发一个请求，等返回再发下一个”。正确做法是按固定到达率发请求，记录计划发送时间到完成时间的延迟，或使用支持 coordinated omission 修正的 histogram/压测工具，并同时报告到达率、完成率、错误率和超时数。它提醒我们，用户感知延迟包括排队和等待，不只是服务端处理时间。
```

## Q011. 如何正确测量端到端延迟？

端到端延迟首先要定义清楚边界：从哪里开始计时，到哪里结束计时。很多团队说自己测了“接口延迟”，实际只测了服务端 handler 的执行时间，漏掉了客户端排队、DNS、连接建立、TLS、负载均衡、网关、线程池等待、下游 RPC、重试、响应传输，甚至前端渲染。这个数字对优化服务端有用，但不能直接代表用户体验。

比较严谨的定义是从用户或上游系统发起请求开始，到结果对用户或上游系统可用为止。HTTP API 可以定义为客户端开始发送请求到完整读到响应；浏览器页面可以定义为导航开始到页面可交互；异步任务可以定义为任务提交到最终状态可见；消息流水线可以定义为事件产生或入队到业务效果落库、通知发出或查询可见。Google SRE 在 SLI 讨论里也提醒，客户端侧延迟通常更接近用户感知，只是有时只能用服务端指标做代理。

常见边界可以这样拆：

```text
客户端视角：request_start -> response_complete
包含客户端排队、连接、网络、代理、服务端处理、重试和响应传输。

入口视角：edge_receive -> edge_response_sent
包含网关到后端链路，但不包含用户网络和客户端等待。

服务端视角：handler_start -> handler_finish
只覆盖本服务处理，不覆盖入口前排队和客户端体感。

异步链路视角：event_created -> effect_visible
覆盖排队、消费、重试、下游写入和最终可见性。
```

端到端指标最好和分段指标一起采集。单个大数字能告诉你用户慢了，但不能告诉你慢在哪里。trace 适合做这个拆分：入口 span、业务 handler span、下游 RPC span、数据库 span、消息 publish/consume span 组成一条链。OpenTelemetry 的 trace 和 context propagation 正是用来把跨进程执行路径串起来的。

重试是延迟测量里最容易被漏掉的部分。用户一次点击可能触发多次 RPC attempt。如果只记录最后一次成功 attempt 的耗时，延迟会被低估；如果把每次 attempt 都当成独立用户请求，流量和错误率会被高估。gRPC OpenTelemetry 指标区分 client call duration 和 client attempt duration，很值得借鉴：call 表示一次应用调用的总耗时，attempt 表示某次底层尝试的耗时。

失败、取消和超时也要进入统计。只统计成功请求会得到很好看的 p99，但用户看到的是超时、错误页或卡住。Google SRE 的四个黄金信号建议区分成功请求延迟和失败请求延迟，因为一个很快返回的 500 不能和成功响应混在一起解释；慢错误也要单独看。

异步系统还要把“排队等待”算进去：

```text
event_time 或 enqueue_time -> dequeue_time：排队等待。
dequeue_time -> ack_time：消费者处理。
event_time 或 enqueue_time -> final_effect_time：端到端完成。
retry_count / redelivery_count：重试放大。
dead_letter_count：最终失败。
```

跨机器测量还要注意时钟同步。trace UI 里的瀑布图依赖各节点时间，NTP 漂移可能让 span 看起来重叠、倒序或出现负间隔。可以用统一时间源、入口统一打点，或者把本进程内 duration 当作更可靠的分段证据。压测时还要避免 coordinated omission：系统卡住时，客户端排队等待也属于用户体感，不能只从服务端收到请求后开始算。

面试里可以这样回答：

```text
端到端延迟要先定义测量边界：从用户或上游系统发起，到结果可用为止。服务端 handler 时间只是分段指标，不等于用户体验。正确做法是同时采集客户端视角、入口视角、服务端视角和关键下游 span，用 trace 拆分网关、排队、业务处理、下游 RPC、重试和响应传输。成功、失败、超时要分开统计，重试要区分一次用户 call 和多次底层 attempt。异步链路还要测 enqueue 到最终完成的总时间，而不是只测 consumer 执行时间。
```

## Q012. RED 方法和 USE 方法分别是什么？

RED 方法面向服务，USE 方法面向资源。两者都常用，但回答的问题不一样。

RED 是 Rate、Errors、Duration，通常用于 HTTP、RPC、消息消费这类服务请求视角：

```text
Rate：请求速率，例如 QPS、每秒消费消息数、每秒写入批次数。
Errors：错误数量或错误率，例如 5xx、业务失败、超时、拒绝、DLQ。
Duration：请求耗时分布，例如 p50、p95、p99、histogram。
```

RED 关注调用方是否顺利得到服务。Rate 告诉你当前负载，Errors 告诉你是否失败，Duration 告诉你是否慢。它适合做服务级 dashboard 和告警，因为每个微服务都可以用类似结构表达健康度。Tom Wilkie 对 RED 的解释就是为每个服务建立一致的请求率、错误率和延迟分布视图。

USE 是 Utilization、Saturation、Errors。Brendan Gregg 的原始表述是：对每个资源检查利用率、饱和度和错误。它更适合定位底层瓶颈：

```text
Utilization：资源有多忙，例如 CPU busy、磁盘 busy、网络带宽占用。
Saturation：资源排队有多严重，例如 run queue、磁盘队列、连接池等待、线程池队列。
Errors：资源层错误，例如磁盘错误、网络丢包、连接失败、OOM、限额拒绝。
```

USE 的价值在于系统性。服务变慢时，真正瓶颈可能在磁盘 I/O、连接池、内存回收、网络接口、文件描述符、线程池或锁队列。USE 让你按资源清单逐项检查，减少遗漏。

两者可以这样配合：

```text
RED 发现用户侧症状：
QPS 正常但 p99 升高，或者错误率上升。

USE 定位资源侧原因：
磁盘 I/O 饱和、线程池排队、连接池耗尽、CPU run queue 上升。
```

Google SRE 的四个黄金信号是 latency、traffic、errors、saturation。RED 覆盖 traffic、errors、latency，少了 saturation；USE 强调 resource saturation，但不直接表达每个业务服务的用户请求表现。实际落地时，服务 dashboard 用 RED 或黄金信号，资源排查用 USE。

面试里可以这样回答：

```text
RED 是面向服务请求的方法，Rate 看请求速率，Errors 看失败率，Duration 看延迟分布，适合 HTTP/RPC/消息消费这类服务级监控。USE 是面向资源的方法，对每个资源检查 Utilization、Saturation、Errors，适合定位 CPU、内存、磁盘、网络、连接池、线程池、锁等资源瓶颈。RED 更像用户症状视角，USE 更像机器和资源视角。线上排查时我会先用 RED 或四个黄金信号确认哪个服务影响用户，再用 trace 和 USE 定位慢在下游、排队还是资源饱和。
```

## Q013. SLO、SLA、SLI 的区别是什么？

SLI 是指标，SLO 是目标，SLA 是带后果的协议。

SLI，Service Level Indicator，是“服务水平指标”。它必须可测量，最好直接反映用户关心的体验。典型 SLI 包括成功率、端到端延迟、正确性、吞吐、数据新鲜度和持久性。例如：

```text
可用性：成功请求数 / 总有效请求数。
延迟：99% 请求在 300ms 内完成。
正确性：返回结果满足业务校验的比例。
新鲜度：数据从产生到可查询的时间。
```

SLO，Service Level Objective，是“SLI 应该达到的目标”。例如：

```text
过去 30 天内，99.9% 的有效 API 请求成功返回非 5xx。
过去 7 天内，99% 的读取请求端到端延迟小于 200ms。
过去 1 小时内，95% 的消息在入队后 30 秒内被处理完成。
```

SLA，Service Level Agreement，是和用户或客户达成的协议，通常包含 SLO，也包含未达标后的后果，例如赔偿、服务抵扣、合同责任或升级流程。Google SRE 对 SLA 的判断很实用：如果没有明确后果，它多半只是 SLO，不是 SLA。

三者可以这样记：

```text
SLI：我们怎么量？
SLO：量出来应该达到什么目标？
SLA：达不到目标有什么合同或业务后果？
```

一个 API 例子：

```text
SLI：good_requests / valid_requests
good_request = HTTP status 非 5xx 且 latency < 300ms

SLO：过去 30 天 good_requests 比例 >= 99.9%

SLA：如果月度可用性低于 99.9%，按合同给客户服务抵扣。
```

好的 SLI 要写清楚统计窗口、数据来源、过滤条件和 good event。否则 SLO 会变成争议源。“99% 请求小于 200ms”还不够，需要说明从客户端看还是服务端看、是否包含 4xx、是否包含超时、是否按地区或租户单独计算、窗口是滚动 30 天还是自然月。

SLO 也不应该盲目追求 100%。100% 目标会让系统过度保守，发布变慢，成本变高，而且分布式系统本身无法承诺永远不失败。SLO 的价值是把可靠性目标和创新速度放进同一套语言里：剩余 error budget 充足，可以正常发布；预算快速燃烧，就该降速、冻结高风险变更或优先修复可靠性问题。

面试里可以这样回答：

```text
SLI 是具体怎么衡量服务水平，比如成功率、端到端延迟、数据新鲜度；SLO 是这些指标要达到的目标，比如 30 天内 99.9% 请求成功且 p99 小于 300ms；SLA 是对外协议，通常包含未达标后的赔偿或合同后果。一个可靠的 SLO 必须写清楚统计窗口、数据来源、good event 定义、排除条件和聚合维度。SLO 不应盲目追求 100%，因为 error budget 正是用来平衡可靠性和发布速度的。
```

## Q014. error budget 如何指导发布节奏？

error budget 是“允许失败的额度”。如果 SLO 是 99.9% 可用性，那么统计窗口内允许 0.1% 的请求不满足目标。这个 0.1% 就是预算。它不是鼓励失败，而是让可靠性讨论从抽象争论变成可计算的工程决策。

发布节奏可以围绕预算状态调整：

```text
预算充足：
正常发布，允许低到中等风险变更，但保持回滚和监控。

预算消耗加快：
减少发布批量，提高灰度门槛，延长观察窗口，优先修复明显风险。

预算接近耗尽：
冻结非紧急功能发布，只允许可靠性修复、安全修复和回滚。

预算已经耗尽：
停止高风险变更，复盘预算消耗来源，恢复 SLO 后再恢复常规发布。
```

它解决的是研发和运维之间的常见冲突。产品希望快发，SRE 希望稳。没有 error budget 时，双方容易变成观点对抗。有了预算，可以改成数据讨论：这次发布预计增加多少失败风险？过去 7 天 burn rate 是否过高？如果失败，能否快速回滚？这比口头说“谨慎一点”更可执行。

error budget 通常不只看剩余额度，还要看燃烧速度。假设 30 天预算看起来还剩 60%，但最近 1 小时 burn rate 很高，说明事故正在发生或刚刚发生。反过来，月度预算少了一点，但近期很稳定，不一定要冻结所有发布。SLO 告警常用多窗口 burn rate，就是为了同时发现快速事故和慢性消耗。

发布治理可以变成几条具体规则：

```text
灰度规则：灰度期间错误率或 p99 触发预算燃烧阈值，自动暂停或回滚。
变更准入：预算不足时，不接受大规模 schema 迁移、依赖升级、批量配置变更。
发布批量：预算越少，单次发布影响面越小，观察时间越长。
优先级：预算燃烧来自哪个服务，那个服务下一轮优先修复可靠性问题。
```

也要防止误用。error budget 不是“本月还有预算，所以可以随便冒险”。一次只影响内部管理接口的 0.01% 错误，和一次影响支付链路的 0.01% 错误，不该被同等对待。预算是决策输入，不是替代工程判断。预算燃烧也不一定来自发布，可能来自依赖故障、容量不足、热点租户、配置漂移或数据质量问题。

面试里可以这样回答：

```text
error budget 是 SLO 允许失败的额度。预算充足时正常发布；预算燃烧过快时缩小灰度、延长观察、提高回滚敏感度；预算接近耗尽时冻结非必要功能变更，只允许可靠性修复和安全修复。更重要的是看 burn rate，而不只是看剩余额度，因为快速燃烧说明事故正在影响用户。error budget 的价值是把“要快还是要稳”的争论变成数据化决策，但它不是冒险许可证，仍然要结合业务影响面和恢复能力判断。
```

## Q015. 高基数 metrics 会带来什么问题？

高基数 metrics 的问题本质是时间序列爆炸。Prometheus 这类系统里，一个指标名加上一组 label key/value 就是一条时间序列。只要 label 组合不同，就是新的 series。指标名本身可能只有一个，但 label 组合可以把它放大到百万、千万级。

例如这个指标还比较可控：

```text
http_requests_total{method="GET", status="200", route="/api/orders"}
```

如果继续加这些 label，就会出问题：

```text
user_id
email
order_id
request_id
session_id
ip
full_url
trace_id
```

这些值会随用户、请求或订单无限增长。Prometheus 官方文档提醒，每个 labelset 都有 RAM、CPU、磁盘和网络成本，并且不要把 user ID、email 这类无界集合放进 label。

高基数会拖垮多个环节：

```text
采集端：客户端库维护大量 label child，内存增加，热路径更新指标变慢。
传输端：scrape 响应变大，网络和序列化开销变高。
存储端：TSDB head series 增加，内存、WAL、索引、压缩成本上升。
查询端：聚合和过滤需要扫描更多 series，dashboard 变慢，告警规则超时。
运维端：成本上升，保留时间缩短，真正重要的指标被噪声挤掉。
```

高基数最危险的地方是它会随着业务增长突然爆炸。开发环境只有 100 个用户，`user_id` label 看起来没问题；上线后 1000 万用户，每个用户、endpoint、status 都可能产生 series，监控系统会出现 scrape timeout、remote write backlog、查询 OOM、告警延迟。

不是所有高基数都绝对不能碰，问题在用途和边界。少量受控维度有价值，比如 `method`、`route_template`、`status_class`、`region`、`service`、`tenant_tier`。无界维度应该进入日志、trace、事件分析或离线系统。要查某个用户的请求，应通过日志或 trace 查；metrics 只保留可聚合的趋势。

面试里可以这样回答：

```text
高基数 metrics 会造成时间序列爆炸。Prometheus 里每个指标名和 label 组合都是一条 series，label 值如果包含 user_id、email、request_id、order_id 这类无界集合，内存、索引、WAL、网络、查询和告警成本都会快速上升。线上症状通常是 scrape 变慢、remote write 积压、dashboard 超时、告警延迟、TSDB 内存飙升。我的原则是 metrics label 只放低基数、可聚合、稳定的维度；用户级、请求级、订单级定位交给日志和 trace。
```

## Q016. label 设计不当会如何拖垮监控系统？

label 设计不当会直接影响监控系统容量、查询复杂度和告警可靠性。Prometheus 的 label 很强大，但强大也意味着容易滥用。

第一类问题是无界 label：

```text
http_request_duration_seconds{path="/users/123/orders/987"}
http_request_duration_seconds{request_id="abc-123"}
http_request_duration_seconds{error="sql duplicate key value violates unique constraint orders_user_id_idx"}
```

`path` 应该用 route template，例如 `/users/{id}/orders/{order_id}`；`request_id` 应该进日志或 trace；`error` 应该归一化成错误码或错误类别，比如 `error_type="duplicate_key"`。

第二类问题是把语义不同的东西放进同一个 metric。Prometheus 命名规范里有个实用原则：同一个 metric 在所有 label 维度上 sum 或 avg 应该有意义。比如 `queue_capacity` 用 `queue` label 表示不同队列容量还可以；但把“队列容量”和“当前队列长度”混进一个 metric，只靠 `type="capacity|depth"` 区分，聚合结果就没有意义。label 是区分同一类事物，不是把不同语义硬塞进一个名字。

第三类问题是维度乘法。即使每个 label 都是低基数，组合起来也可能很大：

```text
service 100
route 200
status 20
region 10
tenant_tier 5
理论组合：100 * 200 * 20 * 10 * 5 = 20,000,000
```

实际不一定所有组合都出现，但新增 label 前要问：这个维度是否真的用于告警、容量规划或排障？如果只是“以后可能有用”，不要默认加。

第四类问题是 label 值不规范。比如同一个状态既有 `status="500"`，又有 `code="500"`；同一个路由有 `/api/user/:id`、`/api/user/{id}`、`/api/user/123` 三种写法。结果是 dashboard 聚合漏数据，告警规则复杂，跨服务对比困难。

第五类问题是敏感信息。email、手机号、IP、token 片段、租户内部 ID 不只带来高基数，也带来合规和泄漏风险。metrics 往往保留时间长、访问面广、会被远程写入多个系统，不能假设它比日志安全。

面试里可以这样回答：

```text
label 设计不当会通过时间序列乘法拖垮监控系统。无界 label 会不断创建新 series；维度过多会让组合数爆炸；语义混乱会让 sum/avg 聚合失去意义；label 值不规范会导致 dashboard 和告警漏算。线上症状是 Prometheus 内存升高、scrape 超时、查询变慢、告警规则延迟、remote write 积压。我的做法是只放低基数、稳定、可聚合的 label，用 route template 替代 full path，用错误类别替代原始错误，把 request_id、user_id、trace_id 放到日志或 trace，而不是 metrics label。
```

## Q017. 如何为队列系统设计关键指标？

队列系统的指标要围绕四个问题设计：生产是否正常，消费是否跟得上，消息是否在可接受时间内完成，失败和重试是否可控。

只看队列长度不够。队列长度是 backlog 的快照，能表示积压规模，但不能直接表示用户等待时间。一个队列有 10 万条消息，如果消费者每秒处理 10 万条，可能 1 秒清完；另一个队列只有 1000 条消息，但消费者每秒只处理 1 条，用户要等很久。所以队列系统至少要同时看 backlog、age、rate 和 processing latency。

核心指标可以分成几组：

```text
生产侧：
messages_published_total
publish_errors_total
publish_latency_seconds
message_size_bytes
producer_throttle_total 或 rejected_total

队列状态：
messages_visible 或 messages_ready
messages_in_flight 或 messages_unacknowledged
oldest_message_age_seconds
delayed_messages
partition_or_group_count

消费侧：
messages_consumed_total
consumer_processing_duration_seconds
ack_total / nack_total
consumer_errors_total
consumer_concurrency
consumer_lag 或 backlog_seconds

重试和失败：
redelivery_total
retry_count 分布
visibility_timeout_expired_total
dead_letter_messages_total
poison_message_total
```

AWS SQS 的 CloudWatch 指标很适合作参考：`ApproximateNumberOfMessagesVisible` 表示可取消息数量，`ApproximateNumberOfMessagesNotVisible` 表示已被消费者收到但还没删除或超时的 in-flight 消息，`ApproximateAgeOfOldestMessage` 表示最老未处理消息年龄，`NumberOfMessagesReceived` 会包含因为 visibility timeout 回到队列后再次收到的消息。SQS 文档还提醒这些值在分布式架构下是 approximate，不能当精确计数。

RabbitMQ 的监控文档也给了类似结构：`messages`、`messages_ready`、`messages_unacknowledged`、publish rate、deliver rate。它还强调应用级指标很重要，因为 broker 指标能告诉你队列积压或节点磁盘满，但不一定能告诉你是哪个 producer 暴走、哪个 consumer 下游数据库慢、哪个业务消息一直失败。

队列延迟建议拆成三段：

```text
enqueue_delay：事件产生到入队成功，反映 producer 和 broker 写入问题。
queue_wait：入队到被消费者拿到，反映 backlog、消费者能力和分区热点。
processing_time：消费者拿到到 ack，反映业务处理和下游依赖。
end_to_end_latency：事件产生或入队到最终业务效果可见。
```

告警上，用户体验更应该看 age 和 end-to-end latency，而不是只看 length。`oldest_message_age_seconds` 持续上升，通常比 `queue_depth` 更直接说明用户等待变差。对 FIFO 或分组队列，还要看活跃 group 数、单 group 阻塞、分区热点，因为一个 poison message 可能卡住同组消息。

面试里可以这样回答：

```text
队列系统不能只看队列长度。我会同时设计生产速率、消费速率、backlog、最老消息年龄、in-flight/unacked、处理耗时、端到端耗时、重试次数、redelivery、DLQ 和 broker 资源指标。用户体验更接近 oldest message age 和 enqueue 到最终完成的延迟，而不是当前有多少条消息。SQS 和 RabbitMQ 的官方指标都体现了这个思路：既看 visible/ready backlog，也看 not visible/unacknowledged 和 message rate。应用层还要记录业务失败、下游慢、poison message 和租户热点，否则 broker 指标只能告诉你有积压，不能解释为什么积压。
```

## Q018. 如何为缓存系统设计关键指标？

缓存系统指标要回答三类问题：缓存有没有帮上忙，缓存本身是否健康，缓存是否正在制造错误或雪崩风险。

第一组是命中效果：

```text
cache_requests_total
cache_hits_total
cache_misses_total
hit_ratio = hits / (hits + misses)
miss_reason：not_found、expired、evicted、bypass、error
```

命中率要分业务维度看，但维度不能太细。按 cache name、operation、object type、region、result 拆分通常有价值；按 key、user_id、full_url 拆分会变成高基数。Redis `INFO` 里有 `keyspace_hits` 和 `keyspace_misses`，可以作为全局命中情况参考，但业务通常还需要在应用侧按缓存用途拆分，否则一个全局 hit ratio 很难解释。

第二组是延迟和负载：

```text
cache_get_duration_seconds
cache_set_duration_seconds
cache_delete_duration_seconds
cache_mget_size / pipeline_size
requests_per_second
connection_pool_in_use
connection_pool_wait_duration_seconds
timeout_total
```

缓存慢有时比缓存 miss 更糟。一次 miss 至少可以回源；一次缓存请求卡住会占用线程、连接池和上游请求预算。缓存系统要看 p95/p99，而不是只看平均耗时。连接池等待时间也很关键：Redis 本身可能很快，但客户端连接池耗尽会让应用请求排队。

第三组是容量和淘汰：

```text
used_memory_bytes
max_memory_bytes
memory_fragmentation_ratio
keys_total
expired_keys_total
evicted_keys_total
ttl_distribution
hot_key_topk 或采样热点
```

Redis `INFO` 里 `evicted_keys` 表示因为 maxmemory 限制被淘汰的 key。`evicted_keys` 增长不一定永远是事故，因为某些缓存策略允许淘汰；但如果伴随 hit ratio 下降、回源 QPS 上升、数据库延迟上升，就说明缓存容量或 TTL 策略影响了用户体验。

第四组是正确性和保护指标：

```text
stale_read_total：读到过期但被允许返回的次数。
cache_fill_errors_total：回源填充失败。
singleflight_dedup_total：请求合并次数。
backend_fallback_total：缓存不可用后回源次数。
negative_cache_hit_total：空值缓存命中。
rebuild_in_progress：缓存重建压力。
```

缓存指标不能只盯 hit ratio。高命中率不代表系统健康。比如 99% 命中，但 1% miss 全部集中在昂贵查询上，数据库仍然会被打爆；或者缓存命中的是陈旧数据，业务正确性出问题；或者所有请求都命中同一个 hot key，单节点 CPU 飙高。

面试里可以这样回答：

```text
缓存系统我会看四类指标：第一是命中效果，包括请求数、hit/miss、按缓存用途拆分的 hit ratio 和 miss reason；第二是延迟和负载，包括 get/set/delete 的 p95/p99、连接池使用和等待；第三是容量和淘汰，包括 used memory、key 数、TTL 分布、expired keys、evicted keys；第四是保护和正确性，包括回源 QPS、填充失败、singleflight 合并、stale read、negative cache。Redis 的 keyspace_hits、keyspace_misses、evicted_keys 可以作为基础指标，但业务侧还要按 cache name 和对象类型拆分。高 hit ratio 不等于健康，还要看 miss 的代价、热点、陈旧数据和回源压力。
```

## Q019. 如何为 RPC 系统设计关键指标？

RPC 系统的指标要覆盖调用方、服务方和传输层。只在 server 端记录 `handler_duration` 不够，因为调用方看到的可能包含连接池等待、负载均衡、重试、hedging、排队、超时和响应传输。

服务级指标可以先用 RED：

```text
Rate：rpc_requests_total 或 rpc_started_total，按 service、method、caller、callee、region 拆分。
Errors：rpc_errors_total，按 status_code、error_type、deadline_exceeded、cancelled、unavailable 拆分。
Duration：rpc_duration_seconds histogram，分别看 client call、client attempt、server call。
```

gRPC OpenTelemetry 指标给了很好的参考：`grpc.client.call.duration` 表示从应用视角完成一次 RPC 的端到端时间；`grpc.client.attempt.duration` 表示某次 attempt 的耗时；`grpc.server.call.duration` 表示 server transport 视角的一次调用耗时；`grpc.status` 可以用来计算错误率；QPS 可以从 duration histogram 的 count 聚合得到。这个设计能避免把一次用户调用和多次重试 attempt 混在一起。

RPC 系统还必须监控 deadline 和取消：

```text
deadline_exceeded_total
cancelled_total
client_timeout_total
server_timeout_total
requests_without_deadline_total
deadline_remaining_ms 分布
```

没有 deadline 的 RPC 会让故障传播更久；deadline 太短会导致误杀；deadline 太长会占住资源。`requests_without_deadline_total` 是很实用的质量指标，因为它能提前暴露“调用方没有定义等待边界”的问题。

连接和负载均衡指标也很关键：

```text
active_connections
connection_attempts_total
connection_failures_total
subchannel_state
lb_pick_latency_seconds
per_backend_inflight_requests
per_backend_error_rate
per_backend_p99_latency
```

RPC 慢不一定是业务慢。可能是客户端连接池满、resolver 返回错误 endpoint、负载均衡把流量打到少数实例、TLS 握手慢、HTTP/2 flow control 阻塞、服务端 accept 队列满。只看方法级 p99 会看到“慢”，但不知道慢在调用前还是调用中。

消息大小和流控也要看：

```text
request_message_bytes
response_message_bytes
compressed_message_bytes
stream_messages_sent_total
stream_messages_received_total
flow_control_blocked_seconds
```

对依赖图要做两个方向的聚合：从 caller 看“我调用每个下游的错误率、延迟、超时、重试”；从 callee 看“谁在调用我，每个 method 的流量、错误率、延迟、payload 大小”。caller/callee 维度要可控，不能把 request_id、user_id 放进 label。

面试里可以这样回答：

```text
RPC 指标我会分 client call、client attempt 和 server call 三层。基础是 RED：请求速率、错误率、延迟分布，按 service、method、status、region、caller/callee 这类低基数维度拆分。gRPC OpenTelemetry 的设计很有参考价值：client call duration 表示一次应用调用总耗时，attempt duration 表示重试或 hedging 中某次尝试，server call duration 表示服务端视角耗时。除此之外还要看 deadline exceeded、cancelled、无 deadline 调用、连接失败、负载均衡分布、in-flight、消息大小和 flow control。这样才能区分业务慢、网络慢、重试放大、连接池排队和下游故障。
```

## Q020. 如何为持久化日志系统设计关键指标？

持久化日志系统，比如 Kafka、Pulsar、NATS JetStream、RocketMQ 或自研 WAL，本质上要保证三件事：写得进、读得出、数据不丢。指标设计也应围绕写入路径、复制持久化、读取消费、存储容量和数据正确性展开。

第一组是写入路径指标：

```text
append_requests_total
append_errors_total
append_latency_seconds
bytes_in_total
records_in_total
batch_size_bytes
fsync_latency_seconds
flush_latency_seconds
write_rejection_total
producer_throttle_time_seconds
```

写入延迟要拆分。客户端看到的 produce latency 可能包括网络、broker 排队、batch、复制等待、磁盘 flush 和 ack。持久化日志最怕只看“broker handler 很快”，却漏掉 page cache flush、磁盘抖动、复制 ISR 变慢。对于要求强持久化的系统，fsync/flush latency、WAL 写入错误、磁盘 I/O 饱和是核心信号。

第二组是复制和一致性指标：

```text
leader_count / follower_count
under_replicated_partitions
in_sync_replica_count
replication_lag_records
replication_lag_bytes
leader_election_total
unclean_leader_election_total
controller_errors_total
```

持久化日志不是“写到 leader 就完事”。如果副本落后，故障时可能扩大恢复时间，甚至在错误配置下造成数据丢失风险。Kafka 这类系统里，under-replicated partitions、ISR 变化、leader election、unclean leader election 都是可靠性指标。

第三组是读取和消费指标：

```text
fetch_requests_total
fetch_errors_total
fetch_latency_seconds
bytes_out_total
records_out_total
consumer_lag_records
consumer_lag_seconds
commit_offset_latency_seconds
read_amplification 或 scan_bytes
```

消费 lag 要同时看 records 和 time。只看落后多少条消息不够，因为不同 topic 的消息速率差异很大。落后 100 万条在高吞吐 topic 里可能只是几秒，在低吞吐 topic 里可能是几天。`lag_seconds` 或“最老未消费消息年龄”更接近业务影响。

第四组是存储和保留：

```text
log_size_bytes
disk_used_ratio
segment_count
segment_roll_total
retention_delete_total
compaction_backlog_bytes
compaction_latency_seconds
oldest_segment_age_seconds
disk_free_bytes
```

持久化日志的存储成本和可靠性强相关。保留时间太短，消费者故障后可能追不上，数据被删；保留时间太长，磁盘成本上升，compaction 和恢复变慢。监控不仅要看当前磁盘使用率，还要看增长速度和预计填满时间。Google SRE 的 saturation 信号里也包含“磁盘将在多久后填满”这类预测。

第五组是端到端语义和数据质量：

```text
duplicate_records_detected_total
out_of_order_records_total
checksum_errors_total
corrupt_segment_total
replay_recovery_duration_seconds
truncation_total
idempotent_producer_errors_total
transaction_abort_total
```

日志能写入不代表语义正确。重复、乱序、截断、事务 abort、幂等 producer 失败、checksum 错误，都会影响下游状态。这类指标不一定高频触发，但一旦出现，优先级通常高于普通延迟抖动。

还要把指标和 SLO 对齐：

```text
写入 SLO：99.9% append 在 50ms 内被多数副本确认。
读取 SLO：99% fetch 在 100ms 内返回。
新鲜度 SLO：99% 消息从 append 到指定 consumer group 可处理小于 30s。
持久性目标：已 ack 的记录在单 broker 故障下不丢失。
```

不同配置下指标解释也不同。`acks=1`、`acks=all`、同步刷盘、异步刷盘、多副本、单副本，代表完全不同的风险边界。回答持久化日志系统指标时，要主动说明监控必须理解 ack 语义和持久化策略，否则同一个 produce latency 或 error rate 没法解释可靠性。

面试里可以这样回答：

```text
持久化日志系统指标要覆盖写入、复制、读取、存储和正确性。写入侧看 append rate、append error、produce latency、batch size、fsync/flush latency、throttle；复制侧看 ISR、under-replicated partitions、replication lag、leader election、unclean election；读取侧看 fetch latency、bytes out、consumer lag records 和 lag seconds；存储侧看 log size、磁盘使用率、segment、retention、compaction backlog 和预计填满时间；正确性侧看 checksum error、重复、乱序、截断、事务 abort 和恢复耗时。关键是把这些指标和 ack 语义、复制策略、保留策略、SLO 绑定，否则只看吞吐和平均延迟会漏掉数据丢失和恢复风险。
```

## Q021. 日志采样会带来什么风险？

日志采样的直接收益是降成本：少写日志、少传输、少索引、少存储。风险也很直接：你主动丢掉了一部分事故证据。日志和 metrics、trace 不一样，很多时候日志是事后还原细节的唯一材料。采样做错了，事故发生时你可能只知道“有问题”，却看不到足够的上下文。

第一类风险是稀有事件被采掉。线上最有价值的日志往往不是高频正常请求，而是低频错误、边界输入、降级路径、权限拒绝、重试耗尽、状态机非法迁移、数据修复动作。如果按固定比例随机采样，比如 1%，一个本来每小时只出现 3 次的异常，很可能整小时都看不到。结果是 metrics 报错了，trace 也许没有命中，日志里没有细节。

第二类风险是时间线断裂。排障时经常需要看同一个 request_id、trace_id、job_id、tenant_id 的连续事件：入口接收、鉴权、入队、下游调用、重试、写库、ack。独立随机采样每一行日志，会把链路切碎。你看到第一行和最后一行，中间关键状态丢了，反而更难判断问题发生在哪一步。

第三类风险是统计失真。采样日志不能直接拿来做错误率、QPS、TopN 用户、TopN 错误码，除非采样率和采样策略被记录并用于校正。更麻烦的是，采样通常不是均匀的：错误日志可能全量保留，info 日志按比例保留，某些租户或接口又有特殊策略。把这种日志直接聚合，很容易得出错误结论。

第四类风险是审计和合规问题。安全审计、计费、权限变更、数据删除、管理员操作、风控命中、密钥访问这类日志通常不能随便采样。它们不是普通调试日志，而是证据。采样后可能无法回答“谁在什么时候做了什么”，也无法满足追责或合规要求。

第五类风险是采样策略本身隐藏事故。比如只按日志级别采样，保留 error，丢弃 info/debug。听起来合理，但很多线上事故一开始不是 error，而是“慢慢变怪”：重试次数上升、缓存 miss 增加、队列等待变长、fallback 被触发。它们可能只出现在 info 或 warn 里。等 error 出现时，已经错过了早期线索。

更稳妥的做法是分层采样：

```text
永不采样：安全审计、计费、权限、数据删除、状态机关键变迁、致命错误。
尽量全量：error、panic、超时、重试耗尽、DLQ、数据不一致。
可采样：高频成功访问日志、重复的健康检查日志、低价值调试日志。
按 key 采样：用 trace_id/request_id/tenant_id 做一致性采样，保留同一条链路的完整性。
记录采样率：每条被保留日志或每个窗口保留采样策略，方便估算真实量级。
```

日志采样还要和 metrics 配套。即使日志被采样，错误计数、请求计数、丢弃日志数量、采样率本身应该用 metrics 全量记录。这样至少能知道“有多少日志被丢掉”“这个错误真实发生了多少次”。OpenTelemetry sampling 文档也提醒，采样虽然能降低可观测成本，但无效采样会带来错过关键信息的机会成本。

面试里可以这样回答：

```text
日志采样的风险是丢事故证据。它可能采掉低频但关键的异常，打断同一个 request_id 或 trace_id 的时间线，让基于日志的统计失真，还可能破坏审计和合规要求。我的原则是：安全审计、计费、权限变更、数据删除、状态机关键事件、error、panic、超时、重试耗尽这类日志不采样；高频成功访问和低价值 debug 日志可以采样。采样最好按 request_id/trace_id 做一致性采样，保留链路完整性，并用 metrics 全量记录真实计数、采样率和被丢弃日志数量。
```

## Q022. trace sampling 如何避免错过慢请求？

避免错过慢请求，不能只靠固定比例 head sampling。慢请求通常是少数样本，1% 随机采样很容易把真正有价值的慢 trace 丢掉。正确思路是：metrics 全量看分布，trace 有选择地保留解释样本。

第一步是延迟分布必须来自 metrics histogram，而不是来自采样 trace。p99、SLO burn rate、超时率应该用全量或近似全量的指标计算。trace 用来解释“为什么慢”，不应该作为唯一的慢请求检测来源。否则采样本身会影响你看到的 p99。

第二步是用 tail-based sampling 保留慢 trace。OpenTelemetry 文档对 tail sampling 的定义是：等到看到一条 trace 的全部或大部分 span 后，再根据 trace 内部信息决定是否保留。这样就可以按整体 latency、error、特定属性、服务名、接口名做策略。OpenTelemetry Collector 的 tail sampling processor 也支持 latency policy，例如超过某个阈值的 trace 保留。

一个常见策略是：

```text
错误 trace：全量保留。
超时或 deadline_exceeded：全量保留。
整体 latency 超过 SLO 阈值：全量保留或高比例保留。
新发布版本、灰度流量：提高采样率。
低流量关键接口：设置最低保留量，而不是纯百分比采样。
普通成功快请求：低比例概率采样，保留基线样本。
```

第三步是把 metrics exemplar 和 trace 关联起来。对 latency histogram 的慢 bucket，可以附带 trace_id exemplar。这样值班时看到 p99 bucket 抬高，可以直接跳到代表性慢 trace，而不是在海量 trace 里搜索。即使 trace 采样率不高，只要慢 bucket 的 exemplar 策略合理，排障效率会高很多。

第四步是调整 tail sampling 的等待窗口。tail sampling 需要等 span 到齐，所以会有 `decision_wait`、缓存容量、late span 等问题。等待太短，慢 trace 的后半段还没到就做了错误决定；等待太长，Collector 内存和决策延迟上升。实际配置要根据服务最长正常耗时、span 上报延迟、Collector 容量来定，并监控 sampling processor 自身的 dropped traces、decision latency、队列长度。

第五步是分层采样。只用“latency > 1s”会漏掉对某些接口来说已经很慢的请求；只用“p99 阈值”又可能漏掉低流量接口。更好的方式是按 service、route、operation、tenant tier 设不同阈值，或者用 SLO good event 的定义判断。支付接口 300ms 可能已经慢，离线报表 3s 可能正常。

还要避免一个误区：为了不漏慢请求而全量采样所有 trace。短期事故排查可以临时提高采样率，但长期全量 trace 成本很高，Collector 和后端也可能被打爆。更稳的方案是全量 metrics、选择性 tail sampling、错误全量、慢请求高保留、普通流量低比例保留。

面试里可以这样回答：

```text
不要用固定 1% head sampling 来期待抓到慢请求。p99 和 SLO 应该由全量 metrics histogram 计算，trace 用来解释慢在哪里。为了避免错过慢请求，我会用 tail-based sampling：错误、超时、deadline_exceeded、整体延迟超过阈值的 trace 全量或高比例保留，普通成功快请求低比例保留；同时用 exemplar 把慢 bucket 关联到具体 trace。tail sampling 要设置合理的 decision_wait 和缓存容量，并按不同接口的 SLO 设置不同慢阈值，否则低流量关键接口和局部长尾仍然会被漏掉。
```

## Q023. head-based sampling 和 tail-based sampling 有什么区别？

head-based sampling 是在 trace 刚开始时做决定。通常入口服务根据 trace_id 和采样率决定这条 trace 是否采样，然后把 sampled 标志通过 trace context 传播给后续服务。它的优点是简单、便宜、延迟低，不需要在 Collector 里缓存整条 trace。OpenTelemetry 文档也把它描述为尽早做采样决定，并且不检查整条 trace。

head sampling 的缺点也来自这个“太早”。入口服务做决定时，还不知道后面会不会出错，不知道数据库会不会慢，不知道某个下游会不会超时。所以它很难保证“所有错误 trace 都保留”或“所有慢 trace 都保留”。如果采样率是 1%，99% 的慢请求也可能被丢掉。

tail-based sampling 是在 trace 结束后或接近结束时做决定。Collector 或采样代理先缓存 span，等看到整条 trace 的大部分信息后，再根据策略决定保留还是丢弃。这样可以按错误、总耗时、span 属性、服务名、接口名、租户等级、新版本等条件采样。它适合大规模系统里“不能全量保留，但又不想丢关键 trace”的场景。

tail sampling 的代价是复杂度和资源。它需要缓存 trace，需要等待 span 到齐，需要处理乱序和 late span，还要考虑 Collector 重启、内存上限、决策延迟。策略写得不好，还可能在高峰期丢掉本来应该保留的 trace。OpenTelemetry 文档也提到 tail sampling 更复杂，有额外计算和维护成本。

可以对比成这样：

```text
head-based sampling：
决策时间：trace 开始时。
依据：trace_id、入口属性、父级 sampled 标志、固定概率。
优点：便宜、简单、低延迟、容易理解。
缺点：无法根据完整 trace 的错误和耗时做决定。

tail-based sampling：
决策时间：trace 结束后或大部分 span 到齐后。
依据：错误、总延迟、span 属性、服务组合、接口、租户、新版本。
优点：能保留慢 trace、错误 trace、关键业务 trace。
缺点：需要缓存、等待、更多 Collector 资源和策略维护。
```

生产里经常混合使用。比如入口做很低比例的 head sampling 作为基线，同时在 Collector 做 tail sampling 保留错误和慢 trace；或者对关键租户、灰度版本、低流量关键接口提高 head 采样率，避免 tail 采样前就被 SDK 丢掉。要注意一点：如果 SDK 在 head 阶段已经完全不生成或不上报 span，Collector 后面的 tail sampling 就没机会救回来。要做 tail sampling，通常需要让上游把候选 trace 发到 Collector。

面试里可以这样回答：

```text
head-based sampling 在 trace 开始时决定是否采样，通常根据 trace_id 和采样率做概率采样，优点是简单、便宜、低延迟，缺点是当时还不知道这条 trace 后面会不会慢或报错。tail-based sampling 等整条 trace 的全部或大部分 span 到齐后再决定，可以按错误、总延迟、span 属性、接口、租户、新版本等条件保留，更适合抓慢请求和错误请求，但需要 Collector 缓存 trace，消耗内存，并处理 late span、乱序和决策延迟。实际系统里常把两者组合使用。
```

## Q024. 如何设计报警避免噪声？

报警的目标不是“把所有异常都通知人”，而是在需要人采取行动时及时通知。Google SRE 和 Prometheus 的建议都很明确：报警要简单，优先对用户症状报警，避免没有行动项的 page。噪声报警的代价很高，值班人员会疲劳、跳过、静音，真正事故反而更容易被淹没。

第一条原则是报警要有行动项。每条 page 都应该能回答：谁负责？用户影响是什么？现在要做什么？在哪里看 dashboard？对应 runbook 是什么？如果一条报警只是在说“某个内部指标有点奇怪”，没有明确处理动作，它更适合做 dashboard 或工单，不适合半夜叫醒人。

第二条原则是尽量按用户症状报警。在线服务优先看高层错误率、延迟、可用性、SLO burn rate；离线任务看数据新鲜度、任务是否按时完成；队列系统看最老消息年龄和端到端处理延迟。底层原因指标可以用于排障，但不要每个原因都 page。一台机器 CPU 高、一次 GC 慢、某个下游 p99 抖一下，如果没有用户影响，不应该直接打断值班。

第三条原则是用时间窗口过滤小抖动。Prometheus alerting rules 的 `for` 可以要求条件持续一段时间才 firing，`keep_firing_for` 可以减少恢复和触发之间来回抖动。短窗口太敏感，长窗口发现慢。SLO 告警通常用多窗口、多 burn rate：短窗口发现快速事故，长窗口避免瞬时噪声。

第四条原则是分级。不是所有异常都 page：

```text
page：正在明显消耗 error budget，用户已经受影响，且需要立即人工处理。
ticket：慢性预算消耗、容量趋势、非紧急可靠性债务。
info：部署事件、自动恢复、低风险降级，只进入事件流或 dashboard。
```

第五条原则是去重、聚合、抑制。一个下游故障可能让 20 个上游同时报警。如果每个服务都 page，值班只会被淹没。Alertmanager 这类系统要按 service、cluster、alertname 聚合；已知根因报警触发后，可以抑制派生报警；维护窗口、发布窗口、演练窗口要有明确静默策略。

第六条原则是定期清理报警。每次报警都应该被分类：有效事故、有效但不可行动、误报、重复、阈值不合适、缺少 runbook。长期没有人处理、没有触发过、触发后总是被忽略的报警，要下线或降级。报警系统本身也要监控，Prometheus 文档把 metamonitoring 单独列出来，就是因为监控坏了会让人对所有报警失去信心。

面试里可以这样回答：

```text
避免报警噪声的核心是只 page 需要人立即行动的用户影响。报警应该基于 SLO、错误率、延迟、数据新鲜度、队列年龄这类症状指标，而不是对每个可能原因都报警。规则上用 for 持续时间、多窗口 burn rate、分级通知、去重聚合、抑制和维护窗口减少抖动。每条报警要有 owner、runbook、dashboard 链接和明确动作。报警触发后要复盘它是不是有效，长期无行动项或重复的报警应该降级或删除。
```

## Q025. 症状报警和原因报警有什么区别？

症状报警关注用户或业务已经感受到的问题。比如 API 错误率升高、p99 超过 SLO、支付成功率下降、消息处理延迟超过 5 分钟、数据 30 分钟没有刷新。这类报警回答的是：“用户现在是不是受影响？”

原因报警关注内部机制或资源异常。比如 CPU 95%、磁盘 I/O await 升高、数据库连接池耗尽、锁竞争增加、GC pause 变长、某个下游 RPC 慢、Kafka consumer lag 上升。这类报警回答的是：“系统内部哪里可能出问题？”

Prometheus 的报警实践建议优先对症状报警，原因用于帮助定位。理由很现实：同一个用户症状可能有很多原因；同一个原因也不一定造成用户影响。如果每个潜在原因都 page，报警会爆炸。比如数据库读延迟升高，但缓存命中率很高，用户请求没有变慢，就没有必要半夜叫人。反过来，用户 p99 已经爆了，即使还不知道原因，也应该 page。

两者的关系可以这样看：

```text
症状报警：
API 5xx > SLO burn rate。
支付端到端成功率下降。
队列最老消息年龄超过业务承诺。
用户可见页面加载 p99 过高。

原因报警：
数据库慢查询增加。
磁盘 fsync latency 升高。
Go mutex contention 增加。
连接池等待时间上升。
CPU run queue 过长。
```

原因报警不是没用，而是要控制级别。它适合做三类事：第一，容量风险，比如磁盘 4 小时后会满，这还没影响用户，但需要提前处理；第二，数据安全风险，比如副本不足、unclean leader election、持久化错误，这类可能还没体现为用户请求失败，但风险很高；第三，明确可行动的内部故障，比如某个后台任务已经停止，过一段时间会导致用户数据不新鲜。

好的告警体系通常是：症状报警负责叫醒人，原因信号负责指路。报警消息里应该直接链接到 dashboard，dashboard 上展示最近发布、依赖延迟、资源饱和、错误日志、慢 trace、容量趋势。这样值班收到的是一个高质量入口，而不是 100 条互相打架的原因报警。

面试里可以这样回答：

```text
症状报警关注用户是否受影响，比如错误率、p99、可用性、数据新鲜度、队列最老消息年龄；原因报警关注内部机制，比如 CPU 高、锁竞争、fsync 慢、连接池耗尽、下游 RPC 慢。线上 page 应优先基于症状，因为原因很多且不一定造成用户影响。原因指标应该放在 dashboard、runbook 或低优先级 ticket 里，用来帮助定位。例外是容量即将耗尽、数据安全风险、副本不足这类还没表现成用户症状但必须提前处理的问题。
```

## Q026. 报警阈值应该固定还是基于历史基线？

固定阈值和历史基线都需要，关键看报警目标。对 SLO、用户承诺、协议限制、资源硬上限，固定阈值更可靠；对强季节性、流量异常、业务自然波动，历史基线更有用。不要把它们当成二选一。

固定阈值适合这些场景：

```text
SLO：99% 请求 < 300ms，错误率 < 0.1%。
容量硬限制：磁盘使用率 > 85%，连接池剩余 < 10%。
协议或业务边界：队列最老消息年龄 > 5min，证书 7 天后过期。
正确性：checksum error、数据丢失、权限绕过，非零就要看。
```

固定阈值的优点是清楚、可解释、可复盘。你可以直接说“超过这个值用户就受影响”或“再不处理就会耗尽资源”。缺点是对自然波动不敏感：白天 QPS 是夜间 10 倍，用同一个固定流量阈值报警，可能白天太迟，夜间太吵。

历史基线适合这些场景：

```text
流量突增或突降：当前 QPS 和过去同一时间段明显不同。
业务转化率：支付成功率、下单率和历史季节性偏离。
成本和资源趋势：某服务成本突然高于过去 7 天同时间段。
异常分布：某地区、某版本、某租户的错误率偏离自身历史。
```

基线的优点是能适应昼夜周期、周末效应和业务季节性。缺点是容易把坏状态学成正常。如果一个系统连续三周慢慢退化，基线也会跟着退化，最后不报警。基线算法还容易在低流量服务上误报：每小时只有 10 个请求，失败 1 个就是 10%，这未必值得 page。

我更倾向于这种组合：

```text
page：固定 SLO 阈值或 error budget burn rate。
ticket：历史基线发现慢性偏移或趋势异常。
dashboard：同时显示当前值、SLO 线、昨天同时间、上周同时间。
保护条件：低流量时加最小样本数，避免一个请求触发大事故报警。
```

阈值还要能解释。复杂模型可以用于提示，但半夜 page 最好简单明确。Google SRE 也提到，他们倾向于简单快速的监控规则，对复杂自动因果系统很谨慎。值班需要知道为什么被叫醒，而不是面对一个无法解释的异常分数。

面试里可以这样回答：

```text
报警阈值不应该只选固定或只选历史基线。SLO、用户承诺、容量硬上限、数据正确性、安全风险适合固定阈值，因为它们需要清楚可解释；流量异常、业务转化率、成本趋势、强周期指标适合历史基线。我的做法是 page 主要基于固定 SLO 或 error budget burn rate，基线更多用于趋势异常和工单。基线要加最小样本数和季节性窗口，也要防止把长期退化学成正常。
```

## Q027. 如何从 p99 升高定位到锁竞争？

从 p99 升高定位到锁竞争，先不要直接跳到“锁有问题”。p99 升高只是症状，可能来自下游慢、GC、磁盘、网络、连接池、队列、CPU 饱和。锁竞争的典型特征是：吞吐上不去，p50 可能还好，p99 随并发上升明显变差，CPU 不一定打满，但 goroutine 或线程大量等待。

第一步是缩小影响面。按 route、method、tenant、版本、实例、可用区拆 p99，看是不是某个接口、某类请求、某个版本开始变慢。如果所有接口都慢，更像全局资源问题；如果只有写路径或某个共享状态路径慢，锁竞争概率会上升。

第二步是看 trace 和应用分段。慢 trace 里如果业务逻辑本身没有下游 RPC、SQL、fsync 这类长 span，但请求在某个本地操作前后耗时很长，就要怀疑本地等待。更好的埋点是显式记录锁等待：

```text
lock_wait_duration_seconds
lock_hold_duration_seconds
lock_contention_total
critical_section_name
```

没有这些指标时，可以用运行时 profile。Go 里 `runtime/pprof` 的 block profile 会记录在同步原语上阻塞的时间，mutex profile 会记录 contended mutex 的持有者。Go 官方文档说明，block profile 覆盖 mutex、RWMutex、WaitGroup、Cond、channel send/receive/select 等同步阻塞；mutex profile 会把竞争归到造成其他 goroutine 等待的临界区释放栈上。`net/http/pprof` 也支持抓 `/debug/pprof/block` 和 `/debug/pprof/mutex`。

第三步是对齐时间。p99 升高的时间窗口里，mutex profile 的热点栈是否同步上升？goroutine dump 里是否大量卡在 `sync.Mutex.Lock`、`sync.RWMutex.RLock`、channel receive/send？实例的 in-flight 请求是否上升，但 CPU 使用率不高？如果这些信号同时出现，锁竞争就比较可信。

第四步是判断锁在哪里被放大。常见问题包括：

```text
临界区里做 I/O、RPC、日志刷盘、JSON 序列化。
全局 map 或全局状态用一把大锁保护。
读多写少场景写锁持有时间过长，阻塞所有读。
热点 key 被所有请求争用。
为了统计或缓存更新，在请求热路径上拿锁。
锁内调用用户回调或复杂业务逻辑。
```

第五步是用实验验证。临时降低并发、关闭某个功能、切流到旧版本、分片锁、把 I/O 移出临界区、对热点 key 做 sharding。如果 p99 随这些动作明显下降，说明定位方向正确。不要只凭一次 profile 下结论，profile 是采样数据，要和指标、trace、变更记录一起看。

面试里可以这样回答：

```text
我会先按接口、版本、实例缩小 p99 升高范围，再看慢 trace 里时间是否消失在本地临界区附近。如果怀疑锁竞争，会补或查看 lock_wait、lock_hold、contention 指标；在 Go 服务里抓 block profile、mutex profile 和 goroutine dump。锁竞争通常表现为 p99 随并发上升、in-flight 增加、CPU 不一定满、大量 goroutine 卡在 Mutex/RWMutex/channel。最后要把 profile 热点栈和 p99 时间窗口对齐，再通过缩小临界区、分片锁、移出 I/O、降低并发或回滚版本验证。
```

## Q028. 如何从 p99 升高定位到 fsync 慢？

fsync 慢常出现在写路径：WAL、持久化日志、事务提交、checkpoint、文件上传元数据、嵌入式数据库、审计日志。它的特点是平均延迟可能没太大变化，但某些批次或某些时间点 p99 被磁盘 flush 拉高。

先理解 fsync 的语义。Linux `fsync(2)` 会把文件的修改数据和相关元数据刷到永久存储，并且调用会阻塞，直到设备报告传输完成。也就是说，普通 write 可能只是写进 page cache，看起来很快；真正的设备延迟和 flush 压力会在 fsync 或 fdatasync 时暴露出来。

定位时先看应用层分段。写请求的 trace 或日志里应拆出：

```text
serialize_time
wal_append_time
write_syscall_time
fsync_duration
commit_wait_time
batch_wait_time
```

如果 p99 升高的窗口里 `fsync_duration` 或 `commit_wait_time` 同步升高，方向就很明确。如果没有应用埋点，可以临时加 syscall tracing、eBPF、strace 采样，或者在 WAL/日志模块周围打点。注意不要在高峰期用过重的 tracing 把系统再拖慢。

然后看系统层 I/O 指标。`iostat -x` 里的 `await` 表示 I/O 请求从排队到服务完成的平均时间，`w_await` 是写请求等待，`f_await` 是 flush 请求等待，`aqu-sz` 是平均队列长度，`%util` 接近 100% 在串行设备上常表示饱和。iostat 文档也提醒，现代 SSD 和 RAID 并行能力更强，`%util` 不能单独作为性能上限判断，所以要结合 await、队列长度、吞吐、设备类型一起看。

还要对齐这些事件：

```text
segment roll 或 log rotation。
checkpoint 或 compaction。
批量写入、导入、备份。
磁盘空间接近满、文件系统 journal 压力。
同机其他进程写盘。
云盘突发额度耗尽或 IOPS 限流。
```

fsync 慢和锁竞争有时会混在一起。比如全局锁内做 fsync，表面看 mutex profile 显示锁竞争，根因其实是锁持有期间等待磁盘。慢 trace、mutex profile、fsync_duration 要一起看：如果锁持有栈里包含持久化提交或日志 flush，优化点不是只换锁，而是把 fsync 从共享临界区里移出去，或者改 group commit。

常见缓解手段包括：批量提交、group commit、把每条消息 fsync 改为按批次 fsync、降低同步刷盘频率、使用 fdatasync 减少不必要 metadata flush、把 WAL 放到更稳定的盘、隔离日志盘和数据盘、控制 checkpoint 和 compaction 并发。每个手段都可能改变持久性语义，不能只看延迟收益。

面试里可以这样回答：

```text
定位 fsync 慢，我会先确认慢的是写路径，并在应用层拆出 wal append、write syscall、fsync、commit wait、batch wait。Linux fsync 会阻塞到数据和相关元数据刷到存储设备完成，所以普通 write 快不代表持久化快。系统层看 iostat 的 w_await、f_await、aqu-sz、iowait、吞吐和设备限流，必要时用 eBPF 或 syscall tracing 看 fsync 分布。还要对齐 checkpoint、compaction、log rotation、云盘额度、同机写盘。修复可能是 group commit、批量 fsync、fdatasync、隔离磁盘或调整 checkpoint，但必须说明这些会影响持久性边界。
```

## Q029. 如何从 p99 升高定位到下游 RPC 慢？

下游 RPC 慢是 p99 升高的常见原因，尤其在 fan-out 服务里，一个入口请求要调用多个后端，任何一个后端的尾部延迟都可能放大到入口 p99。定位时要先看 trace，而不是只看入口服务自己的 CPU 或日志。

第一步是用 trace 找关键路径。慢请求里，哪个 span 占了最长 wall time？是 `database.query`，还是 `pricing.GetQuote`，还是 `inventory.CheckStock`？如果入口 span 1.2s，其中下游 RPC span 占 1.0s，方向很清楚。还要看是串行等待还是并行 fan-out 中最慢的一个分支决定总耗时。

第二步是区分 client 视角和 server 视角。gRPC OpenTelemetry 指标里区分 `grpc.client.call.duration`、`grpc.client.attempt.duration` 和 `grpc.server.call.duration`。这很关键：

```text
client call 慢，server call 也慢：下游服务处理慢。
client call 慢，server call 不慢：网络、负载均衡、连接池、排队、重试、客户端等待可能有问题。
attempt 不慢，但 call 慢：重试、hedging、retry delay 或连接建立拉长了总耗时。
server call 慢，但入口不慢：可能被缓存、降级、并行掩盖，暂时不是用户症状。
```

第三步是按依赖拆指标。入口服务需要有 per dependency、per method、per status 的 RPC 指标：请求数、错误率、p95/p99、timeout、deadline_exceeded、cancelled、retry_count、in_flight、连接池等待、每个 backend 的负载分布。只看总 p99 不够，因为一个低流量但关键的下游可能被聚合淹没。

第四步是检查 deadline 和重试。gRPC 文档强调客户端应该设置 realistic deadline；没有 deadline 的请求可能一直等，deadline 太短又会导致大量 `DEADLINE_EXCEEDED`。重试也会放大 p99：一次用户调用可能包含多个 attempt 和 backoff delay。gRPC retry 文档也提醒，重试会替换失败调用并重新发起新 attempt，应用需要理解哪些操作适合重试、退避参数和重试次数。

第五步是看下游自己的症状。如果下游服务的 server p99、错误率、队列长度、CPU、连接池、数据库依赖也同时异常，说明问题在下游内部。如果只有调用方到下游慢，而下游自报很快，就要看网络、LB、DNS、连接复用、TLS、跨区流量、客户端连接池、限流或服务发现。

最后要注意 fan-out 放大。假设入口请求并发调用 20 个下游，即使每个下游 p99 都还行，入口“至少一个慢”的概率也会升高。这个时候优化单个下游 p99、减少 fan-out 数量、设置 deadline budget、做部分结果降级、缓存热点结果，都可能比盯入口代码更有效。

面试里可以这样回答：

```text
我会先用 trace 看慢请求的关键路径，确认入口 p99 是否被某个下游 span 占满。然后区分 client call、client attempt 和 server call：client 和 server 都慢，多半是下游处理慢；client 慢但 server 不慢，可能是网络、连接池、LB、重试或排队；attempt 不慢但 call 慢，要看重试和 backoff。指标上按 dependency/method/status 拆请求量、错误率、p99、deadline_exceeded、retry_count、in-flight 和连接池等待。还要看 fan-out 放大，因为入口请求依赖越多，任一慢分支都会抬高入口 p99。
```

## Q030. 如何设计 dashboard 支持值班排障？

值班 dashboard 的目标不是展示所有数据，而是让人在压力下快速回答三个问题：用户是否受影响，影响范围在哪里，下一步该查什么。Prometheus dashboard 文档也提醒，不要把所有数据堆到一个大屏；运营视角的 dashboard 应围绕最可能的失败模式，帮助区分原因。

第一屏应该是症状层。对在线服务，放四个黄金信号或 RED：流量、错误率、延迟分布、饱和度。对队列系统，放生产速率、消费速率、最老消息年龄、in-flight、DLQ。对数据流水线，放数据新鲜度、处理延迟、失败任务。第一屏不要先放 CPU、内存、GC 这种原因指标，否则值班需要自己判断它们是否真的影响用户。

第二层是影响范围。dashboard 应该能按 service、route、region、cluster、version、tenant tier 展开，但默认聚合要清楚。值班最常问的是：只有一个区域吗？只有新版本吗？只有某个接口吗？只有某类租户吗？如果每次都要手写 PromQL 才能回答，dashboard 就没有完成任务。

第三层是依赖和关键路径。服务 dashboard 应展示它调用的下游 RPC、数据库、缓存、队列、对象存储的延迟、错误率和超时。Grafana 的文档建议按层级和 drill-down 组织 dashboard，避免随机浏览。一个实用布局是：从入口服务开始，点到依赖服务，再点到依赖的资源层。

第四层是近期变化。很多事故来自变更，所以 dashboard 要直接显示或链接：最近部署、配置变更、灰度比例、实例重启、扩缩容、依赖版本、feature flag。p99 升高时，如果旁边能看到“10 分钟前发布 v1.8.3 到 20% 流量”，排障速度会快很多。

第五层是证据跳转。每个报警应该链接到对应 dashboard，每个 dashboard 应该能跳到日志查询、trace 查询、runbook、服务 owner、最近发布记录。日志和 trace 链接最好自动带上 service、route、region、version、时间窗口。值班最怕的是报警说“APIHighLatency”，然后还要自己猜该打开哪个系统。

设计上要克制。Prometheus 文档建议运营 console 不要塞太多图，Grafana 文档也强调 dashboard 要回答明确问题、降低认知负担。一个页面放 50 个 panel，看起来完整，实际会让人迷路。更好的方式是 overview 少而准，服务页有 drill-down，资源页按 USE 展开。

一个值班友好的 dashboard 可以这样组织：

```text
Overview：SLO burn rate、error rate、p95/p99、traffic、saturation、active incidents。
Service：按 route/method/status/version/region 拆 RED 指标。
Dependencies：下游 RPC、DB、cache、queue 的 latency/error/timeout。
Resources：CPU、memory、GC、disk I/O、fsync、network、connection pool、thread/goroutine。
Changes：deploy、config、feature flag、autoscaling、instance restart。
Links：runbook、logs、traces、profiling、owner、rollback 操作说明。
```

面试里可以这样回答：

```text
值班 dashboard 要从用户症状开始，而不是从机器指标开始。第一屏放 SLO burn rate、流量、错误率、p95/p99、饱和度；第二层支持按 route、region、version、tenant tier 判断影响范围；第三层展示下游 RPC、DB、缓存、队列的延迟和错误；再往下才是 CPU、内存、GC、磁盘、锁、连接池这些原因指标。每个报警要链接到对应 dashboard，dashboard 要能跳到日志、trace、profile、runbook 和最近变更。面板要少而准，默认回答值班最关心的问题：用户是否受影响，影响在哪里，下一步查什么。
```

## Q031. 如何把业务指标和系统指标关联起来？

业务指标和系统指标的关联，核心不是把两类图放在同一个 dashboard 上，而是建立一条可解释的链：

```text
业务结果 -> 用户可感知症状 -> 服务层 SLI -> 依赖和资源层原因 -> 具体日志/trace 证据
```

比如一个在线下单系统里，业务指标可能是 `checkout_success_rate`、`payment_success_rate`、`order_created_total`、`revenue`、`active_users`。系统指标可能是 `http_request_duration_seconds`、`http_requests_total`、`grpc_client_call_duration`、数据库连接池等待、队列 backlog、CPU、内存、磁盘 I/O。它们之间不能只靠肉眼看曲线相似。更稳妥的做法是先定义业务流程，再为流程里的关键系统步骤建模。

拿 LogServe 这种 workflow runtime 举例，业务侧关心的可能不是 CPU，而是：

```text
workflow 提交后是否完成？
用户等待一个 workflow 结果要多久？
任务失败后是否能被重试或恢复？
LLM 请求是否命中本地模型缓存？
actor 状态是否能从 snapshot + tail log 恢复？
```

这些问题可以落成业务/产品语义更强的指标：

```text
logserve_workflow_submitted_total
logserve_workflow_succeeded_total
logserve_workflow_failed_total
logserve_workflow_duration_seconds
logserve_task_redelivered_total
logserve_llm_cache_hit_ratio
logserve_actor_replay_commands
```

然后再把它们和系统指标关联起来：

```text
workflow p99 升高
  -> task queue age 升高
  -> worker in-flight 达到上限
  -> executor pool queue wait 升高
  -> 某类 Python task 或 LLM call 的 span 变慢

LLM 请求成功率下降
  -> model cache hit rate 下降
  -> checkpoint fetch latency 上升
  -> worker-local cache miss 增加
  -> 对象存储或本地磁盘 I/O 异常
```

这里有一个重要原则：业务维度要进入遥测上下文，但不一定都进入指标 label。Prometheus 文档反复提醒 label cardinality 会带来 RAM、CPU、磁盘和网络成本。`user_id`、`order_id`、`request_id`、`workflow_id` 这类高基数字段通常不适合作为 metric label；它们适合放在日志字段、trace attribute 或 exemplars 里。适合进入指标 label 的，是低基数、稳定、能用于聚合判断的维度：

```text
service
route / operation
status
region / zone
deployment_version
tenant_tier
workflow_type
model_name
cache_hit
error_class
```

业务和系统指标关联时，最好统一几类公共维度。OpenTelemetry 的 resource 描述的是产生 telemetry 的实体，例如 service name、service version、deployment environment。trace、log、metrics 都带这些资源属性后，才能可靠地问：

```text
是不是只有 v1.8.3 版本出问题？
是不是只有 us-east-1 这个 region 出问题？
是不是 premium tenant 的 checkout p99 更高？
是不是 mock LLM 和 vLLM adapter 的延迟分布不同？
```

跨请求的业务上下文可以通过 context propagation 传递。OpenTelemetry baggage 适合传播少量跨进程上下文，例如 tenant tier、experiment group、workflow type。它不适合塞敏感数据，也不适合塞无限增长的业务字段，因为 baggage 会跟着请求传播，放大网络、日志和隐私风险。

一个比较实用的分层方式是：

```text
业务 KPI：订单数、成功率、收入、活跃用户、任务完成率。
用户 SLI：请求成功率、端到端延迟、数据新鲜度、workflow 完成时间。
服务 RED：rate、errors、duration。
资源 USE：utilization、saturation、errors。
证据层：trace_id、span_id、request_id、structured logs、profile。
```

在 dashboard 上，不要把业务指标孤立放在最上面，然后把 CPU 放在最下面就算关联完成。更好的方式是为每个业务流程建立一张路径图。比如 checkout：

```text
checkout_success_rate
checkout_request_duration p99
api_gateway -> checkout-service -> inventory -> payment -> risk-check -> order-db
每个依赖的 client/server latency、error、timeout、retry
最近 deploy / config / feature flag
日志和 trace 跳转
```

这样值班时看到业务成功率下降，可以沿着同一条链往下查，而不是在几十张机器图里猜。

还要避免一个误区：曲线同时变化不等于因果成立。业务指标和系统指标的关联至少要看四件事：

```text
时间顺序：系统异常是否先于业务下降，还是业务流量变化导致系统压力？
影响范围：同一 region、version、tenant、route 是否同时异常？
trace 证据：慢请求里是否真的卡在对应 span？
修复验证：调整后业务指标和系统指标是否一起恢复？
```

例如 CPU 升高和订单下降同时发生，不一定是 CPU 导致订单下降。可能是促销流量来了，订单请求增加，CPU 升高；真正问题是支付下游限流。这个时候 trace 和下游错误码比 CPU 图更有解释力。

在 LogServe 里，我会把业务指标和系统指标这样挂钩：

```text
workflow_success_rate
  -> workflow step failed total by error_class
  -> task redelivery total
  -> worker executor queue wait
  -> log append/fsync latency

llm_request_duration p99
  -> cache hit ratio
  -> checkpoint fetch duration
  -> model load duration
  -> worker cache capacity / eviction count

actor_recovery_duration
  -> replay command count
  -> snapshot age
  -> compactable log bytes
  -> log read latency
```

这也符合项目边界：当前 LogServe 是单机、多进程的机制验证系统，不能把这些指标包装成生产级多节点可靠性结论。但在面试里可以强调，指标设计是按生产系统思路做的：先定义用户或业务可感知的 SLI，再向下关联到 runtime、依赖和资源层。

面试里可以这样回答：

```text
我会先从业务流程定义业务 KPI 和用户 SLI，例如成功率、端到端延迟、任务完成率，而不是从 CPU、内存开始。然后用统一的低基数维度把业务指标和系统指标接起来，比如 service、route、version、region、tenant_tier、workflow_type。高基数字段如 user_id、order_id、workflow_id 不放进 metric label，而是放到日志、trace 或 exemplar 里。排障时路径是：业务指标发现影响，SLI 判断用户症状，RED/USE 指标定位服务和资源，再用 trace/log 证明具体原因。最后要用修复后的业务指标和系统指标一起恢复来验证因果，不把曲线相关误判为根因。
```

## Q032. 如何处理时钟不同步导致的 trace 时间线错乱？

分布式 trace 的时间线看起来像一条连续的调用链，但每个 span 的开始和结束时间通常来自不同机器的本地时钟。只要机器之间有 clock skew，就可能出现很怪的画面：

```text
子 span 看起来早于父 span 开始。
服务 B 的 server span 看起来在服务 A 的 client span 发送前就开始了。
某个跨服务调用出现负耗时。
并行分支的先后顺序和实际因果不一致。
入口请求总耗时比所有子 span 加起来还短或还长很多。
```

这不是 trace 系统一定坏了，而是 wall clock 被拿来表达跨机器顺序时天然有风险。W3C Trace Context 解决的是 trace-id、parent-id、trace-flags、tracestate 怎么跨服务传播，它保证同一次分布式请求能被关联起来，但它不保证所有机器的物理时间完全一致。NTP 这类时间同步协议只能把偏移控制在一定范围内，不能让跨主机时间戳拥有严格因果语义。

处理这个问题要分成三层。

第一层是基础设施上减少偏移。生产环境应该统一启用 NTP、chrony 或 PTP，并监控每台机器的 clock offset、同步状态、stratum、last sync time。虚拟化环境、容器宿主机、长时间 GC pause、系统 suspend/resume、NTP step 调整，都可能让某些进程的时间戳短时间异常。这个问题不能只在 tracing UI 里修，源头还是要保证节点时间健康。

第二层是埋点时正确区分 wall clock 和 monotonic clock。绝对时间戳需要 wall clock，因为日志和 trace 要显示“这件事发生在什么时候”。但本地耗时最好用 monotonic clock 计算，例如一次函数调用、一次 RPC client span 的 duration、一次 fsync 的 duration。单进程内用 monotonic clock 计算 duration，可以避免系统时间被向前或向后调整导致本地耗时变成负数。跨进程的先后顺序，则不能只依赖 wall clock。

第三层是分析时保留因果关系，不要迷信横轴。trace 里真正可靠的结构首先是 parent-child、span links、trace context，而不是不同主机时间戳的绝对先后。服务 A 的 client span 和服务 B 的 server span 存在调用关系，即使 UI 上 server span 看起来提前开始，也应该先把它理解成时钟偏移，而不是认为服务 B 真的提前处理了请求。

有些 tracing 系统会做 clock skew correction。这个思路可以用于展示层：根据父子 span、client/server span、网络调用边界，推测某个 host 的时间偏移，把图画得更符合因果。但我不建议直接改写原始 span 时间。原因有两个：

```text
原始时间戳本身是诊断证据，改写后会掩盖时钟同步问题。
自动校正依赖假设，例如网络延迟对称、父子关系完整，这些假设不总成立。
```

更稳妥的做法是：原始数据保留，展示层或查询层标记 skew correction，必要时给 span 加上 `clock_skew_detected=true`、`observed_offset_ms` 之类的诊断字段。

对于 RPC，可以采集更细的时间点来估计问题：

```text
client_send_time
server_receive_time
server_send_time
client_receive_time
client_duration_ms
server_duration_ms
```

如果 client 侧 duration 是 500ms，server 侧 duration 是 20ms，但 server_receive_time 比 client_send_time 早 2s，就很可能是时钟偏移。此时排查重点不是服务 B 真的提前处理，而是检查两台机器的 clock offset、NTP 状态、虚拟机时间漂移。

异步系统还要格外小心。消息队列、workflow、actor mailbox 里经常不是严格嵌套调用。生产者 span、broker span、消费者 span 可能跨越很长时间，也可能并行处理。OpenTelemetry 里 span links 可以表达“有关联但不是父子嵌套”的关系。面试时要明确：不要为了让图好看，把异步消息都硬塞成父子 span，否则时间线和语义都会错。

在 LogServe 这样的 shared log runtime 里，我会避免用 wall clock 作为状态转移的唯一依据。状态恢复、actor command 顺序、workflow step 去重，更应该依赖 log offset、sequence number、epoch、command_seq、idempotency key。wall clock 可以用于 latency 分析和 dashboard 展示，但不能作为 actor 状态应用顺序或 exactly-once-ish 去重的核心依据。这个边界说清楚，比单纯说“我们会同步时间”更像工程回答。

排障时可以按这个顺序处理：

```text
1. 先确认 trace context 是否完整，trace_id/span_id/parent_id 有没有断。
2. 看异常是否集中在某些 host、zone、node pool 或 runtime。
3. 检查这些机器的 NTP/chrony 状态和 clock offset 指标。
4. 用本地 duration 判断单个 span 是否真的慢。
5. 用 parent-child、span links、日志里的 sequence/log offset 判断因果。
6. 对展示层做 skew correction 或标记，但保留原始 span 时间。
7. 对 clock offset 超阈值的节点报警，并把这些 trace 从精确延迟归因里降权。
```

面试里可以这样回答：

```text
trace 时间线错乱时，我不会先假设调用顺序真的反了。跨机器 span 的绝对时间来自各自主机的 wall clock，NTP 只能减少偏移，不能提供严格因果顺序。处理上先保证基础设施有时间同步，并监控 clock offset；埋点时本地 duration 用 monotonic clock，绝对 timestamp 才用 wall clock；分析时以 trace parent-child、span links、log offset、sequence number 判断因果。UI 可以做 clock skew correction，但原始 span 时间要保留。对异步队列和 workflow，不要硬造父子嵌套，能用 span links 就用 links。像 LogServe 这种系统，状态顺序应该依赖 log offset、command_seq、epoch，而不是依赖墙上时间。
```

## Q033. 日志、metrics、trace 的数据保留策略如何设计？

保留策略要从用途出发，而不是统一说“都保留 30 天”。日志、metrics、trace 的价值密度、查询方式、成本结构和合规风险都不一样。

一个实用的原则是：

```text
metrics 保留更久，用于趋势、SLO、容量规划。
logs 保留按事件价值分层，用于 debug、审计、安全和复盘。
traces 原始数据保留较短，慢请求和错误 trace 可以保留更久。
```

metrics 的单位成本通常最低，因为它们是聚合后的时间序列。它们适合支持 SLO 窗口、容量趋势、发布对比和长期回归分析。比如 30 天 SLO 至少要保留覆盖整个 SLO 窗口的数据，最好再留出复盘和对比空间。常见做法是：

```text
高分辨率 metrics：7-30 天，例如 10s/30s/60s scrape。
中分辨率 rollup：90-180 天，例如 5m 聚合。
长期趋势：1 年或更久，例如 1h/1d rollup。
```

本地 Prometheus 一般适合作短中期查询；长期保留通常会接 remote write、对象存储或专门的长期 TSDB。Prometheus storage 和 remote write 文档都说明，样本、WAL、队列、series churn 都会消耗资源，所以长期保留不只是调大磁盘这么简单。高基数 label 会把保留成本成倍放大。

日志的策略更复杂。应用日志有 debug、info、warn、error、安全审计、业务事件、访问日志等多种类型。它们不应该共用一个保留周期：

```text
debug 日志：短保留，通常按小时或少数几天，必要时动态打开。
普通应用日志：覆盖主要故障排查窗口，例如 7-30 天。
错误日志和关键业务事件：可以保留更久，例如 30-90 天。
安全审计日志：按合规要求保留，可能是 180 天、1 年或更久。
原始请求/响应体：默认不保留或强脱敏后短保留。
```

日志还要区分 hot、warm、cold。hot 层有索引，查询快，成本高；warm 层可以降低索引密度；cold/archive 层主要用于合规和离线复盘，查询慢也可以接受。Loki 这类日志系统的保留通常还涉及 compactor、对象存储生命周期、tenant/stream 级策略。一个常见坑是只删对象存储，不处理索引，或者只改保留配置却以为历史数据会自动按新策略重算。具体系统的行为要看官方文档，不能凭感觉。

trace 的原始数据量很大，特别是高 QPS 服务、fan-out 服务和详细 span attributes。一般不会全量长时间保留。常见做法是：

```text
全量或较高采样 trace：短保留，用于近实时排障。
错误 trace：保留更久。
慢 trace：通过 tail sampling 保留更久。
关键交易 trace：按业务重要性提高采样或保留。
聚合后的 span metrics：保留更久，用于趋势和 SLO。
```

OpenTelemetry sampling 和 tail sampling 的意义就在这里：不是所有 trace 都同等有价值。head-based sampling 成本低，但可能在请求刚开始时还不知道它会不会慢；tail-based sampling 可以等看到 duration、status、attributes 后再决定是否保留，更适合保留错误和慢请求。代价是 collector 需要缓存 trace，带来内存和延迟压力。

保留策略还要考虑隐私和安全。日志和 trace 经常比 metrics 更容易带上敏感信息，例如用户输入、token、手机号、邮箱、订单号、prompt、模型输出。策略上应该先做字段级治理：

```text
禁止采集 secret、token、password、完整身份证号。
对用户标识做 hash 或 tokenization。
请求/响应体默认不进日志，必要时抽样并脱敏。
trace attributes 只放排障需要的字段。
删除、合规、legal hold 要有明确流程。
```

如果一个字段不应该长期保存，不能只靠“查询时不展示”。它最好在采集端或 collector 端就被过滤或脱敏。

设计保留周期时，我会问这几类问题：

```text
SLO 计算窗口是多长？
事故发现到复盘通常需要多久？
发布回滚和版本对比需要看多长历史？
合规或安全审计要求是什么？
数据里是否包含敏感字段？
查询是在线排障为主，还是离线分析为主？
当前摄入量、索引量、series 数、span 数增长速度是多少？
```

对 LogServe 来说，可以这样落地：

```text
控制面 metrics：保留较久，用于 workflow/task/actor/LLM 的趋势分析。
shared log 事件：作为系统状态源，要按 snapshot、logical trim 和恢复需求设计，不能简单当普通日志删。
debug 日志：短保留，主要用于开发和故障定位。
trace：保留错误、慢 workflow、慢 LLM、重试多的请求。
大 result 或模型输出：通过 result reference/object store 管生命周期，不塞进日志和 trace。
```

面试里可以这样回答：

```text
我不会给 logs、metrics、traces 设同一个保留周期。metrics 成本低、适合聚合和 SLO，会保留最长，并用 rollup/downsample 支持长期趋势；logs 价值和风险差异大，debug 日志短保留，错误和关键业务事件保留更久，安全审计按合规要求保留；traces 原始数据量大，通常短保留，但错误、慢请求、关键交易可以通过 tail sampling 留更久，span metrics 再长期保存。所有保留策略都要考虑 SLO 窗口、事故复盘窗口、合规、隐私、成本和查询速度。像 LogServe 的 shared log 是状态源，不能当普通 debug log 随便删，要结合 snapshot、logical trim 和恢复语义设计。
```

## Q034. 可观测性本身会带来哪些性能和成本开销？

可观测性不是免费的。它通常值得做，但不能假装没有成本。开销主要来自七个地方。

第一是应用 hot path 的 CPU 开销。每次请求创建 span、记录 attribute、更新 histogram、拼 JSON 日志、序列化 OTLP、计算 label hash，都会占用 CPU。Prometheus 文档说 instrumentation 的收益通常远大于资源成本，但也特别提醒，对于单进程每秒调用超过 100k 次的内层循环，要小心指标更新次数、label 查找和取时间的成本。也就是说，普通业务入口可以放心埋点，极热路径要 benchmark。

第二是内存和队列开销。OpenTelemetry SDK、collector、日志 agent、Prometheus remote write 都会有 buffer、batch queue、retry queue。OpenTelemetry 性能规范强调客户端默认不应该阻塞应用，也不应该无限消耗内存。但现实里如果下游 telemetry backend 慢了，队列就会涨。队列如果无界，会把业务进程拖死；队列如果有界，就要决定丢数据、采样还是阻塞。

第三是 I/O 和网络开销。日志写 stdout 或文件，collector 读取后再发送；metrics scrape 或 remote write；trace 通过 OTLP exporter 发出去。高 QPS、详细 span、全量 trace、结构化日志都会产生持续网络流量。Prometheus remote write 文档也说明 remote write 会增加内存，并增加 CPU 和网络使用。多个遥测后端并行发送时，成本还会叠加。

第四是存储和索引成本。metrics 的成本主要受 series 数影响，series 数又由 metric 数和 label cardinality 决定。logs 的成本来自原始字节、压缩、索引和查询扫描。traces 的成本来自 span 数、attribute 数、事件数、采样率和保留周期。一个看起来无害的 label，例如 `user_id`、`workflow_id`、`request_id`，如果进入 metrics，可能直接制造海量时间序列。

第五是延迟和抖动。异步 exporter 通常不会明显增加请求延迟，但同步日志、同步 flush、异常路径打印大栈、请求结束时强制导出 trace，都可能拉高 tail latency。最糟的是在故障时打更多日志、产生更多错误 span、更多 retry，遥测流量和业务故障互相放大。这个现象在事故里很常见：系统已经慢了，错误日志又把磁盘、stdout 管道、日志 agent 或网络打满。

第六是锁竞争和分配开销。很多 metrics registry、logger、span processor 内部需要锁、原子操作或内存分配。平时影响不明显，但在高并发服务里，label lookup、动态创建 metric、每次请求分配 attribute map，都可能出现在 profile 里。优化办法通常不是“不要可观测性”，而是把 label handle 缓存起来，避免热路径动态拼 label，减少 per-request allocation。

第七是人和流程成本。指标太多会制造 dashboard 噪声，报警太多会让值班麻木，trace 字段不规范会让查询困难，日志没有脱敏会带来安全风险。这些不一定表现为 CPU 或账单，但同样是成本。

控制开销的常见手段包括：

```text
指标 label 使用 allowlist，拒绝高基数字段。
日志按级别、模块和采样率控制，错误日志保留必要上下文。
trace 对普通请求采样，对错误和慢请求用 tail sampling。
collector/exporter 使用 batch、压缩、重试和有界队列。
telemetry backend 慢时优先降级或丢弃遥测，不阻塞核心业务。
对 hot path 埋点做 microbenchmark 和 profile。
用 collector 自身指标监控 dropped spans、export failures、queue length。
对敏感字段在采集端脱敏，不把治理压力留到查询端。
```

这里有一个面试中很加分的边界：可观测性系统应该 fail open。也就是说，遥测后端挂了，业务服务不能因为上报 trace 阻塞而一起挂。可以丢 trace，可以降采样，可以写本地 fallback，可以暴露 dropped telemetry 指标，但不能让日志系统、metrics exporter、trace collector 成为主链路的单点故障。

在 LogServe 中，可以把这个问题说得更具体。比如：

```text
每个 task 状态变化都写日志和 metric，会增加控制面 CPU 和存储压力。
LLM 请求如果记录完整 prompt/output，会带来隐私和存储成本，应该只记录摘要、长度、模型版本和 result ref。
workflow 每个 step 都建 span，可以帮助定位慢步骤，但高 fan-out workflow 会放大 span 数。
shared log append/fsync 本身已经是关键路径，不能为了观测再在锁内做同步日志 flush。
dashboard snapshot 应该读取 materialized view，而不是每次实时 replay 全量日志。
```

面试里可以这样回答：

```text
可观测性会带来 CPU、内存、网络、存储、索引、tail latency 和运维噪声成本。metrics 的主要风险是高基数 label，logs 的风险是字节量、索引量和敏感数据，traces 的风险是 span 数、attribute 数、采样率和 collector 队列。控制手段是低基数 label、日志级别和采样、trace head/tail sampling、batch exporter、有界队列、collector 自监控、字段脱敏，以及对 hot path 做 benchmark。最重要的原则是 telemetry 不能阻塞核心业务：后端慢了可以降采样或丢遥测，但不能把业务请求拖死。
```

## Q035. 如何评估新指标是否值得加入？

评估一个新指标，先不要问“能不能加”，而要问“谁会用它做什么决定”。一个指标如果不能支持报警、排障、容量规划、SLO、发布验证或产品判断，大概率只是 dashboard 装饰。

我通常用一张准入清单：

```text
1. 它回答什么具体问题？
2. 谁是 owner？
3. 它会出现在报警、dashboard、runbook、SLO 还是容量规划里？
4. 它是症状指标还是原因指标？
5. 数据源是否权威？
6. metric type、单位、语义是否清楚？
7. label 基数是否可控？
8. 采集频率、保留周期和成本是否可接受？
9. 异常时有没有明确动作？
10. 试运行后如果没人用，是否会删除？
```

第一个问题最关键。比如有人想加 `logserve_worker_last_seen_timestamp_seconds`，它能回答 worker 是否还活着，能用于控制面判断 worker 心跳和告警，这就有明确价值。有人想加 `logserve_request_user_id_total`，这看似能做分析，但会产生高基数时间序列，还可能泄露用户信息，应该拒绝进入 metrics，改放日志或离线分析系统。

第二步是判断它属于哪一层。症状指标更适合报警，原因指标更适合排障。比如：

```text
症状指标：workflow_success_rate、api_request_duration p99、checkout_error_rate。
原因指标：worker_queue_depth、db_connection_wait、fsync_duration、cache_evictions_total。
```

如果一个新指标只是原因指标，通常不要直接报警，除非它已经被证明能稳定预测用户影响。否则容易制造噪声。Google SRE 的 SLO 思路也是先从用户可感知结果定义目标，再把原因指标作为诊断线索。

第三步是检查 metric type 和单位。Prometheus 命名规范建议 metric 表达单一单位和单一数量，尽量使用 base units。常见错误包括：

```text
用 gauge 表示累计请求数。
用 counter 表示当前队列长度。
同一个 metric 里混合 milliseconds 和 seconds。
把成功数、失败数、总数拆成多个难以聚合的名字，而不是用 status label。
histogram bucket 乱设，导致 p99 无法解释。
```

如果指标要表达延迟分布，通常要用 histogram，而不是只打平均值。bucket 要围绕 SLO 和排障阈值设计。例如接口 SLO 是 500ms，bucket 至少要覆盖 100ms、250ms、500ms、1s、2s 这类边界。只打 `avg_latency_ms`，面试里很容易被追问“p99 怎么看”。

第四步是估算 cardinality。粗略公式是：

```text
series = metric_name_count * label_value_1 * label_value_2 * ... * instances
```

比如一个 histogram 有 10 个 bucket，加上 `le`、`sum`、`count`，再按 `route=200`、`status=5`、`region=4`、`version=20`、`instance=100` 拆，series 数会很快爆炸。这里还没算 tenant、method、cache_hit、error_class。评估新指标时，必须写出 label allowlist，并说明哪些字段禁止进入 label。

第五步是确认它有动作。一个好指标异常后，runbook 应该能说清楚下一步：

```text
worker_queue_depth 持续升高 -> 看 worker in-flight、executor pool、下游 task latency、是否扩容。
llm_cache_hit_ratio 下降 -> 看模型分布、worker cache 容量、eviction、checkpoint fetch。
actor_replay_commands 增加 -> 看 snapshot 是否生成、logical trim 是否推进、actor stream 是否异常。
remote_write_samples_pending 增加 -> 看 remote backend、网络、queue_config 和 Prometheus 资源。
```

如果异常后没人知道该做什么，它可能还不是一个成熟的生产指标。

第六步是先试运行。新指标可以先进入 dashboard 或 recording rule，观察一两个发布周期：

```text
正常范围是否稳定？
和事故或性能回归是否有关？
是否被值班或开发实际查询？
label 是否产生意外高基数？
采集成本是否可接受？
命名和单位是否需要调整？
```

试运行后没人使用，就应该删除或降级到日志/trace。指标不是越多越好。过期指标会误导排障，因为没人维护语义，dashboard 上却还显示一条看似权威的曲线。

在 LogServe 里，值得加入的指标通常有明确运行语义：

```text
workflow_duration_seconds：支持端到端 SLO 和 p99 分析。
task_redelivery_total：支持故障恢复和重复投递分析。
actor_replay_commands：支持 snapshot 效果验证。
llm_checkpoint_fetch_duration_seconds：支持 cold start 定位。
log_append_duration_seconds：支持 shared log 写路径分析。
```

不太适合加入 metrics 的字段包括：

```text
workflow_id
task_id
actor_id
request_id
完整 prompt
完整 result payload
任意用户输入
```

这些可以进入日志、trace、对象存储引用或离线分析，但不应该成为 metric label。

面试里可以这样回答：

```text
我会用“问题、动作、成本”来评估新指标。它必须回答一个具体问题，并服务报警、dashboard、SLO、runbook、容量规划或发布验证；异常时要有明确处理动作；类型、单位、命名和 label 语义要清楚；label cardinality 要先估算，禁止 user_id、request_id、workflow_id 这类高基数字段进 metrics。新指标最好先试运行一个发布周期，看它是否真的被使用、是否能解释事故、成本是否可控。没人使用或语义不清的指标要删除，避免 dashboard 变成旧指标博物馆。
```

## Q036. structured logging 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

structured logging 的核心目标是让日志事件从“给人看的句子”变成“机器和人都能可靠解释的事件记录”。它最先解决的不是业务正确性，也不是性能优化，而是可观测性里的可维护性和可诊断性问题：字段稳定、语义稳定、查询稳定，出了问题以后能按字段过滤、聚合、关联和复盘。

一个 structured log record 至少应该让人和系统回答这些问题：

```text
什么时候发生？
在哪个 service、instance、version、environment 发生？
是什么事件？
严重级别是什么？
和哪个 request/trace/span/workflow/task 相关？
结果是什么？成功、失败、超时、取消还是重试？
如果失败，错误类别和可行动原因是什么？
这条日志能不能被安全地索引、保留、分享？
```

OpenTelemetry Logs Data Model 里把 log record 拆成 `Timestamp`、`ObservedTimestamp`、`TraceId`、`SpanId`、`SeverityText`、`SeverityNumber`、`Body`、`Resource`、`InstrumentationScope`、`Attributes`、`EventName` 等字段。这个模型背后的思想很明确：日志不是一段随手写的文本，而是一个有语义的事件。字段可以映射、传输、存储和解释，最好不要靠下游用正则猜。

如果要在“正确性、性能、安全性、可维护性”里选一个主目标，我会选可维护性，具体说是 operational maintainability。structured logging 让日志查询、告警、审计、跨服务关联和故障复盘更稳定。它对其他三个方面也有影响，但不是同一种关系。

对正确性的影响是间接的。structured logging 不会让业务逻辑更正确，也不能替代状态机、事务、幂等、fencing、测试。它能帮助发现正确性问题，例如同一个 workflow step 被重复完成、同一个 actor command 的 `command_seq` 不连续、同一个 idempotency key 出现 payload conflict。发现问题和防止问题不是一回事。面试里要说清楚这个边界。

对性能的影响通常是负担，而不是收益。字段化以后可以减少下游解析成本，查询也更稳定，但应用热路径要多做字段构造、JSON 编码、时间戳获取、缓冲、网络发送。zap 的官方文档也把性能瓶颈说得很直接：反射式序列化、字符串格式化和大量小对象分配在 hot path 里很贵。structured logging 的设计要控制这些成本，不能假设“结构化”天然更快。

对安全性的影响有两面。字段化以后可以做字段级脱敏，例如统一删除 `authorization`、`password`、`token`，这比在纯文本里找敏感信息可靠。但 structured logging 也让泄露变得更“干净”：如果有人把 `access_token`、`user_email`、`request_body` 当字段打出来，日志系统会很认真地把它索引、复制、保留、导出。OWASP Logging Cheat Sheet 的思路是按用途记录，同时排除敏感数据。structured logging 让治理更容易，但不自动安全。

所以我会这样划分：

```text
主目标：可维护性、可诊断性、可关联性。
间接帮助：正确性问题定位、安全字段治理、自动化分析。
潜在代价：CPU、内存、锁竞争、I/O、网络、存储、隐私风险。
不能替代：业务状态机、事务一致性、安全审计策略、性能测试。
```

在 LogServe 里，structured logging 最有价值的地方是把控制面事件和运行时上下文说清楚。比如 `TaskSubmitted`、`TaskStarted`、`TaskCompleted`、`WorkflowStepFailed`、`ActorCommandApplied`、`LLMCompleted` 这些事件，如果只是纯文本，后续很难稳定地问“哪个 workflow type 的 p99 高”“哪个 worker 的 redelivery 多”“哪类 LLM 请求 cache miss”。如果它们带上稳定字段，日志就能和 trace、metrics、dashboard materialized view 接上。

但这不等于 LogServe 的 shared log 本身就是普通 structured logging。shared log 是状态源，参与恢复和 replay；应用结构化日志是观测信号，主要服务排障和复盘。一个能重建系统状态，一个帮助理解系统行为。两者可以共用字段设计思路，但语义强度不同。

面试里可以这样回答：

```text
structured logging 的核心目标是把日志变成稳定、机器可读、可关联的事件记录。它主要解决可维护性和可诊断性问题：字段稳定、查询稳定、跨日志/trace/metrics 关联稳定。它能间接帮助发现正确性问题，也能让脱敏和安全治理更容易，但它不会替代业务正确性机制；性能上它反而可能增加开销，需要控制序列化、分配、锁和 I/O。我的理解是：structured logging 的主价值是让线上系统在出问题时可解释，而不是让系统天然更快、更安全或更正确。
```

## Q037. structured logging 的典型适用场景和不适用场景分别是什么？

structured logging 适合那些“以后一定要被机器查询、聚合、关联”的日志。它不适合所有输出，也不等于把每个对象都转成 JSON。

典型适用场景有几类。

第一类是请求入口和跨服务调用。HTTP、gRPC、消息消费、workflow step、actor command、LLM request 这些边界事件都应该结构化。因为它们是排障入口，通常需要按 `service`、`route`、`method`、`status`、`duration`、`trace_id`、`span_id`、`request_id`、`tenant_tier`、`version` 查询。

```json
{
  "level": "INFO",
  "event": "workflow_step_completed",
  "workflow_id": "wf-123",
  "step_id": "summarize",
  "worker_id": "worker-1",
  "duration_ms": 82,
  "attempt": 1,
  "trace_id": "..."
}
```

第二类是状态转移。只要系统有状态机，日志就应该把状态变化写成字段，而不是写成一句模糊的“done”。例如：

```text
old_state=SCHEDULED new_state=STARTED
old_owner=worker-1 new_owner=worker-2 epoch=7
command_seq=42 result=applied
```

这类日志对排查重试、恢复、竞态、旧 owner 写入尤其有用。LogServe 的 workflow、actor、LLM 调度都属于这一类。

第三类是错误和降级。错误日志需要稳定字段：`error_class`、`error_code`、`retryable`、`timeout_ms`、`attempt`、`dependency`、`upstream_status`。只写 `failed` 没用。线上真正需要的是“失败是暂时的吗”“是否应该重试”“影响哪个依赖”“是否和某个版本有关”。

第四类是审计和安全事件。登录失败、权限拒绝、配置变更、密钥轮换、管理员操作、数据导出，都需要结构化字段。审计日志尤其要求字段语义稳定、时间准确、主体和动作清楚。这里也要更严格地脱敏，不能因为要审计就记录敏感原文。

第五类是可关联的业务事件。比如订单创建、支付失败、任务完成、模型缓存命中。这类事件不一定进入 metrics，因为维度可能太细；但放在结构化日志里，可以支持问题定位和离线分析。

不适用场景也要讲清楚。

第一，临时本地调试不一定需要 structured logging。开发者在本地跑一个小脚本，打印中间变量，看完就删，用 `fmt.Println` 或普通 log 足够。强行包装成结构化日志，只会降低开发速度。

第二，极热的内层循环要谨慎。比如每条数据、每个 token、每次 CAS、每次锁自旋都打结构化日志，会直接把 CPU、分配、I/O 打满。这里应该优先用 metrics、采样、计数器、profile，必要时只在异常路径或抽样路径记录。

第三，大对象和敏感对象不适合直接结构化记录。完整 request body、response body、prompt、模型输出、文件内容、SQL 参数、用户输入，进入日志后会带来隐私、成本和查询噪声。更好的方式是记录长度、hash、摘要、对象引用、错误类别。

第四，严格状态恢复不能只靠普通 structured logs。如果日志是系统状态源，就需要 WAL、append-only log、fsync、checksum、sequence、replay、compaction 这些机制。普通应用 structured logging 一般是 best effort，不保证 exactly-once，不保证 crash 前一定落盘。LogServe 的 shared log 和应用日志在这里必须分开。

第五，高频指标不应该用日志模拟。有人会打 `request_completed` 日志，然后从日志里聚合 QPS、p99、错误率。低流量时可以，生产高流量下成本很高，也容易受采样、丢日志、索引延迟影响。核心 SLI 应该直接做 metrics，日志用于解释样本。

可以用一个简单判断：

```text
需要按字段查询、聚合、关联、审计 -> structured logging。
需要连续趋势和低成本告警 -> metrics。
需要跨服务因果链和耗时分解 -> tracing。
只是本地临时看一眼 -> 普通输出。
需要恢复状态 -> WAL/shared log/event store。
```

面试里可以这样回答：

```text
structured logging 适合请求边界、RPC/消息消费、状态转移、错误降级、审计安全事件，以及需要和 trace/metrics 关联的业务事件。它不适合极热内层循环、临时本地调试、完整请求响应体、大对象、敏感数据，也不适合替代 metrics 或 WAL。我的判断标准是：如果以后要按字段查询、聚合、关联或审计，就结构化；如果只是低成本趋势，用 metrics；如果要恢复状态，用真正的持久化日志；如果只是本地看变量，没必要上结构化日志。
```

## Q038. structured logging 和相近概念最容易混淆的边界在哪里？

structured logging 最容易和五类东西混在一起：JSON 日志、metrics、tracing、audit log、event sourcing/WAL。

第一，JSON 日志不等于 structured logging。JSON 只是编码格式。真正的结构化日志要求字段语义稳定、类型稳定、命名稳定。下面这种虽然是 JSON，但价值很低：

```json
{
  "message": "payment failed for user 123 order 456 timeout after 2s"
}
```

它本质还是纯文本，只是包了一层 JSON。更好的写法是：

```json
{
  "event": "payment_failed",
  "user_hash": "...",
  "order_id": "456",
  "error_code": "UPSTREAM_TIMEOUT",
  "duration_ms": 2000,
  "retryable": true
}
```

反过来也一样，structured logging 不一定非要 JSON。它可以是 logfmt、protobuf、OTLP LogRecord、二进制编码。关键是字段和语义，不是花括号。

第二，structured logging 不等于 metrics。日志描述一次事件，metrics 描述聚合后的时间序列。日志适合回答“这次请求发生了什么”，metrics 适合回答“这类请求总体怎么样”。如果把每次请求都打成日志再实时算 p99，成本会很高；如果只看 metrics，又看不到具体失败上下文。两者要互补。

第三，structured logging 不等于 tracing。trace 关注一次请求经过哪些 span、父子关系是什么、每段耗时多少。structured log 关注某个事件的上下文。日志可以带 `trace_id`、`span_id`，从而挂到 trace 上，但日志本身不表达完整调用树。OpenTelemetry 也把 logs、metrics、traces 作为不同 signal 对待。

第四，structured logging 不等于 audit log。audit log 是一种特殊日志，关注“谁在什么时候对什么资源做了什么动作，结果如何”。它强调完整性、不可抵赖、保留周期、访问控制和合规。普通 structured logs 可以被采样、降级、短保留；审计日志通常不能随便采样，也不能随便丢。

第五，structured logging 不等于 event sourcing 或 WAL。event sourcing 的事件是业务状态源，WAL 是恢复协议的一部分。它们对顺序、持久性、重放、幂等、版本兼容有严格要求。structured logging 主要是观测信号，很多系统会异步写、批量发、失败时丢弃。把 structured log 当状态源，通常会在 crash、重试、采样、日志丢失时出事。

还容易混淆的是“message”和“event name”。`message` 是给人看的说明，可以变化；`event_name` 或 `event_type` 是机器识别事件类别的稳定字段，不应该随文案改动。比如：

```text
event_name=actor_command_rejected
message="stale actor owner tried to apply command"
reason=stale_epoch
owner_epoch=8
request_epoch=7
```

这里 `message` 可以润色，`event_name` 和 `reason` 不应随便改。否则 dashboard、告警、查询和 runbook 都会失效。

在 LogServe 里，边界可以这样解释：

```text
shared log：系统状态源，用于 replay 和恢复。
structured logs：观测事件，用于排障和复盘。
metrics：聚合趋势，用于 SLO、告警和容量。
trace：请求因果链，用于定位耗时和跨服务路径。
audit log：安全/合规事件，用于追责和审计。
```

这些系统可以共享 ID，比如 `workflow_id`、`task_id`、`trace_id`、`worker_id`，但不能混成一种东西。

面试里可以这样回答：

```text
最容易混淆的边界是：JSON 不等于 structured logging，metrics 不等于日志，trace 不等于日志，audit log 和 WAL/event sourcing 也不是普通结构化日志。structured logging 的关键是稳定字段和语义；metrics 是聚合时间序列；trace 是调用链；audit log 有合规和不可抵赖要求；WAL 或 event store 是状态恢复机制。LogServe 里 shared log 是状态源，structured logs 是观测信号，这个边界必须分清。
```

## Q039. structured logging 在高并发场景下可能出现哪些隐藏问题？

高并发下，structured logging 最大的问题不是“打不出来”，而是它会悄悄进入热路径，把延迟、CPU、内存、锁竞争和 I/O 都放大。平时看不出来，QPS 一上去就变成 p99 问题。

第一个隐藏问题是锁竞争。很多 logger 会共享 encoder、buffer pool、writer、level config 或 sink。多 goroutine 同时写日志时，如果每条日志都抢同一把锁，就会把日志库变成串行瓶颈。表现可能是业务 CPU 不高，但 goroutine 堆在 logger 或 stdout 写入上；或者 mutex profile 里 logger 相关锁很明显。

第二个问题是分配和 GC。结构化日志通常要构造字段、切片、map、interface、字符串。使用反射式 JSON 编码、`fmt.Sprintf`、`map[string]any`、动态字段名，会制造大量小对象。zap 官方文档强调反射式序列化和字符串格式化在 hot path 中代价很高，这也是它提供 strongly typed fields 的原因。Go 的 `slog` 也提供 `LogAttrs`，目的之一就是减少频繁日志路径上的分配。

第三个问题是 disabled log 也可能有成本。很多人以为 debug level 关了就没事，但如果代码先构造字段、拼字符串、序列化对象，再调用 logger，开关已经晚了。

```go
logger.Debug("payload", "body", expensiveJSON(req)) // 即使 Debug 关闭，expensiveJSON 也可能已经执行
```

更好的方式是让 level check 尽早发生，或者使用延迟求值、typed fields、惰性 attribute。

第四个问题是输出 sink 阻塞。stdout、文件、socket、日志 agent、collector 都可能慢。容器里写 stdout 看起来像内存操作，实际可能经过 pipe、runtime、log driver、agent、网络。下游慢时，日志调用可能阻塞业务 goroutine；异步队列满时，要么丢日志，要么阻塞，要么占用更多内存。这个选择必须明确，不能靠默认行为碰运气。

第五个问题是日志行原子性和交错。结构化日志通常要求一条记录是完整的一行 JSON。如果多个线程写同一个 writer，但没有保证单条记录原子写入，就可能出现两条 JSON 拼在一起、半条 JSON 丢失、换行错乱。下游解析器会把这些当成坏记录丢掉，事故时最需要的日志反而不见了。

第六个问题是字段对象被并发修改。有些实现为了少拷贝，把 map、slice、指针对象直接放进日志字段，异步 encoder 稍后才序列化。调用方如果随后修改对象，就可能出现数据竞争，或者日志内容和调用时状态不一致。结构化日志应该在调用边界复制必要值，或者只接受不可变/基础类型字段。

第七个问题是高基数字段爆炸。高并发下 `user_id`、`request_id`、`workflow_id`、完整 URL、SQL、错误 message 如果都被索引，日志平台会变慢，账单会上升，查询会超时。日志比 metrics 更能承受高基数，但不是无限承受。字段可以保留和索引是两回事，很多平台需要区分 indexed fields 和 stored fields。

第八个问题是采样和限流造成证据偏差。错误高峰时如果日志采样太激进，可能只留下少量重复错误，看不到真正根因；如果完全不采样，可能又把系统拖垮。比较稳妥的策略是普通成功请求采样，错误和慢请求保留更多，同时暴露 dropped log count、sampling ratio、queue length。

在 LogServe 里，这些问题会出现在控制面和 worker 两侧。例如每个 task 状态变化都打日志，如果日志写入阻塞了调度路径，workflow p99 会被观测系统拉高；如果每个 LLM token 都打结构化日志，mock 环境看不明显，真实 serving 下会迅速变成 I/O 和存储问题。actor mailbox 的日志还要保证 `actor_id`、`command_seq`、`epoch` 是调用时的值，不能异步读一个已经变化的 actor 对象。

面试里可以这样回答：

```text
高并发下 structured logging 的隐藏问题包括 logger 内部锁竞争、字段构造和 JSON 编码分配、disabled debug 日志仍然计算字段、stdout/file/network sink 阻塞、异步队列积压、日志行交错、异步序列化读到被修改的对象、高基数字段拖垮索引，以及采样导致证据偏差。处理上要尽早做 level check，使用 typed fields 和 buffer pool，避免热路径动态 map 和反射，保证单条记录原子写入，队列有界并暴露 dropped count，错误和慢请求优先保留。日志系统不能把业务 p99 拖上去。
```

## Q040. structured logging 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

structured logging 在正常路径里很好看，真正的边界经常出现在崩溃、重启、超时和重试里。原因很简单：日志不是业务事务的一部分，很多实现又是异步和批量写的。

崩溃时，第一类问题是 buffered logs 丢失。高性能 logger 通常会缓冲、批量编码、异步发送。进程 panic、`os.Exit`、容器被 SIGKILL、机器断电时，缓冲区里的日志可能来不及 flush。zap 示例里常见 `defer logger.Sync()`，就是为了在正常退出时刷出缓冲。但 `defer` 对 SIGKILL 没用，对某些 fatal exit 路径也不一定执行。

第二类问题是部分写。进程在写一条 JSON 日志时崩溃，文件或 stdout 里可能只留下半条记录。下游 parser 可能丢弃这一行。崩溃附近的日志尤其重要，所以日志系统最好做到单条记录尽量原子写入，并让采集端能统计 parse error。

第三类问题是 fatal/panic 语义。调用 `logger.Fatal` 是否会先 flush？是否会执行 defer？不同库语义不同。Go 里 `os.Exit` 不会运行 deferred functions。面试时不要笼统说“Fatal 会保证写入”，要看库实现和部署环境。更稳妥的做法是在真正退出前显式 flush，并设置超时，避免 flush 卡死。

重启时，问题主要是上下文断裂。进程内的 request context、attempt counter、buffer、logger field cache 都没了。如果服务重启后继续处理同一个队列消息或 workflow step，日志里必须带稳定的业务 ID 和 attempt 信息，否则看起来像两次无关请求。

```text
workflow_id
step_id
attempt
idempotency_key
worker_id
process_start_time
service_instance_id
```

这些字段能帮助区分“同一任务重试”“新任务”“旧 worker 恢复后继续处理”。LogServe 的 worker kill recovery、queue redelivery、control restart probe 就属于这种场景。日志如果没有 `attempt`、`worker_id`、`epoch`，很难判断一次完成事件是合法重试还是旧 worker 的迟到写入。

超时时，边界在“谁认为它超时”。客户端超时不代表服务端停止执行；服务端完成不代表客户端还在等；context cancelled 不代表所有 goroutine 立刻退出。structured logs 应该把 timeout 的观察者写清楚：

```text
event=rpc_timeout
side=client
deadline_ms=500
elapsed_ms=501
dependency=payment
attempt=2
```

如果服务端后来又写 `request_completed`，这不是矛盾，而是 client 和 server 看到的事实不同。trace 和日志要能表达这种差异。

重试时，最常见的问题是重复日志。一次用户操作可能产生多条 `payment_failed`、多条 `task_started`、多条 `llm_request_timeout`。如果日志没有 `attempt`、`retry_reason`、`previous_error`、`idempotency_key`，值班会误以为发生了多个独立请求。指标也可能被重复日志误导：从日志里数失败次数时，要区分 attempt-level failure 和 operation-level failure。

还有一个边界是日志顺序。重试、异步 flush、collector 重排、不同节点 clock skew 都会让日志到达顺序不同于发生顺序。structured logging 可以带 timestamp、observed_timestamp、sequence、log offset、attempt，但不能保证分布式全局顺序。OpenTelemetry Logs Data Model 区分 `Timestamp` 和 `ObservedTimestamp`，这在采集延迟和重启恢复时很有用。

对崩溃和重启场景，我会要求日志至少满足这些设计点：

```text
退出前 best-effort flush，但设置超时。
异步队列满或 flush 失败时暴露 dropped count。
每条日志带 service instance / process start 标识。
重试日志带 attempt 和 idempotency key。
超时日志标明 client/server/worker 哪一侧观察到超时。
状态转移日志带旧状态、新状态、epoch 或 sequence。
日志不作为唯一恢复依据，恢复靠 WAL/shared log/数据库。
```

面试里可以这样回答：

```text
崩溃时 structured logging 可能丢 buffered logs、写出半条 JSON、来不及 flush；重启后进程内上下文丢失，需要靠 workflow_id、request_id、attempt、worker_id、process_start_time 关联；超时时要说明是 client、server 还是 worker 观察到 timeout；重试会产生重复日志，所以必须区分 attempt-level 和 operation-level。日志顺序也不能当成分布式全局顺序。像 LogServe 这种系统，恢复语义应该依赖 shared log、sequence、epoch、idempotency key，structured logs 只是帮助解释发生了什么。
```

## Q041. structured logging 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

答案不是单选。structured logging 的瓶颈会随着实现和部署方式移动。低 QPS 时可能什么都看不出来；高 QPS、错误风暴、collector 故障、日志索引慢时，CPU、内存、锁、I/O、网络都可能成为瓶颈。面试里更好的回答是按日志链路拆开看：

```text
调用点 -> level check -> 字段构造 -> 编码/序列化 -> buffer/queue -> sink 写入 -> agent/collector -> 网络 -> 后端索引和存储
```

CPU 瓶颈主要来自字段构造和编码。典型来源有：`fmt.Sprintf`、反射式 JSON 编码、把复杂对象转成 `map[string]any`、异常栈格式化、时间格式化、字段排序、字符串 escape。zap 官方文档提到，反射式序列化和字符串格式化在大量日志场景下很贵，会带来 CPU 和小对象分配。Go `slog` 的文档和博客也强调，对于性能敏感路径，可以用 `LogAttrs`、提前构造 `Attr`、自定义 `LogValuer` 等方式减少开销。

内存瓶颈通常来自分配和缓冲。每条日志如果都分配字段切片、map、临时字符串、JSON buffer，高并发下 GC 压力会很明显。异步 logger 为了避免阻塞业务，会引入队列；collector 或 remote write 也会有队列。下游慢时，队列要么增长、要么丢日志、要么阻塞。OpenTelemetry 的性能规范强调客户端默认不应该阻塞应用，也不应该无限消耗内存，本质就是在说这个 trade-off。

锁竞争来自共享资源。常见位置包括全局 logger、level 配置、encoder、buffer pool、writer、日志轮转、stdout 写入。很多时候日志本身不慢，是大家都在抢同一个 sink。表现是 p99 升高、goroutine 堆积在 `Write`、mutex profile 出现 logger 内部锁。优化方向是减少共享锁、每条记录先在 goroutine 本地编码完整，再用短临界区写入；或者异步队列化，但要接受丢弃/积压策略。

I/O 瓶颈来自 stdout、文件、fsync、日志轮转、容器 log driver。写文件如果每条都 fsync，延迟会非常高；不 fsync 则崩溃时可能丢日志。写 stdout 也不是免费，容器运行时和日志 agent 可能成为瓶颈。日志轮转时还可能遇到 rename、压缩、权限、磁盘满、inode 耗尽。

网络瓶颈通常出现在集中日志链路：应用发 OTLP、agent 发 collector、collector 发后端、Prometheus remote write 或日志平台 ingest API。网络抖动时，异步队列积压；后端限流时，重试又会放大流量。这个时候业务服务如果同步等待日志上报，问题会被直接传到用户请求。

后端索引和存储也不能忽略。即使应用侧很快，日志平台也可能因为字段太多、索引太宽、高基数字段、保留周期过长而变慢。structured logging 的成本常常不是写出那一刻，而是在查询、索引、压缩、复制、保留上慢慢体现。

如何判断瓶颈在哪？我会分层看：

```text
应用 profile：CPU 是否在 JSON encode、fmt、logger、time formatting、stacktrace？
Go benchmark：ns/op、B/op、allocs/op 是否异常？
mutex/block profile：是否卡在 logger lock 或 writer Write？
runtime metrics：GC、heap、goroutine、queue length 是否上涨？
I/O 指标：stdout/file 写入、磁盘 await、fsync、log rotation 是否异常？
网络指标：export latency、retry、dropped、collector queue、backend 429/5xx。
后端指标：ingest rate、index latency、query latency、storage growth。
```

优化顺序通常是：先避免没必要的日志，再降低每条日志成本。不要一上来就换日志库。先检查 debug 是否关掉、字段是否过多、是否在热循环打日志、是否打印大对象、是否重复记录同一个错误。确认确实是日志库成本后，再考虑 typed fields、对象池、异步批量、采样、降级、低成本编码。

在 LogServe 里，最需要警惕的是日志写入和 shared log 写入混在关键路径里。shared log append/fsync 是系统机制的一部分，应用 structured logging 是观测信号。如果观测日志在同一个锁或同一个磁盘路径上阻塞，就可能把 workflow p99、task throughput、LLM 调度都拖慢。设计上应该让观测日志 fail open，并用 metrics 监控 dropped logs、export latency、queue depth。

面试里可以这样回答：

```text
structured logging 的瓶颈不是固定来自 CPU、内存、锁、I/O 或网络中的某一个，而是要按链路拆。CPU 常见在字段构造、fmt、反射 JSON、时间格式化、stacktrace；内存来自字段对象、buffer、异步队列和 GC；锁竞争来自共享 logger、encoder、writer；I/O 来自 stdout、文件、fsync、log rotation；网络来自 collector 和后端 ingest。判断时看 profile、allocs/op、mutex profile、队列长度、dropped count、export latency、磁盘和后端指标。原则是日志不能阻塞主链路，热路径要少分配、少锁、少反射，必要时采样和降级。
```

## Q042. structured logging 的 correctness test、stress test 和 benchmark 应该分别测什么？

这三类测试目标不同。correctness test 证明“日志语义对”；stress test 证明“压力和故障下不会把系统拖垮或写坏”；benchmark 证明“单条日志和典型路径成本可接受”。不要把它们混成一个 `go test`。

correctness test 先测 schema 和字段语义。最基本的是：

```text
输出是合法 JSON / logfmt / OTLP record。
每条日志有 timestamp、level、message/event、service/resource。
level 过滤正确：debug 关闭时不会输出 debug。
字段类型稳定：duration 是数字，不是有时字符串有时数字。
时间单位稳定：seconds、milliseconds 不混用。
错误字段稳定：error_code、error_class、retryable 可查询。
trace_id/span_id/request_id 能按上下文注入。
字段名不动态变化，枚举值在允许集合内。
```

还要测安全和脱敏。给 logger 输入带密码、token、Authorization header、Cookie、银行卡、邮箱、手机号的对象，断言输出里没有原文。这个测试很实际，因为很多泄露来自“临时把整个 request 打出来”。如果有 `LogValuer` 或自定义 marshaler，也要测它不会绕过脱敏。

correctness test 还应该覆盖并发安全。多个 goroutine 同时写日志，输出应是一条条完整记录，不能交错成坏 JSON；异步 logger 不能读到调用后被修改的 map/slice；`With` 出来的子 logger 不应该污染父 logger；重复 key 的处理策略要明确，是后者覆盖、保留两者，还是拒绝。

stress test 测的是极端场景：

```text
100/1000/10000 goroutine 并发写。
日志后端变慢、阻塞、返回错误。
异步队列满。
磁盘满、权限错误、文件轮转。
collector 断开后恢复。
错误风暴：每个请求都报错并带 stacktrace。
超大字段、超长 message、非法 UTF-8、换行、控制字符。
进程 shutdown 时 flush 超时。
```

stress test 的预期不是“所有日志一条不丢”。很多生产 logger 在压力下会丢弃或采样。预期应该写清楚：业务 goroutine 是否允许阻塞？最多阻塞多久？队列满时丢新日志还是丢旧日志？dropped count 是否递增？恢复后是否继续工作？坏 sink 是否会导致 panic？

benchmark 测的是成本。至少要测几种路径：

```text
disabled debug log：不开启时每次调用成本。
info log 无字段：静态 message 成本。
info log 5-10 个 typed fields：常见请求日志成本。
error log + stacktrace：异常路径成本。
With 固定上下文后反复打日志：每请求 logger / 每组件 logger 成本。
异步队列 enqueue 成本。
真实 sink 写入成本：stdout、文件、discard、网络 exporter 分开测。
```

指标上要看：

```text
ns/op
B/op
allocs/op
records/s
p50/p95/p99 log call latency
队列长度和 dropped count
CPU profile 中 logger 占比
```

如果是 Go 项目，benchmark 至少要用 `go test -bench -benchmem`；如果怀疑并发瓶颈，再加 `-cpu 1,4,16` 或单独写并发 benchmark。日志库 benchmark 要非常小心：写到 `io.Discard` 只测编码，不测真实 I/O；写 stdout 会受环境影响；网络 exporter benchmark 要把后端延迟单独标出来。

对 LogServe，我会这样设计测试：

```text
correctness：TaskSubmitted/Started/Completed、ActorCommandApplied、LLMCompleted 字段完整，trace_id/workflow_id/task_id/attempt/worker_id 都在。
stress：worker kill recovery、queue redelivery、control restart 时日志不阻塞恢复路径，dropped count 可见。
benchmark：在 workflow/task/LLM hot path 打典型 structured log，测 ns/op、B/op、allocs/op，以及对 task p99 的影响。
```

面试里可以这样回答：

```text
correctness test 测日志语义：格式合法、schema 稳定、字段类型和单位正确、level 过滤正确、trace/request context 注入正确、敏感字段脱敏、并发写不交错。stress test 测压力和故障：高并发、错误风暴、后端阻塞、队列满、磁盘满、collector 断开、shutdown flush、超大字段，重点看是否阻塞业务、是否 panic、dropped count 是否可见。benchmark 测成本：disabled log、普通 info、带 10 个字段、error+stacktrace、With 上下文、异步 enqueue、真实 sink，报告 ns/op、B/op、allocs/op、records/s 和 p99 log latency。
```

## Q043. 如果要求从零实现一个简化版 structured logging，你会先定义哪些不变量？

从零实现时，我不会先写 JSON encoder，而是先定义不变量。日志库最怕“看起来能用”，但在并发、崩溃、采样、脱敏、字段冲突时语义不清。

我会先定义这些不变量。

第一，一次 `Log` 调用最多产生一条完整记录。记录要么完整写出，要么明确计入 dropped/error，不允许半条记录被当作成功。对于文本 sink，默认一条记录一行；对于二进制或 OTLP sink，要有明确 frame 边界。

第二，top-level schema 稳定。简化版可以固定这些字段：

```text
timestamp
level
event
message
service
instance_id
trace_id
span_id
request_id
attributes
```

`message` 给人读，`event` 给机器识别。`attributes` 放事件相关字段。不要今天叫 `err_code`，明天叫 `errorCode`，后天又塞进 `message`。

第三，字段类型稳定。`duration_ms` 永远是数字，`attempt` 永远是整数，`retryable` 永远是布尔，`trace_id` 永远是字符串。类型漂移会让查询和索引非常痛苦。

第四，level 过滤必须在昂贵字段计算前发生。简化 API 可以这样设计：

```go
if logger.Enabled(Debug) {
    logger.Debug("cache_probe", Attr("state", expensiveState()))
}
```

或者提供惰性字段。反正不能让关闭的 debug log 仍然序列化大对象。

第五，日志调用必须并发安全。多 goroutine 写同一个 logger，不能数据竞争，不能输出交错。`With` 返回的子 logger 应该是不可变或 copy-on-write，不能修改父 logger 的字段集合。

第六，调用边界要拷贝必要值。不能把可变 map/slice/pointer 丢给异步 encoder，稍后再读。否则调用方修改对象后，日志内容就不是调用时的事实，还可能触发 data race。简化实现可以只允许基础类型、字符串、数字、布尔、time、error，复杂对象必须显式转换。

第七，重复 key 策略要固定。比如同一条记录里出现两次 `worker_id`，到底后者覆盖前者，还是保留数组，还是报错？我倾向于开发期检测并报警，生产里按确定规则处理。不能让不同 encoder 表现不同。

第八，敏感字段默认治理。至少要有字段 denylist 或 redactor：`password`、`token`、`authorization`、`cookie`、`secret`、`api_key`。更好的方式是业务白名单，但简化版也不能完全不管。

第九，输出失败不应默认拖垮业务。同步 logger 可以返回 error 或写到 fallback；异步 logger 要有有界队列，并定义队列满时策略：drop、block with timeout、sample。无论哪种，都要暴露内部计数：`dropped_logs_total`、`encode_errors_total`、`write_errors_total`。

第十，flush 和 shutdown 语义明确。`Flush(ctx)` 尝试把缓冲日志写出，但受 context deadline 限制；flush 失败要返回 error。不能承诺 SIGKILL 或机器断电时一定不丢日志。

第十一，时间语义清楚。记录事件发生时间 `timestamp`，如果有 collector 再加 observed time。单条日志内 duration 用 monotonic clock 算好后以数值字段写入，不要让下游拿两个跨机器 timestamp 相减。

第十二，编码必须可被下游稳定解析。JSON 字符串要 escape，换行不能破坏一行一条记录，非法 UTF-8 要处理，error 对象要转成明确字段，例如 `error.message`、`error.type`、`error.stack`。

简化版 API 可以很小：

```go
type Logger interface {
    Enabled(level Level) bool
    With(attrs ...Attr) Logger
    Log(ctx context.Context, level Level, event string, msg string, attrs ...Attr)
    Flush(ctx context.Context) error
}
```

这里 `ctx` 用来取 trace/span/request 信息，不用来在日志库里做业务控制流。`With` 用于 service、component、worker_id 这类稳定上下文。每次 `Log` 的 attrs 用于事件上下文。

在 LogServe 里，如果我做简化版，会优先保证：`workflow_id`、`task_id`、`actor_id`、`command_seq`、`attempt`、`worker_id`、`epoch` 这些字段语义稳定；日志失败不能影响 shared log append；错误和 dropped count 进入 metrics；敏感的 LLM prompt/result 不默认记录，只记录 result ref、长度、hash 或错误类别。

面试里可以这样回答：

```text
从零实现 structured logging，我会先定义不变量：一次调用一条完整记录；schema、字段名、字段类型和单位稳定；message 给人读，event 给机器识别；level 过滤在昂贵字段计算前；logger 并发安全；With 不污染父 logger；异步编码不能读取调用后会变的对象；重复 key 策略固定；敏感字段有 redaction；队列有界，失败不默认阻塞业务；Flush 有超时；输出失败和 dropped logs 可观测。实现 JSON encoder 之前先把这些语义定下来，否则上线后会出现坏 JSON、字段漂移、日志丢失、敏感信息泄露和 p99 被日志拖慢。
```

## Q044. structured logging 的常见误用是什么，误用后通常会产生什么线上症状？

structured logging 的误用很常见，因为它看起来只是“多加几个字段”。问题是日志一旦进入集中系统，字段会被索引、聚合、告警、保留，错误设计会迅速变成线上成本。

第一种误用是把 JSON 当 structured logging。所有信息都塞进 `message`，字段只有 `level` 和 `msg`。症状是查询仍然靠模糊搜索和正则，dashboard 无法按 `error_code`、`operation`、`dependency` 聚合。出了事故，值班还是在日志平台里搜字符串。

第二种误用是字段名不统一。一个服务写 `user_id`，另一个写 `uid`，第三个写 `userId`；延迟有的叫 `latency`，有的叫 `duration_ms`，有的叫 `elapsed`。症状是跨服务查询很难写，告警规则越来越多，复盘时要先整理字段字典。

第三种误用是动态字段名。比如把用户 ID、HTTP header 名、SQL 表名、业务参数拼进 key：

```json
{"user_12345_latency_ms": 30}
```

这会让 schema 失控。字段名应该稳定，变化的内容放 value。

第四种误用是高基数字段滥用。`request_id`、`trace_id`、`workflow_id` 可以作为存储字段用于精确查询，但不一定应该被默认索引或用于高频聚合。完整 URL、SQL、exception message、用户输入更危险。症状是日志平台索引膨胀、查询变慢、账单上涨、写入被限流。

第五种误用是记录敏感数据。最常见的是 dump request、dump response、打印 JWT、Cookie、Authorization header、数据库连接串、LLM prompt 和输出。症状可能不是马上报错，而是安全审计失败、日志访问权限被迫收紧、复盘材料不能分享，甚至发生数据泄露事件。

第六种误用是日志级别乱用。正常失败打 ERROR，用户输入错误打 FATAL，重试中间态打 ERROR，真正的数据损坏却只打 INFO。症状是报警噪声很大，值班对 ERROR 麻木；真正严重问题被淹没。

第七种误用是把日志当 metrics。每次请求都打一条 structured log，然后从日志里算 QPS、错误率和 p99。症状是日志成本高、实时性差、采样后结果不准，报警延迟大。核心 SLI 应该走 metrics，日志用来解释样本。

第八种误用是把日志当状态源。比如系统重启后从普通应用日志里恢复状态，或者根据日志是否存在判断任务是否完成。症状是 crash 后状态不一致、采样导致恢复缺口、日志重放出现重复副作用。状态恢复应该依赖数据库、WAL、event store 或 LogServe 里的 shared log。

第九种误用是在热路径打大对象。比如每个 token、每个队列 item、每次锁等待都打完整结构。症状是 CPU 高、GC 高、stdout 阻塞、p99 升高、磁盘和网络打满。严重时日志系统会反过来制造事故。

第十种误用是没有上下文。错误日志只有 `failed`，没有 `trace_id`、`request_id`、`dependency`、`attempt`、`error_code`。症状是日志很多，但不能串起来；从报警跳到日志后，仍然不知道是哪条请求、哪个下游、哪次重试。

第十一种误用是同步等待日志后端。应用请求要等 collector 或日志平台 ack，后端一慢，业务也慢。症状是日志后端故障引发业务 p99 升高，甚至服务不可用。

第十二种误用是没有过期治理。旧字段没人用，event 名改了但旧 dashboard 还在，日志格式漂移没人知道。症状是 dashboard 里出现看似权威但已经失真的图，排障被旧指标和旧字段误导。

对 LogServe 来说，典型误用是把 `workflow_id`、`task_id`、完整 prompt/result 当成默认索引字段；或者把 shared log 的恢复事件和普通 debug log 混在一起；再或者每个 worker poll 都打 INFO。线上症状会是日志量暴涨、查询卡顿、LLM 数据泄露风险、worker 热路径 p99 被日志拖慢。

面试里可以这样回答：

```text
常见误用包括：JSON 包纯文本、字段名不统一、动态 key、高基数字段乱索引、记录敏感数据、日志级别乱用、用日志替代 metrics、用普通日志当状态源、热路径打印大对象、没有 trace/request/attempt 上下文、同步等待日志后端、旧字段不治理。线上症状通常是查询靠字符串、告警噪声大、日志平台成本和索引膨胀、p99 升高、日志丢失、排障串不起来，严重时会有敏感信息泄露或恢复状态错误。
```

## Q045. structured logging 在单机和分布式环境中的语义有什么差异？

单机环境里，structured logging 主要解决“一个进程或一台机器内部发生了什么”。分布式环境里，它要解决“多个服务、多个节点、多个时钟、多个采集链路看到的局部事实怎么关联”。难度不一样。

单机里，日志顺序相对可信，但也不是绝对可信。多线程或多 goroutine 同时写日志时，只能保证单条记录完整，不一定保证业务事件的真实先后。异步 logger 还可能重排。即便如此，单机至少共享同一个进程上下文、同一个本地时钟、同一个 stdout/file sink。排障时可以更多依赖进程内序列、goroutine id、线程 id、局部 monotonic duration。

分布式环境里，没有天然全局顺序。不同机器的 wall clock 可能有偏移，collector 采集时间也不同。OpenTelemetry Logs Data Model 里有 `Timestamp` 和 `ObservedTimestamp`，就是因为事件发生时间和采集观察时间可能不是一回事。跨机器日志不能简单按 timestamp 排序后就当作真实因果链。

单机里，request context 往往在进程内传递。分布式里，必须显式传播 trace context、request ID、correlation ID。W3C Trace Context 解决的是跨服务传递 `traceparent` 和 `tracestate`；OpenTelemetry 也建议日志里带 trace/span context，才能从 trace 跳到日志，从日志跳回 trace。没有上下文传播，分布式 structured logs 只是很多服务各写各的 JSON。

单机里，日志丢失通常来自进程崩溃、缓冲未 flush、磁盘满。分布式里还会多出 agent 崩溃、collector 限流、网络分区、后端 ingest 失败、跨 region 延迟、租户限额。也就是说，分布式日志链路通常是 at-least-once 或 best-effort：可能重复，可能延迟，可能丢。设计查询和审计时不能假设“每个事件只出现一次”。

单机里，字段命名不统一已经麻烦；分布式里会变成组织问题。不同语言、不同团队、不同框架如果都发明自己的字段名，跨服务查询会失败。分布式环境更需要 schema 规范和语义约定：

```text
service.name
service.version
deployment.environment
trace_id
span_id
http.method
http.route
rpc.service
error.type
```

单机里，高基数字段主要影响本机日志文件大小和查询。分布式里，高基数字段会影响全局索引、存储、租户隔离和账单。一个服务加了 `user_id` 默认索引，可能拖慢整个日志集群。

单机里，安全边界相对窄。分布式里，日志会经过 sidecar、agent、collector、queue、对象存储、第三方 SaaS，访问面扩大。structured fields 让脱敏更容易，也让敏感字段更容易被稳定复制。跨服务传播的 baggage、trace state、headers 都要特别小心。

单机里，日志和状态的关系容易控制。分布式里，日志常常只是局部观察。服务 A 记录“请求已发送”，服务 B 可能没收到；服务 B 记录“处理成功”，服务 A 可能已经超时；队列消费者记录“任务开始”，随后崩溃，另一个消费者又记录“任务开始”。这些日志都是真的，但它们描述的是不同组件的局部事实。

这也是为什么分布式 structured logging 必须带观察者身份：

```text
service.name
instance_id
region
zone
side=client/server/consumer/producer
attempt
message_id
workflow_id
epoch
sequence/log_offset
```

在 LogServe 里，这个差异非常明显。单机实验中，control、logd、worker 都在同一台机器上，时钟和网络问题较少；但系统语义仍然是多进程的，worker kill、redelivery、control restart 已经会产生重复和迟到事件。如果扩展到多机，structured logs 更不能承担状态顺序判断，必须依赖 shared log offset、actor `command_seq`、epoch fencing、idempotency key 来判定状态，日志只负责解释这些机制的观察结果。

面试里可以这样回答：

```text
单机 structured logging 主要面对并发写、缓冲、文件 I/O 和进程崩溃；分布式环境还要面对时钟不同步、上下文传播、collector/agent 故障、网络分区、重复和延迟日志、跨团队 schema 不一致、高基数字段放大全局成本。单机日志顺序相对可用，但分布式日志不能按 timestamp 当作因果顺序，必须依赖 trace_id/span_id、request_id、attempt、message_id、sequence、log offset、epoch 等字段关联。每条日志只是某个组件的局部观察，不是全局真相。像 LogServe 这种系统，状态顺序靠 shared log 和 fencing，structured logs 负责解释和排障。
```

## Q046. trace_id 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

`trace_id` 的核心目标是把一次端到端请求或一次分布式操作里的多个 span 归到同一条 trace 里。它解决的是“这些局部事件是不是同一件事的一部分”这个关联问题。W3C Trace Context 里，`traceparent` 包含 `version`、`trace-id`、`parent-id`、`trace-flags` 四段；其中 `trace-id` 是 16 字节、32 个小写十六进制字符，全 0 是无效值。OpenTelemetry 也沿用这个思路：同一条 trace 下的 span 共享同一个 trace ID，不同 span 用各自的 span ID 和 parent 关系表达调用树。

所以，`trace_id` 的主目标是可维护性和可诊断性。更准确一点，是跨进程、跨服务、跨日志/trace/metrics 的关联能力。它让值班人员能从一条错误日志跳到整条 trace，从慢 span 跳回相关日志，从入口请求看到下游 RPC、数据库、队列、LLM 调用等路径。

它不直接解决业务正确性。一个请求带了 `trace_id`，不代表它只会执行一次，不代表状态机会按顺序推进，也不代表结果可以安全去重。正确性仍然要靠事务、幂等键、sequence、epoch、fencing、WAL、数据库约束。LogServe 里 `workflow_id + step_id + input_hash` 才适合表达 step 级结果去重；actor 的 `command_seq` 和 epoch 才适合表达状态应用顺序。`trace_id` 只能帮你把相关证据串起来。

它也不直接解决性能。trace 让性能问题更容易定位，例如看到入口 p99 被某个下游 span 拉高，但 trace ID 本身不会让请求更快。生成 ID、传播 header、创建 span、导出 trace 还会带来开销。采样、批量导出和 collector 设计，就是为了控制这部分成本。

安全性也不是它的主目标。`trace_id` 通常不是秘密，不应该被当成 token、权限凭据、会话 ID 或用户身份。W3C Trace Context 文档专门有 privacy 和 security considerations，因为 trace context 会跨边界传播；如果把敏感信息塞进 `tracestate`、baggage 或日志字段，就可能泄露。但 `trace_id` 本身只是一种关联 ID，不负责认证授权。

面试里容易被追问：“有 trace_id 是不是就能定位所有问题？”答案是否定的。`trace_id` 只能把观测数据放进同一个篮子里。能不能解释问题，还取决于 span 是否覆盖关键路径、attributes 是否有业务语义、日志是否带错误类别、采样是否保留了这条 trace、上下文传播是否在异步边界继续传下去。

在 LogServe 里，`trace_id` 适合贯穿一次 workflow 提交、step 调度、worker 执行、LLM 调用、actor command 处理和 result ref 写入。这样当 `workflow_duration` p99 升高时，可以沿着 trace 找到慢在 queue wait、Python executor、LLM checkpoint fetch，还是 shared log append。但这条 trace 不是恢复协议。重启后系统状态仍然要从 shared log replay，不能从 trace 后端恢复。

面试里可以这样回答：

```text
trace_id 的核心目标是关联一次分布式操作里的所有 span、日志和相关证据。它主要解决可维护性和可诊断性问题，不是业务正确性机制，也不是性能优化或安全凭证。W3C Trace Context 里 trace-id 是 traceparent 的一部分，用来唯一标识一条分布式 trace；span_id 表示其中某个操作。trace_id 能帮助我从日志跳到 trace、从慢请求找到下游路径，但它不能替代 idempotency key、transaction、sequence、epoch 或访问 token。在 LogServe 里它适合串联 workflow/task/actor/LLM 的观测证据，状态恢复仍然靠 shared log。
```

## Q047. trace_id 的典型适用场景和不适用场景分别是什么？

`trace_id` 最适合用在“同一次操作跨过多个组件”的场景。只要一个请求不再停留在单个函数里，而是穿过网关、服务、数据库、缓存、队列、worker、对象存储，就应该考虑把 trace context 传下去。

典型适用场景包括：

```text
HTTP/gRPC 入口请求穿过多个微服务。
一次请求 fan-out 到多个下游服务。
消息生产者和消费者之间需要关联。
workflow step 从 control 调度到 worker，再写回结果。
LLM 请求经历模型选择、checkpoint fetch、model load、inference。
一次用户操作同时产生日志、trace、metrics exemplar。
事故复盘时要把多台机器上的证据放回同一条链路。
```

对在线服务来说，`trace_id` 的价值很直接。入口 p99 升高时，你可以用 trace ID 找到慢请求经过了哪些服务；下游服务报错时，你可以反查这次错误来自哪个入口请求；日志里只有一条 `dependency timeout` 时，你可以用 trace ID 找到同一请求里的重试、deadline、fan-out 分支。

对异步系统来说，`trace_id` 仍然有用，但语义要小心。消息队列、workflow、actor mailbox 不一定是严格父子调用。OpenTelemetry 里可以用 span links 表达“有关联但不是嵌套父子”的关系。比如一个 workflow 提交事件触发多个 step，step 可能并行执行；把所有 step 都硬塞成同步父子 span 会误导时间线。更合理的是保留同一 trace 或建立 link，并在字段里带 `workflow_id`、`step_id`、`attempt`。

对日志关联来说，`trace_id` 很适合放进 structured logs。OpenTelemetry Logs Data Model 里也有 `TraceId`、`SpanId`、`TraceFlags` 字段。这样从日志平台搜索某个 trace ID，可以看到同一次操作下的错误日志、状态变化和关键上下文。

不适用场景同样重要。

第一，`trace_id` 不适合作为幂等键。幂等键应该表达“同一个业务操作的重复提交”，通常由客户端或业务层控制，并和 payload、operation、资源语义绑定。trace ID 可能因为重试、入口代理、采样策略、客户端缺失 context 而变化。用它做去重会漏，也会误伤。

第二，`trace_id` 不适合作为安全凭证。它可能出现在日志、响应头、错误页面、第三方服务、工单和截图里。把它当 session token 或权限票据，是典型安全错误。

第三，`trace_id` 不适合作为业务主键。订单、用户、workflow、task、actor 都应该有自己的业务 ID。trace ID 的生命周期通常是一次请求或一次操作，业务对象的生命周期可能跨越天、月甚至年。

第四，`trace_id` 不适合作为 metrics label。它是高基数字段。把 `trace_id` 放进 Prometheus label，会制造接近“一次请求一个时间序列”的灾难。它可以用于 exemplars 或日志字段，但不该进入常规聚合标签。

第五，不要用 `trace_id` 做全局排序。trace ID 随机生成，不表达时间，不表达 causal order。顺序要看 span parent、timestamp、sequence、log offset、message offset、command_seq。

第六，不要用 `trace_id` 做用户跟踪。它不是 user ID，也不应该跨很长时间稳定绑定用户。若把 trace ID 和用户身份长期绑定，还会带来隐私风险。

在 LogServe 里，适用场景是把一次 SDK 调用或 workflow run 的观测路径串起来；不适用场景是替代 `workflow_id`、`task_id`、`actor_id`、`idempotency_key`、`command_seq`。它可以帮助定位“这次 workflow 为什么慢”，不能用来判断“这个 step 是否已经正确完成”。

面试里可以这样回答：

```text
trace_id 适合跨服务、跨进程、跨日志和 trace 关联一次端到端操作，例如 HTTP/gRPC 请求、消息消费、workflow step、LLM 调用和事故复盘。它不适合作为幂等键、安全 token、业务主键、metrics label、全局排序字段或长期用户跟踪 ID。异步系统里还要注意，消息和 workflow 不总是父子调用，必要时用 span links，再配合 workflow_id、message_id、attempt 等业务字段。在 LogServe 里 trace_id 用于观测关联，workflow_id 和 shared log 才是业务和恢复语义。
```

## Q048. trace_id 和相近概念最容易混淆的边界在哪里？

`trace_id` 最容易和 `span_id`、`request_id`、`correlation_id`、`parent_id`、`tracestate`、baggage、业务 ID、幂等键混在一起。边界没说清楚，排障和设计都会出问题。

先看 `trace_id` 和 `span_id`。`trace_id` 标识整条 trace，`span_id` 标识这条 trace 里的某个操作。一次请求可能只有一个 trace ID，但会有很多 span ID：入口 span、RPC client span、RPC server span、DB query span、queue publish span、worker consume span。W3C `traceparent` 里的 `parent-id` 对应调用方看到的当前 span 位置，不是整条 trace 的 ID。

`request_id` 和 `trace_id` 的边界更容易混。`request_id` 通常是网关或应用定义的相关 ID，范围由系统自己决定；`trace_id` 是 tracing 语义里的分布式 trace ID。一个系统可能只有 request ID，没有 tracing；也可能 trace ID 贯穿多个内部请求。迁移阶段常见做法是两个都带：`request_id` 服务日志查询习惯，`trace_id` 服务调用链分析。

`correlation_id` 是更宽泛的词。它表示“用于关联的 ID”，可以是 request ID、trace ID、message correlation ID、workflow ID。面试里我会避免把 correlation ID 当成具体协议字段。问到具体实现时，要说清楚它到底怎么生成、传播、生命周期多长。

`tracestate` 不是 trace ID。W3C Trace Context 用 `tracestate` 携带厂商或系统特定的 trace 元数据。它可以帮助多 tracing 系统互操作，但不应该塞业务大字段，也不应该放敏感信息。`trace_id` 是公共主线，`tracestate` 是扩展槽位。

baggage 也不是 trace ID。OpenTelemetry baggage 用来传播少量跨服务上下文，比如 tenant tier、experiment group。它会跟着请求传到下游，因此更要小心隐私和体积。trace ID 只是关联这条 trace，baggage 是额外上下文。

业务 ID 和 trace ID 的边界要特别强调。`workflow_id`、`task_id`、`actor_id`、`order_id` 是业务对象或运行时对象的身份。它们可以出现在多条 trace 中。比如同一个 workflow 重试了三次，可能出现多个 trace ID；同一个 actor 处理很多 command，也会出现在很多 trace 中。反过来，一条 trace 里也可能涉及多个业务对象。

幂等键和 trace ID 更不能混。幂等键用于判断重复请求是否代表同一个业务操作；trace ID 用于观测关联。同一次业务重试可能复用幂等键，但生成新的 trace ID；同一 trace 内也可能有多个下游幂等操作。用 trace ID 去做幂等，会把观测层和业务层绑死。

采样标记也不是 trace ID。W3C `trace-flags` 里当前定义了 sampled flag，它只是传播采样相关建议或状态。它不保证这条 trace 一定被完整保存，也不保证下游必须采样。不同组件负载不同，可能会做自己的采样决定。

在 LogServe 里，可以这样分：

```text
trace_id：一次端到端观测链路。
span_id：链路中的一个操作，例如 schedule step、execute task、LLM call。
workflow_id：workflow 实例身份。
task_id：任务身份。
actor_id：actor 身份。
command_seq：actor 内部顺序。
idempotency_key：重复提交判定。
log offset：shared log 中的持久化顺序。
```

这些字段经常同时出现，但职责不一样。trace ID 负责“查证据”，业务 ID 和序列负责“定义系统语义”。

面试里可以这样回答：

```text
trace_id 标识整条 trace，span_id 标识其中一个操作；request_id 是应用或网关定义的请求相关 ID；correlation_id 是泛称；tracestate 是厂商扩展上下文；baggage 是跨服务传播的业务上下文；workflow_id、task_id、order_id 是业务对象 ID；idempotency key 用于重复提交判定。trace_id 只负责观测关联，不表达业务身份、顺序、权限或幂等。在 LogServe 里，trace_id 帮我查一次 workflow 调用的证据，workflow_id、command_seq、epoch、log offset 才定义状态语义。
```

## Q049. trace_id 在高并发场景下可能出现哪些隐藏问题？

高并发下，`trace_id` 本身只是 16 字节 ID，真正的问题来自生成、传播、采样、存储和索引。问题经常很隐蔽，因为低流量测试时一切正常，线上 QPS 上来才暴露。

第一个问题是 ID 生成质量。W3C 要求 trace-id 不能全 0，并强调唯一性和随机性。高并发服务如果用低质量随机数、时间戳拼接、进程内自增、短 ID，可能产生碰撞。碰撞后两次无关请求会被拼成一条 trace，排障会非常混乱。更糟的是，如果 trace ID 可预测，外部调用方可能构造大量带同一 trace ID 的请求，污染日志和 trace。

第二个问题是随机数生成成为瓶颈。每个入口请求都要生成 trace ID，极高 QPS 下如果每次都阻塞在系统随机源或全局锁上，会影响入口延迟。成熟 SDK 通常会处理这件事，但从零实现时要测。不能为了性能退回可预测 ID。

第三个问题是上下文串线。高并发服务里，context 如果放在线程局部变量、全局变量、复用对象或 goroutine 不安全结构里，就可能把 A 请求的 trace ID 带到 B 请求。异步任务、goroutine pool、worker pool、callback、future/promise 都容易出这种问题。症状是日志明明来自不同用户或 workflow，却共享同一个 trace ID。

第四个问题是上下文丢失。只在入口创建 trace ID，但没有注入 HTTP/gRPC header、消息属性、队列 metadata、worker context，下游就会生成新 trace。结果是调用链断开，看起来像多个孤立请求。异步边界最常见：producer 有 trace ID，consumer 没有提取；workflow control 有 trace ID，worker 执行没有继承或 link。

第五个问题是采样放大偏差。高并发下不可能全量保留所有 trace。head-based sampling 成本低，但可能错过慢请求；tail-based sampling 能保留慢请求和错误请求，但 collector 要缓存更多 span。W3C 也提到只记录部分请求会产生 broken traces，随机或组件各自决定可能导致 trace 碎片化。采样策略如果没有统一设计，trace ID 存在，trace 数据却不完整。

第六个问题是日志和指标平台被高基数字段拖慢。trace ID 可以写日志字段，但不应该变成常规 metrics label。日志平台也要区分存储和索引：每条日志都带 trace ID 很有用，但未必每个环境都要把它作为昂贵索引。高并发下，一个请求一个 trace ID，索引成本会很快上来。

第七个问题是头部体积和传播成本。`traceparent` 很小，但如果同时携带大 `tracestate`、baggage、内部调试字段，请求头会变大。高 QPS 下这会增加网络和解析成本，也可能触发代理、网关、消息系统的 header size 限制。

第八个问题是信任边界。外部请求可以带入 `traceparent`。服务端如果完全信任外部 trace ID，可能被恶意请求污染内部 trace，或者把不同租户请求故意串在一起。通常要校验格式、拒绝全 0、限制 tracestate/baggage，必要时在边界重新生成内部 trace，并把外部 ID 作为单独字段保留。

在 LogServe 里，高并发场景会集中出现在 worker pool 和 LLM serving。worker 复用 goroutine 时要确保每个 task 的 context 独立；LLM 请求如果 fan-out 或重试，要保证 attempt 的 span 关系清楚；dashboard 或 metrics 不能把 trace ID 当 label；日志可以带 trace ID，但状态去重仍然看 idempotency key 和 log-first 事件。

面试里可以这样回答：

```text
高并发下 trace_id 的隐藏问题包括 ID 碰撞或可预测、随机数生成成本、context 串线、异步边界丢 context、采样导致 trace 碎片化、trace_id 进入 metrics label 造成高基数、tracestate/baggage 过大、外部 traceparent 污染内部链路。处理上要用合格的 16 字节随机 trace ID，校验输入，context 随请求显式传递，worker/goroutine 不能复用错上下文，异步场景用 propagation 或 span links，采样策略要统一，trace_id 只用于日志精确关联，不进常规 metrics label。
```

## Q050. trace_id 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

`trace_id` 在正常请求里很好理解，边界情况会复杂很多。崩溃、重启、超时、重试都会让“同一次操作”这个概念变得不稳定。

崩溃时，正在内存里的 span 可能还没导出。应用已经处理了一半，请求日志里有 trace ID，但 trace 后端里没有完整 span；或者只有入口 span，没有下游 span。异步 exporter、batch span processor、collector 队列都会带来这种窗口。崩溃附近的 trace 数据是 best effort，不能当成审计级事实。

重启后，进程内 context 消失。队列消息、workflow step、actor command 如果被重新投递，新的 worker 可能创建新 trace ID，也可能从消息 metadata 里恢复原 trace context。两种选择都可以，但语义要明确。恢复原 trace 可以把重试串回同一条链路；新 trace 更能表达“这是一次新的执行尝试”。实际系统里常见做法是：operation-level 字段保持稳定，比如 `workflow_id`、`step_id`、`idempotency_key`；attempt-level trace 可以新建，并用 link 或字段指回原始 trace。

超时时，要区分观察者。客户端 timeout 后，服务端可能继续执行并成功；worker timeout 后，control 可能重新投递；下游 RPC timeout 后，远端可能已经提交副作用。trace ID 能把这些局部事实串起来，但不能消除分歧。日志和 span 里应写清楚：

```text
side=client/server/worker/control
attempt=2
deadline_ms=500
elapsed_ms=501
cancelled=true
remote_may_continue=true
```

重试时，最大的问题是 trace 形状。一次用户操作可能有多个 attempt。它们应该是同一个 trace 里的 sibling span，还是多个 trace 通过 link 关联？没有唯一答案，取决于重试发生在哪里。同步 RPC 客户端内部重试，通常可以放在同一 trace 下，用 attempt span 表达；队列 redelivery、进程重启后的 workflow step 再执行，可能更适合新 trace 加 link。关键是不要让 attempt 消失。

还要注意 sampling。第一次 attempt 可能没采样，第二次失败才采样；或者入口采样了，下游因为负载丢了。这样 trace ID 存在，但证据不完整。面试里要承认这一点，不要说“有 trace ID 就一定能看到完整链路”。

传播失败也常出现在重启和边界组件上。代理、消息队列、批处理任务、cron、serverless 冷启动、CLI 工具可能没有正确提取或注入 `traceparent`。结果是同一业务操作被拆成多个 trace。解决办法是对关键边界做 propagation test，并在日志里保留业务 ID，作为 trace 断裂时的后备关联。

在 LogServe 中，worker kill recovery 和 queue redelivery 就是典型边界。如果一个 task 执行中 worker 被 kill，control 重新调度该 task。此时同一个 `task_id` 可能出现两次 `TaskStarted`，它们可以有不同 trace ID 和 attempt。判断哪次结果有效，不能看 trace ID，要看 idempotency、log-first completion、worker epoch 或控制面状态。trace ID 负责解释“为什么发生了两次”。

面试里可以这样回答：

```text
崩溃时，trace span 可能没导出，trace 数据只能 best effort；重启后进程内 context 消失，redelivery 或恢复执行可能生成新 trace，也可能从消息里恢复原 trace，语义要提前定；超时时要区分 client、server、worker 哪一侧观察到 timeout，因为远端可能继续执行；重试时要显式记录 attempt，决定是在同一 trace 下建 sibling spans，还是新 trace 加 link。采样和传播失败会让 trace 不完整。在 LogServe 里，同一个 task 重试可以有多个 trace，真正的有效性判断仍然靠 shared log、idempotency、epoch 和状态机。
```

## Q051. trace_id 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

`trace_id` 字段本身很小，真正的性能成本来自围绕它展开的 tracing 机制：生成、解析、传播、span 创建、attribute 记录、采样、导出、存储和查询。所以瓶颈也不是单点。

CPU 成本主要来自几类操作：生成随机 trace ID，解析/格式化 `traceparent`，创建 span，记录 attributes/events，时间戳读取，采样判断，序列化 OTLP/JSON。单次成本不大，但入口 QPS 很高、span 很密、每个 span attributes 很多时，CPU 会变明显。

内存成本来自 span 对象、context 对象、attribute map、events、links、batch 队列。tail-based sampling 还会把同一 trace 的 span 暂存在 collector 里，等看到结果后再决定是否保留。trace 越长、fan-out 越大、采样窗口越长，内存压力越高。

锁竞争通常出现在 ID generator、global tracer provider、sampler、span processor、export queue、resource/attribute 共享结构。成熟 SDK 会尽量降低这些成本，但自研实现或错误使用全局可变状态，很容易在高并发下卡住。

I/O 成本来自 exporter 和 collector。应用把 span 发到本地 agent 或远端 collector，collector 再写后端。后端慢、磁盘慢、队列满，都会影响 tracing 管道。好的实现会批量、异步、有界队列，并在压力下丢弃或采样，而不是阻塞业务请求。

网络成本来自 header 传播和 trace 导出。`traceparent` 自身很小，通常不是问题；`tracestate` 和 baggage 变大后，才会明显增加请求头解析和传输成本。真正的大头通常是 span export：高 QPS 服务、全量采样、详细 attributes、错误堆栈、events，会让 OTLP 流量和后端 ingest 压力上来。

还有一个容易忽略的成本是存储和查询。trace ID 高基数是正常现象，trace 后端就是为按 trace ID 查单条链路设计的；但如果把 trace ID 复制到 metrics label，Prometheus 这类 TSDB 会被打爆。日志系统也要控制 trace ID 的索引策略和保留周期。

优化时不要只盯 trace ID 生成。更常见的收益来自这些地方：

```text
控制 span 数量，不给每个小函数都建 span。
控制 attributes 数量和大小，不记录大对象和高敏字段。
使用 head/tail sampling，错误和慢请求优先保留。
batch export，队列有界，后端慢时 fail open。
避免在 hot path 里反复解析/格式化 trace ID 字符串。
异步边界只传播必要 context，不滥用 baggage。
监控 dropped spans、export latency、queue length、sampling ratio。
```

在 LogServe 里，trace 的开销可能出现在 workflow fan-out、worker poll、LLM token 级别埋点。每个 workflow step 建 span 是合理的；每个内部循环或每个 token 都建 span，通常不合理。LLM 的关键 span 应该是 checkpoint fetch、model load、inference、result store 写入，而不是把整个模型输出塞进 attributes。

面试里可以这样回答：

```text
trace_id 本身不是主要瓶颈，成本来自 tracing 管道。CPU 在 ID 生成、traceparent 解析、span 创建、采样、序列化；内存在 span、attributes、events、links、batch queue 和 tail sampling 缓存；锁竞争可能在 generator、sampler、processor、export queue；I/O 和网络来自 exporter、collector、后端 ingest。优化重点是控制 span 数和 attribute 大小，采样，批量导出，有界队列，后端慢时不阻塞业务，并避免把 trace_id 放进 metrics label。LogServe 里适合按 workflow step、LLM call、actor command 建 span，不适合给每个热循环操作建 span。
```

## Q052. trace_id 的 correctness test、stress test 和 benchmark 应该分别测什么？

这三类测试要分开。correctness test 测语义对不对；stress test 测高并发和故障下会不会断链、串线、拖垮系统；benchmark 测 trace ID 和 tracing 管道成本。

correctness test 先测格式和传播：

```text
生成的 trace_id 是 16 字节、32 个小写 hex 字符。
全 0 trace_id 被拒绝。
非法 traceparent 被忽略或重新生成，行为明确。
span_id 是 8 字节、16 个小写 hex 字符，不能全 0。
收到合法 traceparent 时，子 span 继承同一 trace_id。
跨 HTTP/gRPC 出站请求会注入 traceparent。
入站请求能提取 traceparent。
日志记录里能带 trace_id 和 span_id。
```

还要测边界概念：`trace_id` 不能替代业务 ID。比如同一个 `workflow_id` 的不同 attempt 可以有不同 trace ID；同一 trace 里可以有多个 `task_id`；幂等判断不依赖 trace ID。测试里要防止开发者偷懒把 trace ID 当 operation key。

异步场景必须单独测。消息 producer 注入 context，consumer 提取 context；如果系统选择新建 trace，也要能用 span link 或字段关联原 trace。workflow step、actor command、queue redelivery 都应该覆盖。单纯测 HTTP 中间件不够。

correctness 还要测安全和信任边界：外部传入全 0、过长、非法字符、混合大小写、超大 `tracestate`、超大 baggage 时，系统不能 panic，不能无限传播，不能把敏感内容写进 trace state。W3C 对 `traceparent` 有明确格式要求，测试应该按这个来。

stress test 主要看高并发：

```text
大量并发入口请求生成 trace ID，无碰撞。
worker pool/goroutine pool 不串 context。
高 fan-out 请求下 trace 树不丢 parent。
collector 慢或不可用时业务请求不被长时间阻塞。
export queue 满时 dropped spans 计数正确。
采样率在高并发下符合预期。
恶意外部 traceparent 不会污染内部系统。
```

还要做故障 stress：进程崩溃、重启、redelivery、超时、重试。预期不是 trace 永远完整，而是行为可解释：哪些 span 可能丢，哪些日志保留 trace ID，哪些 attempt 生成新 trace，dropped count 是否可见。

benchmark 则要拆得更细：

```text
只生成 trace ID 的 ns/op、B/op、allocs/op。
解析和格式化 traceparent 的成本。
创建 no-op span 的成本。
创建 sampled span + N 个 attributes 的成本。
context inject/extract 的成本。
日志注入 trace_id/span_id 的成本。
batch export enqueue 成本。
collector/exporter 在不同采样率下的吞吐。
```

如果是 Go 项目，可以用 `go test -bench -benchmem` 看 allocs；并发 benchmark 要看 `-cpu` 不同设置下是否出现锁竞争。单机 benchmark 只能说明 SDK 成本，不能代表 trace 后端查询性能；后端还要测 ingest、query by trace ID、retention、索引压力。

对 LogServe，我会设计这些测试：

```text
correctness：SDK -> control -> worker -> LLM/actor 的 trace_id 能传递或 link，日志字段带 trace_id/span_id。
stress：高并发 workflow 提交、worker kill recovery、queue redelivery 下 context 不串、不 panic。
benchmark：workflow step span、LLMCompleted span、actor command span 的 ns/op、B/op、allocs/op，以及开启 tracing 前后 task p99 差异。
```

面试里可以这样回答：

```text
correctness test 测 trace_id 格式、非法 traceparent 处理、入站提取、出站注入、span 父子关系、日志关联、异步消息传播或 span links，并确认 trace_id 不参与幂等和业务状态判断。stress test 测高并发生成无碰撞、worker pool 不串 context、fan-out 不断 parent、collector 慢时业务不阻塞、队列满时 dropped spans 可见、崩溃重启重试行为可解释。benchmark 测 ID 生成、traceparent 解析、span 创建、attribute 记录、context inject/extract、export enqueue 的 ns/op、B/op、allocs/op，再看开启 tracing 对 p99 的影响。
```

## Q053. 如果要求从零实现一个简化版 trace_id，你会先定义哪些不变量？

从零实现 trace ID，我会先定义不变量，而不是先写随机数函数。trace ID 一旦跨服务传播，后面很难改。错误的不变量会造成断链、串线、碰撞、隐私问题，排障时很痛苦。

第一，格式不变量。采用 W3C Trace Context：`trace_id` 是 16 字节，序列化为 32 个小写十六进制字符；全 0 无效；`span_id` 是 8 字节，16 个小写十六进制字符；`traceparent` 版本先支持 `00`。非法输入不能 panic，要忽略并重新生成或按明确策略处理。

第二，唯一性和随机性不变量。新 root trace 必须用足够随机的 16 字节 ID。不能用时间戳、自增、机器 ID 拼短随机数。不能在 fork、重启、容器复制后重复种子。生成器要并发安全，不能因为锁竞争把入口打慢。

第三，传播不变量。只要进入受控边界，当前 context 里最多有一个 active span context；出站 HTTP/gRPC/message 必须注入 `traceparent`；入站请求必须提取并校验 `traceparent`。跨异步边界时，要么传播原 context，要么创建新 trace 并记录 link，不能悄悄丢。

第四，父子关系不变量。同一 trace 下，新 span 必须有自己的 span ID；如果有 active parent，则记录 parent；如果没有 parent，就是 root span。不能复用 parent span ID 当 child span ID。不能让两个并发 child 共享同一个 span ID。

第五，生命周期不变量。trace ID 的生命周期是一次分布式操作或一次观测链路，不等于用户会话，不等于业务对象。`workflow_id`、`task_id`、`actor_id`、`message_id` 要作为 attributes 或日志字段存在，不能被 trace ID 替代。

第六，采样不变量。采样决定不能改变 trace ID。未采样请求也可以传播 trace context，让下游有机会关联或按策略采样。`sampled` flag 只是采样相关信号，不是权限，不是完整性保证。

第七，安全边界不变量。外部传来的 trace context 不能无条件信任。必须校验格式、限制 `tracestate` 和 baggage 大小、过滤敏感字段。对公网入口，可以选择接受外部 trace ID，也可以生成内部 trace ID 并把外部 ID 作为单独字段记录。不要把 trace ID 当 secret。

第八，日志关联不变量。所有结构化日志如果发生在 active span 内，就应该能拿到 `trace_id` 和 `span_id`。但日志写失败不能影响 trace 状态，trace 导出失败也不能影响业务结果。

第九，失败语义不变量。collector 不可用、export queue 满、采样丢弃、进程崩溃，都不能让业务请求因为 trace 系统卡死。要有 dropped span 计数、export error 计数和队列长度指标。

第十，测试不变量。任何改动都要跑格式测试、传播测试、并发测试、非法输入 fuzz、benchmark。trace ID 是基础设施字段，出错后影响所有排障工具。

一个简化版接口可以很小：

```go
type TraceContext struct {
    TraceID TraceID
    SpanID  SpanID
    Sampled bool
}

func NewRoot() TraceContext
func ParseTraceparent(header string) (TraceContext, bool)
func Inject(ctx context.Context, carrier HeaderCarrier)
func Extract(ctx context.Context, carrier HeaderCarrier) context.Context
func StartSpan(ctx context.Context, name string) (context.Context, Span)
```

在 LogServe 里，我会把这些不变量落到 SDK、control、worker 三个边界上：SDK 创建或接收 trace context；control 调度 workflow/task 时传递；worker 执行 task、actor command、LLM call 时创建子 span或 link；日志统一带 `trace_id`、`span_id`、`workflow_id`、`task_id`、`attempt`。恢复和去重仍然走 shared log 与 idempotency，不把 trace ID 拉进状态机。

面试里可以这样回答：

```text
从零实现 trace_id，我会先定这些不变量：W3C 兼容格式，16 字节 trace_id、8 字节 span_id、全 0 无效；新 root trace 用高质量随机数；生成器并发安全；入站提取并校验 traceparent，出站注入；子 span 不复用 parent span_id；异步边界要传播 context 或建立 link；trace_id 生命周期是一次观测链路，不替代 workflow_id、task_id、idempotency key；采样不改变 trace_id；外部 trace context 不无条件信任；日志能关联 trace/span；collector 故障不能阻塞业务。实现前先把这些写成测试，比先写随机字符串函数更重要。
```
## Q054. trace_id 的常见误用是什么，误用后通常会产生什么线上症状？

`trace_id` 最常见的误用，是把它从观测层 ID 提升成业务语义 ID。它本来只负责把一组 span、日志和相关证据串起来，帮助排障；一旦被拿去做幂等、权限、状态机顺序、业务主键或指标维度，问题就会变得很隐蔽。因为低流量测试时它看起来“刚好能用”，线上一遇到重试、异步、采样、网关改写、外部请求注入，就会暴露。

第一类误用是把 `trace_id` 当幂等键。幂等键要表达“这两次提交是不是同一个业务意图”，通常和 operation、payload、resource、client token、过期时间绑定。`trace_id` 表达的是“这批遥测证据属于同一条观测链路”。一次业务重试可能生成新的 trace ID；一次 trace 里也可能包含多个下游幂等操作。拿 trace ID 去去重，线上症状通常是重复提交没有被拦住，或者不相关的请求被误判为重复。支付、订单、workflow submit 这类路径会尤其危险。

第二类误用是把 `trace_id` 当安全凭证。W3C Trace Context 设计的是跨服务传播上下文，`traceparent` 可能从外部请求进入，也可能出现在响应头、日志、工单截图和第三方服务里。trace ID 不是 secret。把它当 session token、下载凭证、权限票据，结果就是只要有人拿到一段日志或错误页面，就可能伪造访问。更常见的轻微版本是把敏感信息塞进 `tracestate`、baggage 或 trace attributes，导致它们被一路传播到下游和日志平台。

第三类误用是把 `trace_id` 放进 metrics label。Prometheus 的 label 代表可聚合维度，`trace_id` 是高基数、近似每次请求一个值。线上症状很典型：时间序列数量暴涨，Prometheus 内存升高，WAL 和 remote write 积压，dashboard 查询变慢，告警规则延迟，最后监控系统本身开始不稳定。trace ID 可以进日志字段，也可以作为 exemplar 关联到某个慢请求样本，但不该成为常规指标标签。

第四类误用是用 `trace_id` 表达顺序。trace ID 通常是随机值，不表达时间，不表达因果顺序，也不表达 shared log offset。排序要看 timestamp、span parent、message offset、command sequence、log offset、epoch 这类字段。误用后，排障时会看到“看起来后发生的事情排在前面”，或者 actor/workflow 的状态恢复被错误解释。

第五类误用是把 `trace_id` 当用户跟踪 ID。trace ID 的生命周期通常是一条请求或一次分布式操作，不应该长期稳定绑定用户。把它跨天、跨会话保存，既不稳定，也会把观测字段变成隐私字段。真正需要用户维度分析时，应使用合规的业务标识、分级脱敏和访问控制，而不是复用 trace ID。

第六类误用是异步上下文串线。比如把 trace context 放在全局变量、线程局部变量、可复用 request 对象或 goroutine pool 的共享字段里，导致 A 用户的 trace ID 出现在 B 用户日志里。线上症状是最难受的：日志看似有关联，实际拼错了链；trace 图里出现互不相关的 span；同一个 trace 下混入多个 workflow、多个 tenant、多个用户。

第七类误用是无条件信任外部 `traceparent`。外部调用方可以带入全 0、非法格式、超大 `tracestate`，也可以故意复用一个 trace ID 污染内部链路。正确做法是校验格式、限制 `tracestate` 和 baggage 大小，必要时在公网边界重新生成内部 trace，并把外部 trace ID 作为单独字段记录。

在 LogServe 里，我会把边界说得更硬一点：`trace_id` 只能帮助定位一次 SDK 调用、control 调度、worker 执行、actor command、LLM call 和 result store 之间的观测证据。它不能替代 `workflow_id`、`task_id`、`actor_id`、`idempotency_key`、`command_seq` 和 shared log offset。状态是否已经提交、任务是否应该重放、actor 命令是否乱序，要看业务日志和状态机，不看 trace ID。

面试里可以这样回答：

```text
trace_id 的常见误用包括：当幂等键、当安全 token、当业务主键、当 metrics label、当全局排序字段、当长期用户跟踪 ID，或者在 worker pool 里错误复用 context。误用后的线上症状通常是重复请求去重失败、权限绕过风险、Prometheus 高基数爆炸、trace 串线、异步链路断裂、日志关联到错误用户、p99 和错误排查互相矛盾。我的原则是：trace_id 只负责观测关联；业务正确性看 idempotency key、transaction、sequence、epoch、log offset；安全看认证授权；聚合指标看低基数 label。
```

## Q055. trace_id 在单机和分布式环境中的语义有什么差异？

单机环境里，`trace_id` 往往更像“进程内请求上下文 ID”。它能把同一次请求里的函数日志、局部 span、错误栈和本地指标 exemplar 串起来。因为代码都在一个进程或一台机器上，上下文可以通过内存里的 `context.Context`、线程局部变量或请求对象传递，时钟也相对容易解释。只要进程没崩，日志和 span 的完整性通常更好，排障时也更容易还原调用顺序。

但这不代表单机里的 trace ID 就有业务语义。它仍然不是事务 ID、不是幂等键、不是状态机版本。单机只是降低了传播复杂度，没有改变 trace ID 的身份。它解决的是“这些观测记录是不是同一次操作的一部分”，不是“这个操作是否已经正确提交”。

分布式环境里的 `trace_id` 语义更严格，也更脆弱。它必须跨 HTTP、gRPC、消息队列、workflow 调度、worker 执行和第三方服务传播。W3C Trace Context 用 `traceparent` 和 `tracestate` 约定了跨服务携带 trace context 的格式；OpenTelemetry 里同一条 trace 的 span 共享 trace ID，通过 span ID、parent span 和 span links 表达调用关系。这里的重点不是“每台机器都生成一个相同字符串”，而是所有参与方对这个字符串的生命周期、父子关系和传播边界达成一致。

分布式环境还有几个单机里不明显的问题。

第一，链路可能不完整。采样、collector 队列、进程崩溃、网络丢包、下游 SDK 版本不一致，都会让某些 span 消失。trace ID 存在，不等于整条 trace 被完整保存。

第二，时间线可能不可靠。不同机器的 wall clock 有偏移，collector 观察时间也可能晚于事件发生时间。trace 图上的左右顺序不能直接当作强因果顺序。强因果要看 parent/span/link、消息 offset、业务 sequence 和持久化日志。

第三，异步关系不总是父子关系。消息消费、workflow redelivery、actor mailbox、批处理 fan-out 经常只是“有关联”，不是同步调用栈里的嵌套子 span。OpenTelemetry 的 span links 就是为这种情况准备的。为了让图好看而强行嵌套，会把等待时间、并行执行和重试语义画错。

第四，信任边界变复杂。公网入口、合作方服务、网关和消息系统都可能带入或修改 trace context。服务端必须校验格式、限制传播体积，并决定是否接受外部 trace ID。单机程序通常不需要这么多边界处理。

第五，聚合视角不同。单机里一个 trace ID 通常只覆盖一个进程的局部证据；分布式里还要把 service name、instance、region、deployment version、tenant tier 这类 resource/attribute 放进去，否则只能看到“一串 span”，看不出它们来自哪类实例和哪个发布版本。

LogServe 虽然主要是单节点、多进程机制验证，但语义上不能完全按“单机函数调用”理解。SDK、control、worker、Python executor、actor 和 LLM mock 之间已经有进程边界和异步边界。我的设计会按分布式思路传播或链接 trace context，但 correctness 仍然落在 shared log、workflow state、task attempt、actor command sequence 上。

面试里可以这样回答：

```text
单机里 trace_id 主要是进程内请求关联 ID，帮助把日志、局部 span 和错误栈串起来；传播靠内存上下文，完整性和时间线比较容易解释。分布式里 trace_id 是跨服务 trace 的公共关联 ID，需要通过 W3C traceparent、消息 metadata 或 RPC metadata 显式传播，还要处理采样、断链、clock skew、span links、信任边界和多实例聚合。两者共同点是 trace_id 都只表达观测关联，不表达业务提交、幂等、权限或全局顺序。在 LogServe 里，即使部署在单机，多进程和异步 worker 也应该按分布式语义处理 trace context。
```

## Q056. histogram 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

histogram 的核心目标是用可聚合、成本可控的方式描述一组观测值的分布。它不是只存一个平均值，而是把观测值按边界放进 bucket，同时维护 count 和 sum。Prometheus 文档把 histogram 描述成对观测值进行 bucket 计数的指标，常见对象是请求耗时和响应大小。OpenTelemetry 的 Metrics Data Model 也把 Histogram 看成对一批测量值的压缩表示，包含时间窗口、count、sum、bucket counts，以及可选的 min、max、exemplar。

它最直接回答的问题是：

```text
请求耗时大多落在哪个区间？
有多少请求小于 SLO 阈值？
p95、p99 大概是多少？
长尾是不是变厚了？
不同实例、不同服务、不同版本的延迟分布能不能聚合比较？
```

所以 histogram 主要解决的是可维护性和可诊断性问题。它让值班、容量规划、性能回归分析、SLO 计算有稳定的分布视角。它也会间接帮助性能优化，因为你能发现尾延迟、排队、重试放大、fsync 慢、锁竞争这些现象；但 histogram 本身不会让业务请求变快。

它不是业务正确性机制。请求被记录到 `http_request_duration_seconds_bucket{le="0.5"}`，只能说明某个观测值进入了这个 bucket，不能证明订单提交成功、事务已经提交、workflow 状态合法。把 histogram 结果拿去驱动状态机，就混淆了观测层和业务层。

它也不是安全机制。histogram 可以帮助发现异常流量、暴力请求、响应大小异常，但它不负责认证、授权、审计证据和防篡改。安全事件要有日志、审计链路、访问控制和告警策略配合。

性能方面，histogram 是一种折中。相比保存每个原始样本，它省内存、便于聚合、适合长期保留；相比一个 counter 或 gauge，它要维护更多 bucket 和 label 组合，成本更高。设计时要围绕 SLO 和排障问题选择 bucket，而不是为了“看起来完整”放几十个边界。

面试里可以这样回答：

```text
histogram 的核心目标是压缩表示观测值分布：把请求耗时、响应大小、队列等待时间这类样本放入 bucket，同时保留 count 和 sum，方便计算 SLO 达标比例、p95/p99 估计和长尾变化。它主要解决可维护性和可诊断性问题，间接服务性能分析；不是业务正确性机制，也不是安全机制。真正的关键是 bucket 要围绕 SLO 和排障边界设计，否则 histogram 存了很多数据，查询出来仍然回答不了问题。
```

## Q057. histogram 的典型适用场景和不适用场景分别是什么？

histogram 适合“每次事件都有一个数值，并且我们关心这个数值的分布”的场景。最典型的是延迟：HTTP 请求耗时、RPC client call duration、RPC attempt duration、数据库查询耗时、队列等待时间、worker 执行时间、fsync 耗时、checkpoint load 耗时、LLM inference 耗时。延迟天然有长尾，只看平均值会漏掉用户体验，所以 histogram 很合适。

第二类适用场景是大小分布。比如响应体大小、请求体大小、batch size、消息大小、对象存储写入大小、压缩前后字节数。这里的目标不是知道“最后一次大小是多少”，而是知道系统常见大小区间、异常大对象比例、是否需要调整 buffer 和限流策略。

第三类是围绕 SLO 的阈值判断。如果 SLO 是 99% 请求在 800ms 内完成，那么 histogram 应该有接近 0.8s 的 bucket。这样可以直接算“满足阈值的请求比例”。如果 bucket 只有 0.5s 和 2s，中间没有 0.8s，p99 和达标比例都会变得粗糙。

第四类是需要跨实例聚合的分布。Prometheus classic histogram 的 bucket 是累积计数，多个实例可以按 `le` 聚合后，再用 `histogram_quantile()` 估算分位数。summary 的本地 quantile 通常不能这样聚合，所以服务端聚合场景更偏向 histogram。

不适用场景也要说清楚。

当前状态值不适合用 histogram 表达。当前 goroutine 数、当前队列长度、当前连接数、当前内存占用，更自然的是 gauge。除非你明确要观察“队列长度在一段时间内的分布”，否则 histogram 会让语义变混。

离散类别不适合用 histogram。错误码、状态码、region、版本号、tenant tier 是标签或日志字段，不是数值分布。把状态码 200、404、500 当数值放进 bucket，查询结果没有工程意义。

精确审计不适合用 histogram。histogram 丢掉了单个样本，只保留桶计数。要追查某个订单、某次请求、某条异常栈、某次越权访问，需要日志、trace 或审计记录。

高基数拆分不适合直接上 histogram。`user_id`、`trace_id`、`order_id`、`workflow_id`、完整 URL、原始 SQL 作为 label，会把每个 bucket 都乘上一个巨大的维度数。histogram 的每个 label 组合本来就会展开成多条时间序列，高基数会更快把监控系统拖垮。

极热内层循环也要谨慎。Prometheus instrumentation 文档提醒，极高频路径要注意指标更新成本。每秒几十万、几百万次的内部操作，如果每次都记录 histogram，CPU、原子操作、label 查找和导出成本可能比业务逻辑还高。这种地方要先 benchmark，或者采样、按批聚合、只在外层记录。

在 LogServe 里，适合 histogram 的是 SDK 到 control 的端到端提交耗时、workflow step 执行耗时、worker poll 等待、LLM mock 调用耗时、result store 写入耗时。不适合的是 `workflow_id` 维度的 histogram、每条 shared log entry 的逐条热路径 histogram、每个 token 的 histogram，除非实验目标就是测这条热路径的开销。

面试里可以这样回答：

```text
histogram 适合请求耗时、RPC 耗时、队列等待、worker 执行、fsync、payload size 这类数值分布，尤其适合 SLO、p95/p99 和跨实例聚合。不适合当前状态值、离散类别、精确审计、高基数按用户或 trace 拆分，也不适合无脑埋到极热内层循环。我的判断标准是：这个值是不是大量事件样本？我们是否关心分布和长尾？bucket 是否能围绕 SLO 或排障边界设计？如果答案不是，就别急着用 histogram。
```

## Q058. histogram 和相近概念最容易混淆的边界在哪里？

histogram 最容易和 summary、gauge、counter、timer、percentile、heatmap 混在一起。面试里要先把数据语义讲清楚，不要只说“都是用来看延迟的”。

histogram 和 summary 的边界最常见。Prometheus 文档说得很明确：histogram 暴露 bucketed observation counts，quantile 在 Prometheus 服务器侧用 `histogram_quantile()` 从 bucket 估算；summary 在客户端预先计算配置好的 quantile，并把 quantile 直接暴露出去。结果是 histogram 更适合跨实例聚合和事后换窗口、换分位数；summary 的本地 quantile 往往不能简单相加或求平均。把每台机器的 p99 summary 平均一下，不能得到全局 p99。

histogram 和 gauge 的边界在“分布”与“当前值”。当前队列长度是 gauge；一段时间里每次入队等待多久，可以是 histogram。当前内存占用是 gauge；每次请求分配了多少临时字节，如果你真能低成本采集，也可以是 histogram。gauge 是某一时刻的状态，histogram 是一段时间内很多观测事件的分布。

histogram 和 counter 的边界在“值域”。counter 是单调递增计数，回答发生了多少次；histogram 内部确实由多个 bucket counter 组成，但外部语义是观测值分布。比如请求总数可以用 `http_requests_total`；请求耗时分布用 `http_request_duration_seconds_bucket/_sum/_count`。不要为了省指标名，把次数和耗时塞进一个指标。

histogram 和 timer 也容易混。timer 通常是客户端库里的便利封装，自动测量一段代码耗时并记录到 histogram 或 summary。timer 不是一种独立的指标语义。面试中说“用 timer 统计 p99”不够，应该继续说明底层是 histogram 还是 summary，bucket 如何设置，是否能跨实例聚合。

bucket 和 percentile 也不是一回事。bucket 是采集时定义的区间，percentile 是查询时从分布估算的分位点。Prometheus `histogram_quantile()` 在分位数落在某个 bucket 内时要做插值。bucket 太粗，估算误差就大。比如 0.5s 和 2s 之间没有桶，p99 落在这里时你只能得到很粗的近似。

classic histogram 和 native histogram 也要区分。classic histogram 通过多个 `_bucket{le="..."}` 时间序列表达累积 bucket；native histogram 用更紧凑的数据结构表达分布，可以减少传统 bucket 展开的部分问题，但查询、存储、兼容性和版本支持要看具体 Prometheus 版本和客户端支持情况。面试时先把 classic histogram 讲清楚，再补充 native histogram，不要反过来。

HdrHistogram 和 Prometheus histogram 的边界也很重要。HdrHistogram 是一种高动态范围、固定内存、低记录开销的本地数据结构，适合进程内或压测工具里记录大量延迟样本，之后再导出或分析。Prometheus histogram 是监控系统里的指标表示，强调 scrape、label、时间序列和聚合。两者都叫 histogram，但使用位置不同。

最后，heatmap 只是展示方式。Grafana heatmap 可以展示 histogram，也可以展示日志聚合或其他分布数据。不要把“画成热力图”当成 histogram 的定义。

面试里可以这样回答：

```text
histogram 的边界主要在四处：和 summary 的区别是服务端 bucket 聚合 vs 客户端 quantile；和 gauge 的区别是事件分布 vs 当前状态；和 counter 的区别是观测值分布 vs 发生次数；和 percentile 的区别是采集时 bucket vs 查询时分位数估计。Prometheus classic histogram 还要记住 `_bucket/_sum/_count` 和 `le`，`histogram_quantile()` 是估算，不是原始精确 p99。HdrHistogram 是本地高动态范围数据结构，Prometheus histogram 是监控指标模型，不能只因为名字一样就混用。
```

## Q059. histogram 在高并发场景下可能出现哪些隐藏问题？

高并发下，histogram 的问题通常不是“数学定义不懂”，而是更新路径、label 组合、导出链路和查询成本一起放大。低流量测试很正常，上线后才看到 CPU 飙升、指标后端积压或 p99 看起来异常平滑。

第一是 label 基数爆炸。一个 classic histogram 有多个 bucket，每个 label 组合都会生成一组 `_bucket`，再加 `_sum` 和 `_count`。如果有 12 个 bucket，10 万个不同 `path` 或 `workflow_id`，实际 series 数会非常快地上去。高并发服务里，错误地把 full URL、user ID、trace ID、order ID 放进 label，比普通 counter 更伤，因为 histogram 本身就会按 bucket 放大。

第二是热路径更新成本。一次 `Observe()` 可能要取时间、查找 labelset、定位 bucket、更新多个计数器或一个内部结构，还可能记录 exemplar。成熟客户端会优化，但在极热路径里这些成本仍然存在。如果每个请求记录很多个 histogram，或者每个内部循环都记录，CPU 和 cache miss 会变明显。

第三是锁竞争和原子竞争。高并发更新同一个 labelset 的同一个 bucket，可能形成热点。实现上可能用 mutex、atomic、分片计数、线程本地缓存或 lock-free 结构。不同语言、不同客户端差异很大。症状是业务 p99 变差，但 profile 里能看到监控埋点函数、label lookup、atomic add 或锁等待。

第四是内存和 GC 压力。动态 label 值会创建新的 time series 和 aggregator；临时构造 label map、字符串拼接、attributes slice、exemplar 对象，会给 GC 增压。OpenTelemetry API 里有 `Enabled`、绑定 attributes 等设计，就是为了让热路径避免不必要的计算。Go 里要特别看 `B/op` 和 `allocs/op`，不能只看平均耗时。

第五是 bucket 设计带来的精度假象。bucket 太少，p99 估算会粗；bucket 太多，series 和导出成本上升。更糟的是 bucket 没围绕 SLO：SLO 是 800ms，bucket 却只有 500ms 和 2s，最后既不能准确算达标比例，也不能判断 p99 是否贴近阈值。

第六是采集链路被拖慢。应用端更新只是第一段，后面还有 scrape、export、remote write、collector、存储、查询。高并发服务如果暴露大量 histogram series，scrape body 会变大，Prometheus scrape 时间变长，remote write 队列积压，dashboard 查询 `histogram_quantile(sum by (..., le)(rate(...)))` 也会变慢。

第七是并发收集与重置边界。delta temporality、进程重启、SDK collection 周期、exporter 重试，如果处理不好，会出现 bucket count 倒退、窗口重叠、重复导出、缺口。Prometheus classic histogram 要求 bucket 计数单调不降；如果输入违反，`histogram_quantile()` 可能需要修正单调性并给出 annotation，这就是数据源已经不干净的信号。

第八是 coordinated omission。高并发压测或异步记录时，如果系统卡住期间没有持续产生观测，histogram 可能只记录卡住后的少量慢样本，漏掉用户本来会遇到的排队延迟。HdrHistogram 专门讨论过这种问题。服务端监控也要注意：只记录成功完成的请求，会低估超时和丢弃请求带来的尾延迟。

在 LogServe 里，我会重点盯住这些点：不要按 `workflow_id`、`task_id`、`trace_id` 拆 histogram；worker hot loop 里不要给每个内部事件都 `Observe()`；LLM 调用可以记录 call-level latency，但不要把每个 token 或大 payload 塞进 attributes；shared log append 可以有 histogram，但要先 benchmark 埋点开销。

面试里可以这样回答：

```text
高并发下 histogram 的隐藏问题包括：bucket 乘以 label 组合导致 series 爆炸；Observe 热路径有时间读取、label 查找、bucket 定位、原子或锁更新成本；动态 label 和 exemplar 带来内存分配；bucket 太粗导致 p99 假精确，太细导致成本过高；scrape/export/remote write 被大量 bucket 拖慢；delta/cumulative 和重启处理不好会出现窗口错乱；压测还可能有 coordinated omission。排查时我会同时看业务 profile、metrics series 数、scrape duration、remote write backlog、collector queue 和 histogram_quantile 的单调性告警。
```

## Q060. histogram 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

histogram 在正常请求里很好理解：开始计时，结束时记录一个值。但崩溃、重启、超时和重试会让“一个观测值到底代表什么”变复杂。

崩溃时，最大的问题是未完成观测丢失。很多代码只在请求完成时 `Observe(duration)`。如果进程在请求中途崩溃，慢请求可能根本没进入 histogram。这样事故窗口里看起来“没有慢请求”，实际是最慢的请求都死在路上了。要配合 in-flight gauge、timeout counter、crash/restart counter、日志和 trace 才能解释。

重启时，cumulative histogram 会出现计数器重置。Prometheus 通常通过 `rate()` 处理 counter reset，但前提是 scrape 能看到重置前后的点。OpenTelemetry 的 Metrics Data Model 里也区分 cumulative 和 delta temporality，并用 start time 表达窗口边界。若 exporter 或 collector 把 start time、temporality 处理错，重启附近的 bucket rate 会出现尖刺、负值修正或缺口。

超时时，要区分观察者视角。客户端超时 500ms，并不代表服务端 500ms 时停止；服务端可能继续执行并在 2s 后成功写入。客户端 histogram 记录的是用户等待时间或 call duration，服务端 histogram 记录的是 handler 实际执行时间。两个都对，但回答的问题不同。只看服务端耗时，可能漏掉客户端 deadline；只看客户端耗时，可能不知道服务端继续执行造成了副作用。

重试时，要区分 operation-level 和 attempt-level。用户一次点击可能触发三次 RPC attempt。记录方式有三种：

```text
client_call_duration：一次业务调用从开始到最终成功或失败的总耗时。
client_attempt_duration：每次底层 attempt 的耗时。
server_request_duration：服务端看到的每次请求耗时。
```

这三类 histogram 不能混。gRPC OpenTelemetry 指标区分 client call、client attempt 和 server call，就是为了避免把一次用户操作和多次重试混成同一个分布。混用后，QPS、错误率和 p99 都会失真：attempt 多的故障期看起来像流量暴涨；只记录最终成功 call 又会掩盖中间的慢 attempt。

还有一个边界是取消和丢弃。请求被 admission control 丢弃、队列满被拒绝、worker 拿到任务前超时，这些是否进入 latency histogram，要先定义清楚。通常做法是：被系统接受并开始处理的请求记录 duration；入口拒绝或限流用单独 counter；排队等待用 queue wait histogram；端到端用户等待用入口侧 call duration histogram。这样每个指标都回答一个问题。

异步系统里，redelivery 会让同一个业务任务产生多次观测。第一次 worker 崩溃前跑了 10s 没记录，第二次重投后 200ms 成功。如果只看成功 attempt histogram，会以为它很快；如果记录 attempt timeout/cancel counter 和 worker crash counter，才能解释真实成本。LogServe 的 workflow step、actor command、LLM call 都应带 `attempt` 维度或单独指标，但不要把 `workflow_id` 放 label。

面试里可以这样回答：

```text
崩溃时，未完成请求可能没有进入 histogram，所以要配 in-flight、timeout、crash counter 和日志；重启时，cumulative bucket 会 reset，必须用 rate 和 start time/temporality 正确处理；超时时，要区分 client 视角和 server 视角，客户端超时不代表服务端停止；重试时，要分 operation-level call duration、attempt duration 和 server request duration，不能混成一个指标。对 LogServe 这类 workflow 系统，还要区分 step attempt、redelivery、worker crash 和最终 workflow 端到端耗时，否则 histogram 会低估真实尾延迟。
```

## Q061. histogram 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

histogram 的瓶颈可以出现在五个地方。应用 hot path 里主要是 CPU、内存和锁竞争；遥测管道里主要是 I/O、网络和后端存储查询。不能只盯着 `Observe()` 一行代码。

CPU 成本来自时间读取、单位转换、bucket 定位、label/attribute 处理、hash 查找、exemplar 采样、序列化。classic histogram 如果 bucket 边界固定，定位 bucket 可以很快；但 labelset 动态查找、字符串处理和 attributes 构造经常比 bucket 定位更贵。OpenTelemetry Histogram API 也提醒记录值时会携带 attributes，这些 attributes 的处理方式会影响热路径成本。

内存成本来自 bucket 数组、每个 labelset 的 aggregator、SDK reader 缓冲、exemplar reservoir、export batch queue、collector 队列，以及监控后端的 series 索引。classic Prometheus histogram 的每个 bucket 都是时间序列，bucket 越多、label 组合越多，内存越快上去。native histogram 和 exponential histogram 能缓解一部分 bucket 展开问题，但不是免费午餐，仍然要看后端支持和查询成本。

锁竞争来自高频并发更新同一组时间序列。实现如果使用全局锁或每个 histogram 一把锁，高 QPS 下会很快暴露。更好的实现会用 atomic、分片、线程本地累计、无锁结构或批量合并。HdrHistogram 的设计目标之一就是在固定内存和常数时间记录下支持高性能记录，但即使使用高性能结构，外围 label 查找和导出仍然可能成为瓶颈。

I/O 成本来自 scrape、export 和持久化。应用暴露 `/metrics` 时需要生成文本或 protobuf；collector 需要接收、批处理、压缩、写后端；Prometheus 本地还要写 WAL 和 block。大量 histogram series 会让 scrape body 变大，Prometheus scrape duration 增加，remote write 队列积压。

网络成本来自指标传输。高频 scrape、大量 bucket、大量 label、remote write、OTLP export 都会增加网络流量。网络慢或后端限流时，如果客户端队列无界，内存会涨；如果队列有界，就会丢指标；如果同步阻塞，业务请求会受影响。OpenTelemetry 性能规范强调遥测 API 默认不应该阻塞应用，本质就是这条边界。

一般经验是：单个低基数 histogram 的 `Observe()` 成本通常不是问题；大量动态 label、高频热路径、过多 bucket、全量 exemplar、下游慢，才是问题。优化也按这个顺序来：

```text
先砍高基数 label。
再减少不必要的 histogram 和 bucket。
给热路径绑定固定 label，避免每次分配。
把极热内部循环改成外层聚合或采样。
监控 scrape duration、sample count、remote write pending、collector queue。
最后再看 bucket 定位算法和原子操作细节。
```

在 LogServe 里，如果开启 histogram 后 task p99 变差，我不会先怀疑 PromQL，而会先跑 benchmark：workflow submit、worker execute、LLM mock call 在开启和关闭 histogram 时的 `ns/op`、`B/op`、`allocs/op` 差异；再看 Prometheus scrape duration 和 series 数。热路径埋点要用数据证明成本可接受。

面试里可以这样回答：

```text
histogram 的应用侧瓶颈通常是 CPU、内存分配和锁/原子竞争：取时间、处理 label、定位 bucket、更新计数、记录 exemplar。管道侧瓶颈是 I/O、网络和后端存储：scrape body 变大、remote write 积压、collector queue 堆积、查询 histogram_quantile 变慢。优化优先级是控制 label 基数和 bucket 数，避免热路径分配，必要时分片或批量聚合，并保证遥测后端慢时不阻塞业务。
```

## Q062. histogram 的 correctness test、stress test 和 benchmark 应该分别测什么？

这三类测试要分开。correctness test 测语义对不对，stress test 测极端并发和故障下会不会崩，benchmark 测引入 histogram 后到底付出了多少成本。

correctness test 先测 bucket 和计数。给定一组固定输入，应该能验证每个 bucket 的计数、总 count、sum、min/max 是否符合预期。Prometheus classic histogram 的 bucket 是累积的，`le="0.5"` 包含所有小于等于 0.5 的观测，`le="+Inf"` 应该等于 `_count`。边界值要单独测：正好等于 bucket 上界、0、很小值、很大值、非法负值、NaN、Inf。OpenTelemetry Histogram API 期望记录值是非负数，具体 SDK 是否校验要看实现，但测试里要明确策略。

还要测单位和命名。`duration_seconds` 就必须记录秒，不能有的路径记毫秒，有的路径记秒。metric name、unit、description、label key 要稳定。不同服务如果 bucket 边界不同，聚合 p99 会失真，所以跨实例指标要测配置一致性。

聚合 correctness 也很重要。多实例 bucket 聚合后，用 `histogram_quantile()` 查询 p90/p99，结果应该符合预期范围。不能把每个实例的 p99 平均。summary quantile 不能当 histogram bucket 聚合。SLO 阈值如果是 800ms，要测试 bucket 里确实有 0.8 或足够接近的边界。

重启和 temporality 也要测。cumulative histogram 重启后 counter reset，Prometheus `rate()` 应能处理；delta histogram 不应重复窗口或漏窗口；start time 要合理。collector 重试不能重复导出同一窗口导致计数翻倍。

stress test 看高并发和故障。典型用例包括：

```text
N 个 goroutine 并发 Observe，同一 labelset 和不同 labelset 都测。
高 QPS 下 bucket count 不丢、不倒退、不出现数据竞争。
动态 label 被限制或拒绝，不允许无界增长。
scrape/export 与 Observe 并发时不 panic、不长时间阻塞。
collector 慢、remote write 慢、后端不可用时，队列有界，业务不被拖死。
进程重启、worker crash、redelivery、timeout、retry 时，attempt/call 指标语义不混。
极端长尾值进入 +Inf 或最高 bucket，不导致溢出。
```

benchmark 要拆得更细。至少测 `Observe()` 本身的 `ns/op`、`B/op`、`allocs/op`，分别覆盖无 label、固定 label、动态 label、不同 bucket 数、开启 exemplar、关闭 exemplar。Go 项目里用 `go test -bench -benchmem`，并用 `-cpu` 看并发下是否有锁竞争。还要测开启 histogram 前后业务路径 p50/p99 的变化，而不是只测指标库微基准。

管道 benchmark 也不能漏。每秒多少 observations 会产生多少 series、scrape body 多大、Prometheus scrape duration 多少、remote write pending 是否增长、collector CPU 和内存多少、`histogram_quantile()` dashboard 查询耗时多少。这些决定线上能不能用。

在 LogServe 里，我会这样落地：

```text
correctness：固定 workflow step 耗时样本，验证 bucket/count/sum/SLO bucket/query 结果。
stress：并发提交 workflow、worker kill/retry/redelivery、collector 不可用，验证业务不阻塞且指标语义可解释。
benchmark：对 SDK submit、control schedule、worker execute、LLM call 分别测开启 histogram 前后的 ns/op、B/op、allocs/op 和端到端 p99 差异。
```

面试里可以这样回答：

```text
correctness test 测 bucket 边界、累积计数、sum/count、单位、label、跨实例聚合、SLO bucket、重启 reset 和 delta/cumulative 语义；stress test 测高并发 Observe、scrape/export 并发、动态 label 限制、后端慢、进程重启、timeout、retry、redelivery 下不丢语义不阻塞；benchmark 测 Observe 的 ns/op、B/op、allocs/op，按 bucket 数、label 数、exemplar、并发度拆开，再测开启 histogram 对业务 p99、scrape size、remote write 和查询耗时的影响。
```

## Q063. 如果要求从零实现一个简化版 histogram，你会先定义哪些不变量？

从零实现 histogram，我会先写不变量，而不是先写 bucket 数组。histogram 一旦进了监控和 SLO，后面 dashboard、alert、容量评估都会依赖它；不变量没定清楚，数据看起来有数，实际不可解释。

第一，指标身份不变量。metric name、unit、description、instrument kind 要稳定。`http_request_duration_seconds` 的单位就是秒，不能中途改成毫秒。不同语义不要复用同一个名字。请求总耗时、下游 attempt 耗时、队列等待时间，要用不同 histogram。

第二，输入值不变量。记录值必须是有限数值，并且对延迟和大小这类指标要求非负。NaN、Inf、负数的处理策略要明确：拒绝、丢弃、计入错误 counter，还是按 SDK 规则处理。不能让非法值污染 sum 和 bucket。

第三，bucket 边界不变量。显式 bucket 必须严格递增。classic Prometheus 语义下需要有 `+Inf` bucket，且 bucket 计数对外是累积的：较大 `le` 的计数不小于较小 `le`。如果内部用非累积 bucket，也要在导出时转换正确。

第四，count 和 sum 不变量。每成功记录一个样本，总 count 加一；sum 增加该样本值；样本只进入一个内部 bucket，但导出为累积 bucket 时会体现在所有更大上界中。`+Inf` bucket 应等于 count。任何并发路径都不能让 count、sum、bucket 之间不一致。

第五，时间窗口不变量。每个数据点要属于明确窗口。cumulative 模式下，start time 表示从什么时候累计；进程重启或显式 reset 要改变 start time。delta 模式下，相邻窗口不能重叠，也不能重复导出同一段。没有这个不变量，rate 和 p99 查询会在重启附近乱跳。

第六，label 不变量。label key 集合要稳定，label value 要低基数、可聚合。实现上可以限制 label key 白名单，拒绝 `trace_id`、`user_id`、`workflow_id` 这类无界字段进入 label。高基数字段应进入日志、trace attributes 或 exemplar。

第七，并发不变量。`Observe()` 必须并发安全。并发记录、并发 scrape/export、进程关闭 flush 不能出现数据竞争、panic、负计数或非单调 bucket。遥测后端慢不能无限阻塞业务路径。

第八，查询误差不变量。histogram 给出的 p99 是估算，不是原始精确值。误差上界主要由 bucket 宽度决定。bucket 设计要让 SLO 附近的误差可接受。实现文档要说明这一点，避免面试官追问时把估算说成精确值。

第九，重启和缺失不变量。进程重启、collector 故障、export queue 满时，系统要暴露 dropped metrics、export errors、last successful export time 或类似指标。没有数据不应该被误解释为“系统很健康”。

第十，观测层边界不变量。histogram 不参与业务状态判断。它可以报告 `workflow_step_duration_seconds`，但不能决定 step 是否完成；可以报告 `shared_log_append_duration_seconds`，但不能替代 log offset 和 fsync 结果。

一个简化接口可以长这样：

```go
type Histogram struct {
    Name    string
    Unit    string
    Buckets []float64
}

func NewHistogram(name, unit string, buckets []float64) (*Histogram, error)
func (h *Histogram) Observe(value float64, labels Labels)
func (h *Histogram) Snapshot() HistogramSnapshot
```

但接口不是重点，重点是不变量能被测试覆盖。实现前我会先写固定样本、边界值、并发、重启、导出格式和非法 label 的测试。

面试里可以这样回答：

```text
从零实现 histogram，我会先定义这些不变量：metric name/unit/语义稳定；输入值有限且非负；bucket 边界严格递增并有 +Inf；count、sum、bucket 计数一致；classic 导出时 bucket 单调累积；cumulative/delta 时间窗口清楚；label 低基数且 key 稳定；Observe 并发安全；后端慢不阻塞业务；p99 是 bucket 估算而非精确值；重启和丢弃要可观测；histogram 不参与业务状态判断。然后再写 bucket 查找和导出代码。
```

## Q064. histogram 的常见误用是什么，误用后通常会产生什么线上症状？

histogram 的误用大多来自两个方向：要么把它当成“万能延迟指标”，要么把它当成“精确样本数据库”。这两种都会让监控看起来很丰富，事故时却回答不了问题。

第一，bucket 乱设。默认 bucket 没贴近业务 SLO，是最常见问题。比如请求 SLO 是 99% 小于 800ms，bucket 却是 0.5s、1s、2.5s。你可以估算 p99，但没法准确回答“有多少请求小于 800ms”。线上症状是 SLO dashboard 模糊，p99 在一个大 bucket 里跳来跳去，报警阈值很难调。

第二，单位混乱。指标名叫 `_seconds`，某些路径却记录毫秒；或者一部分服务记录秒，另一部分服务记录毫秒。症状是 p99 突然差 1000 倍，bucket 几乎全落在 `+Inf`，或者所有请求都看起来小到离谱。这类问题很低级，但真实线上很常见。

第三，把高基数字段放进 label。`trace_id`、`request_id`、`user_id`、`order_id`、`workflow_id`、完整 URL 都不适合作 histogram label。症状是 series 暴涨，Prometheus 内存和磁盘上升，scrape 超时，remote write 积压，查询和告警变慢。

第四，把每台机器的 p99 平均。分位数不能这样聚合。正确做法是对 classic histogram 先按 `le` 聚合 bucket，再用 `histogram_quantile()`。误用后的症状是全局 p99 和用户体感不一致：一台低流量机器的高 p99 会被过度放大，或者高流量机器的问题被平均掉。

第五，拿 summary 的 quantile 当 histogram 聚合。summary 暴露的本地 quantile 通常不能跨实例相加、求平均或重新计算窗口。症状是 dashboard 上有一个“全局 p99”，但它没有严格语义。

第六，只记录成功请求。错误、超时、取消、限流、队列丢弃都不进 histogram，结果故障期 p99 反而看起来更好，因为慢请求都失败了。正确做法是明确定义：端到端 call duration、attempt duration、server handling duration、queue wait、rejection counter 各自记录什么。

第七，把当前状态塞进 histogram。当前队列长度、当前连接数、当前 goroutine 数如果用 histogram 记录，很容易把“状态采样分布”和“事件延迟分布”混起来。大多数时候这些应该是 gauge。

第八，把 histogram 当精确审计。histogram 不能告诉你是哪一次请求慢、哪个用户受影响、哪条 SQL 卡住。它只能提示分布异常。需要单请求证据时，要用 trace、日志、exemplar 或采样事件。

第九，在极热路径无脑记录。比如每处理一条 shared log 内部循环、每个 token、每个小对象都 `Observe()`，最后业务 p99 被观测代码拉高。症状是 profile 里监控库占比上升，关闭指标后性能恢复。

第十，忽视 `histogram_quantile()` 的输入要求。classic histogram 需要 `le`，bucket count 应该单调不降。查询时忘记 `sum by (le, ...)`，或者导出数据不单调，就会得到 NaN、奇怪跳变或 Prometheus 的单调性修正 annotation。

在 LogServe 里，最容易犯的错是按 `workflow_id` 拆分所有 duration histogram，或者把 workflow 端到端耗时、step attempt 耗时、LLM call 耗时放进同一个 metric。前者会打爆指标系统，后者会让 p99 没有解释力。

面试里可以这样回答：

```text
histogram 常见误用包括：bucket 不围绕 SLO，单位秒/毫秒混乱，高基数 label，平均各实例 p99，把 summary quantile 当全局 p99，只记录成功请求，把当前状态当分布，把 histogram 当审计日志，极热路径过度 Observe，以及 PromQL 聚合忘记 `le`。线上症状是 p99 假精确、SLO 计算错误、Prometheus series 爆炸、scrape 和查询变慢、故障期延迟反而变好看、`histogram_quantile()` 返回 NaN 或出现单调性修正。解决办法是先定义语义、单位、bucket、label 和查询，再写埋点。
```

## Q065. histogram 在单机和分布式环境中的语义有什么差异？

单机环境里的 histogram 语义比较直接：一个进程记录一批观测值，本地聚合成 bucket、count、sum。你可以把它理解成“这个进程在这个时间窗口里看到的分布”。如果用 HdrHistogram 这类本地结构，还可以在压测或进程内分析里保留高分辨率分布。这里的问题主要是并发安全、记录成本、进程重启和本地窗口。

分布式环境里，histogram 的核心变成“能否正确聚合”。每个实例都在记录自己的本地分布，但值班时通常关心全局服务、某个 region、某个版本、某个 endpoint 的分布。要想聚合，必须满足几个条件：

```text
metric name 一致。
unit 一致。
bucket 边界一致，或者使用兼容的 native/exponential histogram。
label 语义一致。
时间窗口和 temporality 可解释。
重启 reset 能被查询正确处理。
```

classic Prometheus histogram 的聚合方式，是对 bucket rate 按 `le` 和目标维度求和，再用 `histogram_quantile()`。不能先在每个实例算 p99，再对 p99 做平均。因为分位数不是可加量，实例流量不同、分布不同，平均分位数没有全局语义。

分布式环境还要处理实例上下线和 scrape 缺口。某个实例重启后 bucket counter reset；某个实例短暂不可 scrape；某个版本只跑了一部分流量。这些都会影响全局分布。查询时要用合适窗口，dashboard 要保留 `instance`、`version`、`region` 等 drill-down 维度，不能只看一个总 p99。

单机里，histogram 和 trace/log 的关联可以靠进程内上下文；分布式里，最好用 exemplar 或 trace/log 字段把某些慢样本链接回 trace。OpenTelemetry Metrics Data Model 里 exemplar 可以携带 `trace_id` 和 `span_id`，这正是连接“聚合分布”和“单次请求证据”的桥。但 exemplar 只是样本链接，不是把每个 trace ID 都放进 metric label。

分布式环境中的语义还要区分层级。入口服务的 `http.server.duration` 是用户入口视角；下游服务的 `rpc.server.duration` 是局部处理视角；客户端的 `rpc.client.attempt.duration` 是一次 attempt 视角；workflow 端到端 duration 是业务操作视角。它们都可以用 histogram，但不能混成一个指标，也不能直接比较 p99 后就说谁慢。要看它们的观察边界。

LogServe 是个很好的例子。即使跑在一台机器上，SDK、control、worker、actor、Python executor、LLM mock 的 histogram 也应该按组件和语义拆开。`workflow_end_to_end_duration_seconds`、`workflow_step_attempt_duration_seconds`、`llm_call_duration_seconds`、`shared_log_append_duration_seconds` 可以各自有 histogram。它们可以按 role、operation、status、version 聚合，但不要按 `workflow_id` 聚合。shared log 负责正确性，histogram 负责看延迟分布。

面试里可以这样回答：

```text
单机 histogram 表示一个进程在一个窗口内看到的本地分布，重点是并发安全、记录成本和重启 reset。分布式 histogram 表示多个实例的可聚合分布，重点是 name、unit、bucket、label、temporality 一致，并且用 `sum by (..., le)(rate(...))` 后再算 quantile，不能平均各实例 p99。分布式还要处理实例上下线、scrape 缺口、counter reset、版本和 region 维度。trace_id 可以通过 exemplar 链接慢样本，但不能作为 label。LogServe 里即使单机多进程，也要按分布式语义统一 bucket 和 label。
```

## Q066. p99 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

p99 的核心目标，是让我们看到尾部用户体验，而不是被平均值和中位数安慰。它回答的问题很具体：在某个时间窗口、某组筛选条件下，99% 的请求耗时不超过多少，剩下最慢的 1% 到底有多慢。这个指标特别适合暴露排队、锁竞争、连接池耗尽、GC 暂停、下游长尾、热点分片、磁盘抖动、网络重传这类问题。它们一开始经常只影响少数请求，p50 和平均值看起来还很正常。

先把语义说清楚。p99 不是最大值，也不是“99% 请求都正好这么慢”。它是分布上的一个位置。假设某 5 分钟窗口里有 100 万次请求，p99 为 800ms，大致意思是有 99 万次请求小于等于 800ms，剩下 1 万次比 800ms 更慢。对高 QPS 服务来说，这 1% 不是小数目。每分钟 100 万请求，1% 就是一万次用户级慢体验。

如果非要在“正确性、性能、安全性、可维护性”里选，p99 首先服务于性能和用户体验，但它本身不是性能优化手段。它不会让系统变快，只是把尾部变慢这件事暴露出来。真正的优化可能是减少锁竞争、隔离慢下游、调连接池、限流、削峰、减少 fan-out、优化 fsync 或 cache 策略。

p99 同时也服务可维护性。值班和排障需要一个信号告诉你：系统是不是只有少数用户慢，慢是不是集中在某个 route、region、tenant、版本或下游。Google SRE 讲监控时把 latency 放在四个黄金信号里，也强调要区分成功请求和失败请求的延迟，因为快速失败和慢成功混在一起会误导判断。p99 在这里是症状指标，不是根因指标。它告诉你“尾部用户正在受影响”，但不会自动告诉你“为什么”。

它不直接解决正确性。一个请求 p99 很好，不代表事务隔离正确、幂等正确、WAL 恢复正确、actor command 顺序正确。反过来，p99 很差也不一定有数据错误，可能只是某个下游慢或资源排队。正确性要靠状态机不变量、事务、幂等键、序列号、日志重放和测试来证明。

它也不是安全机制。p99 上升可能暗示攻击流量、爬虫、暴力请求或资源耗尽，但 p99 本身不做认证、授权、审计和入侵检测。安全事件要看访问日志、审计记录、身份系统、速率限制和异常行为指标。把安全判断压到 p99 上，会漏掉快速失败型攻击，也会把普通性能事故误判成安全事件。

p99 和 SLO 的关系要小心。很多人说“我们的 SLO 是 p99 小于 500ms”，这在口语上能理解，但更严谨的写法通常是 good event 比例：例如过去 30 天内，99% 的有效请求在 500ms 内成功返回。这样更适合 error budget 和 burn rate 计算。p99 是读分布的一个方式，SLO 是对用户承诺和预算消耗的定义。两者经常一起出现，但不是同一个东西。

在 LogServe 里，p99 的价值是观察 workflow 端到端、step attempt、worker queue wait、LLM call、shared log append 这些路径的尾部。比如平均 workflow 耗时 200ms，但 p99 是 3s，说明大多数任务没问题，少数任务被排队、重试、Python executor、LLM mock 或 shared log 写入拖住了。它能帮助定位性能症状，但不能证明 workflow 状态恢复正确。恢复正确性仍然要看 shared log、checkpoint、idempotency 和 replay 语义。

面试里可以这样回答：

```text
p99 的核心目标是暴露尾部用户体验：在指定窗口和筛选条件下，99% 请求不超过哪个延迟，剩下 1% 有多慢。它主要服务性能观测和可维护性，能让我们发现平均值、p50 看不到的排队、锁竞争、连接池耗尽、GC、下游长尾和热点问题。它不是业务正确性证明，也不是安全机制；p99 变好不代表状态机、事务、幂等一定正确，p99 变差也不一定是安全事件。工程上我会把 p99 当症状指标，用 histogram/SLO 判断影响范围，再用 trace、日志、profile 和资源指标找原因。
```

## Q067. p99 的典型适用场景和不适用场景分别是什么？

p99 适合用在高流量、用户体验对尾延迟敏感、并且系统有排队或 fan-out 风险的路径。最典型的是在线 API、RPC 服务、网关、搜索、推荐、支付、登录、对象存储读写、队列消费、数据库访问、缓存访问、LLM serving 和 workflow 调度。只要用户或上游调用方会被少数慢请求影响，p99 就比平均值更有用。

第一个典型场景是交互式请求。用户点击按钮、页面加载、提交订单、发起一次 RPC 调用，体验由自己那次请求决定，不由系统平均值决定。100 个请求里 99 个 50ms、1 个 5s，平均值仍然可能看起来不坏，但那个等 5 秒的用户已经感知到问题。p99 能把这类尾部显出来。

第二个场景是容量和饱和早期信号。系统接近容量上限时，往往不是所有请求一起变慢，而是少数请求先开始排队。连接池快满、线程池快满、锁竞争加剧、GC 周期变长、磁盘 flush 抖动，都会先抬高 p95/p99。等 p50 都明显变差时，事故通常已经更严重。

第三个场景是 fan-out 系统。一个入口请求如果要调用 10 个下游，整体延迟通常由最慢的那个分支决定。下游每个服务单独看 p99 还可以，组合后入口 p99 可能明显变差。微服务、workflow DAG、批量 RPC、LLM 工具调用、分片查询都属于这类。

第四个场景是 SLO 和发布守护。灰度发布后，错误率可能没变，但 p99 变差，说明新版本让少数请求变慢。p99 可以作为发布观察指标之一。更严谨的 SLO 仍然建议用“多少比例请求低于阈值”的 good event 方式，但 p99 是 dashboard 和排障里很直观的尾部视图。

第五个场景是对比优化效果。比如优化锁、缓存、批处理、连接池、fsync 策略，平均耗时可能变化不大，但 p99 大幅下降。这说明优化主要改善了尾部，而不是典型路径。面试里提这个点，会显得你真的理解性能问题，而不是只会看吞吐。

不适用场景同样重要。

低流量接口不适合过度盯短窗口 p99。每分钟只有几十个请求时，p99 基本由一两个样本决定，抖动很大。此时更适合看更长窗口、固定阈值下的 good event 比例、慢请求列表、超时数、最大值或人工 trace。对后台管理接口、低频运维操作，短窗口 p99 经常只是噪声。

批处理和离线任务也不总是适合 p99。一个每天跑一次的任务，没有 p99 的统计意义。每天跑 100 万个小 job，则可以看 job duration 的 p95/p99。关键是样本量和业务语义，不是所有耗时都应该贴一个 p99。

p99 不适合做单次事故取证。它告诉你尾部变慢，但不知道是哪条请求、哪条 SQL、哪个用户、哪个 trace。要找单次原因，需要 trace、日志、profile、慢查询、事件记录。p99 是入口，不是证据链本身。

p99 不适合用来证明正确性。比如 LogServe 的 shared log append p99 很低，不代表崩溃恢复、重复提交、actor command 顺序一定正确。正确性要看写入协议、fsync 边界、幂等键、replay 测试和 crash test。

p99 也不适合脱离分组条件使用。全站一个 p99 往往把不同 endpoint、region、tenant、状态码和版本混在一起。上传接口和读缓存接口放在一起算 p99，结果没有解释力。p99 必须带上合理维度：route template、operation、status class、region、service version、caller/callee，且这些维度要低基数。

在 LogServe 里，我会看这些 p99：`workflow_end_to_end_duration`、`workflow_step_attempt_duration`、`worker_queue_wait`、`llm_call_duration`、`shared_log_append_duration`。不适合看的是“所有 workflow 混在一起的唯一 p99”，也不适合按 `workflow_id` 拆 p99。前者太粗，后者高基数。更好的拆法是 workflow type、step type、status、worker role、版本。

面试里可以这样回答：

```text
p99 适合高 QPS、交互式、对尾延迟敏感的路径，比如 API、RPC、网关、支付、缓存、数据库、队列消费、workflow step 和 LLM serving。它适合发现饱和早期信号、fan-out 长尾、发布后尾部回退和 SLO 风险。不适合低流量短窗口接口、每天只跑一次的离线任务、单次事故取证、业务正确性证明，也不适合把不同语义的接口混成一个全局 p99。低流量场景我会看更长窗口、阈值达标比例、超时数和慢请求明细；高流量核心路径才严肃看 p99/p999。
```

## Q068. p99 和相近概念最容易混淆的边界在哪里？

p99 最容易和平均值、最大值、p95、p999、tail latency、SLO、histogram、summary、trace 慢样本混在一起。边界不清，dashboard 会很漂亮，排障时却会误导人。

先看 p99 和平均值。平均值回答“总耗时除以请求数是多少”，适合估算成本、容量和总体工作量。p99 回答“尾部 1% 的边界在哪里”，适合看用户体验和饱和信号。平均值可以很低，p99 很高；也可能大量快速失败导致平均值下降，但用户体验变差。面试里不要说平均值没用，它有用，只是不能代表尾部。

p99 和最大值也不一样。最大值对单个极端异常非常敏感，一次 GC 暂停、一次网络抖动、一个探针异常就可能把最大值拉爆。p99 忽略最极端的 1%，更稳定，适合常规 SLO 和趋势观察。但这也意味着 p99 看不到最坏的 1%。如果业务对任何单次极慢都敏感，比如交易撮合、实时控制、少量高价值请求，仍然要配合 max、超时数和慢请求日志。

p99 和 p95、p999 的边界在样本量和敏感度。p95 看 5% 慢请求，更稳定，适合中等流量和较粗的用户体验判断。p99 更敏感，适合高流量服务和尾部排障。p999 看 0.1% 极端尾部，需要更高请求量，否则统计噪声很大。每分钟 100 个请求时谈 p999 很虚；每分钟 100 万请求时，p999 对基础设施服务可能很有意义。

p99 和 tail latency 也不是完全同义。tail latency 是尾部延迟这个现象，p95、p99、p999、max 都是观察尾部的不同切面。p99 只是其中一个常用点。真正分析 tail latency 时，还要看分布形状、慢请求占比、超时、错误、重试、队列等待、下游 span，而不是只盯一个数字。

p99 和 SLO 的边界最容易被口语化说法弄混。可以说“我们关注 p99 小于 500ms”，但严谨的 SLO 往往定义为“在 30 天窗口内，99% 的合格请求在 500ms 内成功返回”。这两者接近，但不等价。p99 是一个分位数值，SLO 是目标和预算规则。SLO 还要定义 good event、窗口、排除条件、错误处理、聚合维度和 burn rate。

p99 和 histogram 也不是同一层东西。histogram 是采集和聚合分布的一种指标类型，p99 是从分布里计算或估算出来的分位数。Prometheus classic histogram 用 bucket 近似分布，然后 `histogram_quantile()` 估算 p99。bucket 太粗，p99 就是粗估。不要把估算值说成原始精确值。

p99 和 summary 的边界在聚合。summary 可以在客户端给出本地 p99，但多个实例的 p99 不能简单平均成全局 p99。Prometheus 文档对这一点讲得很清楚：summary 的 quantile 不能像 histogram bucket 那样跨实例聚合。线上服务如果要全局 p99，通常优先用 histogram bucket 聚合，或者用原始样本/日志离线计算。

p99 和 trace 慢样本也不同。trace 里的一个慢请求可以解释“为什么慢”，但不能代表整体 p99。trace 还可能被采样。p99 应该来自全量或近似全量 metrics；trace 用来解释 p99 升高背后的路径。把采样 trace 当成 p99 来源，会被采样策略带偏。

还有一个边界是 client-side p99 和 server-side p99。客户端看到的延迟包含排队、DNS、连接池、重试、网络、服务端处理和响应传输；服务端看到的延迟只覆盖自己开始处理到响应写出的区间。两边都对，但回答的问题不同。只看 server p99，可能漏掉客户端连接池等待；只看 client p99，又不一定知道服务端内部哪段慢。

在 LogServe 里，`workflow_end_to_end` 的 p99、`step_attempt` 的 p99、`shared_log_append` 的 p99 不是一个东西。前者是用户或 SDK 视角，后者是局部机制视角。workflow p99 升高时，应该用 step、worker queue、LLM call、shared log append 的 histogram 和 trace 去拆分，而不是直接说 shared log 慢。

面试里可以这样回答：

```text
p99 是分位数，不是平均值、不是最大值、不是 SLO 本身，也不是 histogram 本身。平均值看总体成本，p99 看尾部边界；最大值看极端个例，p99 更稳定但会忽略最慢 1%；p95 更稳，p999 更敏感但需要更大样本；tail latency 是现象，p99 是观察点；histogram 是采集分布的数据结构，p99 是从分布估算出的结果；summary 的本地 p99 不能跨实例平均；trace 慢样本用于解释 p99，不应用采样 trace 直接计算 p99。client p99 和 server p99 也要分开看，因为观察边界不同。
```

## Q069. p99 在高并发场景下可能出现哪些隐藏问题？

高并发场景下，p99 看起来只是一个数字，背后却有很多坑。最常见的问题不是公式，而是样本、窗口、聚合、重试、采样和观测边界。

第一，样本量和窗口会改变解释。高 QPS 服务里，1 分钟 p99 可能已经有足够样本；低 QPS 接口里，1 分钟 p99 可能只有几十个样本，抖动很大。高并发下还要注意窗口太长会抹平事故，窗口太短会放大瞬时抖动。值班 dashboard 通常要同时看短窗口和长窗口：短窗口发现快速事故，长窗口确认是不是持续影响。

第二，聚合方式容易错。不能把每个实例、每个 pod、每个 shard 的 p99 平均后当全局 p99。高并发系统里流量分布通常不均匀，一个热点实例可能承载大部分请求。平均 p99 会掩盖热点，也可能放大小流量实例的噪声。正确做法是用 histogram bucket 按 `le` 和目标维度聚合后再算 quantile，或者从原始样本计算全局分位数。

第三，高基数维度会让 p99 失控。大家想看“每个用户的 p99”“每个 workflow_id 的 p99”“每个 trace_id 的 p99”，结果指标系统被 label 打爆。p99 需要维度，但维度要低基数、可聚合、能指导动作。route template、region、status class、version、operation、tenant tier 通常有价值；user_id、trace_id、order_id、full URL 通常应该去日志和 trace。

第四，重试和 hedging 会污染语义。一次用户调用可能触发多次 attempt。你看的是 user call p99，还是 attempt p99，还是 server request p99？三者差别很大。故障时重试会增加下游流量，attempt p99 可能不高，但 call p99 很高；或者 server p99 看起来高，是因为客户端 hedging 导致重复请求堆积。gRPC OpenTelemetry 指标区分 client call、client attempt 和 server call，就是为了解决这个边界。

第五，只统计成功请求会低估尾部。高并发事故中，慢请求可能超时、取消、被限流或被丢弃。如果这些不进延迟统计，p99 可能反而变好看。Google SRE 监控章节也提醒，失败请求的延迟要和成功请求区分，因为快速 500 和慢错误都需要解释。我的做法是成功延迟、失败延迟、timeout、cancelled、rejected、in-flight 都要有自己的指标。

第六，coordinated omission 会让压测 p99 偏低。压测客户端如果“发一个请求，等响应，再发下一个”，系统卡住期间就不会继续按真实到达率产生请求，排队的慢请求没有被记录。HdrHistogram 项目一直强调这个测量偏差。高并发服务做压测时，应该按固定到达率或目标到达过程发请求，并记录计划发送时间到完成时间的延迟，同时报告吞吐、完成率、错误率和超时。

第七，bucket 精度会制造假象。Prometheus histogram 的 p99 是从 bucket 估算的。高并发下样本很多，但如果 bucket 设计很粗，估算仍然粗。SLO 是 800ms，bucket 只有 500ms 和 2s，p99 再稳定也不能精确回答是否贴近 800ms。高并发不是精度的替代品，bucket 仍然要围绕 SLO 和排障边界设计。

第八，观测系统本身可能成为瓶颈。高并发服务如果为每个 route、status、tenant、version 都记录多个 histogram，series 数、scrape body、remote write、dashboard 查询都会增加。最终可能出现业务还没挂，监控先慢了；或者监控开销抬高了业务 p99。排障时要同时看 telemetry pipeline 的 scrape duration、sample count、remote write backlog、collector queue、dropped samples。

第九，排队位置会被隐藏。服务端 handler p99 可能不高，但请求在负载均衡、连接池、线程池、队列、worker poll 前已经等了很久。高并发下这种情况很常见。要区分端到端 latency、queue wait、service time、downstream time、serialization time。只看一个 server duration p99，容易把排队问题漏掉。

第十，多租户和热点会把全局 p99 搞得很难解释。全局 p99 升高，可能是某个大租户慢、某个 region 慢、某个 route 慢、某个 partition 慢。高并发下总体流量很大，少数热点也能占据 p99 的尾部。需要保留低基数但有动作价值的切分维度，并配合 trace exemplar 找代表性慢请求。

在 LogServe 里，高并发下最容易看错的是 workflow p99。比如 worker queue wait 升高，但 step execution 本身不慢；或者 LLM call attempt 不慢，但重试导致 workflow end-to-end p99 升高；或者 shared log append p99 偶发抖动，只有某类 workflow 被影响。正确做法是把端到端、queue wait、attempt、append、result store 分开，用同一时间窗口观察，再从慢 bucket 跳到 trace 和日志。

面试里可以这样回答：

```text
高并发下 p99 的隐藏问题包括：短窗口和样本量导致抖动；把实例 p99 平均成全局 p99；高基数维度打爆指标；重试和 hedging 混淆 call/attempt/server 语义；只统计成功请求低估超时和取消；压测存在 coordinated omission；histogram bucket 太粗导致假精确；监控管道自身成为瓶颈；server p99 漏掉队列和连接池等待；全局 p99 被某个租户、region、route 或热点分片主导。我的处理方式是用 histogram 正确聚合，明确观察边界，区分 success/error/timeout/retry，按低基数维度 drill down，并用 trace/log/profile 找根因。
```

## 参考资料

- OpenTelemetry Docs, [Observability primer](https://opentelemetry.io/docs/concepts/observability-primer/)
- OpenTelemetry Docs, [Signals](https://opentelemetry.io/docs/concepts/signals/)
- OpenTelemetry Docs, [Logs](https://opentelemetry.io/docs/concepts/signals/logs/)
- OpenTelemetry Docs, [Traces](https://opentelemetry.io/docs/concepts/signals/traces/)
- OpenTelemetry Specification, [Trace API](https://opentelemetry.io/docs/specs/otel/trace/api/)
- OpenTelemetry Docs, [Context propagation](https://opentelemetry.io/docs/concepts/context-propagation/)
- W3C Recommendation, [Trace Context](https://www.w3.org/TR/trace-context/)
- Prometheus Docs, [Metric types](https://prometheus.io/docs/concepts/metric_types/)
- Prometheus Docs, [Histograms and summaries](https://prometheus.io/docs/practices/histograms/)
- Prometheus Docs, [histogram_quantile()](https://prometheus.io/docs/prometheus/latest/querying/functions/#histogram_quantile)
- Prometheus Docs, [Native Histograms](https://prometheus.io/docs/specs/native_histograms/)
- OpenTelemetry Specification, [Metrics API - Histogram](https://opentelemetry.io/docs/specs/otel/metrics/api/#histogram)
- OpenTelemetry Specification, [Metrics Data Model](https://opentelemetry.io/docs/specs/otel/metrics/data-model/)
- OpenTelemetry Specification, [Metrics SDK - Histogram aggregation](https://opentelemetry.io/docs/specs/otel/metrics/sdk/#histogram-aggregation)
- Google SRE Book, [Monitoring Distributed Systems](https://sre.google/sre-book/monitoring-distributed-systems/)
- Google SRE Book, [Service Level Objectives](https://sre.google/sre-book/service-level-objectives/)
- Google SRE Workbook, [Implementing SLOs](https://sre.google/workbook/implementing-slos/)
- OWASP Cheat Sheet Series, [Logging Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Logging_Cheat_Sheet.html)
- HdrHistogram, [HdrHistogram README](https://github.com/HdrHistogram/HdrHistogram)
- Brendan Gregg, [The USE Method](https://www.brendangregg.com/usemethod.html)
- Grafana Labs, [The RED Method: How to Instrument Your Services](https://grafana.com/blog/the-red-method-how-to-instrument-your-services/)
- Prometheus Docs, [Instrumentation](https://prometheus.io/docs/practices/instrumentation/)
- Prometheus Docs, [Metric and label naming](https://prometheus.io/docs/practices/naming/)
- Amazon SQS Developer Guide, [Available CloudWatch metrics for Amazon SQS](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/sqs-available-cloudwatch-metrics.html)
- RabbitMQ Docs, [Monitoring](https://www.rabbitmq.com/docs/monitoring)
- Redis Docs, [INFO](https://redis.io/docs/latest/commands/info/)
- gRPC Docs, [OpenTelemetry Metrics](https://grpc.io/docs/guides/opentelemetry-metrics/)
- OpenTelemetry Docs, [Sampling](https://opentelemetry.io/docs/concepts/sampling/)
- OpenTelemetry Collector Contrib, [Tail Sampling Processor](https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/processor/tailsamplingprocessor)
- Google SRE Workbook, [Alerting on SLOs](https://sre.google/workbook/alerting-on-slos/)
- Prometheus Docs, [Alerting](https://prometheus.io/docs/practices/alerting/)
- Prometheus Docs, [Alerting rules](https://prometheus.io/docs/prometheus/latest/configuration/alerting_rules/)
- Prometheus Docs, [Consoles and dashboards](https://prometheus.io/docs/practices/consoles/)
- Grafana Docs, [Dashboard best practices](https://grafana.com/docs/grafana/latest/visualizations/dashboards/build-dashboards/best-practices/)
- Go Standard Library, [runtime/pprof](https://pkg.go.dev/runtime/pprof)
- Go Standard Library, [net/http/pprof](https://pkg.go.dev/net/http/pprof)
- Linux man-pages, [fsync(2)](https://man7.org/linux/man-pages/man2/fsync.2.html)
- Linux man-pages, [iostat(1)](https://man7.org/linux/man-pages/man1/iostat.1.html)
- gRPC Docs, [Deadlines](https://grpc.io/docs/guides/deadlines/)
- gRPC Docs, [Retry](https://grpc.io/docs/guides/retry/)
- OpenTelemetry Docs, [Baggage](https://opentelemetry.io/docs/concepts/signals/baggage/)
- OpenTelemetry Docs, [Resources](https://opentelemetry.io/docs/concepts/resources/)
- OpenTelemetry Specification, [Performance and Blocking of OpenTelemetry API](https://opentelemetry.io/docs/specs/otel/performance/)
- Prometheus Docs, [Storage](https://prometheus.io/docs/prometheus/latest/storage/)
- Prometheus Docs, [Remote write tuning](https://prometheus.io/docs/practices/remote_write/)
- Grafana Loki Docs, [Log retention](https://grafana.com/docs/loki/latest/operations/storage/retention/)
- IETF RFC 5905, [Network Time Protocol Version 4](https://www.rfc-editor.org/rfc/rfc5905)
- IETF RFC 3339, [Date and Time on the Internet: Timestamps](https://www.rfc-editor.org/rfc/rfc3339)
- Google Research, [Dapper, a Large-Scale Distributed Systems Tracing Infrastructure](https://research.google/pubs/dapper-a-large-scale-distributed-systems-tracing-infrastructure/)
- OpenTelemetry Specification, [Logs Data Model](https://opentelemetry.io/docs/specs/otel/logs/data-model/)
- OpenTelemetry Specification, [Trace Context in non-OTLP Log Formats](https://opentelemetry.io/docs/specs/otel/compatibility/logging_trace_context/)
- Go Standard Library, [log/slog](https://pkg.go.dev/log/slog)
- Go Blog, [Structured Logging with slog](https://go.dev/blog/slog)
- Uber Go, [zap README](https://github.com/uber-go/zap)
- rs/zerolog, [zerolog README](https://github.com/rs/zerolog)
- Rust tracing crate, [tracing](https://docs.rs/tracing/latest/tracing/)
- Rust tracing-subscriber crate, [fmt](https://docs.rs/tracing-subscriber/latest/tracing_subscriber/fmt/)
