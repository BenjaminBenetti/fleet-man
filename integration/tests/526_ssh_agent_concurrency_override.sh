#!/usr/bin/env bash
# itest: k8s
# Description: 25 concurrent signers in an instance all get a good signature through its agent relay; on a cluster FLEET_SSH_AGENT_SOCK naming a live socket on the daemon host serves the pods that agent, and naming a path missing there leaves them on the daemon's relay, with a warning in fleet.log.
set -euo pipefail

source "$(dirname "$0")/../common.sh"

agent_dir=""
agent_pids=()
itest_cleanup() {
  pkill -f "${FLEET_BIN} server" >/dev/null 2>&1 || true
  # Only the agents this test started — never one inherited from the runner.
  local pid
  for pid in "${agent_pids[@]+"${agent_pids[@]}"}"; do kill "${pid}" >/dev/null 2>&1 || true; done
  if [ -n "${agent_dir}" ]; then rm -rf "${agent_dir}"; fi
}
itest_begin

setup_test

# Two throwaway agents on the daemon host, each holding its own key: A is the
# agent the daemon is started with (the relay's), B one FLEET_SSH_AGENT_SOCK
# names. A short dir: unix socket paths are capped at 108 bytes.
agent_dir="$(mktemp -d /tmp/fleet-agent.XXXXXX)"
ssh-keygen -q -t ed25519 -N '' -C fleet-itest-a -f "${agent_dir}/a"
ssh-keygen -q -t ed25519 -N '' -C fleet-itest-b -f "${agent_dir}/b"
eval "$(ssh-agent -a "${agent_dir}/a.sock" -s)" >/dev/null
agent_pids+=("${SSH_AGENT_PID}")
ssh-add -q "${agent_dir}/a"
b_env="$(ssh-agent -a "${agent_dir}/b.sock" -s)"
agent_pids+=("$(printf '%s\n' "${b_env}" | sed -nE 's/^SSH_AGENT_PID=([0-9]+);.*/\1/p')")
SSH_AUTH_SOCK="${agent_dir}/b.sock" ssh-add -q "${agent_dir}/b"
fp_a="$(ssh-keygen -lf "${agent_dir}/a.pub" | awk '{print $2}')"
fp_b="$(ssh-keygen -lf "${agent_dir}/b.pub" | awk '{print $2}')"
info "agent A ${fp_a}, agent B ${fp_b}"

stop_daemon() {
  pkill -f "${FLEET_BIN} server" >/dev/null 2>&1 || true
  local deadline=$(( $(date +%s) + $(_scale_timeout 10) ))
  while [ "$(server_count)" != "0" ] && [ "$(date +%s)" -lt "${deadline}" ]; do
    sleep 0.2
  done
  assert_equals "0" "$(server_count)" "daemon should be stopped"
}
# The daemon relays the agent it was started with: start it from here.
stop_daemon

info "fleet up alpha"
fleet_up alpha
inst="${FIXTURE_REPO_NAME}/alpha"

# wait_for_key <fingerprint> — the instance's agent lists that key (a new
# daemon serves the instance's socket once its session is back).
wait_for_key() {
  local out="" deadline=$(( $(date +%s) + $(_scale_timeout 60) ))
  while [ "$(date +%s)" -lt "${deadline}" ]; do
    out="$("${FLEET_BIN}" exec "${inst}" -- ssh-add -l 2>&1 || true)"
    case "${out}" in *"$1"*) printf '%s' "${out}"; return 0 ;; esac
    sleep 0.5
  done
  fail "the instance's agent never listed $1: ${out}"
}

