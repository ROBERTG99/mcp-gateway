---
name: add-eval-case
description: Add labelled eval cases by extending the deterministic generator, never by editing the test partition.
---

# Add eval cases

1. Extend `internal/datagen/evals.go` (the generator), not the JSONL files.
2. Regenerate: `go run ./cmd/datagen evals --out evals/datasets` (seed 20260). The generator refuses to change an existing test file (`ErrTestPartitionChanged`), so a case that would alter `evals/datasets/test/` needs a new dataset version (e.g. `injection.v2`), not an edit.
3. The guard hook blocks edits to existing files in `evals/datasets/test/`; regenerating new files is the only path.
4. Run `make eval` and update `evals/baseline/` only with a recorded reason.
