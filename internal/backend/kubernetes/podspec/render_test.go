//go:build !nok8s

package podspec

import (
	"encoding/json"
	"math/rand"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/control"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/jobboot"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeshim"
	"github.com/BenjaminBenetti/fleet-man/internal/podinit"
	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
)

// The render goldens: each recorded fixture's translation (TestFixtureGoldens)
// rendered into its Kubernetes objects for a local and a managed cluster,
// into testdata/render.

const (
	testDaemonID  = "0123456789abcdef0123456789abcdef"
	testFleet     = "fleet-man"
	testInstance  = "web"
	testImageID   = "sha256:" + "1111111111111111111111111111111111111111111111111111111111111111"
	testDigest    = "sha256:" + "2222222222222222222222222222222222222222222222222222222222222222"
	testToolsDir  = "/fleet-tools"
	testDiskPath  = "/fleet-pvc"
	testSeedShell = "i=0; until [ -f /fleet-tools/.ready ]; do i=$((i+1)); [ $i -gt 600 ] && exit 70; sleep 1; done; exec /fleet-tools/fleet pod-job --spec /fleet-tools/job.json"
)

var testCreatedAt = time.Date(2026, 10, 10, 12, 0, 0, 123456789, time.UTC)

// testRenderInput is the render input of in's connection: a node-loaded image
// on a local cluster, a registry-delivered one elsewhere.
func testRenderInput(in Input) RenderInput {
	c := in.Cluster
	key := names.InstanceKey(testDaemonID, testFleet, testInstance, testCreatedAt)
	sts := names.Instance(testFleet, testInstance, key)
	r := RenderInput{
		Cluster:      c,
		Namespace:    c.Namespace,
		Name:         sts,
		Key:          key,
		Owner:        names.Owner(testDaemonID),
		ClusterID:    c.ID,
		FleetName:    testFleet,
		FleetHash:    names.FleetHash(testDaemonID, c.ID, testFleet),
		InstanceName: testInstance,
		Gen:          1,
		CreatedAt:    testCreatedAt,
		Replicas:     1,
		Root:         RootLayout{Dirs: []string{"etc", "home", "opt", "root", "srv", "tmp", "usr", "var"}, Extra: []string{"workspaces"}},
		Seed: &JobInput{
			Name:           names.SeedJob(sts, 1),
			Command:        []string{"sh", "-c", testSeedShell},
			ToolsDir:       testToolsDir,
			DiskPath:       testDiskPath,
			ActiveDeadline: 16 * time.Minute,
		},
	}
	if in.Facts.Local {
		r.Image = ImageRef{Ref: "fleet.local/0123456789ab/devcontainer:111111111111", PullPolicy: corev1.PullNever, ID: testImageID}
	} else {
		r.Image = ImageRef{Ref: "registry.example.com/fleet/01234567/devcontainer@" + testDigest, PullSecrets: []string{"fleet-pull-0123456789ab"}, ID: testDigest}
		r.StorageClass = "premium-rwo"
		r.SELinuxMount = true
	}
	return r
}

// goldenRender is a render as a golden file shows it. Secret values are
// left out (TestFixtureGoldens holds them); their keys and sizes stay.
type goldenRender struct {
	Error            string                        `json:"error,omitempty"`
	Refusals         []string                      `json:"refusals,omitempty"`
	Warnings         []string                      `json:"warnings,omitempty"`
	Disk             *corev1.PersistentVolumeClaim `json:"disk,omitempty"`
	SpecSecret       *goldenSecret                 `json:"specSecret,omitempty"`
	EnvSecret        *goldenSecret                 `json:"envSecret,omitempty"`
	StatefulSet      *appsv1.StatefulSet           `json:"statefulSet,omitempty"`
	DisruptionBudget *policyv1.PodDisruptionBudget `json:"disruptionBudget,omitempty"`
	Seed             *batchv1.Job                  `json:"seed,omitempty"`
}

type goldenSecret struct {
	Meta metav1.ObjectMeta `json:"metadata"`
	// Data maps each key to its value's size.
	Data map[string]string `json:"data"`
}

func secretGolden(s *corev1.Secret) *goldenSecret {
	g := &goldenSecret{Meta: s.ObjectMeta, Data: map[string]string{}}
	for k, v := range s.Data {
		g.Data[k] = strconv.Itoa(len(v)) + " bytes"
	}
	return g
}

func renderGolden(o *Objects, err error) goldenRender {
	if err != nil {
		g := goldenRender{Error: err.Error()}
		for _, r := range Refusals(err) {
			g.Refusals = append(g.Refusals, r.Code)
		}
		return g
	}
	g := goldenRender{Warnings: o.Warnings, Disk: o.Disk, SpecSecret: secretGolden(o.SpecSecret), EnvSecret: secretGolden(o.EnvSecret),
		StatefulSet: o.StatefulSet, DisruptionBudget: o.DisruptionBudget}
	if o.Seed != nil {
		g.Seed = o.Seed.Job
	}
	return g
}

func TestRenderGoldens(t *testing.T) {
	for _, fx := range captureFixtures {
		rc := loadRecorded(t, fx.name)
		for _, p := range profiles {
			t.Run(fx.name+"/"+p.name, func(t *testing.T) {
				in := rc.input()
				p.in(&in)
				res, err := Translate(in)
				if err != nil {
					t.Skipf("the fixture does not translate here (%v); TestFixtureGoldens has it", err)
				}
				r := testRenderInput(in)
				o, err := Render(res, r)
				if err == nil {
					checkObjects(t, res, r, o)
				}
				checkGolden(t, filepath.Join("render", fx.name+"."+p.name+".json"), mustIndent(t, renderGolden(o, err)))
			})
		}
	}
}

