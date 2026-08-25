#!/usr/bin/env bash
# Entrypoint for the oease GitHub Actions runner image.
#
#  1. optionally installs extra apt packages (ADDITIONAL_PACKAGES)
#  2. obtains a registration token from the KMS, retrying while it starts up
#  3. registers the runner and starts the listener (RunnerService.js keeps it
#     alive across runner self-updates)
#  4. on SIGTERM/SIGINT waits for an in-flight job to finish, stops the
#     listener and deregisters the runner so it does not linger as "offline"
#
# Required:  KMS_SERVER_ADDR, RUNNER_REGISTER_TO
# Optional:  KMS_AUTH_TOKEN, RUNNER_NAME, RUNNER_NAME_PREFIX, RUNNER_LABELS, RUNNER_GROUP,
#            RUNNER_WORKDIR, RUNNER_EPHEMERAL, RUNNER_DISABLE_UPDATE,
#            RUNNER_GRACEFUL_STOP_TIMEOUT, ADDITIONAL_PACKAGES, ADDITIONAL_FLAGS,
#            GITHUB_URL, KMS_RETRY_INTERVAL, KMS_MAX_ATTEMPTS
set -euo pipefail

RUNNER_HOME="${RUNNER_HOME:-/actions-runner}"
GITHUB_URL="${GITHUB_URL:-https://github.com}"
RUNNER_WORKDIR="${RUNNER_WORKDIR:-_work}"
KMS_RETRY_INTERVAL="${KMS_RETRY_INTERVAL:-5}"
KMS_MAX_ATTEMPTS="${KMS_MAX_ATTEMPTS:-60}"
RUNNER_GRACEFUL_STOP_TIMEOUT="${RUNNER_GRACEFUL_STOP_TIMEOUT:-900}"

log() { printf '[entrypoint] %s\n' "$*"; }
die() { printf '[entrypoint] ERROR: %s\n' "$*" >&2; exit 1; }

[ -n "${KMS_SERVER_ADDR:-}" ] || die "KMS_SERVER_ADDR is required, e.g. http://kms:3000"
[ -n "${RUNNER_REGISTER_TO:-}" ] || die "RUNNER_REGISTER_TO is required: an organization (oeasenet) or owner/repo (oeasenet/platform)"

cd "$RUNNER_HOME"

# --- optional extra packages -------------------------------------------------
if [ -n "${ADDITIONAL_PACKAGES:-}" ]; then
    packages=$(printf '%s' "$ADDITIONAL_PACKAGES" | tr ',' ' ')
    log "Installing additional packages: $packages"
    apt-get update
    # shellcheck disable=SC2086  # the package list is intentionally word-split
    apt-get install -y --no-install-recommends $packages
    apt-get clean
    [ -d /var/lib/apt/lists ] && rm -rf /var/lib/apt/lists/*
fi

# --- KMS token retrieval -----------------------------------------------------
KMS_SERVER_ADDR="${KMS_SERVER_ADDR%/}"
GITHUB_URL="${GITHUB_URL%/}"
case "$RUNNER_REGISTER_TO" in
    */*) KMS_TOKEN_BASE="$KMS_SERVER_ADDR/repo/$RUNNER_REGISTER_TO" ;;
    *)   KMS_TOKEN_BASE="$KMS_SERVER_ADDR/$RUNNER_REGISTER_TO" ;;
esac
RUNNER_URL="$GITHUB_URL/$RUNNER_REGISTER_TO"
# Default name: <RUNNER_NAME_PREFIX>-<hostname>; the container id keeps replicas unique.
RUNNER_NAME="${RUNNER_NAME:-${RUNNER_NAME_PREFIX:+${RUNNER_NAME_PREFIX}-}$(hostname)}"

# kms_token <registration-token|remove-token>
# Prints a token issued by the KMS. Retries while the KMS is unreachable, since
# it normally starts alongside the runners.
kms_token() {
    local kind="$1" attempt=1 token
    local -a curl_auth=()
    if [ -n "${KMS_AUTH_TOKEN:-}" ]; then
        curl_auth=(-H "Authorization: Bearer $KMS_AUTH_TOKEN")
    fi
    while :; do
        if token=$(curl -fsS --max-time 15 ${curl_auth[@]+"${curl_auth[@]}"} "$KMS_TOKEN_BASE/$kind") && [ -n "$token" ]; then
            printf '%s' "$token"
            return 0
        fi
        if [ "$attempt" -ge "$KMS_MAX_ATTEMPTS" ]; then
            log "Could not obtain a $kind from $KMS_TOKEN_BASE after $attempt attempts" >&2
            return 1
        fi
        log "KMS unavailable ($KMS_TOKEN_BASE/$kind); retrying in ${KMS_RETRY_INTERVAL}s (attempt $attempt/$KMS_MAX_ATTEMPTS)" >&2
        attempt=$((attempt + 1))
        sleep "$KMS_RETRY_INTERVAL"
    done
}

