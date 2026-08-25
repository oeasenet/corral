# syntax=docker/dockerfile:1

# ---- build stage -----------------------------------------------------------
# Runs on the build host's native platform and cross-compiles for the target,
# so multi-arch builds never need QEMU for the Go compiler.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS builder

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

WORKDIR /src

# Build context is the repository root (the flavor catalog is copied below).
COPY controller/go.mod controller/go.sum* ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY controller/ .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/controller .

# ---- runtime stage ---------------------------------------------------------
FROM alpine:3.24

LABEL org.opencontainers.image.source="https://github.com/oeasenet/corral" \
      org.opencontainers.image.description="corral: runs and manages GitHub Actions self-hosted runner containers through the Docker socket" \
      org.opencontainers.image.licenses="Apache-2.0"

RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /out/controller /usr/local/bin/controller
# The runner flavor catalog (images/<name>/flavor.json) feeds the dashboard's runtime list.
COPY images/ /etc/corral/images/

# Runs as root: it needs the host's Docker socket, which is root-equivalent anyway.
WORKDIR /data
ENV PORT=8080 \
    DATA_DIR=/data \
    FLAVORS_DIR=/etc/corral/images

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- "http://127.0.0.1:${PORT}/health" >/dev/null || exit 1

ENTRYPOINT ["controller"]
