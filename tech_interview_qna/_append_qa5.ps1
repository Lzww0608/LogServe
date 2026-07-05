# One-shot helper for appending the Q017-Q020 system-design interview batch.
# Run only when the target markdown file does not already contain this batch; AppendAllText does not deduplicate.
$file = "F:\Code\Go-Programming\LogServe\tech_interview_qna\52_system_design_fundamentals_high_availability_capacity_planning.md"

# Keep the payload in a literal here-string so Markdown fences, quotes, and backslashes stay byte-for-byte literal.
$q017to020 = @'

## Q017. 如何设计通知系统？

**回答：**

通知系统是一个**消息路由平台**，位于业务服务（生产者）和送达渠道（APNs、FCM、SES、Twilio）之间。生产者发布一条逻辑通知，系统解析接收人、按渠道拆分、应用偏好设置，并保证至少一次送达。

**架构设计：**

```
生产者 -> Notification API（校验、入队、幂等）-> Kafka（按 user_id 分区）
       -> 偏好解析器 -> 渠道 Worker（Push/Email/SMS/In-App）
       -> 外部提供商 -> 送达日志
```

**核心组件：**

| 组件 | 职责 | 技术选型 |
|------|------|---------|
| Notification API | 统一入口，返回 202 Accepted | 无状态服务 |
| 消息队列 | 持久缓冲，解耦接收和送达 | Kafka（按 user_id 分区） |
| 偏好服务 | 每用户 opt-in/out、免打扰时段、渠道偏好 | PostgreSQL + Redis 缓存 |
| 渠道 Worker | Push（APNs/FCM）、Email（SES/SendGrid）、SMS（Twilio） | 独立 Worker 池 |
| 送达日志 | 追踪每次发送、重试、结果 | Cassandra 或 DynamoDB |
| 死信队列 | 重试耗尽后的失败消息 | Kafka DLQ |

**优先级分级：**

| 优先级 | 示例 | 延迟目标 | 策略 |
|--------|------|---------|------|
| P0 - 事务型 | OTP、密码重置、支付确认 | p99 < 5s | 独立 Worker，绕过批处理 |
| P1 - 用户交互 | 聊天消息、评论回复 | p99 < 60s | 标准管道，10s 去重窗口 |
| P2 - 批量/营销 | 周报、促销 | 小时级 | 激进批处理，免打扰时段 |

**渠道降级策略：**

1. 先尝试 Push（最便宜、摩擦最小）。如果设备 60s 内确认，停止。
2. 否则延迟后尝试 In-App 收件箱 + Email。
3. 仅对时间关键事件升级到 SMS。

**关键设计决策：**

- **幂等性**：每个事件带 idempotency key。Redis 中去重窗口（如 24 小时）。
- **限流**：按提供商做 token bucket。SES 有 MaxSendRate 限制，Twilio 有每秒 SMS 上限。
- **模板化**：模板存储变量占位符，发送时渲染。模板版本化管理。
- **Webhook 回调**：提供商回调更新送达状态（delivered、bounced、complained）。

**为什么按 user_id 分区：**

同一用户的通知发到同一 Kafka 分区，保证同一用户的消息顺序处理。避免"密码重置"通知在"账号锁定"通知之后到达。

**扩展性考虑：**

- 渠道 Worker 按渠道独立扩缩容。Push Worker 可能需要 100 个实例，SMS Worker 可能只需要 10 个。
- 外部提供商有速率限制，Worker 需要本地 token bucket 限流，避免被提供商封禁。
- 送达日志写入量大，用 Cassandra/DynamoDB 而非关系型 DB。

常见错误：
- 同步发通知——一个慢的 SMS 提供商拖慢整个请求
- 不幂等——用户收到重复通知
- 没有优先级——OTP 验证码和营销邮件混在一起排队
- 硬编码模板——改文案需要发版

面试里可以这样答：

```
通知系统是消息路由平台，生产者发布逻辑通知，系统按渠道拆分并保证至少一次送达。架构上：Notification API -> Kafka（按 user_id 分区）-> 偏好解析 -> 渠道 Worker -> 外部提供商。优先级分 P0（事务型，p99<5s）、P1（交互型）、P2（批量型）。关键设计包括幂等去重、渠道降级、提供商限流和送达追踪。
```

## Q018. 如何设计配置中心？

**回答：**

配置中心（Configuration Center）是一个集中式的分布式 KV 存储，管理应用配置（DB 连接串、feature flag、限流阈值等）。核心需求不是"存配置"，而是**配置变更后所有客户端能近乎实时地感知并热加载。**

**核心需求：**

- **KV 存储**带命名空间（如 `/production/database/url`）
- **强一致性**：写入确认后所有客户端读到相同值
- **Watch 通知**：客户端订阅 key 前缀，变更时推送
- **高可用**：少数节点故障不影响服务
- **访问控制**：按命名空间做读写权限

**Watch 机制（杀手特性）：**

