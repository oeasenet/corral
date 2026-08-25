#!/usr/bin/env bash
# Entrypoint for the oease GitHub Actions runner image.
#
# The controller creates each runner container with a fresh registration
# token in RUNNER_TOKEN. On restarts the runner is already configured
# (.runner exists) and registration is skipped, so no new token is needed.
#
#  1. optionally installs extra apt packages (ADDITIONAL_PACKAGES)
#  2. registers the runner (config.sh) unless it is already configured
#  3. starts the listener (RunnerService.js keeps it alive across self-updates)
#  4. on SIGTERM/SIGINT waits for an in-flight job to finish, then stops the
#     listener; the controller deletes the GitHub registration afterwards
#
# Required:  RUNNER_REGISTER_TO, RUNNER_TOKEN (unless already configured)
# Optional:  RUNNER_NAME, RUNNER_NAME_PREFIX, RUNNER_LABELS, RUNNER_GROUP,
#            RUNNER_WORKDIR, RUNNER_EPHEMERAL, RUNNER_DISABLE_UPDATE,
#            RUNNER_GRACEFUL_STOP_TIMEOUT, ADDITIONAL_PACKAGES, ADDITIONAL_FLAGS,
#            GITHUB_URL
set -euo pipefail

RUNNER_HOME="${RUNNER_HOME:-/actions-runner}"
GITHUB_URL="${GITHUB_URL:-https://github.com}"
RUNNER_WORKDIR="${RUNNER_WORKDIR:-_work}"
RUNNER_GRACEFUL_STOP_TIMEOUT="${RUNNER_GRACEFUL_STOP_TIMEOUT:-900}"

log() { printf '[entrypoint] %s\n' "$*"; }
die() { printf '[entrypoint] ERROR: %s\n' "$*" >&2; exit 1; }

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
fi

# --- registration ------------------------------------------------------------
if [ -f .runner ]; then
    log "Runner is already configured; skipping registration"
else
    [ -n "${RUNNER_TOKEN:-}" ] || die "RUNNER_TOKEN is required to register (the controller provides it; for manual runs mint one with the GitHub API)"
    RUNNER_URL="${GITHUB_URL%/}/$RUNNER_REGISTER_TO"
    # Default name: <RUNNER_NAME_PREFIX>-<hostname>; the container id keeps replicas unique.
    RUNNER_NAME="${RUNNER_NAME:-${RUNNER_NAME_PREFIX:+${RUNNER_NAME_PREFIX}-}$(hostname)}"

    config_args=(--unattended --replace --url "$RUNNER_URL" --token "$RUNNER_TOKEN" --name "$RUNNER_NAME" --work "$RUNNER_WORKDIR")
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
fi

# The registration token is single-purpose and short-lived; keep it away from jobs.
unset RUNNER_TOKEN

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

if [ "$STOP_REQUESTED" -eq 1 ]; then
    log "Runner stopped"
    exit 0
fi
log "Runner listener exited with status $LISTENER_STATUS"
exit "$LISTENER_STATUS"
