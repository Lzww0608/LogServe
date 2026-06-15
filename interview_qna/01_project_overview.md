# 一、项目总览与开场问题

这份题库按面试回答来写。真实回答时不需要逐字背，可以先讲主线，再按面试官追问展开。

## Q001. 请用 30 秒介绍 LogServe 是什么。

LogServe 是我做的一个基于 shared log 的 AI runtime。它不是只做一个任务队列，而是把任务执行、workflow DAG、有状态 actor、LLM serving、模型缓存感知调度和故障恢复放在一条链路里做。

最短可以这样说：

> LogServe 是一个用 Go 和 Python 写的 AI runtime。用户通过 Python SDK 提交 `@task`、`@workflow`、`@actor` 或 LLM 请求；Go 控制面先把事件写入 shared log，再更新当前状态视图；worker 从控制面拉取任务执行。系统可以在 worker 失败后通过 replay 恢复 workflow 和 actor 状态，也能根据 worker 上已有的模型缓存调度 LLM 请求，减少冷启动。

如果面试官只让讲 30 秒，我会把重点放在三件事上：log-first、replay recovery、LLM locality scheduling。

## Q002. 这个项目最核心的技术关键词是什么？

我会说这几个词：

- shared log
- log-first control plane
- materialized metadata view
- workflow DAG runtime
- replay recovery
- exactly-once-ish idempotency
- actor mailbox
- snapshot-aware retention
- model-cache-aware scheduling
- worker-local executor pool

其中 shared log 是底座。workflow、actor、LLM 的状态都写成事件流，控制面的内存状态和 metadata view 都是从这些事件 materialize 出来的。这样做的好处是故障恢复路径比较清楚：进程可以死，内存可以丢，但只要日志还在，就可以 replay 出当前状态。

我不会把它包装成严格 exactly-once 系统。更准确的说法是 exactly-once-ish：worker 可能重复执行，但控制面对最终状态写入做幂等检查，避免重复结果污染 workflow 或 actor 状态。

## Q003. LogServe 解决的具体问题是什么？

它解决的是 AI 应用运行时里的几个实际问题。

第一，AI workflow 往往不是单个函数调用。一个 RAG 任务可能包含 embed、search、build prompt、LLM generate。中间某一步失败后，不应该从头重跑所有步骤，尤其 embed、search 或 LLM 调用可能比较贵。LogServe 用 DAG step 和 replay 记录每一步状态，失败后可以从未完成的 step 继续。

第二，AI 应用里有状态对象越来越常见，比如 agent memory、会话状态、计数器、缓存管理器。普通任务队列天然更适合无状态任务。LogServe 加了 actor runtime，同一 actor 的请求进入 mailbox，按 `command_seq` 顺序应用，worker 挂掉后可以从 actor stream 和 snapshot 恢复内存状态。

第三，LLM serving 不只是“找个空 worker”。模型是否已经在 worker 本地缓存，会直接影响冷启动延迟。LogServe 让 worker 上报本地模型缓存，调度器可以优先把请求发给已有 checkpoint 的 worker。

第四，系统要能解释自己的状态。shared log 保留了 workflow、actor、LLM 的事件历史，metadata view 只是当前状态。出现故障时，可以拿 replay 状态和 DB/metadata 状态做对比。

## Q004. 它和普通任务队列有什么区别？

普通任务队列通常关心三件事：提交任务、worker 拉取任务、完成后返回结果。它可以做重试，也可以做延迟队列，但它通常不负责解释整个 workflow 的状态历史。

LogServe 多做了几层：

- 任务提交前后都会写 shared log，状态变化有事件可查。
- workflow 被拆成 DAG step，控制面只调度依赖满足的 step。
- actor 有 mailbox、ownership、epoch fencing 和 snapshot replay。
- LLM 请求带模型元数据，调度时会考虑 worker 是否已有模型缓存。
- metadata view 可以丢，恢复时从日志重建。

所以它更像一个小型 runtime，而不是单纯 queue。queue 是其中一部分，主要负责把 ready work 交给 worker。

## Q005. 它和 Airflow、Temporal、Ray、Celery 分别有什么相似点和差异？

和 Airflow 相似的地方是都有 DAG，都会表达多步骤任务之间的依赖。差异在于 Airflow 更偏离线数据管道和定时调度，适合 ETL、批处理、任务编排。LogServe 的重点是运行时语义，包括 shared log replay、worker redelivery、actor 状态恢复和 LLM 模型缓存调度。它没有 Airflow 那种成熟 UI、调度生态和生产部署能力。

