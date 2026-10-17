# Runbook

For operators of mcp-gateway. It covers install with Helm, policy rollback, key rotation, audit archival, one section per alert in `deploy/prometheus/alerts.yaml`, and three incident drills.

Every environment variable, endpoint and metric named here exists in the code. Defaults come from `internal/config/config.go` (also listed in `.env.example`). Alert thresholds come from `deploy/prometheus/alerts.yaml`. Nothing in this runbook was run against a real cluster or a real AWS account: `make kind-smoke` was not run in the build environment (`docs/KNOWN_LIMITATIONS.md`). The drills in the last section were written for the Docker Compose stack.

## Before you start

| Port | What | Source |
|---|---|---|
| 8210 | MCP endpoint (`/mcp`), bearer JWT | `MCPGW_MCP_ADDR` |
| 8200 | Admin API (`/admin/...`), console, `/healthz`, `/readyz` | `MCPGW_ADMIN_ADDR` |
| 8211 | Prometheus metrics (`/metrics`) | `MCPGW_METRICS_ADDR` |

The commands below assume a Helm release named `gw` in namespace `mcpgw`. With that name the chart's objects are called `gw-mcp-gateway` (`deploy/helm/mcp-gateway/templates/_helpers.tpl`, `mcpgw.fullname`). Change the names to match your release.

Admin API calls need a bearer token from your issuer with the scope named in each section (`mcpgw:admin` for reload, upstreams and chain verification). Put it in `ADMIN_TOKEN`. To reach the ports from a workstation:

```bash
kubectl -n mcpgw port-forward svc/gw-mcp-gateway 8200:8200 8211:8211
```

Useful reads:

```bash
curl -s http://127.0.0.1:8200/readyz
curl -s http://127.0.0.1:8211/metrics | grep '^mcpgw_'
curl -s -H "Authorization: Bearer $ADMIN_TOKEN" http://127.0.0.1:8200/admin/upstreams
```

`/readyz` answers 200 or 503 with `{"ready": ..., "checks": {"policy", "postgres", "redis", "audit"}}` (`internal/server/server.go:209`). Postgres, the policy bundle and the audit sequence gate readiness. Redis is reported but does not gate it, because reads keep working without Redis (see Drill 2). DynamoDB is not checked.

## Install with Helm

### What runs outside the chart

The chart installs the gateway only. You provide:

- Postgres 16 (the version the Compose stack and the tests use) and a schema-owner login for it;
- Redis (rate limits, daily budgets, and idempotency records unless you use DynamoDB);
- a token issuer that publishes a JWKS (RS256 keys, each with a `kid`);
- optionally a DynamoDB table (see below);
- the upstream MCP servers named in the `upstreams` value.

### The existing Secret

All secrets come from one Secret that you create before the install. Its name is `existingSecret` in `values.yaml` (default `mcp-gateway`; the values schema rejects an empty name). The gateway container reads only `MCPGW_DATABASE_URL`, `MCPGW_REDIS_URL` and `MCPGW_APPROVAL_WEBHOOK_SECRET` (optional) from it, each by key (`secretKeyRef` in `templates/deployment.yaml`). The migrate Job loads every key (`envFrom` in `templates/migrate-job.yaml`), so the schema owner's URL and the role passwords never reach the gateway pods.

The keys are listed in the comment above `existingSecret` in `deploy/helm/mcp-gateway/values.yaml`:

| Key | Used by | Notes |
|---|---|---|
| `MCPGW_DATABASE_URL` | gateway | Postgres URL for the least-privilege role `mcpgw_app` |
| `MCPGW_MIGRATE_DATABASE_URL` | migrate Job | Postgres URL of the schema owner (a superuser is needed to set role passwords) |
| `MCPGW_REDIS_URL` | gateway | Redis URL |
| `MCPGW_APPROVAL_WEBHOOK_SECRET` | gateway | HMAC key, at least 16 bytes when `approvals.webhookURL` is set (`internal/config/config.go:135`) |
| `MCPGW_APP_DB_PASSWORD` | migrate Job | Password set for role `mcpgw_app` |
| `MCPGW_READONLY_DB_PASSWORD` | migrate Job | Password set for role `mcpgw_readonly` |
| `MCPGW_UPSTREAM_DB_PASSWORD` | migrate Job | Password set for role `mcpgw_upstream` (the example upstreams only) |

An empty or missing password key leaves that role's password unchanged (`internal/store/store.go:107-110`).

Create the Secret from a file that is not committed, so values stay out of shell history:

```bash
kubectl -n mcpgw create secret generic mcp-gateway --from-env-file=mcp-gateway.secret.env
```

Because the Deployment loads the whole Secret, the gateway pods also receive `MCPGW_MIGRATE_DATABASE_URL` and the role passwords. The chart has no option to split the Secret in two. Limit who can read pod environments and Secrets in the namespace.

