"""Release preparation (T-48): the changelog has the release section, and the version agrees across
the changelog, the Helm chart and the release workflow."""

from __future__ import annotations

import re
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[2]
VERSION = "0.1.0"


def test_changelog_has_version() -> None:
    text = (ROOT / "CHANGELOG.md").read_text(encoding="utf-8")
    assert text.startswith("# Changelog")
    sections = re.findall(r"(?m)^## \[([^\]]+)\](?: - (\d{4}-\d{2}-\d{2}))?$", text)
    assert sections and sections[0][0] in ("Unreleased", VERSION), sections
    assert (VERSION, "2026-10-17") in sections, sections
    body = text.split(f"## [{VERSION}]", 1)[1].split("\n## [", 1)[0]
    for heading in ("### Added", "### Security", "### Known limitations"):
        assert heading in body, f"{heading} missing from {VERSION}"
    assert f"[{VERSION}]: " in text, "link reference for the version"


def test_versions_agree() -> None:
    chart = yaml.safe_load((ROOT / "deploy/helm/mcp-gateway/Chart.yaml").read_text(encoding="utf-8"))
    assert chart["version"] == VERSION and chart["appVersion"] == VERSION
    values = yaml.safe_load((ROOT / "deploy/helm/mcp-gateway/values.yaml").read_text(encoding="utf-8"))
    assert values["image"]["tag"] == VERSION
    release = (ROOT / ".github/workflows/release.yml").read_text(encoding="utf-8")
    assert 'tags: ["v*"]' in release
    # The release notes step extracts the section by the same heading format.
    assert '"^## \\\\[" v "\\\\]"' in release
