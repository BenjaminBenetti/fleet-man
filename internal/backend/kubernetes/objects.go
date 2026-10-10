//go:build !nok8s

package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// objects.go is how fleet reaches its own objects by kind, as the journal
// and `fleet cluster gc` name the kinds: list by label, get, delete, and a
// merge patch of the metadata. VolumeSnapshots, a CRD clusters may lack, go
// through the raw REST client, and a cluster without the API has none.

// objectMeta is what fleet reads of any object.
type objectMeta struct {
	Kind        string
	Name        string
	UID         string
	Labels      map[string]string
	Annotations map[string]string
	Deleting    bool
}

func metaOf(kind string, m metav1.Object) objectMeta {
	return objectMeta{Kind: kind, Name: m.GetName(), UID: string(m.GetUID()), Labels: m.GetLabels(), Annotations: m.GetAnnotations(), Deleting: m.GetDeletionTimestamp() != nil}
}

// objectKind is one kind's operations in a namespace.
type objectKind struct {
	kind   string
	list   func(ctx context.Context, cs kclient.Interface, ns, selector string) ([]objectMeta, error)
	get    func(ctx context.Context, cs kclient.Interface, ns, name string) (objectMeta, error)
	delete func(ctx context.Context, cs kclient.Interface, ns, name string, opts metav1.DeleteOptions) error
	patch  func(ctx context.Context, cs kclient.Interface, ns, name string, data []byte) error
}

// teardownKinds are the kinds an instance's objects come in, in the order a
// teardown deletes them: controllers before what they own, the disk last.
var teardownKinds = []string{KindStatefulSet, KindDeployment, KindJob, KindPod, KindPDB, KindService, KindNetworkPolicy, KindSecret, KindConfigMap, KindVolumeSnapshot, KindPVC}

// typedClient is what fleet uses of a typed client-go client in a
// namespace.
type typedClient[T metav1.Object, L any] interface {
	List(context.Context, metav1.ListOptions) (L, error)
	Get(context.Context, string, metav1.GetOptions) (T, error)
	Delete(context.Context, string, metav1.DeleteOptions) error
	Patch(context.Context, string, types.PatchType, []byte, metav1.PatchOptions, ...string) (T, error)
}

// typed is a kind served by a typed client; items lists a list's objects,
// skipping those a teardown leaves to their controller.
func typed[T metav1.Object, L any](kind string, client func(kclient.Interface, string) typedClient[T, L], items func(L) []T) objectKind {
	return objectKind{kind: kind,
		list: func(ctx context.Context, cs kclient.Interface, ns, sel string) ([]objectMeta, error) {
			l, err := client(cs, ns).List(ctx, metav1.ListOptions{LabelSelector: sel})
			if err != nil {
				return nil, err
			}
			var out []objectMeta
			for _, o := range items(l) {
				out = append(out, metaOf(kind, o))
			}
			return out, nil
		},
		get: func(ctx context.Context, cs kclient.Interface, ns, name string) (objectMeta, error) {
			o, err := client(cs, ns).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return objectMeta{}, err
			}
			return metaOf(kind, o), nil
		},
		delete: func(ctx context.Context, cs kclient.Interface, ns, name string, opts metav1.DeleteOptions) error {
			return client(cs, ns).Delete(ctx, name, opts)
		},
		patch: func(ctx context.Context, cs kclient.Interface, ns, name string, data []byte) error {
			_, err := client(cs, ns).Patch(ctx, name, types.MergePatchType, data, metav1.PatchOptions{FieldManager: DefaultFieldManager})
			return err
		},
	}
}

// pointers is a list's items, addressed.
func pointers[T any](items []T) []*T {
	out := make([]*T, len(items))
	for i := range items {
		out[i] = &items[i]
	}
	return out
}

