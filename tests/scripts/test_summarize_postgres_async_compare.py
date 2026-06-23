import importlib.util
import io
import json
import tempfile
import unittest
from contextlib import redirect_stdout
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
MODULE_PATH = ROOT / "scripts" / "summarize_postgres_async_compare.py"


def load_module():
    spec = importlib.util.spec_from_file_location("summarize_postgres_async_compare", MODULE_PATH)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def write_run(root, mode, task, postgres, materializer):
    run_dir = root / mode
    run_dir.mkdir(parents=True)
    (run_dir / "summary.json").write_text(json.dumps({"verdict": "PASS"}), encoding="utf-8")
    (run_dir / "benchmark.json").write_text(json.dumps({"task_throughput": task}), encoding="utf-8")
    (run_dir / "postgres_benchmark_stats.json").write_text(json.dumps(postgres), encoding="utf-8")
    (run_dir / "dashboard_snapshot.json").write_text(json.dumps({"metadata_materializer": materializer}), encoding="utf-8")


def write_comparison_case(root, async_better):
    write_run(
        root,
        "sync",
        {"throughput_tps": 100, "p99_latency_ms": 80},
        {"transactions_per_sec": 1000, "row_writes_per_sec": 800, "transactions_delta": 1000, "row_writes_delta": 800},
        {"mode": "sync", "flush_error_count": 0},
    )
    if async_better:
        write_run(
            root,
            "async",
            {"throughput_tps": 150, "p99_latency_ms": 40},
            {"transactions_per_sec": 200, "row_writes_per_sec": 100, "transactions_delta": 200, "row_writes_delta": 100},
            {"mode": "async", "flush_error_count": 0, "pending_deltas": 0, "eventual_lag_estimate_ms": 0},
        )
    else:
        write_run(
            root,
            "async",
            {"throughput_tps": 90, "p99_latency_ms": 90},
            {"transactions_per_sec": 1200, "row_writes_per_sec": 900, "transactions_delta": 1200, "row_writes_delta": 900},
            {"mode": "async", "flush_error_count": 0, "pending_deltas": 0, "eventual_lag_estimate_ms": 0},
        )


class PostgresAsyncCompareSummaryTest(unittest.TestCase):
    def test_writes_pass_summary_and_preserves_zero_flush_errors(self):
        module = load_module()
        with tempfile.TemporaryDirectory(dir=ROOT) as tmp:
            root = Path(tmp)
            write_comparison_case(root, async_better=True)

            comparison = module.write_comparison(root, require_improvement=True)

            self.assertTrue(comparison["acceptance"]["pass"])
            self.assertEqual(0, comparison["modes"]["async"]["metadata_materializer_flush_errors"])
            self.assertEqual(1.5, comparison["ratios"]["task_throughput_async_over_sync"])
            self.assertTrue((root / "comparison.json").exists())
            self.assertIn("Acceptance: `PASS`", (root / "summary.md").read_text(encoding="utf-8"))

    def test_main_returns_failure_when_required_improvement_is_missing(self):
        module = load_module()
        with tempfile.TemporaryDirectory(dir=ROOT) as tmp:
            root = Path(tmp)
            write_comparison_case(root, async_better=False)

            with redirect_stdout(io.StringIO()):
                code = module.main([str(root)])

            self.assertEqual(1, code)
            comparison = json.loads((root / "comparison.json").read_text(encoding="utf-8"))
            self.assertFalse(comparison["acceptance"]["pass"])
            self.assertIn("task_throughput_improved", comparison["acceptance"]["checks"])


if __name__ == "__main__":
    unittest.main()
