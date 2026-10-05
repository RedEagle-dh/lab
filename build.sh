#!/bin/sh
# Cross-compiles lab via Docker (no local Go toolchain needed).
set -e
cd "$(dirname "$0")"
docker run --rm -v "$PWD":/src -w /src -u "$(id -u):$(id -g)" -e HOME=/tmp -e CGO_ENABLED=0 golang:1 sh -c '
  go vet ./... &&
  for a in amd64 arm64; do GOOS=linux GOARCH=$a go build -trimpath -ldflags="-s -w" -o dist/lab-linux-$a .; done'
ls -l dist
