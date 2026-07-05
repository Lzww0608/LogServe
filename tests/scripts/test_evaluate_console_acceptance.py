# Tests the console acceptance evaluator across packaged, lightweight, and failed inputs.
import importlib.util
import io
import json
import tarfile
import tempfile
import unittest
from contextlib import redirect_stdout
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
MODULE_PATH = ROOT / "scripts" / "evaluate_console_acceptance.py"


# load_module imports the evaluator from scripts/ without requiring it to be installed as a package.
def load_module():
    spec = importlib.util.spec_from_file_location("evaluate_console_acceptance", MODULE_PATH)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


# write_summary creates the minimal acceptance_summary.json shape consumed by evaluate_result.
def write_summary(root, run_docker=True, verdict="PASS", probe_verdict="PASS", probe_checks=None, include_package_command=True):
    if probe_checks is None:
        probe_checks = {
            "healthz_without_auth": True,
            "dashboard_requires_auth": True,
            "dashboard_with_auth": True,
            "static_root": True,
            "static_deep_link": True,
            "static_admin_route": True,
            "static_functions_route": True,
            "static_submit_function_hash_route": True,
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
            "functions_requires_auth": True,
            "functions_list_with_auth": True,
            "function_detail_with_auth": True,
            "admin_config_requires_auth": True,
            "admin_backpressure_requires_auth": True,
            "admin_config_with_auth": True,
            "admin_config_has_materializer_stats": True,
            "admin_backpressure_rejects_invalid_values": True,
            "admin_backpressure_update_with_auth": True,
            "admin_config_reflects_backpressure_update": True,
        }
    checks = {
        "go_web_tests": True,
        "go_web_vet": True,
        "web_build": True,
        "python_script_tests": True,
        "web_npm_ci": True,
    }
    commands = [
        {"name": "go_test_web", "exit_code": 0, "duration_sec": 1, "log": "go_test_web.log"},
    ]
    if include_package_command:
        commands.append({"name": "package_results", "exit_code": 0, "duration_sec": 0, "log": "package.log"})
    if run_docker:
        checks.update(
            {
                "docker_compose_config": True,
                "docker_compose_build": True,
                "docker_compose_up": True,
                "web_health_ready": True,
                "console_http_probe": probe_verdict == "PASS",
            }
        )
        checks.update({f"probe_{name}": passed for name, passed in probe_checks.items()})
        commands.append({"name": "console_http_probe", "exit_code": 0 if probe_verdict == "PASS" else 1, "duration_sec": 1, "log": "console_http_probe.log"})
    payload = {
        "verdict": verdict,
        "result_dir": str(root),
        "failed_commands": [] if verdict == "PASS" and probe_verdict == "PASS" else ["console_http_probe"],
        "failed_checks": [] if verdict == "PASS" and probe_verdict == "PASS" else ["console_http_probe"],
        "commands": commands,
        "checks": checks,
        "probe": {"verdict": probe_verdict, "checks": probe_checks} if run_docker else {},
        "run_config": {"run_docker": run_docker, "run_npm_ci": True},
    }
    (root / "acceptance_summary.json").write_text(json.dumps(payload), encoding="utf-8")


# make_package builds a tar.gz fixture so materialize_input can be tested through the packaged path.
def make_package(source_dir, package_path):
    with tarfile.open(package_path, "w:gz") as archive:
        for path in source_dir.rglob("*"):
            archive.add(path, arcname=path.relative_to(source_dir))


