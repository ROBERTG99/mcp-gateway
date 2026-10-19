#!/usr/bin/env bash
# make docs, against the running stack (make up && make seed):
#   - diagrams: the Mermaid sources live in the Markdown files (GitHub renders them); markdownlint checks them
#   - OpenAPI: api/openapi.yaml against the live admin API (scripts/schemathesis.sh)
#   - frontend types: tsc --noEmit over e2e/ (the console is server-rendered and has no generated types)
#   - screenshots: every console page at 375, 768 and 1280 px in both themes, into docs/images/
#   - video: the keyboard-only approval flow, into docs/images/console-approval-flow.webm
set -euo pipefail
cd "$(dirname "$0")/.."
ADMIN_URL=${ADMIN_URL:-http://localhost:8200}
if ! curl -fsS "$ADMIN_URL/readyz" >/dev/null; then
  echo "docs: the stack is not ready at $ADMIN_URL (run make up && make seed)" >&2
  exit 1
fi
echo "== diagrams (Mermaid in Markdown)"
e2e/node_modules/.bin/markdownlint-cli2
echo "== OpenAPI contract"
./scripts/schemathesis.sh
echo "== frontend types"
(cd e2e && pnpm exec tsc --noEmit)
echo "== screenshots"
(cd e2e && SCREENSHOTS=1 pnpm exec playwright test tests/console.spec.ts)
echo "== video"
rm -rf e2e/test-results
(cd e2e && VIDEO=1 pnpm exec playwright test tests/approvals.spec.ts -g "keyboard only")
video=$(find e2e/test-results -name '*.webm' -printf '%s %p\n' | sort -rn | head -n1 | cut -d' ' -f2-) # the page, not the axe helper pages
if [[ -z "$video" ]]; then
  echo "docs: no video recorded" >&2
  exit 1
fi
cp "$video" docs/images/console-approval-flow.webm
echo "docs: screenshots and docs/images/console-approval-flow.webm updated"