和 Temporal 相似的地方是都把 durable execution 当成重点，都关心事件历史、worker 失败恢复和 workflow 状态重建。差异在于 Temporal 是成熟的通用工作流系统，语言 SDK、history service、matching service、visibility、版本兼容都完整得多。LogServe 更小，重点放在 AI runtime 的几个点：workflow DAG、actor、LLM serving、checkpoint cache、locality scheduling。它可以用来说明我理解 durable execution 的关键机制，但不能说已经达到 Temporal 的工程成熟度。

和 Ray 相似的地方是都有 worker、task、actor，也都可以承载 AI workload。差异在于 Ray 更偏分布式计算框架，强调并行计算、对象存储、资源调度和大规模集群。LogServe 没有做 Ray 那种通用分布式对象存储和大规模调度，它更强调 shared log、状态恢复、actor command log、LLM cache-aware scheduling。

和 Celery 相似的地方是都有任务提交、worker 执行、重试。差异在于 Celery 的核心是消息队列式任务执行，状态语义比较薄。LogServe 把任务状态、workflow 状态、actor 状态和 LLM 事件写入 shared log，并且用 replay 检查恢复结果。Celery 可以完成很多后台任务场景，但它不是为 workflow replay、actor recovery 或模型缓存调度设计的。

如果面试官问“你到底做了什么新的东西”，我会说：我不是重写这些系统，而是把它们和 AI serving 相关的几个机制压缩到一个可跑的实验系统里，重点展示 log-first runtime、stateful actor recovery 和 locality-aware LLM scheduling。

## Q006. 为什么你把它定义为 AI runtime，而不是普通 workflow engine？

普通 workflow engine 主要解决步骤编排、依赖、重试和状态持久化。LogServe 做了这些，但项目主线不止 workflow。

我把它叫 AI runtime，原因有三个。

第一，它支持 RAG 这类 AI workflow。`simple_rag` 里有 embed、search、generate_mock，后面又接了 `llm_generate()`。workflow step 不只是普通业务函数，也可以是模型调用前后的计算链路。

第二，它把 LLM serving 纳入 runtime。系统有 model registry、mock LLM、vLLM adapter、worker model cache 上报、checkpoint cache、冷启动指标和 LLM event log。调度器不是只看空闲 worker，还会看模型缓存和历史延迟。

第三，它支持有状态 actor。很多 AI agent 或会话型应用都有状态，不能只靠无状态任务。actor mailbox、snapshot replay、epoch fencing 这几件事，就是为了让状态对象在 worker crash 后还能继续工作。

所以 workflow engine 是它的一部分。AI runtime 更能描述这个项目的实际范围。

## Q007. 项目里哪些模块是你认为最有技术含量的？

我会讲四块。

第一块是 shared log 和 log-first 控制面。控制面先 append 事件，再更新 metadata view。这个顺序很重要，因为恢复时日志才是状态源。比如 workflow step 成功、actor command 提交、LLM 完成事件，都要能从日志重建。

第二块是 workflow runtime。Python SDK 在提交 workflow 时追踪 `@task` 调用，把函数调用关系转成 DAG。控制面根据依赖调度 ready step，失败后根据日志判断哪些 step 已经完成，避免从头重跑。

第三块是 actor runtime。这里最容易出问题的是并发顺序和旧 worker 写入。LogServe 用 `ActorCommandSubmitted` 记录命令进入 mailbox 的顺序，用 `command_seq` 保证只能按序应用，用 `owner_worker_id + epoch` 拒绝旧 owner 的完成。

第四块是 LLM scheduling。resource-only 策略只看 worker 空闲情况，locality-aware 会优先已有模型缓存的 worker，predicted-latency 使用 `LLMCompleted` 事件维护 EWMA stats，调度时不再扫描全部 `llm:*` stream。

如果只挑一个最能体现系统设计的点，我会选 log-first，因为后面的 replay、idempotency、dashboard 和故障恢复都靠它。

## Q008. 项目的主要使用路径是什么？用户如何提交一个任务？

最简单的路径是 Python SDK。

用户写一个函数，加上 `@task`：

```python
from logserve import task

@task
def add(a: int, b: int) -> int:
    return a + b

print(add(1, 2))
```

SDK 会把函数名、参数、模块信息、运行方式提交给 control。control 写 `TaskSubmitted` 事件，放入队列。worker poll 到任务后，通过 Python executor 执行函数，完成后把结果交回 control。control 再写完成事件，并更新任务状态。

