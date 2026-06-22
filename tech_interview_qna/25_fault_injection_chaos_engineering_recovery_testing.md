# 25. Fault injection、chaos engineering 与恢复测试

这一章回答故障注入、混沌工程和恢复测试相关的问题。面试里这类问题最容易被问成一句空话：系统能不能扛故障？更好的回答方式是把故障模型、注入位置、观测指标、恢复判定和边界说清楚。故障测试不是“把进程杀掉看会不会重启”，它要验证错误处理路径、状态恢复路径、超时与重试策略、数据一致性边界，以及系统在不完整失败下会不会进入更糟的状态。

下面的回答参考 Linux kernel fault-injection 文档、Principles of Chaos Engineering、AWS Fault Injection Service、Chaos Mesh、Kubernetes probes/Pod lifecycle、Linux `kill`/`signal`/`fsync` man-pages，以及 Google SRE 关于可靠性测试和级联故障的章节。结合 LogServe 时，我会把结论限制在当前项目边界内：它是单机 Ubuntu 环境下的 shared-log AI runtime 机制验证，已有 worker redelivery、control bootstrap、logstore recovery、actor snapshot replay、checkpoint cache 和 dashboard snapshot，不把它说成生产级多机混沌工程平台。

## Q001. fault injection 的目的是什么？

fault injection 的核心目的，是主动把系统平时很少走到的错误路径打出来，看系统在故障发生、传播、被检测、被隔离、被恢复的过程中是否符合预期。它不是为了“制造事故”，也不是为了证明系统永远不会失败。它更像一次受控审计：我先假设某个组件会失败，然后检查系统有没有把这个失败限制在可接受范围内。

这个问题可以从四个层次回答。

第一，验证错误处理代码真的能工作。大部分系统的 happy path 会被日常测试、联调和演示覆盖很多次，但错误路径经常只停留在代码分支里。比如 `append log` 失败后是否还更新 metadata view，worker lease 过期后旧 completion 是否还能写回，object store `Put` 超时后是否还写 `ActorSnapshotCreated`，这些都不能靠成功路径证明。fault injection 让这些分支被真实执行。

第二，验证恢复机制不是纸面设计。恢复机制常见说法包括 replay、checkpoint、redelivery、retry、fencing、failover。单看代码结构，很容易误以为有这些词就够了。真正要看的是故障之后系统能不能回到一个可解释状态。LogServe 里，worker 挂掉后任务要被 redelivery；control 重启后 metadata view 要从 shared log bootstrap；actor 要能通过 snapshot 加 tail log 恢复；旧 worker 的 completion 要被 lease epoch 拒绝。这些都需要故障测试把状态推到恢复路径上。

第三，确认故障边界和语义承诺。比如 shared log 的 fsync policy 不同，承诺就不同。`FsyncAlways` 下，如果 append 已经返回成功，崩溃后记录丢失就是严重问题；`batch` 或 `interval` 下，如果文档明确存在 durability window，那么未刷盘记录丢失可能符合语义。fault injection 的价值不只是找 bug，也包括逼你把“什么算正确”写清楚。

第四，暴露组合效应。单个组件看起来都能处理错误，组合起来可能会放大。一个慢依赖会导致请求堆积，请求堆积导致内存上涨，内存上涨导致 GC 或 OOM，随后健康检查失败，流量又被转移到剩余节点，最后形成级联故障。Google SRE 对级联故障的分析里反复强调，过载、重试、资源耗尽和健康检查会互相放大。fault injection 能把这些连锁反应提前暴露出来。

所以，面试里我不会把 fault injection 说成“随机 kill 服务”。更准确的回答是：

```text
fault injection 的目的是验证系统在非理想条件下的语义，而不是只看正常请求是否成功。
它会把 crash、timeout、slow IO、网络分区、磁盘错误、依赖失败、数据损坏等故障注入到指定位置，
然后检查检测、隔离、重试、降级、恢复和一致性是否符合预期。
对 LogServe 来说，重点不是证明它已经生产级高可用，而是证明 shared log、lease redelivery、control bootstrap、actor replay、checkpoint cache 这些机制在单机实验里真的走通。
```

做 fault injection 时要避免几个误区。

一个误区是只看服务最后有没有恢复。恢复成功只是结果之一，还要看恢复期间有没有重复执行、有没有写出不一致状态、有没有把错误吞掉、有没有导致上游无限重试。比如 LogServe 中任务 redelivery 后，旧 worker 如果晚到一个 completion，这个 completion 必须被 lease epoch 拒绝；否则表面上任务完成了，实际可能发生重复结果覆盖。

另一个误区是只注入最容易注入的故障。进程 kill 很方便，但它覆盖不了慢盘、partial write、网络抖动、依赖返回错误、对象存储返回损坏 payload、metadata store 写入变慢等场景。Linux kernel fault-injection 文档里列出的能力就很细，包括 slab/page allocation failure、futex failure、block IO error、NVMe fault、特定函数返回错误等。它的启发是：故障应该贴近系统真正依赖的资源，而不是只贴近测试工具最方便的动作。

还有一个误区是把 fault injection 和正确性测试分开。实际上它们要连在一起。注入故障之后，必须有 oracle，也就是判定结果对不对的规则。比如：

```text
任务最终状态必须是 SUCCEEDED 或 FAILED，不能永久 RUNNING。
同一个 idempotency key 不能生成两个互相冲突的最终结果。
control 重启后 dashboard state 和 replay state 要一致。
actor command_seq 必须单调，旧 epoch 不能写入。
日志 CRC 校验失败时不能把损坏记录当成有效事件。
```

没有这些检查，fault injection 只是在制造噪声。好的故障注入一定要带断言。

结合 LogServe，我会把 fault injection 的目的讲成三句话。

第一，验证 log-first 设计：所有关键状态变化先写 shared log，control 的 metadata view 可以从 log 重建，不能出现“内存 view 更新了但 log 没写”的状态。

第二，验证 at-least-once 执行和 exactly-once-ish 结果提交的边界：worker 可以重复拿到任务，但最终结果要靠 idempotency、lease epoch 和 step input hash 收敛。

第三，验证恢复证据可复查：fault injection 不是终端里看到 PASS 就结束，应该产出 `fault_injection.json`、`summary.json`、`dashboard_snapshot.json` 和组件日志，让别人能复盘故障发生时系统处在什么状态。

如果面试官追问“fault injection 最终能证明什么”，我会谨慎回答：

```text
它能证明被测试的故障模型下，系统的恢复路径符合预期；不能证明所有故障都被覆盖。
故障空间很大，所以要按影响面和发生概率挑高价值场景，比如 worker crash、control restart、log append failure、object store timeout、network partition、slow dependency 和 data corruption。
每个场景都要写清楚注入点、预期行为、检查指标和没有覆盖的边界。
```

## Q002. chaos engineering 和普通故障测试有什么区别？

普通故障测试更像“针对某个已知故障模式的验证”。chaos engineering 更像“围绕系统稳态假设的实验”。两者有重叠，但关注点不同。

普通故障测试通常比较确定。比如我写一个测试：worker poll 到任务后不调用 `StartTask`，等待 redelivery timeout，再让另一个 worker poll，断言同一个 task 被重新投递。这个测试有明确输入、明确步骤、明确断言，适合放进 CI。它回答的是：这条恢复分支有没有按预期工作。

chaos engineering 的起点不是某一行代码，而是系统稳态。Principles of Chaos Engineering 把流程说得很清楚：先定义 steady state，再假设对照组和实验组都能保持 steady state，然后注入接近真实世界的变量，最后观察实验组是否偏离 steady state。这里的 steady state 可以是成功率、p95/p99、错误率、队列长度、SLO violation rate、业务吞吐、cache hit rate 等。

举个例子，普通故障测试会这样问：

```text
kill 一个 worker 后，running task 是否会 redelivery？
control 重启后，metadata view 是否能从 shared log 重建？
logstore reopen 后，seq/index/idempotency 状态是否正确？
```

chaos engineering 会这样问：

```text
在持续提交 workflow 的情况下，每 30 秒随机 kill 一个 worker，系统的成功率、result-ready p99、queue depth 和 redelivery count 是否仍在可接受范围内？
当 control 和某个 worker 先后重启时，dashboard state、replay state 和用户可见结果是否保持一致？
当 log append latency 被人为拉高时，backpressure 是否能让系统降速，而不是把队列打爆？
```

差别不在于“有没有随机性”。很多成熟的 chaos 实验反而很克制，故障是精心挑选的，blast radius 也很小。差别在于实验对象从单个代码分支上升到系统行为。

可以从几个维度区分。

第一，目标不同。普通故障测试目标是验证某个恢复逻辑。chaos engineering 目标是增加对系统在真实扰动下保持稳态的信心。AWS FIS 的文档也强调，它会对真实 AWS workload 执行真实动作，观察应用如何响应，并提供 stop condition 这类保护。这说明混沌实验不是单元测试，它有运行环境和安全控制问题。

第二，运行环境不同。普通故障测试常在本地、CI、集成环境跑。chaos engineering 更接近预生产或生产环境，因为系统在测试环境和生产环境的流量、数据、依赖、配置、资源限制通常不同。Principles of Chaos Engineering 提到偏好在生产流量上实验，但这不是让人一上来就在生产乱动。正确做法是先在本地和预生产里把工具、断言、回滚、blast radius 练熟，再逐步靠近真实环境。

第三，观测方式不同。普通故障测试的断言常常是布尔值：是否 redeliver、是否拒绝 stale completion、是否恢复 metadata。chaos engineering 必须看时间序列和稳态指标，比如 p99 是否飙升、错误率是否超过 SLO、队列是否持续增长、恢复耗时是否在预算内、是否有 retry amplification。

第四，安全机制不同。普通测试失败了大不了测试红。chaos 实验可能影响真实资源，所以必须有范围控制、权限控制、停止条件、回滚路径和实验记录。AWS FIS 的 experiment template 包含 actions、targets、stop conditions；Chaos Mesh 也有 selector、duration、mode 等字段来限制目标和持续时间。没有这些护栏，只能叫“破坏性测试”，不能叫工程化的 chaos experiment。

第五，组织方式不同。普通故障测试一般由研发维护，和代码一起进 CI。chaos engineering 常常涉及 SRE、平台、业务 owner、值班和监控规则，因为它要验证的是系统级承诺。比如一次实验可能需要提前确认报警静默策略、客户影响面、回滚负责人、实验窗口和中止条件。

在 LogServe 语境下，我会这样说：

```text
LogServe 当前已经有一些 fault injection 和恢复测试，比如 worker redelivery、control restart bootstrap、actor snapshot replay、logstore recovery。
这些更接近普通故障测试和实验脚本，能证明单机机制路径。
如果要升级成 chaos engineering，需要在持续 workload 下编排组合故障，定义 steady state，比如 workflow success rate、result-ready p99、queue depth、redelivery count、stale completion rejection count、dashboard/replay 一致性。
还要有 stop condition 和 blast radius 控制。当前项目还没到完整生产混沌工程阶段。
```

面试里这句话很重要：chaos engineering 不是“随机搞挂系统”。随机性只是一种手段，核心是实验。没有假设、没有稳态指标、没有安全边界、没有回滚和分析，就不是合格的 chaos engineering。

可以把两者关系总结成一张表：

| 维度 | 普通故障测试 | Chaos engineering |
|---|---|---|
| 起点 | 已知故障路径 | 系统稳态假设 |
| 目标 | 验证某个恢复分支 | 发现系统性弱点，建立抗扰动信心 |
| 环境 | 本地、CI、集成环境较多 | 预生产或受控生产环境更有价值 |
| 故障 | 通常单点、确定、可重复 | 可以组合、定时、随机，但必须受控 |
| 断言 | 状态、返回值、日志、数据一致性 | SLO、错误率、p99、吞吐、队列、业务稳态 |
| 安全 | 测试失败即停止 | 需要 blast radius、stop condition、回滚和审计 |
| 产物 | 测试报告、CI 结果 | 实验记录、指标曲线、复盘、改进项 |

如果面试官问“那是不是普通故障测试不重要”，答案正好相反。普通故障测试是 chaos engineering 的地基。没有确定性的恢复测试，直接做 chaos 只会得到一堆难解释的现象。我的做法会是先把关键故障路径做成小而确定的测试，再用 chaos 实验验证这些路径在持续 workload 和组合故障下是否仍然成立。

## Q003. 为什么只测试 happy path 不足以证明系统可靠？

因为可靠性主要取决于非 happy path。正常请求成功，只能证明系统在理想条件下能完成业务；它不能证明系统在重试、超时、部分失败、资源耗尽、数据损坏、进程重启、依赖变慢时仍然能保持正确边界。

happy path 测试通常覆盖这些内容：

```text
提交任务 -> 调度成功 -> worker 执行 -> 写回完成 -> 查询结果。
提交 workflow -> step 按依赖执行 -> 所有 step 成功 -> workflow 完成。
提交 actor command -> owner worker 执行 -> state 更新 -> 返回结果。
提交 LLM request -> 命中或加载模型 -> 返回 mock/vLLM 结果。
```

这些当然要测，但它们只覆盖了系统最顺的一条路径。分布式系统、runtime、队列、日志系统真正麻烦的地方，往往发生在“操作进行到一半”的时候。

比如任务系统里，happy path 不能回答这些问题：

```text
worker poll 到任务后，还没 StartTask 就挂了怎么办？
worker 已经 StartTask，执行到一半挂了怎么办？
worker lease 已经过期，但旧 worker 后来又提交 completion，是否会污染最终结果？
control 重启后，内存队列丢了，任务还能从 log 恢复吗？
同一个 idempotency key 被客户端重试提交，payload 不一致时如何处理？
```

workflow 里也一样。happy path 能证明 DAG 能跑完，不能证明已完成 step 不会在恢复后重复执行，不能证明失败 step 的 retry 不会打爆系统，不能证明 result ref 丢失后系统会给出可解释错误。

actor 更明显。actor 的核心语义是串行 mailbox、command_seq、snapshot replay 和 epoch fencing。happy path 只说明 command 能执行。真正要测的是 owner 切换、旧 epoch 写回、snapshot 写入失败、snapshot 已写但 log 事件没写、logical trim 后 replay 是否仍然正确。

LLM serving 也不能只看正常返回。真实问题常在冷启动、checkpoint cache miss、object store 慢、模型版本不存在、vLLM endpoint 超时、GPU OOM、调度器选错 worker 时出现。mock LLM happy path 只能证明接口通了，不能证明真实 serving 可靠。

happy path 不足的根本原因，是它不覆盖状态空间。系统在成功路径上通常是线性的：

```text
submitted -> scheduled -> running -> completed
```

故障路径会把状态空间变成网状：

```text
submitted -> leased -> worker lost -> redelivered -> stale completion arrives
submitted -> running -> timeout -> retry -> duplicate completion
snapshot writing -> object store timeout -> log event should not be appended
append log success -> metadata update failed -> replay should repair view
append log returned success -> crash -> recovery must match fsync policy
```

这些路径靠代码审查很难完全确认，靠 happy path 更确认不了。

Google SRE 的可靠性测试章节里有一个很实用的观点：通过测试可以减少系统变化带来的不确定性，但通过一组测试并不等于证明系统可靠；失败的测试通常能证明可靠性缺失。这个说法很适合面试。可靠性不是单个 PASS，它是对风险空间的逐步收敛。

只测 happy path 还会带来三个具体风险。

第一，错误处理代码腐烂。很多错误处理分支从写完后从未执行过，变量没初始化、锁没释放、context 没取消、错误被吞掉、指标没上报。这类问题在正常请求中不会暴露。

第二，恢复路径和正常路径不一致。比如 control 正常运行时 metadata view 是正确的，但重启后 bootstrap 少回放了一类事件；actor 正常执行时 state 对，但从 snapshot 恢复时漏了 tail log；LLM 正常调度时 cache hit 对，但 replay LLM 时读不出 checkpoint_fetch_ms。这些都要通过恢复测试发现。

第三，系统性弱点会被隐藏。happy path 往往没有排队、没有超时、没有重试风暴、没有慢依赖。真实故障中，一个小问题会改变流量分布和资源占用，最终变成级联失败。Google SRE 对级联故障的分析里提到，过载会导致请求变慢、in-flight 增多、队列变长、deadline miss、retry 增加，最后把剩余容量也拖垮。happy path 看不到这种反馈环。

对 LogServe，我会把“happy path 不足”落到这些测试缺口上：

- 只跑 workflow 成功，不足以证明 worker kill 后 workflow 能从已完成 step 继续。
- 只跑 actor command 成功，不足以证明 snapshot replay 和 epoch fencing 正确。
- 只跑 log append/read 成功，不足以证明 crash 后 segment/index/idempotency 状态恢复正确。
- 只跑 mock LLM 成功，不足以证明 checkpoint cache miss、vLLM timeout、模型加载失败路径正确。
- 只跑 dashboard 导出成功，不足以证明故障期间 dashboard state 和 replay state 一致。

面试可以这样回答：

```text
happy path 测试证明系统在理想条件下能工作，但可靠性主要取决于非理想条件下是否仍然保持语义。
分布式系统的关键风险在中途失败：任务已 lease 但未 start、旧 worker 晚到 completion、control 重启丢失内存状态、log append 和 metadata update 之间失败、snapshot 写一半失败、依赖变慢导致重试放大。
所以我会把 happy path、故障注入、恢复测试和一致性检查放在一起。
对 LogServe 来说，happy path 只是入口；更关键的是 redelivery、fencing、replay、bootstrap、fsync policy 和 dashboard/replay 一致性。
```

还有一个细节：只测 happy path 容易让指标也失真。比如 benchmark 在没有故障时 p99 很好，但一旦 worker 变慢，queue wait 才是主要延迟来源；没有 fault injection，你根本不知道应该监控哪个指标。故障测试能反过来指导 observability：你会知道该记录 redelivery count、lease expired count、stale completion rejection、log append error、checkpoint fetch timeout、actor mailbox backlog。

## Q004. 常见故障类型有哪些：crash、hang、slow、partial failure、data corruption？

常见故障类型可以按“表现”和“影响层次”拆开。面试里只背名词没有意义，最好说明每种故障会破坏什么假设，以及系统应该怎么检测和恢复。

### crash

crash 是组件直接停止工作。它可能是进程退出、容器被 kill、节点宕机、panic、OOM、机器重启。crash 的特点是明显：心跳消失、连接断开、进程不存在、Pod 重启、任务没有继续执行。

crash 相对容易检测，但不代表容易处理。系统必须回答：

```text
谁负责发现它死了？
多久发现？
发现之前任务处于什么状态？
正在执行的请求是否会重试？
旧进程如果恢复或延迟写回，是否会被 fencing？
持久化状态是否足够恢复？
```

在 LogServe 中，worker crash 对应 task lease 过期和 redelivery。control crash 对应从 shared log bootstrap metadata view。logd crash 对应 logstore reopen、segment/index 恢复、fsync policy 边界。actor owner crash 对应 ownership epoch 和 snapshot/tail replay。

### hang

hang 是组件还活着，但不再推进。它比 crash 更危险，因为表面信号可能还是正常的。进程存在，TCP 连接也可能还在，甚至健康检查如果写得太浅也会通过，但业务请求一直卡住。

hang 常见原因包括死锁、线程池耗尽、goroutine leak、阻塞在某个外部依赖、GC 长暂停、文件锁等待、无限循环、队列消费停止。对 runtime 系统来说，hang 往往比 crash 更难恢复，因为系统可能误以为 worker 还活着。

检测 hang 不能只看进程是否存在。要看 progress：

```text
heartbeat 是否更新？
lease 是否续约？
任务状态是否长时间停在 RUNNING？
queue depth 是否持续增长？
actor mailbox oldest pending age 是否变大？
log append latency 是否超过阈值？
```

Kubernetes liveness probe 的设计就是为了处理“进程还在但已经坏掉”的场景。它会根据命令、HTTP、TCP 或 gRPC 探针判断容器是否需要重启。readiness probe 则负责把暂时不能接流量的实例摘出服务。把这个思路迁移到 LogServe，就是 worker 不能只注册一次，还要持续心跳和报告 capacity/running tasks；control 不能只看 worker 进程存在，还要看任务是否推进。

### slow

slow 是系统还能处理请求，但速度显著下降。慢故障最容易被低估，因为它不是立即失败，却会造成排队、超时、重试和资源耗尽。

常见 slow 故障包括慢磁盘、慢 fsync、慢 object store、慢数据库查询、慢 LLM prefill/decode、网络延迟上升、CPU 被抢占、GC 变长。slow 的风险在于它会把一个局部问题扩散到整个系统。比如 log append 慢，control submit 变慢；submit 堆积后 client 重试；重试增加后 shared log 压力更大；最终 p99 和错误率一起上升。

slow 故障要靠 latency breakdown 观察，不能只看总耗时。LogServe 里应该拆：

```text
control queue wait
log append latency
worker local queue wait
executor runtime
checkpoint fetch latency
model load latency
first token latency
completion append latency
```

对 slow 的处理也不只是重试。很多 slow 故障下，盲目重试会雪上加霜。更合理的手段是 timeout、deadline 传播、backpressure、限流、降级、快速失败和优先级队列。

### partial failure

partial failure 是分布式系统里最典型的故障：不是所有东西都坏了，而是一部分坏了，另一部分还在运行。比如一个 worker 看不到 control，但 control 看得到其他 worker；control 能写 log，但 object store 慢；某个 AZ 网络分区；某个模型只在一台 worker 上可用；一部分请求成功，一部分请求超时。

partial failure 的困难在于观察者不同，看到的世界不同。客户端觉得请求超时，不知道服务端是否已经执行；worker 觉得 completion 发出去了，不知道 control 是否接受；control 觉得 worker lease 过期，不知道 worker 是否还在本地执行。

这类故障必须靠幂等和 fencing 控制。LogServe 里：

- task completion 要检查 lease/epoch，旧 worker 不能覆盖新结果。
- workflow step 要靠 `workflow_id + step_id + input_hash` 做 exactly-once-ish 结果收敛。
- actor 要靠 owner epoch 和 command_seq 防止旧 owner 写入。
- metadata view 不是 source of truth，control 重启后要从 shared log 重建。
- object store 成功与 log event 写入之间要有明确顺序，不能产生悬空引用或不可恢复 snapshot。

partial failure 不能靠单个 health check 解决。它需要端到端一致性检查：最终状态是否收敛，重复请求是否幂等，超时后 late response 是否被正确处理。

### data corruption

data corruption 是数据被写坏、读坏、被截断、被错序解释、checksum 不匹配、schema 解释错误、对象内容和 manifest 不一致。它比 crash 更危险，因为系统可能继续运行，但基于错误数据做决定。

