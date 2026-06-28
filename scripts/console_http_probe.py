#!/usr/bin/env python3
import argparse
import json
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path


def http_request(method, url, token=None, body=None, timeout=10):
    data = None
    headers = {}
    if body is not None:
        data = json.dumps(body).encode("utf-8")
        headers["Content-Type"] = "application/json"
    if token:
        headers["Authorization"] = f"Bearer {token}"
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read()
            return {
                "status": resp.status,
                "content_type": resp.headers.get("Content-Type", ""),
                "body": raw.decode("utf-8", errors="replace"),
            }
    except urllib.error.HTTPError as exc:
        raw = exc.read()
        return {
            "status": exc.code,
            "content_type": exc.headers.get("Content-Type", ""),
            "body": raw.decode("utf-8", errors="replace"),
        }
    except Exception as exc:
        return {"status": 0, "content_type": "", "body": "", "error": str(exc)}


def parse_json(response):
    try:
        return json.loads(response.get("body") or "")
    except json.JSONDecodeError:
        return None


def same_json_value(actual, expected):
    return actual == expected or str(actual) == str(expected)


def wait_for(predicate, timeout_sec=30, interval_sec=1):
    deadline = time.time() + timeout_sec
    last = None
    while time.time() < deadline:
        ok, last = predicate()
        if ok:
            return True, last
        time.sleep(interval_sec)
    return False, last