# sign25 <fingerprint> <pubkey> <what> — 25 signers at once in the instance,
# each through its own agent connection; every one must succeed, and every
# signature must verify on the host against the key the agent holds.
sign25() {
  local fp=$1 pub=$2 what=$3 out
  out="$("${FLEET_BIN}" exec "${inst}" -- bash -c '
    set -u
    d=$(mktemp -d) && cd "$d" || exit 1
    ssh-add -L | grep -F "'"$(awk '{print $2}' "${pub}")"'" > k.pub || { echo "NOKEY"; exit 1; }
    for i in $(seq 1 25); do
      echo "message $i" > "m$i"
      ( ssh-keygen -q -Y sign -f k.pub -n fleet-itest "m$i" 2>"e$i" ) &
    done
    ok=0
    for job in $(jobs -p); do wait "$job" && ok=$((ok+1)); done
    echo "OK=$ok"
    for i in $(seq 1 25); do
      [ -s "m$i.sig" ] || { echo "MISSING $i: $(cat "e$i")"; continue; }
      printf "SIG %s " "$i"; base64 -w0 < "m$i.sig"; echo
    done
    cd / && rm -rf "$d"
  ')" || fail "${what}: the signers' run failed: ${out}"
  assert_contains "${out}" "OK=25" "${what}: all 25 concurrent signers should succeed"
  assert_not_contains "${out}" "MISSING" "${what}: every signer leaves a signature"
  local signers="${TEST_SCRATCH_DIR}/allowed_signers" n=0 i sig
  printf 'itest %s\n' "$(cat "${pub}")" > "${signers}"
  while read -r _ i sig; do
    printf '%s' "${sig}" | base64 -d > "${TEST_SCRATCH_DIR}/m${i}.sig"
    echo "message ${i}" | ssh-keygen -q -Y verify -f "${signers}" -I itest -n fleet-itest \
      -s "${TEST_SCRATCH_DIR}/m${i}.sig" >/dev/null 2>&1 \
      || fail "${what}: signature ${i} does not verify against ${fp}"
    n=$((n + 1))
  done < <(printf '%s\n' "${out}" | grep '^SIG ')
  assert_equals "25" "${n}" "${what}: signatures verified on the host"
}

info "25 concurrent signers through the relay (agent A)"
wait_for_key "${fp_a}" >/dev/null
sign25 "${fp_a}" "${agent_dir}/a.pub" "relay"

if itest_backend_is kubernetes; then
  # On a cluster the override is the daemon's to dial for each connection a
  # pod relays (docker bind-mounts it at create), so a restart changes it
  # for the running instance.
  info "kubernetes: FLEET_SSH_AGENT_SOCK naming a live host socket serves agent B"
  stop_daemon
  FLEET_SSH_AGENT_SOCK="${agent_dir}/b.sock" "${FLEET_BIN}" ls >/dev/null
  out="$(wait_for_key "${fp_b}")"
  assert_not_contains "${out}" "${fp_a}" "the override's agent only, not the relay's"
  sign25 "${fp_b}" "${agent_dir}/b.pub" "live override"

  info "kubernetes: FLEET_SSH_AGENT_SOCK naming a path missing on the host falls back to the relay"
  missing="${agent_dir}/missing.sock"
  stop_daemon
  FLEET_SSH_AGENT_SOCK="${missing}" "${FLEET_BIN}" ls >/dev/null
  out="$(wait_for_key "${fp_a}")"
  assert_not_contains "${out}" "${fp_b}" "the relay's agent, not the earlier override's"
  sign25 "${fp_a}" "${agent_dir}/a.pub" "missing override"
  # The warning is written once a pod is asked about, i.e. right away here.
  log="${HOME}/.fleet/fleet.log"
  line=""
  deadline=$(( $(date +%s) + $(_scale_timeout 30) ))
  while [ "$(date +%s)" -lt "${deadline}" ]; do
    line="$(grep -F 'FLEET_SSH_AGENT_SOCK is not a socket on the daemon host' "${log}" 2>/dev/null | tail -1 || true)"
    [ -n "${line}" ] && break
    sleep 0.5
  done
  [ -n "${line}" ] || fail "fleet.log has no warning that FLEET_SSH_AGENT_SOCK names no socket: $(tail -20 "${log}" 2>/dev/null)"
  assert_contains "${line}" "level=WARN" "the fallback warning's level"
  assert_contains "${line}" "${missing}" "the fallback warning names the path"
  stop_daemon
fi

pass "25 concurrent signers through the agent relay$(itest_backend_is kubernetes && printf '; pods follow a live FLEET_SSH_AGENT_SOCK and fall back to the relay, with a warning, for a missing one')"
