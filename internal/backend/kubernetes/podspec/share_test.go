//go:build !nok8s

package podspec

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/topology"
	"github.com/BenjaminBenetti/fleet-man/internal/podshare"
)

// testShare is the share of in's fleet mounts, on claim ("" the copy tier),
// laid out for the share uid 1000.
func testShare(in Input, claim string) *Share {
	l := podshare.Layout{UID: 1000, GID: 1000}
	for _, m := range in.FleetMounts {
		if m.Scope == backend.MountScopeFleetShared {
			l.Entries = append(l.Entries, podshare.Entry{Path: m.SubPath, Dir: true, Mode: 0o777})
		}
	}
	if len(l.Entries) > 0 {
		l.Entries = append(l.Entries, podshare.Entry{Path: "files/.claude.json", Mode: 0o666, Seed: "{}"})
	}
	return &Share{Claim: claim, Layout: l}
}

// shareMounts are a fleet's mounts with Claude Code's and a custom mount on,
// one read-only.
func shareMounts(in *Input) {
	in.FleetMounts = []backend.Mount{
		{LocalPath: "/h/.fleet/workspaces/f/.claude", ContainerPath: "/home/vscode/.claude", Scope: backend.MountScopeFleetShared, SubPath: ".claude"},
		{LocalPath: "/h/.fleet/workspaces/f/files", ContainerPath: "/fleet-mounts/files", Scope: backend.MountScopeFleetShared, SubPath: "files"},
		{LocalPath: "/h/.fleet/workspaces/f/.mnt/opt/data", ContainerPath: "/opt/data", Scope: backend.MountScopeFleetShared, SubPath: ".mnt/opt/data", ReadOnly: true},
	}
}

