#!/usr/bin/env bash
# itest: k8s
# Description: with the fleet's deb cache on, every instance's apt goes through the fleet's one apt-cacher-ng, so a package one instance downloaded is a cache hit for the next; the cache comes back when an instance starts, the fleet options' Delete cache empties it, and its data outlives a fleet destroy. On a cluster the cache is the fleet's Service there, and apt uses it only while it answers (Proxy-Auto-Detect through fleet net-probe), going direct when it does not.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; _itest_reclaim_cache_dirs; }
itest_begin

# _itest_reclaim_cache_dirs — apt-cacher-ng writes the cache dir as its own
# uid; hand it back to the test user (with the cache's own image, so nothing is
# pulled) so teardown can wipe ~/.fleet.
_itest_reclaim_cache_dirs() {
  itest_backend_is devcontainer || return 0
  local dir
  dir=$(itest_share_dir "${FIXTURE_REPO_NAME}" .aptcache)
  [ -d "${dir}" ] || return 0
  docker run --rm --entrypoint chown -v "${dir}:/reclaim" sameersbn/apt-cacher-ng:latest \
    -R "$(id -u):$(id -g)" /reclaim >/dev/null 2>&1 || true
}

setup_test
cat > "${HOME}/.fleet/state.json" <<EOF
{
  "fleets": {
    "${FIXTURE_REPO_NAME}": {
      "name": "${FIXTURE_REPO_NAME}",
      "remote": "${FIXTURE_REPO_URL}",
      "settings": { "debCacheServer": true },
      "instances": []
    }
  }
}
EOF

alpha="${FIXTURE_REPO_NAME}/alpha"
beta="${FIXTURE_REPO_NAME}/beta"

# apt_proxy <ref> — the http proxy the instance's apt uses now: its fixed
# proxy, or what its proxy detector answers (the cache's URL, or DIRECT).
apt_proxy() {
  "${FLEET_BIN}" exec "$1" -- sh -c 'eval "$(apt-config shell P Acquire::http::Proxy D Acquire::http::Proxy-Auto-Detect)"
    if [ -n "${D:-}" ]; then "$D" http://deb.debian.org/debian/; else printf "%s\n" "${P:-}"; fi'
}
# apt_log <ref> <package> — where apt_install keeps apt's output (with its
# Debug::Acquire::http lines) of installing <package> in <ref>.
apt_log() { printf '%s/apt-%s-%s.log' "${TEST_SCRATCH_DIR}" "${1//\//_}" "$2"; }
# apt_install <ref> <package> — apt-get update + install, failing the test with
# apt's output when either fails. apt runs with Debug::Acquire::http, so a
# failed cache-hit check can show how it reached each host (apt_debug).
apt_install() {
  local log
  log=$(apt_log "$1" "$2")
  mkdir -p "$(dirname "${log}")"
  "${FLEET_BIN}" exec "$1" -- sh -c 'sudo apt-get -o Debug::Acquire::http=true update -qq && sudo apt-get -o Debug::Acquire::http=true install -y -qq --no-install-recommends "$1"' sh "$2" > "${log}" 2>&1 \
    || fail "apt-get install $2 in $1 failed: $(grep -v -E '^(GET |Host: |User-Agent: |Accept|Cache-Control: |If-|Answer for: |HTTP/1|[A-Z][A-Za-z-]+: |Connection: |Range: |$)' "${log}" | tail -40)"
}
# apt_debug <ref> <package> — what the detector answered apt when it installed
# <package> in <ref>, and the requests it sent (through a proxy, a GET names
# the whole URL; direct, only its path); then what the detector answers now,
# and how long it took.
apt_debug() {
  info "apt in $1 installing $2 (Debug::Acquire::http):"
  grep -E "auto detect|Using auto proxy|[Pp]roxy|^GET " "$(apt_log "$1" "$2")" | head -40 >&2 || true
  info "$1's detector now: $("${FLEET_BIN}" exec "$1" -- sh -c 's=$(date +%s%N); a=$(/etc/apt/fleet-proxy-detect http://deb.debian.org/debian/); e=$(date +%s%N); echo "${a} after $(( (e - s) / 1000000 ))ms"' 2>&1)"
}
# cache_stats <ref> — the cache's own report of today's requests (period,
# hits, misses, total, then the same in bytes), read from inside <ref> over
# the fleet network.
cache_stats() {
  "${FLEET_BIN}" exec "$1" -- sh -c 'curl -fsS "$1/acng-report.html?doCount=Count+Data"' sh "${proxy}" \
    | grep -o '<td class="colcont">[^<]*</td>' | sed 's/<[^>]*>//g' | head -7 | tr '\n' '|' || true
}
# cache_hits <ref> — request hits so far in the cache's own report; empty when
# the report has none (a cache that has served nothing yet).
cache_hits() { cache_stats "$1" | cut -d'|' -f2 | sed -n 's/^\([0-9]*\).*/\1/p'; }
# wait_hits_above <ref> <n> — the hit count once it is above <n>, waiting
# up to a minute: the cache writes a client's requests to the log its report
# counts only once that client has disconnected, which under load comes some
# time after apt is done.
wait_hits_above() {
  local hits deadline=$(( $(date +%s) + $(_scale_timeout 60) )) start
  start=$(date +%s)
  while :; do
    hits=$(cache_hits "$1")
    if [ "${hits:-0}" -gt "$2" ] || [ "$(date +%s)" -ge "${deadline}" ]; then break; fi
    sleep 2
  done
  [ "$(date +%s)" -le "$(( start + 1 ))" ] || info "the cache's report counted the hits $(( $(date +%s) - start ))s after apt was done" >&2
  printf '%s\n' "${hits}"
}

