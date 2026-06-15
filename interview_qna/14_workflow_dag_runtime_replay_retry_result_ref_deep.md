# 五、Workflow DAG Runtime、Replay、Retry 与 Result Ref（深度）

## Q331. Python tracing 的局限是什么？哪些 Python 控制流无法可靠转成 DAG？

LogServe 里的 Python tracing 不是通用 Python 编译器，它更接近一次“带记录的执行”。用户调用 `@workflow` 函数时，SDK 会在 trace context 里真正跑一遍 Python 函数。被 `@task` 包住的函数不会真的执行，而是返回 `StepRef`，SDK 顺便把这次 task 调用记录成一个 DAG step。

这个做法的优点是实现直接，用户写出来的 workflow 看起来也像普通 Python 代码。但它有一个清晰边界：只能记录这一次 trace 实际走到的控制流。

比较稳的场景是：

- 普通顺序调用：`a = f(x); b = g(a); return b`
- 基于普通输入参数的分支：`if query_type == "rag": ...`
- 基于普通整数或列表的循环：`for item in items: ...`
- task 参数中嵌套 `StepRef` 的 list、dict、tuple

不可靠的场景主要是这些：

- 分支条件依赖上游 task 结果，例如 `if classify(x): ...`。`classify(x)` 在 trace 时只是 `StepRef`，不是实际结果，SDK 无法知道运行时该走哪个分支。
- 循环次数依赖上游 task 结果，例如 `for doc in search(query): ...`。`search(query)` 还没执行，DAG 无法展开。
- workflow body 里夹杂真实副作用，比如写文件、发请求、修改数据库。trace 本身会执行这些普通 Python 语句，容易产生一次“提交前副作用”。
- 动态反射调用、运行时拼函数、闭包里生成 task、根据全局状态选择 task。这些东西可以在 Python 里跑，但很难稳定地转成可重放、可审计的 DAG。

所以我在介绍这个 DSL 时不会说它能覆盖任意 Python。它适合表达“提交时能静态展开的 DAG”，对依赖运行时结果的动态 workflow，需要以后加动态 step 提交或者子 workflow 机制。

## Q332. 如果 workflow 中有 if/for 动态分支，当前 DSL 如何表现？

当前 DSL 的行为取决于分支条件和循环边界在 trace 时是不是普通 Python 值。

如果条件来自 workflow 入参，比如：

```python
@workflow
def route(query: str, use_rag: bool):
    if use_rag:
        docs = search(embed(query))
        return generate(query, docs)
    return direct_answer(query)
```

这类代码可以工作。因为 `use_rag` 是提交时已知的普通布尔值，trace 会只记录本次提交走到的那条分支。换句话说，每次提交都会得到一个具体 DAG。

如果循环来自普通输入，也可以工作：

```python
@workflow
def batch_embed(texts: list[str]):
    refs = []
    for text in texts:
        refs.append(embed(text))
    return merge(refs)
```

这会生成和 `texts` 长度一致的多个 step。

真正麻烦的是分支或循环依赖上游 task 结果：

```python
@workflow
def bad_case(query: str):
    intent = classify(query)
    if intent == "rag":
        ...
```

这里的 `intent` 是 `StepRef`，不是真实分类结果。Python 的 `if` 语句不会等到 worker 执行完 `classify`，SDK 也没有机会在运行时重新展开 DAG。因此这类动态控制流目前不应该写进 DSL。

工程上我会把这个限制明确写进文档：当前 workflow 是提交时展开 DAG，不是运行时持续解释 Python。后续如果要支持动态 DAG，需要把“step 完成后追加新 step”做成一等能力。

## Q333. StepRef 嵌套在 list/dict 中时如何发现依赖？

SDK 在构造 step 参数时会递归扫描参数结构。大致逻辑是：如果某个值是 `StepRef`，就把它编码成一个特殊 JSON 对象，同时把对应的 `step_id` 加进 `depends_on`。

例如：

```python
vec = embed(query)
docs = search({"vector": vec, "top_k": 5})
```

`search` 的参数里并不是直接传 `vec`，而是传了一个 dict。SDK 会进入 dict 内部，发现 `vector` 位置上是 `StepRef("embed")`，于是把参数编码成类似：

```json
{"vector": {"__step_ref__": "embed"}, "top_k": 5}
```

同时 `search` 这个 step 的 `depends_on` 会包含 `embed`。

当前实现支持递归处理常见容器：

- list
- tuple
- dict
- 直接的 StepRef
- 普通 JSON 值

这个设计让用户可以自然地把上游结果放在复杂参数结构里，不需要手写依赖列表。依赖推导由 SDK 完成，控制面只需要看 `depends_on` 判断哪个 step ready。

