# 八、Python SDK、DSL、gRPC/CLI Transport 与 Python Executor（拓展）

这一组问题看 SDK 继续做下去会遇到什么。当前 LogServe 的 Python DSL 已经能把 `@task`、`@workflow`、`@actor` 和 `llm_generate()` 跑通，但它还不是完整的语言运行时。要走向更严肃的执行平台，需要处理确定性、跨语言、代码打包、安全隔离、资源限制和 tracing。

## Q611. Python DSL tracing 和 AST 编译两种方式有什么取舍？

Python DSL tracing 的优点是实现快，也贴近用户写法。

LogServe 当前就是这种方式。SDK 在提交 workflow 时运行一遍 workflow 函数。运行过程中，`@task` 不真的执行函数体，而是返回 `StepRef`，SDK 顺便记录 step 和依赖。

这种方式的好处是简单。用户写普通 Python 函数，不需要学习一套新的 DAG 描述语言。函数调用、参数传递、`llm_generate()` 都能自然地被捕获。

缺点是 tracing 只能捕获“这次运行经过的路径”。如果 workflow 里有动态分支、循环、随机数、时间、外部 API 调用，构图结果可能依赖提交时的本地状态。它也很难静态发现所有可能的 step。

AST 编译是另一种路线。SDK 读取 workflow 源码，分析 Python AST，提取函数调用、依赖和控制流。它的优点是更可控，可以在提交前检查禁止语法、发现一些非确定性代码，也更适合做静态错误提示。

但 AST 编译难度高。Python 语法太灵活，函数别名、动态调用、装饰器、闭包、import、反射都让分析变复杂。你最后可能不得不限制 Python 子集。

所以我的判断是：LogServe 早期用 tracing 是对的，能把主链路跑通。后续如果要支持更严格的 workflow，可以增加 AST lint，而不是完全替换 tracing。也就是“运行时 tracing 构图 + 静态检查兜底”。

## Q612. Temporal Python SDK 如何处理 deterministic workflow？LogServe 是否需要类似约束？

Temporal 的核心要求是 workflow replay 必须确定。workflow 代码重放历史事件时，应该走出同样的决策路径，否则历史事件和新代码决策对不上。

Temporal Python SDK 为此做了 sandbox。官方文档说明，Python workflow 会在 sandbox 环境中运行，用来帮助发现非确定性错误；sandbox 包含全局状态隔离和限制机制，会对一些危险的标准库调用或模块成员做限制。它不是完整安全沙箱，但能降低 workflow 写出非确定性代码的概率。

Temporal 还把真正有副作用的代码放到 Activity 里。Workflow 负责决策，Activity 负责 I/O、网络请求、数据库写入等外部副作用。Workflow 中需要时间、随机数、等待这些能力时，也要用 Temporal 提供的 API，让这些结果进入 history。

LogServe 当前没有做到 Temporal 这一级确定性。它的 workflow tracing 只在提交时构建 DAG，后续 replay 是重建 step 状态，不是重新运行 workflow Python 代码来做确定性决策。

LogServe 是否需要类似约束，要看目标。

如果只支持静态 DAG，当前限制基本够用：workflow 提交时构图，运行时按 DAG 执行。

如果未来要支持长时间运行 workflow、等待外部事件、动态分支、循环、信号、子 workflow，就需要引入 Temporal 类似的确定性约束。否则 replay 时无法保证同一段 workflow 代码会做出同样的调度决策。

我的回答会很直接：LogServe 当前是 DAG replay，不是 Temporal 式 deterministic workflow replay。要走到后者，需要 sandbox、确定性 API、side effect 记录、workflow 版本管理和更强的代码约束。

## Q613. 如果用 decorators 构建 DAG，如何支持动态 task mapping？

动态 task mapping 指根据运行时输入生成多个 task，比如对一批文档并行 embedding。

在当前 LogServe tracing 模型下，可以支持一种受限形式：