### Install

Write a values file with at least the issuer settings and your upstreams and tenants. The schema (`values.schema.json`) rejects unknown keys, an image tag of `latest` and an empty secret name.

```bash
helm install gw deploy/helm/mcp-gateway --namespace mcpgw --create-namespace -f my-values.yaml
```

Before you install, `make helm-check` lints and renders the chart with both shipped values files (`scripts/helm_check.sh`).

Settings you will usually change:

- `auth.jwksURL`, `auth.issuer`, `auth.audience` (become `MCPGW_JWKS_URL`, `MCPGW_JWT_ISSUER`, `MCPGW_JWT_AUDIENCE`);
- `image.repository` and `image.tag`;
- `upstreams` and `tenants` (rendered into a ConfigMap as `upstreams.yaml` and `tenants.yaml`);
- `approvals.webhookURL`, if approvals should be announced;
- `otel.endpoint` (becomes `MCPGW_OTEL_ENDPOINT`);
- `networkPolicy.mcpFrom`, `networkPolicy.adminFrom` (keep it narrower than MCP) and `networkPolicy.metricsFrom`.

The chart sets `MCPGW_CONSOLE_SECURE_COOKIES=true` and leaves `MCPGW_DEV_ISSUER_URL` empty, so console sign-in is off (`templates/NOTES.txt`). Use the admin API with tokens from your issuer.

### The migrate Job

`templates/migrate-job.yaml` is a Helm hook that runs before install and before upgrade (`helm.sh/hook: pre-install,pre-upgrade`). It runs `mcp-gateway migrate up` with the same image and Secret. Then it sets the three role passwords from the environment. Postgres runs the migrations under an advisory lock, so two migrators do not collide. The Job retries up to 3 times (`backoffLimit` in the template).

Check it:

```bash
kubectl -n mcpgw get jobs
kubectl -n mcpgw logs job/gw-mcp-gateway-migrate
```

A succeeded Job is deleted (`hook-delete-policy: before-hook-creation,hook-succeeded`), so a failed one is the one you can still read. A hook Job also blocks the install until it finishes, so a wrong `MCPGW_MIGRATE_DATABASE_URL` shows up as a failed `helm install`.

Notes:

- The Job runs before the other chart objects exist, so the Secret must already be there.
- `helm rollback` does not run the Job (the hook is only for install and upgrade). Migrations only move forward in normal operation.
- Do not run `mcp-gateway migrate down` or `reset` in production. The down step of migration 2 drops the audit table (`migrations/00002_audit.sql:45`).
- Set `migrations.enabled=false` if you run `mcp-gateway migrate up` yourself.

After the install, check readiness on a port-forward (`/readyz` should answer 200 with every check `ok`) and that `GET /admin/upstreams` lists your upstreams with `"up": true`.

### DynamoDB for idempotency records

`values-dynamodb.yaml` moves idempotency records from Redis to DynamoDB:

```bash
helm install gw deploy/helm/mcp-gateway --namespace mcpgw -f deploy/helm/mcp-gateway/values-dynamodb.yaml -f my-values.yaml
```

It sets `idempotency.store: dynamodb`, the table name `mcpgw_idempotency` and the region (`eu-west-1` in the file; change it). The chart passes them as `MCPGW_IDEMPOTENCY_STORE`, `MCPGW_DYNAMODB_TABLE` and `MCPGW_DYNAMODB_REGION`.

Create the table yourself. With a real AWS endpoint the gateway does not create it (`internal/idempotency/open.go`: `EnsureTable` runs only when `MCPGW_DYNAMODB_ENDPOINT` is set, which is meant for dynamodb-local). The store expects:

- a partition key named `pk` of type string (`internal/idempotency/dynamodb.go:54-58`);
- on-demand billing (the code creates its test table that way);
- a time-to-live setting on the attribute `expires_at`, which holds epoch seconds (`internal/idempotency/dynamodb.go:64-65`, `:81`).

With the AWS CLI that is:

```bash
aws dynamodb create-table --table-name mcpgw_idempotency --region eu-west-1 \
  --attribute-definitions AttributeName=pk,AttributeType=S \
  --key-schema AttributeName=pk,KeyType=HASH --billing-mode PAY_PER_REQUEST
aws dynamodb update-time-to-live --table-name mcpgw_idempotency --region eu-west-1 \
  --time-to-live-specification Enabled=true,AttributeName=expires_at
```

These commands follow the AWS CLI reference. They were not run in this build.

TTL deletion in DynamoDB is lazy. The gateway does not depend on it: it enforces expiry in its conditional writes (`internal/idempotency/dynamodb.go:19-20`). Records live for `MCPGW_IDEMPOTENCY_TTL` (default 24h).