边界也很明确：参数最终必须能被 JSON 表示。自定义对象、打开的文件句柄、生成器、数据库连接这类对象不适合作为 workflow 参数。

## Q334. inspect.getsource 获取源码有什么限制？

`inspect.getsource` 是一个实用方案，但不是一个完全可靠的代码打包方案。

它比较适合普通 `.py` 文件里的函数，例如：

```python
@task
def embed(query: str):
    ...
```

但下面这些场景容易出问题：

- 函数来自 REPL、Notebook、动态 `exec`，没有稳定源码文件。
- 函数被多层 decorator 包住，`inspect` 取到的是 wrapper，而不是用户真正想执行的函数。
- 源文件已经被修改，但当前 Python 进程里加载的是旧函数对象。
- 函数定义在 zip 包、编译产物、特殊 import loader 里，源码不可读。
- 函数依赖同文件里的 helper、常量、import，仅仅截取函数体不够 worker 执行。

所以项目里后来更偏向把所在模块源码一起提交，而不是只提交单个函数源码。这能解决一部分 helper 缺失问题，但也带来更大的 payload 和更强的环境假设。

面试时我会这样讲：当前 SDK 的源码捕获是为了让 demo 和本地实验能闭环，不是生产级代码分发系统。真正生产化要引入 artifact 打包、依赖锁定、签名校验和执行环境隔离。

## Q335. module_source 替换 function_source 有什么好处和风险？

把 `module_source` 放进 task spec 的好处是 worker 拿到的不只是一个函数体，而是整个 Python 模块。这样 task 函数可以引用同文件里的 helper、常量和 import，执行成功率比单独提交函数源码高。

好处主要有三个：

- 上下文更完整。函数里调用 `helper()` 时，worker 也能拿到 `helper` 的定义。
- 用户体验更接近“写一个普通 Python 文件”，不用把所有逻辑塞进单个函数。
- fingerprint 更能反映真实执行代码。模块源码变了，提交出来的定义 hash 也会跟着变。

风险也很明显：

- payload 变大，日志和控制面传输压力更高。
- 模块里可能有无关代码，甚至包含 token、路径、实验参数等不该下发的内容。
- import 或模块顶层语句如果有副作用，worker 加载时也可能执行。
- 同一个模块里改了无关函数，也会改变提交 fingerprint，导致幂等判断更保守。
- 依赖包还是不在源码里。模块源码解决不了第三方依赖环境问题。

所以它是一个合理的实验阶段取舍：比单函数源码更可用，但离生产级 artifact 还有距离。长期应该把 Python 代码作为受控包上传，TaskSpec 里放 artifact 引用和校验摘要。

## Q336. workflow definition 的 fingerprint 应该包含哪些字段？

fingerprint 的目标是判断“两次提交是否代表同一个 workflow 定义和同一批输入”。它不能只看 workflow 名字，也不能只看 idempotency key。

合理的 fingerprint 至少应该覆盖：

- workflow name
- workflow 顶层源码或模块源码
- workflow 入参 `args_json`
- result step id
- step 列表及顺序
- 每个 step 的 step_id
- 每个 step 的 task_name、function_name
- 每个 step 的函数源码或模块源码引用
- 每个 step 的参数 JSON
- 每个 step 的 depends_on
- retry 配置，例如 max_attempts
- timeout 配置
- LLM 相关字段，例如模型名、模型标识、提示词、调度策略

当前实现的思路是把 workflow definition 做规范化 JSON，再和 workflow name 一起 hash。这样做比较保守：只要定义结构、代码、参数或依赖发生变化，fingerprint 就不同。

如果同一个 idempotency key 携带不同 fingerprint，控制面应该把它当成冲突，而不是悄悄返回旧 workflow。否则用户以为提交了新逻辑，系统却复用了旧结果，这是很危险的。

## Q337. input_hash 如何计算？如果依赖 step 的结果很大，如何避免重复读取？

`input_hash` 是按 step 的“解析后输入”算出来的。

流程大概是：

1. 读取 step 的 `args_json`。
2. 找到其中的 `{"__step_ref__": "xxx"}`。
3. 从 workflow state 里取上游 step 的结果。
4. 如果上游结果是 inline JSON，就直接使用。
5. 如果上游结果只有 `result_ref`，就从 result store 加载对象。
6. 把 StepRef 替换成真实结果，得到 resolved args。
7. 对 resolved args 做 JSON 编码，再算 SHA-256。

这样算出来的 hash 代表这个 step 本次真正看到的输入。它和 `workflow_id + step_id` 一起构成 step 级去重基础。

大结果的麻烦在第 5 步。如果上游结果很大，每次调度下游 step 都加载完整对象，会带来对象存储读放大。

可以优化的方向有几个：

