# 八、Python SDK、DSL、gRPC/CLI Transport 与 Python Executor（深度）

这一组问题主要看 SDK 和 Python executor 的边界。LogServe 的 Python 层让用户写起来像普通函数调用，但运行时其实经过了 tracing、源码采集、JSON/protobuf 序列化、Go control plane 调度，以及 worker 里的长驻 Python runner。这里的设计不难懂，难点在边界条件。

## Q586. `_TRACE_STACK` 为什么使用栈？嵌套 workflow 会怎样？

`_TRACE_STACK` 用栈，是为了让当前正在 tracing 的 workflow context 可以被动态查到。

SDK 构建 workflow definition 时，会调用 `trace_workflow(fn, *args, **kwargs)`。这个函数先创建一个 `WorkflowTraceContext`，压入 `_TRACE_STACK`，然后执行用户的 workflow 函数。workflow 内部的 `@task` wrapper 调用 `current_trace_context()`，拿到栈顶 context，把这次调用记录成 step。

用栈而不是单个全局变量，是为了保证 tracing 结束后能正确恢复旧 context。`trace_workflow` 用 `finally` pop，即使 workflow 构图时报错，也不会把脏 context 留在全局状态里。

嵌套 workflow 在当前实现里不是正式支持的语义。如果一个 workflow tracing 过程中又触发另一个 `trace_workflow`，新的 context 会压栈，内部 `@task` 会记录到内层 context。内层结束后再回到外层。

但这不等于系统支持 sub-workflow。因为 `_build_workflow_definition()` 只处理当前被提交的那个 workflow 返回的 `StepRef` 和 steps。嵌套 workflow 的结果不会自动变成外层 DAG 里的一个 sub-workflow 节点。面试里应该说清楚：栈结构避免 context 串扰，但当前 DSL 没有完整的嵌套 workflow 语义。

## Q587. `current_trace_context` 如何区分普通本地执行和 workflow tracing？

它只看 `_TRACE_STACK` 是否为空。

如果栈为空，说明当前不在 workflow tracing 中。此时 `@task` wrapper 会直接调用原函数，`llm_generate()` 也会直接提交 LLM 请求。

如果栈不为空，说明 SDK 正在构建 workflow DAG。此时 `@task` 不执行函数体，而是调用 `ctx.add_step(...)` 记录 step；`llm_generate()` 也会调用 `ctx.add_llm_step(...)` 记录 LLM step。

这个判断很简单，也让同一个 Python 函数有两种行为：

- 普通执行：像本地函数一样运行。
- workflow tracing：只记录 DAG 节点，不执行用户函数体。

这里有一个隐含边界：tracing 是进程内全局状态，不是线程隔离的。当前适合单线程 SDK 提交流程。如果多个线程同时 build workflow，最好改成 `contextvars.ContextVar`，避免 trace context 串到别的线程。

## Q588. `StepRef` 在 args/kwargs 中如何编码？

`StepRef` 会被 `_encode_refs()` 编码成一个特殊 JSON 对象：

```json
{"__step_ref__": "step_id"}
```

同时 `_encode_refs()` 会把这个 `step_id` 加入依赖列表。

举个例子：

```python
vec = embed(query)
docs = search(vec)
ans = generate(query, docs)
```

在 tracing 时，`embed(query)` 返回 `StepRef("embed")`。`search(vec)` 的参数里出现了这个 `StepRef`，SDK 会把它编码成：

```json
{"__step_ref__": "embed"}
```

并把 `depends_on` 设置为 `["embed"]`。

`StepRef` 不只支持直接出现在 args 里，也支持嵌套在 list、tuple、dict 里。SDK 会递归扫描，把所有依赖 step 收集出来。

## Q589. `json.dumps(value)` 在 `_encode_refs` 中起什么校验作用？

它用来提前检查这个值是否 JSON 可序列化。

`_encode_refs()` 处理完 `StepRef`、list、tuple、dict 后，剩下的普通值会调用 `json.dumps(value)`。如果这个值不能被 JSON 编码，比如文件句柄、socket、数据库连接、自定义对象，`json.dumps` 会直接抛错。

