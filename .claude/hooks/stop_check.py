#!/usr/bin/env python3
"""Stop hook: run `make check-fast` before the agent stops.

On failure, block once with a short summary of the failing lines. When `stop_hook_active` is true
(the agent is already continuing because of this hook) exit 0 so the session can end.
`MCPGW_STOP_CHECK_CMD` overrides the command (used by the tests).
"""

from __future__ import annotations

import json
import os
import subprocess
import sys

MAX_SUMMARY_LINES = 25


def summarize(output: str) -> str:
    lines = [line for line in output.splitlines() if line.strip()]
    interesting = [
        line
        for line in lines
        if any(word in line for word in ("FAIL", "Error", "error", "---", "panic", "gofmt needed", ".go:"))
    ]
    picked = interesting or lines
    return "\n".join(picked[-MAX_SUMMARY_LINES:])


def main() -> int:
    try:
        payload = json.load(sys.stdin)
    except json.JSONDecodeError:
        payload = {}
    if payload.get("stop_hook_active"):
        return 0
    command = os.environ.get("MCPGW_STOP_CHECK_CMD", "make check-fast")
    cwd = os.environ.get("CLAUDE_PROJECT_DIR") or os.getcwd()
    try:
        result = subprocess.run(
            command, shell=True, cwd=cwd, capture_output=True, text=True, timeout=280, check=False
        )
    except subprocess.TimeoutExpired:
        print(
            json.dumps({"decision": "block", "reason": f"`{command}` timed out; investigate before stopping"})
        )
        return 0
    if result.returncode != 0:
        reason = f"`{command}` failed (exit {result.returncode}). Fix before stopping:\n" + summarize(
            result.stdout + "\n" + result.stderr
        )
        print(json.dumps({"decision": "block", "reason": reason}))
    return 0


if __name__ == "__main__":
    sys.exit(main())