- result store 在写入对象时同时记录内容 hash，metadata 里保存 `result_ref + content_hash`。
- input_hash 计算时优先使用内容 hash，而不是反复读取大对象。
- 对同一个 workflow 内刚加载过的 result_ref 做短生命周期缓存。
- 把 resolved args 中的大对象改成引用传递，让真正需要读取内容的 worker 去拉取。

当前实现更偏正确性优先：能从 inline 或 ref 还原真实输入，但还没有把大对象 hash 缓存做得很彻底。

## Q338. ResolveArgs 如何把 StepRef 替换为上游结果？

`ResolveArgs` 做的是 workflow 调度前的参数解析。SDK 在提交时把上游引用编码成 `{"__step_ref__": step_id}`，控制面调度 step 时再把它替换成真实结果。

例如提交时的参数是：

```json
{"query": "x", "docs": {"__step_ref__": "search"}}
```

当 `search` 已经成功以后，`ResolveArgs` 会从 workflow state 里找到 `search` step，然后拿到它的结果。结果可能有两种形式：

- `ResultJSON`：小结果直接存在事件或 metadata 里。
- `ResultRef`：大结果存在 result store，事件里只放引用。

拿到结果以后，`ResolveArgs` 会把 `{"__step_ref__": "search"}` 替换成真实 JSON 值，最后返回：

- resolved args JSON：给 worker 执行 task。
- input hash：用于 step 幂等。

如果依赖 step 不存在、还没成功、结果引用加载失败，`ResolveArgs` 应该返回错误，调度器不能把这个 step 交给 worker。

## Q339. 如果上游结果只有 result_ref，没有 inline result，如何加载？

当上游结果超过 inline 阈值时，控制面不会把完整结果塞进 workflow 事件，而是先写到 result store，再在事件里记录 `result_ref`。

下游 step 需要这个结果时，`ResolveArgs` 会通过 result loader 按 `result_ref` 读取对象。读取成功后，再把对象内容反序列化成 JSON 值，替换掉参数里的 StepRef。

这个路径有两个重要含义：

- workflow log 仍然是 source of truth 的索引。它知道哪个 step 产生了哪个 result_ref。
- result store 是实际大对象的承载层。它丢了对象，日志还能 replay 出“应该有这个对象”，但不能凭空恢复对象内容。

所以 result store 和 shared log 之间不是强事务关系。更稳的做法是：result store 对象写入后带内容 hash，replay 或读取时校验；后台定期扫描日志中的 result_ref，发现缺失对象就报警或标记 workflow 需要人工处理。

## Q340. scheduleReadySteps 持有 workflowMu 的原因是什么？

`scheduleReadySteps` 会检查 workflow state，找出可以运行的 step，然后创建对应 task。这里必须防止多个 goroutine 同时调度同一个 workflow，把同一个 step 调度出多份 task。

持有 `workflowMu` 的主要原因是保护这些操作的原子性：

- 检查 step 当前状态。
- 判断依赖是否全部成功。
- 判断 step 是否已经有 task_id。
- 计算下一次 attempt。
- 创建 TaskSpec。
- 写 step 调度事件。
- 更新 workflow metadata 中的 step task_id、attempt、input_hash。

如果不加锁，两个并发路径可能都看到“step ready 且没有 task_id”，然后各自提交一个 task，后面就要靠更多去重和 lease fencing 来兜底。

不过这也带来一个代价：`workflowMu` 是比较粗的锁。它能让实现简单可靠，但会限制多个 workflow 的并发调度。长期可以改成 per-workflow lock，甚至把 workflow 调度做成按 workflow_id 分片的 actor 化调度器。

## Q341. workflowMu 会不会限制多个 workflow 并发调度？

会。当前 `workflowMu` 是控制面里保护 workflow 调度和状态推进的粗粒度锁。如果大量 workflow 同时有 step ready，调度路径会串行进入这把锁。

这在实验规模下问题不大，因为它让状态推进更容易推理，也降低了并发 bug 风险。但如果 workflow 数量很大，瓶颈会比较明显：

- 多个互不相关的 workflow 也要排队调度。
- 某个 workflow 的 result_ref 加载慢，可能拖住其他 workflow 的调度。
- append log 或 metadata 更新慢，也可能延长锁持有时间。

更好的演进方向是：

- 按 workflow_id 建 per-workflow lock。
- 把 `ResolveArgs` 中可能慢的对象读取移出全局锁范围。
- 调度器只在提交状态变更时短暂加锁。
- 对每个 workflow stream 做单独调度循环，天然保持单 workflow 内顺序。

面试里我会承认这个锁粒度是当前实现的工程取舍：优先保证语义正确，再用 benchmark 判断是否需要细化。

## Q342. StepScheduled 事件和 TaskSubmitted 事件的顺序为什么重要？

