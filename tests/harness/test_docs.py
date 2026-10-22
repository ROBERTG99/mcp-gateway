"""Documentation deliverables: every document exists, ADRs name the rejected
alternative, every README number cites its source, and the design FAQ answers 15 questions with
file:line references that resolve."""

from __future__ import annotations

import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]

REQUIRED = [
    "README.md",
    "docs/architecture.md",
    "docs/evaluation.md",
    "evals/REPORT.md",
    "docs/security.md",
    "docs/runbook.md",
    "docs/DEVELOPMENT.md",
    "docs/design-faq.md",
    "docs/KNOWN_LIMITATIONS.md",
    "CONTRIBUTING.md",
    "SECURITY.md",
    "CHANGELOG.md",
    "LICENSE",
]
ADRS = {
    "0001": "auth",
    "0002": "opa",
    "0003": "idempotency",
    "0004": "audit",
    "0005": "injection",
    "0006": "stdio",
    "0007": "console",
    "0008": "dedup",
}
# A citation next to a number: a make target or command in backticks, or a generated report.
CITATION = re.compile(
    r"`(make [a-z][a-z-]*|go test[^`]*|uv run[^`]*|\./scripts/[^`]*|scripts/[^`]*|mcp-gateway [^`]*)`"
    r"|evals/(reports/|REPORT\.md)|docs/perf/|docs/verification\.md|evals/datasets/"
)
# Numbers that state a measurement or result (not section numbers, ports, versions or years).
MEASUREMENT = re.compile(
    r"\b\d+(?:[.,]\d+)*\s?(?:ms|µs|MB|MiB|KiB|GB|%|calls/s|s\b|x\b|×)"
    r"|\b(?:recall|precision|accuracy|rate|ratio|p50|p95|p99)\b[^|.]{0,40}?\b\d+\.\d+"
)


def prose_lines(path: Path) -> list[tuple[int, str]]:
    out, fenced = [], False
    for i, line in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
        if line.lstrip().startswith("```"):
            fenced = not fenced
            continue
        if not fenced:
            out.append((i, line))
    return out


def test_required_docs_exist() -> None:
    missing = [
        p for p in REQUIRED if not (ROOT / p).is_file() or not (ROOT / p).read_text(encoding="utf-8").strip()
    ]
    assert not missing, missing
    for num, word in ADRS.items():
        found = sorted((ROOT / "docs" / "adr").glob(f"{num}-*.md"))
        assert found, f"ADR {num} missing"
        text = found[0].read_text(encoding="utf-8")
        assert word in (found[0].name + text).lower(), f"ADR {num} is not about {word}"
        assert re.search(r"(?im)^## (Alternatives considered|Rejected alternative)", text), (
            f"ADR {num}: no rejected alternative"
        )
        assert "Rejected" in text, f"ADR {num}: does not say which alternative was rejected"
        assert re.search(r"(?m)^- Status: Accepted", text), f"ADR {num}: no status"


def test_readme_numbers_cite_sources() -> None:
    readme = ROOT / "README.md"
    uncited = [
        f"README.md:{i}: {line.strip()}"
        for i, line in prose_lines(readme)
        if MEASUREMENT.search(line) and not CITATION.search(line)
    ]
    assert not uncited, "numbers without a command or report next to them:\n" + "\n".join(uncited)
    text = readme.read_text(encoding="utf-8")
    assert "Under construction" not in text and "under construction" not in text


REF = re.compile(
    r"`?((?:cmd|internal|pkg|policies|migrations|deploy|scripts|config|tests|e2e)/[\w./-]+\.\w+):(\d+)(?:-(\d+))?`?"
)


def test_faq_answers_fifteen_questions_with_references() -> None:
    faq = (ROOT / "docs" / "design-faq.md").read_text(encoding="utf-8")
    questions = re.findall(r"(?m)^## (\d+)\. ", faq)
    assert [int(q) for q in questions] == list(range(1, 16)), questions
    sections = re.split(r"(?m)^## \d+\. ", faq)[1:]
    for n, body in enumerate(sections, 1):
        refs = REF.findall(body)
        assert refs, f"FAQ {n}: no file:line reference"
        for path, start, end in refs:
            err = ref_error(path, start, end)
            assert err is None, f"FAQ {n}: {err}"


def ref_error(path: str, start: str, end: str) -> str | None:
    """Why a path:start[-end] reference does not resolve, or None when it points inside the file."""
    f = ROOT / path
    if not f.is_file():
        return f"{path} missing"
    n = len(f.read_text(encoding="utf-8").splitlines())
    if not 1 <= int(start) <= n or (end and not int(start) <= int(end) <= n):
        return f"{path}:{start}{'-' + end if end else ''} outside {n} lines"
    return None


def test_doc_references_resolve() -> None:
    """file:line references anywhere in docs/ and the README point inside existing files."""
    bad = []
    for doc in [ROOT / "README.md", *sorted((ROOT / "docs").rglob("*.md"))]:
        if "specs" in doc.parts:
            continue
        for i, line in prose_lines(doc):
            for path, start, end in REF.findall(line):
                if err := ref_error(path, start, end):
                    bad.append(f"{doc.relative_to(ROOT)}:{i}: {err}")
    assert not bad, "\n".join(bad)