workflow 的路径类似，只是 SDK 会先把 `@workflow` 内部的 task 调用记录成 DAG：

```python
@workflow
def simple_rag(query: str):
    vec = embed(query)
    docs = search(vec)
    ans = generate_mock(query, docs)
    return ans
```

actor 的路径是先 `create_actor(Counter)`，再调用 `counter.inc()` 或 `counter.get()`。这些调用会变成 actor command，进入同一个 actor 的 mailbox。

LLM 请求可以通过 `llm_generate("model-A", prompt, version="v1", adapter="mock")` 提交。控制面会按调度策略选择 worker。

## Q009. 控制面、worker、logd、SDK 分别承担什么职责？

SDK 面向用户。它把 Python 函数、workflow、actor 调用和 LLM 请求包装成提交请求。SDK 也负责 workflow tracing，把 Python 里的函数调用转成 DAG step。

control 是调度和状态中心。它接收提交请求，写 shared log，维护内存队列和 metadata view，处理重试、timeout、幂等、actor mailbox、ownership、model registry、scheduler policy 和 backpressure。

worker 是执行端。它注册到 control，持续 heartbeat，然后 poll task。拿到任务后，worker 会分发到本地 executor pool。普通 Python task 交给 Python executor，LLM task 调 mock/vLLM adapter，actor task 要带着 actor 当前状态执行。

logd 是 shared log 服务。它提供 append 和 read stream 的能力，底层有 segment rolling、fsync policy、启动恢复和 logical trim。它不理解 workflow 或 actor 的业务语义，只保证事件能按 stream 读写。

这四个组件的边界比较清楚：SDK 负责用户体验，control 负责决策，worker 负责执行，logd 负责持久事件。

## Q010. 为什么需要 Python SDK？为什么后端实现用 Go？

Python SDK 是因为目标用户写 AI workload 时大概率会用 Python。RAG、embedding、LLM 调用、数据处理、模型工具链，大部分都在 Python 生态里。让用户用 `@task`、`@workflow`、`@actor` 写代码，比让用户手动拼 gRPC 请求自然很多。

后端用 Go 是因为 control、worker、logd 这些组件更像基础设施服务。Go 写网络服务、并发 worker、日志服务、CLI 和单二进制部署都比较顺手。goroutine 和 channel 对 worker poll、heartbeat、executor pool 这类逻辑也合适。

还有一个工程上的考虑：Python 适合表达用户函数，Go 适合做稳定的 runtime 进程。两边通过 gRPC 和 Python executor bridge 连接。这个边界能避免把控制面写成一个长驻 Python 进程后，被用户代码、依赖冲突或 GIL 影响。

## Q011. 项目中的 shared log 是什么角色？

shared log 是状态源。LogServe 里很多状态都不是只存在数据库或内存里，而是先以事件形式写进日志。

比如：

- task stream 记录 `TaskSubmitted`、`TaskStarted`、`TaskCompleted`。
- workflow stream `wf:<workflow_id>` 记录 workflow 和 step 的状态变化。
- actor stream `actor:<actor_id>` 记录 actor 创建、ownership、command submitted、command applied、snapshot。
- LLM stream `llm:<task_id>` 记录 model load 和 LLM completed。

控制面运行时会把这些事件 materialize 成当前状态，给调度器、dashboard、查询 API 使用。进程重启后，可以从日志 replay。

我把 shared log 放在系统中心，是为了让故障恢复有一个明确的依据。内存状态快，但不可靠；日志慢一点，但可恢复。

## Q012. metadata view 和 shared log 的关系是什么？

shared log 是事实记录，metadata view 是当前视图。

更具体地说，shared log 保存“发生过什么”：某个 task 提交了，某个 step 成功了，某个 actor command 被应用了，某个 worker 完成了一次 LLM 请求。metadata view 保存“现在是什么状态”：这个 workflow 是否完成、这个 actor 当前 owner 是谁、这个 worker 缓存了哪些模型、dashboard 该显示多少个 task。

这个设计有两个好处。

第一，读取当前状态不用每次扫日志。调度器和 dashboard 可以直接看 materialized view。

第二，view 可以重建。control 重启时会从 shared log bootstrap workflow、actor、model、task 和 backpressure 状态。如果 view 和 replay 结果不一致，说明 materialization 或写入路径有 bug。

所以我会把 metadata view 类比成缓存，但它不是随便的缓存。它是从日志按规则投影出来的状态。

