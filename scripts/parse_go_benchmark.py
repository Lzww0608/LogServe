#!/usr/bin/env python3
import json
import re
import sys
from pathlib import Path

LINE = re.compile(
    r"^(?P<name>Benchmark\S+)(?:/(?P<case>[^/]+))*\s+\d+\s+"
    r"(?P<ns_per_op>[\d.]+)\s+ns/op(?:\s+(?P<bytes_per_op>\d+)\s+B/op)?"
    r"(?:\s+(?P<allocs_per_op>\d+)\s+allocs/op)?"
)


def parse_benchmark_text(text):
    out = {}
    current = None
    for raw in text.splitlines():
        line = raw.strip()
        if line.startswith("Benchmark"):
            m = LINE.match(line)
            if not m:
                continue
            name = m.group("name")
            case = m.group("case") or ""
            key = f"{name}/{case}" if case else name
            out[key] = {
                "ns_per_op": float(m.group("ns_per_op")),
                "bytes_per_op": int(m.group("bytes_per_op") or 0),
                "allocs_per_op": int(m.group("allocs_per_op") or 0),
            }
            current = key
            continue
        if current and line.startswith("---"):
            current = None
    return out


def main():
    if len(sys.argv) != 2:
        print("usage: parse_go_benchmark.py <bench.txt>", file=sys.stderr)
        return 2
    text = Path(sys.argv[1]).read_text(encoding="utf-8", errors="replace")
    print(json.dumps(parse_benchmark_text(text), indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
