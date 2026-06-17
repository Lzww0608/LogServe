# 13. DAG、workflow engine、拓扑排序与调度语义

这一组问题讨论 workflow engine 里最容易被问到的一层：怎么把一个多步骤任务表达成 DAG，怎么判断哪些 step 可以运行，怎么处理失败、重试、取消、超时和中间结果。

面试里不要只说“DAG 就是有向无环图”。这个定义当然对，但工程上更重要的是：DAG 是 workflow engine 的调度契约。节点代表 step / task，边代表依赖；调度器不能只看列表顺序，而要根据依赖状态、输入可用性、资源容量、重试策略和取消状态来判断一个节点是否 ready。

结合 LogServe 当前实现，可以先抓住这几条边界：

```text
workflow definition:
  steps[] 定义节点，depends_on 定义调度依赖。
ready rule:
  step 还没有绑定 task，并且所有 depends_on step 都已经 SUCCEEDED。
input resolution:
  args_json 里的 {"__step_ref__": "step_id"} 会在调度前替换成上游结果。
result materialization:
  小结果内联，大结果通过 result_ref 放到外部 result store。
retry:
  当前 step 失败后先重试当前 step，超过 max_attempts 后 workflow 失败。
timeout:
  timeout_ms 传到 task，worker 用 context deadline 约束执行。
current gaps:
  当前定义解析没有显式拒绝有环 DAG，也没有独立的 workflow cancel 状态。
```

Apache Airflow 文档把 DAG 解释为包含任务、任务依赖、调度和回调等运行信息的 workflow 模型，并强调 task 默认等待所有上游成功后再运行。Python 标准库 `graphlib.TopologicalSorter` 提供了 `prepare`、`get_ready`、`done` 这类接口，正好对应“发现 ready node、交给 worker、完成后释放下游”的调度循环。Argo Workflows 的 DAG 文档也强调，依赖图比纯 steps 序列更适合复杂 workflow，并能释放最大并行度。LogServe 当前实现采取的是更小的版本：每次有 step 完成后扫描定义顺序，找到依赖已经成功的 step，再创建 task。

## Q001. DAG 的定义是什么？

**回答：**

DAG 是 directed acyclic graph，有向无环图。形式化地说，它由一组节点 `V` 和一组有方向的边 `E` 组成；如果存在边 `A -> B`，就表示从 `A` 指向 `B` 的依赖关系；“无环”表示不能从某个节点沿着有向边走一圈又回到自己。

放到 workflow engine 里，DAG 的含义通常是：

```text
node:
  一个 step / task / activity / operator。
edge:
  一个依赖约束。A -> B 通常表示 B 要等 A 完成后才能运行。
topological order:
  一种合法执行顺序。任何依赖边 A -> B 中，A 都必须排在 B 前面。
parallelism:
  没有依赖关系的节点可以并行执行，不必按照文件里的声明顺序串行执行。
```

所以 DAG 不是“任务列表”。任务列表只有先后顺序，DAG 表达的是偏序。比如：

```text
A -> B
A -> C
B -> D
C -> D
```

这里合法执行不是只有 `A, B, C, D` 一种。`A` 完成后，`B` 和 `C` 都 ready，可以并行；只要 `B` 和 `C` 都完成，`D` 才能运行。这个形状常叫 diamond DAG。

面试时可以这样答：

```text
DAG 在 workflow engine 里是调度语义，不只是数据结构。节点是 step，边是依赖，边的方向要统一解释，比如 upstream -> downstream。无环保证至少存在一种拓扑顺序，也保证调度器不会因为 A 等 B、B 又等 A 而永远没有 ready node。DAG 的价值是表达偏序和并行度：有依赖的节点按约束执行，互不依赖的节点可以并发执行。
```

需要注意两个边界。

第一，DAG 不等于“执行历史”。DAG 是定义或计划，告诉引擎什么可以先后执行；执行历史还包括每个 step 的 attempt、开始时间、结束时间、worker、错误、重试、结果引用等运行时状态。

第二，DAG 不等于“数据流图”。有些系统里边表示控制依赖：A 成功后 B 才能运行；有些系统里边表示数据依赖：B 的输入来自 A 的输出。两者经常重合，但不是同一个概念。生产系统最好显式区分，否则会出现“输入引用了 A，但 depends_on 忘了写 A”的隐性依赖。

LogServe 当前的定义在 `internal/workflow/model.go` 里：`Definition` 里有 `Steps []StepDefinition`，每个 `StepDefinition` 里有 `DependsOn []string`。也就是说，LogServe 把 workflow DAG 表达成“节点列表 + 每个节点声明自己的上游依赖”。这里的边可以理解为：

```text
dep in step.depends_on  =>  dep -> step
```

也就是 `step` 要等 `dep` 成功后才能被调度。

## Q002. 如何检测 workflow 定义中是否存在环？

**回答：**

检测环之前，先把 workflow definition 规范化成图。通常要做四类校验：

```text
1. step_id 唯一，不能重复。
2. depends_on 指向的 step_id 必须存在。
3. step 不能依赖自己。
4. 整个依赖图不能有有向环。
```

检测有向环最常见有两种方法。

第一种是 Kahn 算法。先统计每个节点的入度。入度为 0 的节点表示没有未满足的前置依赖，可以进入 ready 队列。然后不断弹出 ready 节点，把它的下游入度减 1；如果某个下游入度变成 0，也加入 ready 队列。最后如果处理过的节点数小于总节点数，就说明剩下的节点互相等待，图中有环。

伪代码可以这样写：

```text
build graph dep -> dependent
indegree[step] = number of dependencies
queue = all steps with indegree 0
visited = 0

while queue not empty:
  node = queue.pop()
  visited += 1
  for child in outgoing[node]:
    indegree[child] -= 1
    if indegree[child] == 0:
      queue.push(child)

if visited != len(steps):
  graph has cycle
```

第二种是 DFS 三色标记。节点初始是 white，正在访问是 gray，访问完成是 black。DFS 访问依赖边时，如果从当前节点走到了 gray 节点，就发现了回边，也就是环。DFS 的好处是容易拿到一条具体环路径，比如 `A -> B -> C -> A`，对报错非常友好。

面试时可以这样答：

```text
我会在 workflow 提交或编译阶段就检测环，而不是运行时才发现没有 ready node。实现上可以用 Kahn 算法或 DFS 三色标记。Kahn 算法适合顺便生成拓扑层级和 ready 集合；DFS 适合报出具体环路径。无论用哪种算法，复杂度都是 O(V + E)，其中 V 是 step 数，E 是依赖边数。
```

工程上还要注意报错质量。不要只返回 `cycle detected`，最好告诉用户：

```text
workflow "rag_pipeline" has dependency cycle:
embed -> search -> answer -> embed
```

这样用户能直接修 definition。

LogServe 当前边界要讲清楚：`ParseDefinition` 只负责 JSON 反序列化和默认值填充，比如默认 `max_attempts=3`、`timeout_ms=30000`，没有在提交阶段显式检测重复 step、未知依赖或环。`scheduleReadySteps` 运行时只会调度 `DependenciesSucceeded` 为 true 的 step。如果定义里有环，相关 step 会一直停在 `SCHEDULED` 且没有 task id，因为没有任何一个节点能先成功。这个行为对最小原型能暴露问题，但对生产系统不够好；更好的做法是在 `SubmitWorkflow` 里先拒绝非法 DAG。

## Q003. 拓扑排序有哪些常见算法？

**回答：**

拓扑排序是把 DAG 的偏序约束转成一个线性顺序。只要图是 DAG，就至少存在一种拓扑序；如果图有环，就不存在合法拓扑序。

常见算法主要有三类。

第一类是 Kahn 算法，也叫入度算法。它维护每个节点剩余未满足依赖数量，把入度为 0 的节点放进 ready 队列。每处理完一个节点，就减少下游节点的入度。这个算法天然适合 workflow engine，因为 ready 队列就是可以提交给 worker 的任务集合。

第二类是 DFS 后序算法。对每个节点做深度优先搜索，先访问它依赖的下游或上游，再把节点放入结果栈，最后反转得到拓扑序。DFS 同时可以用三色标记检测环。

第三类是工程增强版本，比如：

```text
stable topological sort:
  多个节点同时 ready 时，按定义顺序、优先级、创建时间或 step_id 排序，保证结果可复现。
layered topological sort:
  输出一层一层的 ready 集合，适合批量调度和并行执行。
incremental topological scheduling:
  不一次性生成完整顺序，而是在节点完成后动态释放下游。
priority Kahn:
  ready 队列不是 FIFO，而是优先队列，用于处理优先级、deadline、资源类型。
SCC-based validation:
  先用强连通分量算法找出环组，用于更好的诊断。
```

面试时可以这样答：

```text
最常见的是 Kahn 入度算法和 DFS 后序算法。Kahn 维护 ready 队列，非常适合调度器；DFS 写起来简单，也容易报出环路径。两者复杂度都是 O(V + E)。如果是生产 workflow engine，我通常会用 Kahn 或 Kahn 的稳定版本做调度，因为它可以直接输出当前可运行节点；同时在 definition validate 阶段保留 DFS 或 Kahn 的 cycle diagnostics。
```

从 LogServe 当前代码看，它还没有单独实现一个完整 `TopologicalSorter`。调度逻辑在 `internal/control/service.go` 的 `scheduleReadySteps`：每次扫描 `state.Definition.Steps`，跳过已经绑定 task 的 step，检查依赖是否全部成功，然后解析输入并 enqueue task。这个逻辑是“运行时 ready 扫描”，不是“一次性拓扑排序”。它的优点是简单，和 workflow 状态天然绑定；缺点是没有显式拓扑层、没有 cycle diagnostics，也没有复杂的优先级调度。

## Q004. Kahn 算法和 DFS 拓扑排序有什么区别？

**回答：**

Kahn 算法和 DFS 都能做拓扑排序，但它们的思维方式不一样。

Kahn 算法是“从没有依赖的节点开始”。它关注的是哪些节点现在 ready。只要一个节点的所有前置依赖都已经处理完，它就进入 ready 队列。这个模型和调度器非常接近：

```text
get_ready() -> 交给 worker
worker done(node) -> 释放下游
get_ready() -> 再交给 worker
```

Python 标准库 `graphlib.TopologicalSorter` 的接口也是这种风格：先 `prepare` 检查图，再不断 `get_ready`，等 worker 完成后调用 `done`，新节点才会变成 ready。这说明 Kahn 思路不仅能排序，还能支持并行处理。

DFS 拓扑排序是“沿着依赖关系一直走到底”。它递归访问节点，把依赖路径访问完以后再把节点加入结果。用三色标记时，DFS 很容易判断是否遇到正在访问的节点，也就是环。

两者对比如下：

```text
Kahn:
  - 数据结构：入度表 + ready 队列。
  - 天然输出当前 ready nodes。
  - 适合 workflow 调度、并发执行、限流、优先级队列。
  - 处理完节点数不足时说明有环，但默认不一定给出完整环路径。

DFS:
  - 数据结构：递归栈或显式栈 + 颜色表。
  - 适合静态校验和生成一个拓扑序。
  - 很容易定位环路径。
  - 递归实现要注意深图导致栈深问题。
```

面试时可以这样答：

```text
如果我要写 workflow runtime，我更偏向 Kahn，因为 runtime 最关心的不是“唯一顺序”，而是“现在有哪些节点可以运行”。Kahn 的 ready queue 能直接接 worker pool。DFS 更适合 definition validate，尤其是要报出 A -> B -> C -> A 这种环路径时。两者复杂度一样，但工程用途不同。
```

还有一个容易忽略的点：拓扑序通常不唯一。比如 `A` 完成后 `B` 和 `C` 都 ready，那么 `A, B, C, D` 和 `A, C, B, D` 都合法。生产系统如果需要可复现的 replay 或测试稳定性，就要定义 tie-breaker，例如按 workflow definition 里的 step 顺序、step priority 或 step id 排序。

LogServe 当前的 tie-breaker 实际上是 definition 顺序。`scheduleReadySteps` 遍历 `state.Definition.Steps`，因此多个 step 同时 ready 时，会按定义顺序创建 task。这个顺序不是 DAG 语义本身的一部分，只是当前实现里的稳定调度策略。

## Q005. DAG 中 ready node 的判定条件是什么？

**回答：**

最基础的 ready node 条件是：一个节点的所有前置依赖都已经满足，并且它自己还没有执行完成。对于默认 `all_success` 语义，就是所有 upstream 节点都成功。

但 workflow engine 里的 ready 通常不只是图论条件，还要叠加运行时条件：

```text
dependency condition:
  所有上游节点达到触发规则要求的状态，比如 all_success、all_done、one_success。
node state condition:
  当前节点没有成功、没有失败到终态、没有正在运行、没有已经绑定未完成 task。
input condition:
  当前节点需要的上游输出、参数、artifact、secret 都能解析。
retry/backoff condition:
  当前 attempt 没超过 max_attempts，且重试退避时间已经到。
resource condition:
  worker capacity、队列容量、并发上限、rate limit 允许调度。
workflow condition:
  workflow 仍处于 RUNNING，没有被 cancel、timeout、pause 或 fail-fast 阻止。
```

Airflow 默认会等一个 task 的所有直接上游成功后再运行，但也提供 trigger rule，比如 `all_done`、`one_success`、`none_failed` 等。Argo DAG 默认是 fail fast：某个 task 失败后不再调度新 task，等待正在运行的 task 结束后把 DAG 标成失败；也可以通过配置让其他分支继续跑完。这些都说明 ready node 的定义不是纯图论，而是“图依赖 + 状态机语义”。

面试时可以这样答：

```text
ready node 是“从依赖角度和运行时角度都可以提交执行的节点”。在最简单 DAG 里，它就是所有 predecessor 都成功且自己未运行的节点。到工程系统里，还要检查输入是否可解析、是否超过 retry 上限、是否处于 backoff、workflow 是否取消、资源和 admission control 是否允许。否则图上 ready 了，也不代表现在就应该创建 task。
```

LogServe 当前的 ready 条件比较清晰，集中在 `scheduleReadySteps`：

```text
1. workflow status 必须是 RUNNING。
2. step status 必须是 SCHEDULED。
3. step.TaskID 必须为空，避免重复创建 task。
4. DependenciesSucceeded(stepDef, state) 必须为 true。
5. ResolveArgs 必须成功。
6. enqueueTask 和 StepScheduled 日志写入成功。
```

其中 `DependenciesSucceeded` 的语义很严格：每个 `depends_on` 指向的 step 必须存在，并且状态必须是 `SUCCEEDED`。它没有 `all_done`、`one_success` 或 skipped 分支语义。所以 LogServe 当前是一个默认 `all_success` DAG 调度器。

这里还有一个实现细节：LogServe 的 step 初始状态叫 `SCHEDULED`，但这不代表已经创建了底层 task。真正创建 task 以后，`TaskID` 才会被写回 step state。因此当前 ready 判断里必须同时看 `Status == SCHEDULED` 和 `TaskID == ""`。如果只看状态，很容易重复 enqueue。

## Q006. workflow step 的输入依赖如何解析？

**回答：**

workflow step 的输入依赖解析，核心是把“定义时的引用”变成“执行时的真实参数”。一个 step 的输入通常有三类：

```text
literal input:
  definition 里直接写死的 JSON、字符串、数字、配置参数。
upstream output:
  引用前面某个 step 的结果，比如 search 的输入来自 embed 的输出。
external resource:
  引用对象存储、数据库、文件、模型 checkpoint、secret 或 artifact。
```

解析时要分清两个概念：调度依赖和数据依赖。

调度依赖回答“什么时候可以运行”。例如 `depends_on: ["embed"]` 表示当前 step 要等 `embed` 成功。

数据依赖回答“输入值从哪里来”。例如 `args_json` 里写 `{"__step_ref__": "embed"}`，表示这里要替换成 `embed` 的输出。

两者经常同时出现，但不能混为一谈。一个 step 可以依赖 A 的完成，但不读取 A 的输出；也可以读取 A 的输出，此时通常也应该隐含依赖 A。否则调度器可能提前解析输入，发现 A 还没有结果。

一个稳妥的输入解析流程是：

```text
1. 解析 step 的 args_json。
2. 遍历 JSON 结构，找到特殊引用节点，比如 {"__step_ref__": "step_id"}。
3. 校验引用的 step 存在。
4. 校验引用的 step 已经成功，并且结果可读。
5. 如果结果内联在状态里，直接解码。
6. 如果结果是 result_ref，从 result store 读取并校验。
7. 把引用替换成真实 JSON 值。
8. 对最终 resolved args 做 canonical marshal，并计算 input hash。
```

面试时可以这样答：

```text
我会把输入依赖解析放在调度前，而不是 worker 运行到一半才发现上游结果不存在。解析过程要递归处理参数里的 step ref，校验上游成功，必要时从外部 result store 拉取大结果，并对解析后的参数计算 hash。这个 hash 可以用于幂等键、缓存键和重试去重。生产系统还应该让数据引用自动补齐控制依赖，或者在校验阶段拒绝“引用了上游但没有声明依赖”的 definition。
```

LogServe 当前就是这个思路。`internal/workflow/args.go` 的 `ResolveArgs` 会解析 `ArgsJSON`，递归查找只包含 `__step_ref__` 的对象，然后把它替换成对应 step 的结果。如果 step 的 `ResultJSON` 为空但有 `ResultRef`，就通过 `ResultLoader.LoadResult` 拉取外部结果。最后它会把 resolved args 重新 marshal 成 JSON，并计算 SHA-256，得到 `inputHash`。

这个 `inputHash` 很关键。`scheduleReadySteps` 创建 task 时会把幂等键拼成：

```text
workflow_id:step_id:input_hash:attempt:n
```

step 成功事件也会带上类似的去重键：

```text
workflow_id:step_id:last_input_hash:succeeded
```

这意味着“同一个 step 在同一份输入下的成功结果”可以被去重；如果上游结果变化导致 resolved args 变化，hash 也会变化，语义上就是另一次不同输入的执行。

## Q007. workflow step 失败后应该重试当前 step 还是重跑下游？

**回答：**

默认答案是：先重试当前失败的 step，而不是重跑下游。因为在普通 DAG 语义里，下游只有在上游成功后才会被调度。如果当前 step 失败时下游还没运行，就没有“重跑下游”的问题。

更准确地说，要分几种情况。

第一种，当前 step 还没有成功过，只是某一次 attempt 失败。比如网络抖动、worker crash、临时超时、外部服务 503。这时应该在 retry policy 限制内重试当前 step，并且用同一个 resolved input。下游继续等待，不应该提前跑。

第二种，当前 step 已经成功，下游也已经基于它的旧结果跑过；后来因为人工修复、缓存失效、动态 workflow 修改或数据回滚，需要重新计算当前 step。这时就不只是 retry，而是 invalidation。所有读取旧输出的下游结果都可能过期，需要按血缘关系重跑下游，或者创建一个新的 workflow run。

第三种，当前 step 有外部副作用，比如发邮件、扣款、写第三方系统。此时不能简单重试，必须先设计幂等键、事务外盒、去重记录或补偿动作。否则重试当前 step 可能比重跑下游更危险。

面试时可以这样答：

```text
在静态 DAG 的默认 all_success 语义里，step 失败后优先重试当前 step，因为下游还没有满足依赖，理论上不会运行。只有当上游结果已经被下游消费，后来又发生重算、回滚或动态修改时，才需要按依赖血缘 invalidation 下游。重试策略要受 max_attempts、backoff、timeout 和 non-retryable error 控制；对于有副作用的 step，还必须先有幂等设计。
```

Temporal 文档也有类似的工程建议：通常不建议简单重试整个 workflow execution，而是重试 workflow 内部失败的 activity 或某个局部步骤。原因是整个 workflow replay 往往会重复同一段逻辑，成本更高，也不一定解决外部依赖问题。

LogServe 当前实现是“重试当前 step”。`completeWorkflowStep` 在收到失败状态后，会先写 `StepFailed` 事件，然后检查 `step.Attempts < StepMaxAttempts`。如果还有重试次数，就把这个 step 状态改回 `SCHEDULED` 并清空 `TaskID`，让下一轮 `scheduleReadySteps` 重新创建 task。如果超过最大次数，才调用 `failWorkflow` 把 workflow 标成失败。

这套语义有几个结果：

```text
1. 下游 step 只有在 depends_on 全部 SUCCEEDED 后才会被调度。
2. 上游失败期间，下游不会跑。
3. 当前 step 重试会增加 attempt。
4. 超过 max_attempts 后 workflow 失败。
5. 如果 task 超时，worker 会把它作为 TaskFailed 回传，走同一套 step retry 逻辑。
```

LogServe 的集成测试也覆盖了这个行为：`TestWorkflowRetriesFailedStep` 验证 flaky step 失败一次后第二次成功；`TestWorkflowRetriesTimedOutStep` 验证超时 step 按最大次数重试后 workflow 失败。

## Q008. workflow 的中间结果应该内联保存还是外部引用？

**回答：**

中间结果应该内联还是外部引用，取决于结果大小、访问频率、可观测性、成本和生命周期。

内联保存的优点是简单。结果直接在 workflow state、事件日志或 metadata 里，查看状态、replay、调试都方便，也不需要额外访问对象存储。它适合小 JSON、小字符串、小结构化结果，比如分类标签、检索 query、少量候选 id。

内联保存的问题是会放大日志和 metadata。大 payload 会让每次状态读取、复制、replay、备份都变慢；如果结果里有敏感数据，还会扩大暴露面。Airflow XCom 文档也提醒，XCom 适合 task 之间传小数据，不适合传大 dataframe 这类大对象。