数据损坏可以发生在很多层：

```text
log record payload 损坏。
segment 尾部 partial record。
index 指向错误 offset。
object store 返回损坏 snapshot。
checkpoint manifest 和 checkpoint 文件不一致。
metadata view 从错误事件重建。
protobuf/JSON schema 变更导致旧数据被误读。
```

data corruption 的基本防线是校验和、长度、magic number、version、schema evolution、幂等键、单调 seq、replay 校验。故障注入要覆盖“坏数据被检测出来”而不是只覆盖“坏数据不存在”。比如把 log 尾部截断一半，系统应该识别 partial tail；把 CRC 改坏，系统应该拒绝读取或明确报错；把 snapshot object 换成非法内容，actor replay 不应该悄悄恢复出错误状态。

可以把常见故障类型整理成表：

| 故障类型 | 表现 | 难点 | 典型检测 | LogServe 中的关注点 |
|---|---|---|---|---|
| crash | 进程/容器/节点退出 | 恢复时是否重复执行或丢状态 | heartbeat missing、process exit、Pod restart | worker redelivery、control bootstrap、logstore recovery |
| hang | 活着但不推进 | 健康检查可能误判 | progress timeout、lease expired、queue age | running task 卡住、actor mailbox backlog、worker poll 停滞 |
| slow | 请求仍成功但变慢 | 排队和重试会放大 | p95/p99、queue depth、latency breakdown | log append slow、checkpoint fetch slow、LLM model load slow |
| partial failure | 一部分组件失败 | 不同组件看到的状态不一致 | end-to-end consistency check、fencing | stale completion、epoch、idempotency、replay state |
| data corruption | 数据内容错误 | 可能静默传播 | CRC、length、schema、manifest 校验 | log CRC、segment tail、snapshot object、checkpoint manifest |

面试回答可以这样收束：

```text
我会把故障分成 crash、hang、slow、partial failure 和 data corruption。
crash 最直观，但不是最难；hang 和 slow 更容易造成排队、超时和重试放大；partial failure 需要幂等、fencing 和最终收敛；data corruption 必须靠校验和、版本、长度、单调序列和 replay 校验发现。
LogServe 的测试不能只 kill worker，还要覆盖 lease 过期、旧 completion、control restart、slow log append、object store timeout、snapshot 损坏和 log recovery。
```

## Q005. 进程 kill 和机器断电能覆盖同样的故障吗？

不能。进程 kill 和机器断电有重叠，但不是同一个故障模型。把二者混为一谈，是可靠性测试里很常见的误判。

进程 kill 主要模拟的是某个进程突然停止。比如 `kill -9` 一个 worker，进程没有机会跑应用层清理逻辑，内存状态丢失，连接断开，其他组件需要通过心跳、lease 或连接错误发现它消失。Linux `kill(2)` 的语义是给进程或进程组发送 signal；`signal(7)` 里也说明了不同 signal 的默认动作，`SIGKILL` 不能被 catch、block 或 ignore。也就是说，`kill -9` 对应用进程很硬，但它仍然只是进程级事件。

机器断电覆盖的是更大的故障面。它会同时影响机器上的所有进程、内核 page cache、磁盘写回、网络连接、本地临时文件、容器 runtime、多个共址服务、系统时钟恢复、设备缓存、未完成 IO。断电后重启，还会进入 cold start：文件系统恢复、服务重新拉起、缓存丢失、连接池重建、worker 重新注册、logstore reopen。

二者的差异可以从几个方面看。

第一，影响范围不同。kill 一个 worker，只影响这个 worker。机器断电会影响同机的 worker、control、logd、object store mock、dashboard、压测客户端，甚至测试 harness。如果 LogServe 在单机环境里 control、logd 和 worker 都跑在同一台机器，断电就是整套 runtime 同时消失；kill worker 只是执行节点消失。

第二，持久化语义不同。kill 进程时，操作系统和磁盘还在。已经进入内核 page cache 的数据可能随后被刷盘；其他进程也可能继续运行。断电时，page cache 和未完成写回会丢失，设备缓存也可能有自己的风险。`fsync(2)` 的文档强调，它会把文件的 modified in-core data 刷到存储设备，并阻塞到设备报告完成；这正是断电模型和进程 kill 模型的差别所在。没有 fsync 或明确 durability window，断电后不能假设成功写入的数据都在。

第三，网络行为不同。kill 进程时，内核通常会关闭 socket，peer 可能收到 FIN/RST，故障相对“干净”。机器断电、网线拔掉、内核级崩溃、交换机故障可能表现为连接黑洞：对端只看到超时，不一定立即知道另一端死了。分布式系统里，超时比明确拒绝更麻烦，因为请求可能已经执行，也可能根本没到。

第四，恢复路径不同。进程 kill 后，supervisor、systemd 或 Kubernetes 可以只重启一个容器。机器断电后，整台机器恢复，所有服务都 cold start，启动顺序也可能变化。比如 logd 还没恢复时 control 启动，control 是否 fail fast？worker 比 control 先启动，注册是否重试？checkpoint cache 是否仍在本地磁盘？这些问题 kill 单个进程测不到。

第五，数据损坏风险不同。kill 进程通常不会制造磁盘 partial write，除非进程正在写文件且没有正确处理中断。断电更容易暴露 WAL、segment、index、rename、fsync directory、设备缓存这类 crash consistency 问题。对 LogServe 的 logstore 来说，真正要测的是 append 返回成功前后崩溃、segment 尾部 partial record、index 与 log 不一致、idempotency 状态恢复，而不只是 logd 进程能不能重启。

所以，进程 kill 能覆盖这些场景：

```text
worker 进程突然消失。
control 进程重启。
logd 进程退出后 reopen。
应用层内存状态丢失。
TCP 连接断开。
lease/heartbeat/redelivery 是否生效。
旧 completion 是否被 fencing。
```

机器断电能覆盖这些额外场景：

```text
同一节点上多个组件同时消失。
page cache 中未 fsync 的数据丢失。
设备写缓存和文件系统恢复行为。
服务冷启动顺序不确定。
本地缓存、临时目录、锁文件、PID 文件残留。
网络对端可能先看到 timeout，而不是明确连接关闭。
节点回来后旧进程状态全部丢失，只有持久化状态可用。
```

对 LogServe，我会这样设计分层测试。

第一层是进程级 kill。用它验证 worker redelivery、control restart bootstrap、logd reopen、旧 completion rejection。这些测试便宜、可重复，适合 CI 或本地集成测试。

第二层是 crash consistency harness。启动 logd，持续 append；把客户端已收到成功响应的 idempotency key 记录到外部确认文件；随机 `kill -9` logd；用同一个 data dir 重启；检查 CRC、seq、index、idempotency 和 confirmed records 是否符合 fsync policy。这比简单 kill 更接近存储故障。

第三层是节点级或虚拟机级断电。用 VM snapshot、断电模拟、云主机 stop/start 或受控电源故障模拟整机消失。重启后检查 control、logd、worker 启动顺序、shared log recovery、dashboard state、workflow/actor replay、checkpoint cache 和未完成任务 redelivery。

第四层是依赖级故障。机器没断电，但 object store 超时、PostgreSQL 慢、网络分区、磁盘 ENOSPC、fsync 卡住。这些和 kill/断电都不同，仍然要单独测。

面试回答可以这样说：

```text
进程 kill 和机器断电不能互相替代。
kill 主要验证进程级失败后的 lease、redelivery、restart 和 fencing；机器断电还会涉及 page cache、fsync、设备缓存、多个组件同时消失、冷启动顺序和本地持久化恢复。
所以我会用 kill 做便宜、确定、可重复的恢复测试，用 crash harness 和节点级故障补存储一致性与整机恢复边界。
对 LogServe，worker kill 能证明任务 redelivery，但不能证明 shared log 在断电后的 fsync 语义；logd restart 能证明 reopen 路径，但不能完整覆盖 partial write 和设备级故障。
```

这里还要补一句边界说明。当前 LogServe 的实验结果已经能说明 worker kill recovery、queue redelivery、control restart probe 和 logstore recovery 相关路径在单机实验里跑通。它还不能证明真实机器断电、磁盘控制器异常、多副本 log 服务、跨节点网络分区下的生产可用性。这个边界说清楚，反而更可信。

## Q006. 如何注入磁盘写失败？

磁盘写失败要先问清楚一件事：你想测的是应用层处理 `write` 返回错误，还是想测文件系统、块设备、page cache、fsync、rename、目录同步这一整套持久化路径。两者都叫“写失败”，但覆盖面差很多。

最便宜、也最适合 CI 的办法，是在应用内部加可控 failpoint。比如 LogServe 的 `logd` 不应该把 `os.File` 到处传，而是把写入路径收敛到很小的接口：

```go
type segmentFile interface {
    Write(p []byte) (int, error)
    Sync() error
    Close() error
}
```

测试时用 wrapper 控制失败点：

```go
type faultFile struct {
    inner      segmentFile
    failAfter  int64
    written    int64
    err        error
}

func (f *faultFile) Write(p []byte) (int, error) {
    if f.failAfter >= 0 && f.written >= f.failAfter {
        return 0, f.err
    }
    n, err := f.inner.Write(p)
    f.written += int64(n)
    return n, err
}
```

这种方式不是“真实磁盘坏了”，但很有用。它能稳定覆盖这些问题：

- `Write` 返回 `EIO`、`ENOSPC`、`EDQUOT`、`EACCES` 时，append 是否返回失败。
- partial write 发生后，record 长度、CRC、index 和 durable watermark 是否仍然一致。
- 写 segment 成功但写 index 失败时，恢复逻辑是否以 log segment 为准，而不是盲信 index。
- 写 data 成功但后续 `fsync` 失败时，系统有没有把这次 append 当成 durable。
- rotate segment 时，new segment、old segment、manifest、directory fsync 的顺序是否可恢复。

面试里如果只说“我 mock 掉 write 返回错误”，答案还不够。mock 只能证明错误分支没漏写，不能证明内核和文件系统边界下也正确。更强的做法是把注入层次分开。

第一层是应用层 failpoint。它最适合单元测试和集成测试。失败点应该放在具体语义边界上，而不是随便 `return error`：

```text
append record header 前失败
append record payload 中间失败
append record payload 后、index 前失败
index 写入后、fsync 前失败
fsync 返回 EIO
rename temp file 前失败
rename 成功后、fsync parent directory 前失败
```

这样测出来的问题通常更接近真实恢复路径。比如 LogServe 的 logstore 不能只检查“写文件是否报错”，还要检查“恢复时是否能识别 segment 尾部的半条 record”。半条 record 如果被当成完整事件读出来，workflow、actor、metadata view 都会被污染。

第二层是文件系统调用级注入。Kubernetes 场景下可以用 Chaos Mesh 的 `IOChaos`。官方文档里 `IOChaos` 支持 `latency`、`fault`、`attrOverride`、`mistake`，其中 `fault` 可以让文件系统调用返回指定 errno，`mistake` 可以把 read/write 的内容改坏，方法列表也包括 `write`、`flush`、`fsync`、`fsyncdir`。例如只在测试 Pod 的 log 目录下注入 50% 的 I/O error：

```yaml
apiVersion: chaos-mesh.org/v1alpha1
kind: IOChaos
metadata:
  name: logserve-logstore-write-eio
  namespace: chaos-mesh
spec:
  action: fault
  mode: one
  selector:
    labelSelectors:
      app: logserve-logd
  volumePath: /var/lib/logserve
  path: /var/lib/logserve/logd/**/*
  methods:
    - WRITE
    - FSYNC
    - FLUSH
  errno: 5
  percent: 50
  duration: 60s
```

这里 `errno: 5` 对应 I/O error。真实配置要以环境支持的方法名为准，先在隔离 Pod 里验证，不要直接对生产 PVC 做这类实验。Chaos Mesh 文档也明确提醒 `IOChaos` 可能损坏数据，这一点面试时要说出来。可靠性测试不是拿生产数据冒险。

第三层是块设备级注入。Linux kernel 的 fault injection 文档里有 `fail_make_request`，它可以对被允许的 block device 注入磁盘 I/O error，并通过 debugfs 控制 probability、interval、times 等参数。这个层次更接近真实块设备错误，但也更危险，必须用 loop device、临时盘或测试 VM：

```bash
dd if=/dev/zero of=/tmp/logserve-disk.img bs=1M count=1024
losetup --find --show /tmp/logserve-disk.img
mkfs.ext4 /dev/loop10
mkdir -p /mnt/logserve-fault
mount /dev/loop10 /mnt/logserve-fault

echo 1 > /sys/block/loop10/make-it-fail
echo 100 > /sys/kernel/debug/fail_make_request/probability
echo 10 > /sys/kernel/debug/fail_make_request/interval
echo 5 > /sys/kernel/debug/fail_make_request/times
```

上面只是形态示例，设备名和权限要按实验机实际情况调整。重点不是背命令，而是说明它的测试语义：这会让 block layer 返回 I/O error，应用看到的可能是 `write`、`fsync`、`close` 或后续读恢复时报错。也就是说，错误不一定发生在你以为的那一次 `write` 上。

第四层是 device-mapper。`dm-flakey` 官方文档说明它可以周期性表现出不可靠行为，用来模拟 failing devices；它支持 `error_writes`、`drop_writes`、`corrupt_bio_byte`、`random_write_corrupt` 这类特性。对 LogServe 这种 append-only log，`dm-flakey` 很适合测两类问题：写返回错误，以及写看似成功但底层数据被丢弃或损坏。

```bash
# 示例：基于测试 loop device 创建一个周期性失败的 dm 设备。
# 真实命令需要先确认 blockdev --getsz 的输出和目标设备没有挂错。
sectors=$(blockdev --getsz /dev/loop10)
dmsetup create logserve-flakey --table "0 $sectors flakey /dev/loop10 0 20 10 1 error_writes"
mkfs.ext4 /dev/mapper/logserve-flakey
mount /dev/mapper/logserve-flakey /mnt/logserve-fault
```

这里的面试回答可以落到 LogServe：

```text
我会把磁盘写失败分成应用层 failpoint、文件系统调用级注入和块设备级注入。应用层 failpoint 用来稳定覆盖 append、index、fsync、rename、dir sync 的错误分支；IOChaos 或 FUSE 这类方法用于验证真实文件系统调用返回 EIO/ENOSPC 时的行为；fail_make_request、dm-flakey 这类块设备注入用于验证 segment 尾部 partial record、CRC、index rebuild 和 durable watermark。对 LogServe 来说，核心断言不是“错误被打印出来”，而是失败后 shared log 不承诺未持久化记录，恢复时不会读出半条 event，重复 append 能靠 idempotency key 收敛，metadata view 只能从合法日志重建。
```

还要补一句边界。当前 LogServe 的实验已经覆盖 worker kill、queue redelivery、control restart、logstore recovery 等路径，但这不等于已经覆盖真实磁盘控制器、设备缓存、文件系统 journal 和断电场景。磁盘写失败注入是下一层硬化工作，不能把 process restart 测试包装成完整 storage fault testing。

## Q007. 如何注入 fsync 慢？

`fsync` 慢和 `write` 失败不是一类问题。`write` 失败通常是明确错误；`fsync` 慢更像系统变得“还活着，但每一步都卡”。它会暴露锁粒度、队列堆积、timeout、backpressure、批量刷盘策略和客户端 deadline 传播。

最直接的办法还是应用层 hook。把 `fsync` 收敛到一个接口：

```go
type syncer interface {
    Sync() error
}

type slowSyncer struct {
    inner syncer
    delay time.Duration
}

func (s *slowSyncer) Sync() error {
    time.Sleep(s.delay)
    return s.inner.Sync()
}
```

这个测试便宜、稳定，适合验证控制面行为。比如 `always fsync` 策略下每次 append 都被拖慢 200ms，`batch fsync` 策略下每批被拖慢 200ms，`interval fsync` 策略下 durable lag 会扩大。它能检查 LogServe 是否出现这几类问题：

- append 请求是否被无界堆在内存里。
- logstore 的全局锁是否覆盖了过长的 `fsync` 阶段，导致读请求、recover 或其他 stream append 被一起卡住。
- control 写 shared log 慢时，是否仍然先写 log 再改 metadata view。
- worker completion 因 log append 慢而超时后，重试是否幂等。
- benchmark 是否同时报告 throughput、p95/p99 append latency、durable lag、queue depth，而不是只给平均吞吐。

不过应用层 `sleep` 有一个弱点：它不一定复现真实 flush 卡住时的线程状态。真实 `fsync` 会在内核里等待脏页、journal、block layer、设备缓存，调用者在系统调用里阻塞。为了更接近这一点，可以用 device-mapper 的 `dm-delay`。Linux kernel 文档说明 `dm-delay` 可以延迟 read、write 和 flush，并且 9 参数形式可以单独指定 flush delay。对 `fsync` 慢，flush delay 比简单 write delay 更贴近语义。

```bash
# 测试环境示例：只延迟 flush 500ms，read/write 不额外延迟。
sectors=$(blockdev --getsz /dev/loop10)
dmsetup create logserve-slow-fsync --table "0 $sectors delay /dev/loop10 0 0 /dev/loop10 0 0 /dev/loop10 0 500"
mkfs.ext4 /dev/mapper/logserve-slow-fsync
mount /dev/mapper/logserve-slow-fsync /mnt/logserve-slow-fsync
```

这个方式要在临时块设备上做。不要对开发机根分区或真实数据盘做。实验结束后也要清理：

```bash
umount /mnt/logserve-slow-fsync
dmsetup remove logserve-slow-fsync
losetup -d /dev/loop10
```

Kubernetes 里可以用 Chaos Mesh `IOChaos` 或 `BlockChaos`。`IOChaos` 的 `latency` 会给指定路径下的文件系统操作加延迟，文档示例中可以对目录下注入 100ms 延迟；方法列表里包含 `fsync` 和 `fsyncdir`，所以可以把注入范围尽量收窄到 log 目录和 fsync 类操作。`BlockChaos` 更偏块设备层，可以模拟 block device latency 或 freeze，但官方文档也说明它还处在早期阶段，且 `freeze` 会影响使用该块设备的所有进程，不只影响目标容器。

```yaml
apiVersion: chaos-mesh.org/v1alpha1
kind: IOChaos
metadata:
  name: logserve-fsync-latency
  namespace: chaos-mesh
spec:
  action: latency
  mode: one
  selector:
    labelSelectors:
      app: logserve-logd
  volumePath: /var/lib/logserve
  path: /var/lib/logserve/logd/**/*
  methods:
    - FSYNC
    - FSYNCDIR
  delay: 500ms
  percent: 100
  duration: 60s
```

`fsync` 慢的实验不能只看“最后有没有成功”。它要看时间维度：

| 观测项 | 为什么要看 |
|---|---|
| append p50/p95/p99 | 判断慢 fsync 是否传导到用户请求 |
| durable lag | 判断已经接收但尚未刷盘的窗口扩大到多少 |
| logstore queue depth | 判断是否出现无界排队 |
| control event append latency | 判断控制面是否被 shared log 卡住 |
| worker heartbeat age | 判断存储慢是否间接导致 worker 被误判死亡 |
| redelivery/stale completion | 判断超时重试后是否还能收敛 |
| error budget / timeout rate | 判断调用方看到的是慢、失败还是雪崩 |

对 LogServe，可以这样回答：

```text
我会先在 logstore 的 Sync 边界加确定性延迟，覆盖 CI 里的 timeout、backpressure 和批量 fsync 策略；然后用 dm-delay 或 IOChaos 把延迟下沉到真实文件系统/块设备路径，特别是 flush delay。验证时不只看 append 是否成功，还要看 p99、队列长度、durable watermark、workflow completion timeout、worker lease 和 metadata view 是否仍然遵守 log-first。fsync 慢最容易把系统拖成半死不活的状态，所以它比直接 EIO 更能暴露排队和重试放大问题。
```

这里也要守住边界。LogServe 简历里已有 `always`、`batch`、`interval` fsync benchmark，`always fsync` 吞吐明显低于批量策略。这说明不同刷盘策略的成本已被测到，但不等于已经系统性覆盖了“设备 flush 偶发卡 5 秒”这种慢故障。慢故障需要单独注入和单独报告。

## Q008. 如何注入网络延迟、丢包、乱序和分区？

网络故障注入要区分两种对象：包级劣化和连通性切断。延迟、丢包、乱序、重复、损坏、限速属于包级劣化；网络分区是拓扑级故障，重点是 A 能不能到 B、B 能不能到 A、第三方 C 是否仍然可达。两者不能混着测。

在 Linux 单机或 VM 里，经典工具是 `tc netem`。`tc-netem(8)` 文档说明 netem 用于模拟真实网络属性，支持 delay、loss、corrupt、duplicate、reorder、rate 等选项。例子如下：

```bash
# 固定延迟 100ms
tc qdisc add dev eth0 root netem delay 100ms

# 100ms 基础延迟，20ms 抖动，按正态分布变化
tc qdisc change dev eth0 root netem delay 100ms 20ms distribution normal

# 1% 随机丢包
tc qdisc change dev eth0 root netem loss 1%

# 0.3% 丢包，25% 相关性，用来模拟 burst loss
tc qdisc change dev eth0 root netem loss 0.3% 25%

# 先引入 10ms 延迟，再让 25% 包乱序
tc qdisc change dev eth0 root netem delay 10ms reorder 25% 50%

# 清理
tc qdisc del dev eth0 root
```

有两个细节容易被忽略。

第一，netem 默认作用在发送方向。你在 A 的 `eth0` 上加 delay，影响的是 A 发出去的包，不一定等价于 B 收到的所有包都慢。TCP 场景如果想更真实，常常要在 receiver ingress、ifb 或两端都做配置。`tc-netem` 文档也提醒，TCP 测试结果要真实，netem 放置位置很重要。

第二，乱序通常需要延迟配合。没有延迟，包没有机会被后发先至。文档里的 reorder 示例也是配合 delay 使用。面试时说“用 netem reorder 就行”太粗，要补上这个原因。

网络分区可以用防火墙规则、路由规则、network namespace，也可以用 Chaos Mesh。最直白的是在 A 上阻断到 B 的流量：

```bash
# iptables 形态示例：阻断 A -> B
iptables -A OUTPUT -p tcp -d 10.0.0.20 --dport 9000 -j DROP

# 如果要模拟双向分区，还要在 B 上阻断 B -> A，或者在中间路由处阻断
iptables -A OUTPUT -p tcp -d 10.0.0.10 --dport 9000 -j DROP

# 清理时删除对应规则，实际实验要用注释或独立 chain 避免误删
iptables -D OUTPUT -p tcp -d 10.0.0.20 --dport 9000 -j DROP
```

