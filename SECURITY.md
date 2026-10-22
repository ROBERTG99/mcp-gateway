# Security policy

mcp-gateway sits between LLM agents and upstream MCP servers and decides which tool calls may run. We take reports about it seriously.

## Supported versions

| Version | Supported |
|---|---|
| 0.1.x | Yes |
| Anything older, or a pre-release build | No |

Only the latest 0.1.x release gets fixes.

## Report a vulnerability

Please report privately. Do not open a public issue, pull request or discussion for a vulnerability.

Use GitHub security advisories:

1. Open the [repository security page](https://github.com/ROBERTG99/mcp-gateway/security).
2. Choose "Report a vulnerability" and fill in the form.

If that button is not shown, open a public issue titled "Security contact request" with no details about the problem. We will then arrange a private channel.

Please include:

- the version or commit you tested;
- the configuration that matters (policy bundle, idempotency store, Helm values);
- the steps to reproduce it, ideally a failing test or a short request sequence;
- what an attacker gains: for example reading another tenant's data, running a write that policy or approval should have stopped, or changing the audit log without detection.

Do not include real secrets, real tokens or real personal data. Use synthetic data.

We will acknowledge the report, work on a fix with you, and agree on a disclosure date before anything is made public.

## Scope

In scope, for a supported version:

- authentication and tenant isolation (`internal/authn`, the tenant taken from the token);
- the policy engine and the bundle shipped in `policies/`, including the default-deny rule;
- approvals, idempotency, rate limits and budgets;
- redaction of personal data in the audit log, webhooks and logs;
- the audit log and its hash chain (`mcp-gateway audit verify`);
- the admin API and the console, including scope checks, CSRF protection and output escaping;
- upstream output marking and limits (body, result and depth limits);
- the public helpers in `pkg/mcpserver`;
- the Helm chart, the `Dockerfile` and the container image.

## Out of scope

- **The dev issuer and the console persona sign-in.** They are for development only. The dev issuer (`mcp-gateway dev-issuer`) signs tokens for anyone who asks. Persona sign-in is mounted only when `MCPGW_DEV_ISSUER_URL` is set, and then anyone who can reach port 8200 can sign in as any persona. Do not run either in production. The Helm chart leaves them off. See `docs/KNOWN_LIMITATIONS.md`, "Console sign-in is development-only".
- **The local Docker Compose stack.** Its passwords and secrets are synthetic development values, and all ports are bound to 127.0.0.1.
- **The example upstreams, the demo agent and the load generator.** They exist to demonstrate and test the gateway. A flaw in `pkg/mcpserver` is in scope. A flaw only in an example upstream's business logic is not.
- **Missed prompt-injection detections.** Injection scoring is defence in depth. Marking of upstream output, policy and approvals are the controls. A way around marking, policy or approvals is in scope. Report a missed phrase as an ordinary issue. The measured limits are in `docs/KNOWN_LIMITATIONS.md` and `evals/REPORT.md`.
- **Limitations already documented** in `docs/KNOWN_LIMITATIONS.md`, for example that a database superuser who removes the newest audit rows is not detected without an external anchor of the head hash (`docs/runbook.md`, "Audit archival").
- Attacks that need a database superuser, cluster administrator or a stolen signing key, apart from the point above.
- Vulnerabilities in third-party dependencies with no effect on this project. Report those to the dependency. `make security` runs the scans we use (`docs/security.md`).

## More information

The threat model, the controls and the accepted scan findings are in [`docs/security.md`](docs/security.md). Operational guidance, including key rotation and incident drills, is in [`docs/runbook.md`](docs/runbook.md).
