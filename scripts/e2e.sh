#!/bin/sh
# Runs the browser tests: the real program, driven through headless Chromium.
#
#   scripts/e2e.sh                       all of them
#   RP_E2E_SHOTS=.cache/shots scripts/e2e.sh    also save a screenshot of each step
#
# Needs only Docker. The image is Go plus Chromium and is built once.
set -e
repo=$(cd "$(dirname "$0")/.." && pwd)
docker build -q -t reflectingpool-e2e - >/dev/null <<'IMAGE'
FROM golang:1.27-alpine
RUN apk add --no-cache chromium
IMAGE
mkdir -p "$repo/.cache/go"
# RP_E2E_MOUNT adds a volume (name:path), for tests that need a large tree.
mount=""
[ -n "$RP_E2E_MOUNT" ] && mount="-v $RP_E2E_MOUNT:ro"
shots=""
if [ -n "$RP_E2E_SHOTS" ]; then
  mkdir -p "$repo/$RP_E2E_SHOTS"
  shots="/src/$RP_E2E_SHOTS"
fi
exec docker run --rm --user "$(id -u):$(id -g)" \
  -v "$repo":/src -w /src/e2e $mount \
  -e HOME=/src/.cache/go -e GOCACHE=/src/.cache/go/build-cgo0 -e GOMODCACHE=/src/.cache/go/mod \
  -e CGO_ENABLED=0 -e GOFLAGS=-buildvcs=false -e RP_E2E_SHOTS="$shots" -e RP_E2E_BIG \
  reflectingpool-e2e go test -count=1 "$@" ./...