这两个事件分别写在不同语义层：

- `TaskSubmitted` 属于 task stream，表示控制面已经创建了一个可执行任务。
- `StepScheduled` 属于 workflow stream，表示某个 workflow step 已经绑定到这个 task。

顺序很重要，因为重启 replay 时要靠事件恢复状态。如果顺序处理不好，可能出现两类问题：

- task stream 里有任务，但 workflow stream 不知道这个 step 已经调度过，容易产生“孤儿 task”。
- workflow stream 里说 step 已经调度，但 task stream 里没有对应 task，worker 无法执行，step 卡住。

当前代码路径里，调度 ready step 时会先通过 task 提交流程写 `TaskSubmitted`，然后再写 `StepScheduled`。这样做能保证 task 已经存在后再把 step 绑定过去，但中间如果 `StepScheduled` 写失败，就可能留下一个 task 层面的记录，而 workflow replay 看不到绑定关系。

更严谨的设计可以考虑把“step scheduled + task submitted”做成一个事务性 append，或者先写 workflow intention，再由恢复逻辑补齐 task。当前实现已经通过 log-first 和重启恢复覆盖了主路径，但这条跨 stream 原子性仍然是后续值得加强的点。

## Q343. 如果 TaskSubmitted 成功但 StepScheduled 失败，重启后如何恢复？

这是一个典型的跨 stream 原子性边界。

如果 `TaskSubmitted` 已经写入 task stream，说明 task 层能 replay 出这个任务。但 `StepScheduled` 没写进 workflow stream，workflow replay 时就不知道该 step 已经绑定过这个 task。

当前系统的恢复会更信任 workflow stream 来重建 workflow step 状态。因此可能出现：

- task metadata 里能看到一个任务。
- workflow state 里这个 step 仍然像未调度。
- 调度器重启后可能再次给该 step 创建新 task。

后面通过 step success 的幂等键、task lease epoch 和 terminal 状态保护，可以避免最终结果被重复写坏，但中间可能多执行一次任务。

更理想的修复方式是：

- 把这两个事件合并成一个控制面原子操作。
- 或者在 workflow stream 先写 `StepScheduleRequested`，replay 时如果缺 task，就补交 task。
- 或者在 bootstrap 时扫描 task spec 中的 `workflow_id + step_id`，反向修复 workflow step 的 task_id。

这也是我不会把当前实现说成严格 exactly-once 的原因之一。它的目标是最终状态正确和结果层去重，而不是保证 task 物理执行只发生一次。

## Q344. 如果 StepScheduled 成功但 metadata 更新失败，重启后如何恢复？

这个场景是 log-first 语义能覆盖得比较好的情况。

只要 `StepScheduled` 已经进入 workflow stream，即使 metadata 更新失败，重启时 `BootstrapFromLog` 也能读 workflow stream，replay 出这个 step 已经绑定到哪个 task、attempt 是多少、input_hash 是什么。

也就是说：

- log 是 source of truth。
- metadata view 是可重建的查询视图。
- metadata 更新失败不会让已确认事件丢失。

恢复后控制面会把 replay 出来的 workflow state upsert 回 metadata。只要 task stream 中对应 task 也存在，worker 就可以继续执行或者由 redelivery 重新投递。

这个例子正好说明为什么项目坚持先 append log，再更新 metadata。如果顺序反过来，metadata 成功但 log 失败，重启以后状态就没有可信来源。

## Q345. StepStarted 是否一定会发生？如果 worker 在 StartTask 前崩溃呢？

`StepStarted` 不一定发生。

正常路径是 worker poll 到 task 后，先向控制面调用 `StartTask`。控制面验证 task 和 lease 后，会把 workflow step 标成 started，并写入 `StepStarted` 事件。

但如果 worker 在这些时间点崩溃，情况会不同：

- worker poll 到 task 后，还没调用 `StartTask` 就崩溃：workflow 只看到 `StepScheduled`，看不到 `StepStarted`。
- worker dispatch 到本地队列后，还没真正开始执行就崩溃：同样可能没有 `StepStarted`。
- `StepStarted` 已经写入，但 worker 后续崩溃：重启恢复时该 task 会按 lease/redelivery 机制重新排队。

所以 `StepStarted` 是观测和状态推进事件，不是每个 step 必然存在的事件。判断 step 是否最终完成，不能依赖它一定出现，而要看 `StepSucceeded`、`StepFailed` 和 task lease 状态。

## Q346. StepSucceeded 的 idempotency key 为什么不带 task_id？

`StepSucceeded` 表示的是“某个 workflow step 在某个输入下已经产生成功结果”，它的幂等范围应该是逻辑 step，而不是某一次物理 task attempt。

所以 key 里放：

- workflow_id
- step_id
- input_hash
- succeeded