这个检查的好处是错误发生在 SDK 构图或提交阶段，而不是等到 worker 执行时才发现参数没法传。

当前实现没有自定义 encoder，所以它支持的是普通 JSON 类型：字符串、数字、布尔、None、list、dict 等。复杂对象应该显式转成 JSON 数据，或者写入对象存储后传 ref。

## Q590. `inspect.unwrap` 的意义是什么？

`inspect.unwrap(fn)` 用来拿到被装饰器包住之前的原始函数。

`@task` 和 `@workflow` 都会返回 wrapper。虽然它们用了 `functools.wraps` 保留 `__name__` 等元数据，但真正的函数源码还是原始函数更可靠。

普通 task 提交时，SDK 会执行：

```python
inspect.getsource(inspect.unwrap(fn))
```

这样拿到的是用户写的函数体，而不是 `@task` 生成的 wrapper 函数。

workflow tracing 的 `add_step()` 里也用了 `inspect.unwrap(fn)`。否则 function_source 可能变成 SDK wrapper 的源码，worker 端找不到用户函数逻辑。

所以 `unwrap` 的价值是把装饰器层剥掉，让源码采集回到用户函数本身。

## Q591. `inspect.getsource` 在 notebook、lambda、动态函数中会有什么问题？

`inspect.getsource` 依赖 Python 能找到对象对应的源码文件或源码缓存。

在普通 `.py` 文件里，它通常能工作。Notebook、REPL、动态生成函数、`exec` 创建的函数、lambda、闭包里的局部函数，就容易出问题。

Notebook 里函数定义可能没有稳定文件路径，或者 source line 被运行环境管理。lambda 没有独立函数名，源码片段也不一定能正确定位。动态函数可能根本没有可读取 source。

即使能读取，闭包也是问题。函数源码里引用了外层变量，但 worker 端只拿到函数源码，没有拿到原来的 Python 运行环境。

所以当前 SDK 更适合“函数定义在 `.py` 文件中，依赖显式 import 或模块级定义”的用法。要支持 notebook 体验，需要额外机制，比如打包 cell source、依赖声明、cloudpickle，或者要求用户把任务函数放到模块文件里。

## Q592. `_module_source` 读取整个模块源码有什么优缺点？

优点是依赖更完整。

如果 task 函数依赖同一个文件里的 helper 函数、常量、class，只传单个函数 source 可能不够。读取整个 module source 后，worker 执行这段源码，就能同时得到 helper 和 task 函数。

workflow 和 actor 都能从中受益。actor class 往往依赖同文件里的工具函数，workflow step 也可能调用模块级 helper。

缺点也明显。

第一，payload 变大。一个模块里可能有很多和当前 task 无关的代码。

第二，泄漏风险变大。模块里如果写了 token、密码、测试数据，也会被一起传给 control plane 和 worker。

第三，可复现边界仍然有限。模块源码不包含 pip 依赖、外部文件、环境变量和运行时配置。

所以 `_module_source` 是一个实用改进，不是完整代码打包方案。它让 demo 和中小模块更容易跑，但生产化还需要构建 artifact 或镜像。

## Q593. 如果模块文件很大，每个 task 都传源码会有什么性能问题？

会带来三类成本。

第一是网络和序列化成本。每个 task 或 workflow step 都携带大段 source，gRPC request 或 CLI JSON payload 会变大，control plane 解析和日志写入也会变慢。

第二是日志膨胀。LogServe 是 log-first，TaskSubmitted 或 WorkflowSubmitted 里保存 TaskSpec。如果每个任务都带大模块源码，shared log 会增长很快，replay 也会变慢。

第三是 worker 执行成本。Python executor 每次都要 `exec(compile(source, ...))`，模块越大，compile/exec 越慢。

更好的设计是 source content-addressing。SDK 先上传代码包或模块 artifact，control plane 只记录 `code_ref` 和 hash。worker 缓存代码包，只有 hash 变化时才拉取。这样 log 里保留可恢复引用，不需要每个 task 重复塞一份源码。

