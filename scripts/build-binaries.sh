#!/bin/sh
# Builds release archives for every supported platform into dist/, with a
# SHA-256 checksum file.
set -eu
version=${1:?usage: build-binaries.sh VERSION}
rm -rf dist && mkdir -p dist
for platform in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do
  os=${platform%/*} arch=${platform#*/}
  name=genaiopengateway_${version}_${os}_${arch}
  ext=""; [ "$os" = windows ] && ext=.exe
  mkdir -p "dist/$name"
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
    -ldflags "-s -w -X main.version=$version" -o "dist/$name/genaiopengateway$ext" ./cmd/gateway
  cp LICENSE README.md CHANGELOG.md config.example.yaml "dist/$name/"
  if [ "$os" = windows ]; then
    (cd dist && zip -qr "$name.zip" "$name")
  else
    tar -C dist -czf "dist/$name.tar.gz" "$name"
  fi
  rm -rf "dist/$name"
done
(cd dist && sha256sum -- *.tar.gz *.zip > checksums.txt)