外部引用的优点是把大结果从控制面剥离出去。workflow state 只保存 `result_ref`、checksum、大小、content type、版本等元信息，真实数据放在对象存储、文件系统、数据库或 artifact store。Argo Workflows 也把 artifact 当作 workflow 输入输出的一等概念，并提供 artifact garbage collection 这类生命周期管理能力。

外部引用的问题是系统复杂度更高：

```text
durability:
  success event 写入前，结果对象必须已经持久化成功。
atomicity:
  不能出现日志说 step succeeded，但 result_ref 指向的对象不存在。
integrity:
  最好保存 checksum、size、content type，读取时校验。
retention:
  中间结果和最终结果生命周期可能不同，不能被过早 GC。
security:
  result_ref 不能泄露敏感 bucket/key，权限要和 workflow 隔离。
```

面试时可以这样答：

```text
小结果可以内联，换取简单和可观测性；大结果、二进制结果、敏感结果和高频中间 artifact 应该外部化，只在 workflow state 里保存 result_ref 和校验元信息。关键不是二选一，而是要保证结果持久化和状态事件的一致性：不能先标成功再上传结果，也不能让 GC 提前删掉仍被下游引用的对象。
```

LogServe 当前使用混合策略。`Service.materializeResult` 会判断 `len(resultJSON) > resultInlineThreshold && resultStore != nil`：如果结果超过阈值且配置了 result store，就调用 `resultStore.Put`，并在 workflow 事件里只保存 `ResultRef`；否则把结果 JSON 内联保存。下游 `ResolveArgs` 解析 `__step_ref__` 时，如果发现上游 step 没有 `ResultJSON` 但有 `ResultRef`，就通过 `LoadResult` 读取真实结果。

这套设计的好处是控制面可以处理大结果，但保持 workflow 状态结构稳定。面试里可以顺带指出一个严谨点：外部结果写入必须发生在 `StepSucceeded` 事件之前。LogServe 当前就是先 `materializeResult`，再 append `StepSucceeded`，避免日志里出现一个不可读的成功结果。

## Q009. workflow 取消和超时如何传播到正在运行的 step？

**回答：**

取消和超时都属于“停止继续执行”的控制信号，但语义不完全一样。

取消通常来自用户或上层系统，表示“不想要这个 workflow 的结果了”。超时通常来自 deadline，表示“这个 workflow 或 step 已经过了允许执行时间”。取消可能是主动的、业务上的；超时是时间策略触发的取消或失败。

一个完整 workflow engine 里，传播路径一般是：

```text
workflow cancellation / deadline fired
  -> workflow state 标记为 canceling / timed out
  -> scheduler 停止调度新的 ready steps
  -> queued steps 从队列移除或标记 canceled
  -> running steps 收到 cancel signal / context cancellation
  -> executor 尝试优雅停止
  -> 超过 grace period 后强制 kill 或标记 lost
  -> step 写入 CANCELED / FAILED / TIMED_OUT 终态
  -> workflow 写入最终 CANCELED / FAILED / TIMED_OUT 状态
```

这里最难的是 running step。已经在 worker 上跑起来的代码不一定能被安全抢占。不同执行器能力也不同：

```text
cooperative cancellation:
  任务代码定期检查 ctx.Done、heartbeat 或 cancel token，然后自己退出。
preemptive kill:
  引擎杀进程、杀容器、断开沙箱，适合隔离强但清理成本高的执行环境。
best-effort cancellation:
  发出取消信号，但不保证任务马上停；最终靠 lease timeout 或 heartbeat timeout 收敛。
```

Temporal 的 Go SDK 文档里也强调，Activity 要能收到 cancellation request，通常需要 heartbeat，并监听上下文取消。这个点在面试里很有价值：取消不是控制面把状态改掉就完事，运行中的 worker 必须有观察取消的机制。

面试时可以这样答：

```text
workflow cancel 首先要阻止新 step 被调度，然后处理已排队和正在运行的 step。已排队的可以直接标记 canceled；正在运行的要通过 context、heartbeat、RPC cancel 或进程信号传播给 worker。超时可以实现成 deadline 触发的取消，但最终状态要区分业务取消、step timeout 和系统失败。为了不丢语义，取消请求、step 观察到取消、最终终态都应该写事件日志。
```

LogServe 当前要分清“已有能力”和“缺口”。

已有能力是 step timeout。`StepDefinition.TimeoutMs` 会进入 `TaskSpec.TimeoutMs`，worker 在 `executeTask` 里用 `context.WithTimeout` 包住执行。如果执行返回 `context.DeadlineExceeded`，worker 会把 task 标为 failed，并把错误写成类似 `task timed out after 50ms`。对于 Python executor，超时后还会重启 runner，避免一个已经卡住的 Python 进程污染后续任务。控制面收到失败后，按当前 step retry 逻辑处理；超过最大次数后 workflow 失败。

缺口是显式 workflow cancellation。当前 proto 里 workflow 状态只有 `RUNNING`、`COMPLETED`、`FAILED`，step 状态也只有 `SCHEDULED`、`STARTED`、`SUCCEEDED`、`FAILED`，没有 `CANCELING` 或 `CANCELED`。控制面也没有 `CancelWorkflow` 这样的 API。所以面试回答要诚实：LogServe 现在支持 step 级 timeout 传播到 worker context，但还没有完整的 workflow cancel 语义。

如果要补齐，至少需要：

```text
1. 增加 CancelWorkflow API。
2. 增加 WorkflowCancelRequested / StepCanceled / WorkflowCanceled 事件。
3. 增加 CANCELED 或 CANCELING 状态。
4. scheduler 在 cancel 后停止创建新 task。
5. queued task 能被撤销或跳过。
6. running task 能通过 lease、context、heartbeat 或 executor kill 收到取消。
7. terminal completion 要能和 cancel 竞争，并有明确优先级。
```

## Q010. workflow 的最终结果如何定义？

**回答：**

workflow 的最终结果应该由 workflow contract 定义，而不是由“最后一个完成的 step”随便决定。因为 DAG 里多个分支可以并行完成，最后完成的节点可能只是一个旁路清理任务、通知任务或统计任务，不一定是业务结果。

常见定义方式有几种：

```text
explicit result step:
  definition 指定 result_step_id，最终结果取这个 step 的输出。
aggregate result:
  workflow 有一个 join / reduce step，汇总多个分支输出。
named outputs:
  workflow 声明多个 output key，每个 key 来自某个 step 或表达式。
side-effect workflow:
  workflow 没有业务返回值，最终结果只是完成状态和审计信息。
artifact output:
  最终结果是一个 artifact ref，比如报告文件、模型文件、数据集路径。
```

一个严谨的 workflow engine 还要定义终态条件：

```text
success:
  需要哪些 step 成功？是否允许某些 optional step skipped？
failure:
  任一 required step 失败就失败，还是 failFast=false 时等其他分支跑完？
canceled:
  用户取消后是否保留部分结果？
timeout:
  workflow deadline 超时后，正在运行的 step 如何收敛？
result consistency:
  result 写入和 workflow completed 事件是否原子可恢复？
```

面试时可以这样答：

```text
最终结果必须是 workflow 定义的一部分。最简单做法是显式指定 result_step_id，或者约定最后一个 definition step 是 result step；更复杂的系统会支持 named outputs 和 aggregate step。不能用“最后完成的 step”来定义结果，因为 DAG 并发下最后完成具有偶然性。最终结果写入也应该是一个幂等的终态事件，避免重复 completion 产生两个不同结果。
```

LogServe 当前采用的是 `ResultStepID`。提交 workflow 时，如果 definition 没有填写 `result_step_id`，控制面会默认使用 `def.Steps` 里的最后一个 step。workflow 什么时候 completed，不是只看 result step，而是 `workflowDone` 要求 `StepOrder` 里的所有 step 都是 `SUCCEEDED`。然后 `completeWorkflow` 读取 `ResultStepID` 对应 step 的结果：如果结果内联就直接用，如果只有 `ResultRef` 就从 result store 拉取，再对 workflow 最终结果做一次 `materializeResult`。

这套语义可以概括为：

```text
terminal success condition:
  所有 step 都 SUCCEEDED。
result source:
  Definition.ResultStepID 对应 step 的输出。
default result step:
  如果用户没指定，用 definition 里的最后一个 step。
result storage:
  小结果内联，大结果 result_ref。
completion idempotency:
  WorkflowCompleted 使用 workflow_id:completed 作为幂等键。
```

LogServe 的集成测试 `TestWorkflowSimpleRAGReplayAndDedup` 还验证了一个关键点：重复完成 final step 不会写入第二个 `WorkflowCompleted`。这说明最终结果不是靠 worker 的重复回调直接覆盖，而是通过控制面的终态和幂等日志收敛。

如果把这个问题讲到架构层，可以补一句：workflow final result 本质上是“状态机进入成功终态时暴露给调用方的投影”。它可以来自某个 step，也可以来自多个 step 的聚合，但必须可重放、可恢复、可幂等。

## Q011. DAG 调度如何利用并行度？

**回答：**

DAG 调度利用并行度的关键，不是把 step 按定义顺序一个个跑，而是不断找出当前所有 ready node，把这些 ready node 尽量分发给 worker。只要两个节点之间没有依赖关系，它们就不需要互相等待。

一个典型调度循环可以这样理解：

```text
1. 找出所有入度为 0 或所有上游已完成的节点。
2. 把这些节点放入 ready queue。
3. 在 worker、pool、资源配额允许的范围内并发启动。
4. 某个节点完成后，释放它的下游依赖。
5. 下游如果所有依赖都满足，也进入 ready queue。
6. 重复这个过程，直到 workflow 成功、失败、取消或超时。
```

所以并行度来自 DAG 的形状。比如：

```text
A -> B -> C -> D
```

这是链式 DAG，即使有 100 个 worker，也只能一次跑一个关键 step。再看：

```text
      -> B ->
A              -> E
      -> C ->
      -> D ->
```

`A` 完成后，`B`、`C`、`D` 可以同时跑。worker 越多，越能吃掉这一层的并行度。

面试时可以这样答：

```text
DAG 调度器利用并行度的方式是维护 ready set，而不是维护一个线性任务序列。每当某个 step 完成，调度器就更新下游依赖计数，把新变 ready 的节点放入队列。实际能跑多少，还要取 min(ready 节点数、worker 数、队列容量、资源池限制、全局并发限制)。DAG 只能提供理论并行度，系统资源和调度策略决定实际并行度。
```

工程里还要注意几类限制。

第一，资源类型限制。两个 step 图上互不依赖，但都要用同一张 GPU 或同一个外部 API 配额，仍然不能无限并行。

第二，队列和 worker 限制。如果 ready node 一次释放 1 万个，直接全部 enqueue 可能把任务队列、metadata store 或下游服务打满。调度器需要 admission control。

第三，公平性限制。一个大 workflow 释放大量 ready step 时，不能把所有 worker 都占住，导致小 workflow 长时间排队。

LogServe 当前的 `scheduleReadySteps` 是一个简单的 ready 扫描器。它遍历 `state.Definition.Steps`，对每个还没有 `TaskID` 的 `SCHEDULED` step 检查 `DependenciesSucceeded`，满足条件就创建 task。也就是说，如果多个 step 同时 ready，LogServe 会在同一轮扫描里把它们都 enqueue，后续由 worker 拉取执行。

这能利用基础并行度，但它还没有更复杂的控制项，比如 per-workflow 并发上限、资源类型、优先级、critical path 优先调度、ready queue 分层。当前实现更适合展示 DAG 调度机制，不是完整的生产级调度器。

## Q012. critical path 如何影响 workflow latency？

**回答：**

Critical path 是 DAG 里耗时最长的一条依赖路径。Workflow 的最短完成时间不可能小于 critical path 的长度，因为这条路径上的 step 必须一个接一个完成。

如果每个节点有执行时间 `duration[v]`，critical path 可以这样算：

```text
earliest_finish[v] = duration[v] + max(earliest_finish[p] for p in predecessors[v])
workflow_latency_lower_bound = max(earliest_finish[v] for all terminal nodes)
```

举个例子：

```text
A(2s) -> B(5s) -> D(2s)
     \-> C(1s) -/
```

`A-B-D` 是 9 秒，`A-C-D` 是 5 秒。即使 `B` 和 `C` 可以并行，workflow 也至少要等 `B` 完成，最后再跑 `D`。所以 critical path 决定了 latency 下界。

面试时可以这样答：

```text
Workflow latency 主要由 critical path 决定。增加 worker 数只能缩短非关键路径上的排队和并行分支耗时，不能突破关键路径的依赖顺序。优化 workflow 延迟时，要先找关键路径，再看关键路径上的 step 是 CPU 慢、I/O 慢、外部服务慢、排队慢，还是 retry/backoff 拉长了时间。
```

实际系统里，critical path 不只包含函数执行时间，还包括：

```text
queue wait:
  step ready 后等 worker 的时间。
scheduling overhead:
  调度循环、metadata 更新、日志写入、RPC 调用。
input materialization:
  读取上游 result_ref、下载 artifact、反序列化参数。
execution time:
  真实业务函数耗时。
retry/backoff:
  失败后的重试等待和重复执行。
result materialization:
  上传大结果、写 StepSucceeded、更新状态。
```

如果某个关键路径 step 重试三次，workflow latency 会被直接拉长。相反，如果一个非关键分支慢一点，只要它没有拖住 join step，用户可能感知不到。

LogServe 当前记录了 step 的 `LatencyMs` 和 workflow 的整体 latency。`StepSucceeded`、`StepFailed`、`WorkflowCompleted` 事件里都有时间信息，`workflow.WorkflowLatencyMs` 也能从 replay 后的状态计算整体耗时。现在还没有直接输出 critical path 的指标，但基于 `StepOrder`、`DependsOn` 和每个 step 的 `LatencyMs`，可以在控制面或分析工具里离线算出来。

一个很实用的压测指标是：

```text
critical_path_ms:
  按 DAG 依赖和 step latency 计算的最长路径。
workflow_latency_ms:
  用户看到的端到端耗时。
overhead_ms = workflow_latency_ms - critical_path_ms:
  调度、排队、存储、worker 空闲间隔等额外开销。
```

如果 `critical_path_ms` 很高，说明业务步骤本身慢；如果 `overhead_ms` 很高，说明调度或资源供给有问题。

## Q013. 如何计算 workflow 的最大并行度？

**回答：**

最大并行度可以从两个角度看：理论并行度和实际并行度。

理论最大并行度只看 DAG 结构。它关心在不违反依赖的情况下，最多有多少个节点可以同时运行。对于每个 step 耗时相同的简化模型，可以按拓扑层计算：

```text
level 0: 没有依赖的节点
level k: 依赖都落在更早层的节点
max_parallelism = max(size(level_i))
```

比如：

```text
level 0: A
level 1: B, C, D
level 2: E
```

理论最大并行度就是 3。

更一般的说，最大并行度和图里的 antichain 有关。Antichain 是一组互相没有依赖顺序的节点，它们理论上可以同时运行。最大 antichain 的大小就是 DAG 在偏序意义上的最大宽度。不过面试里通常不需要展开 Dilworth 定理，讲到“同一时间最多 ready 的无依赖节点数”就够了。

如果每个 step 耗时不同，计算会更接近调度模拟。可以假设无限 worker，按最早开始时间调度：

```text
start[v] = max(finish[p] for p in predecessors[v])
finish[v] = start[v] + duration[v]
```

然后扫描所有时间区间，看任意时刻有多少 step 处于 running 状态，最大值就是这组 duration 下的瞬时最大并行度。

还有一个常用指标叫平均并行度：

```text
total_work = sum(duration[v])
critical_path = longest_dependency_path_duration
average_parallelism = total_work / critical_path
```

这个值表示，如果想达到 critical path 下界，平均需要多少并行资源。它不是瞬时最大值，但很适合容量估算。比如总工作量 100 秒，critical path 20 秒，平均并行度是 5。你给 2 个 worker 肯定跑不到 20 秒；给 100 个 worker 也未必比 20 秒更快。

面试时可以这样答：

```text
我会先区分理论最大并行度和实际最大并行度。理论上可以按拓扑层或最大 antichain 估算，带 duration 时可以做最早开始时间模拟。实际并行度还要受 worker 数、资源池、外部配额、队列容量和调度器限流影响，所以运行时看到的是 min(DAG ready 宽度, 系统资源限制)。
```

放到 LogServe，当前可以做两类计算。

第一类是静态计算。读取 `Definition.Steps` 和 `DependsOn`，构建 DAG，然后按 Kahn 算法分层，得到每层节点数和理论 `max_ready_width`。

第二类是运行时计算。用 workflow log 里的 `StepStarted` 和 `StepSucceeded` / `StepFailed` 事件构造每个 step 的运行区间，扫描时间线得到实际并发数。这个指标比静态宽度更真实，因为它包含 worker 数、排队、重试和调度延迟。

LogServe 当前没有内置 `max_parallelism` 字段。面试里可以说：项目已经有 replay 和 step latency 事件基础，下一步可以很自然地加出 `ready_width`、`running_steps`、`critical_path_ms` 和 `scheduler_overhead_ms` 这几个指标。

## Q014. 如何设计 workflow 的 retry policy？

**回答：**

Workflow 的 retry policy 不应该只写一个 `max_attempts=3`。更完整的设计至少要回答这些问题：

```text
retry target:
  重试整个 workflow，还是只重试失败 step？
max attempts:
  最多尝试几次？不同 step 是否可以不同？
timeout budget:
  单次 attempt timeout、整个 step timeout、整个 workflow deadline 怎么配合？
backoff:
  固定间隔、指数退避、最大退避、jitter 怎么设置？
retryable errors:
  哪些错误可重试，哪些错误应该立即失败？
idempotency:
  重试是否会重复写外部系统？有没有幂等键？
resource pressure:
  大量重试是否会放大故障，是否需要 retry budget 或熔断？
observability:
  attempt、last error、next retry time、最终失败原因是否可见？
```

默认策略通常是“重试局部 step，而不是重试整个 workflow”。Temporal 文档也强调，Activity 默认有 retry policy，而 Workflow Execution 默认不重试；失败点应该尽量局部化，因为整个 workflow 重跑会重复同一段编排逻辑，成本更高，也未必解决外部依赖问题。

Argo Workflows 的 `retryStrategy` 也体现了这个思路。它支持 `limit`、`retryPolicy`，比如 `Always`、`OnFailure`、`OnError`、`OnTransientError`，还支持按 `lastRetry.exitCode`、`lastRetry.duration`、`lastRetry.message` 等条件表达式决定是否继续重试。

面试时可以这样答：

```text
我会把 retry policy 设计成 per-step 的局部恢复策略。默认重试失败 step，不重试整个 workflow；可重试错误才重试，永久错误直接失败；每次重试有 timeout、指数退避和 jitter；总次数和总时间受 budget 限制。对有副作用的 step，retry policy 必须和 idempotency key 一起设计，否则重试只是在制造重复副作用。
```

一个比较稳妥的配置可以长这样：

```text
max_attempts: 3
initial_backoff_ms: 500
backoff_multiplier: 2.0
max_backoff_ms: 30_000
jitter: true
attempt_timeout_ms: 10_000
step_deadline_ms: 60_000
retryable_errors:
  - timeout
  - transient_network
  - rate_limited
non_retryable_errors:
  - validation_error
  - permission_denied
  - schema_mismatch
```

LogServe 当前实现是一个简化版本。`ParseDefinition` 会给 workflow 和 step 填默认 `MaxAttempts` 和 `TimeoutMs`，`completeWorkflowStep` 失败后检查 `step.Attempts < StepMaxAttempts`。如果还有次数，就把 step 状态改回 `SCHEDULED`，清空 `TaskID`，让下一轮调度重新创建 task；如果超过次数，就写 `WorkflowFailed`。

当前还没有 backoff、jitter、retryable error 分类、retry budget 和 per-resource 限流。这个边界要诚实讲：LogServe 已经实现了“局部 step 重试 + 超时失败 + 最大次数”，但生产级 retry policy 还需要错误分类、退避、预算和副作用幂等。

## Q015. 重试会不会破坏 step 的幂等性？

**回答：**

重试本身不会破坏幂等性，但它会暴露 step 是否真的幂等。如果 step 只是纯计算，比如把输入 JSON 转成另一个 JSON，同样输入执行多次结果相同，那么重试问题不大。可一旦 step 有外部副作用，重试就可能造成重复写、重复扣款、重复发送通知、重复创建资源。

这里要区分两层幂等。

第一层是 workflow engine 内部幂等。比如同一个 task 完成回调重复到达，控制面不能写两次 `StepSucceeded` 或两次 `WorkflowCompleted`。这属于状态机幂等。

第二层是业务副作用幂等。比如 step 调了支付接口、发邮件、写第三方工单。即使 workflow engine 自己没有重复记账，外部系统也可能已经被调用了两次。这一层必须由 step 代码和外部系统一起保证。

面试时可以这样答：

```text
重试不会自动破坏幂等性，它只是把非幂等操作的问题放大。设计上要把“内部 completion 去重”和“外部副作用去重”分开。内部用 workflow_id、step_id、input_hash 去重；外部副作用要把同一个业务操作的幂等键传给下游系统，而且这个业务幂等键通常不能包含 attempt，否则每次重试都会被下游当成新操作。
```

这个细节很容易答错。对于 task 调度本身，attempt 应该进入 task idempotency key，因为每次 attempt 是一次新的执行尝试：

```text
workflow_id:step_id:input_hash:attempt:2
```

