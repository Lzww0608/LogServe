# Tests sync-vs-async PostgreSQL comparison summaries and tolerance behavior.
import importlib.util
import io
import json
import tempfile
import unittest
from contextlib import redirect_stdout
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
MODULE_PATH = ROOT / "scripts" / "summarize_postgres_async_compare.py"


# load_module imports the async-vs-sync comparison summarizer from scripts/.
def load_module():
    spec = importlib.util.spec_from_file_location("summarize_postgres_async_compare", MODULE_PATH)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


# write_run builds one mode directory with benchmark, Postgres, and materializer fixtures.
def write_run(root, mode, task, postgres, materializer):
    run_dir = root / mode
    run_dir.mkdir(parents=True)
    (run_dir / "summary.json").write_text(json.dumps({"verdict": "PASS"}), encoding="utf-8")
    (run_dir / "benchmark.json").write_text(json.dumps({"task_throughput": task}), encoding="utf-8")
    (run_dir / "postgres_benchmark_stats.json").write_text(json.dumps(postgres), encoding="utf-8")
    (run_dir / "dashboard_snapshot.json").write_text(json.dumps({"metadata_materializer": materializer}), encoding="utf-8")


# write_comparison_case creates paired sync/async runs that either satisfy or violate improvement checks.
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


# PostgresAsyncCompareSummaryTest covers acceptance ratios, tolerance windows, and omitted proto-zero fields.
class PostgresAsyncCompareSummaryTest(unittest.TestCase):
    # test_writes_pass_summary_and_preserves_zero_flush_errors covers the positive async-improvement case.
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

    # test_main_returns_failure_when_required_improvement_is_missing verifies CLI failure when async regresses.
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
            self.assertIn("task_throughput_within_tolerance", comparison["acceptance"]["checks"])

    # test_near_equal_task_metrics_pass_with_default_tolerance documents the non-strict tolerance contract.
    def test_near_equal_task_metrics_pass_with_default_tolerance(self):
        module = load_module()
        with tempfile.TemporaryDirectory(dir=ROOT) as tmp:
            root = Path(tmp)
            write_run(
                root,
                "sync",
                {"throughput_tps": 100, "p99_latency_ms": 80},
                {"transactions_per_sec": 1000, "row_writes_per_sec": 800},
                {"mode": "sync"},
            )
            write_run(
                root,
                "async",
                {"throughput_tps": 99.5, "p99_latency_ms": 80},
                {"transactions_per_sec": 200, "row_writes_per_sec": 100},
                {"mode": "async", "pending_deltas": 0, "eventual_lag_estimate_ms": 0},
            )

            comparison = module.write_comparison(root, require_improvement=True)

            self.assertTrue(comparison["acceptance"]["pass"])
            self.assertTrue(comparison["acceptance"]["checks"]["task_throughput_within_tolerance"])
            self.assertTrue(comparison["acceptance"]["checks"]["task_submit_p99_within_tolerance"])
            self.assertFalse(comparison["observations"]["task_throughput_strictly_improved"])
            self.assertFalse(comparison["observations"]["task_submit_p99_strictly_improved"])

    # test_omitted_proto_zero_flush_errors_counts_as_zero keeps missing proto-zero fields compatible with old fixtures.
    def test_omitted_proto_zero_flush_errors_counts_as_zero(self):
        module = load_module()
        with tempfile.TemporaryDirectory(dir=ROOT) as tmp:
            root = Path(tmp)
            write_run(
                root,
                "sync",
                {"throughput_tps": 100, "p99_latency_ms": 80},
                {"transactions_per_sec": 1000, "row_writes_per_sec": 800},
                {"mode": "sync"},
            )
            write_run(
                root,
                "async",
                {"throughput_tps": 150, "p99_latency_ms": 40},
                {"transactions_per_sec": 200, "row_writes_per_sec": 100},
                {"mode": "async", "pending_deltas": 0, "eventual_lag_estimate_ms": 0},
            )

            comparison = module.write_comparison(root, require_improvement=True)

            self.assertTrue(comparison["acceptance"]["checks"]["async_materializer_flush_errors_zero"])
            self.assertEqual(0, comparison["modes"]["async"]["metadata_materializer_flush_errors"])


if __name__ == "__main__":
    unittest.main()
