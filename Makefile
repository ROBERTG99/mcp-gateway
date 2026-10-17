# mcp-gateway Makefile. Run `make help` for the target list.
SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

BIN := $(CURDIR)/bin
export PATH := $(BIN):$(PATH)
export GOBIN := $(BIN)
GO ?= go
COMPOSE ?= docker compose
PNPM ?= pnpm
UV ?= uv

# Pinned tool versions (installed into ./bin by `make bootstrap`).
GOLANGCI_LINT_VERSION := v2.15.0
GOIMPORTS_VERSION := v0.52.0
GOSEC_VERSION := v2.29.0
GOVULNCHECK_VERSION := v1.9.0
ACTIONLINT_VERSION := v1.7.12
GITLEAKS_VERSION := v8.30.1
KUBECONFORM_VERSION := v0.8.0
PROMTOOL_VERSION := v3.15.0
GO_LICENSES_VERSION := v2.0.1
HELM_VERSION := v3.22.1
HADOLINT_VERSION := v2.14.0
TRIVY_VERSION := v0.76.0
export GOLANGCI_LINT_VERSION GOIMPORTS_VERSION GOSEC_VERSION GOVULNCHECK_VERSION ACTIONLINT_VERSION \
	GITLEAKS_VERSION KUBECONFORM_VERSION PROMTOOL_VERSION GO_LICENSES_VERSION HELM_VERSION \
	HADOLINT_VERSION TRIVY_VERSION

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
LDFLAGS := -s -w -X github.com/ROBERTG99/mcp-gateway/internal/version.version=$(VERSION) \
	-X github.com/ROBERTG99/mcp-gateway/internal/version.commit=$(COMMIT)

GO_PKGS := ./...
TENANT ?= t_alpha
AGENT ?= agent-support
SCOPES ?= ledger:read,ledger:write,crm:read,crm:write,orders:read,orders:write
DEV_ISSUER_URL ?= http://localhost:8212
GATEWAY_URL ?= http://localhost:8210/mcp
ADMIN_URL ?= http://localhost:8200
METRICS_URL ?= http://localhost:8211/metrics
LIVE_EVAL_BUDGET_USD ?= 2

.PHONY: help
help: ## List targets
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-16s %s\n", $$1, $$2}'

##@ Setup
.PHONY: bootstrap
bootstrap: tools ## Install toolchains and dependencies into ./bin, create .env
	$(GO) mod download
	$(UV) sync --frozen
	cd e2e && $(PNPM) install --frozen-lockfile
	@test -f .env || cp .env.example .env

.PHONY: tools
tools: ## Install pinned developer tools into ./bin (idempotent)
	./scripts/install_tools.sh

.PHONY: build
build: ## Build all binaries into ./bin
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN)/ ./cmd/...

##@ Local stack
.PHONY: up
up: ## Start the full stack in Docker Compose (healthchecks)
	$(COMPOSE) up -d --build --wait

.PHONY: observability
observability: ## Stack plus Prometheus (8250), Grafana (8251) and Jaeger (8253); traces exported to Jaeger
	MCPGW_OTEL_ENDPOINT=http://jaeger:4318 $(COMPOSE) --profile observability up -d --build --wait

.PHONY: down
down: ## Stop the stack
	$(COMPOSE) --profile tools --profile dynamodb --profile observability --profile chaos down

.PHONY: logs
logs: ## Follow logs of every service
	$(COMPOSE) logs -f

.PHONY: seed
seed: ## Load the synthetic dataset (fixed seed)
	$(COMPOSE) --profile tools run --rm seed

.PHONY: migrate
migrate: ## Apply goose migrations (up)
	$(COMPOSE) run --rm migrate

.PHONY: demo
demo: ## Run the scripted demo scenario against the running stack
	./scripts/demo.sh

.PHONY: dev-token
dev-token: ## Mint a dev token (TENANT, AGENT, SCOPES)
	@$(GO) run ./cmd/mcp-gateway dev-token --issuer $(DEV_ISSUER_URL) --tenant $(TENANT) --agent $(AGENT) --scopes $(SCOPES)

.PHONY: audit-verify
audit-verify: ## Verify the audit hash chain on the stack
	$(COMPOSE) exec -T gateway /mcp-gateway audit verify

##@ Quality
.PHONY: check-fast
check-fast: fmt-check vet golangci unit ## Lint and unit tests (< 60 s)

.PHONY: fmt
fmt: ## Format Go, Rego and Python sources
	gofmt -w $$(git ls-files '*.go')
	goimports -local github.com/ROBERTG99/mcp-gateway -w $$(git ls-files '*.go')
	$(GO) run ./cmd/mcp-gateway policy fmt policies
	$(UV) run ruff format .claude/hooks scripts tests

