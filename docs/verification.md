# Verification

Final verification of v0.1.0 against `docs/specs/000-build-spec.md` §19. This file records what was run, on
what, and with which result. A step that was not run says so.

## Environment

- Date: 2026-10-17.
- Machine: Claude Code on the web, Linux 6.18, 4 vCPUs (Intel Xeon @ 2.10 GHz).
- Toolchains: Docker 29.8.2, Go 1.27.2, Python 3.13 with uv 0.11.32, Node 22 with pnpm 10.28.0 (versions in
  `docs/DEVELOPMENT.md`).
- No `.env` file and no model API keys: the gateway calls no model.

## Results so far

| Check | Command | Result | Duration |
|---|---|---|---|
| Stack up from an empty build cache | `make up` | every service healthy | 119 s |
| Gateway image | `make image-size` | 17.6 MB compressed (budget < 40 MB), 57.6 MB unpacked | 5 s |
| Seed | `make seed` | `seeded datagen.v1 … accounts=600 customers=600 orders=2000 notes=1500` | 3 s |
| Demo | `make demo` | PASS steps 1-7 and the restore, transcript below | 35 s |
| Full suite in containers | `make test-docker` | all Go packages ok (`go test -race -tags=integration ./...`), 95 Python tests passed | 3 min 49 s |
| Integration on the host | `make integration` | all packages ok | about 1 min |
| Helm | `make helm-check` | lint and kubeconform clean for both values files, render tests ok | 3 s |
| Alerts | `make promtool` | 6 rules, all rule tests pass | 1 s |
| Benchmarks | `make bench` | overhead p95 0.398 ms, audit page p95 11.79 ms, both within budget (`docs/perf/bench.txt`) | 30 s |
| Load | `make loadtest-profile` | 630.0 calls/s, 0 errors, RSS 93.9 MB (`docs/perf/loadtest.json`) | 40 s |
| Docs | `make docs` | Schemathesis 824 generated, 824 passed; tsc clean; 45 screenshot tests; approval-flow video | about 3 min |

`make verify-clean` (§19 step 1) runs last and is recorded in the next section.

### Demo transcript (`make demo`)

```text
PASS step 1: only allowed tools are listed (crm.get_customer, orders.get_order, orders.refund_order)
PASS step 2: USD 50 refund executes (O-00002 refund_id 42205)
PASS step 3: USD 500 refund is held, approved and the retry executes once (approval 10273193-95f7-4881-a0f2-c81f6b617493)
PASS step 4: orders call fails mid-flight; the same-key retry leaves one refund (one refund after 2 attempts with key demo-stall-1792250010914377628)
PASS step 5: injected CRM note is flagged as untrusted (score 0.55 signals override_phrase,tool_reference)
PASS step 6: audit chain verifies (143786 events verified)
PASS step 7: tampered audit row is detected at its seq (verify failed at seq 90172 as expected)
PASS restore: seq 90172 restored; mcp-gateway audit verify passes again
```

## NFR budgets

| NFR | Budget | Result | Source |
|---|---|---|---|
| NFR-1 overhead p95 | < 5 ms | 0.398 ms, met | `make bench`, `docs/perf/bench.txt` |
| NFR-2 throughput | ≥ 2,000 calls/s on 8 vCPUs (CI ≥ 500) | 630.0 calls/s on 4 vCPUs: **not met** (CI budget met) | `make loadtest-profile`, `docs/perf/loadtest.json` |
| NFR-5 startup | < 2 s | 22.2 ms, met | `go test -tags=integration -run TestStartupUnder2s -v ./internal/server`, `docs/perf/startup.txt` |
| NFR-7 RSS | < 128 MB at 2,000 calls/s | 93.9 MB at 630 calls/s: **not verified** at 2,000 calls/s | `make loadtest-profile`, `docs/perf/loadtest.json` |
| NFR-8 console | < 300 ms p95 at 100,000 rows | 11.79 ms, met | `make bench`, `docs/perf/bench.txt` |
| NFR-10 image and start | < 40 MB; `make up` < 5 min | 17.6 MB compressed (57.6 MB unpacked); 119 s, met | `make image-size`, `make up` |

Why NFR-2 and NFR-7 fall short, and what it would take, is in `docs/KNOWN_LIMITATIONS.md`.
