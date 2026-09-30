#!/bin/sh
# Stretch one-line installer.
#
#   curl -fsSL https://your-release-host/stretch/install.sh | sh
#
# Detects OS/arch, downloads the matching prebuilt tarball, verifies its
# SHA-256 checksum, and installs the four binaries to a user-local bin dir.
#
# Env knobs:
#   STRETCH_VERSION       version to install (default: 0.1.0)
#   STRETCH_RELEASE_BASE  base URL hosting the release tarballs + SHA256SUMS.txt
#                         (required unless --from-source)
#   STRETCH_BINDIR        install dir override (default: ~/.local/bin, or $PREFIX/bin on Termux)
#
# Local build instead of download (run inside the repo checkout):
#   ./install.sh --from-source
#
set -eu

VERSION="${STRETCH_VERSION:-0.1.0}"
BASE="${STRETCH_RELEASE_BASE:-https://github.com/jaykk99/stretch/releases/download/v0.1.0}"
BINDIR="${STRETCH_BINDIR:-}"
FROM_SOURCE=0
[ "${1:-}" = "--from-source" ] && FROM_SOURCE=1

# ---- detect platform ----
OS="$(uname -s)"
ARCH="$(uname -m)"
TERMUX=0
case "${PREFIX:-}" in *termux*) TERMUX=1 ;; esac
case "${TERMUX_VERSION:-}" in ?*) TERMUX=1 ;; esac

if [ "$TERMUX" = 1 ]; then
  OS=android
elif [ "$OS" = "Darwin" ]; then
  OS=darwin
elif [ "$OS" = "Linux" ]; then
  OS=linux
else
  case "$OS" in MINGW*|MSYS*|CYGWIN*) OS=windows ;; *)
    echo "error: unsupported OS: $OS" >&2; exit 1 ;;
  esac
fi

case "$ARCH" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "error: unsupported arch: $ARCH" >&2; exit 1 ;;
esac

EXT=""; [ "$OS" = "windows" ] && EXT=".exe"
NAME="stretch-$VERSION-$OS-$ARCH"

if [ -z "$BINDIR" ]; then
  if [ "$TERMUX" = 1 ]; then BINDIR="$PREFIX/bin"; else BINDIR="$HOME/.local/bin"; fi
fi
mkdir -p "$BINDIR"

fetch() { # $1=url $2=dest
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$1" -o "$2"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO "$2" "$1"
  else
    echo "error: need curl or wget" >&2; exit 1
  fi
}

sha256_of() { # $1=file
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1
  elif command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | cut -d' ' -f1
  else echo "error: need sha256sum or shasum" >&2; exit 1; fi
}

if [ "$FROM_SOURCE" = 1 ]; then
  if ! command -v go >/dev/null 2>&1; then
    echo "error: --from-source needs the Go toolchain on PATH" >&2; exit 1
  fi
  echo "building from source into $BINDIR ..."
  for bin in stretchstore stretchmem stretchcpu stretchnet; do
    (CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$BINDIR/$bin$EXT" "./cmd/$bin")
  done
else
  [ -n "$BASE" ] || { echo "error: set STRETCH_RELEASE_BASE to the URL hosting the release tarballs" >&2
    echo "   or run with --from-source inside a repo checkout." >&2; exit 1; }
  TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
  echo "downloading $NAME.tar.gz ..."
  fetch "$BASE/$NAME.tar.gz" "$TMP/pkg.tar.gz"
  fetch "$BASE/SHA256SUMS.txt" "$TMP/SHA256SUMS.txt"
  want="$(grep " $NAME.tar.gz\$" "$TMP/SHA256SUMS.txt" | cut -d' ' -f1)"
  [ -n "$want" ] || { echo "error: no checksum entry for $NAME.tar.gz" >&2; exit 1; }
  got="$(sha256_of "$TMP/pkg.tar.gz")"
  [ "$want" = "$got" ] || { echo "error: SHA-256 mismatch! refusing to install." >&2
    echo "  expected: $want"; echo "  got:      $got"; exit 1; }
  echo "checksum OK"
  tar -xzf "$TMP/pkg.tar.gz" -C "$TMP"
  for bin in stretchstore stretchmem stretchcpu stretchnet; do
    cp "$TMP/$NAME/$bin$EXT" "$BINDIR/$bin$EXT"
    chmod +x "$BINDIR/$bin$EXT"
  done
fi

echo ""
echo "installed to $BINDIR:"
for bin in stretchstore stretchmem stretchcpu stretchnet; do
  echo "  $bin$EXT"
done

# ---- per-OS notes ----
echo ""
echo "notes for $OS/$ARCH:"
case "$OS" in
  linux)
    if [ -e /dev/fuse ]; then
      echo "  - FUSE mount available: stretchstore mount --dir ./vol --mp /mnt/stretch"
    else
      echo "  - no /dev/fuse here; use the fallback: stretchstore serve --dir ./vol"
      echo "    (on Debian/Ubuntu with FUSE: sudo apt install fuse3, then mount works)"
    fi ;;
  darwin)
    echo "  - FUSE mount needs macFUSE (https://osxfuse.github.io); without it use:"
    echo "    stretchstore serve --dir ./vol" ;;
  windows)
    echo "  - Windows builds have no FUSE backend; use the fallback:"
    echo "    stretchstore serve --dir ./vol   (virtual disk.img over HTTP/WebDAV on localhost)" ;;
  android)
    echo "  - Termux: no FUSE without root; use the fallback:"
    echo "    stretchstore serve --dir ./vol   (then open http://127.0.0.1:8080 on the device)" ;;
esac
echo "  - stretchmem, stretchcpu, stretchnet work identically on every OS (no FUSE involved)."
echo "  - self-tests: stretchstore test | stretchmem test | stretchcpu test | stretchnet test"

case ":$PATH:" in *":$BINDIR:"*) ;; *)
  echo ""; echo "tip: add $BINDIR to your PATH to run the binaries from anywhere." ;; esac
