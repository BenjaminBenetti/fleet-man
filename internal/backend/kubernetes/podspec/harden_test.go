//go:build !nok8s

package podspec

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
)

// bareWorkload is a pod template as a renderer might start one, before
// Harden: a service pod with no opinions of its own.
func bareWorkload() corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "cache"}},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "main", Image: "registry.example.com/apt-cacher@sha256:" + strings.Repeat("a", 64)},
				{Name: "debug", Image: "registry.example.com/tool@sha256:" + strings.Repeat("b", 64),
					SecurityContext: &corev1.SecurityContext{SELinuxOptions: &corev1.SELinuxOptions{Type: "spc_t"}}},
			},
			InitContainers: []corev1.Container{{Name: "init", Image: "registry.example.com/init@sha256:" + strings.Repeat("c", 64)}},
		},
	}
}

func TestHarden(t *testing.T) {
	opts := HardenOptions{ShareGroup: "abcd2345", SELinuxMount: true, PinnedImages: true}
	w := bareWorkload()
	if v := CheckHardened(w.ObjectMeta, w.Spec, opts); len(v) < 8 {
		t.Fatalf("a bare workload shows only %d violations: %v", len(v), v)
	}
	if err := HardenTemplate(&w, opts); err != nil {
		t.Fatal(err)
	}
	if v := CheckHardened(w.ObjectMeta, w.Spec, opts); len(v) > 0 {
		t.Fatalf("hardened, still: %v", v)
	}
	s := w.Spec
	if *s.AutomountServiceAccountToken || *s.EnableServiceLinks {
		t.Error("token or service links left on")
	}
	level := SELinuxLevel("abcd2345")
	if s.SecurityContext.SELinuxOptions.Level != level || s.Containers[1].SecurityContext.SELinuxOptions.Level != level || s.Containers[1].SecurityContext.SELinuxOptions.Type != "spc_t" {
		t.Errorf("SELinux options: pod %+v, container %+v", s.SecurityContext.SELinuxOptions, s.Containers[1].SecurityContext.SELinuxOptions)
	}
	if s.Containers[0].SecurityContext != nil {
		t.Error("Harden gave a container without SELinux options its own")
	}
	if w.Labels["app"] != "cache" || w.Annotations["linkerd.io/inject"] != "disabled" || w.Labels["sidecar.istio.io/inject"] != "false" {
		t.Errorf("metadata %+v", w.ObjectMeta)
	}
	// Idempotent.
	again := *w.DeepCopy()
	if err := HardenTemplate(&again, opts); err != nil || SpecHash(&again.Spec) != SpecHash(&w.Spec) {
		t.Errorf("a second Harden changed the pod: %v", err)
	}
	// An unconfined pod-level profile is replaced; a container's own stays.
	w = bareWorkload()
	w.Spec.SecurityContext = &corev1.PodSecurityContext{SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}}
	w.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}}
	if err := HardenTemplate(&w, HardenOptions{}); err != nil {
		t.Fatal(err)
	}
	if w.Spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault || w.Spec.Containers[0].SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeUnconfined {
		t.Errorf("seccomp: pod %v, container %v", w.Spec.SecurityContext.SeccompProfile, w.Spec.Containers[0].SecurityContext.SeccompProfile)
	}
	if w.Spec.SecurityContext.SELinuxOptions != nil || w.Spec.SecurityContext.SELinuxChangePolicy != nil {
		t.Errorf("no share-group, yet SELinux options %+v", w.Spec.SecurityContext)
	}
	// An explicit level (an OpenShift namespace's) wins.
	w = bareWorkload()
	if err := HardenTemplate(&w, HardenOptions{ShareGroup: "abcd2345", SELinuxLevel: "s0:c26,c5"}); err != nil || w.Spec.SecurityContext.SELinuxOptions.Level != "s0:c26,c5" {
		t.Errorf("explicit level: %v %+v", err, w.Spec.SecurityContext.SELinuxOptions)
	}
}

