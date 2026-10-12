//go:build !nok8s

package podspec

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// Refusals as the API server words them; the pod security, quota, limit
// range, service account, invalid and namespace ones were recorded from a
// Kubernetes 1.37 API server, the others are the wording of the admission
// code that produces them.
var refusalMessages = []struct {
	name, msg string
	kind      DenialKind
}{
	{"baseline", `pods "dev-0" is forbidden: violates PodSecurity "baseline:latest": non-default capabilities (container "dev" must not include "SYS_PTRACE" in securityContext.capabilities.add), privileged (container "dev" must not set securityContext.privileged=true), seccompProfile (container "dev" must not set securityContext.seccompProfile.type to "Unconfined")`, DeniedPodSecurity},
	{"restricted", `pods "t" is forbidden: violates PodSecurity "restricted:latest": allowPrivilegeEscalation != false (container "dev" must set securityContext.allowPrivilegeEscalation=false), runAsNonRoot != true (pod or container "dev" must set securityContext.runAsNonRoot=true), runAsUser=0 (container "dev" must not set runAsUser=0)`, DeniedPodSecurity},
	{"quota", `pods "dev-0" is forbidden: exceeded quota: small, requested: limits.memory=3Gi,requests.memory=2Gi, used: limits.memory=0,requests.memory=0, limited: limits.memory=2Gi,requests.memory=1Gi`, DeniedQuota},
	{"quota needs limits", `pods "dev-0" is forbidden: failed quota: small: must specify limits.memory for: dev`, DeniedQuota},
	{"limit range", `pods "dev-0" is forbidden: maximum memory usage per Container is 4Gi, but limit is 8Gi`, DeniedLimitRange},
	{"limit range ratio", `pods "dev-0" is forbidden: memory max limit to request ratio per Container is 2, but provided ratio is 4.000000`, DeniedLimitRange},
	{"service account", `pods "dev-0" is forbidden: error looking up service account fleet-it/nosuch: serviceaccount "nosuch" not found`, DeniedServiceAccount},
	{"gatekeeper", `admission webhook "validation.gatekeeper.sh" denied the request: [psp-privileged-container] Privileged container is not allowed: dev, securityContext: {"privileged": true}`, DeniedWebhook},
	{"kyverno", "admission webhook \"validate.kyverno.svc-fail\" denied the request: \n\nresource Pod/team/dev-0 was blocked due to the following policies\n\ndisallow-privileged-containers:\n  privileged-containers: validation error", DeniedWebhook},
	{"admission policy", `pods "dev-0" is forbidden: ValidatingAdmissionPolicy 'no-root.example.com' with binding 'no-root-binding' denied request: failed expression: object.spec.securityContext.runAsNonRoot == true`, DeniedPolicy},
	{"rbac", `pods is forbidden: User "system:serviceaccount:ci:fleet" cannot create resource "pods" in API group "" in the namespace "team"`, DeniedRBAC},
	{"terminating", `pods "dev-0" is forbidden: unable to create new content in namespace team because it is being terminated`, DeniedNamespace},
	{"other plugin", `pods "dev-0" is forbidden: pod node label selector conflicts with its namespace node label selector`, DeniedAdmission},
}

