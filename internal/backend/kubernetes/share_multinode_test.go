//go:build !nok8s

package kubernetes

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/devcli"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/podspec"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/topology"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeerr"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeshim"
)

// nfsClass is an RWX class that passed the multi-node test.
var nfsClass = &fleetgrpc.KubeStorageClass{Name: "nfs", Provisioner: "nfs.csi.k8s.io", BindingMode: string(storagev1.VolumeBindingImmediate), AllowExpansion: true}

func multiNodeCaps() *fleetgrpc.KubeCapabilities {
	caps := shareCaps(wffc, immediate, nfsClass)
	caps.ShareMode, caps.SharedStorageClass, caps.InstanceStorageClass = string(topology.MultiNode), "nfs", "standard"
	return caps
}

// bindShareAnywhere binds the fleet's share once it is made, to a volume
// every node reaches (an NFS one).
func bindShareAnywhere(t *testing.T, cs *fake.Clientset) {
	t.Helper()
	go func() {
		ctx := context.Background()
		claim := ShareClaim("alpha", h8())
		for {
			pvc, err := cs.CoreV1().PersistentVolumeClaims(handleNS).Get(ctx, claim, metav1.GetOptions{})
			if err == nil {
				_, _ = cs.CoreV1().PersistentVolumes().Create(ctx, &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "pv-nfs"}}, metav1.CreateOptions{})
				pvc.Spec.VolumeName, pvc.Status.Phase = "pv-nfs", corev1.ClaimBound
				_, _ = cs.CoreV1().PersistentVolumeClaims(handleNS).Update(ctx, pvc, metav1.UpdateOptions{})
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
}

// TestPrepareShareMultiNode: in multi-node mode the fleet's share is a
// ReadWriteMany volume of the class that passed the multi-node test, even
// one that binds Immediately on several nodes (as an instance disk class
// that does is fine too); its pods are placed by the volume's own topology
// alone (none for an NFS volume): no node of a seed, no pod affinity.
func TestPrepareShareMultiNode(t *testing.T) {
	cs := fake.NewClientset()
	bindShareAnywhere(t, cs)
	c := shareCreation(cs, "web", multiNodeCaps(), fleet.KubeCluster{StorageClass: "fast"})
	if err := c.prepareShare(context.Background()); err != nil {
		t.Fatalf("prepareShare: %v", err)
	}
	pvc, err := cs.CoreV1().PersistentVolumeClaims(handleNS).Get(context.Background(), ShareClaim("alpha", h8()), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pvc.Spec.AccessModes, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}) || pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "nfs" || pvc.Annotations[names.AnnotationShareMode] != string(topology.MultiNode) {
		t.Fatalf("the share is %+v", pvc.Spec)
	}
	s := c.share
	if s.mode != topology.MultiNode || s.bind || len(s.placement.Terms) != 0 {
		t.Fatalf("share state: mode %s bind %v terms %+v", s.mode, s.bind, s.placement.Terms)
	}
	if a := s.placementFor().Affinity(true); a != nil {
		t.Fatalf("a multi-node share's pod is placed by %+v", a)
	}
	if got := c.input(nil, kubeshim.ImageConfig{}, &devcli.Config{}).MultiNodeShare; !got {
		t.Fatal("the translation does not know the share is mounted from several nodes")
	}

	// The next instance takes the share as made.
	next := shareCreation(cs, "api", multiNodeCaps(), fleet.KubeCluster{})
	if err := next.prepareShare(context.Background()); err != nil || next.share.mode != topology.MultiNode {
		t.Fatalf("the next instance: %v, mode %s", err, next.share.mode)
	}
}

