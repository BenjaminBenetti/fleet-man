#!/usr/bin/env bash
# itest: no-docker
# Description: the itest harness itself: marker parsing, the state.json container-id reader, FLEET_ITEST_BACKEND dispatch and run.sh's kubernetes selection and parity summary.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

# Harness-only: no instance, so no setup_test. Everything lives under the
# scratch dir, which teardown removes.
#
# Marker lines are always built with printf here, never written out
# literally: run.sh reads every line of this file that starts with the
# marker prefix as one of this test's own markers.
work="${TEST_SCRATCH_DIR}/harness"
rm -rf "${work}"
mkdir -p "${work}/markers"

# marker_file <name> [<marker>...] — a test file carrying those markers.
marker_file() {
  local f="${work}/markers/$1.sh" m
  shift
  printf '#!/usr/bin/env bash\n' > "${f}"
  for m in "$@"; do
    printf '# itest: %s\n' "${m}" >> "${f}"
  done
  printf '%s\n' "${f}"
}

# ===========================================
# Markers: class, exemption reason, malformed-marker detection.
# ===========================================
info "classifying markers"
check_markers() {  # <want-class> <want-reason> <want-problem-substring> <marker>...
  local want_class="$1" want_reason="$2" want_problem="$3" f problem
  shift 3
  f=$(marker_file "case" "$@")
  assert_equals "${want_class}" "$(itest_k8s_class "${f}")" "class of [$*]"
  assert_equals "${want_reason}" "$(itest_exemption "${f}")" "exemption reason of [$*]"
  problem=$(itest_marker_problem "${f}")
  if [ -z "${want_problem}" ]; then
    assert_equals "" "${problem}" "[$*] should be well-formed"
  else
    assert_contains "${problem}" "${want_problem}" "[$*] should be malformed"
  fi
}
check_markers pending "" ""
check_markers host-only "" "" no-docker
check_markers k8s "" "" k8s
check_markers k8s "" "" no-docker k8s
check_markers devcontainer-only "a host socket" "" "devcontainer-only: a host socket"
check_markers devcontainer-only "reason" "" "devcontainer-only:reason  "
check_markers devcontainer-only "why" "" no-docker "devcontainer-only: why"
check_markers pending "" "needs a reason" devcontainer-only
check_markers pending "" "needs a reason" "devcontainer-only:   "
check_markers pending "" "unknown marker" k8
check_markers pending "" "unknown marker" "devcontainer-only : spaced"
check_markers k8s "why" "contradict" k8s "devcontainer-only: why"

# ===========================================
# state.json reader: fleet-scoped and bare refs, empty and k8s ids.
# ===========================================
info "reading container ids from state.json"
home="${work}/home"
mkdir -p "${home}/.fleet"
cat > "${home}/.fleet/state.json" <<EOF
{
  "fleets": {
    "zeta": {"name": "zeta", "instances": [{"name": "alpha", "container_id": "z-alpha"}]},
    "${FIXTURE_REPO_NAME}": {"name": "${FIXTURE_REPO_NAME}", "instances": [
      {"name": "creating", "container_id": ""},
      {"name": "alpha", "container_id": "0123456789ab"},
      {"name": "beta", "container_id": "k8s:minikube/fleet-it/beta@g2"}
    ]}
  }
}
EOF
in_home() { HOME="${home}" "$@"; }
assert_equals "0123456789ab" "$(in_home itest_container_id alpha)" "a bare ref is the fixture fleet's instance"
assert_equals "0123456789ab" "$(in_home itest_container_id "${FIXTURE_REPO_NAME}/alpha")" "fleet-scoped ref"
assert_equals "z-alpha" "$(in_home itest_container_id zeta/alpha)" "the same instance name in another fleet"
assert_equals "k8s:minikube/fleet-it/beta@g2" "$(in_home itest_container_id beta)" "a k8s container id"
assert_equals "" "$(in_home itest_container_id creating)" "an instance with no container yet"
assert_equals "" "$(in_home itest_container_id missing)" "an unknown instance"
assert_equals "0123456789ab" "$(in_home itest_container_id)" "no ref: the first instance that has an id"
assert_equals "0123456789ab k8s:minikube/fleet-it/beta@g2 z-alpha" \
  "$(in_home itest_container_ids | tr '\n' ' ' | sed 's/ $//')" "every recorded id"
assert_equals "" "$(HOME="${work}/no-such-home" itest_container_id alpha)" "no state.json"

