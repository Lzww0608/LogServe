# 十二、高频链式追问脚本：从 exactly-once-ish 开始

这一组问题要先把边界放稳。LogServe 没有宣称严格 exactly-once execution。它做的是：提交请求幂等、日志追加幂等、任务完成提交去重、workflow final result 去重、actor 状态变更按 command_seq 串行提交。worker 侧执行仍然可能重复，尤其在超时、redelivery、worker crash、客户端重试这些场景里。

我在面试里会先说清楚这句话：

> LogServe 追求的是 deduplicated commit，不是物理层面的 single execution。执行可以多次发生，系统尽量保证同一个逻辑操作只提交一次最终状态。

## Q846. 你为什么不说 exactly-once？

因为严格 exactly-once 很容易被说过头。只要系统里有 worker、网络、外部服务和用户代码，就很难保证“函数只运行一次”。worker 可能在执行完成后还没来得及 `CompleteTask` 就断连；control plane 可能因为 lease timeout 把任务重新投递；客户端可能因为 gRPC timeout 重试提交。站在平台外部看，同一段用户代码有可能跑两遍。

LogServe 现在能保证的是另一件事：同一个逻辑操作尽量只提交一次最终结果。这个范围更准确。

具体到代码路径：

- 提交 task/workflow/actor/LLM 时，用户可以传 `idempotency_key`。
- control plane 计算 `idempotency_fingerprint`，同 key 同 payload 返回已有对象，同 key 不同 payload 报 conflict。
- shared log 的 `AppendLog` 也有 idempotency key，同一个 stream 内重复 append 会返回旧 record。
- `CompleteTask` 会检查 terminal 状态，已经成功或失败的 task 不再被新 completion 覆盖。
- workflow step 成功事件使用 `workflow_id + step_id + input_hash + succeeded` 去重。
- actor 状态变更使用 `actor_id + actor_call_id + applied` 去重，并用 `command_seq` 保证顺序。

所以我用 exactly-once-ish。它表达的是工程上的边界：执行路径是 at-least-once，提交路径做幂等和 fencing，最终状态尽量只推进一次。这个说法比直接说 exactly-once 更诚实，也更容易经得住追问。

## Q847. worker 可能执行多次，那用户函数有副作用怎么办？

副作用要分两类看。

第一类是平台内部副作用，比如写 task result、更新 workflow step、更新 actor state、写 LLM stats。这些由 LogServe 控制，可以通过 shared log、idempotency key、lease epoch、terminal guard 来限制重复提交。

第二类是用户函数里的外部副作用，比如扣款、发邮件、调用第三方 API、写用户自己的数据库。平台不能凭空让这些副作用严格只发生一次。worker 执行到一半时，外部系统可能已经完成扣款，但 worker 还没把结果提交回来；这时平台重投任务，用户函数再跑一遍，外部系统就可能被调用第二次。

正确做法是让副作用接口自己幂等。常见方式有几种：

- 外部 API 支持 idempotency key，例如支付系统里的 request id。
- 用户把 `task_id`、`workflow_id`、`step_id`、`actor_call_id` 当作外部请求的幂等键。
- 用户业务库对 `(operation_id)` 建唯一索引，重复写入时返回已有结果。
- 对发邮件这类动作，先写 outbox 表，再由独立 worker 根据唯一 key 发送。
- 对不可逆操作，任务函数要把“是否已经执行过”放在外部系统可检查的位置。

LogServe 可以提供上下文和规范，比如把 task id、attempt、workflow step id 暴露给 SDK，让用户更容易写幂等代码。但平台不能替用户控制一个完全不支持幂等的外部系统。

面试回答里可以直说：

> 对外部副作用，平台最多把重复执行风险收敛到可识别的 operation id 上。真正的 exactly-once 要外部系统配合。

## Q848. 结果去重和执行去重有什么区别？

执行去重是“代码不要跑第二次”。结果去重是“就算代码跑了第二次，也不要提交第二个最终状态”。这两个差很多。

执行去重需要在调度和 worker 层保证同一任务不会被两个 executor 同时执行，也不会在超时误判后重投。分布式环境里这很难。worker crash、网络分区、GC pause、control restart 都可能让控制面无法确定任务到底还在不在跑。

结果去重更现实。它承认 worker 可能重复执行，然后把幂等点放在提交层：

- task 完成时，`CompleteTask` 检查 task 是否已经 terminal。
- stale lease epoch 的 completion 会被拒绝。
- workflow step 成功时，用 step 级 idempotency key 合并重复成功事件。
- actor command applied 时，用 call id 和 command_seq 防止重复推进 actor state。
- LLM stats materializer 避免 duplicate completion 重复计数。

举个例子，某个 workflow step 被 worker A 和 worker B 都执行了。执行层已经重复了，执行去重失败。但只要只有一个 `StepSucceeded` 事件真正影响 workflow state，最终结果就没有重复提交。这个叫结果去重。

