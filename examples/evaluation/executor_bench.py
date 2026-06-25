import hashlib
import importlib.util
import json
import os
import sys
import time
from pathlib import Path


def load_executor():
    path = Path(__file__).resolve().parents[2] / "executor" / "python" / "server.py"
    spec = importlib.util.spec_from_file_location("logserve_python_executor_bench", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def module_source(padding_lines):
    return "".join(
        f"# padding {i:04d}\n" for i in range(padding_lines)
    ) + "def bench_echo(value):\n    return value\n"


def run_case(executor, source, function_hash, count, compile_cache):
    requests = []
    for i in range(count):
        req = {
            "function_hash": function_hash,
            "function_name": "bench_echo",
            "args_json": {"args": [i], "kwargs": {}},
        }
        if compile_cache or i == 0:
            req["function_source"] = source
        requests.append(req)
    if not compile_cache:
        executor._FUNCTION_CODE_CACHE.clear()
    start = time.perf_counter()
    for req in requests:
        resp = executor.handle_task(req)
        if not resp.get("ok"):
            raise RuntimeError(resp.get("error", "executor failed"))
    elapsed_ms = (time.perf_counter() - start) * 1000
    return elapsed_ms


def main():
    executor = load_executor()
    padding = int(os.getenv("LOGSERVE_EXECUTOR_PADDING_LINES", "200"))
    counts = [int(x) for x in os.getenv("LOGSERVE_EXECUTOR_COUNTS", "1,100,1000").split(",") if x.strip()]
    source = module_source(padding)
    function_hash = "sha256:" + hashlib.sha256(source.encode("utf-8")).hexdigest()
    report = {"padding_lines": padding, "cases": {}}
    for count in counts:
        executor._FUNCTION_CODE_CACHE.clear()
        cold_ms = run_case(executor, source, function_hash, count, compile_cache=False)
        warm_ms = run_case(executor, source, function_hash, count, compile_cache=True)
        report["cases"][str(count)] = {
            "requests": count,
            "compile_cache_off_ms": cold_ms,
            "compile_cache_on_ms": warm_ms,
            "speedup_ratio": cold_ms / warm_ms if warm_ms > 0 else None,
        }
    print(json.dumps(report, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
