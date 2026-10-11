//go:build !nok8s

package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kclient "k8s.io/client-go/kubernetes"
)

// snapshot.go is the VolumeSnapshot API (snapshot.storage.k8s.io/v1, a CRD
// that clusters may lack) as a clone uses it: the snapshot of a source disk
// a clone's disk is restored from, and the classes that take snapshots of a
// CSI driver's volumes. It goes through the raw REST client, as the rest of
// fleet's snapshot reads do (objects.go).

// snapshotGroup is the API group a PVC's dataSource names a snapshot by.
const snapshotGroup = "snapshot.storage.k8s.io"

// annotationDefaultSnapshotClass marks the default class of its driver.
const annotationDefaultSnapshotClass = "snapshot.storage.kubernetes.io/is-default-class"

// volumeSnapshot is a VolumeSnapshot's body: what fleet writes and reads.
type volumeSnapshot struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   metav1.ObjectMeta `json:"metadata"`
	Spec       struct {
		VolumeSnapshotClassName string `json:"volumeSnapshotClassName,omitempty"`
		Source                  struct {
			PersistentVolumeClaimName string `json:"persistentVolumeClaimName,omitempty"`
		} `json:"source"`
	} `json:"spec"`
	Status *snapshotStatus `json:"status,omitempty"`
}

// snapshotStatus is how far a snapshot is: CreationTime is set once the
// driver has cut it (the point in time it holds), ReadyToUse once a disk can
// be restored from it.
type snapshotStatus struct {
	CreationTime *metav1.Time `json:"creationTime,omitempty"`
	ReadyToUse   *bool        `json:"readyToUse,omitempty"`
	RestoreSize  string       `json:"restoreSize,omitempty"`
	Error        *struct {
		Message string `json:"message,omitempty"`
	} `json:"error,omitempty"`
}

func (s *snapshotStatus) cut() bool   { return s != nil && s.CreationTime != nil }
func (s *snapshotStatus) ready() bool { return s != nil && s.ReadyToUse != nil && *s.ReadyToUse }
func (s *snapshotStatus) failure() string {
	if s == nil || s.Error == nil {
		return ""
	}
	return s.Error.Message
}

// snapshotBody is the apply body of the VolumeSnapshot name of the PVC
// claim, with class, carrying labels.
func snapshotBody(name, ns, class, claim string, labels map[string]string) ([]byte, error) {
	var v volumeSnapshot
	v.APIVersion, v.Kind = snapshotGroup+"/v1", "VolumeSnapshot"
	v.Metadata = metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels}
	v.Spec.VolumeSnapshotClassName = class
	v.Spec.Source.PersistentVolumeClaimName = claim
	return json.Marshal(v)
}

// applySnapshot server-side applies a VolumeSnapshot body and returns its
// UID.
func applySnapshot(ctx context.Context, cs kclient.Interface, ns, name string, body []byte) (string, error) {
	rc, ok := snapshotREST(cs)
	if !ok {
		return "", apierrors.NewNotFound(snapshotResource(), name)
	}
	raw, err := rc.Patch(types.ApplyPatchType).AbsPath(snapshotPath+ns+"/volumesnapshots/"+name).
		Param("fieldManager", DefaultFieldManager).Param("force", "true").Body(body).Do(ctx).Raw()
	if err != nil {
		return "", err
	}
	var v volumeSnapshot
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", fmt.Errorf("volumesnapshot %s: %w", name, err)
	}
	return string(v.Metadata.UID), nil
}

// readSnapshot reads a VolumeSnapshot.
func readSnapshot(ctx context.Context, cs kclient.Interface, ns, name string) (*volumeSnapshot, error) {
	rc, ok := snapshotREST(cs)
	if !ok {
		return nil, apierrors.NewNotFound(snapshotResource(), name)
	}
	raw, err := rc.Get().AbsPath(snapshotPath + ns + "/volumesnapshots/" + name).Do(ctx).Raw()
	if err != nil {
		return nil, err
	}
	var v volumeSnapshot
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("volumesnapshot %s: %w", name, err)
	}
	return &v, nil
}

// snapshotClassOf is the VolumeSnapshotClass that takes snapshots of
// driver's volumes: the one named by setting (when it serves the driver),
// else the driver's default, else the first by name; "" when none (or the
// classes cannot be read: snapshots are optional).
func snapshotClassOf(ctx context.Context, cs kclient.Interface, driver, setting string) string {
	if driver == "" {
		return ""
	}
	rc, ok := snapshotREST(cs)
	if !ok {
		return ""
	}
	raw, err := rc.Get().AbsPath("/apis/" + snapshotGroup + "/v1/volumesnapshotclasses").Do(ctx).Raw()
	if err != nil {
		return ""
	}
	var list struct {
		Items []struct {
			Metadata metav1.ObjectMeta `json:"metadata"`
			Driver   string            `json:"driver"`
		} `json:"items"`
	}
	if json.Unmarshal(raw, &list) != nil {
		return ""
	}
	var names []string
	def := ""
	for _, c := range list.Items {
		if c.Driver != driver {
			continue
		}
		if setting != "" {
			if c.Metadata.Name == setting {
				return setting
			}
			continue
		}
		names = append(names, c.Metadata.Name)
		if c.Metadata.Annotations[annotationDefaultSnapshotClass] == "true" && (def == "" || c.Metadata.Name < def) {
			def = c.Metadata.Name
		}
	}
	if setting != "" {
		return ""
	}
	if def != "" {
		return def
	}
	sort.Strings(names)
	if len(names) > 0 {
		return names[0]
	}
	return ""
}

// snapshotSource is a PVC dataSource naming a VolumeSnapshot.
func snapshotSource(name string) *corev1.TypedLocalObjectReference {
	g := snapshotGroup
	return &corev1.TypedLocalObjectReference{APIGroup: &g, Kind: "VolumeSnapshot", Name: name}
}

// pvcSource is a PVC dataSource naming another PVC (a CSI clone of it).
func pvcSource(name string) *corev1.TypedLocalObjectReference {
	return &corev1.TypedLocalObjectReference{Kind: "PersistentVolumeClaim", Name: name}
}

// ListSnapshots lists the VolumeSnapshots in namespace ns that selector
// matches, by their metadata: none on a cluster without the API (or where
// fleet may not read it).
func ListSnapshots(ctx context.Context, cs kclient.Interface, ns, selector string) ([]metav1.ObjectMeta, error) {
	rc, ok := snapshotREST(cs)
	if !ok {
		return nil, nil
	}
	raw, err := rc.Get().AbsPath(snapshotPath+ns+"/volumesnapshots").Param("labelSelector", selector).Do(ctx).Raw()
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
	out := make([]metav1.ObjectMeta, 0, len(l.Items))
	for _, it := range l.Items {
		out = append(out, it.Metadata)
	}
	return out, nil
}

// DeleteSnapshot deletes the VolumeSnapshot name in namespace ns.
func DeleteSnapshot(ctx context.Context, cs kclient.Interface, ns, name string, opts metav1.DeleteOptions) error {
	return deleteSnapshot(ctx, cs, ns, name, opts)
}
