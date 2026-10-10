# shellcheck shell=bash
# Devcontainer (Docker) side of the backend-neutral helpers in common.sh.
#
# common.sh sources this file when FLEET_ITEST_BACKEND=devcontainer (the
# default). Tests never call the _be_* functions here directly: they call the
# itest_* / assert_* wrappers in common.sh. kubernetes.sh defines the same set
# of functions; keep the two in step.
#
# Instance refs are <fleet>/<instance>. Containers are named by the
# container_id fleet recorded in state.json.

# _be_preflight — backend checks run.sh makes before the first test. The
# docker + devcontainer CLI checks run.sh already makes cover this backend.
_be_preflight() { :; }

# _be_setup — backend setup at the end of setup_test. Nothing to do: fleet
# provisions on the local Docker daemon by default.
_be_setup() { :; }

# _be_teardown — backend backstop teardown_test runs after `fleet destroy`.
_be_teardown() {
  command -v docker >/dev/null 2>&1 || return 0

  # Remove any devcontainer created from the fixture workspace. The devcontainer
  # CLI labels containers with the workspace folder path.
  local stale
  stale=$(docker ps -aq --filter "label=devcontainer.local_folder=${HOME}/.fleet/workspaces" 2>/dev/null || true)
  if [ -n "${stale}" ]; then
    docker rm -f ${stale} >/dev/null 2>&1 || true
  fi

  # Sweep any shared buildkit servers (named fleet-<fleet>-buildkit). They carry
  # --restart unless-stopped, so a test that crashed before `fleet destroy` ran
  # must not leave one to auto-restart across runs. The per-fleet `destroy`
  # already removes them on the happy path; this is the crash-recovery backstop.
  local bk
  bk=$(docker ps -a --format '{{.Names}}' 2>/dev/null | grep -E '^fleet-.*-buildkit$' || true)
  if [ -n "${bk}" ]; then
    docker rm -f ${bk} >/dev/null 2>&1 || true
  fi
}

# --- containers ---

_be_container_status() {  # <container_id>
  local status
  status=$(docker inspect -f '{{.State.Status}}' "$1" 2>/dev/null) || status="absent"
  printf '%s\n' "${status}"
}

_be_container_exists() {  # <container_id>
  docker inspect "$1" >/dev/null 2>&1
}

# The devcontainer CLI matches a container to its workspace by the
# devcontainer.local_folder label; that is the identity the stale-container
# prune (#51) queries.
_be_container_belongs_to() {  # <container_id> <ref>
  local want got
  want=$(itest_host_workspace_dir "$2")
  got=$(docker inspect --format '{{ index .Config.Labels "devcontainer.local_folder" }}' "$1" 2>/dev/null || true)
  [ "${got}" = "${want}" ] && return 0
  printf 'container %s: devcontainer.local_folder=[%s], want [%s]\n' "$1" "${got}" "${want}" >&2
  return 1
}

_be_container_leftovers() {  # <ref>
  docker ps -aq --filter "label=devcontainer.local_folder=$(itest_host_workspace_dir "$1")" 2>/dev/null || true
}

_be_container_logs() {  # <container_id>
  docker logs "$1" 2>&1
}

_be_container_stop() {  # <container_id>
  docker stop "$1" >/dev/null
}

_be_container_exec_as() {  # <container_id> <user> <cmd> [args...]
  local cid="$1" user="$2"
  shift 2
  docker exec -u "${user}" "${cid}" "$@"
}

# --- workspace: the instance's checkout, which is the host checkout itself ---

_be_workspace_exists() {  # <ref>
  [ -e "$(itest_host_workspace_dir "$1")" ]
}

_be_workspace_test() {  # <ref> <test-op> <relpath>
  test "$2" "$(itest_host_workspace_dir "$1")/$3"
}

_be_workspace_read() {  # <ref> <relpath>
  cat "$(itest_host_workspace_dir "$1")/$2"
}

_be_workspace_write() {  # <ref> <relpath> <content>
  printf '%s\n' "$3" > "$(itest_host_workspace_dir "$1")/$2"
}

_be_workspace_git() {  # <ref> <git-args...>
  local ws
  ws=$(itest_host_workspace_dir "$1")
  shift
  git -C "${ws}" "$@"
}

# --- control dir: the host dir bind-mounted at /fleet-mounts/control ---

_be_control_test() {  # <ref> <test-op> <name>
  test "$2" "$(itest_host_control_dir "$1")/$3"
}

_be_control_read() {  # <ref> <name>
  cat "$(itest_host_control_dir "$1")/$2"
}

_be_control_mode() {  # <ref> <name>
  file_mode "$(itest_host_control_dir "$1")/$2"
}

# --- shared fleet mounts ---

# The share tree is bind-mounted into every instance: writes on either side are
# visible on the other at once.
_be_share_sync() { :; }  # <fleet>
