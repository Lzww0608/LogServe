# 十一、分布式系统原理与生产化追问：API、兼容性与产品化

这一组问题更像产品化面试。回答时不要只说“加接口就行”。LogServe 现在已经有 proto、Python SDK、CLI、dashboard snapshot、workflow/actor/LLM 三条主线；后续要考虑的是：哪些字段会变、老客户端还能不能用、用户怎么查进度、失败任务怎么处理、取消和重跑怎么保持日志语义。

## Q816. proto 中哪些字段未来最可能需要兼容演进？

最容易演进的是 `TaskSpec`。现在它已经承载了普通 task、workflow step、actor call 和 LLM request：`function_source`、`args_json`、`workflow_id`、`step_id`、`actor_id`、`actor_state_json`、`llm_model_name`、`timeout_ms`、`task_lease_epoch` 都在里面。这个结构很方便，但越往产品化走，越会变大。

我觉得未来最可能变的字段有几类。

第一类是代码交付字段。现在是 `function_source` 和 `actor_class_source`。生产里更可能改成 `artifact_ref`、`artifact_digest`、`runtime_env`、`image`、`entrypoint`，避免每次传源码。

第二类是调度字段。现在有 `target_worker_id`、LLM model 信息和 `timeout_ms`。以后可能要加 `tenant_id`、`project_id`、`priority`、`resource_requirements`、`gpu_requirements`、`affinity`、`queue_name`。

第三类是 retry 和取消。现在 workflow definition 里有简单的 retries/timeout 思路，但 proto 层没有完整 `RetryPolicy`。以后会需要 `max_attempts`、`backoff_initial_ms`、`backoff_multiplier`、`max_elapsed_ms`、`retryable_error_codes`。

第四类是结果和数据安全。`result_json`、`result_ref`、`args_json`、prompt 相关字段未来可能要加 `content_type`、`schema_ref`、`encryption_key_id`、`redaction_policy`。

第五类是 dashboard 和查询接口。`DashboardSnapshot` 现在一次性返回 tasks、workflows、actors、workers、models，适合 demo。生产里会加分页、过滤、排序、时间范围和权限上下文。

proto 演进要守住几条规则：字段号不能复用；删除字段时要 `reserved`；新增字段给默认行为；老客户端看不懂新字段也不能崩；枚举必须能处理 unknown。这个比“字段设计得全不全”更重要。

## Q817. 如何处理新增 enum value 的兼容性？

新增 enum value 时，最大风险是老客户端不知道这个值。比如 `TaskStatus` 现在只有 `QUEUED/RUNNING/SUCCEEDED/FAILED`，如果以后加 `CANCELLED`、`TIMED_OUT`、`DEAD_LETTERED`，老 SDK 的 `_task_status_name()` 可能会返回 `UNSPECIFIED`，用户代码就不知道怎么处理。

处理方法有几条。

第一，所有 enum 都保留 `UNSPECIFIED = 0`。这已经做了。客户端遇到未知值时，不要假装成某个已知状态，应该显示为 `UNKNOWN(value)` 或 `UNSPECIFIED`，并保留原始数值。

第二，状态类 enum 要分清 terminal 和 non-terminal。老客户端即使不认识 `CANCELLED`，也需要知道它是不是 terminal。可以在响应里增加 `bool terminal` 或 `status_category`，让新旧客户端都能做基本判断。

第三，SDK 的状态解析不要用封闭字典直接丢信息。现在 Python gRPC client 用 dict 映射，不在表里的返回 `UNSPECIFIED`。未来可以改成：

```python
return known.get(status, f"UNKNOWN_{status}")
```

第四，CLI 输出 JSON 时也保留原始 enum name 和 numeric value。这样自动化脚本可以在升级前先兼容未知状态。

服务端也要谨慎。新增 enum value 后，不要立刻让所有路径都返回新值。可以用 API feature flag 或 capabilities，让老客户端还看到旧语义，新客户端 opt-in 新状态。

## Q818. TaskSpec 过大时 gRPC message size 如何处理？

`TaskSpec` 过大主要来自 `function_source`、`args_json`、`actor_state_json` 和大 prompt。gRPC 默认有 message size 限制，超过后客户端会收到资源耗尽或传输错误。即使调大限制，也不应该把大对象都塞进 RPC。