// TestMultiNodeRefusesAnImmediateNodeLocalDisk: in multi-node mode an
// instance disk class on one node's disk that binds at once (minikube's
// hostpath on several nodes) is refused before anything is made: its disk
// could be made on another node than its pod.
func TestMultiNodeRefusesAnImmediateNodeLocalDisk(t *testing.T) {
	hostpath := &fleetgrpc.KubeStorageClass{Name: "standard", NodeLocal: true, BindingMode: string(storagev1.VolumeBindingImmediate)}
	caps := shareCaps(hostpath, nfsClass)
	caps.ShareMode, caps.SharedStorageClass, caps.InstanceStorageClass = string(topology.MultiNode), "nfs", "standard"
	cs := fake.NewClientset()
	err := shareCreation(cs, "web", caps, fleet.KubeCluster{}).prepareShare(context.Background())
	code, _ := kubeerr.Of(err)
	if code != kubeerr.Misconfigured.Narrow("disk_binding") || !strings.Contains(err.Error(), "WaitForFirstConsumer") {
		t.Fatalf("prepareShare = %v (%s)", err, code)
	}
	if l, _ := cs.CoreV1().PersistentVolumeClaims(handleNS).List(context.Background(), metav1.ListOptions{}); len(l.Items) != 0 {
		t.Fatal("the share was made")
	}
	// On one node it is fine.
	caps.Nodes = caps.Nodes[:1]
	bindShareAnywhere(t, cs)
	if err := shareCreation(cs, "web", caps, fleet.KubeCluster{}).prepareShare(context.Background()); err != nil {
		t.Fatalf("one node: %v", err)
	}
}

