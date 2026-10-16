#!/usr/bin/env python3
"""Update one task row in PROGRESS.md: progress.py T-02 done "evidence text"."""

from __future__ import annotations

import re
import sys
from pathlib import Path


def main(argv: list[str]) -> int:
    if len(argv) != 4:
        print("usage: progress.py T-NN status evidence", file=sys.stderr)
        return 2
    task, status, evidence = argv[1], argv[2], argv[3]
    path = Path(__file__).resolve().parents[1] / "PROGRESS.md"
    text = path.read_text(encoding="utf-8")
    pattern = re.compile(rf"^\| {re.escape(task)} \| (.+?) \| (M\d) \| [^|]* \| .*\|$", re.M)
    match = pattern.search(text)
    if match is None:
        print(f"{task} not found", file=sys.stderr)
        return 1
    row = f"| {task} | {match.group(1)} | {match.group(2)} | {status} | {evidence.replace('|', '/')} |"
    path.write_text(text[: match.start()] + row + text[match.end() :], encoding="utf-8")
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