1. 客户端打开长连接 gRPC stream 到配置中心
2. 客户端调用 `Watch(prefix)` —— 服务端在 watch registry 中注册
3. 当 prefix 下任何 key 被写入，服务端 fan-out 到所有匹配的 watch
4. 变更的 key、新值和版本号通过已注册的 stream 发送
5. 重连时，客户端发送最后收到的 revision；服务端回放错过的事件

**技术对比：**

| 特性 | etcd | ZooKeeper | Consul |
|------|------|-----------|--------|
| 共识算法 | Raft | ZAB | Raft |
| API | 简单 KV | 层级 znode | KV + DNS + HTTP |
| 写入性能 | 数千/秒 | 数百/秒 | 数千/秒 |
| 语言 | Go | Java | Go |
| Kubernetes | 默认数据存储 | 遗留 | Service Mesh（Connect） |
| Watch 支持 | 是（gRPC stream） | 是 | 是 |

**etcd 为什么是 Kubernetes 的选择：**

- 简单 KV API（不需要 ZooKeeper 的层级 znode 概念）
- 高性能（数千写入/秒 vs ZooKeeper 的数百/秒）
- Go 实现，与 K8s 技术栈一致
- Raft 共识，易于理解和运维
- MVCC 存储，支持历史版本和 revision 追踪

**配置变更的推送模式 vs 拉取模式：**

| 模式 | 原理 | 延迟 | 资源消耗 |
|------|------|------|---------|
| 推送（Watch） | 服务端主动推送变更 | 毫秒级 | 长连接占用 |
| 拉取（Polling） | 客户端定时拉取 | 取决于轮询间隔 | 请求开销 |
| 长轮询 | 客户端请求挂起直到有变更 | 接近推送 | 连接占用 |

生产环境推荐 Watch + 定期拉取作为兜底（防止 Watch 连接断开漏掉事件）。

**配置热加载的客户端实现：**

1. 启动时拉取全量配置
2. 建立 Watch 连接
3. 收到变更事件后，更新本地缓存
4. 触发 `OnConfigChange` 回调，应用新配置
5. 对于连接池大小、超时时间等需要重建资源的配置，需要优雅关闭旧资源

**配置版本和回滚：**

- 每次变更产生新 revision
- 支持回滚到任意历史 revision
- 变更审计日志：谁、什么时间、改了什么、旧值和新值

常见错误：
- 配置放代码里或环境变量里——改配置需要重新部署
- 没有 Watch 机制——配置变更后需要重启才能生效
- 配置中心单点——它自己成了全系统的 SPOF
- 敏感配置明文存储——需要加密或集成 Vault

面试里可以这样答：

```
配置中心的核心是 Watch 机制：客户端订阅 key 前缀，变更时服务端主动推送。etcd 是 Kubernetes 的选择，Raft 共识 + 简单 KV API + gRPC Watch。客户端启动时全量拉取，运行时 Watch 增量更新，回调热加载。关键设计：强一致性、版本管理、变更审计、敏感配置加密。
```

## Q019. 如何设计服务注册中心？

**回答：**

在微服务架构中，实例是短暂的（自动扩缩容、容器重启、滚动更新）。服务注册中心（Service Registry）追踪哪些实例是活的、健康的、可以接收流量的。

**注册模式：**

| 模式 | 原理 | 示例 |
|------|------|------|
| 自注册 | 服务启动时自己注册，发送心跳 | Netflix Eureka 客户端 |
| 第三方注册 | 外部注册器注册服务 | Kubernetes、Consul Agent |

**发现模式：**

| 模式 | 原理 | 示例 |
|------|------|------|
| 客户端发现 | 客户端查注册中心，选实例，直接调用 | Eureka + Ribbon |
| 服务端发现 | 客户端调负载均衡器，LB 查注册中心后转发 | AWS ELB、Kubernetes Service |

**CAP 取舍：AP vs CP 注册中心**

这是服务注册中心最关键的架构决策：

| 注册中心 | CAP 选择 | 网络分区时行为 |
|----------|---------|--------------|
| Netflix Eureka | AP（可用） | 返回可能过期的数据，自我保护模式 |
| HashiCorp Consul | CP（一致） | 无 quorum 时返回错误 |
| etcd | CP（一致） | 无 quorum 时返回错误 |
| ZooKeeper | CP（一致） | 无 quorum 时返回错误 |

**为什么 Eureka 选择 AP：**

Netflix 的设计哲学是：服务发现返回一个可能已经挂掉的实例，比返回"找不到服务"要好。因为客户端本身有重试和熔断机制，拿到过期列表后调用失败可以重试下一个。但如果注册中心返回"没有可用实例"，客户端就完全无法工作了。Eureka 的自我保护模式：如果心跳丢失比例超过阈值，Eureka 认为不是服务挂了而是网络问题，保留所有注册信息不剔除。

**为什么 etcd/Consul 选择 CP：**

作为配置中心和分布式锁的基础设施，返回过期数据可能导致严重问题（比如两个节点都以为自己是 leader）。对于配置和协调场景，一致性优先于可用性。

**健康检查：**

