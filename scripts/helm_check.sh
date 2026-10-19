#!/usr/bin/env bash
# make helm-check: helm lint --strict and kubeconform -strict on deploy/helm/mcp-gateway with each
# values file, then the render tests (tests/helm: security context, objects, schema rejections).
set -euo pipefail
export PATH="$PWD/bin:$PATH"
chart=deploy/helm/mcp-gateway
kube=1.31.0 # keep in step with tests/helm/helm_render_test.go
for values in values.yaml values-dynamodb.yaml; do
  echo "== $values"
  helm lint --strict --kube-version "$kube" "$chart" -f "$chart/$values"
  helm template gw "$chart" --namespace mcpgw --kube-version "$kube" -f "$chart/$values" |
    kubeconform -strict -summary -kubernetes-version "$kube"
done
go test -count=1 -tags=integration ./tests/helm/