Permissions: `values-dynamodb.yaml` annotates the service account with an IAM role (IRSA); replace the placeholder ARN with yours. The code calls `PutItem`, `GetItem` and `DeleteItem` (`internal/idempotency/dynamodb.go:92`, `:104`, `:157`). The comment in the values file also lists `UpdateItem`.

Things to know:

- Redis is still required. Rate limits and daily budgets use it, and a write fails when the limiter is down (see "Redis outage").
- Do not set `idempotency.dynamodb.endpoint` in production. It switches the client to fixed local credentials (`internal/idempotency/open.go`).
- `/readyz` does not probe DynamoDB, so a DynamoDB outage shows up as refused writes, not as an unready pod.

## Policy rollback

### How the policy bundle loads

The Rego bundle is a directory (`MCPGW_POLICY_DIR`). The image holds one at `/policies` (`Dockerfile`). With `policies.existingConfigMap` set, the chart mounts that ConfigMap at `/etc/mcp-gateway/policies` instead and sets `MCPGW_POLICY_DIR` to it.

The gateway checks the directory every `MCPGW_POLICY_RELOAD_INTERVAL` (default 5s) and reloads when the content hash changes (`internal/policy/policy.go:155`). A reload swaps the compiled bundle atomically. Agents then get `tools/list_changed` because the per-principal tool lists are refreshed (`docs/adr/0002-embedded-opa-vs-sidecar.md`).

The bundle hash is the SHA-256 of the loaded bundle's files (`internal/policy/policy.go:110`). You can read it from the responses of two admin calls, as `bundle_hash`:

```bash
curl -s -X POST -H "Authorization: Bearer $ADMIN_TOKEN" http://127.0.0.1:8200/admin/policy/reload
```

`POST /admin/policy/reload` needs `mcpgw:admin`. `POST /admin/policy/evaluate` returns the hash next to a decision and needs `mcpgw:admin` or `mcpgw:audit:read`. A port-forward through the Service reaches one pod. To compare replicas, port-forward to each pod. The audit table has no column for the bundle hash (`migrations/00002_audit.sql`), so write down which hash was active when you change a bundle.

### A failed reload keeps the last good bundle

- The previous bundle keeps serving. The gateway logs `policy reload failed; keeping the previous bundle` (`internal/server/server.go:282`).
- `/readyz` stays 200 and reports `"policy": "stale: last reload failed: ..."` (`internal/server/server.go:226-230`).
- `mcpgw_policy_reload_failures_total` goes up and `mcpgw_policy_stale` is 1 until a reload succeeds. The `PolicyLoadFailure` alert fires.
- `POST /admin/policy/reload` answers 422 and says that the previous bundle stays active.

If the first load at start-up fails, there is no previous bundle. Every call is denied, `/readyz` answers 503 and `mcpgw_policy_loaded` is 0. This is the fail-closed rule (`docs/adr/0002-embedded-opa-vs-sidecar.md`).

### Roll back

1. Decide which bundle to return to. Keep known-good bundles in version control.
2. Test it offline. `mcp-gateway policy test <dir>` runs the Rego tests and prints one `PASS` or `FAIL` line per test, then a summary. It exits 1 on any failure or when it finds no tests (`cmd/mcp-gateway/policy.go:37-64`). From a checkout use `make policy-test`. With the chart's image (the default `ghcr.io/robertg99/mcp-gateway:0.1.0` was not published by this build; use the tag from your registry, or `mcp-gateway:dev` from `make up`):

   ```bash
   docker run --rm -v "$PWD/policies:/policies:ro" ghcr.io/robertg99/mcp-gateway:0.1.0 policy test /policies
   ```

3. Apply it:
   - Bundle in the image: `helm rollback gw <revision> -n mcpgw`, or `helm upgrade` with the previous `image.tag`. Pods restart with that image's bundle.
   - Bundle in a ConfigMap: re-apply the previous ConfigMap content. The gateway reloads when the mounted files change.

4. Force the reload and read the hash:

   ```bash
   curl -s -X POST -H "Authorization: Bearer $ADMIN_TOKEN" http://127.0.0.1:8200/admin/policy/reload
   ```

5. Confirm the result. `/readyz` shows `"policy": "ok"`, `mcpgw_policy_stale` is 0 and `mcpgw_policy_loaded` is 1. Evaluate a known input through `POST /admin/policy/evaluate` and check the decision.

Notes:

- A ConfigMap volume exposes files through symlinks. The loader skips the hidden directories, so each file loads once (`docs/DECISIONS.md`, 2026-10-17 row on `TestLoadBundleConfigMapLayout`).
- `helm rollback` does not undo database migrations.

## Key rotation