Kubernetes 里用 Chaos Mesh `NetworkChaos` 更方便。官方文档列出的 action 包括 `delay`、`loss`、`duplicate`、`corrupt`、`partition`、`bandwidth`；partition 示例里可以指定方向 `to`、`from`、`both`。例如让 `logserve-worker` 到 `logserve-control` 出现双向分区：

```yaml
apiVersion: chaos-mesh.org/v1alpha1
kind: NetworkChaos
metadata:
  name: worker-control-partition
spec:
  action: partition
  mode: all
  selector:
    namespaces:
      - default
    labelSelectors:
      app: logserve-worker
  direction: both
  target:
    mode: all
    selector:
      namespaces:
        - default
      labelSelectors:
        app: logserve-control
  duration: 60s
```

延迟、丢包和乱序也可以写成 `NetworkChaos`。实际实验里建议分开跑，先只测 delay，再只测 loss，再只测 reorder。混合故障更真实，但定位会很难：

```yaml
apiVersion: chaos-mesh.org/v1alpha1
kind: NetworkChaos
metadata:
  name: control-logd-delay
spec:
  action: delay
  mode: one
  selector:
    namespaces:
      - default
    labelSelectors:
      app: logserve-control
  direction: to
  target:
    mode: all
    selector:
      namespaces:
        - default
      labelSelectors:
        app: logserve-logd
  delay:
    latency: 100ms
    jitter: 30ms
    correlation: "50"
  duration: 60s
```

可靠的实验顺序通常是：

```text
baseline
只加 delay
只加 loss
只加 reorder
只做单向 partition
再做双向 partition
最后才做组合故障
```

对 LogServe，网络故障要按链路来设计：

| 链路 | 注入故障 | 要验证什么 |
|---|---|---|
| SDK -> control | delay/loss | client deadline、retry、idempotency conflict |
| control -> logd | delay/partition | log-first 是否成立，metadata view 是否不会先写 |
| control -> worker | partition | heartbeat/lease 过期后 redelivery |
| worker -> control | loss/delay | completion 丢失后是否重试，late completion 是否被 fencing |
| worker -> object store | delay/failure | result ref、checkpoint fetch、LLM cache cold path |
| dashboard -> control | delay | 只影响观测面，不应影响运行面 |

最值得测的是非对称分区。比如 control 看不到某个 worker，但 worker 还能继续跑本地任务；或者 worker 能把 completion 发出去，但 control 的响应丢了。这个时候系统不能靠“我觉得对方死了”做结论，必须靠 lease、epoch、idempotency key 和 replay 收敛。

面试回答可以这样说：

```text
我会用 tc netem 或 Chaos Mesh NetworkChaos 注入网络故障。delay、loss、reorder 是包级故障，partition 是连通性故障，实验上要分开。对 LogServe，我不会只做全局网络变慢，而是按链路注入：SDK 到 control、control 到 logd、control 到 worker、worker 到 object store。最重要的断言是：control 写 shared log 失败或超时时不能先改 metadata；worker 被分区后 lease 过期会触发 redelivery；旧 worker 恢复后提交 completion 会被 epoch/fencing 拒绝；重复请求靠 idempotency 收敛。实验记录必须包含注入规则、持续时间、清理动作、p99、timeout rate、redelivery 次数和最终状态一致性。
```

## Q009. 如何注入时钟跳变？

时钟跳变不是简单把系统时间改一下。你要先分清 wall clock 和 monotonic clock。Linux `clock_gettime(2)` 文档里，`CLOCK_REALTIME` 是可设置的系统 wall clock，手工改时间或 NTP 调整都会影响它；`CLOCK_MONOTONIC` 不受系统时间的离散跳变影响。Go 的 `time` 包也写得很清楚：wall clock 用来表示日期时间，monotonic clock 用来测量时间间隔；`time.Now()` 返回的 `Time` 在进程内通常同时带有 wall 和 monotonic reading，`Sub`、`Since` 这类测量会优先使用 monotonic reading。

所以注入时钟跳变要按目标来选方法。

第一种是应用层 clock abstraction。把系统里所有和时间有关的判断收敛到接口：

```go
type Clock interface {
    Now() time.Time
    Since(time.Time) time.Duration
    Sleep(time.Duration)
}
```

测试里用 fake clock：

```go
clk.Advance(30 * time.Second)
control.ScanExpiredLeases()
```

这最适合测 lease、timeout、retry backoff、TTL、idempotency window。它不会测内核时间，但能稳定覆盖业务语义。LogServe 里 worker heartbeat、lease expiry、retry deadline、actor ownership epoch、idempotency TTL 如果散落着直接调用 `time.Now()`，时钟测试会很难做；收敛到 Clock 接口以后，才能精确制造“control 时间前跳 30 秒”“worker 时间后退 10 秒”“retry deadline 已过期”等场景。

第二种是容器/进程级 time chaos。Chaos Mesh `TimeChaos` 可以给指定 Pod 里的进程注入 time offset，文档里还可以指定 `clockIds`，例如 `CLOCK_REALTIME`、`CLOCK_MONOTONIC`。它还有一个很重要的限制：默认影响容器 PID namespace 中的 PID 1 及其子进程，`kubectl exec` 启动的进程不一定受影响。这个细节在排查实验结果时很关键。

```yaml
apiVersion: chaos-mesh.org/v1alpha1
kind: TimeChaos
metadata:
  name: worker-clock-forward
  namespace: chaos-mesh
spec:
  mode: one
  selector:
    labelSelectors:
      app: logserve-worker
  timeOffset: "30s"
  clockIds:
    - CLOCK_REALTIME
  duration: 60s
```

第三种是节点或 VM 级改时间。比如在隔离 VM 里关闭 NTP，再用 `date`、`timedatectl` 或直接走 `clock_settime` 改 `CLOCK_REALTIME`。这类实验覆盖面更大，但副作用也大：TLS 证书校验、日志时间戳、包管理、监控、数据库、对象存储客户端都可能受影响。只能在测试 VM 做，不要在共享开发机和生产节点做。

```bash
timedatectl set-ntp false
date -s "2026-06-19 10:00:00"
date -s "2026-06-19 09:55:00"
timedatectl set-ntp true
```

第四种是只偏移某一方。分布式系统里最危险的不是所有机器一起跳，而是只有一台机器跳。比如只让 worker 快 30 秒，只让 control 慢 20 秒，只让 object store 客户端所在节点跳变。这样才能发现系统有没有错误地相信远端时间戳。

对 LogServe，时钟跳变要重点查这些地方：

- lease 判断应该尽量用 control 接收 heartbeat 的时间，不要信 worker 自己报的 wall clock。
- timeout 和 elapsed time 应该用 monotonic duration，不要用两个序列化后的 wall timestamp 相减。
- 持久化日志里的时间戳可以用于审计和排序辅助，但不能替代 seq、epoch、offset、logical time。
- actor fencing 不能依赖“谁的时间更新”，要依赖 owner epoch 和 command sequence。
- idempotency window 如果基于 wall clock TTL，要能承受时间后退，不要让旧 key 立刻复活或永远不清理。

可以设计几组实验：

```text
control clock forward 60s:
  检查 lease 是否集中误过期，是否触发过量 redelivery。

worker clock forward 60s:
  如果 heartbeat timestamp 来自 worker，control 是否被误导；正确设计应由 control 盖接收时间。

worker clock backward 60s:
  检查 completion timestamp 变旧是否影响事件接受；正确设计应看 epoch/idempotency/seq，而不是看 worker wall time。

logd clock backward:
  检查日志时间戳是否乱序时，恢复仍按 offset/sequence 重建。

all nodes realtime jump:
  检查日志、metrics、dashboard 展示是否混乱；核心执行语义不应依赖 wall clock 排序。
```

面试里可以这样收束：

```text
我会先用 Clock 接口做确定性测试，再用 TimeChaos 或测试 VM 做进程/节点级时钟跳变。关键不是把 date 改掉，而是区分 CLOCK_REALTIME 和 CLOCK_MONOTONIC：duration、deadline、lease elapsed time 应该尽量走 monotonic；持久化和跨进程传输的时间戳会丢失进程内 monotonic 语义，所以不能拿它做唯一正确性依据。对 LogServe，我会验证 worker 时钟偏移不会骗过 control 的 lease 判断，control 时钟前跳不会破坏 epoch/fencing，日志恢复按 offset/seq/replay，而不是按 wall timestamp。
```

这也是一个边界问题。单机实验里，如果所有组件在同一台机器上共享同一个系统时钟，时钟偏移的风险会被低估。真正的多节点部署里，节点间 clock skew 才是常态。因此当前 LogServe 可以说有机制上的 epoch、seq、log offset 基础，但还不能说已经完整验证了多节点 clock skew。

## Q010. 如何注入 GC pause 或 stop-the-world？

先把概念拆开。GC pause 是运行时垃圾回收导致的暂停；stop-the-world 是所有应用线程都不能推进的一种现象。真实 GC 可能造成 STW，但 STW 不一定来自 GC，也可能来自 `SIGSTOP`、虚拟机挂起、cgroup freezer、宿主机调度停顿、CPU 被抢占、容器 checkpoint、内核长时间不可调度。测试时不要把它们混成一个故障。

如果是 Go 程序，最小注入点是 `runtime.GC()`。Go 官方 `runtime.GC` 文档说明它会运行一次 garbage collection，并阻塞调用者直到完成，也可能阻塞整个程序。可以配合大量分配、低 `GOGC`、较小 `GOMEMLIMIT` 或 `debug.SetGCPercent`、`debug.SetMemoryLimit` 来增加 GC 压力：

```go
debug.SetGCPercent(10)
debug.SetMemoryLimit(256 << 20)

var sink [][]byte
for i := 0; i < 20000; i++ {
    sink = append(sink, make([]byte, 64<<10))
}
runtime.GC()
```

但要说清楚：Go 的 GC 不是传统意义上完全 stop-the-world 的 GC。Go GC guide 里明确说 Go GC 大部分工作和应用并发执行，主要是为了降低延迟；仍然有短暂 STW 阶段，比如 mark/sweep 转换、root scanning 等。也就是说，`runtime.GC()` 能制造 GC 压力和一些暂停，但不能保证“暂停全进程 10 秒”。如果你需要稳定的长暂停，用它不合适。

稳定模拟 STW 的办法是暂停进程。Linux `signal(7)` 里 `SIGSTOP` 的默认动作是 stop process，并且 `SIGKILL` 和 `SIGSTOP` 不能被 catch、block 或 ignore。实验上可以这样做：

```bash
pid=$(pidof logserve-worker)
kill -STOP "$pid"
sleep 10
kill -CONT "$pid"
```

这不是 GC，但它很好地模拟了“这个进程 10 秒内完全不推进”。对分布式系统来说，这个故障非常关键：进程没死，TCP 连接未必立刻断，内存状态还在，恢复后它会继续执行旧代码路径。很多 fencing bug 都是在这种场景下暴露的。

更精确的办法是在代码里加测试 barrier。比如 worker 在几个危险点停住：

```text
拿到 task 之后、写 STARTED 之前停住
写 STARTED 之后、真正执行 task 前停住
执行完成后、提交 completion 前停住
提交 completion 后、等待 ack 前停住
actor command 执行中、持有 actor ownership 时停住
```

然后测试控制面是否按 lease 超时重派，旧 worker 恢复后是否因为 epoch 过期被拒绝。这比随机制造 GC pause 更能打到正确性问题。

如果是 JVM 服务，可以用更专门的工具。Chaos Mesh 的 `JVMChaos` 官方文档列出 `gc`、`latency`、`exception`、`return`、`stress` 等 action，其中 `gc` 用来触发 garbage collection。但 LogServe 主体是 Go 和 Python，不能把 JVMChaos 当成 Go runtime 的方案。面试时可以顺手提一句“JVM 有 JVMChaos 或 jcmd 这类路径，Go 要用 Go runtime、进程暂停或测试 hook”，这样边界比较清楚。

对 LogServe，GC pause/STW 注入最有价值的场景有四个。

第一，暂停 worker 超过 lease TTL：

```text
worker poll 到 task
worker 进入 STOP 10s
control 发现 heartbeat/lease 超时
control redeliver task 给另一个 worker
旧 worker CONT 后提交 completion
期望：旧 completion 被 epoch/fencing/idempotency 拦住，最终只有一个结果生效
```

第二，暂停 control：

```text
control STOP 10s
workers 继续执行或 poll 超时
control CONT 后从 shared log 恢复/继续维护 metadata view
期望：control 不因为本地内存状态滞后而覆盖日志事实
```

第三，暂停 logd：

```text
logd STOP 10s
control append event 卡住或 deadline exceeded
workers completion 无法落 log
logd CONT 后恢复
期望：没有先写 metadata 后补 log 的路径；客户端看到明确 timeout 或 backpressure
```

第四，暂停 actor owner：

```text
worker A 持有 actor ownership
worker A STOP 超过 lease
ownership 转移到 worker B
worker A CONT 后继续提交旧 command result
期望：owner epoch 拦住旧写入，command_seq 不倒退，snapshot/replay 后状态一致
```

实验指标不要只看日志里有没有 “worker resumed”。至少要记录：

| 指标 | 用途 |
|---|---|
| heartbeat age | 判断暂停是否被控制面观测到 |
| lease expiration count | 判断是否触发预期 redelivery |
| stale completion rejected | 判断 fencing 是否真正生效 |
| duplicate execution count | 判断任务是否重复执行，以及结果是否被幂等收敛 |
| actor command_seq | 判断 actor 状态是否被旧 owner 污染 |
| append latency / timeout rate | 判断 logd 或 control pause 的外部影响 |
| Go GC pause / gctrace / runtime metrics | 区分真实 GC 压力和人为 STOP |

一个比较稳的面试回答是：

```text
GC pause 和 stop-the-world 要分开测。Go 里可以用 runtime.GC、低 GOGC、GOMEMLIMIT 和大量分配制造 GC 压力，但 Go GC 大部分并发执行，不能保证任意长度的全进程暂停。要稳定模拟 STW，我会用 SIGSTOP/SIGCONT 或测试 barrier，让 worker/control/logd 在关键点停住，再验证 lease、redelivery、epoch fencing、idempotency 和 replay。对 LogServe，最关键的不是进程恢复后还能不能跑，而是旧 worker 恢复后提交的 completion 会不会污染新 owner 或新 attempt 的状态。
```

最后补一个实践判断：如果目标是性能分析，用真实 GC 压力、`gctrace`、runtime metrics、pprof、trace；如果目标是可靠性正确性，用 `SIGSTOP` 和测试 barrier。前者解释 latency spike，后者验证系统能不能从“活着但暂停”的组件中恢复。
## Q011. 如何测试重启恢复是否依赖内存状态？

重启恢复测试最核心的问题是：把进程内存清空以后，系统还能不能只靠持久化状态恢复到正确结果。这里的“内存状态”包括 map、queue、timer、lease cache、in-flight task、actor mailbox、materialized metadata view、worker registry、模型缓存统计，以及所有没有落到 shared log 或外部存储里的临时变量。

对 LogServe 来说，正确的设计口径很明确：shared log 是状态源，metadata view 是可重建视图。`docs/report.md` 里也写了控制面先写事件日志，再更新 materialized metadata view；workflow、actor、LLM 状态都可以从日志 replay 重建。所以测试不能只看“进程重启后服务起来了”，要专门证明它没有偷偷依赖旧内存。

最有效的办法是做 cold restart，而不是 warm restart。

```text
1. 启动 logd、control、workers。
2. 提交一批 task/workflow/actor/LLM 请求。
3. 等系统进入几个中间状态：queued、running、completed、actor command in-flight、LLM cache updated。
4. 强制 kill control 和 worker，不允许优雅 shutdown。
5. 删除或隔离所有进程内派生状态：metadata view cache、临时 in-memory queue、worker runtime state。
6. 保留 shared log、result store、checkpoint cache 等本来就应该持久化的状态。
7. 重启 control。
8. control 必须从 shared log replay，而不是从旧内存恢复。
9. 对比恢复后的任务、workflow、actor、LLM view 和重启前已经持久化的事件。
```

测试里有个小技巧：重启前故意打乱内存状态。比如在测试版本里给 metadata view 加一个 debug endpoint，把某个 task 的内存状态改成错误值；然后重启 control。如果恢复后仍然带着错误值，说明它依赖了内存快照或临时文件；如果恢复后回到 shared log 推导出的值，说明 replay 真的生效。

更干净的做法是起一个完全新的 control 进程，使用同一个 log directory，但不给它任何上一个进程的内存对象。测试伪代码可以这样写：

```go
func TestControlRecoveryDoesNotDependOnMemory(t *testing.T) {
    dir := t.TempDir()
    logd := startLogd(t, dir)

    c1 := startControl(t, logd.Addr())
    submitWorkflow(t, c1, "wf-1")
    waitForEvent(t, logd, "WorkflowStepStarted")

    killProcess(t, c1)

    c2 := startControl(t, logd.Addr())
    got := c2.GetWorkflow("wf-1")

    want := replayWorkflowFromLog(t, logd, "wf-1")
    assertEqual(t, want, got)
}
```

这里 `want` 不能来自重启前的内存对象。它应该来自独立 replay，或者来自测试预先记录的外部事实。否则测试会变成“用同一个错误实现验证自己”。

还可以做差分测试：同一组日志，启动两个 control，一个正常 replay，一个只读 replay 工具。两边输出同一份规范化快照：

```json
{
  "tasks": {
    "task-1": {
      "state": "SUCCEEDED",
      "attempt": 2,
      "result_ref": "result://..."
    }
  },
  "workflows": {
    "wf-1": {
      "state": "SUCCEEDED",
      "completed_steps": ["a", "b", "c"],
      "failed_steps": []
    }
  },
  "actors": {
    "actor-1": {
      "owner_epoch": 4,
      "command_count": 21,
      "snapshot_seq": 20
    }
  }
}
```

然后比较规范化 JSON。不要比较日志文本、map 遍历顺序、时间戳字符串这类不稳定输出。

为了抓出“隐性依赖内存”，我会加几类断言：

| 断言 | 说明 |
|---|---|
| 重启后 running task 不应永远 running | 如果 worker 已消失，control 应靠 lease/timeout/redelivery 收敛 |
| 已完成 step 不应重复提交不同结果 | `workflow_id + step_id + input_hash` 要稳定去重 |
| actor command_seq 不应倒退 | replay 后 actor 状态必须符合单调 command sequence |
| metadata view 可删除重建 | 删除派生 view 后，从 log replay 结果应一致 |
| worker registry 不能当真相 | worker 心跳是活性信息，不是持久事实 |
| result_ref 必须可读 | 日志里指向的 result/snapshot object 必须存在且校验通过 |

LogServe 里尤其要测 control crash。control 是最容易“顺手把状态存在内存 map 里”的地方。比如 task queue、workflow ready set、actor mailbox、LLM model stats 都可能被写成内存优先。正确的测试会在 control 重启后检查这些 view 是否由事件流重建，而不是只检查 gRPC 端口重新监听。

面试可以这样答：

```text
我会用 cold restart 测恢复是否依赖内存状态。具体做法是保留 shared log、result store、checkpoint cache，杀掉 control/worker，丢弃所有内存 view，再启动一个全新的 control，从 shared log replay 出 metadata view。验证时不用旧进程的对象当 expected，而是用独立 replay 工具或规范化快照对比 task、workflow、actor、LLM 状态。对 LogServe，关键断言是：metadata view 可删除重建，已完成 step 不重复提交，actor command_seq 和 owner epoch 不倒退，running task 能通过 lease/redelivery 收敛。这样才能说明恢复依赖的是持久化日志，不是上一个进程残留的内存。
```

## Q012. 如何测试任务执行到一半 worker crash？

任务执行到一半 worker crash，是恢复测试里最有价值的场景之一。因为它处在最尴尬的位置：control 可能已经把 task 标成 running，worker 可能已经执行了外部副作用，也可能还没执行；completion 可能还没发，也可能发出去了但 control 没收到。只 kill 空闲 worker 没有太大意义，真正要测的是这些中间点。

测试前先把任务生命周期拆开：

```text
TaskSubmitted
TaskScheduled
TaskStarted
worker actually starts user function
user function writes side effect / result object
worker sends TaskCompleted
control appends TaskCompleted
metadata view marks task SUCCEEDED
```

然后在每个边界插入 crash。最好不要只用随机 kill。随机 kill 会覆盖一些路径，但复现困难。更好的方式是测试 hook 或 barrier：

```go
type CrashPoint string

const (
    CrashAfterPoll           CrashPoint = "after_poll"
    CrashAfterStartedEvent   CrashPoint = "after_started_event"
    CrashDuringUserFunction  CrashPoint = "during_user_function"
    CrashAfterResultWrite    CrashPoint = "after_result_write"
    CrashBeforeCompletion    CrashPoint = "before_completion"
    CrashAfterCompletionSend CrashPoint = "after_completion_send"
)
```

worker 到达 crash point 后通知测试 harness，测试 harness 再 `SIGKILL` worker。这样每次都能打到同一个位置。

一个典型流程：

```text
1. 启动 control、logd、worker A、worker B。
2. 提交一个带 idempotency key 的 task。
3. worker A poll 到 task，写入 STARTED。
4. 在用户函数执行到一半时阻塞。
5. 测试 harness kill -9 worker A。
6. control 通过 heartbeat/lease timeout 发现 attempt 失效。
7. control redeliver task 给 worker B。
8. worker B 执行并提交 completion。
9. 如果 worker A 恢复后还有迟到 completion，control 必须拒绝旧 attempt。
10. 最终 task 只有一个成功结果，workflow 不重复推进。
```

这个测试要区分“执行至少一次”和“结果只提交一次”。LogServe 的语义是 exactly-once-ish，不声明严格 distributed exactly-once。也就是说，worker crash 后任务函数可能被重新执行；系统要保证的是最终提交和状态推进幂等。对纯函数任务，这很好处理；对有外部副作用的任务，SDK 层必须要求用户提供幂等 key，或者把副作用挪到可去重的外部系统。

测试数据要专门设计。不要用简单 `return 1+1`，它看不出重复执行。可以让任务写一个外部 side-effect log：

```python
@task(idempotency_key="wf-1:step-a:input-hash")
def half_crash_task(x):
    append_side_effect("task started")
    wait_for_test_barrier("during_user_function")
    append_side_effect("task finished")
    return x + 1
```

然后断言：

```text
side_effect 可能出现两次 started；
最终 TaskCompleted 只能有一个被接受；
workflow step result 只能有一个 canonical result_ref；
迟到 attempt 的 completion 必须 rejected 或 ignored；
dashboard 不能显示两个成功 task；
replay 后状态和在线状态一致。
```

