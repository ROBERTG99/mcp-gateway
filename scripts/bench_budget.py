#!/usr/bin/env python3
"""Fail when a benchmark or load-test result is over its budget.

    bench_budget.py bench.out                 # go test -bench output (make bench)
    bench_budget.py --load loadtest.json      # loadgen --mode load report (make loadtest)

Budgets are the NFRs; with --ci (or CI=true in the environment) the CI budgets apply
instead: overhead p95 < 10 ms and >= 500 calls/s. A budgeted benchmark missing from the output is
a failure, so a renamed or skipped benchmark cannot pass silently.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
from dataclasses import dataclass
from pathlib import Path


@dataclass(frozen=True)
class Budget:
    name: str  # benchmark name (without the -N suffix) or load-report key
    unit: str  # ReportMetric unit or report field
    limit: float
    ci_limit: float
    lower_is_better: bool
    nfr: str


BENCH_BUDGETS = [
    Budget("BenchmarkGatewayOverhead", "overhead-p95-ms", 5.0, 10.0, True, "NFR-1"),
    Budget("BenchmarkAuditPage", "p95-ms", 300.0, 300.0, True, "NFR-8"),
]

LOAD_BUDGETS = [
    Budget("load", "calls_per_second", 2000.0, 500.0, False, "NFR-2"),
    Budget("load", "rss_max_mb", 128.0, 128.0, True, "NFR-7"),
    Budget("load", "error_ratio", 0.0, 0.0, True, "NFR-2"),
]

LINE = re.compile(r"^(Benchmark\S+?)(?:-\d+)?\s+\d+\s+(.*)$")


def parse_bench(text: str) -> dict[str, dict[str, float]]:
    """Return {benchmark: {unit: value}} from `go test -bench` output."""
    out: dict[str, dict[str, float]] = {}
    for line in text.splitlines():
        m = LINE.match(line.strip())
        if not m:
            continue
        fields = m.group(2).split()
        metrics = out.setdefault(m.group(1), {})
        for value, unit in zip(fields[0::2], fields[1::2], strict=False):
            try:
                metrics[unit] = float(value)
            except ValueError:
                continue
    return out


def check(budgets: list[Budget], results: dict[str, dict[str, float]], ci: bool) -> list[str]:
    """Return one line per budget; failures start with FAIL."""
    lines = []
    for b in budgets:
        limit = b.ci_limit if ci else b.limit
        value = results.get(b.name, {}).get(b.unit)
        op = "<=" if b.unit == "error_ratio" else ("<" if b.lower_is_better else ">=")
        if value is None:
            lines.append(f"FAIL {b.nfr} {b.name} {b.unit}: missing (budget {op} {limit:g})")
            continue
        if b.unit == "error_ratio":
            ok = value <= limit
        else:
            ok = value < limit if b.lower_is_better else value >= limit
        status = "ok  " if ok else "FAIL"
        lines.append(f"{status} {b.nfr} {b.name} {b.unit} = {value:g} (budget {op} {limit:g})")
    return lines


def main(argv: list[str]) -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("bench", nargs="?", help="go test -bench output")
    ap.add_argument("--load", help="loadgen --mode load JSON report")
    ap.add_argument(
        "--ci",
        action="store_true",
        default=os.environ.get("CI", "").lower() == "true",
        help="apply the CI budgets (default when CI=true)",
    )
    args = ap.parse_args(argv)
    if not args.bench and not args.load:
        ap.error("give a bench output file, --load, or both")
    lines: list[str] = []
    if args.bench:
        lines += check(BENCH_BUDGETS, parse_bench(Path(args.bench).read_text(encoding="utf-8")), args.ci)
    if args.load:
        report = json.loads(Path(args.load).read_text(encoding="utf-8"))
        numbers = {k: float(v) for k, v in report.items() if isinstance(v, int | float)}
        lines += check(LOAD_BUDGETS, {"load": numbers}, args.ci)
    print(f"budgets ({'CI' if args.ci else 'NFR'}):")
    for line in lines:
        print("  " + line)
    return 1 if any(line.startswith("FAIL") for line in lines) else 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
