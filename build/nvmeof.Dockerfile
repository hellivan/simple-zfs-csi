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
    -o /out/nvmeof-controller ./cmd/nvmeof-controller

# ---- runtime ----
# The NVMe-oF controller only touches configfs and device nodes, so a static
# distroless image suffices. The nvmet and nvmet-tcp kernel modules must be
# loaded on the host (Talos system extensions), and configfs mounted.
FROM gcr.io/distroless/static:latest
COPY --from=build /out/nvmeof-controller /usr/local/bin/nvmeof-controller
ENTRYPOINT ["/usr/local/bin/nvmeof-controller"]
