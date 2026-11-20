#!/bin/sh
# Regression: checksum lookup must match the literal release filename.
set -eu
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
NAME='stretch-0.2.0-linux-amd64'
HASH='0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef'
printf '%s  %s\n' "$HASH" 'stretch-0X2X0-linux-amd64.tar.gz' > "$TMP/SHA256SUMS.txt"
printf '%s  %s\n' "$HASH" "$NAME.tar.gz" >> "$TMP/SHA256SUMS.txt"
lookup() {
  awk -v file="$NAME.tar.gz" '$2 == file && length($1) == 64 && $1 ~ /^[0-9a-f]+$/ { print $1 }' "$TMP/SHA256SUMS.txt"
}
[ "$(lookup)" = "$HASH" ] || { echo 'exact filename lookup failed' >&2; exit 1; }
printf '%s  %s\n' "$HASH" "$NAME.tar.gz" >> "$TMP/SHA256SUMS.txt"
[ "$(lookup | wc -l | tr -d ' ')" = 2 ] || { echo 'duplicate entries not detected' >&2; exit 1; }
printf 'checksum filename regression passed\n'
