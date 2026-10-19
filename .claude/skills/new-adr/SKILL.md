---
name: new-adr
description: Create a numbered Architecture Decision Record in docs/adr with context, decision, consequences and the rejected alternative.
---

# New ADR

1. Find the next number: `ls docs/adr | sort | tail -1`.
2. Create `docs/adr/NNNN-kebab-title.md` with sections: Status (Accepted, date), Context, Decision, Alternatives considered (at least the rejected one, with why), Consequences.
3. Link it from `docs/architecture.md` and add a row to `docs/DECISIONS.md` if it came from a build-time ambiguity.
4. `make lint` (markdownlint).
