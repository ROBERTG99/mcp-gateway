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

# Test image: full source and toolchain, used by compose.test.yaml (make test-docker).
FROM golang:1.27 AS test
ARG HTTPS_PROXY
ENV GOTOOLCHAIN=local
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build ./... && go test -count=1 -run '^$' ./... > /dev/null
CMD ["go", "test", "-race", "-count=1", "./..."]

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
