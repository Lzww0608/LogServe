# 6. 幂等、重试、超时、去重与 exactly-once 语义

这一组问题讨论“请求到底执行了几次”。面试里经常把幂等、去重、重试、超时和 exactly-once 混成一个词，但它们不是一回事。

```text
幂等: 同一个操作重复执行，业务效果等同于执行一次
去重: 识别重复输入，并丢弃、合并或返回已有结果
重试: 在失败或不确定时再次发起请求
超时: 调用方停止等待，但不说明被调方一定没执行
exactly-once: 对外承诺某个业务效果只发生一次，通常靠协议、状态机和持久化记录组合出来
```

## Q001. 幂等的严格定义是什么？

**回答：**

严格一点说，幂等不是“返回一样”，而是“同一个操作重复执行多次，对系统产生的目标业务效果和执行一次相同”。

数学里常写成：

```text
f(f(x)) = f(x)
```

放到服务接口里，可以写成：

```text
apply(apply(state, request), request) == apply(state, request)
```

这句话有几个前提。

1. **必须是同一个 request**

   “同一个”不能只靠 URL 判断。通常至少要看：

   ```text
   method
   endpoint / operation
   tenant / account
   idempotency key
   request payload fingerprint
   resource scope
   ```

   `POST /orders` 调两次，如果没有业务唯一键或 idempotency key，通常就是创建两笔订单，不是同一个操作。

2. **比较的是业务效果，不一定是响应字节**

   RFC 9110 对 HTTP 幂等方法的定义看的是服务器上的“预期效果”。服务器仍然可以为每次请求写 access log、revision history 或监控指标。也就是说，幂等不要求所有内部痕迹完全一样。

   例如：

   ```text
   DELETE /sessions/abc
   ```

   第一次可能返回 `204`，第二次可能返回 `404` 或继续返回 `204`。只要目标状态都是“session abc 不存在”，这个操作仍然可以是幂等的。

3. **幂等有作用域**

   一定要问“在哪个范围内幂等”。AWS EC2 的 client token 就区分 regional idempotency 和 zonal idempotency。同一个 token 在不同 Region 或不同 Availability Zone 里，语义可能不同。

4. **幂等有有效期**

   使用 idempotency key 的系统通常只在一段时间内记住 key。Stripe 文档提到，key 至少 24 小时后可以被清理；清理后同一个 key 再来，可能被当成新请求。

5. **幂等不等于没有副作用**

   `PUT /users/1/status = disabled` 执行十次，最终状态还是 disabled。它仍然可能写十条访问日志、刷新十次缓存、更新内部指标。面试里要说清楚：幂等关心的是业务承诺的目标效果，不是所有内部动作都只发生一次。

**常见例子**

```text
幂等:
  PUT /users/1/status = "disabled"
  DELETE /sessions/abc
  SET balance = 100
  cancel(order_id=123)

非幂等:
  POST /orders
  charge(card, amount=100)
  append(log, record)
  increment(counter)
  send_email(to, body)
```

非幂等操作也可以被包装成“可安全重试”。例如 `POST /charges` 本来会创建一笔新扣款；如果客户端带上唯一 idempotency key，服务端保存第一次执行结果，后续相同 key 返回同一个结果，那么这类创建操作就可以获得幂等语义。

**弱幂等和强幂等**

工程里常分两层：

```text
状态幂等:
  多次执行后的业务状态一样，但响应可能不同

响应幂等:
  多次执行不但业务状态一样，status code 和 response body 也一样
```

Stripe 的实现接近响应幂等：同一个 idempotency key 的第一次请求进入执行后，后续相同 key 会返回第一次保存的 status code 和 body，哪怕第一次是 `500`。

一句话：幂等的严格定义是，同一个语义操作在同一作用域和有效期内执行一次或多次，对外可观察的目标业务效果相同；响应相同、日志相同、内部执行次数相同，都不是幂等定义本身。

## Q002. 幂等和去重是同一个概念吗？

**回答：**

不是。幂等是语义，去重是实现手段。

```text
幂等问: 重复执行后的业务效果是不是一样？
去重问: 这次输入是不是之前见过？
```

去重经常用来实现幂等，但幂等不一定靠去重。

**不用去重也能幂等**

例如：

```sql
UPDATE users
SET status = 'disabled'
WHERE id = 123;
```

执行一次和执行十次，最终状态都是 `disabled`。这里没有专门的 dedup table，也没有 idempotency key。操作本身就是幂等的。

再比如：

```text
DELETE /sessions/abc
```

如果目标语义是“session abc 不存在”，重复删除可以是幂等的。

**有去重也不一定幂等**

去重做错了，会破坏业务。

例子：

```text
dedup_key = user_id
```

如果用户真的下了两笔不同订单，按 `user_id` 去重会误删第二笔订单。系统没有做到幂等，而是把合法请求当重复请求丢了。

另一个例子：

```text
dedup_key = idempotency_key
payload 不校验
```

第一次请求：

```json
{"amount": 100, "currency": "USD"}
```

第二次同 key：

```json
{"amount": 900, "currency": "USD"}
```

如果服务端只看 key，不看 payload fingerprint，可能把客户端 bug 或攻击请求吞掉。Stripe 文档明确说，会比较后续请求参数和原请求参数，不一致时返回错误。

**去重的几种处理方式**

重复请求到来时，服务端可以有不同策略：

1. **返回第一次结果**

   API idempotency key 常见做法。第一次执行成功或失败，后续相同 key 返回保存的结果。

2. **丢弃重复消息**

   消费者常这样做：

   ```text
   if event_id already processed:
     ack and skip
   ```

3. **合并重复请求**

   同一个用户短时间多次刷新缓存，只触发一次后台任务。

4. **返回冲突**

   原请求还在处理中，后来的相同 key 请求可以返回 `409 Conflict` 或 operation 状态。

5. **拒绝 payload 不一致的重复 key**

   常见语义是 `422 Unprocessable Content` 或业务错误码，例如 AWS EC2 的 `IdempotentParameterMismatch`。

**去重也不等于 exactly-once**

消费者去重只能说明“这个消费者尽量不重复处理同一个 event id”。它管不了：

- 生产者是否发送了两个不同 id 的相同业务事件。
- 下游数据库是否重复写。
- 外部邮件、短信、支付是否重复触发。
- 去重状态过期后重复消息是否又被处理。

一句话：幂等是业务语义，去重是识别重复输入的机制；去重可以帮助实现幂等，但错误的去重键、过短的去重窗口或不校验 payload，都会让系统既不安全也不幂等。

## Q003. 幂等 key 应该由客户端生成还是服务端生成？

**回答：**

大多数需要“安全重试”的 API，幂等 key 应该由客户端生成。原因很直接：客户端最清楚哪些请求属于同一个业务意图，也只有客户端能在超时后带着同一个 key 再发一次。

Stripe 和 AWS EC2 都是这种模式。Stripe 文档要求客户端生成 idempotency key；AWS EC2 使用 client token，并要求同一个 token 不要复用于其他 API 请求。

**为什么通常由客户端生成**

考虑这个场景：

```text
client -> server: 创建订单
server: 订单创建成功
server -> client: 响应在网络中丢失
client: 超时，不知道订单是否创建
```

如果 key 是服务端在创建时生成的，但响应丢了，客户端拿不到 key。下一次重试就无法表达“我是在重试刚才那次创建订单”。

客户端生成 key 后，流程变成：

```text
key = uuid()

client -> server: POST /orders, Idempotency-Key: key
server: 创建订单并保存 key -> result
响应丢失
client -> server: POST /orders, Idempotency-Key: same key
server: 返回第一次结果
```

这才解决了超时后的不确定性。

**服务端生成什么时候合适**

服务端生成也有用，但适合另一类协议：

1. **先申请 operation id，再提交**

   ```text
   POST /operations -> op_id
   PUT /operations/{op_id}/commit
   ```

   第一阶段最好没有不可逆副作用。客户端拿到 `op_id` 后，后续用它实现幂等。

2. **资源 ID 由客户端指定**

   ```text
   PUT /orders/{order_id}
   ```

   如果 `order_id` 是客户端生成的唯一业务 ID，这个 ID 本身就承担了部分幂等职责。

3. **异步任务返回 operation handle**

   第一次请求成功返回 task id。之后客户端用 task id 查询或重试某个阶段，而不是重新发起创建。

共同点是：客户端在需要重试时，必须已经掌握稳定标识。否则服务端生成 key 帮不上忙。

**客户端 key 的要求**

客户端 key 要满足：

- 作用域内足够唯一。
- 随机性足够，避免碰撞。
- 不包含敏感信息。
- 在整个重试周期内保持不变。
- 同一个业务意图复用同一个 key。
- 不同业务意图使用不同 key。

常见选择：

```text
UUID v4
ULID
KSUID
128-bit random token
```

不要用：

```text
user_id
timestamp seconds
phone number
email
order amount
```

这些要么容易碰撞，要么泄露隐私，要么不能区分不同业务意图。

**服务端仍然要做什么**

客户端生成 key，不代表服务端可以完全相信客户端。服务端要：

- 定义 key 的作用域。
- 保存 key、payload fingerprint、状态和结果。
- 检查 payload 是否一致。
- 处理并发相同 key。
- 设置过期策略。
- 防止跨租户 key 泄露结果。
- 限制 key 长度和字符集。

一句话：幂等 key 通常应该由客户端生成，因为重试发生在客户端，只有客户端能把多次请求绑定到同一个业务意图；服务端生成 key 只有在客户端先拿到稳定 operation id 或资源 ID 后才可靠。

## Q004. 幂等 key 的作用域应该如何定义？

**回答：**

幂等 key 的作用域要小心定义。作用域太大，会误判重复；作用域太小，又挡不住重复副作用。工程上通常不要只存一个裸 key，而是存一个组合键。

常见组合：

```text
tenant_id / account_id
API operation
HTTP method
resource type
idempotency_key
region / zone
payload fingerprint
```

例如：

```text
(merchant_id, "CreatePayment", idempotency_key)
(tenant_id, "CreateOrder", idempotency_key)
(account_id, "RunInstances", region, availability_zone, client_token)
```

**为什么不能全局只看 key**

如果只看：

```text
idempotency_key = "abc"
```

两个不同租户都用了 `"abc"`，就可能串单。更严重的是，B 租户可能拿到 A 租户的第一次响应。这是安全问题，不只是幂等问题。

所以第一层作用域通常是：

```text
tenant / account / credential principal
```

**为什么要包含 operation**

同一个 key 如果在不同接口复用，语义应该隔离：

```text
POST /orders        Idempotency-Key: k1
POST /refunds       Idempotency-Key: k1
```

这两个请求不应该互相去重。key 的作用域里应该包含 operation 或 route。否则一个客户端 SDK 的 bug 会让完全不同的接口互相干扰。

**为什么要包含区域或分片**

分布式系统里，作用域有时天然带区域。AWS EC2 文档区分 regional idempotency 和 zonal idempotency：

```text
regional:
  同一个 client token 在同一个 Region 内只完成一次

zonal:
  同一个 client token 在同一个 Region 的每个 Availability Zone 内各自幂等
```

这说明幂等语义不是抽象地“全局一次”，而是要和资源调度边界一致。

**payload fingerprint 是否属于作用域**

更稳妥的做法是：

```text
dedup identity:
  (tenant_id, operation, idempotency_key)

payload_fingerprint:
  用来校验后续请求是否和第一次请求一致
```

也就是说，同一个 key 下 payload 不一致，不是创建一个新作用域，而是报错。

**推荐表结构**

```sql
CREATE UNIQUE INDEX uniq_idempotency
ON idempotency_records (
  tenant_id,
  operation_name,
  idempotency_key
);
```

记录里再存：

```text
request_fingerprint
status: processing / succeeded / failed
response_code
response_body_ref
resource_id
created_at
expires_at
```

一句话：幂等 key 的作用域应该绑定租户、操作和必要的资源/区域边界；payload fingerprint 用来发现误用，不应该把同一个 key 的不同 payload 悄悄变成不同请求。

## Q005. 幂等 key 过期时间如何选择？

**回答：**

幂等 key 的 TTL 不是越长越好，也不是越短越省事。它要覆盖“客户端可能合理重试的时间”，还要考虑业务上重复执行的风险。

可以从这个公式开始：

```text
TTL >= 最大客户端重试窗口
     + 最大网络延迟和超时误差
     + 服务端异步任务完成时间
     + 消息队列滞留时间
     + 时钟偏差和运维缓冲
```

然后再按业务风险加长。

**TTL 太短的问题**

假设 key 只保留 30 秒：

```text
t=0s   client 发起创建订单，服务端成功，响应丢失
t=35s  client 因网络恢复后重试
t=35s  服务端 key 已过期，创建第二个订单
```

这就是 TTL 太短导致的重复副作用。

**TTL 太长的问题**

长期保存 key 也有成本：

- 存储空间增长。
- 查询索引变大。
- 热 key 攻击面变大。
- 重放请求窗口变长。
- key 中如果错误包含敏感信息，会增加隐私风险。
- 客户端意外复用旧 key 时更容易撞上历史结果。

所以 key 不能无限保留，除非业务真的要求长期唯一。

**官方实现的参考**

Stripe 文档提到，idempotency key 至少 24 小时后可以被自动清理；清理后同一个 key 再来，会生成新的请求。IETF 的 `Idempotency-Key` 草案截至 2026-04-18 已经过期，不是正式 RFC，但里面“服务端应定义并发布 key 过期策略”的建议仍有工程参考价值。

**按业务选 TTL**

可以这样估：

```text
普通后台任务:
  1 小时到 24 小时

支付、订单、资源创建:
  24 小时到 7 天，视客户端重试和对账周期而定

移动端弱网请求:
  至少覆盖离线重试窗口

消息消费去重:
  覆盖消息最大保留时间或业务可接受重复窗口

金融对账:
  使用长期业务唯一键，而不是只靠 idempotency key TTL
```

如果业务不能接受“一周后同一个业务请求再次执行”，就不要只靠短 TTL idempotency key。应该引入业务唯一约束，例如：

```text
merchant_order_id
payment_intent_id
external_trade_no
```

这些业务 ID 可以长期唯一，idempotency key 只负责请求重试窗口。

**processing 状态的 TTL 要单独考虑**

幂等记录通常有状态：

```text
processing
succeeded
failed
```

`processing` 的 TTL 不应该和最终结果 TTL 混在一起。请求可能在服务端执行中崩溃，留下一个永远 processing 的 key。

处理方式：

- 给 processing 状态设置 lease。
- 超过 lease 后允许恢复、查询真实业务结果或标记 unknown。
- 不要简单删除 processing key 后重新执行，除非能证明原操作没有发生。

一句话：幂等 key TTL 至少要覆盖合理重试和异步完成窗口；高价值业务还需要长期业务唯一键兜底。TTL 是成本、重试安全和重复业务风险之间的权衡，不是随便设一个缓存过期时间。

## Q006. 重复请求 payload 不一致时应该返回什么语义？

**回答：**

同一个 idempotency key 带了不同 payload，应该当成客户端误用或冲突，不能悄悄执行第二次，也不应该返回第一次结果装作没事。

推荐语义：

```text
同 key + 同 payload:
  返回第一次结果，或返回当前 operation 状态

同 key + 不同 payload:
  拒绝请求，返回明确错误

同 key + 原请求仍在处理中:
  返回处理中或冲突，让客户端稍后重试/查询
```

**为什么不能执行第二次**

第一次：

```json
{"amount": 100, "currency": "USD"}
```

第二次：

```json
{"amount": 900, "currency": "USD"}
```

如果同一个 key 能创建两笔不同扣款，idempotency key 就失效了。

**为什么也不建议直接返回第一次结果**

直接返回第一次结果会掩盖严重问题。客户端以为 `900 USD` 的请求成功了，实际服务端返回的是 `100 USD` 的结果。后续对账会很难查。

更好的做法是返回：

```text
409 Conflict
或 422 Unprocessable Content
或业务错误码: IDEMPOTENCY_KEY_REUSED_WITH_DIFFERENT_PAYLOAD
```

IETF 的 `Idempotency-Key` 草案建议同 key 不同 payload 返回 `422 Unprocessable Content`。这个草案截至 2026-04-18 已经过期，不是正式 RFC，但这个错误语义很清楚：请求能理解，但语义不允许处理。

AWS EC2 的语义也类似：同一个 client token 重试成功请求，但参数不同，会返回 `IdempotentParameterMismatch`。Stripe 的做法是比较后续请求参数和原请求参数，不一致时报错。

**payload fingerprint 怎么做**

服务端应该保存请求 fingerprint：

```text
canonical_payload_hash = hash(canonical_json(payload))
```

但 canonicalization 要说清楚：

- JSON 字段顺序不应该影响 hash。
- 空字段、默认值、浮点数、时间格式要规范化。
- 幂等 key 本身不要参与 payload hash。
- trace id、request timestamp 这类每次变化的技术字段，要明确是否排除。
- 文件上传要考虑 body digest，而不是只看 metadata。

**哪些字段可以不同**

可以白名单化一些技术字段：

```text
允许不同:
  trace_id
  request_id
  client_send_time
  refresh 后的 Authorization header

不允许不同:
  amount
  currency
  target account
  quantity
  resource type
  business operation
```

不要用“忽略所有 metadata”这种宽泛规则。哪些字段影响业务效果，必须由接口定义说清楚。

**原请求仍在处理中怎么办**

如果第一条请求还没完成，第二条相同 key 请求来了：

```text
key exists, status = processing
payload same
```

常见处理：

```text
409 Conflict
202 Accepted + operation location
返回 operation 当前状态
```

这和 payload mismatch 是两类问题。payload mismatch 是“同 key 表达了不同意图”；processing 是“同一个意图还没完成”。

一句话：同一个 idempotency key 的 payload 不一致，应该明确拒绝，常见是 `422` 或业务级 mismatch 错误；原请求还在处理中可以返回 `409` 或 operation 状态。不要执行第二次，也不要把第一次结果伪装成第二次请求的结果。

## Q007. 为什么重试会放大非幂等操作的风险？

**回答：**

因为超时和网络错误只说明“调用方没拿到确定结果”，不说明“服务端没执行”。非幂等操作一旦被重复执行，副作用就会叠加。

典型场景：

```text
client -> payment-service: 扣款 100 元
payment-service: 扣款成功
response: 网络中丢失
client: 超时，以为失败
client -> payment-service: 重试扣款 100 元
payment-service: 又扣一次
```

从客户端看，它只是“重试一次”。从业务看，用户被扣了两次。

**超时是不确定态**

超时至少有三种可能：

```text
请求根本没到服务端
请求到了，但服务端还没执行
请求到了，服务端已经执行，响应丢了
```

客户端只知道自己等太久了，不知道真实状态。对非幂等操作，第三种最危险。

**非幂等副作用会累加**

这些操作都容易被重试放大：

- 创建订单。
- 扣款。
- 发券。
- 发短信。
- 发送邮件。
- 增加余额。
- 扣库存。
- 追加日志。
- 触发外部 webhook。

如果没有幂等 key、业务唯一约束或事务状态机，重试一次就可能多做一次。

**多层重试会乘法放大**

AWS Builders Library 里举过一个典型例子：如果调用链有 5 层，每层都重试 3 次，底层数据库面对的请求可能被放大到 243 倍。

```text
3 * 3 * 3 * 3 * 3 = 243
```

如果底层本来已经过载，这种重试会让它更难恢复。

**重试会把瞬时故障变成流量风暴**

服务短暂抖动时，大量客户端同时超时，然后同时重试。如果没有 backoff 和 jitter，就会出现：

```text
服务变慢 -> 客户端超时 -> 重试增多 -> 服务更慢 -> 更多超时
```

这就是 retry storm。

**下游副作用也要幂等**

入口 API 幂等不代表全链路安全：

```text
本服务创建订单有幂等
但每次重试都重新调用短信网关
短信网关没有幂等
用户收到多条短信
```

幂等要覆盖整个副作用链。外部支付、短信、邮件、webhook 要么使用对方的幂等机制，要么通过 outbox、状态机和补偿流程控制。

一句话：重试放大非幂等风险，是因为调用方无法从超时判断服务端是否已经执行；一旦重复执行会叠加副作用，多层重试还会把负载按乘法放大。

## Q008. 重试应该在客户端、中间层还是服务端做？

**回答：**

重试最好只在一个明确层次做。不要客户端、网关、service mesh、服务端、SDK、数据库驱动每层都自作主张地重试。否则你很难估算实际请求次数，也很难控制副作用。

**客户端重试**

适合：

- 客户端知道业务意图。
- 客户端能稳定复用 idempotency key。
- API 文档明确允许重试。
- 调用跨网络，可能出现连接 reset、超时、临时 5xx。

优点：

- 最了解用户是否还在等待。
- 能带 end-to-end deadline。
- 能复用同一个 idempotency key。
- 能处理最终失败后的用户提示。

缺点：

- 客户端数量多，容易同时重试。
- 不同客户端实现不一致。
- 老版本 SDK 可能策略错误。

所以客户端重试要配 SDK 默认策略、retry budget、backoff 和 jitter。

**中间层重试**

中间层包括 API gateway、proxy、service mesh、消息网关。它适合处理非常明确的短暂传输失败，例如连接建立失败、上游连接池临时 reset，或者只读请求。

风险是中间层通常不知道业务语义。它看到的是：

```text
POST /pay
timeout
```

但它不知道这笔支付是否已经落库、是否调用了外部渠道、是否能安全重试。中间层如果要重试写请求，必须依赖明确的协议标记，例如 method 幂等、idempotency key 存在、状态码可重试、request body 可重放。

**服务端内部重试**

服务端适合重试内部短暂失败：

- 数据库死锁或可重试事务冲突。
- 乐观锁冲突。
- 临时连接失败。
- 下游限流后的延迟重试。

服务端的优势是了解业务状态，能把重试包在事务、状态机或 outbox 里。缺点是它可能占用线程和资源太久，也可能掩盖下游长期故障。

**推荐原则**

```text
端到端业务重试:
  客户端或客户端 SDK 做，带 idempotency key 和 deadline

短暂内部错误:
  服务端局部重试，限制次数和时间

透明代理重试:
  只对明确安全的请求做，默认不要重试非幂等 POST
```

还要保证：

- 整条链路有统一 deadline。
- 每层都尊重上游取消。
- 不在多层同时配置高次数重试。
- 监控里能看到原始请求数和重试次数。

一句话：重试应该放在最了解语义的层。业务级重试通常在客户端或 SDK，服务端只做局部可控重试，中间层只重试明确安全的场景；最危险的是每一层都觉得自己“只重试几次”。

## Q009. 自动重试需要满足哪些前提？

**回答：**

自动重试不是看到错误就再发一次。至少要满足这些前提。

1. **操作可安全重试**

   满足其中之一：

   ```text
   操作本身幂等
   或请求带稳定 idempotency key
   或服务端能证明原操作没有执行
   或业务能接受重复副作用
   ```

   对扣款、发货、发券这类操作，如果没有幂等协议，不要自动重试。

2. **错误类型可重试**

   常见可重试：

   ```text
   连接超时
   连接 reset
   408 Request Timeout
   429 Too Many Requests
   500 / 502 / 503 / 504
   可重试事务冲突
   临时 DNS / 连接池错误
   ```

   常见不可重试：

   ```text
   400 Bad Request
   401 Unauthorized
   403 Forbidden
   404 Not Found（多数写操作下）
   409 Conflict（除非文档说明可以）
   422 Unprocessable Content
   payload mismatch
   ```

3. **有总 deadline**

   每次重试都要受总时间预算限制：

   ```text
   overall_deadline = 2s
   attempt1 timeout = 300ms
   attempt2 timeout = 500ms
   attempt3 timeout = 剩余时间
   ```

   没有 deadline 的重试会堆积请求，拖垮线程池。

4. **有最大次数**

   例如：

   ```text
   max_attempts = 3
   ```

   不要无限重试。消息队列消费也要有死信队列或人工处理路径。

5. **使用 backoff 和 jitter**

   失败后立刻重试，会把故障放大。指数退避减少压力，jitter 打散同步重试。

6. **同一次业务意图复用同一个 idempotency key**

   自动重试时如果每次生成新 key，就等于每次都是新请求。

   ```text
   错误:
     retry1 key=a
     retry2 key=b

   正确:
     retry1 key=a
     retry2 key=a
   ```

7. **尊重服务端信号**

   如果服务端返回：

   ```text
   Retry-After
   rate limit reset time
   operation status URL
   ```

   客户端要尊重这些信号，不要按本地固定间隔硬打。

8. **请求体可重放**

   大文件上传、streaming body、一次性 reader，如果不能重新读取 body，就不能透明重试。要么先落本地临时文件，要么使用分片上传协议。

9. **有观测和预算**

   要记录：

   ```text
   original_requests
   retry_attempts
   retry_success
   retry_exhausted
   retry_latency_added
   retry_error_type
   ```

   否则出了事故只会看到“请求变多了”，不知道是谁在重试。

一句话：自动重试的前提是操作可安全重试、错误确实可重试、请求体可重放、重试有 deadline、次数、backoff、jitter 和观测，并且同一业务意图必须复用同一个 idempotency key。

## Q010. 指数退避和 jitter 解决什么问题？

**回答：**

指数退避解决“失败后不要立刻继续打”的问题；jitter 解决“大家不要同一时刻一起重试”的问题。二者经常一起用。

**没有退避会怎样**

服务开始变慢：

```text
t=0ms    10000 个客户端请求超时
t=1ms    10000 个客户端立刻重试
t=2ms    服务更慢，更多请求超时
t=3ms    又一轮重试
```

这会把短暂故障变成重试风暴。

**指数退避做什么**

指数退避让等待时间逐步增加：

```text
attempt 1 failed -> wait 100ms
attempt 2 failed -> wait 200ms
attempt 3 failed -> wait 400ms
attempt 4 failed -> wait 800ms
```

通常还要设置上限：

```text
backoff = min(base * 2^attempt, max_backoff)
```

它给服务恢复时间，也减少无效请求。

**只有退避还不够**

如果所有客户端都用同一个算法、同一个起点，它们仍然会同步：

```text
所有客户端 100ms 后一起重试
所有客户端 200ms 后一起重试
所有客户端 400ms 后一起重试
```

这只是把尖峰往后挪了。

**jitter 做什么**

jitter 在等待时间里加入随机性：

```text
wait = random(0, exponential_backoff)
```

这样客户端重试会被打散：

```text
client A: 37ms
client B: 82ms
client C: 11ms
client D: 96ms
```

AWS Builders Library 强调 jitter 的原因就在这里：分布式系统里，失败和重试很容易同步，随机化可以显著降低重试尖峰。

**常见 jitter 策略**

```text
full jitter:
  sleep = random(0, cap)

equal jitter:
  sleep = cap / 2 + random(0, cap / 2)

decorrelated jitter:
  sleep = min(max_backoff, random(base, previous_sleep * 3))
```

面试里不用背公式，但要说清楚：jitter 的目的不是“更慢”，而是“打散”。

**还要配合什么**

指数退避和 jitter 不是万能药。还要配：

- 最大重试次数。
- 总 deadline。
- retry budget。
- 服务端限流。
- `Retry-After`。
- idempotency key。
- 熔断或降级。

**它不能解决什么**

它不能让非幂等操作变安全。扣款接口没有幂等 key，加再好的 backoff 和 jitter，重复扣款风险仍然存在。它也不能修复永久性错误，比如参数错误、权限错误、payload mismatch。

一句话：指数退避降低连续重试对故障服务的压力，jitter 打散大量客户端的同步重试；它们解决的是流量形态问题，不解决非幂等副作用和错误分类问题。

## Q011. 固定间隔重试会造成什么风险？

**回答：**