// checkObjects holds a render to what every render must be.
func checkObjects(t *testing.T, res *Result, r RenderInput, o *Objects) {
	t.Helper()
	opts := HardenOptions{ShareGroup: r.FleetHash, SELinuxLevel: r.SELinuxLevel, SELinuxMount: r.SELinuxMount, AllowHostAccess: true}
	tmpl := o.StatefulSet.Spec.Template
	if v := CheckHardened(tmpl.ObjectMeta, tmpl.Spec, opts); len(v) > 0 {
		t.Errorf("the instance pod is not hardened: %v", v)
	}
	if o.Seed != nil {
		opts.AllowHostAccess = false
		jt := o.Seed.Job.Spec.Template
		if v := CheckHardened(jt.ObjectMeta, jt.Spec, opts); len(v) > 0 {
			t.Errorf("the seed pod is not hardened: %v", v)
		}
		if jt.Labels[names.LabelRole] == string(names.RoleInstance) || selects(o.StatefulSet.Spec.Selector, jt.Labels) {
			t.Errorf("the StatefulSet's selector matches the seed pod: %v", jt.Labels)
		}
	}
	if !selects(o.StatefulSet.Spec.Selector, tmpl.Labels) {
		t.Errorf("the StatefulSet's selector %v does not match its pods %v", o.StatefulSet.Spec.Selector, tmpl.Labels)
	}
	if got, want := o.StatefulSet.Annotations[names.AnnotationSpecHash], SpecHash(&tmpl.Spec); got != want {
		t.Errorf("spec-hash %s, want %s", got, want)
	}
	for _, obj := range []metav1.Object{o.Disk, o.SpecSecret, o.EnvSecret, o.StatefulSet} {
		checkLabels(t, obj.GetName(), obj.GetLabels(), r)
	}
	checkLabels(t, "pod", tmpl.Labels, r)
	// The commands are fleet's own.
	if dev := tmpl.Spec.Containers[0]; jsonString(dev.Command) != jsonString(Command()) || len(dev.Args) > 0 || len(tmpl.Spec.Containers) != 1 || len(tmpl.Spec.InitContainers) > 0 {
		t.Errorf("the instance pod runs %q %q", dev.Command, dev.Args)
	}
	if o.Seed != nil {
		if c := o.Seed.Job.Spec.Template.Spec.Containers[0]; jsonString(c.Command) != jsonString(r.Seed.Command) || len(c.Args) > 0 || len(c.Env) > 0 {
			t.Errorf("the seed runs %q %q with env %v", c.Command, c.Args, c.Env)
		}
	}
	// Every env value comes from the env Secret, under a key it has, but
	// the pod's uid for pod-init, which comes last.
	env := tmpl.Spec.Containers[0].Env
	if last := env[len(env)-1]; last.Name != podinit.PodUIDEnv || last.ValueFrom.FieldRef.FieldPath != "metadata.uid" {
		t.Errorf("the last env is %+v, want the pod's uid", last)
	}
	for _, e := range env[:len(env)-1] {
		ref := e.ValueFrom
		if e.Value != "" || ref == nil || ref.SecretKeyRef == nil || ref.SecretKeyRef.Name != o.EnvSecret.Name {
			t.Errorf("env %s does not come from the env Secret: %+v", e.Name, e)
			continue
		}
		if _, ok := o.EnvSecret.Data[ref.SecretKeyRef.Key]; !ok {
			t.Errorf("env %s names key %s, which the env Secret lacks", e.Name, ref.SecretKeyRef.Key)
		}
	}
	for name := range res.Secrets.Spec {
		if _, ok := o.SpecSecret.Data[name]; !ok {
			t.Errorf("the spec Secret lacks %s", name)
		}
	}
}

func checkLabels(t *testing.T, what string, l map[string]string, r RenderInput) {
	t.Helper()
	for k, v := range map[string]string{
		names.LabelManagedBy: names.ManagedBy, names.LabelOwner: r.Owner, names.LabelCluster: r.ClusterID,
		names.LabelFleet: r.FleetHash, names.LabelKey: r.Key, names.LabelShareGroup: r.FleetHash,
	} {
		if l[k] != v {
			t.Errorf("%s: label %s = %q, want %q", what, k, l[k], v)
		}
	}
	if l[names.LabelRole] == "" {
		t.Errorf("%s has no role label", what)
	}
}

func selects(sel *metav1.LabelSelector, labels map[string]string) bool {
	for k, v := range sel.MatchLabels {
		if labels[k] != v {
			return false
		}
	}
	return len(sel.MatchExpressions) == 0
}

// renderOf translates and renders a run given as extra docker run options
// (the option cases' base run), on a local cluster.
func renderOf(t *testing.T, args []string, edit func(*Input, *RenderInput)) (*Objects, error) {
	t.Helper()
	in := baseInput(args, nil, nil)
	local(&in)
	r := testRenderInput(in)
	if edit != nil {
		edit(&in, &r)
	}
	res, err := Translate(in)
	if err != nil {
		t.Fatalf("Translate(%v): %v", args, err)
	}
	return Render(res, r)
}

func mustRender(t *testing.T, args []string, edit func(*Input, *RenderInput)) *Objects {
	t.Helper()
	o, err := renderOf(t, args, edit)
	if err != nil {
		t.Fatalf("Render(%v): %v", args, err)
	}
	return o
}

func devOf(o *Objects) corev1.Container { return o.StatefulSet.Spec.Template.Spec.Containers[0] }

func mountAt(c corev1.Container, target string) (corev1.VolumeMount, bool) {
	for _, m := range c.VolumeMounts {
		if m.MountPath == target {
			return m, true
		}
	}
	return corev1.VolumeMount{}, false
}

