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
    -o /out/zpool-watcher ./cmd/zpool-watcher

# ---- runtime ----
# The Tier 2 watcher only talks to the Kubernetes API (watching Nodes, patching
# ZfsPool status), so a static distroless image suffices.
FROM gcr.io/distroless/static:latest
COPY --from=build /out/zpool-watcher /usr/local/bin/zpool-watcher
ENTRYPOINT ["/usr/local/bin/zpool-watcher"]
