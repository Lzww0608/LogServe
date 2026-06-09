import json
import os

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
    }


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
    print(json.dumps(report, indent=2, ensure_ascii=False))


if __name__ == "__main__":
    main()
