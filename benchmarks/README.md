# LogServe Benchmarks

Run the Phase 5 benchmark harness from the repository root:

```powershell
powershell.exe -ExecutionPolicy Bypass -File .\scripts\phase5_benchmark.ps1
```

The script starts local logd/control services plus three workers and writes the
latest JSON report to:

```text
benchmarks/phase5_latest.json
```

The report includes workflow latency, task throughput, actor replay/snapshot
ablation, LLM cold start, locality scheduling ablation, and a replay-enabled vs
replay-disabled analysis baseline.
