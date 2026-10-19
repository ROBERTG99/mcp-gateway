#!/usr/bin/env bash
# make image-size: size of a built image, compressed (the layers as a registry stores them and
# `docker pull` transfers them; `docker save | gzip`) and unpacked (the container filesystem;
# `docker export`). Fails when the compressed size reaches the budget in MB (spec T-41: < 40 MB).
# The two numbers are measured the same way on the classic and the containerd image store.
set -euo pipefail
image=${1:-mcp-gateway:dev}
budget_mb=${2:-40}

compressed=$(docker save "$image" | gzip -c | wc -c)
cid=$(docker create "$image")
trap 'docker rm -f "$cid" >/dev/null' EXIT
unpacked=$(docker export "$cid" | wc -c)

mb() { awk -v b="$1" 'BEGIN { printf "%.1f", b / 1000000 }'; }
echo "$image: compressed $(mb "$compressed") MB (budget < $budget_mb MB), unpacked $(mb "$unpacked") MB"
if [ "$compressed" -ge $((budget_mb * 1000000)) ]; then
  echo "FAIL: compressed image size over budget"
  exit 1
fi