func TestRenderPersistedRoot(t *testing.T) {
	o := mustRender(t, []string{"--read-only", "--tmpfs", "/tmp:size=64m", "-v", "/home", "--mount", "type=bind,source=" + baseWS + "/sub,target=/srv/sub,readonly"}, func(in *Input, r *RenderInput) {
		r.Root.Dirs = append(r.Root.Dirs, "proc", "etc")
	})
	dev := devOf(o)
	want := map[string]struct {
		volume, subPath string
		ro              bool
	}{
		"/etc":                    {"disk", "root/etc", true},
		"/usr":                    {"disk", "root/usr", true},
		"/srv":                    {"disk", "root/srv", true},
		"/opt":                    {"disk", "root/opt", true},
		"/root":                   {"disk", "root/root", true},
		"/var":                    {"disk", "root/var", true},
		"/workspaces":             {"disk", "root-extra/workspaces", true},
		"/etc/resolv.conf":        {"disk", podseed.ResolvConfFile, false},
		"/etc/hostname":           {"disk", podseed.HostnameFile, false},
		"/opt/fleet":              {"disk", podseed.DirFleet, false},
		"/opt/fleet/spec":         {"spec", "", true},
		"/opt/fleet/run":          {"run", "", false},
		control.ContainerMountDir: {"control", "", false},
		"/workspaces/app":         {"disk", "ws", false},
		"/srv/sub":                {"disk", "ws/sub", true},
		"/tmp":                    {"tmpfs-" + sha8("/tmp"), "", false},
		"/home":                   {"disk", "vol/anon-" + sha8("/home"), false},
	}
	for target, w := range want {
		m, ok := mountAt(dev, target)
		if !ok {
			t.Errorf("no mount at %s", target)
			continue
		}
		if m.Name != w.volume || m.SubPath != w.subPath || m.ReadOnly != w.ro {
			t.Errorf("mount at %s = %s:%s ro=%v, want %s:%s ro=%v", target, m.Name, m.SubPath, m.ReadOnly, w.volume, w.subPath, w.ro)
		}
	}
	if len(dev.VolumeMounts) != len(want) {
		t.Errorf("%d mounts, want %d: %v", len(dev.VolumeMounts), len(want), dev.VolumeMounts)
	}
	// The tmpfs is memory-backed and sized; /proc is never persisted.
	for _, v := range o.StatefulSet.Spec.Template.Spec.Volumes {
		if v.Name == "tmpfs-"+sha8("/tmp") && (v.EmptyDir == nil || v.EmptyDir.Medium != corev1.StorageMediumMemory || v.EmptyDir.SizeLimit.String() != "64Mi") {
			t.Errorf("tmpfs volume %+v", v)
		}
	}
	// A run mount at a path the root would mount replaces the root's.
	o = mustRender(t, []string{"--mount", "type=bind,source=" + baseWS + "/sub,target=/etc/resolv.conf"}, nil)
	if m, _ := mountAt(devOf(o), "/etc/resolv.conf"); m.SubPath != "ws/sub" {
		t.Errorf("/etc/resolv.conf mounts %+v, want the run's", m)
	}
}

// What the instance pod runs, and where the env values come from, is
// fleet's alone: no string of the run reaches a field the kubelet expands
// ($(VAR) in command, args and env values, subPathExpr), for any option
// case or fixture.
func TestNoRunStringReachesKubeletExpansion(t *testing.T) {
	check := func(name string, o *Objects, job []string) {
		pods := []corev1.PodSpec{o.StatefulSet.Spec.Template.Spec}
		if o.Seed != nil {
			pods = append(pods, o.Seed.Job.Spec.Template.Spec)
		}
		for i, spec := range pods {
			for _, c := range append(append([]corev1.Container(nil), spec.InitContainers...), spec.Containers...) {
				want := Command()
				if i > 0 {
					want = job
				}
				if strings.Join(c.Command, "\x00") != strings.Join(want, "\x00") || len(c.Args) > 0 {
					t.Errorf("%s: container %s runs %q %q", name, c.Name, c.Command, c.Args)
				}
				for _, e := range c.Env {
					if e.Value != "" || e.ValueFrom == nil || (e.ValueFrom.SecretKeyRef == nil && e.Name != podinit.PodUIDEnv) {
						t.Errorf("%s: env %s is not a secretKeyRef", name, e.Name)
					}
				}
				if len(c.EnvFrom) > 0 {
					t.Errorf("%s: envFrom %+v", name, c.EnvFrom)
				}
				for _, m := range c.VolumeMounts {
					if m.SubPathExpr != "" {
						t.Errorf("%s: subPathExpr %s", name, m.SubPathExpr)
					}
				}
			}
		}
	}
	rendered := 0
	for _, c := range optionCases {
		in := baseInput(c.args, c.env, c.labels)
		if c.in != nil {
			c.in(&in)
		}
		res, err := Translate(in)
		if err != nil {
			continue
		}
		r := testRenderInput(in)
		o, err := Render(res, r)
		if err != nil {
			continue
		}
		rendered++
		check(c.name, o, r.Seed.Command)
		checkObjects(t, res, r, o)
	}
	if rendered < len(optionCases)/2 {
		t.Errorf("only %d of %d option cases render", rendered, len(optionCases))
	}
}

