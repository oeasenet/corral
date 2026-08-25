#!/usr/bin/env bash
# Tests for scripts/flavors.sh against fixture catalogs. Run: bash scripts/test/flavors_test.sh
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
SCRIPT="$HERE/../flavors.sh"
PASSED=0; FAILED=0; TEST_FAILED=0
fail() { echo "    ✗ $*"; TEST_FAILED=1; }
run_test() { TEST_FAILED=0; TMP=$(mktemp -d); export IMAGES_DIR="$TMP"; "$1"; rm -rf "$TMP"; if [ "$TEST_FAILED" -eq 0 ]; then PASSED=$((PASSED+1)); echo "ok   $1"; else FAILED=$((FAILED+1)); echo "FAIL $1"; fi; }
flavor() { mkdir -p "$IMAGES_DIR/$1"; printf '%s\n' "$2" > "$IMAGES_DIR/$1/flavor.json"; }

test_list_puts_the_default_first_then_alphabetical() {
    flavor zeta '{"label":"Zeta","base":"zeta:1"}'
    flavor alpha '{"label":"Alpha","base":"alpha:1"}'
    flavor mid '{"label":"Mid","base":"mid:1","default":true}'
    [ "$(bash "$SCRIPT" list | tr '\n' ' ')" = "mid alpha zeta " ] || fail "list: $(bash "$SCRIPT" list | tr '\n' ' ')"
}

test_get_reads_fields_and_picks_the_dockerfile() {
    flavor plain '{"label":"Plain","base":"debian:13","default":true}'
    flavor own '{"label":"Own","base":"fedora:42"}'
    : > "$IMAGES_DIR/own/Dockerfile"
    [ "$(bash "$SCRIPT" get plain base)" = "debian:13" ] || fail "base"
    [ "$(bash "$SCRIPT" get plain label)" = "Plain" ] || fail "label"
    [ "$(bash "$SCRIPT" get plain dockerfile)" = "images/Dockerfile" ] || fail "shared dockerfile: $(bash "$SCRIPT" get plain dockerfile)"
    [ "$(bash "$SCRIPT" get own dockerfile)" = "images/own/Dockerfile" ] || fail "own dockerfile: $(bash "$SCRIPT" get own dockerfile)"
    [ "$(bash "$SCRIPT" get own default)" = "false" ] || fail "default false"
    bash "$SCRIPT" get nope base >/dev/null 2>&1 && fail "unknown flavor must fail"
}

test_matrix_is_json_with_one_entry_per_flavor() {
    flavor ubuntu '{"label":"Ubuntu 26.04","base":"ubuntu:26.04","default":true}'
    flavor debian '{"label":"Debian 13","base":"debian:13"}'
    out=$(bash "$SCRIPT" matrix)
    echo "$out" | python3 -c '
import json,sys
m=json.load(sys.stdin)
assert [e["flavor"] for e in m]==["ubuntu","debian"], m
u=m[0]; assert u["name"]=="runner-ubuntu" and u["image"]=="runner" and u["base"]=="ubuntu:26.04" and u["dockerfile"]=="images/Dockerfile" and u["default"] is True and u["tag_prefix"]=="ubuntu-", u
assert m[1]["default"] is False
' || fail "matrix: $out"
}

test_rejects_bad_names_and_missing_fields() {
    flavor ok '{"label":"x","base":"y","default":true}'
    flavor 'Bad_Name' '{"label":"x","base":"y"}'
    bash "$SCRIPT" list >/dev/null 2>&1 && fail "uppercase/underscore names must be rejected"
    rm -rf "$IMAGES_DIR/Bad_Name"
    long63=$(printf 'l%.0s' $(seq 1 63)); long64="${long63}l"
    flavor "$long64" '{"label":"x","base":"y"}'
    bash "$SCRIPT" list >/dev/null 2>&1 && fail "64-char names must be rejected"
    rm -rf "${IMAGES_DIR:?}/$long64"
    flavor "$long63" '{"label":"x","base":"y"}'
    bash "$SCRIPT" list >/dev/null 2>&1 || fail "63-char names must pass"
    rm -rf "${IMAGES_DIR:?}/$long63"
    flavor nobase '{"label":"x"}'
    bash "$SCRIPT" list >/dev/null 2>&1 && fail "a flavor without base must be rejected"
}

test_requires_exactly_one_default() {
    flavor a '{"label":"A","base":"a:1","default":true}'
    flavor b '{"label":"B","base":"b:1","default":true}'
    bash "$SCRIPT" list >/dev/null 2>&1 && fail "two defaults must be rejected"
    rm -rf "$IMAGES_DIR/b"; flavor b '{"label":"B","base":"b:1"}'
    bash "$SCRIPT" list >/dev/null 2>&1 || fail "one default must pass"
    rm -rf "$IMAGES_DIR/a"
    bash "$SCRIPT" list >/dev/null 2>&1 && fail "no default must be rejected"
}

test_real_catalog_has_three_flavors_with_ubuntu_default() {
    export IMAGES_DIR="$HERE/../../images"
    [ "$(bash "$SCRIPT" list | head -n1)" = "ubuntu" ] || fail "ubuntu should be the default"
    [ "$(bash "$SCRIPT" list | wc -l | tr -d ' ')" = "3" ] || fail "expected 3 flavors: $(bash "$SCRIPT" list | tr '\n' ' ')"
}

for t in $(declare -F | awk '{print $3}' | grep '^test_'); do run_test "$t"; done
echo; echo "$PASSED passed, $FAILED failed"; [ "$FAILED" -eq 0 ]
