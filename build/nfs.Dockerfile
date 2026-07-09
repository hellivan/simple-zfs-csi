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
    -o /out/nfs-controller ./cmd/nfs-controller

# ---- runtime ----
# Debian base provides the kernel NFS server userspace (rpcbind, rpc.mountd,
# rpc.nfsd, exportfs). The nfsd/sunrpc kernel modules must be present on the host
# (Talos ZFS/NFS system extensions).
FROM debian:trixie-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends nfs-kernel-server nfs-common rpcbind \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /var/lib/nfs/rpc_pipefs /proc/fs/nfsd
COPY --from=build /out/nfs-controller /usr/local/bin/nfs-controller
ENTRYPOINT ["/usr/local/bin/nfs-controller"]
