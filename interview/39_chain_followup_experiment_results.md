# 十二、高频链式追问脚本：从实验结果开始

这一组问题最容易被追问“你是不是只跑了一个 demo”。回答时不要把单机结果包装成生产性能。我的口径是：这些实验验证的是机制链路，包括 log-first 恢复、actor snapshot、LLM cache locality、checkpoint cache、故障恢复和实验自动化；绝对性能数字只代表这台实验机器和这组参数。

## Q876. 你的实验是在什么环境做的？

我的实验在单机实验环境上跑，机器信息是 Linux lab2439，内核为 6.8.0-111-generic，Ubuntu 22.04 系列，x86_64 架构。实验是在同一台机器上启动 logd、control 和 3 个 worker 进程，所以它模拟的是“单机多进程分布式 runtime”，不是多物理节点部署。

这点我会主动说明清楚。实验环境里没有真实 GPU，也没有跨机网络。LLM serving 走 mock LLM 和本地 checkpoint cache，checkpoint source 也是本地目录。这组实验不承担真实 GPU 性能结论，只负责把调度策略、cache hit/miss、冷启动事件、checkpoint materialization、worker 上报模型缓存这些路径跑通，并且留下可复查的 JSON 结果。

实际跑过的内容包括：

- Go 全量测试、go vet、race test。
- Python unittest、compileall、gRPC 依赖检查。
- logstore benchmark，比较 always、batch、interval 三种 fsync 策略。
- fault injection 测试，包括 worker kill recovery、queue redelivery、control restart bootstrap。
- runtime 启动测试，启动 logd、control 和 3 个 worker。
- workflow、task throughput、actor snapshot、LLM locality、checkpoint cache 的 benchmark。
- dashboard snapshot 和实验 summary 生成。

一次完整实验的命令状态里，核心检查项 exit_code 都是 0。也就是说，它已经有脚本化的实验产物，不停留在 README 或手工截图上。产物包括 `command_status.jsonl`、`summary.json`、benchmark 结果 JSON、`checkpoint_cache_probe.json`、日志文件和 runtime 目录。

## Q877. 为什么单机实验不能代表生产性能？

因为生产性能受很多单机实验覆盖不到的因素影响。比如多机网络延迟、logd 多副本复制、control 高可用切换、真实对象存储延迟、真实 GPU 显存压力、vLLM continuous batching、Kubernetes Pod 迁移、磁盘抖动、长时间运行后的内存和文件句柄问题，这些在单机 mock 环境里都没有完全出现。

所以我不会说“系统生产环境 p95 就是 205ms”这种话。我的说法是：单机实验能说明机制方向对，比如 locality-aware 比 resource-only 更少冷启动，snapshot replay 比 full replay 少读命令，checkpoint cache 可以落盘并被 warm request 命中，log-first 路径能在 control 重启后恢复 view。它证明的是设计闭环和相对趋势，不代表生产部署的绝对吞吐。

要把它提升到生产性能结论，需要多节点实验、真实 GPU、真实对象存储、长时间压测、固定 workload 分布、多轮重复和置信区间。

## Q878. 哪些指标最能证明你的机制有效？

我会优先讲能对应机制的指标，而不是只挑吞吐数字。

第一类是恢复机制。fault injection 里 worker_kill_recovery、queue_redelivery、control_restart_probe 都通过，说明任务租约、redelivery、control 从 log bootstrap 的路径可用。actor recovery 里 snapshot replay commands 从 21 降到 1，说明 snapshot 真的改变了 replay 成本。

第二类是 LLM locality。实验中 resource-only 的 cache hit rate 是 0.833，locality-aware 和 predicted-latency 是 1.0；resource-only 的 p95 latency 是 305ms，locality-aware 和 predicted-latency 是 205ms；resource-only 出现 1 次 cold start，locality-aware 没有 cold start。这组指标直接对应“调度器是否把请求送到已有模型缓存的 worker”。

第三类是 checkpoint cache。checkpoint probe 里 cold request 是 cache_hit=false，warm request 是 cache_hit=true；cache_used_bytes 变成 3145728，cache_capacity_bytes 是 16777216；runtime/model-cache 下能看到 checkpoint 文件和 manifest。这个结果说明 cache 已经落到本地文件，而非单纯的一段内存状态。

第四类是 logstore。always fsync 的 append records/s 约 1685，batch 约 239518，interval 约 266441。这个结果说明 fsync 策略对 WAL 写入吞吐影响很大。它是 log 层 microbenchmark，不是端到端任务吞吐。

我更愿意用这些成对指标来解释项目：机制打开前后有什么变化，故障后能不能恢复，重复提交会不会污染最终状态。单个高吞吐数字反而没那么有说服力。

