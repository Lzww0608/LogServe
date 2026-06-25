# Benchmark artifacts

Generated benchmark outputs and profile captures are written here by:

- `bash scripts/benchmark_micro.sh`
- `bash scripts/collect_pprof.sh`
- `bash scripts/run_experiment.sh`

Use `benchmarks/baseline.example.json` as a template. After a known-good run:

```bash
cp benchmarks/micro-<timestamp>.json benchmarks/baseline.json
python3 scripts/compare_benchmark.py benchmarks/baseline.json benchmarks/micro-<timestamp>.json
```

Set `LOGSERVE_BENCH_MAX_RATIO` (default `1.15`) to control microbenchmark regression tolerance.

Rollback switches for optimizations are environment flags documented in README
sections (for example `LOGSERVE_SCHEDULER_V2`, `LOGSERVE_LOG_MMAP_READ`).
