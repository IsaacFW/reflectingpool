#!/bin/sh
# Runs the Go toolchain in a container, for machines with Docker but no Go.
#
#   scripts/go.sh go test ./...
#   scripts/go.sh go vet ./...
#   RACE=1 scripts/go.sh go test -race ./...   (the race detector needs cgo)
#
# Build and module caches are kept in .cache/ so repeated runs are fast.
set -e
repo=$(cd "$(dirname "$0")/.." && pwd)
mkdir -p "$repo/.cache/go"
image=golang:1.27-alpine
cgo=0
if [ -n "$RACE" ]; then
  image=golang:1.27
  cgo=1
fi
exec docker run --rm --user "$(id -u):$(id -g)" \
  -v "$repo":/src -w /src \
  -e HOME=/src/.cache/go -e GOCACHE=/src/.cache/go/build-cgo$cgo -e GOMODCACHE=/src/.cache/go/mod \
  -e CGO_ENABLED=$cgo -e GOFLAGS=-buildvcs=false \
  "$image" "$@"