func TestRenderNonRoot(t *testing.T) {
	id := Identity{NonRoot: true, UID: ptr(int64(1000650000)), FSGroup: ptr(int64(1000650000))}
	o := mustRender(t, []string{"--cap-add", "NET_BIND_SERVICE"}, func(_ *Input, r *RenderInput) { r.Identity = id })
	for name, spec := range map[string]corev1.PodSpec{"instance": o.StatefulSet.Spec.Template.Spec, "seed": o.Seed.Job.Spec.Template.Spec} {
		psc := spec.SecurityContext
		if psc.RunAsNonRoot == nil || !*psc.RunAsNonRoot || *psc.RunAsUser != *id.UID || psc.RunAsGroup != nil || *psc.FSGroup != *id.FSGroup || *psc.FSGroupChangePolicy != corev1.FSGroupChangeOnRootMismatch {
			t.Errorf("%s: pod security %+v", name, psc)
		}
		sc := spec.Containers[0].SecurityContext
		if *sc.RunAsNonRoot != true || *sc.AllowPrivilegeEscalation || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" || (sc.Privileged != nil && *sc.Privileged) {
			t.Errorf("%s: container security %+v", name, sc)
		}
	}
	if add := devOf(o).SecurityContext.Capabilities.Add; len(add) != 1 || add[0] != "NET_BIND_SERVICE" {
		t.Errorf("non-root adds %v, want only the run's", add)
	}
	if add := o.Seed.Job.Spec.Template.Spec.Containers[0].SecurityContext.Capabilities.Add; len(add) != 0 {
		t.Errorf("the non-root seed adds %v", add)
	}
	// fsGroup follows the gid, then the uid; with neither the cluster
	// assigns it.
	for _, tt := range []struct {
		id   Identity
		want *int64
	}{
		{Identity{NonRoot: true, UID: ptr(int64(1000)), GID: ptr(int64(2000))}, ptr(int64(2000))},
		{Identity{NonRoot: true, UID: ptr(int64(1000))}, ptr(int64(1000))},
		{Identity{NonRoot: true}, nil},
		{Identity{UID: ptr(int64(1000))}, nil},
	} {
		o := mustRender(t, nil, func(_ *Input, r *RenderInput) { r.Identity = tt.id })
		if got := o.StatefulSet.Spec.Template.Spec.SecurityContext.FSGroup; jsonString(got) != jsonString(tt.want) {
			t.Errorf("%+v: fsGroup %v", tt.id, jsonString(got))
		}
	}
	// A pod without root cannot be privileged: refused by name, not left
	// to the API server's "invalid".
	if _, err := renderOf(t, []string{"--privileged"}, func(_ *Input, r *RenderInput) { r.Identity = id }); err == nil || !strings.HasPrefix(err.Error(), CodePrivileged+": ") {
		t.Errorf("privileged without root: %v", err)
	}
	// A tool Job may run as another identity than the instance.
	other := Identity{NonRoot: true, UID: ptr(int64(1000))}
	res, err := Translate(baseInput(nil, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	r := testRenderInput(baseInput(nil, nil, nil))
	job, err := RenderJob(res, r, JobInput{Name: "tool-export", Command: []string{"sh"}, ToolsDir: testToolsDir, DiskPath: testDiskPath, Identity: &other}, names.RoleTool)
	if err != nil {
		t.Fatal(err)
	}
	if sc := job.Job.Spec.Template.Spec.Containers[0].SecurityContext; *sc.RunAsUser != 1000 || !*sc.RunAsNonRoot {
		t.Errorf("tool Job security %+v", sc)
	}
	if got := job.Job.Spec.Template.Spec.Containers[0].Resources.Requests.Memory().String(); got != "2Gi" {
		t.Errorf("tool Job requests %s of memory, want the instance's", got)
	}
	// ... and ask for less than the instance.
	small := corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}}
	job, err = RenderJob(res, r, JobInput{Name: "copy-1", Command: []string{"sh"}, ToolsDir: testToolsDir, DiskPath: testDiskPath, Resources: &small}, names.RoleCopier)
	if err != nil {
		t.Fatal(err)
	}
	if got := job.Job.Spec.Template.Spec.Containers[0].Resources; jsonString(got) != jsonString(small) || job.Job.Labels[names.LabelRole] != string(names.RoleCopier) {
		t.Errorf("copier resources %s, role %s", jsonString(got), job.Job.Labels[names.LabelRole])
	}
}