.PHONY: fmt-check
fmt-check:
	@out=$$(gofmt -l $$(git ls-files '*.go' | grep -v '^vendor/')); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

.PHONY: vet
vet:
	$(GO) vet $(GO_PKGS)

.PHONY: golangci
golangci:
	golangci-lint run --timeout 5m

.PHONY: lint
lint: fmt-check golangci ## All linters (Go, Rego, Python, TS, Markdown, Actions)
	$(GO) run ./cmd/mcp-gateway policy fmt --check policies
	$(UV) run ruff check .claude/hooks scripts tests
	$(UV) run ruff format --check .claude/hooks scripts tests
	cd e2e && $(PNPM) exec eslint . && $(PNPM) exec prettier --check .
	e2e/node_modules/.bin/markdownlint-cli2
	actionlint

.PHONY: typecheck
typecheck: vet ## go vet, tsc --noEmit, mypy --strict
	cd e2e && $(PNPM) exec tsc --noEmit
	$(UV) run mypy --strict .claude/hooks scripts tests

.PHONY: security
security: ## gitleaks, gosec, govulncheck, trivy, hadolint, actionlint, pnpm audit, go-licenses
	./scripts/security.sh

##@ Tests
.PHONY: unit
unit: ## Unit tests with the race detector
	$(GO) test -race -count=1 -short $(GO_PKGS)

.PHONY: integration
integration: ## Integration tests (testcontainers or compose services)
	$(GO) test -race -count=1 -tags=integration ./tests/integration/... ./internal/... ./pkg/... ./cmd/...

.PHONY: contract
contract: ## Contract tests (golden JSON, Schemathesis)
	$(GO) test -count=1 -tags=integration ./tests/contract/...
	./scripts/schemathesis.sh

.PHONY: policy-test
policy-test: ## Rego unit tests and golden decisions
	$(GO) run ./cmd/mcp-gateway policy test policies
	$(GO) test -count=1 ./internal/policy/...

.PHONY: hooks-test
hooks-test: ## Claude Code hook tests and the other Python tests (eval gate, budgets, Docker setup)
	$(UV) run pytest

.PHONY: test
test: unit integration contract policy-test hooks-test ## Unit, integration, contract, policy and hook tests on the host

.PHONY: coverage
coverage: ## Coverage report with thresholds (85% core, 70% overall)
	$(GO) test -count=1 -tags=integration -covermode=atomic -coverpkg=./internal/...,./pkg/... -coverprofile=coverage.out ./internal/... ./pkg/... ./tests/integration/...
	$(UV) run python scripts/coverage_check.py coverage.out

.PHONY: test-docker
# Runs next to a running stack (only postgres, redis and dynamodb are needed; tests use their own
# databases), so `make verify` can call it between `up` and `down`.
COMPOSE_TEST = $(COMPOSE) -f compose.yaml -f compose.test.yaml --profile dynamodb --profile test
test-docker: ## Full test suite inside containers
	$(COMPOSE_TEST) build go-tests
	$(COMPOSE_TEST) up -d --wait postgres redis dynamodb
	$(COMPOSE_TEST) run --rm go-tests
	$(COMPOSE_TEST) run --rm harness-tests

.PHONY: image-size
image-size: ## Gateway image size: compressed (budget < 40 MB) and unpacked
	./scripts/image_size.sh mcp-gateway:dev 40

.PHONY: e2e
e2e: ## Playwright and axe against the running stack
	cd e2e && $(PNPM) exec playwright test

.PHONY: bench
# BenchmarkAuditPage (NFR-8) needs Docker (testcontainers Postgres with 100,000 audit rows).
bench: ## Benchmarks with budgets (NFR-1 overhead, NFR-8 console)
	set -o pipefail; $(GO) test -tags=integration -run '^$$' -bench . -benchtime=2000x -count=1 ./internal/pipeline/ ./internal/console/ | tee bench.out
	$(UV) run python scripts/bench_budget.py bench.out

.PHONY: loadtest
# t_alpha's token buckets are raised (config/tenants.loadtest.yaml) so the run measures the gateway,
# not the configured limit; the stack is restored afterwards. LOADTEST_ARGS: e.g. --duration 60s.
LOADTEST_ENV = MCPGW_TENANTS_FILE=/config/tenants.loadtest.yaml
loadtest: ## Load test against the stack (NFR-2, NFR-7); writes loadtest.json and checks budgets
	$(LOADTEST_ENV) $(COMPOSE) up -d --wait gateway
	$(GO) run ./cmd/loadgen --mode load --gateway $(GATEWAY_URL) --issuer $(DEV_ISSUER_URL) --metrics $(METRICS_URL) \
	  --report loadtest.json $(LOADTEST_ARGS) && $(UV) run python scripts/bench_budget.py --load loadtest.json; \
	  status=$$?; $(COMPOSE) up -d --wait gateway; exit $$status