如果任务执行到一半时已经写了 result object，但 completion 没写入 shared log，这个 result object 应被视为 orphan。恢复逻辑不能扫描 object store 后自动把它当成成功。source of truth 仍然是 shared log。可以有后台清理 orphan object，但不能让它改变任务状态。

对 actor task 还要多一层 fencing。比如 worker A 持有 actor owner epoch 3，执行 command 到一半 crash；control 转移 ownership 给 worker B，epoch 变成 4。worker A 恢复后如果提交 epoch 3 的 `ActorCommandApplied`，必须被拒绝。否则 actor 状态会被旧 owner 污染。

面试回答可以这样收束：

```text
我会在 worker 生命周期的关键边界打 crash point：poll 后、STARTED 后、用户函数中间、result object 写完后、completion 前、completion 发出但 ack 前。测试用 kill -9，不走优雅退出。恢复后看 lease 是否过期、任务是否 redeliver、旧 attempt completion 是否被 fencing/idempotency 拒绝、workflow 是否只推进一次。对 LogServe，允许 worker 至少执行一次，但 shared log 中最终只能接受一个 canonical completion；result object 没有对应 TaskCompleted 事件时只能算 orphan，不能反推任务成功。
```

## Q013. 如何测试控制面 crash 后状态重建？

control crash 测的是“控制面是不是只是一个可重建的 materialized view”。如果 control crash 后必须依赖内存里的 queue、workflow DAG 状态、actor mailbox、worker map 才能继续，那 shared log 其实没有承担 source of truth 的角色。

测试要覆盖两类 crash：启动恢复和运行中恢复。

启动恢复比较简单：准备一段已有日志，启动 control，看它能不能重建 view。运行中恢复更重要：control 正在处理请求时 crash，重启后必须从日志里推导出已经发生的事实，并把未完成的工作重新调度。

可以构造一组日志：

```text
TaskSubmitted(task-1)
TaskScheduled(task-1, worker-A, attempt=1)
TaskStarted(task-1, worker-A, attempt=1)

WorkflowSubmitted(wf-1)
WorkflowStepScheduled(wf-1, step=a)
WorkflowStepCompleted(wf-1, step=a)
WorkflowStepScheduled(wf-1, step=b)

ActorCreated(actor-1)
ActorOwnershipGranted(actor-1, worker-A, epoch=1)
ActorCommandSubmitted(actor-1, seq=1)
ActorCommandApplied(actor-1, seq=1)
ActorSnapshotCreated(actor-1, snapshot_seq=1)
```

control 重启后，应该得到：

```text
task-1: running 或 expired 后待 redelivery，取决于 lease 时间
wf-1.step-a: completed，不重跑
wf-1.step-b: scheduled/runnable，需要继续执行
actor-1: 从 snapshot_seq=1 恢复，command_count=1
metadata view: 从 log 重建，不依赖旧 map
```

这类测试最好做成 golden replay。把输入日志和期望 view 写成 fixture：

```text
fixtures/recovery/control_crash/input.log
fixtures/recovery/control_crash/expected_view.json
```

测试启动 control 后导出 `/debug/snapshot` 或 dashboard snapshot，规范化后与 `expected_view.json` 对比。规范化要去掉当前时间、进程 id、map 顺序、短暂队列长度等非确定性字段。

还要测 crash timing。control 的危险点通常在“写日志”和“改内存 view”之间：

```text
case A: append TaskCompleted 前 crash
  恢复后不能把 task 当成功。

case B: append TaskCompleted 成功后、更新 metadata view 前 crash
  恢复后必须从 TaskCompleted 重建成功状态。

case C: 更新 view 后、返回客户端前 crash
  客户端可能重试；重试要走 idempotency，不能产生两个完成事件。

case D: workflow step completed 后、调度下游 step 前 crash
  恢复后要发现下游 step ready，并继续调度。

case E: actor ownership granted 后、内存 owner map 更新前 crash
  恢复后 owner epoch 必须来自 log。
```

这些 case 能直接验证 log-first 语义。如果实现里存在“先改内存，再异步写 log”，case A 会暴露出来：重启后这个状态丢失，或者更糟，在线路径已经对外返回成功但日志里没有事实。

LogServe 的 control crash 测试还应该和 worker 行为一起测。control 停掉时 worker 可能继续执行，completion 可能失败或卡住。control 恢复后，要处理这些情况：

- worker 重试提交 completion。
- worker 已经超过 lease，任务被 redeliver。
- 旧 worker 和新 worker 都提交 completion。
- workflow 下游 step 被重复调度请求触发。
- actor owner 旧 epoch 的 command result 迟到。

面试可以这样答：

```text
我会把 control 当成 materialized view 来测。先准备 shared log，再启动一个全新的 control，导出规范化 view，和独立 replay 的 expected view 对比。更重要的是在运行中插 crash point：append event 前、append 成功后但更新 view 前、更新 view 后但返回客户端前、workflow step 完成后但调度下游前。恢复后，TaskCompleted 已落 log 就必须可见，没落 log 就不能假装成功；workflow 已完成 step 不重跑，ready step 能继续调度；actor owner epoch 和 command_seq 从 actor stream 恢复。这个测试能直接证明 log-first 是否真实存在。
```

## Q014. 如何测试日志尾部 partial write？

日志尾部 partial write 是 append-only log 必测项。Linux `write(2)` 文档明确说成功返回也可能少写一部分；直接 I/O 返回错误时，目标 offset 的部分数据还可能处于不一致状态。`fsync(2)` 文档也强调只有同步完成后，修改过的 in-core data 才能认为到达设备；目录项还需要显式 fsync 目录。对 logstore 来说，这些细节不是理论问题，直接决定 crash recovery 是否会读出半条 event。

测试目标很简单：日志尾部损坏时，恢复逻辑只能读取最后一条完整、CRC 正确、长度正确、commit 状态明确的 record。尾部半条 record 必须截断、忽略或报出可恢复错误，不能被当成事件。

建议把 record 格式写清楚：

```text
magic
version
record_length
stream_id
sequence
payload
crc32
```

然后对每个字段做截断：

```text
只写 magic 的一半
写完 header，payload 没写完
payload 写完，CRC 没写完
CRC 写完但值错误
record_length 声称 1MB，实际只有 300B
两条完整 record 后接随机垃圾
index 指向 partial record 的 offset
```

测试可以直接生成文件，不一定要真的 crash 一百次。比如：

```go
func TestRecoverIgnoresPartialTail(t *testing.T) {
    dir := t.TempDir()
    seg := filepath.Join(dir, "000001.seg")

    r1 := encodeRecord("stream-a", 1, []byte("ok-1"))
    r2 := encodeRecord("stream-a", 2, []byte("ok-2"))
    r3 := encodeRecord("stream-a", 3, []byte("will-be-cut"))

    data := append(append(r1, r2...), r3[:len(r3)/2]...)
    os.WriteFile(seg, data, 0o644)

    recovered := RecoverSegment(seg)
    assertRecords(t, recovered, []Record{rec1, rec2})
}
```

还要测 index rebuild。很多 logstore 会有 segment file 和 index file。crash 时可能出现几种组合：

| segment | index | 恢复策略 |
|---|---|---|
| segment 完整，index 缺失 | 扫 segment 重建 index |
| segment 完整，index 少一条 | 以 segment 为准补 index |
| segment partial，index 指到 partial offset | index 不能越过 valid tail |
| segment CRC 错，index 正常 | CRC 错的 record 不能读 |
| segment 被截断，index 仍旧 | index 必须降级重建 |

如果实现里有 durable watermark，还要验证 watermark 不会越过最后一次成功 `fsync` 的位置。否则可能出现“append 返回成功但实际没有 durable”的语义混乱。LogServe 的 benchmark 里有 `always`、`batch`、`interval` fsync 策略；不同策略下的 durable 边界不同，partial tail 测试要把策略写进 expected。

更接近真实故障的做法是 crash harness：

```text
1. 启动 logd，持续 append 带单调 seq 和 CRC 的 record。
2. 客户端只记录已经收到成功响应的 record id。
3. 在随机时间 kill -9 logd 或整机断电。
4. 重启 logd，recover。
5. 检查 recovered records 是某个合法前缀，不能有 CRC 错误、半条 record、seq 跳跃、重复 offset。
6. 对已确认成功的 record，根据 fsync policy 判断是否必须存在。
```

“合法前缀”很重要。append-only log 的恢复不一定保证所有发起过的写都存在，但它必须保证读出来的是完整前缀或符合 commit marker 的集合。不能读出未来的一半，也不能把损坏 payload 解释成另一个合法事件。

面试回答：

```text
我会直接构造 segment 尾部 partial write：截断 header、payload、CRC，制造错误 length、错误 CRC、segment/index 不一致。恢复逻辑只能接受最后一条完整且校验通过的 record，后面的 partial tail 要截断或忽略。然后再做 crash harness：持续 append、随机 kill、重启 recover，检查 recovered log 是合法前缀，seq 单调，CRC 正确，index 不越过 valid tail。对 LogServe，这个测试保护的是 shared log 的根基；如果半条 TaskCompleted 被读出来，workflow 和 actor replay 都会被污染。
```

## Q015. 如何测试重复完成、迟到完成和乱序完成？

这三个场景要放在一起测，因为它们都在挑战同一个问题：control 能不能只接受当前 attempt、当前 epoch、当前 command sequence 下的合法 completion。

先定义三类 completion：

```text
重复完成：
  同一个 worker、同一个 attempt、同一个 task，多次提交相同 completion。

迟到完成：
  旧 worker 或旧 attempt 超时后，任务已经 redeliver 给新 worker；旧 completion 后到。

乱序完成：
  completion 到达顺序和调度顺序、workflow step 依赖顺序、actor command_seq 顺序不一致。
```

普通任务和 workflow step 的判断依据通常是 task id、attempt id、lease token、idempotency key、input hash。actor 的判断依据还要加 owner epoch 和 command_seq。LogServe 文档里已经有两个关键基础：workflow step 用 `workflow_id + step_id + input_hash` 做 exactly-once-ish 结果去重；actor 使用 `owner_worker_id + epoch` 做 fencing，并用单调 `command_seq` 保证 mailbox 串行。

测试重复完成：

```text
1. 提交 task-1。
2. worker A attempt=1 执行成功。
3. 发送 TaskCompleted(task-1, attempt=1, result=R1)。
4. 再发送一次完全相同的 TaskCompleted。
5. 期望：第一次 accepted，第二次 idempotent accepted 或 ignored，但不能产生第二个状态推进。
```

如果第二次携带不同 result：

```text
TaskCompleted(task-1, attempt=1, result=R2)
```

这就不是无害重复。系统应该返回 conflict 或 reject。否则同一个 attempt 可以覆盖结果，replay 也会不确定。

测试迟到完成：

```text
1. worker A 拿到 task-1 attempt=1。
2. 暂停 worker A，直到 lease 过期。
3. control redeliver task-1 给 worker B attempt=2。
4. worker B 提交 TaskCompleted(task-1, attempt=2, result=R2)，accepted。
5. 恢复 worker A，让它提交 TaskCompleted(task-1, attempt=1, result=R1)。
6. 期望：attempt=1 completion rejected/ignored，最终 result 仍然是 R2。
```

这个场景比简单重复更关键。它证明 timeout/redelivery 不会让旧 worker 污染新 attempt。

测试乱序完成要分 workflow 和 actor。

workflow 的例子：

```text
wf:
  step A -> step B -> step C

故障注入：
  先伪造或延迟送达 step B completion，再送达 step A completion。

期望：
  如果 B 尚未合法 STARTED，B completion 必须被拒绝；
  A completion 到达后，control 才能调度 B；
  已完成 A 重复到达时不再重复调度 B。
```

actor 的例子：

```text
ActorCommandSubmitted(seq=1)
ActorCommandSubmitted(seq=2)
ActorCommandSubmitted(seq=3)

故障注入：
  先到 ActorCommandApplied(seq=3)，再到 seq=2，再到 seq=1。

期望：
  seq=3 和 seq=2 在 command_count=0 时不能应用；
  seq=1 应用后，seq=2 才能应用；
  seq=3 最后应用；
  replay 后 command_count=3，状态和顺序执行一致。
```

如果 actor owner epoch 发生变化：

```text
worker A epoch=1 的 seq=2 result 迟到；
worker B epoch=2 已经接管 actor；
```

旧 epoch 的 completion 必须被拒绝。这里不能只看 command_seq，因为旧 owner 可能拿着一个看似正确的 seq。

自动化测试可以做成矩阵：

| 场景 | 注入方法 | 期望 |
|---|---|---|
| duplicate same result | replay 同一 completion | 幂等，不重复推进 |
| duplicate different result | 同 attempt 不同 result | conflict/reject |
| stale attempt | lease 过期后旧 attempt 完成 | reject/ignore |
| workflow out-of-order | 下游 completion 先到 | reject，等待依赖满足 |
| actor out-of-order seq | seq=3 先到 | 不应用，保持 command_count |
| stale actor epoch | 旧 owner completion | fencing reject |

最后一定要跑 replay。在线路径可能刚好忽略了重复事件，但如果 log 里写入了两个互相冲突的 completion，重启后 replay 可能选错。最稳的规则是：非法 completion 不进 log；如果为了审计要进 log，也必须有 `Rejected` 类型，不能被 replay 当成状态事实。

面试回答：

```text
我会把 completion 测成一个状态机合法性问题。重复完成要验证幂等，相同 result 不重复推进，不同 result 返回 conflict；迟到完成要让旧 worker 在 lease 过期后提交 completion，验证 attempt/lease token 被拒绝；乱序完成要分别测 workflow dependency 和 actor command_seq。对 LogServe，workflow 用 workflow_id + step_id + input_hash 收敛，actor 用 owner epoch + command_seq fencing。最终还要重启 replay，确保在线状态和日志重放状态一致，不能只在内存里忽略重复。
```

## Q016. 如何判断恢复后的状态是否正确？

恢复后的状态正确，不是“服务能启动”或“接口返回 200”。正确性要有 oracle。没有 oracle 的恢复测试只能证明没有 panic，不能证明状态对。

对 LogServe，最直接的 oracle 是 shared log replay。因为项目设计里 shared log 是事实来源，metadata view 是派生状态。判断恢复正确，应至少比较三份东西：

```text
online view:
  故障发生前后 control 对外暴露的 metadata/dashboard 状态。

replayed view:
  重启后 control 从 shared log 重建出的状态。

independent checker:
  测试工具独立读取 log，按更小、更保守的规则计算 expected state。
```

如果 online view 和 replayed view 不一致，说明重启恢复有问题。如果 replayed view 和 independent checker 不一致，说明 replay 规则或日志内容有问题。如果三者一致，还要继续看外部 artifact：result object、actor snapshot、checkpoint cache、dashboard snapshot 是否和日志引用一致。

可以定义一份规范化状态快照：

```json
{
  "tasks": {
    "task-1": {
      "state": "SUCCEEDED",
      "attempt": 2,
      "accepted_completion": "worker-B:attempt-2",
      "result_ref": "result://task-1"
    }
  },
  "workflows": {
    "wf-1": {
      "state": "SUCCEEDED",
      "completed_steps": ["extract", "infer", "store"],
      "ready_steps": [],
      "failed_steps": []
    }
  },
  "actors": {
    "actor-1": {
      "owner_epoch": 4,
      "command_count": 21,
      "snapshot_seq": 20,
      "tail_commands": [21]
    }
  }
}
```

比较时要去掉不稳定字段：

```text
wall clock timestamp
process pid
map iteration order
temporary queue ordering
last heartbeat age
debug log line number
```

保留决定语义的字段：

```text
task state
attempt number
lease/epoch
accepted completion id
workflow step state
dependency closure
actor command_seq
snapshot_seq
result_ref
log offset / stream sequence
```

恢复正确性还要做端到端断言。比如 workflow 不能只看每个 step 的状态，还要看 DAG 约束：

```text
一个 step 只有在所有 parent completed 后才能 completed。
同一 step 不能有两个不同 result_ref。
workflow SUCCEEDED 表示所有 required steps 都 completed。
workflow FAILED 表示失败策略允许它失败，而不是某个中间状态丢失。
```

task 侧：

```text
SUCCEEDED task 必须有 exactly one accepted completion。
RUNNING task 的 attempt 必须有未过期 lease，否则应转为 retryable。
FAILED task 必须有明确失败事件或 retry exhausted 证据。
重复 completion 不改变 canonical result。
```

actor 侧：

```text
command_seq 从 1 到 N 无洞或按协议明确跳过。
ActorCommandApplied 只能来自当前 owner epoch。
snapshot_seq <= command_count。
从 snapshot + tail replay 的状态等于 full replay。
旧 epoch 的 command result 不进入 actor state。
```

logstore 侧：

```text
record CRC 正确。
stream sequence 单调。
segment tail 没有 partial record 被读取。
index 不指向 invalid tail。
logical trim 不删除恢复所需的 snapshot 之后 tail log。
```

还有一个实践上很有用的指标：recovery idempotence。恢复跑一次和跑两次结果应该一样。

```text
start from log -> recover view A
shutdown -> start from same log -> recover view B
assert A == B
```

如果第二次恢复结果不同，通常说明恢复过程本身写入了不该写的事件，或者 replay 不纯。

面试回答：

```text
我会用规范化状态快照和独立 replay checker 判断恢复是否正确。恢复后不是看服务能不能启动，而是比较 task、workflow、actor、LLM/materialized view 是否和 shared log 推导出的 expected state 一致。对 LogServe，重点字段是 task attempt、accepted completion、workflow step dependency、actor owner epoch、command_seq、snapshot_seq、result_ref 和 log offset。还要检查外部 artifact 可读、partial tail 没被读、恢复过程幂等。能通过这些断言，才说明恢复后的状态正确。
```

## Q017. recovery invariant 应该如何定义？

recovery invariant 是故障前、故障中、故障后都必须成立的约束。它不是测试步骤，也不是监控指标，而是系统承诺。没有 recovery invariant，恢复测试很容易变成“看起来差不多恢复了”。

定义 invariant 时，我会分四层：日志层、任务/workflow 层、actor 层、外部 artifact 层。

日志层 invariant：

```text
L1. recovered log 只能包含完整且校验通过的 record。
L2. 每个 stream 内 sequence 单调，不出现重复或倒退。
L3. segment tail 的 partial record 不参与 replay。
L4. index 是 log 的派生结构，不能让 replay 读到 log 中不存在或无效的 record。
L5. trim 不能删除任何恢复 snapshot 之后仍需要的 tail log。
```

任务层 invariant：

```text
T1. 每个 task 最多有一个 canonical accepted completion。
T2. 相同 idempotency key + 相同 payload 的重复提交收敛到同一结果。
T3. 相同 idempotency key + 不同 payload 必须 conflict。
T4. 过期 attempt 的 completion 不能覆盖当前 attempt。
T5. RUNNING task 如果 lease 已过期，必须最终 redeliver、fail 或进入明确可恢复状态，不能永久悬挂。
```

workflow 层 invariant：

```text
W1. step completed 前，所有必需 parent step 必须 completed。
W2. 同一 step 的 canonical result 由 workflow_id + step_id + input_hash 唯一确定。
W3. 已完成 step 在 recovery/redelivery 后不重复产生不同结果。
W4. workflow 的终态必须能由所有 step 状态推导出来。
W5. replay 后 ready set 等于 DAG 依赖规则计算出的 ready set。
```

actor 层 invariant：

```text
A1. 每个 actor command_seq 单调应用。
A2. ActorCommandApplied 只能来自当前 owner epoch。
A3. 旧 owner 或旧 epoch 的完成不能改变 actor state。
A4. snapshot_seq 不大于 command_count。
A5. snapshot + tail replay 等价于 full replay。
A6. mailbox 串行化不能因为 crash/restart 破坏顺序。
```

外部 artifact 层 invariant：

```text
R1. 日志中引用的 result_ref 必须可读，内容校验通过。
R2. 没有 TaskCompleted 引用的 orphan result object 不能反推任务成功。
R3. actor snapshot object 和 ActorSnapshotCreated 事件一致。
R4. checkpoint cache 命中只能影响性能，不能改变语义结果。
```

生产恢复还要加活性 invariant。安全性说的是“不产生坏状态”，活性说的是“最终能走出去”。比如：

```text
S1. 不接受旧 completion。
S2. 不读 partial log。
S3. 不重复应用 actor command。

P1. 过期 running task 最终会被重新调度或明确失败。
P2. control 重启后最终能导出 dashboard/materialized view。
P3. workflow 在依赖满足后最终继续调度 ready step。
```

安全性通常比活性优先。如果不知道一个 completion 是否来自当前 attempt，宁可拒绝或重试，也不要把它当成功。否则恢复看似更快，但状态可能已经错了。

invariant 要能落成测试。比如：

```go
func CheckRecoveryInvariants(view View, log []Record, artifacts ArtifactStore) error {
    checkLogRecords(log)
    checkTaskCanonicalCompletion(view.Tasks)
    checkWorkflowDependencies(view.Workflows)
    checkActorEpochAndSequence(view.Actors)
    checkResultRefs(view.Tasks, artifacts)
    return nil
}
```

面试回答可以这样说：

```text
我会把 recovery invariant 写成故障前后都必须成立的系统约束，而不是写成“重启成功”。LogServe 的核心 invariant 包括：shared log 只 replay 完整 CRC 正确的记录；metadata view 可由 log 重建；每个 task 最多一个 canonical completion；过期 attempt 不能覆盖当前 attempt；workflow step 不能越过依赖完成；actor command_seq 单调，旧 owner epoch 不能写状态；snapshot + tail replay 等价于 full replay；result_ref 必须可读。再补活性约束：过期 running task 最终 redeliver，control 重启后最终重建 view。测试就是围绕这些 invariant 打 crash point。
```

## Q018. 故障注入是否应该在生产环境运行？

答案不是简单的“应该”或“不应该”。故障注入分层级：开发环境、CI、预生产、影子流量、小流量生产、全生产。绝大多数破坏性 fault injection 应该先在非生产环境完成；生产 chaos test 只适合在系统、团队和保护措施都成熟以后，以小 blast radius、可停止、可回滚的方式运行。

Principles of Chaos Engineering 确实强调生产实验，因为真实流量和真实依赖在非生产环境很难完全复现。它的理由是：系统在不同环境和流量模式下行为不同，直接采样生产流量最能反映真实请求路径。但同一份原则也强调 minimize blast radius。也就是说，生产 chaos 不是“随便在生产搞破坏”，而是在可控实验里观察 steady state 是否被破坏。

AWS FIS 的文档说得更工程化：FIS 会对真实 AWS 资源执行真实动作，所以 AWS 建议先规划，并在预生产环境运行实验；生产运行要依赖 stop conditions、targets、actions、IAM role 等 guardrails。这个表述很适合面试：生产故障注入可以做，但必须把它当变更管理和风险操作，而不是普通测试。

