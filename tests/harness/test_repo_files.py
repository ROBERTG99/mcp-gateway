"""Repository bookkeeping checks."""

from __future__ import annotations

from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]


def test_bookkeeping_files_exist() -> None:
    for rel in (
        "docs/DEVELOPMENT.md",
        "docs/DECISIONS.md",
        "docs/KNOWN_LIMITATIONS.md",
        ".editorconfig",
        "LICENSE",
    ):
        assert (ROOT / rel).is_file(), rel
    assert "ROBERTG99" in (ROOT / "LICENSE").read_text(encoding="utf-8")


def test_gitignore_has_required_entries() -> None:
    lines = (ROOT / ".gitignore").read_text(encoding="utf-8").splitlines()
    for entry in (
        "bin/",
        "dist/",
        "coverage*.out",
        ".env",
        ".env.*",
        "!.env.example",
        "e2e/node_modules/",
        "e2e/test-results/",
    ):
        assert any(line.strip().lstrip("/") == entry.lstrip("/") for line in lines), entry
