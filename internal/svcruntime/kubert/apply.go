//go:build !nok8s

package kubert

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kclient "k8s.io/client-go/kubernetes"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/podspec"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
)

// apply.go server-side applies a fleet's objects, never over one that is not
// this daemon's.

// ForeignError is an object fleet would apply that exists already but is not
// this daemon's object for the fleet: its labels name another daemon, another
// connection, another fleet or another role (or none). Fleet leaves it alone.
type ForeignError struct {
	Object string
	Why    string
}

func (e *ForeignError) Error() string {
	return fmt.Sprintf("%s exists in the namespace but is not this fleet's (%s); fleet leaves it alone: delete or rename it to let fleet create its own", e.Object, e.Why)
}

// objectOps is one kind's typed operations in a namespace.
type objectOps struct {
	kind  string
	get   func(ctx context.Context, name string) (metav1.Object, error)
	patch func(ctx context.Context, name string, body []byte, opts metav1.PatchOptions) (metav1.Object, error)
	del   func(ctx context.Context, name string, opts metav1.DeleteOptions) error
}

// opsFor is obj's kind's operations, by obj's type.
func opsFor(cs kclient.Interface, ns string, obj metav1.Object) (objectOps, error) {
	switch obj.(type) {
	case *corev1.ConfigMap:
		c := cs.CoreV1().ConfigMaps(ns)
		return objectOps{kind: kubernetes.KindConfigMap,
			get: func(ctx context.Context, n string) (metav1.Object, error) { return c.Get(ctx, n, metav1.GetOptions{}) },
			patch: func(ctx context.Context, n string, b []byte, o metav1.PatchOptions) (metav1.Object, error) {
				return c.Patch(ctx, n, types.ApplyPatchType, b, o)
			},
			del: c.Delete}, nil
	case *corev1.Service:
		c := cs.CoreV1().Services(ns)
		return objectOps{kind: kubernetes.KindService,
			get: func(ctx context.Context, n string) (metav1.Object, error) { return c.Get(ctx, n, metav1.GetOptions{}) },
			patch: func(ctx context.Context, n string, b []byte, o metav1.PatchOptions) (metav1.Object, error) {
				return c.Patch(ctx, n, types.ApplyPatchType, b, o)
			},
			del: c.Delete}, nil
	case *corev1.PersistentVolumeClaim:
		c := cs.CoreV1().PersistentVolumeClaims(ns)
		return objectOps{kind: kubernetes.KindPVC,
			get: func(ctx context.Context, n string) (metav1.Object, error) { return c.Get(ctx, n, metav1.GetOptions{}) },
			patch: func(ctx context.Context, n string, b []byte, o metav1.PatchOptions) (metav1.Object, error) {
				return c.Patch(ctx, n, types.ApplyPatchType, b, o)
			},
			del: c.Delete}, nil
	case *networkingv1.NetworkPolicy:
		c := cs.NetworkingV1().NetworkPolicies(ns)
		return objectOps{kind: kubernetes.KindNetworkPolicy,
			get: func(ctx context.Context, n string) (metav1.Object, error) { return c.Get(ctx, n, metav1.GetOptions{}) },
			patch: func(ctx context.Context, n string, b []byte, o metav1.PatchOptions) (metav1.Object, error) {
				return c.Patch(ctx, n, types.ApplyPatchType, b, o)
			},
			del: c.Delete}, nil
	case *appsv1.Deployment:
		c := cs.AppsV1().Deployments(ns)
		return objectOps{kind: kubernetes.KindDeployment,
			get: func(ctx context.Context, n string) (metav1.Object, error) { return c.Get(ctx, n, metav1.GetOptions{}) },
			patch: func(ctx context.Context, n string, b []byte, o metav1.PatchOptions) (metav1.Object, error) {
				return c.Patch(ctx, n, types.ApplyPatchType, b, o)
			},
			del: c.Delete}, nil
	}
	return objectOps{}, fmt.Errorf("kubernetes runtime: no apply for a %T", obj)
}

// gone reports whether err says the object is not there (any more).
func gone(err error) bool {
	if apierrors.IsNotFound(err) || apierrors.IsGone(err) {
		return true
	}
	var se apierrors.APIStatus
	return errors.As(err, &se) && se.Status().Code == http.StatusNotFound
}

