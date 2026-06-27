import importlib.util
import io
import json
import tempfile
import unittest
from contextlib import redirect_stdout
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
MODULE_PATH = ROOT / "scripts" / "summarize_console_acceptance.py"


def load_module():
    spec = importlib.util.spec_from_file_location("summarize_console_acceptance", MODULE_PATH)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def write_status(root, name, exit_code):
    with (root / "command_status.jsonl").open("a", encoding="utf-8") as handle:
        handle.write(json.dumps({"name": name, "exit_code": exit_code, "duration_sec": 1, "log": f"{name}.log"}) + "\n")


def write_config(root, run_docker=True, run_npm_ci=True):
    (root / "run_config.json").write_text(
        json.dumps({"run_docker": run_docker, "run_npm_ci": run_npm_ci, "base_url": "http://127.0.0.1:8080"}),
        encoding="utf-8",
    )


def write_local_statuses(root):
    for name in ("go_test_web", "go_vet_web", "web_npm_ci", "web_build", "python_script_tests", "package_results"):
        write_status(root, name, 0)


def write_docker_statuses(root):
    for name in (
        "docker_compose_config",
        "docker_compose_build",
        "docker_compose_up",
        "web_health_ready",
        "console_api_ready",
        "console_worker_ready",
        "console_http_probe",
    ):
        write_status(root, name, 0)


def write_probe(root, verdict="PASS", checks=None):
    if checks is None:
        checks = {
            "healthz_without_auth": True,
            "dashboard_requires_auth": True,
            "dashboard_with_auth": True,
            "static_root": True,
            "static_deep_link": True,
            "submit_task_via_console_api": True,
            "get_task_detail": True,
            "task_visible_in_dashboard_view": True,
            "submit_workflow_via_console_api": True,
            "workflow_detail_has_dag_dependencies": True,
            "workflow_replay_consistent": True,
            "register_model_via_console_api": True,
            "submit_llm_via_console_api": True,
            "llm_replay_trace_has_latency": True,
            "worker_cache_matrix_has_model": True,
            "create_actor_via_console_api": True,
            "call_actor_via_console_api": True,
            "get_actor_status": True,
            "log_streams_list_system_functions": True,
            "log_stream_read_system_functions": True,
            "log_stream_read_workflow": True,
            "log_stream_read_actor": True,
            "admin_config_with_auth": True,
        }
    (root / "console_http_probe.json").write_text(json.dumps({"verdict": verdict, "checks": checks}), encoding="utf-8")


class ConsoleAcceptanceSummaryTest(unittest.TestCase):
    def test_writes_pass_summary_for_full_console_acceptance(self):
        module = load_module()
        with tempfile.TemporaryDirectory(dir=ROOT) as tmp:
            root = Path(tmp)
            write_config(root, run_docker=True, run_npm_ci=True)
            write_local_statuses(root)
            write_docker_statuses(root)
            write_probe(root)

            summary = module.write_summary(root)

            self.assertEqual("PASS", summary["verdict"])
            self.assertTrue(summary["checks"]["probe_submit_task_via_console_api"])
            self.assertEqual("PASS", summary["features_6_10"]["verdict"])
            self.assertEqual("PASS", summary["features_6_10"]["features"]["feature_10_log_stream_explorer"]["state"])
            markdown = (root / "acceptance_summary.md").read_text(encoding="utf-8")
            self.assertIn("Ubuntu Console Acceptance Summary", markdown)
            self.assertIn("Features 6-10", markdown)
            self.assertIn("feature_6_workflow_dag", markdown)
            self.assertIn("HTTP Probe", markdown)

    def test_docker_disabled_skips_probe_checks(self):
        module = load_module()
        with tempfile.TemporaryDirectory(dir=ROOT) as tmp:
            root = Path(tmp)
            write_config(root, run_docker=False, run_npm_ci=True)
            write_local_statuses(root)

            summary = module.write_summary(root)

            self.assertEqual("PASS", summary["verdict"])
            self.assertEqual("INCOMPLETE", summary["features_6_10"]["verdict"])
            self.assertNotIn("console_http_probe", summary["checks"])
            self.assertNotIn("probe_dashboard_with_auth", summary["checks"])

    def test_failed_probe_check_marks_summary_failed(self):
        module = load_module()
        with tempfile.TemporaryDirectory(dir=ROOT) as tmp:
            root = Path(tmp)
            write_config(root, run_docker=True, run_npm_ci=True)
            write_local_statuses(root)
            write_docker_statuses(root)
            write_probe(root, verdict="FAIL", checks={"healthz_without_auth": True, "dashboard_requires_auth": False})

            with redirect_stdout(io.StringIO()):
                code = module.main([str(root)])

            self.assertEqual(1, code)
            summary = json.loads((root / "acceptance_summary.json").read_text(encoding="utf-8"))
            self.assertEqual("FAIL", summary["verdict"])
            self.assertIn("probe_dashboard_requires_auth", summary["failed_checks"])


if __name__ == "__main__":
    unittest.main()