我会把 TaskSpec 大小控制在“描述任务”这个级别。源码、依赖包、大参数、大输入文件、大结果都改成引用。

具体做法：

- `function_source` 改成 `artifact_ref + digest`，源码或包放对象存储。
- `args_json` 超过阈值时写 object store，TaskSpec 里放 `args_ref`。
- actor state 不应该每次都塞进 TaskSpec，大 state 用 snapshot ref 或增量状态。
- LLM prompt 如果敏感或很大，放 prompt object ref，log 里只放 hash 和长度。
- result 已经有 result_ref 方向，继续沿用。

gRPC 层仍然要设置最大 message size。这个限制是保护，不是容量规划。服务端收到超大请求要返回明确错误，比如 `RESOURCE_EXHAUSTED: task spec too large, use artifact_ref`。SDK 也可以在发送前预检查，给用户更早的错误。

如果确实要上传大文件，应该单独做 upload API 或 presigned URL，而不是让 `SubmitTask` 承担文件传输。

## Q819. SDK 同步 API 和异步 API 应如何设计？

当前 Python SDK 的 `submit()`、`submit_workflow()`、`submit_llm()` 都偏同步：提交后会轮询状态，直到成功或失败再返回结果。这个用来 demo 很方便，但生产里会卡住用户进程。长 workflow、LLM 请求、actor call 都不适合默认阻塞。

我会把 SDK 分成两层。

同步 API 保留，适合短任务：

```python
result = logserve.submit(fn, 1, 2)
```

异步 API 返回 handle：

```python
run = client.submit_async(fn, 1, 2)
print(run.task_id)
status = run.status()
result = run.result(timeout=60)
```

workflow 也一样：

```python
wf = client.submit_workflow_async(simple_rag, "query")
print(wf.workflow_id)
for step in wf.steps():
    print(step.step_id, step.status)
```

`asyncio` 用户还需要真正的 async API：

```python
wf = await client.submit_workflow_asyncio(simple_rag, "query")
status = await wf.status()
```

底层语义要统一：submit 只表示 control 接受了请求并返回 id；wait/result 才是等待执行完成。这样用户能自由选择阻塞还是非阻塞，也方便服务端保持 API 简单。

## Q820. 如何让用户查询 workflow 进度而不阻塞 submit？

`SubmitWorkflow` 应该快速返回 `workflow_id` 和初始状态，例如 `RUNNING`。用户后续用 `GetWorkflowStatus(workflow_id)` 查询进度。当前 control 的 proto 已经有 `SubmitWorkflow` 和 `GetWorkflowStatus`，只是 Python SDK 的高级封装会同步等待完成。

产品化时，SDK 可以提供两种方法：

```python
workflow_id = client.start_workflow(simple_rag, query)
status = client.get_workflow_status(workflow_id)
```

查询结果里要包含 step 级状态。现在 `GetWorkflowStatusResponse` 已经返回 `steps`，每个 step 有 `step_id`、`task_id`、`status`、`attempts`、`error`、`latency_ms`。这个结构可以直接用于进度条、DAG 图和排错。

还要支持增量查询。大 workflow 不适合每次返回所有 step。可以加：

- `page_size/page_token`
- `updated_after_ms`
- `step_status_filter`
- `include_results=false`

submit 不阻塞后，客户端需要处理 unknown outcome。`SubmitWorkflow` 超时不代表失败。SDK 要用 idempotency key 重试，或提供 `get_workflow_by_idempotency_key()` 查询。

## Q821. 是否需要 callback/webhook？

需要，但不能替代状态查询。

callback/webhook 适合用户不想一直 poll 的场景。workflow 完成、step 失败、actor command 超时、LLM 请求完成，都可以触发 webhook。用户系统收到通知后再调用 LogServe API 拉取最终状态和结果。

设计时要注意几件事。

第一，webhook 也要事件化。用户注册 webhook、更新 URL、禁用 webhook，都应该写配置事件，方便审计。

第二，webhook 投递至少一次。网络失败时要 retry，带指数退避和最大重试次数。用户接收端也必须幂等，event_id 或 delivery_id 要固定。

第三，payload 不要塞敏感结果。通知里放 workflow_id、task_id、event_type、status、timestamp、signature。结果让用户带权限来拉。

第四，要签名。比如用 tenant webhook secret 对 payload 做 HMAC，用户验证签名和时间戳。

