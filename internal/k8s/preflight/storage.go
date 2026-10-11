//go:build !nok8s

package preflight

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/podspec"
)

// storage.go classifies the cluster's StorageClasses for instance disks and
// picks auto's class (KubeCapabilities.instance_storage_class, which new
// instance disks are made in). Fleet shares' class is share.go's, from the
// multi-node test (storagetest.go).

// nodeLocalProvisioners keep a volume on one node's disk: its data goes with
// the node.
var nodeLocalProvisioners = map[string]bool{
	"rancher.io/local-path":            true,
	"k8s.io/minikube-hostpath":         true,
	"hostpath.csi.k8s.io":              true,
	"kubernetes.io/no-provisioner":     true,
	"openebs.io/local":                 true,
	"local.csi.openebs.io":             true,
	"topolvm.io":                       true,
	"topolvm.cybozu.com":               true,
	"microk8s.io/hostpath":             true,
	"docker.io/hostpath":               true,
	"kubevirt.io.hostpath-provisioner": true,
}

// fileShareMarkers mark provisioners of shared network file systems, which
// are not block disks.
var fileShareMarkers = []string{"nfs", "efs.csi.aws.com", "file.csi.azure.com", "azure-file", "filestore.csi.storage.gke.io", "cephfs", "smb", "glusterfs"}

// nfsMarkers mark the file systems whose root_squash breaks package installs
// on an instance disk.
var nfsMarkers = []string{"nfs", "efs.csi.aws.com", "filestore.csi.storage.gke.io"}

const annotationDefaultClass = "storageclass.kubernetes.io/is-default-class"

