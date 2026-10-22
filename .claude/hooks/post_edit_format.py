#!/usr/bin/env python3
"""PostToolUse formatter for Write and Edit.

gofmt and goimports for Go, prettier for files under e2e/, `mcp-gateway policy fmt` for Rego.
Each formatter is optional: when it is not installed the hook does nothing. Always exits 0.
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
from pathlib import Path


def project_dir() -> Path:
    return Path(os.environ.get("CLAUDE_PROJECT_DIR") or os.getcwd()).resolve()


def find(tool: str) -> str | None:
    local = project_dir() / "bin" / tool
    if local.is_file() and os.access(local, os.X_OK):
        return str(local)
    return shutil.which(tool)


def run(cmd: list[str], cwd: Path | None = None) -> None:
    try:
        subprocess.run(cmd, cwd=cwd, capture_output=True, timeout=60, check=False)
    except (OSError, subprocess.SubprocessError) as exc:
        print(f"post_edit_format: {cmd[0]} failed: {exc}", file=sys.stderr)


def format_file(path: Path) -> None:
    root = project_dir()
    if path.suffix == ".go":
        if gofmt := find("gofmt"):
            run([gofmt, "-w", str(path)])
        if goimports := find("goimports"):
            run([goimports, "-local", "github.com/ROBERTG99/mcp-gateway", "-w", str(path)])
    elif path.suffix == ".rego":
        if gw := find("mcp-gateway"):
            run([gw, "policy", "fmt", str(path)])
    elif path.suffix in (".ts", ".js", ".json", ".css") and "e2e" in path.parts:
        prettier = root / "e2e" / "node_modules" / ".bin" / "prettier"
        if prettier.exists():
            run([str(prettier), "--write", str(path)], cwd=root / "e2e")


def main() -> int:
    try:
        payload = json.load(sys.stdin)
        file_path = (payload.get("tool_input") or {}).get("file_path", "")
    except (json.JSONDecodeError, AttributeError):
        return 0
    if file_path:
        path = Path(file_path)
        if path.is_file():
            format_file(path)
    return 0


if __name__ == "__main__":
    sys.exit(main())
