//go:build !nok8s

package podspec

import (
	"slices"
	"sort"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
)

// SeedSpec is what of a generation's seed follows from its render: the
// instance pod's directory mount targets, its hostname, the volumes to copy
// up from the image, and the fleet's named volumes it mounts (filled on the
// share when missing or empty there). The caller adds the rest: Reset and
// PrevGen for a rebuild, the share group's Remap, the Workspace, and where
// the share is in the seed's pod (Share) when it has shared volumes.
//
// The mount targets are what the seed makes mountpoints for (root-extra/
// for a top-level dir the image lacks, a dir without the image's content
// below it otherwise): every mount of the dev container but the persisted
// root itself, the files mounted over the runtime's (/etc/resolv.conf,
// /etc/hostname) and devices.
func SeedSpec(res *Result, o *Objects) *podseed.SeedSpec {
	s := &podseed.SeedSpec{Hostname: names.Pod(o.StatefulSet.Name)}
	dev := o.StatefulSet.Spec.Template.Spec.Containers[0]
	hostPaths := map[string]bool{}
	for _, v := range o.StatefulSet.Spec.Template.Spec.Volumes {
		if v.HostPath != nil {
			hostPaths[v.Name] = true
		}
	}
	for _, m := range dev.VolumeMounts {
		switch {
		case hostPaths[m.Name]:
		case m.Name == volumeDisk && (strings.HasPrefix(m.SubPath, podseed.DirRoot+"/") || strings.HasPrefix(m.SubPath, podseed.DirRootExtra+"/")):
		case m.Name == volumeDisk && (m.SubPath == podseed.ResolvConfFile || m.SubPath == podseed.HostnameFile):
		default:
			s.MountDirs = append(s.MountDirs, m.MountPath)
		}
	}
	sort.Strings(s.MountDirs)
	seen, shared := map[string]bool{}, map[string]int{}
	for _, m := range res.Pod.Mounts {
		switch m.Kind {
		case MountVolume:
			name, _, _ := strings.Cut(strings.TrimPrefix(m.SubPath, podseed.DirVolumes+"/"), "/")
			if seen[name] {
				continue
			}
			seen[name] = true
			s.Volumes = append(s.Volumes, podseed.Volume{Name: name, Target: m.Target, CopyUp: m.CopyUp})
		case MountSharedVolume:
			// A volume mounted by a subpath of it takes no copy-up (as on
			// docker); one it is mounted whole too copies up from that
			// mount's target.
			id := sharedVolumeID(m.SubPath)
			v := podseed.Volume{Name: id, Target: m.Target, CopyUp: m.CopyUp}
			if i, ok := shared[id]; ok {
				if v.CopyUp && !s.SharedVolumes[i].CopyUp {
					s.SharedVolumes[i] = v
				}
				continue
			}
			shared[id] = len(s.SharedVolumes)
			s.SharedVolumes = append(s.SharedVolumes, v)
		}
	}
	return s
}

// SharedVolumes reports whether the instance pod res describes mounts any of
// the fleet's named volumes (its seed then needs the share).
func SharedVolumes(res *Result) bool {
	return slices.ContainsFunc(res.Pod.Mounts, func(m Mount) bool { return m.Kind == MountSharedVolume })
}
