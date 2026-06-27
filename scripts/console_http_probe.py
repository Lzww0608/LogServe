#!/usr/bin/env python3
import argparse
import json
import sys
import time
import urllib.error
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

    for name, path in (("static_root", "/"), ("static_deep_link", "/tasks/console-acceptance")):
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

    admin = http_request("GET", f"{base_url}/api/admin/config", token=token, timeout=10)
    admin_json = parse_json(admin)
    admin_ok = admin["status"] == 200 and isinstance(admin_json, dict) and "scheduling_policy" in admin_json
    record(
        "admin_config_with_auth",
        admin_ok,
        {"status": admin["status"], "body": admin_json if not admin_ok else None, "error": admin.get("error")},
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
