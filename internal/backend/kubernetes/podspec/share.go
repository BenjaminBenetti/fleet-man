//go:build !nok8s

package podspec

import (
	"encoding/json"
	"fmt"
	"path"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
	"github.com/BenjaminBenetti/fleet-man/internal/podshare"
)

// share.go renders where an instance's fleet mounts (its agents' state, the
// shared files, the custom mounts: MountFleet) live on a cluster: the
// fleet's share volume, one PVC per (connection, fleet) every instance pod of
// the fleet mounts, or on the copy tier (no volume two pods can mount) the
// instance disk's own copy. Either way the tree is laid out the same
// (internal/podshare): each fleet mount is the subPath roots/<its SubPath>,
// and the tree's root mounts at podshare.MountDir, inside a folder only root
// may enter, for the pod's spoke. A share-layout init container (the dev
// image, running fleet's binary from the instance disk) replays the fleet's
// Layout there before the dev container starts, so every subPath exists,
// with the fleet's modes, owned by the share uid, and no symlink sits where
// a fleet folder belongs: the kubelet makes a missing subPath root-owned,
// makes a missing file a folder, and follows a link inside the volume.

// ShareInit is the share-layout init container's name.
const ShareInit = "fleet-share"

// The instance pod's share volume.
const volumeShare = "share"

// Share is where an instance's fleet mounts live.
type Share struct {
	// Claim is the fleet's share PVC (names.FleetObject(fleet, h8,
	// names.SuffixShare)); "" keeps them on the instance disk (the copy
	// tier: podshare.DiskDir).
	Claim string
	// Layout is the tree the init container lays out: the fleet's Layout,
	// owned by the share uid.
	Layout podshare.Layout
}

// shareSubPath is the subPath on its volume of the fleet folder at sub.
func (s *Share) shareSubPath(sub string) string {
	if s.Claim == "" {
		return path.Join(podshare.DiskDir, podshare.SubPath(sub))
	}
	return podshare.SubPath(sub)
}

// volume is the volume the tree is on.
func (s *Share) volume() string {
	if s.Claim == "" {
		return volumeDisk
	}
	return volumeShare
}

// treeMount mounts the tree's root at podshare.MountDir.
func (s *Share) treeMount() corev1.VolumeMount {
	m := corev1.VolumeMount{Name: s.volume(), MountPath: podshare.MountDir}
	if s.Claim == "" {
		m.SubPath = podshare.DiskDir
	}
	return m
}

// fleetMount is the dev container's mount of fleet mount m.
func (s *Share) fleetMount(m Mount) corev1.VolumeMount {
	return corev1.VolumeMount{Name: s.volume(), MountPath: m.Target, SubPath: s.shareSubPath(m.SubPath), ReadOnly: m.ReadOnly}
}

// shareVolumes are the pod volumes the share needs (none on the copy tier,
// whose tree is on the instance disk).
func (s *Share) shareVolumes() []corev1.Volume {
	if s.Claim == "" {
		return nil
	}
	return []corev1.Volume{{Name: volumeShare, VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: s.Claim}}}}
}

// layoutFile is the layout as the spec Secret holds it.
func (s *Share) layoutFile() ([]byte, error) {
	l := s.Layout
	l.V = podshare.LayoutVersion
	if err := l.Validate(); err != nil {
		return nil, fmt.Errorf("podspec: the fleet's share layout: %w", err)
	}
	return json.Marshal(l)
}

// shareInitResources are the init container's: it touches a handful of
// paths.
var shareInitResources = corev1.ResourceRequirements{Requests: corev1.ResourceList{
	corev1.ResourceCPU:    resource.MustParse("10m"),
	corev1.ResourceMemory: resource.MustParse("32Mi"),
}}

// initTree is where the init container finds the tree, its volume mounted
// whole at podshare.InitDir.
func (s *Share) initTree() string {
	if s.Claim == "" {
		return path.Join(podshare.InitDir, podshare.DiskDir)
	}
	return podshare.InitDir
}

// shareInit is the share-layout init container: fleet's binary on the
// instance disk replaying the layout in the spec Secret on the tree. It
// mounts the tree's volume whole: a subPath the kubelet makes when missing is
// root-owned, which a pod without root could not lay out.
func (r *renderer) shareInit(s *Share) corev1.Container {
	return corev1.Container{
		Name:            ShareInit,
		Image:           r.in.Image.Ref,
		ImagePullPolicy: r.pullPolicy(),
		Command:         ShareInitCommand(s.initTree()),
		Resources:       *shareInitResources.DeepCopy(),
		VolumeMounts: []corev1.VolumeMount{
			{Name: volumeDisk, MountPath: podseed.InstallDir, SubPath: podseed.DirFleet, ReadOnly: true},
			{Name: volumeSpec, MountPath: podseed.SpecDir, ReadOnly: true},
			{Name: volumeRun, MountPath: podseed.RunDir},
			{Name: s.volume(), MountPath: podshare.InitDir},
		},
		SecurityContext:          r.containerSecurity(shareInitSecurity(r.in.Identity), r.in.Identity),
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
	}
}

// ShareInitCommand is the share-layout init container's command, laying out
// the tree at tree.
func ShareInitCommand(tree string) []string {
	return []string{podseed.CurrentBinary, "pod-layout", "--spec", podshare.LayoutPath, "--tree", tree}
}

// shareInitSecurity is what the init container needs: as root, giving the
// tree to the share uid (CHOWN) and setting modes on what others own
// (FOWNER, DAC_OVERRIDE), nothing more; without root, nothing at all (a
// restricted namespace admits no added capability).
func shareInitSecurity(id Identity) Security {
	if id.NonRoot {
		return Security{}
	}
	return Security{CapAdd: []string{"CHOWN", "DAC_OVERRIDE", "FOWNER"}, CapDrop: []string{"ALL"}}
}

// fleetMountsWithoutShare are the fleet mounts of a run that has no share
// to put them on.
func (r *renderer) fleetMountsWithoutShare() []string {
	var out []string
	for _, m := range r.pod.Mounts {
		if m.Kind == MountFleet {
			out = append(out, m.Target)
		}
	}
	return out
}

// SELinuxMountSupported reports whether an API server of version takes a
// pod's seLinuxChangePolicy (beta and on by default from Kubernetes 1.33):
// an older one drops the field, and a pod made without it would not hash as
// fleet rendered it.
func SELinuxMountSupported(version string) bool {
	minor, ok := kubeMinor(version)
	return ok && minor >= 33
}