第五，失败投递要进 webhook DLQ，dashboard 里能看到最后错误。

所以 webhook 是用户体验功能，也是集成能力。它不能承担 source of truth，真正状态仍然以 shared log 和 metadata view 为准。

## Q822. 如何设计 CLI 与 SDK 的一致错误模型？

现在 CLI 和 SDK 都会把错误转成字符串。Python SDK 很多地方直接 `raise RuntimeError(output.get("error"))`。这对 demo 足够，对产品不够。用户需要区分：参数错、幂等冲突、资源不足、认证失败、任务失败、系统暂时不可用。

我会定义统一错误模型：

```json
{
  "code": "IDEMPOTENCY_CONFLICT",
  "message": "idempotency key reused with different payload",
  "retryable": false,
  "resource": "workflow",
  "request_id": "...",
  "details": {}
}
```

gRPC 侧用标准 status code 加 error details。比如：

- `INVALID_ARGUMENT`：参数不合法。
- `ALREADY_EXISTS` 或 `ABORTED`：幂等冲突。
- `RESOURCE_EXHAUSTED`：queue/backpressure/quota/message too large。
- `UNAVAILABLE`：logd/control 临时不可用。
- `DEADLINE_EXCEEDED`：等待超时。
- `PERMISSION_DENIED`：权限不足。

CLI 输出 JSON 时也使用同样结构。非 0 exit code 对应错误，stdout/stderr 规则固定：机器可读 JSON 走 stdout，诊断文本走 stderr，最好不要混。

SDK 再把这些 code 映射成异常类，例如 `IdempotencyConflict`、`QuotaExceeded`、`WorkflowFailed`、`TransportUnavailable`。这样用户才能写可靠的重试逻辑。

## Q823. 如何设计 dashboard 权限和分页？

当前 `DashboardSnapshot` 一次返回 queue、tasks、workflows、actors、workers、models，适合实验报告。生产 dashboard 要做权限和分页，否则数据量和安全都会出问题。

权限上，dashboard 不能直接展示全局数据。普通用户只能看自己 tenant/project 的 workflow、task、actor、result 摘要。operator 可以看 worker、queue、backpressure、log append latency。admin 才能看跨 tenant 聚合和系统配置。

分页上，tasks、workflows、actors 都不能一次全量返回。接口可以拆成：

```text
ListTasks(filter, page_size, page_token)
ListWorkflows(filter, page_size, page_token)
ListActors(filter, page_size, page_token)
GetDashboardSummary()
```

summary 返回聚合数字：queue depth、running tasks、failed workflows、cache hit rate、p95 latency。列表接口再按需分页。

过滤也很重要：按 status、workflow_id、actor_id、model_name、worker_id、created_after、updated_after。没有过滤的大列表会拖垮 metadata store。

结果和 payload 默认不展示。dashboard 可以显示 result_ref 和大小，用户点击查看时再单独鉴权和审计。replay log 更敏感，应该作为单独权限，不和普通 dashboard read 混在一起。

## Q824. 如何设计 project/namespace 概念？

我会把层级设计成：

```text
tenant -> project -> namespace -> resource
```

tenant 对应组织或账户，是计费、RBAC、密钥、总 quota 的边界。project 对应一个应用或业务线，方便隔离模型、workflow、actor 和实验。namespace 更像运行环境，比如 `dev`、`staging`、`prod`，也可以对应团队内部子空间。

资源命名都带 namespace：

```text
tenants/<tenant>/projects/<project>/namespaces/<ns>/workflows/<workflow_id>
```

stream_id 也可以编码这些信息，或在 payload metadata 中保存。更推荐两者都有：stream_id 用于路由和快速过滤，payload metadata 用于审计和 replay。

project/namespace 要影响几类行为：RBAC、quota、object store prefix、model registry 可见性、dashboard 过滤、webhook 配置、secret scope。比如同一个 tenant 下，`prod` namespace 的 secret 不应该被 `dev` workflow 读取。

对用户来说，namespace 还解决命名冲突。两个项目都可以有 `simple_rag` workflow，只要 namespace 不同就不会混。

## Q825. 如何为 workflow/actor/model 命名和版本管理？

命名要分“显示名”和“稳定标识”。用户可以把 workflow 叫 `simple_rag`，但系统内部还是用 `workflow_id`。版本管理则要保证老实例能继续跑。