func TestClassifyMessages(t *testing.T) {
	for _, m := range refusalMessages {
		t.Run(m.name, func(t *testing.T) {
			d := classifyMessage("Pod dev-0", m.msg)
			if d.Kind != m.kind {
				t.Fatalf("kind %s, want %s", d.Kind, m.kind)
			}
			r := d.Refusal()
			if !strings.HasPrefix(d.Error(), r.Code+": ") || !strings.Contains(r.Reason, "Pod dev-0") {
				t.Errorf("error %q", d.Error())
			}
			if strings.Contains(r.Reason, `is forbidden: `) {
				t.Errorf("the reason repeats the API server's lead: %s", r.Reason)
			}
			// The event the controllers record wraps the same message.
			for _, ev := range []string{"create Pod dev-0 in StatefulSet dev failed error: " + m.msg, "Error creating: " + m.msg} {
				if got := ClassifyFailedCreate("StatefulSet dev", ev); got == nil || got.Kind != m.kind || got.Message != m.msg {
					t.Errorf("FailedCreate %q classified %+v", ev, got)
				}
			}
		})
	}
	want := map[DenialKind]string{
		DeniedPodSecurity: CodeAdmissionDenied, DeniedWebhook: CodeAdmissionDenied, DeniedPolicy: CodeAdmissionDenied,
		DeniedAdmission: CodeAdmissionDenied, DeniedLimitRange: CodeAdmissionDenied, DeniedQuota: CodeQuota,
		DeniedServiceAccount: CodeSettings, DeniedNamespace: CodeSettings, DeniedRBAC: CodeForbidden, DeniedInvalid: CodeInvalidSpec,
	}
	for kind, code := range want {
		if got := (&Denial{Kind: kind}).Code(); got != code {
			t.Errorf("%s: code %s, want %s", kind, got, code)
		}
	}
	d := classifyMessage("Pod dev-0", refusalMessages[0].msg)
	if r := d.Refusal().Reason; !strings.Contains(r, "pod security (baseline)") || !strings.HasPrefix(strings.SplitN(r, ": ", 2)[1], "non-default capabilities") {
		t.Errorf("pod security reason %q", r)
	}
}

func TestClassifyFailedCreateOthers(t *testing.T) {
	for _, ev := range []string{
		"create Pod dev-0 in StatefulSet dev failed error: Timeout: request did not complete within requested timeout",
		"Error creating: Internal error occurred: etcdserver: request timed out",
		"",
	} {
		if d := ClassifyFailedCreate("StatefulSet dev", ev); d != nil {
			t.Errorf("%q classified as %+v", ev, d)
		}
	}
	for ev, kind := range map[string]DenialKind{
		`Error creating: Internal error occurred: failed calling webhook "policy.example.com": failed to call webhook: context deadline exceeded`:          DeniedWebhook,
		`create Pod dev-0 in StatefulSet dev failed error: Pod "dev-0" is invalid: spec.hostAliases[0].ip: Invalid value: "x": must be a valid IP address`: DeniedInvalid,
		`Error creating: namespaces "gone" not found`: DeniedNamespace,
	} {
		if d := ClassifyFailedCreate("Job seed", ev); d == nil || d.Kind != kind {
			t.Errorf("%q classified as %+v, want %s", ev, d, kind)
		}
	}
}

func TestDenialOf(t *testing.T) {
	pods := schema.GroupResource{Resource: "pods"}
	for _, tt := range []struct {
		name string
		err  error
		kind DenialKind // "" for no denial
	}{
		{"forbidden", apierrors.NewForbidden(pods, "dev-0", errors.New(strings.SplitN(refusalMessages[0].msg, "is forbidden: ", 2)[1])), DeniedPodSecurity},
		{"invalid", apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, "dev-0", nil), DeniedInvalid},
		{"namespace", apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, "gone"), DeniedNamespace},
		{"webhook call", apierrors.NewInternalError(errors.New(`failed calling webhook "x": context deadline exceeded`)), DeniedWebhook},
		// Admission's own decisions, whatever status code they came with: a
		// policy's reason Invalid answers 422, a webhook picks its own code.
		{"policy reason Invalid", statusErr(422, metav1.StatusReasonInvalid, refusalMessages[9].msg), DeniedPolicy},
		{"webhook 422", statusErr(422, "", refusalMessages[7].msg), DeniedWebhook},
		{"webhook 400", statusErr(400, metav1.StatusReasonBadRequest, refusalMessages[8].msg), DeniedWebhook},
		{"webhook 500", statusErr(500, metav1.StatusReasonInternalError, refusalMessages[7].msg), DeniedWebhook},
		{"exists", apierrors.NewAlreadyExists(pods, "dev-0"), ""},
		{"other not found", apierrors.NewNotFound(pods, "dev-0"), ""},
		{"internal", apierrors.NewInternalError(errors.New("etcd")), ""},
		{"timeout", apierrors.NewTimeoutError("slow", 1), ""},
		{"transport", errors.New("dial tcp: connection refused"), ""},
	} {
		d := DenialOf("Pod dev-0", tt.err)
		switch {
		case tt.kind == "" && d != nil:
			t.Errorf("%s: classified as %+v", tt.name, d)
		case tt.kind != "" && (d == nil || d.Kind != tt.kind):
			t.Errorf("%s: %+v, want %s", tt.name, d, tt.kind)
		}
	}
}