但对于外部业务操作，attempt 通常不该进入幂等键。比如扣款 step 的幂等键更应该是：

```text
charge:workflow_id:step_id:input_hash
```

如果写成：

```text
charge:workflow_id:step_id:input_hash:attempt:2
```

那第二次重试就可能真的扣第二笔钱。

LogServe 当前内部幂等做了几件事。`scheduleReadySteps` 创建 task 时使用 `workflowID + stepID + inputHash + attempt` 作为 task idempotency key；`StepSucceeded` 事件使用 `workflowID + stepID + LastInputHash + succeeded`；`WorkflowCompleted` 使用 `workflowID:completed`。集成测试也验证了重复 final step completion 不会写第二个 `WorkflowCompleted`。

但 LogServe 不能自动保证用户函数里的外部副作用幂等。尤其是 timeout 场景更危险：worker 认为 step 超时并回传失败，控制面可能发起重试，但原来的外部请求可能还在下游系统里继续执行。生产环境要靠外部幂等键、事务外盒、状态检查、补偿任务或强隔离执行器来兜住。

## Q016. workflow engine 如何处理 partial failure？

**回答：**

Partial failure 指 workflow 的一部分失败了，但其他部分可能已经成功、正在运行，或者还没被调度。DAG 系统里这很常见：一个分支查数据库失败，另一个分支已经把文件处理完了。

处理 partial failure，首先要定义失败策略：

```text
fail fast:
  某个 required step 最终失败后，不再调度新的 step。
continue on error:
  某些分支失败不影响其他分支继续跑完。
optional step:
  step 失败后标记 skipped/degraded，下游按 trigger rule 决定是否继续。
compensation:
  对已经成功但有副作用的 step 执行补偿动作。
manual intervention:
  暂停 workflow，等待人工修复输入或重试。
partial result:
  返回 degraded result，同时记录哪些分支失败。
```

Argo Workflows 的 DAG 默认是 fail fast：一个 task 失败后，不再调度新的 task，等已经运行的 task 结束后把 DAG 标成失败；如果配置 `failFast: false`，其他分支可以继续跑完。Temporal 则把错误处理放在 workflow 代码里，Activity 失败、取消、超时、panic 都能以不同错误类型暴露给 workflow 逻辑，由代码决定重试、补偿还是失败。

面试时可以这样答：

```text
workflow engine 处理 partial failure 的核心是把失败变成显式状态，而不是让它停在半路。每个 step 要有 terminal state，workflow 要有 fail-fast、continue-on-error、optional、compensation 等语义。已经成功的 step 不能丢，正在运行的 step 要么等它完成，要么发 cancel；还没调度的 step 要根据失败策略决定跳过还是继续。所有决定都应该写入事件日志，保证 crash 后能恢复到同一个结论。
```

Partial failure 还会牵涉结果一致性。比如 `A` 成功，`B` 失败，`C` 依赖 `A` 和 `B`。这时 `C` 不能假装 ready。除非 workflow 定义了 `B` 可选，或者 `C` 的 trigger rule 是 `none_failed_min_one_success` 这类条件，否则它应该一直不运行，直到 `B` 重试成功或 workflow 失败。

LogServe 当前采用的是比较严格的语义：

```text
1. step 失败后先重试当前 step。
2. 超过 MaxAttempts 后写 WorkflowFailed。
3. 下游只有在 depends_on 全部 SUCCEEDED 后才会调度。
4. 已经成功的 step 保留在 shared log 里，replay 后仍然可见。
5. 当前没有 optional step、skip、continue-on-error、compensation 和 cancel in-flight 语义。
```

这意味着 LogServe 更接近“required steps + fail workflow after retry exhaustion”的模型。它适合解释基本恢复路径，但还不是完整业务编排引擎。生产化时要补的不是一个 if 分支，而是一整套状态语义：`SKIPPED`、`CANCELED`、`UPSTREAM_FAILED`、`COMPENSATING`，以及这些状态如何影响下游 trigger rule。

## Q017. workflow engine 和 message queue 的职责边界是什么？

**回答：**

Message queue 负责消息传输，workflow engine 负责任务编排。两者经常一起出现，但职责边界不一样。

Message queue 主要解决：

```text
1. producer 和 consumer 解耦。
2. 消息持久化和投递。
3. 消费者扩缩容。
4. 分区、顺序、ack、redelivery。
5. dead letter queue 和基本重试。
```

Workflow engine 主要解决：

```text
1. 多 step 的依赖关系。
2. 每个 step 的状态机。
3. 上游结果如何传给下游。
4. retry、timeout、cancel、compensation。
5. workflow 级别的最终状态和最终结果。
6. crash 后如何 replay / recover。
7. 用户如何观察整个 workflow 的进度。
```

面试时可以这样答：

```text
Message queue 只知道消息和消费者，不知道这个消息属于哪个 DAG 节点，也不知道下游依赖什么时候满足。Workflow engine 知道 DAG、step 状态、结果引用、重试和终态。它可以用 message queue 作为底层执行队列，但不能把 queue 当成 workflow engine。否则你会很快遇到依赖判断、部分失败、重复执行、最终结果和可观测性这些问题。
```

举个例子，队列里有三个消息：

```text
embed
search
answer
```

Message queue 可以保证它们被投递给 worker，但它不知道 `search` 必须等 `embed` 的向量结果，`answer` 必须等 `search` 的文档结果。这个依赖判断属于 workflow engine。

反过来，workflow engine 也不应该自己包揽所有队列能力。高吞吐投递、consumer group、分区、长轮询、ack/redelivery 这些可以交给成熟队列或日志系统。否则引擎会把大量精力花在通用消息中间件问题上。

LogServe 的设计刚好能说明这个边界。它有 shared log、task metadata、worker polling，也有 workflow DAG runtime。对 workflow 来说，`scheduleReadySteps` 根据 step 依赖创建 task；对 worker 来说，它只是在拉取可执行 task。Task queue 不负责判断 DAG 依赖，workflow runtime 才负责这件事。

所以 LogServe 里可以这样划分：

```text
workflow engine:
  维护 wf:<workflow_id> 事件流，判断 ready step，解析 __step_ref__，写 StepScheduled/StepSucceeded/WorkflowCompleted。
task queue / worker layer:
  承接已经 ready 的 task，分配给 worker 执行，处理 task start/complete/fail。
shared log:
  作为状态真相和 replay 输入。
```

## Q018. workflow engine 和 BPMN 引擎有什么区别？

**回答：**

Workflow engine 是一个更宽的概念，BPMN 引擎是其中一类更偏业务流程建模的引擎。BPMN 是 OMG 标准，目标是用图形化符号描述业务流程，让业务人员、流程设计者和技术实现者能用同一套模型沟通。OMG 对 BPMN 2.0.2 的说明里也强调，它是业务流程图的事实标准，既给业务干系人使用，也要精确到可以转换成软件过程组件。

普通 workflow engine 不一定使用 BPMN。它可以用代码、YAML、Python DAG、Kubernetes CRD、JSON definition 或 SDK API 来定义 workflow。比如 Airflow 用 Python DAG，Argo 用 Kubernetes CRD，Temporal 用通用编程语言写 workflow definition，Dagster 把 data asset 依赖作为核心对象。

BPMN 引擎通常更关注：

```text
business process:
  审批、开户、理赔、采购、工单流转。
graphical notation:
  start event、end event、task、gateway、event、pool、lane。
human task:
  人工审批、表单、分派、SLA。
business events:
  timer、message、signal、error、compensation。
cross-organization collaboration:
  participant、message flow、泳道。
```

DAG / workflow engine 通常更关注：

```text
technical orchestration:
  数据处理、批任务、ML pipeline、微服务 activity 编排。
execution substrate:
  worker、container、pod、task queue、activity。
programmatic definition:
  代码或配置生成 workflow。
observability:
  step latency、retry、artifact、log、lineage。
```

面试时可以这样答：

```text
BPMN 引擎强调标准化业务流程建模，适合人参与、审批、网关、事件、补偿和跨组织流程；一般 workflow engine 更强调技术任务编排，可能用代码或配置表达 DAG，不一定有 BPMN 的图形语义。两者有重叠，Camunda 这类 BPMN 引擎当然也是 workflow engine，但 Airflow、Argo、Temporal、Dagster 这类系统的核心抽象并不是 BPMN 图。
```

一个判断方式是看用户是谁。

如果主要用户是业务分析师、流程经理、审批系统管理员，BPMN 的图形语义很有价值。流程图本身就是沟通材料。

如果主要用户是平台工程师、数据工程师、后端工程师，代码式或配置式 workflow 往往更直接。它更容易和测试、版本控制、CI/CD、资源调度、容器运行时结合。

LogServe 当前显然不是 BPMN 引擎。它没有 BPMN XML、gateway、human task、pool/lane、message event、compensation event。它是一个更小的技术型 workflow runtime：SDK trace 出 DAG definition，控制面根据 shared log 调度 ready steps，worker 执行 Python function，最终用 replay 校验状态。

## Q019. Temporal、Airflow、Argo Workflows、Dagster 的设计侧重点有什么差异？

**回答：**

这四个系统都能“跑 workflow”，但它们解决的问题不一样。面试里不要把它们讲成同类替代品。更准确的比较维度是：workflow 怎么定义，运行在哪里，状态怎么保存，失败怎么恢复，主要服务什么场景。

Temporal 的重点是 durable execution。Workflow 用通用语言写，外部副作用放到 Activity。Temporal 记录 Event History，worker crash 后通过 replay 恢复 workflow 状态。它适合长时间运行的业务流程、微服务编排、Saga、订单流、支付流、需要强恢复语义的后端流程。它的核心约束是 workflow code 必须 deterministic。

Airflow 的重点是批处理和数据 pipeline 调度。DAG 用 Python 定义，scheduler 负责把满足条件的 TaskInstance 放到 executor，同时考虑 pool、concurrency 等限制。它很适合定时 ETL、报表、数据仓库任务、批量依赖编排。Airflow 的优势是生态和调度能力，弱点是它不是为低延迟在线请求编排或极细粒度任务设计的。

Argo Workflows 的重点是 Kubernetes-native workflow。Workflow 是 Kubernetes CRD，step 通常是容器或模板，天然适合在 K8s 上跑 CI、ML pipeline、批处理、容器化任务。Argo 的 DAG 文档明确说 DAG 比纯 steps 序列更适合复杂 workflow，并允许最大并行度；它还支持 artifact、retryStrategy、failFast 等容器工作流常见能力。

Dagster 的重点是 data assets。它不只是“任务依赖图”，而是把数据资产、上游依赖、materialization、partition、asset check、I/O manager、观测性放在中心。Dagster 文档里 asset definition 包含 AssetKey、上游 asset keys，以及负责计算和存储资产内容的 Python function；job 是执行和监控一部分 asset graph 或 ops graph 的单位。它更像现代数据平台的资产编排和可观测层。

可以用一张表记：

```text
Temporal:
  侧重点：durable execution、事件历史、确定性 replay、Activity retry。
  适合：长业务流程、微服务编排、Saga、可靠后端状态机。

Airflow:
  侧重点：Python DAG、scheduler、executor、定时批处理、数据工程生态。
  适合：ETL、报表、数据仓库、周期性 pipeline。

Argo Workflows:
  侧重点：Kubernetes CRD、容器 step、artifact、K8s 原生调度。
  适合：CI、ML pipeline、容器化批任务、K8s 平台工作流。

Dagster:
  侧重点：data asset、lineage、partition、materialization、asset checks。
  适合：数据资产平台、数据质量、可观测数据 pipeline。
```

面试时可以这样答：

```text
Temporal 是 durable execution 引擎，关心代码式 workflow 如何可靠恢复；Airflow 是批数据 DAG 调度器，关心定时调度和任务编排；Argo 是 K8s 原生容器 workflow，关心 pod、artifact 和集群执行；Dagster 是 data asset orchestration，关心资产依赖、物化和数据可观测性。它们都能表达依赖，但核心对象不同，所以选型要看你是在编排业务状态机、批数据任务、容器任务，还是数据资产。
```

和这些成熟系统相比，LogServe 的定位更窄。它不是要替代 Temporal、Airflow、Argo 或 Dagster，而是把 shared log、DAG scheduling、retry、timeout、result_ref、replay、actor state 这些底层机制自己实现一遍，服务于项目展示和机制验证。按简历答辩的口径，应该说“我参考了成熟系统的语义边界，但 LogServe 的目标是证明自己理解这些机制，而不是宣称功能覆盖成熟平台”。

## Q020. workflow replay 为什么要求 deterministic？

**回答：**

Workflow replay 要求 deterministic，是因为 replay 的目标不是“重新做一遍外部世界”，而是用历史事件恢复出同一个 workflow 状态，并继续产生和过去兼容的调度决策。

以 Temporal 为例，workflow code 在 replay 时会重新执行，但外部副作用不会直接重做。历史里已经记录了 Activity completed、Timer fired、Signal received 等事件。Workflow code 必须在同样输入和同样历史下，发出同样顺序的 workflow API calls，也就是同样的 commands。如果 replay 时因为随机数、当前时间、map 遍历顺序、数据库查询、LLM 调用结果不同，代码走到另一条分支，就会出现历史事件和新 command 对不上。

一个简单例子：

```text
if random() < 0.5:
  schedule ActivityA
else:
  schedule ActivityB
```

第一次运行调度了 `ActivityA`，历史里也记录了 `ActivityA`。worker crash 后 replay，如果 `random()` 这次走到 `ActivityB`，引擎就没法把历史里的 `ActivityA` 对到当前代码路径。这就是 nondeterminism。

所以 Temporal 文档要求 workflow definition deterministic，并建议把 API 调用、LLM/AI 调用、数据库查询等非确定性外部交互放到 Activity。Activity 的结果进入 event history，replay 时读历史结果，不重新猜。

面试时可以这样答：

```text
Replay 要 deterministic，是为了保证同一段历史可以还原出同一个状态和同一组后续 commands。Workflow 代码里不能直接依赖 wall clock、random、外部 API、数据库查询、线程竞争顺序这类不稳定输入；这些要放到 activity 或 step 里，让结果作为事件记录下来。否则 crash recovery 后，调度器可能重复调度、漏调度，或者和历史事件对不上。
```

常见 nondeterministic 来源包括：

```text
time:
  直接读当前时间，而不是用 workflow 提供的 deterministic time。
random:
  直接生成随机数，而不是记录随机结果。
external I/O:
  workflow 编排代码里直接查数据库、调 HTTP、调用 LLM。
concurrency race:
  多线程竞争导致 command 顺序不稳定。
unordered iteration:
  遍历 map/set 产生不稳定顺序，然后据此调度 activity。
code change:
  老 workflow 的 history 用新代码 replay，新代码改变了 command 序列。
```

LogServe 当前的 replay 模型和 Temporal 不完全一样。LogServe 的 `workflow.Replay` 不是重新执行用户的 Python workflow function，而是把 `wf:<workflow_id>` 事件流当作输入，按事件类型做 reducer：`WorkflowStarted` 创建状态，`StepScheduled` 更新 task id 和 attempt，`StepSucceeded` 写 result，`WorkflowCompleted` 标终态。这个 replay 过程要求 reducer deterministic，也要求事件 schema 稳定。

这带来一个边界：LogServe 当前可以允许 step 内部函数本身非确定性，因为 replay 不会重新跑 step code，它只读已经写入日志的结果。但调度决策仍然要可恢复：同一个事件流 reducer 出来的状态必须和 metadata 一致，`ReplayWorkflow` 的集成测试也在验证这一点。

如果未来 LogServe 要支持像 Temporal 那样“重新执行 workflow 编排代码来生成 commands”，那 deterministic 要求就会更严格。到那时，SDK trace、DAG 生成、条件分支、动态 step 创建都必须保证同一份历史下产生同样的命令序列。

## Q021. deterministic replay 中系统时间、随机数、外部 I/O 如何处理？

**回答：**

Deterministic replay 的基本原则是：workflow 编排逻辑不能直接依赖不可重放的东西。系统时间、随机数、数据库查询、HTTP 调用、LLM 调用、文件系统状态，这些东西每次运行都可能变。它们如果直接决定 workflow 下一步调度什么，就会让 replay 走到另一条路。

Temporal 文档把这个问题讲得很直接：workflow code 必须在相同输入下产生相同顺序的 Workflow API calls。对 API 调用、数据库查询、LLM/AI 调用这类非确定性交互，应该放到 Activity 里；Activity 结果写进 Event History，replay 时读历史，不重新调外部世界。

处理方式可以分成几类。

```text
系统时间:
  不直接调用 time.Now() / datetime.now() 来决定分支。
  用 workflow runtime 提供的 deterministic clock / timer API。
  时间事件进入 history，replay 时看到同一个 timer fired 结果。

随机数:
  不直接调用普通 random。
  用 runtime 提供的 replay-safe random，或用 SideEffect 把随机结果记录进 history。
  replay 时返回历史里的随机值。

外部 I/O:
  不在 workflow 编排代码里直接查数据库、调 HTTP、读文件、调用 LLM。
  封装成 activity / step。
  activity 的输入、输出、失败、重试状态进入 history。

配置读取:
  如果配置会变化，要么作为 workflow 启动参数固定下来，
  要么用 mutable side effect 记录版本化配置值。

日志:
  replay 时可能重复执行编排代码，普通日志容易重复打印。
  成熟 SDK 通常提供 replay-aware logger。
```

面试时可以这样答：

```text
Deterministic replay 不是禁止时间、随机数和 I/O，而是禁止它们直接进入 replay 路径。时间要通过 workflow timer，随机数要通过 SideEffect 或 replay-safe random，外部 I/O 要封成 activity。这样第一次执行时把结果写进 history，后续 replay 读同一个结果。否则同一个 workflow history 可能在恢复时走出不同 command 序列。
```

Temporal 的 SideEffect 文档给了一个典型例子：生成 UUID 或随机数这类非确定性值，可以用 SideEffect 执行一次，并把结果存到 Workflow Event History。Replay 时 SideEffect 不再执行函数，而是返回历史里的值。但 SideEffect 也有边界：它应该返回一个值，不应该在 SideEffect 函数里直接修改 workflow state，因为 replay 时那个函数不会重新执行。

LogServe 当前要分清两层。

第一层是 step 内部执行。LogServe 的 `workflow.Replay` 不重新执行 Python step function，所以 step 里用系统时间、随机数或外部 I/O，不会影响当前的 workflow reducer replay。只要 step 成功后结果已经写成 `StepSucceeded` 事件，replay 读的就是事件里的 `ResultJSON` 或 `ResultRef`。

第二层是调度和编排语义。LogServe 现在的调度决策来自 `Definition.Steps`、`DependsOn`、step status 和 `ResolveArgs`，不是重新跑一段用户 workflow code。所以它还没有 Temporal 那种“编排代码 replay 必须 deterministic”的完整约束。但 `workflow.Replay` 这个 reducer 本身必须 deterministic：同一串 `WorkflowStarted`、`StepScheduled`、`StepSucceeded`、`WorkflowCompleted` 事件，必须还原出同一个状态。

如果未来 LogServe 支持运行中的动态 DAG 生成，比如根据某个 step 输出继续生成新的 step，那么时间、随机数和外部 I/O 就必须被事件化。否则恢复后 SDK 重新 trace 一遍，可能生成一套不同的 DAG。

## Q022. workflow history 膨胀后如何压缩？

**回答：**

Workflow history 膨胀是长时间运行 workflow 一定会遇到的问题。每个 step schedule、start、complete、retry、timer、signal、cancel、result reference 都可能变成事件。一个 workflow 跑几小时还好，如果跑几个月，或者 fan-out 几十万个子任务，history 会拖慢 replay、查询、存储和 UI 展示。

常见处理方式有五类。

```text
1. result 外部化:
   大结果不要塞进 history，只保存 result_ref、checksum、size、content type。

2. snapshot / checkpoint:
   每隔一段时间保存当前 materialized state。
   恢复时从 snapshot 开始，再 replay snapshot 之后的事件。

3. continue-as-new:
   关闭当前 run，用同一个 workflow id 链接到一个新的 run。
   新 run 带上必要状态，但拥有新的 history。

4. history compaction:
   把多条中间事件折叠成一条摘要事件。
   例如多个 heartbeat 或进度事件只保留最后状态。

5. offload:
   把过大的 node status、参数或 payload 放到外部数据库或对象存储。
   主状态只保留引用。
```

Temporal 的 Continue-As-New 就是典型做法：当前 Workflow Execution 成功关闭，然后创建同一条 chain 里的新 Execution，Workflow ID 保持，Run ID 变新，Event History 重新开始。它适合 history 接近限制、循环处理、长期监听信号、批量消费队列这类场景。

Argo Workflows 也有类似问题。它把 workflow 存成 Kubernetes resource，节点状态在 `/status/nodes` 里，资源大小会受 etcd 限制。Argo 的大 workflow offload 文档说明：先压缩 node status，如果仍然太大，就把 node status 存到 SQL 数据库；另外还建议用 `withItems` / `withParam`、workflow templates、workflow-of-workflows 拆小。

面试时可以这样答：

```text
history 压缩不能随便删事件，因为 replay 和审计依赖 history。正确做法是把“恢复需要的状态”和“审计需要的明细”分层：恢复路径可以用 snapshot、continue-as-new、result_ref、状态摘要来减小 replay 成本；审计明细可以归档到冷存储。压缩时必须有边界，比如 terminal event、幂等键、结果引用、版本 marker 不能丢。
```

LogServe 当前已经有一部分基础。

