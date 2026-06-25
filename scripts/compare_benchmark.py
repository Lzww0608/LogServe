#!/usr/bin/env python3
import json
import os
import sys
from pathlib import Path


def load(path):
    if not path.exists():
        return {}
    return json.loads(path.read_text(encoding="utf-8"))


def compare(baseline, current, max_ratio):
    regressions = []
    for key, base in sorted(baseline.items()):
        if key not in current:
            continue
        cur = current[key]
        base_ns = base.get("ns_per_op")
        cur_ns = cur.get("ns_per_op")
        if not base_ns or not cur_ns:
            continue
        ratio = cur_ns / base_ns
        if ratio > max_ratio:
            regressions.append(
                {
                    "benchmark": key,
                    "baseline_ns_per_op": base_ns,
                    "current_ns_per_op": cur_ns,
                    "ratio": ratio,
                }
            )
    return regressions


def main():
    if len(sys.argv) < 3:
        print(
            "usage: compare_benchmark.py <baseline.json> <current.json> [max_ratio]",
            file=sys.stderr,
        )
        return 2
    baseline_path = Path(sys.argv[1])
    current_path = Path(sys.argv[2])
    max_ratio = float(sys.argv[3] if len(sys.argv) > 3 else os.getenv("LOGSERVE_BENCH_MAX_RATIO", "1.15"))
    baseline = load(baseline_path)
    current = load(current_path)
    regressions = compare(baseline, current, max_ratio)
    report = {
        "baseline": str(baseline_path),
        "current": str(current_path),
        "max_ratio": max_ratio,
        "regressions": regressions,
        "verdict": "pass" if not regressions else "fail",
    }
    print(json.dumps(report, indent=2))
    return 1 if regressions else 0


if __name__ == "__main__":
    raise SystemExit(main())
