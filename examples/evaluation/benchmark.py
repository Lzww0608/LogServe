# End-to-end benchmark driver for local LogServe runs. It exercises tasks,
# workflows, actor replay, LLM cache behavior, scheduler locality, and executor
# function-registry payload reduction from one JSON report.
import hashlib
import importlib.util
import json
import os
import statistics
from pathlib import Path
import time

from logserve import (
    LogServeClient,
    actor,
    create_actor,
    llm_generate,
    register_model,
    replay_actor,
    replay_llm,
    set_scheduling_policy,
    submit,
    submit_llm,
    task,
    workflow,
)


# ping is the smallest possible remote task used for task throughput timing.
@task
def ping(value: int) -> int:
    return value + 1


# embed is deterministic so workflow timing includes orchestration, not model work.
@task
def embed(query: str) -> str:
    return "vec:" + query


# search returns one synthetic document to keep workflow shape stable.
@task
def search(vec: str) -> list[str]:
    return ["doc:" + vec]


# build_prompt separates ordinary task cost from the final LLM step.
@task
def build_prompt(query: str, docs: list[str]) -> str:
    return "answer " + query + " using " + docs[0]


# rag_with_llm is the workflow benchmark target: three normal tasks followed by
# one mock LLM generation step with replayable model metadata.
@workflow
def rag_with_llm(query: str):
    vec = embed(query)
    docs = search(vec)
    prompt = build_prompt(query, docs)
    return llm_generate("model-A", prompt, version="v1", adapter="mock", step_id="generate_answer")


# SnapshotCounter snapshots frequently so replay can trim most actor commands in
# the snapshot ablation benchmark.
@actor(snapshot_every=5)
class SnapshotCounter:
    # __init__ initializes the actor state captured by snapshot metadata.
    def __init__(self):
        self.value = 0

    # inc mutates state once per benchmark command and returns the observable count.
    def inc(self):
        self.value += 1
        return self.value

    # get forces a final read command so replay sees the settled actor value.
    def get(self):
        return self.value


# NoSnapshotCounter effectively disables snapshots for the same command count,
# giving a full-replay comparison against SnapshotCounter.
@actor(snapshot_every=100000)
class NoSnapshotCounter:
    # __init__ mirrors SnapshotCounter so the ablation changes only snapshot policy.
    def __init__(self):
        self.value = 0

    # inc matches SnapshotCounter's command behavior for apples-to-apples replay cost.
    def inc(self):
        self.value += 1
        return self.value

    # get returns the final counter value after all replay-baseline commands.
    def get(self):
        return self.value


# ms wraps a callable and returns both its value and elapsed wall-clock time in milliseconds.
def ms(fn):
    start = time.perf_counter()
    value = fn()
    return value, int((time.perf_counter() - start) * 1000)


# percentile uses a simple nearest-rank style index so small benchmark samples
# still choose an observed latency instead of interpolating synthetic values.
def percentile(values, pct):
    if not values:
        return 0
    values = sorted(values)
    # The +99 integer math rounds up for percentile ranks while staying in-bounds.
    idx = max(0, min(len(values) - 1, int((len(values) * pct + 99) / 100) - 1))
    return values[idx]


# pick accepts both snake_case SDK fields and camelCase transport fields from replay APIs.
def pick(data, snake, camel):
    if snake in data:
        return data[snake]
    return data.get(camel)


# pick_bool normalizes optional replay flags such as cache_hit/cacheHit.
def pick_bool(data, snake, camel):
    return bool(pick(data, snake, camel))


# pick_int turns missing numeric replay metrics into zero for report stability.
def pick_int(data, snake, camel):
    return int(pick(data, snake, camel) or 0)


# env_int reads positive integer knobs and falls back for absent, malformed, or non-positive values.
def env_int(name, default):
    value = os.getenv(name)
    if not value:
        return default
    try:
        parsed = int(value)
    except ValueError:
        return default
    return parsed if parsed > 0 else default


# task_throughput submits independent ping tasks and reports aggregate TPS plus tail latency.
def task_throughput(n):
    latencies = []
    start = time.perf_counter()
    for i in range(n):
        _, elapsed = ms(lambda i=i: submit(ping, i))
        latencies.append(elapsed)
    total = time.perf_counter() - start
    return {
        "requests": n,
        "throughput_tps": round(n / total, 2) if total else 0,
        "p95_latency_ms": percentile(latencies, 95),
        "p99_latency_ms": percentile(latencies, 99),
    }


# workflow_latency times the RAG+LLM workflow path and asserts mock LLM output
# so failed model routing does not look like a successful latency sample.
def workflow_latency(n):
    latencies = []
    for i in range(n):
        result, elapsed = ms(lambda i=i: submit(rag_with_llm, f"hello-{i}"))
        assert "mock:model-A" in result
        latencies.append(elapsed)
    return {
        "requests": n,
        "median_ms": int(statistics.median(latencies)),
        "p95_ms": percentile(latencies, 95),
        "p99_ms": percentile(latencies, 99),
    }