## Q879. locality-aware p95 降低是否显著？样本量够吗？

这组结果能说明策略方向，但不能严格说统计显著。

实验里 resource-only 的 p95 latency 是 305ms，locality-aware 是 205ms，同时 cache hit rate 从 0.833 提到 1.0，cold start 从 1 次降到 0 次。这个差距和机制是对得上的：resource-only 有概率把请求调度到没有模型缓存的 worker，locality-aware 会优先选有缓存的 worker，所以少了一次冷启动，p95 也随之下降。

但样本量只有 6 个 LLM 请求。这个规模更像功能性 ablation，不是严谨性能论文里的统计实验。我要是现场解释，会补一句：这说明实现方向正确，但显著性需要更大的 N。更完整的做法是跑几百到几千个请求，固定随机种子，控制模型分布和 prompt 长度，重复多轮实验，然后报告 p50、p95、p99、均值、标准差和置信区间。还可以用 bootstrap 或 Mann-Whitney U test 对两种策略的 latency 分布做检验。

所以我不会把 305ms 到 205ms 说成最终性能结论。它是一个清晰的机制信号，后续需要用更大样本确认稳定性。

## Q880. actor snapshot 1 vs 21 commands 说明了什么？

这个实验说明 actor replay 已经从“从头读完整 actor stream”变成了“读取最近 snapshot，再回放 tail log”。

实验里 actor 连续执行 20 次命令。加上创建类事件，full replay 需要处理 21 条 command 相关记录；启用 snapshot 后，snapshot replay 只需要处理 1 条 tail command。关闭 snapshot 时，snapshot replay 和 full replay 都是 21。这个对比很直接：snapshot 生效以后，恢复路径不再需要扫描全部历史命令。

它的价值在 actor 场景很明显。actor 是有状态对象，命令会越积越多。如果每次 worker 接管都从头 replay，actor 越老恢复越慢。snapshot 相当于把某个时间点的 actor state 固化下来，后面只读 snapshot 之后的增量日志。

不过这组实验的 state 很小，命令数量也只有 20。它反映出 replay command count 降低，不能直接外推成“大 actor 恢复时间一定降低多少倍”。如果要把结论讲得更硬，需要补一组 1k、10k、100k command 的曲线，比较 full replay、snapshot replay、trimmed replay 的 wall-clock recovery time。

## Q881. checkpoint cold fetch 1ms 是否太理想化？

是的，1ms 很理想化。这个数字来自单机本地文件环境，checkpoint source 和 worker cache 都在同一台机器上，checkpoint 体量也很小。它只能验证 checkpoint cache 的代码路径，不能代表真实几十 GB 模型权重从对象存储拉取的耗时。

我会这样解释：这个 probe 不用来表达“真实 cold fetch 只要 1ms”。它检查的是冷启动时是否会创建本地 checkpoint、是否写 manifest、warm request 是否命中同一个 cache、cache_used_bytes 和 eviction_count 是否正确上报。实验结果里 cold request cache_hit=false，warm request cache_hit=true，validation_errors 为空，runtime/model-cache 下也能看到 model-D 的 checkpoint 文件和 manifest。这说明 cache plumbing 是通的。

真实场景里，cold fetch 可能是秒级甚至分钟级，取决于 checkpoint 大小、对象存储延迟、网络带宽、磁盘写入速度、校验开销和并发下载数。后续如果要增强实验，我会用 MinIO 或 S3 兼容对象存储，准备更大的 checkpoint 文件，加入带宽限制和并发 cold miss，然后单独报告 checkpoint_fetch_ms、model_load_ms、first_token_ms 和 total_latency_ms。

## Q882. logstore batch append 20 万 records/s 是否说明系统能处理 20 万 task/s？

不能。这个数字只说明 logstore 在指定参数下可以很快追加小 record，不等于端到端任务吞吐。

logstore benchmark 的 workload 是 20000 条记录、16 个 stream、256B payload，测的是 append-only log 的写入、读取和恢复。batch fsync 下 append records/s 接近 24 万，interval fsync 接近 26 万；always fsync 只有约 1685。这里的差异主要来自 fsync 策略，而不是完整 runtime 的业务处理能力。

一个 task 走完整链路要做很多事：SDK 提交、gRPC、control 生成 TaskSubmitted、metadata view 更新、调度、worker poll、lease/start、Python runner 执行、result store 写入、TaskCompleted 或 TaskFailed、workflow/actor/LLM 上层状态推进。一个任务还可能对应多条 log event。Python runner、用户函数、result store、control 锁、worker executor pool 都会成为瓶颈。