固定间隔重试的主要风险是把失败流量变成周期性冲击。它看起来比立即重试温和，但如果大量客户端在同一时间失败，又按同一个间隔重试，请求会一波一波地打回故障服务，形成重试风暴。

**固定间隔重试是什么**

固定间隔重试通常是：

```text
request failed
wait 1s
retry
wait 1s
retry
wait 1s
retry
```

这种策略的缺点是它不关心系统当前是否已经恢复，也不关心失败规模有多大。它只是在固定节奏上持续施压。

**风险一：同步重试**

假设 10000 个客户端同时请求一个服务，服务因为短暂抖动在 `t=0` 超时。如果所有客户端都设置为 1 秒后重试，流量会变成：

```text
t=0s   10000 个请求超时
t=1s   10000 个重试请求同时到达
t=2s   如果继续失败，又是 10000 个重试请求同时到达
t=3s   下一波继续到达
```

这不是平滑恢复，而是周期性洪峰。服务刚有一点恢复能力，又被下一波重试压垮。

Kafka producer 配置文档在连接重试相关参数里也强调了 jitter 的价值：退避时间会加入随机因子，避免大量客户端在同一时间重新连接。这个问题不是 Kafka 独有的，所有集中式依赖都可能遇到。

**风险二：把局部故障放大成全局故障**

固定间隔重试很容易和调用链叠加：

```text
client -> api-gateway -> service-a -> service-b -> database
```

如果每一层都做 3 次固定间隔重试，最底层一次慢查询可能被放大成多倍请求。更麻烦的是，上游看不到下游真正的拥塞程度，只看到“超时了，所以再试一次”。

这会带来几类线上症状：

- 数据库连接池被重试请求占满。
- API 网关 QPS 周期性尖峰。
- 下游已经过载，上游仍然持续补充新请求。
- p99 延迟升高后触发更多超时，更多超时又触发更多重试。
- 熔断器、限流器和 autoscaling 都变得更难判断真实负载。

**风险三：固定间隔不适应故障类型**

同样是失败，背后的含义可能完全不同：

| 失败类型 | 是否适合固定间隔重试 | 原因 |
| --- | --- | --- |
| 短暂网络抖动 | 不理想 | 需要退避和随机化，避免同步冲击 |
| 下游过载 | 危险 | 重试会继续增加负载 |
| 参数错误 | 不应该重试 | 请求永远不会因为等待而变正确 |
| 权限错误 | 不应该重试 | 重试只会浪费资源 |
| leader 切换 | 可以重试 | 但应使用退避、deadline 和错误分类 |
| 外部服务限流 | 需要看 `Retry-After` | 固定间隔可能违反对方节流信号 |

固定间隔的问题在于它把这些情况都当成“等一会儿再来”。这会掩盖错误分类，让永久性错误、限流错误和过载错误都走同一套路径。

**风险四：间隔太短会打爆系统，间隔太长会拖慢恢复**

固定间隔还有一个很难调的参数：间隔到底设多大。

如果设得太短：

```text
timeout = 500ms
retry interval = 100ms
```

服务还没从前一批请求中恢复，下一批请求已经来了。

如果设得太长：

```text
timeout = 500ms
retry interval = 10s
```

一次短暂的 leader 切换可能 1 秒内已经恢复，但客户端还要等很久，用户感知延迟被不必要地放大。

指数退避比固定间隔更合理，是因为它会随着失败次数增加等待时间：

```text
100ms -> 200ms -> 400ms -> 800ms -> capped max
```

它给系统恢复空间，也避免无限制高频打下游。

**风险五：周期性流量会干扰监控和容量判断**

固定间隔重试会让指标呈现规律性锯齿：

```text
QPS:      high -> low -> high -> low
latency:  high -> lower -> high -> lower
errors:   burst -> quiet -> burst -> quiet
```

这会让排障更难。你看到的 QPS 不再是用户真实请求，而是“真实请求 + 重试请求 + 多层放大后的请求”。如果没有把原始请求和 retry attempt 分开统计，容量评估会偏离事实。

**应该怎么做**

固定间隔不是完全不能用，但只能用于很窄的场景，比如单机后台任务、低频管理任务、没有大规模同步客户端的内部脚本。面向线上服务时，更稳妥的策略通常是：

- 按错误类型决定是否重试。
- 使用指数退避。
- 加 jitter 打散客户端。
- 设置最大重试次数。
- 设置总 deadline，而不是每次尝试无限延长。
- 遵守服务端的 `Retry-After` 或限流响应。
- 用 retry budget 控制重试流量占比。
- 对非幂等操作要求 idempotency key。
- 在监控里区分原始请求和重试请求。

一句话：固定间隔重试最大的问题不是“会慢一点”，而是它会让大量客户端在同一节奏上反复冲击故障点，把短暂失败放大成周期性过载。

## Q012. timeout 和 cancellation 的区别是什么？

**回答：**

timeout 是“我最多等多久”，cancellation 是“我不再需要这项工作，请停止”。timeout 可以触发 cancellation，但两者不是同一个概念。

**timeout 关注等待预算**

timeout 表达的是调用方的等待上限：

```text
client waits at most 500ms
if no response before 500ms, client returns timeout
```

它回答的问题是：调用方还愿意等多久。

gRPC 文档把 deadline 描述为客户端愿意等待 RPC 完成的截止时间。超过这个截止时间，客户端会看到 `DEADLINE_EXCEEDED`。这里的重点是客户端视角：客户端已经不再等待这个调用的结果。

**cancellation 关注停止信号**

cancellation 表达的是调用方或系统主动撤销工作：

```text
user closed page
request context canceled
server should stop unnecessary work
```

它回答的问题是：这项工作是否还应该继续做。

Go 的 `context.Context` 把 deadline、取消信号和请求范围值放在同一个接口里。`context.Canceled` 表示上下文被主动取消，`context.DeadlineExceeded` 表示 deadline 到期。两者最终都会让 `ctx.Done()` 关闭，但语义不同。

**二者的关系**

可以这样理解：

| 维度 | timeout | cancellation |
| --- | --- | --- |
| 核心含义 | 等待时间耗尽 | 调用方撤销工作 |
| 触发来源 | 时间预算到期 | 用户操作、上游取消、连接断开、deadline 到期 |
| 典型错误 | deadline exceeded、timeout | canceled |
| 是否一定通知服务端 | 不一定 | 取决于协议和实现 |
| 是否一定停止服务端执行 | 不一定 | 也不一定，通常是协作式停止 |
| 关注点 | 调用方不再等待 | 系统应该释放资源并停止无用工作 |

**timeout 不一定等于服务端停止**

这是面试里最容易答错的地方。客户端 timeout 只说明客户端不等了，不说明服务端一定没有收到请求，也不说明服务端一定停止执行。

可能发生的情况包括：

```text
case 1: 请求还没发出去，客户端本地就超时了
case 2: 请求发出去了，但服务端没收到
case 3: 服务端收到了，还没开始执行
case 4: 服务端正在执行，客户端超时了
case 5: 服务端已经提交成功，但响应丢了
case 6: 服务端继续执行，稍后才提交成功
```

所以 timeout 后的状态通常是 unknown，而不是 failed。

**cancellation 也通常是协作式的**

即使协议把取消信号传给了服务端，服务端也未必能立即停止。gRPC cancellation 文档说明，服务端 handler 需要检查 RPC 是否已经被取消，并停止继续处理；gRPC 通常不能强行中断应用层已经开始的业务逻辑。

这点很重要。很多业务代码是这样的：

```text
begin transaction
write order
call payment provider
send message
commit transaction
```

如果取消信号在中间到达，系统需要决定：

- 已经写入的状态是否回滚？
- 外部支付调用是否已经成功？
- 消息是否已经发送？
- 如果不能回滚，应该如何记录最终状态？

取消不是魔法，它只是一个信号。业务代码必须设计检查点、回滚策略和最终状态。

**工程上怎么区分**

接口设计时，建议把三类状态分开：

```text
timeout: 调用方等待超时，结果未知
canceled: 调用方明确撤销，服务端应尽量停止
failed: 服务端确认执行失败
```

日志和监控也应该分开记录：

- `deadline_exceeded`
- `client_canceled`
- `server_canceled`
- `server_failed`
- `committed_after_timeout`

如果把 timeout 和 cancellation 都记成 “failed”，排障时会丢掉最关键的信息。

一句话：timeout 是等待预算耗尽，cancellation 是停止意图；timeout 可以导致 cancellation，但客户端超时不等于服务端失败，取消信号也不等于业务副作用已经被撤销。

## Q013. 客户端超时是否代表服务端没有执行成功？

**回答：**

不代表。客户端超时只代表客户端在规定时间内没有拿到结果，服务端是否执行成功是未知状态。

**为什么不能直接判失败**

分布式系统里，请求和响应是两条不同方向的消息。客户端看到超时，可能是请求没到，也可能是响应没回来。

典型情况有：

```text
client sends request
server receives request
server commits business transaction
server sends response
response is lost or delayed
client times out
```

从客户端看，这次调用失败了；从业务状态看，这次操作已经成功了。

**超时后的真实状态空间**

客户端超时后，服务端状态至少有这些可能：

| 服务端状态 | 客户端看到的现象 | 风险 |
| --- | --- | --- |
| 请求未发出 | timeout | 重试通常安全，但客户端未必知道 |
| 请求已发出但服务端未收到 | timeout | 重试可能安全 |
| 服务端收到但尚未执行 | timeout | 后续可能执行，也可能被取消 |
| 服务端执行中 | timeout | 重试可能并发执行同一逻辑 |
| 服务端已提交，响应未返回 | timeout | 重试可能造成重复副作用 |
| 服务端执行失败，错误响应丢失 | timeout | 客户端无法区分失败和成功 |

所以 timeout 后不能简单写：

```text
timeout => operation failed
```

更准确的写法是：

```text
timeout => operation result unknown
```

**这对重试意味着什么**

如果请求是幂等的，客户端可以带同一个 idempotency key 重试：

```text
POST /payments
Idempotency-Key: req-123
```

服务端用这个 key 查到第一次请求的最终结果，然后返回同一个语义结果。这样客户端不需要猜测第一次到底有没有成功。

如果请求不是幂等的，客户端超时后直接重试就有风险：

```text
create order timeout
retry create order
two orders may be created
```

扣款、发券、发货、发邮件、发消息这类有外部副作用的操作尤其危险。

**服务端应该提供什么能力**

可靠的 API 通常会提供至少一种确认机制：

- idempotency key，重复请求返回第一次执行结果。
- operation id，客户端可以查询操作状态。
- resource id，由客户端指定资源标识，使创建操作天然可重试。
- transactional outbox，保证业务提交和消息发布状态可恢复。
- status endpoint，例如 `GET /operations/{id}`。
- 幂等业务约束，例如订单号、支付单号、转账流水号唯一。

没有这些机制时，客户端超时后只剩两个坏选择：

- 不重试，可能丢掉一次本来应该成功的操作。
- 重试，可能制造重复副作用。

**gRPC 语义里的注意点**

gRPC 客户端 deadline 到期后会得到 `DEADLINE_EXCEEDED`。这个错误只说明 deadline 到了，并不自动说明服务端业务没有提交。服务端收到取消信号后，应用 handler 也需要主动检查并停止；如果业务代码没有检查，或者已经越过提交点，仍然可能完成写入。

面试时可以直接说：

```text
DEADLINE_EXCEEDED means the client gave up waiting.
It does not prove the server did not commit.
```

**正确的处理思路**

客户端超时后，应该按操作类型处理：

| 操作类型 | 超时后处理 |
| --- | --- |
| 纯读请求 | 可以重试，但要考虑读放大和缓存 |
| 幂等写请求 | 带同一个 idempotency key 重试 |
| 非幂等写请求 | 不应盲目重试，需要查询状态或人工补偿 |
| 长任务 | 返回 operation id，异步查询最终状态 |
| 外部副作用 | 以业务流水号做唯一约束，避免重复执行 |

一句话：客户端超时后，服务端可能没收到、正在执行、已经成功或已经失败；正确语义是“结果未知”，要靠幂等 key、业务唯一约束或状态查询把未知状态收敛掉。

## Q014. 请求超时后服务端继续执行会带来什么一致性问题？

**回答：**

请求超时后服务端继续执行，最大的问题是客户端和服务端对同一操作的认知分裂：客户端以为失败或未知，服务端却可能稍后提交成功。之后客户端重试、取消、补偿或发起新操作，都可能和已经发生的副作用冲突。

**问题一：丢失确认**

最常见的场景是：

```text
client -> create order
server -> order created
server -> response lost
client -> timeout
```

业务已经成功，但成功确认丢了。客户端如果把 timeout 当失败处理，可能会：

- 再创建一个订单。
- 提示用户重新提交。
- 启动退款或取消流程。
- 把本地状态标记为失败。
- 在上游系统里记录错误审计。

这类问题本质是 lost acknowledgement。响应丢了，不代表执行没发生。

**问题二：重复执行**

如果客户端超时后立即重试，而服务端第一次请求还在执行，就会变成并发重复：

```text
t=0    request A arrives
t=500  client timeout
t=501  client retries request B
t=700  request A commits
t=800  request B also commits
```

如果没有幂等 key 或业务唯一约束，结果可能是：

- 创建两条订单。
- 扣两次款。
- 发两张券。
- 发送两封确认邮件。
- 同一事件被下游处理两次。

这也是为什么幂等表通常要在业务执行前先占位，而不是执行完再记录。

**问题三：取消与提交竞争**

客户端超时后可能发送取消请求：

```text
create payment timeout
client sends cancel payment
server completes original payment
cancel arrives later
```

这会产生很难解释的状态：

```text
client thought: payment canceled
server state: payment succeeded
external provider: money captured
```

取消不是简单的反向操作。对于已经越过提交点的请求，取消可能只能变成补偿，例如退款，而不是撤销原操作。

**问题四：跨系统状态不一致**

一个请求往往不只写一个地方：

```text
write database
publish message
call payment provider
update cache
send email
```

超时后服务端继续执行，如果中间没有事务边界和恢复机制，就可能出现：

- 数据库已提交，消息没发出去。
- 消息发出去了，数据库事务回滚。
- 支付成功了，本地订单仍是 pending。
- 缓存更新了，主库没更新。
- 邮件发了，但业务状态后来失败。

这类问题不能只靠“不要超时”解决。超时一定会发生，系统必须定义提交点、幂等点和补偿点。

**问题五：客户端本地状态与真实状态漂移**

客户端或上游系统可能会把 timeout 记成失败：

```text
local status = failed
server status = succeeded
```

之后用户刷新页面、查询账单或收到通知时，会看到互相矛盾的状态。更严重的是，上游可能基于错误状态继续执行新的业务流程。

例如：

```text
下单超时 -> 前端提示失败
服务端稍后创建订单成功
用户再次下单 -> 两个订单都成功
```

**应该如何设计**

比较稳妥的设计是把操作显式建模成状态机：

```text
accepted
running
succeeded
failed
cancel_requested
canceled
unknown
```

客户端超时后不要直接假设失败，而是查询 operation 状态：

```text
POST /transfers
Idempotency-Key: k1

timeout

GET /operations/k1
```

服务端需要保证：

- 幂等 key 和业务变更在同一事务里处理。
- 同一个 key 的重复请求返回同一个最终结果。
- 如果第一次请求还在执行，后续请求能等待、返回处理中，或返回可查询的 operation id。
- 业务提交和消息发布用 outbox 或等价机制关联起来。
- 取消只能在安全检查点生效，越过提交点后要走补偿流程。

**接口语义要讲清楚**

面向客户端时，timeout 后最好不要返回含糊语义。文档里应该明确：

```text
timeout means the client did not receive a final result.
The operation may still complete.
Retry with the same idempotency key or query operation status.
```

一句话：请求超时后服务端继续执行，会让客户端认知、服务端状态和外部副作用分裂；解决办法不是假装超时等于失败，而是用幂等 key、状态查询、事务边界和补偿流程管理未知状态。

## Q015. at-most-once、at-least-once、effectively-once、exactly-once 的区别是什么？

**回答：**

这几个术语的核心区别在于系统如何权衡丢失、重复和最终副作用。面试时要先说清楚讨论的是 delivery、processing、execution 还是 externally visible effect，否则很容易把不同层面的语义混在一起。

**对比表**

| 语义 | 是否可能丢 | 是否可能重复 | 典型实现思路 | 适用场景 |
| --- | --- | --- | --- | --- |
| at-most-once | 可能 | 不应该重复 | 先确认或先提交 offset，再处理 | 可以容忍丢失，不能接受重复 |
| at-least-once | 不应丢，前提是系统最终恢复 | 可能 | 先处理，成功后再确认或提交 offset | 大多数可靠消息系统的基础语义 |
| effectively-once | 底层可能重复 | 底层可能重复 | 幂等、去重、事务让最终效果只有一次 | 支付单、订单、消息消费等业务系统 |
| exactly-once | 理想上不丢不重 | 取决于定义 | 受控边界内用事务、幂等 producer、offset 原子提交等保证 | 流处理、日志系统内部状态转换 |

**at-most-once**

at-most-once 的意思是最多执行一次，宁可丢，不要重复。

典型流程是：

```text
receive message
commit offset or ack first
process message
```

如果 `commit offset` 成功后进程崩溃，消息不会再被消费，业务处理也没有完成，所以会丢。

它适合：

- 低价值指标。
- 心跳。
- 可以被后续采样覆盖的数据。
- 重复比丢失更糟的操作。

但它不适合订单、支付、库存这类关键业务。

**at-least-once**

at-least-once 的意思是至少执行一次，尽量不丢，但可能重复。

典型流程是：

```text
receive message
process message
commit offset or ack
```

如果业务处理成功后、提交 offset 前进程崩溃，消息会被重新投递。于是业务逻辑可能执行两次。

这类语义很常见，因为它比 at-most-once 更容易满足“别丢数据”。代价是业务必须处理重复。

**effectively-once**

effectively-once 指底层可能重试、重放、重复执行，但最终对业务可见的效果只有一次。

例如：

```text
message delivered twice
consumer handles twice
business table has unique(order_id)
second insert becomes no-op or returns existing result
```

从消息投递角度看，它不是 exactly-once；从业务效果看，它像一次。

effectively-once 通常靠这些机制组合出来：

- 幂等 key。
- 去重表。
- 业务唯一约束。
- 事务。
- outbox。
- consumer offset 和业务状态绑定提交。
- 可重放但确定性的处理逻辑。

很多业务系统真正需要的是 effectively-once，而不是证明某段代码只运行过一次。

**exactly-once**

exactly-once 这个词必须问清楚边界。

可能有三种含义：

```text
exactly-once delivery: 消息只投递一次
exactly-once execution: 处理函数只运行一次
exactly-once effect: 最终可见副作用只发生一次
```

严格的 exactly-once execution 在分布式系统里很难承诺，因为网络超时、进程崩溃、响应丢失和重试都会让调用方无法判断对方是否已经执行。

Kafka 文档里常说的 exactly-once 语义更接近 exactly-once processing：幂等 producer 确保 producer 重试不会在日志中写入重复消息，事务可以把消费 offset 和输出消息放在一个原子边界里提交。它不表示用户代码、外部 HTTP 调用或数据库写入天然只执行一次。

**面试回答的关键**

不要只背定义，最好补一句边界：

```text
at-most-once avoids duplicates by accepting loss.
at-least-once avoids loss by accepting duplicates.
effectively-once allows duplicate attempts but makes final business effect idempotent.
exactly-once must specify the boundary, otherwise it is an ambiguous promise.
```

一句话：at-most-once 牺牲不丢，at-least-once 牺牲不重，effectively-once 用幂等和去重把重复尝试收敛成一次业务效果，exactly-once 必须限定在明确系统边界内讨论。

## Q016. 为什么分布式 exactly-once 很难？

**回答：**

分布式 exactly-once 难，是因为调用方在失败时无法可靠区分“对方没执行”“对方执行了但响应丢了”“对方正在执行”。只要存在网络延迟、进程崩溃、重试和外部副作用，精确一次执行就很难在开放边界内成立。

**难点一：失败状态不可观察**

最核心的问题是失败后的不确定性：

```text
client sends request
server executes operation
server crashes before response
client sees timeout
```

客户端不知道服务端到底执行到了哪里：

- 请求是否到达？
- 服务端是否开始执行？
- 事务是否提交？
- 外部系统是否已经产生副作用？
- 响应是否只是丢在网络里？

如果客户端不重试，可能丢操作；如果重试，可能重复执行。

**难点二：网络不能证明“对方没有做”**

在异步网络里，超时只能说明“我等不到”，不能说明“对方没做”。消息可能延迟，响应可能丢失，节点可能只是慢，不一定死。

这就是 exactly-once 比 at-least-once 难得多的原因。at-least-once 可以选择重试，把不确定性变成重复；exactly-once 想同时避免丢失和重复，就必须额外引入持久化身份、去重状态和原子提交。

**难点三：副作用不都在同一个事务边界里**

如果所有状态都在一个数据库里，可以用唯一约束和事务处理很多问题。但真实系统经常跨越多个边界：

```text
write local database
publish Kafka message
call payment provider
send email
write cache
```

这些副作用通常不能放进同一个 ACID 事务。数据库提交成功后，调用外部支付失败怎么办？支付成功后，本地提交失败怎么办？邮件发送成功后，事务回滚怎么办？

一旦副作用跨系统，exactly-once execution 基本会退化成：

- 至少一次尝试。
- 用业务唯一键去重。
- 用事务 outbox 恢复消息发布。
- 用补偿流程修正不可回滚副作用。

**难点四：去重状态本身也会失败**

exactly-once 往往需要去重表或幂等状态：

```text
(scope, idempotency_key) -> result
```

但这个状态也会遇到问题：

- 去重表写入成功，业务写入失败。
- 业务写入成功，去重表写入失败。
- 去重状态过期后，旧请求再次出现。
- 主从切换丢了最近写入的去重 key。
- 多区域并发写入同一个 key。

如果去重状态不能和业务状态原子维护，exactly-once 的承诺就会漏。

**难点五：重试需要 fencing**

在分布式系统里，旧 leader、旧 producer、旧 worker 可能在超时后继续工作。新的实例接管后，如果没有 fencing，两个实例可能同时写。

Kafka transactional producer 使用 `transactional.id` 和 producer epoch 这类机制来隔离旧 producer。epoch 的作用可以理解为：

```text
new producer instance gets a higher epoch
old producer with lower epoch is fenced off
```

这类机制说明 exactly-once processing 不是简单“加个重试”就能得到，而是要在协议层处理旧实例、重复写和事务边界。

**难点六：保留窗口无法无限大**

去重状态不能无限保存。idempotency key、消息 offset、事务元数据、operation status 都有保留成本。可是一旦清理过早，旧请求或旧消息又可能回来。

因此系统经常只能承诺：

```text
within retention window, duplicates are suppressed
outside retention window, behavior depends on business identifiers and source retention
```

这比“全时间范围 exactly-once”弱，但更真实。

**更准确的说法**

严格讲，很多系统不是保证“代码只执行一次”，而是保证在某个受控边界内：

- 重复 producer 写入不会形成重复日志记录。
- 消费 offset 和输出结果原子提交。
- 重放后状态机仍然得到同一个结果。
- 外部可见业务状态只改变一次。

这就是为什么工程上更常说 exactly-once processing、effectively-once 或 idempotent processing。

一句话：分布式 exactly-once 难在失败不可观察、网络不可靠、副作用跨事务边界、去重状态也会失败，以及旧实例需要 fencing；工程上通常靠幂等、去重、事务、offset 原子提交和补偿，把重复尝试收敛成一次可见效果。

## Q017. 为什么很多系统实际提供的是 exactly-once processing 而不是 exactly-once execution？

**回答：**

因为系统可以控制自己的日志、状态和提交协议，却很难控制用户代码是否运行过几次。exactly-once processing 关注“处理结果是否只生效一次”，exactly-once execution 关注“处理函数是否只运行一次”。后者在崩溃、超时和重放下很难保证。

**execution 和 processing 的差别**

可以先把两个词拆开：

```text
execution: 某段代码、某个 handler、某次函数调用实际运行
processing: 输入、状态、输出在系统定义边界内完成一次一致转换
```

例如一个流处理任务：

```text
read message M
update state S
write output O
commit offset
```

如果 worker 在 `write output O` 后、`commit offset` 前崩溃，恢复后可能重新读取 M。处理函数会再运行一次，但系统可以通过事务或去重保证最终只留下一个 O，并且 offset 与 O 一起提交。

这就是 exactly-once processing，而不是 exactly-once execution。

**为什么不能承诺 handler 只运行一次**

handler 只运行一次需要系统在所有失败点上都知道“它是否已经运行”。但崩溃和超时会切断观察：

```text
worker starts handler
handler writes external side effect
worker crashes before checkpoint
system restarts worker
message is replayed
handler runs again
```

如果系统不重放，可能丢消息；如果重放，handler 可能再跑。为了不丢，很多系统选择重放，再要求处理逻辑可幂等，或把输出纳入事务边界。

**Kafka 的边界**

Kafka producer 的 idempotence 保证同一个 producer 在重试时不会因为网络错误把同一批消息重复写入日志。Kafka transactions 进一步允许把多条输出记录以及消费 offset 放入同一个事务语义里。

这解决的是 Kafka 内部日志和消费位置的一致性问题：

```text
consume input records
produce output records
commit consumed offsets
```

它不自动保证下面这些外部行为只执行一次：

- HTTP 调用外部支付网关。
- 写入不参与同一事务的数据库。
- 发送邮件或短信。
- 修改第三方 SaaS 状态。

如果用户代码在事务外做了这些副作用，exactly-once processing 的边界就被打破了。

**为什么 processing 更现实**

processing 可以通过系统协议收敛：

- 输入 offset 可持久化。
- 输出日志可事务提交。
- 状态存储可 checkpoint。
- producer 可用 epoch fencing。
- 重复消息可由序列号或事务 ID 识别。
- 崩溃后可以从 checkpoint 恢复并重放。

execution 则包含更多不可控因素：

- 用户代码可能有外部副作用。
- 函数可能在崩溃前执行了一半。
- 外部系统不支持同一个事务。
- 响应丢失后无法知道对方是否生效。
- 运行时通常不能安全地强行中断业务代码。

所以系统会说：

```text
we provide exactly-once processing within this dataflow boundary
```

而不是：

```text
your callback will physically execute exactly once
```

**面试里怎么讲清楚**

可以用一句话区分：

```text
Exactly-once execution is about attempts.
Exactly-once processing is about committed state transitions.
```

然后补边界：

```text
Kafka/Flink-like EOS can protect records, offsets and managed state inside their protocol.
It cannot make arbitrary external side effects execute exactly once unless those side effects join the same transactional or idempotent protocol.
```

**对业务系统的启发**

如果一个系统宣称 exactly-once，要继续追问：

- exactly-once 的对象是消息、状态、输出，还是业务副作用？
- 边界是否包含外部数据库？
- 是否包含第三方 API？
- offset 和业务状态是否原子提交？
- 崩溃恢复后是否会重放 handler？
- 重放时业务代码是否幂等？

这比争论术语更有价值。

一句话：很多系统能保证的是受控边界内的状态转换只提交一次，而不是用户处理函数只运行一次；所以它们提供 exactly-once processing，而不是开放世界里的 exactly-once execution。

## Q018. 去重表如何设计索引和保留策略？

**回答：**

去重表的核心是用一个稳定作用域内的唯一键，把重复请求映射到同一条操作记录。索引要保证并发下只能有一个执行者，保留策略要保证 key 的生命周期覆盖所有可能重试和重放窗口。

**典型表结构**

一个通用的幂等或去重表可以这样设计：

