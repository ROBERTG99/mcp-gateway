# 0006. Streamable HTTP only, no stdio

- Status: Accepted (2026-10-17)

## Context

MCP defines two standard transports: stdio and Streamable HTTP. With stdio, a client starts the server as a child process and talks over pipes. The gateway has to serve many agents and tenants at once, check a bearer token on every request, run as several replicas behind a load balancer, and call upstream servers that run as separate services.

## Decision

The gateway speaks Streamable HTTP on both sides and has no stdio code path.

- Agent side: `internal/proxy/proxy.go` serves `POST`, `GET` and `DELETE /mcp` with the SDK's `NewStreamableHTTPHandler`. The address is `MCPGW_MCP_ADDR` (`internal/config/config.go`). Every request passes the JWT middleware first (`internal/server/server.go`, `MCPHandler`).
- Upstream side: `internal/catalog/catalog.go` connects to each upstream with `mcp.StreamableClientTransport` and the URL from `config/upstreams.yaml`. The example upstreams serve the same transport (`internal/upstreams/upstreams.go`).
- Sessions of the legacy session protocol are bound to the authenticated principal, so a session id cannot be reused by another principal (`TestSessionBoundToPrincipal` in `internal/proxy/session_test.go`).
- Stdio is out of scope for v0.1.0 (`docs/specs/000-build-spec.md`, section 3.3). No non-test Go file under `cmd/` or `internal/` refers to a stdio transport.

## Alternatives considered

- **Support stdio as well.** Rejected: a stdio server has no request headers, so there is no place for a bearer token, and each client would need its own child process. That does not fit tenant isolation, horizontal scaling or per-request audit. Launching arbitrary commands from configuration would also widen the attack surface of a security component.
- **Stdio for upstreams only.** Rejected: the gateway would have to spawn and supervise processes, and the circuit breaker, timeouts and health checks assume a network call. Upstreams stay separate services (`internal/catalog/catalog.go`).
- **A custom JSON-RPC over HTTP protocol.** Rejected: agents and SDKs already speak Streamable HTTP, and a private protocol would need its own clients.

## Consequences

- An upstream that only supports stdio needs an adapter that exposes it over Streamable HTTP. The gateway does not provide one.
- Standard load balancers, TLS terminators and HTTP tracing work unchanged. W3C trace context travels in headers and in `_meta` (`docs/DECISIONS.md`).
- The 2026-07-28 stateless protocol revision has no `Mcp-Session-Id`, so the session-to-principal binding applies only to clients that negotiate the legacy session protocol (`docs/KNOWN_LIMITATIONS.md`). Every request is authenticated either way.
- Local use needs a running gateway process. `make up` starts the stack, and `mcp-gateway dev-issuer` supplies development tokens.
