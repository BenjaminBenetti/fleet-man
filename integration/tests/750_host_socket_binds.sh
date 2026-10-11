#!/usr/bin/env bash
# itest: k8s
# Description: a devcontainer.json that bind-mounts the host's SSH agent (${localEnv:SSH_AUTH_SOCK}) and gpg-agent sockets gets working agents at those paths (ssh and gpg signatures made in the instance verify on the host), and its remoteEnv SSH_AUTH_SOCK wins over fleet's relay. On a cluster the agent bind is a link to the pod's agent relay, and the gpg socket one the pod agent binds, owned by the remote user and relayed to the host socket recorded at create; one fleetd cannot reach reaches fleet.log at error with its fleet and instance.
set -euo pipefail

source "$(dirname "$0")/../common.sh"

agent_dir=""
test_agent_pid=""
gnupg_home=""
itest_cleanup() {
  # The daemon was started with this test's agents in its environment.
  pkill -f "${FLEET_BIN} server" >/dev/null 2>&1 || true
  if [ -n "${test_agent_pid}" ]; then kill "${test_agent_pid}" >/dev/null 2>&1 || true; fi
  if [ -n "${gnupg_home}" ]; then
    GNUPGHOME="${gnupg_home}" gpgconf --kill gpg-agent >/dev/null 2>&1 || true
    rm -rf "${gnupg_home}"
  fi
  if [ -n "${agent_dir}" ]; then rm -rf "${agent_dir}"; fi
}
itest_begin

# The fixture binds ${localEnv:SSH_AUTH_SOCK} at /run/itest-host-ssh-agent.sock
# and ${localEnv:FLEET_ITEST_GPG_SOCK} at /run/itest-host-gpg-agent.sock, the
# way projects forward agents themselves.
FIXTURE_SRC="${INTEGRATION_DIR}/fixture-hostsock" setup_test

# Throwaway agents holding fresh keys. Short dirs: unix socket paths are capped
# at 108 bytes.
agent_dir="$(mktemp -d /tmp/fleet-agent.XXXXXX)"
ssh-keygen -q -t ed25519 -N '' -C fleet-itest-key -f "${agent_dir}/key"
eval "$(ssh-agent -a "${agent_dir}/agent.sock" -s)" >/dev/null
test_agent_pid="${SSH_AGENT_PID}"
ssh-add -q "${agent_dir}/key"
fingerprint="$(ssh-keygen -lf "${agent_dir}/key.pub" | awk '{print $2}')"
info "ssh key ${fingerprint}"

gnupg_home="$(mktemp -d /tmp/fleet-gpg.XXXXXX)"
chmod 700 "${gnupg_home}"
export GNUPGHOME="${gnupg_home}"
gpg --batch --quiet --passphrase '' --quick-gen-key 'Fleet Itest <itest@fleet.local>' ed25519 sign never 2>/dev/null
gpgconf --launch gpg-agent
# The restricted "extra" socket, the one meant for forwarding to a remote.
FLEET_ITEST_GPG_SOCK="$(gpgconf --list-dirs agent-extra-socket)"
export FLEET_ITEST_GPG_SOCK
[ -S "${FLEET_ITEST_GPG_SOCK}" ] || fail "gpg-agent extra socket missing: ${FLEET_ITEST_GPG_SOCK}"
info "gpg-agent extra socket ${FLEET_ITEST_GPG_SOCK}"

# `devcontainer up` resolves ${localEnv:...} in the daemon's environment, so the
# daemon must be (re)started from this one.
pkill -f "${FLEET_BIN} server" >/dev/null 2>&1 || true
deadline=$(( $(date +%s) + $(_scale_timeout 10) ))
while [ "$(server_count)" != "0" ] && [ "$(date +%s)" -lt "${deadline}" ]; do sleep 0.2; done
assert_equals "0" "$(server_count)" "daemon should be stopped"

fleet_up alpha
inst="${FIXTURE_REPO_NAME}/alpha"

info "the bound SSH agent lists the host key"
out=$("${FLEET_BIN}" exec "${inst}" -- sh -c 'test -S /run/itest-host-ssh-agent.sock && SSH_AUTH_SOCK=/run/itest-host-ssh-agent.sock ssh-add -l')
assert_contains "${out}" "${fingerprint}" "ssh-add -l through the bound socket"

