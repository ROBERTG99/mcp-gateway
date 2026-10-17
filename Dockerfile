# syntax=docker/dockerfile:1
# Multi-stage build for mcp-gateway. Targets: gateway (default runtime), examples, test.

FROM golang:1.27 AS build
ARG HTTPS_PROXY
ARG VERSION=dev
ARG COMMIT=unknown
ENV CGO_ENABLED=0 GOFLAGS=-trimpath GOTOOLCHAIN=local
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -ldflags "-s -w -X github.com/ROBERTG99/mcp-gateway/internal/version.version=${VERSION} -X github.com/ROBERTG99/mcp-gateway/internal/version.commit=${COMMIT}" \
      -o /out/ ./cmd/...

# Test image: full source, the Go toolchain (with gcc for -race), helm (tests/helm) and the Python
# test environment (uv.lock, system Python 3.13), used by compose.test.yaml (make test-docker).
# Test binaries are compiled at build time so the containers only run them.
FROM golang:1.27 AS test
ARG HTTPS_PROXY
ENV GOTOOLCHAIN=local UV_PYTHON_DOWNLOADS=never UV_PROJECT_ENVIRONMENT=/opt/venv
COPY --from=astral/uv:0.11.32 /uv /usr/local/bin/uv
COPY --from=alpine/helm:3.22.1 /usr/bin/helm /usr/local/bin/helm
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY pyproject.toml uv.lock ./
RUN uv sync --frozen --no-install-project
COPY . .
RUN go test -race -count=1 -tags=integration -run '^$' ./... > /dev/null
CMD ["go", "test", "-race", "-count=1", "-tags=integration", "./..."]

# Example upstreams, demo agent and generators.
FROM gcr.io/distroless/static-debian12:nonroot AS examples
COPY --from=build /out/ /usr/local/bin/
COPY migrations /migrations
COPY config /config
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/upstream-echo"]

# Gateway runtime image.
FROM gcr.io/distroless/static-debian12:nonroot AS gateway
COPY --from=build /out/mcp-gateway /mcp-gateway
COPY policies /policies
COPY config /config
COPY migrations /migrations
ENV MCPGW_POLICY_DIR=/policies \
    MCPGW_UPSTREAMS_FILE=/config/upstreams.yaml \
    MCPGW_TENANTS_FILE=/config/tenants.yaml
USER nonroot:nonroot
EXPOSE 8200 8210 8211
HEALTHCHECK --interval=5s --timeout=3s --start-period=5s --retries=10 CMD ["/mcp-gateway", "healthcheck", "http://127.0.0.1:8200/readyz"]
ENTRYPOINT ["/mcp-gateway"]
CMD ["serve"]