### Issuer JWKS keys

The gateway only verifies tokens. The private signing keys belong to your issuer. The gateway reads the issuer's public keys from `MCPGW_JWKS_URL` and checks `MCPGW_JWT_ISSUER` and `MCPGW_JWT_AUDIENCE`.

What the verifier does (`internal/authn/verifier.go`):

- It accepts RSA keys of at least 2048 bits with a `kid`, and only RS256 tokens.
- A known key is served from the cache while it is younger than `MCPGW_JWKS_CACHE_TTL` (default 5m, `internal/config/config.go:42`).
- After that, the cached key is still served while one refresh runs in the background, until `MCPGW_JWKS_STALE_LIMIT` (default 1h, `internal/config/config.go:43`). This keeps tokens working while the issuer is down.
- An unknown `kid` triggers a synchronous fetch. Fetches are limited to one per 10 s, with a longer wait after failures (`internal/authn/verifier.go:164`, `:167`).
- A token may live at most 1 hour plus 30 s of clock skew (`internal/authn/verifier.go:30-31`).

Planned rotation:

1. Add the new public key, with a new `kid`, to the JWKS. Keep the old key there.
2. Start signing with the new key. The gateway fetches the JWKS when it first sees the new `kid`. No restart is needed.
3. Wait until the last token signed with the old key has expired: the maximum token lifetime plus the skew from the list above.
4. Remove the old key from the JWKS. A gateway drops it at its next successful refresh. That refresh starts when a request arrives with a known key older than `MCPGW_JWKS_CACHE_TTL`.

Emergency rotation (a key is compromised):

1. Remove the key from the JWKS and publish a new one.
2. Restart the gateway pods: `kubectl -n mcpgw rollout restart deployment/gw-mcp-gateway`. The key cache is in memory, so a restart drops the old key at once. Without a restart, a gateway keeps accepting the old key until its next successful refresh.
3. If the issuer is unreachable while you restart, a new pod has no keys and rejects every token until the issuer answers. Check the JWKS URL first.

A smaller `MCPGW_JWKS_STALE_LIMIT` shortens how long a removed key can live during an issuer outage. It also shortens how long tokens keep working during that outage. Pick based on which risk you accept.

Never enable the dev issuer in production (`MCPGW_DEV_ISSUER_ENABLED` defaults to false).

### Approval webhook HMAC secret

`MCPGW_APPROVAL_WEBHOOK_SECRET` signs every webhook body. The header is `X-MCPGW-Signature: sha256=<hex HMAC-SHA256>` (`internal/approvals/webhook.go:23`). The gateway refuses to start with a webhook URL and a secret shorter than 16 bytes (`internal/config/config.go:135`).

The gateway holds one secret at a time. There is no overlap mode, so the receiver must accept both secrets during the change:

1. Change the receiver to accept signatures made with the old or the new secret.
2. Update `MCPGW_APPROVAL_WEBHOOK_SECRET` in the Secret.
3. Restart the pods (`kubectl -n mcpgw rollout restart deployment/gw-mcp-gateway`). Environment variables are read at start.
4. Signatures are made when a delivery is attempted (`internal/approvals/webhook.go:57`), so rows waiting in the outbox are signed with the new secret once a new pod sends them.
5. When the rollout is finished and no old pod is left, remove the old secret from the receiver.

Failed deliveries are retried with a growing delay and abandoned after 24 attempts (`docs/DECISIONS.md`, 2026-10-16 row on the webhook outbox). A receiver that rejects signatures for a long time can therefore lose notifications. Approvals stay pending and visible through `GET /admin/approvals?status=pending`.

### Console sessions

The console keeps sessions in memory under random identifiers (`internal/console/console.go`); there is no session-signing key to rotate. To end every console session, restart the pods. Sessions are not shared between replicas and do not survive a restart (`docs/KNOWN_LIMITATIONS.md`, "Console sign-in is development-only"). Console sign-in is off in the Helm chart.

### Database role passwords

Three roles exist: `mcpgw_app` (the gateway), `mcpgw_readonly` and `mcpgw_upstream` (the example upstreams). `mcp-gateway migrate up` sets their passwords from `MCPGW_APP_DB_PASSWORD`, `MCPGW_READONLY_DB_PASSWORD` and `MCPGW_UPSTREAM_DB_PASSWORD` (`cmd/mcp-gateway/migrate.go:30-32`). It connects as the schema owner with `MCPGW_MIGRATE_DATABASE_URL`.

To rotate the gateway role:

1. Generate a new password.
2. In the Secret, set `MCPGW_APP_DB_PASSWORD` to it and put the same password into the `MCPGW_DATABASE_URL` value.
3. Run the migrate step. With the chart, `helm upgrade gw deploy/helm/mcp-gateway -n mcpgw -f my-values.yaml` runs the pre-upgrade Job. Without it, run `mcp-gateway migrate up` yourself with the same environment.
4. Restart the pods at once: `kubectl -n mcpgw rollout restart deployment/gw-mcp-gateway`.

Between steps 3 and 4 the running pods hold the old password. New database connections from them fail. Audit writes fail closed, so calls can answer `-32048` until the restart. Do this at a quiet time.

Then check `/readyz`, and run `kubectl -n mcpgw exec deploy/gw-mcp-gateway -- /mcp-gateway audit verify`, which exits 0 when the chain is intact.

For Docker Compose, set the variable in `.env` and run `docker compose run --rm migrate`, then `docker compose up -d gateway`. The schema owner password itself (`MCPGW_MIGRATE_DATABASE_URL`) is not managed by the gateway. Rotate it in Postgres and in the Secret.

Redis credentials live inside `MCPGW_REDIS_URL`. Change the Redis password, update the Secret and restart the pods.

## Audit archival

The audit log is the `audit_events` table (`migrations/00002_audit.sql`). Each row carries the previous row's hash and its own hash over the canonical event, so `mcp-gateway audit verify` can walk the chain. The table is append-only: triggers reject UPDATE, DELETE and TRUNCATE for every role, the table owner included (`migrations/00002_audit.sql:36-39`). The gateway role has SELECT and INSERT only.

The gateway ships no purge or archive command, and the retention loop (`MCPGW_RETENTION_INTERVAL`) never touches `audit_events`. It deletes old approvals, findings and delivered outbox rows. Archiving in this version means copying the data out and recording proof of its state.

Do this in order:

1. Verify the chain. Either of these works:

   ```bash
   kubectl -n mcpgw exec deploy/gw-mcp-gateway -- /mcp-gateway audit verify
   curl -s -H "Authorization: Bearer $ADMIN_TOKEN" http://127.0.0.1:8200/admin/audit/verify
   ```

   The command prints `OK: verified <n> events, head hash <hash>` and exits 0, or prints `FAIL: audit chain broken at seq <seq> (<reason>) after <n> valid events` and exits 1. Exit 2 means it could not run. The admin call returns the same facts as JSON (`ok`, `checked`, `first_bad_seq`, `reason`, `head_hash`), flushes buffered events first, and needs a cross-tenant viewer or `mcpgw:admin`. With Compose, `make audit-verify` runs the command.

   Do not archive a range that fails to verify. Go to "Tamper detection".

2. Export the range. Two options:
   - The admin API, `GET /admin/audit` with `limit` (up to 200, `api/openapi.yaml`) and `cursor`. The cursor is the `chain_pos` of the last event. The JSON uses the same timestamp layout as the hash, so a consumer can re-hash each event (`docs/DECISIONS.md`, 2026-10-16 rows on the cursor and on `ts`).
   - SQL as the read-only role: `\copy (SELECT * FROM audit_events WHERE chain_pos BETWEEN <a> AND <b> ORDER BY chain_pos) TO 'audit-<a>-<b>.csv' CSV HEADER`. Use it for reading and forensics. CSV and jsonb formatting differ from the hashed layout, so it is not a good source for re-hashing.

3. Store the export in write-once storage. Record its checksum (`sha256sum`).

4. Record the head hash as an external anchor, with the date and the number of events checked. This is the missing piece named in `docs/KNOWN_LIMITATIONS.md`: a superuser can remove the newest rows, and `audit verify` alone cannot see that, because the shorter chain is still consistent. Repeat step 1 and the anchor on a schedule, for example daily, from a job you operate. The repository does not ship that job.

5. Check an anchor later. The anchored hash must still be in the table, and the chain must still verify:

   ```sql
   SELECT chain_pos FROM audit_events WHERE hash = '<anchored head hash>';
   ```

   No row means the anchored events are gone or changed. Treat that as an incident.

Removing old rows is outside the product. The only way past the trigger is a superuser session with `session_replication_role = replica`, which is how `scripts/tamper_audit.sh` simulates tampering. If storage limits force you to do it:

- record the decision and keep the export and the anchor first;
- afterwards `mcp-gateway audit verify` from the start fails, because the first remaining row no longer follows the genesis hash;
- `mcp-gateway audit verify --from-seq <seq>` checks the rest of the chain from that event. It trusts that event's `prev_hash` (`internal/audit/event.go:199`), so compare it by hand with the last archived hash.

This path is not tested by the repository's own tests.

## Alerts

All six rules are in `deploy/prometheus/alerts.yaml` and are tested by `make promtool` (`deploy/prometheus/alerts_test.yaml`).