- **心跳**：服务定期发送心跳（如每 30s）
- **健康端点**：注册中心探测 `/health` 端点（HTTP、TCP、gRPC）
- **注销**：连续 N 次检查失败后，从注册中心移除
- **优雅下线**：服务主动注销，避免流量打到即将关闭的实例

**缓存和容错：**

- 客户端本地缓存服务列表
- 注册中心不可用时，用本地缓存继续路由
- 定期全量同步作为兜底

常见错误：
- 用 CP 注册中心做服务发现——网络分区时整个服务发现不可用
- 没有健康检查——流量打到已挂的实例
- 没有客户端缓存——注册中心挂了客户端也无法路由
- 心跳间隔太短——注册中心压力大；太长——故障检测慢

面试里可以这样答：

```
服务注册中心的核心决策是 AP vs CP。Eureka 选 AP：宁可返回过期列表也不拒绝服务，因为客户端有重试和熔断。etcd/Consul 选 CP：配置和协调场景下一致性优先。健康检查用心跳 + 主动探测，客户端本地缓存服务列表做容错。关键设计：注册、发现、健康检查、缓存容错。
```

## Q020. 如何设计限流系统？

**回答：**

限流（Rate Limiting）允许在时间窗口 T 内最多 N 个请求。算法是简单的部分，难的是**在网关集群中实施一个统一的逻辑限制。**

**算法对比：**

| 算法 | 每 key 状态 | 突发 | 精度 | 原子性成本 |
|------|-----------|------|------|-----------|
| Token Bucket | 2 字段（tokens, last_refill） | 可配置 | 精确（Lua） | EVALSHA |
| 固定窗口 | 1 计数器 | 2x 边界突发 | 近似 | INCR + EXPIRE |
| 滑动窗口日志 | Sorted Set（所有时间戳） | 无 | 精确 | ZADD + ZRANGEBYSCORE |
| 滑动窗口计数器 | 2 整数 | 无 | ~0.003% 误差 | INCR + GET |
| Leaky Bucket | 队列 + 漏出 | 无 | 平滑输出 | 后台漏出 |
| GCRA | 1 标量（TAT） | 可配置 | 精确 | EVALSHA（Lua） |

**推荐**：Token Bucket 用于用户面 API（允许突发），滑动窗口计数器用于严格端点限制。

**分布式限流的难题：**

两个网关 Pod 同时看到同一用户的请求。两个都读 bucket，都发现还有 1 个 token，都放行。用户拿到 2 个而不是 1 个。

**解决方案：两级方案（Cloudflare、Stripe、Envoy 模式）**

1. 每个网关从全局 Redis bucket 借一批 token（如 10 个）
2. 网关本地消耗，直到本地 token 用完或过期（5s TTL）
3. 本地用完再借。全局 bucket 空了就借不到
4. 效果：Redis 流量减少 10x，精度误差约 6%

**原子性选项：**

| 方案 | 原理 | 代价 |
|------|------|------|
| Redis Lua | 一个 EVAL 脚本完成 refill + decrement + write | ~50us/脚本，单次往返 |
| Redis WATCH/MULTI | 乐观锁，冲突重试 | 成功时两次往返 |
| DB 行锁 | SELECT FOR UPDATE | 比 Redis 慢 ~10x |

**Fail-Open vs Fail-Closed：**

限流器必须 **fail open** —— 永远不要成为它本应防止的故障。如果 Redis 挂了，网关应该放行请求（配合本地保守限制），而不是拒绝所有流量。否则限流器本身就成了最大的单点故障。

**限流维度：**

| 维度 | 示例 | 用途 |
|------|------|------|
| 用户级 | 每用户 100 req/min | 防止单用户滥用 |
| IP 级 | 每 IP 1000 req/min | 防爬虫 |
| API 级 | 每 API key 10000 req/min | 按套餐限流 |
| 服务级 | 支付服务 5000 req/s | 保护下游 |

**响应头设计：**

```
X-RateLimit-Limit: 100
X-RateLimit-Remaining: 45
X-RateLimit-Reset: 1620000000
Retry-After: 30
```

让客户端知道自己的配额状态，避免盲目重试。

常见错误：
- 单网关内存限流——多网关场景下形同虚设
- 限流器 fail closed——Redis 挂了全站 429
- 固定窗口算法——窗口边界处允许 2x 突发
- 没有响应头——客户端不知道何时重试

面试里可以这样答：

```
限流算法推荐 Token Bucket（允许突发），分布式实施用两级方案：网关从全局 Redis 借 token 批次，本地消耗。Redis Lua 保证原子性。限流器必须 fail open——Redis 挂了放行而非拒绝。限流维度包括用户、IP、API key、服务。响应头告知客户端配额状态。
```

'@

# Use UTF-8 without BOM to match the repository markdown files and avoid Windows PowerShell legacy encodings.
[System.IO.File]::AppendAllText($file, $q017to020, [System.Text.UTF8Encoding]::new($false))
Write-Output "Q017-Q020 appended successfully"