```python
@workflow
def embed_many(docs):
    refs = [embed(doc) for doc in docs]
    return merge(refs)
```

提交时 `docs` 已经确定，SDK 执行 workflow 函数，就能生成 N 个 `embed` step，再让 `merge` 依赖这些 step。

问题是，这种 mapping 的规模在提交时固定。如果 `docs` 来自上游 step 的结果，SDK 构图时还不知道它有多长，就没法生成后续 step。

要支持真正动态 mapping，有两种做法。

第一种是把 mapping 交给 runtime。新增一个 `MapStep`，它先执行上游 step，拿到 list 后，control plane 再展开子 step。这样 DAG 可以在运行时扩展。

第二种是引入 sub-workflow 或 child workflow。上游 step 产出分片列表后，调度一批子 workflow，每个子 workflow 处理一片。

无论哪种方式，都要控制 fan-out。不能让一个上游结果一下子展开几十万 task，把 queue 打爆。需要 max_parallelism、batch size、backpressure 和 result aggregation。

## Q614. 如果用 async/await API，worker 和 control 需要如何适配？

SDK 层先要拆开“提交”和“等待”。

当前 `submit()` 更像同步调用，提交后轮询到任务完成再返回结果。async API 应该提供：

```python
handle = await client.submit_task(...)
result = await handle.result()
```

这样用户可以并发提交多个任务，不阻塞事件循环。

control plane 不一定要大改。已有的 `SubmitTask`、`GetTaskStatus`、`SubmitWorkflow` 仍然能用。真正需要补的是更好的异步通知机制，比如 server-side streaming、watch API、webhook，避免 SDK 大量轮询。

worker 侧也要分开看。

普通 Python executor 当前是进程池模型，适合 CPU-bound 或同步函数。async task 可以有两条路：要么仍然在进程中 `asyncio.run()`，一次 runner 一次任务；要么做一个 async executor，让同一个进程的事件循环同时跑多个 I/O-bound task。

后者吞吐更好，但隔离更弱。一个用户 coroutine 卡住事件循环，会影响同进程其他任务。

所以我会先在 SDK 和 control 查询层支持 async，再谨慎考虑 worker 端 async executor。

## Q615. 如果支持 Java/TypeScript SDK，proto 和 TaskSpec 需要如何抽象？

不能让 `TaskSpec` 永远以 Python 函数源码为中心。

当前 TaskSpec 里有 `function_source`、`function_name`、`args_json`。这对 Python demo 很方便，但 Java/TypeScript 不应该传 Python 源码。

要跨语言，TaskSpec 应该抽象成更通用的执行描述：

```text
task_name
language
entrypoint
code_ref
runtime_ref
args_payload
serializer
timeout
retry_policy
idempotency_key
```

Python SDK 可以把 `code_ref` 指向 Python zip 或镜像，entrypoint 是 `module:function`。TypeScript SDK 可以指向 npm package 或 bundle，entrypoint 是 JS module export。Java SDK 可以指向 jar 和 class/method。

proto 层也要避免语言私有字段膨胀。可以保留通用字段，再用 `oneof` 或 metadata map 表达语言特定配置。

worker 侧也要变成多 runtime executor：PythonExecutor、NodeExecutor、JVMExecutor。control plane 不应该关心具体语言怎么执行，它只负责调度、状态机、日志和幂等。

## Q616. 如何设计 SDK 的版本兼容？

SDK 兼容要分三层看。

第一层是 API 兼容。用户代码里的 `@task`、`@workflow`、`submit()`、`ActorHandle` 这些接口不能随便改。要改就保留旧参数，新增参数用 keyword-only，废弃前给 warning。

第二层是 wire compatibility。Python SDK 和 Go control plane 通过 proto 通信。proto 字段只能追加，不能随便改 tag、改语义、复用删除字段编号。新增字段要有默认行为，老 server 看到新 client、老 client 连接新 server 都要能退化运行。