## Q013. LogServe 的最小可运行 demo 是什么？

最小 demo 是 `@task` 加本地 dev runner。

可以先启动：

```powershell
go run ./cmd/logserve-dev
```

然后在另一个 PowerShell 里运行：

```powershell
$env:PYTHONPATH = "$PWD\sdk\python"
python .\examples\hello_task\add.py
```

预期输出是：

```text
3
```

这个 demo 虽然简单，但它已经跑通了 SDK submit、control 写日志、worker poll、Python executor 执行、结果返回这条主链路。

如果想展示 workflow，可以跑：

```powershell
powershell.exe -ExecutionPolicy Bypass -File .\scripts\smoke_workflow.ps1
```

输出里会有：

```text
answer:hello:doc:vec:hello
```

这个例子能说明 DAG step 和依赖调度已经工作。

## Q014. 你如何证明项目不是只停留在 README 设计？

我会从三类证据回答。

第一，代码路径是完整的。仓库里有 `cmd/logserve-logd`、`cmd/logserve-control`、`cmd/logserve-worker`、`cmd/logserve-dev`、Python SDK、executor、workflow、actor、logstore、LLM scheduling、dashboard 和 benchmark 脚本。不是只有接口定义。

第二，有端到端 demo。`hello_task`、`simple_rag`、`actor_counter`、`rag_llm` 都能通过脚本运行。它们分别覆盖普通 task、workflow DAG、actor mailbox 和 LLM 调用。

第三，有实验结果和测试日志。单机 Ubuntu 实验里跑过 `go test ./...`、`go vet`、race test、Python unittest、compileall、logstore benchmark、fault injection、benchmark、checkpoint cache probe 和 dashboard snapshot。实验目录里有 `command_status.jsonl`、`benchmark.json`、`checkpoint_cache_probe.json`、`fault_injection.json`、`dashboard_snapshot.json`。

我不会说它已经是生产系统。更准确的说法是：它是一个能跑通关键机制的实验型 runtime，有代码、有 demo、有故障测试、有性能和消融结果。

## Q015. 你在项目中实现了哪些端到端测试或实验？

主要有这些。

普通 task 方面，测试覆盖 task 提交、worker heartbeat、poll、执行、状态查询和 task event log chain。

workflow 方面，测试覆盖 `simple_rag` 完成、worker 在 `embed` 后退出、重启 worker 后从 `search` 或后续 step 继续、replay 状态和 metadata 状态一致、重复 step completion 不重复写最终结果、失败 step retry、timeout retry。

actor 方面，测试覆盖 Counter actor 连续 `inc()`、worker 挂掉后另一个 worker 接管、`get()` 返回正确状态、1000 次并发 `inc()` 后最终值为 1000、snapshot replay 比 full replay 回放更少命令、旧 worker 的 stale completion 被 epoch fencing 拒绝。

LLM 方面，测试覆盖 resource-only、locality-aware、predicted-latency 三种策略，mock LLM event replay，file-backed checkpoint cache 冷热请求，以及 RAG workflow 中调用 `llm_generate()`。

实验脚本方面，`scripts/run_experiment.sh` 会在单机 Ubuntu 环境里启动 logd、control、3 个 worker，跑测试、benchmark、fault injection、checkpoint cache probe 和 dashboard snapshot。最后生成 `summary.md` 和 `summary.json`，便于写报告。

## Q016. 项目最难调试的问题是什么？

最难的是状态恢复和重复投递组合在一起时的边界。

一个任务失败并不复杂，重试就行。麻烦的是 worker 可能在不同时间点死掉：可能刚 poll 到任务就死，可能执行完但还没 complete 就死，也可能 complete 请求到了 control，但后续状态还没更新完。这个时候如果没有清楚的事件顺序，就很容易出现重复执行、重复写结果、workflow 从头跑、actor command 乱序这些问题。

workflow 里我用 `workflow_id + step_id + input_hash` 做 step 结果去重，失败 attempt 则带 attempt number，让真正需要重试的任务还能重跑。actor 里更严格一些，command 提交时先写 `ActorCommandSubmitted`，分配 `command_seq`，完成时只能应用 `actor.command_count + 1` 的命令。

另一个难点是旧 worker。worker 失联后，新 worker 可以接管 actor，但旧 worker 如果又把结果交回来，不能让它写入状态。所以 actor completion 要检查 owner worker 和 epoch。这个 bug 如果不做故障注入，平时很难看出来。