我会这样划分：

| 环境 | 可以做什么 | 不该做什么 |
|---|---|---|
| 单元测试/CI | failpoint、mock write failure、partial log fixture、重复 completion | 真实断网、真实磁盘破坏 |
| 集成环境 | kill worker/control/logd、tc netem、IOChaos、TimeChaos | 影响共享依赖或真实用户 |
| 预生产 | 接近生产拓扑的 chaos、依赖慢/断、节点重启 | 没有 rollback/stop condition 的破坏性实验 |
| 生产小流量 | 单实例 kill、单 AZ 小比例隔离、限时延迟注入 | 全局网络分区、全量数据库故障、无监控实验 |
| 生产全局 | 极少数、成熟后、演练级别 | 默认不做，除非业务和组织都接受风险 |

对 LogServe 当前阶段，我不会说“应该在生产跑 chaos”。项目文档里的边界是单机 Ubuntu、3 workers、mock LLM、机制验证。它可以做本地 fault injection、CI 恢复测试、预生产式脚本，但没有多节点生产部署、真实对象存储/数据库/GPU 负载、完整 SLO 和回滚体系。因此它不具备直接生产 chaos 的前提。

生产运行前至少要满足这些条件：

- 有明确 steady state：成功率、p99、队列长度、redelivery rate、worker heartbeat、error budget。
- 有自动 stop condition：指标超过阈值就停止实验。
- 有人工 kill switch：值班人员可以一键停止。
- 有 blast radius 限制：只影响一小部分实例、租户、流量、AZ 或时间窗口。
- 有回滚路径：配置、流量、注入规则能恢复。
- 有演练窗口和告警静默策略：不要和发布、迁移、活动峰值叠加。
- 有审计记录：谁启动、影响范围、持续时间、指标变化、结论。
- 有客户影响评估：实验消耗 error budget，不能假装没有风险。

面试回答：

```text
故障注入可以在生产运行，但不能一开始就在生产跑破坏性实验。我的顺序是 CI failpoint、集成环境 kill/netem、预生产 chaos、小流量生产实验。生产实验必须有 steady state 假设、stop condition、blast radius 限制、回滚、值班和审计。对 LogServe 当前版本，我会明确说它还处在单机机制验证阶段，适合在本地和预生产环境做 fault injection；如果未来上生产，再把实验缩到单 worker、单租户、单流量切片，先验证保护措施，再考虑更真实的 chaos。
```

## Q019. 生产 chaos test 需要哪些保护措施？

生产 chaos test 的保护措施要覆盖实验前、实验中、实验后。只写“有监控”不够。监控只是发现问题，真正的保护还包括目标选择、权限边界、停止条件、流量隔离、回滚和审计。

实验前要有 review：

```text
实验目的是什么？
steady state 指标是什么？
注入的故障是什么？
影响哪些资源、租户、流量、区域？
最大持续时间多久？
停止条件是什么？
谁值班？
如何回滚？
失败后如何复盘？
```

实验模板要写清楚 actions、targets、stop conditions。AWS FIS 的 experiment template 就是这个模型：actions 表示对资源做什么，targets 表示作用在哪些资源上，stop conditions 表示哪些 CloudWatch alarm 触发后停止实验，experiment role 控制权限。这个结构可以作为生产 chaos 的通用模板，即使不用 AWS，也应该有类似字段。

保护措施可以分成几类。

访问控制：

- 只有授权人员或自动化系统能启动实验。
- 实验角色最小权限，只能操作允许的资源类型和标签。
- 禁止对数据库主节点、全局控制面、支付链路等高危资源直接注入，除非有专门审批。
- 所有实验有审计日志。

目标限制：

- 用标签选择目标，比如 `chaos-allowed=true`。
- 默认只选一台实例、一个 Pod、一个 shard、一个租户或 1% 流量。
- 禁止空 selector，避免误选全量资源。
- 对 stateful 组件先测 follower/replica，再测 leader。

时间限制：

- 每个实验必须有最大 duration。
- 不在发布窗口、迁移窗口、流量峰值和大促期间运行。
- 实验自动到期清理注入规则。
- 注入前后都要有观察窗口。

停止条件：

- p99 latency 超阈值。
- 5xx/error rate 超阈值。
- redelivery 或 retry storm 超阈值。
- queue depth 超阈值。
- SLO burn rate 超阈值。
- 关键业务指标下降。
- 值班人员手动停止。

流量保护：

- 先用 shadow/canary 流量。
- 对用户请求按租户、地域、实验 header、百分比流量切片。
- 有熔断、限流、降级。
- 失败时自动从负载均衡摘除目标。

回滚和清理：

- 自动删除 `tc` 规则、iptables 规则、Chaos CRD、FIS experiment。
- 重启被暂停的进程，恢复 `SIGCONT`。
- 恢复副本数、路由权重、feature flag。
- 检查实验资源没有残留。

观测和证据：

- 实验开始/结束事件进入日志和 tracing。
- dashboard 标注 chaos window，避免误判为普通事故。
- 采集 before/during/after 的 steady state。
- 记录实际 blast radius 和客户影响。

对 LogServe，可以把保护措施映射成具体指标：

| 保护项 | LogServe 观测 |
|---|---|
| stop condition | workflow p99、task p99、queue depth、worker heartbeat age |
| data safety | shared log CRC error、partial tail、result_ref missing |
| recovery safety | stale completion rejected、redelivery count、actor epoch conflict |
| blast radius | 只选一个 worker、一个 workflow namespace、一个 model version |
| rollback | 移除 NetworkChaos/IOChaos，重启 worker，恢复调度策略 |

面试回答：

```text
生产 chaos test 必须像一次受控变更。保护措施包括最小权限、目标标签白名单、单实例或小流量 blast radius、最大持续时间、自动 stop condition、手动 kill switch、回滚脚本、观测标注和值班确认。以 AWS FIS 的模型看，实验模板至少要有 actions、targets、stop conditions 和 experiment role。对 LogServe，如果未来做生产实验，我会先只选一个 worker 或一个 workflow tenant，监控 p99、queue depth、redelivery、stale completion、actor epoch conflict 和 shared log error；任何 SLO burn 或数据一致性异常都自动停止。
```

## Q020. fault injection 的 blast radius 如何限制？

blast radius 就是一次故障注入最多能影响多大范围。限制 blast radius 的原则是：先限制目标，再限制动作，再限制时间，最后限制影响传播。只靠“实验人员小心一点”不算保护。

第一层是目标范围。不要让实验工具可以随便选全量资源。用标签、命名空间、租户、shard、AZ、实例数做硬限制：

```yaml
selector:
  namespaces:
    - logserve-chaos-canary
  labelSelectors:
    chaos-allowed: "true"
    app: logserve-worker
mode: fixed
value: "1"
```

这表示只在 `logserve-chaos-canary` 命名空间里选一个带白名单标签的 worker。比“所有 worker 随机一个”安全，因为 namespace 和 label 都在控制面上形成了边界。

第二层是流量范围。即使故障打到一个实例，也要保证只有少量请求会经过它：

```text
1% canary traffic
内部测试租户
shadow traffic
指定 workflow namespace
指定 model version
指定 region/AZ
```

对 LogServe，可以只让某个 `workflow_namespace=chaos-lab` 的请求进入被注入 worker。真实用户 workflow 不调度到这个 worker。这样 worker kill、slow fsync、network delay 都不会直接影响全量业务。

第三层是动作强度。故障不是越猛越好。先从弱故障开始：

```text
delay 50ms -> 200ms -> 1s
loss 0.1% -> 1% -> 5%
kill one worker -> kill one worker repeatedly
partition one direction -> partition both directions
fsync delay 100ms -> 500ms -> 2s
```

每次只提高一个维度。不要一开始同时加 1s 延迟、10% 丢包、kill worker、让磁盘 EIO。那样就算系统崩了，也很难知道是哪条恢复路径坏了。

第四层是时间。每个实验必须有 duration 和自动清理：

```text
start: 10:00
inject: 60s
observe during: 60s
cleanup: automatic
observe after: 5min
hard timeout: 10min
```

没有 hard timeout 的注入规则很危险。`tc`、iptables、Chaos CRD、暂停进程、磁盘 freeze 都可能残留。blast radius 不只看影响范围，也看影响持续时间。

第五层是传播控制。很多故障本身只影响一个实例，但重试、队列、熔断配置会把影响放大。要限制传播：

- retry 要有上限和 jitter，防止 retry storm。
- deadline 要端到端传递，防止请求无限堆积。
- queue 要有容量上限，防止内存被打爆。
- circuit breaker 要能快速摘除坏实例。
- backpressure 要让上游看到明确失败或排队状态。
- redelivery 要限速，防止 worker crash 后所有任务同时重跑。

第六层是停止条件。AWS FIS 里 stop condition 用 CloudWatch alarm 停止实验；同样的概念可以迁移到任何平台。比如：

```text
if workflow_p99 > baseline_p99 * 2 for 3 minutes: stop
if task_error_rate > 1%: stop
if queue_depth > 10000: stop
if stale_completion_conflict > expected threshold: stop
if shared_log_crc_error > 0: stop immediately
if customer-facing SLO burn rate > threshold: stop
```

对 LogServe，我会把 blast radius 写成实验模板字段：

```yaml
experiment:
  name: kill-one-worker-canary
  target:
    namespace: chaos-lab
    labels:
      app: logserve-worker
      chaos-allowed: "true"
    maxTargets: 1
  traffic:
    workflowNamespace: chaos-lab
    maxPercent: 1
  action:
    type: kill
    duration: 30s
  stopConditions:
    - task_error_rate > 0.01
    - workflow_p99_ms > 2 * baseline
    - shared_log_crc_error > 0
  rollback:
    - remove chaos rule
    - restart worker
    - restore scheduler weights
```

面试回答：

```text
限制 blast radius 不能靠口头约定，要靠机制。目标上，用 namespace、label、tenant、shard、maxTargets 限制；流量上，只给 canary 或 shadow 流量；动作上，从低强度开始，一次只改一个变量；时间上，必须有 duration、hard timeout 和自动清理；传播上，用 retry limit、deadline、backpressure、circuit breaker 防止放大；停止上，用 SLO 和一致性指标做 stop condition。对 LogServe，我会先只打一个 chaos-allowed worker，限定 workflow namespace，最多 1% 流量，监控 p99、queue depth、redelivery、stale completion 和 shared log 错误。只要一致性指标异常，立刻停止，而不是等用户感知到故障。
```
## Q021. 如何避免故障测试本身造成数据丢失？

故障测试最容易被忽略的风险是：我们本来想验证系统能不能扛住故障，结果测试工具、清理脚本或误选目标反而把真实数据删了。避免这种问题，核心不是“测试人员小心”，而是把故障测试当成一次有权限边界、有目标边界、有回滚边界的受控变更。

第一步是环境隔离。能不用生产数据就不用生产数据；必须接近生产形态时，也要使用脱敏数据、合成工作负载、影子流量或专门的 chaos tenant。测试目录、对象存储 bucket/prefix、数据库 schema、Kubernetes namespace、云账号和 IAM role 都应该分开。不要让故障注入工具拿到能删除生产存储、生产快照、生产日志目录的权限。

```text
推荐：
chaos account / chaos namespace / chaos data prefix / synthetic workload

避免：
直接指向生产数据库
直接复用生产共享日志目录
清理脚本 rm -rf 可由变量拼错扩大到根目录
故障工具有全账号写权限
```

第二步是目标白名单。故障测试工具默认应该找不到任何目标，只有显式带 `chaos-allowed=true`、位于指定 namespace、属于指定 test tenant 的资源才能被选中。AWS FIS 的 experiment template 把 actions、targets、stop conditions、experiment role 分开，就是一种很好的安全模型：工具做什么、对谁做、什么时候停、用什么权限，必须在模板里写清楚。Chaos Mesh、Kubernetes 或自研注入器也应该遵循同样的结构。

第三步是先备份再注入。对状态组件，注入前至少要知道：

```text
最后一次备份在哪里？
备份是否可恢复，而不是只证明“备份任务成功”？
当前测试数据的命名空间或 prefix 是什么？
如何区分测试产生的数据和用户数据？
如果注入导致数据损坏，回滚到哪个恢复点？
RPO 允许丢多少数据？
```

注意，备份不是只为了灾难恢复，也是为了避免故障测试误伤。AWS 的灾备文档也强调，删除、损坏、误操作这类数据灾难需要 point-in-time recovery 或备份回退；高可用副本不等于能从逻辑错误里恢复。

第四步是把 destructive fault 和 non-destructive fault 分级。杀 worker、加网络延迟、让请求超时，一般不会直接修改持久数据；但磁盘写失败、partial write、数据 corruption、删除目录、时钟跳变、强制 failover，都可能影响持久状态。高危故障需要更小目标、更短 duration、更严格 stop condition，最好先在一次性环境里跑。

第五步是所有清理动作都必须有保护条件。很多数据丢失不是注入本身造成的，而是清理阶段造成的。清理脚本必须检查路径、namespace、标签和租户，不允许空变量参与删除。

```powershell
$root = (Resolve-Path -LiteralPath ".").Path
$target = Join-Path $root "chaos-data"
$resolved = [System.IO.Path]::GetFullPath($target)
if (-not $resolved.StartsWith($root, [System.StringComparison]::OrdinalIgnoreCase)) {
    throw "refuse to clean outside workspace"
}
Remove-Item -LiteralPath $resolved -Recurse -Force
```

这段脚本的重点不是 PowerShell，而是“先解析绝对路径，再确认它仍在允许的根目录下”。同样的原则适用于 Linux shell、CI cleanup、Kubernetes resource cleanup。

第六步是 stop condition。AWS FIS 的 stop condition 用 CloudWatch alarm 做实验停止条件；抽象出来就是：在实验开始前先定义 steady state，实验中一旦 latency、error rate、queue depth、retry storm、数据校验错误或业务指标超过阈值，就自动停止。尤其是数据安全指标要比性能指标更敏感，比如发现 CRC 错误、不可解析日志尾部、重复提交冲突异常升高时，不要继续“观察系统能不能恢复”，先停止实验并保留现场。

对 LogServe，避免故障测试造成数据丢失，我会这样落地：

| 风险 | 保护措施 |
|---|---|
| 误删共享日志 | 所有故障测试使用临时 log dir，清理脚本检查绝对路径 |
| 误写真实结果 | result store 使用 chaos prefix，不复用真实 prefix |
| 注入 partial write 后覆盖证据 | 先复制 segment，再对副本注入截断或 corruption |
| worker crash 导致外部副作用重复 | 测试 payload 使用幂等 side effect 或 mock sink |
| control restart 后状态错乱 | 以 shared log replay 后的 materialized view 为准，不信内存状态 |
| chaos 规则残留 | 每个实验有 duration、cleanup、post-check |

面试回答：

```text
我会先把故障测试和真实数据隔离：独立 namespace、独立目录、独立 bucket prefix、独立 test tenant、最小权限 role。然后把实验模板写清楚 actions、targets、stop conditions 和 cleanup，目标必须用白名单标签选择，禁止空 selector。对状态组件，注入前要有可验证恢复的备份或快照；对 destructive fault，比如 partial write、磁盘 corruption、删除、强制 failover，只在一次性环境或副本上先跑。LogServe 里我会让故障测试只操作临时 shared log 和 synthetic workflow，任何 shared log CRC error、不可解析 tail、result_ref missing 或 stale completion 异常都会触发停止，而不是继续扩大实验。
```

## Q022. 如何自动化故障场景并纳入 CI？

故障测试要纳入 CI，不能把“混沌工程”原样搬进每个 PR。CI 的目标是快速、可重复、能定位回归；大规模随机 chaos 更适合 nightly、pre-release 或专门环境。一个成熟做法是分层：

```text
PR gate:
  deterministic failpoint
  单进程 crash/restart
  partial write parser
  duplicate completion
  race test

nightly:
  随机 kill worker
  随机网络 delay/loss
  随机 fsync slow
  多 seed 重复运行

pre-release:
  长时间 soak
  Jepsen-style black-box history test
  更真实的多节点/多进程故障
  dashboard/observability artifact
```

第一类是确定性 failpoint。把故障点做成测试可控的开关，而不是靠 sleep 撞运气。例如：

```text
FAIL_BEFORE_APPEND_TASK_STARTED
FAIL_AFTER_APPEND_TASK_STARTED_BEFORE_ACK
FAIL_AFTER_WORKER_EXEC_BEFORE_COMPLETE
FAIL_DURING_CONTROL_REBUILD
FAIL_ON_FSYNC_EVERY_N
FAIL_ON_LOG_TAIL_READ
```

每个 failpoint 都对应一个恢复 invariant。这样 PR 级测试可以在几秒到几十秒内跑完，而且失败时能告诉你具体是哪条恢复路径坏了。

第二类是进程级集成测试。CI 可以启动真实 `logd`、control、worker，但使用临时目录和短 workload。测试流程类似：

```text
start logd with temp dir
start control
start 2 workers
submit workflow
wait until one task started
kill selected worker
restart or keep down
wait for redelivery
assert workflow completed once
restart control
assert materialized view rebuilt from log
```

这里不追求模拟所有生产细节，而是覆盖“任务执行到一半 crash、控制面重启、日志重放、重复完成处理”这些核心语义。

第三类是历史检查。CI 不只看最后状态，还要保存 operation history：

```json
{"type":"invoke","op":"submit_workflow","workflow_id":"w1","time":"..."}
{"type":"ok","op":"submit_workflow","workflow_id":"w1","task_id":"t1","time":"..."}
{"type":"nemesis","op":"kill_worker","worker_id":"worker-1","time":"..."}
{"type":"invoke","op":"complete_task","task_id":"t1","attempt":1,"time":"..."}
{"type":"info","op":"complete_task","task_id":"t1","attempt":1,"error":"connection lost","time":"..."}
{"type":"ok","op":"complete_task","task_id":"t1","attempt":2,"time":"..."}
```

这种 history 可以给自研 checker，也可以转换成 Jepsen/Knossos/Elle 风格的输入。Jepsen 的 README 明确描述了这条链路：客户端生成操作，nemesis 注入故障，记录 operation history，最后由 checker 分析历史是否正确。

第四类是 CI artifact。故障测试失败时，最怕只看到一句 timeout。每次运行都应该产出：

```text
seed
git commit
binary version
test config
fault schedule
operation history
control log
worker log
shared log segment metadata
metrics snapshot
trace export
checker output
```

第五类是资源和时间预算。PR gate 里的故障测试必须短、稳定、低权限。需要 root、`tc netem`、iptables、真实多机网络分区的测试，不适合默认在普通 CI runner 上跑；可以放到带专用权限的 nightly runner。CI 里要避免残留系统级规则，因为一个失败的 `tc qdisc` 或 iptables 规则可能污染后续测试。

对 LogServe，我会把 CI 分成三档：

| 层级 | 场景 | 通过条件 |
|---|---|---|
| PR 快速测试 | partial tail、worker crash、duplicate completion、control restart | recovery invariant 全部通过 |
| PR 并发门禁 | `go test -race ./internal/control ./internal/worker` | 无 data race |
| nightly 随机测试 | 随机 kill、slow fsync、网络延迟、重复完成乱序 | 记录 seed，失败可复现 |
| release 前 | 长时间 fault campaign、history checker、dashboard snapshot | 无丢任务、无重复提交、RTO/RPO 达标 |

面试回答：

```text
我不会把重型 chaos 全塞进每个 PR，而是做 CI 分层。PR 上跑确定性 failpoint 和小型进程级恢复测试，比如 worker crash、control restart、partial log tail、duplicate completion；nightly 再跑多 seed 随机故障；release 前跑更接近 Jepsen 风格的历史检查和长时间 soak。每个场景都要有临时目录、固定 seed、fault schedule、operation history、日志和 checker 输出。这样故障测试既能自动化，又不会因为慢、不稳定或权限过大把 CI 变成另一个故障源。
```

## Q023. 随机故障测试和确定性故障测试各有什么价值？

随机故障测试和确定性故障测试不是互相替代的关系。随机测试负责发现你没想到的组合，确定性测试负责把已经发现的问题固定成回归用例。

确定性故障测试的价值是可复现、可定位、可作为 CI gate。比如你明确知道一个风险点：worker 在执行完成后、写 `TaskCompleted` 前 crash。这就应该写成确定性测试：

```text
submit task
worker poll task
worker execute task
inject crash before TaskCompleted append
restart worker or let another worker redeliver
assert task eventually completed once
assert no duplicate external result
```

这种测试很适合覆盖边界条件：

```text
append 前 crash
append 后 ack 前 crash
fsync 前 crash
fsync 后 crash
control rebuild 一半 crash
日志尾部只有 header 没有 body
重复 completion 先到
迟到 completion 后到
```

它的优点是失败时原因明确，缺点是只能覆盖你已经写出来的场景。分布式系统的问题往往出在组合上：慢 fsync 加 worker crash，加上 control restart，再叠加 retry storm，单个确定性用例很难穷举。

随机故障测试的价值是探索 interleaving。它通过随机选择故障类型、故障时间、目标实例、并发客户端、网络延迟、重启顺序，逼系统进入平时很少出现的状态。随机测试更容易暴露：

```text
隐含竞态
时序窗口
恢复路径之间的相互影响
罕见重复完成
只在高并发下出现的 stale owner
只在网络慢和重启同时发生时出现的 lost ack
```

但随机测试的问题也很明显：失败难复现、噪声大、时间长、结果解释成本高。所以随机测试必须记录 seed 和 schedule。一个随机测试失败后，正确流程不是“再跑几次看看”，而是：

```text
保存 seed 和 fault schedule
保存 operation history
保存所有进程日志和 trace
用同 seed 重放
缩小 workload 和故障集合
把最小复现转成确定性 failpoint 测试
```

两者的分工可以总结成：

| 类型 | 主要价值 | 适合放在哪里 | 失败后怎么处理 |
|---|---|---|---|
| 确定性测试 | 回归、防止已知 bug 复发 | PR CI | 直接修复 |
| 随机测试 | 发现未知 interleaving | nightly/soak | 记录 seed，缩小并固化 |
| property-based fault test | 在随机空间里检查 invariant | nightly 或专项 | shrink 到最小案例 |
| Jepsen-style test | 黑盒验证历史正确性 | 专项或 release 前 | 分析 history 和 checker 证据 |

对 LogServe，确定性测试应该覆盖 shared log replay、worker redelivery、actor epoch fencing、duplicate completion rejection、partial tail recovery。随机测试则适合组合这些因素：随机 kill worker/control、随机 fsync slow、随机重复 completion、随机 actor command 并发，并始终检查“日志是事实源、视图可重建、任务不丢、actor command_seq 不倒退”这些 invariant。