# --- registration ------------------------------------------------------------
REGISTRATION_TOKEN=$(kms_token registration-token) || die "Giving up: the KMS at $KMS_SERVER_ADDR did not issue a registration token"

config_args=(--unattended --replace --url "$RUNNER_URL" --token "$REGISTRATION_TOKEN" --name "$RUNNER_NAME" --work "$RUNNER_WORKDIR")
if [ -n "${RUNNER_LABELS:-}" ]; then config_args+=(--labels "$RUNNER_LABELS"); fi
if [ -n "${RUNNER_GROUP:-}" ]; then config_args+=(--runnergroup "$RUNNER_GROUP"); fi
if [ "${RUNNER_EPHEMERAL:-false}" = "true" ]; then config_args+=(--ephemeral); fi
if [ "${RUNNER_DISABLE_UPDATE:-false}" = "true" ]; then config_args+=(--disableupdate); fi
if [ -n "${ADDITIONAL_FLAGS:-}" ]; then
    # shellcheck disable=SC2206  # extra flags are intentionally word-split
    config_args+=(${ADDITIONAL_FLAGS})
fi

log "Registering runner '$RUNNER_NAME' with $RUNNER_URL"
./config.sh "${config_args[@]}"

# config.sh snapshots PATH into .path; use it so jobs see the same environment.
if [ -f .path ]; then
    PATH=$(cat .path)
    export PATH
fi

# The runner bundles its own node runtimes; pick the newest one.
NODE_BIN=$(for n in externals/node*/bin/node; do v=${n#externals/node}; echo "${v%%/*} $n"; done | sort -n | tail -n1 | cut -d' ' -f2)
[ -n "$NODE_BIN" ] && [ -x "$NODE_BIN" ] || die "No bundled node runtime found under $RUNNER_HOME/externals"

# --- graceful stop -----------------------------------------------------------
LISTENER_PID=""
STOP_REQUESTED=0

# shellcheck disable=SC2329  # invoked through the trap below
graceful_stop() {
    STOP_REQUESTED=1
    log "Stop requested"
    local waited=0
    while pgrep -f Runner.Worker >/dev/null 2>&1; do
        if [ "$waited" -ge "$RUNNER_GRACEFUL_STOP_TIMEOUT" ]; then
            log "A job is still running after ${RUNNER_GRACEFUL_STOP_TIMEOUT}s; stopping anyway"
            break
        fi
        if [ $((waited % 30)) -eq 0 ]; then
            log "Waiting for the running job to finish (${waited}s elapsed, limit ${RUNNER_GRACEFUL_STOP_TIMEOUT}s)"
        fi
        sleep 1
        waited=$((waited + 1))
    done
    if [ -n "$LISTENER_PID" ] && kill -0 "$LISTENER_PID" 2>/dev/null; then
        # RunnerService.js treats SIGTERM like SIGINT (stop the listener, SIGKILL
        # after 30s). TERM is used because processes started with & from a
        # non-interactive shell ignore SIGINT.
        log "Stopping runner listener"
        kill -TERM "$LISTENER_PID" 2>/dev/null || true
    fi
}
trap graceful_stop INT TERM

deregister() {
    local token
    log "Deregistering runner '$RUNNER_NAME'"
    if token=$(KMS_MAX_ATTEMPTS=3 kms_token remove-token); then
        ./config.sh remove --token "$token" || log "WARNING: deregistration failed; GitHub removes stale offline runners automatically"
    else
        log "WARNING: could not obtain a remove token; GitHub removes stale offline runners automatically"
    fi
}

# --- run ---------------------------------------------------------------------
log "Starting runner listener"
"$NODE_BIN" ./bin/RunnerService.js &
LISTENER_PID=$!

set +e
wait "$LISTENER_PID"
LISTENER_STATUS=$?
# wait returns early (>128) when a trapped signal arrives; keep waiting until
# the listener has actually exited.
while kill -0 "$LISTENER_PID" 2>/dev/null; do
    wait "$LISTENER_PID"
    LISTENER_STATUS=$?
done
set -e
trap - INT TERM

if [ "${RUNNER_EPHEMERAL:-false}" = "true" ] && [ "$STOP_REQUESTED" -eq 0 ] && [ "$LISTENER_STATUS" -eq 0 ]; then
    log "Ephemeral runner finished its job; GitHub has already removed its registration"
else
    deregister
fi

if [ "$STOP_REQUESTED" -eq 1 ]; then
    log "Runner stopped"
    exit 0
fi
log "Runner listener exited with status $LISTENER_STATUS"
exit "$LISTENER_STATUS"