```sql
CREATE TABLE idempotency_records (
  tenant_id        text        NOT NULL,
  operation        text        NOT NULL,
  idempotency_key  text        NOT NULL,
  payload_hash     text        NOT NULL,
  status           text        NOT NULL,
  result_ref       text        NULL,
  error_code       text        NULL,
  created_at       timestamptz NOT NULL,
  updated_at       timestamptz NOT NULL,
  expires_at       timestamptz NOT NULL,
  locked_until     timestamptz NULL,
  fencing_token    bigint      NOT NULL DEFAULT 0,
  PRIMARY KEY (tenant_id, operation, idempotency_key)
);
```

字段含义：

| 字段 | 作用 |
| --- | --- |
| `tenant_id` | 多租户隔离，避免不同租户 key 冲突 |
| `operation` | 区分不同 API 或业务动作 |
| `idempotency_key` | 客户端或上游传入的去重键 |
| `payload_hash` | 检测同一个 key 下 payload 是否变化 |
| `status` | `processing/succeeded/failed/expired` 等状态 |
| `result_ref` | 指向业务结果，不一定存完整响应 |
| `expires_at` | 清理依据 |
| `locked_until` | 防止执行者崩溃后永久占位 |
| `fencing_token` | 防止旧执行者恢复后继续提交 |

**唯一索引怎么设计**

最重要的是唯一约束：

```sql
PRIMARY KEY (tenant_id, operation, idempotency_key)
```

这个索引回答的是：同一个租户、同一种操作、同一个 key，只能有一条记录。

不要只对 `idempotency_key` 单列唯一，除非 key 是全局强唯一。否则不同租户、不同 API 或不同业务域可能互相冲突。

常见作用域有：

```text
(tenant_id, operation, idempotency_key)
(account_id, api_name, idempotency_key)
(consumer_group, topic, partition, message_key)
(business_type, business_id)
```

消息消费侧也可以按来源建唯一键：

```sql
CREATE UNIQUE INDEX uq_processed_message
ON processed_messages (consumer_group, topic, partition, message_id);
```

如果消息没有稳定 `message_id`，可以用 `(topic, partition, offset)`，但要注意 offset 只在具体 topic-partition 内有意义。

**辅助索引怎么设计**

除了唯一索引，通常还需要：

```sql
CREATE INDEX idx_idempotency_expire
ON idempotency_records (expires_at);

CREATE INDEX idx_idempotency_status_updated
ON idempotency_records (status, updated_at);
```

`expires_at` 用于后台清理，`status + updated_at` 用于扫描长时间卡在 `processing` 的记录。

如果数据量很大，可以按时间或租户分区：

```text
partition by expires_at day/week
partition by tenant_id hash
partition by operation
```

分区的目标不是炫技，而是让清理和热点隔离更可控。

**请求处理流程**

去重表应该在业务执行前写入占位记录：

```text
begin transaction
insert idempotency record(status=processing)
if insert succeeds:
    execute business mutation
    update idempotency record(status=succeeded, result_ref=...)
commit
```

如果插入遇到唯一冲突：

```text
load existing idempotency record
compare payload_hash
if payload differs:
    return idempotency key reuse with different payload
if status=succeeded:
    return existing result
if status=processing:
    return in-progress / wait / retry later
if status=failed:
    return cached failure or allow retry based on policy
```

Stripe 的幂等设计里有一个重要点：重复请求参数必须和第一次一致，否则应该报错，避免同一个 key 被误用于不同操作。

**是否存完整响应**

不一定。可以按业务选择：

| 存储方式 | 优点 | 缺点 |
| --- | --- | --- |
| 存完整响应 | 重复请求可原样返回 | 表膨胀，隐私和 schema 演进麻烦 |
| 存 `result_ref` | 轻量，指向业务资源 | 需要二次查询 |
| 存状态和摘要 | 成本低 | 不能完全复现第一次响应 |
| 存错误码 | 能稳定处理重复失败 | 要定义哪些失败可缓存 |

支付、订单这类系统通常更倾向存业务资源 ID 和状态，而不是把完整 HTTP response 长期塞进去。

**保留时间怎么定**

保留时间不能随便拍脑袋。它至少要覆盖：

- 客户端最大重试窗口。
- 服务端异步执行最长时间。
- 消息系统的保留时间或可重放窗口。
- 跨区域复制延迟。
- 业务对账和审计窗口。
- 上游可能重复投递的最长时间。

例如：

```text
client retry window = 24h
message retention = 7d
audit replay window = 30d
```

如果去重表只保留 24 小时，而消息可能 7 天后重放，那么第 3 天重放的消息就可能被当成新消息处理。

所以保留策略要和源系统保留策略对齐。

**删除策略**

后台 GC 可以按 `expires_at` 删除，但要注意：

- 不删除 `processing` 且未超出最大执行时间的记录。
- 清理前确认没有 reader、snapshot、replay 仍依赖这些 key。
- 对高价值业务可以先转冷存或审计表。
- 删除最好分批，避免长事务和索引抖动。
- 清理任务要有速率限制。

有些系统会保留 tombstone：

```text
key -> expired tombstone
```

这样可以在一段额外窗口内识别“旧 key 已经过期”，而不是完全像没见过一样。

**常见错误**

- key 没有作用域，导致跨租户冲突。
- 先执行业务再写去重表，崩溃后无法识别重复。
- 只查后插，没有唯一约束，并发下双写。
- payload 不做 hash，重复 key 被误用于不同请求。
- TTL 小于消息保留时间，重放后重复生效。
- 把所有响应大对象塞进去，导致表和索引快速膨胀。
- 去重表和业务表不在同一事务，造成一边成功一边失败。

一句话：去重表要用带作用域的唯一索引保证并发互斥，用 payload hash 防止 key 误用，用状态字段表达处理中和完成结果，并让保留时间覆盖所有可能重试、重放和审计窗口。

## Q019. 去重状态丢失会造成什么后果？

**回答：**

去重状态丢失会让系统忘记“哪些请求已经处理过”。对 at-least-once 系统来说，这通常意味着重复投递会重新生效，effectively-once 语义被破坏。

**直接后果：重复副作用**

如果去重表丢了，历史重复请求会被当成新请求：

```text
request key = k1
first execution succeeded
dedup record lost
retry with key = k1
server treats it as new
business mutation runs again
```

可能产生：

- 重复扣款。
- 重复创建订单。
- 重复发货。
- 重复发送短信或邮件。
- 重复消费优惠券。
- 重复增加计数器。
- 重复向下游发布事件。

如果业务表没有自己的唯一约束，问题会直接暴露成脏数据。如果业务表有唯一约束，请求可能变成冲突错误，但至少不会重复生效。

**间接后果：客户端语义变化**

幂等 API 的契约通常是：

```text
same key + same payload => same result
same key + different payload => mismatch error
```

去重状态丢失后，服务端无法知道第一次 payload 是什么，也无法返回第一次结果。于是同一个 key 的重试可能得到一个全新的结果，客户端会发现 API 契约被破坏。

例如：

```text
POST /orders key=k1 payload=A -> order_1
dedup state lost
POST /orders key=k1 payload=A -> order_2
```

这比简单失败更危险，因为客户端以为自己在安全重试。

**运行中状态丢失会更麻烦**

如果丢的是 `processing` 状态，会出现并发执行：

```text
worker A has started operation k1
dedup row for k1 is lost
worker B receives retry k1
worker B starts another execution
```

这会造成 split-brain 式的业务执行。两个 worker 都认为自己有资格提交。如果没有 fencing token 或业务唯一约束，最终可能双写。

**不同丢失范围的影响**

| 丢失范围 | 后果 |
| --- | --- |
| 丢最近几秒 | 超时重试和快速重放最危险 |
| 丢一个分区 | 某些租户或 topic 的重复保护失效 |
| 丢全部去重表 | 所有历史 key 都可能被当成新请求 |
| 丢 payload_hash | 无法检测 key 被不同 payload 复用 |
| 丢 status/result | 无法返回第一次执行结果 |
| 丢 tombstone | 过期 key 可能被误判为从未出现 |

**对消息系统的影响**

消息消费通常是 at-least-once。消费者依赖去重表把重复消息压掉：

```text
message delivered
consumer writes business state
consumer records message_id as processed
```

如果 `processed_messages` 丢失，旧消息重放时会再次执行业务逻辑。即使 Kafka offset 没回退，也可能因为恢复、重平衡、人工 replay 或死信回灌重新看到旧消息。

**如何降低伤害**

最重要的是不要把去重表当成可随便丢的缓存。它是 correctness state。

常见保护手段：

- 去重表和业务表使用同等级备份。
- 去重状态写入 WAL 或 changelog。
- 关键业务在业务表上也放唯一约束。
- 去重 key 同时成为业务资源的自然键或外部流水号。
- 定期校验去重表与业务表的一致性。
- 恢复时从业务表、outbox、事件日志重建去重状态。
- 对清理任务做审计，避免误删未过期 key。
- 监控 dedup hit rate，突然归零要告警。

**能否从业务状态恢复**

有些系统可以恢复：

```text
order table has client_order_id
payment table has payment_request_id
outbox has event_id
```

这时可以从业务表反推：

```text
(tenant_id, operation, idempotency_key) -> resource_id
```

但如果业务表没有保存原始 idempotency key 或 payload hash，就只能恢复一部分能力。它可能能防止重复创建资源，却不能准确返回第一次响应，也不能检测 payload mismatch。

**面试里要强调的点**

去重状态不只是性能缓存。它承载语义：

```text
without dedup state, at-least-once falls back to duplicate effects
without payload hash, key reuse cannot be detected
without durable status, timeout recovery returns to unknown
```

一句话：去重状态丢失会让系统忘记已处理请求，重复投递会重新产生业务副作用；关键业务必须把去重状态当成持久一致性数据，并用业务唯一约束、WAL、备份和可重建路径兜底。

## Q020. 幂等和事务隔离级别有什么关系？

**回答：**

幂等和事务隔离级别不是同一个概念，但它们经常一起出现。幂等解决“同一个逻辑请求重复到达时，最终效果是否一致”，事务隔离解决“并发事务之间能看到什么、会不会产生并发异常”。

**先区分两个问题**

幂等关心的是重复请求：

```text
same operation + same idempotency key + same payload
should produce same business effect
```

事务隔离关心的是并发事务：

```text
transaction A and transaction B run at the same time
what can they see
what anomalies are allowed
```

两者交集在于：重复请求往往也是并发请求。比如客户端超时后立刻重试，第一次请求还没结束，第二次请求已经到了。

**只靠低隔离级别容易出 race**

一个错误实现可能是：

```sql
BEGIN;
SELECT * FROM idempotency_records
WHERE tenant_id = 't1' AND operation = 'pay' AND idempotency_key = 'k1';

-- no row found
-- execute payment

INSERT INTO idempotency_records (...);
COMMIT;
```

如果两个事务并发执行，在 `READ COMMITTED` 下都可能先看到 no row，然后都去执行支付。最后再插入去重记录时，即使有一个失败，外部支付副作用可能已经发生了。

正确做法是先用唯一约束占位：

```sql
BEGIN;
INSERT INTO idempotency_records (...)
VALUES (...)
ON CONFLICT DO NOTHING;

-- only the transaction that inserted the row may execute business mutation
COMMIT;
```

也就是说，幂等不能只靠“先查再写”，必须让数据库用唯一约束或锁参与并发控制。

**唯一约束比隔离级别更关键**

很多情况下，最重要的不是把隔离级别调到最高，而是建立正确的唯一约束：

```sql
UNIQUE (tenant_id, operation, idempotency_key)
UNIQUE (tenant_id, client_order_id)
UNIQUE (payment_provider, external_payment_id)
```

唯一约束把“同一个逻辑操作只能有一个结果”交给数据库强制执行。没有唯一约束，即使用较高隔离级别，也容易在业务逻辑里漏掉某些写路径。

**不同隔离级别下的含义**

| 隔离级别 | 对幂等实现的影响 |
| --- | --- |
| Read Committed | 可以用，但必须依赖唯一约束、`INSERT ... ON CONFLICT` 或行锁 |
| Repeatable Read | 能减少同一事务内读视图变化，但仍不能替代唯一约束 |
| Serializable | 能阻止更多并发异常，但可能出现序列化失败，需要上层重试 |

PostgreSQL 文档说明，Serializable 隔离级别会让并发事务的效果等价于某种串行执行。但这不表示业务就自动幂等。序列化失败后应用仍然要重试，而重试本身仍然需要幂等 key 来识别同一个逻辑操作。

**事务回滚不等于所有副作用回滚**

数据库事务能回滚数据库里的变更，但不能自动回滚外部副作用：

```text
call payment provider
insert local row
transaction aborts
```

支付调用可能已经成功，本地事务却回滚了。

即使在数据库内部，也有一些对象不完全跟随事务回滚。PostgreSQL 文档提到，序列对象的变化不会因为事务 abort 回滚。这个例子说明：事务隔离和事务回滚并不等于“所有可见效果都恢复到调用前”。

所以幂等设计不能只说“我有事务”。还要看副作用是否在同一事务边界内。

**推荐模式**

对写请求，常见模式是：

```text
begin transaction
insert idempotency record with unique key
create or update business resource
write outbox event
mark idempotency record succeeded with result_ref
commit
```

这个模式把三件事放在同一事务里：

- 这个 key 已经被接收。
- 业务资源已经变更。
- 后续要发布的事件已经记录。

事务提交后，如果响应丢失，客户端用同一个 key 重试，服务端可以查到结果。消息发布失败也可以由 outbox worker 后续补发。

**已经存在记录时怎么处理**

重复请求进来后，应该锁定或稳定读取已有记录：

```sql
SELECT *
FROM idempotency_records
WHERE tenant_id = $1
  AND operation = $2
  AND idempotency_key = $3
FOR UPDATE;
```

然后按状态处理：

```text
succeeded  -> return existing result
processing -> wait, return 409/202, or ask client to poll
failed     -> return cached failure or allow retry according to policy
```

如果不加锁，多个请求可能同时看到 `processing`，又各自尝试接管执行。是否需要锁取决于你的状态机设计，但必须有一个明确的并发互斥点。

**幂等 key 与事务重试**

Serializable 或乐观并发控制下，事务可能因为冲突失败，需要应用重试。这里有两个层次：

```text
database transaction retry: same logical request, retry internal transaction
client request retry: same logical operation, retry API call
```

两者都应该复用同一个幂等身份。否则事务重试或 API 重试可能变成新的业务操作。

**面试回答可以这样收束**

```text
Idempotency defines request-level semantics.
Isolation defines transaction-level concurrency visibility.
To implement idempotency under concurrency, we usually need unique constraints, row locks or serializable transactions, and the idempotency record must be committed atomically with the business mutation.
```

一句话：幂等和事务隔离互补而不是替代关系；隔离级别控制并发可见性，幂等 key 定义重复请求身份，真正可靠的实现要靠唯一约束、事务原子提交、必要的锁和外部副作用的补偿设计。

## Q021. 幂等和乐观锁如何组合？

**回答：**

幂等和乐观锁解决的是两个不同问题。幂等负责识别“这是不是同一个逻辑请求的重试”，乐观锁负责识别“资源在我读到之后有没有被别人改过”。两者组合后，系统既能安全处理超时重试，也能防止并发覆盖更新。

**先把边界分清楚**

幂等 key 的问题是：

```text
same idempotency key + same fingerprint
should return the same operation result
```

乐观锁的问题是：

```text
update resource
only if current version == expected version
```

如果只做幂等，没有乐观锁，两个不同请求可能互相覆盖：

```text
user A reads order version=7
user B reads order version=7
user A updates address, version becomes 8
user B updates amount with old version=7
```

如果只做乐观锁，没有幂等，客户端超时后重试可能把一次成功误判成冲突：

```text
request A updates version 7 -> 8, response lost
client retries same update with expected_version=7
server now sees version=8 and returns conflict
client thinks original update failed
```

这里原始请求可能已经成功了。幂等记录可以把第二次请求映射回第一次的结果。

**推荐的请求模型**

写请求里通常同时带两个东西：

```http
PATCH /orders/order_123
Idempotency-Key: k-abc
If-Match: "version-7"

{
  "shipping_address": "...",
  "expected_version": 7
}
```

服务端持久化：

```text
idempotency_key = k-abc
resource_id = order_123
expected_version = 7
payload_fingerprint = hash(canonical request)
status = processing/succeeded/failed/conflict
result_ref = order_123
result_version = 8
```

`expected_version` 必须进入 fingerprint。否则同一个 key 被拿来更新不同版本，服务端很难判断这是重试还是误用。

**事务流程**

一个比较稳的流程是：

```text
begin transaction

insert idempotency record(scope, key, fingerprint, status=processing)
if conflict on (scope, key):
    load existing record
    if fingerprint differs:
        return key reuse error
    return stored result or in-progress response

update resource
set fields = ..., version = version + 1
where id = resource_id
  and version = expected_version

if updated 1 row:
    mark idempotency record succeeded with result_version
    commit
    return success

if updated 0 rows:
    mark idempotency record conflict with observed/current version if safe
    commit
    return 409/412
```

重点在于：幂等记录和资源更新要在同一个事务边界里完成。不要先更新资源，再写幂等表。那样一旦响应丢失或进程崩溃，重试时就找不到第一次执行结果。

**响应语义怎么设计**

可以按情况返回：

| 情况 | 处理 |
| --- | --- |
| 第一次请求，version 匹配 | 执行业务更新，保存成功结果 |
| 第一次请求，version 不匹配 | 保存冲突结果，返回 `409 Conflict` 或 `412 Precondition Failed` |
| 同 key、同 fingerprint 重试 | 返回第一次保存的成功或冲突结果 |
| 同 key、不同 fingerprint | 返回 key 误复用错误，例如 `422` |
| 同资源、不同 key、旧 version | 按新的业务请求处理，通常返回乐观锁冲突 |

这里有个细节：乐观锁冲突本身也是一个结果。如果第一次请求已经判断为冲突，客户端用同一个 key 重试时，最好返回同一个冲突结果，而不是重新判断一次后给出另一个状态。

**和 DynamoDB 乐观锁的关系**

DynamoDB 的 Java mapper 支持用 `@DynamoDBVersionAttribute` 做版本号乐观锁：保存对象时版本会递增，更新或删除只有在客户端版本和表里版本匹配时才成功。如果版本不一致，会抛出条件检查失败相关错误。这个机制和 SQL 里的 `WHERE version = ?` 是同一类思想。

但 DynamoDB 文档还提醒了一个边界：global tables 使用 last writer wins 做冲突协调，乐观锁策略在这种场景下不会按单区域的直觉工作。面试里提到多区域时，要把这个限制说出来。

**常见错误**

- 只用幂等 key，不带 `expected_version`，导致旧状态更新覆盖新状态。
- 只做乐观锁，不记录幂等结果，超时重试后把成功误报成冲突。
- 同一个 key 下 payload 变了还继续执行，破坏幂等契约。
- 幂等表和业务表不在同一事务，崩溃后状态对不上。
- 冲突结果不缓存，重复请求每次得到不同错误细节。
- 多区域 last-writer-wins 下仍假设乐观锁能阻止所有并发覆盖。

**一句话**

幂等 key 负责把重试收敛到同一次逻辑操作，乐观锁负责保护资源版本不被旧读覆盖；可靠实现要把 `idempotency_key`、`fingerprint`、`resource_id` 和 `expected_version` 一起纳入同一个事务状态机。

## Q022. 幂等和 compare-and-swap 如何组合？

**回答：**

CAS 是一种条件写：只有当前值等于预期值，才把它换成新值。幂等和 CAS 组合时，CAS 保证并发正确性，幂等保证同一次 CAS 请求在超时、重试、响应丢失后不会变成新的操作。

**CAS 的基本形态**

CAS 可以写成：

```text
compare current_value with expected_value
if equal:
    set current_value = new_value
    return success
else:
    return compare_failed
```

在数据库里，它通常是：

```sql
UPDATE accounts
SET balance = balance - 100,
    version = version + 1
WHERE account_id = 'a1'
  AND version = 42;
```

受影响行数为 1，说明 CAS 成功；受影响行数为 0，说明预期版本已经过期。

**没有幂等时的歧义**

CAS 本身不能解决响应丢失：

```text
client: CAS version 42 -> 43
server: update succeeds
response: lost
client: timeout
client retries: CAS version 42 -> 43
server: compare fails because current version is 43
```

第二次返回 compare failed，但这并不表示第一次失败。它可能正是第一次成功造成的结果。

所以 CAS 的失败结果不能直接解释成“业务没有发生”。这和 Q013 里的 timeout unknown 是同一个问题。

**组合方式**

服务端应该把 CAS attempt 绑定到一个 operation id：

```text
scope = tenant/account/resource
idempotency_key = operation id
fingerprint = hash(resource_id, expected_value, new_value, mutation)
```

执行流程：

```text
begin transaction

insert idempotency record(key, fingerprint, status=processing)
if key exists:
    if fingerprint differs:
        return key reuse error
    return stored CAS result

perform CAS:
    update resource where current == expected

if CAS success:
    store result = success, new_version/value
else:
    store result = compare_failed, observed_version/value if allowed

commit
```

客户端用同一个 key 重试时，拿到第一次 CAS 的结果，而不是重新做一次 CAS。

**CAS 失败是否要缓存**

通常要缓存。原因很简单：CAS 失败也是这次逻辑请求的确定结果。

例如：

```text
request key=k1
expected_version=42
server sees current_version=43
return compare_failed
```

如果同一个 key 重试，仍然应该返回第一次的 compare_failed。客户端要基于新版本再试，应该生成新的 idempotency key：

```text
read current version=43
new request key=k2
expected_version=43
```

不要让同一个 key 在不同 expected value 上反复尝试。那会把“重试”变成“新业务意图”。

**和条件写的关系**

DynamoDB condition expression 就是常见的条件写形式。比如 `PutItem` 默认会覆盖同主键 item，如果要避免覆盖，可以加 `attribute_not_exists()`，只有目标主键不存在时才写入。条件不满足时，请求失败。

这和 CAS 的思想一致：

```text
write only if condition holds
```

幂等层则在条件写之外，再回答：

```text
if this exact conditional write was already attempted, what was its first result?
```

**哪些字段进入 fingerprint**

CAS 请求的 fingerprint 至少要覆盖：

- resource id。
- compare 条件，例如 expected version、expected value、expected revision。
- swap 内容，例如 new value、patch、delta。
- 操作类型，例如 `set_balance`、`reserve_inventory`。
- 业务作用域，例如 tenant、account、region。
- API version 或 fingerprint version。

如果 expected value 没有进入 fingerprint，同一个 key 可以被拿来比较不同版本，服务端无法区分重试和误用。

**常见错误**

- CAS 成功响应丢失后，重试返回 compare failed，客户端误以为第一次没成功。
- CAS 失败不保存结果，重复请求每次重新比较，返回结果随资源变化漂移。
- 同一个 idempotency key 被用于新的 expected version。
- 幂等表提交成功，CAS 更新失败或反过来，造成恢复困难。
- 把 CAS 当成 exactly-once execution。CAS 只能保护条件写，不保证 handler 没跑两次。

**一句话**

CAS 负责“当前状态还是不是我预期的状态”，幂等负责“这次 CAS 尝试是不是已经有结果”；组合时要把 compare 条件、swap 内容和幂等记录一起提交。

## Q023. 消息重复投递下消费者如何保证副作用幂等？

**回答：**

消费者不能假设消息只来一次。对 at-least-once 队列和日志系统来说，重复投递是正常故障恢复路径。消费者要把消息处理设计成“重复执行不会产生重复业务效果”。

**为什么会重复投递**

SQS 标准队列文档写得很直白：为了高可用，消息会存多份；如果删除消息时某个存储副本不可用，那份副本后面可能再次被收到。因此应用要设计成幂等，处理同一条消息多次也不应产生坏影响。

Kafka、RabbitMQ、Pulsar、SQS 这类系统的具体机制不同，但消费者侧问题类似：

```text
process message
commit offset / ack
```

如果处理成功后、提交 offset 或 ack 前崩溃，消息会再来。

**基本模式：inbox 或 processed_messages 表**

常见做法是在消费者本地事务里记录消息已经处理过：

```sql
CREATE TABLE processed_messages (
  consumer_group text NOT NULL,
  topic          text NOT NULL,
  partition_id   int  NOT NULL,
  message_id     text NOT NULL,
  processed_at   timestamptz NOT NULL,
  PRIMARY KEY (consumer_group, topic, partition_id, message_id)
);
```

处理流程：

```text
begin transaction

insert processed_messages(...)
if duplicate key:
    commit
    ack message
    return

apply business mutation
write outbox event if needed

commit
ack message
```

插入 `processed_messages` 和业务更新必须在同一个事务里。否则会出现“业务成功但没记录消息”或“记录消息但业务没做”的半成功状态。

**消息 ID 怎么选**

去重 ID 要来自消息的稳定身份：

| 来源 | 可用去重键 |
| --- | --- |
| Kafka | `(topic, partition, offset)` 或业务 event id |
| SQS | 业务 message id，FIFO 可用 deduplication id，但消费者仍要防副作用重复 |
| Webhook | provider event id |
| CDC/event log | source table primary key + version/LSN |
| 自研事件 | producer 生成的 event_id |

如果同一业务事件可能被重新发布到不同 topic，只靠 `(topic, partition, offset)` 不够；这时要用业务 `event_id` 或 `operation_id`。

**副作用要放进事务或 outbox**

消费者通常会做两类事：

```text
local DB mutation
external side effect
```

本地 DB 可以和 `processed_messages` 放进同一个事务。外部副作用不能直接塞进数据库事务，所以更稳的做法是：

```text
consume message
begin transaction
insert processed_messages
update business state
insert outbox(send_email, idempotency_key=...)
commit

outbox worker sends external request
records provider result
```

这样消费者崩溃后，外部副作用可以由 outbox worker 按同一个 operation id 重试。

**业务表也要有唯一约束**

只靠 processed table 还不够。高价值业务最好在业务表上也放自然唯一键：

```sql
UNIQUE (tenant_id, order_id)
UNIQUE (tenant_id, coupon_grant_id)
UNIQUE (tenant_id, payment_request_id)
```

这样即使去重表丢失，业务表仍能挡住一部分重复副作用。

**处理顺序**

ack 或 commit offset 的时机要在业务事务之后：

```text
bad:
  ack message
  update database

good:
  begin transaction
  dedup + update database + write outbox
  commit
  ack message
```

提前 ack 会造成丢消息。延后 ack 会带来重复，但重复可以靠幂等处理；这是 at-least-once 消费的基本取舍。

**重复时返回什么**

消费者收到重复消息后，通常不要报错。可以：

- 直接 ack 并跳过。
- 返回已经处理过的状态。
- 对未完成的处理中记录，等待或延迟重试。
- 对 payload 不一致但 message id 相同的情况报警。

同一个 message id 对应不同 payload，通常是 producer bug 或数据损坏，不能当普通重复处理。

**一句话**

重复投递下，消费者要用稳定消息 ID 做去重，把去重记录、业务变更和 outbox 写入同一个事务；ack 放在事务提交之后，外部副作用再用 outbox 和 provider idempotency key 继续保证幂等。

## Q024. 发送邮件、扣款、发券等外部副作用如何做幂等？

**回答：**

外部副作用不能只靠本地数据库事务解决。邮件可能已经发出，支付可能已经扣款，券可能已经写进第三方系统。幂等设计要把“本地操作 ID”和“外部系统调用结果”绑定起来，重试时沿用同一个外部身份。

**先定义业务操作 ID**

不要让每次重试都生成新的外部请求。应该先有稳定的业务 ID：

```text
payment_request_id = pay_123
email_notification_id = mail_456
coupon_grant_id = coupon_789
```

这些 ID 要进入本地表的唯一约束：

