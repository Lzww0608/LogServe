import json
import os
import sys

from logserve import register_model, replay_llm, submit_llm


def pick(data, snake, camel):
    if snake in data:
        return data[snake]
    return data.get(camel)


def pick_bool(data, snake, camel):
    return bool(pick(data, snake, camel))


def pick_int(data, snake, camel):
    return int(pick(data, snake, camel) or 0)


def snapshot(replay):
    return {
        "cache_hit": pick_bool(replay, "cache_hit", "cacheHit"),
        "checkpoint_fetch_ms": pick_int(replay, "checkpoint_fetch_ms", "checkpointFetchMs"),
        "cache_used_bytes": pick_int(replay, "cache_used_bytes", "cacheUsedBytes"),
        "cache_capacity_bytes": pick_int(replay, "cache_capacity_bytes", "cacheCapacityBytes"),
        "eviction_count": pick_int(replay, "eviction_count", "evictionCount"),
        "model_load_ms": pick_int(replay, "model_load_ms", "modelLoadMs"),
        "first_token_ms": pick_int(replay, "first_token_ms", "firstTokenMs"),
        "total_latency_ms": pick_int(replay, "total_latency_ms", "totalLatencyMs"),
        "worker_id": pick(replay, "worker_id", "workerId") or "",
    }


def validation_errors(report):
    errors = []
    cold = report["cold"]
    warm = report["warm"]
    if cold["cache_hit"]:
        errors.append("cold request unexpectedly hit cache; run the probe with a fresh model or clean model-cache dir")
    if not warm["cache_hit"]:
        errors.append("warm request did not hit cache")
    if cold["cache_used_bytes"] <= 0 and warm["cache_used_bytes"] <= 0:
        errors.append("checkpoint cache was not populated; check worker --model-source-dir and --model-cache-dir")
    if cold["cache_capacity_bytes"] <= 0 and warm["cache_capacity_bytes"] <= 0:
        errors.append("checkpoint cache capacity was not reported; check worker --model-cache-capacity-bytes")
    if cold["checkpoint_fetch_ms"] <= 0:
        errors.append("cold request did not report checkpoint fetch latency")
    return errors


def main():
    model = os.getenv("LOGSERVE_CHECKPOINT_MODEL", "model-D")
    version = os.getenv("LOGSERVE_CHECKPOINT_VERSION", "v1")
    register_model(model, version=version, size_bytes=0, path=f"checkpoint://{model}", adapter="mock")
    cold = submit_llm(model, "checkpoint cold probe", version=version, adapter="mock")
    warm = submit_llm(model, "checkpoint warm probe", version=version, adapter="mock")
    report = {
        "model": model,
        "version": version,
        "cold_task_id": cold["task_id"],
        "warm_task_id": warm["task_id"],
        "cold": snapshot(replay_llm(cold["task_id"])),
        "warm": snapshot(replay_llm(warm["task_id"])),
    }
    report["validation_errors"] = validation_errors(report)
    print(json.dumps(report, indent=2, ensure_ascii=False))
    if report["validation_errors"]:
        print(
            "checkpoint cache probe failed: " + "; ".join(report["validation_errors"]),
            file=sys.stderr,
        )
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
