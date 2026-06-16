# 八、Python SDK、DSL、gRPC/CLI Transport 与 Python Executor（简单）

这一组问题讲 LogServe 的 Python 入口。用户看到的是 `@task`、`@workflow`、`@actor`、`llm_generate()` 这些 API；背后实际分成三层：SDK 负责把 Python 调用转成结构化请求，transport 负责把请求发给 Go control plane，worker 的 Python executor 负责真正执行用户函数或 actor 方法。

## Q571. `@task` 装饰器做了什么？

`@task` 做两件事。

第一，它给函数打上 LogServe 需要的元数据，比如 `_logserve_task`、重试次数和 timeout。默认是 `retries=3`、`timeout_ms=30000`。

第二，它改变函数在 workflow tracing 中的行为。平时直接调用这个函数，它就正常执行 Python 代码。可是在 `@workflow` 被提交时，SDK 会进入 trace context。此时调用 `@task` 函数不会真的执行函数体，而是记录一个 workflow step，并返回 `StepRef`。

所以 `@task` 不是简单的本地装饰器。它同时支持本地普通调用和 workflow DAG 构建。

可以这样理解：

```python
@task
def embed(query):
    return [1, 2, 3]
```

普通调用 `embed("hi")` 会返回真正结果。workflow tracing 时调用 `embed("hi")`，会生成一个 step，函数体不会在 SDK 进程里执行。

## Q572. `@workflow` 装饰器做了什么？

`@workflow` 主要给函数打标，让 SDK 知道它应该按 workflow 提交，而不是按普通 task 提交。

代码上，它会设置 `_logserve_workflow=True`，同时保存 workflow 默认的重试次数和 timeout。真正的 DAG 构建不是在装饰时完成，而是在 `submit_workflow()` 或 `submit()` 检测到这个标记后完成。

提交 workflow 时，SDK 会执行 `_build_workflow_definition()`。这个函数会开启 trace context，然后运行一遍 workflow 函数。运行过程中，里面的 `@task` 调用被记录成 step；如果调用了 `llm_generate()`，也会被记录成 LLM step。

因此 `@workflow` 的重点不是“立刻运行一个流程”，而是让 SDK 能在提交时把 Python 函数调用关系转成 DAG definition。

## Q573. `@actor` 装饰器做了什么？

`@actor` 用来声明一个 Python class 是 LogServe actor class。

它会给 class 设置 `_logserve_actor=True`，并记录 snapshot 频率 `_logserve_snapshot_every`。默认 `snapshot_every=25`，也就是每应用一定数量的 command 后可以创建 snapshot。

比如：

```python
@actor(snapshot_every=10)
class Counter:
    def __init__(self):
        self.value = 0

    def inc(self):
        self.value += 1
        return self.value
```

创建 actor 时，SDK 会读取 class source 或 module source，把 class 名、初始化参数、snapshot_every 一起提交给 Go control plane。后续调用 actor method 时，SDK 不会直接本地执行，而是通过 `CallActor` 走 control plane 和 worker。

## Q574. `llm_generate` 在 workflow tracing 中如何变成一个 step？

`llm_generate()` 会先检查当前是否处在 workflow trace context 中。

如果不在 workflow 里，它会直接调用 `submit_llm()`，提交一个独立的 LLM task。

如果在 workflow tracing 里，它不会马上向 control plane 提交 LLM 请求，而是调用 `ctx.add_llm_step(...)` 记录一个 workflow step。

这个 step 的结构和普通 task step 类似，但会带 LLM 专用字段：

- `task_name` 是 `llm:<model_name>`
- `function_name` 是内部保留名 `__logserve_llm__`
- `function_source` 为空
- prompt 放在 `args_json`
- 额外记录 model name、model version、adapter、max tokens
- 返回值是 `StepRef`

这样 RAG workflow 里的生成阶段就不是旁路调用，而是 DAG 里的一个可调度 step。上游检索 step 的结果可以作为 prompt 的一部分传给它，SDK 会把依赖关系记录下来。

## Q575. SDK 默认使用什么 transport？

SDK 默认使用 `LOGSERVE_SDK_TRANSPORT=auto`。

在 `auto` 模式下，它会优先尝试 gRPC transport，也就是 `GrpcControlTransport`。如果 Python 环境里能导入 `grpcio` 和 protobuf 生成代码，SDK 会创建 gRPC channel，直接调用 Go control plane 的 RPC。

如果 gRPC 依赖缺失，并且模式是 `auto`，SDK 会 fallback 到 CLI transport。

所以默认策略是：能走 gRPC 就走 gRPC；本地 Python 环境缺依赖时，用 CLI 保底。

