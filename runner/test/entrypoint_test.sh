#!/usr/bin/env bash
# Hermetic tests for runner/entrypoint.sh.
#
# config.sh and the bundled node runtime are replaced by fakes that log their
# invocations, so the tests need no network, Docker or GitHub access and run
# on macOS (bash 3.2) as well as Linux. Run: bash runner/test/entrypoint_test.sh [test...]
set -u

HERE=$(cd "$(dirname "$0")" && pwd)
ENTRYPOINT="$HERE/../entrypoint.sh"
PASSED=0
FAILED=0

setup() {
    TMP=$(mktemp -d)
    export RUNNER_HOME="$TMP/runner"
    export FAKE_LOG="$TMP/calls.log"
    export FAKE_NODE_BEHAVIOUR=exit
    mkdir -p "$RUNNER_HOME/bin" "$RUNNER_HOME/externals/node20/bin" "$RUNNER_HOME/externals/node24/bin" "$TMP/fakebin"
    : > "$FAKE_LOG"

    cat > "$TMP/fakebin/apt-get" <<'EOF'
#!/usr/bin/env bash
echo "apt-get $*" >> "$FAKE_LOG"
EOF

    cat > "$TMP/fakebin/curl" <<'EOF'
#!/usr/bin/env bash
echo "curl $*" >> "$FAKE_LOG"
exit 7
EOF

    cat > "$RUNNER_HOME/config.sh" <<'EOF'
#!/usr/bin/env bash
echo "config.sh $*" >> "$FAKE_LOG"
echo '{}' > .runner
echo "$PATH" > .path
EOF

    # Only the newest node runtime is a working fake; the older one must be ignored.
    cat > "$RUNNER_HOME/externals/node24/bin/node" <<'EOF'
#!/usr/bin/env bash
echo "node $* RUNNER_TOKEN=${RUNNER_TOKEN:-unset}" >> "$FAKE_LOG"
case "${FAKE_NODE_BEHAVIOUR:-exit}" in
    wait)
        trap 'echo "node got INT" >> "$FAKE_LOG"; exit 0' INT
        trap 'echo "node got TERM" >> "$FAKE_LOG"; exit 0' TERM
        while :; do sleep 0.2; done
        ;;
    crash) exit 2 ;;
    *) exit 0 ;;
esac
EOF
    # shellcheck disable=SC2016  # $FAKE_LOG must stay literal inside the fake script
    printf '#!/usr/bin/env bash\necho "WRONG NODE USED" >> "$FAKE_LOG"; exit 99\n' > "$RUNNER_HOME/externals/node20/bin/node"
    : > "$RUNNER_HOME/bin/RunnerService.js"
    chmod +x "$TMP/fakebin/apt-get" "$TMP/fakebin/curl" "$RUNNER_HOME/config.sh" "$RUNNER_HOME/externals/node24/bin/node" "$RUNNER_HOME/externals/node20/bin/node"

    ORIG_PATH=$PATH
    export PATH="$TMP/fakebin:$PATH"
    export RUNNER_REGISTER_TO=oeasenet
    export RUNNER_TOKEN=AREGTOKEN
    unset RUNNER_NAME RUNNER_NAME_PREFIX RUNNER_LABELS RUNNER_GROUP RUNNER_WORKDIR RUNNER_EPHEMERAL \
        RUNNER_DISABLE_UPDATE ADDITIONAL_FLAGS ADDITIONAL_PACKAGES RUNNER_GRACEFUL_STOP_TIMEOUT GITHUB_URL
}

teardown() {
    export PATH=$ORIG_PATH
    rm -rf "$TMP"
}

run_entrypoint() {
    bash "$ENTRYPOINT" >"$TMP/stdout" 2>"$TMP/stderr"
    ENTRY_STATUS=$?
}

