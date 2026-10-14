//go:build !nok8s

package podspec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
)

// DryRun has the API server admit an instance generation's objects without
// creating any (dryRun=All): the pod the StatefulSet would create and the
// StatefulSet itself (a controller-created pod can meet other rules than
// its controller), the seed Job and its pod, the disk, the Secrets and the
// disruption budget. Admission runs in full: pod security, webhooks,
// admission policies, LimitRanges, ResourceQuotas.
//
// It returns nil when every object is admitted, the refusals joined (each a
// *Denial; Denials lists them, the first one's code leads) when any is not,
// and any other error (the API server unreachable) as it is.
//
// A re-render of an existing instance is checked as what applying it does:
// the StatefulSet and the budget are applied (server-side), so an existing
// one is checked as the update it is; a disk, Secret or Job that exists
// already is not created again and is not asked about (its admission was
// when it was created, and a count quota it fills would refuse its
// duplicate). The pods are asked about as new pods (a generated name), as
// the controllers create them: while an instance's old pod still runs it
// counts against the namespace's quota beside the new one, so a quota
// refusal of a re-render made before the old pod is gone may not hold
// once it is.
//
// DryRun is the one function in the package that talks to the API server.
func DryRun(ctx context.Context, api kubernetes.Interface, o *Objects) error {
	ns := o.StatefulSet.Namespace
	create := metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}, FieldManager: FieldManager}
	apply := metav1.PatchOptions{DryRun: []string{metav1.DryRunAll}, FieldManager: FieldManager, Force: ptr(true)}
	type check struct {
		object string
		// exists reports whether the object is there already; nil asks
		// regardless.
		exists func() (bool, error)
		run    func() error
	}
	found := func(err error) (bool, error) {
		switch {
		case err == nil:
			return true, nil
		case apierrors.IsNotFound(err):
			return false, nil
		case apierrors.IsForbidden(err):
			// Not allowed to look: ask, and the create says what it may do.
			return false, nil
		}
		return false, err
	}
	pod := o.Pod()
	pod.Name, pod.GenerateName = "", pod.Name+"-"
	rendered := fsGroupOf(&o.StatefulSet.Spec.Template.Spec)
	var admitted *corev1.Pod
	checks := []check{
		{object: "Pod " + names.Pod(o.StatefulSet.Name), run: func() error {
			got, err := api.CoreV1().Pods(ns).Create(ctx, pod, create)
			admitted = got
			return err
		}},
		{object: "StatefulSet " + o.StatefulSet.Name, run: func() error {
			data, err := ApplyBody(o.StatefulSet)
			if err != nil {
				return err
			}
			_, err = api.AppsV1().StatefulSets(ns).Patch(ctx, o.StatefulSet.Name, types.ApplyPatchType, data, apply)
			return err
		}},
	}
	if o.Seed != nil {
		job := o.Seed.Job
		seedPod := o.Seed.Pod()
		checks = append(checks,
			check{object: "Pod " + seedPod.GenerateName + "*", run: func() error {
				_, err := api.CoreV1().Pods(ns).Create(ctx, seedPod, create)
				return err
			}},
			check{object: "Job " + job.Name,
				exists: func() (bool, error) {
					_, err := api.BatchV1().Jobs(ns).Get(ctx, job.Name, metav1.GetOptions{})
					return found(err)
				},
				run: func() error {
					_, err := api.BatchV1().Jobs(ns).Create(ctx, job, create)
					return err
				}})
	}
	checks = append(checks, check{object: "PersistentVolumeClaim " + o.Disk.Name,
		exists: func() (bool, error) {
			_, err := api.CoreV1().PersistentVolumeClaims(ns).Get(ctx, o.Disk.Name, metav1.GetOptions{})
			return found(err)
		},
		run: func() error {
			_, err := api.CoreV1().PersistentVolumeClaims(ns).Create(ctx, o.Disk, create)
			return err
		}})
	for _, secret := range []*corev1.Secret{o.SpecSecret, o.EnvSecret} {
		checks = append(checks, check{object: "Secret " + secret.Name,
			exists: func() (bool, error) {
				_, err := api.CoreV1().Secrets(ns).Get(ctx, secret.Name, metav1.GetOptions{})
				return found(err)
			},
			run: func() error {
				_, err := api.CoreV1().Secrets(ns).Create(ctx, secret, create)
				return err
			}})
	}
	if b := o.DisruptionBudget; b != nil {
		checks = append(checks, check{object: "PodDisruptionBudget " + b.Name, run: func() error {
			data, err := ApplyBody(b)
			if err != nil {
				return err
			}
			_, err = api.PolicyV1().PodDisruptionBudgets(ns).Patch(ctx, b.Name, types.ApplyPatchType, data, apply)
			return err
		}})
	}

	var ds []*Denial
	for _, c := range checks {
		if c.exists != nil {
			there, err := c.exists()
			if err != nil {
				return fmt.Errorf("looking up %s: %w", c.object, err)
			}
			if there {
				continue
			}
		}
		err := c.run()
		if err == nil || apierrors.IsAlreadyExists(err) {
			continue
		}
		d := DenialOf(c.object, err)
		if d == nil {
			return fmt.Errorf("dry-run of %s: %w", c.object, err)
		}
		ds = addDenial(ds, d)
	}
	if len(ds) == 0 {
		if w := injectedFSGroupWarning(rendered, admitted); w != "" && !slices.Contains(o.Warnings, w) {
			o.Warnings = append(o.Warnings, w)
		}
		return nil
	}
	return &denials{ds: ds}
}

