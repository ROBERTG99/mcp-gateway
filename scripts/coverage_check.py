#!/usr/bin/env python3
"""Fail when Go line coverage is under the spec §10 thresholds (make coverage).

    coverage_check.py coverage.out [--core-min 85] [--overall-min 70]

Reads a `go test -coverprofile` file written with `-coverpkg=./internal/...,./pkg/...`. A block that
appears several times (one entry per test binary) counts once and is covered if any run covered it.
Coverage is statements covered / statements, per package and overall. Core packages must each reach
--core-min; all packages together must reach --overall-min. Test helpers (internal/testutil,
internal/testupstream) are not product code and are left out.
"""

from __future__ import annotations

import argparse
import re
import sys
from pathlib import Path

MODULE = "github.com/ROBERTG99/mcp-gateway/"
CORE = [
    "internal/pipeline",
    "internal/policy",
    "internal/idempotency",
    "internal/audit",
    "internal/redact",
    "internal/injection",
    "internal/authn",
    "internal/approvals",
]
EXCLUDED = ("internal/testutil", "internal/testupstream")
LINE = re.compile(r"^(\S+\.go):(\d+\.\d+,\d+\.\d+) (\d+) (\d+)$")

Block = tuple[str, str]  # (file, range)


def parse(text: str) -> dict[Block, tuple[int, bool]]:
    """Return {(file, range): (statements, covered)} with duplicate blocks merged."""
    blocks: dict[Block, tuple[int, bool]] = {}
    for line in text.splitlines():
        m = LINE.match(line.strip())
        if not m:
            continue
        key = (m.group(1), m.group(2))
        stmts, covered = int(m.group(3)), int(m.group(4)) > 0
        prev = blocks.get(key)
        blocks[key] = (stmts, covered or (prev is not None and prev[1]))
    return blocks


def package_of(file: str) -> str:
    return file.removeprefix(MODULE).rsplit("/", 1)[0]


def per_package(blocks: dict[Block, tuple[int, bool]]) -> dict[str, tuple[int, int]]:
    """Return {package: (covered statements, statements)} without the test helpers."""
    out: dict[str, tuple[int, int]] = {}
    for (file, _), (stmts, covered) in blocks.items():
        pkg = package_of(file)
        if pkg.startswith(EXCLUDED):
            continue
        cov, tot = out.get(pkg, (0, 0))
        out[pkg] = (cov + (stmts if covered else 0), tot + stmts)
    return out


def pct(cov: int, tot: int) -> float:
    return 100.0 * cov / tot if tot else 0.0


def check(
    blocks: dict[Block, tuple[int, bool]], core_min: float, overall_min: float
) -> tuple[list[str], bool]:
    return check_packages(per_package(blocks), core_min, overall_min)


def check_packages(
    pkgs: dict[str, tuple[int, int]], core_min: float, overall_min: float
) -> tuple[list[str], bool]:
    lines, ok = [], True
    for pkg in CORE:
        if pkg not in pkgs:
            lines.append(f"FAIL {pkg}: missing from the profile (budget >= {core_min:g}%)")
            ok = False
            continue
        p = pct(*pkgs[pkg])
        good = p >= core_min
        ok &= good
        lines.append(f"{'ok  ' if good else 'FAIL'} {pkg}: {p:.1f}% (core, budget >= {core_min:g}%)")
    cov = sum(c for c, _ in pkgs.values())
    tot = sum(t for _, t in pkgs.values())
    p = pct(cov, tot)
    good = p >= overall_min
    ok &= good
    lines.append(
        f"{'ok  ' if good else 'FAIL'} overall: {p:.1f}% of {tot} statements (budget >= {overall_min:g}%)"
    )
    return lines, ok


def main(argv: list[str]) -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("profile", help="go test -coverprofile output")
    ap.add_argument("--core-min", type=float, default=85.0)
    ap.add_argument("--overall-min", type=float, default=70.0)
    args = ap.parse_args(argv)
    pkgs = per_package(parse(Path(args.profile).read_text(encoding="utf-8")))
    lines, ok = check_packages(pkgs, args.core_min, args.overall_min)
    print("coverage (statements):")
    for line in lines:
        print("  " + line)
    lowest = sorted(pkgs.items(), key=lambda kv: pct(*kv[1]))[:6]
    print("lowest packages: " + ", ".join(f"{k} {pct(*v):.0f}%" for k, v in lowest))
    return 0 if ok else 1


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
