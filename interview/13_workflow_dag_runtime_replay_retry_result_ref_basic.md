# 五、Workflow DAG Runtime、Replay、Retry 与 Result Ref（简单）

这一组问题主要讲 workflow 这条主链路：Python `@workflow` 在 SDK 里被 trace 成 DAG definition，control 把 definition 写入 `wf:<workflow_id>` stream，再根据 step 依赖调度 ready step。step 完成后写 workflow event，metadata view 只是投影；恢复时可以从 workflow stream replay 出同一份状态。

## Q311. Python @workflow 是如何变成 DAG 的？

Python SDK 不是直接执行完整 workflow，而是先 trace 一遍 workflow 函数。

用户写：

```python
@workflow
def simple_rag(query):
    vec = embed(query)
    docs = search(vec)
    ans = generate_mock(query, docs)
    return ans
```

这里 `embed`、`search`、`generate_mock` 是 `@task`。当 workflow 被提交时，SDK 调用 `_build_workflow_definition`，内部用 `trace_workflow` 建一个 `WorkflowTraceContext`。在这个上下文里，`@task` 不真正执行用户函数，而是调用 `ctx.add_step(...)` 记录一个 step，并返回 `StepRef`。

所以 `embed(query)` 会生成一个 step，`search(vec)` 的参数里有 `StepRef("embed")`，SDK 就能推导出 search 依赖 embed。最后 workflow 返回的 `ans` 也是一个 StepRef，SDK 把它作为 `result_step_id`。

最终提交给 control 的不是 Python 调用栈，而是一份 JSON definition：workflow_name、steps、每个 step 的函数源码、参数、depends_on、max_attempts、timeout_ms 和 result_step_id。

## Q312. StepRef 是什么？

`StepRef` 是 SDK trace 阶段返回的占位引用。

当一个 `@task` 在 workflow trace context 里被调用时，它不会马上执行，而是返回 `StepRef(step_id)`。这个对象代表“这个 step 将来执行完成后的结果”。

比如：

```python
vec = embed(query)
docs = search(vec)
```

这里 `vec` 不是 embedding 向量，而是 `StepRef("embed")`。`search(vec)` 的参数里带了这个 StepRef，SDK 就知道 search 依赖 embed，并且真正执行 search 时，要把 embed 的结果填回参数里。

StepRef 的意义是把普通 Python 函数调用写法，转换成 DAG 里的节点和边。

## Q313. depends_on 如何从 task 参数中推导出来？

SDK 在 `WorkflowTraceContext.add_step` 里会编码 task 参数。

它会递归扫描 args 和 kwargs。如果发现参数里有 `StepRef`，就把它编码成：

```json
{"__step_ref__": "step_id"}
```

同时把这个 step_id 加入依赖列表。列表、tuple、dict 都会递归处理，所以 StepRef 可以嵌在复杂参数里。

最后 SDK 对依赖去重并排序，写入 step definition 的 `depends_on`。control 侧只需要看这个字段，就能判断一个 step 要等哪些上游 step 成功。

## Q314. workflow step 有哪些状态？

workflow step 的状态定义在 proto 里，主要有四个实际使用的状态：

- `SCHEDULED`
- `STARTED`
- `SUCCEEDED`
- `FAILED`

新建 workflow 时，所有 step 初始都是 `SCHEDULED`。这里的 scheduled 不是“已经投递给 worker”，而是“这个 step 在 DAG 里存在，等待依赖满足后调度”。

当 control 为 ready step 创建 task 并写 `StepScheduled` 后，step 会记录 task_id、attempt 和 input_hash。worker 真正开始执行后，control 写 `StepStarted`，step 进入 started。

worker 完成后，如果 task 成功，control 写 `StepSucceeded`；如果失败，写 `StepFailed`。如果失败但还可以 retry，metadata 会把 step 状态重新放回 scheduled，并清空 task_id，等待下一次调度。

## Q315. ResultStepID 表示什么？

`ResultStepID` 表示 workflow 最终返回哪个 step 的结果。

Python workflow 必须 return 一个 `StepRef`。SDK 会把这个 StepRef 的 step_id 写到 definition 的 `result_step_id`。比如 `return ans`，而 ans 是 `generate_mock` 返回的 StepRef，那么 result_step_id 就是 `generate_mock`。

control 判断 workflow 所有 step 都成功后，会取 `ResultStepID` 对应 step 的结果，作为整个 workflow 的 final result。

如果提交的 definition 没有 result_step_id，control 会默认使用最后一个 step。这是兜底逻辑，但正常 Python SDK 会显式生成 result_step_id。

## Q316. ready step 是如何判断的？

control 在 `scheduleReadySteps` 里判断 ready step。

一个 step 要被调度，必须满足几个条件。

第一，workflow 本身还在 running。已经 completed 或 failed 的 workflow 不再调度新 step。

第二，step 状态是 `SCHEDULED`，并且当前没有 task_id。已经有 task_id 说明它已经被调度过，不能重复创建 task。

第三，所有 depends_on 指向的上游 step 都是 `SUCCEEDED`。这个判断由 `workflow.DependenciesSucceeded` 完成。