func TestHardenRefusesWhatItMayNotFix(t *testing.T) {
	for name, edit := range map[string]func(*corev1.PodTemplateSpec){
		"hostNetwork": func(w *corev1.PodTemplateSpec) { w.Spec.HostNetwork = true },
		"hostPID":     func(w *corev1.PodTemplateSpec) { w.Spec.HostPID = true },
		"hostIPC":     func(w *corev1.PodTemplateSpec) { w.Spec.HostIPC = true },
		"hostPath": func(w *corev1.PodTemplateSpec) {
			w.Spec.Volumes = append(w.Spec.Volumes, corev1.Volume{Name: "h", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}})
		},
		"unpinned": func(w *corev1.PodTemplateSpec) { w.Spec.InitContainers[0].Image = "registry.example.com/init:latest" },
	} {
		t.Run(name, func(t *testing.T) {
			w := bareWorkload()
			edit(&w)
			before := SpecHash(&w.Spec)
			opts := HardenOptions{PinnedImages: true}
			if err := HardenTemplate(&w, opts); err == nil {
				t.Fatal("Harden took it")
			}
			if SpecHash(&w.Spec) != before || w.Spec.AutomountServiceAccountToken != nil {
				t.Error("a refused Harden changed the pod")
			}
			if len(CheckHardened(w.ObjectMeta, w.Spec, opts)) == 0 {
				t.Error("CheckHardened misses it")
			}
			if name != "unpinned" {
				opts.AllowHostAccess = true
				if err := HardenTemplate(&w, opts); err != nil {
					t.Errorf("allowed host access: %v", err)
				}
			}
		})
	}
}

func TestSELinuxLevel(t *testing.T) {
	re := regexp.MustCompile(`^s0:c([0-9]+),c([0-9]+)$`)
	seen := map[string]bool{}
	for i := 0; i < 2000; i++ {
		group := "g" + strconv.Itoa(i)
		l := SELinuxLevel(group)
		if l != SELinuxLevel(group) {
			t.Fatalf("SELinuxLevel(%s) is not stable", group)
		}
		m := re.FindStringSubmatch(l)
		if m == nil {
			t.Fatalf("SELinuxLevel(%s) = %q", group, l)
		}
		a, _ := strconv.Atoi(m[1])
		b, _ := strconv.Atoi(m[2])
		if a >= b || b > 1023 {
			t.Fatalf("SELinuxLevel(%s) = %q: categories not distinct, ascending, within c0-c1023", group, l)
		}
		seen[l] = true
	}
	if len(seen) < 1990 {
		t.Errorf("2000 share-groups got only %d levels", len(seen))
	}
}

// specHashPod is a rendered instance pod.
func specHashPod(t *testing.T) *corev1.Pod {
	t.Helper()
	o := mustRender(t, []string{"-e", "A=1", "--cap-add", "NET_ADMIN", "--tmpfs", "/scratch"}, nil)
	return o.Pod()
}