// TestRenderShare renders a fleet's mounts on its share and on the copy
// tier (goldens testdata/render/share.*.json), and holds both to the tree's
// layout: each fleet mount the subPath roots/<SubPath> of the tree's volume,
// the tree's root at podshare.MountDir, and the share-layout init container
// first, replaying the layout file the spec Secret holds.
func TestRenderShare(t *testing.T) {
	for _, tier := range []struct {
		name  string
		claim string
	}{{"share", "fs-fleet-man-abcdefgh-share"}, {"copy", ""}} {
		t.Run(tier.name, func(t *testing.T) {
			in := baseInput(nil, nil, nil)
			local(&in)
			shareMounts(&in)
			r := testRenderInput(in)
			r.Share = testShare(in, tier.claim)
			res, err := Translate(in)
			if err != nil {
				t.Fatal(err)
			}
			o, err := Render(res, r)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			checkObjects(t, res, r, o)
			checkGolden(t, filepath.Join("render", "share."+tier.name+".json"), mustIndent(t, renderGolden(o, nil)))

			spec := o.StatefulSet.Spec.Template.Spec
			vol, prefix := "share", ""
			if tier.claim == "" {
				vol, prefix = "disk", "sync/"
			}
			dev := spec.Containers[0]
			for target, want := range map[string]corev1.VolumeMount{
				"/home/vscode/.claude":  {Name: vol, MountPath: "/home/vscode/.claude", SubPath: prefix + "roots/.claude"},
				"/fleet-mounts/files":   {Name: vol, MountPath: "/fleet-mounts/files", SubPath: prefix + "roots/files"},
				"/opt/data":             {Name: vol, MountPath: "/opt/data", SubPath: prefix + "roots/.mnt/opt/data", ReadOnly: true},
				podshare.MountDir:       {Name: vol, MountPath: podshare.MountDir, SubPath: strings.TrimSuffix(prefix, "/")},
				"/opt/fleet/run":        {Name: "run", MountPath: "/opt/fleet/run"},
				"/fleet-mounts/control": {Name: "control", MountPath: "/fleet-mounts/control"},
			} {
				if got, ok := mountAt(dev, target); !ok || !reflect.DeepEqual(got, want) {
					t.Errorf("dev mount at %s = %+v, want %+v", target, got, want)
				}
			}
			var claims []string
			for _, v := range spec.Volumes {
				if v.PersistentVolumeClaim != nil {
					claims = append(claims, v.PersistentVolumeClaim.ClaimName)
				}
			}
			want := []string{r.Name}
			if tier.claim != "" {
				want = append(want, tier.claim)
			}
			if !reflect.DeepEqual(claims, want) {
				t.Errorf("the pod's claims are %v, want %v", claims, want)
			}
			init := spec.InitContainers[0]
			for target, want := range map[string]corev1.VolumeMount{
				"/opt/fleet":      {Name: "disk", MountPath: "/opt/fleet", SubPath: "fleet", ReadOnly: true},
				"/opt/fleet/spec": {Name: "spec", MountPath: "/opt/fleet/spec", ReadOnly: true},
				"/opt/fleet/run":  {Name: "run", MountPath: "/opt/fleet/run"},
			} {
				if got, ok := mountAt(init, target); !ok || !reflect.DeepEqual(got, want) {
					t.Errorf("init mount at %s = %+v, want %+v", target, got, want)
				}
			}
			// The tree's volume whole: a missing subPath the kubelet made
			// would be root's.
			if got, ok := mountAt(init, podshare.InitDir); !ok || got.Name != vol || got.SubPath != "" {
				t.Errorf("the init container has the tree's volume at %+v", got)
			}
			if _, ok := mountAt(init, podshare.MountDir); ok {
				t.Error("the init container mounts the tree by its subPath")
			}
			tree := podshare.InitDir
			if tier.claim == "" {
				tree += "/sync"
			}
			if !reflect.DeepEqual(init.Command, ShareInitCommand(tree)) || init.Command[len(init.Command)-1] != tree {
				t.Errorf("the init container runs %q, want it to lay out %s", init.Command, tree)
			}
			if init.Image != dev.Image || init.SecurityContext == nil || *init.SecurityContext.RunAsUser != 0 {
				t.Errorf("the init container runs %s as %+v, want the dev image as root", init.Image, init.SecurityContext)
			}
			var l podshare.Layout
			if err := json.Unmarshal(o.SpecSecret.Data[podshare.LayoutFile], &l); err != nil || l.V != podshare.LayoutVersion || l.UID != 1000 || len(l.Entries) != 4 {
				t.Errorf("the spec Secret's layout = %+v (%v)", l, err)
			}
			// The seed binds no share unless the backend asks it to (the
			// first instance of a fleet in single-node mode).
			for _, v := range o.Seed.Job.Spec.Template.Spec.Volumes {
				if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName != r.Name {
					t.Errorf("the seed mounts %s", v.PersistentVolumeClaim.ClaimName)
				}
			}
		})
	}
}

// TestRenderShareRefusesABadLayout: a layout that would leave the tree (or
// set an id bit) never reaches the cluster.
func TestRenderShareRefusesABadLayout(t *testing.T) {
	in := baseInput(nil, nil, nil)
	shareMounts(&in)
	r := testRenderInput(in)
	r.Share.Layout.Entries = append(r.Share.Layout.Entries, podshare.Entry{Path: "../escape", Dir: true})
	res, err := Translate(in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Render(res, r); err == nil || !strings.Contains(err.Error(), "layout") {
		t.Fatalf("Render = %v, want the layout refused", err)
	}
}

// TestRenderShareRunsAsThePodsUIDWithoutRoot: in a namespace without root
// the init container runs as the pod's uid, like the rest of the pod.
func TestRenderShareRunsAsThePodsUIDWithoutRoot(t *testing.T) {
	in := baseInput(nil, nil, nil)
	shareMounts(&in)
	r := testRenderInput(in)
	uid, gid := int64(1000), int64(1000)
	r.Identity = Identity{NonRoot: true, UID: &uid, GID: &gid}
	res, err := Translate(in)
	if err != nil {
		t.Fatal(err)
	}
	o, err := Render(res, r)
	if err != nil {
		t.Fatal(err)
	}
	sc := o.StatefulSet.Spec.Template.Spec.InitContainers[0].SecurityContext
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || *sc.RunAsUser != 1000 || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Fatalf("the init container's security context = %+v", sc)
	}
}