满足这些条件后，control 会解析 step 参数，把上游 StepRef 替换成真实结果，计算 input_hash，然后创建 task 并写 `StepScheduled`。

## Q317. workflow step 的 max_attempts 从哪里来？

max_attempts 主要来自 Python SDK 的 decorator 参数。

对普通 task，用户可以写：

```python
@task(retries=3, timeout_ms=30000)
def embed(query):
    ...
```

SDK trace 这个 task 时，会把 `_logserve_retries` 写入 step definition 的 `max_attempts`。如果用户没配置，默认是 3。

workflow 本身也有默认 retries。`@workflow(retries=3, timeout_ms=30000)` 会写到 definition 顶层的 max_attempts 和 timeout_ms。Go 侧 `ParseDefinition` 会做兜底：step 没有 max_attempts 时，用 workflow 默认值；workflow 默认值也没有时，用 3。

执行时，step 失败后 control 调用 `StepMaxAttempts` 判断还能不能 retry。

## Q318. timeout_ms 在 workflow 中如何生效？

timeout_ms 最后会进入 `TaskSpec.TimeoutMs`。

SDK 在 trace step 时，把 task decorator 上的 timeout_ms 写进 step definition。control 调度 ready step 时，从 `stepDef.TimeoutMs` 复制到 TaskSpec。

worker 执行 task 时会根据 `TaskSpec.timeout_ms` 创建带超时的 context。超时后，worker 把 task 标记为 failed，写 `TaskFailed`，再调用 CompleteTask。

control 收到 failed 后，把对应 workflow step 标记为 failed。如果 attempts 还没达到 max_attempts，就把 step 重新变成 scheduled，等待下一次 retry。否则 workflow 失败。

所以 timeout 的触发点在 worker，retry 的决策点在 control。

## Q319. workflow_id + step_id + input_hash 用来解决什么问题？

这组字段用来给 step 的输入结果去重。

同一个 workflow、同一个 step，如果输入完全一样，那么 `ResolveArgs` 解析出来的 args_json 也一样，input_hash 就一样。control 在调度时会用它生成 task idempotency key，在 step succeeded 时也用它生成成功事件的幂等键：

```text
workflow_id + step_id + input_hash + succeeded
```

它解决的是重复投递、worker retry 或 CompleteTask 重试带来的重复完成问题。

如果同一个 step 同一份输入已经成功写过最终结果，后来的重复 completion 不应该再写第二份 workflow final result，也不应该把 step 结果覆盖成另一份。

## Q320. 为什么 workflow final result 要去重？

final result 是 workflow 对外的最终答案。它一旦确定，就应该保持稳定。

在 at-least-once 执行模型下，worker 可能重试，RPC 可能超时，CompleteTask 可能被重复调用。没有去重的话，同一个 step 的重复完成可能触发两次 `WorkflowCompleted`，甚至写出两个不同的 result_ref。

LogServe 用 step 级 input_hash 去重，再用 `workflow_id:completed` 作为 workflow completed 事件的幂等键，保证重复完成不会生成第二个最终结果。

这就是项目里说的 exactly-once-ish：不保证用户函数只执行一次，但保证 workflow 最终状态提交尽量只接受一次有效结果。

## Q321. 大结果为什么要写 result store？

shared log 应该保存状态变化，不应该变成大对象存储。

如果把大结果直接写进 workflow event，日志会快速膨胀。replay 要读更多数据，segment rolling、复制、备份都会变慢。一个大结果还可能拖慢其他小事件的 append。

LogServe 的做法是设置 inline threshold。小结果直接放在 event 的 `result_json` 里；超过阈值时，control 调用 result store，把数据写到本地对象存储或 S3-compatible MinIO 边界，workflow event 里只放 `result_ref`。

这样 replay 仍然能知道结果在哪里，日志也保持轻量。

## Q322. result_ref 的作用是什么？

`result_ref` 是大结果在 result store 里的引用。

当 step result 或 workflow final result 超过 inline threshold 时，control 把结果写到 result store，返回一个 ref。workflow 的 `StepSucceeded` 或 `WorkflowCompleted` event 里保存这个 ref，metadata view 也保存同一个 ref。

后续如果下游 step 需要这个结果，`ResolveArgs` 看到上游 step 没有 inline `ResultJSON`，但有 `ResultRef`，就通过 `LoadResult` 把对象读出来，再填入下游 step 的参数。

所以 result_ref 是日志和对象存储之间的桥。日志解释状态，result store 保存大数据。

## Q323. ReplayWorkflow 做了什么？

`ReplayWorkflow` 会从 `wf:<workflow_id>` stream 读取 workflow 事件，从 seq 1 开始读，当前限制是最多 10000 条。

然后它调用 `workflow.Replay`，按事件顺序重建 workflow state。它会处理 `WorkflowStarted`、`StepScheduled`、`StepStarted`、`StepSucceeded`、`StepFailed`、`WorkflowCompleted`、`WorkflowFailed`。

重建完成后，control 再读取当前 metadata 中的 workflow state，用 `workflow.Consistent` 做对比。