第三层是 runtime compatibility。TaskSpec、WorkflowDefinition、ActorCommand 的 JSON payload 要能被旧 worker 或新 worker理解。这里最好加 `schema_version` 或 capability negotiation。

启动时 SDK 可以调用 `GetServerInfo` 或 `GetCapabilities`，确认 control plane 支持哪些能力，比如 actor、LLM、async watch、artifact code_ref。没有能力就给明确错误，而不是提交后才失败。

实际项目里最重要的是别让 SDK 和控制面“隐式耦合”。每个可变协议都要有版本或 capability。

## Q617. 如何防止用户在 workflow 定义阶段执行真实副作用？

这是 tracing DSL 的典型坑。

当前 SDK 构建 workflow 时，会真的执行 workflow 函数。虽然 `@task` 调用不会执行函数体，但普通 Python 代码会执行。如果用户在 workflow 里写：

```python
requests.post(...)
open("file", "w").write(...)
```

这些副作用会在提交阶段发生，而不是 runtime step 执行阶段发生。

防止方法有几类。

第一，文档和 lint 明确规定：workflow 定义阶段只能做 DAG 构建，不要做外部 I/O、副作用、随机数和时间读取。

第二，SDK 做静态检查。用 AST 扫描 workflow source，发现 `requests`、`open(..., "w")`、`subprocess`、数据库 client 等调用时给 warning 或拒绝。

第三，运行时限制。构图时在 sandbox 里执行 workflow，限制网络、文件写入和危险模块。

第四，把副作用 API 做成 task。用户要发请求，就必须写成 `@task`，让它在 worker 里执行，并受 retry、timeout、日志和幂等约束。

Temporal 的 workflow 约束可以作为参考：workflow 里只做确定性决策，外部副作用放到 activity。LogServe 如果继续扩展动态 workflow，也要往这个方向走。

## Q618. 如何把依赖打包成 zip/容器镜像而不是传源码？

可以先做 zip artifact。

SDK 收集当前项目代码、`requirements.lock`、入口函数信息，打成 zip，计算 hash，上传到对象存储。TaskSpec 不再放完整 `function_source`，而是放：

```text
code_ref
code_hash
entrypoint
runtime
dependency_hash
```

worker 收到任务后，按 hash 检查本地缓存。没有就拉 zip，解压到隔离目录，再运行 entrypoint。

容器镜像更适合生产。用户构建 image，里面包含代码、依赖、系统库。TaskSpec 只记录 image digest 和 entrypoint。worker 通过容器 runtime 启动任务。

zip 方式启动快，适合 Python 小任务；容器方式隔离强，环境更稳定，但冷启动更重。

无论 zip 还是镜像，都要写入 shared log 的是引用和 hash，而不是只写一个可变 tag。否则 replay 时无法知道当时到底运行的是哪份代码。

## Q619. 如何实现远程代码执行的 sandbox？

先要承认：当前 `executor/python/server.py` 不是安全沙箱。它在 worker 机器上直接 `exec` 用户源码，适合受控实验，不适合执行不可信代码。

真正的 sandbox 至少要有几层。

进程隔离：每个任务或每个租户在独立进程、容器或 microVM 里执行，避免共享 Python 全局状态。

文件系统隔离：只挂载必要目录，默认只读 rootfs，工作目录独立，禁止访问宿主机敏感路径。

网络隔离：默认禁止出网，或只允许访问白名单服务，防止 SSRF 和数据外传。

资源限制：CPU、内存、进程数、文件大小、运行时间、打开文件数都要有限制。

系统调用限制：seccomp、AppArmor、SELinux、gVisor 或 microVM 限制危险 syscall。

身份隔离：不要用 worker 主进程用户执行任务。每个 sandbox 使用低权限 uid/gid，不能访问宿主机凭据。

LogServe 的 worker 可以把 Python runner 换成 sandbox backend。control plane 仍然调度 TaskSpec，worker 决定用本地进程、容器、Firecracker、gVisor 或 WASM 执行。

