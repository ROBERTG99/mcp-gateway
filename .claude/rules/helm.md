---
paths: ["deploy/helm/**"]
---

# Helm rules

- Pods run as non-root with `readOnlyRootFilesystem: true`, `allowPrivilegeEscalation: false`, all capabilities dropped and the `RuntimeDefault` seccomp profile.
- Every workload has a NetworkPolicy, resource requests and limits, and probes on `/healthz` and `/readyz`.
- Every value is described in `values.schema.json`; `make helm-check` (helm lint and kubeconform -strict) must pass for both values files.
- Secrets are referenced by name, never inlined in values.
