"""Tests for scripts/evalgate_check.py (the eval gate used by `make eval`)."""

from __future__ import annotations

import json
import sys
from pathlib import Path
from typing import Any

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "scripts"))

import evalgate_check  # noqa: E402

REPO = Path(__file__).resolve().parents[2]


def report(suite: str, metrics: dict[str, Any], cases: list[dict[str, Any]]) -> dict[str, Any]:
    return {
        "schema": "evalgate.report.v1",
        "suite": suite,
        "run_id": "r",
        "created_at": "2026-10-17T00:00:00Z",
        "git_sha": "x",
        "mode": "offline",
        "models": [],
        "prompt_hashes": {},
        "n_cases": len(cases),
        "metrics": metrics,
        "cost_usd_total": 0,
        "cost_usd_per_case": 0,
        "latency_p50_ms": 0,
        "latency_p95_ms": 0,
        "cases": cases,
    }


def policy_report(correct: int, total: int = 150) -> dict[str, Any]:
    cases = [
        {
            "id": f"c{i}",
            "split": "test",
            "scores": {"decision_correct": 1.0 if i < correct else 0.0},
            "passed": i < correct,
            "cost_usd": 0,
            "latency_ms": 0,
            "error": None,
        }
        for i in range(total)
    ]
    value = correct / total
    return report(
        "policy.v1",
        {
            "decision_accuracy": {
                "value": value,
                "threshold": 0.9,
                "direction": "higher",
                "passed": value >= 0.9,
                "ci_low": value,
                "ci_high": value,
            }
        },
        cases,
    )


def write(dir_: Path, name: str, doc: dict[str, Any]) -> None:
    dir_.mkdir(parents=True, exist_ok=True)
    (dir_ / name).write_text(json.dumps(doc), encoding="utf-8")


def run(tmp: Path, *extra: str) -> int:
    return evalgate_check.main(
        ["--reports", str(tmp / "reports"), "--baseline", str(tmp / "baseline"), "--resamples", "500", *extra]
    )


def test_fails_below_threshold(tmp_path: Path) -> None:
    write(tmp_path / "reports", "policy.v1.json", policy_report(120))  # 0.80 < 0.90
    out = tmp_path / "check.json"
    assert run(tmp_path, "--out-json", str(out)) == 1
    assert [f["reason"] for f in json.loads(out.read_text())[0]["failures"]] == ["threshold"]


def test_passes_at_threshold_without_baseline(tmp_path: Path) -> None:
    write(tmp_path / "reports", "policy.v1.json", policy_report(150))
    assert run(tmp_path) == 0


def test_forced_regression_fails(tmp_path: Path) -> None:
    # 150 -> 140 correct of 150 is a 6.7 pp drop, still above the 0.9 threshold: only the
    # baseline comparison can fail it (evalgate FR-7).
    write(tmp_path / "baseline", "policy.v1.json", policy_report(150))
    write(tmp_path / "reports", "policy.v1.json", policy_report(140))
    out = tmp_path / "check.json"
    assert run(tmp_path, "--out-json", str(out)) == 1
    check = json.loads(out.read_text())[0]
    assert check["schema"] == "evalgate.check.v1" and not check["passed"]
    assert [f["reason"] for f in check["failures"]] == ["regression"]
    assert check["comparisons"][0]["n_paired"] == 150


def test_small_drop_within_noise_passes(tmp_path: Path) -> None:
    write(tmp_path / "baseline", "policy.v1.json", policy_report(150))
    write(tmp_path / "reports", "policy.v1.json", policy_report(149))  # 0.67 pp < 1.0 pp
    assert run(tmp_path) == 0


def test_dev_reports_are_not_gated(tmp_path: Path) -> None:
    write(tmp_path / "reports", "policy.v1.json", policy_report(150))
    write(tmp_path / "reports", "policy.v1.dev.json", policy_report(10))
    assert run(tmp_path) == 0


@pytest.mark.parametrize("doc", [{"schema": "other"}, {"schema": "evalgate.report.v1", "suite": "x"}])
def test_invalid_input_exits_2(tmp_path: Path, doc: dict[str, Any]) -> None:
    write(tmp_path / "reports", "bad.json", doc)
    assert run(tmp_path) == 2


def test_committed_reports_pass_against_committed_baseline() -> None:
    assert (
        evalgate_check.main(
            [
                "--reports",
                str(REPO / "evals/reports"),
                "--baseline",
                str(REPO / "evals/baseline"),
                "--resamples",
                "300",
            ]
        )
        == 0
    )


def committed(name: str) -> dict[str, Any]:
    doc: dict[str, Any] = json.loads((REPO / "evals/reports" / name).read_text(encoding="utf-8"))
    return doc


def test_forced_regression_on_committed_reports_fails(tmp_path: Path) -> None:
    """One benign test case newly flagged (FPR 0 -> 1/40 = 2.5 pp, still under the 0.05
    threshold) fails as a regression against the committed baseline."""
    doc = committed("injection.v1.json")
    benign = [c for c in doc["cases"] if c["scores"]["malicious"] == 0][:1]
    for c in benign:
        c["scores"]["flagged"] = 1.0
    n_benign = sum(1 for c in doc["cases"] if c["scores"]["malicious"] == 0)
    fpr = len(benign) / n_benign
    doc["metrics"]["false_positive_rate"]["value"] = fpr  # 1/40 = 0.025: below the 0.05 threshold
    write(tmp_path / "reports", "injection.v1.json", doc)
    write(tmp_path / "baseline", "injection.v1.json", committed("injection.v1.json"))
    out = tmp_path / "check.json"
    assert run(tmp_path, "--out-json", str(out)) == 1
    reasons = {f["reason"] for f in json.loads(out.read_text())[0]["failures"]}
    assert reasons == {"regression"}


def test_missing_report_or_metric_fails(tmp_path: Path) -> None:
    write(tmp_path / "baseline", "policy.v1.json", policy_report(150))
    write(tmp_path / "baseline", "redaction.v1.json", committed("redaction.v1.json"))
    write(tmp_path / "reports", "policy.v1.json", policy_report(150))
    assert run(tmp_path) == 1  # redaction report missing
    doc = committed("redaction.v1.json")
    del doc["metrics"]["recall"]
    write(tmp_path / "reports", "redaction.v1.json", doc)
    assert run(tmp_path) == 1  # recall metric missing


def test_reported_value_must_match_cases(tmp_path: Path) -> None:
    doc = committed("injection.v1.json")
    for c in doc["cases"]:
        c["scores"]["flagged"] = 0.0  # nothing detected, but the reported recall is unchanged
    write(tmp_path / "reports", "injection.v1.json", doc)
    out = tmp_path / "check.json"
    assert run(tmp_path, "--out-json", str(out)) == 1
    assert "inconsistent" in {f["reason"] for f in json.loads(out.read_text())[0]["failures"]}