# actor_snapshot_ablation compares replay metadata for identical actors with and
# without frequent snapshots, then adds dashboard compaction counters.
def actor_snapshot_ablation(commands):
    with_snapshot = create_actor(SnapshotCounter, snapshot_every=5)
    without_snapshot = create_actor(NoSnapshotCounter, snapshot_every=100000)
    for _ in range(commands):
        with_snapshot.inc()
        without_snapshot.inc()
    # Final reads force both actors to append a terminal command before replay metrics are collected.
    with_snapshot.get()
    without_snapshot.get()
    snap = replay_actor(with_snapshot.actor_id)
    no_snap = replay_actor(without_snapshot.actor_id)
    dashboard = LogServeClient().transport.run("dashboard-snapshot")
    return {
        "commands": commands,
        "snapshot_enabled": {
            "full_replay_commands": pick(snap, "full_replay_commands", "fullReplayCommands"),
            "snapshot_replay_commands": pick(snap, "snapshot_replay_commands", "snapshotReplayCommands"),
            "trimmed_replay_commands": pick(snap, "snapshot_replay_commands", "snapshotReplayCommands"),
        },
        "snapshot_disabled": {
            "full_replay_commands": pick(no_snap, "full_replay_commands", "fullReplayCommands"),
            "snapshot_replay_commands": pick(no_snap, "snapshot_replay_commands", "snapshotReplayCommands"),
        },
        "compactable_log_records": pick_int(dashboard, "compactable_log_records", "compactableLogRecords"),
        "compactable_log_bytes": pick_int(dashboard, "compactable_log_bytes", "compactableLogBytes"),
    }


# llm_cold_start submits two requests for the same model to expose cold-load and
# warm-cache replay fields in one report section.
def llm_cold_start():
    cold = submit_llm("model-C", "cold start probe", version="v1", adapter="mock")
    cold_replay = replay_llm(cold["task_id"])
    warm = submit_llm("model-C", "warm cache probe", version="v1", adapter="mock")
    warm_replay = replay_llm(warm["task_id"])
    return {
        "cold": {
            "cache_hit": pick_bool(cold_replay, "cache_hit", "cacheHit"),
            "model_load_ms": pick_int(cold_replay, "model_load_ms", "modelLoadMs"),
            "checkpoint_fetch_ms": pick_int(cold_replay, "checkpoint_fetch_ms", "checkpointFetchMs"),
            "first_token_ms": pick_int(cold_replay, "first_token_ms", "firstTokenMs"),
            "total_latency_ms": pick_int(cold_replay, "total_latency_ms", "totalLatencyMs"),
            "cache_used_bytes": pick_int(cold_replay, "cache_used_bytes", "cacheUsedBytes"),
            "cache_capacity_bytes": pick_int(cold_replay, "cache_capacity_bytes", "cacheCapacityBytes"),
            "eviction_count": pick_int(cold_replay, "eviction_count", "evictionCount"),
        },
        "warm": {
            "cache_hit": pick_bool(warm_replay, "cache_hit", "cacheHit"),
            "model_load_ms": pick_int(warm_replay, "model_load_ms", "modelLoadMs"),
            "checkpoint_fetch_ms": pick_int(warm_replay, "checkpoint_fetch_ms", "checkpointFetchMs"),
            "first_token_ms": pick_int(warm_replay, "first_token_ms", "firstTokenMs"),
            "total_latency_ms": pick_int(warm_replay, "total_latency_ms", "totalLatencyMs"),
            "cache_used_bytes": pick_int(warm_replay, "cache_used_bytes", "cacheUsedBytes"),
            "cache_capacity_bytes": pick_int(warm_replay, "cache_capacity_bytes", "cacheCapacityBytes"),
            "eviction_count": pick_int(warm_replay, "eviction_count", "evictionCount"),
        },
    }


# locality_ablation runs the same LLM request mix under each scheduler policy so
# cache hit rate, queue wait, and SLO violations are comparable by policy.
def locality_ablation(requests):
    out = {}
    slo_ms = 250
    for policy in ("RESOURCE_ONLY", "LOCALITY_AWARE", "PREDICTED_LATENCY"):
        set_scheduling_policy(policy)
        hits = 0
        cold_starts = 0
        latencies = []
        queue_waits = []
        slo_violations = 0
        for i in range(requests):
            submitted, elapsed = ms(lambda i=i: submit_llm("model-A", f"locality-{policy}-{i}", version="v1", adapter="mock"))
            replay = replay_llm(submitted["task_id"])
            hits += 1 if pick_bool(replay, "cache_hit", "cacheHit") else 0
            cold_starts += 0 if pick_bool(replay, "cache_hit", "cacheHit") else 1
            total_latency = pick_int(replay, "total_latency_ms", "totalLatencyMs")
            queue_waits.append(max(0, elapsed - total_latency))
            latencies.append(elapsed)
            if elapsed > slo_ms:
                slo_violations += 1
        out[policy.lower()] = {
            "requests": requests,
            "cache_hit_rate": round(hits / requests, 3),
            "cold_starts": cold_starts,
            "cold_start_rate": round(cold_starts / requests, 3),
            "p50_latency_ms": percentile(latencies, 50),
            "p95_latency_ms": percentile(latencies, 95),
            "p99_latency_ms": percentile(latencies, 99),
            "p95_queue_wait_ms": percentile(queue_waits, 95),
            "p99_queue_wait_ms": percentile(queue_waits, 99),
            "slo_ms": slo_ms,
            "slo_violation_rate": round(slo_violations / requests, 3),
        }
    return out