workflow 版本可以用 definition fingerprint。用户每次提交 workflow definition，control 计算 fingerprint。如果 name 相同但 definition 不同，就形成新 revision。运行中的 workflow 绑定提交时的 definition，不受后续修改影响。

actor 版本更敏感。actor 有长期状态，class source 变更后不能随便用新代码解释旧 state。需要 `actor_class_name + actor_version`，并提供 migration hook。没有 migration 的情况下，旧 actor 继续用旧版本代码，新 actor 才用新版本。

model 版本现在已经有 `name + version`，默认 `v1`。生产里 version 应该指向不可变 artifact 或 model digest。比如 `model-A:v1` 背后对应固定 checkpoint digest。模型灰度时可以注册 `v2`，调度器按 policy 分流。

命名规则也要限制。不要允许任意路径字符进入 name，避免影响 object store key 和 stream_id。常见规则是小写字母、数字、短横线、下划线，长度有限制。

## Q826. 如何支持 retry policy 的更复杂配置，比如指数退避、最大总时长？

当前 workflow retry 更接近简单 `max_attempts`。生产里需要把 retry policy 显式建模。

可以在 proto 或 workflow definition 中增加：

```json
{
  "max_attempts": 5,
  "initial_backoff_ms": 500,
  "max_backoff_ms": 30000,
  "backoff_multiplier": 2.0,
  "jitter": true,
  "max_elapsed_ms": 300000,
  "retryable_error_codes": ["TIMEOUT", "UNAVAILABLE"]
}
```

调度时，step 失败后不要马上重新入队，而是写 `StepRetryScheduled`，包含 next_attempt、next_run_at_ms、reason。control 的 scheduler 到时间后再提交下一次 task。

最大总时长要和单次 timeout 分开。单次 timeout 是某个 attempt 最长跑多久；max_elapsed 是从 step 第一次 schedule 到最后放弃的总时长。超过后 workflow step 进入 failed。

错误分类也要明确。用户代码抛 `ValueError` 这类业务错误可能不该 retry；worker lost、log append timeout、vLLM 503 更适合 retry。平台可以提供默认策略，用户也可以按 task/step 覆盖。

所有 retry 决策都要写入 workflow stream。否则 replay 时看不到为什么重试，也无法解释 step attempts。

## Q827. 如何支持 dead letter queue？

DLQ 用来保存最终无法处理但还需要人工排查的任务或事件。

Task 维度上，任务超过 max_attempts、反复 timeout、payload 无法解析、目标 actor 已删除、模型不存在，都可以进入 DLQ。事件可以写成：

```text
TaskDeadLettered(task_id, reason, final_error, attempts)
```

DLQ 不一定是单独消息队列，也可以是一个 log stream，比如 `dlq:<tenant_id>`，metadata view 再 materialize 出 DLQ 列表。这样 DLQ 本身也能 replay 和审计。

用户需要几个操作：查看 DLQ、查看原始错误、重试、丢弃、导出。重试时不能直接复用旧 attempt，要创建新的 attempt 或新的 task，并保留和原 DLQ item 的关联。

workflow step 进入 DLQ 后，workflow 可以进入 `FAILED` 或 `WAITING_FOR_MANUAL_ACTION`。如果支持人工修复后继续，就要给 workflow 增加这种暂停状态。

DLQ 也需要 quota。恶意用户不能通过制造失败任务把 DLQ 写爆。

## Q828. 如何支持手动重跑某个 failed step？

手动重跑不能简单把数据库里的 step 状态改回 scheduled。LogServe 的主线是 log-first，所以重跑也要写事件。

我会提供 `RerunWorkflowStep(workflow_id, step_id, mode)`。mode 至少有两种：

- `same_input`：使用上一次 resolved input。
- `latest_upstream`：重新读取当前上游 step 结果。

调用后写：

```text
StepRerunRequested(workflow_id, step_id, requested_by, mode)
StepScheduled(... attempt = previous + 1)
```

重跑前要检查依赖。只有上游都 succeeded 的 step 才能重跑。已经 completed 的 workflow 是否允许重跑，要看产品语义。如果允许，就会产生新的 workflow revision of execution，不能悄悄覆盖旧最终结果。

结果去重也要处理。原来的 `StepSucceeded` idempotency key 可能按 `workflow_id + step_id + input_hash` 去重。手动重跑如果允许覆盖，就要引入 rerun_id 或 attempt generation，否则会被当作 duplicate。

