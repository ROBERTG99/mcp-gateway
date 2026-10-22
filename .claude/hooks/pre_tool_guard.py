#!/usr/bin/env python3
"""PreToolUse guard for Bash, Read, Write and Edit.

Blocks (exit 2 with a reason on stderr) the never-do actions of this repository: terraform apply/destroy,
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

# `git` followed by any global options (-C dir, -c key=value, --git-dir=...) before the subcommand.
GIT = r"\bgit(?:\s+(?:-[Cc]\s+\S+|--[\w-]+(?:=\S+)?))*\s+"

BASH_RULES: list[tuple[re.Pattern[str], str]] = [
    (re.compile(r"\bterraform\s+(?:\S+\s+)*?(apply|destroy)\b"), "terraform apply/destroy is never run"),
    (
        re.compile(
            GIT + r"push\b[^;&|]*?(\s--force(?:-with-lease|-if-includes)?\b|\s-[a-zA-Z]*f[a-zA-Z]*\b|\s\+\S)"
        ),
        "force-push is forbidden",
    ),
    (
        re.compile(GIT + r"push\b[^;&|]*(--tags\b|--follow-tags\b|\sv\d+\.\d+\.\d+\b|refs/tags/)"),
        "tags are pushed by hand, not from a session",
    ),
    (re.compile(GIT + r"config\b[^;&|]*\buser\.(name|email|signingkey)\b"), "never change git identity"),
    (re.compile(r"\bgit\s+(?:[^;&|]*\s)?-c\s*user\.(name|email)"), "never change git identity"),
    (re.compile(r"\bgit\b[^;&|]*\s--author\b"), "never commit under another author"),
    (
        re.compile(r"(^|[\s;&|(])(export\s+)?GIT_(AUTHOR|COMMITTER)_(NAME|EMAIL|DATE)\s*="),
        "never change GIT_AUTHOR_*/GIT_COMMITTER_* variables",
    ),
    (
        re.compile(r"\bgh\s+repo\s+(create|delete|rename|archive|fork)\b"),
        "never create or alter repositories",
    ),
    (re.compile(r"\bgh\s+repo\s+edit\b[^;&|]*--visibility"), "never change repository visibility"),
    (re.compile(r"\bgh\s+release\s+create\b"), "releases are published by hand, not from a session"),
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

# A .env file token anywhere in a path: start of word or after a path separator / quote / '='.
ENV_TOKEN = re.compile(r"(?:^|(?<=[\s'\"=:/<>(]))\.env(?:\.[\w.-]+)?(?=$|[\s'\";|&)<>])")
# `cp .env.example .env` creates the file: only that destination is exempt.
CP_ENV = re.compile(r"\bcp\s+(?:\S*/)?\.env\.example\s+(?:\S*/)?\.env(?=$|[\s;&|])")

TEST_PARTITION = "evals/datasets/test"
MUTATING = re.compile(r"(\bsed\s+(-\w+\s+)*-i|\brm\b|\bmv\b|\btee\b|\btruncate\b|\bcp\b|\bdd\b|\bchmod\b)")
REDIRECT_INTO_TEST = re.compile(r">>?\s*\S*" + re.escape(TEST_PARTITION))


def project_dir() -> Path:
    return Path(os.environ.get("CLAUDE_PROJECT_DIR") or os.getcwd()).resolve()


def block(reason: str) -> int:
    print(f"pre_tool_guard: blocked: {reason}", file=sys.stderr)
    return 2


def env_file_reads(command: str) -> list[str]:
    scrubbed = CP_ENV.sub("cp .env.example <created-env-file>", command)
    return [m.group(0) for m in ENV_TOKEN.finditer(scrubbed) if m.group(0) != ".env.example"]


def message_files(command: str) -> list[Path]:
    try:
        parts = shlex.split(command)
    except ValueError:
        return []
    files = []
    for i, part in enumerate(parts):
        if part in ("-F", "--file", "--body-file") and i + 1 < len(parts):
            files.append(Path(parts[i + 1]))
        elif part.startswith(("--file=", "--body-file=")):
            files.append(Path(part.split("=", 1)[1]))
    return files


def is_publishing(command: str) -> bool:
    return bool(
        re.search(GIT + r"(commit|tag|merge|notes)\b", command)
        or re.search(r"\bgh\s+(pr|issue)\s+(create|edit|comment|review)\b|\bgh\s+api\b", command)
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
        for path in message_files(command):
            try:
                text = path.read_text(encoding="utf-8")
            except (OSError, UnicodeDecodeError):
                continue
            if ATTRIBUTION.search(text):
                return block(f"attribution trailer found in message file {path}")
    reads = env_file_reads(command)
    if reads:
        return block(f"reading {reads[0]} is not allowed; only .env.example may be read")
    if TEST_PARTITION in command and (MUTATING.search(command) or REDIRECT_INTO_TEST.search(command)):
        return block("existing files in evals/datasets/test/ must never be edited")
    return 0


def check_edit(file_path: str, read_only: bool = False) -> int:
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
    if not read_only and rel.startswith("evals/datasets/test/") and path.exists():
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
    if tool == "Read":
        return check_edit(str(tool_input.get("file_path", "")), read_only=True)
    if tool in ("Write", "Edit", "MultiEdit", "NotebookEdit"):
        return check_edit(str(tool_input.get("file_path") or tool_input.get("notebook_path") or ""))
    return 0


if __name__ == "__main__":
    sys.exit(main())
