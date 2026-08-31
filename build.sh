#!/usr/bin/env bash
set -euo pipefail

VERSION="${1:-0.1.0}"
OUT=dist
mkdir -p "$OUT"

# Go on PATH? fall back to the local SDK used during development.
if ! command -v go >/dev/null 2>&1; then
  export PATH="$HOME/go-sdk/bin:$PATH"
fi

build() {
  local goos="$1" goarch="$2" ext="${3:-}"
  local name="daiyaku-${goos}-${goarch}${ext}"
  echo "  building $name"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$OUT/$name" ./cmd/daiyaku
}

echo "daiyaku $VERSION → $OUT/"
build windows amd64 .exe
build windows arm64 .exe
build linux   amd64
build linux   arm64
build darwin  amd64
build darwin  arm64

echo "done:"
ls -la "$OUT"
