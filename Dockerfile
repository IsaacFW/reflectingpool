# The runtime image's Alpine release decides which ZFS tools it carries, and
# they must be the same release series as the host's ZFS kernel module.
# Alpine 3.24 ships ZFS 2.4.x, matching Unraid 7.3. For an older host, build
# with an older Alpine, e.g. --build-arg ALPINE_VERSION=3.22 for ZFS 2.3.x.
ARG ALPINE_VERSION=3.24

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/reflectingpool ./cmd/reflectingpool

FROM alpine:${ALPINE_VERSION}
# zfs: the `zfs list` command, used read-only when /dev/zfs is passed in.
RUN apk add --no-cache zfs ca-certificates tzdata
COPY --from=build /out/reflectingpool /usr/local/bin/reflectingpool
ENV RP_DATA=/data
VOLUME /data
EXPOSE 8443
# Runs as root so that every file on the pool can be read whatever its owner.
# Do not add --privileged: without it ZFS refuses destructive commands.
ENTRYPOINT ["/usr/local/bin/reflectingpool"]
CMD ["serve"]