```text
已有:
  - workflow 结果大于阈值且配置 resultStore 时写 result_ref。
  - workflow.Replay 可以从 wf:<workflow_id> 事件流恢复状态。
  - ReplayWorkflow 可以比较 replay state 和 metadata。
  - actor runtime 有 snapshot 和 logical trim 思路。

缺口:
  - workflow 还没有 snapshot。
  - workflow 还没有 continue-as-new。
  - workflow stream 还没有 physical compaction。
  - fan-out 很大时，每个 StepScheduled/StepSucceeded 仍会进入 wf stream。
```

如果给 LogServe 补生产级 history 压缩，我会先做 workflow snapshot，而不是直接删日志。比如每 N 个 workflow events 或每 M 秒写 `WorkflowSnapshotCreated`，记录 step 状态、attempt、task id、result refs、workflow status。恢复时先找最后一个 snapshot，再 replay 后续事件。等这个可靠以后，再把老事件做归档或逻辑 trim。

## Q023. workflow versioning 如何支持长时间运行任务？

**回答：**

长时间运行 workflow 最怕一个问题：老 workflow 还没跑完，新代码已经部署了。新代码如果改变了编排顺序、activity 名称、timer 位置、分支条件，老 history 用新代码 replay 时可能直接 nondeterminism。

Temporal 文档把这个场景说得很典型：Workflow Execution 可能运行几个月甚至几年，所以必须允许 Workflow Definition 在已有 execution 还在跑的时候演进。它给了两类主要方法：Worker Versioning 和 patching。Worker Versioning 让不同版本的 worker 运行不同代码路径；patching 则在 workflow code 里加版本分支，让老 execution 走老逻辑，新 execution 走新逻辑。

一个版本化方案通常包括这些层：

```text
workflow definition version:
  workflow 编排逻辑的版本。影响 DAG、分支、timer、activity 顺序。

activity / step type version:
  某个 step 函数或 activity 的版本。影响业务行为和参数协议。

payload schema version:
  input/output JSON 的版本。影响序列化和兼容性。

worker version:
  哪些 worker 能处理哪些 workflow / step version。

data converter version:
  历史 payload 如何反序列化，老数据能不能被新代码读懂。
```

面试时可以这样答：

```text
长时间运行 workflow 的 versioning 目标是让老 run 继续用兼容的逻辑跑完，新 run 可以用新逻辑开始。不能直接把 worker 全部升级，然后要求老 history 适配新 command 序列。常见做法是 worker version pinning、代码 patch marker、workflow definition version、activity version 和 payload schema version。版本信息本身也要进 history，否则 replay 时不知道当时选择的是哪条分支。
```

有几条工程规则很实用。

第一，能兼容就兼容。新增字段要给默认值，删除字段要保留一段时间，输出 schema 尽量向后兼容。

第二，改变 command 序列要显式 version。比如把 `ActivityA` 换成 `ActivityC`，或者在两个 activity 中间插入 timer，这都可能改变 replay command 顺序，必须 patch。

第三，长任务要能 drain。保留老 worker 一段时间，让老 run 自然跑完；或者把老 run 迁移到新版本，但迁移动作本身也要有事件。

LogServe 当前有一个有利点：workflow definition 里每个 `StepDefinition` 保存了 `FunctionSource`，调度时 `TaskSpec` 会带上这段 source。也就是说，一个已提交 workflow 的 step 代码可以随 definition 固定下来，而不是运行时去某个全局 registry 取最新函数。这对版本稳定很有帮助。

但 LogServe 还没有完整 versioning 体系。比如 `Definition` 没有显式 schema version，控制面调度语义没有 patch marker，worker 也没有按 workflow version 做路由。现在适合回答成：项目通过把 function source 固化进 workflow definition，避免了一部分“部署后函数变了”的问题；如果要支持长期运行任务，还需要 workflow definition version、payload schema version、worker capability/version、以及可 replay 的 migration event。

## Q024. DAG 动态生成和静态定义的 trade-off 是什么？

**回答：**

静态 DAG 是在 workflow 提交或解析阶段就知道所有节点和边。动态 DAG 是运行过程中根据上游结果、外部输入或代码逻辑生成后续节点。两者没有绝对好坏，取决于场景。

静态定义的优点很直接：

```text
可校验:
  提交前能检查环、未知依赖、权限、资源需求。

可观察:
  UI 可以提前画出完整 DAG。

可估算:
  可以计算最大并行度、critical path、资源预算。

可审计:
  运行前 definition 已经固定，容易复现。

可控:
  不容易在运行时突然生成百万级任务。
```

静态定义的问题是表达力有限。比如“先扫描一个目录，然后对里面每个文件跑一个任务”，文件数量只有运行时才知道。硬把它写成静态 DAG，要么提前展开一个巨大图，要么把真正的 fan-out 藏在一个大 step 里，调度器看不到内部并行度。

动态生成的优点是灵活：

```text
1. 根据上游输出决定下游数量。
2. 支持 map / fan-out。
3. 支持条件分支和递归。
4. 避免提交时构造超大 DAG。
5. 可以按数据规模自然扩缩。
```

但动态 DAG 的代价也明显：更难做提交前校验，更难估算资源，更容易产生调度风暴，也更容易破坏 deterministic replay。Airflow Dynamic Task Mapping 的设计就是折中：DAG 文件里声明一个 mapped task，真正有多少个 task instances，要等上游结果出来后由 scheduler 展开。Argo 的 `withParam` 也类似，可以让一个 step 输出 JSON 数组，再让下一个模板按这个数组迭代。

面试时可以这样答：

```text
静态 DAG 胜在可校验、可视化、可估算；动态 DAG 胜在表达运行时数据规模。生产系统通常不会完全二选一，而是用静态骨架加受控动态展开。比如 definition 里声明一个 mapped step，运行时根据上游结果展开 N 个子任务，但 N 要有上限，展开动作要进 history，展开后的每个子任务要有稳定 id 和幂等键。
```

LogServe 当前更接近“提交时固定 DAG”。README 里说 Python SDK trace `@task` 调用形成 DAG，Go 控制面只调度 ready steps；一旦 `Definition.Steps` 提交，控制面就按这个定义扫描。它还没有运行中新增 step 的机制，也没有 mapped step 抽象。

这对项目现阶段是合理的：静态 DAG 更容易展示调度、重试、replay、result_ref 这些基础机制。生产化时可以加一个受控动态层，例如：

```text
MappedStep:
  upstream step 输出一个 JSON array。
  控制面生成 child step ids: map_step[0], map_step[1], ...
  展开数量受 max_fanout 限制。
  每个 child 的 input hash 和 idempotency key 稳定。
  fan-in step 读取所有 child result_ref。
  expansion event 写入 wf log，replay 时不重新猜数量。
```

关键点是最后一句：动态展开的结果必须事件化。不能 crash 后重新读一遍外部目录，然后发现文件数变了。

## Q025. fan-out/fan-in 模式如何处理大量子任务？

**回答：**

Fan-out/fan-in 是 workflow 里最常见的并行模式。Fan-out 把一批输入拆成很多子任务并行执行，fan-in 等这些子任务完成后做聚合。比如给 10 万个文件提特征，再汇总成一个索引。

处理大量子任务，最怕两个极端：一口气把所有子任务都展开，压垮 scheduler；或者把所有工作塞进一个大 step，调度器完全看不见内部进度。比较稳的做法是“逻辑上 fan-out，物理上分批展开和执行”。

设计时要考虑这些点：

```text
stable child id:
  每个子任务要有稳定 id，比如 shard-000001。
  重试和恢复时不能生成另一批 id。

bounded expansion:
  fan-out 数量要有上限，或者分页展开。
  避免一次生成百万条 task metadata。

concurrency limit:
  同时运行的子任务要受 worker 数、队列水位、外部服务配额限制。

chunking:
  一个子任务处理一小批 item，而不是一个 item 一个 task。
  chunk 大小要在调度开销和 straggler 风险之间折中。

result externalization:
  子任务输出尽量保存 result_ref，不要把大数组塞进 workflow history。

tree reduce:
  fan-in 不一定是一个巨大的 reduce step。
  可以分层聚合，先局部 reduce，再全局 reduce。

partial retry:
  某个 shard 失败只重试这个 shard，不重跑整批。

progress tracking:
  暴露 total、completed、failed、running、queued。
```

Airflow Dynamic Task Mapping 里有一个很实用的细节：mapped task 的聚合输出不是普通 list，而是 lazy proxy，需要时再取每个 mapped instance 的结果。文档也提醒，强行 `list(values)` 会有性能影响。这个点说明 fan-in 不能总是假设所有子结果都能一次性拉进内存。

Argo 的 loops 提供 `withSequence`、`withItems`、`withParam`。其中 `withParam` 可以来自上游 step 输出，因此适合运行时 fan-out。Argo 文档也说明 loop 的全部迭代结果可以作为 JSON array 访问，但每个迭代输出必须是合法 JSON。这个对大规模任务是提醒：聚合结果格式必须受控。

面试时可以这样答：

```text
大量 fan-out/fan-in 要把“任务数量”和“调度压力”分开控制。我会用稳定 child id、分批展开、并发上限、chunking、result_ref 和 tree reduce。不要把 10 万个结果内联进一个 workflow state，也不要让一个 workflow 一次性占满全局 worker。fan-in 侧要支持流式或分层聚合，只重试失败 shard。
```

LogServe 当前没有专门的 mapped step。用户可以在 workflow definition 里手动生成很多 step，但这会放大 `wf:<workflow_id>` 事件流、metadata 和 ready 扫描成本。更适合的演进方向是加 `MappedStep`：

```text
map source:
  上游 step 输出 items_ref。

expansion:
  控制面写 MapExpanded 事件，记录 item count、chunk size、child ids。

execution:
  每个 child 是普通 step，有独立 attempt、task_id、result_ref。

fan-in:
  reduce step 读取 child result refs，而不是读取一个巨大内联数组。

limits:
  max_fanout、max_running_children、max_result_bytes。
```

这能把 fan-out 变成 workflow engine 一等语义，而不是用户代码里的隐藏循环。

## Q026. map-reduce 型 workflow 如何处理 straggler？

**回答：**

Straggler 是拖慢整批任务的慢节点或慢分片。Map-reduce 型 workflow 里，reduce 往往要等所有 map shard 完成；如果 999 个 shard 都结束了，最后 1 个 shard 卡住，整个 workflow latency 还是被它拖住。

Google MapReduce 论文里提到，运行时系统会处理分区、调度、机器失败和机器间通信，并在大规模集群上执行任务。MapReduce 这类系统后来最经典的 straggler 手段之一就是 backup / speculative execution：当某个任务明显落后时，在别的 worker 上启动一个副本，谁先完成就采用谁的结果，另一个副本取消或忽略。

处理 straggler 通常有几类方法。

```text
小分片:
  把任务切得更均匀，减少单个 shard 太大的概率。
  但不能小到调度开销超过业务开销。

动态分片:
  worker 从 work queue 里持续取小块，而不是固定分配大块。
  慢 worker 少拿任务，快 worker 多拿任务。

speculative execution:
  检测到 shard 明显慢于 p95 / expected duration 时，启动备份副本。
  采用第一个成功结果，另一个副本通过幂等键和 fencing 防止重复提交。

timeout and retry:
  超过 deadline 的 shard 失败重试。
  适合卡死，不一定适合只是慢。

data locality:
  把 shard 放到靠近数据的位置跑，减少远程读导致的慢尾。

tree reduce:
  reduce 分层，避免所有结果都压到一个最终 reducer。

partial aggregation:
  map 侧先 combine，减少 shuffle 和 reduce 压力。
```

面试时可以这样答：

```text
Straggler 会把 map-reduce 型 workflow 的尾延迟拉高，因为 fan-in 往往等最慢 shard。处理办法不是盲目加 worker，而是先把 shard 切均匀，暴露每个 shard 的进度和耗时，再对异常慢 shard 做 speculative execution 或 timeout retry。Speculative execution 必须配幂等和 fencing：两个副本可能同时完成，但只能有一个结果被接受。
```

Speculative execution 的难点在“什么时候复制”。太早复制会浪费资源，太晚复制没有收益。一个常见策略是：

```text
if shard runtime > max(p95_expected, median_runtime * factor)
and remaining_time_estimate is high
and spare capacity exists:
  launch backup attempt
```

然后用 `attempt_id`、`lease_epoch`、`input_hash`、`result_key` 控制只接收一个终态。外部副作用型任务不适合随便 speculative，因为两个副本可能真的写两次外部系统。纯计算、读多写少、输出可按 key 覆盖的任务更适合。

LogServe 当前的 worker 层有任务 lease epoch 和 redelivery 机制，stale completion 会被拒绝；workflow step 有 `attempt` 和 `LastInputHash`。这些机制对“失败后重试”和“worker loss 后恢复”有帮助，但还不等同于 speculative execution。现在一个 step 同一时间只绑定一个 `TaskID`，没有“同一个 shard 多个并行 attempt，最快成功者获胜”的模型。

如果要支持 straggler mitigation，可以加：

```text
1. step progress heartbeat。
2. expected duration / p95 duration 统计。
3. backup attempt 状态。
4. result winner fencing。
5. loser attempt cancellation。
6. speculative execution 只允许在 pure / idempotent step 上开启。
```

否则在有副作用 step 上复制执行，优化的是延迟，牺牲的是正确性。

## Q027. workflow step 的 side effect 应该如何抽象？

**回答：**

Workflow step 的 side effect 指会改变外部世界的操作，比如写数据库、扣款、发邮件、创建云资源、调用第三方 API、写对象存储。它和纯计算不同：纯计算失败重试通常只是浪费 CPU，side effect 失败重试可能造成重复副作用。

最安全的抽象是：workflow 编排层只描述“要执行一个副作用动作”，真正的副作用放到 step / activity 里，并且这个 step 有明确的幂等键、输入 hash、重试策略、超时、补偿逻辑和结果记录。

一个 side-effect step 至少要有这些字段或语义：

```text
operation identity:
  这次外部动作的业务 id，比如 charge_id、email_id、provision_request_id。

idempotency key:
  下游系统能识别重复请求。
  通常由 workflow_id + step_id + business_key + input_hash 组成。

effect type:
  pure、idempotent-write、non-idempotent、compensatable。

retry policy:
  哪些错误可重试，哪些错误必须停止。

timeout and cancellation:
  超时后外部请求是否还可能成功，如何确认。

compensation:
  如果后续 workflow 失败，是否需要撤销、退款、删除资源。

result contract:
  外部系统返回的 receipt / confirmation id 必须写入 history。
```

Temporal 的 Activity 抽象就是这个边界：Activity 是普通函数，可以执行非确定性工作，比如调用另一个服务、转码文件、发邮件；Temporal 文档也建议 Activity 保持幂等。SideEffect 则更适合记录 workflow 内部很小的非确定性值，比如随机数，不适合承载复杂外部 I/O。

面试时可以这样答：

```text
我会把 side effect 从 workflow 编排代码里拿出去，封成 activity 或 step。编排层只调度和等待结果，side effect step 自己带幂等键、超时、retry policy 和补偿语义。成功以后要把外部 receipt 写进 history。这样 replay 不会重新执行副作用，重试也有机会被下游幂等挡住。
```

这里最容易踩坑的是超时。Workflow engine 看到 step timeout，不代表外部世界没有成功。比如支付请求超时，可能是本地没收到响应，但支付网关已经扣款。这个时候不能简单重试一个新的业务请求，而应该先用同一个 idempotency key 查询外部状态。

LogServe 当前的 step 是 Python function，控制面给每个 task 传 `IdempotencyKey`，并在内部用 `workflowID:stepID:inputHash:attempt` 做 task 幂等。这个内部幂等能保证 task 和 workflow 状态不被重复 completion 搞乱，但它不能自动保证用户函数调用的外部系统幂等。

如果要把 LogServe 的 side effect 抽象补强，可以加：

```text
StepDefinition.effect_type:
  pure / idempotent / compensatable / unsafe

StepDefinition.business_idempotency_key:
  传给用户函数和外部系统，不包含 attempt。

StepSucceeded.receipt:
  保存外部确认号。

StepCompensated:
  补偿事件，记录撤销结果。

retry guard:
  unsafe step 默认不自动重试，除非显式声明幂等。
```

这样面试时能把“会重试”讲成“有边界地重试”，而不是把副作用风险盖过去。

## Q028. 如何保证 workflow 状态和 task 状态一致？

**回答：**

Workflow 状态和 task 状态一致，本质上是两个状态机之间的一致性问题。Workflow step 说“我已经调度了 task-1”，task 表里就必须有 task-1；task-1 成功了，workflow step 才能变成 succeeded；如果 task completion 重复到达，workflow 不能重复完成；如果 worker 拿的是旧 lease，不能覆盖新 attempt。

一个稳妥设计通常有几条规则。

```text
single source of truth:
  事件日志或事务数据库必须有一个权威状态来源。
  metadata 只是 materialized view 时，要能从日志重放恢复。

log-before-state:
  先写不可变事件，再更新可变 view。
  避免 view 写了但 crash 后日志没有证据。

idempotency:
  StepScheduled、StepSucceeded、WorkflowCompleted 都要有幂等键。
  重复请求只产生一个有效终态。

lease / fencing:
  worker 完成 task 时必须带 lease epoch。
  旧 worker 的 stale completion 被拒绝。

state transition validation:
  QUEUED -> RUNNING -> SUCCEEDED/FAILED。
  workflow step 也要有合法状态转换。

atomic boundary:
  task terminal 和 workflow step terminal 最好在一个事务里更新。
  如果不能同事务，就要靠 log replay 修复。

reconciliation:
  后台定期扫描 task 和 workflow step，发现不一致就按日志或终态规则修正。
```

面试时可以这样答：

```text
我会先定义谁是 source of truth。然后所有 task 和 workflow step 的状态变化都写事件，metadata 只是投影。调度时先创建 task，再写 StepScheduled 并把 task_id 写回 step；完成时校验 lease epoch，先让 task 进入终态，再用幂等 StepSucceeded/StepFailed 推进 workflow。重复 completion、stale completion、crash recovery 都必须能通过幂等键和 replay 收敛。
```

LogServe 当前就是比较典型的 log-first + metadata 投影方案。`enqueueTask` 先写 `TaskSubmitted` 日志，再创建 task metadata 并把 task id 放进内存队列。`scheduleReadySteps` 成功 enqueue 后写 `StepScheduled` 到 `wf:<workflow_id>`，再更新 workflow step 的 `TaskID`、`Attempts`、`LastInputHash`。Worker 执行时写 `TaskStarted`，再调用 `StartTask`，控制面写 `StepStarted`。

完成路径也有防护。`CompleteTask` 要求 terminal status，只接受 `SUCCEEDED` 或 `FAILED`。metadata 层用 worker id 和 `TaskLeaseEpoch` 校验 lease，stale task completion 会被拒绝。对于 workflow step，`completeWorkflowStep` 如果发现 step 已经 `SUCCEEDED`，重复完成会 no-op。`WorkflowCompleted` 也用 `workflowID:completed` 做幂等键。

恢复路径同样重要。`bootstrapWorkflows` 会读 `wf:` stream，用 `workflow.Replay` 重建状态，再 `UpsertWorkflow` 到 metadata；如果 workflow 还在 running，会 restore workflow tasks 并继续 `scheduleReadySteps`。`ReplayWorkflow` 接口还会比较 replay state 和当前 metadata 是否一致。

LogServe 当前的边界也要说清楚：控制面 metadata 目前有内存和 Postgres adapter，但很多一致性靠单进程锁、日志幂等和 bootstrap replay 来证明；不是一个多节点强事务调度器。生产化时需要把 task metadata、workflow step metadata 和 log append 的事务边界做得更硬，或者引入可靠 outbox / transactional log。

## Q029. workflow 的可观测性应该暴露哪些维度？

**回答：**

Workflow 的可观测性不能只看“成功几个、失败几个”。面试里最好按四层讲：workflow 层、scheduler 层、task/worker 层、dependency/result 层。OpenTelemetry 也把观测信号分成 traces、metrics、logs、baggage、profiles 等，workflow engine 至少要把 metrics、logs、traces 这三类打通。

Workflow 层要看：

```text
workflow_started_total
workflow_completed_total
workflow_failed_total
workflow_canceled_total
workflow_latency_ms
workflow_status_age_ms
workflow_replay_latency_ms
workflow_history_events
workflow_result_size_bytes
```

Scheduler 层要看：

```text
ready_nodes
scheduled_nodes_total
scheduler_loop_latency_ms
scheduler_lag_ms
queue_backlog
queue_wait_ms
admission_rejected_total
backpressure_active
critical_path_ms
scheduler_overhead_ms
```

Task 和 worker 层要看：

```text
task_attempts
task_execution_latency_ms
task_queue_wait_ms
task_timeout_total
task_retry_total
worker_active_tasks
worker_capacity
worker_heartbeat_lag_ms
lease_expired_total
stale_completion_rejected_total
```

Dependency 和 result 层要看：

```text
step_blocked_by_dependency_count
fanout_children_total
fanin_waiting_children
result_inline_bytes
result_ref_count
result_store_put_latency_ms
result_store_get_latency_ms
input_resolution_latency_ms
```

面试时可以这样答：

```text
我会让 workflow 可观测性回答三个问题：为什么没开始、为什么没结束、为什么变慢。没开始要看 ready node、队列水位、worker 容量和 admission reject；没结束要看依赖阻塞、失败重试、fan-in 等待和 straggler；变慢要拆成 queue wait、scheduler overhead、input materialization、execution latency、result store latency 和 retry/backoff。
```