```sql
UNIQUE (tenant_id, payment_request_id)
UNIQUE (tenant_id, email_notification_id)
UNIQUE (tenant_id, coupon_grant_id)
```

然后用同一个 ID 调外部服务。如果外部服务支持 idempotency key，就把它作为 provider request id；如果不支持，就至少在本地 send log 里记录每一次尝试和最终结果。

**扣款**

支付最需要强幂等。

推荐流程：

```text
begin transaction
create local payment_request(pay_123, status=pending)
write outbox(provider=Stripe, idempotency_key=pay_123)
commit

outbox worker:
  call provider with idempotency_key=pay_123
  store provider_charge_id and final status
```

Stripe 的 API 幂等文档说明，客户端可以给 `POST` 请求传 idempotency key；服务端会保存第一次请求的状态码和响应体，后续同 key 请求返回同一结果；如果 key 被复用但参数不同，会报错。这个语义非常适合支付请求。

支付系统还要保存：

- 本地 payment request id。
- provider idempotency key。
- provider charge/payment intent id。
- 金额、币种、收款方。
- 状态机：pending、succeeded、failed、requires_action、refunded。

不要只保存“已调用过”。要能回答：外部到底返回了什么。

**发送邮件**

邮件服务不一定提供严格幂等。即便提供，也可能只在短窗口内去重。邮件场景通常用本地幂等挡住重复发送：

```sql
CREATE TABLE email_sends (
  tenant_id text NOT NULL,
  notification_id text NOT NULL,
  recipient text NOT NULL,
  template_id text NOT NULL,
  payload_hash text NOT NULL,
  status text NOT NULL,
  provider_message_id text NULL,
  PRIMARY KEY (tenant_id, notification_id)
);
```

发送前先插入 `email_sends`。如果唯一键冲突，说明这封业务邮件已经有发送记录，后续按状态处理：

```text
succeeded -> 不再发送
pending   -> 等待或接管
failed    -> 根据错误类型重试
```

邮件还有一个现实问题：即使本地认为失败，用户可能已经收到。重复发送确认邮件通常比少发一封更糟，所以要保守处理 provider timeout。最好通过 provider message id 或事件回调确认状态。

**发券**

发券看起来像普通写入，但它通常也有外部副作用：库存、预算、活动规则、用户权益。

比较稳的设计是：

```text
coupon_grant_id = campaign_id + user_id + grant_reason
```

业务表加唯一约束：

```sql
UNIQUE (tenant_id, campaign_id, user_id, grant_reason)
```

第一次请求创建 grant，重复请求返回已有 grant。发券还要在同一事务里扣减库存或预算：

```text
insert coupon_grant
update campaign_budget set remaining = remaining - 1 where remaining > 0
write outbox(coupon_granted)
```

如果第三方券系统支持外部流水号，用 `coupon_grant_id` 传过去；不支持时，本地必须保证不会发起第二次调用，必要时通过对账任务修正状态。

**为什么要用 outbox**

直接在业务事务中调用外部系统会卡在这个状态：

```text
external call succeeds
local transaction fails
```

反过来也会出问题：

```text
local transaction commits
external call times out
```

outbox 把本地事实先持久化，再由后台 worker 重试外部调用：

```text
business state + outbox event committed together
external side effect retried with stable idempotency key
```

这不是让外部调用变成数据库事务，而是让恢复路径可控。

**外部不支持幂等怎么办**

只能把重复风险压到最低：

- 本地唯一约束防止多次发起。
- 单线程或分片 worker 处理同一个 operation id。
- 记录 provider response、request body、timeout、重试次数。
- 超时后先查 provider 状态，再决定是否重试。
- 做对账任务，把 unknown 状态收敛成 succeeded 或 failed。
- 对不可逆副作用设计补偿，比如退款、撤券、作废通知。

不要在文档里承诺 exactly-once execution。更准确的承诺是：系统会尽量让同一个业务 operation 只有一个最终可见结果。

**一句话**

邮件、扣款、发券这类外部副作用要先有业务 operation id，再用本地唯一约束、outbox、provider idempotency key、状态查询和对账把超时重试收敛起来；没有外部幂等支持时，只能靠本地防重和补偿降低损害。

## Q025. 幂等 key 是否应该参与签名？

**回答：**

如果 API 使用请求签名或 HMAC 鉴权，幂等 key 应该参与签名。原因是幂等 key 会改变服务端处理语义：同一个 body，换一个 key，可能从“重复请求”变成“新操作”。

**为什么要签名**

假设签名只覆盖 body，不覆盖 `Idempotency-Key`：

```http
POST /payments
Idempotency-Key: k1
Signature: sign(body)

{"amount": 100, "currency": "USD"}
```

如果中间层、代理或错误客户端把 key 改成 `k2`，body 签名仍然通过，但服务端会把它当成新的支付请求。这破坏了签名想保护的语义。

签名应该覆盖会影响业务含义的组件：

- HTTP method。
- path 和重要 query。
- tenant/account。
- content digest 或 canonical body hash。
- `Idempotency-Key`。
- 时间戳、nonce 或 request date。
- API version。

**HTTP Message Signatures 的启发**

RFC 9421 定义 HTTP Message Signatures 时，签名基于一组被覆盖的 HTTP message components 生成 signature base。规范里有 method、authority、path、query、content-digest、content-type 等组件示例。这个模型说明了一点：不是“请求有签名”就够了，重要的是哪些组件被签名覆盖。

对幂等 API 来说，`Idempotency-Key` 就应该是被覆盖组件之一。

**和 payload fingerprint 的区别**

签名和 fingerprint 不是一回事：

| 项目 | 作用 |
| --- | --- |
| 签名 | 证明请求组件没有被未授权修改 |
| fingerprint | 判断同一个 idempotency key 下的请求是否语义一致 |
| idempotency key | 标识同一次逻辑操作 |

签名保护传输和身份语义，fingerprint 保护幂等语义。两者可以使用同一个 canonical body，但目的不同。

**是否所有系统都必须签**

如果是纯 HTTPS 内部调用、没有应用层签名，谈“参与签名”没有直接意义。但只要你已经有应用层签名机制，就应该把 idempotency key 纳入签名基底。

这样可以避免：

- key 被网关误改。
- key 被攻击者替换后重放。
- 同一个签名 body 被换 key 多次提交。
- 审计时无法证明 key 和 payload 是同一请求的一部分。

**不要把 key 当秘密**

idempotency key 不是密码。它可以是随机 UUID，也可以是业务 operation id。签名它，不代表它必须保密；签名的目的只是绑定语义，防止被篡改。

如果担心 key 泄漏导致重放，还需要：

- 短期签名时间窗口。
- nonce 或 timestamp。
- 认证主体绑定。
- 服务端 idempotency key 作用域。
- replay protection。

**一句话**

幂等 key 会决定请求是重试还是新操作；有应用层签名时，它应和 method、path、body digest、tenant、timestamp 一起参与签名，否则请求的幂等语义可能被未授权改变。

## Q026. 如何避免用户误复用幂等 key？

**回答：**

避免误复用，不能只靠文档提醒。服务端要用作用域、fingerprint 和错误语义兜住，客户端要让 SDK 或表单逻辑自动生成 key，尽量别让用户手写。

**客户端生成策略**

对大多数 API，客户端应该为每一次新的业务意图生成新 key：

```text
new checkout attempt -> new key
retry same checkout attempt -> same key
user changes amount or recipient -> new key
```

好的 SDK 会把这个行为封装掉：

```text
operation = create payment
operation id = uuid
retry policy reuses same operation id
new user submission creates a different operation id
```

不要用这些低熵或不稳定做法：

- 用户 ID 当 key。
- 当前日期当 key。
- endpoint 名称当 key。
- 订单金额当 key。
- 时间戳精确到秒。
- 前端页面固定生成一个 key 后长期复用。

这些都容易把不同业务请求压到同一个 key 上。

**服务端作用域**

服务端不要把 key 当全局单列唯一。更合理的是：

```text
(tenant_id, operation, idempotency_key)
```

或者：

```text
(merchant_id, api_name, idempotency_key)
(account_id, region, operation, idempotency_key)
```

AWS EC2 文档里把 idempotency 分成 regional 和 zonal 两类，就是在说明作用域会影响幂等语义。同一个 client token 在不同 Region 或不同 Availability Zone 下，可能是不同作用域里的请求。

**fingerprint 校验**

服务端看到同一个 key 时，不要只返回已有结果。必须比较 fingerprint：

```text
same key + same fingerprint -> retry
same key + different fingerprint -> key misuse
```

IETF Idempotency-Key 草案也写了类似语义：key 必须唯一，不应被不同 payload 复用；资源可以生成 idempotency fingerprint 来判断请求唯一性；如果同 key 搭配不同 payload，资源应该返回错误。

**错误要明确**

误复用 key 时，不建议返回含糊的 `500` 或普通业务失败。应该告诉客户端这是幂等 key 使用错误：

```http
422 Unprocessable Content
{
  "error": "idempotency_key_reused_with_different_payload",
  "idempotency_key": "k1"
}
```

如果同 key 的第一次请求还在处理，返回：

```http
409 Conflict
{
  "error": "idempotency_key_in_progress"
}
```

这样客户端知道：422 需要生成新 key 或修正 payload；409 可以稍后用同 key 查询或重试。

**前端表单场景**

前端重复提交常见于刷新、双击按钮、移动端网络抖动。

推荐做法：

- 页面加载时不要提前生成长期 key。
- 用户点击提交时生成 operation id。
- 同一次提交的自动重试复用 key。
- 用户修改金额、地址、收款方后生成新 key。
- 提交成功后清理本地 key。
- 未确认结果时保留 key，用于查询或重试。

按钮置灰只能改善体验，不能作为正确性保证。用户可以刷新页面，移动端也可能自动重发。

**服务端治理**

服务端还应该有：

- key 长度和字符集校验。
- 最小熵或 UUID 格式建议。
- per-tenant key 使用速率监控。
- key reuse mismatch 告警。
- `payload_hash` 与 `fingerprint_version` 存档。
- 清晰的 TTL 文档。

key 过期后再次出现，也不要静默当成普通新请求。高价值业务可以保留 tombstone 或依赖业务唯一键兜底。

**一句话**

避免误复用要靠 SDK 自动生成、服务端作用域约束、fingerprint 比对和明确错误语义；同一个 key 只能代表同一次业务意图，payload 或预期版本变了就应该换 key。

## Q027. idempotency fingerprint 应该覆盖哪些字段？

**回答：**

fingerprint 应该覆盖所有会改变业务语义的字段，不应该覆盖纯传输、观测或重试相关字段。原则是：同一个 key 下，两个请求如果会产生不同业务效果，它们的 fingerprint 必须不同。

**应该覆盖的字段**

通常包括：

| 字段 | 原因 |
| --- | --- |
| HTTP method 或 operation name | `POST /payments` 和 `POST /refunds` 不能混用 |
| path 中的资源 ID | `/orders/1` 和 `/orders/2` 是不同资源 |
| 重要 query 参数 | 查询或写入语义可能被 query 改变 |
| tenant/account/merchant | 防止跨租户 key 冲突 |
| canonical request body | 金额、币种、收款方、数量等业务输入 |
| expected version/revision | 乐观锁和 CAS 语义的一部分 |
| API version | 不同版本可能解释字段的方式不同 |
| content type 或 schema version | 同一字节在不同格式下可能含义不同 |
| 外部业务引用 | 如 client_order_id、payment_request_id |

如果接口支持条件请求，还要覆盖：

- `If-Match`。
- expected revision。
- compare value。
- precondition flags。

这些字段变了，请求就不再是同一次逻辑操作。

**不应该覆盖的字段**

不要把这些字段放进 fingerprint：

- `Date`。
- `User-Agent`。
- trace id。
- request id。
- retry attempt。
- `Authorization`。
- 签名值本身。
- 服务端接收时间。
- 连接信息、IP、TLS session。
- 幂等 key 本身。

这些字段会在重试时变化，但不改变业务意图。放进去会导致同一次重试被误判成不同请求。

**幂等 key 是否进入 fingerprint**

通常不需要。幂等 key 是查找去重记录的 key，fingerprint 是该记录下的请求内容摘要。表结构一般是：

```text
primary key: (scope, idempotency_key)
columns: fingerprint, status, result_ref
```

如果把 key 也放进 fingerprint，不会增加判断能力，反而容易混淆“查找维度”和“内容维度”。签名可以覆盖 idempotency key，但 fingerprint 不一定要包含它。

**raw body 还是语义字段**

有两种做法：

```text
wire fingerprint: hash(raw body bytes)
semantic fingerprint: hash(canonical business fields)
```

raw body 简单，但对 JSON 空白、字段顺序、默认值、序列化差异敏感。semantic fingerprint 更适合业务 API，但要有稳定的规范化逻辑。

例如下面两个 JSON 语义相同：

```json
{"amount":100,"currency":"USD"}
```

```json
{
  "currency": "USD",
  "amount": 100
}
```

如果使用 raw body hash，它们不同；如果使用 canonical JSON 或 schema-aware normalization，它们可以得到同一个 fingerprint。

**推荐存储内容**

去重表里不要只存 hash，最好存：

```text
fingerprint_hash
fingerprint_algorithm
fingerprint_version
canonical_payload_ref or small canonical snapshot
request_content_length
api_version
created_at
```

只存 hash 会让排障困难，也不方便区分 key 误复用和极小概率 hash 碰撞。

**一句话**

fingerprint 要覆盖业务语义，不覆盖重试噪声；method、resource、tenant、body、条件字段、API version 通常要进来，trace、时间、签名值、retry attempt 通常不进来。

## Q028. 序列化格式变化会不会影响 fingerprint？

**回答：**

会。如果 fingerprint 基于原始字节或某个语言的默认序列化输出，序列化格式变化很容易让同一个业务请求得到不同 fingerprint。解决办法是使用稳定的规范化格式，并给 fingerprint 算法做版本管理。

**常见变化**

JSON 请求里这些变化都可能影响 hash：

```text
字段顺序变化
空白和换行变化
数字 1.0 变成 1
字符串转义方式变化
null 字段被省略
默认值从缺省变成显式写出
map 遍历顺序变化
时间格式从毫秒变成微秒
字段重命名或 API version 变化
```

如果 fingerprint 是 `SHA256(raw_body)`，上面任何变化都可能改变结果。

**什么时候用 raw body hash**

raw body hash 适合这些场景：

- 协议要求字节级一致。
- 请求内容本来就是不可解释的二进制对象。
- 签名或内容摘要要保护 wire representation。
- 客户端和服务端都明确把“同一请求”定义为同一字节序列。

例如文件上传，fingerprint 可以覆盖文件内容 digest，而不是服务端重新解释后的对象。

**业务 API 更适合 canonical representation**

对 JSON/Protobuf 这类结构化 API，更推荐先构造 canonical request：

```text
canonical = normalize(method, path, tenant, api_version, business_body)
fingerprint = SHA256(canonical)
```

RFC 8785 的 JSON Canonicalization Scheme 就是为 hashing/signing 这类场景设计的。它要求 JSON 以稳定形式表示，包括不输出多余空白、确定性排序对象属性、按规范序列化字符串和数字，最后生成 UTF-8 字节。

如果你的 API 使用 JSON，并且需要跨语言稳定 fingerprint，可以参考 JCS 的思路。

**schema-aware normalization**

有些场景只靠通用 JSON canonicalization 还不够。业务层还需要处理默认值：

```json
{"amount":100}
```

和：

```json
{"amount":100,"currency":"USD"}
```

如果 `currency` 默认就是 USD，它们语义可能相同。但 JCS 不会知道业务默认值。你需要在 fingerprint 前做 schema-aware normalization：

```text
apply defaults
remove unknown ignored fields
normalize enum casing if API allows it
normalize decimal and timestamp
sort unordered sets
then canonicalize
```

这一步要谨慎。只有 API 文档明确允许等价，才能合并。

**算法版本很重要**

fingerprint 逻辑一旦上线，就会进入持久状态。后续改算法时，旧记录还要能比较。

表里应该存：

```text
fingerprint_version = v1
fingerprint_algorithm = sha256+jcs+api_2026_01
fingerprint_hash = ...
```

服务端处理重复请求时，要用旧记录的 version 重新计算或兼容比较，而不是直接用最新算法覆盖旧语义。

**API version 是否应该进入 fingerprint**

通常应该。因为同一个 JSON 字段在不同 API version 下可能含义不同：

```text
api_version=2025-01: amount is cents
api_version=2026-01: amount is decimal string
```

如果 API version 不进 fingerprint，同一个 key 可能跨版本复用，结果很难解释。

**一句话**

序列化变化会影响 fingerprint，尤其是 raw body hash；稳定做法是先做 schema-aware normalization，再用 canonical JSON 或等价规范生成字节，并把 fingerprint 算法和 API version 一起存下来。

## Q029. 如何处理浮点数、map 顺序、时间戳导致的 fingerprint 不稳定？

**回答：**

这三类问题都来自同一个根源：业务上看起来相同的数据，序列化后可能长得不一样。处理办法是先规范化，再 fingerprint。不要把语言运行时的默认输出当成协议。

**浮点数**

金额、库存、积分这类字段不要用 float 做 fingerprint 输入。更稳的做法是：

```text
money: integer minor units, e.g. cents
ratio: decimal string with fixed scale
quantity: integer or fixed precision decimal
```

例如：

```json
{"amount_cents": 1000, "currency": "USD"}
```

比下面这种稳定得多：

```json
{"amount": 10.0}
```

如果业务必须接收小数，要先规范化：

```text
parse decimal
validate scale
round or reject according to business rule
serialize as canonical decimal string
```

不要让 `10`、`10.0`、`10.00` 在不同客户端里随意漂移。

RFC 8785 也提醒了 JSON number 的边界：JCS 基于 ECMAScript 的 number 序列化，数字要能以 IEEE 754 double 表示；需要更高精度或更长整数时，建议用 JSON string 表示。对金融业务来说，这个建议很实用。

**map 顺序**

很多语言的 map 遍历顺序不稳定：

```json
{"a":1,"b":2}
```

和：

```json
{"b":2,"a":1}
```

业务上相同，raw hash 不同。解决办法是递归排序 object key：

```text
sort object keys recursively
keep array order
```

数组不要随便排序，因为数组顺序通常有业务含义。

如果某个字段语义上是集合，而不是列表，要在 schema 里标出来，然后对集合元素排序：

```text
tags: unordered set -> sort
line_items: ordered list -> do not sort
```

不要让通用 canonicalizer 自己猜。

**时间戳**

时间戳常见问题更多：

```text
2026-06-16T10:00:00+08:00
2026-06-16T02:00:00Z
2026-06-16T02:00:00.000Z
2026-06-16 02:00:00 UTC
```

它们可能表示同一时刻。fingerprint 前应该统一成：

```text
UTC
固定格式
固定精度
无本地时区
无单调时钟部分
```

例如：

```text
2026-06-16T02:00:00.000Z
```

精度也要写清楚。如果业务只接受毫秒，就不要有的客户端传微秒、有的传纳秒。

**不要包含服务端生成时间**

这些字段不要进入 fingerprint：

```text
received_at
processed_at
server_now
trace_start_time
retry_time
```

它们每次重试都会变。它们应该进日志，不应该决定“这是不是同一次业务请求”。

**null、缺省值和字符串**

还要处理几个细节：

- `null` 和字段缺失是否等价，要由 schema 定义。
- 默认值要在 fingerprint 前统一填充或统一删除。
- Unicode 是否做 normalization 要写进规范。
- 大小写是否敏感，不能靠客户端猜。
- `NaN`、`Infinity` 这类值应该拒绝，不要参与 fingerprint。

JCS 对 Unicode 字符串的处理选择是保持原样，并不自动做 Unicode normalization。你的业务如果要把不同 Unicode 表示归一化，需要在 fingerprint 前显式做，而且要所有客户端和服务端一致。

**测试怎么做**

fingerprint 稳定性要做跨语言测试：

```text
Java client payload
Go client payload
JavaScript client payload
Python client payload
```

同一个语义请求应该得到同一个 canonical bytes 和 hash。不同语义请求必须得到不同 hash。

**一句话**

浮点数用整数或 decimal string，map 递归排序，时间戳统一 UTC 和精度；所有规范化规则都要 schema 化、版本化，并用跨语言测试验证。

## Q030. 幂等 key 冲突和 hash 碰撞如何区分？

**回答：**

幂等 key 冲突是同一个 key 被用于不同逻辑请求；hash 碰撞是不同请求经过 hash 后得到同一个 digest。前者是常见使用错误，后者在使用现代密码学 hash 时极少见，但高价值系统仍应保留可诊断证据。

**先看四种情况**

| 情况 | 含义 | 处理 |
| --- | --- | --- |
| same key + same fingerprint | 正常重试 | 返回第一次结果 |
| same key + different fingerprint | key 被误复用 | 返回 mismatch，例如 `422` |
| different key + same fingerprint | 两个 key 表达相同业务内容 | 通常按两次请求处理，业务唯一约束另行判断 |
| same key + same hash 但 canonical payload 不同 | 可能是 hash 碰撞、canonical bug 或存储损坏 | 拒绝请求并报警 |

面试里最容易混淆的是第二种和第四种。大多数线上问题都是 key 冲突，不是 hash 碰撞。

**key 冲突**

key 冲突通常长这样：

```text
key = k1
first request:  amount=100, currency=USD
second request: amount=200, currency=USD
```

服务端查到 `(scope, k1)` 已存在，再比较 fingerprint，发现不同。此时应该返回 key misuse，而不是把它当成新请求。

AWS EC2 对类似情况有明确语义：同一个 client token 的成功请求如果用不同参数重试，会返回 `IdempotentParameterMismatch`。IETF Idempotency-Key 草案也建议同 key 不同 payload 返回错误。

**hash 碰撞**

hash 碰撞是：

```text
canonical_payload_A != canonical_payload_B
hash(A) == hash(B)
```

如果使用 SHA-256 这类密码学 hash，随机碰撞概率可以忽略到工程上近乎不可见。但“近乎不可见”不等于排障时可以不留证据。

高价值系统建议存：

```text
fingerprint_hash
fingerprint_algorithm
fingerprint_version
canonical_payload_length
canonical_payload_ref or encrypted snapshot
```

这样如果出现同 hash 但内容不同，可以判断是：

- 真正 hash 碰撞。
- canonicalization 代码 bug。
- 存储损坏。
- 旧版本算法兼容问题。
- 攻击性构造输入。

只存 hash，后面就很难查。

**不同 key、相同 fingerprint 怎么办**

这不是幂等 key 冲突。可能是用户真的发了两次相同请求，但每次生成了不同 key：

```text
key=k1, create order amount=100
key=k2, create order amount=100
```

幂等层通常会把它们当两次不同业务意图。是否阻止重复创建，要看业务唯一约束：

```text
client_order_id unique
cart_checkout_id unique
payment_request_id unique
```

不要把 fingerprint 当全局去重键随便拦请求。两个不同客户买同一个商品，fingerprint 可能很像，但业务上应该都成功。

**随机 key 碰撞**

还有一种情况是 idempotency key 本身碰撞。比如两个不同操作碰巧生成了同一个 UUID。概率很低，但服务端处理方式和误复用一样：

```text
same scope + same key + different fingerprint -> mismatch
```

服务端无法判断这是随机碰撞还是客户端复用了 key，也不需要猜。返回明确错误，让客户端生成新 key。

**安全处理策略**

建议规则：

```text
if key not found:
    create record
else:
    if fingerprint hash differs:
        return 422 key reused with different request
    if hash same and canonical snapshot same:
        return stored result
    if hash same but canonical snapshot differs:
        return 500/409 class error, freeze key, alert security
```

对支付、发券、权限变更这类操作，遇到疑似碰撞或 canonical bug 时不要继续执行。先冻结 key，再人工或自动安全流程处理。

**一句话**

同 key 不同 fingerprint 是 key 冲突，按客户端误用处理；同 hash 不同 canonical payload 才可能是 hash 碰撞或规范化 bug。工程上用强 hash 降低碰撞概率，用 canonical snapshot、算法版本和长度信息保留排障能力。

## Q031. 去重缓存被击穿时会发生什么？

**回答：**

去重缓存被击穿，指大量请求没有命中缓存，直接打到后面的去重表、业务库或外部服务。它的麻烦不只是性能下降。对写请求来说，如果系统把缓存当成唯一防重机制，缓存击穿会直接变成重复执行风险。

**典型结构**

很多系统会这样做：

```text
request with idempotency key
  -> check dedup cache
  -> if hit, return cached result
  -> if miss, read/write dedup table
  -> execute business mutation
```

这个设计没问题，但前提是缓存只是加速层。真正的互斥点应该在持久层：

```text
UNIQUE(scope, idempotency_key)
```

如果没有这个唯一约束，缓存一旦失效，并发请求就可能一起穿透。

**会发生什么**

最常见的链路是：

```text
t=0   热点 key 或大批 key 的缓存过期
t=1   重试请求同时进入
t=2   dedup cache miss
t=3   所有请求打到 dedup DB
t=4   DB 连接池、行锁、唯一索引开始排队
t=5   上游等待变长，触发 timeout
t=6   timeout 又触发更多 retry
```

结果会变成：

- 去重表 QPS 暴涨。
- 同一个 key 的行锁竞争加剧。
- 唯一索引冲突大量出现。
- 业务库连接池被占满。
- 请求延迟升高，客户端开始重试。
- 重试请求继续击穿缓存。
- 服务端还没返回，客户端已经发起下一轮。

如果后端还有外部副作用，比如扣款、发券、发邮件，风险更大。一次缓存 miss 不该绕过去重表直接执行业务。

**缓存击穿和缓存雪崩的区别**

可以这样区分：

| 现象 | 含义 |
| --- | --- |
| 缓存穿透 | 请求的 key 本来就不存在，持续打到后端 |
| 缓存击穿 | 热点 key 过期，大量请求同时打到后端 |
| 缓存雪崩 | 大量 key 同时过期或缓存集群故障，后端被整体打穿 |

去重场景里，这三种都会出现。比如攻击者构造大量随机幂等 key，是穿透；某个支付请求的 key 处于处理中，被大量重试打穿，是击穿；整个 Redis 集群故障，是雪崩。

**缓存不能承担正确性**

错误设计：

```text
if cache miss:
    execute business mutation
    set cache key
```

并发下两个请求都可能 miss，然后都执行业务。

更稳的设计：

```text
if cache hit:
    return cached status/result

begin transaction
insert dedup record(scope, key, fingerprint, status=processing)
if unique conflict:
    load existing durable record
    refresh cache
    return existing status/result

execute business mutation
update dedup record(status=succeeded, result_ref=...)
commit
refresh cache
```

缓存挂了，系统最多慢；去重语义不能消失。

**如何防击穿**

常用手段有：

- 持久层唯一约束兜底。
- per-key singleflight，同一进程内同一 key 只放一个请求访问后端。
- 分布式锁或短租约，但不要替代数据库唯一约束。
- `processing` 状态也写入缓存，重复请求看到后返回 `409`、`202` 或等待。
- 热点 key 设置短 TTL 时加 jitter，避免同一时刻过期。
- 对不存在的 key 做有限的负缓存，但要小心新请求被误挡。
- dedup DB 单独连接池，避免拖垮业务主库。
- 缓存 miss rate、DB unique conflict、lock wait、retry attempt 联合告警。

**缓存不可用时怎么办**

非幂等写请求宁可变慢，也不要 fail open：

```text
cache unavailable -> go to durable dedup table
dedup table unavailable -> reject or return retryable error
```

如果去重表也不可用，直接执行业务等于把重复执行风险转给用户。支付、发券、发货这类接口更适合 fail closed，返回 `503` 或明确的 retryable unknown 状态，让客户端用同一个 key 稍后再试。

