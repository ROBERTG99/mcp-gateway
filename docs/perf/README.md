# Performance results

Raw outputs behind the performance numbers in the docs, from the 2026-10-17 runs on Claude Code on the web: 4 vCPUs (Intel Xeon @ 2.10 GHz), 16 GB RAM, with the whole compose stack (Postgres, Redis, three upstreams, the gateway, the issuer, the webhook sink, Prometheus, Grafana, Jaeger) and the load generator on the same machine.

| File | Command | Result |
|---|---|---|
| `bench.txt` | `make bench` | overhead p95 0.409 ms (NFR-1 < 5 ms), console audit page p95 8.919 ms over 100,000 rows (NFR-8 < 300 ms) |
| `loadtest.json` | `make loadtest` (30 s, 64 workers, 80% reads) | 642.8 calls/s, 0 errors, gateway RSS max 87.3 MB; NFR-7 (< 128 MB) and the CI budget (>= 500 calls/s) pass, NFR-2 (>= 2,000 calls/s on 8 vCPUs) is not met on this machine (`docs/KNOWN_LIMITATIONS.md`) |

`TestStartupUnder2s` (NFR-5) logs the time to ready; it was 14.7 ms (`go test -tags=integration -run TestStartupUnder2s -v ./internal/server`).