info "an ssh signature made in the instance verifies on the host"
sig=$("${FLEET_BIN}" exec "${inst}" -- sh -c '
  cd "$(mktemp -d)" && echo fleet-itest-payload > m &&
  SSH_AUTH_SOCK=/run/itest-host-ssh-agent.sock ssh-add -L > k.pub &&
  SSH_AUTH_SOCK=/run/itest-host-ssh-agent.sock ssh-keygen -q -Y sign -f k.pub -n fleet-itest m >/dev/null && cat m.sig')
printf '%s\n' "${sig}" > "${agent_dir}/m.sig"
printf 'itest %s\n' "$(cat "${agent_dir}/key.pub")" > "${agent_dir}/allowed_signers"
echo fleet-itest-payload | ssh-keygen -Y verify -f "${agent_dir}/allowed_signers" -I itest -n fleet-itest -s "${agent_dir}/m.sig" >/dev/null 2>&1 \
  || fail "the instance's ssh signature does not verify on the host: ${sig}"

info "a gpg signature made in the instance through the bound gpg-agent verifies on the host"
pubkey=$(gpg --armor --export itest@fleet.local)
# A scratch keyring holding only the public key; its agent socket is the bound
# one, so the private key never leaves the host.
signed=$("${FLEET_BIN}" exec "${inst}" -- sh -c '
  test -S /run/itest-host-gpg-agent.sock || { echo "no bound gpg socket" >&2; exit 1; }
  export GNUPGHOME="$(mktemp -d)"
  ln -s /run/itest-host-gpg-agent.sock "${GNUPGHOME}/S.gpg-agent"
  printf "%s\n" "$1" | gpg --batch --quiet --import 2>/dev/null
  echo fleet-itest-gpg-payload | gpg --batch --no-autostart --armor --local-user itest@fleet.local --sign' sh "${pubkey}")
verified=$(printf '%s\n' "${signed}" | gpg --batch --decrypt 2>&1) \
  || fail "the instance's gpg signature does not verify on the host: ${verified}"
assert_contains "${verified}" "fleet-itest-gpg-payload" "the signed payload"
assert_contains "${verified}" 'Good signature from "Fleet Itest <itest@fleet.local>"' "gpg --verify on the host"

if itest_backend_is kubernetes; then
  info "the pod's agent bind is a link to its agent relay; its gpg socket is the remote user's"
  assert_equals "/fleet-mounts/control/ssh-agent.sock" "$("${FLEET_BIN}" exec "${inst}" -- readlink /run/itest-host-ssh-agent.sock)" \
    "the ssh-agent bind's link"
  assert_equals "$("${FLEET_BIN}" exec "${inst}" -- id -un)" "$("${FLEET_BIN}" exec "${inst}" -- stat -c %U /run/itest-host-gpg-agent.sock)" \
    "the gpg socket's owner"
fi

info "the config's remoteEnv SSH_AUTH_SOCK is what instance processes get"
# fleet passes its own relay socket on every exec, but a devcontainer.json
# remoteEnv entry takes precedence over it (devcontainer CLI semantics), so a
# project that forwards its agent itself keeps its own path.
assert_equals "/run/itest-host-ssh-agent.sock" "$("${FLEET_BIN}" exec "${inst}" -- printenv SSH_AUTH_SOCK)" \
  "SSH_AUTH_SOCK inside the instance"
out=$("${FLEET_BIN}" exec "${inst}" -- ssh-add -l)
assert_contains "${out}" "${fingerprint}" "the host key through the instance's own SSH_AUTH_SOCK"

if itest_backend_is kubernetes; then
  info "a host socket fleetd cannot reach fails the pod's connection and reaches fleet.log at error, tagged with the instance"
  GNUPGHOME="${gnupg_home}" gpgconf --kill gpg-agent >/dev/null 2>&1 || true
  if "${FLEET_BIN}" exec "${inst}" -- curl -sS --max-time 10 --unix-socket /run/itest-host-gpg-agent.sock http://gpg-agent/ >/dev/null 2>&1; then
    fail "a connection to the bound gpg socket was served with the host's gpg-agent gone"
  fi
  log="${HOME}/.fleet/fleet.log"
  line=""
  deadline=$(( $(date +%s) + $(_scale_timeout 15) ))
  until [ -n "${line}" ]; do
    [ "$(date +%s)" -lt "${deadline}" ] || fail "no host-socket relay error for alpha in fleet.log"
    line=$(grep ' level=ERROR' "${log}" 2>/dev/null | grep 'could not reach a host socket' | grep 'instance=alpha' | tail -n 1 || true)
    [ -n "${line}" ] || sleep 0.5
  done
  printf 'fleet.log: %s\n' "${line}"
  assert_contains "${line}" "fleet=${FIXTURE_REPO_NAME}" "the error is not tagged with its fleet"
  assert_contains "${line}" "target=/run/itest-host-gpg-agent.sock" "the error does not name the bind"
fi

pass "host agent socket binds"