Trace 也很重要。一个 workflow run 应该有一个 trace id，step、task、worker execution、result store 操作都挂在下面。这样用户看到 workflow 慢，不需要在控制面日志、worker 日志、对象存储日志之间手工拼时间线。

LogServe 当前已经有一些观测点。README 里提到 `TaskStarted` 包含 `local_queue_wait_ms`；worker 和控制面会通过 `observability.Info/Error` 打出 `workflow_step_scheduled`、`workflow_step_succeeded`、`workflow_step_retrying`、`workflow_completed`、`task_execution_failed`、`worker_poll_failed` 等结构化日志。Workflow event payload 里有 `TimestampMs`、`LatencyMs`、`Attempt`、`InputHash`、`ResultRef`。

还可以补几项更面向 scheduler 的指标：

```text
ready_width:
  每轮扫描有多少 step 满足依赖。

blocked_steps:
  因哪些 depends_on 未完成而阻塞。

enqueue_failure_total:
  ready 了但被 queue high watermark 或 log append slow 拒绝。

replay_consistency:
  ReplayWorkflow 与 metadata 是否一致。

critical_path_estimate:
  基于 step latency 和 DAG 依赖算出的最长路径。
```

这些指标能直接服务面试中的“你怎么判断系统瓶颈”的问题。

## Q030. 如何压测 workflow scheduler？

**回答：**

压测 workflow scheduler 不能只提交几个简单 DAG 看能不能跑完。Scheduler 的压力来自 ready node 释放、队列写入、metadata 更新、日志写入、worker completion 回调、重试风暴、fan-out/fan-in、恢复 replay。压测要把这些维度分开打。

我会先定义几类 DAG 模型：

```text
chain:
  A -> B -> C -> ...
  测 critical path 和调度循环开销。

wide fan-out:
  A -> 10000 个并行 child -> Z
  测 ready burst、队列水位、metadata 写入。

diamond:
  fan-out 后 fan-in。
  测 join、result resolution、straggler。

random DAG:
  N 个节点、可控 edge probability。
  测一般拓扑调度和环校验。

retry storm:
  大量 step 同时失败并重试。
  测 backoff、retry budget、队列保护。

large result:
  子任务产生大结果。
  测 result_ref、result store、input resolution。

recovery:
  scheduler/worker 中途 crash。
  测 replay、redelivery、stale completion。
```

压测指标要分层收集：

```text
throughput:
  workflows/sec、steps/sec、task completions/sec。

latency:
  submit -> first step scheduled
  ready -> task enqueued
  task queued -> started
  started -> terminal
  final step succeeded -> workflow completed

resource:
  CPU、内存、goroutine、锁等待、GC、DB 连接、日志 append latency。

correctness:
  no duplicate terminal event
  no lost step
  no step runs before dependency succeeds
  replay state equals metadata
  stale completion rejected

backpressure:
  queue high watermark hit rate
  admission reject count
  retry storm 下是否保护 log store 和 worker。
```

面试时可以这样答：

```text
我会把 scheduler 压测分成性能和正确性两条线。性能上构造 chain、wide fan-out、diamond、random DAG、retry storm，测 steps/sec、ready-to-enqueue latency、queue wait、workflow latency、CPU 和锁竞争。正确性上每次压测后 replay 所有 workflow，确认 metadata 和 log 一致，确认没有依赖未满足就运行、没有重复 terminal event、没有丢 step。
```

还要做故障注入。比如：

```text
1. enqueueTask 写 TaskSubmitted 后进程 crash。
2. StepScheduled 写入失败。
3. worker poll 后未 StartTask 就崩。
4. worker StartTask 后执行超时。
5. CompleteTask 重复发送。
6. 旧 lease worker 延迟回传 completion。
7. result store Put 成功但后续 StepSucceeded 写失败。
8. 控制面重启后 bootstrapWorkflows。
```

这些场景比单纯吞吐更能证明 scheduler 可靠性。

LogServe 当前已有一些基础测试和机制可以用来扩展压测。集成测试覆盖了 workflow replay 一致性、重复 final completion 不产生第二个 `WorkflowCompleted`、worker recovery 后不重跑已成功 step、失败 step 重试、timeout step 重试后失败。控制面也有 backpressure 配置、redelivery timeout、stale task lease rejection。

如果专门给 LogServe 写 scheduler benchmark，我会做一个生成器：

```text
generate_workflow(shape, node_count, fanout, failure_rate, result_size)
run_with_workers(worker_count, local_queue_size, executor_pool_size)
collect:
  workflow_latency_ms
  step_latency_ms
  queue_wait_ms
  scheduler_overhead_ms
  replay_latency_ms
  duplicate_event_count
  consistency_failures
```

最后用 replay 做收尾验证。吞吐高但 replay 不一致，不能算 scheduler 压测通过。

## Q031. topological sort 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

Topological sort 的核心目标是：在一个有向无环依赖图里，找出一种不违反依赖关系的执行顺序。换句话说，如果有边 `A -> B` 表示 `B` 依赖 `A`，那么拓扑序里 `A` 一定要排在 `B` 前面。

所以它首先解决的是正确性问题。它保证“依赖还没完成，下游不能先跑”。在 workflow engine、构建系统、包管理器、数据库迁移、编译器 pass 里，这都是硬约束，不是优化项。

它也间接影响性能和可维护性：

```text
正确性:
  不让下游 step 在上游结果不存在时运行。
  提交或准备阶段发现 cycle，避免运行时卡死。

性能:
  找出所有 ready nodes，释放并行度。
  但它不自动给出最优调度，也不处理资源竞争。

可维护性:
  把依赖关系显式化，便于画 DAG、调试、审计。
  比手写 if/else 顺序更容易解释。

安全性:
  它本身不是安全机制。
  但依赖顺序错误可能造成危险副作用，比如先删除再备份、先扣款再校验。
```

面试时可以这样答：

```text
topological sort 的主目标是 correctness：保证每个节点只在所有前置依赖之前的节点排好之后才出现。它顺带提供 ready set，所以能帮助调度器利用并行度；也能提升可维护性，因为依赖关系从隐式代码顺序变成显式图。但它不是完整 scheduler，不负责 worker 选择、限流、公平性、资源隔离和失败恢复。
```

Python 标准库 `graphlib.TopologicalSorter` 的文档也很贴合这个理解：拓扑序是一个线性顺序，对每条 `u -> v`，`u` 都在 `v` 前面；只有图没有有向环时，完整拓扑序才存在。它还提供 `get_ready()` / `done()`，说明拓扑排序不只是一次性输出列表，也可以作为并行处理节点的接口。

放到 LogServe，topological sort 对应的不是一个独立函数，而是 `scheduleReadySteps` 里的 ready step 判定：step 仍是 `SCHEDULED`、没有绑定 `TaskID`、所有 `DependsOn` 都已经 `SUCCEEDED`，才会被创建成 task。这个语义的核心仍然是正确性，先保证依赖不乱，再谈并发执行。

## Q032. topological sort 的典型适用场景和不适用场景分别是什么？

**回答：**

Topological sort 适合“任务之间有先后依赖，而且依赖图必须无环”的场景。典型例子很多：

```text
workflow engine:
  step B 依赖 step A 的输出，A 成功前 B 不能跑。

build system:
  先编译依赖库，再链接上层目标。

package manager:
  先安装依赖包，再安装依赖它的包。

database migration:
  先建基础表，再建外键、视图、索引或数据迁移。

compiler:
  某些 pass 或模块依赖要按顺序处理。

data pipeline:
  先抽取数据，再清洗，再聚合，再发布。

course prerequisite:
  先修完基础课，再选高级课。
```

它不适合几类场景。

第一类是不允许无环假设的系统。比如事件循环、反馈控制、状态机循环、BPMN 里的循环审批、流处理里的 feedback edge。这些不是“拓扑排序一下”就能解决的。你要么把循环拆成多个 iteration / run，要么用固定点计算、状态机或 event loop。

第二类是依赖会在运行中随意变化，但变化没有被事件化的系统。比如调度过程中突然新增边、删除边，或者下游依赖是从外部数据库临时读出来的。拓扑排序可以支持动态更新，但必须定义图版本和一致性边界。否则排序结果没有明确语义。

第三类是资源调度问题。比如 100 个任务都 ready，但只有 4 张 GPU，应该谁先跑？这不是 topological sort 本身能回答的。它只告诉你“谁可以跑”，不告诉你“谁最应该跑”。

面试时可以这样答：

```text
topological sort 适合无环依赖关系，比如 workflow、构建、包依赖、迁移、数据 pipeline。它不适合表达天然有循环的业务流程，也不能替代资源调度、优先级调度、最短路径、锁顺序证明或分布式事务。只要问题的核心不是“依赖必须先后满足”，就不要硬套拓扑排序。
```

Airflow 文档里 Dag 封装了 tasks、task dependencies、schedule、callbacks 等 workflow 信息，并说明 DAG 自己不关心 task 内部做什么，只关心如何执行它们、顺序、重试、timeout 等。Argo 也把 DAG 作为 steps 序列的替代方案，用依赖关系表达复杂 workflow，并释放最大并行度。这些都是 topological sort 的典型应用环境。

LogServe 当前的适用场景也很明确：多 step Python workflow，step 之间通过 `depends_on` 和 `__step_ref__` 传递结果。它不适合当前阶段表达循环 workflow、动态子 DAG、条件 trigger rule 或有人工审批回路的 BPMN 流程。

## Q033. topological sort 和相近概念最容易混淆的边界在哪里？

**回答：**

Topological sort 最容易和四类东西混淆：图遍历、调度器、关键路径分析、执行历史。

第一，topological sort 不等于 DFS / BFS。DFS 和 BFS 是遍历方式，拓扑排序是输出满足依赖约束的顺序。DFS 可以用来实现拓扑排序，但普通 DFS 访问顺序不一定是合法拓扑序。BFS 也一样，普通广度优先遍历只关心距离或层次，不一定关心所有前置依赖是否完成。

第二，topological sort 不等于 scheduler。拓扑排序告诉你哪些节点可以在依赖上运行，scheduler 还要处理 worker、队列、资源池、优先级、公平性、backpressure、timeout、retry、cancel。Airflow scheduler 文档里就明确提到：scheduler 会选出 schedulable TaskInstances，并在 pool 和 concurrency limit 下 enqueue。这里“依赖满足”只是进入调度的一道门，不是整个调度器。

第三，topological sort 不等于 critical path。拓扑排序只给合法顺序，不计算最长路径。Critical path 要结合每个节点耗时，找出决定 workflow latency 的最长依赖链。一个 DAG 可能有很多拓扑序，但 critical path 通常是由耗时和依赖共同决定的。

第四，topological sort 不等于执行历史。拓扑序是计划层或调度约束；执行历史记录的是某个 run 里实际发生了什么，包括 task id、worker、attempt、start time、result、error、retry。两个 run 可以有同一个拓扑约束，但实际完成顺序不同。

面试时可以这样答：

```text
topological sort 的边界是“依赖顺序”。它不是 BFS/DFS 本身，不是完整 scheduler，不是 critical path，不是执行 trace，也不是分布式一致性协议。它只回答一个问题：在当前依赖图里，哪些节点必须排在另一些节点前面，哪些节点依赖已经满足可以释放。
```

还有一个细节：拓扑序通常不唯一。`A` 完成后 `B` 和 `C` 都可以跑，那么 `A, B, C, D` 和 `A, C, B, D` 都合法。如果系统测试或 replay 要求稳定输出，就必须加 tie-breaker，比如按 definition 顺序、step id、priority 排序。不要把某一次输出的顺序误认为“唯一正确顺序”。

LogServe 当前多个 step 同时 ready 时，`scheduleReadySteps` 按 `state.Definition.Steps` 遍历并 enqueue。这是一种稳定实现策略，但不是 DAG 数学语义要求。换成按优先级、deadline 或资源类型排序，只要不违反 `DependsOn`，仍然是合法调度。

## Q034. topological sort 在高并发场景下可能出现哪些隐藏问题？

**回答：**

高并发下，topological sort 最大的问题不是算法公式，而是“谁有权把一个节点从 not ready 改成 ready，再从 ready 改成 scheduled”。如果这个边界没保护好，就会出现重复调度、漏调度、依赖未满足先运行。

常见隐藏问题有这些。

```text
ready burst:
  一个上游节点完成后释放成千上万个下游节点。
  如果调度器一次性 enqueue，队列和 metadata store 会被打满。

duplicate scheduling:
  两个 scheduler 同时看到同一个节点 ready。
  如果没有 CAS、锁、lease 或唯一幂等键，可能创建两个 task。

lost readiness:
  依赖完成事件已经发生，但 ready queue 更新失败。
  下游一直不被调度。

race on indegree:
  多个 predecessor 并发完成，同时更新同一个 child 的 unresolved count。
  没有原子操作就可能多减、少减。

unstable tie-breaking:
  多个节点同时 ready，但遍历 map/set 顺序不稳定。
  测试、replay 或缓存命中表现不稳定。

hot join node:
  大量分支同时完成，集中更新同一个 fan-in 节点。
  容易形成锁竞争和 metadata 热点。

priority inversion:
  拓扑上 ready 的低优先级任务太多，高优先级 workflow 排队。

cycle hidden as starvation:
  没有 cycle 检测时，某些节点永远不 ready，看起来像调度器慢。
```

面试时可以这样答：

```text
高并发下 topological sort 的风险主要在 ready state 的并发更新。单线程算法里 indegree-- 很简单；分布式调度里它变成了共享状态修改。必须保证同一个节点只被释放一次、只被调度一次，ready burst 要有 backpressure，tie-breaker 要稳定，fan-in 热点要能承受大量 predecessor completion。
```

Airflow 多 scheduler 的设计能说明这个问题。它支持多个 scheduler 并发运行，但关键 section 需要数据库行级锁，以确保 pool/concurrency limit 被正确遵守。否则多个 scheduler 都认为还有空位，就会超发任务。

LogServe 当前通过一个单进程 `workflowMu` 包住 `scheduleReadySteps` 和 `completeWorkflowStep` 的关键 workflow 状态推进，并且 ready step 必须满足 `Status == SCHEDULED` 且 `TaskID == ""`。这个设计在单控制面进程里简单有效。边界是：如果未来变成多控制面实例，不能只靠进程内 mutex，需要把 ready -> scheduled 的状态转换做成数据库事务、CAS 或日志驱动的唯一提交。

## Q035. topological sort 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

拓扑排序在白板上通常是一次性算法：读图、排序、结束。Workflow engine 里不是这样。节点会运行、失败、超时、重试；控制面和 worker 可能崩溃；完成事件可能重复到达。这里最关键的边界是：什么时候一个节点可以被认为 done，并释放下游？

几个边界条件必须讲清楚。

```text
scheduled 不等于 done:
  节点被放进队列，只代表已经分配执行，不代表依赖完成。
  下游不能因为上游 scheduled 就 ready。

started 不等于 done:
  worker 开始执行后可能 crash、timeout、失败。
  下游仍然不能释放。

failed 是否 done:
  在 all_success 语义里，failed 不是下游成功依赖。
  在 all_done / optional / trigger rule 语义里，failed 可能满足某些下游条件。

retry 不能重复释放:
  某 step 第一次 attempt 失败，第二次成功。
  只有最终成功那一次能释放下游。

duplicate completion:
  worker 重试回调、网络重发或旧 worker 延迟回传。
  同一个 step 成功不能让 child indegree 多减一次。

crash after enqueue:
  task 创建了，但 workflow step 没写 StepScheduled，或反过来。
  恢复时必须能收敛，不丢不重。

timeout ambiguity:
  step timeout 不一定表示外部副作用没发生。
  重试前要考虑幂等和外部状态确认。

cycle after restart:
  如果定义中有环，重启不会自动变好。
  需要在 submit/prepare 阶段拒绝，或在恢复时标出 stuck cause。
```

面试时可以这样答：

```text
在 workflow engine 里，topological sort 的 done 必须绑定 step 的终态语义。对默认 all_success DAG 来说，只有上游 SUCCEEDED 才能释放下游。scheduled、started、failed、timeout 都不能误当作完成。重试和重复 completion 还要求释放下游是幂等的，否则 child indegree 会被多减，导致依赖未满足的节点被提前调度。
```

Python `TopologicalSorter.done()` 的文档也有类似约束：只能把已经从 `get_ready()` 返回过、尚未处理过的 node 标成 done；重复 done 或 done 未 ready 的节点会报错。工程系统也是这个意思，只是错误表现从本地异常变成了线上重复调度或状态不一致。

LogServe 当前的语义是：只有 `StepSucceeded` 会把 step 状态改成 `SUCCEEDED`，`DependenciesSucceeded` 只认这个状态。失败 step 如果还有 retry 次数，会回到 `SCHEDULED` 且清空 `TaskID`；超过次数才 `WorkflowFailed`。重复完成已经成功的 step 时，`completeWorkflowStep` 会 no-op。重启时 `bootstrapWorkflows` 通过 `workflow.Replay` 重建 workflow 状态，并对 running workflow 继续 schedule ready steps。

当前缺口也要诚实说：LogServe 还没有 submit 阶段的显式 cycle validation；有环或未知依赖会表现成 step 一直不 ready，而不是清晰的 validation error。

## Q036. topological sort 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

单机纯算法里，topological sort 的复杂度通常是 `O(V + E)`，`V` 是节点数，`E` 是边数。对普通规模的图，CPU 很少是问题。真正进入 workflow engine 后，瓶颈经常从“排序算法”转移到状态存储、锁竞争、数据库 I/O 和 ready queue 写入。

可以分层看。

```text
CPU:
  构图、计算 indegree、遍历边。
  图很大或每轮重复全量扫描时会变成瓶颈。

内存:
  保存 adjacency list、predecessor set、indegree、ready queue、node state。
  fan-out 很大时，边数和状态对象数量会压内存。

锁竞争:
  多 worker completion 同时释放下游。
  fan-in 节点和全局 ready queue 会变热点。

I/O:
  每个 step 状态变化都要写数据库、日志或对象存储。
  调度器为了判断 ready 频繁读写 metadata。

网络:
  分布式 scheduler、远程 DB、远程 queue、远程 result store 都会引入网络延迟。
  单机内存排序没有这个问题。
```

Airflow scheduler 文档也把 scheduler 性能拆成部署资源、DAG 数量和复杂度、每轮处理多少 task instances、数据库连接和数据库使用等因素。它还提到多 scheduler 场景下需要数据库锁来保护关键 section。这说明生产调度器的瓶颈往往不在 `indegree--`，而在状态协调。

面试时可以这样答：

```text
纯 topological sort 的瓶颈一般是 O(V+E) 的 CPU 和图结构内存；但在 workflow scheduler 里，瓶颈更多来自 I/O 和锁竞争。每个 ready 节点都要创建 task、写日志、更新 metadata、进入队列。高并发下，全局 workflow lock、数据库行锁、fan-in 节点更新、ready queue 写入都会比排序算法本身更贵。
```

LogServe 当前 `scheduleReadySteps` 每次会遍历 `state.Definition.Steps`，所以单个 workflow 的一次扫描是 `O(number_of_steps)`；每调度一个 ready step，还要 `ResolveArgs`、`enqueueTask`、append `StepScheduled`、更新 metadata。小 DAG 下这很清楚；大 fan-out DAG 下，瓶颈更可能是日志写入、metadata 更新和 `workflowMu` 临界区，而不是依赖判断本身。

如果要优化，路线通常是：

```text
1. 提交时构建 predecessor / successor 索引。
2. 维护 unresolved dependency count，避免每次全量扫描。
3. completion 时只检查受影响的下游节点。
4. ready queue 分片，减少全局锁。
5. 批量写 StepScheduled 或批量 enqueue。
6. 对超大 DAG 做分页、mapped step 或 child workflow。
```

但这些优化要等正确性语义稳了再做。过早做并发优化，很容易把 duplicate scheduling 和 lost readiness 引进来。

## Q037. topological sort 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

这三类测试目的不同，不能混着看。

Correctness test 关心“结果对不对”。它应该覆盖：

```text
valid order:
  对每条边 A -> B，A 在输出中必须早于 B。

cycle detection:
  A -> B -> C -> A 必须报 cycle。

self dependency:
  A -> A 必须报错。

unknown dependency:
  depends_on 指向不存在节点时要拒绝或明确定义行为。

multiple roots:
  多个无依赖节点都应该 ready。

duplicate edges:
  重复依赖不能导致 indegree 多算。

disconnected components:
  多个独立子图都能排序。

stable tie-breaker:
  如果要求可复现，同一图多次输出顺序必须一致。

ready/done protocol:
  未 get_ready 的节点不能 done，重复 done 不能通过。

large fan-in/fan-out:
  子节点必须等所有 predecessor 都 done。
```

Stress test 关心“在压力和并发下会不会坏”。它应该构造很宽的 DAG、很深的 DAG、随机 DAG、边数很大的 DAG、多 worker 并发 completion、重复 completion、retry storm、scheduler crash/restart。重点看有没有死锁、重复调度、漏调度、内存暴涨和状态不一致。

Benchmark 关心“多快、瓶颈在哪”。它要拆开测：

```text
build_graph_time:
  解析 definition 并构建图结构耗时。

prepare_time:
  cycle detection / indegree 初始化耗时。

get_ready_time:
  获取 ready set 的耗时。

done_time:
  一个或一批节点完成后释放下游的耗时。

memory_per_node_edge:
  每个节点和边大概占多少内存。

scheduler_overhead:
  从 ready 到 task enqueued 的端到端成本。

replay_time:
  崩溃后从 history 恢复图状态的耗时。
```