.PHONY: loadtest-profile
# make loadtest with scripts/cpu_usage.py sampling the measured window (5-30 s after start): average
# cores per compose service and host process (loadgen, docker-proxy), written to cpu.txt.
loadtest-profile: ## Load test plus CPU per service and process (cpu.txt)
	$(GO) build -o $(BIN)/loadgen ./cmd/loadgen
	$(LOADTEST_ENV) $(COMPOSE) up -d --wait gateway
	( $(UV) run python scripts/cpu_usage.py --delay 5 --seconds 25 > cpu.txt ) & \
	  $(BIN)/loadgen --mode load --gateway $(GATEWAY_URL) --issuer $(DEV_ISSUER_URL) --metrics $(METRICS_URL) \
	  --report loadtest.json $(LOADTEST_ARGS); status=$$?; wait; cat cpu.txt; $(COMPOSE) up -d --wait gateway; exit $$status

.PHONY: chaos
# 10% of refund keys stall 3 s inside the orders upstream, past a 2 s gateway timeout; the breaker
# cooldown is 5 s so the run is not dominated by waits. The stack is restored afterwards.
CHAOS_ENV = MCPGW_FAULT_TIMEOUT_RATE=0.1 MCPGW_FAULT_STALL=3s MCPGW_UPSTREAM_TIMEOUT=2s MCPGW_BREAKER_COOLDOWN=5s
chaos: ## Chaos eval against the stack (10,000 calls); writes evals/reports/chaos.v1.json
	$(CHAOS_ENV) $(COMPOSE) up -d --wait upstream-orders gateway
	$(CHAOS_ENV) $(GO) run ./cmd/loadgen --mode chaos --gateway $(GATEWAY_URL) --issuer $(DEV_ISSUER_URL) --admin $(ADMIN_URL) --calls 10000; \
	  status=$$?; $(COMPOSE) up -d --wait upstream-orders gateway; exit $$status

##@ Evals
.PHONY: eval
eval: ## Offline evals with thresholds; writes evals/REPORT.md
	$(GO) run ./cmd/evalgen run --out evals/reports
	$(UV) run python scripts/evalgate_check.py --baseline evals/baseline --reports evals/reports --max-regression-pp 1.0

.PHONY: eval-live
eval-live: ## Live evals within LIVE_EVAL_BUDGET_USD (estimated cost USD 0.00: no model is called)
	@echo "estimated cost USD 0.00 (budget USD $(LIVE_EVAL_BUDGET_USD)); this repository calls no model"
	$(GO) run ./cmd/evalgen run --live --gateway $(GATEWAY_URL) --issuer $(DEV_ISSUER_URL) --admin $(ADMIN_URL) --out evals/reports-live

##@ Deploy checks
.PHONY: helm-check
helm-check: ## helm lint and kubeconform, both values files
	./scripts/helm_check.sh

.PHONY: promtool
promtool: ## Prometheus alert rule tests
	promtool check rules deploy/prometheus/alerts.yaml
	promtool test rules deploy/prometheus/alerts_test.yaml

.PHONY: kind-smoke
kind-smoke: ## Optional chart install on kind (outside make verify)
	./scripts/kind_smoke.sh

##@ Gates
.PHONY: verify
verify: lint typecheck security test coverage policy-test hooks-test helm-check promtool eval bench ## The single gate (everything except eval-live)
	$(MAKE) up
	$(MAKE) image-size
	$(MAKE) seed
	$(MAKE) demo
	$(MAKE) e2e
	$(MAKE) test-docker
	$(MAKE) down

.PHONY: verify-clean
verify-clean: ## make verify in a fresh clone in a temporary folder
	./scripts/verify_clean.sh

##@ Docs
.PHONY: docs
docs: ## Regenerate diagrams, OpenAPI checks, frontend types and screenshots
	$(GO) run ./cmd/mcp-gateway version >/dev/null
	./scripts/docs.sh

.PHONY: clean
clean: ## Remove containers, volumes, caches and build outputs
	-$(COMPOSE) --profile tools --profile dynamodb --profile observability --profile chaos down -v --remove-orphans
	rm -rf dist coverage*.out coverage.html bench.out loadtest.json cpu.txt e2e/test-results e2e/playwright-report
	$(GO) clean -testcache