LogServe 当前主线就是结果去重。它没有假装 worker 永远只执行一次，而是在完成、状态推进、事件 replay 这些地方做防线。

## Q849. workflow step 去重为什么使用 workflow_id + step_id + input_hash？

这个 key 的含义是：同一个 workflow 里，同一个 step，在同一组输入下，只应该有一个成功结果。

`workflow_id` 用来限定作用域。两个 workflow 即使 step 名相同，也不能互相去重。比如两个用户同时跑 `simple_rag`，里面都有 `embed` step，它们的执行结果不能混在一起。

`step_id` 标识 DAG 节点。workflow 里每个 step 都有自己的语义位置。`embed(query)` 和 `search(vec)` 即使输入 JSON 刚好长得像，也不能用同一个结果。

`input_hash` 处理输入变化。动态 workflow 或 retry 场景里，同一个 step_id 在不同输入下不能复用旧结果。比如 `search(vec)` 的上游 embedding 变了，`step_id` 仍然叫 `search`，但输入已经不同，必须产生新的结果。

成功事件的 idempotency key 不带 attempt，这是刻意的。retry 的不同 attempt 代表执行过程不同，但只要 step 和输入相同，最终成功结果只需要提交一次。这样可以处理这种情况：第一次 attempt 提交成功但响应丢了，第二次 attempt 又提交同样结果，日志层会把它当成同一个逻辑成功。

失败事件通常会带 task_id 或 attempt，因为失败是某次尝试的事实，需要保留每次尝试的错误信息。成功结果面向逻辑 step，失败结果面向具体 attempt，这两个粒度不一样。

## Q850. actor command 去重为什么使用 actor_id + actor_call_id + applied？

actor 的状态更新是串行状态机。一次 actor call 最终对应一次状态推进，所以 applied 事件必须绑定到“哪个 actor 的哪次 call”。

`actor_id` 是作用域。不同 actor 之间的 call id 可能相同，不能互相影响。

`actor_call_id` 是这次调用的逻辑身份。客户端重试、control 重试、log append 重试，都应该复用同一个 call id 或由同一个 idempotency key 映射到同一个 call。这样重复提交时，系统知道它们是同一次 actor command。

`applied` 用来区分阶段。actor stream 里有 `ActorCommandSubmitted` 和 `ActorCommandApplied`。submitted 表示命令进入 mailbox，applied 表示命令已经执行并产生新状态。两者不能共用一个 idempotency key，否则提交事件和应用事件会互相覆盖。

`command_seq` 再加一层顺序约束。即使 command 2 比 command 1 先完成，control plane 也不会让它先 apply。代码里要求 completion 的 `command_seq == actor.command_count + 1`。不满足这个条件，completion 会被拒绝。

所以 `actor_id + actor_call_id + applied` 解决的是重复 apply，`command_seq` 解决的是乱序 apply。两者一起用，actor 才能在并发提交下保持单 actor 内部状态线性推进。

## Q851. 如果旧 attempt 成功晚于新 attempt，会不会覆盖结果？

正常情况下不应该覆盖。这里有几层防线。

第一层是 task lease epoch。任务被 lease 给 worker 时，metadata 里的 `TaskLeaseEpoch` 会递增。worker 完成时必须带上 worker_id 和 lease epoch。任务已经 redelivery 给新 worker 后，旧 worker 带旧 epoch 调用 `CompleteTask`，会被视为 stale completion。

第二层是 terminal guard。metadata 的 `CompleteTask` 看到 task 已经是 `SUCCEEDED` 或 `FAILED`，会直接返回已有 terminal 状态，不会让后来的 completion 改写结果。

第三层是 workflow step 的成功幂等键。`StepSucceeded` 的 key 是 `workflow_id + step_id + input_hash + succeeded`。同一个 step 同一输入下，多次成功提交会合并到同一条逻辑成功事件。

actor 路径还有额外防线：owner worker、actor epoch、command_seq 都要匹配。旧 owner 或旧 command_seq 迟到，不能推进 actor state。

有一个生产级细节要讲清楚：workflow retry 里最好还要显式校验 completion 对应的 `task_id/attempt` 是否仍是当前 step 的 active attempt。当前项目已经有 lease epoch、terminal guard 和 step success 去重，能覆盖主要重复 completion；如果要把这块做得更硬，我会在 `completeWorkflowStep` 里增加 active task/attempt fencing。这样旧 attempt 即使迟到，也只能被记录为历史尝试，不能影响当前 step。

这类回答不能只说“不会”。更准确的说法是：当前设计用 lease、terminal guard、step idempotency 和 actor fencing 降低覆盖风险；生产化会继续把 attempt fencing 做到 workflow step 层。

## Q852. 如果客户端超时重试，idempotency_key 如何避免重复创建？

客户端超时有两种情况：服务端没处理成功，或者服务端已经处理成功但响应丢了。客户端只看见 timeout，分不清是哪一种。

`idempotency_key` 就是为这个场景准备的。客户端第一次提交时带一个稳定 key，例如 `submit-rag-20260615-001`。control plane 收到请求后，会先检查这个 key 是否已经存在：

