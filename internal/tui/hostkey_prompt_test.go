package tui

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/configutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// unknownKeyErr builds the error the local daemon returns for an unknown host
// key: FailedPrecondition with an UnknownSSHHostKey detail.
func unknownKeyErr(t *testing.T, url, fp string) error {
	t.Helper()
	st, err := status.New(codes.FailedPrecondition, "host key for desktop is not known ("+fp+")").WithDetails(&fleetgrpc.UnknownSSHHostKey{
		Url: url, Name: "desktop", Host: "10.0.0.5", Port: 22, KeyType: "ED25519", KnownHostsPath: "/home/ben/.ssh/known_hosts",
		Keys: []*fleetgrpc.SSHHostKey{
			{KeyType: "ssh-ed25519", Fingerprint: fp, KnownHostsLine: "desktop ssh-ed25519 AAAA"},
			{KeyType: "ssh-rsa", Fingerprint: "SHA256:rsa", KnownHostsLine: "desktop ssh-rsa BBBB"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return st.Err()
}

// stubTrust replaces the accept RPC, recording the line it was asked to trust.
func stubTrust(t *testing.T, result error) *[]string {
	t.Helper()
	var lines []string
	orig := fleetclient.TrustSSHHostKey
	fleetclient.TrustSSHHostKey = func(_ context.Context, url, line string) error {
		lines = append(lines, url+" "+line)
		return result
	}
	t.Cleanup(func() { fleetclient.TrustSSHHostKey = orig })
	return &lines
}

// startAddFlow drives the add flow to its connection test for url and returns
// the settings page.
func startAddFlow(t *testing.T, m *model, url string) *settingsPage {
	t.Helper()
	sp := m.currentPage.(*settingsPage)
	sp.cursor = settingsPositionOf(sp, m, settingsItemArmadaAdd)
	sp.Update(m, tea.KeyMsg{Type: tea.KeyEnter})
	typeRunes(sp, m, url)
	sp.Update(m, tea.KeyMsg{Type: tea.KeyEnter})
	if sp.armadaAddStage != armadaAddTesting {
		t.Fatalf("stage = %v, want testing", sp.armadaAddStage)
	}
	return sp
}

// TestHostKeyPromptAcceptRegistersRemote: an unknown key during the add-flow
// connection test opens the prompt (the flow stays in testing); accept trusts
// exactly the offered ED25519 line and, once the daemon connected, registers
// the remote.
func TestHostKeyPromptAcceptRegistersRemote(t *testing.T) {
	origPing := pingArmadaRemote
	pingArmadaRemote = func(string, string) error { return nil }
	defer func() { pingArmadaRemote = origPing }()
	var saved []configutil.ArmadaRemote
	origSave := saveArmadaLocal
	saveArmadaLocal = func(remotes []configutil.ArmadaRemote) ([]configutil.ArmadaRemote, error) {
		saved = remotes
		return remotes, nil
	}
	defer func() { saveArmadaLocal = origSave }()
	trusted := stubTrust(t, nil)

	sp := newSettingsPage()
	m := armadaTestModel(sp)
	startAddFlow(t, m, "ssh://ben@desktop")

	cmd := m.handleArmadaMsg(armadaTestResultMsg{url: "ssh://ben@desktop", err: unknownKeyErr(t, "ssh://ben@desktop", "SHA256:abc")})
	if cmd != nil {
		t.Fatal("an unknown key must not chain into a save")
	}
	if !m.hostKeyPromptShowing() || sp.armadaAddStage != armadaAddTesting {
		t.Fatalf("prompt=%v stage=%v; want prompt up with the flow still testing", m.hostKeyPromptShowing(), sp.armadaAddStage)
	}
	view := ansi.Strip(m.View())
	for _, want := range []string{"Unknown SSH host key for desktop", "ssh-ed25519", "SHA256:abc", "10.0.0.5:22", "/home/ben/.ssh/known_hosts", "[a]ccept", "[r]eject", "also offered"} {
		if !strings.Contains(view, want) {
			t.Errorf("prompt view lacks %q:\n%s", want, view)
		}
	}

	// Keys other than accept/reject are swallowed — including enter and y:
	// enter is the key that raised the prompt (it starts the connection test
	// and the explicit ping), so a second press must never trust a key.
	for _, k := range []string{"j", "enter", "y", " "} {
		if cmd := m.resolveHostKeyPrompt(k); cmd != nil || !m.hostKeyPromptShowing() || m.hostKeyPrompt.busy {
			t.Fatalf("key %q must neither accept nor dismiss the prompt", k)
		}
	}
	if len(*trusted) != 0 {
		t.Fatal("no key other than a may trust")
	}

	acceptCmd := m.resolveHostKeyPrompt("a")
	if acceptCmd == nil || !m.hostKeyPrompt.busy {
		t.Fatal("accept should run the trust command and mark the prompt busy")
	}
	if strings.Contains(ansi.Strip(m.View()), "[a]ccept") {
		t.Fatal("busy prompt should not offer the keys again")
	}
	msg := acceptCmd().(hostKeyTrustedMsg)
	if len(*trusted) != 1 || (*trusted)[0] != "ssh://ben@desktop desktop ssh-ed25519 AAAA" {
		t.Fatalf("trusted %v, want exactly the offered ed25519 line", *trusted)
	}
	saveCmd := m.handleHostKeyTrusted(msg)
	if m.hostKeyPromptShowing() || saveCmd == nil {
		t.Fatal("a successful trust during the add flow should register the remote")
	}
	m.handleArmadaMsg(saveCmd())
	if len(saved) != 1 || saved[0].URL != "ssh://ben@desktop" || saved[0].Token != "" {
		t.Fatalf("saved = %+v", saved)
	}
	if m.armadaStatus["ssh://ben@desktop"].state != armadaStatusConnected {
		t.Fatal("trusted remote should show connected")
	}
}

// TestHostKeyPromptRejectCancelsAdd: reject writes nothing, cancels the add
// flow with a message, and records the decision.
func TestHostKeyPromptRejectCancelsAdd(t *testing.T) {
	trusted := stubTrust(t, nil)
	sp := newSettingsPage()
	m := armadaTestModel(sp)
	startAddFlow(t, m, "ssh://ben@desktop")
	m.handleArmadaMsg(armadaTestResultMsg{url: "ssh://ben@desktop", err: unknownKeyErr(t, "ssh://ben@desktop", "SHA256:abc")})

	if cmd := m.resolveHostKeyPrompt("r"); cmd != nil {
		t.Fatal("reject must not run anything")
	}
	if m.hostKeyPromptShowing() || sp.armadaAddStage != armadaAddNone {
		t.Fatalf("prompt=%v stage=%v after reject", m.hostKeyPromptShowing(), sp.armadaAddStage)
	}
	if !strings.Contains(m.message, "rejected") || !strings.Contains(m.message, "not added") {
		t.Fatalf("message = %q", m.message)
	}
	if len(*trusted) != 0 {
		t.Fatal("reject must not call TrustSSHHostKey")
	}
	if !m.hostKeyDeclined["ssh://ben@desktop|SHA256:abc"] {
		t.Fatal("the rejection should be remembered per remote+fingerprint")
	}
}

// TestHostKeyPromptOncePerKey: a connect-path error (the Watch stream at
// boot / on reconnect) prompts once; further copies while it is up, and
// reconnects after a reject, are swallowed; a different key asks again; an
// explicit ping re-asks even after a reject.
func TestHostKeyPromptOncePerKey(t *testing.T) {
	t.Setenv("FLEET_GATEWAY", "")
	t.Setenv("FLEET_SERVER", "")
	t.Setenv("FLEET_SSH", "ssh://ben@desktop")
	m := armadaTestModel(nil)
	errA := unknownKeyErr(t, "ssh://ben@desktop", "SHA256:abc")

	// The Update dispatch routes a Watch connect failure into the prompt
	// (value receiver: inspect the returned model).
	res, _ := m.Update(watchErrMsg{err: errA})
	if rm := res.(model); rm.hostKeyPrompt == nil {
		t.Fatal("the connect-path error should open the prompt")
	}
	// The dedupe rules, driven through offerHostKey as that dispatch does.
	connect := func(err error) bool { return m.offerHostKey(currentSSHURL(), err, hostKeyOriginConnect) }
	if !connect(errA) || !m.hostKeyPromptShowing() {
		t.Fatal("the connect-path error should open the prompt")
	}
	first := m.hostKeyPrompt
	if !connect(errA) || m.hostKeyPrompt != first {
		t.Fatal("a concurrent dial's copy must reuse the open prompt")
	}
	m.resolveHostKeyPrompt("r")
	if !connect(errA) || m.hostKeyPromptShowing() {
		t.Fatal("after a reject, background reconnects must stay quiet (but the error is still taken over)")
	}
	if !connect(unknownKeyErr(t, "ssh://ben@desktop", "SHA256:different")) || !m.hostKeyPromptShowing() {
		t.Fatal("a different key for the same remote must be asked about afresh")
	}
	m.resolveHostKeyPrompt("r")

	// A non-host-key error never prompts and is not taken over.
	if connect(errors.New("connection refused")) || m.hostKeyPromptShowing() {
		t.Fatal("plain errors must not prompt")
	}

	// An explicit ping re-asks even for a rejected key; a background sweep
	// result only updates the status text.
	m.armadaRemotes = []configutil.ArmadaRemote{{URL: "ssh://ben@desktop"}}
	m.handleArmadaMsg(armadaPingResultMsg{url: "ssh://ben@desktop", err: errA})
	if m.hostKeyPromptShowing() {
		t.Fatal("a background ping must not prompt")
	}
	if st := m.armadaStatus["ssh://ben@desktop"]; st.state != armadaStatusError || !strings.Contains(st.err, "unknown host key") {
		t.Fatalf("status after background ping = %+v", st)
	}
	m.armadaExplicitPing["ssh://ben@desktop"] = true
	m.handleArmadaMsg(armadaPingResultMsg{url: "ssh://ben@desktop", err: errA})
	if !m.hostKeyPromptShowing() || m.hostKeyPrompt.origin != hostKeyOriginPing {
		t.Fatal("an explicit ping should re-open the prompt")
	}
}

// TestHostKeyPromptAcceptOnConnectReconnects: accepting for the connection
// the TUI is on bounces the Watch stream and reloads, instead of waiting out
// the reconnect backoff.
func TestHostKeyPromptAcceptOnConnectReconnects(t *testing.T) {
	t.Setenv("FLEET_GATEWAY", "")
	t.Setenv("FLEET_SERVER", "")
	t.Setenv("FLEET_SSH", "ssh://ben@desktop")
	trusted := stubTrust(t, nil)
	m := armadaTestModel(nil)
	m.offerHostKey("ssh://ben@desktop", unknownKeyErr(t, "ssh://ben@desktop", "SHA256:abc"), hostKeyOriginConnect)
	cmd := m.resolveHostKeyPrompt("a")
	msg := cmd().(hostKeyTrustedMsg)
	if len(*trusted) != 1 {
		t.Fatalf("trusted %v", *trusted)
	}
	// (bounceWatchStream is a no-op in unit tests — the stream was never
	// started — so the reload command is the observable effect here.)
	reload := m.handleHostKeyTrusted(msg)
	if reload == nil || m.hostKeyPromptShowing() {
		t.Fatal("accept on the live connection should bounce the watch and reload")
	}
	if !strings.Contains(m.message, "trusted") {
		t.Fatalf("message = %q", m.message)
	}
}

// TestHostKeyPromptTrustFailureReported: a failed accept (e.g. the daemon
// could not write known_hosts) reports the reason and cancels the add flow.
func TestHostKeyPromptTrustFailureReported(t *testing.T) {
	stubTrust(t, status.Error(codes.FailedPrecondition, "write /home/ben/.ssh/known_hosts: permission denied"))
	sp := newSettingsPage()
	m := armadaTestModel(sp)
	startAddFlow(t, m, "ssh://ben@desktop")
	m.handleArmadaMsg(armadaTestResultMsg{url: "ssh://ben@desktop", err: unknownKeyErr(t, "ssh://ben@desktop", "SHA256:abc")})
	msg := m.resolveHostKeyPrompt("a")().(hostKeyTrustedMsg)
	if cmd := m.handleHostKeyTrusted(msg); cmd != nil {
		t.Fatal("a failed trust must not save the remote")
	}
	if sp.armadaAddStage != armadaAddNone || !strings.Contains(m.message, "permission denied") || strings.Contains(m.message, "trusted,") {
		t.Fatalf("stage=%v message=%q (must not claim the key was trusted)", sp.armadaAddStage, m.message)
	}
}

// TestUnknownSSHHostKeyDetailRoundTrip pins the client-side extraction the
// prompt depends on, including the nil cases.
func TestUnknownSSHHostKeyDetailRoundTrip(t *testing.T) {
	if fleetclient.UnknownSSHHostKey(nil) != nil || fleetclient.UnknownSSHHostKey(errors.New("x")) != nil ||
		fleetclient.UnknownSSHHostKey(status.Error(codes.FailedPrecondition, "plain")) != nil {
		t.Fatal("non-detail errors must yield nil")
	}
	uk := fleetclient.UnknownSSHHostKey(unknownKeyErr(t, "ssh://ben@desktop", "SHA256:abc"))
	if uk == nil || uk.GetName() != "desktop" || uk.GetKeys()[0].GetFingerprint() != "SHA256:abc" {
		t.Fatalf("detail = %+v", uk)
	}
	_ = os.Getenv // keep os imported for the env-driven tests above
}

// TestHostKeyPromptDoesNotSwallowOtherRemote: an open prompt for remote A
// must not take over remote B's add-flow test result — B's flow fails normally
// instead of sitting in its testing stage waiting for a decision about A.
func TestHostKeyPromptDoesNotSwallowOtherRemote(t *testing.T) {
	sp := newSettingsPage()
	m := armadaTestModel(sp)
	m.armadaRemotes = []configutil.ArmadaRemote{{URL: "ssh://a"}}
	// An explicit ping of A comes back with an unknown key: the prompt opens for A.
	m.armadaExplicitPing["ssh://a"] = true
	m.handleArmadaMsg(armadaPingResultMsg{url: "ssh://a", err: unknownKeyErr(t, "ssh://a", "SHA256:aaa")})
	if !m.hostKeyPromptShowing() || m.hostKeyPrompt.url != "ssh://a" {
		t.Fatal("expected the prompt for A")
	}
	// Meanwhile B's add-flow test also fails on an unknown key.
	startAddFlow(t, m, "ssh://b")
	m.handleArmadaMsg(armadaTestResultMsg{url: "ssh://b", err: unknownKeyErr(t, "ssh://b", "SHA256:bbb")})
	if sp.armadaAddStage != armadaAddNone || !strings.Contains(m.message, "Connection test failed") {
		t.Fatalf("B's flow should fail normally: stage=%v message=%q", sp.armadaAddStage, m.message)
	}
	if m.hostKeyPrompt.url != "ssh://a" {
		t.Fatal("A's prompt must stay up, unchanged")
	}
	// A's own concurrent copy is still shared.
	if !m.offerHostKey("ssh://a", unknownKeyErr(t, "ssh://a", "SHA256:aaa"), hostKeyOriginConnect) {
		t.Fatal("the same remote's copy should be swallowed by the open prompt")
	}
}

// TestSwitchArmadaReasksRejectedKey: an explicit switch to a remote whose key
// was rejected earlier clears the rejection, so the switch's dial failure
// reopens the prompt instead of leaving "Switching to …" hanging silently.
func TestSwitchArmadaReasksRejectedKey(t *testing.T) {
	t.Setenv("FLEET_GATEWAY", "")
	t.Setenv("FLEET_TOKEN", "")
	t.Setenv("FLEET_SERVER", "")
	t.Setenv("FLEET_SSH", "ssh://ben@desktop")
	m := armadaTestModel(nil)
	errA := unknownKeyErr(t, "ssh://ben@desktop", "SHA256:abc")

	m.offerHostKey("ssh://ben@desktop", errA, hostKeyOriginConnect)
	m.resolveHostKeyPrompt("r")
	if !m.offerHostKey("ssh://ben@desktop", errA, hostKeyOriginConnect) || m.hostKeyPromptShowing() {
		t.Fatal("after a reject, a background reconnect stays quiet")
	}

	// Switch away and explicitly back.
	m.switchArmada(m.armadaEntries()[0]) // local
	m.switchArmada(armadaEntry{url: "ssh://ben@desktop"})
	if len(m.hostKeyDeclined) != 0 {
		t.Fatalf("an explicit switch should clear the remote's rejections, got %v", m.hostKeyDeclined)
	}
	m.handleArmadaMsg(armadaSwitchedMsg{label: "desktop", gen: m.watchGen, err: errA})
	if !m.hostKeyPromptShowing() || m.hostKeyPrompt.url != "ssh://ben@desktop" {
		t.Fatalf("the switch's dial failure should reopen the prompt; message=%q", m.message)
	}
}

// TestArmadaSelectEnterRetriesRejectedCurrent: enter on the CURRENT remote in
// the Armada selector, when it is in error (its row invites the keypress),
// retries it explicitly — clearing a rejected host key so the prompt can
// reopen — instead of claiming "Already connected".
func TestArmadaSelectEnterRetriesRejectedCurrent(t *testing.T) {
	t.Setenv("FLEET_GATEWAY", "")
	t.Setenv("FLEET_SERVER", "")
	t.Setenv("FLEET_SSH", "ssh://qa@fleet-remote")
	origPing := pingArmadaRemote
	pingArmadaRemote = func(string, string) error { return nil }
	defer func() { pingArmadaRemote = origPing }()
	m := armadaTestModel(nil)
	m.armadaRemotes = []configutil.ArmadaRemote{{URL: "ssh://qa@fleet-remote"}}
	errK := unknownKeyErr(t, "ssh://qa@fleet-remote", "SHA256:abc")
	m.offerHostKey("ssh://qa@fleet-remote", errK, hostKeyOriginConnect)
	m.resolveHostKeyPrompt("r")
	if m.armadaStatus["ssh://qa@fleet-remote"].state != armadaStatusError || len(m.hostKeyDeclined) != 1 {
		t.Fatal("setup: rejected remote should be in error with the rejection recorded")
	}

	fp := m.fleetPage
	fp.openArmadaSelect(m)
	for i, e := range m.armadaEntries() {
		if e.current {
			fp.armadaSel.dialogRow = i
		}
	}
	cmd := fp.updateArmadaSelect(m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil || strings.Contains(m.message, "Already connected") {
		t.Fatalf("enter on the erroring current remote should retry it: cmd=%v message=%q", cmd != nil, m.message)
	}
	if len(m.hostKeyDeclined) != 0 || !m.armadaExplicitPing["ssh://qa@fleet-remote"] {
		t.Fatal("the retry must clear the rejection and count as an explicit ping")
	}
	// The retry's unknown-key result reopens the prompt; accepting reconnects
	// the live connection.
	stubTrust(t, nil)
	m.handleArmadaMsg(armadaPingResultMsg{url: "ssh://qa@fleet-remote", err: errK})
	if !m.hostKeyPromptShowing() {
		t.Fatal("the explicit retry should reopen the prompt")
	}
	msg := m.resolveHostKeyPrompt("a")().(hostKeyTrustedMsg)
	if reload := m.handleHostKeyTrusted(msg); reload == nil || !strings.Contains(m.message, "connecting to") {
		t.Fatalf("accepting for the current connection should reconnect; message=%q", m.message)
	}

	// A healthy current entry says so — including in the window right after
	// the dropdown opened, when its own re-ping is still in flight (the entry
	// reads Pinging, with no rejection on record).
	m.armadaStatus["ssh://qa@fleet-remote"] = armadaStatus{state: armadaStatusConnected}
	fp.openArmadaSelect(m)
	for i, e := range m.armadaEntries() {
		if e.current {
			fp.armadaSel.dialogRow = i
		}
	}
	if m.armadaStatus["ssh://qa@fleet-remote"].state != armadaStatusPinging {
		t.Fatal("setup: opening the dropdown re-pings the current entry")
	}
	if cmd := fp.updateArmadaSelect(m, tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil || !strings.Contains(m.message, "Already connected") {
		t.Fatalf("a healthy current entry mid-ping: cmd=%v message=%q", cmd != nil, m.message)
	}
}

// TestArmadaSelectRetryShowsOutcome: an explicit retry from the selector that
// does not open the prompt replaces "Retrying …" with its outcome, success or
// failure (the dropdown has closed, so its row can't show it).
func TestArmadaSelectRetryShowsOutcome(t *testing.T) {
	t.Setenv("FLEET_GATEWAY", "")
	t.Setenv("FLEET_TOKEN", "")
	t.Setenv("FLEET_SERVER", "")
	t.Setenv("FLEET_SSH", "ssh://qa@fleet-remote")
	origPing := pingArmadaRemote
	pingArmadaRemote = func(string, string) error { return nil }
	defer func() { pingArmadaRemote = origPing }()
	m := armadaTestModel(nil)
	m.armadaRemotes = []configutil.ArmadaRemote{{URL: "ssh://qa@fleet-remote"}}
	fp := m.fleetPage

	retry := func() {
		m.armadaStatus["ssh://qa@fleet-remote"] = armadaStatus{state: armadaStatusError, err: "connection refused"}
		fp.openArmadaSelect(m)
		m.armadaStatus["ssh://qa@fleet-remote"] = armadaStatus{state: armadaStatusError, err: "connection refused"} // the sweep's result landed
		for i, e := range m.armadaEntries() {
			if e.current {
				fp.armadaSel.dialogRow = i
			}
		}
		if cmd := fp.updateArmadaSelect(m, tea.KeyMsg{Type: tea.KeyEnter}); cmd == nil || !strings.Contains(m.message, "Retrying") {
			t.Fatalf("an erroring current entry should retry: cmd=%v message=%q", cmd != nil, m.message)
		}
	}
	retry()
	m.handleArmadaMsg(armadaPingResultMsg{url: "ssh://qa@fleet-remote"})
	if m.message != "Connected to fleet-remote" || m.armadaStatus["ssh://qa@fleet-remote"].state != armadaStatusConnected {
		t.Fatalf("successful retry: message=%q", m.message)
	}
	retry()
	m.handleArmadaMsg(armadaPingResultMsg{url: "ssh://qa@fleet-remote", err: status.Error(codes.Unavailable, "x")})
	if m.message != "fleet-remote: ssh tunnel unreachable" {
		t.Fatalf("failed retry: message=%q", m.message)
	}
	// A background sweep result never touches the status line.
	m.message = "untouched"
	m.handleArmadaMsg(armadaPingResultMsg{url: "ssh://qa@fleet-remote"})
	if m.message != "untouched" {
		t.Fatalf("background result changed the message: %q", m.message)
	}
}

// TestSettingsRowPingOutcomeWording: an explicit Settings-row ping of a remote
// the TUI is NOT on reports a probe result ("X is reachable"), never
// "Connected to X"; a failure repeats the reason (useful when the row
// truncates it); names match the selector's display names.
func TestSettingsRowPingOutcomeWording(t *testing.T) {
	t.Setenv("FLEET_GATEWAY", "")
	t.Setenv("FLEET_TOKEN", "")
	t.Setenv("FLEET_SERVER", "")
	t.Setenv("FLEET_SSH", "") // the TUI is on local
	origPing := pingArmadaRemote
	pingArmadaRemote = func(string, string) error { return nil }
	defer func() { pingArmadaRemote = origPing }()
	sp := newSettingsPage()
	m := armadaTestModel(sp)
	m.armadaRemotes = []configutil.ArmadaRemote{{URL: "ssh://qa@fleet-remote"}, {URL: "ssh://root@fleet-remote"}}

	sp.cursor = settingsPositionOf(sp, m, settingsItemArmadaBase)
	if cmd := sp.Update(m, tea.KeyMsg{Type: tea.KeyEnter}); cmd == nil {
		t.Fatal("enter on the row should ping")
	}
	m.handleArmadaMsg(armadaPingResultMsg{url: "ssh://qa@fleet-remote"})
	if m.message != "qa@fleet-remote is reachable" {
		t.Fatalf("success while local: message=%q", m.message)
	}
	sp.Update(m, tea.KeyMsg{Type: tea.KeyEnter})
	m.handleArmadaMsg(armadaPingResultMsg{url: "ssh://qa@fleet-remote", err: status.Error(codes.Unavailable, "x")})
	if m.message != "qa@fleet-remote: ssh tunnel unreachable" {
		t.Fatalf("failure: message=%q", m.message)
	}
}

// TestHostKeyPromptIgnoresStaleDetailForOtherRemote: a Watch error carrying
// remote A's key that lands after the TUI switched to B (watchErrMsg has no
// generation stamp) must not open a prompt labelled B with A's fingerprint,
// and must not take over B's own errors.
func TestHostKeyPromptIgnoresStaleDetailForOtherRemote(t *testing.T) {
	t.Setenv("FLEET_GATEWAY", "")
	t.Setenv("FLEET_TOKEN", "")
	t.Setenv("FLEET_SERVER", "")
	t.Setenv("FLEET_SSH", "ssh://b")
	m := armadaTestModel(nil)
	stale := unknownKeyErr(t, "ssh://a", "SHA256:aaa")
	if m.offerHostKey(currentSSHURL(), stale, hostKeyOriginConnect) || m.hostKeyPromptShowing() {
		t.Fatal("a detail for another remote must be ignored, not prompted")
	}
	res, _ := m.Update(watchErrMsg{err: stale})
	if rm := res.(model); rm.hostKeyPrompt != nil {
		t.Fatal("the Update dispatch must not prompt on a stale detail either")
	}
	// B's own error still prompts, labelled B.
	if !m.offerHostKey("ssh://b", unknownKeyErr(t, "ssh://b", "SHA256:bbb"), hostKeyOriginConnect) || m.hostKeyPrompt.url != "ssh://b" || m.hostKeyPrompt.key.GetKeys()[0].GetFingerprint() != "SHA256:bbb" {
		t.Fatal("B's own error should open B's prompt")
	}
}

// TestHostKeyPromptConnectOriginFinishesAddFlow: a Connect prompt already open
// for the unregistered FLEET_SSH remote absorbs that remote's own add-flow
// test result; the decision must still resolve the add flow — reject cancels
// it, accept registers it AND reconnects the live connection.
func TestHostKeyPromptConnectOriginFinishesAddFlow(t *testing.T) {
	t.Setenv("FLEET_GATEWAY", "")
	t.Setenv("FLEET_TOKEN", "")
	t.Setenv("FLEET_SERVER", "")
	t.Setenv("FLEET_SSH", "ssh://desktop")
	origPing := pingArmadaRemote
	pingArmadaRemote = func(string, string) error { return nil }
	defer func() { pingArmadaRemote = origPing }()
	var saved []configutil.ArmadaRemote
	origSave := saveArmadaLocal
	saveArmadaLocal = func(remotes []configutil.ArmadaRemote) ([]configutil.ArmadaRemote, error) {
		saved = remotes
		return remotes, nil
	}
	defer func() { saveArmadaLocal = origSave }()
	errK := unknownKeyErr(t, "ssh://desktop", "SHA256:abc")

	// Reject path.
	sp := newSettingsPage()
	m := armadaTestModel(sp)
	startAddFlow(t, m, "ssh://desktop")
	m.offerHostKey("ssh://desktop", errK, hostKeyOriginConnect) // the Watch dial got there first
	if cmd := m.handleArmadaMsg(armadaTestResultMsg{url: "ssh://desktop", err: errK}); cmd != nil || sp.armadaAddStage != armadaAddTesting {
		t.Fatal("the add result should be absorbed by the open prompt for the same remote")
	}
	m.resolveHostKeyPrompt("r")
	if sp.armadaAddStage != armadaAddNone || !strings.Contains(m.message, "was not added") {
		t.Fatalf("reject must cancel the waiting add flow: stage=%v message=%q", sp.armadaAddStage, m.message)
	}

	// Accept path: registers the remote and reconnects.
	stubTrust(t, nil)
	sp = newSettingsPage()
	m = armadaTestModel(sp)
	startAddFlow(t, m, "ssh://desktop")
	m.offerHostKey("ssh://desktop", errK, hostKeyOriginConnect)
	m.handleArmadaMsg(armadaTestResultMsg{url: "ssh://desktop", err: errK})
	msg := m.resolveHostKeyPrompt("a")().(hostKeyTrustedMsg)
	batch := m.handleHostKeyTrusted(msg)
	if batch == nil || !strings.Contains(m.message, "connecting to desktop") {
		t.Fatalf("accept should reconnect the live connection; message=%q", m.message)
	}
	// Run the batched commands: one of them is the registry save.
	for _, out := range runBatch(batch) {
		if sm, ok := out.(armadaSaveResultMsg); ok {
			m.handleArmadaMsg(sm)
		}
	}
	if len(saved) != 1 || saved[0].URL != "ssh://desktop" || sp.armadaAddStage != armadaAddNone {
		t.Fatalf("accept should also register the remote: saved=%+v stage=%v", saved, sp.armadaAddStage)
	}
}

// runBatch executes a tea.Cmd that may be a Batch, returning every message
// it produced (a single command yields one).
func runBatch(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, runBatch(c)...)
		}
		return out
	}
	return []tea.Msg{msg}
}

// TestHostKeyAcceptFromSettingsRowReconnectsCurrent is QA's recovery path:
// boot on FLEET_SSH, reject the key at boot, then Settings → enter on the
// remote's row → accept. The accept must reconnect the live connection —
// drop the cached mutation connection, bounce the Watch stream, reload — and
// the reload must clear the boot error banner, instead of leaving the TUI on
// the stale "host key … is not known" error until the reconnect backoff runs
// out.
func TestHostKeyAcceptFromSettingsRowReconnectsCurrent(t *testing.T) {
	t.Setenv("FLEET_GATEWAY", "")
	t.Setenv("FLEET_TOKEN", "")
	t.Setenv("FLEET_SERVER", "")
	t.Setenv("FLEET_SSH", "ssh://bob@fleethost")
	origPing := pingArmadaRemote
	pingArmadaRemote = func(string, string) error { return nil }
	defer func() { pingArmadaRemote = origPing }()
	trusted := stubTrust(t, nil)

	sp := newSettingsPage()
	m := armadaTestModel(sp)
	m.armadaRemotes = []configutil.ArmadaRemote{{URL: "ssh://bob@fleethost"}}
	errK := unknownKeyErr(t, "ssh://bob@fleethost", "SHA256:abc")

	// Boot: the reload failed on the unknown key (banner), the Watch dial
	// prompted, the user rejected.
	m.err = errK
	m.offerHostKey("ssh://bob@fleethost", errK, hostKeyOriginConnect)
	m.resolveHostKeyPrompt("r")

	// Settings → enter on the remote's row: an explicit ping, whose result
	// reopens the prompt with the ping origin.
	sp.cursor = settingsPositionOf(sp, m, settingsItemArmadaBase)
	if cmd := sp.Update(m, tea.KeyMsg{Type: tea.KeyEnter}); cmd == nil || !m.armadaExplicitPing["ssh://bob@fleethost"] {
		t.Fatal("enter on the row should ping explicitly")
	}
	m.handleArmadaMsg(armadaPingResultMsg{url: "ssh://bob@fleethost", err: errK})
	if !m.hostKeyPromptShowing() || m.hostKeyPrompt.origin != hostKeyOriginPing {
		t.Fatal("the explicit ping should reopen the prompt")
	}

	// Accept: trusts, then reconnects the CURRENT connection whatever raised
	// the prompt.
	msg := m.resolveHostKeyPrompt("a")().(hostKeyTrustedMsg)
	if len(*trusted) != 1 {
		t.Fatalf("trusted %v", *trusted)
	}
	reload := m.handleHostKeyTrusted(msg)
	if reload == nil || !strings.Contains(m.message, "connecting to fleethost") {
		t.Fatalf("accept for the live connection must bounce and reload; message=%q", m.message)
	}
	if m.armadaStatus["ssh://bob@fleethost"].state != armadaStatusConnected {
		t.Fatal("row should show connected")
	}
	// The reload's success clears the boot banner.
	m.handleArmadaMsg(armadaSwitchedMsg{label: "fleethost", gen: m.watchGen, st: &configutil.State{}, config: configutil.DefaultConfig()})
	if m.err != nil {
		t.Fatalf("the stale host-key banner must be cleared after reconnecting: %v", m.err)
	}
	// The trusted key retires the boot-time rejection, so a later A → enter
	// on the (healthy, mid-ping) current entry is "Already connected", not a
	// spurious retry.
	if m.hasHostKeyRejection("ssh://bob@fleethost") {
		t.Fatal("a successful accept must clear the remote's rejection")
	}
	fp := m.fleetPage
	fp.openArmadaSelect(m)
	for i, e := range m.armadaEntries() {
		if e.current {
			fp.armadaSel.dialogRow = i
		}
	}
	if cmd := fp.updateArmadaSelect(m, tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil || !strings.Contains(m.message, "Already connected") {
		t.Fatalf("healthy current entry after an accept: cmd=%v message=%q", cmd != nil, m.message)
	}
}