// TestMultiNodeShareWithoutItsVolume: a multi-node share whose volume
// cannot be read (no cluster-scoped read) places nothing, and records
// nothing, so a later create reads it again; it never pins the fleet to a
// pod's node.
func TestMultiNodeShareWithoutItsVolume(t *testing.T) {
	cs := fake.NewClientset()
	cs.PrependReactor("get", "persistentvolumes", func(k8stesting.Action) (bool, kruntime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "persistentvolumes"}, "pv-nfs", errors.New("no"))
	})
	bindShareAnywhere(t, cs)
	// A pod of the fleet runs on node-a mounting the share: a single-node
	// share would be placed beside it.
	holder := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "holder-0", Namespace: handleNS, Labels: map[string]string{
			names.LabelManagedBy: names.ManagedBy, names.LabelOwner: handleOwner, names.LabelCluster: handleCluster, names.LabelShareGroup: h8(),
		}},
		Spec:   corev1.PodSpec{NodeName: "node-a", Volumes: []corev1.Volume{{Name: "share", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: ShareClaim("alpha", h8())}}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if _, err := cs.CoreV1().Pods(handleNS).Create(context.Background(), holder, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	c := shareCreation(cs, "web", multiNodeCaps(), fleet.KubeCluster{})
	if err := c.prepareShare(context.Background()); err != nil {
		t.Fatalf("prepareShare: %v", err)
	}
	if len(c.share.placement.Terms) != 0 {
		t.Fatalf("placed by %+v", c.share.placement.Terms)
	}
	pvc, _ := cs.CoreV1().PersistentVolumeClaims(handleNS).Get(context.Background(), ShareClaim("alpha", h8()), metav1.GetOptions{})
	if _, ok := pvc.Annotations[names.AnnotationShareTopology]; ok {
		t.Fatalf("an unknown topology was recorded: %v", pvc.Annotations)
	}
}

// TestPrepareShareRefusedClass: a connection whose shared class fails the
// multi-node test's file system checks makes no share; the create is
// refused with the reason and the fix.
func TestPrepareShareRefusedClass(t *testing.T) {
	cs := fake.NewClientset()
	caps := shareCaps(wffc)
	caps.ShareRefused = "storage class azurefile fails the multi-node test: file modes do not behave across the pods"
	err := shareCreation(cs, "web", caps, fleet.KubeCluster{SharedStorageClass: "azurefile"}).prepareShare(context.Background())
	code, _ := kubeerr.Of(err)
	if code != kubeerr.Misconfigured.Narrow("shared_storage") || !strings.Contains(err.Error(), "file modes") || !strings.Contains(err.Error(), "passes the multi-node test") {
		t.Fatalf("prepareShare = %v (%s)", err, code)
	}
	if l, _ := cs.CoreV1().PersistentVolumeClaims(handleNS).List(context.Background(), metav1.ListOptions{}); len(l.Items) != 0 {
		t.Fatal("a refused share was made")
	}
}

// TestStorageClassesResolve: an instance disk is made in the connection's
// class, else Auto's (the preflight's pick); a rebuilt one stays in its own.
// A share is made in the connection's shared class, else Auto's, else the
// disks'.
func TestStorageClassesResolve(t *testing.T) {
	caps := multiNodeCaps()
	c := shareCreation(fake.NewClientset(), "web", caps, fleet.KubeCluster{})
	if c.diskClass() != "standard" || c.renderInput(podspec.ImageRef{}, nil).StorageClass != "standard" || c.shareClass() != "nfs" {
		t.Fatalf("auto: disk %q, render %q, share %q", c.diskClass(), c.renderInput(podspec.ImageRef{}, nil).StorageClass, c.shareClass())
	}
	c.rec.StorageClass, c.rec.SharedStorageClass = "fast", "cephfs"
	if c.diskClass() != "fast" || c.shareClass() != "cephfs" {
		t.Fatalf("set: disk %q, share %q", c.diskClass(), c.shareClass())
	}
	c.keptDiskClass = "old"
	if c.diskClass() != "old" {
		t.Fatalf("a rebuild's disk: %q", c.diskClass())
	}
	single := shareCaps(wffc)
	single.InstanceStorageClass = "standard"
	if got := shareCreation(fake.NewClientset(), "web", single, fleet.KubeCluster{}).shareClass(); got != "standard" {
		t.Fatalf("single-node auto shares in %q, want the disks' class", got)
	}
	if got := shareCreation(fake.NewClientset(), "web", nil, fleet.KubeCluster{}).diskClass(); got != "" {
		t.Fatalf("before any preflight: %q, want the cluster's default", got)
	}
}

// TestCodexSQLiteHomeWarning: a fleet Codex config.toml whose sqlite_home
// is on the share (below a fleet mount, or not absolute) is warned of; one
// elsewhere, or none, is not.
func TestCodexSQLiteHomeWarning(t *testing.T) {
	dir := t.TempDir()
	mounts := []backend.Mount{
		{LocalPath: dir, ContainerPath: "/home/node/.codex", Scope: backend.MountScopeFleetShared, SubPath: ".codex"},
		{LocalPath: "/x", ContainerPath: "/home/node/.claude", Scope: backend.MountScopeFleetShared, SubPath: ".claude"},
	}
	for config, warn := range map[string]bool{
		"":                                     false,
		`model = "o4"`:                         false,
		`sqlite_home = "/var/lib/codex"`:       false,
		`sqlite_home = "/home/node/.codex/db"`: true,
		`sqlite_home = "/home/node/.claude"`:   true,
		`sqlite_home = "db"`:                   true,
		`sqlite_home = "~/.codex/db"`:          true,
		"not = [toml":                          false,
	} {
		if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
		got := codexSQLiteHomeWarning(mounts)
		if (got != "") != warn {
			t.Errorf("config %q: warning %q", config, got)
		}
		if warn && !strings.Contains(got, "remove sqlite_home") {
			t.Errorf("config %q: the warning names no fix: %q", config, got)
		}
	}
	if err := os.Remove(filepath.Join(dir, "config.toml")); err != nil {
		t.Fatal(err)
	}
	if got := codexSQLiteHomeWarning(mounts); got != "" {
		t.Errorf("no config.toml: %q", got)
	}
	if got := codexSQLiteHomeWarning(mounts[1:]); got != "" {
		t.Errorf("no Codex mount: %q", got)
	}
}