assert_equals "${HOME}/.fleet/workspaces/${FIXTURE_REPO_NAME}/alpha/${FIXTURE_REPO_NAME}" \
  "$(itest_host_workspace_dir alpha)" "host workspace dir"
assert_equals "${HOME}/.fleet/workspaces/scratch/beta/.control" \
  "$(itest_host_control_dir scratch/beta)" "host control dir"
assert_equals "${HOME}/.fleet/workspaces/${FIXTURE_REPO_NAME}/.config/gh" \
  "$(itest_share_dir "${FIXTURE_REPO_NAME}" .config/gh)" "share dir subpath"
assert_equals "${HOME}/.fleet/workspaces/${FIXTURE_REPO_NAME}" \
  "$(itest_share_dir "${FIXTURE_REPO_NAME}")" "share dir root"

# ===========================================
# FLEET_ITEST_BACKEND dispatch.
# ===========================================
# in_backend <backend> <script> [VAR=value...] — run <script> in a fresh bash
# that sourced common.sh with FLEET_ITEST_BACKEND=<backend> (and no results
# file, so nothing it does can add a row to this run); prints its output and rc.
in_backend() {
  local backend="$1" script="$2" out rc=0
  shift 2
  out=$(env FLEET_ITEST_RESULTS= FLEET_ITEST_BACKEND="${backend}" "$@" \
    bash -c "source \"\$1\"; ${script}" _ "${INTEGRATION_DIR}/common.sh" 2>&1) || rc=$?
  printf '%s\nrc=%s\n' "${out}" "${rc}"
}

info "an unknown backend is fatal"
out=$(in_backend nomad 'echo unreachable')
assert_contains "${out}" "unknown FLEET_ITEST_BACKEND=nomad" "unknown backend message"
assert_contains "${out}" "rc=2" "unknown backend exit code"
assert_not_contains "${out}" "unreachable" "sourcing must stop on an unknown backend"

info "devcontainer helpers dispatch to backends/devcontainer.sh"
out=$(in_backend devcontainer 'itest_backend_is devcontainer && itest_share_sync any && echo ok')
assert_contains "${out}" "ok" "devcontainer dispatch"
assert_contains "${out}" "rc=0" "devcontainer dispatch exit code"

info "kubernetes control-dir helpers look in the pod's /fleet-mounts/control through fleet exec"
stand_in="${work}/fleet-stand-in.sh"
printf '#!/bin/sh\nprintf "%%s|" "$@"; echo\n' > "${stand_in}"
chmod +x "${stand_in}"

info "kubernetes share sync waits through fleet sync wait, and ends the test when it does not converge"
recorder="${work}/fleet-recorder.sh"
printf '#!/bin/sh\nprintf "%%s|" "$@" > "%s"\n' "${work}/argv" > "${recorder}"
chmod +x "${recorder}"
out=$(in_backend kubernetes 'itest_share_sync myfleet && echo synced' FLEET_BIN="${recorder}")
assert_equals "sync|wait|myfleet|--timeout|120s|" "$(cat "${work}/argv")" "itest_share_sync runs fleet sync wait"
assert_contains "${out}" "synced" "a converged share sync"
assert_contains "${out}" "rc=0" "a converged share sync's exit code"
behind="${work}/fleet-behind.sh"
printf '#!/bin/sh\necho "  host .claude: 3 to send" >&2; echo "Error: not in sync within 120s" >&2; exit 1\n' > "${behind}"
chmod +x "${behind}"
out=$(in_backend kubernetes 'itest_share_sync myfleet; echo unreachable' FLEET_BIN="${behind}")
assert_contains "${out}" "fleet sync wait myfleet:" "a share sync that does not converge fails, naming the fleet"
assert_contains "${out}" "host .claude: 3 to send" "it names what was behind"
assert_contains "${out}" "rc=1" "its exit code"
assert_not_contains "${out}" "unreachable" "the test stops there"
# In a condition set -e is off, so only an exit stops the test there.
out=$(in_backend kubernetes 'if itest_share_sync myfleet; then echo synced; fi; echo unreachable' FLEET_BIN="${behind}")
assert_not_contains "${out}" "unreachable" "the test stops at a share sync inside a condition"
out=$(in_backend kubernetes 'itest_control_test x/y -S ssh-agent.sock; itest_control_read x/y claude-status.json; itest_control_mode x/y mcp.sock' FLEET_BIN="${stand_in}")
assert_contains "${out}" "exec|x/y|--|test|-S|/fleet-mounts/control/ssh-agent.sock|" "itest_control_test in the pod"
assert_contains "${out}" "exec|x/y|--|cat|/fleet-mounts/control/claude-status.json|" "itest_control_read in the pod"
assert_contains "${out}" "exec|x/y|--|stat|-c|%a|/fleet-mounts/control/mcp.sock|" "itest_control_mode in the pod"
out=$(in_backend kubernetes '_be_teardown; echo after-teardown' FLEET_BIN=/nonexistent/fleet)
assert_contains "${out}" "after-teardown" "the kubernetes teardown must never exit"

