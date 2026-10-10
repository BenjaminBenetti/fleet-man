package podseed

import (
	"fmt"
	"path"
)

// The instance PVC's top-level layout. Each entry is a subPath the instance
// pod (or a Job) mounts.
const (
	// DirRoot holds the image's persisted top-level dirs: root/<d> mounts
	// at /<d>.
	DirRoot = "root"
	// DirRootExtra holds top-level dirs the image does not have: mountpoint
	// parents such as /workspaces, and dirs created at runtime (swept here on
	// stop). root-extra/<d> mounts at /<d>.
	DirRootExtra = "root-extra"
	// DirWorkspace is the workspace folder's content.
	DirWorkspace = "ws"
	// DirVolumes holds named and anonymous volumes: vol/<name>.
	DirVolumes = "vol"
	// DirFleet mounts at InstallDir: binaries, logs and pod-local state.
	DirFleet = "fleet"
	// DirMeta holds the manifest and the files the pod mounts over the
	// kubelet's.
	DirMeta = "meta"
	// DirSync is sharesync's copy-tier state.
	DirSync = "sync"
)

// Files and dirs the layout defines below its top-level dirs.
const (
	// ManifestFile is the seed's result: the current generation's Manifest.
	ManifestFile = DirMeta + "/rootfs.json"
	// ResolvConfFile is mounted at /etc/resolv.conf: the cluster DNS config
	// the seed pod got, written by every seed.
	ResolvConfFile = DirMeta + "/resolv.conf"
	// HostnameFile is mounted at /etc/hostname: the pod's hostname.
	HostnameFile = DirMeta + "/hostname"
	// FleetBinDir holds the content-addressed fleet binaries (fleet-<sha>)
	// and the `fleet` link to the current one.
	FleetBinDir = DirFleet + "/bin"
	// FleetLogDir holds pod-init's rolling copy of PID 1's output.
	FleetLogDir = DirFleet + "/log"
)

// Where the instance pod sees the fleet/ subPath and what is in it.
const (
	// InstallDir is where fleet/ mounts in the instance pod.
	InstallDir = "/opt/fleet"
	// BinDir is fleet/bin as the pod sees it.
	BinDir = InstallDir + "/bin"
	// CurrentBinary is the link to the current fleet binary, the pod's
	// command and the agent's.
	CurrentBinary = BinDir + "/fleet"
	// LogDir is fleet/log as the pod sees it.
	LogDir = InstallDir + "/log"
	// SpecDir is where the <sts>-spec Secret mounts.
	SpecDir = InstallDir + "/spec"
	// RunDir is the pod-local emptyDir for sockets, caches and pod-init's
	// cgroup layout record.
	RunDir = InstallDir + "/run"
	// FleetLink is the stable path in-instance tools find fleet at
	// (fleetlaunch.RemotePath on docker); a symlink to CurrentBinary.
	FleetLink = "/usr/bin/fleet"
)

// BinaryName is the content-addressed name of the fleet binary with sha256
// sha: fleet-<sha> in BinDir.
func BinaryName(sha string) string { return "fleet-" + sha }

// RootSubPath is the PVC subPath the instance pod mounts at /<top> for a
// top-level dir the image has.
func RootSubPath(top string) string { return path.Join(DirRoot, top) }

// RootExtraSubPath is the subPath mounted at /<top> for a top-level dir the
// image lacks.
func RootExtraSubPath(top string) string { return path.Join(DirRootExtra, top) }

// VolumeSubPath is the subPath of volume name.
func VolumeSubPath(name string) string { return path.Join(DirVolumes, name) }

// trashDir is where a reset moves dir's generation gen.
func trashDir(dir string, gen int) string { return fmt.Sprintf("%s.g%d.trash", dir, gen) }

// partialDir is where a discarded, partly seeded generation gen of dir goes on
// its way out.
func partialDir(dir string, gen int) string { return fmt.Sprintf("%s.g%d.partial", dir, gen) }

// trashManifest is where a reset keeps generation gen's manifest.
func trashManifest(gen int) string { return fmt.Sprintf("%s/rootfs.g%d.json", DirMeta, gen) }

// persistedDirs are the top-level PVC dirs a reset moves to trash and a
// rollback restores: the root generation itself.
var persistedDirs = []string{DirRoot, DirRootExtra}
