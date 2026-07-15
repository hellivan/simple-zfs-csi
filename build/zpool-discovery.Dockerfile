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
# The Tier 1 discovery agent shells out to `zpool`/`zfs`, so the image ships the
# ZFS userspace tools. The zfs kernel module must be loaded on the host (Talos
# ZFS system extension) and /dev/zfs mounted into the pod. The zfsutils-linux
# version should track the host's ZFS kernel module version to avoid ioctl
# incompatibilities.
FROM debian:trixie-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends zfsutils-linux \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/zpool-discovery /usr/local/bin/zpool-discovery
ENTRYPOINT ["/usr/local/bin/zpool-discovery"]
