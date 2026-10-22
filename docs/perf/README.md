# Performance results

Raw outputs behind the performance numbers in the docs, from the 2026-10-17 runs in a cloud development container:
4 vCPUs (Intel Xeon @ 2.10 GHz), with the compose stack (Postgres, Redis, three upstreams, the gateway, the
issuer, the webhook sink, dynamodb-local) and the load generator on the same machine.

| File | Command | Result |
|---|---|---|
| `bench.txt` | `make bench` | overhead p95 0.398 ms (NFR-1 < 5 ms); console audit page p95 11.79 ms over 100,000 rows (NFR-8 < 300 ms) |
| `loadtest.json` | `make loadtest-profile` (3 s warm-up, 30 s measured, 64 workers over 16 sessions, 80% reads, 20% refunds with fresh keys) | 630.0 calls/s, 0 errors, gateway RSS max 93.9 MB. The CI budget (>= 500 calls/s) passes. NFR-2 (>= 2,000 calls/s on 8 vCPUs) is **not met**, and NFR-7 (< 128 MB) is met only at this rate, not verified at 2,000 calls/s (`docs/KNOWN_LIMITATIONS.md`) |
| `cpu.txt` | `make loadtest-profile` (`scripts/cpu_usage.py` over seconds 5-30 of the same run) | average cores per compose service and host process; the machine had 3.52 of 4 cores busy |
| `startup.txt` | `go test -tags=integration -run TestStartupUnder2s -v ./internal/server` | ready after 22.2 ms (NFR-5 < 2 s) |
| `scorer.txt` | `go test -count=3 -run '^$' -bench BenchmarkScoreToolResult ./internal/injection` | 0.23-0.24 ms per scored result, with the stack running |

For the load test, `make loadtest` and `make loadtest-profile` restart the gateway with
`config/tenants.loadtest.yaml`, where t_alpha's token buckets are far above the target rate, so the run
measures the gateway rather than the configured rate limit; the stack is restored afterwards
(`docs/DECISIONS.md`). `make image-size` prints the image size.
