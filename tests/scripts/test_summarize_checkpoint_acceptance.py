import importlib.util
import io
import json
import tempfile
import unittest
from contextlib import redirect_stdout
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
MODULE_PATH = ROOT / "scripts" / "summarize_checkpoint_acceptance.py"


def load_module():
    spec = importlib.util.spec_from_file_location("summarize_checkpoint_acceptance", MODULE_PATH)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def write_status(root, name, exit_code):
    with (root / "command_status.jsonl").open("a", encoding="utf-8") as handle:
        handle.write(json.dumps({"name": name, "exit_code": exit_code, "duration_sec": 1, "log": f"{name}.log"}) + "\n")


def write_acceptance(root, verdict="PASS", checks=None):
    if checks is None:
        checks = {
            "checkpoint_created": True,
            "checkpoint_replay_consistent": True,
            "checkpoint_read_records_reduced": True,
            "checkpoint_tail_only_reads": True,
            "corrupt_checkpoint_fallback": True,
            "checkpoint_retention": True,
        }
    payload = {
        "verdict": verdict,
        "workload": {"tasks": 100, "workflows": 8, "actors": 8, "llm_streams": 20, "tail_events": 12},
        "checkpoint": {"id": "checkpoint-1", "stream_count": 136, "task_count": 108, "workflow_count": 8, "actor_count": 8, "llm_stats_count": 3},
        "full_replay": {"duration_ms": 31, "read_log_calls": 144, "records_read": 412},
        "checkpoint_replay": {"duration_ms": 8, "read_log_calls": 144, "records_read": 28},
        "ratios": {"checkpoint_records_over_full": 0.068, "checkpoint_duration_over_full": 0.258},
        "consistency": {"consistent": True, "checked_count": 124, "checkpoint_id": "checkpoint-1"},
        "checks": checks,
    }
    (root / "checkpoint_acceptance.json").write_text(json.dumps(payload), encoding="utf-8")


class CheckpointAcceptanceSummaryTest(unittest.TestCase):
    def test_writes_pass_summary(self):
        module = load_module()
        with tempfile.TemporaryDirectory(dir=ROOT) as tmp:
            root = Path(tmp)
            write_status(root, "go_test_checkpoint", 0)
            write_acceptance(root)

            summary = module.write_summary(root)

            self.assertEqual("PASS", summary["verdict"])
            self.assertEqual(0.068, summary["acceptance"]["ratios"]["checkpoint_records_over_full"])
            self.assertTrue((root / "summary.json").exists())
            markdown = (root / "summary.md").read_text(encoding="utf-8")
            self.assertIn("Verdict: **PASS**", markdown)
            self.assertIn("checkpoint_read_records_reduced", markdown)

    def test_main_returns_failure_when_a_check_fails(self):
        module = load_module()
        with tempfile.TemporaryDirectory(dir=ROOT) as tmp:
            root = Path(tmp)
            write_status(root, "go_test_checkpoint", 0)
            write_acceptance(root, verdict="FAIL", checks={"checkpoint_created": True, "checkpoint_replay_consistent": False})

            with redirect_stdout(io.StringIO()):
                code = module.main([str(root)])

            self.assertEqual(1, code)
            summary = json.loads((root / "summary.json").read_text(encoding="utf-8"))
            self.assertEqual("FAIL", summary["verdict"])
            self.assertIn("checkpoint_replay_consistent", summary["failed_checks"])

    def test_failed_command_marks_summary_failed(self):
        module = load_module()
        with tempfile.TemporaryDirectory(dir=ROOT) as tmp:
            root = Path(tmp)
            write_status(root, "checkpoint_acceptance_go_test", 1)
            write_acceptance(root)

            summary = module.write_summary(root)

            self.assertEqual("FAIL", summary["verdict"])
            self.assertIn("checkpoint_acceptance_go_test", summary["failed_commands"])


if __name__ == "__main__":
    unittest.main()