## Q576. 什么时候 fallback 到 CLI？

当前 fallback 到 CLI 的条件比较明确：`LOGSERVE_SDK_TRANSPORT=auto`，并且导入 gRPC client 失败。

也就是说，fallback 主要解决的是“Python 环境没有安装 grpcio/protobuf”这一类问题。

如果用户显式设置：

```text
LOGSERVE_SDK_TRANSPORT=cli
```

SDK 会直接使用 CLI。

如果用户显式设置：

```text
LOGSERVE_SDK_TRANSPORT=grpc
```

但 Python gRPC 依赖缺失，SDK 会报错，不会偷偷退回 CLI。这样做是为了避免用户明明要求 gRPC，结果实际走了另一条路径。

还有一个边界要说清楚：如果 gRPC 依赖存在，但 control plane 地址连不上，当前不会自动 fallback 到 CLI。这种情况应该暴露连接错误，因为它说明服务状态或地址配置有问题，不是 transport 依赖缺失。

## Q577. `LOGSERVE_CONTROL_ADDR` 的作用是什么？

`LOGSERVE_CONTROL_ADDR` 用来指定 Go control plane 的地址。

SDK 创建 `LogServeClient` 时，如果没有手动传 address，就会读取这个环境变量。默认值是：

```text
127.0.0.1:50052
```

在 gRPC transport 下，这个地址会传给 `grpc.insecure_channel()`，用于连接 control service。

在 CLI transport 下，CLI 命令也会按自己的配置或默认地址去访问 control plane。SDK 这一层主要负责把用户请求转成 JSON payload，并调用 `logservectl`。

实验环境里，如果 control plane 跑在默认端口，就不需要设置这个变量。如果端口变了，就要设置它。

## Q578. `LOGSERVE_SDK_TRANSPORT` 有哪些模式？

当前主要有三种模式。

`auto` 是默认模式。SDK 优先使用 gRPC；如果 gRPC Python 依赖不存在，就使用 CLI。

`grpc` 是强制 gRPC 模式。SDK 会要求 `grpcio` 和 protobuf 可用。如果依赖缺失，会直接报错。

`cli` 是强制 CLI 模式。SDK 会调用 `logservectl`，把 payload 通过 stdin 传给命令行工具，再读取 JSON 输出。

这三个模式的用途不同。实验和正常开发推荐 `auto` 或 `grpc`；如果 Python 依赖没装好，或者要快速验证控制面命令，`cli` 可以作为兜底。

## Q579. `GrpcControlTransport` 和 `CLIControlTransport` 的区别是什么？

`GrpcControlTransport` 是原生 RPC client。它直接构造 protobuf request，调用 `ControlService` 的 RPC，比如 `SubmitTask`、`SubmitWorkflow`、`CreateActor`、`CallActor`、`SubmitLLM`。提交 task 或 workflow 后，它还会轮询状态，直到任务成功或失败。

`CLIControlTransport` 是命令行桥接。它调用 `go run cmd/logservectl`，或者使用 `LOGSERVE_CLI` 指定的可执行文件。对于提交类命令，它把 JSON payload 写到 stdin；对于查询类命令，它把 id 转成命令行 flag。

两者对 SDK 上层暴露同一个 `run(command, payload)` 接口，所以 `LogServeClient` 不需要关心底层是 gRPC 还是 CLI。

差别主要在性能和部署体验。gRPC 更适合常规使用，少一层进程启动成本；CLI 更适合没有 Python gRPC 依赖或临时调试。

## Q580. SDK 为什么要读取 function source？

worker 最终要执行用户写的 Python 函数。Go control plane 本身不会理解 Python 函数对象，所以 SDK 必须把可执行的 Python source 一起提交过去。

普通 task 提交时，SDK 会用 `inspect.getsource(inspect.unwrap(fn))` 读取函数源码，并放到 `function_source` 字段。

workflow 构建 step 时，SDK 也会读取每个 task 的 source。为了让跨函数依赖更稳定，当前实现会优先读取 module source。如果 module source 可用，workflow 里的 step 会带整个模块源码，而不是只带单个函数。

actor 创建时也类似。SDK 会读取 actor class 所在模块的源码；如果读不到模块，就退回读取 class source。

这样 worker 收到任务后，可以在隔离的 namespace 里 `exec` 这段 source，再根据 `function_name` 或 `class_name` 找到要执行的对象。

这条路径有明显边界：`inspect.getsource` 依赖源码文件存在。交互式定义、动态生成函数、闭包捕获复杂对象，都可能不可靠。

## Q581. `executor/python/server.py` 的作用是什么？

`executor/python/server.py` 是 worker 执行 Python 用户代码的轻量服务。

