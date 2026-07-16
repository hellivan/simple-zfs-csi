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
    -o /out/operator ./cmd/operator

# ---- runtime ----
# The operator only talks to the Kubernetes API (watching Nodes, patching
# ZfsPool status, rendering NetworkExports), so a static distroless image suffices.
FROM gcr.io/distroless/static:latest
COPY --from=build /out/operator /usr/local/bin/operator
ENTRYPOINT ["/usr/local/bin/operator"]