而不放 task_id。

这样设计是为了处理 redelivery 和 retry 下的重复完成。如果同一个 step、同一份输入，因为 worker 超时或网络抖动被执行了两次，两个 task_id 可能不同，但它们在 workflow 语义上代表同一个 step 的成功结果。此时应该只接受一个最终成功事件。

如果把 task_id 放进成功 key，重复执行的两个 task 都能写出各自的 `StepSucceeded`，workflow stream 里会出现多个成功结果，后面 final result 选择和 replay 都会变复杂。

当然，这不代表用户函数只执行了一次。它只保证结果提交层按逻辑 step 去重。

## Q347. StepFailed 的 idempotency key 为什么带 task_id？

失败事件和成功事件的语义不一样。

`StepSucceeded` 要表达的是逻辑 step 的最终结果，所以按 `workflow_id + step_id + input_hash` 去重。

`StepFailed` 表达的是某一次 attempt 的失败证据。retry 时，同一个 step 可能会失败多次，每次失败都应该被记录下来，方便 replay 重建 attempts、错误信息和延迟指标。

因此失败 key 带 task_id 是合理的：

- 第一次 attempt 失败，要记录。
- 第二次 attempt 失败，也要记录。
- 同一个 task_id 的失败事件因为重试 append 或网络重发重复到达时，应该去重。

这能同时满足两件事：保留每次 attempt 的失败历史，又避免同一次失败被重复写多遍。

## Q348. retry 时为什么 task dispatch key 要加 attempt？

retry 生成的是新的物理 task，它虽然属于同一个 workflow step，但不是上一轮 task 的重复提交。

如果 dispatch key 不带 attempt，那么第一次失败后再次提交同一个 step 时，控制面可能把 retry 当成重复提交，直接返回已有 task，导致 step 无法真正重试。

加上 attempt 后，key 形如：

```text
workflow_id:step_id:input_hash:attempt:N
```

这样：

- 同一次 attempt 的重复提交会被幂等去重。
- 不同 attempt 会生成不同 task，可以正常重试。
- replay 时可以根据 step attempts 判断当前已经尝试了几次。

这个设计把“重复请求”和“有意重试”区分开了。

## Q349. 如果失败后 retry，但旧 attempt 后来成功，如何避免污染最终结果？

这个问题靠两层机制处理。

第一层是 task lease epoch。task 被 redelivery 或重新 lease 后，新的执行拥有新的 lease epoch。旧 worker 如果后来才提交 completion，控制面会校验 `worker_id + lease_epoch`，发现它不是当前有效 lease，就拒绝 stale completion。

第二层是 workflow step 的 terminal 状态和成功事件幂等。如果某个 step 已经成功，再来的重复成功不会覆盖最终结果。如果某个 step 已经进入失败终态，后续旧 attempt 的完成也不应该把它拉回成功。

retry 场景里最关键的是：旧 attempt 的 completion 不能只靠 task_id 被接受，必须和当前 lease 绑定。否则 redelivery 后两个 worker 都可能写 terminal event。

所以当前语义不是“任务只执行一次”，而是“过期执行不能改变最终状态，重复成功不能写出第二份最终结果”。

## Q350. workflowDone 的判断条件是什么？

`workflowDone` 的判断非常直接：workflow 定义里的所有 step 都处于 `SUCCEEDED`。

它不会只看 result step 是否成功。原因是一个 workflow 的 DAG 里可能存在并行分支、清理 step、审计 step 或者多个中间结果。如果只看 result step，可能有些已调度 step 还没完成，workflow 就被提前标成 completed。

当前实现更保守：遍历 `StepOrder`，只要有一个 step 不是 succeeded，workflow 就还没 done。

这个选择的好处是状态语义清晰。缺点是如果用户写了不会影响最终结果的旁路 step，它也会阻塞 workflow completed。长期可以支持“result-only completion”或“optional step”，但那需要在定义里显式标注。

## Q351. completeWorkflow 如何选择 final result？

workflow definition 里有一个 `result_step_id`。这个字段来自 Python workflow 函数的返回值。当前 SDK 要求 workflow 最终返回一个 `StepRef`，这个 StepRef 指向哪个 step，哪个 step 就是 final result step。

当所有 step 成功后，`completeWorkflow` 会：

1. 找到 `result_step_id` 对应的 step state。
2. 读取这个 step 的结果。
3. 如果结果很小，可以内联保存。
4. 如果结果较大，就写入 result store，并在 `WorkflowCompleted` 事件里放引用。
5. 更新 workflow metadata 为 completed。

这样 final result 的来源是显式的，不靠“最后一个 step”这种不稳定规则。并行 DAG 里谁最后完成是不确定的，所以必须用 `result_step_id`。

