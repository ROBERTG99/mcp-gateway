# 0005. Heuristic injection scoring instead of an ML classifier

- Status: Accepted (2026-10-17)

## Context

Upstream tool results can carry text written by an attacker, such as a CRM note that says "ignore previous instructions". The gateway should flag such text before an agent reads it. The gateway calls no model, and `depguard` forbids importing a model provider SDK (`CLAUDE.md`, `.claude/rules/llm-code.md`). The scorer also runs on every result, so it must be fast and give the same answer for the same text.

## Decision

Score results with a deterministic heuristic in `internal/injection/injection.go`.

- The text is normalised first: NFKC, a confusables map, hidden characters removed, and spaced-out letters and leetspeak folded.
- Seven signals add fixed weights, and the sum is clipped to 1.0: override phrase 0.35, hidden Unicode 0.3, tool reference 0.2, role marker 0.2, imperative density 0.15, external link 0.15 and base64 blob 0.1 (`Weights`; `TestSignalWeights`).
- The pipeline flags a result at or above `MCPGW_INJECTION_THRESHOLD` (default 0.5). In `mark` mode it only flags. In `block` mode (`MCPGW_INJECTION_MODE`) it withholds the text (`internal/pipeline/output.go`).
- Every result is marked untrusted whatever its score: `structuredContent.untrusted = true` and a `<<untrusted tool output>>` prefix on text. The score and signals go in `_meta.mcpgw.injection`.
- Rules were tuned on the dev partition only. The test partition was evaluated after tuning (`docs/DECISIONS.md`).

The measured result is in `evals/REPORT.md` (from `make eval`): recall and false positive rate on `injection.v1`, with the gate in the same file. The report notes that the test partition shares templates with dev, so its recall measures new fills of known phrasings, not new phrasings (`docs/KNOWN_LIMITATIONS.md`).

## Alternatives considered

- **An ML classifier.** Rejected: it needs a model call or a bundled model, which breaks the rule that the gateway calls no model. It would also add latency to every result and make scores change between versions.
- **Block on any suspicious pattern.** Rejected: false positives would stop legitimate notes. The default is to mark and score, and block mode is opt-in.

## Consequences

- Scoring is defence in depth, not a solution. A paraphrase the rules do not cover will score low. The controls that hold are untrusted marking, scopes, policy and approvals (`docs/security.md`).
- Known misses on dev include exfiltration instructions that name an email address and forged chat transcripts (`docs/KNOWN_LIMITATIONS.md`).
- The scorer is a pure function. Its fast paths are checked against the plain regular expressions by `TestRulePrefiltersAreExact` and `TestTokensMatchRegexp`.
- Changing a rule needs a new `make eval` run. The gate fails any regression beyond the allowed delta (`scripts/evalgate_check.py`).