- 不存在：按正常路径创建 task/workflow/actor/LLM 请求，写 log，再更新 metadata。
- 已存在且 fingerprint 相同：返回已有 task_id、workflow_id 或 actor_id。
- 已存在但 fingerprint 不同：返回 conflict。

这样客户端重试时不会创建第二个 workflow。即使第一次请求已经成功写入 log，但响应在网络里丢了，第二次请求也能拿回同一个 workflow_id。

LogServe 里有两层幂等：

- metadata 层维护 `taskByIdemKey`、`workflowByIdemKey`、`actorByIdemKey`。
- logstore 层在同一个 stream 内用 `idempotency_key` 返回重复 append 的旧 record。

控制面重启后，metadata view 可以从 log 里重建，idempotency fingerprint 也会恢复。这样幂等语义不会只停留在内存 map 上。

## Q853. 如果同一个 idempotency_key payload 不同，为什么应该报 conflict？

因为 idempotency key 表达的是“这两次请求是同一个逻辑操作”。如果 key 相同但 payload 不同，系统无法知道用户到底想要哪一个。

举个例子，第一次提交：

```text
idempotency_key = order-123
payload = charge 100 yuan
```

第二次提交：

```text
idempotency_key = order-123
payload = charge 200 yuan
```

如果系统静默返回第一次结果，用户可能以为第二次请求生效了；如果系统覆盖成第二次，就破坏了第一次请求的确认语义。两个选择都危险。

所以正确做法是报 conflict。LogServe 当前会为请求计算 fingerprint：task 会把 function、source、args、workflow/actor/LLM 字段纳入指纹；workflow 会把 workflow name 和 definition 纳入指纹；actor create 会把 class、init args、snapshot_every 纳入指纹。重复 key 时，已有 fingerprint 和新 fingerprint 不一致，就返回 `idempotency conflict`。

这条规则也方便面试解释：idempotency key 不是“去重字符串”这么简单，它是用户声明的操作身份。相同身份必须对应相同语义。

## Q854. 如果 CompleteTask 被调用两次，metadata 如何保证 terminal 状态不被覆盖？

metadata store 的 `CompleteTask` 先检查当前状态。如果 task 已经是 `SUCCEEDED` 或 `FAILED`，直接返回已有 task，不再写新的 status、result 或 error。这样重复 `CompleteTask` 不会覆盖 terminal 状态。

如果 task 还在 running，会继续检查 worker_id 和 lease epoch。worker 不匹配或者 epoch 不匹配，就返回 stale task lease rejected。这个逻辑防止旧 worker 在任务被重新投递后写入结果。

控制面在调用 metadata 完成任务后，还会处理 workflow、actor、LLM 的上层状态：

- workflow step 如果已经 succeeded，重复 completion 不再调度后续 step。
- LLM stats 会根据 wasTerminal 避免重复 completion 重复计数。
- actor completion 在 metadata 完成之前先做 actor epoch 和 command_seq 检查，避免旧 owner 或乱序 command 改状态。

这一点很重要：terminal guard 保护的是 task 行本身，workflow/actor/LLM 还需要自己的幂等逻辑。因为一个 task completion 往往会触发上层状态变化，只靠 task 表不够。

所以我会把答案拆成两层说：

> metadata 保证 task terminal 状态单调推进；workflow、actor、LLM 再用各自的事件幂等键和 fencing，保证上层状态不被重复 completion 推乱。

## Q855. 如果外部系统不可幂等，你的平台还能保证什么？

平台还能保证内部状态尽量不重复提交，但不能保证外部副作用只发生一次。

比如用户函数调用一个不支持幂等的短信网关。worker 第一次调用已经发出了短信，但在 `CompleteTask` 前崩溃。control plane 看到 lease 过期后重投任务，第二个 worker 又调用短信网关。LogServe 可以保证最终 task result 只提交一次，但短信可能已经发了两条。

这种情况下平台能做的事情有边界：

- 给每次逻辑操作提供稳定 id，例如 task_id、workflow_id、step_id、actor_call_id。
- 在 SDK 文档里要求用户把这些 id 传给外部系统。
- 对 task retry 提供配置，让用户关闭不安全副作用任务的自动 retry。
- 支持 outbox/compensation 模式，让副作用从用户业务库的唯一记录中发出。
- 在日志里记录 attempt、worker、lease epoch，方便审计重复执行。

如果外部系统完全不可幂等，也不支持查询是否已经执行过，那平台只能做到 at-least-once execution + deduplicated internal commit。外部副作用可能重复。这个边界必须写进文档，不能靠“exactly-once-ish”这个词让用户误解。

我会给面试官一个很直接的结论：

> LogServe 能控制自己的日志和状态机，不能控制一个没有幂等协议的第三方系统。遇到扣款、发邮件、发消息这类副作用，要么外部系统支持幂等键，要么用户用 outbox 或唯一约束自己兜住。