fail() { echo "    ✗ $*"; TEST_FAILED=1; }
assert_log_contains() { grep -qF -- "$1" "$FAKE_LOG" || fail "expected log to contain: $1"; }
assert_log_not_contains() { grep -qF -- "$1" "$FAKE_LOG" && fail "expected log NOT to contain: $1"; }
assert_status() { [ "$ENTRY_STATUS" -eq "$1" ] || fail "expected exit status $1, got $ENTRY_STATUS (stderr: $(cat "$TMP/stderr"))"; }
config_line() { grep -F 'config.sh --' "$FAKE_LOG" | head -n1; }

run_test() {
    TEST_FAILED=0
    setup
    "$1"
    teardown
    if [ "$TEST_FAILED" -eq 0 ]; then PASSED=$((PASSED + 1)); echo "ok   $1"; else FAILED=$((FAILED + 1)); echo "FAIL $1"; fi
}

# ---------------------------------------------------------------------------

test_registers_org_runner_with_all_configured_flags() {
    export RUNNER_NAME=my-runner RUNNER_LABELS=docker,oease RUNNER_WORKDIR=/srv/work RUNNER_GROUP=prod
    run_entrypoint
    assert_status 0
    line=$(config_line)
    for expected in "--unattended" "--replace" "--url https://github.com/oeasenet" "--token AREGTOKEN" \
        "--name my-runner" "--labels docker,oease" "--work /srv/work" "--runnergroup prod"; do
        case "$line" in *"$expected"*) ;; *) fail "config.sh call missing '$expected': $line" ;; esac
    done
    assert_log_contains "bin/RunnerService.js"
    assert_log_not_contains "WRONG NODE USED"
}

test_registers_repo_runner() {
    export RUNNER_REGISTER_TO=oeasenet/platform
    run_entrypoint
    assert_status 0
    assert_log_contains "--url https://github.com/oeasenet/platform"
}

test_token_is_not_exposed_to_the_listener_or_jobs() {
    run_entrypoint
    assert_status 0
    assert_log_contains "RUNNER_TOKEN=unset"
}

test_skips_registration_when_already_configured() {
    echo '{"agentName":"x"}' > "$RUNNER_HOME/.runner"
    unset RUNNER_TOKEN
    run_entrypoint
    assert_status 0
    assert_log_not_contains "config.sh"
    assert_log_contains "bin/RunnerService.js"
}

test_fails_fast_without_token_when_not_configured() {
    unset RUNNER_TOKEN
    run_entrypoint
    assert_status 1
    assert_log_not_contains "config.sh"
    grep -q "RUNNER_TOKEN" "$TMP/stderr" || fail "error message should name RUNNER_TOKEN"
}

test_fails_fast_without_target() {
    unset RUNNER_REGISTER_TO
    run_entrypoint
    assert_status 1
    assert_log_not_contains "config.sh"
    grep -q "RUNNER_REGISTER_TO" "$TMP/stderr" || fail "error message should name RUNNER_REGISTER_TO"
}

test_defaults_name_to_hostname_and_omits_optional_flags() {
    run_entrypoint
    assert_status 0
    line=$(config_line)
    case "$line" in *"--name $(hostname)"*) ;; *) fail "expected --name $(hostname): $line" ;; esac
    case "$line" in *"--work _work"*) ;; *) fail "expected default --work _work: $line" ;; esac
    for absent in "--labels" "--runnergroup" "--ephemeral" "--disableupdate"; do
        case "$line" in *"$absent"*) fail "did not expect '$absent': $line" ;; esac
    done
}

test_name_prefix_is_prepended_to_hostname() {
    export RUNNER_NAME_PREFIX=oease-prod
    run_entrypoint
    assert_status 0
    assert_log_contains "--name oease-prod-$(hostname)"
}

test_explicit_name_wins_over_prefix() {
    export RUNNER_NAME_PREFIX=oease-prod RUNNER_NAME=fixed-name
    run_entrypoint
    assert_status 0
    assert_log_contains "--name fixed-name "
    assert_log_not_contains "oease-prod"
}

test_ephemeral_and_disable_update_flags() {
    export RUNNER_EPHEMERAL=true RUNNER_DISABLE_UPDATE=true
    run_entrypoint
    assert_status 0
    assert_log_contains "--ephemeral"
    assert_log_contains "--disableupdate"
}

