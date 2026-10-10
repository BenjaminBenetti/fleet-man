#!/usr/bin/env bash
# Description: the fleet binary is the Kubernetes backend's docker shim when started as docker or docker-compose from a kubeshim directory: capture mode passes builds to the real docker with their exit code, lists no containers, captures docker run (exit 97, env values kept 0600 and redacted in the journal), blocks events until the directory goes, and refuses compose and container verbs; pod mode answers inspect from the pod's container and has no engine.
# itest: no-docker
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() {
  if [ -n "${events_pid:-}" ]; then
    kill "${events_pid}" 2>/dev/null || true
  fi
}
itest_begin

setup_test

# A stand-in for the real docker: prints its argv and exits with $STUB_RC.
stub="${TEST_SCRATCH_DIR}/stub-docker"
cat > "${stub}" <<'EOF'
#!/bin/sh
echo "real docker:${*:+ $*}"
exit "${STUB_RC:-0}"
EOF
chmod +x "${stub}"

# shim_dir <mode-json>: a kubeshim directory whose links point at the fleet
# binary under test.
fleet_abs="$(command -v "${FLEET_BIN}")"
shim_dir() {
  local d
  d="$(mktemp -d "${TEST_SCRATCH_DIR}/kubeshim-XXXXXX")"
  ln -s "${fleet_abs}" "${d}/docker"
  ln -s "${fleet_abs}" "${d}/docker-compose"
  printf '%s\n' "$1" > "${d}/kubeshim.json"
  printf '%s\n' "${d}"
}

# call <binary> <args...> — the shim's exit code and output as "<rc>|<output>".
call() {
  local bin="$1" out rc=0
  shift
  out=$("${dir}/${bin}" "$@" 2>&1) || rc=$?
  printf '%s|%s\n' "${rc}" "${out}"
}

# --- capture mode ---
dir="$(shim_dir "{\"mode\":\"capture\",\"docker\":\"${stub}\"}")"

assert_equals "0|real docker: -v" "$(call docker -v)" "-v goes to the real docker"
assert_equals "3|real docker: buildx build --load -t img ." "$(STUB_RC=3 call docker buildx build --load -t img .)" \
  "a build goes to the real docker with its exit code"
assert_equals "0|real docker: inspect --type image img" "$(call docker inspect --type image img)" "an image inspect goes to the real docker"
assert_equals "0|" "$(call docker ps -q -a --filter label=devcontainer.local_folder=/ws)" "ps lists no containers"
got="$(call docker inspect --type container abc)"
assert_contains "${got}" "1|[]" "an inspect of a container finds nothing"
assert_contains "${got}" "No such container: abc" "and says so"

got="$(call docker run --sig-proxy=false -a STDOUT -e SECRET=s3cr3t-value -l devcontainer.local_folder=/ws --entrypoint /bin/sh img:1 -c 'echo Container started' -)"
assert_contains "${got}" "97|" "docker run is captured with exit 97"
assert_file_exists "${dir}/capture.raw.json"
assert_equals "600" "$(file_mode "${dir}/capture.raw.json")" "the capture (env values) is 0600"
assert_contains "$(cat "${dir}/capture.raw.json")" '"name":"SECRET","value":"s3cr3t-value","source":"flag"' "the capture keeps the env value"
assert_contains "$(cat "${dir}/capture.raw.json")" '"devcontainer.local_folder":"/ws"' "the capture keeps the labels"
assert_not_contains "$(cat "${dir}/calls.jsonl")" "s3cr3t-value" "the call journal redacts env values"
assert_contains "$(cat "${dir}/calls.jsonl")" '"outcome":"captured"' "the journal records the capture"
assert_contains "$(call docker run img)" "125|" "a second run is refused"

captured_dir="${dir}"
dir="$(shim_dir "{\"mode\":\"capture\",\"docker\":\"${stub}\"}")"
got="$(call docker run --frobnicate img)"
assert_contains "${got}" "125|" "a run with an unknown flag is refused"
assert_contains "${got}" "kubernetes:unsupported:flag:frobnicate" "with the unsupported-flag code"
assert_file_absent "${dir}/capture.raw.json"
dir="${captured_dir}"