info "kubernetes container ids and instance names"
out=$(in_backend kubernetes '_be_cid_parts "k8s:mk-abc234/fleet-itest/fi-itest-fleet-alpha-abcdefgh@g3"')
assert_contains "${out}" "mk-abc234 fleet-itest fi-itest-fleet-alpha-abcdefgh" "a container id's connection, namespace and StatefulSet"
out=$(in_backend kubernetes 'itest_container_status c0ffee')
assert_contains "${out}" "absent" "a docker id is no StatefulSet"
# The StatefulSet names of names.Instance (internal/k8s/names): each part
# slugged and cut to fit, the key last.
out=$(in_backend kubernetes '_be_sts_prefix itest-fleet/alpha; _be_sts_prefix "My_Fleet.Name-very-long-indeed/An Instance With A Very Long Name Indeed"; _be_sts_prefix itest-fleet/x--y')
assert_contains "${out}" "fi-itest-fleet-alpha-" "a short name"
assert_contains "${out}" "fi-my-fleet-name-ve-an-instance-with-a-very-" "long names are cut as names.Instance cuts them"
assert_contains "${out}" "fi-itest-fleet-x-y-" "runs of other characters are one dash"
assert_equals "⬡" "$(in_backend devcontainer itest_row_target | head -n 1)" "a devcontainer row's target"
assert_equals "☸ itest" "$(in_backend kubernetes itest_row_target | head -n 1)" "a cluster row's target"

# A stand-in kubectl that knows one context, fake-ctx; FAKE_KUBE_DOWN=1 makes
# the API server unreachable.
mkdir -p "${work}/bin"
cat > "${work}/bin/kubectl" <<'EOF'
#!/bin/sh
case "$*" in
  "config current-context") echo fake-ctx ;;
  "config get-contexts -o name") printf 'fake-ctx\nother\n' ;;
  *"get --raw=/readyz") [ -z "${FAKE_KUBE_DOWN:-}" ] || exit 1; echo ok ;;
  *) echo "unexpected: kubectl $*" >&2; exit 1 ;;
esac
EOF
chmod +x "${work}/bin/kubectl"

info "kubernetes preflight"
fake_path="${work}/bin:${PATH}"
pod_var="FLEET_POD_BINARY_$(in_backend kubernetes _be_arch | head -n 1 | tr '[:lower:]' '[:upper:]')"
: > "${work}/fleet-pod"
out=$(in_backend kubernetes 'unset FLEET_ITEST_CLUSTER; _be_preflight' PATH="${fake_path}" "${pod_var}=")
assert_contains "${out}" "${pod_var} names no file" "the preflight wants the pod build"
assert_contains "${out}" "rc=1" "no pod build fails the preflight"
out=$(in_backend kubernetes 'unset FLEET_ITEST_CLUSTER; _be_preflight && echo "cluster=${FLEET_ITEST_CLUSTER}"' PATH="${fake_path}" "${pod_var}=${work}/fleet-pod")
assert_contains "${out}" "cluster=fake-ctx" "FLEET_ITEST_CLUSTER defaults to the current context"
out=$(in_backend kubernetes 'FLEET_ITEST_CLUSTER=nope; _be_preflight' PATH="${fake_path}")
assert_contains "${out}" "no kubeconfig context named nope" "an unknown context"
assert_contains "${out}" "rc=1" "an unknown context fails the preflight"
out=$(in_backend kubernetes 'unset FLEET_ITEST_CLUSTER; _be_preflight' PATH="${fake_path}" FAKE_KUBE_DOWN=1)
assert_contains "${out}" "cluster fake-ctx is not ready" "an unreachable cluster"
assert_contains "${out}" "rc=1" "an unreachable cluster fails the preflight"