## Q594. 如果源码中包含密钥，传 `function_source` 会有什么风险？

风险很直接：密钥会进入 LogServe 的控制面、shared log、worker 进程和实验报告目录。

比如用户在模块里写了：

```python
API_KEY = "sk-..."
```

如果 SDK 读取整个 module source，这个字符串会被放进 `function_source` 或 actor class source。log-first 语义又会把提交事件写入 log。后面 replay、debug、dashboard、备份都可能接触到它。

这比普通程序日志泄漏还严重，因为 shared log 是恢复系统状态的基础，通常不会很快删除。

正确做法是：代码里不要硬编码密钥。SDK 也应该在提交前做敏感模式检查，比如扫描常见 secret pattern，或者允许用户用 `.logserveignore` 排除文件。更生产化的做法是 task 通过 secret manager 注入环境变量，log 中只记录 secret name，不记录 secret value。

## Q595. 如果用户函数依赖外部文件，单纯传源码是否足够？

不够。

源码只能描述 Python 代码，不能自动带上外部文件。比如函数读取 `config.yaml`、加载本地模型文件、访问 `data/input.csv`，worker 上没有这些文件就会失败。

当前 SDK 适合无外部文件或依赖已经预装在 worker 环境里的任务。

如果要支持外部文件，需要把文件依赖显式化。可以让 SDK 支持 artifact upload，把文件上传到对象存储，然后在 TaskSpec 里记录 artifact ref。worker 执行前下载或挂载这些文件。

还有一种做法是用容器镜像。用户把代码和文件打进镜像，worker 运行对应镜像。这样环境更一致，但启动成本和调度复杂度也会增加。

所以面试里可以说：当前实现传源码解决了最小可运行链路，外部文件需要 artifact/ref 机制补齐。

## Q596. 如果用户函数依赖 pip 包版本，如何保证 worker 环境一致？

当前实现主要依赖实验环境预装依赖。SDK 提交 source，不会自动打包 pip 环境。

要保证一致性，需要引入环境声明。

最简单的是 `requirements.txt`。SDK 提交任务时记录依赖文件 hash，worker 启动时按固定环境运行。缺点是动态安装慢，也容易污染长驻 runner。

更稳的是容器镜像。每个任务或一组任务绑定一个 image digest。control plane 记录 image digest，worker 按 digest 拉取和运行。这样 Python 版本、pip 包、系统库都能固定。

还可以做 virtualenv/conda env cache。worker 本地按 dependency lock hash 缓存环境。相同 hash 复用，不同 hash 创建新环境。

对 LogServe 来说，关键是把环境也纳入可恢复元数据：代码 hash、依赖 hash、镜像 digest 都要写入 log。否则 replay 只能恢复任务状态，不能解释当时到底用什么环境执行。

## Q597. CLI fallback 为什么对本地 demo 有帮助？

CLI fallback 降低了 Python SDK 的启动门槛。

如果用户机器上没有安装 `grpcio` 或 protobuf 依赖，`LOGSERVE_SDK_TRANSPORT=auto` 会退回 `CLIControlTransport`。SDK 通过 `go run cmd/logservectl` 或 `LOGSERVE_CLI` 指定的二进制提交请求。

这对单机实验很有用。Go 工具链已经在项目里，control plane 也在本地，用户不需要先解决 Python gRPC 依赖问题，就能跑 `@task`、`@workflow`、`@actor` 的基本 demo。

它还方便调试。CLI 请求和输出都是 JSON，出问题时容易把 payload 打出来复现。

所以 CLI fallback 的定位不是高性能生产路径，而是本地可用性和调试兜底。

## Q598. CLI fallback 的性能和可靠性问题是什么？

CLI fallback 每次请求都要启动一个外部命令，默认还可能执行 `go run`。

性能上，进程启动、Go 编译或加载、JSON stdin/stdout 都有额外开销。提交少量 demo 任务可以接受，提交大量小任务就很慢。

可靠性上，CLI 多了一层失败点。`go` 不在 PATH、项目目录不对、`LOGSERVE_CLI` 指错、stderr 输出非预期、命令返回非 JSON，都会让 SDK 请求失败。