## Q620. 容器隔离、Firecracker、gVisor、WASM 分别适合什么场景？

容器是最通用的选择。生态成熟，镜像构建方便，和 Kubernetes 集成好。适合大多数可信或半可信任务。缺点是内核共享，安全边界比 microVM 弱。

Firecracker 是 microVM。隔离强，适合运行不可信代码或多租户场景。缺点是启动和运维成本更高，镜像管理、网络和存储集成比普通容器复杂。

gVisor 是用户态内核拦截 syscall。它比普通容器安全边界更强，比 microVM 更轻。适合希望提高容器安全性，但又不想引入完整 VM 管理成本的场景。代价是 syscall 密集型任务可能变慢，兼容性也要测试。

WASM 启动快、沙箱边界清楚、可移植性好。适合轻量计算、插件、规则执行。缺点是 Python 生态支持有限，很多依赖、C 扩展、文件系统和网络能力都受限制。

对 LogServe 来说，可以分层支持：受控实验用本地 Python runner；生产可信 workload 用容器；不可信多租户用 Firecracker 或 gVisor；轻量插件考虑 WASM。

## Q621. 如果执行不可信 Python 代码，需要限制哪些系统能力？

至少要限制这些能力。

文件系统：禁止读取宿主机敏感目录，比如 `/etc`、`/home`、SSH key、云厂商凭据、Docker socket。工作目录应该是临时目录，任务结束后清理。

网络：默认无出网，或者只允许访问明确白名单。尤其要防止访问 metadata service、内网控制面、对象存储内部地址。

进程：限制 fork、exec、ptrace、进程数和线程数。否则用户代码可以 fork bomb 或探测其他进程。

资源：CPU、内存、磁盘、文件描述符、运行时间都要有限制。

系统调用：禁止 mount、setns、keyctl、bpf、模块加载、原始 socket 等危险能力。

凭据：不要把 worker 的环境变量、云凭证、服务 token 暴露给用户代码。需要访问 secret 时，用最小权限、短期 token、按任务注入。

日志和结果：限制 stdout/stderr 大小，防止日志炸盘；限制 result size，超大结果写对象存储。

Python 语言层的限制不够。单靠删 `__import__` 或限制 builtins 很容易被绕过，必须依赖 OS/container/microVM 级隔离。

## Q622. 如何限制用户代码访问宿主机文件系统？

核心是“不要把宿主机文件系统直接暴露给任务”。

如果用容器，可以使用只读 rootfs，挂载一个空的工作目录，再按需挂载输入 artifact。不要挂载 Docker socket、宿主机 `/var/run`、用户 home 目录和项目根目录。

如果用 chroot 或 namespace，也要配合 mount namespace，把任务能看到的路径限制到临时 root。只给必要的 `/tmp`、输入目录、输出目录。

文件权限上，用低权限 uid/gid 执行任务。即使路径误挂载了，任务也没有权限读写敏感文件。

还要限制输出。用户代码不能随便写无限文件，应该有磁盘 quota。任务结束后，worker 只收集约定目录下的输出文件，其他临时文件直接删除。

在 LogServe 的 TaskSpec 里，可以显式声明：

```text
input_refs
output_policy
read_only_mounts
writable_scratch_bytes
```

worker 根据这些声明构造 sandbox，而不是默认给任务完整工作目录。

## Q623. 如何限制网络出口和防止 SSRF？

默认策略应该是 deny by default。

任务没有声明网络权限时，不允许出网。需要访问外部 API 时，必须声明域名、端口、协议和用途。worker 或 sidecar 通过 egress proxy 强制执行。

防 SSRF 要特别禁止内网敏感地址：

- `169.254.169.254` metadata service
- localhost 和 worker/control/logd 内部端口
- Kubernetes service account endpoint
- 私有网段里未授权服务
- Unix domain socket 和 Docker socket

仅靠应用层检查 URL 不够。用户代码可能绕过 DNS、使用 IP、重定向、IPv6、编码形式。最好在网络层做 egress policy，加 DNS 解析控制和代理审计。

