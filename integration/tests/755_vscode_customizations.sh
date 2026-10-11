#!/usr/bin/env bash
# itest: k8s
# Description: a devcontainer.json with customizations.vscode, postAttachCommand, forwardPorts and portsAttributes: postAttachCommand runs once per create and per start (never per exec), fleet does not forward forwardPorts itself, the merged config an editor attach applies carries the extensions, settings and port attributes, and over Remote-SSH fleet's ssh-server applies them once VS Code's server appears (each extension installed once, machine settings with remote.portsAttributes) and runs postAttachCommand once per VS Code connection, reporting in the creation log.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
source "$(dirname "$0")/../net/net.sh"
source "$(dirname "$0")/../editor/editor.sh"
forward_pid=""
itest_cleanup() {
  [ -z "${forward_pid}" ] || kill "${forward_pid}" 2>/dev/null || true
  itest_editor_vscode_detach
  itest_editor_cleanup
}
itest_begin

# The fixture's postStartCommand serves a banner on 17390, the port it lists in
# forwardPorts; postAttachCommand appends a line to ~/.fleet-postattach.log.
FIXTURE_SRC="${INTEGRATION_DIR}/fixture-vscode" setup_test
itest_editor_isolate_ssh
# Remote fleet through a gateway: an editor on "another machine" opens even a
# devcontainer instance over Remote-SSH (the Remote-SSH part below).
itest_editor_gateway_start
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
  # (On a cluster, and from another machine, fleet's ssh-server applies them
  # over Remote-SSH: below.)
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

info "over Remote-SSH, fleet's ssh-server applies the customizations"
itest_editor_gateway_url
printed=$(env -u FLEET_TOKEN FLEET_GATEWAY="${ITEST_EDITOR_GATEWAY_URL}" "${FLEET_BIN}" code --print "${inst}" 2>/dev/null)
alias=$(itest_editor_uri_part "${printed}" alias)
info "alias ${alias}"
commit=c0ffee1234
itest_editor_fake_vscode "${inst}" "${commit}"
installs() {
  "${FLEET_BIN}" exec "${inst}" -- sh -c 'cat "$HOME/.vscode-server/fake-installs.log" 2>/dev/null' || true
}
machine_settings() {
  "${FLEET_BIN}" exec "${inst}" -- sh -c 'cat "$HOME/.vscode-server/data/Machine/settings.json" 2>/dev/null' || true
}
wait_attach_runs() {
  local want="$1" deadline
  deadline=$(( $(date +%s) + $(_scale_timeout 60) ))
  until [ "$(attach_runs)" = "${want}" ]; do
    [ "$(date +%s)" -lt "${deadline}" ] || fail "postAttachCommand ran $(attach_runs) times, want ${want}"
    sleep 0.5
  done
}

info "a VS Code connection: the server is launched, then forwarded to"
itest_editor_vscode_attach "${alias}" "${commit}" 17391 "${TEST_SCRATCH_DIR}/vscode-1"
deadline=$(( $(date +%s) + $(_scale_timeout 120) ))
until [ "$(installs)" = "fleet-itest.fixture-extension" ] && [ -n "$(machine_settings)" ]; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "extensions $(installs) / settings $(machine_settings) never applied"
  sleep 1
done
problems=$(machine_settings | perl -MJSON::PP -e '
  my $s = decode_json(do { local $/; <STDIN> });
  my @bad;
  push @bad, "fleetItest.fromFixture" unless ($s->{"fleetItest.fromFixture"} // "") eq "customized";
  push @bad, "remote.portsAttributes" unless ($s->{"remote.portsAttributes"}{17390}{label} // "") eq "Itest Forwarded"
    && ($s->{"remote.portsAttributes"}{17390}{onAutoForward} // "") eq "silent";
  push @bad, "remote.otherPortsAttributes" unless ($s->{"remote.otherPortsAttributes"}{onAutoForward} // "") eq "ignore";
  print join(" ", @bad);
') || fail "machine settings are not JSON: $(machine_settings)"
[ -z "${problems}" ] || fail "machine settings lack: ${problems} ($(machine_settings))"
wait_attach_runs 3
assert_equals "vscode-server" "$(head -1 "${TEST_SCRATCH_DIR}/vscode-1")" "the forward reached the VS Code server"

info "a plain ssh connection is not a VS Code connection"
itest_editor_ssh "${alias}" true
sleep 3
assert_equals "3" "$(attach_runs)" "a plain ssh connection ran postAttachCommand"
itest_editor_vscode_detach

info "the next VS Code connection runs postAttachCommand again, and installs nothing again"
itest_editor_vscode_attach "${alias}" "${commit}" 17391 "${TEST_SCRATCH_DIR}/vscode-2"
wait_attach_runs 4
itest_editor_vscode_detach
assert_equals "fleet-itest.fixture-extension" "$(installs)" "each extension is installed once"

info "the creation log records what was applied"
log="${HOME}/.fleet/logs/${FIXTURE_REPO_NAME}-alpha.log"
deadline=$(( $(date +%s) + $(_scale_timeout 20) ))
until grep -q 'postAttachCommand: done' "${log}" 2>/dev/null; do
  [ "$(date +%s)" -lt "${deadline}" ] || { cat "${log}" 2>/dev/null || true; fail "the creation log has no postAttachCommand report"; }
  sleep 0.5
done
assert_contains "$(cat "${log}")" "==> vscode extension fleet-itest.fixture-extension: installed" "the creation log reports the install"
assert_contains "$(cat "${log}")" "==> vscode machine settings:" "the creation log reports the settings"
assert_contains "$(cat "${log}")" "==> postAttachCommand (VS Code attached)" "the creation log reports postAttachCommand"

pass "VS Code customizations, postAttachCommand and forwardPorts"