for args in "compose up -d" "compose version --short"; do
  # shellcheck disable=SC2086
  got="$(call docker ${args})"
  assert_contains "${got}" "kubernetes:unsupported:compose:" "docker ${args} is refused"
  assert_not_contains "${got}" "real docker" "docker ${args} never reaches the real docker"
done
got="$(call docker-compose -f docker-compose.yml up)"
assert_contains "${got}" "kubernetes:unsupported:compose:" "docker-compose is refused"

for args in "exec c1 true" "start c1" "rm -f c1" "info" "push img"; do
  # shellcheck disable=SC2086
  got="$(call docker ${args})"
  assert_contains "${got}" "125|kubeshim (capture mode): refusing" "docker ${args} is refused"
done

# events blocks until the shim directory goes.
"${dir}/docker" events --format '{{json .}}' >/dev/null 2>&1 &
events_pid=$!
sleep 1
kill -0 "${events_pid}" 2>/dev/null || fail "docker events should block while its directory exists"
rm "${dir}/kubeshim.json"
for _ in $(seq 1 20); do
  kill -0 "${events_pid}" 2>/dev/null || break
  sleep 0.25
done
if kill -0 "${events_pid}" 2>/dev/null; then
  fail "docker events kept blocking after its directory went"
fi
wait "${events_pid}" || fail "docker events should end cleanly"
events_pid=""

# --- pod mode ---
cid='k8s:c1/fleet/fi-app-main-1a2b3c4d@g1'
dir="$(shim_dir "{\"mode\":\"pod\",\"pod\":{\"execSocket\":\"${TEST_SCRATCH_DIR}/none.sock\",\"container\":{\"containerId\":\"${cid}\",\"name\":\"fi-app-main-1a2b3c4d-0\",\"image\":\"img\",\"created\":\"2026-10-01T12:00:00Z\",\"startedAt\":\"2026-10-02T08:30:15.123456789Z\",\"user\":\"root\",\"labels\":{\"devcontainer.metadata\":\"[{\\\"remoteUser\\\":\\\"vscode\\\"}]\"}}}}")"
id="$(perl -MDigest::SHA=sha256_hex -e 'print sha256_hex(shift)' "${cid}")"
got="$(call docker inspect --type container "${cid}")"
assert_contains "${got}" "0|[" "inspect answers for the pod's container"
assert_contains "${got}" "\"Id\": \"${id}\"" "its Id is the sha256 of the ContainerID"
assert_contains "${got}" '"StartedAt": "2026-10-02T08:30:15.123456789Z"' "with the StartedAt fleet gave it"
assert_contains "${got}" 'devcontainer.metadata' "and the captured labels"
assert_contains "$(call docker inspect "${id:0:12}")" "0|[" "a 12-character id prefix names it"
assert_contains "$(call docker inspect other)" "No such object: other" "another name is not found"
assert_contains "$(call docker -v)" "1|kubeshim: no docker engine" "pod mode has no docker engine"
assert_contains "$(call docker exec "${cid}" true)" "125|kubeshim: exec: connecting to fleetd" "an exec with nothing serving it fails as the shim's error"
assert_contains "$(call docker ps)" "125|kubeshim (pod mode): refusing" "pod mode refuses the rest"

# --- outside a shim directory, or under its own name ---
lone="${TEST_SCRATCH_DIR}/lone"
mkdir -p "${lone}"
ln -s "${fleet_abs}" "${lone}/docker"
got="$("${lone}/docker" ps 2>&1)" && fail "a docker link outside a kubeshim directory should fail"
assert_contains "${got}" "kubeshim.json" "and name the missing mode file"
"${FLEET_BIN}" version >/dev/null || fail "fleet under its own name is unaffected"

pass "kubeshim multicall: capture mode, pod mode, refusals"