面试里我会强调：手动重跑是新事件，不是修改历史。

## Q829. 如何支持 task cancellation 和 workflow cancellation？

取消要分状态层和执行层。

task cancellation 先加 RPC：

```text
CancelTask(task_id, reason)
```

control 写 `TaskCancellationRequested`。如果 task 还在 queue 里，可以直接标记 `CANCELLED`，并从队列移除。如果 task 已经 running，control 需要通知 worker。worker 收到 cancel 后给 executor 发取消信号；Python 任务如果不配合，就只能等 timeout 或杀掉 runner。

workflow cancellation 要递归处理。control 写 `WorkflowCancellationRequested`，不再调度新的 ready step；queued step task 直接取消；running step 发送 cancel；已经 succeeded 的 step 保留结果。所有 step 终止后写 `WorkflowCancelled`。

actor command cancellation 更谨慎。如果 command 已经进入 mailbox 但还没执行，可以取消。正在执行的 actor method 如果取消失败，旧 completion 仍可能回来。要靠 command_seq 和状态机保证不会破坏顺序。

proto 要新增状态：`TASK_STATUS_CANCELLED`、`WORKFLOW_STATUS_CANCELLED`，也可能有 `CANCELLING`。老客户端要能处理 unknown enum。

取消不是删除。日志中要保留取消请求、执行结果和原因。

## Q830. 如何支持 actor 删除和状态导出？

actor 删除不能直接删 metadata。需要事件化。

可以提供 `DeleteActor(actor_id, mode)`。mode 可以是 graceful 和 force。graceful 删除先停止接受新 command，等 mailbox 清空，写 `ActorDeleted`。force 删除立即拒绝后续 command，running command 的 completion 即使回来，也会因为 actor status/epoch 不匹配被拒绝。

状态导出可以提供 `ExportActorState(actor_id)`。如果 state 小，可以返回 JSON；如果 state 大，写 object store，返回 `export_ref`。导出本身也应写事件：

```text
ActorStateExported(actor_id, export_ref, command_count)
```

导出要绑定 `command_count`。否则用户不知道这份状态对应哪一个 mailbox 位置。恢复或迁移时，可以用 `command_count` 判断是否还需要 replay tail log。

删除后数据保留策略要明确。业务上删除 actor，不一定立刻删除 log 和 snapshot。多租户和合规场景可能要求 retention，到期后再 physical compaction 和对象清理。

## Q831. 如何支持模型预加载和下线？

模型预加载是为了减少 LLM cold start。可以提供 `PreloadModel(model_name, version, worker_selector)`。control 根据 worker labels、GPU、cache capacity 选择 worker，提交内部 preload task。worker 拉取 checkpoint，加载或至少写入本地 checkpoint cache，然后上报 cached_models。

事件上可以记录：

```text
ModelPreloadRequested
ModelLoadStarted
ModelLoaded
ModelPreloadFailed
```

下线模型要分两步。先从 registry 标记 deprecated，不再接新请求；已经 running 的请求继续完成。等没有 inflight 请求后，再通知 worker evict cache，写 `ModelUnloaded` 或 `ModelEvicted`。

灰度发布时，不要直接覆盖 `model-A:v1`。注册 `model-A:v2`，通过 routing policy 或 workflow config 决定流量比例。回滚就是把流量切回旧版本。

下线还要考虑 workflow/actor 的历史依赖。旧 workflow replay 可能需要知道当时用的是哪个模型版本。日志里保留 model_name/version 和 adapter 信息，这一点已经有基础。

## Q832. 如何支持成本统计和计费？

计费要基于事件，而不是只看最终状态。LogServe 已经记录 task、workflow、actor、LLM 事件，可以在这些事件上 materialize usage。

可统计的指标包括：

- task 执行时长、CPU worker 占用时间。
- workflow step 数、attempt 数、总 latency。
- actor command 数、snapshot 存储量、mailbox backlog。
- LLM prompt tokens、completion tokens、model load 时间、GPU 占用时间、cache storage。
- object store 读写字节和保存时长。
- log append 量和 retention 占用。

LLM 当前 mock/vLLM adapter 还没有完整 token usage 字段。生产里需要在 `LLMCompleted` 里加入 prompt_tokens、completion_tokens、total_tokens、model_price_key。