## Q352. 如果 final result 很大，WorkflowCompleted 存 inline 还是 ref？

大结果应该存 ref，不应该直接塞进 workflow log。

当前控制面有 inline 阈值。结果小的时候可以放 inline JSON，读取方便；超过阈值时写入 result store，`WorkflowCompleted` 事件只记录 `result_ref`。

这样做有几个原因：

- shared log 保持轻量，replay 不用反复搬运大对象。
- 大对象更适合对象存储或本地 result store，后续可以做生命周期管理。
- 事件里保留 ref，仍然能解释状态历史。

但这也引入一个边界：log 和 result store 不是同一个原子存储。如果 ref 指向的对象丢了，replay 能知道 workflow 已经完成，也知道对象应该在哪里，但无法恢复对象内容。因此 result store 最好配合内容 hash、引用扫描和对象完整性校验。

## Q353. workflow replay 如何重建 step attempts、latency、result_ref？

workflow replay 会按 workflow stream 的事件顺序重放。

主要事件含义是：

- `WorkflowStarted`：创建 workflow state 和定义。
- `StepScheduled`：记录 step 绑定的 task_id、attempt、input_hash。
- `StepStarted`：记录开始时间，把 step 推到 started。
- `StepSucceeded`：记录结果、result_ref、完成时间、latency，把 step 推到 succeeded。
- `StepFailed`：记录错误、attempt 失败信息、完成时间、latency。
- `WorkflowCompleted`：恢复 workflow final result 或 final result ref。
- `WorkflowFailed`：恢复 workflow 失败状态和错误原因。

attempts 可以从 `StepScheduled` 或失败/成功事件中恢复。latency 可以从事件 payload 里已有字段恢复，也可以在 started/completed timestamp 都存在时重新计算。result_ref 来自 success 或 workflow completed 事件。

replay 的目标不是重新执行 Python 代码，而是从事件历史重建控制面状态。只要事件完整，metadata view 就可以被重建出来。

## Q354. 如果 replay 出来的 state 与 metadata 不一致，你如何 debug？

我会先把它当成 materialized view drift，而不是先怀疑 log。因为项目的设计里 log 是 source of truth，metadata 是可重建视图。

排查步骤一般是：

1. 读 workflow stream，从 seq 1 开始完整 replay。
2. 看 replay 结果里的 workflow status、step status、attempts、task_id、result_ref。
3. 对比 metadata view 中同一个 workflow 的字段。
4. 找到第一个不一致的 step 或 terminal event。
5. 再去 task stream 查对应 task_id 的 TaskSubmitted、TaskStarted、TaskCompleted、TaskFailed。
6. 如果涉及大结果，检查 result_ref 对象是否存在、大小和内容 hash 是否匹配。
7. 如果涉及 retry，检查 lease_epoch 是否有 stale completion。

常见原因包括：

- log append 成功后 metadata 更新失败。
- 控制面重启前 metadata 只更新了一半。
- 跨 stream 事件之间缺少原子性，task stream 和 workflow stream 有短暂不一致。
- result store 对象缺失，导致 replay 能恢复 ref 但不能恢复内容。
- 代码升级后 replay 逻辑对旧 payload 兼容不够。

修复方式应该是从 log 重建 metadata，而不是手工改 metadata。只有当 log 自身损坏时，才需要进入人工恢复路径。

## Q355. workflow timeout 是 step 级还是 workflow 级？当前实现支持全局 workflow timeout 吗？

当前主要是 step 级 timeout。

Python SDK 里可以给 task 或 workflow wrapper 配置 timeout。提交后，workflow 顶层 timeout 会作为 step 默认 timeout；具体 step 如果自己有 timeout，就用自己的配置。worker 执行 task 时按 task timeout 控制执行时间。

这解决的是“某个 step 不能无限跑”的问题。

但它不等于全局 workflow timeout。全局 timeout 指的是：从 workflow 提交开始，整个 DAG 必须在某个 wall-clock deadline 前完成，否则 workflow 失败。当前实现没有完整的一等全局 deadline 机制。

如果要支持全局 timeout，需要增加：

- workflow started_at + deadline 字段。
- 控制面定期扫描超时 workflow。
- 超时后写 `WorkflowFailed` 或 `WorkflowTimedOut`。
- 调度 ready step 前检查 deadline。
- worker completion 到达时，如果 workflow 已超时，要拒绝或忽略。

所以现在可以说：支持 step 执行超时，不完整支持全局 workflow 超时。

## Q356. 如果一个 step 永远失败，workflow 什么时候进入 failed？

每个 step 有 `max_attempts`。当 step 执行失败时，控制面会写 `StepFailed`，然后判断当前 attempts 是否还小于 `max_attempts`。

如果还没到上限：

