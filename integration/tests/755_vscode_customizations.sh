#!/usr/bin/env bash
# Description: a devcontainer.json with customizations.vscode, postAttachCommand, forwardPorts and portsAttributes: postAttachCommand runs once per create and per start (never per exec), fleet does not forward forwardPorts itself, and the merged config an editor attach applies carries the extensions, settings and port attributes.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
source "$(dirname "$0")/../net/net.sh"
forward_pid=""
itest_cleanup() {
  [ -z "${forward_pid}" ] || kill "${forward_pid}" 2>/dev/null || true
}
itest_begin

# The fixture's postStartCommand serves a banner on 17390, the port it lists in
# forwardPorts; postAttachCommand appends a line to ~/.fleet-postattach.log.
FIXTURE_SRC="${INTEGRATION_DIR}/fixture-vscode" setup_test
fleet_up alpha
inst="${FIXTURE_REPO_NAME}/alpha"

# attach_runs — how many times postAttachCommand has run in the instance.
attach_runs() {
  "${FLEET_BIN}" exec "${inst}" -- sh -c 'cat "$HOME/.fleet-postattach.log" 2>/dev/null | wc -l' | tr -d '[:space:]'
}
# banner — what the instance's forwardPorts service answers, from inside it.
banner() {
  "${FLEET_BIN}" exec "${inst}" -- perl -MIO::Socket::INET \
    -e 'my $c = IO::Socket::INET->new(PeerAddr => "127.0.0.1:17390", Timeout => 5) or die "connect: $!\n"; print scalar <$c>'
}

info "postAttachCommand ran once at create"
assert_equals "1" "$(attach_runs)" "postAttachCommand runs after create"

info "fleet exec does not run it"
"${FLEET_BIN}" exec "${inst}" -- true
"${FLEET_BIN}" exec "${inst}" -- true
assert_equals "1" "$(attach_runs)" "postAttachCommand is not an exec hook"

info "the forwardPorts service runs in the instance, and fleet does not forward it"
assert_equals "fleet-itest-forwarded" "$(banner)" "the service listed in forwardPorts"
# An editor attach forwards forwardPorts while it is connected; fleet itself
# leaves the port alone on this host.
if itest_net_accepts 17390; then
  fail "something on the host accepts on 17390: fleet should not forward forwardPorts itself"
fi

info "an explicit fleet port-forward reaches it"
local_port=$(itest_net_free_port)
"${FLEET_BIN}" port-forward add "${inst}" "${local_port}:17390" >/dev/null 2>&1 &
forward_pid=$!
itest_net_wait_accepts "${local_port}" 30 || fail "the explicit forward never listened"
got=$(python3 -c 'import socket, sys; s = socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=15); print(s.makefile().readline().strip())' "${local_port}")
assert_equals "fleet-itest-forwarded" "${got}" "the forwardPorts service through fleet port-forward"
kill "${forward_pid}" 2>/dev/null || true
wait "${forward_pid}" 2>/dev/null || true
forward_pid=""

info "the merged config an editor attach applies carries the customizations"
if itest_backend_is devcontainer; then
  # VS Code's Dev Containers extension reads it from the container label the
  # devcontainer CLI stamped at up; it installs the extensions, applies the
  # settings and port attributes, and runs postAttachCommand on each attach.
  # (On a cluster, fleet's ssh-server applies them over Remote-SSH, P7.)
  metadata=$(itest_container_label "$(itest_container_id "${inst}")" devcontainer.metadata)
  [ -n "${metadata}" ] || fail "the container has no devcontainer.metadata label"
  problems=$(printf '%s' "${metadata}" | perl -MJSON::PP -e '
    my $layers = decode_json(do { local $/; <STDIN> });
    my %seen;
    for my $l (@$layers) {
      my $vs = $l->{customizations}{vscode} || {};
      $seen{extension} = 1 if grep { $_ eq "fleet-itest.fixture-extension" } @{ $vs->{extensions} || [] };
      $seen{setting} = 1 if ($vs->{settings}{"fleetItest.fromFixture"} // "") eq "customized";
      $seen{postAttachCommand} = 1 if ($l->{postAttachCommand} // "") =~ /fleet-postattach/;
      $seen{forwardPorts} = 1 if grep { $_ eq "17390" } @{ $l->{forwardPorts} || [] };
      $seen{portsAttributes} = 1 if ($l->{portsAttributes}{17390}{label} // "") eq "Itest Forwarded";
      $seen{otherPortsAttributes} = 1 if ($l->{otherPortsAttributes}{onAutoForward} // "") eq "ignore";
    }
    print join(" ", grep { !$seen{$_} } qw(extension setting postAttachCommand forwardPorts portsAttributes otherPortsAttributes));
  ') || fail "devcontainer.metadata is not JSON: ${metadata}"
  [ -z "${problems}" ] || fail "devcontainer.metadata lacks: ${problems} (${metadata})"
fi

info "a stop and start runs postAttachCommand again (and postStart restarts the service)"
"${FLEET_BIN}" stop "${inst}"
"${FLEET_BIN}" start "${inst}"
assert_equals "2" "$(attach_runs)" "postAttachCommand runs after each start"
deadline=$(( $(date +%s) + $(_scale_timeout 20) ))
until [ "$(banner 2>/dev/null)" = "fleet-itest-forwarded" ]; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "the forwardPorts service did not come back after the start"
  sleep 0.5
done

pass "VS Code customizations, postAttachCommand and forwardPorts"
