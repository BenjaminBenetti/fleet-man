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
	saveArmadaLocal = func(remotes []configutil.ArmadaRemote) error { saved = remotes; return nil }
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

	// Keys other than accept/reject are swallowed.
	m.resolveHostKeyPrompt("j")
	if !m.hostKeyPromptShowing() {
		t.Fatal("an unrelated key must not dismiss the prompt")
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
	if st := m.armadaStatus["ssh://ben@desktop"]; st.state != armadaStatusError || !strings.Contains(st.err, "unknown host key SHA256:abc") {
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
	cmd := m.resolveHostKeyPrompt("y")
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
	if sp.armadaAddStage != armadaAddNone || !strings.Contains(m.message, "permission denied") {
		t.Fatalf("stage=%v message=%q", sp.armadaAddStage, m.message)
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
