"""Tests for scripts/coverage_check.py (make coverage, spec §10 coverage gate)."""

from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "scripts"))

import coverage_check  # noqa: E402

M = "github.com/ROBERTG99/mcp-gateway"


def profile(*lines: str) -> str:
    return "mode: atomic\n" + "\n".join(lines) + "\n"


def test_duplicate_blocks_count_once_and_covered_if_any_run_covered_them() -> None:
    text = profile(
        f"{M}/internal/audit/a.go:1.1,2.1 4 0",
        f"{M}/internal/audit/a.go:1.1,2.1 4 7",  # same block from another test binary
        f"{M}/internal/audit/a.go:3.1,4.1 1 0",
    )
    pkgs = coverage_check.per_package(coverage_check.parse(text))
    assert pkgs["internal/audit"] == (4, 5)


def test_thresholds_core_and_overall() -> None:
    text = profile(
        f"{M}/internal/pipeline/p.go:1.1,2.1 9 1",
        f"{M}/internal/pipeline/p.go:3.1,4.1 1 0",  # 90%
        f"{M}/internal/admin/a.go:1.1,2.1 3 1",
        f"{M}/internal/admin/a.go:3.1,4.1 7 0",  # 30%
    )
    lines, ok = coverage_check.check(coverage_check.parse(text), core_min=85, overall_min=70)
    assert not ok  # overall 12/20 = 60%
    assert any(line.startswith("ok") and "internal/pipeline" in line for line in lines)
    assert any(line.startswith("FAIL") and "overall" in line for line in lines)


def test_core_package_below_threshold_fails_and_missing_core_fails() -> None:
    text = profile(f"{M}/internal/redact/r.go:1.1,2.1 8 1", f"{M}/internal/redact/r.go:3.1,4.1 2 0")  # 80%
    lines, ok = coverage_check.check(coverage_check.parse(text), core_min=85, overall_min=70)
    assert not ok
    assert any(line.startswith("FAIL") and "internal/redact" in line for line in lines)
    assert any(line.startswith("FAIL") and "internal/authn" in line and "missing" in line for line in lines)


def test_test_helpers_are_excluded() -> None:
    text = profile(
        f"{M}/internal/testutil/stack/s.go:1.1,2.1 100 0",
        f"{M}/internal/testupstream/e.go:1.1,2.1 100 0",
        f"{M}/internal/admin/a.go:1.1,2.1 10 1",
    )
    assert set(coverage_check.per_package(coverage_check.parse(text))) == {"internal/admin"}