// What the API server and the controllers do to a pod leaves its hash; what
// a substitute or an edit does changes it.
func TestSpecHash(t *testing.T) {
	rendered := specHashPod(t)
	want := SpecHash(&rendered.Spec)
	same := map[string]func(*corev1.Pod){
		"defaulting": func(p *corev1.Pod) {
			s := &p.Spec
			s.SchedulerName, s.DeprecatedServiceAccount = "default-scheduler", "default"
			s.Priority, s.PreemptionPolicy = ptr(int32(0)), ptr(corev1.PreemptLowerPriority)
			s.Tolerations = append(s.Tolerations, corev1.Toleration{Key: "node.kubernetes.io/not-ready", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: ptr(int64(300))})
			c := &s.Containers[0]
			c.TerminationMessagePath = "/dev/termination-log"
			c.ImagePullPolicy = corev1.PullAlways
			for i := range c.VolumeMounts {
				c.VolumeMounts[i].MountPropagation = ptr(corev1.MountPropagationNone)
			}
		},
		"default service account": func(p *corev1.Pod) { p.Spec.ServiceAccountName = "default" },
		"limit range":             func(p *corev1.Pod) { p.Spec.Containers[0].Resources = corev1.ResourceRequirements{} },
		"re-pointed image": func(p *corev1.Pod) {
			p.Spec.Containers[0].Image = "gateway.example.com/abc/devcontainer@sha256:" + strings.Repeat("9", 64)
		},
		"node and status":       func(p *corev1.Pod) { p.Spec.NodeName = "node-1"; p.Status.Phase = corev1.PodRunning },
		"metadata":              func(p *corev1.Pod) { p.Annotations["x"] = "y"; p.Labels["controller-revision-hash"] = "abc" },
		"secret mode defaulted": func(p *corev1.Pod) {},
	}
	differ := map[string]func(*corev1.Pod){
		"privileged": func(p *corev1.Pod) { p.Spec.Containers[0].SecurityContext.Privileged = ptr(true) },
		"hostPath volume": func(p *corev1.Pod) {
			p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{Name: "root", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}})
		},
		"other command": func(p *corev1.Pod) { p.Spec.Containers[0].Command = []string{"/bin/sh"} },
		"other args":    func(p *corev1.Pod) { p.Spec.Containers[0].Args = []string{"x"} },
		"extra mount": func(p *corev1.Pod) {
			p.Spec.Containers[0].VolumeMounts = append(p.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "disk", MountPath: "/x"})
		},
		"extra env": func(p *corev1.Pod) {
			p.Spec.Containers[0].Env = append(p.Spec.Containers[0].Env, corev1.EnvVar{Name: "LD_PRELOAD", Value: "/x.so"})
		},
		"debug container": func(p *corev1.Pod) {
			p.Spec.EphemeralContainers = []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debugger", Image: "busybox"}}}
		},
		"sidecar": func(p *corev1.Pod) {
			p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "proxy", Image: "x"})
		},
		"init container":     func(p *corev1.Pod) { p.Spec.InitContainers = []corev1.Container{{Name: "i", Image: "x"}} },
		"service account":    func(p *corev1.Pod) { p.Spec.ServiceAccountName = "admin" },
		"token mounted":      func(p *corev1.Pod) { p.Spec.AutomountServiceAccountToken = ptr(true) },
		"host network":       func(p *corev1.Pod) { p.Spec.HostNetwork = true },
		"pod security":       func(p *corev1.Pod) { p.Spec.SecurityContext.RunAsUser = ptr(int64(0)) },
		"other spec Secret":  func(p *corev1.Pod) { p.Spec.Volumes[1].Secret.SecretName = "attacker" },
		"other claim":        func(p *corev1.Pod) { p.Spec.Volumes[0].PersistentVolumeClaim.ClaimName = "other" },
		"runtime class":      func(p *corev1.Pod) { p.Spec.RuntimeClassName = ptr("runc-unconfined") },
		"read-only mount rw": func(p *corev1.Pod) { p.Spec.Containers[0].VolumeMounts[12].ReadOnly = false },
		"lifecycle hook": func(p *corev1.Pod) {
			p.Spec.Containers[0].Lifecycle = &corev1.Lifecycle{PostStart: &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: []string{"sh"}}}}
		},
		"host port": func(p *corev1.Pod) {
			p.Spec.Containers[0].Ports = []corev1.ContainerPort{{ContainerPort: 22, HostPort: 2222}}
		},
	}
	for name, edit := range same {
		p := rendered.DeepCopy()
		edit(p)
		if got := SpecHash(&p.Spec); got != want {
			t.Errorf("%s changes the hash", name)
		}
	}
	for name, edit := range differ {
		p := rendered.DeepCopy()
		edit(p)
		if got := SpecHash(&p.Spec); got == want {
			t.Errorf("%s leaves the hash as it was", name)
		}
	}
	// A Secret volume without a mode hashes as the API server defaults it.
	a, b := rendered.DeepCopy(), rendered.DeepCopy()
	a.Spec.Volumes[1].Secret.DefaultMode = nil
	b.Spec.Volumes[1].Secret.DefaultMode = ptr(int32(0o644))
	if SpecHash(&a.Spec) != SpecHash(&b.Spec) {
		t.Error("a defaulted Secret mode hashes differently")
	}
	if m, _ := mountAt(rendered.Spec.Containers[0], podseed.SpecDir); !m.ReadOnly || rendered.Spec.Containers[0].VolumeMounts[12].MountPath != podseed.SpecDir {
		t.Fatalf("the test's mount index 12 is no longer the spec Secret's: %+v", rendered.Spec.Containers[0].VolumeMounts)
	}
}