func TestRenderCapabilities(t *testing.T) {
	adds := func(o *Objects) []string {
		var out []string
		if c := devOf(o).SecurityContext.Capabilities; c != nil {
			for _, a := range c.Add {
				out = append(out, string(a))
			}
		}
		return out
	}
	o := mustRender(t, []string{"--cap-drop", "MKNOD", "--cap-drop", "NET_RAW", "--cap-add", "NET_ADMIN"}, nil)
	got := strings.Join(adds(o), ",")
	if strings.Contains(got, "MKNOD") || !strings.Contains(got, "SETFCAP") || !strings.HasSuffix(got, "NET_ADMIN") {
		t.Errorf("adds %s", got)
	}
	if drop := devOf(o).SecurityContext.Capabilities.Drop; len(drop) != 2 {
		t.Errorf("drops %v", drop)
	}
	if got := adds(mustRender(t, []string{"--cap-drop", "ALL", "--cap-add", "CHOWN"}, nil)); strings.Join(got, ",") != "CHOWN" {
		t.Errorf("--cap-drop ALL adds %v", got)
	}
	o = mustRender(t, []string{"--privileged"}, nil)
	if got := adds(o); len(got) != 0 || !*devOf(o).SecurityContext.Privileged {
		t.Errorf("privileged adds %v", got)
	}
	// no-new-privileges, where Kubernetes takes it.
	o = mustRender(t, []string{"--security-opt", "no-new-privileges"}, nil)
	if ape := devOf(o).SecurityContext.AllowPrivilegeEscalation; ape == nil || *ape || len(o.Warnings) > 0 {
		t.Errorf("no-new-privileges: %v %v", ape, o.Warnings)
	}
	o = mustRender(t, []string{"--security-opt", "no-new-privileges", "--privileged"}, nil)
	if devOf(o).SecurityContext.AllowPrivilegeEscalation != nil || len(o.Warnings) != 1 || !strings.Contains(o.Warnings[0], "no-new-privileges") {
		t.Errorf("privileged: allowPrivilegeEscalation %v, warnings %v", devOf(o).SecurityContext.AllowPrivilegeEscalation, o.Warnings)
	}
	// The API server takes it beside SYS_ADMIN (it refuses only the
	// CAP_SYS_ADMIN spelling) and ALL, and docker applies it there.
	for _, args := range [][]string{{"--cap-add", "SYS_ADMIN"}, {"--cap-add", "ALL"}} {
		o = mustRender(t, append([]string{"--security-opt", "no-new-privileges"}, args...), nil)
		if ape := devOf(o).SecurityContext.AllowPrivilegeEscalation; ape == nil || *ape || len(o.Warnings) > 0 {
			t.Errorf("%v: allowPrivilegeEscalation %v, warnings %v", args, jsonString(ape), o.Warnings)
		}
	}
	// AppArmor, SELinux options (with the share-group's level), seccomp.
	o = mustRender(t, []string{"--security-opt", "apparmor=my-profile", "--security-opt", "label=type:container_engine_t"}, nil)
	sc := devOf(o).SecurityContext
	if sc.AppArmorProfile == nil || sc.AppArmorProfile.Type != corev1.AppArmorProfileTypeLocalhost || *sc.AppArmorProfile.LocalhostProfile != "my-profile" {
		t.Errorf("apparmor %+v", sc.AppArmorProfile)
	}
	if se := sc.SELinuxOptions; se == nil || se.Type != "container_engine_t" || se.Level != SELinuxLevel(testRenderInput(baseInput(nil, nil, nil)).FleetHash) {
		t.Errorf("selinux %+v", se)
	}
}

