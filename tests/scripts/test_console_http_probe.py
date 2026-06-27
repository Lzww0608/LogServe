import importlib.util
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
MODULE_PATH = ROOT / "scripts" / "console_http_probe.py"


def load_module():
    spec = importlib.util.spec_from_file_location("console_http_probe", MODULE_PATH)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class ConsoleHTTPProbeTest(unittest.TestCase):
    def test_probe_passes_expected_console_surface(self):
        module = load_module()

        def fake_http_request(method, url, token=None, body=None, timeout=10):
            if url.endswith("/api/healthz"):
                return {"status": 200, "content_type": "application/json", "body": '{"status":"ok"}'}
            if url.endswith("/api/dashboard") and token is None:
                return {"status": 401, "content_type": "application/json", "body": '{"code":"UNAUTHENTICATED"}'}
            if url.endswith("/api/dashboard"):
                return {
                    "status": 200,
                    "content_type": "application/json",
                    "body": '{"queue_depth":0,"tasks":[],"workers":[{"worker_id":"worker-1"}]}',
                }
            if url.endswith("/") or url.endswith("/tasks/console-acceptance"):
                return {"status": 200, "content_type": "text/html", "body": "<title>LogServe Console</title>"}
            if method == "POST" and "/api/tasks?wait=true" in url:
                self.assertEqual("console_acceptance_add", body["task_name"])
                return {
                    "status": 200,
                    "content_type": "application/json",
                    "body": '{"task_id":"task-1","status":"SUCCEEDED","result_json":3}',
                }
            if url.endswith("/api/tasks/task-1"):
                return {
                    "status": 200,
                    "content_type": "application/json",
                    "body": '{"task_id":"task-1","status":"SUCCEEDED","result_json":3}',
                }
            if "/api/tasks?q=console_acceptance_add" in url:
                return {"status": 200, "content_type": "application/json", "body": '{"tasks":[{"task_id":"task-1"}]}'}
            if url.endswith("/api/admin/config"):
                return {"status": 200, "content_type": "application/json", "body": '{"scheduling_policy":"LOCALITY_AWARE"}'}
            return {"status": 404, "content_type": "text/plain", "body": url}

        module.http_request = fake_http_request
        summary = module.run_probe("http://127.0.0.1:8080", "secret", 1)

        self.assertEqual("PASS", summary["verdict"])
        self.assertTrue(summary["checks"]["dashboard_requires_auth"])
        self.assertTrue(summary["checks"]["submit_task_via_console_api"])
        self.assertTrue(summary["checks"]["task_visible_in_dashboard_view"])


if __name__ == "__main__":
    unittest.main()
