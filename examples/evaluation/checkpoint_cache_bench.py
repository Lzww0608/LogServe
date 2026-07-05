# Lightweight checkpoint-cache benchmark for multiple model ids. It reports
# cold/warm replay timings without enforcing pass/fail validation rules.
import json
import os
import sys
import time

from logserve import register_model, replay_llm, submit_llm


# snapshot keeps only timing fields needed for the benchmark summary and accepts
# both snake_case and camelCase replay shapes.
def snapshot(replay):
    return {
        "cache_hit": bool(replay.get("cache_hit") or replay.get("cacheHit")),
        "checkpoint_fetch_ms": int(replay.get("checkpoint_fetch_ms") or replay.get("checkpointFetchMs") or 0),
        "model_load_ms": int(replay.get("model_load_ms") or replay.get("modelLoadMs") or 0),
        "total_latency_ms": int(replay.get("total_latency_ms") or replay.get("totalLatencyMs") or 0),
    }


# probe_model registers one mock checkpoint model and compares first-use versus
# repeated-use replay metadata for that model id.
def probe_model(model, version):
    register_model(model, version=version, size_bytes=0, path=f"checkpoint://{model}", adapter="mock")
    cold_task = submit_llm(model, "cold", version=version, adapter="mock")
    warm_task = submit_llm(model, "warm", version=version, adapter="mock")
    return {
        "cold": snapshot(replay_llm(cold_task["task_id"])),
        "warm": snapshot(replay_llm(warm_task["task_id"])),
    }


# main reads the model list from the environment so scripts can scale the cache
# benchmark without editing this example file.
def main():
    models = [x.strip() for x in os.getenv("LOGSERVE_CHECKPOINT_MODELS", "model-D,model-E").split(",") if x.strip()]
    version = os.getenv("LOGSERVE_CHECKPOINT_VERSION", "v1")
    report = {"models": {}, "started_at_ms": int(time.time() * 1000)}
    for model in models:
        report["models"][model] = probe_model(model, version)
    report["finished_at_ms"] = int(time.time() * 1000)
    print(json.dumps(report, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