# ConsoleAcceptanceEvaluationTest covers evaluator verdicts for full, lightweight, failed, and packaged inputs.
class ConsoleAcceptanceEvaluationTest(unittest.TestCase):
    # test_full_pass_result_matches_expectations proves a complete Docker-backed acceptance run maps to PASS.
    def test_full_pass_result_matches_expectations(self):
        module = load_module()
        with tempfile.TemporaryDirectory(dir=ROOT) as tmp:
            root = Path(tmp)
            write_summary(root)

            result = module.evaluate_result(root)

            self.assertEqual("PASS", result["verdict"])
            self.assertFalse(result["failures"])
            self.assertEqual("PASS", result["features_6_10"]["verdict"])
            self.assertEqual("PASS", result["frontend_admin_functions"]["verdict"])

    # test_lightweight_result_is_incomplete preserves the rule that local-only runs are incomplete, not full PASS.
    def test_lightweight_result_is_incomplete(self):
        module = load_module()
        with tempfile.TemporaryDirectory(dir=ROOT) as tmp:
            root = Path(tmp)
            write_summary(root, run_docker=False)

            result = module.evaluate_result(root)

            self.assertEqual("INCOMPLETE", result["verdict"])
            self.assertEqual("Docker console runtime was not exercised", result["reason"])
            self.assertEqual("INCOMPLETE", result["features_6_10"]["verdict"])
            self.assertEqual("INCOMPLETE", result["frontend_admin_functions"]["verdict"])

    # test_lightweight_failed_local_check_is_fail ensures local gate failures still fail without Docker evidence.
    def test_lightweight_failed_local_check_is_fail(self):
        module = load_module()
        with tempfile.TemporaryDirectory(dir=ROOT) as tmp:
            root = Path(tmp)
            write_summary(root, run_docker=False)
            summary_path = root / "acceptance_summary.json"
            summary = json.loads(summary_path.read_text(encoding="utf-8"))
            summary["verdict"] = "FAIL"
            summary["checks"]["go_web_tests"] = False
            summary["failed_commands"] = ["go_test_web"]
            summary["failed_checks"] = ["go_web_tests"]
            summary_path.write_text(json.dumps(summary), encoding="utf-8")

            result = module.evaluate_result(root)

            self.assertEqual("FAIL", result["verdict"])
            self.assertTrue(any("go_web_tests" in item for item in result["failures"]))

    # test_rejects_unsafe_tar_link_member protects extraction against symlink traversal entries.
    def test_rejects_unsafe_tar_link_member(self):
        module = load_module()
        with tempfile.TemporaryDirectory(dir=ROOT) as tmp:
            package = Path(tmp) / "console-acceptance-package.tar.gz"
            with tarfile.open(package, "w:gz") as archive:
                link = tarfile.TarInfo("link")
                link.type = tarfile.SYMTYPE
                link.linkname = "../outside"
                archive.addfile(link)

            with self.assertRaisesRegex(ValueError, "unsafe tar link member"):
                module.materialize_input(package)

    # test_failed_probe_result_fails_expectations verifies probe failures propagate into evaluator failures.
    def test_failed_probe_result_fails_expectations(self):
        module = load_module()
        with tempfile.TemporaryDirectory(dir=ROOT) as tmp:
            root = Path(tmp)
            write_summary(root, verdict="FAIL", probe_verdict="FAIL", probe_checks={"dashboard_requires_auth": False})

            result = module.evaluate_result(root)

            self.assertEqual("FAIL", result["verdict"])
            self.assertTrue(any("dashboard_requires_auth" in item for item in result["failures"]))

    # test_failed_feature_group_fails_expectations keeps feature 6-10 grouping tied to probe check names.
    def test_failed_feature_group_fails_expectations(self):
        module = load_module()
        with tempfile.TemporaryDirectory(dir=ROOT) as tmp:
            root = Path(tmp)
            write_summary(root)
            summary_path = root / "acceptance_summary.json"
            summary = json.loads(summary_path.read_text(encoding="utf-8"))
            summary["probe"]["checks"]["worker_cache_matrix_has_model"] = False
            summary["checks"]["probe_worker_cache_matrix_has_model"] = False
            summary_path.write_text(json.dumps(summary), encoding="utf-8")

            result = module.evaluate_result(root)

            self.assertEqual("FAIL", result["verdict"])
            self.assertEqual("FAIL", result["features_6_10"]["features"]["feature_8_worker_cache_matrix"]["state"])
            self.assertTrue(any("feature_8_worker_cache_matrix" in item for item in result["failures"]))

    # test_failed_frontend_admin_group_fails_expectations covers the admin/functions feature group verdict.
    def test_failed_frontend_admin_group_fails_expectations(self):
        module = load_module()
        with tempfile.TemporaryDirectory(dir=ROOT) as tmp:
            root = Path(tmp)
            write_summary(root)
            summary_path = root / "acceptance_summary.json"
            summary = json.loads(summary_path.read_text(encoding="utf-8"))
            summary["probe"]["checks"]["admin_config_reflects_backpressure_update"] = False
            summary["checks"]["probe_admin_config_reflects_backpressure_update"] = False
            summary_path.write_text(json.dumps(summary), encoding="utf-8")

            result = module.evaluate_result(root)

            self.assertEqual("FAIL", result["verdict"])
            self.assertEqual("FAIL", result["frontend_admin_functions"]["verdict"])
            self.assertTrue(any("frontend admin/functions" in item for item in result["failures"]))

    # test_main_accepts_package exercises CLI-style package input and verifies extracted warnings stay empty.
    def test_main_accepts_package(self):
        module = load_module()
        with tempfile.TemporaryDirectory(dir=ROOT) as tmp:
            root = Path(tmp) / "result"
            root.mkdir()
            write_summary(root, include_package_command=False)
            package = Path(tmp) / "console-acceptance-package.tar.gz"
            make_package(root, package)

            with redirect_stdout(io.StringIO()):
                code = module.main([str(package)])

            self.assertEqual(0, code)

            extracted, temp_dir = module.materialize_input(package)
            try:
                result = module.evaluate_result(extracted)
                self.assertEqual([], result["warnings"])
            finally:
                if temp_dir is not None:
                    module.shutil.rmtree(temp_dir, ignore_errors=True)


if __name__ == "__main__":
    unittest.main()
