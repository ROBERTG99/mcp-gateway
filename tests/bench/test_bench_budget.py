"""Tests for scripts/bench_budget.py (make bench, make loadtest)."""

from __future__ import annotations

import json
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "scripts"))

import bench_budget  # noqa: E402

BENCH_OK = "\n".join(
    [
        "goos: linux",
        "BenchmarkGatewayOverhead-4   \t    2000\t    227037 ns/op\t"
        "         0.1680 overhead-p50-ms\t         0.4640 overhead-p95-ms",
        "ok  \tgithub.com/ROBERTG99/mcp-gateway/internal/pipeline\t0.494s",
        "BenchmarkAuditPage-4   \t    2000\t   3267250 ns/op\t         8.147 p95-ms",
    ]
)


@pytest.fixture(autouse=True)
def _no_ci_env(monkeypatch: pytest.MonkeyPatch) -> None:
    """Keep the NFR/CI choice explicit: GitHub Actions sets CI=true, which flips --ci's default."""
    monkeypatch.delenv("CI", raising=False)


def write(tmp_path: Path, name: str, text: str) -> str:
    p = tmp_path / name
    p.write_text(text, encoding="utf-8")
    return str(p)


def test_parse_bench_reads_custom_metrics() -> None:
    got = bench_budget.parse_bench(BENCH_OK)
    assert got["BenchmarkGatewayOverhead"]["overhead-p95-ms"] == pytest.approx(0.464)
    assert got["BenchmarkAuditPage"]["p95-ms"] == pytest.approx(8.147)
    assert got["BenchmarkAuditPage"]["ns/op"] == pytest.approx(3267250)


def test_within_budget_passes(tmp_path: Path) -> None:
    assert bench_budget.main([write(tmp_path, "b.out", BENCH_OK)]) == 0


def test_over_budget_fails(tmp_path: Path) -> None:
    slow = BENCH_OK.replace("0.4640 overhead-p95-ms", "5.2000 overhead-p95-ms")
    assert bench_budget.main([write(tmp_path, "b.out", slow)]) == 1


def test_ci_budget_is_looser_for_overhead(tmp_path: Path) -> None:
    ci_ok = BENCH_OK.replace("0.4640 overhead-p95-ms", "7.0000 overhead-p95-ms")
    path = write(tmp_path, "b.out", ci_ok)
    assert bench_budget.main([path]) == 1
    assert bench_budget.main([path, "--ci"]) == 0


def test_ci_env_selects_ci_budgets(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    ci_ok = BENCH_OK.replace("0.4640 overhead-p95-ms", "7.0000 overhead-p95-ms")
    monkeypatch.setenv("CI", "true")
    assert bench_budget.main([write(tmp_path, "b.out", ci_ok)]) == 0


def test_missing_benchmark_fails(tmp_path: Path) -> None:
    only_overhead = "\n".join(line for line in BENCH_OK.splitlines() if "AuditPage" not in line)
    assert bench_budget.main([write(tmp_path, "b.out", only_overhead)]) == 1


@pytest.mark.parametrize(
    ("report", "ci", "want"),
    [
        ({"calls_per_second": 2100.0, "rss_max_mb": 90.0, "error_ratio": 0.0}, False, 0),
        ({"calls_per_second": 643.0, "rss_max_mb": 87.0, "error_ratio": 0.0}, False, 1),
        ({"calls_per_second": 643.0, "rss_max_mb": 87.0, "error_ratio": 0.0}, True, 0),
        ({"calls_per_second": 643.0, "rss_max_mb": 130.0, "error_ratio": 0.0}, True, 1),
        ({"calls_per_second": 2100.0, "rss_max_mb": 90.0, "error_ratio": 0.001}, False, 1),
        ({"calls_per_second": 2100.0, "error_ratio": 0.0}, False, 1),
    ],
)
def test_load_budgets(tmp_path: Path, report: dict[str, float], ci: bool, want: int) -> None:
    path = write(tmp_path, "load.json", json.dumps(report))
    args = ["--load", path] + (["--ci"] if ci else [])
    assert bench_budget.main(args) == want
