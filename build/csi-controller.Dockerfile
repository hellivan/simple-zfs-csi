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
    -o /out/csi-controller ./cmd/csi-controller

# ---- runtime ----
# The CSI controller only talks to the Kubernetes API (writing ZfsDataset /
# ZfsShare, reading PVCs) and serves gRPC over a unix socket, so a static
# distroless image suffices.
FROM gcr.io/distroless/static:latest
COPY --from=build /out/csi-controller /usr/local/bin/csi-controller
ENTRYPOINT ["/usr/local/bin/csi-controller"]
