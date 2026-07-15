# syntax=docker/dockerfile:1

# ---- build ----
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" \
    -o /out/zpool-discovery ./cmd/zpool-discovery

# ---- runtime ----
# The Tier 1 discovery agent shells out to `zpool`/`zfs`. By default (chart
# discovery.hostExec) it runs the HOST's version-matched binaries via
# `chroot /host` or `nsenter` (both from the base image's coreutils/util-linux),
# so the bundled zfsutils-linux below is only used as a fallback when hostExec is
# disabled. When relying on the bundled tools, keep their version close to the
# host ZFS kernel module (Talos siderolabs/zfs extension) to avoid ioctl
# incompatibilities, and mount /dev/zfs into the pod.
FROM debian:trixie-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends zfsutils-linux \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/zpool-discovery /usr/local/bin/zpool-discovery
ENTRYPOINT ["/usr/local/bin/zpool-discovery"]