func statusErr(code int32, reason metav1.StatusReason, msg string) error {
	return &apierrors.StatusError{ErrStatus: metav1.Status{Status: metav1.StatusFailure, Code: code, Reason: reason, Message: msg}}
}

// A webhook that could not be called refused nothing the debug grants
// decide; one that answered did.
func TestDenialPolicy(t *testing.T) {
	call := ClassifyFailedCreate("Job seed", `Error creating: Internal error occurred: failed calling webhook "policy.example.com": failed to call webhook: context deadline exceeded`)
	if call == nil || call.Kind != DeniedWebhook || call.Policy() {
		t.Errorf("webhook call failure %+v: policy %v", call, call != nil && call.Policy())
	}
	if d := classifyMessage("Pod dev-0", refusalMessages[7].msg); !d.Policy() {
		t.Errorf("webhook denial %+v is no policy", d)
	}
	// A webhook's message that happens to say "is invalid" is still the
	// webhook's.
	msg := `admission webhook "v.example.com" denied the request: spec.containers[0] is invalid: privileged`
	if d := ClassifyFailedCreate("StatefulSet dev", "create Pod dev-0 in StatefulSet dev failed error: "+msg); d == nil || d.Kind != DeniedWebhook {
		t.Errorf("FailedCreate %+v", d)
	}
}

func TestDenialDecide(t *testing.T) {
	pod := &PodSpec{DebugGrants: []string{"--cap-add SYS_PTRACE"}}
	psa := classifyMessage("Pod dev-0", refusalMessages[0].msg)
	if rerender, err := psa.Decide(&fleet.KubeCluster{}, pod); !rerender || err != nil {
		t.Errorf("auto: %v %v", rerender, err)
	}
	if _, err := psa.Decide(&fleet.KubeCluster{DebugCapabilities: fleet.KubeDebugCapabilitiesRequire}, pod); err == nil || !strings.HasPrefix(err.Error(), CodeAdmissionDenied+":debug_capabilities: ") {
		t.Errorf("require: %v", err)
	}
	quota := classifyMessage("Pod dev-0", refusalMessages[2].msg)
	if rerender, err := quota.Decide(&fleet.KubeCluster{}, pod); rerender || err == nil || !strings.HasPrefix(err.Error(), CodeQuota+": ") {
		t.Errorf("quota: %v %v", rerender, err)
	}
}

// fakeAdmission is an API server whose admission refuses what deny says:
// deny gets each dry-run create or apply and returns its answer. The
// resources in existing have the object DryRun looks up.
func fakeAdmission(t *testing.T, deny func(verb, resource string, obj runtime.Object) error, existing ...string) (*fake.Clientset, *[]string) {
	t.Helper()
	cs := fake.NewClientset()
	var seen []string
	cs.PrependReactor("*", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetVerb() == "get" {
			if containsString(existing, a.GetResource().Resource) {
				return true, map[string]runtime.Object{"jobs": &batchv1.Job{}, "persistentvolumeclaims": &corev1.PersistentVolumeClaim{}, "secrets": &corev1.Secret{}}[a.GetResource().Resource], nil
			}
			return true, nil, apierrors.NewNotFound(a.GetResource().GroupResource(), a.(k8stesting.GetAction).GetName())
		}
		if !a.Matches("create", a.GetResource().Resource) && !a.Matches("patch", a.GetResource().Resource) {
			t.Errorf("DryRun did %s %s", a.GetVerb(), a.GetResource().Resource)
			return true, nil, errors.New("unexpected")
		}
		var obj runtime.Object
		var dry []string
		switch act := a.(type) {
		case k8stesting.CreateActionImpl:
			obj, dry = act.GetObject(), act.CreateOptions.DryRun
		case k8stesting.PatchActionImpl:
			dry = act.PatchOptions.DryRun
			if act.PatchOptions.FieldManager != FieldManager || act.PatchOptions.Force == nil || !*act.PatchOptions.Force {
				t.Errorf("apply options %+v", act.PatchOptions)
			}
		}
		if len(dry) != 1 || dry[0] != metav1.DryRunAll {
			t.Errorf("%s %s without dryRun=All", a.GetVerb(), a.GetResource().Resource)
		}
		seen = append(seen, a.GetVerb()+" "+a.GetResource().Resource)
		if err := deny(a.GetVerb(), a.GetResource().Resource, obj); err != nil {
			return true, nil, err
		}
		return true, obj, nil
	})
	return cs, &seen
}

