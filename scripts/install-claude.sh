#!/usr/bin/env bash
# Installs the native Claude Code binary (no Node.js needed) and verifies its
# SHA-256 checksum against the official release manifest.
#
#   install-claude.sh [stable|latest|X.Y.Z] [destination]
set -euo pipefail

target="${1:-stable}"
dest="${2:-/usr/local/bin/claude}"
base="https://downloads.claude.ai/claude-code-releases"

arch="${TARGETARCH:-$(uname -m)}"
case "$arch" in
  x86_64|amd64) arch=x64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo "unsupported architecture: $arch" >&2; exit 1 ;;
esac
platform="linux-${arch}"
if [ -f /lib/libc.musl-x86_64.so.1 ] || [ -f /lib/libc.musl-aarch64.so.1 ]; then
  platform="${platform}-musl"
fi

case "$target" in
  stable|latest) version="$(curl -fsSL "$base/$target")" ;;
  *) version="$target" ;;
esac
if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+ ]]; then
  echo "could not resolve a Claude Code version (got '$version')" >&2
  exit 1
fi

manifest="$(curl -fsSL "$base/$version/manifest.json")"
checksum="$(jq -r --arg p "$platform" '.platforms[$p].checksum // empty' <<<"$manifest")"
if [[ ! "$checksum" =~ ^[a-f0-9]{64}$ ]]; then
  echo "no checksum for $platform in the $version manifest" >&2
  exit 1
fi

echo "Installing Claude Code $version ($platform)"
curl -fsSL -o "$dest" "$base/$version/$platform/claude"
echo "$checksum  $dest" | sha256sum -c -
chmod 0755 "$dest"
echo "$version" > /claude-version