所以 batch append 20 万 records/s 的正确表述是：log 层在 batch fsync 下具备较高顺序写能力；系统 task/s 需要看端到端 benchmark。当前实验里的 task throughput 是 5.17 tasks/s，这是受测试参数和 mock workload 影响的端到端数字，也不能拿来代表生产上限。

## Q883. fault injection 覆盖了哪些失败？没覆盖哪些失败？

已经覆盖的主要是 runtime 常见故障：

- worker kill recovery：worker 挂掉后，任务可以通过 lease/redelivery 恢复。
- queue redelivery：任务被投递后没有正常完成时，control 可以重新投递。
- control restart probe：control 重启后可以从 shared log bootstrap metadata view。
- logd restart 相关路径：通过 logstore recovery 和进程日志覆盖了一部分恢复场景。
- stale completion 和重复 completion 相关路径：测试里检查过旧 lease、重复完成不会覆盖终态或重复计数。

没覆盖的部分我也会说清楚：

- 真实断电或 `kill -9` 正好发生在 record 写一半、fsync 前后、index 更新中间的 crash consistency。
- 磁盘满、磁盘慢、segment 中间损坏、retention.json 写失败。
- 多节点网络分区、control active-active、logd 多副本 leader 切换。
- MinIO/S3 超时、对象丢失、result_ref 指向的对象损坏。
- 真实 GPU OOM、vLLM 请求超时、continuous batching 下的排队。
- Kubernetes Pod eviction、节点重启、本地 model cache 丢失。
- 长时间 soak test 下的内存泄漏、goroutine 泄漏、文件句柄泄漏。

这些问题还没有被当前实验解决，应该放进下一轮生产化实验。

## Q884. 如果面试官让你现场增强实验，你会加哪三项？

我会加三项，优先选最能提高可信度的。

第一项是扩大 LLM scheduling ablation。把请求数从 6 提到几百或几千，模型按热度分布生成，比如 80% 请求打到两个热门模型，20% 打到冷门模型。对 resource-only、locality-aware、predicted-latency 分别跑多轮，报告 cache hit rate、cold start rate、p50/p95/p99、SLO violation rate 和置信区间。这样能回答“p95 下降是不是偶然”的问题。

第二项是真实一点的 checkpoint 实验。用 MinIO 或 S3 兼容对象存储放 checkpoint，准备不同大小的文件，比如 100MB、1GB、5GB；再加入并发 cold miss 和带宽限制。指标看 checkpoint_fetch_ms、model_load_ms、cache_used_bytes、eviction_count、cache hit rate。这样能把 1ms 本地 fetch 的理想化边界补上。

第三项是 crash consistency 和 view drift 测试。用脚本在 log append、metadata update、result store Put、ActorSnapshotCreated append 的中间随机 kill 进程，然后重启系统，比较 replay 出来的 view 和 metadata view 是否一致。这个实验能直接检验 log-first 设计是否经得住更粗糙的故障。

如果面试现场只能演示一个，我会选第三项。因为它最能体现这个项目的主线：shared log 是 source of truth，metadata view 可以重建。

## Q885. 如何把 benchmark 做成 CI 中可持续运行的性能回归测试？

我会把 benchmark 分成两层：PR 级 smoke benchmark 和 nightly benchmark。

PR 级别不能太重，否则会拖慢开发。它应该只跑小规模、确定性的用例，比如 logstore 小样本 append/read/recovery、workflow 3 到 5 个请求、actor snapshot 20 到 50 条命令、LLM locality 6 到 20 个 mock 请求、checkpoint cache probe。CI 保存 JSON 结果，但只对明显回归失败，比如 Go test 失败、replay command count 不符合预期、cache hit 没命中、validation_errors 非空、p95 超过宽松阈值。

nightly benchmark 可以跑大一点。比如 LLM scheduling 跑 1000 个请求，logstore 跑更多 records，actor replay 跑 10k commands，fault injection 跑多轮随机 kill。结果写成固定 schema 的 JSON，保留 command_status、environment、summary、dashboard snapshot 和原始日志。

回归判断不能只用绝对阈值。单机 CI 机器会有抖动，更稳的办法是用历史基线做相对比较，比如 p95 延迟连续两次高于基线 20% 才报警，cache hit rate 低于基线一定比例才失败，logstore tps 下降超过 30% 进入 warning。对短 benchmark，取多轮中位数比取单次结果更稳。

我还会把性能结果分成 blocking 和 non-blocking 两类。功能正确性、replay correctness、validation_errors、fault injection 失败应该阻塞合并；p99 波动、吞吐下降这种容易受环境影响的指标可以先标成 warning，要求人工确认。这样 CI 既能抓住真实退化，也不会因为机器抖动变得没人敢用。
