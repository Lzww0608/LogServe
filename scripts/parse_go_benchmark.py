#!/usr/bin/env python3
# Convert `go test -bench` text output into a compact JSON metric map.
import json
import re
import sys
from pathlib import Path

# Matches stable benchmark metric columns emitted by Go's testing package.
# Optional B/op and allocs/op groups keep the parser usable without -benchmem.
LINE = re.compile(
    r"^(?P<name>Benchmark\S+)(?:/(?P<case>[^/]+))*\s+\d+\s+"
    r"(?P<ns_per_op>[\d.]+)\s+ns/op(?:\s+(?P<bytes_per_op>\d+)\s+B/op)?"
    r"(?:\s+(?P<allocs_per_op>\d+)\s+allocs/op)?"
)


# Return benchmark rows keyed by benchmark name plus optional subcase.
# Non-benchmark lines are ignored so verbose test output can be piped in directly.
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
        # Test failure/detail sections after a benchmark should not be
        # associated with the previous successful metric row.
        if current and line.startswith("---"):
            current = None
    return out


# Read one benchmark text file and print normalized JSON to stdout.
def main():
    if len(sys.argv) != 2:
        print("usage: parse_go_benchmark.py <bench.txt>", file=sys.stderr)
        return 2
    text = Path(sys.argv[1]).read_text(encoding="utf-8", errors="replace")
    print(json.dumps(parse_benchmark_text(text), indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