一句话：去重缓存被击穿后，后端会承受成倍重试和锁竞争；缓存只能加速，不能做唯一正确性边界，最终防线必须是持久去重记录、唯一约束和明确的处理中状态。

## Q032. 重试风暴如何触发雪崩？

**回答：**

重试风暴触发雪崩的路径通常是：下游变慢，上游超时，上游重试，下游更慢，更多请求超时，更多上游开始重试。这个反馈环一旦闭合，原本只影响一小部分请求的故障，会被放大成整个链路的资源耗尽。

**基本反馈环**

可以把它写成一条循环：

```text
downstream latency increases
-> upstream timeout increases
-> clients retry
-> downstream QPS increases
-> queue length increases
-> latency increases again
```

雪崩不是单点失败本身造成的，而是恢复策略把失败放大了。

**为什么会放大**

AWS Builders Library 给过一个很有代表性的例子：如果调用栈有 5 层，每层各自重试 3 次，最底层数据库在故障时可能承受 243 倍请求。计算很简单：

```text
3 * 3 * 3 * 3 * 3 = 243
```

每一层都觉得自己只做了 3 次重试，合起来就变成指数级放大。

**资源是怎么被耗尽的**

重试风暴会同时吃掉多种资源：

| 资源 | 被耗尽的方式 |
| --- | --- |
| 线程 | 请求等待下游，线程长时间占用 |
| 连接池 | 下游慢，连接不释放 |
| 内存 | 队列、future、request context 堆积 |
| CPU | 序列化、鉴权、错误处理、日志暴涨 |
| 数据库 | 重复查询、锁等待、事务堆积 |
| 网络 | 大量失败请求和响应占用带宽 |
| 日志系统 | 错误日志高峰反过来拖慢服务 |

等这些资源被占满，服务可能连健康检查、取消请求、限流响应都处理不了。负载均衡器看到实例不健康，会把流量转给剩余实例，剩余实例也被打垮。

**为什么 timeout 会加速雪崩**

timeout 设太低时，轻微延迟抖动就会被误判成失败。上游开始重试，下游收到更多请求，延迟继续升高。AWS 文档也提到，timeout 太低会增加后端流量和延迟，甚至让小的后端延迟上升演变成完整 outage。

timeout 设太高也有问题。请求长时间占用线程和连接，系统排队变长，后续请求被拖慢。最终还是触发 timeout。

**用户行为也会参与放大**

系统慢的时候，用户会刷新页面、重复点击、重新提交表单。浏览器、移动端 SDK、网关、服务端 RPC 客户端可能都在重试。你看到的流量不再是用户真实流量，而是：

```text
真实请求 + 客户端重试 + 网关重试 + 服务间重试 + 用户刷新
```

没有 attempt 级指标时，很容易误判成“用户流量突然涨了”。

**雪崩时的信号**

重试风暴通常会出现这些指标：

- client attempt QPS 增长快于 logical request QPS。
- timeout rate 上升早于业务错误率。
- 同一个 idempotency key 出现多次 attempt。
- 下游 p99 延迟升高后，上游 retry count 跟着升高。
- 连接池等待时间、线程池队列长度上升。
- 熔断器 open、限流 rejection、bulkhead full 同时出现。
- 成功率下降，但总处理量没有同步上升。

如果只有普通 QPS 和 error rate，发现时通常已经晚了。

**如何切断反馈环**

关键是让系统在过载时少做无效工作：

- retry 只放在调用链的一个明确层次。
- 使用总 deadline，不能每层重新给满 timeout。
- 指数退避和 jitter。
- retry budget，限制重试流量占比。
- client-side throttling，本地拒绝一部分请求。
- server-side load shedding，过载时便宜地拒绝。
- 熔断，减少对已经不健康依赖的请求。
- bulkhead，保护不同依赖和不同租户互不拖垮。
- 对非幂等写请求要求 idempotency key。

一句话：重试风暴本质是一个正反馈环，timeout 和重试把下游慢放大成更多下游流量；没有 retry budget、deadline propagation、限流和隔离，局部故障很容易变成级联雪崩。

## Q033. 如何通过限流、熔断、隔离仓控制重试风暴？

**回答：**

限流、熔断、隔离仓控制的是重试风暴的三个不同位置。限流控制进入系统和进入依赖的速率，熔断在依赖明显不健康时减少无效调用，隔离仓限制故障依赖能占用多少资源。三者要配合 retry budget 和 deadline，单独上一个通常不够。

**限流：先控制流量形态**

限流要区分原始请求和重试请求。重试不应该无限挤占正常请求。

常见做法：

```text
retry_budget = min(absolute_limit, original_requests * ratio)
```

比如：

```text
每 1000 个原始请求，最多允许 100 个 retry attempt
```

更细一点，可以按维度限：

- tenant。
- caller service。
- API operation。
- downstream dependency。
- idempotency key。
- retry reason。
- priority 或 criticality。

HTTP API 可以用 `429 Too Many Requests` 表达限流，并配合 `Retry-After` 告诉客户端等待多久。RFC 6585 对 429 的语义就是一段时间内请求太多，响应里可以带 `Retry-After`。

**本地限流比全局协调更稳**

全局限流需要中心状态，中心状态自己也可能成为故障点。Google SRE 书里介绍的 client-side throttling 思路是让客户端根据最近窗口内 attempted requests 和 backend accepts 的比例自我调节。AWS Builders Library 也提到用 token bucket 限制本地 retry，当 token 用完后按固定速率重试。

这种策略的好处是：下游开始拒绝时，上游不需要等中央控制面下发规则，就能减少发出的请求。

**熔断：减少对坏依赖的无效尝试**

熔断器通常有三种状态：

```text
CLOSED -> 正常放行
OPEN -> 直接拒绝或走降级
HALF_OPEN -> 放少量探测请求
```

Resilience4j 的 circuit breaker 用滑动窗口统计失败率和慢调用率，达到阈值后从 CLOSED 切到 OPEN；等待一段时间后进入 HALF_OPEN，允许少量请求探测依赖是否恢复。

熔断适合这些场景：

- 下游持续超时。
- 错误率明显超过阈值。
- 慢调用率升高，继续调用只会堆积。
- 外部依赖已经限流或维护。

但熔断也有代价。AWS Builders Library 提到 circuit breaker 会引入 modal behavior，测试和恢复都更复杂。所以熔断器参数要能演练，不能只在事故当天第一次走到 OPEN。

**隔离仓：限制故障扩散**

隔离仓限制的是并发资源，而不是请求速率。

例如：

```text
payment provider bulkhead: max 50 concurrent calls
email provider bulkhead: max 20 concurrent calls
coupon provider bulkhead: max 30 concurrent calls
```

Resilience4j bulkhead 的核心参数包括最大并发数和等待时间；线程池隔离仓还会限制线程池大小和队列容量。

它解决的问题是：

```text
email provider slow
-> email calls occupy all worker threads
-> payment and order API also cannot run
```

有隔离仓后，邮件依赖慢，只能占用它自己的资源池，不会把支付和下单也拖死。

**三者如何组合**

一个常见顺序是：

```text
request enters service
-> global/tenant rate limit
-> retry budget check
-> per-dependency bulkhead acquire
-> circuit breaker permission
-> downstream call with propagated deadline
```

失败后：

```text
if retryable and budget remains:
    backoff + jitter
else:
    return controlled error
```

不要在每一层都偷偷重试。更好的方式是：选一个明确层次负责 retry，其他层只做 deadline、限流、熔断、隔离和错误上报。

**错误码和客户端行为要配套**

控制重试风暴时，服务端错误码不能乱给：

| 情况 | 建议语义 |
| --- | --- |
| 客户端请求太多 | `429`，可带 `Retry-After` |
| 服务过载 | `503`，可带 `Retry-After` |
| 幂等 key 正在处理 | `409` 或 `202`，按 API 约定 |
| payload mismatch | `422`，不要重试同一请求 |
| deadline exceeded | 结果未知，客户端用同 key 查询或重试 |

客户端也要遵守：不是所有 5xx 都无限重试，不是所有 timeout 都立刻重试。

**一句话**

限流控制 retry 的数量，熔断减少对故障依赖的无效调用，隔离仓限制故障能占用的资源池；三者配合 retry budget、jitter、deadline propagation 和幂等 key，才能把重试风暴压回可控范围。

## Q034. deadline propagation 为什么重要？

**回答：**

deadline propagation 的作用是把调用方的剩余时间预算传给下游。没有它，下游可能在客户端已经放弃后继续做无用工作，或者每一层都重新给自己一份 timeout，把一次用户请求拖成很长的后台计算。

**deadline 和 timeout 的差别**

timeout 通常是一个持续时间：

```text
timeout = 500ms
```

deadline 是一个绝对截止点：

```text
deadline = 2026-06-16T10:00:00.500Z
```

gRPC 文档里说，客户端可以指定 deadline，让服务端知道还有多久才应该放弃这次 RPC。传播给下游时，gRPC 会把 deadline 转成 timeout，并扣掉已经过去的时间，这样可以避免时钟不一致带来的问题。

**没有 propagation 会怎样**

假设用户入口请求总预算是 800ms：

```text
frontend receives request, budget=800ms
frontend spends 300ms
frontend calls service A with timeout=800ms again
service A spends 400ms
service A calls service B with timeout=800ms again
```

用户在 800ms 时已经不等了，但 service B 可能还在做查询、拿锁、调用外部系统。这些工作没人会用到，却继续消耗资源。

正确方式是传剩余预算：

```text
frontend budget=800ms
after 300ms, call A with 500ms remaining
after A spends 400ms, call B with 100ms remaining
```

如果剩余时间不够，B 可以直接拒绝或走降级，而不是排队 2 秒后再返回。

**对过载保护很重要**

Google SRE 关于级联故障的章节提到，排队很久的请求往往已经不值得处理。比如用户搜索请求已经在队列里等了 10 秒，用户很可能刷新了页面，旧请求的响应不会再被使用。配合 RPC deadline，服务端可以丢弃已经过期的工作。

deadline propagation 让每一层都知道：

- 这个请求还值不值得排队。
- 是否应该跳过低价值子调用。
- 是否应该返回部分结果。
- 是否应该取消下游调用。
- 是否应该释放锁、线程和连接。

**和重试的关系**

没有 deadline propagation，重试会更危险：

```text
attempt 1 uses 500ms
attempt 2 uses another 500ms
attempt 3 uses another 500ms
```

用户本来只愿意等 700ms，系统却在后台花了 1500ms 甚至更久。

有总 deadline 后，重试共享同一个预算：

```text
total deadline = 700ms
attempt 1 uses 300ms
backoff 100ms
attempt 2 has 300ms left
```

这样 retry 不会无限延长请求生命周期。

**工程实现要点**

在 RPC 链路里，应该传播：

- deadline 或剩余 timeout。
- cancellation signal。
- request id / trace id。
- retry attempt 信息。
- criticality 或优先级。

Go 里通常用 `context.Context` 传递 deadline 和 cancellation；gRPC 各语言栈也有自己的 deadline 机制。关键是：业务代码和下游客户端都要使用这个 context，不能只在入口层设置一次。

**一句话**

deadline propagation 把用户请求的剩余时间预算带到每一层，避免过期请求继续排队和执行；它是控制重试风暴、减少无用工作、支持取消和负载丢弃的基础。

## Q035. RPC 链路中每层都设置 timeout 会带来什么问题？

**回答：**

每层都设置 timeout 本身没错，问题在于每层各自重新开始计时、各自重试、各自解释超时。这样会让真实请求生命周期远超用户预算，也会制造重复执行、资源泄漏和排障困难。

**问题一：总耗时被悄悄放大**

错误示例：

```text
client -> gateway timeout 1s
gateway -> service A timeout 1s
service A -> service B timeout 1s
service B -> database timeout 1s
```

看起来每层都只有 1 秒，但用户请求可能已经过了入口 deadline，下游还在继续执行。timeout 如果不是共享总预算，只是每一段的局部等待上限。

**问题二：每层重试会乘法放大**

如果每层都设置：

```text
timeout = 500ms
retries = 3
```

链路就会变成：

```text
gateway retries service A
service A retries service B
service B retries database
```

底层看到的不是 3 次，而是多层乘起来的尝试数。AWS Builders Library 的 5 层、每层 3 次重试例子会把数据库请求放大到 243 倍。

**问题三：错误语义变乱**

同一个失败可能在不同层被翻译：

```text
database timeout
-> service B returns 500
-> service A wraps as timeout
-> gateway returns 504
-> client retries POST
```

到最后，客户端看不到原始原因，也不知道是否发生了副作用。服务端日志里也很难把这些 attempt 关联到同一个 logical operation。

**问题四：取消信号断掉**

如果入口层超时后没有把 cancellation 传给下游，下游会继续执行：

```text
client gave up
gateway canceled
service A still running
service B still holding DB connection
```

这会浪费资源。更糟的是，写请求可能在客户端放弃后提交成功，后续重试又触发重复执行。

**问题五：timeout 没覆盖完整工作**

AWS Builders Library 提到过一个常见坑：有些 timeout 只覆盖 socket read，不覆盖 DNS、TLS handshake、连接建立或连接池等待。RPC 链路里每层都写了 timeout，仍可能有盲区。

所以要问清楚：

- timeout 是否覆盖排队等待？
- 是否覆盖连接池获取？
- 是否覆盖 DNS 和 TLS？
- 是否覆盖 request body upload？
- 是否覆盖 response body read？
- 是否能取消应用层 handler？

**更好的做法**

使用一个入口 deadline，并在每层分配剩余预算：

```text
request deadline = now + 800ms
gateway uses 100ms, passes 700ms
service A uses 200ms, passes 500ms
service B decides DB needs 600ms, skips or degrades
```

同时约定：

- 哪一层负责 retry。
- 哪些错误可重试。
- 重试是否共享总 deadline。
- 下游收到 deadline exceeded 时是否停止工作。
- 写请求是否必须带 idempotency key。

每层仍然可以有局部保护，比如数据库查询最多 100ms，但局部 timeout 不能超过剩余 deadline，也不能绕过总预算。

**一句话**

RPC 链路每层独立 timeout 会让总耗时、重试次数和错误语义失控；正确做法是入口设置总 deadline，沿链路传播剩余预算，局部 timeout 只能作为更小的保护栏。

## Q036. 如何设计可观测指标识别重试导致的重复执行？

**回答：**

要识别重试导致的重复执行，指标必须区分 logical request 和 attempt。只看 QPS、错误率和延迟不够，因为重试风暴会把一次用户操作变成多次执行尝试。

**先定义两个层次**

```text
logical request: 用户或上游的一次业务意图
attempt: 为完成这个意图发起的一次实际调用
```

同一个 logical request 可以有多个 attempt：

```text
operation_id = op1
attempt = 1, timeout
attempt = 2, success
```

如果没有这层区分，你看到的服务端 QPS 会被 retry 放大，无法判断到底是用户流量涨了，还是 attempt 涨了。

**核心指标**

建议至少有：

| 指标 | 含义 |
| --- | --- |
| `logical_request_total` | 入口逻辑请求数 |
| `request_attempt_total` | 实际调用尝试数 |
| `retry_attempt_total` | retry attempt 数 |
| `retry_amplification_ratio` | `attempts / logical_requests` |
| `idempotency_hit_total` | 同 key 重复命中 |
| `idempotency_mismatch_total` | 同 key 不同 fingerprint |
| `idempotency_in_progress_total` | 同 key 请求仍在处理 |
| `dedup_cache_hit_total` / `miss_total` | 去重缓存命中和穿透 |
| `side_effect_started_total` | 外部副作用开始次数 |
| `side_effect_committed_total` | 外部副作用确认成功次数 |
| `duplicate_suppressed_total` | 被去重挡住的重复执行 |
| `provider_idempotency_replay_total` | 外部 provider 返回已有结果 |

重试风暴的早期信号通常是：

```text
request_attempt_total grows
logical_request_total stable
retry_amplification_ratio rises
timeout rate rises
dedup hit/in_progress rises
```

**gRPC 可以直接利用 attempt 级指标**

gRPC 的 OpenTelemetry metrics 文档把 per-call 和 per-attempt 分开。它有 `grpc.client.attempt.started`、`grpc.client.attempt.duration`，也有 client retry 相关指标，比如 `grpc.client.call.retries`、`transparent_retries`、`hedges` 和 retry delay。这种拆法正好能用来观察一次 RPC 调用背后的多次 attempt。

业务系统也应该照这个思路设计自己的指标。

**日志字段**

每条日志最好带：

```text
trace_id
logical_request_id
idempotency_key_hash
fingerprint_hash
attempt_no
is_retry
retry_reason
deadline_remaining_ms
downstream
side_effect_id
dedup_decision
```

不要把原始 idempotency key 当高基数 label 直接打进 Prometheus。可以在日志里记录 hash，在指标 label 里只保留 operation、tenant tier、caller、status、retry reason 这类低基数字段。

**trace 里要看出重试**

分布式 tracing 里，建议：

- 一个 logical request 只有一个 root span。
- 每次 attempt 是一个 child span。
- attempt span 标出 `retry.attempt`、`retry.reason`、`deadline.remaining_ms`。
- 外部副作用 span 标出 `side_effect.id` 和 `idempotency.decision`。

这样可以在一条 trace 里看到：

```text
attempt 1 -> DEADLINE_EXCEEDED
attempt 2 -> duplicate suppressed
attempt 3 -> returned stored result
```

**识别重复执行**

几个实用查询：

```text
attempts per idempotency_key_hash > 1
side_effect_started_total > unique(operation_id)
duplicate_suppressed_total suddenly drops
idempotency_mismatch_total > 0
provider_idempotency_replay_total increases
```

还可以看：

```text
committed_side_effects / logical_requests
```

对支付这类接口，这个比例长期高于 1 就是严重信号。

**告警不要只看错误率**

有些重复执行不会立刻变成错误。比如重复发邮件、重复发券，HTTP 可能都是 200。告警要覆盖：

- retry amplification ratio。
- per-key attempt 分布。
- duplicate suppression rate。
- dedup cache miss surge。
- side effect start 与 commit 差值。
- provider idempotency conflict。
- deadline exceeded 后成功提交的数量。

一句话：识别 retry 引发的重复执行，必须把 logical request、attempt、idempotency decision 和 side effect 四层指标串起来；只看普通 QPS 和错误率会漏掉最危险的部分。

## Q037. 如何测试幂等逻辑在并发提交下是否正确？

**回答：**

测试幂等并发提交，不能只测“同一个请求发两次”。真正要测的是多个线程、多个进程、响应丢失、事务中断、缓存失效时，最终业务副作用是否仍然只发生一次。

**先写不变量**

测试前先定义不变量：

```text
same scope + same key + same fingerprint
  -> at most one business mutation commits
  -> all completed requests observe the same final result or compatible in-progress status

same scope + same key + different fingerprint
  -> no second business mutation
  -> returns mismatch error

different key
  -> treated as different logical operation unless business unique key rejects it
```

没有这些不变量，测试很容易只验证表面状态。

**并发提交测试**

最基础的测试：

```text
N = 100
barrier: all goroutines start at the same time
all send same idempotency key and same payload
wait for all responses

assert:
  business rows changed once
  external provider called once
  dedup table has one record
  all responses are success/stored result/in-progress according to API contract
```

关键是用 barrier 同步开始，制造真正的竞争，而不是循环顺序调用。

**payload mismatch 测试**

同一个 key，不同 payload：

```text
request A: key=k1, amount=100
request B: key=k1, amount=200
```

断言：

- 最多一个 payload 被接受。
- 另一个返回明确 mismatch。
- 不会出现两条业务记录。
- dedup record 保留第一次 fingerprint。
- mismatch 计数增加。

**处理中状态测试**

故意在服务端插入延迟：

```text
insert dedup record(status=processing)
sleep
execute mutation
```

在 sleep 期间发第二个同 key 请求。断言第二个请求不会绕过处理中状态去执行业务。它可以返回 `409`、`202`、等待结果，具体取决于 API 设计，但不能开第二条执行路径。

**崩溃点测试**

幂等逻辑最怕半成功。要在这些点注入故障：

```text
after dedup insert, before business mutation
after business mutation, before dedup succeeded
after commit, before response
after outbox insert, before external call
after external call success, before local status update
```

每个故障点后重启服务，用同一个 key 重试。断言最终状态能收敛，不会重复副作用。

**外部副作用测试**

用 fake provider，不要真的发邮件或扣款。fake provider 要记录：

```text
provider_idempotency_key
payload
call_count
result
```

并模拟：

- 成功响应丢失。
- provider timeout 但实际成功。
- provider 返回 duplicate replay。
- provider 先 500 后成功。
- provider 不支持 idempotency。

测试目标不是让所有场景都成功，而是确认系统不会在 unknown 状态下盲目重复副作用。

**缓存失效测试**

关掉或清空 dedup cache：

```text
cache miss for all requests
same key concurrent submissions
```

断言持久唯一约束仍然挡住重复执行。这个测试能抓出“缓存当正确性边界”的错误实现。

**压力和属性测试**

可以随机生成操作序列：

```text
submit same key
submit different key
retry after timeout
change payload
expire cache
crash worker
replay message
```

最后检查不变量：

```text
unique side effects per logical operation <= 1
dedup record fingerprint never changes
business unique constraints hold
outbox events are not duplicated beyond contract
```

这类测试比单元测试慢，但非常值。幂等 bug 往往只在并发和故障组合下出现。

**一句话**

并发幂等测试要用 barrier、故障注入、缓存失效、响应丢失和 fake provider，把同 key 同 payload、同 key 不同 payload、处理中重试和崩溃恢复都覆盖到；验收标准是业务副作用最多提交一次。

## Q038. 幂等设计如何影响 API 语义和错误码设计？

**回答：**

幂等设计会改变 API 对超时、重复提交、处理中状态和参数不一致的表达方式。一个支持幂等的 API，不能只返回成功或失败，还要能表达“已经处理过”“正在处理”“同 key 但内容不一致”“结果未知但可查询”。

**文档必须先定义契约**

API 文档要写清：

- 哪些接口需要 idempotency key。
- key 的作用域。
- key 的 TTL。
- fingerprint 覆盖哪些字段。
- 同 key 重试返回什么。
- 同 key 不同 payload 返回什么。
- 第一次请求仍在处理时返回什么。
- 服务端是否缓存失败结果。
- 超时后客户端应该重试还是查询状态。

IETF Idempotency-Key 草案要求资源发布幂等相关规范，并包含过期策略。这个要求很合理。没有文档，客户端很容易误用 key。

**典型错误码**

常见设计如下：

| 场景 | 建议状态码 | 语义 |
| --- | --- | --- |
| 需要 key 但没传 | `400 Bad Request` | 客户端请求缺少必需幂等字段 |
| 同 key、同 fingerprint，第一次已完成 | 返回第一次结果 | 可以是 `200`、`201` 或原始错误 |
| 同 key、第一次仍在处理 | `409 Conflict` 或 `202 Accepted` | 客户端稍后查询或重试 |
| 同 key、不同 fingerprint | `422 Unprocessable Content` | key 被误复用，不应继续重试同一请求 |
| 乐观锁版本不匹配 | `409 Conflict` 或 `412 Precondition Failed` | 资源状态已变化 |
| 请求太多 | `429 Too Many Requests` | 限流，可带 `Retry-After` |
| 服务过载 | `503 Service Unavailable` | 可重试，但要遵守 `Retry-After` 和 key |
| 客户端超时 | 客户端本地状态 | 服务端结果未知，需查询或同 key 重试 |

IETF 草案示例里也使用了 `400`、`422`、`409` 来分别表达缺少 key、key 搭配不同 payload、原请求仍在处理。

**重复成功时返回 200 还是 201**

这取决于 API 契约。

有两种常见方式：

```text
return original status code and response body
```

或者：

```text
return 200 with existing resource
```

Stripe 的语义偏向保存第一次请求的状态码和响应体，后续同 key 请求返回同一结果。这样客户端可以把重试当成第一次请求的回放。

如果你的 API 选择第二种方式，也要写清楚：

```text
first create -> 201 Created
duplicate retry -> 200 OK with same resource
```

不要让客户端猜。

**失败结果是否缓存**

这要按失败类型区分：

| 失败类型 | 是否缓存 |
| --- | --- |
| payload mismatch | 缓存或记录，防止同 key 反复误用 |
| 业务校验失败 | 通常可缓存，因为同请求重试仍会失败 |
| 乐观锁冲突 | 可以缓存为这次 key 的结果 |
| 临时 503 | 通常不作为最终业务结果缓存，除非请求已经进入执行阶段 |
| handler 执行后 500 | 需要非常谨慎，Stripe 选择保存第一次结果 |

关键是文档要说明：哪些错误代表最终结果，哪些错误代表请求没有开始执行。

**不能泄露其他人的结果**

幂等查重必须在认证和作用域检查之后：

```text
authenticate caller
derive scope = tenant/account/operation
lookup idempotency record under that scope
```

如果只用全局 key 查结果，攻击者拿到或猜到 key，可能读到别人的响应。

**一句话**

幂等 API 的错误码要表达缺 key、处理中、重复成功、payload mismatch、版本冲突和限流过载；状态码只是表面，真正的契约是 key 作用域、fingerprint、TTL、结果缓存和重试规则。

## Q039. 幂等 key 泄露是否会造成安全问题？

**回答：**

幂等 key 不应该被当成秘密，但泄露后仍可能造成安全问题。它能影响请求是否被视为重试，也可能被用来查询或覆盖幂等状态。安全设计的底线是：key 不能单独授予任何权限。

**可能的风险**

第一类是结果泄露。如果服务端用全局 key 查记录，攻击者拿到 key 后可能重放请求并拿到第一次响应：

```text
Idempotency-Key: leaked-key
```

如果响应里有订单、支付、地址等信息，就发生数据泄露。

第二类是请求阻塞。攻击者如果能预测 key，可能先用这个 key 发一个不同 payload，占住幂等记录：

```text
victim will use key = order-123
attacker sends key=order-123 with junk payload
victim later receives mismatch or in-progress
```

这本质是 key squatting。

第三类是业务推断。很多人会把业务信息放进 key：

```text
user_123_payment_999
```

日志、代理、监控系统里一旦泄露 key，就泄露了用户和业务信息。

第四类是重放。攻击者拿到完整请求、签名还没过期，并且幂等 key 仍有效时，可能反复重放。同 key 重放通常不会造成重复副作用，但会占用资源、干扰审计，或者触发返回已有结果。

**key 不是认证凭证**

正确处理顺序是：

```text
authenticate request
authorize operation
derive scope from authenticated principal
lookup idempotency record within scope
```

不要：

```text
lookup by idempotency key
return stored result
then check auth
```

幂等 key 只能在认证主体、租户、operation、region 等作用域下生效。

**签名和作用域**

如果 API 有 HMAC 或 HTTP Message Signature，`Idempotency-Key` 应该参与签名。这样中间人或错误代理不能在 body 不变的情况下改 key，把一次重试变成新操作。

服务端存储时也要绑定：

```text
tenant_id
account_id
auth_subject
operation
idempotency_key
fingerprint
```

同一个 key 在不同租户下可以同时存在；同一个 key 跨租户不能互相读取结果。

**日志与监控**

不要把原始 key 打到公共日志、指标 label 或错误消息里。更好的做法：

```text
idempotency_key_hash = HMAC(log_key, raw_key)
```

这样排障时能关联同一个 key，又不会把原始 key 泄出去。指标 label 更要避免原始 key，因为高基数会打爆监控系统。

**key 生成**

客户端应使用高熵随机 key，例如 UUIDv4 或类似随机标识。IETF Idempotency-Key 草案也建议使用 UUID 或类似随机标识，并强调 key 不应被不同 payload 复用。

