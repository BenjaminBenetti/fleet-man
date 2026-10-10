package podspec

import (
	"errors"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// After a denial: auto re-renders a pod that carried debug grants, require
// fails naming the knob, and any other denial (functional grants, or the
// re-render denied again) fails with the denial.
func TestDenied(t *testing.T) {
	const denial = `violates PodSecurity "baseline:latest": non-default capabilities`
	withDebug := &PodSpec{DebugGrants: []string{"--cap-add SYS_PTRACE"}}
	dropped := &PodSpec{DebugGrants: []string{"--cap-add SYS_PTRACE"}, DebugDropped: true}
	functional := &PodSpec{}
	auto := &fleet.KubeCluster{ID: "c1", DisplayName: "prod"}
	require := &fleet.KubeCluster{ID: "c1", DisplayName: "prod", DebugCapabilities: fleet.KubeDebugCapabilitiesRequire}

	tests := []struct {
		name     string
		cluster  *fleet.KubeCluster
		pod      *PodSpec
		rerender bool
		code     string
		mentions string
	}{
		{"auto with debug grants", auto, withDebug, true, "", ""},
		{"no connection reads as auto", nil, withDebug, true, "", ""},
		{"require with debug grants", require, withDebug, false, CodeAdmissionDenied + ":debug_capabilities", "set it to auto"},
		{"re-render denied again", auto, dropped, false, CodeAdmissionDenied, denial},
		{"functional denial", auto, functional, false, CodeAdmissionDenied, denial},
		{"functional denial under require", require, functional, false, CodeAdmissionDenied, denial},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rerender, err := Denied(tt.cluster, tt.pod, denial)
			if rerender != tt.rerender {
				t.Errorf("rerender = %v, want %v", rerender, tt.rerender)
			}
			if tt.code == "" {
				if err != nil {
					t.Errorf("err = %v, want none", err)
				}
				return
			}
			var r *Refusal
			if !errors.As(err, &r) || r.Code != tt.code {
				t.Fatalf("err = %v, want code %s", err, tt.code)
			}
			if !strings.HasPrefix(err.Error(), tt.code+": ") || !strings.Contains(err.Error(), tt.mentions) {
				t.Errorf("err = %q, want it to start with %s and mention %q", err, tt.code, tt.mentions)
			}
		})
	}
}

// The whole debug-caps path: a render with the grants is denied, Denied
// asks for a re-render, and the re-render has none of them and says so.
func TestDebugGrantsRerender(t *testing.T) {
	in := baseInput([]string{"--cap-add", "SYS_PTRACE", "--security-opt", "seccomp=unconfined", "--security-opt", "apparmor=unconfined", "--cap-add", "NET_ADMIN"}, nil, nil)
	full, err := Translate(in)
	if err != nil {
		t.Fatal(err)
	}
	s := full.Pod.Security
	if !containsString(s.CapAdd, "SYS_PTRACE") || s.Seccomp != SeccompUnconfined || s.AppArmor != AppArmorUnconfined {
		t.Fatalf("the first render does not request the debug grants: %+v", s)
	}
	rerender, err := Denied(in.Cluster, &full.Pod, "denied")
	if err != nil || !rerender {
		t.Fatalf("Denied = %v, %v", rerender, err)
	}
	in.DropDebugGrants = true
	again, err := Translate(in)
	if err != nil {
		t.Fatal(err)
	}
	s = again.Pod.Security
	if containsString(s.CapAdd, "SYS_PTRACE") || s.Seccomp != "" || s.AppArmor != "" {
		t.Errorf("the re-render keeps debug grants: %+v", s)
	}
	if !containsString(s.CapAdd, "NET_ADMIN") {
		t.Errorf("the re-render dropped a functional grant: %+v", s)
	}
	if !again.Pod.DebugDropped || len(again.Pod.Warnings) == 0 || !strings.HasPrefix(again.Pod.Warnings[0], DebugDroppedWarning) {
		t.Errorf("the re-render does not say it dropped them: %v", again.Pod.Warnings)
	}
	// A second denial is functional now: it fails.
	if rerender, err := Denied(in.Cluster, &again.Pod, "denied"); rerender || err == nil {
		t.Errorf("Denied after the re-render = %v, %v; want a failure", rerender, err)
	}
}

// Privileged pods are auto only on a local cluster, and every privileged
// need of a run is refused together, naming the toggle.
func TestPrivilegePolicy(t *testing.T) {
	yes, no := true, false
	tests := []struct {
		name    string
		local   bool
		allow   *bool
		args    []string
		allowed bool
	}{
		{"local auto", true, nil, []string{"--privileged"}, true},
		{"managed auto", false, nil, []string{"--privileged"}, false},
		{"managed allowed", false, &yes, []string{"--privileged"}, true},
		{"local refused", true, &no, []string{"--privileged"}, false},
		{"host network managed", false, nil, []string{"--network", "host"}, false},
		{"host network allowed", false, &yes, []string{"--network", "host"}, true},
		{"device local", true, nil, []string{"--device", "/dev/kvm"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := baseInput(tt.args, nil, nil)
			in.Facts.Local = tt.local
			in.Cluster.AllowPrivileged = tt.allow
			res, err := Translate(in)
			if tt.allowed {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			rs := Refusals(err)
			if len(rs) != 1 || rs[0].Code != CodePrivileged || !strings.Contains(rs[0].Reason, "Allow privileged pods") {
				t.Fatalf("got %v, %v; want one %s refusal naming the toggle", res, err, CodePrivileged)
			}
		})
	}
}

// A run's refusals are all reported, the first one's code leading.
func TestRefusalsAreJoined(t *testing.T) {
	_, err := Translate(baseInput([]string{"--link", "db", "--network", "my-net", "--sysctl", "kernel.msgmax=1"}, nil, nil))
	rs := Refusals(err)
	var codes []string
	for _, r := range rs {
		codes = append(codes, r.Code)
	}
	want := []string{"kubernetes:unsupported:flag:link", "kubernetes:unsupported:flag:network", "kubernetes:unsupported:flag:sysctl"}
	if strings.Join(codes, " ") != strings.Join(want, " ") {
		t.Fatalf("refusals %v, want %v", codes, want)
	}
	if !strings.HasPrefix(err.Error(), want[0]+": ") {
		t.Errorf("the error does not lead with the first code: %v", err)
	}
	for _, r := range rs {
		if !errors.Is(err, r) {
			t.Errorf("errors.Is does not find %v in the joined error", r)
		}
	}
	if Refusals(nil) != nil || Refusals(errors.New("plain")) != nil {
		t.Error("Refusals of a non-refusal error is not empty")
	}
}