// ours explains why live is not obj as this daemon makes it ("" when it is):
// the managed-by, owner, connection, fleet and role labels must all agree.
func ours(live, obj metav1.Object) string {
	want, have := obj.GetLabels(), live.GetLabels()
	for _, k := range []string{names.LabelManagedBy, names.LabelOwner, names.LabelCluster, names.LabelFleet, names.LabelRole, names.LabelService} {
		if want[k] != have[k] {
			if have[k] == "" {
				return "it has no " + k + " label"
			}
			return fmt.Sprintf("its %s label is %q", k, have[k])
		}
	}
	return ""
}

// ownedBy reports whether o names uid among its owners.
func ownedBy(o metav1.Object, uid types.UID) bool {
	for _, ref := range o.GetOwnerReferences() {
		if ref.UID == uid {
			return true
		}
	}
	return false
}

// apply server-side applies obj (fleet's field manager, forced over other
// managers' changes to fleet's fields) and returns it as the API server
// stored it. An object of the same name that is not this daemon's is
// refused (ForeignError); one being deleted is waited out first, and so is
// one that names an anchor other than its owner in obj (a dependent of a
// deleted anchor, which the garbage collector would delete under it).
func (c *conn) apply(ctx context.Context, obj metav1.Object) (metav1.Object, error) {
	ops, err := opsFor(c.scope.Client, c.scope.Namespace, obj)
	if err != nil {
		return nil, err
	}
	name := obj.GetName()
	ref := ops.kind + "/" + name
	live, err := ops.get(ctx, name)
	switch {
	case gone(err):
	case err != nil:
		return nil, fmt.Errorf("reading %s: %w", ref, err)
	default:
		if why := ours(live, obj); why != "" {
			return nil, &ForeignError{Object: ref, Why: why}
		}
		stale := false
		for _, want := range obj.GetOwnerReferences() {
			if !ownedBy(live, want.UID) {
				stale = true
			}
		}
		if live.GetDeletionTimestamp() == nil && stale {
			if err := c.delete(ctx, ops, name, live.GetUID()); err != nil {
				return nil, fmt.Errorf("replacing %s, which belongs to an earlier anchor: %w", ref, err)
			}
		}
		if live.GetDeletionTimestamp() != nil || stale {
			if err := waitGone(ctx, ops, name, live.GetUID()); err != nil {
				return nil, fmt.Errorf("%s is being deleted: %w", ref, err)
			}
		}
	}
	body, err := podspec.ApplyBody(obj)
	if err != nil {
		return nil, err
	}
	force := true
	stored, err := ops.patch(ctx, name, body, metav1.PatchOptions{FieldManager: kubernetes.DefaultFieldManager, Force: &force})
	if err != nil {
		return nil, fmt.Errorf("applying %s: %w", ref, err)
	}
	return stored, nil
}

// delete deletes name (under uid, when given), after the owner check.
func (c *conn) delete(ctx context.Context, ops objectOps, name string, uid types.UID) error {
	if gate := c.scope.Gate; gate != nil {
		if err := gate(ctx); err != nil {
			return err
		}
	}
	propagation := metav1.DeletePropagationBackground
	opts := metav1.DeleteOptions{PropagationPolicy: &propagation}
	if uid != "" {
		opts.Preconditions = &metav1.Preconditions{UID: &uid}
	}
	if err := ops.del(ctx, name, opts); err != nil && !gone(err) && !apierrors.IsConflict(err) {
		return err
	}
	flog.Info("kube: deleted", "cluster", c.cluster, "namespace", c.scope.Namespace, "object", ops.kind+"/"+name)
	return nil
}

// waitGone waits for name, under uid, to be gone (within goneTimeout).
func waitGone(ctx context.Context, ops objectOps, name string, uid types.UID) error {
	ctx, cancel := context.WithTimeout(ctx, goneTimeout)
	defer cancel()
	for {
		live, err := ops.get(ctx, name)
		switch {
		case gone(err):
			return nil
		case err == nil && live.GetUID() != uid:
			return nil // made again meanwhile: not the object waited for
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("still there after %s", goneTimeout)
		case <-time.After(pollInterval):
		}
	}
}
