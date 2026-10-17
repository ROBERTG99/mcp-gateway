"""Static checks of the Docker setup (spec §16, .claude/rules/docker.md): compose.yaml,
compose.test.yaml and the Dockerfile (make test-docker runs these inside the test image too)."""

from __future__ import annotations

import re
from pathlib import Path
from typing import Any

import yaml

ROOT = Path(__file__).resolve().parents[2]

# Services that run once and exit; every other service is long-running and needs a healthcheck.
ONE_SHOT = {"migrate", "seed", "go-tests", "harness-tests"}
EXPECTED = {
    "postgres",
    "redis",
    "migrate",
    "dev-issuer",
    "gateway",
    "upstream-ledger",
    "upstream-crm",
    "upstream-orders",
    "approval-sink",
    "seed",
    "dynamodb",
}


def load(name: str) -> dict[str, Any]:
    doc = yaml.safe_load((ROOT / name).read_text(encoding="utf-8"))
    assert isinstance(doc, dict)
    return doc


def services(*names: str) -> dict[str, dict[str, Any]]:
    out: dict[str, dict[str, Any]] = {}
    for name in names:
        for svc, body in load(name)["services"].items():
            out.setdefault(svc, {}).update(body)
    return out


def host_ports(body: dict[str, Any]) -> list[tuple[str, int]]:
    out = []
    for p in body.get("ports", []):
        m = re.fullmatch(r"(?:(\d+\.\d+\.\d+\.\d+):)?(\d+):\d+(?:/\w+)?", str(p))
        assert m, f"unparsed port {p!r}"
        out.append((m.group(1) or "", int(m.group(2))))
    return out


def test_ports_in_range_and_healthchecks() -> None:
    svcs = services("compose.yaml", "compose.test.yaml")
    assert EXPECTED <= set(svcs), sorted(EXPECTED - set(svcs))
    seen: dict[int, str] = {}
    for name, body in svcs.items():
        for bind, port in host_ports(body):
            assert 8200 <= port <= 8299, f"{name}: host port {port} outside 8200-8299"
            assert bind == "127.0.0.1", f"{name}: port {port} must bind 127.0.0.1"
            assert port not in seen, f"{name} and {seen[port]} both publish {port}"
            seen[port] = name
        hc = body.get("healthcheck", {})
        if name in ONE_SHOT:
            continue
        assert hc and not hc.get("disable"), f"{name}: long-running service without a healthcheck"
        assert hc["test"][0] in ("CMD", "CMD-SHELL"), f"{name}: healthcheck must be exec or shell form"
    for name, body in svcs.items():
        deps = body.get("depends_on", {})
        assert isinstance(deps, dict), f"{name}: depends_on must use conditions"
        for dep, cond in deps.items():
            want = "service_completed_successfully" if dep in ONE_SHOT else "service_healthy"
            assert cond.get("condition") == want, f"{name} -> {dep}: condition {cond} (want {want})"


def test_spec_ports() -> None:
    svcs = services("compose.yaml")
    want = {
        "postgres": [8220],
        "redis": [8230],
        "dynamodb": [8240],
        "approval-sink": [8241],
        "dev-issuer": [8212],
        "gateway": [8200, 8210, 8211],
        "upstream-ledger": [8213],
        "upstream-crm": [8214],
        "upstream-orders": [8215],
    }
    for name, ports in want.items():
        assert [p for _, p in host_ports(svcs[name])] == ports, name


def test_compose_test_runs_suites_in_containers() -> None:
    test = load("compose.test.yaml")["services"]
    assert set(test) >= {"go-tests", "harness-tests"}
    for name in ("go-tests", "harness-tests"):
        body = test[name]
        assert body["build"]["target"] == "test", name
        assert "test" in body.get("profiles", []), f"{name} must not start with a plain `up`"
        assert not body.get("ports"), f"{name} publishes no ports"
    go = test["go-tests"]
    assert go["environment"]["MCPGW_TEST_USE_COMPOSE"] == "1"
    cmd = " ".join(go["command"])
    assert "-race" in cmd and "-tags=integration" in cmd and "./..." in cmd
    # Own Postgres and Redis: the tests set role passwords and flush databases, which must never
    # touch a stack running next to them (make verify runs test-docker between up and down).
    assert set(go["depends_on"]) >= {"postgres-test", "redis-test", "dynamodb"}
    assert "postgres-test:5432" in go["environment"]["MCPGW_TEST_DATABASE_URL"]
    assert "redis-test:6379" in go["environment"]["MCPGW_TEST_REDIS_URL"]
    for name in ("postgres-test", "redis-test"):
        assert "test" in test[name].get("profiles", []) and not test[name].get("ports"), name
    assert "pytest" in " ".join(test["harness-tests"]["command"])


def test_dockerfile_rules() -> None:
    text = (ROOT / "Dockerfile").read_text(encoding="utf-8")
    stages = re.split(r"(?m)^FROM ", text)[1:]
    names = {}
    for stage in stages:
        head = stage.splitlines()[0]
        m = re.fullmatch(r"(\S+) AS (\S+)", head.strip())
        assert m, f"unnamed stage: FROM {head}"
        names[m.group(2)] = (m.group(1), stage)
    assert {"gateway", "examples", "test"} <= set(names)
    for target in ("gateway", "examples"):
        base, _ = names[target]
        assert base == "gcr.io/distroless/static-debian12:nonroot", target
    for name, (base, body) in names.items():
        if base.startswith("golang:"):
            assert "ARG HTTPS_PROXY" in body, f"stage {name} downloads but lacks ARG HTTPS_PROXY"
    assert "apt-get" not in text
    hc = re.search(r"(?m)^HEALTHCHECK .*CMD (.*)$", names["gateway"][1])
    assert hc and hc.group(1).startswith("["), "gateway HEALTHCHECK must be exec form"
    assert "CGO_ENABLED=0" in names[next(n for n, (b, _) in names.items() if b.startswith("golang:"))][1]


def test_env_example_does_not_change_the_stack() -> None:
    """`make bootstrap` copies .env.example to .env, which Compose reads for ${VAR:-default}
    substitution. A value set there must equal the compose default, or a fresh clone runs a
    different stack (MCPGW_TENANTS_FILE=config/tenants.yaml broke the gateway in make verify-clean)."""
    env = {}
    for line in (ROOT / ".env.example").read_text(encoding="utf-8").splitlines():
        if "=" in line and not line.lstrip().startswith("#"):
            key, value = line.split("=", 1)
            env[key.strip()] = value.strip()
    for name in ("compose.yaml", "compose.test.yaml"):
        text = (ROOT / name).read_text(encoding="utf-8")
        for var, default in re.findall(r"\$\{([A-Z0-9_]+):-([^}]*)\}", text):
            if env.get(var):
                assert env[var] == default, (
                    f"{name}: .env.example sets {var}={env[var]!r}, compose default {default!r}"
                )