- step 状态回到 scheduled。
- 清掉当前 task_id。
- 调度器后续会重新提交下一次 attempt。

如果已经达到上限：

- workflow 会进入 failed。
- 控制面写 workflow failed 事件。
- metadata 中 workflow 状态变成 failed。
- 后续 step 不再继续调度。

默认情况下，step 的 max_attempts 来自 workflow 顶层配置。如果 step 自己配置了更具体的值，就用 step 自己的值。

这个策略避免了无限 retry。失败会留下每次 attempt 的事件，方便 replay 和排查。

## Q357. 如果 max_attempts 设置为 0 或负数，系统应该如何处理？

从用户语义上讲，`max_attempts <= 0` 不应该表示“永远不执行”。更合理的处理方式是把它当成未设置，然后使用默认值。

当前定义解析里就是这个方向：workflow 顶层 max_attempts 如果小于等于 0，会被默认成 3；step 自己的 max_attempts 如果小于等于 0，会继承 workflow 默认值。

这样可以避免一个常见坑：用户忘了填 retry 配置，结果 step 被解析成 0 次尝试，workflow 永远无法执行。

如果要更严格，也可以在 SDK 层直接校验，发现负数就报错。但控制面仍然应该做兜底归一化，因为不能完全信任客户端。

## Q358. 如果两个 dependent step 同时 ready，调度顺序是否影响结果？

对一个设计良好的 DAG 来说，不应该影响最终结果。

如果两个 step 的依赖都已经成功，且它们之间没有依赖关系，那么它们可以并行调度。谁先拿到 task_id、谁先执行完成，只会影响时间线，不应该影响最终 workflow result。

当前实现会按 workflow definition 里的 step 顺序扫描并调度 ready step。因此调度顺序是稳定的，但执行完成顺序仍然取决于 worker、队列和任务耗时。

有几个前提要注意：

- task 函数应该尽量是纯函数，或至少对同一输入幂等。
- 并行 step 不应该写同一个外部资源，除非外部资源自己有并发控制。
- final result 由 `result_step_id` 决定，不由“最后完成的 step”决定。

如果 step 之间靠全局变量、文件或外部服务隐式通信，那 DAG 依赖就不完整，调度顺序就可能影响结果。这属于用户代码的语义问题，runtime 很难自动修复。

## Q359. DAG 中环如何检测？当前是否检测？

严格来说，DAG 提交时应该检测环。

典型做法是对 step graph 做拓扑排序：

1. 建立 `step_id -> depends_on` 图。
2. 检查依赖的 step_id 是否存在。
3. 用 DFS 或 Kahn 算法检测是否有环。
4. 如果不能得到完整拓扑序，就拒绝提交。

当前实现主要依赖 SDK tracing 生成依赖图，正常顺序写法天然不容易产生环。但控制面仍然应该防御恶意或手工构造的 workflow definition。

如果真的提交了一个有环 DAG，`DependenciesSucceeded` 会一直等不到所有依赖成功，step 可能永远不会 ready，workflow 就卡在 running。这不是理想行为。

所以这里我会把它列为边界条件：主路径能跑，但生产化应该在 `SubmitWorkflow` 解析 definition 时显式做 DAG 校验，包括环检测、重复 step_id 检测和 missing dependency 检测。

## Q360. 如果 step name 重复，SDK 如何生成 step_id？

SDK 会根据 task 函数名生成 step_id，并在同一个 workflow trace 中做去重。

例如：

```python
a = embed("a")
b = embed("b")
c = embed("c")
```

生成的 step_id 大致是：

- `embed`
- `embed_2`
- `embed_3`

也就是说，第一个调用保留函数名，后续重复调用加数字后缀。

这个策略简单易读，适合面试和实验展示。它也让 result step 比较直观，例如 workflow 返回第三个 `embed` 的结果时，`result_step_id` 就会指向 `embed_3`。

长期如果要支持更复杂的动态 DAG，step_id 生成还可以加入 source location、调用路径或用户显式命名，减少重构代码后 step_id 漂移的问题。

## Q361. 如果任务函数修改全局状态，retry 和 replay 会有什么问题？

这会破坏 workflow 的可推理性。

replay 只重放事件，不会重新执行 Python 函数。它能恢复“某个 step 成功了、结果是什么、尝试了几次”，但不会恢复 Python 进程里的全局变量。

如果 task 依赖全局状态，问题会很多：

- retry 可能看到和第一次不同的全局状态。
- worker 重启后全局状态丢失，结果变化。
- 多个 task 共享同一个 Python runner，状态可能互相污染。
- replay 看起来状态正确，但无法解释隐藏副作用。
- redelivery 到另一个 worker 后，另一个 worker 的全局状态完全不同。

因此 workflow task 最好写成输入决定输出的函数。必须访问外部状态时，要通过显式参数、对象存储、数据库或 actor 来表达，并把幂等键传到外部系统。