func forbidden(resource, msg string) error {
	return apierrors.NewForbidden(schema.GroupResource{Resource: resource}, "x", errors.New(msg))
}

func TestDryRun(t *testing.T) {
	o := mustRender(t, nil, func(_ *Input, r *RenderInput) { r.Cluster.ProtectFromDrain = true })
	if o.DisruptionBudget == nil {
		t.Fatal("no disruption budget")
	}
	cs, seen := fakeAdmission(t, func(string, string, runtime.Object) error { return nil })
	if err := DryRun(context.Background(), cs, o); err != nil {
		t.Fatal(err)
	}
	want := "create pods,patch statefulsets,create pods,create jobs,create persistentvolumeclaims,create secrets,create secrets,patch poddisruptionbudgets"
	if got := strings.Join(*seen, ","); got != want {
		t.Errorf("dry-runs %s\nwant      %s", got, want)
	}
	// The pods are asked about as new ones, by a generated name.
	cs, _ = fakeAdmission(t, func(verb, resource string, obj runtime.Object) error {
		if p, ok := obj.(*corev1.Pod); ok && (p.Name != "" || !strings.HasPrefix(p.GenerateName, o.StatefulSet.Name+"-")) {
			t.Errorf("pod dry-run named %q / %q", p.Name, p.GenerateName)
		}
		return nil
	})
	if err := DryRun(context.Background(), cs, o); err != nil {
		t.Fatal(err)
	}
	// What exists already is not asked about again.
	cs, seen = fakeAdmission(t, func(string, string, runtime.Object) error { return nil }, "persistentvolumeclaims", "jobs", "secrets")
	if err := DryRun(context.Background(), cs, o); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(*seen, ","); got != "create pods,patch statefulsets,create pods,patch poddisruptionbudgets" {
		t.Errorf("dry-runs of an existing instance: %s", got)
	}

	// Every refusal is reported, the same one once; an existing object
	// passes.
	cs, _ = fakeAdmission(t, func(verb, resource string, _ runtime.Object) error {
		switch resource {
		case "pods":
			return forbidden("pods", "exceeded quota: q, requested: requests.memory=2Gi")
		case "persistentvolumeclaims":
			return forbidden("persistentvolumeclaims", "exceeded quota: q, requested: requests.storage=50Gi")
		case "secrets":
			return apierrors.NewAlreadyExists(schema.GroupResource{Resource: resource}, "x")
		}
		return nil
	})
	err := DryRun(context.Background(), cs, o)
	ds := Denials(err)
	if len(ds) != 2 || ds[0].Object != "Pod "+o.Pod().Name || ds[1].Object != "PersistentVolumeClaim "+o.Disk.Name || !strings.HasPrefix(err.Error(), CodeQuota+": ") {
		t.Errorf("DryRun = %v (%d denials)", err, len(ds))
	}
	if rs := Refusals(err); len(rs) != 2 || rs[0].Code != CodeQuota {
		t.Errorf("Refusals = %v", rs)
	}

	// A failure to ask is no refusal.
	cs, _ = fakeAdmission(t, func(string, string, runtime.Object) error { return errors.New("connection refused") })
	if err := DryRun(context.Background(), cs, o); err == nil || Denials(err) != nil || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("DryRun = %v", err)
	}
}