test_additional_flags_are_passed_through() {
    export ADDITIONAL_FLAGS="--no-default-labels --foo bar"
    run_entrypoint
    assert_status 0
    assert_log_contains "--no-default-labels --foo bar"
}

test_installs_additional_packages_before_registering() {
    export ADDITIONAL_PACKAGES=kubectl,awscli
    run_entrypoint
    assert_status 0
    assert_log_contains "apt-get install -y --no-install-recommends kubectl awscli"
    apt_line=$(grep -n "apt-get install" "$FAKE_LOG" | head -n1 | cut -d: -f1)
    config_line_no=$(grep -n "config.sh --" "$FAKE_LOG" | head -n1 | cut -d: -f1)
    [ -n "$config_line_no" ] && [ "$apt_line" -lt "$config_line_no" ] || fail "packages must be installed before registration"
}

test_custom_github_url_for_enterprise() {
    export GITHUB_URL=https://github.example.com/
    run_entrypoint
    assert_status 0
    assert_log_contains "--url https://github.example.com/oeasenet"
}

test_never_calls_out_over_the_network_itself() {
    run_entrypoint
    assert_status 0
    assert_log_not_contains "curl"
}

test_listener_crash_propagates_status() {
    export FAKE_NODE_BEHAVIOUR=crash
    run_entrypoint
    assert_status 2
}

test_graceful_stop_waits_for_running_job_then_stops_listener() {
    export FAKE_NODE_BEHAVIOUR=wait
    bash "$ENTRYPOINT" >"$TMP/stdout" 2>"$TMP/stderr" &
    entry_pid=$!
    for _ in $(seq 1 50); do grep -q "RunnerService.js" "$FAKE_LOG" 2>/dev/null && break; sleep 0.1; done
    grep -q "RunnerService.js" "$FAKE_LOG" || { fail "listener never started"; kill "$entry_pid" 2>/dev/null; return; }

    # Simulate an in-flight job: the runner spawns Runner.Worker while a job runs.
    bash -c 'exec -a Runner.Worker sleep 3' &
    job_pid=$!
    job_started=$(date +%s)
    sleep 0.3

    kill -TERM "$entry_pid"
    wait "$entry_pid"; ENTRY_STATUS=$?
    stopped_at=$(date +%s)
    wait "$job_pid" 2>/dev/null

    assert_status 0
    assert_log_contains "node got TERM"
    [ "$stopped_at" -ge $((job_started + 3)) ] || fail "stopped at $stopped_at before the 3s job finished (started $job_started)"
}

test_graceful_stop_gives_up_waiting_after_timeout() {
    export FAKE_NODE_BEHAVIOUR=wait RUNNER_GRACEFUL_STOP_TIMEOUT=1
    bash "$ENTRYPOINT" >"$TMP/stdout" 2>"$TMP/stderr" &
    entry_pid=$!
    for _ in $(seq 1 50); do grep -q "RunnerService.js" "$FAKE_LOG" 2>/dev/null && break; sleep 0.1; done

    bash -c 'exec -a Runner.Worker sleep 6' &
    job_pid=$!
    job_started=$(date +%s)
    sleep 0.3

    kill -TERM "$entry_pid"
    wait "$entry_pid"; ENTRY_STATUS=$?
    stopped_at=$(date +%s)
    kill "$job_pid" 2>/dev/null; wait "$job_pid" 2>/dev/null

    assert_status 0
    assert_log_contains "node got TERM"
    [ "$stopped_at" -lt $((job_started + 5)) ] || fail "should have stopped after the 1s timeout, stopped at $stopped_at (job started $job_started)"
}

# ---------------------------------------------------------------------------

# Run all tests, or only the ones named on the command line.
if [ "$#" -gt 0 ]; then
    for t in "$@"; do run_test "$t"; done
else
    for t in $(declare -F | awk '{print $3}' | grep '^test_'); do run_test "$t"; done
fi

echo
echo "$PASSED passed, $FAILED failed"
[ "$FAILED" -eq 0 ]
