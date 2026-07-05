# Tests top-level Ubuntu project acceptance summary aggregation and sub-suite handling.
import importlib.util
import io
import json
import tempfile
import unittest
from contextlib import redirect_stdout
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
MODULE_PATH = ROOT / "scripts" / "summarize_ubuntu_project_acceptance.py"


# load_module imports the project-level acceptance summarizer from scripts/.
def load_module():
    spec = importlib.util.spec_from_file_location("summarize_ubuntu_project_acceptance", MODULE_PATH)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


# write_status appends one command result for project-level failed-command detection.
def write_status(root, name, exit_code):
    with (root / "command_status.jsonl").open("a", encoding="utf-8") as handle:
        handle.write(json.dumps({"name": name, "exit_code": exit_code, "duration_sec": 1, "log": f"{name}.log"}) + "\n")


# write_required_statuses records the baseline gates required for a PASS project summary.
def write_required_statuses(root):
    for name in (
        "go_test_all",
        "go_test_physical_compaction",
        "go_race_logstore",
        "python_script_tests",
        "python_compileall",
        "package_results",
    ):
        write_status(root, name, 0)


# write_child_summary creates a nested sub-suite summary plus Markdown artifact for send-back paths.
def write_child_summary(root, rel_dir, filename, verdict="PASS"):
    directory = root / rel_dir
    directory.mkdir(parents=True, exist_ok=True)
    (directory / filename).write_text(json.dumps({"verdict": verdict}), encoding="utf-8")
    markdown_name = "summary.md" if filename == "summary.json" else "acceptance_summary.md"
    (directory / markdown_name).write_text(f"# {rel_dir}\n", encoding="utf-8")


# write_config controls which optional sub-suites are enabled in project acceptance.
def write_config(root, compose=True, checkpoint=True, postgres=True):
    (root / "run_config.json").write_text(
        json.dumps(
            {
                "run_compose_experiment": compose,
                "run_checkpoint_acceptance": checkpoint,
                "run_postgres_async_acceptance": postgres,
            }
        ),
        encoding="utf-8",
    )


# UbuntuProjectAcceptanceSummaryTest verifies strict aggregation of baseline commands and enabled sub-suites.
class UbuntuProjectAcceptanceSummaryTest(unittest.TestCase):
    # test_writes_pass_summary_with_all_subsuites covers the full project acceptance happy path.
    def test_writes_pass_summary_with_all_subsuites(self):
        module = load_module()
        with tempfile.TemporaryDirectory(dir=ROOT) as tmp:
            root = Path(tmp)
            write_config(root)
            write_required_statuses(root)
            write_child_summary(root, "compose_experiment", "summary.json", "PASS")
            write_child_summary(root, "checkpoint_acceptance", "acceptance_summary.json", "PASS")
            write_child_summary(root, "postgres_async_acceptance", "acceptance_summary.json", "pass")

            summary = module.write_summary(root)

            self.assertEqual("PASS", summary["verdict"])
            self.assertTrue(summary["checks"]["physical_compaction_tests"])
            self.assertEqual("PASS", summary["subsuites"]["compose_experiment"]["state"])
            markdown = (root / "acceptance_summary.md").read_text(encoding="utf-8")
            self.assertIn("Ubuntu Project Acceptance Summary", markdown)
            self.assertIn("compose_experiment_pass", markdown)

    # test_disabled_subsuite_is_skipped_without_failing keeps intentionally disabled suites from failing the run.
    def test_disabled_subsuite_is_skipped_without_failing(self):
        module = load_module()
        with tempfile.TemporaryDirectory(dir=ROOT) as tmp:
            root = Path(tmp)
            write_config(root, compose=False, checkpoint=False, postgres=False)
            write_required_statuses(root)

            summary = module.write_summary(root)

            self.assertEqual("PASS", summary["verdict"])
            self.assertEqual("SKIPPED", summary["subsuites"]["compose_experiment"]["state"])
            self.assertNotIn("compose_experiment_pass", summary["checks"])

    # test_failed_command_marks_summary_failed confirms any command_status failure fails the project summary.
    def test_failed_command_marks_summary_failed(self):
        module = load_module()
        with tempfile.TemporaryDirectory(dir=ROOT) as tmp:
            root = Path(tmp)
            write_config(root, compose=False, checkpoint=False, postgres=False)
            write_required_statuses(root)
            write_status(root, "go_vet", 1)

            summary = module.write_summary(root)

            self.assertEqual("FAIL", summary["verdict"])
            self.assertIn("go_vet", summary["failed_commands"])

    # test_main_returns_failure_for_failed_check verifies missing enabled sub-suite artifacts produce a non-zero CLI result.
    def test_main_returns_failure_for_failed_check(self):
        module = load_module()
        with tempfile.TemporaryDirectory(dir=ROOT) as tmp:
            root = Path(tmp)
            write_config(root, compose=True, checkpoint=False, postgres=False)
            write_required_statuses(root)

            with redirect_stdout(io.StringIO()):
                code = module.main([str(root)])

            self.assertEqual(1, code)
            summary = json.loads((root / "acceptance_summary.json").read_text(encoding="utf-8"))
            self.assertEqual("FAIL", summary["verdict"])
            self.assertIn("compose_experiment_pass", summary["failed_checks"])


if __name__ == "__main__":
    unittest.main()