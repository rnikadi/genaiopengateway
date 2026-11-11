#!/bin/sh
# Installs the latest GenAIOpenGateway release binary (docs/quickstart.md):
#
#   curl -fsSL https://raw.githubusercontent.com/rnikadi/genaiopengateway/main/scripts/install.sh | sh
#
# Environment: VERSION (such as 0.8.0; default the latest release) and
# BIN_DIR (default /usr/local/bin, or ~/.local/bin when that is not
# writable). The archive is checked against the release's checksums.txt;
# verify that file's cosign signature too for a production install
# (docs/releasing.md).
set -eu
repo=rnikadi/genaiopengateway

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in linux|darwin) ;; *) echo "unsupported OS $os; download a release from https://github.com/$repo/releases" >&2; exit 1 ;; esac
arch=$(uname -m)
case "$arch" in x86_64|amd64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) echo "unsupported architecture $arch" >&2; exit 1 ;; esac

version=${VERSION:-}
if [ -z "$version" ]; then
  version=$(curl -fsSL "https://api.github.com/repos/$repo/releases/latest" | sed -n 's/.*"tag_name": *"v\([^"]*\)".*/\1/p' | head -n1)
  [ -n "$version" ] || { echo "could not find the latest release" >&2; exit 1; }
fi
version=${version#v}

name=genaiopengateway_${version}_${os}_${arch}
url=https://github.com/$repo/releases/download/v$version
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
echo "Downloading GenAIOpenGateway $version for $os/$arch"
curl -fsSL -o "$tmp/$name.tar.gz" "$url/$name.tar.gz"
curl -fsSL -o "$tmp/checksums.txt" "$url/checksums.txt"
(cd "$tmp" && grep " $name.tar.gz\$" checksums.txt > want.txt && { sha256sum -c want.txt 2>/dev/null || shasum -a 256 -c want.txt; } >/dev/null) \
  || { echo "checksum mismatch for $name.tar.gz" >&2; exit 1; }
tar -xzf "$tmp/$name.tar.gz" -C "$tmp"

dir=${BIN_DIR:-/usr/local/bin}
if [ -z "${BIN_DIR:-}" ] && [ ! -w "$dir" ]; then
  dir=$HOME/.local/bin
  mkdir -p "$dir"
fi
install -m 0755 "$tmp/$name/genaiopengateway" "$dir/genaiopengateway"
echo "Installed $dir/genaiopengateway"
if [ -f "$tmp/$name/gogctl" ]; then
  install -m 0755 "$tmp/$name/gogctl" "$dir/gogctl"
  echo "Installed $dir/gogctl"
fi
case ":$PATH:" in *":$dir:"*) ;; *) echo "Add $dir to your PATH." ;; esac
echo "Run it with no arguments to start with zero config, then open http://localhost:8080/ui/"