// kindOf is kind's operations, or false for a kind fleet does not know.
func kindOf(kind string) (objectKind, bool) {
	switch kind {
	case KindStatefulSet:
		return typed(kind, func(cs kclient.Interface, ns string) typedClient[*appsv1.StatefulSet, *appsv1.StatefulSetList] {
			return cs.AppsV1().StatefulSets(ns)
		}, func(l *appsv1.StatefulSetList) []*appsv1.StatefulSet { return pointers(l.Items) }), true
	case KindDeployment:
		return typed(kind, func(cs kclient.Interface, ns string) typedClient[*appsv1.Deployment, *appsv1.DeploymentList] {
			return cs.AppsV1().Deployments(ns)
		}, func(l *appsv1.DeploymentList) []*appsv1.Deployment { return pointers(l.Items) }), true
	case KindJob:
		return typed(kind, func(cs kclient.Interface, ns string) typedClient[*batchv1.Job, *batchv1.JobList] {
			return cs.BatchV1().Jobs(ns)
		}, func(l *batchv1.JobList) []*batchv1.Job { return pointers(l.Items) }), true
	case KindPod:
		return typed(kind, func(cs kclient.Interface, ns string) typedClient[*corev1.Pod, *corev1.PodList] {
			return cs.CoreV1().Pods(ns)
		}, func(l *corev1.PodList) []*corev1.Pod {
			var out []*corev1.Pod
			for _, p := range pointers(l.Items) {
				if len(p.OwnerReferences) == 0 { // the rest go with their controller
					out = append(out, p)
				}
			}
			return out
		}), true
	case KindPVC:
		return typed(kind, func(cs kclient.Interface, ns string) typedClient[*corev1.PersistentVolumeClaim, *corev1.PersistentVolumeClaimList] {
			return cs.CoreV1().PersistentVolumeClaims(ns)
		}, func(l *corev1.PersistentVolumeClaimList) []*corev1.PersistentVolumeClaim { return pointers(l.Items) }), true
	case KindSecret:
		return typed(kind, func(cs kclient.Interface, ns string) typedClient[*corev1.Secret, *corev1.SecretList] {
			return cs.CoreV1().Secrets(ns)
		}, func(l *corev1.SecretList) []*corev1.Secret { return pointers(l.Items) }), true
	case KindConfigMap:
		return typed(kind, func(cs kclient.Interface, ns string) typedClient[*corev1.ConfigMap, *corev1.ConfigMapList] {
			return cs.CoreV1().ConfigMaps(ns)
		}, func(l *corev1.ConfigMapList) []*corev1.ConfigMap { return pointers(l.Items) }), true
	case KindService:
		return typed(kind, func(cs kclient.Interface, ns string) typedClient[*corev1.Service, *corev1.ServiceList] {
			return cs.CoreV1().Services(ns)
		}, func(l *corev1.ServiceList) []*corev1.Service { return pointers(l.Items) }), true
	case KindNetworkPolicy:
		return typed(kind, func(cs kclient.Interface, ns string) typedClient[*networkingv1.NetworkPolicy, *networkingv1.NetworkPolicyList] {
			return cs.NetworkingV1().NetworkPolicies(ns)
		}, func(l *networkingv1.NetworkPolicyList) []*networkingv1.NetworkPolicy { return pointers(l.Items) }), true
	case KindPDB:
		return typed(kind, func(cs kclient.Interface, ns string) typedClient[*policyv1.PodDisruptionBudget, *policyv1.PodDisruptionBudgetList] {
			return cs.PolicyV1().PodDisruptionBudgets(ns)
		}, func(l *policyv1.PodDisruptionBudgetList) []*policyv1.PodDisruptionBudget { return pointers(l.Items) }), true
	case KindVolumeSnapshot:
		return objectKind{kind: kind, list: listSnapshots, get: getSnapshot, delete: deleteSnapshot, patch: patchSnapshot}, true
	}
	return objectKind{}, false
}

// The VolumeSnapshot API (snapshot.storage.k8s.io/v1), a CRD.
const snapshotPath = "/apis/snapshot.storage.k8s.io/v1/namespaces/"

type snapshotList struct {
	Items []struct {
		Metadata metav1.ObjectMeta `json:"metadata"`
	} `json:"items"`
}

// snapshotResource names the API in errors.
func snapshotResource() schema.GroupResource {
	return schema.GroupResource{Group: "snapshot.storage.k8s.io", Resource: "volumesnapshots"}
}