不要用：

- 自增订单号。
- 手机号。
- 用户 ID。
- 时间戳。
- 可猜的短字符串。
- 包含 PII 的业务描述。

如果需要业务可读 ID，用另一个字段表达，不要塞进幂等 key。

**泄露后的影响边界**

设计得当时，泄露 key 的影响应该被限制为：

- 攻击者不能跨认证主体读取结果。
- 攻击者不能绕过签名修改 key。
- 攻击者不能用 key 单独执行操作。
- key 过期后不能继续使用。
- 误用会返回 mismatch，而不是执行新副作用。

如果这些条件不满足，key 泄露就可能变成真实安全漏洞。

一句话：幂等 key 不是密码，但会影响请求语义；要用认证作用域、签名覆盖、高熵随机值、TTL、日志脱敏和权限检查，确保 key 泄露不能变成越权读取、请求阻塞或重放攻击。

## Q040. idempotency key 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

idempotency key 的核心目标是正确性。它让客户端在结果未知时可以安全重试，让服务端把多次网络尝试识别为同一次业务意图，最终避免重复副作用。

**它解决的具体正确性问题**

分布式调用里最麻烦的状态是：

```text
client timeout
server may have committed
response may have been lost
```

客户端不知道该不该重试：

- 不重试，可能丢掉一次应该成功的操作。
- 重试，可能创建两次订单、扣两次款、发两张券。

idempotency key 把问题改成：

```text
same key + same fingerprint -> same logical operation
```

服务端第一次处理时保存状态，后续重试按同一个 key 返回已有结果或处理中状态。

**为什么主要不是性能问题**

幂等表、去重缓存和结果缓存可能提升性能，但这只是副作用。事实上，严肃的幂等设计往往会增加开销：

- 多一次去重表写入。
- 唯一索引冲突处理。
- payload fingerprint 计算。
- 结果状态保存。
- TTL 和 GC。
- 并发锁或事务。

如果只追求性能，可能会把去重放进缓存；但缓存丢了，正确性就没了。幂等 key 的价值不在快，而在“重试不会把业务搞错”。

**为什么也不是主要安全机制**

幂等 key 需要安全保护，比如参与签名、按租户作用域隔离、日志脱敏。但它本身不是认证、授权、CSRF 防护或重放防护。

安全系统回答的是：

```text
who are you?
are you allowed to do this?
has this signed request been tampered with?
```

idempotency key 回答的是：

```text
is this the same logical operation as before?
```

两者要配合，但不要混用。

**可维护性收益是间接的**

幂等 key 会让系统更容易排障：

- 可以用 operation id 关联日志。
- 可以查询操作最终状态。
- 可以解释 timeout 后发生了什么。
- 可以恢复 outbox 和外部副作用。

这些是可维护性收益，但它们建立在正确性语义之上。没有正确性边界，operation id 只是日志字段。

**它不解决什么**

idempotency key 不能替代：

- 事务。
- 唯一约束。
- 乐观锁。
- CAS。
- 消息 ack/offset 管理。
- 外部 provider 的幂等能力。
- 权限校验。
- 限流和熔断。

它只是把“同一次业务意图”的身份传给服务端。服务端还要用数据库约束、状态机、outbox、fingerprint、TTL 和故障恢复把这个身份落实成正确行为。

**一句话**

idempotency key 主要解决正确性：在 timeout、重试、重复投递和响应丢失下，把多次 attempt 收敛成同一次业务操作；性能、安全性和可维护性都会受影响，但它们不是这个机制的首要目标。

## Q041. idempotency key 的典型适用场景和不适用场景分别是什么？

**回答：**

idempotency key 适合处理“客户端不知道第一次写请求有没有成功”的场景。它不适合拿来给所有接口硬套，也不适合替代业务唯一键、锁、事务或权限校验。

**典型适用场景**

最适合的是非幂等写请求：

```text
POST /orders
POST /payments
POST /coupon-grants
POST /transfers
POST /email-notifications
POST /jobs
```

这些操作有共同点：

- 客户端可能因为 timeout、连接断开、网关 502 看不到最终结果。
- 服务端可能已经提交业务状态。
- 客户端会自然想重试。
- 重复执行会产生真实副作用。

Stripe 文档里也把 idempotency key 用在创建或更新对象的请求上，目的就是让连接错误后可以安全重试，不会创建第二个对象或执行第二次更新。

**支付和资金类操作**

支付是最典型的例子：

```text
client sends create payment
server charges card
response lost
client retries
```

没有 idempotency key，第二次请求可能再扣一次款。用同一个 key 后，服务端可以返回第一次的支付结果。

支付系统还会配合：

- 本地 payment request id。
- provider idempotency key。
- 金额和币种 fingerprint。
- provider charge id。
- 状态查询和对账。

**资源创建**

创建云资源、订单、任务、实例也适合：

```text
create VM
create volume
create order
create batch job
```

AWS EC2 的 client token 就是这种用法。文档说明，mutating API 可能在异步 workflow 完成前返回，也可能 timeout，导致客户端不知道操作是否成功；client token 可以保证成功请求的后续重试不会执行额外动作。

**外部副作用**

发邮件、发短信、发券、调用第三方 API 都适合带 operation id：

```text
email_notification_id
coupon_grant_id
third_party_request_id
```

这些副作用一旦发生，通常很难回滚。幂等 key 加 outbox 能把重试控制在同一个业务操作上。

**消息消费**

消费者处理 at-least-once 消息时，虽然不一定叫 idempotency key，本质上也需要类似键：

```text
event_id
message_id
source_lsn
business_operation_id
```

消费者用它判断同一条消息是否已经处理过，避免重复写业务表或重复调用外部服务。

**不适用场景**

第一类是纯读请求：

```http
GET /orders/123
```

读请求本身不应该产生副作用，重复执行不会创建新资源。Stripe 文档也明确说不要在 `GET` 和 `DELETE` 请求里发送 idempotency key，因为这些请求按定义已经幂等，发送 key 没有效果。

第二类是本来就有明确资源 ID 的创建：

```http
PUT /orders/client_order_123
```

如果客户端指定资源 ID，资源 ID 本身就能承担幂等身份。此时再加一个 idempotency key 可能有用，但不是第一道防线。业务唯一键更直接。

第三类是用户明确想重复执行的操作：

```text
buy another ticket
send another reminder
roll dice again
create another coupon
```

这些请求即使 payload 相同，也可能代表新的业务意图。不能只因为内容一样就用同一个 key 去重。

第四类是长时间开放的会话或批处理流：

```text
upload stream
interactive session
multi-step workflow without stable operation boundary
```

这类场景更适合 session id、job id、checkpoint 或事务状态机。单个 idempotency key 很难覆盖整个生命周期。

**判断标准**

可以问四个问题：

```text
这是不是写请求？
客户端是否可能看不到最终结果？
重复执行是否会产生坏副作用？
客户端能否为同一次业务意图稳定复用同一个 key？
```

四个问题都答“是”，idempotency key 很可能适合。

一句话：idempotency key 适合非幂等写、资源创建、支付、外部副作用和 at-least-once 消息处理；纯读、天然幂等的资源 ID 写入、刻意重复执行的业务意图，以及没有清晰操作边界的长会话，不适合硬套。

## Q042. idempotency key 和相近概念最容易混淆的边界在哪里？

**回答：**

idempotency key 最容易和 request id、trace id、dedup key、业务唯一键、payload fingerprint、nonce、事务 ID 混在一起。区分它们要看一个问题：这个标识到底在回答什么。

**对比表**

| 概念 | 回答的问题 | 是否等同 idempotency key |
| --- | --- | --- |
| request id | 这一次 HTTP/RPC attempt 是谁 | 不是 |
| trace id | 这条调用链怎么串起来 | 不是 |
| retry token/client token | 这次业务操作的重试身份 | 可能是 |
| dedup key | 这条消息或事件是否处理过 | 相近，但作用域可能不同 |
| business unique key | 业务世界里哪个资源唯一 | 可作为更强约束 |
| payload fingerprint | 同 key 下 payload 是否一致 | 不是 key，配合 key 使用 |
| nonce | 防重放或保证一次性使用 | 目标不同 |
| transaction id | 数据库或分布式事务的一次提交身份 | 不是 |
| outbox event id | 对外发布事件的唯一身份 | 可作为下游去重键 |

**request id 不是 idempotency key**

request id 通常每次 attempt 都不同：

```text
attempt 1: request_id=r1, idempotency_key=k1
attempt 2: request_id=r2, idempotency_key=k1
```

如果把 request id 当 idempotency key，每次重试都会被服务端当成新操作。结果是幂等失效。

正确关系是：

```text
one logical operation -> one idempotency key
one logical operation -> many request ids
```

**trace id 也不是**

trace id 用于可观测性。它能把入口请求、RPC 调用、数据库查询串起来，但它不应该决定业务是否重复执行。

有些系统会让一次 retry 继承 trace id，有些系统会新建 span。无论哪种，trace id 都不能替代持久去重状态。

**business unique key 更靠近业务事实**

业务唯一键是业务层自然约束：

```text
client_order_id
payment_request_id
coupon_grant_id
```

它经常比 idempotency key 更强。比如订单表的 `client_order_id` 唯一，可以防止同一个客户端订单创建两次。

但两者边界不同：

- idempotency key 绑定一次 API 操作。
- business unique key 绑定一个业务实体或业务事实。

有时它们可以相同，有时不应该相同。比如一次“更新订单地址”的 idempotency key 不一定等于订单号，因为同一订单可以有多次不同更新。

**fingerprint 不是 key**

fingerprint 用来回答：

```text
同一个 key 下，这次请求内容是否和第一次一致？
```

它不能单独代表一次操作。两个不同用户可能提交相同 payload，fingerprint 一样，但业务上应该分别处理。

错误做法：

```text
dedup by hash(payload)
```

这样可能把两个合法请求误合并。正确做法是：

```text
dedup by (scope, idempotency_key)
then compare fingerprint
```

**nonce 的目标不同**

nonce 常用于防重放：

```text
same signed request cannot be accepted twice
```

idempotency key 的目标是安全重试：

```text
same logical operation can be submitted more than once, but effect only once
```

nonce 的常见语义是第二次同 nonce 请求失败；idempotency key 的常见语义是第二次同 key 请求返回第一次结果。方向相反。

**事务 ID 不是对外契约**

数据库 transaction id 是内部提交机制。客户端不应该拿它当重试身份。事务可能重试、回滚、切换连接，也可能只覆盖本地数据库，覆盖不到外部邮件、支付和消息发布。

**一句话**

idempotency key 标识同一次业务意图的重试；request id 和 trace id 用于观测，fingerprint 用于参数一致性，business unique key 约束业务实体，nonce 防重放。混淆这些边界，最常见后果就是该去重的不去重，不该去重的被误去重。

## Q043. idempotency key 在高并发场景下可能出现哪些隐藏问题？

**回答：**

高并发下，idempotency key 的难点不在“查一下有没有 key”这么简单，而在同一个 key 或同一批热点 key 同时到达时，谁有资格执行业务、谁应该等待、谁应该返回已有结果，以及这个决定能不能在崩溃后恢复。

**问题一：check-then-insert race**

错误实现：

```text
select dedup where key = k1
if not found:
    execute business mutation
    insert dedup record
```

两个线程同时查，都看到不存在，然后都执行业务。高并发下这个 bug 很容易出现。

正确做法是先占位：

```text
insert dedup(scope, key, fingerprint, status=processing)
on conflict do ...
```

让数据库唯一约束成为并发互斥点。

**问题二：热点 key 锁竞争**

同一个 key 被大量重试时，所有请求都打到同一行：

```text
key=k1, 1000 concurrent retries
```

可能出现：

- 行锁等待变长。
- 唯一索引冲突暴涨。
- 数据库 CPU 花在冲突处理上。
- 上游等待变长后继续重试。
- `processing` 状态被反复查询。

缓存可以缓解读压力，但不能替代持久唯一约束。

**问题三：处理中状态设计不清**

第一次请求还没完成时，第二次同 key 请求来了。API 必须定义行为：

```text
return 409 in_progress
return 202 accepted and status URL
wait for first request
long-poll result
```

如果没有定义，就会出现两个坏实现：

- 第二个请求直接开新执行路径。
- 第二个请求一直阻塞，最终耗尽线程池。

Stripe 文档提到，如果请求和另一个正在执行的请求冲突，且 endpoint 尚未开始执行，就不会保存 idempotent result，这类请求可以重试。这个细节说明“处理中”和“已开始执行”需要拆开。

**问题四：读副本延迟**

如果写入主库后，从只读副本查幂等记录，可能读不到刚写入的 key：

```text
request A inserts key on primary
request B reads replica
replica lag
request B thinks key missing
```

幂等查重最好读写同一个强一致点。至少，同 key 的决策不能依赖可能滞后的副本。

**问题五：跨实例内存状态不一致**

单机用内存 map 能挡住一部分重复请求：

```text
map[key] = processing
```

多实例后，请求可能被负载均衡到不同机器。每台机器各有一份 map，语义马上破掉。高并发服务必须使用共享持久状态或分片一致路由。

**问题六：payload mismatch 和成功结果竞争**

同一个 key 下，一个请求 payload A 已经进入执行，另一个请求 payload B 同时进来。服务端要保证 fingerprint 一旦确定就不可改变：

```text
key=k1, fingerprint=A
later key=k1, fingerprint=B -> mismatch
```

不能让后来的请求覆盖 fingerprint，也不能把 B 的结果存成 k1 的最终结果。

**问题七：TTL 和长事务冲突**

如果处理时间很长，TTL 太短，key 可能在第一次操作还没收敛时被清理：

```text
operation running for 2h
idempotency TTL = 1h
retry arrives at 90min
key missing
```

这会把一个处理中请求变成新请求。TTL 必须覆盖最大执行时间、最大重试窗口和消息重放窗口。

**问题八：高基数观测打爆指标系统**

把原始 idempotency key 放进 metrics label，会造成高基数：

```text
idempotency_key="k-..."
```

Prometheus、日志索引、APM 都可能被打爆。可观测性要用 hash、采样和低基数字段。

**一句话**

高并发下 idempotency key 的隐藏问题集中在唯一约束、热点锁、处理中状态、读副本延迟、跨实例共享状态、TTL 和高基数观测；真正可靠的实现不能只靠缓存或先查后写。

## Q044. idempotency key 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

这些场景会把幂等实现拆成一连串故障点。只要某个状态没有持久化，或者业务副作用和幂等记录没有同一恢复路径，重试就可能返回错误结果或重复执行。

**崩溃点一：写入幂等记录后崩溃**

流程：

```text
insert idempotency record(status=processing)
process crashes
```

重启后同 key 请求进来，服务端看到 `processing`。这时必须能判断它是：

- 真的仍有 worker 在处理。
- 旧 worker 已经崩溃，记录需要接管。
- 业务已经完成但状态没更新。

常见做法是加入：

```text
locked_until
owner_id
fencing_token
heartbeat_at
```

接管前要确认旧执行者不会继续提交，或者用 fencing token 阻止旧执行者写回。

**崩溃点二：业务提交后，幂等记录未更新**

流程：

```text
business mutation committed
process crashes before idempotency status=succeeded
```

如果业务变更和幂等记录不在同一个事务里，就会出现业务成功、幂等状态仍是 processing。重试可能卡住，也可能被错误接管再次执行。

解决方式：

- 幂等记录和业务变更同事务提交。
- 如果跨系统，用 outbox 和可恢复状态机。
- 重启后能从业务表反查 result_ref。

**崩溃点三：响应发送前崩溃**

流程：

```text
commit succeeds
process crashes before response reaches client
client timeout
client retries same key
```

这是 idempotency key 最该处理的场景。重试应该返回第一次结果，而不是重新执行业务。

**崩溃点四：外部副作用成功，本地状态未知**

流程：

```text
call payment provider succeeds
local update fails or crashes
```

这是最难的边界。外部 provider 不在本地事务里。重启后不能盲目再扣款，应该：

- 用同一个 provider idempotency key 查询或重试。
- 保存本地 payment_request_id。
- 做对账任务。
- 把 unknown 状态显式暴露给恢复流程。

**重启后内存去重丢失**

如果 idempotency 状态只存在内存里：

```text
server restart
all in-memory keys lost
client retries
operation executes again
```

这说明内存 map 只能做优化，不能做正确性存储。非幂等写请求的 key、fingerprint、status、result_ref 至少要持久化到数据库或等价的 durable store。

**超时后的边界**

客户端 timeout 后，服务端可能仍在执行：

```text
client timeout at 500ms
server commits at 700ms
client retries at 800ms
```

同一个 key 重试必须看到服务端在 700ms 的提交结果。如果重试先到而第一次还没提交，要返回处理中或等待，而不是开第二次。

**TTL 边界**

key 过期后再次出现，很多系统会把它当新请求。Stripe 文档明确提到，key 至少 24 小时后可以清理；清理后复用 key 会生成新的请求。

这要求 API 文档写清：

```text
within TTL: same key means retry
after TTL: same key may be treated as new operation
```

高价值业务不能只依赖短 TTL，还要有业务唯一键或审计记录兜底。

**一句话**

崩溃、重启、超时和重试会暴露幂等记录写入、业务提交、结果保存、外部副作用、内存状态和 TTL 的边界；每个边界都要能恢复到同一个最终业务结果，不能靠“这段代码大概不会崩”来保证。

## Q045. idempotency key 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

大多数线上系统里，idempotency key 的瓶颈首先来自 I/O 和锁竞争，其次是网络和内存。CPU 只有在 payload 很大、fingerprint 规范化复杂或加密签名很重时，才会成为主瓶颈。

**I/O 瓶颈**

每个非幂等写请求通常都要写去重表：

```text
insert idempotency record
update status/result
maybe write outbox
maybe update business row
```

这会产生：

- WAL 写入。
- B-tree 索引更新。
- fsync 或 replication wait。
- 事务提交延迟。
- TTL/GC 扫描。
- 大 response 存储。

如果把完整 response body 存进幂等表，表膨胀会更快。索引变大后，cache hit 下降，I/O 更重。

**锁竞争**

热点 key 或热点租户会让唯一索引和行锁成为瓶颈：

```text
many attempts for (tenant, operation, key)
```

表现为：

- unique constraint conflict rate 上升。
- lock wait time 上升。
- deadlock 或 serialization failure 增加。
- 同 key 请求 p99 很高。
- 数据库 CPU 花在冲突检查和事务管理上。

这种瓶颈不是加机器一定能解决。需要 per-key singleflight、缓存 `processing/succeeded` 状态、热点隔离、合理的 retry budget。

**网络瓶颈**

如果去重表在远程数据库、Redis、跨区域存储或共识系统里，网络延迟会进入每个写请求的主路径：

```text
service -> remote dedup store -> service
```

跨区域 active-active 更明显。为了保证同一个 key 只执行一次，系统可能需要强一致写入或分区归属，这会增加跨区域延迟。

**内存瓶颈**

内存主要来自：

- dedup cache。
- in-flight key map。
- 存储大 response。
- 高基数日志缓冲。
- 排队等待同 key 结果的请求。

如果大量请求卡在 `processing` 等待结果，服务的 goroutine、future、promise、线程栈也会堆积。

**CPU 瓶颈**

CPU 开销通常来自：

- JSON canonicalization。
- payload hash。
- HMAC/签名验证。
- 大 body 序列化。
- 压缩或加密 response snapshot。

对小 payload 来说，这些通常不是主瓶颈。对大批量导入、文件上传、复杂嵌套 JSON，CPU 可能明显上升。可以用预先计算的 content digest、流式 hash、限制 payload size 来控制。

**如何定位**

可以看这些指标：

| 瓶颈 | 指标 |
| --- | --- |
| CPU | fingerprint 计算耗时、序列化耗时、CPU profile |
| 内存 | in-flight key 数、response snapshot 大小、cache size |
| 锁竞争 | lock wait、unique conflict、deadlock、hot key top N |
| I/O | dedup table write latency、WAL、fsync、index size |
| 网络 | dedup store RTT、cross-region latency、timeout |

不要只看接口平均延迟。幂等逻辑常常只在冲突、重试、热点 key 时变慢。

一句话：idempotency key 的瓶颈通常是持久化去重记录的 I/O 和热点 key 的锁竞争；CPU 多半来自 fingerprint，网络多半来自远程或跨区域 dedup store，内存多半来自缓存和 in-flight 等待。

## Q046. idempotency key 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

这三类测试目的不同。correctness test 验证语义不变量，stress test 找并发和故障下的破坏点，benchmark 衡量吞吐、延迟和资源成本。把它们混在一起，容易出现“压测数字很好，但正确性没测到”的问题。

**correctness test**

correctness test 要回答：语义是否正确。

应该覆盖：

```text
same key + same payload -> only one business effect
same key + same payload retry -> same result
same key + different payload -> mismatch
same key while first is processing -> in-progress/wait/result, no second execution
same key after terminal success -> replay result
same key after terminal failure -> follow documented policy
missing key on required API -> 400
expired key -> documented behavior
```

还要测：

- key scope，跨 tenant 不互相影响。
- fingerprint 不可被后续请求覆盖。
- 业务表唯一约束和幂等表状态一致。
- 外部 provider 只收到一次副作用或同一个 provider key。

correctness test 的断言应该落到业务副作用上，而不是只看 HTTP status。

**stress test**

stress test 要回答：并发、故障和重试叠加时会不会破。

典型场景：

```text
1000 concurrent requests with same key
1000 concurrent requests with different keys
hot tenant with many keys
cache unavailable
dedup DB slow
dedup DB failover
read replica lag
process crash after dedup insert
process crash after business commit
provider timeout after success
retry storm with same key
retry storm with many unique keys
```

stress test 关注：

- 是否出现重复业务副作用。
- 是否出现死锁。
- 是否有锁等待雪崩。
- `processing` 状态是否泄漏。
- TTL 清理是否误删未完成记录。
- 系统是否 fail open。

压测时要加入故障注入。没有故障注入的高并发测试，只能证明 happy path 能跑。

**benchmark**

benchmark 要回答：成本是多少，瓶颈在哪里。

要分几组测：

```text
no idempotency baseline
idempotency with cache hit
idempotency with cache miss and DB insert
duplicate retry hit terminal result
same key high contention
payload fingerprint small/medium/large
stored result small/large
TTL GC running
```

指标包括：

- throughput。
- p50/p95/p99 latency。
- dedup DB QPS。
- unique conflict rate。
- lock wait。
- WAL 写入量。
- cache hit rate。
- CPU profile。
- memory footprint。
- response snapshot size。

benchmark 不要只测平均值。同 key 竞争下的 p99 和锁等待更有价值。

**测试数据设计**

数据要区分：

- 高基数 key。
- 热点 key。
- 热点 tenant。
- 大 payload。
- 大 response。
- 短 TTL 和长 TTL。
- 成功、业务失败、系统失败、处理中。

这样能看出幂等表索引、缓存和 GC 的真实压力。

一句话：correctness test 测语义不变量，stress test 测并发和故障组合下是否重复执行，benchmark 测引入幂等后的延迟、吞吐、I/O、锁竞争和资源成本。

## Q047. 如果要求从零实现一个简化版 idempotency key，你会先定义哪些不变量？

**回答：**

从零实现时，先别急着写缓存、TTL 和重试策略。先定义不变量。没有不变量，代码很快会变成“看起来能去重”的状态机，线上一遇到 timeout 和并发就破。

**不变量一：作用域内 key 唯一**

```text
UNIQUE(scope, idempotency_key)
```

scope 至少包含：

```text
tenant/account
operation
region or shard if semantics requires it
```

不要做全局单列 key，也不要只按用户传来的字符串查。

**不变量二：fingerprint 第一次写入后不可变**

第一次请求确定 fingerprint：

```text
key=k1 -> fingerprint=f1
```

后续同 key：

```text
fingerprint=f1 -> retry
fingerprint=f2 -> mismatch
```

任何后续请求都不能覆盖 f1。

**不变量三：同一 key 同一时间最多一个 executor**

状态可以是：

```text
processing
succeeded
failed
conflict
expired
```

当状态是 `processing` 时，只能有一个 owner 有资格执行业务。其他请求要等待、返回处理中，或读取结果。不能开第二条执行路径。

**不变量四：终态结果可重放**

一旦进入终态：

```text
succeeded
failed
conflict
```

同 key 同 fingerprint 的重试应该返回同一个语义结果。可以不逐字节返回同一个 response，但业务含义必须稳定。

**不变量五：幂等记录和业务变更同一恢复边界**

理想情况：

```text
begin transaction
insert/update idempotency record
apply business mutation
write outbox
commit
```

如果跨外部系统，就要有 outbox、provider idempotency key、状态查询和对账，不能让外部副作用独自漂在事务外。

**不变量六：同 key 不同 payload 不执行业务**

payload mismatch 是客户端误用或攻击信号。处理方式：

```text
return 422 or equivalent mismatch error
do not execute mutation
do not change original fingerprint
```

**不变量七：TTL 不得早于可重试窗口**

key 保留时间至少覆盖：

```text
client retry window
server max execution time
message replay window
external provider reconciliation window
```

清理只应该清理已到期且不再被 reader、worker、snapshot 使用的记录。

**不变量八：认证作用域先于 key 查找**

处理顺序：

```text
authenticate
authorize
derive scope
lookup idempotency record
```

不能让 key 变成跨租户读取结果的入口。

**最小实现**

表结构可以很小：

```sql
CREATE TABLE idempotency_records (
  scope text NOT NULL,
  idempotency_key text NOT NULL,
  fingerprint text NOT NULL,
  status text NOT NULL,
  result_ref text NULL,
  error_code text NULL,
  created_at timestamptz NOT NULL,
  expires_at timestamptz NOT NULL,
  PRIMARY KEY (scope, idempotency_key)
);
```

流程：

```text
begin transaction
try insert processing record
if conflict:
    compare fingerprint
    return existing result or in-progress/mismatch
execute business mutation
mark succeeded with result_ref
commit
```

一句话：简化版 idempotency key 先定义 scope 唯一、fingerprint 不变、单 executor、终态可重放、业务变更同事务、mismatch 不执行、TTL 覆盖重试窗口、认证作用域先行这几个不变量，再谈缓存和优化。

## Q048. idempotency key 的常见误用是什么，误用后通常会产生什么线上症状？

**回答：**

idempotency key 的误用很容易在线上表现成两类症状：该挡的重复副作用没挡住，或者不该挡的正常请求被挡住。很多问题不是接口立刻报错，而是在账单、订单、库存、邮件和对账里慢慢暴露。

**误用一：每次重试都生成新 key**

症状：

```text
client timeout
retry with new key
server treats it as new operation
```

线上表现：

- 重复订单。
- 重复扣款。
- 重复发券。
- dedup hit rate 很低。
- retry amplification 上升，但 duplicate suppression 没上升。

正确做法是同一次业务意图复用同一个 key。

**误用二：不同业务意图复用同一个 key**

比如前端页面加载时生成一个 key，用户修改表单后还继续用它：

```text
first submit amount=100
second submit amount=200
same key
```

线上表现：

- `payload mismatch` 或 `IdempotentParameterMismatch` 增加。
- 用户看到“重复请求”或“参数不一致”。
- 正常新请求被挡住。
- 客服收到“我改了金额为什么提交不了”的问题。

**误用三：不做 fingerprint 比对**

同 key 不同 payload 被当成重试：

```text
key=k1, amount=100 -> success
key=k1, amount=200 -> returns amount=100 result
```

线上表现：

- 用户看到的结果和本次请求参数不一致。
- 审计日志难以解释。
- 安全团队会把它看成请求污染或越权风险。

**误用四：只把 key 放进缓存**

缓存重启或过期后，系统忘记已处理请求。

线上表现：