// An admission that gives the instance's pod an fsGroup fleet did not
// render (an OpenShift SCC sets one from the namespace's range) is a warning
// of the render: the kubelet re-owns the pod's volumes to that group, so the
// share's 0600 logins become 0660. Fleet's own fsGroup (tier C) and none at
// all are no warning; another group in place of fleet's is.
func TestDryRunWarnsOfAnInjectedFSGroup(t *testing.T) {
	inject := func(gid *int64) func(string, string, runtime.Object) error {
		return func(_, resource string, obj runtime.Object) error {
			if p, ok := obj.(*corev1.Pod); ok && gid != nil {
				if p.Spec.SecurityContext == nil {
					p.Spec.SecurityContext = &corev1.PodSecurityContext{}
				}
				p.Spec.SecurityContext.FSGroup = gid
			}
			return nil
		}
	}
	scc := ptr(int64(1000650000))
	for _, tt := range []struct {
		name    string
		nonRoot bool
		admits  *int64
		want    string
	}{
		{"none rendered, none given", false, nil, ""},
		{"an SCC's over none", false, scc, "fsGroup 1000650000 (fleet sets none"},
		{"fleet's own kept", true, ptr(int64(1000)), ""},
		{"another in place of fleet's", true, scc, "fsGroup 1000650000 (fleet sets 1000"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			o := mustRender(t, nil, func(_ *Input, r *RenderInput) {
				if tt.nonRoot {
					r.Identity = Identity{NonRoot: true, UID: ptr(int64(1000)), GID: ptr(int64(1000))}
				}
			})
			cs, _ := fakeAdmission(t, inject(tt.admits))
			if err := DryRun(context.Background(), cs, o); err != nil {
				t.Fatal(err)
			}
			got := ""
			for _, w := range o.Warnings {
				if strings.Contains(w, "fsGroup") {
					got = w
				}
			}
			switch {
			case tt.want == "" && got != "":
				t.Errorf("warned %q", got)
			case tt.want != "" && (!strings.Contains(got, tt.want) || !strings.Contains(got, "0660")):
				t.Errorf("warning %q, want one with %q", got, tt.want)
			}
			// Asked again, the warning is there once.
			if err := DryRun(context.Background(), cs, o); err != nil {
				t.Fatal(err)
			}
			n := 0
			for _, w := range o.Warnings {
				if strings.Contains(w, "fsGroup") {
					n++
				}
			}
			if n > 1 {
				t.Errorf("the warning is there %d times", n)
			}
		})
	}
}

// baselinePSA refuses what baseline pod security refuses that the tests
// render: the debug grants and privileged pods.
func baselinePSA(_, resource string, obj runtime.Object) error {
	if resource != "pods" {
		return nil
	}
	var why []string
	for _, c := range podOf(obj).Spec.Containers {
		sc := c.SecurityContext
		if sc == nil {
			continue
		}
		if sc.Privileged != nil && *sc.Privileged {
			why = append(why, `privileged (container "`+c.Name+`" must not set securityContext.privileged=true)`)
		}
		if sc.Capabilities != nil {
			for _, a := range sc.Capabilities.Add {
				if a == "SYS_PTRACE" || a == "NET_ADMIN" {
					why = append(why, `non-default capabilities (container "`+c.Name+`" must not include "`+string(a)+`" in securityContext.capabilities.add)`)
				}
			}
		}
		if sc.SeccompProfile != nil && sc.SeccompProfile.Type == "Unconfined" {
			why = append(why, `seccompProfile (container "`+c.Name+`" must not set securityContext.seccompProfile.type to "Unconfined")`)
		}
	}
	if len(why) == 0 {
		return nil
	}
	return forbidden("pods", `violates PodSecurity "baseline:latest": `+strings.Join(why, ", "))
}

