package podspec

import (
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/kubeshim"
)

// admissionView is what of a translation admission decides on: what a
// preflight must get right.
type admissionView struct {
	Security    Security
	DebugGrants []string
	HostNetwork bool
	HostIPC     bool
	HostPID     bool
	DooD        bool
	Resources   Resources
	DiskSize    string
	Runtime     string
	Sysctls     []Sysctl
	Mounts      []string
	Refusals    []string
}

func viewOf(res *Result, err error) admissionView {
	if err != nil {
		var v admissionView
		for _, r := range Refusals(err) {
			v.Refusals = append(v.Refusals, r.Code)
		}
		return v
	}
	p := res.Pod
	v := admissionView{Security: p.Security, DebugGrants: p.DebugGrants, HostNetwork: p.HostNetwork, HostIPC: p.HostIPC, HostPID: p.HostPID,
		DooD: p.DooD, Resources: p.Resources, DiskSize: p.DiskSize, Runtime: p.RuntimeClassName, Sysctls: p.Sysctls}
	for _, m := range p.Mounts {
		// The image's VOLUMEs are unknown before the build; they are
		// anonymous volumes on the instance disk, which admission does not see.
		if m.Anonymous && m.Kind == MountVolume && strings.HasPrefix(m.SubPath, "vol/anon-") && !m.ReadOnly && m.Source == "" {
			continue
		}
		v.Mounts = append(v.Mounts, string(m.Kind)+" "+m.Target)
	}
	return v
}

// For every recorded fixture, the preflight translates to what the real
// capture does, as far as admission sees.
func TestPreflightMatchesTheCapture(t *testing.T) {
	for _, fx := range captureFixtures {
		rc := loadRecorded(t, fx.name)
		for _, p := range profiles {
			t.Run(fx.name+"/"+p.name, func(t *testing.T) {
				real := rc.input()
				p.in(&real)
				pre := real
				capture, err := PreflightCapture(rc.Merged, rc.WorkspaceDir, rc.WorkspaceFolder)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := capture.Run(); err != nil {
					t.Fatalf("the preflight capture does not parse: %v", err)
				}
				pre.Capture, pre.Image = capture, kubeshim.ImageConfig{}
				want := viewOf(Translate(real))
				got := viewOf(Translate(pre))
				if jsonString(got) != jsonString(want) {
					t.Errorf("preflight\n  %s\nreal capture\n  %s", jsonString(got), jsonString(want))
				}
			})
		}
	}
}

func TestPreflightCapture(t *testing.T) {
	merged := []byte(`{"privileged":true,"init":true,"capAdd":["SYS_PTRACE"],"securityOpt":["seccomp=unconfined"],"containerUser":"vscode",
		"runArgs":["--env-file","/ws/.env","-e","A=1","--label","x=y","--network=host","--shm-size","1g"],
		"mounts":["source=cache-${devcontainerId},target=/cache,type=volume",{"type":"tmpfs","target":"/scratch"}],
		"configFilePath":{"fsPath":"/ws/.devcontainer/devcontainer.json"}}`)
	c, err := PreflightCapture(merged, "/ws", "/workspaces/ws")
	if err != nil {
		t.Fatal(err)
	}
	run, err := c.Run()
	if err != nil {
		t.Fatal(err)
	}
	for _, dropped := range []string{"env", "env-file", "label-file"} {
		if _, ok := run.Value(dropped); ok {
			t.Errorf("the preflight keeps --%s", dropped)
		}
	}
	if !run.Bool("privileged", false) || !run.Bool("init", false) || run.Values("network")[0] != "host" || run.Values("shm-size")[0] != "1g" || run.Values("user")[0] != "vscode" {
		t.Errorf("preflight run %+v", run.Flags)
	}
	mounts := strings.Join(run.Values("mount"), " | ")
	id := devcontainerID(c.Labels)
	if !strings.Contains(mounts, "type=bind,source=/ws,target=/workspaces/ws") || !strings.Contains(mounts, "source=cache-"+id+",") || !strings.Contains(mounts, "type=tmpfs,target=/scratch") {
		t.Errorf("mounts %s", mounts)
	}
	if run.Image != PreflightImage {
		t.Errorf("image %s", run.Image)
	}
	// A group named, not numbered, is the built image's to resolve: the
	// preflight leaves it to the real capture's admission.
	c, err = PreflightCapture([]byte(`{"runArgs":["--group-add","docker","--group-add=999"]}`), "/ws", "/workspaces/ws")
	if err != nil {
		t.Fatal(err)
	}
	if run, err = c.Run(); err != nil || strings.Join(run.Values("group-add"), ",") != "999" {
		t.Errorf("--group-add in the preflight: %v (%v)", run.Values("group-add"), err)
	}
	// A runArgs option docker run has not: the code the real capture would
	// fail with.
	if _, err := PreflightCapture([]byte(`{"runArgs":["--no-such-option"]}`), "/ws", "/workspaces/ws"); err == nil || !strings.HasPrefix(err.Error(), kubeshim.UnsupportedFlag("no-such-option")+": ") {
		t.Errorf("unknown runArgs: %v", err)
	}
	if _, err := PreflightCapture([]byte(`{"mounts":[42]}`), "/ws", "/w"); err == nil {
		t.Error("a mount that is neither string nor object")
	}
}
