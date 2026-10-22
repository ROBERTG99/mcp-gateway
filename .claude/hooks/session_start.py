#!/usr/bin/env python3
"""SessionStart hook.

Only acts on Claude Code on the web (`CLAUDE_CODE_REMOTE=true`). Idempotent: configures the Docker
registry mirror, starts dockerd if it is not running, installs the pinned tools into ./bin and syncs
Python and pnpm dependencies. `MCPGW_HOOK_DRY_RUN=1` prints the plan without executing it.
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
import time
from pathlib import Path

DAEMON_JSON = Path("/etc/docker/daemon.json")
MIRROR = {"registry-mirrors": ["https://mirror.gcr.io"]}


def project_dir() -> Path:
    return Path(os.environ.get("CLAUDE_PROJECT_DIR") or os.getcwd()).resolve()


def sh(cmd: list[str] | str, dry: bool, cwd: Path | None = None, timeout: int = 900) -> bool:
    label = cmd if isinstance(cmd, str) else " ".join(cmd)
    print(f"session_start: {label}")
    if dry:
        return True
    try:
        res = subprocess.run(
            cmd,
            cwd=cwd,
            shell=isinstance(cmd, str),
            capture_output=True,
            text=True,
            timeout=timeout,
            check=False,
        )
    except (OSError, subprocess.SubprocessError) as exc:
        print(f"session_start: failed: {exc}", file=sys.stderr)
        return False
    if res.returncode != 0:
        print(f"session_start: exit {res.returncode}: {res.stderr.strip()[-400:]}", file=sys.stderr)
    return res.returncode == 0


def docker_ready() -> bool:
    if shutil.which("docker") is None:
        return False
    return subprocess.run(["docker", "info"], capture_output=True, check=False).returncode == 0


def ensure_docker(dry: bool) -> None:
    print("session_start: docker (registry mirror + daemon)")
    if dry:
        return
    if not DAEMON_JSON.exists():
        try:
            DAEMON_JSON.parent.mkdir(parents=True, exist_ok=True)
            DAEMON_JSON.write_text(json.dumps(MIRROR), encoding="utf-8")
        except OSError as exc:
            print(f"session_start: cannot write {DAEMON_JSON}: {exc}", file=sys.stderr)
    if docker_ready() or shutil.which("dockerd") is None:
        return
    with open("/tmp/dockerd.log", "ab") as log:
        subprocess.Popen(["dockerd"], stdout=log, stderr=log, start_new_session=True)
    for _ in range(30):
        if docker_ready():
            return
        time.sleep(1)
    print("session_start: dockerd did not become ready in 30 s", file=sys.stderr)


def main() -> int:
    if os.environ.get("CLAUDE_CODE_REMOTE") != "true":
        print("session_start: skipped (not Claude Code on the web)")
        return 0
    dry = os.environ.get("MCPGW_HOOK_DRY_RUN") == "1"
    root = project_dir()
    ensure_docker(dry)
    print("session_start: tools (make tools)")
    sh(["make", "-s", "tools"], dry, cwd=root)
    sh(["uv", "sync", "--frozen"], dry, cwd=root)
    if (root / "e2e" / "package.json").exists():
        sh(["pnpm", "install", "--frozen-lockfile"], dry, cwd=root / "e2e")
    return 0


if __name__ == "__main__":
    sys.exit(main())