info "fleet up alpha + beta (deb cache on)"
fleet_up alpha
fleet_up beta
if itest_backend_is kubernetes; then
  # The cluster brings the cache's server up on its own time: until it
  # answers, the instances' apt goes direct.
  itest_wait_service "${FIXTURE_REPO_NAME}" debcache running 240
fi

proxy=$(apt_proxy "${alpha}")
info "alpha's apt proxy: ${proxy}"
[ -n "${proxy}" ] && [ "${proxy}" != DIRECT ] || fail "alpha's apt has no http proxy configured (${proxy:-none})"
assert_equals "${proxy}" "$(apt_proxy "${beta}")" "both instances should use the fleet's one cache"
if itest_backend_is devcontainer; then
  # Docker reaches the cache by its container name on the fleet's network.
  assert_equals "http://fleet-${FIXTURE_REPO_NAME}-aptcache:3142" "${proxy}" "the proxy should name the fleet's cache container"
else
  # A cluster's instances reach it by its Service in their namespace, and
  # ask fleet net-probe first.
  assert_equals "http://$(itest_k8s_fleet_object "${FIXTURE_REPO_NAME}" services aptcache):3142" "${proxy}" "the proxy should name the fleet's cache Service"
  detector=$("${FLEET_BIN}" exec "${alpha}" -- sh -c 'eval "$(apt-config shell D Acquire::http::Proxy-Auto-Detect)"; printf "%s" "${D:-}"')
  assert_equals "/etc/apt/fleet-proxy-detect" "${detector}" "apt's proxy detector"
  assert_contains "$("${FLEET_BIN}" exec "${alpha}" -- cat "${detector}")" "/usr/bin/fleet net-probe '${proxy}'" "the detector asks fleet net-probe"
fi

info "alpha installs a package through the cache"
apt_install "${alpha}" hello
assert_equals "Hello, world!" "$("${FLEET_BIN}" exec "${alpha}" -- hello)" "the package should install and run in alpha"
hits_before=$(cache_hits "${alpha}")
info "cache hits after alpha: ${hits_before:-?}"
[ -n "${hits_before}" ] || fail "could not read the cache's hit count from ${proxy}/acng-report.html"

info "beta installs the same package, served from the cache"
apt_install "${beta}" hello
assert_equals "Hello, world!" "$("${FLEET_BIN}" exec "${beta}" -- hello)" "the package should install and run in beta"
hits_after=$(wait_hits_above "${beta}" "${hits_before}")
info "cache hits after beta: ${hits_after:-?}"
if [ "${hits_after:-0}" -le "${hits_before}" ]; then
  info "the cache's report: $(cache_stats "${beta}")"
  itest_backend_is kubernetes && { apt_debug "${alpha}" hello; apt_debug "${beta}" hello; }
  fail "beta's download should be cache hits (hits ${hits_before} -> ${hits_after:-?})"
