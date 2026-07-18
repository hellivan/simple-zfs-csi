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
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" \
    -o /out/zpool-scrub ./cmd/zpool-scrub

# ---- runtime ----
# The Tier 1 discovery agent runs the HOST's own version-matched zpool/zfs via
# host-exec (chart discovery.hostExec): `chroot /host` (busybox, the default
# mode) or `nsenter` (util-linux-misc). The container ships NO ZFS tools of its
# own — the host provides them (e.g. the Talos siderolabs/zfs extension). The
# controller binary is fully static (CGO disabled), so Alpine/musl runs it
# unchanged. The image also carries zpool-scrub, the one-shot scrub launched by
# the operator-reconciled maintenance CronJob (ADR-0012).
FROM alpine:3.21
RUN apk add --no-cache util-linux-misc
COPY --from=build /out/zpool-discovery /usr/local/bin/zpool-discovery
COPY --from=build /out/zpool-scrub /usr/local/bin/zpool-scrub
ENTRYPOINT ["/usr/local/bin/zpool-discovery"]