func containsAny(s string, markers []string) bool {
	s = strings.ToLower(s)
	for _, m := range markers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// ClassifyStorageClass describes one StorageClass. drivers are the CSI
// drivers the cluster lists (for SELinux mount support).
func ClassifyStorageClass(sc *storagev1.StorageClass, drivers map[string]*storagev1.CSIDriver) *fleetgrpc.KubeStorageClass {
	ks := &fleetgrpc.KubeStorageClass{
		Name:          sc.Name,
		Provisioner:   sc.Provisioner,
		IsDefault:     sc.Annotations[annotationDefaultClass] == "true",
		Block:         !containsAny(sc.Provisioner, fileShareMarkers),
		NodeLocal:     nodeLocalProvisioners[sc.Provisioner],
		BindingMode:   string(storagev1.VolumeBindingImmediate),
		ReclaimPolicy: string(corev1.PersistentVolumeReclaimDelete),
	}
	if sc.VolumeBindingMode != nil {
		ks.BindingMode = string(*sc.VolumeBindingMode)
	}
	if sc.AllowVolumeExpansion != nil {
		ks.AllowExpansion = *sc.AllowVolumeExpansion
	}
	if sc.ReclaimPolicy != nil {
		ks.ReclaimPolicy = string(*sc.ReclaimPolicy)
	}
	if d := drivers[sc.Provisioner]; d != nil && d.Spec.SELinuxMount != nil {
		ks.SelinuxMount = *d.Spec.SELinuxMount
	}
	// A CSI clone needs a CSI driver: an in-tree or external provisioner
	// takes a data source it does not know for an empty disk.
	ks.CsiClone = drivers[sc.Provisioner] != nil && podspec.ClonesVolumes(sc.Provisioner)
	return ks
}

// PickInstanceClass is auto's instance-disk class: a block class, preferring
// (in this order) one that is not node-local on a multi-node cluster, one
// with a snapshot class for fast clones, WaitForFirstConsumer binding,
// expansion, and the cluster default. Only file-share classes left: the best
// of those. None: nil.
func PickInstanceClass(classes []*fleetgrpc.KubeStorageClass, snapshotDrivers map[string]bool, multiNode bool) *fleetgrpc.KubeStorageClass {
	var best *fleetgrpc.KubeStorageClass
	bestScore := -1
	for _, c := range classes {
		score := 0
		if c.Block {
			score += 64
		}
		if multiNode && !c.NodeLocal {
			score += 32
		}
		if snapshotDrivers[c.Provisioner] {
			score += 8
		}
		if c.BindingMode == string(storagev1.VolumeBindingWaitForFirstConsumer) {
			score += 4
		}
		if c.AllowExpansion {
			score += 2
		}
		if c.IsDefault {
			score++
		}
		if score > bestScore || (score == bestScore && c.Name < best.Name) {
			best, bestScore = c, score
		}
	}
	return best
}

// snapshotClass is the part of a VolumeSnapshotClass fleet reads.
type snapshotClass struct {
	Metadata metav1.ObjectMeta `json:"metadata"`
	Driver   string            `json:"driver"`
}

// snapshotClasses reads the cluster's VolumeSnapshotClasses through the raw
// API (they are a CRD). ok is false when they could not be read; a cluster
// without the CRD has none.
func (r *run) snapshotClasses(ctx context.Context) (classes []snapshotClass, ok bool, why string) {
	if r.t.Raw == nil {
		return nil, false, "not read"
	}
	cctx, cancel := call(ctx)
	defer cancel()
	body, err := r.t.Raw(cctx, "/apis/snapshot.storage.k8s.io/v1/volumesnapshotclasses")
	switch {
	case apierrors.IsNotFound(err):
		return nil, true, ""
	case apierrors.IsForbidden(err):
		return nil, false, "VolumeSnapshotClasses are not readable (optional)"
	case err != nil:
		return nil, false, Message(err)
	}
	var list struct {
		Items []snapshotClass `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, false, "decoding VolumeSnapshotClasses: " + err.Error()
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Metadata.Name < list.Items[j].Metadata.Name })
	return list.Items, true, ""
}

func (r *run) rootStorage(ctx context.Context) {
	r.running(CheckRootStorage)
	snaps, snapsOK, snapsWhy := r.snapshotClasses(ctx)
	snapDrivers := map[string]bool{}
	for _, s := range snaps {
		snapDrivers[s.Driver] = true
		r.caps.SnapshotClasses = append(r.caps.SnapshotClasses, s.Metadata.Name)
	}
	defer r.reportSnapshots(snaps, snapsOK, snapsWhy)

	cctx, cancel := call(ctx)
	defer cancel()
	list, err := r.t.Client.StorageV1().StorageClasses().List(cctx, metav1.ListOptions{})
	want := r.t.Cluster.StorageClass
	r.caps.InstanceStorageClass = want
	if err != nil {
		if apierrors.IsForbidden(err) {
			r.classesUnreadable = true
			if want != "" {
				r.warn(CheckRootStorage, fmt.Sprintf("StorageClasses are not readable; using %q as set", want), "")
			} else {
				r.warn(CheckRootStorage, "StorageClasses are not readable: instance disks use the cluster's default class", "set the instance disk class by name")
			}
			return
		}
		r.warn(CheckRootStorage, "listing StorageClasses: "+Message(err), "")
		return
	}
	drivers := map[string]*storagev1.CSIDriver{}
	if dl, err := r.t.Client.StorageV1().CSIDrivers().List(cctx, metav1.ListOptions{}); err == nil {
		for i := range dl.Items {
			drivers[dl.Items[i].Name] = &dl.Items[i]
		}
	}
	r.classObjs = map[string]*storagev1.StorageClass{}
	for i := range list.Items {
		r.classObjs[list.Items[i].Name] = &list.Items[i]
		r.caps.StorageClasses = append(r.caps.StorageClasses, ClassifyStorageClass(&list.Items[i], drivers))
	}
	sort.Slice(r.caps.StorageClasses, func(i, j int) bool { return r.caps.StorageClasses[i].Name < r.caps.StorageClasses[j].Name })

	var chosen *fleetgrpc.KubeStorageClass
	auto := want == ""
	if auto {
		chosen = PickInstanceClass(r.caps.StorageClasses, snapDrivers, r.multiNode)
		if chosen == nil {
			msg := "no StorageClass: instance disks (the persistent root) need one"
			r.fail(CheckRootStorage, msg, "install a CSI driver with a block StorageClass")
			r.failState(fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_MISCONFIGURED, msg)
			return
		}
	} else {
		for _, c := range r.caps.StorageClasses {
			if c.Name == want {
				chosen = c
			}
		}
		if chosen == nil {
			msg := fmt.Sprintf("storage class %q does not exist", want)
			r.fail(CheckRootStorage, msg, "pick one the cluster offers, or clear the setting for auto")
			r.failState(fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_MISCONFIGURED, msg)
			return
		}
	}
	r.rootClass = chosen
	r.caps.InstanceStorageClass = chosen.Name
	r.fillAttachLimits(ctx, chosen.Provisioner)
	switch {
	case snapDrivers[chosen.Provisioner]:
		r.caps.CloneTier = "snapshot"
	case chosen.CsiClone:
		r.caps.CloneTier = "csi-clone"
	default:
		r.caps.CloneTier = "copy"
	}

	traits := []string{}
	if chosen.Block {
		traits = append(traits, "RWO")
	} else {
		traits = append(traits, "file share")
	}
	traits = append(traits, bindingLabel(chosen.BindingMode))
	if chosen.AllowExpansion {
		traits = append(traits, "expandable")
	}
	if chosen.NodeLocal {
		traits = append(traits, "node-local")
	} else {
		traits = append(traits, "network")
	}
	detail := chosen.Name + " (" + strings.Join(traits, ", ") + ")"
	if auto {
		detail = "auto → " + detail
	}

	var warns []string
	if chosen.NodeLocal && r.multiNode {
		r.warning("node-local storage: data lost if node replaced")
		if !r.t.Cluster.NodeLocalAck {
			warns = append(warns, "node-local storage on a multi-node cluster: an instance's data is lost with its node")
			r.limit("node-local storage")
		}
	}
	if containsAny(chosen.Provisioner, nfsMarkers) {
		warns = append(warns, "an NFS class for instance disks: root_squash breaks package installs")
	} else if !chosen.Block {
		warns = append(warns, "a network file share for instance disks")
	}
	if !chosen.Block {
		r.limit("instance disks on a file share")
	}
	if chosen.ReclaimPolicy == string(corev1.PersistentVolumeReclaimRetain) {
		warns = append(warns, "reclaim policy Retain: disks outlive the instances they belonged to")
		r.warning(fmt.Sprintf("storage class %s keeps disks after destroy (reclaim policy Retain)", chosen.Name))
	}
	if len(warns) > 0 {
		fix := ""
		if chosen.NodeLocal && r.multiNode && !r.t.Cluster.NodeLocalAck {
			fix = "pick a network-attached class, or acknowledge node-local storage"
		}
		r.warn(CheckRootStorage, detail+": "+strings.Join(warns, "; "), fix)
		return
	}
	r.pass(CheckRootStorage, detail)
}

func bindingLabel(mode string) string {
	if mode == string(storagev1.VolumeBindingWaitForFirstConsumer) {
		return "WFFC"
	}
	return mode
}

func (r *run) reportSnapshots(snaps []snapshotClass, ok bool, why string) {
	switch {
	case !ok:
		r.skip(CheckSnapshots, why)
	case len(snaps) == 0 && r.caps.CloneTier == "csi-clone":
		r.pass(CheckSnapshots, "no snapshot classes (clone: a CSI clone of a stopped instance, a copy of a running one)")
	case len(snaps) == 0:
		r.pass(CheckSnapshots, "no snapshot classes (clone copies files)")
	default:
		detail := strings.Join(r.caps.SnapshotClasses, ", ")
		if r.rootClass != nil {
			detail += " (clone: " + r.caps.CloneTier + ")"
		}
		r.pass(CheckSnapshots, detail)
	}
}