func TestAdmitDebugCapabilities(t *testing.T) {
	debug := []string{"--cap-add", "SYS_PTRACE", "--security-opt", "seccomp=unconfined"}
	admit := func(args []string, cluster fleet.KubeCluster, deny func(string, string, runtime.Object) error) (*Admitted, error, []string) {
		in := baseInput(args, nil, nil)
		local(&in)
		in.Cluster = &cluster
		cluster.ID, cluster.Namespace = "minikube", "fleet"
		r := testRenderInput(in)
		r.Cluster = nil
		cs, seen := fakeAdmission(t, deny)
		a, err := Admit(context.Background(), cs, in, r)
		return a, err, *seen
	}

	// auto: the grants alone are refused, so they are dropped, with the
	// warning.
	a, err, seen := admit(debug, fleet.KubeCluster{}, baselinePSA)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Result.Pod.DebugDropped || a.Result.Pod.Security.Seccomp != "" || containsString(a.Result.Pod.Security.CapAdd, "SYS_PTRACE") || !strings.Contains(strings.Join(a.Result.Pod.Warnings, "\n"), DebugDroppedWarning) {
		t.Errorf("admitted pod %+v", a.Result.Pod.Security)
	}
	if sc := devOf(a.Objects).SecurityContext; sc.SeccompProfile != nil || containsCap(sc, "SYS_PTRACE") {
		t.Errorf("the admitted render still carries the grants: %+v", sc)
	}
	if len(seen) != 2*7 {
		t.Errorf("%d dry-runs, want two rounds of 7", len(seen))
	}

	// require: refused, naming the knob.
	if _, err, _ := admit(debug, fleet.KubeCluster{DebugCapabilities: fleet.KubeDebugCapabilitiesRequire}, baselinePSA); err == nil || !strings.HasPrefix(err.Error(), CodeAdmissionDenied+":debug_capabilities: ") || !strings.Contains(err.Error(), "SYS_PTRACE") {
		t.Errorf("require: %v", err)
	}

	// A functional grant refused beside the debug ones: the second
	// refusal, which names only it, is the error, in either mode.
	for _, mode := range []fleet.KubeDebugCapabilities{"", fleet.KubeDebugCapabilitiesRequire} {
		_, err, _ := admit(append(debug, "--cap-add", "NET_ADMIN"), fleet.KubeCluster{DebugCapabilities: mode}, baselinePSA)
		if err == nil || !strings.HasPrefix(err.Error(), CodeAdmissionDenied+": ") || strings.Contains(err.Error(), "SYS_PTRACE") || !strings.Contains(err.Error(), "NET_ADMIN") {
			t.Errorf("mode %q: %v", mode, err)
		}
	}

	// No debug grants: one round, the refusal as it is.
	if _, err, seen := admit([]string{"--privileged"}, fleet.KubeCluster{}, baselinePSA); err == nil || !strings.Contains(err.Error(), "privileged") || len(seen) != 7 {
		t.Errorf("privileged: %v after %d dry-runs", err, len(seen))
	}

	// A quota is no policy: no second round.
	quota := func(_, resource string, _ runtime.Object) error {
		if resource == "pods" {
			return forbidden("pods", "exceeded quota: q, requested: requests.memory=2Gi")
		}
		return nil
	}
	if _, err, seen := admit(debug, fleet.KubeCluster{}, quota); err == nil || !strings.HasPrefix(err.Error(), CodeQuota+": ") || len(seen) != 7 {
		t.Errorf("quota: %v after %d dry-runs", err, len(seen))
	}
}

func podOf(obj runtime.Object) *corev1.Pod {
	p, _ := obj.(*corev1.Pod)
	if p == nil {
		return &corev1.Pod{}
	}
	return p
}

func containsCap(sc *corev1.SecurityContext, c string) bool {
	if sc == nil || sc.Capabilities == nil {
		return false
	}
	for _, a := range sc.Capabilities.Add {
		if string(a) == c {
			return true
		}
	}
	return false
}

func TestApplyBody(t *testing.T) {
	long := int64(1<<62 + 1)
	o := mustRender(t, nil, func(_ *Input, r *RenderInput) {
		r.Cluster.Tolerations = []fleet.KubeToleration{{Key: "k", Operator: "Exists", Effect: "NoExecute", TolerationSeconds: &long}}
	})
	data, err := ApplyBody(o.StatefulSet)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	for _, gone := range []string{`"status"`, `"creationTimestamp"`} {
		if strings.Contains(body, gone) {
			t.Errorf("the apply body holds %s: %s", gone, body)
		}
	}
	for _, kept := range []string{`"apiVersion":"apps/v1"`, `"kind":"StatefulSet"`, `"name":"` + o.StatefulSet.Name + `"`, `"serviceName"`, `"volumeMounts"`,
		`"tolerationSeconds":` + strconv.FormatInt(long, 10)} {
		if !strings.Contains(body, kept) {
			t.Errorf("the apply body lacks %s", kept)
		}
	}
}
