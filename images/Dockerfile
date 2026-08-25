# syntax=docker/dockerfile:1

# The shared runner build: any apt-based base, same tooling everywhere. Flavors
# are the directories under images/ with a flavor.json ("base" becomes
# BASE_IMAGE, the directory name becomes FLAVOR); a flavor that needs something
# else ships its own Dockerfile next to its flavor.json (build context: images/).
# Bump GITHUB_RUNNER_VERSION (or pass --build-arg) to move to a new runner
# release; CI resolves the latest release automatically.
ARG BASE_IMAGE=ubuntu:26.04
ARG GITHUB_RUNNER_VERSION=2.336.0

# ---- fetch the runner for the target architecture ---------------------------
# Runs natively on the build host; it only downloads and unpacks a tarball.
FROM --platform=$BUILDPLATFORM ${BASE_IMAGE} AS runner-download

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
    && curl -fsSL --retry 3 -o runner.tar.gz \
        "https://github.com/actions/runner/releases/download/v${GITHUB_RUNNER_VERSION}/actions-runner-linux-${RUNNER_ARCH}-${GITHUB_RUNNER_VERSION}.tar.gz" \
    && tar -xzf runner.tar.gz \
    && rm runner.tar.gz

# ---- runtime image ----------------------------------------------------------
FROM ${BASE_IMAGE}

ARG GITHUB_RUNNER_VERSION
ARG FLAVOR=ubuntu
ARG DEBIAN_FRONTEND=noninteractive

COPY apt-install /usr/local/sbin/apt-install
RUN chmod 0755 /usr/local/sbin/apt-install

# General-purpose build tooling for workflows (the same list in every flavor).
RUN apt-install \
        ca-certificates curl wget gnupg lsb-release tzdata locales \
        sudo jq git git-lfs openssh-client rsync \
        zip unzip tar gzip xz-utils \
        iputils-ping dnsutils netcat-openbsd procps psmisc \
        build-essential pkg-config cmake clang make gettext perl \
        libssl-dev libcurl4-gnutls-dev zlib1g-dev \
        python3 python3-pip python3-venv

# add-apt-repository for workflows that add PPAs; Ubuntu has it, Debian 13 does not.
RUN apt-get update \
    && { apt-get install -y --no-install-recommends software-properties-common \
         || echo "software-properties-common is not available on this base; skipping" >&2; } \
    && rm -rf /var/lib/apt/lists/*

# Docker CLI + buildx + compose plugins (no daemon) from Docker's repository
# for this distribution (ubuntu or debian; minideb is Debian underneath).
# Mount the host's /var/run/docker.sock into the container to let jobs build.
RUN . /etc/os-release \
    && install -m 0755 -d /etc/apt/keyrings \
    && curl -fsSL "https://download.docker.com/linux/${ID}/gpg" -o /etc/apt/keyrings/docker.asc \
    && chmod a+r /etc/apt/keyrings/docker.asc \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/${ID} ${VERSION_CODENAME} stable" \
        > /etc/apt/sources.list.d/docker.list \
    && apt-install docker-ce-cli docker-buildx-plugin docker-compose-plugin

ENV RUNNER_HOME=/actions-runner \
    RUNNER_ALLOW_RUNASROOT=1 \
    RUNNER_FLAVOR=${FLAVOR} \
    GITHUB_RUNNER_VERSION=${GITHUB_RUNNER_VERSION}

WORKDIR /actions-runner
COPY --from=runner-download /actions-runner/ ./
# The runner's own dependency installer uses apt too; give it the same patience.
RUN attempt=1; until ./bin/installdependencies.sh; do \
        [ "$attempt" -lt 3 ] || exit 1; attempt=$((attempt + 1)); sleep 20; \
    done \
    && rm -rf /var/lib/apt/lists/*

COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod 0755 /usr/local/bin/entrypoint.sh

# Healthy once the listener process is up (registration can take a while on
# first start, especially with ADDITIONAL_PACKAGES).
HEALTHCHECK --interval=30s --timeout=10s --start-period=120s --retries=3 \
    CMD pgrep -f Runner.Listener >/dev/null || exit 1

# Metadata last, so changing it does not rebuild the layers above. The controller
# reads corral.flavor and corral.runner-version from the containers.
LABEL org.opencontainers.image.source="https://github.com/oeasenet/corral" \
      org.opencontainers.image.description="corral runner image, ${FLAVOR} flavor: GitHub Actions self-hosted runner with Docker CLI and build tooling" \
      org.opencontainers.image.licenses="Apache-2.0" \
      corral.flavor="${FLAVOR}" \
      corral.runner-version="${GITHUB_RUNNER_VERSION}"

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