func TestRenderSeedJob(t *testing.T) {
	o := mustRender(t, []string{"--runtime", "gvisor", "--dns", "10.0.0.53", "-m", "4g"}, func(in *Input, r *RenderInput) {
		in.Facts.RuntimeClasses = []string{"gvisor"}
		in.Cluster.ServiceAccount = "fleet-pods"
		in.Cluster.Tolerations = []fleet.KubeToleration{{Key: "pool", Value: "dev", Effect: "NoSchedule"}}
		r.Seed.Claims = []JobClaim{{ClaimName: "fs-fleet-man-x-share", Path: "/fleet-share"}}
		r.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "kubernetes.io/hostname", Operator: corev1.NodeSelectorOpIn, Values: []string{"node-1"}}}}},
		}}}
	})
	job := o.Seed.Job
	if *job.Spec.BackoffLimit != 0 || *job.Spec.TTLSecondsAfterFinished != 600 || *job.Spec.ActiveDeadlineSeconds != 960 || *job.Spec.Completions != 1 {
		t.Errorf("job spec %+v", job.Spec)
	}
	js, is := job.Spec.Template.Spec, o.StatefulSet.Spec.Template.Spec
	if js.RestartPolicy != corev1.RestartPolicyNever || job.Labels[names.LabelRole] != string(names.RoleSeed) || job.Spec.Template.Labels[names.LabelRole] != string(names.RoleSeed) {
		t.Errorf("restart policy %s, role %s", js.RestartPolicy, job.Labels[names.LabelRole])
	}
	// What decides where the Job binds the disk is the instance's.
	same := func(what string, a, b any) {
		if jsonString(a) != jsonString(b) {
			t.Errorf("seed %s %s, instance %s", what, jsonString(a), jsonString(b))
		}
	}
	same("node selector", js.NodeSelector, is.NodeSelector)
	same("tolerations", js.Tolerations, is.Tolerations)
	same("affinity", js.Affinity, is.Affinity)
	same("runtime class", js.RuntimeClassName, is.RuntimeClassName)
	same("service account", js.ServiceAccountName, is.ServiceAccountName)
	same("pull secrets", js.ImagePullSecrets, is.ImagePullSecrets)
	same("DNS", js.DNSConfig, is.DNSConfig)
	same("resources", js.Containers[0].Resources, is.Containers[0].Resources)
	same("image", js.Containers[0].Image, is.Containers[0].Image)
	if js.ServiceAccountName != "fleet-pods" || *js.RuntimeClassName != "gvisor" || js.DNSConfig.Nameservers[0] != "10.0.0.53" {
		t.Errorf("seed placement %+v", js)
	}
	if len(js.Volumes) != 3 || js.Volumes[2].PersistentVolumeClaim.ClaimName != "fs-fleet-man-x-share" {
		t.Errorf("seed volumes %+v", js.Volumes)
	}
	if m, ok := mountAt(js.Containers[0], "/fleet-share"); !ok || m.Name != js.Volumes[2].Name {
		t.Errorf("share claim mount %+v", m)
	}
	if _, ok := job.Spec.Template.Annotations[annotationSafeToEvict]; ok {
		t.Error("a Job blocks scale-down")
	}

	res, err := Translate(baseInput(nil, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	r := testRenderInput(baseInput(nil, nil, nil))
	for name, j := range map[string]JobInput{
		"no name":         {Command: []string{"sh"}, ToolsDir: "/t", DiskPath: "/d"},
		"bad name":        {Name: "Seed_1", Command: []string{"sh"}, ToolsDir: "/t", DiskPath: "/d"},
		"long name":       {Name: strings.Repeat("a", 64), Command: []string{"sh"}, ToolsDir: "/t", DiskPath: "/d"},
		"relative tools":  {Name: "j", Command: []string{"sh"}, ToolsDir: "t", DiskPath: "/d"},
		"same paths":      {Name: "j", Command: []string{"sh"}, ToolsDir: "/d", DiskPath: "/d"},
		"claim w/o path":  {Name: "j", Command: []string{"sh"}, ToolsDir: "/t", DiskPath: "/d", Claims: []JobClaim{{ClaimName: "x"}}},
		"claim w/o claim": {Name: "j", Command: []string{"sh"}, ToolsDir: "/t", DiskPath: "/d", Claims: []JobClaim{{Path: "/x"}}},
	} {
		if _, err := RenderJob(res, r, j, names.RoleTool); err == nil {
			t.Errorf("%s: rendered", name)
		}
	}
	if _, err := RenderJob(res, r, JobInput{Name: "j", Command: []string{"sh"}, ToolsDir: "/t", DiskPath: "/d"}, names.RoleInstance); err == nil {
		t.Error("a Job pod took the instance role")
	}
	// A name alone is the bootstrap protocol's Job.
	j, err := RenderJob(res, r, JobInput{Name: "tool-1"}, names.RoleTool)
	if err != nil {
		t.Fatal(err)
	}
	c := j.Job.Spec.Template.Spec.Containers[0]
	if jsonString(c.Command) != jsonString(jobboot.Command(jobboot.Paths{}, 0)) || *j.Job.Spec.TTLSecondsAfterFinished != int32(jobboot.TTLAfterFinished/time.Second) || *j.Job.Spec.BackoffLimit != jobboot.BackoffLimit {
		t.Errorf("default Job %q ttl %d", c.Command, *j.Job.Spec.TTLSecondsAfterFinished)
	}
	if m, ok := mountAt(c, jobboot.ToolsDir); !ok || m.Name != volumeTools {
		t.Errorf("no tools dir at %s", jobboot.ToolsDir)
	}
	if m, ok := mountAt(c, JobDiskPath); !ok || m.Name != volumeDisk || m.SubPath != "" {
		t.Errorf("no whole disk at %s", JobDiskPath)
	}
}

func TestRenderDisruptionBudget(t *testing.T) {
	if o := mustRender(t, nil, nil); o.DisruptionBudget != nil {
		t.Error("a budget without protect_from_drain")
	}
	o := mustRender(t, nil, func(in *Input, _ *RenderInput) { in.Cluster.ProtectFromDrain = true })
	b := o.DisruptionBudget
	if b == nil || b.Spec.MaxUnavailable.IntValue() != 0 || *b.Spec.UnhealthyPodEvictionPolicy != policyv1.AlwaysAllow || !selects(b.Spec.Selector, o.StatefulSet.Spec.Template.Labels) || selects(b.Spec.Selector, o.Seed.Job.Spec.Template.Labels) {
		t.Errorf("budget %+v", b)
	}
}

func TestRenderInputValidation(t *testing.T) {
	res, err := Translate(baseInput(nil, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(*RenderInput){
		"no namespace":  func(r *RenderInput) { r.Namespace = "" },
		"no image":      func(r *RenderInput) { r.Image.Ref = "" },
		"no key":        func(r *RenderInput) { r.Key = "" },
		"gen 0":         func(r *RenderInput) { r.Gen = 0 },
		"gen too high":  func(r *RenderInput) { r.Gen = names.MaxGeneration + 1 },
		"2 replicas":    func(r *RenderInput) { r.Replicas = 2 },
		"nested root":   func(r *RenderInput) { r.Root.Dirs = []string{"usr/local"} },
		"dot root":      func(r *RenderInput) { r.Root.Extra = []string{".."} },
		"seed w/o name": func(r *RenderInput) { r.Seed.Name = "" },
	} {
		r := testRenderInput(baseInput(nil, nil, nil))
		edit(&r)
		if _, err := Render(res, r); err == nil {
			t.Errorf("%s: rendered", name)
		}
	}
	// Generation-named objects follow the generation.
	r := testRenderInput(baseInput(nil, nil, nil))
	r.Gen, r.Replicas = 7, 0
	o, err := Render(res, r)
	if err != nil {
		t.Fatal(err)
	}
	if o.SpecSecret.Name != names.SpecSecret(r.Name, 7) || o.EnvSecret.Name != names.EnvSecret(r.Name, 7) || *o.StatefulSet.Spec.Replicas != 0 ||
		o.StatefulSet.Annotations[names.AnnotationRebuildGen] != "7" || o.StatefulSet.Spec.Template.Spec.Volumes[1].Secret.SecretName != o.SpecSecret.Name {
		t.Errorf("generation 7 render: %s %s %v", o.SpecSecret.Name, o.EnvSecret.Name, o.StatefulSet.Annotations)
	}
}

func TestRenderRefusesWhatItCannotProvide(t *testing.T) {
	sock := "/home/dev/host/S.gpg-agent"
	for name, tt := range map[string]struct {
		args []string
		edit func(*Input, *RenderInput)
		code string
	}{
		"ssh-agent":       {args: []string{"-v", "/home/dev/host/agent.sock:/ssh.sock"}, code: CodeUnsupportedMount},
		"host socket":     {args: []string{"-v", sock + ":/gpg.sock"}, code: CodeUnsupportedMount},
		"host socket dir": {args: []string{"-v", "/home/dev/host/x11:/tmp/.X11-unix"}, code: CodeUnsupportedMount},
		"host dir":        {args: []string{"-v", "/home/dev/host/aws:/root/.aws"}, code: CodeUnsupportedMount},
		"host file":       {args: []string{"-v", "/home/dev/host/gitconfig:/root/.gitconfig"}, code: CodeUnsupportedMount},
		"shared volume":   {args: []string{"-v", "cache:/cache"}, code: CodeUnsupportedMount},
		"docker socket":   {args: []string{"-v", "/var/run/docker.sock:/var/run/docker.sock"}, code: CodeUnsupportedMount},
		"api socket":      {args: []string{"--use-api-socket"}, code: CodeUnsupportedMount},
		"fleet mounts": {edit: func(in *Input, _ *RenderInput) {
			in.FleetMounts = []backend.Mount{
				{ContainerPath: "/home/vscode/.claude", Scope: backend.MountScopeFleetShared, SubPath: "claude"},
				{ContainerPath: "/data", Scope: backend.MountScopeFleetShared, SubPath: ".mnt/data"},
			}
		}, code: CodeUnsupportedMount},
		"pod overrides": {edit: func(in *Input, _ *RenderInput) {
			in.Cluster.PodOverridesYAML = "spec:\n  priorityClassName: high\n"
		}, code: CodeSettings},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := renderOf(t, tt.args, tt.edit)
			rs := Refusals(err)
			if len(rs) != 1 || rs[0].Code != tt.code {
				t.Fatalf("Render = %v, want one %s", err, tt.code)
			}
		})
	}
	o := mustRender(t, []string{"--add-host", "host.docker.internal:host-gateway"}, nil)
	if len(o.Warnings) != 1 || !strings.Contains(o.Warnings[0], "host-gateway") {
		t.Errorf("host-gateway warnings %v", o.Warnings)
	}
	// An image VOLUME at a path fleet reserves.
	for _, vol := range []string{"/opt/fleet", "/opt/fleet/run", control.ContainerMountDir} {
		_, err := renderOf(t, nil, func(in *Input, _ *RenderInput) {
			in.Image.Config.Volumes = map[string]struct{}{vol: {}}
		})
		if rs := Refusals(err); len(rs) != 1 || rs[0].Code != CodeUnsupportedMount {
			t.Errorf("image VOLUME %s: %v", vol, err)
		}
	}
	o = mustRender(t, []string{"--device", "/dev/kvm"}, nil)
	var dev corev1.Volume
	for _, v := range o.StatefulSet.Spec.Template.Spec.Volumes {
		if v.HostPath != nil {
			dev = v
		}
	}
	if dev.HostPath == nil || dev.HostPath.Path != "/dev/kvm" || *dev.HostPath.Type != corev1.HostPathCharDev {
		t.Errorf("device volume %+v", dev)
	}
	if m, ok := mountAt(devOf(o), "/dev/kvm"); !ok || m.Name != dev.Name {
		t.Errorf("device mount %+v", m)
	}
}

// renderOptionResult is one option case's render in the golden file: what
// it changes in the rendered instance pod and seed pod, by JSON pointer.
type renderOptionResult struct {
	Changes  map[string]json.RawMessage `json:"changes,omitempty"`
	Warnings []string                   `json:"warnings,omitempty"`
	Error    string                     `json:"error,omitempty"`
}

// Every row of the flag table rendered: the golden holds, for each option
// case that translates, how its render differs from the base run's.
func TestRenderOptionGoldens(t *testing.T) {
	render := func(in Input) (*Objects, error) {
		res, err := Translate(in)
		if err != nil {
			return nil, err
		}
		return Render(res, testRenderInput(in))
	}
	base, err := render(baseInput(nil, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	pods := func(o *Objects) map[string]any {
		return map[string]any{"pod": o.StatefulSet.Spec.Template, "seed": o.Seed.Job.Spec.Template, "disk": o.Disk.Spec}
	}
	results := map[string]renderOptionResult{}
	for _, c := range optionCases {
		in := baseInput(c.args, c.env, c.labels)
		if c.in != nil {
			c.in(&in)
		}
		if _, err := Translate(in); err != nil {
			continue
		}
		o, err := render(in)
		if err != nil {
			results[c.name] = renderOptionResult{Error: err.Error()}
			continue
		}
		results[c.name] = renderOptionResult{Changes: changes(t, pods(base), pods(o)), Warnings: o.Warnings}
	}
	checkGolden(t, "render.options.golden.json", mustIndent(t, results))
}

// Property: whatever the run's environment holds, no value reaches an
// object other than the env Secret (and inspect.json in the spec Secret,
// which answers docker inspect): not the StatefulSet, the Job, the disk,
// the budget, or any Secret's metadata.
func TestEnvValuesStayOutOfTheObjects(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	alphabet := []rune("abcXYZ019 $()={}[]\"'\\\n\t:,;/._-ü✓")
	word := func(n int) string {
		r := make([]rune, n)
		for i := range r {
			r[i] = alphabet[rng.Intn(len(alphabet))]
		}
		return string(r)
	}
	for i := 0; i < 200; i++ {
		var args []string
		var env []kubeshim.EnvVar
		seen := map[string]bool{}
		for j := 0; j < 1+rng.Intn(5); j++ {
			name := "V" + strconv.Itoa(j) + strings.Map(func(r rune) rune {
				if r == '=' || r == 0 {
					return '_'
				}
				return r
			}, word(1+rng.Intn(6)))
			if seen[name] {
				continue
			}
			seen[name] = true
			value := "$(SECRET" + word(6+rng.Intn(20)) + ")"
			args = append(args, "-e", name+"="+value)
			env = append(env, kubeshim.EnvVar{Name: name, Value: value, Source: "flag"})
		}
		in := baseInput(args, env, nil)
		in.Cluster.ProtectFromDrain = true
		res, err := Translate(in)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		r := testRenderInput(in)
		o, err := Render(res, r)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		objects := jsonString([]any{o.StatefulSet, o.Seed.Job, o.Disk, o.DisruptionBudget, o.SpecSecret.ObjectMeta, o.EnvSecret.ObjectMeta})
		for _, e := range env {
			if strings.Contains(objects, jsonString(e.Value)[1:len(jsonString(e.Value))-1]) {
				t.Fatalf("case %d: the value of %s shows in the objects", i, e.Name)
			}
			if string(o.EnvSecret.Data[envKey(e.Name)]) != e.Value {
				t.Fatalf("case %d: the env Secret holds %q for %s", i, o.EnvSecret.Data[envKey(e.Name)], e.Name)
			}
		}
	}
}

// A size the translation reads but the API does not is refused, never a
// panic in the daemon.
func TestRenderRefusesQuantitiesTheAPIDoesNotTake(t *testing.T) {
	_, err := renderOf(t, nil, func(in *Input, _ *RenderInput) { in.Cluster.DiskSize = " 60Gi " })
	if rs := Refusals(err); len(rs) != 1 || rs[0].Code != CodeSettings || !strings.Contains(err.Error(), "disk size") {
		t.Errorf("Render = %v", err)
	}
}

// FuzzRender runs arbitrary docker run options through the table and the
// renderer: neither panics, and a render is hardened, with no run string in
// a kubelet-expanded field.
func FuzzRender(f *testing.F) {
	for _, c := range optionCases {
		f.Add(strings.Join(c.args, "\x00"), false)
		f.Add(strings.Join(c.args, "\x00"), true)
	}
	f.Fuzz(func(t *testing.T, joined string, local bool) {
		var extra []string
		if joined != "" {
			extra = strings.Split(joined, "\x00")
		}
		if _, err := kubeshim.ParseRun(append(append([]string{}, extra...), "img")); err != nil {
			return
		}
		in := baseInput(extra, nil, nil)
		in.Facts.Local = local
		res, err := Translate(in)
		if err != nil {
			return
		}
		r := testRenderInput(in)
		o, err := Render(res, r)
		if err != nil {
			if len(Refusals(err)) == 0 {
				t.Fatalf("Render(%q) failed without a refusal: %v", extra, err)
			}
			return
		}
		tmpl := o.StatefulSet.Spec.Template
		if v := CheckHardened(tmpl.ObjectMeta, tmpl.Spec, r.hardenFor(res)); len(v) > 0 {
			t.Fatalf("Render(%q) is not hardened: %v", extra, v)
		}
		dev := tmpl.Spec.Containers[0]
		if jsonString(dev.Command) != jsonString(Command()) || len(dev.Args) > 0 {
			t.Fatalf("Render(%q) runs %q %q", extra, dev.Command, dev.Args)
		}
		for _, e := range dev.Env {
			if e.Value != "" || e.ValueFrom == nil || (e.ValueFrom.SecretKeyRef == nil && e.Name != podinit.PodUIDEnv) {
				t.Fatalf("Render(%q): env %s", extra, e.Name)
			}
		}
	})
}

// hardenFor is the HardenOptions r renders res with.
func (r RenderInput) hardenFor(res *Result) HardenOptions {
	p := res.Pod
	return HardenOptions{ShareGroup: r.FleetHash, SELinuxLevel: r.SELinuxLevel, SELinuxMount: r.SELinuxMount,
		AllowHostAccess: p.HostNetwork || p.HostIPC || p.HostPID || hasMountKind(p.Mounts, MountDevice)}
}

// The seed is told what the rendered pod mounts: the mountpoints it makes,
// the volumes it copies up, the hostname.
func TestSeedSpecFollowsTheRender(t *testing.T) {
	rc := loadRecorded(t, "fixture-dind")
	in := rc.input()
	profiles[0].in(&in)
	res, err := Translate(in)
	if err != nil {
		t.Fatal(err)
	}
	r := testRenderInput(in)
	o, err := Render(res, r)
	if err != nil {
		t.Fatal(err)
	}
	s := SeedSpec(res, o)
	want := []string{control.ContainerMountDir, podseed.InstallDir, podseed.RunDir, podseed.SpecDir, "/var/lib/docker", rc.WorkspaceFolder}
	sort.Strings(want)
	if strings.Join(s.MountDirs, " ") != strings.Join(want, " ") {
		t.Errorf("mount dirs %v, want %v", s.MountDirs, want)
	}
	if s.Hostname != names.Pod(r.Name) || len(s.Volumes) != 1 || !strings.HasPrefix(s.Volumes[0].Name, "dind-var-lib-docker-") || s.Volumes[0].Target != "/var/lib/docker" || !s.Volumes[0].CopyUp {
		t.Errorf("seed spec %+v", s)
	}
	job := podseed.JobSpec{V: podseed.JobSpecVersion, Op: podseed.OpSeed, Gen: 1, PVC: JobDiskPath, Seed: s}
	if err := job.Validate(); err != nil {
		t.Errorf("the seed spec does not validate: %v", err)
	}
	// A device is a file the seed does not make.
	o = mustRender(t, []string{"--device", "/dev/kvm", "--tmpfs", "/scratch"}, nil)
	res2, _ := Translate(func() Input {
		in := baseInput([]string{"--device", "/dev/kvm", "--tmpfs", "/scratch"}, nil, nil)
		local(&in)
		return in
	}())
	if dirs := strings.Join(SeedSpec(res2, o).MountDirs, " "); strings.Contains(dirs, "/dev/kvm") || !strings.Contains(dirs, "/scratch") {
		t.Errorf("mount dirs %s", dirs)
	}
}
