# Benchmark 产物

这个目录保存 benchmark JSON、pprof 和 baseline 文件。脚本会把生成结果写到这里，报告和验收文档再引用这些结果。

常用脚本：

```bash
bash scripts/benchmark_micro.sh
bash scripts/collect_pprof.sh 127.0.0.1:6062 benchmarks/profiles
bash scripts/run_experiment.sh
```

`benchmarks/baseline.example.json` 是 baseline 模板。确认某次结果可作为基线后，可以复制成正式 baseline：

```bash
cp benchmarks/micro-latest.json benchmarks/baseline.json
python3 scripts/compare_benchmark.py benchmarks/baseline.json benchmarks/micro-latest.json
```

默认回归阈值由 `LOGSERVE_BENCH_MAX_RATIO` 控制，默认是 `1.15`。如果某个 microbenchmark 的耗时超过 baseline 的 1.15 倍，比较脚本会标记为回归。

优化相关说明见：

- `docs/optimizations.md`
- `docs/report.md`
- `docs/plan.md`

常见回滚开关包括 `LOGSERVE_SCHEDULER_V2`、`LOGSERVE_LOG_MMAP_READ` 和 `LOGSERVE_POSTGRES_MODE`。