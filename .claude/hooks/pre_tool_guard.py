#!/usr/bin/env python3
"""PreToolUse guard for Bash, Write and Edit (spec §7.2).

Blocks (exit 2 with a reason on stderr) the never-do actions of the build: terraform apply/destroy,
force pushes and tag pushes, git identity changes, AI attribution trailers in commits or pull
requests, repository creation or visibility changes, messages to real recipients, reads of .env
files other than .env.example, and edits to existing files in evals/datasets/test/.
Fails open on its own errors (malformed input) so a hook bug never wedges the session.
"""

from __future__ import annotations

import json
import os
import re
import shlex
import sys
from pathlib import Path

ATTRIBUTION = re.compile(r"co-authored-by|claude-session|generated with \[?claude code", re.I)

BASH_RULES: list[tuple[re.Pattern[str], str]] = [
    (re.compile(r"\bterraform\s+(?:\S+\s+)*?(apply|destroy)\b"), "terraform apply/destroy is never run"),
    (re.compile(r"\bgit\s+push\b[^;&|]*\s(--force(?:-with-lease)?|-f)\b"), "force-push is forbidden"),
    (
        re.compile(r"\bgit\s+push\b[^;&|]*(--tags\b|\sv\d+\.\d+\.\d+\b|refs/tags/)"),
        "tags are pushed by the maintainer only",
    ),
    (re.compile(r"\bgit\s+config\b[^;&|]*\buser\.(name|email|signingkey)\b"), "never change git identity"),
    (
        re.compile(r"(^|[\s;&|(])(export\s+)?GIT_(AUTHOR|COMMITTER)_(NAME|EMAIL|DATE)\s*="),
        "never change GIT_AUTHOR_*/GIT_COMMITTER_* variables",
    ),
    (
        re.compile(r"\bgh\s+repo\s+(create|delete|rename|archive|fork)\b"),
        "never create or alter repositories",
    ),
    (re.compile(r"\bgh\s+repo\s+edit\b[^;&|]*--visibility"), "never change repository visibility"),
    (re.compile(r"\bgh\s+release\s+create\b"), "releases are published by the maintainer"),
    (
        re.compile(r"(hooks\.slack\.com|api\.twilio\.com|api\.sendgrid\.com|smtp\.|api\.mailgun\.net)"),
        "never send messages to real recipients",
    ),
    (re.compile(r"(^|[\s;&|(])(sendmail|mailx?|mutt)\s"), "never send messages to real recipients"),
    (
        re.compile(
            r"\b(gcloud\s+\S+.*\bdeploy|aws\s+cloudformation\s+(deploy|create-stack|update-stack)|"
            r"az\s+deployment|cdk\s+deploy|serverless\s+deploy|fly\s+deploy)\b"
        ),
        "never deploy to a cloud account",
    ),
]

ENV_TOKEN = re.compile(r"(?<![\w./-])(?:\./)?\.env(?:\.[\w.-]+)?(?![\w/])")


def project_dir() -> Path:
    return Path(os.environ.get("CLAUDE_PROJECT_DIR") or os.getcwd()).resolve()


def block(reason: str) -> int:
    print(f"pre_tool_guard: blocked: {reason}", file=sys.stderr)
    return 2


def env_file_reads(command: str) -> list[str]:
    hits = []
    for match in ENV_TOKEN.finditer(command):
        token = match.group(0).removeprefix("./")
        if token == ".env.example":
            continue
        # `cp .env.example .env` creates the file; that is a write, not a read.
        if re.search(r"\bcp\s+(\./)?\.env\.example\s+(\./)?\.env\b", command) and token == ".env":
            continue
        hits.append(token)
    return hits


def commit_message_files(command: str) -> list[Path]:
    try:
        parts = shlex.split(command)
    except ValueError:
        return []
    files = []
    for i, part in enumerate(parts):
        if part in ("-F", "--file") and i + 1 < len(parts):
            files.append(Path(parts[i + 1]))
        elif part.startswith("--file="):
            files.append(Path(part.split("=", 1)[1]))
    return files


def is_publishing(command: str) -> bool:
    return bool(
        re.search(r"\bgit\s+(commit|tag)\b|\bgh\s+pr\s+(create|edit|comment)\b|\bgh\s+api\b", command)
    )


def check_bash(command: str) -> int:
    for pattern, reason in BASH_RULES:
        if pattern.search(command):
            return block(reason)
    if is_publishing(command):
        if ATTRIBUTION.search(command):
            return block(
                "AI attribution (Co-Authored-By, Claude-Session, Generated with Claude Code) "
                "is not allowed in commits or pull requests"
            )
        for path in commit_message_files(command):
            try:
                if ATTRIBUTION.search(path.read_text(encoding="utf-8")):
                    return block(f"attribution trailer found in commit message file {path}")
            except OSError:
                continue
    elif not re.search(r"\bgit\s+log\b", command):
        reads = env_file_reads(command)
        if reads:
            return block(f"reading {reads[0]} is not allowed; only .env.example may be read")
    rel_test = "evals/datasets/test/"
    if rel_test in command and re.search(
        r"(\bsed\s+-i|\brm\b|\bmv\b|>|\btee\b|\btruncate\b|\bcp\b)", command
    ):
        return block("existing files in evals/datasets/test/ must never be edited")
    return 0


def check_edit(file_path: str) -> int:
    if not file_path:
        return 0
    path = Path(file_path)
    if not path.is_absolute():
        path = project_dir() / path
    path = path.resolve()
    name = path.name
    if name.startswith(".env") and name != ".env.example":
        return block(f"{name} files other than .env.example are off limits")
    try:
        rel = path.relative_to(project_dir()).as_posix()
    except ValueError:
        return 0
    if rel.startswith("evals/datasets/test/") and path.exists():
        return block(f"existing files in evals/datasets/test/ must never be edited ({rel})")
    return 0


def main() -> int:
    try:
        payload = json.load(sys.stdin)
        tool = payload.get("tool_name", "")
        tool_input = payload.get("tool_input") or {}
    except (json.JSONDecodeError, AttributeError) as exc:
        print(f"pre_tool_guard: ignoring malformed input: {exc}", file=sys.stderr)
        return 0
    if tool == "Bash":
        return check_bash(str(tool_input.get("command", "")))
    if tool in ("Write", "Edit", "MultiEdit", "NotebookEdit"):
        return check_edit(str(tool_input.get("file_path") or tool_input.get("notebook_path") or ""))
    return 0


if __name__ == "__main__":
    sys.exit(main())
