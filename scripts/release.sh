#!/usr/bin/env bash
# Builds dist/mojols_<os>_<arch>.tar.gz, the asset names the Zed extension downloads.
set -euo pipefail
cd "$(dirname "$0")/.."
VERSION="${VERSION:-dev}"
rm -rf dist && mkdir -p dist
for target in darwin/arm64 darwin/amd64 linux/arm64 linux/amd64 windows/amd64; do
  os="${target%/*}"; arch="${target#*/}"
  exe=mojols; [ "$os" = windows ] && exe=mojols.exe
  stage="$(mktemp -d)"
  (cd server && CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath \
      -ldflags "-s -w -X main.version=$VERSION" -o "$stage/$exe" ./cmd/mojols)
  cp LICENSE "$stage/"
  tar -C "$stage" -czf "dist/mojols_${os}_${arch}.tar.gz" "$exe" LICENSE
  rm -rf "$stage"
  echo "built dist/mojols_${os}_${arch}.tar.gz"
done
(cd dist && shasum -a 256 *.tar.gz > checksums.txt 2>/dev/null || sha256sum *.tar.gz > checksums.txt)