// fsGroupOf is a pod spec's fsGroup, nil without one.
func fsGroupOf(spec *corev1.PodSpec) *int64 {
	if spec.SecurityContext == nil {
		return nil
	}
	return spec.SecurityContext.FSGroup
}

// injectedFSGroupWarning is the warning of an admission that gives the
// instance's pod an fsGroup fleet did not render (rendered; an OpenShift SCC
// sets one from the namespace's range): the kubelet then re-owns the pod's
// volumes to that group as they mount and makes their files group-readable
// and writable, so the share's 0600 agent logins become 0660, readable by
// every pod the cluster gives that group. "" when it set none, or fleet's.
func injectedFSGroupWarning(rendered *int64, admitted *corev1.Pod) string {
	if admitted == nil {
		return ""
	}
	got := fsGroupOf(&admitted.Spec)
	if got == nil || (rendered != nil && *rendered == *got) {
		return ""
	}
	fleets := "none"
	if rendered != nil {
		fleets = strconv.FormatInt(*rendered, 10)
	}
	return fmt.Sprintf("the cluster's admission gives the instance's pod fsGroup %d (fleet sets %s; an OpenShift SCC or another mutating policy): Kubernetes re-owns the pod's volumes to group %d as they mount and makes their files group-readable and writable, so the fleet's share's private files (agent logins, 0600) become 0660, readable by every pod the cluster gives group %d", *got, fleets, *got, *got)
}

// Admitted is a translation and its render, admitted by the API server.
type Admitted struct {
	Result  *Result
	Objects *Objects
}

// Admit translates a run, renders it and has the API server admit the
// render (DryRun), applying the debug-capabilities policy: when admission
// refuses a render that carries debug grants, it renders without them and
// asks again, and the second answer decides.
//
//   - Admitted without the grants: they alone were refused. With
//     debug_capabilities auto that render is the result (its warnings say
//     the grants were dropped, DebugDroppedWarning); with require the create
//     fails with kubernetes:admission_denied:debug_capabilities.
//   - Refused again: something else is refused too, and the create fails
//     with the second refusal, which names only that.
//
// A refusal that is not admission policy (a quota, a LimitRange, a missing
// service account) fails as it is: dropping grants cannot get past it.
// After an apply, a FailedCreate (ClassifyFailedCreate) means the cluster's
// admission changed since; Admit again decides it the same way.
func Admit(ctx context.Context, api kubernetes.Interface, in Input, r RenderInput) (*Admitted, error) {
	if r.Cluster == nil {
		r.Cluster = in.Cluster
	}
	// try translates and renders an input and asks the API server about it;
	// denied is DryRun's answer.
	type attempt struct {
		*Admitted
		denied error
	}
	try := func(in Input) (attempt, error) {
		res, err := Translate(in)
		if err != nil {
			return attempt{}, err
		}
		o, err := Render(res, r)
		if err != nil {
			return attempt{}, err
		}
		return attempt{&Admitted{Result: res, Objects: o}, DryRun(ctx, api, o)}, nil
	}
	first, err := try(in)
	if err != nil {
		return nil, err
	}
	if first.denied == nil {
		return first.Admitted, nil
	}
	ds := Denials(first.denied)
	if len(ds) == 0 {
		// Not a refusal: the API server could not be asked.
		return nil, first.denied
	}
	policy := false
	for _, d := range ds {
		policy = policy || d.Policy()
	}
	pod := &first.Result.Pod
	if !policy || len(pod.DebugGrants) == 0 || pod.DebugDropped {
		return nil, first.denied
	}
	in.DropDebugGrants = true
	second, err := try(in)
	if err != nil {
		return nil, err
	}
	if second.denied != nil {
		return nil, second.denied
	}
	if rerender, err := Denied(r.Cluster, pod, policyDetail(ds)); !rerender {
		return nil, err
	}
	return second.Admitted, nil
}

// ApplyBody is obj as a server-side apply body: its JSON without the
// status, which an apply of the object itself does not set, and without
// the empty creation times (the object's and its pod template's), which
// the server owns.
func ApplyBody(obj any) ([]byte, error) {
	data, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	// Numbers stay as written: an int64 (a toleration's seconds) is not
	// a float64.
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	delete(m, "status")
	dropNullCreation(m)
	return json.Marshal(m)
}

func dropNullCreation(v any) {
	switch x := v.(type) {
	case map[string]any:
		if t, ok := x["creationTimestamp"]; ok && t == nil {
			delete(x, "creationTimestamp")
		}
		for _, e := range x {
			dropNullCreation(e)
		}
	case []any:
		for _, e := range x {
			dropNullCreation(e)
		}
	}
}

// policyDetail is what the policy denials say, for the create's error.
func policyDetail(ds []*Denial) string {
	out := ""
	for _, d := range ds {
		if !d.Policy() {
			continue
		}
		if out != "" {
			out += "; "
		}
		out += d.Object + ": " + denialDetail(d.Message)
	}
	return out
}
