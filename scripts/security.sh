#!/usr/bin/env bash
# make security: secret scan, Go SAST and vulnerability check, filesystem and image scan, Dockerfile
# and workflow lint, npm audit for e2e/, and license allowlist. Fails on any finding not documented
# in docs/security.md ("Accepted findings").
set -uo pipefail
export PATH="$PWD/bin:$PATH"
status=0
step() {
  echo "== $1"
  shift
  if "$@"; then echo "   ok"; else echo "   FAILED: $*"; status=1; fi
}

step "gitleaks (working tree and history; .gitleaks.toml)" gitleaks detect --source . --config .gitleaks.toml --redact --no-banner --exit-code 1
# gosec through golangci-lint, so reviewed //nolint:gosec justifications apply (bin/gosec does not read them).
step "gosec (golangci-lint)" golangci-lint run --enable-only gosec ./...
echo "== govulncheck"
if out=$(govulncheck ./... 2>&1); then
  echo "   ok"
elif grep -q "fetching vulnerabilities" <<<"$out" && [ "${CI:-}" != "true" ]; then
  echo "   NOT RUN: vuln.go.dev is unreachable from this environment (fails under CI=true; docs/security.md)"
else
  echo "$out"; echo "   FAILED: govulncheck"; status=1
fi
step "trivy filesystem (vulnerabilities, secrets, misconfigurations; high and critical)" \
  trivy fs --quiet --scanners vuln,secret,misconfig --severity HIGH,CRITICAL --exit-code 1 \
  --skip-dirs e2e/node_modules --skip-dirs bin .
# The image is built when missing (make verify runs security before up), so it is always scanned.
if ! docker image inspect mcp-gateway:dev >/dev/null 2>&1; then
  step "build mcp-gateway:dev for the image scan" \
    docker build --network host --build-arg HTTPS_PROXY="${HTTPS_PROXY:-}" --target gateway -t mcp-gateway:dev .
fi
step "trivy image mcp-gateway:dev (high and critical)" \
  trivy image --quiet --severity HIGH,CRITICAL --exit-code 1 --ignore-unfixed mcp-gateway:dev
step "hadolint" hadolint Dockerfile
if compgen -G ".github/workflows/*.y*ml" >/dev/null; then
  step "actionlint" actionlint
else
  echo "== actionlint: no workflows yet; skipped"
fi
step "pnpm audit (e2e, high and critical)" bash -c 'cd e2e && pnpm audit --audit-level high'
# segmentio/asm is MIT (checked by hand; its LICENSE file is not recognised by the classifier).
step "go-licenses (allowlist)" env GOROOT="$(go env GOROOT)" go-licenses check ./... \
  --allowed_licenses=Apache-2.0,MIT,BSD-2-Clause,BSD-3-Clause,ISC,MPL-2.0 \
  --ignore github.com/ROBERTG99/mcp-gateway --ignore github.com/segmentio/asm
exit $status