LogServe 可以把网络权限写进 task policy。事件日志记录网络策略 id，而不是把 secret 或 token 写进日志。这样审计时能知道某个任务被允许访问哪些外部地址。

## Q624. 如何实现 per-task resource limit？

不同隔离后端做法不同，但语义要统一。

CPU 可以用 cgroup quota 或 Kubernetes requests/limits。内存也用 cgroup limit，超过后 OOM kill，并把错误类型记录为 `RESOURCE_EXHAUSTED`。

运行时间用 task timeout。当前 LogServe 已经有 `timeout_ms`，普通 Python task 超时会 kill runner。生产里还要在容器或 sandbox 层设置硬 timeout，防止 worker 逻辑失效。

磁盘用 ephemeral storage quota 或 scratch dir 配额。stdout/stderr 也要有限制，不然日志可以把磁盘打满。

进程数、文件描述符、线程数可以用 rlimit。网络带宽可以通过 cgroup、tc 或 egress proxy 限速。

TaskSpec 可以扩展：

```text
cpu_millis
memory_bytes
ephemeral_storage_bytes
max_stdout_bytes
max_runtime_ms
network_policy
```

control plane 做 admission check，worker 做实际 enforce。两层都要有，不然调度器以为能跑，worker 实际一启动就失败。

## Q625. 如何做 SDK 的本地单元测试和 contract test？

本地单元测试先测纯 Python 逻辑，不依赖 Go 服务。

比如 `@task` 在普通执行时是否真的执行函数；workflow tracing 时是否返回 `StepRef`；`StepRef` 嵌套在 list/dict 里是否正确生成 `depends_on`；idempotency key 是否只在显式传入时出现；transport payload 是否符合预期。

这些可以用 fake transport 做，就像当前 `sdk/python/tests/test_client_idempotency.py` 那样。

contract test 要测 Python SDK 和 Go control plane 的协议是否一致。

可以启动真实 control/logd/worker，用 SDK 提交 task、workflow、actor、LLM 请求，然后校验状态、结果和 replay。还要测错误场景：缺字段、重复 idempotency key、payload 冲突、不可序列化参数、任务失败。

proto contract 也要测。Python 生成代码和 Go proto 如果不同步，contract test 应该立刻失败。

最后可以加 golden payload。SDK 构造出的 JSON/protobuf payload 保存成样例，Go 端解析这些样例，避免字段名悄悄漂移。

## Q626. 如果 proto 变更，Python 生成代码如何同步？

需要把 proto 生成纳入固定命令和 CI。

仓库里应该有一个脚本，比如：

```text
scripts/gen_proto.sh
```

它同时生成 Go 代码和 Python 代码。Python 生成物写到 `sdk/python/logserve/_generated/`，Go 生成物写到 `gen/`。

CI 里跑生成命令，然后检查工作区是否有 diff。如果开发者改了 `proto/control.proto` 但忘了更新 Python 生成代码，CI 就失败。

还要遵守 protobuf 兼容规则。已有字段编号不能复用；删除字段要 reserved；新增字段要有默认语义；老 client 不认识新字段时不能崩。

SDK 初始化时可以做 capability check。比如 server 不支持 `ReplayLLM`，Python SDK 调用时给明确错误，而不是 protobuf 层报一个难懂的 unknown method。

## Q627. 如何为 SDK 增加 tracing context propagation？

目标是让一次用户请求从 SDK、control plane、worker、Python executor、LLM/vLLM 都能串成一条 trace。

SDK 提交时生成或读取 trace context。比如支持 W3C `traceparent`，也可以先用简单的 `trace_id`、`span_id`。

`SubmitTaskRequest`、`SubmitWorkflowRequest`、`CallActorRequest`、`SubmitLLMRequest` 都带 trace metadata。control plane 写事件时，把 trace_id 放进 event payload 或 metadata。worker poll 到 task 后继续传给 Python executor。

