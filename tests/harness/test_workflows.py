"""CI workflows and repository integrations match spec §17 (actionlint runs in make lint)."""

from __future__ import annotations

from pathlib import Path
from typing import Any

import yaml

ROOT = Path(__file__).resolve().parents[2]
WF = ROOT / ".github" / "workflows"
REQUIRED = {"ci.yml", "e2e.yml", "evals.yml", "docker.yml", "codeql.yml", "release.yml"}


def load(name: str) -> dict[Any, Any]:
    doc = yaml.safe_load((WF / name).read_text(encoding="utf-8"))
    assert isinstance(doc, dict), name
    return doc


def on(doc: dict[Any, Any]) -> dict[str, Any]:
    # PyYAML reads the bare key `on` as the boolean True.
    trigger = doc.get("on", doc.get(True))
    assert isinstance(trigger, dict)
    return trigger


def run_lines(doc: dict[Any, Any]) -> str:
    return "\n".join(
        str(step.get("run", "")) for job in doc["jobs"].values() for step in job.get("steps", [])
    )


def test_required_workflows_exist() -> None:
    names = {p.name for p in WF.glob("*.y*ml")}
    assert REQUIRED <= names, sorted(REQUIRED - names)
    assert not any("terraform" in n for n in names)


def test_least_privilege_and_concurrency() -> None:
    for p in WF.glob("*.y*ml"):
        doc = load(p.name)
        assert "permissions" in doc, f"{p.name}: no top-level permissions"
        perms = doc["permissions"]
        assert perms == {} or perms.get("contents") == "read", f"{p.name}: top-level permissions {perms}"
        for job, body in doc["jobs"].items():
            assert "timeout-minutes" in body, f"{p.name}/{job}: no timeout-minutes"
            for step in body.get("steps", []):
                uses = step.get("uses", "")
                assert not uses or uses.startswith("./") or "@" in uses, (
                    f"{p.name}/{job}: unpinned action {uses}"
                )
                assert "${{ github.event" not in str(step.get("run", "")), (
                    f"{p.name}/{job}: event data interpolated into a shell script"
                )


def test_ci_covers_spec_checks() -> None:
    doc = load("ci.yml")
    assert {"pull_request", "push"} <= set(on(doc))
    runs = run_lines(doc)
    for target in (
        "make lint",
        "make typecheck",
        "make unit",
        "make integration",
        "make contract",
        "make hooks-test",
        "make helm-check",
        "make promtool",
        "make policy-test",
        "make coverage",
    ):
        assert target in runs, f"ci.yml does not run {target}"
    integration = next(j for j in doc["jobs"].values() if "make integration" in run_lines({"jobs": {"j": j}}))
    assert {"postgres", "redis"} <= set(integration.get("services", {})), (
        "integration needs service containers"
    )
    assert "security" in doc["jobs"] and "CI" in str(doc.get("env", {})) + str(doc["jobs"]["security"])


def test_other_workflows() -> None:
    assert "playwright" in run_lines(load("e2e.yml")) or "make e2e" in run_lines(load("e2e.yml"))
    evals = load("evals.yml")
    assert "make eval" in run_lines(evals)
    assert evals["jobs"]["eval"].get("permissions", {}).get("pull-requests") == "write"
    docker = run_lines(load("docker.yml"))
    for word in ("hadolint", "trivy", "image-size"):
        assert word in docker, f"docker.yml lacks {word}"
    assert "--severity HIGH,CRITICAL" in docker and "--exit-code 1" in docker
    release = load("release.yml")
    assert set(on(release)) == {"push"} and on(release)["push"] == {"tags": ["v*"]}
    assert "ghcr.io/robertg99/mcp-gateway" in (WF / "release.yml").read_text(encoding="utf-8")
    codeql = (WF / "codeql.yml").read_text(encoding="utf-8")
    assert "github/codeql-action/analyze" in codeql and "go" in codeql


def test_repository_integrations() -> None:
    assert (ROOT / "CODEOWNERS").read_text(encoding="utf-8").strip().startswith("#")
    assert "@ROBERTG99" in (ROOT / "CODEOWNERS").read_text(encoding="utf-8")
    pre = yaml.safe_load((ROOT / ".pre-commit-config.yaml").read_text(encoding="utf-8"))
    ids = {h["id"] for repo in pre["repos"] for h in repo["hooks"]}
    assert {"gofmt", "golangci-lint", "ruff", "gitleaks", "actionlint", "policy-fmt"} <= ids, ids
    dev = yaml.safe_load((ROOT / ".devcontainer" / "devcontainer.json").read_text(encoding="utf-8"))
    assert "make bootstrap" in dev["postCreateCommand"]
    assert (ROOT / ".github" / "dependabot.yml").exists()