还有错误语义问题。gRPC 可以拿到结构化 RPC 错误，CLI 通常只能解析 stdout/stderr。错误分类、重试和 timeout 控制都比较粗。

所以常规运行应优先 gRPC。CLI fallback 是“能跑起来”的保底路径，不适合作为高吞吐 transport。

## Q599. gRPC client 的错误重试应该放在 SDK 层还是 control 层？

两层都需要，但职责不同。

SDK 层适合处理 transport 级瞬时错误，比如连接重置、短暂 unavailable、deadline exceeded。SDK 可以有限重试，但必须只重试幂等或带 idempotency key 的请求。否则 SubmitTask 重试可能提交两个任务。

control 层负责业务语义的幂等。比如相同 idempotency key 和相同 fingerprint，应该返回已有任务或结果；相同 key 但 payload 不同，应该返回冲突。

worker redelivery、task retry、actor command 顺序，这些属于 control plane 和 runtime 语义，不应该靠 SDK 重试解决。

所以更准确的回答是：SDK 只做安全的 transport retry；真正的提交幂等和状态推进要在 control plane。没有 idempotency key 的 submit，SDK 不应该随便自动重试。

## Q600. SDK 同步等待任务完成会不会阻塞用户？是否需要 async API？

会阻塞。

当前 `GrpcControlTransport._submit_task()` 调用 `SubmitTask` 后，会进入 `_wait_task()` 轮询 `GetTaskStatus`，直到状态变成 `SUCCEEDED` 或 `FAILED`。workflow 和 LLM submit 也是类似模式。

这个设计适合 demo：用户调用 `submit(add, 1, 2)`，直接拿到结果。

但长任务、批量任务、UI 服务端都不适合一直阻塞。用户可能只想拿到 task_id，然后异步查询状态，或者用 callback/webhook 等待完成。

后续可以补三类 API：

- `submit_async()`：返回 task_id，不等待结果。
- `await_result(task_id)`：用户自己决定何时等待。
- `asyncio` 版本 client：用 async gRPC 和 async polling，适合服务端集成。

当前同步 API 是好用的第一层，但不是完整 SDK 形态。

## Q601. `submit_llm` 返回 output 而 `llm_generate` 非 workflow 场景返回 result，这个 API 是否一致？

不完全一致。

`LogServeClient.submit_llm()` 返回的是完整 output，里面包含 task 状态、结果、worker_id、时间戳等字段。

`llm_generate()` 在非 workflow 场景下会调用 `submit_llm(...).get("result")`，只返回结果文本。

这个差异有历史原因：`llm_generate()` 更像 workflow step 或普通生成函数，用户通常只关心生成结果；`submit_llm()` 更像底层接口，需要暴露任务元数据。

但从 API 一致性看，确实容易让人困惑。一个更清楚的设计是：

- `llm_generate()` 始终返回生成结果或 `StepRef`。
- `submit_llm()` 始终返回完整 task output。
- 文档明确说明二者定位不同。

如果要更严格，可以增加 `return_metadata=True` 参数，或者定义 `LLMResult` 对象，里面同时有 text 和 metadata。

## Q602. `ActorHandle.__getattr__` 动态方法有什么可维护性问题？

动态方法让用户写起来很自然：

```python
counter.inc()
```

但它也有几个问题。

第一，IDE 和类型检查器不知道 actor 有哪些方法。自动补全、mypy、pyright 都很难推断 `counter.inc` 是否存在。

第二，拼写错误会变成运行时错误。`counter.icn()` 也会被包装成一次 actor call，只有到 control plane 或 worker 执行时才会发现没有这个方法。

第三，保留方法名容易冲突。比如 actor 自己有方法名叫 `call`、`actor_id`，可能和 `ActorHandle` 本身属性冲突。

第四，错误信息不够早。用户本地看不到 class method schema，只能等远端返回失败。

改进方向是生成 typed handle。创建 actor 时 SDK 可以知道 class source，生成一个包含明确方法的 proxy，或者提供 `ActorHandle[Counter]` 类型辅助。简单项目里动态 `__getattr__` 够用，生产 SDK 需要更强的类型体验。