def run_probe(base_url, token, timeout_sec):
    base_url = base_url.rstrip("/")
    checks = {}
    failures = []
    details = {"base_url": base_url}

    def record(name, passed, detail=None):
        checks[name] = bool(passed)
        if detail is not None:
            details[name] = detail
        if not passed:
            failures.append({"check": name, "detail": detail})

    health = http_request("GET", f"{base_url}/api/healthz", timeout=10)
    health_json = parse_json(health)
    record(
        "healthz_without_auth",
        health["status"] == 200 and isinstance(health_json, dict) and health_json.get("status") == "ok",
        {"status": health["status"], "body": health_json or health.get("body"), "error": health.get("error")},
    )

    dashboard_no_auth = http_request("GET", f"{base_url}/api/dashboard", timeout=10)
    record(
        "dashboard_requires_auth",
        dashboard_no_auth["status"] == 401,
        {"status": dashboard_no_auth["status"], "body": parse_json(dashboard_no_auth) or dashboard_no_auth.get("body")},
    )

    dashboard = http_request("GET", f"{base_url}/api/dashboard", token=token, timeout=10)
    dashboard_json = parse_json(dashboard)
    dashboard_shape_ok = (
        dashboard["status"] == 200
        and isinstance(dashboard_json, dict)
        and isinstance(dashboard_json.get("tasks"), list)
        and isinstance(dashboard_json.get("workers"), list)
        and "queue_depth" in dashboard_json
    )
    record(
        "dashboard_with_auth",
        dashboard_shape_ok,
        {
            "status": dashboard["status"],
            "task_count": len(dashboard_json.get("tasks") or []) if isinstance(dashboard_json, dict) else None,
            "worker_count": len(dashboard_json.get("workers") or []) if isinstance(dashboard_json, dict) else None,
            "body": dashboard_json if not dashboard_shape_ok else None,
        },
    )

    for name, path in (
        ("static_root", "/"),
        ("static_deep_link", "/tasks/console-acceptance"),
        ("static_admin_route", "/admin"),
        ("static_functions_route", "/functions"),
        ("static_submit_function_hash_route", "/submit/task?function_hash=sha256%3Aconsole-acceptance&function_name=console_acceptance_add"),
    ):
        resp = http_request("GET", f"{base_url}{path}", timeout=10)
        ok = resp["status"] == 200 and "LogServe Console" in resp.get("body", "")
        record(name, ok, {"status": resp["status"], "content_type": resp.get("content_type"), "error": resp.get("error")})

    task_body = {
        "task_name": "console_acceptance_add",
        "function_name": "add",
        "function_source": "def add(a, b):\n    return a + b\n",
        "args": [1, 2],
        "kwargs": {},
        "idempotency_key": "console-acceptance-add",
    }
    task_resp = http_request(
        "POST",
        f"{base_url}/api/tasks?wait=true&timeout_ms={max(1000, timeout_sec * 1000)}",
        token=token,
        body=task_body,
        timeout=timeout_sec + 5,
    )
    task_json = parse_json(task_resp)
    task_id = task_json.get("task_id") if isinstance(task_json, dict) else ""
    task_result = task_json.get("result_json") if isinstance(task_json, dict) else None
    task_ok = (
        task_resp["status"] == 200
        and isinstance(task_json, dict)
        and task_json.get("status") == "SUCCEEDED"
        and same_json_value(task_result, 3)
        and bool(task_id)
    )
    record(
        "submit_task_via_console_api",
        task_ok,
        {"status": task_resp["status"], "task_id": task_id, "body": task_json or task_resp.get("body"), "error": task_resp.get("error")},
    )
    details["task"] = task_json if isinstance(task_json, dict) else None

    if task_id:
        task_detail = http_request("GET", f"{base_url}/api/tasks/{task_id}", token=token, timeout=10)
        task_detail_json = parse_json(task_detail)
        detail_ok = (
            task_detail["status"] == 200
            and isinstance(task_detail_json, dict)
            and task_detail_json.get("task_id") == task_id
            and task_detail_json.get("status") == "SUCCEEDED"
        )
        record(
            "get_task_detail",
            detail_ok,
            {"status": task_detail["status"], "body": task_detail_json or task_detail.get("body"), "error": task_detail.get("error")},
        )

        def task_visible():
            listed = http_request("GET", f"{base_url}/api/tasks?q=console_acceptance_add", token=token, timeout=10)
            listed_json = parse_json(listed)
            tasks = listed_json.get("tasks") if isinstance(listed_json, dict) else []
            if not isinstance(tasks, list):
                tasks = []
            found = any(item.get("task_id") == task_id for item in tasks if isinstance(item, dict))
            return found, {"status": listed["status"], "task_count": len(tasks or []), "body": listed_json if not found else None}

        visible, visible_detail = wait_for(task_visible, timeout_sec=20, interval_sec=1)
        record("task_visible_in_dashboard_view", visible, visible_detail)

    run_id = str(int(time.time() * 1000))
    model_name = f"model-A-{run_id}"
    model_version = "v1"
    workflow_body = {
        "workflow_name": "console_acceptance_workflow",
        "definition": {
            "workflow_name": "console_acceptance_workflow",
            "steps": [
                {
                    "step_id": "first",
                    "task_name": "console_acceptance_first",
                    "function_name": "add",
                    "function_source": "def add(a, b):\n    return a + b\n",
                    "args_json": {"args": [1, 2], "kwargs": {}},
                    "depends_on": [],
                },
                {
                    "step_id": "second",
                    "task_name": "console_acceptance_second",
                    "function_name": "add",
                    "function_source": "def add(a, b):\n    return a + b\n",
                    "args_json": {"args": [2, 3], "kwargs": {}},
                    "depends_on": ["first"],
                },
            ],
            "result_step_id": "second",
            "max_attempts": 3,
            "timeout_ms": 30000,
        },
        "idempotency_key": f"console-acceptance-workflow-{run_id}",
    }
    workflow_resp = http_request(
        "POST",
        f"{base_url}/api/workflows?wait=true&timeout_ms={max(1000, timeout_sec * 1000)}",
        token=token,
        body=workflow_body,
        timeout=timeout_sec + 10,
    )
    workflow_json = parse_json(workflow_resp)
    workflow_id = workflow_json.get("workflow_id") if isinstance(workflow_json, dict) else ""
    workflow_ok = (
        workflow_resp["status"] == 200
        and isinstance(workflow_json, dict)
        and workflow_json.get("status") == "COMPLETED"
        and bool(workflow_id)
    )
    record(
        "submit_workflow_via_console_api",
        workflow_ok,
        {"status": workflow_resp["status"], "workflow_id": workflow_id, "body": workflow_json or workflow_resp.get("body"), "error": workflow_resp.get("error")},
    )
    details["workflow"] = workflow_json if isinstance(workflow_json, dict) else None

    if workflow_id:
        workflow_detail = http_request("GET", f"{base_url}/api/workflows/{workflow_id}", token=token, timeout=10)
        workflow_detail_json = parse_json(workflow_detail)
        steps = workflow_detail_json.get("steps") if isinstance(workflow_detail_json, dict) else []
        if not isinstance(steps, list):
            steps = []
        has_dependency = any(
            isinstance(step, dict) and step.get("step_id") == "second" and "first" in (step.get("depends_on") or [])
            for step in steps
        )
        record(
            "workflow_detail_has_dag_dependencies",
            workflow_detail["status"] == 200 and has_dependency,
            {"status": workflow_detail["status"], "step_count": len(steps), "body": workflow_detail_json if not has_dependency else None},
        )
        workflow_replay = http_request("POST", f"{base_url}/api/workflows/{workflow_id}/replay", token=token, timeout=10)
        workflow_replay_json = parse_json(workflow_replay)
        replay_ok = workflow_replay["status"] == 200 and isinstance(workflow_replay_json, dict) and workflow_replay_json.get("consistent_with_metadata") is True
        record(
            "workflow_replay_consistent",
            replay_ok,
            {"status": workflow_replay["status"], "body": workflow_replay_json if not replay_ok else None, "error": workflow_replay.get("error")},
        )

    model_body = {"name": model_name, "version": model_version, "adapter": "mock", "path": f"/models/{model_name}", "size_bytes": 1}
    model_resp = http_request("POST", f"{base_url}/api/models", token=token, body=model_body, timeout=10)
    model_json = parse_json(model_resp)
    model_ok = (
        model_resp["status"] == 200
        and isinstance(model_json, dict)
        and model_json.get("name") == model_name
        and model_json.get("version") == model_version
    )
    record(
        "register_model_via_console_api",
        model_ok,
        {"status": model_resp["status"], "body": model_json or model_resp.get("body"), "error": model_resp.get("error")},
    )

    llm_body = {
        "model_name": model_name,
        "model_version": model_version,
        "adapter": "mock",
        "prompt": "Summarize LogServe in one sentence.",
        "max_tokens": 32,
        "idempotency_key": f"console-acceptance-llm-{run_id}",
    }
    llm_resp = http_request(
        "POST",
        f"{base_url}/api/llm?wait=true&timeout_ms={max(1000, timeout_sec * 1000)}",
        token=token,
        body=llm_body,
        timeout=timeout_sec + 10,
    )
    llm_json = parse_json(llm_resp)
    llm_task_id = llm_json.get("task_id") if isinstance(llm_json, dict) else ""
    llm_worker_id = llm_json.get("worker_id") if isinstance(llm_json, dict) else ""
    llm_ok = llm_resp["status"] == 200 and isinstance(llm_json, dict) and llm_json.get("status") == "SUCCEEDED" and bool(llm_task_id)
    record(
        "submit_llm_via_console_api",
        llm_ok,
        {"status": llm_resp["status"], "task_id": llm_task_id, "body": llm_json or llm_resp.get("body"), "error": llm_resp.get("error")},
    )
    details["llm"] = llm_json if isinstance(llm_json, dict) else None

    if llm_task_id:
        llm_replay = http_request("POST", f"{base_url}/api/llm/{llm_task_id}/replay", token=token, timeout=10)
        llm_replay_json = parse_json(llm_replay)
        events = llm_replay_json.get("events") if isinstance(llm_replay_json, dict) else []
        if not isinstance(events, list):
            events = []
        trace_ok = (
            llm_replay["status"] == 200
            and isinstance(llm_replay_json, dict)
            and llm_replay_json.get("task_id") == llm_task_id
            and llm_replay_json.get("model_name") == model_name
            and llm_replay_json.get("model_version") == model_version
            and "total_latency_ms" in llm_replay_json
            and "first_token_ms" in llm_replay_json
            and any(isinstance(event, dict) and event.get("event_type") == "LLMCompleted" for event in events)
        )
        record(
            "llm_replay_trace_has_latency",
            trace_ok,
            {"status": llm_replay["status"], "body": llm_replay_json if not trace_ok else None, "error": llm_replay.get("error")},
        )

    def worker_has_model():
        workers = http_request("GET", f"{base_url}/api/workers", token=token, timeout=10)
        workers_json = parse_json(workers)
        rows = workers_json.get("workers") if isinstance(workers_json, dict) else []
        if not isinstance(rows, list):
            rows = []
        found = any(
            isinstance(worker, dict)
            and (not llm_worker_id or worker.get("worker_id") == llm_worker_id)
            and any(
                isinstance(model, dict)
                and model.get("name") == model_name
                and (model.get("version") or "v1") == model_version
                for model in (worker.get("cached_models") or [])
            )
            for worker in rows
        )
        return found, {"status": workers["status"], "worker_count": len(rows), "body": workers_json if not found else None}

    worker_cached, worker_detail = wait_for(worker_has_model, timeout_sec=20, interval_sec=1)
    record("worker_cache_matrix_has_model", worker_cached, worker_detail)

    actor_body = {
        "class_name": "Counter",
        "class_source": "class Counter:\n    def __init__(self, value=0):\n        self.value = value\n\n    def inc(self, by=1):\n        self.value += by\n        return self.value\n",
        "init_args": [0],
        "init_kwargs": {},
        "idempotency_key": f"console-acceptance-actor-{run_id}",
        "snapshot_every": 10,
    }
    actor_resp = http_request("POST", f"{base_url}/api/actors", token=token, body=actor_body, timeout=10)
    actor_json = parse_json(actor_resp)
    actor_id = actor_json.get("actor_id") if isinstance(actor_json, dict) else ""
    actor_ok = actor_resp["status"] == 200 and isinstance(actor_json, dict) and actor_json.get("status") == "ACTIVE" and bool(actor_id)
    record(
        "create_actor_via_console_api",
        actor_ok,
        {"status": actor_resp["status"], "actor_id": actor_id, "body": actor_json or actor_resp.get("body"), "error": actor_resp.get("error")},
    )
    details["actor"] = actor_json if isinstance(actor_json, dict) else None

    if actor_id:
        actor_call = http_request(
            "POST",
            f"{base_url}/api/actors/{actor_id}/calls",
            token=token,
            body={"method_name": "inc", "args": [1], "kwargs": {}, "timeout_ms": max(1000, timeout_sec * 1000)},
            timeout=timeout_sec + 10,
        )
        actor_call_json = parse_json(actor_call)
        actor_call_ok = actor_call["status"] == 200 and isinstance(actor_call_json, dict) and actor_call_json.get("status") == "SUCCEEDED"
        record(
            "call_actor_via_console_api",
            actor_call_ok,
            {"status": actor_call["status"], "body": actor_call_json or actor_call.get("body"), "error": actor_call.get("error")},
        )
        actor_status = http_request("GET", f"{base_url}/api/actors/{actor_id}", token=token, timeout=10)
        actor_status_json = parse_json(actor_status)
        actor_status_ok = actor_status["status"] == 200 and isinstance(actor_status_json, dict) and actor_status_json.get("actor_id") == actor_id
        record(
            "get_actor_status",
            actor_status_ok,
            {"status": actor_status["status"], "body": actor_status_json if not actor_status_ok else None, "error": actor_status.get("error")},
        )

    def list_streams(prefix):
        query = urllib.parse.urlencode({"prefix": prefix})
        resp = http_request("GET", f"{base_url}/api/logs/streams?{query}", token=token, timeout=10)
        parsed = parse_json(resp)
        ids = parsed.get("stream_ids") if isinstance(parsed, dict) else []
        if not isinstance(ids, list):
            ids = []
        return resp, parsed, ids

    def read_stream(stream_id):
        encoded = urllib.parse.quote(stream_id, safe="")
        resp = http_request("GET", f"{base_url}/api/logs/streams/{encoded}?from_seq=1&limit=50", token=token, timeout=10)
        parsed = parse_json(resp)
        records = parsed.get("records") if isinstance(parsed, dict) else []
        if not isinstance(records, list):
            records = []
        return resp, parsed, records

    def stream_read_matches(parsed, records, stream_id):
        if not isinstance(parsed, dict) or parsed.get("stream_id") != stream_id:
            return False
        stats = parsed.get("stats")
        if isinstance(stats, dict) and stats.get("stream_id") != stream_id:
            return False
        return all(isinstance(record, dict) and record.get("stream_id") == stream_id for record in records)

    def stream_has_event(records, event_type):
        return any(isinstance(record, dict) and record.get("event_type") == event_type for record in records)

    system_streams, system_streams_json, system_ids = list_streams("system:")
    system_functions_listed = "system:functions" in system_ids
    record(
        "log_streams_list_system_functions",
        system_streams["status"] == 200 and system_functions_listed,
        {"status": system_streams["status"], "stream_count": len(system_ids), "body": system_streams_json if not system_functions_listed else None},
    )
    system_read, system_read_json, system_records = read_stream("system:functions")
    system_read_ok = (
        system_read["status"] == 200
        and len(system_records) > 0
        and stream_read_matches(system_read_json, system_records, "system:functions")
        and stream_has_event(system_records, "FunctionRegistered")
    )
    record(
        "log_stream_read_system_functions",
        system_read_ok,
        {"status": system_read["status"], "record_count": len(system_records), "body": system_read_json if not system_read_ok else None},
    )

    if workflow_id:
        workflow_stream = f"wf:{workflow_id}"
        workflow_streams, workflow_streams_json, workflow_ids = list_streams("wf:")
        workflow_read, workflow_read_json, workflow_records = read_stream(workflow_stream)
        workflow_stream_ok = (
            workflow_stream in workflow_ids
            and workflow_read["status"] == 200
            and len(workflow_records) > 0
            and stream_read_matches(workflow_read_json, workflow_records, workflow_stream)
            and stream_has_event(workflow_records, "WorkflowStarted")
        )
        record(
            "log_stream_read_workflow",
            workflow_stream_ok,
            {
                "list_status": workflow_streams["status"],
                "read_status": workflow_read["status"],
                "record_count": len(workflow_records),
                "body": workflow_read_json or workflow_streams_json if not workflow_stream_ok else None,
            },
        )

    if actor_id:
        actor_stream = f"actor:{actor_id}"
        actor_streams, actor_streams_json, actor_ids = list_streams("actor:")
        actor_read, actor_read_json, actor_records = read_stream(actor_stream)
        actor_stream_ok = (
            actor_stream in actor_ids
            and actor_read["status"] == 200
            and len(actor_records) > 0
            and stream_read_matches(actor_read_json, actor_records, actor_stream)
            and stream_has_event(actor_records, "ActorCreated")
        )
        record(
            "log_stream_read_actor",
            actor_stream_ok,
            {
                "list_status": actor_streams["status"],
                "read_status": actor_read["status"],
                "record_count": len(actor_records),
                "body": actor_read_json or actor_streams_json if not actor_stream_ok else None,
            },
        )
    functions_no_auth = http_request("GET", f"{base_url}/api/functions", timeout=10)
    record(
        "functions_requires_auth",
        functions_no_auth["status"] == 401,
        {"status": functions_no_auth["status"], "body": parse_json(functions_no_auth) or functions_no_auth.get("body")},
    )

    functions = http_request("GET", f"{base_url}/api/functions", token=token, timeout=10)
    functions_json = parse_json(functions)
    function_rows = functions_json.get("functions") if isinstance(functions_json, dict) else []
    if not isinstance(function_rows, list):
        function_rows = []
    matching_function = next(
        (
            item
            for item in function_rows
            if isinstance(item, dict)
            and item.get("function_hash")
            and (item.get("entrypoint") or "").endswith("add")
        ),
        None,
    )
    function_hash = matching_function.get("function_hash") if isinstance(matching_function, dict) else ""
    functions_ok = functions["status"] == 200 and bool(function_rows) and bool(function_hash)
    record(
        "functions_list_with_auth",
        functions_ok,
        {"status": functions["status"], "function_count": len(function_rows), "body": functions_json if not functions_ok else None},
    )

    if function_hash:
        encoded_hash = urllib.parse.quote(function_hash, safe="")
        function_detail = http_request("GET", f"{base_url}/api/functions/{encoded_hash}", token=token, timeout=10)
        function_detail_json = parse_json(function_detail)
        detail_ok = (
            function_detail["status"] == 200
            and isinstance(function_detail_json, dict)
            and function_detail_json.get("function_hash") == function_hash
            and bool(function_detail_json.get("source_ref"))
            and bool(function_detail_json.get("entrypoint"))
            and bool(function_detail_json.get("language"))
        )
        record(
            "function_detail_with_auth",
            detail_ok,
            {"status": function_detail["status"], "body": function_detail_json if not detail_ok else None, "error": function_detail.get("error")},
        )

    admin_no_auth = http_request("GET", f"{base_url}/api/admin/config", timeout=10)
    record(
        "admin_config_requires_auth",
        admin_no_auth["status"] == 401,
        {"status": admin_no_auth["status"], "body": parse_json(admin_no_auth) or admin_no_auth.get("body")},
    )

    admin_backpressure_no_auth = http_request(
        "POST",
        f"{base_url}/api/admin/backpressure",
        body={"queue_high_watermark": 1024, "redelivery_timeout_ms": 30000, "log_append_slow_ms": 100},
        timeout=10,
    )
    record(
        "admin_backpressure_requires_auth",
        admin_backpressure_no_auth["status"] == 401,
        {"status": admin_backpressure_no_auth["status"], "body": parse_json(admin_backpressure_no_auth) or admin_backpressure_no_auth.get("body")},
    )

    admin = http_request("GET", f"{base_url}/api/admin/config", token=token, timeout=10)
    admin_json = parse_json(admin)
    expected_admin_fields = (
        "scheduling_policy",
        "queue_high_watermark",
        "redelivery_timeout_ms",
        "log_append_slow_ms",
        "compactable_log_records",
        "compactable_log_bytes",
    )
    admin_ok = admin["status"] == 200 and isinstance(admin_json, dict) and all(field in admin_json for field in expected_admin_fields)
    record(
        "admin_config_with_auth",
        admin_ok,
        {"status": admin["status"], "body": admin_json if not admin_ok else None, "error": admin.get("error")},
    )

    materializer_stats = admin_json.get("metadata_materializer") if isinstance(admin_json, dict) else None
    materializer_ok = isinstance(materializer_stats, dict) and bool(materializer_stats.get("mode"))
    record(
        "admin_config_has_materializer_stats",
        materializer_ok,
        {"status": admin["status"], "metadata_materializer": materializer_stats},
    )

    invalid_backpressure_cases = (
        {"queue_high_watermark": 0, "redelivery_timeout_ms": 30000, "log_append_slow_ms": 100},
        {"queue_high_watermark": 1024, "redelivery_timeout_ms": 0, "log_append_slow_ms": 100},
        {"queue_high_watermark": 1024, "redelivery_timeout_ms": 30000, "log_append_slow_ms": 0},
    )
    invalid_results = []
    for payload in invalid_backpressure_cases:
        resp = http_request("POST", f"{base_url}/api/admin/backpressure", token=token, body=payload, timeout=10)
        invalid_results.append({"status": resp["status"], "body": parse_json(resp) or resp.get("body"), "payload": payload})
    record(
        "admin_backpressure_rejects_invalid_values",
        all(item["status"] == 400 for item in invalid_results),
        {"results": invalid_results},
    )
    backpressure_body = {
        "queue_high_watermark": 1536,
        "redelivery_timeout_ms": 31000,
        "log_append_slow_ms": 110,
    }
    backpressure = http_request("POST", f"{base_url}/api/admin/backpressure", token=token, body=backpressure_body, timeout=10)
    backpressure_json = parse_json(backpressure)
    post_ok = backpressure["status"] == 200 and isinstance(backpressure_json, dict) and all(
        backpressure_json.get(key) == value for key, value in backpressure_body.items()
    )
    record(
        "admin_backpressure_update_with_auth",
        post_ok,
        {"status": backpressure["status"], "body": backpressure_json or backpressure.get("body"), "error": backpressure.get("error")},
    )

    admin_after = http_request("GET", f"{base_url}/api/admin/config", token=token, timeout=10)
    admin_after_json = parse_json(admin_after)
    reflected_ok = admin_after["status"] == 200 and isinstance(admin_after_json, dict) and all(
        admin_after_json.get(key) == value for key, value in backpressure_body.items()
    )
    record(
        "admin_config_reflects_backpressure_update",
        reflected_ok,
        {"status": admin_after["status"], "body": admin_after_json if not reflected_ok else None, "error": admin_after.get("error")},
    )
    return {
        "verdict": "PASS" if not failures else "FAIL",
        "checks": checks,
        "failures": failures,
        "details": details,
    }


def main(argv=None):
    parser = argparse.ArgumentParser(description="Probe the LogServe Console HTTP surface.")
    parser.add_argument("--base-url", required=True)
    parser.add_argument("--token", required=True)
    parser.add_argument("--timeout-sec", type=int, default=30)
    parser.add_argument("--out", required=True)
    args = parser.parse_args(argv)

    result = run_probe(args.base_url, args.token, args.timeout_sec)
    out = Path(args.out)
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(json.dumps(result, indent=2, ensure_ascii=False), encoding="utf-8")
    print(out)
    return 0 if result["verdict"] == "PASS" else 1


if __name__ == "__main__":
    raise SystemExit(main())
