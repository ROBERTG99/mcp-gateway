#!/usr/bin/env python3
"""Gate evalgate.report.v1 reports against thresholds and baselines (evalgate check, v0.1.0 rule).

Used by `make eval` and CI because the evalgate action tag `v0.1.0` does not exist yet
(docs/DECISIONS.md). For every gated report in --reports (`<suite>.json`, the test partition):

- a metric fails `threshold` if its value misses the threshold in its direction;
- with a baseline report of the same name, a metric fails `regression` if its delta is worse than
  --max-regression-pp AND the 95% paired-bootstrap CI of the delta excludes 0 on the worse side.
  Cases are paired by id; the metric is recomputed from per-case scores (METRICS below).
  Metrics without a per-case definition (latencies, chaos counts) are gated on threshold only.

Exit codes: 0 passed, 1 gate failed, 2 invalid input. --out-json writes an evalgate.check.v1 document.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import random
import sys
from collections.abc import Callable
from pathlib import Path
from typing import Any

Scores = dict[str, float]
Ratio = tuple[Callable[[Scores], float], Callable[[Scores], float]]

# (suite, metric) -> (numerator, denominator) per case; the metric is sum(num) / sum(den).
METRICS: dict[tuple[str, str], Ratio] = {
    ("policy.v1", "decision_accuracy"): (lambda s: s["decision_correct"], lambda s: 1.0),
    ("injection.v1", "recall"): (lambda s: s["flagged"] * s["malicious"], lambda s: s["malicious"]),
    ("injection.v1", "false_positive_rate"): (
        lambda s: s["flagged"] * (1 - s["malicious"]),
        lambda s: 1 - s["malicious"],
    ),
    ("redaction.v1", "recall"): (lambda s: s["spans_removed"], lambda s: s["spans"]),
    ("redaction.v1", "precision"): (lambda s: s["true_markers"], lambda s: s["markers"]),
}

REQUIRED = ("schema", "suite", "metrics", "cases")


class InvalidInput(Exception):
    """A report is missing or malformed."""


def load(path: Path) -> dict[str, Any]:
    try:
        doc = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as err:
        raise InvalidInput(f"{path}: {err}") from err
    if not isinstance(doc, dict) or doc.get("schema") != "evalgate.report.v1":
        raise InvalidInput(f"{path}: not an evalgate.report.v1 document")
    missing = [k for k in REQUIRED if k not in doc]
    if missing:
        raise InvalidInput(f"{path}: missing {', '.join(missing)}")
    return doc


def sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def ratio(cases: list[Scores], fn: Ratio) -> float:
    num = sum(fn[0](c) for c in cases)
    den = sum(fn[1](c) for c in cases)
    return num / den if den else 0.0


def paired_delta_ci(
    base: dict[str, Scores], cand: dict[str, Scores], fn: Ratio, resamples: int, seed: int
) -> tuple[float, float, int]:
    """95% percentile CI (in pp) of candidate minus baseline over cases paired by id."""
    ids = sorted(set(base) & set(cand))
    if not ids:
        return 0.0, 0.0, 0
    rng = random.Random(seed)
    deltas = []
    for _ in range(resamples):
        sample = [rng.choice(ids) for _ in ids]
        deltas.append((ratio([cand[i] for i in sample], fn) - ratio([base[i] for i in sample], fn)) * 100)
    deltas.sort()
    lo = deltas[int(0.025 * (resamples - 1))]
    hi = deltas[int(0.975 * (resamples - 1))]
    return lo, hi, len(ids)


def worse(direction: str, value: float, threshold: float) -> bool:
    return value > threshold if direction == "lower" else value < threshold


def check_report(
    path: Path, baseline_dir: Path, max_pp: float, resamples: int, seed: int
) -> tuple[list[dict[str, Any]], list[dict[str, Any]], str | None]:
    doc = load(path)
    suite = doc["suite"]
    failures: list[dict[str, Any]] = []
    comparisons: list[dict[str, Any]] = []
    base_path = baseline_dir / path.name
    base = load(base_path) if base_path.exists() else None
    for name, m in sorted(doc["metrics"].items()):
        value, threshold, direction = float(m["value"]), float(m["threshold"]), m["direction"]
        if worse(direction, value, threshold):
            failures.append(
                {
                    "metric": f"{suite}:{name}",
                    "reason": "threshold",
                    "value": value,
                    "threshold": threshold,
                    "delta_pp": None,
                    "ci_low_pp": None,
                    "ci_high_pp": None,
                }
            )
        fn = METRICS.get((suite, name))
        if base is None or fn is None or name not in base["metrics"]:
            continue
        b_cases = {c["id"]: c["scores"] for c in base["cases"]}
        c_cases = {c["id"]: c["scores"] for c in doc["cases"]}
        b_value, c_value = float(base["metrics"][name]["value"]), value
        delta_pp = (c_value - b_value) * 100
        lo, hi, n = paired_delta_ci(b_cases, c_cases, fn, resamples, seed)
        comparisons.append(
            {
                "metric": f"{suite}:{name}",
                "baseline": b_value,
                "candidate": c_value,
                "delta_pp": round(delta_pp, 4),
                "ci_low_pp": round(lo, 4),
                "ci_high_pp": round(hi, 4),
                "n_paired": n,
            }
        )
        if direction == "lower":
            regressed = delta_pp > max_pp and lo > 0
        else:
            regressed = delta_pp < -max_pp and hi < 0
        if regressed:
            failures.append(
                {
                    "metric": f"{suite}:{name}",
                    "reason": "regression",
                    "value": c_value,
                    "threshold": threshold,
                    "delta_pp": round(delta_pp, 4),
                    "ci_low_pp": round(lo, 4),
                    "ci_high_pp": round(hi, 4),
                }
            )
    return failures, comparisons, sha256(base_path) if base else None


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--reports", type=Path, default=Path("evals/reports"))
    ap.add_argument("--baseline", type=Path, default=Path("evals/baseline"))
    ap.add_argument("--max-regression-pp", type=float, default=1.0)
    ap.add_argument("--resamples", type=int, default=10000)
    ap.add_argument("--seed", type=int, default=1337)
    ap.add_argument("--out-json", type=Path)
    args = ap.parse_args(argv)

    reports = sorted(p for p in args.reports.glob("*.json") if not p.name.endswith(".dev.json"))
    if not reports:
        print(f"evalgate_check: no reports in {args.reports}", file=sys.stderr)
        return 2
    all_failures: list[dict[str, Any]] = []
    checks = []
    try:
        for path in reports:
            failures, comparisons, base_sha = check_report(
                path, args.baseline, args.max_regression_pp, args.resamples, args.seed
            )
            all_failures += failures
            checks.append(
                {
                    "schema": "evalgate.check.v1",
                    "report_sha256": sha256(path),
                    "baseline_sha256": base_sha,
                    "max_regression_pp": args.max_regression_pp,
                    "seed": args.seed,
                    "resamples": args.resamples,
                    "passed": not failures,
                    "failures": failures,
                    "comparisons": comparisons,
                }
            )
            for c in comparisons:
                print(
                    f"{c['metric']}: baseline {c['baseline']:.4f} candidate {c['candidate']:.4f} "
                    f"delta {c['delta_pp']:+.2f} pp [{c['ci_low_pp']:+.2f}, {c['ci_high_pp']:+.2f}] "
                    f"n={c['n_paired']}"
                )
    except InvalidInput as err:
        print(f"evalgate_check: {err}", file=sys.stderr)
        return 2
    if args.out_json:
        args.out_json.write_text(json.dumps(checks, indent=2) + "\n", encoding="utf-8")
    for f in all_failures:
        print(
            f"FAIL {f['metric']}: {f['reason']} (value {f['value']}, threshold {f['threshold']}, "
            f"delta {f['delta_pp']} pp)",
            file=sys.stderr,
        )
    print(f"evalgate_check: {len(reports)} reports, {len(all_failures)} failures")
    return 1 if all_failures else 0


if __name__ == "__main__":
    sys.exit(main())