## Q603. actor class source 用 `_module_source` 还是 `inspect.getsource`，有什么差异？

当前 `create_actor()` 优先用 `_module_source(cls)`。如果读不到模块文件，再退回 `inspect.getsource(cls)`。

用 `_module_source` 的好处是上下文更完整。actor class 如果依赖同文件里的 helper、常量、父类，worker 执行整个模块源码后更容易找到这些对象。

用 `inspect.getsource(cls)` 的好处是 payload 小，只传 class 定义本身。缺点是 class 里引用外部 helper 时，worker 端可能缺东西。

当前选择偏向可运行性：优先传整个模块，让 actor 在 worker 上更容易执行成功。

代价是风险更大：模块里无关代码和敏感信息也会被传过去；模块很大时 payload 和 log 都会膨胀。

更成熟的做法是构建代码包或 artifact。SDK 记录 artifact hash，worker 拉取完整代码环境，log 中只保存引用和 hash。

## Q604. Python executor 如何返回 result 和 state？

普通 task 和 actor 返回结构不一样。

普通 task 执行后，`handle_task()` 返回：

```json
{"ok": true, "result": ...}
```

如果执行出错，外层 `handle_request()` 会捕获异常并返回：

```json
{"ok": false, "error": "traceback..."}
```

actor 执行后，`handle_actor()` 会返回：

```json
{"ok": true, "result": ..., "state": obj.__dict__}
```

这里的 `state` 是 actor 对象执行完方法后的内存状态。Go worker 收到后，会把 result 作为本次 actor call 的返回值，把 state 作为新的 actor state 写回 control plane。

如果 result 为空，Go worker 会补成 JSON `null`；如果 actor state 为空，会补成 `{}`。这让后续日志和 metadata 不会出现空字节含义不清的问题。

## Q605. Python runner 长驻进程如何处理 stdin/stdout 协议？

worker 启动 Python runner 时执行：

```text
python executor/python/server.py --loop
```

runner 进入循环，从 stdin 按行读取 JSON。每一行是一条请求。执行完成后，它向 stdout 打印一行 JSON 响应，并 flush。

Go 侧的 `pythonRunner.Execute()` 会：

1. 用 mutex 锁住 runner。
2. 把请求 JSON marshal 成一行，写入 stdin。
3. 用 scanner 从 stdout 读取一行。
4. 把这一行 unmarshal 成 `executorResponse`。

mutex 很重要。一个 runner 只有一条 stdin/stdout 流，如果多个 goroutine 同时写请求、同时读响应，响应就会错配。当前通过每个 runner 内部串行，保证一次请求对应一次响应。

并发来自 runner pool，而不是单个 runner 内并发。也就是说，提高 Python task 并发要增加 runner 数量，而不是让一个 runner 同时执行多个请求。

## Q606. 如果用户代码死循环，executor 如何超时和重启？

worker 在执行 task 前会根据 `timeout_ms` 创建 `context.WithTimeout`。

`runner.Execute()` 等待 stdout 响应时，同时监听 context。如果超时，Go 侧会 kill Python 进程，然后返回 `context.DeadlineExceeded`。

`executeTask()` 看到普通 Python task 超时后，会调用 `runner.Restart(ctx, cfg)`，重新启动一个干净的 Python runner。这样死循环残留的进程不会继续占用这个 runner。

最终任务会被标记为 failed，错误信息类似：

```text
task timed out after 50ms
```

workflow step 如果还有 retry 次数，control plane 会调度下一次 attempt。这样 timeout 不会把 worker 永久卡死。

需要注意：这是进程级终止，不是 Python 线程级中断。对于 C 扩展卡死、死循环、阻塞系统调用，kill 进程比依赖 Python 信号更可靠。

## Q607. 如果 executor 进程崩溃，worker 如何检测？

worker 通过 stdout scanner 检测。

`runner.Execute()` 写入请求后，会启动 goroutine 调用 `scanner.Scan()` 等待一行响应。如果 Python 进程崩溃或 stdout 关闭，`Scan()` 会返回 false。