面试回答：

```text
确定性故障测试适合验证已知恢复路径，价值是稳定、可复现、能进 PR CI；随机故障测试适合探索未知时序组合，价值是发现单个手写用例想不到的竞态和恢复路径交互。我的做法是随机测试用于发现问题，确定性测试用于保存问题。随机失败后必须记录 seed、fault schedule、history、日志和 trace，然后把失败场景缩小，最后变成一个确定性的回归用例。
```

## Q024. Jepsen 风格测试主要关注什么？

Jepsen 风格测试主要关注：在真实故障下，系统对外暴露的行为是否满足它声称的正确性模型。它不是只看服务有没有挂，也不是只看 p99 有没有变差，而是把客户端看到的操作历史拿出来检查：这些成功、失败、超时、读取、写入、事务、队列操作，能不能解释成一个合法的系统行为。

Jepsen 的典型结构是：

```text
client/generator: 生成并发操作
system under test: 被测分布式系统
nemesis: 注入故障，比如网络分区、进程 kill、时钟偏移
history: 记录每个操作的 invoke/ok/fail/info
checker: 检查 history 是否满足模型
report: 输出一致性、可用性、性能和故障时间线
```

Jepsen README 里也强调：测试会启动一组逻辑单线程 client，generator 为每个 client 生成操作；nemesis 在执行过程中注入故障；每个操作开始和结束都会记录到 history；最后 checker 分析 history 的正确性。

这和普通 chaos test 的重点不同。普通 chaos test 常问：

```text
系统还活着吗？
延迟有没有上升？
错误率有没有超过阈值？
自动恢复有没有完成？
```

Jepsen 风格测试会继续问：

```text
已经 ack 的写是否丢了？
读是否读到了不可能存在的值？
两个客户端是否同时认为自己拿到了唯一锁？
队列是否丢消息或重复确认？
事务是否出现脏读、丢失更新、实时顺序违背？
在网络分区期间返回成功的操作，分区恢复后还能被解释吗？
```

所以 Jepsen 的核心是 correctness under faults。性能和可用性当然也会记录，但它最有价值的地方是把故障期间的外部历史和一致性模型绑定起来。Jepsen consistency guide 把一致性模型定义成 safety property，也就是一组系统允许执行的合法 histories；checker 要做的就是判断观测到的 history 是否属于这组合法 histories。

如果把 LogServe 映射成 Jepsen 风格测试，可以这样建模：

| LogServe 操作 | 可观察历史 |
|---|---|
| submit workflow | invoke/ok/fail/info |
| poll task | worker 获取任务 |
| complete task | completion attempt |
| get workflow state | 读 workflow view |
| actor command | submit/apply/read actor state |
| control restart | nemesis event |
| worker kill | nemesis event |
| fsync slow/partial write | nemesis event |

checker 关注的不是内部日志“看起来写了几条”，而是外部可见结果能不能解释：

```text
一个 task 最终只有一个 canonical completion
已经成功返回的 workflow submit 不应在恢复后消失
actor command_seq 单调递增
旧 epoch worker 的 completion 不能覆盖新 owner
control 重启后的 view 能从 shared log 重建
read state 不应看到未来事件或丢失已确认事件
```

需要注意，Jepsen 风格测试也不是万能的。它检查的是你建模过的 API 和一致性语义；如果外部副作用、缓存状态、dashboard 展示没有进入 history，checker 就不会自动发现这些问题。测试结论也要写清楚模型范围：是单对象 linearizability、事务 serializability、队列语义，还是某组业务 invariant。

面试回答：

```text
Jepsen 风格测试关注的是故障下的外部可观察正确性。它会用并发客户端产生操作，用 nemesis 注入网络分区、kill、时钟偏移等故障，把每个操作的开始、成功、失败、超时记录成 history，再用 checker 判断这个 history 是否满足系统声称的一致性模型。它和普通 chaos test 的区别是：普通 chaos 常验证系统是否恢复、SLO 是否被打破；Jepsen 还会问已确认写是否丢失、读是否读到不可能的值、锁是否双持有、事务是否违反隔离。对 LogServe，我会把 workflow submit/complete、actor command/read、worker kill/control restart 写进 history，再检查 shared log replay 后外部状态是否仍能解释。
```

## Q025. linearizability checker 能发现哪些问题？

linearizability checker 检查的是：并发操作的外部历史，是否可以排列成一个尊重实时顺序的合法顺序执行。Herlihy 和 Wing 的经典定义是，每个操作看起来都像在 invocation 和 response 之间某个瞬间原子生效。Jepsen 的 linearizable model 也强调两个要点：单对象语义，以及如果 A 在 B 开始前已经完成，那么 B 的逻辑生效顺序必须在 A 之后。

一个 linearizability checker 通常能发现这些问题：

| 问题 | 表现 |
|---|---|
| stale read | 写成功后，后续读仍读到旧值 |
| lost write | 写返回成功，但之后永远读不到 |
| dirty/failed write visible | 写失败或超时，却被后续读到，而且模型无法解释 |
| read impossible value | 读到了从未成功写入、也无法由 pending 操作解释的值 |
| duplicate success | 互斥锁、CAS、队列确认这类操作出现多个互斥成功 |
| real-time order violation | A 完成后 B 才开始，但系统表现像 B 先发生 |
| split-brain | 两个分区都返回了互斥资源的成功状态 |
| non-monotonic read | 同一对象的读取顺序倒退 |
| queue/register state impossible | 出队顺序、寄存器值、CAS 结果无法被任意合法顺序解释 |

Knossos 的 README 里有一个很典型的模型：history 由 invoke 和 completion 组成，completion 可以是 `ok`、`fail` 或 `info`。`info` 表示不确定，比如超时或连接断了，checker 会尝试判断这个 pending 操作是否可能已经生效。这个能力很重要，因为真实系统里最难处理的不是明确失败，而是“客户端不知道服务端有没有执行”。

举一个寄存器例子：

```text
初始值: 0

client A: invoke write(1)
client A: ok write(1)
client B: invoke read()
client B: ok read(0)
```

如果 `write(1)` 已经在 `read()` 开始前返回成功，那么 `read()` 还读到 0，就违反 linearizability。再比如互斥锁：

```text
client A: ok acquire(lock)
client B: ok acquire(lock)
```

如果两个 acquire 没有 release 夹在中间，checker 会发现没有任何合法顺序能让两个客户端同时成功。

但 linearizability checker 也有边界：

```text
它不证明性能达标。
它不证明系统最终一定恢复，因为 linearizability 是 safety，不是 liveness。
它主要适合单对象或明确定义对象边界的 API。
如果系统只承诺 eventual consistency，就不能拿 linearizability 当默认标准。
如果业务副作用没有被建模，checker 不会替你发现重复扣款或重复发邮件。
如果 history 记录不完整，结果可能是 unknown 或漏检。
```

对 LogServe，linearizability checker 适合用在“单对象、强语义”的地方：

| 对象 | 可检查语义 |
|---|---|
| task completion | 一个 task 只有一个 canonical completion |
| actor command stream | 同一 actor 的 command_seq 顺序一致 |
| worker ownership epoch | 旧 epoch completion 不应成功覆盖新 epoch |
| workflow state read | 已确认事件不应在后续读里消失 |
| lock/lease 类控制面对象 | 不能双 owner |

但它不适合直接检查整个工作流所有行为。工作流有 DAG、异步执行、重试、结果引用、缓存和外部副作用，应该拆成多个对象或写业务 invariant checker。例如“每个 step 的最终输出最多一次生效”“成功 workflow 的所有依赖 step 都完成”“失败 step 的 retry 不超过策略上限”。

面试回答：

```text
linearizability checker 能发现那些无法解释成合法原子顺序的问题，比如成功写丢失、旧读、读到不可能的值、互斥资源双成功、CAS 错误成功、队列乱序、split brain、实时顺序违背。它的输入通常是 invoke/ok/fail/info 这样的操作历史，checker 会尝试把并发操作排成一个满足对象顺序语义和实时顺序的序列。它的边界也很重要：linearizability 是 safety，不保证 liveness 或性能；它适合单对象语义，不适合拿来泛化证明整个复杂业务系统正确。LogServe 里我会用它检查 task completion、actor command_seq、ownership epoch 这类对象，而不是直接说整个 runtime 都 linearizable。
```

## Q026. 故障测试中如何记录时间线？

故障测试的时间线要同时服务两件事：人能看懂发生了什么，checker 能用它验证历史。只靠进程日志不够，因为日志通常分散在 control、worker、logd、client、fault injector、metrics 系统里，而且机器时钟可能不同步。时间线应该是一个统一的实验证据流。

最基本的事件类型包括：

```text
experiment_started
steady_state_sampled
client_invoke
client_ok
client_fail
client_info_timeout
nemesis_start
nemesis_stop
process_started
process_killed
network_partition_on
network_partition_off
fsync_delay_on
fsync_delay_off
recovery_started
recovery_completed
invariant_checked
experiment_stopped
```

每条事件至少要有这些字段：

```json
{
  "run_id": "chaos-2026-06-19-001",
  "seq": 128,
  "wall_time": "2026-06-19T10:03:21.123456Z",
  "monotonic_ms": 84231,
  "component": "worker",
  "node": "n2",
  "event": "task_completed_append_attempt",
  "workflow_id": "wf-17",
  "task_id": "task-9",
  "attempt": 2,
  "worker_id": "worker-2",
  "epoch": 4,
  "trace_id": "4bf92f3577b34da6a3ce929d0e0e4736",
  "span_id": "00f067aa0ba902b7",
  "log_offset": 18492,
  "result": "ok"
}
```

这里同时记录 wall time 和 monotonic time。wall time 方便人对齐 dashboard、报警和日志；monotonic time 适合计算持续时间，避免 NTP 调整、时钟跳变让耗时变成负数。对分布式系统，不要把不同机器的 wall clock 当成严格因果顺序。真正的顺序应该来自 log offset、operation history 的 invoke/complete 边界、trace parent-child/link、actor command_seq、epoch、版本号等逻辑信息。

OpenTelemetry 的 traces 可以帮助把跨组件请求串起来。它的 span context 包含 Trace ID、Span ID、Trace Flags、Trace State；span event 适合记录某个具体时间点发生的事件。W3C Trace Context 则定义了 `traceparent`/`tracestate` 头，用来在不同服务之间传播统一 trace context。故障测试里，client、control、worker、logd、fault injector 都应该带同一个 `run_id`，业务请求带 `trace_id`，异步队列用 span link 或显式 causal id 关联。

Jepsen/Knossos 风格的 history 还需要记录操作生命周期：

```text
invoke: 客户端开始调用
ok: 调用成功，并返回明确结果
fail: 调用明确失败，系统保证没有生效或按模型定义失败
info: 调用结果不确定，比如超时、连接断开、客户端 crash
```

`info` 不能随便丢。很多一致性 bug 就藏在超时操作里：客户端以为失败了，系统其实执行了；或者系统返回成功前 crash，恢复后又执行了一次。checker 需要这些不确定操作来判断 history 是否仍可解释。

对 LogServe，我会把时间线分成三条再汇总：

| 时间线 | 关键字段 |
|---|---|
| client history | workflow_id、operation、invoke/ok/fail/info、trace_id |
| system event log | log_offset、event_type、stream、task_id、actor_id、epoch |
| nemesis schedule | fault_type、target、start/stop、duration、seed |

最终报告要能回答：

```text
故障什么时候开始？
当时 steady state 是什么？
哪些请求受影响？
系统什么时候检测到故障？
什么时候开始恢复？
哪些任务被 redeliver？
哪些 completion 被拒绝？
恢复后哪些 invariant 被检查？
用户可见错误和内部恢复事件如何对应？
```

面试回答：

```text
故障测试时间线不能只靠散落日志。我会记录统一的 JSONL history：每个 client operation 有 invoke/ok/fail/info，每个 nemesis action 有 start/stop，每个恢复动作有 detected/recovered，每条系统事件带 run_id、trace_id、component、node、monotonic time、wall time、log offset、epoch 和业务 id。wall time 用来对齐报警，monotonic time 用来算耗时，真正的因果顺序靠 log offset、operation boundary、trace/span、command_seq、epoch。对 LogServe，shared log offset 是很关键的顺序锚点，control/worker 的日志和 OpenTelemetry trace 只是辅助解释。
```

## Q027. 如何复现一个只在随机故障中出现的问题？

随机故障里出现的问题，复现思路不是“继续随机跑，期待再撞一次”，而是把随机性变成可控输入，把复杂场景缩小成最小失败用例。

第一步是保留现场。随机故障测试每次运行都必须输出：

```text
random seed
fault schedule
workload config
client count
operation mix
binary version
git commit
environment info
process logs
operation history
metrics snapshot
trace export
shared log / WAL / segment metadata
checker result
```

没有 seed 和 schedule 的随机失败，基本只能当作线索，不能当作可工程化修复依据。seed 只能复现伪随机选择；如果故障还依赖真实时间、CPU 调度、网络抖动，就还需要记录 fault schedule 和关键同步点。

第二步是同 seed 重放。先用完全相同配置跑一次，确认是否稳定复现。如果能稳定复现，就直接进入缩小；如果不能稳定复现，说明还有未受控因素，比如真实时间窗口、goroutine 调度、磁盘延迟、外部依赖、测试污染或资源竞争。这时要增加 instrumentation，而不是盲目加 sleep。

第三步是缩小变量。一次只关掉一个维度：

```text
减少 client 数量
减少操作种类
减少 key/workflow/actor 数量
缩短测试时间
固定 nemesis 目标
只保留一种故障
降低并发
固定 worker 数量
固定调度顺序
```

如果原始失败是：

```text
5 clients + worker kill + fsync slow + control restart + duplicate completion
```

可以逐步缩成：

```text
2 clients + kill worker + duplicate completion
```

再缩成：

```text
1 workflow
1 task
worker attempt 1 完成前断连
worker attempt 2 完成成功
attempt 1 的迟到 completion 后到
```

第四步是把时间点改成 failpoint。随机测试经常靠“刚好在某个窗口 kill 掉进程”触发 bug，但这种窗口很窄。修 bug 前，应该在代码关键点加测试开关：

```text
after append TaskStarted before fsync
after worker side effect before TaskCompleted
after TaskCompleted append before client ack
during control replay after N events
after actor ownership granted before snapshot
```

这样就从“概率复现”变成“稳定复现”。

第五步是用 history checker 辅助定位。不要只看最后报错，要找第一个无法解释的操作。Knossos 输出里会指出无法继续 linearize 的操作和之前仍然合法的前缀；Elle 也会给出事务依赖环和最小 witness。这个思路对自研 checker 一样有用：找到“历史第一次变坏”的位置，往往比看最终状态更快。

第六步是固化回归测试。修复后要保留两个测试：

```text
一个最小确定性用例：PR 必跑
一个原始 seed 或简化 seed：nightly 保留
```

这样既防止 bug 复发，也能继续覆盖随机空间。

对 LogServe，复现随机故障时我会优先收集：

```text
seed
workflow_id / task_id / actor_id
worker_id / epoch / attempt
TaskSubmitted/Started/Completed 的 log offset
control restart 发生在 replay 的哪个 offset
迟到 completion 的到达时间
redelivery decision 的依据
最终 materialized view
```

如果发现的是“迟到 completion 覆盖了新 attempt”，就把它缩成 deterministic case：attempt 1 执行后断连，attempt 2 完成并写入 canonical completion，attempt 1 的 completion 迟到，断言 control 必须拒绝旧 completion。

面试回答：

```text
复现随机故障要先把随机性记录下来：seed、fault schedule、workload、operation history、日志、trace、版本和环境。然后用同 seed 重放，逐步减少 client、操作种类、故障种类和目标范围，找到最小失败历史。真正关键的一步是把随机时间窗口改成确定性 failpoint，例如“worker side effect 后、TaskCompleted append 前 crash”。修复后保留一个最小确定性回归测试，再把原始 seed 放到 nightly，避免这个问题以后只靠运气才能发现。
```

## Q028. 如何将故障场景映射到用户可见影响？

故障测试不能只说“worker 被 kill 后系统恢复了”。面向用户，真正关心的是：请求是否失败、延迟是否升高、结果是否丢失、是否重复执行、数据是否变旧、恢复用了多久。也就是说，故障场景要映射到用户旅程和 SLI。

Google SRE 里常用的四个 golden signals 是 latency、traffic、errors、saturation。故障测试可以把内部事件映射到这些外部指标：

| 内部故障 | 用户可见影响 |
|---|---|
| worker crash | 任务完成时间变长、部分请求超时、重试增加 |
| control crash | 提交/查询短暂失败、dashboard 变旧、调度暂停 |
| logd fsync slow | 写路径延迟上升、队列堆积 |
| network delay | p95/p99 上升、deadline exceeded、retry storm |
| partial write | 恢复时间变长，严重时可能拒绝启动并要求人工处理 |
| GC pause | 短时间心跳丢失、误判 worker unhealthy、tail latency spike |
| clock jump | lease/timeout 误判、过早重试、过晚恢复 |

第二步是把用户旅程拆开。以 LogServe 为例，至少有几条不同路径：

```text
提交 workflow
等待 workflow 完成
查询 workflow 状态
执行 actor command
查询 actor state
提交 LLM inference task
命中或未命中 checkpoint cache
查看 dashboard
```

同一个内部故障对不同路径影响不同。worker crash 可能让 workflow 完成变慢，但不一定影响已经完成 workflow 的查询；control restart 可能让提交短暂失败，但不应该让 shared log 里已确认事件丢失；checkpoint cache 丢失可能只让 LLM 冷启动变慢，不应该改变正确性。

第三步是定义影响等级：

| 等级 | 含义 | 示例 |
|---|---|---|
| 无用户影响 | 内部恢复完成，SLI 无明显变化 | 单 worker kill，任务被快速 redeliver |
| 性能影响 | 请求成功但变慢 | workflow p99 翻倍 |
| 可用性影响 | 用户看到错误或超时 | submit workflow 5xx |
| 新鲜度影响 | 用户看到旧状态 | dashboard 落后 30s |
| 一致性影响 | 用户看到错误状态 | 已完成任务恢复后变成 pending |
| 数据丢失/重复副作用 | 用户结果丢失或重复执行 | 已确认 completion 消失，外部 sink 重复写 |

第四步是把故障窗口和 SLI 窗口对齐。时间线里必须标注：

```text
fault start
fault end
detection time
mitigation start
recovery complete
user-visible error window
SLO burn window
```

如果内部恢复很快，但用户错误窗口持续很久，说明问题可能在负载均衡、缓存、客户端重试、队列积压或降级策略，而不在核心恢复逻辑。

第五步是区分“系统恢复”和“用户恢复”。例如：

```text
control 已经重启成功
materialized view 已经 replay 完成
worker 已经重新注册
```

这些只能说明内部恢复。用户侧还要看：

```text
新请求是否成功？
旧请求是否完成？
用户是否看到重复结果？
查询是否读到最新状态？
dashboard 是否仍显示过期告警？
```

对 LogServe，可以建立一张映射表：

| 场景 | 用户指标 | 正确性指标 |
|---|---|---|
| worker kill | task completion latency、workflow makespan | no lost task、canonical completion only once |
| control restart | submit/query error rate、rebuild time | view equals replay(shared log) |
| fsync slow | append latency、queue depth | acked event not lost |
| actor owner crash | actor command latency | command_seq monotonic、epoch fencing |
| checkpoint cache miss | inference cold-start latency | output semantics unchanged |

面试回答：

```text
我会把每个故障映射到用户旅程和 SLI，而不是只看内部恢复。比如 worker crash 对用户表现为 workflow 完成变慢、可能超时；control crash 表现为提交或查询短暂失败；fsync slow 表现为写路径延迟和队列堆积；partial write 则可能影响恢复时间和数据安全。指标上用 latency、traffic、errors、saturation，再补充一致性、新鲜度、重复副作用和 RTO/RPO。对 LogServe，内部恢复完成不等于用户无影响，必须继续检查 workflow 是否最终完成一次、actor state 是否正确、dashboard 是否过期、已确认日志事件是否恢复后仍存在。
```

## Q029. MTTR、MTBF、RTO、RPO 分别是什么？

这四个词经常被混用，但它们不是一类东西。

| 指标 | 全称 | 含义 | 类型 |
|---|---|---|---|
| MTTR | Mean Time To Repair/Recover | 平均修复或恢复时间 | 观测统计 |
| MTBF | Mean Time Between Failures | 平均故障间隔 | 观测统计 |
| RTO | Recovery Time Objective | 业务可接受的最大恢复时间 | 目标 |
| RPO | Recovery Point Objective | 业务可接受的最大数据丢失窗口 | 目标 |

MTTR 是“实际平均多久恢复”。比如过去 10 次 worker crash，从检测到恢复分别用了 8s、12s、9s……平均就是 MTTR。它是历史统计，不是承诺。MTTR 可以按故障类型拆开，否则平均值会掩盖差异：

```text
worker crash MTTR: 10s
control restart MTTR: 25s
log tail repair MTTR: 2min
manual data restore MTTR: 45min
```

MTBF 是“故障之间平均多久”。它通常来自生产历史，而不是一次故障注入实验。比如某组件 30 天内发生 3 次故障，粗略 MTBF 是 10 天。MTBF 适合描述可靠性趋势，但对低频高影响故障，单纯平均值可能误导，因为样本少、分布重尾。

RTO 是“最多允许多久恢复服务”。AWS DR 文档把 RTO 定义为服务中断到服务恢复之间的最大可接受延迟。RTO 是业务目标，不是实验结果。比如：

```text
控制面查询 RTO: 30s
工作流提交 RTO: 2min
离线报表 RTO: 4h
```

RPO 是“最多允许丢多少时间窗口内的数据”。AWS DR 文档把 RPO 定义为从上一个数据恢复点到灾难发生之间可接受的最大时间量。简单说，如果 RPO 是 5 分钟，就代表灾难后最多接受丢失最近 5 分钟的数据；如果 RPO 是 0，就代表已确认数据不能丢。

RTO/RPO 和 MTTR/MTBF 的区别很关键：

```text
RTO/RPO 是你设计时承诺或目标。
MTTR/MTBF 是你运行后测出来的统计。
```

如果目标 RTO 是 30 秒，但实验测得恢复时间经常 90 秒，说明设计或实现不满足目标。不能把“平均恢复 90 秒”说成“RTO 90 秒”，除非业务方接受并修改目标。

对 LogServe，可以这样定义：