# ===========================================
# run.sh on a copy of the harness with four stand-in tests: k8s-marked,
# devcontainer-only, host-only and malformed. Each records that it ran.
# ===========================================
h="${work}/integration"
mkdir -p "${h}/tests" "${work}/ran"
cp "${INTEGRATION_DIR}/run.sh" "${INTEGRATION_DIR}/common.sh" "${h}/"
cp -R "${INTEGRATION_DIR}/backends" "${h}/"
stand_in() {  # <name> [<marker>...]
  local f="${h}/tests/$1.sh" m
  printf '#!/usr/bin/env bash\n# Description: stand-in %s\n' "$1" > "${f}"
  shift
  for m in no-docker "$@"; do
    printf '# itest: %s\n' "${m}" >> "${f}"
  done
  printf '%s\n' \
    'set -euo pipefail' \
    'source "$(dirname "$0")/../common.sh"' \
    'itest_begin' \
    ': > "${RAN_DIR}/$(basename "$0" .sh)"' \
    'pass ok' >> "${f}"
}
stand_in 001_k8s k8s
stand_in 002_exempt "devcontainer-only: needs a host socket"
stand_in 003_host
stand_in 004_bad k8

# nested_run <backend> [VAR=value...] — run.sh on the copy, isolated from this
# run's HOME, scratch dir, tmux session, selection and step summary.
nested_run() {
  local backend="$1" out rc=0
  shift
  rm -rf "${work}/ran" "${work}/summary.md"
  mkdir -p "${work}/ran"
  out=$(cd "${h}" && env -u FLEET_ITEST_RANGE -u FLEET_ITEST_SKIP -u FLEET_ITEST_CLUSTER \
    HOME="${work}/nested-home" TEST_SCRATCH_DIR="${work}/nested-scratch" \
    TUI_SESSION="fleet-itest-harness-$$" RAN_DIR="${work}/ran" PATH="${fake_path}" FLEET_BIN="${FLEET_BIN}" \
    GITHUB_STEP_SUMMARY="${work}/summary.md" FLEET_ITEST_NO_DOCKER=1 "${pod_var}=${work}/fleet-pod" \
    FLEET_ITEST_BACKEND="${backend}" "$@" ./run.sh 2>&1) || rc=$?
  printf '%s\nrc=%s\nran:[%s]\n' "${out}" "${rc}" "$(ls "${work}/ran" | tr '\n' ' ' | sed 's/ $//')"
}

info "run.sh on kubernetes runs only the k8s tests"
out=$(nested_run kubernetes)
printf '%s\n' "${out}"
assert_contains "${out}" "Backend: kubernetes (context fake-ctx)" "backend banner"
assert_contains "${out}" "ran:[001_k8s]" "only the k8s-marked test runs"
assert_contains "${out}" "SKIP 002_exempt (devcontainer-only: needs a host socket)" "the exemption is a skip with its reason"
assert_contains "${out}" "FAIL 004_bad (malformed itest marker: unknown marker" "a malformed marker fails without running"
assert_contains "${out}" "Summary: 1 passed, 1 failed, 1 skipped" "summary counts"
assert_contains "${out}" "Backend kubernetes; k8s parity of 4 test(s): 1 marked k8s, 1 devcontainer-only, 0 not ported yet, 2 host-only" "parity line"
assert_contains "${out}" "devcontainer-only 002_exempt: needs a host socket" "the exemption is listed"
assert_contains "${out}" "rc=1" "a malformed marker fails the run"
summary=$(cat "${work}/summary.md")
assert_contains "${summary}" "k8s parity of 4 test(s)" "step summary parity line"
assert_contains "${summary}" "<summary>devcontainer-only exemptions (1)</summary>" "step summary exemption list"

info "run.sh on devcontainer runs the exemption too"
out=$(nested_run devcontainer)
printf '%s\n' "${out}"
assert_contains "${out}" "ran:[001_k8s 002_exempt 003_host]" "every well-formed test runs"
assert_contains "${out}" "FAIL 004_bad (malformed itest marker" "a malformed marker fails on devcontainer too"
assert_contains "${out}" "Backend devcontainer; k8s parity of 4 test(s)" "parity line on devcontainer"
assert_contains "${out}" "rc=1" "devcontainer run exit code"

info "run.sh stops before any test when the cluster does not answer"
out=$(nested_run kubernetes FAKE_KUBE_DOWN=1)
assert_contains "${out}" "cluster fake-ctx is not ready" "preflight failure message"
assert_contains "${out}" "rc=2" "preflight failure exit code"
assert_contains "${out}" "ran:[]" "no test runs after a failed preflight"

pass "itest harness: markers, state reader, backend dispatch and kubernetes selection"