最后 RPC 返回两部分：replayed workflow 状态，以及 `consistent_with_metadata`。

## Q324. consistent_with_metadata 表示什么？

`consistent_with_metadata` 表示 replay 出来的 workflow 状态，是否和当前 metadata view 里的状态一致。

它不是说 workflow 一定正确，也不是说日志没有任何问题。它只表示：按当前 replay 逻辑，从 workflow stream 重建出的状态，和 control metadata 里保存的状态是否匹配。

如果是 true，说明 materialized view 至少和 shared log 对齐。如果是 false，通常表示 metadata 落后、更新失败，或者某条状态更新没有按照 log-first 路径落到 view。

这个字段很适合做实验验收：证明 workflow 状态不是只存在数据库里，而是可以从 log 独立恢复出来。

## Q325. workflow 失败时如何记录错误？

workflow 失败通常来自某个 step 失败并且 retry 次数用完。

worker 执行失败后写 `TaskFailed`，control 的 CompleteTask 会进入 `completeWorkflowStep`。它先写 `StepFailed` 到 workflow stream，payload 里包含 workflow_id、step_id、task_id、error、timestamp 和 latency。

如果这个 step 的 attempts 已经达到 max_attempts，control 会调用 `failWorkflow`，写 `WorkflowFailed` 事件。这个事件里保存 workflow_id、error 和 timestamp。

metadata view 也会更新 workflow status 为 failed，Error 字段保存失败原因。

## Q326. workflow step 失败后如何 retry？

step 失败后，control 先记录失败事实：写 `StepFailed`，并把 step 状态更新为 failed。

然后它看当前 attempts 是否小于 max_attempts。如果还能 retry，就把这个 step 在 metadata 中重新改回 `SCHEDULED`，清空 task_id，保留 attempts。

CompleteTask 返回后，control 会再次调用 `scheduleReadySteps`。这个 step 的依赖仍然满足，而且 task_id 已经清空，所以会被重新调度。新的调度会生成新的 task_id，attempt 也会加 1。

如果 attempts 已经达到上限，就不再重试，workflow 进入 failed。

## Q327. worker 重启后 workflow 如何从已完成 step 继续？

关键是已完成 step 的结果已经写进 workflow stream。

control 重启时会 `bootstrapWorkflows`，扫描 `wf:` streams，对每个 workflow 调用 `workflow.Replay`。Replay 能恢复每个 step 的状态、attempt、task_id、result_json 或 result_ref。

已经 `SUCCEEDED` 的 step 不会重新调度。只有 running workflow 里还处于 scheduled/started 且需要继续执行的 step，会通过 `restoreWorkflowTasks` 恢复 TaskSpec，重新放回 queue。

这样 embed 已经完成后，worker 崩溃再重启，workflow 不会从 embed 开始重跑，而是从 search 或后续还没成功的 step 继续。

## Q328. duplicate step completion 为什么不应该生成第二个 final result？

因为重复 completion 通常来自重试、redelivery 或 RPC 超时，不代表用户希望产生新的 workflow 输出。

如果同一个 step 的同一份输入已经成功，第二次 completion 只是同一事实的重复提交。生成第二个 final result 会带来几个问题：result_ref 可能不同，metadata 可能被覆盖，下游审计也会看到一个 workflow 完成了两次。

LogServe 在 `completeWorkflowStep` 里先检查 step 是否已经 succeeded。如果已经 succeeded，就直接返回，不再重复推进 workflow。

同时 StepSucceeded 和 WorkflowCompleted 的 idempotency key 也固定，防止日志层重复写最终成功事件。

## Q329. 为什么不直接把整个 workflow 状态存在数据库？

可以存在数据库，但不能只存在数据库。

数据库里的 workflow state 是 materialized view，适合查询和 dashboard 展示。问题是它不是最可靠的恢复来源。如果 control 在写完 log 后更新数据库失败，数据库会落后。如果只信数据库，就会丢掉已经 append 成功的状态变化。

shared log 记录的是事件历史。它能解释 workflow 为什么走到当前状态，也能在重启时重新构建 metadata view。

所以 LogServe 的选择是：workflow 事件写 shared log，数据库或 memory metadata 保存当前 view。读状态时查 view，验证和恢复时查 log。

这比只存一行 workflow JSON 更适合做故障恢复和 replay 验证。

## Q330. 这个 workflow runtime 与简单串行执行脚本有什么区别？

简单脚本是进程内顺序执行。进程挂了，脚本状态通常也没了，除非用户自己写 checkpoint。它也不天然支持 step 级 retry、去重和 replay。

LogServe 的 workflow runtime 把每个 task 调用变成 DAG step。control 按依赖调度 ready step，worker 执行具体 task，结果通过 workflow stream 和 result store 保存。

它能做到几件脚本不好做的事：失败后从已完成 step 继续；失败 step 按 max_attempts retry；大结果用 result_ref；从 shared log replay 出 workflow 状态；重复 step completion 不生成第二个最终结果。

所以它不是“把三个函数按顺序调用一遍”，而是把函数调用拆成可恢复、可调度、可观测的 step 状态机。
