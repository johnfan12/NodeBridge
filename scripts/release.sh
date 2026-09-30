#!/usr/bin/env bash
set -euo pipefail
cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.."
GO=${GO:-go}
VERSION=${VERSION:-dev}
[[ $VERSION =~ ^[A-Za-z0-9._-]+$ ]] || { echo 'Invalid VERSION' >&2; exit 1; }
mkdir -p dist
cp scripts/install.sh scripts/uninstall.sh dist/
chmod 755 dist/install.sh dist/uninstall.sh
tmp=$(mktemp -d)
trap 'rm -rf -- "$tmp"' EXIT
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch "$GO" build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$tmp/nodebridge" ./cmd/nodebridge
  cp scripts/install.sh scripts/uninstall.sh "$tmp/"
  chmod 755 "$tmp/nodebridge" "$tmp/install.sh" "$tmp/uninstall.sh"
  tar -czf "dist/nodebridge-linux-$arch.tar.gz" -C "$tmp" nodebridge install.sh uninstall.sh
done
(cd dist && sha256sum nodebridge-linux-amd64.tar.gz nodebridge-linux-arm64.tar.gz > SHA256SUMS)
