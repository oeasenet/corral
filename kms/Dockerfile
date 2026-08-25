# syntax=docker/dockerfile:1

# ---- build stage -----------------------------------------------------------
# Runs on the build host's native platform and cross-compiles for the target,
# so multi-arch builds never need QEMU for the Go compiler.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS builder

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

WORKDIR /src

COPY go.mod go.sum* ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/kms .

# ---- runtime stage ---------------------------------------------------------
FROM alpine:3.24

LABEL org.opencontainers.image.source="https://github.com/oeasenet/gha-docker-runner" \
      org.opencontainers.image.description="oease GitHub Runner KMS: exchanges GitHub PATs for short-lived runner registration tokens" \
      org.opencontainers.image.licenses="Apache-2.0"

RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S -g 10001 kms \
    && adduser -S -u 10001 -G kms -h /app kms

WORKDIR /app
COPY --from=builder /out/kms /usr/local/bin/kms

# Numeric UID so runAsNonRoot-style policies can verify it.
USER 10001:10001

ENV PORT=3000 \
    CONFIG_FILE=/app/config.json

EXPOSE 3000

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD wget -qO- "http://127.0.0.1:${PORT}/health" >/dev/null || exit 1

ENTRYPOINT ["kms"]