面试时可以这样答：

```text
correctness test 验证拓扑顺序、cycle、未知依赖、重复 done、稳定顺序；stress test 用宽图、深图、随机图、并发 completion、retry storm 和 crash recovery 去打状态机；benchmark 则拆开测构图、prepare、get_ready、done、enqueue 和 replay 的耗时与内存。对 workflow scheduler 来说，benchmark 不能只测纯算法，还要测写日志、写 metadata 和队列入队的成本。
```

LogServe 当前适合补的测试有几类：

```text
1. SubmitWorkflow 拒绝 cycle definition。
2. SubmitWorkflow 拒绝 unknown depends_on。
3. 多个 roots 同时被 schedule。
4. diamond DAG 中 D 只能在 B、C 都 SUCCEEDED 后 schedule。
5. 重复 CompleteTask 不重复释放下游。
6. retry failure 不释放下游，retry success 后才释放。
7. 控制面重启后 replay state 与 metadata 一致，并继续 schedule ready steps。
8. 大 fan-out workflow 下没有重复 TaskID。
```

这些测试比单纯“排序结果是某个数组”更贴近 workflow runtime。

## Q038. 如果要求从零实现一个简化版 topological sort，你会先定义哪些不变量？

**回答：**

从零实现前，我会先写不变量。因为 topological sort 很容易看起来简单，实际 bug 经常藏在重复边、cycle、并发 done、ready 集合重复返回这些细节里。

核心不变量可以这样定义：

```text
graph immutability:
  prepare 之后图不能再被修改。
  如果要支持动态图，必须引入图版本或增量协议。

node uniqueness:
  每个 node id 唯一。
  输出中每个 node 只能出现一次。

edge direction:
  统一定义 edge 是 dependency -> dependent，还是 node -> predecessor。
  整个实现不能混用。

unresolved count:
  unresolved[node] 永远等于还没 done 的 predecessor 数量。

ready invariant:
  ready queue 里的节点必须 unresolved == 0，且未 emitted、未 done。

emitted invariant:
  get_ready 返回过的节点不能再次返回，除非系统显式支持 retry 为新 attempt。

done invariant:
  只有 emitted 且未 done 的节点才能 done。
  一个节点只能 done 一次。

order invariant:
  对每条 dep -> child，child 不能在 dep done 前被 ready。

cycle invariant:
  如果 ready 为空、仍有 unresolved 节点、且没有 in-flight 节点，就存在 cycle 或未满足依赖错误。

determinism invariant:
  如果需要稳定输出，ready 集合内部顺序必须有确定 tie-breaker。
```

一个最小 Kahn 实现可以这么设计：

```text
input:
  nodes
  edges dep -> child

state:
  successors[dep] = children
  unresolved[child] = number of unique predecessors
  ready = nodes with unresolved == 0
  emitted = set()
  done = set()

prepare:
  validate nodes, deps, duplicate ids
  compute successors and unresolved
  initialize ready

get_ready:
  return ready nodes not emitted
  mark them emitted

done(node):
  assert node in emitted and not done
  mark done
  for child in successors[node]:
    unresolved[child] -= 1
    if unresolved[child] == 0:
      ready.push(child)

finish:
  if len(done) == len(nodes), success
  if no ready and no in-flight, cycle or invalid graph
```

面试时可以这样答：

```text
我会先定义 unresolved count、ready、emitted、done 这几个集合的不变量。最重要的是：ready 里的节点必须所有 predecessor 已 done；每个节点只能 ready 一次、done 一次；done 只能发生在 get_ready 之后；任何 child 的 unresolved 不能被重复 completion 多减。定义好这些不变量，再写 Kahn 算法，测试会清楚很多。
```

放到 workflow engine，还要多加一个运行时不变量：

```text
step ready != task scheduled != task started != step succeeded
```

这几个状态不能混。LogServe 当前用 `StepState.Status` 和 `TaskID` 区分：初始是 `SCHEDULED` 但 `TaskID` 为空；真正 enqueue 以后才有 `TaskID`；只有 `StepSucceeded` 才能让下游 `DependenciesSucceeded`。

## Q039. topological sort 的常见误用是什么，误用后通常会产生什么线上症状？

**回答：**

Topological sort 的误用，很多不是算法错，而是把它用在了错误的语义层。

常见误用包括：

```text
把拓扑序当唯一顺序:
  多个合法顺序都可能对。
  如果没定义稳定 tie-breaker，测试和 replay 会抖。

把拓扑排序当完整调度器:
  只检查依赖，不检查 worker、pool、quota、priority、deadline。
  线上表现是 ready 任务堆积、资源打满、低优先级任务占满 worker。

不做 cycle detection:
  有环时没有明确报错。
  线上表现是 workflow 一直 RUNNING，某些 step 永远 SCHEDULED。

把 scheduled 当 succeeded:
  上游刚入队就释放下游。
  线上表现是下游读不到上游结果，或者读到空结果/旧结果。

忽略数据依赖:
  只写 control dependency，忘了 args 里还有 step ref。
  或者引用了上游输出但没声明 depends_on。
  线上表现是 input resolution 失败，workflow 卡在调度阶段。

重复 completion 多次释放 child:
  retry 或网络重发导致 child indegree 多减。
  线上表现是 join step 提前运行。

动态图不事件化:
  第一次运行生成一批节点，恢复后生成另一批。
  线上表现是 replay 不一致、重复 task、漏 task。

超大 fan-out 全量展开:
  一次生成大量 ready nodes。
  线上表现是 scheduler CPU 飙高、DB 连接耗尽、队列水位打满。
```

面试时可以这样答：

```text
最常见的误用是把 topological sort 当成 scheduler。它只解决依赖顺序，不解决资源、失败、重试、取消和公平性。另一个常见错误是把“已经调度”误当作“已经完成”，这会让下游提前跑。线上症状通常是 workflow 卡住、join 节点提前执行、重复 task、队列暴涨、或者不同 run 的执行顺序不稳定。
```

在 LogServe 当前代码里，比较典型的边界是 cycle 和 unknown dependency。`DependenciesSucceeded` 对不存在依赖会返回 false，所以错误定义不会提前跑，但也不会给用户一个清晰的提交错误。线上表现会是 workflow 保持 running，相关 step 没有 `TaskID`，看起来像调度器没工作。更好的做法是在 `SubmitWorkflow` 调用 `ParseDefinition` 后立刻做 DAG validation。

还有一个误用是忽略幂等。即使拓扑顺序正确，`CompleteTask` 重复到达也可能发生。LogServe 通过 step 已成功 no-op、`WorkflowCompleted` 幂等键、task lease epoch 来降低这个风险。这些不是 topological sort 的一部分，但没有它们，拓扑调度在线上会被重复事件打穿。

## Q040. topological sort 在单机和分布式环境中的语义有什么差异？

**回答：**

单机环境里，topological sort 可以是一个内存算法：一个进程持有完整图、indegree 表、ready queue 和 done set。只要没有并发修改，语义很干净。`prepare()` 后图固定，`get_ready()` 返回 ready nodes，`done()` 释放下游。

分布式环境里，拓扑排序变成了分布式状态推进问题。图可能很大，节点可能分散在不同 worker，多个 scheduler 可能同时运行，完成事件可能重复、乱序、延迟到达。此时真正的问题是：谁来判断节点 ready？谁能提交 ready -> scheduled？这个提交如何持久化？旧 scheduler 或旧 worker 的结果如何被拒绝？

差异可以这样看：

```text
单机:
  图在内存里。
  ready queue 在一个进程里。
  done 调用顺序由本进程控制。
  锁很少，恢复问题简单。
  崩溃后如果没有持久化，状态丢失。

分布式:
  图和状态要持久化。
  ready 计算可能由多个 scheduler 竞争。
  task completion 可能重复、乱序、延迟。
  需要 lease、fencing、idempotency、事务、日志或共识。
  恢复时要从 durable history 重建 ready/done 状态。
```

Airflow 多 scheduler 的设计是一个现实例子。它不是让 scheduler 之间直接跑 Raft，而是利用已有 metadata database；但在 schedulable TaskInstances enqueue 的关键 section，需要数据库行级锁，保证 pool 和 concurrency limit 不被多个 scheduler 同时突破。这说明分布式拓扑调度最难的不是数学排序，而是共享状态的并发控制。

面试时可以这样答：

```text
单机 topological sort 是数据结构问题，分布式 topological sort 是状态一致性问题。单机只要保证 indegree 和 ready queue 正确；分布式还要保证多个 scheduler 不重复释放同一个节点，不重复创建 task，worker completion 有 lease/fencing，crash 后能从 durable log 恢复。拓扑顺序只是约束，分布式执行还需要幂等和一致性协议来保护状态转换。
```

LogServe 当前更接近单控制面、多 worker 的模型。控制面用 `workflowMu` 保护 workflow 状态推进，shared log 是 source of truth，metadata 是 materialized view，worker 完成 task 时带 `TaskLeaseEpoch`。这已经比纯内存拓扑排序多了恢复和幂等语义，但还不是多 scheduler 分布式拓扑调度。

如果未来要做多控制面实例，至少要补这些东西：

```text
1. workflow step ready -> scheduled 的事务或 CAS。
2. StepScheduled 的唯一幂等键和数据库唯一约束。
3. 多 scheduler 的 shard ownership 或 row-level locking。
4. task lease epoch / fencing 覆盖所有 terminal completion。
5. bootstrap replay 和在线 reconciliation。
6. ready queue 分片，避免单点热点。
7. cycle / stuck workflow 的可观测诊断。
```

这时候 topological sort 仍然是核心依赖算法，但系统正确性取决于它周围的持久化和并发控制。

## Q041. cycle detection 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

Cycle detection 的核心目标是拒绝一个无法被正确调度的依赖定义。对 workflow engine 来说，它回答的问题不是"怎么跑得更快"，而是：

```text
这个 workflow 的依赖关系是不是一个合法 DAG？
有没有某些 step 必须等自己，或者间接等自己？
如果继续接受这个定义，scheduler 会不会永远找不到 ready node？
```

所以它主要解决正确性问题。性能和可维护性是副作用：提前发现环，可以避免 scheduler 反复扫描一个永远无法推进的 workflow；错误信息清楚，也能让用户更快修配置。但本质上，cycle detection 是提交期或编译期的合法性检查。

Python 标准库 `graphlib.TopologicalSorter` 的语义很适合作为面试参照：完整拓扑序存在的前提是图中没有有向环；`prepare()` 会标记图构建完成并检查 cycle，发现 cycle 时抛出 `CycleError`。这说明 cycle detection 和 topological sort 是一体两面：没有 cycle，才有完整拓扑排序；有 cycle，调度器只能跑出部分无依赖节点，最后会被环卡住。

在 workflow 场景里，cycle detection 要保护这些不变量：

```text
1. 每个 step 只能依赖已存在的 step。
2. depends_on 形成的是有向无环图。
3. 任意 step 不能直接或间接依赖自己。
4. workflow 至少能找到一个起点，除非它本身为空定义。
5. 所有非起点 step 最终都能通过上游成功释放。
```

如果不做 cycle detection，系统不一定马上崩，但会出现更糟糕的症状：workflow 被接受，状态是 RUNNING，某些 step 一直停在 SCHEDULED，`TaskID` 为空，worker 永远拿不到任务。用户看到的是"调度器卡住了"，但根因其实是定义非法。

结合 LogServe 当前实现看，这个边界很明确。`ParseDefinition` 现在主要做 JSON 解析和默认参数填充，还没有做 step id 唯一性、unknown dependency、cycle detection。`scheduleReadySteps` 依赖 `DependenciesSucceeded` 判断上游是否成功；如果存在 A 依赖 B、B 依赖 A，两个 step 都不会被 enqueue。系统不会错误执行，但也不会给出一个提交期错误。这是正确性上可以继续补强的地方。

面试里可以这样答：

```text
cycle detection 的核心目标是保证依赖图是 DAG。它主要解决 correctness，而不是性能优化。没有它，workflow engine 可能接受一个永远无法完成的定义，scheduler 会一直扫描但没有新的 ready node。成熟实现通常在提交或 prepare 阶段检测 cycle，并返回可诊断的环路径；运行期 scheduler 不应该靠“长时间没有进展”来猜测是不是有环。
```

## Q042. cycle detection 的典型适用场景和不适用场景分别是什么？

**回答：**

Cycle detection 适用于所有"先有依赖图，再按依赖推进"的场景。只要系统声明自己处理 DAG，cycle detection 基本就是必要能力。

典型适用场景包括：

```text
workflow / DAG engine:
  step B depends_on A，step D depends_on B/C。
  提交定义时要拒绝 A -> B -> A。

构建系统:
  target A 依赖 target B，B 又依赖 A。
  如果不拒绝，构建永远无法确定顺序。

包管理器:
  package dependency graph 要避免不可解的循环依赖。
  有些语言允许有限形式的循环引用，但包级安装顺序通常仍要处理 cycle。

数据管道:
  dataset B 由 dataset A 生成，dataset A 又引用 dataset B。
  这会让增量计算和血缘分析失去明确起点。

任务编排:
  初始化、迁移、清理任务之间有顺序约束。
  cycle 意味着没有一个任务能合法开始。

编译器和模块系统:
  import graph、类型依赖、初始化顺序可能需要检测强连通分量。
```

不适用或不能简单使用的场景也不少：

```text
本来就是有环的状态机:
  状态机通常允许 A -> B -> A。
  这里的 cycle 是业务行为，不是非法依赖。

事件循环和反馈控制:
  control loop、stream processing、actor message loop 本来就会循环。
  不能用 DAG cycle detection 直接拒绝。

迭代算法:
  PageRank、训练 loop、固定点计算会重复运行同一组节点。
  正确抽象应该是 loop / iteration / convergence，不是 DAG。

长期服务依赖:
  服务 A 调用服务 B，B 偶尔回调 A。
  这更像 runtime dependency 或 deadlock 风险，不等于 workflow DAG cycle。

动态依赖未冻结:
  运行中才生成节点和边。
  可以做增量 cycle detection，但必须先定义图版本和变更边界。
```

一个容易被忽略的点是：不是所有 cycle 都应该被禁止，只有"调度前置依赖"里的 cycle 必须被禁止。比如 Argo Workflows 的 DAG 用 task dependencies 表达执行顺序，这种依赖必须是 acyclic；但一个业务 step 内部可以执行循环逻辑，或者一个 workflow 可以通过上层机制反复触发下一轮 run。后者不应该被 DAG cycle detection 拒绝。

LogServe 当前 `depends_on` 是控制依赖。它的语义是"只有这些上游 step succeeded，当前 step 才能入队"。在这个语义下，cycle detection 应该适用于 `Definition.Steps[].DependsOn`，也应该扩展检查 `ArgsJSON` 里的 `__step_ref__` 是否和 `depends_on` 一致。否则用户可能没有写 depends_on，却在参数里引用上游结果，调度顺序就不完整。

面试里可以这样答：

```text
cycle detection 适合 DAG workflow、构建图、包依赖、数据血缘和任务编排，因为这些系统需要从无依赖节点开始推进。不适合本来就允许循环的状态机、事件循环、反馈控制和迭代算法。关键是看边的语义：如果边表示“必须先完成”，cycle 是非法；如果边表示“可能跳转”或“下一轮迭代”，cycle 未必是错。
```

## Q043. cycle detection 和相近概念最容易混淆的边界在哪里？

**回答：**

Cycle detection 最容易和下面几个概念混在一起：

```text
cycle detection vs topological sort:
  cycle detection 判断图是否合法。
  topological sort 给出一个合法执行顺序。
  很多算法会同时做这两件事，但语义不是一回事。

cycle detection vs deadlock detection:
  cycle detection 通常发生在静态依赖图上。
  deadlock detection 发生在运行期等待图上。
  A 等 B、B 等 A 可以是资源死锁，也可以是非法 DAG，取决于边的含义。

cycle detection vs stuck workflow diagnosis:
  workflow 没有进展不一定是 cycle。
  也可能是 worker 不足、backpressure、上游失败、timeout、外部 I/O 卡住。

cycle detection vs reachability check:
  无环图也可能有不可达节点，取决于是否允许多 root。
  多 root DAG 在 Argo 这类系统里是正常的，不应该误判为错误。

cycle detection vs strongly connected components:
  SCC 是图分析结果。
  cycle detection 只需要知道是否存在非平凡 SCC，或者自环。

cycle detection vs recursive workflow:
  一个 workflow 触发另一个 workflow，再触发回自己，可能是业务递归。
  这不是单个 DAG 定义里的 cycle，除非系统把跨 workflow 依赖也纳入同一张图。
```

面试中比较好的说法是：cycle detection 检查的是 dependency graph 的结构正确性；deadlock detection 检查的是 runtime wait-for graph；stuck workflow diagnosis 则是运维问题，需要结合 worker、队列、资源、失败策略一起看。

拿 LogServe 举例，如果 A depends_on B，B depends_on A，这是定义期 cycle。应该在 `SubmitWorkflow` 阶段拒绝。如果 A 已经被 enqueue 但 worker 长时间不返回，这是执行期 stuck，不是 cycle。如果 queue high watermark 满了，`scheduleReadySteps` 返回 backpressure 错误，也不是 cycle。如果上游 step 失败且不再 retry，下游永远不会 ready，这也不是 cycle，而是失败传播策略的问题。

还有一个边界是"控制依赖"和"数据依赖"。`depends_on` 是控制边，`ArgsJSON` 里的 `__step_ref__` 是数据边。理想情况下，数据引用应该要求对应控制依赖存在，或者由引擎自动补边。否则 cycle detection 只看 `depends_on`，会漏掉参数引用形成的真实依赖。

面试里可以这样答：

```text
cycle detection 不是死锁检测，也不是 stuck workflow 检测。它检查的是静态依赖图中有没有从一个节点沿 depends_on 回到自己的路径。运行期没有进展可能有很多原因，cycle 只是其中一种，而且最好在提交期就发现。实现上还要分清控制依赖和数据依赖，否则只检查 depends_on 会漏掉 args 里的 step reference。
```

## Q044. cycle detection 在高并发场景下可能出现哪些隐藏问题？

**回答：**

高并发下，cycle detection 的难点不只是算法复杂度，而是"你到底在检查哪一个版本的图"。

常见隐藏问题包括：

```text
边检查边修改:
  一个线程还在检测 graph，另一个线程新增依赖边。
  检测通过的可能不是最终提交的图。

读到了混合版本:
  step 列表来自 version N，depends_on 来自 version N+1。
  结果可能 false negative，也可能误报 cycle。

动态图扩展没有事务:
  fan-out 动态生成节点时，先写节点后写边。
  scheduler 在中间状态读图，可能认为节点无依赖并提前入队。

缓存的 validation 结果过期:
  定义改了，但仍复用旧的 "acyclic=true"。
  线上表现是偶发卡住，很难复现。

增量检测漏边:
  只检查新增节点，不检查新增边是否连接了旧路径。
  A -> B 已存在，新增 B -> A 会被漏掉。

全局锁过粗:
  每次提交大图都锁住 scheduler。
  workflow 提交量一上来，调度延迟抖动。

错误地把无进展当 cycle:
  高并发下队列满、worker 慢、DB 慢都可能没有 ready 进展。
  如果直接标记为 cycle，会误杀正常 workflow。

错误信息不稳定:
  多个 cycle 同时存在时，不同 goroutine 或 map iteration 返回不同环。
  测试和用户诊断都会变得不稳定。
```

Python `TopologicalSorter.prepare()` 有一个很重要的边界：prepare 后图不能再修改。这个约束看起来简单，但对并发系统很关键。它等价于说，validation 和后续调度基于同一份冻结的图。如果要支持动态图，也应该显式引入 graph version，而不是让 scheduler 读一个随时变化的对象。

在 LogServe 当前实现里，workflow definition 在 `WorkflowStarted` 事件里持久化，之后 `scheduleReadySteps` 扫描 `state.Definition.Steps`。这比运行中任意改图简单很多。当前更实际的问题是提交阶段缺少 validation：如果多个 workflow 并发提交，每个 workflow 的图彼此独立，cycle detection 不难；真正要小心的是不要在持有 `workflowMu` 的运行期反复做重型 validation，把调度和 completion 都堵住。

比较稳的设计是：

```text
1. SubmitWorkflow 解析 definition。
2. 构造本地不可变 graph。
3. 检查 step id 唯一、dependency 存在、数据引用合法。
4. 做 cycle detection，生成稳定错误路径。
5. validation 通过后，把 definition 随 WorkflowStarted 一起写入 durable log。
6. 运行期只基于已验证的 graph 推进 ready state。
```

面试里可以这样答：

```text
高并发下 cycle detection 最大的问题是图版本一致性。检测必须基于冻结快照，否则会出现检查通过的不是最终图、动态图半写入被 scheduler 看到、validation 缓存过期等问题。算法本身是 O(V+E)，通常不是最难的部分；最难的是让 validation、持久化和调度看到同一版依赖定义。
```

## Q045. cycle detection 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

Cycle detection 看起来是提交期逻辑，但崩溃和恢复会把它的边界放大。关键问题是：validation 结果有没有和 workflow definition 一起成为 durable state。

典型边界条件有：

