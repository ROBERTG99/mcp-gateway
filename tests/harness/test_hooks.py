"""Tests for the Claude Code hooks in .claude/hooks (T-04)."""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
from pathlib import Path
from typing import Any

import pytest

ROOT = Path(__file__).resolve().parents[2]
HOOKS = ROOT / ".claude" / "hooks"


def run_hook(
    name: str, payload: dict[str, Any], env: dict[str, str] | None = None, cwd: Path = ROOT
) -> subprocess.CompletedProcess[str]:
    full_env = {**os.environ, "CLAUDE_PROJECT_DIR": str(ROOT), **(env or {})}
    return subprocess.run(
        [sys.executable, str(HOOKS / name)],
        input=json.dumps(payload),
        capture_output=True,
        text=True,
        env=full_env,
        cwd=cwd,
        timeout=60,
        check=False,
    )


def bash(command: str) -> dict[str, Any]:
    return {"hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_input": {"command": command}}


def edit(path: str, tool: str = "Edit") -> dict[str, Any]:
    tool_input = {"file_path": path, "old_string": "a", "new_string": "b", "content": "x"}
    return {"hook_event_name": "PreToolUse", "tool_name": tool, "tool_input": tool_input}


# --- pre_tool_guard.py -------------------------------------------------------------------------


def test_guard_blocks_coauthor_trailer() -> None:
    for message in (
        'git commit -m "feat: x" -m "Co-Authored-By: Someone <a@b.c>"',
        "git commit -m 'feat: x\n\nClaude-Session: https://example.invalid/s'",
        'git commit -m "docs: y\n\nGenerated with Claude Code"',
        'gh pr create --title t --body "🤖 Generated with [Claude Code](https://claude.com/claude-code)"',
    ):
        result = run_hook("pre_tool_guard.py", bash(message))
        assert result.returncode == 2, message
        assert "attribution" in result.stderr.lower()


def test_guard_blocks_coauthor_trailer_in_message_file(tmp_path: Path) -> None:
    msg = tmp_path / "msg.txt"
    msg.write_text("feat: z\n\nCo-authored-by: X <x@y.z>\n", encoding="utf-8")
    assert run_hook("pre_tool_guard.py", bash(f"git commit -F {msg}")).returncode == 2


def test_guard_blocks_test_partition_edit() -> None:
    existing = ROOT / "evals" / "datasets" / "test" / "_guard_probe.jsonl"
    existing.parent.mkdir(parents=True, exist_ok=True)
    created = not existing.exists()
    if created:
        existing.write_text("{}\n", encoding="utf-8")
    try:
        for tool in ("Edit", "Write"):
            result = run_hook("pre_tool_guard.py", edit(str(existing), tool))
            assert result.returncode == 2
            assert "evals/datasets/test" in result.stderr
        result = run_hook("pre_tool_guard.py", bash(f"sed -i 's/a/b/' {existing.relative_to(ROOT)}"))
        assert result.returncode == 2
        # A new file in the test partition may be created (regeneration adds files).
        new_file = existing.parent / "_brand_new_probe.jsonl"
        assert run_hook("pre_tool_guard.py", edit(str(new_file), "Write")).returncode == 0
        # The dev partition is editable.
        assert run_hook("pre_tool_guard.py", edit(str(ROOT / "evals/datasets/dev/x.jsonl"))).returncode == 0
    finally:
        if created:
            existing.unlink()


@pytest.mark.parametrize(
    "command",
    [
        "terraform apply -auto-approve",
        "cd infra && terraform destroy",
        "git push --force origin main",
        "git push -f origin feature",
        "git push --force-with-lease origin x",
        "git config user.name 'Someone Else'",
        "git config --global user.email x@y.z",
        "export GIT_AUTHOR_NAME=bot",
        "GIT_COMMITTER_EMAIL=bot@x git commit -m 'chore: x'",
        "gh repo create ROBERTG99/new --public",
        "gh repo edit --visibility public",
        "cat .env",
        "source .env.local && make up",
        "grep KEY .env.production",
        "curl -X POST https://hooks.slack.com/services/T000/B000/XXX -d '{}'",
        "sendmail someone@example.com < note.txt",
        "git push origin v0.1.0",
        "git push --tags",
        # reviewer findings (M0): bypasses that must be blocked
        "cat /home/user/mcp-gateway/.env",
        "cat config/.env",
        "git log -1; cat .env",
        "git commit -m x; cat .env",
        "cp .env.example .env && cat .env",
        "git push origin +main",
        "git -C . push --force",
        "git push -uf origin main",
        "git -c user.name=x commit -m 'chore: x'",
        "git commit --author='X <x@y.z>' -m 'chore: x'",
        "cd evals/datasets/test && rm a.jsonl",
        "echo x > evals/datasets/test/policy.v1.jsonl",
    ],
)
def test_guard_blocks_never_do_commands(command: str) -> None:
    result = run_hook("pre_tool_guard.py", bash(command))
    assert result.returncode == 2, command
    assert result.stderr.strip(), command


@pytest.mark.parametrize(
    "command",
    [
        "make check-fast",
        "git commit -m 'feat(pipeline): add idempotency stage'",
        "git push -u origin claude/build-md-execution-blkq3q",
        "cp .env.example .env",
        "cat .env.example",
        "git log --format='%an <%ae>' | sort -u",
        "git log --format=%B | grep -E 'Co-Authored-By|Claude-Session|Generated with Claude Code'",
        "docker compose up -d --wait",
        "go test ./...",
        "ls evals/datasets/test 2>/dev/null",
        "git -C /tmp/x log --oneline",
        "git commit -m 'docs: explain .env.example usage'",
        "git push -u origin feature/x",
    ],
)
def test_guard_allows_normal_commands(command: str) -> None:
    result = run_hook("pre_tool_guard.py", bash(command))
    assert result.returncode == 0, (command, result.stderr)