| Alert | Fires when | For | Severity |
|---|---|---|---|
| `AuditBufferHigh` | `mcpgw_audit_buffer_depth / mcpgw_audit_buffer_capacity > 0.8` | 1m | critical |
| `PolicyLoadFailure` | a reload failure in the last 5m, or `mcpgw_policy_stale == 1`, or `mcpgw_policy_loaded == 0` | none | critical |
| `UpstreamDown` | `mcpgw_upstream_up == 0` | 2m | warning |
| `ApprovalPendingTooLong` | `mcpgw_approvals_oldest_pending_seconds > 600` | none | warning |
| `GatewayOverheadSLO` | p95 of `mcpgw_overhead_seconds` above 0.005 s | 10m | warning |
| `AuditFlushSLO` | p99 of `mcpgw_audit_flush_seconds` above 0.5 s | 10m | warning |

### AuditBufferHigh

**What it means.** The audit buffer is more than 80% full on one gateway pod. Every call reserves an audit slot before it does anything. When the buffer is full, a call fails with `-32048` and nothing runs (`docs/security.md`, "Fail closed"). Those rejected calls are not audited themselves. They are counted in `mcpgw_audit_rejections_total`.

**First checks.**

- `mcpgw_audit_buffer_depth` against `mcpgw_audit_buffer_capacity` (`MCPGW_AUDIT_BUFFER`, default 10000), per instance.
- `rate(mcpgw_audit_rejections_total[5m])`: is the gateway already refusing calls?
- Is `AuditFlushSLO` also firing? That points at slow Postgres writes.
- `/readyz`: `postgres` unreachable, or `audit: sequence not allocated yet`.
- Postgres: connections, disk, locks, replication lag. All replicas append to one chain under one advisory lock (`internal/audit/postgres.go:122`).
- Pod logs for audit writer errors.
- A flood from one agent: `mcpgw_tool_calls_total` by `tenant` and `status`. Malformed calls also take audit slots (`docs/security.md`, "Limits and their edges").

**Mitigation.**

- Restore Postgres first. The buffer drains by itself once batches commit again.
- If the cause is load, lower the offending tenant's limits in `tenants` (values) and apply them. Do not just raise `MCPGW_AUDIT_BUFFER`: it only delays the failure when the database is the bottleneck.
- After recovery, run `mcp-gateway audit verify`.

### PolicyLoadFailure

**What it means.** A policy load or reload failed in the last 5 minutes, the gateway is still serving a stale bundle (`mcpgw_policy_stale == 1`), or no bundle is loaded (`mcpgw_policy_loaded == 0`). In the first two cases the previous bundle keeps serving. In the last case every call is denied and `/readyz` answers 503.

**First checks.**

- `curl -s http://127.0.0.1:8200/readyz`: the `policy` check holds the error text.
- Pod logs: `policy reload failed; keeping the previous bundle`.
- What changed: the image tag, or the ConfigMap named in `policies.existingConfigMap`.
- Run `mcp-gateway policy test <dir>` on the new bundle (see "Policy rollback").

**Mitigation.**

- Fix the bundle and let it reload, or force it with `POST /admin/policy/reload`.
- If you cannot fix it quickly, roll back as described in "Policy rollback".
- If `mcpgw_policy_loaded` is 0 on a new pod, roll back before anything else. That pod denies everything.

### UpstreamDown

**What it means.** An upstream (label `upstream`) failed its last catalog refresh or its circuit breaker is open (`internal/server/server.go:92-95`). Calls to its tools fail with `-32046` until it recovers. Other upstreams are not affected: each has its own breaker.

**First checks.**

- `GET /admin/upstreams` (needs `mcpgw:admin` or `mcpgw:audit:read`): `up`, `breaker` (`closed`, `open` or `half_open`), `last_error` and `last_refresh`.
- Is the upstream itself healthy? Check its pods and its `/mcp` URL from inside the cluster.
- DNS and network paths. The chart's NetworkPolicy limits ingress only, so egress problems come from elsewhere in the cluster.
- `MCPGW_UPSTREAM_TIMEOUT` (default 10s): is the upstream slow rather than down?

**Mitigation.**

- Fix or restart the upstream. The catalog refreshes every `MCPGW_CATALOG_REFRESH` (default 30s).
- The breaker opens after `MCPGW_BREAKER_THRESHOLD` consecutive failures (default 5). After `MCPGW_BREAKER_COOLDOWN` (default 30s) it lets one probe through and closes on success. The cooldown must be longer than the timeout; the gateway refuses to start otherwise (`internal/config/config.go:133`).
- Agents can retry writes with the same idempotency key. A retried write is not executed twice (`evals/reports/chaos.v1.json`, produced by `make chaos`).
- If the upstream is retired, remove it from the `upstreams` value and upgrade.

### ApprovalPendingTooLong