Python executor 执行用户函数时，可以通过环境变量或上下文对象暴露 trace_id。用户 task 调外部服务时，也能继续传播。

LLM adapter 调 vLLM 时，把 trace id 放进 header；如果 vLLM 暴露 request id 或 metrics label，LogServe 把它和 task_id 绑定。

dashboard 里按 trace_id 查整条链路：SDK submit、log append、queue wait、worker start、executor run、result commit。这样排查慢请求会比单看日志容易很多。

## Q628. 如何处理 Python GIL 对 task executor 并发的影响？

GIL 主要影响同一个 Python 进程内的 CPU-bound 线程并发。LogServe 当前不是在线程里跑多个 task，而是用多个 Python runner 进程。

这点很关键。多进程可以绕开 GIL。`task_pool_size` 越大，worker 会启动越多 Python runner；每个 runner 内部一次只执行一个请求。

对于 CPU-bound Python 任务，增加进程数通常比在一个进程里开线程更有效。但进程数不能无限加，受 CPU 核数、内存、启动成本和任务依赖影响。

对于大量使用 NumPy、PyTorch 等释放 GIL 的库，单进程内也可能有并行，但仍要看底层线程池配置。多个 runner 同时跑这些库，可能把 CPU 线程打爆。

所以 worker 配置要区分任务类型。CPU-bound 任务按 CPU 核数配置 runner；I/O-bound 任务可以考虑 async executor；GPU/LLM task 则走单独 LLM pool，避免和普通 Python task 抢 runner。

## Q629. 如果任务是 CPU-bound，Python runner 进程池如何配置？

CPU-bound 任务应该按 CPU 核数和任务内线程数配置。

如果任务是纯 Python 计算，每个 runner 基本占一个 CPU core。`task_pool_size` 可以接近物理核心数或略低，给 Go worker、logd、control plane 留出 CPU。

如果任务调用 NumPy、BLAS、PyTorch 这类库，它们内部可能开多线程。此时 runner 数不能按核心数直接开满，否则每个进程再开一堆线程，会严重超卖。需要设置 `OMP_NUM_THREADS`、`MKL_NUM_THREADS`、PyTorch thread 数。

还要考虑内存。多个 runner 会各自加载模块和数据，内存占用可能线性增长。大模型或大数据处理任务不适合盲目增加 runner。

对 LogServe 来说，比较好的配置方式是：

```text
task_pool_size = floor(cpu_cores / threads_per_task)
```

再结合 benchmark 调整。dashboard 应该展示 local queue wait、CPU 使用率、runner 忙闲状态，帮助判断池子是太小还是太大。

## Q630. 如果任务是 I/O-bound，async executor 是否更合适？

可能更合适。

I/O-bound 任务大部分时间在等网络、磁盘、数据库或对象存储。用多个进程也能提高并发，但进程成本高，内存占用也高。

async executor 可以在一个 Python 进程里跑多个 coroutine，让同一个事件循环同时等待多个 I/O。对于 HTTP 请求、对象存储读写、数据库查询这类任务，吞吐会更好。

但 async executor 会带来新的边界。

第一，用户函数必须是 async 或可被包装成 async。同步阻塞调用会卡住整个事件循环。

第二，隔离更弱。一个 coroutine 写全局状态，可能影响同进程其他 coroutine。

第三，timeout、取消、stdout 捕获和异常隔离都要重新设计。

第四，actor 不适合简单并发执行。actor 仍然要保持 per-actor 顺序，async 只能优化等待，不能破坏 mailbox 语义。

所以我的选择是：先保留进程池作为默认 executor；为明确声明为 I/O-bound 的 task 增加 async executor。用户通过装饰器参数声明：

```python
@task(executor="async")
async def fetch(url):
    ...
```

control plane 和 worker 根据 executor 类型路由到不同本地 pool。这样不会把 CPU-bound、I/O-bound、actor 和 LLM 请求混在一条执行路径里。
