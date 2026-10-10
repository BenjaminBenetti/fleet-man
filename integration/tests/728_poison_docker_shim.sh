#!/usr/bin/env bash
# Description: the poison-docker shim itself, against a stand-in docker: record passes every call through with its exit code, poison refuses exactly the container verbs (after global options, inspect of an image exempt, list overridable) with exit 125, and every call is logged on one line.
# itest: no-docker
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_test

# A stand-in for the real docker: prints its argv and exits with $STUB_RC.
stub_dir="${TEST_SCRATCH_DIR}/stub-docker"
mkdir -p "${stub_dir}"
cat > "${stub_dir}/docker" <<'EOF'
#!/bin/sh
echo "real docker:${*:+ $*}"
exit "${STUB_RC:-0}"
EOF
chmod +x "${stub_dir}/docker"
export PATH="${stub_dir}:${PATH}"

itest_poison_docker record
shim="${INTEGRATION_DIR}/poison-docker/docker"
assert_equals "${shim}" "$(command -v docker)" "the shim should come first on PATH"
assert_equals "${stub_dir}/docker" "${FLEET_ITEST_REAL_DOCKER}" "the real docker is the next one on PATH"
itest_poison_docker record
assert_equals "1" "$(printf '%s\n' "${PATH}" | tr ':' '\n' | grep -cxF "${INTEGRATION_DIR}/poison-docker")" \
  "installing twice must not stack the shim on PATH"
assert_equals "${stub_dir}/docker" "${FLEET_ITEST_REAL_DOCKER}" "a second install must not take the shim for the real docker"

# run <args...> — the shim's output and exit code as "<rc>|<output>".
run() {
  local out rc=0
  out=$(docker "$@" 2>&1) || rc=$?
  printf '%s|%s\n' "${rc}" "${out}"
}

# --- record: everything passes, exit codes included ---
assert_equals "0|real docker: exec c1 true" "$(run exec c1 true)" "record passes container verbs through"
assert_equals "3|real docker: run --rm img" "$(STUB_RC=3 run run --rm img)" "record keeps the real exit code"
itest_poison_docker_saw '^pass exec c1 true$' || fail "record should log the call: $(itest_poison_docker_log)"
if itest_poison_docker_saw '^refused '; then
  fail "record must not refuse anything: $(itest_poison_docker_log)"
fi

# --- poison: container verbs refused, the rest passes ---
itest_poison_docker poison
for args in "exec c1 true" "run --rm img" "start c1" "stop c1" "rm -f c1" "logs c1" "stats --no-stream" \
  "ps -a" "commit c1 img" "cp c1:/x /y" "inspect c1" "inspect --type container c1" "container ls" \
  "--context ctx exec c1 true" "-H unix:///sock ps" "--log-level debug start c1"; do
  got=$(run ${args})
  case "${got}" in
    "125|poison docker: refused 'docker ${args}'"*) ;;
    *) fail "poison should refuse 'docker ${args}' with exit 125, got: ${got}" ;;
  esac
done
for args in "version" "info" "build -t img ." "buildx build ." "image ls" "image inspect img" \
  "inspect --type image img" "inspect --type=image img" "network create n" "volume ls" "pull img" "tag a b"; do
  assert_equals "0|real docker: ${args}" "$(run ${args})" "poison should pass 'docker ${args}' through"
done
assert_equals "0|real docker:" "$(run)" "poison should pass a bare docker through"
itest_poison_docker_saw '^refused inspect c1$' || fail "a refusal should be logged as refused: $(itest_poison_docker_log)"
itest_poison_docker_saw '^pass inspect --type image img$' || fail "a pass should be logged as pass: $(itest_poison_docker_log)"

# A multi-line argument (a script handed to exec) is logged on one line.
run exec c1 sh -c "$(printf 'echo one\necho two')" >/dev/null
itest_poison_docker_saw '^refused exec c1 sh -c echo one\\necho two$' \
  || fail "a multi-line argument should be logged with \\n on one line: $(itest_poison_docker_log)"

# The refused-verb list can be overridden.
export FLEET_ITEST_POISON_VERBS=build
assert_equals "0|real docker: exec c1 true" "$(run exec c1 true)" "an overridden list passes verbs it leaves out"
case "$(run build .)" in
  "125|"*) ;;
  *) fail "an overridden list should refuse the verbs it names" ;;
esac
unset FLEET_ITEST_POISON_VERBS

# A shim with no real docker to hand calls to says so instead of recursing.
rc=0
out=$(FLEET_ITEST_REAL_DOCKER="" "${shim}" version 2>&1) || rc=$?
assert_equals "125" "${rc}" "a shim without a real docker should fail"
assert_contains "${out}" "FLEET_ITEST_REAL_DOCKER" "and say which variable is missing"

pass "poison-docker shim: record, poison, log"
