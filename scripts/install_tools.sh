#!/usr/bin/env bash
# Installs pinned developer tools into ./bin. Idempotent: a tool whose binary
# already reports the pinned version is skipped. Versions come from the Makefile.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/bin"
mkdir -p "$BIN"
export GOBIN="$BIN"
# Build every Go tool with the module toolchain so linters understand its language version.
export GOTOOLCHAIN="$(awk '/^toolchain/{print $2}' "$ROOT/go.mod")"

: "${GOLANGCI_LINT_VERSION:?run through make bootstrap}"

stamp() { echo "$BIN/.$1.$2"; }

go_tool() { # name version module
  local name=$1 version=$2 module=$3
  if [[ -x "$BIN/$name" && -f "$(stamp "$name" "$version")" ]]; then return; fi
  echo "installing $name $version"
  go install "$module@$version"
  rm -f "$BIN/.$name."* && touch "$(stamp "$name" "$version")"
}

go_tool golangci-lint "$GOLANGCI_LINT_VERSION" github.com/golangci/golangci-lint/v2/cmd/golangci-lint
go_tool goimports "$GOIMPORTS_VERSION" golang.org/x/tools/cmd/goimports
go_tool gosec "$GOSEC_VERSION" github.com/securego/gosec/v2/cmd/gosec
go_tool govulncheck "$GOVULNCHECK_VERSION" golang.org/x/vuln/cmd/govulncheck
go_tool actionlint "$ACTIONLINT_VERSION" github.com/rhysd/actionlint/cmd/actionlint
go_tool gitleaks "$GITLEAKS_VERSION" github.com/zricethezav/gitleaks/v8
go_tool kubeconform "$KUBECONFORM_VERSION" github.com/yannh/kubeconform/cmd/kubeconform
go_tool go-licenses "$GO_LICENSES_VERSION" github.com/google/go-licenses/v2
go_tool helm "$HELM_VERSION" helm.sh/helm/v3/cmd/helm

os=$(uname -s); arch=$(uname -m)

# promtool: GitHub release tarball; the Go module cannot be `go install`ed
# (replace directives), so the fallback wraps the official Prometheus image.
if [[ ! -f "$(stamp promtool "$PROMTOOL_VERSION")" ]]; then
  v=${PROMTOOL_VERSION#v}
  pos=$(echo "$os" | tr '[:upper:]' '[:lower:]'); parch=amd64; [[ "$arch" == "arm64" || "$arch" == "aarch64" ]] && parch=arm64
  tmp=$(mktemp -d)
  if curl -fsSL -o "$tmp/p.tgz" "https://github.com/prometheus/prometheus/releases/download/${PROMTOOL_VERSION}/prometheus-${v}.${pos}-${parch}.tar.gz"; then
    tar -xzf "$tmp/p.tgz" -C "$tmp" && mv "$tmp/prometheus-${v}.${pos}-${parch}/promtool" "$BIN/promtool"
  else
    echo "promtool release download failed; using the prom/prometheus:${PROMTOOL_VERSION} image"
    cat > "$BIN/promtool" <<WRAP
#!/usr/bin/env bash
# Wrapper: runs promtool ${PROMTOOL_VERSION} from the official Prometheus image.
exec docker run --rm -v "\$PWD:/work:ro" -w /work --entrypoint promtool prom/prometheus:${PROMTOOL_VERSION} "\$@"
WRAP
    chmod +x "$BIN/promtool"
  fi
  rm -rf "$tmp"
  rm -f "$BIN/.promtool."* && touch "$(stamp promtool "$PROMTOOL_VERSION")"
fi

# hadolint: GitHub release binary; falls back to a wrapper around the official
# image when GitHub release downloads are blocked (Claude Code on the web).
if [[ ! -f "$(stamp hadolint "$HADOLINT_VERSION")" ]]; then
  asset="hadolint-${os}-${arch/aarch64/arm64}"
  [[ "$os" == "Darwin" ]] && asset="hadolint-macos-${arch/aarch64/arm64}"
  if curl -fsSL -o "$BIN/hadolint" "https://github.com/hadolint/hadolint/releases/download/${HADOLINT_VERSION}/${asset}"; then
    chmod +x "$BIN/hadolint"
  else
    echo "hadolint release download failed; using the hadolint/hadolint:${HADOLINT_VERSION} image"
    cat > "$BIN/hadolint" <<WRAP
#!/usr/bin/env bash
# Wrapper: runs hadolint ${HADOLINT_VERSION} from its official image.
args=(); for a in "\$@"; do if [[ -f "\$a" ]]; then args+=("/work/\$a"); else args+=("\$a"); fi; done
exec docker run --rm -v "\$PWD:/work:ro" -w /work hadolint/hadolint:${HADOLINT_VERSION} hadolint "\${args[@]}"
WRAP
    chmod +x "$BIN/hadolint"
  fi
  rm -f "$BIN/.hadolint."* && touch "$(stamp hadolint "$HADOLINT_VERSION")"
fi

# trivy: GitHub release tarball; falls back to `go install` of the same tag.
if [[ ! -f "$(stamp trivy "$TRIVY_VERSION")" ]]; then
  v=${TRIVY_VERSION#v}
  tos=Linux; [[ "$os" == "Darwin" ]] && tos=macOS
  tarch=64bit; [[ "$arch" == "arm64" || "$arch" == "aarch64" ]] && tarch=ARM64
  tmp=$(mktemp -d)
  if curl -fsSL -o "$tmp/trivy.tgz" "https://github.com/aquasecurity/trivy/releases/download/${TRIVY_VERSION}/trivy_${v}_${tos}-${tarch}.tar.gz"; then
    tar -xzf "$tmp/trivy.tgz" -C "$tmp" trivy && mv "$tmp/trivy" "$BIN/trivy"
  else
    echo "trivy release download failed; building ${TRIVY_VERSION} with go install"
    go install "github.com/aquasecurity/trivy/cmd/trivy@${TRIVY_VERSION}"
  fi
  rm -rf "$tmp"
  rm -f "$BIN/.trivy."* && touch "$(stamp trivy "$TRIVY_VERSION")"
fi

echo "tools ready in $BIN"
