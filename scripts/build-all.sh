#!/usr/bin/env bash
# Build every cmd/* binary into bin/ with consistent ldflags.
#
# Used by `make build-all`. Picks up VERSION from the environment when
# set, otherwise derives one from `git describe`. Stops on the first
# build failure.

set -euo pipefail

VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
LDFLAGS="-X main.version=${VERSION}"
BIN_DIR="bin"

mkdir -p "${BIN_DIR}"

BINARIES=(
  "rikugan:cmd/rikugan"
  "rikugan-orchestration:cmd/orchestration-mcp"
  "rikugan-logs:cmd/logs-mcp"
  "rikugan-metrics:cmd/metrics-mcp"
  "rikugan-alerts:cmd/alerts-mcp"
)

for entry in "${BINARIES[@]}"; do
  name="${entry%%:*}"
  pkg="${entry##*:}"
  out="${BIN_DIR}/${name}"
  echo ">> building ${name} (${pkg}) -> ${out}"
  go build -ldflags "${LDFLAGS}" -o "${out}" "./${pkg}"
done

echo
echo "All binaries built into ${BIN_DIR}/:"
ls -lh "${BIN_DIR}"