它从 stdin 读取 JSON 请求，执行后把 JSON 结果写到 stdout。也支持 `--loop` 模式，worker 可以让它作为长驻进程反复处理请求。

普通 task 请求里会带：

- `function_source`
- `function_name`
- `args_json`

executor 会安装一个假的 `logserve` 模块，让用户源码里的 `from logserve import task`、`@task`、`@actor` 这些导入和装饰器不会在执行环境里报错。然后它执行 source，找到函数名，传入 args/kwargs，返回结果。

actor 请求则带 class source、class name、method name、state_json 和 init args。executor 会恢复 actor 对象的 `__dict__`，调用指定方法，然后把新 state 返回给 worker。

它不是完整沙箱。它解决的是“worker 如何运行 Python 函数和 actor 方法”这个问题，不负责强隔离、资源限制或安全审计。

## Q582. `ActorHandle` 如何把 `counter.inc()` 转成 `call_actor`？

`ActorHandle` 保存了 `actor_id` 和 client。

当用户写：

```python
counter.inc()
```

Python 会先查找 `ActorHandle` 上有没有 `inc` 属性。没有的话，会触发 `__getattr__`。

`ActorHandle.__getattr__` 会返回一个函数。这个函数被调用时，会执行：

```python
self.call("inc", *args, **kwargs)
```

`call()` 再调用 client 的 `call_actor(actor_id, method_name, ...)`，最终通过 transport 发给 Go control plane。

所以 `counter.inc()` 只是一个语法上的便利。它不会在 SDK 本地调用 Counter 的 Python 方法，而是转成一次 actor command 提交。

这也是 actor mailbox 能生效的前提：所有 actor 方法调用都要经过 control plane，不能绕过 runtime 直接改本地对象。

## Q583. SDK submit 如何区分 workflow 和普通 task？

`LogServeClient.submit()` 会检查函数上有没有 `_logserve_workflow` 标记。

如果有，就调用 `submit_workflow()`。

如果没有，就按普通 task 处理：读取函数源码，构造 payload，调用 transport 的 `submit` 命令。

所以用户可以写：

```python
submit(add, 1, 2)
submit(simple_rag, "query")
```

SDK 会根据装饰器标记决定走哪条路径。`@workflow` 函数走 workflow-submit；普通函数或 `@task` 函数走 task submit。

这个设计让入口简单，但也意味着装饰器标记很重要。一个 workflow 函数如果忘了加 `@workflow`，SDK 会把它当普通 task 提交。

## Q584. workflow 必须返回 `StepRef` 的原因是什么？

workflow 提交时，SDK 需要知道最终结果来自哪个 step。

在 tracing 过程中，每个 `@task` 或 `llm_generate()` 调用都会返回 `StepRef(step_id)`。workflow 最后返回其中一个 `StepRef`，SDK 就能把它写成 `result_step_id`。

control plane 执行 DAG 时，会根据 `result_step_id` 判断哪个 step 的结果是整个 workflow 的 final result。

如果 workflow 返回普通 Python 值，比如字符串或 dict，SDK 就没法知道这个值应该由哪个 runtime step 产生。更糟的是，workflow tracing 阶段并不会真正执行 task 函数，普通值可能只是构图时的本地对象，不代表 runtime 结果。

因此当前 SDK 明确要求 workflow 返回 `StepRef`。如果不是，`_build_workflow_definition()` 会抛出：

```text
workflows must return the result of a @task call
```

这条限制牺牲了一点 Python 灵活性，但换来 DAG 结果语义清楚。

## Q585. Python SDK 和 Go control plane 之间如何序列化参数？

参数通过 JSON 序列化。

普通 task 提交时，SDK 会把位置参数和关键字参数包装成：

```json
{
  "args": [...],
  "kwargs": {...}
}
```

gRPC transport 会把这个 JSON 编码成 UTF-8 bytes，放到 protobuf 的 `args_json` 或 `definition_json` 字段里。CLI transport 则把整个 payload 转成 JSON，通过 stdin 传给 `logservectl`。

workflow tracing 时，如果参数里包含 `StepRef`，SDK 会把它编码成：

```json
{"__step_ref__": "step_id"}
```

同时把这个 step_id 加到 `depends_on` 里。控制面后续调度 step 时，就知道这个参数需要等哪个上游 step 完成。

worker 执行 Python task 时，会把 `args_json` 传给 Python executor。executor 解码 JSON，取出 args/kwargs，再调用用户函数。

这意味着 SDK 参数必须是 JSON 可序列化的。复杂 Python 对象、文件句柄、数据库连接、lambda 等不能直接作为参数传过去。要传大对象时，应该走 result store 或对象存储引用。