- 服务重启后重复副作用上升。
- Redis 故障期间重复扣款或重复发券。
- cache miss surge 后业务表唯一冲突增加。
- dedup 状态无法审计。

**误用五：scope 定义过宽或过窄**

过宽：

```text
UNIQUE(idempotency_key)
```

不同租户同 key 冲突。

过窄：

```text
scope includes request_id
```

每次重试都是新 scope，完全去不了重。

线上表现：

- 跨租户 key collision。
- 某些 region 或 AZ 重复创建资源。
- dedup hit rate 异常低。
- 同 key 在不同 operation 互相污染。

AWS EC2 的 regional/zonal idempotency 就是在提醒：scope 是语义的一部分，不是实现细节。

**误用六：TTL 太短**

线上表现：

- 消息 replay 后重复消费。
- 客户端离线后重试被当成新请求。
- 长任务还没完成，key 已经过期。
- 对账发现老请求再次生效。

TTL 要跟 retry window、消息保留时间、长任务最大时长对齐。

**误用七：把 key 当安全凭证**

线上表现：

- 泄露 key 后可以查到别人结果。
- 日志里出现 PII。
- 支持人员通过 key 能看到敏感响应。
- 跨租户返回已有结果。

key 不能替代认证授权。必须先 auth，再按 scope 查 key。

**误用八：存完整大响应不设边界**

线上表现：

- 幂等表暴涨。
- 索引膨胀。
- 清理任务拖慢主库。
- p99 延迟随表大小上升。
- 备份和恢复时间变长。

大响应应该存 `result_ref` 或摘要，不要无脑塞进去。

**一句话**

idempotency key 的常见误用包括每次重试换 key、不同业务复用 key、不比对 fingerprint、只用缓存、scope 错、TTL 短、把 key 当凭证、存大响应；线上症状通常是重复副作用、mismatch 激增、跨租户污染、延迟上升和对账异常。

## Q049. idempotency key 在单机和分布式环境中的语义有什么差异？

**回答：**

单机环境里，idempotency key 可以靠本地锁、本地事务和单进程状态实现一部分语义。分布式环境里，请求可能落到不同实例、不同分区、不同区域，key 的语义必须依赖共享持久状态和明确作用域。

**单机环境**

单机服务可以做：

```text
in-memory map
local mutex
local database transaction
single process owner
```

这能挡住同进程内的重复提交：

```text
map[key] = processing
```

但单机方案有明显边界：

- 进程重启后内存 key 丢失。
- 多进程部署后 map 不共享。
- 业务库提交和内存状态无法同事务。
- 外部副作用仍然不可回滚。

所以即使是单机，只要接口有真实副作用，也最好把 key 写进本地数据库，而不是只放内存。

**分布式环境**

分布式环境多了这些问题：

```text
load balancer sends retries to different instances
dedup store has primary/replica lag
multiple regions accept writes
message queue redelivers to different consumers
old worker may continue after new worker takes over
```

这时 idempotency key 需要：

- 共享 durable store。
- 强一致唯一约束或分区归属。
- scope 定义，例如 tenant、operation、region、AZ。
- fencing token 或 owner lease。
- read-your-writes。
- TTL 和 GC 协调。
- 跨区域冲突策略。

**区域语义**

AWS EC2 文档把 idempotency 分成 regional 和 zonal：

```text
regional: 同一 Region 内同 client token 只能完成一次
zonal: 同一 AZ 内同 client token 只能完成一次
```

这说明分布式系统里“同一个 key”不是天然全局唯一。它的意义取决于系统定义的 scope。

如果 API 文档不说清楚 scope，客户端会误以为同一个 key 在所有区域都去重，结果可能在多个 region 各创建一个资源。

**复制延迟**

如果 idempotency record 写入 region A，还没复制到 region B，客户端重试打到 region B：

```text
region A accepts key k1
replication lag
retry goes to region B
region B thinks k1 missing
```

解决方式包括：

- 同 key 路由到同一 home region。
- 使用全局强一致存储。
- 让 region 成为 scope 的一部分，并在 API 文档说明。
- 用业务唯一键和对账兜底。

**故障恢复**

单机重启只需要恢复本地状态；分布式恢复还要处理旧 owner：

```text
worker A owns key k1
lease expires
worker B takes over
worker A pauses then resumes
```

如果没有 fencing，A 和 B 都可能提交。分布式幂等状态经常要带：

```text
owner_id
lease_epoch
fencing_token
locked_until
```

**一句话**

单机幂等主要处理进程内并发和重启恢复；分布式幂等还要处理多实例路由、共享存储、复制延迟、区域作用域、旧 owner fencing 和跨区域冲突。scope 和一致性模型必须写进 API 语义里。

## Q050. payload fingerprint 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

payload fingerprint 的核心目标也是正确性，但它解决的是 idempotency key 的另一半问题：同一个 key 下，请求内容是否仍然是同一个业务意图。没有 fingerprint，服务端只能知道 key 相同，不能知道 payload 有没有被误改。

**它解决什么正确性问题**

典型问题：

```text
POST /payments
Idempotency-Key: k1
amount=100

POST /payments
Idempotency-Key: k1
amount=200
```

这不是重试，而是同一个 key 被用于两个不同业务请求。payload fingerprint 让服务端能识别：

```text
stored fingerprint != incoming fingerprint
```

然后返回 mismatch，而不是返回旧结果或执行新请求。

Stripe 文档也说明，idempotency layer 会比较后续请求参数和原请求参数，如果不一致就报错，防止 accidental misuse。AWS EC2 的 `IdempotentParameterMismatch` 也是同一类语义。

**它不是去重 key**

fingerprint 不能替代 idempotency key。

两个用户可能发送同样 payload：

```text
tenant A: amount=100
tenant B: amount=100
```

fingerprint 可能相同，但它们不是同一个业务操作。正确结构是：

```text
lookup by (scope, idempotency_key)
then compare fingerprint
```

不要做：

```text
dedup by fingerprint only
```

那会把正常请求误合并。

**它和安全的关系**

fingerprint 有安全收益，但不是主要安全机制。它能发现同 key 下 payload 被替换，能帮助审计，也能降低请求污染风险。

但它不能替代：

- 请求签名。
- 认证。
- 授权。
- replay protection。
- content digest。

如果担心请求被篡改，应该让 method、path、body digest、`Idempotency-Key` 参与签名。fingerprint 更多是服务端幂等语义的一部分。

**它和性能的关系**

fingerprint 会增加 CPU 和存储开销：

- canonicalization。
- hash 计算。
- fingerprint 存储。
- 版本兼容。
- mismatch 诊断。

它通常不是为了性能。缓存 fingerprint 可以减少重复请求的比对成本，但这是优化，不是目的。

**它和可维护性的关系**

fingerprint 能提升排障能力：

- 知道同 key 下 payload 是否变化。
- 能解释 mismatch。
- 能追查客户端 SDK 是否错误复用 key。
- 能比较不同 fingerprint version 的行为。

但可维护性仍是附带收益。它首先是为了避免错误地把不同业务请求当成同一次重试。

**fingerprint 应该怎么做**

应该基于 canonical request：

```text
operation
resource id
tenant/account
canonical body
expected version
API version
```

不应该包含：

```text
trace id
Date
User-Agent
retry attempt
Authorization signature
server received_at
```

JSON 请求可以参考 RFC 8785 的 JSON Canonicalization Scheme，或者用 schema-aware normalization 生成稳定表示。

**一句话**

payload fingerprint 主要解决正确性：防止同一个 idempotency key 被不同 payload 误复用；它辅助安全和排障，但不能替代签名、认证、授权，也不能单独当去重键。

## Q051. payload fingerprint 的典型适用场景和不适用场景分别是什么？

**回答：**

payload fingerprint 适合用来判断“同一个 idempotency key 下，请求内容是否仍然等价”。它不适合当全局去重键，也不适合给语义不稳定、顺序不明确、格式经常漂移的 payload 直接做 raw hash。

**典型适用场景**

第一类是幂等写请求：

```text
Idempotency-Key: k1
payload_fingerprint: hash(canonical_payload)
```

服务端第一次收到 key 时保存 fingerprint。后续同 key 请求进来，先比较 fingerprint：

```text
same key + same fingerprint -> retry
same key + different fingerprint -> key misuse
```

Stripe 的幂等文档就是这个方向：后续请求会和原请求参数比较，不一致时报错，用来防止 key 被误复用。

第二类是有金额、收款方、数量、版本号的高风险写操作：

```text
create payment
create transfer
reserve inventory
grant coupon
update order with expected_version
```

这些字段一旦变了，业务意图就变了。fingerprint 能挡住“同一个 key 下金额从 100 变 200”这种危险情况。

第三类是跨语言 SDK。不同客户端可能用 Java、Go、Python、JavaScript 发送同一语义请求。服务端需要一种稳定规范化方式，而不是依赖各语言默认 JSON 序列化。RFC 8785 的 JCS 说明了 canonical JSON 对 hashing/signing 的价值：数据需要以 invariant format 表示，hashing 和 signing 才能稳定重复。

第四类是审计和排障。存 fingerprint 后，可以快速回答：

- 这次 retry 的 payload 是否和第一次一致。
- mismatch 是哪个客户端触发的。
- fingerprint 版本升级后是否产生兼容问题。
- 同 key 是否被攻击或误用。

**不适用场景**

不适合把 fingerprint 当全局去重键：

```text
dedup by hash(payload)
```

两个用户买同一件商品，payload 可能一样，但业务上应该产生两笔订单。fingerprint 只能在 `(scope, idempotency_key)` 找到记录后用于比对，不能单独决定“这个请求已经处理过”。

不适合对 raw JSON body 直接 hash 后长期承诺语义：

```json
{"amount":100,"currency":"USD"}
```

和：

```json
{"currency":"USD","amount":100}
```

raw bytes 不同，业务语义相同。除非 API 明确要求字节级一致，否则 raw hash 会制造误报。

不适合 payload 里包含每次都会变化的字段：

```text
timestamp
nonce
trace_id
request_id
signature
retry_attempt
client_generated_at
```

这些字段进入 fingerprint 后，同一次重试也会变成 mismatch。

不适合语义上允许重复提交的操作：

```text
send another reminder
buy another ticket
create another identical order
```

如果用户明确要做第二次相同业务，fingerprint 一样也不能自动合并。

**一句话**

payload fingerprint 适合在同一个 idempotency key 下做参数一致性校验，尤其适合支付、订单、库存和跨语言 JSON API；它不适合单独做全局去重，也不适合直接 hash 不稳定的 raw payload。

## Q052. payload fingerprint 和相近概念最容易混淆的边界在哪里？

**回答：**

payload fingerprint 容易和 content digest、请求签名、idempotency key、业务唯一键、ETag、cache key 混在一起。它们都像“hash”，但回答的问题不一样。

**概念边界**

| 概念 | 回答的问题 | 和 fingerprint 的差别 |
| --- | --- | --- |
| idempotency key | 这是哪一次逻辑操作 | key 用于查记录，fingerprint 用于校验内容 |
| payload fingerprint | 同 key 下内容是否一致 | 只在 key 作用域内有意义 |
| content digest | HTTP body 字节有没有变 | 偏传输完整性，不一定懂业务语义 |
| request signature | 请求组件是否被授权方签过 | 安全机制，不替代幂等比对 |
| ETag | 资源当前版本是什么 | 面向资源状态，不是请求 payload |
| cache key | 缓存能否复用响应 | 关注缓存命中，不等于写请求幂等 |
| business unique key | 业务实体是否唯一 | 约束业务事实，比 fingerprint 更靠近业务表 |

**和 idempotency key 的边界**

常见表结构是：

```text
primary key: (scope, idempotency_key)
columns: fingerprint, status, result_ref
```

先用 key 找记录，再比较 fingerprint。顺序不能反：

```text
bad: find by fingerprint
good: find by key, then compare fingerprint
```

fingerprint 相同不表示同一次操作，key 相同但 fingerprint 不同表示 key 被误用。

**和 content digest 的边界**

content digest 通常保护原始 body：

```text
SHA256(raw bytes)
```

payload fingerprint 可以是语义化的：

```text
SHA256(canonical(operation, resource_id, expected_version, normalized_body))
```

这两个值有时相同，有时不应相同。比如 JSON 字段顺序变化，content digest 会变；如果业务语义不变，payload fingerprint 可以不变。

**和签名的边界**

签名证明请求没有被未授权修改。fingerprint 判断同 key 下 payload 是否和第一次一样。

签名失败时，请求不可信；fingerprint mismatch 时，请求可信但语义不是同一次重试。

**和 ETag/乐观锁的边界**

ETag 或 version 表示资源状态：

```text
If-Match: version-7
```

它应该进入 fingerprint，因为 expected version 是业务语义的一部分。但 ETag 本身不是 fingerprint。两个不同 patch 都可以基于同一个 ETag，它们不是同一次请求。

**和 cache key 的边界**

cache key 为了复用响应，通常会覆盖 method、path、query、headers。payload fingerprint 为了防止 key 误用，覆盖业务写入语义。

把写请求 fingerprint 当缓存 key，会把“同内容的两个新请求”误认为一个请求。反过来，把缓存 key 当 fingerprint，又可能把无关 header 变化误判成 payload 变化。

**一句话**

payload fingerprint 是 idempotency key 记录下的内容一致性校验，不是请求身份、不是签名、不是缓存键、不是资源版本。它要和 key、签名、ETag、业务唯一键配合使用。

## Q053. payload fingerprint 在高并发场景下可能出现哪些隐藏问题？

**回答：**

高并发下，payload fingerprint 的问题通常不在 hash 算法本身，而在 fingerprint 什么时候确定、谁能写入、是否会被后续请求覆盖，以及规范化逻辑在多实例之间是否一致。

**问题一：fingerprint 被并发覆盖**

错误实现：

```text
upsert key -> set fingerprint = incoming_fingerprint
```

两个请求同时进来：

```text
request A: key=k1, amount=100, fingerprint=f1
request B: key=k1, amount=200, fingerprint=f2
```

如果后写覆盖前写，系统会丢掉第一次请求的语义。正确做法是 fingerprint 首次写入后不可变：

```text
insert only
on conflict: compare, never overwrite
```

**问题二：规范化版本不一致**

多实例滚动发布时，老版本和新版本可能生成不同 fingerprint：

```text
v1: null field kept
v2: null field removed
```

同一个 payload 打到不同实例，一个算出 f1，一个算出 f2，于是正常重试被判 mismatch。

解决方式：

- 存 `fingerprint_version`。
- 发布前做跨版本兼容测试。
- 同一 key 后续请求按旧记录的 version 重新计算。
- fingerprint 逻辑不要无版本静默变更。

**问题三：高并发大 payload 造成 CPU 尖峰**

canonicalization 可能比普通 JSON parse 更重：

```text
parse -> normalize -> sort keys recursively -> serialize -> hash
```

大 payload、高嵌套 JSON、大 map、高并发同时到来时，CPU 会上升。服务端如果在锁内做 fingerprint，还会把 CPU 开销变成锁等待。

比较稳的做法：

- 限制 payload size。
- 在进入数据库事务前计算 fingerprint。
- 对大 body 使用 content digest 或业务摘要。
- 对 schema 做白名单字段提取。

**问题四：同一语义不同表示**

高并发重试可能来自不同 SDK：

```text
Java sends 10.0
Go sends 10
JS sends 1e1
```

如果规范化不稳定，同一次业务意图会被误判。金额、时间、map 顺序、空字段都要提前定义。

**问题五：mismatch 风暴**

客户端 bug 可能让大量请求用同一个 key，但 payload 不同：

```text
same key reused by browser tab
payload changes on every submit
```

服务端会产生大量 mismatch，数据库和日志被打爆。要给 mismatch 做限流和观测，不能无限记录大 payload。

**问题六：fingerprint 计算和签名校验顺序**

如果服务端在认证、大小限制、content type 校验前就计算 fingerprint，攻击者可以用大 payload 消耗 CPU。顺序应是：

```text
auth basics / size limit / content type check
parse and validate
canonicalize and fingerprint
idempotency lookup
```

**一句话**

高并发下 payload fingerprint 的隐藏问题集中在不可变写入、版本一致性、CPU 尖峰、跨 SDK 表示差异、mismatch 风暴和校验顺序；fingerprint 必须先定义稳定规范，再进入并发路径。

## Q054. payload fingerprint 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

这些场景会暴露一个核心问题：第一次请求的 fingerprint 是否已经被持久保存，并且后续重试是否能用同一套规则重新计算。只要这两点不稳定，幂等判断就会漂。

**边界一：fingerprint 计算后，记录写入前崩溃**

流程：

```text
compute fingerprint=f1
process crashes before idempotency insert
```

这次请求对服务端没有持久痕迹。客户端重试时，服务端会重新计算 fingerprint。只要规范化 deterministic，结果仍是 f1，系统可以正常继续。

风险在于：fingerprint 依赖了当前时间、随机值或服务端默认值。重试时重新计算就不一样了。

**边界二：写入 fingerprint 后，业务未开始**

流程：

```text
insert key, fingerprint=f1, status=processing
crash before business mutation
```

重启后同 key 同 fingerprint 请求可以接管或重试；同 key 不同 fingerprint 应返回 mismatch。这里 fingerprint 已经成为这次 key 的语义锚点，不能因为业务没开始就让后续请求覆盖它。

**边界三：业务成功后，response 丢失**

客户端重试时：

```text
same key
same payload
same fingerprint
```

服务端应返回第一次结果。fingerprint 的作用是确认这确实是同一次请求，而不是用户拿旧 key 发了新 payload。

**边界四：规范化代码重启后变化**

重启本身不应该影响 fingerprint。但如果重启同时带了新版本代码，就可能出现：

```text
before restart: fingerprint_version=v1
after restart: fingerprint_version=v2
```

旧记录仍然要按 v1 比对，不能直接按 v2 算出 mismatch。表里必须存：

```text
fingerprint_version
algorithm
api_version
```

**边界五：默认值和服务端补全**

假设第一次请求：

```json
{"amount":100}
```

服务端补默认币种：

```json
{"amount":100,"currency":"USD"}
```

fingerprint 应该基于哪个？必须写清楚。通常更稳的是基于“校验后、默认值补齐后、业务语义明确后”的 canonical form。否则客户端重试时显式传 `currency=USD` 可能被误判不同。

**边界六：过期记录和重放**

fingerprint 跟 idempotency record 一起过期。过期后，同 key 同 payload 再来，系统可能当新请求。高价值业务需要业务唯一键或审计表兜底，不能把短期 fingerprint 当永久去重。

**一句话**

payload fingerprint 在崩溃和重试下要求计算规则 deterministic、首次写入后持久且不可变、算法有版本、默认值处理明确；否则同一次业务请求会在重启或版本切换后被误判成不同请求。

## Q055. payload fingerprint 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

payload fingerprint 的主瓶颈通常来自 CPU，其次是内存和 I/O。锁竞争一般是幂等记录写入造成的，不是 fingerprint 本身；网络只有在 fingerprint 依赖远程 schema、远程密钥或跨服务校验时才会明显。

**CPU**

CPU 开销来自：

```text
parse JSON
validate schema
apply defaults
normalize decimals/timestamps
sort object keys recursively
serialize canonical form
hash canonical bytes
```

RFC 8785 的 JCS 要求去掉空白、按规则序列化原始类型、递归排序 object 属性并输出 UTF-8。大对象和深层嵌套会让这些步骤变贵。

如果 payload 里有大 map：

```text
sort O(n log n)
```

高并发下，排序和序列化会比 SHA-256 本身更重。

**内存**

内存开销来自：

- 解析后的对象树。
- canonical bytes。
- 临时排序数组。
- 大 payload 的拷贝。
- mismatch 诊断快照。

如果实现里先把完整 body 读进内存，再构造完整 canonical string，大 payload 会压高 RSS 和 GC。更稳的是限制 body size，或者做流式 hash，不过流式 canonical JSON 很难，需要权衡。

**I/O**

I/O 来自存储：

```text
fingerprint_hash
fingerprint_version
canonical_payload_ref
payload sample for audit
```

如果只存 hash，I/O 很小；如果保存 canonical snapshot，表会膨胀。支付、风控、审计系统有时需要保存摘要或加密快照，但要设大小上限。

**锁竞争**

fingerprint 计算本身不该在数据库锁内做。错误做法：

```text
begin transaction
lock idempotency row
parse and canonicalize huge payload
compare fingerprint
commit
```

这会把 CPU 时间转成锁等待。正确做法是先完成本地计算，再进入短事务比较和写状态。

**网络**

正常情况下 fingerprint 是本地计算，不需要网络。网络瓶颈通常来自这些设计：

- 远程 schema registry。
- 远程 KMS 参与 HMAC。
- 跨服务查询默认值。
- 调用配置中心获取 normalization 规则。

这些依赖不应在每次请求主路径里实时访问。规则和 schema 应本地缓存，并带版本。

**一句话**

payload fingerprint 的瓶颈主要是 CPU 和内存，尤其是 canonicalization、排序、序列化和大对象复制；I/O 来自存储快照，锁竞争通常是实现把 fingerprint 放进事务锁内造成的。

## Q056. payload fingerprint 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

correctness test 测“语义相同是否同 fingerprint，语义不同是否不同 fingerprint”。stress test 测并发、版本切换和大 payload 下是否误判。benchmark 测 canonicalization、hash 和存储带来的成本。

**correctness test**

要构造等价输入：

```json
{"amount":100,"currency":"USD"}
```

```json
{
  "currency": "USD",
  "amount": 100
}
```

如果 API 定义它们语义相同，fingerprint 应相同。

再构造非等价输入：

```json
{"amount":100,"currency":"USD"}
```

```json
{"amount":200,"currency":"USD"}
```

fingerprint 必须不同。

还要测：

- `null` 和字段缺失的规则。
- 默认值补齐。
- decimal 表示，如 `10`、`10.0`、`10.00`。
- 时间戳时区和精度。
- map key 顺序。
- array 顺序是否保留。
- unordered set 是否排序。
- Unicode 表示。
- API version 变化。
- expected version / CAS 条件进入 fingerprint。
- trace id、request id、Date 不进入 fingerprint。

**stress test**

stress test 要覆盖：

```text
many concurrent same key + same payload
many concurrent same key + different payload
rolling deploy v1/v2 fingerprint code
very large JSON maps
deeply nested objects
many clients using different language SDKs
cache miss and DB conflict at same time
```

断言：

- 首次 fingerprint 不被覆盖。
- mismatch 不执行业务。
- v1 记录能用 v1 规则比较。
- 大 payload 不导致 OOM。
- 高并发 mismatch 不打爆日志。

**benchmark**

benchmark 要分层测：

```text
parse only
parse + normalize
parse + normalize + canonicalize
parse + normalize + canonicalize + hash
full request with idempotency DB write
```

测这些指标：

- payload size 与延迟。
- object key 数量与排序耗时。
- 嵌套深度与内存。
- p50/p95/p99。
- CPU profile。
- allocation rate。
- GC pause。
- canonical bytes size。
- mismatch path 成本。
- stored snapshot 成本。

**跨语言测试**

payload fingerprint 很容易被 SDK 差异打破。至少要有一组 golden tests：

```text
same semantic payload
Java/Go/Python/JavaScript clients
expected canonical bytes
expected fingerprint hash
```

只测服务端一种语言不够。

**一句话**

payload fingerprint 的 correctness test 测语义等价和差异，stress test 测并发和版本切换下是否误判，benchmark 测 canonicalization、排序、hash、内存分配和存储快照成本。

## Q057. 如果要求从零实现一个简化版 payload fingerprint，你会先定义哪些不变量？

**回答：**

先定义 fingerprint 的输入、规范化规则、版本和不可变性。不要先写 `hash(json.Marshal(payload))`，那只是一个容易漂的字节摘要。

**不变量一：同一语义请求生成同一 fingerprint**

如果 API 文档定义两个 payload 等价，它们必须得到同一 fingerprint：

```text
field order differs -> same
insignificant whitespace differs -> same
default value omitted or explicit -> depends on schema rule
```

等价关系必须由 schema 定义，不能让每个客户端猜。

**不变量二：不同业务语义生成不同 fingerprint**

这些字段变了，fingerprint 必须变：

- operation。
- resource id。
- tenant/account scope。
- amount、currency、recipient。
- expected version。
- quantity。
- API version。
- critical flags。

同 key 下这些字段变化，服务端应返回 mismatch。

**不变量三：非业务噪声不进入 fingerprint**

这些不应进入：

```text
trace_id
request_id
Date
User-Agent
Authorization
signature
retry_attempt
received_at
```

否则正常重试也会 mismatch。

**不变量四：canonical form 可复现**

对同一个 validated request：

```text
canonicalize(request) -> bytes
hash(bytes) -> fingerprint
```

这个过程必须 deterministic。服务重启、并发执行、不同实例执行，结果都要一致。

**不变量五：fingerprint 规则有版本**

表里要存：

```text
fingerprint_version
fingerprint_algorithm
api_version
```

算法升级后，旧记录仍按旧规则比对。

**不变量六：第一次记录不可被覆盖**

同一个 `(scope, idempotency_key)`：

```text
first fingerprint wins
later same fingerprint -> retry
later different fingerprint -> mismatch
```

任何 upsert 都不能更新已有 fingerprint。

**不变量七：非法输入不生成 fingerprint**

如果 payload 解析失败、schema 校验失败、数字非法、时间戳非法，不应该进入幂等执行路径。先返回 validation error。否则会把无效请求也写成持久幂等状态，后面很难解释。

**最小实现**

```text
1. parse request
2. validate schema
3. apply documented defaults
4. normalize decimal/time/map/set
5. build canonical object with whitelisted fields
6. canonical JSON encode
7. SHA-256 hash
8. store hash + version beside idempotency key
```

一句话：简化版 payload fingerprint 的不变量是语义等价同 hash、语义不同不同 hash、噪声字段排除、canonical form 可复现、规则版本化、首次 fingerprint 不可覆盖、非法输入不入幂等状态。

## Q058. payload fingerprint 的常见误用是什么，误用后通常会产生什么线上症状？

**回答：**

payload fingerprint 的误用一般有两种后果：正常重试被误判成 mismatch，或者不同业务请求被误合并。前者让用户失败，后者更危险，会造成错单、错扣、错发券。

**误用一：直接 hash raw body**

症状：

- 同一语义请求因为 JSON 字段顺序不同被拒绝。
- 不同 SDK 重试失败。
- 发布新客户端后 mismatch 突增。

原因是 raw bytes 对空白、字段顺序、转义方式敏感。

**误用二：把 timestamp、nonce、request id 放进 fingerprint**

症状：

- 每次重试 fingerprint 都不同。
- 幂等 key 形同虚设。
- `payload mismatch` 随 retry attempt 数增加。

这些字段应该用于签名、防重放或观测，不应决定业务 payload 是否相同。

**误用三：不把 expected version 放进去**

请求：

```text
update order with expected_version=7
update order with expected_version=8
same key
```

如果 fingerprint 不覆盖 expected version，服务端可能把两个不同 CAS 意图当成同一次请求。

线上症状：

- 乐观锁冲突被吞掉。
- 用户看到旧结果。
- 更新覆盖顺序难以解释。

**误用四：fingerprint 当全局 dedup key**

症状：

- 两个不同用户的相同请求被合并。
- 批量下单时少创建资源。
- 同 payload 的合法重复操作被误挡。

fingerprint 只能在 idempotency key 记录下比对，不能替代 key。

**误用五：算法无版本**

症状：

- 灰度发布期间 mismatch 增加。
- 回滚后旧请求无法比对。
- 去重表里的旧记录和新代码不兼容。

表里至少要有 `fingerprint_version`。

**误用六：保存完整敏感 payload**

症状：

