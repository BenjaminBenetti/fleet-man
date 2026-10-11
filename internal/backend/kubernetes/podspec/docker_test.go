package podspec

import (
	"reflect"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/podinit"
)

const dockerSockBind = "type=bind,source=/var/run/docker.sock,target=/var/run/docker-host.sock"

// A run that reaches the docker engine gets the pod's own engine, a
// sidecar: privileged where privileged pods are allowed, under sysbox where
// the cluster offers it (and nothing else needs privilege), refused with
// its own code everywhere else. A privileged dev container gets its own
// engine's config.
func TestDockerEngines(t *testing.T) {
	yes, no := true, false
	pools := []fleet.DockerAddressPool{{Base: "10.200.0.0/16", Size: 24}}
	for _, tt := range []struct {
		name string
		args []string
		in   func(*Input)
		// dood: the sidecar's privilege ("" none, "privileged",
		// "unprivileged"); dind: pid1.json configures an engine.
		dood    string
		dind    bool
		runtime string
		code    string
		warn    string
	}{
		{name: "dood on a local cluster", args: []string{"--mount", dockerSockBind}, in: local, dood: "privileged", warn: "pod-local siblings"},
		{name: "dood without the toggle", args: []string{"--mount", dockerSockBind}, code: CodeDooDNeedsPrivileged},
		{name: "dood with the toggle off on a local cluster", args: []string{"--mount", dockerSockBind},
			in: func(in *Input) { local(in); in.Cluster.AllowPrivileged = &no }, code: CodeDooDNeedsPrivileged},
		{name: "dood with the toggle on", args: []string{"--mount", dockerSockBind},
			in: func(in *Input) { in.Cluster.AllowPrivileged = &yes }, dood: "privileged"},
		{name: "dood under sysbox", args: []string{"--mount", dockerSockBind},
			in: func(in *Input) { in.Facts.RuntimeClasses = []string{"sysbox-runc"} }, dood: "unprivileged", dind: true, runtime: "sysbox-runc", warn: "sysbox-runc runtime class"},
		{name: "dood beside host networking is never sysbox", args: []string{"--mount", dockerSockBind, "--network", "host"},
			in: func(in *Input) { local(in); in.Facts.RuntimeClasses = []string{"sysbox-runc"} }, dood: "privileged"},
		{name: "api socket", args: []string{"--use-api-socket"}, in: local, dood: "privileged"},
		// The engine is never the daemon host's, so a host without a
		// socket there (a rootless or remote docker) changes nothing.
		{name: "a docker.sock the daemon host lacks", args: []string{"--mount", "type=bind,source=/run/user/1000/docker.sock,target=/var/run/docker.sock"}, in: local, dood: "privileged"},
		{name: "dind and dood refused together", args: []string{"--privileged", "--mount", dockerSockBind}, code: CodePrivileged},
		{name: "dind", args: []string{"--privileged"}, in: local, dind: true},
		{name: "dind under sysbox", args: []string{"--privileged"}, in: func(in *Input) { in.Facts.RuntimeClasses = []string{"sysbox-runc"} }, dind: true, runtime: "sysbox-runc"},
		{name: "a connection's sysbox runtime class", in: func(in *Input) { in.Cluster.RuntimeClass = "sysbox-runc" }, dind: true, runtime: "sysbox-runc"},
		{name: "unprivileged", in: local},
		{name: "dood on an old cluster", args: []string{"--mount", dockerSockBind}, in: func(in *Input) { local(in); in.Facts.ServerVersion = "v1.28.9" }, code: CodeDooDNeedsSidecars},
		{name: "dood on a managed cluster's version", args: []string{"--mount", dockerSockBind}, in: func(in *Input) { local(in); in.Facts.ServerVersion = "v1.30.5-gke.1014001" }, dood: "privileged"},
		{name: "dind on a file share", args: []string{"--privileged"}, in: func(in *Input) { local(in); in.Facts.DiskFileShare = "nfs-csi" }, dind: true, warn: "nfs-csi is a network file share"},
		{name: "a pool that does not read", args: []string{"--privileged"}, in: func(in *Input) { local(in); in.Cluster.DinDAddressPool = "10.200.0.0" }, code: CodeSettings},
	} {
		t.Run(tt.name, func(t *testing.T) {
			in := baseInput(tt.args, nil, nil)
			in.Cluster.DinDAddressPool = "10.200.0.0/16"
			if tt.in != nil {
				tt.in(&in)
			}
			res, err := Translate(in)
			if tt.code != "" {
				rs := Refusals(err)
				if len(rs) == 0 || rs[0].Code != tt.code {
					t.Fatalf("Translate = %v, want %s first", err, tt.code)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			p := res.Pod
			switch {
			case tt.dood == "" && p.DooD != nil:
				t.Errorf("a sidecar %+v", p.DooD)
			case tt.dood != "" && (p.DooD == nil || p.DooD.Privileged != (tt.dood == "privileged") || !reflect.DeepEqual(p.DooD.AddressPools, pools)):
				t.Errorf("sidecar %+v, want a %s one with the pools", p.DooD, tt.dood)
			}
			if tt.dood != "" {
				var target string
				for _, m := range p.Mounts {
					if m.Kind == MountDockerSocket {
						target = m.Target
					}
				}
				if want := []podinit.Link{{Path: target, Target: DockerSocket}}; target == "" || !reflect.DeepEqual(p.PID1.Links, want) {
					t.Errorf("links %+v, want %+v", p.PID1.Links, want)
				}
				// The sidecar is privileged, never the dev container.
				if p.Security.Privileged || p.PID1.Privileged {
					t.Errorf("the dev container is privileged for a sidecar's sake")
				}
			} else if len(p.PID1.Links) > 0 {
				t.Errorf("links %+v without a sidecar", p.PID1.Links)
			}
			if tt.dind != (p.PID1.Docker != nil) {
				t.Errorf("pid1 docker %+v, want one: %v", p.PID1.Docker, tt.dind)
			} else if tt.dind && !reflect.DeepEqual(p.PID1.Docker.AddressPools, []podinit.AddressPool{{Base: "10.200.0.0/16", Size: 24}}) {
				t.Errorf("pid1 docker pools %+v", p.PID1.Docker.AddressPools)
			}
			if p.RuntimeClassName != tt.runtime {
				t.Errorf("runtime class %q, want %q", p.RuntimeClassName, tt.runtime)
			}
			if w := strings.Join(p.Warnings, "\n"); tt.warn != "" && !strings.Contains(w, tt.warn) {
				t.Errorf("warnings %q, want one about %q", w, tt.warn)
			}
			checkPID1Boots(t, tt.name, res)
		})
	}
}

// The pod-local warning comes with every sidecar, once, naming each socket.
func TestDooDWarnsOnce(t *testing.T) {
	in := baseInput([]string{"--mount", dockerSockBind, "--mount", "type=bind,source=/var/run/docker.sock,target=/var/run/docker.sock"}, nil, nil)
	local(&in)
	res, err := Translate(in)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, w := range res.Pod.Warnings {
		if strings.Contains(w, "pod-local") {
			found = append(found, w)
		}
	}
	if len(found) != 1 || !strings.HasPrefix(found[0], "/var/run/docker-host.sock and /var/run/docker.sock: ") {
		t.Fatalf("warnings %q", res.Pod.Warnings)
	}
	if len(res.Pod.PID1.Links) != 2 {
		t.Fatalf("links %+v", res.Pod.PID1.Links)
	}
}

func TestKubeMinor(t *testing.T) {
	for in, want := range map[string]int{"v1.29.0": 29, "1.37.0": 37, "v1.30.5-gke.1014001": 30, "v1.28": 28, " v1.31.2+k3s1 ": 31} {
		if got, ok := kubeMinor(in); !ok || got != want {
			t.Errorf("kubeMinor(%q) = %d, %v; want %d", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "v2.0.0", "v1.", "latest"} {
		if got, ok := kubeMinor(bad); ok {
			t.Errorf("kubeMinor(%q) = %d, want none", bad, got)
		}
	}
}