// TestRenderSeedBindsTheShare: the first instance's seed mounts the fleet's
// share beside its disk (JobInput.Claims), so a WaitForFirstConsumer class
// binds both on the node the seed's pod lands on.
func TestRenderSeedBindsTheShare(t *testing.T) {
	in := baseInput(nil, nil, nil)
	shareMounts(&in)
	r := testRenderInput(in)
	claim := names.FleetObject(testFleet, r.FleetHash, names.SuffixShare)
	r.Seed.Claims = []JobClaim{{ClaimName: claim, Path: "/fleet-share"}}
	res, err := Translate(in)
	if err != nil {
		t.Fatal(err)
	}
	o, err := Render(res, r)
	if err != nil {
		t.Fatal(err)
	}
	spec := o.Seed.Job.Spec.Template.Spec
	var found bool
	for _, v := range spec.Volumes {
		found = found || (v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == claim)
	}
	if m, ok := mountAt(spec.Containers[0], "/fleet-share"); !found || !ok {
		t.Fatalf("the seed does not mount the share (volume %v, mount %+v)", found, m)
	}
}

// TestRenderPlacesByTheShare: in single-node mode the instance pod takes the
// share volume's topology and goes beside the running holders; the seed
// takes only the topology (it holds nothing, and with every instance
// stopped no holder exists for it); a clone's Jobs drop the holders' term
// they copy from the source.
func TestRenderPlacesByTheShare(t *testing.T) {
	in := baseInput(nil, nil, nil)
	shareMounts(&in)
	r := testRenderInput(in)
	zone := corev1.NodeSelectorTerm{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{"eu-1a"}}}}
	r.Placement = &topology.Placement{Mode: topology.SingleNode, Terms: []corev1.NodeSelectorTerm{zone}, ShareGroup: r.FleetHash}
	res, err := Translate(in)
	if err != nil {
		t.Fatal(err)
	}
	o, err := Render(res, r)
	if err != nil {
		t.Fatal(err)
	}
	pod := o.StatefulSet.Spec.Template.Spec.Affinity
	if pod == nil || pod.NodeAffinity == nil || !reflect.DeepEqual(pod.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms, []corev1.NodeSelectorTerm{zone}) {
		t.Fatalf("the instance pod's node affinity = %+v, want the share's zone", pod)
	}
	if pod.PodAffinity == nil || len(pod.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution) != 1 {
		t.Fatalf("the instance pod has no affinity to the share's holders: %+v", pod.PodAffinity)
	}
	term := pod.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0]
	if !selects(term.LabelSelector, o.StatefulSet.Spec.Template.Labels) || term.TopologyKey != corev1.LabelHostname {
		t.Fatalf("the holders' term %+v does not select the instance pod itself (labels %v): the first could never schedule", term, o.StatefulSet.Spec.Template.Labels)
	}
	seed := o.Seed.Job.Spec.Template.Spec.Affinity
	if seed == nil || seed.PodAffinity != nil || !reflect.DeepEqual(seed.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms, []corev1.NodeSelectorTerm{zone}) {
		t.Fatalf("the seed's affinity = %+v, want the share's zone alone", seed)
	}
	if selects(term.LabelSelector, o.Seed.Job.Spec.Template.Labels) {
		t.Fatal("the holders' term selects the seed's pod")
	}
	var spec corev1.PodSpec
	fromPlacement(&spec, &o.StatefulSet.Spec.Template.Spec)
	if spec.Affinity == nil || spec.Affinity.PodAffinity != nil || spec.Affinity.NodeAffinity == nil {
		t.Fatalf("a Job placed from the instance pod has affinity %+v, want its node terms without the holders'", spec.Affinity)
	}
}

func TestSELinuxMountSupported(t *testing.T) {
	for v, want := range map[string]bool{"v1.37.0": true, "v1.33.1": true, "v1.32.9": false, "": false, "v2.0": false} {
		if got := SELinuxMountSupported(v); got != want {
			t.Errorf("SELinuxMountSupported(%q) = %v, want %v", v, got, want)
		}
	}
}