计费聚合按 tenant/project/namespace 做。事件先进入 usage stream，再异步汇总到 billing view。为了可审计，账单上的每一项最好能追溯到 event id 或时间窗口。

还要区分成本统计和收费。内部成本可以更细；对用户收费要稳定、可解释，不要每次策略调整都让账单口径变化。

## Q833. 如何向用户解释 exactly-once-ish，不让用户误用？

我会直接说：LogServe 不保证用户代码只执行一次。它保证的是平台内部的结果提交和状态推进尽量去重，并用 lease epoch、command_seq、idempotency key 拒绝旧写入。

用户需要知道三条边界。

第一，worker 执行是 at-least-once。worker crash、网络超时、redelivery 都可能让同一个 task attempt 被重新执行。

第二，平台会对提交层去重。比如相同 idempotency key 的 workflow 不会重复创建；同一个 workflow step 的最终结果不会重复写；旧 worker 的 stale completion 会被拒绝。

第三，外部副作用要用户自己做幂等。如果 task 里扣款、发邮件、调用第三方 API，平台无法自动撤销已经发生的外部行为。用户应该给下游 API 带业务幂等键，或者把外部副作用放进 outbox/compensation 流程。

文档里不要写“exactly once”。可以写：

```text
Execution may happen more than once.
Result commit is deduplicated when idempotency keys and platform state checks apply.
External side effects must be idempotent at the application layer.
```

这个说法不华丽，但能避免用户误用。

## Q834. 如果要写一篇技术博客，你会如何安排结构？

我会按问题驱动来写，不按功能清单堆模块。

开头先讲背景：AI workflow 里有普通 task，也有有状态 actor、LLM serving、模型缓存、失败恢复。普通队列能跑任务，但很难解释“失败后从哪里恢复”“actor 状态怎么算”“LLM cold start 怎么调度”。

第二部分讲核心设计：shared log 是 source of truth，metadata view 是投影。用一张图解释 SDK、control、logd、worker 的请求路径。

第三部分讲 workflow：`@workflow` 如何变成 DAG，step 状态机、retry、result_ref、replay 如何工作。用 simple RAG 例子。

第四部分讲 actor：mailbox、command_seq、snapshot、epoch fencing。用 Counter actor 说明 worker 挂掉后为什么还能恢复到 100。

第五部分讲 LLM：model registry、checkpoint cache、locality-aware 和 predicted-latency。放实验结果，比如 cache hit rate 和 p95 latency 对比。

第六部分讲工程边界：exactly-once-ish、单机实验环境、mock LLM 与真实 GPU 的差距、logd 单点、多租户和安全还需要补什么。

结尾不要夸项目“生产级”。更好的收束是：这个项目展示了一个 AI runtime 的主链路，下一步是多副本 logd、强隔离 executor 和更真实的 GPU benchmark。

## Q835. 如果要做 demo，最能展示技术深度的 3 分钟流程是什么？

我会做一个三段式 demo，每段都展示一个恢复或调度点。

第一分钟演示 workflow。用 Python SDK 提交：

```python
@workflow
def simple_rag(query: str):
    vec = embed(query)
    docs = search(vec)
    ans = llm_generate("model-A", query)
    return ans
```

dashboard 上显示 DAG step 状态。然后让 `embed` 完成后 kill worker，重启 worker，展示 workflow 没有重跑 `embed`，从后续 step 继续。这一段证明 replay 和 step 去重。

第二分钟演示 actor。创建 Counter，连续提交 100 次 `inc()`，kill owner worker，让另一个 worker 接管。调用 `get()` 返回 100。顺手展示 actor stream 里的 `ActorCommandSubmitted`、`ActorCommandApplied`、snapshot 和 epoch。这里能说明 mailbox、snapshot、ownership fencing。

第三分钟演示 LLM locality。启动 3 个 worker：worker-1 缓存 model-A，worker-2 缓存 model-B，worker-3 空缓存。连续提交 model-A 请求，先用 resource-only，再切 locality-aware 或 predicted-latency。展示 cache hit rate、cold start latency、p95 latency 的对比。最后打开 checkpoint cache probe，证明 warm request 命中本地 checkpoint。

如果时间只剩十秒，就总结一句：这个 demo 跑的是任务，重点展示的是 log-first recovery、有状态 actor 恢复和模型缓存感知调度三条主线。
