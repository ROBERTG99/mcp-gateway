# Development

## Build environment (recorded at bootstrap, 2026-10-16)

| Item | Value |
|---|---|
| Environment | Claude Code on the web (`CLAUDE_CODE_REMOTE=true`), Appendix A of the spec applied |
| OS | Ubuntu 24.04.5 LTS, Linux 6.18 x86_64 |
| CPU | 4 vCPU, Intel Xeon @ 2.10 GHz |
| RAM | 16 GB (`/proc/meminfo` MemTotal 16479424 kB) |
| Go | host 1.24.7; module toolchain `go1.27.3` (downloaded through the Go proxy by `GOTOOLCHAIN=auto`) |
| Node / pnpm | 22.22.0 / 10.28.0 |
| Python / uv | 3.13.16 / 0.11.32 |
| Docker | 29.8.2 (daemon started by hand, `nohup dockerd &`, with the `mirror.gcr.io` registry mirror; the SessionStart hook does it once `.claude/settings.json` is installed) |
| PostgreSQL client and server | 16.15 (host binaries; the stack uses `pgvector/pgvector:pg16`) |
| Redis | 7.0.15 (host); the stack uses `redis:7` |
| Chromium | `/opt/pw-browsers` (Playwright, preinstalled). `make docs` records a video, and Playwright 1.64 looks for `/opt/pw-browsers/ffmpeg-1013/ffmpeg-linux`, while the image ships only `ffmpeg-1011`, which has no VP9 encoder. In the cloud container, make that path a two-line shell wrapper that runs `exec /usr/bin/ffmpeg "$@"` (never `playwright install`). |
| Helm, kubeconform, golangci-lint, gosec, govulncheck, actionlint, gitleaks, promtool, go-licenses | missing at bootstrap; installed into `./bin` by `make bootstrap` (`go install`) |
| hadolint, trivy | missing at bootstrap; downloaded from GitHub releases into `./bin` by `make bootstrap` |
| Terraform, kubectl, kind | missing; not needed (no Terraform in this repository, `make kind-smoke` is optional) |

Network notes (Appendix A): `get.helm.sh` is blocked, so Helm is built with `go install helm.sh/helm/v3/cmd/helm`. Docker Hub pulls go through `mirror.gcr.io`.

## Getting started

```bash
make bootstrap        # tools into ./bin, Go/pnpm/uv dependencies, .env from .env.example
make check-fast       # lint and unit tests (< 60 s)
make up && make seed  # full stack in Docker Compose, synthetic data
make demo             # scripted scenario, prints PASS per step
```

Ports are in the 8200-8299 range (see `compose.yaml`; host ports are bound to 127.0.0.1).

## Conventions

- Test first: every task starts with a failing test (see `.claude/skills/tdd/SKILL.md`).
- Go: errors wrapped with `%w`, `context.Context` first, no package-level mutable state, `go test -race` clean, injectable clock (`internal/clock`).
- Commits: Conventional Commits, authored by the maintainer, no AI attribution trailers (`.claude/rules/git-commits.md`).
- Progress: `PROGRESS.md`; decisions: `docs/DECISIONS.md`; architecture decisions: `docs/adr/`.
