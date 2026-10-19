#!/usr/bin/env bash
# make verify-clean (spec §19 step 1): clone the committed HEAD into a temporary folder and run
# `make bootstrap && make verify` there with no .env file and no model API keys. The clone uses the
# same compose project and host ports, so the stack of this checkout must be down first.
set -euo pipefail
cd "$(dirname "$0")/.."
dest=${VERIFY_DIR:-$(mktemp -d /tmp/verify-mcp-gateway.XXXXXX)}
if curl -fsS -o /dev/null http://localhost:8200/healthz 2>/dev/null; then
  echo "verify-clean: a stack is running on :8200; run make down first" >&2
  exit 1
fi
if [[ -n "$(git status --porcelain --untracked-files=no)" ]]; then
  echo "verify-clean: uncommitted changes would not be verified; commit first" >&2
  exit 1
fi
if [[ -n "$(ls -A "$dest" 2>/dev/null)" ]]; then
  echo "verify-clean: $dest is not empty" >&2
  exit 1
fi
git clone --quiet . "$dest"
cd "$dest"
# Reuse the tools already installed here (same pinned versions, checked by their version stamps)
# unless VERIFY_COLD=1, which makes make bootstrap download every tool again.
if [[ "${VERIFY_COLD:-}" != "1" && -d "$OLDPWD/bin" ]]; then cp -a "$OLDPWD/bin" .; fi
test ! -e .env
echo "verify-clean: $(git rev-parse HEAD) in $dest"
no_keys=(env -u ANTHROPIC_API_KEY -u OPENAI_API_KEY -u CLAUDE_API_KEY -u GEMINI_API_KEY)
"${no_keys[@]}" make bootstrap
"${no_keys[@]}" make verify
