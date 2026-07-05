# Tests the console HTTP probe contract using a deterministic fake web/API surface.
import importlib.util
import json
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
MODULE_PATH = ROOT / "scripts" / "console_http_probe.py"


# load_module imports the probe script by path so tests exercise the working tree copy.
def load_module():
    spec = importlib.util.spec_from_file_location("console_http_probe", MODULE_PATH)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


# ConsoleHTTPProbeTest drives the probe against a fake HTTP layer instead of a live web server.
class ConsoleHTTPProbeTest(unittest.TestCase):
    # test_probe_passes_expected_console_surface verifies every expected console route and API check can pass together.
    def test_probe_passes_expected_console_surface(self):
        module = load_module()
        seen = {}

        # fake_http_request is a stateful router for the probe; it records model/backpressure values so later checks can assert continuity.
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
            if "/api/" not in url and (
                url.endswith("/")
                or url.endswith("/tasks/console-acceptance")
                or url.endswith("/admin")
                or url.endswith("/functions")
                or url.endswith("/submit/task?function_hash=sha256%3Aconsole-acceptance&function_name=console_acceptance_add")
            ):
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
            if method == "POST" and "/api/workflows?wait=true" in url:
                return {
                    "status": 200,
                    "content_type": "application/json",
                    "body": '{"workflow_id":"wf-1","status":"COMPLETED","steps":[{"step_id":"first","status":"SUCCEEDED"},{"step_id":"second","status":"SUCCEEDED","depends_on":["first"]}],"result_json":5}',
                }
            if url.endswith("/api/workflows/wf-1"):
                return {
                    "status": 200,
                    "content_type": "application/json",
                    "body": '{"workflow_id":"wf-1","status":"COMPLETED","steps":[{"step_id":"second","status":"SUCCEEDED","depends_on":["first"]}]}',
                }
            if "/api/workflows/wf-1/replay" in url:
                return {"status": 200, "content_type": "application/json", "body": '{"consistent_with_metadata":true,"workflow":{"workflow_id":"wf-1"}}'}
            if url.endswith("/api/models") and method == "POST":
                self.assertTrue(body["name"].startswith("model-A-"))
                seen["model_name"] = body["name"]
                seen["model_version"] = body["version"]
                return {"status": 200, "content_type": "application/json", "body": json.dumps({"name": body["name"], "version": body["version"], "adapter": "mock"})}
            if method == "POST" and "/api/llm?wait=true" in url:
                self.assertEqual(seen["model_name"], body["model_name"])
                self.assertEqual(seen["model_version"], body["model_version"])
                return {"status": 200, "content_type": "application/json", "body": '{"task_id":"llm-1","status":"SUCCEEDED","result_json":"ok","worker_id":"worker-1"}'}
            if "/api/llm/llm-1/replay" in url:
                return {
                    "status": 200,
                    "content_type": "application/json",
                    "body": json.dumps({
                        "task_id": "llm-1",
                        "model_name": seen["model_name"],
                        "model_version": seen["model_version"],
                        "cache_hit": True,
                        "model_load_ms": 1,
                        "first_token_ms": 2,
                        "total_latency_ms": 3,
                        "events": [{"event_type": "LLMCompleted"}],
                    }),
                }
            if url.endswith("/api/workers"):
                return {"status": 200, "content_type": "application/json", "body": json.dumps({"workers": [{"worker_id": "worker-1", "cached_models": [{"name": seen["model_name"], "version": seen["model_version"]}]}]})}
            if url.endswith("/api/actors") and method == "POST":
                return {"status": 200, "content_type": "application/json", "body": '{"actor_id":"actor-1","status":"ACTIVE"}'}
            if url.endswith("/api/actors/actor-1"):
                return {"status": 200, "content_type": "application/json", "body": '{"actor_id":"actor-1","status":"ACTIVE","state_json":{"value":1}}'}
            if url.endswith("/api/actors/actor-1/calls"):
                return {"status": 200, "content_type": "application/json", "body": '{"actor_id":"actor-1","call_id":"call-1","status":"SUCCEEDED","result_json":1}'}
            if "/api/logs/streams?prefix=system%3A" in url:
                return {"status": 200, "content_type": "application/json", "body": '{"stream_ids":["system:functions"],"stats":[{"stream_id":"system:functions"}]}' }
            if "/api/logs/streams?prefix=wf%3A" in url:
                return {"status": 200, "content_type": "application/json", "body": '{"stream_ids":["wf:wf-1"],"stats":[{"stream_id":"wf:wf-1"}]}' }
            if "/api/logs/streams?prefix=actor%3A" in url:
                return {"status": 200, "content_type": "application/json", "body": '{"stream_ids":["actor:actor-1"],"stats":[{"stream_id":"actor:actor-1"}]}' }
            if "/api/logs/streams/system%3Afunctions" in url:
                return {"status": 200, "content_type": "application/json", "body": '{"stream_id":"system:functions","records":[{"stream_id":"system:functions","seq":1,"event_type":"FunctionRegistered","payload_json":{"function_hash":"abc"}}],"stats":{"stream_id":"system:functions"}}'}
            if "/api/logs/streams/wf%3Awf-1" in url:
                return {"status": 200, "content_type": "application/json", "body": '{"stream_id":"wf:wf-1","records":[{"stream_id":"wf:wf-1","seq":1,"event_type":"WorkflowStarted"}],"stats":{"stream_id":"wf:wf-1"}}'}
            if "/api/logs/streams/actor%3Aactor-1" in url:
                return {"status": 200, "content_type": "application/json", "body": '{"stream_id":"actor:actor-1","records":[{"stream_id":"actor:actor-1","seq":1,"event_type":"ActorCreated"}],"stats":{"stream_id":"actor:actor-1"}}'}
            if url.endswith("/api/functions") and token is None:
                return {"status": 401, "content_type": "application/json", "body": '{"error":{"code":"UNAUTHENTICATED"}}'}
            if url.endswith("/api/functions"):
                return {
                    "status": 200,
                    "content_type": "application/json",
                    "body": '{"functions":[{"function_hash":"sha256:abc","source_ref":"s3://functions/abc.py","entrypoint":"module:add","language":"python","timestamp_ms":1234}]}',
                }
            if url.endswith("/api/functions/sha256%3Aabc"):
                return {
                    "status": 200,
                    "content_type": "application/json",
                    "body": '{"function_hash":"sha256:abc","source_ref":"s3://functions/abc.py","entrypoint":"module:add","language":"python","timestamp_ms":1234}',
                }
            if url.endswith("/api/admin/config") and token is None:
                return {"status": 401, "content_type": "application/json", "body": '{"error":{"code":"UNAUTHENTICATED"}}'}
            if url.endswith("/api/admin/backpressure") and token is None:
                return {"status": 401, "content_type": "application/json", "body": '{"error":{"code":"UNAUTHENTICATED"}}'}
            if url.endswith("/api/admin/backpressure") and method == "POST":
                if body["queue_high_watermark"] <= 0 or body["redelivery_timeout_ms"] <= 0 or body["log_append_slow_ms"] <= 0:
                    return {"status": 400, "content_type": "application/json", "body": '{"error":{"code":"INVALID_ARGUMENT"}}'}
                seen["backpressure"] = dict(body)
                return {"status": 200, "content_type": "application/json", "body": json.dumps(body)}
            if url.endswith("/api/admin/config"):
                backpressure = seen.get("backpressure") or {"queue_high_watermark": 1024, "redelivery_timeout_ms": 30000, "log_append_slow_ms": 100}
                payload = {
                    "scheduling_policy": "LOCALITY_AWARE",
                    "queue_high_watermark": backpressure["queue_high_watermark"],
                    "redelivery_timeout_ms": backpressure["redelivery_timeout_ms"],
                    "log_append_slow_ms": backpressure["log_append_slow_ms"],
                    "compactable_log_records": 0,
                    "compactable_log_bytes": 0,
                    "metadata_materializer": {"mode": "async", "pending_deltas": 0},
                }
                return {"status": 200, "content_type": "application/json", "body": json.dumps(payload)}
            return {"status": 404, "content_type": "text/plain", "body": url}

        # Replace network I/O with the fake router so this remains a deterministic unit test.
        module.http_request = fake_http_request
        summary = module.run_probe("http://127.0.0.1:8080", "secret", 1)

        self.assertEqual("PASS", summary["verdict"])
        self.assertTrue(summary["checks"]["dashboard_requires_auth"])
        self.assertTrue(summary["checks"]["submit_task_via_console_api"])
        self.assertTrue(summary["checks"]["task_visible_in_dashboard_view"])
        for check in (
            "submit_workflow_via_console_api",
            "workflow_detail_has_dag_dependencies",
            "workflow_replay_consistent",
            "register_model_via_console_api",
            "submit_llm_via_console_api",
            "llm_replay_trace_has_latency",
            "worker_cache_matrix_has_model",
            "create_actor_via_console_api",
            "call_actor_via_console_api",
            "get_actor_status",
            "log_streams_list_system_functions",
            "log_stream_read_system_functions",
            "log_stream_read_workflow",
            "log_stream_read_actor",
            "static_admin_route",
            "static_functions_route",
            "static_submit_function_hash_route",
            "functions_requires_auth",
            "functions_list_with_auth",
            "function_detail_with_auth",
            "admin_config_requires_auth",
            "admin_backpressure_requires_auth",
            "admin_config_with_auth",
            "admin_config_has_materializer_stats",
            "admin_backpressure_rejects_invalid_values",
            "admin_backpressure_update_with_auth",
            "admin_config_reflects_backpressure_update",
        ):
            self.assertTrue(summary["checks"][check], check)


if __name__ == "__main__":
    unittest.main()
