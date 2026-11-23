#!/usr/bin/env bash
# Installs the Cursor CLI (cursor-agent) from Cursor's download server, the
# same package https://cursor.com/install fetches. Cursor publishes no
# checksums for it, so the version is pinned and only HTTPS is used. Nothing
# from the package is run here.
#
#   install-cursor.sh [VERSION|latest|none] [install dir] [bin dir]
set -euo pipefail

version="${1:-latest}"
dir="${2:-/opt/cursor-agent}"
bindir="${3:-/usr/local/bin}"

mkdir -p "$dir"
if [ "$version" = none ]; then
  echo "Skipping the Cursor CLI"
  exit 0
fi

arch="${TARGETARCH:-$(uname -m)}"
case "$arch" in
  x86_64|amd64) arch=x64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo "unsupported architecture: $arch" >&2; exit 1 ;;
esac

if [ "$version" = latest ]; then
  # sed -n 1p reads all of its input, so nothing in the pipe gets SIGPIPE.
  version="$(curl -fsSL https://cursor.com/install \
    | sed -nE 's#.*downloads\.cursor\.com/lab/([0-9]{4}\.[0-9]{1,2}\.[0-9]{1,2}-[0-9a-f]+)/.*#\1#p' \
    | sed -n 1p)"
fi
if [[ ! "$version" =~ ^[0-9]{4}\.[0-9]{1,2}\.[0-9]{1,2}-[0-9a-f]+$ ]]; then
  echo "could not resolve a Cursor CLI version (got '$version')" >&2
  exit 1
fi

url="https://downloads.cursor.com/lab/${version}/linux/${arch}/agent-cli-package.tar.gz"
echo "Installing the Cursor CLI $version (linux/$arch)"
curl -fsSL "$url" | tar --strip-components=1 -xzf - -C "$dir"
if [ ! -f "$dir/cursor-agent" ]; then
  echo "the package has no cursor-agent executable" >&2
  exit 1
fi
mkdir -p "$bindir"
ln -sf "$dir/cursor-agent" "$bindir/cursor-agent"
echo "$version" > "$dir/.version"