- 幂等表出现 PII。
- 日志和备份泄露风险上升。
- 表膨胀，查询变慢。

可以存 hash、必要摘要、加密快照或 result_ref，不要默认存完整原文。

**误用七：mismatch 后仍继续执行业务**

这是严重 bug。同 key 不同 fingerprint 应该停止，不应该“按新请求继续”。线上症状是：

- 同一个 key 对应多个业务结果。
- 客户端重试得到互相矛盾的响应。
- 审计无法说明哪次请求生效。

**一句话**

payload fingerprint 常见误用包括 raw hash、噪声字段入 hash、漏掉版本条件、当全局去重键、无算法版本、保存敏感原文和 mismatch 后继续执行；线上症状是 mismatch 激增、正常重试失败、合法请求被误合并和审计不可信。

## Q059. payload fingerprint 在单机和分布式环境中的语义有什么差异？

**回答：**

单机里，payload fingerprint 只需要和本进程、本版本代码一致。分布式环境里，它必须跨实例、跨语言、跨版本、跨区域保持同一语义。难点从“算一个 hash”变成“维护一个稳定协议”。

**单机环境**

单机系统通常只有：

```text
one process
one code version
one local database
one serializer
```

只要重启前后代码没变，fingerprint 基本稳定。即便用语言默认 JSON 序列化，问题也可能短期不暴露。

但单机仍然要注意：

- 进程重启后不能改变默认值规则。
- map 遍历顺序不能随机影响结果。
- 本地时间不能进入 fingerprint。
- 版本升级要兼容旧记录。

**分布式环境**

分布式环境会多出这些变量：

```text
multiple service instances
rolling deploy
multiple SDK languages
multiple regions
multiple schema versions
replicated idempotency store
```

同一次重试可能第一次打到 v1 Go 实例，第二次打到 v2 Java 实例。fingerprint 规则必须是一份协议，而不是某个语言的默认序列化副产物。

**跨语言**

不同语言对这些字段处理不同：

- map order。
- float serialization。
- timezone。
- Unicode escaping。
- null 和 missing。
- decimal scale。

所以要有 canonicalization 和 golden tests。RFC 8785 JCS 是 JSON 场景的一个参考，但业务默认值、集合排序、decimal 仍要由 schema 说明。

**跨版本**

滚动发布时，服务端可能同时运行两套 fingerprint 逻辑。要避免：

```text
first request saved with v1
retry handled by v2
v2 computes different fingerprint
return mismatch
```

解决方式：

- 存版本。
- 新版本能计算旧版本 fingerprint。
- 先发布兼容代码，再切换默认版本。
- 对 fingerprint 变更做迁移计划。

**跨区域**

跨区域还要处理复制延迟：

```text
region A saves key + fingerprint
retry reaches region B before replication
region B sees key missing
```

这不是 fingerprint 独有问题，但 fingerprint 必须跟 idempotency record 一起复制，并保持同一作用域语义。

**一句话**

单机中 payload fingerprint 更像本地校验函数；分布式中它是跨实例、跨语言、跨版本的协议。没有 canonicalization、版本字段和 golden tests，重试会在不同节点上得到不同判断。

## Q060. retry storm 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

retry storm 没有“核心目标”。它不是要实现的机制，而是系统故障形态。面试里如果被这样问，应该先纠正：要讨论的是 retry policy、retry control 或 retry storm mitigation。它们主要解决可用性和性能保护，同时也会影响正确性。

**retry 和 retry storm 的区别**

retry 的目标是提高临时故障下的成功率：

```text
transient failure -> retry after backoff -> request succeeds
```

retry storm 是 retry 失控后的结果：

```text
downstream slow
-> upstream timeout
-> many clients retry
-> downstream receives more load
-> downstream slower
-> more timeout
```

所以 retry 是机制，retry storm 是故障。

**主要影响什么**

| 维度 | retry storm 的影响 |
| --- | --- |
| 性能 | 放大 QPS、排队、p99、连接池等待 |
| 正确性 | 非幂等写可能重复执行 |
| 可用性 | 局部故障变成级联故障 |
| 安全性 | 通常不是核心，但可能被滥用成放大攻击 |
| 可维护性 | 指标失真，排障困难 |

如果问“治理 retry storm 主要解决什么”，答案是性能和可用性；如果涉及写请求，还会直接影响正确性。

**官方资料里的边界**

AWS Builders Library 提到，多层调用各自重试会乘法放大，5 层每层 3 次重试会让底层数据库看到 243 倍请求。Google SRE 也强调，重试可能让 backend 保持在过载状态，并建议随机指数退避、限制每请求重试次数、server-wide retry budget，以及避免多层放大。

这说明 retry storm 不是“重试次数多一点”，而是反馈环。

**一句话**

retry storm 本身不是目标；retry policy 的目标是提高短暂故障下的成功率，retry storm mitigation 的目标是保护性能和可用性，并防止非幂等操作在风暴中重复生效。

## Q061. retry storm 的典型适用场景和不适用场景分别是什么？

**回答：**

retry storm 没有适用场景。它是事故，不是设计模式。真正有适用场景的是“受控重试”：少量、可预算、带退避和 jitter、只针对可恢复错误的 retry。

**受控重试适用场景**

适合：

- 短暂网络抖动。
- 临时连接断开。
- leader 选举或 failover。
- 下游返回明确可重试错误。
- 幂等读请求。
- 带 idempotency key 的写请求。
- 有总 deadline 和 retry budget 的 RPC。

这类场景的共同点是：重试有机会成功，并且不会显著增加副作用风险。

**不适合重试的场景**

不适合：

- 参数错误。
- 权限错误。
- 认证失败。
- payload mismatch。
- 非幂等写请求且没有 idempotency key。
- 下游已经明确 overload，且没有 `Retry-After` 或预算。
- 请求已经超过 deadline。
- 客户端没有能力控制 retry rate。

这些情况重试只会浪费资源或制造重复副作用。

**retry storm 的典型触发场景**

虽然没有“适用场景”，但有常见触发场景：

```text
fixed interval retry by many clients
every layer retries independently
timeout too low
no jitter
no retry budget
client ignores Retry-After
batch job retries all failed tasks immediately
mobile clients reconnect together after network recovery
```

这些都应该被设计规避。

**一句话**

retry storm 没有适用场景；适用的是受控重试。重试只应服务于短暂、可恢复、可预算的故障，并且写请求要有幂等保护。

## Q062. retry storm 和相近概念最容易混淆的边界在哪里？

**回答：**

retry storm 容易和高 QPS、流量突增、thundering herd、缓存雪崩、DDoS、队列堆积混在一起。区分点是：retry storm 的流量主要来自失败后的再次尝试，而不是新的用户需求。

**概念边界**

| 概念 | 核心特征 | 和 retry storm 的区别 |
| --- | --- | --- |
| 高 QPS | 用户真实请求多 | retry storm 是 attempt 多，logical request 不一定多 |
| thundering herd | 大量 worker 同时被唤醒或抢同一资源 | retry storm 的同步点来自失败重试 |
| 缓存雪崩 | 大量缓存同时失效打后端 | 可能触发 retry storm，但不是同一个概念 |
| DDoS | 恶意外部流量消耗资源 | retry storm 常来自正常客户端和系统策略 |
| backpressure | 系统主动让上游变慢 | 是治理手段，不是风暴 |
| retry amplification | 重试带来的放大倍数 | 是 retry storm 的度量之一 |
| cascading failure | 故障沿依赖链扩散 | retry storm 是常见诱因 |

**关键指标区别**

retry storm 的典型指标是：

```text
attempt QPS rises faster than logical request QPS
retry_attempt_total rises
timeout rate rises before user traffic rises
same operation_id appears multiple attempts
downstream errors cause upstream retries
```

如果 logical request QPS 和 attempt QPS 同步上涨，可能是真实流量高峰。如果 logical request 稳定但 attempt 暴涨，更像 retry storm。

**和 thundering herd 的关系**

两者可以叠加：

```text
cache expires
many clients miss
backend slows
clients timeout and retry
```

前半段是 herd/cache avalanche，后半段是 retry storm。排障时要找到反馈环从哪里开始。

**一句话**

retry storm 的边界在“重复尝试放大了同一批逻辑请求”；它和高 QPS、缓存雪崩、DDoS、级联故障可能同时出现，但识别它要看 attempt/logical request 比例和 retry reason。

## Q063. retry storm 在高并发场景下可能出现哪些隐藏问题？

**回答：**

高并发下 retry storm 的隐藏问题是很多资源会被重复 attempt 占满，而监控看起来只是“流量变高”。真正危险的是系统把失败请求、重试请求和正常新请求混在一起处理。

**问题一：连接池被慢请求占满**

下游变慢后，上游连接不释放：

```text
attempts wait on downstream
connections held longer
new attempts wait for connections
timeout increases
more retries start
```

连接池等待本身也会变成延迟来源。

**问题二：线程和协程堆积**

每个 attempt 都占资源：

- goroutine。
- future/promise。
- thread stack。
- request context。
- timer。
- log buffer。

当 retry attempt 数涨到 logical request 的几倍，内存和调度开销会先于 CPU 报警。

**问题三：热点 idempotency key 被打爆**

同一个写请求超时后，客户端大量重试同一个 key：

```text
key=k1, attempts=1000
```

幂等表同一行锁、缓存同一 key、日志同一 operation 都变成热点。

**问题四：错误日志反向放大**

每次 retry 都打错误日志：

```text
timeout log
retry log
downstream failed log
```

日志系统、trace collector、metrics pipeline 也可能被拖垮。事故时日志丢失，反而更难排障。

**问题五：公平性被破坏**

重试多的客户端会占更多资源。AWS Builders Library 说 retries are selfish，就是这个意思：重试是在要求服务为同一个请求花更多资源。

没有 per-client、per-tenant、per-operation retry budget 时，激进客户端会挤掉正常客户端。

**问题六：负载均衡误判**

重试会把流量打向剩余健康实例：

```text
instance A slow
LB shifts traffic to B/C
B/C receive original traffic + retry traffic
B/C also slow
```

局部故障变成整体故障。

**一句话**

高并发 retry storm 的隐藏问题是连接池、线程、内存、幂等热点、日志系统和租户公平性一起失控；它不是单纯 QPS 高，而是 attempt 流量挤占了正常业务资源。

## Q064. retry storm 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

这些场景会暴露 retry policy 是否真的有状态边界。很多系统平时看起来只有几次重试，一旦进程重启、客户端超时或批量任务恢复，重试状态丢失，风暴就开始了。

**崩溃后重试状态丢失**

如果 retry budget 只在内存里：

```text
process restarts
retry counters reset
all failed operations retry again
```

批处理任务、消息消费者、移动端同步都容易这样。需要持久化或分片级预算，至少要有启动后的 warm-up 和速率限制。

**重启后的同步启动**

服务重启后，所有 worker 同时恢复：

```text
restart at t=0
all workers resume failed jobs
all clients reconnect
all timers fire
```

如果没有 jitter，恢复动作会同步打下游。定时任务、连接重建、批量重放都要加随机化。

**超时后原请求仍在执行**

客户端 timeout 后发起 retry，但服务端第一次 attempt 还没停止：

```text
attempt 1 still running
attempt 2 starts
attempt 3 starts
```

这会让同一逻辑请求并发执行。写请求必须带 idempotency key，RPC 必须传播 cancellation/deadline，服务端要在检查点停止过期工作。

**队列 replay 后重复风暴**

消费者崩溃后，未 ack 的消息被重新投递：

```text
consumer group restarts
many messages redeliver
each message triggers downstream retry
```

如果每条消息还会调用多个下游，恢复时会形成 replay storm。要限制恢复速率，按分区、租户、下游设置并发上限。

**错误分类丢失**

重启后，系统可能只记得“失败了”，不记得失败原因：

```text
400 validation error
saved as generic failure
restart
retry as transient error
```

永久错误被当成临时错误重试，会产生无意义流量。

**一句话**

retry storm 在崩溃和重启时暴露的是 retry 状态、预算、错误分类、in-flight 请求和恢复速率的边界；这些状态不能只靠内存计数和固定间隔重试来维持。

## Q065. retry storm 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

retry storm 的瓶颈通常先出现在网络、连接池、线程/内存和下游 I/O 上。CPU 也会上升，但它常常是错误处理、日志、序列化和调度被放大后的结果。锁竞争则出现在热点资源和幂等表上。

**网络和连接**

retry storm 会直接增加：

- outbound connection 数。
- TLS handshake。
- DNS 查询。
- socket wait。
- request/response bytes。
- load balancer 连接压力。

下游越慢，上游连接占用越久。连接池满后，新请求排队，进一步触发 timeout。

**内存和线程**

每个 attempt 都有上下文：

```text
request object
timeout timer
retry state
trace span
log fields
future/goroutine/thread
```

attempt 数翻倍，内存和调度压力也会翻倍。Google SRE 在过载章节里提到，工作堆积会导致任务耗尽内存或陷入内存抖动，延迟随之恶化。

**I/O**

下游数据库、缓存、日志、消息队列都会被重复请求打到：

- 重复查询。
- 重复锁等待。
- 重复写失败状态。
- 重复 outbox 记录。
- 重复日志写入。

如果失败路径也写数据库，例如记录每次失败 attempt，I/O 会更快打满。

**锁竞争**

锁竞争来自：

- 同一个 idempotency key。
- 同一个账户余额。
- 同一个库存行。
- 同一个 rate limit bucket。
- 同一个 circuit breaker 状态。
- 同一个日志或 metrics buffer。

retry storm 会把低概率冲突变成高概率冲突。

**CPU**

CPU 常见来源：

- JSON parse/serialize。
- TLS。
- retry policy 计算。
- logging/tracing。
- exception handling。
- high-cardinality metrics。
- GC。

CPU 高不一定是根因。可能只是大量无效 attempt 的症状。

**一句话**

retry storm 的性能瓶颈多半先落在网络、连接池、内存线程和下游 I/O，随后放大 CPU 和锁竞争；排障时要看 attempt QPS、连接池等待、队列长度、下游 p99 和 retry amplification，而不只看 CPU。

## Q066. retry storm 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

这里测试的不是“实现一个风暴”，而是测试 retry policy 和 storm mitigation 能不能阻止风暴。correctness test 测重试决策是否正确，stress test 测过载时是否失控，benchmark 测重试控制的资源成本。

**correctness test**

要覆盖：

```text
retryable error -> retry
non-retryable error -> no retry
deadline exceeded -> no retry after budget exhausted
Retry-After -> respected
idempotency required for unsafe write
max attempts enforced
retry budget enforced
exponential backoff increases
jitter produces distributed retry times
```

还要测多层调用：

```text
only one layer retries
downstream client does not secretly retry again
```

**stress test**

构造过载：

```text
downstream latency increases
downstream returns 500/503/429
network timeout rate rises
many clients fail at once
cache miss surge
consumer group restart and replay
```

观察：

- attempt/logical ratio 是否有上限。
- retry budget 是否生效。
- retry 时间是否被 jitter 打散。
- 下游 QPS 是否被限住。
- 正常请求是否仍有配额。
- 写请求是否没有重复副作用。
- 系统是否 fail closed，而不是绕过幂等保护。

**benchmark**

benchmark 要测不同策略：

```text
no retry
fixed interval retry
exponential backoff
exponential backoff + jitter
retry budget
client-side throttling
circuit breaker
bulkhead
```

指标：

- 成功率。
- p50/p95/p99。
- total attempts。
- downstream QPS。
- connection pool wait。
- CPU/memory。
- rejected locally。
- rejected by backend。
- time to recovery。

benchmark 的重点不是让 retry 数越多越好，而是在 transient failure 下提高成功率，同时不让 overload 恢复时间变长。

**一句话**

retry storm 相关 correctness test 测策略是否按语义重试，stress test 测过载和恢复时是否放大流量，benchmark 测不同重试控制策略的成功率、延迟、attempt 放大和恢复时间。

## Q067. 如果要求从零实现一个简化版 retry storm，你会先定义哪些不变量？

**回答：**

这个问题应该先纠正：不应该实现 retry storm，应该实现一个简化版 retry controller，并保证它不会制造 storm。要先定义 retry 的安全不变量。

**不变量一：每个 logical request 有总 deadline**

```text
total_deadline = start + budget
```

所有 attempt 共享这个 deadline。任何 retry 都不能把请求生命周期无限延长。

**不变量二：每个 logical request 有最大 attempt 数**

```text
attempt_no <= max_attempts
```

max attempts 包括第一次请求。不要把“重试 3 次”理解成总共 4 次还是 3 次不清楚，配置要明确。

**不变量三：全局或本地 retry budget 有上限**

```text
retry_attempts <= original_requests * ratio + burst
```

预算耗尽后，客户端应该 fail fast 或返回受控错误，不再向下游发请求。

**不变量四：只有 retryable error 能重试**

可重试：

```text
transient network error
503 with Retry-After
timeout before side effect starts, if operation is safe
```

不可重试：

```text
validation error
auth error
payload mismatch
non-idempotent write without key
deadline exhausted
```

**不变量五：backoff 必须带 jitter**

```text
sleep = jitter(backoff(attempt))
```

固定间隔会同步客户端，capped backoff 没有 jitter 也会在 cap 处重新同步。

**不变量六：写请求必须有幂等保护**

如果操作有副作用：

```text
no idempotency key -> do not auto retry
```

AWS Builders Library 也强调，有副作用的 API 如果没有幂等机制，重试并不安全。

**不变量七：多层不能各自无限重试**

系统要约定 retry ownership：

```text
only edge layer retries
or only SDK retries
or only workflow engine retries
```

不能每一层都默认 retry。

**不变量八：过载信号要被尊重**

```text
429/503 + Retry-After
circuit breaker open
bulkhead full
retry budget exhausted
```

这些信号应减少请求，而不是触发更激进 retry。

**一句话**

从零实现的不是 retry storm，而是 retry controller；先定义总 deadline、最大 attempt、retry budget、错误分类、jitter、幂等要求、retry ownership 和过载信号这几个不变量。

## Q068. retry storm 的常见误用是什么，误用后通常会产生什么线上症状？

**回答：**

retry storm 的常见误用，本质上是把 retry 当成“失败就再试”，没有错误分类、预算、deadline、jitter 和幂等保护。线上症状往往先表现为延迟抖动，随后变成资源耗尽和级联故障。

**误用一：固定间隔重试**

症状：

- QPS 出现周期性尖峰。
- 下游 p99 周期性升高。
- 大量客户端在同一秒 retry。
- 短故障恢复变慢。

**误用二：每层都自动重试**

症状：

- 下游 QPS 远高于入口 QPS。
- 数据库请求数按倍数放大。
- trace 里一条用户请求出现大量 attempt。
- outage 时恢复不了，因为 retry 一直压着下游。

**误用三：没有 jitter**

症状：

- backoff 到 cap 后请求重新同步。
- 客户端恢复时一起冲击服务。
- 定时任务和 retry 同时打峰。

**误用四：不区分错误类型**

把 400、401、403、payload mismatch、validation error 都重试。

症状：

- 永久错误 QPS 高。
- 日志里同一错误重复出现。
- 用户请求失败时间变长，但成功率不变。
- 后端浪费资源处理不可能成功的请求。

**误用五：非幂等写自动重试**

症状：

- 重复扣款。
- 重复订单。
- 重复发券。
- 外部 provider duplicate 冲突。
- 对账异常。

**误用六：忽略 `Retry-After` 和限流**

症状：

- 服务端越限流，客户端越重试。
- 429/503 比例升高。
- 客户端本地成功率下降。
- 后端大量资源花在拒绝请求上。

Google SRE 的 client-side throttling 章节也提到，如果拒绝请求本身也消耗资源，大量被拒请求仍然会拖垮后端，所以客户端要在本地自我限流。

**误用七：没有观测 attempt**

症状：

- 看 QPS 以为用户流量涨了。
- 看错误率以为下游坏了。
- 找不到 retry 是原因还是结果。
- 事故复盘只能猜。

**一句话**

retry storm 的误用包括固定间隔、多层重试、无 jitter、错误不分类、非幂等写重试、忽略限流信号和缺少 attempt 指标；线上症状是 QPS 放大、p99 变差、连接池耗尽、重复副作用和恢复时间拉长。

## Q069. retry storm 在单机和分布式环境中的语义有什么差异？

**回答：**

单机里的 retry storm 多半是一个进程、一个队列或一个依赖被重试打满。分布式环境里，retry storm 会跨客户端、网关、服务层、消息队列和区域传播，流量放大不再是线性的，而是层层乘起来。

**单机环境**

单机通常表现为：

```text
worker retries failed jobs too fast
thread pool full
local queue grows
same dependency receives repeated calls
```

治理手段相对直接：

- 限制 worker 并发。
- 指数退避和 jitter。
- 本地 token bucket。
- 最大 attempt。
- 死信队列。
- 本地 circuit breaker。

单机能看到比较完整的状态，retry budget 也容易做成进程内计数。

**分布式环境**

分布式环境有多个重试源：

```text
browser/mobile SDK
API gateway
service mesh
service client
message consumer
workflow engine
batch job
```

每层都可能觉得自己“只重试几次”。合起来就是乘法放大。AWS Builders Library 的 243 倍例子和 Google SRE 的多层 retry 放大，都指向这个问题。

**状态不可见**

单机能知道自己已经 retry 了几次。分布式里，每一层看到的只是局部失败：

```text
gateway sees service timeout
service sees DB timeout
consumer sees provider timeout
client sees gateway timeout
```

如果没有统一的 logical request id、attempt metadata 和 retry budget，各层无法知道总 attempt 数。

**恢复会同步**

分布式客户端常常一起恢复：

- 移动网络恢复。
- 服务滚动重启结束。
- DNS 恢复。
- 缓存集群恢复。
- 队列消费者全部重新上线。

没有 jitter 时，这些恢复动作会同步撞下游。

**跨区域更复杂**

跨区域 failover 后，流量会带着 retry 一起转移：

```text
region A slow
clients retry to region B
region B receives normal traffic + failover traffic + retry traffic
```

如果 region B 的 retry budget 不知道 region A 的失败历史，也会继续放大。

**治理差异**

单机治理偏本地：

```text
local queue limit
local token bucket
local worker cap
```

分布式治理要有协议：

```text
deadline propagation
retry ownership
attempt metadata
client-side throttling
server-side load shedding
criticality
per-tenant quotas
idempotency key for writes
```

**一句话**

单机 retry storm 是本地资源被重复 attempt 打满；分布式 retry storm 是多个层次、多个客户端和多个区域把局部失败乘法放大。分布式场景必须靠 deadline、retry budget、jitter、限流、观测和幂等语义一起控制。

## 参考和校验点

- [RFC 9110: HTTP Semantics](https://www.rfc-editor.org/rfc/rfc9110.html) 定义 HTTP 方法的 safe、idempotent、cacheable 语义，并说明幂等关注的是请求对服务器资源的预期效果。
- [Stripe Docs: Idempotent requests](https://docs.stripe.com/api/idempotent_requests) 说明客户端生成 idempotency key、保存第一次请求结果、比较重复请求参数，以及至少 24 小时后可以清理 key。
- [AWS EC2 Developer Guide: Ensuring idempotency in Amazon EC2 API requests](https://docs.aws.amazon.com/ec2/latest/devguide/ec2-api-idempotency.html) 说明 client token、regional/zonal idempotency 和 `IdempotentParameterMismatch`。
- [AWS Builders Library: Timeouts, retries, and backoff with jitter](https://aws.amazon.com/builders-library/timeouts-retries-and-backoff-with-jitter/) 说明超时、重试、重试放大、指数退避和 jitter 的工程风险。
- [IETF draft: The Idempotency-Key HTTP Header Field](https://datatracker.ietf.org/doc/draft-ietf-httpapi-idempotency-key-header/) 是 HTTP API 幂等 key 的 IETF 草案；截至 2026-04-18 已过期，可作为设计参考，不应写成正式 RFC。
- [gRPC Docs: Deadlines](https://grpc.io/docs/guides/deadlines/) 说明 deadline、timeout、`DEADLINE_EXCEEDED`、服务端取消和 deadline propagation 的边界。
- [gRPC Docs: Cancellation](https://grpc.io/docs/guides/cancellation/) 说明客户端取消、deadline 触发的取消，以及服务端 handler 需要协作式停止处理。
- [Go `context` package](https://pkg.go.dev/context) 定义 deadline、取消信号、`Canceled` 和 `DeadlineExceeded` 等请求范围控制语义。
- [Apache Kafka Design](https://kafka.apache.org/40/design/design/) 说明消息投递语义、consumer offset 以及 Kafka 对 at-least-once、at-most-once 和 exactly-once 语义的边界。
- [Apache Kafka Producer Configs](https://kafka.apache.org/40/configuration/producer-configs/) 说明 producer idempotence、`delivery.timeout.ms`、backoff jitter 和 `transactional.id`。
- [Apache Kafka Transaction Protocol](https://kafka.apache.org/40/operations/transaction-protocol/) 说明 Kafka 事务协议、producer epoch 和事务边界内的重复写防护。
- [PostgreSQL Documentation: Transaction Isolation](https://www.postgresql.org/docs/current/transaction-iso.html) 说明事务隔离级别、Serializable 语义，以及序列变化不随事务 abort 回滚的边界。
- [Amazon DynamoDB: Optimistic locking with version number](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/DynamoDBMapper.OptimisticLocking.html) 说明基于版本号的乐观锁、版本递增和条件检查失败语义，并提示 global tables 的 last writer wins 边界。
- [Amazon DynamoDB: Condition expressions](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/Expressions.ConditionExpressions.html) 说明 `attribute_not_exists`、conditional put/update/delete 等条件写模式。
- [Amazon SQS: At-least-once delivery](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/standard-queues-at-least-once-delivery.html) 说明标准队列可能再次投递消息副本，并要求应用处理同一消息多次时保持幂等。
- [RFC 8785: JSON Canonicalization Scheme](https://www.rfc-editor.org/rfc/rfc8785.html) 说明为 hashing/signing 生成稳定 JSON 表示的规则，包括确定性属性排序、数字序列化和 UTF-8 输出。
- [RFC 9421: HTTP Message Signatures](https://www.rfc-editor.org/rfc/rfc9421.html) 说明 HTTP 签名基底由被覆盖的 message components 构成，可用于讨论 `Idempotency-Key` 是否应参与请求签名。
- [Google SRE Book: Addressing Cascading Failures](https://sre.google/sre-book/addressing-cascading-failures/) 说明级联故障、load shedding、graceful degradation、RPC deadline 和 naive retry 的风险。
- [Google SRE Book: Handling Overload](https://sre.google/sre-book/handling-overload/) 说明 client-side throttling、adaptive throttling、criticality 和过载控制。
- [RFC 6585: Additional HTTP Status Codes](https://www.rfc-editor.org/rfc/rfc6585.html) 定义 `428 Precondition Required` 和 `429 Too Many Requests`，并说明 `Retry-After` 可用于限流响应。
- [Resilience4j CircuitBreaker](https://resilience4j.readme.io/docs/circuitbreaker) 说明 circuit breaker 的 CLOSED、OPEN、HALF_OPEN 状态、失败率/慢调用阈值和滑动窗口。
- [Resilience4j Bulkhead](https://resilience4j.readme.io/docs/bulkhead) 说明 bulkhead 的最大并发数、等待时长、线程池大小和队列容量等隔离参数。
- [Resilience4j RateLimiter](https://resilience4j.readme.io/docs/ratelimiter) 说明 rate limiter 的 permission、刷新周期、周期内配额和运行时调参。
- [gRPC Docs: OpenTelemetry Metrics](https://grpc.io/docs/guides/opentelemetry-metrics/) 说明 gRPC per-call、per-attempt、retry、transparent retry、hedging 和 retry delay 等可观测指标。
