//go:build !nok8s

package podspec

import (
	"encoding/json"
	"io/fs"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/mounts/hostbind"
	"github.com/BenjaminBenetti/fleet-man/internal/podinit"
	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
	"github.com/BenjaminBenetti/fleet-man/internal/podshare"
)

// hostBindArgs bind a daemon-host folder read-write and a file read-only,
// name two shared volumes (one by a subpath), and mount a custom mount's
// chosen host folder.
var hostBindArgs = []string{
	"-v", "/home/dev/host/aws:/home/vscode/.aws",
	"--mount", "type=bind,source=/home/dev/host/gitconfig,target=/home/vscode/.gitconfig,readonly",
	"-v", "cache:/cache",
	"--mount", "type=volume,source=cache,target=/cache-part,volume-subpath=sub",
	"-v", "nocopy:/nocopy:nocopy",
}

func withHostFolder(in *Input) {
	in.FleetMounts = append(in.FleetMounts, backend.Mount{
		LocalPath: "/home/dev/host/aws", ContainerPath: "/opt/data",
		Scope: backend.MountScopeHostFolder, SubPath: hostbind.Name("/home/dev/host/aws"),
	})
}

// TestRenderHostBindsAndNamedVolumes renders what a run keeps on the fleet's
// share, on a share and on the copy tier: a bound folder's copy at its
// target, a bound file's copy's folder at hostbind.FileDir with the target
// linked to the file in it, a custom mount's host folder like a bound folder,
// and each named volume in the tree's volumes folder (never under roots/, the
// synced folders); the layout lists each copy's folder (a folder's with its
// host mode) and each volume.
func TestRenderHostBindsAndNamedVolumes(t *testing.T) {
	aws, git := hostbind.Name("/home/dev/host/aws"), hostbind.Name("/home/dev/host/gitconfig")
	cache, nocopy := SharedVolumeSubPath("cache"), SharedVolumeSubPath("nocopy")
	for _, tier := range []struct{ name, claim, vol, prefix string }{
		{"share", "fs-fleet-man-abcdefgh-share", "share", ""},
		{"copy", "", "disk", "sync/"},
	} {
		t.Run(tier.name, func(t *testing.T) {
			in := baseInput(hostBindArgs, nil, nil)
			local(&in)
			withHostFolder(&in)
			res, err := Translate(in)
			if err != nil {
				t.Fatal(err)
			}
			if !NeedsShare(&res.Pod) {
				t.Fatal("NeedsShare is false for a run with host binds and named volumes")
			}
			r := testRenderInput(in)
			r.Share = testShare(in, tier.claim)
			o, err := Render(res, r)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			checkObjects(t, res, r, o)
			dev := devOf(o)
			for target, want := range map[string]corev1.VolumeMount{
				"/home/vscode/.aws":   {SubPath: tier.prefix + "roots/" + aws},
				"/opt/data":           {SubPath: tier.prefix + "roots/" + aws},
				hostbind.FileDir(git): {SubPath: tier.prefix + "roots/" + git, ReadOnly: true},
				"/cache":              {SubPath: tier.prefix + cache},
				"/cache-part":         {SubPath: tier.prefix + cache + "/sub"},
				"/nocopy":             {SubPath: tier.prefix + nocopy},
			} {
				want.Name, want.MountPath = tier.vol, target
				if got, ok := mountAt(dev, target); !ok || got != want {
					t.Errorf("mount at %s = %+v, want %+v", target, got, want)
				}
			}
			if _, ok := mountAt(dev, "/home/vscode/.gitconfig"); ok {
				t.Error("a bound file's target is a mount; it should be a link to its copy")
			}
			wantLink := podinit.Link{Path: "/home/vscode/.gitconfig", Target: hostbind.FileDir(git) + "/gitconfig"}
			if links := res.Pod.PID1.Links; len(links) != 1 || links[0] != wantLink {
				t.Errorf("links %+v, want %+v", links, wantLink)
			}

			var l podshare.Layout
			if err := json.Unmarshal(o.SpecSecret.Data[podshare.LayoutFile], &l); err != nil {
				t.Fatal(err)
			}
			modes := map[string]uint32{}
			for _, e := range l.Entries {
				if e.Dir {
					modes[e.Path] = e.Mode
				}
			}
			if modes[aws] != 0o700 || modes[git] != 0o755 {
				t.Errorf("layout folders %v, want %s 0700 (the host's) and %s 0755", modes, aws, git)
			}
			want := []string{sharedVolumeID(cache), sharedVolumeID(nocopy)}
			slices.Sort(want)
			if !slices.Equal(l.Volumes, want) {
				t.Errorf("layout volumes %v, want %v", l.Volumes, want)
			}

			spec := SeedSpec(res, o)
			got := map[string]podseed.Volume{}
			for _, v := range spec.SharedVolumes {
				got[v.Name] = v
			}
			if len(got) != 2 || got[sharedVolumeID(cache)] != (podseed.Volume{Name: sharedVolumeID(cache), Target: "/cache", CopyUp: true}) ||
				got[sharedVolumeID(nocopy)] != (podseed.Volume{Name: sharedVolumeID(nocopy), Target: "/nocopy"}) {
				t.Errorf("seed shared volumes %+v", spec.SharedVolumes)
			}
			if !SharedVolumes(res) {
				t.Error("SharedVolumes is false")
			}
		})
	}
}

// TestTranslateRefusesBindsOfFleetsOwnState: a bind of fleet's own state on
// the daemon host, of a folder holding it, or of a file in it, and a custom
// mount host folder overlapping it, are refused: the sync would copy the
// daemon's files into the pod.
func TestTranslateRefusesBindsOfFleetsOwnState(t *testing.T) {
	host := fakeFS{}
	for k, v := range baseHost {
		host[k] = v
	}
	host["/home/dev/.fleet"] = fs.ModeDir | 0o700
	host["/home/dev/.fleet/kube_clusters.json"] = 0o600
	for name, edit := range map[string]func(*Input){
		"the state":    func(in *Input) { in.Capture.Args = withArgs(in, "-v", "/home/dev/.fleet:/x") },
		"its parent":   func(in *Input) { in.Capture.Args = withArgs(in, "-v", "/home/dev:/x") },
		"a file in it": func(in *Input) { in.Capture.Args = withArgs(in, "-v", "/home/dev/.fleet/kube_clusters.json:/x") },
		"a host folder": func(in *Input) {
			in.FleetMounts = []backend.Mount{{LocalPath: "/home/dev/.fleet", ContainerPath: "/x", Scope: backend.MountScopeHostFolder}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			in := baseInput(nil, nil, nil)
			in.Host, in.FleetDir = host, "/home/dev/.fleet"
			edit(&in)
			_, err := Translate(in)
			if rs := Refusals(err); len(rs) != 1 || rs[0].Code != CodeHostMount {
				t.Fatalf("Translate = %v, want one %s", err, CodeHostMount)
			}
		})
	}
	in := baseInput([]string{"-v", "/home/dev/host/aws:/x"}, nil, nil)
	in.FleetDir = "/home/dev/.fleet"
	if _, err := Translate(in); err != nil {
		t.Errorf("a bind beside fleet's state: %v", err)
	}
}

// withArgs is in's run with extra options before its image.
func withArgs(in *Input, extra ...string) []string {
	args := in.Capture.Args
	for i, a := range args {
		if a == baseImage {
			return append(append(append([]string(nil), args[:i]...), extra...), args[i:]...)
		}
	}
	return append(args, extra...)
}
