#!/usr/bin/env bash
# release.sh <version> — cross-compile release binaries into dist/ with a
# checksums file. Version is embedded via -ldflags -X main.version. Called by
# `task release:binaries`.
set -euo pipefail

cd "$(dirname "$0")/.."

VERSION="${1:?usage: release.sh <version>}"
OUT=dist
PLATFORMS="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64"

rm -rf "$OUT"
mkdir -p "$OUT"

for p in $PLATFORMS; do
  os="${p%/*}"
  arch="${p#*/}"
  bin="target_${VERSION}_${os}_${arch}"
  echo "==> building $bin"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
    go build -ldflags="-s -w -X main.version=${VERSION}" -o "$OUT/$bin" .
done

# Checksums (sha256sum on Linux, shasum on macOS).
echo "==> checksums"
if command -v sha256sum >/dev/null; then
  ( cd "$OUT" && sha256sum target_* > SHA256SUMS )
else
  ( cd "$OUT" && shasum -a 256 target_* > SHA256SUMS )
fi

echo "==> artifacts in $OUT/"
ls -1 "$OUT"