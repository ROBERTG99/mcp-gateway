#!/usr/bin/env bash
# make kind-smoke (optional, outside make verify): install the chart on a throwaway local kind
# cluster in a namespace that enforces the "restricted" Pod Security Standard, and check that the
# gateway pods are admitted and created. Postgres, Redis and an issuer are not installed, so the
# pods are not expected to become ready. Needs kind, kubectl and helm; touches no cloud account.
set -euo pipefail
export PATH="$PWD/bin:$PATH"
cluster=${KIND_CLUSTER:-mcpgw-smoke}
ns=mcpgw-smoke
for tool in kind kubectl helm docker; do
  command -v "$tool" >/dev/null || { echo "kind-smoke: $tool not found"; exit 1; }
done
created=0
if ! kind get clusters | grep -qx "$cluster"; then
  kind create cluster --name "$cluster" --wait 120s
  created=1
fi
cleanup() { if [ "$created" = 1 ]; then kind delete cluster --name "$cluster"; fi; }
trap cleanup EXIT
ctx="kind-$cluster"
docker build --network host --target gateway -t ghcr.io/robertg99/mcp-gateway:0.1.0-smoke .
kind load docker-image --name "$cluster" ghcr.io/robertg99/mcp-gateway:0.1.0-smoke
kubectl --context "$ctx" create namespace "$ns" --dry-run=client -o yaml | kubectl --context "$ctx" apply -f -
kubectl --context "$ctx" label namespace "$ns" --overwrite pod-security.kubernetes.io/enforce=restricted
kubectl --context "$ctx" -n "$ns" create secret generic mcp-gateway --dry-run=client -o yaml \
  --from-literal=MCPGW_DATABASE_URL=postgres://mcpgw_app:smoke@postgres:5432/mcpgw \
  --from-literal=MCPGW_REDIS_URL=redis://redis:6379/0 | kubectl --context "$ctx" apply -f -
helm upgrade --install gw deploy/helm/mcp-gateway --kube-context "$ctx" -n "$ns" \
  --set image.tag=0.1.0-smoke --set migrations.enabled=false --wait=false
kubectl --context "$ctx" -n "$ns" rollout status deployment/gw-mcp-gateway --timeout=5s || true
pods=$(kubectl --context "$ctx" -n "$ns" get pods -l app.kubernetes.io/instance=gw -o name | wc -l)
if [ "$pods" -lt 2 ]; then
  kubectl --context "$ctx" -n "$ns" get events --field-selector reason=FailedCreate
  echo "kind-smoke: FAIL ($pods pods admitted under the restricted Pod Security Standard)"
  exit 1
fi
echo "kind-smoke: PASS ($pods gateway pods admitted under the restricted Pod Security Standard)"