**What it means.** A held call has waited for a human decision for more than 10 minutes (`mcpgw_approvals_oldest_pending_seconds`, 0 when nothing is pending). Pending approvals expire after `MCPGW_APPROVAL_TTL` (default 15m). After that the agent's retry creates a new hold.

**First checks.**

- `GET /admin/approvals?status=pending`, or the console approvals page.
- Was the webhook delivered? Check the receiver, the gateway logs, and `MCPGW_APPROVAL_WEBHOOK_URL`. Failed deliveries are retried with a growing delay.
- Does an approver exist for that tenant? Approvers need `mcpgw:approve` and the approval's own tenant.
- Is the secret rotation in progress? A receiver that rejects the signature drops the notification.

**Mitigation.**

- Ask an approver to decide through the console or `POST /admin/approvals/{ref}`. A reason is required, and the decision is audited.
- Do not approve only to clear the alert. Read the redacted arguments and the reasons first.
- If the webhook is the problem, fix the receiver or the URL and secret. Pending approvals stay visible in the API either way.

### GatewayOverheadSLO

**What it means.** The 95th percentile of the gateway's own time per `tools/call`, upstream time excluded, has been above 5 ms for 10 minutes (`mcpgw_overhead_seconds`; the SLO is in `docs/specs/000-build-spec.md`, section 14).

**First checks.**

- CPU: pods with a CPU request of `250m` and no CPU limit (`values.yaml`, `resources`) share nodes. Look for node pressure and for the HPA at its maximum.
- Redis latency. Rate limiting and idempotency are in the call path.
- Policy evaluation time. The `latency.v1` gate is reported in `evals/REPORT.md` (`make eval`).
- Large upstream results: the injection scorer runs on each result. Check result sizes.
- Traces (`otel.endpoint`): look for the stage that grew, using the `mcpgw.*` spans.
- Under CPU saturation the metric includes time spent waiting for a CPU, so a load test on a small machine can raise this alert (`docs/KNOWN_LIMITATIONS.md`, "Throughput below NFR-2").

**Mitigation.**

- Add replicas or raise the CPU request and the HPA maximum.
- Fix the slow dependency (Redis first).
- For the performance method and results see `docs/perf/README.md`.

### AuditFlushSLO

**What it means.** The 99th percentile of the time to persist one audit batch (`mcpgw_audit_flush_seconds`) has been above 500 ms for 10 minutes. This usually comes before `AuditBufferHigh`.

**First checks.**

- Postgres write latency, CPU, disk and connection counts.
- Several replicas writing at once: they take turns on one advisory lock (`internal/audit/postgres.go:122`).
- `mcpgw_audit_buffer_depth`: is it growing?
- Batch settings: `MCPGW_AUDIT_BATCH` (default 500) and `MCPGW_AUDIT_FLUSH_INTERVAL` (default 50ms).

**Mitigation.**

- Fix the database. This is almost always a Postgres problem.
- Change the batch settings only after testing them. Check that `mcp-gateway audit verify` still passes afterwards.

## Recovering from a sequence reset

After a database restore, `audit_events_seq` can end up behind the highest stored `seq`. New events
then collide with stored ones, every call fails with `-32048`, and the logs repeat `audit flush failed;
will retry` with a unique violation on `audit_events_pkey`. This is deliberate: the sink only accepts a
colliding row when it is the very same event (a lost commit acknowledgement), never a different one.

1. Move the sequence past the stored rows, as the schema owner:

   ```sql
   SELECT setval('audit_events_seq', (SELECT max(seq) FROM audit_events));
   ```

2. Restart the gateway pods (`kubectl -n mcpgw rollout restart deploy/gw-mcp-gateway`). A pod keeps its
   already reserved seqs in memory, so `setval` alone does not unwedge it. Events still buffered in a pod
   when it restarts are lost: note the time window from the logs for the incident record.
3. Run `mcp-gateway audit verify` and confirm calls succeed again.

## Incident drills

Run these on the Docker Compose stack, not in production. Start it with `make up && make seed`. `make demo` runs the scripted scenario, which includes a tamper-and-restore step (`scripts/demo.sh`).

To make calls, mint a dev token and use the demo agent. These dev tools exist only for the local stack:

```bash
export MCPGW_DEV_TOKEN=$(make -s dev-token)
go run ./cmd/demo-agent --list
go run ./cmd/demo-agent --call crm.get_customer --args '{"customer_id":"<a seeded id>"}'
```

### Drill 1: tamper detection

Goal: show that an edited audit row is found, and which row.

1. Make sure there are audit rows (`make demo` makes some), then verify:

   ```bash
   make audit-verify
   ```

   Expected: `OK: verified <n> events, head hash <hash>`, exit 0.