fi

# cached <package> — the package's .deb is in the fleet's cache on the daemon
# host (docker: the bind-mounted .aptcache dir).
cache_dir=$(itest_share_dir "${FIXTURE_REPO_NAME}" .aptcache)
cached() { [ -n "$(find "${cache_dir}" -name "$1_*.deb" -print -quit 2>/dev/null)" ]; }
cache_name="fleet-${FIXTURE_REPO_NAME}-aptcache"

if itest_backend_is devcontainer; then
  cached hello || fail "the package should be stored in the fleet's cache dir ${cache_dir}"

  # Revival on start: a cache container removed behind fleet's back (a host
  # reboot, a docker prune) comes back when an instance of the fleet starts.
  # Clusters run the cache as their own workload, which fleet does not
  # resurrect on start.
  info "removing the cache container, then fleet stop + start alpha"
  docker rm -f "${cache_name}" >/dev/null
  "${FLEET_BIN}" stop "${alpha}" >/dev/null
  "${FLEET_BIN}" start "${alpha}" >/dev/null
  assert_container_running "${cache_name}" "starting an instance should bring the fleet's cache back"
  apt_install "${alpha}" sl
  cached sl || fail "apt in the restarted instance should go through the revived cache"
fi

if itest_backend_is kubernetes; then
  # Its server gone behind fleet's back, the cluster's cache does not
  # answer: apt goes direct instead of failing, and the instance installs.
  info "removing the cache's server: apt goes direct"
  itest_service_remove "${FIXTURE_REPO_NAME}" debcache
  itest_wait_service "${FIXTURE_REPO_NAME}" debcache absent 60
  assert_equals "DIRECT" "$(apt_proxy "${alpha}")" "with the cache down, the detector should send apt direct"
  apt_install "${alpha}" sl
  "${FLEET_BIN}" exec "${alpha}" -- test -x /usr/games/sl || fail "sl should install directly while the cache is down"
fi

# Delete cache in the fleet options empties the cache and brings an empty one
# back, which the instances keep using.
info "Delete cache from the TUI's fleet options"
tui_spawn
tui_wait_for "${FIXTURE_REPO_NAME}" 15 || fail "the TUI never rendered the fleet"
tui_delete_fleet_cache "Deb package cache" "Deb cache cleared"
tui_kill
if itest_backend_is devcontainer; then
  ! cached hello || fail "Delete cache should remove the cached packages from ${cache_dir}"
  assert_container_running "${cache_name}" "Delete cache should bring the cache server back"
else
  itest_wait_service "${FIXTURE_REPO_NAME}" debcache running 120
fi
assert_equals "${proxy}" "$(apt_proxy "${beta}")" "the instances should keep the same cache after a delete"
hits_before=$(cache_hits "${beta}")
apt_install "${beta}" figlet
apt_install "${alpha}" figlet
hits_after=$(wait_hits_above "${alpha}" "${hits_before:-0}")
if [ "${hits_after:-0}" -le "${hits_before:-0}" ]; then
  info "the cache's report: $(cache_stats "${alpha}")"
  itest_backend_is kubernetes && { apt_debug "${beta}" figlet; apt_debug "${alpha}" figlet; }
  fail "alpha's download of what beta fetched through the emptied cache should be cache hits (hits ${hits_before:-?} -> ${hits_after:-?})"
fi
if itest_backend_is devcontainer; then
  cached figlet || fail "apt after Delete cache should go through the emptied cache"

  # Destroy removes the cache container but keeps its data for the next fleet
  # of the same name.
  info "fleet destroy ${FIXTURE_REPO_NAME}"
  "${FLEET_BIN}" destroy "${FIXTURE_REPO_NAME}" >/dev/null
  assert_container_absent "${cache_name}" "destroy should remove the fleet's cache container"
  cached figlet || fail "destroy should keep the cache's data in ${cache_dir}"
fi

pass "deb cache shared across instances, revived on start (direct while down on a cluster), emptied by Delete cache, data kept on destroy"
