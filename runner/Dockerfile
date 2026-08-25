# syntax=docker/dockerfile:1

# Bump this (or pass --build-arg) to move the fleet to a new runner release.
# CI resolves the latest release automatically on scheduled builds.
ARG GITHUB_RUNNER_VERSION=2.336.0

# ---- fetch the runner for the target architecture ---------------------------
# Runs natively on the build host; it only downloads and unpacks a tarball.
FROM --platform=$BUILDPLATFORM ubuntu:26.04 AS runner-download

ARG GITHUB_RUNNER_VERSION
ARG TARGETARCH

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /actions-runner
RUN case "${TARGETARCH}" in \
        amd64) RUNNER_ARCH=x64 ;; \
        arm64) RUNNER_ARCH=arm64 ;; \
        *) echo "unsupported TARGETARCH: ${TARGETARCH}" >&2; exit 1 ;; \
    esac \
    && curl -fsSL -o runner.tar.gz \
        "https://github.com/actions/runner/releases/download/v${GITHUB_RUNNER_VERSION}/actions-runner-linux-${RUNNER_ARCH}-${GITHUB_RUNNER_VERSION}.tar.gz" \
    && tar -xzf runner.tar.gz \
    && rm runner.tar.gz

# ---- runtime image ----------------------------------------------------------
FROM ubuntu:26.04

ARG GITHUB_RUNNER_VERSION
ARG DEBIAN_FRONTEND=noninteractive

LABEL org.opencontainers.image.source="https://github.com/oeasenet/gha-docker-runner" \
      org.opencontainers.image.description="oease GitHub Actions self-hosted runner (registers via the oease KMS)" \
      org.opencontainers.image.licenses="Apache-2.0"

# General-purpose build tooling for workflows.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates curl wget gnupg lsb-release tzdata locales \
        sudo jq git git-lfs openssh-client rsync \
        zip unzip tar gzip xz-utils \
        iputils-ping dnsutils netcat-openbsd procps psmisc \
        build-essential pkg-config cmake clang make gettext perl \
        libssl-dev libcurl4-gnutls-dev zlib1g-dev \
        python3 python3-pip python3-venv \
        software-properties-common \
    && rm -rf /var/lib/apt/lists/*

# Docker CLI + buildx + compose plugins (no daemon). Mount the host's
# /var/run/docker.sock into the container to let jobs build and run images.
RUN install -m 0755 -d /etc/apt/keyrings \
    && curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc \
    && chmod a+r /etc/apt/keyrings/docker.asc \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo "${VERSION_CODENAME}") stable" \
        > /etc/apt/sources.list.d/docker.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends docker-ce-cli docker-buildx-plugin docker-compose-plugin \
    && rm -rf /var/lib/apt/lists/*

ENV RUNNER_HOME=/actions-runner \
    RUNNER_ALLOW_RUNASROOT=1 \
    GITHUB_RUNNER_VERSION=${GITHUB_RUNNER_VERSION}

WORKDIR /actions-runner
COPY --from=runner-download /actions-runner/ ./
RUN ./bin/installdependencies.sh \
    && rm -rf /var/lib/apt/lists/*

COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod 0755 /usr/local/bin/entrypoint.sh

# Healthy once the listener process is up (registration can take a while on
# first start, especially with ADDITIONAL_PACKAGES).
HEALTHCHECK --interval=30s --timeout=10s --start-period=120s --retries=3 \
    CMD pgrep -f Runner.Listener >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
