import json
import statistics
import time

from logserve import (
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


@task
def ping(value: int) -> int:
    return value + 1


@task
def embed(query: str) -> str:
    return "vec:" + query


@task
def search(vec: str) -> list[str]:
    return ["doc:" + vec]


@task
def build_prompt(query: str, docs: list[str]) -> str:
    return "answer " + query + " using " + docs[0]


@workflow
def rag_with_llm(query: str):
    vec = embed(query)
    docs = search(vec)
    prompt = build_prompt(query, docs)
    return llm_generate("model-A", prompt, version="v1", adapter="mock", step_id="generate_answer")


@actor(snapshot_every=5)
class SnapshotCounter:
    def __init__(self):
        self.value = 0

    def inc(self):
        self.value += 1
        return self.value

    def get(self):
        return self.value


@actor(snapshot_every=100000)
class NoSnapshotCounter:
    def __init__(self):
        self.value = 0

    def inc(self):
        self.value += 1
        return self.value

    def get(self):
        return self.value


def ms(fn):
    start = time.perf_counter()
    value = fn()
    return value, int((time.perf_counter() - start) * 1000)


def percentile(values, pct):
    if not values:
        return 0
    values = sorted(values)
    idx = max(0, min(len(values) - 1, int((len(values) * pct + 99) / 100) - 1))
    return values[idx]


def pick(data, snake, camel):
    if snake in data:
        return data[snake]
    return data.get(camel)


def pick_bool(data, snake, camel):
    return bool(pick(data, snake, camel))


def pick_int(data, snake, camel):
    return int(pick(data, snake, camel) or 0)


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


def actor_snapshot_ablation(commands):
    with_snapshot = create_actor(SnapshotCounter, snapshot_every=5)
    without_snapshot = create_actor(NoSnapshotCounter, snapshot_every=100000)
    for _ in range(commands):
        with_snapshot.inc()
        without_snapshot.inc()
    with_snapshot.get()
    without_snapshot.get()
    snap = replay_actor(with_snapshot.actor_id)
    no_snap = replay_actor(without_snapshot.actor_id)
    return {
        "commands": commands,
        "snapshot_enabled": {
            "full_replay_commands": pick(snap, "full_replay_commands", "fullReplayCommands"),
            "snapshot_replay_commands": pick(snap, "snapshot_replay_commands", "snapshotReplayCommands"),
        },
        "snapshot_disabled": {
            "full_replay_commands": pick(no_snap, "full_replay_commands", "fullReplayCommands"),
            "snapshot_replay_commands": pick(no_snap, "snapshot_replay_commands", "snapshotReplayCommands"),
        },
    }


def llm_cold_start():
    cold = submit_llm("model-C", "cold start probe", version="v1", adapter="mock")
    cold_replay = replay_llm(cold["task_id"])
    warm = submit_llm("model-C", "warm cache probe", version="v1", adapter="mock")
    warm_replay = replay_llm(warm["task_id"])
    return {
        "cold": {
            "cache_hit": pick_bool(cold_replay, "cache_hit", "cacheHit"),
            "model_load_ms": pick_int(cold_replay, "model_load_ms", "modelLoadMs"),
            "first_token_ms": pick_int(cold_replay, "first_token_ms", "firstTokenMs"),
            "total_latency_ms": pick_int(cold_replay, "total_latency_ms", "totalLatencyMs"),
        },
        "warm": {
            "cache_hit": pick_bool(warm_replay, "cache_hit", "cacheHit"),
            "model_load_ms": pick_int(warm_replay, "model_load_ms", "modelLoadMs"),
            "first_token_ms": pick_int(warm_replay, "first_token_ms", "firstTokenMs"),
            "total_latency_ms": pick_int(warm_replay, "total_latency_ms", "totalLatencyMs"),
        },
    }


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


def main():
    register_model("model-A", version="v1", size_bytes=100, path="mock://model-A", adapter="mock")
    register_model("model-C", version="v1", size_bytes=100, path="mock://model-C", adapter="mock")
    set_scheduling_policy("LOCALITY_AWARE")
    report = {
        "workflow_latency": workflow_latency(3),
        "task_throughput": task_throughput(8),
        "actor_recovery_snapshot_ablation": actor_snapshot_ablation(20),
        "llm_cold_start": llm_cold_start(),
        "locality_ablation": locality_ablation(6),
        "replay_ablation": {
            "enabled": "workflow/actor/llm state can be reconstructed from log streams",
            "disabled": "no independent recovery validation; dashboard marks this as analysis-only baseline",
        },
    }
    print(json.dumps(report, indent=2, ensure_ascii=False))


if __name__ == "__main__":
    main()