2. Tamper with one row. The script edits the `agent` column of one row as the Postgres superuser, with `session_replication_role = replica` so the append-only trigger is skipped. It prints the `seq` it changed. By default it picks the middle row; pass a `seq` to choose:

   ```bash
   scripts/tamper_audit.sh
   ```

3. Verify again:

   ```bash
   make audit-verify
   ```

   Expected: `FAIL: audit chain broken at seq <seq> (hash does not match the event content) after <n> valid events`, exit 1, naming the `seq` the script printed. `GET /admin/audit/verify` reports the same in `first_bad_seq`.

4. Restore the row and verify again:

   ```bash
   scripts/tamper_audit.sh --restore <seq>
   make audit-verify
   ```

What this shows: an edit or a deleted row in the middle of the chain is detected (a deleted row shows as a gap in `chain_pos`). What it does not show: removal of the newest rows. That is why "Audit archival" asks for an external anchor of the head hash.

If verification fails in production:

1. Do not modify the table. Take a snapshot of the database.
2. Note the `seq` and the reason in the output.
3. Look at the row and its neighbours: `GET /admin/audit/{seq}`, or SQL as the read-only role. Compare each row's `prev_hash` with the hash of the row before it.
4. Compare with your latest export and anchor.
5. Find out who had a superuser session or the schema-owner login. Rotate the database credentials (see "Database role passwords").
6. Keep the evidence. The `audit-forensics` skill in `.claude/skills/audit-forensics/SKILL.md` has the same steps for developers.

### Drill 2: Redis outage

Goal: see what keeps working when Redis is gone.

1. Stop Redis: `docker compose stop redis`.
2. Make a read call and a write call with the demo agent. The write needs a fresh key:

   ```bash
   go run ./cmd/demo-agent --call crm.get_customer --args '{"customer_id":"<a seeded id>"}'
   go run ./cmd/demo-agent --call orders.refund_order --args '{"order_id":"<an order id>","amount_cents":100,"reason":"drill"}' --key drill-1
   ```

Expected from the code (`internal/pipeline/pipeline.go:358-375`):

- Reads (tools marked read-only) still run. The rate limit falls back to a per-process bucket, so limits are no longer shared between replicas. The gateway logs `rate limiter unavailable; reads use the local bucket`.
- Writes are refused with `-32049` (`dependency_unavailable`, with `data.dependency` set to `rate_limiter`) before any upstream call.
- The idempotency store also refuses writes when it is down (`internal/pipeline/idempotency.go:57`). With the DynamoDB store, writes still fail while Redis is down, because the rate limiter needs Redis.
- `/readyz` stays 200 and reports `"redis": "unreachable: writes refused, reads on the local rate limiter"` (`internal/server/server.go:220-224`), so the Service keeps routing reads to the pods (`TestRedisOutageKeepsReadsServed`).

Recovery: `docker compose start redis`. Calls resume without a gateway restart. If Redis comes back empty, idempotency records are gone. A retried write is then forwarded to the upstream, whose own dedup on `(tenant, idempotency_key)` still prevents a second write for upstreams built with `pkg/mcpserver` (`docs/KNOWN_LIMITATIONS.md`).

### Drill 3: an upstream is down

Goal: see the breaker open, the alert condition, and a safe retry.

1. Start Prometheus with `make observability` if you want the alert to fire (Prometheus is on `http://localhost:8250`).
2. Stop one upstream: `docker compose stop upstream-orders`.
3. Call one of its tools more than `MCPGW_BREAKER_THRESHOLD` times in a row (default 5):

   ```bash
   go run ./cmd/demo-agent --call orders.get_order --args '{"order_id":"<an order id>"}'
   ```

   Expected: each call fails with `-32046`. Once the breaker is open, calls fail fast without waiting for the timeout. Tools of the other upstreams keep working.

4. Look at the state:

   ```bash
   curl -s -H "Authorization: Bearer $ADMIN_TOKEN" http://localhost:8200/admin/upstreams
   curl -s http://localhost:8211/metrics | grep '^mcpgw_upstream_up'
   ```

   `breaker` is `open` and `mcpgw_upstream_up{upstream="orders"}` is 0. After 2 minutes `UpstreamDown` fires in Prometheus.

5. Start it again: `docker compose start upstream-orders`. After `MCPGW_BREAKER_COOLDOWN` (default 30s) one probe goes through and closes the breaker. The next catalog refresh (`MCPGW_CATALOG_REFRESH`) sets the metric back to 1.

6. For writes, retry with the same idempotency key. The chaos eval measures this under a stalled upstream: `make chaos` writes `evals/reports/chaos.v1.json` with the duplicate count (`docs/KNOWN_LIMITATIONS.md`, "Chaos eval parameters"). `make demo` step 4 shows the same case with one refund row.