| 场景 | RTO | RPO |
|---|---|---|
| worker crash | 从 heartbeat 失效到任务 redelivery 并继续执行的最大时间 | 已确认 TaskSubmitted/Started 不丢 |
| control restart | 从 control 退出到 materialized view 从 shared log rebuild 完成的最大时间 | 已 fsync ack 的 log event 不丢 |
| logd restart | 从 logd 退出到可继续 append/read 的最大时间 | 取决于 fsync 策略，always fsync 目标可接近 0 |
| actor owner crash | 新 owner 接管并从 snapshot+tail replay 恢复的最大时间 | 已确认 ActorCommandApplied 不丢 |
| checkpoint cache 丢失 | cache 重新 warm up 时间 | cache 是性能数据，不应影响业务正确性 |

还要小心 RPO 和 fsync 策略的关系。如果系统在返回成功前每条事件都 fsync，那么对“已确认事件”的目标可以是 RPO=0；如果采用 batch fsync 或 interval fsync，那么 crash 可能丢失最近一个 batch/window 内尚未落盘的数据，这个窗口就必须明确写进 RPO 讨论。

面试回答：

```text
MTTR 是实际平均恢复时间，MTBF 是实际平均故障间隔，它们是观测统计；RTO 是业务可接受的最大恢复时间，RPO 是业务可接受的最大数据丢失窗口，它们是设计目标。故障测试里要用实验数据验证 MTTR 是否低于 RTO，用持久化和恢复结果验证实际数据丢失是否不超过 RPO。对 LogServe，如果 shared log 事件在 ack 前已经 fsync，那么已确认事件的 RPO 目标可以接近 0；如果用 batch fsync，就必须承认可能丢一个 batch 窗口内的数据。不能把测出来的平均恢复时间偷换成 RTO。
```

## Q030. 故障恢复是否应该优先保证一致性还是可用性？

不能脱离系统语义回答“一定优先一致性”或“一定优先可用性”。正确回答是：先按操作类型和数据重要性分层；对持久状态和不可逆副作用，优先保证一致性和安全；对缓存、派生视图、只读降级、可重试请求，可以更偏向可用性。

CAP 的现实含义是：发生网络分区时，如果某个操作需要跨分区保持一致，就不可能同时保证强一致和完全可用。Jepsen 的 linearizable model 页面也明确指出，linearizability 不能在分区下保持 total/sticky availability；有些节点必须停止进展，否则就可能返回无法解释的结果。

所以恢复策略应该按风险分层：

| 层级 | 优先级 | 例子 |
|---|---|---|
| 核心持久状态 | 一致性优先 | shared log、事务提交、账务、actor command_seq |
| 互斥控制权 | 一致性优先 | lease、leader、worker ownership epoch |
| 外部不可逆副作用 | 一致性/幂等优先 | 扣款、发消息、写外部 sink |
| 可重试写请求 | 一致性优先，必要时返回重试 | submit/complete 不确定时返回 info/timeout |
| 只读查询 | 可根据语义降级 | 允许 stale read 时必须标注版本或 staleness |
| 缓存/派生视图 | 可用性优先 | checkpoint cache、dashboard snapshot |
| 推荐/调度优化 | 可用性优先 | model cache locality、负载均衡 hint |

一致性优先不等于永远不可用。它的意思是：当系统不知道一个状态变更是否安全时，宁可拒绝或等待，也不要返回一个会破坏 invariant 的成功。例如：

```text
不能确认 task completion 是否已经 canonical -> 不接受第二个完成覆盖第一个
不能确认 actor owner epoch 是否最新 -> 拒绝旧 epoch worker 写入
不能 append shared log -> 不更新 materialized view
不能判断写是否落盘 -> 不向客户端承诺成功
```

可用性优先也不等于乱返回。它通常要配合降级语义：

```text
返回 cached result，但标注 version/staleness
dashboard 显示 last updated time
缓存 miss 时重新计算，而不是返回错误结果
调度器失去 cache locality 信息时退化为普通负载均衡
只读接口允许 eventual consistency 时，文档明确说明
```

对 LogServe，因为设计上 shared log 是 source of truth，control 的 materialized metadata view 是可重建视图，所以核心原则应该是：

```text
先写 shared log，再更新内存视图。
恢复后以内存视图为准是错误的，以 shared log replay 为准才是安全的。
如果不能 append log，就不要承诺 task/workflow/actor 状态已经改变。
如果旧 worker 带着旧 epoch 回来提交 completion，宁可拒绝也不能覆盖新 owner。
checkpoint cache、dashboard snapshot、model locality hint 可以牺牲新鲜度换可用性，但必须不改变业务事实。
```

在面试里可以把答案说得更工程化：一致性和可用性不是全局二选一，而是每条 API 的契约。比如：

```text
submit workflow: 成功返回后 workflow 不能丢，所以一致性优先
complete task: 不能重复生效，所以一致性优先
query dashboard: 可以短暂展示旧数据，但要标注时间
checkpoint cache: 可以丢，最多影响性能
worker poll: 可以超时重试，不能让两个 worker 同时拥有不可隔离的同一 attempt
```

面试回答：

```text
我不会全局说一定优先一致性或可用性，而是按操作分层。对持久状态、互斥控制权和不可逆副作用，恢复时优先一致性；不确定就拒绝、等待或返回可重试错误，不能为了可用性返回破坏 invariant 的成功。对缓存、dashboard、调度 hint、只读派生视图，可以优先可用性，但要标注 staleness，不能把旧视图伪装成最新事实。LogServe 里 shared log 是事实源，所以 task/workflow/actor 状态变更必须先写 log；control 重启后从 log replay；旧 epoch worker 的 completion 必须被 fence 掉。checkpoint cache 和 dashboard snapshot 则可以降级，因为它们不应该改变系统事实。
```
## Q031. partial failure 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

partial failure 的核心目标是让系统在“一部分组件失败、另一部分组件仍在运行”的情况下，仍然能给出可解释、可恢复、不会放大故障的行为。它不是单纯追求不报错。更准确地说，它要求系统不要因为局部超时、局部不可达、局部慢、局部状态落后，就把全局状态带进不可恢复的混乱。

这里最主要解决的是可靠性和正确性问题。性能、安全性、可维护性都会受到影响，但不是第一目标。

| 维度 | partial failure 关心什么 | 典型问题 |
|---|---|---|
| 正确性 | 故障期间的成功、失败、超时是否仍能解释 | 已确认写丢失、重复提交、旧 owner 写入 |
| 可靠性 | 局部故障是否会被隔离、恢复、降级 | 一个 worker 掉线后任务是否 redeliver |
| 性能 | 超时、重试、排队是否被控制 | retry storm、线程池耗尽、p99 被拖长 |
| 安全性 | 故障期间是否绕过鉴权、隔离或租户边界 | fallback 错用默认权限 |
| 可维护性 | 故障模式是否能被观测和复现 | 日志只有 timeout，没有 operation history |

最容易答错的地方，是把 partial failure 理解成“只要服务还能返回，就算处理好了”。这不够。一个系统在分区期间仍然返回 200，但返回的是旧数据、重复执行外部副作用、接受旧 epoch worker 的写入，这种可用性反而会破坏正确性。

AWS Builders Library 在讲 timeout、retry、backoff 时把 partial failure 定义得很直接：一部分请求成功，一部分请求失败。这个定义很实用，因为它把问题落在客户端可观察行为上：同一个依赖、同一段时间、同一类请求，有些成功，有些失败，有些慢到超时。分布式系统不能假设“依赖要么全好，要么全坏”。

对 LogServe 来说，partial failure 的目标可以写成几条具体要求：

```text
worker 看不到 control，不代表 worker 的本地执行一定没发生。
control 看不到 worker，不代表 worker 一定已经停止。
client 超时，不代表请求没有进入 shared log。
completion 迟到，不代表它还能覆盖新的 canonical completion。
control 内存状态丢失，不代表 shared log 里的事实丢失。
```

所以 LogServe 的核心处理方式是：用 shared log 作为事实源，用 idempotency key、attempt、epoch、command_seq 和 replay 来约束恢复行为。这里的重点是正确性。性能优化，比如 backoff、jitter、限流，是为了避免 partial failure 被重试和排队放大成全局故障。

面试回答：

```text
partial failure 的目标是让系统在局部失败下仍然保持可解释和可恢复。它主要解决可靠性和正确性问题：超时后请求到底有没有生效、重复请求会不会造成副作用、旧 owner 会不会继续写状态、恢复后视图是否能从事实源重建。性能问题也很重要，因为重试和排队会把局部故障放大；安全性和可维护性是边界条件，比如 fallback 不能绕过权限，日志要能复现故障。对 LogServe，我会把 shared log 作为事实源，把 worker redelivery、actor epoch fencing、step idempotency 和 control replay 都看成 partial failure 下的正确性保护。
```

## Q032. partial failure 的典型适用场景和不适用场景分别是什么？

partial failure 不是一个具体算法，更像一类设计问题。只要系统跨进程、跨机器、跨网络、跨存储、跨异步队列，就应该按 partial failure 来设计。相反，如果所有状态都在同一个进程、同一个地址空间、同一个事务边界里，很多 partial failure 的复杂处理会显得多余。

典型适用场景有这些：

| 场景 | 为什么适用 |
|---|---|
| 微服务调用 | 网络、下游服务、负载均衡、连接池都可能局部失败 |
| 分布式任务队列 | worker 可能执行了任务但 ack 丢失 |
| workflow runtime | 某个 step 失败或慢，不代表整个 workflow 状态不可恢复 |
| actor runtime | owner 失联、旧 owner 迟到写入、mailbox 重放都属于局部失败 |
| 分布式存储 | 某个 replica 不可达或落后，其他 replica 仍然可服务 |
| LLM serving | 某个模型副本冷启动、某台 GPU 慢、某个 cache miss，不代表全局不可用 |
| 控制面和数据面分离 | 数据面可能继续处理，控制面短暂不可达 |
| 多 AZ 或多 region | 一个区域失败，其他区域仍然运行 |

不适用或收益较低的场景也要说清楚：

| 场景 | 为什么不适合过度套用 |
|---|---|
| 单进程纯内存算法 | 没有远程不确定性，重点是并发正确性和内存安全 |
| 同一个本地事务内的操作 | 数据库已经提供原子提交，业务层重复实现会增加复杂度 |
| 明确不可重试的外部副作用 | 比如没有幂等键的扣款或发货，不能靠简单 retry 处理 |
| 强实时控制环 | 超时后重试可能已经过了物理控制窗口，应 fail fast 或进入安全状态 |
| 批处理离线任务 | 可以重跑整个批次时，不一定需要细粒度 partial recovery |
| 小型脚本或一次性工具 | 复杂的 fencing、circuit breaker、重放机制可能超过收益 |

适用场景的共同点是“观察者看到的事实不一致”。例如客户端看到 timeout，服务端可能已经执行；control 认为 worker dead，worker 可能只是网络断开；reader 看到旧状态，writer 可能已经提交但复制还没追上。只要存在这种不确定性，就要设计 partial failure 语义。

Azure Retry pattern 的适用边界也可以作为参考：短暂的网络、服务繁忙、超时适合 retry；长期故障、业务逻辑错误、容量不足，不应该用 retry 掩盖。这个边界和 partial failure 一样：它处理的是局部、短暂、不确定的故障，不是替代容量规划、业务异常处理和数据一致性协议。

对 LogServe，适用场景很明确：

```text
worker crash 后 task redelivery
worker completion ack 丢失后重复提交
control restart 后从 shared log 重建 view
actor owner 失联后 epoch fencing
LLM worker cache miss 或模型副本不可用
logd 短暂不可达导致 append timeout
```

不适合的场景也要承认：当前 LogServe 是单机机制验证，不应该声称已经解决多 region 网络分区、多副本共识、跨机 shared log quorum、真实 GPU 集群故障隔离。那些属于下一阶段多节点实验。

面试回答：

```text
partial failure 适合跨进程、跨网络、跨存储和异步队列的系统，典型场景是微服务调用、任务队列、workflow、actor、分布式存储、控制面和数据面分离。它不适合拿来包装所有错误：单进程纯内存算法、数据库本地事务、不可重试的外部副作用、强实时控制和容量不足问题，都不能只靠 partial failure 处理。LogServe 里它适用于 worker redelivery、control replay、actor epoch fencing、重复 completion 和 LLM worker 局部不可用；但我不会把当前单机实验说成已经覆盖多 region 或 quorum 级别的 partial failure。
```

## Q033. partial failure 和相近概念最容易混淆的边界在哪里？

partial failure 经常和 crash、transient failure、timeout、overload、network partition、eventual consistency、graceful degradation 混在一起。它们有关联，但边界不同。

| 概念 | 关注点 | 和 partial failure 的边界 |
|---|---|---|
| crash | 进程或机器停止运行 | crash 是一种明确失败；partial failure 可能是局部 crash，也可能只是不可达或慢 |
| hang | 组件还活着但不响应 | hang 更难，因为 health check 可能误判；partial failure 要处理这种不确定性 |
| transient failure | 短暂失败后恢复 | transient 强调时间短；partial 强调只影响一部分请求或组件 |
| timeout | 调用方等待超过期限 | timeout 是观察结果，不等于服务端没执行 |
| overload | 资源接近或超过容量 | overload 会制造 partial failure，但根因是容量或排队 |
| network partition | 网络把节点分成互不可见的集合 | partition 是典型 partial failure，尤其会引出一致性和可用性取舍 |
| eventual consistency | 状态最终收敛 | 它是一种一致性模型，不是故障本身 |
| graceful degradation | 降级提供部分功能 | 它是应对策略，不是故障类型 |
| circuit breaker | 达阈值后快速失败 | 它是保护机制，用来防止局部故障扩散 |

最危险的混淆是把 timeout 当成 failure。调用方超时只能说明“我没在期限内收到结果”。它不能证明服务端没有收到请求，也不能证明请求没有产生副作用。gRPC deadline 文档也强调，客户端 deadline 过期后会失败，但服务端应用仍然需要主动检查 cancellation 并停止自己派生出来的工作。否则客户端已经放弃，服务端还在消耗资源，甚至继续写状态。

另一个常见混淆是把 partial failure 和 eventual consistency 混成一件事。eventual consistency 允许读到旧状态，只要最终收敛；partial failure 是故障模型，描述系统局部失败时的行为。一个强一致系统也会遇到 partial failure，只是它在分区或不确定状态下可能选择拒绝请求。一个 eventual consistency 系统如果没有幂等、版本和冲突处理，也会在 partial failure 下产生丢写和乱序覆盖。

还要区分 fault、error、failure。工程上可以这样理解：fault 是根因，比如网络丢包；error 是内部状态已经偏离，比如 retry 队列堆积；failure 是用户可见行为不符合契约，比如请求超时或读到错误状态。partial failure 经常从小 fault 开始，经过错误处理不当，最后变成全局 failure。

对 LogServe，边界要这样说：

```text
worker crash 是故障事件。
control 看不到 worker 是局部观察。
task timeout 是调用方结果。
redelivery 是恢复策略。
重复 completion 是需要处理的边界条件。
shared log replay 后状态一致，是正确性判定。
```

把这些词分清楚，才能设计测试。否则很容易写出一个“kill worker 后 workflow completed”的测试，却漏掉更关键的问题：第一次 worker 是否已经产生外部副作用？迟到 completion 是否被拒绝？control 的内存 view 是否和 replay view 一致？

面试回答：

```text
partial failure 容易和 timeout、transient failure、crash、partition、eventual consistency 混淆。timeout 只是调用方没等到结果，不代表服务端没执行；transient failure 强调持续时间短，partial failure 强调只影响一部分组件或请求；partition 是 partial failure 的一种；eventual consistency 是一致性模型，不是故障类型。LogServe 里 worker crash、completion timeout、redelivery、actor epoch fencing、shared log replay 是不同层面的概念，不能混成一句“失败后重试即可”。
```

## Q034. partial failure 在高并发场景下可能出现哪些隐藏问题？

高并发会把 partial failure 从“小范围异常”放大成系统级事故。单个请求超时并不可怕，几千个请求同时超时、同时重试、同时占住连接和线程，就会把下游拖进更深的 overload。

最常见的隐藏问题是 retry amplification。AWS Builders Library 里有一个典型例子：调用链有 5 层，每层都重试 3 次，底层数据库故障时，请求量可能被放大到 243 倍。这个数字不重要，重要的是结构：每层都觉得自己只是在“提高成功率”，合在一起就是重试风暴。

高并发下还会出现这些问题：

| 隐藏问题 | 具体表现 |
|---|---|
| retry storm | 大量客户端在同一时间重试，下游恢复不了 |
| timeout herd | timeout 配置相同，请求成批失败又成批重试 |
| connection pool exhaustion | 慢请求占住连接，健康请求也拿不到连接 |
| thread/goroutine pile-up | 等待远程结果的执行单元越积越多 |
| queue head-of-line blocking | 慢任务排在前面，后面的健康任务也被拖住 |
| lock contention | failure detector、breaker、metrics 热点锁成为瓶颈 |
| thundering herd recovery | 依赖恢复瞬间，所有等待请求同时冲进去 |
| duplicate side effect | 超时后 retry，原请求和重试请求都执行成功 |
| stale owner race | 旧 owner 网络恢复后和新 owner 同时写 |
| correlated timers | 心跳、扫描、重试、定时任务在同一秒触发 |

这也是为什么 backoff 还不够，通常还要加 jitter。AWS 的实践经验里，jitter 不只用于 retry，也适用于周期任务和延迟任务，因为同一批机器按同样周期运行会制造尖峰。Azure Retry pattern 也提醒，激进 retry 会降低吞吐，甚至让繁忙服务更忙。

高并发场景下，partial failure 的测试不能只看平均值。平均延迟可能没变，但 p99、p999、queue depth、in-flight requests、retry count、connection wait time 已经爆了。更麻烦的是，很多指标被聚合后看不出来。例如 1 分钟粒度的平均 QPS 很平稳，秒级视角可能有一波尖峰把服务打穿。

对 LogServe，高并发下要特别看这些点：

```text
大量 worker 同时 heartbeat timeout 后，control 是否一次性 redeliver 太多任务？
同一 task 的多个 attempt completion 同时到达时，canonical completion 是否唯一？
actor mailbox 高并发下 command_seq 是否仍然单调？
旧 epoch worker 恢复后是否会和新 owner 竞争？
control rebuild 时是否拿全局锁阻塞 submit/query？
logd fsync slow 时 append backlog 是否拖垮所有 stream？
LLM cache miss 是否让所有请求同时冷启动同一模型？
```

这里的性能瓶颈和正确性会交织在一起。比如 completion 去重需要锁或事务，锁粒度太粗会压低吞吐；锁粒度太细又容易放过重复完成。面试时不要把它讲成纯性能问题。

面试回答：

```text
高并发会放大 partial failure。常见隐藏问题包括 retry storm、timeout herd、连接池耗尽、goroutine 堆积、队头阻塞、锁竞争、恢复瞬间的 thundering herd、重复副作用和 stale owner race。观察指标不能只看平均延迟，要看 p99/p999、in-flight、queue depth、retry count、connection wait、redelivery burst。对 LogServe，我会重点测大量 worker timeout 后的 redelivery 限速、同一 task 多 attempt completion 的唯一性、actor command_seq 单调性、旧 epoch fencing，以及 logd fsync slow 时是否把所有 stream 都拖住。
```

## Q035. partial failure 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

partial failure 最难的部分通常出现在“状态刚改变一点点”的窗口。崩溃、重启、超时、重试都会把这些窗口暴露出来。

崩溃场景要问：崩溃发生在操作的哪个点？

```text
请求到达前 crash
请求到达后、持久化前 crash
持久化后、ack 前 crash
执行外部副作用后、记录完成前 crash
记录完成后、响应客户端前 crash
更新内存状态后、写日志前 crash
写日志后、更新内存状态前 crash
```

每个点的恢复语义不同。尤其是“持久化后、ack 前 crash”：客户端看到失败或超时，但系统恢复后应该承认那次写已经发生。反过来，“更新内存状态后、写日志前 crash”在 log-first 系统里不应该被恢复出来，因为事实源没有记录这次改变。

重启场景要问：系统是否依赖崩溃前的内存状态？如果 control 重启后必须靠内存里的 map 才知道哪些任务 started，那恢复就不可靠。正确做法是从持久事实源重建，比如 shared log、WAL、数据库事务日志、快照加 tail log。Kubernetes 的 CrashLoopBackOff 也说明了另一个边界：平台会用指数退避避免反复重启压垮系统，但这只解决重启节奏，不解决应用状态正确性。

超时场景要问：超时是谁观察到的？

```text
client deadline exceeded
control call worker timeout
worker append completion timeout
logd fsync timeout
object store put timeout
health check timeout
```

同样是 timeout，语义完全不同。gRPC deadline 文档强调，客户端不应无限等待；服务端收到 cancellation 后也要停止自己派生出来的工作。否则 timeout 只是把问题从客户端转移到服务端，资源还在消耗。

重试场景要问：操作是否幂等？有没有幂等键？重试发生在哪一层？

```text
只读查询通常可以 retry。
带 side effect 的写入必须有 idempotency key。
外部副作用要有去重键或事务 outbox。
多层 retry 会放大流量。
重试不能覆盖业务逻辑错误。
```

gRPC retry 文档里有一个细节很重要：一旦 response header 被收到，RPC 就被视为 committed，不再重试。这个边界说明 retry 不是随便重放请求。系统必须知道请求在哪个阶段失败，才能判断是否安全重试。

对 LogServe，可以把边界条件写成测试表：

| 场景 | 必测边界 |
|---|---|
| worker crash | 执行前、执行后完成前、完成写入后 ack 前 |
| control restart | replay 到一半、rebuild view 前后、存在 pending task |
| append timeout | logd 实际写入但 client 没收到 ack |
| completion retry | attempt 1 和 attempt 2 同时返回 |
| actor owner failover | old epoch completion 迟到 |
| result store timeout | result 已写但 result_ref 没写入 log，或反过来 |

面试回答：

```text
partial failure 的边界条件集中在 crash、restart、timeout、retry 的交界处。崩溃要看发生在持久化前还是持久化后，ack 前还是 ack 后；重启要验证状态是否能从持久事实源重建，而不是依赖内存；超时只能说明调用方没等到结果，不能说明服务端没执行；重试必须区分只读和有副作用操作，并依赖幂等键。LogServe 里我会重点测 TaskCompleted 写入前后 crash、control replay 中途重启、append timeout 后日志是否实际存在、重复 completion 和旧 epoch actor completion。
```

## Q036. partial failure 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