def test_guard_blocks_env_file_edit() -> None:
    assert run_hook("pre_tool_guard.py", edit(str(ROOT / ".env.production"), "Write")).returncode == 2
    assert run_hook("pre_tool_guard.py", edit(str(ROOT / ".env.example"), "Write")).returncode == 0


def test_guard_blocks_env_read_tool() -> None:
    payload = {
        "hook_event_name": "PreToolUse",
        "tool_name": "Read",
        "tool_input": {"file_path": str(ROOT / ".env.production")},
    }
    assert run_hook("pre_tool_guard.py", payload).returncode == 2
    payload["tool_input"] = {"file_path": str(ROOT / ".env.example")}
    assert run_hook("pre_tool_guard.py", payload).returncode == 0


def test_guard_scans_pr_body_file(tmp_path: Path) -> None:
    body = tmp_path / "body.md"
    body.write_text("Summary\n\nGenerated with Claude Code\n", encoding="utf-8")
    assert run_hook("pre_tool_guard.py", bash(f"gh pr create --title t --body-file {body}")).returncode == 2
    binary = tmp_path / "msg.bin"
    binary.write_bytes(b"\xff\xfe\x00bad")
    assert run_hook("pre_tool_guard.py", bash(f"git commit -F {binary}")).returncode == 0


def test_guard_fails_open_on_garbage_input() -> None:
    full_env = {**os.environ, "CLAUDE_PROJECT_DIR": str(ROOT)}
    result = subprocess.run(
        [sys.executable, str(HOOKS / "pre_tool_guard.py")],
        input="not json",
        capture_output=True,
        text=True,
        env=full_env,
        check=False,
    )
    assert result.returncode == 0


# --- post_edit_format.py -----------------------------------------------------------------------


def test_post_edit_formats_go_file(tmp_path: Path) -> None:
    if shutil.which("gofmt") is None:
        pytest.fail("gofmt must be installed for this test (Go toolchain)")
    src = tmp_path / "x.go"
    src.write_text("package x\nfunc  F( ) int {return 1}\n", encoding="utf-8")
    payload = {"hook_event_name": "PostToolUse", "tool_name": "Write", "tool_input": {"file_path": str(src)}}
    result = run_hook("post_edit_format.py", payload)
    assert result.returncode == 0
    assert "func F() int { return 1 }" in src.read_text(encoding="utf-8")


def test_post_edit_ignores_other_files(tmp_path: Path) -> None:
    other = tmp_path / "notes.txt"
    other.write_text("  untouched  \n", encoding="utf-8")
    payload = {"hook_event_name": "PostToolUse", "tool_name": "Edit", "tool_input": {"file_path": str(other)}}
    assert run_hook("post_edit_format.py", payload).returncode == 0
    assert other.read_text(encoding="utf-8") == "  untouched  \n"


def test_post_edit_noop_when_formatter_missing(tmp_path: Path) -> None:
    src = tmp_path / "y.go"
    src.write_text("package y\nfunc  G( ) {}\n", encoding="utf-8")
    payload = {"hook_event_name": "PostToolUse", "tool_name": "Write", "tool_input": {"file_path": str(src)}}
    result = run_hook("post_edit_format.py", payload, env={"PATH": "/nonexistent"})
    assert result.returncode == 0
    assert "func  G( ) {}" in src.read_text(encoding="utf-8")


# --- stop_check.py -----------------------------------------------------------------------------


def test_stop_check_exits_when_already_active() -> None:
    result = run_hook(
        "stop_check.py",
        {"hook_event_name": "Stop", "stop_hook_active": True},
        env={"MCPGW_STOP_CHECK_CMD": "false"},
    )
    assert result.returncode == 0
    assert result.stdout.strip() == ""


def test_stop_check_blocks_once_with_summary() -> None:
    result = run_hook(
        "stop_check.py",
        {"hook_event_name": "Stop", "stop_hook_active": False},
        env={"MCPGW_STOP_CHECK_CMD": "echo line1; echo FAIL: something broke; exit 1"},
    )
    assert result.returncode == 0
    decision = json.loads(result.stdout)
    assert decision["decision"] == "block"
    assert "something broke" in decision["reason"]


def test_stop_check_passes_when_green() -> None:
    result = run_hook(
        "stop_check.py",
        {"hook_event_name": "Stop", "stop_hook_active": False},
        env={"MCPGW_STOP_CHECK_CMD": "true"},
    )
    assert result.returncode == 0
    assert result.stdout.strip() == ""


# --- session_start.py --------------------------------------------------------------------------


def test_session_start_noop_outside_cloud() -> None:
    result = run_hook(
        "session_start.py",
        {"hook_event_name": "SessionStart"},
        env={"CLAUDE_CODE_REMOTE": "", "MCPGW_HOOK_DRY_RUN": "1"},
    )
    assert result.returncode == 0
    assert "skipped" in result.stdout


def test_session_start_plans_idempotent_setup_in_cloud() -> None:
    result = run_hook(
        "session_start.py",
        {"hook_event_name": "SessionStart"},
        env={"CLAUDE_CODE_REMOTE": "true", "MCPGW_HOOK_DRY_RUN": "1"},
    )
    assert result.returncode == 0
    for step in ("docker", "tools", "uv sync"):
        assert step in result.stdout