```text
validation 通过但 definition 没写入日志:
  服务崩溃后什么都没有，应该允许用户重试提交。

definition 写入日志但 validation 结果没持久化:
  重启 replay 时如果不重新验证，可能恢复出一个非法 workflow。
  如果重新验证，又要保证算法版本变化不会把旧定义突然判死。

validation 失败但写了部分 metadata:
  线上会出现 metadata-only workflow。
  用户查得到 workflow，但日志里没有合法 WorkflowStarted。

动态图运行中引入 cycle:
  之前的 graph 是合法的，新扩展的边产生 cycle。
  必须把 graph mutation 当成事件，并在提交 mutation 前做增量检测。

超时被误判为 cycle:
  某个上游 step 超时，导致下游无法 ready。
  这是失败传播或 retry 策略，不是 cycle。

retry 被误认为能打破 cycle:
  A 等 B、B 等 A，重试多少次都不会释放。
  retry 只适用于已被调度并执行失败的 step。

恢复后 stuck workflow 没有诊断:
  replay 得到 RUNNING，但没有 ready node，也没有 running task。
  如果没有 cycle/stuck 分类，运维只能看到一个不动的 workflow。
```

LogServe 已经有一些 log-first 的设计：`WorkflowStarted` 写日志后再 materialize，`bootstrapWorkflows` 通过读取 `wf:` stream 来 `Replay`，再恢复 running workflow 的 task。`log_first_test.go` 也覆盖了 append failure 不应留下 metadata-only workflow，以及 backpressure 不应留下 phantom `TaskID`。这些测试保护的是崩溃恢复和状态一致性。

但 cycle detection 如果要补进去，应该放在 `WorkflowStarted` 写入之前：

```text
SubmitWorkflow:
  ParseDefinition
  ValidateWorkflowDefinition
    - step id 唯一
    - depends_on 存在
    - __step_ref__ 指向存在 step
    - 数据依赖被控制依赖覆盖
    - cycle detection
  append WorkflowStarted
  materialize metadata
  scheduleReadySteps
```

这样崩溃恢复时有两个选择：要么信任已经写入日志的 definition 一定合法；要么 replay 时重新验证，但只把验证失败作为"历史数据损坏"或"兼容性错误"处理，不能悄悄把 workflow 变成另一种状态。

面试里可以这样答：

```text
cycle detection 的恢复边界是 validation 和 durable definition 必须同生共死。不能 validation 失败还留下 metadata，也不能恢复时接受一个没有验证过的 definition。超时和重试不会打破 cycle，因为 cycle 阻止的是 ready 生成；retry 只处理已经执行过的失败 step。一个成熟 engine 还应该把“无 ready、无 running、非 terminal”的 workflow 诊断为 stuck，并区分 cycle、上游失败、资源阻塞和数据缺失。
```

## Q046. cycle detection 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

单机内存里的 cycle detection 通常是 CPU 和内存问题，复杂度一般是 O(V+E)。V 是 step 数，E 是依赖边数。对普通 workflow 来说，这个成本很小；真正让它变慢的，通常是大规模图、频繁提交、动态图增量更新，或者 validation 被放在了不合适的锁里。

可以分层看：

```text
CPU:
  DFS 三色标记、Kahn indegree 扣减、SCC 算法都要扫节点和边。
  如果每次调度 tick 都重新检测全图，CPU 会被浪费。

内存:
  需要 adjacency list、indegree map、visited/color map、parent map。
  大 fan-out / fan-in workflow 会让边表膨胀。

锁竞争:
  如果在全局 scheduler 锁下做 O(V+E) 检测，completion 和 enqueue 都会排队。
  多 workflow 并发提交时，最好按 workflow 或 definition 局部处理。

I/O:
  如果 workflow definition 很大，需要从 DB、对象存储或远端配置中心读取，I/O 会比算法本身贵。
  JSON/YAML 解析也可能比 DFS 更明显。

网络:
  单机检测通常没有网络瓶颈。
  分布式图分片、跨服务读取依赖、远程 validation 才会引入网络成本。
```

Airflow scheduler 文档提到调度性能会受 DAG 数量、DAG 复杂度、CPU、内存、网络吞吐、每轮处理 task 数等因素影响。这给 workflow engine 一个很实用的提醒：cycle detection 自己可能很便宜，但它所在的"解析、序列化、DB 查询、调度循环"不一定便宜。

对 LogServe 当前规模来说，cycle detection 的成本应该放在提交期，不要放进 `scheduleReadySteps` 的循环里。`scheduleReadySteps` 已经要扫描 steps、解析参数、enqueue task、append `StepScheduled`、更新 metadata。如果每次 step succeeded 后又重新全图检测 cycle，会把运行期热路径拖慢，而且没有必要。definition 提交后不变，提交期验证一次就够。

面试里可以这样答：

```text
cycle detection 算法本身通常是 O(V+E)，主要吃 CPU 和内存。在线上更常见的瓶颈是锁放错位置，或者每次调度都重复全图检测。大规模 workflow 还会把 JSON/YAML 解析、DB 读取和图构建变成主要成本。我的设计倾向是提交期基于不可变 definition 做一次 validation，运行期只使用验证后的图推进 ready 状态。
```

## Q047. cycle detection 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

Cycle detection 的测试要分三层：正确性测试证明判断没错，压力测试证明并发和大图下不出状态问题，benchmark 才看性能。

Correctness test 应该覆盖：

```text
合法图:
  空图。
  单节点无依赖。
  多 root DAG。
  diamond: A -> B/C -> D。
  长链: A -> B -> C -> ...。

非法图:
  自环: A -> A。
  两节点环: A -> B -> A。
  长环: A -> B -> C -> A。
  多个 cycle 同时存在。
  cycle 上还有入边和出边。

定义错误:
  duplicate step_id。
  depends_on 指向不存在 step。
  __step_ref__ 指向不存在 step。
  args 里引用了上游结果但 depends_on 没声明。
  result_step_id 不存在。

错误质量:
  返回明确错误类型。
  返回稳定 cycle path。
  不修改输入 definition。
```

Stress test 应该覆盖：

```text
并发提交大量 workflow:
  每个 workflow 独立检测，不能互相污染状态。

大 fan-out / fan-in:
  一个 root 释放几千个 child，或者几千个 parent 汇聚到一个 join。

随机 DAG:
  生成 N 个节点，只允许从小编号指向大编号，保证无环。
  再随机加一条回边，确认能检测出来。

动态图模拟:
  在 graph version N 上检测通过。
  尝试追加一条会产生 cycle 的边，确认增量检测拒绝。

崩溃注入:
  validation 失败不能留下 metadata-only workflow。
  append 失败不能留下半提交状态。
```

Benchmark 应该测：

```text
不同 V/E 规模下的耗时:
  100 nodes / 1k edges。
  10k nodes / 100k edges。
  100k nodes / 1M edges。

不同图形态:
  长链。
  宽 fan-out。
  宽 fan-in。
  稀疏随机 DAG。
  接近完全 DAG 的稠密图。

内存分配:
  adjacency / indegree / color map 的 alloc 次数和峰值。

错误路径构造:
  检测到 cycle 后是否为了生成 path 额外扫太多图。

并发影响:
  多 goroutine 提交时 p50/p95/p99 validation latency。
  scheduler 热路径是否被 validation 锁阻塞。
```

对 LogServe，可以把测试落到两个层面：

```text
workflow 包:
  新增 ValidateDefinition(def)。
  单测覆盖 step id、unknown dep、cycle、step ref。

control 包:
  SubmitWorkflow 收到非法 definition 应返回 error。
  appendLog 不应被调用，metadata 不应出现 workflow。
  合法 workflow 仍能 schedule root step。
```

面试里可以这样答：

```text
correctness test 测图判断和错误语义，stress test 测并发提交、大图和崩溃注入，benchmark 测 O(V+E) 在不同图形态下的耗时、内存和锁影响。对 workflow engine 来说，我不会只测“能不能找到环”，还会测 validation 失败时不会写日志、不会创建 metadata，也不会留下一个 RUNNING 但永远不动的 workflow。
```

## Q048. 如果要求从零实现一个简化版 cycle detection，你会先定义哪些不变量？

**回答：**

从零实现 cycle detection，我会先定义不变量，而不是先写 DFS。因为很多线上 bug 不是 DFS 写错，而是图语义没定清楚。

第一组是不变量：

```text
节点不变量:
  每个 step_id 唯一。
  每个依赖引用的 step_id 必须存在。
  每个 step_id 在 graph 中正好对应一个 node。

边不变量:
  depends_on 的方向要固定。
  如果 B depends_on A，可以表示为 A -> B，也可以表示为 B -> A。
  但整个实现必须统一，错误信息也要按用户能理解的方向输出。

图版本不变量:
  validation 期间 graph 不可变。
  validation 通过的 graph 才能进入调度。
  如果允许动态图，每次 mutation 都有版本号，并且 mutation 自己也要通过检测。

状态不变量:
  white 表示未访问。
  gray 表示当前 DFS 栈中。
  black 表示该节点及其后继已经验证无环。
  遇到 gray 节点就说明存在 cycle。

错误不变量:
  检测到 cycle 时返回一条可解释路径。
  错误不能依赖 map iteration 的随机顺序。
  validation 失败不能修改 workflow state。
```

一个简化 DFS 版本可以这样设计：

```text
validate(def):
  index steps by step_id
  reject duplicate step_id
  build adjacency from dependency to dependent
  reject unknown dependency
  sort node ids for deterministic traversal
  color[node] = white
  parent[node] = ""

  for node in sorted nodes:
    if color[node] == white:
      dfs(node)

dfs(node):
  color[node] = gray
  for next in sorted adjacency[node]:
    if color[next] == gray:
      return cycle path using parent
    if color[next] == white:
      parent[next] = node
      dfs(next)
  color[node] = black
```

Kahn 算法也能做 cycle detection：计算 indegree，把 indegree 为 0 的节点入队，不断弹出并减少下游 indegree；最后如果 processed count 小于 node count，剩下的节点就在 cycle 或被 cycle 阻塞的区域里。Kahn 的优势是和 ready queue / topological sort 更接近；DFS 的优势是构造 cycle path 更直接。

对 workflow engine，我还会定义一个更上层的不变量：

```text
如果 validation 成功，则运行期一定存在至少一种合法推进路径：
  所有 root step 最初可 ready；
  任意 step 的所有上游成功后，它可以 ready；
  所有 step 成功后，workflow 可以进入 terminal completed。
```

这个不变量不保证资源足够，也不保证 step 一定成功。它只保证依赖结构不会让 workflow 先天无法完成。

面试里可以这样答：

```text
我会先定义 step id 唯一、dependency 必须存在、graph 在 validation 期间不可变、边方向统一、每个节点只有 white/gray/black 三种访问状态、遇到 gray 节点必须返回稳定 cycle path。然后再选 DFS 或 Kahn。DFS 更容易报告环，Kahn 更贴近拓扑排序和 ready queue。对 workflow 来说，validation 成功只说明依赖结构可推进，不代表资源、执行和重试一定成功。
```

## Q049. cycle detection 的常见误用是什么，误用后通常会产生什么线上症状？

**回答：**

Cycle detection 的常见误用，通常是把它放错层，或者检查了不完整的图。

常见误用和症状如下：

```text
只在运行期靠 stuck 推测 cycle:
  症状是 workflow 长时间 RUNNING，报警很晚，错误信息不指向具体依赖。

只检查 depends_on，不检查数据引用:
  症状是某个 step 提前执行，ResolveArgs 找不到结果或读到空值。

把缺失 dependency 当作 cycle:
  症状是用户得到误导性错误，实际只是 step_id 拼错。

把多 root 当作非法:
  症状是合法并行 workflow 被拒绝。

把失败等待当作 cycle:
  症状是上游失败后，下游没跑被报告为 cycle。
  实际应该是 failure propagation 或 trigger rule 问题。

每次 scheduler tick 全图检测:
  症状是 scheduler CPU 高、completion 延迟高，大 workflow 影响所有 workflow。

动态图只检测初始图:
  症状是前半段正常，动态扩展后 workflow 卡住。

检测结果不稳定:
  症状是同一份定义在不同实例返回不同 cycle path，测试偶发失败。

validation 失败后仍写 metadata:
  症状是控制台能看到 workflow，但 replay 找不到合法 start event。
```

LogServe 当前最需要警惕的是第一类和第二类。由于 `ParseDefinition` 还没有 validation，如果用户提交 cycle，系统更可能表现为 running workflow 没有可调度 step，而不是直接返回"definition has cycle"。同时，`ResolveArgs` 支持 `__step_ref__`，但当前定义层没有强制这个引用必须出现在 `depends_on`。这会把数据依赖问题拖到运行期。

比较好的修复路线是：

```text
1. 在 workflow 包增加 ValidateDefinition。
2. SubmitWorkflow 写日志前调用。
3. 对 depends_on 和 __step_ref__ 建同一张依赖图或做一致性检查。
4. 返回具体错误，例如:
   - duplicate step_id: x
   - unknown dependency: b depends_on missing a
   - missing control dependency for step ref: b refs a
   - cycle detected: a -> b -> c -> a
5. 增加 stuck workflow 指标，但不要用它替代提交期 validation。
```

面试里可以这样答：

```text
最常见的误用是把 cycle detection 当成运行期诊断，而不是提交期 validation。另一个误用是只检查显式 depends_on，漏掉参数里的数据依赖。线上症状通常是 workflow 一直 RUNNING、step 没有 TaskID、scheduler 反复扫描但没有 ready node，或者下游提前执行后参数解析失败。正确做法是提交时拒绝非法图，运行期再用 stuck 指标做兜底诊断。
```

## Q050. cycle detection 在单机和分布式环境中的语义有什么差异？

**回答：**

单机环境里，cycle detection 通常很简单：把完整 graph 放在内存里，基于一个不可变快照跑 DFS 或 Kahn。只要函数返回成功，后续 scheduler 就可以认为这张图是 DAG。

分布式环境里，语义会复杂很多。不是因为找环算法变了，而是图的边界变了：

```text
单机:
  graph 一次性加载到内存。
  validation 和调度在一个进程里。
  没有跨实例版本问题。
  错误路径容易稳定输出。

分布式:
  workflow definition 可能存 DB、对象存储或配置中心。
  多个控制面实例可能同时提交或调度。
  动态节点可能由远端 worker 生成。
  graph 可能按 namespace、tenant、shard 拆分。
  validation 结果必须和 graph version 绑定。
```

如果只是单个 workflow 内部的 DAG，分布式系统也可以把 validation 收敛到一个控制面实例完成。比如提交 workflow 时，由 leader 或持有 shard ownership 的 scheduler 读取完整 definition，验证后写入 durable log。后续其他实例 replay 时信任这份 definition。

麻烦的是跨 workflow、跨 tenant、跨服务的依赖：

```text
workflow A 的 step 依赖 workflow B 的输出。
dataset X 由 pipeline P 生成，pipeline P 又读取 dataset X。
服务部署顺序依赖跨多个 repo 和环境。
```

这时 cycle detection 可能需要全局图或分层图。全局图很难，因为边可能跨存储系统，图版本不一致，权限也不一定允许一次性读取全部依赖。工程上常见做法是限制边界：单 workflow 内必须 acyclic；跨 workflow 依赖用事件、dataset version 或外部 orchestrator 管，不轻易承诺全局无环。

LogServe 当前更接近单控制面、多 worker。`workflowMu` 保护单进程内 workflow 状态推进，definition 在 `WorkflowStarted` 里持久化，worker 只执行 task，不修改 workflow graph。如果补 cycle detection，单机语义已经足够。只有未来做多 control service 实例、workflow sharding 或动态 DAG 扩展时，才需要把 validation 结果和 graph version、shard ownership、CAS/事务绑定起来。

面试里可以这样答：

```text
单机 cycle detection 是一个基于内存快照的图算法。分布式环境里，它变成 graph version 和 ownership 问题：谁有权冻结图，谁验证，验证结果和哪一版 definition 绑定，其他 scheduler 如何信任这份结果。通常我会先限定单 workflow 内必须无环，提交期由拥有该 workflow shard 的控制面验证；跨 workflow 的全局无环不要轻易承诺，除非系统真的维护全局依赖图和一致性协议。
```

## Q051. ready queue 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

Ready queue 的核心目标是保存"依赖已经满足、可以提交给执行器，但还没有完成执行"的工作项。它是 dependency resolver 和 executor 之间的缓冲层。

在 DAG workflow 里，ready queue 回答的问题是：

```text
哪些 step 的所有上游已经成功？
这些 step 是否已经被发出去执行？
executor / worker 现在可以拿哪些任务？
如果 worker 暂时处理不过来，ready work 应该在哪里排队？
```

所以 ready queue 同时解决正确性和性能问题，但优先级不同：

```text
正确性:
  一个 step 只能在依赖满足后入队。
  同一个 attempt 不能重复入队。
  上游失败、取消、超时后不能错误释放下游。

性能:
  避免每个 worker poll 都全图扫描。
  让 scheduler 批量释放 ready tasks。
  吸收短期 worker 供需波动。

可维护性:
  把“依赖判定”和“执行分发”分开。
  更容易加 backpressure、priority、fairness、observability。

安全性:
  不是它的主要职责。
  但如果队列无界，可能造成资源耗尽，这属于可用性安全。
```

Python `TopologicalSorter.get_ready()` 可以理解为一个最小形式的 ready set：初始返回所有没有前驱的节点，之后每次 `done()` 标记节点完成，再释放新的 ready 节点。Airflow scheduler 也有类似语义：它监控 task 和 DAG，依赖完成后触发 task instance，并把 ready task 交给 executor。Argo DAG 文档里的 diamond 例子也是同一个思想：A 完成后 B 和 C 并行，B/C 都完成后 D ready。

LogServe 当前没有单独叫 `ready queue` 的 workflow 数据结构，但 `scheduleReadySteps` 起到了 ready 判定和入队的作用：

```text
for each step in definition order:
  step status == SCHEDULED
  step TaskID == ""
  DependenciesSucceeded(stepDef, state)
  ResolveArgs(...)
  enqueueTask(...)
  append StepScheduled
  set TaskID / attempt / input hash
```

真正的执行队列是 control service 里的 `s.queue`，worker poll 时从里面拿 task。也就是说，LogServe 把 ready set 计算和 task queue 写入合在一个函数里了。这个简化可以工作，但面试时要说清楚：ready queue 的语义不是"所有排队任务"，而是"依赖已满足、等待执行资源的任务"。

面试里可以这样答：

```text
ready queue 的核心目标是把已经满足依赖的节点交给执行层，同时保证同一个节点不会在依赖未满足或重复完成时被提前释放。它主要是 correctness + performance 机制：correctness 保证 ready 的语义正确，performance 避免 worker 或 scheduler 每次都全图扫描。资源限制、优先级和公平性可以作用在 ready queue 上，但它们不是 ready 判定本身。
```

## Q052. ready queue 的典型适用场景和不适用场景分别是什么？

**回答：**

Ready queue 适合"工作已经具备执行条件，但执行资源有限"的场景。它不只用于 DAG，也常见于线程池、事件循环、构建系统和任务调度器。

典型适用场景包括：

```text
DAG workflow:
  上游成功后，下游 step 进入 ready queue。
  scheduler 再根据 worker、pool、priority、deadline 做分发。

构建系统:
  依赖文件或 target 都完成后，某个 target ready。
  ready target 可以交给并行编译 worker。

线程池:
  外部请求已经通过 admission control。
  task 进入 bounded ready queue，等待 worker 执行。

批处理系统:
  一批 job 已经到达执行窗口。
  ready queue 按资源和优先级释放。

actor / mailbox:
  对单个 actor 来说，mailbox 里的消息可以看作该 actor 的 ready work。
  但 actor 还要额外保证单 actor 串行状态修改。

异步 runtime:
  future 被唤醒后进入 runnable queue。
  executor 从 runnable queue 中取任务继续 poll。
```

不适用或不应该单独依赖 ready queue 的场景：

```text
没有等待关系的同步调用:
  直接调用函数即可，不需要队列。

极低延迟且任务极短:
  入队/出队/锁竞争可能比任务本身还贵。

必须强事务执行的逻辑:
  如果任务和状态更新必须在一个数据库事务里完成，简单 ready queue 不够。

需要全局最优调度:
  ready queue 只表示可执行，不表示最优。
  还需要 cost model、resource model、deadline scheduling。

无限流处理:
  stream processing 中 ready 的概念更多由 watermark、offset、backpressure 决定。
  单纯 DAG ready queue 不够表达。

不允许异步化的用户请求:
  用户必须同步得到结果时，队列只能作为内部优化，不能改变 API 语义。
```

Airflow 和 Argo 都说明了一个重要事实：ready 不等于马上运行。Airflow 的 scheduler 会在 dependencies complete 后触发 task instances，但还要考虑 executor、pool 和 concurrency limit。Argo 也有 controller-level parallelism、namespace parallelism、priority、mutex/semaphore 等机制。也就是说，ready queue 只是"可以运行"的集合，不是"现在一定运行"的承诺。

LogServe 里也一样。`scheduleReadySteps` 判断 step ready 后调用 `enqueueTask`，但 `enqueueTask` 会受 queue high watermark 等 backpressure 影响。`TestWorkflowScheduleBackpressureDoesNotLeavePhantomTaskID` 专门覆盖了这个边界：如果入队失败，step 不能留下假的 `TaskID`。这说明 ready 和 enqueued 之间也有失败点。

面试里可以这样答：

```text
ready queue 适合依赖已满足但执行资源有限的系统，比如 workflow、构建系统、线程池、批处理和异步 runtime。不适合把它当成全局最优调度器，也不适合替代事务、资源模型或流式 backpressure。ready 只说明任务可以被考虑执行，不说明它已经拿到资源，也不说明它一定会马上开始。
```

## Q053. ready queue 和相近概念最容易混淆的边界在哪里？

**回答：**

