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
    -o /out/csi-node ./cmd/csi-node

# ---- runtime ----
# The node plugin performs real mounts, so unlike the controller it needs the
# userspace tooling: NFS client (mount.nfs), NVMe-oF client (nvme-cli), block
# utilities (blkid, mount/umount) and mkfs helpers. These are used when the
# plugin runs the in-image tools directly; with --host-exec-mode it instead
# invokes the host's binaries via chroot/nsenter.
FROM debian:stable-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        nfs-common \
        nvme-cli \
        util-linux \
        e2fsprogs \
        xfsprogs \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/csi-node /usr/local/bin/csi-node
ENTRYPOINT ["/usr/local/bin/csi-node"]