partial failure 的性能瓶颈通常不是单点来源。正常状态下系统可能受 CPU 或 I/O 限制；一旦进入 partial failure，瓶颈经常转移到网络等待、连接池、锁竞争、队列和持久化路径。最常见的模式是：网络或下游变慢触发 timeout，timeout 触发 retry，retry 增加队列和锁竞争，最后 CPU 也被调度、序列化、日志和监控打满。

可以按阶段看：

| 阶段 | 常见瓶颈 | 为什么出现 |
|---|---|---|
| 故障刚出现 | 网络、下游 I/O | 请求变慢或不可达 |
| 客户端等待 | 内存、连接池、线程/goroutine | in-flight 请求堆积 |
| 重试开始 | 网络、CPU、下游容量 | 重试流量放大 |
| 保护机制启动 | 锁竞争、原子计数、metrics 热点 | breaker、limiter、failure detector 都在更新共享状态 |
| 恢复阶段 | I/O、队列、锁 | redelivery、replay、补偿任务集中发生 |
| 事后分析 | 日志/trace I/O | 高基数观测数据集中写入 |

网络是 partial failure 的常见触发点，但不是唯一瓶颈。比如同一个进程内调用 logd 时，问题可能来自 fsync slow；actor 恢复时瓶颈可能来自 snapshot 读取和 tail replay；control 在高并发 completion 下可能卡在 per-task 锁或全局 metadata lock；LLM serving 可能卡在 checkpoint 读取、模型加载和 GPU 队列。

锁竞争很容易被低估。为了处理 partial failure，我们会加入幂等表、attempt 表、lease/epoch、circuit breaker 状态、retry token bucket、metrics counter。每个机制都需要共享状态。如果实现粗糙，这些保护机制本身会成为热点。

I/O 也很关键。partial failure 下，系统更依赖持久化事实源：WAL、shared log、快照、result store、operation history。正常路径可能只写一条日志；恢复路径可能要读大量历史、校验 tail、重放状态、写补偿事件。I/O 性能决定了 RTO。

对 LogServe，我会这样判断瓶颈：

| 组件 | partial failure 下可能的瓶颈 |
|---|---|
| logd | append fsync、segment recovery、tail read、per-stream scan |
| control | metadata lock、idempotency lookup、redelivery scan、replay rebuild |
| worker | executor pool、completion retry、result store write、heartbeat contention |
| actor runtime | mailbox 串行化、command_seq 检查、snapshot fetch、epoch fencing |
| LLM serving | model cache miss、checkpoint fetch、cold start、worker cache stats 更新 |
| dashboard | 高并发查询 materialized view，影响控制面热路径 |

benchmark 要把这些维度拆开。只跑正常吞吐没有意义，还要跑故障状态下的吞吐、p99、queue depth、redelivery rate、replay time、lock profile、fsync latency、连接等待时间。

面试回答：

```text
partial failure 的瓶颈经常是链式变化：网络或下游 I/O 先变慢，in-flight 请求堆积占用内存、线程和连接池，然后 retry 放大流量，保护机制带来锁竞争，恢复阶段又集中打到日志、快照和队列。不能只说它是 CPU 或网络问题。LogServe 里我会分别看 logd fsync 和 replay I/O、control metadata lock 和 redelivery scan、worker executor pool、actor mailbox/epoch 检查、LLM checkpoint cache miss。测试时要结合 p99、queue depth、retry count、lock profile、fsync latency 和 replay time 判断瓶颈。
```

## Q037. partial failure 的 correctness test、stress test 和 benchmark 应该分别测什么？

这三类测试经常被混用。correctness test 关心“结果对不对”，stress test 关心“压力和故障叠加时会不会崩”，benchmark 关心“在明确条件下性能是多少”。它们可以复用场景，但判断标准不同。

correctness test 应该测不变量。它不需要很大规模，但必须覆盖关键边界：

```text
已确认写恢复后仍存在。
超时请求如果实际写入，replay 后能看到。
重复 completion 只有一个 canonical result。
旧 epoch worker 不能写入新 owner 状态。
actor command_seq 不倒退、不跳过已确认命令。
control 重启后 view 等于 replay(shared log)。
partial log tail 被截断或忽略，不产生半条事件。
retry 不产生重复外部副作用。
```

stress test 应该测系统在高并发、长时间、随机故障下是否暴露隐藏问题。它不只看最后成功，还要看资源是否被耗尽：

```text
高并发 submit + worker kill
大量 completion timeout + retry
fsync slow + queue buildup
control restart + clients continue submitting
actor hot key + owner failover
LLM cache miss storm
随机 delay/loss/partition + fixed seed
```

stress test 的通过条件应该包含资源指标：队列不无限增长、goroutine 不泄漏、连接池不耗尽、retry 不爆炸、breaker 不抖动、恢复后指标回落。它更像“找系统弱点”。失败不一定表示正确性错了，也可能表示恢复太慢或保护不足。

benchmark 应该测可解释的性能数字。它要固定环境、输入规模、故障类型、并发度、fsync 策略、worker 数、payload size，输出 p50/p95/p99、throughput、B/op、allocs/op、replay time、RTO 等。benchmark 不该混入太多随机故障，否则数字不可比较。

可以这样分：

| 测试类型 | 主要问题 | 输出 |
|---|---|---|
| correctness test | 状态是否正确 | pass/fail、最小反例、history |
| stress test | 高压下是否稳定 | 资源曲线、错误率、泄漏、是否恢复 |
| benchmark | 性能是多少 | throughput、latency、alloc、RTO、replay time |

对 LogServe，一组合理测试是：

| 类型 | LogServe 场景 |
|---|---|
| correctness | worker crash 后同一 task 只完成一次；actor old epoch 被拒绝；control replay view 一致 |
| stress | 1000 workflow 并发提交，随机 kill worker/control，检查 queue 和 retry 是否收敛 |
| benchmark | 不同 fsync 策略下 append throughput；control restart replay time；worker redelivery RTO |

还要注意数据收集。Jepsen 风格测试会记录 operation history，再用 checker 分析正确性；OpenTelemetry trace 适合定位跨组件延迟；pprof 或 Go trace 适合找锁和 CPU；日志适合保留 fault schedule。不同证据服务不同目的，不要拿 benchmark 数字证明 correctness。

面试回答：

```text
correctness test 测不变量，比如已确认写不丢、重复 completion 不重复生效、旧 epoch 被 fence、replay view 等于事实源；stress test 测高并发和随机故障叠加时资源是否耗尽、retry 是否爆炸、队列是否收敛；benchmark 测固定条件下的吞吐、p99、alloc、replay time 和 RTO。三者不能混用。对 LogServe，我会用 correctness test 保证 task/actor/shared log 语义，用 stress test 找 retry storm 和锁竞争，用 benchmark 比较 fsync 策略、redelivery RTO 和 control replay 成本。
```

## Q038. 如果要求从零实现一个简化版 partial failure，你会先定义哪些不变量？

从零实现时，不要先写 retry loop。先定义不变量。partial failure 的难点不是“失败后再试一次”，而是重试、超时、恢复、重复消息同时出现时，系统仍然不越界。

我会先定义这些基础不变量：

```text
1. 每个外部请求都有 request_id 或 idempotency_key。
2. 已确认成功的状态变更必须持久化到事实源。
3. 恢复后的内存 view 必须能由事实源 replay 得到。
4. 同一 logical operation 最多只有一个 canonical result。
5. retry 不能产生重复不可逆副作用。
6. lease/ownership 必须带 epoch，旧 epoch 写入必须被拒绝。
7. 每个 operation 都有 deadline，deadline 会向下游传播。
8. pending/unknown 状态不能被误当成失败或成功。
9. redelivery 有上限、退避和去重。
10. 故障检测可以误判，但误判不能破坏状态安全。
```

如果是一个简化任务系统，可以把状态机写清楚：

```text
TaskSubmitted -> TaskLeased(worker_id, epoch, attempt)
TaskLeased -> TaskCompleted(result_ref, attempt)
TaskLeased -> TaskExpired
TaskExpired -> TaskLeased(new_worker_id, new_epoch, new_attempt)
TaskCompleted 是终态，后续 completion 只能被忽略或记录为 duplicate。
```

再定义持久化顺序：

```text
先 append TaskSubmitted，再向客户端返回 submit 成功。
先 append TaskLeased，再让 worker 执行。
worker 写 result_ref 后，再 append TaskCompleted。
control 根据 log 更新内存 view。
重启时丢弃内存 view，从 log replay。
```

然后定义超时和重试语义：

```text
client timeout: result unknown，可以查询 request_id。
worker heartbeat timeout: lease suspect，不等于 worker 已停止。
task lease expired: 可以 redeliver，但旧 attempt completion 必须被 attempt/epoch 检查拦住。
append timeout: 必须查询 log 或用 idempotency key 重试，不能盲目写第二条逻辑事件。
```

最小实现可以包含这些组件：

| 组件 | 作用 |
|---|---|
| durable log | 事实源，支持 append/read/replay |
| idempotency table | request_id 到 result 的映射 |
| lease manager | worker ownership、epoch、TTL |
| retry policy | deadline、max attempts、backoff、jitter |
| recovery loop | 扫描 expired lease，redeliver |
| checker | 验证 no lost task、no duplicate completion |

对 LogServe 的简化版，可以先实现 task，不急着实现完整 workflow、actor、LLM：一个 shared log、两个 worker、一个 control、一个 client，就足以暴露 partial failure 的核心问题。等 task 语义稳定，再扩展到 actor mailbox 和 workflow DAG。

面试回答：

```text
我会先定义不变量，而不是先写 retry。最小不变量包括：请求有 idempotency key；成功返回前状态必须持久化；恢复 view 必须来自事实源 replay；同一 logical operation 只有一个 canonical result；旧 epoch 写入必须被拒绝；deadline 必须传播；unknown 不能被误判成失败；redelivery 要有上限和退避。简化实现上，我会先做一个 durable log、task lease、worker heartbeat、attempt/epoch、completion 去重和 replay checker。只要 task 在 crash、timeout、retry 下不丢不重，再扩展 workflow 和 actor。
```

## Q039. partial failure 的常见误用是什么，误用后通常会产生什么线上症状？

partial failure 的误用通常不是“完全没处理失败”，而是处理得太粗糙。系统看起来有 timeout、retry、health check、circuit breaker，但这些机制之间没有一致的语义，线上就会出现很怪的症状。

常见误用包括：

| 误用 | 线上症状 |
|---|---|
| 没有 deadline，只设置连接超时 | 请求长时间挂住，线程和连接池耗尽 |
| 每一层都 retry | 下游故障时流量倍增，恢复时间变长 |
| retry 无 idempotency key | 重复扣款、重复发消息、重复完成任务 |
| timeout 当成失败 | 实际已提交的操作被再次执行 |
| health check 过浅 | 进程活着但业务不可用，流量继续打进坏实例 |
| liveness/readiness 混用 | 本该摘流量的 Pod 被重启，或本该重启的进程继续接流量 |
| 单个全局 circuit breaker | 一个 shard 故障导致所有 shard 被熔断 |
| fallback 返回假成功 | 用户看到旧数据或默认数据，以为写入成功 |
| failure detector 结果当真理 | 误判节点死亡后双 owner 写入 |
| recovery 无限并发 | 恢复瞬间打爆 log、数据库或对象存储 |
| 只看平均值 | p99 和错误率已经恶化，dashboard 仍显示正常 |

Kubernetes 的 readiness 和 liveness 是一个很好的边界例子。readiness 失败表示暂时不要给这个 Pod 发流量；liveness 失败表示容器可能需要重启。把两者混用，会带来线上抖动：依赖暂时不可用时，本来只需要摘流量，结果 liveness 把进程杀掉；启动慢时探针太激进，Pod 进入 CrashLoopBackOff。

Azure Circuit Breaker 文档也提醒了一个常见误用：一个 breaker 保护多个独立资源时，某个 shard 出问题可能把其他健康 shard 也挡掉。partial failure 要尊重故障的局部性。把局部错误聚合成全局错误，会降低可用性；把全局风险误当成局部错误，又会放大故障。

线上症状通常有这些：

```text
错误率不高，但 p99/p999 突然变差。
下游已经恢复，上游还在重试导致流量居高不下。
同一用户请求出现两次结果。
任务状态在 RUNNING、PENDING、SUCCEEDED 之间来回跳。
某个 shard 故障，所有 shard 流量都被熔断。
worker 已经被判 dead，但之后又写入 completion。
control 重启后 dashboard 状态和实际日志对不上。
看日志只有 timeout，看不出请求是否真正执行。
```

对 LogServe，最需要避免的误用是：把 worker heartbeat timeout 当成 worker 已停止。正确做法是把它当成 suspect，然后通过 lease/epoch/attempt 保护后续写入。另一个误用是 completion retry 没有幂等和 canonical result，导致同一 task 被两个 worker 都提交成功。

面试回答：

```text
partial failure 的常见误用包括无 deadline、每层都 retry、没有 idempotency key、把 timeout 当成服务端失败、health check 过浅、readiness/liveness 混用、全局 circuit breaker 保护多个独立 shard、fallback 返回假成功。线上症状通常是 p99 恶化、retry storm、连接池耗尽、重复副作用、状态来回跳、局部故障变全局不可用。LogServe 里我会特别避免把 heartbeat timeout 当成 worker 已停止，必须用 epoch fencing 和 attempt 去约束迟到 completion。
```

## Q040. partial failure 在单机和分布式环境中的语义有什么差异？

单机环境也有 partial failure，但语义和分布式环境不一样。单机里常见的是进程、线程、磁盘、文件、锁、IPC、子进程局部失败；分布式环境多了网络不确定性、时钟偏差、分区、复制延迟、多副本一致性和跨节点观察差异。

单机 partial failure 的例子：

```text
worker 子进程 crash，control 进程还活着。
logd fsync slow，worker 还能执行但 completion 写不进去。
某个线程死锁，健康检查线程仍然返回 ok。
文件写了一半，进程 crash。
本地 result store 满了，但 control 内存队列还在接任务。
```

分布式 partial failure 的例子：

```text
节点 A 能访问节点 B，节点 B 访问不了节点 A。
客户端到 leader 超时，但 leader 已经提交了写。
一个 replica 落后，另一个 replica 已经有新值。
网络分区后两个节点都以为自己是 owner。
不同节点时钟不同，lease 判断不一致。
跨 region 延迟抖动导致同一超时策略误杀远端请求。
```

单机环境的优势是：故障边界更少，通常有本地文件系统、本地进程树、本机 monotonic clock，复现和观测容易一些。很多行为可以通过进程退出码、文件锁、WAL、本地端口、临时目录来控制。LogServe 当前单机多进程实验就属于这一类：它能验证 shared log、replay、worker redelivery、actor fencing 的机制，但不覆盖真实多机网络分区和副本共识。

分布式环境的难点是观察者不同。一个节点看到 timeout，另一个节点可能已经提交；一个节点认为 lease 过期，另一个节点的本地时钟认为还没过期；一个 region 认为依赖不可用，另一个 region 正常。这里不能靠本机锁或本机时间解决问题，需要 quorum、租约协议、fencing token、版本向量、单调日志、幂等键、冲突解决或共识协议。

语义差异可以总结成：

| 维度 | 单机 | 分布式 |
|---|---|---|
| 失败检测 | 进程退出、IPC 断开、本地探针 | timeout、heartbeat、gossip、探针都可能误判 |
| 时间 | 可用 monotonic clock 统一判断本机耗时 | 跨节点时钟不能当严格顺序 |
| 状态源 | 本地 WAL/shared log/文件 | 多副本日志、数据库、对象存储、quorum |
| 隔离 | 进程、线程、文件、端口 | 节点、AZ、region、shard、tenant |
| 恢复 | 重启进程、replay 本地日志 | leader election、replica catch-up、failover、冲突解决 |
| 正确性风险 | partial write、锁、重复执行 | split brain、stale read、lost ack、双 owner |

对 LogServe 的面试边界要讲清楚：当前项目在单机 Ubuntu、3 worker、mock LLM、file-backed checkpoint cache 下验证机制。它可以证明“log-first、replay、redelivery、actor epoch fencing 这些机制在单机多进程下跑通”；它不能证明“跨机器网络分区、多副本 shared log、跨 region failover 都已解决”。如果继续扩展，下一步应该引入多节点部署、真实网络注入、持久化 PostgreSQL/MinIO、log quorum 或外部一致性存储。

面试回答：

```text
单机也会有 partial failure，比如 worker 子进程 crash、logd fsync slow、文件 partial write、result store 满、某个线程 hang。但单机通常有统一的本机时钟、本地日志和进程边界，恢复可以靠本地 WAL、进程重启和 replay。分布式环境多了网络分区、时钟偏差、复制延迟、双 owner、stale read 和 lost ack，timeout 只能说明观察者没收到结果，不能说明远端没执行。LogServe 当前验证的是单机多进程 partial failure 机制，不等价于多机共识或跨 region 容灾；这个边界要主动说清楚。
```
## 参考资料
- AWS Builders Library, [Timeouts, retries, and backoff with jitter](https://aws.amazon.com/builders-library/timeouts-retries-and-backoff-with-jitter/)
- Microsoft Azure Architecture Center, [Retry pattern](https://learn.microsoft.com/en-us/azure/architecture/patterns/retry)
- Microsoft Azure Architecture Center, [Circuit Breaker pattern](https://learn.microsoft.com/en-us/azure/architecture/patterns/circuit-breaker)
- Microsoft Azure Architecture Center, [Bulkhead pattern](https://learn.microsoft.com/en-us/azure/architecture/patterns/bulkhead)
- gRPC, [Deadlines](https://grpc.io/docs/guides/deadlines/)
- gRPC, [Retry](https://grpc.io/docs/guides/retry/)
- Kubernetes, [Configure Liveness, Readiness and Startup Probes](https://kubernetes.io/docs/tasks/configure-pod-container/configure-liveness-readiness-startup-probes/)
- Jepsen, [Jepsen consistency models](https://jepsen.io/consistency)
- Jepsen, [Linearizability](https://jepsen.io/consistency/models/linearizable)
- GitHub, [jepsen-io/jepsen](https://github.com/jepsen-io/jepsen)
- GitHub, [jepsen-io/knossos](https://github.com/jepsen-io/knossos)
- GitHub, [jepsen-io/elle](https://github.com/jepsen-io/elle)
- Maurice P. Herlihy and Jeannette M. Wing, [Linearizability: A Correctness Condition for Concurrent Objects](https://cs.brown.edu/~mph/HerlihyW90/p463-herlihy.pdf)
- OpenTelemetry, [Traces](https://opentelemetry.io/docs/concepts/signals/traces/)
- W3C, [Trace Context](https://www.w3.org/TR/trace-context/)
- Google SRE, [Monitoring Distributed Systems](https://sre.google/sre-book/monitoring-distributed-systems/)
- AWS Architecture Blog, [Disaster Recovery Architecture on AWS, Part I: Strategies for Recovery in the Cloud](https://aws.amazon.com/blogs/architecture/disaster-recovery-dr-architecture-on-aws-part-i-strategies-for-recovery-in-the-cloud/)
- Linux man-pages, [write(2)](https://man7.org/linux/man-pages/man2/write.2.html)
- Linux man-pages, [rename(2)](https://man7.org/linux/man-pages/man2/rename.2.html)
- AWS Fault Injection Service, [Stop conditions](https://docs.aws.amazon.com/fis/latest/userguide/stop-conditions.html)
- AWS Fault Injection Service, [Experiment template components](https://docs.aws.amazon.com/fis/latest/userguide/experiment-templates.html)
- AWS Well-Architected Reliability Pillar, [Test reliability](https://docs.aws.amazon.com/wellarchitected/latest/reliability-pillar/test-reliability.html)
- Kubernetes Docs, [Deployments](https://kubernetes.io/docs/concepts/workloads/controllers/deployment/)
- Linux Kernel Docs, [dm-delay](https://docs.kernel.org/admin-guide/device-mapper/delay.html)
- Linux Kernel Docs, [dm-flakey](https://docs.kernel.org/admin-guide/device-mapper/dm-flakey.html)
- Linux man-pages, [tc-netem(8)](https://man7.org/linux/man-pages/man8/tc-netem.8.html)
- Linux man-pages, [clock_gettime(2) / clock_settime(2)](https://man7.org/linux/man-pages/man2/clock_gettime.2.html)
- Chaos Mesh, [Simulate File I/O Faults](https://chaos-mesh.org/docs/simulate-io-chaos-on-kubernetes/)
- Chaos Mesh, [Simulate Block Device Incidents](https://chaos-mesh.org/docs/simulate-block-chaos-on-kubernetes/)
- Chaos Mesh, [Simulate Time Faults](https://chaos-mesh.org/docs/simulate-time-chaos-on-kubernetes/)
- Chaos Mesh, [Simulate JVM Application Faults](https://chaos-mesh.org/docs/simulate-jvm-application-chaos/)
- Go Packages, [runtime.GC](https://pkg.go.dev/runtime#GC)
- Go Packages, [runtime/debug](https://pkg.go.dev/runtime/debug)
- Go Documentation, [A Guide to the Go Garbage Collector](https://go.dev/doc/gc-guide)
- Go Packages, [time: monotonic clocks](https://pkg.go.dev/time)
- Linux Kernel Docs, [Fault injection capabilities infrastructure](https://docs.kernel.org/fault-injection/fault-injection.html)
- Principles of Chaos Engineering, [Principles of Chaos Engineering](https://principlesofchaos.org/)
- AWS Documentation, [What is AWS Fault Injection Service?](https://docs.aws.amazon.com/fis/latest/userguide/what-is.html)
- Chaos Mesh, [Chaos Mesh Overview](https://chaos-mesh.org/docs/)
- Chaos Mesh, [Simulate Pod Faults](https://chaos-mesh.org/docs/simulate-pod-chaos-on-kubernetes/)
- Chaos Mesh, [Simulate Network Faults](https://chaos-mesh.org/docs/simulate-network-chaos-on-kubernetes/)
- Kubernetes Docs, [Pod Lifecycle](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/)
- Kubernetes Docs, [Configure Liveness, Readiness and Startup Probes](https://kubernetes.io/docs/tasks/configure-pod-container/configure-liveness-readiness-startup-probes/)
- Kubernetes Docs, [Nodes](https://kubernetes.io/docs/concepts/architecture/nodes/)
- Linux man-pages, [kill(2)](https://man7.org/linux/man-pages/man2/kill.2.html)
- Linux man-pages, [signal(7)](https://man7.org/linux/man-pages/man7/signal.7.html)
- Linux man-pages, [fsync(2)](https://man7.org/linux/man-pages/man2/fsync.2.html)
- Google SRE Book, [Testing for Reliability](https://sre.google/sre-book/testing-reliability/)
- Google SRE Book, [Addressing Cascading Failures](https://sre.google/sre-book/addressing-cascading-failures/)