// snapshotREST is the client the snapshot API is reached through; a fake
// clientset has none.
func snapshotREST(cs kclient.Interface) (rest.Interface, bool) {
	d := cs.Discovery()
	if d == nil {
		return nil, false
	}
	rc := d.RESTClient()
	if p, ok := rc.(*rest.RESTClient); rc == nil || (ok && p == nil) {
		return nil, false
	}
	return rc, true
}

// noSnapshotAPI: the cluster has no VolumeSnapshot API (or fleet may not
// read it), so it has no snapshots of fleet's.
func noSnapshotAPI(err error) bool {
	return apierrors.IsNotFound(err) || apierrors.IsForbidden(err) || apierrors.IsMethodNotSupported(err)
}

func listSnapshots(ctx context.Context, cs kclient.Interface, ns, sel string) ([]objectMeta, error) {
	rc, ok := snapshotREST(cs)
	if !ok {
		return nil, nil
	}
	raw, err := rc.Get().AbsPath(snapshotPath+ns+"/volumesnapshots").Param("labelSelector", sel).Do(ctx).Raw()
	if err != nil {
		if noSnapshotAPI(err) {
			return nil, nil
		}
		return nil, err
	}
	var l snapshotList
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, fmt.Errorf("volumesnapshots: %w", err)
	}
	out := make([]objectMeta, 0, len(l.Items))
	for i := range l.Items {
		out = append(out, metaOf(KindVolumeSnapshot, &l.Items[i].Metadata))
	}
	return out, nil
}

func getSnapshot(ctx context.Context, cs kclient.Interface, ns, name string) (objectMeta, error) {
	rc, ok := snapshotREST(cs)
	if !ok {
		return objectMeta{}, apierrors.NewNotFound(snapshotResource(), name)
	}
	raw, err := rc.Get().AbsPath(snapshotPath + ns + "/volumesnapshots/" + name).Do(ctx).Raw()
	if err != nil {
		return objectMeta{}, err
	}
	var o struct {
		Metadata metav1.ObjectMeta `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return objectMeta{}, fmt.Errorf("volumesnapshot %s: %w", name, err)
	}
	return metaOf(KindVolumeSnapshot, &o.Metadata), nil
}

func deleteSnapshot(ctx context.Context, cs kclient.Interface, ns, name string, opts metav1.DeleteOptions) error {
	rc, ok := snapshotREST(cs)
	if !ok {
		return apierrors.NewNotFound(snapshotResource(), name)
	}
	body, err := json.Marshal(opts)
	if err != nil {
		return err
	}
	return rc.Delete().AbsPath(snapshotPath + ns + "/volumesnapshots/" + name).Body(body).Do(ctx).Error()
}

func patchSnapshot(ctx context.Context, cs kclient.Interface, ns, name string, data []byte) error {
	rc, ok := snapshotREST(cs)
	if !ok {
		return apierrors.NewNotFound(snapshotResource(), name)
	}
	return rc.Patch(types.MergePatchType).AbsPath(snapshotPath+ns+"/volumesnapshots/"+name).Param("fieldManager", DefaultFieldManager).Body(data).Do(ctx).Error()
}

// gone reports whether err says the object is not there (any more).
func gone(err error) bool {
	if apierrors.IsNotFound(err) || apierrors.IsGone(err) {
		return true
	}
	var se apierrors.APIStatus
	return errors.As(err, &se) && se.Status().Code == http.StatusNotFound
}

// annotationPatch is the merge patch setting annotation key to value, or
// removing it when value is nil.
func annotationPatch(key string, value *string) []byte {
	v := "null"
	if value != nil {
		b, _ := json.Marshal(*value)
		v = string(b)
	}
	k, _ := json.Marshal(key)
	return []byte(`{"metadata":{"annotations":{` + string(k) + `:` + v + `}}}`)
}

// objectRef is "kind/name", as reports and fleet.log name an object.
func objectRef(kind, name string) string { return kind + "/" + name }

// joinErrors is errs as one error, or nil.
func joinErrors(errs []error) error {
	switch len(errs) {
	case 0:
		return nil
	case 1:
		return errs[0]
	}
	msgs := make([]string, len(errs))
	for i, e := range errs {
		msgs[i] = e.Error()
	}
	return fmt.Errorf("%s", strings.Join(msgs, "; "))
}