此时 Go 侧会检查 scanner error；如果 stderr buffer 有内容，就把 stderr 作为错误返回；如果没有 stderr，就返回 `python executor stopped`。

这个错误会让当前 task failed，并写入 TaskFailed 事件。对于 timeout，worker 会主动 restart runner；对于普通崩溃，当前代码会把错误返回。后续任务如果继续使用同一个已经停止的 runner，写 stdin 可能失败，这时也会暴露为执行错误。

更完善的设计是：只要检测到 executor stopped 或 broken pipe，就自动 restart runner，并让当前任务按 retry 策略重投。当前实现已经能检测失败，但自愈策略还可以更积极。

## Q608. 如果用户代码打印大量 stdout，会不会破坏 JSON 通信协议？

会，这是当前协议的一个明显边界。

Python runner 用 stdout 返回一行 JSON 响应。Go 侧 scanner 读取 stdout 的下一行，并尝试按 executorResponse 解析。

如果用户函数里写：

```python
print("debug")
```

这行内容也会出现在 stdout。Go 侧可能先读到 `debug`，然后 JSON unmarshal 失败，任务就会失败。

大量 stdout 还可能撑爆 scanner buffer，或者让协议响应错位。

更稳的做法是把用户 stdout/stderr 重定向到单独通道。比如 executor 在执行用户函数时临时捕获 `sys.stdout` 和 `sys.stderr`，把日志作为字段返回，或写到文件/对象存储。控制协议本身只使用专门的 pipe，不和用户 stdout 混用。

当前项目适合演示和受控任务，用户代码最好不要向 stdout 打印大量内容。

## Q609. 如果函数返回不可 JSON 序列化对象，错误在哪一层暴露？

主要在 Python executor 层暴露。

executor 执行用户函数后，会把 response 用：

```python
json.dumps(response, ensure_ascii=False)
```

写到 stdout。如果 `result` 里包含不可 JSON 序列化对象，比如 datetime、自定义 class、numpy array、文件句柄，`json.dumps` 会抛出 TypeError。

这个异常发生在 `main()` 或 `--loop` 打印响应时。因为它不在 `handle_request()` 的 try/except 内部包住整个打印流程，可能导致 Python runner 退出或 stdout 协议中断。

Go worker 会看到 executor stopped、scanner error 或 JSON 解析错误，然后把 task 标记为 failed。

更好的设计是在 `handle_request()` 返回前就验证 result/state 是否可序列化。如果不可序列化，返回结构化错误：

```json
{"ok": false, "error": "result is not JSON serializable: ..."}
```

SDK 侧也可以提供 helper，提醒用户返回 JSON 类型，复杂结果写 result store。

## Q610. 如何为 SDK 增加类型提示、schema 校验和更好的错误信息？

可以分三步做。

第一步，加完整 type hints。`LogServeClient.submit()`、`submit_workflow()`、`create_actor()`、`llm_generate()`、`ActorHandle.call()` 都应该标注参数和返回类型。`StepRef`、transport response、workflow status、actor status 可以用 `TypedDict` 或 dataclass 表达。

第二步，加 schema 校验。提交前检查 args/kwargs 是否 JSON 可序列化，检查 workflow 是否返回 `StepRef`，检查 actor init args 是否可序列化，检查 LLM 参数是否合法。错误要指向具体字段，比如 `kwargs["docs"][2] is not JSON serializable`，不要只给一个 TypeError。

第三步，改进远端错误包装。gRPC 错误、control plane 业务错误、worker executor traceback、timeout、idempotency conflict 应该有不同异常类型。比如：

- `LogServeConnectionError`
- `LogServeTimeoutError`
- `LogServeTaskFailed`
- `LogServeIdempotencyConflict`
- `LogServeSerializationError`

这样用户可以按异常类型处理，而不是解析字符串。

如果再往前走，可以从函数签名生成 input schema。普通 task 和 actor method 的参数类型可以转成 JSON schema，SDK 提交前校验，control plane 也能保存 schema，dashboard 里可以展示更清楚的输入输出约束。
