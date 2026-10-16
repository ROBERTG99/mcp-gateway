"""Repository bookkeeping checks for the build (T-01)."""

from __future__ import annotations

import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]


def _spec_text() -> str:
    for candidate in (ROOT / "BUILD.md", ROOT / "docs" / "specs" / "000-build-spec.md"):
        if candidate.exists():
            return candidate.read_text(encoding="utf-8")
    raise AssertionError("build specification not found")


def _task_ids(text: str) -> list[str]:
    return sorted(set(re.findall(r"\*\*(T-\d{2}) ·", text)))


def test_progress_lists_all_tasks() -> None:
    spec_ids = _task_ids(_spec_text())
    assert len(spec_ids) == 49
    progress = (ROOT / "PROGRESS.md").read_text(encoding="utf-8")
    assert progress.splitlines()[0].startswith("# ")
    assert "## Needs the maintainer" in progress
    first_section = re.findall(r"^## (.+)$", progress, flags=re.M)[0]
    assert first_section == "Needs the maintainer"
    for task_id in spec_ids:
        assert re.search(rf"\|\s*{task_id}\s*\|", progress), f"{task_id} missing from PROGRESS.md"
    assert "## Global Definition of Done" in progress
    dod = progress.split("## Global Definition of Done", 1)[1]
    assert len(re.findall(r"^- \[[ x]\] ", dod, flags=re.M)) >= 21


def test_bookkeeping_files_exist() -> None:
    for rel in (
        "docs/DEVELOPMENT.md",
        "docs/DECISIONS.md",
        "docs/KNOWN_LIMITATIONS.md",
        "docs/specs/000-build-spec.md",
        ".editorconfig",
        "LICENSE",
    ):
        assert (ROOT / rel).is_file(), rel
    assert "ROBERTG99" in (ROOT / "LICENSE").read_text(encoding="utf-8")


def test_gitignore_has_required_entries() -> None:
    lines = (ROOT / ".gitignore").read_text(encoding="utf-8").splitlines()
    for entry in ("bin/", "dist/", "coverage*.out", ".env", ".env.*", "!.env.example",
                  "e2e/node_modules/", "e2e/test-results/"):
        assert any(line.strip().lstrip("/") == entry.lstrip("/") for line in lines), entry
