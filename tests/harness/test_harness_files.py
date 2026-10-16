"""Checks for the Claude Code harness files (T-03)."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import yaml

ROOT = Path(__file__).resolve().parents[2]
CLAUDE = ROOT / ".claude"

RULES_WITH_PATHS = [
    "go",
    "rego",
    "helm",
    "python",
    "frontend",
    "testing",
    "evals",
    "docker",
    "security",
    "docs",
    "llm-code",
]
AGENTS = [
    "reviewer",
    "security-auditor",
    "test-designer",
    "eval-analyst",
    "ux-reviewer",
    "docs-editor",
    "policy-reviewer",
]
SKILLS = [
    "tdd",
    "quality-gate",
    "eval-report",
    "new-adr",
    "add-migration",
    "add-endpoint",
    "add-agent-tool",
    "add-eval-case",
    "demo-capture",
    "release",
    "add-policy",
    "add-upstream-server",
    "audit-forensics",
]
ALLOW = [
    "Bash(make *)",
    "Bash(go test *)",
    "Bash(go build *)",
    "Bash(go run *)",
    "Bash(go mod *)",
    "Bash(docker compose *)",
    "Bash(docker build *)",
    "Bash(pnpm *)",
    "Bash(uv run *)",
    "Bash(uvx *)",
    "Bash(helm *)",
    "Bash(git add *)",
    "Bash(git commit *)",
    "Bash(git status)",
    "Bash(git diff *)",
    "Bash(git log *)",
    "Bash(git push -u origin *)",
]
DENY = [
    "Read(./.env)",
    "Read(./.env.local)",
    "Bash(terraform apply *)",
    "Bash(terraform destroy *)",
    "Bash(git push --force *)",
    "Bash(git push -f *)",
    "Bash(rm -rf /*)",
    "Bash(rm -rf ~*)",
]


def frontmatter(path: Path) -> dict[str, Any] | None:
    text = path.read_text(encoding="utf-8")
    if not text.startswith("---\n"):
        return None
    block = text.split("---\n", 2)[1]
    data = yaml.safe_load(block)
    assert isinstance(data, dict), path
    return data


def settings_files() -> list[Path]:
    """The settings template is always checked; the live file too when present."""
    files = [ROOT / "docs" / "harness" / "settings.json"]
    live = CLAUDE / "settings.json"
    if live.exists():
        files.append(live)
    return files


def test_frontmatter_and_attribution() -> None:
    # Attribution rule: always loaded, so no frontmatter.
    git_rule = CLAUDE / "rules" / "git-commits.md"
    assert frontmatter(git_rule) is None
    body = git_rule.read_text(encoding="utf-8")
    for word in ("Co-Authored-By", "Claude-Session", "Generated with Claude Code", "git config user.*"):
        assert word in body

    for name in RULES_WITH_PATHS:
        fm = frontmatter(CLAUDE / "rules" / f"{name}.md")
        assert fm is not None and fm.get("paths"), f"rule {name} needs paths: frontmatter"

    for name in AGENTS:
        fm = frontmatter(CLAUDE / "agents" / f"{name}.md")
        assert fm is not None, name
        for key in ("name", "description", "tools", "model"):
            assert fm.get(key), f"agent {name} missing {key}"
        assert fm["name"] == name
    reviewer = frontmatter(CLAUDE / "agents" / "reviewer.md")
    assert reviewer is not None and reviewer["model"] == "opus"
    assert "Edit" not in str(reviewer["tools"]) and "Write" not in str(reviewer["tools"])

    for name in SKILLS:
        fm = frontmatter(CLAUDE / "skills" / name / "SKILL.md")
        assert fm is not None and fm.get("name") == name and fm.get("description"), name

    for path in settings_files():
        settings = json.loads(path.read_text(encoding="utf-8"))
        assert settings["attribution"] == {"commit": "", "pr": "", "sessionUrl": False}, path
        perms = settings["permissions"]
        for entry in ALLOW:
            assert entry in perms["allow"], (path, entry)
        for entry in DENY:
            assert entry in perms["deny"], (path, entry)
        hooks = settings["hooks"]
        for event in ("SessionStart", "PostToolUse", "PreToolUse", "Stop"):
            assert event in hooks, (path, event)


def test_instructions_files() -> None:
    claude_md = (ROOT / "CLAUDE.md").read_text(encoding="utf-8")
    assert len(claude_md.splitlines()) < 200
    for needle in (
        "make check-fast",
        "PROGRESS.md",
        "docs/DECISIONS.md",
        "docs/adr/",
        "Co-Authored-By",
        "terraform apply",
        "test first",
    ):
        assert needle.lower() in claude_md.lower(), needle
    agents_md = (ROOT / "AGENTS.md").read_text(encoding="utf-8")
    assert "make check-fast" in agents_md and "Co-Authored-By" in agents_md


def test_mcp_json() -> None:
    servers = json.loads((ROOT / ".mcp.json").read_text(encoding="utf-8"))["mcpServers"]
    assert {"mcp-gateway", "playwright", "postgres-readonly"} <= set(servers)
    assert servers["mcp-gateway"]["url"] == "http://localhost:8210/mcp"