## Q017. 如果面试官只给你 5 分钟，你会重点讲哪三点？

我会按这三点讲。

第一，log-first runtime。LogServe 不是只把任务放进队列，而是先把 task、workflow、actor、LLM 事件写进 shared log，再 materialize 当前状态。这样 control 重启后能 replay，dashboard 和 DB 状态也有依据。

第二，workflow 和 actor 的恢复语义。workflow 支持 DAG、retry、timeout、result ref 和 step 去重；actor 支持 mailbox、`command_seq`、snapshot replay、ownership 和 epoch fencing。这部分能说明我处理过并发顺序、重复投递和 crash recovery。

第三，LLM locality scheduling。AI serving 场景里，模型缓存直接影响冷启动。LogServe 让 worker 上报本地 cache，调度策略从 resource-only 扩展到 locality-aware 和 predicted-latency。实验里 locality-aware cache hit rate 从 0.833 到 1.000，p95 latency 从 305 ms 到 205 ms。

这三点讲完，面试官基本能看到项目不是一个 CRUD demo，而是围绕运行时状态、恢复和调度做的系统。

## Q018. 如果面试官问“这和开源系统比有什么价值”，你会怎么组织思路？

我不会说 LogServe 比 Temporal、Ray 或 Airflow 更强。那不现实，也没必要。

我会这样组织：

第一，承认成熟系统的边界。Temporal 的 durable execution、Ray 的分布式计算、Airflow 的数据管道生态、Celery 的任务队列能力都很成熟。LogServe 是个人项目，不是替代它们。

第二，说明项目价值在于把关键机制自己做了一遍。比如 shared log、workflow replay、actor mailbox、epoch fencing、checkpoint cache、locality scheduling。这些机制在真实系统里分散在不同组件中。我把它们收敛到一个能运行、能实验、能解释的系统里。

第三，强调 AI workload 的切入点。普通 workflow 系统不一定关心模型缓存、checkpoint 冷启动、LLM event log、RAG workflow 和 actor state recovery 的组合。LogServe 把这些放在同一条实验链路里。

第四，用实验结果收尾。不是只讲设计，而是有单机实验、故障注入、benchmark 和消融对比。结果不能代表生产性能，但能说明机制跑通了。

这个回答的关键是不要硬碰成熟开源项目。更好的说法是：我通过这个项目拆解并复现了 AI runtime 里几个重要机制。

## Q019. 项目现在最明显的限制是什么？

限制比较清楚。

第一，实验主要是单机 Ubuntu 环境，3 个 worker，mock LLM。它能验证机制，但不能说明多节点生产性能。

第二，vLLM adapter 已经有，但没有在真实 GPU 负载下给出实验结论。现在 LLM 延迟主要来自 mock serving 和 checkpoint cache probe。

第三，shared log 做了 segment rolling 和 logical trim，但还没有真正物理删除 compacted segment。也就是说 retention 现在能标记 compactable records/bytes，完整 compaction 还没做完。

第四，metadata store 有 PostgreSQL 路径，但核心实验仍以本地脚本和单机流程为主。生产级部署还需要更完整的 HA、权限、安全、监控和运维配置。

第五，调度器策略还是相对简单。locality-aware 和 predicted-latency 已经能说明方向，但还没有做多租户、公平性、GPU memory、batching、prefill/decode 分离这些更复杂的问题。

我会主动讲这些限制，因为它们反而能说明我知道项目边界，没有把实验系统包装成生产平台。

## Q020. 如果让你重做一次，你会先实现哪条主链路？

我会先做最短但完整的 log-first task loop：

```text
Python SDK -> control -> shared log -> queue -> worker -> executor -> completion -> replay check
```

这条链路一旦稳定，后面所有东西都有地方接。workflow 是在 task 之上加 DAG 和依赖调度；actor 是在 task 之上加 mailbox、state 和 command log；LLM 是在 task 之上加 model metadata、adapter 和 cache-aware scheduling；dashboard 是从 metadata view 读取状态；benchmark 和 fault injection 是验证这些路径。

如果一开始就同时做 workflow、actor、LLM，很容易变成很多半成品。重做的话我会更早把 shared log 的事件格式、idempotency key、replay reducer 和 metadata materializer 定下来。这样后面扩展模块时，不会反复改状态模型。

简单说，我会先把“提交一个任务，worker 执行，失败后能从日志解释状态”做扎实。这个主链路做稳了，项目才有继续长大的空间。
