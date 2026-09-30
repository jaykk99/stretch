#!/bin/sh
# Build the full release matrix: static binaries for 6 targets, tarballs,
# and a SHA-256 checksum file. Output lands in ./dist/.
#
# Usage: ./scripts/make-release.sh [version]
# Requires: Go toolchain on PATH.
set -eu

VERSION="${1:-0.1.0}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DIST="$ROOT/dist"

if ! command -v go >/dev/null 2>&1; then
  echo "error: go toolchain not found on PATH" >&2
  exit 1
fi

rm -rf "$DIST"
mkdir -p "$DIST/stage"
export CGO_ENABLED=0

echo "building stretch $VERSION ..."
for target in "linux amd64" "linux arm64" "darwin amd64" "darwin arm64" "windows amd64" "android arm64"; do
  set -- $target
  os="$1"; arch="$2"
  ext=""; [ "$os" = "windows" ] && ext=".exe"
  name="stretch-$VERSION-$os-$arch"
  stage="$DIST/stage/$name"
  mkdir -p "$stage"
  for bin in stretchstore stretchmem stretchcpu stretchnet; do
    (cd "$ROOT" && GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "-s -w" \
      -o "$stage/$bin$ext" "./cmd/$bin")
  done
  cp "$ROOT/README.md" "$stage/README.md"
  (cd "$DIST/stage" && tar -czf "$DIST/$name.tar.gz" "$name")
  echo "  $name.tar.gz"
done

(cd "$DIST" && sha256sum stretch-*.tar.gz > SHA256SUMS.txt)
rm -rf "$DIST/stage"
echo ""
echo "release artifacts in $DIST:"
ls -la "$DIST"