Ready queue 最容易和下面几类东西混淆：

```text
ready set vs ready queue:
  ready set 是所有依赖满足的节点集合。
  ready queue 是按某种顺序等待 dispatch 的数据结构。
  set 更强调语义，queue 更强调出队顺序。

ready queue vs worker queue:
  ready queue 关注依赖是否满足。
  worker queue 关注任务如何交给 worker。
  一个 ready task 可能还没进入某个具体 worker 的本地队列。

ready queue vs bounded queue:
  bounded queue 控制容量和 backpressure。
  ready queue 控制可执行语义。
  ready queue 可以是 bounded 的，但两者不是同一个概念。

ready queue vs priority queue:
  priority queue 决定先取谁。
  ready queue 决定谁有资格被取。
  priority 不能让未满足依赖的任务提前运行。

ready queue vs delay / retry queue:
  delay queue 表示时间未到。
  retry queue 表示失败后等待下一次尝试。
  retry 到期后还要重新检查依赖和 workflow 状态。

ready queue vs executor:
  executor 负责运行任务。
  ready queue 只是给 executor 提供候选任务。

ready queue vs topological sort:
  topological sort 可以一次性给出顺序。
  ready queue 是运行期逐步释放的 frontier。
```

这个边界在面试里很重要。很多人会说"拓扑排序后放进队列就行"，但真实 workflow engine 不是这么简单。因为节点完成、失败、重试、取消、资源限制都是运行期发生的。一次性拓扑序只能说明依赖顺序合法，不能决定每个时刻谁该入队。

LogServe 当前把 ready 判定和 task queue 写入放在 `scheduleReadySteps` 里。它先检查 step 状态和依赖，再 `enqueueTask`。这意味着 `s.queue` 里已经不是纯粹的 ready set，而是全局待 worker poll 的 task queue。对现在的单控制面实现来说这样简单；如果未来要加优先级、公平性、分布式 scheduler，最好把几个层次拆开：

```text
dependency frontier:
  哪些 step 理论上 ready。

admission / backpressure:
  现在是否允许把 ready step 变成 task。

global task queue:
  已经创建 task，等待 worker poll。

worker local queue:
  某个 worker 已经预取但还没开始。

running set:
  已经被 lease 或 started 的任务。
```

面试里可以这样答：

```text
ready queue 不是 worker queue，也不是 priority queue。ready 的语义是依赖满足；worker queue 的语义是等待执行资源；priority 只是在 ready 候选里排序；bounded queue 负责容量和 backpressure。真实系统里最好把 ready frontier、admission、global queue、worker local queue、running set 分开，否则出了重复执行或卡住问题，很难判断是哪一层的状态错了。
```

## Q054. ready queue 在高并发场景下可能出现哪些隐藏问题？

**回答：**

Ready queue 的高并发问题，核心是"释放一次"和"消费一次"都很难保证。一个上游 step 成功可能释放很多下游；多个 scheduler 或 completion 事件并发到达时，很容易重复入队或提前入队。

常见隐藏问题包括：

```text
重复入队:
  两个 scheduler 同时看到 step ready 且 TaskID 为空。
  两边都创建 task，最终同一个 step 执行两次。

提前入队:
  join step 有多个 parent。
  completion 事件并发处理时，indegree 被多扣一次，下游提前 ready。

丢失 ready:
  上游完成后，scheduler 崩溃在更新 ready queue 之前。
  如果没有 durable log 或 reconciliation，child 永远不入队。

ABA 问题:
  step 第一次 attempt 失败，TaskID 清空准备 retry。
  旧 completion 延迟到达，把新 attempt 状态覆盖。

队列风暴:
  一个 root 完成后释放上万个 child。
  queue、DB、worker poll 和日志写入同时被打满。

热点 join:
  大量 parent 都更新同一个 join 的 readiness。
  锁竞争和 metadata 写放大明显。

优先级反转:
  低优先级 fan-out 占满 ready queue。
  高优先级 workflow 的 ready task 后到但排不上。

head-of-line blocking:
  队头任务需要特殊资源，后面的普通任务也被堵住。

backpressure 状态不一致:
  task 创建成功但入队失败，或者 step 写了 TaskID 但 queue 没有 task。
```

Airflow 多 scheduler 的文档很有代表性：它允许多个 scheduler 并发运行，但在 TaskInstances 从 scheduled 进入 executor 的关键区间要用数据库行级锁保护 pool 和 concurrency limits。这说明 ready queue 不是简单的内存队列；一旦有多个调度者，"谁能把 ready 任务变成 queued/running"必须受事务或锁保护。

LogServe 当前用 `workflowMu` 把 workflow 状态推进串起来，能避免同一个进程内 `scheduleReadySteps` 和 completion 并发打架。`scheduleReadySteps` 还要求 `step.Status == SCHEDULED && step.TaskID == ""`，这能阻止已经入队的 step 被重复调度。入队失败时不留下 phantom `TaskID` 的测试也覆盖了一个关键边界。

但如果未来做多控制面实例，单进程 `workflowMu` 就不够了。至少需要：

```text
1. ready -> task creation 的数据库事务或 CAS。
2. step_id + attempt 的唯一约束。
3. StepScheduled 的幂等键。
4. completion 的 lease epoch / fencing。
5. 定期 reconciliation，从 durable workflow log 重建应有队列。
6. ready burst 的批量限制和公平调度。
```

面试里可以这样答：

```text
ready queue 高并发下最常见的问题是重复入队、丢失 ready 和提前释放 join。根因通常是 completion、ready 判定和 task enqueue 不是一个原子状态转换。单机可以用锁保护，分布式要用事务、CAS、唯一约束、lease/fencing 和 reconciliation。否则线上会看到同一 step 多个 TaskID、join 提前跑、workflow 卡住、队列突然暴涨。
```

## Q055. ready queue 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

Ready queue 最怕处在"已经决定 ready，但还没可靠入队"的中间状态。崩溃恢复时必须能回答：哪些任务已经 durable scheduled？哪些只是内存里短暂出现过？哪些 running task 需要恢复或重新投递？

典型边界条件有：

```text
ready 已计算，任务未创建:
  scheduler 崩溃。
  恢复时应该重新扫描 workflow state，重新发现 ready step。

task 创建成功，queue 写入失败:
  metadata 有 task，worker 拿不到。
  需要 log-first 或事务避免半状态。

queue 写入成功，StepScheduled 未持久化:
  worker 可能执行了一个 workflow 状态里不存在的 task。
  completion 回来后无法正确推进 step。

StepScheduled 已持久化，内存 queue 丢失:
  重启后内存队列为空。
  bootstrap 必须从 log/replay/metadata 恢复 queue。

worker 已拿到 task，control service 重启:
  task 可能正在执行，也可能 worker 已死。
  需要 lease、timeout 或 redelivery。

旧 attempt completion 延迟到达:
  retry 已创建新 task。
  旧 task 结果不能覆盖新 attempt。

workflow 被取消或失败:
  ready queue 里残留 task。
  worker poll 或 completion 时要检查 terminal 状态。

超时后重试:
  旧 task 可能后来成功返回。
  需要 fencing，不能让旧成功释放下游。
```

LogServe 的恢复路径已经体现了这些问题。`bootstrapWorkflows` 读取 `wf:` stream，`workflow.Replay` 重建状态，`prepareRetryableFailedSteps` 把未超过 max attempts 的 failed step 重新置为 SCHEDULED 并清空 `TaskID`，`restoreWorkflowTasks` 会把 SCHEDULED/STARTED 且有 `TaskID` 的 step 重新创建 task spec 并放回 `s.queue`。然后如果 workflow 仍是 RUNNING，再调用 `scheduleReadySteps` 补发没有 `TaskID` 且依赖已满足的 step。

这里的核心原则是：

```text
durable log 决定 workflow truth。
metadata 和 memory queue 都可以重建。
ready queue 不能是唯一事实来源。
```

这和 Airflow、Argo 这类系统的思路一致：调度器可以重启，但 DAG/task 状态必须在 metadata store 或 Kubernetes CRD 等持久层里，调度器根据持久状态重新计算下一步。

面试里可以这样答：

```text
ready queue 在崩溃恢复中的边界是，它不能作为唯一事实来源。真正的状态要在 durable log、DB 或 workflow history 里。重启后应该从持久状态重建：已经 StepScheduled 的任务恢复到队列，失败但可重试的 step 清空 TaskID 后重新 ready，依赖满足但还没入队的 step 重新调度。超时和 retry 要配合 lease/fencing，防止旧 attempt 的迟到 completion 覆盖新 attempt。
```

## Q056. ready queue 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

Ready queue 的瓶颈取决于实现层次。单机内存队列常见瓶颈是锁竞争和内存；workflow scheduler 里的 ready queue 常见瓶颈还包括 I/O、DB 事务和日志写入。

可以拆开看：

```text
CPU:
  反复扫描所有 step 判断 ready。
  大 fan-out/fan-in 下维护 indegree 或依赖状态。
  参数解析、input hash 计算、序列化。

内存:
  ready task 堆积。
  大量 task spec、args、result 引用占用内存。
  无界队列会把内存打满。

锁竞争:
  多 worker poll 同一个 global queue。
  多 scheduler 同时释放 ready tasks。
  join 节点状态频繁更新。

I/O:
  每次入队前后写 workflow log、metadata DB、task spec。
  大规模调度时 DB 或日志服务可能先满。

网络:
  worker 远程 poll。
  scheduler 调用远端 executor、Kubernetes API 或消息队列。
  多控制面实例之间共享状态。
```

Airflow scheduler 文档提到调优要看 CPU、内存、网络吞吐、DAG 数量和复杂度、每轮处理多少 task instance 等因素。Argo 的 parallelism 文档也说明，即使 workflow 已经可以运行，也会被 controller-level 或 namespace-level parallelism 限制。这些都说明 ready queue 的性能不能只看队列本身，还要看它连接的调度循环、资源限制和持久化系统。

LogServe 当前 `scheduleReadySteps` 的热路径大致是：

```text
1. 持有 workflowMu。
2. 扫描 definition steps。
3. 对每个候选 step 调 DependenciesSucceeded。
4. ResolveArgs，可能读取上游 ResultRef。
5. 计算 input hash。
6. enqueueTask，写 task metadata 和内存 queue。
7. append StepScheduled 到 workflow log。
8. UpdateWorkflow 写 materialized state。
```

所以 LogServe 的 ready queue 相关瓶颈不只是 `s.queue` 出入队。`workflowMu` 持锁时间、`ResolveArgs` 读取结果、append log 延迟、metadata update、queue high watermark 都可能影响调度延迟。`containsTaskID` 在恢复时对内存 queue 做线性查找，如果队列很大，也会变成恢复期成本。

优化方向通常是：

```text
小规模:
  保持简单，先加指标。

中等规模:
  用 per-workflow lock，避免全局锁。
  ready frontier 增量维护，减少全图扫描。
  enqueue 批量写日志/metadata。

大规模:
  ready queue 分片。
  按 tenant/workflow/pool 做公平调度。
  用 bounded queue 和 admission control。
  把大参数和大结果外部化，只在队列里放引用。
```

面试里可以这样答：

```text
ready queue 的性能瓶颈通常先来自锁竞争和持久化 I/O，其次才是 CPU。简单内存队列会卡在全局锁和无界内存；workflow engine 会卡在 ready 扫描、metadata 更新、日志追加、executor API 和 worker poll。评估时要把排队延迟、调度循环耗时、入队失败率、队列长度、worker 空闲率分开看，否则很难判断是 scheduler 慢还是 worker 慢。
```

## Q057. ready queue 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

Ready queue 的测试不能只测 FIFO 出入队。对 workflow engine 来说，更重要的是依赖语义、幂等、崩溃恢复和 backpressure。

Correctness test 应该覆盖：

```text
依赖释放:
  root step 初始 ready。
  A 成功后 B/C ready。
  B/C 都成功后 D ready。
  B 成功但 C 未完成时 D 不 ready。

失败语义:
  上游失败且不 retry，下游不 ready。
  上游失败但可 retry，只重新 ready 上游 attempt。
  failFast 开启时，不再释放新的 branch。

重复事件:
  同一个 StepSucceeded 重放两次，下游只入队一次。
  同一个 task completion 重复到达，不创建重复 TaskID。

状态边界:
  step 已经有 TaskID 时不重复调度。
  workflow terminal 后不再入队。
  cancellation 后 queue 中残留 task 不应继续推进 workflow。

backpressure:
  enqueue 失败不留下 phantom TaskID。
  queue 满时返回明确错误或做 admission control。

恢复:
  StepScheduled 已持久化但内存 queue 丢失，重启后恢复 task。
  failed retryable step 重启后重新 ready。
  started task 重启后能 redeliver 或等待 lease 过期。
```

Stress test 应该覆盖：

```text
大 fan-out:
  一个 root 释放大量 child，检查没有漏入队、重复入队和内存失控。

大 fan-in:
  多个 parent 并发完成同一个 join，join 只能 ready 一次。

高并发 completion:
  多 worker 同时提交结果。
  检查锁、幂等键和状态推进是否正确。

worker 慢 / scheduler 快:
  ready queue 堆积，backpressure 是否触发。

scheduler 慢 / worker 快:
  worker 空闲但 ready 未释放，观察调度延迟。

崩溃注入:
  在 enqueueTask、append StepScheduled、UpdateWorkflow 之间注入失败。
```

Benchmark 应该测：

```text
enqueue / dequeue 吞吐:
  单 worker、多 worker。
  单 scheduler、多 scheduler。

调度延迟:
  upstream success -> child enqueued 的 p50/p95/p99。

队列操作成本:
  push/pop/peek/contains 的耗时和 alloc。

扫描成本:
  每次 scheduleReadySteps 扫描 N 个 step 的成本。
  与增量 ready frontier 对比。

持久化成本:
  每个 ready task 产生多少 log append、metadata write、DB round trip。

公平性:
  多 workflow、多 tenant 下，高优先级和小 workflow 是否被大 fan-out 淹没。
```

对 LogServe，可以优先补这些测试：

```text
1. cycle/unknown dependency 被提交期拒绝后，不产生 workflow log。
2. diamond join 在 B/C 并发完成时，D 只创建一个 TaskID。
3. duplicate StepSucceeded 不重复 schedule child。
4. queue high watermark 下，不留下 phantom TaskID。
5. restart 后，已 StepScheduled 的 task 能回到 s.queue。
6. retry 后，旧 attempt completion 不能覆盖新 attempt。
```

其中第 4 点已经有测试覆盖，第 5 点也有 worker recovery 相关测试基础。后续可以围绕 fan-in 并发和 cycle validation 加强。

面试里可以这样答：

```text
correctness test 重点测依赖释放、重复事件、失败语义、terminal 状态和 backpressure；stress test 测大 fan-out/fan-in、高并发 completion、worker/scheduler 速率不匹配和崩溃注入；benchmark 测 enqueue/dequeue 吞吐、upstream success 到 child enqueue 的延迟、扫描成本、持久化成本和公平性。ready queue 不是普通 FIFO，测试必须围绕 workflow 状态推进来设计。
```

## Q058. 如果要求从零实现一个简化版 ready queue，你会先定义哪些不变量？

**回答：**

从零实现 ready queue，我会先定义状态机不变量。否则队列看起来能 push/pop，但一接入 workflow 就会出现重复执行、提前执行或丢任务。

核心不变量如下：

```text
ready 资格不变量:
  step 只有在所有控制依赖都处于 SUCCEEDED 后才能 ready。
  如果有 trigger rule，要把 trigger rule 纳入 ready 条件。
  数据依赖的结果必须可解析，或者至少有有效 ResultRef。

单次入队不变量:
  同一个 workflow_id + step_id + attempt 只能入队一次。
  step 已有 TaskID 时不能再次创建 task。
  retry 必须增加 attempt 或使用新的 fencing token。

状态转换不变量:
  SCHEDULED(no TaskID) -> QUEUED/StepScheduled(with TaskID)
  QUEUED -> STARTED
  STARTED -> SUCCEEDED/FAILED/TIMED_OUT/CANCELLED
  terminal step 不能回到 ready，除非显式 retry 且状态被重置。

持久化不变量:
  durable state 是事实来源。
  内存 queue 可以丢，但必须能从 log/DB 重建。
  queue 中的 task 必须能找到对应 task spec 和 workflow step。

消费不变量:
  出队不等于完成。
  worker 拿到 task 后要有 lease 或 visibility timeout。
  lease 过期前旧 worker 的 completion 必须带 fencing 信息。

backpressure 不变量:
  queue 满时不能写一半状态。
  admission 失败不能留下 phantom TaskID。
  被拒绝的 ready step 后续仍能被重新发现。

公平性不变量:
  一个 workflow 的 fan-out 不能永久压住其他 workflow。
  priority 只能在 ready task 之间排序，不能破坏依赖。
```

一个简化实现可以分三层：

```text
WorkflowState:
  保存每个 step 的 status、attempt、TaskID、remainingDeps。

ReadyIndex:
  保存当前 ready 但尚未 enqueued 的 step。
  可以按 workflow、priority、created_at 分桶。

TaskQueue:
  保存已经创建 TaskID、等待 worker poll 的 task。
  可以是 bounded queue，并带 idempotency key。
```

伪代码可以这样写：

```text
onWorkflowStarted(def):
  validate DAG
  for each step:
    remainingDeps[step] = number of dependencies
  for root step:
    markReady(step)

markReady(step):
  if workflow terminal: return
  if step.status != SCHEDULED: return
  if step.taskID != "": return
  if readyIndex already contains step/attempt: return
  readyIndex.add(step/attempt)

dispatch():
  while capacity available:
    item = readyIndex.popFair()
    if stillReady(item):
      create task id
      persist StepScheduled
      enqueue task

onStepSucceeded(step):
  if duplicate success: return
  persist success
  for child in children[step]:
    remainingDeps[child]--
    if remainingDeps[child] == 0:
      markReady(child)
```

这里有一个细节：`remainingDeps` 是优化，不是事实来源。崩溃恢复时应该从 durable step statuses 重新计算，而不是只相信内存计数。否则一次重复 completion 就可能把 indegree 扣错。

LogServe 当前没有显式 `remainingDeps`，而是每次 `scheduleReadySteps` 用 `DependenciesSucceeded` 重新检查。这种做法简单且容易恢复，代价是每次扫描成本更高。对当前项目规模，这是合理的。如果未来 workflow 很大，可以再引入增量 ready frontier，但要配幂等和 replay 校验。

面试里可以这样答：

```text
我会先定义 ready 资格、单次入队、状态转换、持久化、消费、backpressure 和公平性不变量。最重要的是：ready 不能破坏依赖，入队必须幂等，出队不等于完成，内存 queue 不能是唯一事实来源。简化版可以先用全图扫描判断 ready，等规模上来后再维护 remainingDeps 和 ready index，但这些增量结构必须能从 durable state 重建。
```

## 参考资料

- Apache Airflow Documentation: [Dags](https://airflow.apache.org/docs/apache-airflow/stable/core-concepts/dags.html), [XComs](https://airflow.apache.org/docs/apache-airflow/stable/core-concepts/xcoms.html)
- Apache Airflow Documentation: [Scheduler](https://airflow.apache.org/docs/apache-airflow/stable/administration-and-deployment/scheduler.html)
- Apache Airflow Documentation: [Dynamic Task Mapping](https://airflow.apache.org/docs/apache-airflow/stable/authoring-and-scheduling/dynamic-task-mapping.html)
- Python Documentation: [graphlib.TopologicalSorter](https://docs.python.org/3/library/graphlib.html)
- Argo Workflows Documentation: [DAG](https://argo-workflows.readthedocs.io/en/latest/walk-through/dag/), [Artifacts](https://argo-workflows.readthedocs.io/en/latest/walk-through/artifacts/), [Retries](https://argo-workflows.readthedocs.io/en/latest/retries/), [Loops](https://argo-workflows.readthedocs.io/en/latest/walk-through/loops/), [Limiting parallelism](https://argo-workflows.readthedocs.io/en/latest/parallelism/), [Offloading Large Workflows](https://argo-workflows.readthedocs.io/en/latest/offloading-large-workflows/)
- Temporal Documentation: [Workflow Definition](https://docs.temporal.io/workflow-definition), [Event History](https://docs.temporal.io/encyclopedia/event-history), [Activities](https://docs.temporal.io/activities), [Go SDK Side Effects](https://docs.temporal.io/develop/go/workflows/side-effects), [Go SDK Continue-As-New](https://docs.temporal.io/develop/go/workflows/continue-as-new), [Go SDK Versioning](https://docs.temporal.io/develop/go/workflows/versioning), [Go SDK workflow cancellation](https://docs.temporal.io/develop/go/workflows/cancellation), [Retry Policies](https://docs.temporal.io/encyclopedia/retry-policies)
- Dagster Documentation: [Defining assets](https://docs.dagster.io/guides/build/assets/defining-assets), [Jobs](https://docs.dagster.io/guides/build/jobs)
- Google Research: [MapReduce: Simplified Data Processing on Large Clusters](https://research.google/pubs/mapreduce-simplified-data-processing-on-large-clusters/)
- OpenTelemetry Documentation: [Signals](https://opentelemetry.io/docs/concepts/signals/)
- Object Management Group: [BPMN 2.0.2 specification page](https://www.omg.org/spec/BPMN/2.0.2/About-BPMN)
- LogServe 本地实现：`README.md` 的 Workflow Semantics，`internal/workflow/model.go`，`internal/workflow/args.go`，`internal/control/service.go`，`internal/worker/agent.go`，`tests/integration/workflow_execution_test.go`