# env_counts parses comma-separated positive integers for executor ablation sizes.
def env_counts(name, default):
    value = os.getenv(name)
    if not value:
        return default
    out = []
    for item in value.split(","):
        try:
            parsed = int(item.strip())
        except ValueError:
            continue
        if parsed > 0:
            out.append(parsed)
    return out or default


# function_registry_executor_ablation bypasses the control plane and calls the
# Python executor directly to isolate payload-size and compile-cache effects.
def function_registry_executor_ablation(counts):
    executor = load_python_executor()
    source = "".join(
        f"# module padding line {i:03d}: repeated source for registry benchmark\n"
        for i in range(200)
    ) + "def registry_ping(value):\n    return value + 1\n"
    # The hash matches the executor's function registry key, so only the first
    # registry request needs to include source text.
    function_hash = "sha256:" + hashlib.sha256(source.encode("utf-8")).hexdigest()
    out = {}
    for count in counts:
        legacy_requests = [
            {
                "function_source": source,
                "function_name": "registry_ping",
                "args_json": {"args": [i], "kwargs": {}},
            }
            for i in range(count)
        ]
        registry_requests = []
        for i in range(count):
            request = {
                "function_hash": function_hash,
                "function_name": "registry_ping",
                "args_json": {"args": [i], "kwargs": {}},
            }
            if i == 0:
                request["function_source"] = source
            registry_requests.append(request)

        # Clear the private executor cache before each mode so timing compares
        # payload strategy rather than leftover compiled code.
        executor._FUNCTION_CODE_CACHE.clear()
        _, legacy_ms = ms(lambda requests=legacy_requests: run_executor_requests(executor, requests))
        executor._FUNCTION_CODE_CACHE.clear()
        _, registry_ms = ms(lambda requests=registry_requests: run_executor_requests(executor, requests))
        legacy_payload_bytes = sum(len(json.dumps(req, separators=(",", ":"))) for req in legacy_requests)
        registry_payload_bytes = sum(len(json.dumps(req, separators=(",", ":"))) for req in registry_requests)
        out[str(count)] = {
            "requests": count,
            "legacy_payload_bytes": legacy_payload_bytes,
            "registry_payload_bytes": registry_payload_bytes,
            "payload_reduction_bytes": legacy_payload_bytes - registry_payload_bytes,
            "legacy_direct_executor_ms": legacy_ms,
            "registry_direct_executor_ms": registry_ms,
        }
    return out


# run_executor_requests executes synthetic task requests through the executor's
# in-process API and fails fast on the first non-ok response.
def run_executor_requests(executor, requests):
    results = []
    for request in requests:
        response = executor.handle_task(request)
        if not response.get("ok"):
            raise RuntimeError(response.get("error", "executor request failed"))
        results.append(response.get("result"))
    return results


# load_python_executor imports executor/python/server.py by path so benchmarks
# use the current checkout instead of an installed package.
def load_python_executor():
    path = Path(__file__).resolve().parents[2] / "executor" / "python" / "server.py"
    spec = importlib.util.spec_from_file_location("logserve_python_executor_benchmark", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


# main registers mock models, runs each benchmark section, and prints the JSON report.
def main():
    register_model("model-A", version="v1", size_bytes=100, path="mock://model-A", adapter="mock")
    register_model("model-C", version="v1", size_bytes=100, path="mock://model-C", adapter="mock")
    set_scheduling_policy("LOCALITY_AWARE")
    report = {
        "workflow_latency": workflow_latency(env_int("LOGSERVE_BENCH_WORKFLOWS", 3)),
        "task_throughput": task_throughput(env_int("LOGSERVE_BENCH_TASKS", 8)),
        "actor_recovery_snapshot_ablation": actor_snapshot_ablation(env_int("LOGSERVE_BENCH_ACTOR_COMMANDS", 20)),
        "llm_cold_start": llm_cold_start(),
        "locality_ablation": locality_ablation(env_int("LOGSERVE_BENCH_LLM_REQUESTS", 6)),
        "function_registry_executor_ablation": function_registry_executor_ablation(
            env_counts("LOGSERVE_BENCH_FUNCTION_COUNTS", [1, 100, 1000])
        ),
        "replay_ablation": {
            "enabled": "workflow/actor/llm state can be reconstructed from log streams",
            "disabled": "no independent recovery validation; dashboard marks this as analysis-only baseline",
        },
    }
    print(json.dumps(report, indent=2, ensure_ascii=False))


if __name__ == "__main__":
    main()