如果确实需要有状态逻辑，应该用 actor runtime，而不是把状态藏在普通 task 的全局变量里。

## Q362. 如果 task result 不可 JSON 序列化，SDK/worker 应如何处理？

workflow runtime 需要能把结果写入事件或 result store，所以 task result 必须有稳定的序列化形式。当前主路径按 JSON 处理。

如果结果不可 JSON 序列化，比如返回文件句柄、数据库连接、自定义类实例，worker 应该把这次执行视为失败，返回明确错误，而不是写一个不可 replay 的结果。

比较好的处理方式是：

- SDK 文档明确 task 入参和返回值必须 JSON 可序列化。
- worker 在执行后尝试 JSON encode。
- encode 失败时写 TaskFailed / StepFailed。
- 错误信息说明哪个对象不可序列化。
- 如果用户要返回大文件或二进制数据，应先写 result store，再返回一个引用对象。

这也是 result ref 存在的原因之一。日志里不应该塞任意 Python 对象，应该放可解释、可重放的 JSON 或对象引用。

## Q363. 如果 result store Put 成功但 StepSucceeded append 失败，会产生孤儿对象吗？

会，可能产生孤儿对象。

这个路径是：

1. worker 完成 task。
2. 控制面发现结果较大。
3. 控制面先把结果写入 result store。
4. 然后尝试写 `StepSucceeded` 事件。
5. 如果第 4 步失败，对象已经存在，但日志里没有引用它。

因为 result store 和 shared log 不是同一个事务系统，所以这里无法天然原子。

孤儿对象不影响 workflow 正确性，因为没有日志引用它，replay 不会把它当成成功结果。但它会浪费存储空间。

解决方向是做对象生命周期管理：

- result object 使用可识别路径，比如带 workflow_id、step_id、task_id。
- 后台扫描 result store，找出没有被任何 log result_ref 引用的对象。
- 超过安全保留时间后删除。
- 或者先写临时对象，`StepSucceeded` 成功后再提交为正式对象。

当前实现更重视状态正确性，对孤儿对象主要靠后续清理，而不是在写入路径上做复杂事务。

## Q364. 如果 StepSucceeded append 成功但 result store 对象丢失，replay 会怎样？

replay 仍然能恢复出 step 已经 succeeded，以及它的 `result_ref` 是什么。但如果后续要读取这个结果，就会失败。

这说明日志保存的是状态历史和引用，不是大对象本身。对象内容丢失后，log 没法凭空还原。

影响分两种：

- 如果没有下游 step 需要加载这个结果，dashboard 仍然能显示 workflow 已成功，但用户拉取 final result 时可能失败。
- 如果下游 step 需要 `ResolveArgs` 读取这个 result_ref，调度会失败，workflow 可能无法继续。

生产化要补几层保护：

- result store 对象写入时记录内容 hash 和大小。
- 读取 result_ref 时校验 hash。
- dashboard 暴露 missing result_ref。
- 定期扫描 log 中的 result_ref，检查对象是否存在。
- 重要结果使用带副本的对象存储，而不是单机临时目录。

所以 `StepSucceeded` append 成功只能说明“系统确认了这个结果引用”，不等于对象存储永远不会丢。

## Q365. workflow 语义中的 exactly-once-ish 具体保证到哪一层？

这里的 exactly-once-ish 是一个很有意的说法。它不是严格 exactly-once。

当前 workflow 能保证的是：

- workflow 提交有 idempotency key 和 fingerprint，重复提交同一请求不会创建多份 workflow。
- step 成功按 `workflow_id + step_id + input_hash` 去重，同一个逻辑 step 的重复 completion 不会写出多份最终成功结果。
- retry attempt 和 redelivery attempt 可以被区分，不会把有意重试当成重复请求。
- task lease epoch 能拒绝旧 worker 的 stale completion。
- 控制面重启后可以从 workflow stream replay 出 step 状态、attempt、结果引用和 workflow terminal 状态。
- 大结果通过 result_ref 进入 result store，日志里保留可追踪引用。

但它不保证：

- 用户函数物理上只执行一次。
- 外部副作用只发生一次。
- result store 和 log 之间有强事务。
- 跨 stream 多事件写入天然原子。
- Python 代码里的全局状态能被 replay。

因此它的准确边界是：runtime 尽量把重复执行的影响收敛在结果提交层和状态机层，保证最终 workflow 状态可恢复、可去重、可解释；但用户任务本身仍然按至少一次执行来设计。

如果任务会扣款、发邮件、写外部数据库，就必须把幂等键继续传到外部系统。LogServe 能提供的是 runtime 层的幂等框架，不是替外部世界凭空制造严格 exactly-once。
