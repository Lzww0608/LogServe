# LogServe 文档导航

这组文档按阅读目的拆开。README 只保留入口，项目细节放在 `docs/` 里。实验和报告产物不要用日期命名；默认使用 `latest`，需要保留多份结果时用 `run-01`、`accepted`、`failed-01` 这类名字。

## 推荐阅读顺序

1. `architecture.md`：先看系统怎么工作，尤其是 shared log、control、worker、workflow、actor 和 LLM serving 的关系。
2. `optimizations.md`：再看已经落地的优化，包括 scheduler v2、metadata checkpoint、physical compaction、PostgreSQL async materializer 等。
3. `report.md`：最后看实验结果和验收结论，确认哪些说法有数据支撑。

## 文档说明

| 文件 | 用途 |
|---|---|
| `architecture.md` | 项目技术架构，适合讲项目设计和实现细节。 |
| `optimizations.md` | 优化技术说明，区分已落地、已验证和后续计划。 |
| `report.md` | 最新项目验收和实验结果。 |
| `resume.md` | 简历、面试和答辩表述。 |
| `ubuntu-project-acceptance.md` | 顶层 Ubuntu 验收流程和通过结果。 |
| `ubuntu-console-features-6-10-acceptance.md` | Console 功能 6-10 的 Ubuntu 专项验收流程。 |
| `ubuntu-console-frontend-acceptance.md` | Console 前端/Admin/Functions 的 Ubuntu 专项验收流程。 |
| `ubuntu-checkpoint-acceptance.md` | metadata checkpoint 验收流程和通过结果。 |
| `ubuntu-postgres-async-test.md` | PostgreSQL async materializer 验收流程和通过结果。 |
| `plan.md` | 更长的优化设计备忘录，里面有很多可拆 issue 的细节。部分内容已经落地，当前状态以 `optimizations.md` 和 `report.md` 为准。 |

## 一句话版本

LogServe 的主线是：用 shared log 保存事实，用 control plane materialize 当前状态，用 worker 执行任务，用 replay 和 checkpoint 解决恢复成本，再用调度和缓存优化 task、actor 和 LLM 的运行路径。

## 结论边界

当前通过的是单台 Ubuntu 服务器上的多进程机制验收。可以说核心机制可复现、主要回归门禁通过、若干优化路径有数据支撑。不要把它说成生产级多机平台，也不要把 mock LLM 的结果说成真实 GPU/vLLM 性能。
